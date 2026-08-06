//go:build linux

package tools

import (
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"syscall"

	"github.com/landlock-lsm/go-landlock/landlock"
	"golang.org/x/sys/unix"
)

const sandboxChildArg = "__cy_sandbox_bash"

const (
	envSandboxCommand = "CY_INTERNAL_SANDBOX_COMMAND"
	envSandboxRoot    = "CY_INTERNAL_SANDBOX_ROOT"
	envSandboxHome    = "CY_INTERNAL_SANDBOX_HOME"
	envSandboxPolicy  = "CY_INTERNAL_SANDBOX_POLICY"
)

func runSandboxChildIfRequested() bool {
	if len(os.Args) < 2 || os.Args[1] != sandboxChildArg {
		return false
	}
	runSandboxedBash()
	return true
}

func sandboxBackend() string { return "landlock" }

func sandboxedBashCommand(command, workspace, workdir, home, policy string) (*exec.Cmd, error) {
	if policy == sandboxOff {
		return ambientBashCommand(command, workdir), nil
	}
	cmd := exec.Command("/proc/self/exe", sandboxChildArg)
	cmd.Dir = workdir
	cmd.Env = sandboxControlEnv(command, workspace, home, policy)
	return cmd, nil
}

func sandboxControlEnv(command, workspace, home, policy string) []string {
	if policy == sandboxOff {
		return nil
	}
	return append(minimalToolEnv(home),
		envSandboxCommand+"="+command,
		envSandboxRoot+"="+workspace,
		envSandboxHome+"="+home,
		envSandboxPolicy+"="+policy,
	)
}

func runSandboxedBash() {
	command := os.Getenv(envSandboxCommand)
	workspace := os.Getenv(envSandboxRoot)
	home := os.Getenv(envSandboxHome)
	policy := os.Getenv(envSandboxPolicy)
	bash := systemBashPath()
	if bash == "" {
		fmt.Fprintln(os.Stderr, "sandbox: system Bash is unavailable")
		os.Exit(126)
	}
	runtime.LockOSThread()
	if err := applyToolLandlock(workspace, home, policy); err != nil {
		fmt.Fprintf(os.Stderr, "sandbox: %v\n", err)
		os.Exit(126)
	}
	if err := syscall.Exec(bash, []string{"bash", "-lc", command}, minimalToolEnv(home)); err != nil {
		fmt.Fprintf(os.Stderr, "sandbox exec bash: %v\n", err)
		os.Exit(126)
	}
}

func systemBashPath() string {
	for _, path := range []string{"/bin/bash", "/usr/bin/bash", "/run/current-system/sw/bin/bash"} {
		if info, err := os.Stat(path); err == nil && !info.IsDir() && info.Mode()&0o111 != 0 {
			return path
		}
	}
	return ""
}

// sandboxReadOnlyDirs holds the system directories a tool process may read.
var sandboxReadOnlyDirs = []string{"/usr", "/bin", "/sbin", "/lib", "/lib32", "/lib64", "/libx32", "/etc", "/nix", "/opt", "/snap", "/run/systemd/resolve"}

// sandboxWritableDirs holds everything a tool process may write. The
// machine-wide temp directories are deliberately absent: they are shared with
// every other process on the box, so granting them means anything a tool
// leaves in TMPDIR is readable by anyone, and anything another user leaves
// there is reachable by the tool. Tools get their own temp inside the home
// instead, and minimalToolEnv points TMPDIR at it.
func sandboxWritableDirs(workspace, home string) []string {
	return []string{workspace, home}
}

func sandboxGrantedDirs(workspace, home string) []string {
	return append(sandboxWritableDirs(workspace, home), sandboxReadOnlyDirs...)
}

func applyToolLandlock(workspace, home, policy string) error {
	rules := []landlock.Rule{
		landlock.RODirs(sandboxReadOnlyDirs...).IgnoreIfMissing(),
		landlock.RWDirs(sandboxWritableDirs(workspace, home)...).IgnoreIfMissing(),
		landlock.RWFiles("/dev/null", "/dev/zero", "/dev/full", "/dev/random", "/dev/urandom", "/dev/tty").IgnoreIfMissing(),
	}
	if policy == sandboxOn {
		return landlock.V1.RestrictPaths(rules...)
	}
	return landlock.V9.BestEffort().RestrictPaths(rules...)
}

func hardenSupervisor() { _ = unix.Prctl(unix.PR_SET_DUMPABLE, 0, 0, 0, 0) }
