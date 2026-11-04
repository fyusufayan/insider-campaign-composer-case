// Package httpapi is the HTTP transport: routing, middleware, DTOs and error
// mapping. It contains no business rules and no SQL.
package httpapi

import (
	"log/slog"
	"net/http"

	"github.com/fyusufayan/insider-campaign-composer-case/backend/internal/campaign"
)

// Deps are the services behind the routes. A nil dependency leaves its
// routes unregistered (used by middleware tests).
type Deps struct {
	Campaigns *campaign.Service
	Readiness ReadinessFunc
}

// NewRouter returns the API handler with all middleware applied.
// Order (outermost first): request id → access log → panic recovery → routes,
// so that the access log records the 500 produced by a recovered panic.
func NewRouter(log *slog.Logger, deps Deps) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", handleHealthz)
	if deps.Readiness != nil {
		mux.HandleFunc("GET /readyz", handleReadyz(deps.Readiness, log))
	}
	if deps.Campaigns != nil {
		(&campaignHandlers{svc: deps.Campaigns, log: log}).register(mux)
	}

	var h http.Handler = envelopeUnmatched(mux)
	h = recoverPanic(log)(h)
	h = accessLog(log)(h)
	h = requestID(h)
	return h
}

// envelopeUnmatched answers requests that match no route with the error
// envelope instead of ServeMux's plain-text 404/405, keeping the Allow header.
func envelopeUnmatched(mux *http.ServeMux) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h, pattern := mux.Handler(r)
		if pattern != "" {
			mux.ServeHTTP(w, r)
			return
		}
		// Let the mux's own fallback decide between 404 and 405 (and set Allow),
		// but discard its plain-text body.
		probe := &headerProbe{header: http.Header{}}
		h.ServeHTTP(probe, r)
		if allow := probe.header.Values("Allow"); len(allow) > 0 {
			w.Header()["Allow"] = allow
		}
		if probe.status == http.StatusMethodNotAllowed {
			writeError(w, &APIError{Status: http.StatusMethodNotAllowed, Code: CodeMethodNotAllowed, Message: "method not allowed"})
			return
		}
		writeError(w, &APIError{Status: http.StatusNotFound, Code: CodeNotFound, Message: "resource not found"})
	})
}

// headerProbe records the status and headers a fallback handler would send.
type headerProbe struct {
	header http.Header
	status int
}

func (p *headerProbe) Header() http.Header { return p.header }
func (p *headerProbe) WriteHeader(status int) {
	if p.status == 0 {
		p.status = status
	}
}
func (p *headerProbe) Write(b []byte) (int, error) {
	if p.status == 0 {
		p.status = http.StatusOK
	}
	return len(b), nil
}
