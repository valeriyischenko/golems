package tools

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/levmv/golems/cy/internal/session"
	"github.com/levmv/golems/pkg/golem"
	"github.com/levmv/golems/pkg/jsonschema"
	"github.com/levmv/golems/pkg/llm"
)

const (
	defaultExternalTimeout = 10 * time.Minute
	maxExternalTimeout     = time.Hour
)

// A tool name has to survive being written into a provider's function-calling
// schema, and providers agree on roughly this much and disagree past it.
var externalToolNamePattern = regexp.MustCompile(`^[a-zA-Z][a-zA-Z0-9_]{0,63}$`)

// ExternalTool is one declaration from the tools file. The JSON tags are the
// file's format, so changing them changes configuration somebody has written.
type ExternalTool struct {
	Name        string            `json:"name"`
	Description string            `json:"description"`
	Effect      string            `json:"effect"`
	Command     []string          `json:"command"`
	Parameters  jsonschema.Schema `json:"parameters"`
	Timeout     int               `json:"timeout,omitempty"`
	Workdir     string            `json:"workdir,omitempty"`
	Env         map[string]string `json:"env,omitempty"`
	// Sandbox is what this tool needs on top of the run-level grants, and what
	// it should not have despite them. An interpreter whose packages live under
	// the real home is the case to expect: it cannot work inside a fence of the
	// workspace and its own tool home, and lifting the fence for every tool to
	// suit one of them is the trade this avoids.
	Sandbox SandboxGrants `json:"sandbox,omitzero"`
	// Background says who decides that this tool's work outlives the call.
	Background BackgroundMode `json:"background,omitempty"`
	// Yield is how many seconds a foreground call waits before it hands the
	// model a job id and lets the tool run on. Zero waits for the tool.
	//
	// Separate from Background because they answer different questions. The
	// mode says whether the model may choose to walk away; the yield says how
	// long the turn is willing to stand still, which is Cy's business and not
	// the model's. A build nobody should background on purpose still should not
	// hold a turn open for ten minutes.
	Yield int `json:"yield,omitempty"`
	// Detach lets this tool's work survive the run that started it. Cy stops
	// every other job on the way out; a detached one is left where it is, and
	// the next run of the same session picks it up from the registry.
	//
	// Off by default, and deliberately per tool rather than per run. A process
	// Cy no longer supervises is a process nobody will notice going wrong, so
	// it is worth having only for the few tools whose work is the point --
	// an index, a build, a sync -- and worth stating one tool at a time.
	Detach bool `json:"detach,omitempty"`
}

// BackgroundMode is who decides that a configured tool keeps running after the
// call that started it returns.
//
// never is the default, and the only thing that ever happened before. auto puts
// a background flag in the tool's own schema and lets the model choose per
// call. always is for a tool that only ever starts something -- a server, a
// watcher -- and has nothing to say by the time it would have returned.
type BackgroundMode string

const (
	BackgroundNever  BackgroundMode = "never"
	BackgroundAuto   BackgroundMode = "auto"
	BackgroundAlways BackgroundMode = "always"
)

// backgroundArg is the flag auto adds to a tool's schema, and the one name a
// tool declaring auto may not use for a parameter of its own.
const backgroundArg = "background"

// UnmarshalJSON accepts a bool as well as a name, because the key was a bool
// first and because true and false are what anyone writing it reaches for.
func (b *BackgroundMode) UnmarshalJSON(raw []byte) error {
	var name string
	if err := json.Unmarshal(raw, &name); err == nil {
		*b = BackgroundMode(strings.ToLower(strings.TrimSpace(name)))
		return nil
	}
	var always bool
	if err := json.Unmarshal(raw, &always); err != nil {
		return errors.New("background must be never, auto, always, or a boolean")
	}
	*b = BackgroundNever
	if always {
		*b = BackgroundAlways
	}
	return nil
}

// ToolConfig is a tools file as one run resolved it: the tools it declares, and
// the fence they and Bash run behind.
//
// The sandbox block is here rather than in a file of its own because the
// per-tool blocks have to live beside the tools anyway, and two files that must
// agree about the same vocabulary is one more thing to get wrong.
type ToolConfig struct {
	Sandbox SandboxGrants `json:"sandbox,omitzero"`
	// Env is given to every tool process this run, Bash included. A sandboxed
	// process starts from an empty environment, so anything the deployment set
	// around Cy -- a proxy above all, which is how a fenced host lets anything
	// out at all -- is invisible to tools until it is named here.
	Env   map[string]string `json:"env,omitempty"`
	Tools []ExternalTool    `json:"tools"`
}

