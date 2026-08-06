package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	toolruntime "github.com/levmv/golems/cy/internal/tools"
)

func TestNormalizeSandboxPolicySupportsOnAndLegacyRequire(t *testing.T) {
	for _, input := range []string{sandboxOn, "require"} {
		got, err := normalizeSandboxPolicy(input)
		if err != nil || got != sandboxOn {
			t.Fatalf("normalizeSandboxPolicy(%q) = %q, %v", input, got, err)
		}
	}
}

func TestEffectiveSandboxPolicy(t *testing.T) {
	for _, test := range []struct {
		requested string
		container string
		want      string
	}{
		{requested: sandboxAuto, want: sandboxAuto},
		{requested: sandboxAuto, container: "podman", want: sandboxOff},
		{requested: sandboxOn, container: "podman", want: sandboxOn},
		{requested: sandboxOff, container: "podman", want: sandboxOff},
	} {
		got := effectiveSandboxPolicy(test.requested, test.container)
		if got != test.want {
			t.Fatalf("effectiveSandboxPolicy(%q, %q) = %q, want %q", test.requested, test.container, got, test.want)
		}
	}
}

func TestDetectContainerWithStrongSignals(t *testing.T) {
	for _, test := range []struct {
		name       string
		files      map[string]string
		detectVirt string
		wantID     string
	}{
		{
			name:   "podman marker",
			files:  map[string]string{"/run/.containerenv": ""},
			wantID: "podman",
		},
		{
			name:   "docker marker",
			files:  map[string]string{"/.dockerenv": ""},
			wantID: "docker",
		},
		{
			name:   "systemd lxc marker",
			files:  map[string]string{"/run/systemd/container": "lxc\n"},
			wantID: "lxc",
		},
		{
			name:   "pid one environment",
			files:  map[string]string{"/proc/1/environ": "PATH=/usr/bin\x00container=systemd-nspawn\x00"},
			wantID: "systemd-nspawn",
		},
		{
			name:       "systemd detect virt fallback",
			detectVirt: "podman\n",
			wantID:     "podman",
		},
		{
			name:       "wsl is not trusted isolation",
			detectVirt: "wsl\n",
		},
		{
			name: "unknown environment",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			probe := containerProbe{
				exists: func(path string) bool {
					_, ok := test.files[path]
					return ok
				},
				readFile: func(path string) ([]byte, error) {
					value, ok := test.files[path]
					if !ok {
						return nil, errors.New("not found")
					}
					return []byte(value), nil
				},
				detectVirt: func() (string, error) {
					if test.detectVirt == "" {
						return "", fmt.Errorf("not detected")
					}
					return test.detectVirt, nil
				},
			}
			got := detectContainerWith(probe)
			if got != test.wantID {
				t.Fatalf("detectContainerWith() = %q, want %q", got, test.wantID)
			}
		})
	}
}

func TestSecuritySummaryIncludesAutoContainer(t *testing.T) {
	state := SecurityState{Container: "podman"}
	if got, want := state.Compact(), "sandbox: off (podman) · network: open"; got != want {
		t.Fatalf("Compact() = %q, want %q", got, want)
	}
}

