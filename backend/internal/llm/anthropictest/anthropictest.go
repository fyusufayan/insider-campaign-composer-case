// Package anthropictest provides an offline http.RoundTripper that stands in
// for the Anthropic Messages API in tests: it records every outgoing request
// (method, URL, headers, raw JSON body) and answers with scripted,
// Anthropic-shaped responses. It never opens a network connection. Test-only;
// production packages must not import it.
package anthropictest

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"sync"
)

// ErrNoMoreReplies is returned when the transport receives more requests than
// scripted replies.
var ErrNoMoreReplies = errors.New("anthropictest: no more scripted replies")

// Reply is one scripted answer. Exactly one form applies:
//   - Err set: the round trip fails with Err (a transport failure);
//   - Block set: the round trip waits until the request context ends;
//   - otherwise an HTTP response with Status (default 200), Body and Header.
//     With HoldOpen the body delivers Body and then blocks until the request
//     context ends (a stream that stalls mid-response).
type Reply struct {
	Status   int
	Body     string
	Header   http.Header
	Err      error
	Block    bool
	HoldOpen bool
}

// Captured is one recorded request.
type Captured struct {
	Method string
	URL    string
	Header http.Header
	Body   []byte
}

// JSON decodes the captured body.
func (c Captured) JSON() (map[string]any, error) {
	var m map[string]any
	err := json.Unmarshal(c.Body, &m)
	return m, err
}

// Transport plays Replies in order. Safe for concurrent use.
type Transport struct {
	mu       sync.Mutex
	replies  []Reply
	requests []Captured
}

// New returns a Transport playing replies in order.
func New(replies ...Reply) *Transport {
	return &Transport{replies: append([]Reply(nil), replies...)}
}

// Client returns an *http.Client using the transport.
func (t *Transport) Client() *http.Client { return &http.Client{Transport: t} }

// Requests returns the recorded requests.
func (t *Transport) Requests() []Captured {
	t.mu.Lock()
	defer t.mu.Unlock()
	return append([]Captured(nil), t.requests...)
}

// Calls returns the number of requests received.
func (t *Transport) Calls() int {
	t.mu.Lock()
	defer t.mu.Unlock()
	return len(t.requests)
}

// RoundTrip implements http.RoundTripper.
func (t *Transport) RoundTrip(req *http.Request) (*http.Response, error) {
	var body []byte
	if req.Body != nil {
		b, err := io.ReadAll(req.Body)
		_ = req.Body.Close()
		if err != nil {
			return nil, err
		}
		body = b
	}
	t.mu.Lock()
	t.requests = append(t.requests, Captured{Method: req.Method, URL: req.URL.String(), Header: req.Header.Clone(), Body: body})
	n := len(t.requests)
	if len(t.replies) == 0 {
		t.mu.Unlock()
		return nil, fmt.Errorf("%w (request %d)", ErrNoMoreReplies, n)
	}
	r := t.replies[0]
	t.replies = t.replies[1:]
	t.mu.Unlock()

	switch {
	case r.Err != nil:
		return nil, r.Err
	case r.Block:
		<-req.Context().Done()
		return nil, req.Context().Err()
	}
	status := r.Status
	if status == 0 {
		status = http.StatusOK
	}
	h := r.Header.Clone()
	if h == nil {
		h = http.Header{}
	}
	if h.Get("Content-Type") == "" {
		h.Set("Content-Type", "application/json")
	}
	var respBody io.ReadCloser = io.NopCloser(bytes.NewReader([]byte(r.Body)))
	length := int64(len(r.Body))
	if r.HoldOpen {
		respBody = &holdOpenBody{r: bytes.NewReader([]byte(r.Body)), done: req.Context().Done(), err: req.Context().Err}
		length = -1
	}
	return &http.Response{
		Status:        strconv.Itoa(status) + " " + http.StatusText(status),
		StatusCode:    status,
		Proto:         "HTTP/1.1",
		ProtoMajor:    1,
		ProtoMinor:    1,
		Header:        h,
		Body:          respBody,
		ContentLength: length,
		Request:       req,
	}, nil
}

// holdOpenBody returns its content, then blocks until done is closed.
type holdOpenBody struct {
	r    *bytes.Reader
	done <-chan struct{}
	err  func() error
}

func (b *holdOpenBody) Read(p []byte) (int, error) {
	if b.r.Len() > 0 {
		return b.r.Read(p)
	}
	<-b.done
	return 0, b.err()
}

func (b *holdOpenBody) Close() error { return nil }

// Block is a content block of a scripted Message.
type Block map[string]any

// Text is a text content block.
func Text(s string) Block { return Block{"type": "text", "text": s, "citations": nil} }

// Thinking is a thinking block with omitted (empty) display text.
func Thinking() Block { return Block{"type": "thinking", "thinking": "", "signature": "sig"} }

// ToolUse is a tool_use block (never requested by the application).
func ToolUse() Block {
	return Block{"type": "tool_use", "id": "toolu_1", "name": "x", "input": map[string]any{}}
}

// Message returns a Messages API response body.
func Message(model, stopReason string, inputTokens, outputTokens int, blocks ...Block) string {
	content := make([]any, len(blocks))
	for i, b := range blocks {
		content[i] = map[string]any(b)
	}
	var stopDetails any
	if stopReason == "refusal" {
		stopDetails = map[string]any{"type": "refusal", "category": "cyber", "explanation": "declined"}
	}
	b, err := json.Marshal(map[string]any{
		"id": "msg_test", "type": "message", "role": "assistant", "model": model,
		"content": content, "stop_reason": stopReason, "stop_sequence": nil, "stop_details": stopDetails,
		"usage": map[string]any{
			"input_tokens": inputTokens, "output_tokens": outputTokens,
			"cache_creation_input_tokens": 0, "cache_read_input_tokens": 0,
		},
	})
	if err != nil {
		panic(err) // static data
	}
	return string(b)
}

