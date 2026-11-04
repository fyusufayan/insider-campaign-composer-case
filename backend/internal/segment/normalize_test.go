package segment

import (
	"encoding/json"
	"errors"
	"reflect"
	"testing"
)

func mustParse(t *testing.T, s string) Validated {
	t.Helper()
	v, err := Parse([]byte(s))
	if err != nil {
		t.Fatalf("Parse(%s): %v", s, err)
	}
	return v
}

func mustJSON(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	return string(b)
}

func TestNormalizeCanonicalForm(t *testing.T) {
	tests := []struct {
		name, in, want string
	}{
		{
			"example brief",
			`{"match":"all","conditions":[{"type":"attribute","field":"city","op":"eq","value":"Istanbul"},{"type":"behavior","predicate":"abandoned_cart","within_days":30}]}`,
			`{"version":1,"match":"all","conditions":[{"type":"attribute","field":"city","op":"eq","value":"istanbul"},{"type":"behavior","predicate":"abandoned_cart","within_days":30}]}`,
		},
		{
			"turkish city spellings in a list collapse to one sorted key set",
			`{"match":"all","conditions":[{"type":"attribute","field":"city","op":"in","value":["İZMİR","İstanbul","ISTANBUL","istanbul","ıstanbul"," Şanlıurfa ","sanliurfa","Ankara"]}]}`,
			`{"version":1,"match":"all","conditions":[{"type":"attribute","field":"city","op":"in","value":["ankara","istanbul","izmir","sanliurfa"]}]}`,
		},
		{
			"decomposed İ",
			"{\"match\":\"all\",\"conditions\":[{\"type\":\"attribute\",\"field\":\"city\",\"op\":\"neq\",\"value\":\"İstanbul\"}]}",
			`{"version":1,"match":"all","conditions":[{"type":"attribute","field":"city","op":"neq","value":"istanbul"}]}`,
		},
		{
			"country, gender, language canonical case and sets",
			`{"match":"any","conditions":[
				{"type":"attribute","field":"country","op":"in","value":["tr","DE","Tr"]},
				{"type":"attribute","field":"gender","op":"eq","value":" FEMALE "},
				{"type":"attribute","field":"language","op":"in","value":["EN","tr","en"]}]}`,
			`{"version":1,"match":"any","conditions":[` +
				`{"type":"attribute","field":"country","op":"in","value":["DE","TR"]},` +
				`{"type":"attribute","field":"gender","op":"eq","value":"female"},` +
				`{"type":"attribute","field":"language","op":"in","value":["en","tr"]}]}`,
		},
		{
			"events, ranges and groups keep their order",
			`{"version":1,"match":"all","conditions":[],"groups":[
				{"match":"any","conditions":[{"type":"event","event":"purchase","op":"count_lte","count":2,"within_days":90,"category_in":["Toys","books","BOOKS"]},
				                             {"type":"attribute","field":"age","op":"between","value":{"max":35,"min":18}}]},
				{"match":"all","conditions":[{"type":"event","event":"cart_add","op":"not_exists","within_days":7}]}]}`,
			`{"version":1,"match":"all","conditions":[],"groups":[` +
				`{"match":"any","conditions":[{"type":"event","event":"purchase","op":"count_lte","count":2,"within_days":90,"category_in":["books","toys"]},` +
				`{"type":"attribute","field":"age","op":"between","value":{"min":18,"max":35}}]},` +
				`{"match":"all","conditions":[{"type":"event","event":"cart_add","op":"not_exists","within_days":7}]}]}`,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			v := mustParse(t, tt.in)
			got := mustJSON(t, v)
			if got != tt.want {
				t.Fatalf("normalized =\n%s\nwant\n%s", got, tt.want)
			}
			// Round trip: the stored form decodes, validates and normalizes to itself.
			v2 := mustParse(t, got)
			if !reflect.DeepEqual(v2.Rule(), v.Rule()) {
				t.Errorf("round trip changed the rule:\n%#v\n%#v", v2.Rule(), v.Rule())
			}
			if again := mustJSON(t, v2); again != got {
				t.Errorf("not idempotent:\n%s\n%s", again, got)
			}
			if errs := Validate(v.Rule()); errs != nil {
				t.Errorf("normalized rule does not validate: %v", errs)
			}
		})
	}
}

