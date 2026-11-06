package approval

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/fyusufayan/insider-campaign-composer-case/backend/internal/campaign"
	"github.com/fyusufayan/insider-campaign-composer-case/backend/internal/segment"
)

// CP15: reading, confirming and withdrawing an approval (ARCHITECTURE §5.2,
// §5.3, §5.5). Every decision runs in one short READ COMMITTED transaction
// that locks the campaign row first (§5.1), applies lazy expiry under that
// lock with clock_timestamp(), re-checks every precondition and writes the
// approval, its held rows and the campaign together. No I/O but the
// database happens while it is open (no LLM, no provider).

// Outcome is an approval operation's result: the campaign as committed (or
// read) and the approval summary.
type Outcome struct {
	Campaign campaign.Campaign
	Summary  Summary
}

const (
	approvalColumns = `id, campaign_id, status, revoke_reason, segment_version, content_id, channels::text[],
  audience_stats, snapshot_count, snapshot_digest, prepared_at, expires_at, approved_at, approved_by, closed_at`

	sqlSelectApproval          = `SELECT ` + approvalColumns + ` FROM campaign_approvals WHERE id = $1 AND campaign_id = $2`
	sqlSelectApprovalForUpdate = sqlSelectApproval + ` FOR UPDATE`

	// The confirming CAS (§5.5 "Confirm"): pending, not expired at the
	// moment of evaluation (clock_timestamp() after the campaign lock,
	// never now()), and still bound to the reviewed segment version,
	// content version and channels. approved_at and the new expires_at
	// derive from one clock_timestamp() value, so expires_at - approved_at
	// is exactly the approved TTL.
	sqlConfirmApproval = `UPDATE campaign_approvals AS a
SET status = 'approved', approved_at = t.ts, approved_by = $3,
    expires_at = t.ts + $4::bigint * interval '1 microsecond'
FROM (SELECT clock_timestamp() AS ts) AS t
WHERE a.id = $1 AND a.campaign_id = $2 AND a.status = 'pending' AND a.expires_at > t.ts
  AND a.segment_version = $5 AND a.content_id = $6 AND a.channels = $7::text[]::channel[]
RETURNING a.approved_at, a.expires_at`

	sqlReadOnlyTimeouts = `SET LOCAL statement_timeout = '10s'`
)

// row is a campaign_approvals row.
type row struct {
	id, campaignID campaign.ID
	status         Status
	revokeReason   *RevokeReason
	segmentVersion int
	contentID      int64
	channels       []segment.Channel
	stats          segment.AudienceStats
	snapshotCount  int64
	digest         string
	preparedAt     time.Time
	expiresAt      time.Time
	approvedAt     *time.Time
	approvedBy     string
	closedAt       *time.Time
}

func readRow(ctx context.Context, tx pgx.Tx, stmt string, approvalID, campaignID campaign.ID) (row, error) {
	var (
		r                 row
		id, cid           pgtype.UUID
		status            string
		reason, by        *string
		segVersion, count int32
		chs               []string
		stats             []byte
	)
	err := tx.QueryRow(ctx, stmt, uuidArg(approvalID), uuidArg(campaignID)).Scan(&id, &cid, &status, &reason, &segVersion,
		&r.contentID, &chs, &stats, &count, &r.digest, &r.preparedAt, &r.expiresAt, &r.approvedAt, &by, &r.closedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return row{}, ErrNotFound
	}
	if err != nil {
		return row{}, fmt.Errorf("approval %s: select: %w", approvalID, err)
	}
	r.id, r.campaignID = campaign.ID(id.Bytes), campaign.ID(cid.Bytes)
	r.status, r.segmentVersion, r.snapshotCount = Status(status), int(segVersion), int64(count)
	if reason != nil {
		rr := RevokeReason(*reason)
		r.revokeReason = &rr
	}
	if by != nil {
		r.approvedBy = *by
	}
	for _, ch := range chs {
		r.channels = append(r.channels, segment.Channel(ch))
	}
	if err := json.Unmarshal(stats, &r.stats); err != nil {
		return row{}, fmt.Errorf("%w: approval %s: audience_stats: %w", ErrInconsistent, approvalID, err)
	}
	r.preparedAt, r.expiresAt = r.preparedAt.UTC(), r.expiresAt.UTC()
	r.approvedAt, r.closedAt = utcPtr(r.approvedAt), utcPtr(r.closedAt)
	return r, nil
}

func utcPtr(t *time.Time) *time.Time {
	if t == nil {
		return nil
	}
	u := t.UTC()
	return &u
}

