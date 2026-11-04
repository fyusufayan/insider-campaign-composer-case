// Package budget enforces a hard cap on paid HTTP requests during manual live
// verification (CP10). Transport wraps the HTTP transport handed to the
// Anthropic adapter and counts every attempt in a file that survives process
// restarts; once the cap is reached it refuses to send anything. Test tooling
// only; production code never imports it.
package budget

import (
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
)

// ErrExhausted is returned instead of sending a request beyond the cap.
var ErrExhausted = errors.New("live budget: request cap reached; nothing was sent")

// ErrHostNotAllowed is returned for requests to any other host.
var ErrHostNotAllowed = errors.New("live budget: host not allowed")

// Transport counts and caps requests. The counter is incremented and synced
// to disk before a request is sent, so a crash never under-counts. It fails
// closed: the counter file must already exist (created deliberately with
// "0"), so a mistyped path or a deleted counter never grants a fresh budget.
type Transport struct {
	Base        http.RoundTripper
	CounterFile string // absolute path
	Max         int
	AllowedHost string

	mu sync.Mutex
}

// Used returns the number of attempts recorded in the counter file.
func (t *Transport) Used() (int, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.read()
}

func (t *Transport) read() (int, error) {
	b, err := os.ReadFile(t.CounterFile)
	if errors.Is(err, os.ErrNotExist) {
		return 0, fmt.Errorf("live budget: counter file %s does not exist; create it with \"0\" to start a new budget", t.CounterFile)
	}
	if err != nil {
		return 0, fmt.Errorf("live budget: read counter: %w", err)
	}
	n, err := strconv.Atoi(strings.TrimSpace(string(b)))
	if err != nil || n < 0 {
		return 0, fmt.Errorf("live budget: corrupt counter file %s", t.CounterFile)
	}
	return n, nil
}

func (t *Transport) write(n int) error {
	tmp := t.CounterFile + ".tmp"
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return fmt.Errorf("live budget: write counter: %w", err)
	}
	if _, err := f.WriteString(strconv.Itoa(n) + "\n"); err != nil {
		_ = f.Close()
		return fmt.Errorf("live budget: write counter: %w", err)
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		return fmt.Errorf("live budget: sync counter: %w", err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("live budget: close counter: %w", err)
	}
	if err := os.Rename(tmp, t.CounterFile); err != nil {
		return fmt.Errorf("live budget: rename counter: %w", err)
	}
	return nil
}

// RoundTrip implements http.RoundTripper.
func (t *Transport) RoundTrip(req *http.Request) (*http.Response, error) {
	if req.Body != nil {
		defer func() { _ = req.Body.Close() }()
	}
	if t.Max <= 0 || !filepath.IsAbs(t.CounterFile) {
		return nil, errors.New("live budget: misconfigured (Max > 0 and an absolute CounterFile are required)")
	}
	if t.AllowedHost == "" || req.URL.Hostname() != t.AllowedHost {
		return nil, ErrHostNotAllowed
	}
	t.mu.Lock()
	used, err := t.read()
	if err == nil && used >= t.Max {
		err = ErrExhausted
	}
	if err == nil {
		err = t.write(used + 1)
	}
	t.mu.Unlock()
	if err != nil {
		return nil, err
	}
	base := t.Base
	if base == nil {
		base = http.DefaultTransport
	}
	return base.RoundTrip(req)
}
