package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"github.com/levmv/golems/cy/internal/session"
	"github.com/levmv/golems/cy/internal/state"
	toolruntime "github.com/levmv/golems/cy/internal/tools"
	"github.com/levmv/golems/cy/internal/ui"
	"github.com/levmv/golems/pkg/golem"
	"github.com/levmv/golems/pkg/llm"
)

const maxPipedInputBytes = 8 * 1024 * 1024

func main() {
	if toolruntime.RunSandboxChildIfRequested() {
		return
	}
	if err := runMain(); err != nil {
		fmt.Fprintf(os.Stderr, "cy: %v\n", err)
		os.Exit(exitCodeFor(err))
	}
}

func runMain() (returnErr error) {
	toolruntime.HardenSupervisor()
	cfg := LoadConfig()

	modelURI := flag.String("model", cfg.ModelURI, "model URI in provider/model format")
	baseURL := flag.String("base-url", cfg.BaseURL, "override the provider endpoint, for a self-hosted or proxied deployment")
	contextWindow := flag.String("context-window", os.Getenv("CY_CONTEXT_WINDOW"), "context window in tokens, for a model Cy has no entry for")
	maxToolIterations := flag.String("max-tool-iterations", os.Getenv("CY_MAX_TOOL_ITERATIONS"), "per-turn cap on model-to-tool cycles, or unlimited")
	retryBudget := flag.String("retry-budget", os.Getenv("CY_RETRY_BUDGET"), "how long one model request may take in total, attempts included, e.g. 90s")
	streamIdleTimeout := flag.String("stream-idle-timeout", os.Getenv("CY_STREAM_IDLE_TIMEOUT"), "how long a model stream may produce nothing before the attempt is abandoned")
	systemPrompt := flag.String("system-prompt", cfg.SystemPrompt, "replace the built-in system prompt")
	rootDir := flag.String("root", cfg.RootDir, "workspace root for file and search tools")
	home := flag.String("home", cfg.Home, "Cy home directory (defaults to CY_HOME or ~/.cy)")
	verbose := flag.Bool("v", false, "show progress and usage in one-shot mode")
	saveSession := flag.Bool("save-session", cfg.SaveSession, "keep a resumable session for a one-shot invocation")
	jsonOutput := flag.Bool("json", cfg.JSON, "emit one versioned JSON result on stdout")
	profile := flag.String("profile", cfg.CapabilityProfile, "capability profile: full, edit, or read-only")
	sandbox := flag.String("sandbox", cfg.SandboxPolicy, "model command isolation: auto, off, or on")
	theme := flag.String("theme", cfg.TerminalTheme, "terminal theme: auto, light, or dark")
	showVersion := flag.Bool("version", false, "print the Cy version and exit")
	flag.Usage = func() { writeCLIUsage(flag.CommandLine) }
	// flag.Parse removes --, so remember whether it was a delimiter before the
	// positional arguments. That distinction makes `cy -- resume ...` a prompt
	// without mistaking a flag value equal to "--" for explicit prompt mode.
	explicitPrompt := hasFlagTerminator(flag.CommandLine, os.Args[1:])
	flag.Parse()
	setFlags := make(map[string]bool)
	flag.Visit(func(value *flag.Flag) { setFlags[value.Name] = true })
	if *showVersion {
		fmt.Fprintf(os.Stdout, "cy %s\n", version)
		return nil
	}
	cfg.ModelURI = strings.TrimSpace(*modelURI)
	cfg.BaseURL = strings.TrimSpace(*baseURL)
	cfg.SystemPrompt = *systemPrompt
	cfg.RootDir = *rootDir
	cfg.Home = *home
	cfg.Verbose = *verbose
	cfg.SaveSession = *saveSession
	cfg.JSON = *jsonOutput
	window, err := normalizeContextWindow(*contextWindow)
	if err != nil {
		return asConfigError(err)
	}
	cfg.ContextWindow = window
	cfg.MaxToolIterations, err = normalizeMaxToolIterations(*maxToolIterations)
	if err != nil {
		return asConfigError(err)
	}
	cfg.RetryBudget, err = normalizePositiveDuration(*retryBudget, "retry budget")
	if err != nil {
		return asConfigError(err)
	}
	cfg.StreamIdleTimeout, err = normalizePositiveDuration(*streamIdleTimeout, "stream idle timeout")
	if err != nil {
		return asConfigError(err)
	}
	normalizedProfile, err := toolruntime.NormalizeCapabilityProfile(*profile)
	if err != nil {
		return asConfigError(err)
	}
	cfg.CapabilityProfile = normalizedProfile
	cfg.SandboxPolicy, err = normalizeSandboxPolicy(*sandbox)
	if err != nil {
		return asConfigError(err)
	}
	cfg.TerminalTheme, err = normalizeTerminalTheme(*theme)
	if err != nil {
		return asConfigError(err)
	}
	// Before anything else reads the catalog, so a malformed declaration stops
	// the run here rather than after a model call has already been paid for.
	cfg.ToolsFile = externalToolsPath(cfg.Home)
	cfg.ExternalTools, err = toolruntime.LoadExternalTools(cfg.ToolsFile)
	if err != nil {
		return asConfigError(err)
	}
	store, err := state.Open(cfg.Home)
	if err != nil {
		return fmt.Errorf("initialize state: %w", err)
	}
	storedSettings, err := store.Config()
	if err != nil {
		return fmt.Errorf("load settings: %w", err)
	}
	if !setFlags["model"] && strings.TrimSpace(os.Getenv("CY_MODEL")) == "" && storedSettings.Model != "" {
		cfg.ModelURI = storedSettings.Model
		cfg.ReasoningEffort = storedSettings.ReasoningEffort
	}
	if !setFlags["profile"] && strings.TrimSpace(os.Getenv("CY_PROFILE")) == "" && storedSettings.Profile != "" {
		cfg.CapabilityProfile, err = toolruntime.NormalizeCapabilityProfile(storedSettings.Profile)
		if err != nil {
			return asConfigError(fmt.Errorf("load saved profile: %w", err))
		}
	}
	if !setFlags["sandbox"] && strings.TrimSpace(os.Getenv("CY_SANDBOX")) == "" && storedSettings.Sandbox != "" {
		cfg.SandboxPolicy, err = normalizeSandboxPolicy(storedSettings.Sandbox)
		if err != nil {
			return asConfigError(fmt.Errorf("load saved sandbox policy: %w", err))
		}
	}

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	invocation := parseInvocation(flag.Args(), explicitPrompt)
	resumeID, args := invocation.SessionID, invocation.Args
	pipedInput := ""
	hasPipe := false
	if len(args) == 0 {
		pipedInput, hasPipe, err = readPipedInput(os.Stdin)
		if err != nil {
			return fmt.Errorf("read stdin: %w", err)
		}
	}
	cfg.PrintMode = len(args) > 0 || hasPipe

	var tools []golem.Tool
	var root string
	if invocation.Resume && resumeID == "" {
		// Bare resume is workspace-scoped. Build the tools now to obtain the same
		// canonical, symlink-free root that is recorded in session journals, then
		// reuse them after opening the selected session.
		tools, root, err = toolruntime.NewWorkspaceTools(cfg.RootDir)
		if err != nil {
			return fmt.Errorf("initialize workspace tools: %w", err)
		}
		resumeID, err = latestSessionID(cfg.Home, root)
		if err != nil {
			return err
		}
	}
	cfg.Ephemeral = useEphemeralSession(cfg, resumeID)

	var journal *session.Session
	var resumed session.State
	journalHandedOff := false
	if resumeID != "" {
		journal, err = session.Open(cfg.Home, resumeID)
		if err != nil {
			return fmt.Errorf("resume session: %w", err)
		}
		defer func(opened *session.Session) {
			if !journalHandedOff {
				returnErr = errors.Join(returnErr, opened.ClosePruningEmpty())
			}
		}(journal)
		resumed, err = journal.Replay()
		if err != nil {
			return fmt.Errorf("replay session: %w", err)
		}
		applyResumedConfig(&cfg, resumed, setFlags)
	}

	if tools == nil {
		tools, root, err = toolruntime.NewWorkspaceTools(cfg.RootDir)
		if err != nil {
			return fmt.Errorf("initialize workspace tools: %w", err)
		}
	}
	cfg.Security = buildSecurityState(ctx, cfg, root, store)
	if err := requireEffectiveSandbox(cfg); err != nil {
		return asConfigError(err)
	}
	if notice := sandboxUnavailableNotice(cfg); notice != "" {
		fmt.Fprintf(os.Stderr, "cy: %s\n", notice)
	}
	// The interactive UI must be able to start without a credential so the
	// user can authenticate with /login. sessionAgent checks the credential at
	// the model-call boundary, before anything is appended to the journal.
	model, err := buildModel(cfg, store, false)
	if err != nil {
		return fmt.Errorf("initialize model: %w", err)
	}
	if journal != nil && resumed.Header.Workspace != "" && root != resumed.Header.Workspace {
		return asConfigError(fmt.Errorf("session workspace is %s, not %s", resumed.Header.Workspace, root))
	}
	if journal == nil {
		sessionHome := cfg.Home
		if cfg.Ephemeral {
			temporaryHome, tempErr := os.MkdirTemp("", "cy-run-")
			if tempErr != nil {
				return fmt.Errorf("create temporary run state: %w", tempErr)
			}
			defer func() { returnErr = errors.Join(returnErr, os.RemoveAll(temporaryHome)) }()
			sessionHome = temporaryHome
		}
		journal, err = session.Create(session.CreateOptions{
			Home:            sessionHome,
			Workspace:       root,
			Model:           cfg.ModelURI,
			ReasoningEffort: cfg.ReasoningEffort,
		})
		if err != nil {
			return fmt.Errorf("create session: %w", err)
		}
		defer func(created *session.Session) {
			if !journalHandedOff {
				returnErr = errors.Join(returnErr, created.ClosePruningEmpty())
			}
		}(journal)
	} else if resumed.Model != "" && (cfg.ModelURI != resumed.Model || cfg.ReasoningEffort != resumed.ReasoningEffort) {
		if _, err := journal.Append(session.RecordModelChanged, session.ModelChanged{Model: cfg.ModelURI, ReasoningEffort: cfg.ReasoningEffort}); err != nil {
			return fmt.Errorf("record model change: %w", err)
		}
	}
	agent, err := newSessionAgent(cfg, model, root, tools, journal, store)
	if err != nil {
		return err
	}
	journalHandedOff = true
	defer func() { returnErr = errors.Join(returnErr, agent.Close()) }()
	defer func() {
		if shouldPrintResumeHint(cfg, agent.HasUserTurn()) {
			fmt.Fprintf(os.Stderr, "Resume with: cy resume %s\n", agent.SessionID())
		}
	}()

	if len(args) > 0 {
		if err := runPrintTurn(ctx, agent, cfg, strings.Join(args, " "), os.Stdout, os.Stderr); err != nil {
			return err
		}
		return nil
	}

	if hasPipe {
		if pipedInput == "" {
			return nil
		}
		if err := runPrintTurn(ctx, agent, cfg, pipedInput, os.Stdout, os.Stderr); err != nil {
			return err
		}
		return nil
	}

	inFile, outFile, ok := ui.CanUseScreen(os.Stdin, os.Stdout)
	if !ok {
		return asConfigError(errors.New("interactive mode requires a terminal; use ssh -t, allocate a TTY, or pass a prompt"))
	}
	return ui.RunScreen(ctx, agent, ui.Config{
		ModelURI:          cfg.ModelURI,
		ReasoningEffort:   cfg.ReasoningEffort,
		CapabilityProfile: cfg.CapabilityProfile,
		Providers:         loginProviderCatalog(),
		TerminalTheme:     cfg.TerminalTheme,
	}, root, inFile, outFile)
}

