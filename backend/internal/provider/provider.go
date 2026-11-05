// Package provider is the HTTP client of the SMS/email provider and the
// classification of its responses (ARCHITECTURE §10.5, §10.6, ADR-009,
// ADR-020). The only provider in scope is the mock provider (cmd/
// mockprovider); no real SMS or email vendor is integrated, and the only
// adapter declares the mock's capabilities.
//
// The package performs no rate limiting (that is delivery's job) and knows
// nothing about campaigns or SQL. Requests carry the delivery's
// idempotency key in the Idempotency-Key header; every retry of a delivery
// must send the same key, destination and content (the caller guarantees
// that; this client marshals the same Message to the same bytes).
package provider

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// Channel is the provider channel.
type Channel string

// Channels.
const (
	ChannelSMS   Channel = "sms"
	ChannelEmail Channel = "email"
)

// Message is one provider request. Text is the SMS text, or the complete
// plain-text email body as sent (body, separator and footer).
type Message struct {
	Channel        Channel
	IdempotencyKey string
	To             string
	Subject        string // email only
	Text           string
}

// Outcome classifies one provider request (delivery_attempts.outcome).
type Outcome string

// Outcomes (§10.5).
const (
	// OutcomeAccepted: 2xx; the provider accepted the message.
	OutcomeAccepted Outcome = "accepted"
	// OutcomeReplayed: 2xx with Idempotent-Replayed: true — an earlier
	// request with the same key was accepted; nothing was sent again.
	OutcomeReplayed Outcome = "replayed"
	// OutcomeRateLimited: 429 (RetryAfter is set).
	OutcomeRateLimited Outcome = "rate_limited"
	// OutcomeTransientError: 5xx, or the connection could not be
	// established (the request certainly did not reach the provider).
	OutcomeTransientError Outcome = "transient_error"
	// OutcomeTimeout: no response within the request timeout. Ambiguous:
	// the provider may have accepted the message.
	OutcomeTimeout Outcome = "timeout"
	// OutcomePermanentError: 4xx other than 429 (incl. 422
	// idempotency_key_reuse); retrying cannot help.
	OutcomePermanentError Outcome = "permanent_error"
	// OutcomeUnknown: the request may have been sent but no usable answer
	// was received (connection broken after writing, unreadable 2xx body,
	// request aborted by shutdown). Ambiguous like a timeout.
	OutcomeUnknown Outcome = "unknown"
)

// Ambiguous reports whether the provider may have accepted the message
// although we could not confirm it (outcome_uncertain, §10.5).
func (o Outcome) Ambiguous() bool { return o == OutcomeTimeout || o == OutcomeUnknown }

// Result is the classified result of one request. It never contains the
// destination or the message text. ErrorCode is a short, sanitized code
// (e.g. "http_503", "idempotency_key_reuse", "timeout"), empty on success.
type Result struct {
	Outcome    Outcome
	HTTPStatus int // 0 without a response
	MessageID  string
	RetryAfter time.Duration // rate_limited only
	ErrorCode  string
	// Duration is the request's wall time measured by SendGated, from
	// building the request to reading the response (metrics only; the
	// clock is read before the gate, never between the gate and Do). It is
	// zero when no HTTP request was issued (e.g. our own request could not
	// be built).
	Duration time.Duration
}

// Capabilities is what an adapter declares about its provider (§10.6):
// idempotency is a provider capability we cannot enforce ourselves.
type Capabilities struct {
	Name string
	// IdempotencyKeys: the provider deduplicates requests with the same
	// Idempotency-Key and the same payload (and rejects a reused key with a
	// different payload).
	IdempotencyKeys bool
	// KeyRetention is how long the provider remembers a key.
	KeyRetention time.Duration
}

// RetentionProcessLifetime: keys are remembered for the provider process's
// lifetime (the mock's in-memory ledger; restarting it forgets them). It is
// a marker, not a duration: it says nothing about how long the process
// lives, so it never satisfies a retry-horizon comparison by itself.
const RetentionProcessLifetime = time.Duration(math.MaxInt64)

