package daemon

import (
	"bytes"
	"context"
	"fmt"
	"log"
	"math/rand"
	"strings"
	"testing"
	"time"

	"aira/internal/runner"
)

// AIRA-350. The small-job lane's deterministic evaluator-seam tests, T1-T26 of
// spec docs/superpowers/specs/2026-11-18-fixed-small-lane-design.md section 6.
// The seam is AIRA-228/307's (queueOrderServer, queueOf, heldLease): one
// evaluateAdmitQueue call per pass, no evaluator goroutine, a fake clock and
// memory reader, and PRODUCTION headroom restored (H(n) = 2 GiB + 64 MiB x n).
//
// Sizes are MiB. x = 8192, y = 1024. 64 CPU slots unless a test states 32;
// every lease and waiter declares one core. "Small" waiters are scoped and
// laneKind; main leases book above y. "Budget spent" is the head's bypassed at
// its budget with the hold phase armed at `now` (never a backstop pass).
//
// Every expected figure is re-derived in a comment beside it and, where it is a
// fixture precondition, asserted (laneRoom) before the pass, so an oracle
// written against the wrong arithmetic fails on the fixture, not silently.

const laneE = 45056 * mib

// laneServer is the seam with the lane on (x 8G, y 1G), production headroom and
// 64 CPU slots. current is the slice's real use (memory.current), 0 unless a test
// moves it.
func laneServer(t *testing.T, maximum int64, current *int64, now *time.Time) *Server {
	t.Helper()
	server := queueOrderServer(t, maximum, now)
	server.admitSliceHeadroomBase = admitSliceHeadroomBaseDefault
	server.admitSliceHeadroomSupervisor = admitSliceHeadroomSupervisorDefault
	server.admitReadMemory = func(string) (int64, int64, int64, bool, string) {
		return *current, maximum, 0, true, ""
	}
	server.smallLaneCap = 8192 * mib
	server.smallLaneJobMax = 1024 * mib
	server.readCPUCores = func() int { return 32 } // 2 slots per core: 64
	return server
}

// laneJob is a queued, scoped, plain (laneKind) job of one core; whether it is
// small is its booking alone.
func laneJob(seq, reserve int64, now time.Time) *admitWaiter {
	waiter := queuedWaiter(seq, reserve, now)
	waiter.scopeID = fmt.Sprintf("CONFINE-lane%d-%d-a", seq, 9000+seq)
	waiter.laneKind = true
	waiter.cpu = 1
	return waiter
}

// laneLease is a granted, accounted, scoped, plain job of one core.
func laneLease(seq, reserve int64) *admitWaiter {
	lease := heldLease(seq, reserve)
	lease.scopeID = fmt.Sprintf("CONFINE-lease%d-%d-a", seq, 8000+seq)
	lease.laneKind = true
	lease.cpu = 1
	return lease
}

// spendBudget puts a head's AIRA-307 budget at its limit and the queue in the
// hold phase (armed now, maxHold one minute).
func spendBudget(queue *sliceQueue, head *admitWaiter, now time.Time) {
	head.bypassed = admitBypassBudget(head)
	queue.freezeArmedAt = now
}

func addWaiters(queue *sliceQueue, waiters ...*admitWaiter) {
	for _, waiter := range waiters {
		queue.waiters = append(queue.waiters, waiter)
		if waiter.seq > queue.seq {
			queue.seq = waiter.seq
		}
	}
	queue.outstanding, queue.cpuOutstanding, queue.vramOutstanding, queue.outstandingJobs = rederiveLedgerLocked(queue)
}

// endLease releases a lease the way the ledger sees it: no longer granted, then
// re-derived.
func endLease(queue *sliceQueue, lease *admitWaiter) {
	lease.state = admitReleased
	queue.outstanding, queue.cpuOutstanding, queue.vramOutstanding, queue.outstandingJobs = rederiveLedgerLocked(queue)
}

// laneRoom is the RAM ledger room one more waiter faces now (real use 0).
func laneRoom(server *Server, queue *sliceQueue, maximum int64) int64 {
	return checkedAvailable(0, maximum, 0, queue.outstanding, server.admitSliceHeadroom(queue.outstandingJobs+1))
}

func laneS(server *Server, queue *sliceQueue) int64 {
	return smallLeasesLocked(queue, server.smallLaneJobMax).booked
}

func requireLaneGranted(t *testing.T, waiter *admitWaiter, granted bool) {
	t.Helper()
	if granted && waiter.state != admitGranted {
		t.Fatalf("waiter %d (reserve %dM) not granted; laneWait %q heldByHead %v", waiter.seq, waiter.reserve/mib, waiter.laneWait, waiter.heldByHead)
	}
	if !granted && waiter.state == admitGranted {
		t.Fatalf("waiter %d (reserve %dM) unexpectedly granted", waiter.seq, waiter.reserve/mib)
	}
}

func requireLaneWait(t *testing.T, waiter *admitWaiter, want string) {
	t.Helper()
	if waiter.laneWait != want {
		t.Fatalf("waiter %d laneWait=%q, want %q", waiter.seq, waiter.laneWait, want)
	}
}

// requireBlocked asserts a waiter's AIRA-370 wait diagnosis, as the wire
// tokens. A lane waiter that does not fit carries no lane token: the main walk
// names the resource instead.
func requireBlocked(t *testing.T, waiter *admitWaiter, want ...string) {
	t.Helper()
	got := admitBlockedTokens(waiter.blocked, waiter.heldByHead)
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("waiter %d blocked_by=%v, want %v", waiter.seq, got, want)
	}
}

func requireHeldByHead(t *testing.T, waiter *admitWaiter, want bool) {
	t.Helper()
	if waiter.heldByHead != want {
		t.Fatalf("waiter %d heldByHead=%v, want %v", waiter.seq, waiter.heldByHead, want)
	}
}

func requireS(t *testing.T, server *Server, queue *sliceQueue, want int64) {
	t.Helper()
	if got := laneS(server, queue); got != want {
		t.Fatalf("S=%d (%.2fM), want %d (%.2fM)", got, float64(got)/float64(mib), want, float64(want)/float64(mib))
	}
}

// t1Fixture is T1's incident: 20 main leases booking 38182 (room 45056 −
// H(21) − 38182 = 45056 − 3392 − 38182 = 3482), a 4096 head with its budget
// spent in the hold phase, then 14 small waiters.
func t1Fixture(t *testing.T, server *Server, now time.Time, leaseCPU func(i int) int64) (*sliceQueue, *admitWaiter, []*admitWaiter) {
	t.Helper()
	var waiters []*admitWaiter
	seq := int64(0)
	for i := 0; i < 20; i++ {
		seq++
		booking := int64(1900) * mib
		if i == 19 {
			booking = 2082 * mib // 19 x 1900 + 2082 = 38182
		}
		lease := laneLease(seq, booking)
		lease.cpu = leaseCPU(i)
		waiters = append(waiters, lease)
	}
	seq++
	head := laneJob(seq, 4096*mib, now)
	waiters = append(waiters, head)
	var smalls []*admitWaiter
	for _, size := range []int64{72, 256, 512, 1024, 512, 256, 1024, 72, 512, 1024, 256, 512, 72, 1024} {
		seq++
		small := laneJob(seq, size*mib, now)
		smalls = append(smalls, small)
		waiters = append(waiters, small)
	}
	queue := queueOf(server, waiters...)
	spendBudget(queue, head, now)
	if room := laneRoom(server, queue, laneE); room != 3482*mib {
		t.Fatalf("fixture room=%dM, want 3482M", room/mib)
	}
	return queue, head, smalls
}

// verifies: AIRA-350 T1 — the incident. The lane grants 1-6, 8 and 11 (each
// grant costs its booking plus 64 MiB of headroom: 3482 → 3346 → 3026 → 2450 →
// 1362 → 786 → 466 → [7: 1024 > 466] → 330 → [9, 10] → 10 → [12, 13, 14]); S
// 2960; the head is refused, its bypassed and the anchor unchanged.
func TestSmallLaneT1Incident(t *testing.T) {
	now := time.Unix(350100, 0)
	current := int64(0)
	server := laneServer(t, laneE, &current, &now)
	queue, head, smalls := t1Fixture(t, server, now, func(int) int64 { return 1 })
	armed := queue.freezeArmedAt

	server.evaluateAdmitQueue(queue)

	grantedIdx := map[int]bool{0: true, 1: true, 2: true, 3: true, 4: true, 5: true, 7: true, 10: true}
	for i, small := range smalls {
		requireLaneGranted(t, small, grantedIdx[i])
		requireLaneWait(t, small, "")
		if grantedIdx[i] {
			requireBlocked(t, small)
		} else {
			requireBlocked(t, small, "ram")
		}
		requireHeldByHead(t, small, false)
	}
	requireS(t, server, queue, 2960*mib)
	if room := laneRoom(server, queue, laneE); room != 10*mib {
		t.Fatalf("room left=%dM, want 10M", room/mib)
	}
	requireLaneGranted(t, head, false)
	if head.bypassed != 2048*mib {
		t.Fatalf("head.bypassed=%dM, want 2048M (lane grants are never counted)", head.bypassed/mib)
	}
	if !queue.freezeArmedAt.Equal(armed) {
		t.Fatalf("anchor moved: %v -> %v", armed, queue.freezeArmedAt)
	}
}

// verifies: AIRA-350 T1b — the lane needs a CPU slot like any grant: T1 with 32
// of 32 slots held grants nothing, every small waiter no-room.
func TestSmallLaneT1bNoCPUSlot(t *testing.T) {
	now := time.Unix(350200, 0)
	current := int64(0)
	server := laneServer(t, laneE, &current, &now)
	server.readCPUCores = func() int { return 16 } // 32 slots
	// 12 leases of 2 cores and 8 of 1 hold all 32 slots.
	queue, _, smalls := t1Fixture(t, server, now, func(i int) int64 {
		if i < 12 {
			return 2
		}
		return 1
	})
	if queue.cpuOutstanding != 32 {
		t.Fatalf("fixture cpu=%d, want 32", queue.cpuOutstanding)
	}

	server.evaluateAdmitQueue(queue)

	for _, small := range smalls {
		requireLaneGranted(t, small, false)
		requireLaneWait(t, small, "")
		requireBlocked(t, small, "cpu") // AIRA-370: the slot ledger, not RAM
	}
}

// verifies: AIRA-350 T2 — the lane adds no memory. (a) room 0 and (b) room
// −512 (an ARDR deficit) grant nothing; (c) room 2000 at memory.max but the
// enforced ceiling 3000 lower (room −1000) grants nothing until it is restored,
// then exactly one 1024 (2000 − 1088 = 912 left).
func TestSmallLaneT2LedgerFull(t *testing.T) {
	for _, test := range []struct {
		name string
		room int64
	}{{"room 0", 0}, {"room -512", -512}} {
		t.Run(test.name, func(t *testing.T) {
			now := time.Unix(350300, 0)
			current := int64(0)
			server := laneServer(t, laneE, &current, &now)
			total := laneE/mib - 2240 - test.room // 45056 − H(3) − room
			queue := queueOf(server, laneLease(1, (total/2)*mib), laneLease(2, (total-total/2)*mib))
			if room := laneRoom(server, queue, laneE); room != test.room*mib {
				t.Fatalf("fixture room=%dM, want %dM", room/mib, test.room)
			}
			var smalls []*admitWaiter
			for seq := int64(3); seq <= 6; seq++ {
				smalls = append(smalls, laneJob(seq, 1024*mib, now))
			}
			addWaiters(queue, smalls...)

			server.evaluateAdmitQueue(queue)

			for _, small := range smalls {
				requireLaneGranted(t, small, false)
				requireLaneWait(t, small, "")
				requireBlocked(t, small, "ram")
			}
		})
	}
	t.Run("enforced ceiling below memory.max", func(t *testing.T) {
		now := time.Unix(350310, 0)
		current := int64(0)
		server := laneServer(t, laneE, &current, &now)
		queue := queueOf(server, laneLease(1, 20408*mib), laneLease(2, 20408*mib)) // 45056 − 2240 − 40816 = 2000
		if room := laneRoom(server, queue, laneE); room != 2000*mib {
			t.Fatalf("fixture room=%dM, want 2000M", room/mib)
		}
		server.publishSliceCeilingSnapshot(sliceCeilingSnapshot{
			Mode: sliceCeilingEnforce, SlicePath: "/slice",
			State: sliceCeilingThrottled, Ceiling: laneE - 3000*mib, StaticMax: laneE,
		})
		var smalls []*admitWaiter
		for seq := int64(3); seq <= 6; seq++ {
			smalls = append(smalls, laneJob(seq, 1024*mib, now))
		}
		addWaiters(queue, smalls...)

		server.evaluateAdmitQueue(queue)
		for _, small := range smalls {
			requireLaneGranted(t, small, false)
			requireLaneWait(t, small, "")
			requireBlocked(t, small, "ram", "ceiling-cut") // AIRA-370: the cut is in force
		}

		server.publishSliceCeilingSnapshot(sliceCeilingSnapshot{
			Mode: sliceCeilingEnforce, SlicePath: "/slice",
			State: sliceCeilingUnthrottled, Ceiling: laneE, StaticMax: laneE,
		})
		server.evaluateAdmitQueue(queue)
		requireLaneGranted(t, smalls[0], true)
		for _, small := range smalls[1:] {
			requireLaneGranted(t, small, false)
		}
	})
}

