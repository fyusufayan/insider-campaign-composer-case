package llm

// This file is the only code that imports the Anthropic SDK (ADR-014). SDK
// types never leave it: callers see Request, Response and *Error only.

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/anthropics/anthropic-sdk-go/v2"
	"github.com/anthropics/anthropic-sdk-go/v2/option"
)

// AnthropicProviderName identifies the provider in logs.
const AnthropicProviderName = "anthropic"

// MaxAnthropicRetries is the largest accepted AnthropicConfig.MaxRetries.
const MaxAnthropicRetries = 5

// Construction errors.
var (
	ErrAnthropicNoAPIKey = errors.New("llm: anthropic requires a non-empty API key")
	ErrAnthropicRetries  = fmt.Errorf("llm: anthropic max retries must be in [0, %d]", MaxAnthropicRetries)
)

// Request validation failures. They are returned as *Error{Kind: ErrRejected}
// before anything is sent: a malformed request is a programming or
// configuration error, never retryable.
var (
	errNoModel      = errors.New("model is required")
	errNoMaxTokens  = errors.New("max tokens must be positive")
	errNoMessages   = errors.New("at least one message is required")
	errLastNotUser  = errors.New("the last message must be a user message (assistant prefill is not used)")
	errEmptyMessage = errors.New("message content must not be empty")
)

// AnthropicConfig configures NewAnthropic.
type AnthropicConfig struct {
	// APIKey is ANTHROPIC_API_KEY. It is passed explicitly to the SDK; no
	// other credential source (ANTHROPIC_AUTH_TOKEN, profiles, federation)
	// and no ANTHROPIC_BASE_URL / ANTHROPIC_CUSTOM_HEADERS are consulted.
	APIKey string
	// MaxRetries is the SDK's automatic retry count per Complete call
	// (connection errors, 408, 409, 429, 5xx; 0 = a single attempt). All
	// attempts share the caller's context deadline.
	MaxRetries int
	// HTTPClient, when non-nil, replaces the default HTTP client (tests use
	// it to capture requests without network access). The default has no
	// client-level timeout: the caller's context bounds every call.
	HTTPClient *http.Client
	// Logger receives one metadata line per call (never prompts, outputs or
	// credentials). Nil discards.
	Logger *slog.Logger
}

// Anthropic is the production Client backed by the official Anthropic Go SDK
// and the Messages API (ARCHITECTURE §7.1).
type Anthropic struct {
	client anthropic.Client
	log    *slog.Logger
	now    func() time.Time
}

var _ Client = (*Anthropic)(nil)

// NewAnthropic validates cfg and builds the client. It performs no I/O: the
// key is not verified by a (billable) request at startup.
func NewAnthropic(cfg AnthropicConfig) (*Anthropic, error) {
	key := strings.TrimSpace(cfg.APIKey)
	if key == "" {
		return nil, ErrAnthropicNoAPIKey
	}
	if cfg.MaxRetries < 0 || cfg.MaxRetries > MaxAnthropicRetries {
		return nil, fmt.Errorf("%w (got %d)", ErrAnthropicRetries, cfg.MaxRetries)
	}
	hc := cfg.HTTPClient
	if hc == nil {
		hc = defaultAnthropicHTTPClient()
	}
	log := cfg.Logger
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	client := anthropic.NewClient(
		// Skip the SDK's environment autoload so the only credential is ours.
		option.WithoutEnvironmentDefaults(),
		option.WithAPIKey(key),
		option.WithHTTPClient(hc),
		option.WithMaxRetries(cfg.MaxRetries),
	)
	return &Anthropic{client: client, log: log, now: time.Now}, nil
}

// defaultAnthropicHTTPClient clones the default transport (proxy settings,
// TLS from the system roots). No per-request or header timeout is set here:
// the interpreter's context deadline is the single time budget, and a
// per-attempt timeout would make the SDK abandon and re-run (and re-bill) a
// slow but healthy generation.
func defaultAnthropicHTTPClient() *http.Client {
	if t, ok := http.DefaultTransport.(*http.Transport); ok {
		return &http.Client{Transport: t.Clone()}
	}
	return &http.Client{Transport: http.DefaultTransport}
}

