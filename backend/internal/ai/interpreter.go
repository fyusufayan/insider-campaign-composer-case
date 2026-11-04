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

// call performs one logical model call (see modelCall.do).
func (it *Interpreter) call(parent, ctx context.Context, st *attempt, messages []llm.Message) (string, error) {
	return it.modelCall().do(parent, ctx, st, messages)
}

func (it *Interpreter) modelCall() modelCall {
	return modelCall{
		client: it.client, task: TaskSegmentInterpretation, system: segmentSystemPrompt,
		promptVersion: SegmentPromptVersion, model: it.cfg.Model, effort: it.cfg.Effort, schema: it.schema,
	}
}

func (it *Interpreter) fail(st *attempt, e *Error) *Error { return it.modelCall().fail(st, e) }

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
