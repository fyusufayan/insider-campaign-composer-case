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
// CP16 scope: no rate limiter yet (CP17 adds the per-channel sliding-window
// limiter and the slot-deadline gate inside gatedSender.Attempt); retries
// use a fixed, bounded delay (CP17 replaces it with exponential backoff
// with jitter); no expiry sweeper, heartbeat or metrics yet.
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

// Policy is the retry policy (§10.5, ADR-012). CP16 uses a fixed delay
// between transient attempts; CP17 replaces RetryDelay with exponential
// backoff and full jitter.
type Policy struct {
	// MaxTransientAttempts bounds non-429 attempts (incl. timeouts and
	// unknown outcomes): the row fails after this many (MAX_TRANSIENT_ATTEMPTS).
	MaxTransientAttempts int
	// MaxRateLimitedAttempts bounds 429 answers (MAX_RATE_LIMITED_ATTEMPTS).
	MaxRateLimitedAttempts int
	// RetryDelay is the delay before a transient retry (DELIVERY_RETRY_DELAY).
	RetryDelay time.Duration
	// RetryAfterCap caps a 429's Retry-After (RETRY_AFTER_CAP).
	RetryAfterCap time.Duration
}

// DefaultPolicy is the §16 configuration (with the CP16 fixed delay).
var DefaultPolicy = Policy{MaxTransientAttempts: 5, MaxRateLimitedAttempts: 10, RetryDelay: 5 * time.Second,
	RetryAfterCap: provider.DefaultRetryAfterCap}

// Validate checks the policy.
func (p Policy) Validate() error {
	switch {
	case p.MaxTransientAttempts < 1 || p.MaxTransientAttempts > 20:
		return errors.New("delivery: MaxTransientAttempts must be in [1, 20]")
	case p.MaxRateLimitedAttempts < 1 || p.MaxRateLimitedAttempts > 50:
		return errors.New("delivery: MaxRateLimitedAttempts must be in [1, 50]")
	case p.RetryDelay < time.Millisecond || p.RetryDelay > 10*time.Minute:
		return errors.New("delivery: RetryDelay must be in [1ms, 10m]")
	case p.RetryAfterCap < time.Millisecond || p.RetryAfterCap > 10*time.Minute:
		return errors.New("delivery: RetryAfterCap must be in [1ms, 10m]")
	}
	return nil
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
}

// Decide applies the retry policy to the outcome of an attempt made after
// attempts completed non-429 attempts and rateLimited 429 answers. It is
// pure.
//
//   - accepted / replayed → sent;
//   - rate_limited → retry after Retry-After (≥ 1 s, ≤ cap), failed once
//     MaxRateLimitedAttempts 429s were received;
//   - transient_error / timeout / unknown → retry after RetryDelay, failed
//     once MaxTransientAttempts attempts were made. A retry of an
//     ambiguous attempt is safe only because every retry sends the same
//     idempotency key, destination and body (§10.6);
//   - permanent_error → failed.
func (p Policy) Decide(o provider.Outcome, attempts, rateLimited int, retryAfter time.Duration) (Decision, error) {
	switch o {
	case provider.OutcomeAccepted, provider.OutcomeReplayed:
		return Decision{Status: StatusSent, AttemptsInc: 1}, nil
	case provider.OutcomeRateLimited:
		d := Decision{RateLimitedInc: 1}
		if rateLimited+1 >= p.MaxRateLimitedAttempts {
			d.Status = StatusFailed
			return d, nil
		}
		d.Status, d.Delay = StatusRetryScheduled, min(max(retryAfter, provider.DefaultRetryAfter), p.RetryAfterCap)
		return d, nil
	case provider.OutcomeTransientError, provider.OutcomeTimeout, provider.OutcomeUnknown:
		d := Decision{AttemptsInc: 1}
		if attempts+1 >= p.MaxTransientAttempts {
			d.Status = StatusFailed
			return d, nil
		}
		d.Status, d.Delay = StatusRetryScheduled, p.RetryDelay
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
// delays between transient attempts and the 429 waits.
func RetryHorizon(p Policy, requestTimeout, lease, reaperInterval time.Duration) time.Duration {
	perAttempt := max(requestTimeout, lease+reaperInterval)
	return time.Duration(p.MaxTransientAttempts)*perAttempt +
		time.Duration(p.MaxTransientAttempts-1)*p.RetryDelay +
		time.Duration(p.MaxRateLimitedAttempts)*p.RetryAfterCap
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
