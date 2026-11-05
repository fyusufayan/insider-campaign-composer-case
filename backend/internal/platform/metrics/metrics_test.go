package metrics_test

import (
	"bytes"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/fyusufayan/insider-campaign-composer-case/backend/internal/platform/metrics"
	"github.com/fyusufayan/insider-campaign-composer-case/backend/internal/platform/metrics/metricstest"
)

func scrape(t *testing.T, r *metrics.Registry) (*metricstest.Exposition, string) {
	t.Helper()
	rr := httptest.NewRecorder()
	r.Handler().ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	if rr.Code != 200 || rr.Header().Get("Content-Type") != metrics.ContentType {
		t.Fatalf("scrape → %d %q", rr.Code, rr.Header().Get("Content-Type"))
	}
	e, err := metricstest.Parse(rr.Body.String())
	if err != nil {
		t.Fatalf("invalid exposition: %v\n%s", err, rr.Body)
	}
	return e, rr.Body.String()
}

func TestExpositionFormat(t *testing.T) {
	r := metrics.NewRegistry()
	c := r.NewCounterVec("b_total", "A counter with a \\ backslash\nand a newline.", metrics.Label{Name: "kind", Values: []string{"x", "other"}})
	g := r.NewGaugeVec("a_depth", "A gauge.", metrics.Label{Name: "k", Values: []string{"p", "q"}})
	h := r.NewHistogramVec("c_seconds", "A histogram.", []float64{0.1, 1}, metrics.Label{Name: "ch", Values: []string{"sms"}})
	c.Inc("x")
	c.Add(2.5, "x")
	c.Inc(`evil"} 1\n# TYPE x counter`) // undeclared → "other"
	c.Add(-5, "x")                      // counters never decrease
	c.Inc("x", "extra")                 // wrong arity → dropped
	h.Observe(0.05, "sms")
	h.Observe(0.5, "sms")
	h.Observe(7, "sms")
	h.Observe(-1, "sms")
	h.Observe(math.NaN(), "sms")
	e, text := scrape(t, r)
	want := `# HELP a_depth A gauge.
# TYPE a_depth gauge
# HELP b_total A counter with a \\ backslash\nand a newline.
# TYPE b_total counter
b_total{kind="x"} 3.5
b_total{kind="other"} 1
# HELP c_seconds A histogram.
# TYPE c_seconds histogram
c_seconds_bucket{ch="sms",le="0.1"} 1
c_seconds_bucket{ch="sms",le="1"} 2
c_seconds_bucket{ch="sms",le="+Inf"} 3
c_seconds_sum{ch="sms"} 7.55
c_seconds_count{ch="sms"} 3
`
	if text != want {
		t.Errorf("exposition:\n%s\nwant:\n%s", text, want)
	}
	if e.Types["a_depth"] != "gauge" || e.Types["b_total"] != "counter" || e.Types["c_seconds"] != "histogram" {
		t.Errorf("types = %v", e.Types)
	}
	// A gauge is absent until sampled, all-zero-filled by SetAll, absent again after Clear.
	g.SetAll(map[string]float64{metrics.Key("p"): 4})
	e, _ = scrape(t, r)
	if v, ok := e.Value("a_depth", "k", "p"); !ok || v != 4 {
		t.Errorf("p = %v %v", v, ok)
	}
	if v, ok := e.Value("a_depth", "k", "q"); !ok || v != 0 {
		t.Errorf("q = %v %v", v, ok)
	}
	g.Clear()
	if e, _ = scrape(t, r); e.SeriesOf("a_depth") != 0 {
		t.Error("cleared gauge still exposed")
	}
	g.Set(2, "q")
	if e, _ = scrape(t, r); e.SeriesOf("a_depth") != 1 {
		t.Error("Set exposes exactly one series")
	}
}

func TestHandlerMethodsAndHead(t *testing.T) {
	r := metrics.NewRegistry()
	r.NewCounterVec("x_total", "x")
	for method, want := range map[string]int{http.MethodGet: 200, http.MethodHead: 200, http.MethodPost: 405} {
		rr := httptest.NewRecorder()
		r.Handler().ServeHTTP(rr, httptest.NewRequest(method, "/metrics", nil))
		if rr.Code != want {
			t.Errorf("%s → %d", method, rr.Code)
		}
		if method == http.MethodHead && rr.Body.Len() != 0 {
			t.Error("HEAD has a body")
		}
	}
}

