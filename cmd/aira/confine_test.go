package main

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"aira/internal/app"
	"aira/internal/core"
	"aira/internal/daemon"
	"aira/internal/runner"
)

func TestParseConfineArgsKeepsTargetByteTransparent(t *testing.T) {
	positional, options, err := parseArgs("confine", []string{"--slice", "safe.slice", "--name", "job", "--", "pytest", "--json", "--name", "child"})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(positional, []string{"pytest", "--json", "--name", "child"}) {
		t.Fatalf("target=%q", positional)
	}
	if options["slice"] != "safe.slice" || options["name"] != "job" {
		t.Fatalf("options=%v", options)
	}

	args, jsonOutput := removeJSON([]string{"confine", "--", "tool", "--json"})
	if jsonOutput || !reflect.DeepEqual(args, []string{"confine", "--", "tool", "--json"}) {
		t.Fatalf("removeJSON args=%q json=%v", args, jsonOutput)
	}
}

func TestParseConfineManagementAndJSON(t *testing.T) {
	args, jsonOutput := removeJSON([]string{"confine", "--list", "--owner", "session-a", "--json"})
	if !jsonOutput {
		t.Fatal("management --json was not recognized")
	}
	positional, options, err := parseArgs("confine", args[1:])
	if err != nil || len(positional) != 0 || options["list"] != "true" || options["owner"] != "session-a" {
		t.Fatalf("positional=%v options=%v err=%v", positional, options, err)
	}
	_, options, err = parseArgs("confine", []string{"--kill", "CONFINE-job-1-a", "--steal", "--slice", "aira.slice"})
	if err != nil || options["kill"] != "CONFINE-job-1-a" || options["steal"] != "true" {
		t.Fatalf("options=%v err=%v", options, err)
	}
	for _, argv := range [][]string{{"--list", "--kill", "x"}, {"--list", "--steal"}, {"--kill"}, {"--list", "--owner", "../bad"}} {
		if _, _, err := parseArgs("confine", argv); err == nil || !strings.HasPrefix(err.Error(), "E_CONFINE_ARGUMENT_INVALID:") {
			t.Fatalf("argv=%v err=%v", argv, err)
		}
	}
}

