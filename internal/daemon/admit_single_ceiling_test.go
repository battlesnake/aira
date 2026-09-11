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

// TestDelegateScopesAdmitOnDeclaredReserveNotContainmentCap pins the S3
// loosening (owner-signed, design §14 DELETE list): a delegate scope is admitted
// on its DECLARED reserve, and its containment cap (scopeCeiling, the AIRA-15
// memory.max) plays no part in the fit-check. Three delegate suites on a 64 GiB
// slice each declare 1 GiB and carry a 48 GiB scope ceiling: Σcap = 144 GiB,
// which the retired AIRA-114 bound refused at its 2x default (128 GiB), while
// Σreserve = 3 GiB fits the single ceiling, so the third suite is admitted.
//
// RED on the pre-S3 tree (30d0659): the aggregate bound refused the third suite.
// The control arm keeps the test honest about WHICH gate it exercises: the same
// third suite declaring more than the ceiling leaves is refused.
//
// Mutation-verified: re-introducing a cap-based fit term (charging the sum of
// granted scope ceilings against the ceiling) reds the "Σcap over the ceiling
// admits" arm while leaving the control arm and the scope-less single-ceiling
// pin above unaffected.
func TestDelegateScopesAdmitOnDeclaredReserveNotContainmentCap(t *testing.T) {
	const (
		maximum      = 64 * gib
		suiteReserve = 1 * gib
		suiteCap     = 48 * gib
	)
	build := func(t *testing.T, newcomerReserve int64) *admitWaiter {
		t.Helper()
		now := time.Unix(600_000, 0)
		server := NewServer(Paths{})
		server.admitNow = func() time.Time { return now }
		server.admitConfineScanInterval = time.Nanosecond
		server.admitConfineScan = noConfinesScan
		server.admitSliceHeadroomBase = 0
		server.admitSliceHeadroomSupervisor = 0
		server.admitReadMemory = func(string) (int64, int64, int64, bool, string) {
			return 0, maximum, 0, true, ""
		}
		suiteA := &admitWaiter{
			seq: 1, reserve: suiteReserve, scopeCeiling: suiteCap, scopeID: "CONFINE-suite-1-a@session-a",
			state: admitGranted, accounted: true,
			grantedCh: make(chan struct{}), grantedAt: now.Add(-time.Hour),
		}
		suiteB := &admitWaiter{
			seq: 2, reserve: suiteReserve, scopeCeiling: suiteCap, scopeID: "CONFINE-suite-2-b@session-b",
			state: admitGranted, accounted: true,
			grantedCh: make(chan struct{}), grantedAt: now.Add(-time.Hour),
		}
		queued := &admitWaiter{
			seq: 3, reserve: newcomerReserve, scopeCeiling: suiteCap, scopeID: "CONFINE-suite-3-c@session-c",
			state: admitQueued, grantedCh: make(chan struct{}), enqueued: now,
		}
		queue := &sliceQueue{
			path: "/slice", server: server,
			waiters:     []*admitWaiter{suiteA, suiteB, queued},
			outstanding: 2 * suiteReserve, outstandingJobs: 2,
		}
		server.evaluateAdmitQueue(queue)
		return queued
	}
	t.Run("Σcap over the ceiling admits when Σreserve fits", func(t *testing.T) {
		if w := build(t, suiteReserve); w.state != admitGranted {
			t.Fatalf("third delegate suite (Σreserve=3G, Σcap=144G on a 64G slice) must be admitted on its declared reserve (state=%v)", w.state)
		}
	})
	t.Run("control: a declared reserve past what the ceiling leaves is refused", func(t *testing.T) {
		if w := build(t, maximum-2*suiteReserve+1); w.state != admitQueued {
			t.Fatalf("a delegate suite declaring one byte more than the ceiling leaves must be refused (state=%v)", w.state)
		}
	})
}
