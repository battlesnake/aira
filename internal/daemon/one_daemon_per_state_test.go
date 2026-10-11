// verifies: AIRA-289

package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"golang.org/x/sys/unix"

	"aira/internal/testdeadline"
)

// stateAndRuntimes returns one state home and n distinct runtime directories:
// the shape of processes that share a state.db but disagree about
// XDG_RUNTIME_DIR (a gate command whose env_allow lacks it, cron, ssh).
func stateAndRuntimes(t *testing.T, n int) (string, []string) {
	t.Helper()
	state := filepath.Join(t.TempDir(), "state")
	runtimes := make([]string, n)
	for i := range runtimes {
		runtimes[i] = shortRuntimeDir(t)
	}
	return state, runtimes
}

func pathsFor(t *testing.T, state, runtime string) Paths {
	t.Helper()
	paths, err := PathsFromEnvironment(state, runtime, "")
	if err != nil {
		t.Fatal(err)
	}
	return paths
}

func serveOnce(t *testing.T, paths Paths) error {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), testdeadline.Wait(2*time.Second))
	defer cancel()
	return NewServer(paths).Serve(ctx)
}

func holdFlock(t *testing.T, path string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if err := unix.Flock(int(file.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		t.Fatalf("test could not take %s: %v", path, err)
	}
	t.Cleanup(func() { _ = file.Close() })
}

func lockIsFree(t *testing.T, path string) bool {
	t.Helper()
	file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	err = unix.Flock(int(file.Fd()), unix.LOCK_EX|unix.LOCK_NB)
	if err == nil {
		_ = unix.Flock(int(file.Fd()), unix.LOCK_UN)
		return true
	}
	if errors.Is(err, unix.EWOULDBLOCK) || errors.Is(err, unix.EAGAIN) {
		return false
	}
	t.Fatal(err)
	return false
}

func assertNoSocket(t *testing.T, paths Paths) {
	t.Helper()
	if _, err := os.Lstat(paths.SocketPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("a socket exists at %s (err=%v)", paths.SocketPath, err)
	}
}

func TestSecondDaemonOnTheSameStateIsRefusedWhateverTheRuntimeDir(t *testing.T) {
	state, runtimes := stateAndRuntimes(t, 2)
	pathsA, pathsB := pathsFor(t, state, runtimes[0]), pathsFor(t, state, runtimes[1])
	if pathsA.StateID != pathsB.StateID || pathsA.SocketPath == pathsB.SocketPath {
		t.Fatalf("fixture: want one state and two sockets: %+v %+v", pathsA, pathsB)
	}
	_, _ = startServer(t, NewServer(pathsA))
	if err := serveOnce(t, pathsB); !errors.Is(err, ErrAlreadyRunning) {
		t.Fatalf("second daemon on the same state, other runtime dir: err=%v, want ErrAlreadyRunning", err)
	}
	assertNoSocket(t, pathsB)
}

func TestDaemonsOnDifferentStatesBothRun(t *testing.T) {
	runtime := shortRuntimeDir(t)
	base := t.TempDir()
	paths1 := pathsFor(t, filepath.Join(base, "state1"), runtime)
	paths2 := pathsFor(t, filepath.Join(base, "state2"), runtime)
	if paths1.StateID == paths2.StateID {
		t.Fatal("fixture: want two states")
	}
	_, _ = startServer(t, NewServer(paths1))
	_, _ = startServer(t, NewServer(paths2))
}

// The mixed-version regression: a pre-v0.34 daemon holds only the lock beside
// its socket. A v0.34 daemon from the same runtime directory must see it.
func TestNewDaemonRefusesWhileALegacyDaemonHoldsTheRuntimeLock(t *testing.T) {
	state, runtimes := stateAndRuntimes(t, 1)
	paths := pathsFor(t, state, runtimes[0])
	holdFlock(t, paths.LegacyLockPath)
	if err := serveOnce(t, paths); !errors.Is(err, ErrAlreadyRunning) {
		t.Fatalf("err=%v, want ErrAlreadyRunning", err)
	}
	assertNoSocket(t, paths)
	if paths.LockPath == paths.LegacyLockPath {
		t.Fatal("the state lock and the legacy lock are the same file")
	}
	if !lockIsFree(t, paths.LockPath) {
		t.Fatal("the refused daemon kept the state lock")
	}
}

// A running daemon holds the legacy lock too, so a pre-v0.34 client sharing the
// daemon's runtime directory cannot win it and start a second daemon.
func TestRunningDaemonHoldsTheLegacyLock(t *testing.T) {
	state, runtimes := stateAndRuntimes(t, 1)
	paths := pathsFor(t, state, runtimes[0])
	_, _ = startServer(t, NewServer(paths))
	if lockIsFree(t, paths.LegacyLockPath) {
		t.Fatal("legacy lock is free while the daemon runs")
	}
	if lockIsFree(t, paths.LockPath) {
		t.Fatal("state lock is free while the daemon runs")
	}
	for _, path := range []string{paths.LockPath, paths.LegacyLockPath} {
		info := readLockInfo(path)
		if info.PID != os.Getpid() || info.Socket != paths.SocketPath {
			t.Fatalf("lock info in %s = %+v, want pid %d socket %s", path, info, os.Getpid(), paths.SocketPath)
		}
	}
}

func TestDaemonStartsWhenTheStateDirectoryDoesNotExistYet(t *testing.T) {
	state, runtimes := stateAndRuntimes(t, 1)
	paths := pathsFor(t, state, runtimes[0])
	if _, err := os.Lstat(filepath.Dir(paths.LockPath)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("fixture: the lock's directory exists (err=%v)", err)
	}
	_, _ = startServer(t, NewServer(paths))
}

func TestStatusFromAMismatchedRuntimeSeesTheLockHolder(t *testing.T) {
	state, runtimes := stateAndRuntimes(t, 2)
	pathsA, pathsB := pathsFor(t, state, runtimes[0]), pathsFor(t, state, runtimes[1])
	_, _ = startServer(t, NewServer(pathsA))
	status := Status(pathsB)
	if !status.Running || status.Ready || status.Lock.PID != os.Getpid() {
		t.Fatalf("status from the other runtime dir = %+v, want running, not ready, pid %d", status, os.Getpid())
	}
}

// closeLocksForTest releases both single-instance locks a timed-out drain
// retains (the state lock and the legacy runtime-directory lock), as a real
// daemon does by exiting.
func (e *ErrDrainTimeout) closeLocksForTest() error {
	first := e.lock.Close()
	if err := e.legacyLock.Close(); err != nil {
		return err
	}
	return first
}

// A pre-v0.34 daemon holds only the legacy lock. Status (and so `aira daemon
// stop` and `aira install`'s reachability check) must still see it as running,
// or a v0.34 `aira install` run before the cutover restart fails its verify.
func TestStatusSeesAPreV034DaemonHoldingOnlyTheLegacyLock(t *testing.T) {
	state, runtimes := stateAndRuntimes(t, 1)
	paths := pathsFor(t, state, runtimes[0])
	if err := os.MkdirAll(filepath.Dir(paths.LegacyLockPath), 0o700); err != nil {
		t.Fatal(err)
	}
	info := LockInfo{PID: 4242, BootID: currentBootID()}
	data, err := json.Marshal(info)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(paths.LegacyLockPath, data, 0o600); err != nil {
		t.Fatal(err)
	}
	holdFlock(t, paths.LegacyLockPath)
	status := Status(paths)
	if !status.Running || status.Lock.PID != 4242 {
		t.Fatalf("status = %+v, want a running pre-v0.34 daemon with pid 4242", status)
	}
}
