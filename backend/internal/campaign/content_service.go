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

// ContentResult is the outcome of a successful content generation.
type ContentResult struct {
	// Campaign is the committed campaign (status content_ready, pointing at
	// Version).
	Campaign Campaign
	// Version is the inserted immutable content version.
	Version content.Version
	// Validation is the content validation result for the campaign's
	// channels (statistics, warnings, rendered example).
	Validation content.Result
	// Notes are display notes of the generator (e.g. demo output).
	Notes []string
}

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
	observed, err := s.store.Get(ctx, id)
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

	start := time.Now()
	gen, err := s.generator.Generate(ctx, ai.GenerationInput{
		Brief: brief, Rule: observed.Segment, Channels: observed.Channels,
	})
	if err != nil {
		s.logGenerateFailure(ctx, id, err, time.Since(start))
		return ContentResult{}, err
	}
	s.log.InfoContext(ctx, "content generated", "campaign_id", id.String(),
		"model", gen.Meta.Model, "prompt_version", gen.Meta.PromptVersion, "llm_calls", gen.Meta.Calls,
		"repaired", gen.Meta.Repaired, "input_tokens", gen.Meta.Usage.InputTokens,
		"output_tokens", gen.Meta.Usage.OutputTokens, "channels", channelNames(gen.Channels),
		"warnings", len(gen.Validation.Warnings), "duration_ms", time.Since(start).Milliseconds())

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
	return ContentResult{Campaign: saved, Version: v, Validation: gen.Validation, Notes: gen.Notes}, nil
}

// CurrentContent returns the campaign and its current content version (nil
// if it has none). The version is read after the campaign; versions are
// immutable, so it is exactly the version the campaign pointed at.
func (s *Service) CurrentContent(ctx context.Context, id ID) (Campaign, *content.Version, error) {
	c, err := s.store.Get(ctx, id)
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

func channelNames(chs []segment.Channel) []string {
	out := make([]string, len(chs))
	for i, ch := range chs {
		out[i] = string(ch)
	}
	return out
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