func applyResumedConfig(cfg *Config, resumed session.State, setFlags map[string]bool) {
	if !setFlags["model"] && strings.TrimSpace(os.Getenv("CY_MODEL")) == "" && resumed.Model != "" {
		cfg.ModelURI = resumed.Model
		cfg.ReasoningEffort = resumed.ReasoningEffort
	}
	if !setFlags["root"] && strings.TrimSpace(os.Getenv("CY_ROOT")) == "" && resumed.Header.Workspace != "" {
		cfg.RootDir = resumed.Header.Workspace
	}
}

func buildModel(cfg Config, store *state.Store, requireCredential bool) (llm.Model, error) {
	uri := strings.TrimSpace(cfg.ModelURI)
	provider, modelID, ok := strings.Cut(uri, "/")
	if !ok || provider == "" || strings.TrimSpace(modelID) == "" ||
		provider != strings.TrimSpace(provider) || modelID != strings.TrimSpace(modelID) {
		return llm.Model{}, fmt.Errorf("invalid model URI %q; expected provider/model", cfg.ModelURI)
	}

	baseURL := strings.TrimSpace(cfg.BaseURL)
	var opts []llm.ProviderOption
	if baseURL != "" {
		opts = append(opts, llm.WithBaseURL(baseURL))
	}

	registry := llm.NewRegistry()
	switch provider {
	case "deepseek", "openai", "openrouter":
		token, _, err := credentialForProvider(store, provider)
		if err != nil {
			return llm.Model{}, err
		}
		// A replaced endpoint is not the provider's, so it need not hold the
		// provider's credential: a local or proxied deployment may accept any
		// token or none. Whatever is configured is still sent, since some
		// proxies do check it.
		if requireCredential && token == "" && baseURL == "" {
			return llm.Model{}, missingProviderCredentialError(provider, cfg.ModelURI)
		}
		if provider == "openrouter" {
			opts = append(opts, llm.WithAppAttribution("Cy", "https://github.com/levmv/golems"))
		}
		registry.WithProvider(provider, token, opts...)
	case "ollama":
		registry.WithProvider(provider, "ollama", opts...)
	default:
		return llm.Model{}, fmt.Errorf("unsupported provider %q", provider)
	}

	registryURI := uri
	if provider == "openrouter" {
		registryURI = provider + "/" + canonicalOpenRouterModelID(modelID)
	}
	model, err := registry.Model(registryURI)
	if err != nil {
		return llm.Model{}, err
	}
	effort, err := normalizeReasoningEffort(uri, cfg.ReasoningEffort)
	if err != nil {
		return llm.Model{}, err
	}
	if effort != defaultReasoningEffort {
		model = model.WithReasoningEffort(effort)
	}

	return model, nil
}

