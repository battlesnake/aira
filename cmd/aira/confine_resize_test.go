package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"aira/internal/codes"
	"aira/internal/core"
	"aira/internal/daemon"
	"aira/internal/runner"
)

// AIRA-312. `aira confine --resize <selector> --memory-reserve SZ [--steal]`,
// its hyphenated `confine-resize` spelling, the aira_confine_resize MCP tool and
// `aira top`'s `e` key are thin faces over the daemon's confine-resize verb.
//
// verifies: AIRA-312

// verifies: AIRA-312 test 6 — `--resize` parses like --kill/--front plus the
// required --memory-reserve (the launch form's own flag and size grammar), is
// mutually exclusive with every other management flag, accepts --steal, and is
// refused without a size, with a bad size, with a command, or when
// --memory-reserve appears without it.
func TestParseConfineResize(t *testing.T) {
	_, options, err := parseArgs("confine", []string{"--resize", "CONFINE-job-1-a", "--memory-reserve", "2G", "--steal", "--slice", "aira.slice"})
	if err != nil || options["resize"] != "CONFINE-job-1-a" || options["memory-reserve"] != "2G" || options["steal"] != "true" {
		t.Fatalf("options=%v err=%v", options, err)
	}
	if _, options, err = parseArgs("confine", []string{"--resize=job", "--memory-reserve=512M"}); err != nil || options["resize"] != "job" || options["memory-reserve"] != "512M" {
		t.Fatalf("inline form options=%v err=%v", options, err)
	}
	for _, test := range []struct {
		argv []string
		want string
	}{
		{argv: []string{"--resize", "job"}, want: "requires --memory-reserve"},
		{argv: []string{"--resize", "job", "--memory-reserve", "0"}, want: "at least 1MiB"},
		{argv: []string{"--resize", "job", "--memory-reserve", "lots"}, want: "--memory-reserve"},
		{argv: []string{"--resize", "job", "--memory-reserve"}, want: "requires a value"},
		{argv: []string{"--resize"}, want: "requires a value"},
		{argv: []string{"--list", "--memory-reserve", "2G"}, want: "valid only with --resize"},
		{argv: []string{"--resize", "job", "--memory-reserve", "2G", "--kill", "job"}, want: "exactly one of"},
		{argv: []string{"--resize", "job", "--memory-reserve", "2G", "--front", "job"}, want: "exactly one of"},
		{argv: []string{"--resize", "job", "--memory-reserve", "2G", "--list"}, want: "exactly one of"},
		{argv: []string{"--resize", "job", "--memory-reserve", "2G", "--", "make", "test"}, want: "--resize"},
		{argv: []string{"--list", "--steal"}, want: "--steal is valid only with --kill, --front or --resize"},
	} {
		_, _, err := parseArgs("confine", test.argv)
		if err == nil || !strings.HasPrefix(err.Error(), "E_CONFINE_ARGUMENT_INVALID:") || !strings.Contains(err.Error(), test.want) {
			t.Fatalf("argv=%v err=%v, want E_CONFINE_ARGUMENT_INVALID containing %q", test.argv, err, test.want)
		}
	}
	// A management invocation is never a launch, so a refused one prints no
	// `ran=no` launch line.
	if isConfineLaunch([]string{"confine", "--resize", "job", "--memory-reserve", "2G"}) {
		t.Fatal("confine --resize was classified as a launch")
	}
}

