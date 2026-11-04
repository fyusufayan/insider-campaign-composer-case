package content

import (
	"encoding/json"
	"errors"
	"slices"
	"strconv"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/fyusufayan/insider-campaign-composer-case/backend/internal/segment"
)

var (
	chSMS   = []segment.Channel{segment.ChannelSMS}
	chEmail = []segment.Channel{segment.ChannelEmail}
	chBoth  = []segment.Channel{segment.ChannelSMS, segment.ChannelEmail}
)

func sp(s string) *string { return &s }

// r returns the rune as a string; used for invisible characters so the test
// source stays readable.
func r(c rune) string { return string(c) }

func errCodes(errs []segment.FieldError) []string {
	out := make([]string, len(errs))
	for i, e := range errs {
		out[i] = e.Path + ":" + e.Code
	}
	return out
}

func smsOnly(s string) Draft { return Draft{SMSBody: sp(s)} }

func email(subject, body string) Draft { return Draft{EmailSubject: sp(subject), EmailBody: sp(body)} }

func mustParse(t *testing.T, d Draft, chs []segment.Channel) (Content, Result) {
	t.Helper()
	c, res := Parse(d, chs)
	if !res.OK() {
		t.Fatalf("unexpected errors: %v", errCodes(res.Errors))
	}
	return c, res
}

func TestRegistry(t *testing.T) {
	vs := Variables()
	got := []string{}
	for _, v := range vs {
		got = append(got, string(v.Name)+"="+itoa(v.MaxRunes))
	}
	want := []string{"first_name=20", "last_name=25", "city=" + itoa(segment.Cities().MaxNameRunes())}
	if !slices.Equal(got, want) {
		t.Fatalf("registry = %v, want %v", got, want)
	}
	if segment.Cities().MaxNameRunes() != utf8.RuneCountInString("Afyonkarahisar") {
		t.Fatalf("city max = %d, want the longest province name", segment.Cities().MaxNameRunes())
	}
	vs[0].MaxRunes = 999 // the copy must not alias the registry
	if v, _ := LookupVariable("first_name"); v.MaxRunes != MaxFirstNameRunes {
		t.Fatal("Variables() aliases the registry")
	}
	if _, ok := LookupVariable("First_Name"); ok {
		t.Fatal("lookup must be case-sensitive")
	}
}

func itoa(n int) string { return strconv.Itoa(n) }

func TestValidSMS(t *testing.T) {
	text := "Merhaba {{first_name}}, sepetindeki ürünler seni bekliyor! %30 indirim: ornek.com/sepet"
	c, res := mustParse(t, smsOnly(text), chSMS)
	tmpl, ok := c.SMSBody()
	if !ok || tmpl.String() != text {
		t.Fatalf("sms = %q, %v", tmpl.String(), ok)
	}
	literal := utf8.RuneCountInString(strings.Replace(text, "{{first_name}}", "", 1))
	if res.SMS == nil || res.SMS.CharsWorstCase != literal+20 || res.SMS.Limit != 160 {
		t.Fatalf("sms stats = %+v, want %d", res.SMS, literal+20)
	}
	if res.Subject != nil || res.Body != nil {
		t.Fatal("stats for absent fields")
	}
	if res.RenderedExample == nil || *res.RenderedExample.SMS != strings.Replace(text, "{{first_name}}", "Ayşe", 1) {
		t.Fatalf("rendered example = %+v", res.RenderedExample)
	}
	if len(res.Warnings) != 0 || len(res.Errors) != 0 {
		t.Fatalf("warnings/errors = %v %v", res.Warnings, res.Errors)
	}
	if got := c.Channels(); !slices.Equal(got, chSMS) {
		t.Fatalf("channels = %v", got)
	}
}

func TestValidEmail(t *testing.T) {
	subject := "{{first_name|Merhaba}}, sepetin seni bekliyor"
	body := "Merhaba {{first_name|değerli müşterimiz}},\n\n{{city}} için özel %30 indirim.\nSevgiler"
	c, res := mustParse(t, email(subject, body), chEmail)
	if !c.Has(segment.ChannelEmail) || c.Has(segment.ChannelSMS) {
		t.Fatal("channel presence")
	}
	if res.Subject.Limit != 80 || res.Body.Limit != 5000 {
		t.Fatalf("limits %+v %+v", res.Subject, res.Body)
	}
	e := res.RenderedExample.Email
	if e.Subject != "Ayşe, sepetin seni bekliyor" ||
		e.Body != "Merhaba Ayşe,\n\nİstanbul için özel %30 indirim.\nSevgiler" || e.Footer != EmailFooter {
		t.Fatalf("rendered = %+v", e)
	}
	if !strings.HasSuffix(e.Text(), "\n\n-- \n"+EmailFooter) {
		t.Fatalf("text = %q", e.Text())
	}
}

