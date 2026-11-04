package segment

import (
	"fmt"
	"reflect"
	"strings"
	"testing"
)

func ruleOf(cs ...Condition) Rule { return Rule{Match: MatchAll, Conditions: cs} }

func attr(f Field, op Operator, v Value) AttributeCondition {
	return AttributeCondition{Field: f, Op: op, Value: v}
}

func intp(i int) *int { return &i }

// validSample returns a valid value of the kind op requires for spec.
func validSample(spec FieldSpec, op Operator) Value {
	str := map[Field]string{FieldCity: "İzmir", FieldCountry: "TR", FieldGender: "female", FieldLanguage: "en"}[spec.Field]
	switch valueKindFor(op) {
	case ValueString:
		return StringValue(str)
	case ValueStrings:
		return StringsValue(str)
	case ValueInt:
		return IntValue(spec.Min)
	default:
		return RangeValue(spec.Min, spec.Max)
	}
}

// Every field × every operator: allowed pairs validate, others are rejected
// with operator_not_allowed listing the field's operators.
func TestEveryFieldOperatorCombination(t *testing.T) {
	allowed := map[Field][]Operator{
		FieldCity:              {OpEq, OpNeq, OpIn, OpNotIn},
		FieldCountry:           {OpEq, OpIn},
		FieldGender:            {OpEq, OpIn},
		FieldAge:               {OpGte, OpLte, OpBetween},
		FieldSignupDaysAgo:     {OpGte, OpLte, OpBetween},
		FieldLastActiveDaysAgo: {OpGte, OpLte},
		FieldLanguage:          {OpEq, OpIn},
	}
	if len(Fields()) != len(allowed) {
		t.Fatalf("registry has %d fields, test expects %d", len(Fields()), len(allowed))
	}
	for _, spec := range Fields() {
		if !reflect.DeepEqual(spec.Operators, allowed[spec.Field]) {
			t.Errorf("%s operators = %v, want %v", spec.Field, spec.Operators, allowed[spec.Field])
		}
		for _, o := range Operators() {
			op := Operator(o)
			t.Run(string(spec.Field)+"/"+o, func(t *testing.T) {
				errs := Validate(ruleOf(attr(spec.Field, op, validSample(spec, op))))
				if spec.allows(op) {
					if errs != nil {
						t.Fatalf("valid combination rejected: %v", errs)
					}
					return
				}
				want := []string{"conditions[0].op:operator_not_allowed"}
				if got := pc(t, errs); !reflect.DeepEqual(got, want) {
					t.Fatalf("errors = %v, want %v", got, want)
				}
				if !reflect.DeepEqual(errs[0].Allowed, opStrings(spec.Operators)) {
					t.Errorf("allowed = %v", errs[0].Allowed)
				}
			})
		}
	}
}

