package tools

import (
	"crypto/sha256"
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

func SandboxedBashCommand(command, workspace, workdir, home, policy string) (*exec.Cmd, error) {
	return sandboxedBashCommand(command, workspace, workdir, home, policy)
}

func ambientBashCommand(command, workdir string) *exec.Cmd {
	cmd := exec.Command("bash", "-lc", command)
	cmd.Dir = workdir
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
