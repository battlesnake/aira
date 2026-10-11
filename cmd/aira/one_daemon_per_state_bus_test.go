// verifies: AIRA-289

package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"aira/internal/daemon"
)

// userManagerSystemctlOnPath puts a `systemctl` first on PATH that behaves like
// the real `systemctl --user` (measured, systemd 255): with
// DBUS_SESSION_BUS_ADDRESS present, even empty, it reaches the user manager only
// if that is the manager's bus; otherwise only when XDG_RUNTIME_DIR is the
// manager's /run/user/<uid>. It fails with the real tool's words. The tests that
// use it leave the dispatcher's systemctl seam unset, so the production runner
// is what runs.
func userManagerSystemctlOnPath(t *testing.T) {
	t.Helper()
	dir := t.TempDir()
	script := `#!/bin/sh
if [ "${DBUS_SESSION_BUS_ADDRESS+set}" = set ]; then
  if [ "$DBUS_SESSION_BUS_ADDRESS" != "$STUB_GOOD_BUS" ]; then
    echo "Failed to connect to bus: No such file or directory" >&2
    exit 1
  fi
elif [ "$XDG_RUNTIME_DIR" != "$STUB_MANAGER_RUNTIME" ]; then
  echo "Failed to connect to bus: No medium found" >&2
  exit 1
fi
case "$*" in
  "--user is-enabled aira-daemon.service") echo enabled ;;
  "--user show -p FragmentPath aira-daemon.service") echo "FragmentPath=/home/agent/.config/systemd/user/aira-daemon.service" ;;
  "--user show-environment") echo "XDG_RUNTIME_DIR=$STUB_MANAGER_RUNTIME" ;;
esac
exit 0
`
	if err := os.WriteFile(filepath.Join(dir, "systemctl"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("STUB_MANAGER_RUNTIME", fmt.Sprintf("/run/user/%d", os.Getuid()))
	t.Setenv("STUB_GOOD_BUS", fmt.Sprintf("unix:path=/run/user/%d/bus", os.Getuid()))
	unsetenvForTest(t, "DBUS_SESSION_BUS_ADDRESS")
	unsetenvForTest(t, "XDG_RUNTIME_DIR")
}

// unsetenvForTest removes name for this test and restores it afterwards.
func unsetenvForTest(t *testing.T, name string) {
	t.Helper()
	t.Setenv(name, "")
	if err := os.Unsetenv(name); err != nil {
		t.Fatal(err)
	}
}

// fragmentPathAnswer answers the unit-file query the way the user manager does
// for a unit installed under HOME (where these tests write it), so a stub that
// models only is-enabled, show-environment and start still identifies the unit.
func fragmentPathAnswer(argv []string) ([]byte, bool) {
	if strings.Join(argv, " ") != "systemctl --user show -p FragmentPath "+daemon.DefaultServiceUnit {
		return nil, false
	}
	return []byte("FragmentPath=" + filepath.Join(os.Getenv("HOME"), ".config", "systemd", "user", daemon.DefaultServiceUnit) + "\n"), true
}

// serviceReader reads, whatever path the manager names, an enabled
// aira-managed unit whose socket directory comes from the user manager (the
// production shape: no baked XDG_RUNTIME_DIR) and which serves stateHome.
func serviceReader(stateHome string) func(string) ([]byte, error) {
	return func(string) ([]byte, error) {
		return []byte("# aira-managed: aira-daemon.service\n[Service]\nEnvironment=XDG_STATE_HOME=" + stateHome + "\n"), nil
	}
}

func homeOnly(name string) string {
	if name == "HOME" {
		return "/home/agent"
	}
	return ""
}

// A cron job or a gate whose env_allow omits XDG_RUNTIME_DIR and the session
// bus cannot reach the user manager through `systemctl --user`. The service is
// down and serves this state at /run/user/<uid>/...; the client looks at the
// state directory. It must refuse, not fork a stray that wins the state lock
// (the stray runs without the unit's watchdog and the service is then restarted
// every 2 s). The stub is the real tool's no-bus behaviour, not an answer given
// whatever the environment (AIRA-289, build review round 2).
func TestClientWithNoBusNeverForksAStrayWhenTheServiceServesThisState(t *testing.T) {
	userManagerSystemctlOnPath(t)
	state := filepath.Join(t.TempDir(), "state")
	paths, err := daemon.PathsFromEnvironment(state, "", "")
	if err != nil {
		t.Fatal(err)
	}
	d := &daemonDispatcher{
		paths: paths, startWait: time.Second, lockWait: time.Second, spawn: noSpawn(t),
		readFile: serviceReader(paths.StateHome), getenv: homeOnly,
	}
	_, err = d.exchangeOrStart(context.Background(), daemon.RequestFrame{Proto: daemon.ProtocolVersion})
	if err == nil {
		t.Fatal("no daemon is reachable from this client, yet it reported success")
	}
	response := transportErrorResponse(err)
	if response.Code != daemon.CodeUnavailable || response.Exit != 4 {
		t.Fatalf("code=%q exit=%d (%v), want %s exit 4", response.Code, response.Exit, err, daemon.CodeUnavailable)
	}
	if !strings.Contains(err.Error(), "aira-daemon.service") {
		t.Fatalf("message %q does not name the service", err.Error())
	}
}

// The confine twin: with the service down there is no lock holder to read, so
// the launch check can only see the unit, and must see it without a bus.
func TestConfineLaunchWithNoBusIsRefusedWhenTheServiceServesThisState(t *testing.T) {
	userManagerSystemctlOnPath(t)
	paths, err := daemon.PathsFromEnvironment(filepath.Join(t.TempDir(), "state"), "", "")
	if err != nil {
		t.Fatal(err)
	}
	refusal := unreachableDaemonError(paths, daemon.RunSystemctl, serviceReader(paths.StateHome), homeOnly)
	if refusal == nil || !strings.Contains(refusal.Error(), daemon.CodeUnavailable) || !strings.Contains(refusal.Error(), "aira-daemon.service") {
		t.Fatalf("refusal=%v, want %s naming the service", refusal, daemon.CodeUnavailable)
	}
}

// writeStaleLockRecord leaves a lock file with a daemon's record and NO flock
// held: what a daemon that died uncleanly leaves behind.
func writeStaleLockRecord(t *testing.T, paths daemon.Paths, socket string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(paths.LockPath), 0o700); err != nil {
		t.Fatal(err)
	}
	bootID, _ := os.ReadFile("/proc/sys/kernel/random/boot_id")
	data, _ := json.Marshal(daemon.LockInfo{PID: os.Getpid(), BootID: strings.TrimSpace(string(bootID)), Socket: socket})
	if err := os.WriteFile(paths.LockPath, data, 0o600); err != nil {
		t.Fatal(err)
	}
}

// leaveDeadSocket leaves a unix socket FILE at path that nothing listens on:
// what a stray that was killed (SIGKILL, a WSL restart) leaves under a state
// directory that outlives reboots.
func leaveDeadSocket(t *testing.T, path string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	listener, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	listener.(*net.UnixListener).SetUnlinkOnClose(false)
	_ = listener.Close()
	if _, err := os.Lstat(path); err != nil {
		t.Fatalf("fixture: the dead socket file is missing: %v", err)
	}
}

// A file at the client's own socket path is not a daemon. Checking only that
// the path exists skipped both refusals for good once a stray had died there:
// every confine then waited forever again and every verb lost its explanation.
func TestADeadSocketFileDoesNotHideAnUnreachableDaemonFromConfine(t *testing.T) {
	pathsA, pathsB, _, _ := oneStateTwoRuntimes(t)
	startCommandDaemon(t, daemon.NewServer(pathsA))
	leaveDeadSocket(t, pathsB.SocketPath)
	refusal := unreachableDaemonError(pathsB, func([]string) ([]byte, error) { return nil, errors.New("no unit") }, os.ReadFile, homeOnly)
	if refusal == nil || !strings.Contains(refusal.Error(), pathsA.SocketPath) {
		t.Fatalf("refusal=%v, want %s naming %s", refusal, daemon.CodeUnavailable, pathsA.SocketPath)
	}
}

func TestADeadSocketFileDoesNotHideAnUnreachableDaemonFromAVerb(t *testing.T) {
	pathsA, pathsB, _, runtimeB := oneStateTwoRuntimes(t)
	startCommandDaemon(t, daemon.NewServer(pathsA))
	leaveDeadSocket(t, pathsB.SocketPath)
	d := &daemonDispatcher{
		paths: pathsB, startWait: 500 * time.Millisecond, lockWait: time.Second, spawn: noSpawn(t),
		getenv: func(name string) string {
			if name == "XDG_RUNTIME_DIR" {
				return runtimeB
			}
			return ""
		},
	}
	_, err := d.exchangeOrStart(context.Background(), daemon.RequestFrame{Proto: daemon.ProtocolVersion})
	response := transportErrorResponse(err)
	if err == nil || response.Code != daemon.CodeUnavailable || response.Exit != 4 {
		t.Fatalf("code=%q exit=%d err=%v, want %s exit 4", response.Code, response.Exit, err, daemon.CodeUnavailable)
	}
}

// Something that ANSWERS at the client's own socket is reachable, whatever the
// lock file says; the check dials only that socket (100 ms) and never refuses
// a client that can connect.
func TestAnAnsweringSocketIsNeverRefusedByTheLaunchCheck(t *testing.T) {
	pathsA, pathsB, _, _ := oneStateTwoRuntimes(t)
	holdStateLock(t, pathsB, pathsA.SocketPath)
	if err := os.MkdirAll(filepath.Dir(pathsB.SocketPath), 0o700); err != nil {
		t.Fatal(err)
	}
	listener, err := net.Listen("unix", pathsB.SocketPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	if refusal := unreachableDaemonError(pathsB, func([]string) ([]byte, error) { return nil, errors.New("no unit") }, os.ReadFile, homeOnly); refusal != nil {
		t.Fatalf("a client whose own socket answers was refused: %v", refusal)
	}
}

// The launch check's decision table, in both directions. Every row that must
// WAIT guards a mutation that would refuse a correctly configured client while
// its daemon restarts; every row that must REFUSE guards the opposite.
func TestConfineLaunchCheckDecisionTable(t *testing.T) {
	// An enabled unit; the state it serves comes from serviceReader(unitState),
	// its socket directory from STUB_SERVICE_RUNTIME.
	enabledUnit := func(argv []string) ([]byte, error) {
		if answer, ok := fragmentPathAnswer(argv); ok {
			return answer, nil
		}
		switch strings.Join(argv, " ") {
		case "systemctl --user is-enabled aira-daemon.service":
			return []byte("enabled\n"), nil
		case "systemctl --user show-environment":
			return []byte("XDG_RUNTIME_DIR=" + os.Getenv("STUB_SERVICE_RUNTIME") + "\n"), nil
		}
		return nil, errors.New("unexpected: " + strings.Join(argv, " "))
	}
	type row struct {
		name   string
		refuse bool
		setup  func(t *testing.T, own daemon.Paths, other daemon.Paths) (systemctl daemon.SystemctlRun, unitState string)
	}
	rows := []row{
		{"holder bound elsewhere: refuse", true, func(t *testing.T, own, other daemon.Paths) (daemon.SystemctlRun, string) {
			holdStateLock(t, own, other.SocketPath)
			return nil, ""
		}},
		{"holder recorded this client's own socket and has not bound it: wait", false, func(t *testing.T, own, other daemon.Paths) (daemon.SystemctlRun, string) {
			holdStateLock(t, own, own.SocketPath)
			return nil, ""
		}},
		{"holder recorded no socket (an older daemon): wait", false, func(t *testing.T, own, other daemon.Paths) (daemon.SystemctlRun, string) {
			holdStateLock(t, own, "")
			return nil, ""
		}},
		{"stale record, no lock held: wait", false, func(t *testing.T, own, other daemon.Paths) (daemon.SystemctlRun, string) {
			writeStaleLockRecord(t, own, other.SocketPath)
			return nil, ""
		}},
		{"enabled unit for this state at another socket, service down: refuse", true, func(t *testing.T, own, other daemon.Paths) (daemon.SystemctlRun, string) {
			t.Setenv("STUB_SERVICE_RUNTIME", filepath.Dir(filepath.Dir(filepath.Dir(other.SocketPath))))
			return enabledUnit, own.StateHome
		}},
		{"enabled unit for this state at this client's own socket, service down: wait", false, func(t *testing.T, own, other daemon.Paths) (daemon.SystemctlRun, string) {
			t.Setenv("STUB_SERVICE_RUNTIME", filepath.Dir(filepath.Dir(filepath.Dir(own.SocketPath))))
			return enabledUnit, own.StateHome
		}},
		{"enabled unit for another state: wait", false, func(t *testing.T, own, other daemon.Paths) (daemon.SystemctlRun, string) {
			t.Setenv("STUB_SERVICE_RUNTIME", filepath.Dir(filepath.Dir(filepath.Dir(other.SocketPath))))
			return enabledUnit, filepath.Join(t.TempDir(), "another-state")
		}},
		{"unit not enabled and no holder: wait", false, func(t *testing.T, own, other daemon.Paths) (daemon.SystemctlRun, string) {
			return func([]string) ([]byte, error) { return []byte("disabled\n"), errors.New("exit 1") }, own.StateHome
		}},
	}
	for _, r := range rows {
		t.Run(r.name, func(t *testing.T) {
			pathsA, pathsB, _, _ := oneStateTwoRuntimes(t)
			systemctl, unitState := r.setup(t, pathsB, pathsA)
			if systemctl == nil {
				systemctl = func([]string) ([]byte, error) { return nil, errors.New("no unit") }
			}
			read := serviceReader(unitState)
			refusal := unreachableDaemonError(pathsB, systemctl, read, homeOnly)
			if r.refuse && (refusal == nil || !strings.Contains(refusal.Error(), daemon.CodeUnavailable)) {
				t.Fatalf("refusal=%v, want %s", refusal, daemon.CodeUnavailable)
			}
			if !r.refuse && refusal != nil {
				t.Fatalf("a launch that must wait was refused: %v", refusal)
			}
		})
	}
}
