// verifies: AIRA-289

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/unix"

	"aira/internal/codes"
	"aira/internal/daemon"
)

// oneStateTwoRuntimes returns the paths of two processes that share one state
// directory (one state.db) and disagree about XDG_RUNTIME_DIR.
func oneStateTwoRuntimes(t *testing.T) (a, b daemon.Paths, runtimeA, runtimeB string) {
	t.Helper()
	state := filepath.Join(t.TempDir(), "state")
	runtimeA, runtimeB = shortRuntimeDir(t), shortRuntimeDir(t)
	var err error
	if a, err = daemon.PathsFromEnvironment(state, runtimeA, ""); err != nil {
		t.Fatal(err)
	}
	if b, err = daemon.PathsFromEnvironment(state, runtimeB, ""); err != nil {
		t.Fatal(err)
	}
	if a.StateID != b.StateID || a.SocketPath == b.SocketPath {
		t.Fatalf("fixture: want one state, two sockets: %+v %+v", a, b)
	}
	return a, b, runtimeA, runtimeB
}

func noSpawn(t *testing.T) func() (<-chan childResult, error) {
	return func() (<-chan childResult, error) {
		t.Error("the client started a daemon; it must never start a second one for a state that has one")
		return nil, errors.New("test: spawn must not be called")
	}
}

func TestClientWithAMismatchedRuntimeDirFailsLoudly(t *testing.T) {
	pathsA, pathsB, _, runtimeB := oneStateTwoRuntimes(t)
	startCommandDaemon(t, daemon.NewServer(pathsA))
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
	if err == nil {
		t.Fatal("a client with another runtime dir reached no daemon yet reported success")
	}
	response := transportErrorResponse(err)
	if response.Code != daemon.CodeUnavailable || response.Exit != 4 {
		t.Fatalf("code=%q exit=%d (%v), want %s exit 4", response.Code, response.Exit, err, daemon.CodeUnavailable)
	}
	for _, want := range []string{pathsB.SocketPath, pathsA.SocketPath, strconv.Itoa(os.Getpid()), "XDG_RUNTIME_DIR", runtimeB} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("message %q lacks %q", err.Error(), want)
		}
	}
	if !daemon.IsRequestNotSent(err) {
		t.Fatalf("the request was never sent but the error does not say so: %v", err)
	}
	if _, statErr := os.Lstat(pathsB.SocketPath); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("a socket exists at the mismatched runtime dir (err=%v)", statErr)
	}
}

func TestClientWithAnUnsetRuntimeDirSaysUnset(t *testing.T) {
	pathsA, pathsB, _, _ := oneStateTwoRuntimes(t)
	startCommandDaemon(t, daemon.NewServer(pathsA))
	d := &daemonDispatcher{paths: pathsB, startWait: 500 * time.Millisecond, lockWait: time.Second, spawn: noSpawn(t),
		getenv: func(string) string { return "" }}
	_, err := d.exchangeOrStart(context.Background(), daemon.RequestFrame{Proto: daemon.ProtocolVersion})
	if err == nil || !strings.Contains(err.Error(), "XDG_RUNTIME_DIR (unset)") {
		t.Fatalf("err=%v, want a message saying XDG_RUNTIME_DIR is unset", err)
	}
}

// holdStateLock plays a daemon that has won the lock and has not bound its
// socket yet: the restart / slow database-open window.
func holdStateLock(t *testing.T, paths daemon.Paths, socket string) {
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
	if err := json.NewEncoder(lock).Encode(daemon.LockInfo{PID: os.Getpid(), BootID: strings.TrimSpace(string(bootID)), Socket: socket}); err != nil {
		t.Fatal(err)
	}
}