func TestPlaceholdersApproved(t *testing.T) {
	cases := []struct {
		text  string
		phs   []Placeholder
		worst int
	}{
		{"{{first_name}}", []Placeholder{{Variable: VarFirstName}}, 20},
		{"{{last_name}}", []Placeholder{{Variable: VarLastName}}, 25},
		{"{{city}}", []Placeholder{{Variable: VarCity}}, 14},
		{"{{first_name|there}}", []Placeholder{{Variable: VarFirstName, Fallback: "there", HasFallback: true}}, 20},
		// A fallback longer than the declared maximum sets the worst case.
		{"{{first_name|our most valued customer}}", []Placeholder{{Variable: VarFirstName, Fallback: "our most valued customer", HasFallback: true}}, 24},
		{"{{city|şehrin}}", []Placeholder{{Variable: VarCity, Fallback: "şehrin", HasFallback: true}}, 14},
		// Fallback may contain spaces, punctuation, a pipe-free Turkish text.
		{"Hi {{first_name|dear friend}}!", []Placeholder{{Variable: VarFirstName, Fallback: "dear friend", HasFallback: true}}, 4 + 20},
		// Repeated and multiple placeholders, adjacent.
		{"{{first_name}}{{first_name}} {{last_name}}, {{city}}",
			[]Placeholder{{Variable: VarFirstName}, {Variable: VarFirstName}, {Variable: VarLastName}, {Variable: VarCity}},
			20 + 20 + 1 + 25 + 2 + 14},
	}
	for _, tc := range cases {
		t.Run(tc.text, func(t *testing.T) {
			c, _ := mustParse(t, smsOnly(tc.text), chSMS)
			tmpl, _ := c.SMSBody()
			if got := tmpl.Placeholders(); !slices.Equal(got, tc.phs) {
				t.Fatalf("placeholders = %+v, want %+v", got, tc.phs)
			}
			if got := tmpl.WorstCaseRunes(); got != tc.worst {
				t.Fatalf("worst = %d, want %d", got, tc.worst)
			}
		})
	}
}

