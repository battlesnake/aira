package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"testing"

	"aira/internal/core"
	"aira/internal/daemon"
	"aira/internal/runner"
)

// TestConfineStdinConnectRequiresDetachAndIsTranscribedNeverInferred is the CLI
// half of AIRA-196's default-off invariant.
//
// Two properties, and both matter:
//
//   - a foreground `aira confine --stdin-connect` is REFUSED, because a
//     foreground confine already passes the caller's own stdin through and
//     accepting the flag there would be a request silently discarded;
//   - the flag is TRANSCRIBED, so a detached launch without it carries
//     StdinConnect false into the supervisor -- which is what keeps the job's
//     stdin on /dev/null.
//
// verifies: AIRA-196
func TestConfineStdinConnectRequiresDetachAndIsTranscribedNeverInferred(t *testing.T) {
	if _, _, err := parseArgs("confine", []string{"--stdin-connect", "--", "cat"}); err == nil {
		t.Fatal("--stdin-connect without --detach was accepted; a foreground confine already has a real stdin")
	} else if !strings.Contains(err.Error(), "requires --detach") {
		t.Fatalf("error %q does not say the flag requires --detach", err)
	}
	if _, _, err := parseArgs("confine", []string{"--detach", "--stdin-connect", "--stdin-connect", "--", "cat"}); err == nil {
		t.Fatal("--stdin-connect twice was accepted")
	}
	// It is not a management option either: `aira confine --stdin-connect --list`
	// must be an argument error, never a silently ignored no-op.
	if _, _, err := parseArgs("confine", []string{"--list", "--stdin-connect"}); err == nil {
		t.Fatal("--stdin-connect was accepted on the management form")
	}

	original := launchConfineDetached
	t.Cleanup(func() { launchConfineDetached = original })
	for _, test := range []struct {
		argv []string
		want bool
	}{
		{argv: []string{"confine", "--detach", "--", "cat"}, want: false},
		{argv: []string{"confine", "--detach", "--stdin-connect", "--", "cat"}, want: true},
	} {
		observed := false
		seen := false
		launchConfineDetached = func(_ context.Context, request runner.ConfineRequest) (*runner.ConfineDetachLaunch, error) {
			seen, observed = true, request.StdinConnect
			return nil, errors.New("E_CONFINE_DETACH_FAILED: stop here; the transcription is what is under test")
		}
		_ = runWithInput(test.argv, io.Discard, io.Discard, strings.NewReader(""))
		if !seen {
			t.Fatalf("%v never reached the detached launch", test.argv)
		}
		if observed != test.want {
			t.Fatalf("%v transcribed StdinConnect=%v, want %v", test.argv, observed, test.want)
		}
	}
}