// RetentionUntilReset: keys are kept in a durable ledger across process
// restarts until that ledger is explicitly destroyed (the mock provider's
// journal on its volume). A marker like RetentionProcessLifetime: it holds
// only while the ledger is retained and intact.
const RetentionUntilReset = time.Duration(math.MaxInt64 - 1)

// MockCapabilities are the mock provider's declared capabilities (ADR-020):
// idempotency keys, retained in its durable ledger until an explicit reset.
var MockCapabilities = Capabilities{Name: "mock", IdempotencyKeys: true, KeyRetention: RetentionUntilReset}

// Response header and wire constants shared with the mock provider.
const (
	HeaderIdempotencyKey = "Idempotency-Key"
	HeaderReplayed       = "Idempotent-Replayed"
	HeaderRetryAfter     = "Retry-After"
	PathSMS              = "/v1/sms"
	PathEmail            = "/v1/email"
	// CodeIdempotencyKeyReuse is the 422 error code for a key reused with a
	// different payload.
	CodeIdempotencyKeyReuse = "idempotency_key_reuse"
)

// SMSRequest and EmailRequest are the JSON request bodies.
type SMSRequest struct {
	To   string `json:"to"`
	Text string `json:"text"`
}

// EmailRequest is the body of POST /v1/email.
type EmailRequest struct {
	To      string `json:"to"`
	Subject string `json:"subject"`
	Text    string `json:"text"`
}

// AcceptedResponse is the 2xx response body.
type AcceptedResponse struct {
	MessageID string `json:"message_id"`
	Status    string `json:"status"`
}

