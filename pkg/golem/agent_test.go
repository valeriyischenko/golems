package golem

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/levmv/golems/pkg/jsonschema"
	"github.com/levmv/golems/pkg/llm"
)

func TestRunTurnRunsSiblingReadToolsConcurrentlyInSourceOrder(t *testing.T) {
	calls := []llm.ToolCall{
		{ID: "call-1", Function: llm.ToolFunction{Name: "read", Arguments: `{"path":"one"}`}},
		{ID: "call-2", Function: llm.ToolFunction{Name: "read", Arguments: `{"path":"two"}`}},
	}
	model := &fakeModel{chatResponses: []*llm.Response{
		{ToolCalls: calls, FinishReason: llm.FinishReasonToolUse},
		{Content: "done", FinishReason: llm.FinishReasonStop},
	}}
	started := make(chan string, 2)
	release := make(chan struct{})
	tool := FunctionToolWithEffect(ToolEffectRead, "read", "read", jsonschema.Object(nil), func(_ context.Context, call llm.ToolCall) (ToolResult, error) {
		started <- call.ID
		<-release
		return ToolResult{Content: call.ID + " result"}, nil
	})
	tools, err := NewToolSet([]Tool{tool})
	if err != nil {
		t.Fatal(err)
	}
	type replyResult struct {
		turn *Turn
		err  error
	}
	done := make(chan replyResult, 1)
	go func() {
		turn, err := RunTurn(context.Background(), TurnConfig{Model: model, Tools: tools, Input: "read both"})
		done <- replyResult{turn: turn, err: err}
	}()
	for i := 0; i < 2; i++ {
		select {
		case <-started:
		case <-time.After(time.Second):
			t.Fatal("sibling read calls did not start concurrently")
		}
	}
	close(release)
	result := <-done
	if result.err != nil {
		t.Fatal(result.err)
	}
	if len(result.turn.Steps) != 4 || result.turn.Steps[0].ToolCallID != "call-1" || result.turn.Steps[1].ToolCallID != "call-1" || result.turn.Steps[2].ToolCallID != "call-2" || result.turn.Steps[3].ToolCallID != "call-2" {
		t.Fatalf("steps are not in source order: %#v", result.turn.Steps)
	}
}

type fakeModel struct {
	chatResponses []*llm.Response
	stream        *fakeStream
	streams       []*fakeStream
	streamErr     error
	requests      []llm.Request
}

func (m *fakeModel) Chat(_ context.Context, req llm.Request) (*llm.Response, error) {
	m.requests = append(m.requests, req)
	if len(m.chatResponses) == 0 {
		return &llm.Response{}, nil
	}
	resp := m.chatResponses[0]
	m.chatResponses = m.chatResponses[1:]
	return resp, nil
}

func (m *fakeModel) Stream(_ context.Context, req llm.Request) (llm.Stream, error) {
	m.requests = append(m.requests, req)
	if m.streamErr != nil {
		return nil, m.streamErr
	}
	if len(m.streams) > 0 {
		stream := m.streams[0]
		m.streams = m.streams[1:]
		return stream, nil
	}
	if m.stream == nil {
		return nil, nil
	}
	return m.stream, nil
}

type fakeStream struct {
	chunks []llm.StreamChunk
	usage  llm.Usage
	idx    int
	closed bool
}

func TestAgentAcceptsRequestFunctionWithoutModel(t *testing.T) {
	agent, err := New(Config{Request: func(_ context.Context, _ int, _ llm.Request, _ bool, _ StreamFunc) (*llm.Response, error) {
		return &llm.Response{Content: "from requester", FinishReason: llm.FinishReasonStop}, nil
	}})
	if err != nil {
		t.Fatal(err)
	}
	turn, err := agent.Reply(context.Background(), "hello")
	if err != nil {
		t.Fatal(err)
	}
	if turn.Reply != "from requester" {
		t.Fatalf("reply = %q", turn.Reply)
	}
}

func (s *fakeStream) Recv() (llm.StreamChunk, error) {
	if s.idx >= len(s.chunks) {
		return llm.StreamChunk{}, io.EOF
	}
	chunk := s.chunks[s.idx]
	s.idx++
	return chunk, nil
}

func (s *fakeStream) Usage() llm.Usage {
	return s.usage
}

func (s *fakeStream) Close() error {
	s.closed = true
	return nil
}

