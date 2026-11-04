package db

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sync"

	"github.com/golang-migrate/migrate/v4"
	migratepgx "github.com/golang-migrate/migrate/v4/database/pgx/v5"
	"github.com/golang-migrate/migrate/v4/source/iofs"
	"github.com/jackc/pgx/v5/stdlib"

	"github.com/fyusufayan/insider-campaign-composer-case/backend/migrations"
)

// MigrationState is the schema version recorded by golang-migrate.
type MigrationState struct {
	Version uint // 0 when no migration has been applied
	Dirty   bool
}

// MigrateUp applies every embedded migration that has not been applied yet.
// Applying when already at the latest version is not an error.
func MigrateUp(ctx context.Context, databaseURL string) error {
	return withMigrator(ctx, databaseURL, func(m *migrate.Migrate) error {
		if err := m.Up(); err != nil && !errors.Is(err, migrate.ErrNoChange) {
			return fmt.Errorf("migrate up: %w", err)
		}
		return nil
	})
}

// MigrateDown reverts every applied migration. It is used by tests and
// development tooling only; no production path calls it.
func MigrateDown(ctx context.Context, databaseURL string) error {
	return withMigrator(ctx, databaseURL, func(m *migrate.Migrate) error {
		if err := m.Down(); err != nil && !errors.Is(err, migrate.ErrNoChange) {
			return fmt.Errorf("migrate down: %w", err)
		}
		return nil
	})
}

// CurrentMigration reports the applied schema version.
func CurrentMigration(ctx context.Context, databaseURL string) (MigrationState, error) {
	var st MigrationState
	err := withMigrator(ctx, databaseURL, func(m *migrate.Migrate) error {
		v, dirty, err := m.Version()
		if errors.Is(err, migrate.ErrNilVersion) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("migration version: %w", err)
		}
		st = MigrationState{Version: v, Dirty: dirty}
		return nil
	})
	return st, err
}

// withMigrator opens a dedicated database/sql handle (golang-migrate needs one),
// with the same session settings as the pool, and asks the migrator to stop
// gracefully when ctx is cancelled. Each migration file runs as a single
// multi-statement script inside its own explicit transaction.
//
// Cancellation semantics (golang-migrate v4.19.1): the stop request is
// honoured between migration steps; the driver runs each statement and its
// pg_advisory_lock wait (bounded by migrate.DefaultLockTimeout, 15 s) with
// context.Background(), so a file that is already executing runs to completion.
// Because every file is transactional, a cancelled run leaves the schema at a
// clean version (possibly already the latest) and re-running is safe. A
// cancelled run always returns an error wrapping ctx.Err().
func withMigrator(ctx context.Context, databaseURL string, fn func(*migrate.Migrate) error) error {
	cfg, err := poolConfig(databaseURL, "composer-migrate")
	if err != nil {
		return err
	}
	sqlDB := stdlib.OpenDB(*cfg.ConnConfig)
	defer sqlDB.Close()
	if err := pingSQL(ctx, sqlDB); err != nil {
		return err
	}

	src, err := iofs.New(migrations.FS, ".")
	if err != nil {
		return fmt.Errorf("open embedded migrations: %w", err)
	}
	drv, err := migratepgx.WithInstance(sqlDB, &migratepgx.Config{})
	if err != nil {
		_ = src.Close()
		return fmt.Errorf("open migration driver: %w", err)
	}
	m, err := migrate.NewWithInstance("iofs", src, "pgx5", drv)
	if err != nil {
		_ = src.Close()
		_ = drv.Close()
		return fmt.Errorf("create migrator: %w", err)
	}
	// Deferred calls run in reverse: the watcher is released (and has exited)
	// before the migrator is closed, and the migrator before the sql.DB.
	defer m.Close()
	release := stopOnCancel(ctx, m.GracefulStop)
	defer release()

	if err := fn(m); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("migration cancelled: %w", err)
	}
	return nil
}

// stopOnCancel starts a watcher that sends one stop request on stop when ctx
// is cancelled. The send never blocks: if nobody can take the value (a full or
// unbuffered channel without a reader) it is dropped, so the watcher cannot
// hang after the migrator is done. release stops the watcher and returns only
// after it has exited, so no goroutine outlives the caller; it is idempotent.
func stopOnCancel(ctx context.Context, stop chan<- bool) (release func()) {
	done := make(chan struct{})
	exited := make(chan struct{})
	go func() {
		defer close(exited)
		select {
		case <-ctx.Done():
			select {
			case stop <- true:
			default:
			}
		case <-done:
		}
	}()
	var once sync.Once
	return func() {
		once.Do(func() {
			close(done)
			<-exited
		})
	}
}

func pingSQL(ctx context.Context, sqlDB *sql.DB) error {
	pingCtx, cancel := context.WithTimeout(ctx, pingTimeout)
	defer cancel()
	if err := sqlDB.PingContext(pingCtx); err != nil {
		return fmt.Errorf("ping database: %w", err)
	}
	return nil
}
