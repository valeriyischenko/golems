package tools

import "encoding/json"

const (
	ProcessResultMetaType  = "process_result"
	processFailureTailSize = 4 * 1024
	processResultMetaType  = ProcessResultMetaType
)

type ProcessResultMeta struct {
	Type           string `json:"type"`
	JobID          string `json:"job_id"`
	Status         string `json:"status"`
	ExitCode       *int   `json:"exit_code,omitempty"`
	DurationMillis int64  `json:"duration_ms"`
	OutputBytes    int64  `json:"output_bytes"`
	DiscardedBytes int64  `json:"discarded_bytes,omitempty"`
	FailureTail    string `json:"failure_tail,omitempty"`
	UserInitiated  bool   `json:"user_initiated,omitempty"`
	// Detached says this job outlives the run. A running job is usually gone
	// when Cy exits and this one is not, which changes what the id is worth:
	// it still means something in the next run of the session.
	Detached bool `json:"detached,omitempty"`
}

type processResultMeta = ProcessResultMeta

func ProcessResultMetaFrom(value any) (ProcessResultMeta, bool) {
	if value == nil {
		return ProcessResultMeta{}, false
	}
	if typed, ok := value.(ProcessResultMeta); ok {
		return typed, typed.Type == ProcessResultMetaType
	}
	if typed, ok := value.(*ProcessResultMeta); ok && typed != nil {
		return *typed, typed.Type == ProcessResultMetaType
	}
	raw, err := json.Marshal(value)
	if err != nil {
		return ProcessResultMeta{}, false
	}
	var decoded ProcessResultMeta
	if err := json.Unmarshal(raw, &decoded); err != nil || decoded.Type != ProcessResultMetaType {
		return ProcessResultMeta{}, false
	}
	return decoded, true
}

func processResultMetaFrom(value any) (processResultMeta, bool) {
	return ProcessResultMetaFrom(value)
}
