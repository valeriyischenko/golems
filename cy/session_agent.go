package main

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/levmv/golems/cy/internal/engine"
	"github.com/levmv/golems/cy/internal/session"
	"github.com/levmv/golems/cy/internal/state"
	toolruntime "github.com/levmv/golems/cy/internal/tools"
	"github.com/levmv/golems/pkg/golem"
	"github.com/levmv/golems/pkg/hackernews"
	"github.com/levmv/golems/pkg/llm"
	"github.com/levmv/golems/pkg/webfetch"
	"github.com/levmv/golems/pkg/websearch"
)

// sessionAgent is the stable UI-facing handle. It owns the session and process
// runtime that change beneath /clear and /resume, while each individual Engine
// remains tied to exactly one saved session.
type sessionAgent struct {
	mu sync.RWMutex

	cfg       Config
	model     golem.Model
	root      string
	baseTools []golem.Tool
	state     *state.Store
	masker    *secretMasker

	engine    *engine.Engine
	journal   *session.Session
	processes *toolruntime.ProcessManager
	context   engine.ContextReport
	usage     llm.Usage
	repaired  bool
	closed    bool
}

func newSessionAgent(cfg Config, model golem.Model, root string, baseTools []golem.Tool, journal *session.Session, store *state.Store) (*sessionAgent, error) {
	managed := &sessionAgent{cfg: cfg, model: model, root: root, baseTools: append([]golem.Tool(nil), baseTools...), state: store, masker: newSecretMasker(store)}
	eng, processes, err := managed.build(journal, cfg, model)
	if err != nil {
		return nil, err
	}
	report, usage, err := eng.Status()
	if err != nil {
		_ = processes.Close()
		return nil, fmt.Errorf("initialize session status: %w", err)
	}
	managed.engine = eng
	managed.journal = journal
	managed.processes = processes
	managed.context = report
	managed.usage = usage
	managed.repaired = journal.TailRepaired()
	return managed, nil
}

func (a *sessionAgent) refreshStatusLocked() {
	if a.engine == nil {
		return
	}
	report, usage, err := a.engine.Status()
	if err == nil {
		a.context = report
		a.usage = usage
	}
}

func (a *sessionAgent) ProviderStatuses() ([]providerStatus, error) {
	return listProviderStatus(a.state)
}

func (a *sessionAgent) Login(provider, key string) error {
	provider = strings.ToLower(strings.TrimSpace(provider))
	key, err := storeProviderCredential(a.state, provider, key)
	if err != nil {
		return err
	}
	a.masker.Add(key)
	if !isModelLoginProvider(provider) {
		return a.reloadTools()
	}
	a.mu.RLock()
	current := modelProvider(a.cfg.ModelURI) == provider
	uri := a.cfg.ModelURI
	effort := a.cfg.ReasoningEffort
	a.mu.RUnlock()
	if current {
		return a.reloadModel(uri, effort, false)
	}
	return nil
}

func (a *sessionAgent) Logout(provider string) error {
	provider = strings.ToLower(strings.TrimSpace(provider))
	if err := deleteProviderCredential(a.state, provider); err != nil {
		return err
	}
	if !isModelLoginProvider(provider) {
		return a.reloadTools()
	}
	a.mu.RLock()
	current := modelProvider(a.cfg.ModelURI) == provider
	uri := a.cfg.ModelURI
	effort := a.cfg.ReasoningEffort
	a.mu.RUnlock()
	if current {
		return a.reloadModel(uri, effort, false)
	}
	return nil
}

func (a *sessionAgent) SwitchModelWithEffort(uri, effort string) error {
	uri = strings.TrimSpace(uri)
	if uri == "" {
		return errors.New("model URI is required")
	}
	normalized, err := normalizeReasoningEffort(uri, effort)
	if err != nil {
		return err
	}
	return a.reloadModel(uri, normalized, true)
}

