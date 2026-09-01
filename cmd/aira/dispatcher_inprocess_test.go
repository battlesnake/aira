package main

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"aira/internal/app"
	"aira/internal/cgrouptest"
	"aira/internal/core"
	"aira/internal/daemon"
	"aira/internal/domain"
	"aira/internal/gitcontext"
	"aira/internal/runner"
	"aira/internal/store"

	"golang.org/x/sys/unix"
)

func runInProcess(argv []string, stdout, stderr io.Writer) int {
	return runInProcessWithInput(argv, stdout, stderr, strings.NewReader(""))
}

func TestTimeCLIExitPassthroughHumanSuppressionAndStructuredRead(t *testing.T) {
	root := t.TempDir()
	if err := exec.Command("git", "-C", root, "init", "-q").Run(); err != nil {
		t.Fatal(err)
	}
	t.Chdir(root)
	t.Setenv("XDG_STATE_HOME", filepath.Join(root, "state"))
	t.Setenv("XDG_RUNTIME_DIR", shortRuntimeDir(t))
	var stdout, stderr bytes.Buffer
	if exit := runInProcess([]string{"init", "--project", "demo", "--prefix", "AIRA"}, &stdout, &stderr); exit != 0 {
		t.Fatalf("init exit=%d out=%q err=%q", exit, stdout.String(), stderr.String())
	}
	stdout.Reset()
	stderr.Reset()
	exit := runInProcess([]string{"time", "--json", "--", "sh", "-c", "exit 7"}, &stdout, &stderr)
	if exit != 7 || stderr.Len() != 0 || !strings.Contains(stdout.String(), `"status":"exited"`) || !strings.Contains(stdout.String(), `"exit_code":7`) {
		t.Fatalf("time exit=%d out=%q err=%q", exit, stdout.String(), stderr.String())
	}
	stdout.Reset()
	stderr.Reset()
	if exit := runInProcess([]string{"time", "--", "true"}, &stdout, &stderr); exit != 0 || stdout.Len() != 0 || stderr.Len() != 0 {
		t.Fatalf("human time exit=%d out=%q err=%q", exit, stdout.String(), stderr.String())
	}
	stdout.Reset()
	stderr.Reset()
	if exit := runInProcess([]string{"commands", "ls", "--json"}, &stdout, &stderr); exit != 0 || !strings.Contains(stdout.String(), `"total":2`) || !strings.Contains(stdout.String(), `"CMD-2"`) {
		t.Fatalf("commands exit=%d out=%q err=%q", exit, stdout.String(), stderr.String())
	}
}

// This discriminator needs a real AF_UNIX listener. The Codex sandbox cannot
// bind it; AIRA_REAL_SOCKET=1 is the Opus real-hardware mode and fails closed.
func TestCommandsRoutedDaemonParityAndPairedDrillRealSocket(t *testing.T) {
	if os.Getenv("AIRA_REAL_SOCKET") != "1" {
		t.Skip("real Unix-socket parity requires AIRA_REAL_SOCKET=1")
	}
	root := t.TempDir()
	if err := exec.Command("git", "-C", root, "init", "-q").Run(); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, ".aira"), 0o755); err != nil {
		t.Fatal(err)
	}
	config := `{"schema":1,"project":{"slug":"demo","prefixes":["AIRA"]},"lease":{"ttl_seconds":900,"heartbeat_seconds":30}}`
	if err := os.WriteFile(filepath.Join(root, ".aira", "config"), []byte(config+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Chdir(root)
	t.Setenv("XDG_STATE_HOME", filepath.Join(root, "state"))
	t.Setenv("XDG_RUNTIME_DIR", shortRuntimeDir(t))
	paths, err := daemon.PathsFromEnv()
	if err != nil {
		t.Fatal(err)
	}
	s, _, err := app.Open(context.Background(), root)
	if err != nil {
		t.Fatal(err)
	}
	zero, wall := int64(0), int64(12)
	_, err = s.AddCommandEvent(context.Background(), domain.CommandEventInput{Key: "go test", KeySource: domain.CommandKeyProgramSubcommand, Program: "go", Status: domain.CommandExited, ExitCode: &zero, WallMS: &wall})
	if closeErr := s.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		t.Fatal(err)
	}
	scope, err := scopeForCWD(context.Background(), root, paths)
	if err != nil {
		t.Fatal(err)
	}
	startCommandDaemon(t, daemon.NewServer(paths))
	request := core.Request{Verb: "commands", Args: map[string]any{"subverb": "ls", "query": "key-source:program-subcommand key:go test"}}
	routed := (&daemonDispatcher{paths: paths, jsonOutput: true, startWait: time.Second}).Dispatch(context.Background(), scope, request)
	inProcess := (&inProcessDispatcher{jsonOutput: true}).Dispatch(context.Background(), scope, request)
	if !routed.OK || !inProcess.OK {
		t.Fatalf("routed=%#v in-process=%#v", routed, inProcess)
	}
	normalise := func(value any) any {
		data, _ := json.Marshal(value)
		var decoded any
		_ = json.Unmarshal(data, &decoded)
		return decoded
	}
	if !reflect.DeepEqual(normalise(routed.Data), normalise(inProcess.Data)) {
		t.Fatalf("routed rows=%#v in-process rows=%#v", routed.Data, inProcess.Data)
	}
	if canonical, route := core.Classify("commands", "ls"); canonical != "commands" || route != core.RouteDaemon {
		t.Fatalf("route=%q/%v", canonical, route)
	}
}

type dispatcherFunc func(context.Context, daemon.WorktreeScope, core.Request) core.Response

func (dispatch dispatcherFunc) Dispatch(ctx context.Context, scope daemon.WorktreeScope, request core.Request) core.Response {
	return dispatch(ctx, scope, request)
}

// shortRuntimeDir returns a short unique XDG_RUNTIME_DIR. The daemon socket must
// fit the 108-byte AF_UNIX limit; t.TempDir() embeds the long test name and would
// overflow it (real /run/user/<uid> dirs are short).
func shortRuntimeDir(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "art")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return dir
}

func autoStartDispatcher(t *testing.T) *daemonDispatcher {
	t.Helper()
	base := t.TempDir()
	t.Setenv("XDG_STATE_HOME", filepath.Join(base, "state"))
	t.Setenv("XDG_RUNTIME_DIR", shortRuntimeDir(t))
	paths, err := daemon.PathsFromEnv()
	if err != nil {
		t.Fatal(err)
	}
	return &daemonDispatcher{paths: paths, startWait: time.Second, lockWait: time.Second}
}

// verifies: a child exiting zero is a lost start race and polling continues
// until the winning daemon accepts.
func TestAutoStartLosingChildExitZeroKeepsPolling(t *testing.T) {
	dispatcher := autoStartDispatcher(t)
	var calls atomic.Int32
	dispatcher.exchange = func(context.Context, string, daemon.RequestFrame) (daemon.ResponseFrame, error) {
		if calls.Add(1) >= 4 {
			return daemon.ResponseFrame{Proto: daemon.ProtocolVersion, OK: true, Code: "OK"}, nil
		}
		return daemon.ResponseFrame{}, fmt.Errorf("%s: not ready", daemon.CodeUnavailable)
	}
	dispatcher.spawn = func() (<-chan childResult, error) {
		done := make(chan childResult, 1)
		done <- childResult{}
		return done, nil
	}
	response, err := dispatcher.exchangeOrStart(context.Background(), daemon.RequestFrame{Proto: daemon.ProtocolVersion})
	if err != nil || !response.OK {
		t.Fatalf("response=%+v err=%v calls=%d", response, err, calls.Load())
	}
}

