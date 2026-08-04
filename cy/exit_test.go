package main

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/levmv/golems/pkg/llm"
)

func TestExitCodeForClassifiesFailures(t *testing.T) {
	for _, tt := range []struct {
		name string
		err  error
		want int
	}{
		{"success", nil, exitOK},
		{"unclassified", errors.New("something went wrong"), exitFailure},
		{"config", asConfigError(errors.New("invalid profile")), exitConfig},
		{"config wrapped further", fmt.Errorf("initialize model: %w", asConfigError(errors.New("no key"))), exitConfig},
		{"invalid request", fmt.Errorf("build: %w", llm.ErrInvalidRequest), exitConfig},
		{"provider", &llm.Error{StatusCode: 503, Provider: "openai", Message: "unavailable"}, exitProvider},
		{"provider wrapped", fmt.Errorf("turn failed: %w", &llm.Error{StatusCode: 500, Provider: "openai"}), exitProvider},
		{"interrupted", fmt.Errorf("run: %w", context.Canceled), exitInterrupted},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if got := exitCodeFor(tt.err); got != tt.want {
				t.Fatalf("exitCodeFor(%v) = %d, want %d", tt.err, got, tt.want)
			}
		})
	}
}

// An interrupt arrives while the run is doing something else, so it can reach
// the top wrapped in whatever failed at the time. It stays an interrupt.
func TestExitCodeForPrefersInterruptOverTheFailureItInterrupted(t *testing.T) {
	err := fmt.Errorf("provider call: %w", errors.Join(&llm.Error{StatusCode: 500}, context.Canceled))
	if got := exitCodeFor(err); got != exitInterrupted {
		t.Fatalf("exitCodeFor() = %d, want %d", got, exitInterrupted)
	}
}

// The class must not change what the user reads, only what a script sees.
func TestConfigErrorKeepsItsMessage(t *testing.T) {
	inner := errors.New("invalid sandbox policy \"maybe\"")
	wrapped := asConfigError(inner)
	if wrapped.Error() != inner.Error() {
		t.Fatalf("message = %q, want %q", wrapped.Error(), inner.Error())
	}
	if !errors.Is(wrapped, inner) {
		t.Fatalf("errors.Is() = false, want the cause to remain reachable")
	}
}

func TestMissingProviderCredentialIsAConfigFailure(t *testing.T) {
	err := missingProviderCredentialError("openai", "openai/gpt-5")
	if got := exitCodeFor(err); got != exitConfig {
		t.Fatalf("exitCodeFor() = %d, want %d", got, exitConfig)
	}
}
