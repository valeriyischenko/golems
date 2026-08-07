package tools

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/levmv/golems/pkg/golem"
	"github.com/levmv/golems/pkg/llm"
)

func TestBashReportsExitAndUsesIsolatedEnvironment(t *testing.T) {
	manager := processManagerForTest(t)
	if err := manager.SetSandbox(sandboxAuto); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CY_TEST_SECRET", "must-not-leak")
	result := runProcessTool(t, manager.bash, bashArgs{Command: "printf 'hello\\n'; printf 'stderr\\n' >&2; env; exit 3"})
	if !strings.Contains(result, "status: failed") || !strings.Contains(result, "exit_code: 3") || !strings.Contains(result, "hello") || !strings.Contains(result, "stderr") {
		t.Fatalf("bash result = %q", result)
	}
	if strings.Contains(result, "CY_TEST_SECRET") || strings.Contains(result, "must-not-leak") {
		t.Fatalf("bash leaked parent environment: %q", result)
	}
	if !strings.Contains(result, "HOME="+manager.toolHome) {
		t.Fatalf("bash HOME is not isolated: %q", result)
	}
}

func TestBashSandboxOffInheritsAmbientEnvironment(t *testing.T) {
	manager := processManagerForTest(t)
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("CY_TEST_AMBIENT", "visible-to-agent")
	result := runProcessTool(t, manager.bash, bashArgs{Command: `printf 'HOME=%s\nCY_TEST_AMBIENT=%s\n' "$HOME" "$CY_TEST_AMBIENT"`})
	if !strings.Contains(result, "HOME="+home) || !strings.Contains(result, "CY_TEST_AMBIENT=visible-to-agent") {
		t.Fatalf("bash did not inherit ambient environment: %q", result)
	}
}

