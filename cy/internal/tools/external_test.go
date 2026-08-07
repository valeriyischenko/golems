package tools

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/levmv/golems/pkg/golem"
	"github.com/levmv/golems/pkg/jsonschema"
	"github.com/levmv/golems/pkg/llm"
)

const echoArgumentsTool = `{
  "tools": [
    {
      "name": "book_search",
      "description": "Search the text. One line per hit.",
      "effect": "read",
      "command": ["python3", "tools/book_search.py"],
      "parameters": {
        "type": "object",
        "properties": {"query": {"type": "string"}},
        "required": ["query"],
        "additionalProperties": false
      },
      "timeout": 120,
      "workdir": ".",
      "env": {"PYTHONHASHSEED": "0"}
    }
  ]
}`

func TestLoadExternalToolsReadsADeclaration(t *testing.T) {
	declarations := loadExternalToolsForTest(t, echoArgumentsTool)
	if len(declarations) != 1 {
		t.Fatalf("declarations = %d", len(declarations))
	}
	declaration := declarations[0]
	if declaration.Name != "book_search" || declaration.Effect != "read" {
		t.Fatalf("declaration = %+v", declaration)
	}
	if len(declaration.Command) != 2 || declaration.Command[0] != "python3" {
		t.Fatalf("command = %q", declaration.Command)
	}
	// The parameters block is verbatim JSON Schema, so it has to survive the
	// trip into the type a tool definition already wants.
	if declaration.Parameters.Properties["query"].Type != "string" {
		t.Fatalf("parameters = %+v", declaration.Parameters)
	}
	if declaration.timeout() != 120*time.Second {
		t.Fatalf("timeout = %s", declaration.timeout())
	}
}

// No tool file is the ordinary case and not a failure. A file that is there
// and wrong is the opposite: it means someone declared tools this run will not
// have, and running anyway hides that.
func TestLoadExternalToolsAcceptsAMissingFile(t *testing.T) {
	declarations, err := LoadExternalTools(filepath.Join(t.TempDir(), "absent.json"))
	if err != nil || declarations != nil {
		t.Fatalf("declarations = %v, err = %v", declarations, err)
	}
}

func TestLoadExternalToolsRejectsBadDeclarations(t *testing.T) {
	cases := []struct {
		name    string
		config  string
		message string
	}{
		{"not json", `{`, "parse tool config"},
		{"unknown field", `{"tools":[{"name":"t","description":"d","effect":"read","command":["x"],"colour":"red"}]}`, "colour"},
		{"missing effect", `{"tools":[{"name":"t","description":"d","command":["x"]}]}`, "effect"},
		{"unknown effect", `{"tools":[{"name":"t","description":"d","effect":"readonly","command":["x"]}]}`, "effect"},
		{"no description", `{"tools":[{"name":"t","description":"  ","effect":"read","command":["x"]}]}`, "description"},
		{"no command", `{"tools":[{"name":"t","description":"d","effect":"read","command":[]}]}`, "command"},
		{"bad name", `{"tools":[{"name":"book search","description":"d","effect":"read","command":["x"]}]}`, "name"},
		{"duplicate name", `{"tools":[{"name":"t","description":"d","effect":"read","command":["x"]},{"name":"t","description":"d","effect":"read","command":["y"]}]}`, "twice"},
		{"scalar parameters", `{"tools":[{"name":"t","description":"d","effect":"read","command":["x"],"parameters":{"type":"string"}}]}`, "object"},
		{"negative timeout", `{"tools":[{"name":"t","description":"d","effect":"read","command":["x"],"timeout":-1}]}`, "timeout"},
		{"absolute workdir", `{"tools":[{"name":"t","description":"d","effect":"read","command":["x"],"workdir":"/etc"}]}`, "workdir"},
		{"overrides HOME", `{"tools":[{"name":"t","description":"d","effect":"read","command":["x"],"env":{"HOME":"/root"}}]}`, "HOME"},
		{"reserved env", `{"tools":[{"name":"t","description":"d","effect":"read","command":["x"],"env":{"CY_INTERNAL_SANDBOX_POLICY":"off"}}]}`, "reserved"},
		{"unknown background", `{"tools":[{"name":"t","description":"d","effect":"read","command":["x"],"background":"sometimes"}]}`, "background"},
		{"background is not a number", `{"tools":[{"name":"t","description":"d","effect":"read","command":["x"],"background":3}]}`, "background"},
		// auto writes a background flag into the tool's schema, and a tool that
		// declares one of its own would be shown a schema its author did not
		// write and handed arguments with a field quietly removed.
		{"auto collides with a parameter", `{"tools":[{"name":"t","description":"d","effect":"read","command":["x"],"background":"auto","parameters":{"type":"object","properties":{"background":{"type":"string"}}}}]}`, "background"},
		{"negative yield", `{"tools":[{"name":"t","description":"d","effect":"read","command":["x"],"yield":-1}]}`, "yield"},
		{"yield without a foreground", `{"tools":[{"name":"t","description":"d","effect":"read","command":["x"],"background":"always","yield":5}]}`, "yield"},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			path := writeToolConfigForTest(t, testCase.config)
			_, err := LoadExternalTools(path)
			if err == nil {
				t.Fatal("bad configuration was accepted")
			}
			if !strings.Contains(err.Error(), testCase.message) {
				t.Fatalf("error = %v, want it to mention %q", err, testCase.message)
			}
		})
	}
}