func TestPlaceholderErrors(t *testing.T) {
	cases := []struct {
		name, text string
		want       []string
	}{
		{"unknown", "Hi {{firstname}}", []string{"sms_body:unknown_variable"}},
		{"misspelled", "Hi {{frist_name}}", []string{"sms_body:unknown_variable"}},
		{"wrong case", "Hi {{First_Name}}", []string{"sms_body:unknown_variable"}},
		{"customer data field", "Hi {{email}}", []string{"sms_body:unknown_variable"}},
		{"phone", "Call {{phone}}", []string{"sms_body:unknown_variable"}},
		{"empty", "Hi {{}}", []string{"sms_body:malformed_placeholder"}},
		{"spaces", "Hi {{ first_name }}", []string{"sms_body:malformed_placeholder"}},
		{"go template field", "Hi {{.FirstName}}", []string{"sms_body:malformed_placeholder"}},
		{"go template action", "{{if .X}}hi{{end}}", []string{"sms_body:malformed_placeholder", "sms_body:unknown_variable"}},
		{"filter", "Hi {{first_name | upper}}", []string{"sms_body:malformed_placeholder"}},
		{"function call", "Hi {{upper(first_name)}}", []string{"sms_body:malformed_placeholder"}},
		{"method call", "Hi {{first_name.upper}}", []string{"sms_body:malformed_placeholder"}},
		{"call syntax", "{{call .F}}", []string{"sms_body:malformed_placeholder"}},
		{"mustache section", "{{#first_name}}x{{/first_name}}", []string{"sms_body:malformed_placeholder", "sms_body:malformed_placeholder"}},
		{"partial", "{{>header}}", []string{"sms_body:malformed_placeholder"}},
		{"two pipes", "{{first_name|a|b}}", []string{"sms_body:malformed_placeholder"}},
		{"nested", "Hi {{first_name {{city}} }}", []string{"sms_body:malformed_placeholder", "sms_body:unbalanced_delimiter"}},
		{"triple open", "Hi {{{first_name}}}", []string{"sms_body:malformed_placeholder", "sms_body:unbalanced_delimiter"}},
		{"triple close", "Hi {{first_name}}}", []string{"sms_body:unbalanced_delimiter"}},
		{"unterminated", "Hi {{first_name, welcome", []string{"sms_body:unbalanced_delimiter"}},
		{"unterminated single close", "Hi {{first_name}", []string{"sms_body:unbalanced_delimiter"}},
		{"stray close", "Hi first_name}}", []string{"sms_body:unbalanced_delimiter"}},
		{"single braces", "Hi {first_name}", []string{"sms_body:unbalanced_delimiter", "sms_body:unbalanced_delimiter"}},
		{"shell style", "Hi ${first_name}", []string{"sms_body:unbalanced_delimiter", "sms_body:unbalanced_delimiter"}},
		{"json-like", `{"a":1}`, []string{"sms_body:unbalanced_delimiter", "sms_body:unbalanced_delimiter"}},
		{"empty fallback", "Hi {{first_name|}}", []string{"sms_body:invalid_fallback"}},
		{"fallback leading space", "Hi {{first_name| there}}", []string{"sms_body:invalid_fallback"}},
		{"fallback newline", "Hi {{first_name|a\nb}}", []string{"sms_body:invalid_fallback"}},
		{"unknown with fallback", "Hi {{nickname|there}}", []string{"sms_body:unknown_variable"}},
		{"pipe only", "Hi {{|there}}", []string{"sms_body:malformed_placeholder"}},
		{"sql-ish", "{{first_name'; DROP TABLE users;--}}", []string{"sms_body:malformed_placeholder"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c, res := Parse(smsOnly(tc.text), chSMS)
			if got := errCodes(res.Errors); !slices.Equal(got, tc.want) {
				t.Fatalf("errors = %v, want %v", got, tc.want)
			}
			if !c.IsZero() || res.SMS != nil || res.RenderedExample != nil {
				t.Fatal("invalid template produced content, stats or a rendering")
			}
			for _, e := range res.Errors {
				if e.Code == CodeUnknownVariable || e.Code == CodeMalformedPlaceholder {
					if !slices.Equal(e.Allowed, []string{"first_name", "last_name", "city"}) {
						t.Fatalf("allowed = %v", e.Allowed)
					}
				}
				if !strings.Contains(e.Message, "at character ") {
					t.Fatalf("message lacks a position: %q", e.Message)
				}
			}
		})
	}
}

func TestErrorPositionsAreRuneBased(t *testing.T) {
	_, res := Parse(smsOnly("İğüş {{x}}"), chSMS)
	if len(res.Errors) != 1 || !strings.Contains(res.Errors[0].Message, "at character 6") {
		t.Fatalf("errors = %+v", res.Errors)
	}
}

func TestMessagesNeverEchoInput(t *testing.T) {
	const marker = "SECRETMARKER"
	inputs := []string{
		"{{" + marker + "}}", "{{first_name|" + marker + "|x}}", "{{ " + marker + " }}",
		"{" + marker, marker + "}", "{{" + marker, "<" + marker + ">", "{{first_name| " + marker + "}}",
	}
	for _, in := range inputs {
		_, res := Parse(Draft{SMSBody: sp(in), EmailSubject: sp(in), EmailBody: sp(in)}, chBoth)
		if res.OK() {
			t.Fatalf("%q unexpectedly valid", in)
		}
		b, _ := json.Marshal(res)
		if strings.Contains(string(b), marker) {
			t.Fatalf("result echoes input: %s", b)
		}
	}
}

