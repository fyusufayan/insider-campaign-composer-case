package config

import (
	"strings"
	"testing"
	"time"
)

func TestLoadSeedDefaults(t *testing.T) {
	cfg, err := LoadSeed(env(validEnv()))
	if err != nil {
		t.Fatalf("LoadSeed: %v", err)
	}
	want := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	if cfg.Users != 5000 || cfg.RandomSeed != 42 || !cfg.ReferenceTime.Equal(want) {
		t.Errorf("cfg = %+v", cfg)
	}
	if cfg.ReferenceTime.Location() != time.UTC {
		t.Errorf("ReferenceTime location = %v, want UTC", cfg.ReferenceTime.Location())
	}
}

func TestLoadSeedOverrides(t *testing.T) {
	cfg, err := LoadSeed(env(with(validEnv(),
		"SEED_USERS", " 30 ",
		"SEED_RANDOM_SEED", "18446744073709551615",
		"SEED_REFERENCE_TIME", "2026-12-01T10:20:30.123456+00:00",
	)))
	if err != nil {
		t.Fatalf("LoadSeed: %v", err)
	}
	want := time.Date(2026, 12, 1, 10, 20, 30, 123456000, time.UTC)
	if cfg.Users != 30 || cfg.RandomSeed != 18446744073709551615 || !cfg.ReferenceTime.Equal(want) {
		t.Errorf("cfg = %+v", cfg)
	}
}

func TestLoadSeedErrors(t *testing.T) {
	tests := []struct {
		name string
		env  map[string]string
		want string
	}{
		{"missing database url", map[string]string{}, "DATABASE_URL is required"},
		{"users zero", with(validEnv(), "SEED_USERS", "0"), "SEED_USERS"},
		{"users negative", with(validEnv(), "SEED_USERS", "-5"), "SEED_USERS"},
		{"users too large", with(validEnv(), "SEED_USERS", "1000001"), "SEED_USERS"},
		{"users not a number", with(validEnv(), "SEED_USERS", "lots"), "SEED_USERS"},
		{"seed negative", with(validEnv(), "SEED_RANDOM_SEED", "-1"), "SEED_RANDOM_SEED"},
		{"seed not a number", with(validEnv(), "SEED_RANDOM_SEED", "abc"), "SEED_RANDOM_SEED"},
		{"reference not rfc3339", with(validEnv(), "SEED_REFERENCE_TIME", "2026-11-01"), "RFC 3339"},
		{"reference not utc", with(validEnv(), "SEED_REFERENCE_TIME", "2026-11-01T03:00:00+03:00"), "UTC"},
		{"reference nanoseconds", with(validEnv(), "SEED_REFERENCE_TIME", "2026-11-01T00:00:00.000000001Z"), "microsecond"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := LoadSeed(env(tt.env))
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Errorf("err = %v, want containing %q", err, tt.want)
			}
		})
	}
}

func TestLoadSeedReportsAllErrors(t *testing.T) {
	_, err := LoadSeed(env(map[string]string{"SEED_USERS": "x", "SEED_REFERENCE_TIME": "y"}))
	if err == nil {
		t.Fatal("want error")
	}
	for _, s := range []string{"DATABASE_URL", "SEED_USERS", "SEED_REFERENCE_TIME"} {
		if !strings.Contains(err.Error(), s) {
			t.Errorf("error %q does not mention %s", err, s)
		}
	}
}
