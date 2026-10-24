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
