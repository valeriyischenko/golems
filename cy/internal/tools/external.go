package tools

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"

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
	// Background is accepted and refused rather than unknown. Background is a
	// property of tools later; naming the key now means turning it on is a
	// change in behaviour rather than a change in the file format, and a
	// config written for that day fails loudly here instead of quietly
	// running in the foreground.
	Background bool `json:"background,omitempty"`
}

type externalToolFile struct {
	Tools []ExternalTool `json:"tools"`
}

// LoadExternalTools reads the tool declarations at path. No file means no
// external tools, which is the ordinary case; a file that does not parse or
// does not validate is a configuration error, because the alternative is a run
// that silently offers the model fewer tools than its author declared.
func LoadExternalTools(path string) ([]ExternalTool, error) {
	raw, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read tool config %s: %w", path, err)
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	var file externalToolFile
	if err := decoder.Decode(&file); err != nil {
		return nil, fmt.Errorf("parse tool config %s: %w", path, err)
	}
	if decoder.Decode(&struct{}{}) != io.EOF {
		return nil, fmt.Errorf("parse tool config %s: multiple JSON values", path)
	}
	declared := make(map[string]bool, len(file.Tools))
	for index := range file.Tools {
		tool := &file.Tools[index]
		label := strings.TrimSpace(tool.Name)
		if label == "" {
			label = fmt.Sprintf("entry %d", index+1)
		}
		if err := tool.normalize(); err != nil {
			return nil, fmt.Errorf("tool %s in %s: %w", label, path, err)
		}
		if declared[tool.Name] {
			return nil, fmt.Errorf("tool config %s declares %q twice", path, tool.Name)
		}
		declared[tool.Name] = true
	}
	return file.Tools, nil
}

func (t *ExternalTool) normalize() error {
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
	if t.Background {
		return errors.New("background external tools are not supported yet; run it in the foreground or have it start its own daemon")
	}
	t.Workdir = strings.TrimSpace(t.Workdir)
	if filepath.IsAbs(t.Workdir) {
		return fmt.Errorf("workdir %q must be relative to the workspace root", t.Workdir)
	}
	for key := range t.Env {
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

// ExternalToolMeta is what the journal keeps about one call: enough to run the
// same thing again by hand. The call's arguments are not repeated here because
// they are already on the tool_call record this result answers.
type ExternalToolMeta struct {
	Tool       string   `json:"tool"`
	Program    string   `json:"program"`
	Command    []string `json:"command"`
	Workdir    string   `json:"workdir"`
	Sandbox    string   `json:"sandbox"`
	ExitCode   *int     `json:"exit_code,omitempty"`
	DurationMS int64    `json:"duration_ms"`
	Truncated  bool     `json:"truncated,omitempty"`
	TimedOut   bool     `json:"timed_out,omitempty"`
}

// ExternalTools turns declarations into runnable tools.
//
// Programs are resolved here rather than at call time, for two reasons. A name
// that is not on PATH is a mistake in configuration, and the run should end
// before the model is ever told the tool exists. And it is what the fence
// needs anyway: it execs a path and never searches PATH itself.
func (m *processManager) ExternalTools(declarations []ExternalTool) ([]golem.Tool, error) {
	tools := make([]golem.Tool, 0, len(declarations))
	for _, declaration := range declarations {
		program, err := m.resolveExternalProgram(declaration.Command[0])
		if err != nil {
			return nil, fmt.Errorf("tool %s: %w", declaration.Name, err)
		}
		tools = append(tools, golem.FunctionToolWithEffect(
			golem.ToolEffect(declaration.Effect),
			declaration.Name,
			declaration.Description,
			declaration.Parameters,
			m.externalRunner(declaration, program),
		))
	}
	return tools, nil
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

func (m *processManager) externalRunner(declaration ExternalTool, program string) golem.ToolFunc {
	timeout := declaration.timeout()
	return func(ctx context.Context, call llm.ToolCall) (golem.ToolResult, error) {
		arguments, err := externalCallArguments(call)
		if err != nil {
			return golem.ToolResult{}, err
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
		closed, sandbox := m.closed, m.sandbox
		m.mu.Unlock()
		if closed {
			return golem.ToolResult{}, errors.New("process manager is closed")
		}

		// The same fence as Bash, deliberately: a program Cy spawns would
		// otherwise inherit Cy's own reach, $CY_HOME and the journal included.
		//
		// TODO: some tools genuinely cannot live inside a ruleset of
		// {workspace, tool home} -- an interpreter with its packages under the
		// real home is the case to expect first. Per-tool isolation levels are
		// wanted and not supported here; the sandbox mechanism becomes a
		// configured launcher first, and the per-tool opt out hangs off that.
		// Until then every external tool is fenced identically, because a tool
		// that visibly does not run is better than isolation that silently is
		// not there.
		command, err := SandboxedCommand(program, declaration.Command, m.workspace.root, workdir, m.toolHome, sandbox)
		if err != nil {
			return golem.ToolResult{}, fmt.Errorf("%w: %s: %v", golem.ErrToolFatal, declaration.Name, err)
		}
		command.Env = appendExternalEnv(command.Env, declaration.Env)
		configureProcessGroup(command)
		stdout := &jobBuffer{limit: m.logLimit}
		stderr := &jobBuffer{limit: m.logLimit}
		command.Stdin = bytes.NewReader(arguments)
		command.Stdout = stdout
		command.Stderr = stderr

		startedAt := time.Now()
		if err := command.Start(); err != nil {
			// Not a tool error. ENOENT, a bad interpreter line or a program
			// that is not executable will fail identically however many times
			// the model rephrases its arguments.
			return golem.ToolResult{}, fmt.Errorf("%w: %s: start %s: %v", golem.ErrToolFatal, declaration.Name, program, err)
		}
		waitErr, timedOut, cancelled := awaitExternal(ctx, command, timeout)
		duration := time.Since(startedAt)
		if cancelled {
			return golem.ToolResult{}, ctx.Err()
		}

		out, outTruncated := stdout.snapshot(int(m.logLimit))
		errOut, errTruncated := stderr.snapshot(int(m.logLimit))
		meta := ExternalToolMeta{
			Tool:       declaration.Name,
			Program:    program,
			Command:    declaration.Command,
			Workdir:    display,
			Sandbox:    sandbox,
			ExitCode:   processExitCode(waitErr),
			DurationMS: duration.Milliseconds(),
			Truncated:  outTruncated || errTruncated,
			TimedOut:   timedOut,
		}
		return golem.ToolResult{
			Content: formatExternalResult(declaration.Name, string(out), string(errOut), meta, timeout),
			Meta:    meta,
		}, nil
	}
}

// awaitExternal waits for the process, killing its group if the call is
// cancelled or the tool outlives its timeout. It always waits for the process
// to actually be gone, so no tool outlives the call that started it.
func awaitExternal(ctx context.Context, command *exec.Cmd, timeout time.Duration) (waitErr error, timedOut, cancelled bool) {
	wait := make(chan error, 1)
	go func() { wait <- command.Wait() }()
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case waitErr = <-wait:
		return waitErr, false, false
	case <-timer.C:
		timedOut = true
	case <-ctx.Done():
		cancelled = true
	}
	stopped := make(chan struct{})
	go keepKillingProcessGroup(command, stopped)
	waitErr = <-wait
	close(stopped)
	return waitErr, timedOut, cancelled
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
