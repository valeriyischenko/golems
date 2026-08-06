package tools

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/levmv/golems/pkg/golem"
	"github.com/levmv/golems/pkg/jsonschema"
	"github.com/levmv/golems/pkg/llm"
)

const (
	defaultBashYield       = 10 * time.Second
	defaultBashTimeout     = 10 * time.Minute
	maxBashTimeout         = time.Hour
	defaultCommandPreview  = 32 * 1024
	defaultCommandLogLimit = 256 * 1024
	maxJobReadBytes        = 256 * 1024
	jobStopTimeout         = 3 * time.Second
	jobKillRetryInterval   = 20 * time.Millisecond
)

const (
	jobRunning   = "running"
	jobCompleted = "completed"
	jobFailed    = "failed"
	jobKilled    = "killed"
	jobTimedOut  = "timed_out"
)

const (
	JobRunning   = jobRunning
	JobCompleted = jobCompleted
	JobFailed    = jobFailed
	JobKilled    = jobKilled
	JobTimedOut  = jobTimedOut
)

type processOrigin uint8

const (
	processOriginAgent processOrigin = iota
	processOriginUser
)

type processManager struct {
	workspace       *workspaceTools
	toolHome        string
	logLimit        int64
	sandbox         string
	allowBackground bool

	mu     sync.Mutex
	jobs   map[string]*processJob
	closed bool
}

type ProcessManager = processManager

type processJob struct {
	mu sync.Mutex

	id             string
	cmd            *exec.Cmd
	log            *jobBuffer
	done           chan struct{}
	status         string
	exitCode       *int
	errText        string
	stopReason     string
	startedAt      time.Time
	finishedAt     time.Time
	completionSeen bool
	userInitiated  bool
}

// jobBuffer continuously drains process output while retaining only a bounded
// tail. Long-running commands therefore cannot fill memory or block on stdout.
type jobBuffer struct {
	mu        sync.Mutex
	data      []byte
	limit     int64
	received  int64
	discarded int64
}

type bashArgs struct {
	Command        string `json:"command"`
	Workdir        string `json:"workdir,omitempty"`
	TimeoutSeconds int    `json:"timeout,omitempty"`
	Background     bool   `json:"background,omitempty"`
}

type jobArgs struct {
	Action string `json:"action"`
	JobID  string `json:"job_id,omitempty"`
}

func NewProcessManager(root, home, sandbox string, allowBackground bool) (*ProcessManager, error) {
	workspace, err := newWorkspaceToolset(root)
	if err != nil {
		return nil, err
	}
	toolHome := WorkspaceToolHome(home, workspace.root)
	if err := os.MkdirAll(toolHome, 0o700); err != nil {
		return nil, fmt.Errorf("create tool home: %w", err)
	}
	if err := os.Chmod(toolHome, 0o700); err != nil {
		return nil, fmt.Errorf("set tool home mode: %w", err)
	}
	if err := os.MkdirAll(WorkspaceToolTemp(toolHome), 0o700); err != nil {
		return nil, fmt.Errorf("create tool temp: %w", err)
	}
	return &processManager{
		workspace:       workspace,
		toolHome:        toolHome,
		logLimit:        defaultCommandLogLimit,
		sandbox:         sandbox,
		allowBackground: allowBackground,
		jobs:            make(map[string]*processJob),
	}, nil
}

func (m *processManager) Status(jobID string) (ProcessResultMeta, bool) {
	job := m.get(jobID)
	if job == nil {
		return ProcessResultMeta{}, false
	}
	return m.processMeta(job, true), true
}

// SetSandbox changes the policy used for future agent-originated Bash
// processes. Existing jobs keep the isolation with which they were started.
func (m *processManager) SetSandbox(policy string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return errors.New("process manager is closed")
	}
	m.sandbox = policy
	return nil
}

