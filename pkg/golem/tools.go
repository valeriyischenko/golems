package golem

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/levmv/golems/pkg/jsonschema"
	"github.com/levmv/golems/pkg/llm"
)

type ToolFunc func(ctx context.Context, call llm.ToolCall) (ToolResult, error)

// ErrToolFatal marks a tool failure that calling again cannot fix: the tool
// could not be started at all, so every retry fails the same way. An ordinary
// tool error is handed to the model to work around, which is right when the
// tool ran and disagreed with its arguments and wrong when it never ran -- an
// unattended chain will spend the rest of its budget being resourceful about a
// mistake in configuration. A runtime that watches tool outcomes stops the run
// on this; the result is still recorded first, so what happened is on file.
var ErrToolFatal = errors.New("tool cannot run")

type ToolResult struct {
	Content string
	Meta    any
}

type ToolEffect string

const (
	ToolEffectRead     ToolEffect = "read"
	ToolEffectWrite    ToolEffect = "write"
	ToolEffectProcess  ToolEffect = "process"
	ToolEffectExternal ToolEffect = "external"
)

type Tool struct {
	Definition llm.Tool
	Run        ToolFunc
	// Effect describes the strongest side effect of a call. Only tools marked
	// read may be scheduled concurrently; an empty effect remains serial.
	Effect ToolEffect
}

func FunctionTool(name, description string, parameters jsonschema.Schema, run ToolFunc) Tool {
	return Tool{
		Definition: llm.Tool{
			Type: llm.ToolTypeFunction,
			Function: llm.ToolDefinition{
				Name:        name,
				Description: description,
				Parameters:  parameters,
			},
		},
		Run: run,
	}
}

func FunctionToolWithEffect(effect ToolEffect, name, description string, parameters jsonschema.Schema, run ToolFunc) Tool {
	tool := FunctionTool(name, description, parameters, run)
	tool.Effect = effect
	return tool
}

// ToolSet owns a validated tool catalog and executes model tool calls. It is
// shared by the in-memory Agent and durable runtimes that persist execution
// around the common scheduler.
type ToolSet struct {
	definitions []llm.Tool
	executors   map[string]toolExecutor
}

// ToolExecutionHooks let a durable runtime record tool boundaries without
// reimplementing validation, error conversion, or parallel read scheduling.
type ToolExecutionHooks struct {
	Before func(call llm.ToolCall) error
	After  func(execution ToolExecution) error
}

// ToolExecution describes one completed call passed to an After hook.
type ToolExecution struct {
	Call        llm.ToolCall
	Result      ToolResult
	Err         error
	StartedAt   time.Time
	CompletedAt time.Time
	Message     llm.Message
	Step        Step
}

type toolExecutor struct {
	run    ToolFunc
	effect ToolEffect
}

// NewToolSet validates tools and builds an executable catalog.
func NewToolSet(tools []Tool) (*ToolSet, error) {
	set := &ToolSet{executors: make(map[string]toolExecutor, len(tools))}
	for _, tool := range tools {
		if err := set.add(tool); err != nil {
			return nil, err
		}
	}
	return set, nil
}

func (s *ToolSet) add(tool Tool) error {
	definition, executor, err := normalizeTool(tool)
	if err != nil {
		return err
	}
	if s.executors == nil {
		s.executors = make(map[string]toolExecutor)
	}
	name := definition.Function.Name
	if _, exists := s.executors[name]; exists {
		return fmt.Errorf("golem: duplicate tool %q", name)
	}
	s.definitions = append(s.definitions, definition)
	s.executors[name] = executor
	return nil
}

// Definitions returns a copy of the model-visible tool catalog.
func (s *ToolSet) Definitions() []llm.Tool {
	if s == nil {
		return nil
	}
	out := make([]llm.Tool, len(s.definitions))
	copy(out, s.definitions)
	return out
}

func (s *ToolSet) clone() *ToolSet {
	if s == nil {
		return &ToolSet{executors: make(map[string]toolExecutor)}
	}
	out := &ToolSet{
		definitions: s.Definitions(),
		executors:   make(map[string]toolExecutor, len(s.executors)),
	}
	for name, executor := range s.executors {
		out.executors[name] = executor
	}
	return out
}

