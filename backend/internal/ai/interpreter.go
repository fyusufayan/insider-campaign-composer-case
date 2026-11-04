package ai

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"golang.org/x/text/unicode/norm"

	"github.com/fyusufayan/insider-campaign-composer-case/backend/internal/llm"
	"github.com/fyusufayan/insider-campaign-composer-case/backend/internal/segment"
)

// Brief limits; identical to the campaigns.brief CHECK constraint (§4).
const (
	MinBriefRunes = 10
	MaxBriefRunes = 2000
)

// maxRawProposalBytes bounds the rejected output kept in an *Error.
const maxRawProposalBytes = 16 << 10

// InterpreterConfig is the model configuration of interpretation
// (CLAUDE_SEGMENT_*); the model name comes only from configuration.
type InterpreterConfig struct {
	Model     string
	Effort    llm.Effort
	Timeout   time.Duration
	MaxTokens int
}

// Interpreter turns a brief into a validated Interpretation:
// prompt → llm.Client.Complete → stop-reason check → strict decode →
// envelope + segment validation → at most one repair turn → result.
// It performs no database access and no SQL.
type Interpreter struct {
	client llm.Client
	cfg    InterpreterConfig
	schema map[string]any
}

// NewInterpreter validates cfg and returns an Interpreter.
func NewInterpreter(client llm.Client, cfg InterpreterConfig) (*Interpreter, error) {
	switch {
	case client == nil:
		return nil, errors.New("ai: nil llm client")
	case strings.TrimSpace(cfg.Model) == "":
		return nil, errors.New("ai: interpreter model is required")
	case cfg.Timeout <= 0:
		return nil, errors.New("ai: interpreter timeout must be positive")
	case cfg.MaxTokens <= 0:
		return nil, errors.New("ai: interpreter max tokens must be positive")
	}
	return &Interpreter{client: client, cfg: cfg, schema: InterpretationSchema()}, nil
}

// attempt tracks the state of one Interpret call.
type attempt struct {
	calls        int
	maxTokens    int
	truncRetried bool
	usage        llm.Usage
	model        string
}

// Interpret interprets brief. Errors are *Error values (see errors.go) or,
// when ctx was cancelled by the caller, an error wrapping ctx.Err().
func (it *Interpreter) Interpret(ctx context.Context, brief string) (Interpretation, error) {
	brief = strings.TrimSpace(norm.NFC.String(brief))
	if n := utf8.RuneCountInString(brief); n < MinBriefRunes || n > MaxBriefRunes {
		return Interpretation{}, it.fail(&attempt{}, &Error{Kind: ErrInvalidBrief,
			Cause: fmt.Errorf("brief must be %d–%d characters", MinBriefRunes, MaxBriefRunes)})
	}

	callCtx, cancel := context.WithTimeout(ctx, it.cfg.Timeout)
	defer cancel()

	st := &attempt{maxTokens: it.cfg.MaxTokens, model: it.cfg.Model}
	messages := []llm.Message{{Role: llm.RoleUser, Content: segmentUserMessage(brief)}}

	text, err := it.call(ctx, callCtx, st, messages)
	if err != nil {
		return Interpretation{}, err
	}
	in, errs := ParseInterpretation([]byte(text))
	if len(errs) == 0 {
		return it.done(in, st, false), nil
	}

	// One repair turn: append-only history (the invalid proposal as the
	// assistant turn, then the structured errors as a user turn).
	messages = append(messages,
		llm.Message{Role: llm.RoleAssistant, Content: text},
		llm.Message{Role: llm.RoleUser, Content: segmentRepairMessage(errs)},
	)
	text, err = it.call(ctx, callCtx, st, messages)
	if err != nil {
		return Interpretation{}, err
	}
	in, errs = ParseInterpretation([]byte(text))
	if len(errs) == 0 {
		return it.done(in, st, true), nil
	}
	return Interpretation{}, it.fail(st, &Error{Kind: ErrOutputInvalid, Details: errs, RawProposal: boundRaw(text)})
}

// call performs one logical model call: at most one extra request when the
// output was truncated (with a doubled max_tokens budget, kept for the rest
// of the interpretation). Only a complete end_turn answer is returned.
func (it *Interpreter) call(parent, ctx context.Context, st *attempt, messages []llm.Message) (string, error) {
	for {
		req := llm.Request{
			Task:       TaskSegmentInterpretation,
			Model:      it.cfg.Model,
			System:     segmentSystemPrompt,
			Messages:   append([]llm.Message(nil), messages...),
			JSONSchema: it.schema,
			MaxTokens:  st.maxTokens,
			Effort:     it.cfg.Effort,
		}
		st.calls++
		resp, err := it.client.Complete(ctx, req)
		if perr := parent.Err(); perr != nil {
			return "", fmt.Errorf("ai: interpretation cancelled: %w", perr)
		}
		if err != nil {
			return "", it.fail(st, classify(ctx, err))
		}
		st.usage = st.usage.Add(resp.Usage)
		if resp.Model != "" {
			st.model = resp.Model
		}
		switch resp.StopReason {
		case llm.StopEndTurn:
			return resp.Text, nil
		case llm.StopRefusal:
			return "", it.fail(st, &Error{Kind: ErrLLMRefused})
		case llm.StopMaxTokens:
			if !st.truncRetried {
				st.truncRetried = true
				st.maxTokens *= 2
				continue
			}
			return "", it.fail(st, &Error{Kind: ErrLLMTruncated})
		default:
			// Any other stop reason means the answer may be incomplete.
			return "", it.fail(st, &Error{Kind: ErrLLMTruncated, Cause: fmt.Errorf("unexpected stop reason %q", resp.StopReason)})
		}
	}
}

// classify maps a client error to a domain error.
func classify(ctx context.Context, err error) *Error {
	var ute *UnsupportedTargetingError
	switch {
	case errors.As(err, &ute):
		// The demo interpreter declined the brief: a caller-side, actionable
		// problem, not a provider failure; repairing would not help.
		return &Error{Kind: ErrInvalidBrief, Details: append(segment.Errors(nil), ute.Problems...), Model: llm.DemoModel, Cause: ute}
	case errors.Is(err, context.DeadlineExceeded) || errors.Is(ctx.Err(), context.DeadlineExceeded):
		return &Error{Kind: ErrLLMUnavailable, Retryable: true, Cause: fmt.Errorf("interpretation timed out: %w", err)}
	case errors.Is(err, llm.ErrRejected):
		return &Error{Kind: ErrLLMRejected, Cause: err}
	default:
		return &Error{Kind: ErrLLMUnavailable, Retryable: llm.Retryable(err), Cause: err}
	}
}

func (it *Interpreter) fail(st *attempt, e *Error) *Error {
	if e.Model == "" {
		e.Model = st.model
	}
	if e.Model == "" {
		e.Model = it.cfg.Model
	}
	e.PromptVersion = SegmentPromptVersion
	e.Calls = st.calls
	return e
}

func (it *Interpreter) done(in Interpretation, st *attempt, repaired bool) Interpretation {
	in.Meta = Metadata{
		Model:         st.model,
		PromptVersion: SegmentPromptVersion,
		Calls:         st.calls,
		Repaired:      repaired,
		Usage:         st.usage,
	}
	return in
}

func boundRaw(s string) string {
	if len(s) <= maxRawProposalBytes {
		return s
	}
	cut := maxRawProposalBytes
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut]
}