func (m *processManager) Tools() []golem.Tool {
	bashProperties := []jsonschema.Property{
		jsonschema.Required("command", jsonschema.Str{Description: "Bash command to run."}),
		jsonschema.Optional("workdir", jsonschema.Str{Description: "Directory relative to the workspace root. Defaults to the root."}),
		jsonschema.Optional("timeout", jsonschema.Int{Description: "Hard timeout in seconds. Defaults to 600; capped at 3600.", Minimum: new(1), Maximum: new(3600)}),
	}
	if m.allowBackground {
		bashProperties = append(bashProperties, jsonschema.Optional("background", jsonschema.Bool{Description: "Return immediately for a long-lived server or watcher. Ordinary commands stay in the foreground and yield automatically if still running after about 10 seconds."}))
	}
	return []golem.Tool{
		golem.FunctionToolWithEffect(
			golem.ToolEffectProcess,
			"bash",
			"Run Bash in the workspace with environment and filesystem access determined by the current sandbox policy, plus process-group cancellation, bounded output, and a hard timeout. Ordinary commands run in the foreground and become a managed job only when explicitly backgrounded or still running after about 10 seconds. Non-zero exits are results, not tool errors.",
			jsonschema.Obj(bashProperties...).NoAdditionalProperties(),
			m.bash,
		),
		golem.FunctionToolWithEffect(
			golem.ToolEffectProcess,
			"job",
			"Inspect or stop a managed Bash process. Actions: output, stop.",
			jsonschema.Obj(
				jsonschema.Required("action", jsonschema.Str{Description: "One of: output, stop."}),
				jsonschema.Required("job_id", jsonschema.Str{Description: "Job id returned by Bash."}),
			).NoAdditionalProperties(),
			m.job,
		),
	}
}

func (m *processManager) bash(ctx context.Context, call llm.ToolCall) (golem.ToolResult, error) {
	var args bashArgs
	if err := decodeToolArgs(call, &args); err != nil {
		return golem.ToolResult{}, err
	}
	return m.runBash(ctx, args, processOriginAgent)
}

// RunShell executes an explicit user command in the foreground with the ambient
// environment and permissions. It deliberately bypasses model sandboxing while
// retaining the process manager's cancellation, timeout, and output bounds.
func (m *processManager) RunShell(ctx context.Context, command string) (golem.ToolResult, error) {
	return m.runBash(ctx, bashArgs{Command: command}, processOriginUser)
}

func (m *processManager) runBash(ctx context.Context, args bashArgs, origin processOrigin) (golem.ToolResult, error) {
	args.Command = strings.TrimSpace(args.Command)
	if args.Command == "" {
		return golem.ToolResult{}, errors.New("command is required")
	}
	if err := ctx.Err(); err != nil {
		return golem.ToolResult{}, err
	}
	if args.Background && !m.allowBackground {
		return golem.ToolResult{}, errors.New("background Bash is unavailable in one-shot mode; run the command in the foreground")
	}
	workdir, display, info, err := m.workspace.resolveExistingPath(args.Workdir)
	if err != nil {
		return golem.ToolResult{}, err
	}
	if !info.IsDir() {
		return golem.ToolResult{}, fmt.Errorf("workdir %s is not a directory", display)
	}
	timeout := defaultBashTimeout
	if args.TimeoutSeconds > 0 {
		timeout = min(time.Duration(args.TimeoutSeconds)*time.Second, maxBashTimeout)
	}
	job, err := m.start(args.Command, workdir, timeout, origin)
	if err != nil {
		return golem.ToolResult{}, err
	}
	if args.Background {
		return golem.ToolResult{Content: m.formatJob(job, nil, false, true, false), Meta: m.processMeta(job, true)}, nil
	}
	if origin == processOriginUser {
		select {
		case <-job.done:
			return m.completedForegroundResult(job), nil
		case <-ctx.Done():
			_, _ = m.stop(context.Background(), job.id, "user shell cancelled")
			m.forget(job)
			return golem.ToolResult{}, ctx.Err()
		}
	}

	timer := time.NewTimer(defaultBashYield)
	defer timer.Stop()
	select {
	case <-job.done:
		return m.completedForegroundResult(job), nil
	case <-timer.C:
		output, truncated := job.log.snapshot(defaultCommandPreview)
		managed := true
		select {
		case <-job.done:
			return m.completedForegroundResult(job), nil
		default:
		}
		return golem.ToolResult{Content: m.formatJob(job, output, true, managed, truncated), Meta: m.processMeta(job, managed)}, nil
	case <-ctx.Done():
		_, _ = m.stop(context.Background(), job.id, "tool call cancelled")
		m.forget(job)
		return golem.ToolResult{}, ctx.Err()
	}
}

