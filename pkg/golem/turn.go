package golem

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/levmv/golems/pkg/llm"
)

// DefaultToolLimitPrompt asks for a final answer once MaxToolIterations is
// spent. Exported rather than merely overridable because a caller that replaces
// it still has to be able to say what the turn actually ran under.
const DefaultToolLimitPrompt = "Tool call limit reached. Do not call any more tools; answer with what you have, and be explicit about anything you could not verify."

type ModelRequestFunc func(ctx context.Context, step int, request llm.Request, stream bool, emit StreamFunc) (*llm.Response, error)

// TurnRuntime customizes the storage and request boundaries around the common
// model/tool state machine. PrepareContext runs before every model request;
// Request may add retry or transport policy; record hooks persist accepted
// boundaries. Complete runs before EventDone. Fail may persist failure state
// and return a replacement error; returning nil preserves the original cause.
// The zero value is an in-memory, direct-model run.
type TurnRuntime struct {
	PrepareContext  func(ctx context.Context, step int, current []llm.Message) ([]llm.Message, error)
	Request         ModelRequestFunc
	RecordAssistant func(message llm.Message, response llm.Response) error
	RecordToolLimit func(calls []llm.ToolCall, messages []llm.Message, steps []Step) error
	ToolExecution   ToolExecutionHooks
	Complete        func(turn *Turn) error
	Fail            func(usage llm.Usage, cause error) error
}

// TurnConfig describes one serialized agent turn. RunTurn appends Input to
// InitialContext before the first request.
type TurnConfig struct {
	Model             Model
	Input             string
	InitialContext    []llm.Message
	Tools             *ToolSet
	ToolChoice        *llm.ToolChoice
	ParallelToolCalls *bool
	MaxToolIterations int
	// ToolLimitPrompt replaces DefaultToolLimitPrompt. Empty keeps it, so a
	// caller with no opinion states none.
	ToolLimitPrompt string
	Stream          bool
	Emit            StreamFunc
	Runtime         TurnRuntime
}

