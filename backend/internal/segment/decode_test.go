package segment

import (
	"bytes"
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

// pc renders errors as "path:code" for compact assertions.
func pc(t *testing.T, err error) []string {
	t.Helper()
	if err == nil {
		return nil
	}
	errs, ok := AsErrors(err)
	if !ok {
		t.Fatalf("error %v (%T) is not segment.Errors", err, err)
	}
	if len(errs) == 0 { // typed nil Errors from Validate
		return nil
	}
	out := make([]string, len(errs))
	for i, e := range errs {
		if e.Message == "" {
			t.Errorf("error %s:%s has no message", e.Path, e.Code)
		}
		out[i] = e.Path + ":" + e.Code
	}
	return out
}

const okAttr = `{"type":"attribute","field":"city","op":"eq","value":"Istanbul"}`

func TestDecodeValidDocuments(t *testing.T) {
	cnt := 3
	tests := []struct {
		name string
		in   string
		want Rule
	}{
		{
			"example brief",
			`{"match":"all","conditions":[` + okAttr + `,{"type":"behavior","predicate":"abandoned_cart","within_days":30}]}`,
			Rule{Match: MatchAll, Conditions: []Condition{
				AttributeCondition{FieldCity, OpEq, StringValue("Istanbul")},
				BehaviorCondition{PredicateAbandonedCart, 30},
			}},
		},
		{
			"every value shape and groups",
			` {"version":1,"match":"any","conditions":[],"groups":[{"match":"all","conditions":[
				{"type":"attribute","field":"language","op":"in","value":["tr","en"]},
				{"type":"attribute","field":"age","op":"between","value":{"min":18,"max":35}},
				{"type":"attribute","field":"signup_days_ago","op":"gte","value":0},
				{"type":"event","event":"purchase","op":"count_gte","count":3,"within_days":90,"category_in":["books"]}]}]} `,
			Rule{Version: 1, Match: MatchAny, Conditions: []Condition{}, Groups: []Group{{Match: MatchAll, Conditions: []Condition{
				AttributeCondition{FieldLanguage, OpIn, StringsValue("tr", "en")},
				AttributeCondition{FieldAge, OpBetween, RangeValue(18, 35)},
				AttributeCondition{FieldSignupDaysAgo, OpGte, IntValue(0)},
				EventCondition{Event: EventPurchase, Op: EventCountGte, Count: &cnt, WithinDays: 90, CategoryIn: []string{"books"}},
			}}}},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := Decode([]byte(tt.in))
			if err != nil {
				t.Fatalf("Decode: %v", err)
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("Decode =\n%#v\nwant\n%#v", got, tt.want)
			}
		})
	}
}