func (m *processManager) completedForegroundResult(job *processJob) golem.ToolResult {
	output, truncated := job.log.snapshot(defaultCommandPreview)
	m.markCompletionSeen(job)
	m.forget(job)
	return golem.ToolResult{
		Content: m.formatJob(job, output, true, false, truncated),
		Meta:    m.processMeta(job, false),
	}
}

func (m *processManager) job(ctx context.Context, call llm.ToolCall) (golem.ToolResult, error) {
	var args jobArgs
	if err := decodeToolArgs(call, &args); err != nil {
		return golem.ToolResult{}, err
	}
	job := m.get(args.JobID)
	if job == nil {
		return golem.ToolResult{}, fmt.Errorf("job %q not found", args.JobID)
	}
	switch strings.ToLower(strings.TrimSpace(args.Action)) {
	case "output":
		content, truncated := job.log.snapshot(maxJobReadBytes)
		m.markCompletionSeen(job)
		return golem.ToolResult{Content: m.formatJob(job, content, true, true, truncated), Meta: m.processMeta(job, true)}, nil
	case "stop":
		job, err := m.stop(ctx, job.id, "stopped by job tool")
		if err != nil {
			return golem.ToolResult{}, err
		}
		m.markCompletionSeen(job)
		content, truncated := job.log.snapshot(defaultCommandPreview)
		return golem.ToolResult{Content: m.formatJob(job, content, true, true, truncated), Meta: m.processMeta(job, true)}, nil
	default:
		return golem.ToolResult{}, errors.New("action must be one of: output, stop")
	}
}

func (m *processManager) start(command, workdir string, timeout time.Duration, origin processOrigin) (*processJob, error) {
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return nil, errors.New("process manager is closed")
	}
	sandbox := m.sandbox
	m.mu.Unlock()

	id, err := newJobID()
	if err != nil {
		return nil, err
	}
	var commandProcess *exec.Cmd
	switch origin {
	case processOriginUser:
		commandProcess = exec.Command("bash", "-lc", command)
		commandProcess.Dir = workdir
		commandProcess.Env = os.Environ()
	case processOriginAgent:
		commandProcess, err = sandboxedBashCommand(command, m.workspace.root, workdir, m.toolHome, sandbox)
		if err != nil {
			return nil, fmt.Errorf("prepare bash: %w", err)
		}
	default:
		return nil, fmt.Errorf("unknown process origin %d", origin)
	}
	configureProcessGroup(commandProcess)
	log := &jobBuffer{limit: m.logLimit}
	commandProcess.Stdout = log
	commandProcess.Stderr = log
	if err := commandProcess.Start(); err != nil {
		return nil, fmt.Errorf("start bash: %w", err)
	}
	job := &processJob{
		id:            id,
		cmd:           commandProcess,
		log:           log,
		done:          make(chan struct{}),
		status:        jobRunning,
		startedAt:     time.Now().UTC(),
		userInitiated: origin == processOriginUser,
	}
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		_ = killProcessGroup(commandProcess)
		_ = commandProcess.Wait()
		return nil, errors.New("process manager closed while starting job")
	}
	m.jobs[id] = job
	m.mu.Unlock()
	go m.monitor(job, timeout)
	return job, nil
}

func (m *processManager) monitor(job *processJob, timeout time.Duration) {
	wait := make(chan error, 1)
	go func() { wait <- job.cmd.Wait() }()
	var waitErr error
	timedOut := false
	if timeout > 0 {
		timer := time.NewTimer(timeout)
		select {
		case waitErr = <-wait:
			timer.Stop()
		case <-timer.C:
			timedOut = true
			stopped := make(chan struct{})
			go keepKillingProcessGroup(job.cmd, stopped)
			waitErr = <-wait
			close(stopped)
		}
	} else {
		waitErr = <-wait
	}

	job.mu.Lock()
	job.finishedAt = time.Now().UTC()
	job.exitCode = processExitCode(waitErr)
	job.errText = processErrorText(waitErr)
	switch {
	case timedOut:
		job.status = jobTimedOut
		job.stopReason = fmt.Sprintf("timeout after %s", timeout)
	case job.stopReason != "":
		job.status = jobKilled
	case waitErr != nil:
		job.status = jobFailed
	default:
		job.status = jobCompleted
		zero := 0
		job.exitCode = &zero
	}
	job.mu.Unlock()
	close(job.done)
}