// Complete implements Client with one non-streaming Messages API call (plus
// the SDK's bounded automatic retries).
func (a *Anthropic) Complete(ctx context.Context, req Request) (Response, error) {
	if err := ctx.Err(); err != nil {
		return Response{}, err
	}
	params, err := buildMessageParams(req)
	if err != nil {
		e := &Error{Kind: ErrRejected, Err: fmt.Errorf("anthropic: invalid request: %w", err)}
		a.logFailure(ctx, req, e, 0)
		return Response{}, e
	}

	var httpResp *http.Response
	start := a.now()
	msg, err := a.client.Messages.New(ctx, params, option.WithResponseInto(&httpResp))
	latency := a.now().Sub(start)
	if err != nil {
		e := classifyAnthropicError(ctx, err)
		a.logFailure(ctx, req, e, latency)
		return Response{}, e
	}

	resp, err := convertMessage(msg, responseRequestID(httpResp), latency)
	if err != nil {
		a.logFailure(ctx, req, err, latency)
		return Response{}, err
	}
	a.log.InfoContext(ctx, "llm request completed", "provider", AnthropicProviderName,
		"task", req.Task, "model_requested", req.Model, "model", resp.Model,
		"effort", string(req.Effort), "max_tokens", req.MaxTokens, "stop_reason", string(resp.StopReason),
		"input_tokens", resp.Usage.InputTokens, "output_tokens", resp.Usage.OutputTokens,
		"latency_ms", latency.Milliseconds(), "provider_request_id", resp.RequestID)
	return resp, nil
}

// Stream implements Client with one streaming Messages API call
// (client.Messages.NewStreaming; the SDK's bounded automatic retries apply to
// establishing the stream, never to an interrupted one). Every event is
// accumulated with Message.Accumulate; a thinking block start is reported as
// EventThinking and each text delta as EventTextDelta. The accumulated message
// goes through the same conversion as Complete, so callers see an identical
// Response. A stream that ends without message_stop, an in-stream error event
// or an Accumulate failure is ErrUnavailable and its partial output is never
// returned. If onEvent fails, the stream (and the HTTP response) is closed and
// that error is returned unchanged.
func (a *Anthropic) Stream(ctx context.Context, req Request, onEvent func(StreamEvent) error) (Response, error) {
	if err := ctx.Err(); err != nil {
		return Response{}, err
	}
	params, err := buildMessageParams(req)
	if err != nil {
		e := &Error{Kind: ErrRejected, Err: fmt.Errorf("anthropic: invalid request: %w", err)}
		a.logFailure(ctx, req, e, 0)
		return Response{}, e
	}

	var httpResp *http.Response
	start := a.now()
	stream := a.client.Messages.NewStreaming(ctx, params, option.WithResponseInto(&httpResp))
	defer func() { _ = stream.Close() }()

	var (
		msg      anthropic.Message
		stopped  bool
		eventErr error
		thinking bool
	)
	for stream.Next() {
		ev := stream.Current()
		if err := msg.Accumulate(ev); err != nil {
			eventErr = &Error{Kind: ErrUnavailable, RequestID: responseRequestID(httpResp),
				Err: errors.New("anthropic: inconsistent stream events")}
			break
		}
		switch v := ev.AsAny().(type) {
		case anthropic.ContentBlockStartEvent:
			switch v.ContentBlock.Type {
			case "thinking", "redacted_thinking":
				if !thinking { // reported once per message
					thinking = true
					if err := onEvent(StreamEvent{Kind: EventThinking}); err != nil {
						eventErr = err
					}
				}
			}
		case anthropic.ContentBlockDeltaEvent:
			if v.Delta.Type == "text_delta" && v.Delta.Text != "" {
				if err := onEvent(StreamEvent{Kind: EventTextDelta, Text: v.Delta.Text}); err != nil {
					eventErr = err
				}
			}
		case anthropic.MessageStopEvent:
			stopped = true
		}
		if eventErr != nil {
			break
		}
	}
	latency := a.now().Sub(start)
	if eventErr != nil {
		if cerr := ctx.Err(); cerr != nil {
			eventErr = fmt.Errorf("llm: anthropic request: %w", cerr)
		}
		a.logFailure(ctx, req, eventErr, latency)
		return Response{}, eventErr
	}
	if err := stream.Err(); err != nil {
		e := classifyAnthropicError(ctx, err)
		a.logFailure(ctx, req, e, latency)
		return Response{}, e
	}
	if cerr := ctx.Err(); cerr != nil {
		e := fmt.Errorf("llm: anthropic request: %w", cerr)
		a.logFailure(ctx, req, e, latency)
		return Response{}, e
	}
	if !stopped {
		e := &Error{Kind: ErrUnavailable, RequestID: responseRequestID(httpResp),
			Err: errors.New("anthropic: stream ended before message_stop")}
		a.logFailure(ctx, req, e, latency)
		return Response{}, e
	}

	resp, err := convertMessage(&msg, responseRequestID(httpResp), latency)
	if err != nil {
		a.logFailure(ctx, req, err, latency)
		return Response{}, err
	}
	a.log.InfoContext(ctx, "llm stream completed", "provider", AnthropicProviderName,
		"task", req.Task, "model_requested", req.Model, "model", resp.Model,
		"effort", string(req.Effort), "max_tokens", req.MaxTokens, "stop_reason", string(resp.StopReason),
		"input_tokens", resp.Usage.InputTokens, "output_tokens", resp.Usage.OutputTokens,
		"latency_ms", latency.Milliseconds(), "provider_request_id", resp.RequestID)
	return resp, nil
}

