package tools

import (
	"os"
	"testing"
)

func TestMain(m *testing.M) {
	// The tests fork this binary as a job supervisor, exactly as Cy forks
	// itself, so the test binary has to answer to the same argument.
	if RunSandboxChildIfRequested() || RunJobChildIfRequested() {
		return
	}
	os.Exit(m.Run())
}

// testSandbox is the fence the sandbox tests build: a workspace, a tool home,
// and no configured grants. Cy's own home is not among them, so nothing here
// exercises the hiding path; the tests that do name it themselves.
func testSandbox(workspace, toolHome, policy string) Sandbox {
	return Sandbox{Policy: policy, Workspace: workspace, ToolHome: toolHome}
}
