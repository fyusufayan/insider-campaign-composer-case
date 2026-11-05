package httpapi

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/fyusufayan/insider-campaign-composer-case/backend/internal/delivery"
	"github.com/fyusufayan/insider-campaign-composer-case/backend/internal/provider"
)

const testCampaignID = "6f1c2a8e-1b2c-4d3e-8f90-0a1b2c3d4e5f"

// fakeStats is a scripted statistics source: Stats returns the current
// snapshot (or error); List records its query.
type fakeStats struct {
	mu      sync.Mutex
	current delivery.CampaignStats
	err     error
	reads   int
	lastQ   delivery.ListQuery
	page    delivery.Page
}

func (f *fakeStats) Stats(ctx context.Context, id string) (delivery.CampaignStats, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.reads++
	if f.err != nil {
		return delivery.CampaignStats{}, f.err
	}
	st := f.current
	st.CampaignID = id
	return st, ctx.Err()
}

func (f *fakeStats) List(_ context.Context, id string, q delivery.ListQuery) (delivery.Page, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.lastQ = q
	if f.err != nil {
		return delivery.Page{}, f.err
	}
	p := f.page
	p.CampaignID = id
	return p, nil
}

func (f *fakeStats) set(fn func(*delivery.CampaignStats)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	fn(&f.current)
}

func (f *fakeStats) setErr(err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.err = err
}

func sendingStats() delivery.CampaignStats {
	sms := delivery.ChannelStats{Partition: delivery.Partition{ApprovedTotal: 3, Pending: 3}, Reasons: []delivery.Reason{}}
	return delivery.CampaignStats{CampaignStatus: "sending", CampaignVersion: 5, Outcome: delivery.OutcomeSending,
		Channels: map[provider.Channel]delivery.ChannelStats{provider.ChannelSMS: sms},
		Totals:   sms.Partition, Remaining: 3, Worker: delivery.ClassifyHeartbeat(nil, 0), UpdatedAt: time.Now().UTC()}
}

// manualTicks hands out one channel per period; the test drives them.
type manualTicks struct {
	mu sync.Mutex
	ch map[time.Duration]chan time.Time
}

func newManualTicks() *manualTicks { return &manualTicks{ch: map[time.Duration]chan time.Time{}} }

func (m *manualTicks) get(d time.Duration) chan time.Time {
	m.mu.Lock()
	defer m.mu.Unlock()
	c, ok := m.ch[d]
	if !ok {
		c = make(chan time.Time)
		m.ch[d] = c
	}
	return c
}

func (m *manualTicks) NewTicker(d time.Duration) (<-chan time.Time, func()) {
	return m.get(d), func() {}
}

// tick delivers one tick (the handler must be waiting for it).
func (m *manualTicks) tick(t *testing.T, d time.Duration) {
	t.Helper()
	select {
	case m.get(d) <- time.Now():
	case <-time.After(5 * time.Second):
		t.Fatalf("handler did not take the %v tick", d)
	}
}

const (
	tPoll  = time.Second
	tBeat  = 15 * time.Second
	tLife  = 10 * time.Minute
	tRetry = 3 * time.Second
)

func newDeliveryTestServer(t *testing.T, stats *fakeStats, cfg EventsConfig) (*httptest.Server, *deliveryHandlers) {
	t.Helper()
	log, _ := newTestLogger()
	if cfg.Poll == 0 {
		cfg.Poll, cfg.Heartbeat, cfg.MaxLifetime, cfg.Retry = tPoll, tBeat, tLife, tRetry
	}
	h := newDeliveryHandlers(&campaignHandlers{log: log}, stats, cfg)
	mux := http.NewServeMux()
	h.register(mux)
	srv := httptest.NewServer(requestID(mux))
	t.Cleanup(srv.Close)
	return srv, h
}

type progressEvent struct {
	name, data string
	comment    string
	retry      string
}

