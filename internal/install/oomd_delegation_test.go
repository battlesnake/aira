package install

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"os/user"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

// RED against the pre-migration installer: requireUserMemoryDelegation aborts
// before any managed user unit is installed. The expected fresh-machine state
// is transitional, not fatal: install all user units and report it honestly.
func TestInstallWithoutDelegationStillInstallsUserUnitsAndReportsPending(t *testing.T) {
	d, state := newFakeInstall(t)
	controllers := filepath.Join(d.cgroupRoot, "user.slice", "user-"+itoa(state.uid)+".slice", "user@"+itoa(state.uid)+".service", "cgroup.controllers")
	state.cgroup[controllers] = []byte("pids\n")
	installFakeDelegationDropin(t, &d, state.home)

	err := runInstall(d, installOpts{memoryMax: "16G", watchdog: "observe", watchdogInterval: 2 * time.Second})
	if err != nil {
		t.Fatalf("not-delegated transition must succeed, got %v", err)
	}
	for _, unit := range []string{defaultSliceUnit, defaultAnchorUnit, defaultDaemonUnit} {
		if _, statErr := os.Stat(filepath.Join(state.unitDir(), unit)); statErr != nil {
			t.Errorf("user unit %s was not installed: %v", unit, statErr)
		}
	}
	logs := strings.Join(state.logs, "\n")
	if strings.Count(logs, "warning: run 'sudo aira install' to apply the /etc oomd + delegation + sysctl drop-ins, then re-login") != 1 {
		t.Fatalf("expected exactly one oomd/delegation warning: %q", logs)
	}
	if !strings.Contains(logs, "pending re-login") {
		t.Fatalf("summary did not report pending re-login: %q", logs)
	}
	if strings.Contains(logs, "memory delegated") || strings.Contains(logs, "memory delegation: active") {
		t.Fatalf("not-delegated summary claimed active enforcement: %q", logs)
	}
	if errors.Is(err, os.ErrNotExist) {
		t.Fatalf("missing controller leaked as an error: %v", err)
	}
}

func TestOomdDropinsRenderToUniquePathsWithExactLessonContent(t *testing.T) {
	rendered, err := renderSystemDropins("/etc", 1000)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{
		"/etc/systemd/oomd.conf.d/aira-oomd.conf": `# aira-managed: aira-oomd.conf
# Tighten systemd-oomd thresholds so it reacts before kernel OOM does.
# See https://www.freedesktop.org/software/systemd/man/oomd.conf.html
[OOM]
# React to lower pressure (default 60%) so we catch growth earlier.
DefaultMemoryPressureLimit=40%
# React faster (default 20s) — rapid anonymous allocation can exhaust
# RAM before a 20s window completes.
DefaultMemoryPressureDurationSec=10s
# Swap-fill-based killing is deliberately disabled (100% = never fires).
# Memory pressure (PSI) above already detects real thrashing; swap fill
# alone just means anonymous pages were evicted to make room for cache,
# which is healthy. The previous SwapUsedLimit=50% killed the desktop
# session on 2026-05-28 when system services held 86% of swap and the
# user session held 14% — user-1000.slice was the only swap-kill-eligible
# target, so it took the bullet regardless of who actually filled swap.
# Do not re-enable swap-based killing without first making system.slice
# swap-kill-eligible too.
SwapUsedLimit=100%
`,
		"/etc/systemd/system/user@.service.d/50-aira-oomd.conf": `# aira-managed: 50-aira-oomd.conf
# Override Ubuntu's hardcoded 50% in 10-oomd-user-service-defaults.conf so
# the global DefaultMemoryPressureLimit (40%) from oomd.conf.d/ applies.
# (50-prefix sorts after 10- so this wins on merge.)
[Service]
ManagedOOMMemoryPressureLimit=40%
`,
		"/etc/systemd/system/user-1000.slice.d/50-aira-oomd.conf": `# aira-managed: 50-aira-oomd.conf
# Extend systemd-oomd coverage from user@1000.service up to the whole
# user-1000.slice — catches pressure from anywhere in the user's tree,
# including login sessions outside user@N.service.
#
# Swap-based killing intentionally NOT enabled here; see the comment in
# /etc/systemd/oomd.conf.d/aira-oomd.conf for why.
[Slice]
ManagedOOMMemoryPressure=kill
`,
		"/etc/systemd/user/session.slice.d/50-aira-oomd-protect.conf": `# aira-managed: 50-aira-oomd-protect.conf
# Tell systemd-oomd to avoid killing session.slice (desktop services:
# dbus, pipewire, xdg-permission-store, gpg-agent, etc.) when picking a
# victim under user@N.service. Heavy work in tmux-spawn-*.scope or
# aira.slice will be preferred.
[Slice]
ManagedOOMPreference=avoid
`,
		"/etc/systemd/system/user@.service.d/10-aira-delegate.conf": `# aira-managed: 10-aira-delegate.conf
# Installed by ` + "`aira install`" + ` (sudo) so the cgroup-v2 MemoryMax cap on
# aira.slice is actually enforced. Without memory delegation to the user
# manager, systemd accepts MemoryMax on a user slice but the kernel ignores it.
# Delegate= applies at the next user@.service (re)start, so re-login or reboot
# after installation before expecting the cap to be enforced.
[Service]
Delegate=yes
`,
		"/etc/sysctl.d/60-inotify-aira.conf": `# aira-managed: 60-inotify-aira.conf
# Raise the per-user inotify instance limit. Every aira confine/aira run
# supervisor opens one inotify instance for scope-membership watching
# (scopeMembershipEvents in internal/runner), and this is a per-user, not
# per-process, kernel limit shared by everything else the user runs too
# (editors, file watchers, other tools). A machine running many concurrent
# AI agent sessions can genuinely exhaust the kernel default of 128,
# surfacing as deterministic inotify_init1 EMFILE failures. See AIRA-96.
fs.inotify.max_user_instances = 4096
`,
	}
	if len(rendered) != len(want) {
		t.Fatalf("rendered %d drop-ins, want %d", len(rendered), len(want))
	}
	for _, dropin := range rendered {
		expected, ok := want[dropin.dst]
		if !ok {
			t.Errorf("unexpected destination %s", dropin.dst)
			continue
		}
		if string(dropin.content) != expected {
			t.Errorf("%s content mismatch\n--- got ---\n%s\n--- want ---\n%s", dropin.dst, dropin.content, expected)
		}
	}
}

