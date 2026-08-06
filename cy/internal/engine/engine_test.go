package engine

import (
	"context"
	"errors"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/levmv/golems/cy/internal/session"
	"github.com/levmv/golems/pkg/golem"
	"github.com/levmv/golems/pkg/jsonschema"
	"github.com/levmv/golems/pkg/llm"
)

func TestEngineDeliversSteeringAtNextModelBoundary(t *testing.T) {
	s, err := session.Create(session.CreateOptions{Home: t.TempDir(), Workspace: "/workspace"})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	model := newBoundaryModel()
	tool := golem.FunctionToolWithEffect(golem.ToolEffectRead, "read", "read", jsonschema.Object(nil), func(context.Context, llm.ToolCall) (golem.ToolResult, error) {
		return golem.ToolResult{Content: "contents"}, nil
	})
	engine, err := New(Config{Model: model, Session: s, Tools: []golem.Tool{tool}})
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		_, err := engine.Stream(context.Background(), "start", nil)
		done <- err
	}()
	select {
	case <-model.firstStarted:
	case <-time.After(time.Second):
		t.Fatal("first model request did not start")
	}
	if err := engine.QueueInput("also inspect tests"); err != nil {
		t.Fatal(err)
	}
	close(model.releaseFirst)
	if err := <-done; err != nil {
		t.Fatal(err)
	}

	requests := model.Requests()
	if len(requests) != 2 {
		t.Fatalf("requests = %d, want 2", len(requests))
	}
	messages := requests[1].Messages
	if got := messages[len(messages)-1]; got.Role != llm.RoleUser || got.Content != "also inspect tests" {
		t.Fatalf("last message at second boundary = %#v", got)
	}
	state, err := s.Replay()
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, message := range state.Messages {
		if message.Role == llm.RoleUser && message.Content == "also inspect tests" {
			found = true
		}
	}
	if !found {
		t.Fatalf("steering missing from replayed messages: %#v", state.Messages)
	}
}

func TestEnginePopQueuedReturnsNewestAndPreservesFIFOOrder(t *testing.T) {
	s, err := session.Create(session.CreateOptions{Home: t.TempDir(), Workspace: "/workspace"})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	eng, err := New(Config{Model: &scriptedModel{}, Session: s})
	if err != nil {
		t.Fatal(err)
	}
	for _, input := range []string{"first", "second", "third"} {
		if err := eng.QueueInput(input); err != nil {
			t.Fatal(err)
		}
	}

	if got, ok, err := eng.PopQueued(); err != nil || !ok || got != "third" {
		t.Fatalf("PopQueued() = %q, %v, %v", got, ok, err)
	}
	if got, ok, err := eng.ClaimQueued(); err != nil || !ok || got != "first" {
		t.Fatalf("ClaimQueued() = %q, %v, %v", got, ok, err)
	}
	if restored, err := eng.RestoreQueued(); err != nil || len(restored) != 1 || restored[0] != "second" {
		t.Fatalf("RestoreQueued() = %#v, %v", restored, err)
	}
}

