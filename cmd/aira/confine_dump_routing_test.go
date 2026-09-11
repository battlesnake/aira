package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"aira/internal/core"
	"aira/internal/daemon"
	"aira/internal/runner"
)

// TestConfineDumpReachesTheDaemonRatherThanAProjectStore is AIRA-201's own
// routing pin, extended to confine-dump: it is RouteClient like the rest of
// the confine family (internal/core/routing.go), and every confine-management
// caller hands Dispatch an EMPTY daemon.WorktreeScope{} because these verbs
// resolve no project by design. Without the management-arm branch this falls
// through to the client path, which opens a project store with that empty
// scope and refuses E_CONFIG_INVALID on every invocation -- exactly the class
// of routing defect AIRA-201 fixed for confine-budget.
//
// verifies: AIRA (admission-counter rebuild) S18
func TestConfineDumpReachesTheDaemonRatherThanAProjectStore(t *testing.T) {
	var sent []daemon.RequestFrame
	d := &daemonDispatcher{}
	d.exchange = func(_ context.Context, _ string, frame daemon.RequestFrame) (daemon.ResponseFrame, error) {
		sent = append(sent, frame)
		return daemon.ResponseFrame{OK: true, Code: "OK"}, nil
	}
	response := d.Dispatch(context.Background(), daemon.WorktreeScope{},
		core.Request{Verb: "confine-dump", Args: map[string]any{"slice": "aira.slice", "owner": "session-a"}})
	if len(sent) != 1 {
		t.Fatalf("confine-dump was answered without a daemon exchange (response=%+v); the management arm did not claim it", response)
	}
	if sent[0].Request.Verb != "confine-dump" {
		t.Fatalf("frame verb=%q, want confine-dump", sent[0].Request.Verb)
	}
	if !response.OK || response.Code != "OK" {
		t.Fatalf("response=%+v", response)
	}
}

// TestConfineDumpDaemonDownIsUnevaluatedAndNeverKills is the false-pass twin:
// the management arm's daemon-down fallback special-cases confine-list and
// lets everything else fall through to KillConfine. Without an explicit
// branch, confine-dump would turn a read-only archival request into a kill
// attempt carrying an empty selector.
//
// verifies: AIRA (admission-counter rebuild) S18
func TestConfineDumpDaemonDownIsUnevaluatedAndNeverKills(t *testing.T) {
	d := &daemonDispatcher{}
	d.exchange = func(context.Context, string, daemon.RequestFrame) (daemon.ResponseFrame, error) {
		return daemon.ResponseFrame{}, &daemon.RequestNotSentError{Err: errors.New(daemon.CodeUnavailable + ": down")}
	}
	d.resolveConfineSlice = func(string) (string, string, error) {
		return "aira.slice", filepath.Join("/unused", "slice"), nil
	}
	d.killConfine = func(context.Context, string, string, string, bool, []runner.ConfineRegistryEntry) (runner.ConfineKillResult, error) {
		t.Fatal("confine-dump must never reach KillConfine: a read-only report became a kill")
		return runner.ConfineKillResult{}, nil
	}

	response := d.Dispatch(context.Background(), daemon.WorktreeScope{},
		core.Request{Verb: "confine-dump", Args: map[string]any{"slice": "aira.slice", "owner": "session-a"}})

	if response.Code != "UNEVALUATED" || response.Exit != 3 {
		t.Fatalf("daemon-down dump response=%+v, want UNEVALUATED exit 3", response)
	}
	result, ok := response.Data.(runner.ConfineDumpResult)
	if !ok {
		t.Fatalf("data type %T, want runner.ConfineDumpResult", response.Data)
	}
	if result.Verdict != "unevaluated" {
		t.Fatalf("verdict=%q, want unevaluated", result.Verdict)
	}
	if len(result.Admissions) != 0 || len(result.Queues) != 0 {
		t.Fatalf("result=%+v; an unevaluated report must carry no rows, not fabricated ones", result)
	}
}

