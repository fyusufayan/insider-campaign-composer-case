package llm

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/fyusufayan/insider-campaign-composer-case/backend/internal/llm/anthropictest"
)

// All tests here are offline: the SDK talks to anthropictest.Transport, which
// records requests and never opens a connection.

const (
	testAPIKey   = "sk-ant-api03-SECRET-test-key"
	testModelCfg = "claude-opus-5-5"
	servedModel  = "claude-opus-5-5-served"
	// Markers that must never appear in logs or error messages.
	secretSystem = "SYSTEM-PROMPT-MARKER"
	secretBrief  = "BRIEF-MARKER Istanbul customers"
	secretOutput = "MODEL-OUTPUT-MARKER"
	secretBody   = "ERROR-BODY-MARKER"
)

func testSchema() map[string]any {
	return map[string]any{
		"type":                 "object",
		"additionalProperties": false,
		"required":             []any{"segment", "notes"},
		"properties": map[string]any{
			"segment": map[string]any{"$ref": "#/$defs/rule"},
			"notes":   map[string]any{"type": "array", "items": map[string]any{"type": "string"}},
		},
		"$defs": map[string]any{
			"rule": map[string]any{
				"type": "object", "additionalProperties": false, "required": []any{"match"},
				"properties": map[string]any{
					"match": map[string]any{"type": "string", "enum": []any{"all", "any"}},
					"value": map[string]any{"anyOf": []any{map[string]any{"type": "string"}, map[string]any{"type": "integer"}}},
				},
			},
		},
	}
}

func testRequest() Request {
	return Request{
		Task:   "segment_interpretation",
		Model:  testModelCfg,
		System: secretSystem + " system instructions",
		Messages: []Message{
			{Role: RoleUser, Content: "<brief>" + secretBrief + "</brief>"},
			{Role: RoleAssistant, Content: `{"invalid":"proposal"}`},
			{Role: RoleUser, Content: "Fix these errors: segment is required"},
		},
		JSONSchema: testSchema(),
		MaxTokens:  4000,
		Effort:     EffortLow,
	}
}

type fixture struct {
	t    *testing.T
	tr   *anthropictest.Transport
	a    *Anthropic
	logs *bytes.Buffer
}

// newFixture builds an adapter over the scripted transport, a JSON logger and
// a fake clock that advances 1.5s per reading (latency = 1.5s per call).
func newFixture(t *testing.T, retries int, replies ...anthropictest.Reply) *fixture {
	t.Helper()
	tr := anthropictest.New(replies...)
	logs := &bytes.Buffer{}
	a, err := NewAnthropic(AnthropicConfig{
		APIKey: testAPIKey, MaxRetries: retries, HTTPClient: tr.Client(),
		Logger: slog.New(slog.NewJSONHandler(logs, nil)),
	})
	if err != nil {
		t.Fatal(err)
	}
	clock := time.Date(2026, 11, 4, 12, 0, 0, 0, time.UTC)
	a.now = func() time.Time {
		clock = clock.Add(1500 * time.Millisecond)
		return clock
	}
	return &fixture{t: t, tr: tr, a: a, logs: logs}
}

func (f *fixture) body(i int) map[string]any {
	f.t.Helper()
	reqs := f.tr.Requests()
	if len(reqs) <= i {
		f.t.Fatalf("only %d requests captured", len(reqs))
	}
	m, err := reqs[i].JSON()
	if err != nil {
		f.t.Fatalf("request body is not JSON: %v", err)
	}
	return m
}

// assertClean checks that no secret, prompt, brief, output or provider error
// body reached the logs or the given error text.
func (f *fixture) assertClean(extra ...string) {
	f.t.Helper()
	for _, where := range append([]string{f.logs.String()}, extra...) {
		for _, s := range []string{testAPIKey, "SECRET", secretSystem, secretBrief, secretOutput, secretBody} {
			if strings.Contains(where, s) {
				f.t.Errorf("leaked %q in %q", s, where)
			}
		}
	}
}

func okMessage(text string) anthropictest.Reply {
	return anthropictest.OK("req_ok_1", anthropictest.Message(servedModel, "end_turn", 1234, 56, anthropictest.Thinking(), anthropictest.Text(text)))
}

