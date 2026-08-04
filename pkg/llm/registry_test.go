package llm

import (
	"testing"

	"github.com/levmv/golems/pkg/openai"
)

func TestWithBaseURLOverridesProviderDefault(t *testing.T) {
	cfg := openai.DeepSeekConfig("token")
	WithBaseURL("http://127.0.0.1:8011/v1")(&cfg)

	if cfg.BaseURL != "http://127.0.0.1:8011/v1" {
		t.Fatalf("base url = %q, want the override", cfg.BaseURL)
	}
	if cfg.AuthToken != "token" {
		t.Fatalf("auth token = %q, want the provider's own value untouched", cfg.AuthToken)
	}
}

func TestWithBaseURLIgnoresEmpty(t *testing.T) {
	cfg := openai.DeepSeekConfig("token")
	want := cfg.BaseURL

	for _, url := range []string{"", "   "} {
		WithBaseURL(url)(&cfg)
		if cfg.BaseURL != want {
			t.Fatalf("base url = %q after WithBaseURL(%q), want the default %q", cfg.BaseURL, url, want)
		}
	}
}
