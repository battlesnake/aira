package main

import (
	"bytes"
	"context"
	"errors"
	"os"
	"reflect"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"aira/internal/core"
	"aira/internal/daemon"
)

func executeEntryMap(entries []executeEntry) map[string]executeEntry {
	result := make(map[string]executeEntry, len(entries))
	for _, entry := range entries {
		result[entry.Verb] = entry
	}
	return result
}

// covers: the launcher is a closed allowlist, not a SafetyExecute predicate.
func TestExecuteListIsExplicitAllowlistAndCapabilityAware(t *testing.T) {
	descriptors := core.New(nil).DispatchDescriptors()
	enabled := executeEntryMap(buildExecuteList(descriptors, true))
	if len(enabled) != 4 {
		t.Fatalf("execute list=%v", enabled)
	}
	for _, verb := range []string{"run", "git", "time"} {
		entry, ok := enabled[verb]
		if !ok || !entry.Enabled || entry.PrintOnly {
			t.Fatalf("enabled %s entry=%#v ok=%v", verb, entry, ok)
		}
	}
	if entry := enabled["confine"]; !entry.Enabled || !entry.PrintOnly {
		t.Fatalf("confine entry=%#v", entry)
	}
	for _, excluded := range []string{"run-kill", "run-input", "reconcile", "check"} {
		if _, ok := enabled[excluded]; ok {
			t.Fatalf("non-allowlisted execute verb %q leaked into launcher", excluded)
		}
	}

	disabled := executeEntryMap(buildExecuteList(descriptors, false))
	for _, verb := range []string{"run", "git", "time"} {
		if disabled[verb].Enabled || !strings.Contains(disabled[verb].Unavailable, "execute unavailable") {
			t.Fatalf("capability-absent %s entry=%#v", verb, disabled[verb])
		}
	}
	if !disabled["confine"].Enabled || !disabled["confine"].PrintOnly {
		t.Fatalf("print-only confine was disabled: %#v", disabled["confine"])
	}
}