// LoadExternalTools reads the tool declarations at path. No file means no
// external tools, which is the ordinary case; a file that does not parse or
// does not validate is a configuration error, because the alternative is a run
// that silently offers the model fewer tools than its author declared.
//
// Sandbox paths are resolved against the directory holding the file, so a
// deployment can be copied elsewhere without being edited, and are checked to
// exist here so that a mistyped one costs a configuration exit rather than a
// fence that quietly grants or hides something other than what was written.
func LoadExternalTools(path string) (ToolConfig, error) {
	raw, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return ToolConfig{}, nil
	}
	if err != nil {
		return ToolConfig{}, fmt.Errorf("read tool config %s: %w", path, err)
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	var file ToolConfig
	if err := decoder.Decode(&file); err != nil {
		return ToolConfig{}, fmt.Errorf("parse tool config %s: %w", path, err)
	}
	if decoder.Decode(&struct{}{}) != io.EOF {
		return ToolConfig{}, fmt.Errorf("parse tool config %s: multiple JSON values", path)
	}
	base := filepath.Dir(path)
	if err := file.Sandbox.resolve(base); err != nil {
		return ToolConfig{}, fmt.Errorf("tool config %s: %w", path, err)
	}
	if err := validateToolEnv(file.Env); err != nil {
		return ToolConfig{}, fmt.Errorf("tool config %s: %w", path, err)
	}
	declared := make(map[string]bool, len(file.Tools))
	for index := range file.Tools {
		tool := &file.Tools[index]
		label := strings.TrimSpace(tool.Name)
		if label == "" {
			label = fmt.Sprintf("entry %d", index+1)
		}
		if err := tool.normalize(base); err != nil {
			return ToolConfig{}, fmt.Errorf("tool %s in %s: %w", label, path, err)
		}
		if declared[tool.Name] {
			return ToolConfig{}, fmt.Errorf("tool config %s declares %q twice", path, tool.Name)
		}
		declared[tool.Name] = true
	}
	return file, nil
}

func (t *ExternalTool) normalize(base string) error {
	t.Name = strings.TrimSpace(t.Name)
	if !externalToolNamePattern.MatchString(t.Name) {
		return errors.New("name must start with a letter and hold only letters, digits and underscores")
	}
	t.Description = strings.TrimSpace(t.Description)
	if t.Description == "" {
		return errors.New("description is required: it is all the model has to decide when to call the tool")
	}
	// No default. The effect decides whether calls run concurrently with
	// others and whether a restricted profile exposes the tool at all, so
	// guessing it wrong is silent rather than visible.
	t.Effect = strings.TrimSpace(t.Effect)
	switch golem.ToolEffect(t.Effect) {
	case golem.ToolEffectRead, golem.ToolEffectWrite, golem.ToolEffectProcess, golem.ToolEffectExternal:
	default:
		return fmt.Errorf("effect %q must be one of read, write, process, external", t.Effect)
	}
	if len(t.Command) == 0 || strings.TrimSpace(t.Command[0]) == "" {
		return errors.New("command must name a program to run")
	}
	t.Command[0] = strings.TrimSpace(t.Command[0])
	// The declared schema is what the model is shown and what it fills in, and
	// a tool call's arguments are always an object.
	if t.Parameters.Type == "" {
		t.Parameters.Type = jsonschema.TypeObject
	}
	if t.Parameters.Type != jsonschema.TypeObject {
		return fmt.Errorf("parameters must be a JSON Schema object, not %q", t.Parameters.Type)
	}
	if t.Timeout < 0 {
		return fmt.Errorf("timeout %d must not be negative", t.Timeout)
	}
	if t.Background == "" {
		t.Background = BackgroundNever
	}
	switch t.Background {
	case BackgroundNever, BackgroundAlways:
	case BackgroundAuto:
		// Refused rather than overwritten. The tool would be shown a schema its
		// author did not write and handed arguments with one field quietly
		// missing, and neither is visible from inside the tool.
		if _, taken := t.Parameters.Properties[backgroundArg]; taken {
			return errors.New("background auto adds a background parameter to this tool's schema and the schema already declares one; rename it, or choose never or always")
		}
	default:
		return fmt.Errorf("background %q must be one of never, auto, always", t.Background)
	}
	if t.Yield < 0 {
		return fmt.Errorf("yield %d must not be negative", t.Yield)
	}
	if t.Yield > 0 && t.Background == BackgroundAlways {
		return errors.New("yield is how long a foreground call waits, and background always never makes one")
	}
	// Nothing to detach from otherwise: a call that is always waited for is
	// over before Cy is, so the setting would never come into play and reading
	// the file would suggest it might.
	if t.Detach && t.Background == BackgroundNever && t.Yield == 0 {
		return errors.New("detach needs work that can outlast its call; set background to auto or always, or give the tool a yield")
	}
	t.Workdir = strings.TrimSpace(t.Workdir)
	if filepath.IsAbs(t.Workdir) {
		return fmt.Errorf("workdir %q must be relative to the workspace root", t.Workdir)
	}
	if err := validateToolEnv(t.Env); err != nil {
		return err
	}
	return t.Sandbox.resolve(base)
}

