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

func TestJobListReportsEveryManagedJobAndWhatItRan(t *testing.T) {
	manager := processManagerForTest(t)
	// No job_id, and that is not an error: list is the one action that asks
	// about all of them.
	if empty := runProcessTool(t, manager.job, jobArgs{Action: "list"}); !strings.Contains(empty, "no managed jobs") {
		t.Fatalf("empty list = %q", empty)
	}
	running := jobIDFromText(t, runProcessTool(t, manager.bash, bashArgs{Command: "sleep 30 # the long one", Background: true}))
	finished := jobIDFromText(t, runProcessTool(t, manager.bash, bashArgs{Command: "printf done # the short one", Background: true}))
	<-manager.get(finished).done

	list := runProcessTool(t, manager.job, jobArgs{Action: "list"})
	// The command, not only the id: an agent cannot choose which job to wait on
	// from a column of opaque identifiers.
	for _, want := range []string{running + " running", finished + " completed", "exit_code=0", "the long one", "the short one"} {
		if !strings.Contains(list, want) {
			t.Fatalf("list is missing %q: %q", want, list)
		}
	}
	if strings.Index(list, running) > strings.Index(list, finished) {
		t.Fatalf("list is not in start order: %q", list)
	}
}

func TestJobWaitReturnsOnCompletionAndOnAnExpiredBound(t *testing.T) {
	manager := processManagerForTest(t)
	short := jobIDFromText(t, runProcessTool(t, manager.bash, bashArgs{Command: "sleep 0.2; printf finally", Background: true}))
	done := runProcessTool(t, manager.job, jobArgs{Action: "wait", JobID: short})
	if !strings.Contains(done, "status: completed") || !strings.Contains(done, "finally") {
		t.Fatalf("wait on a short job = %q", done)
	}
	// Waiting counts as being told. A completion the model has just read must
	// not arrive a second time as a boundary event.
	pending, err := manager.PendingCompletionEvents("")
	if err != nil {
		t.Fatal(err)
	}
	for _, event := range pending {
		if event.JobID == short {
			t.Fatalf("wait left the completion pending delivery: %+v", event)
		}
	}

	long := jobIDFromText(t, runProcessTool(t, manager.bash, bashArgs{Command: "sleep 30", Background: true}))
	started := time.Now()
	// An expired bound is a result rather than an error: "still running" is a
	// legitimate answer to "wait a second".
	stillRunning := runProcessTool(t, manager.job, jobArgs{Action: "wait", JobID: long, TimeoutSeconds: 1})
	if elapsed := time.Since(started); elapsed < time.Second || elapsed > 10*time.Second {
		t.Fatalf("bounded wait took %s", elapsed)
	}
	if !strings.Contains(stillRunning, "status: running") {
		t.Fatalf("wait on a long job = %q", stillRunning)
	}
}

func TestASupervisorRecordsTheJobsFateWhereAnotherProcessCouldReadIt(t *testing.T) {
	manager := processManagerForTest(t)
	id := jobIDFromText(t, runProcessTool(t, manager.bash, bashArgs{Command: "printf supervised; exit 5", Background: true}))
	job := manager.get(id)
	<-job.done

	result, ok := readJobResult(manager.mailboxFor(id))
	if !ok {
		t.Fatalf("nothing in the mailbox at %s", manager.mailboxFor(id))
	}
	if result.ExitCode == nil || *result.ExitCode != 5 || result.Pid == 0 || result.FinishedAt.IsZero() {
		t.Fatalf("recorded result = %#v", result)
	}
	job.mu.Lock()
	code, status := job.exitCode, job.status
	job.mu.Unlock()
	if status != jobFailed || code == nil || *code != 5 {
		t.Fatalf("job = %s / %v, want failed with exit 5", status, code)
	}
}