// verifies: AIRA-350 T3 — the lane cap, and that a lane refusal is still an
// ordinary main waiter. S 7680 (eight 960 small leases), room 6000 (one main
// lease of 28688: 45056 − H(10) − 7680 − 28688 = 6000).
//
// Pass 1: the 1024 is full (8704 > 8192); the 512 is granted at equality (S
// 8192); today's walk then grants the 1024 in main (S 9216). Pass 2 (hold): an
// 8192 waiter, refused (room 5424 − 1088 = 4336) with its budget spent, is the
// head; another 512 is full and, in the walk, held behind the head. Pass 3 (a
// restart freeze, so neither the pre-pass nor the walk reaches the 512): it is
// still queued, and both pass-2 diagnoses are gone, since each pass clears them
// before rewriting (spec §2.1); a stale "lane full" or "held behind the head"
// would otherwise reach the wait line and `confine --list`.
func TestSmallLaneT3LaneFull(t *testing.T) {
	now := time.Unix(350400, 0)
	current := int64(0)
	server := laneServer(t, laneE, &current, &now)
	var waiters []*admitWaiter
	for seq := int64(1); seq <= 8; seq++ {
		waiters = append(waiters, laneLease(seq, 960*mib))
	}
	waiters = append(waiters, laneLease(9, 28688*mib))
	queue := queueOf(server, waiters...)
	if room := laneRoom(server, queue, laneE); room != 6000*mib {
		t.Fatalf("fixture room=%dM, want 6000M", room/mib)
	}
	requireS(t, server, queue, 7680*mib)
	first, second := laneJob(10, 1024*mib, now), laneJob(11, 512*mib, now)
	addWaiters(queue, first, second)

	server.evaluateAdmitQueue(queue)

	requireLaneWait(t, first, laneWaitFull)
	requireLaneGranted(t, first, true) // by today's walk
	requireLaneWait(t, second, "")
	requireLaneGranted(t, second, true)
	requireS(t, server, queue, 9216*mib)

	head, third := laneJob(12, 8192*mib, now), laneJob(13, 512*mib, now)
	addWaiters(queue, head, third)
	spendBudget(queue, head, now)
	server.evaluateAdmitQueue(queue)

	requireLaneGranted(t, head, false)
	requireLaneGranted(t, third, false)
	requireLaneWait(t, third, laneWaitFull)
	requireHeldByHead(t, third, true)

	server.restartFreezeUntilNanos.Store(now.Add(time.Hour).UnixNano())
	server.evaluateAdmitQueue(queue)

	if third.state != admitQueued {
		t.Fatalf("waiter %d state %v, want still queued", third.seq, third.state)
	}
	requireLaneWait(t, third, "")
	requireHeldByHead(t, third, false)
}

// t4Fixture: E 45056; a 40960 head with its budget spent (hold); a never-ending
// small lease (silt); main leases 2 x 1536 (room 45056 − H(4) − 4096 = 38656);
// small waiters 1024, 512, 320, 256.
func t4Fixture(t *testing.T, server *Server, now time.Time, silt *admitWaiter) (*sliceQueue, *admitWaiter, []*admitWaiter, []*admitWaiter) {
	t.Helper()
	head := laneJob(1, 40960*mib, now)
	mains := []*admitWaiter{laneLease(3, 1536*mib), laneLease(4, 1536*mib)}
	smalls := []*admitWaiter{laneJob(5, 1024*mib, now), laneJob(6, 512*mib, now), laneJob(7, 320*mib, now), laneJob(8, 256*mib, now)}
	queue := queueOf(server, append([]*admitWaiter{head, silt, mains[0], mains[1]}, smalls...)...)
	spendBudget(queue, head, now)
	if room := laneRoom(server, queue, laneE); room != 38656*mib {
		t.Fatalf("fixture room=%dM, want 38656M", room/mib)
	}
	return queue, head, mains, smalls
}

// verifies: AIRA-350 T4 — guard G, with silt.
//
// Pass 1, G = 45056 − H(J_s + 1) − S_pin against the 40960 head: the 1024 kept
// (45056 − H(3) − 2048 = 40768) and held behind the head; the 512 granted
// (45056 − 2240 − 1536 = 41280); the 320 kept (45056 − H(4) − 1856 = 40896);
// the 256 granted at equality (45056 − 2304 − 1792 = 40960). Pass 2 (both main
// leases end): the head is granted, room 45056 − H(4) − 1792 = 40960.
func TestSmallLaneT4GuardSilt(t *testing.T) {
	t.Run("base", func(t *testing.T) {
		now := time.Unix(350500, 0)
		current := int64(0)
		server := laneServer(t, laneE, &current, &now)
		queue, head, mains, smalls := t4Fixture(t, server, now, laneLease(2, 1024*mib))

		server.evaluateAdmitQueue(queue)

		requireLaneGranted(t, smalls[0], false)
		requireLaneWait(t, smalls[0], laneWaitKept)
		requireHeldByHead(t, smalls[0], true)
		requireLaneGranted(t, smalls[1], true)
		requireLaneGranted(t, smalls[2], false)
		requireLaneWait(t, smalls[2], laneWaitKept)
		requireLaneGranted(t, smalls[3], true)
		requireLaneGranted(t, head, false)
		requireS(t, server, queue, 1792*mib)
		if head.bypassed != 20480*mib {
			t.Fatalf("head.bypassed=%dM, want 20480M", head.bypassed/mib)
		}

		endLease(queue, mains[0])
		endLease(queue, mains[1])
		server.evaluateAdmitQueue(queue)
		requireLaneGranted(t, head, true)
		requireLaneGranted(t, smalls[0], false)
		requireLaneGranted(t, smalls[2], false)
	})
	t.Run("G on the pin of a discounted silt lease", func(t *testing.T) {
		// (b) The silt lease is a discounted pin 1137 booking 1024: pins 1137 +
		// 512 + 256 = 1905, so the 256 is kept (45056 − 2304 − 1905 = 40847).
		now := time.Unix(350510, 0)
		current := int64(0)
		server := laneServer(t, laneE, &current, &now)
		silt := laneLease(2, 1137*mib)
		silt.charge = 1024 * mib
		queue, _, _, smalls := t4Fixture(t, server, now, silt)

		server.evaluateAdmitQueue(queue)

		requireLaneGranted(t, smalls[1], true)
		requireLaneWait(t, smalls[3], laneWaitKept)
		requireLaneGranted(t, smalls[3], false)
	})
	t.Run("small-job churn does not starve the head behind G", func(t *testing.T) {
		// (c) Each pass from pass 2 one non-silt small lease ends and a 1024
		// small waiter arrives. With G the head is granted in pass 2 (the 320 is
		// lane-granted beside it: 45056 − H(4) − 1600 = 41152 >= 40960, and the
		// head's room is that same 41152). The twin without G keeps re-filling
		// the lane ahead of it (3648 booked, room 38976) and never grants it in 50
		// passes: that twin is the mutation this subtest kills.
		now := time.Unix(350520, 0)
		current := int64(0)
		server := laneServer(t, laneE, &current, &now)
		silt := laneLease(2, 1024*mib)
		queue, head, mains, _ := t4Fixture(t, server, now, silt)
		server.evaluateAdmitQueue(queue)
		endLease(queue, mains[0])
		endLease(queue, mains[1])
		seq := int64(100)
		grantedAt := 0
		for pass := 2; pass <= 50 && grantedAt == 0; pass++ {
			for _, waiter := range queue.waiters {
				if waiter != silt && waiter != head && waiter.state == admitGranted && waiter.reserve <= 1024*mib {
					endLease(queue, waiter)
					break
				}
			}
			seq++
			addWaiters(queue, laneJob(seq, 1024*mib, now))
			server.evaluateAdmitQueue(queue)
			if head.state == admitGranted {
				grantedAt = pass
			}
		}
		if grantedAt != 2 {
			t.Fatalf("head granted at pass %d, want pass 2 (0 = never in 50 passes)", grantedAt)
		}
	})
	t.Run("four silt leases", func(t *testing.T) {
		// (d) Four never-ending 1024 small leases: room 45056 − H(7) − 7168 =
		// 35392; G for every waiter 45056 − H(6) − 4096 − w = 38528 − w < 40960.
		// After the main leases end the head's room is 45056 − H(5) − 4096 =
		// 38592 < 40960: it waits on the silt, which the lane never adds to.
		now := time.Unix(350530, 0)
		current := int64(0)
		server := laneServer(t, laneE, &current, &now)
		head := laneJob(1, 40960*mib, now)
		silts := []*admitWaiter{laneLease(2, 1024*mib), laneLease(3, 1024*mib), laneLease(4, 1024*mib), laneLease(5, 1024*mib)}
		mains := []*admitWaiter{laneLease(6, 1536*mib), laneLease(7, 1536*mib)}
		smalls := []*admitWaiter{laneJob(8, 1024*mib, now), laneJob(9, 512*mib, now), laneJob(10, 320*mib, now), laneJob(11, 256*mib, now)}
		queue := queueOf(server, append(append(append([]*admitWaiter{head}, silts...), mains...), smalls...)...)
		spendBudget(queue, head, now)
		if room := laneRoom(server, queue, laneE); room != 35392*mib {
			t.Fatalf("fixture room=%dM, want 35392M", room/mib)
		}
		for pass := 1; pass <= 2; pass++ {
			if pass == 2 {
				endLease(queue, mains[0])
				endLease(queue, mains[1])
			}
			server.evaluateAdmitQueue(queue)
			requireLaneGranted(t, head, false)
			for _, small := range smalls {
				requireLaneGranted(t, small, false)
				requireLaneWait(t, small, laneWaitKept)
			}
		}
	})
	t.Run("use outside the leases (R11) and the main walk's physical floor", func(t *testing.T) {
		// (e) Pass 2 with real use 2048 (256 in no lease): the head's room is
		// 45056 − H(4) − max(2048, 1792) = 40704, refused; pass 3 with 1792:
		// 40960, granted.
		now := time.Unix(350540, 0)
		current := int64(0)
		server := laneServer(t, laneE, &current, &now)
		queue, head, mains, _ := t4Fixture(t, server, now, laneLease(2, 1024*mib))
		server.evaluateAdmitQueue(queue)
		requireS(t, server, queue, 1792*mib)

		endLease(queue, mains[0])
		endLease(queue, mains[1])
		current = 2048 * mib
		server.evaluateAdmitQueue(queue)
		requireLaneGranted(t, head, false)

		current = 1792 * mib
		server.evaluateAdmitQueue(queue)
		requireLaneGranted(t, head, true)
	})
}

// verifies: AIRA-350 T5 — G's CPU conjunct. 32 slots: 26 small leases and 6
// main ones; a head needing 4 slots (RAM ample), budget spent. Each pass one
// main lease ends and a one-slot small waiter arrives: the lane grants at 27 and
// 28 small slots (28 + 4 = 32), keeps the next, and the head is granted once 4
// slots are free (pass 6: 28 small, 0 main).
func TestSmallLaneT5GuardCPU(t *testing.T) {
	now := time.Unix(350600, 0)
	current := int64(0)
	server := laneServer(t, laneE, &current, &now)
	server.readCPUCores = func() int { return 16 } // 32 slots
	head := laneJob(1, 4096*mib, now)
	head.cpu = 4
	waiters := []*admitWaiter{head}
	seq := int64(1)
	for i := 0; i < 26; i++ {
		seq++
		waiters = append(waiters, laneLease(seq, 100*mib))
	}
	var mains []*admitWaiter
	for i := 0; i < 6; i++ {
		seq++
		lease := laneLease(seq, 2048*mib)
		mains = append(mains, lease)
		waiters = append(waiters, lease)
	}
	queue := queueOf(server, waiters...)
	spendBudget(queue, head, now)
	var arrivals []*admitWaiter
	for pass := 1; pass <= 6; pass++ {
		endLease(queue, mains[pass-1])
		seq++
		arrival := laneJob(seq, 100*mib, now)
		arrivals = append(arrivals, arrival)
		addWaiters(queue, arrival)

		server.evaluateAdmitQueue(queue)

		switch {
		case pass <= 2:
			requireLaneGranted(t, arrival, true)
			requireLaneGranted(t, head, false)
		case pass < 6:
			requireLaneGranted(t, arrival, false)
			requireLaneWait(t, arrival, laneWaitKept)
			requireLaneGranted(t, head, false)
		default:
			requireLaneGranted(t, head, true)
			requireLaneWait(t, arrival, laneWaitKept)
		}
	}
	for _, arrival := range arrivals[2:] {
		requireLaneGranted(t, arrival, false)
	}
}

