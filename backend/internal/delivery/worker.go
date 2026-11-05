package delivery

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/fyusufayan/insider-campaign-composer-case/backend/internal/content"
	"github.com/fyusufayan/insider-campaign-composer-case/backend/internal/provider"
)

// Sender is the provider client (the only interface of this package: tests
// substitute it). Send must never return before the request is finished or
// abandoned, and must classify every failure as an outcome.
type Sender interface {
	Send(ctx context.Context, m provider.Message) provider.Result
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
	// ReaperInterval, CompletionInterval: periods of the reaper and of the
	// completion detector.
	ReaperInterval     time.Duration
	CompletionInterval time.Duration
	// ShutdownGrace bounds how long in-progress requests may finish after
	// shutdown starts; then they are aborted (recorded as unknown).
	ShutdownGrace time.Duration
	// PrecheckTimeout bounds the precheck statement (CP17 tightens it to
	// DISPATCH_MAX_PRECHECK together with the rate limiter).
	PrecheckTimeout time.Duration
	// DBTimeout bounds every other worker statement or transaction.
	DBTimeout time.Duration
	Policy    Policy
}

// Defaults (§10.1, §10.7, §16).
const (
	DefaultPoolSize           = 20
	DefaultClaimBatch         = 50
	DefaultLease              = 60 * time.Second
	DefaultPollInterval       = 500 * time.Millisecond
	DefaultReaperInterval     = 10 * time.Second
	DefaultCompletionInterval = 5 * time.Second
	DefaultShutdownGrace      = 10 * time.Second
	DefaultPrecheckTimeout    = 2 * time.Second
	DefaultDBTimeout          = 10 * time.Second
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
	def(&c.Lease, DefaultLease)
	def(&c.PollInterval, DefaultPollInterval)
	def(&c.ReaperInterval, DefaultReaperInterval)
	def(&c.CompletionInterval, DefaultCompletionInterval)
	def(&c.ShutdownGrace, DefaultShutdownGrace)
	def(&c.PrecheckTimeout, DefaultPrecheckTimeout)
	def(&c.DBTimeout, DefaultDBTimeout)
	if c.Policy == (Policy{}) {
		c.Policy = DefaultPolicy
	}
	return c
}

// Worker processes released delivery rows.
type Worker struct {
	repo   *Repository
	sender Sender
	cfg    Config
	log    *slog.Logger

	contentMu sync.Mutex
	contents  map[string]content.Content // approval id → approved content (immutable)

	completionKick chan struct{}
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
	}
	for _, ch := range cfg.Channels {
		if ch != provider.ChannelSMS && ch != provider.ChannelEmail {
			return nil, fmt.Errorf("delivery: unknown channel %q", ch)
		}
	}
	if err := cfg.Policy.Validate(); err != nil {
		return nil, err
	}
	return &Worker{repo: repo, sender: sender, cfg: cfg, log: log, contents: map[string]content.Content{},
		completionKick: make(chan struct{}, 1)}, nil
}

