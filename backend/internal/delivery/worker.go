package delivery

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"sync"
	"time"

	"github.com/fyusufayan/insider-campaign-composer-case/backend/internal/content"
	"github.com/fyusufayan/insider-campaign-composer-case/backend/internal/platform/clock"
	"github.com/fyusufayan/insider-campaign-composer-case/backend/internal/provider"
)

// Sender is the provider client (the only interface of this package: tests
// substitute it). SendGated must call gate immediately before the HTTP
// request and send nothing if it returns false (sent = false); otherwise
// it must not return before the request is finished or abandoned, and must
// classify every failure as an outcome.
type Sender interface {
	SendGated(ctx context.Context, m provider.Message, gate func() bool) (provider.Result, bool)
	Capabilities() provider.Capabilities
}

// Config configures the worker.
type Config struct {
	// WorkerID identifies this process in leased_by; unique per process
	// start (cmd/worker appends a random suffix), so a restarted worker
	// never mistakes its predecessor's leases for its own.
	WorkerID string
	// Channels to dispatch (default sms and email), one dispatcher each.
	Channels []provider.Channel
	// PoolSize is the number of concurrent deliveries per channel
	// (SENDER_POOL_SIZE).
	PoolSize int
	// ClaimBatch bounds one claim (CLAIM_BATCH_SIZE); a claim never asks
	// for more rows than the pool has free slots, so claimed rows wait at
	// most for the current deliveries, never for an unbounded backlog.
	ClaimBatch int
	// Lease is the claim lease, renewed by the precheck (LEASE_DURATION);
	// it must be much longer than a request.
	Lease time.Duration
	// PollInterval is how long a dispatcher waits after finding no work
	// (WORKER_POLL_INTERVAL).
	PollInterval time.Duration
	// ReaperInterval, CompletionInterval, HeartbeatInterval: periods of the
	// reaper, the completion detector and the heartbeat.
	ReaperInterval     time.Duration
	CompletionInterval time.Duration
	HeartbeatInterval  time.Duration
	// ShutdownGrace bounds how long in-progress requests may finish after
	// shutdown starts; then they are aborted (recorded as unknown).
	ShutdownGrace time.Duration
	// DBTimeout bounds every worker statement or transaction except the
	// precheck (bounded by the gate).
	DBTimeout time.Duration
	Policy    Policy

	// RatePerSecond is N (RATE_LIMIT_PER_SECOND): at most N slots per
	// channel in any window shorter than RateWindow.
	RatePerSecond int
	// RateWindow is W = 1 s + RATE_SAFETY_MARGIN.
	RateWindow time.Duration
	// Gate bounds the slot-to-request interval (DISPATCH_MAX_PRECHECK,
	// DISPATCH_MAX_SLOT_TO_REQUEST); it requires MaxPrecheck ≤
	// MaxSlotToRequest < RateWindow − 1 s.
	Gate GateConfig

	// DispatchLockRetry is how often a standby process tries to take the
	// dispatch lock; LockCheckInterval/LockCheckTimeout bound how late the
	// holder notices a lost lock session (ADR-025).
	DispatchLockRetry time.Duration
	LockCheckInterval time.Duration
	LockCheckTimeout  time.Duration

	// Clock is the monotonic clock of the limiter and gate (default real).
	Clock clock.Clock
	// Rand returns uniform samples in [0, 1) for retry jitter (default
	// math/rand/v2, safe for concurrent use).
	Rand func() float64
}

// Defaults (§10.1, §10.2, §10.4, §10.7, §16).
const (
	DefaultPoolSize           = 20
	DefaultClaimBatch         = 50
	DefaultLease              = 60 * time.Second
	DefaultPollInterval       = 500 * time.Millisecond
	DefaultReaperInterval     = 10 * time.Second
	DefaultCompletionInterval = 5 * time.Second
	DefaultHeartbeatInterval  = 5 * time.Second
	DefaultShutdownGrace      = 10 * time.Second
	DefaultDBTimeout          = 10 * time.Second
	DefaultRatePerSecond      = 100
	DefaultRateSafetyMargin   = 50 * time.Millisecond
	DefaultDispatchLockRetry  = time.Second
	DefaultLockCheckInterval  = time.Second
	DefaultLockCheckTimeout   = time.Second
	reapBatch                 = 100
	completionScan            = 100
	contentCacheSize          = 256
)

