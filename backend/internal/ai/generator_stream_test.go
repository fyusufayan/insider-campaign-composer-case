package ai

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/fyusufayan/insider-campaign-composer-case/backend/internal/llm"
)

// progressLog records progress events as compact strings and the preview
// text per attempt and field.
type progressLog struct {
	events  []string
	preview map[int]map[string]string
}

func (p *progressLog) on(ev Progress) error {
	if p.preview == nil {
		p.preview = map[int]map[string]string{}
	}
	switch ev.Kind {
	case ProgressAttempt:
		p.events = append(p.events, fmt.Sprintf("attempt:%d:%s", ev.Attempt, ev.Reason))
	case ProgressDelta:
		if p.preview[ev.Attempt] == nil {
			p.preview[ev.Attempt] = map[string]string{}
		}
		p.preview[ev.Attempt][ev.Field] += ev.Text
		if n := len(p.events); n == 0 || p.events[n-1] != "delta" {
			p.events = append(p.events, "delta")
		}
	case ProgressRepair:
		p.events = append(p.events, fmt.Sprintf("repair:%d:%d", ev.Attempt, ev.Problems))
	default:
		p.events = append(p.events, fmt.Sprintf("%s:%d", ev.Kind, ev.Attempt))
	}
	return nil
}

// splitBytes splits s into n-byte chunks (inside runes and escapes).
func splitBytes(s string, n int) []string {
	var out []string
	for len(s) > n {
		out = append(out, s[:n])
		s = s[n:]
	}
	return append(out, s)
}

func TestGenerateStreamReportsProgressInOrder(t *testing.T) {
	text := okFor(both)
	for _, n := range []int{1, 3, 7, 1000} {
		t.Run(fmt.Sprint(n), func(t *testing.T) {
			client := llm.NewScripted(llm.Step{Response: llm.Response{Text: text, StopReason: llm.StopEndTurn}, Chunks: splitBytes(text, n)})
			var p progressLog
			gen, err := newGen(t, client).GenerateStream(context.Background(), genInput(t, both, pct30), p.on)
			if err != nil {
				t.Fatal(err)
			}
			want := []string{"attempt:1:initial", "delta", "validating:1"}
			if strings.Join(p.events, " ") != strings.Join(want, " ") {
				t.Errorf("events = %v", p.events)
			}
			// The concatenated deltas are exactly the decoded field values.
			pv := p.preview[1]
			if pv["sms_body"] != okSMS || pv["email_subject"] != okSubject || pv["email_body"] != okBody {
				t.Errorf("preview = %#v", pv)
			}
			if gen.Meta.Calls != 1 {
				t.Errorf("meta = %+v", gen.Meta)
			}
		})
	}
}

func TestGenerateStreamThinkingAndRepairAttempts(t *testing.T) {
	bad := out("sms_body", js("Hi {{email}}, enjoy 50% off."))
	client := llm.NewScripted(
		llm.Step{Func: func(ctx context.Context, req llm.Request) (llm.Response, error) {
			return llm.Response{Text: bad, StopReason: llm.StopEndTurn}, nil
		}},
		llm.Reply(okFor(sms)),
	)
	// Thinking is emitted by a wrapping client.
	var p progressLog
	gen, err := newGen(t, thinkingClient{client}).GenerateStream(context.Background(), genInput(t, sms, pct30), p.on)
	if err != nil {
		t.Fatal(err)
	}
	want := "attempt:1:initial thinking:1 delta validating:1 repair:1:2 attempt:2:repair thinking:2 delta validating:2"
	if got := strings.Join(p.events, " "); got != want {
		t.Errorf("events = %s\nwant     %s", got, want)
	}
	// Each attempt's preview is separate: the rejected draft never merges
	// into the repaired one.
	if p.preview[1]["sms_body"] != "Hi {{email}}, enjoy 50% off." || p.preview[2]["sms_body"] != okSMS {
		t.Errorf("preview = %#v", p.preview)
	}
	if !gen.Meta.Repaired {
		t.Error("not repaired")
	}
}

