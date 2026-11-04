package llm

import (
	"errors"
	"fmt"

	"github.com/fyusufayan/insider-campaign-composer-case/backend/internal/platform/config"
)

// ErrAnthropicNotAvailable is returned while the Anthropic client is not yet
// implemented (Slice 1b). Selection never falls back to Demo instead.
var ErrAnthropicNotAvailable = errors.New("llm: the anthropic provider is not available until slice 1b; use LLM_PROVIDER=demo or unset ANTHROPIC_API_KEY")

// New builds the Client for the provider resolved by configuration
// (ARCHITECTURE §7.3). It is called once at process startup. Unit tests do
// not use it; they construct Scripted directly.
func New(cfg config.LLM, demoResponders map[string]DemoResponder) (Client, error) {
	switch cfg.Provider {
	case config.LLMProviderDemo:
		return NewDemo(demoResponders), nil
	case config.LLMProviderAnthropic:
		return nil, ErrAnthropicNotAvailable
	default:
		return nil, fmt.Errorf("llm: unresolved provider %q", cfg.Provider)
	}
}
