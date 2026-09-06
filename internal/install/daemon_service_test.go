package install

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"aira/internal/daemon"
)

// TestInstallDaemonNoChangeRunDoesNotBounceDaemon: a byte-identical convergence
// re-run must NOT stop or restart the live machine daemon (that would drop the
// admission queue + reset the watchdog latch on a no-op); only a watchdog change
// may restart it. RED against the pre-fix unconditional stop+restart.
func TestInstallDaemonNoChangeRunDoesNotBounceDaemon(t *testing.T) {
	d, state := newFakeInstall(t)
	stops := 0
	baseStop := d.daemonStop
	d.daemonStop = func(p daemon.Paths) error { stops++; return baseStop(p) }

	if err := runInstall(d, installOpts{memoryMax: "16G", watchdog: "observe", watchdogInterval: 2 * time.Second}); err != nil {
		t.Fatal(err)
	}

	// Byte-identical convergence — must leave the live daemon untouched.
	state.commands = nil
	stops = 0
	if err := runInstall(d, installOpts{memoryMax: "16G", watchdog: "observe", watchdogInterval: 2 * time.Second}); err != nil {
		t.Fatal(err)
	}
	if stops != 0 {
		t.Fatalf("no-change convergence run stopped the live daemon %d time(s)", stops)
	}
	for _, argv := range state.commands {
		if strings.Join(argv, " ") == "systemctl --user restart "+defaultDaemonUnit {
			t.Fatalf("no-change convergence run restarted the live daemon; commands=%q", state.commands)
		}
	}

	// A watchdog change — only a changing run may restart.
	state.commands = nil
	if err := runInstall(d, installOpts{memoryMax: "16G", watchdog: "enforce", watchdogInterval: 2 * time.Second}); err != nil {
		t.Fatal(err)
	}
	foundRestart := false
	for _, argv := range state.commands {
		if strings.Join(argv, " ") == "systemctl --user restart "+defaultDaemonUnit {
			foundRestart = true
		}
	}
	if !foundRestart {
		t.Fatalf("watchdog change did not restart daemon; commands=%q", state.commands)
	}
}

// verifies (AIRA-106): an ABSENT daemon-mode option parses to the ZERO VALUE.
// That is load-bearing, not cosmetic: it is the only signal resolveDaemonModes
// has for "keep what is installed", and pre-filling "observe" here is precisely
// what made every re-install reset the watchdog's mode.
func TestParseInstallWatchdogFlags(t *testing.T) {
	opts, err := parseInstallArgs(nil)
	if err != nil || opts.watchdog != "" || opts.sliceCeiling != "" || opts.watchdogInterval != 0 {
		t.Fatalf("absent options must parse to the zero value, got %+v err=%v", opts, err)
	}
	opts, err = parseInstallArgs([]string{"--watchdog=enforce", "--watchdog-interval", "5s", "--slice-ceiling", "enforce"})
	if err != nil || opts.watchdog != "enforce" || opts.watchdogInterval != 5*time.Second || opts.sliceCeiling != "enforce" {
		t.Fatalf("explicit=%+v err=%v", opts, err)
	}
	for _, args := range [][]string{
		{"--watchdog=invalid"}, {"--watchdog-interval=999ms"}, {"--watchdog-interval=30s"},
		{"--slice-ceiling=invalid"}, {"--slice-ceiling"}, {"--status", "--slice-ceiling=off"},
	} {
		if _, err := parseInstallArgs(args); err == nil || !strings.Contains(err.Error(), CodeArgumentInvalid) {
			t.Fatalf("args=%q err=%v", args, err)
		}
	}
}

