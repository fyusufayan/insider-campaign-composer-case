package content

import (
	"errors"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/fyusufayan/insider-campaign-composer-case/backend/internal/segment"
)

// maxValues are values at exactly the declared maxima (2-byte runes).
func maxValues() Values {
	return Values{
		FirstName: strings.Repeat("Ğ", MaxFirstNameRunes),
		LastName:  strings.Repeat("Ş", MaxLastNameRunes),
		City:      strings.Repeat("İ", segment.Cities().MaxNameRunes()),
	}
}

func TestRenderSubstitutionAndFallback(t *testing.T) {
	c, _ := mustParse(t, Draft{
		SMSBody:      sp("Merhaba {{first_name|dostum}} {{last_name}}, {{city}} için %30!"),
		EmailSubject: sp("{{first_name}} için"),
		EmailBody:    sp("Merhaba {{first_name|değerli müşterimiz}},\n{{city|şehrin}}"),
	}, chBoth)

	cases := []struct {
		name    string
		v       Values
		sms     string
		smsErr  string
		subject string
		body    string
		mailErr string
	}{
		{name: "all values", v: Values{FirstName: "Ayşe", LastName: "Yılmaz", City: "İzmir"},
			sms: "Merhaba Ayşe Yılmaz, İzmir için %30!", subject: "Ayşe için", body: "Merhaba Ayşe,\nİzmir"},
		{name: "missing first name uses fallback", v: Values{LastName: "Yılmaz", City: "İzmir"},
			sms: "Merhaba dostum Yılmaz, İzmir için %30!", mailErr: "email_subject:first_name:missing_value"},
		{name: "whitespace value counts as missing", v: Values{FirstName: "  ", LastName: "Kaya", City: "Van"},
			sms: "Merhaba dostum Kaya, Van için %30!", mailErr: "email_subject:first_name:missing_value"},
		{name: "missing without fallback", v: Values{FirstName: "Ali", City: "Van"},
			smsErr: "sms_body:last_name:missing_value", subject: "Ali için", body: "Merhaba Ali,\nVan"},
		{name: "missing city with fallback in email", v: Values{FirstName: "Ali", LastName: "Kaya"},
			smsErr: "sms_body:city:missing_value", subject: "Ali için", body: "Merhaba Ali,\nşehrin"},
		{name: "values trimmed and NFC", v: Values{FirstName: " Ays" + r(0x0327) + "e ", LastName: "K", City: "V"},
			sms: "Merhaba Ayşe K, V için %30!", subject: "Ayşe için", body: "Merhaba Ayşe,\nV"},
		{name: "value too long is not truncated", v: Values{FirstName: strings.Repeat("a", 21), LastName: "K", City: "V"},
			smsErr: "sms_body:first_name:value_too_long", mailErr: "email_subject:first_name:value_too_long"},
		{name: "too-long value does not fall back", v: Values{FirstName: "A", LastName: "K", City: strings.Repeat("c", 15)},
			smsErr: "sms_body:city:value_too_long", mailErr: "email_body:city:value_too_long"},
		{name: "value with newline", v: Values{FirstName: "A\nBcc: x", LastName: "K", City: "V"},
			smsErr: "sms_body:first_name:invalid_value", mailErr: "email_subject:first_name:invalid_value"},
		{name: "value with control", v: Values{FirstName: "A\x00", LastName: "K", City: "V"},
			smsErr: "sms_body:first_name:invalid_value", mailErr: "email_subject:first_name:invalid_value"},
		{name: "value invalid utf8", v: Values{FirstName: "A\xff", LastName: "K", City: "V"},
			smsErr: "sms_body:first_name:invalid_value", mailErr: "email_subject:first_name:invalid_value"},
		// Values are substituted literally and never re-parsed or evaluated.
		{name: "placeholder-looking value is literal", v: Values{FirstName: "{{city}}", LastName: "{{.X}}", City: "<b>"},
			sms: "Merhaba {{city}} {{.X}}, <b> için %30!", subject: "{{city}} için", body: "Merhaba {{city}},\n<b>"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			sms, err := c.RenderSMS(tc.v)
			checkRender(t, "sms", sms, err, tc.sms, tc.smsErr)
			e, err := c.RenderEmail(tc.v)
			if tc.mailErr == "" && tc.subject == "" {
				t.Fatal("bad case")
			}
			checkRender(t, "email", e.Subject+"|"+e.Body, err, tc.subject+"|"+tc.body, tc.mailErr)
		})
	}
}