func validateToolEnv(env map[string]string) error {
	for key := range env {
		switch {
		case strings.TrimSpace(key) == "" || strings.ContainsRune(key, '='):
			return fmt.Errorf("env name %q is not a variable name", key)
		// HOME and TMPDIR are how the fence tells a tool where it may write.
		// Overriding them from config points the tool at somewhere it cannot
		// reach, which reads as the tool being broken rather than as the
		// setting being refused.
		case key == "HOME" || key == "TMPDIR":
			return fmt.Errorf("env %s is set by the sandbox and cannot be overridden", key)
		case strings.HasPrefix(key, "CY_INTERNAL_"):
			return fmt.Errorf("env %s is reserved", key)
		}
	}
	return nil
}

func (t ExternalTool) timeout() time.Duration {
	if t.Timeout <= 0 {
		return defaultExternalTimeout
	}
	return min(time.Duration(t.Timeout)*time.Second, maxExternalTimeout)
}

// rendered is the declaration as this run resolved it, for the journal. Values
// of declared variables are deliberately left behind; see ExternalToolConfig.
//
// The background mode is the one that will be used and not the one that was
// written, because a run with background turned off runs every configured tool
// in the foreground and the file alone would not say so.
func (t ExternalTool) rendered(program string, mode BackgroundMode, yield time.Duration, detach bool) session.ExternalToolConfig {
	config := session.ExternalToolConfig{
		Name:    t.Name,
		Effect:  t.Effect,
		Program: program,
		Command: t.Command,
		Workdir: t.Workdir,
		Timeout: t.timeout().String(),
		Sandbox: t.Sandbox.Journal(),
	}
	if mode != BackgroundNever {
		config.Background = string(mode)
	}
	if yield > 0 {
		config.Yield = yield.String()
	}
	config.Detach = detach
	for name := range t.Env {
		config.EnvNames = append(config.EnvNames, name)
	}
	slices.Sort(config.EnvNames)
	return config
}

// ExternalToolMeta is what the journal keeps about one call: enough to run the
// same thing again by hand. The call's arguments are not repeated here because
// they are already on the tool_call record this result answers.
type ExternalToolMeta struct {
	Tool    string   `json:"tool"`
	Program string   `json:"program"`
	Command []string `json:"command"`
	Workdir string   `json:"workdir"`
	Sandbox string   `json:"sandbox"`
	// JobID is set when the call left the tool running. It is the link between
	// this record and everything the job tool later says about the same work.
	JobID      string `json:"job_id,omitempty"`
	ExitCode   *int   `json:"exit_code,omitempty"`
	DurationMS int64  `json:"duration_ms"`
	Truncated  bool   `json:"truncated,omitempty"`
	TimedOut   bool   `json:"timed_out,omitempty"`
}

