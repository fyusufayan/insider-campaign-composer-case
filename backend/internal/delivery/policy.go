// Package delivery is the PostgreSQL-backed delivery worker (ARCHITECTURE
// §10, ADR-002, ADR-008 – ADR-010): it claims released delivery rows with
// FOR UPDATE SKIP LOCKED and a lease, re-checks consent and contact in a
// fencing precheck immediately before the provider request, renders the
// immutable approved content with the frozen personalization values, sends
// through the provider client, records every attempt and the resulting
// delivery status in one transaction, recovers expired leases (reaper) and
// completes campaigns whose rows are all terminal.
//
// It never imports ai or llm, never inserts or deletes delivery rows (only
// approval.Prepare inserts them), never re-evaluates the audience and never
// holds a transaction or row lock across the provider request.
//
// Every provider request goes through gatedSender.Attempt: per-channel
// sliding-window limiter slot → precheck within DISPATCH_MAX_PRECHECK →
// final slot-deadline gate immediately before http.Client.Do (§10.2). A
// 429 pauses only its channel, in memory and durably (channel_pauses).
// Only the worker process holding the PostgreSQL dispatch lock dispatches,
// so the in-process limiter bounds the whole deployment (ADR-025).
// Retries use exponential backoff with full jitter (ADR-012).
package delivery

import (
	"errors"
	"fmt"
	"time"

	"github.com/fyusufayan/insider-campaign-composer-case/backend/internal/provider"
)

// Status is campaign_deliveries.status (§5.4).
type Status string

// Statuses.
const (
	StatusHeld                  Status = "held"
	StatusPending               Status = "pending"
	StatusInFlight              Status = "in_flight"
	StatusRetryScheduled        Status = "retry_scheduled"
	StatusSent                  Status = "sent"
	StatusFailed                Status = "failed"
	StatusSkippedConsentRevoked Status = "skipped_consent_revoked"
	StatusSkippedNoContact      Status = "skipped_no_contact"
	StatusCancelled             Status = "cancelled"
)

// Terminal reports whether no further attempt follows.
func (s Status) Terminal() bool {
	switch s {
	case StatusSent, StatusFailed, StatusSkippedConsentRevoked, StatusSkippedNoContact, StatusCancelled:
		return true
	}
	return false
}

// Policy is the retry policy (§10.5, ADR-012): classified outcomes with
// separate budgets, exponential backoff with full jitter for transient and
// ambiguous outcomes, Retry-After for 429s.
type Policy struct {
	// MaxTransientAttempts bounds non-429 attempts (incl. timeouts and
	// unknown outcomes): the row fails after this many (MAX_TRANSIENT_ATTEMPTS).
	MaxTransientAttempts int
	// MaxRateLimitedAttempts bounds 429 answers (MAX_RATE_LIMITED_ATTEMPTS).
	MaxRateLimitedAttempts int
	// BackoffBase and BackoffCap define the backoff ceiling after the n-th
	// transient attempt: min(BackoffCap, BackoffBase·2^(n−1))
	// (BACKOFF_BASE 1 s, BACKOFF_CAP 60 s).
	BackoffBase, BackoffCap time.Duration
	// RetryAfterCap caps a 429's Retry-After (RETRY_AFTER_CAP).
	RetryAfterCap time.Duration
}

// DefaultPolicy is the §16 configuration.
var DefaultPolicy = Policy{MaxTransientAttempts: 5, MaxRateLimitedAttempts: 10, BackoffBase: time.Second,
	BackoffCap: 60 * time.Second, RetryAfterCap: provider.DefaultRetryAfterCap}

// Validate checks the policy.
func (p Policy) Validate() error {
	switch {
	case p.MaxTransientAttempts < 1 || p.MaxTransientAttempts > 20:
		return errors.New("delivery: MaxTransientAttempts must be in [1, 20]")
	case p.MaxRateLimitedAttempts < 1 || p.MaxRateLimitedAttempts > 50:
		return errors.New("delivery: MaxRateLimitedAttempts must be in [1, 50]")
	case p.BackoffBase < time.Millisecond || p.BackoffBase > 10*time.Minute:
		return errors.New("delivery: BackoffBase must be in [1ms, 10m]")
	case p.BackoffCap < p.BackoffBase || p.BackoffCap > time.Hour:
		return errors.New("delivery: BackoffCap must be in [BackoffBase, 1h]")
	case p.RetryAfterCap < time.Millisecond || p.RetryAfterCap > 10*time.Minute:
		return errors.New("delivery: RetryAfterCap must be in [1ms, 10m]")
	}
	return nil
}

// Backoff is the ceiling of the delay after the n-th transient attempt
// (n ≥ 1): min(BackoffCap, BackoffBase·2^(n−1)), computed without
// overflow.
func (p Policy) Backoff(n int) time.Duration {
	d := p.BackoffBase
	for i := 1; i < n; i++ {
		if d >= p.BackoffCap/2 {
			return p.BackoffCap
		}
		d *= 2
	}
	return min(d, p.BackoffCap)
}

// RateLimitWait bounds a 429's Retry-After: at least 1 s, at most
// RetryAfterCap.
func (p Policy) RateLimitWait(retryAfter time.Duration) time.Duration {
	return min(max(retryAfter, provider.DefaultRetryAfter), p.RetryAfterCap)
}