// summarize builds the summary of approval r of campaign cur.
//
// For a live approval it re-reads the bound content version, re-runs the
// content review for the bound channels against the CURRENT content brief
// (offer findings are never taken from client state or from a cached
// preparation result) and re-reads the held rows: their per-channel counts
// and digest must equal the stored snapshot_count, partition and digest
// (else ErrInconsistent), and every frozen value set is trial-rendered.
// Recipient values never leave readSnapshot.
//
// A closed approval has no held rows any more; its summary carries the
// stored binding and partition only.
func summarize(ctx context.Context, tx pgx.Tx, cur campaign.Campaign, r row) (Summary, error) {
	v, err := campaign.ContentTx(ctx, tx, cur.ID, r.contentID)
	if err != nil {
		return Summary{}, err
	}
	s := Summary{
		ApprovalID: r.id, CampaignID: r.campaignID, Status: r.status, RevokeReason: r.revokeReason,
		CampaignVersion: cur.Version, SegmentVersion: r.segmentVersion,
		ContentID: v.ID, ContentVersion: v.Number, ContentProvenance: v.Provenance,
		Channels: append([]segment.Channel{}, r.channels...), AudienceStats: r.stats,
		SnapshotCount: r.snapshotCount, SnapshotDigest: r.digest,
		PreparedAt: r.preparedAt, ExpiresAt: r.expiresAt, ApprovedAt: r.approvedAt, ApprovedBy: r.approvedBy,
		ClosedAt: r.closedAt, Snapshot: map[segment.Channel]ChannelSnapshot{},
	}
	if !r.status.Live() {
		for _, ch := range r.channels {
			s.Snapshot[ch] = ChannelSnapshot{Recipients: r.stats.ByChannel[ch].Eligible}
		}
		return s, nil
	}

	// A live approval is the campaign's active approval, in the matching
	// status, bound to its current segment version, content and channels
	// (edits are refused while it is live).
	wantStatus := campaign.StatusPendingApproval
	if r.status == StatusApproved {
		wantStatus = campaign.StatusApproved
	}
	if cur.Status != wantStatus || cur.ActiveApprovalID == nil || *cur.ActiveApprovalID != r.id {
		return Summary{}, fmt.Errorf("%w: campaign %s (%s) does not point at its %s approval %s",
			ErrInconsistent, cur.ID, cur.Status, r.status, r.id)
	}
	if fields := bindingVsCampaign(cur, r); len(fields) > 0 {
		return Summary{}, fmt.Errorf("%w: live approval %s differs from campaign %s in %v", ErrInconsistent, r.id, cur.ID, fields)
	}
	review, err := cur.ReviewForApproval(v)
	if err != nil {
		return Summary{}, err
	}
	snap, digest, err := readSnapshot(ctx, tx, r.id, v.Content, r.channels)
	if err != nil {
		return Summary{}, err
	}
	var total int64
	for _, ch := range []segment.Channel{segment.ChannelSMS, segment.ChannelEmail} {
		cs := snap[ch]
		stat, selected := r.stats.ByChannel[ch]
		if !selected {
			if cs.Recipients != 0 {
				return Summary{}, fmt.Errorf("%w: approval %s has %s rows for an unselected channel", ErrInconsistent, r.id, ch)
			}
			delete(snap, ch)
			continue
		}
		if cs.Recipients != stat.Eligible {
			return Summary{}, fmt.Errorf("%w: approval %s: %d held %s rows, stored eligible %d",
				ErrInconsistent, r.id, cs.Recipients, ch, stat.Eligible)
		}
		total += cs.Recipients
	}
	if total != r.snapshotCount || digest != r.digest {
		return Summary{}, fmt.Errorf("%w: approval %s: held rows do not match the stored snapshot (count %d/%d)",
			ErrInconsistent, r.id, total, r.snapshotCount)
	}
	s.Snapshot, s.Review, s.Reviewed = snap, review, true
	return s, nil
}

// bindingVsCampaign lists the bound values of approval r that differ from
// the campaign's current ones (paths campaign.<field>).
func bindingVsCampaign(cur campaign.Campaign, r row) []string {
	var fields []string
	if r.segmentVersion != cur.SegmentVersion {
		fields = append(fields, "campaign.segment_version")
	}
	if cur.CurrentContentID == nil || *cur.CurrentContentID != r.contentID {
		fields = append(fields, "campaign.content_version")
	}
	if !slices.Equal(r.channels, cur.Channels) {
		fields = append(fields, "campaign.channels")
	}
	return fields
}

