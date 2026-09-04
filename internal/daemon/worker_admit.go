package daemon

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"aira/internal/core"
	"aira/internal/runner"
)

// workerAdmitHeadroomDefault is a SEPARATE, much smaller headroom than
// admitSliceHeadroomBase (2 GiB, sized for the whole machine-wide slice,
// admit.go). Reusing the slice-wide constant here would swallow most of a
// realistically-sized outer scope's own cap in production. This is a
// build-time tunable, not yet sized from field data — a reasonable small
// fixed default for Slice 1.
const workerAdmitHeadroomDefault int64 = 64 << 20 // 64 MiB

// workerAdmitEstimatedBytesMin matches --memory-reserve's minimum
// (cmd/aira/main.go), so a sub-page estimate can never floor memory.max to
// zero pages and instant-OOM the worker on placement.
const workerAdmitEstimatedBytesMin int64 = 1 << 20 // 1 MiB

// WorkerAdmitResponse is the one grant/denial payload the worker-admit
// connection sends before optionally holding itself open as the lease.
//
// AIRA-42: State, Class and Reason are all drawn from the single vocabulary in
// internal/runner (runner.WorkerAdmitState*/Class*/Reason*), never spelled as
// literals here. Class is the load-bearing field — the disposition the
// supervisor acts on — and it replaced the "reject:"/"fallback:" reason-string
// prefix convention that used to carry that meaning. Reason is a stable
// exact-match token; Detail is free text that NOTHING parses.
type WorkerAdmitResponse struct {
	State string `json:"state"`
	Class string `json:"class"`
	// Reason is an exact-match token from the runner vocabulary. It is
	// diagnostic: no consumer branches on it, and none may.
	Reason string `json:"reason,omitempty"`
	// Detail elaborates for a human. It carries cgroup paths and raw file
	// contents, i.e. operator-controlled text, which is exactly why nothing
	// classifies from it.
	Detail     string `json:"detail,omitempty"`
	WaitedMS   int64  `json:"waited_ms"`
	WorkerID   string `json:"worker_id,omitempty"`
	ScopePath  string `json:"scope_path,omitempty"`
	MemoryMax  int64  `json:"memory_max,omitempty"`
	MemoryHigh int64  `json:"memory_high,omitempty"`
}

type workerAdmitRequest struct {
	jobID      string
	outerScope string
	// signature is accepted on the wire (the key spec 3.3 names for a
	// future per-suite peak-history-based cap-sizing backstop) but UNUSED
	// for anything in Slice 1 — deferred past Slice 1; estimatedBytes
	// alone governs the backstop cap for now (see also Task 17's
	// _resolve_estimated_bytes, which states the same deferral on the
	// Python side).
	signature      string
	estimatedBytes int64
	maxWaitMS      int64
}

// workerScopeChildPrefix is the directory-name prefix every aitest worker
// scope carries under its outer scope. It is the whole membership test for the
// ledger: `.aira-supervisor` (deliberately uncapped, charged separately by the
// aggregate guard's supervisor-RSS term) and any non-AIRA child are excluded
// by construction.
const workerScopeChildPrefix = ".aira-worker-"

// workerScopeChildren is one cgroupfs-derived reading of an outer scope's
// worker children. AIRA-39: this, not an in-memory grants map, is the
// worker-admit ledger — it survives a daemon restart because the kernel object
// it sums IS the state.
type workerScopeChildren struct {
	// committed is Σ memory.max over every `.aira-worker-*` child. Every such
	// child is charged whatever its suffix looks like: CreateWorkerScope
	// accepts any slashless id (runner.WorkerScopeChildPath), so charging only
	// the numeric ones would silently under-count an externally created or
	// oddly named worker scope — the exact direction this fix exists to close.
	committed int64
	// maxIndex is the largest NUMERIC suffix seen, and only feeds worker-id
	// allocation. A non-numeric or out-of-range suffix contributes nothing
	// here while still being charged above; the two concerns are deliberately
	// separate (found by Sol plan-review).
	maxIndex int
	count    int
}

// workerScopeState is the per-OUTER-SCOPE ledger cell. Keyed by outer scope
// alone, not by (job_id, outer_scope): the committed sum is now taken over the
// scope's real children, so it already covers every job that placed a worker
// there, and the old workerScopeOwner binding (which existed only because the
// sum was per-job) is deleted with it. The lock must then be per outer scope
// or two jobs sharing one scope would evaluate and create under two different
// locks and both grant against the same pre-create sum.
//
// Not pruned, same accepted slow-growth gap as the map it replaces — now one
// entry per outer scope rather than one per (job_id, outer_scope) pair.
type workerScopeState struct {
	// lock is a 1-buffered channel rather than a sync.Mutex so a waiter can
	// abandon it when its peer disconnects or the daemon stops. The previous
	// job.mu was documented as "uninterruptible and not itself deadline-aware";
	// with a cgroupfs scan and a scope creation now inside the critical
	// section, and with worker-admit consuming a shared admitSlots token while
	// it waits (AIRA-63), an uninterruptible wait would let one slow outer
	// scope pin admission slots that ordinary `aira confine` admission needs.
	lock           chan struct{}
	outerScopePath string
	nextSeq        int
	committed      int64
	maxIndex       int
	committedAt    time.Time // last scan ATTEMPT, successful or not
	scanned        bool
	// scanErr is the last attempt's failure, held for the same interval a
	// successful sum is. Without it the throttle below is defeated on exactly
	// the path that most needs it: `scanned` is false after a failure, so every
	// poll would walk the tree again -- the AIRA-61 CPU-regression shape, on a
	// filesystem already misbehaving. Replaying the error (rather than the last
	// good sum) keeps the answer honestly "unevaluated" instead of silently
	// reverting to a stale number. Found independently by Sol and DeepSeek
	// build-review.
	scanErr error
}