// verifies: AIRA-350 T6 — G's VRAM conjunct, which assumes each small lease uses
// its whole declaration (today's fit charges a running job twice: in the card's
// free and in the ledger). Card 21504, headroom 1024, budget auto. Main leases
// declare and use 8192 (free 13312); the 16384 head's fit is min(21504, 12288)
// − 8192 = 4096, refused.
//
// Pass 1: the first 2048 granted (G: min(21504, 21504 − 2048 − 1024) − 2048 =
// 16384); the second kept (own fit min(21504, 12288) − 10240 = 2048 passes; G
// 16384 − 4096 = 12288) and held behind the head; the 0-VRAM one granted. Pass 2
// (main leases end, the granted small uses 2048, fresh free 19456): the head is
// granted (min(21504, 18432) − 2048 = 16384); the second refused on fit.
func TestSmallLaneT6GuardVRAM(t *testing.T) {
	now := time.Unix(350700, 0)
	current := int64(0)
	server := laneServer(t, laneE, &current, &now)
	server.vramHeadroom = 1024 * mib
	server.vramSnap.Store(&vramSnapshot{total: 21504 * mib, free: 13312 * mib, evaluated: true, sampledAt: now})
	main := laneLease(1, 2048*mib)
	main.vram = 8192 * mib
	head := laneJob(2, 4096*mib, now)
	head.vram = 16384 * mib
	first, second, third := laneJob(3, 512*mib, now), laneJob(4, 512*mib, now), laneJob(5, 512*mib, now)
	first.vram, second.vram = 2048*mib, 2048*mib
	queue := queueOf(server, main, head, first, second, third)
	spendBudget(queue, head, now)

	server.evaluateAdmitQueue(queue)

	requireLaneGranted(t, head, false)
	requireLaneGranted(t, first, true)
	requireLaneGranted(t, second, false)
	requireLaneWait(t, second, laneWaitKept)
	requireHeldByHead(t, second, true)
	requireLaneGranted(t, third, true)

	endLease(queue, main)
	server.vramSnap.Store(&vramSnapshot{total: 21504 * mib, free: 19456 * mib, evaluated: true, sampledAt: now})
	server.evaluateAdmitQueue(queue)

	requireLaneGranted(t, head, true)
	requireLaneGranted(t, second, false)
}

// t7Fixture: room 3000 (leases 30000 and 9816: 45056 − H(3) − 39816 = 3000).
func t7Fixture(t *testing.T, server *Server) (*sliceQueue, *admitWaiter) {
	t.Helper()
	ending := laneLease(2, 9816*mib)
	queue := queueOf(server, laneLease(1, 30000*mib), ending)
	if room := laneRoom(server, queue, laneE); room != 3000*mib {
		t.Fatalf("fixture room=%dM, want 3000M", room/mib)
	}
	return queue, ending
}

func pushWaiter(queue *sliceQueue, waiter *admitWaiter) {
	queue.seq++
	waiter.pushSeq = queue.seq
}

// verifies: AIRA-350 T7 — pushed. (a) Behind a pushed refused 4096 head the
// lane is closed; in the pass it fits (room 45056 − H(2) − 30000 = 12880) today's
// walk grants it and then, the same pass, the small waiters (S rises by their
// bookings). (b) A pushed small waiter is the reference: lane-granted. (c) A
// pushed small waiter behind a later push: closed.
func TestSmallLaneT7Pushed(t *testing.T) {
	t.Run("closed behind a pushed reference, then the same-pass main grants", func(t *testing.T) {
		now := time.Unix(350800, 0)
		current := int64(0)
		server := laneServer(t, laneE, &current, &now)
		queue, ending := t7Fixture(t, server)
		head := laneJob(3, 4096*mib, now)
		smalls := []*admitWaiter{laneJob(4, 512*mib, now), laneJob(5, 256*mib, now)}
		addWaiters(queue, head, smalls[0], smalls[1])
		pushWaiter(queue, head)

		server.evaluateAdmitQueue(queue)
		requireLaneGranted(t, head, false)
		for _, small := range smalls {
			requireLaneGranted(t, small, false)
			requireLaneWait(t, small, laneWaitPushed)
			requireHeldByHead(t, small, true)
		}
		requireS(t, server, queue, 0)

		endLease(queue, ending)
		server.evaluateAdmitQueue(queue)
		requireLaneGranted(t, head, true)
		for _, small := range smalls {
			requireLaneGranted(t, small, true)
		}
		requireS(t, server, queue, 768*mib)
	})
	t.Run("a pushed small waiter is the reference", func(t *testing.T) {
		now := time.Unix(350810, 0)
		current := int64(0)
		server := laneServer(t, laneE, &current, &now)
		queue, _ := t7Fixture(t, server)
		head := laneJob(3, 4096*mib, now)
		small := laneJob(4, 512*mib, now)
		addWaiters(queue, head, small)
		spendBudget(queue, head, now)
		pushWaiter(queue, small)

		server.evaluateAdmitQueue(queue)
		requireLaneGranted(t, small, true)
		requireLaneWait(t, small, "")
		requireLaneGranted(t, head, false)
	})
	t.Run("a pushed small waiter behind a later push", func(t *testing.T) {
		now := time.Unix(350820, 0)
		current := int64(0)
		server := laneServer(t, laneE, &current, &now)
		queue, _ := t7Fixture(t, server)
		head := laneJob(3, 4096*mib, now)
		small := laneJob(4, 512*mib, now)
		addWaiters(queue, head, small)
		pushWaiter(queue, small)
		pushWaiter(queue, head)

		server.evaluateAdmitQueue(queue)
		requireLaneGranted(t, small, false)
		requireLaneWait(t, small, laneWaitPushed)
	})
}

// verifies: AIRA-350 T8 — high is order only: past an unpushed `high` head with
// its budget spent, the lane grants.
func TestSmallLaneT8High(t *testing.T) {
	now := time.Unix(350900, 0)
	current := int64(0)
	server := laneServer(t, laneE, &current, &now)
	queue, _ := t7Fixture(t, server)
	head := laneJob(3, 4096*mib, now)
	head.high = true
	smalls := []*admitWaiter{laneJob(4, 512*mib, now), laneJob(5, 256*mib, now)}
	addWaiters(queue, head, smalls[0], smalls[1])
	spendBudget(queue, head, now)

	server.evaluateAdmitQueue(queue)

	requireLaneGranted(t, head, false)
	for _, small := range smalls {
		requireLaneGranted(t, small, true)
	}
}

// verifies: AIRA-350 T9 — exclusivity. (a) Under an exclusive holder a small
// waiter that is not its own is not granted. (b) A drain is granted only after
// the small leases release, and no small waiter passes it. (c) An exclusive
// small waiter is not eligible.
func TestSmallLaneT9Exclusivity(t *testing.T) {
	t.Run("held", func(t *testing.T) {
		now := time.Unix(351000, 0)
		current := int64(0)
		server := laneServer(t, laneE, &current, &now)
		holder := laneLease(1, 2048*mib)
		holder.exclusive = true
		small := laneJob(2, 512*mib, now)
		queue := queueOf(server, holder, small)

		server.evaluateAdmitQueue(queue)
		requireLaneGranted(t, small, false)
		requireLaneWait(t, small, "")
	})
	t.Run("draining", func(t *testing.T) {
		now := time.Unix(351010, 0)
		current := int64(0)
		server := laneServer(t, laneE, &current, &now)
		leases := []*admitWaiter{laneLease(1, 512*mib), laneLease(2, 256*mib)}
		drain := laneJob(3, 2048*mib, now)
		drain.exclusive = true
		small := laneJob(4, 512*mib, now)
		queue := queueOf(server, leases[0], leases[1], drain, small)

		server.evaluateAdmitQueue(queue)
		requireLaneGranted(t, drain, false)
		requireLaneGranted(t, small, false)

		endLease(queue, leases[0])
		server.evaluateAdmitQueue(queue)
		requireLaneGranted(t, drain, false)
		requireLaneGranted(t, small, false)

		endLease(queue, leases[1])
		server.evaluateAdmitQueue(queue)
		requireLaneGranted(t, drain, true)
		requireLaneGranted(t, small, false)
	})
	t.Run("an exclusive small waiter is not eligible", func(t *testing.T) {
		now := time.Unix(351020, 0)
		current := int64(0)
		server := laneServer(t, laneE, &current, &now)
		small := laneJob(1, 512*mib, now)
		if !server.laneEligibleLocked(small) {
			t.Fatal("a plain small waiter must be eligible (control)")
		}
		small.exclusive = true
		if server.laneEligibleLocked(small) {
			t.Fatal("an exclusive small waiter is eligible")
		}
	})
}

// verifies: AIRA-350 T10 — the pre-pass is skipped during the restart freeze.
func TestSmallLaneT10RestartFreeze(t *testing.T) {
	now := time.Unix(351100, 0)
	current := int64(0)
	server := laneServer(t, laneE, &current, &now)
	server.restartFreezeUntilNanos.Store(now.Add(time.Hour).UnixNano())
	small := laneJob(1, 512*mib, now)
	queue := queueOf(server, small)

	server.evaluateAdmitQueue(queue)

	requireLaneGranted(t, small, false)
	if !small.waited {
		t.Fatal("a frozen waiter must be marked waited")
	}
	requireLaneWait(t, small, "")
}

// verifies: AIRA-350 T11 — lane grants never touch the fairness duty cycle. (a)
// Idle, a head with its budget spent, small waiters that fit: lane grants and
// the anchor stays zero; the lane-off twin withholds them and arms. (b) The
// backstop pass: the lane grants first, then today's backfill, and bypassed
// counts only the main grant. (c) Hold: lane grants.
func TestSmallLaneT11FreezeAndBackstop(t *testing.T) {
	build := func(t *testing.T, server *Server, now time.Time) (*sliceQueue, *admitWaiter, []*admitWaiter) {
		queue, _ := t7Fixture(t, server)
		head := laneJob(3, 8192*mib, now)
		smalls := []*admitWaiter{laneJob(4, 512*mib, now), laneJob(5, 256*mib, now)}
		addWaiters(queue, head, smalls[0], smalls[1])
		head.bypassed = admitBypassBudget(head)
		return queue, head, smalls
	}
	t.Run("idle", func(t *testing.T) {
		for _, lane := range []bool{true, false} {
			now := time.Unix(351200, 0)
			current := int64(0)
			server := laneServer(t, laneE, &current, &now)
			if !lane {
				server.smallLaneCap = 0
			}
			queue, head, smalls := build(t, server, now)

			server.evaluateAdmitQueue(queue)

			for _, small := range smalls {
				requireLaneGranted(t, small, lane)
			}
			if lane && !queue.freezeArmedAt.IsZero() {
				t.Fatal("a lane grant armed the hold")
			}
			if !lane && queue.freezeArmedAt.IsZero() {
				t.Fatal("lane-off twin: the withheld fit must arm the hold")
			}
			if head.bypassed != 4096*mib {
				t.Fatalf("head.bypassed=%dM, want 4096M", head.bypassed/mib)
			}
		}
	})
	t.Run("backstop", func(t *testing.T) {
		now := time.Unix(351210, 0)
		current := int64(0)
		server := laneServer(t, laneE, &current, &now)
		queue, head, smalls := build(t, server, now)
		backfill := laneJob(6, 1536*mib, now)
		addWaiters(queue, backfill)
		queue.freezeArmedAt = now.Add(-2 * time.Minute) // hold over: one backfilling pass

		server.evaluateAdmitQueue(queue)

		for _, small := range smalls {
			requireLaneGranted(t, small, true)
		}
		requireLaneGranted(t, backfill, true)
		if head.bypassed != (4096+1536)*mib {
			t.Fatalf("head.bypassed=%dM, want 5632M (only the main backfill grant)", head.bypassed/mib)
		}
	})
	t.Run("hold", func(t *testing.T) {
		now := time.Unix(351220, 0)
		current := int64(0)
		server := laneServer(t, laneE, &current, &now)
		queue, head, smalls := build(t, server, now)
		queue.freezeArmedAt = now
		armed := queue.freezeArmedAt

		server.evaluateAdmitQueue(queue)

		for _, small := range smalls {
			requireLaneGranted(t, small, true)
		}
		if !queue.freezeArmedAt.Equal(armed) || head.bypassed != 4096*mib {
			t.Fatalf("anchor %v (want %v), bypassed %dM (want 4096M)", queue.freezeArmedAt, armed, head.bypassed/mib)
		}
	})
}

