package mockprovider_test

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/fyusufayan/insider-campaign-composer-case/backend/internal/mockprovider"
	"github.com/fyusufayan/insider-campaign-composer-case/backend/internal/provider"
)

func quietLog() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// env is the mock provider over real TCP plus the production client.
type env struct {
	t      *testing.T
	srv    *mockprovider.Server
	http   *httptest.Server
	client *provider.Client
}

func newEnv(t *testing.T, cfg mockprovider.Config, timeout time.Duration, opts ...mockprovider.Option) *env {
	t.Helper()
	srv, err := mockprovider.New(cfg, quietLog(), opts...)
	if err != nil {
		t.Fatal(err)
	}
	hs := httptest.NewServer(srv.Handler())
	t.Cleanup(hs.Close)
	c, err := provider.NewClient(provider.ClientConfig{BaseURL: hs.URL, RequestTimeout: timeout})
	if err != nil {
		t.Fatal(err)
	}
	return &env{t: t, srv: srv, http: hs, client: c}
}

var (
	sms = provider.Message{Channel: provider.ChannelSMS, IdempotencyKey: "c1:42:sms", To: "+905550001234",
		Text: "Hi Ayşe, your cart is waiting."}
	email = provider.Message{Channel: provider.ChannelEmail, IdempotencyKey: "c1:42:email", To: "user42@example.test",
		Subject: "Your cart", Text: "Hi Ayşe,\n\nYour cart is waiting.\n\n-- \nfooter"}
)

func (e *env) send(m provider.Message) provider.Result {
	e.t.Helper()
	return e.client.Send(context.Background(), m)
}

// Same key + same payload → the original result is replayed (200,
// original message id, Idempotent-Replayed) and nothing is sent again.
func TestIdempotentReplay(t *testing.T) {
	e := newEnv(t, mockprovider.Config{}, 2*time.Second)
	for _, m := range []provider.Message{sms, email} {
		first := e.send(m)
		if first.Outcome != provider.OutcomeAccepted || first.HTTPStatus != http.StatusAccepted || first.MessageID == "" {
			t.Fatalf("%s first = %+v", m.Channel, first)
		}
		for i := 0; i < 3; i++ {
			again := e.send(m)
			if again.Outcome != provider.OutcomeReplayed || again.HTTPStatus != http.StatusOK || again.MessageID != first.MessageID {
				t.Fatalf("%s replay = %+v (first %+v)", m.Channel, again, first)
			}
		}
		if n := e.srv.Arrivals(m.IdempotencyKey); n != 4 {
			t.Errorf("%s arrivals = %d", m.Channel, n)
		}
	}
	msgs := e.srv.Messages()
	if len(msgs) != 2 || msgs[0].Key != sms.IdempotencyKey || msgs[0].Text != sms.Text || msgs[1].Subject != email.Subject {
		t.Fatalf("ledger = %+v", msgs)
	}
	st := e.srv.Stats()
	for _, ch := range []provider.Channel{provider.ChannelSMS, provider.ChannelEmail} {
		c := st.Channels[ch]
		if c.Arrivals != 4 || c.Accepted != 1 || c.Replayed != 3 || c.Messages != 1 || c.MaxArrivalsPerKey != 4 {
			t.Errorf("%s stats = %+v", ch, c)
		}
	}
	if st.DuplicateAcceptedKeys != 0 {
		t.Errorf("duplicates = %d", st.DuplicateAcceptedKeys)
	}
}

// Same key + different payload (destination, text, subject, channel) →
// 422 idempotency_key_reuse, a permanent error; the ledger keeps the
// original message only.
func TestKeyReuseWithDifferentPayloadIsRejected(t *testing.T) {
	e := newEnv(t, mockprovider.Config{}, 2*time.Second)
	first := e.send(sms)
	changed := []provider.Message{sms, sms, email}
	changed[0].Text += "!"
	changed[1].To = "+905550009999"
	changed[2].IdempotencyKey = sms.IdempotencyKey // same key on the other channel
	emailChanged := email
	emailChanged.Subject = "Other subject"
	e.send(email)
	changed = append(changed, emailChanged)
	for i, m := range changed {
		r := e.send(m)
		if r.Outcome != provider.OutcomePermanentError || r.HTTPStatus != http.StatusUnprocessableEntity ||
			r.ErrorCode != provider.CodeIdempotencyKeyReuse || r.MessageID != "" {
			t.Errorf("case %d: %+v", i, r)
		}
	}
	msgs := e.srv.Messages()
	if len(msgs) != 2 || msgs[0].Text != sms.Text || msgs[0].To != sms.To || msgs[0].MessageID != first.MessageID {
		t.Errorf("ledger = %+v", msgs)
	}
	// The original request still replays.
	if r := e.send(sms); r.Outcome != provider.OutcomeReplayed || r.MessageID != first.MessageID {
		t.Errorf("replay after reuse = %+v", r)
	}
}