func (a *sessionAgent) reloadModel(uri, effort string, selected bool) error {
	a.mu.RLock()
	cfg := a.cfg
	a.mu.RUnlock()
	cfg.ModelURI = uri
	cfg.ReasoningEffort = effort
	model, err := buildModel(cfg, a.state, selected)
	if err != nil {
		return err
	}
	spec := resolveModelSpecFor(cfg, uri, a.state, selected)

	a.mu.Lock()
	defer a.mu.Unlock()
	eng := a.engine
	journal := a.journal
	if a.closed || eng == nil || journal == nil {
		return errors.New("cy session runtime is closed")
	}
	if a.state != nil && selected {
		if err := a.state.SetDefaultModelSelection(uri, effort); err != nil {
			return fmt.Errorf("remember selected model: %w", err)
		}
	}
	if a.cfg.ModelURI != uri || a.cfg.ReasoningEffort != effort {
		if _, err := journal.Append(session.RecordModelChanged, session.ModelChanged{Model: uri, ReasoningEffort: effort}); err != nil {
			return err
		}
	}
	if err := eng.ReconfigureModel(model, uri, spec.ContextWindow, spec.Estimated); err != nil {
		return err
	}
	a.cfg.ModelURI = uri
	a.cfg.ReasoningEffort = effort
	a.model = model
	a.refreshStatusLocked()
	return nil
}

func (a *sessionAgent) KnownModels() []string {
	return knownModels(a.state)
}

func (a *sessionAgent) CurrentModel() string {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.cfg.ModelURI
}

func (a *sessionAgent) CurrentReasoningEffort() string {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.cfg.ReasoningEffort
}

func (a *sessionAgent) ReasoningEfforts(uri string) []string {
	return reasoningEffortsForModel(uri)
}

func (a *sessionAgent) CurrentProfile() string {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.cfg.CapabilityProfile
}

func (a *sessionAgent) CurrentSandbox() string {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.cfg.SandboxPolicy
}

func (a *sessionAgent) SecuritySummary() string {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.cfg.Security.Compact()
}

func (a *sessionAgent) ProcessStatus(jobID string) (toolruntime.ProcessResultMeta, bool) {
	a.mu.RLock()
	processes := a.processes
	if processes == nil {
		a.mu.RUnlock()
		return toolruntime.ProcessResultMeta{}, false
	}
	meta, ok := processes.Status(jobID)
	a.mu.RUnlock()
	return meta, ok
}

func (a *sessionAgent) RunShell(ctx context.Context, command string) (string, toolruntime.ProcessResultMeta, error) {
	return a.runShell(ctx, command, true)
}

func (a *sessionAgent) RunPrivateShell(ctx context.Context, command string) (string, toolruntime.ProcessResultMeta, error) {
	return a.runShell(ctx, command, false)
}

