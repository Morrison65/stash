package txn

import "sync/atomic"

// generation is incremented whenever data visible through the API may have
// changed: after every committed write transaction, and explicitly by code
// that changes state outside the database (configuration, plugin reloads,
// finished jobs). Caches key their entries by it, so a bump invalidates them
// all at once.
var generation atomic.Uint64

// Generation returns the current data generation.
func Generation() uint64 {
	return generation.Load()
}

// BumpGeneration marks all cached API results as stale.
func BumpGeneration() {
	generation.Add(1)
}
