package daemon

import (
	"errors"
	"math"
	"os"
	"strings"
	"testing"
	"time"

	"aira/internal/runner"
)

// AIRA-29. The dynamic reserve: the admission ledger charges each confine scope
// its LIVE cgroup usage plus a margin, re-evaluated on the evaluator's own <=1s
// confine scan, instead of holding the frozen peak ESTIMATE for the whole job
// lifetime.
//
// Every test here states the exact expected charge rather than a bound, and
// names the wrong implementation that a bound would have let through. That is
// not stylistic: "the charge is below the reserve" is satisfied by an
// implementation that charges a constant fraction of the cap and tracks nothing
// at all, and "the charge is at least peak+growth" is satisfied by one that
// always charges the cap. Both are exactly the plausible wrong builds here.
//
// verifies: the AIRA-29 charge formula, the usable-record rule, ledger
// conservation, and the post-restart adoption generalisation.

const (
	gib = int64(1) << 30
	mib = int64(1) << 20
)

// dynamicChargeServer is a server whose admission arithmetic is entirely
// determined by the test: no headroom, a fixed clock, and a scan seam.
func dynamicChargeServer(now *time.Time, scan func(string) (runner.ConfineListResult, error)) *Server {
	server := NewServer(Paths{})
	server.admitNow = func() time.Time { return *now }
	server.admitConfineScanInterval = time.Nanosecond
	server.admitConfineScan = scan
	server.admitSliceHeadroomBase = 0
	server.admitSliceHeadroomSupervisor = 0
	server.admitReadMemory = func(string) (int64, int64, int64, bool, string) {
		return 0, 1 << 50, 0, true, ""
	}
	return server
}

// chargeScanRecord is a scan record shaped the way a live, healthy confine
// scope reads: a usable hierarchical memory.current and a finite memory.max.
func chargeScanRecord(scopeID string, rss, capBytes int64) runner.ConfineRecord {
	populated := 1
	capText := formatInt64(capBytes)
	age := int64(3600)
	return runner.ConfineRecord{
		ScopeID: scopeID, Populated: &populated, RSSBytes: &rss, Cap: &capText, AgeSeconds: &age,
	}
}

func formatInt64(value int64) string {
	return string(appendInt(nil, value))
}

func appendInt(dst []byte, value int64) []byte {
	if value == 0 {
		return append(dst, '0')
	}
	negative := value < 0
	var digits [20]byte
	index := len(digits)
	magnitude := value
	if negative {
		magnitude = -magnitude
	}
	for magnitude > 0 {
		index--
		digits[index] = byte('0' + magnitude%10)
		magnitude /= 10
	}
	if negative {
		index--
		digits[index] = '-'
	}
	return append(dst, digits[index:]...)
}

// grantedChargeWaiter builds a waiter already granted and accounted, as the
// grant loop leaves one, with its charge still at the frozen reserve.
func grantedChargeWaiter(scopeID string, reserve int64, grantedAt time.Time) *admitWaiter {
	return &admitWaiter{
		seq: 1, reserve: reserve, state: admitGranted, accounted: true,
		scopeID: scopeID, grantedAt: grantedAt,
	}
}

func expectedMargin(server *Server, peak, growth int64) int64 {
	margin := server.chargeMarginFloor
	if pct := pctClamp(peak, server.chargeMarginPct); pct > margin {
		margin = pct
	}
	if growth > margin {
		margin = growth
	}
	return margin
}

// TestDynamicChargeTracksActualUsage is the headline behaviour: the money case
// from the ticket, a job holding a 33.6G reserve while using 2.6G.
//
// The assertion is the EXACT charge. A "charge < reserve" assertion would pass
// against an implementation that charged, say, half the cap and never read
// memory.current at all.
func TestDynamicChargeTracksActualUsage(t *testing.T) {
	now := time.Unix(10_000, 0)
	const reserve = 33600 * mib
	const rss = 2600 * mib
	server := dynamicChargeServer(&now, func(string) (runner.ConfineListResult, error) {
		return runner.ConfineListResult{Verdict: "pass", Scopes: []runner.ConfineRecord{
			chargeScanRecord("CONFINE-money-1-a", rss, reserve),
		}}, nil
	})
	// Granted long enough ago that the cold floor has lapsed.
	waiter := grantedChargeWaiter("CONFINE-money-1-a", reserve, now.Add(-10*time.Minute))
	queue := &sliceQueue{path: "/slice", server: server, waiters: []*admitWaiter{waiter}, outstanding: reserve, outstandingJobs: 1}

	server.evaluateAdmitQueue(queue)

	want := rss + expectedMargin(server, rss, 0)
	if waiter.ledgerCharge() != want {
		t.Fatalf("effective charge = %d, want exactly %d (peak %d + margin)", waiter.ledgerCharge(), want, rss)
	}
	if queue.outstanding != want {
		t.Fatalf("outstanding = %d, want exactly %d", queue.outstanding, want)
	}
	if waiter.ledgerCharge() >= reserve {
		t.Fatalf("charge %d did not fall below the frozen reserve %d", waiter.ledgerCharge(), reserve)
	}
}

