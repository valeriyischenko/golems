package tools

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// Cy's home holds the journal and the provider credentials, and the tool home
// sits inside it. Hiding the one without keeping the other would leave every
// tool without a $HOME it can write to, so the two belong in the same test.
func TestSandboxPlanHidesCyHomeAndKeepsTheToolHome(t *testing.T) {
	stateHome := t.TempDir()
	toolHome := WorkspaceToolHome(stateHome, "/workspace")
	if err := os.MkdirAll(toolHome, 0o700); err != nil {
		t.Fatal(err)
	}
	plan := Sandbox{Workspace: t.TempDir(), ToolHome: toolHome, StateHome: stateHome}.plan()
	if !slices.Contains(plan.hide, canonicalSandboxPath(stateHome)) {
		t.Fatalf("hide = %q, want Cy's home in it", plan.hide)
	}
	if !slices.Contains(plan.write, canonicalSandboxPath(toolHome)) {
		t.Fatalf("write = %q, want the tool home in it", plan.write)
	}
	if got := plan.belowHidden(plan.write); !slices.Contains(got, canonicalSandboxPath(toolHome)) {
		t.Fatalf("belowHidden(write) = %q, want the tool home in it", got)
	}
}

// A path granted and hidden by the same file is hidden: the two settings have
// to resolve one way or the other, and taking access away is the direction that
// fails safely.
func TestSandboxPlanHideBeatsAGrantOfTheSamePath(t *testing.T) {
	dir := t.TempDir()
	plan := Sandbox{
		Workspace: t.TempDir(),
		ToolHome:  t.TempDir(),
		StateHome: t.TempDir(),
		Grants:    SandboxGrants{Read: []string{dir}, Hide: []string{dir}},
	}.plan()
	if slices.Contains(plan.read, canonicalSandboxPath(dir)) {
		t.Fatalf("read = %q, want %s left out", plan.read, dir)
	}
}

func TestSandboxPathsResolveAgainstTheToolsFile(t *testing.T) {
	base := t.TempDir()
	pyenv := filepath.Join(base, "pyenv")
	if err := os.Mkdir(pyenv, 0o700); err != nil {
		t.Fatal(err)
	}
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatal(err)
	}
	grants := SandboxGrants{Read: []string{"pyenv", "~", base}}
	if err := grants.resolve(base); err != nil {
		t.Fatal(err)
	}
	want := []string{pyenv, home, base}
	if !slices.Equal(grants.Read, want) {
		t.Fatalf("read = %q, want %q", grants.Read, want)
	}
}

// A path that is not there is a typo, and a typo in a hide is a run that does
// not hide what it was told to. Both cost a configuration error rather than a
// fence quietly built from something else.
func TestSandboxPathsRefuseWhatIsNotThere(t *testing.T) {
	grants := SandboxGrants{Hide: []string{"absent"}}
	if err := grants.resolve(t.TempDir()); err == nil {
		t.Fatal("resolve() accepted a path that does not exist")
	}
}

func TestSandboxGrantsMergeKeepsBothHides(t *testing.T) {
	run := SandboxGrants{Read: []string{"/a"}, Hide: []string{"/x"}}
	tool := SandboxGrants{Read: []string{"/b"}, Hide: []string{"/y"}}
	merged := run.merge(tool)
	if !slices.Equal(merged.Read, []string{"/a", "/b"}) || !slices.Equal(merged.Hide, []string{"/x", "/y"}) {
		t.Fatalf("merged = %+v", merged)
	}
}

// The two grants a configured tool actually needs, checked through the fence
// rather than through the plan: an interpreter's packages made readable outside
// the workspace, and one directory inside that grant kept out of reach.
//
// Written once for both backends because the words mean the same thing on each,
// which is the point of them being one vocabulary. How they get there differs —
// Seatbelt writes a deny, Landlock rebuilds the grant around the hidden path —
// and that difference is exactly what a shared test is for.
func TestSandboxGrantsAndHidesConfiguredPaths(t *testing.T) {
	if SandboxBackend() == "" {
		t.Skip("no platform sandbox")
	}
	// Under the invoking user's home, because macOS grants /private/var/folders
	// to every process and a workspace in the machine temp would prove nothing.
	userHome, err := os.UserHomeDir()
	if err != nil {
		t.Fatal(err)
	}
	dir := func(prefix string) string {
		made, err := os.MkdirTemp(userHome, ".cy-sandbox-"+prefix+"-")
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = os.RemoveAll(made) })
		return made
	}
	root, toolHome, packages := dir("workspace"), dir("tool-home"), dir("packages")
	private := filepath.Join(packages, "private")
	if err := os.Mkdir(private, 0o700); err != nil {
		t.Fatal(err)
	}
	open, secret := filepath.Join(packages, "open.txt"), filepath.Join(private, "secret.txt")
	for path, content := range map[string]string{open: "granted", secret: "hidden"} {
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	box := Sandbox{
		Policy:    sandboxOn,
		Workspace: root,
		ToolHome:  toolHome,
		Grants:    SandboxGrants{Read: []string{packages}, Hide: []string{private}},
	}
	// The denied read is expected to fail, so the command ends on something
	// that does not: a non-zero exit here would mean the fence never ran.
	cmd, err := box.BashCommand("cat "+shellQuoteForTest(open)+"; cat "+shellQuoteForTest(secret)+" 2>/dev/null; true", root)
	if err != nil {
		t.Fatal(err)
	}
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("sandboxed bash failed: %v: %s", err, output)
	}
	if !strings.Contains(string(output), "granted") {
		t.Errorf("configured read grant did not reach the tool: %q", output)
	}
	if strings.Contains(string(output), "hidden") {
		t.Errorf("hidden path was readable: %q", output)
	}
}
