package main

import (
	"strings"
	"testing"

	"aira/internal/runner"
)

// AIRA-119. `confine --list`'s exclusive line was reported as naming a job that
// had already released. The daemon's identity is derived live from its waiter
// list on every read and provably tracks the gate (see
// internal/daemon/admit_exclusive_identity_test.go), so the name is not stale —
// but the LINE could not be acted on, and that is what made a live, correct
// report indistinguishable from a stale one:
//
//   - `draining for "X"` reads as though X were running. It is not: `aira
//     confine` creates its scope and launches its target only after admission is
//     granted, so a draining job owns no process and no cgroup scope and has no
//     row in the table above. An operator greps for X, finds nothing, and
//     concludes the daemon is naming a departed job.
//   - `Name` defaults to "job" for every unnamed confine and `Owner` may come
//     from AIRA_CONFINE_OWNER, so neither need appear in any argv. The scope id —
//     unique, greppable, and the selector `confine --kill` takes — was on the
//     wire from the start and no face printed it.
//   - Nothing said how long the state had lasted, which is the only thing that
//     separates a routine 30-second drain from an 18-minute wedge.
//
// verifies: AIRA-119
func TestRenderConfineListExclusiveLineCanBeActedOn(t *testing.T) {
	base := func() *runner.ConfineSliceReserve {
		return &runner.ConfineSliceReserve{GrantedBytes: 1 << 30, CeilingBytes: 61 << 30, Jobs: 1}
	}

	held := base()
	held.Exclusive = &runner.ConfineExclusiveState{
		State: "held", Name: "job", Owner: "fdtd4",
		ScopeID: "CONFINE-job-4242-1@fdtd4", WaitingJobs: 4, SinceMS: 252_000,
	}
	heldOut := renderExclusiveList(t, held)
	if !strings.Contains(heldOut, "scope=CONFINE-job-4242-1@fdtd4") {
		t.Fatalf("the held line must name the scope id, the only unique handle an operator has:\n%s", heldOut)
	}
	if !strings.Contains(heldOut, "running alone for 4m12s") {
		t.Fatalf("the held line must say how long the job has been running alone:\n%s", heldOut)
	}
	if strings.Contains(heldOut, "not started") {
		t.Fatalf("a HELD job is running; the line must not say otherwise:\n%s", heldOut)
	}

	draining := base()
	draining.Exclusive = &runner.ConfineExclusiveState{
		State: "draining", Name: "job", Owner: "fdtd4",
		ScopeID: "CONFINE-job-4242-1@fdtd4", WaitingJobs: 10, SinceMS: 1_083_000,
	}
	drainingOut := renderExclusiveList(t, draining)
	// The exact misreading AIRA-119 was filed as: the reporter searched for the
	// named job, found no process, and concluded the daemon was naming a job that
	// had already released. The line must state that it has not started.
	if !strings.Contains(drainingOut, "not started yet") {
		t.Fatalf("a DRAINING job has not launched — the line must say so, or it reads as a stale name:\n%s", drainingOut)
	}
	if !strings.Contains(drainingOut, "draining for 18m3s") {
		t.Fatalf("the draining line must say how long the drain has been failing to converge:\n%s", drainingOut)
	}
	if !strings.Contains(drainingOut, "scope=CONFINE-job-4242-1@fdtd4") {
		t.Fatalf("the draining line must name the scope id — a draining job has no table row to find it in:\n%s", drainingOut)
	}
}

// An unestablished age is an ABSENCE, never "0s". Printing zero would state that
// a drain had just begun, which is the one reading that would send an operator
// away from a wedge rather than towards it.
//
// verifies: AIRA-119
func TestRenderConfineListOmitsAnUnestablishedExclusiveAge(t *testing.T) {
	reserve := &runner.ConfineSliceReserve{GrantedBytes: 1 << 30, CeilingBytes: 61 << 30, Jobs: 1}
	reserve.Exclusive = &runner.ConfineExclusiveState{
		State: "draining", Name: "bench", Owner: "mark",
		ScopeID: "CONFINE-bench-9-1@mark", WaitingJobs: 2, SinceMS: 0,
	}
	out := renderExclusiveList(t, reserve)
	if strings.Contains(out, "for 0s") {
		t.Fatalf("an unestablished age must not be rendered as a duration:\n%s", out)
	}
	if !strings.Contains(out, "not started yet") {
		t.Fatalf("the state itself is still established and must still be stated:\n%s", out)
	}
	if !strings.Contains(out, "2 jobs waiting") {
		t.Fatalf("the rest of the line must survive an absent age:\n%s", out)
	}
}
