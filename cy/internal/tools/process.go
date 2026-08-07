package tools

import (
	"cmp"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
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
	defaultJobWait         = time.Minute
	maxCommandSummary      = 120
)

const (
	jobRunning   = "running"
	jobCompleted = "completed"
	jobFailed    = "failed"
	jobKilled    = "killed"
	jobTimedOut  = "timed_out"
	// jobNotStarted is the program never running at all: missing, not
	// executable, a bad interpreter line. Its own status because only the
	// supervisor can see it -- Cy forks the supervisor, which starts fine --
	// and because it is the one outcome no amount of rephrasing will fix.
	jobNotStarted = "not_started"
)

const (
	JobRunning    = jobRunning
	JobCompleted  = jobCompleted
	JobFailed     = jobFailed
	JobKilled     = jobKilled
	JobTimedOut   = jobTimedOut
	JobNotStarted = jobNotStarted
)

type processOrigin uint8

const (
	processOriginAgent processOrigin = iota
	processOriginUser
)

type processManager struct {
	workspace       *workspaceTools
	home            string
	toolHome        string
	sessionID       string
	jobLauncher     []string
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
	command        string
	mailbox        string
	cmd            *exec.Cmd
	log            *jobBuffer
	errLog         *jobBuffer
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
	Action         string `json:"action"`
	JobID          string `json:"job_id,omitempty"`
	TimeoutSeconds int    `json:"timeout,omitempty"`
}

// ProcessOptions is everything a process manager has to be told before it runs
// anything. A struct rather than a parameter list because most of these are
// strings and would otherwise be distinguishable only by position.
type ProcessOptions struct {
	Root      string
	Home      string
	SessionID string
	Sandbox   string
	// Background says whether the model may ask for work that outlives its
	// tool call.
	Background bool
	// JobLauncher is the program that supervises a job. Empty is the built-in
	// supervisor, which is this binary re-exec'd; a configured one is how a job
	// comes to run somewhere Cy knows nothing about.
	JobLauncher string
	// Delivered are the jobs whose completion the model has already been told
	// about, as the journal has it. Only the journal knows: the registry says a
	// job finished, and a mailbox left behind by a run that stopped between
	// telling the model and tidying up looks exactly like one that finished
	// unheard.
	Delivered map[string]struct{}
}

func NewProcessManager(opts ProcessOptions) (*ProcessManager, error) {
	workspace, err := newWorkspaceToolset(opts.Root)
	if err != nil {
		return nil, err
	}
	launcher := []string{opts.JobLauncher}
	if opts.JobLauncher == "" {
		self, err := os.Executable()
		if err != nil {
			return nil, fmt.Errorf("locate the Cy binary to supervise jobs: %w", err)
		}
		launcher = []string{self, jobChildArg}
	}
	toolHome := WorkspaceToolHome(opts.Home, workspace.root)
	if err := os.MkdirAll(toolHome, 0o700); err != nil {
		return nil, fmt.Errorf("create tool home: %w", err)
	}
	if err := os.Chmod(toolHome, 0o700); err != nil {
		return nil, fmt.Errorf("set tool home mode: %w", err)
	}
	if err := os.MkdirAll(WorkspaceToolTemp(toolHome), 0o700); err != nil {
		return nil, fmt.Errorf("create tool temp: %w", err)
	}
	manager := &processManager{
		workspace:       workspace,
		home:            opts.Home,
		toolHome:        toolHome,
		sessionID:       opts.SessionID,
		jobLauncher:     launcher,
		logLimit:        defaultCommandLogLimit,
		sandbox:         opts.Sandbox,
		allowBackground: opts.Background,
		jobs:            make(map[string]*processJob),
	}
	manager.adoptMailboxes(opts.Delivered)
	return manager, nil
}

