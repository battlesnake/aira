//go:build linux

package daemon

import (
	"context"
	"log"
	"path/filepath"
	"time"
)

// AIRA-64. The CPU-concurrency gate.
//
// aitest bounds per-worker RAM but had no CPU governance at all, so N concurrent
// pytest runs each sized their pool at NumCPU and put N*NumCPU heavy workers on
// NumCPU cores. Wall-clock pytest-timeout then misfires on tests that are merely
// starved, reporting phantom failures in untouched code. This gate bounds the
// concurrency instead of reinterpreting the symptom.
//
// It is deliberately NOT a second scheduler: it is one more denial condition on
// the verb that already decides "may I add one worker?", it reads the same
// capacity concept the governor uses (desiredCPUSlots), and it derives its live
// count from the cgroup tree rather than from connections, because the tree is
// what survives a daemon restart and what a killed relay cannot falsify.

// cpuSlotsVerdict is what evaluateWorkerAdmit acts on.
type cpuSlotsVerdict uint8

const (
	// cpuSlotsAdmitNormal: the slice is under capacity.
	cpuSlotsAdmitNormal cpuSlotsVerdict = iota
	// cpuSlotsAdmitFloor: the slice is at or over capacity, but this outer
	// scope holds no live worker at all, so the liveness floor admits it
	// anyway. The caller must record the grant so the floor cannot be claimed
	// again inside one grace window.
	cpuSlotsAdmitFloor
	// cpuSlotsSaturated: retriable denial.
	cpuSlotsSaturated
	// cpuSlotsUnevaluated: the reading could not be established. Admission
	// proceeds on the RAM decision alone and the condition is surfaced, never
	// rendered as a zero and never as a pass.
	cpuSlotsUnevaluated
)

type cpuSlotsCacheEntry struct {
	snapshot cpuSlotsSnapshot
	err      error
	at       time.Time
}

// cpuSlotsSnapshotFor returns a slice's snapshot, refreshing the cache when
// force is set or the entry is older than the scan interval.
//
// The two-phase cached/forced split mirrors workerCommitted exactly, and for
// the same reasons. The cadence bound is load-bearing (the AIRA-61 precedent: a
// per-poll O(tree) sweep produced 25-65% supervisor CPU): evaluateWorkerAdmit
// runs once per 200ms poll per waiter, so the contended DENIAL path reads the
// cache. force is passed only on the path about to GRANT — at most once per
// admitted worker, never per poll — because a cached total can be stale-LOW
// exactly there (a worker scope created since the last scan is invisible to the
// cache). Everywhere else a stale cache is stale-HIGH, which only ever denies.
//
// A failed scan is cached exactly like a successful one, by replaying its
// error. Without that, a failing filesystem would turn every poll into another
// full walk — the AIRA-61 shape, on a filesystem already misbehaving.
func (s *Server) cpuSlotsSnapshotFor(root string, force bool) (cpuSlotsSnapshot, error) {
	interval := s.cpuSlotsScanInterval
	if interval <= 0 {
		interval = admitConfineScanIntervalDefault
	}
	now := s.admitNowTime()
	s.cpuSlotsMu.Lock()
	defer s.cpuSlotsMu.Unlock()
	if entry, found := s.cpuSlotsCache[root]; found && !force && now.Sub(entry.at) < interval {
		return entry.snapshot, entry.err
	}
	scan := s.cpuSlotsScan
	if scan == nil {
		scan = scanSliceWorkerScopes
	}
	snapshot, err := scan(root, now, s.cpuSlotsGraceLocked())
	if s.cpuSlotsCache == nil {
		s.cpuSlotsCache = map[string]cpuSlotsCacheEntry{}
	}
	// at records the last ATTEMPT, successful or not.
	s.cpuSlotsCache[root] = cpuSlotsCacheEntry{snapshot: snapshot, err: err, at: now}
	return snapshot, err
}

// cpuSlotsGraceLocked returns the placement grace. Caller holds cpuSlotsMu.
func (s *Server) cpuSlotsGraceLocked() time.Duration {
	if s.cpuSlotsGrace > 0 {
		return s.cpuSlotsGrace
	}
	return cpuSlotsPlacementGraceDefault
}

