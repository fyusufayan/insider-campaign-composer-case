package llm

import (
	"fmt"
	"log/slog"

	"github.com/fyusufayan/insider-campaign-composer-case/backend/internal/platform/config"
)

// New builds the Client for the provider resolved by configuration
// (ARCHITECTURE §7.3). It is called once at process startup; the choice is
// immutable for the process lifetime, and an Anthropic client never falls
// back to Demo (invariant 17). It performs no network I/O. Unit tests do not
// use it; they construct Scripted directly.
func New(cfg config.LLM, demoResponders map[string]DemoResponder, log *slog.Logger) (Client, error) {
	switch cfg.Provider {
	case config.LLMProviderDemo:
		return NewDemo(demoResponders), nil
	case config.LLMProviderAnthropic:
		c, err := NewAnthropic(AnthropicConfig{APIKey: cfg.APIKey.Reveal(), MaxRetries: cfg.MaxRetries, Logger: log})
		if err != nil {
			return nil, err
		}
		return c, nil
	default:
		return nil, fmt.Errorf("llm: unresolved provider %q", cfg.Provider)
	}
}
