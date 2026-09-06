package daemon

import (
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"aira/internal/runner"
)

// AIRA-114. The aggregate over-subscription bound.
//
// AIRA-29 traded away the one property that made the non-delegate confine class
// airtight: with the ledger charging live usage instead of the frozen estimate,
// Sigma(scope memory.max) may exceed the slice cap without limit. Its own
// residual §4e named the failure — several scopes expanding between scans until
// aira.slice hits its cap, producing a memcg OOM biased only by the class
// steering — and deferred the fix here.
//
// Every test below states the EXACT aggregate rather than a bound wherever it
// can, and names the wrong implementation a looser assertion would have let
// through. Three wrong builds are plausible enough to be pinned explicitly:
//
//   - one that gates on record.Populated (leaf cgroup.procs) and so drops every
//     busy aitest outer scope — the largest caps on the machine — out of the
//     total, leaving a "bound" that does not bind;
//   - one that counts a locally-uncapped scope as zero, which is porous, or as
//     unbounded, which wedges this machine-wide slice;
//   - one that blocks so eagerly that an idle slice can no longer start a job.
//
// verifies: the AIRA-114 aggregate accounting, its subtree-aware liveness, the
// uncapped-scope policy, and the bound's fail-open and never-wedge rules.

// oversubServer is a server whose admission arithmetic is entirely the test's:
// no headroom, a fixed clock, a scan seam, and an explicit bound.
func oversubServer(now *time.Time, sliceMax int64, current int64, factorPct int64, scan func(string) (runner.ConfineListResult, error)) *Server {
	server := NewServer(Paths{})
	server.admitNow = func() time.Time { return *now }
	server.admitConfineScanInterval = time.Nanosecond
	server.admitConfineScan = scan
	server.admitSliceHeadroomBase = 0
	server.admitSliceHeadroomSupervisor = 0
	server.oversubscriptionFactorPct = factorPct
	server.admitReadMemory = func(string) (int64, int64, int64, bool, string) {
		return current, sliceMax, 0, true, ""
	}
	return server
}

func staticScan(scopes ...runner.ConfineRecord) func(string) (runner.ConfineListResult, error) {
	return func(string) (runner.ConfineListResult, error) {
		return runner.ConfineListResult{Verdict: "pass", Scopes: scopes}, nil
	}
}

// oversubRecord is a scan record shaped as a live scope reads: subtree-live,
// a usable memory.current, and a finite memory.max.
func oversubRecord(scopeID string, rss, capBytes int64) runner.ConfineRecord {
	populated, live, age := 1, true, int64(3600)
	capText := formatInt64(capBytes)
	return runner.ConfineRecord{
		ScopeID: scopeID, Populated: &populated, SubtreePopulated: &live,
		RSSBytes: &rss, Cap: &capText, AgeSeconds: &age,
	}
}

// leafDrainedRecord is the trap AIRA-29 §3.5 named: a busy aitest outer scope
// whose pids have all been drained into a child cgroup, so LEAF cgroup.procs
// reads zero while the kernel's subtree signal says it is very much alive.
func leafDrainedRecord(scopeID string, rss, capBytes int64) runner.ConfineRecord {
	record := oversubRecord(scopeID, rss, capBytes)
	zero := 0
	record.Populated = &zero
	return record
}

func uncappedRecord(scopeID string, rss int64) runner.ConfineRecord {
	record := oversubRecord(scopeID, rss, 0)
	unlimited := "max"
	record.Cap = &unlimited
	return record
}

func queuedScopeWaiter(seq int64, scopeID string, reserve int64, now time.Time) *admitWaiter {
	return &admitWaiter{
		seq: seq, reserve: reserve, state: admitQueued, scopeID: scopeID,
		grantedCh: make(chan struct{}), enqueued: now,
	}
}

func heldScopeWaiter(seq int64, scopeID string, reserve int64, grantedAt time.Time) *admitWaiter {
	return &admitWaiter{
		seq: seq, reserve: reserve, state: admitGranted, accounted: true,
		scopeID: scopeID, grantedAt: grantedAt,
	}
}