// adoptMailboxes takes over the jobs an earlier run of this session left in the
// registry. A job with a result is a finished job, and becomes an ordinary
// entry here: the model can be told it ended, and can read its output, exactly
// as if this process had started it.
//
// A job without one is a job whose supervisor never got to write, which today
// can only mean it died with the run that started it -- nothing yet outlives
// Cy. Its mailbox is removed. C10f is what gives a job the right to be still
// running here, and it is that commit's business to tell the two apart.
//
// Best effort throughout. An unreadable registry costs the run its outstanding
// completions, which is bad, but refusing to start costs it everything.
func (m *processManager) adoptMailboxes(delivered map[string]struct{}) {
	entries, err := os.ReadDir(m.jobsDir())
	if err != nil {
		return
	}
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		id := entry.Name()
		mailbox := m.mailboxFor(id)
		result, ok := readJobResult(mailbox)
		if !ok {
			_ = os.RemoveAll(mailbox)
			continue
		}
		job := &processJob{
			id:             id,
			command:        readJobCommand(mailbox),
			mailbox:        mailbox,
			log:            &jobBuffer{limit: m.logLimit},
			done:           closedChannel(),
			status:         cmp.Or(result.Status, jobCompleted),
			exitCode:       result.ExitCode,
			errText:        result.Error,
			startedAt:      result.StartedAt,
			finishedAt:     result.FinishedAt,
			completionSeen: contains(delivered, id),
		}
		if tail, ok := readJobOutput(mailbox); ok {
			job.log.adopt(tail, result.OutputBytes)
		}
		// Already reported, and only the tidying was missed. Nothing here needs
		// it any more, so finish what the last run started.
		if job.completionSeen {
			_ = os.RemoveAll(mailbox)
			continue
		}
		m.jobs[id] = job
	}
}

func closedChannel() chan struct{} {
	done := make(chan struct{})
	close(done)
	return done
}

func contains(set map[string]struct{}, key string) bool {
	_, ok := set[key]
	return ok
}

// jobsDir is where this session's supervisors report. Under the state home
// rather than the tool home, which is the point rather than a detail: the tool
// home is granted to the fenced child, so a job able to write its own mailbox
// would be a job able to forge its own completion -- and what is behind the
// fence is the record everything downstream believes.
func (m *processManager) jobsDir() string {
	return filepath.Join(m.home, "jobs", cmp.Or(m.sessionID, "no-session"))
}

