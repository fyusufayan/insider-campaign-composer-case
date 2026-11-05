package httpapi

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"time"
	"unicode/utf8"
)

// Server-sent events (ARCHITECTURE §12). Every event has an explicit name
// and one line of JSON data; comments are heartbeats. Each write is flushed
// immediately (nginx: proxy_buffering off, X-Accel-Buffering: no) and bounded
// by a write deadline, so a stalled client cannot pin the handler.
const (
	// maxSSEEventBytes bounds one event's data; larger payloads are a bug.
	maxSSEEventBytes = 256 << 10
	// maxSSEDeltaBytes bounds the text of one delta event; longer deltas
	// are split at rune boundaries.
	maxSSEDeltaBytes = 2 << 10
	sseWriteTimeout  = 30 * time.Second
	sseHeartbeat     = 15 * time.Second
)

var errSSEEventTooLarge = errors.New("httpapi: sse event too large")

// sseWriter writes an event stream. Not safe for concurrent use: one
// goroutine (the handler) owns it.
type sseWriter struct {
	w       http.ResponseWriter
	rc      *http.ResponseController
	started bool
	failed  error
}

func newSSEWriter(w http.ResponseWriter) *sseWriter {
	return &sseWriter{w: w, rc: http.NewResponseController(w)}
}

// start sends the 200 response headers.
func (s *sseWriter) start() {
	if s.started {
		return
	}
	s.started = true
	h := s.w.Header()
	h.Set("Content-Type", "text/event-stream; charset=utf-8")
	h.Set("Cache-Control", "no-cache")
	h.Set("X-Accel-Buffering", "no")
	h.Set("X-Content-Type-Options", "nosniff")
	s.w.WriteHeader(http.StatusOK)
	s.flush()
}

// event writes "event: name\ndata: <json>\n\n". After a write failure (the
// client went away) every later call returns that error without writing.
func (s *sseWriter) event(name string, v any) error {
	if s.failed != nil {
		return s.failed
	}
	data, err := json.Marshal(v)
	if err != nil {
		return fmt.Errorf("httpapi: encode sse %s: %w", name, err)
	}
	if len(data) > maxSSEEventBytes {
		return fmt.Errorf("%w: %s (%d bytes)", errSSEEventTooLarge, name, len(data))
	}
	// encoding/json escapes control characters, so data is one line.
	if bytes.ContainsAny(data, "\r\n") {
		return fmt.Errorf("httpapi: sse %s data is not one line", name)
	}
	s.start()
	var buf bytes.Buffer
	buf.Grow(len(name) + len(data) + 16)
	buf.WriteString("event: ")
	buf.WriteString(name)
	buf.WriteString("\ndata: ")
	buf.Write(data)
	buf.WriteString("\n\n")
	return s.write(buf.Bytes())
}

// retry writes the reconnection delay field ("retry: <ms>") for
// EventSource clients.
func (s *sseWriter) retry(d time.Duration) error {
	if s.failed != nil {
		return s.failed
	}
	s.start()
	return s.write([]byte("retry: " + strconv.FormatInt(d.Milliseconds(), 10) + "\n\n"))
}

// comment writes a heartbeat comment.
func (s *sseWriter) comment(text string) error {
	if s.failed != nil {
		return s.failed
	}
	s.start()
	return s.write([]byte(": " + text + "\n\n"))
}

func (s *sseWriter) write(p []byte) error {
	_ = s.rc.SetWriteDeadline(time.Now().Add(sseWriteTimeout)) // unsupported by test recorders
	if _, err := s.w.Write(p); err != nil {
		s.failed = err
		return err
	}
	return s.flush()
}

func (s *sseWriter) flush() error {
	if err := s.rc.Flush(); err != nil && !errors.Is(err, http.ErrNotSupported) {
		s.failed = err
		return err
	}
	return nil
}

// finish clears the write deadline so it does not leak into the next
// request on a kept-alive connection.
func (s *sseWriter) finish() {
	_ = s.rc.SetWriteDeadline(time.Time{})
}

// splitRunes splits s into pieces of at most n bytes at rune boundaries.
func splitRunes(s string, n int) []string {
	if len(s) <= n {
		return []string{s}
	}
	var out []string
	for len(s) > n {
		cut := n
		for cut > 0 && !utf8.RuneStart(s[cut]) {
			cut--
		}
		if cut == 0 {
			cut = n
		}
		out = append(out, s[:cut])
		s = s[cut:]
	}
	if s != "" {
		out = append(out, s)
	}
	return out
}
