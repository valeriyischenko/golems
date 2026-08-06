package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/levmv/golems/cy/internal/session"
	toolruntime "github.com/levmv/golems/cy/internal/tools"
	"github.com/levmv/golems/cy/internal/ui"
	"github.com/levmv/golems/pkg/golem"
	"github.com/levmv/golems/pkg/jsonschema"
	"github.com/levmv/golems/pkg/llm"
)

func TestMain(m *testing.M) {
	if toolruntime.RunSandboxChildIfRequested() {
		return
	}
	os.Exit(m.Run())
}

func TestLoadConfigReadsSystemPrompt(t *testing.T) {
	t.Setenv("CY_SYSTEM_PROMPT", "custom system prompt")
	if got := LoadConfig().SystemPrompt; got != "custom system prompt" {
		t.Fatalf("SystemPrompt = %q", got)
	}
}

func TestApplyResumedConfigHonorsExplicitEnvironment(t *testing.T) {
	t.Setenv("CY_MODEL", "openai/from-env")
	t.Setenv("CY_ROOT", "/from-env")
	cfg := Config{ModelURI: "openai/from-env", RootDir: "/from-env"}
	applyResumedConfig(&cfg, session.State{
		Model:  "deepseek/from-session",
		Header: session.SessionStarted{Workspace: "/from-session"},
	}, nil)
	if cfg.ModelURI != "openai/from-env" || cfg.RootDir != "/from-env" {
		t.Fatalf("explicit environment was overwritten: %#v", cfg)
	}
}

func TestApplyResumedConfigRestoresReasoningEffortWithModel(t *testing.T) {
	t.Setenv("CY_MODEL", "")
	cfg := Config{ModelURI: "deepseek/deepseek-v4-flash", RootDir: "."}
	applyResumedConfig(&cfg, session.State{
		Model:           "deepseek/deepseek-v4-flash",
		ReasoningEffort: "high",
	}, nil)
	if cfg.ModelURI != "deepseek/deepseek-v4-flash" || cfg.ReasoningEffort != "high" {
		t.Fatalf("resumed selection = %#v", cfg)
	}
}

func TestNormalizeTerminalTheme(t *testing.T) {
	for _, test := range []struct {
		input string
		want  string
	}{
		{input: "", want: "auto"},
		{input: " AUTO ", want: "auto"},
		{input: "LIGHT", want: "light"},
		{input: "dark", want: "dark"},
	} {
		got, err := normalizeTerminalTheme(test.input)
		if err != nil || got != test.want {
			t.Fatalf("normalizeTerminalTheme(%q) = %q, %v; want %q", test.input, got, err, test.want)
		}
	}
	if _, err := normalizeTerminalTheme("sepia"); err == nil {
		t.Fatal("normalizeTerminalTheme accepted an unknown theme")
	}
}

func TestNormalizeContextWindow(t *testing.T) {
	for _, test := range []struct {
		input string
		want  int
	}{
		{input: "", want: 0},
		{input: " 262144 ", want: 262144},
	} {
		got, err := normalizeContextWindow(test.input)
		if err != nil || got != test.want {
			t.Fatalf("normalizeContextWindow(%q) = %d, %v; want %d", test.input, got, err, test.want)
		}
	}
	for _, input := range []string{"lots", "0", "-1", "128k"} {
		if _, err := normalizeContextWindow(input); err == nil {
			t.Fatalf("normalizeContextWindow(%q) accepted a value that is not a positive token count", input)
		}
	}
}

func TestNormalizeMaxToolIterations(t *testing.T) {
	for _, test := range []struct {
		input string
		want  int
	}{
		{input: "", want: 0},
		{input: " 40 ", want: 40},
		{input: "unlimited", want: -1},
		{input: " UNLIMITED ", want: -1},
	} {
		got, err := normalizeMaxToolIterations(test.input)
		if err != nil || got != test.want {
			t.Fatalf("normalizeMaxToolIterations(%q) = %d, %v; want %d", test.input, got, err, test.want)
		}
	}
	// -1 is rejected on purpose. Removing the fuse is spelled, so a mistyped
	// number cannot silently mean no limit at all.
	for _, input := range []string{"many", "0", "-1", "12 cycles"} {
		if _, err := normalizeMaxToolIterations(input); err == nil {
			t.Fatalf("normalizeMaxToolIterations(%q) accepted a value that is not a positive cycle count or unlimited", input)
		}
	}
}