// verifies (AIRA-106): the PRESERVATION rule, at the seam that decides it.
//
// RED against the pre-AIRA-106 behaviour, in which an omitted flag rendered the
// ship default and so silently reverted an operator's enforce on the next
// unrelated deploy -- observed live on the development box.
func TestResolveDaemonModesPreservesInstalledModes(t *testing.T) {
	installed := "# aira-managed: aira-daemon.service\n[Service]\n" +
		"Environment=AIRA_DAEMON_WATCHDOG_MODE=enforce\n" +
		"Environment=AIRA_DAEMON_WATCHDOG_INTERVAL=5s\n" +
		"Environment=AIRA_DAEMON_SLICE_CEILING_MODE=enforce\n"
	resolved, err := resolveDaemonModes(installOpts{}, installed)
	if err != nil || resolved.watchdog != "enforce" || resolved.sliceCeiling != "enforce" || resolved.watchdogInterval != 5*time.Second {
		t.Fatalf("omitted options did not preserve the installed unit: %+v err=%v", resolved, err)
	}
	resolved, err = resolveDaemonModes(installOpts{watchdog: "off", sliceCeiling: "off", watchdogInterval: time.Second}, installed)
	if err != nil || resolved.watchdog != "off" || resolved.sliceCeiling != "off" || resolved.watchdogInterval != time.Second {
		t.Fatalf("an explicit option was overridden by the installed unit: %+v err=%v", resolved, err)
	}
	resolved, err = resolveDaemonModes(installOpts{}, "")
	if err != nil || resolved.watchdog != "observe" || resolved.sliceCeiling != "observe" || resolved.watchdogInterval != 2*time.Second {
		t.Fatalf("first install did not take the ship defaults: %+v err=%v", resolved, err)
	}
	// A hand-edited or newer-vocabulary unit must neither be propagated nor make
	// a later install fail: the ship default is the safe answer.
	resolved, err = resolveDaemonModes(installOpts{}, "Environment=AIRA_DAEMON_WATCHDOG_MODE=paranoid\nEnvironment=AIRA_DAEMON_WATCHDOG_INTERVAL=17\n")
	if err != nil || resolved.watchdog != "observe" || resolved.watchdogInterval != 2*time.Second {
		t.Fatalf("an unrecognised installed value was propagated or refused: %+v err=%v", resolved, err)
	}
	// systemd accepts a QUOTED assignment and gives a LATER assignment of the
	// same name precedence. A first-match, unquoted-only reader mis-reads both --
	// and mis-reading means falling back to the ship default, i.e. silently
	// resetting the operator's mode, the exact failure this mechanism prevents.
	for _, test := range []struct{ name, unit, want string }{
		{"quoted", "[Service]\nEnvironment=\"AIRA_DAEMON_WATCHDOG_MODE=enforce\"\n", "enforce"},
		{"later-assignment-wins", "[Service]\nEnvironment=AIRA_DAEMON_WATCHDOG_MODE=observe\nEnvironment=AIRA_DAEMON_WATCHDOG_MODE=enforce\n", "enforce"},
		{"trailing-space", "[Service]\nEnvironment=AIRA_DAEMON_WATCHDOG_MODE=enforce  \n", "enforce"},
		// A multi-assignment line is NOT parsed: it reads as absent and falls back
		// to the ship default, which is the safe direction. Pinned so a later
		// "improvement" that guesses at it has to say so.
		{"multi-assignment-falls-back", "[Service]\nEnvironment=FOO=1 AIRA_DAEMON_WATCHDOG_MODE=enforce\n", "observe"},
	} {
		resolved, err = resolveDaemonModes(installOpts{}, test.unit)
		if err != nil || resolved.watchdog != test.want {
			t.Fatalf("%s: watchdog=%q err=%v, want %q -- a mis-read here silently resets the operator's mode", test.name, resolved.watchdog, err, test.want)
		}
	}
}

// verifies (AIRA-106): the ROOT RE-EXEC does not forge an explicit flag.
//
// This is the half that makes the preservation real on `sudo aira install` --
// the path install itself recommends. reexecRequestFor used to append --watchdog
// unconditionally, so the re-exec'd unprivileged install always saw an explicit
// flag and "absent means preserve" could never fire. RED against that.
func TestReexecDoesNotForwardUngivenDaemonModes(t *testing.T) {
	target := installTarget{uid: 1000, gid: 1000, home: "/home/someone", username: "someone"}
	args := strings.Join(reexecRequestFor("/usr/bin/aira", target, installOpts{memoryMax: "16G"}).args, " ")
	for _, forbidden := range []string{"--watchdog", "--watchdog-interval", "--slice-ceiling"} {
		if strings.Contains(args, forbidden) {
			t.Fatalf("re-exec argv %q forwards %s though it was never given: the unprivileged install would read it as explicit and reset the installed mode", args, forbidden)
		}
	}
	given := installOpts{watchdog: "enforce", sliceCeiling: "off", watchdogInterval: 5 * time.Second}
	args = strings.Join(reexecRequestFor("/usr/bin/aira", target, given).args, " ")
	for _, want := range []string{"--watchdog enforce", "--slice-ceiling off", "--watchdog-interval 5s"} {
		if !strings.Contains(args, want) {
			t.Fatalf("re-exec argv %q drops the explicit %q", args, want)
		}
	}
}