// An injected timeout commits the message before stalling: the client sees
// an ambiguous timeout; the retry with the same key is reconciled by the
// replay, with exactly one ledger entry.
func TestTimeoutThatCommittedIsReconciledByRetry(t *testing.T) {
	var calls atomic.Int32
	e := newEnv(t, mockprovider.Config{TimeoutStall: 10 * time.Second}, 200*time.Millisecond,
		mockprovider.WithFaults(func(r mockprovider.Request) mockprovider.Fault {
			if calls.Add(1) == 1 {
				return mockprovider.FaultTimeout
			}
			return mockprovider.FaultNone
		}))
	first := e.send(sms)
	if first.Outcome != provider.OutcomeTimeout || !first.Outcome.Ambiguous() || first.MessageID != "" {
		t.Fatalf("first = %+v", first)
	}
	retry := e.send(sms)
	if retry.Outcome != provider.OutcomeReplayed || retry.MessageID == "" {
		t.Fatalf("retry = %+v", retry)
	}
	msgs := e.srv.Messages()
	if len(msgs) != 1 || msgs[0].MessageID != retry.MessageID {
		t.Fatalf("ledger = %+v", msgs)
	}
	if st := e.srv.Stats().Channels[provider.ChannelSMS]; st.InjectedTimeouts != 1 || st.Accepted != 1 || st.Replayed != 1 {
		t.Errorf("stats = %+v", st)
	}
}

// Injected 5xx and 429 answer before the ledger: nothing is recorded; the
// next attempt is accepted as a new message.
func TestInjectedTransientFaultsRecordNothing(t *testing.T) {
	seq := []mockprovider.Fault{mockprovider.Fault5xx, mockprovider.Fault429, mockprovider.FaultNone}
	var n atomic.Int32
	e := newEnv(t, mockprovider.Config{RetryAfter: 2 * time.Second}, 2*time.Second,
		mockprovider.WithFaults(func(mockprovider.Request) mockprovider.Fault { return seq[n.Add(1)-1] }))
	if r := e.send(sms); r.Outcome != provider.OutcomeTransientError || r.HTTPStatus != 503 || r.ErrorCode != "unavailable" {
		t.Errorf("5xx = %+v", r)
	}
	if r := e.send(sms); r.Outcome != provider.OutcomeRateLimited || r.HTTPStatus != 429 || r.RetryAfter != 2*time.Second {
		t.Errorf("429 = %+v", r)
	}
	if len(e.srv.Messages()) != 0 {
		t.Fatal("faulted requests were recorded")
	}
	if r := e.send(sms); r.Outcome != provider.OutcomeAccepted {
		t.Errorf("third = %+v", r)
	}
	if len(e.srv.Messages()) != 1 {
		t.Error("ledger")
	}
}

func TestPermanentFailurePattern(t *testing.T) {
	e := newEnv(t, mockprovider.Config{PermanentFailSuffix: "0000"}, 2*time.Second)
	m := sms
	m.To = "+905551230000"
	r := e.send(m)
	if r.Outcome != provider.OutcomePermanentError || r.HTTPStatus != 400 || r.ErrorCode != "destination_rejected" {
		t.Errorf("result = %+v", r)
	}
	if len(e.srv.Messages()) != 0 {
		t.Error("rejected message recorded")
	}
	if r := e.send(sms); r.Outcome != provider.OutcomeAccepted {
		t.Errorf("other destination = %+v", r)
	}
}

