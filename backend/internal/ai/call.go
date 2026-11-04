package ai

import (
	"context"
	"errors"
	"fmt"
	"unicode/utf8"

	"github.com/fyusufayan/insider-campaign-composer-case/backend/internal/llm"
	"github.com/fyusufayan/insider-campaign-composer-case/backend/internal/segment"
)

// modelCall is the fixed configuration of the model calls of one AI use case
// (interpretation or content generation). It owns the policy shared by both:
// stop-reason handling, one truncation retry with a doubled budget, and the
// mapping of client errors to domain errors. Prompts, schemas, decoding,
// validation and the repair turn stay with the use case.
type modelCall struct {
	client        llm.Client
	task          string
	system        string
	promptVersion string
	model         string
	effort        llm.Effort
	schema        map[string]any
}

// attempt tracks the state of one use-case invocation across its calls.
type attempt struct {
	calls        int
	maxTokens    int
	truncRetried bool
	usage        llm.Usage
	model        string
}

// do performs one logical model call: at most one extra request when the
// output was truncated (with a doubled max_tokens budget, kept for the rest
// of the invocation). Only a complete end_turn answer is returned. parent is
// the caller's context (its cancellation is reported as such); ctx carries
// the use case's deadline.
func (mc modelCall) do(parent, ctx context.Context, st *attempt, messages []llm.Message) (string, error) {
	for {
		req := llm.Request{
			Task:       mc.task,
			Model:      mc.model,
			System:     mc.system,
			Messages:   append([]llm.Message(nil), messages...),
			JSONSchema: mc.schema,
			MaxTokens:  st.maxTokens,
			Effort:     mc.effort,
		}
		st.calls++
		resp, err := mc.client.Complete(ctx, req)
		if perr := parent.Err(); perr != nil {
			return "", fmt.Errorf("ai: %s cancelled: %w", mc.task, perr)
		}
		if err != nil {
			return "", mc.fail(st, classify(ctx, err))
		}
		st.usage = st.usage.Add(resp.Usage)
		if resp.Model != "" {
			st.model = resp.Model
		}
		switch resp.StopReason {
		case llm.StopEndTurn:
			return resp.Text, nil
		case llm.StopRefusal:
			return "", mc.fail(st, &Error{Kind: ErrLLMRefused})
		case llm.StopMaxTokens:
			if !st.truncRetried {
				st.truncRetried = true
				st.maxTokens *= 2
				continue
			}
			return "", mc.fail(st, &Error{Kind: ErrLLMTruncated})
		default:
			// Any other stop reason means the answer may be incomplete.
			return "", mc.fail(st, &Error{Kind: ErrLLMTruncated, Cause: fmt.Errorf("unexpected stop reason %q", resp.StopReason)})
		}
	}
}

// fail completes e with the identity of the failing configuration.
func (mc modelCall) fail(st *attempt, e *Error) *Error {
	if e.Model == "" {
		e.Model = st.model
	}
	if e.Model == "" {
		e.Model = mc.model
	}
	e.PromptVersion = mc.promptVersion
	e.Calls = st.calls
	return e
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
		return &Error{Kind: ErrLLMUnavailable, Retryable: true, Cause: fmt.Errorf("model call timed out: %w", err)}
	case errors.Is(err, llm.ErrRejected):
		return &Error{Kind: ErrLLMRejected, Cause: err}
	default:
		return &Error{Kind: ErrLLMUnavailable, Retryable: llm.Retryable(err), Cause: err}
	}
}

// boundRaw truncates a rejected proposal (at a rune boundary) for *Error.
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
