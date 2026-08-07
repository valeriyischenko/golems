package main

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"strings"
	"testing"

	"github.com/levmv/golems/cy/internal/session"
	"github.com/levmv/golems/cy/internal/state"
	toolruntime "github.com/levmv/golems/cy/internal/tools"
	"github.com/levmv/golems/pkg/golem"
	"github.com/levmv/golems/pkg/hackernews"
	"github.com/levmv/golems/pkg/llm"
	"github.com/levmv/golems/pkg/webfetch"
)

func TestSessionAgentRunShellRecordsModelVisibleTurn(t *testing.T) {
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash is unavailable")
	}
	home := t.TempDir()
	root := t.TempDir()
	journal, err := session.Create(session.CreateOptions{Home: home, Workspace: root, Model: "fake/model"})
	if err != nil {
		t.Fatal(err)
	}
	model := &runTurnFakeModel{streams: [][]llm.StreamChunk{{{
		Text: "ack", FinishReason: llm.FinishReasonStop,
	}}}}
	agent, err := newSessionAgent(
		Config{Home: home, ModelURI: "fake/model", CapabilityProfile: "full", SandboxPolicy: sandboxOff},
		model, root, nil, journal, nil,
	)
	if err != nil {
		t.Fatal(err)
	}
	defer agent.Close()

	content, meta, err := agent.RunShell(context.Background(), "printf 'shell-context'")
	if err != nil {
		t.Fatal(err)
	}
	if !meta.UserInitiated || meta.Status != toolruntime.JobCompleted || !strings.Contains(content, "shell-context") {
		t.Fatalf("shell result content=%q meta=%#v", content, meta)
	}
	history, err := agent.SessionHistory()
	if err != nil {
		t.Fatal(err)
	}
	if len(history) != 3 {
		t.Fatalf("history = %#v", history)
	}
	if history[0].Role != llm.RoleUser || history[0].Content != "!printf 'shell-context'" {
		t.Fatalf("shell user message = %#v", history[0])
	}
	if history[1].Role != llm.RoleAI || len(history[1].ToolCalls) != 1 || history[1].ToolCalls[0].Function.Name != "bash" {
		t.Fatalf("shell tool call = %#v", history[1])
	}
	replayedMeta, ok := toolruntime.ProcessResultMetaFrom(history[2].Meta)
	if history[2].Role != llm.RoleTool || !ok || !replayedMeta.UserInitiated || !strings.Contains(history[2].Content, "shell-context") {
		t.Fatalf("shell tool result = %#v", history[2])
	}
	if _, err := agent.Stream(context.Background(), "use that output", nil); err != nil {
		t.Fatal(err)
	}
	if len(model.requests) != 1 {
		t.Fatalf("model requests = %d, want 1", len(model.requests))
	}
	request := model.requests[0]
	if len(request.Messages) < 4 {
		t.Fatalf("model context = %#v", request.Messages)
	}
	contextTail := request.Messages[len(request.Messages)-4:]
	if contextTail[0].Role != llm.RoleUser || contextTail[0].Content != "!printf 'shell-context'" ||
		contextTail[1].Role != llm.RoleAI || len(contextTail[1].ToolCalls) != 1 ||
		contextTail[2].Role != llm.RoleTool || !strings.Contains(contextTail[2].Content, "shell-context") ||
		contextTail[3].Role != llm.RoleUser || contextTail[3].Content != "use that output" {
		t.Fatalf("model context tail = %#v", contextTail)
	}
}

func TestSessionAgentRunShellIsAvailableOutsideFullProfile(t *testing.T) {
	home := t.TempDir()
	root := t.TempDir()
	journal, err := session.Create(session.CreateOptions{Home: home, Workspace: root, Model: "fake/model"})
	if err != nil {
		t.Fatal(err)
	}
	agent, err := newSessionAgent(
		Config{Home: home, ModelURI: "fake/model", CapabilityProfile: "edit", SandboxPolicy: sandboxOff},
		&runTurnFakeModel{}, root, nil, journal, nil,
	)
	if err != nil {
		t.Fatal(err)
	}
	defer agent.Close()

	content, meta, err := agent.RunShell(context.Background(), "printf user-shell")
	if err != nil {
		t.Fatal(err)
	}
	if !meta.UserInitiated || meta.Status != toolruntime.JobCompleted || !strings.Contains(content, "user-shell") {
		t.Fatalf("shell result content=%q meta=%#v", content, meta)
	}
	history, err := agent.SessionHistory()
	if err != nil {
		t.Fatal(err)
	}
	if len(history) != 3 || history[0].Content != "!printf user-shell" {
		t.Fatalf("shell history = %#v", history)
	}
}