func TestConfineOwnerDerivationChain(t *testing.T) {
	t.Setenv("AIRA_CONFINE_OWNER", "environment-owner")
	if owner, err := resolveConfineOwner(context.Background(), "flag-owner"); err != nil || owner != "flag-owner" {
		t.Fatalf("flag owner=%q err=%v", owner, err)
	}
	if owner, err := resolveConfineOwner(context.Background(), ""); err != nil || owner != "environment-owner" {
		t.Fatalf("environment owner=%q err=%v", owner, err)
	}
	t.Setenv("AIRA_CONFINE_OWNER", "")
	projectRoot := t.TempDir()
	if err := exec.Command("git", "-C", projectRoot, "init", "-q").Run(); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(projectRoot, ".aira"), 0o755); err != nil {
		t.Fatal(err)
	}
	config := `{"schema":1,"project":{"slug":"demo","prefixes":["AIRA"]},"lease":{"ttl_seconds":900,"heartbeat_seconds":30}}`
	if err := os.WriteFile(filepath.Join(projectRoot, ".aira", "config"), []byte(config+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Chdir(projectRoot)
	project, err := app.Discover(context.Background(), ".")
	if err != nil {
		t.Fatal(err)
	}
	if owner, err := resolveConfineOwner(context.Background(), ""); err != nil || owner != project.WorktreeID {
		t.Fatalf("project owner=%q want=%q err=%v", owner, project.WorktreeID, err)
	}
	t.Chdir(t.TempDir())
	if owner, err := resolveConfineOwner(context.Background(), ""); err != nil || owner != runner.ConfineUnknownOwner {
		t.Fatalf("unknown owner=%q err=%v", owner, err)
	}
	if _, err := resolveConfineOwner(context.Background(), "bad/owner"); err == nil {
		t.Fatal("invalid explicit owner accepted")
	}
}

func TestConfineLaunchOwnerThreadsIntoAdmissionRequest(t *testing.T) {
	original := runConfined
	t.Cleanup(func() { runConfined = original })
	t.Setenv("AIRA_CONFINE_OWNER", "session-launch")
	runConfined = func(_ context.Context, request runner.ConfineRequest) (runner.ConfineResult, error) {
		if request.Owner != "session-launch" {
			t.Fatalf("owner=%q", request.Owner)
		}
		return runner.ConfineResult{}, nil
	}
	if exit := runWithInput([]string{"confine", "--owner", "session-launch", "--", "true"}, io.Discard, io.Discard, strings.NewReader("")); exit != 0 {
		t.Fatalf("exit=%d", exit)
	}
}

func TestConfineManagementDispatchesOutsideProject(t *testing.T) {
	t.Chdir(t.TempDir())
	t.Setenv("AIRA_CONFINE_OWNER", "session-a")
	called := false
	dispatch := dispatcherFunc(func(_ context.Context, scope daemon.WorktreeScope, request core.Request) core.Response {
		called = true
		if scope.Root != "" || request.Verb != "confine-kill" || request.Args["owner"] != "session-a" || request.Args["selector"] != "job" || request.Args["steal"] != true {
			t.Fatalf("scope=%+v request=%+v", scope, request)
		}
		return core.Response{OK: true, Code: "OK", Data: runner.ConfineKillResult{Status: "killed", ScopeID: "CONFINE-job-1-a", Name: "job", Owner: "session-a"}}
	})
	var stdout, stderr bytes.Buffer
	if exit := runWithInputDispatcher([]string{"confine", "--kill", "job", "--steal"}, &stdout, &stderr, strings.NewReader(""), dispatch); exit != 0 || !called {
		t.Fatalf("exit=%d called=%v stdout=%q stderr=%q", exit, called, stdout.String(), stderr.String())
	}
}

func TestConfineListRendersHumanTableAndAllowsJSON(t *testing.T) {
	zero, pid, rss, age, cap := 0, 123, int64(4096), int64(2), "8192"
	result := runner.ConfineListResult{Verdict: "pass", Scopes: []runner.ConfineRecord{{
		Name: "husk", Owner: runner.ConfineUnknownOwner, SupervisorPID: &pid,
		ScopeID: "CONFINE-husk-123-abc", Populated: &zero, RSSBytes: &rss, AgeSeconds: &age, Cap: &cap,
	}}}
	dispatch := dispatcherFunc(func(_ context.Context, _ daemon.WorktreeScope, request core.Request) core.Response {
		if request.Verb != "confine-list" {
			t.Fatalf("request=%+v", request)
		}
		return core.Response{OK: true, Code: "OK", Data: result}
	})
	var stdout, stderr bytes.Buffer
	if exit := runWithInputDispatcher([]string{"confine", "--list"}, &stdout, &stderr, strings.NewReader(""), dispatch); exit != 0 || !strings.Contains(stdout.String(), "SUPERVISOR-PID") || !strings.Contains(stdout.String(), "husk") || !strings.Contains(stdout.String(), "  0") {
		t.Fatalf("exit=%d stdout=%q stderr=%q", exit, stdout.String(), stderr.String())
	}
	stdout.Reset()
	stderr.Reset()
	if exit := runWithInputDispatcher([]string{"confine", "--list", "--json"}, &stdout, &stderr, strings.NewReader(""), dispatch); exit != 0 || !strings.Contains(stdout.String(), `"populated":0`) {
		t.Fatalf("json exit=%d stdout=%q stderr=%q", exit, stdout.String(), stderr.String())
	}
}

func TestRenderConfineListReserveSummary(t *testing.T) {
	tests := []struct {
		name    string
		reserve *runner.ConfineSliceReserve
		want    string
	}{
		{
			name:    "present",
			reserve: &runner.ConfineSliceReserve{GrantedBytes: 3 << 30, CeilingBytes: 12 << 30, Jobs: 1},
			want:    "slice reserve: 3G granted / 12G ceiling across 1 admitted job\n",
		},
		{
			// An idle slice (0 granted) renders a genuine zero as "0B", never
			// "unknown" — reserve/ceiling here are always established values.
			name:    "idle-zero",
			reserve: &runner.ConfineSliceReserve{GrantedBytes: 0, CeilingBytes: 12 << 30, Jobs: 0},
			want:    "slice reserve: 0B granted / 12G ceiling across 0 admitted jobs\n",
		},
		{name: "unavailable"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			result := runner.ConfineListResult{Verdict: "pass", Scopes: []runner.ConfineRecord{}, SliceReserve: test.reserve}
			var stdout, stderr bytes.Buffer
			exit := renderConfineListResponse(core.Response{OK: true, Code: "OK", Data: result}, &stdout, &stderr)
			if exit != 0 || stderr.Len() != 0 {
				t.Fatalf("exit=%d stderr=%q", exit, stderr.String())
			}
			if gotSummary := strings.Contains(stdout.String(), "slice reserve:"); gotSummary != (test.reserve != nil) {
				t.Fatalf("stdout=%q", stdout.String())
			}
			if test.want != "" && !strings.Contains(stdout.String(), test.want) {
				t.Fatalf("stdout=%q, want summary %q", stdout.String(), test.want)
			}
		})
	}
}