// verifies: AIRA-350 T12 — AIRA-324. y is in bookings: under enforce at 90% a
// pin of 1137 MiB books 1023.3 (eligible) and 1138 books 1024.2 (not); S rises
// by the booking, G sees the pin. Under observe the booking is the pin: 1024
// eligible, 1025 not.
func TestSmallLaneT12PinCharge(t *testing.T) {
	t.Run("enforce", func(t *testing.T) {
		now := time.Unix(351300, 0)
		current := int64(0)
		server := laneServer(t, laneE, &current, &now)
		server.pinChargeMode, server.pinChargePercent = pinChargeEnforce, 90
		eligible, over := pinWaiter(server, 2, 1137*mib, now), pinWaiter(server, 3, 1138*mib, now)
		eligible.laneKind, over.laneKind = true, true
		eligible.cpu, over.cpu = 1, 1
		booking := eligible.ledgerCharge()
		if booking != (1137*mib*90+99)/100 || booking > 1024*mib || over.ledgerCharge() <= 1024*mib {
			t.Fatalf("fixture bookings %d and %d", booking, over.ledgerCharge())
		}
		if !server.laneEligibleLocked(eligible) || server.laneEligibleLocked(over) {
			t.Fatal("eligibility must be on the booking: 1137 eligible, 1138 not")
		}
		// G sees the pin: a reference needing exactly one byte more than G
		// leaves beside the pin (45056 − H(2) − 1137) keeps it back, though it
		// would fit beside the booking.
		// A 4096 main lease (not small) keeps the reference refused (room 45056 −
		// H(2) − 4096 = 38784) without entering G's occupancy.
		ref := laneJob(1, laneE-2176*mib-1137*mib+1, now)
		queue := queueOf(server, laneLease(9, 4096*mib), ref, eligible)
		spendBudget(queue, ref, now)
		server.evaluateAdmitQueue(queue)
		requireLaneWait(t, eligible, laneWaitKept)

		ref.reserve--
		server.evaluateAdmitQueue(queue)
		requireLaneGranted(t, eligible, true)
		requireS(t, server, queue, booking)
	})
	t.Run("observe", func(t *testing.T) {
		now := time.Unix(351310, 0)
		current := int64(0)
		server := laneServer(t, laneE, &current, &now)
		server.pinChargeMode, server.pinChargePercent = pinChargeObserve, 90
		at, over := pinWaiter(server, 1, 1024*mib, now), pinWaiter(server, 2, 1025*mib, now)
		at.laneKind, over.laneKind = true, true
		if !server.laneEligibleLocked(at) || server.laneEligibleLocked(over) {
			t.Fatal("observe: 1024 eligible, 1025 not")
		}
	})
}

// t13Leases builds T13's leases on a server in enforce at 90%: two lane-granted
// (512, 256), one main-granted (768), a discounted pin 1100 (booked 990) and pin
// 1000 (booked 900), resized 4096 to 512 and 1024 to 256, and three main
// leases. Before: S = 512 + 256 + 768 + 990 + 900 + 512 + 256 = 4194.
type t13Lease struct {
	pin, booking int64
	discountable bool
}

func t13Set() []t13Lease {
	return []t13Lease{
		{512, 512, false}, {256, 256, false}, {768, 768, false},
		{1100, 990, true}, {1000, 900, true},
		{4096, 512, false}, {1024, 256, false},
		{4096, 4096, false}, {6144, 6144, false}, {2048, 2048, false},
	}
}

func t13Server(t *testing.T, now *time.Time) *Server {
	t.Helper()
	current := int64(0)
	server := laneServer(t, laneE, &current, now)
	server.pinChargeMode, server.pinChargePercent = pinChargeEnforce, 90
	server.admitManualEvaluation = true
	return server
}

func t13Request(index int, lease t13Lease) admitRequest {
	return admitRequest{
		reserve: lease.pin * mib, cpu: 1, scopeID: fmt.Sprintf("CONFINE-t13j%d-%d-a", index, 6000+index),
		name: fmt.Sprintf("t13j%d", index), owner: "a", peerSameUID: true, signature: "plain",
	}
}

// establishT13 re-declares every lease (absent: establish) in the given order
// on a fresh server.
func establishT13(t *testing.T, server *Server, set []t13Lease, order []int) *sliceQueue {
	t.Helper()
	var queue *sliceQueue
	for _, index := range order {
		q, _, code, err := server.enqueueReDeclare("/slice", set[index].pin*mib, "pinned:client", t13Request(index, set[index]))
		if err != nil {
			t.Fatalf("establish %d: %s %v", index, code, err)
		}
		queue = q
	}
	return queue
}

// liveT13 builds the live leases (as granted by a live daemon) on a server's
// queue.
func liveT13(t *testing.T, server *Server, set []t13Lease) *sliceQueue {
	t.Helper()
	server.admitRegistryMu.Lock()
	queue, err := server.admitQueueLocked("/slice")
	server.admitRegistryMu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	queue.mu.Lock()
	defer queue.mu.Unlock()
	for index, lease := range set {
		request := t13Request(index, lease)
		waiter := newEstablishedWaiter(int64(index+1), lease.pin*mib, 1, "pinned:client", request, time.Unix(0, 0))
		waiter.discountable = lease.discountable
		waiter.charge = bookingField(lease.booking*mib, lease.pin*mib)
		waiter.laneKind = true
		queue.waiters = append(queue.waiters, waiter)
		queue.seq = int64(index + 1)
	}
	queue.outstanding, queue.cpuOutstanding, queue.vramOutstanding, queue.outstandingJobs = rederiveLedgerLocked(queue)
	return queue
}

// setT13 re-declares every lease IN PLACE (the reconnect SET).
func setT13(t *testing.T, server *Server, set []t13Lease) {
	t.Helper()
	for index, lease := range set {
		if _, _, code, err := server.enqueueReDeclare("/slice", lease.pin*mib, "pinned:client", t13Request(index, lease)); err != nil {
			t.Fatalf("SET %d: %s %v", index, code, err)
		}
	}
}

// verifies: AIRA-350 T13 — S after a restart and a reconnect is derived from the
// re-established leases, whatever their order. (a) An establish re-books every
// pin: S = 4194 − 990 − 512 + 100 + 768 = 3560 (the pin-1100 and the
// resized-from-4096 leases leave S; pin 1000 and resized-from-1024 re-book at
// their pins). (b) A SET drops only resizes: S = 4194 − 512 + 768 = 4450, the
// discounted leases keeping 990 and 900.
func TestSmallLaneT13RestartReconnect(t *testing.T) {
	set := t13Set()
	now := time.Unix(351400, 0)
	t.Run("before", func(t *testing.T) {
		server := t13Server(t, &now)
		queue := liveT13(t, server, set)
		requireS(t, server, queue, 4194*mib)
	})
	for _, order := range [][]int{{0, 1, 2, 3, 4, 5, 6, 7, 8, 9}, {9, 8, 7, 6, 5, 4, 3, 2, 1, 0}, {5, 3, 9, 0, 6, 2, 8, 1, 4, 7}} {
		t.Run(fmt.Sprintf("establish order %v", order), func(t *testing.T) {
			server := t13Server(t, &now)
			queue := establishT13(t, server, set, order)
			requireS(t, server, queue, 3560*mib)
		})
	}
	t.Run("reconnect SET", func(t *testing.T) {
		server := t13Server(t, &now)
		queue := liveT13(t, server, set)
		setT13(t, server, set)
		requireS(t, server, queue, 4450*mib)
		for _, waiter := range queue.waiters {
			if waiter.reserve == 1100*mib && waiter.ledgerCharge() != 990*mib || waiter.reserve == 1000*mib && waiter.ledgerCharge() != 900*mib {
				t.Fatalf("discounted lease pin %dM re-booked at %d", waiter.reserve/mib, waiter.ledgerCharge())
			}
		}
	})
}

// verifies: AIRA-350 T13c — "a restart only lowers S" is false. S 7500 (six
// 1024 small leases and a 200, the pin-1000 booked 900, the resized 1024→256,
// none of which leaves S on a re-declare; plus three main leases): a 256 small
// waiter is granted (7756). After a restart (establish: 7500 + 100 + 768 =
// 8368) it is full; after a reconnect (SET: 7500 + 768 = 8268) it is full.
func TestSmallLaneT13cRestartRaisesS(t *testing.T) {
	set := []t13Lease{
		{1024, 1024, false}, {1024, 1024, false}, {1024, 1024, false},
		{1024, 1024, false}, {1024, 1024, false}, {1024, 1024, false}, {200, 200, false},
		{1000, 900, true}, {1024, 256, false},
		{4096, 4096, false}, {6144, 6144, false}, {2048, 2048, false},
	}
	order := make([]int, len(set))
	for i := range order {
		order[i] = i
	}
	now := time.Unix(351500, 0)
	small := func(queue *sliceQueue) *admitWaiter {
		waiter := laneJob(100, 256*mib, now)
		queue.mu.Lock()
		queue.waiters = append(queue.waiters, waiter)
		queue.mu.Unlock()
		return waiter
	}
	t.Run("live", func(t *testing.T) {
		server := t13Server(t, &now)
		queue := liveT13(t, server, set)
		requireS(t, server, queue, 7500*mib)
		waiter := small(queue)
		server.evaluateAdmitQueue(queue)
		requireLaneGranted(t, waiter, true)
		requireLaneWait(t, waiter, "")
		requireS(t, server, queue, 7756*mib)
	})
	t.Run("after a restart", func(t *testing.T) {
		server := t13Server(t, &now)
		queue := establishT13(t, server, set, order)
		requireS(t, server, queue, 8368*mib)
		waiter := small(queue)
		server.evaluateAdmitQueue(queue)
		requireLaneWait(t, waiter, laneWaitFull)
	})
	t.Run("after a reconnect", func(t *testing.T) {
		server := t13Server(t, &now)
		queue := liveT13(t, server, set)
		setT13(t, server, set)
		requireS(t, server, queue, 8268*mib)
		waiter := small(queue)
		server.evaluateAdmitQueue(queue)
		requireLaneWait(t, waiter, laneWaitFull)
	})
}

// verifies: AIRA-350 T14 — a resize lowers the booking, and S follows the
// booking: a 1024 small lease resized to 256 lowers S by 768; a 4096 lease
// resized to 1024 becomes small and raises it by 1024. The booking is written as
// resizeRunningConfineLocked writes it (bookingField) and the ledger re-derived.
func TestSmallLaneT14Resize(t *testing.T) {
	now := time.Unix(351600, 0)
	current := int64(0)
	server := laneServer(t, laneE, &current, &now)
	small, large := laneLease(1, 1024*mib), laneLease(2, 4096*mib)
	queue := queueOf(server, small, large)
	requireS(t, server, queue, 1024*mib)

	small.charge = bookingField(256*mib, small.reserve)
	queue.outstanding, queue.cpuOutstanding, queue.vramOutstanding, queue.outstandingJobs = rederiveLedgerLocked(queue)
	requireS(t, server, queue, 256*mib)

	large.charge = bookingField(1024*mib, large.reserve)
	queue.outstanding, queue.cpuOutstanding, queue.vramOutstanding, queue.outstandingJobs = rederiveLedgerLocked(queue)
	requireS(t, server, queue, 1280*mib)
}

