package campaign

import (
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/fyusufayan/insider-campaign-composer-case/backend/internal/ai"
	"github.com/fyusufayan/insider-campaign-composer-case/backend/internal/segment"
)

const (
	istanbulOutput = `{"segment":{"match":"all","conditions":[{"type":"attribute","field":"city","op":"eq","value":"Istanbul"},{"type":"behavior","predicate":"abandoned_cart","within_days":30}]},` +
		`"interpretation":"Users in Istanbul with an abandoned cart.","assumptions":["a"],"unsupported":[],` +
		`"suggested_channels":["email","sms"],` +
		`"content_brief":{"goal":"cart_recovery","tone":"friendly","length":"short","offer":{"kind":"percent_discount","value":30}}}`
	// Same rule written differently (case, spelling): normalizes identically.
	istanbulOutputRespelled = `{"segment":{"match":"all","conditions":[{"type":"attribute","field":"city","op":"eq","value":"İSTANBUL"},{"type":"behavior","predicate":"abandoned_cart","within_days":30}]},` +
		`"interpretation":"Same audience, other words.","assumptions":[],"unsupported":["b"],` +
		`"suggested_channels":["sms"],` +
		`"content_brief":{"goal":"cart_recovery","tone":"warm","length":"medium","offer":{"kind":"none"}}}`
	ankaraOutput = `{"segment":{"match":"all","conditions":[{"type":"attribute","field":"city","op":"eq","value":"Ankara"}]},` +
		`"interpretation":"Users in Ankara.","assumptions":[],"unsupported":[],` +
		`"suggested_channels":["sms"],` +
		`"content_brief":{"goal":"promo","tone":"formal","length":"long","offer":{"kind":"free_shipping"}}}`
)

func interp(t *testing.T, out string) ai.Interpretation {
	t.Helper()
	in, errs := ai.ParseInterpretation([]byte(out))
	if len(errs) > 0 {
		t.Fatalf("ParseInterpretation: %v", errs)
	}
	in.Meta = ai.Metadata{Model: "m", PromptVersion: ai.SegmentPromptVersion, Calls: 1}
	return in
}

func draft() Campaign {
	return Campaign{ID: ID{1}, Name: "n", Brief: "a brief of enough length", Status: StatusDraft,
		Channels: []segment.Channel{}, Version: 1}
}

func ruleJSON(t *testing.T, c Campaign) string {
	t.Helper()
	b, err := json.Marshal(c.Segment)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestParseID(t *testing.T) {
	good := []string{
		"0b8f3e2a-1c4d-4e5f-8a9b-0c1d2e3f4a5b",
		"0B8F3E2A-1C4D-4E5F-8A9B-0C1D2E3F4A5B",
		"00000000-0000-0000-0000-000000000000",
	}
	for _, s := range good {
		id, err := ParseID(s)
		if err != nil {
			t.Errorf("ParseID(%q): %v", s, err)
			continue
		}
		if id.String() != strings.ToLower(s) {
			t.Errorf("String() = %q, want %q", id.String(), strings.ToLower(s))
		}
	}
	bad := []string{
		"", "nope", "0b8f3e2a1c4d4e5f8a9b0c1d2e3f4a5b", "{0b8f3e2a-1c4d-4e5f-8a9b-0c1d2e3f4a5b}",
		"0b8f3e2a-1c4d-4e5f-8a9b-0c1d2e3f4a5", "0b8f3e2a-1c4d-4e5f-8a9b-0c1d2e3f4a5bb",
		"0b8f3e2g-1c4d-4e5f-8a9b-0c1d2e3f4a5b", "0b8f3e2a_1c4d-4e5f-8a9b-0c1d2e3f4a5b",
		"' OR 1=1 --", "0b8f3e2a-1c4d-4e5f-8a9b-0c1d2e3f4a5é",
	}
	for _, s := range bad {
		if _, err := ParseID(s); !errors.Is(err, ErrInvalidID) {
			t.Errorf("ParseID(%q) err = %v, want ErrInvalidID", s, err)
		}
	}
}

func TestNewDraft(t *testing.T) {
	long := func(n int) string { return strings.Repeat("ş", n) }
	tests := []struct {
		name, n, b string
		wantErrs   []string // "path:code"
		wantName   string
		wantBrief  string
	}{
		{"ok, trimmed and NFC", "  Spring sale ", "  Users in İstanbul who shop.  ", nil, "Spring sale", "Users in İstanbul who shop."},
		{"name max", long(120), long(10), nil, long(120), long(10)},
		{"brief max with newlines", "n", long(1998) + "\n\t", nil, "n", long(1998)},
		{"empty name", "   ", "a valid brief text", []string{"name:empty"}, "", ""},
		{"name too long", long(121), "a valid brief text", []string{"name:too_long"}, "", ""},
		{"name with newline", "a\nb", "a valid brief text", []string{"name:invalid_value"}, "", ""},
		{"brief too short", "n", "short", []string{"brief:too_short"}, "", ""},
		{"brief too long", "n", long(2001), []string{"brief:too_long"}, "", ""},
		{"brief control char", "n", "a valid brief\u0000text", []string{"brief:invalid_value"}, "", ""},
		{"both", "", "", []string{"name:empty", "brief:empty"}, "", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			d, err := NewDraft(tt.n, tt.b)
			if tt.wantErrs == nil {
				if err != nil {
					t.Fatalf("err = %v", err)
				}
				if d.Name != tt.wantName || d.Brief != tt.wantBrief {
					t.Errorf("draft = %q / %q", d.Name, d.Brief)
				}
				return
			}
			var ve *ValidationError
			if !errors.As(err, &ve) || !errors.Is(err, ErrInvalidInput) {
				t.Fatalf("err = %v, want *ValidationError", err)
			}
			var got []string
			for _, p := range ve.Problems {
				got = append(got, p.Path+":"+p.Code)
				if strings.Contains(p.Message, tt.b) && tt.b != "" {
					t.Errorf("message echoes input: %q", p.Message)
				}
			}
			if !slices.Equal(got, tt.wantErrs) {
				t.Errorf("problems = %v, want %v", got, tt.wantErrs)
			}
		})
	}
}

