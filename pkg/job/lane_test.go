package job

import (
	"context"
	"testing"
	"time"
)

func waitStarted(t *testing.T, e *testExec, name string) {
	t.Helper()
	select {
	case <-e.started:
	case <-time.After(time.Second):
		t.Fatalf("%s did not start", name)
	}
}

func assertNotStarted(t *testing.T, e *testExec, name string) {
	t.Helper()
	select {
	case <-e.started:
		t.Fatalf("%s started but should be waiting", name)
	case <-time.After(sleepTime):
	}
}

func TestLanesRunConcurrently(t *testing.T) {
	m := NewManager()
	ctx := context.Background()

	scan := newTestExec(make(chan struct{}))
	identify := newTestExec(make(chan struct{}))
	generate := newTestExec(make(chan struct{}))

	m.AddInLane(ctx, LaneLibrary, "scan", scan)
	m.AddInLane(ctx, LaneMetadata, "identify", identify)
	m.AddInLane(ctx, LaneLibrary, "generate", generate)

	// identify does not wait for the scan; generate waits behind it
	waitStarted(t, scan, "scan")
	waitStarted(t, identify, "identify")
	assertNotStarted(t, generate, "generate")

	close(scan.finish)
	waitStarted(t, generate, "generate")

	close(identify.finish)
	close(generate.finish)
}

func TestExclusiveLane(t *testing.T) {
	m := NewManager()
	ctx := context.Background()

	scan := newTestExec(make(chan struct{}))
	migrate := newTestExec(make(chan struct{}))
	identify := newTestExec(make(chan struct{}))

	m.AddInLane(ctx, LaneLibrary, "scan", scan)
	m.AddInLane(ctx, LaneExclusive, "migrate", migrate)
	// queued after the exclusive job: must wait for it even though its own
	// lane is free
	m.AddInLane(ctx, LaneMetadata, "identify", identify)

	waitStarted(t, scan, "scan")
	assertNotStarted(t, migrate, "migrate")
	assertNotStarted(t, identify, "identify")

	close(scan.finish)
	waitStarted(t, migrate, "migrate")
	assertNotStarted(t, identify, "identify")

	close(migrate.finish)
	waitStarted(t, identify, "identify")
	close(identify.finish)
}

func TestCancelQueuedExclusiveUnblocks(t *testing.T) {
	m := NewManager()
	ctx := context.Background()

	scan := newTestExec(make(chan struct{}))
	migrate := newTestExec(make(chan struct{}))
	identify := newTestExec(make(chan struct{}))

	m.AddInLane(ctx, LaneLibrary, "scan", scan)
	migrateID := m.AddInLane(ctx, LaneExclusive, "migrate", migrate)
	m.AddInLane(ctx, LaneMetadata, "identify", identify)

	waitStarted(t, scan, "scan")
	assertNotStarted(t, identify, "identify")

	m.CancelJob(migrateID)
	waitStarted(t, identify, "identify")

	close(scan.finish)
	close(identify.finish)
}
