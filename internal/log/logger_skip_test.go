package log

import (
	"testing"
	"time"
)

func TestTraceSkippedWithoutViewer(t *testing.T) {
	l := NewLogger()
	l.SetLogLevel("Info")

	l.Tracef("SQL %s", "nobody listens")
	l.mutex.Lock()
	buffered := len(l.logBuffer)
	l.mutex.Unlock()
	if buffered != 0 {
		t.Fatalf("trace message buffered without a viewer: %d", buffered)
	}

	stop := make(chan int)
	ch := l.SubscribeToLog(stop)
	l.Tracef("SQL %s", "viewer listens")
	select {
	case items := <-ch:
		if len(items) != 1 || items[0].Type != "trace" {
			t.Fatalf("viewer got %+v", items)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("viewer got no trace message")
	}
	close(stop)
	time.Sleep(50 * time.Millisecond)
	if n := l.subscribers.Load(); n != 0 {
		t.Fatalf("subscribers after unsubscribe: %d", n)
	}
}
