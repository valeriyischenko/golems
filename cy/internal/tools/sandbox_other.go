//go:build !linux && !darwin

package tools

import (
	"errors"
	"os/exec"
	"runtime"
)

func runSandboxChildIfRequested() bool { return false }

func sandboxBackend() string { return "" }

// Nothing is fenced here, so nothing is granted in the sense the other
// platforms mean it: there is no boundary for a path to be inside of.
var sandboxReadOnlyDirs []string

func sandboxedCommand(box Sandbox, program string, args []string, workdir string) (*exec.Cmd, error) {
	if box.Policy == sandboxOn {
		return nil, errors.New("sandbox is unavailable on " + runtime.GOOS)
	}
	return ambientCommand(program, args, workdir, box.ToolHome), nil
}

func sandboxedBashCommand(box Sandbox, command, workdir string) (*exec.Cmd, error) {
	if box.Policy == sandboxOn {
		return nil, errors.New("sandbox is unavailable on " + runtime.GOOS)
	}
	return ambientBashCommand(command, workdir), nil
}

func hardenSupervisor() {}