func checkRender(t *testing.T, what, got string, err error, want, wantErr string) {
	t.Helper()
	if wantErr != "" {
		var re *RenderError
		if !errors.As(err, &re) || !errors.Is(err, ErrRender) {
			t.Fatalf("%s: err = %v, want %s", what, err, wantErr)
		}
		if g := re.Field + ":" + string(re.Variable) + ":" + re.Code; g != wantErr {
			t.Fatalf("%s: err = %s, want %s", what, g, wantErr)
		}
		return
	}
	if err != nil || got != want {
		t.Fatalf("%s = %q, %v; want %q", what, got, err, want)
	}
}

func TestRenderErrorDoesNotEchoValue(t *testing.T) {
	c, _ := mustParse(t, smsOnly("Hi {{first_name}}"), chSMS)
	_, err := c.RenderSMS(Values{FirstName: "SECRET\nVALUE"})
	if err == nil || strings.Contains(err.Error(), "SECRET") {
		t.Fatalf("err = %v", err)
	}
}

func TestRenderWorstCaseFitsLimits(t *testing.T) {
	sms := strings.Repeat("a", 160-20-25-14) + "{{first_name}}{{last_name}}{{city}}"
	subject := strings.Repeat("b", 80-20-25-14) + "{{first_name}}{{last_name}}{{city}}"
	body := strings.Repeat("c", 5000-3*20) + "{{first_name}}{{first_name}}{{first_name}}"
	c, res := mustParse(t, Draft{SMSBody: &sms, EmailSubject: &subject, EmailBody: &body}, chBoth)
	if res.SMS.CharsWorstCase != 160 || res.Subject.CharsWorstCase != 80 || res.Body.CharsWorstCase != 5000 {
		t.Fatalf("stats %+v %+v %+v", res.SMS, res.Subject, res.Body)
	}
	out, err := c.RenderSMS(maxValues())
	if err != nil || utf8.RuneCountInString(out) != 160 {
		t.Fatalf("sms %d runes, %v", utf8.RuneCountInString(out), err)
	}
	e, err := c.RenderEmail(maxValues())
	if err != nil || utf8.RuneCountInString(e.Subject) != 80 || utf8.RuneCountInString(e.Body) != 5000 {
		t.Fatalf("email %v", err)
	}
	// The footer is outside the body budget.
	if utf8.RuneCountInString(e.Text()) <= 5000 {
		t.Fatal("footer missing from the sent text")
	}
}

func TestRenderContentTooLongAssertion(t *testing.T) {
	// Unreachable through Parse; exercise the render-time assertion (§8.7)
	// directly with a template that bypassed validation.
	rep := reporter{}
	tmpl, ok := parseTemplate(strings.Repeat("a", 161), &rep)
	if !ok {
		t.Fatal("parse")
	}
	_, err := renderField(tmpl, specSMS, Values{})
	var re *RenderError
	if !errors.As(err, &re) || re.Code != RenderContentTooLong || re.Field != FieldSMSBody {
		t.Fatalf("err = %v", err)
	}
}

func TestRenderChannelAbsent(t *testing.T) {
	c, _ := mustParse(t, smsOnly("hi"), chSMS)
	if _, err := c.RenderEmail(ExampleValues()); !errors.Is(err, ErrChannelAbsent) {
		t.Fatalf("err = %v", err)
	}
	var zero Content
	if _, err := zero.RenderSMS(ExampleValues()); !errors.Is(err, ErrChannelAbsent) {
		t.Fatalf("err = %v", err)
	}
}

func TestRenderDeterministic(t *testing.T) {
	c, _ := mustParse(t, Draft{SMSBody: sp("{{first_name}} {{city|x}}")}, chSMS)
	v := Values{FirstName: "Çağrı", City: "Muğla"}
	first, _ := c.RenderSMS(v)
	for range 100 {
		if s, _ := c.RenderSMS(v); s != first {
			t.Fatal("non-deterministic rendering")
		}
	}
}
