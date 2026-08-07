//go:build linux

package tools

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"syscall"

	"github.com/landlock-lsm/go-landlock/landlock"
	"golang.org/x/sys/unix"
)

const sandboxChildArg = "__cy_sandbox_run"

const (
	envSandboxProgram = "CY_INTERNAL_SANDBOX_PROGRAM"
	envSandboxArgs    = "CY_INTERNAL_SANDBOX_ARGS"
	envSandboxFence   = "CY_INTERNAL_SANDBOX_FENCE"
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
func sandboxedCommand(box Sandbox, program string, args []string, workdir string) (*exec.Cmd, error) {
	if box.Policy == sandboxOff {
		return ambientCommand(program, args, workdir, box.ToolHome), nil
	}
	encodedArgs, err := json.Marshal(args)
	if err != nil {
		return nil, fmt.Errorf("encode sandboxed arguments: %w", err)
	}
	// The fence is passed across already resolved rather than rebuilt from
	// configuration on the far side: the child is not the process that read the
	// tools file, and a ruleset that could be recomputed differently there would
	// be a fence nobody had checked.
	encodedFence, err := json.Marshal(box)
	if err != nil {
		return nil, fmt.Errorf("encode sandbox: %w", err)
	}
	cmd := exec.Command("/proc/self/exe", sandboxChildArg)
	cmd.Dir = workdir
	cmd.Env = append(minimalToolEnv(box.ToolHome),
		envSandboxProgram+"="+program,
		envSandboxArgs+"="+string(encodedArgs),
		envSandboxFence+"="+string(encodedFence),
	)
	return cmd, nil
}

func sandboxedBashCommand(box Sandbox, command, workdir string) (*exec.Cmd, error) {
	if box.Policy == sandboxOff {
		return ambientBashCommand(command, workdir), nil
	}
	bash := systemBashPath()
	if bash == "" {
		return nil, errors.New("system Bash is unavailable")
	}
	return sandboxedCommand(box, bash, []string{"bash", "-lc", command}, workdir)
}

func runSandboxedProgram() {
	program := os.Getenv(envSandboxProgram)
	var args []string
	var box Sandbox
	if err := json.Unmarshal([]byte(os.Getenv(envSandboxArgs)), &args); err != nil {
		fmt.Fprintf(os.Stderr, "sandbox: decode arguments: %v\n", err)
		os.Exit(126)
	}
	if err := json.Unmarshal([]byte(os.Getenv(envSandboxFence)), &box); err != nil {
		fmt.Fprintf(os.Stderr, "sandbox: decode sandbox: %v\n", err)
		os.Exit(126)
	}
	if program == "" || len(args) == 0 {
		fmt.Fprintln(os.Stderr, "sandbox: no program to run")
		os.Exit(126)
	}
	runtime.LockOSThread()
	if err := applyToolLandlock(box); err != nil {
		fmt.Fprintf(os.Stderr, "sandbox: %v\n", err)
		os.Exit(126)
	}
	// The control variables are deliberately not passed on: the program runs
	// with the same scrubbed environment it would have had without them.
	if err := syscall.Exec(program, args, minimalToolEnv(box.ToolHome)); err != nil {
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

// The machine-wide temp directories are deliberately never writable: they are
// shared with every other process on the box, so granting them means anything a
// tool leaves in TMPDIR is readable by anyone, and anything another user leaves
// there is reachable by the tool. Tools get their own temp inside the tool home
// instead, and minimalToolEnv points TMPDIR at it.
func applyToolLandlock(box Sandbox) error {
	plan := box.plan()
	readDirs, readFiles := plan.expandAll(plan.read)
	writeDirs, writeFiles := plan.expandAll(plan.write)
	writeFiles = append(writeFiles, "/dev/null", "/dev/zero", "/dev/full", "/dev/random", "/dev/urandom", "/dev/tty")
	rules := []landlock.Rule{
		landlock.RODirs(readDirs...).IgnoreIfMissing(),
		landlock.ROFiles(readFiles...).IgnoreIfMissing(),
		landlock.RWDirs(writeDirs...).IgnoreIfMissing(),
		landlock.RWFiles(writeFiles...).IgnoreIfMissing(),
	}
	if box.Policy == sandboxOn {
		return landlock.V1.RestrictPaths(rules...)
	}
	return landlock.V9.BestEffort().RestrictPaths(rules...)
}

// expandAll rewrites the granted paths so that nothing hidden is inside one of
// them, and sorts what comes out into directories and files.
//
// Landlock has no way to say "everything except this". A rule only ever grants,
// and the rule that applies to a path is the most specific one covering it, so
// a grant that swallows a hidden path has to be replaced by grants on that
// path's siblings, level by level, until the hidden path is the one entry
// nobody named. The lists this produces are short in practice — three or four
// directories deep, twenty-odd entries — because a directory rule already
// covers its whole subtree, including what is created under it later.
//
// What it does not cover is a sibling created after the ruleset is installed.
// Such a path matches no rule and is denied, which is the safe direction and
// worth knowing about: this reconstructs a mask rather than being one.
func (p sandboxPlan) expandAll(granted []string) (dirs, files []string) {
	for _, path := range granted {
		for _, entry := range p.expand(path) {
			info, err := os.Stat(entry)
			// An absent path is left with the directories, where
			// IgnoreIfMissing drops it; that is how the built-in list carries
			// /nix and /snap on machines that have neither.
			if err != nil || info.IsDir() {
				dirs = append(dirs, entry)
				continue
			}
			files = append(files, entry)
		}
	}
	return dirs, files
}

func (p sandboxPlan) expand(dir string) []string {
	if len(p.hidesUnder(dir)) == 0 {
		return []string{dir}
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	var granted []string
	for _, entry := range entries {
		// Resolved, because a Landlock rule attaches to the inode a path leads
		// to: a symlink pointing into a hidden directory would otherwise grant
		// it straight back under another name.
		child := canonicalSandboxPath(filepath.Join(dir, entry.Name()))
		// Skipped whole. A grant that belongs inside this subtree is in the
		// plan under its own name and is expanded on its own account, which is
		// how the tool home survives Cy's home being hidden around it.
		if p.hidden(child) {
			continue
		}
		granted = append(granted, p.expand(child)...)
	}
	return granted
}

func hardenSupervisor() { _ = unix.Prctl(unix.PR_SET_DUMPABLE, 0, 0, 0, 0) }
