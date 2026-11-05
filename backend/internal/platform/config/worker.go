package config

import (
	"errors"
	"fmt"
	"net/url"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// Worker is the validated configuration of the delivery worker
// (ARCHITECTURE §16). The worker has no LLM configuration and never sees
// ANTHROPIC_API_KEY.
type Worker struct {
	DatabaseURL Secret
	// WorkerID is the leased_by prefix (WORKER_ID, default "worker"); the
	// process appends a random suffix per start.
	WorkerID string
	// HealthAddr serves /healthz (WORKER_METRICS_ADDR; /metrics follows later).
	HealthAddr string
	// ProviderBaseURL is the mock provider (PROVIDER_BASE_URL).
	ProviderBaseURL        string
	ProviderRequestTimeout time.Duration
	SenderPoolSize         int
	ClaimBatchSize         int
	LeaseDuration          time.Duration
	PollInterval           time.Duration
	MaxTransientAttempts   int
	MaxRateLimitedAttempts int
	// BackoffBase/BackoffCap: transient retries wait a full-jitter delay in
	// [0, min(BACKOFF_CAP, BACKOFF_BASE·2^(n−1))) after the n-th attempt.
	BackoffBase   time.Duration
	BackoffCap    time.Duration
	RetryAfterCap time.Duration
	// RateLimitPerSecond is N per channel; the window is 1 s +
	// RateSafetyMargin (§10.4).
	RateLimitPerSecond int
	RateSafetyMargin   time.Duration
	// DispatchMaxPrecheck < DispatchMaxSlotToRequest < RateSafetyMargin
	// (§10.2).
	DispatchMaxPrecheck      time.Duration
	DispatchMaxSlotToRequest time.Duration
	// ApprovalSweepInterval is the period of the approval-expiry sweeper
	// (§5.3, default 30 s).
	ApprovalSweepInterval time.Duration
}

// Worker defaults (§16).
const (
	DefaultProviderBaseURL        = "http://mockprovider:8090"
	DefaultProviderRequestTimeout = 5 * time.Second
	DefaultSenderPoolSize         = 20
	DefaultClaimBatchSize         = 50
	DefaultLeaseDuration          = 60 * time.Second
	DefaultPollInterval           = 500 * time.Millisecond
	DefaultMaxTransientAttempts   = 5
	DefaultMaxRateLimitedAttempts = 10
	DefaultBackoffBase            = time.Second
	DefaultBackoffCap             = 60 * time.Second
	DefaultRetryAfterCap          = 30 * time.Second
	DefaultRateLimitPerSecond     = 100
	DefaultRateSafetyMargin       = 50 * time.Millisecond
	DefaultDispatchMaxSlot        = 20 * time.Millisecond
	DefaultDispatchMaxPrecheck    = 15 * time.Millisecond
	DefaultApprovalSweepInterval  = 30 * time.Second
	defaultWorkerHealthAddr       = ":9091"
	defaultWorkerID               = "worker"
)

var workerIDPattern = regexp.MustCompile(`^[A-Za-z0-9._-]{1,64}$`)

// envReader collects problems while reading typed values.
type envReader struct {
	get  func(string) string
	errs []error
}

func (r *envReader) duration(key string, def, lo, hi time.Duration) time.Duration {
	v := r.get(key)
	if v == "" {
		return def
	}
	d, err := time.ParseDuration(v)
	if err != nil || d < lo || d > hi {
		r.errs = append(r.errs, fmt.Errorf("%s must be a duration in [%s, %s] (got %q)", key, lo, hi, v))
		return def
	}
	return d
}

func (r *envReader) integer(key string, def, lo, hi int) int {
	v := r.get(key)
	if v == "" {
		return def
	}
	n, err := strconv.Atoi(v)
	if err != nil || n < lo || n > hi {
		r.errs = append(r.errs, fmt.Errorf("%s must be an integer in [%d, %d] (got %q)", key, lo, hi, v))
		return def
	}
	return n
}

func (r *envReader) float(key string, def, lo, hi float64) float64 {
	v := r.get(key)
	if v == "" {
		return def
	}
	f, err := strconv.ParseFloat(v, 64)
	if err != nil || f < lo || f > hi {
		r.errs = append(r.errs, fmt.Errorf("%s must be a number in [%g, %g] (got %q)", key, lo, hi, v))
		return def
	}
	return f
}

func (r *envReader) addr(key, def string) string {
	v := r.get(key)
	if v == "" {
		v = def
	}
	if err := validateListenAddr(v); err != nil {
		r.errs = append(r.errs, fmt.Errorf("%s: %w", key, err))
	}
	return v
}

// LoadWorker reads and validates the worker configuration.
func LoadWorker(getenv func(string) string) (Worker, error) {
	r := &envReader{get: func(k string) string { return strings.TrimSpace(getenv(k)) }}
	w := Worker{DatabaseURL: Secret(r.get("DATABASE_URL"))}
	if w.DatabaseURL == "" {
		r.errs = append(r.errs, errors.New("DATABASE_URL is required"))
	}
	w.WorkerID = r.get("WORKER_ID")
	if w.WorkerID == "" {
		w.WorkerID = defaultWorkerID
	}
	if !workerIDPattern.MatchString(w.WorkerID) {
		r.errs = append(r.errs, errors.New("WORKER_ID must be 1-64 characters of A-Z a-z 0-9 . _ -"))
	}
	w.HealthAddr = r.addr("WORKER_METRICS_ADDR", defaultWorkerHealthAddr)
	w.ProviderBaseURL = r.get("PROVIDER_BASE_URL")
	if w.ProviderBaseURL == "" {
		w.ProviderBaseURL = DefaultProviderBaseURL
	}
	if err := validateProviderURL(w.ProviderBaseURL); err != nil {
		r.errs = append(r.errs, fmt.Errorf("PROVIDER_BASE_URL %w", err))
	}
	w.ProviderRequestTimeout = r.duration("PROVIDER_REQUEST_TIMEOUT", DefaultProviderRequestTimeout, 100*time.Millisecond, 60*time.Second)
	w.SenderPoolSize = r.integer("SENDER_POOL_SIZE", DefaultSenderPoolSize, 1, 200)
	w.ClaimBatchSize = r.integer("CLAIM_BATCH_SIZE", DefaultClaimBatchSize, 1, 500)
	w.LeaseDuration = r.duration("LEASE_DURATION", DefaultLeaseDuration, time.Second, time.Hour)
	w.PollInterval = r.duration("WORKER_POLL_INTERVAL", DefaultPollInterval, 10*time.Millisecond, 10*time.Second)
	w.MaxTransientAttempts = r.integer("MAX_TRANSIENT_ATTEMPTS", DefaultMaxTransientAttempts, 1, 20)
	w.MaxRateLimitedAttempts = r.integer("MAX_RATE_LIMITED_ATTEMPTS", DefaultMaxRateLimitedAttempts, 1, 50)
	if r.get("DELIVERY_RETRY_DELAY") != "" {
		r.errs = append(r.errs, errors.New("DELIVERY_RETRY_DELAY (CP16 fixed retry delay) was replaced by BACKOFF_BASE and BACKOFF_CAP; remove it"))
	}
	w.BackoffBase = r.duration("BACKOFF_BASE", DefaultBackoffBase, 10*time.Millisecond, 10*time.Minute)
	w.BackoffCap = r.duration("BACKOFF_CAP", DefaultBackoffCap, 10*time.Millisecond, time.Hour)
	if w.BackoffCap < w.BackoffBase {
		r.errs = append(r.errs, fmt.Errorf("BACKOFF_CAP (%s) must be at least BACKOFF_BASE (%s)", w.BackoffCap, w.BackoffBase))
	}
	w.RetryAfterCap = r.duration("RETRY_AFTER_CAP", DefaultRetryAfterCap, time.Second, 10*time.Minute)
	w.RateLimitPerSecond = r.integer("RATE_LIMIT_PER_SECOND", DefaultRateLimitPerSecond, 1, 1000)
	w.RateSafetyMargin = r.duration("RATE_SAFETY_MARGIN", DefaultRateSafetyMargin, time.Millisecond, time.Second)
	w.DispatchMaxSlotToRequest = r.duration("DISPATCH_MAX_SLOT_TO_REQUEST", DefaultDispatchMaxSlot, time.Millisecond, time.Second)
	w.DispatchMaxPrecheck = r.duration("DISPATCH_MAX_PRECHECK", DefaultDispatchMaxPrecheck, time.Millisecond, time.Second)
	// The margin must cover the enforced slot-to-request bound (§10.4).
	if !(w.DispatchMaxPrecheck < w.DispatchMaxSlotToRequest && w.DispatchMaxSlotToRequest < w.RateSafetyMargin) {
		r.errs = append(r.errs, fmt.Errorf("need DISPATCH_MAX_PRECHECK (%s) < DISPATCH_MAX_SLOT_TO_REQUEST (%s) < RATE_SAFETY_MARGIN (%s)",
			w.DispatchMaxPrecheck, w.DispatchMaxSlotToRequest, w.RateSafetyMargin))
	}
	w.ApprovalSweepInterval = r.duration("APPROVAL_SWEEP_INTERVAL", DefaultApprovalSweepInterval, time.Second, 10*time.Minute)
	// The lease must comfortably exceed a request, so that a live worker's
	// row is never reaped mid-request (§16).
	if w.LeaseDuration < 10*w.ProviderRequestTimeout {
		r.errs = append(r.errs, fmt.Errorf("LEASE_DURATION (%s) must be at least 10 × PROVIDER_REQUEST_TIMEOUT (%s)",
			w.LeaseDuration, w.ProviderRequestTimeout))
	}
	if len(r.errs) > 0 {
		return Worker{}, fmt.Errorf("invalid configuration: %w", errors.Join(r.errs...))
	}
	return w, nil
}

func validateProviderURL(raw string) error {
	u, err := url.Parse(raw)
	switch {
	case err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "":
		return errors.New("must be an absolute http(s) URL")
	case u.User != nil:
		return errors.New("must not contain credentials")
	case u.RawQuery != "" || u.Fragment != "":
		return errors.New("must not contain a query or fragment")
	}
	return nil
}

// MockProvider is the validated configuration of cmd/mockprovider.
type MockProvider struct {
	Addr                string
	Fault5xxRate        float64
	Fault429Rate        float64
	FaultTimeoutRate    float64
	LatencyMin          time.Duration
	LatencyMax          time.Duration
	PermanentFailSuffix string
	TimeoutStall        time.Duration
	RetryAfter          time.Duration
	Seed                uint64
	// LedgerPath is MOCK_LEDGER_PATH: the durable idempotency journal
	// (required; on a persistent volume).
	LedgerPath string
	// AllowReset is MOCK_ADMIN_RESET_ENABLED (default false): enables the
	// destructive POST /admin/reset.
	AllowReset bool
	// RateLimit is MOCK_RATE_LIMIT_PER_SECOND (default 100, 0 disables):
	// the mock's own strict per-channel limit.
	RateLimit int
}

// Mock provider defaults (§16).
const (
	DefaultMock5xxRate          = 0.03
	DefaultMock429Rate          = 0.01
	DefaultMockTimeoutRate      = 0.005
	DefaultMockLatency          = "50-200"
	DefaultMockPermanentPattern = "0000"
)

var suffixPattern = regexp.MustCompile(`^[0-9A-Za-z@._+-]{0,32}$`)

// LoadMockProvider reads and validates the mock provider configuration.
func LoadMockProvider(getenv func(string) string) (MockProvider, error) {
	r := &envReader{get: func(k string) string { return strings.TrimSpace(getenv(k)) }}
	m := MockProvider{Addr: r.addr("MOCK_PROVIDER_ADDR", ":8090")}
	m.Fault5xxRate = r.float("MOCK_FAULT_5XX_RATE", DefaultMock5xxRate, 0, 1)
	m.Fault429Rate = r.float("MOCK_FAULT_429_RATE", DefaultMock429Rate, 0, 1)
	m.FaultTimeoutRate = r.float("MOCK_FAULT_TIMEOUT_RATE", DefaultMockTimeoutRate, 0, 1)
	if m.Fault5xxRate+m.Fault429Rate+m.FaultTimeoutRate > 1 {
		r.errs = append(r.errs, errors.New("MOCK_FAULT_*_RATE must sum to at most 1"))
	}
	lat := r.get("MOCK_LATENCY_MS")
	if lat == "" {
		lat = DefaultMockLatency
	}
	lo, hi, ok := parseRange(lat)
	if !ok {
		r.errs = append(r.errs, fmt.Errorf("MOCK_LATENCY_MS must be N or MIN-MAX milliseconds in [0, 10000] (got %q)", lat))
	}
	m.LatencyMin, m.LatencyMax = time.Duration(lo)*time.Millisecond, time.Duration(hi)*time.Millisecond
	m.PermanentFailSuffix = DefaultMockPermanentPattern
	if v, set := lookup(getenv, "MOCK_PERMANENT_FAIL_PATTERN"); set {
		m.PermanentFailSuffix = v // empty disables
	}
	if !suffixPattern.MatchString(m.PermanentFailSuffix) {
		r.errs = append(r.errs, errors.New("MOCK_PERMANENT_FAIL_PATTERN must be a destination suffix of at most 32 characters"))
	}
	m.TimeoutStall = r.duration("MOCK_TIMEOUT_STALL", 30*time.Second, 100*time.Millisecond, 10*time.Minute)
	m.RetryAfter = r.duration("MOCK_RETRY_AFTER", time.Second, time.Second, 60*time.Second)
	m.Seed = uint64(r.integer("MOCK_RANDOM_SEED", 1, 0, 1<<31-1))
	m.RateLimit = r.integer("MOCK_RATE_LIMIT_PER_SECOND", 100, 0, 100000)
	m.LedgerPath = r.get("MOCK_LEDGER_PATH")
	if m.LedgerPath == "" || !filepath.IsAbs(m.LedgerPath) || filepath.Clean(m.LedgerPath) != m.LedgerPath {
		r.errs = append(r.errs, errors.New("MOCK_LEDGER_PATH must be a clean absolute file path (the durable idempotency ledger)"))
	}
	switch v := r.get("MOCK_ADMIN_RESET_ENABLED"); v {
	case "", "false":
	case "true":
		m.AllowReset = true
	default:
		r.errs = append(r.errs, fmt.Errorf("MOCK_ADMIN_RESET_ENABLED must be true or false (got %q)", v))
	}
	if len(r.errs) > 0 {
		return MockProvider{}, fmt.Errorf("invalid configuration: %w", errors.Join(r.errs...))
	}
	return m, nil
}

// lookup distinguishes an unset variable from one set to the empty string
// (getenv returns "" for both; Compose passes unset defaults explicitly).
func lookup(getenv func(string) string, key string) (string, bool) {
	v := getenv(key)
	if v == "" {
		return "", false
	}
	if strings.TrimSpace(v) == "-" {
		return "", true // "-" disables explicitly
	}
	return strings.TrimSpace(v), true
}

func parseRange(s string) (int, int, bool) {
	a, b, found := strings.Cut(s, "-")
	lo, err1 := strconv.Atoi(strings.TrimSpace(a))
	hi := lo
	var err2 error
	if found {
		hi, err2 = strconv.Atoi(strings.TrimSpace(b))
	}
	if err1 != nil || err2 != nil || lo < 0 || hi < lo || hi > 10000 {
		return 0, 0, false
	}
	return lo, hi, true
}