func TestDecodeRejects(t *testing.T) {
	cond := func(c string) string { return `{"match":"all","conditions":[` + c + `]}` }
	tests := []struct {
		name string
		in   string
		want []string // exact "path:code" list
	}{
		// malformed / framing
		{"empty input", ``, []string{":malformed_json"}},
		{"whitespace only", "  \n", []string{":malformed_json"}},
		{"truncated", `{"match":"all"`, []string{":malformed_json"}},
		{"trailing comma", `{"match":"all",}`, []string{":malformed_json"}},
		{"single quotes", `{'match':'all'}`, []string{":malformed_json"}},
		{"comment", `{"match":"all" /* x */}`, []string{":malformed_json"}},
		{"bare value", `all`, []string{":malformed_json"}},
		{"trailing object", `{"match":"all","conditions":[` + okAttr + `]}{}`, []string{":trailing_data"}},
		{"trailing garbage", `{"match":"all","conditions":[` + okAttr + `]} x`, []string{":trailing_data"}},
		{"trailing number", `{"match":"all","conditions":[` + okAttr + `]} 1`, []string{":trailing_data"}},
		{"invalid utf8", "{\"match\":\"all\xff\",\"conditions\":[]}", []string{":malformed_json"}},
		{"too large", `{"match":"all","conditions":[],"x":"` + strings.Repeat("a", MaxRuleBytes) + `"}`, []string{":too_large"}},
		{"too deep", `{"match":"all","conditions":[{"type":"attribute","field":"city","op":"in","value":[[[[[[["x"]]]]]]]}]}`, []string{"conditions[0].value[0][0][0][0][0]:too_deep"}},
		{"duplicate key", `{"match":"all","match":"any","conditions":[]}`, []string{"match:duplicate_property"}},
		{"duplicate key in condition", cond(`{"type":"attribute","field":"city","field":"phone","op":"eq","value":"x"}`), []string{"conditions[0].field:duplicate_property"}},

		// root shape
		{"root array", `[]`, []string{":wrong_type"}},
		{"root null", `null`, []string{":wrong_type"}},
		{"root string", `"rule"`, []string{":wrong_type"}},
		{"missing match and conditions", `{}`, []string{"match:required", "conditions:required"}},
		{"unknown root property", `{"match":"all","conditions":[],"sql":"DROP TABLE users"}`, []string{"sql:unknown_property"}},
		{"match wrong type", `{"match":true,"conditions":[]}`, []string{"match:wrong_type"}},
		{"match null", `{"match":null,"conditions":[]}`, []string{"match:wrong_type"}},
		{"match invalid enum", `{"match":"ALL","conditions":[]}`, []string{"match:not_allowed"}},
		{"match none", `{"match":"none","conditions":[]}`, []string{"match:not_allowed"}},
		{"conditions object", `{"match":"all","conditions":{}}`, []string{"conditions:wrong_type"}},
		{"conditions null", `{"match":"all","conditions":null}`, []string{"conditions:wrong_type"}},
		{"version string", `{"version":"1","match":"all","conditions":[]}`, []string{"version:wrong_type"}},
		{"version float", `{"version":1.0,"match":"all","conditions":[]}`, []string{"version:wrong_type"}},
		{"groups object", `{"match":"all","conditions":[],"groups":{}}`, []string{"groups:wrong_type"}},
		{"groups null", `{"match":"all","conditions":[],"groups":null}`, []string{"groups:wrong_type"}},

		// groups
		{"nested groups", `{"match":"all","conditions":[],"groups":[{"match":"all","conditions":[],"groups":[]}]}`, []string{"groups[0].groups:nested_groups_not_supported"}},
		{"group as condition", cond(`{"type":"group","match":"all","conditions":[]}`), []string{"conditions[0].type:nested_groups_not_supported"}},
		{"group unknown property", `{"match":"all","conditions":[],"groups":[{"match":"all","conditions":[],"not":true}]}`, []string{"groups[0].not:unknown_property"}},
		{"group missing match", `{"match":"all","conditions":[],"groups":[{"conditions":[]}]}`, []string{"groups[0].match:required"}},
		{"group not object", `{"match":"all","conditions":[],"groups":[[]]}`, []string{"groups[0]:wrong_type"}},

		// discriminator
		{"condition not object", cond(`"city = 'x'"`), []string{"conditions[0]:wrong_type"}},
		{"condition null", cond(`null`), []string{"conditions[0]:wrong_type"}},
		{"missing type", cond(`{"field":"city","op":"eq","value":"x"}`), []string{"conditions[0].type:required"}},
		{"type wrong json type", cond(`{"type":1}`), []string{"conditions[0].type:wrong_type"}},
		{"unknown type", cond(`{"type":"sql","query":"SELECT 1"}`), []string{"conditions[0].type:not_allowed"}},
		{"type case", cond(`{"type":"Attribute","field":"city","op":"eq","value":"x"}`), []string{"conditions[0].type:not_allowed"}},
		{"consent type", cond(`{"type":"consent","channel":"sms"}`), []string{"conditions[0].type:consent_is_backend_enforced"}},

		// attribute variant strictness
		{"attribute unknown property", cond(`{"type":"attribute","field":"city","op":"eq","value":"x","negate":true}`), []string{"conditions[0].negate:unknown_property"}},
		{"attribute with event property", cond(`{"type":"attribute","field":"city","op":"eq","value":"x","within_days":3}`), []string{"conditions[0].within_days:unknown_property"}},
		{"attribute missing all", cond(`{"type":"attribute"}`), []string{"conditions[0].field:required", "conditions[0].op:required", "conditions[0].value:required"}},
		{"attribute field number", cond(`{"type":"attribute","field":1,"op":"eq","value":"x"}`), []string{"conditions[0].field:wrong_type"}},
		{"attribute op invalid", cond(`{"type":"attribute","field":"city","op":"like","value":"x"}`), []string{"conditions[0].op:not_allowed"}},
		{"attribute op sql", cond(`{"type":"attribute","field":"city","op":"= 'x' OR 1=1 --","value":"x"}`), []string{"conditions[0].op:not_allowed"}},
		{"value null", cond(`{"type":"attribute","field":"city","op":"eq","value":null}`), []string{"conditions[0].value:wrong_type"}},
		{"value bool", cond(`{"type":"attribute","field":"city","op":"eq","value":true}`), []string{"conditions[0].value:wrong_type"}},
		{"value float", cond(`{"type":"attribute","field":"age","op":"gte","value":18.5}`), []string{"conditions[0].value:wrong_type"}},
		{"value exponent", cond(`{"type":"attribute","field":"age","op":"gte","value":1e1}`), []string{"conditions[0].value:wrong_type"}},
		{"value huge", cond(`{"type":"attribute","field":"age","op":"gte","value":99999999999999999999}`), []string{"conditions[0].value:out_of_range"}},
		{"value mixed list", cond(`{"type":"attribute","field":"city","op":"in","value":["Ankara",1,null]}`), []string{"conditions[0].value[1]:wrong_type", "conditions[0].value[2]:wrong_type"}},
		{"range missing max", cond(`{"type":"attribute","field":"age","op":"between","value":{"min":18}}`), []string{"conditions[0].value.max:required"}},
		{"range unknown key", cond(`{"type":"attribute","field":"age","op":"between","value":{"min":18,"max":30,"inclusive":false}}`), []string{"conditions[0].value.inclusive:unknown_property"}},
		{"range string bound", cond(`{"type":"attribute","field":"age","op":"between","value":{"min":"18","max":30}}`), []string{"conditions[0].value.min:wrong_type"}},

		// event variant strictness
		{"event unknown property", cond(`{"type":"event","event":"purchase","op":"exists","within_days":3,"field":"city"}`), []string{"conditions[0].field:unknown_property"}},
		{"event missing", cond(`{"type":"event"}`), []string{"conditions[0].event:required", "conditions[0].op:required", "conditions[0].within_days:required"}},
		{"event unknown name", cond(`{"type":"event","event":"page_view","op":"exists","within_days":3}`), []string{"conditions[0].event:not_allowed"}},
		{"event consent name", cond(`{"type":"event","event":"opted_in","op":"exists","within_days":3}`), []string{"conditions[0].event:consent_is_backend_enforced"}},
		{"event attribute op", cond(`{"type":"event","event":"purchase","op":"eq","within_days":3}`), []string{"conditions[0].op:not_allowed"}},
		{"event count string", cond(`{"type":"event","event":"purchase","op":"count_gte","count":"2","within_days":3}`), []string{"conditions[0].count:wrong_type"}},
		{"event count null", cond(`{"type":"event","event":"purchase","op":"count_gte","count":null,"within_days":3}`), []string{"conditions[0].count:wrong_type"}},
		{"event within_days float", cond(`{"type":"event","event":"purchase","op":"exists","within_days":3.5}`), []string{"conditions[0].within_days:wrong_type"}},
		{"event category string", cond(`{"type":"event","event":"purchase","op":"exists","within_days":3,"category_in":"books"}`), []string{"conditions[0].category_in:wrong_type"}},
		{"event category items", cond(`{"type":"event","event":"purchase","op":"exists","within_days":3,"category_in":[1]}`), []string{"conditions[0].category_in[0]:wrong_type"}},

		// behavior variant strictness
		{"behavior unknown property", cond(`{"type":"behavior","predicate":"abandoned_cart","within_days":3,"count":1}`), []string{"conditions[0].count:unknown_property"}},
		{"behavior missing", cond(`{"type":"behavior"}`), []string{"conditions[0].predicate:required", "conditions[0].within_days:required"}},
		{"behavior unknown predicate", cond(`{"type":"behavior","predicate":"churned","within_days":3}`), []string{"conditions[0].predicate:not_allowed"}},
		{"behavior consent predicate", cond(`{"type":"behavior","predicate":"unsubscribed","within_days":3}`), []string{"conditions[0].predicate:consent_is_backend_enforced"}},

		// several errors are all reported, in traversal order
		{"multiple errors", `{"match":"x","conditions":[{"type":"attribute","field":"city","op":"eq"},{"type":"nope"}],"extra":1}`,
			[]string{"extra:unknown_property", "match:not_allowed", "conditions[0].value:required", "conditions[1].type:not_allowed"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r, err := Decode([]byte(tt.in))
			if err == nil {
				t.Fatalf("Decode succeeded: %#v", r)
			}
			if !reflect.DeepEqual(r, Rule{}) {
				t.Errorf("Decode returned a partial rule on error: %#v", r)
			}
			if got := pc(t, err); !reflect.DeepEqual(got, tt.want) {
				t.Errorf("errors = %v, want %v", got, tt.want)
			}
			// The full pipeline never gets past the decoder for these inputs.
			if v, perr := Parse([]byte(tt.in)); perr == nil || !v.IsZero() {
				t.Errorf("Parse accepted invalid input")
			}
		})
	}
}