// TestAggregateBoundRefusesTheJobThatWouldBreachIt is the headline: the exact
// case AIRA-29 §4e recorded and deferred. Six scopes hold 20 GiB caps while
// using 1 GiB each, so the dynamic ledger has plenty of room and the reserve
// check would admit a seventh — after which the caps handed out total 140 GiB
// against a 64 GiB slice.
//
// Three arms, because one alone is porous in a different direction each time:
//
//	bounded    the seventh is REFUSED and the aggregate stays at 120 GiB;
//	unbounded  with the bound off it is admitted and the aggregate reaches
//	           140 GiB, 2.19x the slice — proving the scenario really does drive
//	           past the old state rather than merely looking as if it might;
//	smaller    a 4 GiB newcomer under the SAME bound IS admitted, which kills an
//	           implementation that simply refuses everything once scopes exist.
func TestAggregateBoundRefusesTheJobThatWouldBreachIt(t *testing.T) {
	const (
		sliceMax  = 64 * gib
		scopeCap  = 20 * gib
		scopeRSS  = 1 * gib
		heldCount = 6
		aggregate = heldCount * scopeCap // 120 GiB
		limit     = 2 * sliceMax         // factor 200%
	)

	build := func(t *testing.T, factorPct, newcomerReserve int64) (*Server, *sliceQueue, *admitWaiter) {
		t.Helper()
		now := time.Unix(200_000, 0)
		records := make([]runner.ConfineRecord, 0, heldCount)
		waiters := make([]*admitWaiter, 0, heldCount+1)
		for index := 0; index < heldCount; index++ {
			scopeID := "CONFINE-hog" + formatInt64(int64(index)) + "-1-a"
			records = append(records, oversubRecord(scopeID, scopeRSS, scopeCap))
			waiters = append(waiters, heldScopeWaiter(int64(index+1), scopeID, scopeCap, now.Add(-time.Hour)))
		}
		newcomer := queuedScopeWaiter(heldCount+1, "CONFINE-newcomer-1-a", newcomerReserve, now)
		waiters = append(waiters, newcomer)
		server := oversubServer(&now, sliceMax, heldCount*scopeRSS, factorPct, staticScan(records...))
		queue := &sliceQueue{
			path: "/slice", server: server, waiters: waiters,
			outstanding: heldCount * scopeCap, outstandingJobs: heldCount,
		}
		return server, queue, newcomer
	}

	t.Run("bounded", func(t *testing.T) {
		server, queue, newcomer := build(t, 200, 20*gib)
		server.evaluateAdmitQueue(queue)

		// The ledger must genuinely have had room, or this would be a test of the
		// pre-existing reserve check wearing a new name.
		available := checkedAvailable(heldCount*scopeRSS, sliceMax, 0, queue.outstanding, 0)
		if available < newcomer.reserve {
			t.Fatalf("test arithmetic: only %d bytes available, so the %d-byte newcomer was refused by the RESERVE check, not by the aggregate bound",
				available, newcomer.reserve)
		}
		if queue.capAggregate != aggregate {
			t.Fatalf("cap aggregate = %d, want exactly %d (%d scopes x %d)", queue.capAggregate, int64(aggregate), heldCount, int64(scopeCap))
		}
		if !queue.capAggregateKnown {
			t.Fatal("the aggregate was derived from a successful scan of fully readable scopes and must be established")
		}
		if newcomer.state != admitQueued {
			t.Fatalf("the newcomer was admitted (state=%v); admitting it takes the caps to %d against a %d limit",
				newcomer.state, int64(aggregate+20*gib), int64(limit))
		}
		if !newcomer.waited {
			t.Fatal("a waiter held back by the aggregate bound must be marked as having waited")
		}
	})

	t.Run("unbounded is what this replaces", func(t *testing.T) {
		server, queue, newcomer := build(t, 0, 20*gib)
		server.evaluateAdmitQueue(queue)

		if newcomer.state != admitGranted {
			t.Fatalf("with the bound disabled the newcomer must be admitted exactly as AIRA-29 leaves it (state=%v)", newcomer.state)
		}
		// The number this ticket exists to bound, stated rather than implied.
		if want := int64(aggregate + 20*gib); want <= sliceMax {
			t.Fatalf("test arithmetic: the unbounded aggregate %d does not exceed the %d slice, so it demonstrates nothing", want, int64(sliceMax))
		}
	})

	t.Run("a smaller newcomer still fits", func(t *testing.T) {
		server, queue, newcomer := build(t, 200, 4*gib)
		server.evaluateAdmitQueue(queue)

		if newcomer.state != admitGranted {
			t.Fatalf("a 4 GiB newcomer takes the caps to %d, under the %d limit, and must be admitted (state=%v)",
				int64(aggregate+4*gib), int64(limit), newcomer.state)
		}
		if want := int64(aggregate + 4*gib); queue.capAggregate != want {
			t.Fatalf("cap aggregate after the grant = %d, want exactly %d", queue.capAggregate, want)
		}
	})
}

