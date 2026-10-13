package main

import (
	"bytes"
	"context"
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
