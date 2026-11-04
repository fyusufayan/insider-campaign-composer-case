package ai

import (
	"strings"
	"testing"

	"github.com/fyusufayan/insider-campaign-composer-case/backend/internal/segment"
)

func TestParseInterpretationValid(t *testing.T) {
	in, errs := ParseInterpretation([]byte(validOutput))
	if len(errs) > 0 {
		t.Fatal(errs)
	}
	if in.Segment.IsZero() || in.Summary == "" || len(in.Assumptions) != 1 || in.Unsupported == nil {
		t.Errorf("interpretation = %+v", in)
	}
}

func TestParseInterpretationNormalizes(t *testing.T) {
	raw := mutate(t, keySuggestedChannels, `["email","sms","email"]`)
	raw = strings.Replace(raw, `"interpretation":"Users`, `"interpretation":"  Users`, 1)
	in, errs := ParseInterpretation([]byte(raw))
	if len(errs) > 0 {
		t.Fatal(errs)
	}
	if len(in.SuggestedChannels) != 2 || in.SuggestedChannels[0] != segment.ChannelSMS {
		t.Errorf("channels = %v, want canonical [sms email]", in.SuggestedChannels)
	}
	if strings.HasPrefix(in.Summary, " ") {
		t.Error("summary not trimmed")
	}
	// Decomposed text is stored NFC.
	in, errs = ParseInterpretation([]byte(mutate(t, keyAssumptions, `["İstanbul"]`)))
	if len(errs) > 0 || in.Assumptions[0] != "İstanbul" {
		t.Errorf("assumptions = %q, %v", in.Assumptions, errs)
	}
}