func TestRenderDaemonUnitIsManagedBoundedAndIndependent(t *testing.T) {
	unit, err := renderDaemonUnit("test-daemon.service", "/opt/aira bin", "/var/lib/aira state", "observe", 2*time.Second, "observe", "")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"# aira-managed: test-daemon.service", `ExecStart="/opt/aira bin" daemon serve`,
		"Environment=AIRA_DAEMON_MANAGED=1", "Environment=AIRA_DAEMON_WATCHDOG_MODE=observe",
		"Environment=AIRA_DAEMON_WATCHDOG_INTERVAL=2s",
		"Environment=AIRA_DAEMON_SLICE_CEILING_MODE=observe", `Environment=XDG_STATE_HOME="/var/lib/aira state"`,
		"MemoryAccounting=yes", "MemoryMax=1G", "MemoryHigh=768M", "TasksMax=512",
		"StartLimitIntervalSec=0", "Restart=always", "WantedBy=default.target",
	} {
		if !strings.Contains(unit, want) {
			t.Errorf("unit lacks %q:\n%s", want, unit)
		}
	}
	if strings.Contains(unit, "Slice=") || strings.Contains(unit, "@") {
		t.Fatalf("daemon unit is copied into a slice or has placeholders:\n%s", unit)
	}
}

func TestInstallDaemonIdempotentAndWatchdogChangeRestarts(t *testing.T) {
	d, state := newFakeInstall(t)
	if err := runInstall(d, installOpts{memoryMax: "16G", watchdog: "observe", watchdogInterval: 2 * time.Second}); err != nil {
		t.Fatal(err)
	}
	writes := state.writes
	if err := runInstall(d, installOpts{memoryMax: "16G", watchdog: "observe", watchdogInterval: 2 * time.Second}); err != nil {
		t.Fatal(err)
	}
	if state.writes != writes {
		t.Fatalf("idempotent daemon install rewrote files: %d -> %d", writes, state.writes)
	}
	if err := runInstall(d, installOpts{memoryMax: "16G", watchdog: "enforce", watchdogInterval: 2 * time.Second}); err != nil {
		t.Fatal(err)
	}
	content, err := os.ReadFile(filepath.Join(state.unitDir(), defaultDaemonUnit))
	if err != nil || !strings.Contains(string(content), "AIRA_DAEMON_WATCHDOG_MODE=enforce") {
		t.Fatalf("daemon content=%q err=%v", content, err)
	}
	foundRestart := false
	for _, argv := range state.commands {
		if strings.Join(argv, " ") == "systemctl --user restart "+defaultDaemonUnit {
			foundRestart = true
		}
	}
	if !foundRestart {
		t.Fatalf("watchdog change did not restart daemon; commands=%q", state.commands)
	}
}

// verifies (AIRA-106): END TO END, a re-install with NO mode flags preserves
// both installed modes and does not bounce the daemon.
//
// This is the integration form of the live defect: the operator flips a mode
// once, some later unrelated deploy runs `aira install` with no flags, and before
// this fix the unit was rewritten to observe and the daemon restarted into it.
// Both memory subsystems are covered because AIRA-106's own rollout depends on
// the slice-ceiling mode surviving exactly this sequence.
func TestInstallDaemonReinstallPreservesModes(t *testing.T) {
	d, state := newFakeInstall(t)
	if err := runInstall(d, installOpts{memoryMax: "16G", watchdog: "enforce", sliceCeiling: "enforce", watchdogInterval: 5 * time.Second}); err != nil {
		t.Fatal(err)
	}
	writes := state.writes
	state.commands = nil
	// The deploy case: memory sizing given, modes omitted entirely.
	if err := runInstall(d, installOpts{memoryMax: "16G"}); err != nil {
		t.Fatal(err)
	}
	content, err := os.ReadFile(filepath.Join(state.unitDir(), defaultDaemonUnit))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"AIRA_DAEMON_WATCHDOG_MODE=enforce",
		"AIRA_DAEMON_SLICE_CEILING_MODE=enforce",
		"AIRA_DAEMON_WATCHDOG_INTERVAL=5s",
	} {
		if !strings.Contains(string(content), want) {
			t.Fatalf("a flagless re-install silently reset the daemon unit; want %q in:\n%s", want, content)
		}
	}
	if state.writes != writes {
		t.Fatalf("a preserving re-install rewrote files: %d -> %d", writes, state.writes)
	}
	for _, argv := range state.commands {
		if strings.Join(argv, " ") == "systemctl --user restart "+defaultDaemonUnit {
			t.Fatalf("a preserving re-install restarted the live daemon; commands=%q", state.commands)
		}
	}
}

