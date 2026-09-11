//go:build linux

package daemon

import (
	"context"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"aira/internal/cgrouptest"
)

// AIRA-49 physical-reap stale-lease backstop.
//
// After S14 deleted the periodic cgroup scan (and, with it, AIRA-68's scan-
// derived seen->gone "vanished" reclaim branch), this pass reclaims a lease ONLY
// on a kernel-enforced physical reap of an empty scope (runner.ReapScopeIfEmpty),
// never on the age signal alone and never on absence. Supervisor death is handled
// by the socket-EOF release (design §3); this backstop covers only the residual
// case where the connection is somehow still held while the scope is empty. These
// tests pin the idempotency, revalidation, ABA-safety and coverage-gap properties
// of that remaining path; confine_reaper_linux_test.go pins the base reclaim.

// reapTestProcessAlive is a real liveness check, not `kill -0`: a child this test
// never waits on becomes a ZOMBIE when it dies, and kill(pid, 0) still succeeds
// for a zombie — which would let the escaped-leader test below pass for a dead
// process and pin nothing at all.
func reapTestProcessAlive(pid int) bool {
	data, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	if err != nil {
		return false
	}
	// State is the field after the parenthesised comm, which may itself contain
	// spaces or parentheses.
	close := strings.LastIndexByte(string(data), ')')
	if close < 0 || close+2 >= len(data) {
		return false
	}
	return data[close+2] != 'Z' && data[close+2] != 'X'
}

// verifies: a lease already released by its own connection is not re-reported as
// reclaimed by this pass. releaseAdmitWaiter is idempotent, so a sweep that
// ignored the return would log a reclaim that a concurrent ordinary release
// actually performed — a receipt for an act this pass did not carry out.
func TestReleaseStaleLeaseCandidateReportsNoReclaimForAnAlreadyReleasedLease(t *testing.T) {
	slicePath := staleLeaseTestSliceRoot(t)
	now := time.Now()
	server := staleLeaseTestServer(t, now, time.Second)
	scopeID := staleLeaseTestScopeID("already-released")
	queue, waiter := staleLeaseTestWaiter(server, slicePath, scopeID, now.Add(-2*time.Hour), now.Add(-time.Hour))

	candidates := server.staleGrantedLeases(time.Second)
	if len(candidates) != 1 {
		t.Fatalf("candidates = %d, want 1", len(candidates))
	}
	// The lease's own connection closes first — the ordinary path.
	server.releaseAdmitWaiter(queue, waiter)

	proof, reclaimed := server.releaseStaleLeaseCandidate(candidates[0])

	if reclaimed || proof != "" {
		t.Fatalf("claimed a reclaim it did not perform: proof=%q reclaimed=%v", proof, reclaimed)
	}
	if queue.outstanding != 0 || queue.outstandingJobs != 0 {
		t.Fatalf("ledger double-discharged: outstanding=%d jobs=%d, want 0/0", queue.outstanding, queue.outstandingJobs)
	}
}

// verifies: the ledger discharge is a ONE-SHOT transition and reports so.
// releaseStaleLeaseCandidate decides whether to log "reclaimed" from this return
// value, so a discharge that a concurrent ordinary release already performed
// must report false — otherwise the daemon prints a receipt for an act it did
// not carry out.
//
// Asserted on the return value directly. The ledger alone cannot see this: the
// second call performs no arithmetic either way, so a test that only checked
// `outstanding` would pass against a function that claimed every call succeeded.
func TestReleaseAdmitWaiterLockedReportsOnlyTheCallThatTransitioned(t *testing.T) {
	now := time.Now()
	server := staleLeaseTestServer(t, now, time.Second)
	queue, waiter := staleLeaseTestWaiter(server, t.TempDir(), staleLeaseTestScopeID("once"), now.Add(-2*time.Hour), now.Add(-time.Hour))

	queue.mu.Lock()
	first := releaseAdmitWaiterLocked(queue, waiter)
	second := releaseAdmitWaiterLocked(queue, waiter)
	outstanding, jobs := queue.outstanding, queue.outstandingJobs
	queue.mu.Unlock()

	if !first {
		t.Fatalf("the first discharge reported no transition")
	}
	if second {
		t.Fatalf("a second discharge of the same waiter claimed to have transitioned it")
	}
	if outstanding != 0 || jobs != 0 {
		t.Fatalf("ledger discharged %d times: outstanding=%d jobs=%d, want 0/0", 2, outstanding, jobs)
	}
}