func (a *sessionAgent) runShell(ctx context.Context, command string, record bool) (string, toolruntime.ProcessResultMeta, error) {
	command = strings.TrimSpace(command)
	if command == "" {
		return "", toolruntime.ProcessResultMeta{}, errors.New("shell command is required")
	}

	a.mu.RLock()
	if a.closed || a.engine == nil || a.journal == nil || a.processes == nil {
		a.mu.RUnlock()
		return "", toolruntime.ProcessResultMeta{}, errors.New("cy session runtime is closed")
	}
	eng := a.engine
	journal := a.journal
	processes := a.processes

	var runID, callID string
	if record {
		var err error
		runID, err = newSessionRunID()
		if err != nil {
			a.mu.RUnlock()
			return "", toolruntime.ProcessResultMeta{}, err
		}
		callID, err = newSessionRunID()
		if err != nil {
			a.mu.RUnlock()
			return "", toolruntime.ProcessResultMeta{}, err
		}
		arguments, err := json.Marshal(struct {
			Command string `json:"command"`
		}{Command: command})
		if err != nil {
			a.mu.RUnlock()
			return "", toolruntime.ProcessResultMeta{}, fmt.Errorf("encode shell command: %w", err)
		}
		call := llm.ToolCall{
			ID:   callID,
			Type: string(llm.ToolTypeFunction),
			Function: llm.ToolFunction{
				Name:      "bash",
				Arguments: string(arguments),
			},
		}
		if _, err := journal.Append(session.RecordUserMessage, session.UserMessage{RunID: runID, Content: "!" + command}); err != nil {
			a.mu.RUnlock()
			return "", toolruntime.ProcessResultMeta{}, err
		}
		if _, err := journal.Append(session.RecordAssistantMessage, session.AssistantMessage{RunID: runID, ToolCalls: []llm.ToolCall{call}}); err != nil {
			_, finishErr := journal.Append(session.RecordRunFinished, session.RunFinished{RunID: runID, Outcome: session.RunFailed, Error: err.Error()})
			a.mu.RUnlock()
			return "", toolruntime.ProcessResultMeta{}, errors.Join(err, finishErr)
		}
	}

	startedAt := time.Now()
	result, runErr := processes.RunShell(ctx, command)
	meta, ok := toolruntime.ProcessResultMetaFrom(result.Meta)
	if !ok {
		meta = toolruntime.ProcessResultMeta{
			Type:           toolruntime.ProcessResultMetaType,
			Status:         toolruntime.JobFailed,
			DurationMillis: time.Since(startedAt).Milliseconds(),
		}
	}
	meta.UserInitiated = true
	content := result.Content
	if runErr != nil {
		meta.Status = toolruntime.JobFailed
		if errors.Is(runErr, context.Canceled) {
			meta.Status = toolruntime.JobKilled
		}
		meta.FailureTail = runErr.Error()
		if strings.TrimSpace(content) == "" {
			content = fmt.Sprintf("status: %s\n\n%s\n", meta.Status, runErr)
		}
	}
	var resultErr, finishErr error
	if record {
		_, resultErr = journal.Append(session.RecordToolResult, session.ToolResult{
			RunID:      runID,
			ToolCallID: callID,
			Content:    content,
			Meta:       meta,
		})
		// The run completed even when the command did not: a command that exits
		// non-zero or is killed is an ordinary result for a shell the user asked
		// for, and its fate is recorded in the tool result's meta. Failing to
		// record that result is a different matter.
		finished := session.RunFinished{RunID: runID, Outcome: session.RunCompleted}
		if resultErr != nil {
			finished.Outcome = session.RunFailed
			finished.Error = resultErr.Error()
		}
		_, finishErr = journal.Append(session.RecordRunFinished, finished)
	}
	a.mu.RUnlock()

	if record {
		a.mu.Lock()
		if a.engine == eng && a.journal == journal {
			a.refreshStatusLocked()
		}
		a.mu.Unlock()
	}
	return content, meta, errors.Join(runErr, resultErr, finishErr)
}

func newSessionRunID() (string, error) {
	id, err := uuid.NewRandom()
	if err != nil {
		return "", err
	}
	return id.String(), nil
}

