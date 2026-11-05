package delivery

import (
	"bytes"
	"testing"
	"time"

	"github.com/fyusufayan/insider-campaign-composer-case/backend/internal/platform/metrics"
	"github.com/fyusufayan/insider-campaign-composer-case/backend/internal/platform/metrics/metricstest"
	"github.com/fyusufayan/insider-campaign-composer-case/backend/internal/provider"
)

func TestPromMetricsAdapter(t *testing.T) {
	r := metrics.NewRegistry()
	m := NewMetrics(metrics.NewDelivery(r))
	m.Finished(provider.ChannelSMS, StatusSent, 1)
	m.Finished(provider.ChannelSMS, StatusSent, 2)
	m.Finished(provider.ChannelSMS, StatusRetryScheduled, 1) // not terminal: never counted
	m.Finished(provider.ChannelEmail, StatusPending, 5)
	m.Finished(provider.ChannelEmail, StatusCancelled, 0)
	m.ProviderResponse(provider.ChannelSMS, provider.Result{Outcome: provider.OutcomeRateLimited, HTTPStatus: 429, Duration: 30 * time.Millisecond})
	m.ProviderResponse(provider.ChannelSMS, provider.Result{Outcome: provider.OutcomeAccepted, Duration: 2 * time.Second})
	// Our own request could not be built: nothing was issued, nothing observed.
	m.ProviderResponse(provider.ChannelSMS, provider.Result{Outcome: provider.OutcomePermanentError, ErrorCode: "invalid_request"})
	m.QueueDepth(map[provider.Channel]map[Status]int64{provider.ChannelEmail: {StatusPending: 7}}, true)
	var b bytes.Buffer
	r.WriteText(&b)
	e, err := metricstest.Parse(b.String())
	if err != nil {
		t.Fatal(err)
	}
	check := func(name string, want float64, kv ...string) {
		t.Helper()
		if v, ok := e.Value(name, kv...); !ok || v != want {
			t.Errorf("%s%v = %v (%v), want %v", name, kv, v, ok, want)
		}
	}
	check("deliveries_total", 3, "channel", "sms", "outcome", "sent")
	if e.Sum("deliveries_total") != 3 {
		t.Errorf("non-terminal transitions counted: %v", e.Sum("deliveries_total"))
	}
	check("provider_rate_limited_total", 1, "channel", "sms")
	check("provider_rate_limited_total", 0, "channel", "email")
	check("provider_request_duration_seconds_count", 2, "channel", "sms")
	check("provider_request_duration_seconds_bucket", 1, "channel", "sms", "le", "0.05")
	check("delivery_queue_depth", 7, "channel", "email", "status", "pending")
	check("delivery_queue_depth", 0, "channel", "sms", "status", "in_flight")
	m.QueueDepth(nil, false) // failed sample: no stale values
	b.Reset()
	r.WriteText(&b)
	if e, _ = metricstest.Parse(b.String()); e.SeriesOf("delivery_queue_depth") != 0 {
		t.Error("stale queue depth exposed after a failed sample")
	}
	var nop nopMetrics
	nop.Finished(provider.ChannelSMS, StatusSent, 1)
	nop.ProviderResponse(provider.ChannelSMS, provider.Result{})
	nop.QueueDepth(nil, true)
}