func TestConfineDaemonDownRequiresSteal(t *testing.T) {
	d := &daemonDispatcher{}
	d.exchange = func(context.Context, string, daemon.RequestFrame) (daemon.ResponseFrame, error) {
		return daemon.ResponseFrame{}, &daemon.RequestNotSentError{Err: errors.New(daemon.CodeUnavailable + ": down")}
	}
	d.resolveConfineSlice = func(string) (string, string, error) { return "aira.slice", filepath.Join("/unused", "slice"), nil }
	d.killConfine = func(_ context.Context, _ string, selector, caller string, steal bool, _ []runner.ConfineRegistryEntry, fresh runner.ConfineOwnerLookup) (runner.ConfineKillResult, error) {
		if selector != "job" || caller != "session-a" || fresh != nil {
			t.Fatalf("selector=%q caller=%q fresh=%v", selector, caller, fresh != nil)
		}
		if !steal {
			return runner.ConfineKillResult{}, errors.New(runner.CodeConfineOwnerUnverified + ": daemon down")
		}
		return runner.ConfineKillResult{Status: "killed", ScopeID: "CONFINE-job-1-a"}, nil
	}
	base := core.Request{Verb: "confine-kill", Args: map[string]any{"selector": "job", "owner": "session-a", "steal": false}}
	if response := d.Dispatch(context.Background(), daemon.WorktreeScope{}, base); response.Code != runner.CodeConfineOwnerUnverified || response.OK {
		t.Fatalf("without steal response=%+v", response)
	}
	base.Args["steal"] = true
	if response := d.Dispatch(context.Background(), daemon.WorktreeScope{}, base); !response.OK || response.Code != "OK" {
		t.Fatalf("with steal response=%+v", response)
	}
}

func TestConfineRunsDirectlyWithoutProjectOrDispatcher(t *testing.T) {
	original := runConfined
	t.Cleanup(func() { runConfined = original })
	called := false
	runConfined = func(_ context.Context, request runner.ConfineRequest) (runner.ConfineResult, error) {
		called = true
		if request.Slice != "test.slice" || request.Name != "job" || !reflect.DeepEqual(request.Argv, []string{"tool", "--flag"}) {
			t.Fatalf("request=%+v", request)
		}
		return runner.ConfineResult{Exit: 27}, nil
	}
	var stdout, stderr bytes.Buffer
	exit := runWithInput([]string{"confine", "--slice", "test.slice", "--name", "job", "--", "tool", "--flag"}, &stdout, &stderr, strings.NewReader("stdin"))
	if !called || exit != 27 || stdout.Len() != 0 || stderr.Len() != 0 {
		t.Fatalf("called=%v exit=%d stdout=%q stderr=%q", called, exit, stdout.String(), stderr.String())
	}
}

// covers: task-57 confine CLI parsing and request threading.
func TestConfineMemoryFlagsThreadIntoRequest(t *testing.T) {
	original := runConfined
	t.Cleanup(func() { runConfined = original })
	runConfined = func(_ context.Context, request runner.ConfineRequest) (runner.ConfineResult, error) {
		if request.ScopeMemoryMax != 32<<20 || request.ScopeMemoryHigh != 16<<20 {
			t.Fatalf("request=%+v", request)
		}
		return runner.ConfineResult{}, nil
	}
	if exit := runWithInput([]string{"confine", "--memory-max", "32M", "--memory-high", "16m", "--", "true"}, io.Discard, io.Discard, strings.NewReader("")); exit != 0 {
		t.Fatalf("exit=%d", exit)
	}
}

