package approval

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/fyusufayan/insider-campaign-composer-case/backend/internal/campaign"
	"github.com/fyusufayan/insider-campaign-composer-case/backend/internal/segment"
)

// CP16: the explicit start of sending (ARCHITECTURE §5.2 approved → send →
// sending, §5.3 consume, §5.5 "Send"). One short READ COMMITTED transaction
// locks the campaign row first, then the approval row, then the held
// delivery rows (§5.1); it consumes the confirmed approval and releases its
// frozen rows to the delivery queue. No I/O but the database happens while
// it is open: the worker, which makes the provider requests, only ever sees
// rows that this transaction committed.

const (
	// The consuming CAS (§5.3): approved, not expired at the moment of
	// evaluation (clock_timestamp() after the campaign lock, never now()),
	// still bound to the binding that was verified. consumed_at uses the
	// same clock. A consumed approval never expires (lazy expiry and the
	// sweeper only consider pending/approved approvals), so its old
	// expires_at cannot interrupt a send that was started in time.
	sqlConsumeApproval = `UPDATE campaign_approvals AS a
SET status = 'consumed', consumed_at = t.ts
FROM (SELECT clock_timestamp() AS ts) AS t
WHERE a.id = $1 AND a.campaign_id = $2 AND a.status = 'approved' AND a.expires_at > t.ts
  AND a.segment_version = $3 AND a.content_id = $4 AND a.channels = $5::text[]::channel[]`

	// Releases the frozen snapshot to the queue (held → pending), counted per
	// channel. next_attempt_at = now() makes the rows claimable at once;
	// destination stays NULL (resolved from the current contact at the
	// first dispatch precheck, §10.3).
	sqlReleaseHeld = `WITH released AS (
  UPDATE campaign_deliveries
  SET status = 'pending', next_attempt_at = now(), updated_at = now()
  WHERE approval_id = $1 AND campaign_id = $2 AND status = 'held'
  RETURNING channel
)
SELECT channel::text, count(*) FROM released GROUP BY channel`

	// The rows of a consumed approval per channel (all statuses): what the
	// send released, reported again by an idempotent repeat.
	sqlCountReleased = `SELECT channel::text, count(*) FROM campaign_deliveries WHERE approval_id = $1 GROUP BY channel`
)

// SendParams are the inputs of a send start: the confirmed approval the
// marketer sends and the campaign version they saw.
type SendParams struct {
	CampaignID, ApprovalID campaign.ID
	ExpectedVersion        int
}

// SendOutcome is the result of a send start.
type SendOutcome struct {
	// Campaign is the campaign as committed (sending), or as read for an
	// idempotent repeat.
	Campaign   campaign.Campaign
	ApprovalID campaign.ID
	// Released is the number of delivery rows of the consumed approval per
	// selected channel (= the approval's eligible counts).
	Released map[segment.Channel]int64
	// Started is true when this request consumed the approval; false for an
	// idempotent repeat on a campaign that is already sending (or finished)
	// with the same approval — nothing was written then.
	Started bool
}

// Send runs the send transaction (§5.5 "Send"):
//
//  1. lock the campaign row, then the approval row; lazy expiry under the
//     lock with clock_timestamp() (lockForDecision: an approval that is due
//     is expired and committed, the request ends with ErrExpired);
//  2. a consumed approval that is the campaign's active approval while the
//     campaign is sending or finished → idempotent repeat (Started=false,
//     nothing written); any other consumed, pending or revoked approval →
//     StateError;
//  3. version CAS (expected_version) and campaign state: approved with this
//     approval active (campaign.CheckSendAt);
//  4. binding: the approval's segment version, content version and channels
//     equal the campaign's current ones (*MismatchError);
//  5. re-review the bound content for the bound channels and verify the held
//     snapshot against the stored count, partition and digest (summarize;
//     ErrInconsistent on any difference);
//  6. CAS approved → consumed with expires_at > clock_timestamp() and the
//     binding; 0 rows: the approval became due during this transaction → it
//     is expired (committed), ErrExpired;
//  7. release the held rows (held → pending): per channel they must equal
//     the stored eligible counts and in total snapshot_count, else
//     ErrInconsistent;
//  8. campaign → sending, send_started_at = now(), version + 1.
//
// Any failure in 2–8 rolls back everything: the approval stays approved and
// its rows held. Two concurrent sends serialize on the campaign lock; the
// second sees the consumed approval and is an idempotent repeat (or a
// version conflict / state error), so the snapshot is released once.
func (r *Repository) Send(ctx context.Context, p SendParams) (SendOutcome, error) {
	var (
		out     SendOutcome
		decided error
	)
	err := r.inLockedTx(ctx, p.CampaignID, func(ctx context.Context, tx pgx.Tx, cur campaign.Campaign) error {
		o, d, err := sendTx(ctx, tx, cur, p)
		out, decided = o, d
		return err
	})
	if err != nil {
		return SendOutcome{}, err
	}
	if decided != nil {
		return SendOutcome{}, decided
	}
	return out, nil
}