// sseReader reads one SSE block (until a blank line) at a time, with a deadline.
type sseReader struct {
	t     *testing.T
	lines chan string
	errs  chan error
}

func newSSEReader(t *testing.T, body io.Reader) *sseReader {
	r := &sseReader{t: t, lines: make(chan string, 64), errs: make(chan error, 1)}
	go func() {
		sc := bufio.NewScanner(body)
		for sc.Scan() {
			r.lines <- sc.Text()
		}
		r.errs <- io.EOF
		close(r.lines)
	}()
	return r
}

func (r *sseReader) next() (progressEvent, bool) {
	r.t.Helper()
	var ev progressEvent
	got := false
	for {
		select {
		case line, ok := <-r.lines:
			if !ok {
				return ev, false
			}
			switch {
			case line == "":
				if got {
					return ev, true
				}
			case strings.HasPrefix(line, ":"):
				ev.comment, got = strings.TrimSpace(line[1:]), true
			case strings.HasPrefix(line, "event: "):
				ev.name, got = line[len("event: "):], true
			case strings.HasPrefix(line, "data: "):
				ev.data, got = line[len("data: "):], true
			case strings.HasPrefix(line, "retry: "):
				ev.retry, got = line[len("retry: "):], true
			default:
				r.t.Fatalf("unexpected SSE line %q", line)
			}
		case <-time.After(5 * time.Second):
			r.t.Fatal("no SSE block within 5 s")
		}
	}
}

func (r *sseReader) expectEOF() {
	r.t.Helper()
	if ev, ok := r.next(); ok {
		r.t.Fatalf("stream continued with %+v", ev)
	}
}

func openStream(t *testing.T, srv *httptest.Server, ctx context.Context) (*http.Response, *sseReader) {
	t.Helper()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL+"/api/v1/campaigns/"+testCampaignID+"/events", nil)
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = res.Body.Close() })
	return res, newSSEReader(t, res.Body)
}

func statsOf(t *testing.T, ev progressEvent) delivery.CampaignStats {
	t.Helper()
	if ev.name != "stats" {
		t.Fatalf("event = %+v, want stats", ev)
	}
	var st delivery.CampaignStats
	if err := json.Unmarshal([]byte(ev.data), &st); err != nil {
		t.Fatal(err)
	}
	return st
}