func TestSessionAgentRunPrivateShellDoesNotEnterSession(t *testing.T) {
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash is unavailable")
	}
	home := t.TempDir()
	root := t.TempDir()
	journal, err := session.Create(session.CreateOptions{Home: home, Workspace: root, Model: "fake/model"})
	if err != nil {
		t.Fatal(err)
	}
	model := &runTurnFakeModel{streams: [][]llm.StreamChunk{{{
		Text: "ack", FinishReason: llm.FinishReasonStop,
	}}}}
	agent, err := newSessionAgent(
		Config{Home: home, ModelURI: "fake/model", CapabilityProfile: "full", SandboxPolicy: sandboxOff},
		model, root, nil, journal, nil,
	)
	if err != nil {
		t.Fatal(err)
	}
	defer agent.Close()

	content, meta, err := agent.RunPrivateShell(context.Background(), "printf 'private-shell-output'")
	if err != nil {
		t.Fatal(err)
	}
	if !meta.UserInitiated || meta.Status != toolruntime.JobCompleted || !strings.Contains(content, "private-shell-output") {
		t.Fatalf("private shell result content=%q meta=%#v", content, meta)
	}
	history, err := agent.SessionHistory()
	if err != nil {
		t.Fatal(err)
	}
	if len(history) != 0 {
		t.Fatalf("private shell entered session history: %#v", history)
	}

	if _, err := agent.Stream(context.Background(), "continue", nil); err != nil {
		t.Fatal(err)
	}
	if len(model.requests) != 1 {
		t.Fatalf("model requests = %d, want 1", len(model.requests))
	}
	for _, message := range model.requests[0].Messages {
		if strings.Contains(message.Content, "private-shell-output") {
			t.Fatalf("private shell entered model context: %#v", model.requests[0].Messages)
		}
	}
}

func TestSessionAgentLoginAddsAndRemovesWebSearch(t *testing.T) {
	t.Setenv("TAVILY_API_KEY", "")
	t.Setenv("EXA_API_KEY", "")
	t.Setenv("FIRECRAWL_API_KEY", "")
	home := t.TempDir()
	root := t.TempDir()
	journal, err := session.Create(session.CreateOptions{Home: home, Workspace: root, Model: "fake/model"})
	if err != nil {
		t.Fatal(err)
	}
	store, err := state.Open(home)
	if err != nil {
		t.Fatal(err)
	}
	agent, err := newSessionAgent(
		Config{Home: home, ModelURI: "fake/model", CapabilityProfile: "full", SandboxPolicy: sandboxOff},
		&runTurnFakeModel{}, root, nil, journal, store,
	)
	if err != nil {
		t.Fatal(err)
	}
	defer agent.Close()

	before, err := agent.ContextReport()
	if err != nil {
		t.Fatal(err)
	}
	if err := agent.Login("tavily", "tvly-test-key"); err != nil {
		t.Fatal(err)
	}
	afterLogin, err := agent.ContextReport()
	if err != nil {
		t.Fatal(err)
	}
	if afterLogin.ToolTokens <= before.ToolTokens {
		t.Fatalf("tool tokens did not grow after Tavily login: before=%d after=%d", before.ToolTokens, afterLogin.ToolTokens)
	}
	if err := agent.Logout("tavily"); err != nil {
		t.Fatal(err)
	}
	afterLogout, err := agent.ContextReport()
	if err != nil {
		t.Fatal(err)
	}
	if afterLogout.ToolTokens != before.ToolTokens {
		t.Fatalf("tool tokens after logout = %d, want %d", afterLogout.ToolTokens, before.ToolTokens)
	}
}

