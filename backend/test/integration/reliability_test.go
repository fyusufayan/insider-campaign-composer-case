//go:build integration

package integration

import (
	"context"
	"errors"
	"log/slog"
	"math/rand/v2"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/fyusufayan/insider-campaign-composer-case/backend/internal/approval"
	"github.com/fyusufayan/insider-campaign-composer-case/backend/internal/campaign"
	"github.com/fyusufayan/insider-campaign-composer-case/backend/internal/delivery"
	"github.com/fyusufayan/insider-campaign-composer-case/backend/internal/mockprovider"
	"github.com/fyusufayan/insider-campaign-composer-case/backend/internal/platform/logging"
	"github.com/fyusufayan/insider-campaign-composer-case/backend/internal/provider"
)

// ---- CP17A: rate limiting, 429 pauses, backoff, cancellation, the expiry
// sweeper and the dispatch lock — against real PostgreSQL, the real worker
// and the mock provider over TCP. No real message is sent anywhere.

// bulkSending creates n users in Bursa (sms + email, opted in) and returns
// a sending campaign whose snapshot holds their 2n rows.
func (e *delEnv) bulkSending(n int) (campaign.Campaign, approval.Summary) {
	e.t.Helper()
	e.exec(`INSERT INTO users (id, first_name, last_name, email, phone, city, city_normalized, gender, signup_at)
	        SELECT 1000 + g, 'Bulk', 'User', 'bulk' || g || '@example.test', '+90532' || lpad(g::text, 7, '0'),
	               'Bursa', 'bursa', 'other', now() FROM generate_series(1, $1) AS g`, n)
	e.exec(`INSERT INTO channel_consents (user_id, channel, status, source)
	        SELECT 1000 + g, ch::channel, 'opted_in', 't' FROM generate_series(1, $1) AS g, unnest(ARRAY['sms', 'email']) AS ch`, n)
	reply := scriptedReply(`{"match":"all","conditions":[{"type":"attribute","field":"city","op":"eq","value":"Bursa"}]}`, `["sms","email"]`)
	c := e.ready(reply, draftOf(prepSMS, prepSubject, prepBody))
	s := e.prepare(c)
	if s.SnapshotCount != int64(2*n) {
		e.t.Fatalf("snapshot = %d", s.SnapshotCount)
	}
	c = e.get(c.ID)
	conf, err := e.approvals.Confirm(ctxBG, c.ID, s.ApprovalID, confirmReq(c, s))
	if err != nil {
		e.t.Fatal(err)
	}
	out, err := e.approvals.Send(ctxBG, c.ID, s.ApprovalID, conf.Campaign.Version)
	if err != nil || !out.Started {
		e.t.Fatalf("send: %+v %v", out, err)
	}
	return out.Campaign, s
}

func (e *delEnv) statusCounts(approvalID campaign.ID) map[string]int64 {
	e.t.Helper()
	rs, err := e.pool.Query(ctxBG, `SELECT status, count(*) FROM campaign_deliveries WHERE approval_id = $1::uuid GROUP BY 1`, approvalID.String())
	if err != nil {
		e.t.Fatal(err)
	}
	defer rs.Close()
	out := map[string]int64{}
	for rs.Next() {
		var (
			s string
			n int64
		)
		if err := rs.Scan(&s, &n); err != nil {
			e.t.Fatal(err)
		}
		out[s] = n
	}
	return out
}

func sum(m map[string]int64) (n int64) {
	for _, v := range m {
		n += v
	}
	return n
}

// arrivalLog records, per channel, when requests reached the mock provider
// (fault functions run inside its handler). The worker and the mock run in
// this test process, so both use the same monotonic clock.
type arrivalLog struct {
	mu sync.Mutex
	at map[provider.Channel][]time.Time
	// rateLimited are the arrivals answered with an injected 429.
	rateLimited map[provider.Channel][]time.Time
}

func newArrivalLog() *arrivalLog {
	return &arrivalLog{at: map[provider.Channel][]time.Time{}, rateLimited: map[provider.Channel][]time.Time{}}
}

func (a *arrivalLog) wrap(inner mockprovider.FaultFunc) mockprovider.FaultFunc {
	return func(r mockprovider.Request) mockprovider.Fault {
		now := time.Now()
		f := mockprovider.FaultNone
		if inner != nil {
			f = inner(r)
		}
		a.mu.Lock()
		a.at[r.Channel] = append(a.at[r.Channel], now)
		if f == mockprovider.Fault429 {
			a.rateLimited[r.Channel] = append(a.rateLimited[r.Channel], now)
		}
		a.mu.Unlock()
		return f
	}
}

func (a *arrivalLog) snapshot(ch provider.Channel) (all, limited []time.Time) {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]time.Time(nil), a.at[ch]...), append([]time.Time(nil), a.rateLimited[ch]...)
}

func countIn(ts []time.Time, from, to time.Time) int {
	n := 0
	for _, t := range ts {
		if t.After(from) && t.Before(to) {
			n++
		}
	}
	return n
}

// ---- Cancellation ----

// Cancel before sending, in every pre-send status (§5.2): a live approval is
// revoked (campaign_cancelled) and its held rows deleted; the version
// advances once; a repeat or a send afterwards is 409 invalid_state.
func TestCancelBeforeSending(t *testing.T) {
	for _, stage := range []string{"content_ready", "pending_approval", "approved"} {
		t.Run(stage, func(t *testing.T) {
			e := newDelEnv(t)
			var (
				c      campaign.Campaign
				s      approval.Summary
				hasApp bool
			)
			switch stage {
			case "content_ready":
				c = e.ready(replyIstanbul, draftOf(prepSMS, prepSubject, prepBody))
			case "pending_approval":
				c, s = e.prepared()
				hasApp = true
			case "approved":
				c, s = e.approved()
				hasApp = true
			}
			var ce *campaign.ConflictError
			if _, err := e.approvals.Cancel(ctxBG, c.ID, c.Version-1); !errors.As(err, &ce) {
				t.Fatalf("stale cancel: %v", err)
			}
			out, err := e.approvals.Cancel(ctxBG, c.ID, c.Version)
			if err != nil {
				t.Fatal(err)
			}
			if out.Campaign.Status != campaign.StatusCancelled || out.Campaign.Version != c.Version+1 ||
				out.Campaign.ActiveApprovalID != nil || string(out.PreviousStatus) != stage || len(out.InFlight) != 0 {
				t.Errorf("outcome = %+v", out)
			}
			var cancelledAt bool
			if err := e.pool.QueryRow(ctxBG, `SELECT cancelled_at IS NOT NULL FROM campaigns WHERE id = $1::uuid`, c.ID.String()).Scan(&cancelledAt); err != nil || !cancelledAt {
				t.Errorf("cancelled_at = %v %v", cancelledAt, err)
			}
			if hasApp {
				if out.RevokedApprovalID == nil || *out.RevokedApprovalID != s.ApprovalID || out.ReleasedHeld != s.SnapshotCount {
					t.Errorf("revoked = %v released = %d", out.RevokedApprovalID, out.ReleasedHeld)
				}
				if row := e.approvalRow(s.ApprovalID); row.Status != "revoked" || row.RevokeReason == nil || *row.RevokeReason != "campaign_cancelled" {
					t.Errorf("approval = %+v", row)
				}
				if _, err := e.approvals.Send(ctxBG, c.ID, s.ApprovalID, out.Campaign.Version); !errors.Is(err, campaign.ErrInvalidState) {
					t.Errorf("send after cancel = %v", err)
				}
			}
			if n := e.count(`SELECT count(*) FROM campaign_deliveries`); n != 0 {
				t.Errorf("delivery rows = %d (held rows must be deleted)", n)
			}
			// A repeat — with the new or the old version — learns it is already cancelled.
			for _, v := range []int{out.Campaign.Version, c.Version} {
				var se *campaign.StateError
				if _, err := e.approvals.Cancel(ctxBG, c.ID, v); !errors.As(err, &se) || se.Reason != "the campaign is already cancelled" {
					t.Errorf("repeat v%d = %v", v, err)
				}
			}
			if _, v := e.campaignStatus(c.ID); v != c.Version+1 {
				t.Errorf("version = %d, want one increment", v)
			}
		})
	}
}

