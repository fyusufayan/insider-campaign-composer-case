package content

import (
	"errors"
	"testing"
)

func TestProvenanceValidate(t *testing.T) {
	cases := []struct {
		p  Provenance
		ok bool
	}{
		{Provenance{Origin: OriginAI, Model: "claude-opus-5-5", PromptVersion: "content_v1"}, true},
		{Provenance{Origin: OriginAI, Model: "m"}, false},
		{Provenance{Origin: OriginAI, PromptVersion: "v"}, false},
		{Provenance{Origin: OriginEdited}, true},
		{Provenance{Origin: OriginEdited, Model: "m"}, false},
		{Provenance{Origin: OriginEdited, PromptVersion: "v"}, false},
		{Provenance{Origin: "imported"}, false},
		{Provenance{}, false},
	}
	for _, tc := range cases {
		err := tc.p.Validate()
		if (err == nil) != tc.ok || (err != nil && !errors.Is(err, ErrInvalidProvenance)) {
			t.Errorf("%+v: err = %v", tc.p, err)
		}
	}
}

func TestNextVersion(t *testing.T) {
	ai := Provenance{Origin: OriginAI, Model: "m", PromptVersion: "content_v1"}
	edited := Provenance{Origin: OriginEdited}
	a, _ := mustParse(t, smsOnly("Merhaba {{first_name}}"), chSMS)
	aSame, _ := mustParse(t, smsOnly("  Merhaba {{first_name}}\n"), chSMS) // same canonical text
	b, _ := mustParse(t, smsOnly("Merhaba {{first_name}}!"), chSMS)
	aPlusEmail, _ := mustParse(t, Draft{SMSBody: sp("Merhaba {{first_name}}"), EmailSubject: sp("s"), EmailBody: sp("b")}, chSMS)

	v1, created, err := NextVersion(nil, a, ai)
	if err != nil || !created || v1.Number != 1 || v1.Provenance != ai || !v1.Content.Equal(a) {
		t.Fatalf("first = %+v %v %v", v1, created, err)
	}
	v1.ID = 10

	cases := []struct {
		name    string
		c       Content
		p       Provenance
		created bool
		number  int
	}{
		{"edit identical text is a no-op", aSame, edited, false, 1},
		{"edit changed text", b, edited, true, 2},
		{"edit adds a channel", aPlusEmail, edited, true, 2},
		{"ai identical text still versions", aSame, ai, true, 2},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			v, created, err := NextVersion(&v1, tc.c, tc.p)
			if err != nil || created != tc.created || v.Number != tc.number {
				t.Fatalf("got %d %v %v", v.Number, created, err)
			}
			if !created && v.ID != v1.ID {
				t.Fatal("no-op must return the current version")
			}
			if created && (v.ID != 0 || !v.CreatedAt.IsZero()) {
				t.Fatal("new version must not carry database fields")
			}
		})
	}

	if _, _, err := NextVersion(&v1, Content{}, edited); !errors.Is(err, ErrEmptyContent) {
		t.Fatalf("empty content: %v", err)
	}
	if _, _, err := NextVersion(&v1, b, Provenance{Origin: OriginEdited, Model: "m"}); !errors.Is(err, ErrInvalidProvenance) {
		t.Fatalf("bad provenance: %v", err)
	}
}
