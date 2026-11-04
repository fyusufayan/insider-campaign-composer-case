package segment

import (
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
)

const (
	goldenToday = "(now() AT TIME ZONE 'UTC')::date"
	goldenHead  = "SELECT u.id FROM users AS u WHERE "
)

// single wraps one condition fragment the way Compile does for a one-condition rule.
func single(frag string) string { return goldenHead + "((" + frag + "))" }

func mustCompile(t *testing.T, v Validated) (string, []any) {
	t.Helper()
	sql, args, err := Compile(v)
	if err != nil {
		t.Fatalf("Compile: %v", err)
	}
	return sql, args
}

func ruleJSON(conds ...string) string {
	return `{"match":"all","conditions":[` + strings.Join(conds, ",") + `]}`
}

// goldenConditions covers every field × operator of the registry, every
// event operator (with and without categories) and every predicate.
var goldenConditions = []struct {
	name string
	cond string
	frag string
	args []any
}{
	// ---- text attributes ----
	{"city eq", `{"type":"attribute","field":"city","op":"eq","value":"İSTANBUL"}`,
		"u.city_normalized = $1::text", []any{"istanbul"}},
	{"city neq", `{"type":"attribute","field":"city","op":"neq","value":"Ankara"}`,
		"u.city_normalized <> $1::text", []any{"ankara"}},
	{"city in", `{"type":"attribute","field":"city","op":"in","value":["İzmir","ISTANBUL","istanbul"]}`,
		"u.city_normalized = ANY($1::text[])", []any{[]string{"istanbul", "izmir"}}},
	{"city not_in", `{"type":"attribute","field":"city","op":"not_in","value":["Çanakkale","Iğdır"]}`,
		"u.city_normalized <> ALL($1::text[])", []any{[]string{"canakkale", "igdir"}}},
	{"country eq", `{"type":"attribute","field":"country","op":"eq","value":"tr"}`,
		"u.country::text = $1::text", []any{"TR"}},
	{"country in", `{"type":"attribute","field":"country","op":"in","value":["gb","DE"]}`,
		"u.country::text = ANY($1::text[])", []any{[]string{"DE", "GB"}}},
	{"gender eq", `{"type":"attribute","field":"gender","op":"eq","value":"Female"}`,
		"u.gender = $1::text", []any{"female"}},
	{"gender in", `{"type":"attribute","field":"gender","op":"in","value":["unknown","other"]}`,
		"u.gender = ANY($1::text[])", []any{[]string{"other", "unknown"}}},
	{"language eq", `{"type":"attribute","field":"language","op":"eq","value":"EN"}`,
		"u.language = $1::text", []any{"en"}},
	{"language in", `{"type":"attribute","field":"language","op":"in","value":["tr","en"]}`,
		"u.language = ANY($1::text[])", []any{[]string{"en", "tr"}}},

	// ---- age (whole years, UTC date) ----
	{"age gte", `{"type":"attribute","field":"age","op":"gte","value":18}`,
		"u.birth_date <= (" + goldenToday + " - make_interval(years => $1::integer))::date", []any{18}},
	{"age lte", `{"type":"attribute","field":"age","op":"lte","value":35}`,
		"u.birth_date > (" + goldenToday + " - make_interval(years => $1::integer + 1))::date", []any{35}},
	{"age between", `{"type":"attribute","field":"age","op":"between","value":{"min":25,"max":34}}`,
		"u.birth_date <= (" + goldenToday + " - make_interval(years => $1::integer))::date AND " +
			"u.birth_date > (" + goldenToday + " - make_interval(years => $2::integer + 1))::date", []any{25, 34}},

	// ---- days ago (whole elapsed days) ----
	{"signup_days_ago gte", `{"type":"attribute","field":"signup_days_ago","op":"gte","value":365}`,
		"u.signup_at <= now() - make_interval(days => $1::integer)", []any{365}},
	{"signup_days_ago lte", `{"type":"attribute","field":"signup_days_ago","op":"lte","value":30}`,
		"u.signup_at > now() - make_interval(days => $1::integer + 1) AND u.signup_at <= now()", []any{30}},
	{"signup_days_ago between", `{"type":"attribute","field":"signup_days_ago","op":"between","value":{"min":30,"max":60}}`,
		"u.signup_at <= now() - make_interval(days => $1::integer) AND u.signup_at > now() - make_interval(days => $2::integer + 1) AND u.signup_at <= now()", []any{30, 60}},
	{"last_active_days_ago gte", `{"type":"attribute","field":"last_active_days_ago","op":"gte","value":30}`,
		"u.last_active_at <= now() - make_interval(days => $1::integer)", []any{30}},
	{"last_active_days_ago lte", `{"type":"attribute","field":"last_active_days_ago","op":"lte","value":7}`,
		"u.last_active_at > now() - make_interval(days => $1::integer + 1) AND u.last_active_at <= now()", []any{7}},

	// ---- events ----
	{"cart exists", `{"type":"event","event":"cart_add","op":"exists","within_days":30}`,
		"EXISTS (SELECT 1 FROM cart_events AS e WHERE e.user_id = u.id AND e.occurred_at >= now() - make_interval(days => $1::integer) AND e.occurred_at <= now())",
		[]any{30}},
	{"cart not_exists", `{"type":"event","event":"cart_add","op":"not_exists","within_days":14}`,
		"NOT EXISTS (SELECT 1 FROM cart_events AS e WHERE e.user_id = u.id AND e.occurred_at >= now() - make_interval(days => $1::integer) AND e.occurred_at <= now())",
		[]any{14}},
	{"purchase exists with categories", `{"type":"event","event":"purchase","op":"exists","within_days":7,"category_in":["Toys","books"]}`,
		"EXISTS (SELECT 1 FROM purchases AS e WHERE e.user_id = u.id AND e.purchased_at >= now() - make_interval(days => $1::integer) AND e.purchased_at <= now() AND e.category = ANY($2::text[]))",
		[]any{7, []string{"books", "toys"}}},
	{"purchase not_exists with categories", `{"type":"event","event":"purchase","op":"not_exists","within_days":90,"category_in":["home"]}`,
		"NOT EXISTS (SELECT 1 FROM purchases AS e WHERE e.user_id = u.id AND e.purchased_at >= now() - make_interval(days => $1::integer) AND e.purchased_at <= now() AND e.category = ANY($2::text[]))",
		[]any{90, []string{"home"}}},
	{"cart count_gte with categories", `{"type":"event","event":"cart_add","op":"count_gte","count":2,"within_days":14,"category_in":["sports"]}`,
		"(SELECT count(*) FROM cart_events AS e WHERE e.user_id = u.id AND e.occurred_at >= now() - make_interval(days => $1::integer) AND e.occurred_at <= now() AND e.category = ANY($2::text[])) >= $3::bigint",
		[]any{14, []string{"sports"}, 2}},
	{"purchase count_lte", `{"type":"event","event":"purchase","op":"count_lte","count":0,"within_days":60}`,
		"(SELECT count(*) FROM purchases AS e WHERE e.user_id = u.id AND e.purchased_at >= now() - make_interval(days => $1::integer) AND e.purchased_at <= now()) <= $2::bigint",
		[]any{60, 0}},
	{"purchase count_gte zero", `{"type":"event","event":"purchase","op":"count_gte","count":0,"within_days":1}`,
		"(SELECT count(*) FROM purchases AS e WHERE e.user_id = u.id AND e.purchased_at >= now() - make_interval(days => $1::integer) AND e.purchased_at <= now()) >= $2::bigint",
		[]any{1, 0}},

	// ---- behaviour ----
	{"abandoned_cart", `{"type":"behavior","predicate":"abandoned_cart","within_days":30}`,
		"EXISTS (SELECT 1 FROM cart_events AS c WHERE c.user_id = u.id AND c.occurred_at >= now() - make_interval(days => $1::integer) AND c.occurred_at <= now()" +
			" AND NOT EXISTS (SELECT 1 FROM purchases AS p WHERE p.user_id = c.user_id AND p.purchased_at >= c.occurred_at AND p.purchased_at <= now()))",
		[]any{30}},
}

