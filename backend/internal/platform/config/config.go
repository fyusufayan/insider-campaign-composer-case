// Package config loads and validates process configuration from the environment.
// It is the only place that reads environment variables; everything else receives
// a validated Config through constructors.
package config

import (
	"errors"
	"fmt"
	"log/slog"
	"net"
	"strconv"
	"strings"
)

// LLMProvider is the LLM implementation selected once at startup.
type LLMProvider string

const (
	LLMProviderAuto      LLMProvider = "auto"
	LLMProviderAnthropic LLMProvider = "anthropic"
	LLMProviderDemo      LLMProvider = "demo"
)

const defaultHTTPAddr = ":8080"

// Secret holds a sensitive value. Its String, GoString and LogValue forms are
// redacted so it cannot leak through fmt or slog; use Reveal to read it.
type Secret string

const redacted = "[REDACTED]"

func (s Secret) Reveal() string               { return string(s) }
func (s Secret) String() string               { return redacted }
func (s Secret) GoString() string             { return redacted }
func (s Secret) LogValue() slog.Value         { return slog.StringValue(redacted) }
func (s Secret) MarshalText() ([]byte, error) { return []byte(redacted), nil }

// LLM is the resolved LLM provider selection.
type LLM struct {
	// Requested is the LLM_PROVIDER value as configured (default auto).
	Requested LLMProvider
	// Provider is the effective provider: anthropic or demo, never auto.
	Provider LLMProvider
	// APIKey is ANTHROPIC_API_KEY, trimmed. Empty when Provider is demo.
	APIKey Secret
}

// FellBackToDemo reports whether auto resolved to demo because no API key is set.
// Callers log a warning in that case.
func (l LLM) FellBackToDemo() bool {
	return l.Requested == LLMProviderAuto && l.Provider == LLMProviderDemo
}

// Config is the validated configuration of the api process.
type Config struct {
	DatabaseURL Secret // may contain credentials
	HTTPAddr    string
	LLM         LLM
}

// Load reads configuration through getenv (os.Getenv in production) and
// validates it. All problems are reported together.
func Load(getenv func(string) string) (Config, error) {
	var errs []error
	get := func(key string) string { return strings.TrimSpace(getenv(key)) }

	cfg := Config{
		DatabaseURL: Secret(get("DATABASE_URL")),
		HTTPAddr:    get("HTTP_ADDR"),
	}
	if cfg.DatabaseURL == "" {
		errs = append(errs, errors.New("DATABASE_URL is required"))
	}
	if cfg.HTTPAddr == "" {
		cfg.HTTPAddr = defaultHTTPAddr
	}
	if err := validateListenAddr(cfg.HTTPAddr); err != nil {
		errs = append(errs, fmt.Errorf("HTTP_ADDR: %w", err))
	}

	llm, err := resolveLLM(get("LLM_PROVIDER"), get("ANTHROPIC_API_KEY"))
	if err != nil {
		errs = append(errs, err)
	}
	cfg.LLM = llm

	if len(errs) > 0 {
		return Config{}, fmt.Errorf("invalid configuration: %w", errors.Join(errs...))
	}
	return cfg, nil
}

// resolveLLM applies the provider selection table (ARCHITECTURE §7.3).
func resolveLLM(requested, apiKey string) (LLM, error) {
	req := LLMProvider(requested)
	if req == "" {
		req = LLMProviderAuto
	}
	hasKey := apiKey != ""

	switch req {
	case LLMProviderAuto:
		if hasKey {
			return LLM{Requested: req, Provider: LLMProviderAnthropic, APIKey: Secret(apiKey)}, nil
		}
		return LLM{Requested: req, Provider: LLMProviderDemo}, nil
	case LLMProviderAnthropic:
		if !hasKey {
			return LLM{}, errors.New("LLM_PROVIDER=anthropic requires ANTHROPIC_API_KEY")
		}
		return LLM{Requested: req, Provider: LLMProviderAnthropic, APIKey: Secret(apiKey)}, nil
	case LLMProviderDemo:
		// The key is ignored and not retained.
		return LLM{Requested: req, Provider: LLMProviderDemo}, nil
	default:
		return LLM{}, fmt.Errorf("LLM_PROVIDER must be one of auto, anthropic, demo (got %q)", requested)
	}
}

func validateListenAddr(addr string) error {
	_, port, err := net.SplitHostPort(addr)
	if err != nil {
		return fmt.Errorf("must be host:port: %w", err)
	}
	n, err := strconv.Atoi(port)
	if err != nil || n < 0 || n > 65535 {
		return fmt.Errorf("port must be a number between 0 and 65535 (got %q)", port)
	}
	return nil
}