func TestRequireEffectiveSandbox(t *testing.T) {
	backend := toolruntime.SandboxBackend()
	if backend == "" {
		// Nothing to fail to obtain, so only the policy-driven answers remain:
		// off proceeds, auto proceeds, on refuses.
		for _, tt := range []struct {
			policy  string
			wantErr bool
		}{
			{sandboxOff, false},
			{sandboxAuto, false},
			{sandboxOn, true},
		} {
			cfg := Config{SandboxPolicy: tt.policy, Security: SecurityState{Probe: "no platform sandbox is available"}}
			if err := requireEffectiveSandbox(cfg); (err != nil) != tt.wantErr {
				t.Fatalf("policy %s: error = %v, want error = %v", tt.policy, err, tt.wantErr)
			}
		}
		return
	}

	t.Run("a sandbox that held lets any policy through", func(t *testing.T) {
		for _, policy := range []string{sandboxAuto, sandboxOn, sandboxOff} {
			cfg := Config{SandboxPolicy: policy, Security: SecurityState{Backend: backend}}
			if err := requireEffectiveSandbox(cfg); err != nil {
				t.Fatalf("policy %s: error = %v, want none", policy, err)
			}
		}
	})

	t.Run("off is a choice and is honoured", func(t *testing.T) {
		cfg := Config{SandboxPolicy: sandboxOff, Security: SecurityState{EffectivePolicy: sandboxOff}}
		if err := requireEffectiveSandbox(cfg); err != nil {
			t.Fatalf("error = %v, want none", err)
		}
	})

	t.Run("auto stops where the backend exists and did not hold", func(t *testing.T) {
		cfg := Config{SandboxPolicy: sandboxAuto, Security: SecurityState{EffectivePolicy: sandboxOff, Probe: "sandbox probe path remained readable"}}
		err := requireEffectiveSandbox(cfg)
		if err == nil {
			t.Fatal("error = nil, want a refusal to run unfenced")
		}
		// Whoever hits this needs both halves: what went wrong, and how to say
		// they meant it.
		if !strings.Contains(err.Error(), "probe path remained readable") || !strings.Contains(err.Error(), "--sandbox off") {
			t.Fatalf("error = %q, want the probe reason and the way out", err)
		}
	})

	t.Run("auto continues where a trusted container was detected", func(t *testing.T) {
		cfg := Config{SandboxPolicy: sandboxAuto, Security: SecurityState{EffectivePolicy: sandboxOff, Container: "podman"}}
		if err := requireEffectiveSandbox(cfg); err != nil {
			t.Fatalf("error = %v, want none: the outer isolation is the isolation", err)
		}
	})

	t.Run("on refuses even inside a container", func(t *testing.T) {
		cfg := Config{SandboxPolicy: sandboxOn, Security: SecurityState{EffectivePolicy: sandboxOn, Probe: "probe failed"}}
		err := requireEffectiveSandbox(cfg)
		if err == nil || !strings.HasPrefix(err.Error(), "required sandbox probe failed: ") {
			t.Fatalf("error = %v, want the unchanged on message", err)
		}
	})
}

func TestSandboxUnavailableNotice(t *testing.T) {
	for _, tt := range []struct {
		name   string
		cfg    Config
		expect string
	}{
		{
			name:   "scripted run that lost its fence says so",
			cfg:    Config{PrintMode: true, SandboxPolicy: sandboxAuto, Security: SecurityState{EffectivePolicy: sandboxOff, Probe: "sandbox probe path remained readable"}},
			expect: "sandbox unavailable, continuing without one: sandbox probe path remained readable",
		},
		{
			name:   "a trusted container is still a run without a fence",
			cfg:    Config{PrintMode: true, SandboxPolicy: sandboxAuto, Security: SecurityState{EffectivePolicy: sandboxOff, Container: "podman"}},
			expect: "sandbox off, trusting the podman container instead",
		},
		{
			name: "scripted run with a working fence says nothing",
			cfg:  Config{PrintMode: true, SandboxPolicy: sandboxAuto, Security: SecurityState{EffectivePolicy: sandboxAuto, Backend: "landlock"}},
		},
		{
			name: "off was asked for, so it is not news",
			cfg:  Config{PrintMode: true, SandboxPolicy: sandboxOff, Security: SecurityState{EffectivePolicy: sandboxOff}},
		},
		{
			name: "interactive already shows it in the startup line",
			cfg:  Config{PrintMode: false, SandboxPolicy: sandboxAuto, Security: SecurityState{EffectivePolicy: sandboxOff, Probe: "sandbox probe path remained readable"}},
		},
		{
			name:   "a probe with no detail still reports the fallback",
			cfg:    Config{PrintMode: true, SandboxPolicy: sandboxAuto, Security: SecurityState{EffectivePolicy: sandboxOff}},
			expect: "sandbox unavailable, continuing without one",
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			got := sandboxUnavailableNotice(tt.cfg)
			if got != tt.expect {
				t.Fatalf("sandboxUnavailableNotice() = %q, want %q", got, tt.expect)
			}
		})
	}
}

