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
