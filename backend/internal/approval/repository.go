package approval

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/fyusufayan/insider-campaign-composer-case/backend/internal/campaign"
	"github.com/fyusufayan/insider-campaign-composer-case/backend/internal/content"
	"github.com/fyusufayan/insider-campaign-composer-case/backend/internal/segment"
)

// Transaction bounds. The prepare transaction runs the audience query three
// times (partition, expected snapshot, insert) and reads the inserted rows
// back; each statement is bounded, and so is the whole transaction.
const (
	// TxTimeout bounds one preparation or expiry transaction end to end
	// (context deadline: the statement in progress is cancelled and the
	// transaction rolled back).
	TxTimeout = 30 * time.Second
	// cleanupTimeout bounds the rollback that runs after ctx may have been
	// cancelled.
	cleanupTimeout = 5 * time.Second
)

// Statements. Every statement is a constant (the audience part comes from
// segment.CompileEligible); every value is a bind parameter. Expiry is
// evaluated and expiry-defining timestamps are set with clock_timestamp()
// after the campaign row lock (§5.1, ADR-006), never now().
const (
	sqlSetTimeouts = `SET LOCAL lock_timeout = '5s'; SET LOCAL statement_timeout = '10s'`

	// Locks the live approval row (lock order: campaign → approval →
	// deliveries) and evaluates expiry after the campaign lock.
	sqlLockApproval = `SELECT status, snapshot_count, expires_at <= clock_timestamp()
FROM campaign_approvals WHERE id = $1 AND campaign_id = $2 FOR UPDATE`

	sqlExpireApproval = `UPDATE campaign_approvals
SET status = 'expired', closed_at = clock_timestamp()
WHERE id = $1 AND campaign_id = $2 AND status IN ('pending', 'approved') AND expires_at <= clock_timestamp()`

	sqlRevokeApproval = `UPDATE campaign_approvals
SET status = 'revoked', revoke_reason = $3, closed_at = clock_timestamp()
WHERE id = $1 AND campaign_id = $2 AND status IN ('pending', 'approved')`

	// Held rows are the only delivery rows the application deletes, and only
	// together with closing their approval (invariant 6).
	sqlDeleteHeld = `DELETE FROM campaign_deliveries WHERE approval_id = $1 AND status = 'held'`

	sqlCountApprovalRows = `SELECT count(*) FROM campaign_deliveries WHERE approval_id = $1`

	// prepared_at and expires_at derive from one clock_timestamp() value
	// taken after the campaign lock, so expires_at - prepared_at = TTL exactly.
	sqlInsertApproval = `INSERT INTO campaign_approvals
  (campaign_id, status, segment_version, segment_rule, content_id, channels, audience_stats,
   snapshot_count, snapshot_digest, prepared_at, expires_at)
SELECT $1, 'pending', $2, $3::jsonb, $4, $5::text[]::channel[], $6::jsonb, $7, $8,
       t.ts, t.ts + $9::bigint * interval '1 microsecond'
FROM (SELECT clock_timestamp() AS ts) AS t
RETURNING id, prepared_at, expires_at`

	// sqlInsertHeldTail follows segment.EligibleQuery.With; %C and %A are
	// replaced by the campaign and approval placeholders. vars has exactly
	// the content.Values shape (first_name, last_name, city), taken
	// verbatim from the user row: no contact data, nothing normalized,
	// truncated or substituted. destination stays NULL until the first
	// dispatch precheck (§10.3); status defaults to 'held'.
	sqlInsertHeldTail = `INSERT INTO campaign_deliveries (campaign_id, approval_id, user_id, channel, vars, idempotency_key)
SELECT %C::uuid, %A::uuid, e.user_id, e.channel,
       jsonb_build_object('first_name', u.first_name, 'last_name', u.last_name, 'city', u.city),
       %C::uuid::text || ':' || e.user_id::text || ':' || e.channel::text
FROM eligible AS e
JOIN users AS u ON u.id = e.user_id`

	// Ordered by the enum (d.channel), not the text output column of the
	// same name: the digest order is sms before email.
	sqlReadSnapshot = `SELECT d.user_id, d.channel::text, d.status, d.destination IS NULL, d.vars
FROM campaign_deliveries AS d WHERE d.approval_id = $1 ORDER BY d.user_id, d.channel`
)

