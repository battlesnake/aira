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
// It is deliberately NOT a scheduler: it is one more denial condition on the
// verb that already decides "may I add one worker?", it sizes itself from
// desiredCPUSlots (a capacity it once shared with the daemon scheduler AIRA-33
// deleted, and now owns outright), and it derives its live count from the cgroup
// tree rather than from connections, because the tree is what survives a daemon
// restart and what a killed relay cannot falsify.

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
// THE SCAN RUNS WITHOUT cpuSlotsMu HELD (Sol build-review, P0). Holding the
// mutex across a full cgroup walk meant a speculative (max_wait_ms == 0)
// request could still block behind ANOTHER job's slow scan — the try-acquire on
// the two locks bought nothing, because this mutex sat in front of them on the
// cached path. The mutex now guards only map access. The cost is that two
// callers can scan the same root concurrently; that is a duplicated readdir,
// not a correctness problem, and the cache collapses it immediately afterwards.
func (s *Server) cpuSlotsSnapshotFor(root string, force bool) (cpuSlotsSnapshot, error) {
	interval := s.cpuSlotsScanInterval
	if interval <= 0 {
		interval = admitConfineScanIntervalDefault
	}
	now := s.admitNowTime()
	s.cpuSlotsMu.Lock()
	entry, found := s.cpuSlotsCache[root]
	s.cpuSlotsMu.Unlock()
	if found && !force && now.Sub(entry.at) < interval {
		return entry.snapshot, entry.err
	}
	scan := s.cpuSlotsScan
	if scan == nil {
		scan = scanSliceWorkerScopes
	}
	snapshot, err := scan(root)
	s.cpuSlotsMu.Lock()
	if s.cpuSlotsCache == nil {
		s.cpuSlotsCache = map[string]cpuSlotsCacheEntry{}
	}
	// at records the last ATTEMPT, successful or not, so a failing filesystem
	// does not turn every poll into another walk.
	s.cpuSlotsCache[root] = cpuSlotsCacheEntry{snapshot: snapshot, err: err, at: now}
	s.cpuSlotsMu.Unlock()
	return snapshot, err
}

// cpuSlotsFloorGrace is the window in which this daemon refuses a SECOND
// liveness-floor grant to the same outer scope.
func (s *Server) cpuSlotsFloorGrace() time.Duration {
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
// lastGrant is when this daemon last granted a worker under this outer scope.
// It is the ONLY thing closing the window between the daemon creating a scope
// and the client placing into it: during that window the scope reads
// unpopulated, so a populated-only floor would let N supervisors under one
// outer scope each take a floor grant (Sol plan-review round 2).
//
// An earlier build closed that window with a directory-age gate instead. Two
// successive reviews falsified its justification — first the claim that mtime
// is creation time (a child-cgroup mkdir moves it), then the salvage that an
// abandoned scope has no process able to create children (any process with
// write access can, member or not). Daemon-owned state has neither problem: it
// cannot be perturbed from outside the daemon, needs no timestamp semantics,
// and closes the same window with one comparison.
//
// After a daemon restart lastGrant is zero, which permits exactly one extra
// floor grant per outer scope and nothing worse.
func (s *Server) cpuSlotsDecide(outerScope string, lastGrant time.Time, force bool) (cpuSlotsVerdict, string) {
	root, scopeKey, ok, reason := s.cpuSlotsLocate(outerScope)
	if !ok {
		return cpuSlotsUnevaluated, reason
	}
	snapshot, err := s.cpuSlotsSnapshotFor(root, force)
	if err != nil {
		return cpuSlotsUnevaluated, "slice worker scopes unreadable: " + err.Error()
	}
	s.cpuSlotsMu.Lock()
	capacity := s.cpuSlotsCapacity
	s.cpuSlotsMu.Unlock()
	grace := s.cpuSlotsFloorGrace()
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
	if snapshot.liveForFloor[scopeKey] == 0 && s.admitNowTime().Sub(lastGrant) >= grace {
		return cpuSlotsAdmitFloor, ""
	}
	return cpuSlotsSaturated, ""
}

// cpuSlotsLocate resolves one outer scope to (the slice to scan, the key the
// scan will file that scope under).
//
// It is ONE function because two callers must agree: cpuSlotsDecide looks a
// scope up in the snapshot, and cpuSlotsInvalidate drops that snapshot from the
// cache. Deriving the path twice, slightly differently, is how a cache gets
// invalidated under one key and read under another.
//
// The derived root is a candidate until the admission slice resolver confirms
// it is a real directory inside the cgroup2 mount. Without that, a caller naming
// `/anywhere/.aira-CONFINE-x` would have the gate count an unrelated directory —
// find nothing there, and admit freely (Sol build-review, P1). The resolver may
// also CANONICALISE the path, which is exactly why the scope key is rebuilt
// from the resolved root rather than from the caller's spelling.
func (s *Server) cpuSlotsLocate(outerScope string) (root, scopeKey string, ok bool, reason string) {
	candidate, resolvedScope, ok := cpuSlotsScanRoot(outerScope)
	if !ok {
		return "", "", false, "outer scope " + outerScope + " is not a confine scope under a slice"
	}
	resolve := s.sliceResolver()
	resolved, resolvedOK, resolveReason := resolve(candidate)
	if !resolvedOK {
		return "", "", false, "slice for " + outerScope + " could not be resolved: " + resolveReason
	}
	// scanSliceWorkerScopes files each scope under filepath.Join(root,
	// entry.Name()), so the key must be built the same way from the same root.
	return resolved, filepath.Join(resolved, filepath.Base(resolvedScope)), true, ""
}

// THERE IS DELIBERATELY NO cpuSlotsInvalidate HERE, unlike the RAM ledger's
// workerScopeState.invalidate, and the asymmetry is the point.
//
// An earlier build had one, by analogy. Mutation testing then showed deleting it
// changed nothing observable, and working out why showed it never could: the
// cache is only ever read by the CHEAP path, whose sole power is to DENY early.
// A stale-low cached total therefore cannot cause a wrong admission — it can
// only fail to deny, sending the request on to the grant path, which forces a
// fresh scan under the gate before deciding anything. That forced scan also
// re-caches, so the cache self-heals after at most one extra poll.
//
// So invalidation bought exactly one avoided scan per grant, in exchange for a
// second path that had to derive the same cache key as cpuSlotsLocate and stay
// in agreement with it forever. Deleting it removes that whole class of bug and
// leaves the correctness guarantee where it actually lives: EVERY GRANT IS
// PRECEDED BY A FORCED FRESH SCAN (pinned by
// TestCPUGateGrantForcesAFreshSnapshotAndSeesAStaleLowCache).

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