func (c Config) withDefaults() Config {
	if len(c.Channels) == 0 {
		c.Channels = []provider.Channel{provider.ChannelSMS, provider.ChannelEmail}
	}
	def := func(d *time.Duration, v time.Duration) {
		if *d == 0 {
			*d = v
		}
	}
	if c.PoolSize == 0 {
		c.PoolSize = DefaultPoolSize
	}
	if c.ClaimBatch == 0 {
		c.ClaimBatch = DefaultClaimBatch
	}
	if c.RatePerSecond == 0 {
		c.RatePerSecond = DefaultRatePerSecond
	}
	if c.Gate.MaxAborts == 0 {
		c.Gate.MaxAborts = DefaultMaxAborts
	}
	def(&c.Lease, DefaultLease)
	def(&c.PollInterval, DefaultPollInterval)
	def(&c.ReaperInterval, DefaultReaperInterval)
	def(&c.CompletionInterval, DefaultCompletionInterval)
	def(&c.HeartbeatInterval, DefaultHeartbeatInterval)
	def(&c.ShutdownGrace, DefaultShutdownGrace)
	def(&c.DBTimeout, DefaultDBTimeout)
	def(&c.RateWindow, time.Second+DefaultRateSafetyMargin)
	def(&c.Gate.MaxPrecheck, DefaultMaxPrecheck)
	def(&c.Gate.MaxSlotToRequest, DefaultMaxSlotToRequest)
	def(&c.DispatchLockRetry, DefaultDispatchLockRetry)
	def(&c.LockCheckInterval, DefaultLockCheckInterval)
	def(&c.LockCheckTimeout, DefaultLockCheckTimeout)
	if c.Policy == (Policy{}) {
		c.Policy = DefaultPolicy
	}
	if c.Clock == nil {
		c.Clock = clock.Real{}
	}
	if c.Rand == nil {
		c.Rand = rand.Float64
	}
	return c
}

// QuietPeriod is how long a process waits after taking the dispatch lock
// before its first slot (ADR-025): long enough that the previous holder —
// which stops granting slots at most LockCheckInterval + LockCheckTimeout
// after it lost the lock and issues no request later than
// MaxSlotToRequest after a slot — has no request within one rate window of
// ours. It also covers a crash-restart of the same worker, whose limiter
// state is lost.
func (c Config) QuietPeriod() time.Duration {
	c = c.withDefaults()
	return c.LockCheckInterval + c.LockCheckTimeout + c.RateWindow
}

// Worker processes released delivery rows.
type Worker struct {
	repo   *Repository
	sender Sender
	cfg    Config
	log    *slog.Logger
	gate   *gatedSender

	contentMu sync.Mutex
	contents  map[string]content.Content // approval id → approved content (immutable)

	completionKick chan struct{}

	stateMu    sync.Mutex
	dispatcher bool // holds the dispatch lock and dispatches
	// leaseOK is the clock reading taken just BEFORE the latest successful
	// statement on the dispatch-lock session (acquisition or check); zero
	// when no lease is held. The lock was certainly held at that moment.
	leaseOK time.Time
}

// New returns a worker.
func New(repo *Repository, sender Sender, cfg Config, log *slog.Logger) (*Worker, error) {
	cfg = cfg.withDefaults()
	switch {
	case repo == nil || sender == nil || log == nil:
		return nil, errors.New("delivery: nil dependency")
	case cfg.WorkerID == "" || len(cfg.WorkerID) > 128:
		return nil, errors.New("delivery: worker id must be 1-128 characters")
	case cfg.PoolSize < 1 || cfg.ClaimBatch < 1:
		return nil, errors.New("delivery: pool size and claim batch must be positive")
	case cfg.Lease < time.Second:
		return nil, errors.New("delivery: lease must be at least 1s")
	case cfg.RatePerSecond < 1 || cfg.RateWindow < time.Second:
		return nil, errors.New("delivery: rate limit needs N ≥ 1 and a window of at least 1s")
	case cfg.Gate.MaxPrecheck <= 0 || cfg.Gate.MaxPrecheck > cfg.Gate.MaxSlotToRequest ||
		cfg.Gate.MaxSlotToRequest >= cfg.RateWindow-time.Second:
		return nil, errors.New("delivery: need 0 < MaxPrecheck ≤ MaxSlotToRequest < rate safety margin")
	case cfg.Gate.MaxAborts < 1:
		return nil, errors.New("delivery: MaxAborts must be positive")
	}
	for _, ch := range cfg.Channels {
		if ch != provider.ChannelSMS && ch != provider.ChannelEmail {
			return nil, fmt.Errorf("delivery: unknown channel %q", ch)
		}
	}
	if err := cfg.Policy.Validate(); err != nil {
		return nil, err
	}
	limiters := map[provider.Channel]*Limiter{}
	for _, ch := range cfg.Channels {
		l, err := NewLimiter(cfg.Clock, cfg.RatePerSecond, cfg.RateWindow)
		if err != nil {
			return nil, err
		}
		limiters[ch] = l // independent per channel: nothing shared
	}
	w := &Worker{repo: repo, sender: sender, cfg: cfg, log: log, contents: map[string]content.Content{},
		completionKick: make(chan struct{}, 1)}
	w.gate = &gatedSender{clk: cfg.Clock, limiters: limiters, sender: sender, cfg: cfg.Gate, admit: w.leaseFresh}
	return w, nil
}