func TestAutoStartRemovesStaleSocketOnlyUnderLock(t *testing.T) {
	dispatcher := autoStartDispatcher(t)
	if err := os.MkdirAll(dispatcher.paths.RuntimeDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dispatcher.paths.SocketPath, []byte("stale"), 0o600); err != nil {
		t.Fatal(err)
	}
	var ready atomic.Bool
	dispatcher.exchange = func(context.Context, string, daemon.RequestFrame) (daemon.ResponseFrame, error) {
		if ready.Load() {
			return daemon.ResponseFrame{Proto: daemon.ProtocolVersion, OK: true, Code: "OK"}, nil
		}
		return daemon.ResponseFrame{}, fmt.Errorf("%s: stale", daemon.CodeUnavailable)
	}
	dispatcher.spawn = func() (<-chan childResult, error) {
		if _, err := os.Stat(dispatcher.paths.SocketPath); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("stale socket still exists at spawn: %v", err)
		}
		ready.Store(true)
		return make(chan childResult, 1), nil
	}
	if response, err := dispatcher.exchangeOrStart(context.Background(), daemon.RequestFrame{Proto: daemon.ProtocolVersion}); err != nil || !response.OK {
		t.Fatalf("response=%+v err=%v", response, err)
	}
}

func TestConcurrentAutoStartClientsAllObserveWinner(t *testing.T) {
	base := autoStartDispatcher(t)
	var ready atomic.Bool
	var spawns atomic.Int32
	exchange := func(context.Context, string, daemon.RequestFrame) (daemon.ResponseFrame, error) {
		if ready.Load() {
			return daemon.ResponseFrame{Proto: daemon.ProtocolVersion, OK: true, Code: "OK"}, nil
		}
		return daemon.ResponseFrame{}, fmt.Errorf("%s: starting", daemon.CodeUnavailable)
	}
	spawn := func() (<-chan childResult, error) {
		if spawns.Add(1) == 1 {
			go func() {
				time.Sleep(50 * time.Millisecond)
				ready.Store(true)
			}()
		}
		done := make(chan childResult, 1)
		done <- childResult{}
		return done, nil
	}
	const clients = 8
	var wait sync.WaitGroup
	errorsCh := make(chan error, clients)
	for range clients {
		wait.Add(1)
		go func() {
			defer wait.Done()
			dispatcher := &daemonDispatcher{paths: base.paths, startWait: time.Second, lockWait: time.Second, exchange: exchange, spawn: spawn}
			response, err := dispatcher.exchangeOrStart(context.Background(), daemon.RequestFrame{Proto: daemon.ProtocolVersion})
			if err != nil || !response.OK {
				errorsCh <- fmt.Errorf("response=%+v err=%v", response, err)
			}
		}()
	}
	wait.Wait()
	close(errorsCh)
	for err := range errorsCh {
		t.Error(err)
	}
	if spawns.Load() < 2 {
		t.Fatalf("race did not exercise losing children: spawns=%d", spawns.Load())
	}
}

// verifies: the complete auto-start race uses the real shared flock and Unix
// listener: one in-process daemon wins, losing children report exit zero, and
// every client continues polling to a successful response.
func TestConcurrentRealAutoStartHasOneReadyDaemon(t *testing.T) {
	base := autoStartDispatcher(t)
	serverContext, stopServers := context.WithCancel(context.Background())
	defer stopServers()
	var attempts atomic.Int32
	spawn := func() (<-chan childResult, error) {
		attempts.Add(1)
		done := make(chan childResult, 1)
		go func() {
			server := daemon.NewServer(base.paths)
			server.Handle = func(context.Context, daemon.WorktreeScope, core.Request) core.Response {
				return core.Response{OK: true, Code: "OK"}
			}
			err := server.Serve(serverContext)
			if errors.Is(err, daemon.ErrAlreadyRunning) {
				err = nil
			}
			done <- childResult{err: err}
		}()
		return done, nil
	}
	const clients = 8
	var wait sync.WaitGroup
	errorsCh := make(chan error, clients)
	for range clients {
		wait.Add(1)
		go func() {
			defer wait.Done()
			dispatcher := &daemonDispatcher{paths: base.paths, startWait: 3 * time.Second, lockWait: time.Second, spawn: spawn}
			response, err := dispatcher.exchangeOrStart(context.Background(), daemon.RequestFrame{
				Proto:   daemon.ProtocolVersion,
				Scope:   daemon.WorktreeScope{StateID: base.paths.StateID},
				Request: core.Request{Verb: "help"},
			})
			if err != nil || !response.OK {
				errorsCh <- fmt.Errorf("response=%+v err=%v", response, err)
			}
		}()
	}
	wait.Wait()
	close(errorsCh)
	for err := range errorsCh {
		t.Error(err)
	}
	status := daemon.Status(base.paths)
	if !status.Running || !status.Ready {
		t.Fatalf("daemon status=%+v attempts=%d", status, attempts.Load())
	}
}

func TestAutoStartChildFailureStillWaitsToBoundedTimeout(t *testing.T) {
	dispatcher := autoStartDispatcher(t)
	dispatcher.startWait = 80 * time.Millisecond
	dispatcher.exchange = func(context.Context, string, daemon.RequestFrame) (daemon.ResponseFrame, error) {
		return daemon.ResponseFrame{}, fmt.Errorf("%s: unavailable", daemon.CodeUnavailable)
	}
	dispatcher.spawn = func() (<-chan childResult, error) {
		done := make(chan childResult, 1)
		done <- childResult{err: errors.New("exit status 1"), stderr: "fixture failure"}
		return done, nil
	}
	_, err := dispatcher.exchangeOrStart(context.Background(), daemon.RequestFrame{Proto: daemon.ProtocolVersion})
	if code := store.ErrorCode(err); code != daemon.CodeTimeout {
		t.Fatalf("error=%v code=%s, want %s", err, code, daemon.CodeTimeout)
	}
}

// verifies: a spawned child exiting (a lost race) must NOT re-trigger a fork; the
// client polls the winner's socket instead. Guards against an auto-start fork storm.
func TestAutoStartSpawnsAtMostOnceDespiteChildExit(t *testing.T) {
	dispatcher := autoStartDispatcher(t)
	dispatcher.startWait = 200 * time.Millisecond
	var spawns atomic.Int32
	dispatcher.exchange = func(context.Context, string, daemon.RequestFrame) (daemon.ResponseFrame, error) {
		return daemon.ResponseFrame{}, fmt.Errorf("%s: never ready", daemon.CodeUnavailable)
	}
	dispatcher.spawn = func() (<-chan childResult, error) {
		spawns.Add(1)
		done := make(chan childResult, 1)
		done <- childResult{} // child exits 0 immediately (lost race)
		return done, nil
	}
	_, err := dispatcher.exchangeOrStart(context.Background(), daemon.RequestFrame{Proto: daemon.ProtocolVersion})
	if code := store.ErrorCode(err); code != daemon.CodeTimeout {
		t.Fatalf("want %s, got %v", daemon.CodeTimeout, err)
	}
	if got := spawns.Load(); got != 1 {
		t.Fatalf("spawned %d times; a child exit must not re-trigger a fork (want 1)", got)
	}
}