func (a *sessionAgent) SwitchProfile(value string) error {
	profile, err := toolruntime.NormalizeCapabilityProfile(value)
	if err != nil {
		return err
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	eng := a.engine
	processes := a.processes
	if a.closed || eng == nil || processes == nil {
		return errors.New("cy session runtime is closed")
	}
	tools, external, err := a.toolsForProfile(processes, profile)
	if err != nil {
		return err
	}
	if a.state != nil {
		if err := a.state.SetDefaultProfile(profile); err != nil {
			return fmt.Errorf("remember selected profile: %w", err)
		}
	}
	if err := eng.ReconfigureTools(tools, external); err != nil {
		return err
	}
	a.cfg.CapabilityProfile = profile
	a.refreshStatusLocked()
	return nil
}

func (a *sessionAgent) SwitchSandbox(value string) error {
	policy, err := normalizeSandboxPolicy(value)
	if err != nil {
		return err
	}
	a.mu.RLock()
	if a.closed || a.processes == nil {
		a.mu.RUnlock()
		return errors.New("cy session runtime is closed")
	}
	cfg := a.cfg
	processes := a.processes
	a.mu.RUnlock()
	if policy == cfg.SandboxPolicy {
		return nil
	}
	cfg.SandboxPolicy = policy
	security := buildSecurityState(context.Background(), cfg, a.root, a.state)
	cfg.Security = security
	// The same rule as at startup, through the same function: switching to a
	// policy the machine cannot honour should be refused where it is asked for,
	// and two copies of the decision would drift.
	if err := requireEffectiveSandbox(cfg); err != nil {
		return err
	}

	a.mu.Lock()
	defer a.mu.Unlock()
	if a.closed || a.processes != processes {
		return errors.New("cy session runtime changed while switching sandbox")
	}
	if a.state != nil {
		if err := a.state.SetDefaultSandbox(policy); err != nil {
			return fmt.Errorf("remember selected sandbox: %w", err)
		}
	}
	if err := processes.SetSandbox(security.EffectivePolicy); err != nil {
		return err
	}
	a.cfg.SandboxPolicy = policy
	a.cfg.Security = security
	if a.engine != nil {
		return a.engine.ReconfigureSandbox(security.Journal(policy))
	}
	return nil
}

func (a *sessionAgent) reloadTools() error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.closed || a.engine == nil || a.processes == nil {
		return errors.New("cy session runtime is closed")
	}
	tools, external, err := a.toolsForProfile(a.processes, a.cfg.CapabilityProfile)
	if err != nil {
		return err
	}
	if err := a.engine.ReconfigureTools(tools, external); err != nil {
		return err
	}
	a.refreshStatusLocked()
	return nil
}

// toolsForProfile assembles the catalog this profile exposes, and beside it the
// configured tools that survived the filter. Both, because the journal has to
// say what the model was offered and how the calls it made would run, and the
// two answers have to describe the same set.
func (a *sessionAgent) toolsForProfile(processes *toolruntime.ProcessManager, profile string) ([]golem.Tool, []session.ExternalToolConfig, error) {
	tools := toolruntime.FilterWorkspaceToolsForProfile(a.baseTools, profile)
	hnClient := hackernews.NewClient()
	fetchBackends, err := a.webFetchBackends(hnClient)
	if err != nil {
		return nil, nil, err
	}
	tools = append(tools, webfetch.NewTool(fetchBackends...))
	credentials := make([]websearch.Credential, 0, len(serviceCatalog))
	for _, service := range serviceCatalog {
		if !service.search {
			continue
		}
		token, _, err := credentialForProvider(a.state, service.name)
		if err != nil {
			return nil, nil, fmt.Errorf("load %s credential: %w", service.name, err)
		}
		if token != "" {
			credentials = append(credentials, websearch.Credential{Provider: service.name, Token: token})
		}
	}
	searchTool, available, err := websearch.NewTool(credentials)
	if err != nil {
		return nil, nil, err
	}
	if available {
		tools = append(tools, searchTool)
	}
	var configured []session.ExternalToolConfig
	if processes != nil {
		tools = append(tools, processes.Tools()...)
		external, configs, err := a.externalTools(processes, tools)
		if err != nil {
			return nil, nil, err
		}
		tools = append(tools, external...)
		configured = configs
	}
	tools = toolruntime.FilterForProfile(tools, profile)
	if processes != nil {
		tools = processes.EnsureJobTool(tools, a.cfg.ExternalTools)
	}
	if _, err := golem.NewToolSet(tools); err != nil {
		return nil, nil, err
	}
	return tools, keepConfiguredIn(configured, tools), nil
}