// LeaseFreshness is how long after the start of the latest successful
// lock-session statement this process may still issue a provider request:
// LockCheckInterval + LockCheckTimeout. QuietPeriod = LeaseFreshness + W.
func (c Config) LeaseFreshness() time.Duration {
	c = c.withDefaults()
	return c.LockCheckInterval + c.LockCheckTimeout
}

// leaseFresh is the leadership fence of the final gate (ADR-025). A request
// may be issued at now only if the dispatch lock was confirmed (a statement
// on its session succeeded) at a moment that began at most LeaseFreshness
// before now. It is measured with the monotonic clock, so it holds however
// late the lease watcher runs: a process that stalls — GC, descheduling,
// SIGSTOP — after losing its lock cannot issue a request once the fence
// has lapsed, even before its watcher notices.
func (w *Worker) leaseFresh(now time.Time) bool {
	w.stateMu.Lock()
	ok := w.leaseOK
	w.stateMu.Unlock()
	return !ok.IsZero() && !now.After(ok.Add(w.cfg.LeaseFreshness()))
}

func (w *Worker) setLeaseOK(t time.Time) {
	w.stateMu.Lock()
	w.leaseOK = t
	w.stateMu.Unlock()
}

// Limiter returns the channel's limiter (tests, metrics).
func (w *Worker) Limiter(ch provider.Channel) *Limiter { return w.gate.limiters[ch] }

// Dispatching reports whether this process currently holds the dispatch
// lock and dispatches.
func (w *Worker) Dispatching() bool {
	w.stateMu.Lock()
	defer w.stateMu.Unlock()
	return w.dispatcher
}

func (w *Worker) setDispatching(v bool) {
	w.stateMu.Lock()
	w.dispatcher = v
	w.stateMu.Unlock()
}

// Run works until ctx is cancelled. The reaper, completion detector and
// heartbeat run in every worker process. Dispatching (claiming rows and
// making provider requests) runs only while this process holds the
// dispatch lock (ADR-025): a second process stands by, so the in-process
// limiter bounds the requests of the whole deployment.
//
// Shutdown is graceful: stop claiming, release claimed rows whose precheck
// has not run, let in-progress provider requests finish for up to
// ShutdownGrace (then abort them; their outcome is recorded as unknown),
// close the dispatch lock session. Rows that cannot be recorded (database
// unavailable) stay leased and are recovered by the reaper after the lease
// expires.
func (w *Worker) Run(ctx context.Context) error {
	// hard is cancelled only when the grace period is over: provider
	// requests and the writes recording them outlive the shutdown signal.
	hard, abort := context.WithCancel(context.WithoutCancel(ctx))
	defer abort()

	var deliveries, loops sync.WaitGroup
	loops.Add(3)
	go func() { defer loops.Done(); w.every(ctx, w.cfg.ReaperInterval, nil, w.reap) }()
	go func() {
		defer loops.Done()
		w.every(ctx, w.cfg.CompletionInterval, w.completionKick, w.detectCompletions)
	}()
	go func() { defer loops.Done(); w.every(ctx, w.cfg.HeartbeatInterval, nil, w.heartbeat) }()
	w.log.Info("delivery worker started", "worker_id", w.cfg.WorkerID, "pool_size", w.cfg.PoolSize,
		"claim_batch", w.cfg.ClaimBatch, "lease", w.cfg.Lease.String(), "rate_per_channel", w.cfg.RatePerSecond,
		"rate_window", w.cfg.RateWindow.String(), "quiet_period", w.cfg.QuietPeriod().String())

	w.dispatchWhileLeader(ctx, hard, &deliveries)

	w.log.Info("delivery worker stopping: no new claims", "worker_id", w.cfg.WorkerID)
	loops.Wait()

	done := make(chan struct{})
	go func() { deliveries.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(w.cfg.ShutdownGrace):
		w.log.Warn("shutdown grace elapsed; aborting in-progress provider requests", "worker_id", w.cfg.WorkerID)
		abort()
		<-done
	}
	// Final completion pass for campaigns finished by the last deliveries.
	w.detectCompletions(hard)
	w.log.Info("delivery worker stopped", "worker_id", w.cfg.WorkerID)
	return nil
}

