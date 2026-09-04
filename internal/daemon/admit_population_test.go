package daemon

import (
	"strconv"
	"testing"
	"time"
)

// AIRA-68. `aira confine --list` printed ONE "N admitted jobs" number over three
// structurally different ledger populations, above a table that lists only
// SCOPES. Reading the number against the table is invalid, and doing so is what
// produced AIRA-68's P0 misdiagnosis: 20 of the 23 "admitted jobs" were
// scope-less `aira confine-reserve` per-test reservations (AIRA-69), which
// create no cgroup scope and so can never appear as a row.
//
// These tests drive the split. They are deliberately written against
// admitSliceSnapshot rather than the rendered line so a renderer change cannot
// silently satisfy them.

func populationTestWaiter(seq int64, reserve int64, scopeID string) *admitWaiter {
	return &admitWaiter{
		seq: seq, reserve: reserve, state: admitGranted, accounted: true,
		grantedCh: make(chan struct{}), scopeID: scopeID,
	}
}

func populationTestScopeID(name string, pid int) string {
	return "CONFINE-" + name + "-" + strconv.Itoa(pid) + "-" + strconv.FormatInt(time.Now().UnixNano(), 36)
}

// verifies: the ledger reports connection-held SCOPE-BACKED jobs, connection-held
// SCOPE-LESS reservations and scan-adopted scopes as three separate populations,
// in both counts and bytes, while the totals keep their present meaning.
//
// The classifier is scopeID, and nothing else. Classifying on name or owner
// would pass a naive test, because validateAdmitArgs requires the
// scope_id/name/owner tuple to be supplied together — so the mutation this test
// must break is "treat every connection-held grant as scope-backed", not
// "classify by name".
func TestAdmitSnapshotSeparatesTheThreeLedgerPopulations(t *testing.T) {
	server := NewServer(Paths{})
	queue := &sliceQueue{path: "/slice", server: server, kick: make(chan struct{}, 1), stop: make(chan struct{})}
	queue.waiters = []*admitWaiter{
		populationTestWaiter(1, 20<<30, populationTestScopeID("test-lite", 3193207)),
		populationTestWaiter(2, 512<<20, populationTestScopeID("merge-gate", 1962025)),
		populationTestWaiter(3, 1<<30, ""),
		populationTestWaiter(4, 1<<30, ""),
		populationTestWaiter(5, 512<<20, ""),
		{seq: 6, reserve: 4 << 30, state: admitQueued, grantedCh: make(chan struct{})},
	}
	queue.outstanding = 20<<30 + 512<<20 + 1<<30 + 1<<30 + 512<<20
	queue.outstandingJobs = 5
	queue.adopted, queue.adoptedJobs = 8<<30, 2
	server.admitQueues["/slice"] = queue

	snapshot := server.admitSliceSnapshot("/slice")

	if snapshot.scopeJobs != 2 || snapshot.scopeBytes != 20<<30+512<<20 {
		t.Errorf("scope-backed population = %d jobs / %d bytes, want 2 / %d", snapshot.scopeJobs, snapshot.scopeBytes, int64(20<<30+512<<20))
	}
	if snapshot.reservationJobs != 3 || snapshot.reservationBytes != 1<<30+1<<30+512<<20 {
		t.Errorf("scope-less reservation population = %d jobs / %d bytes, want 3 / %d", snapshot.reservationJobs, snapshot.reservationBytes, int64(1<<30+1<<30+512<<20))
	}
	if snapshot.adoptedJobs != 2 || snapshot.adopted != 8<<30 {
		t.Errorf("adopted population = %d jobs / %d bytes, want 2 / %d", snapshot.adoptedJobs, snapshot.adopted, int64(8<<30))
	}
	if snapshot.queued != 1 {
		t.Errorf("queued = %d, want 1 (the split must not swallow queued waiters)", snapshot.queued)
	}
	// The totals keep their present meaning: nothing about admission changes.
	if snapshot.outstandingJobs != 5 || snapshot.outstanding != queue.outstanding {
		t.Errorf("totals moved: %d jobs / %d bytes", snapshot.outstandingJobs, snapshot.outstanding)
	}
	if jobs, bytes := snapshot.residualJobs(), snapshot.residualBytes(); jobs != 0 || bytes != 0 {
		t.Errorf("consistent ledger reported residual jobs=%d bytes=%d, want 0/0", jobs, bytes)
	}
}

// verifies: the derived split and the incremental counters are cross-checked,
// and a JOB-count divergence is reported rather than hidden. Any divergence is a
// real lost/double decrement: a waiter is `granted && accounted` if and only if
// it was counted.
func TestAdmitSnapshotReportsJobResidualWhenTheCounterDesynchronises(t *testing.T) {
	server := NewServer(Paths{})
	queue := &sliceQueue{path: "/slice", server: server, kick: make(chan struct{}, 1), stop: make(chan struct{})}
	queue.waiters = []*admitWaiter{populationTestWaiter(1, 4<<30, "")}
	queue.outstanding, queue.outstandingJobs = 4<<30, 3 // two phantom jobs
	server.admitQueues["/slice"] = queue

	snapshot := server.admitSliceSnapshot("/slice")
	if got := snapshot.residualJobs(); got != 2 {
		t.Fatalf("residual jobs = %d, want 2", got)
	}
	if got := snapshot.residualBytes(); got != 0 {
		t.Fatalf("residual bytes = %d, want 0 (only the job counter diverged)", got)
	}
}

