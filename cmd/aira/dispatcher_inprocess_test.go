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
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"aira/internal/core"
	"aira/internal/daemon"
	"aira/internal/store"
)

func runInProcess(argv []string, stdout, stderr io.Writer) int {
	return runInProcessWithInput(argv, stdout, stderr, strings.NewReader(""))
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

func TestOlderClientNeverReplacesNewerDaemon(t *testing.T) {
	dispatcher := autoStartDispatcher(t)
	spawned := false
	dispatcher.exchange = func(context.Context, string, daemon.RequestFrame) (daemon.ResponseFrame, error) {
		return daemon.ResponseFrame{Proto: daemon.ProtocolVersion + 1, Code: daemon.CodeProtocol, Error: daemon.CodeProtocol + ": newer daemon"}, nil
	}
	dispatcher.spawn = func() (<-chan childResult, error) {
		spawned = true
		return nil, errors.New("must not spawn")
	}
	response := dispatcher.Dispatch(context.Background(), daemon.WorktreeScope{}, core.Request{Verb: "list"})
	if response.Code != daemon.CodeProtocol || spawned {
		t.Fatalf("response=%+v spawned=%v", response, spawned)
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
	startCommandDaemon(t, daemon.NewServer(paths))
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

func runInProcessWithInput(argv []string, stdout, stderr io.Writer, stdin io.Reader) int {
	dispatcher := &inProcessDispatcher{stdin: stdin, stdout: stdout, diagnostics: stderr}
	return runWithInputDispatcher(argv, stdout, stderr, stdin, dispatcher)
}