// dispatchWhileLeader takes the dispatch lock (retrying while another
// process holds it), dispatches while it holds it, and returns when ctx
// ends. When the lock session is lost, dispatching stops (no new slots),
// the in-progress deliveries finish, and the lock is sought again.
func (w *Worker) dispatchWhileLeader(ctx, hard context.Context, deliveries *sync.WaitGroup) {
	standbyLogged := false
	for ctx.Err() == nil {
		lctx, cancel := w.dbCtx(ctx, w.cfg.DBTimeout)
		attemptStart := w.cfg.Clock.Now() // before the statement: the lock is held no earlier than this
		lease, err := w.repo.TryDispatchLock(lctx)
		cancel()
		if err != nil || lease == nil {
			if err != nil && ctx.Err() == nil {
				w.log.Warn("dispatch lock unavailable", "worker_id", w.cfg.WorkerID, "err", err)
			} else if lease == nil && err == nil && !standbyLogged {
				w.log.Info("another worker process holds the dispatch lock; standing by", "worker_id", w.cfg.WorkerID)
				standbyLogged = true
			}
			w.sleep(ctx, w.cfg.DispatchLockRetry)
			continue
		}
		standbyLogged = false
		w.setLeaseOK(attemptStart)
		w.lead(ctx, hard, lease, deliveries)
		w.setLeaseOK(time.Time{}) // no request after this without a new lease
		cctx, ccancel := context.WithTimeout(context.WithoutCancel(ctx), w.cfg.DBTimeout)
		_ = lease.Close(cctx) // ends the session: the lock is released
		ccancel()
	}
}

// lead dispatches while lease is held.
func (w *Worker) lead(ctx, hard context.Context, lease *DispatchLease, deliveries *sync.WaitGroup) {
	dctx, stop := context.WithCancel(ctx)
	var monitor sync.WaitGroup
	monitor.Add(1)
	go func() {
		defer monitor.Done()
		w.watchLease(dctx, lease, stop)
	}()
	defer func() {
		stop() // ends the watcher (dctx is normally done already)
		monitor.Wait()
	}()

	q := w.cfg.QuietPeriod()
	w.log.Info("dispatch lock acquired; waiting the quiet period before the first request",
		"worker_id", w.cfg.WorkerID, "quiet_period", q.String())
	if !w.sleep(dctx, q) {
		return
	}
	if err := w.loadPauses(dctx); err != nil {
		// Fail closed: without the durable pauses this process could resume
		// a channel the provider asked to pause. Give the lock up and retry.
		w.log.Error("could not read the durable channel pauses; not dispatching", "worker_id", w.cfg.WorkerID, "err", err)
		w.sleep(ctx, w.cfg.DispatchLockRetry)
		return
	}
	w.setDispatching(true)
	defer w.setDispatching(false)

	var dispatchers sync.WaitGroup
	var mine sync.WaitGroup // this lead's deliveries (also counted in deliveries)
	for _, ch := range w.cfg.Channels {
		dispatchers.Add(1)
		go func() {
			defer dispatchers.Done()
			w.dispatch(dctx, hard, ch, deliveries, &mine)
		}()
	}
	dispatchers.Wait()
	if ctx.Err() == nil {
		// The lock was lost while running: let the requests already on the
		// wire finish before the session (and the lock) is given up and
		// sought again; no new slot is granted meanwhile (dctx is done).
		w.log.Warn("dispatch lock lost; dispatching stopped", "worker_id", w.cfg.WorkerID)
		mine.Wait()
	}
}