// verifies: the post-reap discharge RE-VALIDATES under the lock before touching
// the ledger, and reports honestly when it does not act.
//
// The reap syscall cannot be made while holding queue.mu, so validation happens
// twice — once to decide whether to touch the filesystem, once to decide whether
// to touch the ledger. Both plan reviewers found that a single unlocked
// validation leaves a window in which the waiter is released and replaced. This
// asserts the second validation directly, because no black-box test can steer
// the interleaving without a synchronisation hook in production code.
func TestDischargeReapedStaleLeaseRevalidatesBeforeTouchingTheLedger(t *testing.T) {
	slicePath := staleLeaseTestSliceRoot(t)
	now := time.Now()
	server := staleLeaseTestServer(t, now, time.Second)
	scopeID := staleLeaseTestScopeID("post-reap")
	queue, waiter := staleLeaseTestWaiter(server, slicePath, scopeID, now.Add(-2*time.Hour), now.Add(-time.Hour))

	candidates := server.staleGrantedLeases(time.Second)
	if len(candidates) != 1 {
		t.Fatalf("candidates = %d, want 1", len(candidates))
	}
	// The lease's own connection closes in the window the reap syscall occupies.
	server.releaseAdmitWaiter(queue, waiter)

	if server.dischargeReapedStaleLease(candidates[0], time.Second) {
		t.Fatalf("discharged a lease that had already been released by its own connection")
	}
	if queue.outstanding != 0 || queue.outstandingJobs != 0 {
		t.Fatalf("ledger double-discharged: outstanding=%d jobs=%d, want 0/0", queue.outstanding, queue.outstandingJobs)
	}
}

// verifies: BOTH halves of the scope-id ABA.
//
// staleLeaseCandidate used to carry only path+scopeID, and the release then
// re-SEARCHED the queue for a granted waiter with that id. Between candidate
// collection and the release step the stale waiter can close its own connection
// and a replacement can be admitted under the same id — so the search releases
// the WRONG lease. The ledger half is fixed by carrying the exact waiter pointer.
//
// The destructive half is subtler: ReapScopeIfEmpty is keyed on the scope-id
// STRING, so it would rmdir the REPLACEMENT's newly created, still-empty scope. A
// pointer-carrying candidate does not fix that on its own; staleLeaseActionableLocked
// re-validating the pointer before the reap does.
func TestReleaseStaleGrantedLeasesPassDoesNotReleaseOrReapAReplacementWithTheSameScopeID(t *testing.T) {
	slicePath := staleLeaseTestSliceRoot(t)
	now := time.Now()
	server := staleLeaseTestServer(t, now, time.Second)
	scopeID := staleLeaseTestScopeID("aba")
	// A stale lease whose scope exists and is empty — the shape that reaches
	// ReapScopeIfEmpty.
	queue, stale := staleLeaseTestWaiter(server, slicePath, scopeID, now.Add(-2*time.Hour), now.Add(-time.Hour))
	scopePath := staleLeaseTestScope(t, slicePath, scopeID)

	candidates := server.staleGrantedLeases(time.Second)
	if len(candidates) != 1 {
		t.Fatalf("candidates = %d, want 1", len(candidates))
	}

	// The stale lease's own connection closes (the ordinary path), then a fresh
	// job is admitted under the same scope id and creates its still-empty scope at
	// the same path. Re-registering the queue is what actually happens when a new
	// waiter arrives, and it is load-bearing here: without it a scope-id search
	// would find no queue at all and this test would pass vacuously against the
	// very defect it exists to catch.
	server.releaseAdmitWaiter(queue, stale)
	replacement := &admitWaiter{
		seq: 99, reserve: 4096, state: admitGranted, accounted: true, grantedCh: make(chan struct{}),
		enqueued: now, grantedAt: now, scopeID: scopeID, name: "aba", owner: "session-b",
	}
	queue.mu.Lock()
	queue.waiters = append(queue.waiters, replacement)
	queue.outstanding, queue.outstandingJobs = 4096, 1
	queue.mu.Unlock()
	server.admitRegistryMu.Lock()
	server.admitQueues[slicePath] = queue
	server.admitRegistryMu.Unlock()

	for _, candidate := range candidates {
		server.releaseStaleLeaseCandidate(candidate)
	}
	replacementScope := scopePath

	if queue.outstanding != 4096 || queue.outstandingJobs != 1 {
		t.Errorf("the replacement's lease was released by a stale candidate: outstanding=%d jobs=%d, want 4096/1", queue.outstanding, queue.outstandingJobs)
	}
	if replacement.state != admitGranted {
		t.Errorf("replacement waiter state=%v, want still granted", replacement.state)
	}
	if _, err := os.Stat(replacementScope); err != nil {
		t.Errorf("the replacement's scope directory was destroyed by a stale candidate's reap: %v", err)
	}
}

