package campaign

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/fyusufayan/insider-campaign-composer-case/backend/internal/ai"
	"github.com/fyusufayan/insider-campaign-composer-case/backend/internal/content"
	"github.com/fyusufayan/insider-campaign-composer-case/backend/internal/segment"
)

// Store is the persistence port of the service. *Repository implements it
// against PostgreSQL; the in-memory implementation in campaigntest exists
// only so the service and HTTP layer can be unit-tested without Docker.
type Store interface {
	Create(ctx context.Context, d Draft) (Campaign, error)
	Get(ctx context.Context, id ID) (Campaign, error)
	Update(ctx context.Context, id ID, mutate Mutation) (Campaign, error)
	// SaveContent atomically inserts a content version decided by mutate
	// under the campaign row lock and points the campaign at it.
	SaveContent(ctx context.Context, id ID, mutate ContentMutation) (Campaign, content.Version, error)
	// GetContent reads one (re-validated) content version of a campaign.
	GetContent(ctx context.Context, id ID, contentID int64) (content.Version, error)
}

// AudienceCounter computes audience statistics. *segment.AudienceRepository
// implements it (its own short read-only transaction with a statement
// timeout); it is the only audience query of the system.
type AudienceCounter interface {
	Stats(ctx context.Context, rule segment.Validated, channels []segment.Channel) (segment.AudienceStats, error)
}

// ApprovalExpirer performs lazy approval expiry (ARCHITECTURE §5.3 (b)):
// if the campaign's live approval is due it is expired in its own committed
// transaction (campaign row lock, clock_timestamp(), held rows deleted,
// version + 1) and its id returned; otherwise nothing is written.
// *approval.Service implements it.
type ApprovalExpirer interface {
	ExpireDue(ctx context.Context, id ID) (*ID, error)
}

// Option configures optional service dependencies.
type Option func(*Service)

// WithApprovalExpirer makes every API operation on a campaign with a live
// approval run lazy expiry first (§5.3), so the API never presents or
// mutates a stale approval state. Without it (unit tests of earlier
// slices) no expiry runs.
func WithApprovalExpirer(e ApprovalExpirer) Option {
	return func(s *Service) { s.expirer = e }
}

// Service implements the campaign authoring use cases.
type Service struct {
	store       Store
	audience    AudienceCounter
	interpreter *ai.Interpreter
	generator   *ai.Generator
	expirer     ApprovalExpirer
	log         *slog.Logger
}

// NewService wires the service.
func NewService(store Store, audience AudienceCounter, interpreter *ai.Interpreter, generator *ai.Generator, log *slog.Logger, opts ...Option) (*Service, error) {
	switch {
	case store == nil:
		return nil, errors.New("campaign: nil store")
	case audience == nil:
		return nil, errors.New("campaign: nil audience counter")
	case interpreter == nil:
		return nil, errors.New("campaign: nil interpreter")
	case generator == nil:
		return nil, errors.New("campaign: nil content generator")
	case log == nil:
		return nil, errors.New("campaign: nil logger")
	}
	s := &Service{store: store, audience: audience, interpreter: interpreter, generator: generator, log: log}
	for _, o := range opts {
		o(s)
	}
	return s, nil
}

// load reads the campaign after lazy approval expiry (§5.3 (b)): when the
// campaign has a live approval (pending_approval/approved) and an expirer
// is configured, a due approval is expired in its own committed transaction
// first and the campaign re-read, so callers see — and version-check
// against — the post-expiry state. Campaigns without a live approval
// (including sending/finished ones with a consumed approval) are never
// touched.
func (s *Service) load(ctx context.Context, id ID) (Campaign, error) {
	c, err := s.store.Get(ctx, id)
	if err != nil || s.expirer == nil || (c.Status != StatusPendingApproval && c.Status != StatusApproved) {
		return c, err
	}
	expired, err := s.expirer.ExpireDue(ctx, id)
	if err != nil {
		return Campaign{}, err
	}
	if expired == nil {
		return c, nil
	}
	return s.store.Get(ctx, id)
}

// Create validates and stores a new draft campaign.
func (s *Service) Create(ctx context.Context, name, brief string) (Campaign, error) {
	d, err := NewDraft(name, brief)
	if err != nil {
		return Campaign{}, err
	}
	c, err := s.store.Create(ctx, d)
	if err != nil {
		return Campaign{}, err
	}
	s.log.InfoContext(ctx, "campaign created", "campaign_id", c.ID.String())
	return c, nil
}

// Get returns a campaign. Reads never change the campaign, except that a
// due live approval is expired first (lazy expiry, §5.3).
func (s *Service) Get(ctx context.Context, id ID) (Campaign, error) {
	return s.load(ctx, id)
}

// SetChannels replaces the channel selection if the campaign is still at
// expectedVersion (compare-and-set under the campaign row lock).
func (s *Service) SetChannels(ctx context.Context, id ID, channels []segment.Channel, expectedVersion int) (Campaign, error) {
	if _, err := s.load(ctx, id); err != nil { // lazy expiry first (committed)
		return Campaign{}, err
	}
	c, err := s.store.Update(ctx, id, func(cur Campaign) (Campaign, bool, error) {
		return cur.WithChannels(expectedVersion, channels)
	})
	if err != nil {
		return Campaign{}, err
	}
	s.log.InfoContext(ctx, "campaign channels set", "campaign_id", id.String(), "version", c.Version)
	return c, nil
}

