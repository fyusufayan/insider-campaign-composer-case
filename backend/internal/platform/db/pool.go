// Package db creates the PostgreSQL connection pool.
package db

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

const pingTimeout = 5 * time.Second

// NewPool creates a pgx pool for databaseURL and verifies connectivity.
// Every session runs with TimeZone=UTC ("last N days" and ages are evaluated in
// UTC, ARCHITECTURE §18) and reports appName as application_name.
func NewPool(ctx context.Context, databaseURL, appName string) (*pgxpool.Pool, error) {
	cfg, err := poolConfig(databaseURL, appName)
	if err != nil {
		return nil, err
	}
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("create pool: %w", err)
	}
	pingCtx, cancel := context.WithTimeout(ctx, pingTimeout)
	defer cancel()
	if err := pool.Ping(pingCtx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("ping database: %w", err)
	}
	return pool, nil
}

func poolConfig(databaseURL, appName string) (*pgxpool.Config, error) {
	cfg, err := pgxpool.ParseConfig(databaseURL)
	if err != nil {
		// pgconn redacts the password in parse errors; a test guards this.
		return nil, fmt.Errorf("parse DATABASE_URL: %w", err)
	}
	params := cfg.ConnConfig.RuntimeParams
	// Session parameters set here override any given in the URL.
	for k := range params {
		if strings.EqualFold(k, "timezone") {
			delete(params, k)
		}
	}
	params["timezone"] = "UTC"
	if appName != "" {
		params["application_name"] = appName
	}
	return cfg, nil
}
