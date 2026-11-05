package delivery

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/fyusufayan/insider-campaign-composer-case/backend/internal/platform/clock"
)

// Limiter is the per-channel sliding-window-log rate limiter (ARCHITECTURE
// §10.4, ADR-011). It grants at most N slots in any interval shorter than
// the window W: a slot is granted at time t only if the N-th previous grant
// is at least W before t (and t is not inside a pause). Check and record
// happen atomically under one mutex, so concurrent callers cannot bypass
// it. Time comes from an injected monotonic clock.
//
// Guarantee (slots): for grants g_i, g_{i+N} − g_i ≥ W. With W = 1 s +
// RATE_SAFETY_MARGIN and every request issued within
// DISPATCH_MAX_SLOT_TO_REQUEST (G) of its slot (the final gate in
// gatedSender.Attempt), requests i and i+N leave this process at least
// W − G apart; the margin also covers the transport jitter J (assumed, and
// measured by the provider-side integration test, not proven).
//
// Fairness: waiters are served strictly first-in, first-out. Only the head
// of the queue may take the next slot; the others wait for it, so no caller
// starves and the order in which rows were claimed is the order in which
// they get slots. A caller whose context ends leaves the queue (a slot that
// was granted is never given back: wasted slots are cheaper than a risk to
// the rate guarantee).
type Limiter struct {
	clk    clock.Clock
	n      int
	window time.Duration

	mu          sync.Mutex
	ring        []time.Time // the last n grants; ring[next] is the oldest once filled
	filled      int
	next        int
	pausedUntil time.Time
	queue       []*limiterWaiter
	granted     int64
}

type limiterWaiter struct {
	wake chan struct{} // 1-buffered: "you may be the head now"
}

// ErrLimiterConfig reports an invalid limiter configuration.
var ErrLimiterConfig = errors.New("delivery: limiter needs n ≥ 1 and a positive window")

// NewLimiter returns a limiter granting at most n slots per window.
func NewLimiter(clk clock.Clock, n int, window time.Duration) (*Limiter, error) {
	if clk == nil || n < 1 || window <= 0 {
		return nil, ErrLimiterConfig
	}
	return &Limiter{clk: clk, n: n, window: window, ring: make([]time.Time, n)}, nil
}

// readyAtLocked is the earliest time the next slot may be granted.
func (l *Limiter) readyAtLocked() time.Time {
	ready := l.pausedUntil
	if l.filled == l.n {
		if t := l.ring[l.next].Add(l.window); t.After(ready) {
			ready = t
		}
	}
	return ready
}

// Acquire waits for a slot and returns the time it was granted (the slot
// time t0 from which the slot deadline is measured). It returns ctx.Err()
// if ctx ends first; no slot is consumed then.
func (l *Limiter) Acquire(ctx context.Context) (time.Time, error) {
	if err := ctx.Err(); err != nil {
		return time.Time{}, err
	}
	w := &limiterWaiter{wake: make(chan struct{}, 1)}
	l.mu.Lock()
	l.queue = append(l.queue, w)
	for {
		if l.queue[0] != w {
			l.mu.Unlock()
			select {
			case <-w.wake:
			case <-ctx.Done():
				l.leave(w)
				return time.Time{}, ctx.Err()
			}
			l.mu.Lock()
			continue
		}
		now := l.clk.Now()
		ready := l.readyAtLocked()
		if !now.Before(ready) {
			l.ring[l.next] = now
			l.next = (l.next + 1) % l.n
			if l.filled < l.n {
				l.filled++
			}
			l.granted++
			l.queue = l.queue[1:]
			l.wakeHeadLocked()
			l.mu.Unlock()
			return now, nil
		}
		t := l.clk.NewTimer(ready.Sub(now))
		l.mu.Unlock()
		select {
		case <-t.C():
		case <-w.wake: // spurious for the head; re-check
			t.Stop()
		case <-ctx.Done():
			t.Stop()
			l.leave(w)
			return time.Time{}, ctx.Err()
		}
		l.mu.Lock()
	}
}

// leave removes w from the queue after its context ended; if it was the
// head, the next waiter becomes the head.
func (l *Limiter) leave(w *limiterWaiter) {
	l.mu.Lock()
	defer l.mu.Unlock()
	for i, x := range l.queue {
		if x == w {
			l.queue = append(l.queue[:i], l.queue[i+1:]...)
			if i == 0 {
				l.wakeHeadLocked()
			}
			return
		}
	}
}

func (l *Limiter) wakeHeadLocked() {
	if len(l.queue) == 0 {
		return
	}
	select {
	case l.queue[0].wake <- struct{}{}:
	default:
	}
}

// Pause blocks new grants until until (monotonic clock of this limiter).
// It only extends a pause, never shortens it, and affects this limiter
// (this channel) only. Grants already made are not revoked: requests in
// flight finish.
func (l *Limiter) Pause(until time.Time) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if until.After(l.pausedUntil) {
		l.pausedUntil = until
	}
}

// PausedUntil returns the end of the current pause (zero if never paused).
func (l *Limiter) PausedUntil() time.Time {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.pausedUntil
}

// Waiting returns the number of callers queued in Acquire (tests, metrics).
func (l *Limiter) Waiting() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.queue)
}

// Granted returns the number of slots granted so far.
func (l *Limiter) Granted() int64 {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.granted
}