// keepConfiguredIn drops the descriptions of tools the profile filtered out.
// A read-only profile hides a tool declared `write`, and recording how it would
// have run reads as though it were on offer.
func keepConfiguredIn(configured []session.ExternalToolConfig, tools []golem.Tool) []session.ExternalToolConfig {
	if len(configured) == 0 {
		return nil
	}
	offered := make(map[string]bool, len(tools))
	for _, tool := range tools {
		offered[tool.Definition.Function.Name] = true
	}
	kept := make([]session.ExternalToolConfig, 0, len(configured))
	for _, config := range configured {
		if offered[config.Name] {
			kept = append(kept, config)
		}
	}
	if len(kept) == 0 {
		return nil
	}
	return kept
}

// externalTools builds the tools declared in configuration, refusing any name
// a built-in already uses. Shadowing would be silent in the worst way: the
// model is shown one name and reaches whichever tool the catalog kept.
//
// Checked against the whole built-in catalog rather than against what this
// profile exposes, so a declaration is not accepted under one profile and
// rejected under another.
func (a *sessionAgent) externalTools(processes *toolruntime.ProcessManager, assembled []golem.Tool) ([]golem.Tool, []session.ExternalToolConfig, error) {
	if len(a.cfg.ExternalTools) == 0 {
		return nil, nil, nil
	}
	taken := make(map[string]bool, len(assembled)+len(a.baseTools))
	for _, tool := range append(append([]golem.Tool(nil), a.baseTools...), assembled...) {
		taken[tool.Definition.Function.Name] = true
	}
	for _, declaration := range a.cfg.ExternalTools {
		if taken[declaration.Name] {
			return nil, nil, asConfigError(fmt.Errorf("tool %q in %s is already a built-in tool; give it another name", declaration.Name, a.cfg.ToolsFile))
		}
	}
	external, configs, err := processes.ExternalTools(a.cfg.ExternalTools)
	if err != nil {
		return nil, nil, asConfigError(fmt.Errorf("%s: %w", a.cfg.ToolsFile, err))
	}
	return external, configs, nil
}

func (a *sessionAgent) webFetchBackends(hnClient *hackernews.Client) ([]webfetch.Backend, error) {
	fetchBackends := []webfetch.Backend{hackernews.NewFetchBackend(hnClient)}
	for _, service := range serviceCatalog {
		if !service.fetch {
			continue
		}
		token, _, err := credentialForProvider(a.state, service.name)
		if err != nil {
			return nil, fmt.Errorf("load %s credential: %w", service.name, err)
		}
		if token == "" {
			continue
		}
		switch service.name {
		case "firecrawl":
			fetchBackends = append(fetchBackends, webfetch.NewFirecrawlBackend(token))
		case "exa":
			fetchBackends = append(fetchBackends, webfetch.NewExaBackend(token))
		}
	}
	fetchBackends = append(fetchBackends, webfetch.NewHTTPBackend())
	return fetchBackends, nil
}