func TestConfineAdmitTimeoutValidationAndThreading(t *testing.T) {
	original := runConfined
	t.Cleanup(func() { runConfined = original })
	for _, raw := range []string{"0", "-1s"} {
		if _, _, err := parseArgs("confine", []string{"--admit-timeout", raw, "--", "true"}); err == nil || !strings.Contains(err.Error(), "--admit-timeout") {
			t.Fatalf("--admit-timeout %q err=%v, want CLI rejection", raw, err)
		}
	}
	runConfined = func(_ context.Context, request runner.ConfineRequest) (runner.ConfineResult, error) {
		if request.AdmissionMaxWait != 25*time.Millisecond {
			t.Fatalf("AdmissionMaxWait=%s want 25ms", request.AdmissionMaxWait)
		}
		return runner.ConfineResult{}, nil
	}
	if exit := runWithInput([]string{"confine", "--admit-timeout", "25ms", "--", "true"}, io.Discard, io.Discard, strings.NewReader("")); exit != 0 {
		t.Fatalf("exit=%d", exit)
	}
}

func TestConfineDelegateRAMFlagThreadsIntoRequest(t *testing.T) {
	original := runConfined
	t.Cleanup(func() { runConfined = original })
	runConfined = func(_ context.Context, request runner.ConfineRequest) (runner.ConfineResult, error) {
		if !request.DelegateRAM {
			t.Fatalf("request=%+v", request)
		}
		return runner.ConfineResult{}, nil
	}
	if exit := runWithInput([]string{"confine", "--delegate-ram", "--memory-reserve", "512M", "--", "pytest", "-q"}, io.Discard, io.Discard, strings.NewReader("")); exit != 0 {
		t.Fatalf("exit=%d", exit)
	}
}

func TestConfineReserveCLIParsesPinnedRequestAndHoldsUntilStdinClose(t *testing.T) {
	original := reserveConfined
	t.Cleanup(func() { reserveConfined = original })
	granted := make(chan runner.ConfineReserveRequest, 1)
	reserveConfined = func(_ context.Context, request runner.ConfineReserveRequest) (*runner.ConfineReservation, error) {
		granted <- request
		return &runner.ConfineReservation{State: "immediate", Reserve: request.Bytes, Basis: "pinned:client"}, nil
	}
	reader, writer := io.Pipe()
	var stdout, stderr bytes.Buffer
	done := make(chan int, 1)
	go func() {
		done <- runWithInput([]string{"confine-reserve", "--bytes", "512M", "--pinned", "--signature", "pytest:test_x.py::test_y", "--max-wait", "5s"}, &stdout, &stderr, reader)
	}()
	request := <-granted
	if request.Bytes != 512<<20 || !request.Pinned || request.Signature != "pytest:test_x.py::test_y" || request.MaxWait != 5*time.Second {
		t.Fatalf("request=%+v", request)
	}
	select {
	case exit := <-done:
		t.Fatalf("helper exited before stdin close: %d", exit)
	case <-time.After(20 * time.Millisecond):
	}
	_ = writer.Close()
	if exit := <-done; exit != 0 || stdout.String() != "granted reserve=536870912 basis=pinned:client\n" || stderr.Len() != 0 {
		t.Fatalf("exit=%d stdout=%q stderr=%q", exit, stdout.String(), stderr.String())
	}
}

func TestConfinePinnedReserveFlagEnvironmentAndMemoryMaxPrecedence(t *testing.T) {
	original := runConfined
	t.Cleanup(func() { runConfined = original })
	for _, test := range []struct {
		name string
		env  string
		argv []string
		want int64
	}{
		{name: "flag", env: "8M", argv: []string{"confine", "--memory-reserve", "12M", "--", "true"}, want: 12 << 20},
		{name: "environment", env: "8M", argv: []string{"confine", "--", "true"}, want: 8 << 20},
		{name: "memory-max", env: "8M", argv: []string{"confine", "--memory-reserve", "12M", "--memory-max", "16M", "--", "true"}, want: 16 << 20},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Setenv("AIRA_CONFINE_RESERVE", test.env)
			runConfined = func(_ context.Context, request runner.ConfineRequest) (runner.ConfineResult, error) {
				if !request.MemoryReservePinned || request.MemoryReserve != test.want {
					t.Fatalf("request=%+v", request)
				}
				return runner.ConfineResult{}, nil
			}
			if exit := runWithInput(test.argv, io.Discard, io.Discard, strings.NewReader("")); exit != 0 {
				t.Fatalf("exit=%d", exit)
			}
		})
	}
}

