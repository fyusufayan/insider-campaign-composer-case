package delivery

import (
	"strings"
	"testing"
	"time"

	"github.com/fyusufayan/insider-campaign-composer-case/backend/internal/content"
	"github.com/fyusufayan/insider-campaign-composer-case/backend/internal/provider"
	"github.com/fyusufayan/insider-campaign-composer-case/backend/internal/segment"
)

var testPolicy = Policy{MaxTransientAttempts: 5, MaxRateLimitedAttempts: 10, RetryDelay: 5 * time.Second, RetryAfterCap: 30 * time.Second}

func TestPolicyDecide(t *testing.T) {
	for _, tc := range []struct {
		o             provider.Outcome
		att, rl       int
		retryAfter    time.Duration
		want          Status
		delay         time.Duration
		incAtt, incRL int
	}{
		{provider.OutcomeAccepted, 0, 0, 0, StatusSent, 0, 1, 0},
		{provider.OutcomeReplayed, 3, 2, 0, StatusSent, 0, 1, 0},
		{provider.OutcomePermanentError, 0, 0, 0, StatusFailed, 0, 1, 0},
		{provider.OutcomeTransientError, 0, 0, 0, StatusRetryScheduled, 5 * time.Second, 1, 0},
		{provider.OutcomeTimeout, 3, 0, 0, StatusRetryScheduled, 5 * time.Second, 1, 0},
		{provider.OutcomeTimeout, 4, 0, 0, StatusFailed, 0, 1, 0}, // 5th attempt exhausts
		{provider.OutcomeUnknown, 4, 9, 0, StatusFailed, 0, 1, 0},
		{provider.OutcomeRateLimited, 0, 0, 2 * time.Second, StatusRetryScheduled, 2 * time.Second, 0, 1},
		{provider.OutcomeRateLimited, 4, 0, 0, StatusRetryScheduled, time.Second, 0, 1}, // ≥ 1 s; 429s don't burn attempts
		{provider.OutcomeRateLimited, 0, 0, time.Hour, StatusRetryScheduled, 30 * time.Second, 0, 1},
		{provider.OutcomeRateLimited, 0, 9, time.Second, StatusFailed, 0, 0, 1},
	} {
		d, err := testPolicy.Decide(tc.o, tc.att, tc.rl, tc.retryAfter)
		if err != nil || d.Status != tc.want || d.Delay != tc.delay || d.AttemptsInc != tc.incAtt || d.RateLimitedInc != tc.incRL {
			t.Errorf("%s att=%d rl=%d: %+v %v", tc.o, tc.att, tc.rl, d, err)
		}
	}
	if _, err := testPolicy.Decide("bogus", 0, 0, 0); err == nil {
		t.Error("unknown outcome accepted")
	}
	// Liveness: any sequence of outcomes reaches a terminal status within the bounds.
	att, rl, steps := 0, 0, 0
	for {
		o := provider.OutcomeTimeout
		if steps%2 == 1 {
			o = provider.OutcomeRateLimited
		}
		d, _ := testPolicy.Decide(o, att, rl, 0)
		att, rl, steps = att+d.AttemptsInc, rl+d.RateLimitedInc, steps+1
		if d.Status.Terminal() {
			break
		}
		if steps > 100 {
			t.Fatal("no terminal status")
		}
	}
}

func TestPolicyValidateAndHorizon(t *testing.T) {
	if err := DefaultPolicy.Validate(); err != nil {
		t.Fatal(err)
	}
	for _, p := range []Policy{{}, {MaxTransientAttempts: 5, MaxRateLimitedAttempts: 10, RetryDelay: 0, RetryAfterCap: time.Second}} {
		if p.Validate() == nil {
			t.Errorf("%+v accepted", p)
		}
	}
	h := RetryHorizon(DefaultPolicy, 5*time.Second, 60*time.Second, 10*time.Second)
	if want := 5*70*time.Second + 4*5*time.Second + 10*30*time.Second; h != want {
		t.Errorf("horizon = %s, want %s", h, want)
	}
	// The mock's durable ledger: conditional on the ledger being retained.
	if g := EffectiveGuarantee(provider.MockCapabilities, h); !strings.HasPrefix(g, "at most one delivered message") ||
		!strings.Contains(g, "durable idempotency ledger is retained and intact") || !strings.Contains(g, "explicit reset") ||
		!strings.Contains(g, "at-least-once") {
		t.Errorf("mock guarantee = %q", g)
	}
	// An in-memory ledger: conditional on no provider restart.
	mem := provider.Capabilities{IdempotencyKeys: true, KeyRetention: provider.RetentionProcessLifetime}
	if g := EffectiveGuarantee(mem, h); !strings.Contains(g, "not restarted") || !strings.Contains(g, "otherwise at-least-once") {
		t.Errorf("in-memory guarantee = %q", g)
	}
	durable := provider.Capabilities{IdempotencyKeys: true, KeyRetention: 24 * time.Hour}
	if g := EffectiveGuarantee(durable, h); !strings.HasPrefix(g, "at most one delivered message") || strings.Contains(g, "restarted") {
		t.Errorf("durable guarantee = %q", g)
	}
	short := provider.Capabilities{IdempotencyKeys: true, KeyRetention: time.Minute}
	for _, c := range []provider.Capabilities{short, {}} {
		if g := EffectiveGuarantee(c, h); !strings.HasPrefix(g, "at-least-once") {
			t.Errorf("%+v guarantee = %q", c, g)
		}
	}
	for _, g := range []string{EffectiveGuarantee(provider.MockCapabilities, h), EffectiveGuarantee(short, h), EffectiveGuarantee(mem, h)} {
		if strings.Contains(strings.ToLower(g), "exactly") && !strings.Contains(g, "not exactly-once") {
			t.Errorf("claims exactly-once: %q", g)
		}
	}
}