func TestStateChecks(t *testing.T) {
	rule := interp(t, istanbulOutput).Segment
	type allowed struct{ interpret, channels, audience bool }
	want := map[Status]allowed{
		StatusDraft:                 {true, false, false},
		StatusSegmented:             {true, true, true},
		StatusContentReady:          {true, true, true},
		StatusPendingApproval:       {false, false, true},
		StatusApproved:              {false, false, true},
		StatusSending:               {false, false, false},
		StatusCompleted:             {false, false, false},
		StatusCompletedWithFailures: {false, false, false},
		StatusCancelled:             {false, false, false},
	}
	if len(want) != len(allStatuses) {
		t.Fatal("table does not cover every status")
	}
	for st, w := range want {
		c := Campaign{Status: st}
		if st != StatusDraft {
			c.Segment = rule
		}
		check := func(op string, err error, ok bool) {
			if ok && err != nil {
				t.Errorf("%s in %s: unexpected %v", op, st, err)
			}
			if !ok {
				var se *StateError
				if !errors.As(err, &se) || !errors.Is(err, ErrInvalidState) || se.Status != st || se.Reason == "" {
					t.Errorf("%s in %s: err = %v, want *StateError", op, st, err)
				}
			}
		}
		check("interpret", c.CheckInterpret(), w.interpret)
		check("channels", c.CheckSetChannels(), w.channels)
		check("audience", c.CheckAudience(), w.audience)
	}
}

func TestWithInterpretationFirst(t *testing.T) {
	c := draft()
	next, err := c.WithInterpretation(1, interp(t, istanbulOutput))
	if err != nil {
		t.Fatal(err)
	}
	if next.Status != StatusSegmented || next.Version != 2 || next.SegmentVersion != 1 {
		t.Errorf("status/version/segment_version = %s/%d/%d", next.Status, next.Version, next.SegmentVersion)
	}
	// Suggested ["email","sms"] stored in canonical order.
	if !slices.Equal(next.Channels, []segment.Channel{segment.ChannelSMS, segment.ChannelEmail}) {
		t.Errorf("channels = %v", next.Channels)
	}
	if got := ruleJSON(t, next); !strings.Contains(got, `"value":"istanbul"`) || !strings.HasPrefix(got, `{"version":1`) {
		t.Errorf("rule not normalized: %s", got)
	}
	if next.Interpretation == nil || next.Interpretation.Summary != "Users in Istanbul with an abandoned cart." ||
		next.Interpretation.Model != "m" || next.Interpretation.PromptVersion != ai.SegmentPromptVersion {
		t.Errorf("interpretation = %+v", next.Interpretation)
	}
	if string(next.ContentBrief) != `{"goal":"cart_recovery","tone":"friendly","length":"short","offer":{"kind":"percent_discount","value":30}}` {
		t.Errorf("content brief = %s", next.ContentBrief)
	}
	// The receiver is not modified.
	if c.Version != 1 || len(c.Channels) != 0 || !c.Segment.IsZero() {
		t.Errorf("receiver mutated: %+v", c)
	}
}

func TestWithInterpretationNeverOverwritesChannelsAfterFirst(t *testing.T) {
	first, err := draft().WithInterpretation(1, interp(t, istanbulOutput))
	if err != nil {
		t.Fatal(err)
	}
	// The marketer explicitly selects no channel.
	empty, changed, err := first.WithChannels(first.Version, []segment.Channel{})
	if err != nil || !changed {
		t.Fatalf("WithChannels: changed=%t err=%v", changed, err)
	}
	again, err := empty.WithInterpretation(empty.Version, interp(t, ankaraOutput))
	if err != nil {
		t.Fatal(err)
	}
	if len(again.Channels) != 0 {
		t.Errorf("explicit empty selection overwritten: %v", again.Channels)
	}
	// A non-empty selection is kept as well.
	emailOnly, _, err := again.WithChannels(again.Version, []segment.Channel{segment.ChannelEmail})
	if err != nil {
		t.Fatal(err)
	}
	third, err := emailOnly.WithInterpretation(emailOnly.Version, interp(t, istanbulOutput))
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(third.Channels, []segment.Channel{segment.ChannelEmail}) {
		t.Errorf("channels = %v, want [email]", third.Channels)
	}
}