func (a *sessionAgent) build(journal *session.Session, cfg Config, model golem.Model) (*engine.Engine, *toolruntime.ProcessManager, error) {
	instructionPrompts, err := loadInstructions(a.root)
	if err != nil {
		return nil, nil, fmt.Errorf("load project instructions: %w", err)
	}
	// Replayed before the manager exists, because the manager has to know which
	// of the completions it is about to find on disk have already been told.
	// A failure here is not fatal: it costs the run its outstanding completions,
	// and the journal itself is checked properly further along.
	replayed, _ := journal.Replay()
	processes, err := toolruntime.NewProcessManager(toolruntime.ProcessOptions{
		Root:        a.root,
		Home:        resolveStateHome(cfg.Home),
		SessionID:   journal.ID(),
		Sandbox:     runtimeSandboxPolicy(cfg),
		Background:  cfg.BackgroundJobs(),
		JobLauncher: cfg.JobLauncher,
		Delivered:   replayed.DeliveredJobs,
	})
	if err != nil {
		return nil, nil, fmt.Errorf("initialize process runtime: %w", err)
	}
	profile, err := toolruntime.NormalizeCapabilityProfile(cfg.CapabilityProfile)
	if err != nil {
		_ = processes.Close()
		return nil, nil, err
	}
	tools, external, err := a.toolsForProfile(processes, profile)
	if err != nil {
		_ = processes.Close()
		return nil, nil, err
	}
	spec := resolveModelSpecFor(cfg, cfg.ModelURI, a.state, false)
	eng, err := engine.New(engine.Config{
		Model:                  model,
		Session:                journal,
		ModelURI:               cfg.ModelURI,
		SystemPrompt:           cfg.SystemPrompt,
		InstructionPrompts:     instructionPrompts,
		CompactionPrompt:       cfg.CompactionPrompt,
		ToolLimitPrompt:        cfg.ToolLimitPrompt,
		BaseURL:                cfg.BaseURL,
		ContextWindow:          spec.ContextWindow,
		ContextEstimated:       spec.Estimated,
		Tools:                  tools,
		ExternalTools:          external,
		Sandbox:                cfg.Security.Journal(cfg.SandboxPolicy),
		MaxToolIterations:      cfg.MaxToolIterations,
		RequestPolicy:          requestPolicyFor(cfg),
		BackgroundJobs:         cfg.BackgroundJobs(),
		JobLauncher:            cfg.JobLauncher,
		BoundaryEvents:         boundaryEventsFrom(processes),
		BoundaryEventDelivered: processes.MarkCompletionDelivered,
		Sanitize:               a.masker.Redact,
	})
	if err != nil {
		_ = processes.Close()
		return nil, nil, fmt.Errorf("initialize engine: %w", err)
	}
	return eng, processes, nil
}

// Defaults for the retry policy of one logical model request. Retries are
// unlimited in number and bounded in time instead, which is why the budget has
// to stay positive: golem refuses unlimited retries without one.
const (
	defaultRetryBudget       = 15 * time.Minute
	defaultStreamIdleTimeout = 5 * time.Minute
)

// requestPolicyFor bounds one model request. Only the two durations that decide
// how long a broken endpoint costs are configurable; the rest is the shape of
// the backoff rather than its limit.
//
// Both are needed for either to mean anything. The budget is checked between
// attempts, so it bounds a flapping endpoint but not a single hung one: an
// attempt that has connected and gone quiet is bounded only by the idle
// timeout. A retry budget on its own would be a limit that a stalled stream
// walks straight past.
func requestPolicyFor(cfg Config) golem.RequestPolicy {
	return golem.RequestPolicy{
		MaxRetries:        -1,
		BaseDelay:         time.Second,
		MaxDelay:          time.Minute,
		RetryBudget:       cmp.Or(cfg.RetryBudget, defaultRetryBudget),
		StreamIdleTimeout: cmp.Or(cfg.StreamIdleTimeout, defaultStreamIdleTimeout),
	}
}

// boundaryEventsFrom adapts the process manager's completions to what the
// engine journals. The two types are deliberately separate: the process manager
// knows about jobs and nothing about sessions, and this is the one place that
// is allowed to know about both.
func boundaryEventsFrom(processes *toolruntime.ProcessManager) func(string) ([]engine.BoundaryEvent, error) {
	return func(runID string) ([]engine.BoundaryEvent, error) {
		completions, err := processes.PendingCompletionEvents(runID)
		if err != nil {
			return nil, err
		}
		events := make([]engine.BoundaryEvent, 0, len(completions))
		for _, completion := range completions {
			events = append(events, engine.BoundaryEvent{JobID: completion.JobID, FinishedAt: completion.FinishedAt, Content: completion.Content})
		}
		return events, nil
	}
}

func runtimeSandboxPolicy(cfg Config) string {
	if cfg.Security.EffectivePolicy != "" {
		return cfg.Security.EffectivePolicy
	}
	return cfg.SandboxPolicy
}

func (a *sessionAgent) SessionID() string {
	a.mu.RLock()
	defer a.mu.RUnlock()
	if a.journal == nil {
		return ""
	}
	return a.journal.ID()
}

