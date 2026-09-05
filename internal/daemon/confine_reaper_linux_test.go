//go:build linux

package daemon

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"aira/internal/cgrouptest"
	"aira/internal/runner"
	"aira/internal/testdeadline"
)

// staleLeaseTestSliceRoot returns a REAL, isolated confine slice path for this
// test, published through AIRA_CONFINE_SLICE and then read back through the
// daemon's own resolver.
//
// Positive control (Fable round-4, corrected by build-review): this package
// had never resolved a slice via AIRA_CONFINE_SLICE before AIRA-49 — every
// prior test keys admitQueues by whatever path it constructs, bypassing
// ResolveConfineManagementSlice entirely. releaseStaleGrantedLeasesPass itself
// never calls that resolver (it iterates admitQueues directly, by design, to
// cover every registered slice) -- but this helper still does, specifically so
// the fixture's admitQueues KEY and its real, physically-created scope
// directory PATH are provably the same path, comparing against what resolution
// itself returns (it is EvalSymlinks-canonicalised, so the raw t.Setenv value
// need not match byte-for-byte). A silent mismatch here would key the ledger
// fixture by one path while creating/reaping directories under a different
// one, making every case below pass or fail for the wrong reason regardless of
// whether the sweep logic itself is correct.
//
// t.Setenv forbids t.Parallel in every test that calls this.
func staleLeaseTestSliceRoot(t *testing.T) string {
	t.Helper()
	parent := cgrouptest.IsolatedScopeParent(t)
	t.Setenv("AIRA_CONFINE_SLICE", parent)
	_, resolved, err := runner.ResolveConfineManagementSlice("")
	if err != nil {
		cgrouptest.SkipOrFailRealCgroup(t, "resolve confine management slice %s: %v", parent, err)
	}
	canonical, err := filepath.EvalSymlinks(parent)
	if err != nil {
		t.Fatal(err)
	}
	if resolved != canonical {
		t.Fatalf("AIRA_CONFINE_SLICE resolution landed on %q, want this test's isolated parent %q", resolved, canonical)
	}
	info, err := os.Stat(resolved)
	if err != nil || !info.IsDir() {
		t.Fatalf("resolved slice %s is not a real directory: %v", resolved, err)
	}
	return resolved
}

// staleLeaseTestScopeID mints an id that passes runner.validConfineScopeID, so
// ReapScopeIfEmpty's own validation never masks a wiring failure.
func staleLeaseTestScopeID(name string) string {
	return "CONFINE-" + name + "-5101-" + strconv.FormatInt(time.Now().UnixNano(), 36)
}

// staleLeaseTestScope mkdirs a real cgroup directory for scopeID under a real
// delegated slice — the daemon-package equivalent of the runner package's
// createReaperTestScope (helpers there are unexported and unreachable here).
func staleLeaseTestScope(t *testing.T, slicePath, scopeID string) string {
	t.Helper()
	path := filepath.Join(slicePath, ".aira-"+scopeID)
	if err := os.Mkdir(path, 0o755); err != nil {
		cgrouptest.SkipOrFailRealCgroup(t, "create real stale-lease scope %s: %v", path, err)
	}
	return path
}

// staleLeaseTestSleeper places a live process in cgroupPath and — belt-and-braces
// against a vacuous pass — does not return until the kernel reports the cgroup
// populated.
func staleLeaseTestSleeper(t *testing.T, cgroupPath string) *exec.Cmd {
	t.Helper()
	fd, err := os.OpenFile(cgroupPath, os.O_RDONLY|syscall.O_DIRECTORY|syscall.O_CLOEXEC, 0)
	if err != nil {
		t.Fatal(err)
	}
	command := exec.Command("/bin/sleep", "60")
	command.SysProcAttr = &syscall.SysProcAttr{UseCgroupFD: true, CgroupFD: int(fd.Fd())}
	if err := command.Start(); err != nil {
		_ = fd.Close()
		cgrouptest.SkipOrFailRealCgroup(t, "start stale-lease workload in %s: %v", cgroupPath, err)
	}
	if err := fd.Close(); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(testdeadline.Wait(time.Second))
	for {
		data, readErr := os.ReadFile(filepath.Join(cgroupPath, "cgroup.events"))
		if readErr == nil && strings.Contains(string(data), "populated 1") {
			break
		}
		if time.Now().After(deadline) {
			stopStaleLeaseTestSleeper(command)
			t.Fatalf("workload never populated %s: events=%q err=%v", cgroupPath, data, readErr)
		}
		time.Sleep(10 * time.Millisecond)
	}
	return command
}