// Execute runs calls in source order. Consecutive read-only calls run in
// parallel; all other calls remain serial. Tool failures become tool-result
// messages, while context and hook failures stop the batch.
func (s *ToolSet) Execute(ctx context.Context, calls []llm.ToolCall, emit StreamFunc, hooks ToolExecutionHooks) ([]llm.Message, []Step, error) {
	messages := make([]llm.Message, 0, len(calls))
	steps := make([]Step, 0, len(calls)*2)

	for start := 0; start < len(calls); {
		if err := ctx.Err(); err != nil {
			return messages, steps, err
		}
		end := start + 1
		if executor, ok := s.executors[calls[start].Function.Name]; ok && executor.effect == ToolEffectRead {
			for end < len(calls) {
				next, ok := s.executors[calls[end].Function.Name]
				if !ok || next.effect != ToolEffectRead {
					break
				}
				end++
			}
		}

		batch := calls[start:end]
		startedAt := make([]time.Time, len(batch))
		for index, call := range batch {
			if hooks.Before != nil {
				if err := hooks.Before(call); err != nil {
					return messages, steps, err
				}
			}
			startedAt[index] = time.Now()
			if emit != nil {
				emit(StreamEvent{Kind: EventToolCall, Step: callStep(call)})
			}
		}

		outcomes := make([]ToolExecution, len(batch))
		if len(batch) == 1 {
			outcomes[0] = s.executeOne(ctx, batch[0], startedAt[0])
		} else {
			var wg sync.WaitGroup
			wg.Add(len(batch))
			for index, call := range batch {
				go func() {
					defer wg.Done()
					outcomes[index] = s.executeOne(ctx, call, startedAt[index])
				}()
			}
			wg.Wait()
		}

		for _, outcome := range outcomes {
			if hooks.After != nil {
				if err := hooks.After(outcome); err != nil {
					return messages, steps, err
				}
			}
			steps = append(steps, callStep(outcome.Call), outcome.Step)
			messages = append(messages, outcome.Message)
			if emit != nil {
				eventKind := EventToolResult
				if outcome.Step.Kind == StepToolError {
					eventKind = EventToolError
				}
				emit(StreamEvent{Kind: eventKind, Step: outcome.Step})
			}
		}
		if err := ctx.Err(); err != nil {
			return messages, steps, err
		}
		start = end
	}

	return messages, steps, nil
}

func (s *ToolSet) executeOne(ctx context.Context, call llm.ToolCall, startedAt time.Time) ToolExecution {
	executor, ok := s.executors[call.Function.Name]
	var result ToolResult
	var err error
	if !ok {
		err = fmt.Errorf("unknown tool %q", call.Function.Name)
	} else {
		result, err = executor.run(ctx, call)
	}
	completedAt := time.Now()
	message, step := toolResult(call, result, err)
	message.CreatedAt = completedAt
	return ToolExecution{
		Call:        call,
		Result:      result,
		Err:         err,
		StartedAt:   startedAt,
		CompletedAt: completedAt,
		Message:     message,
		Step:        step,
	}
}

func normalizeTool(tool Tool) (llm.Tool, toolExecutor, error) {
	definition := tool.Definition
	if definition.Type == "" {
		definition.Type = llm.ToolTypeFunction
	}

	definition.Function.Name = strings.TrimSpace(definition.Function.Name)
	if definition.Function.Name == "" {
		return llm.Tool{}, toolExecutor{}, errors.New("golem: tool name is required")
	}
	if tool.Run == nil {
		return llm.Tool{}, toolExecutor{}, fmt.Errorf("golem: tool %q run function is required", definition.Function.Name)
	}

	return definition, toolExecutor{run: tool.Run, effect: tool.Effect}, nil
}

func toolResult(call llm.ToolCall, result ToolResult, err error) (llm.Message, Step) {
	if err != nil {
		content := fmt.Sprintf("tool %s error: %v", call.Function.Name, err)
		return llm.Message{Role: llm.RoleTool, Content: content, ToolCallID: call.ID}, Step{
			Kind:       StepToolError,
			ToolName:   call.Function.Name,
			ToolCallID: call.ID,
			Arguments:  call.Function.Arguments,
			Error:      err.Error(),
		}
	}
	return llm.Message{Role: llm.RoleTool, Content: result.Content, ToolCallID: call.ID, Meta: result.Meta}, Step{
		Kind:       StepToolResult,
		ToolName:   call.Function.Name,
		ToolCallID: call.ID,
		Arguments:  call.Function.Arguments,
		Result:     result.Content,
		Meta:       result.Meta,
	}
}

func callStep(call llm.ToolCall) Step {
	return Step{Kind: StepToolCall, ToolName: call.Function.Name, ToolCallID: call.ID, Arguments: call.Function.Arguments}
}
