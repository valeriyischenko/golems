package golem

import (
	"context"
	"errors"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/levmv/golems/pkg/llm"
)

type requestScriptModel struct {
	mu           sync.Mutex
	chat         func(call int, request llm.Request) (*llm.Response, error)
	streams      []llm.Stream
	streamErrors []error
	chatCalls    int
	streamCalls  int
}

func (m *requestScriptModel) Chat(_ context.Context, request llm.Request) (*llm.Response, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.chatCalls++
	return m.chat(m.chatCalls, request)
}

func (m *requestScriptModel) Stream(_ context.Context, _ llm.Request) (llm.Stream, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.streamCalls++
	index := m.streamCalls - 1
	if index < len(m.streamErrors) && m.streamErrors[index] != nil {
		return nil, m.streamErrors[index]
	}
	if index >= len(m.streams) {
		return nil, errors.New("no scripted stream")
	}
	return m.streams[index], nil
}

type requestScriptStream struct {
	chunks   []llm.StreamChunk
	finalErr error
	usage    llm.Usage
	index    int
}

func (s *requestScriptStream) Recv() (llm.StreamChunk, error) {
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

func (s *requestScriptStream) Usage() llm.Usage { return s.usage }
func (s *requestScriptStream) Close() error     { return nil }

func TestRequesterRetriesAndResetsPartialStream(t *testing.T) {
	wantErr := errors.New("connection reset")
	model := &requestScriptModel{streams: []llm.Stream{
		&requestScriptStream{chunks: []llm.StreamChunk{{Text: "partial"}}, finalErr: wantErr},
		&requestScriptStream{chunks: []llm.StreamChunk{{Text: "complete", FinishReason: llm.FinishReasonStop}}},
	}}
	var failures []RequestFailure
	requester, err := NewRequester(RequesterConfig{Model: model, Policy: RequestPolicy{MaxRetries: 1}, Hooks: RequestHooks{
		AttemptFailed: func(failure RequestFailure) error {
			failures = append(failures, failure)
			return nil
		},
	}})
	if err != nil {
		t.Fatal(err)
	}

	var events []StreamEvent
	response, err := requester.Request(context.Background(), 3, llm.Request{}, true, func(event StreamEvent) {
		events = append(events, event)
	})
	if err != nil {
		t.Fatal(err)
	}
	if response.Content != "complete" || model.streamCalls != 2 {
		t.Fatalf("response=%#v calls=%d", response, model.streamCalls)
	}
	wantKinds := []StreamEventKind{EventTextDelta, EventAttemptReset, EventModelRetry, EventTextDelta}
	if len(events) != len(wantKinds) {
		t.Fatalf("events = %#v", events)
	}
	for index, kind := range wantKinds {
		if events[index].Kind != kind {
			t.Fatalf("event %d kind = %q, want %q", index, events[index].Kind, kind)
		}
	}
	if retry := events[2]; retry.RetryKey != wantErr.Error() || !strings.Contains(retry.Text, "#1 in ") || strings.Contains(retry.Text, "budget remaining") {
		t.Fatalf("retry event = %#v, want compact text and stable cause", retry)
	}
	if len(failures) != 1 || failures[0].Step != 3 || failures[0].Attempt != 1 || failures[0].ProvisionalText != "partial" || !failures[0].HadProvisionalOutput {
		t.Fatalf("failures = %#v", failures)
	}
}

func TestFormatRetryCauseCompactsProviderError(t *testing.T) {
	err := &llm.Error{Provider: "openrouter", StatusCode: 429, Message: "Provider returned error"}
	if got, want := formatRetryCause(err), "openrouter 429: Provider returned error"; got != want {
		t.Fatalf("formatRetryCause() = %q, want %q", got, want)
	}
}

func TestRequesterResetsReasoningOnlyAttempt(t *testing.T) {
	model := &requestScriptModel{streams: []llm.Stream{
		&requestScriptStream{chunks: []llm.StreamChunk{{ReasoningContent: "thinking"}}, finalErr: errors.New("reset")},
		&requestScriptStream{chunks: []llm.StreamChunk{{Text: "done"}}},
	}}
	requester, err := NewRequester(RequesterConfig{Model: model, Policy: RequestPolicy{MaxRetries: 1}})
	if err != nil {
		t.Fatal(err)
	}
	var events []StreamEvent
	if _, err := requester.Request(context.Background(), 1, llm.Request{}, true, func(event StreamEvent) {
		events = append(events, event)
	}); err != nil {
		t.Fatal(err)
	}
	if len(events) < 2 || events[0].Kind != EventReasoningDelta || events[1].Kind != EventAttemptReset {
		t.Fatalf("events = %#v", events)
	}
}

func TestRequesterRecoveryCanRebuildRequestOutsideRetryLimit(t *testing.T) {
	contextErr := &llm.Error{StatusCode: 400, Code: "context_length_exceeded", Message: "too long"}
	model := &requestScriptModel{chat: func(call int, request llm.Request) (*llm.Response, error) {
		if call == 1 {
			return nil, contextErr
		}
		if got := request.Messages[0].Content; got != "compacted" {
			return nil, errors.New("request was not rebuilt")
		}
		return &llm.Response{Content: "done"}, nil
	}}
	recovered := false
	requester, err := NewRequester(RequesterConfig{Model: model, Hooks: RequestHooks{
		Recover: func(_ context.Context, failure RequestFailure, request *llm.Request) (bool, string, error) {
			if recovered || !errors.Is(failure.Err, contextErr) {
				return false, "", nil
			}
			recovered = true
			request.Messages = []llm.Message{{Role: llm.RoleSystem, Content: "compacted"}}
			return true, "context compacted", nil
		},
	}})
	if err != nil {
		t.Fatal(err)
	}
	response, err := requester.Request(context.Background(), 1, llm.Request{Messages: []llm.Message{{Content: "large"}}}, false, nil)
	if err != nil {
		t.Fatal(err)
	}
	if response.Content != "done" || model.chatCalls != 2 {
		t.Fatalf("response=%#v calls=%d", response, model.chatCalls)
	}
}

func TestRequesterHonorsRetryAfterAgainstBudget(t *testing.T) {
	providerErr := &llm.Error{StatusCode: 429, Message: "slow down", RetryAfter: time.Hour}
	model := &requestScriptModel{chat: func(_ int, _ llm.Request) (*llm.Response, error) {
		return nil, providerErr
	}}
	requester, err := NewRequester(RequesterConfig{Model: model, Policy: RequestPolicy{MaxRetries: -1, RetryBudget: time.Minute, BaseDelay: time.Second}})
	if err != nil {
		t.Fatal(err)
	}
	_, err = requester.Request(context.Background(), 1, llm.Request{}, false, nil)
	if !errors.Is(err, providerErr) || model.chatCalls != 1 {
		t.Fatalf("error=%v calls=%d", err, model.chatCalls)
	}
}

type blockingRequestStream struct {
	closed chan struct{}
	once   sync.Once
}

func newBlockingRequestStream() *blockingRequestStream {
	return &blockingRequestStream{closed: make(chan struct{})}
}

func (s *blockingRequestStream) Recv() (llm.StreamChunk, error) {
	<-s.closed
	return llm.StreamChunk{}, context.Canceled
}

func (s *blockingRequestStream) Usage() llm.Usage { return llm.Usage{} }
func (s *blockingRequestStream) Close() error {
	s.once.Do(func() { close(s.closed) })
	return nil
}

func TestRequesterRetriesIdleStreamWithinCountLimit(t *testing.T) {
	first := newBlockingRequestStream()
	second := newBlockingRequestStream()
	model := &requestScriptModel{streams: []llm.Stream{first, second}}
	requester, err := NewRequester(RequesterConfig{Model: model, Policy: RequestPolicy{MaxRetries: 1, StreamIdleTimeout: 10 * time.Millisecond}})
	if err != nil {
		t.Fatal(err)
	}
	_, err = requester.Request(context.Background(), 1, llm.Request{}, true, nil)
	if !errors.Is(err, ErrStreamIdle) || model.streamCalls != 2 {
		t.Fatalf("error=%v calls=%d", err, model.streamCalls)
	}
}

func TestRequesterRejectsUnsafeUnlimitedRetries(t *testing.T) {
	model := &requestScriptModel{chat: func(_ int, _ llm.Request) (*llm.Response, error) { return nil, nil }}
	for name, policy := range map[string]RequestPolicy{
		"without budget":     {MaxRetries: -1},
		"without base delay": {MaxRetries: -1, RetryBudget: time.Minute},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := NewRequester(RequesterConfig{Model: model, Policy: policy}); err == nil {
				t.Fatal("NewRequester() accepted unsafe unlimited retries")
			}
		})
	}
}

