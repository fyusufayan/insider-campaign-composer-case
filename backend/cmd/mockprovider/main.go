// Command mockprovider is the fake SMS/email provider (ADR-020). It sends
// nothing: accepted messages are recorded in a durable idempotency journal
// (MOCK_LEDGER_PATH, fsynced before 202); it refuses to start if the journal
// cannot be opened or validated.
// Internal to the Compose network; never expose it as a real provider.
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/fyusufayan/insider-campaign-composer-case/backend/internal/mockprovider"
	"github.com/fyusufayan/insider-campaign-composer-case/backend/internal/platform/config"
	"github.com/fyusufayan/insider-campaign-composer-case/backend/internal/platform/logging"
)

func main() {
	log := logging.New(os.Stdout, slog.LevelInfo)
	if err := run(log); err != nil {
		log.Error("mock provider stopped", "err", err)
		os.Exit(1)
	}
}

func run(log *slog.Logger) error {
	cfg, err := config.LoadMockProvider(os.Getenv)
	if err != nil {
		return err
	}
	srv, err := mockprovider.New(mockprovider.Config{
		Fault5xxRate: cfg.Fault5xxRate, Fault429Rate: cfg.Fault429Rate, FaultTimeoutRate: cfg.FaultTimeoutRate,
		LatencyMin: cfg.LatencyMin, LatencyMax: cfg.LatencyMax, PermanentFailSuffix: cfg.PermanentFailSuffix,
		TimeoutStall: cfg.TimeoutStall, RetryAfter: cfg.RetryAfter, Seed: cfg.Seed,
		LedgerPath: cfg.LedgerPath, AllowReset: cfg.AllowReset, RateLimit: cfg.RateLimit,
	}, log)
	if err != nil {
		// Fail closed: never start with a fresh ledger when the durable one
		// cannot be opened or validated.
		return err
	}
	defer srv.Close()
	log.Info("mock provider configuration", "addr", cfg.Addr, "fault_5xx_rate", cfg.Fault5xxRate,
		"fault_429_rate", cfg.Fault429Rate, "fault_timeout_rate", cfg.FaultTimeoutRate,
		"latency_ms", fmt.Sprintf("%d-%d", cfg.LatencyMin.Milliseconds(), cfg.LatencyMax.Milliseconds()),
		"permanent_fail_suffix_set", cfg.PermanentFailSuffix != "", "seed", cfg.Seed,
		"ledger_path", cfg.LedgerPath, "admin_reset_enabled", cfg.AllowReset, "rate_limit_per_second", cfg.RateLimit)

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	hs := &http.Server{Addr: cfg.Addr, Handler: srv.Handler(), ReadHeaderTimeout: 5 * time.Second, IdleTimeout: 120 * time.Second,
		ErrorLog: slog.NewLogLogger(log.Handler(), slog.LevelError)}
	errc := make(chan error, 1)
	go func() { errc <- hs.ListenAndServe() }()
	select {
	case err := <-errc:
		if !errors.Is(err, http.ErrServerClosed) {
			return fmt.Errorf("http server: %w", err)
		}
		return nil
	case <-ctx.Done():
	}
	sctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return hs.Shutdown(sctx)
}
