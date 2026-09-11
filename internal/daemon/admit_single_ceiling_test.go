package daemon

import (
	"testing"
	"time"
)

// TestSingleCeilingRefusesWhenSumReserveExceedsCeiling pins the S3 invariant:
// with the AIRA-114 aggregate over-subscription bound deleted, the strict
// Σreserve ≤ ceiling property is enforced by the single slice ceiling ALONE.
// Under declared-only accounting (post-S1) checkedAvailable reduces to
// ceiling − Σreserve, so a queued waiter is granted iff its reserve fits what
// the ceiling leaves after the already-granted reserves.
//
// Σreserve landing EXACTLY on the ceiling is admitted; one byte past it is
// refused. The waiters are scope-less (`aira admit`), which carry no cgroup cap
// and so were never subject to the aggregate bound even before S3 — that is
// what makes this a test of the single ceiling rather than of the deleted
// aggregate, and why it is GREEN on the pre-deletion tree too.
//
// Mutation-verified: replacing the fit-check `waiter.reserve > available` with
// `waiter.reserve > addClamp(available, effectiveMaximum)` (an oversubscription
// multiplier on the ceiling) admits the one-byte-over case and reds the second
// arm.
func TestSingleCeilingRefusesWhenSumReserveExceedsCeiling(t *testing.T) {
	const (
		maximum = 64 * gib
		held    = 40 * gib
		fit     = maximum - held // 24 GiB: Σreserve lands exactly on the ceiling
	)
	build := func(t *testing.T, newcomer int64) *admitWaiter {
		t.Helper()
		now := time.Unix(500_000, 0)
		server := NewServer(Paths{})
		server.admitNow = func() time.Time { return now }
		server.admitConfineScanInterval = time.Nanosecond
		server.admitConfineScan = noConfinesScan
		// No headroom and a tiny physical current+reclaimable, so `available`
		// reduces to the declared term `ceiling − Σreserve`: this pins the
		// single-ceiling bound, not the physical floor (S4) or headroom.
		server.admitSliceHeadroomBase = 0
		server.admitSliceHeadroomSupervisor = 0
		server.admitReadMemory = func(string) (int64, int64, int64, bool, string) {
			return 0, maximum, 0, true, ""
		}
		granted := &admitWaiter{
			seq: 1, reserve: held, state: admitGranted, accounted: true,
			grantedCh: make(chan struct{}), grantedAt: now.Add(-time.Hour),
		}
		queued := &admitWaiter{
			seq: 2, reserve: newcomer, state: admitQueued,
			grantedCh: make(chan struct{}), enqueued: now,
		}
		queue := &sliceQueue{
			path: "/slice", server: server,
			waiters:     []*admitWaiter{granted, queued},
			outstanding: held, outstandingJobs: 1,
		}
		server.evaluateAdmitQueue(queue)
		return queued
	}

	t.Run("exactly at the ceiling is admitted", func(t *testing.T) {
		if w := build(t, fit); w.state != admitGranted {
			t.Fatalf("Σreserve of %d exactly at the %d ceiling must be admitted (state=%v)",
				int64(held+fit), int64(maximum), w.state)
		}
	})
	t.Run("one byte over the ceiling is refused", func(t *testing.T) {
		if w := build(t, fit+1); w.state != admitQueued {
			t.Fatalf("Σreserve of %d one byte over the %d ceiling must be refused (state=%v)",
				int64(held+fit+1), int64(maximum), w.state)
		}
	})
}