// watchLease checks the lock session every LockCheckInterval; a failed or
// slow check stops dispatching (stop). Safety does not depend on how
// promptly this runs: the final gate's leadership fence (leaseFresh) stops
// requests LeaseFreshness after the start of the last successful check,
// which QuietPeriod covers. After each successful check the durable channel
// pauses are re-read (a 429 recorded by another process, e.g. a former
// holder's late response).
func (w *Worker) watchLease(ctx context.Context, lease *DispatchLease, stop context.CancelFunc) {
	for w.sleep(ctx, w.cfg.LockCheckInterval) {
		start := w.cfg.Clock.Now()
		cctx, cancel := context.WithTimeout(ctx, w.cfg.LockCheckTimeout)
		err := lease.Check(cctx)
		cancel()
		if err == nil {
			w.setLeaseOK(start)
			w.refreshPauses(ctx)
		}
		if err != nil {
			if ctx.Err() == nil {
				w.log.Error("dispatch lock session check failed; stopping dispatch", "worker_id", w.cfg.WorkerID, "err", err)
			}
			stop()
			return
		}
	}
}

// loadPauses applies the durable channel pauses (a 429 seen before a
// restart, by the previous lock holder, or by a former holder's late
// response) to the limiters. Pauses only ever extend.
func (w *Worker) loadPauses(ctx context.Context) error {
	cctx, cancel := w.dbCtx(ctx, w.cfg.DBTimeout)
	defer cancel()
	pauses, err := w.repo.ActivePauses(cctx)
	if err != nil {
		return err
	}
	now := w.cfg.Clock.Now()
	for ch, d := range pauses {
		if l := w.gate.limiters[ch]; l != nil {
			until := now.Add(d)
			if until.After(l.PausedUntil()) {
				l.Pause(until)
				w.log.Info("channel paused by a recorded 429", "channel", ch, "remaining", d.String())
			}
		}
	}
	return nil
}

// refreshPauses is loadPauses during dispatch; a failed read is logged (the
// in-memory pauses stay; claims skip durably paused channels in SQL).
func (w *Worker) refreshPauses(ctx context.Context) {
	if err := w.loadPauses(ctx); err != nil && ctx.Err() == nil {
		w.log.Warn("could not refresh the durable channel pauses", "err", err)
	}
}

// sleep waits d or until ctx ends; it reports whether d elapsed.
func (w *Worker) sleep(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}

// every runs fn every period (and when kick fires) until ctx is cancelled.
func (w *Worker) every(ctx context.Context, period time.Duration, kick <-chan struct{}, fn func(context.Context)) {
	t := time.NewTicker(period)
	defer t.Stop()
	for {
		fn(ctx)
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		case <-kick:
		}
	}
}

// dbCtx bounds one database operation; it is not cancelled by shutdown
// (base is the hard context), so that a claim, precheck or completion that
// started is finished and its result is known.
func (w *Worker) dbCtx(base context.Context, d time.Duration) (context.Context, context.CancelFunc) {
	return context.WithTimeout(base, d)
}

// dispatch claims rows of one channel while the pool has free slots.
func (w *Worker) dispatch(ctx, hard context.Context, ch provider.Channel, deliveries, mine *sync.WaitGroup) {
	slots := make(chan struct{}, w.cfg.PoolSize)
	freed := make(chan struct{}, 1)
	wait := func(d time.Duration) {
		t := time.NewTimer(d)
		defer t.Stop()
		select {
		case <-ctx.Done():
		case <-t.C:
		case <-freed:
		}
	}
	for ctx.Err() == nil {
		free := cap(slots) - len(slots)
		if free == 0 {
			select {
			case <-ctx.Done():
			case <-freed:
			}
			continue
		}
		n := min(free, w.cfg.ClaimBatch)
		cctx, cancel := w.dbCtx(hard, w.cfg.DBTimeout)
		jobs, err := w.repo.Claim(cctx, ch, n, w.cfg.WorkerID, w.cfg.Lease)
		cancel()
		if err != nil {
			w.log.Warn("claim failed", "channel", ch, "err", err)
			wait(w.cfg.PollInterval)
			continue
		}
		for _, j := range jobs {
			slots <- struct{}{} // never blocks: n ≤ free and only this loop fills slots
			deliveries.Add(1)
			mine.Add(1)
			go func() {
				defer func() {
					<-slots
					mine.Done()
					deliveries.Done()
					select {
					case freed <- struct{}{}:
					default:
					}
				}()
				w.process(ctx, hard, j)
			}()
		}
		if len(jobs) < n {
			wait(w.cfg.PollInterval)
		}
	}
}