// verifies (AIRA-106): a mode change that lands BETWEEN the pre-lock unit read
// and the install lock is preserved, not overwritten.
//
// The unit content that seeds resolveDaemonModes is read before the lock (it has
// to be: --dry-run renders and returns before ever taking one). A concurrent
// `aira install --watchdog enforce` in that window would otherwise be undone by
// this run's stale-preserved "observe", AND the daemon restarted into the
// reverted mode -- exactly the defect class the preservation exists to stop,
// through a narrower door. publishManagedUnit's own locked re-read cannot catch
// it: it compares CONTENT and has no way to recompute a preserved setting.
//
// The concurrent install is simulated at the only instant that matters, by
// rewriting the unit inside d.flock -- which is called once, immediately before
// the locked re-read. RED against resolving only before the lock.
func TestInstallDaemonConcurrentModeChangeSurvivesTheLock(t *testing.T) {
	d, state := newFakeInstall(t)
	if err := runInstall(d, installOpts{memoryMax: "16G", watchdog: "observe", sliceCeiling: "observe", watchdogInterval: 2 * time.Second}); err != nil {
		t.Fatal(err)
	}
	unitPath := filepath.Join(state.unitDir(), defaultDaemonUnit)
	installed, err := os.ReadFile(unitPath)
	if err != nil {
		t.Fatal(err)
	}
	concurrent := strings.NewReplacer(
		"AIRA_DAEMON_WATCHDOG_MODE=observe", "AIRA_DAEMON_WATCHDOG_MODE=enforce",
		"AIRA_DAEMON_SLICE_CEILING_MODE=observe", "AIRA_DAEMON_SLICE_CEILING_MODE=enforce",
	).Replace(string(installed))
	if concurrent == string(installed) {
		t.Fatalf("the seeded unit did not carry both observe modes:\n%s", installed)
	}
	baseFlock := d.flock
	swapped := false
	d.flock = func(fd, how int) error {
		if !swapped {
			swapped = true
			if writeErr := os.WriteFile(unitPath, []byte(concurrent), 0o644); writeErr != nil {
				t.Fatalf("simulate the concurrent install: %v", writeErr)
			}
		}
		return baseFlock(fd, how)
	}
	// The deploy case: memory sizing given, both modes omitted.
	if err := runInstall(d, installOpts{memoryMax: "16G"}); err != nil {
		t.Fatal(err)
	}
	if !swapped {
		t.Fatal("the install never took the lock; this test proved nothing")
	}
	final, err := os.ReadFile(unitPath)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"AIRA_DAEMON_WATCHDOG_MODE=enforce", "AIRA_DAEMON_SLICE_CEILING_MODE=enforce"} {
		if !strings.Contains(string(final), want) {
			t.Fatalf("a concurrent mode change was reverted by a flagless install; want %q in:\n%s", want, final)
		}
	}
}

func TestInstallDaemonMainPIDMismatchIsUnavailable(t *testing.T) {
	d, state := newFakeInstall(t)
	paths, err := d.daemonPaths()
	if err != nil {
		t.Fatal(err)
	}
	state.daemonRunning = true
	d.daemonStatus = func(daemon.Paths) daemon.StatusInfo {
		return daemon.StatusInfo{Running: true, Ready: true, Lock: daemon.LockInfo{PID: 42}}
	}
	prior := d.run
	d.run = func(argv []string, stdin []byte) ([]byte, error) {
		if strings.Join(argv, " ") == "systemctl --user show -p MainPID --value "+defaultDaemonUnit {
			return []byte("41\n"), nil
		}
		return prior(argv, stdin)
	}
	err = unavailable(verifyDaemonReachable(d, paths))
	if err == nil || !strings.Contains(err.Error(), CodeUnavailable) || !strings.Contains(err.Error(), "MainPID") {
		t.Fatalf("err=%v", err)
	}
}

func TestInstallRefusesSocketIdentityDivergence(t *testing.T) {
	d, state := newFakeInstall(t)
	priorGetenv := d.getenv
	d.getenv = func(name string) string {
		if name == "XDG_RUNTIME_DIR" {
			return filepath.Join(state.home, "client-runtime")
		}
		return priorGetenv(name)
	}
	priorRun := d.run
	d.run = func(argv []string, stdin []byte) ([]byte, error) {
		if strings.Join(argv, " ") == "systemctl --user show-environment" {
			return []byte("XDG_RUNTIME_DIR=" + filepath.Join(state.home, "manager-runtime") + "\n"), nil
		}
		return priorRun(argv, stdin)
	}
	err := runInstall(d, installOpts{memoryMax: "16G", watchdog: "observe", watchdogInterval: 2 * time.Second})
	if err == nil || !strings.Contains(err.Error(), CodeUnavailable) || !strings.Contains(err.Error(), "SocketPath") {
		t.Fatalf("err=%v", err)
	}
	if _, statErr := os.Stat(filepath.Join(state.unitDir(), defaultDaemonUnit)); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("divergent install published daemon unit: %v", statErr)
	}
}
