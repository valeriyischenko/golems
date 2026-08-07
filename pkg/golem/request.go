package golem

import (
	"context"
	"errors"
	"fmt"
	"io"
	mathrand "math/rand/v2"
	"strings"
	"time"

	"github.com/levmv/golems/pkg/llm"
)

// ErrStreamIdle is returned when a model stream produces no chunk before the
// configured idle timeout. It is retryable unless the surrounding context has
// ended or the request policy has exhausted its limits.
var ErrStreamIdle = errors.New("model stream idle timeout")

// ErrRequestBudget is returned when an attempt is still running at the end of
// the request's RetryBudget. It is the budget doing its job, so it is not
// retryable: by definition there is nothing left to retry within.
var ErrRequestBudget = errors.New("model request budget exhausted")

// RequestPolicy bounds retries for one logical model request. MaxRetries is the
// number of retries after the first attempt; a negative value means unlimited
// retries and requires a positive RetryBudget and BaseDelay. RetryBudget is the
// wall-clock bound on the whole request, attempts included, so a single attempt
// that never returns cannot outlive it. A zero StreamIdleTimeout leaves stream
// reads unbounded by this layer.
type RequestPolicy struct {
	MaxRetries        int
	RetryBudget       time.Duration
	BaseDelay         time.Duration
	MaxDelay          time.Duration
	StreamIdleTimeout time.Duration
}

// RequestFailure describes one failed model attempt. ProvisionalText contains
// visible text already emitted before failure; HadProvisionalOutput also covers
// reasoning output that a consumer may need to reset.
type RequestFailure struct {
	Step                 int
	Attempt              int
	Err                  error
	ProvisionalText      string
	HadProvisionalOutput bool
}

// RequestHooks attach durable bookkeeping and application-specific recovery to
// a Requester. Recover may mutate request and return true to retry immediately,
// independently of the normal retry allowance. It is responsible for bounding
// its own recovery strategy.
type RequestHooks struct {
	AttemptStarted func(step, attempt int) error
	AttemptFailed  func(failure RequestFailure) error
	Recover        func(ctx context.Context, failure RequestFailure, request *llm.Request) (recovered bool, note string, err error)
}

// Requester executes complete logical model requests, including consuming a
// stream. Unlike a transport retry wrapper, it can discard partial streamed
// output before starting a fresh attempt.
type Requester struct {
	model    Model
	policy   RequestPolicy
	hooks    RequestHooks
	sanitize func(string) string
}

// RequesterConfig describes a reusable logical request executor. Sanitize is
// applied to retry-status text emitted to callers; nil leaves it unchanged.
type RequesterConfig struct {
	Model    Model
	Policy   RequestPolicy
	Hooks    RequestHooks
	Sanitize func(string) string
}

// NewRequester builds a reusable complete-request executor.
func NewRequester(cfg RequesterConfig) (*Requester, error) {
	if cfg.Model == nil {
		return nil, errors.New("golem: model is required")
	}
	if cfg.Policy.MaxRetries < 0 && cfg.Policy.RetryBudget <= 0 {
		return nil, errors.New("golem: unlimited model retries require a positive retry budget")
	}
	if cfg.Policy.MaxRetries < 0 && cfg.Policy.BaseDelay <= 0 {
		return nil, errors.New("golem: unlimited model retries require a positive base delay")
	}
	if cfg.Policy.RetryBudget < 0 || cfg.Policy.BaseDelay < 0 || cfg.Policy.MaxDelay < 0 || cfg.Policy.StreamIdleTimeout < 0 {
		return nil, errors.New("golem: retry durations cannot be negative")
	}
	if cfg.Sanitize == nil {
		cfg.Sanitize = func(text string) string { return text }
	}
	return &Requester{model: cfg.Model, policy: cfg.Policy, hooks: cfg.Hooks, sanitize: cfg.Sanitize}, nil
}

