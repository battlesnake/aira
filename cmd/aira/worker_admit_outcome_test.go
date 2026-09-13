package main

import (
	"bytes"
	"context"
	"path/filepath"
	"strings"
	"testing"

	"aira/internal/codes"
	"aira/internal/runner"
)

func parseOnlyOutcomeLine(t *testing.T, stdout string) map[string]string {
	t.Helper()
	lines := strings.Split(strings.TrimSpace(stdout), "\n")
	if len(lines) != 1 || lines[0] == "" {
		t.Fatalf("worker-admit must write exactly one stdout line, got %d:\n%s", len(lines), stdout)
	}
	fields, err := runner.ParseWorkerAdmitOutcomeLine(lines[0])
	if err != nil {
		t.Fatalf("parse %q: %v", lines[0], err)
	}
	return fields
}

// TestRunWorkerAdmitCommandAlwaysWritesOneStructuredOutcome covers the failure
// paths that never reach the daemon at all. Before AIRA-42 each of these wrote
// only a prose stderr line, which the aitest supervisor could read only as
// "the relay produced nothing" — and therefore as daemon unavailability, i.e.
// as a reason to run the rest of the suite with no per-worker RAM containment.
//
// verifies: AIRA-42
func TestRunWorkerAdmitCommandAlwaysWritesOneStructuredOutcome(t *testing.T) {
	tests := []struct {
		name       string
		options    map[string]string
		wantState  string
		wantClass  string
		wantReason string
		wantCode   string
	}{
		{
			name:      "estimated bytes below the floor",
			options:   map[string]string{"job-id": "j", "outer-scope": "/outer", "estimated-bytes": "100000"},
			wantState: runner.WorkerAdmitStateArgumentInvalid, wantClass: runner.WorkerAdmitClassRequestInvalid,
			wantReason: runner.WorkerAdmitReasonEstimatedBytesOutOfRange, wantCode: "E_CONFINE_ARGUMENT_INVALID",
		},
		{
			name:      "estimated bytes above the ceiling",
			options:   map[string]string{"job-id": "j", "outer-scope": "/outer", "estimated-bytes": "2000000000000000"},
			wantState: runner.WorkerAdmitStateArgumentInvalid, wantClass: runner.WorkerAdmitClassRequestInvalid,
			wantReason: runner.WorkerAdmitReasonEstimatedBytesOutOfRange, wantCode: "E_CONFINE_ARGUMENT_INVALID",
		},
		{
			name: "unparseable max wait",
			options: map[string]string{
				"job-id": "j", "outer-scope": "/outer", "estimated-bytes": "400M", "max-wait": "not-a-duration",
			},
			wantState: runner.WorkerAdmitStateArgumentInvalid, wantClass: runner.WorkerAdmitClassRequestInvalid,
			wantReason: runner.WorkerAdmitReasonMaxWaitInvalid, wantCode: "E_CONFINE_ARGUMENT_INVALID",
		},
		{
			// AIRA-64 CHANGED THIS CASE DELIBERATELY. It used to assert that
			// "0s" was argument-invalid. Zero now means SPECULATIVE ("answer
			// from what you can obtain without waiting") and only NEGATIVES are
			// refused. The old contract was actively dangerous once the aitest
			// supervisor started issuing speculative pool-growth probes:
			// request-invalid is a WorkerAdmitTerminal, so every probe would
			// have drained the run's remaining queue to `unevaluated`.
			name: "negative max wait",
			options: map[string]string{
				"job-id": "j", "outer-scope": "/outer", "estimated-bytes": "400M", "max-wait": "-1s",
			},
			wantState: runner.WorkerAdmitStateArgumentInvalid, wantClass: runner.WorkerAdmitClassRequestInvalid,
			wantReason: runner.WorkerAdmitReasonMaxWaitInvalid, wantCode: "E_CONFINE_ARGUMENT_INVALID",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			exit := runWorkerAdmitCommand(context.Background(), test.options, strings.NewReader(""), &stdout, &stderr)
			if want := codes.ExitForCode(test.wantCode); exit != want {
				t.Fatalf("exit=%d want %d; stderr=%s", exit, want, stderr.String())
			}
			fields := parseOnlyOutcomeLine(t, stdout.String())
			if fields["state"] != test.wantState || fields["class"] != test.wantClass || fields["reason"] != test.wantReason {
				t.Fatalf("outcome=%v, want state=%s class=%s reason=%s",
					fields, test.wantState, test.wantClass, test.wantReason)
			}
			if strings.TrimSpace(stderr.String()) == "" {
				t.Fatal("a human diagnostic must still reach stderr")
			}
		})
	}
}

