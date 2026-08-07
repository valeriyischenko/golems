package main

import (
	"cmp"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	toolruntime "github.com/levmv/golems/cy/internal/tools"
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
	RetryBudget       time.Duration
	StreamIdleTimeout time.Duration
	SystemPrompt      string
	// CompactionPrompt and ToolLimitPrompt are the two prompts Cy sends on its
	// own account rather than the user's: the one a compaction summary is
	// produced under, and the one that asks for a final answer when the tool
	// iteration fuse blows. Empty keeps the built-in wording.
	CompactionPrompt  string
	ToolLimitPrompt   string
	RootDir           string
	Home              string
	Verbose           bool
	JSON              bool
	CapabilityProfile string
	SandboxPolicy     string
	TerminalTheme     string
	// ToolsFile and ExternalTools are the declared tool catalog: where it was
	// read from, and what it said. Read once at startup rather than per call,
	// so every turn of a session offers the same tools and the session can
	// record which ones they were.
	ToolsFile     string
	ExternalTools []toolruntime.ExternalTool
	Security      SecurityState
	PrintMode     bool
	SaveSession   bool
	Ephemeral     bool
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

// loadPromptFile reads a prompt whose text is configuration rather than code.
// An unset path is not an error; it means the built-in wording stands.
//
// Read at startup rather than at the moment the prompt is used, which is the
// whole reason this exists as a step of its own. A file consulted mid-run is a
// prompt that can change under a session and leave nothing in the record to say
// it did, and a path that is wrong should cost a configuration exit before the
// first model call rather than a failure forty turns in -- the same bargain the
// tool catalog is loaded under.
//
// An empty file is an error, not a way to ask for the built-in. Naming a file
// and silently getting the default back is the failure this is meant to remove;
// not naming one is how to keep it.
func loadPromptFile(path, setting string) (string, error) {
	path = strings.TrimSpace(path)
	if path == "" {
		return "", nil
	}
	content, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("read %s: %w", setting, err)
	}
	prompt := strings.TrimSpace(string(content))
	if prompt == "" {
		return "", fmt.Errorf("%s file %s is empty", setting, path)
	}
	return prompt, nil
}

// externalToolsPath is where tool declarations are read from: CY_TOOLS when it
// is set, so a run can be given a catalog of its own without moving Cy's home,
// and otherwise a file beside the rest of Cy's state.
func externalToolsPath(home string) string {
	if path := strings.TrimSpace(os.Getenv("CY_TOOLS")); path != "" {
		return path
	}
	return filepath.Join(resolveStateHome(home), "tools.json")
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

// normalizePositiveDuration reads one of the durations that bound a model
// request. Zero means the caller did not say and the default stands.
//
// Neither of them accepts "unlimited", unlike the tool fuse. Cy asks for
// unlimited retries and lets time be the limit, so a budget of zero is not a
// generous setting but an invalid one -- golem refuses the combination outright.
// A caller who wants the bound out of the way raises it rather than removes it.
// The idle timeout is the tighter of the two on an attempt that has connected
// and gone quiet, which is the failure an unattended run most needs to survive;
// the budget is the outer bound, and it is the only one on a request that is not
// streamed.
func normalizePositiveDuration(value, setting string) (time.Duration, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return 0, nil
	}
	duration, err := time.ParseDuration(value)
	if err != nil || duration <= 0 {
		return 0, fmt.Errorf("invalid %s %q; expected a positive duration such as 90s or 5m", setting, value)
	}
	return duration, nil
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
