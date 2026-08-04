package session

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/levmv/golems/pkg/llm"
)

func TestSessionPersistsConversationAndRepairsPartialTail(t *testing.T) {
	home := t.TempDir()
	s, err := Create(CreateOptions{Home: home, Workspace: "/workspace/project", Model: "deepseek/deepseek-v4-flash", ReasoningEffort: "high"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Append(RecordModelChanged, ModelChanged{Model: "deepseek/deepseek-v4-pro"}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Append(RecordUserMessage, UserMessage{RunID: "run-1", Content: "hello"}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Append(RecordAssistantMessage, AssistantMessage{
		RunID: "run-1", Content: "hi",
		Usage: llm.Usage{PromptTokens: 2, CompletionTokens: 1, TotalTokens: 3},
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Append(RecordRunFinished, RunFinished{RunID: "run-1"}); err != nil {
		t.Fatal(err)
	}
	id, journalPath := s.ID(), s.journalPath
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	file, err := os.OpenFile(journalPath, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := file.WriteString(`{"seq":5,"type":"partial`); err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}

	resumed, err := Open(home, id)
	if err != nil {
		t.Fatal(err)
	}
	defer resumed.Close()
	if !resumed.TailRepaired() {
		t.Fatal("partial journal tail was repaired without recording the notice")
	}
	if _, err := resumed.Append(RecordUserMessage, UserMessage{RunID: "run-2", Content: "continue"}); err != nil {
		t.Fatalf("append after repair: %v", err)
	}
	state, err := resumed.Replay()
	if err != nil {
		t.Fatal(err)
	}
	if state.Header.Workspace != "/workspace/project" || state.Header.ReasoningEffort != "high" || state.Model != "deepseek/deepseek-v4-pro" || state.ReasoningEffort != "" {
		t.Fatalf("state header/model = %#v / %q", state.Header, state.Model)
	}
	if len(state.Messages) != 3 || state.Messages[0].Content != "hello" || state.Messages[2].Content != "continue" {
		t.Fatalf("messages = %#v", state.Messages)
	}
	if state.Usage.TotalTokens != 3 || len(state.ActiveRuns) != 1 {
		t.Fatalf("usage/active runs = %#v / %#v", state.Usage, state.ActiveRuns)
	}
	assertMode(t, home, 0o700)
	assertMode(t, resumed.dir, 0o700)
	assertMode(t, journalPath, 0o600)
}

func TestOpenUsesExclusiveLockAndUniquePrefix(t *testing.T) {
	home := t.TempDir()
	s, err := Create(CreateOptions{Home: home, Workspace: "/workspace"})
	if err != nil {
		t.Fatal(err)
	}
	id := s.ID()
	if _, err := Open(home, id[:12]); !errors.Is(err, ErrSessionLocked) {
		t.Fatalf("open locked session: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	resumed, err := Open(home, id[:12])
	if err != nil {
		t.Fatal(err)
	}
	defer resumed.Close()
	if resumed.ID() != id {
		t.Fatalf("resumed ID = %q, want %q", resumed.ID(), id)
	}
}

func TestSessionReplaysLatestToolPruningBoundary(t *testing.T) {
	s, err := Create(CreateOptions{Home: t.TempDir(), Workspace: "/workspace"})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if _, err := s.Append(RecordUserMessage, UserMessage{RunID: "run-1", Content: "first"}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Append(RecordToolResultsPruned, ToolResultsPruned{ThroughSeq: 2, HeadBytes: 4096, TailBytes: 4096}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Append(RecordUserMessage, UserMessage{RunID: "run-2", Content: "second"}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Append(RecordToolResultsPruned, ToolResultsPruned{ThroughSeq: 4, HeadBytes: 4096, TailBytes: 4096}); err != nil {
		t.Fatal(err)
	}
	state, err := s.Replay()
	if err != nil {
		t.Fatal(err)
	}
	if state.ToolPruning == nil || state.ToolPruning.ThroughSeq != 4 {
		t.Fatalf("tool pruning = %#v", state.ToolPruning)
	}
	state.ToolPruning.ThroughSeq = 1
	again, err := s.Replay()
	if err != nil || again.ToolPruning == nil || again.ToolPruning.ThroughSeq != 4 {
		t.Fatalf("cached replay was mutated: %#v, %v", again.ToolPruning, err)
	}
}

func TestReplayCacheIsIsolatedAndUpdatedByAppend(t *testing.T) {
	s, err := Create(CreateOptions{Home: t.TempDir(), Workspace: "/workspace"})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if _, err := s.Append(RecordUserMessage, UserMessage{RunID: "run", Content: "hello"}); err != nil {
		t.Fatal(err)
	}

	first, err := s.Replay()
	if err != nil {
		t.Fatal(err)
	}
	first.Messages[0].Content = "mutated"
	delete(first.ActiveRuns, "run")
	second, err := s.Replay()
	if err != nil {
		t.Fatal(err)
	}
	if second.Messages[0].Content != "hello" {
		t.Fatalf("cached replay was mutated through its result: %#v", second.Messages)
	}
	if _, active := second.ActiveRuns["run"]; !active {
		t.Fatalf("cached active runs were mutated through their result: %#v", second.ActiveRuns)
	}

	if _, err := s.Append(RecordAssistantMessage, AssistantMessage{
		RunID: "run", Content: "done", Usage: llm.Usage{TotalTokens: 7},
	}); err != nil {
		t.Fatal(err)
	}
	if !s.replayValid {
		t.Fatal("append invalidated a valid replay cache")
	}
	updated, err := s.Replay()
	if err != nil {
		t.Fatal(err)
	}
	if len(updated.Messages) != 2 || updated.Messages[1].Content != "done" || updated.Usage.TotalTokens != 7 {
		t.Fatalf("replay after append = %#v", updated)
	}
}

func TestListReturnsCurrentWorkspaceWithPromptTitle(t *testing.T) {
	home := t.TempDir()
	other, err := Create(CreateOptions{Home: home, Workspace: "/other", Model: "fake/old"})
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()
	empty, err := Create(CreateOptions{Home: home, Workspace: "/current", Model: "fake/empty"})
	if err != nil {
		t.Fatal(err)
	}
	defer empty.Close()
	current, err := Create(CreateOptions{Home: home, Workspace: "/current", Model: "fake/current"})
	if err != nil {
		t.Fatal(err)
	}
	defer current.Close()
	if _, err := current.Append(RecordUserMessage, UserMessage{RunID: "run", Content: "\n Fix background jobs\nwith details"}); err != nil {
		t.Fatal(err)
	}
	summaries, err := List(home, "/current")
	if err != nil {
		t.Fatal(err)
	}
	if len(summaries) != 1 || summaries[0].ID != current.ID() || summaries[0].Title != "Fix background jobs" {
		t.Fatalf("summaries = %#v", summaries)
	}
}

func TestOpenMarksInterruptedToolOutcomeUnknown(t *testing.T) {
	home := t.TempDir()
	s, err := Create(CreateOptions{Home: home, Workspace: "/workspace"})
	if err != nil {
		t.Fatal(err)
	}
	call := llm.ToolCall{ID: "call-1", Function: llm.ToolFunction{Name: "bash", Arguments: `{"command":"do-something"}`}}
	if _, err := s.Append(RecordUserMessage, UserMessage{RunID: "run-1", Content: "run it"}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Append(RecordAssistantMessage, AssistantMessage{RunID: "run-1", ToolCalls: []llm.ToolCall{call}}); err != nil {
		t.Fatal(err)
	}
	id := s.ID()
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	resumed, err := Open(home, id)
	if err != nil {
		t.Fatal(err)
	}
	defer resumed.Close()
	state, err := resumed.Replay()
	if err != nil {
		t.Fatal(err)
	}
	if len(state.PendingTools) != 0 || len(state.ActiveRuns) != 0 || len(state.Messages) != 3 {
		t.Fatalf("reconciled state = %#v", state)
	}
	tool := state.Messages[2]
	if tool.Role != llm.RoleTool || tool.ToolCallID != call.ID || !strings.Contains(tool.Content, "unknown") {
		t.Fatalf("tool result = %#v", tool)
	}
}

func TestReconciledRunIsMarkedInterrupted(t *testing.T) {
	home := t.TempDir()
	s, err := Create(CreateOptions{Home: home, Workspace: "/workspace"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Append(RecordUserMessage, UserMessage{RunID: "run-1", Content: "run it"}); err != nil {
		t.Fatal(err)
	}
	id := s.ID()
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	resumed, err := Open(home, id)
	if err != nil {
		t.Fatal(err)
	}
	defer resumed.Close()
	records, err := resumed.Records()
	if err != nil {
		t.Fatal(err)
	}
	last := records[len(records)-1]
	if last.Type != RecordRunFinished {
		t.Fatalf("last record = %q, want run_finished", last.Type)
	}
	finished, err := DecodePayload[RunFinished](last)
	if err != nil {
		t.Fatal(err)
	}
	if finished.RunID != "run-1" || finished.Outcome != RunInterrupted {
		t.Fatalf("run_finished = %#v, want run-1 interrupted", finished)
	}
	if finished.Error == "" {
		t.Fatal("an interrupted run should say what happened to it")
	}
}

// A journal written before Cy recorded outcomes still replays. The outcome is
// absent rather than assumed, since those runs may have ended either way.
func TestReplayAcceptsRunFinishedWithoutOutcome(t *testing.T) {
	s, err := Create(CreateOptions{Home: t.TempDir(), Workspace: "/workspace"})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if _, err := s.Append(RecordUserMessage, UserMessage{RunID: "run-1", Content: "hello"}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Append(RecordRunFinished, map[string]string{"run_id": "run-1"}); err != nil {
		t.Fatal(err)
	}
	state, err := s.Replay()
	if err != nil {
		t.Fatalf("Replay() error = %v", err)
	}
	if len(state.ActiveRuns) != 0 {
		t.Fatalf("active runs = %#v, want the run closed", state.ActiveRuns)
	}
	records, err := s.Records()
	if err != nil {
		t.Fatal(err)
	}
	finished, err := DecodePayload[RunFinished](records[len(records)-1])
	if err != nil {
		t.Fatal(err)
	}
	if finished.Outcome != "" {
		t.Fatalf("outcome = %q, want it left unset", finished.Outcome)
	}
}

func TestClosePruningEmptyKeepsUsedSession(t *testing.T) {
	home := t.TempDir()
	empty, err := Create(CreateOptions{Home: home, Workspace: "/workspace"})
	if err != nil {
		t.Fatal(err)
	}
	emptyDir := empty.dir
	if err := empty.ClosePruningEmpty(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(emptyDir); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("empty session still exists: %v", err)
	}

	used, err := Create(CreateOptions{Home: home, Workspace: "/workspace"})
	if err != nil {
		t.Fatal(err)
	}
	usedDir := used.dir
	if _, err := used.Append(RecordUserMessage, UserMessage{RunID: "run", Content: "hello"}); err != nil {
		t.Fatal(err)
	}
	if err := used.ClosePruningEmpty(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(usedDir); err != nil {
		t.Fatalf("used session was removed: %v", err)
	}
}

func TestDefaultHomeUsesCYHome(t *testing.T) {
	want, err := filepath.Abs(filepath.Join(t.TempDir(), "state"))
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("CY_HOME", want)
	got, err := DefaultHome()
	if err != nil || got != want {
		t.Fatalf("DefaultHome() = %q, %v; want %q", got, err, want)
	}
}

func TestDefaultHomeUsesDotCY(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("CY_HOME", "")
	got, err := DefaultHome()
	want := filepath.Join(home, ".cy")
	if err != nil || got != want {
		t.Fatalf("DefaultHome() = %q, %v; want %q", got, err, want)
	}
}

func assertMode(t *testing.T, path string, want os.FileMode) {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != want {
		t.Fatalf("mode %s = %#o, want %#o", path, got, want)
	}
}