// ExternalTools turns declarations into runnable tools, and returns beside them
// what the journal should say about how those tools will run.
//
// Programs are resolved here rather than at call time, for two reasons. A name
// that is not on PATH is a mistake in configuration, and the run should end
// before the model is ever told the tool exists. And it is what the fence
// needs anyway: it execs a path and never searches PATH itself. Resolving once
// is also what makes the record true for the whole session rather than for the
// moment it was written.
func (m *processManager) ExternalTools(declarations []ExternalTool) ([]golem.Tool, []session.ExternalToolConfig, error) {
	tools := make([]golem.Tool, 0, len(declarations))
	configs := make([]session.ExternalToolConfig, 0, len(declarations))
	for _, declaration := range declarations {
		program, err := m.resolveExternalProgram(declaration.Command[0])
		if err != nil {
			return nil, nil, fmt.Errorf("tool %s: %w", declaration.Name, err)
		}
		mode, yield := m.externalBackground(declaration)
		detach := m.externalDetach(declaration)
		parameters := declaration.Parameters
		if mode == BackgroundAuto {
			parameters = withBackgroundArg(parameters)
		}
		tools = append(tools, golem.FunctionToolWithEffect(
			golem.ToolEffect(declaration.Effect),
			declaration.Name,
			declaration.Description,
			parameters,
			m.externalRunner(declaration, program, mode, yield, detach),
		))
		configs = append(configs, declaration.rendered(program, mode, yield, detach))
	}
	return tools, configs, nil
}

// externalBackground is how a declaration's background settings apply to this
// run. A run with background jobs turned off runs configured tools in the
// foreground too: the setting is about work outliving a tool call, and a
// configured tool is not an exception to it. Waiting can only give the model a
// more complete answer than it asked for, which is the harmless direction.
func (m *processManager) externalBackground(t ExternalTool) (BackgroundMode, time.Duration) {
	if !m.allowBackground {
		return BackgroundNever, 0
	}
	return cmp.Or(t.Background, BackgroundNever), time.Duration(t.Yield) * time.Second
}

// withBackgroundArg adds the flag the model fills in for a tool declared auto.
// A declaration that already names the parameter is refused when it is loaded,
// so this never replaces one the tool meant to have.
func withBackgroundArg(schema jsonschema.Schema) jsonschema.Schema {
	properties := make(map[string]jsonschema.Schema, len(schema.Properties)+1)
	maps.Copy(properties, schema.Properties)
	properties[backgroundArg] = jsonschema.Bool{
		Description: "Return a job id immediately and let the tool keep running. Use it for work whose result you do not need in this reply; leave it out to wait for the answer.",
	}.BuildSchema()
	schema.Properties = properties
	return schema
}

// canBackground reports whether a call on this tool can end with the tool still
// running, either way it happens.
func (m *processManager) canBackground(t ExternalTool) bool {
	mode, yield := m.externalBackground(t)
	return mode != BackgroundNever || yield > 0
}

// externalDetach is whether this tool's work may be left running when Cy exits.
// Nothing can be in a run where nothing outlives its call, so the two settings
// are read together rather than separately.
func (m *processManager) externalDetach(t ExternalTool) bool {
	return t.Detach && m.canBackground(t)
}

// resolveExternalProgram finds the executable a declaration names. A bare name
// comes from PATH, as a shell would find it; anything with a separator is a
// path, and a relative one is relative to the workspace so that a tool checked
// in beside the code it works on can be named the way it is written.
func (m *processManager) resolveExternalProgram(program string) (string, error) {
	if !strings.ContainsRune(program, filepath.Separator) && !strings.ContainsRune(program, '/') {
		resolved, err := exec.LookPath(program)
		if err != nil {
			return "", fmt.Errorf("program %q is not on PATH: %w", program, err)
		}
		return resolved, nil
	}
	resolved := program
	if !filepath.IsAbs(program) {
		absolute, _, info, err := m.workspace.resolveExistingPath(program)
		if err != nil {
			return "", fmt.Errorf("program %q: %w", program, err)
		}
		if info.IsDir() {
			return "", fmt.Errorf("program %q is a directory", program)
		}
		resolved = absolute
	}
	info, err := os.Stat(resolved)
	if err != nil {
		return "", fmt.Errorf("program %q: %w", program, err)
	}
	if info.IsDir() || info.Mode()&0o111 == 0 {
		return "", fmt.Errorf("program %q is not executable", program)
	}
	return resolved, nil
}

