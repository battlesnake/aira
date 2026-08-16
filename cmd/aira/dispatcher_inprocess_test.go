package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"aira/internal/app"
	"aira/internal/core"
	"aira/internal/daemon"
	"aira/internal/store"
	"golang.org/x/sys/unix"
)

func runInProcess(argv []string, stdout, stderr io.Writer) int {
	return runInProcessWithInput(argv, stdout, stderr, strings.NewReader(""))
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

func runInProcessWithInput(argv []string, stdout, stderr io.Writer, stdin io.Reader) int {
	dispatcher := &inProcessDispatcher{stdin: stdin, stdout: stdout, diagnostics: stderr}
	return runWithInputDispatcher(argv, stdout, stderr, stdin, dispatcher)
}
