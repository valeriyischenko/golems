package tools

import (
	"crypto/sha256"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"

	"github.com/levmv/golems/cy/internal/session"
)

const (
	sandboxAuto = "auto"
	sandboxOn   = "on"
	sandboxOff  = "off"
)

func RunSandboxChildIfRequested() bool { return runSandboxChildIfRequested() }

func HardenSupervisor() { hardenSupervisor() }

func SandboxBackend() string { return sandboxBackend() }

// Sandbox is everything that decides what one tool process may reach: the
// policy that says whether a fence is applied at all, the two directories every
// tool gets, and whatever configuration added or took away.
//
// It travels as a value rather than as a growing argument list because every
// caller needs all of it and none of it varies within a call.
type Sandbox struct {
	Policy    string
	Workspace string
	// ToolHome is the per-workspace directory a tool process gets as $HOME. It
	// lives inside StateHome and is the one part of it a tool may reach.
	ToolHome string
	// StateHome is Cy's own home, and is hidden from every tool process whatever
	// configuration says. The journal and the provider credentials are in it,
	// and keeping a program Cy spawned out of them is the whole reason this
	// fence exists on top of whatever the deployment wraps Cy in: an outer
	// sandbox has one ruleset for the lot and cannot tell Cy from Cy's children.
	StateHome string
	Grants    SandboxGrants
}

// SandboxGrants is what configuration adds to, and takes away from, the fence
// around a tool process. Paths here are absolute and have been checked to
// exist; see resolveSandboxPaths for why that happens at startup.
type SandboxGrants struct {
	Read  []string `json:"read,omitempty"`
	Write []string `json:"write,omitempty"`
	Hide  []string `json:"hide,omitempty"`
}

// journal is these grants in the shape the session record keeps them.
// Journal is this set of grants as the session should record them: the paths as
// resolved at startup, so a reader of the file sees what the fence was built
// from rather than what the configuration said before resolution.
func (g SandboxGrants) Journal() session.SandboxGrants {
	return session.SandboxGrants{Read: g.Read, Write: g.Write, Hide: g.Hide}
}

func (g SandboxGrants) empty() bool {
	return len(g.Read) == 0 && len(g.Write) == 0 && len(g.Hide) == 0
}

// merge lays one set of grants over another, which is how a tool's own block
// combines with the run-level one. Both are kept whole: a hide added by either
// applies, since taking access away is the direction it is safe to accumulate.
func (g SandboxGrants) merge(over SandboxGrants) SandboxGrants {
	return SandboxGrants{
		Read:  append(slices.Clone(g.Read), over.Read...),
		Write: append(slices.Clone(g.Write), over.Write...),
		Hide:  append(slices.Clone(g.Hide), over.Hide...),
	}
}

// With returns this sandbox with a tool's own grants folded in, for the one
// tool that declared them.
func (s Sandbox) With(grants SandboxGrants) Sandbox {
	if grants.empty() {
		return s
	}
	s.Grants = s.Grants.merge(grants)
	return s
}

// Command builds a command that runs program under this platform's fence.
// program is a path, never resolved against PATH by the fence itself, and args
// is what the program sees as its own arguments with args[0] included — so a
// caller can give a program a process name that differs from its path.
//
// Callers that want a shell want BashCommand, which is one use of this rather
// than a mechanism beside it.
func (s Sandbox) Command(program string, args []string, workdir string) (*exec.Cmd, error) {
	if program == "" {
		return nil, errors.New("sandboxed command needs a program to run")
	}
	if len(args) == 0 {
		return nil, errors.New("sandboxed command needs at least a process name")
	}
	return sandboxedCommand(s, program, args, workdir)
}

