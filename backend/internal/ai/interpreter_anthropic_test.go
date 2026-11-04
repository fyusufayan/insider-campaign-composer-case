package ai

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"reflect"
	"strings"
	"testing"

	"github.com/fyusufayan/insider-campaign-composer-case/backend/internal/llm"
	"github.com/fyusufayan/insider-campaign-composer-case/backend/internal/llm/anthropictest"
)

// The real Interpreter over the real Anthropic adapter, with the Messages API
// replaced by an offline transport. Nothing here needs an API key or network.

const anthropicServedModel = "claude-opus-5-5"

func newAnthropicInterpreter(t *testing.T, replies ...anthropictest.Reply) (*Interpreter, *anthropictest.Transport) {
	t.Helper()
	tr := anthropictest.New(replies...)
	c, err := llm.NewAnthropic(llm.AnthropicConfig{APIKey: "sk-ant-test", MaxRetries: 2, HTTPClient: tr.Client()})
	if err != nil {
		t.Fatal(err)
	}
	return newTestInterpreter(t, c), tr
}

func anthropicText(requestID, stop, text string) anthropictest.Reply {
	return anthropictest.OK(requestID, anthropictest.Message(anthropicServedModel, stop, 2000, 300, anthropictest.Thinking(), anthropictest.Text(text)))
}

func capturedBody(t *testing.T, c anthropictest.Captured) map[string]any {
	t.Helper()
	m, err := c.JSON()
	if err != nil {
		t.Fatal(err)
	}
	return m
}

func wireMessages(t *testing.T, body map[string]any) [][2]string {
	t.Helper()
	raw, _ := body["messages"].([]any)
	out := make([][2]string, len(raw))
	for i, m := range raw {
		mm := m.(map[string]any)
		blocks := mm["content"].([]any)
		if len(blocks) != 1 {
			t.Fatalf("message %d has %d blocks", i, len(blocks))
		}
		out[i] = [2]string{mm["role"].(string), blocks[0].(map[string]any)["text"].(string)}
	}
	return out
}

// Invalid first proposal → one repair turn with the append-only history →
// valid result; the wire carries the interpretation schema unchanged.
func TestInterpreterAnthropicRepairFlow(t *testing.T) {
	invalid := mutate(t, keyInterpretation, "")
	it, tr := newAnthropicInterpreter(t,
		anthropicText("req_first", "end_turn", invalid),
		anthropicText("req_repair", "end_turn", validOutput),
	)
	in, err := it.Interpret(context.Background(), exampleBrf)
	if err != nil {
		t.Fatal(err)
	}
	if got := segmentJSON(t, in); got != wantNormalizedSegment {
		t.Errorf("segment = %s", got)
	}
	if !in.Meta.Repaired || in.Meta.Calls != 2 || in.Meta.Model != anthropicServedModel ||
		in.Meta.Usage != (llm.Usage{InputTokens: 4000, OutputTokens: 600}) {
		t.Errorf("meta = %+v", in.Meta)
	}

	reqs := tr.Requests()
	if len(reqs) != 2 {
		t.Fatalf("requests = %d", len(reqs))
	}
	first, second := capturedBody(t, reqs[0]), capturedBody(t, reqs[1])

	wantFirst := [][2]string{{"user", segmentUserMessage(exampleBrf)}}
	if got := wireMessages(t, first); !reflect.DeepEqual(got, wantFirst) {
		t.Errorf("first messages = %q", got)
	}
	got := wireMessages(t, second)
	if len(got) != 3 || got[0] != wantFirst[0] || got[1] != [2]string{"assistant", invalid} || got[2][0] != "user" ||
		!strings.Contains(got[2][1], keyInterpretation) {
		t.Errorf("repair messages = %q", got)
	}

	var wantSchema any
	if err := json.Unmarshal(InterpretationSchemaJSON(), &wantSchema); err != nil {
		t.Fatal(err)
	}
	for i, body := range []map[string]any{first, second} {
		if body["model"] != testModel || body["max_tokens"] != float64(1000) {
			t.Errorf("request %d model/max_tokens = %v/%v", i, body["model"], body["max_tokens"])
		}
		sys := body["system"].([]any)
		if len(sys) != 1 || sys[0].(map[string]any)["text"] != segmentSystemPrompt {
			t.Errorf("request %d system prompt not sent verbatim", i)
		}
		oc := body["output_config"].(map[string]any)
		format := oc["format"].(map[string]any)
		if oc["effort"] != "low" || format["type"] != "json_schema" || !reflect.DeepEqual(format["schema"], wantSchema) {
			t.Errorf("request %d output_config differs from the interpretation schema", i)
		}
	}
}

