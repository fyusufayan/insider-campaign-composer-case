package approval

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/fyusufayan/insider-campaign-composer-case/backend/internal/campaign"
	"github.com/fyusufayan/insider-campaign-composer-case/backend/internal/segment"
)

// DefaultApprover is APPROVER_NAME's default (§16; no authentication).
const DefaultApprover = "marketer@demo"

// Config configures the approval service.
type Config struct {
	// PendingTTL is how long a prepared approval may be confirmed
	// (APPROVAL_PENDING_TTL, default 15 min, ADR-006).
	PendingTTL time.Duration
	// ApprovedTTL is how long a confirmed approval may be consumed, counted
	// from approved_at (APPROVAL_APPROVED_TTL, default 24 h, §5.3). Zero
	// means DefaultApprovedTTL.
	ApprovedTTL time.Duration
	// ApprovedBy is recorded as campaign_approvals.approved_by
	// (APPROVER_NAME). Empty means DefaultApprover.
	ApprovedBy string
}

// withDefaults fills the zero optional fields.
func (c Config) withDefaults() Config {
	if c.ApprovedTTL == 0 {
		c.ApprovedTTL = DefaultApprovedTTL
	}
	if c.ApprovedBy == "" {
		c.ApprovedBy = DefaultApprover
	}
	return c
}

// Validate checks the configuration (after defaults).
func (c Config) Validate() error {
	c = c.withDefaults()
	if c.PendingTTL < MinPendingTTL || c.PendingTTL > MaxPendingTTL {
		return fmt.Errorf("approval: pending TTL must be in [%s, %s] (got %s)", MinPendingTTL, MaxPendingTTL, c.PendingTTL)
	}
	if c.ApprovedTTL < MinApprovedTTL || c.ApprovedTTL > MaxApprovedTTL {
		return fmt.Errorf("approval: approved TTL must be in [%s, %s] (got %s)", MinApprovedTTL, MaxApprovedTTL, c.ApprovedTTL)
	}
	return validateApprover(c.ApprovedBy)
}

// Service implements the approval use cases. It has no LLM dependency.
type Service struct {
	repo *Repository
	cfg  Config
	log  *slog.Logger
}

// NewService wires the service.
func NewService(repo *Repository, cfg Config, log *slog.Logger) (*Service, error) {
	switch {
	case repo == nil:
		return nil, errors.New("approval: nil repository")
	case log == nil:
		return nil, errors.New("approval: nil logger")
	}
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	return &Service{repo: repo, cfg: cfg.withDefaults(), log: log}, nil
}

// Prepare freezes the audience of the campaign as reviewed at
// expectedVersion into a pending approval.
//
// It first performs lazy expiry (§5.3) in its own committed transaction:
// expiry is a campaign state change that advances the version, so when the
// marketer's view predates it the preparation below fails with a version
// conflict — and the expiry must already be committed, otherwise the
// rollback would undo it and the client could never observe the new state.
// The preparation transaction (Repository.Prepare) re-checks expiry after
// its own lock; an approval that became due in between makes it a conflict
// as well (rolled back; the next request's lazy expiry commits it).
//
// Errors: campaign.ErrNotFound, *campaign.ConflictError (stale version, including a
// concurrent preparation that committed first), *campaign.StateError,
// *campaign.ValidationError (no channel, content not valid for the
// selection), *EmptyAudienceError, ErrAudienceChanged and ErrBusy
// (retryable, nothing written).
func (s *Service) Prepare(ctx context.Context, campaignID campaign.ID, expectedVersion int) (Outcome, error) {
	start := time.Now()
	if _, err := s.ExpireDue(ctx, campaignID); err != nil {
		return Outcome{}, err
	}
	out, err := s.repo.Prepare(ctx, PrepareParams{
		CampaignID: campaignID, ExpectedVersion: expectedVersion, PendingTTL: s.cfg.PendingTTL,
	})
	if err != nil {
		s.logFailure(ctx, "approval preparation failed", campaignID, err,
			"expected_version", expectedVersion, "duration_ms", time.Since(start).Milliseconds())
		return Outcome{}, err
	}
	sum := out.Summary
	// Identifiers and counts only: never recipient values or content text.
	attrs := []any{
		"campaign_id", campaignID.String(), "approval_id", sum.ApprovalID.String(),
		"campaign_version", sum.CampaignVersion, "segment_version", sum.SegmentVersion,
		"content_id", sum.ContentID, "snapshot_count", sum.SnapshotCount,
		"matched_users", sum.AudienceStats.MatchedUsers, "offer_findings", len(sum.Review.OfferFindings),
		"render_issues", sum.RenderIssueCount(), "expires_at", sum.ExpiresAt,
		"duration_ms", time.Since(start).Milliseconds(),
	}
	for ch, cs := range sum.Snapshot {
		attrs = append(attrs, "recipients_"+string(ch), cs.Recipients)
	}
	if sum.Superseded != nil {
		attrs = append(attrs, "superseded_approval_id", sum.Superseded.String())
	}
	s.log.InfoContext(ctx, "approval prepared", attrs...)
	return out, nil
}

