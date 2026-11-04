package logging

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"testing"
)

func TestMaskPhone(t *testing.T) {
	tests := map[string]string{
		"+905321234567": "+90********67",
		"+14155550100":  "+14*******00",
		"05321234567":   "*********67",
		"+9053":         "*****",
		"":              "",
	}
	for in, want := range tests {
		if got := MaskPhone(in); got != want {
			t.Errorf("MaskPhone(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestMaskEmail(t *testing.T) {
	tests := map[string]string{
		"ayse.yilmaz@example.com": "a***@example.com",
		"ş@örnek.com.tr":          "ş***@örnek.com.tr",
		"no-at-sign":              "**********",
		"@example.com":            "************",
		"":                        "",
	}
	for in, want := range tests {
		if got := MaskEmail(in); got != want {
			t.Errorf("MaskEmail(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestMaskText(t *testing.T) {
	tests := []struct{ in, want string }{
		{"send to ayse@example.com failed", "send to a***@example.com failed"},
		{"sms +905321234567 rejected", "sms +90********67 rejected"},
		{"a@b.co and +905551112233", "a***@b.co and +90********33"},
		// Non-contact values are left untouched.
		{"campaign 3f2b1c9e-8d4a-4b6f-9c1e-2a7d5e8f0b13 attempt 2", "campaign 3f2b1c9e-8d4a-4b6f-9c1e-2a7d5e8f0b13 attempt 2"},
		{"2026-11-03T10:00:00+03:00 took 1234567890ms", "2026-11-03T10:00:00+03:00 took 1234567890ms"},
		{"1 + 1 = 2", "1 + 1 = 2"},
	}
	for _, tt := range tests {
		if got := MaskText(tt.in); got != tt.want {
			t.Errorf("MaskText(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

func decode(t *testing.T, line []byte) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(line, &m); err != nil {
		t.Fatalf("log line is not JSON: %v: %s", err, line)
	}
	return m
}

func TestLoggerMasksContactsEverywhere(t *testing.T) {
	var buf bytes.Buffer
	log := New(&buf, slog.LevelInfo)

	log.Info("delivery to ayse@example.com",
		"destination", "+905321234567",
		"phone", Phone("+905321234567"),
		"email", Email("mehmet@example.org"),
		"err", fmt.Errorf("provider rejected %s: %w", "+905551112233", errors.New("bad number")),
		slog.Group("user", "email", "zeynep@example.net"),
	)

	out := buf.String()
	for _, raw := range []string{"ayse@", "5321234567", "mehmet@", "5551112233", "zeynep@"} {
		if strings.Contains(out, raw) {
			t.Errorf("log output leaks %q: %s", raw, out)
		}
	}
	m := decode(t, buf.Bytes())
	if m["msg"] != "delivery to a***@example.com" {
		t.Errorf("msg = %v", m["msg"])
	}
	if m["phone"] != "+90********67" || m["destination"] != "+90********67" {
		t.Errorf("phone = %v, destination = %v", m["phone"], m["destination"])
	}
	if m["email"] != "m***@example.org" {
		t.Errorf("email = %v", m["email"])
	}
	if m["err"] != "provider rejected +90********33: bad number" {
		t.Errorf("err = %v", m["err"])
	}
	if user, _ := m["user"].(map[string]any); user["email"] != "z***@example.net" {
		t.Errorf("user = %v", m["user"])
	}
}

func TestLoggerAddsRequestIDFromContext(t *testing.T) {
	var buf bytes.Buffer
	log := New(&buf, slog.LevelInfo).With("component", "test")

	ctx := WithRequestID(context.Background(), "req-123")
	log.InfoContext(ctx, "with id")
	log.InfoContext(context.Background(), "without id")

	lines := bytes.Split(bytes.TrimSpace(buf.Bytes()), []byte("\n"))
	if len(lines) != 2 {
		t.Fatalf("got %d lines, want 2: %s", len(lines), buf.String())
	}
	first, second := decode(t, lines[0]), decode(t, lines[1])
	if first["request_id"] != "req-123" || first["component"] != "test" {
		t.Errorf("first line = %v", first)
	}
	if _, ok := second["request_id"]; ok {
		t.Errorf("second line has request_id: %v", second)
	}
}

func TestLoggerRespectsLevel(t *testing.T) {
	var buf bytes.Buffer
	log := New(&buf, slog.LevelWarn)
	log.Info("dropped")
	log.Warn("kept")
	if strings.Contains(buf.String(), "dropped") || !strings.Contains(buf.String(), "kept") {
		t.Errorf("unexpected output: %s", buf.String())
	}
}
