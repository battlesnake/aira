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

func TestParseInstallWatchdogFlags(t *testing.T) {
	opts, err := parseInstallArgs(nil)
	if err != nil || opts.watchdog != "observe" || opts.watchdogInterval != 2*time.Second {
		t.Fatalf("defaults=%+v err=%v", opts, err)
	}
	opts, err = parseInstallArgs([]string{"--watchdog=enforce", "--watchdog-interval", "5s"})
	if err != nil || opts.watchdog != "enforce" || opts.watchdogInterval != 5*time.Second {
		t.Fatalf("explicit=%+v err=%v", opts, err)
	}
	for _, args := range [][]string{{"--watchdog=invalid"}, {"--watchdog-interval=999ms"}, {"--watchdog-interval=30s"}} {
		if _, err := parseInstallArgs(args); err == nil || !strings.Contains(err.Error(), CodeArgumentInvalid) {
			t.Fatalf("args=%q err=%v", args, err)
		}
	}
}

func TestRenderDaemonUnitIsManagedBoundedAndIndependent(t *testing.T) {
	unit, err := renderDaemonUnit("test-daemon.service", "/opt/aira bin", "/var/lib/aira state", "observe", 2*time.Second, "")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"# aira-managed: test-daemon.service", `ExecStart="/opt/aira bin" daemon serve`,
		"Environment=AIRA_DAEMON_MANAGED=1", "Environment=AIRA_DAEMON_WATCHDOG_MODE=observe",
		"Environment=AIRA_DAEMON_WATCHDOG_INTERVAL=2s", `Environment=XDG_STATE_HOME="/var/lib/aira state"`,
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
