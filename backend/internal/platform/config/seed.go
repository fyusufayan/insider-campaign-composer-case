package config

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// Seed defaults (ARCHITECTURE §16 plus the fixed reference time that makes the
// dataset reproducible; override SEED_REFERENCE_TIME, e.g. via `make seed-today`,
// for demos run long after the default date).
const (
	DefaultSeedUsers         = 5000
	DefaultSeedRandomSeed    = 42
	DefaultSeedReferenceTime = "2026-11-01T00:00:00Z"

	// MaxSeedUsers bounds SEED_USERS to keep a mistyped value from producing
	// an unbounded load.
	MaxSeedUsers = 1_000_000
)

// Seed is the validated configuration of the seed command.
type Seed struct {
	DatabaseURL Secret
	Users       int
	RandomSeed  uint64
	// ReferenceTime is the UTC instant all generated timestamps are derived
	// from; it has at most microsecond precision (PostgreSQL's resolution).
	ReferenceTime time.Time
}

// LoadSeed reads the seed command's configuration through getenv and
// validates it. All problems are reported together.
func LoadSeed(getenv func(string) string) (Seed, error) {
	var errs []error
	get := func(key string) string { return strings.TrimSpace(getenv(key)) }

	cfg := Seed{DatabaseURL: Secret(get("DATABASE_URL"))}
	if cfg.DatabaseURL == "" {
		errs = append(errs, errors.New("DATABASE_URL is required"))
	}

	cfg.Users = DefaultSeedUsers
	if v := get("SEED_USERS"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > MaxSeedUsers {
			errs = append(errs, fmt.Errorf("SEED_USERS must be an integer between 1 and %d (got %q)", MaxSeedUsers, v))
		}
		cfg.Users = n
	}

	cfg.RandomSeed = DefaultSeedRandomSeed
	if v := get("SEED_RANDOM_SEED"); v != "" {
		n, err := strconv.ParseUint(v, 10, 64)
		if err != nil {
			errs = append(errs, fmt.Errorf("SEED_RANDOM_SEED must be an unsigned 64-bit integer (got %q)", v))
		}
		cfg.RandomSeed = n
	}

	ref := get("SEED_REFERENCE_TIME")
	if ref == "" {
		ref = DefaultSeedReferenceTime
	}
	t, err := ParseReferenceTime(ref)
	if err != nil {
		errs = append(errs, fmt.Errorf("SEED_REFERENCE_TIME: %w", err))
	}
	cfg.ReferenceTime = t

	if len(errs) > 0 {
		return Seed{}, fmt.Errorf("invalid configuration: %w", errors.Join(errs...))
	}
	return cfg, nil
}

// ParseReferenceTime parses an RFC 3339 timestamp that must be in UTC (offset
// zero) and have at most microsecond precision.
func ParseReferenceTime(s string) (time.Time, error) {
	t, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		return time.Time{}, fmt.Errorf("must be an RFC 3339 timestamp (got %q)", s)
	}
	if _, off := t.Zone(); off != 0 {
		return time.Time{}, fmt.Errorf("must be in UTC, e.g. 2026-11-01T00:00:00Z (got %q)", s)
	}
	if t.Nanosecond()%int(time.Microsecond) != 0 {
		return time.Time{}, fmt.Errorf("must have at most microsecond precision (got %q)", s)
	}
	return t.UTC(), nil
}
