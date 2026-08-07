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
	// Grants is what configuration added to the fence for every tool this run,
	// as resolved: absolute paths, not the relative ones the file was written
	// with. That resolution is the part a reader cannot redo, since it depended
	// on where the tools file was and who ran Cy.
	Grants SandboxGrants `json:"grants,omitzero"`
}

// SandboxGrants is the paths configuration added to, and took away from, a
// fence. It repeats three fields the runtime also has rather than sharing them,
// for the reason SandboxState and SecurityState stay apart: one is a decision
// with machinery behind it, this is what a reader of the file needs afterwards.
type SandboxGrants struct {
	Read  []string `json:"read,omitempty"`
	Write []string `json:"write,omitempty"`
	Hide  []string `json:"hide,omitempty"`
}

// SessionConfigured is what the model is working with: the prompts it was given,
// the tools it may call, the fence those tools run behind, and the bounds the
// run is held to. It is written when a session starts and again whenever any of
// them changes.
//
// It is not part of session_started, tempting as that is. The tool catalog
// changes mid-session when a capability profile is switched, the sandbox
// changes on /sandbox, the context window changes with the model, and a resume
// can start the process with a different system prompt, or different flags,
// than the session began under. Written once into the header, all of it would
// be recorded and then quietly go stale.
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
	SystemPrompt       string   `json:"system_prompt"`
	InstructionPrompts []string `json:"instruction_prompts,omitempty"`
	// CompactionPrompt and ToolLimitPrompt are the two prompts Cy sends on its
	// own account rather than the user's, recorded for the same reason as the
	// system prompt and without omitempty for the same one: a reader should not
	// have to know which build's built-in wording applied. The compaction prompt
	// is the one that earns it -- it decides what a summary keeps, and that
	// summary becomes the whole of the history behind it.
	CompactionPrompt string          `json:"compaction_prompt"`
	ToolLimitPrompt  string          `json:"tool_limit_prompt"`
	Tools            []llm.Tool      `json:"tools,omitempty"`
	Sandbox          SandboxState    `json:"sandbox"`
	Settings         SessionSettings `json:"settings"`
	// ExternalTools describes the ones among Tools that came from
	// configuration. Same set, seen from the other side: Tools is what the
	// model was shown, this is what those calls actually ran.
	ExternalTools []ExternalToolConfig `json:"external_tools,omitempty"`
}

// ExternalToolConfig is one configured tool as the run resolved it, not as the
// file wrote it: the program after the PATH lookup, the timeout after the
// default and the ceiling. Recorded because a tool declared outside the binary
// is the one thing a reader of the journal cannot look up in the source -- the
// same session file, replayed against a different tools file, is a different
// run, and nothing else in the journal would say so.
//
// The declared schema is not repeated here; it is already on the matching entry
// in Tools, which is what the model was shown.
//
// Environment names without their values, unlike everything else in
// SessionConfigured. The rest is what the model was handed and has to be
// recorded verbatim; a variable set for a tool is neither -- it is a way to
// pass the tool a token, and writing it here would put a credential in a file
// the model never saw it in. The names are kept because a tool that behaves
// differently on two machines usually differs by exactly one of them.
type ExternalToolConfig struct {
	Name string `json:"name"`
	// Effect decides whether calls run concurrently and whether a restricted
	// profile exposes the tool, so it changes behaviour that nothing else here
	// would explain.
	Effect  string   `json:"effect"`
	Program string   `json:"program"`
	Command []string `json:"command"`
	Workdir string   `json:"workdir,omitempty"`
	Timeout string   `json:"timeout,omitempty"`
	// Background and Yield say whether a call on this tool can end with the
	// tool still running, which changes both the schema the model was shown and
	// what a result means. Absent is the tool always being waited for.
	Background string `json:"background,omitempty"`
	Yield      string `json:"yield,omitempty"`
	// Detach says the tool's work may still be running after Cy exits, and so
	// that a later run adopting a job of this session can be traced back to the
	// setting that let it happen.
	Detach   bool     `json:"detach,omitempty"`
	EnvNames []string `json:"env_names,omitempty"`
	// Sandbox is what this tool asked for beyond the run-level grants, which is
	// the one thing about a configured tool's fence that differs from every
	// other tool's and from Bash's.
	Sandbox SandboxGrants `json:"sandbox,omitzero"`
}

