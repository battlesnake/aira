package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"aira/internal/codes"
	"aira/internal/core"
	"aira/internal/daemon"
	"aira/internal/runner"
)

// verifies: AIRA-309 test 15 — `aira confine --front` parses like --kill, is
// mutually exclusive with every other management flag, accepts --steal (which
// is otherwise --kill-only), and requires a selector.
func TestParseConfineFront(t *testing.T) {
	_, options, err := parseArgs("confine", []string{"--front", "CONFINE-job-1-a", "--steal", "--slice", "aira.slice"})
	if err != nil || options["front"] != "CONFINE-job-1-a" || options["steal"] != "true" {
		t.Fatalf("options=%v err=%v", options, err)
	}
	if _, options, err = parseArgs("confine", []string{"--front=job"}); err != nil || options["front"] != "job" {
		t.Fatalf("inline form options=%v err=%v", options, err)
	}
	for _, argv := range [][]string{
		{"--front", "x", "--kill", "y"},
		{"--front", "x", "--list"},
		{"--front", "x", "--budget"},
		{"--front", "x", "--status"},
		{"--front"},
		{"--front="},
		{"--list", "--steal"},
	} {
		if _, _, err := parseArgs("confine", argv); err == nil || !strings.HasPrefix(err.Error(), "E_CONFINE_ARGUMENT_INVALID:") {
			t.Fatalf("argv=%v err=%v, want E_CONFINE_ARGUMENT_INVALID", argv, err)
		}
	}
}

// verifies: AIRA-309 — both spellings (`confine --front <sel>` and
// `confine-front <sel>`) reach the daemon as confine-front with the selector,
// the steal flag and the caller's owner, project-less.
func TestConfineFrontDispatchesBothSpellings(t *testing.T) {
	t.Chdir(t.TempDir())
	t.Setenv("AIRA_CONFINE_OWNER", "session-a")
	for _, argv := range [][]string{{"confine", "--front", "job", "--steal"}, {"confine-front", "job", "--steal"}} {
		called := false
		dispatch := dispatcherFunc(func(_ context.Context, scope daemon.WorktreeScope, request core.Request) core.Response {
			called = true
			if scope.Root != "" || request.Verb != "confine-front" || request.Args["owner"] != "session-a" || request.Args["selector"] != "job" || request.Args["steal"] != true {
				t.Fatalf("argv=%v scope=%+v request=%+v", argv, scope, request)
			}
			return core.Response{OK: true, Code: "OK", Data: runner.ConfineFrontResult{Status: "pushed", ScopeID: "CONFINE-job-1-a@session-a", Name: "job", Owner: "session-a", Position: 1}}
		})
		var stdout, stderr bytes.Buffer
		if exit := runWithInputDispatcher(argv, &stdout, &stderr, strings.NewReader(""), dispatch); exit != 0 || !called {
			t.Fatalf("argv=%v exit=%d called=%v stdout=%q stderr=%q", argv, exit, called, stdout.String(), stderr.String())
		}
		if !strings.Contains(stdout.String(), "CONFINE-job-1-a@session-a") {
			t.Fatalf("argv=%v stdout=%q does not name the pushed job", argv, stdout.String())
		}
	}
}

// verifies: AIRA-309 test 15 — an OLD daemon has no confine-front route and
// answers with an error code (never a silent success). The client renders that
// reply as a failure with a non-zero exit and the daemon's words.
func TestConfineFrontOldDaemonRefusalRendersAsAnError(t *testing.T) {
	t.Chdir(t.TempDir())
	t.Setenv("AIRA_CONFINE_OWNER", "session-a")
	for _, code := range []string{"E_UNKNOWN_VERB", "E_CONFIG_INVALID", "E_PROTOCOL"} {
		dispatch := dispatcherFunc(func(context.Context, daemon.WorktreeScope, core.Request) core.Response {
			return core.Response{Code: code, Error: code + ": old daemon", Exit: codes.ExitForCode(code)}
		})
		var stdout, stderr bytes.Buffer
		exit := runWithInputDispatcher([]string{"confine", "--front", "job"}, &stdout, &stderr, strings.NewReader(""), dispatch)
		if exit == 0 {
			t.Fatalf("%s: exit=0, an old daemon's refusal rendered as success (stdout=%q)", code, stdout.String())
		}
		if !strings.Contains(stdout.String()+stderr.String(), code) {
			t.Fatalf("%s: the refusal code is not shown: stdout=%q stderr=%q", code, stdout.String(), stderr.String())
		}
	}
}