// OK is a 200 reply with body and a request-id header.
func OK(requestID, body string) Reply {
	return Reply{Status: http.StatusOK, Body: body, Header: http.Header{"Request-Id": {requestID}}}
}

// APIError is an error reply in the Anthropic error envelope. retry-after-ms
// is 0 so SDK retries of retryable statuses happen without a backoff wait.
func APIError(status int, errType, message, requestID string) Reply {
	b, err := json.Marshal(map[string]any{
		"type": "error", "error": map[string]any{"type": errType, "message": message}, "request_id": requestID,
	})
	if err != nil {
		panic(err)
	}
	return Reply{Status: status, Body: string(b), Header: http.Header{
		"Request-Id":     {requestID},
		"Retry-After-Ms": {"0"},
	}}
}

// ---- Streaming (text/event-stream) ----

// SSEEvent is one server-sent event of a scripted Messages stream.
type SSEEvent struct {
	Name string
	Data any // marshalled to JSON
}

// SSE returns a 200 text/event-stream reply with the events in order.
func SSE(requestID string, events ...SSEEvent) Reply {
	var b bytes.Buffer
	for _, ev := range events {
		data, err := json.Marshal(ev.Data)
		if err != nil {
			panic(err) // static data
		}
		fmt.Fprintf(&b, "event: %s\ndata: %s\n\n", ev.Name, data)
	}
	return Reply{Status: http.StatusOK, Body: b.String(), Header: http.Header{
		"Request-Id":   {requestID},
		"Content-Type": {"text/event-stream"},
	}}
}

// MessageStart is the message_start event.
func MessageStart(model string, inputTokens int) SSEEvent {
	return SSEEvent{Name: "message_start", Data: map[string]any{"type": "message_start", "message": map[string]any{
		"id": "msg_stream", "type": "message", "role": "assistant", "model": model, "content": []any{},
		"stop_reason": nil, "stop_sequence": nil, "stop_details": nil,
		"usage": map[string]any{"input_tokens": inputTokens, "output_tokens": 1,
			"cache_creation_input_tokens": 0, "cache_read_input_tokens": 0},
	}}}
}

// BlockStart starts content block index with the given (empty) block.
func BlockStart(index int, block Block) SSEEvent {
	return SSEEvent{Name: "content_block_start", Data: map[string]any{
		"type": "content_block_start", "index": index, "content_block": map[string]any(block)}}
}

// TextBlockStart starts an empty text block.
func TextBlockStart(index int) SSEEvent { return BlockStart(index, Text("")) }

// TextDelta appends text to block index.
func TextDelta(index int, text string) SSEEvent {
	return SSEEvent{Name: "content_block_delta", Data: map[string]any{
		"type": "content_block_delta", "index": index, "delta": map[string]any{"type": "text_delta", "text": text}}}
}

// ThinkingDelta appends (omitted, empty) thinking text to block index.
func ThinkingDelta(index int) SSEEvent {
	return SSEEvent{Name: "content_block_delta", Data: map[string]any{
		"type": "content_block_delta", "index": index, "delta": map[string]any{"type": "signature_delta", "signature": "sig"}}}
}

// BlockStop stops block index.
func BlockStop(index int) SSEEvent {
	return SSEEvent{Name: "content_block_stop", Data: map[string]any{"type": "content_block_stop", "index": index}}
}

// MessageDelta carries the stop reason and the final output token count.
func MessageDelta(stopReason string, outputTokens int) SSEEvent {
	var stopDetails any
	if stopReason == "refusal" {
		stopDetails = map[string]any{"type": "refusal", "category": "cyber", "explanation": "declined"}
	}
	return SSEEvent{Name: "message_delta", Data: map[string]any{
		"type":  "message_delta",
		"delta": map[string]any{"stop_reason": stopReason, "stop_sequence": nil, "stop_details": stopDetails},
		"usage": map[string]any{"output_tokens": outputTokens},
	}}
}

// MessageStop is the message_stop event.
func MessageStop() SSEEvent {
	return SSEEvent{Name: "message_stop", Data: map[string]any{"type": "message_stop"}}
}

// Ping is a keep-alive event.
func Ping() SSEEvent { return SSEEvent{Name: "ping", Data: map[string]any{"type": "ping"}} }

// StreamError is an in-stream error event (sent after the 200 response
// started).
func StreamError(errType, message string) SSEEvent {
	return SSEEvent{Name: "error", Data: map[string]any{
		"type": "error", "error": map[string]any{"type": errType, "message": message}}}
}

// TextStream is a complete, successful stream: an optional thinking block,
// then one text block delivered in chunks.
func TextStream(requestID, model, stopReason string, withThinking bool, chunks ...string) Reply {
	evs := []SSEEvent{MessageStart(model, 11)}
	idx := 0
	if withThinking {
		evs = append(evs, BlockStart(0, Thinking()), ThinkingDelta(0), BlockStop(0), Ping())
		idx = 1
	}
	evs = append(evs, TextBlockStart(idx))
	for _, c := range chunks {
		evs = append(evs, TextDelta(idx, c))
	}
	evs = append(evs, BlockStop(idx), MessageDelta(stopReason, 42), MessageStop())
	return SSE(requestID, evs...)
}
