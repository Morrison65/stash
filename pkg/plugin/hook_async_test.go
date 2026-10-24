package plugin

import (
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stashapp/stash/pkg/plugin/hook"
)

func TestParseTrigger(t *testing.T) {
	for in, want := range map[hook.TriggerEnum]struct {
		base hook.TriggerEnum
		sync bool
	}{
		"Scene.Update.Post":      {hook.SceneUpdatePost, false},
		"sync:Scene.Update.Post": {hook.SceneUpdatePost, true},
	} {
		base, sync := parseTrigger(in)
		if base != want.base || sync != want.sync {
			t.Errorf("parseTrigger(%q) = %q, %v", in, base, sync)
		}
	}
}

func TestLoadSyncTriggerFromYAML(t *testing.T) {
	const yml = `
name: test
exec: [python, test.py]
interface: raw
hooks:
  - name: async hook
    triggeredBy:
      - Scene.Update.Post
  - name: sync hook
    triggeredBy:
      - sync:Scene.Update.Post
      - Scene.Create.Post
`
	c, err := loadPluginFromYAML(strings.NewReader(yml))
	if err != nil {
		t.Fatal(err)
	}

	matches := c.getHooks(hook.SceneUpdatePost)
	if len(matches) != 2 {
		t.Fatalf("want 2 hooks for Scene.Update.Post, got %d", len(matches))
	}
	if matches[0].Name != "async hook" || matches[0].sync {
		t.Errorf("first hook should be the async one: %+v", matches[0])
	}
	if matches[1].Name != "sync hook" || !matches[1].sync {
		t.Errorf("second hook should be sync: %+v", matches[1])
	}

	created := c.getHooks(hook.SceneCreatePost)
	if len(created) != 1 || created[0].sync {
		t.Errorf("Scene.Create.Post: %+v", created)
	}
}

func TestAsyncHookRunnerOrderAndParallelism(t *testing.T) {
	r := newAsyncHookRunner()

	var mu sync.Mutex
	var order []int

	// plugin a: slow first event must still finish before the second
	r.enqueue("a", func() {
		time.Sleep(50 * time.Millisecond)
		mu.Lock()
		order = append(order, 1)
		mu.Unlock()
	})
	r.enqueue("a", func() {
		mu.Lock()
		order = append(order, 2)
		mu.Unlock()
	})

	// plugin b runs in parallel and does not wait for plugin a
	bDone := make(chan struct{})
	r.enqueue("b", func() { close(bDone) })

	select {
	case <-bDone:
	case <-time.After(40 * time.Millisecond):
		t.Error("plugin b was blocked by plugin a")
	}

	r.Wait()
	if len(order) != 2 || order[0] != 1 || order[1] != 2 {
		t.Errorf("per-plugin order not preserved: %v", order)
	}
}

func TestAsyncHookRunnerRecoversPanics(t *testing.T) {
	r := newAsyncHookRunner()
	r.enqueue("a", func() { panic("boom") })
	ran := false
	r.enqueue("a", func() { ran = true })
	r.Wait()
	if !ran {
		t.Error("queue stopped after a panicking hook")
	}
}
