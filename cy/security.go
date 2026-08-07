package main

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/levmv/golems/cy/internal/session"
	"github.com/levmv/golems/cy/internal/state"
	toolruntime "github.com/levmv/golems/cy/internal/tools"
)

const (
	sandboxAuto = "auto"
	sandboxOn   = "on"
	sandboxOff  = "off"
)

type SecurityState struct {
	Backend         string
	Probe           string
	EffectivePolicy string
	Container       string
	Grants          toolruntime.SandboxGrants
	EnvNames        []string
}

type containerProbe struct {
	exists     func(string) bool
	readFile   func(string) ([]byte, error)
	detectVirt func() (string, error)
}

func normalizeSandboxPolicy(policy string) (string, error) {
	policy = strings.ToLower(strings.TrimSpace(policy))
	if policy == "" {
		policy = defaultSandboxPolicy
	}
	switch policy {
	case sandboxAuto, sandboxOn, sandboxOff:
		return policy, nil
	case "require":
		return sandboxOn, nil
	default:
		return "", fmt.Errorf("unknown sandbox policy %q (want auto, off, or on)", policy)
	}
}

func buildSecurityState(ctx context.Context, cfg Config, root string, store *state.Store) SecurityState {
	container := ""
	if cfg.SandboxPolicy == sandboxAuto {
		container = detectContainer(ctx)
	}
	effectivePolicy := effectiveSandboxPolicy(cfg.SandboxPolicy, container)
	state := SecurityState{EffectivePolicy: effectivePolicy, Grants: cfg.SandboxGrants, EnvNames: slices.Sorted(maps.Keys(cfg.ToolEnv))}
	if effectivePolicy == sandboxOff {
		state.Container = container
		return state
	}
	backend := toolruntime.SandboxBackend()
	if backend == "" {
		return unavailableSandbox(state, "no platform sandbox is available")
	}
	stateHome := resolveStateHome(cfg.Home)
	box := toolruntime.Sandbox{
		Policy:    effectivePolicy,
		Workspace: root,
		ToolHome:  toolruntime.WorkspaceToolHome(stateHome, root),
		StateHome: stateHome,
		Grants:    cfg.SandboxGrants,
		Env:       cfg.ToolEnv,
	}
	probeDir := stateHome
	if store != nil {
		probeDir = store.Dir()
	}
	probe, err := os.CreateTemp(probeDir, ".sandbox-probe-")
	if err != nil {
		return unavailableSandbox(state, "probe setup failed: "+err.Error())
	}
	probePath := probe.Name()
	defer os.Remove(probePath)
	if _, err := probe.WriteString("supervisor-only\n"); err != nil {
		_ = probe.Close()
		return unavailableSandbox(state, "probe setup failed: "+err.Error())
	}
	if err := probe.Close(); err != nil {
		return unavailableSandbox(state, "probe setup failed: "+err.Error())
	}
	command := "if IFS= read -r _ < " + bashQuote(probePath) + " 2>/dev/null; then exit 42; else exit 0; fi"
	cmd, err := box.BashCommand(command, root)
	if err != nil {
		return unavailableSandbox(state, "sandbox command failed: "+err.Error())
	}
	commandEnv := cmd.Env
	probeCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	cmd = exec.CommandContext(probeCtx, cmd.Path, cmd.Args[1:]...)
	cmd.Dir = root
	cmd.Env = commandEnv
	err = cmd.Run()
	if err == nil {
		state.Backend = backend
		return state
	}
	var exit *exec.ExitError
	if errors.As(err, &exit) && exit.ExitCode() == 42 {
		return unavailableSandbox(state, probeReadableReason(probeDir, box))
	}
	return unavailableSandbox(state, "sandbox probe failed: "+err.Error())
}

// probeReadableReason explains a probe that came back readable. The bare fact
// is a symptom; the cause is almost always that Cy's home sits inside a
// directory tool processes are granted, most often the workspace itself. Say
// which directory it is and how to move out of it, because the reader has to
// act on this and the symptom alone does not tell them how.
func probeReadableReason(probeDir string, box toolruntime.Sandbox) string {
	reason := "sandbox probe path remained readable"
	granted := grantedDirContaining(probeDir, box.GrantedDirs())
	if granted == "" {
		return reason
	}
	return fmt.Sprintf("%s: Cy's home %s is inside %s, which tool processes may read (move it with --home or CY_HOME)", reason, probeDir, granted)
}

// grantedDirContaining returns the innermost directory in granted that holds
// path, or "" when none does. Paths are compared after resolving symlinks,
// since /tmp and /var are symlinks into /private on macOS and a comparison on
// the unresolved strings would miss every one of them.
func grantedDirContaining(path string, granted []string) string {
	path = canonicalPath(path)
	innermost := ""
	for _, dir := range granted {
		if dir == "" {
			continue
		}
		dir = canonicalPath(dir)
		if path != dir && !strings.HasPrefix(path, dir+string(os.PathSeparator)) {
			continue
		}
		if len(dir) > len(innermost) {
			innermost = dir
		}
	}
	return innermost
}

func canonicalPath(path string) string {
	if resolved, err := filepath.EvalSymlinks(path); err == nil {
		return filepath.Clean(resolved)
	}
	return filepath.Clean(path)
}

func unavailableSandbox(state SecurityState, probe string) SecurityState {
	state.Probe = probe
	if state.EffectivePolicy == sandboxAuto {
		state.EffectivePolicy = sandboxOff
	}
	return state
}

func (s SecurityState) Active() bool { return s.Backend != "" }

