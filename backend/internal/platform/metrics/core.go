package metrics

// The six core metrics of ARCHITECTURE §15, with their closed label sets.
// Names and label dimensions are the documented ones; the types follow the
// Prometheus naming conventions the names use (_total = counter, _seconds
// = histogram) and §10.1 (the queue depth is sampled every 5 s: a gauge).
// No label carries a campaign, user, destination or any free text.

// Metric names (§15).
const (
	NameDeliveriesTotal          = "deliveries_total"
	NameProviderRequestDuration  = "provider_request_duration_seconds"
	NameProviderRateLimitedTotal = "provider_rate_limited_total"
	NameDeliveryQueueDepth       = "delivery_queue_depth"
	NameLLMRequestsTotal         = "llm_requests_total"
	NameLLMRequestDuration       = "llm_request_duration_seconds"
)

// Label values.
var (
	Channels = []string{"sms", "email"}
	// DeliveryOutcomes are the terminal delivery statuses.
	DeliveryOutcomes = []string{"sent", "failed", "skipped_consent_revoked", "skipped_no_contact", "cancelled"}
	// QueueStatuses are the released, not yet terminal statuses (held rows
	// are a snapshot awaiting confirmation, not queued work).
	QueueStatuses = []string{"pending", "in_flight", "retry_scheduled"}
	LLMTasks      = []string{"segment_interpretation", "content_generation", Other}
	// LLMOutcomes classify one provider request (not a use-case invocation).
	LLMOutcomes = []string{"ok", "max_tokens", "refused", "unavailable", "rejected", "timeout", "cancelled", "aborted", "error"}
)

// Buckets (seconds). The provider request timeout is 5 s; model requests
// may take up to the configured LLM timeouts (minutes).
var (
	ProviderBuckets = []float64{0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10}
	LLMBuckets      = []float64{0.1, 0.25, 0.5, 1, 2.5, 5, 10, 20, 30, 60, 120, 300}
)

// Delivery is the worker's (and, for cancellations, the API's) delivery metric set.
type Delivery struct {
	Deliveries  *CounterVec
	Duration    *HistogramVec
	RateLimited *CounterVec
	QueueDepth  *GaugeVec
}

// NewDeliveriesTotal registers deliveries_total{channel,outcome}.
func NewDeliveriesTotal(r *Registry) *CounterVec {
	return r.NewCounterVec(NameDeliveriesTotal,
		"Delivery rows this process moved to a terminal status (counted once per delivery, however many attempts), since process start.",
		Label{"channel", Channels}, Label{"outcome", DeliveryOutcomes})
}

// NewDelivery registers the four delivery metrics (worker).
func NewDelivery(r *Registry) *Delivery {
	return &Delivery{
		Deliveries: NewDeliveriesTotal(r),
		Duration: r.NewHistogramVec(NameProviderRequestDuration,
			"Duration of provider HTTP requests issued by this process (request build to response read), including 429 and error responses.",
			ProviderBuckets, Label{"channel", Channels}),
		RateLimited: r.NewCounterVec(NameProviderRateLimitedTotal,
			"Provider responses with HTTP 429 received by this process, since process start.",
			Label{"channel", Channels}),
		QueueDepth: r.NewGaugeVec(NameDeliveryQueueDepth,
			"Released delivery rows not yet terminal, by status, sampled from PostgreSQL every 5 s; absent when the last sample failed.",
			Label{"channel", Channels}, Label{"status", QueueStatuses}),
	}
}

// LLM is the API's model-call metric set.
type LLM struct {
	Requests *CounterVec
	Duration *HistogramVec
}

// NewLLM registers the two LLM metrics (API).
func NewLLM(r *Registry) *LLM {
	return &LLM{
		Requests: r.NewCounterVec(NameLLMRequestsTotal,
			"LLM provider requests made by this process (each repair or truncation retry is a request), by result, since process start.",
			Label{"task", LLMTasks}, Label{"outcome", LLMOutcomes}),
		Duration: r.NewHistogramVec(NameLLMRequestDuration,
			"Duration of LLM provider requests made by this process, whatever their outcome.",
			LLMBuckets, Label{"task", LLMTasks}),
	}
}