// verifies: AIRA-309 — with the daemon unreachable, a push is REFUSED and never
// reaches the management arm's KillConfine fallback. Before the explicit branch,
// every non-list management verb fell through to a kill, so "push this job to
// the front" would have killed it whenever the daemon was down. Also pinned for
// an unknown future management verb (the default-deny before the kill).
func TestConfineFrontDaemonDownIsRefusedAndNeverKills(t *testing.T) {
	for _, verb := range []string{"confine-front", "confine-someday"} {
		t.Run(verb, func(t *testing.T) {
			d := &daemonDispatcher{}
			d.exchange = func(context.Context, string, daemon.RequestFrame) (daemon.ResponseFrame, error) {
				return daemon.ResponseFrame{}, &daemon.RequestNotSentError{Err: errors.New(daemon.CodeUnavailable + ": down")}
			}
			d.resolveConfineSlice = func(string) (string, string, error) {
				return "aira.slice", filepath.Join("/unused", "slice"), nil
			}
			d.killConfine = func(context.Context, string, string, string, bool, []runner.ConfineRegistryEntry) (runner.ConfineKillResult, error) {
				t.Fatalf("%s reached KillConfine with the daemon down: a push became a kill", verb)
				return runner.ConfineKillResult{}, nil
			}
			response := d.dispatchConfineManagementOnce(context.Background(),
				core.Request{Verb: verb, Args: map[string]any{"slice": "aira.slice", "owner": "session-a", "selector": "job", "steal": true}})
			if response.OK || response.Code == "" || response.Code == "OK" || response.Exit == 0 {
				t.Fatalf("daemon-down %s response=%+v, want a refusal", verb, response)
			}
		})
	}
}

// verifies: AIRA-309 — the `aira top` palette path delivers the push. The
// palette is routed-only and refuses client-routed verbs (Sol P0), and
// confine-front is client-routed, so `f` reached the confirmation and then
// failed with "not daemon-routed" until DispatchPalette admitted this one verb
// through the management exchange. Driven from the real confirmation the `f`
// key builds, through executePaletteRequest, the executor's own call.
func TestTopFrontPushReachesTheDaemonThroughThePalette(t *testing.T) {
	state, queued := topFrontState(t, false)
	state = onTopHighlight(state, 3)
	state, _ = onTUIKey(state, 'f', core.New(nil).DispatchDescriptors())
	if state.PaletteConfirm == nil {
		t.Fatal("no confirmation")
	}
	request := state.PaletteConfirm.Request

	var sent []daemon.RequestFrame
	reply := daemon.ResponseFrame{OK: true, Code: "OK"}
	d := &daemonDispatcher{}
	d.exchange = func(_ context.Context, _ string, frame daemon.RequestFrame) (daemon.ResponseFrame, error) {
		sent = append(sent, frame)
		return reply, nil
	}
	data, _ := json.Marshal(runner.ConfineFrontResult{Status: "pushed", ScopeID: queued.ScopeID, Position: 1})
	reply.Data = data
	result := executePaletteRequest(context.Background(), d, daemon.WorktreeScope{}, request)
	if len(sent) != 1 {
		t.Fatalf("push sent %d frames (result %+v), want exactly one exchange", len(sent), result)
	}
	args := sent[0].Request.Args
	if sent[0].Request.Verb != "confine-front" || args["selector"] != queued.ScopeID || args["steal"] != true || sent[0].Scope.Root != "" {
		t.Fatalf("frame=%+v, want a project-less confine-front of %s with steal", sent[0], queued.ScopeID)
	}
	if result.Outcome != paletteApplied {
		t.Fatalf("outcome=%v text=%q, want applied", result.Outcome, result.Text)
	}

	// A daemon refusal (the job was admitted meanwhile) is shown as a rejection
	// with its code, not as an unknown outcome.
	reply = daemon.ResponseFrame{OK: false, Code: runner.CodeConfineNotQueued, Error: runner.CodeConfineNotQueued + ": already admitted"}
	result = executePaletteRequest(context.Background(), d, daemon.WorktreeScope{}, request)
	if result.Outcome != paletteRejected || !strings.Contains(result.Text, runner.CodeConfineNotQueued) {
		t.Fatalf("refusal outcome=%v text=%q, want rejected with %s", result.Outcome, result.Text, runner.CodeConfineNotQueued)
	}
}
