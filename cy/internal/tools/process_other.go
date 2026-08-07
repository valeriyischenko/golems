//go:build !aix && !darwin && !dragonfly && !freebsd && !linux && !netbsd && !openbsd && !solaris

package tools

import "os/exec"

func configureProcessGroup(*exec.Cmd) {}

func killProcessGroup(command *exec.Cmd) error {
	if command == nil || command.Process == nil {
		return nil
	}
	return command.Process.Kill()
}

// processGroupAlive has no answer here: without process groups there is nothing
// to ask about, and claiming a detached job is alive would strand it. Detached
// work is a Unix affordance and reads as gone everywhere else.
func processGroupAlive(int) bool { return false }

func killProcessGroupByPID(int) error { return nil }