// TestDynamicChargeColdFloorHoldsTheEstimate covers BOTH halves. The warm half
// alone would pass against an implementation with no cold floor whatsoever,
// which is exactly the cold-start hole: a job admitted on a 33.6G estimate that
// has not allocated yet must not free 33.6G of ledger and then allocate 20G.
func TestDynamicChargeColdFloorHoldsTheEstimate(t *testing.T) {
	now := time.Unix(20_000, 0)
	const reserve = 33600 * mib
	const rss = 4 * mib
	server := dynamicChargeServer(&now, func(string) (runner.ConfineListResult, error) {
		return runner.ConfineListResult{Verdict: "pass", Scopes: []runner.ConfineRecord{
			chargeScanRecord("CONFINE-cold-1-a", rss, reserve),
		}}, nil
	})
	granted := now.Add(-server.chargeColdFloorWindow / 2)
	waiter := grantedChargeWaiter("CONFINE-cold-1-a", reserve, granted)
	queue := &sliceQueue{path: "/slice", server: server, waiters: []*admitWaiter{waiter}, outstanding: reserve, outstandingJobs: 1}

	server.evaluateAdmitQueue(queue)
	if waiter.ledgerCharge() != reserve {
		t.Fatalf("inside the cold-floor window the charge = %d, want exactly the reserve %d", waiter.ledgerCharge(), reserve)
	}
	if queue.outstanding != reserve {
		t.Fatalf("inside the cold-floor window outstanding = %d, want %d", queue.outstanding, reserve)
	}

	// One tick past the window the floor lapses and the tracked value takes over.
	now = granted.Add(server.chargeColdFloorWindow + time.Second)
	queue.adoptedAt = time.Time{}
	server.evaluateAdmitQueue(queue)

	want := rss + expectedMargin(server, rss, 0)
	if waiter.ledgerCharge() != want {
		t.Fatalf("past the cold-floor window the charge = %d, want exactly %d", waiter.ledgerCharge(), want)
	}
}

// TestDynamicChargeRatchetsOverLifetime pins the lifetime peak ratchet: a job
// that has demonstrated 8G never charges less again, even when its usage falls
// back. "not below 8G" would pass against frozen-at-reserve AND against
// always-charge-the-cap, so the assertion is the exact value plus a strict
// below-cap check.
func TestDynamicChargeRatchetsOverLifetime(t *testing.T) {
	now := time.Unix(30_000, 0)
	const reserve = 32 * gib
	rss := int64(1 * gib)
	server := dynamicChargeServer(&now, func(string) (runner.ConfineListResult, error) {
		return runner.ConfineListResult{Verdict: "pass", Scopes: []runner.ConfineRecord{
			chargeScanRecord("CONFINE-ratchet-1-a", rss, reserve),
		}}, nil
	})
	waiter := grantedChargeWaiter("CONFINE-ratchet-1-a", reserve, now.Add(-time.Hour))
	queue := &sliceQueue{path: "/slice", server: server, waiters: []*admitWaiter{waiter}, outstanding: reserve, outstandingJobs: 1}

	server.evaluateAdmitQueue(queue)
	firstCharge := waiter.ledgerCharge()

	rss = 8 * gib
	now = now.Add(time.Second)
	queue.adoptedAt = time.Time{}
	server.evaluateAdmitQueue(queue)
	peakCharge := waiter.ledgerCharge()
	if peakCharge <= firstCharge {
		t.Fatalf("charge did not rise with usage: %d -> %d", firstCharge, peakCharge)
	}

	rss = 1 * gib
	now = now.Add(time.Second)
	queue.adoptedAt = time.Time{}
	server.evaluateAdmitQueue(queue)

	if waiter.ledgerCharge() != peakCharge {
		t.Fatalf("charge fell after the peak: %d, want exactly the ratcheted %d", waiter.ledgerCharge(), peakCharge)
	}
	if waiter.ledgerCharge() >= reserve {
		t.Fatalf("ratcheted charge %d reached the cap %d; the ratchet must not become always-charge-the-cap", waiter.ledgerCharge(), reserve)
	}
	if queue.outstanding != waiter.ledgerCharge() {
		t.Fatalf("outstanding %d != effective charge %d", queue.outstanding, waiter.ledgerCharge())
	}
}

// TestDynamicChargeMarginAbsorbsObservedGrowth pins the self-tuning margin: a
// job that grew 4G in the last interval is charged its peak plus 4G, so the
// NEXT interval's growth of the same size is already budgeted. It also pins
// that the FIRST sample contributes no growth at all -- without havePrevRSS the
// first sample would read as growth == rss and double every cold charge.
func TestDynamicChargeMarginAbsorbsObservedGrowth(t *testing.T) {
	now := time.Unix(40_000, 0)
	const reserve = 32 * gib
	rss := int64(1 * gib)
	server := dynamicChargeServer(&now, func(string) (runner.ConfineListResult, error) {
		return runner.ConfineListResult{Verdict: "pass", Scopes: []runner.ConfineRecord{
			chargeScanRecord("CONFINE-growth-1-a", rss, reserve),
		}}, nil
	})
	waiter := grantedChargeWaiter("CONFINE-growth-1-a", reserve, now.Add(-time.Hour))
	queue := &sliceQueue{path: "/slice", server: server, waiters: []*admitWaiter{waiter}, outstanding: reserve, outstandingJobs: 1}

	server.evaluateAdmitQueue(queue)
	firstWant := rss + expectedMargin(server, rss, 0)
	if waiter.ledgerCharge() != firstWant {
		t.Fatalf("first sample charge = %d, want exactly %d (growth must be 0 on the first sample, not %d)",
			waiter.ledgerCharge(), firstWant, rss)
	}

	rss = 5 * gib
	now = now.Add(time.Second)
	queue.adoptedAt = time.Time{}
	server.evaluateAdmitQueue(queue)

	want := rss + expectedMargin(server, rss, 4*gib)
	if want != 9*gib {
		t.Fatalf("test arithmetic: growth must dominate the margin, got margin %d", want-rss)
	}
	if waiter.ledgerCharge() != want {
		t.Fatalf("after a 4G growth the charge = %d, want exactly %d", waiter.ledgerCharge(), want)
	}
}