func TestCompileGolden(t *testing.T) {
	for _, tc := range goldenConditions {
		t.Run(tc.name, func(t *testing.T) {
			sql, args := mustCompile(t, mustParse(t, ruleJSON(tc.cond)))
			if want := single(tc.frag); sql != want {
				t.Errorf("SQL\n got: %s\nwant: %s", sql, want)
			}
			if !reflect.DeepEqual(args, tc.args) {
				t.Errorf("args = %#v, want %#v", args, tc.args)
			}
		})
	}
}

// Every registry field × allowed operator, every event × event operator and
// every predicate has a golden case.
func TestCompileGoldenCoversVocabulary(t *testing.T) {
	covered := map[string]bool{}
	for _, tc := range goldenConditions {
		v := mustParse(t, ruleJSON(tc.cond))
		switch c := v.Rule().Conditions[0].(type) {
		case AttributeCondition:
			covered["attr:"+string(c.Field)+":"+string(c.Op)] = true
		case EventCondition:
			covered["event:"+string(c.Event)] = true
			covered["eventop:"+string(c.Op)] = true
			if c.CategoryIn != nil {
				covered["event_categories:"+string(c.Event)] = true
			}
		case BehaviorCondition:
			covered["predicate:"+string(c.Predicate)] = true
		}
	}
	var want []string
	for _, s := range Fields() {
		for _, op := range s.Operators {
			want = append(want, "attr:"+string(s.Field)+":"+string(op))
		}
	}
	for _, e := range EventNames() {
		want = append(want, "event:"+e, "event_categories:"+e)
	}
	for _, op := range EventOps() {
		want = append(want, "eventop:"+op)
	}
	for _, p := range BehaviorPredicates() {
		want = append(want, "predicate:"+p)
	}
	for _, k := range want {
		if !covered[k] {
			t.Errorf("no golden case for %s", k)
		}
	}
}

