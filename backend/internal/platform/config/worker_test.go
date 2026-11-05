package config

import (
	"strings"
	"testing"
	"time"
)

func TestLoadWorkerDefaults(t *testing.T) {
	w, err := LoadWorker(env(map[string]string{"DATABASE_URL": "postgres://x"}))
	if err != nil {
		t.Fatal(err)
	}
	if w.WorkerID != "worker" || w.HealthAddr != ":9091" || w.ProviderBaseURL != "http://mockprovider:8090" ||
		w.ProviderRequestTimeout != 5*time.Second || w.SenderPoolSize != 20 || w.ClaimBatchSize != 50 ||
		w.LeaseDuration != time.Minute || w.PollInterval != 500*time.Millisecond || w.MaxTransientAttempts != 5 ||
		w.MaxRateLimitedAttempts != 10 || w.RetryDelay != 5*time.Second || w.RetryAfterCap != 30*time.Second {
		t.Errorf("defaults = %+v", w)
	}
}

func TestLoadWorkerRejectsInvalid(t *testing.T) {
	for key, val := range map[string]string{
		"WORKER_ID": "has space", "PROVIDER_BASE_URL": "ftp://x", "PROVIDER_REQUEST_TIMEOUT": "1ms",
		"SENDER_POOL_SIZE": "0", "CLAIM_BATCH_SIZE": "x", "LEASE_DURATION": "10s", // < 10 × 5s
		"WORKER_POLL_INTERVAL": "1h", "MAX_TRANSIENT_ATTEMPTS": "0", "DELIVERY_RETRY_DELAY": "-1s",
		"WORKER_METRICS_ADDR": "nope",
	} {
		_, err := LoadWorker(env(map[string]string{"DATABASE_URL": "postgres://x", key: val}))
		if err == nil || !strings.Contains(err.Error(), key) {
			t.Errorf("%s=%q: %v", key, val, err)
		}
	}
	if _, err := LoadWorker(env(map[string]string{})); err == nil || !strings.Contains(err.Error(), "DATABASE_URL") {
		t.Errorf("missing DATABASE_URL: %v", err)
	}
	// The secret never appears in errors.
	_, err := LoadWorker(env(map[string]string{"DATABASE_URL": "postgres://u:hunter2@h/db", "SENDER_POOL_SIZE": "0"}))
	if err == nil || strings.Contains(err.Error(), "hunter2") {
		t.Errorf("err = %v", err)
	}
}

func TestLoadMockProvider(t *testing.T) {
	if _, err := LoadMockProvider(env(map[string]string{})); err == nil || !strings.Contains(err.Error(), "MOCK_LEDGER_PATH") {
		t.Errorf("missing ledger path: %v", err)
	}
	const lp = "/var/lib/mockprovider/ledger.journal"
	m, err := LoadMockProvider(env(map[string]string{"MOCK_LEDGER_PATH": lp}))
	if err != nil {
		t.Fatal(err)
	}
	if m.LedgerPath != lp || m.AllowReset {
		t.Errorf("ledger = %q reset = %v", m.LedgerPath, m.AllowReset)
	}
	if m.Addr != ":8090" || m.Fault5xxRate != 0.03 || m.Fault429Rate != 0.01 || m.FaultTimeoutRate != 0.005 ||
		m.LatencyMin != 50*time.Millisecond || m.LatencyMax != 200*time.Millisecond || m.PermanentFailSuffix != "0000" {
		t.Errorf("defaults = %+v", m)
	}
	m, err = LoadMockProvider(env(map[string]string{"MOCK_LATENCY_MS": "0", "MOCK_PERMANENT_FAIL_PATTERN": "-",
		"MOCK_FAULT_5XX_RATE": "0", "MOCK_LEDGER_PATH": lp, "MOCK_ADMIN_RESET_ENABLED": "true"}))
	if err != nil || m.LatencyMax != 0 || m.PermanentFailSuffix != "" || m.Fault5xxRate != 0 || !m.AllowReset {
		t.Errorf("overrides = %+v %v", m, err)
	}
	for key, val := range map[string]string{"MOCK_FAULT_5XX_RATE": "1.5", "MOCK_LATENCY_MS": "9-3",
		"MOCK_PERMANENT_FAIL_PATTERN": "a b", "MOCK_PROVIDER_ADDR": "x", "MOCK_LEDGER_PATH": "relative/ledger",
		"MOCK_ADMIN_RESET_ENABLED": "yes"} {
		if _, err := LoadMockProvider(env(map[string]string{"MOCK_LEDGER_PATH": lp, key: val})); err == nil {
			t.Errorf("%s=%q accepted", key, val)
		}
	}
	if _, err := LoadMockProvider(env(map[string]string{"MOCK_LEDGER_PATH": lp, "MOCK_FAULT_5XX_RATE": "0.6", "MOCK_FAULT_429_RATE": "0.6"})); err == nil {
		t.Error("rates summing over 1 accepted")
	}
}