// Concurrent cancels of one campaign serialize on the campaign lock: one
// transition, the others are told it is already cancelled.
func TestCancelConcurrentRequests(t *testing.T) {
	e := newDelEnv(t)
	c, _ := e.approved()
	const n = 8
	var (
		ok, already atomic.Int32
		wg          sync.WaitGroup
	)
	start := make(chan struct{})
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			_, err := e.approvals.Cancel(ctxBG, c.ID, c.Version)
			var se *campaign.StateError
			switch {
			case err == nil:
				ok.Add(1)
			case errors.As(err, &se):
				already.Add(1)
			default:
				t.Errorf("cancel: %v", err)
			}
		}()
	}
	close(start)
	wg.Wait()
	if ok.Load() != 1 || already.Load() != n-1 {
		t.Fatalf("ok = %d already = %d", ok.Load(), already.Load())
	}
	if st, v := e.campaignStatus(c.ID); st != "cancelled" || v != c.Version+1 {
		t.Errorf("campaign = %s v%d", st, v)
	}
}

// Cancel during sending with the real worker (§10.8): queued rows are
// cancelled at once; a request that is on the wire at the moment of the
// cancellation is completed truthfully (sent, provider message id kept);
// further provider requests are bounded by the rows in flight at commit;
// the campaign stays cancelled and is never completed; every row is
// accounted for.
func TestCancelDuringSending(t *testing.T) {
	e := newDelEnv(t)
	c, s := e.bulkSending(60)
	held := make(chan struct{})
	release := make(chan struct{})
	var holdOnce, releaseOnce sync.Once
	t.Cleanup(func() { releaseOnce.Do(func() { close(release) }) })
	var heldKey atomic.Value
	e.setFaults(func(r mockprovider.Request) mockprovider.Fault {
		if r.Channel == provider.ChannelEmail {
			holdOnce.Do(func() { // the first email request stays on the wire
				heldKey.Store(r.IdempotencyKey)
				close(held)
				<-release
			})
		}
		return mockprovider.FaultNone
	})
	cfg := e.workerConfig("w1")
	cfg.RatePerSecond = 10 // slow enough to cancel mid-way
	e.start(cfg)
	<-held
	e.waitFor("some sms sent", func() bool {
		return e.count(`SELECT count(*) FROM campaign_deliveries WHERE approval_id = $1::uuid AND channel = 'sms' AND status = 'sent'`, s.ApprovalID.String()) >= 5
	})

	out, err := e.approvals.Cancel(ctxBG, c.ID, c.Version)
	if err != nil {
		t.Fatal(err)
	}
	arrivalsAtCancel := e.mock.Stats().Channels[provider.ChannelSMS].Arrivals + e.mock.Stats().Channels[provider.ChannelEmail].Arrivals
	var inFlight int64
	for _, n := range out.InFlight {
		inFlight += n
	}
	if out.PreviousStatus != campaign.StatusSending || out.Campaign.Status != campaign.StatusCancelled ||
		out.Campaign.ActiveApprovalID == nil || *out.Campaign.ActiveApprovalID != s.ApprovalID ||
		out.CancelledRows["sms"] == 0 || out.CancelledRows["email"] == 0 || out.InFlight["email"] < 1 {
		t.Fatalf("cancel = %+v", out)
	}
	// Queued rows are cancelled in the cancel transaction itself.
	if n := e.count(`SELECT count(*) FROM campaign_deliveries WHERE approval_id = $1::uuid AND status IN ('pending', 'retry_scheduled')`, s.ApprovalID.String()); n != 0 {
		t.Errorf("queued rows after cancel = %d", n)
	}
	releaseOnce.Do(func() { close(release) }) // the held request now completes at the provider
	e.waitFor("no row in flight", func() bool {
		return e.count(`SELECT count(*) FROM campaign_deliveries WHERE approval_id = $1::uuid AND status = 'in_flight'`, s.ApprovalID.String()) == 0
	})
	st := e.mock.Stats()
	after := st.Channels[provider.ChannelSMS].Arrivals + st.Channels[provider.ChannelEmail].Arrivals - arrivalsAtCancel
	if after > inFlight {
		t.Errorf("%d provider requests after the cancellation, more than the %d rows in flight", after, inFlight)
	}
	// The request that was on the wire was accepted: recorded as sent, not erased.
	var status string
	var msgID *string
	if err := e.pool.QueryRow(ctxBG, `SELECT status, provider_message_id FROM campaign_deliveries WHERE idempotency_key = $1`,
		heldKey.Load().(string)).Scan(&status, &msgID); err != nil || status != "sent" || msgID == nil {
		t.Errorf("held row = %s %v %v", status, msgID, err)
	}
	counts := e.statusCounts(s.ApprovalID)
	if sum(counts) != s.SnapshotCount || counts["sent"]+counts["cancelled"] != s.SnapshotCount {
		t.Errorf("partition = %v (snapshot %d)", counts, s.SnapshotCount)
	}
	if counts["sent"] != int64(len(e.mock.Messages())) {
		t.Errorf("sent = %d, provider messages = %d", counts["sent"], len(e.mock.Messages()))
	}
	// Every sent row has exactly its one accepted attempt; cancelled rows
	// keep any attempt history they had.
	if n := e.count(`SELECT count(*) FROM campaign_deliveries d WHERE d.approval_id = $1::uuid AND d.status = 'sent'
	    AND (SELECT count(*) FROM delivery_attempts x WHERE x.delivery_id = d.id AND x.outcome = 'accepted') <> 1`, s.ApprovalID.String()); n != 0 {
		t.Errorf("%d sent rows without exactly one accepted attempt", n)
	}
	if res, err := e.repo.CompleteCampaign(ctxBG, c.ID.String()); err != nil || res.Completed {
		t.Errorf("a cancelled campaign was completed: %+v %v", res, err)
	}
	if stc, v := e.campaignStatus(c.ID); stc != "cancelled" || v != c.Version+1 {
		t.Errorf("campaign = %s v%d", stc, v)
	}
	if st.DuplicateAcceptedKeys != 0 {
		t.Errorf("duplicate accepted keys = %d", st.DuplicateAcceptedKeys)
	}
}

