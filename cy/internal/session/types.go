package session

import (
	"encoding/json"
	"time"

	"github.com/levmv/golems/pkg/llm"
)

type RecordType string

const (
	RecordSessionStarted      RecordType = "session_started"
	RecordSessionConfigured   RecordType = "session_configured"
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

// SandboxState is the fence the session's tools run behind, as it was decided
// rather than as it was asked for. Policy is the request; Effective is what the
// request resolved to; Backend names the mechanism that is actually in place and
// is empty when there is none. Probe says why there is none, where a fence was
// wanted and did not happen, and Container names the outer isolation Cy decided
// to trust instead of an inner one.
//
// All five, because "auto" can silently mean "no fence" and the four other
// fields are the difference between a run that was isolated and a run that only
// asked to be.
type SandboxState struct {
	Policy    string `json:"policy,omitempty"`
	Effective string `json:"effective_policy,omitempty"`
	Backend   string `json:"backend,omitempty"`
	Probe     string `json:"probe,omitempty"`
	Container string `json:"container,omitempty"`
}

// SessionConfigured is what the model is working with: the prompts it was given,
// the tools it may call, and the fence those tools run behind. It is written
// when a session starts and again whenever any of the three changes.
//
// It is not part of session_started, tempting as that is. The tool catalog
// changes mid-session when a capability profile is switched, the sandbox
// changes on /sandbox, and a resume can start the process with a different
// system prompt than the one the session began under. Written once into the
// header, all three would be recorded and then quietly go stale.
//
// It is recorded at all because otherwise a replay cannot say what the model
// saw. The conversation shows the tools that were called but not the ones that
// were offered, and the answers but not the prompt they were given under; a run
// that went wrong because a tool was absent, or because the fence was not there,
// reads exactly like one that did not.
//
// Unredacted, unlike a boundary event. That masking exists to keep a credential
// the model never saw out of the file; these are strings the model was handed
// verbatim, and a journal that disagreed with them would be describing a
// different run.
type SessionConfigured struct {
	SystemPrompt       string       `json:"system_prompt"`
	InstructionPrompts []string     `json:"instruction_prompts,omitempty"`
	Tools              []llm.Tool   `json:"tools,omitempty"`
	Sandbox            SandboxState `json:"sandbox"`
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