// ---- Read ----

// Get reads approval approvalID of the campaign and its summary in one
// read-only REPEATABLE READ transaction (one consistent snapshot of the
// campaign, the approval, its content and its held rows). It writes
// nothing; Service.Get performs lazy expiry first.
func (r *Repository) Get(ctx context.Context, campaignID, approvalID campaign.ID) (out Outcome, err error) {
	ctx, cancel := context.WithTimeout(ctx, TxTimeout)
	defer cancel()
	defer func() { err = classify(ctx, err) }()
	tx, err := r.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return Outcome{}, fmt.Errorf("approval: begin: %w", err)
	}
	defer func() {
		cctx, ccancel := context.WithTimeout(context.WithoutCancel(ctx), cleanupTimeout)
		defer ccancel()
		_ = tx.Rollback(cctx)
	}()
	if _, err := tx.Exec(ctx, sqlReadOnlyTimeouts); err != nil {
		return Outcome{}, fmt.Errorf("approval: set timeouts: %w", err)
	}
	cur, err := campaign.GetTx(ctx, tx, campaignID)
	if err != nil {
		return Outcome{}, err
	}
	a, err := readRow(ctx, tx, sqlSelectApproval, approvalID, campaignID)
	if err != nil {
		return Outcome{}, err
	}
	sum, err := summarize(ctx, tx, cur, a)
	if err != nil {
		return Outcome{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return Outcome{}, fmt.Errorf("approval: commit: %w", err)
	}
	return Outcome{Campaign: cur, Summary: sum}, nil
}

// ---- Shared prelude of a decision ----

// lockForDecision locks the target approval (after the campaign lock, §5.1)
// and applies lazy expiry under the lock.
//
//   - Unknown approval or one of another campaign → ErrNotFound (nothing
//     written).
//   - The campaign's live approval is due now (it became due since the
//     service's own committed lazy expiry, possibly while this transaction
//     waited for the lock): it is expired here, and decided reports how the
//     request ends — ErrExpired if it was the target, else a version
//     conflict (the campaign changed under the client). The caller must
//     COMMIT in that case: the expiry is a real, final transition and must
//     not be rolled back with the rejected request.
//   - The target was already expired → ErrExpired (nothing written).
func lockForDecision(ctx context.Context, tx pgx.Tx, cur campaign.Campaign, approvalID campaign.ID,
	expectedVersion int) (r row, decided error, err error) {
	r, err = readRow(ctx, tx, sqlSelectApprovalForUpdate, approvalID, cur.ID)
	if err != nil {
		return row{}, nil, err
	}
	after, expired, err := expireIfDue(ctx, tx, cur)
	if err != nil {
		return row{}, nil, err
	}
	if expired != nil {
		if *expired == approvalID {
			return row{}, ErrExpired, nil
		}
		return row{}, &campaign.ConflictError{Expected: expectedVersion, Current: after.Version}, nil
	}
	if r.status == StatusExpired {
		return row{}, nil, ErrExpired
	}
	return r, nil, nil
}

// inDecisionTx runs fn in a campaign-locked transaction. fn returns either
// an error (rolled back) or a decided error (committed: an expiry was
// written) or neither (committed: the decision was applied).
func (r *Repository) inDecisionTx(ctx context.Context, id campaign.ID,
	fn func(ctx context.Context, tx pgx.Tx, cur campaign.Campaign) (Outcome, error, error)) (Outcome, error) {
	var (
		out     Outcome
		decided error
	)
	err := r.inLockedTx(ctx, id, func(ctx context.Context, tx pgx.Tx, cur campaign.Campaign) error {
		o, d, err := fn(ctx, tx, cur)
		out, decided = o, d
		return err
	})
	if err != nil {
		return Outcome{}, err
	}
	if decided != nil {
		return Outcome{}, decided
	}
	return out, nil
}

// ---- Confirm ----

// ConfirmParams are the inputs of a confirmation: the approval the marketer
// reviewed, the campaign version they saw, and the binding they confirm.
type ConfirmParams struct {
	CampaignID, ApprovalID campaign.ID
	ExpectedVersion        int
	SegmentVersion         int
	ContentVersion         int
	Channels               []segment.Channel
	ApprovedTTL            time.Duration
	ApprovedBy             string
}