// A cancellation racing the worker's completions (real goroutines): an
// accepted request stays sent, a retryable outcome ends cancelled
// whichever commits first; the campaign is cancelled once and never
// completed.
func TestCancelRacesWorkerCompletion(t *testing.T) {
	e := newDelEnv(t)
	c, s := e.sending()
	const w = "w"
	jobs, err := e.repo.Claim(ctxBG, provider.ChannelEmail, 3, w, time.Minute)
	if err != nil || len(jobs) != 3 {
		t.Fatal(jobs, err)
	}
	pres := make([]delivery.Precheck, len(jobs))
	for i, j := range jobs {
		if pres[i], err = e.repo.Precheck(ctxBG, j, w, time.Minute); err != nil {
			t.Fatal(err)
		}
	}
	results := []provider.Result{
		{Outcome: provider.OutcomeAccepted, HTTPStatus: 202, MessageID: "m-accepted"},
		{Outcome: provider.OutcomeTransientError, HTTPStatus: 503, ErrorCode: "unavailable"},
		{Outcome: provider.OutcomeTimeout, ErrorCode: "timeout"},
	}
	start := make(chan struct{})
	var wg sync.WaitGroup
	for i := range jobs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			if _, ok, err := e.repo.Complete(ctxBG, jobs[i], w, pres[i], results[i], testPolicy, 0.5); err != nil || !ok {
				t.Errorf("complete %d: %v %v", i, ok, err)
			}
		}()
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		<-start
		if _, err := e.approvals.Cancel(ctxBG, c.ID, c.Version); err != nil {
			t.Errorf("cancel: %v", err)
		}
	}()
	close(start)
	wg.Wait()
	rows := e.rows(s.ApprovalID)
	byID := map[int64]delRow{}
	for _, r := range rows {
		byID[r.ID] = r
	}
	if r := byID[jobs[0].ID]; r.Status != "sent" || r.MessageID == nil || *r.MessageID != "m-accepted" {
		t.Errorf("accepted row = %+v", r)
	}
	if r := byID[jobs[1].ID]; r.Status != "cancelled" || r.Uncertain {
		t.Errorf("transient row = %+v", r)
	}
	if r := byID[jobs[2].ID]; r.Status != "cancelled" || !r.Uncertain { // its timed-out request may have been delivered
		t.Errorf("timeout row = %+v", r)
	}
	for _, r := range rows {
		if !delivery.Status(r.Status).Terminal() {
			t.Errorf("row %d:%s left %s", r.UserID, r.Channel, r.Status)
		}
	}
	if st, v := e.campaignStatus(c.ID); st != "cancelled" || v != c.Version+1 {
		t.Errorf("campaign = %s v%d", st, v)
	}
	if res, err := e.repo.CompleteCampaign(ctxBG, c.ID.String()); err != nil || res.Completed {
		t.Errorf("completed after cancel: %+v %v", res, err)
	}
}

// Rows handed back after a cancellation never strand as pending: a release
// (shutdown, slot aborts) and a reaped expired lease both end cancelled.
func TestReleaseAndReapAfterCancelEndCancelled(t *testing.T) {
	e := newDelEnv(t)
	c, s := e.sending()
	jobs, err := e.repo.Claim(ctxBG, provider.ChannelEmail, 2, "w", time.Minute)
	if err != nil || len(jobs) != 2 {
		t.Fatal(jobs, err)
	}
	if _, err := e.approvals.Cancel(ctxBG, c.ID, c.Version); err != nil {
		t.Fatal(err)
	}
	if st, ok, err := e.repo.Release(ctxBG, jobs[0], "w", 0); err != nil || !ok || st != delivery.StatusCancelled {
		t.Errorf("release = %s %v %v", st, ok, err)
	}
	e.exec(`UPDATE campaign_deliveries SET lease_until = now() - interval '1 second' WHERE id = $1`, jobs[1].ID)
	if st, err := e.repo.Reap(ctxBG, 10, testPolicy, func() float64 { return 0.5 }); err != nil || st.Released != 1 {
		t.Errorf("reap = %+v %v", st, err)
	}
	counts := e.statusCounts(s.ApprovalID)
	if counts["cancelled"] != s.SnapshotCount || len(counts) != 1 {
		t.Errorf("partition = %v", counts)
	}
}

// ---- Approval-expiry sweeper ----