func TestReplyStoresHistoryAndUsage(t *testing.T) {
	model := &fakeModel{
		chatResponses: []*llm.Response{{
			Content:      " hello ",
			FinishReason: llm.FinishReasonStop,
			Usage: llm.Usage{
				PromptTokens:     5,
				CompletionTokens: 2,
				TotalTokens:      7,
			},
		}},
	}

	agent, err := New(Config{
		Model:        model,
		SystemPrompt: "system",
	})
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}

	turn, err := agent.Reply(context.Background(), " hi ")
	if err != nil {
		t.Fatalf("Reply() error = %v", err)
	}
	if turn.Reply != "hello" {
		t.Fatalf("Reply() reply = %q, want %q", turn.Reply, "hello")
	}

	if len(model.requests) != 1 {
		t.Fatalf("requests len = %d, want 1", len(model.requests))
	}
	reqMessages := model.requests[0].Messages
	if len(reqMessages) != 2 {
		t.Fatalf("request messages len = %d, want 2", len(reqMessages))
	}
	if reqMessages[0].Role != llm.RoleSystem || reqMessages[0].Content != "system" {
		t.Fatalf("system message = %#v", reqMessages[0])
	}
	if reqMessages[1].Role != llm.RoleUser || reqMessages[1].Content != "hi" {
		t.Fatalf("user message = %#v", reqMessages[1])
	}

	history := agent.History()
	if len(history) != 2 {
		t.Fatalf("history len = %d, want 2", len(history))
	}
	if history[0].Role != llm.RoleUser || history[0].Content != "hi" {
		t.Fatalf("history user = %#v", history[0])
	}
	if history[1].Role != llm.RoleAI || history[1].Content != "hello" {
		t.Fatalf("history assistant = %#v", history[1])
	}

	usage := agent.Usage()
	if usage.TotalTokens != 7 {
		t.Fatalf("usage total = %d, want 7", usage.TotalTokens)
	}
}

func TestStreamEmitsEventsAndStoresReply(t *testing.T) {
	stream := &fakeStream{
		chunks: []llm.StreamChunk{
			{ReasoningContent: "think "},
			{Text: "hel"},
			{Text: "lo", FinishReason: llm.FinishReasonStop},
		},
		usage: llm.Usage{PromptTokens: 4, CompletionTokens: 2, TotalTokens: 6},
	}
	model := &fakeModel{stream: stream}
	agent, err := New(Config{Model: model})
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}

	var events []StreamEvent
	turn, err := agent.Stream(context.Background(), "hi", func(ev StreamEvent) {
		events = append(events, ev)
	})
	if err != nil {
		t.Fatalf("Stream() error = %v", err)
	}

	if turn.Reply != "hello" {
		t.Fatalf("reply = %q, want hello", turn.Reply)
	}
	if turn.Reasoning != "think" {
		t.Fatalf("reasoning = %q, want think", turn.Reasoning)
	}
	if !stream.closed {
		t.Fatal("stream was not closed")
	}
	if len(events) != 4 {
		t.Fatalf("events len = %d, want 4", len(events))
	}
	if events[0].Kind != EventReasoningDelta || events[1].Kind != EventTextDelta || events[3].Kind != EventDone {
		t.Fatalf("unexpected event sequence: %#v", events)
	}
	if events[3].Usage.TotalTokens != 6 {
		t.Fatalf("done usage = %d, want 6", events[3].Usage.TotalTokens)
	}
}

func TestStreamReturnsModelErrorWithoutPanic(t *testing.T) {
	wantErr := errors.New("stream unavailable")
	agent, err := New(Config{Model: &fakeModel{streamErr: wantErr}})
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}

	turn, err := agent.Stream(context.Background(), "hi", nil)
	if !errors.Is(err, wantErr) {
		t.Fatalf("Stream() error = %v, want %v", err, wantErr)
	}
	if turn != nil {
		t.Fatalf("Stream() turn = %#v, want nil", turn)
	}
}

func TestStreamRejectsNilStreamWithoutPanic(t *testing.T) {
	agent, err := New(Config{Model: &fakeModel{}})
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}

	turn, err := agent.Stream(context.Background(), "hi", nil)
	if err == nil {
		t.Fatal("Stream() error = nil, want nil stream error")
	}
	if !strings.Contains(err.Error(), "nil stream") {
		t.Fatalf("Stream() error = %v, want nil stream error", err)
	}
	if turn != nil {
		t.Fatalf("Stream() turn = %#v, want nil", turn)
	}
}