// Repository runs approval transactions against PostgreSQL.
type Repository struct {
	pool *pgxpool.Pool
}

// NewRepository returns a repository using pool.
func NewRepository(pool *pgxpool.Pool) *Repository { return &Repository{pool: pool} }

// PrepareParams are the inputs of one preparation.
type PrepareParams struct {
	CampaignID campaign.ID
	// ExpectedVersion is the campaign version the marketer reviewed.
	ExpectedVersion int
	PendingTTL      time.Duration
}

// Prepare runs the preparation transaction (ARCHITECTURE §5.5 "Prepare"):
//
//  1. lock the campaign row; set lock and statement timeouts;
//  2. expire a live approval that is due (clock_timestamp()); this
//     advances the version, so step 3 then fails with a version conflict
//     and everything is rolled back (Service.Prepare commits lazy expiry
//     in its own transaction first, so this only happens when the approval
//     became due between the two transactions);
//  3. check the version (CAS) and the state machine, re-read and
//     re-validate the current content version for the selected channels
//     against the current content brief (incl. offer findings);
//  4. revoke a still-live pending approval (superseded) and delete its
//     held rows (an approved campaign is refused in step 3);
//  5. compute, in ONE statement (segment.ComputeSnapshotAudience), the
//     audience partition and the expected snapshot (per-channel eligible
//     counts and digest); no eligible recipient ⇒ *EmptyAudienceError;
//  6. insert the pending approval (binding segment version + frozen rule,
//     content id, channels, partition, count, digest, prepared_at and
//     expires_at from clock_timestamp());
//  7. INSERT … SELECT the held rows (frozen vars, idempotency keys) from
//     the same eligible definition;
//  8. read the held rows back in order: counts and digest must equal step
//     5 (else ErrAudienceChanged: customer data committed between the two
//     statements; rollback); trial-render each frozen value set;
//  9. campaign → pending_approval, active_approval_id, version +1; commit.
//
// Every failure rolls back the whole transaction, so no approval ever
// points at an incomplete snapshot and a superseded approval stays live
// when preparation fails. No network I/O other than the database happens
// while the transaction is open.
func (r *Repository) Prepare(ctx context.Context, p PrepareParams) (Outcome, error) {
	if p.PendingTTL < MinPendingTTL || p.PendingTTL > MaxPendingTTL {
		return Outcome{}, fmt.Errorf("approval: pending TTL %s out of range", p.PendingTTL)
	}
	var out Outcome
	err := r.inLockedTx(ctx, p.CampaignID, func(ctx context.Context, tx pgx.Tx, cur campaign.Campaign) error {
		o, err := prepareTx(ctx, tx, cur, p)
		out = o
		return err
	})
	if err != nil {
		return Outcome{}, err
	}
	return out, nil
}

// ExpireDue expires the campaign's live approval if it is due
// (expires_at <= clock_timestamp() after the campaign lock): approval →
// expired, its held rows deleted, campaign → content_ready with
// active_approval_id cleared, all in one transaction (§5.5 "Expire"). It
// returns the expired approval's id, or nil if nothing was due.
func (r *Repository) ExpireDue(ctx context.Context, id campaign.ID) (*campaign.ID, error) {
	var expired *campaign.ID
	err := r.inLockedTx(ctx, id, func(ctx context.Context, tx pgx.Tx, cur campaign.Campaign) error {
		_, e, err := expireIfDue(ctx, tx, cur)
		expired = e
		return err
	})
	if err != nil {
		return nil, err
	}
	return expired, nil
}

