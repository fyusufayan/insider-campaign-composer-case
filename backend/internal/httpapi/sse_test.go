package httpapi

import (
	"bufio"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/fyusufayan/insider-campaign-composer-case/backend/internal/llm"
)

func TestSSEWriterFormat(t *testing.T) {
	rr := httptest.NewRecorder()
	s := newSSEWriter(rr)
	if err := s.event("delta", map[string]string{"text": "line1\nline2 \u2028 </script>"}); err != nil {
		t.Fatal(err)
	}
	if err := s.comment("keep-alive"); err != nil {
		t.Fatal(err)
	}
	want := "event: delta\ndata: {\"text\":\"line1\\nline2 \\u2028 \\u003c/script\\u003e\"}\n\n: keep-alive\n\n"
	if rr.Body.String() != want {
		t.Errorf("body = %q", rr.Body.String())
	}
	if !rr.Flushed || rr.Header().Get("Content-Type") != "text/event-stream; charset=utf-8" {
		t.Errorf("flushed = %v, headers = %v", rr.Flushed, rr.Header())
	}
	big := strings.Repeat("x", maxSSEEventBytes)
	if err := s.event("delta", big); !errors.Is(err, errSSEEventTooLarge) {
		t.Errorf("err = %v", err)
	}
}

type failingWriter struct{ *httptest.ResponseRecorder }

func (failingWriter) Write([]byte) (int, error) { return 0, errors.New("broken pipe") }

func TestSSEWriterStopsAfterWriteFailure(t *testing.T) {
	s := newSSEWriter(failingWriter{httptest.NewRecorder()})
	if err := s.event("meta", 1); err == nil {
		t.Fatal("no error")
	}
	if err := s.comment("x"); err == nil || s.failed == nil {
		t.Fatal("later writes did not fail")
	}
}

func TestSplitRunes(t *testing.T) {
	s := strings.Repeat("aş🎉", 1000)
	parts := splitRunes(s, 7)
	if strings.Join(parts, "") != s {
		t.Fatal("parts do not reassemble")
	}
	for _, p := range parts {
		if len(p) > 7 || !utf8.ValidString(p) || p == "" {
			t.Fatalf("bad part %q", p)
		}
	}
	if got := splitRunes("short", 7); len(got) != 1 || got[0] != "short" {
		t.Errorf("short = %v", got)
	}
}

// While the model thinks for a long time, keep-alive comments flow so that
// proxies do not time out the stream.
func TestGenerateContentHeartbeat(t *testing.T) {
	f := newAPIFixture(t, nil)
	c := f.segmented()
	release := make(chan struct{})
	f.llm.Push(llm.Step{Func: func(ctx context.Context, _ llm.Request) (llm.Response, error) {
		select {
		case <-release:
		case <-ctx.Done():
		}
		return llm.Response{Text: okGen, StopReason: llm.StopEndTurn}, nil
	}})
	log, _ := newTestLogger()
	h := &campaignHandlers{svc: f.svcForTest(t), log: log, heartbeat: 5 * time.Millisecond}
	mux := http.NewServeMux()
	h.register(mux)
	srv := httptest.NewServer(mux)
	defer srv.Close()

	resp, err := http.Post(srv.URL+"/api/v1/campaigns/"+c.ID+"/content/generations", "application/json", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	br := bufio.NewReader(resp.Body)
	deadline := time.Now().Add(5 * time.Second)
	beats := 0
	for beats < 2 {
		if time.Now().After(deadline) {
			t.Fatal("no heartbeat")
		}
		line, err := br.ReadString('\n')
		if err != nil {
			t.Fatal(err)
		}
		if line == ": keep-alive\n" {
			beats++
		}
	}
	close(release)
	evs := parseSSE(t, br)
	if len(evs) == 0 || evs[len(evs)-1].Name != "validated" {
		t.Fatalf("events = %v", names(evs))
	}
}
