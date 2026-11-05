package llm

import (
	"bytes"
	"context"
	"errors"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/fyusufayan/insider-campaign-composer-case/backend/internal/llm/anthropictest"
)

// Offline tests of Anthropic.Stream: the SDK reads scripted text/event-stream
// bodies from anthropictest.Transport.

type recorded struct {
	kinds []StreamEventKind
	text  strings.Builder
}

func (r *recorded) on(ev StreamEvent) error {
	r.kinds = append(r.kinds, ev.Kind)
	if ev.Kind == EventTextDelta {
		r.text.WriteString(ev.Text)
	}
	return nil
}

func TestAnthropicStreamHappyPath(t *testing.T) {
	chunks := []string{`{"sms_bo`, `dy":"Hi {{first_`, `name|there}} – ş`, `ğü"}`}
	f := newFixture(t, 2, anthropictest.TextStream("req_stream_1", servedModel, "end_turn", true, chunks...))
	var rec recorded
	resp, err := f.a.Stream(context.Background(), testRequest(), rec.on)
	if err != nil {
		t.Fatal(err)
	}
	want := strings.Join(chunks, "")
	if resp.Text != want || rec.text.String() != want {
		t.Fatalf("text = %q / deltas = %q, want %q", resp.Text, rec.text.String(), want)
	}
	if resp.StopReason != StopEndTurn || resp.Model != servedModel || resp.RequestID != "req_stream_1" ||
		resp.Usage.InputTokens != 11 || resp.Usage.OutputTokens != 42 {
		t.Errorf("resp = %+v", resp)
	}
	// Thinking is reported once, before the first text delta.
	if len(rec.kinds) != 1+len(chunks) || rec.kinds[0] != EventThinking {
		t.Errorf("events = %v", rec.kinds)
	}
	for _, k := range rec.kinds[1:] {
		if k != EventTextDelta {
			t.Errorf("events = %v", rec.kinds)
		}
	}

	// The request is the same Messages request as Complete plus stream=true.
	body := f.body(0)
	if body["stream"] != true || body["model"] != testModelCfg {
		t.Errorf("stream/model = %v/%v", body["stream"], body["model"])
	}
	oc, _ := body["output_config"].(map[string]any)
	if oc == nil || oc["format"] == nil || oc["effort"] != "low" {
		t.Errorf("output_config = %v", body["output_config"])
	}
	for _, k := range []string{"temperature", "top_p", "top_k", "tool_choice", "tools"} {
		if _, ok := body[k]; ok {
			t.Errorf("%s sent", k)
		}
	}
	if !strings.Contains(f.logs.String(), "llm stream completed") {
		t.Errorf("no completion log: %s", f.logs.String())
	}
	f.assertClean()
}

func TestAnthropicStreamStopReasons(t *testing.T) {
	for _, stop := range []string{"max_tokens", "refusal", "pause_turn"} {
		t.Run(stop, func(t *testing.T) {
			f := newFixture(t, 0, anthropictest.TextStream("r", servedModel, stop, false, `{"partial":`))
			resp, err := f.a.Stream(context.Background(), testRequest(), func(StreamEvent) error { return nil })
			if err != nil {
				t.Fatal(err)
			}
			if string(resp.StopReason) != stop {
				t.Errorf("stop = %q", resp.StopReason)
			}
		})
	}
}

// A stream that ends without message_stop (connection dropped by a proxy)
// is unavailable, never a usable (partial) answer.
func TestAnthropicStreamIncomplete(t *testing.T) {
	f := newFixture(t, 0, anthropictest.SSE("req_cut",
		anthropictest.MessageStart(servedModel, 3), anthropictest.TextBlockStart(0),
		anthropictest.TextDelta(0, `{"sms_body":"`+secretOutput)))
	_, err := f.a.Stream(context.Background(), testRequest(), func(StreamEvent) error { return nil })
	var e *Error
	if !errors.As(err, &e) || !errors.Is(err, ErrUnavailable) || !Retryable(err) || e.RequestID != "req_cut" {
		t.Fatalf("err = %v", err)
	}
	f.assertClean(err.Error())
}

// An error event inside the 200 stream is classified by its API error type
// (the HTTP status is 200); its message never reaches errors or logs.
func TestAnthropicStreamErrorEvent(t *testing.T) {
	tests := []struct {
		errType string
		want    error
	}{
		{"overloaded_error", ErrUnavailable},
		{"api_error", ErrUnavailable},
		{"rate_limit_error", ErrUnavailable},
		{"invalid_request_error", ErrRejected},
	}
	for _, tt := range tests {
		t.Run(tt.errType, func(t *testing.T) {
			f := newFixture(t, 2, anthropictest.SSE("req_mid",
				anthropictest.MessageStart(servedModel, 3), anthropictest.TextBlockStart(0),
				anthropictest.TextDelta(0, `{"a":`), anthropictest.StreamError(tt.errType, secretBody)))
			_, err := f.a.Stream(context.Background(), testRequest(), func(StreamEvent) error { return nil })
			var e *Error
			if !errors.As(err, &e) || !errors.Is(err, tt.want) || e.StatusCode != 0 {
				t.Fatalf("err = %v", err)
			}
			if !strings.Contains(err.Error(), tt.errType) {
				t.Errorf("error lacks the type: %v", err)
			}
			// Mid-stream errors are not retried by the SDK (the response
			// had already started).
			if f.tr.Calls() != 1 {
				t.Errorf("calls = %d", f.tr.Calls())
			}
			f.assertClean(err.Error())
		})
	}
}

