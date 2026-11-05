package httpapi

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/fyusufayan/insider-campaign-composer-case/backend/internal/platform/metrics"
	"github.com/fyusufayan/insider-campaign-composer-case/backend/internal/platform/metrics/metricstest"
)

// GET /metrics is served from memory: it answers while the database is down
// (readiness fails at the same time) and is unaffected by the router's
// error envelope.
func TestMetricsRoute(t *testing.T) {
	log, _ := newTestLogger()
	reg := metrics.NewRegistry()
	llmM := metrics.NewLLM(reg)
	llmM.Requests.Inc("segment_interpretation", "ok")
	router := NewRouter(log, Deps{Metrics: reg.Handler(), Readiness: func(context.Context) (ReadinessReport, error) {
		return ReadinessReport{Database: "unavailable"}, errors.New("db down")
	}})
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/readyz", nil))
	if rr.Code != 503 {
		t.Fatalf("readyz = %d", rr.Code)
	}
	rr = httptest.NewRecorder()
	router.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	if rr.Code != 200 || rr.Header().Get("Content-Type") != metrics.ContentType {
		t.Fatalf("metrics = %d %q", rr.Code, rr.Header().Get("Content-Type"))
	}
	e, err := metricstest.Parse(rr.Body.String())
	if err != nil {
		t.Fatal(err)
	}
	if v, _ := e.Value("llm_requests_total", "task", "segment_interpretation", "outcome", "ok"); v != 1 {
		t.Errorf("llm_requests_total = %v", v)
	}
	// The API exposes only what it measures: no worker metrics are invented here.
	if strings.Contains(rr.Body.String(), "delivery_queue_depth") || strings.Contains(rr.Body.String(), "provider_") {
		t.Error("API exposes worker metrics")
	}
	rr = httptest.NewRecorder()
	router.ServeHTTP(rr, httptest.NewRequest(http.MethodPost, "/metrics", nil))
	if rr.Code != 405 {
		t.Errorf("POST /metrics = %d", rr.Code)
	}
	// Without a registry the route does not exist.
	rr = httptest.NewRecorder()
	NewRouter(log, Deps{}).ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	if rr.Code != 404 {
		t.Errorf("no registry = %d", rr.Code)
	}
}