func TestShellWordsQuotesEscapesEmptyAndErrors(t *testing.T) {
	got, err := shellWords(`-- printf '%s %s' "a b" c\ d '' "" 'single\literal'`)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"--", "printf", "%s %s", "a b", "c d", "", "", `single\literal`}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("shell words=%#v want=%#v", got, want)
	}
	for _, input := range []string{`-- sh -c 'unterminated`, `-- sh -c "unterminated`, `-- echo trailing\`} {
		if _, err := shellWords(input); err == nil || !strings.Contains(err.Error(), "unterminated") {
			t.Fatalf("shellWords(%q) error=%v", input, err)
		}
	}
}

// verifies: lexed text is fed through the real CLI parsers/buildRequest, so the
// standalone delimiter and telemetry-value StoreFreeCarved split cannot drift.
func TestParseExecuteRequestMatchesCLIAndStoreFreeCarved(t *testing.T) {
	request, err := parseExecuteRequest("run", `--merge -- printf '%s' 'a b'`)
	if err != nil {
		t.Fatal(err)
	}
	positional, options, err := parseRunArgs([]string{"--merge", "--", "printf", "%s", "a b"})
	if err != nil {
		t.Fatal(err)
	}
	want, err := buildRequest("run", positional, options)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(request, want) {
		t.Fatalf("execute request=%#v CLI=%#v", request, want)
	}

	for _, test := range []struct {
		line      string
		storeFree bool
	}{
		{line: `-- true`, storeFree: true},
		{line: `--tool '' -- true`, storeFree: true},
		{line: `--tool codex -- true`, storeFree: false},
		{line: `--usage usage.json -- true`, storeFree: false},
	} {
		req, err := parseExecuteRequest("run", test.line)
		if err != nil {
			t.Fatalf("parse %q: %v", test.line, err)
		}
		if got := core.StoreFreeCarved(req.Verb, req.Args); got != test.storeFree {
			t.Fatalf("StoreFreeCarved(%q)=%v request=%#v", test.line, got, req)
		}
	}
	for _, verb := range []string{"run", "time", "git"} {
		if _, err := parseExecuteRequest(verb, `echo missing-delimiter`); err == nil || !strings.Contains(err.Error(), "standalone --") {
			t.Fatalf("%s missing delimiter error=%v", verb, err)
		}
	}
	if _, err := parseExecuteRequest("run", `--detach -- true`); err == nil || !strings.Contains(err.Error(), "detached run is unavailable") {
		t.Fatalf("foreground launcher accepted detached run: %v", err)
	}
	gitReq, err := parseExecuteRequest("git", `-- fetch origin`)
	if err != nil || gitReq.Args["subverb"] != "fetch" || gitReq.Args["remote"] != "origin" {
		t.Fatalf("git request=%#v err=%v", gitReq, err)
	}
}

func TestExecuteControllerConfirmIsExactlyOnce(t *testing.T) {
	state := newTUIState(8)
	state.CanExecute = true
	state = onExecuteOpen(state)
	entry := executeEntryMap(buildExecuteList(core.New(nil).DispatchDescriptors(), true))["run"]
	state = onExecuteSelect(state, entry)
	var err error
	state, err = onExecuteSubmit(state, `-- printf 'hello world'`)
	if err != nil || state.ExecuteConfirm == nil || state.ExecuteRunning {
		t.Fatalf("submit state=%#v running=%v err=%v", state.ExecuteConfirm, state.ExecuteRunning, err)
	}
	state, launch := onExecuteConfirm(state)
	if launch == nil || !state.ExecuteRunning || state.ExecuteConfirm != nil {
		t.Fatalf("confirm launch=%#v state=%#v", launch, state)
	}
	for i := 0; i < 8; i++ {
		var repeated *executeLaunch
		state, repeated = onExecuteConfirm(state)
		if repeated != nil {
			t.Fatalf("repeat %d relaunched %#v", i, repeated)
		}
	}
	state = onExecuteComplete(state)
	if state.ExecuteRunning || state.ExecuteOpen {
		t.Fatalf("completion left execute active: %#v", state)
	}
}

func TestExecuteResumeForcesAllViewsBeforeRunningClears(t *testing.T) {
	state := newTUIState(8)
	state.ExecuteRunning = true
	state, commands := onExecuteResume(state)
	if !state.ExecuteRunning || len(commands) != len(dataViews) {
		t.Fatalf("resume state running=%v commands=%#v", state.ExecuteRunning, commands)
	}
	for index, view := range dataViews {
		if commands[index].Kind != cmdFetch || commands[index].View != view {
			t.Fatalf("resume command[%d]=%#v want %s fetch", index, commands[index], view)
		}
	}
}

type executeRouteRecorder struct {
	mu            sync.Mutex
	dispatch      int
	dispatchRoute core.Route
	palette       int
	response      core.Response
}

func (r *executeRouteRecorder) Dispatch(_ context.Context, _ daemon.WorktreeScope, request core.Request) core.Response {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.dispatch++
	_, r.dispatchRoute = core.ClassifyRequest(request)
	return r.response
}

func (r *executeRouteRecorder) DispatchPalette(context.Context, daemon.WorktreeScope, core.Request) paletteDispatchAttempt {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.palette++
	return paletteDispatchAttempt{Err: errors.New("wrong route"), Send: paletteSendNotSent}
}

func (r *executeRouteRecorder) counts() (int, int, core.Route) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.dispatch, r.palette, r.dispatchRoute
}

// verifies: execute dispatch is physically disjoint from the palette, while
// confine is permanently print-only and shell-quotes every positional argument.
func TestExecuteDispatchUsesClientRouteAndConfineDispatchesNothing(t *testing.T) {
	recorder := &executeRouteRecorder{response: core.Response{OK: true, Code: "OK"}}
	req, err := parseExecuteRequest("run", `-- true`)
	if err != nil {
		t.Fatal(err)
	}
	report := dispatchExecuteLaunch(context.Background(), recorder, daemon.WorktreeScope{}, executeLaunch{Entry: executeEntry{Verb: "run", Enabled: true}, Request: req})
	dispatch, palette, route := recorder.counts()
	if dispatch != 1 || palette != 0 || route != core.RouteClient || report.Execution == "" {
		t.Fatalf("routes dispatch=%d palette=%d route=%v report=%#v", dispatch, palette, route, report)
	}

	line, err := renderConfineCommand(`--slice 'safe slice' -- printf '%s' 'a b' "x'y"`)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(line, " -- ") || !strings.Contains(line, `'a b'`) || !strings.Contains(line, `'x'"'"'y'`) {
		t.Fatalf("unsafe confine rendering %q", line)
	}
	confine := dispatchExecuteLaunch(context.Background(), recorder, daemon.WorktreeScope{}, executeLaunch{
		Entry: executeEntry{Verb: "confine", Enabled: true, PrintOnly: true}, ConfineText: line,
	})
	dispatchAfter, paletteAfter, _ := recorder.counts()
	if dispatchAfter != dispatch || paletteAfter != palette || !strings.Contains(confine.Execution, "print only") {
		t.Fatalf("confine dispatched or claimed execution: before=%d/%d after=%d/%d report=%#v", dispatch, palette, dispatchAfter, paletteAfter, confine)
	}
}

// covers: execution evidence and telemetry persistence evidence are independent.
func TestExecuteHonestyReportsExecutionAndPersistenceSeparately(t *testing.T) {
	telemetry, err := parseExecuteRequest("run", `--tool codex -- true`)
	if err != nil {
		t.Fatal(err)
	}
	plain, err := parseExecuteRequest("run", `-- true`)
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name        string
		verb        string
		request     core.Request
		response    core.Response
		execution   string
		persistence string
	}{
		{name: "time exit byte transparent", verb: "time", request: core.Request{Verb: "time"}, response: core.Response{OK: true, Code: "OK", Exit: 9}, execution: "process exit 9"},
		{name: "time ensure scope policy rejection not launched", verb: "time", request: core.Request{Verb: "time"}, response: core.Response{Code: "E_PREFIX_OWNERSHIP_CONFLICT", Exit: 1}, execution: "not launched"},
		{name: "run killed", verb: "run", request: plain, response: core.Response{Code: "E_RUN_KILLED", Exit: 1}, execution: "E_RUN_KILLED"},
		{name: "run argument family", verb: "run", request: plain, response: core.Response{Code: "E_RUN_ARGUMENT_INVALID", Exit: 2}, execution: "E_RUN_ARGUMENT_INVALID"},
		{name: "run oom", verb: "run", request: plain, response: core.Response{Code: "E_RUN_OOM_KILLED", Exit: 1}, execution: "E_RUN_OOM_KILLED"},
		{name: "run exit unknown", verb: "run", request: plain, response: core.Response{Code: "U_RUN_EXIT_UNKNOWN", Exit: 3}, execution: "U_RUN_EXIT_UNKNOWN"},
		{name: "telemetry persisted", verb: "run", request: telemetry, response: core.Response{OK: true, Code: "OK", RawData: []byte(`{"status":"exited","exit_code":0,"wiring":{"wiring_complete":true}}`)}, execution: "exited", persistence: "persisted"},
		{name: "telemetry relay unknown", verb: "run", request: telemetry, response: core.Response{OK: true, Code: "OK", RawData: []byte(`{"status":"exited","exit_code":0,"wiring":{"wiring_complete":false,"warnings":[{"code":"U_DAEMON_OUTCOME_UNKNOWN"}]}}`)}, execution: "exited", persistence: "relay unknown"},
		{name: "ensure scope not launched", verb: "run", request: telemetry, response: core.Response{Code: daemon.CodeUnavailable, Error: "dial failed", Exit: 4}, execution: "not launched", persistence: "not attempted"},
		{name: "ensure scope policy rejection not launched", verb: "run", request: telemetry, response: core.Response{Code: "E_PREFIX_OWNERSHIP_CONFLICT", Error: "prefix owned by another project", Exit: 1}, execution: "not launched", persistence: "not attempted"},
		{name: "git reports gitops exit", verb: "git", request: core.Request{Verb: "git"}, response: core.Response{Code: "E_GIT_FAILED", Exit: 1, RawData: []byte(`{"exit_code":128}`)}, execution: "gitops exit 128"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			report := classifyExecuteResponse(test.verb, test.request, test.response)
			if !strings.Contains(strings.ToLower(report.Execution), strings.ToLower(test.execution)) {
				t.Fatalf("execution=%q want containing %q", report.Execution, test.execution)
			}
			if test.persistence == "" {
				if report.Persistence != "" {
					t.Fatalf("unexpected persistence=%q", report.Persistence)
				}
			} else if !strings.Contains(strings.ToLower(report.Persistence), strings.ToLower(test.persistence)) {
				t.Fatalf("persistence=%q want containing %q", report.Persistence, test.persistence)
			}
		})
	}
}

func TestTUISignalLoopSwallowsDuringExecuteAndCancelsWhenIdle(t *testing.T) {
	ctx, stop := context.WithCancel(context.Background())
	defer stop()
	signals := make(chan os.Signal, 2)
	var mu sync.Mutex
	running := true
	cancelled := 0
	done := make(chan struct{})
	go func() {
		runTUISignalLoop(ctx, signals, func() bool {
			mu.Lock()
			defer mu.Unlock()
			return running
		}, func() {
			mu.Lock()
			cancelled++
			mu.Unlock()
		})
		close(done)
	}()
	signals <- syscall.SIGINT
	time.Sleep(20 * time.Millisecond)
	mu.Lock()
	if cancelled != 0 {
		mu.Unlock()
		t.Fatal("SIGINT cancelled TUI while execute was running")
	}
	running = false
	mu.Unlock()
	signals <- syscall.SIGINT
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		mu.Lock()
		got := cancelled
		mu.Unlock()
		if got == 1 {
			break
		}
		time.Sleep(time.Millisecond)
	}
	mu.Lock()
	got := cancelled
	mu.Unlock()
	if got != 1 {
		t.Fatalf("idle SIGINT cancel count=%d", got)
	}
	stop()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("signal loop did not stop with context")
	}
}

type panicExecuteDispatcher struct{}

func (panicExecuteDispatcher) Dispatch(context.Context, daemon.WorktreeScope, core.Request) core.Response {
	panic("execute seam panic")
}

func TestExecuteSuspendAbortAndCallbackPanicClearAtomicGuard(t *testing.T) {
	request, err := parseExecuteRequest("run", `-- true`)
	if err != nil {
		t.Fatal(err)
	}
	launch := executeLaunch{Entry: executeEntry{Verb: "run", Enabled: true}, Request: request}
	for _, test := range []struct {
		name       string
		dispatcher Dispatcher
		suspend    func(func()) bool
		want       string
	}{
		{name: "suspend false", dispatcher: &executeRouteRecorder{}, suspend: func(func()) bool { return false }, want: "not launched"},
		{name: "callback panic", dispatcher: panicExecuteDispatcher{}, suspend: func(callback func()) bool { callback(); return true }, want: "callback panic"},
	} {
		t.Run(test.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			var stdout, stderr bytes.Buffer
			runtime := newTUIRuntime(ctx, &tuiSmokeDispatcher{started: make(chan struct{})}, test.dispatcher,
				daemon.WorktreeScope{}, strings.NewReader("\n"), &stdout, &stderr, nil)
			runtime.suspend = test.suspend
			runtime.state.ExecuteRunning = true
			runtime.executeLaunchOnUI(launch)
			if runtime.executeRunning.Load() || runtime.state.ExecuteRunning {
				t.Fatalf("execute guard stuck: atomic=%v state=%v", runtime.executeRunning.Load(), runtime.state.ExecuteRunning)
			}
			if combined := stdout.String() + stderr.String(); !strings.Contains(combined, test.want) {
				t.Fatalf("output=%q want %q", combined, test.want)
			}
			cancel()
			runtime.executor.wait()
		})
	}
}