// A program named but not present is a mistake in configuration, and it has to
// be found before the model is told the tool exists rather than fifty turns
// into an unattended run.
func TestExternalToolsRefuseAProgramThatIsNotThere(t *testing.T) {
	manager := processManagerForTest(t)
	_, _, err := manager.ExternalTools([]ExternalTool{{
		Name: "missing", Description: "d", Effect: "read",
		Command: []string{"cy-no-such-program-anywhere"},
	}})
	if err == nil || !strings.Contains(err.Error(), "not on PATH") {
		t.Fatalf("err = %v", err)
	}
}

func TestExternalToolPassesArgumentsOnStdinAndReturnsStdout(t *testing.T) {
	manager := processManagerForTest(t)
	tool := externalToolForTest(t, manager, `cat; printf 'ignored\n' >&2`, ExternalTool{Effect: "read"})
	result := runExternalToolForTest(t, tool, `{"query":"schelling","limit":3}`)
	if result.Content != `{"query":"schelling","limit":3}` {
		t.Fatalf("content = %q", result.Content)
	}
	meta, ok := result.Meta.(ExternalToolMeta)
	if !ok {
		t.Fatalf("meta = %T", result.Meta)
	}
	if meta.ExitCode == nil || *meta.ExitCode != 0 || meta.Tool != "probe" {
		t.Fatalf("meta = %+v", meta)
	}
}

// A tool that ran and failed is a result the model can act on, not a tool
// error, and it needs stderr to act on it.
func TestExternalToolReportsANonZeroExitToTheModel(t *testing.T) {
	manager := processManagerForTest(t)
	tool := externalToolForTest(t, manager, `printf 'partial\n'; printf 'no such book\n' >&2; exit 4`, ExternalTool{Effect: "read"})
	result := runExternalToolForTest(t, tool, `{}`)
	for _, want := range []string{"exited 4", "partial", "no such book"} {
		if !strings.Contains(result.Content, want) {
			t.Fatalf("content = %q, want it to mention %q", result.Content, want)
		}
	}
}

// The environment is the fence's, plus what configuration adds, and never the
// supervisor's -- a tool has no business reading Cy's provider credentials.
func TestExternalToolGetsTheFencedEnvironment(t *testing.T) {
	manager := processManagerForTest(t)
	t.Setenv("CY_TEST_SECRET", "must-not-leak")
	tool := externalToolForTest(t, manager, `env`, ExternalTool{Effect: "read", Env: map[string]string{"PYTHONHASHSEED": "0"}})
	result := runExternalToolForTest(t, tool, `{}`)
	if strings.Contains(result.Content, "must-not-leak") {
		t.Fatalf("supervisor environment leaked: %q", result.Content)
	}
	if !strings.Contains(result.Content, "PYTHONHASHSEED=0") {
		t.Fatalf("declared environment missing: %q", result.Content)
	}
	if !strings.Contains(result.Content, "HOME="+manager.toolHome) {
		t.Fatalf("HOME is not the tool home: %q", result.Content)
	}
}

