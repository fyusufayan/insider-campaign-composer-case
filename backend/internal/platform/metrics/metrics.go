// Package metrics is a minimal Prometheus text-exposition (format 0.0.4)
// registry built on the standard library (ADR-026): counters, gauges and
// histograms with **closed label sets** declared at registration.
//
// It is deliberately not a general replacement for client_golang: no
// summaries, no exemplars, no OpenMetrics/protobuf negotiation, no process
// or Go runtime collectors, no dynamic label values. Every label value
// must be one of the values declared for that label; anything else is
// recorded under the declared fallback value "other" when the label has
// one and dropped otherwise, so cardinality is fixed at registration and
// no user-controlled string can ever reach the output.
package metrics

import (
	"bytes"
	"fmt"
	"math"
	"net/http"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// ContentType is the Prometheus text exposition content type.
const ContentType = "text/plain; version=0.0.4; charset=utf-8"

// Other is the fallback label value (declare it to catch unexpected values).
const Other = "other"

var (
	nameRE  = regexp.MustCompile(`^[a-zA-Z_:][a-zA-Z0-9_:]*$`)
	labelRE = regexp.MustCompile(`^[a-zA-Z_][a-zA-Z0-9_]*$`)
	valueRE = regexp.MustCompile(`^[a-zA-Z0-9_.+-]{1,64}$`)
)

// Label is one label dimension and its closed set of values.
type Label struct {
	Name   string
	Values []string
}

type kind string

const (
	kindCounter   kind = "counter"
	kindGauge     kind = "gauge"
	kindHistogram kind = "histogram"
)

// family is one metric with all its label combinations.
type family struct {
	name, help string
	kind       kind
	labels     []Label
	index      map[string]int // label-value key → series index
	keys       [][]string     // series label values, in declaration order
	buckets    []float64      // histograms

	mu      sync.Mutex
	values  []float64 // counter/gauge value per series
	present []bool    // gauge: series currently known
	counts  [][]uint64
	sums    []float64
	totals  []uint64
}

// Registry holds metric families and renders them.
type Registry struct {
	mu       sync.Mutex
	families []*family
	names    map[string]bool
}

// NewRegistry returns an empty registry.
func NewRegistry() *Registry { return &Registry{names: map[string]bool{}} }

func (r *Registry) register(name, help string, k kind, labels []Label, buckets []float64) *family {
	if !nameRE.MatchString(name) {
		panic(fmt.Sprintf("metrics: invalid metric name %q", name)) // programming error at startup
	}
	seen := map[string]bool{}
	for _, l := range labels {
		if !labelRE.MatchString(l.Name) || strings.HasPrefix(l.Name, "__") || l.Name == "le" || seen[l.Name] || len(l.Values) == 0 {
			panic(fmt.Sprintf("metrics: invalid label %q of %s", l.Name, name))
		}
		seen[l.Name] = true
		vs := map[string]bool{}
		for _, v := range l.Values {
			if !valueRE.MatchString(v) || vs[v] {
				panic(fmt.Sprintf("metrics: invalid value %q of label %s", v, l.Name))
			}
			vs[v] = true
		}
	}
	for i := 1; i < len(buckets); i++ {
		if !(buckets[i] > buckets[i-1]) || math.IsInf(buckets[i], 0) || math.IsNaN(buckets[i]) {
			panic(fmt.Sprintf("metrics: buckets of %s must be finite and increasing", name))
		}
	}
	f := &family{name: name, help: help, kind: k, labels: labels, index: map[string]int{}, buckets: buckets}
	keys := [][]string{{}}
	for _, l := range labels {
		var next [][]string
		for _, prefix := range keys {
			for _, v := range l.Values {
				next = append(next, append(append([]string(nil), prefix...), v))
			}
		}
		keys = next
	}
	for i, k := range keys {
		f.index[strings.Join(k, "\x00")] = i
	}
	f.keys = keys
	f.values = make([]float64, len(keys))
	f.present = make([]bool, len(keys))
	if k == kindHistogram {
		f.counts = make([][]uint64, len(keys))
		for i := range f.counts {
			f.counts[i] = make([]uint64, len(buckets))
		}
		f.sums = make([]float64, len(keys))
		f.totals = make([]uint64, len(keys))
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.names[name] {
		panic(fmt.Sprintf("metrics: %s registered twice", name))
	}
	r.names[name] = true
	r.families = append(r.families, f)
	return f
}

// series resolves label values (mapping undeclared values to "other" where
// declared); ok is false when the combination cannot be recorded.
func (f *family) series(values []string) (int, bool) {
	if len(values) != len(f.labels) {
		return 0, false
	}
	key := make([]string, len(values))
	for i, v := range values {
		key[i] = v
		if !contains(f.labels[i].Values, v) {
			if !contains(f.labels[i].Values, Other) {
				return 0, false
			}
			key[i] = Other
		}
	}
	i, ok := f.index[strings.Join(key, "\x00")]
	return i, ok
}

func contains(xs []string, s string) bool {
	for _, x := range xs {
		if x == s {
			return true
		}
	}
	return false
}

// CounterVec is a monotonically increasing count per label combination,
// starting at 0 for every declared combination when the process starts.
type CounterVec struct{ f *family }

// NewCounterVec registers a counter.
func (r *Registry) NewCounterVec(name, help string, labels ...Label) *CounterVec {
	return &CounterVec{r.register(name, help, kindCounter, labels, nil)}
}

// Add adds n ≥ 0 to the series of values (negative n is ignored: counters never decrease).
func (c *CounterVec) Add(n float64, values ...string) {
	if !(n > 0) || math.IsInf(n, 0) {
		return
	}
	i, ok := c.f.series(values)
	if !ok {
		return
	}
	c.f.mu.Lock()
	c.f.values[i] += n
	c.f.mu.Unlock()
}

// Inc adds 1.
func (c *CounterVec) Inc(values ...string) { c.Add(1, values...) }

// Value returns the current value (tests).
func (c *CounterVec) Value(values ...string) float64 {
	i, ok := c.f.series(values)
	if !ok {
		return 0
	}
	c.f.mu.Lock()
	defer c.f.mu.Unlock()
	return c.f.values[i]
}

// GaugeVec is a sampled value per label combination. A series that was
// never set (or was cleared) is not exposed, so no value is invented.
type GaugeVec struct{ f *family }

// NewGaugeVec registers a gauge.
func (r *Registry) NewGaugeVec(name, help string, labels ...Label) *GaugeVec {
	return &GaugeVec{r.register(name, help, kindGauge, labels, nil)}
}

// Set sets the series of values.
func (g *GaugeVec) Set(v float64, values ...string) {
	if math.IsNaN(v) {
		return
	}
	i, ok := g.f.series(values)
	if !ok {
		return
	}
	g.f.mu.Lock()
	g.f.values[i], g.f.present[i] = v, true
	g.f.mu.Unlock()
}

// SetAll replaces every series at once: combinations missing from vals are
// set to 0. Keys are label values joined by "\x00" (see Key).
func (g *GaugeVec) SetAll(vals map[string]float64) {
	g.f.mu.Lock()
	defer g.f.mu.Unlock()
	for i, k := range g.f.keys {
		g.f.values[i], g.f.present[i] = vals[strings.Join(k, "\x00")], true
	}
}

// Clear removes every series (the sample is unknown, e.g. database down).
func (g *GaugeVec) Clear() {
	g.f.mu.Lock()
	defer g.f.mu.Unlock()
	for i := range g.f.present {
		g.f.present[i] = false
	}
}

// Key joins label values for SetAll.
func Key(values ...string) string { return strings.Join(values, "\x00") }

// HistogramVec counts observations into cumulative buckets per label combination.
type HistogramVec struct{ f *family }

// NewHistogramVec registers a histogram with the given upper bounds (+Inf is implicit).
func (r *Registry) NewHistogramVec(name, help string, buckets []float64, labels ...Label) *HistogramVec {
	if len(buckets) == 0 {
		panic("metrics: histogram without buckets")
	}
	return &HistogramVec{r.register(name, help, kindHistogram, labels, append([]float64(nil), buckets...))}
}

// Observe records v (seconds, ≥ 0).
func (h *HistogramVec) Observe(v float64, values ...string) {
	if math.IsNaN(v) || math.IsInf(v, 0) || v < 0 {
		return
	}
	i, ok := h.f.series(values)
	if !ok {
		return
	}
	h.f.mu.Lock()
	defer h.f.mu.Unlock()
	for b, ub := range h.f.buckets {
		if v <= ub {
			h.f.counts[i][b]++
		}
	}
	h.f.sums[i] += v
	h.f.totals[i]++
}

// ObserveDuration records d in seconds.
func (h *HistogramVec) ObserveDuration(d time.Duration, values ...string) {
	h.Observe(d.Seconds(), values...)
}

// Count returns the number of observations (tests).
func (h *HistogramVec) Count(values ...string) uint64 {
	i, ok := h.f.series(values)
	if !ok {
		return 0
	}
	h.f.mu.Lock()
	defer h.f.mu.Unlock()
	return h.f.totals[i]
}

// ---- exposition ----

func escapeHelp(s string) string {
	return strings.NewReplacer(`\`, `\\`, "\n", `\n`).Replace(s)
}

func escapeLabel(s string) string {
	return strings.NewReplacer(`\`, `\\`, `"`, `\"`, "\n", `\n`).Replace(s)
}

// FormatFloat renders a sample value as the text format expects.
func FormatFloat(v float64) string {
	switch {
	case math.IsInf(v, 1):
		return "+Inf"
	case math.IsInf(v, -1):
		return "-Inf"
	case math.IsNaN(v):
		return "NaN"
	}
	return strconv.FormatFloat(v, 'g', -1, 64)
}

func (f *family) labelText(i int, extra ...string) string {
	if len(f.labels) == 0 && len(extra) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteByte('{')
	for j, l := range f.labels {
		if j > 0 {
			b.WriteByte(',')
		}
		b.WriteString(l.Name)
		b.WriteString(`="`)
		b.WriteString(escapeLabel(f.keys[i][j]))
		b.WriteByte('"')
	}
	for j := 0; j+1 < len(extra); j += 2 {
		if len(f.labels) > 0 || j > 0 {
			b.WriteByte(',')
		}
		b.WriteString(extra[j])
		b.WriteString(`="`)
		b.WriteString(escapeLabel(extra[j+1]))
		b.WriteByte('"')
	}
	b.WriteByte('}')
	return b.String()
}

func (f *family) write(b *bytes.Buffer) {
	f.mu.Lock()
	defer f.mu.Unlock()
	fmt.Fprintf(b, "# HELP %s %s\n# TYPE %s %s\n", f.name, escapeHelp(f.help), f.name, f.kind)
	for i := range f.keys {
		switch f.kind {
		case kindCounter:
			fmt.Fprintf(b, "%s%s %s\n", f.name, f.labelText(i), FormatFloat(f.values[i]))
		case kindGauge:
			if f.present[i] {
				fmt.Fprintf(b, "%s%s %s\n", f.name, f.labelText(i), FormatFloat(f.values[i]))
			}
		case kindHistogram:
			for j, ub := range f.buckets {
				fmt.Fprintf(b, "%s_bucket%s %d\n", f.name, f.labelText(i, "le", FormatFloat(ub)), f.counts[i][j])
			}
			fmt.Fprintf(b, "%s_bucket%s %d\n", f.name, f.labelText(i, "le", "+Inf"), f.totals[i])
			fmt.Fprintf(b, "%s_sum%s %s\n", f.name, f.labelText(i), FormatFloat(f.sums[i]))
			fmt.Fprintf(b, "%s_count%s %d\n", f.name, f.labelText(i), f.totals[i])
		}
	}
}

// WriteText renders every family (sorted by name) in the text format.
func (r *Registry) WriteText(b *bytes.Buffer) {
	r.mu.Lock()
	fams := append([]*family(nil), r.families...)
	r.mu.Unlock()
	sort.Slice(fams, func(i, j int) bool { return fams[i].name < fams[j].name })
	for _, f := range fams {
		f.write(b)
	}
}

// Handler serves GET/HEAD /metrics. It renders in-memory values only (no
// I/O besides the response), so a scrape is bounded and unaffected by a
// database outage.
func (r *Registry) Handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.Method != http.MethodGet && req.Method != http.MethodHead {
			w.Header().Set("Allow", "GET, HEAD")
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		var b bytes.Buffer
		r.WriteText(&b)
		h := w.Header()
		h.Set("Content-Type", ContentType)
		h.Set("Cache-Control", "no-store")
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Content-Length", strconv.Itoa(b.Len()))
		w.WriteHeader(http.StatusOK)
		if req.Method == http.MethodGet {
			_, _ = w.Write(b.Bytes())
		}
	})
}
