package delivery

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/fyusufayan/insider-campaign-composer-case/backend/internal/content"
	"github.com/fyusufayan/insider-campaign-composer-case/backend/internal/provider"
	"github.com/fyusufayan/insider-campaign-composer-case/backend/internal/segment"
)

// Worker statements (§5.5 "Claim", "Precheck", "Complete", "Reap",
// "Completion detection"). Every statement is a constant and every value a
// bind parameter. Worker statements never lock the campaign row except
// completion detection, which updates it (one statement, no delivery-row
// locks, so it cannot deadlock with the send/cancel transactions that lock
// the campaign first). No statement runs while a provider request is in
// flight on the same row's behalf with a transaction open: the precheck
// commits before the request, the completion starts after the response.
const (
	sqlSetWorkerTimeouts = `SET LOCAL lock_timeout = '5s'; SET LOCAL statement_timeout = '10s'`

	// Claim: only rows of the consumed approval of a sending campaign, ready
	// now, in deterministic order (next_attempt_at, id), skipping rows
	// locked by another claimer. The lease makes a crashed worker's rows
	// recoverable (reaper). dispatch_started_at is cleared: no request of
	// this claim has happened yet.
	sqlClaim = `WITH picked AS (
  SELECT d.id
  FROM campaign_deliveries AS d
  JOIN campaign_approvals AS a ON a.id = d.approval_id AND a.status = 'consumed'
  JOIN campaigns AS c ON c.id = d.campaign_id AND c.status = 'sending' AND c.active_approval_id = d.approval_id
  WHERE d.channel = $1::channel AND d.status IN ('pending', 'retry_scheduled') AND d.next_attempt_at <= now()
  ORDER BY d.next_attempt_at, d.id
  LIMIT $2
  FOR UPDATE OF d SKIP LOCKED
)
UPDATE campaign_deliveries AS d
SET status = 'in_flight', lease_until = now() + $3::bigint * interval '1 microsecond', leased_by = $4,
    dispatch_started_at = NULL, updated_at = now()
FROM picked, campaign_approvals AS a
WHERE d.id = picked.id AND a.id = d.approval_id
RETURNING d.id, d.campaign_id::text, d.approval_id::text, d.channel::text, d.vars, d.idempotency_key,
          d.attempts, d.rate_limited_attempts, a.content_id`

	// validContact is the delivery-time contact rule: an E.164 phone for
	// SMS, a syntactically plausible address of at most 254 characters for
	// email. A missing or invalid contact is no contact.
	validContact = `COALESCE(CASE d.channel
    WHEN 'sms' THEN u.phone ~ '^\+[1-9][0-9]{7,14}$'
    ELSE char_length(u.email) <= 254 AND u.email ~ '^[^@[:space:]]+@[^@[:space:]]+\.[^@[:space:]]+$' END, false)`

	consentOptedIn = `EXISTS (SELECT 1 FROM channel_consents AS cc
              WHERE cc.user_id = d.user_id AND cc.channel = d.channel AND cc.status = 'opted_in')`

	// Precheck (§10.3), one statement immediately before the request:
	// fencing (we still own the lease and no request of this claim has been
	// made), campaign still sending, approval consumed, CURRENT consent
	// opted_in (a missing consent row is not consent), and a destination:
	// the persisted one on retries, otherwise the user's CURRENT contact,
	// validated and persisted here — before the request — and never changed
	// afterwards. The lease is renewed. Audience membership and the frozen
	// vars are not looked at.
	sqlPrecheck = `UPDATE campaign_deliveries AS d
SET destination         = COALESCE(d.destination, CASE d.channel WHEN 'sms' THEN u.phone ELSE u.email END),
    dispatch_started_at = now(),
    lease_until         = now() + $3::bigint * interval '1 microsecond',
    updated_at          = now()
FROM users AS u, campaigns AS c, campaign_approvals AS a
WHERE d.id = $1 AND d.leased_by = $2 AND d.status = 'in_flight' AND d.dispatch_started_at IS NULL
  AND u.id = d.user_id
  AND c.id = d.campaign_id AND c.status = 'sending'
  AND a.id = d.approval_id AND a.status = 'consumed'
  AND ` + consentOptedIn + `
  AND (d.destination IS NOT NULL OR ` + validContact + `)
RETURNING d.destination, d.dispatch_started_at`

	// Why did the precheck match nothing? Evaluated in §10.3 order.
	sqlClassifyMiss = `SELECT d.status = 'in_flight' AND d.leased_by IS NOT DISTINCT FROM $2 AND d.dispatch_started_at IS NULL,
       c.status = 'sending' AND a.status = 'consumed',
       ` + consentOptedIn + `,
       d.destination IS NOT NULL OR ` + validContact + `
FROM campaign_deliveries AS d
JOIN users AS u ON u.id = d.user_id
JOIN campaigns AS c ON c.id = d.campaign_id
JOIN campaign_approvals AS a ON a.id = d.approval_id
WHERE d.id = $1`

	priorAmbiguous = `EXISTS (SELECT 1 FROM delivery_attempts AS x WHERE x.delivery_id = d.id AND x.outcome IN ('timeout', 'unknown'))`

	// A terminal status without a provider request (precheck skip, render
	// failure, campaign no longer sending). A render failure of a campaign
	// that is no longer sending is recorded as cancelled. outcome_uncertain
	// is set when an earlier attempt of the row was ambiguous (§10.7 step 5).
	sqlFinishWithoutRequest = `UPDATE campaign_deliveries AS d
SET status = CASE WHEN $3::text = 'failed' AND c.status <> 'sending' THEN 'cancelled' ELSE $3::text END,
    last_error = $4, outcome_uncertain = d.outcome_uncertain OR ` + priorAmbiguous + `,
    lease_until = NULL, leased_by = NULL, dispatch_started_at = NULL, updated_at = now()
FROM campaigns AS c
WHERE d.id = $1 AND d.leased_by = $2 AND d.status = 'in_flight' AND d.dispatch_started_at IS NULL
  AND c.id = d.campaign_id
RETURNING d.status`

	// Gives a claimed row back without counting an attempt: no request was
	// made (shutdown before the precheck, precheck failed or raced). Also
	// clears dispatch_started_at of a precheck whose outcome is unknown.
	sqlRelease = `UPDATE campaign_deliveries
SET status = 'pending', next_attempt_at = now() + $3::bigint * interval '1 microsecond',
    lease_until = NULL, leased_by = NULL, dispatch_started_at = NULL, updated_at = now()
WHERE id = $1 AND leased_by = $2 AND status = 'in_flight'`

	// Complete, step 1: lock our row (guarded by lease owner, status and
	// the dispatch_started_at of our precheck — a fencing token, so a
	// re-claimed row is never completed by a stale attempt).
	sqlLockForComplete = `SELECT d.attempts, d.rate_limited_attempts, d.destination, d.outcome_uncertain, c.status,
       ` + priorAmbiguous + `
FROM campaign_deliveries AS d JOIN campaigns AS c ON c.id = d.campaign_id
WHERE d.id = $1 AND d.leased_by = $2 AND d.status = 'in_flight' AND d.dispatch_started_at = $3
FOR UPDATE OF d`

	sqlCompleteRow = `UPDATE campaign_deliveries
SET status = $2, attempts = attempts + $3, rate_limited_attempts = rate_limited_attempts + $4,
    provider_message_id = CASE WHEN $2 = 'sent' THEN $5 ELSE provider_message_id END,
    sent_at = CASE WHEN $2 = 'sent' THEN now() ELSE sent_at END,
    next_attempt_at = CASE WHEN $2 = 'retry_scheduled' THEN now() + $6::bigint * interval '1 microsecond' ELSE next_attempt_at END,
    last_error = $7, outcome_uncertain = $8,
    lease_until = NULL, leased_by = NULL, dispatch_started_at = NULL, updated_at = now()
WHERE id = $1`

	sqlInsertAttempt = `INSERT INTO delivery_attempts
  (delivery_id, attempt_no, destination, outcome, http_status, retry_after_ms, error, started_at, finished_at)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, now())`

	// Reaper: in-flight rows whose lease expired (crashed or stalled worker).
	sqlLockExpiredLeases = `SELECT d.id, d.dispatch_started_at, d.destination, d.attempts, d.rate_limited_attempts,
       d.outcome_uncertain, c.status = 'sending' AND a.status = 'consumed', ` + priorAmbiguous + `
FROM campaign_deliveries AS d
JOIN campaigns AS c ON c.id = d.campaign_id
JOIN campaign_approvals AS a ON a.id = d.approval_id
WHERE d.status = 'in_flight' AND d.lease_until < now()
ORDER BY d.lease_until, d.id
LIMIT $1
FOR UPDATE OF d SKIP LOCKED`

	sqlReapUndispatched = `UPDATE campaign_deliveries
SET status = 'pending', next_attempt_at = now(), lease_until = NULL, leased_by = NULL, dispatch_started_at = NULL, updated_at = now()
WHERE id = $1 AND status = 'in_flight'`

	sqlSendingCampaigns = `SELECT id::text FROM campaigns WHERE status = 'sending' ORDER BY send_started_at NULLS FIRST, id LIMIT $1`

	// Completion detection: a sending campaign whose consumed approval's rows
	// are all terminal (and exactly snapshot_count of them exist) becomes
	// completed, or completed_with_failures if any row failed (§5.2: skipped
	// and cancelled rows are not failures). The status predicate makes the
	// transition happen once even when several workers try concurrently
	// (the second waits for the row lock, then no longer matches).
	sqlCompleteCampaign = `UPDATE campaigns AS c
SET status = CASE WHEN EXISTS (SELECT 1 FROM campaign_deliveries AS d WHERE d.approval_id = a.id AND d.status = 'failed')
                  THEN 'completed_with_failures' ELSE 'completed' END,
    completed_at = now(), version = c.version + 1, updated_at = now()
FROM campaign_approvals AS a
WHERE c.id = $1::uuid AND c.status = 'sending' AND a.id = c.active_approval_id AND a.status = 'consumed'
  AND (SELECT count(*) FROM campaign_deliveries AS d WHERE d.approval_id = a.id) = a.snapshot_count
  AND NOT EXISTS (SELECT 1 FROM campaign_deliveries AS d WHERE d.approval_id = a.id
                  AND d.status IN ('held', 'pending', 'in_flight', 'retry_scheduled'))
RETURNING c.status, c.version, a.id::text`

	sqlStatusCounts = `SELECT d.channel::text, d.status, count(*), count(*) FILTER (WHERE d.outcome_uncertain)
FROM campaign_deliveries AS d WHERE d.approval_id = $1::uuid GROUP BY 1, 2 ORDER BY 1, 2`

	sqlApprovedContent = `SELECT c.sms_body, c.email_subject, c.email_body, a.channels::text[]
FROM campaign_approvals AS a
JOIN campaign_contents AS c ON c.id = a.content_id AND c.campaign_id = a.campaign_id
WHERE a.id = $1::uuid AND a.status = 'consumed'`
)