func (a *sessionAgent) SessionRepaired() bool {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.repaired
}

func (a *sessionAgent) HasUserTurn() bool {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.journal != nil && a.journal.HasUserTurn()
}

func (a *sessionAgent) Stream(ctx context.Context, input string, emit golem.StreamFunc) (*golem.Turn, error) {
	if err := a.requireModelCredential(); err != nil {
		return nil, err
	}
	a.mu.RLock()
	if a.engine == nil {
		a.mu.RUnlock()
		return nil, errors.New("cy session runtime is closed")
	}
	eng := a.engine
	turn, err := eng.Stream(ctx, input, emit)
	report, usage, statusErr := eng.Status()
	a.mu.RUnlock()
	a.mu.Lock()
	if a.engine == eng && statusErr == nil {
		a.context = report
		a.usage = usage
	}
	a.mu.Unlock()
	return turn, err
}

func (a *sessionAgent) SessionHistory() ([]llm.Message, error) {
	a.mu.RLock()
	defer a.mu.RUnlock()
	if a.engine == nil {
		return nil, errors.New("cy session runtime is closed")
	}
	return a.engine.History()
}

func (a *sessionAgent) SessionUsage() (llm.Usage, error) {
	a.mu.RLock()
	defer a.mu.RUnlock()
	if a.engine == nil {
		return llm.Usage{}, errors.New("cy session runtime is closed")
	}
	return a.usage, nil
}

func (a *sessionAgent) QueueInput(content string) error {
	a.mu.RLock()
	defer a.mu.RUnlock()
	if a.engine == nil {
		return errors.New("cy session runtime is closed")
	}
	// QueueInput runs synchronously in the TUI event loop, so keep it limited to
	// the in-memory queue; context and status refresh at the model boundary.
	return a.engine.QueueInput(content)
}

func (a *sessionAgent) ClaimQueued() (string, bool, error) {
	a.mu.RLock()
	defer a.mu.RUnlock()
	if a.engine == nil {
		return "", false, errors.New("cy session runtime is closed")
	}
	return a.engine.ClaimQueued()
}

func (a *sessionAgent) PopQueued() (string, bool, error) {
	a.mu.RLock()
	defer a.mu.RUnlock()
	if a.engine == nil {
		return "", false, errors.New("cy session runtime is closed")
	}
	return a.engine.PopQueued()
}

func (a *sessionAgent) RestoreQueued() ([]string, error) {
	a.mu.RLock()
	defer a.mu.RUnlock()
	if a.engine == nil {
		return nil, errors.New("cy session runtime is closed")
	}
	return a.engine.RestoreQueued()
}

func (a *sessionAgent) ClearSession() (string, error) {
	a.mu.RLock()
	cfg := a.cfg
	model := a.model
	a.mu.RUnlock()
	journal, err := session.Create(session.CreateOptions{
		Home:            cfg.Home,
		Workspace:       a.root,
		Model:           cfg.ModelURI,
		ReasoningEffort: cfg.ReasoningEffort,
	})
	if err != nil {
		return "", err
	}
	if err := a.install(journal, cfg, model); err != nil {
		_ = journal.Close()
		return "", err
	}
	return journal.ID(), nil
}

func (a *sessionAgent) ResumeSession(idOrPrefix string) (string, error) {
	a.mu.RLock()
	cfg := a.cfg
	model := a.model
	a.mu.RUnlock()
	journal, err := session.Open(cfg.Home, idOrPrefix)
	if err != nil {
		return "", err
	}
	state, err := journal.Replay()
	if err != nil {
		_ = journal.Close()
		return "", err
	}
	if state.Header.Workspace != "" && state.Header.Workspace != a.root {
		_ = journal.Close()
		return "", fmt.Errorf("session workspace is %s; current workspace is %s", state.Header.Workspace, a.root)
	}
	if state.Model != "" && (state.Model != cfg.ModelURI || state.ReasoningEffort != cfg.ReasoningEffort) {
		cfg.ModelURI = state.Model
		cfg.ReasoningEffort = state.ReasoningEffort
		model, err = buildModel(cfg, a.state, false)
		if err != nil {
			_ = journal.Close()
			return "", fmt.Errorf("restore session model: %w", err)
		}
	}
	if err := a.install(journal, cfg, model); err != nil {
		_ = journal.Close()
		return "", err
	}
	return journal.ID(), nil
}