func TestExternalToolIsKilledWhenItOutlivesItsTimeout(t *testing.T) {
	manager := processManagerForTest(t)
	tool := externalToolForTest(t, manager, `sleep 30`, ExternalTool{Effect: "read", Timeout: 1})
	started := time.Now()
	result := runExternalToolForTest(t, tool, `{}`)
	if elapsed := time.Since(started); elapsed > 20*time.Second {
		t.Fatalf("timeout did not fire: %s", elapsed)
	}
	if !strings.Contains(result.Content, "timed out") {
		t.Fatalf("content = %q", result.Content)
	}
	meta, ok := result.Meta.(ExternalToolMeta)
	if !ok || !meta.TimedOut {
		t.Fatalf("meta = %+v", result.Meta)
	}
}

// Preflight catches the ordinary case, so this is the one it cannot: a program
// that was there when the catalog was built and is gone by the time it is
// called. The model cannot call its way around that, so the run ends.
func TestExternalToolThatCannotStartEndsTheRun(t *testing.T) {
	manager := processManagerForTest(t)
	script := filepath.Join(t.TempDir(), "vanishing.sh")
	if err := os.WriteFile(script, []byte("#!/bin/sh\nexit 0\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	tools, _, err := manager.ExternalTools([]ExternalTool{{
		Name: "vanishing", Description: "d", Effect: "read", Command: []string{script},
	}})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(script); err != nil {
		t.Fatal(err)
	}
	_, err = tools[0].Run(context.Background(), llm.ToolCall{Function: llm.ToolFunction{Arguments: "{}"}})
	if !errors.Is(err, golem.ErrToolFatal) {
		t.Fatalf("err = %v, want it to be fatal", err)
	}
}

func TestExternalToolRejectsArgumentsThatAreNotOneObject(t *testing.T) {
	manager := processManagerForTest(t)
	tool := externalToolForTest(t, manager, `cat`, ExternalTool{Effect: "read"})
	for _, arguments := range []string{`{"a":1} {"b":2}`, `[1,2]`, `not json`} {
		if _, err := tool.Run(context.Background(), llm.ToolCall{Function: llm.ToolFunction{Arguments: arguments}}); err == nil {
			t.Fatalf("arguments %q were accepted", arguments)
		}
	}
}

func TestExternalToolCarriesItsDeclaredEffect(t *testing.T) {
	manager := processManagerForTest(t)
	tool := externalToolForTest(t, manager, `true`, ExternalTool{Effect: "write"})
	if tool.Effect != golem.ToolEffectWrite {
		t.Fatalf("effect = %q", tool.Effect)
	}
	if kept := FilterForProfile([]golem.Tool{tool}, "read-only"); len(kept) != 0 {
		t.Fatal("a tool declared write survived the read-only profile")
	}
}

// runFencedExternalProbe runs a shell script as an external tool with the
// fence on, and returns what the model would read. The workspace and home are
// arguments because the platforms disagree about which directories a test may
// use as a workspace at all.
func runFencedExternalProbe(t *testing.T, workspace, home, script string) string {
	t.Helper()
	bash, err := exec.LookPath("bash")
	if err != nil {
		t.Skip("bash is unavailable")
	}
	manager, err := NewProcessManager(ProcessOptions{Root: workspace, Home: home, Sandbox: sandboxOn, Background: true})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = manager.Close() })
	declaration := ExternalTool{
		Name: "probe", Description: "test probe", Effect: "read",
		Command: []string{bash, "-c", script},
	}
	if err := declaration.normalize(); err != nil {
		t.Fatal(err)
	}
	tools, _, err := manager.ExternalTools([]ExternalTool{declaration})
	if err != nil {
		t.Fatal(err)
	}
	return runExternalToolForTest(t, tools[0], "{}").Content
}