// TestDynamicChargeNeverOscillates is the only test that fails against the
// un-ratcheted formula, which is a correct-looking implementation. Without the
// tracked ratchet, growth enters the margin on the growing scan and leaves it
// on the next flat one, so a bursty job swings the SHARED ledger by gigabytes
// every second and neighbours are admitted precisely during the lulls.
func TestDynamicChargeNeverOscillates(t *testing.T) {
	now := time.Unix(50_000, 0)
	const reserve = 32 * gib
	rss := int64(1 * gib)
	server := dynamicChargeServer(&now, func(string) (runner.ConfineListResult, error) {
		return runner.ConfineListResult{Verdict: "pass", Scopes: []runner.ConfineRecord{
			chargeScanRecord("CONFINE-burst-1-a", rss, reserve),
		}}, nil
	})
	waiter := grantedChargeWaiter("CONFINE-burst-1-a", reserve, now.Add(-time.Hour))
	queue := &sliceQueue{path: "/slice", server: server, waiters: []*admitWaiter{waiter}, outstanding: reserve, outstandingJobs: 1}

	server.evaluateAdmitQueue(queue)
	rss = 5 * gib
	now = now.Add(time.Second)
	queue.adoptedAt = time.Time{}
	server.evaluateAdmitQueue(queue)
	burstCharge := waiter.ledgerCharge()
	if burstCharge != 9*gib {
		t.Fatalf("burst charge = %d, want 9G", burstCharge)
	}

	// A flat scan: the raw formula would drop the margin from 4G to 600M.
	now = now.Add(time.Second)
	queue.adoptedAt = time.Time{}
	server.evaluateAdmitQueue(queue)
	if waiter.ledgerCharge() != burstCharge {
		t.Fatalf("flat scan moved the charge %d -> %d; the tracked ratchet must hold it", burstCharge, waiter.ledgerCharge())
	}

	// And a fall in usage must not move it either.
	rss = 2 * gib
	now = now.Add(time.Second)
	queue.adoptedAt = time.Time{}
	server.evaluateAdmitQueue(queue)
	if waiter.ledgerCharge() != burstCharge {
		t.Fatalf("falling usage moved the charge %d -> %d", burstCharge, waiter.ledgerCharge())
	}
	if queue.outstanding != burstCharge {
		t.Fatalf("outstanding %d != charge %d", queue.outstanding, burstCharge)
	}
}

// TestDynamicChargeConservesTheLedger walks grant -> scan -> scan -> release
// across a mixed population and asserts the ledger returns to EXACTLY zero,
// never goes negative, and that a scope-less waiter is never dynamically
// replaced.
func TestDynamicChargeConservesTheLedger(t *testing.T) {
	now := time.Unix(60_000, 0)
	const reserve = 16 * gib
	rss := int64(2 * gib)
	server := dynamicChargeServer(&now, func(string) (runner.ConfineListResult, error) {
		return runner.ConfineListResult{Verdict: "pass", Scopes: []runner.ConfineRecord{
			chargeScanRecord("CONFINE-a-1-a", rss, reserve),
			chargeScanRecord("CONFINE-b-2-b", rss, reserve),
		}}, nil
	})
	scoped := grantedChargeWaiter("CONFINE-a-1-a", reserve, now.Add(-time.Hour))
	scoped.seq = 1
	other := grantedChargeWaiter("CONFINE-b-2-b", reserve, now.Add(-time.Hour))
	other.seq = 2
	// A plain reservation: no scope id, so nothing may ever replace its charge.
	reservation := &admitWaiter{seq: 3, reserve: 3 * gib, state: admitGranted, accounted: true}
	queue := &sliceQueue{
		path: "/slice", server: server,
		waiters:     []*admitWaiter{scoped, other, reservation},
		outstanding: reserve + reserve + 3*gib, outstandingJobs: 3,
	}

	server.evaluateAdmitQueue(queue)
	if queue.outstanding < 0 {
		t.Fatalf("outstanding went negative: %d", queue.outstanding)
	}
	if reservation.ledgerCharge() != 3*gib {
		t.Fatalf("a scope-less waiter was dynamically replaced: %d", reservation.ledgerCharge())
	}
	if sum := scoped.ledgerCharge() + other.ledgerCharge() + reservation.ledgerCharge(); queue.outstanding != sum {
		t.Fatalf("outstanding %d != sum of effective charges %d", queue.outstanding, sum)
	}

	// Release ONE between scans: it must discharge its CURRENT effective value,
	// not the frozen reserve.
	beforeRelease := queue.outstanding
	releasedCharge := scoped.ledgerCharge()
	queue.mu.Lock()
	releaseAdmitWaiterLocked(queue, scoped)
	queue.mu.Unlock()
	if want := beforeRelease - releasedCharge; queue.outstanding != want {
		t.Fatalf("release discharged %d, want the effective charge %d (outstanding %d, want %d)",
			beforeRelease-queue.outstanding, releasedCharge, queue.outstanding, want)
	}

	// A further scan, then release the rest.
	rss = 4 * gib
	now = now.Add(time.Second)
	queue.adoptedAt = time.Time{}
	server.evaluateAdmitQueue(queue)
	if queue.outstanding < 0 {
		t.Fatalf("outstanding went negative after the second scan: %d", queue.outstanding)
	}
	queue.mu.Lock()
	releaseAdmitWaiterLocked(queue, other)
	releaseAdmitWaiterLocked(queue, reservation)
	queue.mu.Unlock()

	if queue.outstanding != 0 {
		t.Fatalf("outstanding = %d after every release, want exactly 0", queue.outstanding)
	}
	if queue.outstandingJobs != 0 {
		t.Fatalf("outstandingJobs = %d after every release, want 0", queue.outstandingJobs)
	}
}

