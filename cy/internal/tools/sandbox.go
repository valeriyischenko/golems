package tools

import (
	"crypto/sha256"
	"errors"
	"fmt"
	"os/exec"
	"path/filepath"
)

const (
	sandboxAuto = "auto"
	sandboxOn   = "on"
	sandboxOff  = "off"
)

func RunSandboxChildIfRequested() bool { return runSandboxChildIfRequested() }

func HardenSupervisor() { hardenSupervisor() }

func SandboxBackend() string { return sandboxBackend() }

// SandboxedCommand builds a command that runs program under this platform's
// fence. program is a path, never resolved against PATH by the fence itself,
// and args is what the program sees as its own arguments with args[0] included
// — so a caller can give a program a process name that differs from its path.
//
// Callers that want a shell want SandboxedBashCommand, which is one use of
// this rather than a mechanism beside it.
func SandboxedCommand(program string, args []string, workspace, workdir, home, policy string) (*exec.Cmd, error) {
	if program == "" {
		return nil, errors.New("sandboxed command needs a program to run")
	}
	if len(args) == 0 {
		return nil, errors.New("sandboxed command needs at least a process name")
	}
	return sandboxedCommand(program, args, workspace, workdir, home, policy)
}

func SandboxedBashCommand(command, workspace, workdir, home, policy string) (*exec.Cmd, error) {
	return sandboxedBashCommand(command, workspace, workdir, home, policy)
}

func ambientBashCommand(command, workdir string) *exec.Cmd {
	cmd := exec.Command("bash", "-lc", command)
	cmd.Dir = workdir
	return cmd
}

// ambientCommand runs a program with no fence around it. Unlike the shell
// above — which is a human's or the model's own command line, and has always
// inherited this process's environment when unsandboxed — a program launched
// from configuration gets the scrubbed environment either way. Losing the
// fence should not also mean handing it the supervisor's credentials.
func ambientCommand(program string, args []string, workdir, home string) *exec.Cmd {
	cmd := exec.Command(program, args[1:]...)
	cmd.Args = args
	cmd.Dir = workdir
	cmd.Env = minimalToolEnv(home)
	return cmd
}

func WorkspaceToolHome(home, root string) string {
	digest := sha256.Sum256([]byte(root))
	return filepath.Join(home, "tool-cache", fmt.Sprintf("%x", digest[:8]))
}

// WorkspaceToolTemp is the scratch directory a sandboxed tool process gets as
// TMPDIR. It lives inside the per-workspace tool home so that temporary files
// are covered by the same grant as the rest of that home, and so that the
// machine-wide temp directory does not have to be granted to reach it.
func WorkspaceToolTemp(home string) string {
	return filepath.Join(home, "tmp")
}

// SandboxGrantedDirs lists the directories a sandboxed tool process can still
// reach on this platform, given the workspace and tool home it was built for.
func SandboxGrantedDirs(workspace, home string) []string {
	return sandboxGrantedDirs(workspace, home)
}