// workerScopeScanIntervalDefault throttles the per-outer-scope child scan to
// the same <=1/second cadence evaluateAdmitQueue already uses for the
// slice-wide adopted-confine scan (admitConfineScanIntervalDefault).
const workerScopeScanIntervalDefault = admitConfineScanIntervalDefault

// maxWorkerScopeSeq bounds worker-id allocation so a reconstructed maxIndex
// near the int limit can never wrap into a colliding low id. Reaching it is a
// terminal create failure, not a silent wrap.
const maxWorkerScopeSeq = 1 << 30

// workerScopeFor returns the ledger cell for outerScope, creating it
// atomically under workerScopesMu so two concurrent first callers can never
// end up with two cells (or a nil lock channel).
func (s *Server) workerScopeFor(outerScope string) *workerScopeState {
	s.workerScopesMu.Lock()
	defer s.workerScopesMu.Unlock()
	if s.workerScopes == nil {
		s.workerScopes = make(map[string]*workerScopeState)
	}
	state := s.workerScopes[outerScope]
	if state == nil {
		state = &workerScopeState{lock: make(chan struct{}, 1), outerScopePath: outerScope}
		s.workerScopes[outerScope] = state
	}
	return state
}

// acquireWorkerScope takes state's lock, or reports false when the caller's
// context is done or the daemon is stopping. The ONLY statement permitted
// after a true return is `defer state.release()`.
func (s *Server) acquireWorkerScope(ctx context.Context, state *workerScopeState) bool {
	// Checked BEFORE the select below, because select picks uniformly at random
	// among ready cases: with an already-cancelled peer AND a free lock, the bare
	// select took the lock about half the time and went on to create a worker
	// scope for a peer that was already gone (CreateWorkerScope does not observe
	// ctx). That orphan scope then charges the ledger until job teardown. This
	// makes the already-cancelled case deterministic; it narrows but cannot close
	// the race against a peer that vanishes mid-acquire, whose residue is the
	// same safe over-charge (Sol build-review round 2).
	select {
	case <-ctx.Done():
		return false
	case <-s.stopping:
		return false
	default:
	}
	select {
	case state.lock <- struct{}{}:
		return true
	case <-ctx.Done():
		return false
	case <-s.stopping:
		return false
	}
}

func (state *workerScopeState) release() { <-state.lock }

// invalidate forces the next committed read to rescan. Called ONLY on the
// daemon's own mutation of the tree (a successful create) and on the one
// interleaving that proves the cache is stale-LOW (an EEXIST collision) —
// never on an externally observed change.
func (state *workerScopeState) invalidate() {
	state.committedAt = time.Time{}
	state.scanned = false
	state.scanErr = nil
}

// scanWorkerScopeChildren sums memory.max over outerScope's `.aira-worker-*`
// children. Fail-closed in the one direction that matters: it never reports a
// smaller total than the tree actually holds. Any reading it cannot establish
// is an error, which the caller turns into "unevaluated" — never a zero.
func scanWorkerScopeChildren(outerScope string) (workerScopeChildren, error) {
	entries, err := os.ReadDir(outerScope)
	if err != nil {
		return workerScopeChildren{}, fmt.Errorf("read worker scopes: %w", err)
	}
	return sumWorkerScopeChildren(outerScope, entries)
}

// sumWorkerScopeChildren is the half of the scan that runs AFTER the directory
// listing. Split out so a test can hand it an entry naming a child that is
// already gone — the benign _retire_worker race — which cannot be constructed
// deterministically through os.ReadDir.
func sumWorkerScopeChildren(outerScope string, entries []os.DirEntry) (workerScopeChildren, error) {
	var children workerScopeChildren
	for _, entry := range entries {
		if !entry.IsDir() || !strings.HasPrefix(entry.Name(), workerScopeChildPrefix) {
			continue
		}
		child := filepath.Join(outerScope, entry.Name())
		data, err := os.ReadFile(filepath.Join(child, "memory.max"))
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				// ENOENT alone does NOT prove the child vanished (Sol
				// plan-review): an existing directory with no memory.max means
				// the memory controller is not delegated, which is an anomaly
				// and must not be silently skipped. Re-stat to tell the two
				// apart. A genuinely vanished child is the benign race with
				// supervisor.py's _retire_worker rmdir and is skipped: it is
				// gone, so it charges nothing, which is correct rather than an
				// under-count.
				if _, statErr := os.Stat(child); errors.Is(statErr, fs.ErrNotExist) {
					continue
				}
				return workerScopeChildren{}, fmt.Errorf("worker scope %s has no memory.max (memory controller not delegated)", child)
			}
			return workerScopeChildren{}, fmt.Errorf("worker scope %s: read memory.max: %w", child, err)
		}
		value, valid := parseAdmitMemory(data)
		if !valid {
			// "max" (uncapped), malformed, or negative. Unreachable in the
			// normal flow — the daemon writes and verifies every worker cap
			// itself — so this is a genuine anomaly, and a fabricated zero
			// here is precisely the AIRA-39 failure.
			return workerScopeChildren{}, fmt.Errorf("worker scope %s: memory.max is not a finite byte count (%q)", child, strings.TrimSpace(string(data)))
		}
		children.committed = addClamp(children.committed, value)
		children.count++
		if index, err := strconv.Atoi(strings.TrimPrefix(entry.Name(), workerScopeChildPrefix)); err == nil && index > children.maxIndex {
			children.maxIndex = index
		}
	}
	return children, nil
}

