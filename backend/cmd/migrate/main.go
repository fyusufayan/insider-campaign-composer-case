// Command migrate applies the embedded schema migrations (backend/migrations)
// to DATABASE_URL and exits. It is the one-shot "migrate" container of the
// Compose stack: the seed and api containers start only after it exits 0.
//
// Applying when the schema is already at the latest version is a no-op. A
// dirty schema (a previously failed migration) is reported as an error and is
// never forced; it must be repaired by hand or the volume recreated.
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/fyusufayan/insider-campaign-composer-case/backend/internal/platform/config"
	"github.com/fyusufayan/insider-campaign-composer-case/backend/internal/platform/db"
	"github.com/fyusufayan/insider-campaign-composer-case/backend/internal/platform/logging"
)

func main() {
	log := logging.New(os.Stdout, slog.LevelInfo)
	if err := run(log, os.Getenv); err != nil {
		log.Error("migrate failed", "err", err)
		os.Exit(1)
	}
}

func run(log *slog.Logger, getenv func(string) string) error {
	cfg, err := config.LoadMigrate(getenv)
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	url := cfg.DatabaseURL.Reveal()
	before, err := db.CurrentMigration(ctx, url)
	if err != nil {
		return err
	}
	if before.Dirty {
		return fmt.Errorf("schema is dirty at version %d (a previous migration failed); repair it or recreate the database", before.Version)
	}
	log.Info("applying migrations", "current_version", before.Version)

	if err := db.MigrateUp(ctx, url); err != nil {
		return err
	}
	after, err := db.CurrentMigration(ctx, url)
	if err != nil {
		return err
	}
	if after.Dirty || after.Version == 0 {
		return errors.New("schema is not at a clean migrated version after migrate up")
	}
	log.Info("migrations applied", "previous_version", before.Version, "version", after.Version)
	return nil
}
