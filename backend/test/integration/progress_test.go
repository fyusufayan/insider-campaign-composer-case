//go:build integration

package integration

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/fyusufayan/insider-campaign-composer-case/backend/internal/campaign"
	"github.com/fyusufayan/insider-campaign-composer-case/backend/internal/delivery"
	"github.com/fyusufayan/insider-campaign-composer-case/backend/internal/httpapi"
	"github.com/fyusufayan/insider-campaign-composer-case/backend/internal/mockprovider"
	"github.com/fyusufayan/insider-campaign-composer-case/backend/internal/platform/logging"
	"github.com/fyusufayan/insider-campaign-composer-case/backend/internal/provider"
)

// ---- CP17B: delivery statistics, the delivery list and the progress
// stream against real PostgreSQL, the real worker and the mock provider
// over TCP. Every expectation is cross-checked with direct SQL over the
// committed rows.

// progressAPI serves the API (as wired in cmd/api, with fast stream polls)
// over the delivery environment's database.
type progressAPI struct {
	e     *delEnv
	srv   *httptest.Server
	stats *delivery.StatsReader
}

func newProgressAPI(e *delEnv) *progressAPI {
	e.t.Helper()
	log := logging.New(e.logs, slog.LevelInfo)
	stats := delivery.NewStatsReader(e.pool, func(ctx context.Context, id string) error {
		cid, err := campaign.ParseID(id)
		if err != nil {
			return err
		}
		_, err = e.approvals.ExpireDue(ctx, cid)
		return err
	})
	srv := httptest.NewServer(httpapi.NewRouter(log, httpapi.Deps{Campaigns: e.campaigns, Approvals: e.approvals, Deliveries: stats,
		Events: httpapi.EventsConfig{Poll: 50 * time.Millisecond, Heartbeat: time.Second}}))
	e.t.Cleanup(srv.Close)
	return &progressAPI{e: e, srv: srv, stats: stats}
}

func (p *progressAPI) get(path string) (int, []byte) {
	p.e.t.Helper()
	res, err := http.Get(p.srv.URL + path)
	if err != nil {
		p.e.t.Fatal(err)
	}
	defer res.Body.Close()
	body, err := io.ReadAll(res.Body)
	if err != nil {
		p.e.t.Fatal(err)
	}
	return res.StatusCode, body
}

func (p *progressAPI) statsOf(id campaign.ID) (delivery.CampaignStats, []byte) {
	p.e.t.Helper()
	code, body := p.get("/api/v1/campaigns/" + id.String() + "/stats")
	if code != 200 {
		p.e.t.Fatalf("stats → %d %s", code, body)
	}
	var st delivery.CampaignStats
	if err := json.Unmarshal(body, &st); err != nil {
		p.e.t.Fatal(err)
	}
	return st, body
}

func (p *progressAPI) page(id campaign.ID, query string) (delivery.Page, []byte) {
	p.e.t.Helper()
	code, body := p.get("/api/v1/campaigns/" + id.String() + "/deliveries" + query)
	if code != 200 {
		p.e.t.Fatalf("deliveries%s → %d %s", query, code, body)
	}
	var pg delivery.Page
	if err := json.Unmarshal(body, &pg); err != nil {
		p.e.t.Fatal(err)
	}
	return pg, body
}

type progressEvent struct{ name, data string }

// progressStream is a progress-stream reader that collects every event.
type progressStream struct {
	mu     sync.Mutex
	events []progressEvent
	raw    strings.Builder
	done   chan struct{}
	cancel context.CancelFunc
}

func (p *progressAPI) open(id campaign.ID) *progressStream {
	p.e.t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, p.srv.URL+"/api/v1/campaigns/"+id.String()+"/events", nil)
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		cancel()
		p.e.t.Fatal(err)
	}
	if res.StatusCode != 200 {
		cancel()
		p.e.t.Fatalf("events → %d", res.StatusCode)
	}
	s := &progressStream{done: make(chan struct{}), cancel: cancel}
	go func() {
		defer close(s.done)
		defer res.Body.Close()
		sc := bufio.NewScanner(res.Body)
		sc.Buffer(make([]byte, 0, 64<<10), 1<<20)
		var name, data string
		for sc.Scan() {
			line := sc.Text()
			s.mu.Lock()
			s.raw.WriteString(line + "\n")
			switch {
			case strings.HasPrefix(line, "event: "):
				name = line[len("event: "):]
			case strings.HasPrefix(line, "data: "):
				data = line[len("data: "):]
			case line == "" && name != "":
				s.events = append(s.events, progressEvent{name: name, data: data})
				name, data = "", ""
			}
			s.mu.Unlock()
		}
	}()
	p.e.t.Cleanup(func() { cancel(); <-s.done })
	return s
}