// verifies: AIRA-350 T15 — suite parents and unsigned jobs are not eligible
// (laneKind, frozen at enqueue); a granted parent of at most y counts in S, its
// worker does not and stays head-exempt.
func TestSmallLaneT15Parents(t *testing.T) {
	now := time.Unix(351700, 0)
	current := int64(0)
	server := laneServer(t, laneE, &current, &now)
	server.admitManualEvaluation = true
	enqueue := func(id int, signature string) *admitWaiter {
		request := admitRequest{reserve: 512 * mib, cpu: 1, scopeID: fmt.Sprintf("CONFINE-t15j%d-%d-a", id, 5000+id),
			name: fmt.Sprintf("t15j%d", id), owner: "a", signature: signature}
		_, waiter, code, err := server.enqueueAdmitInternal("/slice", 512*mib, "pinned:client", laneE, false, request)
		if err != nil {
			t.Fatalf("enqueue: %s %v", code, err)
		}
		return waiter
	}
	parent := enqueue(1, runner.AitestParentSignaturePrefix+"pytest")
	unsigned := enqueue(2, "")
	plain := enqueue(3, "make test")
	if server.laneEligibleLocked(parent) || server.laneEligibleLocked(unsigned) || !server.laneEligibleLocked(plain) {
		t.Fatalf("eligibility: parent %v unsigned %v plain %v, want false false true",
			server.laneEligibleLocked(parent), server.laneEligibleLocked(unsigned), server.laneEligibleLocked(plain))
	}

	grantedParent := laneLease(1, 512*mib)
	grantedParent.laneKind = false
	worker := laneLease(2, 512*mib)
	worker.parentScopeID = grantedParent.scopeID
	head := laneJob(3, 42000*mib, now) // refused (room 45056 − H(3) − 1024 = 41792), could fit empty (42944): the head
	queuedWorker := laneJob(4, 512*mib, now)
	queuedWorker.parentScopeID = grantedParent.scopeID
	queuedWorker.laneKind = false
	fresh := laneServer(t, laneE, &current, &now)
	queue := queueOf(fresh, grantedParent, worker, head, queuedWorker)
	spendBudget(queue, head, now)
	requireS(t, fresh, queue, 512*mib)

	fresh.evaluateAdmitQueue(queue)
	requireLaneGranted(t, head, false)
	requireLaneGranted(t, queuedWorker, true) // head-exempt, in main
	requireS(t, fresh, queue, 512*mib)
}

// verifies: AIRA-350 T16 — a scope-less, parentless waiter is not eligible: its
// real use is not capped by its reserve, so G's sum of pins would not bound it.
func TestSmallLaneT16ScopeLess(t *testing.T) {
	now := time.Unix(351800, 0)
	current := int64(0)
	server := laneServer(t, laneE, &current, &now)
	queue, _ := t7Fixture(t, server)
	head := laneJob(3, 4096*mib, now)
	scopeless := laneJob(4, 512*mib, now)
	scopeless.scopeID = ""
	addWaiters(queue, head, scopeless)
	spendBudget(queue, head, now)
	if server.laneEligibleLocked(scopeless) {
		t.Fatal("a scope-less waiter is eligible")
	}

	server.evaluateAdmitQueue(queue)
	requireLaneGranted(t, scopeless, false)
	requireLaneWait(t, scopeless, "")
	requireHeldByHead(t, scopeless, true)
}

// verifies: AIRA-350 T17 — ci-shim: no lane grant, and a lane configured in
// ci-shim mode is off everywhere it is read: the startup line, and the snapshot
// before the first admission (no `small-job lane:` line in CI).
func TestSmallLaneT17CIShim(t *testing.T) {
	now := time.Unix(351900, 0)
	current := int64(0)
	server := laneServer(t, laneE, &current, &now)
	server.confineMode = runner.ConfineModeShim
	queue, _ := t7Fixture(t, server)
	head := laneJob(3, 4096*mib, now)
	small := laneJob(4, 512*mib, now)
	addWaiters(queue, head, small)
	spendBudget(queue, head, now)

	server.evaluateAdmitQueue(queue)
	requireLaneGranted(t, small, false)
	requireLaneWait(t, small, "")

	// The lane stays configured (x 8G, y 1G); only the mode turns it off.
	if line := server.smallLaneStartupLine(); line != "aira daemon: small-job lane off" {
		t.Fatalf("shim startup line %q", line)
	}
	if snapshot := server.admitSliceSnapshotFor("/no-queue", ""); snapshot.present || snapshot.laneOn ||
		snapshot.laneCapacity != 0 || snapshot.laneJobMax != 0 {
		t.Fatalf("shim no-queue snapshot published the lane: %+v", snapshot)
	}
	server.confineMode = runner.ConfineModeReal
	if line := server.smallLaneStartupLine(); line != "aira daemon: small-job lane 8G, jobs booking at most 1G" {
		t.Fatalf("default startup line %q", line)
	}
}

// verifies: AIRA-350 T18 — off is today. With x = 0 nothing the lane would
// change is changed: past a head with its budget spent the small waiters are
// withheld and the hold arms (today's rule), no lane diagnosis is written, and
// degenerate queues pass unchanged.
func TestSmallLaneT18OffIsToday(t *testing.T) {
	for _, test := range []struct {
		name    string
		build   func(server *Server, now time.Time) (*sliceQueue, []*admitWaiter, []bool)
		bypass  int64
		armedAt bool
	}{
		{"head with budget spent", func(server *Server, now time.Time) (*sliceQueue, []*admitWaiter, []bool) {
			queue := queueOf(server, laneLease(1, 30000*mib), laneLease(2, 9816*mib))
			head := laneJob(3, 4096*mib, now)
			smalls := []*admitWaiter{laneJob(4, 512*mib, now), laneJob(5, 256*mib, now)}
			addWaiters(queue, head, smalls[0], smalls[1])
			head.bypassed = admitBypassBudget(head)
			return queue, []*admitWaiter{head, smalls[0], smalls[1]}, []bool{false, false, false}
		}, 2048 * mib, true},
		{"head with budget left", func(server *Server, now time.Time) (*sliceQueue, []*admitWaiter, []bool) {
			queue := queueOf(server, laneLease(1, 30000*mib), laneLease(2, 9816*mib))
			head := laneJob(3, 4096*mib, now)
			smalls := []*admitWaiter{laneJob(4, 512*mib, now), laneJob(5, 256*mib, now)}
			addWaiters(queue, head, smalls[0], smalls[1])
			return queue, []*admitWaiter{head, smalls[0], smalls[1]}, []bool{false, true, true}
		}, 768 * mib, false},
		{"empty queue", func(server *Server, now time.Time) (*sliceQueue, []*admitWaiter, []bool) {
			return queueOf(server), nil, nil
		}, 0, false},
		{"only leases", func(server *Server, now time.Time) (*sliceQueue, []*admitWaiter, []bool) {
			return queueOf(server, laneLease(1, 512*mib)), nil, nil
		}, 0, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			now := time.Unix(352000, 0)
			current := int64(0)
			server := laneServer(t, laneE, &current, &now)
			server.smallLaneCap, server.smallLaneJobMax = 0, 0
			queue, waiters, granted := test.build(server, now)

			server.evaluateAdmitQueue(queue)

			for i, waiter := range waiters {
				requireLaneGranted(t, waiter, granted[i])
				requireLaneWait(t, waiter, "")
			}
			if len(waiters) > 0 && waiters[0].bypassed != test.bypass {
				t.Fatalf("bypassed=%dM, want %dM", waiters[0].bypassed/mib, test.bypass/mib)
			}
			if queue.freezeArmedAt.IsZero() == test.armedAt {
				t.Fatalf("armed=%v, want %v", !queue.freezeArmedAt.IsZero(), test.armedAt)
			}
		})
	}
}

// verifies: AIRA-350 T19 — random grants, releases, establishes, SETs, resizes
// and worker grants with the lane on: after every step S equals an independent
// walk, and at every lane grant (the observer seam) S <= x, G held for the
// reference, and the one ceiling held.
func TestSmallLaneT19DerivedProperty(t *testing.T) {
	now := time.Unix(352100, 0)
	current := int64(0)
	server := laneServer(t, laneE, &current, &now)
	server.admitFreezeMaxHold = time.Minute
	rng := rand.New(rand.NewSource(350))
	laneGrants := 0
	server.smallLaneGrantObserver = func(queue *sliceQueue, granted, reference *admitWaiter, _ laneOccupancy, effectiveMaximum int64) {
		laneGrants++
		// Every check reads `after`, a walk of the queue taken here, after the
		// grant: never the occupancy the code under test hands the observer.
		after := smallLeasesLocked(queue, server.smallLaneJobMax)
		if after.booked > server.smallLaneCap {
			t.Fatalf("lane grant %d left S=%d above x", granted.seq, after.booked)
		}
		if reference != nil && reference != granted && !server.admitCouldFitBesideLocked(reference, effectiveMaximum, after) {
			t.Fatalf("lane grant %d past reference %d without G", granted.seq, reference.seq)
		}
		if queue.outstanding+server.admitSliceHeadroom(queue.outstandingJobs) > effectiveMaximum {
			t.Fatalf("lane grant %d broke the ceiling: %d + H(%d) > %d", granted.seq, queue.outstanding, queue.outstandingJobs, effectiveMaximum)
		}
	}
	queue := queueOf(server)
	seq := int64(0)
	sizes := []int64{64, 200, 512, 900, 1024, 1025, 2048, 6144, 12288, 30000}
	independentS := func() int64 {
		total := int64(0)
		for _, waiter := range queue.waiters {
			if waiter.state == admitGranted && waiter.accounted && waiter.parentScopeID == "" {
				booking := waiter.reserve
				if waiter.charge > 0 && waiter.charge < waiter.reserve {
					booking = waiter.charge
				}
				if booking <= server.smallLaneJobMax {
					total += booking
				}
			}
		}
		return total
	}
	granted := func() []*admitWaiter {
		var out []*admitWaiter
		for _, waiter := range queue.waiters {
			if waiter.state == admitGranted {
				out = append(out, waiter)
			}
		}
		return out
	}
	for step := 0; step < 3000; step++ {
		switch op := rng.Intn(10); {
		case op < 4: // a new waiter
			seq++
			waiter := laneJob(seq, sizes[rng.Intn(len(sizes))]*mib, now)
			if rng.Intn(5) == 0 {
				waiter.laneKind = false // a suite parent or unsigned job
			}
			if rng.Intn(6) == 0 {
				if parents := granted(); len(parents) > 0 {
					waiter.parentScopeID = parents[rng.Intn(len(parents))].scopeID // a worker
				}
			}
			if rng.Intn(8) == 0 {
				queue.seq++
				waiter.pushSeq = queue.seq
			}
			addWaiters(queue, waiter)
		case op < 6: // a release
			if leases := granted(); len(leases) > 0 {
				endLease(queue, leases[rng.Intn(len(leases))])
			}
		case op < 7: // a resize (lower the booking)
			if leases := granted(); len(leases) > 0 {
				lease := leases[rng.Intn(len(leases))]
				if lower := lease.ledgerCharge() / 2; lower >= mib {
					lease.charge = bookingField(lower, lease.reserve)
					queue.outstanding, queue.cpuOutstanding, queue.vramOutstanding, queue.outstandingJobs = rederiveLedgerLocked(queue)
				}
			}
		case op < 8: // an establish (books its pin, past the ceiling if need be)
			seq++
			lease := laneLease(seq, sizes[rng.Intn(len(sizes))]*mib)
			addWaiters(queue, lease)
		case op < 9: // a SET drops a resize
			if leases := granted(); len(leases) > 0 {
				lease := leases[rng.Intn(len(leases))]
				lease.charge = 0
				queue.outstanding, queue.cpuOutstanding, queue.vramOutstanding, queue.outstandingJobs = rederiveLedgerLocked(queue)
			}
		default:
			if rng.Intn(4) == 0 {
				queue.freezeArmedAt = now
			}
			server.evaluateAdmitQueue(queue)
			now = now.Add(time.Second)
		}
		if got, want := laneS(server, queue), independentS(); got != want {
			t.Fatalf("step %d: S=%d, independent walk %d", step, got, want)
		}
	}
	if laneGrants == 0 {
		t.Fatal("the walk made no lane grant: the property was never exercised")
	}
}

// verifies: AIRA-350 T20 — the boundaries are inclusive: S + w == x granted,
// one byte more full; a booking == y eligible, one byte more not.
func TestSmallLaneT20Boundaries(t *testing.T) {
	for _, test := range []struct {
		name  string
		extra int64
		full  bool
	}{{"S + w == x", 0, false}, {"one byte over", 1, true}} {
		t.Run(test.name, func(t *testing.T) {
			now := time.Unix(352200, 0)
			current := int64(0)
			server := laneServer(t, laneE, &current, &now)
			leases := []*admitWaiter{}
			for seq := int64(1); seq <= 7; seq++ {
				leases = append(leases, laneLease(seq, 1024*mib))
			}
			if test.extra > 0 {
				leases = append(leases, laneLease(9, test.extra)) // S = 7168 MiB + 1 byte
			}
			queue := queueOf(server, leases...)
			small := laneJob(10, 1024*mib, now)
			addWaiters(queue, small)
			server.evaluateAdmitQueue(queue)
			if test.full {
				requireLaneWait(t, small, laneWaitFull)
			} else {
				requireLaneWait(t, small, "")
				requireLaneGranted(t, small, true)
				requireS(t, server, queue, 8192*mib)
			}
		})
	}
	now := time.Unix(352210, 0)
	current := int64(0)
	server := laneServer(t, laneE, &current, &now)
	if !server.laneEligibleLocked(laneJob(1, 1024*mib, now)) || server.laneEligibleLocked(laneJob(2, 1024*mib+1, now)) {
		t.Fatal("y boundary: 1024M eligible, one byte more not")
	}
}

