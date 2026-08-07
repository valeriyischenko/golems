//go:build aix || darwin || dragonfly || freebsd || linux || netbsd || openbsd || solaris

package tools

import (
	"os/exec"
	"syscall"
)

func configureProcessGroup(command *exec.Cmd) {
	command.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}

func killProcessGroup(command *exec.Cmd) error {
	if command == nil || command.Process == nil {
		return nil
	}
	return syscall.Kill(-command.Process.Pid, syscall.SIGKILL)
}

// processGroupAlive reports whether the group Cy left behind for a detached job
// is still there. Signal zero is the portable "does this exist" question, and
// the group check is what keeps a recycled pid from answering it: a pid handed
// to some unrelated process is almost never a group leader, and the pid Cy
// wrote down always was.
//
// A guess either way, and both directions are survivable. Wrong about a dead
// job and it lingers as running until something reaps the mailbox; wrong about
// a live one and Cy reports it abandoned, which is what the status says.
func processGroupAlive(pid int) bool {
	if pid <= 1 {
		return false
	}
	if syscall.Kill(pid, 0) != nil {
		return false
	}
	group, err := syscall.Getpgid(pid)
	return err == nil && group == pid
}

func killProcessGroupByPID(pid int) error {
	if !processGroupAlive(pid) {
		return nil
	}
	return syscall.Kill(-pid, syscall.SIGKILL)
}