func TestMaxHistoryMessagesLimitsRequestContext(t *testing.T) {
	model := &fakeModel{
		chatResponses: []*llm.Response{
			{Content: "one"},
			{Content: "two"},
			{Content: "three"},
		},
	}
	agent, err := New(Config{
		Model:              model,
		SystemPrompt:       "system",
		MaxHistoryMessages: 2,
	})
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}

	for _, input := range []string{"first", "second", "third"} {
		if _, err := agent.Reply(context.Background(), input); err != nil {
			t.Fatalf("Reply(%q) error = %v", input, err)
		}
	}

	reqMessages := model.requests[2].Messages
	if len(reqMessages) != 4 {
		t.Fatalf("third request messages len = %d, want 4", len(reqMessages))
	}
	want := []struct {
		role    llm.Role
		content string
	}{
		{llm.RoleSystem, "system"},
		{llm.RoleUser, "second"},
		{llm.RoleAI, "two"},
		{llm.RoleUser, "third"},
	}
	for i, msg := range reqMessages {
		if msg.Role != want[i].role || msg.Content != want[i].content {
			t.Fatalf("message %d = %#v, want role=%s content=%q", i, msg, want[i].role, want[i].content)
		}
	}
}

func TestMaxHistoryMessagesKeepsToolBlockIntact(t *testing.T) {
	call := llm.ToolCall{
		ID:   "call_1",
		Type: string(llm.ToolTypeFunction),
		Function: llm.ToolFunction{
			Name:      "read_file",
			Arguments: `{"path":"README.md"}`,
		},
	}
	model := &fakeModel{
		chatResponses: []*llm.Response{
			{ToolCalls: []llm.ToolCall{call}, FinishReason: llm.FinishReasonToolUse},
			{Content: "read it", FinishReason: llm.FinishReasonStop},
			{Content: "next", FinishReason: llm.FinishReasonStop},
		},
	}
	tool := FunctionTool(
		"read_file",
		"Read a file",
		jsonschema.Object(map[string]jsonschema.Schema{"path": jsonschema.String("Path")}, "path"),
		func(context.Context, llm.ToolCall) (ToolResult, error) {
			return ToolResult{Content: "README contents"}, nil
		},
	)

	agent, err := New(Config{
		Model:              model,
		SystemPrompt:       "system",
		MaxHistoryMessages: 2,
		Tools:              []Tool{tool},
	})
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}

	if _, err := agent.Reply(context.Background(), "read"); err != nil {
		t.Fatalf("first Reply() error = %v", err)
	}
	if _, err := agent.Reply(context.Background(), "next"); err != nil {
		t.Fatalf("second Reply() error = %v", err)
	}

	reqMessages := model.requests[2].Messages
	if len(reqMessages) != 6 {
		t.Fatalf("request messages len = %d, want 6: %#v", len(reqMessages), reqMessages)
	}
	want := []struct {
		role       llm.Role
		content    string
		toolCalls  int
		toolCallID string
	}{
		{role: llm.RoleSystem, content: "system"},
		{role: llm.RoleUser, content: "read"},
		{role: llm.RoleAI, toolCalls: 1},
		{role: llm.RoleTool, content: "README contents", toolCallID: "call_1"},
		{role: llm.RoleAI, content: "read it"},
		{role: llm.RoleUser, content: "next"},
	}
	for i, msg := range reqMessages {
		if msg.Role != want[i].role || msg.Content != want[i].content || len(msg.ToolCalls) != want[i].toolCalls || msg.ToolCallID != want[i].toolCallID {
			t.Fatalf("message %d = %#v, want %#v", i, msg, want[i])
		}
	}
}

func TestHistorySeedAndSetHistoryAreCopied(t *testing.T) {
	seed := []llm.Message{{Role: llm.RoleUser, Content: "old"}}
	model := &fakeModel{
		chatResponses: []*llm.Response{{Content: "new"}},
	}

	agent, err := New(Config{
		Model:        model,
		SystemPrompt: "system",
		History:      seed,
	})
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	seed[0].Content = "mutated"

	if _, err := agent.Reply(context.Background(), "next"); err != nil {
		t.Fatalf("Reply() error = %v", err)
	}
	reqMessages := model.requests[0].Messages
	if len(reqMessages) != 3 {
		t.Fatalf("request messages len = %d, want 3", len(reqMessages))
	}
	if reqMessages[1].Content != "old" {
		t.Fatalf("seeded history content = %q, want old", reqMessages[1].Content)
	}

	replacement := []llm.Message{{
		Role:    llm.RoleAI,
		Content: "restored",
		ToolCalls: []llm.ToolCall{{
			ID: "call_1",
		}},
	}}
	agent.SetHistory(replacement)
	replacement[0].Content = "mutated again"
	replacement[0].ToolCalls[0].ID = "changed"

	history := agent.History()
	if len(history) != 1 {
		t.Fatalf("history len = %d, want 1", len(history))
	}
	if history[0].Content != "restored" || history[0].ToolCalls[0].ID != "call_1" {
		t.Fatalf("history = %#v", history)
	}

	history[0].Content = "caller mutation"
	if got := agent.History()[0].Content; got != "restored" {
		t.Fatalf("History() returned mutable backing storage, got %q", got)
	}
}