// TestDynamicChargeKeepsTheReportedSplitConsistent is the false-pass guard for
// forgetting to move admitSliceSnapshotFor's sums along with the ledger.
// residualBytes() alone is NOT sufficient: it does not involve vanishedBytes,
// so a build that left vanishedBytes summing the frozen reserve would report a
// clean residual while `confine --list` printed a fabricated vanished total.
func TestDynamicChargeKeepsTheReportedSplitConsistent(t *testing.T) {
	now := time.Unix(70_000, 0)
	const reserve = 16 * gib
	const rss = 2 * gib
	scopes := []runner.ConfineRecord{chargeScanRecord("CONFINE-live-1-a", rss, reserve)}
	server := dynamicChargeServer(&now, func(string) (runner.ConfineListResult, error) {
		return runner.ConfineListResult{Verdict: "pass", Scopes: scopes}, nil
	})
	waiter := grantedChargeWaiter("CONFINE-live-1-a", reserve, now.Add(-time.Hour))
	reservation := &admitWaiter{seq: 2, reserve: gib, state: admitGranted, accounted: true}
	queue := &sliceQueue{
		path: "/slice", server: server, waiters: []*admitWaiter{waiter, reservation},
		outstanding: reserve + gib, outstandingJobs: 2,
	}
	server.admitRegistryMu.Lock()
	server.admitQueues["/slice"] = queue
	server.admitRegistryMu.Unlock()

	server.evaluateAdmitQueue(queue)

	snapshot := server.admitSliceSnapshot("/slice")
	if snapshot.residualBytes() != 0 {
		t.Fatalf("residualBytes = %d, want 0 (outstanding %d, scopeBytes %d, reservationBytes %d)",
			snapshot.residualBytes(), snapshot.outstanding, snapshot.scopeBytes, snapshot.reservationBytes)
	}
	if snapshot.residualJobs() != 0 {
		t.Fatalf("residualJobs = %d, want 0", snapshot.residualJobs())
	}
	if snapshot.scopeBytes != waiter.ledgerCharge() {
		t.Fatalf("scopeBytes = %d, want the effective charge %d", snapshot.scopeBytes, waiter.ledgerCharge())
	}

	// Now make the scope vanish. vanishedBytes is a SUBSET of scopeBytes and must
	// report the same effective charge, which residualBytes cannot detect.
	scopes = nil
	now = now.Add(time.Second)
	queue.adoptedAt = time.Time{}
	server.evaluateAdmitQueue(queue)
	if !waiter.scopeVanished {
		t.Fatal("the scope did not register as vanished; the rest of this test proves nothing")
	}
	snapshot = server.admitSliceSnapshot("/slice")
	if snapshot.vanishedBytes != waiter.ledgerCharge() {
		t.Fatalf("vanishedBytes = %d, want the effective charge %d", snapshot.vanishedBytes, waiter.ledgerCharge())
	}
	if snapshot.residualBytes() != 0 {
		t.Fatalf("residualBytes = %d after the scope vanished, want 0", snapshot.residualBytes())
	}
}

// TestDynamicChargeOnlyMovesOnAUsableRecord is the fail-closed rule. Five of the
// six cases must leave the charge byte-identical; the two that are readable must
// update. The Populated == 0 case is the anti-regression assertion: gating the
// held-waiter charge on the LEAF cgroup.procs count would drop every busy aitest
// outer scope out of the ledger, which is the under-charge direction.
func TestDynamicChargeOnlyMovesOnAUsableRecord(t *testing.T) {
	const reserve = 16 * gib
	const rss = 2 * gib

	nilRSS := func() runner.ConfineRecord {
		record := chargeScanRecord("CONFINE-x-1-a", rss, reserve)
		record.RSSBytes = nil
		return record
	}
	nilCap := func() runner.ConfineRecord {
		record := chargeScanRecord("CONFINE-x-1-a", rss, reserve)
		record.Cap = nil
		return record
	}
	maxCap := func() runner.ConfineRecord {
		record := chargeScanRecord("CONFINE-x-1-a", rss, reserve)
		unlimited := "max"
		record.Cap = &unlimited
		return record
	}
	unpopulated := func() runner.ConfineRecord {
		record := chargeScanRecord("CONFINE-x-1-a", rss, reserve)
		zero := 0
		record.Populated = &zero
		return record
	}
	otherScope := func() runner.ConfineRecord {
		return chargeScanRecord("CONFINE-someone-else-2-b", rss, reserve)
	}

	cases := []struct {
		name       string
		record     func() runner.ConfineRecord
		scanErr    bool
		mustUpdate bool
	}{
		{name: "no record for this scope (grant before create)", record: otherScope},
		{name: "memory.current unevaluated", record: nilRSS},
		{name: "scan failed", record: otherScope, scanErr: true},
		{name: "memory.max unevaluated", record: nilCap},
		{name: "memory.max is the literal max", record: maxCap, mustUpdate: true},
		{name: "leaf cgroup.procs reads empty", record: unpopulated, mustUpdate: true},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			now := time.Unix(80_000, 0)
			server := dynamicChargeServer(&now, func(string) (runner.ConfineListResult, error) {
				if testCase.scanErr {
					return runner.ConfineListResult{}, errors.New("scan unavailable")
				}
				return runner.ConfineListResult{Verdict: "pass", Scopes: []runner.ConfineRecord{testCase.record()}}, nil
			})
			waiter := grantedChargeWaiter("CONFINE-x-1-a", reserve, now.Add(-time.Hour))
			queue := &sliceQueue{path: "/slice", server: server, waiters: []*admitWaiter{waiter}, outstanding: reserve, outstandingJobs: 1}

			server.evaluateAdmitQueue(queue)

			if testCase.mustUpdate {
				want := rss + expectedMargin(server, rss, 0)
				if waiter.ledgerCharge() != want {
					t.Fatalf("charge = %d, want exactly %d -- this record IS usable and must move the charge", waiter.ledgerCharge(), want)
				}
				return
			}
			if waiter.ledgerCharge() != reserve {
				t.Fatalf("charge = %d, want the held %d -- an unusable record must never move the ledger", waiter.ledgerCharge(), reserve)
			}
			if queue.outstanding != reserve {
				t.Fatalf("outstanding = %d, want the held %d", queue.outstanding, reserve)
			}
		})
	}
}

