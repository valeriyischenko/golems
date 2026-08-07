//go:build linux

package tools

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"syscall"

	"github.com/landlock-lsm/go-landlock/landlock"
	"golang.org/x/sys/unix"
)

const sandboxChildArg = "__cy_sandbox_run"

const (
	envSandboxProgram = "CY_INTERNAL_SANDBOX_PROGRAM"
	envSandboxArgs    = "CY_INTERNAL_SANDBOX_ARGS"
	envSandboxRoot    = "CY_INTERNAL_SANDBOX_ROOT"
	envSandboxHome    = "CY_INTERNAL_SANDBOX_HOME"
	envSandboxPolicy  = "CY_INTERNAL_SANDBOX_POLICY"
)

func runSandboxChildIfRequested() bool {
	if len(os.Args) < 2 || os.Args[1] != sandboxChildArg {
		return false
	}
	runSandboxedProgram()
	return true
}

func sandboxBackend() string { return "landlock" }

// sandboxedCommand re-execs this binary, which restricts itself and then execs
// program. The restriction has to be applied by the process that will run the
// program: a Landlock ruleset covers the thread that installs it and everything
// it goes on to spawn, and can never be relaxed, so the supervisor must stay
// outside it.
func sandboxedCommand(program string, args []string, workspace, workdir, home, policy string) (*exec.Cmd, error) {
	if policy == sandboxOff {
		return ambientCommand(program, args, workdir, home), nil
	}
	encoded, err := json.Marshal(args)
	if err != nil {
		return nil, fmt.Errorf("encode sandboxed arguments: %w", err)
	}
	cmd := exec.Command("/proc/self/exe", sandboxChildArg)
	cmd.Dir = workdir
	cmd.Env = sandboxControlEnv(program, string(encoded), workspace, home, policy)
	return cmd, nil
}

func sandboxedBashCommand(command, workspace, workdir, home, policy string) (*exec.Cmd, error) {
	if policy == sandboxOff {
		return ambientBashCommand(command, workdir), nil
	}
	bash := systemBashPath()
	if bash == "" {
		return nil, errors.New("system Bash is unavailable")
	}
	return sandboxedCommand(bash, []string{"bash", "-lc", command}, workspace, workdir, home, policy)
}

func sandboxControlEnv(program, args, workspace, home, policy string) []string {
	return append(minimalToolEnv(home),
		envSandboxProgram+"="+program,
		envSandboxArgs+"="+args,
		envSandboxRoot+"="+workspace,
		envSandboxHome+"="+home,
		envSandboxPolicy+"="+policy,
	)
}

func runSandboxedProgram() {
	program := os.Getenv(envSandboxProgram)
	workspace := os.Getenv(envSandboxRoot)
	home := os.Getenv(envSandboxHome)
	policy := os.Getenv(envSandboxPolicy)
	var args []string
	if err := json.Unmarshal([]byte(os.Getenv(envSandboxArgs)), &args); err != nil {
		fmt.Fprintf(os.Stderr, "sandbox: decode arguments: %v\n", err)
		os.Exit(126)
	}
	if program == "" || len(args) == 0 {
		fmt.Fprintln(os.Stderr, "sandbox: no program to run")
		os.Exit(126)
	}
	runtime.LockOSThread()
	if err := applyToolLandlock(workspace, home, policy); err != nil {
		fmt.Fprintf(os.Stderr, "sandbox: %v\n", err)
		os.Exit(126)
	}
	// The control variables are deliberately not passed on: the program runs
	// with the same scrubbed environment it would have had without them.
	if err := syscall.Exec(program, args, minimalToolEnv(home)); err != nil {
		fmt.Fprintf(os.Stderr, "sandbox exec %s: %v\n", program, err)
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
