package ai

import (
	"bytes"
	"flag"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/fyusufayan/insider-campaign-composer-case/backend/internal/segment"
)

var update = flag.Bool("update", false, "rewrite golden files")

const schemaGolden = "testdata/segment_schema.golden.json"

func TestSchemaGolden(t *testing.T) {
	got := append(InterpretationSchemaJSON(), '\n')
	if *update {
		if err := os.MkdirAll(filepath.Dir(schemaGolden), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(schemaGolden, got, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(schemaGolden)
	if err != nil {
		t.Fatalf("read golden (run go test ./internal/ai -run TestSchemaGolden -update): %v", err)
	}
	if !bytes.Equal(got, want) {
		t.Errorf("schema differs from %s; if intended, bump SegmentPromptVersion and rerun with -update", schemaGolden)
	}
}

func TestSchemaDeterministic(t *testing.T) {
	first := InterpretationSchemaJSON()
	for range 20 {
		if !bytes.Equal(first, InterpretationSchemaJSON()) {
			t.Fatal("schema encoding is not deterministic")
		}
	}
	// A fresh value each call: mutating one must not affect the next.
	s := InterpretationSchema()
	s["type"] = "mutated"
	if !bytes.Equal(first, InterpretationSchemaJSON()) {
		t.Fatal("schema shares state between calls")
	}
}

// walkSchema visits every schema node (not property or $defs maps).
func walkSchema(node map[string]any, fn func(map[string]any)) {
	fn(node)
	for _, key := range []string{"properties", "$defs"} {
		if m, ok := node[key].(map[string]any); ok {
			for _, child := range m {
				walkSchema(child.(map[string]any), fn)
			}
		}
	}
	if items, ok := node["items"].(map[string]any); ok {
		walkSchema(items, fn)
	}
	if alts, ok := node["anyOf"].([]any); ok {
		for _, a := range alts {
			walkSchema(a.(map[string]any), fn)
		}
	}
}

// Only keywords supported by structured outputs; every object is closed and
// lists only existing properties as required; every $ref resolves.
func TestSchemaKeywords(t *testing.T) {
	allowed := []string{"type", "enum", "properties", "required", "additionalProperties", "items", "anyOf", "$ref", "$defs", "description"}
	root := InterpretationSchema()
	defs := root["$defs"].(map[string]any)
	walkSchema(root, func(m map[string]any) {
		for k := range m {
			if !slices.Contains(allowed, k) {
				t.Errorf("unsupported keyword %q", k)
			}
		}
		if ref, ok := m["$ref"].(string); ok {
			if _, ok := defs[strings.TrimPrefix(ref, "#/$defs/")]; !ok || !strings.HasPrefix(ref, "#/$defs/") {
				t.Errorf("dangling $ref %q", ref)
			}
		}
		if m["type"] == "object" {
			if m["additionalProperties"] != false {
				t.Errorf("object without additionalProperties:false: %v", m)
			}
			props := m["properties"].(map[string]any)
			for _, r := range m["required"].([]any) {
				if _, ok := props[r.(string)]; !ok {
					t.Errorf("required %q is not a property", r)
				}
			}
		}
	})
}

func enumAt(t *testing.T, v any, path ...string) []string {
	t.Helper()
	for _, p := range path {
		m, ok := v.(map[string]any)
		if !ok {
			t.Fatalf("path %v: not an object", path)
		}
		v = m[p]
	}
	m := v.(map[string]any)
	var out []string
	for _, e := range m["enum"].([]any) {
		out = append(out, e.(string))
	}
	return out
}

func conditionVariant(t *testing.T, s map[string]any, typ string) map[string]any {
	t.Helper()
	cond := s["$defs"].(map[string]any)["condition"].(map[string]any)
	for _, v := range cond["anyOf"].([]any) {
		m := v.(map[string]any)
		if enumAt(t, m, "properties", "type")[0] == typ {
			return m
		}
	}
	t.Fatalf("no %s variant", typ)
	return nil
}

// The schema's enums are exactly the registry vocabulary.
func TestSchemaMatchesRegistry(t *testing.T) {
	s := InterpretationSchema()
	var fields []string
	for _, f := range segment.Fields() {
		fields = append(fields, string(f.Field))
	}
	attr := conditionVariant(t, s, "attribute")
	ev := conditionVariant(t, s, "event")
	beh := conditionVariant(t, s, "behavior")
	checks := []struct {
		name      string
		got, want []string
	}{
		{"fields", enumAt(t, attr, "properties", "field"), fields},
		{"operators", enumAt(t, attr, "properties", "op"), segment.Operators()},
		{"events", enumAt(t, ev, "properties", "event"), segment.EventNames()},
		{"event ops", enumAt(t, ev, "properties", "op"), segment.EventOps()},
		{"categories", enumAt(t, ev, "properties", "category_in", "items"), segment.Categories()},
		{"predicates", enumAt(t, beh, "properties", "predicate"), segment.BehaviorPredicates()},
		{"match", enumAt(t, s, "$defs", "rule", "properties", "match"), segment.MatchValues()},
		{"channels", enumAt(t, s, "properties", keySuggestedChannels, "items"), []string{"sms", "email"}},
		{"length", enumAt(t, s, "properties", keyContentBrief, "properties", keyLength), lengthValues},
	}
	for _, c := range checks {
		if !slices.Equal(c.got, c.want) {
			t.Errorf("%s: schema %v, registry %v", c.name, c.got, c.want)
		}
	}
	var types []string
	for _, v := range s["$defs"].(map[string]any)["condition"].(map[string]any)["anyOf"].([]any) {
		types = append(types, enumAt(t, v, "properties", "type")[0])
	}
	if !slices.Equal(types, segment.ConditionTypes()) {
		t.Errorf("condition types %v, want %v", types, segment.ConditionTypes())
	}
	var offerKinds []string
	for _, v := range s["properties"].(map[string]any)[keyContentBrief].(map[string]any)["properties"].(map[string]any)[keyOffer].(map[string]any)["anyOf"].([]any) {
		offerKinds = append(offerKinds, enumAt(t, v, "properties", keyKind)...)
	}
	slices.Sort(offerKinds)
	want := slices.Clone(offerKindValues)
	slices.Sort(want)
	if !slices.Equal(offerKinds, want) {
		t.Errorf("offer kinds %v, want %v", offerKinds, want)
	}
	if !slices.Equal(stringsOf(s["required"]), envelopeKeys) {
		t.Errorf("envelope required = %v", s["required"])
	}
}

func stringsOf(v any) []string {
	var out []string
	for _, x := range v.([]any) {
		out = append(out, x.(string))
	}
	return out
}

func TestSchemaHasNoSQLOrCustomerData(t *testing.T) {
	js := strings.ToLower(string(InterpretationSchemaJSON()))
	for _, bad := range []string{"select ", "sql", "phone", "\"email_address", "consent", "opt_in", "first_name"} {
		if strings.Contains(js, bad) {
			t.Errorf("schema mentions %q", bad)
		}
	}
}