// inLockedTx runs fn in one READ COMMITTED transaction whose first
// statement after the timeouts locks the campaign row (§5.1), and commits
// iff fn succeeds. Database errors that mean "try again" are wrapped with
// ErrBusy.
func (r *Repository) inLockedTx(ctx context.Context, id campaign.ID,
	fn func(ctx context.Context, tx pgx.Tx, cur campaign.Campaign) error) (err error) {
	ctx, cancel := context.WithTimeout(ctx, TxTimeout)
	defer cancel()
	defer func() { err = classify(ctx, err) }()

	tx, err := r.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted, AccessMode: pgx.ReadWrite})
	if err != nil {
		return fmt.Errorf("approval: begin: %w", err)
	}
	defer func() {
		cctx, ccancel := context.WithTimeout(context.WithoutCancel(ctx), cleanupTimeout)
		defer ccancel()
		_ = tx.Rollback(cctx) // no-op after commit
	}()
	if _, err := tx.Exec(ctx, sqlSetTimeouts); err != nil {
		return fmt.Errorf("approval: set timeouts: %w", err)
	}
	cur, err := campaign.LockTx(ctx, tx, id)
	if err != nil {
		return err
	}
	if err := fn(ctx, tx, cur); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("approval: commit: %w", err)
	}
	return nil
}

// classify wraps retryable database failures with ErrBusy: lock_timeout
// (55P03), statement_timeout (57014, only when the caller's context is still
// alive), serialization failure (40001) and deadlock (40P01).
func classify(ctx context.Context, err error) error {
	if err == nil {
		return nil
	}
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		switch pgErr.Code {
		case "55P03", "40001", "40P01":
			return fmt.Errorf("%w: %w", ErrBusy, err)
		case "57014":
			if ctx.Err() == nil {
				return fmt.Errorf("%w: %w", ErrBusy, err)
			}
		}
	}
	return err
}

// liveApproval is the locked row of the campaign's active approval.
type liveApproval struct {
	id            campaign.ID
	status        Status
	snapshotCount int64
	due           bool
}

// lockActiveApproval locks and reads the campaign's live approval. Only a
// campaign in pending_approval/approved has one, and it must be pending or
// approved respectively. In every other status there is nothing live to
// expire or supersede: a sending or finished campaign keeps pointing at its
// consumed approval (§4: active_approval_id is "set while an approval is
// pending/approved/consumed"), which consumed approvals never leave (§5.3);
// it is left untouched and the state check rejects preparation.
func lockActiveApproval(ctx context.Context, tx pgx.Tx, cur campaign.Campaign) (*liveApproval, error) {
	if cur.Status != campaign.StatusPendingApproval && cur.Status != campaign.StatusApproved {
		return nil, nil
	}
	if cur.ActiveApprovalID == nil {
		return nil, fmt.Errorf("%w: campaign %s is %s without an active approval", ErrInconsistent, cur.ID, cur.Status)
	}
	a := liveApproval{id: *cur.ActiveApprovalID}
	var (
		status string
		count  int32
	)
	err := tx.QueryRow(ctx, sqlLockApproval, uuidArg(a.id), uuidArg(cur.ID)).Scan(&status, &count, &a.due)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, fmt.Errorf("%w: campaign %s: active approval %s is missing", ErrInconsistent, cur.ID, a.id)
	}
	if err != nil {
		return nil, fmt.Errorf("approval %s: lock: %w", a.id, err)
	}
	a.status, a.snapshotCount = Status(status), int64(count)
	if !a.status.Live() {
		return nil, fmt.Errorf("%w: campaign %s (%s): active approval %s is %s", ErrInconsistent, cur.ID, cur.Status, a.id, a.status)
	}
	if (a.status == StatusPending) != (cur.Status == campaign.StatusPendingApproval) ||
		(a.status == StatusApproved) != (cur.Status == campaign.StatusApproved) {
		return nil, fmt.Errorf("%w: campaign %s is %s but its approval %s is %s", ErrInconsistent, cur.ID, cur.Status, a.id, a.status)
	}
	return &a, nil
}