func sp(s string) *string { return &s }

func approvedContent(t *testing.T) content.Content {
	t.Helper()
	c, err := ParseApprovedContent(content.Draft{
		SMSBody:      sp("Hi {{first_name|there}}, 30% off in {{city}}."),
		EmailSubject: sp("{{first_name}}, your cart"),
		EmailBody:    sp("Hello {{first_name}} {{last_name}},\n\nYour cart is waiting."),
	}, []segment.Channel{segment.ChannelSMS, segment.ChannelEmail})
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// Frozen values are rendered deterministically; the email always carries the footer.
func TestBuildMessageUsesFrozenValues(t *testing.T) {
	c := approvedContent(t)
	vars := []byte(`{"first_name":"Ayşe","last_name":"Yılmaz","city":"İstanbul"}`)
	m, code, err := BuildMessage(c, provider.ChannelSMS, "k:1:sms", vars)
	if err != nil || code != "" || m.Text != "Hi Ayşe, 30% off in İstanbul." || m.IdempotencyKey != "k:1:sms" || m.To != "" {
		t.Fatalf("sms = %+v %q %v", m, code, err)
	}
	again, _, _ := BuildMessage(c, provider.ChannelSMS, "k:1:sms", vars)
	if again != m {
		t.Error("rendering is not deterministic")
	}
	e, code, err := BuildMessage(c, provider.ChannelEmail, "k:1:email", vars)
	if err != nil || code != "" || e.Subject != "Ayşe, your cart" ||
		!strings.HasPrefix(e.Text, "Hello Ayşe Yılmaz,\n\nYour cart is waiting.\n\n-- \n") || !strings.HasSuffix(e.Text, content.EmailFooter) {
		t.Fatalf("email = %+v %q %v", e, code, err)
	}
	// A blank value uses the fallback where there is one …
	blank := []byte(`{"first_name":" ","last_name":"Y","city":"Ankara"}`)
	if m, code, _ := BuildMessage(c, provider.ChannelSMS, "k", blank); code != "" || m.Text != "Hi there, 30% off in Ankara." {
		t.Errorf("fallback = %+v %q", m, code)
	}
	// … and fails permanently where there is none: never sent as empty.
	if _, code, err := BuildMessage(c, provider.ChannelEmail, "k", blank); err != nil || code != content.RenderMissingValue {
		t.Errorf("missing value = %q %v", code, err)
	}
	long := []byte(`{"first_name":"` + strings.Repeat("a", 21) + `","last_name":"Y","city":"Ankara"}`)
	if _, code, _ := BuildMessage(c, provider.ChannelSMS, "k", long); code != content.RenderValueTooLong {
		t.Errorf("too long = %q", code)
	}
	for _, bad := range []string{`{}`, `[]`, `{"first_name":"A","last_name":"B"}`, `{"first_name":"A","last_name":"B","city":1}`,
		`{"first_name":"A","last_name":"B","city":"C","phone":"+905550001234"}`, `not json`} {
		if _, code, err := BuildMessage(c, provider.ChannelSMS, "k", []byte(bad)); err != nil || code != CodeInvalidVars {
			t.Errorf("%s: %q %v", bad, code, err)
		}
	}
	if _, _, err := BuildMessage(c, "fax", "k", vars); err == nil {
		t.Error("unknown channel accepted")
	}
}

// Stored content is re-validated with content.Parse; invalid or
// non-canonical text, or a missing channel, is never sent.
func TestParseApprovedContentRejectsInvalid(t *testing.T) {
	both := []segment.Channel{segment.ChannelSMS, segment.ChannelEmail}
	for name, d := range map[string]content.Draft{
		"unknown variable":   {SMSBody: sp("Hi {{phone}}")},
		"not canonical":      {SMSBody: sp(" Hi there ")},
		"missing email":      {SMSBody: sp("Hi")},
		"markup":             {SMSBody: sp("Hi <b>x</b>"), EmailSubject: sp("s"), EmailBody: sp("b")},
		"sms over 160 runes": {SMSBody: sp(strings.Repeat("a", 161)), EmailSubject: sp("s"), EmailBody: sp("b")},
	} {
		if _, err := ParseApprovedContent(d, both); err == nil {
			t.Errorf("%s accepted", name)
		}
	}
}
