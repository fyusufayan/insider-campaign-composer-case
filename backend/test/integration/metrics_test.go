//go:build integration

package integration

import (
	"context"
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

	"github.com/fyusufayan/insider-campaign-composer-case/backend/internal/approval"
	"github.com/fyusufayan/insider-campaign-composer-case/backend/internal/campaign"
	"github.com/fyusufayan/insider-campaign-composer-case/backend/internal/delivery"
	"github.com/fyusufayan/insider-campaign-composer-case/backend/internal/mockprovider"
	"github.com/fyusufayan/insider-campaign-composer-case/backend/internal/platform/logging"
	"github.com/fyusufayan/insider-campaign-composer-case/backend/internal/platform/metrics"
	"github.com/fyusufayan/insider-campaign-composer-case/backend/internal/platform/metrics/metricstest"
	"github.com/fyusufayan/insider-campaign-composer-case/backend/internal/provider"
	"github.com/fyusufayan/insider-campaign-composer-case/backend/internal/segment"
	"github.com/fyusufayan/insider-campaign-composer-case/backend/test/integration/testsupport"
)

// ---- CP17B: /metrics (ADR-026) against real PostgreSQL, the real worker
// and the mock provider over TCP.

type metricsEndpoint struct {
	t   *testing.T
	srv *httptest.Server
}

func serveMetrics(t *testing.T, reg *metrics.Registry) *metricsEndpoint {
	srv := httptest.NewServer(reg.Handler())
	t.Cleanup(srv.Close)
	return &metricsEndpoint{t: t, srv: srv}
}

func (m *metricsEndpoint) scrape() (*metricstest.Exposition, string) {
	m.t.Helper()
	res, err := http.Get(m.srv.URL + "/metrics")
	if err != nil {
		m.t.Fatal(err)
	}
	defer res.Body.Close()
	body, _ := io.ReadAll(res.Body)
	if res.StatusCode != 200 || res.Header.Get("Content-Type") != metrics.ContentType {
		m.t.Fatalf("scrape → %d %q", res.StatusCode, res.Header.Get("Content-Type"))
	}
	e, err := metricstest.Parse(string(body))
	if err != nil {
		m.t.Fatalf("invalid exposition: %v\n%s", err, body)
	}
	return e, string(body)
}

func (e *delEnv) metricsWorker(id string, reg *metrics.Registry) delivery.Config {
	cfg := e.workerConfig(id)
	cfg.Metrics = delivery.NewMetrics(metrics.NewDelivery(reg))
	cfg.QueueSampleInterval = 50 * time.Millisecond
	return cfg
}