func TestAttributeValues(t *testing.T) {
	long := strings.Repeat("a", MaxValueRunes+1)
	many := make([]string, MaxListItems+1)
	for i := range many {
		many[i] = "Ankara"
	}
	fifty := many[:MaxListItems]
	tests := []struct {
		name string
		c    AttributeCondition
		want []string
	}{
		// value shape vs operator
		{"eq needs string", attr(FieldCity, OpEq, StringsValue("Ankara")), []string{"conditions[0].value:wrong_type"}},
		{"in needs list", attr(FieldCity, OpIn, StringValue("Ankara")), []string{"conditions[0].value:wrong_type"}},
		{"gte needs int", attr(FieldAge, OpGte, StringValue("18")), []string{"conditions[0].value:wrong_type"}},
		{"between needs range", attr(FieldAge, OpBetween, IntValue(18)), []string{"conditions[0].value:wrong_type"}},
		{"zero value", attr(FieldCity, OpEq, Value{}), []string{"conditions[0].value:wrong_type"}},

		// city
		{"city turkish", attr(FieldCity, OpEq, StringValue("ŞANLIURFA")), nil},
		{"city ascii", attr(FieldCity, OpNeq, StringValue("sanliurfa")), nil},
		{"city unknown", attr(FieldCity, OpEq, StringValue("Paris")), []string{"conditions[0].value:unknown_city"}},
		{"city empty", attr(FieldCity, OpEq, StringValue(" ")), []string{"conditions[0].value:invalid_value"}},
		{"city too long", attr(FieldCity, OpEq, StringValue(long)), []string{"conditions[0].value:too_long"}},
		{"city list unknown item", attr(FieldCity, OpNotIn, StringsValue("Ankara", "Atlantis", "İzmir")), []string{"conditions[0].value[1]:unknown_city"}},
		{"city list empty", attr(FieldCity, OpIn, StringsValue()), []string{"conditions[0].value:empty"}},
		{"city list 50", attr(FieldCity, OpIn, StringsValue(fifty...)), nil},
		{"city list 51", attr(FieldCity, OpIn, StringsValue(many...)), []string{"conditions[0].value:too_many"}},

		// injection strings are plain (invalid) values
		{"sql in city", attr(FieldCity, OpEq, StringValue("istanbul'; DROP TABLE users; --")), []string{"conditions[0].value:unknown_city"}},
		{"sql in city list", attr(FieldCity, OpIn, StringsValue("Ankara", "x' OR '1'='1")), []string{"conditions[0].value[1]:unknown_city"}},
		{"sql in country", attr(FieldCountry, OpEq, StringValue("TR' --")), []string{"conditions[0].value:unknown_country"}},
		{"sql in gender", attr(FieldGender, OpEq, StringValue("female' OR 1=1")), []string{"conditions[0].value:not_allowed"}},
		{"sql in language", attr(FieldLanguage, OpIn, StringsValue("tr); DELETE FROM users;--")), []string{"conditions[0].value[0]:not_allowed"}},
		{"sql as field", attr("city = city OR 1", OpEq, StringValue("x")), []string{"conditions[0].field:unknown_field"}},
		{"column as field", attr("first_name", OpEq, StringValue("x")), []string{"conditions[0].field:unknown_field"}},

		// country
		{"country lower", attr(FieldCountry, OpEq, StringValue("tr")), nil},
		{"country list", attr(FieldCountry, OpIn, StringsValue("DE", "tr")), nil},
		{"country unassigned", attr(FieldCountry, OpEq, StringValue("XX")), []string{"conditions[0].value:unknown_country"}},
		{"country alpha3", attr(FieldCountry, OpEq, StringValue("TUR")), []string{"conditions[0].value:unknown_country"}},
		{"country name", attr(FieldCountry, OpEq, StringValue("Turkey")), []string{"conditions[0].value:unknown_country"}},

		// gender / language
		{"gender all", attr(FieldGender, OpIn, StringsValue("female", "male", "other", "unknown")), nil},
		{"gender case", attr(FieldGender, OpEq, StringValue("Female")), nil},
		{"gender unknown", attr(FieldGender, OpEq, StringValue("f")), []string{"conditions[0].value:not_allowed"}},
		{"language de", attr(FieldLanguage, OpEq, StringValue("de")), []string{"conditions[0].value:not_allowed"}},

		// integer ranges (boundaries)
		{"age min", attr(FieldAge, OpGte, IntValue(13)), nil},
		{"age below", attr(FieldAge, OpGte, IntValue(12)), []string{"conditions[0].value:out_of_range"}},
		{"age max", attr(FieldAge, OpLte, IntValue(120)), nil},
		{"age above", attr(FieldAge, OpLte, IntValue(121)), []string{"conditions[0].value:out_of_range"}},
		{"age between equal", attr(FieldAge, OpBetween, RangeValue(30, 30)), nil},
		{"age between reversed", attr(FieldAge, OpBetween, RangeValue(40, 30)), []string{"conditions[0].value:invalid_range"}},
		{"age between out", attr(FieldAge, OpBetween, RangeValue(0, 200)), []string{"conditions[0].value.min:out_of_range", "conditions[0].value.max:out_of_range"}},
		{"signup zero", attr(FieldSignupDaysAgo, OpLte, IntValue(0)), nil},
		{"signup negative", attr(FieldSignupDaysAgo, OpGte, IntValue(-1)), []string{"conditions[0].value:out_of_range"}},
		{"signup max", attr(FieldSignupDaysAgo, OpBetween, RangeValue(0, 3650)), nil},
		{"signup above", attr(FieldSignupDaysAgo, OpLte, IntValue(3651)), []string{"conditions[0].value:out_of_range"}},
		{"last active max", attr(FieldLastActiveDaysAgo, OpGte, IntValue(3650)), nil},
		{"last active above", attr(FieldLastActiveDaysAgo, OpGte, IntValue(3651)), []string{"conditions[0].value:out_of_range"}},

		// unknown field / op
		{"empty field", attr("", OpEq, StringValue("x")), []string{"conditions[0].field:required"}},
		{"unknown op", attr(FieldCity, "like", StringValue("x")), []string{"conditions[0].op:not_allowed"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := pc(t, Validate(ruleOf(tt.c))); !reflect.DeepEqual(got, tt.want) {
				t.Errorf("errors = %v, want %v", got, tt.want)
			}
		})
	}
}