func TestOomdDesktopSafetyInvariantsArePinned(t *testing.T) {
	rendered, err := renderSystemDropins("/etc", 1000)
	if err != nil {
		t.Fatal(err)
	}
	byPath := map[string][]byte{}
	for _, dropin := range rendered {
		byPath[dropin.dst] = dropin.content
	}
	if !bytes.Contains(byPath["/etc/systemd/oomd.conf.d/aira-oomd.conf"], []byte("SwapUsedLimit=100%")) {
		t.Fatal("desktop safety regression: oomd global drop-in no longer disables swap-fill killing")
	}
	if !bytes.Contains(byPath["/etc/systemd/user/session.slice.d/50-aira-oomd-protect.conf"], []byte("ManagedOOMPreference=avoid")) {
		t.Fatal("desktop safety regression: session.slice is no longer protected")
	}
}

func TestRootInstallWritesAndActivatesInotifySysctlDropin(t *testing.T) {
	d, state := newFakeRootInstall(t)
	if err := runInstall(d, installOpts{memoryMax: "16G"}); err != nil {
		t.Fatal(err)
	}
	dst := filepath.Join(d.etcRoot, "sysctl.d/60-inotify-aira.conf")
	content, err := os.ReadFile(dst)
	if err != nil || !bytes.Contains(content, []byte("fs.inotify.max_user_instances = 4096")) {
		t.Fatalf("inotify sysctl drop-in not written: err=%v content=%q", err, content)
	}
	if !containsArgv(state.commands, []string{"sysctl", "--system"}) {
		t.Fatalf("first install did not activate the sysctl drop-in: %q", state.commands)
	}
}

// TestSystemDropinsCurrentRequiresLiveSysctlMatch pins the health-check
// requirement from AIRA-96: a sysctl.d file that matches on disk is not
// "current" unless the kernel has actually picked up its value (sysctl.d is
// only applied at boot or by an explicit sysctl --system /
// systemctl restart systemd-sysctl, so a matching file can still be inert).
func TestSystemDropinsCurrentRequiresLiveSysctlMatch(t *testing.T) {
	d, state := newFakeInstall(t)
	installFakeSystemDropins(t, &d, state.home)

	priorReadFile := d.readFile
	d.readFile = func(path string) ([]byte, error) {
		if path == procInotifyMaxUserInstances {
			return []byte("4096\n"), nil
		}
		return priorReadFile(path)
	}
	if !systemDropinsCurrent(d, state.uid) {
		t.Fatal("expected drop-ins current when the on-disk file and live kernel value both match")
	}

	d.readFile = func(path string) ([]byte, error) {
		if path == procInotifyMaxUserInstances {
			return []byte("128\n"), nil
		}
		return priorReadFile(path)
	}
	if systemDropinsCurrent(d, state.uid) {
		t.Fatal("drop-ins reported current from a written file alone, without the kernel actually applying it")
	}
}

