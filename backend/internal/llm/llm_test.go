package llm

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/fyusufayan/insider-campaign-composer-case/backend/internal/platform/config"
)

func TestScriptedPlaysStepsInOrder(t *testing.T) {
	boom := errors.New("boom")
	s := NewScripted(Reply("one"), Fail(boom), Stopped("cut", StopMaxTokens))
	ctx := context.Background()

	r, err := s.Complete(ctx, Request{Model: "m", Messages: []Message{{Role: RoleUser, Content: "a"}}})
	if err != nil || r.Text != "one" || r.StopReason != StopEndTurn {
		t.Fatalf("step 1 = %+v, %v", r, err)
	}
	if _, err := s.Complete(ctx, Request{}); !errors.Is(err, boom) {
		t.Fatalf("step 2 err = %v", err)
	}
	r, err = s.Complete(ctx, Request{})
	if err != nil || r.StopReason != StopMaxTokens {
		t.Fatalf("step 3 = %+v, %v", r, err)
	}
	if _, err := s.Complete(ctx, Request{}); !errors.Is(err, ErrScriptExhausted) {
		t.Fatalf("exhausted err = %v", err)
	}
	reqs := s.Requests()
	if len(reqs) != 4 || reqs[0].Model != "m" || reqs[0].Messages[0].Content != "a" {
		t.Errorf("requests = %+v", reqs)
	}
	// Recorded requests are copies.
	reqs[0].Messages[0].Content = "changed"
	if s.Requests()[0].Messages[0].Content != "a" {
		t.Error("Requests exposes internal state")
	}
}

func TestScriptedHangHonoursContext(t *testing.T) {
	s := NewScripted(Hang(), Hang())
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	if _, err := s.Complete(ctx, Request{}); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v", err)
	}
	ctx2, cancel2 := context.WithCancel(context.Background())
	cancel2()
	if _, err := s.Complete(ctx2, Request{}); !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v", err)
	}
	if s.Calls() != 1 {
		t.Errorf("a call with a cancelled context consumed a step (calls = %d)", s.Calls())
	}
}

func TestScriptedStream(t *testing.T) {
	s := NewScripted(Step{Response: Response{Text: "héllo wörld", StopReason: StopEndTurn}, Chunks: []string{"hé", "llo w", "örld"}}, Reply("x"))
	var got []string
	r, err := s.Stream(context.Background(), Request{}, func(e StreamEvent) error {
		got = append(got, e.Text)
		return nil
	})
	if err != nil || strings.Join(got, "") != r.Text || len(got) != 3 {
		t.Fatalf("stream = %q, %+v, %v", got, r, err)
	}
	stop := errors.New("client gone")
	if _, err := s.Stream(context.Background(), Request{}, func(StreamEvent) error { return stop }); !errors.Is(err, stop) {
		t.Fatalf("err = %v", err)
	}
}

func TestScriptedConcurrent(t *testing.T) {
	const n = 50
	steps := make([]Step, n)
	for i := range steps {
		steps[i] = Reply("ok")
	}
	s := NewScripted(steps...)
	var wg sync.WaitGroup
	for range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := s.Complete(context.Background(), Request{}); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	if s.Calls() != n || s.Remaining() != 0 {
		t.Errorf("calls = %d, remaining = %d", s.Calls(), s.Remaining())
	}
}

func TestDemo(t *testing.T) {
	calls := 0
	d := NewDemo(map[string]DemoResponder{"echo": func(r Request) (string, error) {
		calls++
		return "İstanbul " + r.Messages[0].Content + strings.Repeat("ş", 50), nil
	}})
	req := Request{Task: "echo", Model: "configured", Messages: []Message{{Role: RoleUser, Content: "x"}}}
	r, err := d.Complete(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	if r.Model != DemoModel || r.StopReason != StopEndTurn {
		t.Errorf("response = %+v", r)
	}
	var b strings.Builder
	sr, err := d.Stream(context.Background(), req, func(e StreamEvent) error {
		if e.Kind != EventTextDelta || !utf8Valid(e.Text) {
			t.Errorf("bad event %+v", e)
		}
		b.WriteString(e.Text)
		return nil
	})
	if err != nil || b.String() != r.Text || sr.Text != r.Text {
		t.Fatalf("stream %q vs %q (%v)", b.String(), r.Text, err)
	}

	if _, err := d.Complete(context.Background(), Request{Task: "other"}); !errors.Is(err, ErrRejected) || !errors.Is(err, ErrDemoUnknownTask) {
		t.Errorf("unknown task err = %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	before := calls
	if _, err := d.Complete(ctx, req); !errors.Is(err, context.Canceled) || calls != before {
		t.Errorf("cancelled err = %v, calls %d→%d", err, before, calls)
	}
	if got := d.Tasks(); len(got) != 1 || got[0] != "echo" {
		t.Errorf("tasks = %v", got)
	}
}

func utf8Valid(s string) bool { return strings.ToValidUTF8(s, "�") == s }

func TestRetryable(t *testing.T) {
	tests := []struct {
		err  error
		want bool
	}{
		{nil, false},
		{&Error{Kind: ErrUnavailable, StatusCode: 529}, true},
		{&Error{Kind: ErrRejected, StatusCode: 401}, false},
		{context.DeadlineExceeded, true},
		{context.Canceled, false},
		{errors.New("other"), false},
	}
	for _, tt := range tests {
		if got := Retryable(tt.err); got != tt.want {
			t.Errorf("Retryable(%v) = %v", tt.err, got)
		}
	}
	e := &Error{Kind: ErrUnavailable, StatusCode: 503, RequestID: "req_1", Err: errors.New("overloaded")}
	if !strings.Contains(e.Error(), "503") || !errors.Is(e, ErrUnavailable) || errors.Is(e, ErrRejected) {
		t.Errorf("error = %v", e)
	}
}

// Provider selection (ARCHITECTURE §7.3) from real configuration: demo when
// selected, an explicit failure — never demo — when anthropic is resolved.
func TestNewSelectsProvider(t *testing.T) {
	const key = "sk-ant-test"
	tests := []struct {
		provider, key string
		wantDemo      bool
		wantErr       error
		wantCfgErr    bool
	}{
		{"", "", true, nil, false},
		{"auto", "", true, nil, false},
		{"auto", key, false, ErrAnthropicNotAvailable, false},
		{"anthropic", key, false, ErrAnthropicNotAvailable, false},
		{"anthropic", "", false, nil, true},
		{"demo", "", true, nil, false},
		{"demo", key, true, nil, false},
	}
	for _, tt := range tests {
		t.Run(tt.provider+"/"+tt.key, func(t *testing.T) {
			env := map[string]string{"DATABASE_URL": "postgres://x", "LLM_PROVIDER": tt.provider, "ANTHROPIC_API_KEY": tt.key}
			cfg, err := config.Load(func(k string) string { return env[k] })
			if tt.wantCfgErr {
				if err == nil {
					t.Fatal("configuration accepted")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			c, err := New(cfg.LLM, nil)
			if tt.wantErr != nil {
				if !errors.Is(err, tt.wantErr) || c != nil {
					t.Fatalf("New = %T, %v; want %v and no client", c, err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if _, ok := c.(*Demo); ok != tt.wantDemo {
				t.Errorf("client = %T", c)
			}
		})
	}
	if _, err := New(config.LLM{Provider: config.LLMProviderAuto}, nil); err == nil {
		t.Error("unresolved provider accepted")
	}
}