func TestEventsStreamLifecycle(t *testing.T) {
	stats := &fakeStats{current: sendingStats()}
	ticks := newManualTicks()
	srv, h := newDeliveryTestServer(t, stats, EventsConfig{Poll: tPoll, Heartbeat: tBeat, MaxLifetime: tLife, Retry: tRetry, NewTicker: ticks.NewTicker})
	res, r := openStream(t, srv, context.Background())
	if res.StatusCode != 200 || res.Header.Get("Content-Type") != "text/event-stream; charset=utf-8" ||
		res.Header.Get("Cache-Control") != "no-cache" || res.Header.Get("X-Accel-Buffering") != "no" {
		t.Fatalf("response %d %v", res.StatusCode, res.Header)
	}
	if ev, _ := r.next(); ev.retry != "3000" {
		t.Fatalf("first block = %+v, want retry", ev)
	}
	// 1. Fresh initial snapshot.
	if st := statsOf(t, mustNext(r)); st.Totals.Pending != 3 || st.CampaignID != testCampaignID || st.Terminal {
		t.Fatalf("initial = %+v", st)
	}
	// 2. An unchanged poll emits nothing (the next block is the heartbeat);
	// a changed heartbeat age alone is not a change either.
	stats.set(func(s *delivery.CampaignStats) {
		age := 3.0
		s.Worker.AgeSeconds = &age
		s.UpdatedAt = s.UpdatedAt.Add(time.Second)
	})
	ticks.tick(t, tPoll)
	ticks.tick(t, tBeat)
	if ev := mustNext(r); ev.comment != "keep-alive" {
		t.Fatalf("after unchanged poll: %+v", ev)
	}
	// 3. Committed progress → a new snapshot.
	stats.set(func(s *delivery.CampaignStats) {
		s.Totals.Pending, s.Totals.Sent, s.Processed, s.Remaining = 1, 2, 2, 1
	})
	ticks.tick(t, tPoll)
	if st := statsOf(t, mustNext(r)); st.Totals.Sent != 2 || st.Remaining != 1 {
		t.Fatalf("progress = %+v", st)
	}
	// 4. Cancelled but rows still in flight: not terminal yet.
	stats.set(func(s *delivery.CampaignStats) {
		s.CampaignStatus, s.CampaignTerminal, s.Outcome, s.CampaignVersion = "cancelled", true, delivery.OutcomeSettling, 6
	})
	ticks.tick(t, tPoll)
	if st := statsOf(t, mustNext(r)); st.Terminal || st.Outcome != delivery.OutcomeSettling {
		t.Fatalf("settling = %+v", st)
	}
	// 5. Terminal snapshot, then exactly one end event, then EOF.
	stats.set(func(s *delivery.CampaignStats) {
		s.Terminal, s.Outcome, s.Remaining = true, delivery.OutcomeCancelled, 0
	})
	ticks.tick(t, tPoll)
	if st := statsOf(t, mustNext(r)); !st.Terminal {
		t.Fatalf("terminal = %+v", st)
	}
	end := mustNext(r)
	var ed endEventDTO
	if end.name != "end" || json.Unmarshal([]byte(end.data), &ed) != nil ||
		ed != (endEventDTO{CampaignID: testCampaignID, CampaignStatus: "cancelled", CampaignVersion: 6, Outcome: "cancelled"}) {
		t.Fatalf("end = %+v", end)
	}
	r.expectEOF()
	waitStreams(t, h, 0)
}

func mustNext(r *sseReader) progressEvent {
	r.t.Helper()
	ev, ok := r.next()
	if !ok {
		r.t.Fatal("stream ended")
	}
	return ev
}

func waitStreams(t *testing.T, h *deliveryHandlers, want int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	tick := time.NewTicker(5 * time.Millisecond) // deadline-bounded polling of handler state
	defer tick.Stop()
	for len(h.streams) != want {
		if time.Now().After(deadline) {
			t.Fatalf("open streams = %d, want %d", len(h.streams), want)
		}
		<-tick.C
	}
}

func TestEventsTerminalAtConnect(t *testing.T) {
	st := sendingStats()
	st.CampaignStatus, st.CampaignTerminal, st.Terminal, st.Outcome = "completed", true, true, delivery.OutcomeCompleted
	srv, _ := newDeliveryTestServer(t, &fakeStats{current: st}, EventsConfig{NewTicker: newManualTicks().NewTicker})
	_, r := openStream(t, srv, context.Background())
	var names []string
	for {
		ev, ok := r.next()
		if !ok {
			break
		}
		if ev.name != "" {
			names = append(names, ev.name)
		}
	}
	if strings.Join(names, ",") != "stats,end" {
		t.Fatalf("events = %v (a reconnect to a finished campaign gets one snapshot and one end)", names)
	}
}

func TestEventsClientDisconnectEndsHandler(t *testing.T) {
	ticks := newManualTicks()
	srv, h := newDeliveryTestServer(t, &fakeStats{current: sendingStats()}, EventsConfig{Poll: tPoll, Heartbeat: tBeat,
		MaxLifetime: tLife, NewTicker: ticks.NewTicker})
	ctx, cancel := context.WithCancel(context.Background())
	_, r := openStream(t, srv, ctx)
	mustNext(r)
	statsOf(t, mustNext(r))
	waitStreams(t, h, 1)
	cancel()
	waitStreams(t, h, 0)
}