// Every registry field × allowed operator compiles (independently of the
// golden table) and the compiler has no definitions beyond the registry.
func TestCompilerDefinitionsMatchRegistry(t *testing.T) {
	for _, s := range Fields() {
		if _, ok := attributeColumns[s.Field]; !ok {
			t.Errorf("registry field %q has no compiler column", s.Field)
		}
		for _, op := range s.Operators {
			r := ruleOf(attr(s.Field, op, validSample(s, op)))
			v, err := Normalize(r)
			if err != nil {
				t.Fatalf("%s %s: %v", s.Field, op, err)
			}
			if _, _, err := Compile(v); err != nil {
				t.Errorf("%s %s: %v", s.Field, op, err)
			}
		}
	}
	for f := range attributeColumns {
		if _, ok := LookupField(f); !ok {
			t.Errorf("compiler column %q is not a registry field", f)
		}
	}
	if len(eventTables) != len(EventNames()) {
		t.Errorf("eventTables has %d entries, want %d", len(eventTables), len(EventNames()))
	}
	for _, e := range EventNames() {
		if _, ok := eventTables[EventName(e)]; !ok {
			t.Errorf("event %q has no table", e)
		}
	}
}

func TestCompileComposition(t *testing.T) {
	const (
		city      = `{"type":"attribute","field":"city","op":"eq","value":"Istanbul"}`
		abandoned = `{"type":"behavior","predicate":"abandoned_cart","within_days":30}`
		lang      = `{"type":"attribute","field":"language","op":"eq","value":"en"}`
		gender    = `{"type":"attribute","field":"gender","op":"in","value":["male","female"]}`
		age       = `{"type":"attribute","field":"age","op":"between","value":{"min":18,"max":30}}`
		carts     = `{"type":"event","event":"cart_add","op":"count_gte","count":3,"within_days":7,"category_in":["toys"]}`
		cityFrag  = "(u.city_normalized = $%d::text)"
		langFrag  = "(u.language = $%d::text)"
	)
	abandonedFrag := func(n int) string {
		return fmt.Sprintf("(EXISTS (SELECT 1 FROM cart_events AS c WHERE c.user_id = u.id AND c.occurred_at >= now() - make_interval(days => $%d::integer) AND c.occurred_at <= now()"+
			" AND NOT EXISTS (SELECT 1 FROM purchases AS p WHERE p.user_id = c.user_id AND p.purchased_at >= c.occurred_at AND p.purchased_at <= now())))", n)
	}
	genderFrag := func(n int) string { return fmt.Sprintf("(u.gender = ANY($%d::text[]))", n) }
	ageFrag := func(a, b int) string {
		return fmt.Sprintf("(u.birth_date <= (%s - make_interval(years => $%d::integer))::date AND u.birth_date > (%s - make_interval(years => $%d::integer + 1))::date)",
			goldenToday, a, goldenToday, b)
	}
	cartsFrag := func(a, b, c int) string {
		return fmt.Sprintf("((SELECT count(*) FROM cart_events AS e WHERE e.user_id = u.id AND e.occurred_at >= now() - make_interval(days => $%d::integer) AND e.occurred_at <= now() AND e.category = ANY($%d::text[])) >= $%d::bigint)", a, b, c)
	}

	tests := []struct {
		name, rule, sql string
		args            []any
	}{
		{
			"example brief: all",
			`{"match":"all","conditions":[` + city + `,` + abandoned + `]}`,
			goldenHead + "(" + fmt.Sprintf(cityFrag, 1) + " AND " + abandonedFrag(2) + ")",
			[]any{"istanbul", 30},
		},
		{
			"any",
			`{"match":"any","conditions":[` + city + `,` + lang + `]}`,
			goldenHead + "(" + fmt.Sprintf(cityFrag, 1) + " OR " + fmt.Sprintf(langFrag, 2) + ")",
			[]any{"istanbul", "en"},
		},
		{
			"all with groups: conditions first, then groups in order",
			`{"match":"all","conditions":[` + city + `],"groups":[{"match":"any","conditions":[` + lang + `,` + gender + `]},{"match":"all","conditions":[` + age + `,` + carts + `]}]}`,
			goldenHead + "(" + fmt.Sprintf(cityFrag, 1) +
				" AND (" + fmt.Sprintf(langFrag, 2) + " OR " + genderFrag(3) + ")" +
				" AND (" + ageFrag(4, 5) + " AND " + cartsFrag(6, 7, 8) + "))",
			[]any{"istanbul", "en", []string{"female", "male"}, 18, 30, 7, []string{"toys"}, 3},
		},
		{
			"any over groups: the between inside a group stays parenthesized",
			`{"match":"any","conditions":[` + lang + `],"groups":[{"match":"all","conditions":[` + age + `,` + city + `]}]}`,
			goldenHead + "(" + fmt.Sprintf(langFrag, 1) + " OR (" + ageFrag(2, 3) + " AND " + fmt.Sprintf(cityFrag, 4) + "))",
			[]any{"en", 18, 30, "istanbul"},
		},
		{
			"groups only",
			`{"match":"all","conditions":[],"groups":[{"match":"any","conditions":[` + city + `,` + abandoned + `]}]}`,
			goldenHead + "((" + fmt.Sprintf(cityFrag, 1) + " OR " + abandonedFrag(2) + "))",
			[]any{"istanbul", 30},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			sql, args := mustCompile(t, mustParse(t, tc.rule))
			if sql != tc.sql {
				t.Errorf("SQL\n got: %s\nwant: %s", sql, tc.sql)
			}
			if !reflect.DeepEqual(args, tc.args) {
				t.Errorf("args = %#v, want %#v", args, tc.args)
			}
			assertPlaceholders(t, sql, len(args))
		})
	}
}