func TestRootInstallAlwaysOwnsDelegationEvenWhenAlreadyDelegated(t *testing.T) {
	d, state := newFakeRootInstall(t)
	controllers := filepath.Join(d.cgroupRoot, "user.slice", "user-1234.slice", "user@1234.service", "cgroup.controllers")
	state.files[controllers] = []byte("cpu memory pids\n")
	if err := runInstall(d, installOpts{memoryMax: "16G"}); err != nil {
		t.Fatal(err)
	}
	dst := filepath.Join(d.etcRoot, "systemd/system/user@.service.d/10-aira-delegate.conf")
	content, err := os.ReadFile(dst)
	if err != nil || !bytes.Contains(content, []byte("Delegate=yes")) {
		t.Fatalf("AIRA did not take ownership of delegation: err=%v content=%q", err, content)
	}
	if state.reexec == nil {
		t.Fatal("root phase did not re-exec the user phase")
	}
}

func TestRootInstallPublishesAllExactDropinsAndLeavesAgentmuxFilesAlone(t *testing.T) {
	d, _ := newFakeRootInstall(t)
	foreign := map[string][]byte{
		filepath.Join(d.etcRoot, "systemd/oomd.conf.d/whale-overrides.conf"):                 []byte("foreign oomd\n"),
		filepath.Join(d.etcRoot, "systemd/system/user@.service.d/10-agentmux-delegate.conf"): []byte("foreign delegate\n"),
		filepath.Join(d.etcRoot, "systemd/system/user-1234.slice.d/oomd.conf"):               []byte("foreign slice\n"),
		filepath.Join(d.etcRoot, "systemd/user/session.slice.d/oomd-protect.conf"):           []byte("foreign protect\n"),
		filepath.Join(d.etcRoot, "systemd/system/user@.service.d/50-whale.conf"):             []byte("foreign service\n"),
	}
	for path, content := range foreign {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, content, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := runInstall(d, installOpts{memoryMax: "16G"}); err != nil {
		t.Fatal(err)
	}
	rendered, err := renderSystemDropins(d.etcRoot, 1234)
	if err != nil {
		t.Fatal(err)
	}
	for _, dropin := range rendered {
		got, readErr := os.ReadFile(dropin.dst)
		if readErr != nil || !bytes.Equal(got, dropin.content) {
			t.Errorf("installed %s mismatch: err=%v got=%q want=%q", dropin.dst, readErr, got, dropin.content)
		}
	}
	for path, want := range foreign {
		got, readErr := os.ReadFile(path)
		if readErr != nil || !bytes.Equal(got, want) {
			t.Errorf("foreign agentmux file changed: %s err=%v got=%q want=%q", path, readErr, got, want)
		}
	}
}

func TestMemoryEnforcementTriStateRequiresControllerAndCapReadback(t *testing.T) {
	t.Run("active", func(t *testing.T) {
		d, state := newFakeInstall(t)
		if err := runInstall(d, installOpts{memoryMax: "16G"}); err != nil {
			t.Fatal(err)
		}
		readback := false
		prior := d.readFile
		d.readFile = func(path string) ([]byte, error) {
			if strings.HasSuffix(path, "/aira.slice/memory.max") {
				readback = true
			}
			return prior(path)
		}
		if got := memoryEnforcementState(d, state.uid, "16G"); got != enforcementActive {
			t.Fatalf("state=%q, want active", got)
		}
		if !readback {
			t.Fatal("active was reported without asserting aira.slice/memory.max")
		}
	})

	t.Run("pending", func(t *testing.T) {
		d, state := newFakeInstall(t)
		controllers := filepath.Join(d.cgroupRoot, "user.slice", "user-"+itoa(state.uid)+".slice", "user@"+itoa(state.uid)+".service", "cgroup.controllers")
		state.cgroup[controllers] = []byte("pids\n")
		installFakeDelegationDropin(t, &d, state.home)
		if got := memoryEnforcementState(d, state.uid, "16G"); got != enforcementPending {
			t.Fatalf("state=%q, want pending re-login", got)
		}
	})

	t.Run("not installed", func(t *testing.T) {
		d, state := newFakeInstall(t)
		controllers := filepath.Join(d.cgroupRoot, "user.slice", "user-"+itoa(state.uid)+".slice", "user@"+itoa(state.uid)+".service", "cgroup.controllers")
		state.cgroup[controllers] = []byte("pids\n")
		d.etcRoot = filepath.Join(state.home, "empty-etc")
		if got := memoryEnforcementState(d, state.uid, "16G"); got != enforcementNotInstalled {
			t.Fatalf("state=%q, want not installed", got)
		}
	})
}

func TestStatusReportsHonestMemoryEnforcementTriState(t *testing.T) {
	d, state := newFakeInstall(t)
	if err := runInstall(d, installOpts{memoryMax: "16G"}); err != nil {
		t.Fatal(err)
	}
	state.logs = nil
	if err := runStatus(d); err != nil {
		t.Fatal(err)
	}
	if logs := strings.Join(state.logs, "\n"); !strings.Contains(logs, "memory delegation: active") {
		t.Fatalf("active status missing: %q", logs)
	}

	controllers := filepath.Join(d.cgroupRoot, "user.slice", "user-"+itoa(state.uid)+".slice", "user@"+itoa(state.uid)+".service", "cgroup.controllers")
	state.cgroup[controllers] = []byte("pids\n")
	installFakeDelegationDropin(t, &d, state.home)
	state.logs = nil
	if err := runStatus(d); err != nil {
		t.Fatal(err)
	}
	logs := strings.Join(state.logs, "\n")
	if !strings.Contains(logs, "memory delegation: pending re-login") || strings.Contains(logs, "memory delegation: active") {
		t.Fatalf("pending status was dishonest: %q", logs)
	}

	d.etcRoot = filepath.Join(state.home, "no-dropins")
	state.logs = nil
	if err := runStatus(d); err != nil {
		t.Fatal(err)
	}
	if logs := strings.Join(state.logs, "\n"); !strings.Contains(logs, "memory delegation: not installed") || strings.Contains(logs, "memory delegation: active") {
		t.Fatalf("not-installed status was dishonest: %q", logs)
	}
}

func TestInactiveSystemdOomdPreventsHealthySummaryAndStatus(t *testing.T) {
	d, state := newFakeInstall(t)
	if err := runInstall(d, installOpts{memoryMax: "16G"}); err != nil {
		t.Fatal(err)
	}
	installFakeSystemDropins(t, &d, state.home)
	priorRun := d.run
	d.run = func(argv []string, stdin []byte) ([]byte, error) {
		if reflect.DeepEqual(argv, []string{"systemctl", "is-active", "systemd-oomd"}) {
			return []byte("inactive\n"), errors.New("exit status 3")
		}
		return priorRun(argv, stdin)
	}

	state.logs = nil
	if err := runInstall(d, installOpts{memoryMax: "16G"}); err != nil {
		t.Fatal(err)
	}
	if logs := strings.Join(state.logs, "\n"); !strings.Contains(logs, "warning: run 'sudo aira install'") {
		t.Fatalf("install summary suppressed the oomd warning while systemd-oomd was inactive: %q", logs)
	}

	state.logs = nil
	if err := runStatus(d); err != nil {
		t.Fatal(err)
	}
	logs := strings.Join(state.logs, "\n")
	if strings.Contains(logs, "oomd + delegation + sysctl drop-ins: up to date") {
		t.Fatalf("status reported inactive systemd-oomd as healthy: %q", logs)
	}
}

func TestRootInstallFailFastBeforeEtcWrites(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*testing.T, *installDeps, *fakeRootInstallState)
	}{
		{name: "ambiguous root without sudo identity", mutate: func(_ *testing.T, d *installDeps, _ *fakeRootInstallState) {
			d.getenv = func(string) string { return "" }
		}},
		{name: "missing session", mutate: func(_ *testing.T, d *installDeps, _ *fakeRootInstallState) {
			prior := d.stat
			d.stat = func(path string) (os.FileInfo, error) {
				if path == "/run/user/1234" {
					return nil, os.ErrNotExist
				}
				return prior(path)
			}
		}},
		{name: "foreign session", mutate: func(_ *testing.T, d *installDeps, _ *fakeRootInstallState) {
			prior := d.stat
			d.stat = func(path string) (os.FileInfo, error) {
				if path == "/run/user/1234" {
					return staticFileInfo{name: "1234", mode: os.ModeDir | 0o700, uid: 999, gid: 999}, nil
				}
				return prior(path)
			}
		}},
		{name: "unreadable executable", mutate: func(_ *testing.T, d *installDeps, _ *fakeRootInstallState) {
			prior := d.stat
			d.stat = func(path string) (os.FileInfo, error) {
				if path == "/opt/aira" {
					return staticFileInfo{name: "aira", mode: 0o700, uid: 0, gid: 0}, nil
				}
				return prior(path)
			}
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			d, state := newFakeRootInstall(t)
			test.mutate(t, &d, state)
			err := runInstall(d, installOpts{memoryMax: "16G"})
			if err == nil {
				t.Fatal("expected fail-fast error")
			}
			if state.writes != 0 || state.reexec != nil {
				t.Fatalf("fail-fast path mutated or re-execed: writes=%d reexec=%+v", state.writes, state.reexec)
			}
			if _, statErr := os.Stat(d.etcRoot); !errors.Is(statErr, os.ErrNotExist) {
				t.Fatalf("fail-fast path touched /etc staging root: %v", statErr)
			}
		})
	}
}

