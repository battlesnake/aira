// verifies: AIRA-289

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/unix"

	"aira/internal/daemon"
)

// A client whose own socket ANSWERS but whose exchange ran out of time (a
// daemon that accepts and is slow to reply) reached a daemon: whatever the lock
// file records, the plain timeout is the truth. Saying the holder "has not
// bound" the socket, or that nothing listens there, would be false (build
// review round 3: the Ready guard in lockHolderError was unpinned).
func TestAnAnsweringSocketKeepsThePlainTimeout(t *testing.T) {
	for _, row := range []struct {
		name     string
		recorded func(own, other daemon.Paths) string
	}{
		{"holder recorded another socket", func(_, other daemon.Paths) string { return other.SocketPath }},
		{"holder recorded this client's socket", func(own, _ daemon.Paths) string { return own.SocketPath }},
	} {
		t.Run(row.name, func(t *testing.T) {
			pathsA, pathsB, _, runtimeB := oneStateTwoRuntimes(t)
			holdStateLock(t, pathsB, row.recorded(pathsB, pathsA))
			if err := os.MkdirAll(filepath.Dir(pathsB.SocketPath), 0o700); err != nil {
				t.Fatal(err)
			}
			listener, err := net.Listen("unix", pathsB.SocketPath)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = listener.Close() })
			d := &daemonDispatcher{
				paths: pathsB, startWait: 300 * time.Millisecond, lockWait: time.Second, spawn: noSpawn(t),
				getenv: func(string) string { return runtimeB },
				exchange: func(context.Context, string, daemon.RequestFrame) (daemon.ResponseFrame, error) {
					return daemon.ResponseFrame{}, errors.New(daemon.CodeUnavailable + ": no reply before the exchange deadline")
				},
			}
			_, err = d.exchangeOrStart(context.Background(), daemon.RequestFrame{Proto: daemon.ProtocolVersion})
			response := transportErrorResponse(err)
			if err == nil || response.Code != daemon.CodeTimeout || response.Exit != 3 {
				t.Fatalf("code=%q exit=%d err=%v, want %s exit 3", response.Code, response.Exit, err, daemon.CodeTimeout)
			}
			for _, false_ := range []string{"has not bound", "no daemon is listening", "XDG_RUNTIME_DIR"} {
				if strings.Contains(err.Error(), false_) {
					t.Fatalf("a client whose socket answers was told %q: %v", false_, err)
				}
			}
		})
	}
}

