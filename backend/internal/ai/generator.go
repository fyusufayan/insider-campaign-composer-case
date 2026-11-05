package ai

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/fyusufayan/insider-campaign-composer-case/backend/internal/content"
	"github.com/fyusufayan/insider-campaign-composer-case/backend/internal/llm"
	"github.com/fyusufayan/insider-campaign-composer-case/backend/internal/segment"
)

// maxContentOutputBytes bounds the accepted model output. The largest valid
// answer is three strings of at most 5,000 + 160 + 80 runes (≤ 4 bytes each,
// ≤ 6 bytes when \u-escaped) plus the object syntax.
const maxContentOutputBytes = 64 << 10

// GeneratorConfig is the model configuration of content generation
// (CLAUDE_CONTENT_*); the model name comes only from configuration.
type GeneratorConfig struct {
	Model     string
	Effort    llm.Effort
	Timeout   time.Duration
	MaxTokens int
}

// Input errors of Generate. They are caller (programming) errors: the
// campaign service checks the same conditions first, and no model call is
// made.
var (
	// ErrNoChannels: content is generated only for selected channels, and
	// none is selected.
	ErrNoChannels = errors.New("ai: content generation requires at least one selected channel")
	// ErrInvalidGenerationInput: the content brief or rule is missing or
	// invalid.
	ErrInvalidGenerationInput = errors.New("ai: invalid content generation input")
)

// DemoContentNotice is reported in Generation.Notes for demo output, so demo
// copy is identified as such wherever it is shown (the persisted model is
// llm.DemoModel as well).
const DemoContentNotice = "Demo content generator (fixed templates, not Claude) produced this content."

// GenerationInput is everything the model sees. It deliberately has no
// field for customer data: no names, contacts, records or delivery rows can
// reach the prompt.
type GenerationInput struct {
	// Brief is the validated content brief (campaigns.content_brief).
	Brief ContentBrief
	// Rule is the validated, normalized segment rule (context only).
	Rule segment.Validated
	// Channels is the marketer's selection; content is generated for
	// exactly these channels.
	Channels []segment.Channel
}

// Generation is validated generated content.
type Generation struct {
	// Content passed content.Parse for Channels and the offer policy. It has
	// content for exactly the selected channels.
	Content content.Content
	// Validation is content.Parse's result (statistics, rendered example)
	// with the offer-policy warnings appended to Warnings.
	Validation content.Result
	// Channels is the normalized selection the content was generated for.
	Channels []segment.Channel
	// Notes are display notes about the generation (e.g. DemoContentNotice).
	Notes []string
	Meta  Metadata
}

// Progress reporting (ARCHITECTURE §7.6, §12). Progress events describe the
// workflow and carry an *unvalidated* live preview of the model's text; they
// are never content. Only the Generation returned by GenerateStream is
// validated.

// ProgressKind distinguishes progress events.
type ProgressKind string

// Progress kinds.
const (
	// ProgressAttempt: a model request starts (Attempt, Reason). A new
	// attempt discards every delta of the previous one.
	ProgressAttempt ProgressKind = "attempt"
	// ProgressThinking: the model is reasoning before it writes output.
	ProgressThinking ProgressKind = "thinking"
	// ProgressDelta: the next fragment (Text) of field Field of the current
	// attempt, extracted from the streamed JSON. Untrusted and unvalidated.
	ProgressDelta ProgressKind = "delta"
	// ProgressValidating: the attempt finished; its complete output is being
	// decoded and validated.
	ProgressValidating ProgressKind = "validating"
	// ProgressRepair: the output failed validation (Problems = number of
	// field errors); a repair attempt follows.
	ProgressRepair ProgressKind = "repair"
)

// AttemptReason says why a model request is made.
type AttemptReason string

// Attempt reasons.
const (
	AttemptInitial         AttemptReason = "initial"
	AttemptTruncationRetry AttemptReason = "truncation_retry"
	AttemptRepair          AttemptReason = "repair"
)

// Progress is one progress event. Attempt counts model requests from 1.
type Progress struct {
	Kind     ProgressKind
	Attempt  int
	Reason   AttemptReason
	Field    string
	Text     string
	Problems int
}

