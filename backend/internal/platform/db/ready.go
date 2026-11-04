package db

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// sqlMigrationState reads golang-migrate's bookkeeping table.
const sqlMigrationState = `SELECT version, dirty FROM schema_migrations LIMIT 1`

// ErrNotMigrated is returned by Readiness when no clean migration is applied.
var ErrNotMigrated = errors.New("database schema is not migrated")

// Readiness pings the database and returns the applied migration state. A
// missing, zero or dirty migration is reported as ErrNotMigrated.
func Readiness(ctx context.Context, pool *pgxpool.Pool) (MigrationState, error) {
	if err := pool.Ping(ctx); err != nil {
		return MigrationState{}, fmt.Errorf("ping database: %w", err)
	}
	var (
		version int64
		st      MigrationState
	)
	err := pool.QueryRow(ctx, sqlMigrationState).Scan(&version, &st.Dirty)
	if errors.Is(err, pgx.ErrNoRows) {
		return MigrationState{}, ErrNotMigrated
	}
	if err != nil {
		return MigrationState{}, fmt.Errorf("read migration state: %w", err)
	}
	if version < 0 {
		return MigrationState{}, fmt.Errorf("negative migration version %d", version)
	}
	st.Version = uint(version)
	if st.Version == 0 || st.Dirty {
		return st, ErrNotMigrated
	}
	return st, nil
}