// verifies: an already-cancelled context never mutates (removes the socket) nor
// launches a daemon.
func TestAutoStartPreCancelledContextDoesNotSpawn(t *testing.T) {
	dispatcher := autoStartDispatcher(t)
	var spawns atomic.Int32
	dispatcher.exchange = func(context.Context, string, daemon.RequestFrame) (daemon.ResponseFrame, error) {
		return daemon.ResponseFrame{}, fmt.Errorf("%s: down", daemon.CodeUnavailable)
	}
	dispatcher.spawn = func() (<-chan childResult, error) {
		spawns.Add(1)
		return make(chan childResult, 1), nil
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := dispatcher.exchangeOrStart(ctx, daemon.RequestFrame{Proto: daemon.ProtocolVersion})
	if code := store.ErrorCode(err); code != daemon.CodeTimeout {
		t.Fatalf("want %s on a cancelled context, got %v", daemon.CodeTimeout, err)
	}
	if got := spawns.Load(); got != 0 {
		t.Fatalf("spawned %d times under a pre-cancelled context; must not launch (want 0)", got)
	}
}

const protocolDaemonHelperEnv = "AIRA_PROTOCOL_DAEMON_HELPER"

// TestProtocolDaemonHelperProcess holds a real daemon lock until SIGTERM. It
// models the process-lifetime part of protocol replacement without requiring a
// Unix listener, which is unavailable in the restricted test sandbox.
func TestProtocolDaemonHelperProcess(t *testing.T) {
	if os.Getenv(protocolDaemonHelperEnv) != "1" {
		return
	}
	paths, err := daemon.PathsFromEnv()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(paths.RuntimeDir, 0o700); err != nil {
		t.Fatal(err)
	}
	lock, err := os.OpenFile(paths.LockPath, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()
	if err := unix.Flock(int(lock.Fd()), unix.LOCK_EX); err != nil {
		t.Fatal(err)
	}
	defer unix.Flock(int(lock.Fd()), unix.LOCK_UN)
	bootID, err := os.ReadFile("/proc/sys/kernel/random/boot_id")
	if err != nil {
		t.Fatal(err)
	}
	if err := json.NewEncoder(lock).Encode(daemon.LockInfo{PID: os.Getpid(), BootID: strings.TrimSpace(string(bootID))}); err != nil {
		t.Fatal(err)
	}
	if _, err := io.WriteString(os.Stdout, "R"); err != nil {
		t.Fatal(err)
	}
	signals := make(chan os.Signal, 1)
	signal.Notify(signals, syscall.SIGTERM)
	defer signal.Stop(signals)
	<-signals
}

type protocolDaemonProcess struct {
	command *exec.Cmd
	done    <-chan struct{}
}

func startProtocolDaemonProcess(t *testing.T) protocolDaemonProcess {
	t.Helper()
	command := exec.Command(os.Args[0], "-test.run=^TestProtocolDaemonHelperProcess$")
	command.Env = append(os.Environ(), protocolDaemonHelperEnv+"=1")
	stdout, err := command.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	command.Stderr = os.Stderr
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	ready := make(chan error, 1)
	go func() {
		var marker [1]byte
		_, err := io.ReadFull(stdout, marker[:])
		if err == nil && marker[0] != 'R' {
			err = fmt.Errorf("unexpected helper marker %q", marker[0])
		}
		ready <- err
	}()
	select {
	case err := <-ready:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		_ = command.Process.Kill()
		t.Fatal("protocol daemon helper did not become ready")
	}
	done := make(chan struct{})
	go func() {
		_ = command.Wait()
		close(done)
	}()
	process := protocolDaemonProcess{command: command, done: done}
	t.Cleanup(func() {
		select {
		case <-process.done:
			return
		default:
		}
		_ = process.command.Process.Signal(syscall.SIGTERM)
		select {
		case <-process.done:
		case <-time.After(5 * time.Second):
			_ = process.command.Process.Kill()
			<-process.done
		}
	})
	return process
}

func TestNewerClientReplacesOlderProtocolDaemon(t *testing.T) {
	dispatcher := autoStartDispatcher(t)
	older := startProtocolDaemonProcess(t)
	var exchanges atomic.Int32
	dispatcher.exchange = func(context.Context, string, daemon.RequestFrame) (daemon.ResponseFrame, error) {
		if exchanges.Add(1) == 1 {
			return daemon.ResponseFrame{Proto: daemon.ProtocolVersion - 1, Code: daemon.CodeProtocol, Error: daemon.CodeProtocol + ": older daemon"}, nil
		}
		return daemon.ResponseFrame{Proto: daemon.ProtocolVersion, OK: true, Code: "OK"}, nil
	}
	spawned := false
	dispatcher.spawn = func() (<-chan childResult, error) {
		spawned = true
		return nil, errors.New("must not spawn")
	}
	response := dispatcher.Dispatch(context.Background(), daemon.WorktreeScope{}, core.Request{Verb: "list"})
	if !response.OK || response.Code != "OK" || spawned || exchanges.Load() != 2 {
		t.Fatalf("response=%+v spawned=%v exchanges=%d", response, spawned, exchanges.Load())
	}
	select {
	case <-older.done:
	case <-time.After(5 * time.Second):
		t.Fatal("older protocol daemon was not stopped for replacement")
	}
}

func TestProtocolFiveComputeGitContextStoreOpReplacesLiveProtocolFourDaemon(t *testing.T) {
	if daemon.ProtocolVersion != 5 {
		t.Fatalf("protocol=%d want=5", daemon.ProtocolVersion)
	}
	dispatcher := autoStartDispatcher(t)
	older := startProtocolDaemonProcess(t)
	input := domain.ComputeEventInput{
		Model: "gpt", Provider: "openai", Source: "manual",
		GitContext: gitcontext.GitContext{HeadHash: gitcontext.Field{Value: "abc123", Status: gitcontext.StatusValue}},
	}
	frame, err := daemon.NewJSONStoreOp(daemon.WorktreeScope{}, "add-compute-event", input)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(frame.Payload, []byte(`"GitContext"`)) {
		t.Fatalf("compute payload omitted git context: %s", frame.Payload)
	}
	var exchanges atomic.Int32
	dispatcher.storeOpExchange = func(context.Context, string, daemon.StoreOpFrame) (daemon.ResponseFrame, error) {
		if exchanges.Add(1) == 1 {
			return daemon.ResponseFrame{Proto: 4, Code: daemon.CodeProtocol, Error: daemon.CodeProtocol + ": protocol-4 daemon"}, nil
		}
		return daemon.ResponseFrame{Proto: 5, OK: true, Code: "OK"}, nil
	}
	response, err := dispatcher.exchangeWithReplacement(context.Background(), func(ctx context.Context) (daemon.ResponseFrame, error) {
		return dispatcher.exchangeOrStartStoreOp(ctx, frame)
	})
	if err != nil || !response.OK || exchanges.Load() != 2 {
		t.Fatalf("response=%+v err=%v exchanges=%d", response, err, exchanges.Load())
	}
	select {
	case <-older.done:
	case <-time.After(5 * time.Second):
		t.Fatal("live protocol-4 daemon was not stopped before retrying compute git-context op")
	}
}

func TestRunGitContextStampingUsesLaunchWorktreeAndSkipsStoreFreeRuns(t *testing.T) {
	root := t.TempDir()
	if err := exec.Command("git", "init", "-q", root).Run(); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "tracked.txt"), []byte("launch\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, argv := range [][]string{
		{"git", "-C", root, "add", "tracked.txt"},
		{"git", "-C", root, "-c", "user.name=Terra", "-c", "user.email=terra@example.test", "commit", "-qm", "launch"},
	} {
		if output, err := exec.Command(argv[0], argv[1:]...).CombinedOutput(); err != nil {
			t.Fatalf("%v: %v: %s", argv, err, output)
		}
	}
	wantHead, err := exec.Command("git", "-C", root, "rev-parse", "HEAD").Output()
	if err != nil {
		t.Fatal(err)
	}
	scope := daemon.WorktreeScope{Root: root, CommonDir: filepath.Join(root, ".git"), GitDir: filepath.Join(root, ".git"), WorktreeID: "main"}
	inGit := core.Request{Verb: "run", Args: map[string]any{"tool": "codex"}}
	stampGitContext(scope, &inGit)
	if inGit.GitContext == nil || inGit.GitContext.HeadHash.Status != gitcontext.StatusValue || inGit.GitContext.HeadHash.Value != strings.TrimSpace(string(wantHead)) {
		t.Fatalf("in-git launch context=%#v want HEAD=%q", inGit.GitContext, strings.TrimSpace(string(wantHead)))
	}

	outsideRoot := t.TempDir()
	outsideGit := filepath.Join(outsideRoot, "unreadable-git")
	if err := os.MkdirAll(filepath.Join(outsideGit, "HEAD"), 0o755); err != nil {
		t.Fatal(err)
	}
	outside := core.Request{Verb: "run", Args: map[string]any{"tool": "codex"}}
	stampGitContext(daemon.WorktreeScope{Root: outsideRoot, CommonDir: outsideGit, GitDir: outsideGit, WorktreeID: "outside"}, &outside)
	if outside.GitContext == nil || outside.GitContext.HeadHash.Status != gitcontext.StatusUnevaluated || outside.GitContext.HeadHash.Value != "" {
		t.Fatalf("outside-git launch context=%#v", outside.GitContext)
	}

	storeFree := core.Request{Verb: "run", Args: map[string]any{}}
	stampGitContext(daemon.WorktreeScope{}, &storeFree)
	if storeFree.GitContext != nil {
		t.Fatalf("store-free run invoked resolver: %#v", storeFree.GitContext)
	}
}

func TestOlderClientNeverReplacesNewerDaemon(t *testing.T) {
	dispatcher := autoStartDispatcher(t)
	newer := startProtocolDaemonProcess(t)
	spawned := false
	dispatcher.exchange = func(context.Context, string, daemon.RequestFrame) (daemon.ResponseFrame, error) {
		return daemon.ResponseFrame{Proto: daemon.ProtocolVersion + 1, Code: daemon.CodeProtocol, Error: daemon.CodeProtocol + ": newer daemon"}, nil
	}
	dispatcher.spawn = func() (<-chan childResult, error) {
		spawned = true
		return nil, errors.New("must not spawn")
	}
	response := dispatcher.Dispatch(context.Background(), daemon.WorktreeScope{}, core.Request{Verb: "list"})
	if response.Code != daemon.CodeProtocol || !strings.HasPrefix(response.Error, daemon.CodeProtocol) || spawned {
		t.Fatalf("response=%+v spawned=%v", response, spawned)
	}
	select {
	case <-newer.done:
		t.Fatal("newer protocol daemon was stopped by an older client")
	default:
	}
}

func TestStoreFreeCarvedDispatchUsesEnsureScopeWithoutOpeningClientStore(t *testing.T) {
	dispatcher, scope, stateHome := storeFreeDispatcherFixture(t)
	var handshakes atomic.Int32
	dispatcher.storeOpExchange = func(_ context.Context, _ string, frame daemon.StoreOpFrame) (daemon.ResponseFrame, error) {
		handshakes.Add(1)
		if frame.Proto != daemon.ProtocolVersion || frame.Op != "ensure-scope" || frame.Scope.WorktreeID != scope.WorktreeID {
			t.Fatalf("ensure-scope frame = %+v", frame)
		}
		return daemon.ResponseFrame{OK: true, Code: "OK"}, nil
	}
	requests := []core.Request{
		{Verb: "run", Args: map[string]any{"argv": []string{"/bin/true"}, "no_admit": true}},
		{Verb: "run-kill", Args: map[string]any{"run_id": "RUN-missing"}},
		{Verb: "run-log", Args: map[string]any{"run_id": "RUN-missing"}},
		{Verb: "show", Args: map[string]any{"selector": "RUN-missing"}},
		{Verb: "get", Args: map[string]any{"selector": "RUN-missing"}},
		{Verb: "git", Args: map[string]any{"subverb": "invalid"}},
	}
	for _, request := range requests {
		response := dispatcher.Dispatch(context.Background(), scope, request)
		if response.Code == daemon.CodeInternal || strings.Contains(response.Error, "unexpectedly used the store") {
			t.Fatalf("request %+v reached store guard: %+v", request, response)
		}
	}
	if got := handshakes.Load(); got != int32(len(requests)) {
		t.Fatalf("ensure-scope handshakes = %d, want %d", got, len(requests))
	}
	for _, name := range []string{"state.db", "state.db-wal", "state.db-shm", "registry.jsonl"} {
		path := filepath.Join(stateHome, "aira", name)
		if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("store-free dispatch created client %s: %v", path, err)
		}
	}
}

