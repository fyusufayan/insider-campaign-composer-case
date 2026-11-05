// Package clock is the injectable time source of the delivery worker
// (CLAUDE.md: "Use the injected Clock (monotonic) only for rate limiting and
// durations"). Real uses the Go runtime's monotonic clock: time.Time values
// returned by time.Now carry a monotonic reading, so Sub/Before/After are
// immune to wall-clock jumps. Fake is a manually advanced clock for
// deterministic tests.
package clock

import (
	"sort"
	"sync"
	"time"
)

// Clock is a monotonic time source with timers.
type Clock interface {
	Now() time.Time
	// NewTimer returns a timer that fires once d has elapsed.
	NewTimer(d time.Duration) Timer
}

// Timer is a one-shot timer.
type Timer interface {
	C() <-chan time.Time
	// Stop prevents the timer from firing; it reports whether it was active.
	Stop() bool
}

// Real is the process's monotonic clock.
type Real struct{}

// Now returns time.Now() (with its monotonic reading).
func (Real) Now() time.Time { return time.Now() }

// NewTimer returns a runtime timer.
func (Real) NewTimer(d time.Duration) Timer { return realTimer{time.NewTimer(d)} }

type realTimer struct{ t *time.Timer }

func (r realTimer) C() <-chan time.Time { return r.t.C }
func (r realTimer) Stop() bool          { return r.t.Stop() }

// Fake is a clock that only moves when Advance or Set is called. It is safe
// for concurrent use. Timers fire (non-blocking send on a 1-buffered
// channel) when the clock reaches their deadline.
type Fake struct {
	mu     sync.Mutex
	now    time.Time
	timers []*fakeTimer
	// changed is closed and replaced whenever the timer set changes (BlockUntilTimers).
	changed chan struct{}
}

// NewFake returns a fake clock at start.
func NewFake(start time.Time) *Fake { return &Fake{now: start, changed: make(chan struct{})} }

// Now returns the fake time.
func (f *Fake) Now() time.Time {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.now
}

// NewTimer registers a timer; a non-positive d fires immediately.
func (f *Fake) NewTimer(d time.Duration) Timer {
	f.mu.Lock()
	defer f.mu.Unlock()
	t := &fakeTimer{f: f, at: f.now.Add(d), c: make(chan time.Time, 1)}
	if d <= 0 {
		t.c <- f.now
		return t
	}
	t.active = true
	f.timers = append(f.timers, t)
	f.notifyLocked()
	return t
}

// Advance moves the clock forward by d and fires due timers.
func (f *Fake) Advance(d time.Duration) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.setLocked(f.now.Add(d))
}

// Set moves the clock to t (never backwards) and fires due timers.
func (f *Fake) Set(t time.Time) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if t.After(f.now) {
		f.setLocked(t)
	}
}

func (f *Fake) setLocked(t time.Time) {
	f.now = t
	sort.SliceStable(f.timers, func(i, j int) bool { return f.timers[i].at.Before(f.timers[j].at) })
	keep := f.timers[:0]
	for _, tm := range f.timers {
		if !tm.at.After(t) {
			tm.active = false
			select {
			case tm.c <- t:
			default:
			}
			continue
		}
		keep = append(keep, tm)
	}
	for i := len(keep); i < len(f.timers); i++ {
		f.timers[i] = nil
	}
	f.timers = keep
}

// Timers returns the number of active timers.
func (f *Fake) Timers() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.timers)
}

// BlockUntilTimers waits until at least n timers are active or the
// deadline (real time) passes; it reports whether n was reached. Tests use
// it to know that goroutines are parked on the clock before advancing it.
func (f *Fake) BlockUntilTimers(n int, deadline time.Duration) bool {
	stop := time.NewTimer(deadline)
	defer stop.Stop()
	for {
		f.mu.Lock()
		if len(f.timers) >= n {
			f.mu.Unlock()
			return true
		}
		ch := f.changed
		f.mu.Unlock()
		select {
		case <-ch:
		case <-stop.C:
			return false
		}
	}
}

func (f *Fake) notifyLocked() {
	close(f.changed)
	f.changed = make(chan struct{})
}

type fakeTimer struct {
	f      *Fake
	at     time.Time
	c      chan time.Time
	active bool
}

func (t *fakeTimer) C() <-chan time.Time { return t.c }

func (t *fakeTimer) Stop() bool {
	t.f.mu.Lock()
	defer t.f.mu.Unlock()
	if !t.active {
		return false
	}
	t.active = false
	for i, x := range t.f.timers {
		if x == t {
			t.f.timers = append(t.f.timers[:i], t.f.timers[i+1:]...)
			break
		}
	}
	t.f.notifyLocked()
	return true
}
