package runner

import (
	"strings"
	"testing"
)

// verifies: AIRA-42 — the outcome line is a structured record, and free text
// inside it can never be read as a field.
func TestWorkerAdmitOutcomeLineRoundTrips(t *testing.T) {
	tests := []struct {
		name    string
		outcome WorkerAdmitOutcome
		grant   *WorkerAdmitGrantFields
	}{
		{
			name: "declined with a plain reason",
			outcome: WorkerAdmitOutcome{
				State: WorkerAdmitStateDenied, Class: WorkerAdmitClassContended,
				Reason: WorkerAdmitReasonInsufficientHeadroom,
			},
		},
		{
			name: "detail carrying spaces, newlines and its own key=value pairs",
			outcome: WorkerAdmitOutcome{
				State: WorkerAdmitStateUnevaluated, Class: WorkerAdmitClassContractViolation,
				Reason: WorkerAdmitReasonUnknownDaemonOutcome,
				// A hostile detail: if the renderer did not escape it, this
				// would forge a grant on the far side.
				Detail: "state=granted class=granted scope=/evil worker_id=9\nmemory_max=1 memory_high=1 100% =",
			},
		},
		{
			name: "granted with placement fields",
			outcome: WorkerAdmitOutcome{
				State: WorkerAdmitStateGranted, Class: WorkerAdmitClassGranted,
			},
			grant: &WorkerAdmitGrantFields{
				ScopePath: "/sys/fs/cgroup/a b/.aira-worker-1", WorkerID: "1",
				MemoryMax: 400 << 20, MemoryHigh: 320 << 20,
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			line, err := WorkerAdmitOutcomeLine(test.outcome, test.grant)
			if err != nil {
				t.Fatalf("render: %v", err)
			}
			if strings.ContainsAny(line, "\n\r") {
				t.Fatalf("outcome line must stay on one line: %q", line)
			}
			fields, err := ParseWorkerAdmitOutcomeLine(line)
			if err != nil {
				t.Fatalf("parse %q: %v", line, err)
			}
			if fields["state"] != test.outcome.State || fields["class"] != test.outcome.Class {
				t.Fatalf("state/class round trip lost: %v from %q", fields, line)
			}
			if fields["reason"] != test.outcome.Reason || fields["detail"] != test.outcome.Detail {
				t.Fatalf("reason/detail round trip lost: %v from %q", fields, line)
			}
			if test.grant == nil {
				if _, present := fields["scope"]; present {
					t.Fatalf("a declined outcome must not carry placement fields: %v", fields)
				}
				return
			}
			if fields["scope"] != test.grant.ScopePath || fields["worker_id"] != test.grant.WorkerID {
				t.Fatalf("placement fields round trip lost: %v", fields)
			}
		})
	}
}

// verifies: AIRA-42 — a granted line always carries placement coordinates and
// a declined line never does, so a half-formed grant cannot be rendered at all.
func TestWorkerAdmitOutcomeLineRefusesInconsistentOutcomes(t *testing.T) {
	tests := []struct {
		name    string
		outcome WorkerAdmitOutcome
		grant   *WorkerAdmitGrantFields
	}{
		{"uncatalogued state", WorkerAdmitOutcome{State: "wat", Class: WorkerAdmitClassContended}, nil},
		{"uncatalogued class", WorkerAdmitOutcome{State: WorkerAdmitStateDenied, Class: "wat"}, nil},
		{
			"state granted but class not",
			WorkerAdmitOutcome{State: WorkerAdmitStateGranted, Class: WorkerAdmitClassContended},
			&WorkerAdmitGrantFields{},
		},
		{
			"class granted but state not",
			WorkerAdmitOutcome{State: WorkerAdmitStateDenied, Class: WorkerAdmitClassGranted},
			nil,
		},
		{
			"granted without placement fields",
			WorkerAdmitOutcome{State: WorkerAdmitStateGranted, Class: WorkerAdmitClassGranted},
			nil,
		},
		{
			"declined with placement fields",
			WorkerAdmitOutcome{State: WorkerAdmitStateDenied, Class: WorkerAdmitClassContended},
			&WorkerAdmitGrantFields{},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if line, err := WorkerAdmitOutcomeLine(test.outcome, test.grant); err == nil {
				t.Fatalf("expected a refusal, rendered %q", line)
			}
		})
	}
}

// verifies: AIRA-42 — the parser refuses anything outside the catalogue rather
// than defaulting, which is what keeps an unrecognised shape from being
// resolved into "the daemon is gone".
func TestParseWorkerAdmitOutcomeLineRefusesUncataloguedInput(t *testing.T) {
	tests := []struct{ name, line string }{
		{"empty", ""},
		{"no marker", "state=denied class=contended"},
		{"wrong marker", "granted scope=/x worker_id=1 memory_max=1 memory_high=1"},
		{"not key=value", WorkerAdmitOutcomeMarker + " state=denied contended"},
		{"missing state", WorkerAdmitOutcomeMarker + " class=contended"},
		{"missing class", WorkerAdmitOutcomeMarker + " state=denied"},
		{"uncatalogued state", WorkerAdmitOutcomeMarker + " state=wat class=contended"},
		{"uncatalogued class", WorkerAdmitOutcomeMarker + " state=denied class=wat"},
		{"grantedness disagreement", WorkerAdmitOutcomeMarker + " state=granted class=contended"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if fields, err := ParseWorkerAdmitOutcomeLine(test.line); err == nil {
				t.Fatalf("expected a refusal for %q, parsed %v", test.line, fields)
			}
		})
	}
}

// verifies: AIRA-42 — exactly two classes strip RAM containment, and both are
// named here so widening that set cannot happen silently.
func TestWorkerAdmitContainmentStrippingClassesAreExactlyTwo(t *testing.T) {
	stripping := map[string]bool{
		WorkerAdmitClassAdmissionUnusable: true,
		WorkerAdmitClassPlacementFailed:   true,
	}
	// The other four must not be in that set: granted uses the grant,
	// contended retries, and both terminal classes mark the queued work
	// unevaluated while leaving the daemon in use.
	for _, class := range []string{
		WorkerAdmitClassGranted, WorkerAdmitClassContended,
		WorkerAdmitClassRequestInvalid, WorkerAdmitClassContractViolation,
	} {
		if stripping[class] {
			t.Fatalf("class %q must not strip containment", class)
		}
	}
	if got := len(WorkerAdmitClasses()); got != len(stripping)+4 {
		t.Fatalf("the class catalogue has %d entries; this test enumerates %d — "+
			"a new class must declare whether it strips containment", got, len(stripping)+4)
	}
}
