package plugin

import (
	"context"
	"strings"
	"sync"

	"github.com/stashapp/stash/pkg/logger"
	"github.com/stashapp/stash/pkg/plugin/hook"
)

// SyncTriggerPrefix marks a hook trigger that must run synchronously, i.e.
// inside the operation that fired it (the behaviour of upstream Stash):
//
//	hooks:
//	  - name: Rename on update
//	    triggeredBy:
//	      - sync:Scene.Update.Post
//
// Triggers without the prefix run asynchronously: the GraphQL mutation
// returns as soon as the data is committed and the hook runs in the
// background.
const SyncTriggerPrefix = "sync:"

// parseTrigger splits a configured trigger into the hook type and whether it
// must run synchronously.
func parseTrigger(t hook.TriggerEnum) (hookType hook.TriggerEnum, sync bool) {
	if rest, ok := strings.CutPrefix(string(t), SyncTriggerPrefix); ok {
		return hook.TriggerEnum(rest), true
	}
	return t, false
}

// asyncQueueSize bounds the pending hook executions per plugin. When a queue
// is full the caller waits, which degrades to synchronous behaviour instead
// of dropping events.
const asyncQueueSize = 1024

// asyncHookRunner executes asynchronous hooks. Each plugin has its own queue
// served by one goroutine, so a plugin sees its events in order (e.g. two
// quick updates of the same scene) while different plugins run in parallel.
type asyncHookRunner struct {
	mu     sync.Mutex
	queues map[string]chan func()
	wg     sync.WaitGroup
}

func newAsyncHookRunner() *asyncHookRunner {
	return &asyncHookRunner{queues: make(map[string]chan func())}
}

func (r *asyncHookRunner) enqueue(pluginID string, fn func()) {
	r.mu.Lock()
	q, ok := r.queues[pluginID]
	if !ok {
		q = make(chan func(), asyncQueueSize)
		r.queues[pluginID] = q
		go r.serve(pluginID, q)
	}
	r.mu.Unlock()

	r.wg.Add(1)
	select {
	case q <- fn:
	default:
		logger.Warnf("plugin %s: async hook queue is full, waiting", pluginID)
		q <- fn
	}
}

func (r *asyncHookRunner) serve(pluginID string, q chan func()) {
	for fn := range q {
		func() {
			defer r.wg.Done()
			defer func() {
				if p := recover(); p != nil {
					logger.Errorf("plugin %s: panic in async hook: %v", pluginID, p)
				}
			}()
			fn()
		}()
	}
}

// Wait blocks until all queued hooks have finished. Used by tests and
// shutdown.
func (r *asyncHookRunner) Wait() {
	r.wg.Wait()
}

// detach keeps the values of ctx (user, visited plugin hooks) but not its
// cancellation, so that an async hook outlives the request that fired it.
func detach(ctx context.Context) context.Context {
	return context.WithoutCancel(ctx)
}