// 1–4: the serialized HTTP request — endpoint, headers, model, max_tokens,
// system, roles and the full repair history, thinking/effort, and the
// structured-output schema byte-for-byte.
func TestAnthropicRequestWireFormat(t *testing.T) {
	f := newFixture(t, 2, okMessage(`{"ok":true}`))
	req := testRequest()
	schemaBefore, _ := json.Marshal(req.JSONSchema)

	if _, err := f.a.Complete(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	r := f.tr.Requests()[0]
	if r.Method != http.MethodPost || r.URL != "https://api.anthropic.com/v1/messages" {
		t.Errorf("endpoint = %s %s", r.Method, r.URL)
	}
	// SDK v2 sends API keys as a bearer credential (option.WithAPIKey).
	if got := r.Header.Get("Authorization"); got != "Bearer "+testAPIKey {
		t.Errorf("authorization = %q", got)
	}
	if got := r.Header.Get("X-Api-Key"); got != "" {
		t.Errorf("unexpected x-api-key header %q", got)
	}
	if got := r.Header.Get("Anthropic-Version"); got != "2026-10-21" {
		t.Errorf("anthropic-version = %q", got)
	}
	if got := r.Header.Get("Anthropic-Beta"); got != "" {
		t.Errorf("a beta header was sent: %q", got)
	}

	body := f.body(0)
	keys := make([]string, 0, len(body))
	for k := range body {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	// Exactly these top-level fields: no temperature/top_p/top_k, tools,
	// tool_choice, stop_sequences, metadata or beta-only fields.
	if want := []string{"max_tokens", "messages", "model", "output_config", "system", "thinking"}; !slices.Equal(keys, want) {
		t.Errorf("top-level keys = %v, want %v", keys, want)
	}
	if body["model"] != testModelCfg || body["max_tokens"] != float64(4000) {
		t.Errorf("model/max_tokens = %v/%v", body["model"], body["max_tokens"])
	}
	wantSystem := []any{map[string]any{"type": "text", "text": req.System}}
	if !reflect.DeepEqual(body["system"], wantSystem) {
		t.Errorf("system = %#v", body["system"])
	}
	wantMsgs := make([]any, len(req.Messages))
	for i, m := range req.Messages {
		wantMsgs[i] = map[string]any{"role": string(m.Role), "content": []any{map[string]any{"type": "text", "text": m.Content}}}
	}
	if !reflect.DeepEqual(body["messages"], wantMsgs) {
		t.Errorf("messages = %#v\nwant %#v", body["messages"], wantMsgs)
	}
	if !reflect.DeepEqual(body["thinking"], map[string]any{"type": "adaptive"}) {
		t.Errorf("thinking = %#v", body["thinking"])
	}

	oc, _ := body["output_config"].(map[string]any)
	if oc["effort"] != "low" {
		t.Errorf("output_config.effort = %v", oc["effort"])
	}
	format, _ := oc["format"].(map[string]any)
	if format["type"] != "json_schema" || len(format) != 2 {
		t.Errorf("output_config.format = %#v", format)
	}
	var wantSchema any
	_ = json.Unmarshal(schemaBefore, &wantSchema)
	if !reflect.DeepEqual(format["schema"], wantSchema) {
		t.Errorf("schema changed on the wire:\n got %#v\nwant %#v", format["schema"], wantSchema)
	}
	if after, _ := json.Marshal(req.JSONSchema); !bytes.Equal(after, schemaBefore) {
		t.Error("the request schema was mutated")
	}
	f.assertClean()
}

func TestAnthropicRequestWithoutSchemaOrSystem(t *testing.T) {
	f := newFixture(t, 0, okMessage("plain"))
	req := Request{Model: testModelCfg, MaxTokens: 10, Effort: EffortMax, Messages: []Message{{Role: RoleUser, Content: "hi"}}}
	if _, err := f.a.Complete(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	body := f.body(0)
	if _, ok := body["system"]; ok {
		t.Error("empty system was sent")
	}
	if !reflect.DeepEqual(body["output_config"], map[string]any{"effort": "max"}) {
		t.Errorf("output_config = %#v", body["output_config"])
	}
}

func TestAnthropicEffortMapping(t *testing.T) {
	for _, e := range []Effort{EffortLow, EffortMedium, EffortHigh, EffortMax} {
		f := newFixture(t, 0, okMessage("x"))
		req := testRequest()
		req.Effort = e
		if _, err := f.a.Complete(context.Background(), req); err != nil {
			t.Fatal(err)
		}
		if got := f.body(0)["output_config"].(map[string]any)["effort"]; got != string(e) {
			t.Errorf("effort %q sent as %v", e, got)
		}
	}
}

// Invalid requests fail as ErrRejected before anything is sent.
func TestAnthropicRejectsInvalidRequestsLocally(t *testing.T) {
	mod := func(fn func(*Request)) Request { r := testRequest(); fn(&r); return r }
	tests := map[string]Request{
		"no model":          mod(func(r *Request) { r.Model = " " }),
		"no max tokens":     mod(func(r *Request) { r.MaxTokens = 0 }),
		"no messages":       mod(func(r *Request) { r.Messages = nil }),
		"assistant prefill": mod(func(r *Request) { r.Messages = r.Messages[:2] }),
		"empty content":     mod(func(r *Request) { r.Messages[0].Content = "" }),
		"unknown role":      mod(func(r *Request) { r.Messages[0].Role = "system" }),
		"empty effort":      mod(func(r *Request) { r.Effort = "" }),
		"unsupported":       mod(func(r *Request) { r.Effort = "xhigh" }),
	}
	for name, req := range tests {
		t.Run(name, func(t *testing.T) {
			f := newFixture(t, 2)
			_, err := f.a.Complete(context.Background(), req)
			if !errors.Is(err, ErrRejected) || Retryable(err) {
				t.Fatalf("err = %v", err)
			}
			if f.tr.Calls() != 0 {
				t.Errorf("%d requests sent", f.tr.Calls())
			}
			f.assertClean(err.Error())
		})
	}
}

// 5–6: text extraction and response metadata.
func TestAnthropicResponseMapping(t *testing.T) {
	body := anthropictest.Message(servedModel, "end_turn", 1234, 56,
		anthropictest.Thinking(), anthropictest.Text(`{"segment":`), anthropictest.Text(`"`+secretOutput+`"}`))
	f := newFixture(t, 2, anthropictest.OK("req_abc123", body))
	resp, err := f.a.Complete(context.Background(), testRequest())
	if err != nil {
		t.Fatal(err)
	}
	want := Response{
		Text: `{"segment":"` + secretOutput + `"}`, StopReason: StopEndTurn, Model: servedModel,
		Usage: Usage{InputTokens: 1234, OutputTokens: 56}, Latency: 1500 * time.Millisecond, RequestID: "req_abc123",
	}
	if resp != want {
		t.Errorf("response = %+v\nwant %+v", resp, want)
	}
	logs := f.logs.String()
	for _, s := range []string{`"provider_request_id":"req_abc123"`, `"stop_reason":"end_turn"`, `"input_tokens":1234`, `"latency_ms":1500`, `"model":"` + servedModel + `"`} {
		if !strings.Contains(logs, s) {
			t.Errorf("log lacks %s: %s", s, logs)
		}
	}
	f.assertClean()
}

// 7–8: refusal, max_tokens and other stop reasons are returned (not errors)
// so the interpreter's policy applies; only end_turn is a complete answer.
func TestAnthropicStopReasons(t *testing.T) {
	tests := []struct {
		stop string
		want StopReason
	}{
		{"end_turn", StopEndTurn},
		{"refusal", StopRefusal},
		{"max_tokens", StopMaxTokens},
		{"stop_sequence", StopStopSequence},
		{"pause_turn", "pause_turn"},
		{"model_context_window_exceeded", "model_context_window_exceeded"},
	}
	for _, tt := range tests {
		t.Run(tt.stop, func(t *testing.T) {
			f := newFixture(t, 0, anthropictest.OK("req_s", anthropictest.Message(servedModel, tt.stop, 10, 5, anthropictest.Text(`{"partial`))))
			resp, err := f.a.Complete(context.Background(), testRequest())
			if err != nil || resp.StopReason != tt.want {
				t.Fatalf("resp = %+v, err = %v", resp, err)
			}
		})
	}
	// A refusal may carry no text at all.
	f := newFixture(t, 0, anthropictest.OK("req_r", anthropictest.Message(servedModel, "refusal", 10, 0)))
	if resp, err := f.a.Complete(context.Background(), testRequest()); err != nil || resp.StopReason != StopRefusal {
		t.Fatalf("empty refusal: %+v, %v", resp, err)
	}
}

// 9: response shapes that are not a plain structured answer are never used.
func TestAnthropicUnexpectedResponseShapes(t *testing.T) {
	tests := map[string]anthropictest.Reply{
		"tool_use block":   anthropictest.OK("req_u", anthropictest.Message(servedModel, "end_turn", 1, 1, anthropictest.Text("{}"), anthropictest.ToolUse())),
		"no text end_turn": anthropictest.OK("req_u", anthropictest.Message(servedModel, "end_turn", 1, 1, anthropictest.Thinking())),
		"malformed JSON":   {Status: 200, Body: `{"content":[{"type":"text","text":"` + secretOutput, Header: http.Header{"Request-Id": {"req_u"}}},
		"not JSON":         {Status: 200, Body: "<html>" + secretOutput + "</html>", Header: http.Header{"Content-Type": {"text/html"}}},
	}
	for name, reply := range tests {
		t.Run(name, func(t *testing.T) {
			f := newFixture(t, 0, reply)
			resp, err := f.a.Complete(context.Background(), testRequest())
			if !errors.Is(err, ErrUnavailable) || resp.Text != "" {
				t.Fatalf("resp = %+v, err = %v", resp, err)
			}
			f.assertClean(err.Error())
		})
	}
}

// 10: HTTP error classification, request id, and the SDK's retry behaviour.
func TestAnthropicHTTPErrors(t *testing.T) {
	tests := []struct {
		status    int
		errType   string
		wantKind  error
		retriedBy bool // the SDK retries this status itself
	}{
		{400, "invalid_request_error", ErrRejected, false},
		{401, "authentication_error", ErrRejected, false},
		{403, "permission_error", ErrRejected, false},
		{404, "not_found_error", ErrRejected, false},
		{413, "request_too_large", ErrRejected, false},
		{408, "timeout_error", ErrUnavailable, true},
		{409, "conflict_error", ErrUnavailable, true},
		{429, "rate_limit_error", ErrUnavailable, true},
		{500, "api_error", ErrUnavailable, true},
		{503, "api_error", ErrUnavailable, true},
		{529, "overloaded_error", ErrUnavailable, true},
	}
	const retries = 2
	for _, tt := range tests {
		t.Run(http.StatusText(tt.status), func(t *testing.T) {
			reply := anthropictest.APIError(tt.status, tt.errType, secretBody+" "+secretBrief, "req_err_1")
			f := newFixture(t, retries, reply, reply, reply)
			_, err := f.a.Complete(context.Background(), testRequest())
			var e *Error
			if !errors.As(err, &e) || !errors.Is(err, tt.wantKind) {
				t.Fatalf("err = %v", err)
			}
			if e.StatusCode != tt.status || e.RequestID != "req_err_1" {
				t.Errorf("status/request id = %d/%q", e.StatusCode, e.RequestID)
			}
			if Retryable(err) != (tt.wantKind == ErrUnavailable) {
				t.Errorf("Retryable = %v", Retryable(err))
			}
			wantCalls := 1
			if tt.retriedBy {
				wantCalls = retries + 1
			}
			if f.tr.Calls() != wantCalls {
				t.Errorf("provider requests = %d, want %d", f.tr.Calls(), wantCalls)
			}
			if !strings.Contains(err.Error(), tt.errType) {
				t.Errorf("error lacks the API error type: %v", err)
			}
			f.assertClean(err.Error())
		})
	}
}

// A transient failure followed by success inside the SDK's retry budget.
func TestAnthropicSDKRetryRecovers(t *testing.T) {
	f := newFixture(t, 2, anthropictest.APIError(529, "overloaded_error", "busy", "req_1"), okMessage("{}"))
	resp, err := f.a.Complete(context.Background(), testRequest())
	if err != nil || resp.StopReason != StopEndTurn || f.tr.Calls() != 2 {
		t.Fatalf("resp = %+v, err = %v, calls = %d", resp, err, f.tr.Calls())
	}
	// Every attempt sends the identical body.
	reqs := f.tr.Requests()
	if !bytes.Equal(reqs[0].Body, reqs[1].Body) {
		t.Error("retry changed the request body")
	}
}

// MaxRetries 0 means exactly one attempt.
func TestAnthropicNoRetries(t *testing.T) {
	f := newFixture(t, 0, anthropictest.APIError(503, "api_error", "x", "req_1"), okMessage("{}"))
	if _, err := f.a.Complete(context.Background(), testRequest()); !errors.Is(err, ErrUnavailable) || f.tr.Calls() != 1 {
		t.Fatalf("err = %v, calls = %d", err, f.tr.Calls())
	}
}

// 11: network failure, deadline and cancellation.
func TestAnthropicNetworkFailure(t *testing.T) {
	const marker = "TRANSPORT-MARKER-pw"
	netErr := &net.OpError{Op: "proxyconnect", Net: "tcp",
		Err: errors.New("http://user:" + marker + "@proxy.internal:3128 refused: " + marker + " body bytes")}
	f := newFixture(t, 0, anthropictest.Reply{Err: netErr})
	_, err := f.a.Complete(context.Background(), testRequest())
	var e *Error
	if !errors.As(err, &e) || !errors.Is(err, ErrUnavailable) || e.StatusCode != 0 || !Retryable(err) {
		t.Fatalf("err = %v", err)
	}
	// Only the fixed message: no transport text, URL, proxy or credentials.
	if got := err.Error(); got != "llm: provider unavailable: anthropic transport failure" {
		t.Errorf("error = %q", got)
	}
	for _, where := range []string{err.Error(), f.logs.String()} {
		if strings.Contains(where, marker) || strings.Contains(where, "proxy.internal") || strings.Contains(where, "api.anthropic.com") {
			t.Errorf("transport details leaked: %q", where)
		}
	}
	if !strings.Contains(f.logs.String(), "anthropic transport failure") {
		t.Errorf("failure not logged: %s", f.logs.String())
	}
	f.assertClean(err.Error())
}

func TestAnthropicDeadline(t *testing.T) {
	f := newFixture(t, 2, anthropictest.Reply{Block: true}, anthropictest.Reply{Block: true}, anthropictest.Reply{Block: true})
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	_, err := f.a.Complete(ctx, testRequest())
	if !errors.Is(err, context.DeadlineExceeded) || !Retryable(err) {
		t.Fatalf("err = %v", err)
	}
	// The caller's deadline ends the call; the SDK does not retry past it.
	if f.tr.Calls() != 1 {
		t.Errorf("requests = %d", f.tr.Calls())
	}
}

func TestAnthropicCancellation(t *testing.T) {
	f := newFixture(t, 2, anthropictest.Reply{Block: true})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := f.a.Complete(ctx, testRequest())
		done <- err
	}()
	deadline := time.Now().Add(5 * time.Second)
	for f.tr.Calls() == 0 {
		if time.Now().After(deadline) {
			t.Fatal("request never sent")
		}
		time.Sleep(time.Millisecond)
	}
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) || Retryable(err) {
			t.Fatalf("err = %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Complete did not return after cancellation")
	}

	// An already-cancelled context sends nothing.
	f2 := newFixture(t, 2, okMessage("{}"))
	if _, err := f2.a.Complete(ctx, testRequest()); !errors.Is(err, context.Canceled) || f2.tr.Calls() != 0 {
		t.Fatalf("err = %v, calls = %d", err, f2.tr.Calls())
	}
}

// The SDK's environment autoload is disabled: the configured key is the only
// credential and ANTHROPIC_BASE_URL / custom headers are ignored.
func TestAnthropicIgnoresSDKEnvironment(t *testing.T) {
	t.Setenv("ANTHROPIC_BASE_URL", "http://attacker.invalid")
	t.Setenv("ANTHROPIC_AUTH_TOKEN", "other-token")
	t.Setenv("ANTHROPIC_CUSTOM_HEADERS", "X-Leak: yes")
	t.Setenv("ANTHROPIC_API_KEY", "env-key-not-used")
	f := newFixture(t, 0, okMessage("{}"))
	if _, err := f.a.Complete(context.Background(), testRequest()); err != nil {
		t.Fatal(err)
	}
	r := f.tr.Requests()[0]
	if !strings.HasPrefix(r.URL, "https://api.anthropic.com/") || r.Header.Get("Authorization") != "Bearer "+testAPIKey ||
		r.Header.Get("X-Leak") != "" || r.Header.Get("X-Api-Key") != "" {
		t.Errorf("request = %s %v", r.URL, r.Header)
	}
}

func TestNewAnthropicValidation(t *testing.T) {
	if _, err := NewAnthropic(AnthropicConfig{APIKey: "  "}); !errors.Is(err, ErrAnthropicNoAPIKey) {
		t.Errorf("blank key: %v", err)
	}
	for _, n := range []int{-1, MaxAnthropicRetries + 1} {
		if _, err := NewAnthropic(AnthropicConfig{APIKey: "k", MaxRetries: n}); !errors.Is(err, ErrAnthropicRetries) {
			t.Errorf("retries %d: %v", n, err)
		}
	}
	if _, err := NewAnthropic(AnthropicConfig{APIKey: "k"}); err != nil {
		t.Errorf("valid config: %v", err)
	}
}

func TestAnthropicStreamNotImplemented(t *testing.T) {
	f := newFixture(t, 0, okMessage("{}"))
	_, err := f.a.Stream(context.Background(), testRequest(), func(StreamEvent) error { return nil })
	if !errors.Is(err, ErrRejected) || f.tr.Calls() != 0 {
		t.Fatalf("err = %v, calls = %d", err, f.tr.Calls())
	}
}