// Job is a claimed delivery row. Vars (frozen personalization values) are
// kept in memory only while the worker owns the row; they are never logged.
type Job struct {
	ID             int64
	CampaignID     string
	ApprovalID     string
	Channel        provider.Channel
	Vars           []byte
	IdempotencyKey string
	Attempts       int
	RateLimited    int
	ContentID      int64
}

// Repository runs the worker's statements against PostgreSQL.
type Repository struct {
	pool *pgxpool.Pool
}

// NewRepository returns a repository using pool.
func NewRepository(pool *pgxpool.Pool) *Repository { return &Repository{pool: pool} }

func micros(d time.Duration) int64 { return d.Microseconds() }

// Claim leases up to n ready rows of channel to worker.
func (r *Repository) Claim(ctx context.Context, ch provider.Channel, n int, worker string, lease time.Duration) ([]Job, error) {
	rows, err := r.pool.Query(ctx, sqlClaim, string(ch), n, micros(lease), worker)
	if err != nil {
		return nil, fmt.Errorf("delivery: claim: %w", err)
	}
	jobs, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (Job, error) {
		var (
			j        Job
			channel  string
			att, rla int32
		)
		err := row.Scan(&j.ID, &j.CampaignID, &j.ApprovalID, &channel, &j.Vars, &j.IdempotencyKey, &att, &rla, &j.ContentID)
		j.Channel, j.Attempts, j.RateLimited = provider.Channel(channel), int(att), int(rla)
		return j, err
	})
	if err != nil {
		return nil, fmt.Errorf("delivery: claim: %w", err)
	}
	return jobs, nil
}

