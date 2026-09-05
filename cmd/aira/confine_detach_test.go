package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"testing"

	"aira/internal/runner"
)

// verifies: AIRA-22
func TestConfineDetachArgumentParsing(t *testing.T) {
	for _, test := range []struct {
		name string
		argv []string
		want string
	}{
		{
			name: "detach without a launch target names the delimiter, not the management verbs",
			argv: []string{"--detach"},
			want: "--detach requires a launch target after --",
		},
		{
			name: "detach with only a name and no target still names the delimiter",
			argv: []string{"--detach", "--name", "gate"},
			want: "--detach requires a launch target after --",
		},
		{name: "detach may occur once", argv: []string{"--detach", "--detach", "--", "true"}, want: "may occur once"},
		{name: "status and list are mutually exclusive", argv: []string{"--list", "--status", "gate"}, want: "exactly one of"},
		{name: "status and kill are mutually exclusive", argv: []string{"--kill", "gate", "--status", "gate"}, want: "exactly one of"},
		{name: "steal is kill-only", argv: []string{"--status", "gate", "--steal"}, want: "--steal is valid only with --kill"},
		{name: "empty inline status selector is refused", argv: []string{"--status="}, want: "--status requires a value"},
		{name: "no management verb at all", argv: []string{"--owner", "session-a"}, want: "exactly one of"},
	} {
		t.Run(test.name, func(t *testing.T) {
			_, _, err := parseArgs("confine", test.argv)
			if err == nil {
				t.Fatalf("argv %v was accepted", test.argv)
			}
			if !strings.Contains(err.Error(), test.want) {
				t.Fatalf("error %q does not contain %q", err, test.want)
			}
		})
	}
	// Accepted forms.
	for _, test := range []struct {
		argv     []string
		selector string
		hasSel   bool
	}{
		{argv: []string{"--status"}, hasSel: false},
		{argv: []string{"--status", "gate"}, selector: "gate", hasSel: true},
		{argv: []string{"--status=gate"}, selector: "gate", hasSel: true},
		{argv: []string{"--status", "--owner", "session-a"}, hasSel: false},
	} {
		_, options, err := parseArgs("confine", test.argv)
		if err != nil {
			t.Fatalf("argv %v: %v", test.argv, err)
		}
		if options["status"] != "true" {
			t.Fatalf("argv %v did not select the status form: %v", test.argv, options)
		}
		got, present := options["status-selector"]
		if present != test.hasSel || got != test.selector {
			t.Fatalf("argv %v selector=%q present=%v, want %q/%v", test.argv, got, present, test.selector, test.hasSel)
		}
	}
	if _, options, err := parseArgs("confine", []string{"--detach", "--name", "gate", "--", "make", "merge-gate"}); err != nil || options["detach"] != "true" {
		t.Fatalf("a valid detached launch was refused: %v %v", options, err)
	}
}

// TestConfineDetachLaunchOutputNeverImpliesTheJobSucceeded guards the largest
// false-pass risk in this verb: `--detach` exits 0 while the job's outcome is
// still unknown, and an automated caller reading only the exit code would take
// that as success. The wording is load-bearing, so it is pinned.
//
// verifies: AIRA-22
func TestConfineDetachLaunchOutputNeverImpliesTheJobSucceeded(t *testing.T) {
	original := launchConfineDetached
	t.Cleanup(func() { launchConfineDetached = original })
	var seen runner.ConfineRequest
	acknowledged := 0
	launchConfineDetached = func(_ context.Context, request runner.ConfineRequest) (*runner.ConfineDetachLaunch, error) {
		seen = request
		return &runner.ConfineDetachLaunch{
			ScopeID: "CONFINE-gate-99-abc@session-a", Slice: "aira.slice", SupervisorPID: 99,
			RecordDir:  "/state/aira/confine/CONFINE-gate-99-abc@session-a",
			RecordPath: "/state/aira/confine/CONFINE-gate-99-abc@session-a/record.json",
			StdoutPath: "/state/aira/confine/CONFINE-gate-99-abc@session-a/stdout",
			StderrPath: "/state/aira/confine/CONFINE-gate-99-abc@session-a/stderr",
			Acknowledge: func(delivered bool) error {
				if !delivered {
					t.Error("the handle was printed but the launcher reported it undelivered")
				}
				acknowledged++
				return nil
			},
		}, nil
	}
	var stdout, stderr bytes.Buffer
	exit := runWithInput([]string{"confine", "--detach", "--name", "gate", "--", "make", "merge-gate"}, &stdout, &stderr, strings.NewReader(""))
	if exit != 0 {
		t.Fatalf("exit=%d stderr=%q", exit, stderr.String())
	}
	text := stdout.String()
	for _, want := range []string{
		"CONFINE-gate-99-abc@session-a",
		"supervisor pid 99",
		"aira.slice",
		"exit code is NOT known yet",
		"not that the job succeeded",
		"aira confine --status CONFINE-gate-99-abc@session-a",
		"/stdout",
		"/stderr",
		"/record.json",
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("detach output omits %q:\n%s", want, text)
		}
	}
	// The launcher must never print an exit code for a job that has not run.
	if strings.Contains(text, "exit=") {
		t.Fatalf("the detach launcher printed an exit code for a job that has not run:\n%s", text)
	}
	// The detached job's stdio belongs to its capture files, never to the
	// launching terminal, which is about to go away.
	if seen.Stdin != nil || seen.Stdout != nil || seen.Stderr != nil {
		t.Fatalf("the detached request carried the launching session's stdio: %+v", seen)
	}
	if seen.DetachStateDir == "" {
		t.Fatal("the detached request carried no durable record directory")
	}
	if !strings.Contains(seen.DetachStateDir, "confine") {
		t.Fatalf("record directory %q is not the confine record store", seen.DetachStateDir)
	}
	if acknowledged != 1 {
		t.Fatalf("the handle was acknowledged %d times, want exactly 1 — the supervisor abandons an unacknowledged launch", acknowledged)
	}
}

