package api

import (
	"context"
	"encoding/json"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/99designs/gqlgen/graphql"
	"github.com/stashapp/stash/pkg/txn"
	"github.com/vektah/gqlparser/v2/ast"
	"github.com/vektah/gqlparser/v2/gqlerror"
)

func opCtx(op ast.Operation, rootField string, vars map[string]any) context.Context {
	return graphql.WithOperationContext(context.Background(), &graphql.OperationContext{
		RawQuery:  "query { " + rootField + " }",
		Variables: vars,
		Operation: &ast.OperationDefinition{
			Operation:    op,
			SelectionSet: ast.SelectionSet{&ast.Field{Name: rootField}},
		},
	})
}

func countingHandler(calls *atomic.Int32, delay time.Duration) graphql.ResponseHandler {
	return func(ctx context.Context) *graphql.Response {
		calls.Add(1)
		time.Sleep(delay)
		return &graphql.Response{Data: json.RawMessage(`{"n":1}`)}
	}
}

func TestQueryCacheHitAndInvalidation(t *testing.T) {
	c := newQueryCache()
	var calls atomic.Int32
	next := countingHandler(&calls, 0)
	ctx := opCtx(ast.Query, "findScenes", map[string]any{"id": 1})

	c.InterceptResponse(ctx, next)
	resp := c.InterceptResponse(ctx, next)
	if calls.Load() != 1 {
		t.Fatalf("second identical query should be cached, handler called %d times", calls.Load())
	}
	if string(resp.Data) != `{"n":1}` {
		t.Errorf("cached data = %s", resp.Data)
	}

	// different variables are a different entry
	c.InterceptResponse(opCtx(ast.Query, "findScenes", map[string]any{"id": 2}), next)
	if calls.Load() != 2 {
		t.Errorf("different variables must not hit the cache")
	}

	// a committed write invalidates everything
	txn.BumpGeneration()
	c.InterceptResponse(ctx, next)
	if calls.Load() != 3 {
		t.Errorf("query after a write must execute again")
	}
}

func TestQueryCacheSkipsMutationsAndVolatileFields(t *testing.T) {
	c := newQueryCache()
	var calls atomic.Int32
	next := countingHandler(&calls, 0)

	for i := 0; i < 2; i++ {
		c.InterceptResponse(opCtx(ast.Mutation, "sceneUpdate", nil), next)
		c.InterceptResponse(opCtx(ast.Query, "jobQueue", nil), next)
		c.InterceptResponse(opCtx(ast.Query, "scrapeSceneURL", nil), next)
	}
	if calls.Load() != 6 {
		t.Errorf("mutations and volatile queries must always execute, got %d calls", calls.Load())
	}
}

func TestQueryCacheDoesNotStoreErrors(t *testing.T) {
	c := newQueryCache()
	var calls atomic.Int32
	next := func(ctx context.Context) *graphql.Response {
		calls.Add(1)
		return &graphql.Response{Errors: gqlerror.List{{Message: "boom"}}}
	}
	ctx := opCtx(ast.Query, "findScene", nil)
	c.InterceptResponse(ctx, next)
	c.InterceptResponse(ctx, next)
	if calls.Load() != 2 {
		t.Errorf("error responses must not be cached")
	}
}

func TestQueryCacheCoalescesConcurrentQueries(t *testing.T) {
	c := newQueryCache()
	var calls atomic.Int32
	next := countingHandler(&calls, 50*time.Millisecond)
	ctx := opCtx(ast.Query, "configuration", nil)

	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if resp := c.InterceptResponse(ctx, next); resp == nil || string(resp.Data) != `{"n":1}` {
				t.Error("missing response")
			}
		}()
	}
	wg.Wait()

	if calls.Load() != 1 {
		t.Errorf("20 concurrent identical queries should execute once, got %d", calls.Load())
	}
}

func TestQueryCacheRejectsResultOfRacingWrite(t *testing.T) {
	c := newQueryCache()
	var calls atomic.Int32
	next := func(ctx context.Context) *graphql.Response {
		calls.Add(1)
		// a write commits while the query is executing
		txn.BumpGeneration()
		return &graphql.Response{Data: json.RawMessage(`{}`)}
	}
	ctx := opCtx(ast.Query, "findScene", nil)
	c.InterceptResponse(ctx, next)
	c.InterceptResponse(ctx, next)
	if calls.Load() != 2 {
		t.Errorf("a result computed across a write must not be cached")
	}
}

func TestQueryCacheKeyIncludesBaseURL(t *testing.T) {
	c := newQueryCache()
	var calls atomic.Int32
	next := countingHandler(&calls, 0)

	base := opCtx(ast.Query, "findScene", nil)
	gateway := context.WithValue(base, BaseURLCtxKey, "http://127.0.0.1:9990")
	direct := context.WithValue(base, BaseURLCtxKey, "http://localhost:9999")

	c.InterceptResponse(gateway, next)
	c.InterceptResponse(direct, next)
	c.InterceptResponse(gateway, next)
	if calls.Load() != 2 {
		t.Errorf("responses contain absolute URLs: each base URL needs its own entry, got %d executions", calls.Load())
	}
}

