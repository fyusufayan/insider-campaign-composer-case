//go:build integration

package integration

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/fyusufayan/insider-campaign-composer-case/backend/internal/approval"
	"github.com/fyusufayan/insider-campaign-composer-case/backend/internal/campaign"
	"github.com/fyusufayan/insider-campaign-composer-case/backend/internal/content"
	"github.com/fyusufayan/insider-campaign-composer-case/backend/internal/delivery"
	"github.com/fyusufayan/insider-campaign-composer-case/backend/internal/mockprovider"
	"github.com/fyusufayan/insider-campaign-composer-case/backend/internal/platform/logging"
	"github.com/fyusufayan/insider-campaign-composer-case/backend/internal/provider"
	"github.com/fyusufayan/insider-campaign-composer-case/backend/internal/segment"
)

// ---- CP16: the delivery worker against real PostgreSQL and the real mock
// provider over TCP (httptest server running mockprovider's handler, the
// production provider client). No real message is sent anywhere.

const (
	delRequestTimeout = 300 * time.Millisecond
	delDeadline       = 30 * time.Second
)

type delEnv struct {
	*prepEnv
	mock   *mockprovider.Server
	url    string
	client *provider.Client
	repo   *delivery.Repository

	faultMu sync.Mutex
	fault   mockprovider.FaultFunc
}