// Decision is what to do after one provider attempt.
type Decision struct {
	// Status is sent, retry_scheduled or failed. (A retry of a campaign that
	// is no longer sending becomes cancelled when it is persisted.)
	Status Status
	// Delay before the next attempt (retry_scheduled only).
	Delay time.Duration
	// AttemptsInc / RateLimitedInc increment the row's counters.
	AttemptsInc, RateLimitedInc int
	// Pause is how long the channel must pause (rate_limited only): the
	// Retry-After, bounded like Delay. It is applied even when the row
	// itself fails (its 429 budget is exhausted): the provider asked the
	// whole channel to slow down.
	Pause time.Duration
}

// Decide applies the retry policy to the outcome of an attempt made after
// attempts completed non-429 attempts and rateLimited 429 answers. u is a
// uniform random number in [0, 1) (injected, so Decide stays pure and
// tests are deterministic).
//
//   - accepted / replayed → sent;
//   - rate_limited → retry after Retry-After (≥ 1 s, ≤ RetryAfterCap),
//     failed once MaxRateLimitedAttempts 429s were received; 429s do not
//     use the transient budget;
//   - transient_error / timeout / unknown → retry after a full-jitter delay
//     u·Backoff(n) for the n-th attempt (uniform in [0, ceiling)), failed
//     once MaxTransientAttempts attempts were made. A retry of an
//     ambiguous attempt is safe only because every retry sends the same
//     idempotency key, destination and body (§10.6);
//   - permanent_error → failed, never retried.
func (p Policy) Decide(o provider.Outcome, attempts, rateLimited int, retryAfter time.Duration, u float64) (Decision, error) {
	if u < 0 || u >= 1 || u != u {
		return Decision{}, fmt.Errorf("delivery: jitter sample %v outside [0, 1)", u)
	}
	switch o {
	case provider.OutcomeAccepted, provider.OutcomeReplayed:
		return Decision{Status: StatusSent, AttemptsInc: 1}, nil
	case provider.OutcomeRateLimited:
		wait := p.RateLimitWait(retryAfter)
		d := Decision{RateLimitedInc: 1, Pause: wait}
		if rateLimited+1 >= p.MaxRateLimitedAttempts {
			d.Status = StatusFailed
			return d, nil
		}
		d.Status, d.Delay = StatusRetryScheduled, wait
		return d, nil
	case provider.OutcomeTransientError, provider.OutcomeTimeout, provider.OutcomeUnknown:
		d := Decision{AttemptsInc: 1}
		if attempts+1 >= p.MaxTransientAttempts {
			d.Status = StatusFailed
			return d, nil
		}
		d.Status, d.Delay = StatusRetryScheduled, time.Duration(u*float64(p.Backoff(attempts+1)))
		return d, nil
	case provider.OutcomePermanentError:
		return Decision{Status: StatusFailed, AttemptsInc: 1}, nil
	}
	return Decision{}, fmt.Errorf("delivery: unknown outcome %q", o)
}

// RetryHorizon is an upper bound on the time between the first and the
// last provider request of one delivery while the worker is able to
// dispatch (§10.6): every transient attempt may take the request timeout or,
// if the worker dies after sending, a lease plus a reaper interval; plus the
// backoff ceilings between transient attempts and the 429 waits. Time a
// ready row waits for a rate-limit slot behind a backlog is not included
// (it depends on the backlog; ≈ backlog / 95 per second per channel).
func RetryHorizon(p Policy, requestTimeout, lease, reaperInterval time.Duration) time.Duration {
	perAttempt := max(requestTimeout, lease+reaperInterval)
	h := time.Duration(p.MaxTransientAttempts)*perAttempt +
		time.Duration(p.MaxRateLimitedAttempts)*p.RetryAfterCap
	for n := 1; n < p.MaxTransientAttempts; n++ {
		h += p.Backoff(n)
	}
	return h
}

// EffectiveGuarantee states the delivery guarantee that follows from the
// provider's declared capabilities and the retry horizon (§10.6, ADR-009).
// Exactly-once delivery is never claimed.
//
// The retention markers are not durations the worker can compare with the
// horizon: "until reset" (the mock's durable ledger) holds while the ledger
// is retained and intact; "process lifetime" (an in-memory ledger) only if
// the provider is not restarted while a retry is pending. The statement
// names the condition.
func EffectiveGuarantee(c provider.Capabilities, horizon time.Duration) string {
	if c.IdempotencyKeys && c.KeyRetention == provider.RetentionUntilReset {
		return "at most one delivered message per (campaign, user, channel) while the provider's durable idempotency ledger is retained and intact (an explicit reset forgets earlier keys) and provided it implements its declared idempotency-key semantics; requests are at-least-once; not exactly-once"
	}
	if c.IdempotencyKeys && c.KeyRetention == provider.RetentionProcessLifetime {
		return "at most one delivered message per (campaign, user, channel) only while the provider process is not restarted with a retry pending (its idempotency keys are kept in memory) and provided it implements its declared idempotency-key semantics; otherwise at-least-once; requests are at-least-once"
	}
	if c.IdempotencyKeys && c.KeyRetention >= horizon {
		return "at most one delivered message per (campaign, user, channel), provided the provider implements its declared idempotency-key semantics; requests are at-least-once"
	}
	return "at-least-once: a duplicate message is possible after an ambiguous attempt (timeout, crash after send)"
}