func TestProcessManagerRunShellInheritsAmbientEnvironment(t *testing.T) {
	manager := processManagerForTest(t)
	if err := manager.SetSandbox(sandboxOn); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(t.TempDir(), "outside.txt")
	if err := os.WriteFile(outside, []byte("ambient-shell"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CY_USER_SHELL_OUTSIDE", outside)
	result, err := manager.RunShell(context.Background(), `cat "$CY_USER_SHELL_OUTSIDE"`)
	if err != nil {
		t.Fatal(err)
	}
	meta, ok := ProcessResultMetaFrom(result.Meta)
	if !ok || meta.Status != jobCompleted || meta.ExitCode == nil || *meta.ExitCode != 0 || meta.JobID != "" || !meta.UserInitiated {
		t.Fatalf("process meta = %#v", result.Meta)
	}
	if !strings.Contains(result.Content, "ambient-shell") {
		t.Fatalf("shell result = %q", result.Content)
	}
}

func TestProcessManagerRunShellCancellationCleansUpProcess(t *testing.T) {
	manager := processManagerForTest(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		_, err := manager.RunShell(ctx, "sleep 30")
		done <- err
	}()

	deadline := time.Now().Add(3 * time.Second)
	for {
		manager.mu.Lock()
		jobCount := len(manager.jobs)
		manager.mu.Unlock()
		if jobCount == 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("user shell did not start")
		}
		time.Sleep(5 * time.Millisecond)
	}
	cancel()

	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("RunShell error = %v, want context cancellation", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("cancelled user shell did not stop")
	}
	manager.mu.Lock()
	remaining := len(manager.jobs)
	manager.mu.Unlock()
	if remaining != 0 {
		t.Fatalf("cancelled user shell left %d managed jobs", remaining)
	}
}

func TestBashReturnsStructuredFailureStatus(t *testing.T) {
	manager := processManagerForTest(t)
	result := runProcessResult(t, manager.bash, bashArgs{Command: "printf 'first\\nlast\\n'; exit 7"})
	meta, ok := processResultMetaFrom(result.Meta)
	if !ok || meta.Status != jobFailed || meta.ExitCode == nil || *meta.ExitCode != 7 || meta.JobID != "" {
		t.Fatalf("process meta = %#v", result.Meta)
	}
	if strings.Contains(result.Content, "job_id:") || len(manager.jobs) != 0 {
		t.Fatalf("completed foreground command was retained as a job: %q / %#v", result.Content, manager.jobs)
	}
	for _, header := range []string{"pid:", "command:", "cwd:", "started_at:", "duration_ms:", "output_bytes:", "discarded_bytes:"} {
		if strings.Contains(result.Content, header) {
			t.Fatalf("foreground result contains service header %q: %q", header, result.Content)
		}
	}
	if meta.OutputBytes == 0 || !strings.Contains(meta.FailureTail, "last") {
		t.Fatalf("process output metadata = %#v", meta)
	}
}

func TestBashTimeoutKillsProcessGroup(t *testing.T) {
	manager := processManagerForTest(t)
	started := time.Now()
	result := runProcessTool(t, manager.bash, bashArgs{Command: "sleep 30; printf done", TimeoutSeconds: 1})
	if elapsed := time.Since(started); elapsed > 5*time.Second {
		t.Fatalf("timeout took %s", elapsed)
	}
	if !strings.Contains(result, "status: timed_out") {
		t.Fatalf("timeout result = %q", result)
	}
}

func TestBackgroundJobCanBeReadAndStopped(t *testing.T) {
	manager := processManagerForTest(t)
	started := runProcessTool(t, manager.bash, bashArgs{Command: "printf ready; sleep 30", Background: true})
	id := jobIDFromText(t, started)
	job := manager.get(id)
	deadline := time.Now().Add(3 * time.Second)
	for {
		content, _ := job.log.snapshot(32)
		if strings.Contains(string(content), "ready") {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("background job did not produce initial output")
		}
		time.Sleep(5 * time.Millisecond)
	}
	output := runProcessTool(t, manager.job, jobArgs{Action: "output", JobID: id})
	if !strings.Contains(output, "status: running") || !strings.Contains(output, "ready") {
		t.Fatalf("job output = %q", output)
	}
	stopped := runProcessTool(t, manager.job, jobArgs{Action: "stop", JobID: id})
	if !strings.Contains(stopped, "status: killed") {
		t.Fatalf("job stop = %q", stopped)
	}
}

func TestBashCapsOutputWithoutBlockingProcess(t *testing.T) {
	manager := processManagerForTest(t)
	manager.logLimit = 64
	result := runProcessResult(t, manager.bash, bashArgs{Command: "head -c 1024 /dev/zero | tr '\\0' x"})
	meta, ok := processResultMetaFrom(result.Meta)
	if !ok || meta.DiscardedBytes < 900 || !strings.Contains(result.Content, "truncated: true") {
		t.Fatalf("meta=%#v result=%q", result.Meta, result.Content)
	}
}

func TestBashMarksTruncatedPreviewBeforeLogBufferOverflows(t *testing.T) {
	manager := processManagerForTest(t)
	result := runProcessResult(t, manager.bash, bashArgs{Command: "head -c 40000 /dev/zero | tr '\\0' x"})
	meta, ok := processResultMetaFrom(result.Meta)
	if !ok || meta.DiscardedBytes != 0 || meta.OutputBytes != 40000 {
		t.Fatalf("process meta = %#v", result.Meta)
	}
	if !strings.Contains(result.Content, "truncated: true") {
		t.Fatalf("truncated preview was not marked: %q", result.Content)
	}
}

func TestProcessManagerCloseKillsBackgroundJobs(t *testing.T) {
	manager := processManagerForTest(t)
	started := runProcessTool(t, manager.bash, bashArgs{Command: "sleep 30", Background: true})
	id := jobIDFromText(t, started)
	if err := manager.Close(); err != nil {
		t.Fatal(err)
	}
	job := manager.get(id)
	job.mu.Lock()
	status := job.status
	job.mu.Unlock()
	if status != jobKilled {
		t.Fatalf("job status = %q, want killed", status)
	}
}

func TestOneShotBashDoesNotExposeBackground(t *testing.T) {
	manager := processManagerForTest(t)
	manager.allowBackground = false
	rawSchema, err := json.Marshal(manager.Tools()[0].Definition.Function.Parameters)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(rawSchema), `"background"`) {
		t.Fatalf("one-shot Bash schema exposes background: %s", rawSchema)
	}
	rawArgs, err := json.Marshal(bashArgs{Command: "printf nope", Background: true})
	if err != nil {
		t.Fatal(err)
	}
	_, err = manager.bash(context.Background(), llm.ToolCall{Function: llm.ToolFunction{Arguments: string(rawArgs)}})
	if err == nil || !strings.Contains(err.Error(), "unavailable in one-shot mode") {
		t.Fatalf("background error = %v", err)
	}
}