// TestAggregateCountsALeafDrainedScope is the anti-regression test for the
// exact trap that made v4's version unbuildable: the adoption loop's
// record.Populated gate is LEAF cgroup.procs, and every busy aitest outer scope
// reads zero there. A bound derived that way silently omits the biggest caps on
// the machine and never binds.
//
// Against a leaf-gated implementation the aggregate is 0, the never-wedge
// clause exempts the waiter, and it is admitted. This is the only test here
// that fails against that build.
func TestAggregateCountsALeafDrainedScope(t *testing.T) {
	const (
		sliceMax = 64 * gib
		suiteCap = 100 * gib
		newcomer = 40 * gib
	)
	now := time.Unix(210_000, 0)
	server := oversubServer(&now, sliceMax, 2*gib, 200,
		staticScan(leafDrainedRecord("CONFINE-@dr-suite-1-a", 2*gib, suiteCap)))
	waiter := queuedScopeWaiter(1, "CONFINE-newcomer-1-a", newcomer, now)
	queue := &sliceQueue{path: "/slice", server: server, waiters: []*admitWaiter{waiter}}

	server.evaluateAdmitQueue(queue)

	if queue.capAggregate != suiteCap {
		t.Fatalf("cap aggregate = %d, want exactly %d: a leaf-empty but SUBTREE-live scope must be counted", queue.capAggregate, int64(suiteCap))
	}
	if waiter.state != admitQueued {
		t.Fatalf("the newcomer was admitted (state=%v); %d + %d exceeds the %d limit",
			waiter.state, int64(suiteCap), int64(newcomer), int64(2*sliceMax))
	}
}

// TestAggregateSkipsAProvablyEmptyScope is the other half: liveness is a real
// reading, not a formality. A scope the kernel says holds nothing contributes
// nothing, so a departing job's cap stops blocking its successor immediately
// rather than at the next reap.
func TestAggregateSkipsAProvablyEmptyScope(t *testing.T) {
	const sliceMax = 64 * gib
	now := time.Unix(220_000, 0)
	empty := leafDrainedRecord("CONFINE-done-1-a", 0, 100*gib)
	dead := false
	empty.SubtreePopulated = &dead
	server := oversubServer(&now, sliceMax, 0, 200, staticScan(empty))
	waiter := queuedScopeWaiter(1, "CONFINE-newcomer-1-a", 40*gib, now)
	queue := &sliceQueue{path: "/slice", server: server, waiters: []*admitWaiter{waiter}}

	server.evaluateAdmitQueue(queue)

	if waiter.state != admitGranted {
		t.Fatalf("a scope the kernel reports EMPTY must not hold capacity against a newcomer (state=%v, aggregate=%d)", waiter.state, queue.capAggregate)
	}
}

// TestUncappedScopeContributesItsMeasuredUsage is the policy AIRA-29 §3.5 said
// did not exist: a locally-uncapped scope (the flock-fallback population, which
// launches when the daemon is unavailable and so never had a cap chosen for it)
// counted at neither zero nor infinity.
//
// Both halves are required. The first proves it is not counted as ZERO, which
// would make the bound porous against exactly the population AIRA does not
// control. The second proves it is not counted as UNBOUNDED, which would wedge
// this machine-wide slice on a job the daemon never admitted.
func TestUncappedScopeContributesItsMeasuredUsage(t *testing.T) {
	const sliceMax = 64 * gib
	now := time.Unix(230_000, 0)

	t.Run("not zero", func(t *testing.T) {
		server := oversubServer(&now, sliceMax, 40*gib, 200, staticScan(uncappedRecord("CONFINE-fallback-1-a", 100*gib)))
		waiter := queuedScopeWaiter(1, "CONFINE-newcomer-1-a", 40*gib, now)
		queue := &sliceQueue{path: "/slice", server: server, waiters: []*admitWaiter{waiter}}

		server.evaluateAdmitQueue(queue)

		if queue.capAggregate != 100*gib {
			t.Fatalf("cap aggregate = %d, want exactly the uncapped scope's measured usage %d", queue.capAggregate, int64(100*gib))
		}
		if waiter.state != admitQueued {
			t.Fatalf("an uncapped scope using %d must not be counted as zero (state=%v)", int64(100*gib), waiter.state)
		}
	})

	t.Run("not unbounded", func(t *testing.T) {
		server := oversubServer(&now, sliceMax, gib, 200, staticScan(uncappedRecord("CONFINE-fallback-1-a", gib)))
		waiter := queuedScopeWaiter(1, "CONFINE-newcomer-1-a", 40*gib, now)
		queue := &sliceQueue{path: "/slice", server: server, waiters: []*admitWaiter{waiter}}

		server.evaluateAdmitQueue(queue)

		if waiter.state != admitGranted {
			t.Fatalf("a 1 GiB uncapped scope must not block a 40 GiB newcomer under a %d limit (state=%v, aggregate=%d)",
				int64(2*sliceMax), waiter.state, queue.capAggregate)
		}
		// Stated exactly, after the grant added the newcomer's own 40 GiB: the
		// uncapped scope contributed its 1 GiB of measured usage and nothing more.
		if want := int64(gib + 40*gib); queue.capAggregate != want {
			t.Fatalf("cap aggregate = %d, want exactly %d: an uncapped scope is its measured usage, never an infinity", queue.capAggregate, want)
		}
	})
}