func (s Sandbox) BashCommand(command, workdir string) (*exec.Cmd, error) {
	return sandboxedBashCommand(s, command, workdir)
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

// GrantedDirs lists the directories a sandboxed tool process can still reach on
// this platform.
func (s Sandbox) GrantedDirs() []string {
	plan := s.plan()
	return append(plan.read, plan.write...)
}

// sandboxPlan is the fence as it will be built: what may be read, what may be
// written, and what neither applies to however broadly the first two were
// granted.
type sandboxPlan struct{ read, write, hide []string }

// plan resolves the grants against the platform's built-in lists.
//
// Where a grant and a hide name the same path the hide wins, and anything
// deeper than a hidden path is more specific and survives it. That second half
// is not a nicety: the tool home sits inside Cy's home, which is always hidden,
// and this is what keeps it reachable.
func (s Sandbox) plan() sandboxPlan {
	plan := sandboxPlan{
		read:  canonicalPaths(append(slices.Clone(sandboxReadOnlyDirs), s.Grants.Read...)),
		write: canonicalPaths(append([]string{s.Workspace, s.ToolHome}, s.Grants.Write...)),
		hide:  canonicalPaths(append([]string{s.StateHome}, s.Grants.Hide...)),
	}
	granted := func(paths []string) []string {
		return slices.DeleteFunc(paths, func(path string) bool { return slices.Contains(plan.hide, path) })
	}
	plan.read, plan.write = granted(plan.read), granted(plan.write)
	return plan
}

// hidesUnder returns the hidden paths lying strictly inside dir.
func (p sandboxPlan) hidesUnder(dir string) []string {
	var found []string
	for _, hidden := range p.hide {
		if hidden != dir && pathUnder(hidden, dir) {
			found = append(found, hidden)
		}
	}
	return found
}

// hidden reports whether path lies inside something hidden, leaving aside the
// more specific grants that may sit further in.
func (p sandboxPlan) hidden(path string) bool {
	for _, candidate := range p.hide {
		if pathUnder(path, candidate) {
			return true
		}
	}
	return false
}

// belowHidden picks out the grants that are inside something hidden. They are
// the more specific rule and so they stand, but a backend that expresses hiding
// as a deny has to say them again afterwards for that to be true.
func (p sandboxPlan) belowHidden(granted []string) []string {
	var found []string
	for _, path := range granted {
		for _, hidden := range p.hide {
			if path != hidden && pathUnder(path, hidden) {
				found = append(found, path)
				break
			}
		}
	}
	return found
}

// pathUnder reports whether path is dir or lies inside it.
func pathUnder(path, dir string) bool {
	return path == dir || strings.HasPrefix(path, dir+string(os.PathSeparator))
}

// canonicalPaths resolves, sorts and deduplicates a list of paths. Sorting is
// what makes two runs of the same configuration build the same fence; resolving
// symlinks is what makes containment comparable, since /tmp and /var are
// symlinks into /private on macOS and a comparison on the written strings would
// miss every one of them.
func canonicalPaths(paths []string) []string {
	resolved := make([]string, 0, len(paths))
	for _, path := range paths {
		if strings.TrimSpace(path) == "" {
			continue
		}
		resolved = append(resolved, canonicalSandboxPath(path))
	}
	slices.Sort(resolved)
	return slices.Compact(resolved)
}

func canonicalSandboxPath(path string) string {
	if resolved, err := filepath.EvalSymlinks(path); err == nil {
		return resolved
	}
	return filepath.Clean(path)
}

// resolveSandboxPaths turns the paths written in a tools file into absolute
// ones. A relative path is relative to the directory holding that file, which
// is the deployment: a tool's interpreter and its packages sit beside the file
// that declares it, and naming them that way is what lets the same deployment be
// copied somewhere else without editing. A leading ~ is the invoking user's
// home, for the things that live there instead.
//
// A path that is not there is a configuration error rather than something
// quietly skipped. The built-in lists ignore what is missing because they are
// one list for every distribution; a path somebody typed is a typo, and for a
// hide the consequence of ignoring it is a run that does not hide what it was
// told to.
func resolveSandboxPaths(base string, paths []string) ([]string, error) {
	resolved := make([]string, 0, len(paths))
	for _, path := range paths {
		trimmed := strings.TrimSpace(path)
		switch {
		case trimmed == "":
			return nil, errors.New("sandbox path is empty")
		case trimmed == "~" || strings.HasPrefix(trimmed, "~/"):
			home, err := os.UserHomeDir()
			if err != nil {
				return nil, fmt.Errorf("sandbox path %q: %w", path, err)
			}
			trimmed = filepath.Join(home, strings.TrimPrefix(trimmed, "~"))
		case !filepath.IsAbs(trimmed):
			trimmed = filepath.Join(base, trimmed)
		}
		if _, err := os.Stat(trimmed); err != nil {
			return nil, fmt.Errorf("sandbox path %q: %w", path, err)
		}
		resolved = append(resolved, filepath.Clean(trimmed))
	}
	return resolved, nil
}

func (g *SandboxGrants) resolve(base string) error {
	var err error
	if g.Read, err = resolveSandboxPaths(base, g.Read); err != nil {
		return err
	}
	if g.Write, err = resolveSandboxPaths(base, g.Write); err != nil {
		return err
	}
	g.Hide, err = resolveSandboxPaths(base, g.Hide)
	return err
}