// A launcher that cannot deliver the handle must NOT report success: the
// supervisor abandons an unacknowledged launch, so an exit 0 here would be a
// fabricated success for a job that never ran.
//
// verifies: AIRA-22
func TestConfineDetachReportsFailureWhenTheHandleCannotBeDelivered(t *testing.T) {
	original := launchConfineDetached
	t.Cleanup(func() { launchConfineDetached = original })
	var deliveredArg *bool
	launchConfineDetached = func(context.Context, runner.ConfineRequest) (*runner.ConfineDetachLaunch, error) {
		return &runner.ConfineDetachLaunch{
			ScopeID: "CONFINE-gate-99-abc@session-a", Slice: "aira.slice", SupervisorPID: 99,
			Acknowledge: func(delivered bool) error {
				value := delivered
				deliveredArg = &value
				return nil
			},
		}, nil
	}
	var stderr bytes.Buffer
	exit := runWithInput([]string{"confine", "--detach", "--", "true"}, failingWriter{}, &stderr, strings.NewReader(""))
	if exit == 0 {
		t.Fatal("an undeliverable handle was reported as a successful detach")
	}
	if deliveredArg == nil || *deliveredArg {
		t.Fatalf("the supervisor was told delivered=%v; it must be told the handle did NOT arrive", deliveredArg)
	}
	if !strings.Contains(stderr.String(), runner.CodeConfineDetachFailed) {
		t.Fatalf("stderr=%q", stderr.String())
	}
}

// A launch object with no acknowledgement channel cannot be confirmed, so it
// must be treated as a failure rather than as "nothing to acknowledge".
//
// verifies: AIRA-22
func TestConfineDetachRefusesALaunchWithNoAcknowledgementChannel(t *testing.T) {
	original := launchConfineDetached
	t.Cleanup(func() { launchConfineDetached = original })
	launchConfineDetached = func(context.Context, runner.ConfineRequest) (*runner.ConfineDetachLaunch, error) {
		return &runner.ConfineDetachLaunch{ScopeID: "CONFINE-gate-99-abc@session-a"}, nil
	}
	var stdout, stderr bytes.Buffer
	if exit := runWithInput([]string{"confine", "--detach", "--", "true"}, &stdout, &stderr, strings.NewReader("")); exit == 0 {
		t.Fatalf("an unconfirmable launch reported success: stdout=%q", stdout.String())
	}
	if !strings.Contains(stderr.String(), "no acknowledgement channel") {
		t.Fatalf("stderr=%q", stderr.String())
	}
}