// TestDynamicChargeClampsToTheScopeCap pins that memory.current transiently
// exceeding memory.max charges the cap, never the reading, and never a negative.
func TestDynamicChargeClampsToTheScopeCap(t *testing.T) {
	now := time.Unix(90_000, 0)
	const capBytes = 4 * gib
	server := dynamicChargeServer(&now, func(string) (runner.ConfineListResult, error) {
		return runner.ConfineListResult{Verdict: "pass", Scopes: []runner.ConfineRecord{
			chargeScanRecord("CONFINE-over-1-a", capBytes+512*mib, capBytes),
		}}, nil
	})
	waiter := grantedChargeWaiter("CONFINE-over-1-a", capBytes, now.Add(-time.Hour))
	queue := &sliceQueue{path: "/slice", server: server, waiters: []*admitWaiter{waiter}, outstanding: capBytes, outstandingJobs: 1}

	server.evaluateAdmitQueue(queue)

	if waiter.ledgerCharge() != capBytes {
		t.Fatalf("charge = %d, want exactly the cap %d", waiter.ledgerCharge(), capBytes)
	}
	if queue.outstanding != capBytes {
		t.Fatalf("outstanding = %d, want %d", queue.outstanding, capBytes)
	}
}

// TestDynamicChargeClampsTheColdFloorToTheScopeCap covers the one path where
// the cap clamp is load-bearing and the tracked value alone would not reach it:
// the COLD FLOOR is the resolved reserve, and a scope's enforced memory.max can
// be slightly BELOW that reserve because the kernel floors it to a page
// multiple (writeScopeMemoryCap/floorMemoryPage). Charging a job more than it
// is physically able to allocate is an over-charge of exactly the kind this
// ticket exists to remove, even at one page.
//
// Found by mutation: without this, deleting the post-cold-floor cap clamp left
// every other test passing, because `tracked` is clamped before the ratchet and
// only the cold floor can push a charge above the cap.
func TestDynamicChargeClampsTheColdFloorToTheScopeCap(t *testing.T) {
	now := time.Unix(95_000, 0)
	const reserve = 4*gib + 4095 // an unaligned resolved estimate
	const capBytes = 4 * gib     // what the kernel actually enforced, page-floored
	server := dynamicChargeServer(&now, func(string) (runner.ConfineListResult, error) {
		return runner.ConfineListResult{Verdict: "pass", Scopes: []runner.ConfineRecord{
			chargeScanRecord("CONFINE-floored-1-a", 8*mib, capBytes),
		}}, nil
	})
	// Inside the cold-floor window, so the floor (the reserve) is what would win.
	waiter := grantedChargeWaiter("CONFINE-floored-1-a", reserve, now.Add(-time.Second))
	queue := &sliceQueue{path: "/slice", server: server, waiters: []*admitWaiter{waiter}, outstanding: reserve, outstandingJobs: 1}

	server.evaluateAdmitQueue(queue)

	if waiter.ledgerCharge() != capBytes {
		t.Fatalf("charge = %d, want exactly the enforced cap %d -- the cold floor must not charge above what the scope can allocate",
			waiter.ledgerCharge(), capBytes)
	}
	if queue.outstanding != capBytes {
		t.Fatalf("outstanding = %d, want %d", queue.outstanding, capBytes)
	}
}

// TestApplyChargeDeltaRefusesRatherThanSaturating pins the conservation
// property at the int64 boundary. A SATURATING implementation passes a
// single-waiter test and only reveals itself once a second waiter releases and
// drives the ledger negative, so both are asserted here.
func TestApplyChargeDeltaRefusesRatherThanSaturating(t *testing.T) {
	if next, ok := applyChargeDelta(100, 40, 60); !ok || next != 120 {
		t.Fatalf("ordinary increase = (%d, %v), want (120, true)", next, ok)
	}
	if next, ok := applyChargeDelta(100, 60, 40); !ok || next != 80 {
		t.Fatalf("ordinary decrease = (%d, %v), want (80, true)", next, ok)
	}
	if next, ok := applyChargeDelta(100, 40, math.MaxInt64); ok || next != 100 {
		t.Fatalf("overflowing increase = (%d, %v), want (100, false) -- it must REFUSE, not saturate", next, ok)
	}
	if next, ok := applyChargeDelta(100, -1, 40); ok || next != 100 {
		t.Fatalf("negative input = (%d, %v), want (100, false)", next, ok)
	}

	// The two-waiter lifecycle a saturating build would fail.
	outstanding := int64(0)
	first, second := int64(60), int64(40)
	outstanding += first
	outstanding += second
	next, ok := applyChargeDelta(outstanding, second, math.MaxInt64)
	if ok {
		t.Fatalf("an overflowing move was accepted; outstanding would become %d", next)
	}
	outstanding = next
	outstanding -= first
	outstanding -= second
	if outstanding != 0 {
		t.Fatalf("outstanding = %d after releasing both waiters, want exactly 0", outstanding)
	}
}