// Strict request validation (400 invalid_request, a permanent error);
// nothing is recorded and the problem text never echoes the input.
func TestStrictRequestValidation(t *testing.T) {
	e := newEnv(t, mockprovider.Config{}, 2*time.Second)
	post := func(path, key, ctype, body string) (int, string) {
		req, _ := http.NewRequest(http.MethodPost, e.http.URL+path, strings.NewReader(body))
		if key != "" {
			req.Header.Set(provider.HeaderIdempotencyKey, key)
		}
		if ctype != "" {
			req.Header.Set("Content-Type", ctype)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(b)
	}
	const js = "application/json"
	for _, tc := range []struct{ name, path, key, ctype, body string }{
		{"no key", provider.PathSMS, "", js, `{"to":"+905550001234","text":"hi"}`},
		{"key with space", provider.PathSMS, "a b", js, `{"to":"+905550001234","text":"hi"}`},
		{"key too long", provider.PathSMS, strings.Repeat("k", 201), js, `{"to":"+905550001234","text":"hi"}`},
		{"media type", provider.PathSMS, "k1", "text/plain", `{"to":"+905550001234","text":"hi"}`},
		{"malformed", provider.PathSMS, "k1", js, `{"to":`},
		{"unknown field", provider.PathSMS, "k1", js, `{"to":"+905550001234","text":"hi","subject":"x"}`},
		{"trailing data", provider.PathSMS, "k1", js, `{"to":"+905550001234","text":"hi"} {}`},
		{"bad phone", provider.PathSMS, "k1", js, `{"to":"05550001234","text":"hi"}`},
		{"empty text", provider.PathSMS, "k1", js, `{"to":"+905550001234","text":""}`},
		{"bad email", provider.PathEmail, "k2", js, `{"to":"not-an-email","subject":"s","text":"t"}`},
		{"subject newline", provider.PathEmail, "k2", js, `{"to":"a@example.test","subject":"a\nb","text":"t"}`},
		{"missing subject", provider.PathEmail, "k2", js, `{"to":"a@example.test","subject":"","text":"t"}`},
	} {
		status, body := post(tc.path, tc.key, tc.ctype, tc.body)
		if status != http.StatusBadRequest || !strings.Contains(body, `"invalid_request"`) {
			t.Errorf("%s: %d %s", tc.name, status, body)
		}
		if strings.Contains(body, "0001234") || strings.Contains(body, "not-an-email") {
			t.Errorf("%s: error echoes input: %s", tc.name, body)
		}
	}
	if len(e.srv.Messages()) != 0 {
		t.Error("invalid requests recorded")
	}
	if st := e.srv.Stats().Channels[provider.ChannelSMS]; st.RejectedInvalid != 9 || st.Arrivals != 9 {
		t.Errorf("sms stats = %+v", st)
	}
}

// The admin stats contain counters only: no keys, destinations or texts.
func TestAdminStatsLeakNothing(t *testing.T) {
	e := newEnv(t, mockprovider.Config{AllowReset: true}, 2*time.Second)
	e.send(sms)
	e.send(email)
	resp, err := http.Get(e.http.URL + "/admin/stats")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	for _, secret := range []string{sms.To, email.To, sms.Text, "Ayşe", sms.IdempotencyKey, "c1:42"} {
		if strings.Contains(string(b), secret) {
			t.Errorf("stats contain %q: %s", secret, b)
		}
	}
	var st mockprovider.Stats
	if err := json.Unmarshal(b, &st); err != nil || st.Channels[provider.ChannelSMS].Messages != 1 {
		t.Errorf("stats = %s (%v)", b, err)
	}
	// Reset clears the ledger (tests only).
	r, _ := http.Post(e.http.URL+"/admin/reset", "application/json", nil)
	r.Body.Close()
	if len(e.srv.Messages()) != 0 {
		t.Error("reset kept the ledger")
	}
}

// Random faults are reproducible for a seed and arrival order.
func TestSeededFaultsAreReproducible(t *testing.T) {
	cfg := mockprovider.Config{Fault5xxRate: 0.3, Fault429Rate: 0.2, Seed: 7}
	run := func() []provider.Outcome {
		e := newEnv(t, cfg, 2*time.Second)
		var out []provider.Outcome
		for i := 0; i < 40; i++ {
			m := sms
			m.IdempotencyKey = "k" + string(rune('a'+i%26)) + string(rune('a'+i/26))
			out = append(out, e.send(m).Outcome)
		}
		return out
	}
	a, b := run(), run()
	var faults int
	for i := range a {
		if a[i] != b[i] {
			t.Fatalf("run differs at %d: %v vs %v", i, a, b)
		}
		if a[i] != provider.OutcomeAccepted {
			faults++
		}
	}
	if faults == 0 || faults == len(a) {
		t.Errorf("faults = %d of %d", faults, len(a))
	}
}

func TestConfigValidation(t *testing.T) {
	for _, c := range []mockprovider.Config{
		{Fault5xxRate: -0.1}, {Fault429Rate: 1.1}, {Fault5xxRate: 0.6, Fault429Rate: 0.6},
		{LatencyMin: time.Second, LatencyMax: time.Millisecond}, {TimeoutStall: -1},
	} {
		if c.Validate() == nil {
			t.Errorf("%+v accepted", c)
		}
	}
}

// Concurrent identical requests: the ledger lookup and insert happen in one
// critical section, so exactly one is accepted as a new message and every
// other one replays it.
func TestConcurrentIdenticalRequestsAcceptOnce(t *testing.T) {
	e := newEnv(t, mockprovider.Config{LatencyMax: 5 * time.Millisecond}, 2*time.Second)
	const n = 32
	results := make(chan provider.Result, n)
	start := make(chan struct{})
	for i := 0; i < n; i++ {
		go func() {
			<-start
			results <- e.client.Send(context.Background(), sms)
		}()
	}
	close(start)
	var accepted, replayed int
	ids := map[string]bool{}
	for i := 0; i < n; i++ {
		r := <-results
		switch r.Outcome {
		case provider.OutcomeAccepted:
			accepted++
		case provider.OutcomeReplayed:
			replayed++
		default:
			t.Errorf("result = %+v", r)
		}
		ids[r.MessageID] = true
	}
	if accepted != 1 || replayed != n-1 || len(ids) != 1 || len(e.srv.Messages()) != 1 {
		t.Errorf("accepted %d, replayed %d, ids %v, ledger %d", accepted, replayed, ids, len(e.srv.Messages()))
	}
}

// The provider's own strict limiter: more than RateLimit arrivals of one
// channel within a rolling second are answered 429 rate_limit_exceeded
// (counted apart from injected 429s, nothing recorded); the other channel
// is independent; every arrival is in max_in_any_rolling_1s.
func TestStrictRateLimitAndRollingStats(t *testing.T) {
	e := newEnv(t, mockprovider.Config{RateLimit: 5}, 2*time.Second)
	start := time.Now()
	var accepted, limited int
	for i := 0; i < 8; i++ {
		m := sms
		m.IdempotencyKey = "c1:" + string(rune('a'+i)) + ":sms"
		switch r := e.send(m); r.Outcome {
		case provider.OutcomeAccepted:
			accepted++
		case provider.OutcomeRateLimited:
			limited++
			if r.RetryAfter != time.Second {
				t.Errorf("429 = %+v", r)
			}
		default:
			t.Fatalf("outcome %+v", r)
		}
	}
	if time.Since(start) >= time.Second {
		t.Skip("8 loopback requests took over a second; the burst premise does not hold on this machine")
	}
	if r := e.send(email); r.Outcome != provider.OutcomeAccepted {
		t.Errorf("email limited by sms traffic: %+v", r)
	}
	st := e.srv.Stats()
	s := st.Channels[provider.ChannelSMS]
	if accepted != 5 || limited != 3 || s.LimitExceeded != 3 || s.Injected429 != 0 || s.Messages != 5 ||
		s.Arrivals != 8 || s.MaxInAnyRolling1s != 8 {
		t.Errorf("accepted=%d limited=%d stats=%+v", accepted, limited, s)
	}
	if em := st.Channels[provider.ChannelEmail]; em.LimitExceeded != 0 || em.MaxInAnyRolling1s != 1 {
		t.Errorf("email stats = %+v", em)
	}
}