// closeApproval runs stmt (expire or revoke) on the live approval a and
// deletes its held rows; their number must be its snapshot_count (a held
// row exists iff its approval is live, invariant 6).
func closeApproval(ctx context.Context, tx pgx.Tx, cur campaign.Campaign, a *liveApproval, stmt string, args ...any) error {
	tag, err := tx.Exec(ctx, stmt, append([]any{uuidArg(a.id), uuidArg(cur.ID)}, args...)...)
	if err != nil {
		return fmt.Errorf("approval %s: close: %w", a.id, err)
	}
	if tag.RowsAffected() != 1 {
		return fmt.Errorf("%w: approval %s: close affected %d rows", ErrInconsistent, a.id, tag.RowsAffected())
	}
	tag, err = tx.Exec(ctx, sqlDeleteHeld, uuidArg(a.id))
	if err != nil {
		return fmt.Errorf("approval %s: delete held rows: %w", a.id, err)
	}
	if tag.RowsAffected() != a.snapshotCount {
		return fmt.Errorf("%w: approval %s: deleted %d held rows, snapshot_count is %d",
			ErrInconsistent, a.id, tag.RowsAffected(), a.snapshotCount)
	}
	var rest int64
	if err := tx.QueryRow(ctx, sqlCountApprovalRows, uuidArg(a.id)).Scan(&rest); err != nil {
		return fmt.Errorf("approval %s: count rows: %w", a.id, err)
	}
	if rest != 0 {
		return fmt.Errorf("%w: approval %s: %d non-held delivery rows", ErrInconsistent, a.id, rest)
	}
	return nil
}

// expireIfDue expires the live approval if it is due and returns the
// updated campaign (content_ready, no active approval, version + 1) and the
// expired approval's id. Nothing is written when nothing is due, and a
// campaign without a live approval (incl. sending/finished campaigns with a
// consumed approval) is never touched.
func expireIfDue(ctx context.Context, tx pgx.Tx, cur campaign.Campaign) (campaign.Campaign, *campaign.ID, error) {
	a, err := lockActiveApproval(ctx, tx, cur)
	if err != nil || a == nil || !a.due {
		return cur, nil, err
	}
	if err := closeApproval(ctx, tx, cur, a, sqlExpireApproval); err != nil {
		return campaign.Campaign{}, nil, err
	}
	next, err := cur.WithApprovalClosed(a.id)
	if err != nil {
		return campaign.Campaign{}, nil, err
	}
	saved, err := campaign.WriteApprovalStateTx(ctx, tx, cur, next)
	if err != nil {
		return campaign.Campaign{}, nil, err
	}
	id := a.id
	return saved, &id, nil
}

