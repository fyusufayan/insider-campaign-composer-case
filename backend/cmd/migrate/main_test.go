package main

import (
	"bytes"
	"log/slog"
	"strings"
	"testing"
)

// A missing DATABASE_URL fails before any connection attempt.
func TestRunRequiresDatabaseURL(t *testing.T) {
	var buf bytes.Buffer
	err := run(slog.New(slog.NewJSONHandler(&buf, nil)), func(string) string { return "" })
	if err == nil || !strings.Contains(err.Error(), "DATABASE_URL is required") {
		t.Fatalf("err = %v, want DATABASE_URL is required", err)
	}
}