// workerCommitted returns the outer scope's committed total, refreshing the
// cached scan when it is older than the scan interval or when force is set.
// Caller holds state's lock.
//
// The cadence bound is load-bearing (AIRA-61 precedent: a per-poll O(tree)
// sweep produced 25-65% supervisor CPU before it was fixed). evaluateWorkerAdmit
// runs once per 200ms poll per waiter, so the contended DENIAL path reads the
// cache and scans at most once per interval. force is passed only on the path
// that is about to GRANT — at most once per admitted worker, never per poll —
// because a cached sum may never be the basis of an admission: a
// `.aira-worker-*` child that appeared since the last scan is invisible to the
// cache and need not collide with the id being allocated, so the cache can be
// stale-LOW exactly there (found by Sol plan-review). Everywhere else a stale
// cache is stale-HIGH, which only ever denies.
func (s *Server) workerCommitted(state *workerScopeState, force bool) (int64, error) {
	interval := s.workerScopeScanInterval
	if interval <= 0 {
		interval = workerScopeScanIntervalDefault
	}
	now := s.admitNowTime()
	if !force && !state.committedAt.IsZero() && now.Sub(state.committedAt) < interval {
		// A FAILED attempt is throttled exactly like a successful one, by
		// replaying its error. Gating this on `scanned` (as it first did) meant a
		// failing scan cleared the flag and every subsequent poll rescanned,
		// which is the per-poll O(tree) walk the cadence bound exists to prevent.
		if state.scanErr != nil {
			return 0, state.scanErr
		}
		if state.scanned {
			return state.committed, nil
		}
	}
	scan := s.workerScopeScan
	if scan == nil {
		scan = scanWorkerScopeChildren
	}
	children, err := scan(state.outerScopePath)
	// committedAt records the last ATTEMPT, successful or not, so a failing
	// filesystem does not turn every poll into another scan.
	state.committedAt = now
	if err != nil {
		state.scanned, state.scanErr = false, err
		return 0, err
	}
	state.committed, state.maxIndex, state.scanned, state.scanErr = children.committed, children.maxIndex, true, nil
	return children.committed, nil
}

// workerScopesUnreadableDetail builds the human detail for a scan the daemon
// could not establish.
//
// It used to be workerScopesUnreadableReason, and it used to MANGLE the token
// "unbounded" into "un-bounded" before returning. That was a defensive hack
// against the prose channel: the detail embeds cgroup paths (which carry the
// operator's own `aira confine --name`) and raw memory.max file contents, and
// supervisor.py's substring classifier disabled daemon-backed admission
// outright — the WHOLE pytest suite UNCONFINED on this RAM-capped shared
// machine — for any "worker-admit unevaluated" message that merely CONTAINED
// the literal token "unbounded". A job named "unbounded-suite", or a corrupt
// memory.max echoed back through %q, was read as a genuinely uncapped outer
// scope (found independently by Sol and DeepSeek build-review on AIRA-39).
//
// AIRA-42 deletes the mangling, as AIRA-39's own comment here predicted it
// would: the outer-scope-unbounded condition is now carried by an exact-match
// reason token in its own field, and Detail is not parsed by anything. There is
// no longer a way for operator-controlled text to be mistaken for a verdict, so
// the diagnostic gets to be accurate again.
func workerScopesUnreadableDetail(err error) string {
	return "worker scopes unreadable: " + err.Error()
}

// readWorkerSupervisorMemory reads a scope's live memory.current (plus its
// memory.stat reclaimable-cache figure) WITHOUT requiring memory.max to be
// set. This is deliberately narrower than readSliceMemory (admit.go), which
// treats memory.max=="max" (uncapped) as a read failure — a correct,
// defensive precondition for the OUTER-scope ledger read above (every
// confine-launched outer scope IS always given an explicit finite cap by
// construction, so "unbounded" there is a genuine anomaly worth failing
// loudly on) but WRONG for the supervisor's own child scope: bootstrap
// (BootstrapAitestSupervisor, aitest_bootstrap_linux.go) deliberately never
// writes memory.max on it at all -- the supervisor is meant to be contained
// transitively by the OUTER scope's cap, never capped individually. Found
// live (AIRA-38, real-cgroup e2e): reusing readSliceMemory for this read
// meant the aggregate guard's supervisor-RSS check (below) reported
// "unevaluated" on EVERY real invocation, since the supervisor scope's own
// memory.max is unconditionally "max" -- the granted (confined) path was
// never actually reachable outside a mocked unit test.
func readWorkerSupervisorMemory(path string) (current, reclaimable int64, ok bool, reason string) {
	currentData, err := os.ReadFile(filepath.Join(path, "memory.current"))
	if err != nil {
		return 0, 0, false, "read-error"
	}
	current, valid := parseAdmitMemory(currentData)
	if !valid {
		return 0, 0, false, "parse-error"
	}
	statData, err := os.ReadFile(filepath.Join(path, "memory.stat"))
	if err == nil {
		reclaimable, valid = parseSliceMemoryStat(statData)
	}
	if err != nil || !valid {
		reclaimable = 0
	}
	return current, reclaimable, true, ""
}