func TestTurkishAndUnicodeCounting(t *testing.T) {
	decomposed := "I" + r(0x0307) + "stanbul" // İ as I + combining dot above
	c, res := mustParse(t, smsOnly(decomposed), chSMS)
	tmpl, _ := c.SMSBody()
	if tmpl.String() != "İstanbul" || res.SMS.CharsWorstCase != 8 {
		t.Fatalf("NFC: %q (%d runes)", tmpl.String(), res.SMS.CharsWorstCase)
	}

	turkish := "çğıöşüÇĞİÖŞÜ"
	_, res = mustParse(t, smsOnly(turkish), chSMS)
	if res.SMS.CharsWorstCase != 12 {
		t.Fatalf("turkish = %d", res.SMS.CharsWorstCase)
	}

	// Emoji with ZWJ and variation selectors are allowed; each code point counts.
	family := "👩" + r(0x200D) + "👩" + r(0x200D) + "👧"
	_, res = mustParse(t, smsOnly("Aile "+family+" ❤"+r(0xFE0F)), chSMS)
	if want := 5 + 5 + 1 + 2; res.SMS.CharsWorstCase != want {
		t.Fatalf("emoji = %d, want %d", res.SMS.CharsWorstCase, want)
	}

	// 160 Turkish letters (2 bytes each) are exactly at the limit: runes, not bytes.
	_, res = mustParse(t, smsOnly(strings.Repeat("ş", 160)), chSMS)
	if res.SMS.CharsWorstCase != 160 {
		t.Fatal("runes vs bytes")
	}
	// 160 decomposed ş (s + U+0327) normalize to 160 runes.
	_, res = mustParse(t, smsOnly(strings.Repeat("s"+r(0x0327), 160)), chSMS)
	if res.SMS.CharsWorstCase != 160 {
		t.Fatalf("decomposed = %d", res.SMS.CharsWorstCase)
	}
}

func TestLengthBoundaries(t *testing.T) {
	ph := "{{first_name}}" // worst case 20
	cases := []struct {
		name  string
		d     Draft
		chs   []segment.Channel
		want  []string
		worst int
	}{
		{"sms 160", smsOnly(strings.Repeat("a", 160)), chSMS, nil, 160},
		{"sms 161", smsOnly(strings.Repeat("a", 161)), chSMS, []string{"sms_body:too_long"}, 161},
		{"sms 140+name", smsOnly(strings.Repeat("a", 140) + ph), chSMS, nil, 160},
		{"sms 141+name", smsOnly(strings.Repeat("a", 141) + ph), chSMS, []string{"sms_body:too_long"}, 161},
		{"sms long fallback", smsOnly(strings.Repeat("a", 135) + "{{first_name|" + strings.Repeat("b", 26) + "}}"), chSMS, []string{"sms_body:too_long"}, 161},
		{"sms fallback fits", smsOnly(strings.Repeat("a", 135) + "{{first_name|" + strings.Repeat("b", 25) + "}}"), chSMS, nil, 160},
		{"sms trailing spaces trimmed", smsOnly("  " + strings.Repeat("a", 160) + " \n "), chSMS, nil, 160},
		{"sms newline counts", smsOnly(strings.Repeat("a", 80) + "\n" + strings.Repeat("a", 80)), chSMS, []string{"sms_body:too_long"}, 161},
		{"subject 80", email(strings.Repeat("ğ", 80), "b"), chEmail, nil, 80},
		{"subject 81", email(strings.Repeat("ğ", 81), "b"), chEmail, []string{"email_subject:too_long"}, 81},
		{"subject 41+city+last", email(strings.Repeat("x", 41)+"{{city}}{{last_name}}", "b"), chEmail, nil, 80},
		{"subject 42+city+last", email(strings.Repeat("x", 42)+"{{city}}{{last_name}}", "b"), chEmail, []string{"email_subject:too_long"}, 81},
		{"body 5000", email("s", strings.Repeat("ö", 5000)), chEmail, nil, 5000},
		{"body 5001", email("s", strings.Repeat("ö", 5001)), chEmail, []string{"email_body:too_long"}, 5001},
		{"body 4980+name", email("s", strings.Repeat("ö", 4980)+ph), chEmail, nil, 5000},
		{"body 4981+name", email("s", strings.Repeat("ö", 4981)+ph), chEmail, []string{"email_body:too_long"}, 5001},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, res := Parse(tc.d, tc.chs)
			if got := errCodes(res.Errors); !slices.Equal(got, tc.want) && !(len(got) == 0 && tc.want == nil) {
				t.Fatalf("errors = %v, want %v", got, tc.want)
			}
			var st *LengthStats
			switch {
			case tc.d.SMSBody != nil:
				st = res.SMS
			case strings.HasPrefix(tc.name, "subject"):
				st = res.Subject
			default:
				st = res.Body
			}
			if st == nil || st.CharsWorstCase != tc.worst {
				t.Fatalf("stats = %+v, want worst %d", st, tc.worst)
			}
			if len(tc.want) > 0 && !strings.Contains(res.Errors[0].Message, "(is "+itoa(tc.worst)+")") {
				t.Fatalf("message = %q", res.Errors[0].Message)
			}
		})
	}
}

