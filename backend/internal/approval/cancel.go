package approval

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/fyusufayan/insider-campaign-composer-case/backend/internal/campaign"
	"github.com/fyusufayan/insider-campaign-composer-case/backend/internal/segment"
)

// CP17: campaign cancellation (ARCHITECTURE §5.2 cancel rows, §5.5
// "Cancel", §10.8, ADR-021). One short READ COMMITTED transaction locks the
// campaign row first, then the live approval, then delivery rows (§5.1).
// It lives in the approval package because, before sending, it closes the
// live approval and deletes its held rows — which only this package does
// (invariant 6).

// RevokeCampaignCancelled closes a live approval of a cancelled campaign.
const RevokeCampaignCancelled RevokeReason = "campaign_cancelled"

const (
	// Queued rows of the consumed approval stop here; rows in flight are
	// not touched (their precheck, completion, release or the reaper turns
	// them into cancelled, or records sent truthfully if the request had
	// already been made). outcome_uncertain is set for a row whose earlier
	// attempt was ambiguous (§10.5).
	sqlCancelQueued = `WITH cancelled AS (
  UPDATE campaign_deliveries AS d
  SET status = 'cancelled', last_error = 'campaign_cancelled',
      outcome_uncertain = d.outcome_uncertain OR EXISTS (
        SELECT 1 FROM delivery_attempts AS x WHERE x.delivery_id = d.id AND x.outcome IN ('timeout', 'unknown')),
      updated_at = now()
  WHERE d.approval_id = $1 AND d.campaign_id = $2 AND d.status IN ('pending', 'retry_scheduled')
  RETURNING d.channel
)
SELECT channel::text, count(*) FROM cancelled GROUP BY channel`

	// Rows still in flight at commit, per channel. Every later provider
	// request of this campaign comes from one of them (claim needs a
	// sending campaign; a claim racing this transaction either committed
	// before the row's update — the row is in flight and counted here — or
	// skips the row this transaction locked), and each can make at most one:
	// its precheck, retry and release all refuse a non-sending campaign.
	// A precheck that started before the commit may still pass, so "already
	// prechecked" is not a reliable split; the count is the upper bound.
	sqlInFlightRows = `SELECT channel::text, count(*)
FROM campaign_deliveries WHERE approval_id = $1 AND status = 'in_flight' GROUP BY channel`
)

// CancelOutcome is the result of a cancellation.
type CancelOutcome struct {
	// Campaign is the cancelled campaign as committed.
	Campaign campaign.Campaign
	// PreviousStatus is the status the campaign was cancelled from.
	PreviousStatus campaign.Status
	// RevokedApprovalID is the live approval closed by the cancellation
	// (pending_approval / approved), its held rows deleted.
	RevokedApprovalID *campaign.ID
	// ReleasedHeld is the number of held rows deleted with it.
	ReleasedHeld int64
	// CancelledRows counts queued delivery rows (pending / retry_scheduled)
	// that were cancelled, per channel (sending campaigns).
	CancelledRows map[segment.Channel]int64
	// InFlight counts rows that were in flight at the commit, per channel:
	// they finish truthfully. It is the upper bound on provider requests
	// that can still happen after the cancellation (at most one per row; a
	// request already on the wire cannot be recalled). Zero further
	// requests is not claimed while it is non-zero.
	InFlight map[segment.Channel]int64
}

// Cancel runs the cancellation transaction (§5.5 "Cancel"):
//
//  1. lock the campaign row; lazy expiry under the lock (a live approval
//     that is due is expired and COMMITTED; the request then ends as a
//     version conflict: the campaign changed under the client);
//  2. finished or already cancelled → *campaign.StateError (checked before
//     the version so a repeat learns why); stale expected_version →
//     *campaign.ConflictError;
//  3. by status:
//     - draft / segmented / content_ready: nothing else;
//     - pending_approval / approved: the live approval → revoked
//     (campaign_cancelled), its held rows deleted (count = snapshot_count),
//     active_approval_id cleared;
//     - sending: pending / retry_scheduled rows of the consumed approval →
//     cancelled; in_flight rows are left to the worker (§10.8);
//  4. campaign → cancelled, cancelled_at = now(), version + 1.
//
// Any failure in 2–4 rolls everything back. Concurrent cancels serialize on
// the campaign lock: the second sees cancelled → StateError. Against the
// worker: claim never takes rows of a non-sending campaign; a row the
// worker claimed before the commit is in_flight, so step 3 leaves it, and
// its precheck (which requires campaigns.status = 'sending') refuses to
// send it. Completion detection only completes sending campaigns, so a
// cancelled campaign is never completed. No lock is held across any
// provider request: the worker holds none, and this transaction makes none.
func (r *Repository) Cancel(ctx context.Context, campaignID campaign.ID, expectedVersion int) (CancelOutcome, error) {
	var (
		out     CancelOutcome
		decided error
	)
	err := r.inLockedTx(ctx, campaignID, func(ctx context.Context, tx pgx.Tx, cur campaign.Campaign) error {
		o, d, err := cancelTx(ctx, tx, cur, expectedVersion)
		out, decided = o, d
		return err
	})
	if err != nil {
		return CancelOutcome{}, err
	}
	if decided != nil {
		return CancelOutcome{}, decided
	}
	return out, nil
}

