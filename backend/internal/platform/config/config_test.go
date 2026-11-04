package config

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"testing"
	"time"
)

const testKey = "sk-ant-test-0123456789"

func env(m map[string]string) func(string) string {
	return func(k string) string { return m[k] }
}

func validEnv() map[string]string {
	return map[string]string{"DATABASE_URL": "postgres://u:p@localhost:5432/db"}
}

func with(base map[string]string, kv ...string) map[string]string {
	out := make(map[string]string, len(base)+len(kv)/2)
	for k, v := range base {
		out[k] = v
	}
	for i := 0; i+1 < len(kv); i += 2 {
		out[kv[i]] = kv[i+1]
	}
	return out
}

func TestLoadDefaults(t *testing.T) {
	cfg, err := Load(env(validEnv()))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.HTTPAddr != ":8080" {
		t.Errorf("HTTPAddr = %q, want :8080", cfg.HTTPAddr)
	}
	if cfg.DatabaseURL.Reveal() != "postgres://u:p@localhost:5432/db" {
		t.Errorf("DatabaseURL = %q", cfg.DatabaseURL.Reveal())
	}
	if cfg.LLM.Requested != LLMProviderAuto || cfg.LLM.Provider != LLMProviderDemo {
		t.Errorf("LLM = %+v, want auto -> demo", cfg.LLM)
	}
	if !cfg.LLM.FellBackToDemo() {
		t.Error("FellBackToDemo = false, want true")
	}
}

func TestLoadTrimsValues(t *testing.T) {
	cfg, err := Load(env(with(validEnv(), "HTTP_ADDR", "  127.0.0.1:9000 ", "LLM_PROVIDER", " demo ")))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.HTTPAddr != "127.0.0.1:9000" || cfg.LLM.Provider != LLMProviderDemo {
		t.Errorf("cfg = %+v", cfg)
	}
}

func TestLoadErrors(t *testing.T) {
	tests := []struct {
		name    string
		env     map[string]string
		wantErr []string
	}{
		{"missing database url", map[string]string{}, []string{"DATABASE_URL is required"}},
		{"blank database url", map[string]string{"DATABASE_URL": "   "}, []string{"DATABASE_URL is required"}},
		{"addr without port", with(validEnv(), "HTTP_ADDR", "localhost"), []string{"HTTP_ADDR"}},
		{"addr non-numeric port", with(validEnv(), "HTTP_ADDR", ":http"), []string{"HTTP_ADDR", "port must be a number"}},
		{"addr port out of range", with(validEnv(), "HTTP_ADDR", ":70000"), []string{"HTTP_ADDR", "port must be a number"}},
		{"unknown provider", with(validEnv(), "LLM_PROVIDER", "openai"), []string{"LLM_PROVIDER must be one of"}},
		{"provider is case sensitive", with(validEnv(), "LLM_PROVIDER", "Demo"), []string{"LLM_PROVIDER must be one of"}},
		{"anthropic without key", with(validEnv(), "LLM_PROVIDER", "anthropic"), []string{"requires ANTHROPIC_API_KEY"}},
		{
			"all problems reported together",
			map[string]string{"HTTP_ADDR": "bad", "LLM_PROVIDER": "x"},
			[]string{"DATABASE_URL is required", "HTTP_ADDR", "LLM_PROVIDER must be one of"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := Load(env(tt.env))
			if err == nil {
				t.Fatal("Load succeeded, want error")
			}
			for _, want := range tt.wantErr {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("error %q does not contain %q", err, want)
				}
			}
		})
	}
}

// The selection table of ARCHITECTURE §7.3. Anthropic is never silently
// replaced by demo when it was selected.
func TestLLMProviderSelection(t *testing.T) {
	tests := []struct {
		name         string
		provider     string
		key          string
		wantErr      bool
		wantProvider LLMProvider
		wantKey      string
		wantFallback bool
	}{
		{"unset, no key", "", "", false, LLMProviderDemo, "", true},
		{"auto, no key", "auto", "", false, LLMProviderDemo, "", true},
		{"auto, blank key", "auto", "   \t", false, LLMProviderDemo, "", true},
		{"auto, key", "auto", testKey, false, LLMProviderAnthropic, testKey, false},
		{"auto, padded key", "auto", "  " + testKey + "\n", false, LLMProviderAnthropic, testKey, false},
		{"anthropic, no key", "anthropic", "", true, "", "", false},
		{"anthropic, blank key", "anthropic", "  ", true, "", "", false},
		{"anthropic, key", "anthropic", testKey, false, LLMProviderAnthropic, testKey, false},
		{"demo, no key", "demo", "", false, LLMProviderDemo, "", false},
		{"demo, key ignored", "demo", testKey, false, LLMProviderDemo, "", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg, err := Load(env(with(validEnv(), "LLM_PROVIDER", tt.provider, "ANTHROPIC_API_KEY", tt.key)))
			if tt.wantErr {
				if err == nil {
					t.Fatalf("Load succeeded with %+v, want error", cfg.LLM)
				}
				return
			}
			if err != nil {
				t.Fatalf("Load: %v", err)
			}
			if cfg.LLM.Provider != tt.wantProvider {
				t.Errorf("Provider = %q, want %q", cfg.LLM.Provider, tt.wantProvider)
			}
			if got := cfg.LLM.APIKey.Reveal(); got != tt.wantKey {
				t.Errorf("APIKey = %q, want %q", got, tt.wantKey)
			}
			if got := cfg.LLM.FellBackToDemo(); got != tt.wantFallback {
				t.Errorf("FellBackToDemo = %v, want %v", got, tt.wantFallback)
			}
		})
	}
}

