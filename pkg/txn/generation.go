package txn

import (
	"context"
	"sync/atomic"
)

// Data generations, used by caches of API results.
//
// generation is incremented whenever data visible through the API changed
// because of something a client did: a committed write transaction from a
// request (UI, plugins, API users), configuration changes, plugin and scraper
// reloads, finished jobs. Caches must treat results from an older generation
// as invalid, so clients always read their own writes.
//
// backgroundGeneration is incremented by writes from background jobs (scan,
// auto-tag, identify, generate, ...), which can commit hundreds of times per
// second. Caches may serve results that are only behind on background writes
// for a bounded time while refreshing them, so the UI stays responsive while
// a job runs. When the job finishes, generation is bumped.
var (
	generation           atomic.Uint64
	backgroundGeneration atomic.Uint64
)

// Generation returns the current data generation.
func Generation() uint64 {
	return generation.Load()
}

// BackgroundGeneration returns the current background data generation.
func BackgroundGeneration() uint64 {
	return backgroundGeneration.Load()
}

// BumpGeneration marks all cached API results as stale.
func BumpGeneration() {
	generation.Add(1)
}

// BumpBackgroundGeneration marks cached API results as behind on background
// writes.
func BumpBackgroundGeneration() {
	backgroundGeneration.Add(1)
}

type backgroundKey struct{}

// WithBackground marks ctx as belonging to a background job: its committed
// writes bump the background generation instead of the generation.
func WithBackground(ctx context.Context) context.Context {
	return context.WithValue(ctx, backgroundKey{}, true)
}

// IsBackground reports whether ctx belongs to a background job.
func IsBackground(ctx context.Context) bool {
	v, _ := ctx.Value(backgroundKey{}).(bool)
	return v
}

// bumpAfterCommit records a committed write transaction.
func bumpAfterCommit(ctx context.Context) {
	if IsBackground(ctx) {
		BumpBackgroundGeneration()
		return
	}
	BumpGeneration()
}
