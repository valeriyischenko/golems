package session

import (
	"fmt"
	"sort"
	"strings"

	"github.com/levmv/golems/pkg/llm"
)

type PendingTool struct {
	Seq   uint64
	RunID string
	Call  llm.ToolCall
}

type State struct {
	Header          SessionStarted
	Configured      *SessionConfigured
	Title           string
	HasUserTurn     bool
	Model           string
	ReasoningEffort string
	Messages        []llm.Message
	MessageSeqs     []uint64
	MessageRunIDs   []string
	Usage           llm.Usage
	ToolPruning     *ToolResultsPruned
	Compaction      *CompactionCompleted
	CompactionCount int
	ActiveRuns      map[string]struct{}
	PendingTools    []PendingTool
	// DeliveredJobs are the jobs whose completion the model has already been
	// told about. A completion is delivered exactly once, and the only durable
	// record that it was is the boundary event; a job registry on disk can say
	// a job finished but not whether anyone heard.
	DeliveredJobs map[string]struct{}
	LastSeq       uint64
}

func (s *Session) Replay() (State, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.replayValid {
		return cloneState(s.replay), nil
	}
	records, err := s.recordsLocked()
	if err != nil {
		return State{}, err
	}
	if len(records) == 0 {
		return State{}, fmt.Errorf("session %s has an empty journal", s.id)
	}
	if records[0].Type != RecordSessionStarted {
		return State{}, fmt.Errorf("session %s journal starts with %s, want %s", s.id, records[0].Type, RecordSessionStarted)
	}

	state := State{ActiveRuns: make(map[string]struct{})}
	for _, record := range records {
		if err := applyRecord(&state, record); err != nil {
			return State{}, err
		}
	}
	sort.Slice(state.PendingTools, func(i, j int) bool {
		return state.PendingTools[i].Seq < state.PendingTools[j].Seq
	})
	s.replay = cloneState(state)
	s.replayValid = true
	s.hasUserTurn = state.HasUserTurn
	return state, nil
}

func applyRecord(state *State, record Record) error {
	if state.ActiveRuns == nil {
		state.ActiveRuns = make(map[string]struct{})
	}
	if state.DeliveredJobs == nil {
		state.DeliveredJobs = make(map[string]struct{})
	}
	switch record.Type {
	case RecordSessionStarted:
		if state.LastSeq != 0 {
			return fmt.Errorf("session journal has more than one header")
		}
		payload, err := DecodePayload[SessionStarted](record)
		if err != nil {
			return decodeError(record, err)
		}
		state.Header = payload
		state.Model = payload.Model
		state.ReasoningEffort = payload.ReasoningEffort
	case RecordSessionConfigured:
		payload, err := DecodePayload[SessionConfigured](record)
		if err != nil {
			return decodeError(record, err)
		}
		// The engine substitutes its default when none is configured, so an
		// empty prompt here is a damaged record rather than a session that ran
		// without one.
		if strings.TrimSpace(payload.SystemPrompt) == "" {
			return fmt.Errorf("session configuration at sequence %d has no system prompt", record.Seq)
		}
		copy := payload
		state.Configured = &copy
	case RecordModelChanged:
		payload, err := DecodePayload[ModelChanged](record)
		if err != nil {
			return decodeError(record, err)
		}
		state.Model = payload.Model
		state.ReasoningEffort = payload.ReasoningEffort
	case RecordUserMessage:
		payload, err := DecodePayload[UserMessage](record)
		if err != nil {
			return decodeError(record, err)
		}
		state.ActiveRuns[payload.RunID] = struct{}{}
		state.HasUserTurn = true
		if state.Title == "" {
			state.Title = titleFromContent(payload.Content)
		}
		state.Messages = append(state.Messages, llm.Message{Role: llm.RoleUser, Content: payload.Content, CreatedAt: record.Timestamp})
		state.MessageSeqs = append(state.MessageSeqs, record.Seq)
		state.MessageRunIDs = append(state.MessageRunIDs, payload.RunID)
	case RecordAssistantMessage:
		payload, err := DecodePayload[AssistantMessage](record)
		if err != nil {
			return decodeError(record, err)
		}
		state.Messages = append(state.Messages, llm.Message{
			Role:             llm.RoleAI,
			Content:          payload.Content,
			ReasoningContent: payload.Reasoning,
			ToolCalls:        llm.CloneToolCalls(payload.ToolCalls),
			CreatedAt:        record.Timestamp,
		})
		state.MessageSeqs = append(state.MessageSeqs, record.Seq)
		state.MessageRunIDs = append(state.MessageRunIDs, payload.RunID)
		for _, call := range payload.ToolCalls {
			state.PendingTools = append(state.PendingTools, PendingTool{
				Seq:   record.Seq,
				RunID: payload.RunID,
				Call:  call,
			})
		}
		state.Usage = state.Usage.Add(payload.Usage)
	case RecordToolResult:
		payload, err := DecodePayload[ToolResult](record)
		if err != nil {
			return decodeError(record, err)
		}
		removePendingTool(state, payload.RunID, payload.ToolCallID)
		state.Messages = append(state.Messages, llm.Message{
			Role:       llm.RoleTool,
			Content:    payload.Content,
			ToolCallID: payload.ToolCallID,
			Meta:       payload.Meta,
			CreatedAt:  record.Timestamp,
		})
		state.MessageSeqs = append(state.MessageSeqs, record.Seq)
		state.MessageRunIDs = append(state.MessageRunIDs, payload.RunID)
	case RecordRunFinished:
		payload, err := DecodePayload[RunFinished](record)
		if err != nil {
			return decodeError(record, err)
		}
		delete(state.ActiveRuns, payload.RunID)
	case RecordToolResultsPruned:
		payload, err := DecodePayload[ToolResultsPruned](record)
		if err != nil {
			return decodeError(record, err)
		}
		if payload.ThroughSeq == 0 || payload.ThroughSeq >= record.Seq || payload.HeadBytes <= 0 || payload.TailBytes <= 0 {
			return fmt.Errorf("invalid tool pruning boundary at sequence %d", record.Seq)
		}
		if state.ToolPruning != nil && payload.ThroughSeq <= state.ToolPruning.ThroughSeq {
			return fmt.Errorf("tool pruning boundary at sequence %d does not advance", record.Seq)
		}
		copy := payload
		state.ToolPruning = &copy
	case RecordBoundaryEvent:
		payload, err := DecodePayload[BoundaryEvent](record)
		if err != nil {
			return decodeError(record, err)
		}
		if strings.TrimSpace(payload.Content) == "" {
			return fmt.Errorf("boundary event at sequence %d has no content", record.Seq)
		}
		state.Messages = append(state.Messages, llm.Message{Role: llm.RoleSystem, Content: payload.Content, CreatedAt: record.Timestamp})
		state.MessageSeqs = append(state.MessageSeqs, record.Seq)
		state.MessageRunIDs = append(state.MessageRunIDs, payload.RunID)
		if payload.JobID != "" {
			state.DeliveredJobs[payload.JobID] = struct{}{}
		}
	case RecordCompactionCompleted:
		payload, err := DecodePayload[CompactionCompleted](record)
		if err != nil {
			return decodeError(record, err)
		}
		if strings.TrimSpace(payload.Summary) == "" || payload.FirstVerbatimSeq == 0 {
			return fmt.Errorf("compaction at sequence %d is incomplete", record.Seq)
		}
		copy := payload
		state.Compaction = &copy
		state.CompactionCount++
		state.Usage = state.Usage.Add(payload.Usage)
	default:
		return fmt.Errorf("session journal has unknown record type %q at sequence %d", record.Type, record.Seq)
	}
	state.LastSeq = record.Seq
	return nil
}