func TestQueryCacheSkipsUnseededRandomSort(t *testing.T) {
	c := newQueryCache()
	var calls atomic.Int32
	next := countingHandler(&calls, 0)

	inline := graphql.WithOperationContext(context.Background(), &graphql.OperationContext{
		RawQuery:  `query { findScenes(filter: {sort: "random"}) { count } }`,
		Operation: &ast.OperationDefinition{Operation: ast.Query, SelectionSet: ast.SelectionSet{&ast.Field{Name: "findScenes"}}},
	})
	viaVars := opCtx(ast.Query, "findScenes", map[string]any{"filter": map[string]any{"sort": "random"}})
	seeded := opCtx(ast.Query, "findScenes", map[string]any{"filter": map[string]any{"sort": "random_12345"}})

	for i := 0; i < 2; i++ {
		c.InterceptResponse(inline, next)
		c.InterceptResponse(viaVars, next)
	}
	if calls.Load() != 4 {
		t.Errorf("unseeded random sort must not be cached, got %d executions", calls.Load())
	}

	c.InterceptResponse(seeded, next)
	c.InterceptResponse(seeded, next)
	if calls.Load() != 5 {
		t.Errorf("seeded random sort is deterministic and may be cached, got %d executions", calls.Load())
	}
}

func TestQueryCacheRetriesWhenSharedExecutionFailed(t *testing.T) {
	c := newQueryCache()
	var calls atomic.Int32
	release := make(chan struct{})
	failing := func(ctx context.Context) *graphql.Response {
		calls.Add(1)
		<-release
		return &graphql.Response{Errors: gqlerror.List{{Message: "context canceled"}}}
	}
	ok := countingHandler(&calls, 0)
	ctx := opCtx(ast.Query, "findScene", nil)

	var wg sync.WaitGroup
	var second *graphql.Response
	wg.Add(1)
	go func() { defer wg.Done(); c.InterceptResponse(ctx, failing) }()
	time.Sleep(20 * time.Millisecond)
	wg.Add(1)
	go func() { defer wg.Done(); second = c.InterceptResponse(ctx, ok) }()
	time.Sleep(20 * time.Millisecond)
	close(release)
	wg.Wait()

	if second == nil || len(second.Errors) > 0 {
		t.Errorf("a waiter must not inherit another request's failure: %+v", second)
	}
}

func TestQueryCacheServesDuringBackgroundWrites(t *testing.T) {
	c := newQueryCache()
	c.staleness = time.Minute

	var calls atomic.Int32
	slowRefresh := make(chan struct{})
	next := func(ctx context.Context) *graphql.Response {
		n := calls.Add(1)
		if n > 1 {
			<-slowRefresh
		}
		return &graphql.Response{Data: json.RawMessage(`{"v":` + string(rune('0'+n)) + `}`)}
	}
	ctx := opCtx(ast.Query, "findScenes", nil)

	c.InterceptResponse(ctx, next) // v=1 cached

	// a background job writes: the cached answer is still served at once
	txn.BumpBackgroundGeneration()
	start := time.Now()
	resp := c.InterceptResponse(ctx, next)
	if time.Since(start) > 50*time.Millisecond {
		t.Fatalf("stale hit waited for the refresh")
	}
	if string(resp.Data) != `{"v":1}` {
		t.Errorf("want the cached answer, got %s", resp.Data)
	}

	// exactly one background refresh runs, even with more requests
	c.InterceptResponse(ctx, next)
	time.Sleep(20 * time.Millisecond)
	if calls.Load() != 2 {
		t.Errorf("want one background refresh, handler called %d times", calls.Load())
	}

	// once the refresh finished, its result is served
	close(slowRefresh)
	time.Sleep(50 * time.Millisecond)
	if resp := c.InterceptResponse(ctx, next); string(resp.Data) != `{"v":2}` {
		t.Errorf("want the refreshed answer, got %s", resp.Data)
	}
}

func TestQueryCacheBackgroundStalenessBound(t *testing.T) {
	c := newQueryCache()
	c.staleness = 30 * time.Millisecond

	var calls atomic.Int32
	next := countingHandler(&calls, 0)
	ctx := opCtx(ast.Query, "findImages", nil)

	c.InterceptResponse(ctx, next)
	time.Sleep(50 * time.Millisecond)
	txn.BumpBackgroundGeneration()

	// older than the bound: computed again before answering
	c.InterceptResponse(ctx, next)
	if calls.Load() != 2 {
		t.Errorf("an entry older than the staleness bound must be recomputed, got %d calls", calls.Load())
	}
}

func TestQueryCacheClientWriteBeatsStaleness(t *testing.T) {
	c := newQueryCache()
	c.staleness = time.Minute

	var calls atomic.Int32
	next := countingHandler(&calls, 0)
	ctx := opCtx(ast.Query, "findPerformers", nil)

	c.InterceptResponse(ctx, next)
	txn.BumpBackgroundGeneration()
	txn.BumpGeneration() // e.g. the user saved a performer

	c.InterceptResponse(ctx, next)
	if calls.Load() != 2 {
		t.Errorf("a client write must invalidate even within the staleness bound")
	}
}