// TestConfineLogAndInputDispatchProjectlessly pins the CLI wiring: both verbs
// resolve NO project (an empty scope), address the job by its own selector, and
// carry the resolved owner. Routing them through project discovery would make
// them refuse to run in most directories an operator is standing in when they
// want to read a running gate's log.
//
// verifies: AIRA-196
func TestConfineLogAndInputDispatchProjectlessly(t *testing.T) {
	t.Chdir(t.TempDir())
	t.Setenv("AIRA_CONFINE_OWNER", "session-a")

	var captured core.Request
	dispatch := dispatcherFunc(func(_ context.Context, scope daemon.WorktreeScope, request core.Request) core.Response {
		if scope.Root != "" {
			t.Fatalf("a project scope was resolved for %s: %+v", request.Verb, scope)
		}
		captured = request
		return core.Response{OK: true, Code: "OK", Data: &runner.ConfineLogChunk{
			ScopeID: "CONFINE-gate-9-a@session-a", Name: "gate", Owner: "session-a",
			Stream: "err", Path: "/state/stderr", Encoding: "base64",
			Bytes: []byte("boom\n"), NextOffset: 5, TotalBytes: 5,
			State: runner.ConfineDetachRunning, Filtered: true, Grep: "boom",
		}}
	})

	var stdout, stderr bytes.Buffer
	exit := runWithInputDispatcher([]string{
		"confine-log", "gate", "--stream", "err", "--tail", "64", "--grep", "boom", "--follow",
	}, &stdout, &stderr, strings.NewReader(""), dispatch)
	if exit != 0 {
		t.Fatalf("exit=%d stderr=%q", exit, stderr.String())
	}
	if captured.Verb != "confine-log" {
		t.Fatalf("verb=%q", captured.Verb)
	}
	for name, want := range map[string]any{
		"selector": "gate", "stream": "err", "tail": "64", "grep": "boom",
		"follow": true, "full": false, "owner": "session-a",
	} {
		if captured.Args[name] != want {
			t.Fatalf("arg %s=%#v, want %#v", name, captured.Args[name], want)
		}
	}
	// The bytes go to stdout unaltered; the metadata goes to stderr as one JSON
	// line. A caller piping confine-log gets the log, not an envelope.
	if stdout.String() != "boom\n" {
		t.Fatalf("stdout=%q, want the captured bytes verbatim", stdout.String())
	}
	var metadata map[string]any
	if err := json.Unmarshal(stderr.Bytes(), &metadata); err != nil {
		t.Fatalf("stderr is not one JSON metadata line: %q (%v)", stderr.String(), err)
	}
	// filtered/truncated/state are how a reader knows the bytes above are not
	// necessarily the whole story. They must be present, not implied.
	for _, key := range []string{"scope_id", "stream", "path", "offset", "next_offset", "total_bytes", "complete", "truncated", "filtered", "grep", "state"} {
		if _, ok := metadata[key]; !ok {
			t.Fatalf("metadata is missing %q: %v", key, metadata)
		}
	}
	if metadata["filtered"] != true || metadata["grep"] != "boom" {
		t.Fatalf("a filtered read did not say so: %v", metadata)
	}

	stdout.Reset()
	stderr.Reset()
	inputDispatch := dispatcherFunc(func(_ context.Context, scope daemon.WorktreeScope, request core.Request) core.Response {
		if scope.Root != "" {
			t.Fatalf("a project scope was resolved for %s: %+v", request.Verb, scope)
		}
		captured = request
		return core.Response{OK: true, Code: "OK", Data: &runner.ConfineInputResult{ScopeID: "CONFINE-gate-9-a@session-a", Accepted: 5, Closed: true}}
	})
	if exit := runWithInputDispatcher([]string{"confine-input", "gate", "--close", "--steal"},
		&stdout, &stderr, strings.NewReader("hi\n"), inputDispatch); exit != 0 {
		t.Fatalf("exit=%d stderr=%q", exit, stderr.String())
	}
	if captured.Verb != "confine-input" {
		t.Fatalf("verb=%q", captured.Verb)
	}
	for name, want := range map[string]any{"selector": "gate", "close": true, "steal": true, "owner": "session-a"} {
		if captured.Args[name] != want {
			t.Fatalf("arg %s=%#v, want %#v", name, captured.Args[name], want)
		}
	}
	// `data` must be ABSENT from a CLI request: its absence is what makes the
	// dispatcher read the caller's real stdin. Present-but-empty would silently
	// send nothing.
	if _, present := captured.Args["data"]; present {
		t.Fatalf("the CLI set a data argument, which suppresses stdin: %+v", captured.Args)
	}
}

// TestConfineLogAndInputSelectorIsRequired keeps the by-handle contract: neither
// verb has a path argument, and neither has a "just show me everything" form
// that could read the wrong job.
//
// verifies: AIRA-196
func TestConfineLogAndInputSelectorIsRequired(t *testing.T) {
	for _, verb := range []string{"confine-log", "confine-input"} {
		if _, err := buildRequest(verb, nil, map[string]string{}); err == nil {
			t.Fatalf("%s accepted no selector", verb)
		}
		if _, err := buildRequest(verb, []string{"a", "b"}, map[string]string{}); err == nil {
			t.Fatalf("%s accepted two selectors", verb)
		}
		// --slice belongs to the cgroup verbs. Accepting and discarding it here
		// would be the silently ignored scope AIRA-82 refuses.
		if _, _, err := parseArgs(verb, []string{"gate", "--slice", "aira.slice"}); err == nil {
			t.Fatalf("%s accepted --slice", verb)
		}
	}
}

