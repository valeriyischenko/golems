//go:build !linux && !darwin

package tools

import (
	"errors"
	"os/exec"
	"runtime"
)

func runSandboxChildIfRequested() bool { return false }

func sandboxBackend() string { return "" }

func sandboxedBashCommand(command, workspace, workdir, home, policy string) (*exec.Cmd, error) {
	if policy == sandboxOn {
		return nil, errors.New("sandbox is unavailable on " + runtime.GOOS)
	}
	return ambientBashCommand(command, workdir), nil
}

// Nothing is fenced here, so no directory is granted in the sense the other
// platforms mean it: there is no boundary for a path to be inside of.
func sandboxGrantedDirs(workspace, home string) []string { return nil }

func hardenSupervisor() {}