func TestEventsReadFailuresAndLifetimeClose(t *testing.T) {
	stats := &fakeStats{current: sendingStats()}
	ticks := newManualTicks()
	srv, _ := newDeliveryTestServer(t, stats, EventsConfig{Poll: tPoll, Heartbeat: tBeat, MaxLifetime: tLife, MaxReadFailures: 3,
		NewTicker: ticks.NewTicker})
	_, r := openStream(t, srv, context.Background())
	mustNext(r)
	statsOf(t, mustNext(r))
	stats.setErr(errors.New("db down"))
	for i := 0; i < 3; i++ {
		ticks.tick(t, tPoll)
	}
	r.expectEOF() // closed without "end": the client reconnects

	stats.setErr(nil)
	_, r2 := openStream(t, srv, context.Background())
	mustNext(r2)
	statsOf(t, mustNext(r2))
	ticks.tick(t, tLife)
	r2.expectEOF()
}

func TestEventsErrorsBeforeStream(t *testing.T) {
	stats := &fakeStats{err: delivery.ErrCampaignNotFound}
	srv, _ := newDeliveryTestServer(t, stats, EventsConfig{NewTicker: newManualTicks().NewTicker, MaxStreams: 1})
	res, err := http.Get(srv.URL + "/api/v1/campaigns/" + testCampaignID + "/events")
	if err != nil {
		t.Fatal(err)
	}
	_ = res.Body.Close()
	if res.StatusCode != 404 || !strings.HasPrefix(res.Header.Get("Content-Type"), "application/json") {
		t.Errorf("unknown campaign → %d %s", res.StatusCode, res.Header.Get("Content-Type"))
	}
	for path, want := range map[string]int{"/api/v1/campaigns/nope/events": 422, "/api/v1/campaigns/" + testCampaignID + "/events?x=1": 422} {
		res, err := http.Get(srv.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		_ = res.Body.Close()
		if res.StatusCode != want {
			t.Errorf("%s → %d", path, res.StatusCode)
		}
	}
	// Stream limit.
	stats.setErr(nil)
	stats.set(func(s *delivery.CampaignStats) { *s = sendingStats() })
	_, r := openStream(t, srv, context.Background())
	mustNext(r)
	statsOf(t, mustNext(r))
	res, err = http.Get(srv.URL + "/api/v1/campaigns/" + testCampaignID + "/events")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(res.Body)
	_ = res.Body.Close()
	if res.StatusCode != 503 || !strings.Contains(string(body), `"too_many_streams"`) || !strings.Contains(string(body), `"retryable":true`) {
		t.Errorf("limit → %d %s", res.StatusCode, body)
	}
}

func TestStatsEndpoint(t *testing.T) {
	stats := &fakeStats{current: sendingStats()}
	srv, _ := newDeliveryTestServer(t, stats, EventsConfig{})
	get := func(path string) (int, map[string]any) {
		res, err := http.Get(srv.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		defer res.Body.Close()
		var m map[string]any
		_ = json.NewDecoder(res.Body).Decode(&m)
		return res.StatusCode, m
	}
	code, body := get("/api/v1/campaigns/" + testCampaignID + "/stats")
	if code != 200 || body["campaign_id"] != testCampaignID || body["campaign_status"] != "sending" {
		t.Fatalf("stats → %d %v", code, body)
	}
	for _, k := range []string{"channels", "totals", "attempts", "processed", "remaining", "progress", "terminal", "campaign_terminal",
		"outcome", "worker", "updated_at", "approval_id", "content_version", "segment_version", "campaign_version"} {
		if _, ok := body[k]; !ok {
			t.Errorf("stats lacks %s", k)
		}
	}
	if code, _ := get("/api/v1/campaigns/" + testCampaignID + "/stats?limit=1"); code != 422 {
		t.Errorf("query → %d", code)
	}
	if code, _ := get("/api/v1/campaigns/x/stats"); code != 422 {
		t.Errorf("bad id → %d", code)
	}
	stats.setErr(delivery.ErrCampaignNotFound)
	if code, body := get("/api/v1/campaigns/" + testCampaignID + "/stats"); code != 404 || body["error"].(map[string]any)["code"] != "not_found" {
		t.Errorf("unknown → %d %v", code, body)
	}
	stats.setErr(delivery.ErrPartitionMismatch)
	if code, body := get("/api/v1/campaigns/" + testCampaignID + "/stats"); code != 500 || body["error"].(map[string]any)["code"] != "internal" {
		t.Errorf("mismatch → %d %v", code, body)
	}
}

func TestDeliveriesQueryValidation(t *testing.T) {
	stats := &fakeStats{page: delivery.Page{Items: []delivery.Item{}, Limit: 50}}
	srv, _ := newDeliveryTestServer(t, stats, EventsConfig{})
	base := srv.URL + "/api/v1/campaigns/" + testCampaignID + "/deliveries"
	ok := map[string]func(q delivery.ListQuery) bool{
		"": func(q delivery.ListQuery) bool {
			return q.Limit == 50 && q.After == 0 && q.Status == nil && q.Channel == nil && q.Uncertain == nil
		},
		"?status=failed": func(q delivery.ListQuery) bool { return q.Status != nil && *q.Status == delivery.StatusFailed },
		"?channel=email&limit=1": func(q delivery.ListQuery) bool {
			return q.Channel != nil && *q.Channel == provider.ChannelEmail && q.Limit == 1
		},
		"?uncertain=true":      func(q delivery.ListQuery) bool { return q.Uncertain != nil && *q.Uncertain },
		"?uncertain=false":     func(q delivery.ListQuery) bool { return q.Uncertain != nil && !*q.Uncertain },
		"?cursor=42&limit=100": func(q delivery.ListQuery) bool { return q.After == 42 && q.Limit == 100 },
	}
	for qs, check := range ok {
		res, err := http.Get(base + qs)
		if err != nil {
			t.Fatal(err)
		}
		_ = res.Body.Close()
		stats.mu.Lock()
		q := stats.lastQ
		stats.mu.Unlock()
		if res.StatusCode != 200 || !check(q) {
			t.Errorf("%q → %d %+v", qs, res.StatusCode, q)
		}
	}
	bad := []string{"?status=held", "?status=SENT", "?status=failed&status=sent", "?channel=push", "?uncertain=1",
		"?cursor=0", "?cursor=-1", "?cursor=abc", "?cursor=99999999999999999999", "?limit=0", "?limit=101", "?limit=+5", "?limit=05",
		"?limit=1.5", "?user_id=1", "?destination=x", "?%zz"}
	for _, qs := range bad {
		res, err := http.Get(base + qs)
		if err != nil {
			t.Fatal(err)
		}
		body, _ := io.ReadAll(res.Body)
		_ = res.Body.Close()
		if res.StatusCode != 422 || !strings.Contains(string(body), `"validation_failed"`) {
			t.Errorf("%q → %d %s", qs, res.StatusCode, body)
		}
	}
}

func TestReadyzReportsWorker(t *testing.T) {
	log, _ := newTestLogger()
	n := int64(1)
	router := NewRouter(log, Deps{Readiness: func(context.Context) (ReadinessReport, error) {
		return ReadinessReport{Database: "ok", LLMProvider: "demo", Worker: delivery.ClassifyHeartbeat(nil, 0), SendingCampaigns: &n}, nil
	}})
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/readyz", nil))
	var body map[string]any
	_ = json.Unmarshal(rr.Body.Bytes(), &body)
	w, _ := body["worker"].(map[string]any)
	// An absent worker is reported, but the API stays ready (§15).
	if rr.Code != 200 || body["status"] != "ready" || w["status"] != "absent" || w["stale_after_seconds"] != float64(15) ||
		body["sending_campaigns"] != float64(1) {
		t.Errorf("readyz → %d %s", rr.Code, rr.Body)
	}
}