// TestConfineLogReachesNoDaemon is the AIRA-22 survivability property carried
// forward to the read half: `confine-log` must answer from the durable record
// store with no daemon round trip, because the daemon is the component most
// likely to have been restarted during exactly the long pause a detached job
// exists to survive.
//
// The dispatcher is a REAL daemonDispatcher whose transport fails the test if it
// is used at all, so this cannot pass by accident.
//
// verifies: AIRA-196
func TestConfineLogReachesNoDaemon(t *testing.T) {
	dispatcher := &daemonDispatcher{
		paths: daemon.Paths{ConfineDetachDir: "/state/confine"},
		exchange: func(context.Context, string, daemon.RequestFrame) (daemon.ResponseFrame, error) {
			t.Fatal("confine-log opened a daemon exchange")
			return daemon.ResponseFrame{}, nil
		},
		storeOpExchange: func(context.Context, string, daemon.StoreOpFrame) (daemon.ResponseFrame, error) {
			t.Fatal("confine-log opened a daemon store exchange")
			return daemon.ResponseFrame{}, nil
		},
		spawn: func() (<-chan childResult, error) {
			t.Fatal("confine-log tried to start a daemon")
			return nil, nil
		},
	}
	var readRoot string
	var readRequest runner.ConfineLogRequest
	dispatcher.readConfineLog = func(_ context.Context, root string, request runner.ConfineLogRequest) (*runner.ConfineLogChunk, error) {
		readRoot, readRequest = root, request
		return &runner.ConfineLogChunk{ScopeID: "CONFINE-gate-9-a@session-a", Stream: "out"}, nil
	}
	response := dispatcher.Dispatch(context.Background(), daemon.WorktreeScope{}, core.Request{
		Verb: "confine-log", Args: map[string]any{"selector": "gate", "owner": "session-a", "from": "12"},
	})
	if !response.OK {
		t.Fatalf("response=%+v", response)
	}
	if readRoot != "/state/confine" {
		t.Fatalf("read root=%q, want the durable confine record directory", readRoot)
	}
	if readRequest.Selector != "gate" || readRequest.Owner != "session-a" || readRequest.From != 12 {
		t.Fatalf("request=%+v", readRequest)
	}

	// The same for confine-input's resolution: the daemon is not on its path
	// either. (Its WRITE side needs the job's own supervisor, which is a
	// different dependency and an unavoidable one.)
	var inputRoot string
	var inputRequest runner.ConfineInputRequest
	dispatcher.confineInput = func(_ context.Context, root string, request runner.ConfineInputRequest) (*runner.ConfineInputResult, error) {
		inputRoot, inputRequest = root, request
		return &runner.ConfineInputResult{ScopeID: "CONFINE-gate-9-a@session-a", Closed: true}, nil
	}
	response = dispatcher.Dispatch(context.Background(), daemon.WorktreeScope{}, core.Request{
		Verb: "confine-input", Args: map[string]any{"selector": "gate", "owner": "session-a", "close": true},
	})
	if !response.OK {
		t.Fatalf("response=%+v", response)
	}
	if inputRoot != "/state/confine" || inputRequest.Selector != "gate" || !inputRequest.Close {
		t.Fatalf("root=%q request=%+v", inputRoot, inputRequest)
	}
}

// TestConfineInputPartialDeliveryTravelsWithItsRefusal pins that a failure still
// reports the byte count. How much of an injection landed is the one thing an
// operator cannot reconstruct afterwards, so discarding it with the error would
// leave them unable to retry safely.
//
// verifies: AIRA-196
func TestConfineInputPartialDeliveryTravelsWithItsRefusal(t *testing.T) {
	dispatcher := &daemonDispatcher{paths: daemon.Paths{ConfineDetachDir: "/state/confine"}}
	dispatcher.confineInput = func(context.Context, string, runner.ConfineInputRequest) (*runner.ConfineInputResult, error) {
		return &runner.ConfineInputResult{ScopeID: "CONFINE-gate-9-a@session-a", Accepted: 41},
			&runner.RunInputError{Code: "E_RUN_INPUT_PARTIAL", Committed: 41, Err: errors.New("child stdin closed mid-stream")}
	}
	response := dispatcher.Dispatch(context.Background(), daemon.WorktreeScope{}, core.Request{
		Verb: "confine-input", Args: map[string]any{"selector": "gate", "owner": "session-a", "close": true},
	})
	if response.OK || response.Code != "E_RUN_INPUT_PARTIAL" {
		t.Fatalf("response=%+v", response)
	}
	data, ok := response.Data.(map[string]any)
	if !ok {
		t.Fatalf("data=%#v", response.Data)
	}
	if data["accepted"] != int64(41) || data["scope_id"] != "CONFINE-gate-9-a@session-a" {
		t.Fatalf("the partial delivery was not reported: %v", data)
	}
}