// TestAggregateUnestablishedWithholdsNothing pins the fail-OPEN rule. A live
// scope whose memory.max AND memory.current are both unreadable leaves the
// total unstatable, and the bound must then withhold nothing rather than stall
// every job on a machine-wide slice because of a diagnostic failure.
//
// The asserted pair matters: the aggregate must be reported UNESTABLISHED, not
// merely small. An implementation that silently treated the unreadable scope as
// zero would admit the waiter too, but would then go on claiming a bound it was
// not enforcing.
func TestAggregateUnestablishedWithholdsNothing(t *testing.T) {
	const sliceMax = 64 * gib
	now := time.Unix(240_000, 0)
	live := true
	opaque := runner.ConfineRecord{ScopeID: "CONFINE-opaque-1-a", SubtreePopulated: &live}
	server := oversubServer(&now, sliceMax, 2*gib, 200,
		staticScan(oversubRecord("CONFINE-hog-1-a", gib, 100*gib), opaque))
	waiter := queuedScopeWaiter(1, "CONFINE-newcomer-1-a", 40*gib, now)
	queue := &sliceQueue{path: "/slice", server: server, waiters: []*admitWaiter{waiter}}
	registerAdmitQueue(server, queue)

	server.evaluateAdmitQueue(queue)

	if queue.capAggregateKnown {
		t.Fatal("a live scope with neither a readable memory.max nor a readable memory.current leaves the aggregate UNESTABLISHED")
	}
	if waiter.state != admitGranted {
		t.Fatalf("the bound must withhold nothing while the aggregate is unestablished (state=%v)", waiter.state)
	}
	snapshot := server.admitSliceSnapshot("/slice")
	// `present` first, and it is the load-bearing half of this assertion. An
	// ABSENT queue returns admitSnapshot{phase} — capAggregateKnown false by
	// construction — so without this line the claim below would hold against
	// every possible implementation, including one that never copied the bit at
	// all. It is asserted here, and the established value is asserted in
	// TestSnapshotCarriesTheEstablishedAggregate, because a false bit alone
	// cannot distinguish "copied and false" from "never copied".
	if !snapshot.present {
		t.Fatal("test wiring: the queue must be REGISTERED, or the snapshot's unestablished bit is an absent-queue zero and proves nothing")
	}
	if snapshot.capAggregateKnown {
		t.Fatal("the snapshot must carry the unestablished bit so no surface renders the total as a fact")
	}
}

// registerAdmitQueue puts a hand-built queue into the server's registry under
// the lock the daemon itself uses, so admitSliceSnapshot resolves it instead of
// returning the absent-queue zero.
func registerAdmitQueue(server *Server, queue *sliceQueue) {
	server.admitRegistryMu.Lock()
	server.admitQueues[queue.path] = queue
	server.admitRegistryMu.Unlock()
}

// TestSnapshotCarriesTheEstablishedAggregate is the POSITIVE arm of the
// snapshot plumbing, and it exists because the unestablished arm above cannot
// be it: `capAggregateKnown == false` is also what a build that never copied
// the field at all would report.
//
// The path under test is queue -> admitSnapshot, which is what every operator
// surface reads (`confine --list` renders it one layer further out, pinned by
// TestConfineListSliceReserveSummary/aggregate-established). Deleting the
// snapshot's `capAggregate/capAggregateKnown` copy leaves `confine --list`
// permanently printing "slice scope caps: unevaluated ... (not applied while
// unevaluated)" WHILE the bound is being enforced — a false operator statement
// of exactly the silent-wait class AIRA-71 was raised for. This test is what
// makes that deletion fail.
//
// Both contributing arms of aggregateScopeCap are present and the total is
// stated EXACTLY, so a snapshot that copied only one of them, or copied a
// stale zero, is caught too:
//
//	scan-visible, not connection-held   a leaf-drained suite scope, 30 GiB cap
//	connection-held, not yet scanned    a granted waiter, 10 GiB prospective cap
func TestSnapshotCarriesTheEstablishedAggregate(t *testing.T) {
	const (
		sliceMax = 64 * gib
		suiteCap = 30 * gib
		heldCap  = 10 * gib
		total    = suiteCap + heldCap
	)
	now := time.Unix(260_000, 0)
	server := oversubServer(&now, sliceMax, 3*gib, 200,
		staticScan(leafDrainedRecord("CONFINE-suite-1-a", 2*gib, suiteCap)))
	// Granted, scope-backed, and deliberately ABSENT from the scan: its cap is
	// the daemon's own prospective figure, which is the grant -> backend.Create
	// window this accounting exists to close.
	held := heldScopeWaiter(1, "CONFINE-held-1-a", heldCap, now.Add(-time.Hour))
	queue := &sliceQueue{
		path: "/slice", server: server, waiters: []*admitWaiter{held},
		outstanding: heldCap, outstandingJobs: 1,
	}
	registerAdmitQueue(server, queue)

	server.evaluateAdmitQueue(queue)

	// The queue's own state first, so a failure below is unambiguously the
	// snapshot copy rather than the accounting the other tests here cover.
	if !queue.capAggregateKnown || queue.capAggregate != total {
		t.Fatalf("test premise: queue aggregate = (%d, known=%v), want (%d, true)",
			queue.capAggregate, queue.capAggregateKnown, int64(total))
	}
	snapshot := server.admitSliceSnapshot("/slice")
	if !snapshot.present {
		t.Fatal("test wiring: the registered queue was not resolved, so nothing below is a reading of it")
	}
	if !snapshot.capAggregateKnown {
		t.Fatal("an aggregate established by a successful scan must reach the snapshot as ESTABLISHED; rendering it unevaluated would tell an operator the bound is not applied while it is")
	}
	if snapshot.capAggregate != total {
		t.Fatalf("snapshot aggregate = %d, want exactly %d (%d scan-visible + %d connection-held)",
			snapshot.capAggregate, int64(total), int64(suiteCap), int64(heldCap))
	}
}