// assertPlaceholders checks that $1..$n each occur, in increasing order of
// first appearance, and nothing beyond $n does.
func assertPlaceholders(t *testing.T, sql string, n int) {
	t.Helper()
	last := -1
	for i := 1; i <= n; i++ {
		p := fmt.Sprintf("$%d:", i)
		at := strings.Index(sql, p)
		if at < 0 {
			t.Fatalf("placeholder $%d missing in %s", i, sql)
		}
		if at < last {
			t.Errorf("placeholder $%d appears before $%d", i, i-1)
		}
		last = at
	}
	if strings.Contains(sql, fmt.Sprintf("$%d:", n+1)) {
		t.Errorf("unexpected placeholder $%d", n+1)
	}
}

func TestCompileMaximalRuleNumbersPlaceholders(t *testing.T) {
	cond := `{"type":"event","event":"purchase","op":"count_lte","count":1,"within_days":3,"category_in":["home","toys"]}`
	conds := strings.TrimSuffix(strings.Repeat(cond+",", MaxConditionsPerGroup), ",")
	groups := strings.TrimSuffix(strings.Repeat(`{"match":"any","conditions":[`+conds+`]},`, MaxGroups), ",")
	v := mustParse(t, `{"match":"all","conditions":[`+conds+`],"groups":[`+groups+`]}`)
	sql, args := mustCompile(t, v)
	want := 3 * MaxConditionsPerGroup * (MaxGroups + 1)
	if len(args) != want {
		t.Fatalf("len(args) = %d, want %d", len(args), want)
	}
	assertPlaceholders(t, sql, len(args))
	for i := 0; i < len(args); i += 3 {
		if args[i] != 3 || !reflect.DeepEqual(args[i+1], []string{"home", "toys"}) || args[i+2] != 1 {
			t.Fatalf("args[%d:%d] = %v", i, i+3, args[i:i+3])
		}
	}
}