func prepareTx(ctx context.Context, tx pgx.Tx, cur campaign.Campaign, p PrepareParams) (Outcome, error) {
	// 2. A due approval is never superseded: expiring it advances the
	// version, so the version check below rejects this preparation.
	cur, _, err := expireIfDue(ctx, tx, cur)
	if err != nil {
		return Outcome{}, err
	}

	// 3. Version, state machine, content.
	if err := cur.CheckPrepareAt(p.ExpectedVersion); err != nil {
		return Outcome{}, err
	}
	v, err := campaign.CurrentContentTx(ctx, tx, cur)
	if err != nil {
		return Outcome{}, err
	}
	if v == nil {
		return Outcome{}, fmt.Errorf("%w: campaign %s has no content version", ErrInconsistent, cur.ID)
	}
	review, err := cur.ReviewForApproval(*v)
	if err != nil {
		return Outcome{}, err
	}

	// 4. Supersede a still-live approval.
	live, err := lockActiveApproval(ctx, tx, cur)
	if err != nil {
		return Outcome{}, err
	}
	var superseded *campaign.ID
	if live != nil {
		// Only a pending approval is ever superseded; an approved one was
		// refused by the state check (defence in depth).
		if live.status != StatusPending {
			return Outcome{}, fmt.Errorf("%w: refusing to supersede %s approval %s", ErrInconsistent, live.status, live.id)
		}
		if err := closeApproval(ctx, tx, cur, live, sqlRevokeApproval, string(RevokeSuperseded)); err != nil {
			return Outcome{}, err
		}
		id := live.id
		superseded = &id
	}

	// 5. Partition and expected snapshot in ONE statement (one database
	// snapshot), recomputed now: preview counts are never reused.
	stats, want, err := segment.ComputeSnapshotAudience(ctx, tx, cur.Segment, cur.Channels)
	if err != nil {
		return Outcome{}, fmt.Errorf("campaign %s: audience: %w", cur.ID, err)
	}
	if stats.Messages == 0 {
		return Outcome{}, &EmptyAudienceError{Stats: stats}
	}
	eq, err := segment.CompileEligible(cur.Segment, cur.Channels)
	if err != nil {
		return Outcome{}, fmt.Errorf("campaign %s: compile eligible: %w", cur.ID, err)
	}

	// 6. The approval row.
	rule, err := json.Marshal(cur.Segment)
	if err != nil {
		return Outcome{}, fmt.Errorf("campaign %s: encode rule: %w", cur.ID, err)
	}
	statsJSON, err := json.Marshal(stats)
	if err != nil {
		return Outcome{}, fmt.Errorf("campaign %s: encode audience stats: %w", cur.ID, err)
	}
	channels := make([]string, len(cur.Channels))
	for i, ch := range cur.Channels {
		channels[i] = string(ch)
	}
	var (
		approvalUUID          pgtype.UUID
		preparedAt, expiresAt time.Time
	)
	if err := tx.QueryRow(ctx, sqlInsertApproval, uuidArg(cur.ID), cur.SegmentVersion, rule, v.ID, channels,
		statsJSON, stats.Messages, want.Digest, p.PendingTTL.Microseconds()).
		Scan(&approvalUUID, &preparedAt, &expiresAt); err != nil {
		return Outcome{}, fmt.Errorf("campaign %s: insert approval: %w", cur.ID, err)
	}
	approvalID := campaign.ID(approvalUUID.Bytes)

	// 7. The held rows.
	insertSQL := eq.With + strings.NewReplacer(
		"%C", "$"+strconv.Itoa(eq.NextParam), "%A", "$"+strconv.Itoa(eq.NextParam+1)).Replace(sqlInsertHeldTail)
	tag, err := tx.Exec(ctx, insertSQL, append(eq.Args, uuidArg(cur.ID), approvalUUID)...)
	if err != nil {
		return Outcome{}, fmt.Errorf("approval %s: insert held rows: %w", approvalID, err)
	}
	if tag.RowsAffected() != stats.Messages {
		return Outcome{}, fmt.Errorf("%w: inserted %d held rows, partition has %d messages",
			ErrAudienceChanged, tag.RowsAffected(), stats.Messages)
	}

	// 8. Read back, verify and review the frozen values.
	snap, digest, err := readSnapshot(ctx, tx, approvalID, v.Content, cur.Channels)
	if err != nil {
		return Outcome{}, err
	}
	if digest != want.Digest || snap[segment.ChannelSMS].Recipients != want.SMS ||
		snap[segment.ChannelEmail].Recipients != want.Email {
		return Outcome{}, fmt.Errorf("%w: held rows differ from the expected snapshot", ErrAudienceChanged)
	}
	for _, ch := range []segment.Channel{segment.ChannelSMS, segment.ChannelEmail} {
		if _, selected := stats.ByChannel[ch]; !selected {
			delete(snap, ch)
		}
	}

	// 9. Campaign state.
	next, err := cur.WithPreparedApproval(p.ExpectedVersion, approvalID)
	if err != nil {
		return Outcome{}, err
	}
	saved, err := campaign.WriteApprovalStateTx(ctx, tx, cur, next)
	if err != nil {
		return Outcome{}, err
	}
	return Outcome{Campaign: saved, Summary: Summary{
		ApprovalID:        approvalID,
		CampaignID:        saved.ID,
		Status:            StatusPending,
		CampaignVersion:   saved.Version,
		SegmentVersion:    saved.SegmentVersion,
		ContentID:         v.ID,
		ContentVersion:    v.Number,
		ContentProvenance: v.Provenance,
		Channels:          append([]segment.Channel{}, saved.Channels...),
		AudienceStats:     stats,
		SnapshotCount:     stats.Messages,
		SnapshotDigest:    want.Digest,
		PreparedAt:        preparedAt.UTC(),
		ExpiresAt:         expiresAt.UTC(),
		Snapshot:          snap,
		Reviewed:          true,
		Review:            review,
		Superseded:        superseded,
	}}, nil
}