// ProgressFunc receives progress events synchronously, in order. If it
// returns an error, generation stops and GenerateStream returns that error
// unchanged (no further model call is made).
type ProgressFunc func(Progress) error

// Generator writes SMS/email templates for a campaign:
// prompt → llm.Client.Stream → stop-reason check → strict decode →
// content.Parse + offer policy → at most one repair turn → result.
// Streamed text is extracted per field only for progress reporting; the
// validated result is computed from the complete text of a finished request.
// It performs no database access.
type Generator struct {
	client llm.Client
	cfg    GeneratorConfig
}

// NewGenerator validates cfg and returns a Generator.
func NewGenerator(client llm.Client, cfg GeneratorConfig) (*Generator, error) {
	switch {
	case client == nil:
		return nil, errors.New("ai: nil llm client")
	case strings.TrimSpace(cfg.Model) == "":
		return nil, errors.New("ai: generator model is required")
	case cfg.Timeout <= 0:
		return nil, errors.New("ai: generator timeout must be positive")
	case cfg.MaxTokens <= 0:
		return nil, errors.New("ai: generator max tokens must be positive")
	}
	return &Generator{client: client, cfg: cfg}, nil
}

// Model returns the configured model (for logging by callers).
func (g *Generator) Model() string { return g.cfg.Model }

// Generate generates content for in.Channels. Errors are ErrNoChannels /
// ErrInvalidGenerationInput (no model call), *Error values (see errors.go),
// or, when ctx was cancelled by the caller, an error wrapping ctx.Err().
func (g *Generator) Generate(ctx context.Context, in GenerationInput) (Generation, error) {
	return g.GenerateStream(ctx, in, nil)
}

// GenerateStream is Generate with progress events reported to onProgress
// (nil = none). Requests are always streamed.
func (g *Generator) GenerateStream(ctx context.Context, in GenerationInput, onProgress ProgressFunc) (Generation, error) {
	chs, err := segment.NormalizeChannels(in.Channels)
	switch {
	case err != nil:
		return Generation{}, fmt.Errorf("%w: %w", ErrInvalidGenerationInput, err)
	case len(chs) == 0:
		return Generation{}, ErrNoChannels
	case in.Rule.IsZero():
		return Generation{}, fmt.Errorf("%w: no segment rule", ErrInvalidGenerationInput)
	case !containsStr(lengthValues, string(in.Brief.Length)) || !containsStr(offerKindValues, string(in.Brief.Offer.Kind)):
		return Generation{}, fmt.Errorf("%w: content brief", ErrInvalidGenerationInput)
	}
	in.Channels = chs
	fields := contentFields(chs)
	first, err := contentUserMessage(in, fields)
	if err != nil {
		return Generation{}, fmt.Errorf("%w: %w", ErrInvalidGenerationInput, err)
	}

	callCtx, cancel := context.WithTimeout(ctx, g.cfg.Timeout)
	defer cancel()

	report := onProgress
	if report == nil {
		report = func(Progress) error { return nil }
	}
	mc := modelCall{
		client: g.client, task: TaskContentGeneration, system: contentSystemPrompt,
		promptVersion: ContentPromptVersion, model: g.cfg.Model, effort: g.cfg.Effort, schema: ContentSchema(chs),
		stream: report, streamFields: fields, streamLimit: maxContentOutputBytes,
	}
	st := &attempt{maxTokens: g.cfg.MaxTokens, model: g.cfg.Model}
	messages := []llm.Message{{Role: llm.RoleUser, Content: first}}

	text, err := mc.do(ctx, callCtx, st, messages, AttemptInitial)
	if err != nil {
		return Generation{}, err
	}
	if err := report(Progress{Kind: ProgressValidating, Attempt: st.calls}); err != nil {
		return Generation{}, err
	}
	c, res, errs := ParseGeneratedContent([]byte(text), chs, in.Brief.Offer)
	if len(errs) == 0 {
		return g.done(c, res, chs, st, false), nil
	}
	if err := report(Progress{Kind: ProgressRepair, Attempt: st.calls, Problems: len(errs)}); err != nil {
		return Generation{}, err
	}

	// One repair turn: append-only history (the invalid proposal as the
	// assistant turn, then the structured errors as a user turn).
	messages = append(messages,
		llm.Message{Role: llm.RoleAssistant, Content: text},
		llm.Message{Role: llm.RoleUser, Content: contentRepairMessage(errs, fields)},
	)
	text, err = mc.do(ctx, callCtx, st, messages, AttemptRepair)
	if err != nil {
		return Generation{}, err
	}
	if err := report(Progress{Kind: ProgressValidating, Attempt: st.calls}); err != nil {
		return Generation{}, err
	}
	c, res, errs = ParseGeneratedContent([]byte(text), chs, in.Brief.Offer)
	if len(errs) == 0 {
		return g.done(c, res, chs, st, true), nil
	}
	return Generation{}, mc.fail(st, &Error{Kind: ErrOutputInvalid, Details: errs, RawProposal: boundRaw(text)})
}