func TestSecretsNeverRendered(t *testing.T) {
	const dbURL = "postgres://user:hunter2@db:5432/composer"
	cfg, err := Load(env(map[string]string{"DATABASE_URL": dbURL, "ANTHROPIC_API_KEY": testKey}))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	var logBuf bytes.Buffer
	slog.New(slog.NewJSONHandler(&logBuf, nil)).Info("config", "cfg", cfg, "key", cfg.LLM.APIKey)
	jsonBytes, err := json.Marshal(cfg)
	if err != nil {
		t.Fatalf("json.Marshal: %v", err)
	}

	renderings := map[string]string{
		"%v":   fmt.Sprintf("%v", cfg),
		"%+v":  fmt.Sprintf("%+v", cfg),
		"%#v":  fmt.Sprintf("%#v", cfg),
		"%q":   fmt.Sprintf("%q", cfg.LLM.APIKey),
		"slog": logBuf.String(),
		"json": string(jsonBytes),
	}
	for name, out := range renderings {
		for _, secret := range []string{testKey, "hunter2"} {
			if strings.Contains(out, secret) {
				t.Errorf("%s rendering leaks %q: %s", name, secret, out)
			}
		}
		if !strings.Contains(out, redacted) {
			t.Errorf("%s rendering lacks %s marker: %s", name, redacted, out)
		}
	}
}

func TestSegmentTaskConfig(t *testing.T) {
	cfg, err := Load(env(validEnv()))
	if err != nil {
		t.Fatal(err)
	}
	want := AITask{Model: "claude-haiku-5-5", Effort: "low", Timeout: 45 * time.Second, MaxTokens: 4000}
	if cfg.Segment != want {
		t.Errorf("defaults = %+v, want %+v", cfg.Segment, want)
	}
	cfg, err = Load(env(with(validEnv(),
		"CLAUDE_SEGMENT_MODEL", "claude-sonnet-5-5", "CLAUDE_SEGMENT_EFFORT", "medium",
		"CLAUDE_SEGMENT_TIMEOUT", "30s", "CLAUDE_SEGMENT_MAX_TOKENS", "8000")))
	if err != nil {
		t.Fatal(err)
	}
	want = AITask{Model: "claude-sonnet-5-5", Effort: "medium", Timeout: 30 * time.Second, MaxTokens: 8000}
	if cfg.Segment != want {
		t.Errorf("overrides = %+v, want %+v", cfg.Segment, want)
	}
	for _, kv := range [][2]string{
		{"CLAUDE_SEGMENT_MODEL", "two words"},
		{"CLAUDE_SEGMENT_EFFORT", "extreme"},
		{"CLAUDE_SEGMENT_TIMEOUT", "soon"},
		{"CLAUDE_SEGMENT_TIMEOUT", "-1s"},
		{"CLAUDE_SEGMENT_TIMEOUT", "1h"},
		{"CLAUDE_SEGMENT_MAX_TOKENS", "0"},
		{"CLAUDE_SEGMENT_MAX_TOKENS", "lots"},
	} {
		if _, err := Load(env(with(validEnv(), kv[0], kv[1]))); err == nil || !strings.Contains(err.Error(), kv[0]) {
			t.Errorf("%s=%q: err = %v", kv[0], kv[1], err)
		}
	}
}

func TestLLMMaxRetries(t *testing.T) {
	cfg, err := Load(env(validEnv()))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.LLM.MaxRetries != 2 {
		t.Errorf("default MaxRetries = %d, want 2", cfg.LLM.MaxRetries)
	}
	for _, v := range []string{"0", "5"} {
		cfg, err := Load(env(with(validEnv(), "CLAUDE_MAX_RETRIES", v)))
		if err != nil || strconv.Itoa(cfg.LLM.MaxRetries) != v {
			t.Errorf("CLAUDE_MAX_RETRIES=%s: %d, %v", v, cfg.LLM.MaxRetries, err)
		}
	}
	for _, v := range []string{"-1", "6", "two"} {
		if _, err := Load(env(with(validEnv(), "CLAUDE_MAX_RETRIES", v))); err == nil || !strings.Contains(err.Error(), "CLAUDE_MAX_RETRIES") {
			t.Errorf("CLAUDE_MAX_RETRIES=%q: err = %v", v, err)
		}
	}
}