// ErrorResponse is the error response body.
type ErrorResponse struct {
	Error struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

// Defaults and bounds.
const (
	DefaultRequestTimeout = 5 * time.Second
	DefaultRetryAfter     = time.Second
	DefaultRetryAfterCap  = 30 * time.Second
	maxResponseBytes      = 64 << 10
	maxMessageIDBytes     = 128
)

// ClientConfig configures the client.
type ClientConfig struct {
	// BaseURL is PROVIDER_BASE_URL (http/https, no credentials, query or
	// fragment).
	BaseURL string
	// RequestTimeout bounds one request including reading the response
	// (PROVIDER_REQUEST_TIMEOUT).
	RequestTimeout time.Duration
	// RetryAfterCap caps a 429's Retry-After (RETRY_AFTER_CAP).
	RetryAfterCap time.Duration
	// MaxConnsPerHost sizes the keep-alive pool (≥ the sender pool size).
	MaxConnsPerHost int
	// Capabilities declared for the configured provider; zero means
	// MockCapabilities (the only provider in scope).
	Capabilities Capabilities
	// Transport overrides the HTTP transport (tests).
	Transport http.RoundTripper
}

// Client sends messages to the provider.
type Client struct {
	base          *url.URL
	http          *http.Client
	timeout       time.Duration
	retryAfterCap time.Duration
	caps          Capabilities
}

// ValidateBaseURL checks a provider base URL.
func ValidateBaseURL(raw string) (*url.URL, error) {
	u, err := url.Parse(raw)
	switch {
	case err != nil:
		return nil, errors.New("must be an absolute URL")
	case u.Scheme != "http" && u.Scheme != "https":
		return nil, errors.New("scheme must be http or https")
	case u.Host == "" || u.Opaque != "":
		return nil, errors.New("must have a host")
	case u.User != nil:
		return nil, errors.New("must not contain credentials")
	case u.RawQuery != "" || u.Fragment != "" || u.ForceQuery:
		return nil, errors.New("must not contain a query or fragment")
	}
	u.Path = strings.TrimSuffix(u.Path, "/")
	return u, nil
}

// NewClient returns a client.
func NewClient(cfg ClientConfig) (*Client, error) {
	base, err := ValidateBaseURL(cfg.BaseURL)
	if err != nil {
		return nil, fmt.Errorf("provider: base URL %w", err)
	}
	if cfg.RequestTimeout <= 0 {
		cfg.RequestTimeout = DefaultRequestTimeout
	}
	if cfg.RetryAfterCap <= 0 {
		cfg.RetryAfterCap = DefaultRetryAfterCap
	}
	if cfg.MaxConnsPerHost <= 0 {
		cfg.MaxConnsPerHost = 32
	}
	if cfg.Capabilities == (Capabilities{}) {
		cfg.Capabilities = MockCapabilities
	}
	rt := cfg.Transport
	if rt == nil {
		t := http.DefaultTransport.(*http.Transport).Clone()
		t.MaxIdleConns = 2 * cfg.MaxConnsPerHost
		t.MaxIdleConnsPerHost = cfg.MaxConnsPerHost
		t.ResponseHeaderTimeout = cfg.RequestTimeout
		t.Proxy = nil // the provider is reached directly, never through an environment proxy
		rt = t
	}
	return &Client{
		base:          base,
		http:          &http.Client{Transport: rt, CheckRedirect: noRedirects},
		timeout:       cfg.RequestTimeout,
		retryAfterCap: cfg.RetryAfterCap,
		caps:          cfg.Capabilities,
	}, nil
}

func noRedirects(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }

// Capabilities returns the declared provider capabilities.
func (c *Client) Capabilities() Capabilities { return c.caps }

// RequestTimeout returns the configured per-request timeout.
func (c *Client) RequestTimeout() time.Duration { return c.timeout }

// Send performs one request and classifies the result. It never returns an
// error: every failure is an Outcome. The request is bounded by the
// request timeout; cancelling ctx aborts it (OutcomeUnknown, since the
// request may already have reached the provider).
func (c *Client) Send(ctx context.Context, m Message) Result {
	r, _ := c.SendGated(ctx, m, nil)
	return r
}

// SendGated is Send with a final gate: gate (if not nil) is called after
// the request has been built — immediately before http.Client.Do, with no
// other work in between — and if it returns false nothing is sent and sent
// is false. The delivery worker's slot-deadline check runs here (§10.2
// step 4), so the time between that check and the request is only the
// transport's own. A request that could not be built is reported as a
// permanent error with sent = true (nothing reached the provider, but the
// attempt is final).
func (c *Client) SendGated(ctx context.Context, m Message, gate func() bool) (r Result, sent bool) {
	start := time.Now()
	issued := false
	defer func() {
		if issued {
			r.Duration = time.Since(start)
		}
	}()
	req, err := c.newRequest(ctx, m)
	if err != nil {
		// Our own request could not be built: nothing was sent.
		return Result{Outcome: OutcomePermanentError, ErrorCode: "invalid_request"}, true
	}
	rctx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()
	req = req.WithContext(rctx)

	if gate != nil && !gate() {
		return Result{}, false
	}
	resp, err := c.http.Do(req) // the only network call of a delivery attempt
	issued = true
	if err != nil {
		return classifyTransportError(ctx, rctx, err), true
	}
	defer resp.Body.Close()
	body, rerr := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))
	return c.classifyResponse(rctx, resp, body, rerr), true
}

func (c *Client) newRequest(ctx context.Context, m Message) (*http.Request, error) {
	var (
		path    string
		payload any
	)
	switch m.Channel {
	case ChannelSMS:
		path, payload = PathSMS, SMSRequest{To: m.To, Text: m.Text}
	case ChannelEmail:
		path, payload = PathEmail, EmailRequest{To: m.To, Subject: m.Subject, Text: m.Text}
	default:
		return nil, fmt.Errorf("unknown channel %q", m.Channel)
	}
	if m.IdempotencyKey == "" || m.To == "" {
		return nil, errors.New("idempotency key and destination are required")
	}
	data, err := json.Marshal(payload) // deterministic: same message, same bytes
	if err != nil {
		return nil, err
	}
	u := *c.base
	u.Path += path
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u.String(), bytes.NewReader(data))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set(HeaderIdempotencyKey, m.IdempotencyKey)
	return req, nil
}

// classifyTransportError maps a failed http.Client.Do. Only a failure to
// establish the connection proves that nothing reached the provider.
func classifyTransportError(parent, rctx context.Context, err error) Result {
	var opErr *net.OpError
	switch {
	case parent.Err() != nil:
		// Aborted by our side (shutdown) while the request may have been
		// on the wire.
		return Result{Outcome: OutcomeUnknown, ErrorCode: "aborted"}
	case errors.Is(err, context.DeadlineExceeded) || rctx.Err() != nil || isTimeout(err):
		return Result{Outcome: OutcomeTimeout, ErrorCode: "timeout"}
	case errors.As(err, &opErr) && opErr.Op == "dial":
		return Result{Outcome: OutcomeTransientError, ErrorCode: "connection_failed"}
	default:
		return Result{Outcome: OutcomeUnknown, ErrorCode: "connection_error"}
	}
}