// TestScanFailureWithholdsNothing is the same rule reached by the other route,
// and it must CLEAR a previously established total rather than keep gating on
// it. A stale aggregate held across a failing scan would block jobs on a
// reading that is no longer current — and it is the state a long scan outage
// leaves behind, so it is the likely one.
func TestScanFailureWithholdsNothing(t *testing.T) {
	const sliceMax = 64 * gib
	now := time.Unix(250_000, 0)
	server := oversubServer(&now, sliceMax, 0, 200, func(string) (runner.ConfineListResult, error) {
		return runner.ConfineListResult{}, errors.New("cgroupfs unreadable")
	})
	waiter := queuedScopeWaiter(1, "CONFINE-newcomer-1-a", 40*gib, now)
	queue := &sliceQueue{
		path: "/slice", server: server, waiters: []*admitWaiter{waiter},
		capAggregate: 500 * gib, capAggregateKnown: true,
	}

	server.evaluateAdmitQueue(queue)

	if queue.capAggregateKnown {
		t.Fatal("a failed scan must clear the established bit; the stale total is not a current reading")
	}
	if waiter.state != admitGranted {
		t.Fatalf("the bound must not gate on a stale aggregate across a failing scan (state=%v)", waiter.state)
	}
}

// TestBoundNeverWedgesAnIdleSlice is the structural anti-wedge guarantee. A
// delegate-ram waiter's reserve is its small pinned framework overhead while
// its scope cap is the large AIRA-15 ceiling, so its prospective cap can exceed
// the limit on its own once the slice ceiling is throttled — and without the
// "it would be the only capped scope" clause such a job could never start, on a
// completely idle machine, for as long as the pressure lasted.
//
// The second arm is what stops that clause becoming a blanket exemption: the
// same waiter beside one other live scope IS held back.
func TestBoundNeverWedgesAnIdleSlice(t *testing.T) {
	const (
		sliceMax     = 32 * gib
		overhead     = 512 * mib
		scopeCeiling = 48 * gib
	)
	now := time.Unix(260_000, 0)

	t.Run("alone", func(t *testing.T) {
		server := oversubServer(&now, sliceMax, 0, 100, staticScan())
		waiter := queuedScopeWaiter(1, "CONFINE-@dr-suite-1-a", overhead, now)
		waiter.scopeCeiling = scopeCeiling
		queue := &sliceQueue{path: "/slice", server: server, waiters: []*admitWaiter{waiter}}

		server.evaluateAdmitQueue(queue)

		if waiter.state != admitGranted {
			t.Fatalf("a waiter whose own scope cap %d exceeds the %d limit must still start on an EMPTY slice (state=%v)",
				int64(scopeCeiling), int64(sliceMax), waiter.state)
		}
		if queue.capAggregate != scopeCeiling {
			t.Fatalf("cap aggregate after the grant = %d, want the delegate scope ceiling %d, not its %d reserve",
				queue.capAggregate, int64(scopeCeiling), int64(overhead))
		}
	})

	t.Run("beside another scope", func(t *testing.T) {
		server := oversubServer(&now, sliceMax, gib, 100, staticScan(oversubRecord("CONFINE-other-1-a", gib, 4*gib)))
		waiter := queuedScopeWaiter(1, "CONFINE-@dr-suite-1-a", overhead, now)
		waiter.scopeCeiling = scopeCeiling
		queue := &sliceQueue{path: "/slice", server: server, waiters: []*admitWaiter{waiter}}

		server.evaluateAdmitQueue(queue)

		if waiter.state != admitQueued {
			t.Fatalf("the never-wedge clause must apply ONLY to an otherwise-idle slice (state=%v, aggregate=%d)", waiter.state, queue.capAggregate)
		}
	})
}

