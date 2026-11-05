package delivery

import (
	"context"
	"errors"
	"runtime"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/fyusufayan/insider-campaign-composer-case/backend/internal/platform/clock"
)

var t0 = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

const waitDeadline = 5 * time.Second

func newTestLimiter(t *testing.T, clk clock.Clock, n int, w time.Duration) *Limiter {
	t.Helper()
	l, err := NewLimiter(clk, n, w)
	if err != nil {
		t.Fatal(err)
	}
	return l
}

// waitFor polls cond (bounded by a real-time deadline; no sleeping).
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(waitDeadline)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		runtimeYield()
	}
}

func runtimeYield() { runtime.Gosched() }

// maxInWindow is the largest number of grants in any half-open interval
// [t, t+w) — computed with two pointers over the sorted grant times.
func maxInWindow(ts []time.Time, w time.Duration) int {
	ts = slices.Clone(ts)
	slices.SortFunc(ts, func(a, b time.Time) int { return a.Compare(b) })
	best, j := 0, 0
	for i := range ts {
		for ts[i].Sub(ts[j]) >= w {
			j++
		}
		best = max(best, i-j+1)
	}
	return best
}

func TestLimiterRejectsBadConfig(t *testing.T) {
	for _, tc := range []struct {
		n int
		w time.Duration
	}{{0, time.Second}, {1, 0}, {-1, time.Second}} {
		if _, err := NewLimiter(clock.NewFake(t0), tc.n, tc.w); !errors.Is(err, ErrLimiterConfig) {
			t.Errorf("n=%d w=%s: %v", tc.n, tc.w, err)
		}
	}
}

// The (N+1)-th grant waits until the first is W old; the boundary is
// inclusive (exactly W later is allowed).
func TestLimiterSlidingWindowBoundary(t *testing.T) {
	clk := clock.NewFake(t0)
	l := newTestLimiter(t, clk, 3, time.Second)
	for i := 0; i < 3; i++ {
		if got, err := l.Acquire(context.Background()); err != nil || !got.Equal(t0) {
			t.Fatalf("grant %d = %v %v", i, got, err)
		}
	}
	res := make(chan time.Time, 1)
	go func() {
		got, _ := l.Acquire(context.Background())
		res <- got
	}()
	if !clk.BlockUntilTimers(1, waitDeadline) {
		t.Fatal("4th caller did not wait")
	}
	clk.Advance(999 * time.Millisecond)
	select {
	case got := <-res:
		t.Fatalf("granted early at %v", got)
	default:
	}
	clk.Advance(time.Millisecond)
	if got := <-res; !got.Equal(t0.Add(time.Second)) {
		t.Fatalf("4th grant at %v", got)
	}
	if l.Granted() != 4 {
		t.Errorf("granted = %d", l.Granted())
	}
}

// 1,000 acquisitions from 50 goroutines: never more than N in any window W
// (checked over the recorded grant times), all granted. Run with -race.
func TestLimiterConcurrentNeverExceedsN(t *testing.T) {
	const (
		n, goroutines, each = 100, 50, 20
		w                   = 1050 * time.Millisecond
	)
	clk := clock.NewFake(t0)
	l := newTestLimiter(t, clk, n, w)
	var (
		mu     sync.Mutex
		grants []time.Time
		done   atomic.Int64
		wg     sync.WaitGroup
	)
	for g := 0; g < goroutines; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < each; i++ {
				at, err := l.Acquire(context.Background())
				if err != nil {
					t.Error(err)
					return
				}
				mu.Lock()
				grants = append(grants, at)
				mu.Unlock()
				done.Add(1)
			}
		}()
	}
	// Drive the clock while anyone waits on it.
	deadline := time.Now().Add(30 * time.Second)
	for done.Load() < goroutines*each {
		if time.Now().After(deadline) {
			t.Fatalf("stalled at %d grants", done.Load())
		}
		if clk.BlockUntilTimers(1, 5*time.Millisecond) {
			clk.Advance(time.Millisecond)
		}
	}
	wg.Wait()
	if len(grants) != goroutines*each {
		t.Fatalf("grants = %d", len(grants))
	}
	if m := maxInWindow(grants, w); m > n {
		t.Fatalf("%d grants in one window of %s (limit %d)", m, w, n)
	}
	// It is not merely slow: ~N per window were granted.
	span := maxTime(grants).Sub(minTime(grants))
	if minSpan := time.Duration(goroutines*each/n-1) * w; span < minSpan || span > minSpan+2*w {
		t.Errorf("span = %s, want ≈ %s", span, minSpan)
	}
}