func sendTx(ctx context.Context, tx pgx.Tx, cur campaign.Campaign, p SendParams) (SendOutcome, error, error) {
	a, decided, err := lockForDecision(ctx, tx, cur, p.ApprovalID, p.ExpectedVersion)
	if err != nil || decided != nil {
		return SendOutcome{}, decided, err
	}
	switch a.status {
	case StatusConsumed:
		active := cur.ActiveApprovalID != nil && *cur.ActiveApprovalID == a.id
		switch {
		case active && (cur.Status == campaign.StatusSending || cur.Status == campaign.StatusCompleted ||
			cur.Status == campaign.StatusCompletedWithFailures):
			released, err := countReleased(ctx, tx, a.id)
			if err != nil {
				return SendOutcome{}, nil, err
			}
			return SendOutcome{Campaign: cur, ApprovalID: a.id, Released: released}, nil, nil
		case active && cur.Status == campaign.StatusCancelled:
			return SendOutcome{}, nil, &campaign.StateError{Op: campaign.OpSend, Status: cur.Status,
				Reason: "the campaign was cancelled; its approval cannot be sent again"}
		default:
			return SendOutcome{}, nil, &campaign.StateError{Op: campaign.OpSend, Status: cur.Status,
				Reason: "the approval has already been used for sending"}
		}
	case StatusPending:
		return SendOutcome{}, nil, &campaign.StateError{Op: campaign.OpSend, Status: cur.Status,
			Reason: "the approval has not been confirmed yet"}
	case StatusRevoked:
		return SendOutcome{}, nil, &campaign.StateError{Op: campaign.OpSend, Status: cur.Status,
			Reason: "the approval was withdrawn or replaced; review the campaign again"}
	}
	if err := cur.CheckSendAt(p.ExpectedVersion, a.id); err != nil {
		return SendOutcome{}, nil, err
	}
	if a.status != StatusApproved { // implied by the checks above
		return SendOutcome{}, nil, fmt.Errorf("%w: approval %s is %s", ErrInconsistent, a.id, a.status)
	}
	if fields := bindingVsCampaign(cur, a); len(fields) > 0 {
		return SendOutcome{}, nil, &MismatchError{Fields: fields}
	}
	// Verifies the bound content (re-validated, still current, valid for the
	// bound channels) and the held rows (counts per channel, total and
	// digest equal what was approved).
	if _, err := summarize(ctx, tx, cur, a); err != nil {
		return SendOutcome{}, nil, err
	}

	tag, err := tx.Exec(ctx, sqlConsumeApproval, uuidArg(a.id), uuidArg(cur.ID), a.segmentVersion, a.contentID, segment.ChannelNames(a.channels))
	if err != nil {
		return SendOutcome{}, nil, fmt.Errorf("approval %s: consume: %w", a.id, err)
	}
	if tag.RowsAffected() != 1 {
		// Everything else was verified under the lock: the approval became
		// due between the lazy expiry above and this statement.
		_, expired, xerr := expireIfDue(ctx, tx, cur)
		if xerr != nil {
			return SendOutcome{}, nil, xerr
		}
		if expired != nil && *expired == a.id {
			return SendOutcome{}, ErrExpired, nil
		}
		return SendOutcome{}, nil, fmt.Errorf("%w: approval %s: consume CAS matched %d rows", ErrInconsistent, a.id, tag.RowsAffected())
	}

	released, err := scanChannelCounts(ctx, tx, sqlReleaseHeld, uuidArg(a.id), uuidArg(cur.ID))
	if err != nil {
		return SendOutcome{}, nil, fmt.Errorf("approval %s: release held rows: %w", a.id, err)
	}
	var total int64
	for ch, n := range released {
		if st, ok := a.stats.ByChannel[ch]; !ok || st.Eligible != n {
			return SendOutcome{}, nil, fmt.Errorf("%w: approval %s: released %d %s rows, approved %d",
				ErrInconsistent, a.id, n, ch, a.stats.ByChannel[ch].Eligible)
		}
		total += n
	}
	for _, ch := range a.channels {
		if _, ok := released[ch]; !ok && a.stats.ByChannel[ch].Eligible != 0 {
			return SendOutcome{}, nil, fmt.Errorf("%w: approval %s: no %s rows released", ErrInconsistent, a.id, ch)
		}
		if _, ok := released[ch]; !ok {
			released[ch] = 0
		}
	}
	if total != a.snapshotCount {
		return SendOutcome{}, nil, fmt.Errorf("%w: approval %s: released %d rows, snapshot_count %d",
			ErrInconsistent, a.id, total, a.snapshotCount)
	}

	next, err := cur.WithSendStarted(p.ExpectedVersion, a.id)
	if err != nil {
		return SendOutcome{}, nil, err
	}
	saved, err := campaign.WriteSendStartedTx(ctx, tx, cur, next)
	if err != nil {
		return SendOutcome{}, nil, err
	}
	return SendOutcome{Campaign: saved, ApprovalID: a.id, Released: released, Started: true}, nil, nil
}

