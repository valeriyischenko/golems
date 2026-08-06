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
	cmd, err := sandboxedBashCommand(command, root, workdir, toolHome, sandboxOn)
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

func TestSeatbeltProfileKeepsNetworkOpen(t *testing.T) {
	if !strings.Contains(seatbeltProfile, "(allow network*)") {
		t.Fatal("seatbelt profile does not keep network open")
	}
}

// The shared temp directories were granted to both read and write. A tool's
// scratch files went somewhere every other process on the machine could read,
// and anything left there by anyone else was reachable from inside the fence.
func TestSeatbeltProfileDoesNotGrantSharedTemp(t *testing.T) {
	for _, shared := range []string{"/private/tmp", "/private/var/tmp", "TEMP_DIR"} {
		if strings.Contains(seatbeltProfile, shared) {
			t.Fatalf("seatbelt profile still grants %s", shared)
		}
	}
}

// The profile is a literal, so the list used to report what is reachable can
// drift away from what is actually granted. Check both directions.
func TestSeatbeltReadOnlyDirsMatchProfile(t *testing.T) {
	granted := make(map[string]bool)
	for _, line := range strings.Split(seatbeltProfile, "\n") {
		line = strings.TrimSpace(line)
		rest, ok := strings.CutPrefix(line, `(subpath "`)
		if !ok {
			continue
		}
		path, _, _ := strings.Cut(rest, `"`)
		granted[path] = true
	}
	for _, dir := range sandboxReadOnlyDirs {
		if !granted[dir] {
			t.Errorf("sandboxReadOnlyDirs names %s, which the profile does not grant", dir)
		}
		delete(granted, dir)
	}
	for dir := range granted {
		if dir == "/dev/fd" {
			continue
		}
		t.Errorf("profile grants %s, which sandboxReadOnlyDirs does not name", dir)
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
	cmd, err := sandboxedBashCommand(command, root, root, toolHome, sandboxOn)
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