func loadExternalToolsForTest(t *testing.T, config string) []ExternalTool {
	t.Helper()
	declarations, err := LoadExternalTools(writeToolConfigForTest(t, config))
	if err != nil {
		t.Fatal(err)
	}
	return declarations
}

func writeToolConfigForTest(t *testing.T, config string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "tools.json")
	if err := os.WriteFile(path, []byte(config), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// externalToolForTest declares a tool that runs a shell script, which is the
// shortest way to write a program that does something specific with the JSON
// it is handed. Real tools are named directly; nothing here depends on the
// difference.
func externalToolForTest(t *testing.T, manager *processManager, script string, declaration ExternalTool) golem.Tool {
	t.Helper()
	bash, err := exec.LookPath("bash")
	if err != nil {
		t.Skip("bash is unavailable")
	}
	declaration.Name = "probe"
	declaration.Description = "test probe"
	declaration.Command = []string{bash, "-c", script}
	if err := declaration.normalize(); err != nil {
		t.Fatal(err)
	}
	tools, _, buildErr := manager.ExternalTools([]ExternalTool{declaration})
	if buildErr != nil {
		t.Fatal(buildErr)
	}
	return tools[0]
}

func runExternalToolForTest(t *testing.T, tool golem.Tool, arguments string) golem.ToolResult {
	t.Helper()
	result, err := tool.Run(context.Background(), llm.ToolCall{Function: llm.ToolFunction{Arguments: arguments}})
	if err != nil {
		t.Fatalf("tool error = %v", err)
	}
	return result
}

// The journal has to describe a configured tool as it will actually run, since
// the source cannot: the program after the PATH lookup, the timeout after the
// default. Values of declared variables are the deliberate omission -- a
// variable set for a tool is how a tool is handed a token.
func TestExternalToolsRenderTheConfigurationForTheJournal(t *testing.T) {
	bash, err := exec.LookPath("bash")
	if err != nil {
		t.Skip("bash is unavailable")
	}
	manager := processManagerForTest(t)
	declarations := loadExternalToolsForTest(t, `{"tools":[
	  {"name":"notes","description":"search notes","effect":"read",
	   "command":["bash","-c","cat"],"workdir":"","timeout":45,
	   "env":{"NOTES_TOKEN":"s3cret","NOTES_INDEX":"/idx"}},
	  {"name":"plain","description":"no frills","effect":"write",
	   "command":["bash","-c","true"]}
	]}`)
	_, configs, err := manager.ExternalTools(declarations)
	if err != nil {
		t.Fatal(err)
	}
	if len(configs) != 2 {
		t.Fatalf("configs = %d, want 2", len(configs))
	}
	notes := configs[0]
	if notes.Name != "notes" || notes.Effect != "read" {
		t.Fatalf("notes = %+v", notes)
	}
	if notes.Program != bash {
		t.Fatalf("program = %q, want the resolved %q", notes.Program, bash)
	}
	if !slices.Equal(notes.Command, []string{"bash", "-c", "cat"}) {
		t.Fatalf("command = %v", notes.Command)
	}
	if notes.Timeout != "45s" {
		t.Fatalf("timeout = %q, want the declared 45s", notes.Timeout)
	}
	// Sorted, so two runs of the same file render identically.
	if !slices.Equal(notes.EnvNames, []string{"NOTES_INDEX", "NOTES_TOKEN"}) {
		t.Fatalf("env names = %v", notes.EnvNames)
	}
	if encoded, err := json.Marshal(configs); err != nil {
		t.Fatal(err)
	} else if strings.Contains(string(encoded), "s3cret") {
		t.Fatalf("a declared variable's value reached the journal: %s", encoded)
	}
	// A tool that declared none of the optional fields renders the resolved
	// default rather than nothing, which is the whole point of recording it.
	if configs[1].Timeout != defaultExternalTimeout.String() {
		t.Fatalf("default timeout = %q", configs[1].Timeout)
	}
}

// A configured program is a managed job, not a second way of spawning
// things. If it is, the supervisor's account of it is what Cy reports
// -- and a launcher that lies is the only way to tell that apart from the wait
// status Cy would have had anyway.
func TestAConfiguredToolRunsAsAJobUnderTheSameSupervisor(t *testing.T) {
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("sh is unavailable")
	}
	launcher := filepath.Join(t.TempDir(), "launcher.sh")
	script := "#!/bin/sh\nmailbox=$1; shift\n\"$@\"\nprintf '{\"status\":\"completed\",\"exit_code\":7}\\n' > \"$mailbox/result.json\"\n"
	if err := os.WriteFile(launcher, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	home := t.TempDir()
	manager, err := NewProcessManager(ProcessOptions{
		Root: t.TempDir(), Home: home, SessionID: "session",
		Sandbox: sandboxOff, JobLauncher: launcher,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = manager.Close() })

	tool := externalToolForTest(t, manager, `cat; printf 'to stderr' >&2`, ExternalTool{Effect: "read"})
	result := runExternalToolForTest(t, tool, `{"question":"answered"}`)
	meta, ok := result.Meta.(ExternalToolMeta)
	if !ok || meta.ExitCode == nil || *meta.ExitCode != 7 {
		t.Fatalf("meta = %#v, want the launcher's exit 7", result.Meta)
	}
	// Arguments still reach the program on stdin, two processes further down
	// than they used to, and the two streams are still shown apart.
	if !strings.Contains(result.Content, `{"question":"answered"}`) || !strings.Contains(result.Content, "stderr:\nto stderr") {
		t.Fatalf("content = %q", result.Content)
	}
	// A synchronous call leaves nothing behind: nobody can ask about it again.
	if entries, err := os.ReadDir(filepath.Join(home, "jobs", "session")); err == nil && len(entries) != 0 {
		t.Fatalf("registry still holds %d jobs", len(entries))
	}
	if len(manager.jobs) != 0 {
		t.Fatalf("manager still holds %d jobs", len(manager.jobs))
	}
}

// A tool declared always is one whose answer is not the point -- it starts a
// server, a watcher, a build -- so the call is over as soon as the work is
// under way. What makes that useful rather than a leak is the job id: the same
// handle Bash hands back, for the same tools to ask about.
func TestAToolDeclaredAlwaysRunsBehindAJobID(t *testing.T) {
	manager := processManagerForTest(t)
	gate := filepath.Join(t.TempDir(), "gate")
	tool := externalToolForTest(t, manager, waitForGate(gate)+`printf 'late answer\n'`,
		ExternalTool{Effect: "read", Background: BackgroundAlways})

	// Returns while the tool is still blocked, which is the whole claim.
	result := runExternalToolForTest(t, tool, `{}`)
	meta, ok := result.Meta.(ExternalToolMeta)
	if !ok || meta.JobID == "" {
		t.Fatalf("meta = %#v, want a job id", result.Meta)
	}
	if meta.ExitCode != nil || !strings.Contains(result.Content, meta.JobID) {
		t.Fatalf("content = %q, meta = %+v", result.Content, meta)
	}
	job := manager.get(meta.JobID)
	if job == nil {
		t.Fatal("the job tool cannot reach the job the model was given")
	}
	openGate(t, gate)
	<-job.done

	if out, _ := job.snapshot(0); !strings.Contains(string(out), "late answer") {
		t.Fatalf("job output = %q", out)
	}
	// And it reports itself the way a background Bash job does, which is the
	// only way the model hears about work it walked away from.
	pending, err := manager.PendingCompletionEvents("")
	if err != nil || len(pending) != 1 || pending[0].JobID != meta.JobID {
		t.Fatalf("pending = %+v, err = %v", pending, err)
	}
}

// auto is the model's call, so the flag has to be in the schema it is shown --
// and out of the arguments the tool is handed, which never declared it.
func TestBackgroundAutoAsksTheModelAndKeepsTheFlagToItself(t *testing.T) {
	manager := processManagerForTest(t)
	tool := externalToolForTest(t, manager, `cat`, ExternalTool{Effect: "read", Background: BackgroundAuto})
	if _, ok := tool.Definition.Function.Parameters.Properties[backgroundArg]; !ok {
		t.Fatalf("schema = %+v, want a background flag", tool.Definition.Function.Parameters)
	}

	waited := runExternalToolForTest(t, tool, `{"query":"schelling","background":false}`)
	if waited.Content != `{"query":"schelling"}` {
		t.Fatalf("content = %q, want the flag stripped and the answer waited for", waited.Content)
	}
	backgrounded := runExternalToolForTest(t, tool, `{"query":"schelling","background":true}`)
	meta, ok := backgrounded.Meta.(ExternalToolMeta)
	if !ok || meta.JobID == "" {
		t.Fatalf("meta = %#v, want a job id", backgrounded.Meta)
	}
}

// yield is not the model's call. A tool nobody should background on purpose
// still should not hold a turn open until its timeout, so the wait is bounded
// and what is left is a job like any other.
func TestALongForegroundToolYieldsIntoAJob(t *testing.T) {
	manager := processManagerForTest(t)
	gate := filepath.Join(t.TempDir(), "gate")
	tool := externalToolForTest(t, manager, waitForGate(gate), ExternalTool{Effect: "read", Yield: 1})
	started := time.Now()
	result := runExternalToolForTest(t, tool, `{}`)
	if elapsed := time.Since(started); elapsed > 30*time.Second {
		t.Fatalf("the call did not yield: %s", elapsed)
	}
	meta, ok := result.Meta.(ExternalToolMeta)
	if !ok || meta.JobID == "" {
		t.Fatalf("meta = %#v, want a job id", result.Meta)
	}
	openGate(t, gate)
	<-manager.get(meta.JobID).done
}

// The run-level setting is about work outliving a tool call, and a configured
// tool is not an exception to it. Waiting anyway is the harmless direction:
// the model gets a complete answer where it would have got a handle.
func TestBackgroundOffForTheRunKeepsConfiguredToolsInTheForeground(t *testing.T) {
	manager, err := NewProcessManager(ProcessOptions{Root: t.TempDir(), Home: t.TempDir(), Sandbox: sandboxOff})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = manager.Close() })
	tool := externalToolForTest(t, manager, `printf 'answered\n'`, ExternalTool{Effect: "read", Background: BackgroundAlways})
	result := runExternalToolForTest(t, tool, `{}`)
	if !strings.Contains(result.Content, "answered") {
		t.Fatalf("content = %q", result.Content)
	}
	if meta, ok := result.Meta.(ExternalToolMeta); !ok || meta.JobID != "" {
		t.Fatalf("meta = %#v, want no job id", result.Meta)
	}
}