// verifies: the BYTE residual is reported independently of the job residual, and
// a NEGATIVE residual survives as a signed value.
//
// This is not redundant with the job residual. The single most plausible
// regression in releaseAdmitWaiter — dropping `outstanding -= waiter.reserve`
// while keeping `outstandingJobs--` — is byte-only, and a job-only residual
// would report a perfectly consistent ledger while the slice silently filled.
func TestAdmitSnapshotReportsByteResidualIndependentlyOfJobs(t *testing.T) {
	server := NewServer(Paths{})
	queue := &sliceQueue{path: "/slice", server: server, kick: make(chan struct{}, 1), stop: make(chan struct{})}
	queue.waiters = []*admitWaiter{populationTestWaiter(1, 4<<30, "")}
	queue.outstanding, queue.outstandingJobs = 6<<30, 1 // 2 GiB never discharged
	server.admitQueues["/slice"] = queue

	snapshot := server.admitSliceSnapshot("/slice")
	if got := snapshot.residualBytes(); got != 2<<30 {
		t.Fatalf("residual bytes = %d, want %d", got, int64(2<<30))
	}
	if got := snapshot.residualJobs(); got != 0 {
		t.Fatalf("residual jobs = %d, want 0 (only the byte counter diverged)", got)
	}

	queue.outstanding = 3 << 30 // discharged more than was ever charged
	if got := server.admitSliceSnapshot("/slice").residualBytes(); got != -(1 << 30) {
		t.Fatalf("residual bytes = %d, want %d — a negative residual must survive as a signed value, never be floored", got, -(int64(1) << 30))
	}
}

// verifies: an ABSENT queue keeps its existing honesty contract exactly — a
// genuine idle zero with present=false — and gains no split, no vanished count
// and no spurious inconsistency. A missing queue positively establishes that
// nothing is admitted against that slice; it is not an unevaluated read, and it
// must not be rendered as one.
func TestAdmitSnapshotAbsentQueueStaysAGenuineIdleZero(t *testing.T) {
	server := NewServer(Paths{})
	snapshot := server.admitSliceSnapshot("/never-used")

	if snapshot.present {
		t.Fatal("absent queue reported present; callers would render a fabricated ledger")
	}
	if snapshot.outstanding != 0 || snapshot.outstandingJobs != 0 || snapshot.adopted != 0 || snapshot.adoptedJobs != 0 {
		t.Fatalf("absent queue reported non-zero totals: %+v", snapshot)
	}
	if snapshot.scopeJobs != 0 || snapshot.reservationJobs != 0 || snapshot.vanishedJobs != 0 {
		t.Fatalf("absent queue reported a non-zero split: %+v", snapshot)
	}
	if snapshot.residualJobs() != 0 || snapshot.residualBytes() != 0 {
		t.Fatalf("absent queue reported a residual: jobs=%d bytes=%d", snapshot.residualJobs(), snapshot.residualBytes())
	}
}

// verifies: vanishedJobs/vanishedBytes are a SUBSET of the scope-backed
// population, not a fourth one — the split must still sum to the totals, or the
// residual cross-check above would cry wolf on every vanished lease.
func TestAdmitSnapshotVanishedLeasesRemainInsideTheScopeBackedPopulation(t *testing.T) {
	server := NewServer(Paths{})
	queue := &sliceQueue{path: "/slice", server: server, kick: make(chan struct{}, 1), stop: make(chan struct{})}
	live := populationTestWaiter(1, 2<<30, populationTestScopeID("live", 101))
	live.scopeSeen = true
	gone := populationTestWaiter(2, 4<<30, populationTestScopeID("gone", 102))
	gone.scopeSeen, gone.scopeVanished = true, true
	queue.waiters = []*admitWaiter{live, gone}
	queue.outstanding, queue.outstandingJobs = 6<<30, 2
	server.admitQueues["/slice"] = queue

	snapshot := server.admitSliceSnapshot("/slice")
	if snapshot.vanishedJobs != 1 || snapshot.vanishedBytes != 4<<30 {
		t.Errorf("vanished = %d jobs / %d bytes, want 1 / %d", snapshot.vanishedJobs, snapshot.vanishedBytes, int64(4<<30))
	}
	if snapshot.scopeJobs != 2 || snapshot.scopeBytes != 6<<30 {
		t.Errorf("scope-backed = %d jobs / %d bytes, want 2 / %d — a vanished lease is still a scope-backed lease", snapshot.scopeJobs, snapshot.scopeBytes, int64(6<<30))
	}
	if snapshot.residualJobs() != 0 || snapshot.residualBytes() != 0 {
		t.Errorf("vanished leases broke the split sum: jobs=%d bytes=%d", snapshot.residualJobs(), snapshot.residualBytes())
	}
}
