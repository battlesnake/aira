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

// vanishedTestProcessAlive is a real liveness check, not `kill -0`: a child this
// test never waits on becomes a ZOMBIE when it dies, and kill(pid, 0) still
// succeeds for a zombie — which would let the escaped-leader test below pass for
// a dead process and pin nothing at all.
func vanishedTestProcessAlive(pid int) bool {
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

// AIRA-68. The AIRA-49 backstop could only ever reclaim a lease it was able to
// PHYSICALLY REAP:
//
//	reaped, reapErr := runner.ReapScopeIfEmpty(...)
//	if reapErr != nil || !reaped { continue }
//
// A lease whose scope directory is ALREADY GONE fails that call with ENOENT and
// is skipped on every pass, forever — the one backstop for "a granted lease that
// never transitions out of granted" is blind to the case where there is nothing
// left to reap. That is exactly AIRA-68's stated failure shape.
//
// The reclaim signal is a scan-derived TRANSITION, never plain absence: the
// scope must have been OBSERVED to exist (scopeSeen) and then observed gone
// (scopeVanished). Plain absence would release the lease of a launcher stalled
// before scope creation, which would then create its scope and run UNCHARGED —
// the #67 aggregate-OOM class. The existing empty-reap branch is safe against
// that launcher only because it is DESTRUCTIVE (the launcher's next cgroupfs
// write then fails); absence has no such fence, and the transition test is what
// replaces it.

func vanishedTestWaiter(server *Server, path, scopeID string, grantedAt time.Time, seen, vanished bool) (*sliceQueue, *admitWaiter) {
	queue, waiter := staleLeaseTestWaiter(server, path, scopeID, grantedAt.Add(-time.Hour), grantedAt)
	waiter.scopeSeen, waiter.scopeVanished = seen, vanished
	return queue, waiter
}

// verifies: a past-TTL lease whose scope was seen by the confine scan and is now
// gone IS reclaimed. Without this the ledger charges that reserve for the whole
// remaining uptime of the daemon.
func TestReleaseStaleGrantedLeasesPassReclaimsALeaseWhoseSeenScopeIsNowGone(t *testing.T) {
	slicePath := staleLeaseTestSliceRoot(t)
	now := time.Now()
	server := staleLeaseTestServer(t, now, time.Second)
	scopeID := staleLeaseTestScopeID("vanished")
	// Deliberately NO scope directory: it existed, was observed, and is gone.
	queue, waiter := vanishedTestWaiter(server, slicePath, scopeID, now.Add(-time.Hour), true, true)

	server.releaseStaleGrantedLeasesPass(context.Background())

	if queue.outstanding != 0 || queue.outstandingJobs != 0 {
		t.Fatalf("ledger not reclaimed for a seen-then-gone scope: outstanding=%d jobs=%d, want 0/0", queue.outstanding, queue.outstandingJobs)
	}
	if waiter.state != admitReleased {
		t.Fatalf("waiter state=%v, want released", waiter.state)
	}
}

// verifies: a past-TTL lease whose scope was NEVER SEEN is NOT reclaimed, even
// though its directory is absent. This is the direct regression for the unsafe
// design: a launcher stalled past the TTL before creating its scope must keep
// its reserve, exactly as today, or it will create the scope afterwards and run
// with no ledger charge at all.
func TestReleaseStaleGrantedLeasesPassLeavesANeverSeenScopeAlone(t *testing.T) {
	slicePath := staleLeaseTestSliceRoot(t)
	now := time.Now()
	server := staleLeaseTestServer(t, now, time.Second)
	scopeID := staleLeaseTestScopeID("never-seen")
	queue, waiter := vanishedTestWaiter(server, slicePath, scopeID, now.Add(-time.Hour), false, false)

	server.releaseStaleGrantedLeasesPass(context.Background())

	if queue.outstanding != 64 || queue.outstandingJobs != 1 {
		t.Fatalf("a lease whose scope was never observed was reclaimed on absence alone: outstanding=%d jobs=%d", queue.outstanding, queue.outstandingJobs)
	}
	if waiter.state != admitGranted {
		t.Fatalf("waiter state=%v, want still granted", waiter.state)
	}
}

// verifies: the vanished branch obeys the SAME TTL as the reap branch. A young
// lease whose scope came and went is a job that simply finished; its connection
// closes on its own and the sweep must not race it.
func TestReleaseStaleGrantedLeasesPassLeavesAYoungVanishedLeaseAlone(t *testing.T) {
	slicePath := staleLeaseTestSliceRoot(t)
	now := time.Now()
	server := staleLeaseTestServer(t, now, time.Hour)
	scopeID := staleLeaseTestScopeID("young-vanished")
	queue, _ := vanishedTestWaiter(server, slicePath, scopeID, now, true, true)

	server.releaseStaleGrantedLeasesPass(context.Background())

	if queue.outstanding != 64 || queue.outstandingJobs != 1 {
		t.Fatalf("young vanished lease reclaimed: outstanding=%d jobs=%d, want 64/1", queue.outstanding, queue.outstandingJobs)
	}
}

// verifies: `vanished` is RE-READ under the queue lock at the moment of acting,
// never trusted from the candidate snapshot. A scope that reappeared and is
// populated between candidate collection and the release step must not be
// reclaimed — the candidate list is a pre-filter, not a decision.
func TestReleaseStaleGrantedLeasesPassRereadsVanishedBeforeActing(t *testing.T) {
	slicePath := staleLeaseTestSliceRoot(t)
	now := time.Now()
	server := staleLeaseTestServer(t, now, time.Second)
	scopeID := staleLeaseTestScopeID("reappeared")
	queue, waiter := vanishedTestWaiter(server, slicePath, scopeID, now.Add(-time.Hour), true, true)

	// The evaluator's next scan observed the scope again; a stale snapshot would
	// still say "vanished".
	candidates := server.staleGrantedLeases(time.Second)
	if len(candidates) != 1 {
		t.Fatalf("candidates = %d, want 1", len(candidates))
	}
	queue.mu.Lock()
	waiter.scopeVanished = false
	queue.mu.Unlock()

	for _, candidate := range candidates {
		server.releaseStaleLeaseCandidate(candidate)
	}

	if queue.outstanding != 64 || queue.outstandingJobs != 1 {
		t.Fatalf("acted on a stale vanished snapshot: outstanding=%d jobs=%d, want 64/1", queue.outstanding, queue.outstandingJobs)
	}
}

// verifies: a vanished lease is NOT reclaimed while the confine scan is
// currently failing.
//
// Both plan reviewers found this independently. scopeVanished is an
// OBSERVATION, and once the scanner starts failing it can no longer be
// refreshed or cleared — so acting on it means reclaiming on a sighting the
// daemon can no longer confirm. AIRA's honesty rule is that a check which
// cannot establish its result reports unevaluated, never a fake pass; here that
// means holding the reserve. Without this guard one persistent cgroupfs failure
// freezes a stale absence into a permanent licence to reclaim.
func TestReleaseStaleGrantedLeasesPassWillNotReclaimAVanishedLeaseWhileTheScanIsFailing(t *testing.T) {
	slicePath := staleLeaseTestSliceRoot(t)
	now := time.Now()
	server := staleLeaseTestServer(t, now, time.Second)
	scopeID := staleLeaseTestScopeID("scan-failing")
	queue, waiter := vanishedTestWaiter(server, slicePath, scopeID, now.Add(-time.Hour), true, true)
	queue.mu.Lock()
	queue.adoptedScanFailed = true
	queue.mu.Unlock()

	server.releaseStaleGrantedLeasesPass(context.Background())

	if queue.outstanding != 64 || queue.outstandingJobs != 1 {
		t.Fatalf("reclaimed on a sighting that can no longer be confirmed: outstanding=%d jobs=%d, want 64/1", queue.outstanding, queue.outstandingJobs)
	}
	if waiter.state != admitGranted {
		t.Fatalf("waiter state=%v, want still granted", waiter.state)
	}
}

// verifies: the reclaim resumes the moment the scan recovers, so the guard above
// is fail-closed and not a permanent wedge of its own.
func TestReleaseStaleGrantedLeasesPassReclaimsOnceTheScanRecovers(t *testing.T) {
	slicePath := staleLeaseTestSliceRoot(t)
	now := time.Now()
	server := staleLeaseTestServer(t, now, time.Second)
	scopeID := staleLeaseTestScopeID("scan-recovers")
	queue, _ := vanishedTestWaiter(server, slicePath, scopeID, now.Add(-time.Hour), true, true)
	queue.mu.Lock()
	queue.adoptedScanFailed = true
	queue.mu.Unlock()

	server.releaseStaleGrantedLeasesPass(context.Background())
	if queue.outstanding != 64 {
		t.Fatalf("precondition: the lease should still be held while the scan fails, got outstanding=%d", queue.outstanding)
	}

	queue.mu.Lock()
	queue.adoptedScanFailed = false
	queue.mu.Unlock()
	server.releaseStaleGrantedLeasesPass(context.Background())

	if queue.outstanding != 0 || queue.outstandingJobs != 0 {
		t.Fatalf("the lease was not reclaimed after the scan recovered: outstanding=%d jobs=%d, want 0/0", queue.outstanding, queue.outstandingJobs)
	}
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
	queue, waiter := vanishedTestWaiter(server, slicePath, scopeID, now.Add(-time.Hour), true, true)

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
	queue, waiter := vanishedTestWaiter(server, t.TempDir(), staleLeaseTestScopeID("once"), now.Add(-time.Hour), true, true)

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
	queue, waiter := vanishedTestWaiter(server, slicePath, scopeID, now.Add(-time.Hour), true, false)

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
// the WRONG lease. The ledger half is fixed by carrying the exact waiter
// pointer.
//
// The destructive half is subtler and is the reason a vanished candidate makes
// no filesystem call at all: ReapScopeIfEmpty is keyed on the scope-id STRING,
// so it would rmdir the REPLACEMENT's newly created, still-empty scope. A
// pointer-carrying candidate does not fix that on its own.
func TestReleaseStaleGrantedLeasesPassDoesNotReleaseOrReapAReplacementWithTheSameScopeID(t *testing.T) {
	slicePath := staleLeaseTestSliceRoot(t)
	now := time.Now()
	server := staleLeaseTestServer(t, now, time.Second)
	scopeID := staleLeaseTestScopeID("aba")
	// Deliberately NOT vanished: a vanished candidate makes no filesystem call at
	// all, so it could never exercise the destructive half. This is the shape that
	// reaches ReapScopeIfEmpty — a stale lease whose scope exists and is empty.
	queue, stale := vanishedTestWaiter(server, slicePath, scopeID, now.Add(-time.Hour), true, false)
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
// has NO cgroup artifact of any kind, so neither the reap nor the scan can prove
// anything about it, and its only release path is its connection closing.
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
// does NOT prove the job is dead — which is why the counter is called
// `vanished`, an observation, and never `ghost`, a verdict.
//
// The behaviour asserted here is the SAME as the pre-existing empty-reap
// branch's: the lease is reclaimed. The escapee is uncontained by construction,
// its reserve buys nothing, the release is ledger-only, and its memory is still
// charged through max(current - reclaimable, sum of reserves).
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

	queue, _ := vanishedTestWaiter(server, slicePath, scopeID, now.Add(-time.Hour), true, false)

	// The escapee is provably ALIVE before the sweep: without this the test could
	// pass for a dead process and would pin nothing.
	if !vanishedTestProcessAlive(escapee) {
		t.Fatalf("escaped leader %d was not alive before the sweep", escapee)
	}
	snapshot := server.admitSliceSnapshot(slicePath)
	if snapshot.scopeJobs != 1 {
		t.Fatalf("scope-backed jobs = %d, want 1", snapshot.scopeJobs)
	}

	server.releaseStaleGrantedLeasesPass(context.Background())

	if !vanishedTestProcessAlive(escapee) {
		t.Fatalf("escaped leader %d died during the sweep; this test no longer pins the disputed live-leader condition", escapee)
	}
	if queue.outstanding != 0 || queue.outstandingJobs != 0 {
		t.Fatalf("the emptied scope of an escaped leader was not reclaimed: outstanding=%d jobs=%d", queue.outstanding, queue.outstandingJobs)
	}
	if after := server.admitSliceSnapshot(slicePath); after.vanishedJobs != 0 || after.scopeJobs != 0 {
		t.Fatalf("population after reclaim = %d scope / %d vanished, want 0/0", after.scopeJobs, after.vanishedJobs)
	}
}
