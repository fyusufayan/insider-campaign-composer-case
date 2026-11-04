//go:build integration

// Package testsupport starts PostgreSQL in a container for integration tests
// and hands out isolated, optionally migrated databases. It requires Docker
// and fails (never skips) when Docker is unavailable.
package testsupport

import (
	"context"
	"fmt"
	"net/url"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/testcontainers/testcontainers-go"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"

	"github.com/fyusufayan/insider-campaign-composer-case/backend/internal/platform/db"
)

// Image is the PostgreSQL major version the application targets.
const Image = "postgres:16-alpine"

const startTimeout = 2 * time.Minute

// Postgres is a running PostgreSQL container shared by a test package.
type Postgres struct {
	container *tcpostgres.PostgresContainer
	adminURL  string
	next      atomic.Int64
}

// StartPostgres starts the container. Docker being unavailable is an error.
func StartPostgres(ctx context.Context) (pg *Postgres, err error) {
	defer func() {
		// testcontainers panics in some Docker-discovery failure paths; report them as errors.
		if r := recover(); r != nil {
			err = fmt.Errorf("start postgres container (is Docker running?): %v", r)
		}
	}()
	ctx, cancel := context.WithTimeout(ctx, startTimeout)
	defer cancel()
	c, err := tcpostgres.Run(ctx, Image,
		tcpostgres.WithDatabase("composer"),
		tcpostgres.WithUsername("composer"),
		tcpostgres.WithPassword("composer"),
		tcpostgres.BasicWaitStrategies(),
	)
	if err != nil {
		if c != nil {
			_ = testcontainers.TerminateContainer(c)
		}
		return nil, fmt.Errorf("start postgres container (is Docker running?): %w", err)
	}
	u, err := c.ConnectionString(ctx, "sslmode=disable")
	if err != nil {
		_ = testcontainers.TerminateContainer(c)
		return nil, fmt.Errorf("postgres connection string: %w", err)
	}
	return &Postgres{container: c, adminURL: u}, nil
}

// Terminate stops and removes the container.
func (p *Postgres) Terminate() error { return testcontainers.TerminateContainer(p.container) }

// NewDatabase creates an empty database for one test and drops it at cleanup.
func (p *Postgres) NewDatabase(t testing.TB) string {
	t.Helper()
	ctx := context.Background()
	name := fmt.Sprintf("test_%d", p.next.Add(1))

	admin, err := pgx.Connect(ctx, p.adminURL)
	if err != nil {
		t.Fatalf("connect admin: %v", err)
	}
	defer admin.Close(ctx)
	if _, err := admin.Exec(ctx, "CREATE DATABASE "+pgx.Identifier{name}.Sanitize()); err != nil {
		t.Fatalf("create database %s: %v", name, err)
	}
	t.Cleanup(func() {
		c, err := pgx.Connect(ctx, p.adminURL)
		if err != nil {
			t.Errorf("cleanup connect: %v", err)
			return
		}
		defer c.Close(ctx)
		if _, err := c.Exec(ctx, "DROP DATABASE "+pgx.Identifier{name}.Sanitize()+" WITH (FORCE)"); err != nil {
			t.Errorf("drop database %s: %v", name, err)
		}
	})

	u, err := url.Parse(p.adminURL)
	if err != nil {
		t.Fatalf("parse admin url: %v", err)
	}
	u.Path = "/" + name
	return u.String()
}

// NewMigratedDatabase creates a database with all migrations applied.
func (p *Postgres) NewMigratedDatabase(t testing.TB) string {
	t.Helper()
	dbURL := p.NewDatabase(t)
	if err := db.MigrateUp(context.Background(), dbURL); err != nil {
		t.Fatalf("migrate up: %v", err)
	}
	return dbURL
}

// Pool opens an application pool (UTC sessions) closed at cleanup.
func Pool(t testing.TB, dbURL string) *pgxpool.Pool {
	t.Helper()
	pool, err := db.NewPool(context.Background(), dbURL, "composer-test")
	if err != nil {
		t.Fatalf("open pool: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}