// on is handled before this notice and fails the run outright, so the two paths
// must not both fire for the same condition.
func TestSandboxUnavailableNoticeStaysQuietForOn(t *testing.T) {
	cfg := Config{PrintMode: true, SandboxPolicy: sandboxOn, Security: SecurityState{EffectivePolicy: sandboxOn, Probe: "probe failed"}}
	if got := sandboxUnavailableNotice(cfg); got != "" {
		t.Fatalf("sandboxUnavailableNotice() = %q, want silence", got)
	}
}

func TestSandboxUnavailableNoticeNamesTheCause(t *testing.T) {
	cfg := Config{PrintMode: true, SandboxPolicy: sandboxAuto, Security: SecurityState{EffectivePolicy: sandboxOff, Probe: "probe setup failed: permission denied"}}
	if got := sandboxUnavailableNotice(cfg); !strings.Contains(got, "permission denied") {
		t.Fatalf("notice = %q, want it to carry the probe's reason", got)
	}
}

func TestGrantedDirContainingPicksTheInnermost(t *testing.T) {
	for _, tt := range []struct {
		name    string
		path    string
		granted []string
		want    string
	}{
		{
			name:    "the workspace is the usual culprit",
			path:    "/work/repo/.cy",
			granted: []string{"/work/repo", "/usr"},
			want:    "/work/repo",
		},
		{
			name:    "the innermost grant wins over an outer one",
			path:    "/work/repo/state/.cy",
			granted: []string{"/work", "/work/repo", "/work/repo/state"},
			want:    "/work/repo/state",
		},
		{
			name:    "a path outside every grant has no cause to name",
			path:    "/home/user/.cy",
			granted: []string{"/work/repo", "/usr"},
		},
		{
			name:    "a sibling with a shared prefix is not inside",
			path:    "/work/repository/.cy",
			granted: []string{"/work/repo"},
		},
		{
			name:    "the grant itself counts as containing",
			path:    "/work/repo",
			granted: []string{"/work/repo"},
			want:    "/work/repo",
		},
		{
			name:    "nothing is granted where there is no sandbox",
			path:    "/work/repo/.cy",
			granted: nil,
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if got := grantedDirContaining(tt.path, tt.granted); got != tt.want {
				t.Fatalf("grantedDirContaining(%q, %v) = %q, want %q", tt.path, tt.granted, got, tt.want)
			}
		})
	}
}

// The workspace is granted, so a home inside it is the one case the message
// has to name: it is both the most common way to get here and the only one the
// reader can fix from the command line.
func TestProbeReadableReasonNamesTheGrantedDirectory(t *testing.T) {
	workspace := t.TempDir()
	home := filepath.Join(workspace, ".cy")
	if err := os.MkdirAll(home, 0o700); err != nil {
		t.Fatal(err)
	}
	reason := probeReadableReason(home, workspace, filepath.Join(home, "tool-cache"))
	for _, want := range []string{"sandbox probe path remained readable", canonicalPath(workspace), "--home"} {
		if !strings.Contains(reason, want) {
			t.Fatalf("reason = %q, want it to mention %q", reason, want)
		}
	}
}

// A home outside every grant means the probe failed for some other reason, and
// guessing at one would send the reader after the wrong thing. Report the
// symptom alone.
func TestProbeReadableReasonKeepsTheSymptomWhenTheCauseIsElsewhere(t *testing.T) {
	workspace := t.TempDir()
	home := t.TempDir()
	reason := probeReadableReason(home, workspace, filepath.Join(home, "tool-cache"))
	if reason != "sandbox probe path remained readable" {
		t.Fatalf("reason = %q, want the symptom alone", reason)
	}
}

func TestUnavailableAutoSandboxFallsBackOff(t *testing.T) {
	state := unavailableSandbox(SecurityState{EffectivePolicy: sandboxAuto}, "probe failed")
	if state.EffectivePolicy != sandboxOff || state.Probe != "probe failed" {
		t.Fatalf("unavailableSandbox(auto) = %#v", state)
	}

	state = unavailableSandbox(SecurityState{EffectivePolicy: sandboxOn}, "probe failed")
	if state.EffectivePolicy != sandboxOn {
		t.Fatalf("unavailableSandbox(on) = %#v", state)
	}
}