func countReleased(ctx context.Context, tx pgx.Tx, approvalID campaign.ID) (map[segment.Channel]int64, error) {
	m, err := scanChannelCounts(ctx, tx, sqlCountReleased, uuidArg(approvalID))
	if err != nil {
		return nil, fmt.Errorf("approval %s: count released rows: %w", approvalID, err)
	}
	return m, nil
}

func scanChannelCounts(ctx context.Context, tx pgx.Tx, stmt string, args ...any) (map[segment.Channel]int64, error) {
	rows, err := tx.Query(ctx, stmt, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[segment.Channel]int64{}
	for rows.Next() {
		var (
			ch string
			n  int64
		)
		if err := rows.Scan(&ch, &n); err != nil {
			return nil, err
		}
		out[segment.Channel(ch)] = n
	}
	return out, rows.Err()
}

// Send starts sending the campaign with its confirmed approval approvalID
// (§5.5 "Send"; Repository.Send). Lazy expiry runs first in its own
// committed transaction (as for Confirm); the send transaction re-evaluates
// expiry after its own campaign lock with clock_timestamp(), so an approval
// that expires while the request waits for the lock is expired (committed)
// and never consumed. Nothing here contacts the provider: the worker
// processes the released rows asynchronously.
//
// Errors: campaign.ErrNotFound, ErrNotFound, ErrExpired, *MismatchError,
// *campaign.ConflictError, *campaign.StateError, *campaign.ValidationError
// (the bound content no longer validates), ErrBusy, ErrInconsistent.
func (s *Service) Send(ctx context.Context, campaignID, approvalID campaign.ID, expectedVersion int) (SendOutcome, error) {
	start := time.Now()
	if _, err := s.ExpireDue(ctx, campaignID); err != nil {
		return SendOutcome{}, err
	}
	out, err := s.repo.Send(ctx, SendParams{CampaignID: campaignID, ApprovalID: approvalID, ExpectedVersion: expectedVersion})
	if err != nil {
		s.logFailure(ctx, "send start failed", campaignID, err, "approval_id", approvalID.String(),
			"expected_version", expectedVersion, "duration_ms", time.Since(start).Milliseconds())
		return SendOutcome{}, err
	}
	// Identifiers and counts only.
	attrs := []any{"campaign_id", campaignID.String(), "approval_id", approvalID.String(),
		"campaign_version", out.Campaign.Version, "started", out.Started, "duration_ms", time.Since(start).Milliseconds()}
	for ch, n := range out.Released {
		attrs = append(attrs, "released_"+string(ch), n)
	}
	msg := "campaign send started"
	if !out.Started {
		msg = "campaign send repeated (already started)"
	}
	s.log.InfoContext(ctx, msg, attrs...)
	return out, nil
}