func (m *processManager) mailboxFor(jobID string) string {
	return filepath.Join(m.jobsDir(), jobID)
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
			"Inspect, wait for, or stop managed Bash processes. Actions: list, output, wait, stop.",
			jsonschema.Obj(
				jsonschema.Required("action", jsonschema.Str{Description: "One of: list, output, wait, stop."}),
				jsonschema.Optional("job_id", jsonschema.Str{Description: "Job id returned by Bash. Required by every action except list."}),
				jsonschema.Optional("timeout", jsonschema.Int{Description: "For wait: seconds to block before returning whatever the job's state is by then. Defaults to 60; capped at 3600.", Minimum: new(1), Maximum: new(3600)}),
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
		return golem.ToolResult{}, errors.New("background Bash is turned off for this run; run the command in the foreground")
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
		output, truncated := job.snapshot(defaultCommandPreview)
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
	output, truncated := job.snapshot(defaultCommandPreview)
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
	action := strings.ToLower(strings.TrimSpace(args.Action))
	if action == "list" {
		return golem.ToolResult{Content: m.listJobs()}, nil
	}
	job := m.get(args.JobID)
	if job == nil {
		return golem.ToolResult{}, fmt.Errorf("job %q not found", args.JobID)
	}
	switch action {
	case "output", "wait":
		if action == "wait" {
			if err := m.await(ctx, job, args.TimeoutSeconds); err != nil {
				return golem.ToolResult{}, err
			}
		}
		content, truncated := job.snapshot(maxJobReadBytes)
		m.markCompletionSeen(job)
		return golem.ToolResult{Content: m.formatJob(job, content, true, true, truncated), Meta: m.processMeta(job, true)}, nil
	case "stop":
		job, err := m.stop(ctx, job.id, "stopped by job tool")
		if err != nil {
			return golem.ToolResult{}, err
		}
		m.markCompletionSeen(job)
		content, truncated := job.snapshot(defaultCommandPreview)
		return golem.ToolResult{Content: m.formatJob(job, content, true, true, truncated), Meta: m.processMeta(job, true)}, nil
	default:
		return golem.ToolResult{}, errors.New("action must be one of: list, output, wait, stop")
	}
}

// await blocks until the job ends or the bound expires. An expired bound is a
// result rather than an error -- the caller asked to wait a while, and "still
// running" answers that -- but it is always a bound: a wait can be configured
// and can never be unlimited. Capped at the longest a job may live, so
// a caller cannot ask to wait past the point where there is anything to wait
// for.
func (m *processManager) await(ctx context.Context, job *processJob, seconds int) error {
	wait := defaultJobWait
	if seconds > 0 {
		wait = min(time.Duration(seconds)*time.Second, maxBashTimeout)
	}
	timer := time.NewTimer(wait)
	defer timer.Stop()
	select {
	case <-job.done:
	case <-timer.C:
	case <-ctx.Done():
		return ctx.Err()
	}
	return nil
}

// listJobs answers "what did I start, and how does it stand" -- the half of
// visibility that ids handed back one at a time do not give, and the thing that
// makes choosing to wait possible at all.
func (m *processManager) listJobs() string {
	m.mu.Lock()
	jobs := make([]*processJob, 0, len(m.jobs))
	for _, job := range m.jobs {
		jobs = append(jobs, job)
	}
	m.mu.Unlock()
	if len(jobs) == 0 {
		return "no managed jobs\n"
	}
	sort.Slice(jobs, func(i, j int) bool { return jobs[i].startedAt.Before(jobs[j].startedAt) })
	var out strings.Builder
	for _, job := range jobs {
		job.mu.Lock()
		fmt.Fprintf(&out, "%s %s %s", job.id, job.status, jobDuration(job.startedAt, job.finishedAt).Truncate(time.Second))
		if job.exitCode != nil {
			fmt.Fprintf(&out, " exit_code=%d", *job.exitCode)
		}
		fmt.Fprintf(&out, " %s\n", summarizeCommand(job.command))
		job.mu.Unlock()
	}
	return out.String()
}

// summarizeCommand cuts a command down to one line that fits beside a job id.
func summarizeCommand(command string) string {
	command = strings.TrimSpace(command)
	if line, _, found := strings.Cut(command, "\n"); found {
		command = strings.TrimSpace(line) + " ..."
	}
	if runes := []rune(command); len(runes) > maxCommandSummary {
		command = strings.TrimSpace(string(runes[:maxCommandSummary])) + "..."
	}
	return command
}

// jobSpec is a program somebody else has already prepared, plus the few things
// the job machinery cannot work out for itself. The caller decides what runs
// and behind which fence; everything that makes a job a job -- the id, the
// mailbox, the supervisor, the process group, the bounded output, the monitor
// -- happens once, here, so that a shell and a configured program differ only
// in the command handed in.
type jobSpec struct {
	command string
	process *exec.Cmd
	timeout time.Duration
	stdin   io.Reader
	// stderr keeps the job's two streams apart. Nil is one buffer for both,
	// which is what a shell wants; a tool whose answer is on stdout wants its
	// logging kept out of the way.
	stderr *jobBuffer
	// supervise is false only for the user's own shell, which bypasses the
	// fence and the scrubbed environment already, never becomes a managed job,
	// and has nobody to deliver its completion to but the person who typed it.
	supervise     bool
	userInitiated bool
}

func (m *processManager) startJob(spec jobSpec) (*processJob, error) {
	m.mu.Lock()
	closed := m.closed
	m.mu.Unlock()
	if closed {
		return nil, errors.New("process manager is closed")
	}
	id, err := newJobID()
	if err != nil {
		return nil, err
	}
	process := spec.process
	mailbox := ""
	if spec.supervise {
		if process.Err != nil {
			return nil, process.Err
		}
		mailbox = m.mailboxFor(id)
		if err := os.MkdirAll(mailbox, 0o700); err != nil {
			return nil, fmt.Errorf("create job mailbox: %w", err)
		}
		// Cy's own note in the mailbox, not the supervisor's: what was asked
		// for, rather than the argv that came of it. Without it a job picked up
		// by a later run can be listed only as an id.
		_ = writeJobFile(mailbox, jobCommandFile, []byte(spec.command))
		process = superviseCommand(m.jobLauncher, mailbox, process)
	}
	configureProcessGroup(process)
	log := &jobBuffer{limit: m.logLimit}
	process.Stdin = spec.stdin
	process.Stdout = log
	process.Stderr = log
	if spec.stderr != nil {
		process.Stderr = spec.stderr
	}
	if err := process.Start(); err != nil {
		return nil, err
	}
	job := &processJob{
		id:            id,
		command:       spec.command,
		mailbox:       mailbox,
		cmd:           process,
		log:           log,
		errLog:        spec.stderr,
		done:          make(chan struct{}),
		status:        jobRunning,
		startedAt:     time.Now().UTC(),
		userInitiated: spec.userInitiated,
	}
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		_ = killProcessGroup(process)
		_ = process.Wait()
		return nil, errors.New("process manager closed while starting job")
	}
	m.jobs[id] = job
	m.mu.Unlock()
	go m.monitor(job, spec.timeout)
	return job, nil
}

