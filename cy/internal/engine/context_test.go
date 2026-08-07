package engine

import (
	"context"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/levmv/golems/cy/internal/session"
	"github.com/levmv/golems/pkg/llm"
)

func TestManualCompactionIsAdditiveAndRebuildsContext(t *testing.T) {
	s := contextSession(t)
	appendCompletedTurn(t, s, "run-1", "old question", "old answer")
	appendCompletedTurn(t, s, "run-2", "recent question", "recent answer")
	model := &scriptedModel{chatResponses: []*llm.Response{{
		Content: "Objective: continue the recent work. Old decision: keep the journal additive.",
		Usage:   llm.Usage{PromptTokens: 30, CompletionTokens: 12, TotalTokens: 42},
	}}}
	eng, err := New(Config{Model: model, Session: s, ModelURI: "test/model", ContextWindow: 32 * 1024})
	if err != nil {
		t.Fatal(err)
	}
	report, _, err := eng.Compact(context.Background(), "retain decisions")
	if err != nil {
		t.Fatal(err)
	}
	if report.CompactionCount != 1 || report.SummaryTokens == 0 {
		t.Fatalf("context report = %#v", report)
	}
	state, err := s.Replay()
	if err != nil {
		t.Fatal(err)
	}
	if len(state.Messages) != 4 {
		t.Fatalf("compaction deleted original messages: %#v", state.Messages)
	}
	if state.Compaction == nil || state.Compaction.FirstVerbatimSeq == 0 || !strings.Contains(state.Compaction.Summary, "journal additive") {
		t.Fatalf("compaction state = %#v", state.Compaction)
	}
	messages, _ := eng.buildContext(state)
	joined := messageContents(messages)
	if strings.Contains(joined, "old question") || !strings.Contains(joined, "Conversation summary") || !strings.Contains(joined, "recent question") {
		t.Fatalf("rebuilt context = %q", joined)
	}
}

func TestAutoCompactionRunsAtSafeRequestBoundary(t *testing.T) {
	s := contextSession(t)
	appendCompletedTurn(t, s, "run-1", strings.Repeat("old context ", 3000), "old answer")
	appendCompletedTurn(t, s, "run-2", "recent question", "recent answer")
	model := &scriptedModel{chatResponses: []*llm.Response{
		{Content: "compact summary", Usage: llm.Usage{TotalTokens: 10}},
		{Content: "final answer", FinishReason: llm.FinishReasonStop},
	}}
	eng, err := New(Config{Model: model, Session: s, ModelURI: "unknown/model", ContextWindow: 12 * 1024})
	if err != nil {
		t.Fatal(err)
	}
	turn, err := eng.Stream(context.Background(), "new question", nil)
	if err != nil {
		t.Fatal(err)
	}
	if turn.Reply != "final answer" || len(model.requests) != 2 {
		t.Fatalf("turn=%#v requests=%d", turn, len(model.requests))
	}
	if len(model.requests[0].Tools) != 0 || !strings.Contains(model.requests[0].Messages[0].Content, "Summarize an older segment") {
		t.Fatalf("first request is not the summarizer: %#v", model.requests[0])
	}
	if joined := messageContents(model.requests[1].Messages); strings.Contains(joined, "old context") || !strings.Contains(joined, "compact summary") || !strings.Contains(joined, "new question") {
		t.Fatalf("post-compaction request = %q", joined)
	}
	state, err := s.Replay()
	if err != nil {
		t.Fatal(err)
	}
	if state.CompactionCount != 1 {
		t.Fatalf("compaction count = %d", state.CompactionCount)
	}
}

func TestAutoPruningAvoidsCompactionWhenOldToolResultsAreSufficient(t *testing.T) {
	s := contextSession(t)
	largeResult := "BEGIN\n" + strings.Repeat("tool-output ", 16*1024) + "\nEND"
	appendCompletedToolTurn(t, s, "run-1", "inspect the large output", largeResult)
	appendCompletedTurn(t, s, "run-2", "recent question", "recent answer")
	model := &scriptedModel{chatResponses: []*llm.Response{{Content: "final answer", FinishReason: llm.FinishReasonStop}}}
	eng, err := New(Config{Model: model, Session: s, ModelURI: "unknown/model", ContextWindow: 32 * 1024})
	if err != nil {
		t.Fatal(err)
	}
	turn, err := eng.Stream(context.Background(), "new question", nil)
	if err != nil {
		t.Fatal(err)
	}
	if turn.Reply != "final answer" || len(model.requests) != 1 {
		t.Fatalf("turn=%#v requests=%d", turn, len(model.requests))
	}

	var providerToolResult string
	for _, message := range model.requests[0].Messages {
		if message.Role == llm.RoleTool {
			providerToolResult = message.Content
			break
		}
	}
	if !strings.HasPrefix(providerToolResult, "BEGIN\n") || !strings.HasSuffix(providerToolResult, "\nEND") || !strings.Contains(providerToolResult, "bytes omitted from old tool result") {
		t.Fatalf("provider tool result was not stable head/tail pruning: %q", providerToolResult)
	}

	state, err := s.Replay()
	if err != nil {
		t.Fatal(err)
	}
	if state.ToolPruning == nil || state.Compaction != nil {
		t.Fatalf("maintenance state: pruning=%#v compaction=%#v", state.ToolPruning, state.Compaction)
	}
	for _, message := range state.Messages {
		if message.Role == llm.RoleTool && message.Content != largeResult {
			t.Fatal("raw tool result was changed in the journal")
		}
	}
}