// TestAggregateTracksGrantsWithinOnePass closes the multi-grant overshoot. The
// derive runs at most once a second; a bound that only ever read that total
// would measure every grant in a burst against the same stale number and let
// them all through together.
//
// Both waiters are delegate-shaped — a small reserve, a large scope cap — so
// the pre-existing reserve check has ample room and cannot be what refuses the
// second. Against a scan-only implementation the aggregate stays 0, the
// never-wedge clause exempts both, and both are admitted.
func TestAggregateTracksGrantsWithinOnePass(t *testing.T) {
	const (
		sliceMax     = 64 * gib
		overhead     = 512 * mib
		scopeCeiling = 40 * gib
	)
	now := time.Unix(270_000, 0)
	server := oversubServer(&now, sliceMax, 0, 100, staticScan())
	first := queuedScopeWaiter(1, "CONFINE-@dr-one-1-a", overhead, now)
	first.scopeCeiling = scopeCeiling
	second := queuedScopeWaiter(2, "CONFINE-@dr-two-1-a", overhead, now)
	second.scopeCeiling = scopeCeiling
	queue := &sliceQueue{path: "/slice", server: server, waiters: []*admitWaiter{first, second}}

	server.evaluateAdmitQueue(queue)

	if first.state != admitGranted {
		t.Fatalf("the first waiter must be admitted onto an empty slice (state=%v)", first.state)
	}
	if second.state != admitQueued {
		t.Fatalf("the second waiter takes the caps to %d against a %d limit and must be held back in the SAME pass (state=%v)",
			int64(2*scopeCeiling), int64(sliceMax), second.state)
	}
	if queue.capAggregate != scopeCeiling {
		t.Fatalf("cap aggregate = %d, want exactly the one granted scope's ceiling %d", queue.capAggregate, int64(scopeCeiling))
	}
}

// TestScopelessWaitersAreNeverBoundedByTheAggregate. Neither an `aira
// confine-reserve` sub-reservation nor a plain `aira admit` creates a cgroup
// scope, so neither adds a cap and neither may be refused by a cap bound.
//
// The fixture is deliberately set up ALREADY OVER the bound — a single
// 300 GiB-capped scope against a 128 GiB limit — because that is the only state
// in which the exemption is observable, and it is the state that matters: a
// sub-reservation blocked here would stall the very suite whose exit is the
// only thing that can relieve the bound, so the queue could never converge.
// (With the aggregate merely under the limit, a build that gated these would
// pass anyway, since their own contribution is zero.)
func TestScopelessWaitersAreNeverBoundedByTheAggregate(t *testing.T) {
	const (
		sliceMax = 64 * gib
		suiteCap = 300 * gib // already far past the 128 GiB limit
	)
	now := time.Unix(280_000, 0)
	build := func(waiter *admitWaiter) (*Server, *sliceQueue) {
		server := oversubServer(&now, sliceMax, 2*gib, 200,
			staticScan(oversubRecord("CONFINE-@dr-suite-1-a", 2*gib, suiteCap)))
		return server, &sliceQueue{path: "/slice", server: server, waiters: []*admitWaiter{waiter}}
	}

	t.Run("sub-reservation", func(t *testing.T) {
		child := &admitWaiter{
			seq: 1, reserve: 4 * gib, state: admitQueued, grantedCh: make(chan struct{}),
			enqueued: now, parentScopeID: "CONFINE-@dr-suite-1-a",
		}
		server, queue := build(child)
		server.evaluateAdmitQueue(queue)

		if queue.capAggregate != suiteCap {
			t.Fatalf("cap aggregate = %d, want exactly %d (already over the bound)", queue.capAggregate, int64(suiteCap))
		}
		if child.state != admitGranted {
			t.Fatalf("a sub-reservation creates no scope and must never be refused by the aggregate bound (state=%v)", child.state)
		}
		if queue.capAggregate != suiteCap {
			t.Fatalf("granting a sub-reservation must add NO cap to the aggregate, got %d", queue.capAggregate)
		}
	})

	t.Run("plain admit", func(t *testing.T) {
		plain := &admitWaiter{
			seq: 1, reserve: 4 * gib, state: admitQueued,
			grantedCh: make(chan struct{}), enqueued: now,
		}
		server, queue := build(plain)
		server.evaluateAdmitQueue(queue)

		if plain.state != admitGranted {
			t.Fatalf("a scope-less waiter adds no cap and must never be refused by the aggregate bound (state=%v)", plain.state)
		}
		if queue.capAggregate != suiteCap {
			t.Fatalf("granting a scope-less waiter must add NO cap to the aggregate, got %d", queue.capAggregate)
		}
	})
}