func TestWithInterpretationSegmentVersion(t *testing.T) {
	first, err := draft().WithInterpretation(1, interp(t, istanbulOutput))
	if err != nil {
		t.Fatal(err)
	}
	same, err := first.WithInterpretation(first.Version, interp(t, istanbulOutputRespelled))
	if err != nil {
		t.Fatal(err)
	}
	if same.SegmentVersion != 1 || same.Version != 3 {
		t.Errorf("identical normalized rule: segment_version=%d version=%d, want 1/3", same.SegmentVersion, same.Version)
	}
	if same.Interpretation.Summary != "Same audience, other words." {
		t.Errorf("interpretation not replaced: %+v", same.Interpretation)
	}
	changed, err := same.WithInterpretation(same.Version, interp(t, ankaraOutput))
	if err != nil {
		t.Fatal(err)
	}
	if changed.SegmentVersion != 2 || changed.Version != 4 || changed.Status != StatusSegmented {
		t.Errorf("changed rule: segment_version=%d version=%d status=%s", changed.SegmentVersion, changed.Version, changed.Status)
	}
}

func TestWithInterpretationStaleOrInvalidState(t *testing.T) {
	c := draft()
	c.Version = 5
	_, err := c.WithInterpretation(4, interp(t, istanbulOutput))
	var ce *ConflictError
	if !errors.As(err, &ce) || !errors.Is(err, ErrVersionConflict) || ce.Expected != 4 || ce.Current != 5 {
		t.Errorf("err = %v, want conflict 4/5", err)
	}
	for _, st := range []Status{StatusPendingApproval, StatusApproved, StatusSending, StatusCancelled, StatusCompleted} {
		c := draft()
		c.Status = st
		if _, err := c.WithInterpretation(1, interp(t, istanbulOutput)); !errors.Is(err, ErrInvalidState) {
			t.Errorf("%s: err = %v, want invalid state", st, err)
		}
	}
}

func TestWithInterpretationKeepsContentReady(t *testing.T) {
	first, err := draft().WithInterpretation(1, interp(t, istanbulOutput))
	if err != nil {
		t.Fatal(err)
	}
	contentID := int64(7)
	first.CurrentContentID = &contentID
	first.Status = StatusContentReady
	next, err := first.WithInterpretation(first.Version, interp(t, ankaraOutput))
	if err != nil {
		t.Fatal(err)
	}
	if next.Status != StatusContentReady {
		t.Errorf("status = %s, want content_ready", next.Status)
	}
}

func TestWithChannels(t *testing.T) {
	seg, err := draft().WithInterpretation(1, interp(t, ankaraOutput)) // channels [sms]
	if err != nil {
		t.Fatal(err)
	}

	if _, _, err := seg.WithChannels(seg.Version-1, nil); !errors.Is(err, ErrVersionConflict) {
		t.Errorf("stale: err = %v", err)
	}
	if _, _, err := draft().WithChannels(1, nil); !errors.Is(err, ErrInvalidState) {
		t.Errorf("draft: err = %v", err)
	}
	if _, _, err := seg.WithChannels(seg.Version, []segment.Channel{"push"}); !errors.Is(err, ErrInvalidInput) {
		t.Errorf("unknown channel: err = %v", err)
	}

	same, changed, err := seg.WithChannels(seg.Version, []segment.Channel{segment.ChannelSMS, segment.ChannelSMS})
	if err != nil || changed || same.Version != seg.Version {
		t.Errorf("no-op: changed=%t version=%d err=%v", changed, same.Version, err)
	}
	both, changed, err := seg.WithChannels(seg.Version, []segment.Channel{segment.ChannelEmail, segment.ChannelSMS, segment.ChannelEmail})
	if err != nil || !changed {
		t.Fatalf("changed=%t err=%v", changed, err)
	}
	if !slices.Equal(both.Channels, []segment.Channel{segment.ChannelSMS, segment.ChannelEmail}) ||
		both.Version != seg.Version+1 || both.SegmentVersion != seg.SegmentVersion || both.Status != StatusSegmented {
		t.Errorf("result = channels %v version %d segment_version %d status %s", both.Channels, both.Version, both.SegmentVersion, both.Status)
	}
	none, changed, err := both.WithChannels(both.Version, []segment.Channel{})
	if err != nil || !changed || len(none.Channels) != 0 || none.Channels == nil {
		t.Errorf("empty selection: channels=%#v changed=%t err=%v", none.Channels, changed, err)
	}
}
