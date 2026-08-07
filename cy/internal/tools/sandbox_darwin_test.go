//go:build darwin

package tools

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSeatbeltRestrictsFilesystem(t *testing.T) {
	userHome, err := os.UserHomeDir()
	if err != nil {
		t.Fatal(err)
	}
	root, err := os.MkdirTemp(userHome, ".cy-seatbelt-workspace-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(root) })
	workdir := filepath.Join(root, "work")
	toolHome, err := os.MkdirTemp(userHome, ".cy-seatbelt-tool-home-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(toolHome) })
	outsideDir, err := os.MkdirTemp(userHome, ".cy-seatbelt-test-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(outsideDir) })
	outside := filepath.Join(outsideDir, "outside.txt")
	for _, dir := range []string{workdir, toolHome} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(outside, []byte("secret"), 0o600); err != nil {
		t.Fatal(err)
	}
	command := "printf ok > inside.txt; " +
		"if cat " + shellQuoteForTest(outside) + " >/dev/null 2>&1; then exit 41; fi; " +
		"if printf nope > " + shellQuoteForTest(outside) + " 2>/dev/null; then exit 42; fi"
	cmd, err := sandboxedBashCommand(testSandbox(root, toolHome, sandboxOn), command, workdir)
	if err != nil {
		t.Fatal(err)
	}
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("seatbelt command failed: %v: %s", err, output)
	}
	if raw, err := os.ReadFile(filepath.Join(workdir, "inside.txt")); err != nil || strings.TrimSpace(string(raw)) != "ok" {
		t.Fatalf("workspace write = %q, %v", raw, err)
	}
	if raw, err := os.ReadFile(outside); err != nil || string(raw) != "secret" {
		t.Fatalf("outside file changed: %q, %v", raw, err)
	}
}

// The fence used to be able to start exactly one thing, bash with a command
// line. A program launched from configuration is not a shell command, so it has
// to be startable directly — under the same profile, with nothing in between.
func TestSeatbeltRunsAProgramWithoutAShell(t *testing.T) {
	userHome, err := os.UserHomeDir()
	if err != nil {
		t.Fatal(err)
	}
	root, err := os.MkdirTemp(userHome, ".cy-seatbelt-program-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(root) })
	toolHome, err := os.MkdirTemp(userHome, ".cy-seatbelt-program-home-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(toolHome) })
	outsideDir, err := os.MkdirTemp(userHome, ".cy-seatbelt-program-outside-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(outsideDir) })

	inside := filepath.Join(root, "inside.txt")
	if err := os.WriteFile(inside, []byte("workspace data\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(outsideDir, "outside.txt")
	if err := os.WriteFile(outside, []byte("secret"), 0o600); err != nil {
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

	denied, err := sandboxedCommand(testSandbox(root, toolHome, sandboxOn), "/bin/cat", []string{"cat", outside}, root)
	if err != nil {
		t.Fatal(err)
	}
	if output, err := denied.CombinedOutput(); err == nil {
		t.Fatalf("file outside the workspace was readable: %s", output)
	}
}

// A tool declared in configuration is fenced exactly as Bash is. Without that,
// a program Cy spawns inherits Cy's own reach -- every path Cy can touch, Cy's
// home and the journal included -- and the record the harness exists to
// produce is the thing behind the fence, not the provider key.
func TestExternalToolIsFencedLikeBash(t *testing.T) {
	userHome, err := os.UserHomeDir()
	if err != nil {
		t.Fatal(err)
	}
	workspace, err := os.MkdirTemp(userHome, ".cy-external-workspace-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(workspace) })
	home, err := os.MkdirTemp(userHome, ".cy-external-home-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(home) })
	outsideDir, err := os.MkdirTemp(userHome, ".cy-external-outside-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(outsideDir) })
	outside := filepath.Join(outsideDir, "outside.txt")
	if err := os.WriteFile(outside, []byte("supervisor-only\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	content := runFencedExternalProbe(t, workspace, home,
		"cat "+shellQuoteForTest(outside)+" 2>&1")
	if strings.Contains(content, "supervisor-only") {
		t.Fatalf("external tool read outside the workspace: %q", content)
	}
}

func TestSeatbeltProfileKeepsNetworkOpen(t *testing.T) {
	if !strings.Contains(seatbeltProfile(testSandbox(t.TempDir(), t.TempDir(), sandboxOn)), "(allow network*)") {
		t.Fatal("seatbelt profile does not keep network open")
	}
}

// The shared temp directories were granted to both read and write. A tool's
// scratch files went somewhere every other process on the machine could read,
// and anything left there by anyone else was reachable from inside the fence.
func TestSeatbeltProfileDoesNotGrantSharedTemp(t *testing.T) {
	profile := seatbeltProfile(testSandbox(t.TempDir(), t.TempDir(), sandboxOn))
	for _, shared := range []string{"/private/tmp", "/private/var/tmp", "TEMP_DIR"} {
		if strings.Contains(profile, shared) {
			t.Fatalf("seatbelt profile still grants %s", shared)
		}
	}
}

func TestSandboxedToolTempStaysInsideTheToolHome(t *testing.T) {
	userHome, err := os.UserHomeDir()
	if err != nil {
		t.Fatal(err)
	}
	root, err := os.MkdirTemp(userHome, ".cy-seatbelt-temp-workspace-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(root) })
	toolHome, err := os.MkdirTemp(userHome, ".cy-seatbelt-temp-home-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(toolHome) })
	if err := os.MkdirAll(WorkspaceToolTemp(toolHome), 0o700); err != nil {
		t.Fatal(err)
	}
	// A file the supervisor left in the machine temp directory, which the
	// profile used to grant outright.
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
		t.Fatalf("seatbelt command failed: %v: %s", err, output)
	}
	if raw, err := os.ReadFile(filepath.Join(WorkspaceToolTemp(toolHome), "scratch.txt")); err != nil || string(raw) != "scratch" {
		t.Fatalf("TMPDIR write = %q, %v", raw, err)
	}
}

func shellQuoteForTest(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "'\\''") + "'"
}
