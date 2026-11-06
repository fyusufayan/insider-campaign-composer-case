package campaign

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/fyusufayan/insider-campaign-composer-case/backend/internal/ai"
	"github.com/fyusufayan/insider-campaign-composer-case/backend/internal/content"
	"github.com/fyusufayan/insider-campaign-composer-case/backend/internal/segment"
)

// ContentResult is the outcome of a successful content generation or edit.
type ContentResult struct {
	// Campaign is the committed campaign (status content_ready, pointing at
	// Version).
	Campaign Campaign
	// Version is the current immutable content version: the inserted one,
	// or for an unchanged edit the existing one.
	Version content.Version
	// Validation is the content validation result for the campaign's
	// channels (statistics, warnings, rendered example); identical to what
	// Content returns for the committed state.
	Validation content.Result
	// Created is false for an edit whose canonical text equals the current
	// version (nothing was written; the campaign version did not move).
	Created bool
	// Notes are display notes of the generator (e.g. demo output).
	Notes []string
}

// GenerationEventKind distinguishes the events of GenerateContentStream.
type GenerationEventKind string

// Generation events, in the order they can occur.
const (
	// GenerationStarted: the state checks passed and the model is about to
	// be called (Start is set). Errors before this event are plain request
	// errors (not found, invalid state, no channels).
	GenerationStarted GenerationEventKind = "started"
	// GenerationProgress: an ai.Progress event (Progress is set). Deltas are
	// unvalidated previews.
	GenerationProgress GenerationEventKind = "progress"
	// GenerationPersisting: validated content is being written in the
	// campaign-locked transaction.
	GenerationPersisting GenerationEventKind = "persisting"
)

// GenerationStart describes the campaign state a generation is based on.
type GenerationStart struct {
	CampaignID      ID
	CampaignVersion int
	Channels        []segment.Channel
	PromptVersion   string
}

// GenerationEvent is one event of GenerateContentStream.
type GenerationEvent struct {
	Kind     GenerationEventKind
	Start    GenerationStart
	Progress ai.Progress
}

// GenerationObserver receives generation events synchronously and in order.
// Returning an error stops the generation (no model call or write follows)
// and GenerateContentStream returns it unchanged.
type GenerationObserver func(GenerationEvent) error

// GenerateContent generates SMS/email templates for the campaign's selected
// channels and stores them as a new immutable content version.
//
//  1. Read the campaign (no lock); check the state machine
//     (CheckGenerateContent) and remember version and channels.
//  2. Re-validate the stored content brief (never trusted blindly).
//  3. Call the generator with no transaction open (invariant 13). Its input
//     is the content brief, the normalized rule and the channels only — no
//     customer data. It returns only strictly decoded, validated content
//     (content.Parse + offer policy, at most one repair turn) or an error;
//     nothing is persisted then.
//  4. One short transaction locks the campaign row, re-checks status,
//     version and channels (WithAIContent), inserts the version and moves
//     current_content_id, status (content_ready) and version together.
//
// If the campaign changed while the model was working, the result is
// discarded with a *ConflictError and no row is inserted.
func (s *Service) GenerateContent(ctx context.Context, id ID) (ContentResult, error) {
	return s.GenerateContentStream(ctx, id, nil)
}

// GenerateContentStream is GenerateContent with events reported to observe
// (nil = none): started (after the state checks), the generator's progress,
// and persisting (before the transaction). Success is returned only after
// the transaction committed.
func (s *Service) GenerateContentStream(ctx context.Context, id ID, observe GenerationObserver) (ContentResult, error) {
	if observe == nil {
		observe = func(GenerationEvent) error { return nil }
	}
	observed, err := s.load(ctx, id)
	if err != nil {
		return ContentResult{}, err
	}
	if err := observed.CheckGenerateContent(); err != nil {
		return ContentResult{}, err
	}
	brief, problems := ai.ParseContentBrief(observed.ContentBrief)
	if len(problems) > 0 {
		return ContentResult{}, fmt.Errorf("campaign %s: stored content brief is invalid: %w", id, problems)
	}

	if err := observe(GenerationEvent{Kind: GenerationStarted, Start: GenerationStart{
		CampaignID: observed.ID, CampaignVersion: observed.Version,
		Channels: append([]segment.Channel(nil), observed.Channels...), PromptVersion: ai.ContentPromptVersion,
	}}); err != nil {
		return ContentResult{}, err
	}

	start := time.Now()
	gen, err := s.generator.GenerateStream(ctx, ai.GenerationInput{
		Brief: brief, Rule: observed.Segment, Channels: observed.Channels,
	}, func(p ai.Progress) error {
		return observe(GenerationEvent{Kind: GenerationProgress, Progress: p})
	})
	if err != nil {
		s.logGenerateFailure(ctx, id, err, time.Since(start))
		return ContentResult{}, err
	}
	s.log.InfoContext(ctx, "content generated", "campaign_id", id.String(),
		"model", gen.Meta.Model, "prompt_version", gen.Meta.PromptVersion, "llm_calls", gen.Meta.Calls,
		"repaired", gen.Meta.Repaired, "input_tokens", gen.Meta.Usage.InputTokens,
		"output_tokens", gen.Meta.Usage.OutputTokens, "channels", segment.ChannelNames(gen.Channels),
		"warnings", len(gen.Validation.Warnings), "duration_ms", time.Since(start).Milliseconds())

	if err := observe(GenerationEvent{Kind: GenerationPersisting}); err != nil {
		return ContentResult{}, err
	}
	prov := content.Provenance{Origin: content.OriginAI, Model: gen.Meta.Model, PromptVersion: gen.Meta.PromptVersion}
	saved, v, err := s.store.SaveContent(ctx, id, func(cur Campaign, current *content.Version) (Campaign, content.Version, bool, error) {
		next, v, err := cur.WithAIContent(observed.Version, observed.Channels, current, gen.Content, prov)
		return next, v, err == nil, err
	})
	if err != nil {
		if errors.Is(err, ErrVersionConflict) || errors.Is(err, ErrInvalidState) || errors.Is(err, ErrInvalidInput) {
			s.log.WarnContext(ctx, "generated content discarded: campaign changed while the model was working",
				"campaign_id", id.String(), "observed_version", observed.Version, "err", err)
		}
		return ContentResult{}, err
	}
	s.log.InfoContext(ctx, "content version saved", "campaign_id", id.String(), "content_id", v.ID,
		"content_version", v.Number, "version", saved.Version, "status", string(saved.Status))
	return ContentResult{Campaign: saved, Version: v, Validation: saved.ValidateStored(v), Created: true, Notes: gen.Notes}, nil
}

