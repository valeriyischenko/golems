package llm

import (
	"strings"

	"github.com/levmv/golems/pkg/openai"
)

type ProviderOption func(*openai.ClientConfig)

type Registry struct {
	providers map[string]Client
}

func NewRegistry() *Registry {
	return &Registry{
		providers: make(map[string]Client),
	}
}

// WithProvider registers a provider by name. It handles the specific defaults natively.
func (r *Registry) WithProvider(name, token string, opts ...ProviderOption) *Registry {
	var cfg openai.ClientConfig

	switch name {
	case "deepseek":
		cfg = openai.DeepSeekConfig(token)
	case "openrouter":
		cfg = openai.OpenRouterConfig(token, "", "")
	case "ollama":
		cfg = openai.OllamaConfig()
	default:
		cfg = openai.DefaultConfig(token)
	}

	for _, opt := range opts {
		opt(&cfg)
	}

	r.providers[name] = newOpenAIAdapter(name, openai.NewClientWithConfig(cfg))
	return r
}

// WithBaseURL points a provider at a different endpoint: a self-hosted or
// proxied deployment, a local server on a non-default port, or a test double.
// It is applied after the provider's own defaults, so it overrides them while
// leaving that provider's headers and auth handling in place. An empty url is
// ignored, so a caller may pass an optional configuration value through
// unconditionally.
func WithBaseURL(url string) ProviderOption {
	return func(cfg *openai.ClientConfig) {
		if url = strings.TrimSpace(url); url != "" {
			cfg.BaseURL = url
		}
	}
}

// WithAppAttribution is purely for OpenRouter.
func WithAppAttribution(title, url string) ProviderOption {
	return func(cfg *openai.ClientConfig) {
		if cfg.Header == nil {
			cfg.Header = make(map[string][]string)
		}
		cfg.Header.Set("HTTP-Referer", url)
		cfg.Header.Set("X-Title", title)
	}
}

func DeepSeek(token string) *Registry {
	return NewRegistry().WithProvider("deepseek", token)
}

func MustModel(uri, token string, opts ...ProviderOption) Model {
	parts := strings.SplitN(uri, "/", 2)
	if len(parts) != 2 {
		panic("invalid model uri (expected provider/model)")
	}
	provider := parts[0]

	r := NewRegistry().WithProvider(provider, token, opts...)
	m, err := r.Model(uri)
	if err != nil {
		panic(err)
	}
	return m
}