func TestPctClampNeverOverflows(t *testing.T) {
	if got := pctClamp(1000, 12); got != 120 {
		t.Fatalf("pctClamp(1000, 12) = %d, want 120", got)
	}
	// The overflow branch must still be ARITHMETIC, not merely non-negative. An
	// earlier version returned MaxInt64/100*pct here -- non-negative, and about
	// 12x LARGER than the true answer. "got >= 0" passed against it.
	huge := pctClamp(math.MaxInt64, 12)
	if huge <= 0 {
		t.Fatalf("pctClamp(MaxInt64, 12) = %d, want a positive result", huge)
	}
	if huge > math.MaxInt64/100*12+100 {
		t.Fatalf("pctClamp(MaxInt64, 12) = %d, far above 12%% of the input; the overflow branch is not computing a percentage", huge)
	}
	if huge > math.MaxInt64/8 {
		t.Fatalf("pctClamp(MaxInt64, 12) = %d, above an eighth of the input; 12%% cannot exceed that", huge)
	}
	// A percentage of at most 100 can never exceed the value it is taken from,
	// on either branch.
	for _, value := range []int64{1000, 1 << 40, math.MaxInt64 / 3, math.MaxInt64} {
		for _, pct := range []int64{1, 12, 100, 250} {
			if got := pctClamp(value, pct); got > value {
				t.Fatalf("pctClamp(%d, %d) = %d, above the value it is a percentage of", value, pct, got)
			}
		}
	}
	// The overflow branch must agree with the exact answer to within its stated
	// error, a remainder smaller than pct. Computed here in big-integer-free form
	// by splitting the multiply, so the assertion does not simply restate the
	// implementation. Broad bounds alone leave a materially wrong overflow branch
	// undetected -- which is how a 12x-too-large version survived a first pass.
	for _, pct := range []int64{1, 12, 100} {
		for _, value := range []int64{math.MaxInt64, math.MaxInt64 - 1, math.MaxInt64/pct + 1} {
			if value <= 0 || value <= math.MaxInt64/pct {
				continue // not the overflow branch
			}
			exact := value/100*pct + (value%100)*pct/100
			got := pctClamp(value, pct)
			if diff := exact - got; diff < 0 || diff >= pct {
				t.Fatalf("pctClamp(%d, %d) = %d, exact %d, off by %d -- the stated error bound is a remainder below pct",
					value, pct, got, exact, diff)
			}
		}
	}
	if got := pctClamp(-5, 12); got != 0 {
		t.Fatalf("pctClamp(-5, 12) = %d, want 0", got)
	}
	if got := pctClamp(1000, 0); got != 0 {
		t.Fatalf("pctClamp(1000, 0) = %d, want 0", got)
	}
}

// TestAdoptionTracksActualForNonDelegateOrphans is the post-restart half. Without
// it the headline win regresses on EVERY daemon restart: a non-delegate orphan
// re-pins its full estimate until it exits.
func TestAdoptionTracksActualForNonDelegateOrphans(t *testing.T) {
	const capBytes = 26 * gib
	const rss = 20 * gib
	warm := int64(3600)
	young := int64(5)

	record := func(mutate func(*runner.ConfineRecord)) runner.ConfineRecord {
		value := chargeScanRecord("CONFINE-orphan-1-a", rss, capBytes)
		value.AgeSeconds = &warm
		if mutate != nil {
			mutate(&value)
		}
		return value
	}

	cases := []struct {
		name   string
		record runner.ConfineRecord
		want   int64
	}{
		{name: "warm with usable rss tracks actual", record: record(nil), want: rss + 12*rss/100},
		{name: "young adopts the full cap", record: record(func(r *runner.ConfineRecord) { r.AgeSeconds = &young }), want: capBytes},
		{name: "unknown age adopts the full cap", record: record(func(r *runner.ConfineRecord) { r.AgeSeconds = nil }), want: capBytes},
		{name: "unreadable rss adopts the full cap", record: record(func(r *runner.ConfineRecord) { r.RSSBytes = nil }), want: capBytes},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			now := time.Unix(100_000, 0)
			server := dynamicChargeServer(&now, func(string) (runner.ConfineListResult, error) {
				return runner.ConfineListResult{Verdict: "pass", Scopes: []runner.ConfineRecord{testCase.record}}, nil
			})
			queue := &sliceQueue{path: "/slice", server: server}

			server.evaluateAdmitQueue(queue)

			if queue.adopted != testCase.want {
				t.Fatalf("adopted = %d, want exactly %d", queue.adopted, testCase.want)
			}
			if queue.adoptedJobs != 1 {
				t.Fatalf("adoptedJobs = %d, want 1", queue.adoptedJobs)
			}
		})
	}
}

// TestAdoptionUsesOneMarginPolicyForDelegateOrphans pins that the delegate
// reconstruction path now shares the charge margin instead of its own bare
// 64 MiB constant. The direction is over-charge, which is the safe one.
func TestAdoptionUsesOneMarginPolicyForDelegateOrphans(t *testing.T) {
	now := time.Unix(110_000, 0)
	const capBytes = 48 * gib
	const rss = 20 * gib
	server := dynamicChargeServer(&now, func(string) (runner.ConfineListResult, error) {
		return runner.ConfineListResult{Verdict: "pass", Scopes: []runner.ConfineRecord{
			chargeScanRecord("CONFINE-@dr-suite-1-a", rss, capBytes),
		}}, nil
	})
	if !runner.IsDelegateRAMScopeID("CONFINE-@dr-suite-1-a") {
		t.Skip("the delegate scope-id marker changed; this test needs updating")
	}
	queue := &sliceQueue{path: "/slice", server: server}

	server.evaluateAdmitQueue(queue)

	want := rss + 12*rss/100
	if queue.adopted != want {
		t.Fatalf("adopted = %d, want exactly %d (rss + the shared margin, not rss + 64MiB)", queue.adopted, want)
	}
}