func isTimeout(err error) bool {
	var ne net.Error
	return errors.As(err, &ne) && ne.Timeout()
}

func (c *Client) classifyResponse(rctx context.Context, resp *http.Response, body []byte, readErr error) Result {
	r := Result{HTTPStatus: resp.StatusCode}
	switch {
	case resp.StatusCode >= 200 && resp.StatusCode < 300:
		var ok AcceptedResponse
		if readErr != nil || json.Unmarshal(body, &ok) != nil || ok.MessageID == "" || len(ok.MessageID) > maxMessageIDBytes {
			// Accepted, but we cannot record what: retry with the same key
			// (an idempotent provider replays the original id).
			r.Outcome, r.ErrorCode = OutcomeUnknown, "unreadable_response"
			if rctx.Err() != nil {
				r.Outcome, r.ErrorCode = OutcomeTimeout, "timeout"
			}
			return r
		}
		r.MessageID = ok.MessageID
		r.Outcome = OutcomeAccepted
		if strings.EqualFold(resp.Header.Get(HeaderReplayed), "true") {
			r.Outcome = OutcomeReplayed
		}
		return r
	case resp.StatusCode == http.StatusTooManyRequests:
		r.Outcome, r.ErrorCode = OutcomeRateLimited, "rate_limited"
		r.RetryAfter = ParseRetryAfter(resp.Header.Get(HeaderRetryAfter), time.Now(), c.retryAfterCap)
		return r
	case resp.StatusCode >= 500:
		r.Outcome, r.ErrorCode = OutcomeTransientError, errorCode(resp.StatusCode, body)
		return r
	default: // other 4xx (and unexpected 1xx/3xx): permanent
		r.Outcome, r.ErrorCode = OutcomePermanentError, errorCode(resp.StatusCode, body)
		return r
	}
}

// errorCode extracts the provider's error code if it is a short
// [a-z0-9_] identifier; otherwise http_<status>. Provider messages are never
// copied (they could echo request data).
func errorCode(status int, body []byte) string {
	var e ErrorResponse
	if json.Unmarshal(body, &e) == nil && validCode(e.Error.Code) {
		return e.Error.Code
	}
	return "http_" + strconv.Itoa(status)
}

func validCode(s string) bool {
	if s == "" || len(s) > 64 {
		return false
	}
	for _, r := range s {
		if (r < 'a' || r > 'z') && (r < '0' || r > '9') && r != '_' {
			return false
		}
	}
	return true
}

// ParseRetryAfter parses a Retry-After header (delay-seconds or HTTP-date,
// RFC 9110 §10.2.3) relative to now. Missing, invalid, non-positive or
// past values mean DefaultRetryAfter (1 s); the result is at least
// DefaultRetryAfter and capped at limit (RETRY_AFTER_CAP), so a malformed or
// hostile value can neither busy-loop the worker nor stall a channel for
// longer than the cap. A delay-seconds value too large for int64 is the cap.
func ParseRetryAfter(v string, now time.Time, limit time.Duration) time.Duration {
	d := DefaultRetryAfter
	v = strings.TrimSpace(v)
	if v != "" && strings.Trim(v, "0123456789") == "" { // delay-seconds = 1*DIGIT
		n, err := strconv.ParseInt(v, 10, 64)
		switch {
		case err != nil: // too large to represent: as long as we allow
			d = limit
		case n > int64(limit/time.Second):
			d = limit
		case n > 0:
			d = time.Duration(n) * time.Second
		}
	} else if t, err := http.ParseTime(v); err == nil {
		if until := t.Sub(now); until > 0 {
			d = until
		}
	}
	if d < DefaultRetryAfter {
		d = DefaultRetryAfter
	}
	if d > limit {
		d = limit
	}
	return d
}