// verifies: D3's accepted coverage gap, pinned so that changing it is
// deliberate. A scope-less reservation (`aira confine-reserve`, scopeID == "")
// has NO cgroup artifact of any kind, so the reap can prove nothing about it, and
// its only release path is its connection closing.
//
// Asserted on the CANDIDATE LIST, not on the sweep's effect: the sweep would
// also reject an empty scope id downstream in ReapScopeIfEmpty's own validation,
// so a sweep-level assertion would pass even against an implementation that
// dropped the selector — the definition of a porous test.
func TestStaleGrantedLeasesNeverSelectsAScopelessReservation(t *testing.T) {
	now := time.Now()
	server := staleLeaseTestServer(t, now, 15*time.Minute)
	path := t.TempDir()
	queue := &sliceQueue{path: path, server: server, kick: make(chan struct{}, 1), stop: make(chan struct{})}
	reservation := &admitWaiter{
		seq: 1, reserve: 1 << 30, state: admitGranted, accounted: true, grantedCh: make(chan struct{}),
		enqueued: now.Add(-time.Hour), grantedAt: now.Add(-time.Hour),
		scopeID: "", name: "", owner: "",
	}
	// A scope-backed control in the same queue, so the assertion cannot pass
	// vacuously against a selector that returns nothing at all.
	control := &admitWaiter{
		seq: 2, reserve: 64, state: admitGranted, accounted: true, grantedCh: make(chan struct{}),
		enqueued: now.Add(-time.Hour), grantedAt: now.Add(-time.Hour),
		scopeID: staleLeaseTestScopeID("control"), name: "control", owner: "session-a",
	}
	queue.waiters = []*admitWaiter{reservation, control}
	server.admitQueues[path] = queue

	candidates := server.staleGrantedLeases(15 * time.Minute)
	if len(candidates) != 1 || candidates[0].waiter != control {
		t.Fatalf("candidates=%+v, want exactly the scope-backed control; a scope-less reservation has no death proof of any kind", candidates)
	}
}

// verifies: the migrated-leader gap, PINNED rather than left silent.
//
// A confined leader can move itself into a sibling cgroup and keep running
// (witnessed by internal/runner/descendant_escape_linux_test.go's sibling-escape
// test). Its original scope then becomes empty and removable, so "scope gone"
// does NOT prove the job is dead.
//
// The behaviour asserted here is the SAME as the base empty-reap branch's: the
// lease is reclaimed. The escapee is uncontained by construction, its reserve
// buys nothing, the release is ledger-only, and its memory is still charged
// through max(current - reclaimable, sum of reserves).
func TestReleaseStaleGrantedLeasesPassReclaimsAnEscapedLeaderStillAlive(t *testing.T) {
	slicePath := staleLeaseTestSliceRoot(t)
	now := time.Now()
	server := staleLeaseTestServer(t, now, time.Second)
	scopeID := staleLeaseTestScopeID("escaped")
	scopePath := staleLeaseTestScope(t, slicePath, scopeID)

	// The leader starts inside the scope and then migrates to a sibling.
	sibling, err := os.MkdirTemp(slicePath, ".aira-sibling-")
	if err != nil {
		cgrouptest.SkipOrFailRealCgroup(t, "create sibling cgroup: %v", err)
	}
	sleeper := staleLeaseTestSleeper(t, scopePath)
	t.Cleanup(func() { stopStaleLeaseTestSleeper(sleeper) })
	escapee := sleeper.Process.Pid
	if err := os.WriteFile(sibling+"/cgroup.procs", []byte(strconv.Itoa(escapee)), 0o644); err != nil {
		cgrouptest.SkipOrFailRealCgroup(t, "migrate %d into the sibling cgroup: %v", escapee, err)
	}
	t.Cleanup(func() { _ = os.Remove(sibling) })

	queue, _ := staleLeaseTestWaiter(server, slicePath, scopeID, now.Add(-2*time.Hour), now.Add(-time.Hour))

	// The escapee is provably ALIVE before the sweep: without this the test could
	// pass for a dead process and would pin nothing.
	if !reapTestProcessAlive(escapee) {
		t.Fatalf("escaped leader %d was not alive before the sweep", escapee)
	}
	snapshot := server.admitSliceSnapshot(slicePath)
	if snapshot.scopeJobs != 1 {
		t.Fatalf("scope-backed jobs = %d, want 1", snapshot.scopeJobs)
	}

	server.releaseStaleGrantedLeasesPass(context.Background())

	if !reapTestProcessAlive(escapee) {
		t.Fatalf("escaped leader %d died during the sweep; this test no longer pins the disputed live-leader condition", escapee)
	}
	if queue.outstanding != 0 || queue.outstandingJobs != 0 {
		t.Fatalf("the emptied scope of an escaped leader was not reclaimed: outstanding=%d jobs=%d", queue.outstanding, queue.outstandingJobs)
	}
	if after := server.admitSliceSnapshot(slicePath); after.scopeJobs != 0 {
		t.Fatalf("scope-backed population after reclaim = %d, want 0", after.scopeJobs)
	}
}