// Counters and histograms follow committed outcomes exactly — retries never
// double-count a delivery — and the queue gauge follows committed state,
// across concurrent scrapes and a worker restart.
func TestWorkerMetricsReconcileWithRows(t *testing.T) {
	e := newDelEnv(t)
	c, s := e.bulkSending(30) // 60 rows
	e.exec(`UPDATE channel_consents SET status = 'opted_out' WHERE user_id = 1001 AND channel = 'sms'`)
	key := func(u int, ch string) string { return fmt.Sprintf("%s:%d:%s", c.ID, u, ch) }
	transient := map[string]bool{key(1003, "sms"): true, key(1004, "email"): true, key(1005, "email"): true}
	limited := map[string]bool{key(1006, "sms"): true, key(1007, "email"): true}
	permanent := map[string]bool{key(1008, "sms"): true}
	e.setFaults(func(r mockprovider.Request) mockprovider.Fault {
		switch {
		case permanent[r.IdempotencyKey]:
			return mockprovider.FaultPermanent
		case transient[r.IdempotencyKey] && r.Arrival == 1:
			return mockprovider.Fault5xx
		case limited[r.IdempotencyKey] && r.Arrival == 1:
			return mockprovider.Fault429
		}
		return mockprovider.FaultNone
	})

	// Hold the dispatch lock: the worker stands by, its sampler still runs.
	held, err := e.repo.TryDispatchLock(ctxBG)
	if err != nil || held == nil {
		t.Fatalf("dispatch lock: %v", err)
	}
	reg := metrics.NewRegistry()
	ep := serveMetrics(t, reg)
	stop := e.start(e.metricsWorker("m1", reg))
	e.waitFor("queue depth sample", func() bool {
		x, _ := ep.scrape()
		v1, ok1 := x.Value("delivery_queue_depth", "channel", "sms", "status", "pending")
		v2, ok2 := x.Value("delivery_queue_depth", "channel", "email", "status", "pending")
		return ok1 && ok2 && v1 == 30 && v2 == 30
	})
	if x, _ := ep.scrape(); x.Sum("deliveries_total") != 0 || x.Sum("provider_request_duration_seconds_count") != 0 {
		t.Fatalf("metrics moved before any dispatch")
	}

	// Concurrent scrapes while the worker delivers: always valid, counters monotonic.
	done := make(chan struct{})
	var scrapes atomic.Int64
	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			var last float64
			for {
				select {
				case <-done:
					return
				default:
				}
				res, err := http.Get(ep.srv.URL + "/metrics")
				if err != nil {
					t.Error(err)
					return
				}
				body, _ := io.ReadAll(res.Body)
				_ = res.Body.Close()
				x, err := metricstest.Parse(string(body))
				if err != nil {
					t.Error(err)
					return
				}
				if v := x.Sum("deliveries_total"); v < last {
					t.Errorf("deliveries_total decreased: %v → %v", last, v)
				} else {
					last = v
				}
				scrapes.Add(1)
			}
		}()
	}
	if err := held.Close(ctxBG); err != nil {
		t.Fatal(err)
	}
	if got := e.waitTerminal(c.ID); got != "completed_with_failures" {
		t.Fatalf("terminal = %s", got)
	}
	e.waitFor("queue drained in the sample", func() bool {
		x, _ := ep.scrape()
		return x.SeriesOf("delivery_queue_depth") == 6 && x.Sum("delivery_queue_depth") == 0
	})
	close(done)
	wg.Wait()
	if scrapes.Load() < 4 {
		t.Errorf("only %d concurrent scrapes", scrapes.Load())
	}

	x, text := ep.scrape()
	// deliveries_total = the committed terminal partition, once per row.
	db := e.dbPartition(s.ApprovalID)
	var total float64
	for _, ch := range []string{"sms", "email"} {
		for _, out := range metrics.DeliveryOutcomes {
			v, ok := x.Value("deliveries_total", "channel", ch, "outcome", out)
			if !ok || v != float64(db[ch][out]) {
				t.Errorf("deliveries_total{%s,%s} = %v, rows %d", ch, out, v, db[ch][out])
			}
			total += v
		}
		var attempts, rateLimited int64
		if err := e.pool.QueryRow(ctxBG, `SELECT count(*), count(*) FILTER (WHERE x.outcome = 'rate_limited')
		    FROM delivery_attempts x JOIN campaign_deliveries d ON d.id = x.delivery_id
		    WHERE d.approval_id = $1::uuid AND d.channel = $2::channel`, s.ApprovalID.String(), ch).Scan(&attempts, &rateLimited); err != nil {
			t.Fatal(err)
		}
		arrivals := e.mock.Stats().Channels[provider.Channel(ch)].Arrivals
		if v, _ := x.Value("provider_request_duration_seconds_count", "channel", ch); v != float64(attempts) || int64(v) != arrivals {
			t.Errorf("%s: duration count %v, attempt rows %d, provider arrivals %d", ch, v, attempts, arrivals)
		}
		if v, _ := x.Value("provider_rate_limited_total", "channel", ch); v != float64(rateLimited) {
			t.Errorf("%s: rate limited %v, rows %d", ch, v, rateLimited)
		}
	}
	if total != 60 || x.Sum("provider_rate_limited_total") != 2 || x.Sum("provider_request_duration_seconds_count") != 59+5 {
		t.Errorf("total %v, 429s %v, requests %v", total, x.Sum("provider_rate_limited_total"), x.Sum("provider_request_duration_seconds_count"))
	}
	if v, _ := x.Value("deliveries_total", "channel", "sms", "outcome", "skipped_consent_revoked"); v != 1 {
		t.Errorf("skip not counted: %v", v)
	}
	// No customer data, ids or free text in the exposition.
	for _, banned := range []string{c.ID.String(), s.ApprovalID.String(), "+90532", "@example.test", "bulk", "Bulk", "Hi ", "campaign_id",
		"user", "destination", "idempotency"} {
		if strings.Contains(text, banned) {
			t.Errorf("exposition contains %q", banned)
		}
	}

	// Restart: a new process starts from zero (no invented history) and
	// samples the committed queue again.
	stop()
	reg2 := metrics.NewRegistry()
	ep2 := serveMetrics(t, reg2)
	e.start(e.metricsWorker("m2", reg2))
	e.waitFor("sample after restart", func() bool {
		x, _ := ep2.scrape()
		return x.SeriesOf("delivery_queue_depth") == 6
	})
	if x, _ := ep2.scrape(); x.Sum("deliveries_total") != 0 || x.Sum("provider_request_duration_seconds_count") != 0 || x.Sum("delivery_queue_depth") != 0 {
		t.Errorf("restarted process reports history")
	}
}

