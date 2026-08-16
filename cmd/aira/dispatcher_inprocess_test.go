package main

import (
	"bytes"
	"context"
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

type recordingDispatcher struct {
	requests []core.Request
	scopes   []daemon.WorktreeScope
}

func (d *recordingDispatcher) Dispatch(_ context.Context, scope daemon.WorktreeScope, request core.Request) core.Response {
	d.scopes = append(d.scopes, scope)
	d.requests = append(d.requests, request)
	return core.Response{OK: true, Code: "OK", Data: map[string]any{"routed": true}}
}

// verifies: the production MCP face reaches the shared Dispatcher seam for a
// mutation instead of opening a project store itself.
func TestMCPMutationUsesSharedDispatcher(t *testing.T) {
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
	recorder := &recordingDispatcher{}
	input := strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"aira_create","arguments":{"title":"MCP mutation","kind":"feature","severity":"P2"}}}` + "\n")
	var output, diagnostics bytes.Buffer
	if exit := runMCPWithDispatcher(context.Background(), input, &output, &diagnostics, recorder); exit != 0 {
		t.Fatalf("MCP exit=%d output=%q diagnostics=%q", exit, output.String(), diagnostics.String())
	}
	if len(recorder.requests) != 1 || recorder.requests[0].Verb != "create" {
		t.Fatalf("dispatcher requests = %+v", recorder.requests)
	}
	if len(recorder.scopes) != 1 || recorder.scopes[0].Root != root {
		t.Fatalf("dispatcher scopes = %+v", recorder.scopes)
	}
}

func runInProcessWithInput(argv []string, stdout, stderr io.Writer, stdin io.Reader) int {
	dispatcher := &inProcessDispatcher{stdin: stdin, stdout: stdout, diagnostics: stderr}
	return runWithInputDispatcher(argv, stdout, stderr, stdin, dispatcher)
}