func TestNormalizePositiveDuration(t *testing.T) {
	for _, test := range []struct {
		input string
		want  time.Duration
	}{
		{input: "", want: 0},
		{input: " 90s ", want: 90 * time.Second},
		{input: "2m30s", want: 2*time.Minute + 30*time.Second},
	} {
		got, err := normalizePositiveDuration(test.input, "retry budget")
		if err != nil || got != test.want {
			t.Fatalf("normalizePositiveDuration(%q) = %v, %v; want %v", test.input, got, err, test.want)
		}
	}
	// Zero is rejected rather than read as "no limit": Cy asks for unlimited
	// retries, so golem refuses a policy with no budget outright.
	for _, input := range []string{"soon", "0", "0s", "-30s", "90"} {
		if _, err := normalizePositiveDuration(input, "retry budget"); err == nil {
			t.Fatalf("normalizePositiveDuration(%q) accepted a value that is not a positive duration", input)
		}
	}
}

func TestRequestPolicyUsesConfiguredBoundsAndStaysValid(t *testing.T) {
	policy := requestPolicyFor(Config{})
	if policy.RetryBudget != defaultRetryBudget || policy.StreamIdleTimeout != defaultStreamIdleTimeout {
		t.Fatalf("unset config = %+v, want the defaults", policy)
	}
	policy = requestPolicyFor(Config{RetryBudget: 90 * time.Second, StreamIdleTimeout: 20 * time.Second})
	if policy.RetryBudget != 90*time.Second || policy.StreamIdleTimeout != 20*time.Second {
		t.Fatalf("configured policy = %+v", policy)
	}
	// Cy keeps retries unlimited in number, so golem requires the budget and the
	// base delay to stay positive. A configured policy that failed this check
	// would surface as a failure to start a session, not as a bad flag.
	if policy.MaxRetries >= 0 {
		t.Fatalf("MaxRetries = %d, want unlimited", policy.MaxRetries)
	}
	if _, err := golem.NewRequester(golem.RequesterConfig{Model: &runTurnFakeModel{}, Policy: policy}); err != nil {
		t.Fatalf("configured policy rejected by golem: %v", err)
	}
}

func TestBuildModelWithBaseURLDoesNotRequireProviderCredential(t *testing.T) {
	t.Setenv("OPENAI_API_KEY", "")
	cfg := Config{ModelURI: "openai/local-model", BaseURL: "http://127.0.0.1:8011/v1"}
	if _, err := buildModel(cfg, nil, true); err != nil {
		t.Fatalf("buildModel() with a replaced endpoint = %v", err)
	}
}

func TestRunPrintTurnJSONWritesOneFinalResult(t *testing.T) {
	agent := sessionTaggedAgent{agentRunner: printTurnAgent(t), id: "saved-session"}
	var stdout, stderr bytes.Buffer
	if err := runPrintTurn(context.Background(), agent, Config{JSON: true, SaveSession: true}, "read it", &stdout, &stderr); err != nil {
		t.Fatal(err)
	}

	decoder := json.NewDecoder(&stdout)
	var result jsonResult
	if err := decoder.Decode(&result); err != nil {
		t.Fatalf("decode JSON result: %v; output=%q", err, stdout.String())
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		t.Fatalf("JSON output contains more than one value: %v", err)
	}
	if result.Version != jsonResultVersion || result.Reply != "done" || result.FinishReason != llm.FinishReasonStop || result.SessionID != "saved-session" {
		t.Fatalf("JSON result = %#v", result)
	}
	if stderr.Len() != 0 {
		t.Fatalf("stderr = %q", stderr.String())
	}
}

func TestEphemeralJSONDoesNotAdvertiseResumableSession(t *testing.T) {
	agent := sessionTaggedAgent{agentRunner: printTurnAgent(t), id: "temporary-session"}
	var stdout, stderr bytes.Buffer
	if err := runPrintTurn(context.Background(), agent, Config{JSON: true, Ephemeral: true}, "hello", &stdout, &stderr); err != nil {
		t.Fatal(err)
	}
	var result jsonResult
	if err := json.NewDecoder(&stdout).Decode(&result); err != nil {
		t.Fatal(err)
	}
	if result.SessionID != "" {
		t.Fatalf("ephemeral JSON result = %#v", result)
	}
}

type sessionTaggedAgent struct {
	agentRunner
	id string
}

func (a sessionTaggedAgent) SessionID() string { return a.id }

func TestBuildModelRejectsIncompleteURI(t *testing.T) {
	for _, uri := range []string{"ollama", "ollama/", "/model"} {
		if _, err := buildModel(Config{ModelURI: uri}, nil, false); err == nil {
			t.Fatalf("buildModel(%q) succeeded", uri)
		}
	}
}

