package main

import (
	"strings"

	"github.com/levmv/golems/cy/internal/state"
)

const unknownModelContextWindow = 128 * 1024

type modelSpec struct {
	URI           string
	ContextWindow int
	Estimated     bool
}

type modelContextLookup func(modelID string) (int, error)

var modelCatalog = []modelSpec{
	{URI: "deepseek/deepseek-v4-flash", ContextWindow: 1_000_000},
	{URI: "deepseek/deepseek-v4-pro", ContextWindow: 1_000_000},
	{URI: "openrouter/free"},
	{URI: "openrouter/~x-ai/grok-latest"},
	{URI: "openrouter/~moonshotai/kimi-latest"},
	{URI: "openrouter/~google/gemini-pro-latest"},
}

func knownModels(store *state.Store) []string {
	models := make([]string, 0, len(modelCatalog)+4)
	seen := make(map[string]struct{}, cap(models))
	add := func(uri string) {
		uri = strings.TrimSpace(uri)
		key := strings.ToLower(uri)
		if key == "" {
			return
		}
		if _, ok := seen[key]; ok {
			return
		}
		seen[key] = struct{}{}
		models = append(models, uri)
	}
	if store != nil {
		if config, err := store.Config(); err == nil {
			add(config.Model)
			for _, uri := range config.RecentModels {
				add(uri)
			}
		}
	}
	for _, spec := range modelCatalog {
		add(spec.URI)
	}
	return models
}

func resolveModelSpec(uri string, store *state.Store, refresh bool) modelSpec {
	return resolveModelSpecWithLookup(uri, store, refresh, openRouterContextWindow)
}

// resolveModelSpecFor resolves a model and then applies an explicit context
// window from configuration. Without one, a model the catalog does not list
// falls back to a guess that drives compaction timing, and the guess cannot be
// corrected: a self-hosted deployment has no catalog entry to look up.
func resolveModelSpecFor(cfg Config, uri string, store *state.Store, refresh bool) modelSpec {
	spec := resolveModelSpec(uri, store, refresh)
	if cfg.ContextWindow > 0 {
		spec.ContextWindow = cfg.ContextWindow
		spec.Estimated = false
	}
	return spec
}

func resolveModelSpecWithLookup(uri string, store *state.Store, refresh bool, lookup modelContextLookup) modelSpec {
	model := strings.ToLower(strings.TrimSpace(uri))
	if modelID, ok := strings.CutPrefix(model, "openrouter/"); ok {
		modelID = canonicalOpenRouterModelID(modelID)
		cached, cachedOK := cachedModelContext(store, model)
		if cachedOK && !refresh && !volatileOpenRouterModel(modelID) {
			return modelSpec{URI: strings.TrimSpace(uri), ContextWindow: cached}
		}
		if window, err := lookup(modelID); err == nil && window > 0 {
			if store != nil {
				_ = store.SetModelContext(model, window)
			}
			return modelSpec{URI: strings.TrimSpace(uri), ContextWindow: window}
		}
		if cachedOK {
			return modelSpec{URI: strings.TrimSpace(uri), ContextWindow: cached}
		}
	}
	for _, spec := range modelCatalog {
		if model == spec.URI {
			if spec.ContextWindow > 0 {
				spec.URI = strings.TrimSpace(uri)
				return spec
			}
			break
		}
	}
	return modelSpec{
		URI:           strings.TrimSpace(uri),
		ContextWindow: unknownModelContextWindow,
		Estimated:     true,
	}
}

func canonicalOpenRouterModelID(modelID string) string {
	modelID = strings.TrimSpace(modelID)
	if strings.EqualFold(modelID, "free") {
		return "openrouter/free"
	}
	return modelID
}

func volatileOpenRouterModel(modelID string) bool {
	modelID = strings.ToLower(strings.TrimSpace(modelID))
	return strings.HasPrefix(modelID, "~") || modelID == "openrouter/free"
}

func cachedModelContext(store *state.Store, uri string) (int, bool) {
	if store == nil {
		return 0, false
	}
	window, ok, err := store.ModelContext(uri)
	return window, ok && err == nil
}