func TestStoreTouchingCarvedDispatchUsesReadOnlyClientAndRelaysCommandWrite(t *testing.T) {
	dispatcher, scope, stateHome := storeFreeDispatcherFixture(t)
	var wiringObserved bool
	dispatcher.afterRelayWiring = func(storeRunner, supervisorLeaseReader bool) {
		wiringObserved = true
		if !storeRunner || !supervisorLeaseReader {
			t.Fatalf("relay wiring store_runner=%v supervisor_lease_reader=%v", storeRunner, supervisorLeaseReader)
		}
	}
	db, err := store.OpenDB(dispatcher.paths.DBPath, dispatcher.paths.RegistryPath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var view *store.Store
	var ops []string
	dispatcher.storeOpExchange = func(_ context.Context, _ string, frame daemon.StoreOpFrame) (daemon.ResponseFrame, error) {
		ops = append(ops, frame.Op)
		switch frame.Op {
		case "ensure-scope":
			var scopeErr error
			view, scopeErr = store.NewScope(db, store.ScopeOptions{
				Root: frame.Scope.Root, CommonDir: frame.Scope.CommonDir, GitDir: frame.Scope.GitDir,
				ProjectID: frame.Scope.ProjectID, WorktreeID: frame.Scope.WorktreeID,
				ProjectSlug: frame.Scope.Slug, Prefixes: frame.Scope.Prefixes, ConfigDigest: frame.Scope.ConfigDigest,
			})
			if scopeErr != nil {
				t.Fatal(scopeErr)
			}
			return daemon.ResponseFrame{OK: true, Code: "OK"}, nil
		case "add-command-event":
			data, marshalErr := json.Marshal(validCommandEventAddResult("CMD-relayed"))
			if marshalErr != nil {
				t.Fatal(marshalErr)
			}
			return daemon.ResponseFrame{OK: true, Code: "OK", Data: data}, nil
		default:
			t.Fatalf("unexpected store op %+v", frame)
			return daemon.ResponseFrame{}, nil
		}
	}
	response := dispatcher.Dispatch(context.Background(), scope, core.Request{Verb: "time", Args: map[string]any{
		"argv": []string{"/bin/true"}, "no_prefix": true,
	}})
	if !response.OK || len(ops) != 2 || ops[0] != "ensure-scope" || ops[1] != "add-command-event" {
		t.Fatalf("response=%+v ops=%v", response, ops)
	}
	if view == nil {
		t.Fatal("daemon scope was not established")
	}
	if !wiringObserved {
		t.Fatal("store-touching branch did not complete relay runner wiring")
	}
	rows, err := view.ListCommandEvents("")
	if err != nil || len(rows) != 0 {
		t.Fatalf("foreground client wrote command rows=%+v err=%v", rows, err)
	}
	registry, err := os.ReadFile(filepath.Join(stateHome, "aira", "registry.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	if lines := strings.Count(strings.TrimSpace(string(registry)), "\n") + 1; lines != 1 {
		t.Fatalf("client registered locally; registry lines=%d", lines)
	}
}

func TestEnsureScopeFailurePreventsStoreFreeVerbExecution(t *testing.T) {
	dispatcher, scope, stateHome := storeFreeDispatcherFixture(t)
	dispatcher.storeOpExchange = func(context.Context, string, daemon.StoreOpFrame) (daemon.ResponseFrame, error) {
		return daemon.ResponseFrame{Code: "E_PREFIX_OWNERSHIP_CONFLICT", Error: "E_PREFIX_OWNERSHIP_CONFLICT: SHARED owned by another project", Exit: 1}, nil
	}
	marker := filepath.Join(scope.Root, "must-not-run")
	response := dispatcher.Dispatch(context.Background(), scope, core.Request{Verb: "run", Args: map[string]any{
		"argv": []string{"/bin/sh", "-c", "touch must-not-run"}, "no_admit": true,
	}})
	if response.Code != "E_PREFIX_OWNERSHIP_CONFLICT" || response.Exit != 1 {
		t.Fatalf("ownership response = %+v", response)
	}
	if _, err := os.Stat(marker); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("verb ran after failed ensure-scope: %v", err)
	}
	if _, err := os.Stat(filepath.Join(stateHome, "aira", "state.db")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("failed ensure-scope opened client store: %v", err)
	}
}

func TestStoreFreeDispatchFailsClosedWhenConfigChangesDuringHandshake(t *testing.T) {
	dispatcher, originalScope, _ := storeFreeDispatcherFixture(t)
	uniqueConfig := `{"schema":1,"project":{"slug":"demo","prefixes":["UNIQUE"]},"lease":{"ttl_seconds":900,"heartbeat_seconds":30}}`
	configPath := filepath.Join(originalScope.Root, ".aira", "config")
	if err := os.WriteFile(configPath, []byte(uniqueConfig+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	scope, err := scopeForCWD(context.Background(), originalScope.Root, dispatcher.paths)
	if err != nil {
		t.Fatal(err)
	}

	owner := t.TempDir()
	if err := exec.Command("git", "-C", owner, "init", "-q").Run(); err != nil {
		t.Fatal(err)
	}
	if _, err := app.Init(context.Background(), owner, map[string]any{"project": "owner", "prefixes": "SHARED"}); err != nil {
		t.Fatal(err)
	}

	sharedConfig := `{"schema":1,"project":{"slug":"demo","prefixes":["SHARED"]},"lease":{"ttl_seconds":900,"heartbeat_seconds":30}}`
	dispatcher.storeOpExchange = func(context.Context, string, daemon.StoreOpFrame) (daemon.ResponseFrame, error) {
		if err := os.WriteFile(configPath, []byte(sharedConfig+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		return daemon.ResponseFrame{OK: true, Code: "OK"}, nil
	}
	marker := filepath.Join(originalScope.Root, "must-not-run")
	response := dispatcher.Dispatch(context.Background(), scope, core.Request{Verb: "run", Args: map[string]any{
		"argv": []string{"/usr/bin/touch", marker}, "no_admit": true,
	}})
	if response.Code != daemon.CodeProjectInvalid || !strings.HasPrefix(response.Error, daemon.CodeProjectInvalid+":") {
		t.Fatalf("snapshot mismatch response = %+v", response)
	}
	if _, err := os.Stat(marker); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("store-free command executed after project snapshot diverged: %v", err)
	}
}

func TestEnsureScopeFrameReplacesOlderProtocolDaemon(t *testing.T) {
	dispatcher := autoStartDispatcher(t)
	older := startProtocolDaemonProcess(t)
	root, scope := writeStoreFreeProject(t, dispatcher.paths)
	t.Chdir(root)
	var exchanges atomic.Int32
	dispatcher.storeOpExchange = func(context.Context, string, daemon.StoreOpFrame) (daemon.ResponseFrame, error) {
		if exchanges.Add(1) == 1 {
			return daemon.ResponseFrame{Proto: daemon.ProtocolVersion - 1, Code: daemon.CodeProtocol, Error: daemon.CodeProtocol + ": older daemon"}, nil
		}
		return daemon.ResponseFrame{OK: true, Code: "OK"}, nil
	}
	response := dispatcher.Dispatch(context.Background(), scope, core.Request{Verb: "show", Args: map[string]any{"selector": "RUN-missing"}})
	if response.Code == daemon.CodeProtocol || exchanges.Load() != 2 {
		t.Fatalf("response=%+v exchanges=%d", response, exchanges.Load())
	}
	select {
	case <-older.done:
	case <-time.After(5 * time.Second):
		t.Fatal("older protocol daemon was not stopped for ensure-scope replacement")
	}
}

func TestEnsureScopeStoreOpFrameRemainsProtoThreeAllowListCompatible(t *testing.T) {
	_, scope, _ := storeFreeDispatcherFixture(t)
	ensureScope := daemon.StoreOpFrame{Proto: daemon.ProtocolVersion, Scope: scope, Op: "ensure-scope"}
	encoded, err := json.Marshal(ensureScope)
	if err != nil {
		t.Fatal(err)
	}
	var members map[string]json.RawMessage
	if err := json.Unmarshal(encoded, &members); err != nil {
		t.Fatal(err)
	}
	if len(members) != 3 || members["proto"] == nil || members["scope"] == nil || members["op"] == nil {
		t.Fatalf("ensure-scope members=%v, want only proto/scope/op", members)
	}
	if err := parseProtoThreeStoreOpFrame(encoded); err != nil {
		t.Fatalf("proto-3 parser rejected ensure-scope frame %s: %v", encoded, err)
	}

	bodyFrame, err := daemon.NewAddTestReportStoreOp(scope, domain.TestReportInput{Raw: []byte("report")})
	if err != nil {
		t.Fatal(err)
	}
	encoded, err = json.Marshal(bodyFrame)
	if err != nil {
		t.Fatal(err)
	}
	if err := parseProtoThreeStoreOpFrame(encoded); err == nil {
		t.Fatalf("proto-3 parser accepted body-bearing store-op frame %s", encoded)
	}
}

// parseProtoThreeStoreOpFrame preserves the pre-D7b strict store-op member
// allow-list. Proto 3 rejected every store-op field except proto/scope/op.
func parseProtoThreeStoreOpFrame(payload []byte) error {
	var members map[string]json.RawMessage
	if err := json.Unmarshal(payload, &members); err != nil {
		return err
	}
	for name := range members {
		if name != "proto" && name != "scope" && name != "op" {
			return fmt.Errorf("unexpected store operation field %q", name)
		}
	}
	var frame daemon.StoreOpFrame
	return json.Unmarshal(payload, &frame)
}

func TestStoreTouchingEnsureScopeReplacesProtoThreeBeforeRelayOp(t *testing.T) {
	dispatcher := autoStartDispatcher(t)
	older := startProtocolDaemonProcess(t)
	root, scope := writeStoreFreeProject(t, dispatcher.paths)
	t.Chdir(root)
	db, err := store.OpenDB(dispatcher.paths.DBPath, dispatcher.paths.RegistryPath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := store.NewScope(db, store.ScopeOptions{
		Root: scope.Root, CommonDir: scope.CommonDir, GitDir: scope.GitDir,
		ProjectID: scope.ProjectID, WorktreeID: scope.WorktreeID, ProjectSlug: scope.Slug,
		Prefixes: scope.Prefixes, ConfigDigest: scope.ConfigDigest,
	}); err != nil {
		t.Fatal(err)
	}
	var exchanges atomic.Int32
	var ops []string
	dispatcher.storeOpExchange = func(_ context.Context, _ string, frame daemon.StoreOpFrame) (daemon.ResponseFrame, error) {
		ops = append(ops, frame.Op)
		switch exchanges.Add(1) {
		case 1:
			if frame.Op != "ensure-scope" {
				t.Fatalf("first op=%q", frame.Op)
			}
			return daemon.ResponseFrame{Proto: 3, Code: daemon.CodeProtocol, Error: daemon.CodeProtocol + ": daemon protocol is 3"}, nil
		case 2:
			if frame.Op != "ensure-scope" {
				t.Fatalf("second op=%q", frame.Op)
			}
			return daemon.ResponseFrame{OK: true, Code: "OK"}, nil
		default:
			if frame.Op != "add-command-event" {
				t.Fatalf("relay op after replacement=%q", frame.Op)
			}
			data, marshalErr := json.Marshal(validCommandEventAddResult("CMD-relayed"))
			if marshalErr != nil {
				t.Fatal(marshalErr)
			}
			return daemon.ResponseFrame{OK: true, Code: "OK", Data: data}, nil
		}
	}
	response := dispatcher.Dispatch(context.Background(), scope, core.Request{Verb: "time", Args: map[string]any{"argv": []string{"/bin/true"}, "no_prefix": true}})
	if !response.OK || exchanges.Load() != 3 || strings.Join(ops, ",") != "ensure-scope,ensure-scope,add-command-event" {
		t.Fatalf("response=%+v exchanges=%d ops=%v", response, exchanges.Load(), ops)
	}
	select {
	case <-older.done:
	case <-time.After(5 * time.Second):
		t.Fatal("proto-3 daemon was not replaced by ensure-scope before the v4 write op")
	}
}

func TestStoreTouchingEnsureScopeConflictPreventsExecution(t *testing.T) {
	dispatcher, scope, _ := storeFreeDispatcherFixture(t)
	dispatcher.storeOpExchange = func(context.Context, string, daemon.StoreOpFrame) (daemon.ResponseFrame, error) {
		return daemon.ResponseFrame{Code: "E_PREFIX_OWNERSHIP_CONFLICT", Error: "E_PREFIX_OWNERSHIP_CONFLICT: owned by another project", Exit: 1}, nil
	}
	marker := filepath.Join(scope.Root, "must-not-run")
	response := dispatcher.Dispatch(context.Background(), scope, core.Request{Verb: "time", Args: map[string]any{
		"argv": []string{"/usr/bin/touch", marker}, "no_prefix": true,
	}})
	if response.Code != "E_PREFIX_OWNERSHIP_CONFLICT" || response.Exit != 1 {
		t.Fatalf("response=%+v", response)
	}
	if _, err := os.Stat(marker); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("time ran after ensure-scope conflict: %v", err)
	}
}

func storeFreeDispatcherFixture(t *testing.T) (*daemonDispatcher, daemon.WorktreeScope, string) {
	t.Helper()
	stateHome := filepath.Join(t.TempDir(), "state")
	t.Setenv("XDG_STATE_HOME", stateHome)
	t.Setenv("XDG_RUNTIME_DIR", shortRuntimeDir(t))
	paths, err := daemon.PathsFromEnv()
	if err != nil {
		t.Fatal(err)
	}
	_, scope := writeStoreFreeProject(t, paths)
	return &daemonDispatcher{
		stdin: strings.NewReader(""), stdout: io.Discard, diagnostics: io.Discard,
		paths: paths, startWait: time.Second, lockWait: time.Second,
	}, scope, stateHome
}

func writeStoreFreeProject(t *testing.T, paths daemon.Paths) (string, daemon.WorktreeScope) {
	t.Helper()
	root := t.TempDir()
	if err := exec.Command("git", "-C", root, "init", "-q").Run(); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, ".aira"), 0o755); err != nil {
		t.Fatal(err)
	}
	config := `{"schema":1,"project":{"slug":"demo","prefixes":["AIRA"]},"lease":{"ttl_seconds":900,"heartbeat_seconds":30}}`
	if err := os.WriteFile(filepath.Join(root, ".aira", "config"), []byte(config+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	scope, err := scopeForCWD(context.Background(), root, paths)
	if err != nil {
		t.Fatal(err)
	}
	return root, scope
}

func startCommandDaemon(t *testing.T, server *daemon.Server) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	ready := make(chan struct{}, 1)
	server.Ready = ready
	done := make(chan error, 1)
	go func() { done <- server.Serve(ctx) }()
	select {
	case <-ready:
	case err := <-done:
		t.Fatalf("daemon exited before ready: %v", err)
	case <-time.After(5 * time.Second):
		t.Fatal("daemon did not become ready")
	}
	t.Cleanup(func() {
		cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Errorf("daemon shutdown: %v", err)
			}
		case <-time.After(5 * time.Second):
			t.Error("daemon did not stop")
		}
	})
}

// verifies: the production MCP face routes a mutation over the real daemon
// socket instead of opening a project store itself.
func TestMCPMutationUsesRealDaemonSocket(t *testing.T) {
	root := t.TempDir()
	if err := exec.Command("git", "-C", root, "init", "-q").Run(); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, ".aira"), 0o755); err != nil {
		t.Fatal(err)
	}
	config := `{"schema":1,"project":{"slug":"demo","prefixes":["AIRA"]},"lease":{"ttl_seconds":900,"heartbeat_seconds":30}}`
	if err := os.WriteFile(filepath.Join(root, ".aira", "config"), []byte(config+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Chdir(root)
	t.Setenv("XDG_STATE_HOME", filepath.Join(root, "state"))
	t.Setenv("XDG_RUNTIME_DIR", shortRuntimeDir(t))
	paths, err := daemon.PathsFromEnv()
	if err != nil {
		t.Fatal(err)
	}
	server := daemon.NewServer(paths)
	var socketRequests atomic.Int32
	server.OnRequest = func(_ daemon.WorktreeScope, request core.Request) {
		if core.CanonicalVerb(request.Verb) == "create" {
			socketRequests.Add(1)
		}
	}
	startCommandDaemon(t, server)
	input := strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"aira_create","arguments":{"title":"MCP mutation","kind":"feature","severity":"P2"}}}` + "\n")
	var output, diagnostics bytes.Buffer
	if exit := runMCPWithDispatcher(context.Background(), input, &output, &diagnostics, nil); exit != 0 {
		t.Fatalf("MCP exit=%d output=%q diagnostics=%q", exit, output.String(), diagnostics.String())
	}
	if !strings.Contains(output.String(), `"code":"OK"`) {
		t.Fatalf("MCP output = %q", output.String())
	}
	if _, err := os.Stat(filepath.Join(root, ".aira", "tickets", "AIRA-1.md")); err != nil {
		t.Fatalf("routed mutation ticket: %v", err)
	}
	if got := socketRequests.Load(); got != 1 {
		t.Fatalf("daemon observed %d MCP create requests, want exactly one", got)
	}
}

func TestMCPConfineKillOutsideProjectKeepsOwnershipAndStealChecks(t *testing.T) {
	airaHelper := filepath.Join(os.Getenv("HOME"), ".local", "bin", "aira")
	if _, err := os.Stat(airaHelper); err != nil {
		t.Skip("installed aira helper unavailable")
	}
	root := t.TempDir()
	t.Chdir(root)
	t.Setenv("XDG_STATE_HOME", filepath.Join(root, "state"))
	t.Setenv("XDG_RUNTIME_DIR", shortRuntimeDir(t))
	t.Setenv("AIRA_CONFINE_OWNER", "session-b")
	paths, err := daemon.PathsFromEnv()
	if err != nil {
		t.Fatal(err)
	}
	startCommandDaemon(t, daemon.NewServer(paths))

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	name := "mcp-e2e-" + strconv.FormatInt(time.Now().UnixNano(), 36)
	done := make(chan error, 1)
	go func() {
		_, launchErr := runner.Confine(ctx, runner.ConfineRequest{
			Slice: "aira.slice", Name: name, Owner: "session-a", Argv: []string{"/bin/sh", "-c", "sleep 60"},
			RuntimeDir: paths.RuntimeDir, AdmitSocketPath: paths.SocketPath, SelfPath: airaHelper,
			Stdout: io.Discard, Stderr: io.Discard,
		})
		done <- launchErr
	}()

	call := func(arguments string) string {
		t.Helper()
		input := strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"aira_confine_kill","arguments":` + arguments + `}}` + "\n")
		var output, diagnostics bytes.Buffer
		if exit := runMCPWithDispatcher(context.Background(), input, &output, &diagnostics, nil); exit != 0 {
			t.Fatalf("MCP exit=%d output=%q diagnostics=%q", exit, output.String(), diagnostics.String())
		}
		return output.String()
	}

	deadline := time.Now().Add(5 * time.Second)
	for {
		input := strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"aira_confine_list","arguments":{}}}` + "\n")
		var output bytes.Buffer
		_ = runMCPWithDispatcher(context.Background(), input, &output, io.Discard, nil)
		if strings.Contains(output.String(), name) && strings.Contains(output.String(), "session-a") {
			break
		}
		select {
		case launchErr := <-done:
			if launchErr != nil {
				cgrouptest.SkipOrFailRealCgroup(t, "real MCP confine launch unavailable: %v", launchErr)
			}
			t.Fatal("confined workload exited before kill")
		default:
		}
		if time.Now().After(deadline) {
			cgrouptest.SkipOrFailRealCgroup(t, "confine registry did not become visible")
		}
		time.Sleep(20 * time.Millisecond)
	}
	withoutSteal := call(fmt.Sprintf(`{"selector":%q}`, name))
	if !strings.Contains(withoutSteal, runner.CodeConfineOwnerUnverified) {
		t.Fatalf("without steal=%q", withoutSteal)
	}
	withSteal := call(fmt.Sprintf(`{"selector":%q,"steal":true}`, name))
	if !strings.Contains(withSteal, `"code":"OK"`) || !strings.Contains(withSteal, `"status":"killed"`) {
		t.Fatalf("with steal=%q", withSteal)
	}
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("confine supervisor did not finish after scope kill")
	}
}

func TestStoreFreeRealCLIRegistersOnlyThroughDaemon(t *testing.T) {
	root := t.TempDir()
	if err := exec.Command("git", "-C", root, "init", "-q").Run(); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, ".aira"), 0o755); err != nil {
		t.Fatal(err)
	}
	config := `{"schema":1,"project":{"slug":"demo","prefixes":["AIRA"]},"lease":{"ttl_seconds":900,"heartbeat_seconds":30}}`
	if err := os.WriteFile(filepath.Join(root, ".aira", "config"), []byte(config+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Chdir(root)
	t.Setenv("XDG_STATE_HOME", filepath.Join(root, "state"))
	t.Setenv("XDG_RUNTIME_DIR", shortRuntimeDir(t))
	paths, err := daemon.PathsFromEnv()
	if err != nil {
		t.Fatal(err)
	}
	startCommandDaemon(t, daemon.NewServer(paths))

	invoke := func(invocation int, argv ...string) core.Response {
		t.Helper()
		argv = append([]string{argv[0], "--json"}, argv[1:]...)
		var stdout, stderr bytes.Buffer
		_ = runWithInput(argv, &stdout, &stderr, strings.NewReader(""))
		var response core.Response
		if err := json.Unmarshal(stdout.Bytes(), &response); err != nil {
			t.Fatalf("argv=%v stdout=%q stderr=%q: %v", argv, stdout.String(), stderr.String(), err)
		}
		registry, err := os.ReadFile(paths.RegistryPath)
		if err != nil {
			t.Fatal(err)
		}
		if lines := strings.Count(strings.TrimSpace(string(registry)), "\n") + 1; lines != invocation {
			t.Fatalf("argv=%v registry lines=%d, want exactly %d daemon registrations", argv, lines, invocation)
		}
		return response
	}

	if response := invoke(1, "show", "RUN-missing"); response.Code != "E_RUN_NOT_FOUND" {
		t.Fatalf("missing run response = %+v", response)
	}
	runResponse := invoke(2, "run", "--no-admit", "--merge", "--", "/bin/true")
	if runResponse.Code == "E_RUN_SCOPE_UNAVAILABLE" {
		t.Skipf("real CLI runner requires delegated writable cgroup-v2: %s", runResponse.Error)
	}
	if !runResponse.OK {
		t.Fatalf("run response = %+v", runResponse)
	}
	data, ok := runResponse.Data.(map[string]any)
	if !ok {
		t.Fatalf("run data type = %T", runResponse.Data)
	}
	runID, _ := data["id"].(string)
	if runID == "" {
		t.Fatalf("run data = %#v", data)
	}
	if response := invoke(3, "show", runID); !response.OK {
		t.Fatalf("show response = %+v", response)
	}
	if response := invoke(4, "run-log", runID, "--full"); !response.OK {
		t.Fatalf("run-log response = %+v", response)
	}
	_ = invoke(5, "run-kill", runID)
}

func TestD7bRealCLIStoreTouchingVerbsRelayThroughDaemon(t *testing.T) {
	if os.Getenv("AIRA_REAL_SOCKET") != "1" {
		t.Skip("real Unix-socket D7b e2e requires AIRA_REAL_SOCKET=1")
	}
	root := t.TempDir()
	if err := exec.Command("git", "-C", root, "init", "-q").Run(); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "tracked.txt"), []byte("launch\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, argv := range [][]string{
		{"git", "-C", root, "add", "tracked.txt"},
		{"git", "-C", root, "-c", "user.name=Terra", "-c", "user.email=terra@example.test", "commit", "-qm", "launch"},
	} {
		if output, err := exec.Command(argv[0], argv[1:]...).CombinedOutput(); err != nil {
			t.Fatalf("%v: %v: %s", argv, err, output)
		}
	}
	headBytes, err := exec.Command("git", "-C", root, "rev-parse", "HEAD").Output()
	if err != nil {
		t.Fatal(err)
	}
	head := strings.TrimSpace(string(headBytes))
	if err := os.MkdirAll(filepath.Join(root, ".aira"), 0o755); err != nil {
		t.Fatal(err)
	}
	config := `{"schema":1,"project":{"slug":"demo","prefixes":["AIRA"]},"lease":{"ttl_seconds":900,"heartbeat_seconds":30}}`
	if err := os.WriteFile(filepath.Join(root, ".aira", "config"), []byte(config+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Chdir(root)
	t.Setenv("XDG_STATE_HOME", filepath.Join(root, "state"))
	t.Setenv("XDG_RUNTIME_DIR", shortRuntimeDir(t))
	paths, err := daemon.PathsFromEnv()
	if err != nil {
		t.Fatal(err)
	}
	startCommandDaemon(t, daemon.NewServer(paths))
	invoke := func(argv ...string) (int, string, string) {
		t.Helper()
		var stdout, stderr bytes.Buffer
		exit := runWithInput(append([]string{argv[0], "--json"}, argv[1:]...), &stdout, &stderr, strings.NewReader(""))
		return exit, stdout.String(), stderr.String()
	}
	if exit, stdout, stderr := invoke("time", "--no-prefix", "--", "/bin/true"); exit != 0 {
		t.Fatalf("time exit=%d stdout=%q stderr=%q", exit, stdout, stderr)
	}
	reportScript := `printf '%s\n' '{"Action":"run","Package":"pkg","Test":"TestOne"}' '{"Action":"pass","Package":"pkg","Test":"TestOne","Elapsed":0.1}' '{"Action":"pass","Package":"pkg","Elapsed":0.1}'`
	if exit, stdout, stderr := invoke("run", "--no-admit", "--report", "go-json", "--suite", "unit", "--shard", "1/1", "--", "/bin/sh", "-c", reportScript); exit != 0 {
		t.Fatalf("run --report exit=%d stdout=%q stderr=%q", exit, stdout, stderr)
	}
	if exit, stdout, stderr := invoke("run", "--no-admit", "--tool", "codex", "--", "/bin/true"); exit != 0 {
		t.Fatalf("run --tool exit=%d stdout=%q stderr=%q", exit, stdout, stderr)
	}
	if exit, stdout, stderr := invoke("spend", "ls"); exit != 0 || !strings.Contains(stdout, head) || !strings.Contains(stdout, `"git_context"`) {
		t.Fatalf("spend ls exit=%d stdout=%q stderr=%q want head=%q", exit, stdout, stderr, head)
	}
	if exit, stdout, stderr := invoke("reconcile"); exit != 0 {
		t.Fatalf("reconcile exit=%d stdout=%q stderr=%q", exit, stdout, stderr)
	}
	if exit, stdout, _ := invoke("gate", "run", "missing"); exit == 0 || !strings.Contains(stdout, "E_NOT_FOUND") {
		t.Fatalf("gate run missing exit=%d stdout=%q", exit, stdout)
	}
	db, err := sql.Open("sqlite", paths.DBPath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	for table, wantAtLeast := range map[string]int{"command_events": 1, "test_reports": 1, "compute_events": 1} {
		var count int
		if err := db.QueryRow("SELECT COUNT(*) FROM " + table).Scan(&count); err != nil {
			t.Fatal(err)
		}
		if count < wantAtLeast {
			t.Fatalf("%s rows=%d want at least %d", table, count, wantAtLeast)
		}
	}
	var storedHead, storedStatus string
	if err := db.QueryRow(`SELECT head_hash,head_hash_status FROM compute_events ORDER BY at_seq DESC LIMIT 1`).Scan(&storedHead, &storedStatus); err != nil {
		t.Fatal(err)
	}
	if storedHead != head || storedStatus != "value" {
		t.Fatalf("persisted compute head=(%q,%q) want=(%q,value)", storedHead, storedStatus, head)
	}
	registry, err := os.ReadFile(paths.RegistryPath)
	if err != nil {
		t.Fatal(err)
	}
	// Five store-touching carved verbs each do one ensure-scope handshake (time,
	// run --report, run --tool, reconcile, gate run); `spend ls` is routed (no
	// handshake). More than five would mean a writable client open added a
	// registration — the D7b invariant this guards.
	if lines := strings.Count(strings.TrimSpace(string(registry)), "\n") + 1; lines != 5 {
		t.Fatalf("registry lines=%d want exactly five daemon handshakes; a writable client open would add registrations", lines)
	}
}

type responseParityPayload struct {
	Z     string `json:"z"`
	Limit int64  `json:"limit"`
	A     struct {
		Value int64 `json:"value"`
	} `json:"a"`
}

func routedParityResponses(t *testing.T) (core.Response, core.Response) {
	t.Helper()
	payload := responseParityPayload{Z: "last", Limit: int64(^uint64(0) >> 1)}
	payload.A.Value = payload.Limit
	want := core.Response{OK: true, Code: "OK", Data: payload}
	raw, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	got := (daemon.ResponseFrame{OK: true, Code: "OK", Data: raw}).CoreResponse()
	if !got.OK {
		t.Fatalf("routed response = %+v", got)
	}
	return want, got
}

func TestCLIRoutedStructuredResponseIsRenderedByteIdentically(t *testing.T) {
	want, got := routedParityResponses(t)
	for _, jsonOutput := range []bool{false, true} {
		var wantOut, gotOut, wantErr, gotErr bytes.Buffer
		wantExit := render(want, jsonOutput, &wantOut, &wantErr)
		gotExit := render(got, jsonOutput, &gotOut, &gotErr)
		if wantExit != gotExit || wantOut.String() != gotOut.String() || wantErr.String() != gotErr.String() {
			t.Fatalf("json=%v routed render differs\n got stdout: %q\nwant stdout: %q\n got stderr: %q\nwant stderr: %q", jsonOutput, gotOut.String(), wantOut.String(), gotErr.String(), wantErr.String())
		}
		if !strings.Contains(gotOut.String(), "9223372036854775807") {
			t.Fatalf("json=%v max int64 missing from %q", jsonOutput, gotOut.String())
		}
	}
}

func TestMCPRoutedStructuredResponseIsRenderedByteIdentically(t *testing.T) {
	want, got := routedParityResponses(t)
	wantWire, err := json.Marshal(toolResponse(json.RawMessage("1"), want))
	if err != nil {
		t.Fatal(err)
	}
	gotWire, err := json.Marshal(toolResponse(json.RawMessage("1"), got))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(gotWire, wantWire) {
		t.Fatalf("routed MCP render differs\n got: %s\nwant: %s", gotWire, wantWire)
	}
	if !bytes.Contains(gotWire, []byte("9223372036854775807")) {
		t.Fatalf("max int64 missing from %s", gotWire)
	}
}

func TestRoutedInitRenderingMatchesInProcessFieldOrder(t *testing.T) {
	cwd := t.TempDir()
	if err := exec.Command("git", "-C", cwd, "init", "-q").Run(); err != nil {
		t.Fatal(err)
	}
	t.Chdir(cwd)
	t.Setenv("XDG_STATE_HOME", filepath.Join(cwd, "state"))
	t.Setenv("XDG_RUNTIME_DIR", shortRuntimeDir(t))
	absolute := app.InitResult{
		Root: cwd, Config: filepath.Join(cwd, ".aira", "config"), Project: "demo", Prefixes: []string{"AIRA"}, Created: true,
	}
	raw, err := json.Marshal(absolute)
	if err != nil {
		t.Fatal(err)
	}
	inProcess := dispatcherFunc(func(context.Context, daemon.WorktreeScope, core.Request) core.Response {
		return core.Response{OK: true, Code: "OK", Data: absolute}
	})
	routed := dispatcherFunc(func(context.Context, daemon.WorktreeScope, core.Request) core.Response {
		return (daemon.ResponseFrame{OK: true, Code: "OK", Data: raw}).CoreResponse()
	})
	for _, jsonOutput := range []bool{false, true} {
		argv := []string{"init", "--project", "demo", "--prefix", "AIRA"}
		if jsonOutput {
			argv = append(argv, "--json")
		}
		var inProcessOut, routedOut, inProcessErr, routedErr bytes.Buffer
		inProcessExit := runWithInputDispatcher(argv, &inProcessOut, &inProcessErr, strings.NewReader(""), inProcess)
		routedExit := runWithInputDispatcher(argv, &routedOut, &routedErr, strings.NewReader(""), routed)
		if inProcessExit != routedExit || inProcessOut.String() != routedOut.String() || inProcessErr.String() != routedErr.String() {
			t.Fatalf("json=%v routed init differs\n got stdout: %q\nwant stdout: %q\n got stderr: %q\nwant stderr: %q", jsonOutput, routedOut.String(), inProcessOut.String(), routedErr.String(), inProcessErr.String())
		}
		if jsonOutput {
			wantOrder := `"data":{"root":".","config":".aira/config","project":"demo","prefixes":["AIRA"],"created":true}`
			if !strings.Contains(routedOut.String(), wantOrder) {
				t.Fatalf("routed init order = %q", routedOut.String())
			}
		}
	}
}

func TestEjectCLIUsesProjectlessMachineRouteAndPreservesSelectors(t *testing.T) {
	var gotScope daemon.WorktreeScope
	var gotRequest core.Request
	dispatcher := dispatcherFunc(func(_ context.Context, scope daemon.WorktreeScope, request core.Request) core.Response {
		gotScope, gotRequest = scope, request
		return core.Response{OK: true, Code: "OK", Data: map[string]any{"project_id": "p"}}
	})
	var stdout, stderr bytes.Buffer
	exit := runWithInputDispatcher([]string{"eject", "--prefix", "LIFE", "--purge", "--force"}, &stdout, &stderr, strings.NewReader(""), dispatcher)
	if exit != 0 {
		t.Fatalf("exit=%d stdout=%q stderr=%q", exit, stdout.String(), stderr.String())
	}
	if gotScope.Root != "" || gotScope.ProjectID != "" || gotScope.WorktreeID != "" || len(gotScope.Prefixes) != 0 {
		t.Fatalf("eject carried project scope: %+v", gotScope)
	}
	if gotRequest.Verb != "eject" || gotRequest.Args["prefix"] != "LIFE" || gotRequest.Args["purge"] != true || gotRequest.Args["force"] != true {
		t.Fatalf("request=%#v", gotRequest)
	}
}

// verifies: MCP eject is decoded and sent through the daemon dispatcher even
// when MCP is started outside an adopted project. The injected dispatcher is
// the daemon-transport seam; the in-process eject handler would instead return
// E_DAEMON_UNAVAILABLE.
func TestMCPEjectUsesProjectlessDaemonRouteAndPreservesSelectors(t *testing.T) {
	root := t.TempDir()
	t.Chdir(root)
	t.Setenv("XDG_STATE_HOME", filepath.Join(root, "state"))
	t.Setenv("XDG_RUNTIME_DIR", shortRuntimeDir(t))
	called := false
	dispatcher := dispatcherFunc(func(_ context.Context, scope daemon.WorktreeScope, request core.Request) core.Response {
		called = true
		if scope.Root != "" || scope.ProjectID != "" || scope.WorktreeID != "" || len(scope.Prefixes) != 0 {
			t.Fatalf("eject carried project scope: %+v", scope)
		}
		want := core.Request{Verb: "eject", Args: map[string]any{"prefix": "X", "force": true}}
		if !reflect.DeepEqual(request, want) {
			t.Fatalf("request=%#v, want %#v", request, want)
		}
		return core.Response{OK: true, Code: "OK", Data: map[string]any{"routed": "daemon"}}
	})
	input := strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"aira_eject","arguments":{"prefix":"X","force":true}}}` + "\n")
	var output, diagnostics bytes.Buffer
	if exit := runMCPWithDispatcher(context.Background(), input, &output, &diagnostics, dispatcher); exit != 0 {
		t.Fatalf("MCP exit=%d output=%q diagnostics=%q", exit, output.String(), diagnostics.String())
	}
	if !called || !strings.Contains(output.String(), `"code":"OK"`) || strings.Contains(output.String(), "E_DAEMON_UNAVAILABLE") {
		t.Fatalf("called=%v output=%q", called, output.String())
	}
}