func TestEngineAddsCurrentInstructionPrompts(t *testing.T) {
	s, err := session.Create(session.CreateOptions{Home: t.TempDir(), Workspace: "/workspace"})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	model := &scriptedModel{chatResponses: []*llm.Response{{Content: "done", FinishReason: llm.FinishReasonStop}}}
	engine, err := New(Config{Model: model, Session: s, SystemPrompt: "base", InstructionPrompts: []string{"Instructions from AGENTS.md:\nAlways run focused tests."}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := engine.Stream(context.Background(), "work", nil); err != nil {
		t.Fatal(err)
	}
	messages := model.requests[0].Messages
	if len(messages) < 3 || messages[0].Content != "base" || messages[1].Role != llm.RoleSystem || !strings.Contains(messages[1].Content, "Always run focused tests.") {
		t.Fatalf("request messages = %#v", messages)
	}
}

func TestEngineRunsReadBatchConcurrentlyAndJournalsSourceOrder(t *testing.T) {
	s, err := session.Create(session.CreateOptions{Home: t.TempDir(), Workspace: "/workspace"})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	calls := []llm.ToolCall{
		{ID: "call-1", Function: llm.ToolFunction{Name: "read", Arguments: `{"path":"one"}`}},
		{ID: "call-2", Function: llm.ToolFunction{Name: "read", Arguments: `{"path":"two"}`}},
	}
	model := &scriptedModel{streams: []llm.Stream{
		&scriptedStream{chunks: []llm.StreamChunk{{ToolCalls: calls, FinishReason: llm.FinishReasonToolUse}}},
		&scriptedStream{chunks: []llm.StreamChunk{{Text: "done", FinishReason: llm.FinishReasonStop}}},
	}}
	started := make(chan string, 2)
	release := make(chan struct{})
	tool := golem.FunctionToolWithEffect(golem.ToolEffectRead, "read", "read", jsonschema.Object(nil), func(_ context.Context, call llm.ToolCall) (golem.ToolResult, error) {
		started <- call.ID
		<-release
		return golem.ToolResult{Content: call.ID + " result"}, nil
	})
	engine, err := New(Config{Model: model, Session: s, Tools: []golem.Tool{tool}})
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		_, err := engine.Stream(context.Background(), "read both", nil)
		done <- err
	}()
	for i := 0; i < 2; i++ {
		select {
		case <-started:
		case <-time.After(time.Second):
			t.Fatal("read batch did not start concurrently")
		}
	}
	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if len(model.requests) != 2 {
		t.Fatalf("requests = %d", len(model.requests))
	}
	requestMessages := model.requests[1].Messages
	if requestMessages[len(requestMessages)-2].ToolCallID != "call-1" || requestMessages[len(requestMessages)-1].ToolCallID != "call-2" {
		t.Fatalf("tool messages not in source order: %#v", requestMessages)
	}
	records, err := s.Records()
	if err != nil {
		t.Fatal(err)
	}
	var results []string
	for _, record := range records {
		if record.Type == session.RecordToolResult {
			payload, _ := session.DecodePayload[session.ToolResult](record)
			results = append(results, payload.ToolCallID)
		}
	}
	if strings.Join(results, ",") != "call-1,call-2" {
		t.Fatalf("results=%v", results)
	}
}

func TestEnginePersistsToolStepAndResumesFromJournal(t *testing.T) {
	home := t.TempDir()
	s, err := session.Create(session.CreateOptions{Home: home, Workspace: "/workspace", Model: "fake/model"})
	if err != nil {
		t.Fatal(err)
	}
	call := llm.ToolCall{
		ID:   "call-1",
		Type: string(llm.ToolTypeFunction),
		Function: llm.ToolFunction{
			Name:      "read",
			Arguments: `{"path":"README.md"}`,
		},
	}
	model := &scriptedModel{streams: []llm.Stream{
		&scriptedStream{chunks: []llm.StreamChunk{{ToolCalls: []llm.ToolCall{call}, FinishReason: llm.FinishReasonToolUse}}, usage: llm.Usage{TotalTokens: 4}},
		&scriptedStream{chunks: []llm.StreamChunk{{Text: "done", FinishReason: llm.FinishReasonStop}}, usage: llm.Usage{TotalTokens: 3}},
	}}
	toolRuns := 0
	readTool := golem.FunctionTool("read", "read", jsonschema.Obj().NoAdditionalProperties(), func(context.Context, llm.ToolCall) (golem.ToolResult, error) {
		toolRuns++
		return golem.ToolResult{Content: "file contents", Meta: map[string]any{"path": "README.md"}}, nil
	})
	engine, err := New(Config{Model: model, Session: s, SystemPrompt: "system", Tools: []golem.Tool{readTool}})
	if err != nil {
		t.Fatal(err)
	}

	var events []golem.StreamEvent
	turn, err := engine.Stream(context.Background(), "inspect", func(event golem.StreamEvent) {
		events = append(events, event)
	})
	if err != nil {
		t.Fatalf("Stream() error = %v", err)
	}
	if turn.Reply != "done" || turn.Usage.TotalTokens != 7 || toolRuns != 1 {
		t.Fatalf("turn=%#v toolRuns=%d", turn, toolRuns)
	}
	if len(events) != 4 || events[0].Kind != golem.EventToolCall || events[1].Kind != golem.EventToolResult || events[2].Kind != golem.EventTextDelta || events[3].Kind != golem.EventDone {
		t.Fatalf("events = %#v", events)
	}
	if meta, ok := events[1].Step.Meta.(map[string]any); !ok || meta["path"] != "README.md" {
		t.Fatalf("tool result meta = %#v", events[1].Step.Meta)
	}
	records, err := s.Records()
	if err != nil {
		t.Fatal(err)
	}
	wantTypes := []session.RecordType{
		session.RecordSessionStarted,
		session.RecordSessionConfigured,
		session.RecordUserMessage,
		session.RecordAssistantMessage,
		session.RecordToolResult,
		session.RecordAssistantMessage,
		session.RecordRunFinished,
	}
	if len(records) != len(wantTypes) {
		t.Fatalf("record count = %d, want %d: %#v", len(records), len(wantTypes), records)
	}
	for i, want := range wantTypes {
		if records[i].Type != want {
			t.Fatalf("record %d type = %q, want %q", i, records[i].Type, want)
		}
	}
	state, err := s.Replay()
	if err != nil {
		t.Fatal(err)
	}
	if len(state.Messages) < 3 {
		t.Fatalf("replayed messages = %#v", state.Messages)
	}
	toolMeta, ok := state.Messages[2].Meta.(map[string]any)
	if !ok || toolMeta["path"] != "README.md" {
		t.Fatalf("replayed tool meta = %#v", state.Messages[2].Meta)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	resumed, err := session.Open(home, s.ID())
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	defer resumed.Close()
	resumeModel := &scriptedModel{chatResponses: []*llm.Response{{Content: "again", FinishReason: llm.FinishReasonStop}}}
	resumedEngine, err := New(Config{Model: resumeModel, Session: resumed, SystemPrompt: "system", Tools: []golem.Tool{readTool}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := resumedEngine.Stream(context.Background(), "continue", nil); err != nil {
		t.Fatalf("Stream() after resume error = %v", err)
	}
	if len(resumeModel.requests) != 1 {
		t.Fatalf("resume requests = %d", len(resumeModel.requests))
	}
	messages := resumeModel.requests[0].Messages
	wantRoles := []llm.Role{llm.RoleSystem, llm.RoleUser, llm.RoleAI, llm.RoleTool, llm.RoleAI, llm.RoleUser}
	if len(messages) != len(wantRoles) {
		t.Fatalf("resume messages = %#v", messages)
	}
	for i, want := range wantRoles {
		if messages[i].Role != want {
			t.Fatalf("message %d role = %q, want %q", i, messages[i].Role, want)
		}
	}
}

func TestEngineRetriesPartialStreamWithoutCommittingPartialAssistant(t *testing.T) {
	s, err := session.Create(session.CreateOptions{Home: t.TempDir(), Workspace: "/workspace"})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	wantErr := errors.New("stream reset")
	model := &scriptedModel{streams: []llm.Stream{
		&scriptedStream{chunks: []llm.StreamChunk{{Text: "partial"}}, finalErr: wantErr},
		&scriptedStream{chunks: []llm.StreamChunk{{Text: "complete", FinishReason: llm.FinishReasonStop}}},
	}}
	engine, err := New(Config{Model: model, Session: s, RequestPolicy: golem.RequestPolicy{MaxRetries: 1}})
	if err != nil {
		t.Fatal(err)
	}
	var events []golem.StreamEvent
	turn, err := engine.Stream(context.Background(), "hello", func(event golem.StreamEvent) {
		events = append(events, event)
	})
	if err != nil {
		t.Fatalf("Stream() error = %v", err)
	}
	if turn.Reply != "complete" || len(model.requests) != 2 {
		t.Fatalf("turn=%#v requests=%d", turn, len(model.requests))
	}
	if len(events) != 5 || events[0].Text != "partial" || events[1].Kind != golem.EventAttemptReset || events[2].Kind != golem.EventModelRetry || events[3].Text != "complete" || events[4].Kind != golem.EventDone {
		t.Fatalf("events = %#v", events)
	}
	state, err := s.Replay()
	if err != nil {
		t.Fatal(err)
	}
	if len(state.Messages) != 2 || state.Messages[1].Content != "complete" {
		t.Fatalf("replayed messages = %#v", state.Messages)
	}
}

func TestProviderContextErrorCompactsAndRebuildsOnce(t *testing.T) {
	s, err := session.Create(session.CreateOptions{Home: t.TempDir(), Workspace: "/workspace"})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	appendCompletedTurn(t, s, "old", strings.Repeat("old context ", 12000), "old answer")
	appendCompletedTurn(t, s, "recent", "recent question", "recent answer")
	model := &scriptedModel{
		chatErrors: []error{&llm.Error{StatusCode: 400, Code: "context_length_exceeded", Message: "maximum context length exceeded", Provider: "test"}},
		chatResponses: []*llm.Response{
			{Content: "summary after provider rejection"},
			{Content: "recovered", FinishReason: llm.FinishReasonStop},
		},
	}
	eng, err := New(Config{Model: model, Session: s, ContextWindow: 1_000_000})
	if err != nil {
		t.Fatal(err)
	}
	turn, err := eng.Stream(context.Background(), "continue", nil)
	if err != nil {
		t.Fatal(err)
	}
	if turn.Reply != "recovered" || len(model.requests) != 3 {
		t.Fatalf("turn=%#v requests=%d", turn, len(model.requests))
	}
	state, err := s.Replay()
	if err != nil {
		t.Fatal(err)
	}
	if state.CompactionCount != 1 {
		t.Fatalf("compactions = %d", state.CompactionCount)
	}
}

func TestActiveToolTurnPreservesReasoningButLaterTurnStripsIt(t *testing.T) {
	s, err := session.Create(session.CreateOptions{Home: t.TempDir(), Workspace: "/workspace"})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	call := llm.ToolCall{ID: "call-1", Function: llm.ToolFunction{Name: "read", Arguments: `{}`}}
	model := &scriptedModel{chatResponses: []*llm.Response{
		{ReasoningContent: "provider-required reasoning", ToolCalls: []llm.ToolCall{call}, FinishReason: llm.FinishReasonToolUse},
		{Content: "first done", FinishReason: llm.FinishReasonStop},
		{Content: "second done", FinishReason: llm.FinishReasonStop},
	}}
	tool := golem.FunctionTool("read", "read", jsonschema.Obj().NoAdditionalProperties(), func(context.Context, llm.ToolCall) (golem.ToolResult, error) {
		return golem.ToolResult{Content: "data"}, nil
	})
	eng, err := New(Config{Model: model, Session: s, Tools: []golem.Tool{tool}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := eng.Stream(context.Background(), "first", nil); err != nil {
		t.Fatal(err)
	}
	if got := model.requests[1].Messages[2].ReasoningContent; got != "provider-required reasoning" {
		t.Fatalf("active-turn reasoning = %q", got)
	}
	if _, err := eng.Stream(context.Background(), "second", nil); err != nil {
		t.Fatal(err)
	}
	for _, message := range model.requests[2].Messages {
		if message.ReasoningContent != "" {
			t.Fatalf("prior-turn reasoning leaked into later request: %#v", message)
		}
	}
}

func TestEnginePersistsCancellationAndInput(t *testing.T) {
	s, err := session.Create(session.CreateOptions{Home: t.TempDir(), Workspace: "/workspace"})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	model := &scriptedModel{streams: []llm.Stream{&scriptedStream{finalErr: context.Canceled}}}
	engine, err := New(Config{Model: model, Session: s, RequestPolicy: golem.RequestPolicy{MaxRetries: 2}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := engine.Stream(context.Background(), "do not lose this", nil); !errors.Is(err, context.Canceled) {
		t.Fatalf("Stream() error = %v, want context.Canceled", err)
	}
	state, err := s.Replay()
	if err != nil {
		t.Fatal(err)
	}
	if len(state.Messages) != 1 || state.Messages[0].Content != "do not lose this" {
		t.Fatalf("messages = %#v", state.Messages)
	}
	if len(model.requests) != 1 {
		t.Fatalf("requests = %d, cancellation must not retry", len(model.requests))
	}
	records, err := s.Records()
	if err != nil {
		t.Fatal(err)
	}
	assertLastRunFinished(t, records, session.RunInterrupted)
}

func TestRunFinishedRecordsHowTheRunEnded(t *testing.T) {
	t.Run("completed", func(t *testing.T) {
		s := newTestSession(t)
		model := &scriptedModel{streams: []llm.Stream{
			&scriptedStream{chunks: []llm.StreamChunk{{Text: "done", FinishReason: llm.FinishReasonStop}}},
		}}
		engine, err := New(Config{Model: model, Session: s})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := engine.Stream(context.Background(), "hello", nil); err != nil {
			t.Fatal(err)
		}
		finished := lastRunFinished(t, s)
		if finished.Outcome != session.RunCompleted || finished.Error != "" {
			t.Fatalf("run_finished = %#v, want a clean completion", finished)
		}
	})

	t.Run("failed", func(t *testing.T) {
		s := newTestSession(t)
		model := &scriptedModel{streams: []llm.Stream{
			&scriptedStream{finalErr: errors.New("provider hung up on sk-secret-token")},
		}}
		engine, err := New(Config{
			Model:    model,
			Session:  s,
			Sanitize: func(text string) string { return strings.ReplaceAll(text, "sk-secret-token", "[redacted]") },
		})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := engine.Stream(context.Background(), "hello", nil); err == nil {
			t.Fatal("Stream() error = nil, want the provider failure")
		}
		finished := lastRunFinished(t, s)
		if finished.Outcome != session.RunFailed {
			t.Fatalf("outcome = %q, want failed", finished.Outcome)
		}
		// The journal outlives the run and gets copied around, so what it keeps
		// of an error is the sanitized text and not the original.
		if !strings.Contains(finished.Error, "[redacted]") || strings.Contains(finished.Error, "sk-secret-token") {
			t.Fatalf("recorded error = %q, want the sanitized text", finished.Error)
		}
	})

	t.Run("interrupted", func(t *testing.T) {
		s := newTestSession(t)
		model := &scriptedModel{streams: []llm.Stream{&scriptedStream{finalErr: context.Canceled}}}
		engine, err := New(Config{Model: model, Session: s})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := engine.Stream(context.Background(), "hello", nil); !errors.Is(err, context.Canceled) {
			t.Fatalf("Stream() error = %v, want context.Canceled", err)
		}
		finished := lastRunFinished(t, s)
		if finished.Outcome != session.RunInterrupted {
			t.Fatalf("outcome = %q, want interrupted", finished.Outcome)
		}
	})
}

func newTestSession(t *testing.T) *session.Session {
	t.Helper()
	s, err := session.Create(session.CreateOptions{Home: t.TempDir(), Workspace: "/workspace"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func lastRunFinished(t *testing.T, s *session.Session) session.RunFinished {
	t.Helper()
	records, err := s.Records()
	if err != nil {
		t.Fatal(err)
	}
	if len(records) == 0 || records[len(records)-1].Type != session.RecordRunFinished {
		t.Fatalf("last record = %#v, want run_finished", records)
	}
	finished, err := session.DecodePayload[session.RunFinished](records[len(records)-1])
	if err != nil {
		t.Fatal(err)
	}
	return finished
}

func assertLastRunFinished(t *testing.T, records []session.Record, want session.RunOutcome) {
	t.Helper()
	if len(records) == 0 || records[len(records)-1].Type != session.RecordRunFinished {
		t.Fatalf("last record = %#v, want run_finished", records)
	}
	finished, err := session.DecodePayload[session.RunFinished](records[len(records)-1])
	if err != nil {
		t.Fatal(err)
	}
	if finished.RunID == "" {
		t.Fatal("run_finished has an empty run ID")
	}
	if finished.Outcome != want {
		t.Fatalf("outcome = %q, want %q", finished.Outcome, want)
	}
}

type scriptedModel struct {
	chatResponses []*llm.Response
	chatErrors    []error
	streams       []llm.Stream
	streamErrors  []error
	requests      []llm.Request
}

type boundaryModel struct {
	mu           sync.Mutex
	requests     []llm.Request
	firstStarted chan struct{}
	releaseFirst chan struct{}
}

func newBoundaryModel() *boundaryModel {
	return &boundaryModel{firstStarted: make(chan struct{}), releaseFirst: make(chan struct{})}
}

func (m *boundaryModel) Chat(ctx context.Context, request llm.Request) (*llm.Response, error) {
	return m.respond(ctx, request)
}

func (m *boundaryModel) Stream(ctx context.Context, request llm.Request) (llm.Stream, error) {
	response, err := m.respond(ctx, request)
	if err != nil {
		return nil, err
	}
	return streamResponse(response), nil
}

func (m *boundaryModel) respond(ctx context.Context, request llm.Request) (*llm.Response, error) {
	m.mu.Lock()
	index := len(m.requests)
	m.requests = append(m.requests, request)
	m.mu.Unlock()
	if index == 0 {
		close(m.firstStarted)
		select {
		case <-m.releaseFirst:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
		return &llm.Response{ToolCalls: []llm.ToolCall{{ID: "call-1", Function: llm.ToolFunction{Name: "read", Arguments: `{}`}}}, FinishReason: llm.FinishReasonToolUse}, nil
	}
	return &llm.Response{Content: "done", FinishReason: llm.FinishReasonStop}, nil
}

func (m *boundaryModel) Requests() []llm.Request {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]llm.Request(nil), m.requests...)
}

func (m *scriptedModel) Chat(_ context.Context, request llm.Request) (*llm.Response, error) {
	m.requests = append(m.requests, request)
	if len(m.chatErrors) > 0 {
		err := m.chatErrors[0]
		m.chatErrors = m.chatErrors[1:]
		return nil, err
	}
	if len(m.chatResponses) == 0 {
		return &llm.Response{}, nil
	}
	response := m.chatResponses[0]
	m.chatResponses = m.chatResponses[1:]
	return response, nil
}

func (m *scriptedModel) Stream(_ context.Context, request llm.Request) (llm.Stream, error) {
	m.requests = append(m.requests, request)
	if len(m.streamErrors) > 0 {
		err := m.streamErrors[0]
		m.streamErrors = m.streamErrors[1:]
		return nil, err
	}
	if len(m.streams) == 0 {
		if len(m.chatErrors) > 0 {
			err := m.chatErrors[0]
			m.chatErrors = m.chatErrors[1:]
			return nil, err
		}
		if len(m.chatResponses) == 0 {
			return nil, errors.New("no scripted stream")
		}
		response := m.chatResponses[0]
		m.chatResponses = m.chatResponses[1:]
		return streamResponse(response), nil
	}
	stream := m.streams[0]
	m.streams = m.streams[1:]
	return stream, nil
}

func streamResponse(response *llm.Response) llm.Stream {
	if response == nil {
		return &scriptedStream{}
	}
	return &scriptedStream{
		chunks: []llm.StreamChunk{{
			Text:             response.Content,
			ReasoningContent: response.ReasoningContent,
			ToolCalls:        response.ToolCalls,
			FinishReason:     response.FinishReason,
		}},
		usage: response.Usage,
	}
}

type scriptedStream struct {
	chunks   []llm.StreamChunk
	usage    llm.Usage
	finalErr error
	index    int
	closed   bool
}

func (s *scriptedStream) Recv() (llm.StreamChunk, error) {
	if s.index < len(s.chunks) {
		chunk := s.chunks[s.index]
		s.index++
		return chunk, nil
	}
	if s.finalErr != nil {
		err := s.finalErr
		s.finalErr = nil
		return llm.StreamChunk{}, err
	}
	return llm.StreamChunk{}, io.EOF
}

func (s *scriptedStream) Usage() llm.Usage { return s.usage }

func (s *scriptedStream) Close() error {
	s.closed = true
	return nil
}

func TestEngineRecordsConfigurationAndEveryChangeToIt(t *testing.T) {
	home := t.TempDir()
	s, err := session.Create(session.CreateOptions{Home: home, Workspace: "/workspace", Model: "fake/model"})
	if err != nil {
		t.Fatal(err)
	}
	newTool := func(name string) golem.Tool {
		return golem.FunctionTool(name, name, jsonschema.Obj().NoAdditionalProperties(), func(context.Context, llm.ToolCall) (golem.ToolResult, error) {
			return golem.ToolResult{Content: "contents"}, nil
		})
	}
	cfg := Config{
		Model:              &scriptedModel{},
		Session:            s,
		SystemPrompt:       "system",
		InstructionPrompts: []string{"project rules"},
		Tools:              []golem.Tool{newTool("read")},
		Sandbox:            session.SandboxState{Policy: "auto", Effective: "off", Probe: "no platform sandbox is available"},
	}
	eng, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}

	first := configurations(t, s)
	if len(first) != 1 {
		t.Fatalf("session_configured records = %d, want 1", len(first))
	}
	if first[0].SystemPrompt != "system" || len(first[0].InstructionPrompts) != 1 || first[0].InstructionPrompts[0] != "project rules" {
		t.Fatalf("recorded prompts = %#v", first[0])
	}
	if len(first[0].Tools) != 1 || first[0].Tools[0].Function.Name != "read" {
		t.Fatalf("recorded tools = %#v", first[0].Tools)
	}
	// The whole point of carrying the sandbox here: "auto" that resolved to no
	// fence has to be legible afterwards, and the probe is why.
	if first[0].Sandbox.Effective != "off" || first[0].Sandbox.Backend != "" || first[0].Sandbox.Probe == "" {
		t.Fatalf("recorded sandbox = %#v", first[0].Sandbox)
	}

	// A resume that changes nothing should say nothing. Otherwise reopening a
	// session ten times leaves ten identical copies of the catalog behind.
	if _, err := New(cfg); err != nil {
		t.Fatal(err)
	}
	if again := configurations(t, s); len(again) != 1 {
		t.Fatalf("session_configured records after an unchanged rebuild = %d, want 1", len(again))
	}

	if err := eng.ReconfigureTools([]golem.Tool{newTool("read"), newTool("write")}); err != nil {
		t.Fatal(err)
	}
	if err := eng.ReconfigureSandbox(session.SandboxState{Policy: "on", Effective: "on", Backend: "seatbelt"}); err != nil {
		t.Fatal(err)
	}
	recorded := configurations(t, s)
	if len(recorded) != 3 {
		t.Fatalf("session_configured records after two changes = %d, want 3", len(recorded))
	}
	if len(recorded[1].Tools) != 2 || recorded[1].Sandbox.Backend != "" {
		t.Fatalf("catalog change recorded as %#v", recorded[1])
	}
	if recorded[2].Sandbox.Backend != "seatbelt" || len(recorded[2].Tools) != 2 {
		t.Fatalf("sandbox change recorded as %#v", recorded[2])
	}

	state, err := s.Replay()
	if err != nil {
		t.Fatal(err)
	}
	if state.Configured == nil || state.Configured.Sandbox.Backend != "seatbelt" {
		t.Fatalf("replayed configuration = %#v, want the last one recorded", state.Configured)
	}
	if len(state.Messages) != 0 {
		t.Fatalf("configuration records became messages: %#v", state.Messages)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
}

func configurations(t *testing.T, s *session.Session) []session.SessionConfigured {
	t.Helper()
	records, err := s.Records()
	if err != nil {
		t.Fatal(err)
	}
	var found []session.SessionConfigured
	for _, record := range records {
		if record.Type != session.RecordSessionConfigured {
			continue
		}
		payload, err := session.DecodePayload[session.SessionConfigured](record)
		if err != nil {
			t.Fatal(err)
		}
		found = append(found, payload)
	}
	return found
}

func TestEngineJournalsBoundaryEventsItDelivers(t *testing.T) {
	s, err := session.Create(session.CreateOptions{Home: t.TempDir(), Workspace: "/workspace"})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	model := &scriptedModel{chatResponses: []*llm.Response{{Content: "noted"}}}
	event := BoundaryEvent{JobID: "job-4f2a", Content: "Background job job-4f2a completed: status=failed, error=token s3cret rejected."}
	served := false
	eng, err := New(Config{
		Model:   model,
		Session: s,
		BoundaryEvents: func(string) ([]BoundaryEvent, error) {
			if served {
				return nil, nil
			}
			served = true
			return []BoundaryEvent{event}, nil
		},
		Sanitize: func(text string) string { return strings.ReplaceAll(text, "s3cret", "[REDACTED]") },
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := eng.Stream(context.Background(), "start", nil); err != nil {
		t.Fatal(err)
	}
	want := strings.ReplaceAll(event.Content, "s3cret", "[REDACTED]")

	requests := model.requests
	if len(requests) != 1 {
		t.Fatalf("requests = %d, want 1", len(requests))
	}
	messages := requests[0].Messages
	last := messages[len(messages)-1]
	if last.Role != llm.RoleSystem || last.Content != want {
		t.Fatalf("last message the model saw = %#v, want the redacted boundary event", last)
	}

	records, err := s.Records()
	if err != nil {
		t.Fatal(err)
	}
	var recorded []session.BoundaryEvent
	for _, record := range records {
		if record.Type != session.RecordBoundaryEvent {
			continue
		}
		payload, err := session.DecodePayload[session.BoundaryEvent](record)
		if err != nil {
			t.Fatal(err)
		}
		recorded = append(recorded, payload)
	}
	if len(recorded) != 1 {
		t.Fatalf("boundary_event records = %d, want 1", len(recorded))
	}
	if recorded[0].JobID != "job-4f2a" {
		t.Fatalf("recorded job id = %q, want the job the event reported on", recorded[0].JobID)
	}
	if recorded[0].Content != want {
		t.Fatalf("recorded content = %q, want it redacted", recorded[0].Content)
	}
	if recorded[0].RunID == "" {
		t.Fatal("recorded boundary event has no run id")
	}

	// The point of the record: a reconstruction of the session has to show the
	// model being told, or a replay shows it reacting to something it never saw.
	state, err := s.Replay()
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, message := range state.Messages {
		if message.Role == llm.RoleSystem && message.Content == want {
			found = true
		}
	}
	if !found {
		t.Fatalf("boundary event missing from replayed messages: %#v", state.Messages)
	}
}

func TestEngineToolFuseDefaultsAndAcceptsUnlimited(t *testing.T) {
	for _, test := range []struct {
		name       string
		configured int
		want       int
	}{
		{name: "unset takes the default fuse", configured: 0, want: defaultMaxToolIterationsPerTurn},
		{name: "a positive value replaces it", configured: 12, want: 12},
		{name: "a negative value survives as unlimited", configured: -1, want: -1},
	} {
		t.Run(test.name, func(t *testing.T) {
			s, err := session.Create(session.CreateOptions{Home: t.TempDir(), Workspace: "/workspace"})
			if err != nil {
				t.Fatal(err)
			}
			defer s.Close()
			eng, err := New(Config{Model: &scriptedModel{}, Session: s, MaxToolIterations: test.configured})
			if err != nil {
				t.Fatal(err)
			}
			if eng.maxToolIterations != test.want {
				t.Fatalf("maxToolIterations = %d, want %d", eng.maxToolIterations, test.want)
			}
		})
	}
}

// The fuse is worth a behavioural test and not only a plumbing one: tripping it
// is not a failure, it is a forced summary, so a fuse wired to the wrong place
// would still produce a turn that looks like it worked.
func TestEngineStopsToolCallsAtTheConfiguredFuse(t *testing.T) {
	s, err := session.Create(session.CreateOptions{Home: t.TempDir(), Workspace: "/workspace"})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	var executions int
	tool := golem.FunctionToolWithEffect(golem.ToolEffectRead, "read", "read", jsonschema.Object(nil), func(context.Context, llm.ToolCall) (golem.ToolResult, error) {
		executions++
		return golem.ToolResult{Content: "contents"}, nil
	})
	engine, err := New(Config{Model: &fuseModel{}, Session: s, Tools: []golem.Tool{tool}, MaxToolIterations: 2})
	if err != nil {
		t.Fatal(err)
	}
	turn, err := engine.Stream(context.Background(), "keep going", nil)
	if err != nil {
		t.Fatal(err)
	}
	if executions != 2 {
		t.Fatalf("tool executions = %d, want the configured fuse of 2", executions)
	}
	// A tripped fuse ends the turn by asking for a summary with tools withheld,
	// so the turn completes. Only the journal says it was cut off.
	if turn.Reply != "summary without tools" {
		t.Fatalf("reply = %q, want the forced summary", turn.Reply)
	}
	records, err := s.Records()
	if err != nil {
		t.Fatal(err)
	}
	limits := 0
	for _, record := range records {
		if record.Type != session.RecordToolResult {
			continue
		}
		payload, err := session.DecodePayload[session.ToolResult](record)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(payload.Content, "tool iteration limit reached after 2 iterations") {
			limits++
		}
	}
	if limits != 1 {
		t.Fatalf("journalled tool-limit results = %d, want 1", limits)
	}
	// The outcome is "completed", which is true and is not the whole story. A
	// reader asking whether this run was cut off should not have to grep the
	// prose of a synthetic tool result to find out.
	finished := lastRunFinished(t, s)
	if finished.Outcome != session.RunCompleted {
		t.Fatalf("outcome = %q, want the run to close as completed", finished.Outcome)
	}
	if !finished.ToolLimitReached {
		t.Fatal("run_finished does not record that the tool fuse blew")
	}
}

func TestRunFinishedDoesNotClaimAFuseThatHeld(t *testing.T) {
	s, err := session.Create(session.CreateOptions{Home: t.TempDir(), Workspace: "/workspace"})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	tool := golem.FunctionToolWithEffect(golem.ToolEffectRead, "read", "read", jsonschema.Object(nil), func(context.Context, llm.ToolCall) (golem.ToolResult, error) {
		return golem.ToolResult{Content: "contents"}, nil
	})
	engine, err := New(Config{Model: &fuseModel{}, Session: s, Tools: []golem.Tool{tool}, MaxToolIterations: 2})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := engine.Stream(context.Background(), "keep going", nil); err != nil {
		t.Fatal(err)
	}
	// A second run on the same engine. The flag belongs to the turn, so a turn
	// that never tripped must not inherit the previous one's.
	engine.model = &scriptedModel{streams: []llm.Stream{
		&scriptedStream{chunks: []llm.StreamChunk{{Text: "done", FinishReason: llm.FinishReasonStop}}},
	}}
	if _, err := engine.Stream(context.Background(), "one more", nil); err != nil {
		t.Fatal(err)
	}
	if finished := lastRunFinished(t, s); finished.ToolLimitReached {
		t.Fatal("a run that never reached the fuse is recorded as having blown it")
	}
}

// fuseModel asks for a tool for as long as it is allowed one, so the turn ends
// only because the fuse blew. The trip path asks for a summary by sending the
// tools with ToolChoice none rather than by withholding them, so that is what
// this has to honour.
type fuseModel struct{ mu sync.Mutex }

func (m *fuseModel) Chat(ctx context.Context, request llm.Request) (*llm.Response, error) {
	return m.respond(request)
}

func (m *fuseModel) Stream(ctx context.Context, request llm.Request) (llm.Stream, error) {
	response, err := m.respond(request)
	if err != nil {
		return nil, err
	}
	return streamResponse(response), nil
}

func (m *fuseModel) respond(request llm.Request) (*llm.Response, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if request.ToolChoice != nil && request.ToolChoice.Mode == llm.ToolChoiceNone {
		return &llm.Response{Content: "summary without tools", FinishReason: llm.FinishReasonStop}, nil
	}
	return &llm.Response{
		ToolCalls:    []llm.ToolCall{{ID: "call", Function: llm.ToolFunction{Name: "read", Arguments: `{}`}}},
		FinishReason: llm.FinishReasonToolUse,
	}, nil
}
