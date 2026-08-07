package main

import (
	"cmp"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
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
	// Background says whether the model may start background jobs. Nil is the
	// caller saying nothing, which is not the same as saying no; see
	// BackgroundJobs for what decides it then.
	Background *bool
	// JobLauncher is the resolved path of the program that supervises a job.
	// Empty is the built-in supervisor.
	JobLauncher string
	// ToolsFile and ExternalTools are the declared tool catalog: where it was
	// read from, and what it said. Read once at startup rather than per call,
	// so every turn of a session offers the same tools and the session can
	// record which ones they were.
	ToolsFile     string
	ExternalTools []toolruntime.ExternalTool
	// SandboxGrants is the run-level part of the same file: what every tool
	// process may reach beyond the built-in ruleset, Bash included.
	SandboxGrants toolruntime.SandboxGrants
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

// BackgroundJobs says whether the model may start work that outlives the tool
// call, and inspect it afterwards.
//
// The default is the invocation, which is a proxy for the property rather than
// the property itself. An interactive session has somebody watching and stays
// open; a one-shot exits when its turn ends and kills whatever it started, so a
// model that backgrounds a build and answers straight away would be reporting
// work that was silently killed. What the proxy misses is a long unattended
// run, which is launched exactly like a one-shot and is neither short-lived nor
// watched -- completions reach it before every model request within a turn, not
// only between turns. So the proxy stays as the default and stops being the
// rule.
func (c Config) BackgroundJobs() bool {
	if c.Background != nil {
		return *c.Background
	}
	return !c.PrintMode
}

// hiddenPaths lists everything the tools file asks to be kept away from tool
// processes, run-level and per tool together. Nothing distinguishes the two
// here: either is a promise that only a working fence can keep.
func (c Config) hiddenPaths() []string {
	hidden := slices.Clone(c.SandboxGrants.Hide)
	for _, tool := range c.ExternalTools {
		hidden = append(hidden, tool.Sandbox.Hide...)
	}
	slices.Sort(hidden)
	return slices.Compact(hidden)
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

// normalizeBackground reads whether the model may start background jobs. An
// empty value means the caller did not say and the invocation decides.
//
// Three states rather than two because the default is not a constant, so an
// explicit "false" has to be distinguishable from silence -- otherwise turning
// background off in an interactive session and saying nothing in a one-shot
// would be the same request. Spelled as a value rather than as a bare switch
// for the same reason: a flag that only turns things on has no way to say off.
func normalizeBackground(value string) (*bool, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return nil, nil
	}
	allowed, err := strconv.ParseBool(value)
	if err != nil {
		return nil, fmt.Errorf("invalid background setting %q; expected true or false", value)
	}
	return &allowed, nil
}

// normalizeJobLauncher resolves the program that will supervise this session's
// jobs. Empty asks for the built-in supervisor, which is Cy re-exec'd.
//
// Resolved at startup and not at the first job: a launcher that is not there is
// a mistake in configuration, and it should end the run before the model is
// told it can run commands rather than at turn 40.
func normalizeJobLauncher(value string) (string, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return "", nil
	}
	resolved, err := exec.LookPath(value)
	if err != nil {
		return "", fmt.Errorf("invalid job launcher %q: %w", value, err)
	}
	return resolved, nil
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
