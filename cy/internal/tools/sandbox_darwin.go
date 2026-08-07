//go:build darwin

package tools

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
)

const sandboxExecPath = "/usr/bin/sandbox-exec"

func runSandboxChildIfRequested() bool { return false }

func sandboxBackend() string { return "seatbelt" }

// systemBashPath mirrors the Linux helper of the same name. macOS always ships
// bash at this path, so there is nothing to search for.
func systemBashPath() string { return "/bin/bash" }

// sandboxedCommand wraps the program in sandbox-exec, which applies the profile
// and then execs it. Note that args[0] cannot be honoured here: sandbox-exec
// takes a path and builds the argument list itself, so the program always sees
// its own path as the process name.
func sandboxedCommand(program string, args []string, workspace, workdir, home, policy string) (*exec.Cmd, error) {
	if policy == sandboxOff {
		return ambientCommand(program, args, workdir, home), nil
	}
	if _, err := os.Stat(sandboxExecPath); err != nil {
		if policy == sandboxOn {
			return nil, errors.New("macOS sandbox-exec is unavailable")
		}
		return ambientCommand(program, args, workdir, home), nil
	}
	sandboxArgs := append([]string{
		"-D", "WORKSPACE=" + canonicalSandboxPath(workspace),
		"-D", "TOOL_HOME=" + canonicalSandboxPath(home),
		"-p", seatbeltProfile,
		program,
	}, args[1:]...)
	cmd := exec.Command(sandboxExecPath, sandboxArgs...)
	cmd.Dir = workdir
	cmd.Env = minimalToolEnv(home)
	return cmd, nil
}

func sandboxedBashCommand(command, workspace, workdir, home, policy string) (*exec.Cmd, error) {
	if policy == sandboxOff {
		return ambientBashCommand(command, workdir), nil
	}
	if _, err := os.Stat(sandboxExecPath); err != nil {
		if policy == sandboxOn {
			return nil, errors.New("macOS sandbox-exec is unavailable")
		}
		return ambientBashCommand(command, workdir), nil
	}
	return sandboxedCommand(systemBashPath(), []string{systemBashPath(), "-lc", command}, workspace, workdir, home, policy)
}

// sandboxReadOnlyDirs holds the system directories named as read-only subpaths
// in the profile below. The two lists have to agree, and a test checks that
// they do.
//
// They are whole trees because that is what an interpreter finding its standard
// library or a tool finding its support files needs, and granting them exposes
// nothing the invoking user could not already read: Seatbelt rules subtract
// from Unix permissions and never add to them. They are still coarser than the
// need. /Library is the clearest case — see the Security section of the README
// for what a narrowing deny block would look like.
var sandboxReadOnlyDirs = []string{"/Applications", "/Library", "/System", "/bin", "/dev", "/etc", "/nix", "/opt", "/private/etc", "/private/var/db/dyld", "/private/var/db/timezone", "/private/var/select", "/sbin", "/usr"}

// sandboxWritableDirs holds everything a tool process may write. The
// machine-wide temp directories are deliberately absent: they are shared with
// every other process on the box, so granting them means anything a tool
// leaves in TMPDIR is readable by anyone, and anything another user leaves
// there is reachable by the tool. Tools get their own temp inside the home
// instead, and minimalToolEnv points TMPDIR at it.
func sandboxWritableDirs(workspace, home string) []string {
	return []string{canonicalSandboxPath(workspace), canonicalSandboxPath(home)}
}

func sandboxGrantedDirs(workspace, home string) []string {
	return append(sandboxWritableDirs(workspace, home), sandboxReadOnlyDirs...)
}

func canonicalSandboxPath(path string) string {
	resolved, err := filepath.EvalSymlinks(path)
	if err == nil {
		return resolved
	}
	return filepath.Clean(path)
}

func hardenSupervisor() {}

// This profile focuses on direct filesystem access and leaves the network
// open. The process and IPC allowances keep common CLI tools usable without
// turning Cy into a general macOS permission manager.
const seatbeltProfile = `(version 1)
(deny default)

(allow process-exec)
(allow process-fork)
(allow process-info* (target same-sandbox))
(allow signal (target same-sandbox))

(allow file-read*
  (literal "/")
  (subpath (param "WORKSPACE"))
  (subpath (param "TOOL_HOME"))
  (subpath "/Applications")
  (subpath "/Library")
  (subpath "/System")
  (subpath "/bin")
  (subpath "/dev")
  (subpath "/etc")
  (subpath "/nix")
  (subpath "/opt")
  (subpath "/private/etc")
  (subpath "/private/var/db/dyld")
  (subpath "/private/var/db/timezone")
  (subpath "/private/var/select")
  (subpath "/sbin")
  (subpath "/usr"))
(allow file-read-metadata)

(allow file-write*
  (subpath (param "WORKSPACE"))
  (subpath (param "TOOL_HOME"))
  (subpath "/dev/fd")
  (literal "/dev/null")
  (literal "/dev/ptmx")
  (literal "/dev/stderr")
  (literal "/dev/stdout")
  (literal "/dev/tty")
  (regex #"^/dev/ttys[0-9]*$"))
(allow file-ioctl (regex #"^/dev/tty.*"))
(allow pseudo-tty)

(allow network*)
(allow system-socket)
(allow sysctl-read)
(allow user-preference-read)
(allow ipc-posix-shm)
(allow ipc-posix-sem)
(allow distributed-notification-post)
(allow mach-lookup
  (global-name "com.apple.SecurityServer")
  (global-name "com.apple.SystemConfiguration.DNSConfiguration")
  (global-name "com.apple.SystemConfiguration.configd")
  (global-name "com.apple.bsd.dirhelper")
  (global-name "com.apple.mDNSResponder")
  (global-name "com.apple.mDNSResponderHelper")
  (global-name "com.apple.networkd")
  (global-name "com.apple.ocspd")
  (global-name "com.apple.system.opendirectoryd.libinfo")
  (global-name "com.apple.system.opendirectoryd.membership")
  (global-name "com.apple.sysmond")
  (global-name "com.apple.trustd")
  (global-name "com.apple.trustd.agent"))
`