// Request implements ModelRequestFunc.
func (r *Requester) Request(ctx context.Context, step int, request llm.Request, stream bool, emit StreamFunc) (*llm.Response, error) {
	startedAt := time.Now()
	for attempt := 1; ; attempt++ {
		if r.hooks.AttemptStarted != nil {
			if err := r.hooks.AttemptStarted(step, attempt); err != nil {
				return nil, err
			}
		}

		attemptCtx, endAttempt := r.attemptContext(ctx, startedAt)
		response, provisional, hadProvisional, err := requestModelOnce(attemptCtx, r.model, request, stream, emit, r.policy.StreamIdleTimeout)
		response, err = validateModelResponse(response, err)
		endAttempt()
		if err == nil {
			return response, nil
		}
		// The budget ran out inside the attempt rather than between attempts. Say
		// so: what the caller gets otherwise is a bare deadline error that reads
		// like a cancellation, from a deadline it never set.
		err = r.nameBudgetExhaustion(ctx, attemptCtx, err)

		failure := RequestFailure{Step: step, Attempt: attempt, Err: err, ProvisionalText: provisional, HadProvisionalOutput: hadProvisional}
		if r.hooks.AttemptFailed != nil {
			if hookErr := r.hooks.AttemptFailed(failure); hookErr != nil {
				return nil, errors.Join(err, hookErr)
			}
		}
		if emit != nil && hadProvisional {
			emit(StreamEvent{Kind: EventAttemptReset, Text: r.sanitize(provisional)})
		}

		if r.hooks.Recover != nil {
			recovered, note, recoveryErr := r.hooks.Recover(ctx, failure, &request)
			if recoveryErr != nil {
				return nil, errors.Join(err, recoveryErr)
			}
			if recovered {
				if emit != nil && strings.TrimSpace(note) != "" {
					emit(StreamEvent{Kind: EventModelRetry, Text: r.sanitize(note)})
				}
				continue
			}
		}

		if errors.Is(err, ErrRequestBudget) || !llm.IsRetryable(err) || !r.retryAllowed(attempt, startedAt) {
			return nil, err
		}
		delay := r.retryDelay(err, attempt)
		remaining := r.retryRemaining(startedAt)
		if r.policy.RetryBudget > 0 && delay > remaining {
			return nil, err
		}
		if emit != nil {
			cause := r.sanitize(formatRetryCause(err))
			message := fmt.Sprintf("#%d in %s — %s", attempt, delay.Round(time.Millisecond), cause)
			emit(StreamEvent{Kind: EventModelRetry, Text: message, RetryKey: cause})
		}
		if err := waitForRetry(ctx, delay); err != nil {
			return nil, err
		}
	}
}

func formatRetryCause(err error) string {
	var providerErr *llm.Error
	if errors.As(err, &providerErr) {
		provider := strings.TrimSpace(providerErr.Provider)
		if provider == "" {
			provider = "model"
		}
		switch {
		case providerErr.StatusCode > 0 && providerErr.Message != "":
			return fmt.Sprintf("%s %d: %s", provider, providerErr.StatusCode, providerErr.Message)
		case providerErr.Message != "":
			return provider + ": " + providerErr.Message
		case providerErr.StatusCode > 0:
			return fmt.Sprintf("%s %d", provider, providerErr.StatusCode)
		}
	}
	return err.Error()
}

// attemptContext gives one attempt whatever is left of the request's budget.
// Without it the budget bounds only the gaps between attempts, so an endpoint
// that accepts a request and then holds it open is unbounded by this layer on
// the streamed path (StreamIdleTimeout covers that one) and unbounded entirely
// on the non-streamed path, whatever the caller configured.
func (r *Requester) attemptContext(ctx context.Context, startedAt time.Time) (context.Context, context.CancelFunc) {
	if r.policy.RetryBudget <= 0 {
		return ctx, func() {}
	}
	return context.WithDeadline(ctx, startedAt.Add(r.policy.RetryBudget))
}

// nameBudgetExhaustion distinguishes our deadline from the caller's. Only the
// attempt context being done makes it ours; if the caller's context ended too,
// that is a cancellation and stays one.
func (r *Requester) nameBudgetExhaustion(ctx, attemptCtx context.Context, err error) error {
	if !errors.Is(attemptCtx.Err(), context.DeadlineExceeded) || ctx.Err() != nil {
		return err
	}
	return fmt.Errorf("%w after %s", ErrRequestBudget, r.policy.RetryBudget)
}

func (r *Requester) retryAllowed(attempt int, startedAt time.Time) bool {
	if r.policy.MaxRetries >= 0 && attempt > r.policy.MaxRetries {
		return false
	}
	return r.policy.RetryBudget <= 0 || time.Since(startedAt) < r.policy.RetryBudget
}

func (r *Requester) retryRemaining(startedAt time.Time) time.Duration {
	if r.policy.RetryBudget <= 0 {
		return 0
	}
	return max(time.Duration(0), r.policy.RetryBudget-time.Since(startedAt))
}

func (r *Requester) retryDelay(err error, attempt int) time.Duration {
	delay := exponentialDelay(r.policy.BaseDelay, r.policy.MaxDelay, attempt)
	var providerErr *llm.Error
	if errors.As(err, &providerErr) && providerErr.RetryAfter > delay {
		// Retry-After is the provider's minimum acceptable delay. Do not clamp it
		// to MaxDelay; the overall RetryBudget decides whether it still fits.
		delay = providerErr.RetryAfter
	}
	return delay
}

