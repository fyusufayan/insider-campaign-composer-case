package content

import (
	"slices"
	"strings"
	"testing"
)

// Invisible code points used as fixtures (real code points, built with r()).
var (
	zwsp = r(0x200B) // ZERO WIDTH SPACE
	zwnj = r(0x200C) // ZERO WIDTH NON-JOINER
	zwj  = r(0x200D) // ZERO WIDTH JOINER
	wj   = r(0x2060) // WORD JOINER
	nbsp = r(0x00A0) // NO-BREAK SPACE (trimmed by strings.TrimSpace)
	acut = r(0x0301) // COMBINING ACUTE ACCENT
)

func TestFallbackWithoutVisibleCharactersRejected(t *testing.T) {
	for name, fb := range map[string]string{
		"zwsp":            zwsp,
		"zwsp+zwj":        zwsp + zwj,
		"zwnj":            zwnj,
		"word joiner":     wj,
		"combining only":  acut,
		"zwj run":         zwj + zwj + zwj,
		"nbsp (trimmed)":  nbsp,
		"zwsp around ws":  zwsp + " " + zwsp,
		"zwsp+wj+zwnj":    zwsp + wj + zwnj,
		"combining twice": acut + acut,
	} {
		t.Run(name, func(t *testing.T) {
			_, res := Parse(smsOnly("Merhaba {{first_name|"+fb+"}}"), chSMS)
			if got := errCodes(res.Errors); !slices.Equal(got, []string{"sms_body:invalid_fallback"}) {
				t.Fatalf("errors = %v", got)
			}
		})
	}
}

func TestFallbackWithVisibleCharactersAccepted(t *testing.T) {
	for name, fb := range map[string]string{
		"turkish":            "değerli müşterimiz",
		"turkish capitals":   "İĞÜŞÖÇ",
		"emoji":              "👋",
		"emoji zwj sequence": "👩" + zwj + "💻",
		"text with zwsp":     "dear" + zwsp + "friend",
		"text with zwnj":     "a" + zwnj + "b",
		"decomposed accent":  "e" + acut + "lise",
	} {
		t.Run(name, func(t *testing.T) {
			c, _ := mustParse(t, smsOnly("Merhaba {{first_name|"+fb+"}}"), chSMS)
			out, err := c.RenderSMS(Values{})
			if err != nil || !strings.HasPrefix(out, "Merhaba ") || len(out) <= len("Merhaba ") {
				t.Fatalf("render = %q, %v", out, err)
			}
		})
	}
}

func TestInvisibleValuesAreMissing(t *testing.T) {
	withFallback, _ := mustParse(t, smsOnly("Merhaba {{first_name|dostum}}, {{last_name|Bey}} / {{city|şehrin}}"), chSMS)
	noFallback, _ := mustParse(t, smsOnly("Merhaba {{first_name}}, {{last_name}} / {{city}}"), chSMS)
	ok := Values{FirstName: "Ayşe", LastName: "Yılmaz", City: "İzmir"}

	cases := []struct {
		name    string
		v       Values
		fbOut   string // rendering with fallbacks
		noFbErr string // error without fallbacks ("" = renders fbOut-equivalent)
		noFbOut string
	}{
		{"first_name zwsp", Values{FirstName: zwsp, LastName: "Yılmaz", City: "İzmir"},
			"Merhaba dostum, Yılmaz / İzmir", "sms_body:first_name:missing_value", ""},
		{"first_name zwsp+zwj+wj", Values{FirstName: zwsp + zwj + wj, LastName: "Yılmaz", City: "İzmir"},
			"Merhaba dostum, Yılmaz / İzmir", "sms_body:first_name:missing_value", ""},
		{"first_name spaces and zwsp", Values{FirstName: " " + zwsp + nbsp + " ", LastName: "Yılmaz", City: "İzmir"},
			"Merhaba dostum, Yılmaz / İzmir", "sms_body:first_name:missing_value", ""},
		{"first_name combining only", Values{FirstName: acut, LastName: "Yılmaz", City: "İzmir"},
			"Merhaba dostum, Yılmaz / İzmir", "sms_body:first_name:missing_value", ""},
		{"last_name zwsp", Values{FirstName: "Ayşe", LastName: zwsp, City: "İzmir"},
			"Merhaba Ayşe, Bey / İzmir", "sms_body:last_name:missing_value", ""},
		{"city zwnj", Values{FirstName: "Ayşe", LastName: "Yılmaz", City: zwnj},
			"Merhaba Ayşe, Yılmaz / şehrin", "sms_body:city:missing_value", ""},
		{"city word joiner", Values{FirstName: "Ayşe", LastName: "Yılmaz", City: wj + wj},
			"Merhaba Ayşe, Yılmaz / şehrin", "sms_body:city:missing_value", ""},
		// Visible values, including Turkish letters, emoji and legitimate
		// joiners next to visible text, are used verbatim.
		{"turkish", Values{FirstName: "Çağrı", LastName: "Öztürk", City: "Şanlıurfa"},
			"Merhaba Çağrı, Öztürk / Şanlıurfa", "", "Merhaba Çağrı, Öztürk / Şanlıurfa"},
		{"emoji", Values{FirstName: "Ayşe 🌸", LastName: "Yılmaz", City: "İzmir"},
			"Merhaba Ayşe 🌸, Yılmaz / İzmir", "", "Merhaba Ayşe 🌸, Yılmaz / İzmir"},
		{"emoji zwj sequence", Values{FirstName: "👩" + zwj + "💻", LastName: "Yılmaz", City: "İzmir"},
			"Merhaba 👩" + zwj + "💻, Yılmaz / İzmir", "", "Merhaba 👩" + zwj + "💻, Yılmaz / İzmir"},
		{"visible text with zwnj", Values{FirstName: "Ay" + zwnj + "şe", LastName: "Yıl" + zwsp + "maz", City: "İzmir"},
			"Merhaba Ay" + zwnj + "şe, Yıl" + zwsp + "maz / İzmir", "", "Merhaba Ay" + zwnj + "şe, Yıl" + zwsp + "maz / İzmir"},
		{"decomposed turkish", Values{FirstName: "Ays" + r(0x0327) + "e", LastName: "Yılmaz", City: "I" + r(0x0307) + "zmir"},
			"Merhaba Ayşe, Yılmaz / İzmir", "", "Merhaba Ayşe, Yılmaz / İzmir"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out, err := withFallback.RenderSMS(tc.v)
			checkRender(t, "with fallback", out, err, tc.fbOut, "")
			out, err = noFallback.RenderSMS(tc.v)
			checkRender(t, "without fallback", out, err, tc.noFbOut, tc.noFbErr)
		})
	}
	if out, err := noFallback.RenderSMS(ok); err != nil || out != "Merhaba Ayşe, Yılmaz / İzmir" {
		t.Fatalf("baseline = %q, %v", out, err)
	}
}

