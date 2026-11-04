//go:build live

// Command liveapi serves the real API (same wiring as cmd/api) with the
// Anthropic adapter's HTTP transport wrapped in budget.Transport, for the
// manually run, billable CP10 verification. It refuses to start unless the
// configuration is the approved one: anthropic provider, SDK retries off,
// an explicitly allowed model, and a persistent counter file.
//
// Build/run only with -tags live. Never used by tests or Compose.
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"slices"
	"syscall"
	"time"

	"github.com/fyusufayan/insider-campaign-composer-case/backend/internal/ai"
	"github.com/fyusufayan/insider-campaign-composer-case/backend/internal/campaign"
	"github.com/fyusufayan/insider-campaign-composer-case/backend/internal/httpapi"
	"github.com/fyusufayan/insider-campaign-composer-case/backend/internal/llm"
	"github.com/fyusufayan/insider-campaign-composer-case/backend/internal/platform/config"
	"github.com/fyusufayan/insider-campaign-composer-case/backend/internal/platform/db"
	"github.com/fyusufayan/insider-campaign-composer-case/backend/internal/platform/logging"
	"github.com/fyusufayan/insider-campaign-composer-case/backend/internal/segment"
	"github.com/fyusufayan/insider-campaign-composer-case/backend/test/live/budget"
)

// maxPaidRequests is the hard cap for the whole CP10 checkpoint.
const maxPaidRequests = 3

var allowedModels = []string{"claude-haiku-5-5", "claude-sonnet-5-5"}

func main() {
	log := logging.New(os.Stdout, slog.LevelInfo)
	if err := run(log); err != nil {
		log.Error("liveapi stopped", "err", err)
		os.Exit(1)
	}
}

func run(log *slog.Logger) error {
	cfg, err := config.Load(os.Getenv)
	if err != nil {
		return err
	}
	counter := os.Getenv("LIVE_BUDGET_FILE")
	switch {
	case cfg.LLM.Provider != config.LLMProviderAnthropic:
		return errors.New("liveapi requires LLM_PROVIDER=anthropic with ANTHROPIC_API_KEY")
	case cfg.LLM.MaxRetries != 0:
		return errors.New("liveapi requires CLAUDE_MAX_RETRIES=0")
	case !slices.Contains(allowedModels, cfg.Segment.Model):
		return fmt.Errorf("liveapi: CLAUDE_SEGMENT_MODEL must be one of %v", allowedModels)
	case !filepath.IsAbs(counter):
		return errors.New("liveapi requires an absolute LIVE_BUDGET_FILE")
	}

	tr := &budget.Transport{
		Base:        http.DefaultTransport.(*http.Transport).Clone(),
		CounterFile: counter,
		Max:         maxPaidRequests,
		AllowedHost: "api.anthropic.com",
	}
	used, err := tr.Used()
	if err != nil {
		return err
	}
	log.Info("live budget", "used", used, "max", maxPaidRequests)

	client, err := llm.NewAnthropic(llm.AnthropicConfig{
		APIKey: cfg.LLM.APIKey.Reveal(), MaxRetries: cfg.LLM.MaxRetries,
		HTTPClient: &http.Client{Transport: tr}, Logger: log,
	})
	if err != nil {
		return err
	}
	interpreter, err := ai.NewInterpreter(client, ai.InterpreterConfig{
		Model: cfg.Segment.Model, Effort: llm.Effort(cfg.Segment.Effort),
		Timeout: cfg.Segment.Timeout, MaxTokens: cfg.Segment.MaxTokens,
	})
	if err != nil {
		return err
	}
	// Wired like cmd/api; no HTTP route reaches it yet (CP12), so the CP10
	// interpretation-only budget is unaffected.
	generator, err := ai.NewGenerator(client, ai.GeneratorConfig{
		Model: cfg.Content.Model, Effort: llm.Effort(cfg.Content.Effort),
		Timeout: cfg.Content.Timeout, MaxTokens: cfg.Content.MaxTokens,
	})
	if err != nil {
		return err
	}
	log.Info("configuration loaded", "http_addr", cfg.HTTPAddr, "llm_provider", cfg.LLM.Provider,
		"llm_max_retries", cfg.LLM.MaxRetries, "segment_model", cfg.Segment.Model,
		"segment_effort", cfg.Segment.Effort, "segment_timeout", cfg.Segment.Timeout.String(),
		"segment_max_tokens", cfg.Segment.MaxTokens, "segment_prompt_version", ai.SegmentPromptVersion)

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	pool, err := db.NewPool(ctx, cfg.DatabaseURL.Reveal(), "composer-liveapi")
	if err != nil {
		return err
	}
	defer pool.Close()
	campaigns, err := campaign.NewService(campaign.NewRepository(pool), segment.NewAudienceRepository(pool), interpreter, generator, log)
	if err != nil {
		return err
	}
	provider := string(cfg.LLM.Provider)
	readiness := func(ctx context.Context) (httpapi.ReadinessReport, error) {
		report := httpapi.ReadinessReport{Database: "ok", LLMProvider: provider}
		st, err := db.Readiness(ctx, pool)
		report.MigrationVersion, report.MigrationDirty = st.Version, st.Dirty
		if err != nil {
			report.Database = "unavailable"
		}
		return report, err
	}
	srv := &http.Server{
		Addr:              cfg.HTTPAddr,
		Handler:           httpapi.NewRouter(log, httpapi.Deps{Campaigns: campaigns, Readiness: readiness}),
		ReadHeaderTimeout: 5 * time.Second,
		ErrorLog:          slog.NewLogLogger(log.Handler(), slog.LevelError),
	}
	errc := make(chan error, 1)
	go func() { errc <- srv.ListenAndServe() }()
	select {
	case err := <-errc:
		return err
	case <-ctx.Done():
	}
	shutdown, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	return srv.Shutdown(shutdown)
}