func (m *processManager) start(command, workdir string, timeout time.Duration, origin processOrigin) (*processJob, error) {
	m.mu.Lock()
	sandbox := m.sandbox
	m.mu.Unlock()

	var commandProcess *exec.Cmd
	var err error
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
	job, err := m.startJob(jobSpec{
		command:       command,
		process:       commandProcess,
		timeout:       timeout,
		supervise:     origin == processOriginAgent,
		userInitiated: origin == processOriginUser,
	})
	if err != nil {
		return nil, fmt.Errorf("start bash: %w", err)
	}
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
	// What the supervisor wrote down beats what this process observed. Today
	// they agree, since this process forked the supervisor and waited on it.
	// The read is here anyway so that the one path reporting a job's fate is
	// the one that still works when the supervisor was forked by a Cy that has
	// since exited -- and so that it is exercised on every job rather than only
	// after a restart.
	//
	// Absent for a job killed hard: the supervisor dies with the group it
	// leads, before it can write. That is why this overlays rather than
	// replaces -- Cy's own account of a job it killed is the only one there is.
	if result, ok := readJobResult(job.mailbox); ok {
		// The one status the supervisor knows better than Cy does: from here
		// the launcher exited, which looks like an ordinary failure.
		if result.Status == jobNotStarted {
			job.status = jobNotStarted
		}
		if result.ExitCode != nil {
			job.exitCode = result.ExitCode
		}
		if result.Error != "" {
			job.errText = result.Error
		}
		if !result.FinishedAt.IsZero() {
			job.finishedAt = result.FinishedAt
		}
		// Not for a job whose two streams are kept apart: this process still
		// holds the finer copy, and the merged file is for the process that
		// does not.
		if job.errLog == nil {
			if tail, ok := readJobOutput(job.mailbox); ok {
				job.log.adopt(tail, result.OutputBytes)
			}
		}
	}
	job.mu.Unlock()
	close(job.done)
}

// CompletionEvent is one background job reporting that it finished. The id and
// the time travel beside the text, rather than only inside it, so a caller can
// record which job was reported, and when it ended, without reading the
// sentence back.
type CompletionEvent struct {
	JobID      string
	FinishedAt time.Time
	Content    string
}

// PendingCompletionEvents reports background completions nobody has been told
// about yet. It does not mark them told: the caller does that with
// MarkCompletionDelivered, once the event is somewhere that survives this
// function returning. Peeking and acknowledging are separate because between
// them is the part that can fail, and a completion consumed by a failed
// delivery is one the model never hears about at all.
//
// Once per completion, across restarts as well as within a run: a job the last
// run left unreported was adopted from the registry at startup and is offered
// here like any other.
func (m *processManager) PendingCompletionEvents(_ string) ([]CompletionEvent, error) {
	m.mu.Lock()
	jobs := make([]*processJob, 0, len(m.jobs))
	for _, job := range m.jobs {
		jobs = append(jobs, job)
	}
	m.mu.Unlock()
	sort.Slice(jobs, func(i, j int) bool { return jobs[i].startedAt.Before(jobs[j].startedAt) })

	var pending []CompletionEvent
	for _, job := range jobs {
		job.mu.Lock()
		if job.status == jobRunning || job.completionSeen {
			job.mu.Unlock()
			continue
		}
		status := job.status
		exitCode := job.exitCode
		errText := job.errText
		id := job.id
		finishedAt := job.finishedAt
		job.mu.Unlock()
		content := fmt.Sprintf("Background job %s completed: status=%s", id, status)
		if exitCode != nil {
			content += fmt.Sprintf(", exit_code=%d", *exitCode)
		}
		if errText != "" {
			content += ", error=" + errText
		}
		content += ". Inspect output with job(action=\"output\", job_id=\"" + id + "\")."
		pending = append(pending, CompletionEvent{JobID: id, FinishedAt: finishedAt, Content: content})
	}
	return pending, nil
}