func (a *Anthropic) logFailure(ctx context.Context, req Request, err error, latency time.Duration) {
	attrs := []any{"provider", AnthropicProviderName, "task", req.Task, "model_requested", req.Model,
		"latency_ms", latency.Milliseconds()}
	var e *Error
	if errors.As(err, &e) {
		kind := "unavailable"
		if errors.Is(e, ErrRejected) {
			kind = "rejected"
		}
		attrs = append(attrs, "error_kind", kind, "status", e.StatusCode, "provider_request_id", e.RequestID)
	}
	// err is built by this file from metadata only (see classifyAnthropicError).
	a.log.WarnContext(ctx, "llm request failed", append(attrs, "err", err.Error())...)
}

// buildMessageParams translates a provider-neutral Request. Sampling
// parameters do not exist in Request (nor in this SDK version's params), no
// tools or tool_choice are sent, and the conversation must end with a user
// turn, so assistant prefill cannot happen.
func buildMessageParams(req Request) (anthropic.MessageNewParams, error) {
	var p anthropic.MessageNewParams
	switch {
	case strings.TrimSpace(req.Model) == "":
		return p, errNoModel
	case req.MaxTokens <= 0:
		return p, errNoMaxTokens
	case len(req.Messages) == 0:
		return p, errNoMessages
	case req.Messages[len(req.Messages)-1].Role != RoleUser:
		return p, errLastNotUser
	}
	effort, err := anthropicEffort(req.Effort)
	if err != nil {
		return p, err
	}

	msgs := make([]anthropic.MessageParam, 0, len(req.Messages))
	for i, m := range req.Messages {
		if m.Content == "" {
			return p, fmt.Errorf("message %d: %w", i, errEmptyMessage)
		}
		switch m.Role {
		case RoleUser:
			msgs = append(msgs, anthropic.NewUserMessage(anthropic.NewTextBlock(m.Content)))
		case RoleAssistant:
			msgs = append(msgs, anthropic.NewAssistantMessage(anthropic.NewTextBlock(m.Content)))
		default:
			return p, fmt.Errorf("message %d: unsupported role %q", i, m.Role)
		}
	}

	p = anthropic.MessageNewParams{
		Model:     anthropic.Model(req.Model),
		MaxTokens: int64(req.MaxTokens),
		Messages:  msgs,
		// Adaptive thinking is sent explicitly so every model receives the
		// same reasoning configuration (depth controlled by effort only). A
		// model that does not support it rejects the request with a 400
		// rather than silently running without thinking.
		Thinking:     anthropic.ThinkingConfigParamUnion{OfAdaptive: &anthropic.ThinkingConfigAdaptiveParam{}},
		OutputConfig: anthropic.OutputConfigParam{Effort: effort},
	}
	if req.System != "" {
		p.System = []anthropic.TextBlockParam{{Text: req.System}}
	}
	if req.JSONSchema != nil {
		// Stable structured outputs: output_config.format {type: json_schema}.
		p.OutputConfig.Format = anthropic.JSONOutputFormatParam{Schema: req.JSONSchema}
	}
	return p, nil
}

// anthropicEffort maps the configured effort one-to-one. An empty or unknown
// value is an error: omitting effort would silently select the model's own
// default (medium on claude-opus-5-5), not the configured level.
func anthropicEffort(e Effort) (anthropic.OutputConfigEffort, error) {
	switch e {
	case EffortLow:
		return anthropic.OutputConfigEffortLow, nil
	case EffortMedium:
		return anthropic.OutputConfigEffortMedium, nil
	case EffortHigh:
		return anthropic.OutputConfigEffortHigh, nil
	case EffortMax:
		return anthropic.OutputConfigEffortMax, nil
	default:
		return "", fmt.Errorf("unsupported effort %q (want low, medium, high or max)", e)
	}
}