// evaluateWorkerAdmit makes one synchronous grant/deny decision for req.
// "Used" is the OUTER scope's own live memory.current, read directly —
// cgroup memory accounting is hierarchical, so this single read already
// includes the supervisor's own RSS plus every already-placed worker's
// (spec 3.3). Summing individually-read worker-scope grants separately (an
// earlier version of this function did) was both redundant with that
// hierarchical accounting AND unsafe: Σ(worker grants) + supervisor RSS
// could exceed outerMax even when the ledger thought there was room,
// risking an outer-scope-level memory.oom.group kill of the ENTIRE run —
// precisely the incident class this design exists to prevent.
//
// AIRA-39/AIRA-41: `committed` is no longer an in-memory grants map. It is
// Σ memory.max over the outer scope's real `.aira-worker-*` children, and the
// daemon creates each worker's scope itself inside this critical section, so
// the ledger survives a daemon restart and a killed relay cannot free it.
//
// Reports (response, false) when the caller's context ended or the daemon is
// stopping while queued on the outer scope's lock — the caller returns without
// writing anything, mirroring workerAdmitConnection's own peer-gone paths.
func (s *Server) evaluateWorkerAdmit(ctx context.Context, req workerAdmitRequest) (WorkerAdmitResponse, bool) {
	readMemory := s.admitReadMemory
	if readMemory == nil {
		readMemory = readSliceMemory
	}
	used, outerMax, reclaimable, ok, reason := readMemory(req.outerScope)
	if !ok {
		// readSliceMemory reports "unbounded" when the outer scope's own
		// memory.max reads back "max". That is structural, not transient: a
		// real confine-launched outer scope is always given a finite
		// memory.max as part of the same atomic grant that launches it, so
		// waiting can never make it capped. It is the one unevaluated
		// sub-case the design spec (§3.7) classifies as fallback-triggering
		// rather than retriable — and it is now carried by Class, instead of
		// by the client sniffing the word "unbounded" out of a sentence that
		// also quotes operator-controlled cgroup paths.
		if reason == "unbounded" {
			return WorkerAdmitResponse{
				State:  runner.WorkerAdmitStateUnevaluated,
				Class:  runner.WorkerAdmitClassAdmissionUnusable,
				Reason: runner.WorkerAdmitReasonOuterScopeUnbounded,
				Detail: "outer scope " + req.outerScope + " has no finite memory.max",
			}, true
		}
		return WorkerAdmitResponse{
			State:  runner.WorkerAdmitStateUnevaluated,
			Class:  runner.WorkerAdmitClassContended,
			Reason: runner.WorkerAdmitReasonOuterScopeUnreadable,
			Detail: reason,
		}, true
	}
	// memory.stat's file pages are reclaimable cache, not non-negotiable
	// worker pressure. Match checkedAvailable's exact floor-and-discount
	// arithmetic so a read-heavy test suite is not persistently denied even
	// though the kernel can reclaim this cache below the outer cap.
	if reclaimable < 0 {
		reclaimable = 0
	}
	used = subtractFloor(used, reclaimable)
	headroom := s.workerAdmitHeadroom
	if headroom < 0 {
		headroom = 0
	}
	ceiling := outerMax - headroom
	if req.estimatedBytes > ceiling {
		// Could never fit even at zero current usage — a stable fact about
		// THIS request, not a transient contention moment. Deny
		// immediately (workerAdmitConnection, Task 5, breaks its poll loop
		// on this reason) instead of waiting out the full poll timeout
		// only to time out anyway.
		return WorkerAdmitResponse{
			State:  runner.WorkerAdmitStateDenied,
			Class:  runner.WorkerAdmitClassRequestInvalid,
			Reason: runner.WorkerAdmitReasonExceedsCeiling,
			Detail: fmt.Sprintf("estimated %d bytes exceeds the outer scope's %d-byte ceiling", req.estimatedBytes, ceiling),
		}, true
	}
	// One lock per OUTER SCOPE (not per job), held across the committed scan,
	// the supervisor read and the scope creation below. It is what makes
	// "read the sum, then add to it" atomic — and it is why deleting
	// workerScopeOwner is safe: the sum now covers every job's workers under
	// this scope, and every requester for the scope queues on this one lock.
	// Moving any of it outside would let two concurrent requests both read the
	// same pre-create sum and both grant, the aggregate-guard-defeating race
	// AIRA-27/28/29 fixed at whole-job granularity.
	//
	// Unlike the sync.Mutex it replaces, this acquisition is abandonable: with
	// a cgroupfs scan and a scope creation inside the critical section, and
	// with worker-admit now holding a shared admitSlots token while it waits
	// (AIRA-63), an uninterruptible wait would let one slow outer scope pin
	// admission slots ordinary `aira confine` admission needs.
	state := s.workerScopeFor(req.outerScope)
	if !s.acquireWorkerScope(ctx, state) {
		return WorkerAdmitResponse{}, false
	}
	defer state.release()
	// Saturating comparison, never `ceiling-used`: with `committed` now summed
	// from the tree it is no longer bounded by admitMaxReserve per grant, and
	// the subtractive form wraps POSITIVE once a term saturates at MaxInt64
	// (ceiling=0, committed=MaxInt64, supervisorUsed=2 wraps the right-hand
	// side to MaxInt64 and grants). addClamp saturates, so a saturated total is
	// always > ceiling and always denies (found by Sol plan-review).
	if addClamp(req.estimatedBytes, used) > ceiling {
		// Not available RIGHT NOW (transient: current live usage), but
		// could be granted once usage drops — the caller's poll loop keeps
		// retrying this until granted or its own max_wait_ms deadline
		// converts it to "timeout".
		return WorkerAdmitResponse{
			State:  runner.WorkerAdmitStateDenied,
			Class:  runner.WorkerAdmitClassContended,
			Reason: runner.WorkerAdmitReasonInsufficientHeadroom,
		}, true
	}
	// Worst-case guard, on top of the live-usage check above: live usage
	// having room RIGHT NOW does not mean it always will. Sum the
	// memory.max already promised to this job's other workers — if every
	// one of them simultaneously grew to its own full cap, the total must
	// still fit under ceiling, or an outer-scope memory.oom.group kill can
	// take out the whole run (supervisor plus every sibling worker), not
	// just the one that grew — precisely what Goal 2 in the design spec
	// requires this NOT be able to do. This trades a little utilization
	// (the live-usage check alone would admit a worker whose siblings
	// simply haven't grown to their peaks YET) for that hard guarantee —
	// the same aggregate-not-bound failure class AIRA-27/28/29 already
	// fixed at whole-job granularity, found here at worker granularity by
	// build-review (a live-usage-only check is silent on the SUM of caps,
	// only on CURRENT usage). Pollable, not an immediate reject: an
	// existing worker retiring frees its share of committed capacity.
	//
	// CORRECTED (found by a second review round: the first version of
	// this guard omitted the supervisor's own footprint entirely). The
	// worst case isn't just Σ(worker caps) — it's supervisor RSS PLUS
	// Σ(worker caps), and a warm-imported pytest supervisor (spec 3.1/3.2:
	// COW-shared interpreter state is the whole point of this design) can
	// routinely hold hundreds of MiB, far more than headroom (64MiB
	// default) alone budgets for. Concretely: an 8G outer cap with a 600M
	// supervisor and eight workers each admitted at 970M (low live usage
	// at grant time) would pass both checks above (Σcaps=7.76G ≤
	// ceiling≈7.94G) yet still exceed the outer cap once every worker
	// grows to its own peak (600M+7.76G=8.36G > 8G) — the exact
	// outer-scope oom.group incident Goal 2 requires be impossible.
	// Reading the supervisor scope's own live memory.current and
	// subtracting it here closes that gap; an unreadable supervisor
	// scope reports unevaluated (fail toward safety — this codebase never
	// silently admits on a read it cannot establish), same as an
	// unreadable outer scope above. Uses readWorkerSupervisorMemory (a
	// SEPARATE seam from readMemory above), not readMemory/readSliceMemory:
	// the supervisor scope is deliberately never given its own memory.max
	// (see readWorkerSupervisorMemory's own doc comment) — reusing the
	// outer-scope reader here made this guard report unevaluated on EVERY
	// real invocation, an AIRA-38 finding.
	readSupervisorMemory := s.admitReadWorkerSupervisorMemory
	if readSupervisorMemory == nil {
		readSupervisorMemory = readWorkerSupervisorMemory
	}
	supervisorScope := runner.WorkerScopeChildPath(req.outerScope, "supervisor")
	supervisorUsed, supervisorReclaimable, supervisorOK, supervisorReason := readSupervisorMemory(supervisorScope)
	if !supervisorOK {
		return WorkerAdmitResponse{
			State:  runner.WorkerAdmitStateUnevaluated,
			Class:  runner.WorkerAdmitClassContended,
			Reason: runner.WorkerAdmitReasonSupervisorScopeUnreadable,
			Detail: "supervisor scope unreadable: " + supervisorReason,
		}, true
	}
	// Apply the same reclaimable-cache discount to the supervisor half of the
	// aggregate guard. Its memory.current is otherwise a second source of
	// spurious denials after the supervisor has populated page cache.
	if supervisorReclaimable < 0 {
		supervisorReclaimable = 0
	}
	supervisorUsed = subtractFloor(supervisorUsed, supervisorReclaimable)
	fits := func(committed int64) bool {
		return addClamp(addClamp(req.estimatedBytes, committed), supervisorUsed) <= ceiling
	}
	// Contended path: the cached sum, refreshed at most once per interval. This
	// is the 200ms-per-waiter poll path AIRA-61's CPU regression was about.
	committed, err := s.workerCommitted(state, false)
	if err != nil {
		return WorkerAdmitResponse{
			State:  runner.WorkerAdmitStateUnevaluated,
			Class:  runner.WorkerAdmitClassContended,
			Reason: runner.WorkerAdmitReasonWorkerScopesUnreadable,
			Detail: workerScopesUnreadableDetail(err),
		}, true
	}
	if !fits(committed) {
		return WorkerAdmitResponse{
			State:  runner.WorkerAdmitStateDenied,
			Class:  runner.WorkerAdmitClassContended,
			Reason: runner.WorkerAdmitReasonAggregateCapExceeded,
		}, true
	}
	// Granting path: force a fresh scan first. A cached sum may never be the
	// basis of an admission — a `.aira-worker-*` child that appeared since the
	// last scan is invisible to the cache and need not collide with the id
	// about to be allocated, so the cache can be stale-LOW exactly here. This
	// costs one scan per ADMITTED WORKER (a handful per suite), never one per
	// poll, so the cadence bound above is untouched.
	if committed, err = s.workerCommitted(state, true); err != nil {
		return WorkerAdmitResponse{
			State:  runner.WorkerAdmitStateUnevaluated,
			Class:  runner.WorkerAdmitClassContended,
			Reason: runner.WorkerAdmitReasonWorkerScopesUnreadable,
			Detail: workerScopesUnreadableDetail(err),
		}, true
	}
	if !fits(committed) {
		return WorkerAdmitResponse{
			State:  runner.WorkerAdmitStateDenied,
			Class:  runner.WorkerAdmitClassContended,
			Reason: runner.WorkerAdmitReasonAggregateCapExceeded,
		}, true
	}
	seq := state.nextSeq
	if state.maxIndex > seq {
		// Restart reconstruction: nothing in RAM knows the ids already on the
		// tree, so the largest existing numeric suffix seeds the sequence.
		seq = state.maxIndex
	}
	if seq >= maxWorkerScopeSeq {
		// Terminal, not retriable: ids are never reused (nextSeq only grows,
		// and a restart re-seeds it from the largest suffix on the tree), so
		// no amount of waiting produces a free id. request-invalid is the
		// TERMINAL-BUT-DAEMON-HEALTHY disposition, not a claim that the
		// request was malformed — see the class's own doc comment.
		return WorkerAdmitResponse{
			State:  runner.WorkerAdmitStateDenied,
			Class:  runner.WorkerAdmitClassRequestInvalid,
			Reason: runner.WorkerAdmitReasonWorkerIDSpaceExhausted,
			Detail: fmt.Sprintf("worker id space exhausted under %s (limit %d)", req.outerScope, maxWorkerScopeSeq),
		}, true
	}
	seq++
	workerID := strconv.Itoa(seq)
	memoryHigh := req.estimatedBytes * 4 / 5
	create := s.workerScopeCreate
	if create == nil {
		create = runner.CreateWorkerScope
	}
	// The daemon creates the scope itself, inside this critical section, using
	// the SAME runner.CreateWorkerScope the CLI used to call — no second
	// scope-creation implementation. This closes the grant->creation window
	// (the grant used to be recorded here and the directory created afterwards
	// by a different process) and makes the ledger's source of truth exist
	// before the grant is ever delivered.
	scopePath, err := create(ctx, req.outerScope, workerID, req.estimatedBytes, memoryHigh)
	if err != nil {
		if errors.Is(err, fs.ErrExist) {
			// A child the scan did not see PROVES the cached sum was
			// stale-low, so this must never "take the next id and grant" —
			// that would admit against a sum omitting the colliding child, the
			// exact AIRA-39 over-admit. Invalidate and deny RETRIABLY: the
			// caller's poll loop re-evaluates against a fresh scan that now
			// includes it, and then grants or denies correctly.
			state.invalidate()
			return WorkerAdmitResponse{
				State:  runner.WorkerAdmitStateDenied,
				Class:  runner.WorkerAdmitClassContended,
				Reason: runner.WorkerAdmitReasonWorkerScopeIDCollision,
			}, true
		}
		// Fail closed: no grant is ever recorded without its scope. Classed
		// request-invalid, which is the TERMINAL-BUT-DAEMON-HEALTHY
		// disposition (formerly carried by the "reject:" reason prefix). Not
		// every such failure is provably permanent, but
		// BootstrapAitestSupervisor has already enabled
		// cgroup.subtree_control before any worker-admit call runs, so the
		// realistic cause is broken daemon-side cgroupfs access — and a
		// `contended` class would be retried INDEFINITELY, stalling every
		// aitest run on the machine rather than the one job that hit it.
		// `admission-unusable` is not the answer either: it would strip
		// containment for a whole run over a daemon that is answering fine.
		return WorkerAdmitResponse{
			State:  runner.WorkerAdmitStateDenied,
			Class:  runner.WorkerAdmitClassRequestInvalid,
			Reason: runner.WorkerAdmitReasonWorkerScopeCreateFailed,
			Detail: err.Error(),
		}, true
	}
	state.nextSeq = seq
	// The tree changed under us; the next committed read must see it.
	state.invalidate()
	return WorkerAdmitResponse{
		State: runner.WorkerAdmitStateGranted, Class: runner.WorkerAdmitClassGranted,
		WorkerID: workerID, ScopePath: scopePath,
		MemoryMax: req.estimatedBytes, MemoryHigh: memoryHigh,
	}, true
}

