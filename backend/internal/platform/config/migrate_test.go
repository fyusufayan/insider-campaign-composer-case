package config

import (
	"strings"
	"testing"
)

func TestLoadMigrate(t *testing.T) {
	cfg, err := LoadMigrate(env(map[string]string{"DATABASE_URL": " postgres://u:p@h/db "}))
	if err != nil {
		t.Fatalf("LoadMigrate: %v", err)
	}
	if cfg.DatabaseURL.Reveal() != "postgres://u:p@h/db" {
		t.Errorf("DatabaseURL not trimmed: %q", cfg.DatabaseURL.Reveal())
	}
}

func TestLoadMigrateRequiresDatabaseURL(t *testing.T) {
	_, err := LoadMigrate(env(map[string]string{"DATABASE_URL": "  "}))
	if err == nil || !strings.Contains(err.Error(), "DATABASE_URL is required") {
		t.Fatalf("err = %v, want DATABASE_URL is required", err)
	}
}