// runPrintTurn keeps stdout suitable for shell pipelines: it contains either
// the final assistant message or one final JSON result. Progress belongs on
// stderr.
func runPrintTurn(ctx context.Context, agent agentRunner, cfg Config, input string, out, errOut io.Writer) error {
	diagnostics := cfg.Verbose
	diagnosticConsole := ui.NewConsole(errOut)

	turn, err := agent.Stream(ctx, input, func(event golem.StreamEvent) {
		if diagnostics {
			printOneShotDiagnostic(diagnosticConsole, event)
		}
	})
	diagnosticConsole.FlushCompactToolEvents()
	if err != nil {
		return err
	}
	if diagnostics {
		diagnosticConsole.PrintChangeSummary()
	}
	if cfg.JSON {
		return writeJSONResult(out, agent, cfg, turn)
	}
	console := ui.NewConsole(out)
	console.PrintMarkdown(turn.Reply)
	fmt.Fprintln(out)
	if cfg.Verbose {
		fmt.Fprintf(errOut, "[usage: %s]\n", golem.FormatUsage(turn.Usage))
	}
	return nil
}

func printOneShotDiagnostic(console *ui.Console, event golem.StreamEvent) {
	switch event.Kind {
	case golem.EventToolCall, golem.EventToolResult, golem.EventToolError:
		console.PrintCompactToolEvent(event)
	case golem.EventModelRetry:
		console.FlushCompactToolEvents()
		console.PrintRetry(event.Text)
	case golem.EventStatus:
		console.PrintStatus(event.Text)
	case golem.EventAttemptReset:
		if event.Text == "" {
			break
		}
		console.PrintDiscardedAttempt()
	}
}

func shouldPrintResumeHint(cfg Config, hasUserTurn bool) bool {
	return hasUserTurn && (!cfg.PrintMode || cfg.SaveSession)
}

func useEphemeralSession(cfg Config, resumeID string) bool {
	return cfg.PrintMode && !cfg.SaveSession && strings.TrimSpace(resumeID) == ""
}

func readPipedInput(stdin *os.File) (string, bool, error) {
	info, err := stdin.Stat()
	if err != nil {
		return "", false, err
	}
	if info.Mode()&os.ModeCharDevice != 0 {
		return "", false, nil
	}

	data, err := io.ReadAll(io.LimitReader(stdin, maxPipedInputBytes+1))
	if err != nil {
		return "", true, err
	}
	if len(data) > maxPipedInputBytes {
		return "", true, fmt.Errorf("piped input exceeds %d bytes", maxPipedInputBytes)
	}
	return strings.TrimSpace(string(data)), true, nil
}

func modelProvider(uri string) string {
	provider, modelID, ok := strings.Cut(strings.TrimSpace(uri), "/")
	if !ok || strings.TrimSpace(provider) == "" || strings.TrimSpace(modelID) == "" {
		return ""
	}
	return strings.ToLower(strings.TrimSpace(provider))
}