// MarkCompletionDelivered records that a job's completion has been told to the
// model and must not be told again. Acknowledging a job that is unknown or
// already acknowledged is not an error: the caller is asserting an outcome, not
// asking for one.
//
// The mailbox goes with it, because a mailbox is what makes a completion
// outstanding. The caller only reaches here once the boundary event is in the
// journal, so the two disagree in one direction only: a crash in between leaves
// a mailbox for a job the model was already told about, and the journal is what
// settles that on the next start.
//
// The output is not removed with it. It stays in this process's cache, so
// job(action="output") still answers for the rest of the run -- which is the
// point at which the model is most likely to ask.
func (m *processManager) MarkCompletionDelivered(id string) error {
	job := m.get(id)
	if job == nil {
		return nil
	}
	job.mu.Lock()
	job.completionSeen = true
	mailbox := job.mailbox
	job.mu.Unlock()
	if mailbox != "" {
		_ = os.RemoveAll(mailbox)
	}
	return nil
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
	if job.mailbox != "" {
		_ = os.RemoveAll(job.mailbox)
	}
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
		mailbox := job.mailbox
		job.mu.Unlock()
		if running {
			if _, err := m.stop(ctx, job.id, "cy exiting"); err != nil {
				errs = append(errs, err)
			}
		}
		// Every job dies with Cy, so the only thing worth leaving behind is a
		// completion nobody has been told about: work that finished on its own
		// between the last boundary and now, and would otherwise fall in the gap
		// between the run that saw it end and the run that could have said so.
		// A job killed just above is not that -- its work is dead and a future
		// run has nothing to report about it.
		job.mu.Lock()
		outstanding := !running && !job.completionSeen
		job.mu.Unlock()
		if !outstanding && mailbox != "" {
			_ = os.RemoveAll(mailbox)
		}
	}
	// Removes the session's directory when it is empty, which is the whole of
	// the common case, and leaves it alone when a completion is still waiting.
	_ = os.Remove(m.jobsDir())
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
		out.WriteString("continue: job(action=\"wait\"|\"output\"|\"stop\", job_id=\"" + job.id + "\")\n")
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
	meta.OutputBytes, meta.DiscardedBytes = job.stats()
	if meta.Status != jobRunning && meta.Status != jobCompleted && meta.OutputBytes > 0 {
		tail, _ := job.snapshot(processFailureTailSize)
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

// adopt replaces the cached tail with the supervisor's copy of it. While Cy
// holds the pipe the two are the same bytes, so this changes nothing today; the
// point is that from here on the file is the account of what a job printed and
// the buffer is a cache of it, which is what lets a job be read by a Cy that
// never held its pipe. received is everything the job printed, so a tail that
// was cut still says how much is missing.
//
// Trimmed to this buffer's own bound, which is what a cache does. The two are
// the same by default; a Cy that keeps less than the supervisor wrote should
// still keep only that much.
func (l *jobBuffer) adopt(data []byte, received int64) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.received = max(received, int64(len(data)))
	switch {
	case l.limit <= 0:
		l.data = nil
	case int64(len(data)) > l.limit:
		l.data = data[int64(len(data))-l.limit:]
	default:
		l.data = data
	}
	l.discarded = l.received - int64(len(l.data))
}

// snapshot is the job's output as one account of it. Two buffers become one
// here, stdout then stderr, for the readers that have no reason to care; the
// caller that asked for them apart holds the buffers and keeps them apart.
func (j *processJob) snapshot(limit int) ([]byte, bool) {
	out, truncated := j.log.snapshot(limit)
	if j.errLog == nil {
		return out, truncated
	}
	errOut, errTruncated := j.errLog.snapshot(limit)
	return append(out, errOut...), truncated || errTruncated
}

func (j *processJob) stats() (stored, discarded int64) {
	stored, discarded = j.log.stats()
	if j.errLog != nil {
		errStored, errDiscarded := j.errLog.stats()
		stored, discarded = stored+errStored, discarded+errDiscarded
	}
	return stored, discarded
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