// Confirm runs the confirmation transaction (§5.5 "Confirm"):
//
//  1. lock the campaign row, then the approval row; lazy expiry under the
//     lock (lockForDecision: a due approval is expired and committed, the
//     request ends with ErrExpired);
//  2. the approval must not be closed or already decided (StateError);
//  3. version CAS (expected_version) and campaign state: pending_approval
//     with this approval active (campaign.CheckConfirmAt);
//  4. binding: the request's segment version, content version and channels
//     equal the approval's, and the approval's equal the campaign's
//     (*MismatchError);
//  5. re-review the bound content for the bound channels against the
//     current content brief, and verify the held snapshot against the
//     stored count, partition and digest (summarize);
//  6. CAS pending → approved with expires_at > clock_timestamp() and the
//     binding; approved_at = clock_timestamp(), expires_at = approved_at +
//     approved TTL. 0 rows: the approval became due during this
//     transaction → it is expired (committed), ErrExpired;
//  7. campaign → approved, version + 1. Held rows and content are not
//     touched; the audience is not re-evaluated.
//
// Any failure in 2–7 rolls back everything.
func (r *Repository) Confirm(ctx context.Context, p ConfirmParams) (Outcome, error) {
	if p.ApprovedTTL < MinApprovedTTL || p.ApprovedTTL > MaxApprovedTTL {
		return Outcome{}, fmt.Errorf("approval: approved TTL %s out of range", p.ApprovedTTL)
	}
	if err := validateApprover(p.ApprovedBy); err != nil {
		return Outcome{}, err
	}
	reqChannels, err := segment.NormalizeChannels(p.Channels)
	if err != nil {
		return Outcome{}, &MismatchError{Fields: []string{"channels"}}
	}
	return r.inDecisionTx(ctx, p.CampaignID, func(ctx context.Context, tx pgx.Tx, cur campaign.Campaign) (Outcome, error, error) {
		a, decided, err := lockForDecision(ctx, tx, cur, p.ApprovalID, p.ExpectedVersion)
		if err != nil || decided != nil {
			return Outcome{}, decided, err
		}
		switch a.status {
		case StatusApproved:
			return Outcome{}, nil, &campaign.StateError{Op: campaign.OpConfirmApproval, Status: cur.Status, Reason: "the approval is already confirmed"}
		case StatusConsumed:
			return Outcome{}, nil, &campaign.StateError{Op: campaign.OpConfirmApproval, Status: cur.Status, Reason: "the approval has already been used for sending"}
		case StatusRevoked:
			return Outcome{}, nil, &campaign.StateError{Op: campaign.OpConfirmApproval, Status: cur.Status, Reason: "the approval was withdrawn or replaced; review the campaign again"}
		}
		if err := cur.CheckConfirmAt(p.ExpectedVersion, p.ApprovalID); err != nil {
			return Outcome{}, nil, err
		}
		if a.status != StatusPending { // implied by the checks above
			return Outcome{}, nil, fmt.Errorf("%w: approval %s is %s", ErrInconsistent, a.id, a.status)
		}

		// Binding: what the marketer confirms = what was frozen = what the
		// campaign currently is.
		v, err := campaign.ContentTx(ctx, tx, cur.ID, a.contentID)
		if err != nil {
			return Outcome{}, nil, err
		}
		var fields []string
		if a.segmentVersion != p.SegmentVersion {
			fields = append(fields, "segment_version")
		}
		if v.Number != p.ContentVersion {
			fields = append(fields, "content_version")
		}
		if !slices.Equal(a.channels, reqChannels) {
			fields = append(fields, "channels")
		}
		fields = append(fields, bindingVsCampaign(cur, a)...)
		if len(fields) > 0 {
			return Outcome{}, nil, &MismatchError{Fields: fields}
		}

		sum, err := summarize(ctx, tx, cur, a)
		if err != nil {
			return Outcome{}, nil, err
		}

		var approvedAt, expiresAt time.Time
		err = tx.QueryRow(ctx, sqlConfirmApproval, uuidArg(a.id), uuidArg(cur.ID), p.ApprovedBy,
			p.ApprovedTTL.Microseconds(), a.segmentVersion, a.contentID, segment.ChannelNames(a.channels)).Scan(&approvedAt, &expiresAt)
		if errors.Is(err, pgx.ErrNoRows) {
			// Everything else was verified under the lock, so the only way
			// the CAS can miss is that the approval became due between the
			// lazy expiry above and this statement: expire it now (committed).
			_, expired, xerr := expireIfDue(ctx, tx, cur)
			if xerr != nil {
				return Outcome{}, nil, xerr
			}
			if expired != nil && *expired == a.id {
				return Outcome{}, ErrExpired, nil
			}
			return Outcome{}, nil, fmt.Errorf("%w: approval %s: confirm CAS matched no row", ErrInconsistent, a.id)
		}
		if err != nil {
			return Outcome{}, nil, fmt.Errorf("approval %s: confirm: %w", a.id, err)
		}

		next, err := cur.WithConfirmedApproval(p.ExpectedVersion, a.id)
		if err != nil {
			return Outcome{}, nil, err
		}
		saved, err := campaign.WriteApprovalStateTx(ctx, tx, cur, next)
		if err != nil {
			return Outcome{}, nil, err
		}
		approvedAt, expiresAt = approvedAt.UTC(), expiresAt.UTC()
		sum.Status, sum.CampaignVersion = StatusApproved, saved.Version
		sum.ApprovedAt, sum.ApprovedBy, sum.ExpiresAt = &approvedAt, p.ApprovedBy, expiresAt
		return Outcome{Campaign: saved, Summary: sum}, nil, nil
	})
}