func TestConfiguredToolsIncludeFetchAndCredentialedSearch(t *testing.T) {
	t.Setenv("TAVILY_API_KEY", "")
	t.Setenv("EXA_API_KEY", "")
	t.Setenv("FIRECRAWL_API_KEY", "")
	store, err := state.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	agent := &sessionAgent{state: store}
	tools, _, err := agent.toolsForProfile(nil, "read-only")
	if err != nil {
		t.Fatal(err)
	}
	if got := toolNames(tools); strings.Join(got, ",") != "web_fetch" {
		t.Fatalf("tools without search credential = %v", got)
	}
	if err := store.SetAPIKey("tavily", "tvly-test-key"); err != nil {
		t.Fatal(err)
	}
	tools, _, err = agent.toolsForProfile(nil, "read-only")
	if err != nil {
		t.Fatal(err)
	}
	if got := toolNames(tools); strings.Join(got, ",") != "web_fetch,web_search" {
		t.Fatalf("tools with search credential = %v", got)
	}
}

func TestWebFetchBackendsFollowConfiguredCredentialOrder(t *testing.T) {
	t.Setenv("EXA_API_KEY", "")
	t.Setenv("FIRECRAWL_API_KEY", "")
	store, err := state.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	agent := &sessionAgent{state: store}
	backends, err := agent.webFetchBackends(hackernews.NewClient())
	if err != nil {
		t.Fatal(err)
	}
	if got := backendNames(backends); strings.Join(got, ",") != "hacker_news,http" {
		t.Fatalf("backends without service credentials = %v", got)
	}
	if err := store.SetAPIKey("exa", "exa-test-key"); err != nil {
		t.Fatal(err)
	}
	if err := store.SetAPIKey("firecrawl", "fc-test-key"); err != nil {
		t.Fatal(err)
	}
	backends, err = agent.webFetchBackends(hackernews.NewClient())
	if err != nil {
		t.Fatal(err)
	}
	if got := backendNames(backends); strings.Join(got, ",") != "hacker_news,firecrawl,exa,http" {
		t.Fatalf("credentialed backends = %v", got)
	}
}

func backendNames(backends []webfetch.Backend) []string {
	names := make([]string, 0, len(backends))
	for _, backend := range backends {
		names = append(names, backend.Name())
	}
	return names
}

func toolNames(tools []golem.Tool) []string {
	names := make([]string, 0, len(tools))
	for _, tool := range tools {
		names = append(names, tool.Definition.Function.Name)
	}
	return names
}

func TestSessionAgentSwitchModelPersistsJournalAndDefault(t *testing.T) {
	home := t.TempDir()
	root := t.TempDir()
	journal, err := session.Create(session.CreateOptions{Home: home, Workspace: root, Model: "deepseek/deepseek-v4-flash"})
	if err != nil {
		t.Fatal(err)
	}
	store, err := state.Open(home)
	if err != nil {
		t.Fatal(err)
	}
	agent, err := newSessionAgent(Config{ModelURI: "deepseek/deepseek-v4-flash", CapabilityProfile: "full", SandboxPolicy: sandboxOff}, &runTurnFakeModel{}, root, nil, journal, store)
	if err != nil {
		t.Fatal(err)
	}
	defer agent.Close()
	if err := agent.SwitchModelWithEffort("ollama/local-model", ""); err != nil {
		t.Fatal(err)
	}
	state, err := journal.Replay()
	if err != nil {
		t.Fatal(err)
	}
	if state.Model != "ollama/local-model" {
		t.Fatalf("journal model = %q", state.Model)
	}
	stored, err := store.Config()
	if err != nil || stored.Model != "ollama/local-model" {
		t.Fatalf("settings = %#v, %v", stored, err)
	}
}