func TestRootActivationFailureIsNonZeroAfterUserPhase(t *testing.T) {
	for _, command := range []string{"systemctl daemon-reload", "systemctl restart systemd-oomd", "sysctl --system"} {
		t.Run(command, func(t *testing.T) {
			d, state := newFakeRootInstall(t)
			state.failCommand = command
			err := runInstall(d, installOpts{memoryMax: "16G"})
			if err == nil || !strings.Contains(err.Error(), "partial /etc") {
				t.Fatalf("activation failure was masked: %v", err)
			}
			if state.reexec == nil {
				t.Fatal("user phase did not run before activation failure surfaced")
			}
		})
	}
}

func TestRootInstallRetriesOomdRestartAfterPriorActivationFailure(t *testing.T) {
	d, state := newFakeRootInstall(t)
	state.failCommand = "systemctl restart systemd-oomd"
	err := runInstall(d, installOpts{memoryMax: "16G"})
	if err == nil || !strings.Contains(err.Error(), "partial /etc") {
		t.Fatalf("first activation failure was masked: %v", err)
	}

	state.failCommand = ""
	state.commands = nil
	if err := runInstall(d, installOpts{memoryMax: "16G"}); err != nil {
		t.Fatalf("convergence run failed: %v", err)
	}
	if !containsArgv(state.commands, []string{"systemctl", "restart", "systemd-oomd"}) {
		t.Fatalf("convergence run did not retry the unconfirmed oomd activation: %q", state.commands)
	}
}