func newDelEnv(t *testing.T) *delEnv {
	t.Helper()
	e := &delEnv{prepEnv: newDecisionEnv(t)}
	mock, err := mockprovider.New(mockprovider.Config{TimeoutStall: 10 * time.Second}, logging.New(e.logs, slog.LevelDebug),
		mockprovider.WithFaults(func(r mockprovider.Request) mockprovider.Fault {
			e.faultMu.Lock()
			f := e.fault
			e.faultMu.Unlock()
			if f == nil {
				return mockprovider.FaultNone
			}
			return f(r)
		}))
	if err != nil {
		t.Fatal(err)
	}
	hs := httptest.NewServer(mock.Handler())
	t.Cleanup(hs.Close)
	e.mock, e.url = mock, hs.URL
	e.client, err = provider.NewClient(provider.ClientConfig{BaseURL: hs.URL, RequestTimeout: delRequestTimeout, RetryAfterCap: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	e.repo = delivery.NewRepository(e.pool)
	return e
}

func (e *delEnv) setFaults(f mockprovider.FaultFunc) {
	e.faultMu.Lock()
	e.fault = f
	e.faultMu.Unlock()
}

var testPolicy = delivery.Policy{MaxTransientAttempts: 3, MaxRateLimitedAttempts: 3, RetryDelay: 30 * time.Millisecond,
	RetryAfterCap: time.Second}

func (e *delEnv) workerConfig(id string) delivery.Config {
	return delivery.Config{WorkerID: id, PoolSize: 8, ClaimBatch: 10, Lease: 5 * time.Second,
		PollInterval: 20 * time.Millisecond, ReaperInterval: 100 * time.Millisecond, CompletionInterval: 100 * time.Millisecond,
		ShutdownGrace: 5 * time.Second, Policy: testPolicy}
}

// start runs a worker until the returned stop function is called (also at
// cleanup); stop waits for Run to return.
func (e *delEnv) start(cfg delivery.Config) (stop func()) {
	e.t.Helper()
	w, err := delivery.New(e.repo, e.client, cfg, logging.New(e.logs, slog.LevelDebug))
	if err != nil {
		e.t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- w.Run(ctx) }()
	var once sync.Once
	stop = func() {
		once.Do(func() {
			cancel()
			select {
			case err := <-done:
				if err != nil {
					e.t.Errorf("worker: %v", err)
				}
			case <-time.After(delDeadline):
				e.t.Error("worker did not stop")
			}
		})
	}
	e.t.Cleanup(stop)
	return stop
}

// sending returns a campaign whose approval was consumed by Send.
func (e *delEnv) sending() (campaign.Campaign, approval.Summary) {
	e.t.Helper()
	c, s := e.approved()
	out, err := e.approvals.Send(ctxBG, c.ID, s.ApprovalID, c.Version)
	if err != nil || !out.Started {
		e.t.Fatalf("send: %+v %v", out, err)
	}
	return out.Campaign, s
}

// waitFor polls cond (database state) until it holds or the deadline passes.
func (e *delEnv) waitFor(what string, cond func() bool) {
	e.t.Helper()
	deadline := time.Now().Add(delDeadline)
	tick := time.NewTicker(10 * time.Millisecond)
	defer tick.Stop()
	for !cond() {
		if time.Now().After(deadline) {
			e.t.Fatalf("timed out waiting for %s", what)
		}
		<-tick.C
	}
}

func (e *delEnv) campaignStatus(id campaign.ID) (string, int) {
	e.t.Helper()
	var (
		s string
		v int
	)
	if err := e.pool.QueryRow(ctxBG, `SELECT status, version FROM campaigns WHERE id = $1::uuid`, id.String()).Scan(&s, &v); err != nil {
		e.t.Fatal(err)
	}
	return s, v
}

func (e *delEnv) waitTerminal(id campaign.ID) string {
	e.t.Helper()
	var s string
	e.waitFor("campaign completion", func() bool {
		s, _ = e.campaignStatus(id)
		return s == "completed" || s == "completed_with_failures"
	})
	return s
}

type delRow struct {
	ID          int64
	UserID      int64
	Channel     string
	Status      string
	Key         string
	Destination *string
	Attempts    int
	RateLimited int
	Uncertain   bool
	LastError   *string
	MessageID   *string
	Leased      bool
	SentAt      bool
}

func (e *delEnv) rows(approvalID campaign.ID) map[string]delRow {
	e.t.Helper()
	rs, err := e.pool.Query(ctxBG, `SELECT id, user_id, channel::text, status, idempotency_key, destination, attempts, rate_limited_attempts,
	       outcome_uncertain, last_error, provider_message_id, leased_by IS NOT NULL OR lease_until IS NOT NULL, sent_at IS NOT NULL
	FROM campaign_deliveries WHERE approval_id = $1::uuid`, approvalID.String())
	if err != nil {
		e.t.Fatal(err)
	}
	defer rs.Close()
	out := map[string]delRow{}
	for rs.Next() {
		var r delRow
		if err := rs.Scan(&r.ID, &r.UserID, &r.Channel, &r.Status, &r.Key, &r.Destination, &r.Attempts, &r.RateLimited,
			&r.Uncertain, &r.LastError, &r.MessageID, &r.Leased, &r.SentAt); err != nil {
			e.t.Fatal(err)
		}
		out[fmt.Sprintf("%d:%s", r.UserID, r.Channel)] = r
	}
	return out
}

type attemptRow struct {
	No          int
	Outcome     string
	HTTPStatus  *int
	RetryAfter  *int
	Destination string
	Err         *string
	Ordered     bool
}

func (e *delEnv) attempts(deliveryID int64) []attemptRow {
	e.t.Helper()
	rs, err := e.pool.Query(ctxBG, `SELECT attempt_no, outcome, http_status, retry_after_ms, destination, error, started_at <= finished_at
	FROM delivery_attempts WHERE delivery_id = $1 ORDER BY attempt_no`, deliveryID)
	if err != nil {
		e.t.Fatal(err)
	}
	defer rs.Close()
	var out []attemptRow
	for rs.Next() {
		var a attemptRow
		if err := rs.Scan(&a.No, &a.Outcome, &a.HTTPStatus, &a.RetryAfter, &a.Destination, &a.Err, &a.Ordered); err != nil {
			e.t.Fatal(err)
		}
		out = append(out, a)
	}
	return out
}

func outcomes(as []attemptRow) string {
	var s []string
	for i, a := range as {
		if a.No != i+1 || !a.Ordered {
			return fmt.Sprintf("bad attempt numbering %+v", as)
		}
		s = append(s, a.Outcome)
	}
	return strings.Join(s, ",")
}

func phoneOf(id int64) string { return fmt.Sprintf("+90555000%04d", id) }
func emailOf(id int64) string { return fmt.Sprintf("user%d@example.test", id) }

// The frozen fixture name is "Test" (prepSMS / prepSubject / prepBody).
const (
	wantSMSText     = "Hi Test, your cart is waiting. Enjoy 30% off today."
	wantSubjectText = "Test, your cart is waiting: 30% off"
	wantBodyText    = "Hi Test,\n\nYour cart is still waiting. Enjoy 30% off your order.\n\nSee you soon!"
)

// assertNoPII checks that logs contain no contact, name or message text.
func (e *delEnv) assertNoPII(extra ...string) {
	e.t.Helper()
	logs := e.logs.String()
	for _, id := range []int64{1, 3, 5, 8} {
		for _, s := range []string{phoneOf(id), emailOf(id)} {
			if strings.Contains(logs, s) {
				e.t.Errorf("logs contain a contact %q", s)
			}
		}
	}
	for _, s := range append([]string{wantSMSText, "Your cart is still waiting", "Hi Test", content.EmailFooter}, extra...) {
		if strings.Contains(logs, s) {
			e.t.Errorf("logs contain message text %q", s)
		}
	}
}

// Happy path: every row of the approved snapshot is sent once to the
// current contact with the frozen personalization; attempts are recorded;
// the campaign completes exactly once.
func TestWorkerDeliversApprovedSnapshot(t *testing.T) {
	e := newDelEnv(t)
	c, s := e.sending()
	e.start(e.workerConfig("w1"))
	if st := e.waitTerminal(c.ID); st != "completed" {
		t.Fatalf("status = %s", st)
	}
	_, version := e.campaignStatus(c.ID)
	if version != c.Version+1 {
		t.Errorf("version = %d, want %d (one completion transition)", version, c.Version+1)
	}
	if n := e.count(`SELECT count(*) FROM campaigns WHERE id = $1::uuid AND completed_at IS NOT NULL AND completed_at >= send_started_at`, c.ID.String()); n != 1 {
		t.Error("completed_at not set")
	}
	rows := e.rows(s.ApprovalID)
	if len(rows) != 5 {
		t.Fatalf("rows = %d", len(rows))
	}
	msgs := map[string]mockprovider.Message{}
	for _, m := range e.mock.Messages() {
		msgs[m.Key] = m
	}
	if len(msgs) != 5 {
		t.Fatalf("provider messages = %d", len(msgs))
	}
	for k, r := range rows {
		want := phoneOf(r.UserID)
		if r.Channel == "email" {
			want = emailOf(r.UserID)
		}
		if r.Status != "sent" || r.Destination == nil || *r.Destination != want || r.Attempts != 1 || r.Uncertain ||
			r.MessageID == nil || r.Leased || !r.SentAt || r.LastError != nil {
			t.Errorf("%s: %+v", k, r)
		}
		m := msgs[r.Key]
		if m.MessageID != *r.MessageID || m.To != want || e.mock.Arrivals(r.Key) != 1 {
			t.Errorf("%s: provider message %+v", k, m)
		}
		switch r.Channel {
		case "sms":
			if m.Text != wantSMSText || m.Subject != "" {
				t.Errorf("%s: sms %q", k, m.Text)
			}
		case "email":
			if m.Subject != wantSubjectText || m.Text != wantBodyText+"\n\n-- \n"+content.EmailFooter {
				t.Errorf("%s: email %q / %q", k, m.Subject, m.Text)
			}
		}
		if got := outcomes(e.attempts(r.ID)); got != "accepted" {
			t.Errorf("%s: attempts %s", k, got)
		}
		if a := e.attempts(r.ID)[0]; a.Destination != want || a.HTTPStatus == nil || *a.HTTPStatus != 202 {
			t.Errorf("%s: attempt %+v", k, a)
		}
	}
	if st := e.mock.Stats(); st.DuplicateAcceptedKeys != 0 || st.Channels[provider.ChannelSMS].Messages != 2 ||
		st.Channels[provider.ChannelEmail].Messages != 3 {
		t.Errorf("provider stats = %+v", st)
	}
	if !strings.Contains(e.logs.String(), `"msg":"campaign completed"`) {
		t.Error("completion not logged")
	}
	e.assertNoPII()
}

// Delivery-time rechecks use CURRENT consent and contact; membership and
// the frozen values are not re-evaluated.
func TestWorkerRechecksConsentAndContact(t *testing.T) {
	e := newDelEnv(t)
	c, s := e.sending()
	// After approval and send, before any attempt:
	e.exec(`UPDATE channel_consents SET status = 'opted_out' WHERE user_id = 1 AND channel = 'sms'`) // revoked
	e.exec(`DELETE FROM channel_consents WHERE user_id = 3 AND channel = 'email'`)                   // missing ≠ consent
	e.exec(`UPDATE users SET email = NULL WHERE id = 5`)                                             // contact removed
	e.exec(`UPDATE users SET email = 'not-an-email' WHERE id = 1`)                                   // contact invalid
	e.exec(`UPDATE users SET phone = '+905559998888', first_name = 'Changed' WHERE id = 8`)          // contact changed, name changed
	// A new user who would now match the rule is never added.
	e.exec(`INSERT INTO users (id, first_name, last_name, email, phone, city, city_normalized, gender, signup_at)
	        VALUES (99, 'New', 'User', 'new@example.test', '+905550000099', 'İstanbul', 'istanbul', 'other', now())`)
	e.exec(`INSERT INTO channel_consents (user_id, channel, status, source) VALUES (99, 'sms', 'opted_in', 't'), (99, 'email', 'opted_in', 't')`)
	e.exec(`INSERT INTO cart_events (user_id, product_id, category, quantity, unit_price, occurred_at) VALUES (99, 'P', 'electronics', 1, 1, now())`)

	e.start(e.workerConfig("w1"))
	if st := e.waitTerminal(c.ID); st != "completed" { // skipped rows are not failures
		t.Fatalf("status = %s", st)
	}
	rows := e.rows(s.ApprovalID)
	want := map[string]string{"1:sms": "skipped_consent_revoked", "3:email": "skipped_consent_revoked",
		"5:email": "skipped_no_contact", "1:email": "skipped_no_contact", "8:sms": "sent"}
	if len(rows) != len(want) {
		t.Fatalf("rows = %v", rows)
	}
	for k, st := range want {
		r := rows[k]
		if r.Status != st || r.Leased || r.Uncertain {
			t.Errorf("%s: %+v, want %s", k, r, st)
		}
		if st != "sent" {
			if r.Destination != nil || r.Attempts != 0 || len(e.attempts(r.ID)) != 0 || e.mock.Arrivals(r.Key) != 0 {
				t.Errorf("%s reached the provider or recorded an attempt: %+v", k, r)
			}
		}
	}
	msgs := e.mock.Messages()
	if len(msgs) != 1 || msgs[0].To != "+905559998888" || msgs[0].Text != wantSMSText {
		t.Fatalf("provider messages = %+v (fresh destination, frozen name)", msgs)
	}
	if d := rows["8:sms"].Destination; d == nil || *d != "+905559998888" {
		t.Errorf("destination = %v", d)
	}
	if n := e.count(`SELECT count(*) FROM campaign_deliveries WHERE user_id = 99`); n != 0 {
		t.Errorf("user added after prepare got %d rows", n)
	}
	e.assertNoPII("+905559998888", "new@example.test")
}

// Frozen values that cannot be rendered fail the delivery permanently
// without a provider request (never truncated or replaced).
func TestWorkerRenderFailureNeverReachesProvider(t *testing.T) {
	e := newDelEnv(t)
	// A blank first name (allowed by the users CHECK) with a placeholder
	// that has no fallback.
	e.exec(`UPDATE users SET first_name = ' ' WHERE id = 1`)
	c := e.ready(replyIstanbul, draftOf("Hi {{first_name}}, your cart is waiting. 30% off today.", prepSubject, prepBody))
	s := e.prepare(c)
	if s.Snapshot[segment.ChannelSMS].RenderIssues[content.RenderMissingValue] != 1 {
		t.Fatalf("prepare render issues = %+v", s.Snapshot)
	}
	c = e.get(c.ID)
	out, err := e.approvals.Confirm(ctxBG, c.ID, s.ApprovalID, confirmReq(c, s))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.approvals.Send(ctxBG, c.ID, s.ApprovalID, out.Campaign.Version); err != nil {
		t.Fatal(err)
	}
	e.start(e.workerConfig("w1"))
	if st := e.waitTerminal(c.ID); st != "completed_with_failures" {
		t.Fatalf("status = %s", st)
	}
	rows := e.rows(s.ApprovalID)
	r := rows["1:sms"]
	if r.Status != "failed" || r.LastError == nil || *r.LastError != content.RenderMissingValue || r.Destination != nil ||
		len(e.attempts(r.ID)) != 0 || e.mock.Arrivals(r.Key) != 0 {
		t.Errorf("render failure row = %+v", r)
	}
	// user 1's email uses fallbacks: "Hello"/"there" for the blank name.
	for _, m := range e.mock.Messages() {
		if m.Key == rows["1:email"].Key && (m.Subject != "Hello, your cart is waiting: 30% off" || !strings.HasPrefix(m.Text, "Hi there,")) {
			t.Errorf("fallback email = %q / %q", m.Subject, m.Text)
		}
	}
	if n := len(e.mock.Messages()); n != 4 {
		t.Errorf("provider messages = %d", n)
	}
}

// Provider outcomes: permanent rejection, transient error then success,
// 429 then success, a timeout that committed (reconciled by the replay),
// and an ambiguous attempt followed by exhausted retries (uncertain).
func TestWorkerProviderOutcomes(t *testing.T) {
	e := newDelEnv(t)
	c, s := e.sending()
	rows := e.rows(s.ApprovalID)
	key := func(k string) string { return rows[k].Key }
	e.setFaults(func(r mockprovider.Request) mockprovider.Fault {
		switch r.IdempotencyKey {
		case key("1:sms"):
			return mockprovider.FaultPermanent
		case key("3:email"):
			if r.Arrival == 1 {
				return mockprovider.Fault5xx
			}
		case key("1:email"):
			if r.Arrival == 1 {
				return mockprovider.Fault429
			}
		case key("5:email"):
			if r.Arrival == 1 {
				return mockprovider.FaultTimeout
			}
		case key("8:sms"):
			if r.Arrival == 1 {
				return mockprovider.FaultTimeout
			}
			return mockprovider.Fault5xx
		}
		return mockprovider.FaultNone
	})
	e.start(e.workerConfig("w1"))
	if st := e.waitTerminal(c.ID); st != "completed_with_failures" {
		t.Fatalf("status = %s", st)
	}
	rows = e.rows(s.ApprovalID)
	for k, want := range map[string]struct {
		status, attempts string
		att, rl          int
		uncertain        bool
	}{
		"1:sms":   {"failed", "permanent_error", 1, 0, false},
		"3:email": {"sent", "transient_error,accepted", 2, 0, false},
		"1:email": {"sent", "rate_limited,accepted", 1, 1, false},
		"5:email": {"sent", "timeout,replayed", 2, 0, false},
		"8:sms":   {"failed", "timeout,transient_error,transient_error", 3, 0, true},
	} {
		r := rows[k]
		if r.Status != want.status || r.Attempts != want.att || r.RateLimited != want.rl || r.Uncertain != want.uncertain || r.Leased {
			t.Errorf("%s: %+v, want %+v", k, r, want)
		}
		as := e.attempts(r.ID)
		if got := outcomes(as); got != want.attempts {
			t.Errorf("%s: attempts %s, want %s", k, got, want.attempts)
		}
		for _, a := range as {
			if r.Destination == nil || a.Destination != *r.Destination {
				t.Errorf("%s: attempt destination differs from the persisted one", k)
			}
		}
	}
	if a := e.attempts(rows["1:email"].ID)[0]; a.RetryAfter == nil || *a.RetryAfter != 1000 || *a.HTTPStatus != 429 {
		t.Errorf("429 attempt = %+v", a)
	}
	if r := rows["1:sms"]; r.LastError == nil || *r.LastError != "destination_rejected" || r.MessageID != nil {
		t.Errorf("permanent row = %+v", r)
	}
	// The committed timeout of 5:email is one message; 8:sms was delivered
	// by its first (timed-out) attempt — the documented uncertain case.
	byKey := map[string]int{}
	for _, m := range e.mock.Messages() {
		byKey[m.Key]++
	}
	if byKey[key("5:email")] != 1 || byKey[key("8:sms")] != 1 || byKey[key("1:sms")] != 0 || len(byKey) != 5-1 {
		t.Errorf("ledger = %v", byKey)
	}
	if rows["5:email"].MessageID == nil {
		t.Error("replayed row without message id")
	}
}

// Two workers on 2,000 rows: every row is claimed and sent exactly once
// (one request per idempotency key), and the campaign completes once.
func TestTwoWorkersDeliverEachRowOnce(t *testing.T) {
	e := newDelEnv(t)
	const bulk = 1000
	e.exec(`INSERT INTO users (id, first_name, last_name, email, phone, city, city_normalized, gender, signup_at)
	        SELECT 1000 + g, 'Bulk', 'User', 'bulk' || g || '@example.test', '+90532' || lpad(g::text, 7, '0'),
	               'Bursa', 'bursa', 'other', now() FROM generate_series(1, $1) AS g`, bulk)
	e.exec(`INSERT INTO channel_consents (user_id, channel, status, source)
	        SELECT 1000 + g, ch::channel, 'opted_in', 't' FROM generate_series(1, $1) AS g, unnest(ARRAY['sms', 'email']) AS ch`, bulk)
	reply := scriptedReply(`{"match":"all","conditions":[{"type":"attribute","field":"city","op":"eq","value":"Bursa"}]}`, `["sms","email"]`)
	c := e.ready(reply, draftOf(prepSMS, prepSubject, prepBody))
	s := e.prepare(c)
	if s.SnapshotCount != 2*bulk {
		t.Fatalf("snapshot = %d", s.SnapshotCount)
	}
	c = e.get(c.ID)
	out, err := e.approvals.Confirm(ctxBG, c.ID, s.ApprovalID, confirmReq(c, s))
	if err != nil {
		t.Fatal(err)
	}
	sent, err := e.approvals.Send(ctxBG, c.ID, s.ApprovalID, out.Campaign.Version)
	if err != nil {
		t.Fatal(err)
	}
	cfg := e.workerConfig("w1")
	cfg.PoolSize, cfg.ClaimBatch = 20, 50
	e.start(cfg)
	cfg.WorkerID = "w2"
	e.start(cfg)
	if st := e.waitTerminal(c.ID); st != "completed" {
		t.Fatalf("status = %s", st)
	}
	if _, v := e.campaignStatus(c.ID); v != sent.Campaign.Version+1 {
		t.Errorf("version = %d", v)
	}
	if n := e.count(`SELECT count(*) FROM campaign_deliveries WHERE approval_id = $1::uuid AND status = 'sent' AND attempts = 1`, s.ApprovalID.String()); n != 2*bulk {
		t.Errorf("sent rows = %d", n)
	}
	if n := e.count(`SELECT count(*) FROM delivery_attempts`); n != 2*bulk {
		t.Errorf("attempt rows = %d", n)
	}
	st := e.mock.Stats()
	for _, ch := range []provider.Channel{provider.ChannelSMS, provider.ChannelEmail} {
		cs := st.Channels[ch]
		if cs.Arrivals != bulk || cs.Messages != bulk || cs.MaxArrivalsPerKey != 1 || cs.Replayed != 0 {
			t.Errorf("%s provider stats = %+v", ch, cs)
		}
	}
	// Both workers took part (claims interleave).
	for _, w := range []string{"w1", "w2"} {
		if !strings.Contains(e.logs.String(), `"worker_id":"`+w+`"`) {
			t.Errorf("worker %s did not start", w)
		}
	}
}

// Concurrent claims never return the same row twice (SKIP LOCKED + lease).
func TestConcurrentClaimsAreDisjoint(t *testing.T) {
	e := newDelEnv(t)
	_, s := e.sending()
	var (
		mu   sync.Mutex
		seen = map[int64]string{}
		wg   sync.WaitGroup
	)
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			id := fmt.Sprintf("c%d", i)
			for _, ch := range []provider.Channel{provider.ChannelSMS, provider.ChannelEmail} {
				jobs, err := e.repo.Claim(ctxBG, ch, 1, id, time.Minute)
				if err != nil {
					t.Error(err)
					return
				}
				mu.Lock()
				for _, j := range jobs {
					if prev, dup := seen[j.ID]; dup {
						t.Errorf("row %d claimed by %s and %s", j.ID, prev, id)
					}
					seen[j.ID] = id
				}
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	if len(seen) != 5 {
		t.Errorf("claimed %d rows", len(seen))
	}
	if n := e.count(`SELECT count(*) FROM campaign_deliveries WHERE approval_id = $1::uuid AND status = 'in_flight' AND leased_by IS NOT NULL AND lease_until > now()`,
		s.ApprovalID.String()); n != 5 {
		t.Errorf("in-flight rows = %d", n)
	}
	more, err := e.repo.Claim(ctxBG, provider.ChannelSMS, 10, "late", time.Minute)
	if err != nil || len(more) != 0 {
		t.Errorf("late claim = %v %v", more, err)
	}
}

// Only rows of a consumed approval of a sending campaign are claimable:
// held rows (approved, not sent), rows of a cancelled campaign.
func TestClaimOnlySendingCampaigns(t *testing.T) {
	e := newDelEnv(t)
	e.approved() // held rows, never claimable
	c, _ := e.sending()
	e.exec(`UPDATE campaigns SET status = 'cancelled' WHERE id = $1::uuid`, c.ID.String())
	for _, ch := range []provider.Channel{provider.ChannelSMS, provider.ChannelEmail} {
		if jobs, err := e.repo.Claim(ctxBG, ch, 50, "w", time.Minute); err != nil || len(jobs) != 0 {
			t.Errorf("%s claim = %v %v", ch, jobs, err)
		}
	}
}

// Crash recovery: (a) a worker died after the provider accepted, before it
// recorded success → the reaper records an 'unknown' attempt, the retry is
// replayed by the provider (one message), the row ends sent; (b) a worker
// died after claiming, before the precheck → the row goes back to pending
// without an attempt.
func TestReaperRecoversCrashedWorker(t *testing.T) {
	e := newDelEnv(t)
	c, s := e.sending()
	const dead = "crashed-worker"
	smsJobs, err := e.repo.Claim(ctxBG, provider.ChannelSMS, 1, dead, time.Minute)
	if err != nil || len(smsJobs) != 1 {
		t.Fatal(smsJobs, err)
	}
	emailJobs, err := e.repo.Claim(ctxBG, provider.ChannelEmail, 1, dead, time.Minute)
	if err != nil || len(emailJobs) != 1 {
		t.Fatal(emailJobs, err)
	}
	// (a) precheck + request accepted, then the process "dies".
	j := smsJobs[0]
	pre, err := e.repo.Precheck(ctxBG, j, dead, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	cont, err := e.repo.ApprovedContent(ctxBG, j.ApprovalID)
	if err != nil {
		t.Fatal(err)
	}
	msg, code, err := delivery.BuildMessage(cont, j.Channel, j.IdempotencyKey, j.Vars)
	if err != nil || code != "" {
		t.Fatal(code, err)
	}
	msg.To = pre.Destination
	if r := e.client.Send(ctxBG, msg); r.Outcome != provider.OutcomeAccepted {
		t.Fatalf("first request = %+v", r)
	}
	// Leases expire (instead of waiting a minute).
	e.exec(`UPDATE campaign_deliveries SET lease_until = now() - interval '1 second' WHERE leased_by = $1`, dead)

	e.start(e.workerConfig("w-new"))
	if st := e.waitTerminal(c.ID); st != "completed" {
		t.Fatalf("status = %s", st)
	}
	rows := e.rows(s.ApprovalID)
	var a, b delRow
	for _, r := range rows {
		switch r.ID {
		case j.ID:
			a = r
		case emailJobs[0].ID:
			b = r
		}
	}
	if a.Status != "sent" || a.Attempts != 2 || a.Uncertain || *a.Destination != pre.Destination {
		t.Errorf("crashed-after-send row = %+v", a)
	}
	if got := outcomes(e.attempts(a.ID)); got != "unknown,replayed" {
		t.Errorf("attempts = %s", got)
	}
	if n := e.mock.Arrivals(a.Key); n != 2 {
		t.Errorf("arrivals = %d", n)
	}
	var msgs int
	for _, m := range e.mock.Messages() {
		if m.Key == a.Key {
			msgs++
		}
	}
	if msgs != 1 {
		t.Errorf("messages for the crashed row = %d", msgs)
	}
	if b.Status != "sent" || b.Attempts != 1 || outcomes(e.attempts(b.ID)) != "accepted" {
		t.Errorf("crashed-before-precheck row = %+v %s", b, outcomes(e.attempts(b.ID)))
	}
	if !strings.Contains(e.logs.String(), "expired leases recovered") {
		t.Error("reaper did not log")
	}
}

// An injected database failure while recording the outcome rolls the
// completion back (row stays in_flight, no attempt row); after the lease
// expires the reaper recovers it and the retry is reconciled by the
// provider's replay — one message per recipient.
func TestCompletionFailureIsRecovered(t *testing.T) {
	e := newDelEnv(t)
	c, s := e.sending()
	e.exec(`CREATE FUNCTION fail_attempt() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'injected failure'; END $$`)
	e.exec(`CREATE TRIGGER fail_attempts BEFORE INSERT ON delivery_attempts FOR EACH ROW EXECUTE FUNCTION fail_attempt()`)
	cfg := e.workerConfig("w1")
	cfg.Lease = time.Second
	e.start(cfg)
	e.waitFor("all requests accepted but unrecorded", func() bool {
		return len(e.mock.Messages()) == 5 &&
			e.count(`SELECT count(*) FROM campaign_deliveries WHERE approval_id = $1::uuid AND status = 'in_flight' AND dispatch_started_at IS NOT NULL`,
				s.ApprovalID.String()) == 5
	})
	if n := e.count(`SELECT count(*) FROM delivery_attempts`); n != 0 {
		t.Fatalf("attempt rows = %d", n)
	}
	if st, _ := e.campaignStatus(c.ID); st != "sending" {
		t.Fatalf("campaign = %s with in-flight rows", st)
	}
	e.exec(`DROP TRIGGER fail_attempts ON delivery_attempts`)
	if st := e.waitTerminal(c.ID); st != "completed" {
		t.Fatalf("status = %s", st)
	}
	for k, r := range e.rows(s.ApprovalID) {
		if r.Status != "sent" || outcomes(e.attempts(r.ID)) != "unknown,replayed" || e.mock.Arrivals(r.Key) != 2 {
			t.Errorf("%s: %+v %s", k, r, outcomes(e.attempts(r.ID)))
		}
	}
	if n := len(e.mock.Messages()); n != 5 {
		t.Errorf("messages = %d", n)
	}
}

// Cancellation (simulated: the cancel endpoint is not part of CP16): the
// precheck refuses a row of a campaign that is no longer sending; an
// outcome recorded after cancellation is truthful (sent stays sent) and a
// retry becomes cancelled; a cancelled campaign is never completed.
func TestWorkerRespectsCancellation(t *testing.T) {
	e := newDelEnv(t)
	c, s := e.sending()
	const w = "w"
	jobs, err := e.repo.Claim(ctxBG, provider.ChannelEmail, 3, w, time.Minute)
	if err != nil || len(jobs) != 3 {
		t.Fatal(jobs, err)
	}
	pre0, err := e.repo.Precheck(ctxBG, jobs[0], w, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	pre1, err := e.repo.Precheck(ctxBG, jobs[1], w, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	e.exec(`UPDATE campaigns SET status = 'cancelled', cancelled_at = now() WHERE id = $1::uuid`, c.ID.String())

	// Not yet prechecked: refused, recorded as cancelled, no request.
	if _, err := e.repo.Precheck(ctxBG, jobs[2], w, time.Minute); !errors.Is(err, delivery.ErrPrecheckMiss) {
		t.Fatalf("precheck after cancel: %v", err)
	}
	if reason, err := e.repo.ClassifyMiss(ctxBG, jobs[2], w); err != nil || reason != delivery.MissNotSending {
		t.Fatalf("reason = %v %v", reason, err)
	}
	if st, ok, err := e.repo.FinishWithoutRequest(ctxBG, jobs[2], w, delivery.StatusCancelled, "campaign_not_sending"); err != nil || !ok || st != delivery.StatusCancelled {
		t.Fatalf("finish = %s %v %v", st, ok, err)
	}
	// In flight when cancelled: an accepted message stays sent; a transient
	// error is not retried.
	if c0, ok, err := e.repo.Complete(ctxBG, jobs[0], w, pre0, provider.Result{Outcome: provider.OutcomeAccepted, HTTPStatus: 202, MessageID: "m0"}, testPolicy); err != nil || !ok || c0.Status != delivery.StatusSent {
		t.Fatalf("complete accepted = %+v %v %v", c0, ok, err)
	}
	if c1, ok, err := e.repo.Complete(ctxBG, jobs[1], w, pre1, provider.Result{Outcome: provider.OutcomeTransientError, HTTPStatus: 503, ErrorCode: "unavailable"}, testPolicy); err != nil || !ok || c1.Status != delivery.StatusCancelled {
		t.Fatalf("complete transient = %+v %v %v", c1, ok, err)
	}
	// Stale completion (fencing): a second Complete for the same precheck is refused.
	if _, ok, err := e.repo.Complete(ctxBG, jobs[0], w, pre0, provider.Result{Outcome: provider.OutcomeAccepted, MessageID: "m0"}, testPolicy); err != nil || ok {
		t.Errorf("duplicate completion = %v %v", ok, err)
	}
	// Remaining sms rows are pending but unclaimable; the campaign stays cancelled.
	e.start(e.workerConfig("w2"))
	if jobs, err := e.repo.Claim(ctxBG, provider.ChannelSMS, 10, "probe", time.Minute); err != nil || len(jobs) != 0 {
		t.Errorf("claim after cancel = %v %v", jobs, err)
	}
	if res, err := e.repo.CompleteCampaign(ctxBG, c.ID.String()); err != nil || res.Completed {
		t.Errorf("completion of a cancelled campaign = %+v %v", res, err)
	}
	if st, _ := e.campaignStatus(c.ID); st != "cancelled" {
		t.Errorf("status = %s", st)
	}
	if len(e.mock.Messages()) != 0 {
		t.Error("a request reached the provider")
	}
	if n := e.count(`SELECT count(*) FROM campaign_deliveries WHERE approval_id = $1::uuid AND status = 'pending'`, s.ApprovalID.String()); n != 2 {
		t.Errorf("pending sms rows = %d", n)
	}
}

// The campaign is not completed while a row is outstanding; concurrent
// completion attempts transition it exactly once.
func TestCompletionWaitsAndHappensOnce(t *testing.T) {
	e := newDelEnv(t)
	c, s := e.sending()
	rows := e.rows(s.ApprovalID)
	slow := rows["3:email"].Key
	e.setFaults(func(r mockprovider.Request) mockprovider.Fault {
		if r.IdempotencyKey == slow {
			return mockprovider.Fault5xx
		}
		return mockprovider.FaultNone
	})
	cfg := e.workerConfig("w1")
	cfg.Policy.RetryDelay = 9 * time.Minute // the retry stays scheduled
	stop := e.start(cfg)
	e.waitFor("four rows sent and one retry scheduled", func() bool {
		return e.count(`SELECT count(*) FROM campaign_deliveries WHERE approval_id = $1::uuid AND status = 'sent'`, s.ApprovalID.String()) == 4 &&
			e.count(`SELECT count(*) FROM campaign_deliveries WHERE approval_id = $1::uuid AND status = 'retry_scheduled'`, s.ApprovalID.String()) == 1
	})
	stop()
	if res, err := e.repo.CompleteCampaign(ctxBG, c.ID.String()); err != nil || res.Completed {
		t.Fatalf("completed with an outstanding row: %+v %v", res, err)
	}
	if st, v := e.campaignStatus(c.ID); st != "sending" || v != c.Version {
		t.Fatalf("campaign = %s v%d", st, v)
	}
	// The outstanding row becomes terminal (simulated: its retries exhausted).
	e.exec(`UPDATE campaign_deliveries SET status = 'failed', last_error = 'http_503' WHERE idempotency_key = $1`, slow)
	var (
		wg        sync.WaitGroup
		mu        sync.Mutex
		completed int
	)
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			res, err := e.repo.CompleteCampaign(ctxBG, c.ID.String())
			if err != nil {
				t.Error(err)
				return
			}
			if res.Completed {
				mu.Lock()
				completed++
				mu.Unlock()
				var total int64
				for _, n := range res.Counts {
					total += n.Rows
				}
				if total != 5 || res.Status != "completed_with_failures" {
					t.Errorf("result = %+v", res)
				}
			}
		}()
	}
	wg.Wait()
	if completed != 1 {
		t.Errorf("completed %d times", completed)
	}
	if st, v := e.campaignStatus(c.ID); st != "completed_with_failures" || v != c.Version+1 {
		t.Errorf("campaign = %s v%d", st, v)
	}
}

// Graceful shutdown: requests in flight when shutdown starts finish and are
// recorded; Run returns; no row stays leased. Past the grace period an
// in-flight request is aborted and recorded as unknown, and the next worker
// reconciles it.
func TestWorkerGracefulShutdown(t *testing.T) {
	t.Run("in-flight requests finish", func(t *testing.T) {
		e := newDelEnv(t)
		c, s := e.sending()
		gate := make(chan struct{})
		var arrived sync.WaitGroup
		arrived.Add(5)
		e.setFaults(func(mockprovider.Request) mockprovider.Fault {
			arrived.Done()
			<-gate
			return mockprovider.FaultNone
		})
		cfg := e.workerConfig("w1")
		cfg.ShutdownGrace = 20 * time.Second
		stop := e.start(cfg)
		arrived.Wait() // all five requests are on the wire, held by the provider
		stopped := make(chan struct{})
		go func() { stop(); close(stopped) }()
		e.waitFor("shutdown started", func() bool { return strings.Contains(e.logs.String(), "no new claims") })
		select {
		case <-stopped:
			t.Fatal("Run returned while requests were in flight")
		default: // still waiting for the held requests
		}
		close(gate)
		<-stopped
		for k, r := range e.rows(s.ApprovalID) {
			if r.Status != "sent" || r.Leased || outcomes(e.attempts(r.ID)) != "accepted" {
				t.Errorf("%s: %+v", k, r)
			}
		}
		if st, _ := e.campaignStatus(c.ID); st != "completed" {
			t.Errorf("status = %s (final completion pass)", st)
		}
	})
	t.Run("grace elapsed aborts and records unknown", func(t *testing.T) {
		e := newDelEnv(t)
		c, s := e.sending()
		gate := make(chan struct{})
		var once sync.Once
		t.Cleanup(func() { once.Do(func() { close(gate) }) })
		var arrived sync.WaitGroup
		arrived.Add(5)
		e.setFaults(func(r mockprovider.Request) mockprovider.Fault {
			if r.Arrival == 1 {
				arrived.Done()
				<-gate
			}
			return mockprovider.FaultNone
		})
		cfg := e.workerConfig("w1")
		cfg.ShutdownGrace = 50 * time.Millisecond
		e.client, _ = provider.NewClient(provider.ClientConfig{BaseURL: e.url, RequestTimeout: 20 * time.Second})
		stop := e.start(cfg)
		arrived.Wait()
		stop() // returns after the grace period, aborting the requests
		for k, r := range e.rows(s.ApprovalID) {
			if r.Status != "retry_scheduled" || r.Leased || r.Attempts != 1 || outcomes(e.attempts(r.ID)) != "unknown" {
				t.Errorf("%s: %+v %s", k, r, outcomes(e.attempts(r.ID)))
			}
		}
		// The held requests now complete at the provider (they were
		// delivered after all) …
		once.Do(func() { close(gate) })
		e.waitFor("provider accepted the held requests", func() bool { return len(e.mock.Messages()) == 5 })
		// … and the next worker's retries are reconciled by the replay.
		e.client, _ = provider.NewClient(provider.ClientConfig{BaseURL: e.url, RequestTimeout: delRequestTimeout})
		e.start(e.workerConfig("w2"))
		if st := e.waitTerminal(c.ID); st != "completed" {
			t.Fatalf("status = %s", st)
		}
		for k, r := range e.rows(s.ApprovalID) {
			if r.Status != "sent" || outcomes(e.attempts(r.ID)) != "unknown,replayed" {
				t.Errorf("%s: %+v %s", k, r, outcomes(e.attempts(r.ID)))
			}
		}
		if n := len(e.mock.Messages()); n != 5 {
			t.Errorf("messages = %d", n)
		}
	})
}

// Provider restart with a pending retry (ADR-020 durable ledger): the
// provider accepts and journals the key; the response is lost (nothing is
// recorded; the lease expires → 'unknown'); the provider process restarts
// on the same ledger; the worker retries with the SAME key and receives a
// replay of the original message id — exactly one logical acceptance.
func TestMockProviderRestartReplaysAfterLostResponse(t *testing.T) {
	e := newDelEnv(t)
	ledgerPath := t.TempDir() + "/ledger.journal"
	quiet := logging.New(e.logs, slog.LevelDebug)
	first, err := mockprovider.New(mockprovider.Config{LedgerPath: ledgerPath}, quiet)
	if err != nil {
		t.Fatal(err)
	}
	firstHS := httptest.NewServer(first.Handler())
	firstClient, err := provider.NewClient(provider.ClientConfig{BaseURL: firstHS.URL, RequestTimeout: delRequestTimeout})
	if err != nil {
		t.Fatal(err)
	}
	c, sum := e.sending()
	const dead = "crashed-worker"
	jobs, err := e.repo.Claim(ctxBG, provider.ChannelSMS, 1, dead, time.Minute)
	if err != nil || len(jobs) != 1 {
		t.Fatal(jobs, err)
	}
	j := jobs[0]
	pre, err := e.repo.Precheck(ctxBG, j, dead, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	cont, err := e.repo.ApprovedContent(ctxBG, j.ApprovalID)
	if err != nil {
		t.Fatal(err)
	}
	msg, _, err := delivery.BuildMessage(cont, j.Channel, j.IdempotencyKey, j.Vars)
	if err != nil {
		t.Fatal(err)
	}
	msg.To = pre.Destination
	accepted := firstClient.Send(ctxBG, msg) // 1. accepted and journaled; 2. the response is "lost"
	if accepted.Outcome != provider.OutcomeAccepted {
		t.Fatalf("first = %+v", accepted)
	}

	// 3. The provider process stops and a new one starts on the same ledger.
	firstHS.Close()
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
	restarted, err := mockprovider.New(mockprovider.Config{LedgerPath: ledgerPath}, quiet)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = restarted.Close() })
	hs := httptest.NewServer(restarted.Handler())
	t.Cleanup(hs.Close)
	e.mock = restarted
	e.client, err = provider.NewClient(provider.ClientConfig{BaseURL: hs.URL, RequestTimeout: delRequestTimeout})
	if err != nil {
		t.Fatal(err)
	}
	e.exec(`UPDATE campaign_deliveries SET lease_until = now() - interval '1 second' WHERE leased_by = $1`, dead)

	// 4. The worker reaps the lease and retries with the same key.
	e.start(e.workerConfig("w-new"))
	if st := e.waitTerminal(c.ID); st != "completed" {
		t.Fatalf("status = %s", st)
	}
	var row delRow
	for _, r := range e.rows(sum.ApprovalID) {
		if r.ID == j.ID {
			row = r
		}
	}
	if got := outcomes(e.attempts(j.ID)); got != "unknown,replayed" {
		t.Fatalf("attempts = %s", got)
	}
	if row.Status != "sent" || row.MessageID == nil || *row.MessageID != accepted.MessageID || row.Uncertain {
		t.Fatalf("row = %+v (original id %s)", row, accepted.MessageID)
	}
	st := restarted.StatsFor(c.ID.String() + ":")
	if st.PrefixMessages[provider.ChannelSMS] != 2 || st.PrefixMessages[provider.ChannelEmail] != 3 ||
		st.Channels[provider.ChannelSMS].Accepted != 1 || st.Channels[provider.ChannelSMS].Replayed != 1 {
		t.Fatalf("one logical acceptance per key expected: %+v", st)
	}
}