func TestInterpreterAnthropicFailures(t *testing.T) {
	t.Run("refusal is never accepted", func(t *testing.T) {
		it, tr := newAnthropicInterpreter(t, anthropictest.OK("req_r", anthropictest.Message(anthropicServedModel, "refusal", 10, 2, anthropictest.Text(validOutput))))
		_, err := it.Interpret(context.Background(), exampleBrf)
		mustAIError(t, err, ErrLLMRefused)
		if tr.Calls() != 1 {
			t.Errorf("calls = %d", tr.Calls())
		}
	})
	t.Run("max_tokens retried once with a doubled budget", func(t *testing.T) {
		it, tr := newAnthropicInterpreter(t,
			anthropicText("req_1", "max_tokens", `{"segment":`),
			anthropicText("req_2", "end_turn", validOutput))
		if _, err := it.Interpret(context.Background(), exampleBrf); err != nil {
			t.Fatal(err)
		}
		reqs := tr.Requests()
		if len(reqs) != 2 || capturedBody(t, reqs[1])["max_tokens"] != float64(2000) {
			t.Errorf("truncation retry = %d requests", len(reqs))
		}
	})
	t.Run("unknown stop reason is not output", func(t *testing.T) {
		it, _ := newAnthropicInterpreter(t, anthropicText("req_p", "pause_turn", validOutput))
		_, err := it.Interpret(context.Background(), exampleBrf)
		mustAIError(t, err, ErrLLMTruncated)
	})
	t.Run("authentication failure is rejected, not retried", func(t *testing.T) {
		it, tr := newAnthropicInterpreter(t, anthropictest.APIError(http.StatusUnauthorized, "authentication_error", "invalid x-api-key", "req_a"))
		_, err := it.Interpret(context.Background(), exampleBrf)
		e := mustAIError(t, err, ErrLLMRejected)
		if e.Retryable || tr.Calls() != 1 {
			t.Errorf("retryable = %v, calls = %d", e.Retryable, tr.Calls())
		}
	})
	t.Run("overload after SDK retries is unavailable and retryable", func(t *testing.T) {
		r := anthropictest.APIError(529, "overloaded_error", "overloaded", "req_o")
		it, tr := newAnthropicInterpreter(t, r, r, r)
		_, err := it.Interpret(context.Background(), exampleBrf)
		e := mustAIError(t, err, ErrLLMUnavailable)
		var le *llm.Error
		if !e.Retryable || tr.Calls() != 3 || !errors.As(err, &le) || le.StatusCode != 529 || le.RequestID != "req_o" {
			t.Errorf("retryable = %v, calls = %d, cause = %v", e.Retryable, tr.Calls(), e.Cause)
		}
	})
	t.Run("second invalid proposal fails without a demo answer", func(t *testing.T) {
		invalid := mutate(t, keyInterpretation, "")
		it, tr := newAnthropicInterpreter(t, anthropicText("req_1", "end_turn", invalid), anthropicText("req_2", "end_turn", invalid))
		_, err := it.Interpret(context.Background(), exampleBrf)
		e := mustAIError(t, err, ErrOutputInvalid)
		if e.RawProposal != invalid || e.Model != anthropicServedModel || tr.Calls() != 2 {
			t.Errorf("error = %+v, calls = %d", e, tr.Calls())
		}
	})
}
