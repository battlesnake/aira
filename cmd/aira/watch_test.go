package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"aira/internal/core"
	"aira/internal/daemon"
	"aira/internal/store"
)

func TestWatchArgumentsBuildCursorFiltersAndTarget(t *testing.T) {
	tests := []struct {
		name string
		argv []string
		want map[string]any
	}{
		{name: "from now", argv: nil, want: map[string]any{"from_now": true}},
		{name: "from", argv: []string{"--from", "42"}, want: map[string]any{"from": int64(42)}},
		{name: "from start", argv: []string{"--from-start"}, want: map[string]any{"from": int64(0)}},
		{name: "filters", argv: []string{"AIRA-2", "--verb", "lease.release,lease.lapse"}, want: map[string]any{"target": "AIRA-2"}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			positional, options, err := parseArgs("watch", test.argv)
			if err != nil {
				t.Fatal(err)
			}
			request, err := buildRequest("watch", positional, options)
			if err != nil {
				t.Fatal(err)
			}
			for key, want := range test.want {
				if got := request.Args[key]; got != want {
					t.Fatalf("%s=%#v want=%#v request=%#v", key, got, want, request)
				}
			}
			if request.Args["wait_ms"] != int64(20_000) {
				t.Fatalf("wait_ms=%#v", request.Args["wait_ms"])
			}
			if test.name == "filters" {
				verbs := request.Args["verbs"].([]string)
				if len(verbs) != 2 || verbs[0] != "lease.release" || verbs[1] != "lease.lapse" {
					t.Fatalf("verbs=%v", verbs)
				}
			}
		})
	}
	for _, argv := range [][]string{{"--from", "1", "--from-start"}, {"--from", "-1"}, {"one", "two"}} {
		positional, options, err := parseArgs("watch", argv)
		if err == nil {
			_, err = buildRequest("watch", positional, options)
		}
		if err == nil {
			t.Fatalf("argv=%v unexpectedly accepted", argv)
		}
	}
}

func TestWatchLoopRetriesUnadvancedCursorPrintsThenStopsOnEOF(t *testing.T) {
	var requests []core.Request
	dispatcher := dispatcherFunc(func(_ context.Context, _ daemon.WorktreeScope, request core.Request) core.Response {
		copyArgs := make(map[string]any, len(request.Args))
		for key, value := range request.Args {
			copyArgs[key] = value
		}
		requests = append(requests, core.Request{Verb: request.Verb, Args: copyArgs})
		switch len(requests) {
		case 1:
			return core.Response{Code: daemon.CodeUnavailable, Error: daemon.CodeUnavailable + ": dropped", Exit: 4}
		case 2:
			return core.Response{OK: true, Code: "OK", Data: daemon.WatchResponse{Events: []store.WatchEvent{{Seq: 6, At: "now", Actor: "a", Verb: "mv", Target: "AIRA-1"}}, Cursor: 6}}
		default:
			return core.Response{OK: true, Code: "OK", Data: daemon.WatchResponse{Events: []store.WatchEvent{}, Cursor: 6, EOF: true}}
		}
	})
	request := core.Request{Verb: "watch", Args: map[string]any{"from": int64(5), "wait_ms": int64(20_000)}}
	var stdout, stderr bytes.Buffer
	if exit := runWatchLoop(context.Background(), dispatcher, daemon.WorktreeScope{}, request, false, &stdout, &stderr); exit != 0 {
		t.Fatalf("exit=%d stdout=%q stderr=%q", exit, stdout.String(), stderr.String())
	}
	if got := stdout.String(); got != "6 now a mv AIRA-1\n" {
		t.Fatalf("stdout=%q", got)
	}
	if !strings.Contains(stderr.String(), "daemon stopped") {
		t.Fatalf("stderr=%q", stderr.String())
	}
	if len(requests) != 3 || requests[0].Args["from"] != int64(5) || requests[1].Args["from"] != int64(5) || requests[2].Args["from"] != int64(6) {
		t.Fatalf("requests=%#v", requests)
	}
}

