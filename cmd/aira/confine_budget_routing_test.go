package main

import (
	"bytes"
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"aira/internal/core"
	"aira/internal/daemon"
	"aira/internal/runner"
)

// TestConfineBudgetReachesTheDaemonRatherThanAProjectStore pins the routing arm
// AIRA-201 was missing.
//
// confine-budget is RouteClient (internal/core/routing.go:52), like the rest of
// the confine family, and every confine-management caller hands Dispatch an
// EMPTY daemon.WorktreeScope{} — because these verbs resolve no project by
// design. daemonDispatcher.Dispatch listed only confine-list and confine-kill in
// its management arm, so confine-budget fell through to the client path, which
// opened a project store with that empty scope and refused with
// `E_CONFIG_INVALID: scope options are incomplete` (internal/store/store.go:580)
// on EVERY invocation, in every directory. Both shipped spellings were dead:
// `aira confine --budget` (cmd/aira/main.go:2135) and the hyphenated
// `aira confine-budget` (main.go:280) converge on the same dispatch, and the
// daemon's own handler at internal/daemon/server.go:785 was unreachable code —
// while internal/core/skill.go instructs agents to run the verb.
//
// The test asserts the frame actually leaves for the daemon. Asserting merely
// that the response is not E_CONFIG_INVALID would pass against a client-side
// answer that happened to succeed, which is the false-pass this pins shut.
//
// verifies: AIRA-201
func TestConfineBudgetReachesTheDaemonRatherThanAProjectStore(t *testing.T) {
	for _, verb := range []string{"confine-budget", "confine-list", "confine-kill"} {
		t.Run(verb, func(t *testing.T) {
			var sent []daemon.RequestFrame
			d := &daemonDispatcher{}
			d.exchange = func(_ context.Context, _ string, frame daemon.RequestFrame) (daemon.ResponseFrame, error) {
				sent = append(sent, frame)
				return daemon.ResponseFrame{OK: true, Code: "OK"}, nil
			}
			args := map[string]any{"slice": "aira.slice", "owner": "session-a"}
			if verb == "confine-kill" {
				args["selector"] = "job"
			}
			response := d.Dispatch(context.Background(), daemon.WorktreeScope{}, core.Request{Verb: verb, Args: args})
			if len(sent) != 1 {
				t.Fatalf("%s was answered without a daemon exchange (response=%+v); the management arm did not claim it", verb, response)
			}
			if sent[0].Request.Verb != verb {
				t.Fatalf("frame verb=%q, want %q", sent[0].Request.Verb, verb)
			}
			if !response.OK || response.Code != "OK" {
				t.Fatalf("%s response=%+v", verb, response)
			}
		})
	}
}

// TestConfineBudgetDaemonDownIsUnevaluatedAndNeverKills is the false-pass twin.
//
// The management arm's daemon-down fallback special-cases confine-list and lets
// EVERYTHING ELSE fall through to KillConfine. Routing confine-budget into that
// arm without an explicit branch would therefore turn a read-only budget report
// into a kill attempt carrying an empty selector, which is a far worse defect
// than the one AIRA-201 fixes.
//
// A budget report is computed from the daemon's own peak-RSS history; with the
// daemon unreachable there is no other source, so the honest answer is
// `unevaluated` with the reason named — never an empty subject list, which reads
// as "nothing is over-provisioned".
//
// verifies: AIRA-201
func TestConfineBudgetDaemonDownIsUnevaluatedAndNeverKills(t *testing.T) {
	d := &daemonDispatcher{}
	d.exchange = func(context.Context, string, daemon.RequestFrame) (daemon.ResponseFrame, error) {
		return daemon.ResponseFrame{}, &daemon.RequestNotSentError{Err: errors.New(daemon.CodeUnavailable + ": down")}
	}
	d.resolveConfineSlice = func(string) (string, string, error) {
		return "aira.slice", filepath.Join("/unused", "slice"), nil
	}
	d.killConfine = func(context.Context, string, string, string, bool, []runner.ConfineRegistryEntry) (runner.ConfineKillResult, error) {
		t.Fatal("confine-budget must never reach KillConfine: a read-only report became a kill")
		return runner.ConfineKillResult{}, nil
	}

	response := d.Dispatch(context.Background(), daemon.WorktreeScope{},
		core.Request{Verb: "confine-budget", Args: map[string]any{"slice": "aira.slice", "owner": "session-a"}})

	if response.Code != "UNEVALUATED" || response.Exit != 3 {
		t.Fatalf("daemon-down budget response=%+v, want UNEVALUATED exit 3", response)
	}
	result, ok := response.Data.(runner.ConfineBudgetResult)
	if !ok {
		t.Fatalf("data type %T, want runner.ConfineBudgetResult", response.Data)
	}
	if result.Verdict != "unevaluated" {
		t.Fatalf("verdict=%q, want unevaluated", result.Verdict)
	}
	if len(result.Subjects) != 0 {
		t.Fatalf("subjects=%+v; an unevaluated report must carry no rows, not fabricated ones", result.Subjects)
	}
}

// TestRenderConfineBudgetUnevaluatedDoesNotClaimAnEmptyHistory is the render-half
// false-pass twin, and it pins a defect the AIRA-201 fix itself introduced.
//
// The human renderer prints "no usage history recorded yet" whenever Subjects is
// empty. The new daemon-down result is ALSO empty, so before this branch existed
// a caller who could not reach the daemon was told the history was empty — "we
// could not look" rendered as "we looked and there is nothing", which is exactly
// the fabrication class this verb exists to avoid, and strictly worse than the
// E_CONFIG_INVALID it replaced.
//
// verifies: AIRA-201
func TestRenderConfineBudgetUnevaluatedDoesNotClaimAnEmptyHistory(t *testing.T) {
	var stdout, stderr bytes.Buffer
	exit := renderConfineBudgetResponse(core.Response{OK: true, Code: "UNEVALUATED", Exit: 3, Data: runner.ConfineBudgetResult{
		Verdict: "unevaluated",
		Reason:  "the daemon is unreachable, and it is the only holder of the peak-RSS history",
	}}, &stdout, &stderr)
	if exit != 3 {
		t.Fatalf("exit=%d, want 3 (unevaluated)", exit)
	}
	out := stdout.String()
	if strings.Contains(out, "no usage history recorded yet") {
		t.Fatalf("an unevaluated report must not claim the history is empty:\n%s", out)
	}
	if !strings.Contains(out, "unevaluated") || !strings.Contains(out, "daemon is unreachable") {
		t.Fatalf("the reason must reach the reader:\n%s", out)
	}
}