// wait blocks until the server closed the stream (deadline-bounded).
func (s *progressStream) wait(t *testing.T) {
	t.Helper()
	select {
	case <-s.done:
	case <-time.After(delDeadline):
		t.Fatal("progress stream did not end")
	}
}

func (s *progressStream) snapshots(t *testing.T) ([]delivery.CampaignStats, int) {
	t.Helper()
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []delivery.CampaignStats
	ends := 0
	for _, ev := range s.events {
		switch ev.name {
		case "stats":
			var st delivery.CampaignStats
			if err := json.Unmarshal([]byte(ev.data), &st); err != nil {
				t.Fatalf("stats event: %v", err)
			}
			out = append(out, st)
		case "end":
			ends++
		default:
			t.Fatalf("unexpected event %q", ev.name)
		}
	}
	return out, ends
}

func (s *progressStream) rawText() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.raw.String()
}

// dbPartition reads (channel, status) → rows directly.
func (e *delEnv) dbPartition(approvalID campaign.ID) map[string]map[string]int64 {
	e.t.Helper()
	rs, err := e.pool.Query(ctxBG, `SELECT channel::text, status, count(*) FROM campaign_deliveries WHERE approval_id = $1::uuid GROUP BY 1, 2`,
		approvalID.String())
	if err != nil {
		e.t.Fatal(err)
	}
	defer rs.Close()
	out := map[string]map[string]int64{"sms": {}, "email": {}}
	for rs.Next() {
		var (
			ch, st string
			n      int64
		)
		if err := rs.Scan(&ch, &st, &n); err != nil {
			e.t.Fatal(err)
		}
		out[ch][st] = n
	}
	return out
}

func partitionMap(p delivery.Partition) map[string]int64 {
	m := map[string]int64{"pending": p.Pending, "in_flight": p.InFlight, "retry_scheduled": p.RetryScheduled, "sent": p.Sent,
		"failed": p.Failed, "skipped_consent_revoked": p.SkippedConsentRevoked, "skipped_no_contact": p.SkippedNoContact, "cancelled": p.Cancelled}
	for k, v := range m {
		if v == 0 {
			delete(m, k)
		}
	}
	return m
}

func checkIdentities(t *testing.T, where string, st delivery.CampaignStats) {
	t.Helper()
	// The client's decoder requires a heartbeat time exactly when a heartbeat exists.
	w := st.Worker
	if (w.Status == delivery.WorkerFresh || w.Status == delivery.WorkerStale) != (w.LastSeenAt != nil && w.AgeSeconds != nil) {
		t.Errorf("%s: worker %+v", where, w)
	}
	var total delivery.Partition
	for ch, cs := range st.Channels {
		var n int64
		for _, v := range partitionMap(cs.Partition) {
			n += v
		}
		if n != cs.ApprovedTotal {
			t.Errorf("%s: %s partition %d ≠ approved %d", where, ch, n, cs.ApprovedTotal)
		}
		if cs.Uncertain > cs.ApprovedTotal-cs.Sent {
			t.Errorf("%s: %s uncertain %d exceeds non-sent rows", where, ch, cs.Uncertain)
		}
		var outcomes int64
		for _, v := range cs.Attempts.ByOutcome {
			outcomes += v
		}
		if outcomes != cs.Attempts.Total || cs.Attempts.Retries < 0 || cs.Attempts.ByOutcome["accepted"]+cs.Attempts.ByOutcome["replayed"] < cs.Sent {
			t.Errorf("%s: %s attempts %+v vs sent %d", where, ch, cs.Attempts, cs.Sent)
		}
		total.ApprovedTotal += cs.ApprovedTotal
		total.Sent += cs.Sent
	}
	if st.Totals.ApprovedTotal != total.ApprovedTotal || st.Totals.Sent != total.Sent ||
		st.Processed+st.Remaining != st.Totals.ApprovedTotal {
		t.Errorf("%s: totals %+v processed %d remaining %d", where, st.Totals, st.Processed, st.Remaining)
	}
}