func stopStaleLeaseTestSleeper(command *exec.Cmd) {
	if command == nil || command.Process == nil {
		return
	}
	_ = command.Process.Kill()
	_ = command.Wait()
}

// staleLeaseTestServer pins the admission clock so every age assertion below is
// exact rather than wall-clock-tight.
func staleLeaseTestServer(t *testing.T, now time.Time, grace time.Duration) *Server {
	t.Helper()
	server := NewServer(Paths{})
	server.admitNow = func() time.Time { return now }
	server.staleLeaseReleaseGrace = grace
	return server
}

// staleLeaseTestWaiter registers one granted lease on `path`, following the
// hand-built sliceQueue/admitWaiter fixture pattern this package already uses in
// confine_manage_test.go.
func staleLeaseTestWaiter(server *Server, path, scopeID string, enqueued, grantedAt time.Time) (*sliceQueue, *admitWaiter) {
	queue := &sliceQueue{path: path, server: server, kick: make(chan struct{}, 1), stop: make(chan struct{})}
	waiter := &admitWaiter{
		seq: 1, reserve: 64, state: admitGranted, accounted: true,
		grantedCh: make(chan struct{}), enqueued: enqueued, grantedAt: grantedAt,
		scopeID: scopeID, name: "stale", owner: "session-a",
	}
	queue.waiters = []*admitWaiter{waiter}
	queue.outstanding, queue.outstandingJobs = 64, 1
	server.admitQueues[path] = queue
	return queue, waiter
}

func staleLeaseTestListed(server *Server, path, scopeID string) bool {
	for _, entry := range server.activeConfines(path) {
		if entry.ScopeID == scopeID {
			return true
		}
	}
	return false
}

// verifies: the AIRA-49 backstop actually reclaims a lease that has been GRANTED
// past its TTL once the kernel itself confirms the scope is empty — and reclaims
// the ledger's ACCOUNTING, not merely its listing (Sol round-4: a registry-listing
// check alone would pass against an impl that forgot outstanding/outstandingJobs
// and left the slice permanently under-admitting).
func TestReleaseStaleGrantedLeasesPassReleasesAWaiterGrantedLongAgoOnceItsScopeReapsEmpty(t *testing.T) {
	slicePath := staleLeaseTestSliceRoot(t)
	now := time.Now()
	server := staleLeaseTestServer(t, now, time.Second)
	scopeID := staleLeaseTestScopeID("released")
	scopePath := staleLeaseTestScope(t, slicePath, scopeID)
	queue, waiter := staleLeaseTestWaiter(server, slicePath, scopeID, now.Add(-2*time.Hour), now.Add(-time.Hour))

	server.releaseStaleGrantedLeasesPass(context.Background())

	if staleLeaseTestListed(server, slicePath, scopeID) {
		t.Fatalf("scope %s still listed as an active confine after the sweep", scopeID)
	}
	if _, err := os.Stat(scopePath); !os.IsNotExist(err) {
		t.Fatalf("scope directory %s survived the sweep: %v", scopePath, err)
	}
	if queue.outstanding != 0 || queue.outstandingJobs != 0 {
		t.Fatalf("ledger accounting not reclaimed: outstanding=%d jobs=%d, want 0/0", queue.outstanding, queue.outstandingJobs)
	}
	if waiter.state != admitReleased {
		t.Fatalf("waiter state=%v, want released", waiter.state)
	}
}

