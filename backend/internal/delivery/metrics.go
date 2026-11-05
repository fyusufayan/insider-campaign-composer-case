package delivery

import (
	"github.com/fyusufayan/insider-campaign-composer-case/backend/internal/platform/metrics"
	"github.com/fyusufayan/insider-campaign-composer-case/backend/internal/provider"
)

// Metrics receives the worker's instrumentation events (ARCHITECTURE §15).
// Calls are in-memory and cheap; none happens between the precheck and the
// provider request, and none performs I/O.
type Metrics interface {
	// Finished: n rows of channel ch were moved to terminal status s by a
	// transaction of this process that committed (counted once per row).
	Finished(ch provider.Channel, s Status, n int)
	// ProviderResponse: a provider request was issued and answered/failed.
	ProviderResponse(ch provider.Channel, res provider.Result)
	// QueueDepth: a sample of released, non-terminal rows (ok = false: the
	// sample failed and the previous one must not be presented as current).
	QueueDepth(depth map[provider.Channel]map[Status]int64, ok bool)
}

type nopMetrics struct{}

func (nopMetrics) Finished(provider.Channel, Status, int)                 {}
func (nopMetrics) ProviderResponse(provider.Channel, provider.Result)     {}
func (nopMetrics) QueueDepth(map[provider.Channel]map[Status]int64, bool) {}

// NewMetrics adapts the registered core delivery metrics.
func NewMetrics(m *metrics.Delivery) Metrics { return promMetrics{m} }

type promMetrics struct{ m *metrics.Delivery }

func (p promMetrics) Finished(ch provider.Channel, s Status, n int) {
	if s.Terminal() && n > 0 {
		p.m.Deliveries.Add(float64(n), string(ch), string(s))
	}
}

func (p promMetrics) ProviderResponse(ch provider.Channel, res provider.Result) {
	if res.Duration <= 0 { // no HTTP request was issued
		return
	}
	p.m.Duration.ObserveDuration(res.Duration, string(ch))
	if res.Outcome == provider.OutcomeRateLimited {
		p.m.RateLimited.Inc(string(ch))
	}
}

func (p promMetrics) QueueDepth(depth map[provider.Channel]map[Status]int64, ok bool) {
	if !ok {
		p.m.QueueDepth.Clear()
		return
	}
	vals := map[string]float64{}
	for ch, byStatus := range depth {
		for st, n := range byStatus {
			vals[metrics.Key(string(ch), string(st))] = float64(n)
		}
	}
	p.m.QueueDepth.SetAll(vals)
}