// ---- Withdraw ----

// WithdrawParams are the inputs of a withdrawal.
type WithdrawParams struct {
	CampaignID, ApprovalID campaign.ID
	ExpectedVersion        int
}

// Withdraw runs the withdrawal transaction (§5.5 "Withdraw"): lock the
// campaign row, then the approval row; lazy expiry under the lock (an
// approval that is due is expired, never withdrawn); the approval must be
// live (a revoked one is already closed, a consumed one is never
// withdrawn); version CAS and campaign state (campaign.CheckWithdrawAt);
// approval → revoked(withdrawn) with closed_at = clock_timestamp(), its
// held rows deleted (their number must equal snapshot_count); campaign →
// content_ready, active_approval_id cleared, version + 1. Works for pending
// and approved approvals alike; nothing reopens it afterwards.
func (r *Repository) Withdraw(ctx context.Context, p WithdrawParams) (Outcome, error) {
	return r.inDecisionTx(ctx, p.CampaignID, func(ctx context.Context, tx pgx.Tx, cur campaign.Campaign) (Outcome, error, error) {
		a, decided, err := lockForDecision(ctx, tx, cur, p.ApprovalID, p.ExpectedVersion)
		if err != nil || decided != nil {
			return Outcome{}, decided, err
		}
		switch a.status {
		case StatusRevoked:
			return Outcome{}, nil, &campaign.StateError{Op: campaign.OpWithdrawApproval, Status: cur.Status, Reason: "the approval was already withdrawn or replaced"}
		case StatusConsumed:
			return Outcome{}, nil, &campaign.StateError{Op: campaign.OpWithdrawApproval, Status: cur.Status, Reason: "the approval has been used for sending and can no longer be withdrawn"}
		}
		if err := cur.CheckWithdrawAt(p.ExpectedVersion, p.ApprovalID); err != nil {
			return Outcome{}, nil, err
		}
		live, err := lockActiveApproval(ctx, tx, cur)
		if err != nil {
			return Outcome{}, nil, err
		}
		if live == nil || live.id != a.id || live.status != a.status {
			return Outcome{}, nil, fmt.Errorf("%w: approval %s is not the campaign's live approval", ErrInconsistent, a.id)
		}
		if err := closeApproval(ctx, tx, cur, live, sqlRevokeApproval, string(RevokeWithdrawn)); err != nil {
			return Outcome{}, nil, err
		}
		next, err := cur.WithApprovalClosed(a.id)
		if err != nil {
			return Outcome{}, nil, err
		}
		saved, err := campaign.WriteApprovalStateTx(ctx, tx, cur, next)
		if err != nil {
			return Outcome{}, nil, err
		}
		closed, err := readRow(ctx, tx, sqlSelectApproval, a.id, cur.ID)
		if err != nil {
			return Outcome{}, nil, err
		}
		sum, err := summarize(ctx, tx, saved, closed)
		if err != nil {
			return Outcome{}, nil, err
		}
		return Outcome{Campaign: saved, Summary: sum}, nil, nil
	})
}

// validateApprover checks APPROVER_NAME (recorded as approved_by; there is
// no authentication, §18): 1–100 runes, valid UTF-8, no control characters,
// no surrounding whitespace.
func validateApprover(name string) error {
	n := utf8.RuneCountInString(name)
	switch {
	case !utf8.ValidString(name) || n < 1 || n > MaxApproverRunes || strings.TrimSpace(name) != name ||
		strings.IndexFunc(name, unicode.IsControl) >= 0:
		return fmt.Errorf("approval: approver name must be 1–%d printable characters without surrounding spaces", MaxApproverRunes)
	}
	return nil
}
