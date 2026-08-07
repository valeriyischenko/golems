package engine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/levmv/golems/cy/internal/session"
	"github.com/levmv/golems/pkg/golem"
	"github.com/levmv/golems/pkg/llm"
)

const (
	minContextReserve = 8 * 1024
	maxContextReserve = 32 * 1024
	minVerbatimTail   = 8 * 1024
	maxVerbatimTail   = 24 * 1024
	compactionOutput  = 4096
	prunedToolHead    = 4 * 1024
	prunedToolTail    = 4 * 1024
)

type ContextReport struct {
	ModelURI             string
	Window               int
	Estimated            bool
	InputLimit           int
	SystemTokens         int
	ToolTokens           int
	InstructionTokens    int
	SummaryTokens        int
	HistoryTokens        int
	PendingTokens        int
	TotalInputTokens     int
	AvailableInputTokens int
	PercentLeft          int
	CompactionCount      int
}

func (e *Engine) ContextReport() (ContextReport, error) {
	report, _, err := e.Status()
	return report, err
}

func (e *Engine) Status() (ContextReport, llm.Usage, error) {
	state, err := e.session.Replay()
	if err != nil {
		return ContextReport{}, llm.Usage{}, err
	}
	_, report := e.buildContext(state)
	return report, state.Usage, nil
}

// Compact summarizes completed older turns and records an additive context
// boundary. It never deletes or rewrites transcript records.
func (e *Engine) Compact(ctx context.Context, focus string) (ContextReport, llm.Usage, error) {
	e.turnMu.Lock()
	defer e.turnMu.Unlock()
	if err := e.compactLocked(ctx, strings.TrimSpace(focus), false); err != nil {
		return ContextReport{}, llm.Usage{}, err
	}
	return e.Status()
}

func (e *Engine) prepareContext(ctx context.Context) ([]llm.Message, ContextReport, error) {
	state, err := e.session.Replay()
	if err != nil {
		return nil, ContextReport{}, err
	}
	messages, report := e.buildContext(state)
	if report.TotalInputTokens <= report.InputLimit {
		return messages, report, nil
	}
	if prunedMessages, prunedReport, applied, pruneErr := e.pruneToolResultsIfSufficient(state); pruneErr != nil {
		return nil, report, fmt.Errorf("prune old tool results: %w", pruneErr)
	} else if applied {
		return prunedMessages, prunedReport, nil
	}
	if err := e.compactLocked(ctx, "", true); err != nil {
		return nil, report, fmt.Errorf("context needs compaction: %w", err)
	}
	state, err = e.session.Replay()
	if err != nil {
		return nil, ContextReport{}, err
	}
	messages, report = e.buildContext(state)
	if report.TotalInputTokens > report.InputLimit {
		return nil, report, fmt.Errorf("context remains oversized after compaction: estimated input %d tokens exceeds limit %d (window %d)", report.TotalInputTokens, report.InputLimit, report.Window)
	}
	return messages, report, nil
}

func (e *Engine) buildContext(state session.State) ([]llm.Message, ContextReport) {
	report := ContextReport{
		ModelURI:        e.modelURI,
		Window:          e.contextWindow,
		Estimated:       e.contextEstimated,
		CompactionCount: state.CompactionCount,
	}
	reserve := clamp(e.contextWindow/5, minContextReserve, maxContextReserve)
	report.InputLimit = max(1, report.Window-reserve)

	messages := make([]llm.Message, 0, len(state.Messages)+2+len(e.instructionPrompts))
	if e.systemPrompt != "" {
		messages = append(messages, llm.Message{Role: llm.RoleSystem, Content: e.systemPrompt})
		report.SystemTokens += llm.EstimateMessageTokens(messages[len(messages)-1])
	}
	for _, prompt := range e.instructionPrompts {
		messages = append(messages, llm.Message{Role: llm.RoleSystem, Content: prompt})
		report.InstructionTokens += llm.EstimateMessageTokens(messages[len(messages)-1])
	}
	if raw, err := json.Marshal(e.tools); err == nil {
		report.ToolTokens = llm.EstimateTextTokens(string(raw))
	}

	start := 0
	if state.Compaction != nil {
		summary := llm.Message{Role: llm.RoleSystem, Content: "Conversation summary:\n" + state.Compaction.Summary}
		messages = append(messages, summary)
		report.SummaryTokens = llm.EstimateMessageTokens(summary)
		start = firstMessageAtOrAfter(state.MessageSeqs, state.Compaction.FirstVerbatimSeq)
	}
	history := llm.CloneMessages(state.Messages[start:])
	historyRunIDs := append([]string(nil), state.MessageRunIDs[start:]...)
	for index, message := range history {
		if pruning := state.ToolPruning; pruning != nil && message.Role == llm.RoleTool && state.MessageSeqs[start+index] <= pruning.ThroughSeq {
			message.Content = pruneToolResult(message.Content, pruning.HeadBytes, pruning.TailBytes)
		}
		if _, active := state.ActiveRuns[historyRunIDs[index]]; !active {
			message.ReasoningContent = ""
		}
		messages = append(messages, message)
		report.HistoryTokens += llm.EstimateMessageTokens(message)
	}
	e.queueMu.Lock()
	for _, pending := range e.pendingInputs {
		report.PendingTokens += llm.EstimateTextTokens(pending) + 4
	}
	e.queueMu.Unlock()
	report.TotalInputTokens = report.SystemTokens + report.ToolTokens + report.InstructionTokens + report.SummaryTokens + report.HistoryTokens + report.PendingTokens
	report.AvailableInputTokens = max(0, report.InputLimit-report.TotalInputTokens)
	report.PercentLeft = clamp(report.AvailableInputTokens*100/max(1, report.InputLimit), 0, 100)
	return messages, report
}