// InterpretResult is the outcome of a successful interpretation. The
// interpretation is committed even when the audience statistics could not be
// computed afterwards; then Audience is nil and AudienceErr says why.
type InterpretResult struct {
	Campaign    Campaign
	Audience    *segment.AudienceStats
	AudienceErr error
}

// Interpret interprets the campaign's brief and persists the result.
//
//  1. Read the campaign (no lock) and remember its version.
//  2. Call the interpreter with no transaction open (invariant 13).
//  3. The interpreter returns only strictly decoded, validated and
//     normalized output (or an error; nothing is persisted then).
//  4. One short transaction locks the campaign row, re-checks status and
//     version (WithInterpretation) and writes rule, interpretation, content
//     brief, versions and, on the first interpretation only, channels.
//  5. The audience is computed from the committed row in its own read-only
//     transaction.
//
// If the campaign changed while the model was working, the result is
// discarded with a *ConflictError.
func (s *Service) Interpret(ctx context.Context, id ID) (InterpretResult, error) {
	observed, err := s.load(ctx, id)
	if err != nil {
		return InterpretResult{}, err
	}
	if err := observed.CheckInterpret(); err != nil {
		return InterpretResult{}, err
	}

	start := time.Now()
	in, err := s.interpreter.Interpret(ctx, observed.Brief)
	if err != nil {
		s.logInterpretFailure(ctx, id, err, time.Since(start))
		return InterpretResult{}, err
	}
	s.log.InfoContext(ctx, "brief interpreted", "campaign_id", id.String(),
		"model", in.Meta.Model, "prompt_version", in.Meta.PromptVersion, "llm_calls", in.Meta.Calls,
		"repaired", in.Meta.Repaired, "input_tokens", in.Meta.Usage.InputTokens,
		"output_tokens", in.Meta.Usage.OutputTokens, "duration_ms", time.Since(start).Milliseconds())

	saved, err := s.store.Update(ctx, id, func(cur Campaign) (Campaign, bool, error) {
		next, err := cur.WithInterpretation(observed.Version, in)
		return next, err == nil, err
	})
	if err != nil {
		if errors.Is(err, ErrVersionConflict) || errors.Is(err, ErrInvalidState) {
			s.log.WarnContext(ctx, "interpretation discarded: campaign changed while the model was working",
				"campaign_id", id.String(), "observed_version", observed.Version, "err", err)
		}
		return InterpretResult{}, err
	}
	s.log.InfoContext(ctx, "interpretation saved", "campaign_id", id.String(),
		"version", saved.Version, "segment_version", saved.SegmentVersion, "status", string(saved.Status))

	res := InterpretResult{Campaign: saved}
	stats, err := s.audienceStats(ctx, saved)
	if err != nil {
		if errors.Is(err, context.Canceled) {
			return InterpretResult{}, err
		}
		s.log.ErrorContext(ctx, "audience statistics after interpretation failed", "campaign_id", id.String(), "err", err)
		res.AudienceErr = err
		return res, nil
	}
	res.Audience = &stats
	return res, nil
}

// AudiencePreview is a live, informational audience preview. It is not a
// snapshot: nothing is frozen or approved by computing it.
type AudiencePreview struct {
	CampaignID      ID
	CampaignVersion int
	SegmentVersion  int
	Channels        []segment.Channel
	Stats           segment.AudienceStats
}

// Audience recomputes the audience statistics of the stored rule for the
// stored channels. Read-only.
func (s *Service) Audience(ctx context.Context, id ID) (AudiencePreview, error) {
	c, err := s.load(ctx, id)
	if err != nil {
		return AudiencePreview{}, err
	}
	if err := c.CheckAudience(); err != nil {
		return AudiencePreview{}, err
	}
	stats, err := s.audienceStats(ctx, c)
	if err != nil {
		return AudiencePreview{}, err
	}
	return AudiencePreview{
		CampaignID:      c.ID,
		CampaignVersion: c.Version,
		SegmentVersion:  c.SegmentVersion,
		Channels:        append([]segment.Channel{}, c.Channels...),
		Stats:           stats,
	}, nil
}

func (s *Service) audienceStats(ctx context.Context, c Campaign) (segment.AudienceStats, error) {
	stats, err := s.audience.Stats(ctx, c.Segment, c.Channels)
	if err == nil {
		return stats, nil
	}
	if ctx.Err() == nil && (errors.Is(err, context.DeadlineExceeded) || isQueryCanceled(err)) {
		return segment.AudienceStats{}, fmt.Errorf("%w: %w", ErrAudienceUnavailable, err)
	}
	return segment.AudienceStats{}, fmt.Errorf("campaign %s: audience: %w", c.ID, err)
}

// isQueryCanceled reports a server-side cancellation (statement_timeout).
func isQueryCanceled(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "57014"
}

func (s *Service) logInterpretFailure(ctx context.Context, id ID, err error, d time.Duration) {
	attrs := []any{"campaign_id", id.String(), "duration_ms", d.Milliseconds()}
	if e, ok := ai.AsError(err); ok {
		// Never the raw proposal or the brief: only metadata and the
		// (display-safe) cause.
		attrs = append(attrs, "code", e.Code(), "model", e.Model, "prompt_version", e.PromptVersion,
			"llm_calls", e.Calls, "retryable", e.Retryable, "details", len(e.Details))
		if e.Cause != nil {
			attrs = append(attrs, "cause", e.Cause.Error())
		}
		s.log.WarnContext(ctx, "brief interpretation failed", attrs...)
		return
	}
	s.log.InfoContext(ctx, "brief interpretation aborted", append(attrs, "err", err)...)
}
