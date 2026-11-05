package segment

import (
	"errors"
	"reflect"
	"strings"
	"testing"
)

func mustParseRule(t *testing.T, js string) Validated {
	t.Helper()
	v, err := Parse([]byte(js))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	return v
}

func TestCompileEligible(t *testing.T) {
	rule := mustParseRule(t, ruleJSON(
		`{"type":"attribute","field":"city","op":"eq","value":"Istanbul"}`,
		`{"type":"behavior","predicate":"abandoned_cart","within_days":30}`))
	matched, matchedArgs := mustCompile(t, rule)

	q, err := CompileEligible(rule, []Channel{ChannelEmail, ChannelSMS, ChannelEmail})
	if err != nil {
		t.Fatal(err)
	}
	n := len(matchedArgs)
	if q.NextParam != n+2 || len(q.Args) != n+1 {
		t.Fatalf("NextParam = %d, args = %d, matched args = %d", q.NextParam, len(q.Args), n)
	}
	if !reflect.DeepEqual(q.Args[:n], matchedArgs) {
		t.Errorf("rule args changed: %#v", q.Args)
	}
	// Channels are de-duplicated, in canonical order, and bound (never SQL text).
	if !reflect.DeepEqual(q.Args[n], []string{"sms", "email"}) {
		t.Errorf("channels arg = %#v", q.Args[n])
	}
	wantPrefix := "WITH matched AS (" + matched + sqlClassifiedCTE + ",\neligible AS ("
	if !strings.HasPrefix(q.With, wantPrefix) {
		t.Errorf("With does not start with the compiled rule:\n%s", q.With)
	}
	if !strings.Contains(q.With, "unnest($3::text[]::channel[])") || strings.Contains(q.With, channelsMarker) {
		t.Errorf("channels placeholder not substituted:\n%s", q.With)
	}
	// Eligibility is the eligible cell (code 2) of the same classified CTE
	// that the audience statistics count.
	if !strings.Contains(q.With, "FROM classified AS c") ||
		!strings.Contains(q.With, "WHEN 'sms' THEN c.sms WHEN 'email' THEN c.email END) = 2") {
		t.Errorf("eligible is not derived from classified:\n%s", q.With)
	}
	if !strings.HasPrefix(sqlAudienceStatsTail, sqlClassifiedCTE) {
		t.Error("audience statistics do not use the shared classified CTE")
	}
	// The snapshot statement reads the partition cells and the eligible
	// summary in one SELECT.
	if !strings.HasPrefix(sqlSnapshotAudienceTail, sqlAudienceCells) || !strings.Contains(sqlSnapshotAudienceTail, "FROM eligible") {
		t.Error("snapshot statement does not combine cells and eligible")
	}

	// Deterministic.
	q2, _ := CompileEligible(rule, []Channel{ChannelSMS, ChannelEmail})
	if q2.With != q.With || !reflect.DeepEqual(q2.Args, q.Args) {
		t.Error("not deterministic")
	}
	// A single channel.
	q3, _ := CompileEligible(rule, []Channel{ChannelEmail})
	if !reflect.DeepEqual(q3.Args[n], []string{"email"}) {
		t.Errorf("email-only arg = %#v", q3.Args[n])
	}
}

func TestCompileEligibleRejects(t *testing.T) {
	rule := mustParseRule(t, ruleJSON(`{"type":"attribute","field":"city","op":"eq","value":"Ankara"}`))
	if _, err := CompileEligible(rule, nil); !errors.Is(err, ErrNoChannels) {
		t.Errorf("no channels: %v", err)
	}
	if _, err := CompileEligible(rule, []Channel{}); !errors.Is(err, ErrNoChannels) {
		t.Errorf("empty channels: %v", err)
	}
	if _, err := CompileEligible(rule, []Channel{"push"}); !errors.Is(err, ErrUnknownChannel) {
		t.Errorf("unknown channel: %v", err)
	}
	if _, err := CompileEligible(Validated{}, []Channel{ChannelSMS}); !errors.Is(err, ErrNotValidated) {
		t.Errorf("zero rule: %v", err)
	}
}

func TestCompileEligibleValuesNeverBecomeSQL(t *testing.T) {
	inj := `istanbul'; DROP TABLE campaign_deliveries; --`
	forged := Rule{Match: MatchAll, Conditions: []Condition{
		AttributeCondition{Field: FieldCity, Op: OpEq, Value: StringValue(inj)},
	}}
	q, err := CompileEligible(Validated{rule: &forged}, []Channel{ChannelSMS})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(q.With, inj) || q.Args[0] != inj {
		t.Errorf("value in SQL text or not bound: %s %#v", q.With, q.Args)
	}
}