func TestWatchLoopJSONPrintsOneObjectPerEvent(t *testing.T) {
	dispatcher := dispatcherFunc(func(context.Context, daemon.WorktreeScope, core.Request) core.Response {
		return core.Response{OK: true, Code: "OK", Data: daemon.WatchResponse{Events: []store.WatchEvent{
			{Seq: 1, At: "a", Actor: "one", Verb: "create", Target: "AIRA-1"},
			{Seq: 2, At: "b", Actor: "two", Verb: "mv", Target: "AIRA-1"},
		}, Cursor: 2, EOF: true}}
	})
	var stdout, stderr bytes.Buffer
	if exit := runWatchLoop(context.Background(), dispatcher, daemon.WorktreeScope{}, core.Request{Verb: "watch", Args: map[string]any{}}, true, &stdout, &stderr); exit != 0 {
		t.Fatal(exit)
	}
	lines := strings.Split(strings.TrimSpace(stdout.String()), "\n")
	if len(lines) != 2 {
		t.Fatalf("lines=%q", lines)
	}
	for index, line := range lines {
		var event store.WatchEvent
		if err := json.Unmarshal([]byte(line), &event); err != nil || event.Seq != int64(index+1) {
			t.Fatalf("line=%q event=%+v err=%v", line, event, err)
		}
	}
}

func TestWatchLoopCancellationExitsZeroAndCancelsExchange(t *testing.T) {
	started := make(chan struct{})
	dispatcher := dispatcherFunc(func(ctx context.Context, _ daemon.WorktreeScope, _ core.Request) core.Response {
		close(started)
		<-ctx.Done()
		return core.Response{Code: daemon.CodeUnavailable, Error: ctx.Err().Error(), Exit: 4}
	})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan int, 1)
	go func() {
		done <- runWatchLoop(ctx, dispatcher, daemon.WorktreeScope{}, core.Request{Verb: "watch", Args: map[string]any{}}, false, io.Discard, io.Discard)
	}()
	<-started
	cancel()
	select {
	case exit := <-done:
		if exit != 0 {
			t.Fatalf("exit=%d", exit)
		}
	case <-time.After(200 * time.Millisecond):
		t.Fatal("cancellation did not stop watch loop")
	}
}

type lockedBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (b *lockedBuffer) Write(data []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.b.Write(data)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.b.String()
}

func TestWatchRealDaemonPrintsConcurrentEventAndEOF(t *testing.T) {
	dispatcher, scope, _ := storeFreeDispatcherFixture(t)
	server := daemon.NewServer(dispatcher.paths)
	ctx, cancelServer := context.WithCancel(context.Background())
	ready := make(chan struct{}, 1)
	server.Ready = ready
	serverDone := make(chan error, 1)
	go func() { serverDone <- server.Serve(ctx) }()
	select {
	case <-ready:
	case err := <-serverDone:
		t.Fatalf("daemon start: %v", err)
	case <-time.After(5 * time.Second):
		t.Fatal("daemon did not start")
	}

	var stdout, stderr lockedBuffer
	watchDone := make(chan int, 1)
	go func() {
		watchDone <- runWatchLoop(context.Background(), dispatcher, scope, core.Request{Verb: "watch", Args: map[string]any{
			"from": int64(0), "verbs": []string{"id.allocate"}, "wait_ms": int64(20_000),
		}}, false, &stdout, &stderr)
	}()
	mutation := dispatcher.Dispatch(context.Background(), scope, core.Request{Verb: "id", Args: map[string]any{"prefix": "AIRA"}})
	if !mutation.OK {
		t.Fatalf("mutation=%+v", mutation)
	}
	deadline := time.Now().Add(2 * time.Second)
	for !strings.Contains(stdout.String(), "id.allocate") && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if !strings.Contains(stdout.String(), "id.allocate") {
		t.Fatalf("watch output=%q", stdout.String())
	}
	cancelServer()
	select {
	case exit := <-watchDone:
		if exit != 0 {
			t.Fatalf("watch exit=%d stderr=%q", exit, stderr.String())
		}
	case <-time.After(5 * time.Second):
		t.Fatal("watch did not exit on daemon EOF")
	}
	if err := <-serverDone; err != nil {
		t.Fatalf("server stop=%v", err)
	}
}
