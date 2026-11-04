package ai

import (
	"context"
	"slices"
	"strings"
	"testing"

	"github.com/fyusufayan/insider-campaign-composer-case/backend/internal/llm"
	"github.com/fyusufayan/insider-campaign-composer-case/backend/internal/segment"
)

func demoGenerator(t *testing.T) *Generator {
	t.Helper()
	return newGen(t, llm.NewDemo(DemoResponders()))
}

// Every combination of goal, tone, length, offer and channel selection
// yields valid content for exactly the selected channels in one call,
// mentioning exactly the authorized offer.
func TestDemoContentAllCombinationsValid(t *testing.T) {
	g := demoGenerator(t)
	goals := []string{"cart_recovery", "win_back", "promotion", "reactivation", "x"}
	tones := []string{"friendly", "urgent", "formal", "playful"}
	offers := []Offer{noOff, fship, {Kind: OfferPercentDiscount, Value: 1}, pct30, {Kind: OfferPercentDiscount, Value: 100}}
	n := 0
	for _, goal := range goals {
		for _, tone := range tones {
			for _, length := range []Length{LengthShort, LengthMedium, LengthLong} {
				for _, o := range offers {
					for _, chs := range [][]segment.Channel{sms, email, both} {
						in := GenerationInput{Brief: ContentBrief{Goal: goal, Tone: tone, Length: length, Offer: o}, Rule: testRule(t), Channels: chs}
						gen, err := g.Generate(context.Background(), in)
						if err != nil {
							t.Fatalf("%+v %v: %v", in.Brief, chs, err)
						}
						n++
						if !slices.Equal(gen.Content.Channels(), chs) || gen.Meta.Calls != 1 || gen.Meta.Repaired ||
							gen.Meta.Model != llm.DemoModel || !slices.Equal(gen.Notes, []string{DemoContentNotice}) {
							t.Fatalf("%+v %v: gen = %+v", in.Brief, chs, gen)
						}
						for _, w := range gen.Validation.Warnings {
							t.Errorf("%+v %v: warning %+v", in.Brief, chs, w)
						}
						if r := gen.Validation.SMS; r != nil && r.CharsWorstCase > targetSMSRunes {
							t.Errorf("sms worst case %d exceeds the %d target", r.CharsWorstCase, targetSMSRunes)
						}
						if r := gen.Validation.Subject; r != nil && r.CharsWorstCase > targetEmailSubjectRunes {
							t.Errorf("subject worst case %d exceeds the %d target", r.CharsWorstCase, targetEmailSubjectRunes)
						}
						if body, ok := gen.Content.EmailBody(); ok && (length == LengthLong) != strings.Contains(body.String(), "{{city|") {
							t.Errorf("%s email body length handling: %q", length, body.String())
						}
					}
				}
			}
		}
	}
	if n != 5*4*3*5*3 {
		t.Errorf("combinations = %d", n)
	}
}

// The offer is rendered from the validated kind/value only.
func TestDemoContentOffers(t *testing.T) {
	g := demoGenerator(t)
	check := func(o Offer, want, forbid []string) {
		t.Helper()
		gen, err := g.Generate(context.Background(), genInput(t, both, o))
		if err != nil {
			t.Fatal(err)
		}
		d := gen.Content.Draft()
		all := strings.ToLower(*d.SMSBody + " " + *d.EmailSubject + " " + *d.EmailBody)
		for _, w := range want {
			if !strings.Contains(all, w) {
				t.Errorf("%+v: missing %q in %q", o, w, all)
			}
		}
		for _, f := range forbid {
			if strings.Contains(all, f) {
				t.Errorf("%+v: unexpected %q in %q", o, f, all)
			}
		}
	}
	check(pct30, []string{"30% off"}, []string{"free shipping"})
	check(fship, []string{"free shipping"}, []string{"%", "discount"})
	check(noOff, nil, []string{"%", "free shipping", "discount", "off your"})
}

// Brief strings select templates but are never copied into the output, so
// marketer/model text can never become template syntax.
func TestDemoContentNeverCopiesBriefText(t *testing.T) {
	in := genInput(t, both, noOff)
	in.Brief.Goal = "cart_{{email}}_recovery"
	in.Brief.Tone = "urgent <b>{{last_name|x}}</b>"
	gen, err := demoGenerator(t).Generate(context.Background(), in)
	if err != nil {
		t.Fatal(err)
	}
	d := gen.Content.Draft()
	all := *d.SMSBody + *d.EmailSubject + *d.EmailBody
	for _, s := range []string{"{{email}}", "<b>", "last_name", "_recovery"} {
		if strings.Contains(all, s) {
			t.Errorf("brief text %q copied into %q", s, all)
		}
	}
	if !strings.Contains(*d.SMSBody, "now") { // urgent voice selected
		t.Errorf("tone not applied: %q", *d.SMSBody)
	}
}

func TestDemoContentDeterministic(t *testing.T) {
	g := demoGenerator(t)
	a, err := g.Generate(context.Background(), genInput(t, both, pct30))
	if err != nil {
		t.Fatal(err)
	}
	b, err := g.Generate(context.Background(), genInput(t, both, pct30))
	if err != nil {
		t.Fatal(err)
	}
	if !a.Content.Equal(b.Content) {
		t.Error("demo output is not deterministic")
	}
}

// The demo responder accepts only a well-formed request document.
func TestDemoContentRejectsMalformedRequest(t *testing.T) {
	for _, msg := range []string{
		"no request here",
		requestOpen + "\n{\"channels\":[\"sms\"]}\n" + requestClose,
		requestOpen + "\n{\"channels\":[\"push\"],\"fields\":[],\"content_brief\":{},\"audience_rule\":{}}\n" + requestClose,
	} {
		_, err := demoContentResponder(llm.Request{Task: TaskContentGeneration, Messages: []llm.Message{{Role: llm.RoleUser, Content: msg}}})
		if err == nil {
			t.Errorf("accepted %q", msg)
		}
	}
}

// Both demo tasks are registered; the segment responder is unchanged.
func TestDemoRespondersTasks(t *testing.T) {
	got := llm.NewDemo(DemoResponders()).Tasks()
	if !slices.Equal(got, []string{TaskContentGeneration, TaskSegmentInterpretation}) {
		t.Errorf("tasks = %v", got)
	}
}