func TestWhitespaceAndCharacters(t *testing.T) {
	cases := []struct {
		name string
		d    Draft
		chs  []segment.Channel
		want []string
	}{
		{"sms empty string", smsOnly(""), chSMS, []string{"sms_body:empty"}},
		{"sms whitespace", smsOnly(" \n\t "), chSMS, []string{"sms_body:empty"}},
		{"sms nbsp only", smsOnly(r(0x00A0) + r(0x3000)), chSMS, []string{"sms_body:empty"}},
		{"sms zero-width only", smsOnly(r(0x200B) + r(0x200D)), chSMS, []string{"sms_body:empty"}},
		{"sms combining mark only", smsOnly(r(0x0301)), chSMS, []string{"sms_body:empty"}},
		{"sms tab", smsOnly("a\tb"), chSMS, []string{"sms_body:invalid_character"}},
		{"sms CR", smsOnly("a\r\nb"), chSMS, []string{"sms_body:invalid_character"}},
		{"sms NUL", smsOnly("a\x00b"), chSMS, []string{"sms_body:invalid_character"}},
		{"sms ESC", smsOnly("a\x1b[31mb"), chSMS, []string{"sms_body:invalid_character"}},
		{"sms DEL", smsOnly("a\x7fb"), chSMS, []string{"sms_body:invalid_character"}},
		{"sms C1 NEL", smsOnly("a" + r(0x85) + "b"), chSMS, []string{"sms_body:invalid_character"}},
		{"sms line separator", smsOnly("a" + r(0x2028) + "b"), chSMS, []string{"sms_body:invalid_character"}},
		{"sms RLO", smsOnly("a" + r(0x202E) + "b"), chSMS, []string{"sms_body:invalid_character"}},
		{"sms isolate", smsOnly("a" + r(0x2067) + "b"), chSMS, []string{"sms_body:invalid_character"}},
		{"sms BOM", smsOnly("a" + r(0xFEFF) + "b"), chSMS, []string{"sms_body:invalid_character"}},
		{"sms noncharacter", smsOnly("a" + r(0xFFFF) + "b"), chSMS, []string{"sms_body:invalid_character"}},
		{"sms invalid utf8", smsOnly("a\xffb"), chSMS, []string{"sms_body:invalid_encoding"}},
		{"sms newline ok", smsOnly("a\nb"), chSMS, nil},
		{"sms emoji ok", smsOnly("Hi 🎉"), chSMS, nil},
		{"sms many errors keep order", smsOnly("a\tb <b> {{x}}"), chSMS,
			[]string{"sms_body:invalid_character", "sms_body:markup_not_allowed", "sms_body:unknown_variable"}},
		{"subject newline", email("a\nb", "c"), chEmail, []string{"email_subject:invalid_character"}},
		{"subject whitespace", email("   ", "c"), chEmail, []string{"email_subject:empty"}},
		{"body whitespace", email("s", "\n\n"), chEmail, []string{"email_body:empty"}},
		{"body newline ok", email("s", "a\n\nb"), chEmail, nil},
		{"body tab", email("s", "a\tb"), chEmail, []string{"email_body:invalid_character"}},
		{"body html", email("s", "<p>Hello</p>"), chEmail, []string{"email_body:markup_not_allowed"}},
		{"body script", email("s", "x<script>alert(1)</script>"), chEmail, []string{"email_body:markup_not_allowed"}},
		{"body comment", email("s", "x<!-- y -->"), chEmail, []string{"email_body:markup_not_allowed"}},
		{"subject markup", email("<b>Sale</b>", "b"), chEmail, []string{"email_subject:markup_not_allowed"}},
		{"plain lt ok", email("I <3 this", "a < b and 3<4"), chEmail, nil},
		{"entity is literal text", email("s", "Fish &amp; chips"), chEmail, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, res := Parse(tc.d, tc.chs)
			if got := errCodes(res.Errors); !slices.Equal(got, tc.want) && !(len(got) == 0 && tc.want == nil) {
				t.Fatalf("errors = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestErrorCap(t *testing.T) {
	_, res := Parse(smsOnly(strings.Repeat("{x} ", 40)), chSMS)
	if len(res.Errors) != maxErrorsPerField+1 || res.Errors[maxErrorsPerField].Code != segment.CodeTooMany {
		t.Fatalf("errors = %v", errCodes(res.Errors))
	}
	// The cap is per field.
	in := strings.Repeat("}", 40)
	_, res = Parse(Draft{SMSBody: sp(in), EmailSubject: sp(in), EmailBody: sp(in)}, chBoth)
	if len(res.Errors) != 3*(maxErrorsPerField+1) {
		t.Fatalf("errors = %d", len(res.Errors))
	}
}

func TestChannelRequirements(t *testing.T) {
	validSMS := sp("Merhaba {{first_name}}")
	cases := []struct {
		name string
		d    Draft
		chs  []segment.Channel
		want []string
		has  []segment.Channel
	}{
		{"sms selected, missing", email("s", "b"), chSMS, []string{"sms_body:required"}, nil},
		{"email selected, missing", Draft{SMSBody: validSMS}, chEmail,
			[]string{"email_subject:required", "email_body:required"}, nil},
		{"email selected, subject missing", Draft{EmailBody: sp("b")}, chEmail, []string{"email_subject:required"}, nil},
		{"email selected, body blank", email("s", " "), chEmail, []string{"email_body:empty"}, nil},
		{"both selected, sms invalid", Draft{SMSBody: sp("{{x}}"), EmailSubject: sp("s"), EmailBody: sp("b")}, chBoth,
			[]string{"sms_body:unknown_variable"}, nil},
		{"both selected, both valid", Draft{SMSBody: validSMS, EmailSubject: sp("s"), EmailBody: sp("b")}, chBoth, nil, chBoth},
		// Unselected but provided content is still validated.
		{"unselected sms invalid", Draft{SMSBody: sp("{{x}}"), EmailSubject: sp("s"), EmailBody: sp("b")}, chEmail,
			[]string{"sms_body:unknown_variable"}, nil},
		{"unselected sms too long", Draft{SMSBody: sp(strings.Repeat("a", 161)), EmailSubject: sp("s"), EmailBody: sp("b")}, chEmail,
			[]string{"sms_body:too_long"}, nil},
		// Unselected and blank counts as absent.
		{"unselected sms blank", Draft{SMSBody: sp("  "), EmailSubject: sp("s"), EmailBody: sp("b")}, chEmail, nil, chEmail},
		// Extra valid content for an unselected channel is kept.
		{"unselected sms valid", Draft{SMSBody: validSMS, EmailSubject: sp("s"), EmailBody: sp("b")}, chEmail, nil, chBoth},
		// The email pair is all-or-nothing even when email is not selected.
		{"unselected partial email", Draft{SMSBody: validSMS, EmailSubject: sp("s")}, chSMS, []string{"email_body:required"}, nil},
		{"unselected partial email blank", Draft{SMSBody: validSMS, EmailSubject: sp("s"), EmailBody: sp("")}, chSMS,
			[]string{"email_body:empty"}, nil},
		{"no channels, nothing", Draft{}, nil, []string{":empty"}, nil},
		{"no channels, blanks", Draft{SMSBody: sp(" "), EmailSubject: sp(""), EmailBody: sp("\n")}, nil, []string{":empty"}, nil},
		{"no channels, sms", Draft{SMSBody: validSMS}, nil, nil, chSMS},
		{"duplicate channels", Draft{SMSBody: validSMS}, []segment.Channel{"sms", "sms"}, nil, chSMS},
		{"unknown channel", Draft{SMSBody: validSMS}, []segment.Channel{"push"}, []string{"channels:not_allowed"}, nil},
		{"all missing, both selected", Draft{}, chBoth,
			[]string{"sms_body:required", "email_subject:required", "email_body:required"}, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c, res := Parse(tc.d, tc.chs)
			if got := errCodes(res.Errors); !slices.Equal(got, tc.want) && !(len(got) == 0 && tc.want == nil) {
				t.Fatalf("errors = %v, want %v", got, tc.want)
			}
			if got := c.Channels(); !slices.Equal(got, tc.has) {
				t.Fatalf("content channels = %v, want %v", got, tc.has)
			}
			if res.OK() != (res.Err() == nil) {
				t.Fatal("OK/Err disagree")
			}
		})
	}
}

func TestWarnings(t *testing.T) {
	_, res := mustParse(t, email("s", "Hello there"), chEmail)
	if got := errCodes(res.Warnings); !slices.Equal(got, []string{"email_body:first_name_not_used"}) {
		t.Fatalf("warnings = %v", got)
	}
	_, res = mustParse(t, email("s", "Hello {{first_name|there}}"), chEmail)
	if len(res.Warnings) != 0 {
		t.Fatalf("warnings = %v", res.Warnings)
	}
	_, res = mustParse(t, smsOnly("Hello"), chSMS) // SMS has no such warning
	if len(res.Warnings) != 0 {
		t.Fatalf("warnings = %v", res.Warnings)
	}
}

// TestResultShape pins the JSON shape and determinism of a failed result.
func TestResultShape(t *testing.T) {
	d := Draft{SMSBody: sp("Hi {{firstname}}"), EmailSubject: sp(strings.Repeat("x", 81))}
	_, res := Parse(d, chBoth)
	got, err := json.Marshal(res)
	if err != nil {
		t.Fatal(err)
	}
	want := `{"errors":[` +
		`{"path":"sms_body","code":"unknown_variable","message":"placeholder at character 4 uses an unknown variable (names are lower-case)","allowed":["first_name","last_name","city"]},` +
		`{"path":"email_subject","code":"too_long","message":"must be at most 80 characters in the worst case after personalization (is 81)"},` +
		`{"path":"email_body","code":"required","message":"is required for the selected channels"}],` +
		`"warnings":[],"subject":{"chars_worst_case":81,"limit":80}}`
	if string(got) != want {
		t.Fatalf("result JSON\n got %s\nwant %s", got, want)
	}
	for range 3 {
		_, again := Parse(d, chBoth)
		b, _ := json.Marshal(again)
		if string(b) != want {
			t.Fatal("non-deterministic result")
		}
	}
	var verr *ValidationError
	if e := res.Err(); e == nil || !errors.As(e, &verr) || len(verr.Problems) != 3 {
		t.Fatalf("Err() = %v", e)
	}

	_, ok := Parse(smsOnly("hi"), chSMS)
	b, _ := json.Marshal(ok)
	if want := `{"errors":[],"warnings":[],"sms":{"chars_worst_case":2,"limit":160},"rendered_example":{"sms":"hi"}}`; string(b) != want {
		t.Fatalf("ok JSON = %s", b)
	}
}

func TestValidateMatchesParse(t *testing.T) {
	d := Draft{SMSBody: sp("Hi {{first_name}}"), EmailSubject: sp("s"), EmailBody: sp("{{x}}")}
	_, pr := Parse(d, chBoth)
	vr := Validate(d, chBoth)
	a, _ := json.Marshal(pr)
	b, _ := json.Marshal(vr)
	if string(a) != string(b) {
		t.Fatalf("Validate != Parse: %s vs %s", b, a)
	}
}

func TestDraftRoundTripAndEqual(t *testing.T) {
	d := Draft{
		SMSBody:      sp("  Merhaba {{first_name|dostum}} " + "I" + r(0x0307) + "stanbul\n"),
		EmailSubject: sp("Konu"),
		EmailBody:    sp("Gövde {{city}}"),
	}
	c, _ := mustParse(t, d, chBoth)
	stored := c.Draft()
	if *stored.SMSBody != "Merhaba {{first_name|dostum}} İstanbul" {
		t.Fatalf("canonical = %q", *stored.SMSBody)
	}
	c2, _ := mustParse(t, stored, chBoth)
	if !c.Equal(c2) || !c2.Equal(c) {
		t.Fatal("round trip not Equal")
	}
	if c3, _ := mustParse(t, Draft{SMSBody: stored.SMSBody}, chSMS); c.Equal(c3) {
		t.Fatal("dropping the email must not be Equal")
	}
	changed := *stored.EmailBody + "!"
	if c4, _ := mustParse(t, Draft{SMSBody: stored.SMSBody, EmailSubject: stored.EmailSubject, EmailBody: &changed}, chBoth); c.Equal(c4) {
		t.Fatal("changed body must not be Equal")
	}
	if !(Content{}).Equal(Content{}) || c.Equal(Content{}) {
		t.Fatal("zero Equal")
	}
}