// verifies: AIRA-64 §9.20 — `--max-wait 0` is a SPECULATIVE request, and the
// CLI must accept it.
//
// This asserts through the REAL argument parser rather than by mocking the
// caller's arguments, and that distinction is the whole point: the shipping
// defect was that only this layer refused zero (the daemon's own
// validateWorkerAdmitArgs has always accepted it), so any test that stubbed the
// CLI would have passed against the bug. It is refused as
// argument-invalid/request-invalid, which the aitest supervisor classes as
// WorkerAdmitTerminal and responds to by draining its remaining queue to
// `unevaluated` — so every speculative pool-growth probe would have destroyed
// the run it was issued to help (Sol plan-review round 2, P0).
//
// The outcome here is necessarily a non-grant; what is ASSERTED is only that it
// is NOT the max-wait argument rejection and NEVER the TERMINAL request-invalid
// class — i.e. that the CLI argument layer ACCEPTED "0"/"0s" instead of refusing
// it before the dial (the AIRA-64 defect). Coverage note: these assertions are
// negative-only, and — taken after an unreachable dial — are agnostic to what
// happens to max-wait past the arg layer, so they would NOT catch a bug that
// accepted zero and then silently coerced it to a blocking wait. That is a
// deliberate, accepted gap: the speculative wire semantic (max_wait_ms
// present-and-zero) is exercised end-to-end by the MaxWait:0 non-blocking-probe
// cases in internal/runner/worker_admit_client_linux_test.go and pinned in
// internal/daemon/protocol_test.go; this test guards only the arg-layer
// acceptance, which is where the shipped defect actually was.
//
// Isolation: the test points the XDG resolver (daemon.PathsFromEnv composes the
// socket path from XDG_RUNTIME_DIR + a hash of XDG_STATE_HOME) at fresh temp
// dirs, so the socket it dials is canonical-but-absent and the non-grant is a
// dial failure regardless of any live daemon. This is load-bearing, not hygiene:
// without it the test relied on the AMBIENT box having no matching-protocol
// daemon, and the v0.7 S2a proto-12 cutover made one reachable — it then
// processed this deliberately parent_scope_id-less request (runWorkerAdmitCommand
// takes a pre-parsed option map, bypassing the parseWorkerAdmitArgs required-field
// check the CLI's Run path applies) and correctly rejected it request-invalid,
// reddening the test for everyone on the box. Isolation ENFORCES the "no daemon
// reachable" precondition the test documents rather than assuming the environment
// supplies it.
//
// Isolation is also why this is fixed by isolating rather than by COMPLETING the
// request (adding a parent_scope_id): THIS test's request, lacking
// parent_scope_id, is rejected request-invalid before any grant and creates
// nothing — but a COMPLETED worker-admit reaching the live daemon would pass
// validation and be granted its requested reserve verbatim, creating a real
// worker cgroup scope on the shared slice ledger as a unit-test side effect. Any
// test in this file that reaches a daemon dial must isolate the same way.
func TestRunWorkerAdmitCommandAcceptsZeroMaxWaitAsSpeculative(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", filepath.Join(t.TempDir(), "state"))
	t.Setenv("XDG_RUNTIME_DIR", filepath.Join(t.TempDir(), "runtime"))
	for _, raw := range []string{"0s", "0"} {
		t.Run(raw, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			runWorkerAdmitCommand(context.Background(), map[string]string{
				"job-id": "j", "outer-scope": "/outer", "estimated-bytes": "400M", "max-wait": raw,
			}, strings.NewReader(""), &stdout, &stderr)
			fields := parseOnlyOutcomeLine(t, stdout.String())
			if fields["reason"] == runner.WorkerAdmitReasonMaxWaitInvalid {
				t.Fatalf("--max-wait %s must be accepted as speculative, not refused: %v", raw, fields)
			}
			if fields["class"] == runner.WorkerAdmitClassRequestInvalid {
				t.Fatalf("--max-wait %s must never yield the TERMINAL request-invalid class "+
					"(it drains the supervisor's queue to unevaluated): %v", raw, fields)
			}
		})
	}
}

// verifies: AIRA-42 — the pre-dispatch faces (argument parsing and the --json
// refusal) speak the same channel. They run one layer above
// runWorkerAdmitCommand and used to leave the supervisor with no outcome.
func TestWorkerAdmitPreDispatchFailuresSpeakTheOutcomeChannel(t *testing.T) {
	t.Run("unknown option", func(t *testing.T) {
		var stdout, stderr bytes.Buffer
		exit := Run([]string{"worker-admit", "--not-an-option", "x"}, &stdout, &stderr)
		if want := codes.ExitForCode("E_CONFINE_ARGUMENT_INVALID"); exit != want {
			t.Fatalf("exit=%d want %d; stderr=%s", exit, want, stderr.String())
		}
		fields := parseOnlyOutcomeLine(t, stdout.String())
		if fields["class"] != runner.WorkerAdmitClassRequestInvalid ||
			fields["reason"] != runner.WorkerAdmitReasonArgumentsInvalid {
			t.Fatalf("outcome=%v", fields)
		}
	})

	t.Run("missing required option", func(t *testing.T) {
		var stdout, stderr bytes.Buffer
		_ = Run([]string{"worker-admit", "--job-id", "j"}, &stdout, &stderr)
		fields := parseOnlyOutcomeLine(t, stdout.String())
		if fields["class"] != runner.WorkerAdmitClassRequestInvalid {
			t.Fatalf("outcome=%v", fields)
		}
	})

	t.Run("--json is refused on the channel, not as a rendered JSON error", func(t *testing.T) {
		var stdout, stderr bytes.Buffer
		_ = Run([]string{"worker-admit", "--json", "--job-id", "j", "--outer-scope", "/o", "--estimated-bytes", "400M"},
			&stdout, &stderr)
		fields := parseOnlyOutcomeLine(t, stdout.String())
		if fields["class"] != runner.WorkerAdmitClassRequestInvalid {
			t.Fatalf("outcome=%v", fields)
		}
	})
}