func exponentialDelay(base, maximum time.Duration, attempt int) time.Duration {
	if base <= 0 {
		return 0
	}
	limit := time.Duration(1<<63 - 1)
	if maximum > 0 {
		limit = maximum
	}
	delay := min(base, limit)
	for step := 1; step < attempt && delay < limit; step++ {
		if delay > limit/2 {
			delay = limit
			break
		}
		delay *= 2
	}
	if jitter := delay / 4; jitter > 0 {
		jitter = mathrand.N(jitter)
		if delay > limit-jitter {
			return limit
		}
		delay += jitter
	}
	return delay
}

func waitForRetry(ctx context.Context, delay time.Duration) error {
	if delay <= 0 {
		if err := ctx.Err(); err != nil {
			return err
		}
		return nil
	}
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func directModelRequest(ctx context.Context, model Model, request llm.Request, stream bool, emit StreamFunc) (*llm.Response, error) {
	response, _, _, err := requestModelOnce(ctx, model, request, stream, emit, 0)
	return response, err
}

func requestModelOnce(ctx context.Context, model Model, request llm.Request, stream bool, emit StreamFunc, idleTimeout time.Duration) (*llm.Response, string, bool, error) {
	if !stream {
		response, err := model.Chat(ctx, request)
		return response, "", false, err
	}
	streamCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	modelStream, err := model.Stream(streamCtx, request)
	if err != nil {
		return nil, "", false, err
	}
	return consumeModelStream(streamCtx, cancel, modelStream, emit, idleTimeout)
}

func consumeModelStream(ctx context.Context, cancel context.CancelFunc, stream llm.Stream, emit StreamFunc, idleTimeout time.Duration) (*llm.Response, string, bool, error) {
	if stream == nil {
		return nil, "", false, errors.New("golem: model returned nil stream")
	}
	defer stream.Close()

	var content strings.Builder
	var reasoning strings.Builder
	var toolCalls []llm.ToolCall
	var finishReason llm.FinishReason
	receive := stream.Recv
	if idleTimeout > 0 {
		results := make(chan streamReceiveResult, 1)
		go receiveModelStream(ctx, stream, results)
		timer := time.NewTimer(idleTimeout)
		defer timer.Stop()
		first := true
		receive = func() (llm.StreamChunk, error) {
			if first {
				first = false
			} else {
				timer.Reset(idleTimeout)
			}
			select {
			case value := <-results:
				if !timer.Stop() {
					select {
					case <-timer.C:
					default:
					}
				}
				return value.chunk, value.err
			case <-timer.C:
				cancel()
				_ = stream.Close()
				return llm.StreamChunk{}, fmt.Errorf("%w after %s", ErrStreamIdle, idleTimeout)
			case <-ctx.Done():
				return llm.StreamChunk{}, ctx.Err()
			}
		}
	}
	for {
		chunk, err := receive()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, content.String(), content.Len() > 0 || reasoning.Len() > 0, err
		}
		if chunk.Text != "" {
			content.WriteString(chunk.Text)
			if emit != nil {
				emit(StreamEvent{Kind: EventTextDelta, Text: chunk.Text})
			}
		}
		if chunk.ReasoningContent != "" {
			reasoning.WriteString(chunk.ReasoningContent)
			if emit != nil {
				emit(StreamEvent{Kind: EventReasoningDelta, Text: chunk.ReasoningContent})
			}
		}
		if len(chunk.ToolCalls) > 0 {
			toolCalls = llm.CloneToolCalls(chunk.ToolCalls)
		}
		if chunk.FinishReason != "" {
			finishReason = chunk.FinishReason
		}
	}
	return &llm.Response{
		Content:          content.String(),
		ReasoningContent: reasoning.String(),
		ToolCalls:        toolCalls,
		FinishReason:     finishReason,
		Usage:            stream.Usage(),
	}, content.String(), content.Len() > 0 || reasoning.Len() > 0, nil
}

type streamReceiveResult struct {
	chunk llm.StreamChunk
	err   error
}

func receiveModelStream(ctx context.Context, stream llm.Stream, results chan<- streamReceiveResult) {
	for {
		chunk, err := stream.Recv()
		select {
		case results <- streamReceiveResult{chunk: chunk, err: err}:
		case <-ctx.Done():
			return
		}
		if err != nil {
			return
		}
	}
}