// verifies: AIRA-312 test 6 — both spellings reach the daemon as confine-resize,
// project-less, with the selector, the new reserve exactly as typed, the steal
// flag and the caller's owner; and the hyphenated spelling refuses a missing or
// malformed size before any daemon round trip.
func TestConfineResizeDispatchesBothSpellings(t *testing.T) {
	t.Chdir(t.TempDir())
	t.Setenv("AIRA_CONFINE_OWNER", "session-a")
	for _, argv := range [][]string{
		{"confine", "--resize", "job", "--memory-reserve", "2G", "--steal"},
		{"confine-resize", "job", "--memory-reserve", "2G", "--steal"},
	} {
		called := false
		dispatch := dispatcherFunc(func(_ context.Context, scope daemon.WorktreeScope, request core.Request) core.Response {
			called = true
			if scope.Root != "" || request.Verb != "confine-resize" || request.Args["owner"] != "session-a" || request.Args["selector"] != "job" ||
				request.Args["memory_reserve"] != "2G" || request.Args["steal"] != true {
				t.Fatalf("argv=%v scope=%+v request=%+v", argv, scope, request)
			}
			return core.Response{OK: true, Code: "OK", Data: runner.ConfineResizeResult{Status: "resized", ScopeID: "CONFINE-job-1-a@session-a", Name: "job", Owner: "session-a", OldReserveBytes: 8 << 30, NewReserveBytes: 2 << 30}}
		})
		var stdout, stderr bytes.Buffer
		if exit := runWithInputDispatcher(argv, &stdout, &stderr, strings.NewReader(""), dispatch); exit != 0 || !called {
			t.Fatalf("argv=%v exit=%d called=%v stdout=%q stderr=%q", argv, exit, called, stdout.String(), stderr.String())
		}
		if !strings.Contains(stdout.String(), "CONFINE-job-1-a@session-a") {
			t.Fatalf("argv=%v stdout=%q does not name the resized job", argv, stdout.String())
		}
	}
	for _, test := range []struct {
		argv []string
		want string
	}{
		{argv: []string{"confine-resize", "job"}, want: "confine-resize requires --memory-reserve SZ"},
		{argv: []string{"confine-resize", "job", "--memory-reserve", "0"}, want: "at least 1MiB"},
		{argv: []string{"confine-resize", "--memory-reserve", "2G"}, want: "requires one selector"},
	} {
		dispatch := dispatcherFunc(func(context.Context, daemon.WorktreeScope, core.Request) core.Response {
			t.Fatalf("argv=%v reached the daemon", test.argv)
			return core.Response{}
		})
		var stdout, stderr bytes.Buffer
		exit := runWithInputDispatcher(test.argv, &stdout, &stderr, strings.NewReader(""), dispatch)
		output := stdout.String() + stderr.String()
		if exit != codes.ExitForCode("E_CONFINE_ARGUMENT_INVALID") || !strings.Contains(output, "E_CONFINE_ARGUMENT_INVALID") || !strings.Contains(output, test.want) {
			t.Fatalf("argv=%v exit=%d stdout=%q stderr=%q, want an argument refusal saying %q", test.argv, exit, stdout.String(), stderr.String(), test.want)
		}
	}
}