func TestRegistrationRejectsInvalidDefinitions(t *testing.T) {
	cases := map[string]func(r *metrics.Registry){
		"bad name": func(r *metrics.Registry) { r.NewCounterVec("1bad", "h") },
		"bad label": func(r *metrics.Registry) {
			r.NewCounterVec("ok_total", "h", metrics.Label{Name: "a-b", Values: []string{"x"}})
		},
		"reserved le": func(r *metrics.Registry) {
			r.NewCounterVec("ok_total", "h", metrics.Label{Name: "le", Values: []string{"x"}})
		},
		"unsafe value": func(r *metrics.Registry) {
			r.NewCounterVec("ok_total", "h", metrics.Label{Name: "a", Values: []string{`x"y`}})
		},
		"no values":         func(r *metrics.Registry) { r.NewCounterVec("ok_total", "h", metrics.Label{Name: "a"}) },
		"duplicate name":    func(r *metrics.Registry) { r.NewCounterVec("d_total", "h"); r.NewCounterVec("d_total", "h") },
		"unordered buckets": func(r *metrics.Registry) { r.NewHistogramVec("h_seconds", "h", []float64{1, 0.5}) },
	}
	for name, fn := range cases {
		func() {
			defer func() {
				if recover() == nil {
					t.Errorf("%s: accepted", name)
				}
			}()
			fn(metrics.NewRegistry())
		}()
	}
}

// The six documented core metrics: names, types and closed, bounded label sets.
func TestCoreMetricDefinitions(t *testing.T) {
	r := metrics.NewRegistry()
	metrics.NewDelivery(r)
	metrics.NewLLM(r)
	e, text := scrape(t, r)
	want := map[string]string{
		"deliveries_total": "counter", "provider_request_duration_seconds": "histogram", "provider_rate_limited_total": "counter",
		"delivery_queue_depth": "gauge", "llm_requests_total": "counter", "llm_request_duration_seconds": "histogram",
	}
	if len(e.Types) != len(want) {
		t.Errorf("families = %v", e.Types)
	}
	for name, typ := range want {
		if e.Types[name] != typ || e.Help[name] == "" {
			t.Errorf("%s: type %q help %q", name, e.Types[name], e.Help[name])
		}
	}
	// Fresh process: counters at zero for every declared combination,
	// histograms empty, the queue gauge absent until the first sample.
	if n := e.SeriesOf("deliveries_total"); n != 10 || e.Sum("deliveries_total") != 0 {
		t.Errorf("deliveries_total: %d series, sum %v", n, e.Sum("deliveries_total"))
	}
	if n := e.SeriesOf("llm_requests_total"); n != 27 {
		t.Errorf("llm_requests_total series = %d", n)
	}
	if e.SeriesOf("delivery_queue_depth") != 0 {
		t.Error("queue depth exposed before any sample")
	}
	allowed := map[string]bool{"channel": true, "outcome": true, "status": true, "task": true, "le": true}
	for key, labels := range e.Labels {
		for k := range labels {
			if !allowed[k] {
				t.Errorf("%s: unexpected label %s", key, k)
			}
		}
	}
	if len(e.Samples) > 200 || len(text) > 32<<10 {
		t.Errorf("output not bounded: %d series, %d bytes", len(e.Samples), len(text))
	}
}

func TestConcurrentUpdatesAndScrapes(t *testing.T) {
	r := metrics.NewRegistry()
	d := metrics.NewDelivery(r)
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(2)
		go func() {
			defer wg.Done()
			for j := 0; j < 500; j++ {
				d.Deliveries.Inc("sms", "sent")
				d.Duration.Observe(0.02, "email")
				d.QueueDepth.Set(float64(j), "sms", "pending")
			}
		}()
		go func() {
			defer wg.Done()
			for j := 0; j < 20; j++ {
				var b bytes.Buffer
				r.WriteText(&b)
				if _, err := metricstest.Parse(b.String()); err != nil {
					t.Error(err)
					return
				}
			}
		}()
	}
	wg.Wait()
	e, _ := scrape(t, r)
	if v, _ := e.Value("deliveries_total", "channel", "sms", "outcome", "sent"); v != 4000 {
		t.Errorf("sent = %v", v)
	}
	if v, _ := e.Value("provider_request_duration_seconds_count", "channel", "email"); v != 4000 {
		t.Errorf("count = %v", v)
	}
}

func TestFormatFloat(t *testing.T) {
	for v, want := range map[float64]string{0: "0", 1.5: "1.5", 1e21: "1e+21", math.Inf(1): "+Inf", math.Inf(-1): "-Inf"} {
		if got := metrics.FormatFloat(v); got != want {
			t.Errorf("%v → %s", v, got)
		}
	}
	if metrics.FormatFloat(math.NaN()) != "NaN" {
		t.Error("NaN")
	}
	if strings.Contains(metrics.FormatFloat(0.1), " ") {
		t.Error("space in value")
	}
}