// Consent and contact are backend-enforced (ADR-004): every way of naming
// them is rejected with consent_is_backend_enforced, through the whole pipeline.
func TestConsentAndContactAreNotExpressible(t *testing.T) {
	fields := []string{
		"consent", "sms_consent", "email_consent", "consent_status", "channel_consents", "marketing_consent",
		"opted_in", "opt_in", "opt-out", "OptIn", "subscribed", "unsubscribed", "is_subscriber", "permission",
		"kvkk_permission", "gdpr", "iys_status",
		"email", "Email", "email_address", "phone", "phone_number", "msisdn", "gsm", "mobile", "sms", "contact",
		"has_email", "has_phone", "reachable",
	}
	for _, f := range fields {
		t.Run(f, func(t *testing.T) {
			in := fmt.Sprintf(`{"match":"all","conditions":[{"type":"attribute","field":%q,"op":"eq","value":"opted_in"}]}`, f)
			_, err := Parse([]byte(in))
			if got := pc(t, err); !reflect.DeepEqual(got, []string{"conditions[0].field:consent_is_backend_enforced"}) {
				t.Errorf("errors = %v", got)
			}
		})
	}
	for _, in := range []string{
		`{"match":"all","conditions":[{"type":"consent","channel":"sms","status":"opted_in"}]}`,
		`{"match":"all","conditions":[{"type":"contact","channel":"email"}]}`,
		`{"match":"all","conditions":[{"type":"event","event":"consent_given","op":"exists","within_days":30}]}`,
		`{"match":"all","conditions":[{"type":"behavior","predicate":"opted_out","within_days":30}]}`,
	} {
		_, err := Parse([]byte(in))
		got := pc(t, err)
		if len(got) != 1 || !strings.HasSuffix(got[0], ":consent_is_backend_enforced") {
			t.Errorf("Parse(%s) = %v", in, got)
		}
	}
}