// Every field of the envelope is validated; each case names one expected error.
func TestParseInterpretationErrors(t *testing.T) {
	brief := func(cb string) string { return mutate(t, keyContentBrief, cb) }
	tests := []struct {
		name, raw, path, code string
	}{
		{"empty", ``, "", segment.CodeMalformedJSON},
		{"not an object", `[]`, "", segment.CodeWrongType},
		{"null", `null`, "", segment.CodeWrongType},
		{"invalid utf8", "{\"a\":\"\xff\"}", "", segment.CodeMalformedJSON},
		{"too large", `"` + strings.Repeat("x", maxEnvelopeBytes) + `"`, "", segment.CodeTooLarge},
		{"duplicate top-level key", `{"interpretation":"a",` + validOutput[1:], "interpretation", segment.CodeDuplicateProperty},
		{"duplicate nested key", mutate(t, keyContentBrief, `{"goal":"g","goal":"h","tone":"t","length":"short","offer":{"kind":"none"}}`), "content_brief.goal", segment.CodeDuplicateProperty},
		{"unknown property", mutate(t, "notes", `[]`), "notes", segment.CodeUnknownProperty},
		{"missing segment", mutate(t, keySegment, ""), "segment", segment.CodeRequired},
		{"null segment", mutate(t, keySegment, "null"), "segment", segment.CodeWrongType},
		{"segment empty conditions", mutate(t, keySegment, `{"match":"all","conditions":[]}`), "segment.conditions", segment.CodeEmpty},
		{"segment nested group", mutate(t, keySegment, `{"match":"all","conditions":[{"type":"behavior","predicate":"abandoned_cart","within_days":3}],"groups":[{"match":"any","conditions":[],"groups":[]}]}`), "segment.groups[0].groups", segment.CodeNestedGroups},
		{"segment sql injection value", mutate(t, keySegment, `{"match":"all","conditions":[{"type":"attribute","field":"city","op":"eq","value":"x'); DROP TABLE users;--"}]}`), "segment.conditions[0].value", segment.CodeUnknownCity},
		{"summary wrong type", mutate(t, keyInterpretation, `42`), "interpretation", segment.CodeWrongType},
		{"summary empty", mutate(t, keyInterpretation, `"   "`), "interpretation", segment.CodeEmpty},
		{"summary too long", mutate(t, keyInterpretation, `"`+strings.Repeat("ü", MaxSummaryRunes+1)+`"`), "interpretation", segment.CodeTooLong},
		{"summary control char", mutate(t, keyInterpretation, `"a\u0000b"`), "interpretation", segment.CodeInvalidValue},
		{"assumptions not array", mutate(t, keyAssumptions, `"x"`), "assumptions", segment.CodeWrongType},
		{"assumptions null", mutate(t, keyAssumptions, `null`), "assumptions", segment.CodeWrongType},
		{"assumptions entry not string", mutate(t, keyAssumptions, `[1]`), "assumptions[0]", segment.CodeWrongType},
		{"assumptions entry empty", mutate(t, keyAssumptions, `[""]`), "assumptions[0]", segment.CodeEmpty},
		{"assumptions too many", mutate(t, keyAssumptions, `["a","b","c","d","e","f","g","h","i","j","k"]`), "assumptions", segment.CodeTooMany},
		{"unsupported entry too long", mutate(t, keyUnsupported, `["`+strings.Repeat("ş", MaxNoteRunes+1)+`"]`), "unsupported[0]", segment.CodeTooLong},
		{"unsupported missing", mutate(t, keyUnsupported, ""), "unsupported", segment.CodeRequired},
		{"channels empty", mutate(t, keySuggestedChannels, `[]`), "suggested_channels", segment.CodeEmpty},
		{"channels unknown", mutate(t, keySuggestedChannels, `["whatsapp"]`), "suggested_channels[0]", segment.CodeNotAllowed},
		{"channels case sensitive", mutate(t, keySuggestedChannels, `["SMS"]`), "suggested_channels[0]", segment.CodeNotAllowed},
		{"channels not strings", mutate(t, keySuggestedChannels, `[{"c":"sms"}]`), "suggested_channels[0]", segment.CodeWrongType},
		{"content brief not object", brief(`"short"`), "content_brief", segment.CodeWrongType},
		{"content brief unknown key", brief(`{"goal":"g","tone":"t","length":"short","offer":{"kind":"none"},"cta":"x"}`), "content_brief.cta", segment.CodeUnknownProperty},
		{"goal missing", brief(`{"tone":"t","length":"short","offer":{"kind":"none"}}`), "content_brief.goal", segment.CodeRequired},
		{"goal too long", brief(`{"goal":"` + strings.Repeat("g", MaxGoalRunes+1) + `","tone":"t","length":"short","offer":{"kind":"none"}}`), "content_brief.goal", segment.CodeTooLong},
		{"tone too long", brief(`{"goal":"g","tone":"` + strings.Repeat("t", MaxToneRunes+1) + `","length":"short","offer":{"kind":"none"}}`), "content_brief.tone", segment.CodeTooLong},
		{"tone empty", brief(`{"goal":"g","tone":"","length":"short","offer":{"kind":"none"}}`), "content_brief.tone", segment.CodeEmpty},
		{"length unknown", brief(`{"goal":"g","tone":"t","length":"tiny","offer":{"kind":"none"}}`), "content_brief.length", segment.CodeNotAllowed},
		{"offer missing", brief(`{"goal":"g","tone":"t","length":"short"}`), "content_brief.offer", segment.CodeRequired},
		{"offer kind unknown", brief(`{"goal":"g","tone":"t","length":"short","offer":{"kind":"amount_discount","value":10}}`), "content_brief.offer.kind", segment.CodeNotAllowed},
		{"offer kind missing", brief(`{"goal":"g","tone":"t","length":"short","offer":{"value":10}}`), "content_brief.offer.kind", segment.CodeRequired},
		{"offer unknown key", brief(`{"goal":"g","tone":"t","length":"short","offer":{"kind":"none","currency":"TRY"}}`), "content_brief.offer.currency", segment.CodeUnknownProperty},
		{"percent missing value", brief(`{"goal":"g","tone":"t","length":"short","offer":{"kind":"percent_discount"}}`), "content_brief.offer.value", segment.CodeRequired},
		{"percent null value", brief(`{"goal":"g","tone":"t","length":"short","offer":{"kind":"percent_discount","value":null}}`), "content_brief.offer.value", segment.CodeWrongType},
		{"percent zero", brief(`{"goal":"g","tone":"t","length":"short","offer":{"kind":"percent_discount","value":0}}`), "content_brief.offer.value", segment.CodeOutOfRange},
		{"percent over 100", brief(`{"goal":"g","tone":"t","length":"short","offer":{"kind":"percent_discount","value":101}}`), "content_brief.offer.value", segment.CodeOutOfRange},
		{"percent fractional", brief(`{"goal":"g","tone":"t","length":"short","offer":{"kind":"percent_discount","value":12.5}}`), "content_brief.offer.value", segment.CodeWrongType},
		{"percent string", brief(`{"goal":"g","tone":"t","length":"short","offer":{"kind":"percent_discount","value":"30"}}`), "content_brief.offer.value", segment.CodeWrongType},
		{"free shipping with value", brief(`{"goal":"g","tone":"t","length":"short","offer":{"kind":"free_shipping","value":1}}`), "content_brief.offer.value", segment.CodeNotAllowed},
		{"none with value", brief(`{"goal":"g","tone":"t","length":"short","offer":{"kind":"none","value":0}}`), "content_brief.offer.value", segment.CodeNotAllowed},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			in, errs := ParseInterpretation([]byte(tt.raw))
			if len(errs) == 0 {
				t.Fatalf("accepted: %+v", in)
			}
			if !hasDetail(errs, tt.path, tt.code) {
				t.Errorf("errors %+v lack %s/%s", errs, tt.path, tt.code)
			}
			if !in.Segment.IsZero() {
				t.Error("an invalid interpretation carries a segment")
			}
		})
	}
}

// All problems of one output are reported together, segment paths prefixed.
func TestParseInterpretationReportsAllErrors(t *testing.T) {
	raw := `{"segment":{"match":"all","conditions":[{"type":"attribute","field":"age","op":"gte","value":7},{"type":"behavior","predicate":"abandoned_cart","within_days":0}]},` +
		`"interpretation":"","assumptions":[],"unsupported":[],"suggested_channels":["fax"],` +
		`"content_brief":{"goal":"g","tone":"t","length":"short","offer":{"kind":"percent_discount","value":500}}}`
	_, errs := ParseInterpretation([]byte(raw))
	for _, want := range [][2]string{
		{"segment.conditions[0].value", segment.CodeOutOfRange},
		{"segment.conditions[1].within_days", segment.CodeOutOfRange},
		{"interpretation", segment.CodeEmpty},
		{"suggested_channels[0]", segment.CodeNotAllowed},
		{"content_brief.offer.value", segment.CodeOutOfRange},
	} {
		if !hasDetail(errs, want[0], want[1]) {
			t.Errorf("missing %v in %+v", want, errs)
		}
	}
}

func FuzzParseInterpretation(f *testing.F) {
	f.Add([]byte(validOutput))
	f.Add([]byte(`{"segment":null}`))
	f.Fuzz(func(t *testing.T, data []byte) {
		in, errs := ParseInterpretation(data)
		if len(errs) == 0 && in.Segment.IsZero() {
			t.Fatal("accepted without a segment")
		}
	})
}