func TestSessionAgentSwitchReasoningEffortPersistsWithModel(t *testing.T) {
	home := t.TempDir()
	root := t.TempDir()
	uri := "deepseek/deepseek-v4-flash"
	journal, err := session.Create(session.CreateOptions{Home: home, Workspace: root, Model: uri})
	if err != nil {
		t.Fatal(err)
	}
	store, err := state.Open(home)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.SetAPIKey("deepseek", "test-key"); err != nil {
		t.Fatal(err)
	}
	agent, err := newSessionAgent(Config{ModelURI: uri, CapabilityProfile: "full", SandboxPolicy: sandboxOff}, &runTurnFakeModel{}, root, nil, journal, store)
	if err != nil {
		t.Fatal(err)
	}
	defer agent.Close()
	if err := agent.SwitchModelWithEffort(uri, "high"); err != nil {
		t.Fatal(err)
	}
	replayed, err := journal.Replay()
	if err != nil {
		t.Fatal(err)
	}
	if replayed.Model != uri || replayed.ReasoningEffort != "high" {
		t.Fatalf("journal selection = %q/%q", replayed.Model, replayed.ReasoningEffort)
	}
	stored, err := store.Config()
	if err != nil || stored.Model != uri || stored.ReasoningEffort != "high" {
		t.Fatalf("stored selection = %#v, %v", stored, err)
	}
	if agent.CurrentReasoningEffort() != "high" {
		t.Fatalf("current effort = %q", agent.CurrentReasoningEffort())
	}
}

func TestSessionAgentSwitchProfilePersistsDefault(t *testing.T) {
	home := t.TempDir()
	root := t.TempDir()
	journal, err := session.Create(session.CreateOptions{Home: home, Workspace: root, Model: "fake/model"})
	if err != nil {
		t.Fatal(err)
	}
	store, err := state.Open(home)
	if err != nil {
		t.Fatal(err)
	}
	agent, err := newSessionAgent(Config{Home: home, ModelURI: "fake/model", CapabilityProfile: "full", SandboxPolicy: sandboxOff}, &runTurnFakeModel{}, root, nil, journal, store)
	if err != nil {
		_ = journal.Close()
		t.Fatal(err)
	}
	defer agent.Close()

	if err := agent.SwitchProfile("edit"); err != nil {
		t.Fatal(err)
	}
	if agent.CurrentProfile() != "edit" {
		t.Fatalf("profile = %q", agent.CurrentProfile())
	}
	stored, err := store.Config()
	if err != nil || stored.Profile != "edit" {
		t.Fatalf("stored settings = %#v, %v", stored, err)
	}
}

func TestSessionAgentSwitchSandboxPersistsRequestedPolicy(t *testing.T) {
	home := t.TempDir()
	root := t.TempDir()
	journal, err := session.Create(session.CreateOptions{Home: home, Workspace: root, Model: "fake/model"})
	if err != nil {
		t.Fatal(err)
	}
	store, err := state.Open(home)
	if err != nil {
		t.Fatal(err)
	}
	agent, err := newSessionAgent(Config{Home: home, ModelURI: "fake/model", CapabilityProfile: "full", SandboxPolicy: sandboxOff}, &runTurnFakeModel{}, root, nil, journal, store)
	if err != nil {
		_ = journal.Close()
		t.Fatal(err)
	}
	defer agent.Close()

	if err := agent.SwitchSandbox(sandboxAuto); err != nil {
		t.Fatal(err)
	}
	if got := agent.CurrentSandbox(); got != sandboxAuto {
		t.Fatalf("sandbox policy = %q", got)
	}
	if summary := agent.SecuritySummary(); !strings.HasPrefix(summary, "sandbox: ") {
		t.Fatalf("security summary = %q", summary)
	}
	stored, err := store.Config()
	if err != nil || stored.Sandbox != sandboxAuto {
		t.Fatalf("stored settings = %#v, %v", stored, err)
	}
}

func TestRuntimeSandboxPolicyUsesEffectivePolicy(t *testing.T) {
	cfg := Config{SandboxPolicy: sandboxAuto, Security: SecurityState{EffectivePolicy: sandboxOff}}
	if got := runtimeSandboxPolicy(cfg); got != sandboxOff {
		t.Fatalf("runtimeSandboxPolicy() = %q, want %q", got, sandboxOff)
	}
}