func (w *Worker) jobLog(j Job) []any {
	return []any{"campaign_id", j.CampaignID, "approval_id", j.ApprovalID, "delivery_id", j.ID, "channel", j.Channel}
}

// process runs one claimed row: render (no I/O but the content read),
// then the gated attempt. Nothing in here logs recipient values or message
// text. ctx is the dispatch context (shutdown or loss of the dispatch
// lock); hard outlives it for writes and requests already started.
func (w *Worker) process(ctx, hard context.Context, j Job) {
	if ctx.Err() != nil {
		w.release(hard, j, 0, "dispatch stopped before precheck")
		return
	}
	c, err := w.approvedContent(hard, j)
	if errors.Is(err, errContentInvalid) {
		w.log.Error("approved content does not validate; failing the delivery without a request", append(w.jobLog(j), "err", err)...)
		w.finish(hard, j, StatusFailed, CodeContentInvalid)
		return
	}
	if err != nil {
		w.log.Warn("could not read the approved content; releasing", append(w.jobLog(j), "err", err)...)
		w.release(hard, j, w.cfg.Policy.BackoffBase, "content unavailable")
		return
	}
	msg, code, err := BuildMessage(c, j.Channel, j.IdempotencyKey, j.Vars)
	if err != nil {
		w.log.Error("message could not be built; releasing", append(w.jobLog(j), "err", err)...)
		w.release(hard, j, w.cfg.Policy.BackoffBase, "build failed")
		return
	}
	if code != "" {
		// Frozen values that cannot be rendered fail permanently (§8.7,
		// §9.1): never truncated, never replaced, never sent.
		w.finish(hard, j, StatusFailed, code)
		return
	}
	w.attempt(ctx, hard, j, msg)
}

