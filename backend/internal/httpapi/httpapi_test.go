package httpapi

import (
	"bytes"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/fyusufayan/insider-campaign-composer-case/backend/internal/platform/logging"
)

func newTestLogger() (*slog.Logger, *bytes.Buffer) {
	var buf bytes.Buffer
	return logging.New(&buf, slog.LevelDebug), &buf
}

func logLines(t *testing.T, buf *bytes.Buffer) []map[string]any {
	t.Helper()
	var out []map[string]any
	for _, line := range bytes.Split(bytes.TrimSpace(buf.Bytes()), []byte("\n")) {
		if len(line) == 0 {
			continue
		}
		var m map[string]any
		if err := json.Unmarshal(line, &m); err != nil {
			t.Fatalf("log line is not JSON: %s", line)
		}
		out = append(out, m)
	}
	return out
}

func assertEnvelope(t *testing.T, rr *httptest.ResponseRecorder, status int, code string) map[string]any {
	t.Helper()
	if rr.Code != status {
		t.Errorf("status = %d, want %d", rr.Code, status)
	}
	if ct := rr.Header().Get("Content-Type"); ct != "application/json; charset=utf-8" {
		t.Errorf("Content-Type = %q", ct)
	}
	var env struct {
		Error map[string]json.RawMessage `json:"error"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &env); err != nil {
		t.Fatalf("body is not an error envelope: %v: %s", err, rr.Body)
	}
	for _, k := range []string{"code", "message", "details", "retryable"} {
		if _, ok := env.Error[k]; !ok {
			t.Errorf("envelope lacks %q: %s", k, rr.Body)
		}
	}
	var got map[string]any
	_ = json.Unmarshal(rr.Body.Bytes(), &got)
	body := got["error"].(map[string]any)
	if body["code"] != code {
		t.Errorf("code = %v, want %s", body["code"], code)
	}
	return body
}

func TestWriteErrorEnvelope(t *testing.T) {
	rr := httptest.NewRecorder()
	writeError(rr, &APIError{
		Status:  http.StatusUnprocessableEntity,
		Code:    CodeValidationFailed,
		Message: "brief is invalid",
		Details: []FieldError{
			{Path: "brief", Code: "too_short", Message: "must be at least 10 characters"},
			{Path: "channels[0]", Code: "not_allowed", Message: "unknown channel", Allowed: []string{"sms", "email"}},
		},
	})

	want := `{"error":{"code":"validation_failed","message":"brief is invalid","details":[` +
		`{"path":"brief","code":"too_short","message":"must be at least 10 characters"},` +
		`{"path":"channels[0]","code":"not_allowed","message":"unknown channel","allowed":["sms","email"]}` +
		`],"retryable":false}}`
	if got := strings.TrimSpace(rr.Body.String()); got != want {
		t.Errorf("body =\n%s\nwant\n%s", got, want)
	}
	assertEnvelope(t, rr, http.StatusUnprocessableEntity, CodeValidationFailed)
	if rr.Header().Get("X-Content-Type-Options") != "nosniff" {
		t.Error("missing X-Content-Type-Options: nosniff")
	}
}

func TestWriteErrorDetailsNeverNull(t *testing.T) {
	rr := httptest.NewRecorder()
	writeError(rr, &APIError{Status: http.StatusServiceUnavailable, Code: "llm_unavailable", Message: "try again", Retryable: true})
	want := `{"error":{"code":"llm_unavailable","message":"try again","details":[],"retryable":true}}`
	if got := strings.TrimSpace(rr.Body.String()); got != want {
		t.Errorf("body = %s, want %s", got, want)
	}
}

func TestHealthz(t *testing.T) {
	log, _ := newTestLogger()
	rr := httptest.NewRecorder()
	NewRouter(log).ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d", rr.Code)
	}
	if got := strings.TrimSpace(rr.Body.String()); got != `{"status":"ok"}` {
		t.Errorf("body = %s", got)
	}
}

func TestUnmatchedRoutesUseEnvelope(t *testing.T) {
	log, _ := newTestLogger()
	router := NewRouter(log)

	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/api/v1/nope", nil))
	assertEnvelope(t, rr, http.StatusNotFound, CodeNotFound)

	rr = httptest.NewRecorder()
	router.ServeHTTP(rr, httptest.NewRequest(http.MethodPost, "/healthz", nil))
	assertEnvelope(t, rr, http.StatusMethodNotAllowed, CodeMethodNotAllowed)
	if allow := rr.Header().Get("Allow"); !strings.Contains(allow, http.MethodGet) {
		t.Errorf("Allow = %q, want it to contain GET", allow)
	}
}

func TestRequestID(t *testing.T) {
	var seen string
	h := requestID(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = logging.RequestID(r.Context())
	}))

	tests := []struct {
		name     string
		incoming string
		keep     bool
	}{
		{"generated when absent", "", false},
		{"valid incoming kept", "abc-123_DEF.9", true},
		{"too long replaced", strings.Repeat("a", 65), false},
		{"log injection replaced", "abc\n{\"level\":\"ERROR\"}", false},
		{"spaces replaced", "abc def", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "/", nil)
			if tt.incoming != "" {
				req.Header.Set(RequestIDHeader, tt.incoming)
			}
			rr := httptest.NewRecorder()
			h.ServeHTTP(rr, req)

			got := rr.Header().Get(RequestIDHeader)
			if got != seen {
				t.Errorf("response id %q != context id %q", got, seen)
			}
			if tt.keep && got != tt.incoming {
				t.Errorf("id = %q, want incoming %q", got, tt.incoming)
			}
			if !tt.keep && (got == tt.incoming || len(got) != 32) {
				t.Errorf("id = %q, want a fresh 32-char hex id", got)
			}
		})
	}
}

func TestAccessLog(t *testing.T) {
	log, buf := newTestLogger()
	req := httptest.NewRequest(http.MethodGet, "/healthz?email=ayse@example.com", nil)
	req.Header.Set(RequestIDHeader, "req-1")
	NewRouter(log).ServeHTTP(httptest.NewRecorder(), req)

	lines := logLines(t, buf)
	if len(lines) != 1 {
		t.Fatalf("got %d log lines, want 1", len(lines))
	}
	l := lines[0]
	if l["msg"] != "http request" || l["method"] != "GET" || l["path"] != "/healthz" ||
		l["status"] != float64(200) || l["request_id"] != "req-1" {
		t.Errorf("access log = %v", l)
	}
	if strings.Contains(buf.String(), "ayse") {
		t.Errorf("access log contains the query string: %s", buf)
	}
}

func TestRecoverPanic(t *testing.T) {
	log, buf := newTestLogger()
	h := requestID(accessLog(log)(recoverPanic(log)(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		panic("boom for +905321234567")
	}))))

	req := httptest.NewRequest(http.MethodGet, "/x", nil)
	req.Header.Set(RequestIDHeader, "req-panic")
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	body := assertEnvelope(t, rr, http.StatusInternalServerError, CodeInternal)
	if body["message"] != "internal server error" {
		t.Errorf("message = %v", body["message"])
	}
	if strings.Contains(rr.Body.String(), "boom") || strings.Contains(rr.Body.String(), "goroutine") {
		t.Errorf("response leaks panic details: %s", rr.Body)
	}

	lines := logLines(t, buf)
	if len(lines) != 2 {
		t.Fatalf("got %d log lines, want 2 (panic + access): %s", len(lines), buf)
	}
	panicLine, access := lines[0], lines[1]
	if panicLine["level"] != "ERROR" || panicLine["request_id"] != "req-panic" ||
		panicLine["panic"] != "boom for +90********67" || panicLine["stack"] == "" {
		t.Errorf("panic log = %v", panicLine)
	}
	if access["status"] != float64(500) {
		t.Errorf("access log status = %v, want 500", access["status"])
	}
}

func TestRecoverPanicAfterHeadersWritten(t *testing.T) {
	log, _ := newTestLogger()
	h := recoverPanic(log)(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusAccepted)
		_, _ = w.Write([]byte("partial"))
		panic("late")
	}))
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/", nil))
	if rr.Code != http.StatusAccepted || rr.Body.String() != "partial" {
		t.Errorf("got %d %q, want the original partial response untouched", rr.Code, rr.Body)
	}
}

func TestRecoverPanicRepanicsAbortHandler(t *testing.T) {
	log, buf := newTestLogger()
	h := recoverPanic(log)(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		panic(http.ErrAbortHandler)
	}))
	defer func() {
		v := recover()
		if err, ok := v.(error); !ok || !errors.Is(err, http.ErrAbortHandler) {
			t.Errorf("recovered %v, want http.ErrAbortHandler", v)
		}
		if buf.Len() != 0 {
			t.Errorf("abort was logged as a panic: %s", buf)
		}
	}()
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/", nil))
	t.Fatal("ServeHTTP returned normally, want re-panic")
}

func TestStatusRecorderSupportsFlush(t *testing.T) {
	rr := httptest.NewRecorder()
	rec := &statusRecorder{ResponseWriter: rr}
	if err := http.NewResponseController(rec).Flush(); err != nil {
		t.Fatalf("Flush through recorder: %v", err)
	}
	if !rr.Flushed {
		t.Error("underlying writer was not flushed")
	}
}
