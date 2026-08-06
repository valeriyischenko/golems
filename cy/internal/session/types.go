package session

import (
	"encoding/json"
	"time"

	"github.com/levmv/golems/pkg/llm"
)

type RecordType string

const (
	RecordSessionStarted      RecordType = "session_started"
	RecordModelChanged        RecordType = "model_changed"
	RecordUserMessage         RecordType = "user_message"
	RecordAssistantMessage    RecordType = "assistant_message"
	RecordToolResult          RecordType = "tool_result"
	RecordRunFinished         RecordType = "run_finished"
	RecordToolResultsPruned   RecordType = "tool_results_pruned"
	RecordCompactionCompleted RecordType = "compaction_completed"
	RecordBoundaryEvent       RecordType = "boundary_event"
)

// Record is one append-only session event. Sequence numbers are sufficient to
// detect a damaged or reordered local journal; records do not need their own
// globally unique identity.
type Record struct {
	Seq       uint64          `json:"seq"`
	Timestamp time.Time       `json:"timestamp"`
	Type      RecordType      `json:"type"`
	Payload   json.RawMessage `json:"payload"`
}

type SessionStarted struct {
	Workspace       string `json:"workspace"`
	Model           string `json:"model"`
	ReasoningEffort string `json:"reasoning_effort"`
}

type ModelChanged struct {
	Model           string `json:"model"`
	ReasoningEffort string `json:"reasoning_effort"`
}

type UserMessage struct {
	RunID   string `json:"run_id"`
	Content string `json:"content"`
}

type AssistantMessage struct {
	RunID     string         `json:"run_id"`
	Content   string         `json:"content"`
	Reasoning string         `json:"reasoning,omitempty"`
	ToolCalls []llm.ToolCall `json:"tool_calls,omitempty"`
	Usage     llm.Usage      `json:"usage"`
}

type ToolResult struct {
	RunID      string `json:"run_id"`
	ToolCallID string `json:"tool_call_id"`
	Content    string `json:"content"`
	Meta       any    `json:"meta,omitempty"`
}

// RunOutcome says how a run ended. A record written before Cy recorded
// outcomes carries none; that is distinct from a run that ended well.
type RunOutcome string

const (
	RunCompleted   RunOutcome = "completed"
	RunFailed      RunOutcome = "failed"
	RunInterrupted RunOutcome = "interrupted"
)

// RunFinished closes a run. It carries how the run ended because the journal
// is the whole account of it: without an outcome a crashed run, a cancelled
// one, and a finished one are the same record, and a reader is left to infer
// the difference from what is missing.
type RunFinished struct {
	RunID   string     `json:"run_id"`
	Outcome RunOutcome `json:"outcome"`
	Error   string     `json:"error,omitempty"`
}

// ToolResultsPruned records a provider-facing context boundary. Original tool
// result records remain untouched in the journal.
type ToolResultsPruned struct {
	ThroughSeq uint64 `json:"through_seq"`
	HeadBytes  int    `json:"head_bytes"`
	TailBytes  int    `json:"tail_bytes"`
}

type CompactionCompleted struct {
	CoveredThroughSeq uint64    `json:"covered_through_seq"`
	FirstVerbatimSeq  uint64    `json:"first_verbatim_seq"`
	Summary           string    `json:"summary"`
	Usage             llm.Usage `json:"usage"`
}

// BoundaryEvent is a message put into context at a turn boundary by Cy itself,
// rather than by the user or the model. Today the only source is a background
// job reporting that it finished, which the model has to be told about because
// nothing in the conversation would otherwise mention it.
//
// It is a record because the journal is the whole account of the run. Delivered
// and not recorded, such a message is in the model's context on the turn it is
// delivered and gone from every reconstruction afterwards, so a replay shows the
// model reacting to something it was never told.
//
// JobID names the job the event reports on. It is separate from Content rather
// than only spelled inside it so that a delivery can be matched to the tool call
// that started the job — the call's own record already carries the id in its
// result metadata — without parsing prose.
type BoundaryEvent struct {
	RunID   string `json:"run_id"`
	JobID   string `json:"job_id,omitempty"`
	Content string `json:"content"`
}

func DecodePayload[T any](record Record) (T, error) {
	var payload T
	err := json.Unmarshal(record.Payload, &payload)
	return payload, err
}