func TestEventConditions(t *testing.T) {
	ev := func(op EventOp, count *int, days int, cats []string) EventCondition {
		return EventCondition{Event: EventCartAdd, Op: op, Count: count, WithinDays: days, CategoryIn: cats}
	}
	tests := []struct {
		name string
		c    Condition
		want []string
	}{
		{"exists", ev(EventExists, nil, 30, nil), nil},
		{"not_exists", ev(EventNotExists, nil, 30, nil), nil},
		{"count_gte", ev(EventCountGte, intp(2), 30, nil), nil},
		{"count_lte", ev(EventCountLte, intp(100), 30, nil), nil},
		{"count_gte min", ev(EventCountGte, intp(1), 30, nil), nil},
		{"count_gte without count", ev(EventCountGte, nil, 30, nil), []string{"conditions[0].count:count_required"}},
		{"count_lte without count", ev(EventCountLte, nil, 30, nil), []string{"conditions[0].count:count_required"}},
		{"exists with count", ev(EventExists, intp(1), 30, nil), []string{"conditions[0].count:count_not_allowed"}},
		{"not_exists with count", ev(EventNotExists, intp(1), 30, nil), []string{"conditions[0].count:count_not_allowed"}},
		{"count_lte zero", ev(EventCountLte, intp(0), 30, nil), nil},
		{"count_gte zero", ev(EventCountGte, intp(0), 30, nil), nil},
		{"count_lte max", ev(EventCountLte, intp(MaxEventCount), 30, nil), nil},
		{"count_gte above", ev(EventCountGte, intp(101), 30, nil), []string{"conditions[0].count:out_of_range"}},
		{"count_lte above", ev(EventCountLte, intp(101), 30, nil), []string{"conditions[0].count:out_of_range"}},
		{"count_lte negative", ev(EventCountLte, intp(-1), 30, nil), []string{"conditions[0].count:out_of_range"}},
		{"count_gte negative", ev(EventCountGte, intp(-1), 30, nil), []string{"conditions[0].count:out_of_range"}},
		{"exists with zero count", ev(EventExists, intp(0), 30, nil), []string{"conditions[0].count:count_not_allowed"}},
		{"not_exists with zero count", ev(EventNotExists, intp(0), 30, nil), []string{"conditions[0].count:count_not_allowed"}},
		{"window min", ev(EventExists, nil, 1, nil), nil},
		{"window max", ev(EventExists, nil, 365, nil), nil},
		{"window zero", ev(EventExists, nil, 0, nil), []string{"conditions[0].within_days:out_of_range"}},
		{"window negative", ev(EventExists, nil, -7, nil), []string{"conditions[0].within_days:out_of_range"}},
		{"window above", ev(EventExists, nil, 366, nil), []string{"conditions[0].within_days:out_of_range"}},
		{"categories", ev(EventExists, nil, 30, []string{"Books", "toys"}), nil},
		{"categories empty", ev(EventExists, nil, 30, []string{}), []string{"conditions[0].category_in:empty"}},
		{"categories unknown", ev(EventExists, nil, 30, []string{"books", "weapons"}), []string{"conditions[0].category_in[1]:unknown_category"}},
		{"categories injection", ev(EventExists, nil, 30, []string{"books') OR 1=1--"}), []string{"conditions[0].category_in[0]:unknown_category"}},
		{"categories too many", ev(EventExists, nil, 30, make([]string, MaxListItems+1)), []string{"conditions[0].category_in:too_many"}},
		{"unknown op", ev("eq", nil, 30, nil), []string{"conditions[0].op:not_allowed"}},
		{"unknown event", EventCondition{Event: "view", Op: EventExists, WithinDays: 3}, []string{"conditions[0].event:not_allowed"}},
		{"behavior ok", BehaviorCondition{PredicateAbandonedCart, 30}, nil},
		{"behavior window zero", BehaviorCondition{PredicateAbandonedCart, 0}, []string{"conditions[0].within_days:out_of_range"}},
		{"behavior window above", BehaviorCondition{PredicateAbandonedCart, 366}, []string{"conditions[0].within_days:out_of_range"}},
		{"behavior unknown", BehaviorCondition{"churned", 30}, []string{"conditions[0].predicate:not_allowed"}},
		{"nil condition", nil, []string{"conditions[0].type:not_allowed"}},
		{"pointer condition", &BehaviorCondition{PredicateAbandonedCart, 30}, []string{"conditions[0].type:not_allowed"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := pc(t, Validate(ruleOf(tt.c))); !reflect.DeepEqual(got, tt.want) {
				t.Errorf("errors = %v, want %v", got, tt.want)
			}
		})
	}
}

// Every combination of event name × operator × count presence.
func TestEventOperatorCountMatrix(t *testing.T) {
	for _, e := range EventNames() {
		for _, o := range EventOps() {
			for _, withCount := range []bool{false, true} {
				c := EventCondition{Event: EventName(e), Op: EventOp(o), WithinDays: 7}
				if withCount {
					c.Count = intp(5)
				}
				needsCount := o == "count_gte" || o == "count_lte"
				errs := Validate(ruleOf(c))
				switch {
				case needsCount == withCount && errs != nil:
					t.Errorf("%s/%s count=%v: unexpected %v", e, o, withCount, errs)
				case needsCount && !withCount && (len(errs) != 1 || errs[0].Code != CodeCountRequired):
					t.Errorf("%s/%s without count: %v", e, o, errs)
				case !needsCount && withCount && (len(errs) != 1 || errs[0].Code != CodeCountNotAllowed):
					t.Errorf("%s/%s with count: %v", e, o, errs)
				}
			}
		}
	}
}