// verifies: task-57 confine rejects invalid cap relationships before launch.
func TestConfineMemoryFlagValidation(t *testing.T) {
	for _, argv := range [][]string{
		{"--memory-high", "1M", "--", "true"},
		{"--memory-high", "0", "--", "true"},
		{"--memory-max", "0", "--", "true"},
		{"--memory-max", "1023K", "--", "true"},
		{"--memory-max", "2M", "--memory-high", "3M", "--", "true"},
		{"--memory-max", "garbage", "--", "true"},
		{"--memory-reserve", "garbage", "--", "true"},
		{"--memory-reserve", "1023K", "--", "true"},
	} {
		if _, _, err := parseArgs("confine", argv); err == nil || !strings.HasPrefix(err.Error(), "E_CONFINE_ARGUMENT_INVALID:") {
			t.Fatalf("parseArgs(%q) err=%v", argv, err)
		}
	}
}

func TestConfineInfraErrorUsesDedicatedExitAndDoesNotDispatch(t *testing.T) {
	original := runConfined
	t.Cleanup(func() { runConfined = original })
	runConfined = func(context.Context, runner.ConfineRequest) (runner.ConfineResult, error) {
		return runner.ConfineResult{}, errors.New("E_CONFINE_UNAVAILABLE: slice missing.slice: not found")
	}
	var stderr bytes.Buffer
	if exit := runWithInput([]string{"confine", "--slice", "missing.slice", "--", "must-not-run"}, io.Discard, &stderr, strings.NewReader("")); exit != 4 {
		t.Fatalf("exit=%d stderr=%q", exit, stderr.String())
	}
	if !strings.Contains(stderr.String(), "E_CONFINE_UNAVAILABLE") {
		t.Fatalf("stderr=%q", stderr.String())
	}
}

func TestParseConfineArgsRejectsUnsafeShapes(t *testing.T) {
	for _, argv := range [][]string{
		{"true"},
		{"--", ""},
		{"--unsafe-unconfined", "--", "true"},
		{"--slice", "one", "--slice", "two", "--", "true"},
		{"positional", "--", "true"},
	} {
		if _, _, err := parseArgs("confine", argv); err == nil || !strings.Contains(err.Error(), "E_CONFINE_ARGUMENT_INVALID") {
			t.Fatalf("parseArgs(%q) err=%v", argv, err)
		}
	}
}

func TestConfineDescriptorIsClientExecuteWithoutMCP(t *testing.T) {
	canonical, route := core.Classify("confine", "")
	if canonical != "confine" || route != core.RouteClient {
		t.Fatalf("classify=%q/%v", canonical, route)
	}
	var found bool
	for _, descriptor := range core.New(nil).DispatchDescriptors() {
		if descriptor.Name != "confine" {
			continue
		}
		found = true
		if descriptor.Safety != core.SafetyExecute || descriptor.MCPTool != "" || descriptor.Include || descriptor.Usage != "confine [--slice S] [--name N] [--owner ID] [--memory-reserve S] [--memory-max S] [--memory-high S] [--admit-timeout D] [--delegate-ram] -- <argv...>" {
			t.Fatalf("descriptor=%+v", descriptor)
		}
	}
	if !found {
		t.Fatal("confine descriptor missing")
	}
}

func TestConfineReserveDescriptorIsCLIOnly(t *testing.T) {
	canonical, route := core.Classify("confine-reserve", "")
	if canonical != "confine-reserve" || route != core.RouteClient {
		t.Fatalf("classify=%q/%v", canonical, route)
	}
	response := core.New(nil).Do(context.Background(), core.Request{Verb: "confine-reserve", Args: map[string]any{
		"bytes": "1", "pinned": true, "signature": "pytest:x",
	}})
	if response.Code != "E_CONFINE_UNAVAILABLE" || response.OK {
		t.Fatalf("response=%+v", response)
	}
	for _, descriptor := range core.New(nil).DispatchDescriptors() {
		if descriptor.Name == "confine-reserve" && (descriptor.Include || descriptor.MCPTool != "") {
			t.Fatalf("descriptor=%+v", descriptor)
		}
	}
}