// TestRootInstallRetriesSysctlActivationAfterPriorActivationFailure pins the
// AIRA-96 self-healing requirement: once the sysctl.d file is written, it
// stops being "stale" on later runs, so activation must keep retrying off the
// kernel's live value (not just the file's staleness) until sysctl --system
// actually succeeds — mirroring the equivalent oomd-restart retry above.
func TestRootInstallRetriesSysctlActivationAfterPriorActivationFailure(t *testing.T) {
	d, state := newFakeRootInstall(t)
	state.failCommand = "sysctl --system"
	err := runInstall(d, installOpts{memoryMax: "16G"})
	if err == nil || !strings.Contains(err.Error(), "partial /etc") {
		t.Fatalf("first activation failure was masked: %v", err)
	}
	if state.sysctlLive {
		t.Fatal("kernel value reported live despite the injected sysctl --system failure")
	}

	state.failCommand = ""
	state.commands = nil
	if err := runInstall(d, installOpts{memoryMax: "16G"}); err != nil {
		t.Fatalf("convergence run failed: %v", err)
	}
	if !containsArgv(state.commands, []string{"sysctl", "--system"}) {
		t.Fatalf("convergence run did not retry the unconfirmed sysctl activation (file was already written, so only the live-value check can catch this): %q", state.commands)
	}
	if !state.sysctlLive {
		t.Fatal("convergence run did not actually apply the sysctl value")
	}
}

// TestRootInstallReportsSysctlOverrideAsActivationFailure pins the requirement
// that a zero exit from `sysctl --system` is not itself proof the wanted
// value took effect: sysctl.d merge order lets a lexically-later file (or
// /etc/sysctl.conf, applied last of all) silently override ours, so the
// kernel must be read back and a mismatch reported as a real failure rather
// than a fake pass.
func TestRootInstallReportsSysctlOverrideAsActivationFailure(t *testing.T) {
	d, state := newFakeRootInstall(t)
	priorRun := d.run
	d.run = func(argv []string, stdin []byte) ([]byte, error) {
		if strings.Join(argv, " ") == "sysctl --system" {
			// Exits 0 but deliberately never flips state.sysctlLive, modeling
			// an apply immediately overridden by something else.
			return nil, nil
		}
		return priorRun(argv, stdin)
	}
	err := runInstall(d, installOpts{memoryMax: "16G"})
	if err == nil || !strings.Contains(err.Error(), "partial /etc") || !strings.Contains(err.Error(), "not live after activation") {
		t.Fatalf("a silently-overridden sysctl value was not reported as an activation failure: %v", err)
	}
	dst := filepath.Join(d.etcRoot, "sysctl.d/60-inotify-aira.conf")
	if _, statErr := os.Stat(dst); statErr != nil {
		t.Fatalf("drop-in file should still be written even though the kernel value stayed overridden: %v", statErr)
	}
	if state.reexec == nil {
		t.Fatal("user phase did not run after the sysctl activation-failure surfaced")
	}
}

func TestRootWriteFailureIsExplicitPartialAndStillRunsUserPhase(t *testing.T) {
	d, state := newFakeRootInstall(t)
	d.writeFD = func(int, []byte) error { return errors.New("injected disk full") }
	err := runInstall(d, installOpts{memoryMax: "16G"})
	if err == nil || !strings.Contains(err.Error(), "partial /etc") || !strings.Contains(err.Error(), "disk full") {
		t.Fatalf("write failure was masked: %v", err)
	}
	if state.reexec == nil {
		t.Fatal("user phase did not run after partial /etc write failure")
	}
}

