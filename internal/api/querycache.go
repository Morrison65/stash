package api

import (
	"container/list"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"regexp"
	"strconv"
	"sync"
	"time"

	"github.com/99designs/gqlgen/graphql"
	"github.com/stashapp/stash/pkg/session"
	"github.com/stashapp/stash/pkg/txn"
	"github.com/vektah/gqlparser/v2/ast"
	"golang.org/x/sync/singleflight"
)

// queryCache caches the responses of read-only GraphQL queries.
//
// Plugins and UI components frequently send the same query several times in
// a row (configuration, findScene, ...). Identical concurrent queries are
// coalesced into one execution, and results are reused until the data
// generation changes (txn.Generation, bumped after every committed write of a
// request and on configuration/plugin/job changes) or the TTL expires. Because
// the generation is captured before execution, a write that commits while a
// query runs makes its result unusable: clients always read their own writes.
//
// Writes of background jobs (txn.BackgroundGeneration) are treated
// differently: a job like auto-tag commits hundreds of times per second, which
// would make every page load recompute the large list queries. A result that
// is only behind on background writes is served for up to `staleness` after
// its computation started, while one background refresh computes a new one.
// So pages stay fast while jobs run and show data at most `staleness` old;
// when the job finishes the generation is bumped and everything is fresh.
//
// Root fields whose results depend on something other than the database
// (external scrapers, jobs, logs, the file system, randomness) are never
// cached.
type queryCache struct {
	ttl        time.Duration
	staleness  time.Duration
	maxEntries int
	maxBytes   int

	mu         sync.Mutex
	entries    map[string]*list.Element
	lru        *list.List
	refreshing map[string]bool

	flight singleflight.Group
}

type queryCacheEntry struct {
	key          string
	generation   uint64
	bgGeneration uint64
	// computed is when the computation of resp started: resp reflects all
	// writes committed before this time
	computed time.Time
	expires  time.Time
	resp     *graphql.Response
}

// envBackgroundStaleness overrides the staleness bound (seconds, 0 = off).
const envBackgroundStaleness = "STASH_CACHE_BACKGROUND_STALENESS"

const defaultBackgroundStaleness = 30 * time.Second

// uncacheableRootFields lists query root fields that must always execute.
var uncacheableRootFields = map[string]bool{
	"markerWall":                  true, // random
	"sceneWall":                   true, // random
	"logs":                        true,
	"listScrapers":                true,
	"scrapeSingleScene":           true,
	"scrapeMultiScenes":           true,
	"scrapeSingleStudio":          true,
	"scrapeSingleTag":             true,
	"scrapeSinglePerformer":       true,
	"scrapeMultiPerformers":       true,
	"scrapeSingleGallery":         true,
	"scrapeSingleMovie":           true,
	"scrapeSingleGroup":           true,
	"scrapeSingleImage":           true,
	"scrapeURL":                   true,
	"scrapePerformerURL":          true,
	"scrapeSceneURL":              true,
	"scrapeGalleryURL":            true,
	"scrapeImageURL":              true,
	"scrapeMovieURL":              true,
	"scrapeGroupURL":              true,
	"installedPackages":           true,
	"availablePackages":           true,
	"directory":                   true,
	"validateStashBoxCredentials": true,
	"systemStatus":                true,
	"jobQueue":                    true,
	"findJob":                     true,
	"dlnaStatus":                  true,
	"latestversion":               true,
}

func newQueryCache() *queryCache {
	staleness := defaultBackgroundStaleness
	if v := os.Getenv(envBackgroundStaleness); v != "" {
		if secs, err := strconv.Atoi(v); err == nil && secs >= 0 {
			staleness = time.Duration(secs) * time.Second
		}
	}

	return &queryCache{
		ttl:        time.Minute,
		staleness:  staleness,
		maxEntries: 2000,
		maxBytes:   4 << 20,
		entries:    make(map[string]*list.Element),
		lru:        list.New(),
		refreshing: make(map[string]bool),
	}
}

var (
	_ graphql.HandlerExtension    = (*queryCache)(nil)
	_ graphql.ResponseInterceptor = (*queryCache)(nil)
)

func (c *queryCache) ExtensionName() string { return "StashQueryCache" }

func (c *queryCache) Validate(graphql.ExecutableSchema) error { return nil }

func (c *queryCache) InterceptResponse(ctx context.Context, next graphql.ResponseHandler) *graphql.Response {
	if !graphql.HasOperationContext(ctx) {
		return next(ctx)
	}

	oc := graphql.GetOperationContext(ctx)
	if !cacheableOperation(oc) {
		return next(ctx)
	}

	key, ok := cacheKey(ctx, oc)
	if !ok {
		return next(ctx)
	}

	gen, bg := txn.Generation(), txn.BackgroundGeneration()
	if resp, stale := c.get(key, gen, bg); resp != nil {
		if stale {
			c.refresh(ctx, key, next)
		}
		return resp
	}

	produced := false
	flightKey := key + "@" + strconv.FormatUint(gen, 10) + "/" + strconv.FormatUint(bg, 10)
	v, _, _ := c.flight.Do(flightKey, func() (any, error) {
		produced = true
		return c.execute(ctx, key, next), nil
	})

	resp, _ := v.(*graphql.Response)

	// The shared execution ran with another request's context. If it failed
	// (e.g. that client disconnected), run the query for this request.
	if !produced && (resp == nil || len(resp.Errors) > 0) && ctx.Err() == nil {
		return next(ctx)
	}

	return cloneResponse(resp)
}