func validateWorkerAdmitArgs(args map[string]any, waitCeilingMs int64) (workerAdmitRequest, error) {
	req := workerAdmitRequest{}
	str := func(key string, required bool) (string, error) {
		raw, exists := args[key]
		if !exists {
			if required {
				return "", fmt.Errorf("%s: worker-admit %s is required", CodeProtocol, key)
			}
			return "", nil
		}
		value, ok := raw.(string)
		if !ok || (required && value == "") {
			return "", fmt.Errorf("%s: worker-admit %s must be a non-empty string", CodeProtocol, key)
		}
		return value, nil
	}
	var err error
	if req.jobID, err = str("job_id", true); err != nil {
		return workerAdmitRequest{}, err
	}
	if req.outerScope, err = str("outer_scope", true); err != nil {
		return workerAdmitRequest{}, err
	}
	// Canonicalise BEFORE anything keys on it. The per-outer-scope lock is now
	// the only serialisation of "read the sum, then add to it", so `/x`, `/x/`
	// and `/x/.` reaching three different ledger cells while mutating one
	// cgroup would defeat it outright — and linuxScopeBackend.Create cleans its
	// parent anyway, so an uncleaned key would also not match the directory the
	// daemon creates (found by Sol plan-review). A relative path is refused
	// rather than resolved against the daemon's own working directory, which
	// would silently name a different cgroup than the client meant.
	if !filepath.IsAbs(req.outerScope) {
		return workerAdmitRequest{}, fmt.Errorf("%s: worker-admit outer_scope must be an absolute cgroup path, got %q", CodeProtocol, req.outerScope)
	}
	req.outerScope = filepath.Clean(req.outerScope)
	if req.signature, err = str("signature", false); err != nil {
		return workerAdmitRequest{}, err
	}
	// exactAdmitInt64 (existing, admit.go) — overflow-safe float64->int64,
	// reused rather than the naive int64(estimated) truncation this used
	// to do, which let an arbitrary huge float64 truncate unchecked.
	estimated, ok := exactAdmitInt64(args["estimated_bytes"])
	if !ok || estimated < workerAdmitEstimatedBytesMin || estimated > admitMaxReserve {
		return workerAdmitRequest{}, fmt.Errorf("%s: worker-admit estimated_bytes must be at least %d bytes and no larger than %d", CodeProtocol, workerAdmitEstimatedBytesMin, admitMaxReserve)
	}
	req.estimatedBytes = estimated
	maxWait, ok := exactAdmitInt64(args["max_wait_ms"])
	if !ok {
		return workerAdmitRequest{}, fmt.Errorf("%s: worker-admit max_wait_ms must be an integer", CodeProtocol)
	}
	if maxWait < 0 {
		maxWait = 0
	}
	// AIRA-58: refuse rather than silently clamp, same honesty fix as the admit
	// path. The code stays CodeProtocol (NOT CodeAdmitWaitTooLong) on purpose:
	// worker_admit_client_linux.go wraps every non-OK response as
	// E_CONFINE_UNAVAILABLE, and the aitest supervisor responds to "unavailable"
	// by disabling daemon admission and running UNCONFINED. It already classifies
	// E_DAEMON_PROTOCOL as permanent, so refusing with that code fails closed
	// instead of silently dropping confinement.
	if maxWait > waitCeilingMs {
		return workerAdmitRequest{}, fmt.Errorf("%s: worker-admit max_wait_ms %d exceeds the ceiling of %d ms (%s)",
			CodeProtocol, maxWait, waitCeilingMs, time.Duration(waitCeilingMs)*time.Millisecond)
	}
	req.maxWaitMS = maxWait
	return req, nil
}

