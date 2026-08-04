package main

import (
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/levmv/golems/cy/internal/state"
)

func TestResolveOpenRouterModelContext(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/model/moonshotai/kimi-k3" {
			t.Errorf("metadata path = %q", r.URL.Path)
		}
		fmt.Fprint(w, `{"data":{"id":"moonshotai/kimi-k3","context_length":1048576}}`)
	}))
	defer server.Close()

	store, err := state.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	lookups := 0
	lookup := func(modelID string) (int, error) {
		lookups++
		return fetchOpenRouterContextWindow(server.Client(), server.URL, modelID)
	}
	spec := resolveModelSpecWithLookup("openrouter/moonshotai/kimi-k3", store, false, lookup)
	if spec.ContextWindow != 1_048_576 || spec.Estimated {
		t.Fatalf("model spec = %#v", spec)
	}
	cached := resolveModelSpecWithLookup("openrouter/moonshotai/kimi-k3", store, false, func(string) (int, error) {
		t.Fatal("cached model context triggered a lookup")
		return 0, nil
	})
	if cached.ContextWindow != 1_048_576 || cached.Estimated || lookups != 1 {
		t.Fatalf("cached model spec = %#v; lookups=%d", cached, lookups)
	}

	fallback := resolveModelSpecWithLookup("openrouter/moonshotai/future", nil, false, func(string) (int, error) {
		return 0, errors.New("metadata unavailable")
	})
	if fallback.ContextWindow != unknownModelContextWindow || !fallback.Estimated {
		t.Fatalf("fallback model spec = %#v", fallback)
	}

	freeLookups := 0
	for _, want := range []int{200_000, 256_000} {
		free := resolveModelSpecWithLookup("openrouter/free", store, false, func(modelID string) (int, error) {
			freeLookups++
			if modelID != "openrouter/free" {
				t.Fatalf("free router model ID = %q", modelID)
			}
			return want, nil
		})
		if free.ContextWindow != want || free.Estimated {
			t.Fatalf("free router model spec = %#v", free)
		}
	}
	if freeLookups != 2 {
		t.Fatalf("free router lookups = %d", freeLookups)
	}

	if err := store.SetModelContext("openrouter/~moonshotai/kimi-latest", 128_000); err != nil {
		t.Fatal(err)
	}
	latest := resolveModelSpecWithLookup("openrouter/~moonshotai/kimi-latest", store, false, func(modelID string) (int, error) {
		if modelID != "~moonshotai/kimi-latest" {
			t.Fatalf("latest model ID = %q", modelID)
		}
		return 1_048_576, nil
	})
	if latest.ContextWindow != 1_048_576 || latest.Estimated {
		t.Fatalf("latest model spec = %#v", latest)
	}
}

func TestKnownModelsIncludesRecentSelections(t *testing.T) {
	store, err := state.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := store.SetDefaultModelSelection("openrouter/moonshotai/kimi-k3", ""); err != nil {
		t.Fatal(err)
	}
	if err := store.SetDefaultModelSelection("deepseek/deepseek-v4-flash", ""); err != nil {
		t.Fatal(err)
	}

	models := knownModels(store)
	if len(models) < 2 || models[0] != "deepseek/deepseek-v4-flash" || models[1] != "openrouter/moonshotai/kimi-k3" {
		t.Fatalf("known models = %#v", models)
	}
	count := 0
	for _, model := range models {
		if model == "deepseek/deepseek-v4-flash" {
			count++
		}
	}
	if count != 1 {
		t.Fatalf("default model appears %d times in %#v", count, models)
	}
}

func TestResolveModelSpecForAppliesConfiguredContextWindow(t *testing.T) {
	uri := "openai/local-model"
	unknown := resolveModelSpecFor(Config{}, uri, nil, false)
	if unknown.ContextWindow != unknownModelContextWindow || !unknown.Estimated {
		t.Fatalf("model spec without an override = %#v", unknown)
	}

	spec := resolveModelSpecFor(Config{ContextWindow: 262144}, uri, nil, false)
	if spec.ContextWindow != 262144 || spec.Estimated {
		t.Fatalf("model spec with an override = %#v", spec)
	}

	known := resolveModelSpecFor(Config{ContextWindow: 262144}, "deepseek/deepseek-v4-flash", nil, false)
	if known.ContextWindow != 262144 {
		t.Fatalf("override did not replace a catalog entry: %#v", known)
	}
}