// A non-empty value that is invalid is never replaced by the fallback, even
// when it has no visible characters; over-length visible values still fail.
func TestInvalidValuesNotSubstituted(t *testing.T) {
	c, _ := mustParse(t, smsOnly("Merhaba {{first_name|dostum}}"), chSMS)
	for name, tc := range map[string]struct{ val, code string }{
		"zwsp + NUL":       {zwsp + "\x00", RenderInvalidValue},
		"bidi override":    {r(0x202E), RenderInvalidValue},
		"BOM":              {r(0xFEFF), RenderInvalidValue},
		"zwsp + newline":   {zwsp + "\n" + zwsp, RenderInvalidValue},
		"invalid utf8":     {zwsp + "\xff", RenderInvalidValue},
		"too long":         {strings.Repeat("ğ", 21), RenderValueTooLong},
		"too long w/ zwsp": {strings.Repeat("a", 20) + zwsp, RenderValueTooLong},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := c.RenderSMS(Values{FirstName: tc.val})
			checkRender(t, "sms", "", err, "", "sms_body:first_name:"+tc.code)
		})
	}
}

// The visibility rule does not change the worst-case computation: fallbacks
// still count every code point, invisible ones included.
func TestWorstCaseUnchangedByVisibilityRule(t *testing.T) {
	cases := []struct {
		text  string
		worst int
	}{
		{"{{first_name|dear friend}}", 20},
		{"{{first_name|a" + strings.Repeat(zwsp, 25) + "}}", 26},
		{"{{city|👩" + zwj + "💻}}", 14},
		{strings.Repeat("a", 140) + "{{first_name|dostum}}", 160},
	}
	for _, tc := range cases {
		_, res := mustParse(t, smsOnly(tc.text), chSMS)
		if res.SMS.CharsWorstCase != tc.worst {
			t.Errorf("%q: worst = %d, want %d", tc.text, res.SMS.CharsWorstCase, tc.worst)
		}
	}
	_, res := Parse(smsOnly(strings.Repeat("a", 141)+"{{first_name|dostum}}"), chSMS)
	if got := errCodes(res.Errors); !slices.Equal(got, []string{"sms_body:too_long"}) {
		t.Fatalf("161 errors = %v", got)
	}
	_, res = mustParse(t, email(strings.Repeat("x", 60)+"{{first_name|"+zwsp+"a}}", strings.Repeat("y", 4980)+"{{first_name}}"), chEmail)
	if res.Subject.CharsWorstCase != 80 || res.Body.CharsWorstCase != 5000 {
		t.Fatalf("email stats %+v %+v", res.Subject, res.Body)
	}
}
