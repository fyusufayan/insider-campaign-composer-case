// Package metricstest parses and validates Prometheus text exposition
// (format 0.0.4) strictly, for tests: every sample belongs to a family
// declared by HELP and TYPE before it, names and labels are well formed,
// values are numbers, and histograms are cumulative with +Inf = _count.
package metricstest

import (
	"fmt"
	"math"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// Exposition is a parsed scrape.
type Exposition struct {
	Types   map[string]string // family → counter | gauge | histogram
	Help    map[string]string
	Samples map[string]float64 // `name{a="x",b="y"}` (labels sorted) → value
	// Labels maps every sample key to its label set.
	Labels map[string]map[string]string
	// Names lists each sample's metric name (keys of Samples → name).
	Names map[string]string
}

var (
	helpRE   = regexp.MustCompile(`^# HELP ([a-zA-Z_:][a-zA-Z0-9_:]*) (.*)$`)
	typeRE   = regexp.MustCompile(`^# TYPE ([a-zA-Z_:][a-zA-Z0-9_:]*) (counter|gauge|histogram|summary|untyped)$`)
	sampleRE = regexp.MustCompile(`^([a-zA-Z_:][a-zA-Z0-9_:]*)(\{(.*)\})? (\S+)$`)
	labelRE  = regexp.MustCompile(`^([a-zA-Z_][a-zA-Z0-9_]*)="((?:[^"\\\n]|\\["\\n])*)"`)
)

// Parse validates text and returns its content.
func Parse(text string) (*Exposition, error) {
	e := &Exposition{Types: map[string]string{}, Help: map[string]string{}, Samples: map[string]float64{},
		Labels: map[string]map[string]string{}, Names: map[string]string{}}
	if text != "" && !strings.HasSuffix(text, "\n") {
		return nil, fmt.Errorf("exposition does not end with a newline")
	}
	for n, line := range strings.Split(strings.TrimSuffix(text, "\n"), "\n") {
		if text == "" {
			break
		}
		fail := func(format string, a ...any) error {
			return fmt.Errorf("line %d %q: %s", n+1, line, fmt.Sprintf(format, a...))
		}
		switch {
		case strings.HasPrefix(line, "# HELP "):
			m := helpRE.FindStringSubmatch(line)
			if m == nil {
				return nil, fail("malformed HELP")
			}
			if _, dup := e.Help[m[1]]; dup {
				return nil, fail("duplicate HELP")
			}
			e.Help[m[1]] = m[2]
		case strings.HasPrefix(line, "# TYPE "):
			m := typeRE.FindStringSubmatch(line)
			if m == nil {
				return nil, fail("malformed TYPE")
			}
			if _, dup := e.Types[m[1]]; dup {
				return nil, fail("duplicate TYPE")
			}
			if _, ok := e.Help[m[1]]; !ok {
				return nil, fail("TYPE before HELP")
			}
			e.Types[m[1]] = m[2]
		case strings.HasPrefix(line, "#") || line == "":
			return nil, fail("unexpected comment or blank line")
		default:
			m := sampleRE.FindStringSubmatch(line)
			if m == nil {
				return nil, fail("malformed sample")
			}
			name, body, raw := m[1], m[3], m[4]
			family := name
			for _, suf := range []string{"_bucket", "_sum", "_count"} {
				if f := strings.TrimSuffix(name, suf); f != name && e.Types[f] == "histogram" {
					family = f
				}
			}
			if _, ok := e.Types[family]; !ok {
				return nil, fail("sample of undeclared family %s", family)
			}
			labels := map[string]string{}
			for body != "" {
				lm := labelRE.FindStringSubmatch(body)
				if lm == nil {
					return nil, fail("malformed labels")
				}
				if _, dup := labels[lm[1]]; dup {
					return nil, fail("duplicate label %s", lm[1])
				}
				labels[lm[1]] = strings.NewReplacer(`\\`, `\`, `\"`, `"`, `\n`, "\n").Replace(lm[2])
				body = strings.TrimPrefix(body[len(lm[0]):], ",")
			}
			v, err := parseValue(raw)
			if err != nil {
				return nil, fail("value: %v", err)
			}
			key := Key(name, labels)
			if _, dup := e.Samples[key]; dup {
				return nil, fail("duplicate series")
			}
			e.Samples[key], e.Labels[key], e.Names[key] = v, labels, name
		}
	}
	return e, e.checkHistograms()
}

func parseValue(s string) (float64, error) {
	switch s {
	case "+Inf":
		return math.Inf(1), nil
	case "-Inf":
		return math.Inf(-1), nil
	case "NaN":
		return math.NaN(), nil
	}
	return strconv.ParseFloat(s, 64)
}

// Key renders a series key with sorted labels.
func Key(name string, labels map[string]string) string {
	if len(labels) == 0 {
		return name
	}
	ks := make([]string, 0, len(labels))
	for k := range labels {
		ks = append(ks, k)
	}
	sort.Strings(ks)
	parts := make([]string, len(ks))
	for i, k := range ks {
		parts[i] = fmt.Sprintf("%s=%q", k, labels[k])
	}
	return name + "{" + strings.Join(parts, ",") + "}"
}

// Value returns the sample of name with labels given as k, v pairs (ok false if absent).
func (e *Exposition) Value(name string, kv ...string) (float64, bool) {
	labels := map[string]string{}
	for i := 0; i+1 < len(kv); i += 2 {
		labels[kv[i]] = kv[i+1]
	}
	v, ok := e.Samples[Key(name, labels)]
	return v, ok
}

// Sum adds every sample of name whose labels include kv.
func (e *Exposition) Sum(name string, kv ...string) float64 {
	var s float64
	for key, v := range e.Samples {
		if e.Names[key] != name {
			continue
		}
		match := true
		for i := 0; i+1 < len(kv); i += 2 {
			if e.Labels[key][kv[i]] != kv[i+1] {
				match = false
			}
		}
		if match {
			s += v
		}
	}
	return s
}

// SeriesOf counts the samples named name.
func (e *Exposition) SeriesOf(name string) int {
	n := 0
	for _, s := range e.Names {
		if s == name {
			n++
		}
	}
	return n
}

func (e *Exposition) checkHistograms() error {
	type group struct {
		les    []float64
		counts []float64
	}
	groups := map[string]*group{}
	for key, name := range e.Names {
		f := strings.TrimSuffix(name, "_bucket")
		if f == name || e.Types[f] != "histogram" {
			continue
		}
		labels := map[string]string{}
		var le float64
		for k, v := range e.Labels[key] {
			if k == "le" {
				var err error
				if le, err = parseValue(v); err != nil {
					return fmt.Errorf("%s: bad le %q", key, v)
				}
				continue
			}
			labels[k] = v
		}
		gk := Key(f, labels)
		g := groups[gk]
		if g == nil {
			g = &group{}
			groups[gk] = g
		}
		g.les, g.counts = append(g.les, le), append(g.counts, e.Samples[key])
	}
	for gk, g := range groups {
		idx := make([]int, len(g.les))
		for i := range idx {
			idx[i] = i
		}
		sort.Slice(idx, func(a, b int) bool { return g.les[idx[a]] < g.les[idx[b]] })
		for i := 1; i < len(idx); i++ {
			if g.counts[idx[i]] < g.counts[idx[i-1]] {
				return fmt.Errorf("%s: buckets not cumulative", gk)
			}
		}
		last := idx[len(idx)-1]
		if !math.IsInf(g.les[last], 1) {
			return fmt.Errorf("%s: no +Inf bucket", gk)
		}
		count, ok := e.Samples[strings.Replace(gk, "{", "_count{", 1)]
		if !strings.Contains(gk, "{") {
			count, ok = e.Samples[gk+"_count"]
		}
		if !ok || count != g.counts[last] {
			return fmt.Errorf("%s: +Inf bucket %v ≠ _count %v", gk, g.counts[last], count)
		}
	}
	return nil
}