func TestAutoPruningFallsThroughToCompactionWhenInsufficient(t *testing.T) {
	s := contextSession(t)
	appendCompletedToolTurn(t, s, "run-1", strings.Repeat("large user context ", 8*1024), strings.Repeat("large tool output ", 8*1024))
	appendCompletedTurn(t, s, "run-2", "recent question", "recent answer")
	model := &scriptedModel{chatResponses: []*llm.Response{
		{Content: "compact summary", Usage: llm.Usage{TotalTokens: 10}},
		{Content: "final answer", FinishReason: llm.FinishReasonStop},
	}}
	eng, err := New(Config{Model: model, Session: s, ModelURI: "unknown/model", ContextWindow: 12 * 1024})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := eng.Stream(context.Background(), "new question", nil); err != nil {
		t.Fatal(err)
	}
	state, err := s.Replay()
	if err != nil {
		t.Fatal(err)
	}
	if state.ToolPruning != nil || state.CompactionCount != 1 {
		t.Fatalf("maintenance state: pruning=%#v compactions=%d", state.ToolPruning, state.CompactionCount)
	}
}

func TestPruneToolResultKeepsUTF8HeadAndTail(t *testing.T) {
	content := "начало-" + strings.Repeat("界", 200) + "-конец"
	got := pruneToolResult(content, 11, 10)
	if !strings.HasPrefix(got, "начал") || !strings.HasSuffix(got, "конец") || !strings.Contains(got, "bytes omitted") {
		t.Fatalf("pruned content = %q", got)
	}
	if !utf8.ValidString(got) {
		t.Fatalf("pruned content is invalid UTF-8: %q", got)
	}
}

func TestPruneToolResultDoesNotExpandSmallSavings(t *testing.T) {
	content := strings.Repeat("x", 101)
	if got := pruneToolResult(content, 50, 50); got != content {
		t.Fatalf("small result expanded to %d bytes", len(got))
	}
}

func TestContextReportIncludesPendingInputAndConservativeLimit(t *testing.T) {
	s := contextSession(t)
	model := &scriptedModel{}
	eng, err := New(Config{Model: model, Session: s, ModelURI: "unknown/model", ContextWindow: 32 * 1024})
	if err != nil {
		t.Fatal(err)
	}
	if err := eng.QueueInput("remember this follow-up"); err != nil {
		t.Fatal(err)
	}
	report, err := eng.ContextReport()
	if err != nil {
		t.Fatal(err)
	}
	if report.InputLimit != 24*1024 || report.PendingTokens == 0 || report.PercentLeft <= 0 || report.PercentLeft > 100 {
		t.Fatalf("report = %#v", report)
	}
}

func TestCompactionKeepsOverflowingNewerTurnsVerbatim(t *testing.T) {
	oldQuestion := "old-question " + strings.Repeat("q", 20*1024)
	oldAnswer := "old-answer " + strings.Repeat("a", 20*1024)
	recentQuestion := "recent-question " + strings.Repeat("q", 20*1024)
	recentAnswer := "recent-answer " + strings.Repeat("a", 20*1024)
	state := session.State{
		Messages: []llm.Message{
			{Role: llm.RoleUser, Content: oldQuestion},
			{Role: llm.RoleAI, Content: oldAnswer},
			{Role: llm.RoleUser, Content: recentQuestion},
			{Role: llm.RoleAI, Content: recentAnswer},
		},
		MessageSeqs: []uint64{1, 2, 3, 4},
	}
	eng := &Engine{contextWindow: 1}
	prompt, compactedEnd := eng.compactionPrompt(state, 0, len(state.Messages), "")
	if compactedEnd != 2 {
		t.Fatalf("compacted end = %d, want complete first turn boundary 2", compactedEnd)
	}
	if !strings.Contains(prompt, "old-question") || strings.Contains(prompt, "recent-question") {
		t.Fatalf("bounded compaction prompt selected wrong turns")
	}
	if !strings.Contains(prompt, "newer records left verbatim") {
		t.Fatalf("bounded compaction prompt omitted boundary note: %q", prompt)
	}
}

