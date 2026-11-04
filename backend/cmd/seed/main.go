// Command seed loads the deterministic demo customer dataset (users, consents,
// carts, purchases).
//
// Inputs (environment): DATABASE_URL, SEED_USERS (default 5000),
// SEED_RANDOM_SEED (default 42) and SEED_REFERENCE_TIME (RFC 3339 UTC, default
// 2026-11-01T00:00:00Z). The same inputs always produce the same dataset and
// the same dataset_hash.
//
// A plain run does nothing when users is already populated. With -reset it
// replaces the customer data in one transaction, and refuses if any campaign
// or campaign_deliveries row exists (customer IDs restart at 1, so existing
// campaigns would silently refer to different people). Reset is a
// development-only operation; to reset a database that has campaigns, recreate
// it (migrate down/up) first. The schema must already be migrated.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/fyusufayan/insider-campaign-composer-case/backend/internal/platform/config"
	"github.com/fyusufayan/insider-campaign-composer-case/backend/internal/platform/db"
	"github.com/fyusufayan/insider-campaign-composer-case/backend/internal/platform/logging"
)

const appName = "composer-seed"

func main() {
	log := logging.New(os.Stdout, slog.LevelInfo)
	if err := run(log, os.Args[1:], os.Getenv); err != nil {
		log.Error("seed failed", "err", err)
		os.Exit(1)
	}
}

func run(log *slog.Logger, args []string, getenv func(string) string) error {
	fs := flag.NewFlagSet("seed", flag.ContinueOnError)
	reset := fs.Bool("reset", false, "replace existing customer data (refused if any campaign_deliveries row exists)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() > 0 {
		return fmt.Errorf("unexpected arguments: %v", fs.Args())
	}

	cfg, err := config.LoadSeed(getenv)
	if err != nil {
		return err
	}
	params := Params{Users: cfg.Users, RandomSeed: cfg.RandomSeed, ReferenceTime: cfg.ReferenceTime}
	log.Info("seed inputs",
		"seed_users", params.Users,
		"seed_random_seed", params.RandomSeed,
		"seed_reference_time", params.ReferenceTime.Format(time.RFC3339Nano),
		"pcg_stream", fmt.Sprintf("%#x", pcgStream),
		"reset", *reset)

	ds, err := Generate(params)
	if err != nil {
		return err
	}
	hash := ds.Hash()
	counts := []any{
		"users", len(ds.Users), "channel_consents", len(ds.Consents),
		"cart_events", len(ds.CartEvents), "purchases", len(ds.Purchases),
	}
	log.Info("dataset generated", append([]any{"dataset_hash", hash}, counts...)...)

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	pool, err := db.NewPool(ctx, cfg.DatabaseURL.Reveal(), appName)
	if err != nil {
		return err
	}
	defer pool.Close()

	loaded, err := load(ctx, pool, ds, hash, *reset)
	if errors.Is(err, ErrDeliveriesExist) || errors.Is(err, ErrCampaignsExist) {
		return err
	}
	if err != nil {
		return fmt.Errorf("load: %w", err)
	}
	if !loaded {
		log.Info("users table already populated; nothing to do (run with -reset to replace the customer data)")
		return nil
	}
	log.Info("seed loaded", append([]any{"dataset_hash", hash}, counts...)...)
	return nil
}