func TestRootPreflightsEveryOwnedTargetBeforeFirstPublish(t *testing.T) {
	d, state := newFakeRootInstall(t)
	// Derived, not hardcoded: whichever drop-in systemDropins() renders last is
	// the one that actually exercises "every target is preflighted before any
	// publish" (an earlier target's foreign content would abort the loop
	// before ever reaching this one, proving nothing about full coverage).
	rendered, err := renderSystemDropins(d.etcRoot, 1234)
	if err != nil {
		t.Fatal(err)
	}
	last := rendered[len(rendered)-1].dst
	if err := os.MkdirAll(filepath.Dir(last), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(last, []byte("foreign, marker-less content\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	err = runInstall(d, installOpts{memoryMax: "16G"})
	if err == nil || !strings.Contains(err.Error(), "marker-less") {
		t.Fatalf("foreign owned-name target was not refused: %v", err)
	}
	if state.writes != 0 {
		t.Fatalf("earlier drop-ins were published before the final target was validated: writes=%d", state.writes)
	}
	if state.reexec == nil {
		t.Fatal("user phase did not run after /etc preflight refusal")
	}
}

func TestRootInstallRefusesSymlinkedEtcDropinTarget(t *testing.T) {
	d, state := newFakeRootInstall(t)
	dst := filepath.Join(d.etcRoot, "systemd/oomd.conf.d/aira-oomd.conf")
	victim := filepath.Join(t.TempDir(), "victim")
	wantVictim := []byte("# aira-managed: aira-oomd.conf\nvictim must remain unchanged\n")
	if err := os.WriteFile(victim, wantVictim, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(victim, dst); err != nil {
		t.Fatal(err)
	}

	err := runInstall(d, installOpts{memoryMax: "16G"})
	if err == nil {
		t.Fatal("install accepted a symlinked /etc drop-in target")
	}
	if state.writes != 0 {
		t.Fatalf("install published files after symlink preflight refusal: writes=%d", state.writes)
	}
	gotVictim, readErr := os.ReadFile(victim)
	if readErr != nil {
		t.Fatal(readErr)
	}
	if !bytes.Equal(gotVictim, wantVictim) {
		t.Fatalf("install wrote through symlink: got %q, want %q", gotVictim, wantVictim)
	}
}

func TestRootDropinInstallIsIdempotent(t *testing.T) {
	d, state := newFakeRootInstall(t)
	if err := runInstall(d, installOpts{memoryMax: "16G"}); err != nil {
		t.Fatal(err)
	}
	writes := state.writes
	state.commands = nil
	if err := runInstall(d, installOpts{memoryMax: "16G"}); err != nil {
		t.Fatal(err)
	}
	if state.writes != writes {
		t.Fatalf("second root run rewrote current drop-ins: %d -> %d", writes, state.writes)
	}
	for _, command := range state.commands {
		joined := strings.Join(command, " ")
		if joined == "systemctl daemon-reload" || joined == "systemctl restart systemd-oomd" || joined == "sysctl --system" {
			t.Fatalf("second root run activated unchanged drop-ins: %q", state.commands)
		}
	}
}

func TestRootDropinInstallRepairsNonCanonicalMode(t *testing.T) {
	d, state := newFakeRootInstall(t)
	if err := runInstall(d, installOpts{memoryMax: "16G"}); err != nil {
		t.Fatal(err)
	}
	dst := filepath.Join(d.etcRoot, "systemd/system/user@.service.d/10-aira-delegate.conf")
	if err := os.Chmod(dst, 0o600); err != nil {
		t.Fatal(err)
	}
	writes := state.writes
	if err := runInstall(d, installOpts{memoryMax: "16G"}); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(dst)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o644 {
		t.Fatalf("mode was not repaired: mode=%v", info.Mode().Perm())
	}
	if state.writes != writes+1 {
		t.Fatalf("mode repair rewrote %d files, want exactly one", state.writes-writes)
	}
}

func TestRootReexecDropsCredentialsAtomicallyAndSanitizesEnvironment(t *testing.T) {
	d, state := newFakeRootInstall(t)
	if err := runInstall(d, installOpts{memoryMax: "20G", memoryHigh: "17G", watchdog: "enforce", watchdogInterval: 5 * time.Second, allowOvercommit: true, dryRun: true}); err != nil {
		t.Fatal(err)
	}
	request := state.reexec
	if request == nil || request.credential == nil {
		t.Fatalf("missing credentialed re-exec: %+v", request)
	}
	if request.credential.Uid != 1234 || request.credential.Gid != 1234 || !reflect.DeepEqual(request.credential.Groups, []uint32{1234, 55}) {
		t.Fatalf("credential=%+v", request.credential)
	}
	wantArgs := []string{"install", "--memory-max", "20G", "--memory-high", "17G", "--watchdog", "enforce", "--watchdog-interval", "5s", "--allow-overcommit", "--dry-run"}
	if !reflect.DeepEqual(request.args, wantArgs) {
		t.Errorf("re-exec argv=%q, want exact ordered argv %q", request.args, wantArgs)
	}
	joinedEnv := strings.Join(request.env, "\n")
	for _, want := range []string{"HOME=/home/alice", "XDG_RUNTIME_DIR=/run/user/1234", "DBUS_SESSION_BUS_ADDRESS=unix:path=/run/user/1234/bus", "AIRA_INSTALL_REEXEC=1"} {
		if !strings.Contains(joinedEnv, want) {
			t.Errorf("re-exec environment lacks %q: %q", want, request.env)
		}
	}
	if strings.Contains(joinedEnv, "SUDO_") {
		t.Fatalf("re-exec environment leaked sudo identity: %q", request.env)
	}
	if state.writes != 0 || len(state.commands) != 0 {
		t.Fatalf("root dry-run mutated: writes=%d commands=%q", state.writes, state.commands)
	}
	logs := strings.Join(state.logs, "\n")
	for _, suffix := range []string{
		"systemd/oomd.conf.d/aira-oomd.conf",
		"systemd/system/user@.service.d/50-aira-oomd.conf",
		"systemd/system/user-1234.slice.d/50-aira-oomd.conf",
		"systemd/user/session.slice.d/50-aira-oomd-protect.conf",
		"systemd/system/user@.service.d/10-aira-delegate.conf",
		"sysctl.d/60-inotify-aira.conf",
	} {
		if !strings.Contains(logs, "planned: write "+filepath.Join(d.etcRoot, suffix)) {
			t.Errorf("root dry-run omitted planned /etc write %s: %q", suffix, logs)
		}
	}
	if !strings.Contains(logs, "planned: sysctl --system for changed sysctl drop-ins") {
		t.Errorf("root dry-run omitted the planned sysctl activation: %q", logs)
	}
}

type fakeRootInstallState struct {
	files       map[string][]byte
	commands    [][]string
	logs        []string
	writes      int
	failCommand string
	reexec      *reexecRequest
	oomdActive  bool
	// sysctlLive models the kernel's live fs.inotify.max_user_instances:
	// false ("128", the kernel default) until a successful `sysctl --system`
	// sets it true ("4096"). Modeled explicitly (rather than falling through
	// to a real /proc read) so root-install tests stay hermetic regardless of
	// the actual value on the host running the test.
	sysctlLive bool
}

func newFakeRootInstall(t *testing.T) (installDeps, *fakeRootInstallState) {
	t.Helper()
	d := realInstallDeps()
	state := &fakeRootInstallState{files: map[string][]byte{}}
	d.etcRoot = filepath.Join(t.TempDir(), "etc")
	d.geteuid = func() int { return 0 }
	d.getenv = func(name string) string {
		return map[string]string{"SUDO_UID": "1234", "SUDO_GID": "1234", "SUDO_USER": "alice"}[name]
	}
	alice := &user.User{Uid: "1234", Gid: "1234", Username: "alice", HomeDir: "/home/alice"}
	d.lookupUID = func(id string) (*user.User, error) {
		if id != "1234" {
			return nil, errors.New("unknown uid")
		}
		copy := *alice
		return &copy, nil
	}
	d.lookupUser = func(name string) (*user.User, error) {
		if name != "alice" {
			return nil, user.UnknownUserError(name)
		}
		copy := *alice
		return &copy, nil
	}
	d.groupIDs = func(*user.User) ([]string, error) { return []string{"1234", "55"}, nil }
	d.executable = func() (string, error) { return "/opt/aira", nil }
	d.abs = filepath.Abs
	d.stat = func(path string) (os.FileInfo, error) {
		switch path {
		case "/run/user/1234":
			return staticFileInfo{name: "1234", mode: os.ModeDir | 0o700, uid: 1234, gid: 1234}, nil
		case "/opt/aira":
			return staticFileInfo{name: "aira", mode: 0o755, uid: 0, gid: 0}, nil
		default:
			return os.Stat(path)
		}
	}
	d.lstat = func(path string) (os.FileInfo, error) {
		info, err := os.Lstat(path)
		if err != nil {
			return nil, err
		}
		if strings.HasPrefix(path, d.etcRoot+string(filepath.Separator)) {
			return ownedFileInfo{FileInfo: info, uid: 0, gid: 0}, nil
		}
		return info, nil
	}
	realFstat := d.fstat
	d.fstat = func(fd int, stat *unix.Stat_t) error {
		if err := realFstat(fd, stat); err != nil {
			return err
		}
		stat.Uid, stat.Gid = 0, 0
		return nil
	}
	realWriteFD := d.writeFD
	d.writeFD = func(fd int, data []byte) error {
		state.writes++
		return realWriteFD(fd, data)
	}
	realReadFile := d.readFile
	d.readFile = func(path string) ([]byte, error) {
		if content, ok := state.files[path]; ok {
			return append([]byte(nil), content...), nil
		}
		if path == procInotifyMaxUserInstances {
			if state.sysctlLive {
				return []byte("4096\n"), nil
			}
			return []byte("128\n"), nil
		}
		return realReadFile(path)
	}
	d.run = func(argv []string, _ []byte) ([]byte, error) {
		state.commands = append(state.commands, append([]string(nil), argv...))
		joined := strings.Join(argv, " ")
		if joined == state.failCommand {
			return nil, errors.New("injected activation failure")
		}
		if joined == "systemctl is-active systemd-oomd" {
			if state.oomdActive {
				return []byte("active\n"), nil
			}
			return []byte("inactive\n"), errors.New("exit status 3")
		}
		if joined == "systemctl restart systemd-oomd" {
			state.oomdActive = true
			return nil, nil
		}
		if joined == "sysctl --system" {
			state.sysctlLive = true
			return nil, nil
		}
		if joined == "timeout 10s loginctl enable-linger 1234" || joined == "systemctl daemon-reload" {
			return nil, nil
		}
		return nil, fmt.Errorf("unexpected real-command attempt: %q", argv)
	}
	d.reexec = func(request reexecRequest) error {
		copy := request
		copy.args = append([]string(nil), request.args...)
		copy.env = append([]string(nil), request.env...)
		if request.credential != nil {
			credential := *request.credential
			credential.Groups = append([]uint32(nil), request.credential.Groups...)
			copy.credential = &credential
		}
		state.reexec = &copy
		return nil
	}
	d.logf = func(format string, args ...any) { state.logs = append(state.logs, fmt.Sprintf(format, args...)) }
	return d, state
}

func installFakeDelegationDropin(t *testing.T, d *installDeps, root string) {
	t.Helper()
	d.etcRoot = filepath.Join(root, "fake-etc")
	rendered, err := renderSystemDropins(d.etcRoot, os.Geteuid())
	if err != nil {
		t.Fatal(err)
	}
	var dropin renderedSystemDropin
	found := false
	for _, candidate := range rendered {
		if candidate.asset == delegationDropinAsset {
			dropin, found = candidate, true
			break
		}
	}
	if !found {
		t.Fatalf("no rendered drop-in has asset %q", delegationDropinAsset)
	}
	if err := os.MkdirAll(filepath.Dir(dropin.dst), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dropin.dst, dropin.content, 0o644); err != nil {
		t.Fatal(err)
	}
	prior := d.lstat
	d.lstat = func(path string) (os.FileInfo, error) {
		info, statErr := prior(path)
		if statErr == nil && path == dropin.dst {
			return ownedFileInfo{FileInfo: info, uid: 0, gid: 0}, nil
		}
		return info, statErr
	}
}

func installFakeSystemDropins(t *testing.T, d *installDeps, root string) {
	t.Helper()
	d.etcRoot = filepath.Join(root, "fake-etc")
	rendered, err := renderSystemDropins(d.etcRoot, os.Geteuid())
	if err != nil {
		t.Fatal(err)
	}
	owned := make(map[string]bool, len(rendered))
	for _, dropin := range rendered {
		if err := os.MkdirAll(filepath.Dir(dropin.dst), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(dropin.dst, dropin.content, 0o644); err != nil {
			t.Fatal(err)
		}
		owned[dropin.dst] = true
	}
	prior := d.lstat
	d.lstat = func(path string) (os.FileInfo, error) {
		info, statErr := prior(path)
		if statErr == nil && owned[path] {
			return ownedFileInfo{FileInfo: info, uid: 0, gid: 0}, nil
		}
		return info, statErr
	}
}

func containsArgv(commands [][]string, want []string) bool {
	for _, command := range commands {
		if reflect.DeepEqual(command, want) {
			return true
		}
	}
	return false
}

type staticFileInfo struct {
	name     string
	mode     os.FileMode
	uid, gid uint32
}

func (info staticFileInfo) Name() string       { return info.name }
func (info staticFileInfo) Size() int64        { return 0 }
func (info staticFileInfo) Mode() os.FileMode  { return info.mode }
func (info staticFileInfo) ModTime() time.Time { return time.Time{} }
func (info staticFileInfo) IsDir() bool        { return info.mode.IsDir() }
func (info staticFileInfo) Sys() any           { return &syscall.Stat_t{Uid: info.uid, Gid: info.gid} }

type ownedFileInfo struct {
	os.FileInfo
	uid, gid uint32
}

func (info ownedFileInfo) Sys() any {
	stat := &syscall.Stat_t{Uid: info.uid, Gid: info.gid}
	if original, ok := info.FileInfo.Sys().(*syscall.Stat_t); ok {
		*stat = *original
		stat.Uid, stat.Gid = info.uid, info.gid
	}
	return stat
}

func containsString(values []string, wanted string) bool {
	for _, value := range values {
		if value == wanted {
			return true
		}
	}
	return false
}

func itoa(value int) string {
	return strconv.Itoa(value)
}