// TestHeldScopePrefersTheKernelCapOverTheProspectiveOne. Once the scope exists
// the kernel's own memory.max is the authority; the daemon's own choice is the
// fallback that covers the grant -> backend.Create window. A build that always
// used the prospective value would miss a client-side --memory-max entirely.
func TestHeldScopePrefersTheKernelCapOverTheProspectiveOne(t *testing.T) {
	const sliceMax = 64 * gib
	now := time.Unix(290_000, 0)
	// The launcher applied an explicit --memory-max of 30 GiB, well above the
	// 8 GiB reserve the daemon granted.
	server := oversubServer(&now, sliceMax, gib, 200, staticScan(oversubRecord("CONFINE-explicit-1-a", gib, 30*gib)))
	held := heldScopeWaiter(1, "CONFINE-explicit-1-a", 8*gib, now.Add(-time.Hour))
	queue := &sliceQueue{
		path: "/slice", server: server, waiters: []*admitWaiter{held},
		outstanding: 8 * gib, outstandingJobs: 1,
	}

	server.evaluateAdmitQueue(queue)

	if queue.capAggregate != 30*gib {
		t.Fatalf("cap aggregate = %d, want the kernel's own memory.max %d rather than the %d reserve",
			queue.capAggregate, int64(30*gib), int64(8*gib))
	}
}

// TestHeldScopeCountsBeforeItsScopeExists closes the grant -> backend.Create
// window that EVERY launch has. A scan-only accounting reads zero for a freshly
// granted job for as long as its launcher takes to make the cgroup, which is
// precisely when a burst of grants would slip past the bound together.
//
// The holder is delegate-shaped — a 512 MiB pinned reserve against a 100 GiB
// scope ceiling — so the reserve ledger has ample room and cannot be what
// refuses the newcomer. Against a scan-only implementation the aggregate is 0,
// the never-wedge clause exempts the newcomer, and it is admitted.
func TestHeldScopeCountsBeforeItsScopeExists(t *testing.T) {
	const (
		sliceMax     = 64 * gib
		overhead     = 512 * mib
		scopeCeiling = 100 * gib
		newcomer     = 40 * gib
	)
	now := time.Unix(300_000, 0)
	server := oversubServer(&now, sliceMax, 0, 200, staticScan())
	// Granted and accounted, but its scope does not exist yet, so the successful
	// scan below returns no record for it at all.
	held := heldScopeWaiter(1, "CONFINE-@dr-launching-1-a", overhead, now)
	held.scopeCeiling = scopeCeiling
	waiter := queuedScopeWaiter(2, "CONFINE-newcomer-1-a", newcomer, now)
	queue := &sliceQueue{
		path: "/slice", server: server, waiters: []*admitWaiter{held, waiter},
		outstanding: overhead, outstandingJobs: 1,
	}

	server.evaluateAdmitQueue(queue)

	available := checkedAvailable(0, sliceMax, 0, overhead, 0)
	if available < newcomer {
		t.Fatalf("test arithmetic: only %d available, so the reserve check refused the %d newcomer", available, int64(newcomer))
	}
	if queue.capAggregate != scopeCeiling {
		t.Fatalf("cap aggregate = %d, want the granted-but-not-yet-created scope's own %d", queue.capAggregate, int64(scopeCeiling))
	}
	if waiter.state != admitQueued {
		t.Fatalf("a scope still being created must hold its cap in the aggregate (state=%v); %d + %d exceeds the %d limit",
			waiter.state, int64(scopeCeiling), int64(newcomer), int64(2*sliceMax))
	}
}

// TestBoundIsExceedNotReach pins the comparison at its boundary. The bound is
// "the caps may TOTAL the limit", so landing exactly on it is admitted and one
// byte past it is not. An off-by-one here is invisible to every other test in
// this file, and it is the difference between a bound an operator can reason
// about and one that quietly refuses the job that exactly fits.
func TestBoundIsExceedNotReach(t *testing.T) {
	const (
		sliceMax = 64 * gib
		scopeCap = 24 * gib
	)
	now := time.Unix(310_000, 0)
	run := func(t *testing.T, reserve int64) *admitWaiter {
		t.Helper()
		server := oversubServer(&now, sliceMax, gib, 100, staticScan(oversubRecord("CONFINE-hog-1-a", gib, scopeCap)))
		waiter := queuedScopeWaiter(1, "CONFINE-newcomer-1-a", reserve, now)
		queue := &sliceQueue{path: "/slice", server: server, waiters: []*admitWaiter{waiter}}
		server.evaluateAdmitQueue(queue)
		if queue.capAggregate < scopeCap {
			t.Fatalf("test arithmetic: the live scope contributed %d, not its %d cap", queue.capAggregate, int64(scopeCap))
		}
		return waiter
	}

	t.Run("exactly at the limit is admitted", func(t *testing.T) {
		if waiter := run(t, sliceMax-scopeCap); waiter.state != admitGranted {
			t.Fatalf("caps totalling exactly the %d limit must be admitted (state=%v)", int64(sliceMax), waiter.state)
		}
	})
	t.Run("one byte past it is not", func(t *testing.T) {
		if waiter := run(t, sliceMax-scopeCap+1); waiter.state != admitQueued {
			t.Fatalf("caps totalling one byte past the %d limit must be refused (state=%v)", int64(sliceMax), waiter.state)
		}
	})
}