// blockingModel holds every request open until its context ends, which is the
// endpoint that accepts a connection and then does nothing with it.
type blockingModel struct {
	mu    sync.Mutex
	calls int
}

func (m *blockingModel) Chat(ctx context.Context, _ llm.Request) (*llm.Response, error) {
	m.mu.Lock()
	m.calls++
	m.mu.Unlock()
	<-ctx.Done()
	return nil, ctx.Err()
}

func (m *blockingModel) Stream(ctx context.Context, request llm.Request) (llm.Stream, error) {
	_, err := m.Chat(ctx, request)
	return nil, err
}

// The non-streamed path has no idle timeout to fall back on, so before the
// budget bounded the attempt itself nothing here ended short of the transport's
// own timeout. Compaction takes this path.
func TestRequesterBudgetBoundsAnAttemptThatNeverReturns(t *testing.T) {
	model := &blockingModel{}
	requester, err := NewRequester(RequesterConfig{Model: model, Policy: RequestPolicy{MaxRetries: 3, RetryBudget: 50 * time.Millisecond, BaseDelay: time.Millisecond}})
	if err != nil {
		t.Fatalf("NewRequester() error = %v", err)
	}
	if _, err := requester.Request(context.Background(), 1, llm.Request{}, false, nil); !errors.Is(err, ErrRequestBudget) {
		t.Fatalf("Request() error = %v, want %v", err, ErrRequestBudget)
	}
	if model.calls != 1 {
		t.Fatalf("calls = %d, want the budget spent on one attempt and no retry after it", model.calls)
	}
}

// The caller's own cancellation must not be reported as our budget running out:
// one means stop, the other means come back later.
func TestRequesterKeepsACallerCancellationDistinctFromTheBudget(t *testing.T) {
	model := &blockingModel{}
	requester, err := NewRequester(RequesterConfig{Model: model, Policy: RequestPolicy{MaxRetries: 3, RetryBudget: time.Hour, BaseDelay: time.Millisecond}})
	if err != nil {
		t.Fatalf("NewRequester() error = %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(20 * time.Millisecond)
		cancel()
	}()
	_, err = requester.Request(ctx, 1, llm.Request{}, false, nil)
	if !errors.Is(err, context.Canceled) || errors.Is(err, ErrRequestBudget) {
		t.Fatalf("Request() error = %v, want a cancellation", err)
	}
	cancel()
}