// Two sweepers race over due pending and approved approvals: each due
// approval is expired once (held rows deleted, campaign content_ready,
// version + 1); approvals that are not due, consumed approvals of sending
// campaigns (even with a past expires_at) and finished campaigns are never
// touched; a repeated sweep changes nothing. A live worker keeps sending
// meanwhile.
func TestApprovalSweeper(t *testing.T) {
	e := newDelEnv(t)
	sweeperFor := func() *approval.Sweeper {
		sw, err := approval.NewSweeper(approval.NewRepository(e.pool), approval.SweeperConfig{Interval: time.Hour, Batch: 2},
			logging.New(e.logs, slog.LevelDebug))
		if err != nil {
			t.Fatal(err)
		}
		return sw
	}
	duePending, dpS := e.prepared()
	dueApproved, daS := e.approved()
	notDue, ndS := e.prepared()
	sending, sS := e.sending()
	for _, id := range []campaign.ID{dpS.ApprovalID, daS.ApprovalID, sS.ApprovalID} {
		e.exec(`UPDATE campaign_approvals SET expires_at = clock_timestamp() - interval '1 second' WHERE id = $1::uuid`, id.String())
	}
	e.start(e.workerConfig("w1")) // a live worker alongside the sweepers

	var wg sync.WaitGroup
	stats := make([]approval.SweepStats, 2)
	for i := range stats {
		wg.Add(1)
		go func() { defer wg.Done(); stats[i] = sweeperFor().Sweep(ctxBG) }()
	}
	wg.Wait()
	if got := stats[0].Expired + stats[1].Expired; got != 2 || stats[0].Failed+stats[1].Failed != 0 {
		t.Fatalf("sweeps = %+v", stats)
	}
	for _, tc := range []struct {
		c campaign.Campaign
		s approval.Summary
	}{{duePending, dpS}, {dueApproved, daS}} {
		cur := e.get(tc.c.ID)
		if cur.Status != campaign.StatusContentReady || cur.Version != tc.c.Version+1 || cur.ActiveApprovalID != nil {
			t.Errorf("campaign = %+v", cur)
		}
		if row := e.approvalRow(tc.s.ApprovalID); row.Status != "expired" {
			t.Errorf("approval = %+v", row)
		}
		if n := e.count(`SELECT count(*) FROM campaign_deliveries WHERE approval_id = $1::uuid`, tc.s.ApprovalID.String()); n != 0 {
			t.Errorf("held rows left: %d", n)
		}
	}
	if cur := e.get(notDue.ID); cur.Status != campaign.StatusPendingApproval || cur.Version != notDue.Version {
		t.Errorf("not-due campaign = %+v", cur)
	}
	if row := e.approvalRow(ndS.ApprovalID); row.Status != "pending" {
		t.Errorf("not-due approval = %+v", row)
	}
	if row := e.approvalRow(sS.ApprovalID); row.Status != "consumed" {
		t.Errorf("consumed approval = %+v", row)
	}
	if st := e.waitTerminal(sending.ID); st != "completed" {
		t.Errorf("sending campaign = %s", st)
	}
	// Idempotent: nothing left to do.
	if again := sweeperFor().Sweep(ctxBG); again != (approval.SweepStats{}) {
		t.Errorf("second sweep = %+v", again)
	}
	if cur := e.get(duePending.ID); cur.Version != duePending.Version+1 {
		t.Errorf("version advanced twice: %d", cur.Version)
	}
}

// §5.3 verification, sweeper case: the sweeper's expiry transaction blocks
// on the campaign lock BEFORE the approval expires; it gets the lock after
// expiry and must expire it (clock_timestamp() after the lock, not the
// transaction start).
func TestSweeperLockWaitExpiry(t *testing.T) {
	e := newDelEnv(t)
	c, s := e.approved()
	var expiresAt time.Time
	if err := e.pool.QueryRow(ctxBG, `UPDATE campaign_approvals SET expires_at = clock_timestamp() + interval '300 milliseconds'
	    WHERE id = $1::uuid RETURNING expires_at`, s.ApprovalID.String()).Scan(&expiresAt); err != nil {
		t.Fatal(err)
	}
	repo := approval.NewRepository(e.pool)
	tx := e.lockCampaign(c.ID)
	type res struct {
		id  *campaign.ID
		err error
	}
	done := make(chan res, 1)
	go func() { id, err := repo.ExpireDue(ctxBG, c.ID); done <- res{id, err} }()
	waitForLockWait(t, e.pool, lockStmtPrefix)
	var xactStart time.Time
	if err := e.pool.QueryRow(ctxBG, `SELECT xact_start FROM pg_stat_activity
	    WHERE datname = current_database() AND wait_event_type = 'Lock' AND starts_with(ltrim(query), $1)`,
		lockStmtPrefix).Scan(&xactStart); err != nil {
		t.Fatal(err)
	}
	if !xactStart.Before(expiresAt) {
		t.Fatalf("the sweeper's transaction started at %s, after expiry %s", xactStart, expiresAt)
	}
	waitForDBClockPast(t, e.pool, expiresAt)
	if err := tx.Commit(ctxBG); err != nil {
		t.Fatal(err)
	}
	var r res
	select {
	case r = <-done:
	case <-time.After(delDeadline):
		t.Fatal("sweeper did not finish")
	}
	if r.err != nil || r.id == nil || *r.id != s.ApprovalID {
		t.Fatalf("expire = %v %v", r.id, r.err)
	}
	if cur := e.get(c.ID); cur.Status != campaign.StatusContentReady || cur.Version != c.Version+1 {
		t.Errorf("campaign = %+v", cur)
	}
	if _, err := e.approvals.Send(ctxBG, c.ID, s.ApprovalID, c.Version+1); !errors.Is(err, approval.ErrExpired) {
		t.Errorf("send after sweep = %v", err)
	}
}

// ---- 429 pauses ----