// verifies: AIRA-350 T21 — priority: freed room goes to the small waiter first,
// even past an earlier main waiter that fits. Room 1500: a 1400 main waiter,
// then a 1024 small waiter: the small one is granted (G against the 1400 is
// vacuous) and the main waiter (room 1500 − 1088 = 412) becomes the head.
func TestSmallLaneT21Priority(t *testing.T) {
	now := time.Unix(352300, 0)
	current := int64(0)
	server := laneServer(t, laneE, &current, &now)
	queue := queueOf(server, laneLease(1, (45056-2176-1500)*mib)) // 45056 − H(2) − 41380 = 1500
	if room := laneRoom(server, queue, laneE); room != 1500*mib {
		t.Fatalf("fixture room=%dM", room/mib)
	}
	main, small := laneJob(2, 1400*mib, now), laneJob(3, 1024*mib, now)
	addWaiters(queue, main, small)

	server.evaluateAdmitQueue(queue)

	requireLaneGranted(t, small, true)
	requireLaneGranted(t, main, false)
	if queue.freezeHolderSeq != main.seq {
		t.Fatalf("head seq=%d, want the main waiter %d", queue.freezeHolderSeq, main.seq)
	}
}

// verifies: AIRA-350 T22 — first fit inside the lane, no lane head-of-line:
// room 600, small waiters 1024 then 256: the 256 is granted, the 1024 no-room.
// The 1024, refused, is today's head; its budget is spent (hold), so only the
// lane can grant the 256 (with budget left today's walk would grant it too, and
// a lane that stopped at its first no-room waiter would pass unseen).
func TestSmallLaneT22FirstFit(t *testing.T) {
	now := time.Unix(352400, 0)
	current := int64(0)
	server := laneServer(t, laneE, &current, &now)
	queue := queueOf(server, laneLease(1, (45056-2176-600)*mib))
	if room := laneRoom(server, queue, laneE); room != 600*mib {
		t.Fatalf("fixture room=%dM", room/mib)
	}
	big, small := laneJob(2, 1024*mib, now), laneJob(3, 256*mib, now)
	addWaiters(queue, big, small)
	spendBudget(queue, big, now)

	server.evaluateAdmitQueue(queue)

	requireLaneGranted(t, small, true)
	requireLaneWait(t, small, "")
	requireLaneGranted(t, big, false)
	requireLaneWait(t, big, "")
	requireBlocked(t, big, "ram")
}

// verifies: AIRA-350 T23 — one room: a lane grant faces the main walk's whole
// fit, the AIRA-324 full-pin physical rule included. Enforce 90%, one 4096 main
// lease, a small discounted pin 1137 (booking 1023.3). (a) Real use 41780: room
// 45056 − H(2) − 41780 = 1100 for the ledger and the pin alike; the booking fits
// the ledger, the pin does not: no-room. (b) Real use 41743: 1137, granted at
// equality. (c) Over a table of states a lone small waiter is lane-granted
// exactly when the lane-off twin grants it.
func TestSmallLaneT23OneRoom(t *testing.T) {
	build := func(t *testing.T, current *int64, lane bool, mutate func(server *Server, lease, small *admitWaiter)) (*Server, *sliceQueue, *admitWaiter) {
		now := time.Unix(352500, 0)
		server := laneServer(t, laneE, current, &now)
		server.pinChargeMode, server.pinChargePercent = pinChargeEnforce, 90
		if !lane {
			server.smallLaneCap = 0
		}
		lease := laneLease(1, 4096*mib)
		small := pinWaiter(server, 2, 1137*mib, now)
		small.laneKind, small.cpu = true, 1
		if mutate != nil {
			mutate(server, lease, small)
		}
		return server, queueOf(server, lease, small), small
	}
	t.Run("pin does not fit the physical room", func(t *testing.T) {
		current := int64(41780) * mib
		server, queue, small := build(t, &current, true, nil)
		server.evaluateAdmitQueue(queue)
		requireLaneGranted(t, small, false)
		requireLaneWait(t, small, "")
		requireBlocked(t, small, "ram") // the full pin's physical room
	})
	t.Run("pin fits at equality", func(t *testing.T) {
		current := int64(41743) * mib
		server, queue, small := build(t, &current, true, nil)
		server.evaluateAdmitQueue(queue)
		requireLaneGranted(t, small, true)
		requireLaneWait(t, small, "")
	})
	t.Run("parity with the lane-off twin", func(t *testing.T) {
		type row struct {
			current, reclaimable int64
			cpuFull              bool
			vram                 int64
			discount             bool
		}
		var rows []row
		for _, current := range []int64{0, 30000, 41743, 41744, 41780, 43000} {
			for _, reclaimable := range []int64{0, 37} {
				for _, cpuFull := range []bool{false, true} {
					for _, vram := range []int64{0, 2048, 9000} {
						for _, discount := range []bool{true, false} {
							rows = append(rows, row{current, reclaimable, cpuFull, vram, discount})
						}
					}
				}
			}
		}
		for _, r := range rows {
			verdict := func(lane bool) bool {
				current := r.current * mib
				server, queue, small := build(t, &current, lane, func(server *Server, lease, small *admitWaiter) {
					server.admitReadMemory = func(string) (int64, int64, int64, bool, string) {
						return current, laneE, r.reclaimable * mib, true, ""
					}
					if r.cpuFull {
						lease.cpu = 64
					}
					small.vram = r.vram * mib
					if !r.discount {
						small.charge = 0
						small.reserve = 1000 * mib
					}
					server.vramSnap.Store(&vramSnapshot{total: 8192 * mib, free: 8192 * mib, evaluated: true, sampledAt: time.Unix(352500, 0)})
				})
				queue.outstanding, queue.cpuOutstanding, queue.vramOutstanding, queue.outstandingJobs = rederiveLedgerLocked(queue)
				server.evaluateAdmitQueue(queue)
				// AIRA-370: a refused lone small waiter carries no lane token, and
				// the main walk names exactly the resources that did not fit.
				if lane && small.state != admitGranted && (small.laneWait != "" || small.blocked == 0 || small.heldByHead) {
					t.Fatalf("row %+v: refused with laneWait %q blocked %v, want no lane token and a resource", r, small.laneWait, admitBlockedTokens(small.blocked, small.heldByHead))
				}
				if lane && r.cpuFull != (small.state != admitGranted && small.blocked&admitBlockedCPU != 0) {
					t.Fatalf("row %+v: cpu named=%v, want %v", r, small.blocked&admitBlockedCPU != 0, r.cpuFull)
				}
				if lane && small.state == admitGranted && small.laneWait != "" {
					t.Fatalf("row %+v: granted with laneWait %q", r, small.laneWait)
				}
				return small.state == admitGranted
			}
			if on, off := verdict(true), verdict(false); on != off {
				t.Fatalf("row %+v: lane on granted=%v, lane-off twin granted=%v", r, on, off)
			}
		}
	})
}

// verifies: AIRA-350 T24 — G guards only the reference (R2), and a kept waiter
// is still an ordinary main waiter. Main leases 2048 and 30768: room 45056 −
// H(3) − 32816 = 10000. Order: A 2048, B 36864, eight 1024 small.
//
// Pass 1: all eight lane-granted (S 8192; room 10000 − 8 x 1088 = 1296; G
// against A is vacuous); A refused, the head. Pass 2 (the 2048 lease ends: room
// 1296 + 2112 = 3408): A granted in main (back to 1296); B refused, the head.
// Pass 3 (one small lease ends: room 2384; a 512 small arrives; B's budget
// unspent, idle): B is the reference, the 512 kept (45056 − H(9) − 7680 = 34752
// < 36864), then granted by today's walk within B's budget (bypassed 512, S
// 7680).
func TestSmallLaneT24GuardMasked(t *testing.T) {
	now := time.Unix(352600, 0)
	current := int64(0)
	server := laneServer(t, laneE, &current, &now)
	ending := laneLease(1, 2048*mib)
	queue := queueOf(server, ending, laneLease(2, 30768*mib))
	if room := laneRoom(server, queue, laneE); room != 10000*mib {
		t.Fatalf("fixture room=%dM, want 10000M", room/mib)
	}
	a, b := laneJob(3, 2048*mib, now), laneJob(4, 36864*mib, now)
	addWaiters(queue, a, b)
	var smalls []*admitWaiter
	for seq := int64(5); seq <= 12; seq++ {
		smalls = append(smalls, laneJob(seq, 1024*mib, now))
	}
	addWaiters(queue, smalls...)

	server.evaluateAdmitQueue(queue)
	for _, small := range smalls {
		requireLaneGranted(t, small, true)
	}
	requireS(t, server, queue, 8192*mib)
	if room := laneRoom(server, queue, laneE); room != 1296*mib {
		t.Fatalf("pass 1 room=%dM, want 1296M", room/mib)
	}
	requireLaneGranted(t, a, false)
	requireLaneGranted(t, b, false)

	endLease(queue, ending)
	if room := laneRoom(server, queue, laneE); room != 3408*mib {
		t.Fatalf("pass 2 room=%dM, want 3408M", room/mib)
	}
	server.evaluateAdmitQueue(queue)
	requireLaneGranted(t, a, true)
	requireLaneGranted(t, b, false)

	endLease(queue, smalls[0])
	if room := laneRoom(server, queue, laneE); room != 2384*mib {
		t.Fatalf("pass 3 room=%dM, want 2384M", room/mib)
	}
	late := laneJob(13, 512*mib, now)
	addWaiters(queue, late)
	server.evaluateAdmitQueue(queue)
	requireLaneWait(t, late, laneWaitKept)
	requireLaneGranted(t, late, true)
	if b.bypassed != 512*mib {
		t.Fatalf("B.bypassed=%dM, want 512M", b.bypassed/mib)
	}
	requireS(t, server, queue, 7680*mib)
	if !queue.freezeArmedAt.IsZero() {
		t.Fatal("pass 3 armed the hold")
	}
}

// verifies: AIRA-350 T25 — the reference waiter.
func TestSmallLaneT25Reference(t *testing.T) {
	base := func(t *testing.T, now time.Time) (*Server, *sliceQueue) {
		current := int64(0)
		server := laneServer(t, laneE, &current, &now)
		queue := queueOf(server, laneLease(1, 1024*mib), laneLease(2, 1536*mib), laneLease(3, 1536*mib))
		if room := laneRoom(server, queue, laneE); room != 38656*mib {
			t.Fatalf("fixture room=%dM, want 38656M", room/mib)
		}
		return server, queue
	}
	t.Run("advances past its own lane grant", func(t *testing.T) {
		// (a) 512 small (the reference, granted), a 40960 waiter (the next
		// reference), 1024 small: kept, 45056 − H(4) − 2560 = 40192 < 40960.
		now := time.Unix(352700, 0)
		server, queue := base(t, now)
		first, large, second := laneJob(4, 512*mib, now), laneJob(5, 40960*mib, now), laneJob(6, 1024*mib, now)
		addWaiters(queue, first, large, second)
		server.evaluateAdmitQueue(queue)
		requireLaneGranted(t, first, true)
		requireLaneWait(t, second, laneWaitKept)
	})
	t.Run("never a sub-reservation", func(t *testing.T) {
		// (b) A queued 39000 worker (refused: room 38656), a 40960 waiter, 1024
		// small: kept against the 40960 (45056 − H(3) − 2048 = 40768).
		now := time.Unix(352710, 0)
		server, queue := base(t, now)
		worker, large, small := laneJob(4, 39000*mib, now), laneJob(5, 40960*mib, now), laneJob(6, 1024*mib, now)
		worker.parentScopeID = "CONFINE-parent-1-a"
		addWaiters(queue, worker, large, small)
		server.evaluateAdmitQueue(queue)
		requireLaneGranted(t, worker, false)
		requireLaneWait(t, small, laneWaitKept)
	})
	t.Run("only a waiter that could fit an empty slice", func(t *testing.T) {
		// (c) Throttled E 38912 (memory.max 45056), no leases: a 40960 waiter
		// cannot fit even empty (38912 − 2112 = 36800); the 1024 small is its own
		// reference and granted.
		now := time.Unix(352720, 0)
		current := int64(0)
		server := laneServer(t, laneE, &current, &now)
		server.publishSliceCeilingSnapshot(sliceCeilingSnapshot{
			Mode: sliceCeilingEnforce, SlicePath: "/slice",
			State: sliceCeilingThrottled, Ceiling: 38912 * mib, StaticMax: laneE,
		})
		large, small := laneJob(1, 40960*mib, now), laneJob(2, 1024*mib, now)
		queue := queueOf(server, large, small)
		server.evaluateAdmitQueue(queue)
		requireLaneGranted(t, small, true)
		requireLaneWait(t, small, "")
	})
	t.Run("never a gated waiter", func(t *testing.T) {
		// (d) An exclusive holder booking 2048; a non-holder 42000 waiter (gated);
		// a 1024 small carrying the holder's token: granted (G against the gated
		// 42000 would keep it: 45056 − H(2) − 1024 = 41856).
		now := time.Unix(352730, 0)
		current := int64(0)
		server := laneServer(t, laneE, &current, &now)
		holder := laneLease(1, 2048*mib)
		holder.exclusive = true
		gated, small := laneJob(2, 42000*mib, now), laneJob(3, 1024*mib, now)
		small.exclusiveHolder = holder.scopeID
		queue := queueOf(server, holder, gated, small)
		server.evaluateAdmitQueue(queue)
		requireLaneGranted(t, gated, false)
		requireLaneGranted(t, small, true)
		requireLaneWait(t, small, "")
	})
}