// SessionSettings are the operational bounds the session runs under: where the
// model was reached, how much room it was given, and the limits that decide
// whether a run stops or keeps going. Every one of them is a flag or an
// environment variable, so every one of them can differ between the run that
// started a session and the run that resumed it.
//
// Recorded because they change how the same conversation behaves without
// appearing anywhere in it. A context window drives when compaction happens, so
// two runs over identical messages compact at different points and diverge from
// there; the iteration fuse and the request bounds decide whether a turn ends in
// an answer or is cut off. A journal that has the messages but not these
// describes a run nobody can reproduce.
//
// Durations are strings rather than the nanosecond counts Go would otherwise
// write, because this file is read.
type SessionSettings struct {
	BaseURL                string `json:"base_url,omitempty"`
	ContextWindow          int    `json:"context_window,omitempty"`
	ContextWindowEstimated bool   `json:"context_window_estimated,omitempty"`
	// MaxToolIterations is the fuse as it applies, not as it was asked for: the
	// default resolved, and negative for a caller that removed it.
	MaxToolIterations int    `json:"max_tool_iterations,omitempty"`
	RetryBudget       string `json:"retry_budget,omitempty"`
	StreamIdleTimeout string `json:"stream_idle_timeout,omitempty"`
	// BackgroundJobs says whether the model could start work that outlives the
	// tool call. It changes the catalog rather than only a bound -- Bash is
	// offered a background parameter or it is not -- so two sessions with the
	// same tools were not offered the same tools.
	//
	// Written even when false, unlike the bounds above. A default that depends
	// on how Cy was invoked is one a reader cannot recompute from the journal,
	// and an omitted false would be indistinguishable from a build that did not
	// know the setting existed.
	BackgroundJobs bool `json:"background_jobs"`
	// JobLauncher is the program each job was supervised by, when it was not
	// the built-in supervisor. Between them these two say what a job could be
	// and where it ran, which is the difference between two runs of the same
	// command that behaved differently.
	JobLauncher string `json:"job_launcher,omitempty"`
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
	// ToolLimitReached says the per-turn tool iteration fuse blew. It is a
	// separate field and not an outcome because the run really did complete: the
	// fuse ends a turn by withholding the tools and asking the model to answer
	// with what it has, and if it answers, the run closes as "completed" like any
	// other. The reply is then a summary of the work reached rather than the work
	// asked for, and nothing in the record said so -- the evidence was the prose
	// of a synthetic tool result, which is readable but not queryable.
	//
	// Absent on a run that was reconciled after a crash, where nobody observed
	// the turn. That is the same silence as an outcome-less record, not a claim
	// the fuse held.
	ToolLimitReached bool `json:"tool_limit_reached,omitempty"`
	// DetachedJobs names the jobs that were still running when the run closed
	// and were left running. Ordinary background work dies with Cy, so a job id
	// in the journal is otherwise a thing of the past; these are the ones a
	// later run can still find in the registry, adopt, and report on. Without
	// them, a completion delivered in a session's second run refers to a job
	// nothing in the first run says survived it.
	DetachedJobs []string `json:"detached_jobs,omitempty"`
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
//
// FinishedAt is when the work ended, which is not the record's own timestamp:
// that one says when the model was told. The gap between them is the thing a
// deferred result has and an ordinary one does not, and reading it back is how
// one tells a job that took ten minutes from a completion that sat undelivered
// for ten minutes. Omitted rather than zeroed when there is no such time,
// because a boundary event need not always report finished work.
type BoundaryEvent struct {
	RunID      string    `json:"run_id"`
	JobID      string    `json:"job_id,omitempty"`
	FinishedAt time.Time `json:"finished_at,omitzero"`
	Content    string    `json:"content"`
}

func DecodePayload[T any](record Record) (T, error) {
	var payload T
	err := json.Unmarshal(record.Payload, &payload)
	return payload, err
}