// A profile that hides Bash hides the job tool with it. That is right until a
// configured tool the profile does keep hands back a job id, which the model
// would then have no way to ask about.
func TestTheJobToolComesBackForAProfileThatKeepsABackgroundTool(t *testing.T) {
	manager := processManagerForTest(t)
	declarations := []ExternalTool{
		{Name: "watcher", Description: "d", Effect: "read", Command: []string{"true"}, Background: BackgroundAlways},
		{Name: "lookup", Description: "d", Effect: "read", Command: []string{"true"}},
	}
	readOnly := FilterForProfile(manager.Tools(), "read-only")
	if len(readOnly) != 0 {
		t.Fatalf("read-only kept %d process tools", len(readOnly))
	}
	quiet := golem.FunctionToolWithEffect(golem.ToolEffectRead, "lookup", "d", jsonschema.Obj(), nil)
	if got := manager.EnsureJobTool([]golem.Tool{quiet}, declarations); len(got) != 1 {
		t.Fatalf("tools = %d, want the job tool left out for a tool nobody can background", len(got))
	}
	loud := golem.FunctionToolWithEffect(golem.ToolEffectRead, "watcher", "d", jsonschema.Obj(), nil)
	got := manager.EnsureJobTool([]golem.Tool{loud}, declarations)
	if len(got) != 2 || got[1].Definition.Function.Name != jobToolName {
		t.Fatalf("tools = %+v, want the job tool appended", got)
	}
	// Once only, and never beside the copy a full profile already has.
	if again := manager.EnsureJobTool(got, declarations); len(again) != 2 {
		t.Fatalf("tools = %d, want the job tool added once", len(again))
	}
}