// CompletionEvent is one background job reporting that it finished. The id
// travels beside the text, rather than only inside it, so a caller can record
// which job was reported without reading the sentence back.
type CompletionEvent struct {
	JobID   string
	Content string
}

// DeliverCompletionEvents reports unobserved background completions once for
// the lifetime of this Cy process. Nothing is persisted across restarts.
func (m *processManager) DeliverCompletionEvents(_ string) ([]CompletionEvent, error) {
	m.mu.Lock()
	jobs := make([]*processJob, 0, len(m.jobs))
	for _, job := range m.jobs {
		jobs = append(jobs, job)
	}
	m.mu.Unlock()
	sort.Slice(jobs, func(i, j int) bool { return jobs[i].startedAt.Before(jobs[j].startedAt) })

	var delivered []CompletionEvent
	for _, job := range jobs {
		job.mu.Lock()
		if job.status == jobRunning || job.completionSeen {
			job.mu.Unlock()
			continue
		}
		job.completionSeen = true
		status := job.status
		exitCode := job.exitCode
		errText := job.errText
		id := job.id
		job.mu.Unlock()
		content := fmt.Sprintf("Background job %s completed: status=%s", id, status)
		if exitCode != nil {
			content += fmt.Sprintf(", exit_code=%d", *exitCode)
		}
		if errText != "" {
			content += ", error=" + errText
		}
		content += ". Inspect output with job(action=\"output\", job_id=\"" + id + "\")."
		delivered = append(delivered, CompletionEvent{JobID: id, Content: content})
	}
	return delivered, nil
}

func (m *processManager) markCompletionSeen(job *processJob) {
	job.mu.Lock()
	if job.status != jobRunning {
		job.completionSeen = true
	}
	job.mu.Unlock()
}

func (m *processManager) stop(ctx context.Context, id, reason string) (*processJob, error) {
	job := m.get(id)
	if job == nil {
		return nil, fmt.Errorf("job %q not found", id)
	}
	job.mu.Lock()
	if job.status != jobRunning {
		job.mu.Unlock()
		return job, nil
	}
	job.stopReason = reason
	job.mu.Unlock()
	stopped := make(chan struct{})
	defer close(stopped)
	go keepKillingProcessGroup(job.cmd, stopped)
	select {
	case <-job.done:
		return job, nil
	case <-ctx.Done():
		return job, ctx.Err()
	case <-time.After(jobStopTimeout):
		return job, errors.New("timed out waiting for job to stop")
	}
}

// keepKillingProcessGroup signals the group until the caller closes stop.
//
// One signal is not enough. The kernel walks the process group to deliver it,
// and a child forked while that walk is in progress joins the group without
// being signalled, so a command that is mid-fork when we kill it can leave a
// survivor behind. The survivor holds the write end of the pipe the job's
// output is read through, which means the job does not merely leak a process:
// Wait blocks in its output copier long after the leader has been reaped, and
// the stop reports a timeout for a process group that is still alive.
//
// Signalling again is safe. A process group id stays reserved for as long as
// the group has a member, so while there is anything left to kill the id cannot
// have been recycled onto some unrelated group.
func keepKillingProcessGroup(command *exec.Cmd, stop <-chan struct{}) {
	for {
		select {
		case <-stop:
			return
		default:
		}
		_ = killProcessGroup(command)
		select {
		case <-stop:
			return
		case <-time.After(jobKillRetryInterval):
		}
	}
}

func (m *processManager) get(id string) *processJob {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.jobs[strings.TrimSpace(id)]
}