func (m *processManager) externalRunner(declaration ExternalTool, program string, mode BackgroundMode, yield time.Duration, detach bool) golem.ToolFunc {
	timeout := declaration.timeout()
	return func(ctx context.Context, call llm.ToolCall) (golem.ToolResult, error) {
		arguments, err := externalCallArguments(call)
		if err != nil {
			return golem.ToolResult{}, err
		}
		background := mode == BackgroundAlways
		if mode == BackgroundAuto {
			if arguments, background, err = takeBackgroundArg(arguments); err != nil {
				return golem.ToolResult{}, err
			}
		}
		workdir, display, info, err := m.workspace.resolveExistingPath(declaration.Workdir)
		if err != nil {
			return golem.ToolResult{}, err
		}
		if !info.IsDir() {
			return golem.ToolResult{}, fmt.Errorf("workdir %s is not a directory", display)
		}
		if err := ctx.Err(); err != nil {
			return golem.ToolResult{}, err
		}

		m.mu.Lock()
		box := m.sandbox.With(declaration.Sandbox, declaration.Env)
		m.mu.Unlock()

		// The same fence as Bash, and the same one for every tool that did not
		// ask for otherwise: a program Cy spawns would inherit Cy's own reach,
		// the journal included, and no outer sandbox can tell it apart from Cy
		// to stop that.
		command, err := box.Command(program, declaration.Command, workdir)
		if err != nil {
			return golem.ToolResult{}, fmt.Errorf("%w: %s: %v", golem.ErrToolFatal, declaration.Name, err)
		}

		// A managed job like any other, so that a configured program gets the
		// supervisor, the mailbox and the bounded output that a shell command
		// has, rather than a second spawn-and-wait beside them.
		// It differs in two ways only: its arguments arrive on stdin, and its
		// two streams are kept apart because the model is shown them apart.
		job, err := m.startJob(jobSpec{
			command:   declaration.Name,
			process:   command,
			timeout:   timeout,
			stdin:     bytes.NewReader(arguments),
			stderr:    &jobBuffer{limit: m.logLimit},
			supervise: true,
			detach:    detach,
		})
		if err != nil {
			return golem.ToolResult{}, fmt.Errorf("%w: %s: start %s: %v", golem.ErrToolFatal, declaration.Name, program, err)
		}
		running := func() golem.ToolResult {
			meta := ExternalToolMeta{
				Tool: declaration.Name, Program: program, Command: declaration.Command,
				Workdir: display, Sandbox: box.Policy, JobID: job.id,
				DurationMS: time.Since(job.startedAt).Milliseconds(),
			}
			return golem.ToolResult{
				Content: fmt.Sprintf("%s is running as job %s. Read its output, wait for it or stop it with the job tool; it will report when it ends.", declaration.Name, job.id),
				Meta:    meta,
			}
		}
		if background {
			return running(), nil
		}

		var yielded <-chan time.Time
		if yield > 0 {
			timer := time.NewTimer(yield)
			defer timer.Stop()
			yielded = timer.C
		}
		select {
		case <-job.done:
		case <-yielded:
			// Raced: the tool may have ended while the timer was firing, and a
			// job id for work that is already done wastes a turn.
			select {
			case <-job.done:
			default:
				return running(), nil
			}
		case <-ctx.Done():
			_, _ = m.stop(context.Background(), job.id, "tool call cancelled")
			m.forget(job)
			return golem.ToolResult{}, ctx.Err()
		}
		// Forgotten only once it has been waited for: the answer is in this
		// result, so there is nothing left for anyone to ask about and the
		// mailbox goes with it. A job left running keeps both.
		defer m.forget(job)

		job.mu.Lock()
		status, exitCode, errText := job.status, job.exitCode, job.errText
		duration := job.finishedAt.Sub(job.startedAt)
		job.mu.Unlock()
		// Not a tool error. ENOENT, a bad interpreter line or a program that is
		// not executable will fail identically however many times the model
		// rephrases its arguments.
		if status == jobNotStarted {
			return golem.ToolResult{}, fmt.Errorf("%w: %s: start %s: %s", golem.ErrToolFatal, declaration.Name, program, errText)
		}
		out, outTruncated := job.log.snapshot(int(m.logLimit))
		errOut, errTruncated := job.errLog.snapshot(int(m.logLimit))
		meta := ExternalToolMeta{
			Tool:       declaration.Name,
			Program:    program,
			Command:    declaration.Command,
			Workdir:    display,
			Sandbox:    box.Policy,
			ExitCode:   exitCode,
			DurationMS: duration.Milliseconds(),
			Truncated:  outTruncated || errTruncated,
			TimedOut:   status == jobTimedOut,
		}
		return golem.ToolResult{
			Content: formatExternalResult(declaration.Name, string(out), string(errOut), meta, timeout),
			Meta:    meta,
		}, nil
	}
}

