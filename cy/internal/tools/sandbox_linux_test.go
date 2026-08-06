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
	cmd, err := sandboxedBashCommand(command, root, root, toolHome, sandboxOn)
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
	cmd, err := sandboxedBashCommand("cat ../source.txt > ../copy.txt", root, workdir, toolHome, sandboxAuto)
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