// Faults by key: some rows retry after a 5xx, some time out once (the mock
// commits, so the retry is a replay), some fail permanently; one user's
// SMS consent is revoked and one email contact removed after approval.
// /stats, the SSE stream and /deliveries must agree with the committed
// rows at every point, and multi-attempt rows must not inflate delivered
// counts.
func TestProgressReconcilesWithDeliveryRows(t *testing.T) {
	e := newDelEnv(t)
	api := newProgressAPI(e)
	c, s := e.bulkSending(40) // 80 rows
	// Revocations after approval (precheck skips).
	e.exec(`UPDATE channel_consents SET status = 'opted_out' WHERE user_id = 1001 AND channel = 'sms'`)
	e.exec(`UPDATE users SET email = NULL WHERE id = 1002`)
	keyOf := func(user int, ch string) string { return fmt.Sprintf("%s:%d:%s", c.ID, user, ch) }
	transient := map[string]bool{keyOf(1003, "sms"): true, keyOf(1004, "email"): true, keyOf(1005, "sms"): true}
	timeouts := map[string]bool{keyOf(1006, "email"): true}
	permanent := map[string]bool{keyOf(1007, "sms"): true, keyOf(1008, "email"): true}
	e.setFaults(func(r mockprovider.Request) mockprovider.Fault {
		switch {
		case permanent[r.IdempotencyKey]:
			return mockprovider.FaultPermanent
		case transient[r.IdempotencyKey] && r.Arrival == 1:
			return mockprovider.Fault5xx
		case timeouts[r.IdempotencyKey] && r.Arrival == 1:
			return mockprovider.FaultTimeout
		}
		return mockprovider.FaultNone
	})

	before, _ := api.statsOf(c.ID)
	if before.CampaignStatus != "sending" || before.Totals.Pending != 80 || before.Attempts.Total != 0 || before.Terminal ||
		before.ApprovalID == nil || *before.ApprovalID != s.ApprovalID.String() || before.Progress != 0 {
		t.Fatalf("before = %+v", before)
	}
	stream := api.open(c.ID)

	// Paginate continuously while the worker updates rows: ids strictly
	// increase, no row is listed twice, and the union is the snapshot.
	cfg := e.workerConfig("w1")
	cfg.RatePerSecond = 40
	e.start(cfg)
	seen := map[int64]bool{}
	pages := 0
	for {
		cursor := ""
		var last int64
		passIDs := 0
		for {
			pg, _ := api.page(c.ID, "?limit=7"+cursor)
			pages++
			for _, it := range pg.Items {
				if it.ID <= last {
					t.Fatalf("ids not increasing: %d after %d", it.ID, last)
				}
				last = it.ID
				seen[it.ID] = true
				passIDs++
			}
			if pg.NextCursor == nil {
				break
			}
			cursor = "&cursor=" + *pg.NextCursor
		}
		if passIDs != 80 {
			t.Fatalf("one pass listed %d rows, want 80", passIDs)
		}
		if st, _ := e.campaignStatus(c.ID); st != "sending" {
			break
		}
	}
	if len(seen) != 80 || pages < 12 {
		t.Errorf("seen %d ids over %d pages", len(seen), pages)
	}
	if got := e.waitTerminal(c.ID); got != "completed_with_failures" {
		t.Fatalf("terminal = %s", got)
	}
	stream.wait(t)

	final, finalBody := api.statsOf(c.ID)
	db := e.dbPartition(s.ApprovalID)
	for _, ch := range []provider.Channel{provider.ChannelSMS, provider.ChannelEmail} {
		cs := final.Channels[ch]
		if fmt.Sprint(partitionMap(cs.Partition)) != fmt.Sprint(db[string(ch)]) || cs.ApprovedTotal != 40 {
			t.Errorf("%s: stats %v ≠ rows %v", ch, partitionMap(cs.Partition), db[string(ch)])
		}
		var attempts, attempted, retried, uncertain int64
		if err := e.pool.QueryRow(ctxBG, `SELECT
		    (SELECT count(*) FROM delivery_attempts x JOIN campaign_deliveries d ON d.id = x.delivery_id WHERE d.approval_id = $1::uuid AND d.channel = $2::channel),
		    (SELECT count(DISTINCT x.delivery_id) FROM delivery_attempts x JOIN campaign_deliveries d ON d.id = x.delivery_id WHERE d.approval_id = $1::uuid AND d.channel = $2::channel),
		    (SELECT count(*) FROM campaign_deliveries WHERE approval_id = $1::uuid AND channel = $2::channel AND attempts + rate_limited_attempts > 1),
		    (SELECT count(*) FROM campaign_deliveries WHERE approval_id = $1::uuid AND channel = $2::channel AND outcome_uncertain)`,
			s.ApprovalID.String(), string(ch)).Scan(&attempts, &attempted, &retried, &uncertain); err != nil {
			t.Fatal(err)
		}
		if cs.Attempts.Total != attempts || cs.Attempts.Retries != attempts-attempted || cs.Attempts.RetriedDeliveries != retried ||
			cs.Uncertain != uncertain {
			t.Errorf("%s: attempts %+v uncertain %d; db attempts %d attempted %d retried %d uncertain %d",
				ch, cs.Attempts, cs.Uncertain, attempts, attempted, retried, uncertain)
		}
	}
	tot := final.Totals
	// 80 rows: 2 skipped, 2 failed, 76 sent — though 3 of them needed a 5xx retry and 1 a timeout replay.
	if tot.Sent != 76 || tot.Failed != 2 || tot.SkippedConsentRevoked != 1 || tot.SkippedNoContact != 1 || final.Remaining != 0 ||
		final.Processed != 80 || final.Progress != 1 || !final.Terminal || final.Outcome != "completed_with_failures" {
		t.Errorf("final = %+v", final)
	}
	if final.Attempts.Total != 78+4 || final.Attempts.ByOutcome["accepted"] != 75 || final.Attempts.ByOutcome["replayed"] != 1 ||
		final.Attempts.ByOutcome["transient_error"] != 3 || final.Attempts.ByOutcome["timeout"] != 1 ||
		final.Attempts.ByOutcome["permanent_error"] != 2 || final.Attempts.Retries != 4 || final.Attempts.RetriedDeliveries != 4 {
		t.Errorf("attempts = %+v", final.Attempts)
	}
	// Delivery count ≠ provider attempt count ≠ the mock's accepted messages
	// (the replay reused the original message): 76 sent, 82 attempts, 76 messages.
	if n := len(e.mock.Messages()); int64(n) != tot.Sent {
		t.Errorf("provider messages = %d, sent = %d", n, tot.Sent)
	}
	wantReasons := map[string]int64{"failed/destination_rejected": 2, "skipped_consent_revoked/no_consent": 1, "skipped_no_contact/no_contact": 1}
	gotReasons := map[string]int64{}
	for _, cs := range final.Channels {
		for _, r := range cs.Reasons {
			gotReasons[string(r.Status)+"/"+r.Code] += r.Count
		}
	}
	if fmt.Sprint(gotReasons) != fmt.Sprint(wantReasons) {
		t.Errorf("reasons = %v, want %v", gotReasons, wantReasons)
	}
	if *final.ContentVersion != 1 || *final.SegmentVersion != s.SegmentVersion || final.CampaignVersion != c.Version+1 {
		t.Errorf("identity = content v%v segment v%v campaign v%d", *final.ContentVersion, *final.SegmentVersion, final.CampaignVersion)
	}

	// The stream: an initial snapshot, snapshots following commits
	// (processed never decreases), exactly one end after the terminal
	// snapshot, whose numbers equal GET /stats.
	snaps, ends := stream.snapshots(t)
	if ends != 1 || len(snaps) < 3 {
		t.Fatalf("stream: %d snapshots, %d end events", len(snaps), ends)
	}
	if snaps[0].Totals.Pending == 0 && snaps[0].Processed == 0 {
		t.Errorf("initial snapshot = %+v", snaps[0])
	}
	for i, st := range snaps {
		checkIdentities(t, fmt.Sprintf("snapshot %d", i), st)
		if i > 0 && st.Processed < snaps[i-1].Processed {
			t.Errorf("processed decreased: %d → %d", snaps[i-1].Processed, st.Processed)
		}
		if st.Terminal != (i == len(snaps)-1) {
			t.Errorf("snapshot %d terminal = %v", i, st.Terminal)
		}
	}
	last := snaps[len(snaps)-1]
	if fmt.Sprint(last.Totals) != fmt.Sprint(final.Totals) || last.Attempts.Total != final.Attempts.Total {
		t.Errorf("last snapshot %+v ≠ stats %+v", last.Totals, final.Totals)
	}
	if !strings.Contains(stream.rawText(), "retry: 3000\n") || !strings.HasSuffix(strings.TrimSpace(stream.rawText()),
		`"outcome":"completed_with_failures"}`) {
		t.Errorf("stream text tail: %q", tail(stream.rawText(), 200))
	}

	// Reconnect after the end: a fresh snapshot from the database + end.
	again := api.open(c.ID)
	again.wait(t)
	snaps2, ends2 := again.snapshots(t)
	if len(snaps2) != 1 || ends2 != 1 || fmt.Sprint(snaps2[0].Totals) != fmt.Sprint(final.Totals) {
		t.Errorf("reconnect: %d snapshots %d ends", len(snaps2), ends2)
	}

	// Filters over the final rows.
	failed, _ := api.page(c.ID, "?status=failed")
	if len(failed.Items) != 2 || failed.NextCursor != nil {
		t.Errorf("failed page = %+v", failed)
	}
	for _, it := range failed.Items {
		if it.Status != "failed" || it.Reason == nil || *it.Reason != "destination_rejected" || it.DestinationMasked == nil ||
			!strings.Contains(*it.DestinationMasked, "*") || it.Attempts != 1 {
			t.Errorf("failed item = %+v", it)
		}
	}
	emails, _ := api.page(c.ID, "?channel=email&limit=100")
	if len(emails.Items) != 40 {
		t.Errorf("email rows = %d", len(emails.Items))
	}
	if uncertain, _ := api.page(c.ID, "?uncertain=true"); int64(len(uncertain.Items)) != final.Totals.Uncertain {
		t.Errorf("uncertain rows = %d, stats %d", len(uncertain.Items), final.Totals.Uncertain)
	}

	// Privacy: no contact, name, vars, message text or idempotency key in
	// any response or in the stream.
	all, allBody := api.page(c.ID, "?limit=100")
	for _, it := range all.Items {
		if it.DestinationMasked == nil {
			continue
		}
		m := *it.DestinationMasked
		if (it.Channel == provider.ChannelSMS && !strings.HasPrefix(m, "+90********")) ||
			(it.Channel == provider.ChannelEmail && !strings.HasPrefix(m, "b***@")) {
			t.Errorf("destination not masked: %q", m)
		}
	}
	if strings.Contains(string(finalBody), "@") || strings.Contains(stream.rawText(), "@") {
		t.Error("statistics contain an email address fragment")
	}
	texts := map[string]string{"stats": string(finalBody), "deliveries": string(allBody), "stream": stream.rawText()}
	for name, text := range texts {
		// Masked destinations keep only "+90", the last two digits and the
		// email domain (b***@example.test); raw local parts and numbers never appear.
		for _, banned := range []string{"bulk", "+905320", "Bulk", "User", `"user_id"`, `"vars"`, `"idempotency_key"`,
			`"destination"`, c.ID.String() + ":", "Hi Bulk", "your cart", "provider_message_id"} {
			if strings.Contains(text, banned) {
				t.Errorf("%s contains %q", name, banned)
			}
		}
	}
	e.assertNoPII("Hi Bulk")
}