func TestBuildModelRequiresCredentialWhenSelected(t *testing.T) {
	t.Setenv("DEEPSEEK_API_KEY", "")
	if _, err := buildModel(Config{ModelURI: "deepseek/deepseek-v4-flash"}, nil, true); err == nil {
		t.Fatal("buildModel() accepted a selected model without credentials")
	}
}

func TestReadPipedInputRejectsUnboundedPayload(t *testing.T) {
	file, err := os.CreateTemp(t.TempDir(), "piped-*")
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	if err := file.Truncate(maxPipedInputBytes + 1); err != nil {
		t.Fatal(err)
	}
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		t.Fatal(err)
	}
	if _, piped, err := readPipedInput(file); !piped || err == nil || !strings.Contains(err.Error(), "exceeds") {
		t.Fatalf("readPipedInput() piped=%v err=%v", piped, err)
	}
}

func TestRunPrintTurnWritesOnlyFinalReplyToStdout(t *testing.T) {
	agent := printTurnAgent(t)
	var stdout, stderr bytes.Buffer

	if err := runPrintTurn(context.Background(), agent, Config{}, "read", &stdout, &stderr); err != nil {
		t.Fatalf("runPrintTurn() error = %v", err)
	}
	if stdout.String() != "done\n" {
		t.Fatalf("stdout = %q, want final reply only", stdout.String())
	}
	if stderr.Len() != 0 {
		t.Fatalf("stderr = %q, want no diagnostics", stderr.String())
	}
}

func TestOneShotFencedJSONPrintsOnlyJSONPayload(t *testing.T) {
	model := &runTurnFakeModel{
		streams: [][]llm.StreamChunk{{
			{
				Text:         "```json\n{\"file_count\": 2}\n```",
				FinishReason: llm.FinishReasonStop,
			},
		}},
	}
	agent, err := golem.New(golem.Config{Model: model})
	if err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	if err := runPrintTurn(context.Background(), agent, Config{PrintMode: true}, "count files", &stdout, &stderr); err != nil {
		t.Fatal(err)
	}
	if got, want := stdout.String(), "{\"file_count\": 2}\n"; got != want {
		t.Fatalf("stdout = %q, want %q", got, want)
	}
	if stderr.Len() != 0 {
		t.Fatalf("stderr = %q", stderr.String())
	}
}

func TestRunPrintTurnVerboseKeepsProgressOutsideJSON(t *testing.T) {
	agent := printTurnAgent(t)
	var stdout, stderr bytes.Buffer

	if err := runPrintTurn(context.Background(), agent, Config{Verbose: true, JSON: true}, "read", &stdout, &stderr); err != nil {
		t.Fatalf("runPrintTurn() error = %v", err)
	}
	var result jsonResult
	if err := json.Unmarshal(stdout.Bytes(), &result); err != nil || result.Reply != "done" {
		t.Fatalf("JSON result = %#v, error = %v", result, err)
	}
	if !strings.Contains(stderr.String(), "read  README.md") {
		t.Fatalf("stderr missed tool progress: %q", stderr.String())
	}
	if strings.Contains(stdout.String(), "read  README.md") {
		t.Fatalf("stdout contains diagnostics: %q", stdout.String())
	}
}

func TestResumeHintVisibility(t *testing.T) {
	tests := []struct {
		name        string
		cfg         Config
		hasUserTurn bool
		want        bool
	}{
		{name: "empty interactive", cfg: Config{}, want: false},
		{name: "interactive", cfg: Config{}, hasUserTurn: true, want: true},
		{name: "quiet one-shot", cfg: Config{PrintMode: true}, hasUserTurn: true, want: false},
		{name: "verbose one-shot", cfg: Config{PrintMode: true, Verbose: true}, hasUserTurn: true, want: false},
		{name: "saved one-shot", cfg: Config{PrintMode: true, SaveSession: true}, hasUserTurn: true, want: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := shouldPrintResumeHint(test.cfg, test.hasUserTurn); got != test.want {
				t.Fatalf("shouldPrintResumeHint() = %v, want %v", got, test.want)
			}
		})
	}
}

