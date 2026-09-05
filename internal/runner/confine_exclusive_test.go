package runner

import (
	"encoding/json"
	"strings"
	"testing"
)

// AIRA-101's must-know result.
//
// These tests exist because of a real incident: an hour of benchmark throughput
// numbers was invalidated by contention nobody noticed. A run that silently
// degrades to non-exclusive produces numbers that LOOK clean, which is strictly
// worse than no feature at all. So the properties under test are not cosmetic —
// they are the whole point of the flag.

// The trailer of an exclusive run must never be byte-identical to a
// non-exclusive one. This is the AIRA-70/91 shape of assertion, and it is what
// stops the facet shipping inert: a facet nobody can distinguish from its
// absence reports nothing.
func TestExclusiveTrailerIsDistinguishableFromANonExclusiveRun(t *testing.T) {
	plain := FormatConfineStatus(ConfineStatus{Slice: "aira.slice", TerminatedBy: ConfineTerminatedNormal})
	exclusive := FormatConfineStatus(ConfineStatus{
		Slice: "aira.slice", TerminatedBy: ConfineTerminatedNormal,
		Exclusive: ConfineExclusiveGranted,
	})
	if plain == exclusive {
		t.Fatal("an exclusive run's trailer must not be byte-identical to a non-exclusive run's")
	}
	if strings.Contains(plain, "exclusive=") {
		t.Fatalf("a run that never asked for exclusivity must not claim a verdict: %s", plain)
	}
	if !strings.Contains(exclusive, "exclusive=granted") {
		t.Fatalf("expected exclusive=granted, got: %s", exclusive)
	}
}

// A lost hold must be reported as lost, never left looking granted. This is the
// exact failure the field incident consisted of.
func TestExclusiveTrailerReportsALostHold(t *testing.T) {
	line := FormatConfineStatus(ConfineStatus{
		Slice: "aira.slice", TerminatedBy: ConfineTerminatedNormal,
		Exclusive: ConfineExclusiveLost,
	})
	if !strings.Contains(line, "exclusive=lost") {
		t.Fatalf("expected exclusive=lost, got: %s", line)
	}
	if strings.Contains(line, "exclusive=granted") {
		t.Fatalf("a lost hold must never also read as granted: %s", line)
	}
}

// The acquisition condition travels with the result, so a benchmark carries its
// own drain time rather than relying on the operator's memory. Only for a run
// that actually got the slice: a lost or unevaluated run has no honest figure.
func TestExclusiveTrailerCarriesTheDrainWaitOnlyWhenGranted(t *testing.T) {
	granted := FormatConfineStatus(ConfineStatus{
		Slice: "aira.slice", TerminatedBy: ConfineTerminatedNormal,
		Exclusive: ConfineExclusiveGranted, ExclusiveDrainedMS: 45_000,
	})
	if !strings.Contains(granted, "drained-for=45s") {
		t.Fatalf("expected drained-for=45s, got: %s", granted)
	}
	lost := FormatConfineStatus(ConfineStatus{
		Slice: "aira.slice", TerminatedBy: ConfineTerminatedNormal,
		Exclusive: ConfineExclusiveLost, ExclusiveDrainedMS: 45_000,
	})
	if strings.Contains(lost, "drained-for=") {
		t.Fatalf("a run that lost its hold must not report a drain figure as if it held: %s", lost)
	}
}

// A DETACHED launch hands its whole ConfineRequest to the supervisor process as
// JSON through a control file. If Exclusive did not survive that round trip, a
// detached benchmark would be admitted NON-exclusively while its operator
// believed otherwise — the silent downgrade this feature exists to prevent, in
// the one path where nobody is watching the terminal.
//
// Pinned explicitly because the failure is invisible: the field carries no json
// tag today, and adding `json:"-"` to it later would break this silently.
func TestExclusiveSurvivesTheDetachedRequestRoundTrip(t *testing.T) {
	encoded, err := json.Marshal(ConfineRequest{Name: "bench", Exclusive: true})
	if err != nil {
		t.Fatal(err)
	}
	var decoded ConfineRequest
	if err := json.Unmarshal(encoded, &decoded); err != nil {
		t.Fatal(err)
	}
	if !decoded.Exclusive {
		t.Fatal("Exclusive did not survive the detached control-file round trip: a detached --exclusive job would run non-exclusively without saying so")
	}
}

// An unevaluated outcome must read as unevaluated and never default to granted.
func TestExclusiveTrailerNeverDefaultsToGranted(t *testing.T) {
	line := FormatConfineStatus(ConfineStatus{
		Slice: "aira.slice", TerminatedBy: ConfineTerminatedNormal,
		Exclusive: ConfineExclusiveUnevaluated,
	})
	if !strings.Contains(line, "exclusive=unevaluated") {
		t.Fatalf("expected exclusive=unevaluated, got: %s", line)
	}
}
