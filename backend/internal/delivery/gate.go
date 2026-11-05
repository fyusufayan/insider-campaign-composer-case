package delivery

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/fyusufayan/insider-campaign-composer-case/backend/internal/platform/clock"
	"github.com/fyusufayan/insider-campaign-composer-case/backend/internal/provider"
)

// GateConfig bounds the slot-to-request interval (§10.2, ADR-011).
type GateConfig struct {
	// MaxPrecheck is DISPATCH_MAX_PRECHECK: the precheck must finish within
	// this time after the slot was granted, else the slot is wasted.
	MaxPrecheck time.Duration
	// MaxSlotToRequest is DISPATCH_MAX_SLOT_TO_REQUEST (G): no request is
	// issued later than this after its slot.
	MaxSlotToRequest time.Duration
	// MaxAborts consecutive wasted slots release the row without an attempt.
	MaxAborts int
}

// Gate defaults (§10.2, §16).
const (
	DefaultMaxPrecheck      = 15 * time.Millisecond
	DefaultMaxSlotToRequest = 20 * time.Millisecond
	DefaultMaxAborts        = 3
	// AbortReleaseDelay reschedules a row released after MaxAborts aborts.
	AbortReleaseDelay = time.Second
)

// gatedSender is the only code path to the provider (§10.2):
//
//  1. everything that needs no I/O was built by the caller (rendered body);
//  2. t0 := limiter[channel].Acquire — reserves a slot; slotDeadline :=
//     t0 + MaxSlotToRequest (monotonic clock);
//  3. precheck — one SQL statement with a context deadline of
//     min(MaxPrecheck, MaxSlotToRequest) after t0;
//  4. final gate, inside provider.Client.SendGated after the request is
//     built, immediately before http.Client.Do: clock.Now() ≤ slotDeadline,
//     the dispatch context is still live and the leadership fence admits
//     (the dispatch lock was confirmed recently enough, ADR-025), else
//     nothing is sent;
//  5. the request.
//
// A slot whose precheck exceeds the budget, or whose deadline passed at the
// final gate, is wasted (never refunded); the precheck is undone and a new
// slot is acquired. After MaxAborts consecutive aborts the attempt gives up
// without a request (the caller releases the row, no attempt counted).
type gatedSender struct {
	clk      clock.Clock
	limiters map[provider.Channel]*Limiter
	sender   Sender
	cfg      GateConfig
	// afterPrecheck runs after a successful, in-budget precheck and before
	// the final gate (test hook: simulates a process delay there).
	afterPrecheck func()
	// admit is the leadership fence, evaluated in the final gate with the
	// gate's own clock reading: false means this process can no longer prove
	// it holds the dispatch lock (ADR-025). nil admits (unit tests of the
	// gate alone).
	admit func(now time.Time) bool
}

// gateOps are the database operations of one attempt (closures over the
// repository, the job and the worker id).
type gateOps struct {
	precheck func(ctx context.Context) (Precheck, error)
	// undo clears dispatch_started_at after an abort whose precheck may have
	// committed, so that the next slot can precheck again.
	undo func() error
}

type attemptKind int

const (
	// attemptSent: the request was handed to the HTTP client; res is its
	// classified result and pre the precheck it belongs to.
	attemptSent attemptKind = iota
	// attemptMiss: the precheck matched no row (ErrPrecheckMiss).
	attemptMiss
	// attemptAborted: MaxAborts consecutive slot-deadline aborts; no request.
	attemptAborted
	// attemptFailed: no request; err says why (dispatch stopped while
	// waiting for a slot, precheck/undo database error).
	attemptFailed
)

type gateResult struct {
	kind   attemptKind
	pre    Precheck
	res    provider.Result
	aborts int
	err    error
}

// errOverBudget marks a precheck that did not finish within its budget.
var errOverBudget = errors.New("delivery: precheck exceeded its budget")

// Attempt runs steps 2–5 for msg (whose To is filled from the precheck).
// waitCtx bounds the wait for a slot (cancelled at shutdown or when the
// dispatch lock is lost); hard bounds the database statements and the
// request, which must outlive the shutdown signal once started.
func (g *gatedSender) Attempt(waitCtx, hard context.Context, msg provider.Message, ops gateOps) gateResult {
	lim := g.limiters[msg.Channel]
	if lim == nil {
		return gateResult{kind: attemptFailed, err: errors.New("delivery: no limiter for channel " + string(msg.Channel))}
	}
	budget := min(g.cfg.MaxPrecheck, g.cfg.MaxSlotToRequest)
	aborts := 0
	for aborts < g.cfg.MaxAborts {
		t0, err := lim.Acquire(waitCtx)
		if err != nil {
			return gateResult{kind: attemptFailed, aborts: aborts, err: err}
		}
		slotDeadline := t0.Add(g.cfg.MaxSlotToRequest)

		pctx, cancel := context.WithTimeout(hard, budget)
		pre, err := ops.precheck(pctx)
		timedOut := pctx.Err() != nil && hard.Err() == nil
		cancel()
		switch {
		case errors.Is(err, ErrPrecheckMiss):
			return gateResult{kind: attemptMiss, aborts: aborts}
		case err != nil && (timedOut || isStatementCanceled(err)):
			err = errOverBudget
		case err != nil:
			return gateResult{kind: attemptFailed, aborts: aborts, err: err}
		case g.clk.Now().Sub(t0) > g.cfg.MaxPrecheck:
			err = errOverBudget
		}
		if err == nil {
			if g.afterPrecheck != nil {
				g.afterPrecheck()
			}
			msg.To = pre.Destination // CPU only; nothing but the gate and Do follow
			// The final gate: the slot deadline, the dispatch context (shutdown
			// or lost lock, observed without I/O) and the leadership fence.
			gate := func() bool {
				now := g.clk.Now()
				return !now.After(slotDeadline) && waitCtx.Err() == nil && (g.admit == nil || g.admit(now))
			}
			res, sent := g.sender.SendGated(hard, msg, gate)
			if sent {
				return gateResult{kind: attemptSent, pre: pre, res: res, aborts: aborts}
			}
		}
		// Abort: the slot is wasted; undo the precheck (no request was made).
		aborts++
		if uerr := ops.undo(); uerr != nil {
			return gateResult{kind: attemptFailed, aborts: aborts, err: uerr}
		}
	}
	return gateResult{kind: attemptAborted, aborts: aborts}
}

// isStatementCanceled reports a server-side cancellation (the precheck's
// context deadline makes pgx cancel the statement: 57014).
func isStatementCanceled(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "57014"
}