func TestOneShotSessionPersistence(t *testing.T) {
	if !useEphemeralSession(Config{PrintMode: true}, "") {
		t.Fatal("ordinary one-shot should be ephemeral")
	}
	if useEphemeralSession(Config{PrintMode: true, SaveSession: true}, "") {
		t.Fatal("--save-session one-shot should be durable")
	}
	if useEphemeralSession(Config{PrintMode: true}, "01234567") {
		t.Fatal("resumed one-shot should keep its existing session")
	}
	if useEphemeralSession(Config{}, "") {
		t.Fatal("interactive mode should be durable")
	}
}

func printTurnAgent(t *testing.T) agentRunner {
	t.Helper()
	model := &runTurnFakeModel{
		streams: [][]llm.StreamChunk{
			{{
				ToolCalls: []llm.ToolCall{{
					ID:   "call_1",
					Type: string(llm.ToolTypeFunction),
					Function: llm.ToolFunction{
						Name:      "read",
						Arguments: `{"path":"README.md"}`,
					},
				}},
				FinishReason: llm.FinishReasonToolUse,
			}},
			{{Text: "done", FinishReason: llm.FinishReasonStop}},
		},
	}
	tool := golem.FunctionTool(
		"read",
		"Read a file",
		jsonschema.Object(map[string]jsonschema.Schema{"path": jsonschema.String("Path")}, "path"),
		func(context.Context, llm.ToolCall) (golem.ToolResult, error) {
			return golem.ToolResult{Content: "README contents"}, nil
		},
	)
	agent, err := golem.New(golem.Config{Model: model, Tools: []golem.Tool{tool}})
	if err != nil {
		t.Fatalf("golem.New() error = %v", err)
	}
	return agent
}

func TestPrintCompactToolEventShowsCallsAndErrorsOnly(t *testing.T) {
	var out bytes.Buffer

	console := ui.NewConsole(&out)
	console.PrintCompactToolEvent(golem.StreamEvent{Kind: golem.EventToolCall, Step: golem.Step{ToolName: "bash", Arguments: `{"command":"go test ./cy/..."}`}})
	console.PrintCompactToolEvent(golem.StreamEvent{Kind: golem.EventToolResult, Step: golem.Step{ToolName: "bash"}})
	console.PrintCompactToolEvent(golem.StreamEvent{Kind: golem.EventToolError, Step: golem.Step{ToolName: "grep", Error: "invalid pattern"}})

	want := "\n$ go test ./cy/...\n\nerror: invalid pattern\n"
	if out.String() != want {
		t.Fatalf("output = %q, want %q", out.String(), want)
	}
}

func TestConsolePrintsStructuredFileDiff(t *testing.T) {
	var out bytes.Buffer
	console := ui.NewConsole(&out)
	change := toolruntime.BuildFileChangeMeta("note.txt", "edited", []byte("one\nold\n"), []byte("one\nnew\n"))

	console.PrintCompactToolEvent(golem.StreamEvent{Kind: golem.EventToolCall, Step: golem.Step{ToolName: "edit", ToolCallID: "call-1", Arguments: `{"path":"note.txt"}`}})
	console.PrintCompactToolEvent(golem.StreamEvent{Kind: golem.EventToolResult, Step: golem.Step{ToolName: "edit", ToolCallID: "call-1", Meta: change}})

	got := out.String()
	for _, want := range []string{"edit  note.txt", "edited  note.txt  +1 −1", "− 2   │ old", "+   2 │ new"} {
		if !strings.Contains(got, want) {
			t.Fatalf("console diff missed %q: %q", want, got)
		}
	}
}

type runTurnFakeModel struct {
	streams  [][]llm.StreamChunk
	requests []llm.Request
}

func (m *runTurnFakeModel) Chat(context.Context, llm.Request) (*llm.Response, error) {
	return nil, nil
}

func (m *runTurnFakeModel) Stream(_ context.Context, request llm.Request) (llm.Stream, error) {
	request.Messages = llm.CloneMessages(request.Messages)
	m.requests = append(m.requests, request)
	if len(m.streams) == 0 {
		return &runTurnFakeStream{}, nil
	}
	chunks := m.streams[0]
	m.streams = m.streams[1:]
	return &runTurnFakeStream{chunks: chunks}, nil
}

type runTurnFakeStream struct {
	chunks []llm.StreamChunk
	idx    int
}

func (s *runTurnFakeStream) Recv() (llm.StreamChunk, error) {
	if s.idx >= len(s.chunks) {
		return llm.StreamChunk{}, io.EOF
	}
	chunk := s.chunks[s.idx]
	s.idx++
	return chunk, nil
}

func (s *runTurnFakeStream) Usage() llm.Usage {
	return llm.Usage{}
}

func (s *runTurnFakeStream) Close() error {
	return nil
}