func TestRunTurnToolLoopLimitForcesFinalReply(t *testing.T) {
	firstCall := llm.ToolCall{
		ID: "call_1",
		Function: llm.ToolFunction{
			Name:      "echo",
			Arguments: `{"n":1}`,
		},
	}
	secondCall := llm.ToolCall{
		ID: "call_2",
		Function: llm.ToolFunction{
			Name:      "echo",
			Arguments: `{"n":2}`,
		},
	}
	model := &fakeModel{
		chatResponses: []*llm.Response{
			{ToolCalls: []llm.ToolCall{firstCall}, FinishReason: llm.FinishReasonToolUse, Usage: llm.Usage{TotalTokens: 2}},
			{ToolCalls: []llm.ToolCall{secondCall}, FinishReason: llm.FinishReasonToolUse, Usage: llm.Usage{TotalTokens: 3}},
			// Model still emits a tool call at the limit; golem must ignore it
			// and use Content as the final reply (not execute or leak it).
			{Content: "final without tools", ToolCalls: []llm.ToolCall{secondCall}, FinishReason: llm.FinishReasonStop, Usage: llm.Usage{TotalTokens: 5}},
		},
	}
	tool := FunctionTool("echo", "Echo", jsonschema.Object(nil), func(_ context.Context, call llm.ToolCall) (ToolResult, error) {
		return ToolResult{Content: call.Function.Arguments}, nil
	})

	tools, err := NewToolSet([]Tool{tool})
	if err != nil {
		t.Fatalf("NewToolSet() error = %v", err)
	}

	turn, err := RunTurn(context.Background(), TurnConfig{Model: model, Tools: tools, Input: "loop", MaxToolIterations: 1})
	if err != nil {
		t.Fatalf("RunTurn() error = %v", err)
	}
	if turn.Reply != "final without tools" {
		t.Fatalf("reply = %q, want final without tools", turn.Reply)
	}
	if turn.Usage.TotalTokens != 10 {
		t.Fatalf("usage = %d, want 10", turn.Usage.TotalTokens)
	}
	if len(turn.Steps) != 3 {
		t.Fatalf("steps len = %d, want 3", len(turn.Steps))
	}
	if turn.Steps[2].Kind != StepToolError || turn.Steps[2].ToolName != "echo" || turn.Steps[2].ToolCallID != "call_2" {
		t.Fatalf("limit step = %#v", turn.Steps[2])
	}
	if !strings.Contains(turn.Steps[2].Error, "tool iteration limit reached") {
		t.Fatalf("limit step error = %q", turn.Steps[2].Error)
	}
	if len(model.requests) != 3 {
		t.Fatalf("requests len = %d, want 3", len(model.requests))
	}
	finalReq := model.requests[2]
	if finalReq.ToolChoice == nil || finalReq.ToolChoice.Mode != llm.ToolChoiceNone {
		t.Fatalf("final request tool choice = %#v, want none", finalReq.ToolChoice)
	}
	if got := finalReq.Messages[len(finalReq.Messages)-2]; got.Role != llm.RoleTool || got.ToolCallID != "call_2" || !strings.Contains(got.Content, "tool iteration limit reached") {
		t.Fatalf("final request penultimate message = %#v, want rejected tool result", got)
	}
	if got := finalReq.Messages[len(finalReq.Messages)-1]; got.Role != llm.RoleUser || !strings.Contains(got.Content, "Tool call limit reached") {
		t.Fatalf("final request last message = %#v, want tool limit prompt", got)
	}
}