// A launch failure must surface the supervisor's OWN code, so `--detach` keeps
// the foreground exit contract for every synchronous precondition.
//
// verifies: AIRA-22
func TestConfineDetachLaunchFailurePropagatesTheUnderlyingCode(t *testing.T) {
	original := launchConfineDetached
	t.Cleanup(func() { launchConfineDetached = original })
	launchConfineDetached = func(context.Context, runner.ConfineRequest) (*runner.ConfineDetachLaunch, error) {
		return nil, errors.New("E_CONFINE_UNAVAILABLE: slice aira.slice: uncapped")
	}
	var stdout, stderr bytes.Buffer
	exit := runWithInput([]string{"confine", "--detach", "--", "true"}, &stdout, &stderr, strings.NewReader(""))
	if exit != 4 {
		t.Fatalf("exit=%d, want 4 (E_CONFINE_UNAVAILABLE) — a detached launch must not soften a synchronous failure", exit)
	}
	if !strings.Contains(stderr.String(), "E_CONFINE_UNAVAILABLE") {
		t.Fatalf("stderr=%q", stderr.String())
	}
	if stdout.Len() != 0 {
		t.Fatalf("a failed launch printed a handle: %q", stdout.String())
	}
}

// verifies: AIRA-22
func TestConfineDetachRejectsJSON(t *testing.T) {
	var stdout, stderr bytes.Buffer
	if exit := runWithInput([]string{"confine", "--json", "--detach", "--", "true"}, &stdout, &stderr, strings.NewReader("")); exit != 2 {
		t.Fatalf("exit=%d, want 2", exit)
	}
	if !strings.Contains(stdout.String()+stderr.String(), "--json is not valid for confine") {
		t.Fatalf("stdout=%q stderr=%q", stdout.String(), stderr.String())
	}
}

func detachStatus(state runner.ConfineDetachState, exit *int) runner.ConfineDetachStatus {
	return runner.ConfineDetachStatus{
		State: state,
		Record: runner.ConfineDetachRecord{
			Schema: runner.ConfineDetachSchema, ScopeID: "CONFINE-gate-99-abc@session-a",
			Name: "gate", Owner: "session-a", Terminal: state == runner.ConfineDetachFinished, Exit: exit,
			StdoutPath: "/s/stdout", StderrPath: "/s/stderr",
		},
	}
}

// TestConfineStatusExitCodeReportsTheQueryNotTheJob pins the deliberate split:
// two meanings on one channel is exactly the dishonesty this project's error
// contract forbids, so a finished job that exited 7 still leaves `--status`
// exiting 0, with the 7 printed.
//
// verifies: AIRA-22
func TestConfineStatusExitCodeReportsTheQueryNotTheJob(t *testing.T) {
	original := confineDetachStatusFor
	t.Cleanup(func() { confineDetachStatusFor = original })
	seven := 7
	for _, test := range []struct {
		name     string
		status   runner.ConfineDetachStatus
		err      error
		wantExit int
		wantOut  string
	}{
		{name: "finished non-zero job still exits 0", status: detachStatus(runner.ConfineDetachFinished, &seven), wantExit: 0, wantOut: "exit=7"},
		{name: "running exits 0", status: detachStatus(runner.ConfineDetachRunning, nil), wantExit: 0, wantOut: "state=running"},
		{name: "admitting exits 0", status: detachStatus(runner.ConfineDetachAdmitting, nil), wantExit: 0, wantOut: "state=admitting"},
		{name: "outcome-unknown exits 3", status: detachStatus(runner.ConfineDetachOutcomeUnknown, nil), wantExit: 3, wantOut: "state=outcome-unknown"},
		{name: "not found exits 2", err: errors.New("E_CONFINE_NOT_FOUND: selector \"gate\" matched no detached confine record"), wantExit: 2},
		{name: "ambiguous exits 2, the selector-error code", err: errors.New("E_SELECTOR_AMBIGUOUS: selector \"gate\" matched 2 detached confine records"), wantExit: 2},
	} {
		t.Run(test.name, func(t *testing.T) {
			confineDetachStatusFor = func(_, _, _ string) (runner.ConfineDetachStatus, error) {
				return test.status, test.err
			}
			var stdout, stderr bytes.Buffer
			exit := runWithInput([]string{"confine", "--status", "gate"}, &stdout, &stderr, strings.NewReader(""))
			if exit != test.wantExit {
				t.Fatalf("exit=%d want %d (stdout=%q stderr=%q)", exit, test.wantExit, stdout.String(), stderr.String())
			}
			if test.wantOut != "" && !strings.Contains(stdout.String(), test.wantOut) {
				t.Fatalf("stdout %q lacks %q", stdout.String(), test.wantOut)
			}
		})
	}
}