func TestAConfiguredLauncherSupervisesTheJobAndItsResultIsWhatCyReads(t *testing.T) {
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("sh is unavailable")
	}
	// The whole launcher contract in five lines of shell, which is the point of
	// making it argv: the mailbox first, then the program and its arguments.
	launcher := filepath.Join(t.TempDir(), "launcher.sh")
	script := "#!/bin/sh\nmailbox=$1; shift\n\"$@\"\nprintf '{\"status\":\"completed\",\"exit_code\":42}\\n' > \"$mailbox/result.json\"\n"
	if err := os.WriteFile(launcher, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	manager, err := NewProcessManager(ProcessOptions{Root: t.TempDir(), Home: t.TempDir(), Sandbox: sandboxOff, Background: true, JobLauncher: launcher})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = manager.Close() })

	id := jobIDFromText(t, runProcessTool(t, manager.bash, bashArgs{Command: "printf launched", Background: true}))
	job := manager.get(id)
	<-job.done
	job.mu.Lock()
	code := job.exitCode
	job.mu.Unlock()
	// The launcher's file says 42 where the program exited 0. Cy reporting 42
	// is the only way to see that the file is what it read, rather than the
	// wait status it happened to have as well.
	if code == nil || *code != 42 {
		t.Fatalf("exit code = %v, want the launcher's 42", code)
	}
	if output, _ := job.log.snapshot(64); !strings.Contains(string(output), "launched") {
		t.Fatalf("job output = %q", output)
	}
}

func TestASupervisorLeavesTheJobsOutputBesideItsResult(t *testing.T) {
	manager := processManagerForTest(t)
	id := jobIDFromText(t, runProcessTool(t, manager.bash, bashArgs{Command: "printf 'on stdout'; printf 'and on stderr' >&2", Background: true}))
	<-manager.get(id).done

	tail, ok := readJobOutput(manager.mailboxFor(id))
	if !ok {
		t.Fatalf("no output in the mailbox at %s", manager.mailboxFor(id))
	}
	// Both streams, because the supervisor gives the job one pipe for the two.
	if !strings.Contains(string(tail), "on stdout") || !strings.Contains(string(tail), "and on stderr") {
		t.Fatalf("recorded output = %q", tail)
	}
	if result, _ := readJobResult(manager.mailboxFor(id)); result.OutputBytes != int64(len(tail)) {
		t.Fatalf("output_bytes = %d, want %d", result.OutputBytes, len(tail))
	}
}

func TestTheOutputCyReportsIsTheSupervisorsFileNotThePipeItAlsoHeld(t *testing.T) {
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("sh is unavailable")
	}
	// A launcher that reports output the program never printed, and claims more
	// of it was cut than there is. Nothing else distinguishes the file from the
	// pipe this process was holding at the same time.
	launcher := filepath.Join(t.TempDir(), "launcher.sh")
	script := "#!/bin/sh\nmailbox=$1; shift\n\"$@\"\n" +
		"printf 'from the file' > \"$mailbox/output\"\n" +
		"printf '{\"status\":\"completed\",\"exit_code\":0,\"output_bytes\":1014}\\n' > \"$mailbox/result.json\"\n"
	if err := os.WriteFile(launcher, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	manager, err := NewProcessManager(ProcessOptions{Root: t.TempDir(), Home: t.TempDir(), Sandbox: sandboxOff, Background: true, JobLauncher: launcher})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = manager.Close() })

	id := jobIDFromText(t, runProcessTool(t, manager.bash, bashArgs{Command: "printf 'from the pipe'", Background: true}))
	job := manager.get(id)
	<-job.done

	tail, truncated := job.log.snapshot(0)
	if string(tail) != "from the file" || !truncated {
		t.Fatalf("output = %q truncated = %v, want the launcher's file", tail, truncated)
	}
	if stored, discarded := job.log.stats(); stored != 13 || discarded != 1001 {
		t.Fatalf("stored = %d discarded = %d, want the file's 13 of 1014", stored, discarded)
	}
}

// The stop this is about is an ordinary one: Cy exits after a job has finished
// but before the next turn boundary, which is the only place a completion is
// ever told. Nothing about the job outlives the run -- only its mailbox does.
func TestACompletionOutlivesTheRunThatNeverGotToReportIt(t *testing.T) {
	home, root := t.TempDir(), t.TempDir()
	first := managerForSession(t, home, root, nil)
	id := jobIDFromText(t, runProcessTool(t, first.bash, bashArgs{Command: "printf 'work done'", Background: true}))
	<-first.get(id).done
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}

	second := managerForSession(t, home, root, nil)
	pending, err := second.PendingCompletionEvents("run")
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 1 || pending[0].JobID != id || pending[0].FinishedAt.IsZero() {
		t.Fatalf("pending = %#v, want the unreported job", pending)
	}
	// Adopted as a whole job, not just as a notification: the model is told it
	// finished and can then ask what it printed.
	if output := runProcessTool(t, second.job, jobArgs{Action: "output", JobID: id}); !strings.Contains(output, "work done") {
		t.Fatalf("output = %q", output)
	}
	if listed := runProcessTool(t, second.job, jobArgs{Action: "list"}); !strings.Contains(listed, "printf 'work done'") {
		t.Fatalf("list = %q", listed)
	}
}