// HTTP errors before the stream starts map exactly like Complete, including
// the SDK's own retries.
func TestAnthropicStreamHTTPErrors(t *testing.T) {
	f := newFixture(t, 1,
		anthropictest.APIError(529, "overloaded_error", secretBody, "req_1"),
		anthropictest.TextStream("req_2", servedModel, "end_turn", false, `{}`))
	resp, err := f.a.Stream(context.Background(), testRequest(), func(StreamEvent) error { return nil })
	if err != nil || resp.Text != "{}" || f.tr.Calls() != 2 {
		t.Fatalf("resp = %+v, err = %v, calls = %d", resp, err, f.tr.Calls())
	}
	reqs := f.tr.Requests()
	if !bytes.Equal(reqs[0].Body, reqs[1].Body) {
		t.Error("retry changed the request body")
	}

	f = newFixture(t, 2, anthropictest.APIError(401, "authentication_error", secretBody, "req_401"))
	_, err = f.a.Stream(context.Background(), testRequest(), func(StreamEvent) error { return nil })
	var e *Error
	if !errors.As(err, &e) || !errors.Is(err, ErrRejected) || e.StatusCode != 401 || f.tr.Calls() != 1 {
		t.Fatalf("err = %v, calls = %d", err, f.tr.Calls())
	}
	f.assertClean(err.Error())

	netErr := &net.OpError{Op: "dial", Net: "tcp", Err: errors.New("refused " + secretBody)}
	f = newFixture(t, 0, anthropictest.Reply{Err: netErr})
	_, err = f.a.Stream(context.Background(), testRequest(), func(StreamEvent) error { return nil })
	if !errors.Is(err, ErrUnavailable) || err.Error() != "llm: provider unavailable: anthropic transport failure" {
		t.Fatalf("err = %v", err)
	}
	f.assertClean(err.Error())
}

// An onEvent error stops reading and is returned unchanged.
func TestAnthropicStreamCallbackError(t *testing.T) {
	stop := errors.New("client went away")
	f := newFixture(t, 0, anthropictest.TextStream("r", servedModel, "end_turn", false, "a", "b", "c"))
	n := 0
	_, err := f.a.Stream(context.Background(), testRequest(), func(StreamEvent) error {
		n++
		if n == 2 {
			return stop
		}
		return nil
	})
	if !errors.Is(err, stop) || n != 2 {
		t.Fatalf("err = %v, events = %d", err, n)
	}
}

// Cancelling the context while the provider stalls mid-stream returns the
// context error promptly.
func TestAnthropicStreamCancellationMidStream(t *testing.T) {
	r := anthropictest.SSE("req_hold", anthropictest.MessageStart(servedModel, 3),
		anthropictest.TextBlockStart(0), anthropictest.TextDelta(0, `{"sms_body":"Hel`))
	r.HoldOpen = true
	f := newFixture(t, 2, r)
	ctx, cancel := context.WithCancel(context.Background())
	firstDelta := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		_, err := f.a.Stream(ctx, testRequest(), func(ev StreamEvent) error {
			if ev.Kind == EventTextDelta {
				close(firstDelta)
			}
			return nil
		})
		done <- err
	}()
	select {
	case <-firstDelta:
	case <-time.After(5 * time.Second):
		t.Fatal("no delta received")
	}
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) || Retryable(err) {
			t.Fatalf("err = %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Stream did not return after cancellation")
	}
	if f.tr.Calls() != 1 {
		t.Errorf("calls = %d", f.tr.Calls())
	}
}

func TestAnthropicStreamDeadline(t *testing.T) {
	r := anthropictest.SSE("req_hold", anthropictest.MessageStart(servedModel, 3))
	r.HoldOpen = true
	f := newFixture(t, 2, r)
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	_, err := f.a.Stream(ctx, testRequest(), func(StreamEvent) error { return nil })
	if !errors.Is(err, context.DeadlineExceeded) || !Retryable(err) {
		t.Fatalf("err = %v", err)
	}
}

func TestAnthropicStreamRejectsInvalidRequestLocally(t *testing.T) {
	f := newFixture(t, 0)
	req := testRequest()
	req.Messages = req.Messages[:2] // ends with an assistant turn (prefill)
	_, err := f.a.Stream(context.Background(), req, func(StreamEvent) error { return nil })
	if !errors.Is(err, ErrRejected) || f.tr.Calls() != 0 {
		t.Fatalf("err = %v, calls = %d", err, f.tr.Calls())
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := f.a.Stream(ctx, testRequest(), nil); !errors.Is(err, context.Canceled) || f.tr.Calls() != 0 {
		t.Fatalf("err = %v", err)
	}
}

// Non-text blocks are never output, as with Complete.
func TestAnthropicStreamUnexpectedBlock(t *testing.T) {
	f := newFixture(t, 0, anthropictest.SSE("r",
		anthropictest.MessageStart(servedModel, 3),
		anthropictest.BlockStart(0, anthropictest.ToolUse()), anthropictest.BlockStop(0),
		anthropictest.MessageDelta("end_turn", 2), anthropictest.MessageStop()))
	_, err := f.a.Stream(context.Background(), testRequest(), func(StreamEvent) error { return nil })
	if !errors.Is(err, ErrUnavailable) {
		t.Fatalf("err = %v", err)
	}
}