// TestAdoptionSkipsAnUnreconstructableDelegateOrphan pins the one adoption arm
// the other cases do not reach: a delegate scope whose memory.current cannot be
// read has a cap that is a CONTAINMENT CEILING, not an estimate, so adopting it
// would charge a 48G ceiling for a suite that may be using 2G. It contributes
// neither bytes nor a headroom job, exactly as before AIRA-29.
func TestAdoptionSkipsAnUnreconstructableDelegateOrphan(t *testing.T) {
	now := time.Unix(115_000, 0)
	server := dynamicChargeServer(&now, func(string) (runner.ConfineListResult, error) {
		record := chargeScanRecord("CONFINE-@dr-suite-1-a", 0, 48*gib)
		record.RSSBytes = nil
		return runner.ConfineListResult{Verdict: "pass", Scopes: []runner.ConfineRecord{record}}, nil
	})
	queue := &sliceQueue{path: "/slice", server: server}

	server.evaluateAdmitQueue(queue)

	if queue.adopted != 0 || queue.adoptedJobs != 0 {
		t.Fatalf("adopted=%d jobs=%d, want 0/0 -- an unreadable delegate ceiling must never be adopted as a reservation",
			queue.adopted, queue.adoptedJobs)
	}
}

// TestDynamicReserveKillSwitchRestoresTheFrozenBehaviour pins the operational
// rollback. This change accepts a new bounded over-subscription on a slice every
// session on the machine shares, so reverting it must not require rebuilding and
// redeploying a daemon at the moment it is misbehaving.
//
// Both halves are asserted, because a switch that reverted the live charge but
// left the adoption margin changed would be a partial rollback presented as a
// whole one.
func TestDynamicReserveKillSwitchRestoresTheFrozenBehaviour(t *testing.T) {
	now := time.Unix(130_000, 0)
	const reserve = 32 * gib
	const rss = 2 * gib
	const delegateCap = 48 * gib

	server := dynamicChargeServer(&now, func(string) (runner.ConfineListResult, error) {
		return runner.ConfineListResult{Verdict: "pass", Scopes: []runner.ConfineRecord{
			chargeScanRecord("CONFINE-held-1-a", rss, reserve),
			chargeScanRecord("CONFINE-orphan-2-b", rss, reserve),
			chargeScanRecord("CONFINE-@dr-suite-3-c", rss, delegateCap),
		}}, nil
	})
	server.dynamicReserve = false
	waiter := grantedChargeWaiter("CONFINE-held-1-a", reserve, now.Add(-time.Hour))
	queue := &sliceQueue{path: "/slice", server: server, waiters: []*admitWaiter{waiter}, outstanding: reserve, outstandingJobs: 1}

	server.evaluateAdmitQueue(queue)

	if waiter.chargeTracked || waiter.ledgerCharge() != reserve {
		t.Fatalf("with the switch off the held waiter charges %d (tracked=%v), want the frozen reserve %d",
			waiter.ledgerCharge(), waiter.chargeTracked, reserve)
	}
	if queue.outstanding != reserve {
		t.Fatalf("with the switch off outstanding = %d, want %d", queue.outstanding, reserve)
	}
	// The non-delegate orphan adopts its full cap, and the delegate orphan uses
	// the pre-AIRA-29 64 MiB margin -- not the shared charge margin.
	want := reserve + addClamp(rss, delegateRAMAdoptionMargin)
	if queue.adopted != want {
		t.Fatalf("with the switch off adopted = %d, want %d (full cap + rss+64MiB), so the rollback is partial", queue.adopted, want)
	}
}

