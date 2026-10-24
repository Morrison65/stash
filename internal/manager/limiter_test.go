package manager

import (
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestLimiterBoundsConcurrency(t *testing.T) {
	l := NewLimiter(4)

	var running, peak atomic.Int32
	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			release := l.Acquire()
			defer release()
			n := running.Add(1)
			for {
				p := peak.Load()
				if n <= p || peak.CompareAndSwap(p, n) {
					break
				}
			}
			time.Sleep(20 * time.Millisecond)
			running.Add(-1)
		}()
	}
	wg.Wait()

	if peak.Load() != 4 {
		t.Errorf("peak concurrency = %d, want 4", peak.Load())
	}
}

func TestLimiterResizeWhileHeld(t *testing.T) {
	l := NewLimiter(1)
	release := l.Acquire()

	// resizing must not strand the current holder or block new acquirers
	l.Resize(3)
	if l.Size() != 3 {
		t.Fatalf("size = %d", l.Size())
	}

	done := make(chan struct{})
	go func() {
		r1 := l.Acquire()
		r2 := l.Acquire()
		r1()
		r2()
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("acquire after resize blocked")
	}

	// releasing into the old semaphore must not block
	release()
	release() // idempotent
}
