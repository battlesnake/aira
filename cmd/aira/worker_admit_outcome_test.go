package main

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"aira/internal/runner"
	"aira/internal/store"
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
			if want := store.ExitForCode(test.wantCode); exit != want {
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
// With no daemon reachable the outcome here is necessarily a non-grant; what is
// asserted is that it is NOT the max-wait argument rejection.
func TestRunWorkerAdmitCommandAcceptsZeroMaxWaitAsSpeculative(t *testing.T) {
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
		if want := store.ExitForCode("E_CONFINE_ARGUMENT_INVALID"); exit != want {
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