// TestDynamicReserveFromEnvRefusesRatherThanSubstituting pins the AIRA-58 rule
// on the kill switch. Silently falling back to "enabled" on a typo is the worst
// behaviour available here: the operator who most needs this variable is
// reverting an admission change on a shared machine under load, and they would
// be told nothing while getting the behaviour they were trying to turn off.
func TestDynamicReserveFromEnvRefusesRatherThanSubstituting(t *testing.T) {
	cases := []struct {
		value   string
		set     bool
		want    bool
		wantErr bool
	}{
		{set: false, want: true},
		{value: "", set: true, want: true},
		{value: "enabled", set: true, want: true},
		{value: "1", set: true, want: true},
		{value: "disabled", set: true, want: false},
		{value: "0", set: true, want: false},
		{value: "off", set: true, wantErr: true},
		{value: "true", set: true, wantErr: true},
		{value: "Disabled", set: true, wantErr: true},
	}
	for _, testCase := range cases {
		name := "unset"
		if testCase.set {
			name = "value=" + testCase.value
		}
		t.Run(name, func(t *testing.T) {
			// t.Setenv registers the restore; os.Unsetenv then gives a genuinely
			// ABSENT variable, which is a different LookupEnv result from the
			// empty string and must be tested as such.
			t.Setenv("AIRA_DAEMON_DYNAMIC_RESERVE", "placeholder")
			if testCase.set {
				os.Setenv("AIRA_DAEMON_DYNAMIC_RESERVE", testCase.value)
			} else if err := os.Unsetenv("AIRA_DAEMON_DYNAMIC_RESERVE"); err != nil {
				t.Fatal(err)
			}
			got, err := dynamicReserveFromEnv()
			if testCase.wantErr {
				if err == nil {
					t.Fatalf("value %q was accepted as %v; an unrecognised setting must be REFUSED, not substituted", testCase.value, got)
				}
				if !strings.Contains(err.Error(), "E_CONFIG_INVALID") {
					t.Fatalf("error %q does not carry a stable code", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("value %q refused: %v", testCase.value, err)
			}
			if got != testCase.want {
				t.Fatalf("value %q = %v, want %v", testCase.value, got, testCase.want)
			}
		})
	}
}

// TestDynamicChargeFreesSpaceForAQueuedWaiter is the reason the ticket exists:
// the freed ledger must actually admit somebody. Without this the whole change
// could be arithmetically correct and operationally inert.
func TestDynamicChargeFreesSpaceForAQueuedWaiter(t *testing.T) {
	now := time.Unix(120_000, 0)
	const reserve = 30 * gib
	const rss = 2 * gib
	server := dynamicChargeServer(&now, func(string) (runner.ConfineListResult, error) {
		return runner.ConfineListResult{Verdict: "pass", Scopes: []runner.ConfineRecord{
			chargeScanRecord("CONFINE-hog-1-a", rss, reserve),
		}}, nil
	})
	// A 32G slice: the hog's frozen 30G reserve leaves no room for a 10G waiter.
	server.admitReadMemory = func(string) (int64, int64, int64, bool, string) {
		return rss, 32 * gib, 0, true, ""
	}
	hog := grantedChargeWaiter("CONFINE-hog-1-a", reserve, now.Add(-time.Hour))
	queued := &admitWaiter{seq: 2, reserve: 10 * gib, state: admitQueued, grantedCh: make(chan struct{}), enqueued: now}
	queue := &sliceQueue{
		path: "/slice", server: server, waiters: []*admitWaiter{hog, queued},
		outstanding: reserve, outstandingJobs: 1,
	}

	server.evaluateAdmitQueue(queue)

	if queued.state != admitGranted {
		t.Fatalf("the queued waiter was not admitted into the freed reserve (state=%v, outstanding=%d, hog charge=%d)",
			queued.state, queue.outstanding, hog.ledgerCharge())
	}
	// The GRANT must charge the whole frozen reserve and leave the waiter
	// untracked. Asserting only `outstanding == hog + queued.ledgerCharge()`
	// would pass against a grant that set chargeTracked with a zero charge --
	// which is precisely the grant-before-scope-creation under-charge the
	// untracked state exists to prevent, and it would be invisible because both
	// sides of that equation would be wrong together.
	if queued.chargeTracked {
		t.Fatal("a freshly granted waiter must be UNTRACKED until a usable scan reading replaces its reserve")
	}
	if queued.ledgerCharge() != queued.reserve {
		t.Fatalf("a freshly granted waiter charges %d, want its whole reserve %d", queued.ledgerCharge(), queued.reserve)
	}
	if queue.outstanding != hog.ledgerCharge()+queued.reserve {
		t.Fatalf("outstanding %d != %d + %d", queue.outstanding, hog.ledgerCharge(), queued.reserve)
	}
}

// TestDynamicChargeNotAppliedToAScopeWithSubReservations is the double-book
// guard. A `--delegate-ram` suite's per-test `aira confine-reserve` waiters are
// SCOPE-LESS waiters in this same queue, each charging its own reserve, while
// the parent scope's memory.current is HIERARCHICAL and already contains
// everything they allocated. Charging the parent its live usage as well counts
// the same bytes twice, and the resulting over-charge refuses healthy jobs with
// the slice half free -- the exact failure this ticket exists to remove,
// reintroduced by its own fix.
func TestDynamicChargeNotAppliedToAScopeWithSubReservations(t *testing.T) {
	now := time.Unix(140_000, 0)
	const overhead = 512 * mib // a delegate parent's pinned framework reserve
	const scopeCap = 48 * gib
	const childReserve = 20 * gib
	const parentRSS = 22 * gib // the children's allocations, hierarchically

	server := dynamicChargeServer(&now, func(string) (runner.ConfineListResult, error) {
		return runner.ConfineListResult{Verdict: "pass", Scopes: []runner.ConfineRecord{
			chargeScanRecord("CONFINE-@dr-suite-1-a", parentRSS, scopeCap),
		}}, nil
	})
	parent := grantedChargeWaiter("CONFINE-@dr-suite-1-a", overhead, now.Add(-time.Hour))
	// Deliberately listed BEFORE its parent: a sub-reservation may appear
	// anywhere in the waiter list, so a single-pass implementation that only
	// noticed children already seen would pass a parent-first ordering.
	child := &admitWaiter{
		seq: 2, reserve: childReserve, state: admitGranted, accounted: true,
		parentScopeID: "CONFINE-@dr-suite-1-a",
	}
	queue := &sliceQueue{
		path: "/slice", server: server, waiters: []*admitWaiter{child, parent},
		outstanding: overhead + childReserve, outstandingJobs: 2,
	}

	server.evaluateAdmitQueue(queue)

	if parent.chargeTracked {
		t.Fatalf("the parent was dynamically charged %d while its children charge %d separately; that double-books the same bytes",
			parent.ledgerCharge(), childReserve)
	}
	if parent.ledgerCharge() != overhead {
		t.Fatalf("parent charge = %d, want its pinned overhead %d", parent.ledgerCharge(), overhead)
	}
	if want := overhead + childReserve; queue.outstanding != want {
		t.Fatalf("outstanding = %d, want %d; the parent's hierarchical usage must not be added on top of its children", queue.outstanding, want)
	}

	// And once the sub-reservation releases, the parent becomes chargeable again
	// -- the exclusion is a property of the CURRENT population, not a latch.
	queue.mu.Lock()
	releaseAdmitWaiterLocked(queue, child)
	queue.mu.Unlock()
	now = now.Add(time.Second)
	queue.adoptedAt = time.Time{}
	server.evaluateAdmitQueue(queue)

	if !parent.chargeTracked {
		t.Fatal("the parent stayed unchargeable after its last sub-reservation released; the exclusion must not latch")
	}
}