func TestACompletionIsNotToldTwiceAcrossAStop(t *testing.T) {
	home, root := t.TempDir(), t.TempDir()
	first := managerForSession(t, home, root, nil)
	id := jobIDFromText(t, runProcessTool(t, first.bash, bashArgs{Command: "printf 'work done'", Background: true}))
	<-first.get(id).done
	if err := first.MarkCompletionDelivered(id); err != nil {
		t.Fatal(err)
	}
	// Acknowledging removes the mailbox, so put it back: this is the crash
	// window between journalling the delivery and tidying up after it, and the
	// journal is the only thing that can settle it.
	if err := os.MkdirAll(first.mailboxFor(id), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := writeJobResult(first.mailboxFor(id), jobResult{Status: jobCompleted}); err != nil {
		t.Fatal(err)
	}
	_ = first.Close()

	second := managerForSession(t, home, root, map[string]struct{}{id: {}})
	pending, err := second.PendingCompletionEvents("run")
	if err != nil || len(pending) != 0 {
		t.Fatalf("pending = %#v err = %v, want nothing to report", pending, err)
	}
	if _, err := os.Stat(second.mailboxFor(id)); !os.IsNotExist(err) {
		t.Fatalf("mailbox still at %s: %v", second.mailboxFor(id), err)
	}
}

func TestAJobKilledOnTheWayOutHasNothingToReportLater(t *testing.T) {
	home, root := t.TempDir(), t.TempDir()
	first := managerForSession(t, home, root, nil)
	runProcessTool(t, first.bash, bashArgs{Command: "sleep 30", Background: true})
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
	if entries, err := os.ReadDir(filepath.Join(home, "jobs")); err == nil && len(entries) != 0 {
		t.Fatalf("registry still holds %d sessions", len(entries))
	}

	second := managerForSession(t, home, root, nil)
	if pending, err := second.PendingCompletionEvents("run"); err != nil || len(pending) != 0 {
		t.Fatalf("pending = %#v err = %v, want nothing", pending, err)
	}
}

// managerForSession builds managers that share a state home and a session id,
// which is what makes the second one a resume of the first.
func managerForSession(t *testing.T, home, root string, delivered map[string]struct{}) *processManager {
	t.Helper()
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash is unavailable")
	}
	manager, err := NewProcessManager(ProcessOptions{
		Root: root, Home: home, SessionID: "session", Sandbox: sandboxOff,
		Background: true, Delivered: delivered,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = manager.Close() })
	return manager
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

func TestBashWithoutBackgroundNeitherOffersItNorAcceptsIt(t *testing.T) {
	manager := processManagerForTest(t)
	manager.allowBackground = false
	rawSchema, err := json.Marshal(manager.Tools()[0].Definition.Function.Parameters)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(rawSchema), `"background"`) {
		t.Fatalf("Bash schema exposes background where it is turned off: %s", rawSchema)
	}
	rawArgs, err := json.Marshal(bashArgs{Command: "printf nope", Background: true})
	if err != nil {
		t.Fatal(err)
	}
	// Refused as well as hidden. A model that has seen the parameter in some
	// other session can still ask for it, and running the command in the
	// foreground instead would answer a question nobody asked.
	_, err = manager.bash(context.Background(), llm.ToolCall{Function: llm.ToolFunction{Arguments: string(rawArgs)}})
	if err == nil || !strings.Contains(err.Error(), "background Bash is turned off") {
		t.Fatalf("background error = %v", err)
	}
}

func processManagerForTest(t *testing.T) *processManager {
	t.Helper()
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash is unavailable")
	}
	manager, err := NewProcessManager(ProcessOptions{Root: t.TempDir(), Home: t.TempDir(), Sandbox: sandboxOff, Background: true})
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
	// Reported, not inferred from when the caller happened to ask.
	if first[0].FinishedAt.IsZero() {
		t.Fatalf("pending completion carries no finish time: %#v", first[0])
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
