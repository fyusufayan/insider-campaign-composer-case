package ai

import (
	"errors"
	"strings"

	"github.com/fyusufayan/insider-campaign-composer-case/backend/internal/segment"
)

// Domain errors of AI use cases (ARCHITECTURE §7.1, §7.5). Interpreter
// returns an *Error whose Kind is one of these, or a context error when the
// caller's context was cancelled. CP6 maps them to the API error envelope.
var (
	// ErrLLMUnavailable: provider unreachable, overloaded or timed out
	// (503 llm_unavailable, retryable).
	ErrLLMUnavailable = errors.New("ai: llm unavailable")
	// ErrLLMRejected: the provider rejected the request (502/500 llm_rejected,
	// not retryable — a configuration problem).
	ErrLLMRejected = errors.New("ai: llm rejected the request")
	// ErrLLMRefused: the model refused (422 llm_refused).
	ErrLLMRefused = errors.New("ai: llm refused")
	// ErrLLMTruncated: output hit max_tokens even after the retry with a
	// doubled budget (422/502 llm_truncated); never used as output.
	ErrLLMTruncated = errors.New("ai: llm output truncated")
	// ErrOutputInvalid: output failed validation after the repair turn
	// (422 ai_output_invalid, with Details and RawProposal).
	ErrOutputInvalid = errors.New("ai: llm output invalid")
	// ErrInvalidBrief: the brief itself is unusable (caller error): outside
	// the length limits, or declined by the demo interpreter because a
	// targeting requirement cannot be expressed (Details say which).
	ErrInvalidBrief = errors.New("ai: invalid brief")
)

// Error codes matching the API envelope codes.
const (
	CodeLLMUnavailable = "llm_unavailable"
	CodeLLMRejected    = "llm_rejected"
	CodeLLMRefused     = "llm_refused"
	CodeLLMTruncated   = "llm_truncated"
	CodeOutputInvalid  = "ai_output_invalid"
	CodeInvalidBrief   = "invalid_brief"
)

// Error is a structured AI failure.
type Error struct {
	// Kind is one of the Err* sentinels above.
	Kind error
	// Retryable reports whether retrying the same request later may succeed.
	Retryable bool
	// Details are the field-level validation errors of the last proposal
	// (ErrOutputInvalid only).
	Details segment.Errors
	// RawProposal is the last rejected model output (ErrOutputInvalid only;
	// untrusted text — display escaped, never log in full).
	RawProposal string
	// Model and PromptVersion identify the failing configuration.
	Model         string
	PromptVersion string
	// Calls is the number of LLM calls made.
	Calls int
	// Cause is the underlying error (provider error, if any).
	Cause error
}

func (e *Error) Error() string {
	s := e.Kind.Error()
	if e.Cause != nil {
		s += ": " + e.Cause.Error()
	} else if len(e.Details) > 0 {
		s += ": " + e.Details.Error()
	}
	return s
}

// Is matches the Kind sentinel.
func (e *Error) Is(target error) bool { return target == e.Kind }

// Unwrap returns the cause.
func (e *Error) Unwrap() error { return e.Cause }

// Code returns the API error code for the error kind.
func (e *Error) Code() string {
	switch e.Kind {
	case ErrLLMUnavailable:
		return CodeLLMUnavailable
	case ErrLLMRejected:
		return CodeLLMRejected
	case ErrLLMRefused:
		return CodeLLMRefused
	case ErrLLMTruncated:
		return CodeLLMTruncated
	case ErrOutputInvalid:
		return CodeOutputInvalid
	case ErrInvalidBrief:
		return CodeInvalidBrief
	default:
		return "internal"
	}
}

// Problem codes of UnsupportedTargetingError (path "brief").
const (
	// CodeNoTargeting: no supported targeting criterion was recognized.
	CodeNoTargeting = "no_supported_targeting"
	// CodeUnsupportedTargeting: a targeting requirement has no DSL equivalent.
	CodeUnsupportedTargeting = "unsupported_targeting"
	// CodeInvalidTargeting: a recognized requirement has an invalid value
	// (e.g. a window outside 1–365 days).
	CodeInvalidTargeting = "invalid_targeting"
	// CodeUnsupportedChannel: only channels other than SMS/email were requested.
	CodeUnsupportedChannel = "unsupported_channel"
)

// UnsupportedTargetingError is returned by the demo interpreter when it
// cannot express every targeting requirement it recognized in the brief;
// emitting a rule without them would silently broaden the audience. The
// Interpreter maps it to ErrInvalidBrief (not retryable, no repair turn).
// Problems hold fixed, display-safe messages (never brief text); at most
// MaxNotes entries.
type UnsupportedTargetingError struct {
	Problems segment.Errors
}

func (e *UnsupportedTargetingError) Error() string {
	codes := make([]string, len(e.Problems))
	for i, p := range e.Problems {
		codes[i] = p.Code
	}
	return "demo: brief cannot be expressed as a segment (" + strings.Join(codes, ", ") + ")"
}

// AsError extracts an *Error.
func AsError(err error) (*Error, bool) {
	var e *Error
	ok := errors.As(err, &e)
	return e, ok
}