func tail(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[len(s)-n:]
}

// Cancellation mid-send through the API: queued rows are cancelled at once,
// the stream shows the cancelled campaign settling while a request is still
// on the wire, then exactly one end with the final, reconciled counts.
func TestProgressCancellation(t *testing.T) {
	e := newDelEnv(t)
	api := newProgressAPI(e)
	c, s := e.bulkSending(30)
	held := make(chan struct{})
	release := make(chan struct{})
	var holdOnce, releaseOnce sync.Once
	t.Cleanup(func() { releaseOnce.Do(func() { close(release) }) })
	e.setFaults(func(r mockprovider.Request) mockprovider.Fault {
		if r.Channel == provider.ChannelEmail {
			holdOnce.Do(func() { close(held); <-release })
		}
		return mockprovider.FaultNone
	})
	stream := api.open(c.ID)
	cfg := e.workerConfig("w1")
	cfg.RatePerSecond = 10
	e.start(cfg)
	<-held
	e.waitFor("some sms sent", func() bool {
		return e.count(`SELECT count(*) FROM campaign_deliveries WHERE approval_id = $1::uuid AND channel = 'sms' AND status = 'sent'`,
			s.ApprovalID.String()) >= 3
	})
	res, err := http.Post(api.srv.URL+"/api/v1/campaigns/"+c.ID.String()+"/cancel", "application/json",
		strings.NewReader(fmt.Sprintf(`{"expected_version":%d}`, c.Version)))
	if err != nil {
		t.Fatal(err)
	}
	var out struct {
		Status              string           `json:"status"`
		PreviousStatus      string           `json:"previous_status"`
		CancelledDeliveries map[string]int64 `json:"cancelled_deliveries"`
		InFlight            map[string]int64 `json:"in_flight"`
	}
	_ = json.NewDecoder(res.Body).Decode(&out)
	_ = res.Body.Close()
	if res.StatusCode != 200 || out.Status != "cancelled" || out.PreviousStatus != "sending" || out.InFlight["email"] < 1 {
		t.Fatalf("cancel → %d %+v", res.StatusCode, out)
	}
	// While the held request is on the wire: cancelled, settling, not terminal.
	mid, _ := api.statsOf(c.ID)
	if mid.CampaignStatus != "cancelled" || !mid.CampaignTerminal || mid.Terminal || mid.Outcome != "settling" ||
		mid.Totals.InFlight < 1 || mid.Totals.Pending+mid.Totals.RetryScheduled != 0 {
		t.Errorf("mid = %+v", mid)
	}
	// A repeat cancel is invalid_state (whatever the version).
	res, err = http.Post(api.srv.URL+"/api/v1/campaigns/"+c.ID.String()+"/cancel", "application/json",
		strings.NewReader(fmt.Sprintf(`{"expected_version":%d}`, c.Version+1)))
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(res.Body)
	_ = res.Body.Close()
	if res.StatusCode != 409 || !strings.Contains(string(body), `"invalid_state"`) {
		t.Errorf("repeat cancel → %d %s", res.StatusCode, body)
	}
	releaseOnce.Do(func() { close(release) })
	stream.wait(t)
	final, _ := api.statsOf(c.ID)
	db := e.dbPartition(s.ApprovalID)
	for _, ch := range []provider.Channel{provider.ChannelSMS, provider.ChannelEmail} {
		if fmt.Sprint(partitionMap(final.Channels[ch].Partition)) != fmt.Sprint(db[string(ch)]) {
			t.Errorf("%s: stats %v ≠ rows %v", ch, partitionMap(final.Channels[ch].Partition), db[string(ch)])
		}
	}
	if !final.Terminal || final.Outcome != "cancelled" || final.Totals.Sent+final.Totals.Cancelled != 60 || final.Remaining != 0 ||
		final.Totals.Sent < 3 || final.Totals.Cancelled < out.CancelledDeliveries["sms"]+out.CancelledDeliveries["email"] {
		t.Errorf("final = %+v (cancel response %+v)", final, out)
	}
	if int64(len(e.mock.Messages())) != final.Totals.Sent {
		t.Errorf("provider messages %d ≠ sent %d", len(e.mock.Messages()), final.Totals.Sent)
	}
	snaps, ends := stream.snapshots(t)
	if ends != 1 {
		t.Fatalf("end events = %d", ends)
	}
	sawSettling := false
	for i, st := range snaps {
		checkIdentities(t, fmt.Sprintf("snapshot %d", i), st)
		sawSettling = sawSettling || st.Outcome == "settling"
	}
	if !sawSettling || snaps[len(snaps)-1].Outcome != "cancelled" || fmt.Sprint(snaps[len(snaps)-1].Totals) != fmt.Sprint(final.Totals) {
		t.Errorf("stream outcomes: settling seen %v, last %+v", sawSettling, snaps[len(snaps)-1])
	}
}