// Precheck is the result of a successful precheck.
type Precheck struct {
	Destination string
	// DispatchStartedAt fences the completion of this attempt.
	DispatchStartedAt time.Time
}

// ErrPrecheckMiss: the precheck matched no row; the row must not be sent
// (see ClassifyMiss).
var ErrPrecheckMiss = errors.New("delivery: precheck matched no row")

// Precheck runs the precheck statement. ErrPrecheckMiss means the row must
// not be sent; ClassifyMiss tells why.
func (r *Repository) Precheck(ctx context.Context, j Job, worker string, lease time.Duration) (Precheck, error) {
	var p Precheck
	err := r.pool.QueryRow(ctx, sqlPrecheck, j.ID, worker, micros(lease)).Scan(&p.Destination, &p.DispatchStartedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return Precheck{}, ErrPrecheckMiss
	}
	if err != nil {
		return Precheck{}, fmt.Errorf("delivery %d: precheck: %w", j.ID, err)
	}
	return p, nil
}

// MissReason explains a precheck that matched no row.
type MissReason int

// Reasons, in the §10.3 evaluation order.
const (
	// MissNone: nothing is wrong any more (the state changed between the
	// two statements); the row is released and retried.
	MissNone MissReason = iota
	// MissLeaseLost: another worker (or the reaper) owns the row; nothing
	// is written.
	MissLeaseLost
	// MissNotSending: the campaign is no longer sending (cancelled) →
	// cancelled.
	MissNotSending
	// MissNoConsent: no current opted_in consent for the channel →
	// skipped_consent_revoked.
	MissNoConsent
	// MissNoContact: no persisted destination and no valid current contact
	// (only possible before the first request) → skipped_no_contact.
	MissNoContact
)