// verifies: AIRA-350 T25e — G is never judged against the waiter itself (the
// pseudo-code's ref != w). A resized small lease (pin 42000, booking 512) leaves
// a lone 1024 small waiter, its own reference, no room beside itself (45056 −
// H(3) − 43024 = −208), but it is granted: the room is the ledger's (45056 −
// H(2) − 512 = 42368). Mutation: G applied when w is the reference -> kept.
func TestSmallLaneT25eNoGuardAgainstItself(t *testing.T) {
	now := time.Unix(352740, 0)
	current := int64(0)
	server := laneServer(t, laneE, &current, &now)
	resized := laneLease(1, 42000*mib)
	resized.charge = 512 * mib
	small := laneJob(2, 1024*mib, now)
	queue := queueOf(server, resized, small)
	server.evaluateAdmitQueue(queue)
	requireLaneGranted(t, small, true)
	requireLaneWait(t, small, "")
}

// verifies: AIRA-350 T26 — while a queued, ungated sub-reservation fits, only
// today's walk runs. Room 1100 (a granted 2048 parent and a 39668 main lease:
// 45056 − H(3) − 41716 = 1100).
func TestSmallLaneT26SubReservation(t *testing.T) {
	build := func(t *testing.T, now time.Time) (*Server, *sliceQueue, *admitWaiter) {
		current := int64(0)
		server := laneServer(t, laneE, &current, &now)
		parent := laneLease(1, 2048*mib)
		queue := queueOf(server, parent, laneLease(2, 39668*mib))
		if room := laneRoom(server, queue, laneE); room != 1100*mib {
			t.Fatalf("fixture room=%dM, want 1100M", room/mib)
		}
		return server, queue, parent
	}
	worker := func(seq, reserve int64, parent *admitWaiter, now time.Time) *admitWaiter {
		waiter := laneJob(seq, reserve, now)
		waiter.parentScopeID = parent.scopeID
		return waiter
	}
	t.Run("a fitting worker skips the lane", func(t *testing.T) {
		now := time.Unix(352800, 0)
		server, queue, parent := build(t, now)
		w, small := worker(3, 1024*mib, parent, now), laneJob(4, 1024*mib, now)
		addWaiters(queue, w, small)
		server.evaluateAdmitQueue(queue)
		requireLaneGranted(t, w, true)
		requireLaneGranted(t, small, false)
		requireLaneWait(t, small, "")
	})
	t.Run("a worker that does not fit does not", func(t *testing.T) {
		// A 4096 head with its budget spent (hold) is queued first, so only the
		// lane can grant the small waiter: with no head today's walk would grant
		// it too, and a skip for ANY queued sub-reservation would pass unseen.
		now := time.Unix(352810, 0)
		server, queue, parent := build(t, now)
		head, w, small := laneJob(3, 4096*mib, now), worker(4, 2048*mib, parent, now), laneJob(5, 1024*mib, now)
		addWaiters(queue, head, w, small)
		spendBudget(queue, head, now)
		server.evaluateAdmitQueue(queue)
		requireLaneGranted(t, small, true)
		requireLaneWait(t, small, "")
		requireLaneGranted(t, w, false)
		requireLaneGranted(t, head, false)
	})
	t.Run("a fitting worker anywhere in the order", func(t *testing.T) {
		now := time.Unix(352820, 0)
		server, queue, parent := build(t, now)
		head, small, w := laneJob(3, 4096*mib, now), laneJob(4, 1024*mib, now), worker(5, 1024*mib, parent, now)
		addWaiters(queue, head, small, w)
		spendBudget(queue, head, now)
		server.evaluateAdmitQueue(queue)
		requireLaneGranted(t, small, false)
		requireHeldByHead(t, small, true)
		requireLaneWait(t, small, "")
		requireLaneGranted(t, w, true)
	})
	t.Run("a worker that fits RAM but has no CPU slot does not", func(t *testing.T) {
		// The main lease takes 61 cores: 1 + 61 = 62 of 64 slots booked, so the
		// 3-core worker fits the 1100 of RAM room but no CPU slot; the 1-core
		// small waiter fits both. A skip on RAM alone would leave the small
		// waiter held behind the spent head. The worker is a sub-reservation,
		// never lane-eligible: no lane diagnosis.
		now := time.Unix(352830, 0)
		server, queue, parent := build(t, now)
		queue.waiters[1].cpu = 61
		queue.outstanding, queue.cpuOutstanding, queue.vramOutstanding, queue.outstandingJobs = rederiveLedgerLocked(queue)
		if free := cpuAvailable(server.cpuCeiling(), queue.cpuOutstanding); free != 2 {
			t.Fatalf("fixture CPU slots free=%d, want 2", free)
		}
		head, w, small := laneJob(3, 4096*mib, now), worker(4, 1024*mib, parent, now), laneJob(5, 1024*mib, now)
		w.cpu = 3
		addWaiters(queue, head, w, small)
		spendBudget(queue, head, now)
		server.evaluateAdmitQueue(queue)
		requireLaneGranted(t, small, true)
		requireLaneWait(t, small, "")
		requireLaneGranted(t, w, false)
		requireLaneWait(t, w, "")
		requireLaneGranted(t, head, false)
	})
	t.Run("a gated worker that fits does not", func(t *testing.T) {
		// An exclusive holder (2048) and another parent (2048) beside a 37556
		// main lease: room 45056 − H(4) − 41652 = 1100. The other parent's
		// fitting worker is gated (only the holder's own sub-reservations pass);
		// the head and the small waiter carry the holder's token, so neither is.
		// A skip that ignored the gate would leave the small waiter held.
		now := time.Unix(352840, 0)
		current := int64(0)
		server := laneServer(t, laneE, &current, &now)
		holder, other := laneLease(1, 2048*mib), laneLease(2, 2048*mib)
		holder.exclusive = true
		queue := queueOf(server, holder, other, laneLease(3, 37556*mib))
		if room := laneRoom(server, queue, laneE); room != 1100*mib {
			t.Fatalf("fixture room=%dM, want 1100M", room/mib)
		}
		head, w, small := laneJob(4, 4096*mib, now), worker(5, 1024*mib, other, now), laneJob(6, 1024*mib, now)
		head.exclusiveHolder, small.exclusiveHolder = holder.scopeID, holder.scopeID
		addWaiters(queue, head, w, small)
		spendBudget(queue, head, now)
		if gate := exclusiveGateLocked(queue); !gate.blocks(queue, w) || gate.blocks(queue, small) || gate.blocks(queue, head) {
			t.Fatal("fixture: the worker must be gated, the head and the small waiter not")
		}
		server.evaluateAdmitQueue(queue)
		requireLaneGranted(t, small, true)
		requireLaneWait(t, small, "")
		requireLaneGranted(t, w, false)
		requireLaneGranted(t, head, false)
	})
}

// verifies: AIRA-350 §2.1, §2.6, through the real enqueue path
// (enqueueResolvedConfineAdmit, pushQueuedConfine, releaseAdmitWaiterLocked)
// and the real snapshot (admitSliceSnapshotFor), over five passes: the lane's
// configuration is published before the first admission (the `small-job lane:`
// line of `confine --list`), each pass's diagnoses replace the last pass's
// (cleared, not merely overwritten), and S and J_s follow the grants and
// releases. Mutations: the no-queue snapshot drops the lane fields; the per-pass
// clearing deleted, or clearing only one of laneWait / heldByHead.
func TestSmallLaneRealPathMultiPass(t *testing.T) {
	now := time.Unix(353100, 0)
	current := int64(0)
	server := laneServer(t, laneE, &current, &now)
	server.admitManualEvaluation = true
	const path = "/slice"
	enqueue := func(name string, pid int, reserve int64, exclusive bool) *admitWaiter {
		t.Helper()
		request := admitRequest{scopeID: reserveScopeID(t, name, pid), name: name, owner: "session-a",
			signature: "sig-" + name, cpu: 1, peerSameUID: true, exclusive: exclusive}
		_, waiter, code, err := server.enqueueResolvedConfineAdmit(path, reserve, "pinned:client", laneE, request)
		if err != nil {
			t.Fatalf("enqueue %s: %s %v", name, code, err)
		}
		return waiter
	}
	pass := func() {
		t.Helper()
		queue := server.admitQueue
		server.evaluateAdmitQueue(queue)
		now = now.Add(time.Second)
	}
	release := func(waiter *admitWaiter) {
		t.Helper()
		queue := server.admitQueue
		queue.mu.Lock()
		released := releaseAdmitWaiterLocked(queue, waiter)
		queue.mu.Unlock()
		if !released {
			t.Fatalf("release of %s", waiter.scopeID)
		}
	}
	diagnoses := func(waiter *admitWaiter, laneWait string, held bool) {
		t.Helper()
		snapshot := server.admitSliceSnapshotFor(path, waiter.scopeID)
		if snapshot.queuedLaneWait != laneWait || snapshot.queuedHeldByHead != held {
			t.Fatalf("probe diagnoses %q %v, want %q %v", snapshot.queuedLaneWait, snapshot.queuedHeldByHead, laneWait, held)
		}
		for _, entry := range snapshot.queuedEntries {
			if entry.ScopeID == waiter.scopeID && (entry.LaneWait != laneWait || entry.HeldByHead != held) {
				t.Fatalf("queued row diagnoses %q %v, want %q %v", entry.LaneWait, entry.HeldByHead, laneWait, held)
			}
		}
	}
	occupancy := func(used int64, jobs int) admitSnapshot {
		t.Helper()
		snapshot := server.admitSliceSnapshotFor(path, "")
		if !snapshot.present || !snapshot.laneOn || snapshot.laneCapacity != 8192*mib || snapshot.laneUsed != used || snapshot.laneJobs != jobs {
			t.Fatalf("lane figures present=%v on=%v x=%dM S=%dM J=%d, want S=%dM J=%d", snapshot.present, snapshot.laneOn,
				snapshot.laneCapacity/mib, snapshot.laneUsed/mib, snapshot.laneJobs, used/mib, jobs)
		}
		return snapshot
	}

	// Before the first admission: no queue, so no ledger (present false, S
	// unevaluated), but the lane's configuration is published.
	if snapshot := server.admitSliceSnapshotFor(path, ""); snapshot.present || !snapshot.laneOn ||
		snapshot.laneCapacity != 8192*mib || snapshot.laneJobMax != 1024*mib || snapshot.laneUsed != 0 {
		t.Fatalf("no-queue snapshot %+v, want the lane's x and y with no ledger", snapshot)
	}

	// Pass 1: a 30000 main job and seven 1024 small jobs; all fit
	// (45056 − H(8) − 37168 = 5328 >= 0), S = 7168 over 7 jobs.
	mainJob := enqueue("mainjob", 6001, 30000*mib, false)
	var smalls []*admitWaiter
	for i := 0; i < 7; i++ {
		smalls = append(smalls, enqueue(fmt.Sprintf("smalljob%d", i), 6010+i, 1024*mib, false))
	}
	pass()
	requireLaneGranted(t, mainJob, true)
	for _, small := range smalls {
		requireLaneGranted(t, small, true)
	}
	snapshot := occupancy(7168*mib, 7)
	if !snapshot.scopeSmall[smalls[0].scopeID] || snapshot.scopeSmall[mainJob.scopeID] {
		t.Fatalf("small marks %v", snapshot.scopeSmall)
	}

	// Pass 2: an 8192 head, pushed, refused (room 45056 − H(9) − 37168 = 5264),
	// and a 1024 small job that fits: the lane is closed behind the pushed
	// reference ("pushed") and the walk holds it behind the head.
	head := enqueue("headjob", 6020, 8192*mib, false)
	x := enqueue("xjob", 6021, 1024*mib, false)
	if _, err := server.pushQueuedConfine(path, head.scopeID, "session-a", false); err != nil {
		t.Fatal(err)
	}
	pass()
	requireLaneGranted(t, head, false)
	requireLaneGranted(t, x, false)
	diagnoses(x, laneWaitPushed, true)

	// Pass 3: an exclusive request drains the slice, so the lane skips x and the
	// walk never reaches its head rule: neither writes a diagnosis, and the
	// pass-2 pair must be gone.
	exclusive := enqueue("exclusivejob", 6022, 512*mib, true)
	pass()
	requireLaneGranted(t, x, false)
	requireLaneGranted(t, exclusive, false)
	diagnoses(x, "", false)

	// Pass 4: the exclusive request ends; pass 2's diagnoses are written again.
	release(exclusive)
	pass()
	diagnoses(x, laneWaitPushed, true)

	// Pass 5: the main job ends; the pushed head fits and is granted, then x in
	// the walk behind no head. S = 8192 over 8 jobs.
	release(mainJob)
	pass()
	requireLaneGranted(t, head, true)
	requireLaneGranted(t, x, true)
	snapshot = occupancy(8192*mib, 8)
	if !snapshot.scopeSmall[x.scopeID] || snapshot.scopeSmall[head.scopeID] {
		t.Fatalf("small marks %v", snapshot.scopeSmall)
	}
}