func TestSessionAgentSwitchDoesNotMutateRuntimeWhenDefaultCannotBeSaved(t *testing.T) {
	stateHome := t.TempDir()
	store, err := state.Open(stateHome)
	if err != nil {
		t.Fatal(err)
	}
	sessionHome := t.TempDir()
	root := t.TempDir()
	journal, err := session.Create(session.CreateOptions{Home: sessionHome, Workspace: root, Model: "fake/model"})
	if err != nil {
		t.Fatal(err)
	}
	agent, err := newSessionAgent(Config{Home: sessionHome, ModelURI: "fake/model", CapabilityProfile: "full", SandboxPolicy: sandboxOff}, &runTurnFakeModel{}, root, nil, journal, store)
	if err != nil {
		t.Fatal(err)
	}
	defer agent.Close()

	if err := os.RemoveAll(stateHome); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(stateHome, []byte("not a directory"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := agent.SwitchModelWithEffort("ollama/local-model", ""); err == nil {
		t.Fatal("SwitchModelWithEffort() succeeded with an unwritable state store")
	}
	if got := agent.CurrentModel(); got != "fake/model" {
		t.Fatalf("model changed after persistence failure: %q", got)
	}
	replayed, err := journal.Replay()
	if err != nil {
		t.Fatal(err)
	}
	if replayed.Model != "fake/model" {
		t.Fatalf("journal model changed after persistence failure: %q", replayed.Model)
	}

	if err := agent.SwitchProfile("edit"); err == nil {
		t.Fatal("SwitchProfile() succeeded with an unwritable state store")
	}
	if got := agent.CurrentProfile(); got != "full" {
		t.Fatalf("profile changed after persistence failure: %q", got)
	}

	if err := agent.SwitchSandbox(sandboxAuto); err == nil {
		t.Fatal("SwitchSandbox() succeeded with an unwritable state store")
	}
	if got := agent.CurrentSandbox(); got != sandboxOff {
		t.Fatalf("sandbox changed after persistence failure: %q", got)
	}
}

func TestSessionAgentMissingCredentialFailsAtModelCallBoundary(t *testing.T) {
	t.Setenv("DEEPSEEK_API_KEY", "")
	home := t.TempDir()
	root := t.TempDir()
	journal, err := session.Create(session.CreateOptions{Home: home, Workspace: root, Model: "deepseek/deepseek-v4-flash"})
	if err != nil {
		t.Fatal(err)
	}
	store, err := state.Open(home)
	if err != nil {
		t.Fatal(err)
	}
	agent, err := newSessionAgent(Config{ModelURI: "deepseek/deepseek-v4-flash", CapabilityProfile: "full", SandboxPolicy: sandboxOff}, &runTurnFakeModel{}, root, nil, journal, store)
	if err != nil {
		t.Fatal(err)
	}
	defer agent.Close()

	_, err = agent.Stream(context.Background(), "hello", nil)
	if err == nil || !strings.Contains(err.Error(), "set DEEPSEEK_API_KEY") || !strings.Contains(err.Error(), "use /login deepseek") {
		t.Fatalf("Stream() error = %v, want login guidance", err)
	}
	state, replayErr := journal.Replay()
	if replayErr != nil {
		t.Fatal(replayErr)
	}
	if len(state.Messages) != 0 {
		t.Fatalf("missing-credential turn changed history: %#v", state.Messages)
	}
}

func TestSessionAgentConfiguredBaseURLDoesNotRequireProviderCredential(t *testing.T) {
	t.Setenv("DEEPSEEK_API_KEY", "")
	home := t.TempDir()
	root := t.TempDir()
	journal, err := session.Create(session.CreateOptions{Home: home, Workspace: root, Model: "deepseek/deepseek-v4-flash"})
	if err != nil {
		t.Fatal(err)
	}
	store, err := state.Open(home)
	if err != nil {
		t.Fatal(err)
	}
	model := &runTurnFakeModel{streams: [][]llm.StreamChunk{{{
		Text: "ack", FinishReason: llm.FinishReasonStop,
	}}}}
	cfg := Config{
		Home:              home,
		ModelURI:          "deepseek/deepseek-v4-flash",
		BaseURL:           "http://127.0.0.1:9099/v1",
		CapabilityProfile: "full",
		SandboxPolicy:     sandboxOff,
	}
	agent, err := newSessionAgent(cfg, model, root, nil, journal, store)
	if err != nil {
		t.Fatal(err)
	}
	defer agent.Close()

	// The credential is checked once more at the model call boundary, which is
	// the check a session reaches when it is already running.
	if _, err := agent.Stream(context.Background(), "hello", nil); err != nil {
		t.Fatalf("Stream() error = %v, want the turn to proceed without a provider credential", err)
	}
}

func TestSessionAgentClearAndResumeSwitchDurableRuntime(t *testing.T) {
	home := t.TempDir()
	root := t.TempDir()
	cfg := Config{Home: home, RootDir: root, ModelURI: "fake/model"}
	initial, err := session.Create(session.CreateOptions{Home: home, Workspace: root, Model: cfg.ModelURI})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := initial.Append(session.RecordUserMessage, session.UserMessage{RunID: "seed", Content: "keep this session"}); err != nil {
		t.Fatal(err)
	}
	agent, err := newSessionAgent(cfg, &runTurnFakeModel{}, root, nil, initial, nil)
	if err != nil {
		_ = initial.Close()
		t.Fatal(err)
	}
	defer agent.Close()
	initialID := agent.SessionID()

	freshID, err := agent.ClearSession()
	if err != nil {
		t.Fatal(err)
	}
	if freshID == initialID || agent.SessionID() != freshID {
		t.Fatalf("clear switched to %q from %q; current=%q", freshID, initialID, agent.SessionID())
	}
	probe, err := session.Open(home, initialID)
	if err != nil {
		t.Fatalf("previous session was not closed after clear: %v", err)
	}
	if err := probe.Close(); err != nil {
		t.Fatal(err)
	}

	resumedID, err := agent.ResumeSession(initialID[:12])
	if err != nil {
		t.Fatal(err)
	}
	if resumedID != initialID || agent.SessionID() != initialID {
		t.Fatalf("resume id=%q current=%q want=%q", resumedID, agent.SessionID(), initialID)
	}
	summaries, err := agent.ListSessions()
	if err != nil {
		t.Fatal(err)
	}
	if len(summaries) != 1 || summaries[0].ID != initialID {
		t.Fatalf("summaries = %#v", summaries)
	}
}

func TestSessionAgentResumeRestoresSessionModel(t *testing.T) {
	home := t.TempDir()
	root := t.TempDir()
	currentModel := "deepseek/deepseek-v4-flash"
	resumedModel := "openai/test-model"
	initial, err := session.Create(session.CreateOptions{Home: home, Workspace: root, Model: currentModel})
	if err != nil {
		t.Fatal(err)
	}
	target, err := session.Create(session.CreateOptions{Home: home, Workspace: root, Model: resumedModel})
	if err != nil {
		t.Fatal(err)
	}
	targetID := target.ID()
	if _, err := target.Append(session.RecordUserMessage, session.UserMessage{RunID: "seed", Content: "resume me"}); err != nil {
		t.Fatal(err)
	}
	if err := target.Close(); err != nil {
		t.Fatal(err)
	}
	cfg := Config{Home: home, RootDir: root, ModelURI: currentModel, SandboxPolicy: sandboxOff}
	agent, err := newSessionAgent(cfg, &runTurnFakeModel{}, root, nil, initial, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer agent.Close()
	if _, err := agent.ResumeSession(targetID); err != nil {
		t.Fatal(err)
	}
	if got := agent.CurrentModel(); got != resumedModel {
		t.Fatalf("CurrentModel() = %q, want %q", got, resumedModel)
	}
	records, err := agent.journal.Records()
	if err != nil {
		t.Fatal(err)
	}
	for _, record := range records {
		if record.Type == session.RecordModelChanged {
			t.Fatalf("resume rewrote session model: %#v", record)
		}
	}
}

func TestSessionAgentResumeReportsAlreadyOpenSession(t *testing.T) {
	home := t.TempDir()
	root := t.TempDir()
	cfg := Config{Home: home, RootDir: root, ModelURI: "fake/model"}
	initial, err := session.Create(session.CreateOptions{Home: home, Workspace: root, Model: cfg.ModelURI})
	if err != nil {
		t.Fatal(err)
	}
	agent, err := newSessionAgent(cfg, &runTurnFakeModel{}, root, nil, initial, nil)
	if err != nil {
		_ = initial.Close()
		t.Fatal(err)
	}
	defer agent.Close()
	locked, err := session.Create(session.CreateOptions{Home: home, Workspace: root, Model: cfg.ModelURI})
	if err != nil {
		t.Fatal(err)
	}
	defer locked.Close()

	_, err = agent.ResumeSession(locked.ID())
	if !errors.Is(err, session.ErrSessionLocked) {
		t.Fatalf("ResumeSession() error = %v, want ErrSessionLocked", err)
	}
}

func TestSessionAgentListsAndResumesOnlyCurrentWorkspace(t *testing.T) {
	home := t.TempDir()
	root := t.TempDir()
	otherRoot := t.TempDir()
	cfg := Config{Home: home, RootDir: root, ModelURI: "fake/model"}
	initial, err := session.Create(session.CreateOptions{Home: home, Workspace: root, Model: cfg.ModelURI})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := initial.Append(session.RecordUserMessage, session.UserMessage{RunID: "run", Content: "current workspace session"}); err != nil {
		_ = initial.Close()
		t.Fatal(err)
	}
	agent, err := newSessionAgent(cfg, &runTurnFakeModel{}, root, nil, initial, nil)
	if err != nil {
		_ = initial.Close()
		t.Fatal(err)
	}
	defer agent.Close()
	other, err := session.Create(session.CreateOptions{Home: home, Workspace: otherRoot, Model: cfg.ModelURI})
	if err != nil {
		t.Fatal(err)
	}
	otherID := other.ID()
	if err := other.Close(); err != nil {
		t.Fatal(err)
	}

	summaries, err := agent.ListSessions()
	if err != nil {
		t.Fatal(err)
	}
	if len(summaries) != 1 {
		t.Fatalf("workspace-filtered summaries = %#v", summaries)
	}
	if _, err := agent.ResumeSession(otherID); err == nil || !strings.Contains(err.Error(), "current workspace") {
		t.Fatalf("cross-workspace resume error = %v", err)
	}
}

// The catalog and the description of how it runs have to name the same tools.
// A read-only profile hides a tool declared `write`, and a journal that still
// described that tool's command would say it was on offer when it was not.
func TestConfiguredToolDescriptionsFollowTheProfileFilter(t *testing.T) {
	bash, err := exec.LookPath("bash")
	if err != nil {
		t.Skip("bash is unavailable")
	}
	t.Setenv("TAVILY_API_KEY", "")
	t.Setenv("EXA_API_KEY", "")
	t.Setenv("FIRECRAWL_API_KEY", "")
	store, err := state.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	processes, err := toolruntime.NewProcessManager(toolruntime.ProcessOptions{Root: t.TempDir(), Home: t.TempDir(), Sandbox: "off"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = processes.Close() })
	agent := &sessionAgent{state: store, cfg: Config{ExternalTools: []toolruntime.ExternalTool{
		{Name: "notes", Description: "read notes", Effect: "read", Command: []string{bash, "-c", "true"}},
		{Name: "publish", Description: "write notes", Effect: "write", Command: []string{bash, "-c", "true"}},
	}}}

	_, configured, err := agent.toolsForProfile(processes, "full")
	if err != nil {
		t.Fatal(err)
	}
	if got := configuredToolNames(configured); strings.Join(got, ",") != "notes,publish" {
		t.Fatalf("configured under full = %v", got)
	}

	tools, configured, err := agent.toolsForProfile(processes, "read-only")
	if err != nil {
		t.Fatal(err)
	}
	if got := configuredToolNames(configured); strings.Join(got, ",") != "notes" {
		t.Fatalf("configured under read-only = %v", got)
	}
	if names := toolNames(tools); strings.Contains(strings.Join(names, ","), "publish") {
		t.Fatalf("read-only still offers publish: %v", names)
	}
}

func configuredToolNames(configured []session.ExternalToolConfig) []string {
	names := make([]string, 0, len(configured))
	for _, config := range configured {
		names = append(names, config.Name)
	}
	return names
}