func (a *sessionAgent) install(journal *session.Session, cfg Config, model golem.Model) error {
	eng, processes, err := a.build(journal, cfg, model)
	if err != nil {
		return err
	}
	report, usage, err := eng.Status()
	if err != nil {
		_ = processes.Close()
		return fmt.Errorf("initialize session status: %w", err)
	}
	a.mu.Lock()
	if a.closed {
		a.mu.Unlock()
		_ = processes.Close()
		return errors.New("cy session runtime is closed")
	}
	oldJournal := a.journal
	oldProcesses := a.processes
	a.engine = eng
	a.journal = journal
	a.processes = processes
	a.cfg = cfg
	a.model = model
	a.context = report
	a.usage = usage
	a.repaired = journal.TailRepaired()
	a.mu.Unlock()

	if oldProcesses != nil {
		_ = oldProcesses.Close()
	}
	if oldJournal != nil {
		_ = oldJournal.ClosePruningEmpty()
	}
	return nil
}

func (a *sessionAgent) ContextReport() (engine.ContextReport, error) {
	a.mu.RLock()
	defer a.mu.RUnlock()
	if a.engine == nil {
		return engine.ContextReport{}, errors.New("cy session runtime is closed")
	}
	return a.engine.ContextReport()
}

func (a *sessionAgent) CachedContextReport() engine.ContextReport {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.context
}

func (a *sessionAgent) Compact(ctx context.Context, focus string) (engine.ContextReport, error) {
	if err := a.requireModelCredential(); err != nil {
		return engine.ContextReport{}, err
	}
	a.mu.RLock()
	if a.engine == nil {
		a.mu.RUnlock()
		return engine.ContextReport{}, errors.New("cy session runtime is closed")
	}
	eng := a.engine
	report, usage, err := eng.Compact(ctx, focus)
	if err != nil {
		a.mu.RUnlock()
		return report, err
	}
	a.mu.RUnlock()
	a.mu.Lock()
	if a.engine == eng {
		a.usage = usage
		a.context = report
	}
	a.mu.Unlock()
	return report, nil
}

func (a *sessionAgent) requireModelCredential() error {
	a.mu.RLock()
	uri := a.cfg.ModelURI
	baseURL := strings.TrimSpace(a.cfg.BaseURL)
	store := a.state
	a.mu.RUnlock()

	provider := modelProvider(uri)
	if !isModelLoginProvider(provider) {
		return nil
	}
	// A replaced endpoint is not the provider's, so it need not hold the
	// provider's credential, on the same grounds as in buildModel. This check
	// is separate because it runs before every turn rather than once at
	// startup, and a session may switch models while it runs.
	if baseURL != "" {
		return nil
	}
	token, _, err := credentialForProvider(store, provider)
	if err != nil {
		return err
	}
	if token == "" {
		return missingProviderCredentialError(provider, uri)
	}
	return nil
}

func (a *sessionAgent) ListSessions() ([]session.Summary, error) {
	return session.List(a.cfg.Home, a.root)
}

func (a *sessionAgent) Close() error {
	a.mu.Lock()
	if a.closed {
		a.mu.Unlock()
		return nil
	}
	a.closed = true
	journal := a.journal
	processes := a.processes
	a.engine = nil
	a.journal = nil
	a.processes = nil
	a.mu.Unlock()
	var err error
	if processes != nil {
		err = errors.Join(err, processes.Close())
	}
	if journal != nil {
		err = errors.Join(err, journal.ClosePruningEmpty())
	}
	return err
}