func TestCompactionPromptIncludesToolCallNameAndArguments(t *testing.T) {
	state := session.State{
		Messages: []llm.Message{
			{Role: llm.RoleUser, Content: "run the tests"},
			{Role: llm.RoleAI, ToolCalls: []llm.ToolCall{{
				Function: llm.ToolFunction{Name: "bash", Arguments: `{"command":"go test ./cy/...","workdir":"."}`},
			}}},
			{Role: llm.RoleTool, Content: "status: completed\nexit_code: 0\n\nok"},
		},
		MessageSeqs: []uint64{1, 2, 3},
	}
	eng := &Engine{contextWindow: 32 * 1024}
	prompt, compactedEnd := eng.compactionPrompt(state, 0, len(state.Messages), "")
	if compactedEnd != len(state.Messages) {
		t.Fatalf("compacted end = %d", compactedEnd)
	}
	if !strings.Contains(prompt, `tool_call: bash args={"command":"go test ./cy/...","workdir":"."}`) {
		t.Fatalf("compaction prompt omitted tool call: %q", prompt)
	}
}

func TestConfiguredCompactionPromptIsSentAndRecorded(t *testing.T) {
	s := contextSession(t)
	appendCompletedTurn(t, s, "run-1", "old question", "old answer")
	appendCompletedTurn(t, s, "run-2", "recent question", "recent answer")
	model := &scriptedModel{chatResponses: []*llm.Response{{Content: "one line summary"}}}
	const instead = "Summarize the transcript in one line."
	eng, err := New(Config{Model: model, Session: s, ContextWindow: 32 * 1024, CompactionPrompt: instead})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := eng.Compact(context.Background(), ""); err != nil {
		t.Fatal(err)
	}
	if len(model.requests) != 1 {
		t.Fatalf("compaction requests = %d, want 1", len(model.requests))
	}
	if system := model.requests[0].Messages[0]; system.Role != llm.RoleSystem || system.Content != instead {
		t.Fatalf("compaction system message = %#v, want the configured prompt", system)
	}
	configured := configurations(t, s)
	if got := configured[len(configured)-1].CompactionPrompt; got != instead {
		t.Fatalf("recorded compaction prompt = %q, want the configured one", got)
	}
}

func TestCompactionRejectsTruncatedSummary(t *testing.T) {
	s := contextSession(t)
	appendCompletedTurn(t, s, "run-1", "old question", "old answer")
	appendCompletedTurn(t, s, "run-2", "recent question", "recent answer")
	model := &scriptedModel{chatResponses: []*llm.Response{{
		Content:      "partial summary",
		FinishReason: llm.FinishReasonLength,
	}}}
	eng, err := New(Config{Model: model, Session: s, ContextWindow: 32 * 1024})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := eng.Compact(context.Background(), ""); err == nil || !strings.Contains(err.Error(), "incomplete summary") {
		t.Fatalf("Compact() error = %v", err)
	}
	state, err := s.Replay()
	if err != nil {
		t.Fatal(err)
	}
	if state.Compaction != nil {
		t.Fatalf("truncated summary advanced compaction boundary: %#v", state.Compaction)
	}
}

func contextSession(t *testing.T) *session.Session {
	t.Helper()
	s, err := session.Create(session.CreateOptions{Home: t.TempDir(), Workspace: "/workspace"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func appendCompletedTurn(t *testing.T, s *session.Session, runID, input, output string) {
	t.Helper()
	if _, err := s.Append(session.RecordUserMessage, session.UserMessage{RunID: runID, Content: input}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Append(session.RecordAssistantMessage, session.AssistantMessage{RunID: runID, Content: output}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Append(session.RecordRunFinished, session.RunFinished{RunID: runID}); err != nil {
		t.Fatal(err)
	}
}

func appendCompletedToolTurn(t *testing.T, s *session.Session, runID, input, toolOutput string) {
	t.Helper()
	call := llm.ToolCall{ID: runID + "-call", Type: string(llm.ToolTypeFunction), Function: llm.ToolFunction{Name: "read", Arguments: `{}`}}
	if _, err := s.Append(session.RecordUserMessage, session.UserMessage{RunID: runID, Content: input}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Append(session.RecordAssistantMessage, session.AssistantMessage{RunID: runID, ToolCalls: []llm.ToolCall{call}}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Append(session.RecordToolResult, session.ToolResult{RunID: runID, ToolCallID: call.ID, Content: toolOutput}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Append(session.RecordAssistantMessage, session.AssistantMessage{RunID: runID, Content: "tool inspected"}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Append(session.RecordRunFinished, session.RunFinished{RunID: runID}); err != nil {
		t.Fatal(err)
	}
}

func messageContents(messages []llm.Message) string {
	var parts []string
	for _, message := range messages {
		parts = append(parts, message.Content)
	}
	return strings.Join(parts, "\n")
}