func (e *Engine) pruneToolResultsIfSufficient(state session.State) ([]llm.Message, ContextReport, bool, error) {
	activeStart := 0
	if state.Compaction != nil {
		activeStart = firstMessageAtOrAfter(state.MessageSeqs, state.Compaction.FirstVerbatimSeq)
	}
	tailBudget := clamp(e.contextWindow/5, minVerbatimTail, maxVerbatimTail)
	firstVerbatim := chooseVerbatimStart(state.Messages, activeStart, tailBudget)
	if firstVerbatim <= activeStart || firstVerbatim > len(state.MessageSeqs)-1 {
		return nil, ContextReport{}, false, nil
	}

	pruning := session.ToolResultsPruned{
		ThroughSeq: state.MessageSeqs[firstVerbatim-1],
		HeadBytes:  prunedToolHead,
		TailBytes:  prunedToolTail,
	}
	if state.ToolPruning != nil {
		if pruning.ThroughSeq <= state.ToolPruning.ThroughSeq {
			return nil, ContextReport{}, false, nil
		}
		pruning.HeadBytes = state.ToolPruning.HeadBytes
		pruning.TailBytes = state.ToolPruning.TailBytes
	}

	projected := state
	projected.ToolPruning = &pruning
	messages, report := e.buildContext(projected)
	if report.TotalInputTokens > report.InputLimit {
		return nil, report, false, nil
	}
	if _, err := e.session.Append(session.RecordToolResultsPruned, pruning); err != nil {
		return nil, report, false, err
	}
	return messages, report, true, nil
}

func pruneToolResult(content string, headBytes, tailBytes int) string {
	if headBytes <= 0 || tailBytes <= 0 || len(content) <= headBytes+tailBytes {
		return content
	}
	headEnd := min(headBytes, len(content))
	for headEnd > 0 && !utf8.ValidString(content[:headEnd]) {
		headEnd--
	}
	tailStart := max(headEnd, len(content)-tailBytes)
	for tailStart < len(content) && !utf8.RuneStart(content[tailStart]) {
		tailStart++
	}
	omitted := tailStart - headEnd
	if omitted <= 0 {
		return content
	}
	marker := fmt.Sprintf("\n[… %d bytes omitted from old tool result; full output remains in session journal …]\n", omitted)
	pruned := content[:headEnd] + marker + content[tailStart:]
	if len(pruned) >= len(content) {
		return content
	}
	return pruned
}

func (e *Engine) compactLocked(ctx context.Context, focus string, automatic bool) error {
	state, err := e.session.Replay()
	if err != nil {
		return err
	}
	activeStart := 0
	if state.Compaction != nil {
		activeStart = firstMessageAtOrAfter(state.MessageSeqs, state.Compaction.FirstVerbatimSeq)
	}
	tailBudget := clamp(e.contextWindow/5, minVerbatimTail, maxVerbatimTail)
	firstVerbatim := chooseVerbatimStart(state.Messages, activeStart, tailBudget)
	if !automatic && firstVerbatim <= activeStart {
		firstVerbatim = latestUserStart(state.Messages, activeStart)
	}
	if firstVerbatim <= activeStart || firstVerbatim >= len(state.Messages) {
		return errors.New("no completed older conversation block is available to compact")
	}
	prompt, compactedEnd := e.compactionPrompt(state, activeStart, firstVerbatim, focus)
	if compactedEnd <= activeStart {
		return errors.New("no conversation record fits in the bounded compaction input")
	}
	coveredSeq := state.MessageSeqs[compactedEnd-1]
	firstSeq := state.MessageSeqs[compactedEnd]
	maxTokens := compactionOutput
	requester, callErr := golem.NewRequester(golem.RequesterConfig{Model: e.model, Policy: e.requestPolicy, Sanitize: e.sanitize})
	if callErr != nil {
		return callErr
	}
	response, callErr := requester.Request(ctx, 0, llm.Request{
		Messages: []llm.Message{
			{Role: llm.RoleSystem, Content: e.compactionSystemPrompt},
			{Role: llm.RoleUser, Content: prompt},
		},
		MaxTokens: &maxTokens,
	}, false, nil)
	if callErr == nil && (response == nil || strings.TrimSpace(response.Content) == "") {
		callErr = errors.New("compaction model returned an empty summary")
	}
	if callErr == nil && (response.FinishReason == llm.FinishReasonLength || response.FinishReason == llm.FinishReasonContentFilter) {
		callErr = fmt.Errorf("compaction model returned an incomplete summary: finish reason %s", response.FinishReason)
	}
	if callErr != nil {
		return callErr
	}
	_, err = e.session.Append(session.RecordCompactionCompleted, session.CompactionCompleted{
		CoveredThroughSeq: coveredSeq,
		FirstVerbatimSeq:  firstSeq,
		Summary:           strings.TrimSpace(response.Content),
		Usage:             response.Usage,
	})
	return err
}