// An empty database is sampled as zero; a database outage clears the gauge
// (never stale values) while /metrics keeps answering.
func TestWorkerMetricsEmptyDatabaseAndOutage(t *testing.T) {
	pool := testsupport.Pool(t, pg.NewMigratedDatabase(t))
	mock, err := mockprovider.New(mockprovider.Config{}, logging.New(io.Discard, slog.LevelInfo))
	if err != nil {
		t.Fatal(err)
	}
	hs := httptest.NewServer(mock.Handler())
	t.Cleanup(hs.Close)
	client, err := provider.NewClient(provider.ClientConfig{BaseURL: hs.URL, RequestTimeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	reg := metrics.NewRegistry()
	ep := serveMetrics(t, reg)
	e := &delEnv{prepEnv: &prepEnv{t: t, pool: pool, logs: &lockedBuffer{}}}
	cfg := e.metricsWorker("empty", reg)
	w, err := delivery.New(delivery.NewRepository(pool), client, cfg, logging.New(io.Discard, slog.LevelInfo))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	runDone := make(chan error, 1)
	go func() { runDone <- w.Run(ctx) }()
	t.Cleanup(func() { cancel(); <-runDone })

	e.waitFor("empty sample", func() bool {
		x, _ := ep.scrape()
		return x.SeriesOf("delivery_queue_depth") == 6
	})
	x, _ := ep.scrape()
	if x.Sum("delivery_queue_depth") != 0 || x.Sum("deliveries_total") != 0 || x.SeriesOf("deliveries_total") != 10 {
		t.Errorf("empty database: %v", x.Samples)
	}
	pool.Close() // database unreachable for this process
	e.waitFor("gauge cleared after a failed sample", func() bool {
		x, _ := ep.scrape()
		return x.SeriesOf("delivery_queue_depth") == 0 && x.SeriesOf("deliveries_total") == 10
	})
}

// Queued rows cancelled by the API's cancel transaction are counted by the
// API process (deliveries_total{outcome="cancelled"}); the worker then has
// nothing to count for them.
func TestAPICancelCountsDeliveries(t *testing.T) {
	e := newDelEnv(t)
	reg := metrics.NewRegistry()
	counter := metrics.NewDeliveriesTotal(reg)
	svc, err := approval.NewService(approval.NewRepository(e.pool), approval.Config{PendingTTL: prepTTL, ApprovedTTL: apiApprovedTTL,
		DeliveriesCancelled: func(ch segment.Channel, n int64) { counter.Add(float64(n), string(ch), "cancelled") }},
		logging.New(e.logs, slog.LevelInfo))
	if err != nil {
		t.Fatal(err)
	}
	c, s := e.bulkSending(20)
	out, err := svc.Cancel(ctxBG, c.ID, c.Version)
	if err != nil || out.Campaign.Status != campaign.StatusCancelled {
		t.Fatalf("cancel: %+v %v", out, err)
	}
	ep := serveMetrics(t, reg)
	x, _ := ep.scrape()
	db := e.dbPartition(s.ApprovalID)
	for _, ch := range []string{"sms", "email"} {
		if v, _ := x.Value("deliveries_total", "channel", ch, "outcome", "cancelled"); v != float64(db[ch]["cancelled"]) || v != 20 {
			t.Errorf("%s cancelled = %v, rows %d", ch, v, db[ch]["cancelled"])
		}
	}
	// A repeat (409) counts nothing.
	if _, err := svc.Cancel(ctxBG, c.ID, out.Campaign.Version); err == nil {
		t.Fatal("repeat cancel succeeded")
	}
	if x, _ := ep.scrape(); x.Sum("deliveries_total") != 40 {
		t.Errorf("after repeat: %v", x.Sum("deliveries_total"))
	}
}
