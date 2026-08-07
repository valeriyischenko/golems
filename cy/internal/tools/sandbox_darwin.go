//go:build darwin

package tools

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
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
func sandboxedCommand(box Sandbox, program string, args []string, workdir string) (*exec.Cmd, error) {
	if box.Policy == sandboxOff {
		return ambientCommand(box, program, args, workdir), nil
	}
	if _, err := os.Stat(sandboxExecPath); err != nil {
		if box.Policy == sandboxOn {
			return nil, errors.New("macOS sandbox-exec is unavailable")
		}
		return ambientCommand(box, program, args, workdir), nil
	}
	sandboxArgs := append([]string{"-p", seatbeltProfile(box), program}, args[1:]...)
	cmd := exec.Command(sandboxExecPath, sandboxArgs...)
	cmd.Dir = workdir
	cmd.Env = box.environ()
	return cmd, nil
}

func sandboxedBashCommand(box Sandbox, command, workdir string) (*exec.Cmd, error) {
	if box.Policy == sandboxOff {
		return ambientBashCommand(box, command, workdir), nil
	}
	if _, err := os.Stat(sandboxExecPath); err != nil {
		if box.Policy == sandboxOn {
			return nil, errors.New("macOS sandbox-exec is unavailable")
		}
		return ambientBashCommand(box, command, workdir), nil
	}
	return sandboxedCommand(box, systemBashPath(), []string{systemBashPath(), "-lc", command}, workdir)
}

// sandboxReadOnlyDirs holds the system directories a tool process may read.
//
// They are whole trees because that is what an interpreter finding its standard
// library or a tool finding its support files needs, and granting them exposes
// nothing the invoking user could not already read: Seatbelt rules subtract
// from Unix permissions and never add to them. They are still coarser than the
// need. /Library is the clearest case — see the Security section of the README
// for what a narrowing deny block would look like.
var sandboxReadOnlyDirs = []string{"/Applications", "/Library", "/System", "/bin", "/dev", "/etc", "/nix", "/opt", "/private/etc", "/private/var/db/dyld", "/private/var/db/timezone", "/private/var/select", "/sbin", "/usr"}

func hardenSupervisor() {}

// seatbeltProfile writes the profile for one tool process.
//
// Seatbelt takes the last matching rule, so hiding is a deny written after the
// grants rather than the reconstruction Landlock needs on the other platform,
// and a grant that sits inside something hidden is simply written once more at
// the end. The two backends agree on what the words mean; only this differs.
func seatbeltProfile(box Sandbox) string {
	plan := box.plan()
	profile := strings.Builder{}
	profile.WriteString(seatbeltPreamble)
	writeSeatbeltRule(&profile, "allow file-read*", plan.read)
	writeSeatbeltRule(&profile, "allow file-read* file-write*", plan.write)
	writeSeatbeltRule(&profile, "deny file-read* file-write*", plan.hide)
	writeSeatbeltRule(&profile, "allow file-read*", plan.belowHidden(plan.read))
	writeSeatbeltRule(&profile, "allow file-read* file-write*", plan.belowHidden(plan.write))
	return profile.String()
}

func writeSeatbeltRule(profile *strings.Builder, operations string, paths []string) {
	if len(paths) == 0 {
		return
	}
	fmt.Fprintf(profile, "(%s", operations)
	for _, path := range paths {
		fmt.Fprintf(profile, "\n  (subpath %q)", path)
	}
	profile.WriteString(")\n")
}

// This preamble focuses on direct filesystem access and leaves the network
// open. The process and IPC allowances keep common CLI tools usable without
// turning Cy into a general macOS permission manager. What a particular run may
// read and write is appended to it by seatbeltProfile.
const seatbeltPreamble = `(version 1)
(deny default)

(allow process-exec)
(allow process-fork)
(allow process-info* (target same-sandbox))
(allow signal (target same-sandbox))

(allow file-read* (literal "/"))
(allow file-read-metadata)

(allow file-write*
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
