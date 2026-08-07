//go:build linux

package tools

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The shared temp directories were granted read-write. A tool's scratch files
// went somewhere every other process on the box could read, and anything left
// there by anyone else was reachable from inside the fence. Tools get a temp
// directory inside their own home instead.
func TestSandboxedToolTempStaysInsideTheToolHome(t *testing.T) {
	root := t.TempDir()
	toolHome := t.TempDir()
	if err := os.MkdirAll(WorkspaceToolTemp(toolHome), 0o700); err != nil {
		t.Fatal(err)
	}
	// A file the supervisor left in the machine temp directory, which the
	// landlock rules used to grant outright. It has to sit directly in the
	// shared temp root, not in a subdirectory the test was handed.
	outside, err := os.CreateTemp("", "cy-shared-temp-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Remove(outside.Name()) })
	if _, err := outside.WriteString("supervisor-only\n"); err != nil {
		t.Fatal(err)
	}
	if err := outside.Close(); err != nil {
		t.Fatal(err)
	}
	command := `printf scratch > "$TMPDIR/scratch.txt"; ` +
		"if cat " + shellQuoteForTest(outside.Name()) + " >/dev/null 2>&1; then exit 41; fi"
	cmd, err := sandboxedBashCommand(testSandbox(root, toolHome, sandboxOn), command, root)
	if err != nil {
		t.Fatal(err)
	}
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("sandboxed command failed: %v: %s", err, output)
	}
	if raw, err := os.ReadFile(filepath.Join(WorkspaceToolTemp(toolHome), "scratch.txt")); err != nil || string(raw) != "scratch" {
		t.Fatalf("TMPDIR write = %q, %v", raw, err)
	}
}

func shellQuoteForTest(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "'\\''") + "'"
}

// The fence used to be able to start exactly one thing, bash with a command
// line. A program launched from configuration is not a shell command, so it has
// to be startable directly — under the same ruleset, with nothing in between.
func TestSandboxedCommandRunsAProgramWithoutAShell(t *testing.T) {
	root := t.TempDir()
	toolHome := t.TempDir()
	inside := filepath.Join(root, "inside.txt")
	if err := os.WriteFile(inside, []byte("workspace data\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	outside, err := os.CreateTemp("", "cy-outside-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Remove(outside.Name()) })
	if _, err := outside.WriteString("supervisor-only\n"); err != nil {
		t.Fatal(err)
	}
	if err := outside.Close(); err != nil {
		t.Fatal(err)
	}

	cmd, err := sandboxedCommand(testSandbox(root, toolHome, sandboxOn), "/bin/cat", []string{"cat", inside}, root)
	if err != nil {
		t.Fatal(err)
	}
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("sandboxed program failed: %v: %s", err, output)
	}
	if string(output) != "workspace data\n" {
		t.Fatalf("output = %q", output)
	}

	denied, err := sandboxedCommand(testSandbox(root, toolHome, sandboxOn), "/bin/cat", []string{"cat", outside.Name()}, root)
	if err != nil {
		t.Fatal(err)
	}
	if output, err := denied.CombinedOutput(); err == nil {
		t.Fatalf("file outside the workspace was readable: %s", output)
	}
}

// A program's own name is part of what it is launched with, and the shell is
// the caller that cares: bash is executed from an absolute path but has always
// seen itself as "bash".
func TestSandboxedCommandHonoursTheProcessName(t *testing.T) {
	bash := systemBashPath()
	if bash == "" {
		t.Skip("system bash is unavailable")
	}
	root := t.TempDir()
	toolHome := t.TempDir()
	cmd, err := sandboxedCommand(testSandbox(root, toolHome, sandboxOn), bash, []string{"cy-tool", "-c", `printf %s "$0"`}, root)
	if err != nil {
		t.Fatal(err)
	}
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("sandboxed program failed: %v: %s", err, output)
	}
	if string(output) != "cy-tool" {
		t.Fatalf("process name = %q", output)
	}
}

// A tool declared in configuration is fenced exactly as Bash is. Without that,
// a program Cy spawns inherits Cy's own reach -- every path Cy can touch, Cy's
// home and the journal included -- and the record the harness exists to
// produce is the thing behind the fence, not the provider key.
func TestExternalToolIsFencedLikeBash(t *testing.T) {
	outside, err := os.CreateTemp("", "cy-outside-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Remove(outside.Name()) })
	if _, err := outside.WriteString("supervisor-only\n"); err != nil {
		t.Fatal(err)
	}
	if err := outside.Close(); err != nil {
		t.Fatal(err)
	}
	content := runFencedExternalProbe(t, t.TempDir(), t.TempDir(),
		"cat "+shellQuoteForTest(outside.Name())+" 2>&1")
	if strings.Contains(content, "supervisor-only") {
		t.Fatalf("external tool read outside the workspace: %q", content)
	}
}

func TestSandboxedBashNestedWorkdirCanAccessWorkspace(t *testing.T) {
	root := t.TempDir()
	workdir := filepath.Join(root, "nested")
	if err := os.Mkdir(workdir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "source.txt"), []byte("workspace data\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	toolHome := t.TempDir()
	cmd, err := sandboxedBashCommand(testSandbox(root, toolHome, sandboxAuto), "cat ../source.txt > ../copy.txt", workdir)
	if err != nil {
		t.Fatal(err)
	}
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("sandboxed command failed: %v: %s", err, output)
	}
	data, err := os.ReadFile(filepath.Join(root, "copy.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "workspace data\n" {
		t.Fatalf("copy = %q", data)
	}
}