// readSnapshot streams the approval's delivery rows in digest order,
// verifies that each is a fresh held row with strictly shaped vars,
// computes the digest and per-channel counts, and trial-renders every row.
// The values never leave this function.
func readSnapshot(ctx context.Context, tx pgx.Tx, approvalID campaign.ID, c content.Content,
	selected []segment.Channel) (map[segment.Channel]ChannelSnapshot, string, error) {
	rows, err := tx.Query(ctx, sqlReadSnapshot, uuidArg(approvalID))
	if err != nil {
		return nil, "", fmt.Errorf("approval %s: read snapshot: %w", approvalID, err)
	}
	defer rows.Close()
	snap := map[segment.Channel]ChannelSnapshot{
		segment.ChannelSMS:   {RenderIssues: map[string]int64{}},
		segment.ChannelEmail: {RenderIssues: map[string]int64{}},
	}
	isSelected := map[segment.Channel]bool{}
	for _, ch := range selected {
		isSelected[ch] = true
	}
	d := newDigester()
	var (
		userID        int64
		channel       string
		status        string
		noDestination bool
		vars          []byte
	)
	for rows.Next() {
		if err := rows.Scan(&userID, &channel, &status, &noDestination, &vars); err != nil {
			return nil, "", fmt.Errorf("approval %s: scan snapshot: %w", approvalID, err)
		}
		ch := segment.Channel(channel)
		if status != "held" || !noDestination || !isSelected[ch] {
			return nil, "", fmt.Errorf("%w: approval %s: unexpected delivery row state", ErrInconsistent, approvalID)
		}
		if err := d.add(userID, ch); err != nil {
			return nil, "", err
		}
		values, err := decodeVars(vars)
		if err != nil {
			return nil, "", fmt.Errorf("%w: approval %s: %w", ErrInconsistent, approvalID, err)
		}
		code, err := renderIssue(c, ch, values)
		if err != nil {
			return nil, "", err
		}
		cs := snap[ch]
		cs.Recipients++
		if code != "" {
			cs.RenderIssues[code]++
		}
		snap[ch] = cs
	}
	if err := rows.Err(); err != nil {
		return nil, "", fmt.Errorf("approval %s: read snapshot: %w", approvalID, err)
	}
	return snap, d.sum(), nil
}

// decodeVars strictly decodes frozen personalization values into
// content.Values: exactly first_name, last_name and city, all strings.
func decodeVars(data []byte) (content.Values, error) {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		return content.Values{}, fmt.Errorf("decode vars: %w", err)
	}
	if len(raw) != 3 {
		return content.Values{}, errors.New("vars must have exactly first_name, last_name and city")
	}
	var v content.Values
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&v); err != nil {
		return content.Values{}, fmt.Errorf("decode vars: %w", err)
	}
	for _, k := range []string{"first_name", "last_name", "city"} {
		if s := raw[k]; len(s) == 0 || s[0] != '"' {
			return content.Values{}, errors.New("vars values must be strings")
		}
	}
	return v, nil
}

func uuidArg(id campaign.ID) pgtype.UUID { return pgtype.UUID{Bytes: id, Valid: true} }