// ClassifyMiss reads why the precheck did not match.
func (r *Repository) ClassifyMiss(ctx context.Context, j Job, worker string) (MissReason, error) {
	var owned, sending, consent, contact bool
	err := r.pool.QueryRow(ctx, sqlClassifyMiss, j.ID, worker).Scan(&owned, &sending, &consent, &contact)
	if errors.Is(err, pgx.ErrNoRows) {
		return MissLeaseLost, nil
	}
	if err != nil {
		return 0, fmt.Errorf("delivery %d: classify precheck: %w", j.ID, err)
	}
	switch {
	case !owned:
		return MissLeaseLost, nil
	case !sending:
		return MissNotSending, nil
	case !consent:
		return MissNoConsent, nil
	case !contact:
		return MissNoContact, nil
	}
	return MissNone, nil
}

// FinishWithoutRequest moves an owned, not yet dispatched row to a terminal
// status (no attempt row: no request was made). ok is false if the lease
// was lost.
func (r *Repository) FinishWithoutRequest(ctx context.Context, j Job, worker string, s Status, lastError string) (Status, bool, error) {
	if !s.Terminal() || s == StatusSent {
		return "", false, fmt.Errorf("delivery %d: %s is not a terminal status without a request", j.ID, s)
	}
	var final string
	err := r.pool.QueryRow(ctx, sqlFinishWithoutRequest, j.ID, worker, string(s), lastError).Scan(&final)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("delivery %d: finish: %w", j.ID, err)
	}
	return Status(final), true, nil
}

// Release gives an owned row back (pending, claimable after delay) without
// counting an attempt. Only for rows whose request was not made.
func (r *Repository) Release(ctx context.Context, j Job, worker string, delay time.Duration) (bool, error) {
	tag, err := r.pool.Exec(ctx, sqlRelease, j.ID, worker, micros(delay))
	if err != nil {
		return false, fmt.Errorf("delivery %d: release: %w", j.ID, err)
	}
	return tag.RowsAffected() == 1, nil
}

// Completion is the persisted result of one attempt.
type Completion struct {
	Status    Status
	AttemptNo int
	Uncertain bool
}

