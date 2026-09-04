package daemon

import (
	"errors"
	"testing"
	"time"

	"aira/internal/runner"
)

// AIRA-68. These drive evaluateAdmitQueue's OWN scan block — the only place
// scopeSeen/scopeVanished are ever written, and the place the whole safety
// argument for the vanished-proof reclaim lives.
//
// The stale-lease tests in confine_reaper_vanished_linux_test.go set those bits
// directly on the waiter, which is right for testing the CONSUMER but leaves the
// PRODUCER unpinned: mutation testing showed that reclaiming on plain absence
// (dropping the scopeSeen requirement) and marking a scope vanished on a
// SUCCESSFUL sighting both survived the entire reaper suite untouched. The
// transition is the invariant that makes the reclaim safe, so it is tested where
// it is computed.

func transitionTestWaiter(scopeID string) *admitWaiter {
	return &admitWaiter{
		seq: 1, reserve: 10, state: admitGranted, accounted: true,
		grantedCh: make(chan struct{}), scopeID: scopeID, name: "job", owner: "session-a",
	}
}

func transitionTestQueue(server *Server, waiter *admitWaiter) *sliceQueue {
	return &sliceQueue{
		path: "/slice", server: server, waiters: []*admitWaiter{waiter},
		outstanding: waiter.reserve, outstandingJobs: 1,
	}
}

// scanOf returns a scan reporting exactly the given scope ids as present.
func scanOf(scopeIDs ...string) func(string) (runner.ConfineListResult, error) {
	return func(string) (runner.ConfineListResult, error) {
		scopes := make([]runner.ConfineRecord, 0, len(scopeIDs))
		for _, id := range scopeIDs {
			scopes = append(scopes, confineScanRecord(id, 1, "10"))
		}
		return runner.ConfineListResult{Verdict: "pass", Scopes: scopes}, nil
	}
}

// verifies: the seen -> gone TRANSITION is what sets scopeVanished, and a scope
// the scan currently observes is never marked vanished.
func TestEvaluateAdmitQueueRecordsTheSeenThenGoneTransition(t *testing.T) {
	now := time.Unix(1000, 0)
	scopeID := "CONFINE-job-5101-a"
	server := reconstructionTestServer(&now, scanOf(scopeID))
	waiter := transitionTestWaiter(scopeID)
	queue := transitionTestQueue(server, waiter)

	server.evaluateAdmitQueue(queue)
	if !waiter.scopeSeen || waiter.scopeVanished {
		t.Fatalf("after a scan that OBSERVED the scope: seen=%v vanished=%v, want true/false",
			waiter.scopeSeen, waiter.scopeVanished)
	}

	// The scope is gone at the next scan. Advance past the refresh interval, or
	// the throttle skips the scan and no bit is written at all.
	now = now.Add(2 * time.Second)
	server.admitConfineScan = scanOf()
	server.evaluateAdmitQueue(queue)
	if !waiter.scopeSeen || !waiter.scopeVanished {
		t.Fatalf("after the scope went away: seen=%v vanished=%v, want true/true",
			waiter.scopeSeen, waiter.scopeVanished)
	}
}

// verifies: a scope the scan has NEVER observed is never marked vanished, no
// matter how many scans miss it. This is the producer-side regression for the
// unsafe design: reclaiming on plain absence would release the lease of a
// launcher stalled before scope creation, which would then create its scope and
// run entirely UNCHARGED — the AIRA-67 aggregate-OOM class.
func TestEvaluateAdmitQueueNeverMarksANeverSeenScopeVanished(t *testing.T) {
	now := time.Unix(2000, 0)
	server := reconstructionTestServer(&now, scanOf("CONFINE-someone-else-1-b"))
	waiter := transitionTestWaiter("CONFINE-stalled-launcher-5101-c")
	queue := transitionTestQueue(server, waiter)

	for pass := 0; pass < 3; pass++ {
		server.evaluateAdmitQueue(queue)
		now = now.Add(2 * time.Second)
	}

	if waiter.scopeSeen || waiter.scopeVanished {
		t.Fatalf("a scope that was never observed became a reclaim candidate: seen=%v vanished=%v, want false/false",
			waiter.scopeSeen, waiter.scopeVanished)
	}
}

// verifies: scopeVanished is a fresh observation, never a latch. A scope
// observed again after a miss must clear the bit, or a transient scan gap would
// leave a permanent reclaim candidate behind for a perfectly live job.
func TestEvaluateAdmitQueueClearsVanishedWhenTheScopeIsObservedAgain(t *testing.T) {
	now := time.Unix(3000, 0)
	scopeID := "CONFINE-flapping-5101-d"
	server := reconstructionTestServer(&now, scanOf(scopeID))
	waiter := transitionTestWaiter(scopeID)
	queue := transitionTestQueue(server, waiter)

	server.evaluateAdmitQueue(queue)
	now = now.Add(2 * time.Second)
	server.admitConfineScan = scanOf()
	server.evaluateAdmitQueue(queue)
	if !waiter.scopeVanished {
		t.Fatalf("precondition: the scope should be marked vanished after the miss")
	}

	now = now.Add(2 * time.Second)
	server.admitConfineScan = scanOf(scopeID)
	server.evaluateAdmitQueue(queue)

	if !waiter.scopeSeen || waiter.scopeVanished {
		t.Fatalf("a re-observed scope stayed vanished: seen=%v vanished=%v, want true/false",
			waiter.scopeSeen, waiter.scopeVanished)
	}
}