// acquireCPUSlotsGate serialises the [fresh scan -> decide -> create] critical
// section machine-wide, so two concurrent grants under DIFFERENT outer scopes
// cannot both observe capacity-1 and both grant.
//
// LOCK ORDER IS `outer-scope lock -> this gate`, NEVER the reverse. Acquiring
// this first would let one slow outer scope — whose own lock is held across a
// cgroupfs scan and a scope creation — pin admission for the whole machine, the
// hazard workerScopeState.lock's own comment records for admitSlots. Nothing
// acquires an outer-scope lock while holding this gate, so no cycle exists.
//
// tryOnly is set for a SPECULATIVE request (max_wait_ms == 0), which must never
// wait on another job's critical section: it reports busy immediately instead.
func (s *Server) acquireCPUSlotsGate(ctx context.Context, tryOnly bool) bool {
	if s.cpuSlotsGate == nil {
		return true
	}
	// A FREE gate is always taken, without consulting ctx. That ordering is
	// deliberate and was found by running the full suite: acquireWorkerScope
	// checks ctx before its own select, and copying that here added a SECOND
	// cancellation checkpoint between the RAM decision and CreateWorkerScope.
	// It made TestWorkerAdmitConnectionKeepsScopeChargedWhenResponseWriteFails
	// flaky -- that test pins AIRA-41's invariant that a grant whose response
	// write fails still leaves its scope charged, and a peer that vanishes
	// mid-evaluate (net.Pipe with the client already closed) would now abort
	// before the scope existed. Taking a free gate unconditionally leaves the
	// pre-existing race exactly as wide as it was, rather than widening it.
	//
	// A plain three-way select would not do: it picks uniformly among READY
	// cases, so a free gate plus a cancelled ctx would still abort about half
	// the time. Try first, then wait.
	select {
	case s.cpuSlotsGate <- struct{}{}:
		return true
	default:
	}
	if tryOnly {
		// Speculative: never wait on a lock another job holds.
		return false
	}
	// Contended. Waiting here is what must be abandonable -- a vanished peer or
	// a stopping daemon must not sit behind another job's critical section.
	select {
	case s.cpuSlotsGate <- struct{}{}:
		return true
	case <-ctx.Done():
		return false
	case <-s.stopping:
		return false
	}
}

func (s *Server) releaseCPUSlotsGate() {
	if s.cpuSlotsGate == nil {
		return
	}
	select {
	case <-s.cpuSlotsGate:
	default:
	}
}

// cpuSlotsDecide is the whole gate policy, in the order that makes each
// direction fail safely.
//
// lastFloorGrant is this outer scope's previous floor grant, used for the
// once-per-grace-window rate limit. The limit exists because the placement
// grace runs from SCOPE CREATION while the client's own ack timer starts only
// after its fork begins: a supervisor stalled past the grace lets its directory
// age out while its grant is still live, and without the limit a second
// supervisor under the same outer scope would become floor-eligible again, once
// per window, without bound (Sol plan-review round 3).
func (s *Server) cpuSlotsDecide(outerScope string, lastFloorGrant time.Time, force bool) (cpuSlotsVerdict, string) {
	root, ok := cpuSlotsScanRoot(outerScope)
	if !ok {
		return cpuSlotsUnevaluated, "outer scope " + outerScope + " is not a confine scope under a slice"
	}
	snapshot, err := s.cpuSlotsSnapshotFor(root, force)
	if err != nil {
		return cpuSlotsUnevaluated, "slice worker scopes unreadable: " + err.Error()
	}
	s.cpuSlotsMu.Lock()
	capacity := s.cpuSlotsCapacity
	grace := s.cpuSlotsGraceLocked()
	s.cpuSlotsMu.Unlock()
	if capacity < 1 {
		// Never silently treat an unestablished capacity as "no room" (which
		// would stall every run) nor as "infinite room" (which would make the
		// gate inert). Report it as what it is.
		return cpuSlotsUnevaluated, "cpu slot capacity is not established"
	}
	if snapshot.total < capacity {
		return cpuSlotsAdmitNormal, ""
	}
	// The liveness floor. Without it this whole change is a REGRESSION: a job
	// arriving while another holds every slot would get zero workers and stall
	// until that run finished, where today it merely runs slowly. Slow must
	// never become stalled.
	if snapshot.liveForFloor[filepath.Clean(outerScope)] == 0 &&
		s.admitNowTime().Sub(lastFloorGrant) >= grace {
		return cpuSlotsAdmitFloor, ""
	}
	return cpuSlotsSaturated, ""
}

// cpuSlotsInvalidate forces the next read for this outer scope's slice to
// rescan. Called ONLY on the daemon's own mutation of the tree (a successful
// worker-scope creation) — never on an externally observed change, exactly as
// workerScopeState.invalidate is.
func (s *Server) cpuSlotsInvalidate(outerScope string) {
	root, ok := cpuSlotsScanRoot(outerScope)
	if !ok {
		return
	}
	s.cpuSlotsMu.Lock()
	delete(s.cpuSlotsCache, root)
	s.cpuSlotsMu.Unlock()
}

// cpuSlotsWarnOnce logs an unestablished CPU reading once per outer scope. The
// same condition is also carried to the client on the granted outcome line
// (cpu_slots=unevaluated), because a daemon-journal-only warning is how a
// governance subsystem ships operationally inert — which this project has done
// once already (the AIRA-59 watchdog).
func (s *Server) cpuSlotsWarnOnce(outerScope, detail string) {
	s.cpuSlotsMu.Lock()
	if s.cpuSlotsWarned == nil {
		s.cpuSlotsWarned = map[string]struct{}{}
	}
	_, seen := s.cpuSlotsWarned[outerScope]
	if !seen {
		s.cpuSlotsWarned[outerScope] = struct{}{}
	}
	s.cpuSlotsMu.Unlock()
	if seen {
		return
	}
	log.Printf("aira daemon: worker-admit CPU governance unevaluated for %s (%s); admitting on the RAM decision alone", outerScope, detail)
}