// verifies: AIRA-312 test 6 — the JSON shape of a resize reply, and that a
// daemon refusal keeps its stable code and exit.
func TestConfineResizeJSONShapeAndStableCodes(t *testing.T) {
	t.Chdir(t.TempDir())
	t.Setenv("AIRA_CONFINE_OWNER", "session-a")
	dispatch := dispatcherFunc(func(context.Context, daemon.WorktreeScope, core.Request) core.Response {
		return core.Response{OK: true, Code: "OK", Data: runner.ConfineResizeResult{Status: "resized", ScopeID: "CONFINE-job-1-a@session-a", Name: "job", Owner: "session-a", OldReserveBytes: 8 << 30, NewReserveBytes: 2 << 30}}
	})
	var stdout, stderr bytes.Buffer
	if exit := runWithInputDispatcher([]string{"confine", "--resize", "job", "--memory-reserve", "2G", "--json"}, &stdout, &stderr, strings.NewReader(""), dispatch); exit != 0 {
		t.Fatalf("exit=%d stdout=%q stderr=%q", exit, stdout.String(), stderr.String())
	}
	var envelope struct {
		OK   bool                   `json:"ok"`
		Code string                 `json:"code"`
		Data map[string]interface{} `json:"data"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &envelope); err != nil {
		t.Fatalf("stdout %q is not one JSON reply: %v", stdout.String(), err)
	}
	keys := make([]string, 0, len(envelope.Data))
	for key := range envelope.Data {
		keys = append(keys, key)
	}
	want := []string{"name", "new_reserve_bytes", "old_reserve_bytes", "owner", "scope_id", "status"}
	if !envelope.OK || envelope.Code != "OK" || !reflect.DeepEqual(sortedStrings(keys), want) || envelope.Data["new_reserve_bytes"] != float64(2<<30) {
		t.Fatalf("envelope=%+v keys=%v, want %v", envelope, sortedStrings(keys), want)
	}

	for _, code := range []string{runner.CodeConfineNotRunning, runner.CodeConfineResizeNotReduction, runner.CodeConfineResizeBelowUsage, runner.CodeConfineResizeUsageUnevaluated, runner.CodeConfineOwnerUnverified} {
		refusal := dispatcherFunc(func(context.Context, daemon.WorktreeScope, core.Request) core.Response {
			return core.Response{Code: code, Error: code + ": daemon words", Exit: codes.ExitForCode(code)}
		})
		stdout.Reset()
		stderr.Reset()
		exit := runWithInputDispatcher([]string{"confine", "--resize", "job", "--memory-reserve", "2G"}, &stdout, &stderr, strings.NewReader(""), refusal)
		if exit != codes.ExitForCode(code) || exit == 0 || !strings.Contains(stdout.String()+stderr.String(), code) {
			t.Fatalf("%s: exit=%d (want %d) stdout=%q stderr=%q", code, exit, codes.ExitForCode(code), stdout.String(), stderr.String())
		}
	}
	// The catalogue decision: state refusal 1, request refusals 2, unevaluated 3.
	for code, exit := range map[string]int{
		runner.CodeConfineNotRunning: 1, runner.CodeConfineResizeNotReduction: 2,
		runner.CodeConfineResizeBelowUsage: 2, runner.CodeConfineResizeUsageUnevaluated: 3,
	} {
		if got := codes.ExitForCode(code); got != exit {
			t.Fatalf("%s exits %d, want %d", code, got, exit)
		}
	}
}

func sortedStrings(values []string) []string {
	out := append([]string(nil), values...)
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && out[j] < out[j-1]; j-- {
			out[j], out[j-1] = out[j-1], out[j]
		}
	}
	return out
}

// verifies: AIRA-312 — with the daemon unreachable a resize is REFUSED with the
// transport error and never reaches the management arm's KillConfine fallback
// or the slice resolver: the ledger it edits exists only inside the daemon.
func TestConfineResizeDaemonDownIsRefusedAndNeverKills(t *testing.T) {
	d := &daemonDispatcher{}
	d.exchange = func(context.Context, string, daemon.RequestFrame) (daemon.ResponseFrame, error) {
		return daemon.ResponseFrame{}, &daemon.RequestNotSentError{Err: errors.New(daemon.CodeUnavailable + ": down")}
	}
	d.resolveConfineSlice = func(string) (string, string, error) {
		t.Error("confine-resize resolved the slice with the daemon down: the refusal must come first")
		return "aira.slice", filepath.Join("/unused", "slice"), nil
	}
	d.killConfine = func(context.Context, string, string, string, bool, []runner.ConfineRegistryEntry) (runner.ConfineKillResult, error) {
		t.Fatal("confine-resize reached KillConfine with the daemon down: a resize became a kill")
		return runner.ConfineKillResult{}, nil
	}
	response := d.dispatchConfineManagementOnce(context.Background(),
		core.Request{Verb: "confine-resize", Args: map[string]any{"slice": "aira.slice", "owner": "session-a", "selector": "job", "memory_reserve": "2G", "steal": true}})
	if response.OK || response.Code != daemon.CodeUnavailable || response.Exit == 0 {
		t.Fatalf("daemon-down resize response=%+v, want the %s transport refusal", response, daemon.CodeUnavailable)
	}
}

// verifies: AIRA-312 — an OLD daemon answers confine-resize with an unrelated
// code; the reply keeps the daemon's code and words and gains the likely cause,
// naming --resize. Every code the verb itself produces — the U_ unevaluated one
// included — passes through untouched.
func TestConfineResizeOldDaemonReplyNamesTheLikelyCause(t *testing.T) {
	for _, test := range []struct {
		code string
		hint bool
	}{
		{code: "E_NOT_PROJECT", hint: true},
		{code: "E_UNKNOWN_VERB", hint: true},
		{code: runner.CodeConfineNotRunning},
		{code: runner.CodeConfineResizeNotReduction},
		{code: runner.CodeConfineResizeBelowUsage},
		{code: runner.CodeConfineResizeUsageUnevaluated},
		{code: runner.CodeConfineOwnerUnverified},
		{code: "E_SELECTOR_AMBIGUOUS"},
	} {
		t.Run(test.code, func(t *testing.T) {
			d := &daemonDispatcher{}
			d.exchange = func(context.Context, string, daemon.RequestFrame) (daemon.ResponseFrame, error) {
				return daemon.ResponseFrame{Code: test.code, Error: test.code + ": daemon words", Exit: codes.ExitForCode(test.code)}, nil
			}
			response := d.dispatchConfineManagementOnce(context.Background(),
				core.Request{Verb: "confine-resize", Args: map[string]any{"slice": "aira.slice", "owner": "session-a", "selector": "job", "memory_reserve": "2G"}})
			if response.OK || response.Code != test.code || !strings.HasPrefix(response.Error, test.code+": daemon words") {
				t.Fatalf("response=%+v, want the daemon's own code and words kept", response)
			}
			if got := strings.Contains(response.Error, "may predate `aira confine --resize`"); got != test.hint {
				t.Fatalf("error=%q hint=%v, want hint=%v", response.Error, got, test.hint)
			}
		})
	}
}

// verifies: AIRA-312 test 6 — the MCP tool is generated from the verb table: it
// declares the selector and the new reserve as required, steal as a boolean,
// and a call reaches the dispatcher project-less with the caller's owner.
func TestMCPConfineResizeToolIsGeneratedAndProjectLess(t *testing.T) {
	server := newMCPServer(nil)
	binding, ok := server.byName["aira_confine_resize"]
	if !ok {
		t.Fatal("aira_confine_resize tool missing")
	}
	if binding.tool.Annotations.ReadOnlyHint || binding.tool.Annotations.DestructiveHint {
		t.Fatalf("resize annotations=%+v, want a non-read-only, non-destructive tool", binding.tool.Annotations)
	}
	schema, ok := binding.tool.InputSchema.(mcpInputSchema)
	if !ok || schema.Properties["steal"].Type != "boolean" || schema.Properties["memory_reserve"].Type != "string" ||
		!reflect.DeepEqual(sortedStrings(schema.Required), []string{"memory_reserve", "selector"}) {
		t.Fatalf("resize schema=%+v", binding.tool.InputSchema)
	}

	t.Chdir(t.TempDir())
	t.Setenv("AIRA_CONFINE_OWNER", "session-m")
	var got core.Request
	var gotScope daemon.WorktreeScope
	dispatcher := dispatcherFunc(func(_ context.Context, scope daemon.WorktreeScope, request core.Request) core.Response {
		got, gotScope = request, scope
		return core.Response{OK: true, Code: "OK", Data: runner.ConfineResizeResult{Status: "resized", ScopeID: "CONFINE-job-1-a@session-m"}}
	})
	message := `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"aira_confine_resize","arguments":{"selector":"job","memory_reserve":"2G","steal":true}}}` + "\n"
	var out, diagnostics bytes.Buffer
	if code := runMCPWithDispatcher(context.Background(), strings.NewReader(message), &out, &diagnostics, dispatcher); code != 0 {
		t.Fatalf("mcp exit=%d out=%q diagnostics=%q", code, out.String(), diagnostics.String())
	}
	if gotScope.Root != "" || got.Verb != "confine-resize" || got.Args["selector"] != "job" || got.Args["memory_reserve"] != "2G" || got.Args["steal"] != true || got.Args["owner"] != "session-m" {
		t.Fatalf("scope=%+v request=%+v, want a project-less confine-resize as session-m", gotScope, got)
	}
}