func TestClientWaitingOnASlowDaemonKeepsTheTimeout(t *testing.T) {
	_, paths, _, runtimeB := oneStateTwoRuntimes(t)
	holdStateLock(t, paths, paths.SocketPath)
	d := &daemonDispatcher{paths: paths, startWait: 400 * time.Millisecond, lockWait: time.Second, spawn: noSpawn(t),
		getenv: func(string) string { return runtimeB }}
	_, err := d.exchangeOrStart(context.Background(), daemon.RequestFrame{Proto: daemon.ProtocolVersion})
	if err == nil {
		t.Fatal("no daemon is listening, yet the client reported success")
	}
	response := transportErrorResponse(err)
	if response.Code != daemon.CodeTimeout || response.Exit != 3 {
		t.Fatalf("code=%q exit=%d (%v), want %s exit 3", response.Code, response.Exit, err, daemon.CodeTimeout)
	}
	if !strings.Contains(err.Error(), strconv.Itoa(os.Getpid())) {
		t.Fatalf("the timeout does not name the lock holder: %v", err)
	}
	if strings.Contains(err.Error(), "XDG_RUNTIME_DIR") {
		t.Fatalf("a daemon that is merely starting was blamed on XDG_RUNTIME_DIR: %v", err)
	}
}

func TestClientOnAFreshMachineCreatesTheStateDirAndStarts(t *testing.T) {
	_, paths, _, _ := oneStateTwoRuntimes(t)
	lockDir := filepath.Dir(paths.LockPath)
	if _, err := os.Lstat(lockDir); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("fixture: %s exists (err=%v)", lockDir, err)
	}
	var started atomic.Bool
	d := &daemonDispatcher{paths: paths, startWait: 5 * time.Second, lockWait: time.Second}
	d.exchange = func(_ context.Context, socket string, _ daemon.RequestFrame) (daemon.ResponseFrame, error) {
		conn, err := net.DialTimeout("unix", socket, 100*time.Millisecond)
		if err != nil {
			return daemon.ResponseFrame{}, fmt.Errorf("%s: %w", daemon.CodeUnavailable, err)
		}
		_ = conn.Close()
		return daemon.ResponseFrame{Proto: daemon.ProtocolVersion, OK: true, Code: "OK"}, nil
	}
	d.spawn = func() (<-chan childResult, error) {
		started.Store(true)
		startCommandDaemon(t, daemon.NewServer(paths))
		done := make(chan childResult, 1)
		done <- childResult{}
		return done, nil
	}
	response, err := d.exchangeOrStart(context.Background(), daemon.RequestFrame{Proto: daemon.ProtocolVersion})
	if err != nil || !response.OK || !started.Load() {
		t.Fatalf("response=%+v err=%v started=%v", response, err, started.Load())
	}
}

// --- the measured shape: real client processes, one real daemon ---------------

// buildAira compiles the real CLI for a subprocess test.
func buildAira(t *testing.T) string {
	t.Helper()
	binary := filepath.Join(t.TempDir(), "aira")
	build := exec.Command("go", "build", "-o", binary, "aira/cmd/aira")
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build aira binary: %v\n%s", err, output)
	}
	return binary
}

// daemonPIDs lists the live `<binary> daemon` processes of exactly this binary.
func daemonPIDs(t *testing.T, binary string) []int {
	t.Helper()
	entries, err := os.ReadDir("/proc")
	if err != nil {
		t.Fatal(err)
	}
	var pids []int
	for _, entry := range entries {
		pid, err := strconv.Atoi(entry.Name())
		if err != nil {
			continue
		}
		exe, err := os.Readlink(filepath.Join("/proc", entry.Name(), "exe"))
		if err != nil || exe != binary {
			continue
		}
		cmdline, err := os.ReadFile(filepath.Join("/proc", entry.Name(), "cmdline"))
		if err != nil {
			continue
		}
		if args := strings.Split(strings.TrimRight(string(cmdline), "\x00"), "\x00"); len(args) >= 2 && args[1] == "daemon" {
			pids = append(pids, pid)
		}
	}
	return pids
}