// Simultaneous injected 429s on SMS: the SMS channel pauses (in memory and
// durably), email keeps flowing during the pause, each 429 is recorded
// exactly once (one rate_limited attempt, rate_limited_attempts = 1, the
// transient budget untouched), and everything is delivered afterwards.
func TestRateLimitedPausesOnlyItsChannel(t *testing.T) {
	// Retry-After 2 s, longer than the rate window (1.3 s here): the
	// sliding-window log grants bursts of N and then waits a window, so only
	// a pause longer than a window can show email flowing "during" it.
	e := newDelEnvWith(t, mockprovider.Config{TimeoutStall: 10 * time.Second, RetryAfter: 2 * time.Second})
	var err error
	e.client, err = provider.NewClient(provider.ClientConfig{BaseURL: e.url, RequestTimeout: delRequestTimeout, RetryAfterCap: 3 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	c, s := e.bulkSending(250)
	log := newArrivalLog()
	var limited atomic.Int32
	e.setFaults(log.wrap(func(r mockprovider.Request) mockprovider.Fault {
		if r.Channel == provider.ChannelSMS && r.Arrival == 1 && limited.Add(1) <= 4 {
			return mockprovider.Fault429 // the first four SMS requests, concurrently in flight
		}
		return mockprovider.FaultNone
	}))
	cfg := e.workerConfig("w1")
	cfg.Policy.RetryAfterCap = 3 * time.Second
	e.start(cfg)
	if st := e.waitTerminalWithin(c.ID, 60*time.Second); st != "completed" {
		t.Fatalf("status = %s", st)
	}
	sms, sms429 := log.snapshot(provider.ChannelSMS)
	email, _ := log.snapshot(provider.ChannelEmail)
	if len(sms429) != 4 {
		t.Fatalf("injected 429s = %d", len(sms429))
	}
	// After the last 429, requests granted a slot before the pause could
	// still arrive within DISPATCH_MAX_SLOT_TO_REQUEST (+ transport); after
	// that, nothing on SMS until Retry-After (2 s) has passed.
	last := sms429[len(sms429)-1]
	slack := testGate.MaxSlotToRequest + 50*time.Millisecond
	if n := countIn(sms, last.Add(slack), last.Add(2*time.Second)); n != 0 {
		t.Errorf("%d SMS requests arrived during the pause", n)
	}
	if n := countIn(sms, last.Add(2*time.Second), last.Add(time.Hour)); n == 0 {
		t.Error("SMS did not resume after the pause")
	}
	if n := countIn(email, last.Add(slack), last.Add(2*time.Second)); n == 0 {
		t.Error("email did not flow during the SMS pause")
	}
	var paused int64
	if err := e.pool.QueryRow(ctxBG, `SELECT count(*) FROM channel_pauses WHERE channel = 'sms'`).Scan(&paused); err != nil || paused != 1 {
		t.Errorf("durable sms pause rows = %d %v", paused, err)
	}
	if n := e.count(`SELECT count(*) FROM channel_pauses WHERE channel = 'email'`); n != 0 {
		t.Errorf("email paused: %d", n)
	}
	// Each 429 counted once; the transient budget untouched.
	if n := e.count(`SELECT count(*) FROM campaign_deliveries d WHERE d.approval_id = $1::uuid AND d.rate_limited_attempts <>
	    (SELECT count(*) FROM delivery_attempts x WHERE x.delivery_id = d.id AND x.outcome = 'rate_limited')`, s.ApprovalID.String()); n != 0 {
		t.Errorf("%d rows whose rate_limited_attempts differ from their 429 attempt rows", n)
	}
	if n := e.count(`SELECT count(*) FROM campaign_deliveries WHERE approval_id = $1::uuid AND rate_limited_attempts = 1 AND attempts = 1 AND status = 'sent'`,
		s.ApprovalID.String()); n != 4 {
		t.Errorf("rows sent after one 429 = %d", n)
	}
	st := e.mock.Stats()
	for _, ch := range []provider.Channel{provider.ChannelSMS, provider.ChannelEmail} {
		attempts := e.count(`SELECT count(*) FROM delivery_attempts x JOIN campaign_deliveries d ON d.id = x.delivery_id WHERE d.channel = $1::channel`, string(ch))
		if st.Channels[ch].Arrivals != attempts {
			t.Errorf("%s: provider arrivals %d ≠ attempt rows %d", ch, st.Channels[ch].Arrivals, attempts)
		}
	}
}

// A durable pause written before a restart (e.g. a 429 just before the
// worker crashed) holds after the restart: SMS waits until it ends while
// email flows; an expired pause has no effect. Claim also skips a paused
// channel.
func TestChannelPauseSurvivesRestart(t *testing.T) {
	e := newDelEnv(t)
	c, _ := e.bulkSending(20)
	e.exec(`INSERT INTO channel_pauses (channel, paused_until) VALUES ('email', clock_timestamp() - interval '1 hour')`) // expired
	const pause = 4 * time.Second
	inserted := time.Now()
	e.exec(`INSERT INTO channel_pauses (channel, paused_until) VALUES ('sms', clock_timestamp() + $1::bigint * interval '1 millisecond')`, pause.Milliseconds())
	if jobs, err := e.repo.Claim(ctxBG, provider.ChannelSMS, 10, "probe", time.Minute); err != nil || len(jobs) != 0 {
		t.Fatalf("claim on a paused channel = %v %v", jobs, err)
	}
	log := newArrivalLog()
	e.setFaults(log.wrap(nil))
	e.start(e.workerConfig("w-restarted"))
	if st := e.waitTerminal(c.ID); st != "completed" {
		t.Fatalf("status = %s", st)
	}
	sms, _ := log.snapshot(provider.ChannelSMS)
	email, _ := log.snapshot(provider.ChannelEmail)
	sort.Slice(sms, func(i, j int) bool { return sms[i].Before(sms[j]) })
	// Round trips to the database bound the error of translating the
	// database-clock pause into this process's clock.
	if first := sms[0]; first.Before(inserted.Add(pause - 200*time.Millisecond)) {
		t.Errorf("first SMS %s after the pause was written, before its end (%s)", first.Sub(inserted), pause)
	}
	if countIn(email, inserted, inserted.Add(pause-500*time.Millisecond)) == 0 {
		t.Error("email waited for the SMS pause (or for its own expired pause)")
	}
}

// ---- Retries ----

// A scheduled retry is durable: the worker that scheduled it stops; the
// next worker sends the retry no earlier than next_attempt_at (database
// time), with the same key and destination, and records the attempts in
// order. Heartbeats are written meanwhile.
func TestScheduledRetrySurvivesRestart(t *testing.T) {
	e := newDelEnv(t)
	c, s := e.sending()
	rows := e.rows(s.ApprovalID)
	key := rows["3:email"].Key
	e.setFaults(func(r mockprovider.Request) mockprovider.Fault {
		if r.IdempotencyKey == key && r.Arrival == 1 {
			return mockprovider.Fault5xx
		}
		return mockprovider.FaultNone
	})
	cfg := e.workerConfig("w1")
	cfg.Policy.BackoffBase, cfg.Policy.BackoffCap = 2*time.Second, 2*time.Second
	cfg.Rand = func() float64 { return 0.99 } // delay = 0.99 × 2 s
	stop := e.start(cfg)
	e.waitFor("retry scheduled", func() bool {
		return e.count(`SELECT count(*) FROM campaign_deliveries WHERE idempotency_key = $1 AND status = 'retry_scheduled'`, key) == 1
	})
	stop()
	if n := e.count(`SELECT count(*) FROM worker_heartbeats WHERE worker_id = 'w1' AND last_seen_at > now() - interval '1 minute'`); n != 1 {
		t.Errorf("heartbeat rows = %d", n)
	}
	cfg.WorkerID = "w2"
	e.start(cfg)
	if st := e.waitTerminal(c.ID); st != "completed" {
		t.Fatalf("status = %s", st)
	}
	r := e.rows(s.ApprovalID)["3:email"]
	as := e.attempts(r.ID)
	if outcomes(as) != "transient_error,accepted" || r.Status != "sent" || as[0].Destination != as[1].Destination {
		t.Fatalf("attempts = %+v row = %+v", as, r)
	}
	var gap time.Duration
	if err := e.pool.QueryRow(ctxBG, `SELECT (EXTRACT(EPOCH FROM (b.started_at - a.finished_at)) * 1000)::bigint * interval '1 millisecond'
	    FROM delivery_attempts a, delivery_attempts b WHERE a.delivery_id = $1 AND b.delivery_id = $1 AND a.attempt_no = 1 AND b.attempt_no = 2`,
		r.ID).Scan(&gap); err != nil {
		t.Fatal(err)
	}
	if gap < 1900*time.Millisecond {
		t.Errorf("retry sent %s after the failure, before its backoff (1.98 s)", gap)
	}
	if n := e.mock.Arrivals(key); n != 2 {
		t.Errorf("arrivals for the key = %d", n)
	}
}

// ---- The rate guarantee from the provider's point of view (§10.11) ----

// Real time, the designated rate test: 600 messages per channel at the
// documented limit N = 100, 5 % injected 5xx and 1 % injected 429 (with
// Retry-After). The provider measures arrivals at handler entry:
// max_in_any_rolling_1s ≤ 100 per channel, its own strict limiter never
// fires, arrivals = recorded attempts (retries counted), every key accepted
// once, and the limiter was actually the bottleneck (≥ 60 in some second).
func TestProviderObservedRateCap(t *testing.T) {
	if testing.Short() {
		t.Skip("real-time rate test")
	}
	e := newDelEnvWith(t, mockprovider.Config{TimeoutStall: 10 * time.Second, RetryAfter: time.Second, RateLimit: 100})
	const users = 600
	c, s := e.bulkSending(users)
	var mu sync.Mutex
	rng := rand.New(rand.NewPCG(42, 7))
	e.setFaults(func(mockprovider.Request) mockprovider.Fault {
		mu.Lock()
		x := rng.Float64()
		mu.Unlock()
		switch {
		case x < 0.05:
			return mockprovider.Fault5xx
		case x < 0.06:
			return mockprovider.Fault429
		}
		return mockprovider.FaultNone
	})
	cfg := e.workerConfig("rate")
	cfg.PoolSize, cfg.ClaimBatch = 20, 50
	cfg.Policy.MaxTransientAttempts = 5
	cfg.Policy.MaxRateLimitedAttempts = 10
	e.start(cfg)
	st0 := time.Now()
	stc := e.waitTerminalWithin(c.ID, 120*time.Second)
	t.Logf("status %s after %s", stc, time.Since(st0).Round(time.Millisecond))
	st := e.mock.Stats()
	for _, ch := range []provider.Channel{provider.ChannelSMS, provider.ChannelEmail} {
		cs := st.Channels[ch]
		attempts := e.count(`SELECT count(*) FROM delivery_attempts x JOIN campaign_deliveries d ON d.id = x.delivery_id
		    WHERE d.approval_id = $1::uuid AND d.channel = $2::channel`, s.ApprovalID.String(), string(ch))
		t.Logf("%s: arrivals=%d attempts=%d max_in_any_rolling_1s=%d injected_5xx=%d injected_429=%d limit_exceeded=%d messages=%d",
			ch, cs.Arrivals, attempts, cs.MaxInAnyRolling1s, cs.Injected5xx, cs.Injected429, cs.LimitExceeded, cs.Messages)
		if cs.MaxInAnyRolling1s > 100 {
			t.Errorf("%s: %d requests arrived within one second (limit 100)", ch, cs.MaxInAnyRolling1s)
		}
		if cs.MaxInAnyRolling1s < 60 {
			t.Errorf("%s: at most %d per second: the limiter was never the bottleneck", ch, cs.MaxInAnyRolling1s)
		}
		if cs.LimitExceeded != 0 {
			t.Errorf("%s: the provider's strict limiter rejected %d requests", ch, cs.LimitExceeded)
		}
		if cs.Arrivals != attempts {
			t.Errorf("%s: arrivals %d ≠ attempt rows %d", ch, cs.Arrivals, attempts)
		}
		if cs.Injected5xx == 0 || cs.Injected429 == 0 {
			t.Errorf("%s: faults were not exercised: %+v", ch, cs)
		}
		if cs.MaxArrivalsPerKey > cfg.Policy.MaxTransientAttempts+cfg.Policy.MaxRateLimitedAttempts {
			t.Errorf("%s: %d arrivals for one key", ch, cs.MaxArrivalsPerKey)
		}
	}
	if st.DuplicateAcceptedKeys != 0 {
		t.Errorf("duplicate accepted keys = %d", st.DuplicateAcceptedKeys)
	}
	counts := e.statusCounts(s.ApprovalID)
	if sum(counts) != s.SnapshotCount || counts["sent"] != int64(len(e.mock.Messages())) {
		t.Errorf("partition = %v, provider messages = %d", counts, len(e.mock.Messages()))
	}
}

func (e *delEnv) waitTerminalWithin(id campaign.ID, d time.Duration) string {
	e.t.Helper()
	deadline := time.Now().Add(d)
	tick := time.NewTicker(50 * time.Millisecond)
	defer tick.Stop()
	for {
		s, _ := e.campaignStatus(id)
		if s == "completed" || s == "completed_with_failures" {
			return s
		}
		if time.Now().After(deadline) {
			e.t.Fatalf("campaign still %s after %s", s, d)
		}
		<-tick.C
	}
}

// ---- One dispatcher across worker processes (ADR-025) ----

// Two worker instances on one database: only the holder of the dispatch
// lock dispatches, so the per-process limit holds for the deployment
// (provider-observed, real time, N = 20 per channel). When the holder
// stops mid-send, the standby takes over after the quiet period and the
// provider still never sees more than N in any second; every key is
// accepted once.
func TestSingleDispatcherAcrossWorkers(t *testing.T) {
	e := newDelEnvWith(t, mockprovider.Config{TimeoutStall: 10 * time.Second, RateLimit: 20})
	c, s := e.bulkSending(100)
	cfg := e.workerConfig("w1")
	cfg.RatePerSecond = 20
	w1, stop1 := e.startWorker(cfg)
	e.waitFor("w1 dispatching", w1.Dispatching)
	cfg.WorkerID = "w2"
	w2, _ := e.startWorker(cfg)

	var both atomic.Bool
	watchCtx, stopWatch := context.WithCancel(context.Background())
	watched := make(chan struct{})
	go func() {
		defer close(watched)
		tick := time.NewTicker(2 * time.Millisecond)
		defer tick.Stop()
		for {
			if w1.Dispatching() && w2.Dispatching() {
				both.Store(true)
			}
			select {
			case <-watchCtx.Done():
				return
			case <-tick.C:
			}
		}
	}()
	e.waitFor("40 sms sent by w1", func() bool {
		return e.count(`SELECT count(*) FROM campaign_deliveries WHERE approval_id = $1::uuid AND channel = 'sms' AND status = 'sent'`, s.ApprovalID.String()) >= 40
	})
	if w2.Dispatching() {
		t.Error("the standby dispatched while the holder was running")
	}
	stop1() // the holder stops mid-send; the standby takes over
	e.waitFor("w2 dispatching", w2.Dispatching)
	if st := e.waitTerminalWithin(c.ID, 60*time.Second); st != "completed" {
		t.Fatalf("status = %s", st)
	}
	stopWatch()
	<-watched
	if both.Load() {
		t.Error("both workers dispatched at the same time")
	}
	st := e.mock.Stats()
	for _, ch := range []provider.Channel{provider.ChannelSMS, provider.ChannelEmail} {
		cs := st.Channels[ch]
		t.Logf("%s: arrivals=%d max_in_any_rolling_1s=%d limit_exceeded=%d", ch, cs.Arrivals, cs.MaxInAnyRolling1s, cs.LimitExceeded)
		if cs.MaxInAnyRolling1s > 20 || cs.LimitExceeded != 0 {
			t.Errorf("%s: %d arrivals within one second (limit 20), %d rejected", ch, cs.MaxInAnyRolling1s, cs.LimitExceeded)
		}
		if cs.Messages != 100 || cs.MaxArrivalsPerKey != 1 {
			t.Errorf("%s: messages = %d max arrivals per key = %d", ch, cs.Messages, cs.MaxArrivalsPerKey)
		}
	}
	if st.DuplicateAcceptedKeys != 0 {
		t.Errorf("duplicate accepted keys = %d", st.DuplicateAcceptedKeys)
	}
	if n := e.count(`SELECT count(DISTINCT worker_id) FROM worker_heartbeats WHERE worker_id IN ('w1', 'w2')`); n != 2 {
		t.Errorf("heartbeats from %d workers (the standby runs reaper/heartbeat too)", n)
	}
}

// ---- No transaction across a provider request (invariant 13) ----

// While a provider request is being served, no database session of the
// worker has a transaction open (the dispatch lock session holds a
// session-level advisory lock, not a transaction).
func TestNoTransactionOpenDuringProviderRequest(t *testing.T) {
	e := newDelEnv(t)
	c, _ := e.sending()
	var violations atomic.Int32
	var checks atomic.Int32
	e.setFaults(func(mockprovider.Request) mockprovider.Fault {
		var open int64
		// A transaction held open across the request shows as "idle in
		// transaction" while the provider is serving it.
		err := e.pool.QueryRow(ctxBG, `SELECT count(*) FROM pg_stat_activity
		    WHERE datname = current_database() AND pid <> pg_backend_pid() AND state LIKE 'idle in transaction%'`).Scan(&open)
		checks.Add(1)
		if err != nil || open != 0 {
			violations.Add(1)
			e.t.Logf("open transactions during a provider request: %d %v", open, err)
		}
		return mockprovider.FaultNone
	})
	// One request at a time, one channel at a time: nothing else of the
	// worker runs while a request is being served.
	cfg := e.workerConfig("w1")
	cfg.PoolSize = 1
	cfg.ReaperInterval, cfg.CompletionInterval, cfg.HeartbeatInterval = time.Hour, time.Hour, time.Hour
	cfg.Channels = []provider.Channel{provider.ChannelSMS}
	stop := e.start(cfg)
	e.waitFor("sms sent", func() bool { return len(e.mock.Messages()) == 2 })
	stop()
	cfg.WorkerID, cfg.Channels = "w2", []provider.Channel{provider.ChannelEmail}
	e.start(cfg)
	e.waitFor("all rows sent", func() bool { return len(e.mock.Messages()) == 5 })
	if st := e.waitTerminal(c.ID); st != "completed" {
		t.Fatalf("status = %s", st)
	}
	if checks.Load() != 5 || violations.Load() != 0 {
		t.Errorf("checks = %d violations = %d", checks.Load(), violations.Load())
	}
}

// A precheck the gate gave up on (timeout on our side) but that committed
// after its undo leaves dispatch_started_at set; the next precheck misses
// and is classified as that stale dispatch — not a lost lease — so the row
// is released without an attempt instead of waiting for the reaper's
// phantom 'unknown' attempt.
func TestStaleDispatchIsReleasedWithoutAttempt(t *testing.T) {
	e := newDelEnv(t)
	_, s := e.sending()
	jobs, err := e.repo.Claim(ctxBG, provider.ChannelSMS, 1, "w", time.Minute)
	if err != nil || len(jobs) != 1 {
		t.Fatal(jobs, err)
	}
	if _, err := e.repo.Precheck(ctxBG, jobs[0], "w", time.Minute); err != nil { // the late commit
		t.Fatal(err)
	}
	if _, err := e.repo.Precheck(ctxBG, jobs[0], "w", time.Minute); !errors.Is(err, delivery.ErrPrecheckMiss) {
		t.Fatalf("second precheck = %v", err)
	}
	if reason, err := e.repo.ClassifyMiss(ctxBG, jobs[0], "w"); err != nil || reason != delivery.MissStaleDispatch {
		t.Fatalf("reason = %v %v", reason, err)
	}
	if reason, err := e.repo.ClassifyMiss(ctxBG, jobs[0], "other"); err != nil || reason != delivery.MissLeaseLost {
		t.Fatalf("other worker: reason = %v %v", reason, err)
	}
	if st, ok, err := e.repo.Release(ctxBG, jobs[0], "w", 0); err != nil || !ok || st != delivery.StatusPending {
		t.Fatalf("release = %s %v %v", st, ok, err)
	}
	if n := e.count(`SELECT count(*) FROM delivery_attempts`); n != 0 {
		t.Errorf("attempt rows = %d", n)
	}
	if n := e.count(`SELECT count(*) FROM campaign_deliveries WHERE approval_id = $1::uuid AND (attempts <> 0 OR dispatch_started_at IS NOT NULL)`,
		s.ApprovalID.String()); n != 0 {
		t.Errorf("%d rows with a counted attempt or a stale dispatch", n)
	}
}

// ---- CP17A final review: leadership fencing and pause durability ----

// Handover after the holder's lock SESSION is lost (terminated server-side
// while the process keeps running): the holder stops (watcher + the final
// gate's leadership fence), a new lead (either process) starts after the
// quiet period,
// they never dispatch at the same time, the provider never sees more than
// N per rolling second per channel, and every key is accepted once.
func TestHandoverAfterLockSessionLoss(t *testing.T) {
	e := newDelEnvWith(t, mockprovider.Config{TimeoutStall: 10 * time.Second, RateLimit: 20})
	c, s := e.bulkSending(100)
	cfg := e.workerConfig("w1")
	cfg.RatePerSecond = 20
	w1, _ := e.startWorker(cfg)
	e.waitFor("w1 dispatching", w1.Dispatching)
	cfg.WorkerID = "w2"
	w2, _ := e.startWorker(cfg)
	var both atomic.Bool
	watchCtx, stopWatch := context.WithCancel(context.Background())
	watched := make(chan struct{})
	go func() {
		defer close(watched)
		tick := time.NewTicker(2 * time.Millisecond)
		defer tick.Stop()
		for {
			if w1.Dispatching() && w2.Dispatching() {
				both.Store(true)
			}
			select {
			case <-watchCtx.Done():
				return
			case <-tick.C:
			}
		}
	}()
	e.waitFor("30 sms sent", func() bool {
		return e.count(`SELECT count(*) FROM campaign_deliveries WHERE approval_id = $1::uuid AND channel = 'sms' AND status = 'sent'`, s.ApprovalID.String()) >= 30
	})
	var terminated bool
	if err := e.pool.QueryRow(ctxBG, `SELECT pg_terminate_backend(pid) FROM pg_locks
	    WHERE locktype = 'advisory' AND granted AND database = (SELECT oid FROM pg_database WHERE datname = current_database())`).Scan(&terminated); err != nil || !terminated {
		t.Fatalf("terminate lock session: %v %v", terminated, err)
	}
	// Either process may win the lock next (the former holder retries as
	// soon as its requests finished; the standby polls): what matters is
	// that the holder stopped, a new lead waited the quiet period, and the
	// two never overlapped.
	e.waitFor("holder noticed the lost lock", func() bool {
		return strings.Contains(e.logs.String(), "dispatch lock lost; dispatching stopped")
	})
	if st := e.waitTerminalWithin(c.ID, 60*time.Second); st != "completed" {
		t.Logf("dispatching: w1=%v w2=%v", w1.Dispatching(), w2.Dispatching())
		t.Fatalf("status = %s", st)
	}
	if n := strings.Count(e.logs.String(), "dispatch lock acquired; waiting the quiet period"); n < 2 {
		t.Errorf("lock acquisitions = %d, want a new lead after the loss", n)
	}
	stopWatch()
	<-watched
	if both.Load() {
		t.Error("both workers dispatched at the same time")
	}
	st := e.mock.Stats()
	for _, ch := range []provider.Channel{provider.ChannelSMS, provider.ChannelEmail} {
		cs := st.Channels[ch]
		if cs.MaxInAnyRolling1s > 20 || cs.LimitExceeded != 0 || cs.Messages != 100 || cs.MaxArrivalsPerKey != 1 {
			t.Errorf("%s: %+v", ch, cs)
		}
	}
}

// If the durable pauses cannot be read when a process takes the dispatch
// lock, it does not dispatch (fail closed) and keeps retrying; once they
// can be read it dispatches normally.
func TestPauseReadFailureFailsClosed(t *testing.T) {
	e := newDelEnv(t)
	c, _ := e.sending()
	e.exec(`ALTER TABLE channel_pauses RENAME TO channel_pauses_hidden`)
	w, _ := e.startWorker(e.workerConfig("w1"))
	e.waitFor("two refused leads", func() bool {
		return strings.Count(e.logs.String(), "could not read the durable channel pauses; not dispatching") >= 2
	})
	if w.Dispatching() || e.mock.Stats().Channels[provider.ChannelSMS].Arrivals+e.mock.Stats().Channels[provider.ChannelEmail].Arrivals != 0 {
		t.Fatal("dispatched without reading the durable pauses")
	}
	e.exec(`ALTER TABLE channel_pauses_hidden RENAME TO channel_pauses`)
	if st := e.waitTerminal(c.ID); st != "completed" {
		t.Fatalf("status = %s", st)
	}
}

// A durable pause is only ever extended: a later, shorter 429 (here via
// the completion transaction and via PersistPause) never shortens it.
func TestDurablePauseNeverShortened(t *testing.T) {
	e := newDelEnv(t)
	_, _ = e.sending()
	e.exec(`INSERT INTO channel_pauses (channel, paused_until) VALUES ('sms', now() + interval '20 seconds')`)
	jobs, err := e.repo.Claim(ctxBG, provider.ChannelEmail, 1, "w", time.Minute) // email is not paused
	if err != nil || len(jobs) != 1 {
		t.Fatal(jobs, err)
	}
	pre, err := e.repo.Precheck(ctxBG, jobs[0], "w", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if c, ok, err := e.repo.Complete(ctxBG, jobs[0], "w", pre, provider.Result{Outcome: provider.OutcomeRateLimited, HTTPStatus: 429,
		RetryAfter: time.Second, ErrorCode: "rate_limited"}, testPolicy, 0.5); err != nil || !ok || c.Status != delivery.StatusRetryScheduled {
		t.Fatalf("complete = %+v %v %v", c, ok, err)
	}
	if err := e.repo.PersistPause(ctxBG, provider.ChannelSMS, time.Second); err != nil {
		t.Fatal(err)
	}
	if n := e.count(`SELECT count(*) FROM channel_pauses WHERE channel = 'sms' AND paused_until > now() + interval '19 seconds'`); n != 1 {
		t.Error("the longer sms pause was shortened")
	}
	if n := e.count(`SELECT count(*) FROM channel_pauses WHERE channel = 'email' AND paused_until BETWEEN now() AND now() + interval '2 seconds'`); n != 1 {
		t.Error("the email 429 was not persisted for email alone")
	}
	pauses, err := e.repo.ActivePauses(ctxBG)
	if err != nil || pauses[provider.ChannelSMS] < 19*time.Second || pauses[provider.ChannelEmail] > 2*time.Second {
		t.Errorf("active pauses = %v %v", pauses, err)
	}
}

// A pause recorded durably by another process while this one dispatches
// (e.g. a former holder's late 429 response) is picked up by the periodic
// refresh: SMS stops within about one lock-check interval; email continues.
func TestDurablePauseFromElsewhereIsRespected(t *testing.T) {
	e := newDelEnv(t)
	c, _ := e.bulkSending(100)
	log := newArrivalLog()
	e.setFaults(log.wrap(nil))
	cfg := e.workerConfig("w1")
	cfg.RatePerSecond = 20
	w, _ := e.startWorker(cfg)
	e.waitFor("dispatching", w.Dispatching)
	e.waitFor("some sms sent", func() bool { sms, _ := log.snapshot(provider.ChannelSMS); return len(sms) >= 10 })
	written := time.Now()
	e.exec(`INSERT INTO channel_pauses (channel, paused_until) VALUES ('sms', clock_timestamp() + interval '3 seconds')`)
	if st := e.waitTerminalWithin(c.ID, 60*time.Second); st != "completed" {
		t.Fatalf("status = %s", st)
	}
	sms, _ := log.snapshot(provider.ChannelSMS)
	email, _ := log.snapshot(provider.ChannelEmail)
	// Refresh after the next successful lock check (100 ms here), plus
	// requests already granted a slot (≤ DISPATCH_MAX_SLOT_TO_REQUEST).
	settle := cfg.LockCheckInterval + cfg.LockCheckTimeout + testGate.MaxSlotToRequest + 100*time.Millisecond
	if n := countIn(sms, written.Add(settle), written.Add(3*time.Second-200*time.Millisecond)); n != 0 {
		t.Errorf("%d SMS requests during a pause recorded by another process", n)
	}
	if countIn(email, written.Add(settle), written.Add(3*time.Second)) == 0 {
		t.Error("email stopped too")
	}
}