func (g *Generator) done(c content.Content, res content.Result, chs []segment.Channel, st *attempt, repaired bool) Generation {
	gen := Generation{
		Content:    c,
		Validation: res,
		Channels:   append([]segment.Channel(nil), chs...),
		Meta: Metadata{
			Model:         st.model,
			PromptVersion: ContentPromptVersion,
			Calls:         st.calls,
			Repaired:      repaired,
			Usage:         st.usage,
		},
	}
	if st.model == llm.DemoModel {
		gen.Notes = []string{DemoContentNotice}
	}
	return gen
}

// ParseGeneratedContent is the full pipeline for one model output:
//
//  1. bounded strict decode: one JSON object, valid UTF-8, no duplicate
//     properties, no properties other than the selected channels' fields,
//     every selected field present as a string (null or another type is an
//     error);
//  2. content.Parse for the selected channels (the single content validator:
//     NFC, characters, markup, placeholders, worst-case lengths);
//  3. the offer policy against the content brief's offer.
//
// All problems of steps 2 and 3 are reported together (the repair turn sees
// all of them). Errors is non-empty iff the output is unusable; on success
// the content has exactly the selected channels.
func ParseGeneratedContent(data []byte, chs []segment.Channel, o Offer) (content.Content, content.Result, segment.Errors) {
	if fe := scanJSONLimit(data, maxContentOutputBytes); fe != nil {
		return content.Content{}, content.Result{}, segment.Errors{*fe}
	}
	fields := contentFields(chs)
	var errs segment.Errors
	root := object(&errs, "", data, fields...)
	if root == nil {
		return content.Content{}, content.Result{}, errs
	}
	var d content.Draft
	var policy []offerField
	for _, f := range fields {
		s, ok := str(&errs, f, root.get(f, true))
		if !ok {
			continue
		}
		policy = append(policy, offerField{path: f, text: s})
		switch f {
		case content.FieldSMSBody:
			d.SMSBody = &s
		case content.FieldEmailSubject:
			d.EmailSubject = &s
		case content.FieldEmailBody:
			d.EmailBody = &s
		}
	}
	if len(errs) > 0 {
		return content.Content{}, content.Result{}, errs
	}

	c, res := content.Parse(d, chs)
	errs = append(errs, res.Errors...)
	perrs, warns := checkOfferPolicy(policy, o)
	errs = append(errs, perrs...)
	if len(errs) > 0 {
		return content.Content{}, content.Result{}, errs
	}
	for _, ch := range chs { // guaranteed by Parse for a selected channel
		if !c.Has(ch) {
			return content.Content{}, content.Result{}, segment.Errors{{Path: "", Code: segment.CodeRequired,
				Message: "content for a selected channel is missing"}}
		}
	}
	if len(c.Channels()) != len(chs) { // unreachable: only selected fields are accepted
		return content.Content{}, content.Result{}, segment.Errors{{Path: "", Code: segment.CodeNotAllowed,
			Message: "content for a channel that is not selected"}}
	}
	res.Warnings = append(res.Warnings, warns...)
	return c, res, nil
}