// convertMessage maps a successful (HTTP 2xx) response. Text blocks are
// concatenated; thinking blocks are skipped (display is omitted, so their
// text is empty anyway). Any other block type means the response is not the
// plain structured answer that was requested and is never used.
func convertMessage(msg *anthropic.Message, requestID string, latency time.Duration) (Response, error) {
	if msg == nil {
		return Response{}, &Error{Kind: ErrUnavailable, RequestID: requestID, Err: errors.New("anthropic: empty response")}
	}
	var text strings.Builder
	hasText := false
	for _, block := range msg.Content {
		switch block.Type {
		case "text":
			hasText = true
			text.WriteString(block.Text)
		case "thinking", "redacted_thinking":
		default:
			return Response{}, &Error{Kind: ErrUnavailable, RequestID: requestID,
				Err: fmt.Errorf("anthropic: unexpected content block type %q", block.Type)}
		}
	}
	stop := mapStopReason(msg.StopReason)
	if stop == StopEndTurn && !hasText {
		return Response{}, &Error{Kind: ErrUnavailable, RequestID: requestID,
			Err: errors.New("anthropic: end_turn response without text content")}
	}
	return Response{
		Text:       text.String(),
		StopReason: stop,
		Model:      string(msg.Model),
		Usage:      Usage{InputTokens: int(msg.Usage.InputTokens), OutputTokens: int(msg.Usage.OutputTokens)},
		Latency:    latency,
		RequestID:  requestID,
	}, nil
}

// mapStopReason maps the known stop reasons; any other value (pause_turn,
// tool_use, model_context_window_exceeded, empty, future values) is passed
// through verbatim, and callers treat everything but end_turn as unusable.
func mapStopReason(r anthropic.StopReason) StopReason {
	switch r {
	case anthropic.StopReasonEndTurn:
		return StopEndTurn
	case anthropic.StopReasonMaxTokens:
		return StopMaxTokens
	case anthropic.StopReasonRefusal:
		return StopRefusal
	case anthropic.StopReasonStopSequence:
		return StopStopSequence
	default:
		return StopReason(r)
	}
}

func responseRequestID(r *http.Response) string {
	if r == nil {
		return ""
	}
	return r.Header.Get("request-id")
}

// classifyAnthropicError maps an SDK error (after the SDK's own retries).
// The resulting message is built from metadata only — status, the API error
// type and the request id — never from the SDK error string, which embeds the
// raw response body.
func classifyAnthropicError(ctx context.Context, err error) error {
	if cerr := ctx.Err(); cerr != nil {
		// The caller's context ended (cancellation or the task deadline).
		return fmt.Errorf("llm: anthropic request: %w", cerr)
	}
	var apiErr *anthropic.Error
	if errors.As(err, &apiErr) {
		e := &Error{StatusCode: apiErr.StatusCode, RequestID: apiErr.RequestID}
		typ := string(apiErr.Type())
		if typ == "" {
			typ = "unknown_error"
		}
		switch {
		case apiErr.StatusCode < http.StatusBadRequest:
			// An error event inside a 200 stream (e.g. overloaded_error
			// after the response started): the HTTP status says nothing,
			// so the API error type decides. The SDK does not retry it.
			e.StatusCode = 0
			if retryableStreamErrorType(typ) {
				e.Kind = ErrUnavailable
			} else {
				e.Kind = ErrRejected
			}
		case retryableStatus(apiErr.StatusCode):
			e.Kind = ErrUnavailable
		default:
			e.Kind = ErrRejected
		}
		e.Err = fmt.Errorf("anthropic %s", typ)
		return e
	}
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
		// A context error not caused by ctx (e.g. a transport-level timeout).
		return &Error{Kind: ErrUnavailable, Err: fmt.Errorf("anthropic request timed out: %w", context.DeadlineExceeded)}
	}
	// Transport failures (DNS, TCP, TLS, proxy, reset). Their text (URLs,
	// proxy addresses and credentials, quoted bytes) is not safe to log, so
	// only a fixed message is kept and the cause is not wrapped.
	var urlErr *url.Error
	if errors.As(err, &urlErr) {
		return &Error{Kind: ErrUnavailable, Err: errors.New("anthropic transport failure")}
	}
	// Anything else (e.g. an undecodable 2xx body) may quote response bytes,
	// so only a fixed message is kept.
	return &Error{Kind: ErrUnavailable, Err: errors.New("anthropic: unreadable response")}
}

// retryableStatus: the statuses the SDK itself retries (408 timeout, 409
// conflict, 429 rate limit, 5xx/529 overload) remain retryable later; other
// 4xx (400 invalid request or schema, 401, 403, 404 unknown model, 413) are
// configuration problems.
func retryableStatus(code int) bool {
	return code == http.StatusRequestTimeout || code == http.StatusConflict ||
		code == http.StatusTooManyRequests || code >= http.StatusInternalServerError
}

// retryableStreamErrorType classifies an in-stream error event by its API
// error type: overload, rate limiting, timeouts and internal errors are
// transient; anything else (invalid_request_error, authentication, …) is not.
func retryableStreamErrorType(typ string) bool {
	switch typ {
	case "overloaded_error", "rate_limit_error", "api_error", "timeout_error":
		return true
	default:
		return false
	}
}