// verifies: AIRA-350 §2.5 — the daemon's configuration: off, empty and unset are
// off; 8G is on with y 1G; a job maximum is honoured only when the lane is on;
// abc, 0, x < y and y = 0 refuse to start the daemon with E_CONFIG_INVALID.
func TestSmallLaneConfigFromEnv(t *testing.T) {
	for _, test := range []struct {
		lane, jobMax string
		capacity     int64
		max          int64
		invalid      bool
	}{
		{"", "", 0, 0, false},
		{"off", "", 0, 0, false},
		{"off", "garbage", 0, 0, false},
		{"8G", "", 8 << 30, 1 << 30, false},
		{"8G", "512M", 8 << 30, 512 << 20, false},
		{"1G", "1G", 1 << 30, 1 << 30, false},
		{"abc", "", 0, 0, true},
		{"0", "", 0, 0, true},
		{"512M", "", 0, 0, true}, // x < the default y
		{"4G", "8G", 0, 0, true}, // x < y
		{"8G", "0", 0, 0, true},
		{"8G", "512K", 0, 0, true},
		{"8G", "lots", 0, 0, true},
	} {
		t.Run(test.lane+"/"+test.jobMax, func(t *testing.T) {
			t.Setenv("AIRA_DAEMON_SMALL_LANE", test.lane)
			t.Setenv("AIRA_DAEMON_SMALL_LANE_JOB_MAX", test.jobMax)
			capacity, jobMax, err := smallLaneConfigFromEnv()
			if test.invalid {
				if err == nil || !containsCode(err.Error(), "E_CONFIG_INVALID") {
					t.Fatalf("err=%v, want E_CONFIG_INVALID", err)
				}
				return
			}
			if err != nil || capacity != test.capacity || jobMax != test.max {
				t.Fatalf("got %d %d %v, want %d %d", capacity, jobMax, err, test.capacity, test.max)
			}
		})
	}
}

func containsCode(message, code string) bool {
	return len(message) >= len(code) && message[:len(code)] == code
}

// verifies: AIRA-350 §2.6 — one journal line per lane grant, with S and J_s
// after it.
func TestSmallLaneGrantJournalLine(t *testing.T) {
	now := time.Unix(352900, 0)
	current := int64(0)
	server := laneServer(t, laneE, &current, &now)
	small := laneJob(1, 512*mib, now.Add(-1500*time.Millisecond))
	queue := queueOf(server, laneLease(2, 256*mib), small)
	lines := captureLaneLog(t, func() { server.evaluateAdmitQueue(queue) })
	want := fmt.Sprintf("aira daemon: small-lane grant seq=1 reserve=%d booking=%d waited=1500 lane=%d/%d jobs=2", 512*mib, 512*mib, 768*mib, 8192*mib)
	if !containsLine(lines, want) {
		t.Fatalf("journal %q lacks %q", lines, want)
	}
}

// captureDaemonLog runs fn with the standard logger writing to a buffer, and
// returns the lines it wrote.
func captureLaneLog(t *testing.T, fn func()) []string {
	t.Helper()
	var buffer bytes.Buffer
	previousOutput, previousFlags := log.Writer(), log.Flags()
	log.SetOutput(&buffer)
	log.SetFlags(0)
	defer func() { log.SetOutput(previousOutput); log.SetFlags(previousFlags) }()
	fn()
	return strings.Split(strings.TrimSpace(buffer.String()), "\n")
}

func containsLine(lines []string, want string) bool {
	for _, line := range lines {
		if line == want {
			return true
		}
	}
	return false
}

// verifies: AIRA-350 §2.6 — the snapshot publishes the lane only while it is on:
// x, y, S and J_s, each small scope, and each queued waiter's (and the probe's
// own) last-pass diagnoses, from the same locked walk; with the lane off every
// one is absent. Mutations: a field published while off; S taken from pins; the
// small mark on a large or sub-reservation lease.
func TestSmallLaneSnapshotFields(t *testing.T) {
	for _, lane := range []bool{true, false} {
		now := time.Unix(353000, 0)
		current := int64(0)
		server := laneServer(t, laneE, &current, &now)
		if !lane {
			server.smallLaneCap, server.smallLaneJobMax = 0, 0
		}
		small := laneLease(1, 1137*mib)
		small.charge = 1000 * mib // discounted: S counts the booking
		large := laneLease(2, 30000*mib)
		worker := laneLease(3, 512*mib)
		worker.parentScopeID = large.scopeID
		head := laneJob(4, 12000*mib, now) // room 45056 − H(4) − 31512 = 11240: refused
		waiter := laneJob(5, 512*mib, now)
		queue := queueOf(server, small, large, worker, head, waiter)
		spendBudget(queue, head, now)
		// Lane full for the waiter: S 1000 + 512 is far below x, so make it full
		// by a cap just below that.
		if lane {
			server.smallLaneCap = 1400 * mib
		}
		server.admitQueue = queue
		server.evaluateAdmitQueue(queue)

		snapshot := server.admitSliceSnapshotFor("/slice", waiter.scopeID)
		if !lane {
			if snapshot.laneOn || snapshot.laneCapacity != 0 || snapshot.laneUsed != 0 || len(snapshot.scopeSmall) != 0 ||
				snapshot.queuedLaneWait != "" || snapshot.queuedHeldByHead {
				t.Fatalf("lane off published lane fields: %+v", snapshot)
			}
			for _, entry := range snapshot.queuedEntries {
				if entry.LaneWait != "" || entry.HeldByHead {
					t.Fatalf("lane off published a queued diagnosis: %+v", entry)
				}
			}
			continue
		}
		if !snapshot.laneOn || snapshot.laneCapacity != 1400*mib || snapshot.laneJobMax != 1024*mib ||
			snapshot.laneUsed != 1000*mib || snapshot.laneJobs != 1 {
			t.Fatalf("lane figures %+v", snapshot)
		}
		if !snapshot.scopeSmall[small.scopeID] || snapshot.scopeSmall[large.scopeID] || snapshot.scopeSmall[worker.scopeID] {
			t.Fatalf("small marks %v", snapshot.scopeSmall)
		}
		if snapshot.queuedLaneWait != laneWaitFull || !snapshot.queuedHeldByHead {
			t.Fatalf("probe diagnoses %q %v, want full and held", snapshot.queuedLaneWait, snapshot.queuedHeldByHead)
		}
		found := false
		for _, entry := range snapshot.queuedEntries {
			if entry.ScopeID == waiter.scopeID {
				found = entry.LaneWait == laneWaitFull && entry.HeldByHead
			}
		}
		if !found {
			t.Fatalf("queued row diagnoses missing: %+v", snapshot.queuedEntries)
		}
	}
}

// verifies: AIRA-350 §2.6 — the confine-list handler carries the lane to the
// wire: the reply's lane fields, each small scope row's mark, and the probe's
// own lane diagnoses, only while the lane is on. Mutations: drop the reply's
// lane fields, the ApplyConfineScopeSmall call, or the probe's diagnoses ->
// red; publish any of them with the lane off -> red.
func TestSmallLaneConfineListReply(t *testing.T) {
	const gi = int64(1) << 30
	for _, lane := range []bool{true, false} {
		slice := t.TempDir()
		server := NewServer(Paths{})
		server.admitResolveSlice = func(string) (string, bool, string) { return slice, true, "" }
		server.admitReadMemory = func(string) (int64, int64, int64, bool, string) { return 2 * gi, 64 * gi, 0, true, "" }
		if lane {
			server.smallLaneCap, server.smallLaneJobMax = 8*gi, gi
		}
		// Before the first admission there is no queue: the reply still carries
		// x and y (the `small-job lane:` line), with the ledger unevaluated.
		if before := reserveListing(t, server).SliceReserve; before == nil || before.GrantedEstablished ||
			(lane && (before.LaneCapacityBytes != 8*gi || before.LaneJobMaxBytes != gi)) ||
			(!lane && (before.LaneCapacityBytes != 0 || before.LaneJobMaxBytes != 0)) {
			t.Fatalf("lane=%v no-queue reply %+v", lane, before)
		}
		small, large := reserveScopeID(t, "smalljob", 5301), reserveScopeID(t, "largejob", 5302)
		queuedID := reserveScopeID(t, "queuedjob", 5303)
		reserveScopeDir(t, slice, small, gi/2, gi/4)
		reserveScopeDir(t, slice, large, 4*gi, gi)
		queue := &sliceQueue{path: slice, server: server}
		queue.waiters = []*admitWaiter{
			{seq: 1, reserve: gi / 2, state: admitGranted, accounted: true, grantedCh: make(chan struct{}), scopeID: small, name: "smalljob", owner: "session-a"},
			{seq: 2, reserve: 4 * gi, state: admitGranted, accounted: true, grantedCh: make(chan struct{}), scopeID: large, name: "largejob", owner: "session-a"},
			{seq: 3, reserve: gi / 4, state: admitQueued, grantedCh: make(chan struct{}), scopeID: queuedID, name: "queuedjob", owner: "session-a",
				laneWait: laneWaitKept, heldByHead: true},
		}
		queue.mu.Lock()
		queue.outstanding, queue.cpuOutstanding, queue.vramOutstanding, queue.outstandingJobs = rederiveLedgerLocked(queue)
		queue.mu.Unlock()
		registerAdmitQueue(server, queue)

		listing := reserveListing(t, server)
		reserve := listing.SliceReserve
		if reserve == nil {
			t.Fatal("no slice reserve in the reply")
		}
		smallRow, largeRow := reserveScopeByID(t, listing, small), reserveScopeByID(t, listing, large)
		request := reserveListRequest()
		request.Args["scope_id"] = queuedID
		probe, ok := server.confineManagement(context.Background(), request).Data.(runner.ConfineListResult)
		if !ok || probe.SliceReserve == nil {
			t.Fatal("probe reply carries no slice reserve")
		}
		if !lane {
			if reserve.LaneCapacityBytes != 0 || reserve.LaneJobMaxBytes != 0 || reserve.LaneUsedBytes != 0 || reserve.LaneJobs != 0 ||
				smallRow.Small || probe.SliceReserve.LaneWait != "" || probe.SliceReserve.HeldByHead {
				t.Fatalf("lane off published lane fields: %+v small=%v probe=%+v", reserve, smallRow.Small, probe.SliceReserve)
			}
			continue
		}
		if reserve.LaneCapacityBytes != 8*gi || reserve.LaneJobMaxBytes != gi || reserve.LaneUsedBytes != gi/2 || reserve.LaneJobs != 1 {
			t.Fatalf("lane fields %d %d %d %d", reserve.LaneCapacityBytes, reserve.LaneJobMaxBytes, reserve.LaneUsedBytes, reserve.LaneJobs)
		}
		if !smallRow.Small || largeRow.Small {
			t.Fatalf("small marks: small row %v, large row %v", smallRow.Small, largeRow.Small)
		}
		if probe.SliceReserve.LaneWait != laneWaitKept || !probe.SliceReserve.HeldByHead {
			t.Fatalf("probe diagnoses %q %v", probe.SliceReserve.LaneWait, probe.SliceReserve.HeldByHead)
		}
	}
}
