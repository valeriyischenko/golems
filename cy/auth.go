package main

import (
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/levmv/golems/cy/internal/state"
	"github.com/levmv/golems/cy/internal/ui"
)

var providerCatalog = []string{"deepseek", "openai", "openrouter"}

type serviceSpec struct {
	name          string
	env           string
	description   string
	credentialURL string
	search        bool
	fetch         bool
}

// The order also defines provider preference after filtering by capability.
var serviceCatalog = []serviceSpec{
	{name: "tavily", env: "TAVILY_API_KEY", description: "web search", credentialURL: "https://app.tavily.com", search: true},
	{name: "firecrawl", env: "FIRECRAWL_API_KEY", description: "web fetch fallback", credentialURL: "https://www.firecrawl.dev/app/api-keys", fetch: true},
	{name: "exa", env: "EXA_API_KEY", description: "web search and fetch", credentialURL: "https://dashboard.exa.ai/api-keys", search: true, fetch: true},
}

type providerStatus = ui.ProviderStatus

func credentialForProvider(store *state.Store, provider string) (token, source string, err error) {
	if token = providerEnvToken(provider); token != "" {
		return token, "environment override", nil
	}
	if store == nil {
		return "", "none", nil
	}
	token, ok, err := store.APIKey(provider)
	if err != nil {
		return "", "", err
	}
	if ok {
		return token, "auth store", nil
	}
	return "", "none", nil
}

func providerEnvToken(provider string) string {
	name := providerEnvName(provider)
	if name == "" {
		return ""
	}
	return strings.TrimSpace(os.Getenv(name))
}

func providerEnvName(provider string) string {
	switch provider {
	case "deepseek":
		return "DEEPSEEK_API_KEY"
	case "openai":
		return "OPENAI_API_KEY"
	case "openrouter":
		return "OPENROUTER_API_KEY"
	}
	for _, service := range serviceCatalog {
		if service.name == provider {
			return service.env
		}
	}
	return ""
}

// Classified where it is built rather than at each return, since both the
// startup check and the per-turn one produce it and it is the same problem
// either way: nothing to do but supply a credential and run again.
func missingProviderCredentialError(provider, modelURI string) error {
	return asConfigError(fmt.Errorf(
		"%s API key is empty for model %q; set %s or use /login %s in interactive Cy",
		provider,
		modelURI,
		providerEnvName(provider),
		provider,
	))
}

func listProviderStatus(store *state.Store) ([]providerStatus, error) {
	statuses := make([]providerStatus, 0, len(providerCatalog)+len(serviceCatalog))
	for _, provider := range providerCatalog {
		_, source, err := credentialForProvider(store, provider)
		if err != nil {
			return nil, err
		}
		statuses = append(statuses, providerStatus{Name: provider, Source: source, Category: "Model providers", Description: "model provider"})
	}
	for _, service := range serviceCatalog {
		_, source, err := credentialForProvider(store, service.name)
		if err != nil {
			return nil, err
		}
		statuses = append(statuses, providerStatus{
			Name:          service.name,
			Source:        source,
			Category:      "Services",
			Description:   service.description,
			CredentialURL: service.credentialURL,
		})
	}
	return statuses, nil
}

func isLoginProvider(provider string) bool {
	provider = strings.ToLower(strings.TrimSpace(provider))
	for _, known := range loginProviderCatalog() {
		if provider == known {
			return true
		}
	}
	return false
}

func isModelLoginProvider(provider string) bool {
	provider = strings.ToLower(strings.TrimSpace(provider))
	for _, known := range providerCatalog {
		if provider == known {
			return true
		}
	}
	return false
}

func loginProviderCatalog() []string {
	providers := make([]string, 0, len(providerCatalog)+len(serviceCatalog))
	providers = append(providers, providerCatalog...)
	for _, service := range serviceCatalog {
		providers = append(providers, service.name)
	}
	return providers
}

func storeProviderCredential(store *state.Store, provider, key string) (string, error) {
	provider = strings.ToLower(strings.TrimSpace(provider))
	if !isLoginProvider(provider) {
		return "", fmt.Errorf("unsupported login provider %q", provider)
	}
	if providerEnvToken(provider) != "" {
		return "", fmt.Errorf("%s is supplied by an environment override; unset it to log in", provider)
	}
	if store == nil {
		return "", errors.New("auth store is unavailable")
	}
	key = strings.TrimSpace(key)
	if key == "" {
		return "", errors.New("API key is required")
	}
	if err := store.SetAPIKey(provider, key); err != nil {
		return "", err
	}
	return key, nil
}

func deleteProviderCredential(store *state.Store, provider string) error {
	provider = strings.ToLower(strings.TrimSpace(provider))
	if !isLoginProvider(provider) {
		return fmt.Errorf("unsupported logout provider %q", provider)
	}
	if providerEnvToken(provider) != "" {
		return fmt.Errorf("%s is supplied by an environment override; unset it to log out", provider)
	}
	if store == nil {
		return errors.New("auth store is unavailable")
	}
	return store.DeleteAPIKey(provider)
}