func removePendingTool(state *State, runID, callID string) {
	key := toolKey(runID, callID)
	for index, pending := range state.PendingTools {
		if toolKey(pending.RunID, pending.Call.ID) == key {
			state.PendingTools = append(state.PendingTools[:index], state.PendingTools[index+1:]...)
			return
		}
	}
}

func cloneState(state State) State {
	cloned := state
	cloned.Messages = llm.CloneMessages(state.Messages)
	cloned.MessageSeqs = append([]uint64(nil), state.MessageSeqs...)
	cloned.MessageRunIDs = append([]string(nil), state.MessageRunIDs...)
	if state.Configured != nil {
		configured := *state.Configured
		configured.InstructionPrompts = append([]string(nil), state.Configured.InstructionPrompts...)
		configured.Tools = append([]llm.Tool(nil), state.Configured.Tools...)
		cloned.Configured = &configured
	}
	if state.Compaction != nil {
		compaction := *state.Compaction
		cloned.Compaction = &compaction
	}
	if state.ToolPruning != nil {
		pruning := *state.ToolPruning
		cloned.ToolPruning = &pruning
	}
	cloned.ActiveRuns = make(map[string]struct{}, len(state.ActiveRuns))
	for runID := range state.ActiveRuns {
		cloned.ActiveRuns[runID] = struct{}{}
	}
	cloned.DeliveredJobs = make(map[string]struct{}, len(state.DeliveredJobs))
	for jobID := range state.DeliveredJobs {
		cloned.DeliveredJobs[jobID] = struct{}{}
	}
	cloned.PendingTools = append([]PendingTool(nil), state.PendingTools...)
	return cloned
}

// Reconcile never replays a tool after a crash. It records an explicit result
// with an unknown outcome, then closes any unfinished run.
func (s *Session) Reconcile() error {
	state, err := s.Replay()
	if err != nil {
		return err
	}
	for _, pending := range state.PendingTools {
		content := fmt.Sprintf("tool %s outcome is unknown after an interrupted session; the call was not replayed", pending.Call.Function.Name)
		if _, err := s.Append(RecordToolResult, ToolResult{
			RunID:      pending.RunID,
			ToolCallID: pending.Call.ID,
			Content:    content,
		}); err != nil {
			return err
		}
	}

	runIDs := make([]string, 0, len(state.ActiveRuns))
	for runID := range state.ActiveRuns {
		runIDs = append(runIDs, runID)
	}
	sort.Strings(runIDs)
	for _, runID := range runIDs {
		// The run is being closed by the next process to open the journal, so
		// whatever it was doing did not finish.
		if _, err := s.Append(RecordRunFinished, RunFinished{RunID: runID, Outcome: RunInterrupted, Error: "session ended before the run finished"}); err != nil {
			return err
		}
	}
	return nil
}

func toolKey(runID, callID string) string {
	return runID + "\x00" + callID
}

func decodeError(record Record, err error) error {
	return fmt.Errorf("decode %s payload at sequence %d: %w", record.Type, record.Seq, err)
}
