package delivery

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"math/rand/v2"
	"sync"
	"testing"
	"time"

	"github.com/fyusufayan/insider-campaign-composer-case/backend/internal/platform/clock"
	"github.com/fyusufayan/insider-campaign-composer-case/backend/internal/provider"
)

// recordingSender is a fake provider: it evaluates the gate exactly where
// the real client does (immediately before Do) and records every request
// with the fake time at which it was issued.
type recordingSender struct {
	clk *clock.Fake
	mu  sync.Mutex
	at  []time.Time
	to  []string
}

func (s *recordingSender) SendGated(_ context.Context, m provider.Message, gate func() bool) (provider.Result, bool) {
	if gate != nil && !gate() {
		return provider.Result{}, false
	}
	s.mu.Lock()
	s.at = append(s.at, s.clk.Now())
	s.to = append(s.to, m.To)
	s.mu.Unlock()
	return provider.Result{Outcome: provider.OutcomeAccepted, HTTPStatus: 202, MessageID: "m"}, true
}

func (s *recordingSender) Capabilities() provider.Capabilities { return provider.MockCapabilities }

func (s *recordingSender) requests() []time.Time {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]time.Time(nil), s.at...)
}

var testGate = GateConfig{MaxPrecheck: 15 * time.Millisecond, MaxSlotToRequest: 20 * time.Millisecond, MaxAborts: 3}

func newTestGate(t *testing.T) (*gatedSender, *clock.Fake, *recordingSender) {
	t.Helper()
	clk := clock.NewFake(t0)
	snd := &recordingSender{clk: clk}
	l := newTestLimiter(t, clk, 100, 1050*time.Millisecond)
	return &gatedSender{clk: clk, limiters: map[provider.Channel]*Limiter{provider.ChannelSMS: l}, sender: snd, cfg: testGate}, clk, snd
}

var smsMsg = provider.Message{Channel: provider.ChannelSMS, IdempotencyKey: "c:1:sms", Text: "hi"}

type opsRecorder struct {
	prechecks, undos int
}

func (o *opsRecorder) ops(pre func(ctx context.Context, n int) (Precheck, error)) gateOps {
	return gateOps{
		precheck: func(ctx context.Context) (Precheck, error) { o.prechecks++; return pre(ctx, o.prechecks) },
		undo:     func() error { o.undos++; return nil },
	}
}

func okPrecheck() (Precheck, error) { return Precheck{Destination: "+905550000001"}, nil }

func TestGateSendsWithinBudget(t *testing.T) {
	g, _, snd := newTestGate(t)
	var rec opsRecorder
	r := g.Attempt(context.Background(), context.Background(), smsMsg, rec.ops(func(context.Context, int) (Precheck, error) { return okPrecheck() }))
	if r.kind != attemptSent || r.res.Outcome != provider.OutcomeAccepted || len(snd.requests()) != 1 || snd.to[0] != "+905550000001" {
		t.Fatalf("result = %+v, requests %v", r, snd.to)
	}
	if rec.undos != 0 || g.limiters[provider.ChannelSMS].Granted() != 1 {
		t.Errorf("undos = %d granted = %d", rec.undos, g.limiters[provider.ChannelSMS].Granted())
	}
}

// (a) A precheck that takes longer than DISPATCH_MAX_PRECHECK: no request,
// the slot is wasted (not refunded), the precheck undone, a new slot taken.
func TestGatePrecheckOverBudget(t *testing.T) {
	g, clk, snd := newTestGate(t)
	var rec opsRecorder
	r := g.Attempt(context.Background(), context.Background(), smsMsg, rec.ops(func(_ context.Context, n int) (Precheck, error) {
		if n == 1 {
			clk.Advance(16 * time.Millisecond) // slow database
		}
		return okPrecheck()
	}))
	if r.kind != attemptSent || r.aborts != 1 || rec.undos != 1 || rec.prechecks != 2 {
		t.Fatalf("result = %+v rec = %+v", r, rec)
	}
	if got := g.limiters[provider.ChannelSMS].Granted(); got != 2 {
		t.Errorf("granted = %d, want 2 (the wasted slot is never refunded)", got)
	}
	if len(snd.requests()) != 1 {
		t.Errorf("requests = %d", len(snd.requests()))
	}
}