// Complete records the result of the provider request made after precheck
// p, in one transaction: the guarded status update and the attempt row
// (§5.5 "Complete"). The decision is computed from the row's counters as
// locked here. A retry of a campaign that is no longer sending becomes
// cancelled. ok is false if the row is no longer ours (lease expired and
// reaped); then nothing is written.
func (r *Repository) Complete(ctx context.Context, j Job, worker string, p Precheck, res provider.Result, policy Policy) (c Completion, ok bool, err error) {
	err = pgx.BeginTxFunc(ctx, r.pool, pgx.TxOptions{IsoLevel: pgx.ReadCommitted}, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, sqlSetWorkerTimeouts); err != nil {
			return err
		}
		var (
			att, rla       int32
			dest           *string
			uncertain      bool
			campaignStatus string
			prior          bool
		)
		err := tx.QueryRow(ctx, sqlLockForComplete, j.ID, worker, p.DispatchStartedAt).
			Scan(&att, &rla, &dest, &uncertain, &campaignStatus, &prior)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		if dest == nil { // the precheck persisted it; a row without one is a bug
			return fmt.Errorf("delivery %d: dispatched row without a destination", j.ID)
		}
		d, err := policy.Decide(res.Outcome, int(att), int(rla), res.RetryAfter)
		if err != nil {
			return err
		}
		c = Completion{Status: d.Status, AttemptNo: int(att) + int(rla) + 1, Uncertain: uncertain}
		if c.Status == StatusRetryScheduled && campaignStatus != "sending" {
			c.Status = StatusCancelled
		}
		switch {
		case c.Status == StatusSent:
			c.Uncertain = false
		case c.Status.Terminal() && (prior || res.Outcome.Ambiguous()):
			c.Uncertain = true
		}
		var (
			msgID     *string
			lastError *string
		)
		if c.Status == StatusSent {
			msgID = &res.MessageID
		} else if res.ErrorCode != "" {
			lastError = &res.ErrorCode
		}
		if _, err := tx.Exec(ctx, sqlCompleteRow, j.ID, string(c.Status), d.AttemptsInc, d.RateLimitedInc, msgID,
			micros(d.Delay), lastError, c.Uncertain); err != nil {
			return err
		}
		var httpStatus, retryAfter *int
		if res.HTTPStatus != 0 {
			hs := res.HTTPStatus
			httpStatus = &hs
		}
		if res.Outcome == provider.OutcomeRateLimited {
			ms := int(res.RetryAfter.Milliseconds())
			retryAfter = &ms
		}
		var attemptErr *string
		if res.ErrorCode != "" {
			attemptErr = &res.ErrorCode
		}
		if _, err := tx.Exec(ctx, sqlInsertAttempt, j.ID, c.AttemptNo, *dest, string(res.Outcome), httpStatus, retryAfter,
			attemptErr, p.DispatchStartedAt); err != nil {
			return err
		}
		ok = true
		return nil
	})
	if err != nil {
		return Completion{}, false, fmt.Errorf("delivery %d: complete: %w", j.ID, err)
	}
	return c, ok, nil
}

// ReapStats counts what one reaper pass did.
type ReapStats struct {
	// Released: leases expired before any request (back to pending).
	Released int
	// Unknown: leases expired after a request may have been made (an
	// 'unknown' attempt was recorded).
	Unknown int
}

// Reap recovers up to n rows whose lease expired (§10.7), in one short
// transaction. A row whose precheck never ran goes back to pending without
// counting an attempt. A row whose request may have been sent gets an
// 'unknown' attempt (attempts + 1) and is rescheduled (same key,
// destination and body on the retry, so an idempotent provider replays),
// failed when the attempts are exhausted, or cancelled when the campaign is
// no longer sending; a terminal non-sent row is marked uncertain.
func (r *Repository) Reap(ctx context.Context, n int, policy Policy) (ReapStats, error) {
	var st ReapStats
	err := pgx.BeginTxFunc(ctx, r.pool, pgx.TxOptions{IsoLevel: pgx.ReadCommitted}, func(tx pgx.Tx) error {
		st = ReapStats{}
		if _, err := tx.Exec(ctx, sqlSetWorkerTimeouts); err != nil {
			return err
		}
		type due struct {
			id                int64
			dispatchStartedAt *time.Time
			destination       *string
			attempts, rl      int32
			uncertain         bool
			sending, prior    bool
		}
		rows, err := tx.Query(ctx, sqlLockExpiredLeases, n)
		if err != nil {
			return err
		}
		list, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (due, error) {
			var d due
			err := row.Scan(&d.id, &d.dispatchStartedAt, &d.destination, &d.attempts, &d.rl, &d.uncertain, &d.sending, &d.prior)
			return d, err
		})
		if err != nil {
			return err
		}
		for _, d := range list {
			if d.dispatchStartedAt == nil || d.destination == nil {
				if _, err := tx.Exec(ctx, sqlReapUndispatched, d.id); err != nil {
					return err
				}
				st.Released++
				continue
			}
			dec, err := policy.Decide(provider.OutcomeUnknown, int(d.attempts), int(d.rl), 0)
			if err != nil {
				return err
			}
			status := dec.Status
			if status == StatusRetryScheduled && !d.sending {
				status = StatusCancelled
			}
			uncertain := d.uncertain || status.Terminal()
			if _, err := tx.Exec(ctx, sqlCompleteRow, d.id, string(status), dec.AttemptsInc, dec.RateLimitedInc, nil,
				micros(dec.Delay), "lease_expired", uncertain); err != nil {
				return err
			}
			if _, err := tx.Exec(ctx, sqlInsertAttempt, d.id, int(d.attempts)+int(d.rl)+1, *d.destination,
				string(provider.OutcomeUnknown), nil, nil, "lease_expired", *d.dispatchStartedAt); err != nil {
				return err
			}
			st.Unknown++
		}
		return nil
	})
	if err != nil {
		return ReapStats{}, fmt.Errorf("delivery: reap: %w", err)
	}
	return st, nil
}