// attempt is gatedSender.Attempt (§10.2) — the only code path that calls
// the provider — followed by the completion:
//
//  1. acquire the channel's rate-limit slot (waits; cancelled by ctx);
//  2. precheck — one statement within DISPATCH_MAX_PRECHECK of the slot:
//     lease, campaign/approval state, current consent, destination
//     (current contact on the first attempt, persisted before the
//     request);
//  3. fill in the destination (CPU only); final gate immediately before
//     http.Client.Do: no request later than DISPATCH_MAX_SLOT_TO_REQUEST
//     after the slot (else the slot is wasted, the precheck undone, and a
//     new slot acquired; after 3 aborts the row is released without an
//     attempt);
//  4. the provider request (bounded by the request timeout);
//  5. a 429 pauses this channel's limiter at once (in memory), then
//  6. complete — one transaction: guarded status update + attempt row (+
//     the durable channel pause for a 429).
func (w *Worker) attempt(ctx, hard context.Context, j Job, msg provider.Message) {
	ops := gateOps{
		precheck: func(pctx context.Context) (Precheck, error) {
			return w.repo.Precheck(pctx, j, w.cfg.WorkerID, w.cfg.Lease)
		},
		undo: func() error {
			cctx, cancel := w.dbCtx(hard, w.cfg.DBTimeout)
			defer cancel()
			_, err := w.repo.ClearDispatch(cctx, j, w.cfg.WorkerID)
			return err
		},
	}
	g := w.gate.Attempt(ctx, hard, msg, ops)
	if g.aborts > 0 {
		w.log.Warn("rate-limit slots wasted: precheck over budget or slot deadline passed", append(w.jobLog(j), "aborts", g.aborts)...)
	}
	switch g.kind {
	case attemptMiss:
		w.precheckMiss(hard, j)
		return
	case attemptAborted:
		w.release(hard, j, AbortReleaseDelay, "slot deadline missed repeatedly")
		return
	case attemptFailed:
		// No request was made. If a precheck committed nonetheless, the
		// persisted destination stays (allowed); dispatch_started_at is
		// cleared by the release.
		if ctx.Err() != nil && errors.Is(g.err, ctx.Err()) {
			w.release(hard, j, 0, "dispatch stopped while waiting for a rate-limit slot")
			return
		}
		w.log.Warn("precheck failed; releasing without an attempt", append(w.jobLog(j), "err", g.err)...)
		w.release(hard, j, w.cfg.Policy.BackoffBase, "precheck failed")
		return
	}
	res, pre := g.res, g.pre
	if res.Outcome == provider.OutcomeRateLimited {
		// Pause this channel only, immediately (requests already granted a
		// slot finish); the durable pause is written with the completion.
		pause := w.cfg.Policy.RateLimitWait(res.RetryAfter)
		w.gate.limiters[j.Channel].Pause(w.cfg.Clock.Now().Add(pause))
		w.log.Warn("provider rate limit: channel paused", append(w.jobLog(j), "pause", pause.String())...)
	}

	cctx, cancel := w.dbCtx(context.WithoutCancel(hard), w.cfg.DBTimeout)
	defer cancel()
	c, ok, err := w.repo.Complete(cctx, j, w.cfg.WorkerID, pre, res, w.cfg.Policy, w.cfg.Rand())
	attrs := append(w.jobLog(j), "outcome", res.Outcome, "http_status", res.HTTPStatus)
	switch {
	case err != nil:
		// The row stays in_flight under our lease; the reaper records an
		// unknown attempt after the lease expires and the retry reconciles.
		w.log.Error("attempt outcome could not be recorded; the reaper will recover the row", append(attrs, "err", err)...)
		if res.Outcome == provider.OutcomeRateLimited {
			// The durable pause was part of the failed transaction: write it
			// on its own (the in-memory pause already holds in this process).
			pause := w.cfg.Policy.RateLimitWait(res.RetryAfter)
			pctx, pcancel := w.dbCtx(context.WithoutCancel(hard), w.cfg.DBTimeout)
			if perr := w.repo.PersistPause(pctx, j.Channel, pause); perr != nil {
				w.log.Error("channel pause could not be persisted; it holds in this process only", append(w.jobLog(j), "err", perr)...)
			}
			pcancel()
		}
	case !ok:
		w.log.Warn("lease lost before the outcome was recorded; the reaper recorded the attempt", attrs...)
	default:
		attrs = append(attrs, "attempt_no", c.AttemptNo, "status", c.Status, "outcome_uncertain", c.Uncertain, "error_code", res.ErrorCode)
		if c.Status == StatusRetryScheduled {
			attrs = append(attrs, "retry_in", c.Delay.String())
		}
		w.log.Info("delivery attempt recorded", attrs...)
		if c.Status.Terminal() {
			w.kickCompletion()
		}
	}
}

func (w *Worker) precheckMiss(hard context.Context, j Job) {
	cctx, cancel := w.dbCtx(hard, w.cfg.DBTimeout)
	reason, err := w.repo.ClassifyMiss(cctx, j, w.cfg.WorkerID)
	cancel()
	if err != nil {
		w.log.Warn("precheck miss could not be classified; releasing", append(w.jobLog(j), "err", err)...)
		w.release(hard, j, w.cfg.Policy.BackoffBase, "classify failed")
		return
	}
	switch reason {
	case MissLeaseLost:
		w.log.Warn("lease lost before the precheck; another owner has the row", w.jobLog(j)...)
	case MissStaleDispatch:
		// One of this claim's own prechecks timed out on our side but
		// committed after its undo: no request was made for it (the gate
		// aborted), so the row is released without an attempt.
		w.release(hard, j, 0, "late commit of an aborted precheck")
	case MissNotSending:
		w.finish(hard, j, StatusCancelled, "campaign_not_sending")
	case MissNoConsent:
		w.finish(hard, j, StatusSkippedConsentRevoked, "no_consent")
	case MissNoContact:
		w.finish(hard, j, StatusSkippedNoContact, "no_contact")
	default:
		w.release(hard, j, 0, "precheck raced with a change")
	}
}