// TestTwoClientsWithDifferentRuntimeDirsGetOneDaemon is AIRA-289 as measured: a
// first client starts a daemon; a second client with the same XDG_STATE_HOME and
// another XDG_RUNTIME_DIR (the shape of a gate command whose env_allow omits it)
// used to start a second daemon on the same state.db and have its writes
// accepted. It must instead fail loudly, and exactly one daemon must exist.
func TestTwoClientsWithDifferentRuntimeDirsGetOneDaemon(t *testing.T) {
	if testing.Short() {
		t.Skip("builds the aira binary and starts a real daemon")
	}
	binary := buildAira(t)
	base := t.TempDir()
	state := filepath.Join(base, "state")
	home := filepath.Join(base, "home")
	runtimeA, runtimeB := shortRuntimeDir(t), shortRuntimeDir(t)
	project := filepath.Join(base, "project")
	for _, dir := range []string{home, project} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := exec.Command("git", "-C", project, "init", "-q").Run(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		for _, pid := range daemonPIDs(t, binary) {
			_ = syscall.Kill(pid, syscall.SIGTERM)
		}
		deadline := time.Now().Add(5 * time.Second)
		for time.Now().Before(deadline) && len(daemonPIDs(t, binary)) > 0 {
			time.Sleep(50 * time.Millisecond)
		}
		for _, pid := range daemonPIDs(t, binary) {
			_ = syscall.Kill(pid, syscall.SIGKILL)
		}
	})
	run := func(runtime string, args ...string) (string, int) {
		t.Helper()
		ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		defer cancel()
		command := exec.CommandContext(ctx, binary, args...)
		command.Dir = project
		command.Env = []string{"PATH=" + os.Getenv("PATH"), "HOME=" + home, "XDG_STATE_HOME=" + state,
			"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@invalid", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@invalid",
			"AIRA_DAEMON_SCOPE_REAP_INTERVAL=disabled", "AIRA_DAEMON_REAP_INTERVAL=disabled"}
		if runtime != "" {
			command.Env = append(command.Env, "XDG_RUNTIME_DIR="+runtime)
		}
		var out bytes.Buffer
		command.Stdout, command.Stderr = &out, &out
		err := command.Run()
		exit := 0
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			exit = exitErr.ExitCode()
		} else if err != nil {
			t.Fatalf("run %v: %v", args, err)
		}
		return out.String(), exit
	}
	if out, exit := run(runtimeA, "init", "--project", "demo", "--prefix", "PW"); exit != 0 {
		t.Fatalf("init exit=%d: %s", exit, out)
	}
	if out, exit := run(runtimeA, "create", "first"); exit != 0 {
		t.Fatalf("create via runtime A exit=%d: %s", exit, out)
	}
	if got := len(daemonPIDs(t, binary)); got != 1 {
		t.Fatalf("after the first client: %d daemons, want 1", got)
	}
	out, exit := run(runtimeB, "create", "second")
	if exit != codes.ExitForCode(daemon.CodeUnavailable) || !strings.Contains(out, daemon.CodeUnavailable) || !strings.Contains(out, "XDG_RUNTIME_DIR") {
		t.Fatalf("second client (other runtime dir): exit=%d out=%q, want %s exit %d naming XDG_RUNTIME_DIR",
			exit, out, daemon.CodeUnavailable, codes.ExitForCode(daemon.CodeUnavailable))
	}
	if got := len(daemonPIDs(t, binary)); got != 1 {
		t.Fatalf("after the second client: %d daemons on one state, want 1", got)
	}
	if out, exit := run("", "create", "third"); exit != codes.ExitForCode(daemon.CodeUnavailable) {
		t.Fatalf("client with no XDG_RUNTIME_DIR: exit=%d out=%q", exit, out)
	}
	if got := len(daemonPIDs(t, binary)); got != 1 {
		t.Fatalf("after the unset-runtime client: %d daemons, want 1", got)
	}
}
