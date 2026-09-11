package daemon

import (
	"testing"
	"time"

	"aira/internal/runner"
)

// TestLedgerChargeIsTheDeclaredReserve pins the declared-only admission model
// (S1): a granted waiter contributes its DECLARED reserve to the ledger for the
// whole lifetime of the lease, with no live-usage tracking (the AIRA-29 dynamic
// charge is retired).
//
// This is the DIRECT mutation target for S1: making ledgerCharge() consult any
// value other than waiter.reserve for a non-nil waiter must red this test. The
// end-to-end ledger accounting is separately pinned by
// admit_small_reservation_contention_test.go's "outstanding == Σreserve"
// invariant, which the same mutation also reds.
func TestLedgerChargeIsTheDeclaredReserve(t *testing.T) {
	for _, tc := range []struct {
		name    string
		reserve int64
	}{
		{"typical", 7 << 30},
		{"zero", 0},
		{"cold-start default", 1 << 30},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// A granted, accounted, scope-backed waiter -- the population that
			// would once have carried a live-tracked charge.
			w := &admitWaiter{
				seq: 1, state: admitGranted, accounted: true,
				scopeID: "CONFINE-x-1-a", reserve: tc.reserve,
			}
			if got := w.ledgerCharge(); got != tc.reserve {
				t.Fatalf("ledgerCharge() = %d, want the declared reserve %d", got, tc.reserve)
			}
		})
	}

	// The nil guard stays: a nil waiter charges nothing rather than panicking.
	if got := (*admitWaiter)(nil).ledgerCharge(); got != 0 {
		t.Fatalf("nil waiter ledgerCharge() = %d, want 0", got)
	}
}

// TestScanPassDoesNotChargeLiveUsage is the evaluator-path pin: a scan pass must
// NOT rewrite queue.outstanding down to a connection-held waiter's live
// memory.current. Under declared-only accounting the waiter is charged its
// DECLARED reserve for the lease's whole lifetime, so a hog holding a 30 GiB
// reserve while its scan reports 2 GiB of live usage keeps the ledger at 30 GiB,
// and a 10 GiB newcomer on a 32 GiB slice stays QUEUED. This is the inverse of
// the retired AIRA-29 TestDynamicChargeFreesSpaceForAQueuedWaiter.
//
// Mutation-verified: any scan-path write of outstanding to the observed rss reds
// both assertions below.
func TestScanPassDoesNotChargeLiveUsage(t *testing.T) {
	const (
		sliceMax = 32 * gib
		reserve  = 30 * gib
		liveRSS  = 2 * gib
		newcomer = 10 * gib
	)
	now := time.Unix(500_000, 0)
	const hogScope = "CONFINE-hog-1-a"

	server := NewServer(Paths{})
	server.admitNow = func() time.Time { return now }
	server.admitConfineScanInterval = time.Second
	server.admitSliceHeadroomBase = 0
	server.admitSliceHeadroomSupervisor = 0
	server.admitReadMemory = func(string) (int64, int64, int64, bool, string) {
		return liveRSS, sliceMax, 0, true, ""
	}
	server.admitConfineScan = func(string) (runner.ConfineListResult, error) {
		populated, live := 1, true
		rss := int64(liveRSS)
		capText := formatInt64(reserve)
		return runner.ConfineListResult{Verdict: "pass", Scopes: []runner.ConfineRecord{
			{ScopeID: hogScope, Populated: &populated, SubtreePopulated: &live, RSSBytes: &rss, Cap: &capText},
		}}, nil
	}

	hog := &admitWaiter{
		seq: 1, reserve: reserve, state: admitGranted, accounted: true,
		scopeID: hogScope, grantedAt: now.Add(-time.Hour),
	}
	waiter := &admitWaiter{
		seq: 2, reserve: newcomer, state: admitQueued,
		grantedCh: make(chan struct{}), enqueued: now,
	}
	queue := &sliceQueue{
		path: "/slice", server: server, waiters: []*admitWaiter{hog, waiter},
		outstanding: reserve, outstandingJobs: 1,
	}

	server.evaluateAdmitQueue(queue)

	if queue.outstanding != reserve {
		t.Fatalf("queue.outstanding = %d after a scan pass, want the DECLARED reserve %d unchanged -- the scan must not charge live usage",
			queue.outstanding, int64(reserve))
	}
	if waiter.state != admitQueued {
		t.Fatalf("the %d newcomer was admitted (state=%v); with %d declared on a %d slice only ~%d is available",
			int64(newcomer), waiter.state, int64(reserve), int64(sliceMax), int64(sliceMax-reserve))
	}
}
