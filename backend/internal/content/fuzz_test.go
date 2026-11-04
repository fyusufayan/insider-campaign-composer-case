package content

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/fyusufayan/insider-campaign-composer-case/backend/internal/segment"
)

var templateSeeds = []string{
	"", " ", "hi", "{{first_name}}", "{{first_name|there}}", "{{city}}{{last_name}}",
	"{{", "}}", "{", "}", "{{}}", "{{{first_name}}}", "{{first_name}}}", "{{a{{b}}}}", "{{first_name|a|b}}",
	"{{ first_name }}", "{{.X}}", "{{if .X}}y{{end}}", "{{first_name|}}", "{{first_name| x}}",
	"<b>x</b>", "a < b", "I\u0307stanbul", "Merhaba {{first_name}}, %30 indirim!", "a\nb", "a\tb", "a\rb",
	"\xff\xfe", "\u202e", "\ufeff", "👩\u200d👩\u200d👧", strings.Repeat("ş", 161),
	strings.Repeat("{x}", 20), "{{first_name|\u0301}}", "x{{city|\u00a0y}}",
}

var knownPaths = map[string]bool{"": true, "channels": true, FieldSMSBody: true, FieldEmailSubject: true, FieldEmailBody: true}

// FuzzParse: arbitrary text never panics; failures yield zero content and
// well-formed errors; accepted content is a fixed point, honours the hard
// limits, and renders within the limits for any values at the declared maxima.
func FuzzParse(f *testing.F) {
	for _, s := range templateSeeds {
		f.Add(s, "Konu {{first_name}}", s)
		f.Add("Merhaba {{first_name}}", s, "Gövde")
	}
	f.Fuzz(func(t *testing.T, sms, subject, body string) {
		d := Draft{SMSBody: &sms, EmailSubject: &subject, EmailBody: &body}
		c, res := Parse(d, chBoth)
		if res.Errors == nil || res.Warnings == nil {
			t.Fatal("nil error/warning slices")
		}
		if !res.OK() {
			if !c.IsZero() || res.RenderedExample != nil {
				t.Fatal("partial content on error")
			}
			for _, e := range res.Errors {
				if !knownPaths[e.Path] || e.Code == "" || e.Message == "" {
					t.Fatalf("malformed error %+v", e)
				}
			}
			if len(res.Errors) > 3*(maxErrorsPerField+1) {
				t.Fatalf("error cap exceeded: %d", len(res.Errors))
			}
			return
		}
		checkAccepted(t, c, res)
	})
}

func checkAccepted(t *testing.T, c Content, res Result) {
	t.Helper()
	for _, tc := range []struct {
		get   func() (Template, bool)
		limit int
	}{{c.SMSBody, MaxSMSRunes}, {c.EmailSubject, MaxEmailSubjectRunes}, {c.EmailBody, MaxEmailBodyRunes}} {
		if tm, ok := tc.get(); ok {
			if tm.WorstCaseRunes() > tc.limit || !utf8.ValidString(tm.String()) {
				t.Fatalf("accepted template over limit or invalid")
			}
			for _, p := range tm.Placeholders() {
				if _, ok := LookupVariable(string(p.Variable)); !ok {
					t.Fatalf("unapproved variable %q accepted", p.Variable)
				}
			}
		}
	}
	// Fixed point: the canonical draft parses to equal content and the same result.
	c2, res2 := Parse(c.Draft(), c.Channels())
	if !c2.Equal(c) {
		t.Fatal("canonical form is not a fixed point")
	}
	a, _ := json.Marshal(res)
	b, _ := json.Marshal(res2)
	if string(a) != string(b) && len(c.Channels()) == 2 {
		t.Fatalf("result differs on re-parse:\n%s\n%s", a, b)
	}
	// Worst-case values always render within limits.
	for _, v := range []Values{maxValues(), ExampleValues(), {}} {
		if s, err := c.RenderSMS(v); err == nil {
			if utf8.RuneCountInString(s) > MaxSMSRunes {
				t.Fatal("rendered sms over limit")
			}
		} else if !errors.Is(err, ErrRender) && !errors.Is(err, ErrChannelAbsent) {
			t.Fatalf("unexpected render error %v", err)
		}
		if e, err := c.RenderEmail(v); err == nil {
			if utf8.RuneCountInString(e.Subject) > MaxEmailSubjectRunes || utf8.RuneCountInString(e.Body) > MaxEmailBodyRunes {
				t.Fatal("rendered email over limit")
			}
		} else if !errors.Is(err, ErrRender) && !errors.Is(err, ErrChannelAbsent) {
			t.Fatalf("unexpected render error %v", err)
		}
	}
	if _, err := c.RenderSMS(maxValues()); c.Has(segment.ChannelSMS) && err != nil {
		t.Fatalf("max values must render: %v", err)
	}
}

// FuzzRender: arbitrary values never panic, never break the hard limits, and
// a successful rendering of a single-line field never contains a line break
// or control character introduced by a value.
func FuzzRender(f *testing.F) {
	f.Add("Hi {{first_name|there}} from {{city}}", "Ayşe", "Yılmaz", "İzmir")
	f.Add("{{last_name}}", "", "{{city}}", "")
	f.Add("{{first_name}}", "a\nb", "x", "y")
	f.Add("{{first_name}}", "\xff", "x", "y")
	f.Fuzz(func(t *testing.T, tmpl, first, last, city string) {
		c, res := Parse(Draft{EmailSubject: &tmpl, EmailBody: &tmpl}, chEmail)
		if !res.OK() {
			return
		}
		e, err := c.RenderEmail(Values{FirstName: first, LastName: last, City: city})
		if err != nil {
			var re *RenderError
			if !errors.As(err, &re) {
				t.Fatalf("unexpected error type %T", err)
			}
			return
		}
		if utf8.RuneCountInString(e.Subject) > MaxEmailSubjectRunes || !utf8.ValidString(e.Subject) {
			t.Fatal("subject over limit or invalid")
		}
		if strings.ContainsFunc(e.Subject, func(r rune) bool { return forbiddenRune(r, false) != "" }) {
			t.Fatal("subject contains a forbidden character")
		}
	})
}