// Detached work is the one thing Cy leaves behind when it exits, and the
// registry is the whole of what connects it to the session that started it.
func TestDetachedWorkOutlivesTheRunAndTheNextRunAdoptsIt(t *testing.T) {
	home, root := t.TempDir(), t.TempDir()
	gate := filepath.Join(t.TempDir(), "gate")
	first := managerForSession(t, home, root, nil)
	tool := externalToolForTest(t, first, waitForGate(gate)+`printf 'finished alone\n'`,
		ExternalTool{Effect: "read", Background: BackgroundAlways, Detach: true})
	result := runExternalToolForTest(t, tool, `{}`)
	meta, ok := result.Meta.(ExternalToolMeta)
	if !ok || meta.JobID == "" {
		t.Fatalf("meta = %#v, want a job id", result.Meta)
	}
	// Named as the run closes, which is what the journal records.
	if ids := first.DetachedJobs(); !slices.Equal(ids, []string{meta.JobID}) {
		t.Fatalf("detached = %v, want %q", ids, meta.JobID)
	}
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(home, "jobs", "session", meta.JobID)); err != nil {
		t.Fatalf("the mailbox went with the run: %v", err)
	}

	second := managerForSession(t, home, root, nil)
	job := second.get(meta.JobID)
	if job == nil {
		t.Fatal("the next run did not adopt the job")
	}
	if ids := second.DetachedJobs(); !slices.Equal(ids, []string{meta.JobID}) {
		t.Fatalf("adopted job = %v, want it still running and still detached", ids)
	}
	// It ends under a Cy that never started it, and reports through the file
	// the supervisor was writing all along.
	openGate(t, gate)
	<-job.done
	if out, _ := job.snapshot(0); !strings.Contains(string(out), "finished alone") {
		t.Fatalf("job output = %q", out)
	}
	pending, err := second.PendingCompletionEvents("")
	if err != nil || len(pending) != 1 || pending[0].JobID != meta.JobID {
		t.Fatalf("pending = %+v, err = %v", pending, err)
	}
}