// Get returns the campaign and the summary of its approval approvalID
// (any status; review and render issues only for a live approval). Lazy
// expiry runs first, in its own committed transaction, so an approval that
// is due is reported as expired, never as pending/approved.
//
// Errors: campaign.ErrNotFound, ErrNotFound, ErrBusy, ErrInconsistent.
func (s *Service) Get(ctx context.Context, campaignID, approvalID campaign.ID) (Outcome, error) {
	if _, err := s.ExpireDue(ctx, campaignID); err != nil {
		return Outcome{}, err
	}
	out, err := s.repo.Get(ctx, campaignID, approvalID)
	if err != nil {
		s.logFailure(ctx, "approval read failed", campaignID, err, "approval_id", approvalID.String())
		return Outcome{}, err
	}
	return out, nil
}

// ConfirmRequest is what the marketer confirms: the approval they reviewed,
// the campaign version they saw and the binding shown to them.
type ConfirmRequest struct {
	ExpectedVersion int
	SegmentVersion  int
	ContentVersion  int
	Channels        []segment.Channel
}

// Confirm promotes the pending approval approvalID to approved (§5.5
// "Confirm"; Repository.Confirm). Lazy expiry runs first in its own
// committed transaction (as for Prepare); the confirmation transaction
// re-evaluates expiry after its own campaign lock with clock_timestamp(),
// so an approval that expires while the request waits for the lock is
// expired (committed) and never confirmed.
//
// Errors: campaign.ErrNotFound, ErrNotFound, ErrExpired, *MismatchError,
// *campaign.ConflictError, *campaign.StateError, *campaign.ValidationError
// (the bound content no longer validates), ErrBusy, ErrInconsistent.
func (s *Service) Confirm(ctx context.Context, campaignID, approvalID campaign.ID, req ConfirmRequest) (Outcome, error) {
	start := time.Now()
	if _, err := s.ExpireDue(ctx, campaignID); err != nil {
		return Outcome{}, err
	}
	out, err := s.repo.Confirm(ctx, ConfirmParams{
		CampaignID: campaignID, ApprovalID: approvalID, ExpectedVersion: req.ExpectedVersion,
		SegmentVersion: req.SegmentVersion, ContentVersion: req.ContentVersion, Channels: req.Channels,
		ApprovedTTL: s.cfg.ApprovedTTL, ApprovedBy: s.cfg.ApprovedBy,
	})
	if err != nil {
		s.logFailure(ctx, "approval confirmation failed", campaignID, err, "approval_id", approvalID.String(),
			"expected_version", req.ExpectedVersion, "duration_ms", time.Since(start).Milliseconds())
		return Outcome{}, err
	}
	sum := out.Summary
	s.log.InfoContext(ctx, "approval confirmed", "campaign_id", campaignID.String(), "approval_id", approvalID.String(),
		"campaign_version", sum.CampaignVersion, "segment_version", sum.SegmentVersion, "content_id", sum.ContentID,
		"snapshot_count", sum.SnapshotCount, "offer_findings", len(sum.Review.OfferFindings),
		"render_issues", sum.RenderIssueCount(), "expires_at", sum.ExpiresAt, "duration_ms", time.Since(start).Milliseconds())
	return out, nil
}

// Withdraw revokes the live (pending or approved) approval approvalID at
// the marketer's explicit request (§5.5 "Withdraw"; Repository.Withdraw):
// its held rows are deleted and the campaign returns to content_ready. A
// due approval is expired instead (ErrExpired); a consumed one is never
// withdrawn.
//
// Errors: campaign.ErrNotFound, ErrNotFound, ErrExpired,
// *campaign.ConflictError, *campaign.StateError, ErrBusy, ErrInconsistent.
func (s *Service) Withdraw(ctx context.Context, campaignID, approvalID campaign.ID, expectedVersion int) (Outcome, error) {
	start := time.Now()
	if _, err := s.ExpireDue(ctx, campaignID); err != nil {
		return Outcome{}, err
	}
	out, err := s.repo.Withdraw(ctx, WithdrawParams{CampaignID: campaignID, ApprovalID: approvalID, ExpectedVersion: expectedVersion})
	if err != nil {
		s.logFailure(ctx, "approval withdrawal failed", campaignID, err, "approval_id", approvalID.String(),
			"expected_version", expectedVersion, "duration_ms", time.Since(start).Milliseconds())
		return Outcome{}, err
	}
	s.log.InfoContext(ctx, "approval withdrawn", "campaign_id", campaignID.String(), "approval_id", approvalID.String(),
		"campaign_version", out.Campaign.Version, "released_held_rows", out.Summary.SnapshotCount,
		"duration_ms", time.Since(start).Milliseconds())
	return out, nil
}

// ExpireDue performs the lazy expiry transition for the campaign (§5.3):
// it returns the expired approval's id, or nil if no live approval was due.
func (s *Service) ExpireDue(ctx context.Context, campaignID campaign.ID) (*campaign.ID, error) {
	id, err := s.repo.ExpireDue(ctx, campaignID)
	if err != nil {
		s.logFailure(ctx, "approval expiry failed", campaignID, err)
		return nil, err
	}
	if id != nil {
		s.log.InfoContext(ctx, "approval expired", "campaign_id", campaignID.String(), "approval_id", id.String())
	}
	return id, nil
}

func (s *Service) logFailure(ctx context.Context, msg string, campaignID campaign.ID, err error, attrs ...any) {
	attrs = append([]any{"campaign_id", campaignID.String(), "err", err}, attrs...)
	switch {
	case errors.Is(err, context.Canceled):
		s.log.InfoContext(ctx, msg, attrs...)
	case errors.Is(err, ErrInconsistent):
		s.log.ErrorContext(ctx, msg, attrs...)
	default:
		s.log.WarnContext(ctx, msg, attrs...)
	}
}