// verifies: a freshly granted lease is never reclaimed. Its scope is legitimately
// empty for the sub-second window between scope creation and child placement, so
// emptiness alone must never release.
func TestReleaseStaleGrantedLeasesPassLeavesARecentlyGrantedWaiterAlone(t *testing.T) {
	slicePath := staleLeaseTestSliceRoot(t)
	now := time.Now()
	server := staleLeaseTestServer(t, now, time.Hour)
	scopeID := staleLeaseTestScopeID("fresh")
	scopePath := staleLeaseTestScope(t, slicePath, scopeID)
	queue, _ := staleLeaseTestWaiter(server, slicePath, scopeID, now.Add(-2*time.Hour), now)

	server.releaseStaleGrantedLeasesPass(context.Background())

	if !staleLeaseTestListed(server, slicePath, scopeID) {
		t.Fatalf("freshly granted lease %s was reclaimed", scopeID)
	}
	if _, err := os.Stat(scopePath); err != nil {
		t.Fatalf("freshly granted scope directory removed: %v", err)
	}
	if queue.outstanding != 64 || queue.outstandingJobs != 1 {
		t.Fatalf("ledger accounting changed: outstanding=%d jobs=%d, want 64/1", queue.outstanding, queue.outstandingJobs)
	}
}

// verifies: Task 2's subtree-aware safety property survives the daemon-level
// wiring end to end. The candidate's own leaf is empty while a nested child holds
// a live process; a leaf-only emptiness check (AIRA-49 v1) would release the lease
// of a job that is still running.
func TestReleaseStaleGrantedLeasesPassLeavesALiveNestedChildScopeAlone(t *testing.T) {
	slicePath := staleLeaseTestSliceRoot(t)
	now := time.Now()
	server := staleLeaseTestServer(t, now, time.Second)
	scopeID := staleLeaseTestScopeID("nested")
	scopePath := staleLeaseTestScope(t, slicePath, scopeID)
	child := filepath.Join(scopePath, "live")
	if err := os.Mkdir(child, 0o755); err != nil {
		cgrouptest.SkipOrFailRealCgroup(t, "create nested live cgroup %s: %v", child, err)
	}
	sleeper := staleLeaseTestSleeper(t, child)
	t.Cleanup(func() { stopStaleLeaseTestSleeper(sleeper) })
	queue, _ := staleLeaseTestWaiter(server, slicePath, scopeID, now.Add(-2*time.Hour), now.Add(-time.Hour))

	server.releaseStaleGrantedLeasesPass(context.Background())

	if !staleLeaseTestListed(server, slicePath, scopeID) {
		t.Fatalf("lease %s with a live nested child was reclaimed", scopeID)
	}
	for _, path := range []string{scopePath, child} {
		if _, err := os.Stat(path); err != nil {
			t.Fatalf("live nested tree changed at %s: %v", path, err)
		}
	}
	if queue.outstanding != 64 || queue.outstandingJobs != 1 {
		t.Fatalf("ledger accounting changed: outstanding=%d jobs=%d, want 64/1", queue.outstanding, queue.outstandingJobs)
	}
}

// verifies: the age decision reads grantedAt and NEVER enqueued. This is the
// direct regression test for AIRA-49's v3 defect — a job that queued for an hour
// under admission contention and was granted a moment ago is NOT stale, even
// though enqueued alone says otherwise. The old-grant control in the same queue
// keeps the assertion from passing vacuously against a selector that returns
// nothing at all.
func TestStaleGrantedLeasesNeverReadsEnqueuedForItsAgeDecision(t *testing.T) {
	now := time.Now()
	server := staleLeaseTestServer(t, now, 15*time.Minute)
	path := t.TempDir()
	queue := &sliceQueue{path: path, server: server, kick: make(chan struct{}, 1), stop: make(chan struct{})}
	queuedLong := &admitWaiter{
		seq: 1, reserve: 64, state: admitGranted, accounted: true, grantedCh: make(chan struct{}),
		enqueued: now.Add(-time.Hour), grantedAt: now.Add(-time.Second),
		scopeID: staleLeaseTestScopeID("queued-long"), name: "queued-long", owner: "session-a",
	}
	control := &admitWaiter{
		seq: 2, reserve: 64, state: admitGranted, accounted: true, grantedCh: make(chan struct{}),
		enqueued: now.Add(-time.Hour), grantedAt: now.Add(-time.Hour),
		scopeID: staleLeaseTestScopeID("control-old-grant"), name: "control-old-grant", owner: "session-a",
	}
	queue.waiters = []*admitWaiter{queuedLong, control}
	server.admitQueues[path] = queue

	candidates := server.staleGrantedLeases(15 * time.Minute)

	if !staleLeaseCandidatesContain(candidates, control.scopeID) {
		t.Fatalf("old-grant control %s not selected; candidates=%+v", control.scopeID, candidates)
	}
	if staleLeaseCandidatesContain(candidates, queuedLong.scopeID) {
		t.Fatalf("waiter granted a second ago selected as stale from its enqueue time; candidates=%+v", candidates)
	}
}

