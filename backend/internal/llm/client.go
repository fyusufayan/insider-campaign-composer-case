// Package llm is the provider-neutral LLM client abstraction (ARCHITECTURE
// §7.2, ADR-016). It knows nothing about campaigns, segments or SQL: callers
// (package ai) own prompts, schemas, decoding, validation and repair.
//
// Implementations: Anthropic (production; anthropic.go is the only code that
// imports the Anthropic SDK), Scripted (unit tests), Demo (running the app
// without an API key).
package llm

import (
	"context"
	"time"
)

// Client sends one request to a model. Implementations must honour ctx
// cancellation and deadlines, and must never substitute output from another
// provider when they fail (invariant 17).
type Client interface {
	// Complete returns the whole response. A non-nil error means no usable
	// response; a nil error still requires the caller to inspect StopReason.
	Complete(ctx context.Context, req Request) (Response, error)
	// Stream delivers the response incrementally to onEvent (called
	// sequentially from the calling goroutine's perspective) and returns the
	// accumulated Response. If onEvent returns an error, streaming stops and
	// that error is returned.
	Stream(ctx context.Context, req Request, onEvent func(StreamEvent) error) (Response, error)
}

// Role is the author of a message.
type Role string

// Roles.
const (
	RoleUser      Role = "user"
	RoleAssistant Role = "assistant"
)

// Message is one turn of the conversation. Requests must end with a user
// message (assistant prefill is never used).
type Message struct {
	Role    Role
	Content string
}

// Effort is the reasoning effort hint (thinking is controlled by effort only).
type Effort string

// Efforts accepted by configuration.
const (
	EffortLow    Effort = "low"
	EffortMedium Effort = "medium"
	EffortHigh   Effort = "high"
	EffortMax    Effort = "max"
)

// Request is one model call. Sampling parameters (temperature, top_p, top_k)
// deliberately do not exist.
type Request struct {
	// Task identifies the calling use case (e.g. "segment_interpretation");
	// used for logging/metrics and by Demo to pick a responder.
	Task string
	// Model comes from configuration only (CLAUDE_*_MODEL).
	Model    string
	System   string
	Messages []Message
	// JSONSchema, when non-nil, requests structured output conforming to it.
	// It is treated as read-only by implementations.
	JSONSchema map[string]any
	MaxTokens  int
	Effort     Effort
}

// StopReason says why the model stopped generating.
type StopReason string

// Stop reasons. Only StopEndTurn denotes a complete answer.
const (
	StopEndTurn      StopReason = "end_turn"
	StopMaxTokens    StopReason = "max_tokens"
	StopRefusal      StopReason = "refusal"
	StopStopSequence StopReason = "stop_sequence"
)

// Usage is the token accounting reported by the provider (zero if unknown).
type Usage struct {
	InputTokens  int
	OutputTokens int
}

// Add returns the sum of two usages.
func (u Usage) Add(o Usage) Usage {
	return Usage{InputTokens: u.InputTokens + o.InputTokens, OutputTokens: u.OutputTokens + o.OutputTokens}
}

// Response is a model answer.
type Response struct {
	// Text is the concatenated text output (the JSON document for
	// structured output).
	Text       string
	StopReason StopReason
	// Model is the model that served the request as reported by the
	// provider; for Demo it is DemoModel.
	Model     string
	Usage     Usage
	Latency   time.Duration
	RequestID string
}

// StreamEventKind distinguishes stream events.
type StreamEventKind string

// Stream event kinds.
const (
	// EventThinking signals that the model is reasoning before output.
	EventThinking StreamEventKind = "thinking"
	// EventTextDelta carries the next fragment of Response.Text.
	EventTextDelta StreamEventKind = "text_delta"
)

// StreamEvent is one incremental stream event.
type StreamEvent struct {
	Kind StreamEventKind
	Text string
}