func TestEjectWithoutSelectorAndWithoutCurrentConfigIsENoProject(t *testing.T) {
	cwd := t.TempDir()
	if err := exec.Command("git", "-C", cwd, "init", "-q").Run(); err != nil {
		t.Fatal(err)
	}
	t.Chdir(cwd)
	called := false
	dispatcher := dispatcherFunc(func(context.Context, daemon.WorktreeScope, core.Request) core.Response {
		called = true
		return core.Response{OK: true, Code: "OK"}
	})
	var stdout, stderr bytes.Buffer
	if exit := runWithInputDispatcher([]string{"eject"}, &stdout, &stderr, strings.NewReader(""), dispatcher); exit != store.ExitForCode("E_NO_PROJECT") {
		t.Fatalf("exit=%d stdout=%q stderr=%q", exit, stdout.String(), stderr.String())
	}
	if called || !strings.Contains(stderr.String(), "E_NO_PROJECT") {
		t.Fatalf("called=%v stderr=%q", called, stderr.String())
	}
}

func runInProcessWithInput(argv []string, stdout, stderr io.Writer, stdin io.Reader) int {
	dispatcher := &inProcessDispatcher{stdin: stdin, stdout: stdout, diagnostics: stderr}
	return runWithInputDispatcher(argv, stdout, stderr, stdin, dispatcher)
}