// SendingCampaigns lists up to n campaigns in status sending.
func (r *Repository) SendingCampaigns(ctx context.Context, n int) ([]string, error) {
	rows, err := r.pool.Query(ctx, sqlSendingCampaigns, n)
	if err != nil {
		return nil, fmt.Errorf("delivery: sending campaigns: %w", err)
	}
	ids, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return nil, fmt.Errorf("delivery: sending campaigns: %w", err)
	}
	return ids, nil
}

// StatusCount is the number of rows in one (channel, status) cell.
type StatusCount struct {
	Channel   provider.Channel
	Status    Status
	Rows      int64
	Uncertain int64
}

// CampaignCompletion is the result of a completion check.
type CampaignCompletion struct {
	// Completed is true iff this call performed the transition.
	Completed  bool
	Status     string
	Version    int
	ApprovalID string
	// Counts is the final partition (only when Completed).
	Counts []StatusCount
}

// CompleteCampaign completes the sending campaign if every row of its
// consumed approval is terminal (§5.5 "Completion detection"); otherwise it
// writes nothing.
func (r *Repository) CompleteCampaign(ctx context.Context, campaignID string) (CampaignCompletion, error) {
	var (
		out     CampaignCompletion
		version int32
	)
	err := r.pool.QueryRow(ctx, sqlCompleteCampaign, campaignID).Scan(&out.Status, &version, &out.ApprovalID)
	if errors.Is(err, pgx.ErrNoRows) {
		return CampaignCompletion{}, nil
	}
	if err != nil {
		return CampaignCompletion{}, fmt.Errorf("delivery: complete campaign %s: %w", campaignID, err)
	}
	out.Completed, out.Version = true, int(version)
	rows, err := r.pool.Query(ctx, sqlStatusCounts, out.ApprovalID)
	if err != nil {
		return out, fmt.Errorf("delivery: campaign %s counts: %w", campaignID, err)
	}
	out.Counts, err = pgx.CollectRows(rows, func(row pgx.CollectableRow) (StatusCount, error) {
		var (
			c          StatusCount
			ch, status string
		)
		err := row.Scan(&ch, &status, &c.Rows, &c.Uncertain)
		c.Channel, c.Status = provider.Channel(ch), Status(status)
		return c, err
	})
	if err != nil {
		return out, fmt.Errorf("delivery: campaign %s counts: %w", campaignID, err)
	}
	return out, nil
}

// ApprovedContent reads and re-validates the content version bound to the
// consumed approval approvalID. Wrapped errContentInvalid means the stored
// text does not parse (permanent); other errors are database errors.
func (r *Repository) ApprovedContent(ctx context.Context, approvalID string) (content.Content, error) {
	var (
		d   content.Draft
		chs []string
	)
	err := r.pool.QueryRow(ctx, sqlApprovedContent, approvalID).Scan(&d.SMSBody, &d.EmailSubject, &d.EmailBody, &chs)
	if err != nil {
		return content.Content{}, fmt.Errorf("delivery: approved content of %s: %w", approvalID, err)
	}
	channels := make([]segment.Channel, len(chs))
	for i, ch := range chs {
		channels[i] = segment.Channel(ch)
	}
	return ParseApprovedContent(d, channels)
}