// managerUnitElsewhere writes an aira-managed unit at a path outside HOME (the
// file the user manager has loaded) and makes the systemctl seam name it as the
// unit's FragmentPath. starts counts `systemctl --user start` calls.
func managerUnitElsewhere(t *testing.T, stateHome string, starts *int) {
	t.Helper()
	unit := filepath.Join(t.TempDir(), "manager", daemon.DefaultServiceUnit)
	if err := os.MkdirAll(filepath.Dir(unit), 0o755); err != nil {
		t.Fatal(err)
	}
	content := "# aira-managed: aira-daemon.service\n[Service]\nEnvironment=XDG_STATE_HOME=" + stateHome + "\n" +
		"Environment=XDG_RUNTIME_DIR=" + filepath.Join(t.TempDir(), "manager-runtime") + "\n"
	if err := os.WriteFile(unit, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	original := daemonSystemctlRun
	t.Cleanup(func() { daemonSystemctlRun = original })
	daemonSystemctlRun = func(argv []string) ([]byte, error) {
		switch strings.Join(argv, " ") {
		case "systemctl --user is-enabled aira-daemon.service":
			return []byte("enabled\n"), nil
		case "systemctl --user show -p FragmentPath aira-daemon.service":
			return []byte("FragmentPath=" + unit + "\n"), nil
		case "systemctl --user start aira-daemon.service":
			*starts++
			return nil, nil
		}
		return nil, errors.New("unexpected command: " + strings.Join(argv, " "))
	}
}

// A sandbox (a test harness, a reviewer's probe) with a private HOME holding an
// aira-managed unit for its OWN state: is-enabled and `start` reach the uid's
// real manager, whose unit is another file serving another state. The hand-run
// `aira daemon` must serve its own state and never `systemctl --user start` the
// box's real service (build review round 3).
func TestDaemonServeInAPrivateHomeNeverStartsTheManagersUnit(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	stateHome := filepath.Join(t.TempDir(), "state")
	runtimeDir := filepath.Join(t.TempDir(), "runtime")
	t.Setenv("XDG_STATE_HOME", stateHome)
	t.Setenv("XDG_RUNTIME_DIR", runtimeDir)
	t.Setenv("AIRA_DAEMON_MANAGED", "")
	// The private HOME's unit names exactly this process's state and runtime.
	unitDir := filepath.Join(home, ".config", "systemd", "user")
	if err := os.MkdirAll(unitDir, 0o755); err != nil {
		t.Fatal(err)
	}
	private := "# aira-managed: aira-daemon.service\n[Service]\nEnvironment=XDG_STATE_HOME=" + stateHome + "\n" +
		"Environment=XDG_RUNTIME_DIR=" + runtimeDir + "\n"
	if err := os.WriteFile(filepath.Join(unitDir, daemon.DefaultServiceUnit), []byte(private), 0o644); err != nil {
		t.Fatal(err)
	}
	starts := 0
	managerUnitElsewhere(t, filepath.Join(t.TempDir(), "box-state"), &starts)
	originalServe := serveDaemon
	t.Cleanup(func() { serveDaemon = originalServe })
	serves := 0
	serveDaemon = func(context.Context, daemon.Paths) error { serves++; return nil }
	if exit := runDaemonCommand([]string{"serve"}, os.Stdout, os.Stderr); exit != 0 || starts != 0 || serves != 1 {
		t.Fatalf("exit=%d starts=%d serves=%d, want the sandbox to serve its own state and start nothing", exit, starts, serves)
	}
}

// managedUnitServing writes, under HOME, the enabled unit that serves stateHome
// through serviceRuntime, and stubs the systemctl seam to report it enabled.
func managedUnitServing(t *testing.T, stateHome, serviceRuntime string, starts *int) {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	unitDir := filepath.Join(home, ".config", "systemd", "user")
	if err := os.MkdirAll(unitDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(unitDir, daemon.DefaultServiceUnit), managedUnitFor(stateHome, serviceRuntime), 0o644); err != nil {
		t.Fatal(err)
	}
	original := daemonSystemctlRun
	t.Cleanup(func() { daemonSystemctlRun = original })
	daemonSystemctlRun = func(argv []string) ([]byte, error) {
		if answer, ok := fragmentPathAnswer(argv); ok {
			return answer, nil
		}
		switch strings.Join(argv, " ") {
		case "systemctl --user is-enabled aira-daemon.service":
			return []byte("enabled\n"), nil
		case "systemctl --user start aira-daemon.service":
			*starts++
			return nil, nil
		}
		return nil, errors.New("unexpected command: " + strings.Join(argv, " "))
	}
}

// A hand-run `aira daemon` whose XDG_RUNTIME_DIR differs from the enabled
// service's, for the service's own state, would serve it as a stray while the
// service is down: the stray takes the state lock, the service exits and is
// restarted every 2 s, and the box loses the unit's watchdog. It refuses, as
// spawnDaemon does.
func TestDaemonServeFromAnotherRuntimeDirRefusesTheServicesState(t *testing.T) {
	stateHome := filepath.Join(t.TempDir(), "state")
	serviceRuntime := shortRuntimeDir(t)
	t.Setenv("XDG_STATE_HOME", stateHome)
	t.Setenv("XDG_RUNTIME_DIR", shortRuntimeDir(t))
	t.Setenv("AIRA_DAEMON_MANAGED", "")
	starts := 0
	managedUnitServing(t, stateHome, serviceRuntime, &starts)
	service, err := daemon.PathsFromEnvironment(stateHome, serviceRuntime, "")
	if err != nil {
		t.Fatal(err)
	}
	originalServe := serveDaemon
	t.Cleanup(func() { serveDaemon = originalServe })
	serves := 0
	serveDaemon = func(context.Context, daemon.Paths) error { serves++; return nil }
	var stderr bytes.Buffer
	exit := runDaemonCommand([]string{"serve"}, os.Stdout, &stderr)
	if exit != 4 || serves != 0 || starts != 0 {
		t.Fatalf("exit=%d serves=%d starts=%d stderr=%q, want a refusal (exit 4) that serves and starts nothing", exit, serves, starts, stderr.String())
	}
	for _, want := range []string{daemon.CodeUnavailable, service.SocketPath, "XDG_RUNTIME_DIR"} {
		if !strings.Contains(stderr.String(), want) {
			t.Fatalf("stderr %q lacks %q", stderr.String(), want)
		}
	}
}

// holdStateLockFor plays a daemon with the given pid that holds the state lock.
func holdStateLockFor(t *testing.T, paths daemon.Paths, pid int, socket string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(paths.LockPath), 0o700); err != nil {
		t.Fatal(err)
	}
	lock, err := os.OpenFile(paths.LockPath, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if err := unix.Flock(int(lock.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = lock.Close() })
	bootID, _ := os.ReadFile("/proc/sys/kernel/random/boot_id")
	if err := json.NewEncoder(lock).Encode(daemon.LockInfo{PID: pid, BootID: strings.TrimSpace(string(bootID)), Socket: socket}); err != nil {
		t.Fatal(err)
	}
}

// Since the daemon lock moved to the state directory, `aira daemon stop` from a
// process whose XDG_RUNTIME_DIR differs from the service's reads the service's
// own lock record. It must redirect to systemctl, as a correctly configured
// shell is redirected, and never SIGTERM the service's daemon (which every
// session shares). The "daemon" here is a child `sleep` of this test.
func TestDaemonStopFromAnotherRuntimeDirNeverSignalsTheServicesDaemon(t *testing.T) {
	stateHome := filepath.Join(t.TempDir(), "state")
	serviceRuntime := shortRuntimeDir(t)
	t.Setenv("XDG_STATE_HOME", stateHome)
	t.Setenv("XDG_RUNTIME_DIR", shortRuntimeDir(t))
	starts := 0
	managedUnitServing(t, stateHome, serviceRuntime, &starts)
	service, err := daemon.PathsFromEnvironment(stateHome, serviceRuntime, "")
	if err != nil {
		t.Fatal(err)
	}
	stand := exec.Command("sleep", "30")
	if err := stand.Start(); err != nil {
		t.Fatal(err)
	}
	exited := make(chan struct{})
	var waitErr error
	go func() { waitErr = stand.Wait(); close(exited) }()
	t.Cleanup(func() {
		_ = stand.Process.Kill()
		<-exited
	})
	holdStateLockFor(t, service, stand.Process.Pid, service.SocketPath)
	var stderr bytes.Buffer
	exit := runDaemonCommand([]string{"stop"}, os.Stdout, &stderr)
	select {
	case <-exited:
		var status *exec.ExitError
		if errors.As(waitErr, &status) {
			if ws, ok := status.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
				t.Fatalf("daemon stop sent %v to the service's daemon", ws.Signal())
			}
		}
		t.Fatalf("the service's daemon exited: %v", waitErr)
	case <-time.After(200 * time.Millisecond):
	}
	if exit == 0 || !strings.Contains(stderr.String(), "systemctl --user stop aira-daemon.service") {
		t.Fatalf("exit=%d stderr=%q, want the systemctl redirect", exit, stderr.String())
	}
}