// TestConfineLogAndInputDescriptorsAreIncludedMCPTools pins the dispatch-table
// registration. Unlike confine and confine-status these ARE generated actions
// and MCP tools: reading a captured file and writing to a socket both have
// honest request/response forms, and an agent driving a detached gate is exactly
// who needs them.
//
// verifies: AIRA-196
func TestConfineLogAndInputDescriptorsAreIncludedMCPTools(t *testing.T) {
	for _, want := range []struct {
		verb   string
		tool   string
		safety core.SafetyClass
	}{
		{verb: "confine-log", tool: "aira_confine_log", safety: core.SafetyRead},
		{verb: "confine-input", tool: "aira_confine_input", safety: core.SafetyExecute},
	} {
		canonical, route := core.Classify(want.verb, "")
		if canonical != want.verb || route != core.RouteClient {
			t.Fatalf("%s classify=%q/%v, want client-routed", want.verb, canonical, route)
		}
		if !core.StoreFreeCarved(want.verb, nil) {
			t.Fatalf("%s must be store-free: it resolves no project", want.verb)
		}
		if verbAcceptsScopeDir(want.verb) || toolAcceptsScopeDir(want.tool) {
			t.Fatalf("%s resolves no project and must refuse a scope-dir override", want.verb)
		}
		var found bool
		for _, descriptor := range core.New(nil).DispatchDescriptors() {
			if descriptor.Name != want.verb {
				continue
			}
			found = true
			if descriptor.MCPTool != want.tool || !descriptor.Include || descriptor.Safety != want.safety {
				t.Fatalf("%s descriptor=%+v", want.verb, descriptor)
			}
			if descriptor.Destructive {
				t.Fatalf("%s is not destructive", want.verb)
			}
		}
		if !found {
			t.Fatalf("%s descriptor missing", want.verb)
		}
		// The in-daemon handler must REFUSE rather than half-answer: these verbs
		// are served client-side, and a core handler that returned something would
		// be a second implementation free to disagree.
		response := core.New(nil).Do(context.Background(), core.Request{Verb: want.verb, Args: map[string]any{"selector": "gate"}})
		if response.OK || response.Code != "E_CONFINE_UNAVAILABLE" {
			t.Fatalf("%s must refuse daemon routing: %+v", want.verb, response)
		}
	}
}

// TestRunLogAcceptsGrep pins that AIRA-196 closed the gap in the EXISTING verb
// too, not only the new one. One filter language across the pair is what stops
// an operator learning two.
//
// verifies: AIRA-196
func TestRunLogAcceptsGrep(t *testing.T) {
	_, options, err := parseArgs("run-log", []string{"RUN-1", "--grep", "^FAIL", "--tail", "512"})
	if err != nil {
		t.Fatalf("run-log --grep: %v", err)
	}
	if options["grep"] != "^FAIL" {
		t.Fatalf("options=%v", options)
	}
	request, err := buildRequest("run-log", []string{"RUN-1"}, options)
	if err != nil {
		t.Fatal(err)
	}
	if request.Args["grep"] != "^FAIL" {
		t.Fatalf("args=%v", request.Args)
	}
	var declared bool
	for _, descriptor := range core.New(nil).DispatchDescriptors() {
		if descriptor.Name != "run-log" {
			continue
		}
		for _, arg := range descriptor.Args {
			if arg.Name == "grep" {
				declared = true
			}
		}
	}
	if !declared {
		t.Fatal("run-log's descriptor does not declare --grep, so MCP callers cannot reach it")
	}
}