// execute runs the query and stores a cacheable result.
func (c *queryCache) execute(ctx context.Context, key string, next graphql.ResponseHandler) *graphql.Response {
	gen, bg, started := txn.Generation(), txn.BackgroundGeneration(), time.Now()
	resp := next(ctx)
	if resp != nil && len(resp.Errors) == 0 && len(resp.Data) <= c.maxBytes {
		c.put(key, gen, bg, started, resp)
	}
	return resp
}

// refresh recomputes a stale entry in the background, at most once per key at
// a time. The request that triggered it has already been answered, so the
// query runs with its values but without its cancellation.
func (c *queryCache) refresh(ctx context.Context, key string, next graphql.ResponseHandler) {
	c.mu.Lock()
	if c.refreshing[key] {
		c.mu.Unlock()
		return
	}
	c.refreshing[key] = true
	c.mu.Unlock()

	go func() {
		defer func() {
			c.mu.Lock()
			delete(c.refreshing, key)
			c.mu.Unlock()
		}()
		rctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Minute)
		defer cancel()
		c.execute(rctx, key, next)
	}()
}

func cacheableOperation(oc *graphql.OperationContext) bool {
	op := oc.Operation
	if op == nil || op.Operation != ast.Query {
		return false
	}

	for _, sel := range op.SelectionSet {
		field, ok := sel.(*ast.Field)
		if !ok {
			// fragments at the root: be conservative
			return false
		}
		if uncacheableRootFields[field.Name] {
			return false
		}
	}

	// sort: "random" without a seed picks a new order on every call
	if unseededRandomSortRE.MatchString(oc.RawQuery) || containsString(oc.Variables, "random") {
		return false
	}

	return true
}

var unseededRandomSortRE = regexp.MustCompile(`sort\s*:\s*"random"`)

// containsString reports whether v (decoded JSON variables) contains the
// exact string s anywhere.
func containsString(v any, s string) bool {
	switch t := v.(type) {
	case string:
		return t == s
	case map[string]any:
		for _, e := range t {
			if containsString(e, s) {
				return true
			}
		}
	case []any:
		for _, e := range t {
			if containsString(e, s) {
				return true
			}
		}
	case *string:
		return t != nil && *t == s
	}
	return false
}

func cacheKey(ctx context.Context, oc *graphql.OperationContext) (string, bool) {
	vars, err := json.Marshal(oc.Variables)
	if err != nil {
		return "", false
	}

	h := sha256.New()
	if uid := session.GetCurrentUserID(ctx); uid != nil {
		h.Write([]byte(*uid))
	}
	h.Write([]byte{0})
	// responses contain absolute URLs built from the request's host
	baseURL, _ := ctx.Value(BaseURLCtxKey).(string)
	h.Write([]byte(baseURL))
	h.Write([]byte{0})
	h.Write([]byte(oc.OperationName))
	h.Write([]byte{0})
	h.Write([]byte(oc.RawQuery))
	h.Write([]byte{0})
	h.Write(vars)
	return hex.EncodeToString(h.Sum(nil)), true
}

// get returns a cached response. stale is true when the response is only
// behind on background writes and still within the staleness bound; the
// caller should trigger a refresh.
func (c *queryCache) get(key string, gen, bg uint64) (resp *graphql.Response, stale bool) {
	c.mu.Lock()
	defer c.mu.Unlock()

	el, ok := c.entries[key]
	if !ok {
		return nil, false
	}

	e := el.Value.(*queryCacheEntry)
	now := time.Now()
	if e.generation != gen || now.After(e.expires) {
		c.lru.Remove(el)
		delete(c.entries, key)
		return nil, false
	}

	if e.bgGeneration != bg {
		if c.staleness <= 0 || now.Sub(e.computed) > c.staleness {
			c.lru.Remove(el)
			delete(c.entries, key)
			return nil, false
		}
		stale = true
	}

	c.lru.MoveToFront(el)
	return cloneResponse(e.resp), stale
}

func (c *queryCache) put(key string, gen, bg uint64, computed time.Time, resp *graphql.Response) {
	c.mu.Lock()
	defer c.mu.Unlock()

	// a client write committed while the query executed: the result may not
	// contain it, so it must not be served
	if gen != txn.Generation() {
		return
	}

	// never replace a newer result with an older one (refresh races)
	if el, ok := c.entries[key]; ok && el.Value.(*queryCacheEntry).computed.After(computed) {
		return
	}

	e := &queryCacheEntry{key: key, generation: gen, bgGeneration: bg, computed: computed, expires: time.Now().Add(c.ttl), resp: cloneResponse(resp)}
	if el, ok := c.entries[key]; ok {
		el.Value = e
		c.lru.MoveToFront(el)
		return
	}

	c.entries[key] = c.lru.PushFront(e)
	for c.lru.Len() > c.maxEntries {
		oldest := c.lru.Back()
		c.lru.Remove(oldest)
		delete(c.entries, oldest.Value.(*queryCacheEntry).key)
	}
}

// cloneResponse returns a shallow copy; Data is never mutated after
// execution, but the response struct itself may be modified by transports.
func cloneResponse(r *graphql.Response) *graphql.Response {
	if r == nil {
		return nil
	}
	ret := *r
	return &ret
}