// Statistics before a send, a due approval expired lazily by a stats
// read, and a campaign cancelled before sending (terminal with no rows).
func TestProgressBeforeSendAndLazyExpiry(t *testing.T) {
	e := newDelEnv(t)
	api := newProgressAPI(e)
	c, s := e.approved()
	st, _ := api.statsOf(c.ID)
	if st.CampaignStatus != "approved" || st.ApprovalID != nil || len(st.Channels) != 0 || st.Terminal || st.Outcome != "not_started" {
		t.Errorf("approved = %+v", st)
	}
	pg, _ := api.page(c.ID, "")
	if pg.ApprovalID != nil || len(pg.Items) != 0 || pg.NextCursor != nil {
		t.Errorf("page before send = %+v", pg)
	}
	e.makeDue(s.ApprovalID)
	st, _ = api.statsOf(c.ID)
	if st.CampaignStatus != "content_ready" || st.CampaignVersion != c.Version+1 {
		t.Errorf("after lazy expiry = %s v%d", st.CampaignStatus, st.CampaignVersion)
	}
	if n := e.count(`SELECT count(*) FROM campaign_deliveries WHERE approval_id = $1::uuid`, s.ApprovalID.String()); n != 0 {
		t.Errorf("held rows after expiry = %d", n)
	}
	if out, err := e.approvals.Cancel(ctxBG, c.ID, st.CampaignVersion); err != nil || out.Campaign.Status != campaign.StatusCancelled {
		t.Fatalf("cancel: %+v %v", out, err)
	}
	stream := api.open(c.ID)
	stream.wait(t)
	snaps, ends := stream.snapshots(t)
	if len(snaps) != 1 || ends != 1 || !snaps[0].Terminal || snaps[0].Outcome != "cancelled" || snaps[0].Totals.ApprovedTotal != 0 {
		t.Errorf("cancelled before send: %+v ends %d", snaps, ends)
	}
	if code, _ := api.get("/api/v1/campaigns/00000000-0000-4000-8000-000000000000/stats"); code != 404 {
		t.Errorf("unknown stats → %d", code)
	}
	if code, _ := api.get("/api/v1/campaigns/00000000-0000-4000-8000-000000000000/deliveries"); code != 404 {
		t.Errorf("unknown deliveries → %d", code)
	}
}