// RunTurn executes the provider-independent model/tool loop. Callers serialize
// concurrent turns when they share conversation state.
func RunTurn(ctx context.Context, cfg TurnConfig) (*Turn, error) {
	input := strings.TrimSpace(cfg.Input)
	if input == "" {
		return nil, ErrEmptyInput
	}
	if cfg.Model == nil && cfg.Runtime.Request == nil {
		return nil, errors.New("golem: model or request function is required")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	toolSet := cfg.Tools.clone()
	maxToolIterations := normalizeMaxToolIterations(cfg.MaxToolIterations)
	toolLimitPrompt := cmp.Or(strings.TrimSpace(cfg.ToolLimitPrompt), DefaultToolLimitPrompt)
	messages := llm.CloneMessages(cfg.InitialContext)
	messages = append(messages, llm.Message{Role: llm.RoleUser, Content: input})
	turnMessages := []llm.Message{{Role: llm.RoleUser, Content: input, CreatedAt: time.Now()}}
	var steps []Step
	var reasoning strings.Builder
	var usage llm.Usage
	var finishReason llm.FinishReason
	toolIterations := 0
	modelStep := 0

	fail := func(cause error) (*Turn, error) {
		if cfg.Runtime.Fail != nil {
			if err := cfg.Runtime.Fail(usage, cause); err != nil {
				return nil, err
			}
		}
		return nil, cause
	}
	prepare := func() error {
		if cfg.Runtime.PrepareContext == nil {
			return nil
		}
		prepared, err := cfg.Runtime.PrepareContext(ctx, modelStep+1, llm.CloneMessages(messages))
		if err != nil {
			return err
		}
		messages = llm.CloneMessages(prepared)
		return nil
	}
	request := func(final bool) (*llm.Response, error) {
		modelStep++
		request := llm.Request{
			Messages:          messages,
			Tools:             toolSet.Definitions(),
			ToolChoice:        llm.CloneToolChoice(cfg.ToolChoice),
			ParallelToolCalls: cloneBool(cfg.ParallelToolCalls),
		}
		if final {
			choice := llm.ToolChoice{Mode: llm.ToolChoiceNone}
			request.ToolChoice = &choice
		}
		stream := cfg.Stream
		if cfg.Runtime.Request != nil {
			response, err := cfg.Runtime.Request(ctx, modelStep, request, stream, cfg.Emit)
			return validateModelResponse(response, err)
		}
		response, err := directModelRequest(ctx, cfg.Model, request, stream, cfg.Emit)
		return validateModelResponse(response, err)
	}
	recordAssistant := func(response *llm.Response) (llm.Message, error) {
		assistant := llm.Message{
			Role:             llm.RoleAI,
			Content:          strings.TrimSpace(response.Content),
			ReasoningContent: strings.TrimSpace(response.ReasoningContent),
			ToolCalls:        llm.CloneToolCalls(response.ToolCalls),
			CreatedAt:        time.Now(),
		}
		usage = usage.Add(response.Usage)
		appendReasoning(&reasoning, response.ReasoningContent)
		finishReason = response.FinishReason
		if cfg.Runtime.RecordAssistant != nil {
			if err := cfg.Runtime.RecordAssistant(assistant, *response); err != nil {
				return llm.Message{}, err
			}
		}
		return assistant, nil
	}
	complete := func(reply llm.Message) (*Turn, error) {
		turn := &Turn{
			Input:        input,
			Reply:        reply.Content,
			Reasoning:    strings.TrimSpace(reasoning.String()),
			Steps:        steps,
			Usage:        usage,
			FinishReason: finishReason,
			messages:     turnMessages,
		}
		if cfg.Runtime.Complete != nil {
			if err := cfg.Runtime.Complete(turn); err != nil {
				return fail(err)
			}
		}
		if cfg.Emit != nil {
			cfg.Emit(StreamEvent{Kind: EventDone, Text: turn.Reply, Usage: turn.Usage, FinishReason: turn.FinishReason})
		}
		return turn, nil
	}

	for {
		if err := prepare(); err != nil {
			return fail(err)
		}
		response, err := request(false)
		if err != nil {
			return fail(err)
		}
		assistant, err := recordAssistant(response)
		if err != nil {
			return fail(err)
		}

		if len(response.ToolCalls) == 0 {
			turnMessages = append(turnMessages, assistant)
			return complete(assistant)
		}

		if maxToolIterations >= 0 && toolIterations >= maxToolIterations {
			limitMessages, limitSteps := toolLimitResults(response.ToolCalls, maxToolIterations)
			steps = append(steps, limitSteps...)
			messages = append(messages, assistant)
			messages = append(messages, limitMessages...)
			turnMessages = append(turnMessages, assistant)
			turnMessages = append(turnMessages, limitMessages...)
			if cfg.Emit != nil {
				for _, step := range limitSteps {
					cfg.Emit(StreamEvent{Kind: EventToolError, Step: step})
				}
			}
			if cfg.Runtime.RecordToolLimit != nil {
				if err := cfg.Runtime.RecordToolLimit(response.ToolCalls, limitMessages, limitSteps); err != nil {
					return fail(err)
				}
			}
			if err := prepare(); err != nil {
				return fail(err)
			}
			messages = append(messages, llm.Message{Role: llm.RoleUser, Content: toolLimitPrompt})
			finalResponse, err := request(true)
			if err != nil {
				return fail(err)
			}
			if strings.TrimSpace(finalResponse.Content) == "" {
				usage = usage.Add(finalResponse.Usage)
				appendReasoning(&reasoning, finalResponse.ReasoningContent)
				return fail(errors.New("golem: model returned no final answer after the tool call limit"))
			}
			finalAnswer := *finalResponse
			finalAnswer.ToolCalls = nil
			finalAssistant, err := recordAssistant(&finalAnswer)
			if err != nil {
				return fail(err)
			}
			turnMessages = append(turnMessages, finalAssistant)
			return complete(finalAssistant)
		}
		toolIterations++

		messages = append(messages, assistant)
		turnMessages = append(turnMessages, assistant)
		toolMessages, toolSteps, err := toolSet.Execute(ctx, response.ToolCalls, cfg.Emit, cfg.Runtime.ToolExecution)
		steps = append(steps, toolSteps...)
		messages = append(messages, toolMessages...)
		turnMessages = append(turnMessages, toolMessages...)
		if err != nil {
			return fail(err)
		}
	}
}

func validateModelResponse(response *llm.Response, err error) (*llm.Response, error) {
	if err != nil {
		return nil, err
	}
	if response == nil {
		return nil, errors.New("golem: model returned nil response")
	}
	if strings.TrimSpace(response.Content) == "" && len(response.ToolCalls) == 0 {
		return nil, errors.New("golem: model returned an empty response")
	}
	return response, nil
}

func toolLimitResults(calls []llm.ToolCall, maxIterations int) ([]llm.Message, []Step) {
	messages := make([]llm.Message, 0, len(calls))
	steps := make([]Step, 0, len(calls))
	message := fmt.Sprintf("tool iteration limit reached after %d iterations", maxIterations)
	for _, call := range calls {
		content := "tool " + call.Function.Name + " error: " + message
		messages = append(messages, llm.Message{Role: llm.RoleTool, Content: content, ToolCallID: call.ID, CreatedAt: time.Now()})
		steps = append(steps, Step{Kind: StepToolError, ToolName: call.Function.Name, ToolCallID: call.ID, Arguments: call.Function.Arguments, Error: message})
	}
	return messages, steps
}

func normalizeMaxToolIterations(value int) int {
	if value == 0 {
		return DefaultMaxToolIterations
	}
	if value < 0 {
		return UnlimitedToolIterations
	}
	return value
}

func appendReasoning(builder *strings.Builder, text string) {
	text = strings.TrimSpace(text)
	if text == "" {
		return
	}
	if builder.Len() > 0 {
		builder.WriteString("\n")
	}
	builder.WriteString(text)
}