func TestCompileIsDeterministic(t *testing.T) {
	rule := `{"match":"any","conditions":[{"type":"attribute","field":"city","op":"in","value":["İZMİR","istanbul","ISTANBUL"]},` +
		`{"type":"event","event":"cart_add","op":"count_gte","count":2,"within_days":10,"category_in":["toys","books"]}],` +
		`"groups":[{"match":"all","conditions":[{"type":"attribute","field":"age","op":"between","value":{"min":20,"max":40}}]}]}`
	v := mustParse(t, rule)
	sql1, args1 := mustCompile(t, v)
	for i := 0; i < 20; i++ {
		sql2, args2 := mustCompile(t, v)
		if sql1 != sql2 || !reflect.DeepEqual(args1, args2) {
			t.Fatal("same Validated compiled differently")
		}
	}
	// The persisted (normalized) form re-parses to the same statement.
	b, err := v.MarshalJSON()
	if err != nil {
		t.Fatal(err)
	}
	sql3, args3 := mustCompile(t, mustParse(t, string(b)))
	if sql1 != sql3 || !reflect.DeepEqual(args1, args3) {
		t.Fatal("normalized round trip compiled differently")
	}
	// Equivalent spellings normalize, and therefore compile, identically.
	alt := strings.NewReplacer(`"İZMİR","istanbul","ISTANBUL"`, `"Istanbul","izmir"`, `"toys","books"`, `"Books","TOYS","toys"`).Replace(rule)
	sql4, args4 := mustCompile(t, mustParse(t, alt))
	if sql1 != sql4 || !reflect.DeepEqual(args1, args4) {
		t.Fatalf("equivalent spellings compiled differently:\n%v\n%v", args1, args4)
	}
}

