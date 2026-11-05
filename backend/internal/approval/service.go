package approval

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/fyusufayan/insider-campaign-composer-case/backend/internal/campaign"
)

// Config configures the approval service.
type Config struct {
	// PendingTTL is how long a prepared approval may be confirmed
	// (APPROVAL_PENDING_TTL, default 15 min, ADR-006).
	PendingTTL time.Duration
}

// Validate checks the configuration.
func (c Config) Validate() error {
	if c.PendingTTL < MinPendingTTL || c.PendingTTL > MaxPendingTTL {
		return fmt.Errorf("approval: pending TTL must be in [%s, %s] (got %s)", MinPendingTTL, MaxPendingTTL, c.PendingTTL)
	}
	return nil
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
	return &Service{repo: repo, cfg: cfg, log: log}, nil
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
func (s *Service) Prepare(ctx context.Context, campaignID campaign.ID, expectedVersion int) (Summary, error) {
	start := time.Now()
	if _, err := s.ExpireDue(ctx, campaignID); err != nil {
		return Summary{}, err
	}
	sum, err := s.repo.Prepare(ctx, PrepareParams{
		CampaignID: campaignID, ExpectedVersion: expectedVersion, PendingTTL: s.cfg.PendingTTL,
	})
	if err != nil {
		s.logFailure(ctx, "approval preparation failed", campaignID, err,
			"expected_version", expectedVersion, "duration_ms", time.Since(start).Milliseconds())
		return Summary{}, err
	}
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
	return sum, nil
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