func TestRuleStructure(t *testing.T) {
	ok := BehaviorCondition{PredicateAbandonedCart, 30}
	n := func(k int) []Condition {
		cs := make([]Condition, k)
		for i := range cs {
			cs[i] = ok
		}
		return cs
	}
	groups := func(k int) []Group {
		gs := make([]Group, k)
		for i := range gs {
			gs[i] = Group{Match: MatchAny, Conditions: n(1)}
		}
		return gs
	}
	tests := []struct {
		name string
		r    Rule
		want []string
	}{
		{"empty rule", Rule{Match: MatchAll}, []string{"conditions:empty"}},
		{"empty rule with empty slice", Rule{Match: MatchAll, Conditions: []Condition{}}, []string{"conditions:empty"}},
		{"groups only", Rule{Match: MatchAny, Conditions: []Condition{}, Groups: groups(2)}, nil},
		{"empty group", Rule{Match: MatchAll, Conditions: n(1), Groups: []Group{{Match: MatchAll}}}, []string{"groups[0].conditions:empty"}},
		{"10 conditions", Rule{Match: MatchAll, Conditions: n(10)}, nil},
		{"11 conditions", Rule{Match: MatchAll, Conditions: n(11)}, []string{"conditions:too_many"}},
		{"10 in group", Rule{Match: MatchAll, Conditions: n(10), Groups: []Group{{Match: MatchAll, Conditions: n(10)}}}, nil},
		{"11 in group", Rule{Match: MatchAll, Groups: []Group{{Match: MatchAll, Conditions: n(11)}}}, []string{"groups[0].conditions:too_many"}},
		{"5 groups", Rule{Match: MatchAll, Groups: groups(5)}, nil},
		{"6 groups", Rule{Match: MatchAll, Groups: groups(6)}, []string{"groups:too_many"}},
		{"bad match", Rule{Match: "xor", Conditions: n(1)}, []string{"match:not_allowed"}},
		{"bad group match", Rule{Match: MatchAll, Groups: []Group{{Match: "", Conditions: n(1)}}}, []string{"groups[0].match:not_allowed"}},
		{"version 1", Rule{Version: 1, Match: MatchAll, Conditions: n(1)}, nil},
		{"version 2", Rule{Version: 2, Match: MatchAll, Conditions: n(1)}, []string{"version:unsupported_version"}},
		{"errors in several places", Rule{Match: MatchAll, Conditions: []Condition{ok, BehaviorCondition{PredicateAbandonedCart, 0}},
			Groups: []Group{{Match: MatchAll, Conditions: []Condition{attr(FieldAge, OpGte, IntValue(5))}}}},
			[]string{"conditions[1].within_days:out_of_range", "groups[0].conditions[0].value:out_of_range"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := pc(t, Validate(tt.r)); !reflect.DeepEqual(got, tt.want) {
				t.Errorf("errors = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestValidateIsDeterministic(t *testing.T) {
	r := Rule{Match: "x", Conditions: []Condition{
		attr("phone", OpEq, StringValue("x")), attr(FieldAge, OpEq, StringValue("x")),
		attr(FieldCity, OpIn, StringsValue("a", "b", "c")), EventCondition{Op: "x"},
	}}
	first := Validate(r)
	for i := 0; i < 20; i++ {
		if got := Validate(r); !reflect.DeepEqual(got, first) {
			t.Fatalf("run %d differs:\n%v\n%v", i, got, first)
		}
	}
}

func TestRegistryInvariants(t *testing.T) {
	if len(countryCodes) != 249 {
		t.Errorf("country list has %d codes, want 249", len(countryCodes))
	}
	for _, spec := range Fields() {
		if spec.Description == "" || len(spec.Operators) == 0 {
			t.Errorf("%s: incomplete spec", spec.Field)
		}
		switch spec.Type {
		case TypeString:
			if spec.canonical == nil || spec.invalidCode == "" {
				t.Errorf("%s: string field without canonicalizer", spec.Field)
			}
			for _, v := range spec.Values {
				if c, ok := spec.canonical(v); !ok || c != v {
					t.Errorf("%s: value %q is not canonical", spec.Field, v)
				}
			}
		case TypeInteger:
			if spec.Min > spec.Max || spec.Values != nil {
				t.Errorf("%s: bad integer spec", spec.Field)
			}
		default:
			t.Errorf("%s: unknown type %q", spec.Field, spec.Type)
		}
		for _, op := range spec.Operators {
			if valueKindFor(op) == ValueInvalid {
				t.Errorf("%s: operator %s has no value kind", spec.Field, op)
			}
			if (spec.Type == TypeInteger) != (op == OpGte || op == OpLte || op == OpBetween) {
				t.Errorf("%s: operator %s does not fit type %s", spec.Field, op, spec.Type)
			}
		}
		// Registry fields themselves are never mistaken for consent/contact.
		if looksLikeConsentOrContact(string(spec.Field)) {
			t.Errorf("%s: matches consent/contact markers", spec.Field)
		}
	}
	// Returned specs are copies.
	f := Fields()
	f[0].Operators[0] = "x"
	if s, _ := LookupField(f[0].Field); s.Operators[0] == "x" {
		t.Error("Fields() exposes registry internals")
	}
	if !isSortedUnique(Categories()) {
		t.Error("categories must be sorted and unique")
	}
}

func isSortedUnique(s []string) bool {
	for i := 1; i < len(s); i++ {
		if s[i-1] >= s[i] {
			return false
		}
	}
	return true
}

// Validation results must be usable through the standard error interface
// without the typed-nil trap.
func TestErrorInterfaceNeverTypedNil(t *testing.T) {
	valid := ruleOf(BehaviorCondition{PredicateAbandonedCart, 30})
	invalid := ruleOf(BehaviorCondition{PredicateAbandonedCart, 0})
	validJSON := []byte(`{"match":"all","conditions":[{"type":"behavior","predicate":"abandoned_cart","within_days":30}]}`)
	invalidJSON := []byte(`{"match":"all","conditions":[{"type":"behavior","predicate":"abandoned_cart","within_days":0}]}`)
	malformedJSON := []byte(`{"match":`)

	if errs := Validate(valid); errs != nil {
		t.Errorf("Validate(valid) = %#v, want nil", errs)
	}
	successes := map[string]func() error{
		"Check":     func() error { return Check(valid) },
		"Decode":    func() error { _, err := Decode(validJSON); return err },
		"Normalize": func() error { _, err := Normalize(valid); return err },
		"Parse":     func() error { _, err := Parse(validJSON); return err },
		"Validated.UnmarshalJSON": func() error {
			var v Validated
			return v.UnmarshalJSON(validJSON)
		},
		"Rule.UnmarshalJSON": func() error {
			var r Rule
			return r.UnmarshalJSON(validJSON)
		},
	}
	for name, f := range successes {
		if err := f(); err != nil { // interface comparison: catches typed nil
			t.Errorf("%s: err = %#v, want untyped nil", name, err)
		}
	}
	failures := map[string]func() error{
		"Check":          func() error { return Check(invalid) },
		"Decode":         func() error { _, err := Decode(malformedJSON); return err },
		"Normalize":      func() error { _, err := Normalize(invalid); return err },
		"Parse semantic": func() error { _, err := Parse(invalidJSON); return err },
		"Parse syntax":   func() error { _, err := Parse(malformedJSON); return err },
		"Validated.UnmarshalJSON": func() error {
			var v Validated
			return v.UnmarshalJSON(invalidJSON)
		},
	}
	for name, f := range failures {
		err := f()
		errs, ok := AsErrors(err)
		if err == nil || !ok || len(errs) == 0 {
			t.Errorf("%s: err = %#v, want non-empty Errors", name, err)
		}
	}
}