// The returned args do not alias the Validated's lists.
func TestCompileArgsAreCopies(t *testing.T) {
	v := mustParse(t, ruleJSON(`{"type":"attribute","field":"city","op":"in","value":["ankara","izmir"]}`))
	_, args := mustCompile(t, v)
	args[0].([]string)[0] = "x"
	_, again := mustCompile(t, v)
	if again[0].([]string)[0] != "ankara" {
		t.Fatal("mutating args changed the Validated rule")
	}
}

func TestCompileRejectsZeroValidated(t *testing.T) {
	var zero Validated
	sql, args, err := Compile(zero)
	if !errors.Is(err, ErrNotValidated) || sql != "" || args != nil {
		t.Fatalf("Compile(zero) = %q, %v, %v; want ErrNotValidated and no SQL", sql, args, err)
	}
}

// A Validated can only be produced by Normalize; these forged values (only
// constructible inside the package) stand in for a registry/compiler
// mismatch and must fail closed: an error, no panic, no SQL.
func TestCompileFailsClosedOnUncompilableRules(t *testing.T) {
	cnt := 1
	ok := AttributeCondition{Field: FieldCity, Op: OpEq, Value: StringValue("istanbul")}
	tests := map[string]Rule{
		"no conditions":           {Match: MatchAll},
		"unknown match":           {Match: "xor", Conditions: []Condition{ok}},
		"unknown group match":     {Match: MatchAll, Groups: []Group{{Match: "none", Conditions: []Condition{ok}}}},
		"empty group":             {Match: MatchAll, Conditions: []Condition{ok}, Groups: []Group{{Match: MatchAll}}},
		"nil condition":           {Match: MatchAll, Conditions: []Condition{nil}},
		"unknown field":           {Match: MatchAll, Conditions: []Condition{AttributeCondition{Field: "phone", Op: OpEq, Value: StringValue("x")}}},
		"operator not for field":  {Match: MatchAll, Conditions: []Condition{AttributeCondition{Field: FieldCity, Op: OpGte, Value: IntValue(1)}}},
		"between on text field":   {Match: MatchAll, Conditions: []Condition{AttributeCondition{Field: FieldGender, Op: OpBetween, Value: RangeValue(1, 2)}}},
		"eq on age":               {Match: MatchAll, Conditions: []Condition{AttributeCondition{Field: FieldAge, Op: OpEq, Value: StringValue("30")}}},
		"value kind mismatch":     {Match: MatchAll, Conditions: []Condition{AttributeCondition{Field: FieldCity, Op: OpEq, Value: IntValue(1)}}},
		"empty not_in list":       {Match: MatchAll, Conditions: []Condition{AttributeCondition{Field: FieldCity, Op: OpNotIn, Value: StringsValue()}}},
		"unknown operator":        {Match: MatchAll, Conditions: []Condition{AttributeCondition{Field: FieldCity, Op: "like", Value: StringValue("%")}}},
		"unknown event":           {Match: MatchAll, Conditions: []Condition{EventCondition{Event: "page_view", Op: EventExists, WithinDays: 1}}},
		"unknown event op":        {Match: MatchAll, Conditions: []Condition{EventCondition{Event: EventCartAdd, Op: "sum_gte", WithinDays: 1}}},
		"count missing":           {Match: MatchAll, Conditions: []Condition{EventCondition{Event: EventCartAdd, Op: EventCountGte, WithinDays: 1}}},
		"count on exists":         {Match: MatchAll, Conditions: []Condition{EventCondition{Event: EventCartAdd, Op: EventExists, Count: &cnt, WithinDays: 1}}},
		"window zero":             {Match: MatchAll, Conditions: []Condition{EventCondition{Event: EventCartAdd, Op: EventExists, WithinDays: 0}}},
		"empty category list":     {Match: MatchAll, Conditions: []Condition{EventCondition{Event: EventCartAdd, Op: EventExists, WithinDays: 1, CategoryIn: []string{}}}},
		"unknown predicate":       {Match: MatchAll, Conditions: []Condition{BehaviorCondition{Predicate: "churned", WithinDays: 1}}},
		"predicate window bounds": {Match: MatchAll, Conditions: []Condition{BehaviorCondition{Predicate: PredicateAbandonedCart, WithinDays: 366}}},
	}
	for name, r := range tests {
		t.Run(name, func(t *testing.T) {
			defer func() {
				if p := recover(); p != nil {
					t.Fatalf("panic: %v", p)
				}
			}()
			r := r
			sql, args, err := Compile(Validated{rule: &r})
			if !errors.Is(err, ErrUncompilable) {
				t.Fatalf("err = %v, want ErrUncompilable", err)
			}
			if sql != "" || args != nil {
				t.Fatalf("returned SQL %q args %v with an error", sql, args)
			}
		})
	}
}