// externalCallArguments returns the JSON the model produced, which goes to the
// tool on stdin unchanged. It is checked for being one JSON object and not for
// matching the declared schema: the tool owns its own arguments, and a message
// written by the thing that will read them beats one written here.
func externalCallArguments(call llm.ToolCall) ([]byte, error) {
	arguments := strings.TrimSpace(call.Function.Arguments)
	if arguments == "" {
		arguments = "{}"
	}
	var object map[string]any
	decoder := json.NewDecoder(strings.NewReader(arguments))
	if err := decoder.Decode(&object); err != nil {
		return nil, fmt.Errorf("invalid tool arguments: %w", err)
	}
	if decoder.Decode(&struct{}{}) != io.EOF {
		return nil, errors.New("invalid tool arguments: multiple JSON values")
	}
	return []byte(arguments), nil
}

// takeBackgroundArg reads the flag auto put in the schema and returns the
// arguments without it, so the tool is handed only what its own schema
// declares. Re-marshalled rather than edited in place: key order in an object
// means nothing, and half-removing a field would hand the tool broken JSON.
func takeBackgroundArg(arguments []byte) ([]byte, bool, error) {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(arguments, &fields); err != nil {
		return nil, false, fmt.Errorf("invalid tool arguments: %w", err)
	}
	raw, ok := fields[backgroundArg]
	if !ok {
		return arguments, false, nil
	}
	var background bool
	if err := json.Unmarshal(raw, &background); err != nil {
		return nil, false, errors.New("invalid tool arguments: background must be true or false")
	}
	delete(fields, backgroundArg)
	trimmed, err := json.Marshal(fields)
	if err != nil {
		return nil, false, err
	}
	return trimmed, background, nil
}

func appendExternalEnv(base []string, extra map[string]string) []string {
	if len(extra) == 0 {
		return base
	}
	names := make([]string, 0, len(extra))
	for name := range extra {
		names = append(names, name)
	}
	// Sorted so that two runs of the same configuration hand the process the
	// same environment in the same order.
	slices.Sort(names)
	for _, name := range names {
		base = append(base, name+"="+extra[name])
	}
	return base
}

// formatExternalResult writes what the model reads. Truncation is stated here
// and not only in the metadata: a result the model reads as complete when it is
// not is worse than one it knows is partial.
func formatExternalResult(name, stdout, stderr string, meta ExternalToolMeta, timeout time.Duration) string {
	var out strings.Builder
	switch {
	case meta.TimedOut:
		fmt.Fprintf(&out, "%s timed out after %s and was killed.\n", name, timeout)
	case meta.ExitCode == nil:
		fmt.Fprintf(&out, "%s did not exit normally.\n", name)
	case *meta.ExitCode != 0:
		fmt.Fprintf(&out, "%s exited %d.\n", name, *meta.ExitCode)
	}
	if stdout != "" {
		if out.Len() > 0 {
			out.WriteString("\n")
		}
		out.WriteString(stdout)
		if !strings.HasSuffix(stdout, "\n") {
			out.WriteString("\n")
		}
	}
	// stderr only when it says something the model needs. On success a tool
	// that logs progress there would otherwise bury its own answer.
	if stderr != "" && (meta.ExitCode == nil || *meta.ExitCode != 0 || stdout == "") {
		if out.Len() > 0 {
			out.WriteString("\n")
		}
		out.WriteString("stderr:\n")
		out.WriteString(stderr)
		if !strings.HasSuffix(stderr, "\n") {
			out.WriteString("\n")
		}
	}
	if meta.Truncated {
		out.WriteString("\n[output truncated: the tool produced more than this run keeps]\n")
	}
	if out.Len() == 0 {
		return fmt.Sprintf("%s produced no output.", name)
	}
	return strings.TrimRight(out.String(), "\n")
}