// Run dispatches until ctx is cancelled, then shuts down gracefully: it
// stops claiming, releases claimed rows whose precheck has not run, lets
// in-progress provider requests finish for up to ShutdownGrace (then
// aborts them; their outcome is recorded as unknown), and returns. Rows
// that cannot be recorded (database unavailable) stay leased and are
// recovered by the reaper of the next worker after the lease expires.
func (w *Worker) Run(ctx context.Context) error {
	// hard is cancelled only when the grace period is over: provider
	// requests and the writes recording them outlive the shutdown signal.
	hard, abort := context.WithCancel(context.WithoutCancel(ctx))
	defer abort()

	var deliveries, loops sync.WaitGroup
	for _, ch := range w.cfg.Channels {
		loops.Add(1)
		go func() {
			defer loops.Done()
			w.dispatch(ctx, hard, ch, &deliveries)
		}()
	}
	loops.Add(2)
	go func() { defer loops.Done(); w.every(ctx, w.cfg.ReaperInterval, nil, w.reap) }()
	go func() {
		defer loops.Done()
		w.every(ctx, w.cfg.CompletionInterval, w.completionKick, w.detectCompletions)
	}()
	w.log.Info("delivery worker started", "worker_id", w.cfg.WorkerID, "pool_size", w.cfg.PoolSize,
		"claim_batch", w.cfg.ClaimBatch, "lease", w.cfg.Lease.String())

	<-ctx.Done()
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
func (w *Worker) dispatch(ctx, hard context.Context, ch provider.Channel, deliveries *sync.WaitGroup) {
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
			go func() {
				defer func() {
					<-slots
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
// text.
func (w *Worker) process(ctx, hard context.Context, j Job) {
	if ctx.Err() != nil {
		w.release(hard, j, 0, "shutdown before precheck")
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
		w.release(hard, j, w.cfg.Policy.RetryDelay, "content unavailable")
		return
	}
	msg, code, err := BuildMessage(c, j.Channel, j.IdempotencyKey, j.Vars)
	if err != nil {
		w.log.Error("message could not be built; releasing", append(w.jobLog(j), "err", err)...)
		w.release(hard, j, w.cfg.Policy.RetryDelay, "build failed")
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

// attempt is gatedSender.Attempt (§10.2), the only code path that calls the
// provider:
//
//  1. (CP17: acquire the channel's rate-limit slot here; slot deadline.)
//  2. precheck — one statement: lease, campaign/approval state, current
//     consent, destination (current contact on the first attempt,
//     persisted before the request);
//  3. fill in the destination (CPU only) — no I/O between the precheck and
//     the request;
//  4. the provider request (bounded by the request timeout);
//  5. complete — one transaction: guarded status update + attempt row.
func (w *Worker) attempt(ctx, hard context.Context, j Job, msg provider.Message) {
	if ctx.Err() != nil {
		w.release(hard, j, 0, "shutdown before precheck")
		return
	}
	pctx, cancel := w.dbCtx(hard, w.cfg.PrecheckTimeout)
	pre, err := w.repo.Precheck(pctx, j, w.cfg.WorkerID, w.cfg.Lease)
	cancel()
	if errors.Is(err, ErrPrecheckMiss) {
		w.precheckMiss(hard, j)
		return
	}
	if err != nil {
		// No request was made. If the precheck committed nonetheless, the
		// persisted destination stays (allowed); dispatch_started_at is
		// cleared by the release.
		w.log.Warn("precheck failed; releasing without an attempt", append(w.jobLog(j), "err", err)...)
		w.release(hard, j, w.cfg.Policy.RetryDelay, "precheck failed")
		return
	}
	msg.To = pre.Destination
	res := w.sender.Send(hard, msg) // ← the only provider request

	cctx, cancel := w.dbCtx(context.WithoutCancel(hard), w.cfg.DBTimeout)
	defer cancel()
	c, ok, err := w.repo.Complete(cctx, j, w.cfg.WorkerID, pre, res, w.cfg.Policy)
	attrs := append(w.jobLog(j), "outcome", res.Outcome, "http_status", res.HTTPStatus)
	switch {
	case err != nil:
		// The row stays in_flight under our lease; the reaper records an
		// unknown attempt after the lease expires and the retry reconciles.
		w.log.Error("attempt outcome could not be recorded; the reaper will recover the row", append(attrs, "err", err)...)
	case !ok:
		w.log.Warn("lease lost before the outcome was recorded; the reaper recorded the attempt", attrs...)
	default:
		w.log.Info("delivery attempt recorded", append(attrs, "attempt_no", c.AttemptNo, "status", c.Status,
			"outcome_uncertain", c.Uncertain, "error_code", res.ErrorCode)...)
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
		w.release(hard, j, w.cfg.Policy.RetryDelay, "classify failed")
		return
	}
	switch reason {
	case MissLeaseLost:
		w.log.Warn("lease lost before the precheck; another owner has the row", w.jobLog(j)...)
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
	if _, err := w.repo.Release(cctx, j, w.cfg.WorkerID, delay); err != nil {
		w.log.Warn("release failed; the reaper will recover the row", append(w.jobLog(j), "reason", why, "err", err)...)
		return
	}
	w.log.Debug("delivery released without an attempt", append(w.jobLog(j), "reason", why)...)
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
		st, err := w.repo.Reap(cctx, reapBatch, w.cfg.Policy)
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