// Injection strings cannot reach the compiler through the validator, and
// even a string that did (simulated with a forged Validated) would only ever
// be a bind value: the SQL text is a function of the rule's closed
// vocabulary, never of its values.
func TestCompileValuesNeverBecomeSQL(t *testing.T) {
	injections := []string{
		`istanbul'; DROP TABLE users; --`,
		`' OR '1'='1`,
		`$$ OR true $$`,
		`"; SELECT pg_sleep(10); --`,
		`istanbul) OR (1=1`,
		"istanbul\x00",
		`\'; COMMIT; --`,
	}
	for _, inj := range injections {
		for _, cond := range []string{
			`{"type":"attribute","field":"city","op":"eq","value":` + mustJSON(t, inj) + `}`,
			`{"type":"attribute","field":"country","op":"in","value":[` + mustJSON(t, inj) + `]}`,
			`{"type":"attribute","field":"gender","op":"eq","value":` + mustJSON(t, inj) + `}`,
			`{"type":"event","event":"cart_add","op":"exists","within_days":1,"category_in":[` + mustJSON(t, inj) + `]}`,
		} {
			if _, err := Parse([]byte(ruleJSON(cond))); err == nil {
				t.Errorf("injection value accepted by the validator: %s", cond)
			}
		}

		forged := Rule{Match: MatchAny, Conditions: []Condition{
			AttributeCondition{Field: FieldCity, Op: OpEq, Value: StringValue(inj)},
			AttributeCondition{Field: FieldCountry, Op: OpIn, Value: StringsValue(inj, "TR")},
			EventCondition{Event: EventPurchase, Op: EventExists, WithinDays: 5, CategoryIn: []string{inj}},
		}}
		sql, args, err := Compile(Validated{rule: &forged})
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(sql, inj) {
			t.Errorf("value %q appears in SQL text: %s", inj, sql)
		}
		if args[0] != inj || !reflect.DeepEqual(args[1], []string{inj, "TR"}) || !reflect.DeepEqual(args[3], []string{inj}) {
			t.Errorf("value not passed as bind parameter: %#v", args)
		}
		benign := Rule{Match: MatchAny, Conditions: []Condition{
			AttributeCondition{Field: FieldCity, Op: OpEq, Value: StringValue("ankara")},
			AttributeCondition{Field: FieldCountry, Op: OpIn, Value: StringsValue("DE")},
			EventCondition{Event: EventPurchase, Op: EventExists, WithinDays: 9, CategoryIn: []string{"toys"}},
		}}
		benignSQL, _, err := Compile(Validated{rule: &benign})
		if err != nil {
			t.Fatal(err)
		}
		if sql != benignSQL {
			t.Errorf("SQL text depends on values:\n%s\n%s", sql, benignSQL)
		}
	}
}