// verifies: fail-closed. A FAILED scan establishes nothing, so it must write
// neither bit — absence-by-error is not an observation of absence. Marking
// vanished here would turn a transient filesystem failure into a fleet-wide
// reclaim of every live lease at once.
func TestEvaluateAdmitQueueWritesNoTransitionBitOnAFailedScan(t *testing.T) {
	now := time.Unix(4000, 0)
	scopeID := "CONFINE-scan-fails-5101-e"
	server := reconstructionTestServer(&now, scanOf(scopeID))
	waiter := transitionTestWaiter(scopeID)
	queue := transitionTestQueue(server, waiter)

	server.evaluateAdmitQueue(queue)

	now = now.Add(2 * time.Second)
	server.admitConfineScan = func(string) (runner.ConfineListResult, error) {
		return runner.ConfineListResult{}, errors.New("cgroupfs unreadable")
	}
	server.evaluateAdmitQueue(queue)

	if !waiter.scopeSeen || waiter.scopeVanished {
		t.Fatalf("a failed scan wrote a transition bit: seen=%v vanished=%v, want true/false",
			waiter.scopeSeen, waiter.scopeVanished)
	}
	if !queue.adoptedScanFailed {
		t.Fatalf("precondition: the scan failure should have been recorded")
	}

	// A failed scan writes NEITHER bit — so an ALREADY-TRUE scopeVanished
	// survives it, and cannot be refreshed or cleared for as long as the scanner
	// stays broken. That is deliberate here (a failed scan must not invent an
	// observation in either direction) and is exactly why the consumer refuses to
	// act on the bit while adoptedScanFailed is set; see
	// TestReleaseStaleGrantedLeasesPassWillNotReclaimAVanishedLeaseWhileTheScanIsFailing.
	now = now.Add(2 * time.Second)
	server.admitConfineScan = scanOf()
	server.evaluateAdmitQueue(queue) // succeeds, scope absent -> vanished
	if !waiter.scopeVanished {
		t.Fatalf("precondition: the scope should be vanished after a successful scan missed it")
	}
	now = now.Add(2 * time.Second)
	server.admitConfineScan = func(string) (runner.ConfineListResult, error) {
		return runner.ConfineListResult{}, errors.New("cgroupfs unreadable again")
	}
	server.evaluateAdmitQueue(queue)
	if !waiter.scopeVanished || !queue.adoptedScanFailed {
		t.Fatalf("a failed scan must leave the bit untouched and record the failure: vanished=%v scanFailed=%v, want true/true",
			waiter.scopeVanished, queue.adoptedScanFailed)
	}
}

// verifies: an "unevaluated" scan verdict is treated as a FAILURE, not as an
// empty scan. AIRA's honesty contract makes unevaluated the value for state that
// could not be read; consuming it as "no scopes exist" would mark every live
// lease vanished at once.
func TestEvaluateAdmitQueueTreatsAnUnevaluatedScanAsAFailure(t *testing.T) {
	now := time.Unix(5000, 0)
	scopeID := "CONFINE-unevaluated-5101-f"
	server := reconstructionTestServer(&now, scanOf(scopeID))
	waiter := transitionTestWaiter(scopeID)
	queue := transitionTestQueue(server, waiter)

	server.evaluateAdmitQueue(queue)

	now = now.Add(2 * time.Second)
	server.admitConfineScan = func(string) (runner.ConfineListResult, error) {
		return runner.ConfineListResult{Verdict: "unevaluated", Reason: "cgroup root not readable"}, nil
	}
	server.evaluateAdmitQueue(queue)

	if !waiter.scopeSeen || waiter.scopeVanished {
		t.Fatalf("an unevaluated scan was read as an observation of absence: seen=%v vanished=%v, want true/false",
			waiter.scopeSeen, waiter.scopeVanished)
	}
}

// verifies: a scope-less reservation (`aira confine-reserve`, scopeID == "") is
// never given a transition bit, even when the scan itself yields a record with
// an empty scope id.
//
// The empty-id record is the point. Today's real scanner cannot produce one —
// listConfines only emits ids that begin with "CONFINE-" and parse — so without
// it this test passes against an implementation that has dropped the
// `scopeID == ""` guard entirely, which mutation testing duly showed. But
// admitConfineScan is an injected seam, and the guard is what stops an empty id
// on either side from matching the other: with the guard gone, a reservation
// would be marked seen by such a record, marked vanished on the next scan
// without it, and become reclaimable — a lease with NO cgroup artifact released
// on a proof that cannot exist for it. The guard is pinned against the seam's
// contract rather than against today's single implementation of it.
func TestEvaluateAdmitQueueLeavesScopelessReservationsUntouched(t *testing.T) {
	now := time.Unix(6000, 0)
	server := reconstructionTestServer(&now, func(string) (runner.ConfineListResult, error) {
		return runner.ConfineListResult{Verdict: "pass", Scopes: []runner.ConfineRecord{
			confineScanRecord("", 1, "10"),
		}}, nil
	})
	reservation := transitionTestWaiter("")
	queue := transitionTestQueue(server, reservation)

	server.evaluateAdmitQueue(queue)
	if reservation.scopeSeen || reservation.scopeVanished {
		t.Fatalf("an empty-id scan record was matched to a scope-less reservation: seen=%v vanished=%v, want false/false",
			reservation.scopeSeen, reservation.scopeVanished)
	}

	// ...and the scope-less reservation is still untouched once that record goes
	// away, which is where the reclaim would actually be armed.
	now = now.Add(2 * time.Second)
	server.admitConfineScan = scanOf()
	server.evaluateAdmitQueue(queue)

	if reservation.scopeSeen || reservation.scopeVanished {
		t.Fatalf("a scope-less reservation got a transition bit: seen=%v vanished=%v, want false/false",
			reservation.scopeSeen, reservation.scopeVanished)
	}
}
