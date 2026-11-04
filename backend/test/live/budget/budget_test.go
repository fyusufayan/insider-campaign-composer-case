package budget

import (
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
)

type countingRT struct{ n atomic.Int32 }

func (c *countingRT) RoundTrip(req *http.Request) (*http.Response, error) {
	c.n.Add(1)
	return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader("{}")), Request: req}, nil
}

func newReq(t *testing.T, url string) *http.Request {
	t.Helper()
	r, err := http.NewRequest(http.MethodPost, url, strings.NewReader("{}"))
	if err != nil {
		t.Fatal(err)
	}
	return r
}

// newCounter creates a counter file holding 0 (a deliberately started budget).
func newCounter(t *testing.T) string {
	t.Helper()
	file := filepath.Join(t.TempDir(), "count")
	if err := os.WriteFile(file, []byte("0\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return file
}

func TestCapPersistsAcrossTransports(t *testing.T) {
	file := newCounter(t)
	base := &countingRT{}
	mk := func() *Transport {
		return &Transport{Base: base, CounterFile: file, Max: 3, AllowedHost: "api.anthropic.com"}
	}
	tr := mk()
	for i := 0; i < 2; i++ {
		if _, err := tr.RoundTrip(newReq(t, "https://api.anthropic.com/v1/messages")); err != nil {
			t.Fatal(err)
		}
	}
	// A "restart": a new Transport reads the same counter.
	tr2 := mk()
	if n, _ := tr2.Used(); n != 2 {
		t.Fatalf("used after restart = %d", n)
	}
	if _, err := tr2.RoundTrip(newReq(t, "https://api.anthropic.com/v1/messages")); err != nil {
		t.Fatal(err)
	}
	if _, err := tr2.RoundTrip(newReq(t, "https://api.anthropic.com/v1/messages")); !errors.Is(err, ErrExhausted) {
		t.Fatalf("4th request err = %v", err)
	}
	if base.n.Load() != 3 {
		t.Errorf("requests sent = %d, want 3", base.n.Load())
	}
}

func TestConcurrentRequestsNeverExceedCap(t *testing.T) {
	file := newCounter(t)
	base := &countingRT{}
	tr := &Transport{Base: base, CounterFile: file, Max: 3, AllowedHost: "api.anthropic.com"}
	var wg sync.WaitGroup
	for range 20 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _ = tr.RoundTrip(newReq(t, "https://api.anthropic.com/v1/messages"))
		}()
	}
	wg.Wait()
	if base.n.Load() != 3 {
		t.Errorf("requests sent = %d, want 3", base.n.Load())
	}
	if n, _ := tr.Used(); n != 3 {
		t.Errorf("counter = %d", n)
	}
}

func TestRefusesOtherHostsAndBadConfig(t *testing.T) {
	file := newCounter(t)
	base := &countingRT{}
	tr := &Transport{Base: base, CounterFile: file, Max: 3, AllowedHost: "api.anthropic.com"}
	if _, err := tr.RoundTrip(newReq(t, "https://example.com/v1/messages")); !errors.Is(err, ErrHostNotAllowed) {
		t.Errorf("other host err = %v", err)
	}
	bad := &Transport{Base: base, CounterFile: "relative", Max: 3, AllowedHost: "api.anthropic.com"}
	if _, err := bad.RoundTrip(newReq(t, "https://api.anthropic.com/v1/messages")); err == nil {
		t.Error("relative counter file accepted")
	}
	if err := os.WriteFile(file, []byte("garbage"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := tr.RoundTrip(newReq(t, "https://api.anthropic.com/v1/messages")); err == nil {
		t.Error("corrupt counter accepted")
	}
	if base.n.Load() != 0 {
		t.Errorf("requests sent = %d", base.n.Load())
	}
}

// A missing counter (mistyped path, or deleted mid-run) fails closed: nothing
// is sent and no fresh budget is created.
func TestMissingCounterFailsClosed(t *testing.T) {
	base := &countingRT{}
	missing := filepath.Join(t.TempDir(), "typo-count")
	tr := &Transport{Base: base, CounterFile: missing, Max: 3, AllowedHost: "api.anthropic.com"}
	if _, err := tr.Used(); err == nil {
		t.Error("Used accepted a missing counter")
	}
	if _, err := tr.RoundTrip(newReq(t, "https://api.anthropic.com/v1/messages")); err == nil {
		t.Error("request sent without a counter file")
	}
	if _, err := os.Stat(missing); !os.IsNotExist(err) {
		t.Error("a counter file was created implicitly")
	}

	file := newCounter(t)
	tr = &Transport{Base: base, CounterFile: file, Max: 3, AllowedHost: "api.anthropic.com"}
	if _, err := tr.RoundTrip(newReq(t, "https://api.anthropic.com/v1/messages")); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(file); err != nil {
		t.Fatal(err)
	}
	if _, err := tr.RoundTrip(newReq(t, "https://api.anthropic.com/v1/messages")); err == nil {
		t.Error("request sent after the counter was deleted")
	}
	if base.n.Load() != 1 {
		t.Errorf("requests sent = %d, want 1", base.n.Load())
	}
}