// For every golden case, changing only the values leaves the SQL text
// unchanged, and no string value (≥ 3 bytes; shorter ones such as "en" occur
// inside identifiers like cart_events by coincidence) occurs in the text.
func TestCompileTextIsValueIndependent(t *testing.T) {
	alt := map[string]string{
		"city eq":                             `{"type":"attribute","field":"city","op":"eq","value":"Van"}`,
		"city neq":                            `{"type":"attribute","field":"city","op":"neq","value":"Muş"}`,
		"city in":                             `{"type":"attribute","field":"city","op":"in","value":["Kars"]}`,
		"city not_in":                         `{"type":"attribute","field":"city","op":"not_in","value":["Rize","Bolu","Sinop"]}`,
		"country eq":                          `{"type":"attribute","field":"country","op":"eq","value":"US"}`,
		"country in":                          `{"type":"attribute","field":"country","op":"in","value":["FR"]}`,
		"gender eq":                           `{"type":"attribute","field":"gender","op":"eq","value":"male"}`,
		"gender in":                           `{"type":"attribute","field":"gender","op":"in","value":["female"]}`,
		"language eq":                         `{"type":"attribute","field":"language","op":"eq","value":"tr"}`,
		"language in":                         `{"type":"attribute","field":"language","op":"in","value":["tr"]}`,
		"age gte":                             `{"type":"attribute","field":"age","op":"gte","value":99}`,
		"age lte":                             `{"type":"attribute","field":"age","op":"lte","value":13}`,
		"age between":                         `{"type":"attribute","field":"age","op":"between","value":{"min":40,"max":41}}`,
		"signup_days_ago gte":                 `{"type":"attribute","field":"signup_days_ago","op":"gte","value":0}`,
		"signup_days_ago lte":                 `{"type":"attribute","field":"signup_days_ago","op":"lte","value":3650}`,
		"signup_days_ago between":             `{"type":"attribute","field":"signup_days_ago","op":"between","value":{"min":1,"max":2}}`,
		"last_active_days_ago gte":            `{"type":"attribute","field":"last_active_days_ago","op":"gte","value":1}`,
		"last_active_days_ago lte":            `{"type":"attribute","field":"last_active_days_ago","op":"lte","value":3650}`,
		"cart exists":                         `{"type":"event","event":"cart_add","op":"exists","within_days":365}`,
		"cart not_exists":                     `{"type":"event","event":"cart_add","op":"not_exists","within_days":1}`,
		"purchase exists with categories":     `{"type":"event","event":"purchase","op":"exists","within_days":8,"category_in":["grocery"]}`,
		"purchase not_exists with categories": `{"type":"event","event":"purchase","op":"not_exists","within_days":91,"category_in":["beauty","fashion"]}`,
		"cart count_gte with categories":      `{"type":"event","event":"cart_add","op":"count_gte","count":100,"within_days":2,"category_in":["electronics"]}`,
		"purchase count_lte":                  `{"type":"event","event":"purchase","op":"count_lte","count":7,"within_days":2}`,
		"purchase count_gte zero":             `{"type":"event","event":"purchase","op":"count_gte","count":5,"within_days":365}`,
		"abandoned_cart":                      `{"type":"behavior","predicate":"abandoned_cart","within_days":1}`,
	}
	for _, tc := range goldenConditions {
		changed, ok := alt[tc.name]
		if !ok {
			t.Errorf("%s: no alternative values", tc.name)
			continue
		}
		a, argsA := mustCompile(t, mustParse(t, ruleJSON(tc.cond)))
		b, argsB := mustCompile(t, mustParse(t, ruleJSON(changed)))
		if a != b {
			t.Errorf("%s: SQL text changed with values:\n%s\n%s", tc.name, a, b)
		}
		if reflect.DeepEqual(argsA, argsB) {
			t.Errorf("%s: alternative values did not change args", tc.name)
		}
		for _, args := range [][]any{argsA, argsB} {
			for _, arg := range args {
				for _, s := range stringsOf(arg) {
					if len(s) >= 3 && strings.Contains(a, s) {
						t.Errorf("%s: string value %q occurs in SQL text", tc.name, s)
					}
				}
			}
		}
	}
}

func stringsOf(v any) []string {
	switch v := v.(type) {
	case string:
		return []string{v}
	case []string:
		return v
	}
	return nil
}