func TestProspectiveScopeCap(t *testing.T) {
	tests := []struct {
		name   string
		waiter *admitWaiter
		want   int64
	}{
		{"non-delegate is its reserve", &admitWaiter{scopeID: "CONFINE-a-1-a", reserve: 8 * gib}, 8 * gib},
		{"delegate is its scope ceiling", &admitWaiter{scopeID: "CONFINE-@dr-a-1-a", reserve: 512 * mib, scopeCeiling: 48 * gib}, 48 * gib},
		{"scope-less contributes nothing", &admitWaiter{reserve: 8 * gib}, 0},
		{"sub-reservation contributes nothing", &admitWaiter{scopeID: "CONFINE-a-1-a", reserve: 8 * gib, parentScopeID: "CONFINE-b-1-a"}, 0},
		{"nil is zero", nil, 0},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := test.waiter.prospectiveScopeCap(); got != test.want {
				t.Fatalf("prospectiveScopeCap() = %d, want %d", got, test.want)
			}
		})
	}
}

func TestScaleByPercent(t *testing.T) {
	tests := []struct {
		value, pct, want int64
	}{
		{64 * gib, 200, 128 * gib},
		{64 * gib, 100, 64 * gib},
		{64 * gib, 250, 160 * gib},
		{0, 200, 0},
		{-1, 200, 0},
		{64 * gib, 0, 0},
		// The overflow branch saturates rather than wrapping, and a saturated
		// LIMIT only ever loosens the gate.
		{1 << 62, 10000, 1<<63 - 1},
	}
	for _, test := range tests {
		if got := scaleByPercent(test.value, test.pct); got != test.want {
			t.Fatalf("scaleByPercent(%d, %d) = %d, want %d", test.value, test.pct, got, test.want)
		}
	}
}

func TestOversubscriptionFactorFromEnv(t *testing.T) {
	const key = "AIRA_DAEMON_OVERSUBSCRIPTION_FACTOR"

	t.Run("absent takes the default", func(t *testing.T) {
		// t.Setenv registers the restore; Unsetenv then gives a genuinely ABSENT
		// variable, which t.Setenv alone cannot express.
		t.Setenv(key, "")
		if err := os.Unsetenv(key); err != nil {
			t.Fatal(err)
		}
		got, err := oversubscriptionFactorFromEnv()
		if err != nil || got != oversubscriptionFactorPctDefault {
			t.Fatalf("absent = (%d, %v), want (%d, nil)", got, err, oversubscriptionFactorPctDefault)
		}
	})

	accepted := map[string]int64{
		"":         oversubscriptionFactorPctDefault,
		"2":        200,
		"2.5":      250,
		" 3 ":      300,
		"1":        100,
		"disabled": 0,
		"off":      0,
		"0":        0,
	}
	for value, want := range accepted {
		t.Run("accepts "+value, func(t *testing.T) {
			t.Setenv(key, value)
			got, err := oversubscriptionFactorFromEnv()
			if err != nil {
				t.Fatalf("%q was refused: %v", value, err)
			}
			if got != want {
				t.Fatalf("%q = %d, want %d", value, got, want)
			}
		})
	}

	// A factor below 1 is the WEDGE case, refused rather than clamped: a job
	// admitted with a reserve at the ceiling gets a scope cap at the ceiling, so
	// a sub-1 factor would make such a job permanently unadmittable on an idle
	// machine. "banana" is the ordinary AIRA-58 refusal.
	for _, value := range []string{"0.9", "0.5", "-2", "banana", "200%", "101", "1e400"} {
		t.Run("refuses "+value, func(t *testing.T) {
			t.Setenv(key, value)
			got, err := oversubscriptionFactorFromEnv()
			if err == nil {
				t.Fatalf("%q was accepted as %d; an unrecognised or wedging factor must be REFUSED, never substituted", value, got)
			}
			if !strings.Contains(err.Error(), "E_CONFIG_INVALID") {
				t.Fatalf("%q refused with %v, want a stable E_CONFIG_INVALID code", value, err)
			}
		})
	}
}