// The heartbeat is absent before any worker ran, fresh while one runs,
// stale once it has stopped for longer than the threshold (simulated by
// ageing the row), and fresh again after a restart. A sending campaign
// with no live worker is reported as such, never as healthy.
func TestWorkerHeartbeatFreshness(t *testing.T) {
	e := newDelEnv(t)
	api := newProgressAPI(e)
	rd, err := api.stats.Readiness(ctxBG)
	if err != nil || rd.Worker.Status != delivery.WorkerAbsent || rd.Worker.AgeSeconds != nil || rd.SendingCampaigns != 0 {
		t.Fatalf("no worker: %+v %v", rd, err)
	}
	c, _ := e.sending()
	st, _ := api.statsOf(c.ID)
	if st.CampaignStatus != "sending" || st.Worker.Status != delivery.WorkerAbsent {
		t.Errorf("sending without worker = %s %+v", st.CampaignStatus, st.Worker)
	}
	if rd, _ := api.stats.Readiness(ctxBG); rd.SendingCampaigns != 1 {
		t.Errorf("sending campaigns = %d", rd.SendingCampaigns)
	}
	stop := e.start(e.workerConfig("hb"))
	var fresh atomic.Bool
	e.waitFor("fresh heartbeat", func() bool {
		rd, err := api.stats.Readiness(ctxBG)
		fresh.Store(err == nil && rd.Worker.Status == delivery.WorkerFresh)
		return fresh.Load() && rd.Worker.FreshProcesses == 1 && *rd.Worker.AgeSeconds <= 15
	})
	// A heartbeat that exists always carries its time (the client requires it).
	if rd, _ := api.stats.Readiness(ctxBG); rd.Worker.LastSeenAt == nil || rd.Worker.AgeSeconds == nil {
		t.Errorf("fresh worker without last_seen_at: %+v", rd.Worker)
	}
	e.waitTerminal(c.ID)
	stop()
	e.exec(`UPDATE worker_heartbeats SET last_seen_at = now() - interval '20 seconds'`)
	rd, err = api.stats.Readiness(ctxBG)
	if err != nil || rd.Worker.Status != delivery.WorkerStale || rd.Worker.FreshProcesses != 0 || *rd.Worker.AgeSeconds < 19 ||
		rd.Worker.LastSeenAt == nil {
		t.Errorf("stopped worker: %+v %v", rd.Worker, err)
	}
	if st, _ := api.statsOf(c.ID); st.Worker.Status != delivery.WorkerStale {
		t.Errorf("stats worker = %+v", st.Worker)
	}
	e.start(e.workerConfig("hb2"))
	e.waitFor("refreshed heartbeat", func() bool {
		rd, err := api.stats.Readiness(ctxBG)
		return err == nil && rd.Worker.Status == delivery.WorkerFresh
	})
}