// The other end of the same story: work left running that is no longer there.
// Dropping it silently would leave the model waiting on a job it was told had
// started, so it is adopted with an ending of its own.
func TestDetachedWorkThatIsGoneIsAdoptedAsAbandoned(t *testing.T) {
	home := t.TempDir()
	mailbox := filepath.Join(home, "jobs", "session", "job-dead")
	if err := os.MkdirAll(mailbox, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(mailbox, jobPidFile), []byte(deadPID(t)), 0o600); err != nil {
		t.Fatal(err)
	}
	manager := managerForSession(t, home, t.TempDir(), nil)
	job := manager.get("job-dead")
	if job == nil {
		t.Fatal("a detached job with no result was dropped")
	}
	<-job.done
	if job.status != jobAbandoned {
		t.Fatalf("status = %q, want abandoned", job.status)
	}
	// Reported like any other ending, since that is the only way the model
	// learns it will never get an answer.
	pending, err := manager.PendingCompletionEvents("")
	if err != nil || len(pending) != 1 || !strings.Contains(pending[0].Content, jobAbandoned) {
		t.Fatalf("pending = %+v, err = %v", pending, err)
	}
}

// deadPID is a process group leader that has already been reaped, which is
// what a mailbox points at when the work it described is over.
func deadPID(t *testing.T) string {
	t.Helper()
	command := exec.Command("sh", "-c", "exit 0")
	configureProcessGroup(command)
	if err := command.Run(); err != nil {
		t.Fatal(err)
	}
	return strconv.Itoa(command.Process.Pid)
}

// waitForGate blocks a test tool until the test says otherwise, which is how a
// background call is shown to have returned early without racing a sleep.
func waitForGate(gate string) string {
	return "cat >/dev/null; while [ ! -f " + gate + " ]; do sleep 0.02; done; "
}

func openGate(t *testing.T, gate string) {
	t.Helper()
	if err := os.WriteFile(gate, nil, 0o600); err != nil {
		t.Fatal(err)
	}
}
