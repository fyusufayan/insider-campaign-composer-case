package httpapi

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"log/slog"
	"net/http"
	"runtime/debug"
	"time"

	"github.com/fyusufayan/insider-campaign-composer-case/backend/internal/platform/logging"
)

// RequestIDHeader carries the request id in requests and responses.
const RequestIDHeader = "X-Request-ID"

const maxRequestIDLen = 64

// requestID accepts a well-formed incoming X-Request-ID or generates one, stores
// it in the request context (picked up by the logger) and echoes it back.
func requestID(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := r.Header.Get(RequestIDHeader)
		if !validRequestID(id) {
			id = newRequestID()
		}
		w.Header().Set(RequestIDHeader, id)
		next.ServeHTTP(w, r.WithContext(logging.WithRequestID(r.Context(), id)))
	})
}

// validRequestID restricts client-supplied ids to a short token charset so they
// cannot inject content into logs or headers.
func validRequestID(id string) bool {
	if id == "" || len(id) > maxRequestIDLen {
		return false
	}
	for i := 0; i < len(id); i++ {
		c := id[i]
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9', c == '-', c == '_', c == '.':
		default:
			return false
		}
	}
	return true
}

func newRequestID() string {
	var b [16]byte
	_, _ = rand.Read(b[:]) // crypto/rand.Read never returns an error
	return hex.EncodeToString(b[:])
}

// statusRecorder captures the response status and size for the access log.
// Unwrap lets http.ResponseController reach Flush on the underlying writer (SSE).
type statusRecorder struct {
	http.ResponseWriter
	status int
	bytes  int
}

func (rec *statusRecorder) WriteHeader(status int) {
	if rec.status == 0 {
		rec.status = status
	}
	rec.ResponseWriter.WriteHeader(status)
}

func (rec *statusRecorder) Write(p []byte) (int, error) {
	if rec.status == 0 {
		rec.status = http.StatusOK
	}
	n, err := rec.ResponseWriter.Write(p)
	rec.bytes += n
	return n, err
}

func (rec *statusRecorder) Unwrap() http.ResponseWriter { return rec.ResponseWriter }

func (rec *statusRecorder) wroteHeader() bool { return rec.status != 0 }

// accessLog logs one line per request. The query string is not logged because
// it may carry user data.
func accessLog(log *slog.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			start := time.Now()
			rec := &statusRecorder{ResponseWriter: w}
			next.ServeHTTP(rec, r)
			status := rec.status
			if status == 0 {
				status = http.StatusOK
			}
			log.InfoContext(r.Context(), "http request",
				"method", r.Method,
				"path", r.URL.Path,
				"status", status,
				"bytes", rec.bytes,
				"duration_ms", time.Since(start).Milliseconds(),
			)
		})
	}
}

// recoverPanic turns a handler panic into a logged 500 with the error envelope.
// http.ErrAbortHandler is re-panicked so net/http aborts the response as intended.
func recoverPanic(log *slog.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			rec := &statusRecorder{ResponseWriter: w}
			defer func() {
				v := recover()
				if v == nil {
					return
				}
				if err, ok := v.(error); ok && errors.Is(err, http.ErrAbortHandler) {
					panic(v)
				}
				log.ErrorContext(r.Context(), "panic in http handler",
					"panic", v,
					"stack", string(debug.Stack()),
				)
				if rec.wroteHeader() {
					// Too late for an error response; the client sees a truncated body.
					return
				}
				writeError(rec, &APIError{
					Status:  http.StatusInternalServerError,
					Code:    CodeInternal,
					Message: "internal server error",
				})
			}()
			next.ServeHTTP(rec, r)
		})
	}
}
