package main

import (
	"cmp"
	"fmt"
	"os"
	"strconv"
	"strings"
)

const (
	defaultModelURI          = "deepseek/deepseek-v4-flash"
	defaultRootDir           = "."
	defaultCapabilityProfile = "full"
	defaultSandboxPolicy     = "auto"
	defaultTerminalTheme     = "auto"
)

type Config struct {
	ModelURI          string
	ReasoningEffort   string
	BaseURL           string
	ContextWindow     int
	MaxToolIterations int
	SystemPrompt      string
	RootDir           string
	Home              string
	Verbose           bool
	JSON              bool
	CapabilityProfile string
	SandboxPolicy     string
	TerminalTheme     string
	Security          SecurityState
	PrintMode         bool
	SaveSession       bool
	Ephemeral         bool
}

func LoadConfig() Config {
	return Config{
		ModelURI:          cmp.Or(os.Getenv("CY_MODEL"), defaultModelURI),
		BaseURL:           strings.TrimSpace(os.Getenv("CY_BASE_URL")),
		SystemPrompt:      os.Getenv("CY_SYSTEM_PROMPT"),
		RootDir:           cmp.Or(os.Getenv("CY_ROOT"), defaultRootDir),
		Home:              strings.TrimSpace(os.Getenv("CY_HOME")),
		CapabilityProfile: cmp.Or(strings.TrimSpace(os.Getenv("CY_PROFILE")), defaultCapabilityProfile),
		SandboxPolicy:     cmp.Or(strings.TrimSpace(os.Getenv("CY_SANDBOX")), defaultSandboxPolicy),
		TerminalTheme:     cmp.Or(strings.TrimSpace(os.Getenv("CY_THEME")), defaultTerminalTheme),
	}
}

// normalizeContextWindow reads an explicit context window in tokens. It is
// parsed here rather than in LoadConfig so that a malformed value is reported
// instead of silently becoming zero, matching the other normalized settings.
func normalizeContextWindow(value string) (int, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return 0, nil
	}
	window, err := strconv.Atoi(value)
	if err != nil || window <= 0 {
		return 0, fmt.Errorf("invalid context window %q; expected a positive number of tokens", value)
	}
	return window, nil
}

// normalizeMaxToolIterations reads the per-turn tool iteration fuse. Zero means
// the caller did not say and the engine default stands.
//
// "unlimited" is spelled out because a bare -1 is not something anyone types by
// accident and not something anyone reads back with confidence either. Removing
// the fuse is a real choice for a long unattended run and a way to burn a budget
// with nobody watching, so it costs a word; every other value must be positive,
// which keeps a mistyped "-1" an error rather than a silent no-limit.
func normalizeMaxToolIterations(value string) (int, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return 0, nil
	}
	if strings.EqualFold(value, "unlimited") {
		return -1, nil
	}
	iterations, err := strconv.Atoi(value)
	if err != nil || iterations <= 0 {
		return 0, fmt.Errorf("invalid tool iteration limit %q; expected a positive number of model-to-tool cycles, or unlimited", value)
	}
	return iterations, nil
}

func normalizeTerminalTheme(value string) (string, error) {
	switch value = strings.ToLower(strings.TrimSpace(value)); value {
	case "", "auto":
		return defaultTerminalTheme, nil
	case "light", "dark":
		return value, nil
	default:
		return "", fmt.Errorf("invalid terminal theme %q; expected auto, light, or dark", value)
	}
}
