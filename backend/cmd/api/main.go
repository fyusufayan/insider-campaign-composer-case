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
	"github.com/fyusufayan/insider-campaign-composer-case/backend/internal/httpapi"
	"github.com/fyusufayan/insider-campaign-composer-case/backend/internal/llm"
	"github.com/fyusufayan/insider-campaign-composer-case/backend/internal/platform/config"
	"github.com/fyusufayan/insider-campaign-composer-case/backend/internal/platform/db"
	"github.com/fyusufayan/insider-campaign-composer-case/backend/internal/platform/logging"
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
	// Selected once at startup; a resolved anthropic provider fails here until
	// Slice 1b and never falls back to demo (invariant 17).
	llmClient, err := llm.New(cfg.LLM, ai.DemoResponders())
	if err != nil {
		return err
	}
	interpreter, err := ai.NewInterpreter(llmClient, ai.InterpreterConfig{
		Model:     cfg.Segment.Model,
		Effort:    llm.Effort(cfg.Segment.Effort),
		Timeout:   cfg.Segment.Timeout,
		MaxTokens: cfg.Segment.MaxTokens,
	})
	if err != nil {
		return err
	}
	_ = interpreter // used by the campaign service (CP6)
	log.Info("configuration loaded", "http_addr", cfg.HTTPAddr, "llm_provider", cfg.LLM.Provider,
		"segment_model", cfg.Segment.Model, "segment_prompt_version", ai.SegmentPromptVersion)

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	pool, err := db.NewPool(ctx, cfg.DatabaseURL.Reveal(), appName)
	if err != nil {
		return err
	}
	defer pool.Close()

	srv := &http.Server{
		Addr:              cfg.HTTPAddr,
		Handler:           httpapi.NewRouter(log),
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