func processManagerForTest(t *testing.T) *processManager {
	t.Helper()
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash is unavailable")
	}
	manager, err := NewProcessManager(t.TempDir(), t.TempDir(), sandboxOff, true)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = manager.Close() })
	return manager
}

func runProcessTool[T any](t *testing.T, run func(context.Context, llm.ToolCall) (golem.ToolResult, error), args T) string {
	t.Helper()
	return runProcessResult(t, run, args).Content
}

func runProcessResult[T any](t *testing.T, run func(context.Context, llm.ToolCall) (golem.ToolResult, error), args T) golem.ToolResult {
	t.Helper()
	raw, err := json.Marshal(args)
	if err != nil {
		t.Fatal(err)
	}
	result, err := run(context.Background(), llm.ToolCall{Function: llm.ToolFunction{Arguments: string(raw)}})
	if err != nil {
		t.Fatalf("tool error = %v", err)
	}
	return result
}

func jobIDFromText(t *testing.T, text string) string {
	t.Helper()
	match := regexp.MustCompile(`(?m)^job_id: (job-[0-9a-f]+)$`).FindStringSubmatch(text)
	if len(match) != 2 {
		t.Fatalf("job id missing: %q", text)
	}
	return match[1]
}

// Peeking must not consume. Between the peek and the acknowledgement is the
// caller writing the event somewhere durable, and that is the part that can
// fail; a completion consumed by a failed delivery is one the model is never
// told about.
func TestPendingCompletionEventsRepeatUntilAcknowledged(t *testing.T) {
	manager := processManagerForTest(t)
	started := runProcessTool(t, manager.bash, bashArgs{Command: "exit 0", Background: true})
	id := jobIDFromText(t, started)
	job := manager.get(id)
	select {
	case <-job.done:
	case <-time.After(3 * time.Second):
		t.Fatal("background job did not finish")
	}

	first, err := manager.PendingCompletionEvents("run-1")
	if err != nil {
		t.Fatal(err)
	}
	if len(first) != 1 || first[0].JobID != id {
		t.Fatalf("pending = %#v, want the finished job", first)
	}
	again, err := manager.PendingCompletionEvents("run-1")
	if err != nil {
		t.Fatal(err)
	}
	if len(again) != 1 {
		t.Fatalf("pending after an unacknowledged peek = %d, want it still offered", len(again))
	}

	if err := manager.MarkCompletionDelivered(id); err != nil {
		t.Fatal(err)
	}
	after, err := manager.PendingCompletionEvents("run-1")
	if err != nil {
		t.Fatal(err)
	}
	if len(after) != 0 {
		t.Fatalf("pending after acknowledgement = %#v, want none", after)
	}
	// A job that was never offered, or was acknowledged twice, is not an error:
	// the caller is asserting an outcome, not asking for one.
	if err := manager.MarkCompletionDelivered(id); err != nil {
		t.Fatal(err)
	}
	if err := manager.MarkCompletionDelivered("job-nonexistent"); err != nil {
		t.Fatal(err)
	}
}