func TestToolLimitPromptIsConfigurable(t *testing.T) {
	call := llm.ToolCall{ID: "call_1", Function: llm.ToolFunction{Name: "echo", Arguments: `{}`}}
	model := &fakeModel{
		chatResponses: []*llm.Response{
			{ToolCalls: []llm.ToolCall{call}, FinishReason: llm.FinishReasonToolUse},
			{ToolCalls: []llm.ToolCall{call}, FinishReason: llm.FinishReasonToolUse},
			{Content: "done", FinishReason: llm.FinishReasonStop},
		},
	}
	tool := FunctionTool("echo", "Echo", jsonschema.Object(nil), func(_ context.Context, call llm.ToolCall) (ToolResult, error) {
		return ToolResult{Content: call.Function.Arguments}, nil
	})
	tools, err := NewToolSet([]Tool{tool})
	if err != nil {
		t.Fatalf("NewToolSet() error = %v", err)
	}

	const wrap = "Out of tool calls. Answer now."
	if _, err := RunTurn(context.Background(), TurnConfig{
		Model: model, Tools: tools, Input: "loop", MaxToolIterations: 1, ToolLimitPrompt: wrap,
	}); err != nil {
		t.Fatalf("RunTurn() error = %v", err)
	}
	if len(model.requests) != 3 {
		t.Fatalf("requests len = %d, want 3", len(model.requests))
	}
	last := model.requests[2].Messages[len(model.requests[2].Messages)-1]
	if last.Role != llm.RoleUser || last.Content != wrap {
		t.Fatalf("final request last message = %#v, want the configured prompt", last)
	}
}

func TestEmptyInput(t *testing.T) {
	agent, err := New(Config{Model: &fakeModel{}})
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}

	if _, err := agent.Reply(context.Background(), "  "); err != ErrEmptyInput {
		t.Fatalf("Reply() error = %v, want ErrEmptyInput", err)
	}
}

func TestReasoningEchoedWithinTurnButStrippedAcrossTurns(t *testing.T) {
	call := llm.ToolCall{
		ID:       "call_1",
		Type:     string(llm.ToolTypeFunction),
		Function: llm.ToolFunction{Name: "read_file", Arguments: `{"path":"README.md"}`},
	}
	model := &fakeModel{
		chatResponses: []*llm.Response{
			{ReasoningContent: "t1 plan", ToolCalls: []llm.ToolCall{call}, FinishReason: llm.FinishReasonToolUse},
			{Content: "answer one", FinishReason: llm.FinishReasonStop},
			{Content: "answer two", FinishReason: llm.FinishReasonStop},
		},
	}
	tool := FunctionTool(
		"read_file",
		"Read a file",
		jsonschema.Object(map[string]jsonschema.Schema{"path": jsonschema.String("Path")}, "path"),
		func(context.Context, llm.ToolCall) (ToolResult, error) {
			return ToolResult{Content: "README contents"}, nil
		},
	)

	agent, err := New(Config{Model: model, Tools: []Tool{tool}})
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	if _, err := agent.Reply(context.Background(), "turn one"); err != nil {
		t.Fatalf("Reply() turn one error = %v", err)
	}
	if _, err := agent.Reply(context.Background(), "turn two"); err != nil {
		t.Fatalf("Reply() turn two error = %v", err)
	}

	toolCallReasoning := func(msgs []llm.Message) string {
		for _, m := range msgs {
			if m.Role == llm.RoleAI && len(m.ToolCalls) > 0 {
				return m.ReasoningContent
			}
		}
		return "<not found>"
	}

	if len(model.requests) != 3 {
		t.Fatalf("requests len = %d, want 3", len(model.requests))
	}
	// Within turn one, the follow-up request (after the tool result) echoes the
	// tool-calling assistant's reasoning back to the model.
	if got := toolCallReasoning(model.requests[1].Messages); got != "t1 plan" {
		t.Fatalf("within-turn reasoning = %q, want echoed", got)
	}
	// Turn two seeds turn one as history; its reasoning must not be replayed.
	if got := toolCallReasoning(model.requests[2].Messages); got != "" {
		t.Fatalf("cross-turn reasoning = %q, want stripped", got)
	}
	// Retained history stays faithful so consumers can persist reasoning.
	if got := toolCallReasoning(agent.History()); got != "t1 plan" {
		t.Fatalf("history reasoning = %q, want retained", got)
	}
}

func TestUseRejectsDuplicateTools(t *testing.T) {
	agent, err := New(Config{Model: &fakeModel{}})
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	tool := FunctionTool("read_file", "Read a file", jsonschema.Object(nil), func(context.Context, llm.ToolCall) (ToolResult, error) {
		return ToolResult{}, nil
	})

	if err := agent.Use(tool); err != nil {
		t.Fatalf("Use() error = %v", err)
	}
	if err := agent.Use(tool); err == nil {
		t.Fatal("Use() duplicate error = nil")
	}
}