func (m *processManager) forget(job *processJob) {
	m.mu.Lock()
	if m.jobs[job.id] == job {
		delete(m.jobs, job.id)
	}
	m.mu.Unlock()
}

func (m *processManager) Close() error {
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return nil
	}
	m.closed = true
	jobs := make([]*processJob, 0, len(m.jobs))
	for _, job := range m.jobs {
		jobs = append(jobs, job)
	}
	m.mu.Unlock()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var errs []error
	for _, job := range jobs {
		job.mu.Lock()
		running := job.status == jobRunning
		job.mu.Unlock()
		if running {
			if _, err := m.stop(ctx, job.id, "cy exiting"); err != nil {
				errs = append(errs, err)
			}
		}
	}
	return errors.Join(errs...)
}

func (m *processManager) formatJob(job *processJob, output []byte, includeOutput, managed, truncated bool) string {
	job.mu.Lock()
	status := job.status
	exitCode := job.exitCode
	errText := job.errText
	job.mu.Unlock()
	var out strings.Builder
	if managed {
		fmt.Fprintf(&out, "job_id: %s\n", job.id)
	}
	fmt.Fprintf(&out, "status: %s\n", status)
	if exitCode != nil {
		fmt.Fprintf(&out, "exit_code: %d\n", *exitCode)
	}
	if errText != "" {
		fmt.Fprintf(&out, "error: %s\n", errText)
	}
	if truncated {
		out.WriteString("truncated: true\n")
	}
	if status == jobRunning {
		out.WriteString("continue: job(action=\"output\", job_id=\"" + job.id + "\")\n")
	}
	if includeOutput {
		out.WriteByte('\n')
		out.Write(output)
		if len(output) > 0 && output[len(output)-1] != '\n' {
			out.WriteByte('\n')
		}
	}
	return out.String()
}

func (m *processManager) processMeta(job *processJob, managed bool) processResultMeta {
	job.mu.Lock()
	meta := processResultMeta{
		Type:           processResultMetaType,
		Status:         job.status,
		ExitCode:       job.exitCode,
		DurationMillis: jobDuration(job.startedAt, job.finishedAt).Milliseconds(),
		UserInitiated:  job.userInitiated,
	}
	if managed {
		meta.JobID = job.id
	}
	job.mu.Unlock()
	meta.OutputBytes, meta.DiscardedBytes = job.log.stats()
	if meta.Status != jobRunning && meta.Status != jobCompleted && meta.OutputBytes > 0 {
		tail, _ := job.log.snapshot(processFailureTailSize)
		meta.FailureTail = strings.TrimSpace(string(tail))
	}
	return meta
}

func (l *jobBuffer) Write(data []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	original := len(data)
	l.received += int64(original)
	if l.limit <= 0 {
		l.discarded = l.received
		return original, nil
	}
	l.data = append(l.data, data...)
	if int64(len(l.data)) > l.limit {
		keep := int(l.limit)
		copy(l.data, l.data[len(l.data)-keep:])
		l.data = l.data[:keep]
	}
	l.discarded = l.received - int64(len(l.data))
	return original, nil
}

func (l *jobBuffer) snapshot(limit int) ([]byte, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	start := 0
	if limit > 0 && len(l.data) > limit {
		start = len(l.data) - limit
	}
	data := append([]byte(nil), l.data[start:]...)
	if !utf8.Valid(data) {
		data = []byte(strings.ToValidUTF8(string(data), "�"))
	}
	return data, start > 0 || l.discarded > 0
}

func (l *jobBuffer) stats() (stored, discarded int64) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return int64(len(l.data)), l.discarded
}

func newJobID() (string, error) {
	var raw [6]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", err
	}
	return "job-" + hex.EncodeToString(raw[:]), nil
}

func processExitCode(err error) *int {
	if err == nil {
		zero := 0
		return &zero
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		code := exitErr.ExitCode()
		return &code
	}
	return nil
}

func processErrorText(err error) string {
	if err == nil {
		return ""
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		return ""
	}
	return err.Error()
}

func jobDuration(started, finished time.Time) time.Duration {
	if finished.IsZero() {
		finished = time.Now().UTC()
	}
	return finished.Sub(started)
}