// Journal converts what Cy decided about isolation into the shape the session
// records it in, carrying the requested policy alongside because SecurityState
// holds only the outcome. The two types stay separate: this one is a decision
// with probe machinery behind it, and the record is what a reader of the file
// needs to know afterwards.
func (s SecurityState) Journal(requested string) session.SandboxState {
	return session.SandboxState{
		Policy:    requested,
		Effective: s.EffectivePolicy,
		Backend:   s.Backend,
		Probe:     s.Probe,
		Container: s.Container,
		Grants:    s.Grants.Journal(),
		EnvNames:  s.EnvNames,
	}
}

// requireEffectiveSandbox decides whether a run that did not get a sandbox may
// proceed. It may when it asked for none; when the platform has none to give,
// since a build with no backend at all would otherwise refuse to start under
// the default policy, which is not a choice anyone made; and when a trusted
// container was detected, which is Cy deciding the outer isolation is the
// isolation rather than failing to obtain the inner one.
//
// It may not when a backend exists here and did not hold. That is a fact about
// this machine worth stopping for, and running without isolation should be
// something asked for rather than something arrived at.
func requireEffectiveSandbox(cfg Config) error {
	if cfg.Security.Active() {
		return nil
	}
	// A hide is an assertion and not a hint. Every other reason to run without
	// a fence is Cy deciding that nothing was promised; a file that says to keep
	// a directory away from tools has promised something, and honouring it
	// nowhere while starting anyway is the one outcome nobody asked for.
	if hidden := cfg.hiddenPaths(); len(hidden) > 0 {
		return fmt.Errorf("%s hides %s from tools and this run has no sandbox to hide it with: %s", cfg.ToolsFile, strings.Join(hidden, ", "), cmp.Or(cfg.Security.Probe, "sandbox is off"))
	}
	if cfg.SandboxPolicy == sandboxOff {
		return nil
	}
	if cfg.SandboxPolicy == sandboxOn {
		return fmt.Errorf("required sandbox probe failed: %s", cfg.Security.Probe)
	}
	if cfg.Security.Container != "" {
		return nil
	}
	backend := toolruntime.SandboxBackend()
	if backend == "" {
		return nil
	}
	return fmt.Errorf("%s is available here but the sandbox probe failed: %s (pass --sandbox off to run without one)", backend, cfg.Security.Probe)
}

// sandboxUnavailableNotice reports what to say when the run asked for a sandbox
// and is not getting one. It returns "" when there is nothing to report: when
// the fence is in place, when it was switched off deliberately, or when the
// interactive startup line is showing the state anyway.
//
// The cases it exists for are the two that requireEffectiveSandbox lets past: a
// platform with no backend to offer, and a container Cy decided to trust.
// Neither was asked for by name, and a scripted run would otherwise hear
// nothing. Where a backend does exist and did not hold, the run has already
// stopped rather than reached here.
func sandboxUnavailableNotice(cfg Config) string {
	if !cfg.PrintMode || cfg.SandboxPolicy != sandboxAuto || cfg.Security.Active() {
		return ""
	}
	if cfg.Security.Container != "" {
		return fmt.Sprintf("sandbox off, trusting the %s container instead", cfg.Security.Container)
	}
	notice := "sandbox unavailable, continuing without one"
	if cfg.Security.Probe != "" {
		notice += ": " + cfg.Security.Probe
	}
	return notice
}

func effectiveSandboxPolicy(requested, container string) string {
	if requested == sandboxAuto && container != "" {
		return sandboxOff
	}
	return requested
}

func detectContainer(ctx context.Context) string {
	return detectContainerWith(containerProbe{
		exists: func(path string) bool {
			_, err := os.Stat(path)
			return err == nil
		},
		readFile: os.ReadFile,
		detectVirt: func() (string, error) {
			detectCtx, cancel := context.WithTimeout(ctx, time.Second)
			defer cancel()
			output, err := exec.CommandContext(detectCtx, "/usr/bin/systemd-detect-virt", "--container").Output()
			return string(output), err
		},
	})
}

func detectContainerWith(probe containerProbe) string {
	for _, marker := range []struct {
		path string
		id   string
	}{
		{path: "/run/.containerenv", id: "podman"},
		{path: "/.dockerenv", id: "docker"},
	} {
		if probe.exists != nil && probe.exists(marker.path) {
			return marker.id
		}
	}
	for _, source := range []string{"/run/systemd/container", "/proc/1/environ"} {
		if probe.readFile == nil {
			break
		}
		raw, err := probe.readFile(source)
		if err != nil {
			continue
		}
		if id := trustedContainerID(raw); id != "" {
			return id
		}
	}
	if probe.detectVirt != nil {
		if output, err := probe.detectVirt(); err == nil {
			if id := trustedContainerID([]byte(output)); id != "" {
				return id
			}
		}
	}
	return ""
}

func trustedContainerID(raw []byte) string {
	for _, field := range strings.FieldsFunc(string(raw), func(r rune) bool {
		return r == 0 || r == '\n' || r == '\r'
	}) {
		field = strings.ToLower(strings.TrimSpace(field))
		field = strings.TrimPrefix(field, "container=")
		switch field {
		case "docker", "lxc", "lxc-libvirt", "openvz", "podman", "systemd-nspawn":
			return field
		}
	}
	return ""
}

func resolveStateHome(home string) string {
	resolved, err := session.ResolveHome(home)
	if err != nil {
		return home
	}
	return resolved
}

func bashQuote(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "'\\''") + "'"
}

func (s SecurityState) Compact() string {
	backend := s.Backend
	if backend == "" {
		backend = "off"
	}
	if s.Container != "" {
		backend += fmt.Sprintf(" (%s)", s.Container)
	}
	return fmt.Sprintf("sandbox: %s · network: open", backend)
}