func TestNormalizeIsIdempotentOnRules(t *testing.T) {
	r := Rule{Match: MatchAny, Conditions: []Condition{
		attr(FieldCity, OpNotIn, StringsValue("Muğla", "MUGLA", "Kütahya")),
		EventCondition{Event: EventCartAdd, Op: EventCountGte, Count: intp(2), WithinDays: 14, CategoryIn: []string{"Home"}},
	}}
	v1, err := Normalize(r)
	if err != nil {
		t.Fatal(err)
	}
	v2, err := Normalize(v1.Rule())
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(v1.Rule(), v2.Rule()) {
		t.Errorf("Normalize(Normalize(r)) != Normalize(r):\n%#v\n%#v", v2.Rule(), v1.Rule())
	}
	// The input is not modified.
	if got := r.Conditions[0].(AttributeCondition).Value.Strs; !reflect.DeepEqual(got, []string{"Muğla", "MUGLA", "Kütahya"}) {
		t.Errorf("input mutated: %v", got)
	}
}

func TestNormalizeRejectsInvalidRules(t *testing.T) {
	v, err := Normalize(ruleOf(attr(FieldCity, OpEq, StringValue("Gotham"))))
	if !v.IsZero() {
		t.Error("invalid rule produced a Validated")
	}
	if got := pc(t, err); !reflect.DeepEqual(got, []string{"conditions[0].value:unknown_city"}) {
		t.Errorf("errors = %v", got)
	}
}

func TestValidatedCannotBeForged(t *testing.T) {
	var zero Validated
	if !zero.IsZero() {
		t.Error("zero Validated is not zero")
	}
	if _, err := json.Marshal(zero); !errors.Is(err, ErrNotValidated) {
		t.Errorf("Marshal(zero) = %v, want ErrNotValidated", err)
	}
	// Unmarshalling runs the full pipeline: invalid JSON never yields a Validated.
	var v Validated
	err := json.Unmarshal([]byte(`{"match":"all","conditions":[{"type":"attribute","field":"phone","op":"eq","value":"x"}]}`), &v)
	if _, ok := AsErrors(err); !ok || !v.IsZero() {
		t.Errorf("Unmarshal invalid: err=%v zero=%v", err, v.IsZero())
	}
	if err := json.Unmarshal([]byte(`null`), &v); err == nil || !v.IsZero() {
		t.Errorf("Unmarshal null: err=%v", err)
	}
	// A stored rule loads and is re-normalized.
	if err := json.Unmarshal([]byte(`{"match":"all","conditions":[{"type":"attribute","field":"city","op":"eq","value":"ANKARA"}]}`), &v); err != nil || v.IsZero() {
		t.Fatalf("Unmarshal valid: %v", err)
	}
	if got := v.Rule().Conditions[0].(AttributeCondition).Value.Str; got != "ankara" {
		t.Errorf("loaded value = %q", got)
	}
}

func TestValidatedRuleReturnsACopy(t *testing.T) {
	v := mustParse(t, `{"match":"all","conditions":[
		{"type":"attribute","field":"city","op":"in","value":["Ankara","İzmir"]},
		{"type":"event","event":"purchase","op":"count_gte","count":2,"within_days":30,"category_in":["books"]}],
		"groups":[{"match":"any","conditions":[{"type":"behavior","predicate":"abandoned_cart","within_days":5}]}]}`)
	before := mustJSON(t, v)
	r := v.Rule()
	r.Match = MatchAny
	r.Conditions[0].(AttributeCondition).Value.Strs[0] = "paris"
	ev := r.Conditions[1].(EventCondition)
	*ev.Count = 99
	ev.CategoryIn[0] = "weapons"
	r.Groups[0].Conditions[0] = attr("phone", OpEq, StringValue("x"))
	if after := mustJSON(t, v); after != before {
		t.Errorf("mutating Rule() changed the Validated:\n%s\n%s", after, before)
	}
}

func TestMarshalRuleShapes(t *testing.T) {
	// Conditions is always an array; optional fields are omitted.
	got := mustJSON(t, Rule{Match: MatchAll, Groups: []Group{{Match: MatchAny}}})
	if want := `{"match":"all","conditions":[],"groups":[{"match":"any","conditions":[]}]}`; got != want {
		t.Errorf("got %s want %s", got, want)
	}
	if _, err := json.Marshal(ruleOf(attr(FieldCity, OpEq, Value{}))); err == nil {
		t.Error("marshalling a value of invalid kind succeeded")
	}
}
