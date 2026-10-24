package plugin

import (
	"sync"

	"github.com/stashapp/stash/pkg/plugin/hook"
)

// PostHookListener receives post hook events: the object id, the hook type
// and the input fields of the update (nil when unknown). It runs inside the
// operation that fired the event, so it must only record the event and
// return.
type PostHookListener func(id int, hookType hook.TriggerEnum, inputFields []string)

type hookListeners struct {
	mu  sync.RWMutex
	fns []PostHookListener
}

func (l *hookListeners) add(fn PostHookListener) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.fns = append(l.fns, fn)
}

func (l *hookListeners) notify(id int, hookType hook.TriggerEnum, inputFields []string) {
	if l == nil {
		return
	}
	l.mu.RLock()
	defer l.mu.RUnlock()
	for _, fn := range l.fns {
		fn(id, hookType, inputFields)
	}
}