func cancelTx(ctx context.Context, tx pgx.Tx, cur campaign.Campaign, expectedVersion int) (CancelOutcome, error, error) {
	after, expired, err := expireIfDue(ctx, tx, cur)
	if err != nil {
		return CancelOutcome{}, nil, err
	}
	if expired != nil {
		return CancelOutcome{}, &campaign.ConflictError{Expected: expectedVersion, Current: after.Version}, nil
	}
	if err := cur.CheckCancelAt(expectedVersion); err != nil {
		return CancelOutcome{}, nil, err
	}
	out := CancelOutcome{PreviousStatus: cur.Status, CancelledRows: map[segment.Channel]int64{},
		InFlight: map[segment.Channel]int64{}}
	switch cur.Status {
	case campaign.StatusPendingApproval, campaign.StatusApproved:
		live, err := lockActiveApproval(ctx, tx, cur)
		if err != nil {
			return CancelOutcome{}, nil, err
		}
		if live == nil {
			return CancelOutcome{}, nil, fmt.Errorf("%w: campaign %s is %s without a live approval", ErrInconsistent, cur.ID, cur.Status)
		}
		if err := closeApproval(ctx, tx, cur, live, sqlRevokeApproval, string(RevokeCampaignCancelled)); err != nil {
			return CancelOutcome{}, nil, err
		}
		id := live.id
		out.RevokedApprovalID, out.ReleasedHeld = &id, live.snapshotCount
	case campaign.StatusSending:
		a := *cur.ActiveApprovalID
		var status string
		if err := tx.QueryRow(ctx, `SELECT status FROM campaign_approvals WHERE id = $1 AND campaign_id = $2 FOR UPDATE`,
			uuidArg(a), uuidArg(cur.ID)).Scan(&status); err != nil {
			return CancelOutcome{}, nil, fmt.Errorf("approval %s: lock: %w", a, err)
		}
		if Status(status) != StatusConsumed {
			return CancelOutcome{}, nil, fmt.Errorf("%w: sending campaign %s: approval %s is %s", ErrInconsistent, cur.ID, a, status)
		}
		out.CancelledRows, err = scanChannelCounts(ctx, tx, sqlCancelQueued, uuidArg(a), uuidArg(cur.ID))
		if err != nil {
			return CancelOutcome{}, nil, fmt.Errorf("approval %s: cancel queued rows: %w", a, err)
		}
		out.InFlight, err = scanChannelCounts(ctx, tx, sqlInFlightRows, uuidArg(a))
		if err != nil {
			return CancelOutcome{}, nil, fmt.Errorf("approval %s: in-flight rows: %w", a, err)
		}
	}
	next, err := cur.WithCancelled(expectedVersion)
	if err != nil {
		return CancelOutcome{}, nil, err
	}
	saved, err := campaign.WriteCancelledTx(ctx, tx, cur, next)
	if err != nil {
		return CancelOutcome{}, nil, err
	}
	out.Campaign = saved
	return out, nil, nil
}

// Cancel cancels the campaign the marketer saw at expectedVersion
// (Repository.Cancel). Lazy expiry runs first in its own committed
// transaction (as for every approval operation): if it expires a live
// approval the cancel is a version conflict and the client reloads.
//
// Errors: campaign.ErrNotFound, *campaign.ConflictError,
// *campaign.StateError (already cancelled / finished), ErrBusy,
// ErrInconsistent.
func (s *Service) Cancel(ctx context.Context, campaignID campaign.ID, expectedVersion int) (CancelOutcome, error) {
	start := time.Now()
	if _, err := s.ExpireDue(ctx, campaignID); err != nil {
		return CancelOutcome{}, err
	}
	out, err := s.repo.Cancel(ctx, campaignID, expectedVersion)
	if err != nil {
		s.logFailure(ctx, "campaign cancellation failed", campaignID, err, "expected_version", expectedVersion,
			"duration_ms", time.Since(start).Milliseconds())
		return CancelOutcome{}, err
	}
	attrs := []any{"campaign_id", campaignID.String(), "previous_status", string(out.PreviousStatus),
		"campaign_version", out.Campaign.Version, "duration_ms", time.Since(start).Milliseconds()}
	if out.RevokedApprovalID != nil {
		attrs = append(attrs, "approval_id", out.RevokedApprovalID.String(), "released_held_rows", out.ReleasedHeld)
	}
	for ch, n := range out.CancelledRows {
		attrs = append(attrs, "cancelled_"+string(ch), n)
	}
	for ch, n := range out.InFlight {
		attrs = append(attrs, "in_flight_"+string(ch), n)
	}
	s.log.InfoContext(ctx, "campaign cancelled", attrs...)
	return out, nil
}
