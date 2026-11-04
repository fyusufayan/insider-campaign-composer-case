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
	"time"
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
	// MaxRetries is CLAUDE_MAX_RETRIES: the Anthropic SDK's automatic
	// retries per model request (connection errors, 408, 409, 429, 5xx).
	MaxRetries int
}

// FellBackToDemo reports whether auto resolved to demo because no API key is set.
// Callers log a warning in that case.
func (l LLM) FellBackToDemo() bool {
	return l.Requested == LLMProviderAuto && l.Provider == LLMProviderDemo
}

// AITask is the model configuration of one AI use case (ARCHITECTURE §7.3).
// Model names come only from here (invariant 17).
type AITask struct {
	Model     string
	Effort    string
	Timeout   time.Duration
	MaxTokens int
}

// Defaults of the segment interpretation task (CLAUDE_SEGMENT_*).
const (
	defaultSegmentModel     = "claude-opus-5-5"
	defaultSegmentEffort    = "low"
	defaultSegmentTimeout   = 45 * time.Second
	defaultSegmentMaxTokens = 4000
)

// Effort values accepted for CLAUDE_*_EFFORT.
var allowedEfforts = []string{"low", "medium", "high", "max"}

const (
	maxAITimeout   = 10 * time.Minute
	maxAIMaxTokens = 64000
)

// CLAUDE_MAX_RETRIES bounds: every retry is another provider request inside
// the same task timeout, so the ceiling keeps amplification small.
const (
	defaultLLMMaxRetries = 2
	maxLLMMaxRetries     = 5
)

// Config is the validated configuration of the api process.
type Config struct {
	DatabaseURL Secret // may contain credentials
	HTTPAddr    string
	LLM         LLM
	// Segment configures brief interpretation (CLAUDE_SEGMENT_*).
	Segment AITask
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
	llm.MaxRetries = defaultLLMMaxRetries
	if v := get("CLAUDE_MAX_RETRIES"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 0 || n > maxLLMMaxRetries {
			errs = append(errs, fmt.Errorf("CLAUDE_MAX_RETRIES must be an integer in [0, %d] (got %q)", maxLLMMaxRetries, v))
		} else {
			llm.MaxRetries = n
		}
	}
	cfg.LLM = llm

	seg, segErrs := loadAITask(get, "CLAUDE_SEGMENT", AITask{
		Model: defaultSegmentModel, Effort: defaultSegmentEffort,
		Timeout: defaultSegmentTimeout, MaxTokens: defaultSegmentMaxTokens,
	})
	errs = append(errs, segErrs...)
	cfg.Segment = seg

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

// loadAITask reads <prefix>_MODEL, _EFFORT, _TIMEOUT and _MAX_TOKENS.
func loadAITask(get func(string) string, prefix string, def AITask) (AITask, []error) {
	var errs []error
	t := def
	if v := get(prefix + "_MODEL"); v != "" {
		t.Model = v
	}
	if strings.ContainsAny(t.Model, " \t\n") {
		errs = append(errs, fmt.Errorf("%s_MODEL must not contain whitespace", prefix))
	}
	if v := get(prefix + "_EFFORT"); v != "" {
		t.Effort = v
	}
	if !contains(allowedEfforts, t.Effort) {
		errs = append(errs, fmt.Errorf("%s_EFFORT must be one of %s (got %q)", prefix, strings.Join(allowedEfforts, ", "), t.Effort))
	}
	if v := get(prefix + "_TIMEOUT"); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil || d <= 0 || d > maxAITimeout {
			errs = append(errs, fmt.Errorf("%s_TIMEOUT must be a duration in (0, %s] (got %q)", prefix, maxAITimeout, v))
		} else {
			t.Timeout = d
		}
	}
	if v := get(prefix + "_MAX_TOKENS"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > maxAIMaxTokens {
			errs = append(errs, fmt.Errorf("%s_MAX_TOKENS must be an integer in [1, %d] (got %q)", prefix, maxAIMaxTokens, v))
		} else {
			t.MaxTokens = n
		}
	}
	return t, errs
}

func contains(set []string, s string) bool {
	for _, v := range set {
		if v == s {
			return true
		}
	}
	return false
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
