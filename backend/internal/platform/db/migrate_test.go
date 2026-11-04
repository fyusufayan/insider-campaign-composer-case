package db

import (
	"context"
	"errors"
	"testing"
	"time"
)

// deadline bounds every wait in these tests; reaching it means a goroutine is stuck.
const deadline = 5 * time.Second

// returnsWithin fails the test if f does not return before the deadline.
func returnsWithin(t *testing.T, what string, f func()) {
	t.Helper()
	done := make(chan struct{})
	go func() {
		defer close(done)
		f()
	}()
	select {
	case <-done:
	case <-time.After(deadline):
		t.Fatalf("%s did not return within %v (watcher goroutine blocked)", what, deadline)
	}
}

func TestStopOnCancelSendsOneStopRequest(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	stop := make(chan bool, 1) // as created by golang-migrate v4.19.1
	release := stopOnCancel(ctx, stop)
	cancel()
	select {
	case v := <-stop:
		if !v {
			t.Error("stop request = false, want true")
		}
	case <-time.After(deadline):
		t.Fatal("no stop request after cancellation")
	}
	returnsWithin(t, "release", release)
	if len(stop) != 0 {
		t.Errorf("%d extra stop requests", len(stop))
	}
}

// The original implementation's hazard: a send nobody can receive. The watcher
// must drop it instead of blocking, so release (which joins the goroutine) returns.
func TestStopOnCancelNeverBlocksOnUnreadChannel(t *testing.T) {
	for name, stop := range map[string]chan bool{
		"unbuffered without reader": make(chan bool),
		"full buffer":               func() chan bool { c := make(chan bool, 1); c <- true; return c }(),
	} {
		t.Run(name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			release := stopOnCancel(ctx, stop)
			cancel()
			returnsWithin(t, "release", release)
		})
	}
}

// Release before cancellation (normal completion): nothing is sent, and a later
// cancellation cannot reach the already-closed migrator.
func TestStopOnCancelReleaseBeforeCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	stop := make(chan bool, 1)
	release := stopOnCancel(ctx, stop)
	returnsWithin(t, "release", release)
	cancel()
	returnsWithin(t, "second release", release) // idempotent
	if len(stop) != 0 {
		t.Error("stop request sent although the watcher was released before cancellation")
	}
}

// Cancellation and release racing each other: whichever wins, release returns and
// at most one stop request is sent. Run with -race.
func TestStopOnCancelRaceWithRelease(t *testing.T) {
	for i := 0; i < 500; i++ {
		ctx, cancel := context.WithCancel(context.Background())
		stop := make(chan bool, 1)
		release := stopOnCancel(ctx, stop)
		go cancel()
		returnsWithin(t, "release", release)
		cancel()
		if len(stop) > 1 {
			t.Fatalf("iteration %d: %d stop requests", i, len(stop))
		}
	}
}

// An already-cancelled context fails before any migration work starts.
func TestMigrateUpWithCancelledContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	// Unroutable address: the cancelled context must win without a network wait.
	err := MigrateUp(ctx, "postgres://u:p@192.0.2.1:5432/db?connect_timeout=30")
	if err == nil {
		t.Fatal("MigrateUp with a cancelled context succeeded")
	}
	if !errors.Is(err, context.Canceled) {
		t.Errorf("err = %v, want a context cancellation error", err)
	}
}
