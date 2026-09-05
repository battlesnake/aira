package main

import (
	"bytes"
	"strings"
	"testing"

	"aira/internal/core"
	"aira/internal/runner"
)

// AIRA-101. `confine --list` must say plainly when a slice is draining for or
// held by an exclusive job.
//
// The line is printed UNCONDITIONALLY, including the "none" case, on the same
// reasoning as the AIRA-68 breakdown beside it: a line that vanished when no
// exclusivity was active would be indistinguishable from a line that vanished
// because the daemon predates the feature, so an operator could not use its
// absence to rule a benchmark out. Ruling one out is most of the value — the
// question this answers is "why is my job waiting when the slice looks empty".
func renderExclusiveList(t *testing.T, reserve *runner.ConfineSliceReserve) string {
	t.Helper()
	result := runner.ConfineListResult{Verdict: "pass", Scopes: []runner.ConfineRecord{}, SliceReserve: reserve}
	var stdout, stderr bytes.Buffer
	if exit := renderConfineListResponse(core.Response{OK: true, Code: "OK", Data: result}, &stdout, &stderr); exit != 0 || stderr.Len() != 0 {
		t.Fatalf("exit=%d stderr=%q", exit, stderr.String())
	}
	return stdout.String()
}

func TestRenderConfineListReportsExclusiveHeldDrainingAndNone(t *testing.T) {
	base := func() *runner.ConfineSliceReserve {
		return &runner.ConfineSliceReserve{GrantedBytes: 1 << 30, CeilingBytes: 61 << 30, Jobs: 1}
	}

	none := renderExclusiveList(t, base())
	if !strings.Contains(none, "slice exclusive: none") {
		t.Fatalf("an idle slice must STATE that nothing is exclusive, got:\n%s", none)
	}

	held := base()
	held.Exclusive = &runner.ConfineExclusiveState{
		State: "held", Name: "bench-fft", Owner: "mark", ScopeID: "CONFINE-bench-fft-100-1@mark", WaitingJobs: 4,
	}
	heldOut := renderExclusiveList(t, held)
	if !strings.Contains(heldOut, `slice exclusive: held by "bench-fft" (mark), 4 jobs waiting`) {
		t.Fatalf("held line missing or malformed:\n%s", heldOut)
	}
	if strings.Contains(heldOut, "slice exclusive: none") {
		t.Fatalf("a held slice must not also report none:\n%s", heldOut)
	}

	draining := base()
	draining.Exclusive = &runner.ConfineExclusiveState{
		State: "draining", Name: "bench-fft", Owner: "mark", WaitingJobs: 1,
	}
	drainingOut := renderExclusiveList(t, draining)
	if !strings.Contains(drainingOut, `slice exclusive: draining for "bench-fft" (mark), 1 job waiting`) {
		t.Fatalf("draining line missing or malformed:\n%s", drainingOut)
	}
	// Draining and held are different situations for a waiting operator: one will
	// clear when running jobs finish, the other when a benchmark finishes.
	if drainingOut == heldOut {
		t.Fatal("draining and held must be distinguishable")
	}
}

// An unowned holder must be described as such rather than rendered with an empty
// pair of brackets, which reads as a bug in the tool rather than as missing
// detail.
func TestRenderConfineListNamesAnUnownedExclusiveHolderHonestly(t *testing.T) {
	reserve := &runner.ConfineSliceReserve{GrantedBytes: 1 << 30, CeilingBytes: 61 << 30, Jobs: 1}
	reserve.Exclusive = &runner.ConfineExclusiveState{State: "held", Name: "bench", WaitingJobs: 0}
	out := renderExclusiveList(t, reserve)
	if !strings.Contains(out, "unknown owner") {
		t.Fatalf("an unowned holder must be named as unknown, got:\n%s", out)
	}
	if strings.Contains(out, "()") {
		t.Fatalf("an empty owner must not render as empty brackets:\n%s", out)
	}
}

// With the daemon down, dispatchConfineManagement falls back to a local scan
// that produces no SliceReserve at all. Nothing about admission can be
// established there, so claiming "none" would be a fabricated fact.
func TestRenderConfineListOmitsExclusivityWhenNothingCanBeEstablished(t *testing.T) {
	out := renderExclusiveList(t, nil)
	if strings.Contains(out, "slice exclusive") {
		t.Fatalf("with no admission state available, exclusivity must not be claimed either way:\n%s", out)
	}
}
