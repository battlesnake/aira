package daemon

import (
	"math"
	"testing"
	"time"

	"aira/internal/runner"
)

// TestAdmitDoesNotDoubleCountAScanVisibleGrantedLease is S12's guard that the
// admission fit-check keeps a SINGLE accounting now that the AIRA-74 restart
// reserve-adoption term is gone. A granted lease whose scope the still-live
// confine scan ALSO sees (the restart-under-load shape: S11 reloads a survivor
// as a granted lease and the scan observes its populated scope on the same pass)
// must contribute its reserve exactly once, via the connection-held ledger
// (queue.outstanding) — never a second time via a scan-reconstructed `adopted`
// addend.
//
// It is the inverted retention of the deleted
// TestAdmitReconstructionGrantWindowCountsEachHeldJobOnce: that test asserted the
// adoption sum stayed 0 for a scan-visible held scope; with adoption deleted the
// live proof is that the fit-check grants a newcomer sized to fit ONLY against
// the single ledger.
//
// MUTATION (verified RED): re-introduce the deleted addend in evaluateAdmitQueue
// — sum the scan's populated finite-cap scopes and fold it into the fit-check's
// `outstanding` (the naive re-count, without the old `held` skip that used to
// keep this very case single) — and the newcomer below blocks instead of being
// granted, reddening this test.
func TestAdmitDoesNotDoubleCountAScanVisibleGrantedLease(t *testing.T) {
	now := time.Unix(120_000, 0)
	server := reconstructionTestServer(&now, func(string) (runner.ConfineListResult, error) {
		return runner.ConfineListResult{Verdict: "pass", Scopes: []runner.ConfineRecord{
			confineScanRecord("CONFINE-visible-1-a", 1, "40"),
		}}, nil
	})
	// A connection-held (or S11-reloaded) GRANTED lease holding the scope the scan
	// reports: 40 of the 100-byte slice, counted in the ledger as outstanding.
	held := &admitWaiter{seq: 1, reserve: 40, state: admitGranted, accounted: true, scopeID: "CONFINE-visible-1-a"}
	// A queued newcomer that fits ONLY if the scan-visible granted lease is counted
	// once: available = ceiling(100) - outstanding(40) = 60 >= 55. Were the scan's
	// populated scope re-counted (the deleted adoption addend), outstanding would
	// read 80 and available 20 < 55, and the newcomer would block.
	waiter := &admitWaiter{seq: 2, reserve: 55, state: admitQueued, grantedCh: make(chan struct{}), enqueued: now}
	queue := &sliceQueue{path: "/slice", server: server, waiters: []*admitWaiter{held, waiter}, outstanding: 40, outstandingJobs: 1}

	server.evaluateAdmitQueue(queue)

	if waiter.state != admitGranted {
		t.Fatalf("newcomer state=%v, want granted: a scan-visible granted lease must be counted ONCE (outstanding), never a second time via scan adoption", waiter.state)
	}
	if queue.outstanding != 95 || queue.outstandingJobs != 2 {
		t.Fatalf("outstanding=%d jobs=%d, want 95/2 (held 40 + granted 55) under a single accounting", queue.outstanding, queue.outstandingJobs)
	}
}

// TestAddClampSaturatesWithoutFabricatingHeadroom pins the saturating add the
// admission ledger uses throughout (queuedBytes, vanishedBytes, the grant-path
// sum). Relocated in S12 from the deleted admit_reconstruction_test.go; addClamp
// outlives the adoption sum that was one of its callers.
func TestAddClampSaturatesWithoutFabricatingHeadroom(t *testing.T) {
	for _, test := range []struct {
		a, b int64
		want int64
	}{
		{40, 2, 42},
		{math.MaxInt64 - 5, 5, math.MaxInt64},
		{math.MaxInt64 - 5, 6, math.MaxInt64},
		{math.MaxInt64, 1, math.MaxInt64},
		{-1, 1, math.MaxInt64},
		{1, -1, math.MaxInt64},
	} {
		if got := addClamp(test.a, test.b); got != test.want {
			t.Fatalf("addClamp(%d,%d)=%d, want %d", test.a, test.b, got, test.want)
		}
	}
}