func TestDecodeErrorsCarryAllowedValues(t *testing.T) {
	_, err := Decode([]byte(`{"match":"all","conditions":[{"type":"x"}]}`))
	errs, _ := AsErrors(err)
	if len(errs) != 1 || !reflect.DeepEqual(errs[0].Allowed, []string{"attribute", "event", "behavior"}) {
		t.Fatalf("errors = %+v", errs)
	}
	_, err = Decode([]byte(`{"match":"all","conditions":[{"type":"behavior","predicate":"abandoned_cart","within_days":1,"x":1}]}`))
	errs, _ = AsErrors(err)
	if len(errs) != 1 || !reflect.DeepEqual(errs[0].Allowed, []string{"type", "predicate", "within_days"}) {
		t.Fatalf("errors = %+v", errs)
	}
}

func TestErrorJSONShapeMatchesAPIContract(t *testing.T) {
	_, err := Decode([]byte(`{"match":"some","conditions":[]}`))
	errs, _ := AsErrors(err)
	b, jerr := json.Marshal(errs)
	if jerr != nil {
		t.Fatal(jerr)
	}
	want := `[{"path":"match","code":"not_allowed","message":"value is not allowed","allowed":["all","any"]}]`
	if string(b) != want {
		t.Errorf("JSON = %s\nwant  %s", b, want)
	}
	pre := errs.WithPrefix("segment")
	if pre[0].Path != "segment.match" || errs[0].Path != "match" {
		t.Errorf("WithPrefix: %q (original %q)", pre[0].Path, errs[0].Path)
	}
	if got := (Errors{{Code: CodeMalformedJSON}}).WithPrefix("segment")[0].Path; got != "segment" {
		t.Errorf("root WithPrefix path = %q", got)
	}
}