// (a') A precheck that does not return within its budget is cancelled by
// the context deadline (real time) and treated as over budget.
func TestGatePrecheckDeadline(t *testing.T) {
	g, _, snd := newTestGate(t)
	g.cfg.MaxPrecheck, g.cfg.MaxSlotToRequest = 2*time.Millisecond, 3*time.Millisecond
	var rec opsRecorder
	r := g.Attempt(context.Background(), context.Background(), smsMsg, rec.ops(func(ctx context.Context, _ int) (Precheck, error) {
		<-ctx.Done() // a statement stuck on a lock
		return Precheck{}, ctx.Err()
	}))
	if r.kind != attemptAborted || r.aborts != 3 || rec.undos != 3 || len(snd.requests()) != 0 {
		t.Fatalf("result = %+v rec = %+v requests = %d", r, rec, len(snd.requests()))
	}
}

// (b) Fast precheck, then a process delay before the final gate past the
// slot deadline: no request.
func TestGateDelayAfterPrecheck(t *testing.T) {
	g, clk, snd := newTestGate(t)
	delays := 0
	g.afterPrecheck = func() {
		if delays == 0 {
			clk.Advance(21 * time.Millisecond) // GC pause / descheduled
		}
		delays++
	}
	var rec opsRecorder
	r := g.Attempt(context.Background(), context.Background(), smsMsg, rec.ops(func(context.Context, int) (Precheck, error) { return okPrecheck() }))
	if r.kind != attemptSent || r.aborts != 1 || rec.undos != 1 {
		t.Fatalf("result = %+v rec = %+v", r, rec)
	}
	reqs := snd.requests()
	if len(reqs) != 1 || !reqs[0].Equal(t0.Add(21*time.Millisecond)) {
		t.Fatalf("requests = %v", reqs)
	}
}

// (c) Three consecutive aborts: no request, attempt given up (the caller
// releases the row without counting an attempt).
func TestGateThreeAborts(t *testing.T) {
	g, clk, snd := newTestGate(t)
	g.afterPrecheck = func() { clk.Advance(time.Second) }
	var rec opsRecorder
	r := g.Attempt(context.Background(), context.Background(), smsMsg, rec.ops(func(context.Context, int) (Precheck, error) { return okPrecheck() }))
	if r.kind != attemptAborted || r.aborts != 3 || rec.prechecks != 3 || rec.undos != 3 || len(snd.requests()) != 0 {
		t.Fatalf("result = %+v rec = %+v requests = %d", r, rec, len(snd.requests()))
	}
}

// (d) Randomized sweep of delays in the precheck and after it: a request
// is never issued later than DISPATCH_MAX_SLOT_TO_REQUEST after its slot.
func TestGateNeverSendsAfterSlotDeadline(t *testing.T) {
	rng := rand.New(rand.NewPCG(1, 2))
	for i := 0; i < 500; i++ {
		g, clk, snd := newTestGate(t)
		var slot time.Time
		pre := time.Duration(rng.IntN(25)) * time.Millisecond
		post := time.Duration(rng.IntN(25)) * time.Millisecond
		g.afterPrecheck = func() { clk.Advance(post) }
		var rec opsRecorder
		r := g.Attempt(context.Background(), context.Background(), smsMsg, rec.ops(func(context.Context, int) (Precheck, error) {
			slot = clk.Now() // the slot was granted at the current fake time
			clk.Advance(pre)
			return okPrecheck()
		}))
		reqs := snd.requests()
		switch {
		case pre > testGate.MaxPrecheck || pre+post > testGate.MaxSlotToRequest:
			if len(reqs) != 0 || r.kind != attemptAborted {
				t.Fatalf("pre=%s post=%s: sent %v (%+v)", pre, post, reqs, r)
			}
		default:
			if len(reqs) != 1 || reqs[0].Sub(slot) > testGate.MaxSlotToRequest {
				t.Fatalf("pre=%s post=%s: requests %v slot %v", pre, post, reqs, slot)
			}
		}
	}
}

func TestGateMissAndErrors(t *testing.T) {
	g, _, snd := newTestGate(t)
	var rec opsRecorder
	if r := g.Attempt(context.Background(), context.Background(), smsMsg, rec.ops(func(context.Context, int) (Precheck, error) {
		return Precheck{}, ErrPrecheckMiss
	})); r.kind != attemptMiss {
		t.Errorf("miss = %+v", r)
	}
	boom := errors.New("connection refused")
	if r := g.Attempt(context.Background(), context.Background(), smsMsg, rec.ops(func(context.Context, int) (Precheck, error) {
		return Precheck{}, boom
	})); r.kind != attemptFailed || !errors.Is(r.err, boom) {
		t.Errorf("db error = %+v", r)
	}
	if len(snd.requests()) != 0 || rec.undos != 0 {
		t.Errorf("requests = %d undos = %d", len(snd.requests()), rec.undos)
	}
}