// verifies: AIRA-22
func TestConfineStatusJSONCarriesTheStateAndExit(t *testing.T) {
	original := confineDetachStatusFor
	t.Cleanup(func() { confineDetachStatusFor = original })
	seven := 7
	confineDetachStatusFor = func(_, _, _ string) (runner.ConfineDetachStatus, error) {
		return detachStatus(runner.ConfineDetachFinished, &seven), nil
	}
	var stdout, stderr bytes.Buffer
	if exit := runWithInput([]string{"confine", "--status", "gate", "--json"}, &stdout, &stderr, strings.NewReader("")); exit != 0 {
		t.Fatalf("exit=%d stderr=%q", exit, stderr.String())
	}
	var decoded runner.ConfineDetachStatus
	if err := json.Unmarshal(stdout.Bytes(), &decoded); err != nil {
		t.Fatalf("status --json is not valid JSON: %v (%q)", err, stdout.String())
	}
	if decoded.State != runner.ConfineDetachFinished || decoded.Record.Exit == nil || *decoded.Record.Exit != 7 {
		t.Fatalf("decoded=%+v", decoded)
	}
}

// verifies: AIRA-22
func TestConfineStatusWithNoSelectorListsTheCallersOwnJobs(t *testing.T) {
	original := confineDetachStatusList
	t.Cleanup(func() { confineDetachStatusList = original })
	var seenOwner string
	confineDetachStatusList = func(_, owner string) ([]runner.ConfineDetachStatus, error) {
		seenOwner = owner
		return []runner.ConfineDetachStatus{detachStatus(runner.ConfineDetachRunning, nil)}, nil
	}
	t.Setenv("AIRA_CONFINE_OWNER", "session-a")
	var stdout, stderr bytes.Buffer
	if exit := runWithInput([]string{"confine", "--status"}, &stdout, &stderr, strings.NewReader("")); exit != 0 {
		t.Fatalf("exit=%d stderr=%q", exit, stderr.String())
	}
	if seenOwner != "session-a" {
		t.Fatalf("listing was not owner-scoped: %q", seenOwner)
	}
	if !strings.Contains(stdout.String(), "CONFINE-gate-99-abc@session-a") {
		t.Fatalf("stdout=%q", stdout.String())
	}
	confineDetachStatusList = func(string, string) ([]runner.ConfineDetachStatus, error) {
		return nil, nil
	}
	stdout.Reset()
	if exit := runWithInput([]string{"confine", "--status"}, &stdout, io.Discard, strings.NewReader("")); exit != 0 {
		t.Fatalf("an empty listing must succeed, got exit=%d", exit)
	}
	if !strings.Contains(stdout.String(), "no detached confine records") {
		t.Fatalf("stdout=%q", stdout.String())
	}
}

// The hidden supervisor verb must reject a malformed invocation rather than
// running with defaults; nothing downstream would notice a supervisor that
// silently supervised the wrong thing.
//
// verifies: AIRA-22
func TestConfineSuperviseVerbRejectsMalformedArguments(t *testing.T) {
	original := superviseConfineDetached
	t.Cleanup(func() { superviseConfineDetached = original })
	called := false
	superviseConfineDetached = func(context.Context, string, int, int) error {
		called = true
		return nil
	}
	for _, argv := range [][]string{
		{"__confine-supervise"},
		{"__confine-supervise", "--control", "/c"},
		{"__confine-supervise", "--control", "/c", "--ready-fd", "x", "--ack-fd", "4"},
		{"__confine-supervise", "--control", "/c", "--ready-fd", "3", "--ack-fd", "-1"},
		{"__confine-supervise", "--nope", "1", "--control", "/c", "--ready-fd", "3", "--ack-fd", "4"},
		{"__confine-supervise", "--control", "/c", "--control", "/d", "--ready-fd", "3", "--ack-fd", "4"},
	} {
		if exit := runWithInput(argv, io.Discard, io.Discard, strings.NewReader("")); exit != 2 {
			t.Fatalf("argv %v exit=%d, want 2", argv, exit)
		}
		if called {
			t.Fatalf("argv %v reached the supervisor", argv)
		}
	}
	if exit := runWithInput([]string{"__confine-supervise", "--control", "/c", "--ready-fd", "3", "--ack-fd", "4"},
		io.Discard, io.Discard, strings.NewReader("")); exit != 0 || !called {
		t.Fatalf("a well-formed invocation did not reach the supervisor (exit=%d called=%v)", exit, called)
	}
}
