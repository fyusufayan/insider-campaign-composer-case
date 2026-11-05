package delivery

import (
	"math/rand/v2"
	"strings"
	"testing"
	"time"

	"github.com/fyusufayan/insider-campaign-composer-case/backend/internal/content"
	"github.com/fyusufayan/insider-campaign-composer-case/backend/internal/provider"
	"github.com/fyusufayan/insider-campaign-composer-case/backend/internal/segment"
)

var testPolicy = Policy{MaxTransientAttempts: 5, MaxRateLimitedAttempts: 10, BackoffBase: time.Second,
	BackoffCap: 60 * time.Second, RetryAfterCap: 30 * time.Second}

func TestPolicyDecide(t *testing.T) {
	for _, tc := range []struct {
		o             provider.Outcome
		att, rl       int
		retryAfter    time.Duration
		u             float64
		want          Status
		delay, pause  time.Duration
		incAtt, incRL int
	}{
		{provider.OutcomeAccepted, 0, 0, 0, 0.5, StatusSent, 0, 0, 1, 0},
		{provider.OutcomeReplayed, 3, 2, 0, 0.5, StatusSent, 0, 0, 1, 0},
		{provider.OutcomePermanentError, 0, 0, 0, 0.5, StatusFailed, 0, 0, 1, 0}, // never retried
		// Full jitter: delay = u · min(cap, base·2^(n−1)) for the n-th attempt.
		{provider.OutcomeTransientError, 0, 0, 0, 0.5, StatusRetryScheduled, 500 * time.Millisecond, 0, 1, 0},
		{provider.OutcomeTransientError, 1, 0, 0, 0.5, StatusRetryScheduled, time.Second, 0, 1, 0},
		{provider.OutcomeTimeout, 2, 0, 0, 0.25, StatusRetryScheduled, time.Second, 0, 1, 0},
		{provider.OutcomeUnknown, 3, 0, 0, 0, StatusRetryScheduled, 0, 0, 1, 0},
		{provider.OutcomeTimeout, 4, 0, 0, 0.5, StatusFailed, 0, 0, 1, 0}, // 5th attempt exhausts
		{provider.OutcomeUnknown, 4, 9, 0, 0.5, StatusFailed, 0, 0, 1, 0},
		// 429: Retry-After (≥ 1 s, ≤ cap), separate budget, pauses the channel.
		{provider.OutcomeRateLimited, 0, 0, 2 * time.Second, 0.9, StatusRetryScheduled, 2 * time.Second, 2 * time.Second, 0, 1},
		{provider.OutcomeRateLimited, 4, 0, 0, 0.9, StatusRetryScheduled, time.Second, time.Second, 0, 1},
		{provider.OutcomeRateLimited, 0, 0, time.Hour, 0.9, StatusRetryScheduled, 30 * time.Second, 30 * time.Second, 0, 1},
		// The 10th 429 fails the row but still pauses the channel.
		{provider.OutcomeRateLimited, 0, 9, time.Second, 0.9, StatusFailed, 0, time.Second, 0, 1},
	} {
		d, err := testPolicy.Decide(tc.o, tc.att, tc.rl, tc.retryAfter, tc.u)
		if err != nil || d.Status != tc.want || d.Delay != tc.delay || d.Pause != tc.pause ||
			d.AttemptsInc != tc.incAtt || d.RateLimitedInc != tc.incRL {
			t.Errorf("%s att=%d rl=%d u=%v: %+v %v", tc.o, tc.att, tc.rl, tc.u, d, err)
		}
	}
	if _, err := testPolicy.Decide("bogus", 0, 0, 0, 0); err == nil {
		t.Error("unknown outcome accepted")
	}
	for _, u := range []float64{-0.1, 1, 2} {
		if _, err := testPolicy.Decide(provider.OutcomeTimeout, 0, 0, 0, u); err == nil {
			t.Errorf("jitter sample %v accepted", u)
		}
	}
	// Liveness: any sequence of outcomes reaches a terminal status within the bounds.
	att, rl, steps := 0, 0, 0
	for {
		o := provider.OutcomeTimeout
		if steps%2 == 1 {
			o = provider.OutcomeRateLimited
		}
		d, _ := testPolicy.Decide(o, att, rl, 0, 0.99)
		att, rl, steps = att+d.AttemptsInc, rl+d.RateLimitedInc, steps+1
		if d.Status.Terminal() {
			break
		}
		if steps > testPolicy.MaxTransientAttempts+testPolicy.MaxRateLimitedAttempts {
			t.Fatal("no terminal status within the attempt budgets")
		}
	}
}

// Backoff ceiling: doubling from the base, capped, without overflow.
func TestPolicyBackoffProgression(t *testing.T) {
	want := []time.Duration{time.Second, 2 * time.Second, 4 * time.Second, 8 * time.Second, 16 * time.Second,
		32 * time.Second, 60 * time.Second, 60 * time.Second}
	for i, w := range want {
		if got := testPolicy.Backoff(i + 1); got != w {
			t.Errorf("Backoff(%d) = %s, want %s", i+1, got, w)
		}
	}
	huge := Policy{BackoffBase: time.Millisecond, BackoffCap: time.Hour}
	if got := huge.Backoff(10_000); got != time.Hour {
		t.Errorf("Backoff(10000) = %s (overflow?)", got)
	}
	// Full jitter: every delay lies in [0, ceiling); a seeded sweep covers
	// the whole range (no synchronized retries) and never exceeds the cap.
	rng := rand.New(rand.NewPCG(7, 11))
	for n := 0; n < 4; n++ {
		ceiling := testPolicy.Backoff(n + 1)
		var lo, hi time.Duration = ceiling, 0
		seen := map[time.Duration]bool{}
		for i := 0; i < 2000; i++ {
			d, err := testPolicy.Decide(provider.OutcomeTransientError, n, 0, 0, rng.Float64())
			if err != nil || d.Delay < 0 || d.Delay >= ceiling {
				t.Fatalf("n=%d: delay %s outside [0, %s) %v", n, d.Delay, ceiling, err)
			}
			lo, hi = min(lo, d.Delay), max(hi, d.Delay)
			seen[d.Delay/(ceiling/10)] = true
		}
		if lo > ceiling/20 || hi < ceiling*19/20 || len(seen) != 10 {
			t.Errorf("n=%d: jitter does not spread over [0, %s): lo=%s hi=%s deciles=%d", n, ceiling, lo, hi, len(seen))
		}
	}
}

func TestPolicyValidateAndHorizon(t *testing.T) {
	if err := DefaultPolicy.Validate(); err != nil {
		t.Fatal(err)
	}
	for _, p := range []Policy{{},
		{MaxTransientAttempts: 5, MaxRateLimitedAttempts: 10, BackoffBase: 0, BackoffCap: time.Second, RetryAfterCap: time.Second},
		{MaxTransientAttempts: 5, MaxRateLimitedAttempts: 10, BackoffBase: 2 * time.Second, BackoffCap: time.Second, RetryAfterCap: time.Second},
	} {
		if p.Validate() == nil {
			t.Errorf("%+v accepted", p)
		}
	}
	h := RetryHorizon(DefaultPolicy, 5*time.Second, 60*time.Second, 10*time.Second)
	if want := 5*70*time.Second + (1+2+4+8)*time.Second + 10*30*time.Second; h != want {
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