func (s *Server) workerAdmitConnection(conn net.Conn, args map[string]any) {
	start := s.admitNowTime()
	// AIRA-63: worker-admit retains a connection and a polling goroutine for
	// its whole wait, and until now had no concurrency bound at all, unlike
	// admitConnection. Share the existing admitSlots semaphore rather than
	// adding a second one.
	//
	// Saturation is deliberately NOT an error frame the way admitConnection's
	// CodeBusy is. An unrecognised error Code is classified
	// contract-violation by RequestWorkerAdmit — terminal and loud — which is
	// not what a transient slot shortage deserves. Before AIRA-42 it was worse
	// still: any non-"OK" Code became the prose "worker-admit request
	// rejected", which matched none of supervisor.py's denial substrings, fell
	// through to WorkerAdmitUnavailable, and made _disable_daemon run the WHOLE
	// suite unconfined — a safety regression, not a denial. Emitting it as an
	// ordinary denial classed `contended` makes supervisor.py raise the
	// retriable WorkerAdmitDenied, which _wait_for_admission_or_disable retries
	// until a slot frees. That disposition is now the Class field itself rather
	// than a "fallback:" spelling convention on the reason.
	if !s.acquireAdmitSlot() {
		saturated := WorkerAdmitResponse{
			State:  runner.WorkerAdmitStateDenied,
			Class:  runner.WorkerAdmitClassContended,
			Reason: runner.WorkerAdmitReasonAdmitSlotsSaturated,
		}
		saturated.WaitedMS = elapsedMilliseconds(start, s.admitNowTime())
		_ = conn.SetWriteDeadline(time.Now().Add(admitWriteTimeout))
		_ = writeFrame(conn, responseFrame(core.Response{OK: false, Code: "OK", Data: saturated}))
		return
	}
	defer s.releaseAdmitSlot()

	req, err := validateWorkerAdmitArgs(args, workerAdmitWaitCeilingMs)
	if err != nil {
		_ = writeFrame(conn, errorFrame(CodeProtocol, err.Error()))
		return
	}
	peerCtx, cancelPeer := context.WithCancel(context.Background())
	defer cancelPeer()
	go func() {
		var one [1]byte
		_, _ = conn.Read(one[:])
		cancelPeer()
	}()

	poll := s.workerAdmitPollInterval
	if poll <= 0 {
		poll = 200 * time.Millisecond
	}
	deadline := s.admitNowTime().Add(time.Duration(req.maxWaitMS) * time.Millisecond)
	var response WorkerAdmitResponse
	for {
		var proceed bool
		// evaluateWorkerAdmit can queue on the outer scope's lock; a peer that
		// vanished or a stopping daemon abandons it there, exactly like the
		// poll sleep's own peerCtx/stopping cases below.
		if response, proceed = s.evaluateWorkerAdmit(peerCtx, req); !proceed {
			return
		}
		if response.State == runner.WorkerAdmitStateGranted || response.State == runner.WorkerAdmitStateUnevaluated {
			break
		}
		// AIRA-42: this was `strings.HasPrefix(response.Reason, "reject:")`
		// — the daemon running the SAME defect on its own reason strings that
		// supervisor.py ran on the daemon's prose. The convention it
		// implemented was real and load-bearing (Fable re-gate round 3: every
		// permanently-impossible verdict must break this loop, not just the
		// two that existed when it was written), but spelling it as a prefix
		// made the disposition a property of how a reason was WORDED. It is
		// now the Class field, so a new terminal verdict gets the right
		// behaviour by declaring its disposition rather than by remembering
		// to spell its reason a particular way.
		//
		// The old scoping hazard disappears by construction rather than by
		// comment: the timeout verdict's reason used to be "reject:saturated",
		// which coincidentally matched the prefix, so this test had to be
		// narrowed to state=="denied" to avoid breaking on it. There is no
		// coincidental match to guard against now — but the state check is
		// KEPT, because it is independently correct: a timeout means the
		// CLIENT's own wait budget expired, which is retriable with a fresh
		// request, never a stable daemon-side verdict.
		if response.State == runner.WorkerAdmitStateDenied && response.Class == runner.WorkerAdmitClassRequestInvalid {
			// A stable "never going to fit" fact about this request, not a
			// transient contention moment — surface "denied" to the client
			// immediately instead of waiting out the full poll timeout
			// only to time out anyway. Every OTHER non-granted state keeps
			// polling below (a live-usage-driven "not right now" is
			// retried until it clears or the deadline converts it to
			// "timeout").
			break
		}
		remaining := deadline.Sub(s.admitNowTime())
		if remaining <= 0 {
			// The reason token is now plain `saturated`. It used to be
			// "reject:saturated", whose accidental prefix match is what forced
			// the state-scoping above to be reasoned about at all.
			response = WorkerAdmitResponse{
				State:  runner.WorkerAdmitStateTimeout,
				Class:  runner.WorkerAdmitClassContended,
				Reason: runner.WorkerAdmitReasonSaturated,
			}
			break
		}
		// Clamp the sleep to whatever's left of the caller's own declared
		// deadline, not the unconditional fixed poll interval (found by
		// Sol build-review, AIRA-38 review wave): sleeping the full
		// interval when only a fraction of it remains let waited_ms
		// overshoot the caller's max_wait_ms by up to one poll interval
		// before the NEXT loop iteration's evaluate ever ran -- low
		// impact (a late grant is a strictly better outcome than a
		// spurious timeout), but a genuine budget-precision violation.
		sleep := poll
		if remaining < sleep {
			sleep = remaining
		}
		select {
		case <-time.After(sleep):
		case <-peerCtx.Done():
			return
		case <-s.stopping:
			return
		}
	}
	// Every response below is written as a single terminal decision. Use the
	// admit clock seam (rather than time.Now) so its observed wait is both
	// consistent with deadline handling and deterministic in tests.
	response.WaitedMS = elapsedMilliseconds(start, s.admitNowTime())

	// There is NO ledger release here any more, on any exit path, and that is
	// deliberate (AIRA-41). The ledger charges the SCOPE, not this connection,
	// so:
	//   - a killed relay no longer silently frees capacity while its worker is
	//     still alive under its still-intact cap — the bug this fix closes;
	//   - the normal release is supervisor.py's _retire_worker, which reaps the
	//     worker FIRST and only then rmdirs the (now empty) scope;
	//   - a daemon-side rmdir on lease close would be actively wrong in the
	//     AIRA-41 case: the cgroup is still populated, the rmdir would fail
	//     EBUSY, and the scope SHOULD keep charging until the worker is reaped.
	//
	// A grant whose response write fails therefore leaves its scope on the tree,
	// charging until the outer confine job's own teardown removes the subtree.
	// Removing it here is NOT safe: writeFrameBytes surfaces the underlying
	// Write error and discards n, so "the client provably never learned the
	// path" is false — a fully delivered frame followed by a deadline or reset
	// error is possible, and a client that forks its worker into a removed
	// cgroup hits WorkerPlacementFailed -> _disable_daemon -> the whole suite
	// unconfined. An over-charge produces a loud, retriable denial instead
	// (found by Sol plan-review; DeepSeek argued the opposite and was not taken).
	_ = conn.SetWriteDeadline(time.Now().Add(admitWriteTimeout))
	ok := response.State == runner.WorkerAdmitStateGranted
	if err := writeFrame(conn, responseFrame(core.Response{OK: ok, Code: "OK", Data: response})); err != nil {
		return
	}
	if !ok {
		return
	}
	select {
	case <-peerCtx.Done():
	case <-s.stopping:
	}
}