func maxTime(ts []time.Time) time.Time {
	return slices.MaxFunc(ts, func(a, b time.Time) int { return a.Compare(b) })
}
func minTime(ts []time.Time) time.Time {
	return slices.MinFunc(ts, func(a, b time.Time) int { return a.Compare(b) })
}

// Waiters are served first-in, first-out (no starvation).
func TestLimiterFIFO(t *testing.T) {
	clk := clock.NewFake(t0)
	l := newTestLimiter(t, clk, 1, time.Second)
	if _, err := l.Acquire(context.Background()); err != nil {
		t.Fatal(err)
	}
	order := make(chan int, 5)
	for i := 0; i < 5; i++ {
		go func() {
			if _, err := l.Acquire(context.Background()); err == nil {
				order <- i
			}
		}()
		waitFor(t, "waiter queued", func() bool { return l.Waiting() == i+1 })
	}
	for i := 0; i < 5; i++ {
		waitFor(t, "head on a timer", func() bool { return clk.Timers() == 1 })
		clk.Advance(time.Second)
		if got := <-order; got != i {
			t.Fatalf("grant %d went to waiter %d", i, got)
		}
	}
}

// A waiter whose context ends leaves the queue without consuming a slot;
// the next waiter is served.
func TestLimiterCancelWhileWaiting(t *testing.T) {
	clk := clock.NewFake(t0)
	l := newTestLimiter(t, clk, 1, time.Second)
	if _, err := l.Acquire(context.Background()); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	headErr := make(chan error, 1)
	go func() { _, err := l.Acquire(ctx); headErr <- err }()
	waitFor(t, "head queued", func() bool { return l.Waiting() == 1 })
	second := make(chan time.Time, 1)
	go func() { at, _ := l.Acquire(context.Background()); second <- at }()
	waitFor(t, "second queued", func() bool { return l.Waiting() == 2 })

	cancel()
	if err := <-headErr; !errors.Is(err, context.Canceled) {
		t.Fatalf("head err = %v", err)
	}
	waitFor(t, "head left", func() bool { return l.Waiting() == 1 })
	if l.Granted() != 1 {
		t.Errorf("a cancelled waiter consumed a slot: granted = %d", l.Granted())
	}
	waitFor(t, "second on a timer", func() bool { return clk.Timers() == 1 })
	clk.Advance(time.Second)
	if at := <-second; !at.Equal(t0.Add(time.Second)) {
		t.Errorf("second granted at %v", at)
	}
	// An already cancelled context never waits.
	if _, err := l.Acquire(ctx); !errors.Is(err, context.Canceled) {
		t.Errorf("cancelled ctx = %v", err)
	}
}

// Pause blocks only its own limiter, only extends, and grants resume at
// the pause end.
func TestLimiterPauseIsolation(t *testing.T) {
	clk := clock.NewFake(t0)
	sms := newTestLimiter(t, clk, 10, time.Second)
	email := newTestLimiter(t, clk, 10, time.Second)
	sms.Pause(t0.Add(5 * time.Second))
	sms.Pause(t0.Add(2 * time.Second)) // never shortens
	if !sms.PausedUntil().Equal(t0.Add(5 * time.Second)) {
		t.Fatalf("paused until %v", sms.PausedUntil())
	}
	if at, err := email.Acquire(context.Background()); err != nil || !at.Equal(t0) {
		t.Fatalf("email blocked by an sms pause: %v %v", at, err)
	}
	got := make(chan time.Time, 1)
	go func() { at, _ := sms.Acquire(context.Background()); got <- at }()
	if !clk.BlockUntilTimers(1, waitDeadline) {
		t.Fatal("sms did not wait")
	}
	clk.Advance(4 * time.Second)
	select {
	case at := <-got:
		t.Fatalf("sms granted during the pause at %v", at)
	default:
	}
	if at, err := email.Acquire(context.Background()); err != nil || !at.Equal(t0.Add(4*time.Second)) {
		t.Fatalf("email during sms pause: %v %v", at, err)
	}
	clk.Advance(time.Second)
	if at := <-got; !at.Equal(t0.Add(5 * time.Second)) {
		t.Errorf("sms granted at %v", at)
	}
}