func latestUserStart(messages []llm.Message, activeStart int) int {
	for i := len(messages) - 1; i > activeStart; i-- {
		if messages[i].Role == llm.RoleUser {
			return i
		}
	}
	return activeStart
}

const defaultCompactionSystemPrompt = `Summarize an older segment of a coding-agent session for reliable continuation. Preserve objective and acceptance criteria, user constraints, decisions and reasons, modified files and current state, commands/tests and results, errors and abandoned approaches, outstanding work/next step, and explicit uncertainty. Be compact but concrete. Do not claim work was done unless the transcript says it was.`

const compactionToolArgumentsLimit = 4 * 1024

func (e *Engine) compactionPrompt(state session.State, start, end int, focus string) (string, int) {
	var out strings.Builder
	if state.Compaction != nil {
		out.WriteString("Previous rolling summary:\n")
		out.WriteString(state.Compaction.Summary)
		out.WriteString("\n\nNewly covered records:\n")
	}
	if focus != "" {
		out.WriteString("User focus for this compaction: ")
		out.WriteString(focus)
		out.WriteString("\n\n")
	}
	maxChars := max(64*1024, e.contextWindow*3)
	compactedEnd := start
	for blockStart := start; blockStart < end; {
		blockEnd := blockStart + 1
		for blockEnd < end && state.Messages[blockEnd].Role != llm.RoleUser {
			blockEnd++
		}
		var block strings.Builder
		for i := blockStart; i < blockEnd; i++ {
			message := state.Messages[i]
			content := message.Content
			limit := 32 * 1024
			if message.Role == llm.RoleTool {
				limit = 8 * 1024
			}
			content = truncateUTF8(content, limit)
			fmt.Fprintf(&block, "[%s seq=%d]", message.Role, state.MessageSeqs[i])
			if content != "" {
				block.WriteByte(' ')
				block.WriteString(content)
			}
			block.WriteByte('\n')
			for _, call := range message.ToolCalls {
				name := strings.TrimSpace(call.Function.Name)
				if name == "" {
					name = "unknown"
				}
				fmt.Fprintf(&block, "tool_call: %s", name)
				if arguments := strings.TrimSpace(call.Function.Arguments); arguments != "" {
					block.WriteString(" args=")
					block.WriteString(truncateUTF8(arguments, compactionToolArgumentsLimit))
				}
				block.WriteByte('\n')
			}
			block.WriteByte('\n')
		}
		if block.Len()+out.Len() > maxChars {
			out.WriteString("\n[newer records left verbatim because the compaction input is bounded]\n")
			break
		}
		out.WriteString(block.String())
		compactedEnd = blockEnd
		blockStart = blockEnd
	}
	return out.String(), compactedEnd
}

func chooseVerbatimStart(messages []llm.Message, activeStart, tailBudget int) int {
	if len(messages) == 0 {
		return activeStart
	}
	latestUser := -1
	for i := len(messages) - 1; i >= activeStart; i-- {
		if messages[i].Role == llm.RoleUser {
			latestUser = i
			break
		}
	}
	if latestUser <= activeStart {
		return activeStart
	}
	start := latestUser
	tokens := 0
	for i := len(messages) - 1; i >= activeStart; i-- {
		tokens += llm.EstimateMessageTokens(messages[i])
		if messages[i].Role == llm.RoleUser {
			if tokens > tailBudget && i < latestUser {
				break
			}
			start = i
		}
	}
	return start
}

func firstMessageAtOrAfter(seqs []uint64, target uint64) int {
	for i, seq := range seqs {
		if seq >= target {
			return i
		}
	}
	return len(seqs)
}

func truncateUTF8(text string, maxBytes int) string {
	if len(text) <= maxBytes {
		return text
	}
	cut := maxBytes
	for cut > 0 && !utf8.ValidString(text[:cut]) {
		cut--
	}
	return text[:cut] + "\n[…truncated for compaction input…]"
}

func clamp(value, low, high int) int {
	return min(max(value, low), high)
}