// TestConfineDumpArgvWritesTheFile drives the REAL CLI entry point (argv ->
// RunWithDispatcher), the same shape TestBothConfineBudgetSpellingsReachTheManagementDispatch
// pins for confine-budget, and closes the gap that test's own comment names:
// a fix verified only at the dispatcher would not notice the CLI argv path
// being broken. It also exercises the one behaviour unique to this verb among
// its confine-management siblings: writing a local file.
//
// verifies: AIRA (admission-counter rebuild) S18
func TestConfineDumpArgvWritesTheFile(t *testing.T) {
	dir := t.TempDir()
	dumpPath := filepath.Join(dir, "dump.jsonl")
	var seen []string
	peak := int64(123 << 20)
	injected := dispatcherFunc(func(_ context.Context, scope daemon.WorktreeScope, request core.Request) core.Response {
		seen = append(seen, request.Verb)
		if scope.ProjectID != "" || scope.Root != "" {
			t.Fatalf("resolved a project scope %+v; confine-dump is project-less", scope)
		}
		return core.Response{OK: true, Code: "OK", Data: runner.ConfineDumpResult{
			Verdict: "ok", Scope: "test-universe",
			Admissions: []runner.ConfineDumpAdmissionRow{{
				RecordType: runner.ConfineDumpRecordAdmission, Kind: "confine", Signature: "make test",
				ObservedPeakBytes: &peak, Outcome: runner.ConfineDumpUnevaluated,
			}},
			Queues: []runner.ConfineDumpQueueRow{{RecordType: runner.ConfineDumpRecordQueue, Slice: "/aira.slice"}},
		}}
	})
	var stdout, stderr bytes.Buffer
	exit := RunWithDispatcher([]string{"confine", "--dump", dumpPath}, &stdout, &stderr, injected)
	if len(seen) != 1 || seen[0] != "confine-dump" {
		t.Fatalf("dispatched %v, want exactly one confine-dump; exit=%d stdout=%q stderr=%q", seen, exit, stdout.String(), stderr.String())
	}
	if exit != 0 {
		t.Fatalf("exit=%d stderr=%q", exit, stderr.String())
	}
	data, err := os.ReadFile(dumpPath)
	if err != nil {
		t.Fatalf("dump file not written: %v", err)
	}
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	if len(lines) != 2 {
		t.Fatalf("want 2 JSONL lines, got %d: %q", len(lines), string(data))
	}
	var row map[string]any
	if err := json.Unmarshal([]byte(lines[0]), &row); err != nil {
		t.Fatalf("line 0 not valid JSON: %v", err)
	}
	if row["signature"] != "make test" {
		t.Fatalf("line 0 = %v", row)
	}
}

// TestConfineDumpUnevaluatedDoesNotWriteAFile pins the honesty discipline at
// the CLI boundary: when the daemon cannot be reached (or otherwise answers
// unevaluated), no file is written at all -- a zero-byte or empty-JSONL dump
// would read as "nothing to record" rather than "could not look".
//
// verifies: AIRA (admission-counter rebuild) S18
func TestConfineDumpUnevaluatedDoesNotWriteAFile(t *testing.T) {
	dir := t.TempDir()
	dumpPath := filepath.Join(dir, "dump.jsonl")
	injected := dispatcherFunc(func(context.Context, daemon.WorktreeScope, core.Request) core.Response {
		return core.Response{OK: true, Code: "UNEVALUATED", Exit: 3, Data: runner.ConfineDumpResult{
			Verdict: "unevaluated", Reason: "the daemon is unreachable",
		}}
	})
	var stdout, stderr bytes.Buffer
	exit := RunWithDispatcher([]string{"confine", "--dump", dumpPath}, &stdout, &stderr, injected)
	if exit != 3 {
		t.Fatalf("exit=%d, want 3; stdout=%q stderr=%q", exit, stdout.String(), stderr.String())
	}
	if !strings.Contains(stdout.String(), "unevaluated") {
		t.Fatalf("stdout must say unevaluated: %q", stdout.String())
	}
	if _, err := os.Stat(dumpPath); err == nil {
		t.Fatal("no file should have been written for an unevaluated dump")
	}
}

// TestParseConfineManagementArgsDump pins --dump's argument-parsing contract:
// it requires a value, joins --list/--kill/--status/--budget in the
// exactly-one-of group, and --slice (unlike --status) is accepted alongside
// it (confine-dump ignores slice server-side, like confine-budget).
//
// verifies: AIRA (admission-counter rebuild) S18
func TestParseConfineManagementArgsDump(t *testing.T) {
	_, options, err := parseConfineManagementArgs([]string{"--dump", "/tmp/x.jsonl"})
	if err != nil {
		t.Fatalf("--dump alone should parse: %v", err)
	}
	if options["dump"] != "/tmp/x.jsonl" {
		t.Fatalf("options=%+v", options)
	}

	if _, _, err := parseConfineManagementArgs([]string{"--dump"}); err == nil {
		t.Fatal("--dump with no value must be refused")
	}

	if _, _, err := parseConfineManagementArgs([]string{"--dump", "/tmp/x.jsonl", "--list"}); err == nil {
		t.Fatal("--dump and --list together must be refused (exactly one of)")
	}

	if _, options, err := parseConfineManagementArgs([]string{"--dump", "/tmp/x.jsonl", "--slice", "aira.slice"}); err != nil || options["slice"] != "aira.slice" {
		t.Fatalf("--dump with --slice should parse (slice accepted, ignored server-side): options=%+v err=%v", options, err)
	}
}