func (w *Worker) finish(hard context.Context, j Job, s Status, code string) {
	cctx, cancel := w.dbCtx(hard, w.cfg.DBTimeout)
	defer cancel()
	final, ok, err := w.repo.FinishWithoutRequest(cctx, j, w.cfg.WorkerID, s, code)
	switch {
	case err != nil:
		w.log.Error("terminal status could not be recorded; the reaper will recover the row", append(w.jobLog(j), "err", err)...)
	case !ok:
		w.log.Warn("lease lost before the terminal status was recorded", w.jobLog(j)...)
	default:
		w.log.Info("delivery finished without a provider request", append(w.jobLog(j), "status", final, "reason", code)...)
		w.kickCompletion()
	}
}

func (w *Worker) release(hard context.Context, j Job, delay time.Duration, why string) {
	cctx, cancel := w.dbCtx(hard, w.cfg.DBTimeout)
	defer cancel()
	st, ok, err := w.repo.Release(cctx, j, w.cfg.WorkerID, delay)
	switch {
	case err != nil:
		w.log.Warn("release failed; the reaper will recover the row", append(w.jobLog(j), "reason", why, "err", err)...)
	case !ok:
		w.log.Warn("lease lost before the row could be released", append(w.jobLog(j), "reason", why)...)
	case st == StatusCancelled:
		w.log.Info("delivery cancelled without a request: the campaign is no longer sending", append(w.jobLog(j), "reason", why)...)
		w.kickCompletion()
	default:
		w.log.Debug("delivery released without an attempt", append(w.jobLog(j), "reason", why)...)
	}
}

func (w *Worker) heartbeat(ctx context.Context) {
	cctx, cancel := w.dbCtx(ctx, w.cfg.DBTimeout)
	defer cancel()
	if err := w.repo.Heartbeat(cctx, w.cfg.WorkerID); err != nil && ctx.Err() == nil {
		w.log.Warn("heartbeat failed", "worker_id", w.cfg.WorkerID, "err", err)
	}
}

func (w *Worker) approvedContent(hard context.Context, j Job) (content.Content, error) {
	w.contentMu.Lock()
	c, ok := w.contents[j.ApprovalID]
	w.contentMu.Unlock()
	if ok {
		return c, nil
	}
	cctx, cancel := w.dbCtx(hard, w.cfg.DBTimeout)
	defer cancel()
	c, err := w.repo.ApprovedContent(cctx, j.ApprovalID)
	if err != nil {
		return content.Content{}, err
	}
	w.contentMu.Lock()
	if len(w.contents) >= contentCacheSize {
		clear(w.contents)
	}
	w.contents[j.ApprovalID] = c
	w.contentMu.Unlock()
	return c, nil
}

func (w *Worker) kickCompletion() {
	select {
	case w.completionKick <- struct{}{}:
	default:
	}
}

func (w *Worker) reap(ctx context.Context) {
	for ctx.Err() == nil {
		cctx, cancel := w.dbCtx(ctx, w.cfg.DBTimeout)
		st, err := w.repo.Reap(cctx, reapBatch, w.cfg.Policy, w.cfg.Rand)
		cancel()
		if err != nil {
			if ctx.Err() == nil {
				w.log.Warn("reaper pass failed", "err", err)
			}
			return
		}
		if st.Released+st.Unknown > 0 {
			w.log.Warn("expired leases recovered", "released_without_attempt", st.Released, "unknown_attempts", st.Unknown)
			w.kickCompletion()
		}
		if st.Released+st.Unknown < reapBatch {
			return
		}
	}
}

func (w *Worker) detectCompletions(ctx context.Context) {
	cctx, cancel := w.dbCtx(ctx, w.cfg.DBTimeout)
	defer cancel()
	ids, err := w.repo.SendingCampaigns(cctx, completionScan)
	if err != nil {
		if ctx.Err() == nil {
			w.log.Warn("completion scan failed", "err", err)
		}
		return
	}
	for _, id := range ids {
		res, err := w.repo.CompleteCampaign(cctx, id)
		if err != nil {
			w.log.Warn("completion check failed", "campaign_id", id, "err", err)
			continue
		}
		if !res.Completed {
			continue
		}
		attrs := []any{"campaign_id", id, "approval_id", res.ApprovalID, "status", res.Status, "campaign_version", res.Version}
		for _, c := range res.Counts {
			attrs = append(attrs, string(c.Channel)+"_"+string(c.Status), c.Rows)
			if c.Uncertain > 0 {
				attrs = append(attrs, string(c.Channel)+"_"+string(c.Status)+"_uncertain", c.Uncertain)
			}
		}
		w.log.Info("campaign completed", attrs...)
	}
}
