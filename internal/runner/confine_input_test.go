package runner

import (
	"errors"
	"strings"
	"testing"
)

// TestClassifyConfineInputStatusRefusesByNameBeforeItDials is the honesty test
// for AIRA-196's default-off design.
//
// The FIRST clause is the load-bearing one and its ORDER is the point: a job
// that never opted into --stdin-connect is refused as UNAVAILABLE whatever else
// is true of it -- finished, queued, or gone. Any other precedence would tell an
// operator who forgot the flag that their job was "closed" or "not ready",
// sending them to wait for a conduit that will never exist.
//
// The refusal also NAMES the flag, because a default-off feature whose refusal
// does not say how to turn it on is a feature nobody finds.
//
// verifies: AIRA-196
func TestClassifyConfineInputStatusRefusesByNameBeforeItDials(t *testing.T) {
	connected := func(state ConfineDetachState, socket string) ConfineDetachStatus {
		return ConfineDetachStatus{State: state, Record: ConfineDetachRecord{
			ScopeID: "CONFINE-gate-1-a@session-a", StdinConnect: true, InputSocket: socket,
		}}
	}
	for _, test := range []struct {
		name     string
		status   ConfineDetachStatus
		wantCode string
		wantPath string
	}{
		{
			name: "a job that did not opt in is unavailable while running",
			status: ConfineDetachStatus{State: ConfineDetachRunning, Record: ConfineDetachRecord{
				ScopeID: "CONFINE-gate-1-a@session-a",
			}},
			wantCode: "E_RUN_INPUT_UNAVAILABLE",
		},
		{
			// Precedence: opting out outranks being finished. A "closed" answer here
			// would describe a conduit that never existed.
			name: "a finished job that did not opt in is still unavailable, not closed",
			status: ConfineDetachStatus{State: ConfineDetachFinished, Record: ConfineDetachRecord{
				ScopeID: "CONFINE-gate-1-a@session-a",
			}},
			wantCode: "E_RUN_INPUT_UNAVAILABLE",
		},
		{name: "a connected finished job is closed", status: connected(ConfineDetachFinished, "/run/s.sock"), wantCode: "E_RUN_INPUT_CLOSED"},
		{name: "a queued job is not ready, never closed", status: connected(ConfineDetachAdmitting, ""), wantCode: "E_RUN_INPUT_NOT_READY"},
		{name: "a starting job is not ready", status: connected(ConfineDetachStarting, ""), wantCode: "E_RUN_INPUT_NOT_READY"},
		{name: "an outcome-unknown job is not ready, never closed", status: connected(ConfineDetachOutcomeUnknown, "/run/s.sock"), wantCode: "E_RUN_INPUT_NOT_READY"},
		{name: "a running job with no socket recorded yet is not ready", status: connected(ConfineDetachRunning, ""), wantCode: "E_RUN_INPUT_NOT_READY"},
		{name: "a running connected job resolves to its socket", status: connected(ConfineDetachRunning, "/run/s.sock"), wantPath: "/run/s.sock"},
	} {
		t.Run(test.name, func(t *testing.T) {
			path, err := classifyConfineInputStatus(test.status)
			if test.wantCode == "" {
				if err != nil {
					t.Fatalf("unexpected refusal: %v", err)
				}
				if path != test.wantPath {
					t.Fatalf("path=%q, want %q", path, test.wantPath)
				}
				return
			}
			if path != "" {
				t.Fatalf("a refusal still handed back a socket path %q to dial", path)
			}
			var inputErr *RunInputError
			if !errors.As(err, &inputErr) {
				t.Fatalf("err=%v, want a *RunInputError", err)
			}
			if inputErr.Code != test.wantCode {
				t.Fatalf("code=%s, want %s", inputErr.Code, test.wantCode)
			}
			if test.wantCode == "E_RUN_INPUT_UNAVAILABLE" && !strings.Contains(inputErr.Error(), "--stdin-connect") {
				t.Fatalf("the refusal does not name the flag that would have enabled it: %v", inputErr)
			}
		})
	}
}