// verifies: the sweep covers EVERY registered admission-queue slice, not just the
// daemon's default one. Sol round-4: the single-slice draft silently missed every
// lease admitted against an explicitly-specified --slice.
func TestStaleGrantedLeasesCoversEveryRegisteredSliceNotJustTheDefault(t *testing.T) {
	now := time.Now()
	server := staleLeaseTestServer(t, now, time.Second)
	first := cgrouptest.IsolatedScopeParent(t)
	second := cgrouptest.IsolatedScopeParent(t)
	if first == second {
		t.Fatalf("isolated parents collided: %s", first)
	}
	firstID := staleLeaseTestScopeID("slice-one")
	secondID := staleLeaseTestScopeID("slice-two")
	staleLeaseTestScope(t, first, firstID)
	staleLeaseTestScope(t, second, secondID)
	staleLeaseTestWaiter(server, first, firstID, now.Add(-2*time.Hour), now.Add(-time.Hour))
	// staleLeaseTestWaiter keys by path, so the second registration is a second,
	// distinct queue rather than an overwrite.
	staleLeaseTestWaiter(server, second, secondID, now.Add(-2*time.Hour), now.Add(-time.Hour))

	candidates := server.staleGrantedLeases(time.Second)

	for _, want := range []struct{ path, scopeID string }{{first, firstID}, {second, secondID}} {
		found := false
		for _, candidate := range candidates {
			if candidate.path == want.path && candidate.scopeID == want.scopeID {
				found = true
			}
		}
		if !found {
			t.Fatalf("no candidate for slice %s scope %s; candidates=%+v", want.path, want.scopeID, candidates)
		}
	}
}

// verifies: the grantedAt.IsZero() fallback is fail-closed — "no grant record
// means never released by this pass" — and never quietly falls back to enqueued.
// Every hand-built admitGranted fixture elsewhere in this suite naturally carries
// a zero grantedAt (the field did not exist before AIRA-49), so this is a real
// drift risk, not a hypothetical. The old-grant control keeps it from passing
// vacuously.
func TestStaleGrantedLeasesSkipsAWaiterWithNoGrantedAtRecordEvenIfEnqueuedIsVeryOld(t *testing.T) {
	now := time.Now()
	server := staleLeaseTestServer(t, now, 15*time.Minute)
	path := t.TempDir()
	queue := &sliceQueue{path: path, server: server, kick: make(chan struct{}, 1), stop: make(chan struct{})}
	noRecord := &admitWaiter{
		seq: 1, reserve: 64, state: admitGranted, accounted: true, grantedCh: make(chan struct{}),
		enqueued: now.Add(-time.Hour), // grantedAt deliberately left as its zero value.
		scopeID:  staleLeaseTestScopeID("no-grant-record"), name: "no-grant-record", owner: "session-a",
	}
	control := &admitWaiter{
		seq: 2, reserve: 64, state: admitGranted, accounted: true, grantedCh: make(chan struct{}),
		enqueued: now.Add(-time.Hour), grantedAt: now.Add(-time.Hour),
		scopeID: staleLeaseTestScopeID("control-old-grant"), name: "control-old-grant", owner: "session-a",
	}
	queue.waiters = []*admitWaiter{noRecord, control}
	server.admitQueues[path] = queue

	candidates := server.staleGrantedLeases(15 * time.Minute)

	if !staleLeaseCandidatesContain(candidates, control.scopeID) {
		t.Fatalf("old-grant control %s not selected; candidates=%+v", control.scopeID, candidates)
	}
	if staleLeaseCandidatesContain(candidates, noRecord.scopeID) {
		t.Fatalf("waiter with no grantedAt record selected from its enqueue time; candidates=%+v", candidates)
	}
}

func staleLeaseCandidatesContain(candidates []staleLeaseCandidate, scopeID string) bool {
	for _, candidate := range candidates {
		if candidate.scopeID == scopeID {
			return true
		}
	}
	return false
}
