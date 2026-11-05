// Command worker is the delivery worker (ARCHITECTURE §10): it processes
// delivery rows released by an explicit send, through the provider at
// PROVIDER_BASE_URL (the mock provider; no real SMS/email vendor exists).
// Run exactly one replica: the rate limiter is in-process. A second process
// (e.g. an overlapping restart) does not dispatch: only the holder of the
// PostgreSQL dispatch lock does (ADR-025); every process runs the reaper,
// completion detector, approval-expiry sweeper and heartbeat.
//
// The worker never consumes an approval or starts a send by itself: it only
// claims rows of a consumed approval of a sending campaign.
package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/fyusufayan/insider-campaign-composer-case/backend/internal/approval"
	"github.com/fyusufayan/insider-campaign-composer-case/backend/internal/delivery"
	"github.com/fyusufayan/insider-campaign-composer-case/backend/internal/platform/config"
	"github.com/fyusufayan/insider-campaign-composer-case/backend/internal/platform/db"
	"github.com/fyusufayan/insider-campaign-composer-case/backend/internal/platform/logging"
	"github.com/fyusufayan/insider-campaign-composer-case/backend/internal/platform/metrics"
	"github.com/fyusufayan/insider-campaign-composer-case/backend/internal/provider"
)

const appName = "composer-worker"

func main() {
	log := logging.New(os.Stdout, slog.LevelInfo)
	if err := run(log); err != nil {
		log.Error("worker stopped", "err", err)
		os.Exit(1)
	}
}

func run(log *slog.Logger) error {
	cfg, err := config.LoadWorker(os.Getenv)
	if err != nil {
		return err
	}
	suffix := make([]byte, 4)
	if _, err := rand.Read(suffix); err != nil {
		return fmt.Errorf("worker id: %w", err)
	}
	// Unique per process start: a restarted worker never treats its
	// predecessor's leases as its own (they are recovered by the reaper).
	workerID := cfg.WorkerID + "-" + hex.EncodeToString(suffix)

	client, err := provider.NewClient(provider.ClientConfig{
		BaseURL: cfg.ProviderBaseURL, RequestTimeout: cfg.ProviderRequestTimeout,
		RetryAfterCap: cfg.RetryAfterCap, MaxConnsPerHost: cfg.SenderPoolSize + 4,
		Capabilities: provider.MockCapabilities, // the only provider in scope (ADR-020)
	})
	if err != nil {
		return err
	}
	policy := delivery.Policy{MaxTransientAttempts: cfg.MaxTransientAttempts, MaxRateLimitedAttempts: cfg.MaxRateLimitedAttempts,
		BackoffBase: cfg.BackoffBase, BackoffCap: cfg.BackoffCap, RetryAfterCap: cfg.RetryAfterCap}
	// GET /metrics on the worker's health port (§15, ADR-026).
	reg := metrics.NewRegistry()
	wcfg := delivery.Config{
		Metrics:  delivery.NewMetrics(metrics.NewDelivery(reg)),
		WorkerID: workerID, PoolSize: cfg.SenderPoolSize, ClaimBatch: cfg.ClaimBatchSize,
		Lease: cfg.LeaseDuration, PollInterval: cfg.PollInterval, Policy: policy,
		RatePerSecond: cfg.RateLimitPerSecond, RateWindow: time.Second + cfg.RateSafetyMargin,
		Gate: delivery.GateConfig{MaxPrecheck: cfg.DispatchMaxPrecheck, MaxSlotToRequest: cfg.DispatchMaxSlotToRequest},
	}
	horizon := delivery.RetryHorizon(policy, cfg.ProviderRequestTimeout, cfg.LeaseDuration, delivery.DefaultReaperInterval)
	caps := client.Capabilities()
	log.Info("configuration loaded", "worker_id", workerID, "provider", caps.Name,
		"provider_idempotency_keys", caps.IdempotencyKeys, "provider_request_timeout", cfg.ProviderRequestTimeout.String(),
		"sender_pool_size", cfg.SenderPoolSize, "claim_batch_size", cfg.ClaimBatchSize, "lease", cfg.LeaseDuration.String(),
		"max_transient_attempts", cfg.MaxTransientAttempts, "max_rate_limited_attempts", cfg.MaxRateLimitedAttempts,
		"backoff_base", cfg.BackoffBase.String(), "backoff_cap", cfg.BackoffCap.String(),
		"retry_after_cap", cfg.RetryAfterCap.String(), "retry_horizon", horizon.String(),
		"rate_limit", fmt.Sprintf("at most %d requests per channel in any %s window (sliding-window log, per channel)",
			cfg.RateLimitPerSecond, (time.Second+cfg.RateSafetyMargin).String()),
		"dispatch_max_precheck", cfg.DispatchMaxPrecheck.String(), "dispatch_max_slot_to_request", cfg.DispatchMaxSlotToRequest.String(),
		"dispatch_quiet_period", wcfg.QuietPeriod().String(), "approval_sweep_interval", cfg.ApprovalSweepInterval.String(),
		"effective_guarantee", delivery.EffectiveGuarantee(caps, horizon))

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	pool, err := db.NewPool(ctx, cfg.DatabaseURL.Reveal(), appName)
	if err != nil {
		return err
	}
	defer pool.Close()

	w, err := delivery.New(delivery.NewRepository(pool), client, wcfg, log)
	if err != nil {
		return err
	}
	sweeper, err := approval.NewSweeper(approval.NewRepository(pool), approval.SweeperConfig{Interval: cfg.ApprovalSweepInterval}, log)
	if err != nil {
		return err
	}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(rw http.ResponseWriter, _ *http.Request) {
		rw.Header().Set("Content-Type", "application/json")
		_, _ = rw.Write([]byte(`{"status":"ok"}` + "\n"))
	})
	mux.Handle("GET /metrics", reg.Handler())
	health := &http.Server{Addr: cfg.HealthAddr, Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	go func() {
		if err := health.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Error("health server failed", "err", err)
			stop()
		}
	}()
	defer func() {
		sctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = health.Shutdown(sctx)
	}()
	sweepDone := make(chan struct{})
	go func() { defer close(sweepDone); sweeper.Run(ctx) }()
	err = w.Run(ctx)
	<-sweepDone
	return err
}
