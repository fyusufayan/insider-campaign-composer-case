package segment

import (
	"encoding/json"
	"reflect"
	"testing"
)

var fuzzSeeds = []string{
	``, `{}`, `[]`, `null`, `"x"`, `{"match":"all"`, `{"match":"all","conditions":[]}{}`,
	`{"match":"all","conditions":[{"type":"attribute","field":"city","op":"eq","value":"Istanbul"},{"type":"behavior","predicate":"abandoned_cart","within_days":30}]}`,
	`{"version":1,"match":"any","conditions":[],"groups":[{"match":"all","conditions":[{"type":"event","event":"purchase","op":"count_gte","count":3,"within_days":90,"category_in":["books","Toys"]}]}]}`,
	`{"match":"all","conditions":[{"type":"attribute","field":"age","op":"between","value":{"min":18,"max":35}}]}`,
	`{"match":"all","conditions":[{"type":"attribute","field":"city","op":"in","value":["İZMİR","ıstanbul","Şanlıurfa"]}]}`,
	`{"match":"all","conditions":[{"type":"attribute","field":"sms_consent","op":"eq","value":"opted_in"}]}`,
	`{"match":"all","conditions":[{"type":"attribute","field":"city","op":"eq","value":"x' OR 1=1 --"}]}`,
	`{"match":"all","conditions":[],"groups":[{"match":"all","conditions":[],"groups":[]}]}`,
	`{"match":"all","match":"any","conditions":[]}`,
	"{\"match\":\"all\",\"conditions\":[{\"type\":\"attribute\",\"field\":\"city\",\"op\":\"eq\",\"value\":\"\\u0130stanbul\"}]}",
	`{"match":"all","conditions":[{"type":"attribute","field":"age","op":"gte","value":-0}]}`,
	`[[[[[[[[[[[[[[[[[[[[]]]]]]]]]]]]]]]]]]]]`,
}

// FuzzParse: arbitrary input never panics, never yields a partial result, and
// whatever is accepted is a fixed point of the pipeline and passes Validate.
func FuzzParse(f *testing.F) {
	for _, s := range fuzzSeeds {
		f.Add([]byte(s))
	}
	f.Fuzz(func(t *testing.T, data []byte) {
		r, derr := Decode(data)
		if derr != nil {
			if _, ok := AsErrors(derr); !ok {
				t.Fatalf("Decode error is not Errors: %T", derr)
			}
			if !reflect.DeepEqual(r, Rule{}) {
				t.Fatalf("partial rule on decode error")
			}
		}
		v, err := Parse(data)
		if err != nil {
			errs, ok := AsErrors(err)
			if !ok || len(errs) == 0 || !v.IsZero() {
				t.Fatalf("bad failure: %v (%T) zero=%v", err, err, v.IsZero())
			}
			return
		}
		if derr != nil {
			t.Fatalf("Parse accepted what Decode rejected: %v", derr)
		}
		if errs := Validate(v.Rule()); errs != nil {
			t.Fatalf("accepted rule fails Validate: %v", errs)
		}
		enc, merr := json.Marshal(v)
		if merr != nil {
			t.Fatalf("Marshal: %v", merr)
		}
		v2, perr := Parse(enc)
		if perr != nil {
			t.Fatalf("normalized form does not parse: %v\n%s", perr, enc)
		}
		if !reflect.DeepEqual(v.Rule(), v2.Rule()) {
			t.Fatalf("not idempotent:\n%s", enc)
		}
		enc2, _ := json.Marshal(v2)
		if string(enc) != string(enc2) {
			t.Fatalf("encoding not deterministic:\n%s\n%s", enc, enc2)
		}
	})
}