func TestGenerateStreamTruncationRetryIsANewAttempt(t *testing.T) {
	client := llm.NewScripted(llm.Stopped(`{"sms_body":"Hi`, llm.StopMaxTokens), llm.Reply(okFor(sms)))
	var p progressLog
	if _, err := newGen(t, client).GenerateStream(context.Background(), genInput(t, sms, pct30), p.on); err != nil {
		t.Fatal(err)
	}
	want := "attempt:1:initial delta attempt:2:truncation_retry delta validating:2"
	if got := strings.Join(p.events, " "); got != want {
		t.Errorf("events = %s", got)
	}
}

// Failures end without a validating event for the failed request, and the
// error is the same domain error as with Generate.
func TestGenerateStreamFailures(t *testing.T) {
	tests := []struct {
		name  string
		steps []llm.Step
		kind  error
	}{
		{"refusal", []llm.Step{llm.Stopped("", llm.StopRefusal)}, ErrLLMRefused},
		{"truncated twice", []llm.Step{llm.Stopped(`{"a`, llm.StopMaxTokens), llm.Stopped(`{"a`, llm.StopMaxTokens)}, ErrLLMTruncated},
		{"unavailable", []llm.Step{llm.Fail(&llm.Error{Kind: llm.ErrUnavailable, StatusCode: 529})}, ErrLLMUnavailable},
		{"invalid twice", []llm.Step{llm.Reply(`{"sms_body":"{{x}}"}`), llm.Reply(`{"sms_body":"{{x}}"}`)}, ErrOutputInvalid},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var p progressLog
			_, err := newGen(t, llm.NewScripted(tt.steps...)).GenerateStream(context.Background(), genInput(t, sms, noOff), p.on)
			if !errors.Is(err, tt.kind) {
				t.Fatalf("err = %v", err)
			}
			if p.events[0] != "attempt:1:initial" {
				t.Errorf("events = %v", p.events)
			}
		})
	}
}

// An observer error (the client went away) stops generation immediately:
// it is returned unchanged, not classified as a provider failure, and no
// further model request is made.
func TestGenerateStreamObserverErrorStops(t *testing.T) {
	stop := errors.New("observer stopped")
	for _, at := range []ProgressKind{ProgressAttempt, ProgressDelta, ProgressValidating, ProgressRepair} {
		t.Run(string(at), func(t *testing.T) {
			client := llm.NewScripted(llm.Reply(`{"sms_body":"{{x}}"}`), llm.Reply(okFor(sms)))
			_, err := newGen(t, client).GenerateStream(context.Background(), genInput(t, sms, noOff), func(ev Progress) error {
				if ev.Kind == at {
					return stop
				}
				return nil
			})
			if !errors.Is(err, stop) {
				t.Fatalf("err = %v", err)
			}
			if _, ok := AsError(err); ok {
				t.Error("observer error was classified as an AI error")
			}
			want := 1
			if at == ProgressAttempt {
				want = 0 // stopped before the request was sent
			}
			if client.Calls() != want {
				t.Errorf("calls = %d, want %d", client.Calls(), want)
			}
		})
	}
}

func TestGenerateStreamCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	client := llm.NewScripted(llm.Hang())
	done := make(chan error, 1)
	go func() {
		_, err := newGen(t, client).GenerateStream(ctx, genInput(t, sms, noOff), func(Progress) error { return nil })
		done <- err
	}()
	cancel()
	err := <-done
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v", err)
	}
	if _, ok := AsError(err); ok {
		t.Error("cancellation classified as an AI error")
	}
}

// Deltas are extracted only for the selected channels' fields.
func TestGenerateStreamPreviewOnlySelectedFields(t *testing.T) {
	text := out("sms_body", js(okSMS), "email_body", js("not selected"))
	client := llm.NewScripted(llm.Reply(text), llm.Reply(okFor(sms)))
	var p progressLog
	if _, err := newGen(t, client).GenerateStream(context.Background(), genInput(t, sms, pct30), p.on); err != nil {
		t.Fatal(err)
	}
	if _, ok := p.preview[1]["email_body"]; ok {
		t.Errorf("preview of an unselected field: %#v", p.preview)
	}
}

// thinkingClient emits a thinking event before every streamed answer.
type thinkingClient struct{ *llm.Scripted }

func (c thinkingClient) Stream(ctx context.Context, req llm.Request, on func(llm.StreamEvent) error) (llm.Response, error) {
	if err := on(llm.StreamEvent{Kind: llm.EventThinking}); err != nil {
		return llm.Response{}, err
	}
	return c.Scripted.Stream(ctx, req, on)
}