// Dispatch stopping (shutdown, lost dispatch lock) while waiting for a slot:
// no precheck, no request, no slot.
func TestGateCancelWhileWaitingForSlot(t *testing.T) {
	g, clk, snd := newTestGate(t)
	l := g.limiters[provider.ChannelSMS]
	l.Pause(t0.Add(time.Hour))
	ctx, cancel := context.WithCancel(context.Background())
	res := make(chan gateResult, 1)
	var rec opsRecorder
	go func() {
		res <- g.Attempt(ctx, context.Background(), smsMsg, rec.ops(func(context.Context, int) (Precheck, error) { return okPrecheck() }))
	}()
	if !clk.BlockUntilTimers(1, waitDeadline) {
		t.Fatal("not waiting")
	}
	cancel()
	r := <-res
	if r.kind != attemptFailed || !errors.Is(r.err, context.Canceled) || rec.prechecks != 0 || len(snd.requests()) != 0 || l.Granted() != 0 {
		t.Fatalf("result = %+v rec = %+v", r, rec)
	}
}

// The final gate refuses when the dispatch context ended between the
// precheck and the request (shutdown, lost lock): no request, the slot is
// wasted, and no new slot is taken.
func TestGateRefusesAfterDispatchStopped(t *testing.T) {
	g, _, snd := newTestGate(t)
	ctx, cancel := context.WithCancel(context.Background())
	g.afterPrecheck = cancel
	var rec opsRecorder
	r := g.Attempt(ctx, context.Background(), smsMsg, rec.ops(func(context.Context, int) (Precheck, error) { return okPrecheck() }))
	if r.kind != attemptFailed || !errors.Is(r.err, context.Canceled) || rec.undos != 1 || len(snd.requests()) != 0 {
		t.Fatalf("result = %+v rec = %+v requests = %d", r, rec, len(snd.requests()))
	}
	if got := g.limiters[provider.ChannelSMS].Granted(); got != 1 {
		t.Errorf("granted = %d", got)
	}
}

// The leadership fence is evaluated in the final gate with the gate's own
// clock reading: a fence that lapses between the slot and the request
// stops the request.
func TestGateLeadershipFence(t *testing.T) {
	g, clk, snd := newTestGate(t)
	fence := clk.Now().Add(5 * time.Millisecond)
	g.admit = func(now time.Time) bool { return !now.After(fence) }
	var rec opsRecorder
	r := g.Attempt(context.Background(), context.Background(), smsMsg, rec.ops(func(context.Context, int) (Precheck, error) {
		clk.Advance(6 * time.Millisecond) // within the precheck budget, past the fence
		return okPrecheck()
	}))
	if r.kind != attemptAborted || rec.undos != 3 || len(snd.requests()) != 0 {
		t.Fatalf("result = %+v rec = %+v requests = %d", r, rec, len(snd.requests()))
	}
}

// Worker.leaseFresh: a request may be issued only within LeaseFreshness
// (check interval + check timeout) of the start of the last successful
// lock-session statement — measured with the monotonic clock, independent
// of when the watcher runs — and never without a lease.
func TestWorkerLeaseFreshnessBoundary(t *testing.T) {
	clk := clock.NewFake(t0)
	w, err := New(&Repository{}, &recordingSender{clk: clk}, Config{WorkerID: "w", Clock: clk,
		LockCheckInterval: time.Second, LockCheckTimeout: 500 * time.Millisecond}, discardLog())
	if err != nil {
		t.Fatal(err)
	}
	if d := w.cfg.LeaseFreshness(); d != 1500*time.Millisecond || w.cfg.QuietPeriod() != d+w.cfg.RateWindow {
		t.Fatalf("freshness = %s quiet = %s", d, w.cfg.QuietPeriod())
	}
	if w.leaseFresh(t0) {
		t.Error("admitted without a lease")
	}
	w.setLeaseOK(t0)
	if !w.leaseFresh(t0.Add(1500*time.Millisecond)) || w.leaseFresh(t0.Add(1500*time.Millisecond+time.Nanosecond)) {
		t.Error("fence boundary")
	}
	// Wired into the worker's gate: a lapsed fence sends nothing.
	w.setLeaseOK(t0.Add(-1500*time.Millisecond + 5*time.Millisecond))
	var rec opsRecorder
	r := w.gate.Attempt(context.Background(), context.Background(), smsMsg, rec.ops(func(context.Context, int) (Precheck, error) {
		clk.Advance(6 * time.Millisecond)
		return okPrecheck()
	}))
	if r.kind != attemptAborted || len(w.sender.(*recordingSender).requests()) != 0 {
		t.Fatalf("result = %+v", r)
	}
}

func discardLog() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }
