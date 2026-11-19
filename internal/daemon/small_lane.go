package daemon

import (
	"fmt"
	"log"
	"os"
	"strings"
	"time"

	"aira/internal/runner"
)

// AIRA-350. The small-job priority lane, inside the ONE slice ceiling (spec
// docs/superpowers/specs/2026-11-18-fixed-small-lane-design.md, round 6).
//
// Jobs booking at most y (smallLaneJobMax, default 1 GiB) are granted ahead of
// larger queued jobs, while the small leases' bookings stay within x
// (smallLaneCap). There is no second pool and no stored pool: the lane's
// occupancy S is DERIVED by a walk of the waiters (smallLeasesLocked), like the
// ledger itself, and every lane grant faces exactly the main walk's fit
// (admitFitLocked) against the one ceiling. The lane only reorders who gets the
// room that exists.
//
// Its one protection is guard G: a lane grant past the reference waiter (the
// waiter today's walk would make the head, or grant) needs the reference to fit
// a slice holding only the small leases and the new grant
// (admitCouldFitBesideLocked). G bounds the room small leases can take from a
// head; it does not make small-job turnover help the head (residual R1).
//
// Off (x = 0, the daemon's raw default) and in ci-shim mode the pre-pass does
// nothing, so every admission decision is today's (spec I8).

// laneWait tokens: why the lane did not grant a queued waiter in the last pass.
// Diagnosis only (AIRA-149 terms); "" means the lane did not refuse it.
const (
	laneWaitFull   = "full"
	laneWaitKept   = "kept"
	laneWaitPushed = "pushed"
	laneWaitNoRoom = "no-room"
)

// smallLaneConfigFromEnv reads AIRA_DAEMON_SMALL_LANE (x) and, only when the lane
// is on, AIRA_DAEMON_SMALL_LANE_JOB_MAX (y), on the pinChargeConfigFromEnv
// pattern. Unset, empty or "off" is off; anything else that is not a valid pair
// (runner.ParseSmallLane) is E_CONFIG_INVALID and the daemon does not start.
func smallLaneConfigFromEnv() (capacity, jobMax int64, err error) {
	capacity, jobMax, problem := runner.ParseSmallLane(os.Getenv("AIRA_DAEMON_SMALL_LANE"), os.Getenv("AIRA_DAEMON_SMALL_LANE_JOB_MAX"))
	switch problem {
	case "":
		return capacity, jobMax, nil
	case "capacity":
		return 0, 0, fmt.Errorf("E_CONFIG_INVALID: AIRA_DAEMON_SMALL_LANE must be off or a size from the job maximum to %s, such as 8G", runner.FormatConfineBytes(runner.SmallLaneMaxBytes))
	default:
		return 0, 0, fmt.Errorf("E_CONFIG_INVALID: AIRA_DAEMON_SMALL_LANE_JOB_MAX must be a size from 1M to the lane size (AIRA_DAEMON_SMALL_LANE), such as 1G")
	}
}

// smallLaneStartupLine is the daemon's startup report of the lane: off whenever
// smallLaneOn is, so a lane configured in ci-shim mode reports off.
func (s *Server) smallLaneStartupLine() string {
	if !s.smallLaneOn() {
		return "aira daemon: small-job lane off"
	}
	return fmt.Sprintf("aira daemon: small-job lane %s, jobs booking at most %s",
		runner.FormatConfineBytes(s.smallLaneCap), runner.FormatConfineBytes(s.smallLaneJobMax))
}

// smallLaneOn is laneOn: a configured lane, never in ci-shim mode (an advisory
// CI ledger gets no priority lane). The one off switch: every reader of the
// configured pair goes through it.
func (s *Server) smallLaneOn() bool {
	return s.smallLaneCap > 0 && !s.shimMode()
}

// laneKindOf is the enqueue-time half of eligibility (admitWaiter.laneKind): a
// signed request that is not a --delegate-ram suite parent. Every signed parent
// carries runner.AitestParentSignaturePrefix; an unsigned job is excluded too,
// since the daemon has no other parent signal.
func laneKindOf(signature string) bool {
	return signature != "" && !strings.HasPrefix(signature, runner.AitestParentSignaturePrefix)
}

// laneOccupancy is the small leases' footprint, derived by a walk, never stored:
// booked (S, their ledger bookings), pins (S_pin, their granted reserves), cpu,
// vram and jobs (J_s).
type laneOccupancy struct {
	booked int64
	pins   int64
	cpu    int64
	vram   int64
	jobs   int
}

// plus is this occupancy with one more lease, the waiter the lane is about to
// grant.
func (o laneOccupancy) plus(waiter *admitWaiter) laneOccupancy {
	o.booked = addClamp(o.booked, waiter.ledgerCharge())
	o.pins = addClamp(o.pins, waiter.reserve)
	o.cpu = addClamp(o.cpu, waiter.cpu)
	o.vram = addClamp(o.vram, waiter.vram)
	o.jobs = addJobCountClamp(o.jobs, 1)
	return o
}

// smallLease is the derived classification: a granted, accounted, job-level
// lease booking at most jobMax, however it was granted (lane, main, budget,
// backstop, re-declare), suite parents and scope-less leases included.
// Sub-reservations (suite workers, confine-reserve holds) are never small.
func smallLease(waiter *admitWaiter, jobMax int64) bool {
	return waiter != nil && waiter.state == admitGranted && waiter.accounted &&
		!waiter.isSubReservation() && waiter.ledgerCharge() <= jobMax
}

// smallLeasesLocked walks the waiters for the small leases' occupancy. Cached
// nowhere: "derived, never incremented", like rederiveLedgerLocked. queue.mu must
// be held.
func smallLeasesLocked(queue *sliceQueue, jobMax int64) laneOccupancy {
	var occ laneOccupancy
	for _, waiter := range queue.waiters {
		if smallLease(waiter, jobMax) {
			occ = occ.plus(waiter)
		}
	}
	return occ
}

// laneEligibleLocked is whether the lane may consider this queued waiter: the
// lane is on, and the waiter is scoped, non-exclusive, job-level, the lane's
// kind (laneKind) and books at most y. queue.mu must be held.
func (s *Server) laneEligibleLocked(waiter *admitWaiter) bool {
	return s.smallLaneOn() && waiter != nil && waiter.state == admitQueued &&
		!waiter.isSubReservation() && !waiter.exclusive && waiter.scopeID != "" &&
		waiter.laneKind && waiter.ledgerCharge() <= s.smallLaneJobMax
}

// smallLanePassLocked is the lane's grant-only pre-pass (spec §2.2), run by
// evaluateAdmitQueue after the fail-fast trip, the slice read, the restart freeze
// and the exclusivity gate are derived, and before today's walk. queue.mu must be
// held.
//
// Each eligible waiter, in today's evaluation order, is granted on today's fit
// alone, with no head rule, while the small leases' bookings stay within x and
// guard G holds for the reference waiter. A waiter it does not grant is left to
// today's walk as an ordinary waiter (room, head rule, budget, hold, backstop).
// Precedence (I7): a fitting sub-reservation (skip the pass), exclusivity gate,
// pushed reference, lane cap, fit, G.
//
// It writes no head bookkeeping: grantWaiterLocked never touches bypassed,
// budgetWithheld or the fairness anchor (I5).
func (s *Server) smallLanePassLocked(queue *sliceQueue, gate exclusiveGate, current, effectiveMaximum, reclaimable int64, now time.Time) {
	if !s.smallLaneOn() {
		return
	}
	order := admitOrderLocked(queue)
	// While any queued, ungated sub-reservation (a suite worker, a confine-reserve
	// hold) fits now, wherever it is in the order, only today's walk runs: the
	// lane never takes room a sub-reservation would get today (R6, T26).
	for _, waiter := range order {
		if waiter.isSubReservation() && !gate.blocks(queue, waiter) &&
			s.admitFitLocked(queue, waiter, current, effectiveMaximum, reclaimable).all() {
			return
		}
	}
	// The reference waiter: the first waiter from index k on that is still
	// queued, job-level, ungated and could fit an empty slice — the one today's
	// walk would make the head if it does not fit, or grant if it does. It
	// advances only when the lane grants it (T25).
	nextRef := func(k int) int {
		for index := k; index < len(order); index++ {
			waiter := order[index]
			if waiter.state == admitQueued && !waiter.isSubReservation() && !gate.blocks(queue, waiter) &&
				s.admitCouldFitBesideLocked(waiter, effectiveMaximum, laneOccupancy{}) {
				return index
			}
		}
		return len(order)
	}
	reference := nextRef(0)
	for index, waiter := range order {
		if !s.laneEligibleLocked(waiter) || gate.blocks(queue, waiter) {
			continue
		}
		var ref *admitWaiter
		if reference < len(order) {
			ref = order[reference]
		}
		// AIRA-309: behind a pushed reference the lane is closed; only another
		// push passes a pushed head. A pushed reference may itself use the lane.
		if ref != nil && ref != waiter && ref.pushSeq > 0 {
			waiter.laneWait = laneWaitPushed
			continue
		}
		occ := smallLeasesLocked(queue, s.smallLaneJobMax)
		if addClamp(occ.booked, waiter.ledgerCharge()) > s.smallLaneCap {
			waiter.laneWait = laneWaitFull
			continue
		}
		fit := s.admitFitLocked(queue, waiter, current, effectiveMaximum, reclaimable)
		if !fit.all() {
			waiter.laneWait = laneWaitNoRoom
			continue
		}
		occ = occ.plus(waiter)
		if ref != nil && ref != waiter && !s.admitCouldFitBesideLocked(ref, effectiveMaximum, occ) {
			waiter.laneWait = laneWaitKept
			continue
		}
		s.grantWaiterLocked(queue, waiter, fit.physicalFree)
		s.logSmallLaneGrantLocked(waiter, occ, now)
		if s.smallLaneGrantObserver != nil {
			s.smallLaneGrantObserver(queue, waiter, ref, occ, effectiveMaximum)
		}
		if index == reference {
			reference = nextRef(index + 1)
		}
	}
}

// logSmallLaneGrantLocked is the journal line of one lane grant. queue.mu must
// be held.
func (s *Server) logSmallLaneGrantLocked(waiter *admitWaiter, occ laneOccupancy, now time.Time) {
	log.Printf("aira daemon: small-lane grant seq=%d reserve=%d booking=%d waited=%d lane=%d/%d jobs=%d",
		waiter.seq, waiter.reserve, waiter.ledgerCharge(), elapsedMilliseconds(waiter.enqueued, now),
		occ.booked, s.smallLaneCap, occ.jobs)
}