// A Rule embedded in a larger document decoded with encoding/json is still
// decoded strictly: the outer decoder's DisallowUnknownFields does not reach
// into custom unmarshalers, so Rule.UnmarshalJSON must enforce it itself.
func TestRuleUnmarshalJSONIsStrictWhenEmbedded(t *testing.T) {
	type envelope struct {
		Segment Rule `json:"segment"`
	}
	decode := func(s string) error {
		dec := json.NewDecoder(strings.NewReader(s))
		dec.DisallowUnknownFields()
		var e envelope
		return dec.Decode(&e)
	}
	if err := decode(`{"segment":{"match":"all","conditions":[` + okAttr + `]}}`); err != nil {
		t.Fatalf("valid envelope: %v", err)
	}
	bad := []string{
		`{"segment":{"match":"all","conditions":[{"type":"attribute","field":"city","op":"eq","value":"x","sql":"1"}]}}`,
		`{"segment":{"match":"all","conditions":[{"type":"behavior","predicate":"abandoned_cart","within_days":3,"within_days":4}]}}`,
		`{"segment":{"match":"all","conditions":[],"groups":[{"match":"all","conditions":[],"groups":[]}]}}`,
	}
	for _, s := range bad {
		err := decode(s)
		if _, ok := AsErrors(err); !ok {
			t.Errorf("decode(%s) = %v, want segment.Errors", s, err)
		}
	}
}

func TestDecodeDoesNotNormalizeOrValidateSemantics(t *testing.T) {
	// Decode only checks shape; registry checks belong to Validate.
	r, err := Decode([]byte(`{"match":"all","conditions":[{"type":"attribute","field":"email","op":"eq","value":"x"}]}`))
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	if got := pc(t, Validate(r)); !reflect.DeepEqual(got, []string{"conditions[0].field:consent_is_backend_enforced"}) {
		t.Errorf("Validate = %v", got)
	}
}

func TestParseJSONDepthBoundary(t *testing.T) {
	// maxJSONDepth levels are fine, one more is too deep.
	ok := strings.Repeat("[", maxJSONDepth) + strings.Repeat("]", maxJSONDepth)
	if _, ferr := parseJSON([]byte(ok)); ferr != nil {
		t.Errorf("depth %d: %v", maxJSONDepth, ferr)
	}
	deep := "[" + ok + "]"
	if _, ferr := parseJSON([]byte(deep)); ferr == nil || ferr.Code != CodeTooDeep {
		t.Errorf("depth %d: %v", maxJSONDepth+1, ferr)
	}
	if _, ferr := parseJSON(bytes.Repeat([]byte("["), MaxRuleBytes)); ferr == nil || ferr.Code != CodeTooDeep {
		t.Errorf("very deep: %v", ferr)
	}
}
