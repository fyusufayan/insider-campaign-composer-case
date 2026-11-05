// Command api serves the Smart Campaign Composer HTTP API.
package main

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/fyusufayan/insider-campaign-composer-case/backend/internal/ai"
	"github.com/fyusufayan/insider-campaign-composer-case/backend/internal/approval"
	"github.com/fyusufayan/insider-campaign-composer-case/backend/internal/campaign"
	"github.com/fyusufayan/insider-campaign-composer-case/backend/internal/delivery"
	"github.com/fyusufayan/insider-campaign-composer-case/backend/internal/httpapi"
	"github.com/fyusufayan/insider-campaign-composer-case/backend/internal/llm"
	"github.com/fyusufayan/insider-campaign-composer-case/backend/internal/platform/config"
	"github.com/fyusufayan/insider-campaign-composer-case/backend/internal/platform/db"
	"github.com/fyusufayan/insider-campaign-composer-case/backend/internal/platform/logging"
	"github.com/fyusufayan/insider-campaign-composer-case/backend/internal/platform/metrics"
	"github.com/fyusufayan/insider-campaign-composer-case/backend/internal/segment"
)

const (
	appName           = "composer-api"
	readHeaderTimeout = 5 * time.Second
	idleTimeout       = 120 * time.Second
	shutdownTimeout   = 10 * time.Second
)

func main() {
	log := logging.New(os.Stdout, slog.LevelInfo)
	if err := run(log); err != nil {
		log.Error("api stopped", "err", err)
		os.Exit(1)
	}
}

func run(log *slog.Logger) error {
	cfg, err := config.Load(os.Getenv)
	if err != nil {
		return err
	}

	if cfg.LLM.FellBackToDemo() {
		log.Warn("ANTHROPIC_API_KEY is not set; LLM_PROVIDER=auto selected the demo provider (mock AI output)")
	}
	// Selected once at startup and never replaced: an Anthropic failure is
	// returned to the caller, never answered with demo output (invariant 17).
	// No request is made here; the key is first used by a real interpretation.
	llmClient, err := llm.New(cfg.LLM, ai.DemoResponders(), log)
	if err != nil {
		return err
	}
	// GET /metrics (§15, ADR-026): process counters/histograms in memory.
	reg := metrics.NewRegistry()
	llmMetrics := metrics.NewLLM(reg)
	deliveriesTotal := metrics.NewDeliveriesTotal(reg) // queued rows cancelled by POST /cancel
	llmClient = llm.Observe(llmClient, func(task, outcome string, d time.Duration) {
		llmMetrics.Requests.Inc(task, outcome)
		llmMetrics.Duration.ObserveDuration(d, task)
	})
	interpreter, err := ai.NewInterpreter(llmClient, ai.InterpreterConfig{
		Model:     cfg.Segment.Model,
		Effort:    llm.Effort(cfg.Segment.Effort),
		Timeout:   cfg.Segment.Timeout,
		MaxTokens: cfg.Segment.MaxTokens,
	})
	if err != nil {
		return err
	}
	generator, err := ai.NewGenerator(llmClient, ai.GeneratorConfig{
		Model:     cfg.Content.Model,
		Effort:    llm.Effort(cfg.Content.Effort),
		Timeout:   cfg.Content.Timeout,
		MaxTokens: cfg.Content.MaxTokens,
	})
	if err != nil {
		return err
	}
	log.Info("configuration loaded", "http_addr", cfg.HTTPAddr, "llm_provider", cfg.LLM.Provider,
		"llm_max_retries", cfg.LLM.MaxRetries, "segment_model", cfg.Segment.Model, "segment_effort", cfg.Segment.Effort,
		"segment_timeout", cfg.Segment.Timeout.String(), "segment_max_tokens", cfg.Segment.MaxTokens,
		"segment_prompt_version", ai.SegmentPromptVersion, "content_model", cfg.Content.Model,
		"content_effort", cfg.Content.Effort, "content_timeout", cfg.Content.Timeout.String(),
		"content_max_tokens", cfg.Content.MaxTokens, "content_prompt_version", ai.ContentPromptVersion,
		"approval_pending_ttl", cfg.Approval.PendingTTL.String(), "approval_approved_ttl", cfg.Approval.ApprovedTTL.String())

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	pool, err := db.NewPool(ctx, cfg.DatabaseURL.Reveal(), appName)
	if err != nil {
		return err
	}
	defer pool.Close()

	approvals, err := approval.NewService(approval.NewRepository(pool), approval.Config{
		PendingTTL: cfg.Approval.PendingTTL, ApprovedTTL: cfg.Approval.ApprovedTTL, ApprovedBy: cfg.Approval.ApproverName,
		DeliveriesCancelled: func(ch segment.Channel, n int64) { deliveriesTotal.Add(float64(n), string(ch), "cancelled") },
	}, log)
	if err != nil {
		return err
	}
	// Lazy approval expiry (§5.3) on every campaign read/mutation of a
	// campaign with a live approval.
	campaigns, err := campaign.NewService(campaign.NewRepository(pool), segment.NewAudienceRepository(pool), interpreter, generator, log,
		campaign.WithApprovalExpirer(approvals))
	if err != nil {
		return err
	}
	// Delivery statistics, the delivery list and the progress stream read
	// committed rows only (CP17B); a due live approval is expired lazily
	// first, as for every other campaign read.
	stats := delivery.NewStatsReader(pool, func(ctx context.Context, id string) error {
		cid, err := campaign.ParseID(id)
		if err != nil {
			return err
		}
		_, err = approvals.ExpireDue(ctx, cid)
		return err
	})
	provider := string(cfg.LLM.Provider)
	readiness := func(ctx context.Context) (httpapi.ReadinessReport, error) {
		report := httpapi.ReadinessReport{Database: "ok", LLMProvider: provider, Worker: delivery.UnknownWorkerHealth()}
		st, err := db.Readiness(ctx, pool)
		report.MigrationVersion, report.MigrationDirty = st.Version, st.Dirty
		if err != nil {
			report.Database = "unavailable"
			return report, err
		}
		// The worker heartbeat is reported, never a readiness failure (§15).
		if d, derr := stats.Readiness(ctx); derr == nil {
			report.Worker, report.SendingCampaigns = d.Worker, &d.SendingCampaigns
		} else {
			log.WarnContext(ctx, "worker heartbeat unavailable", "err", derr)
		}
		return report, nil
	}

	srv := &http.Server{
		Addr: cfg.HTTPAddr,
		Handler: httpapi.NewRouter(log, httpapi.Deps{Campaigns: campaigns, Approvals: approvals, Readiness: readiness, Deliveries: stats,
			Metrics: reg.Handler()}),
		ReadHeaderTimeout: readHeaderTimeout,
		IdleTimeout:       idleTimeout,
		// No WriteTimeout: SSE responses are long-lived.
		ErrorLog: slog.NewLogLogger(log.Handler(), slog.LevelError),
	}

	serveErr := make(chan error, 1)
	go func() {
		log.Info("http server listening", "addr", cfg.HTTPAddr)
		serveErr <- srv.ListenAndServe()
	}()

	select {
	case err := <-serveErr:
		return fmt.Errorf("http server: %w", err)
	case <-ctx.Done():
	}

	log.Info("shutting down")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		return fmt.Errorf("http shutdown: %w", err)
	}
	return nil
}