// ContentView is the current content of a campaign: the version it points
// at (nil if none) and its validation for the current channel selection.
type ContentView struct {
	Campaign   Campaign
	Version    *content.Version
	Validation *content.Result
}

// Content returns the campaign's current content version (re-validated when
// read) with its validation for the selected channels. Read-only; allowed in
// every status. A campaign without content has Version and Validation nil.
func (s *Service) Content(ctx context.Context, id ID) (ContentView, error) {
	c, v, err := s.CurrentContent(ctx, id)
	if err != nil {
		return ContentView{}, err
	}
	view := ContentView{Campaign: c, Version: v}
	if v != nil {
		res := c.ValidateStored(*v)
		view.Validation = &res
	}
	return view, nil
}

// DraftValidation is the read-only validation of a draft against a campaign
// state (CampaignVersion and Channels identify it).
type DraftValidation struct {
	CampaignID      ID
	CampaignVersion int
	Channels        []segment.Channel
	Result          content.Result
}

// ValidateContent validates a draft for the campaign's selected channels
// without saving anything (debounced editor validation). Content problems
// are part of the result, not an error.
func (s *Service) ValidateContent(ctx context.Context, id ID, d content.Draft) (DraftValidation, error) {
	c, err := s.load(ctx, id)
	if err != nil {
		return DraftValidation{}, err
	}
	if err := c.CheckValidateContent(); err != nil {
		return DraftValidation{}, err
	}
	_, res := c.ValidateDraft(d)
	return DraftValidation{CampaignID: c.ID, CampaignVersion: c.Version,
		Channels: append([]segment.Channel{}, c.Channels...), Result: res}, nil
}

// EditContent saves marketer-edited content on top of expectedVersion: one
// short transaction locks the campaign row, re-checks version and status,
// validates the whole draft for the selected channels and inserts a new
// immutable version (origin edited) — or, if the canonical text equals the
// current version, writes nothing and returns the current version with
// Created = false. A stale expectedVersion is a *ConflictError; invalid
// content a *ValidationError. No approval is touched (pending_approval and
// approved are refused until Slice 3).
func (s *Service) EditContent(ctx context.Context, id ID, d content.Draft, expectedVersion int) (ContentResult, error) {
	if _, err := s.load(ctx, id); err != nil { // lazy expiry first (committed)
		return ContentResult{}, err
	}
	created := false
	saved, v, err := s.store.SaveContent(ctx, id, func(cur Campaign, current *content.Version) (Campaign, content.Version, bool, error) {
		next, v, ok, err := cur.WithEditedContent(expectedVersion, current, d)
		created = ok
		return next, v, ok, err
	})
	if err != nil {
		return ContentResult{}, err
	}
	if created {
		s.log.InfoContext(ctx, "content version saved", "campaign_id", id.String(), "content_id", v.ID,
			"content_version", v.Number, "origin", string(v.Provenance.Origin), "version", saved.Version)
	} else {
		s.log.InfoContext(ctx, "content unchanged; no version created", "campaign_id", id.String(),
			"content_id", v.ID, "content_version", v.Number, "version", saved.Version)
	}
	return ContentResult{Campaign: saved, Version: v, Validation: saved.ValidateStored(v), Created: created}, nil
}

// CurrentContent returns the campaign and its current content version (nil
// if it has none). The version is read after the campaign; versions are
// immutable, so it is exactly the version the campaign pointed at.
func (s *Service) CurrentContent(ctx context.Context, id ID) (Campaign, *content.Version, error) {
	c, err := s.load(ctx, id)
	if err != nil {
		return Campaign{}, nil, err
	}
	if c.CurrentContentID == nil {
		return c, nil, nil
	}
	v, err := s.store.GetContent(ctx, id, *c.CurrentContentID)
	if err != nil {
		return Campaign{}, nil, err
	}
	return c, &v, nil
}

func (s *Service) logGenerateFailure(ctx context.Context, id ID, err error, d time.Duration) {
	attrs := []any{"campaign_id", id.String(), "duration_ms", d.Milliseconds()}
	if e, ok := ai.AsError(err); ok {
		// Never the raw proposal, the content brief or generated text: only
		// metadata and the (display-safe) cause.
		attrs = append(attrs, "code", e.Code(), "model", e.Model, "prompt_version", e.PromptVersion,
			"llm_calls", e.Calls, "retryable", e.Retryable, "details", len(e.Details))
		if e.Cause != nil {
			attrs = append(attrs, "cause", e.Cause.Error())
		}
		s.log.WarnContext(ctx, "content generation failed", attrs...)
		return
	}
	s.log.InfoContext(ctx, "content generation aborted", append(attrs, "err", err)...)
}
