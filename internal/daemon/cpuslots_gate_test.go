//go:build linux

package daemon

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"aira/internal/runner"
)

// AIRA-64 gate tests.
//
// Every existing worker-admit test uses a synthetic outer scope like "/outer",
// which is not a `.aira-CONFINE-*` child, so its CPU dimension is honestly
// unevaluated and the RAM behaviour those tests pin is untouched. These tests
// use real confine-shaped paths so the gate is actually exercised.

const testSlice = "/slice"
const testOuterA = "/slice/.aira-CONFINE-a"
const testOuterB = "/slice/.aira-CONFINE-b"

// cpuSlotsFixture installs an in-memory slice tree behind the CPU scan seam.
// workers maps an outer scope to its (total directories, liveForFloor) pair, so
// a test can make the two counts DIFFER — which is the whole point of the split.
type cpuSlotsFixture struct {
	mu      sync.Mutex
	total   map[string]int
	live    map[string]int
	scans   atomic.Int64
	err     error
	release chan struct{} // when non-nil, the scan blocks on it (barrier)
}

func newCPUSlotsFixture() *cpuSlotsFixture {
	return &cpuSlotsFixture{total: map[string]int{}, live: map[string]int{}}
}

func (f *cpuSlotsFixture) install(server *Server, capacity int) *cpuSlotsFixture {
	server.cpuSlotsCapacity = capacity
	server.cpuSlotsGrace = testGrace
	server.cpuSlotsScanInterval = time.Hour // cache never expires unless forced
	// The slice resolver is a real cgroupfs lookup in production; these tests
	// use synthetic paths, so it is stubbed to accept the test slice and
	// nothing else. Stubbing it to accept EVERYTHING would silently disable the
	// containment check the resolver exists to provide.
	server.admitResolveSlice = func(slice string) (string, bool, string) {
		if slice == testSlice {
			return testSlice, true, ""
		}
		return "", false, "slice-not-found"
	}
	server.cpuSlotsScan = func(root string) (cpuSlotsSnapshot, error) {
		f.scans.Add(1)
		if f.release != nil {
			<-f.release
		}
		f.mu.Lock()
		defer f.mu.Unlock()
		if f.err != nil {
			return cpuSlotsSnapshot{}, f.err
		}
		snapshot := cpuSlotsSnapshot{liveForFloor: map[string]int{}}
		for outer, count := range f.total {
			if count == 0 {
				continue
			}
			snapshot.total += count
			snapshot.scopes++
			snapshot.liveForFloor[outer] = f.live[outer]
		}
		return snapshot, nil
	}
	// The daemon's own scope creation must move this fixture's counts, or the
	// concurrency test would be measuring nothing.
	inner := server.workerScopeCreate
	server.workerScopeCreate = func(ctx context.Context, outer, id string, memMax, memHigh int64) (string, error) {
		path, err := inner(ctx, outer, id, memMax, memHigh)
		if err == nil {
			f.mu.Lock()
			f.total[outer]++
			f.live[outer]++
			f.mu.Unlock()
		}
		return path, err
	}
	return f
}

func (f *cpuSlotsFixture) set(outer string, total, live int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.total[outer], f.live[outer] = total, live
}

// cpuGateServer builds a server whose RAM decision always grants, so any denial
// observed is unambiguously the CPU gate's.
func cpuGateServer(t *testing.T) *Server {
	t.Helper()
	server := NewServer(Paths{})
	_ = newWorkerScopeTree().install(server)
	server.admitReadMemory = admitReadMemoryFixture(map[string]int64{}, 1<<40)
	server.admitReadWorkerSupervisorMemory = admitReadWorkerSupervisorMemoryFixture(map[string]int64{})
	server.workerAdmitHeadroom = 0
	return server
}

func admitOn(t *testing.T, server *Server, outer string) WorkerAdmitResponse {
	t.Helper()
	return evaluateWorkerAdmitForTest(t, server, workerAdmitRequest{
		jobID: "job", outerScope: outer, estimatedBytes: 1 << 20, maxWaitMS: 0,
	})
}

// verifies: AIRA-64 §9.1 — under capacity the gate does not deny what RAM would grant.
func TestCPUGateGrantsUnderCapacity(t *testing.T) {
	server := cpuGateServer(t)
	fixture := newCPUSlotsFixture().install(server, 4)
	fixture.set(testOuterA, 2, 2)
	response := admitOn(t, server, testOuterA)
	if response.State != runner.WorkerAdmitStateGranted {
		t.Fatalf("under capacity must grant: %+v", response)
	}
	if response.CPUSlots != runner.WorkerAdmitCPUSlotsOK {
		t.Fatalf("a governed grant must report cpu_slots=ok, got %q", response.CPUSlots)
	}
}

// verifies: AIRA-64 §9.2, §9.11 — at capacity, a scope that already has a
// worker is denied, with the EXACT disposition the client depends on.
func TestCPUGateDeniesAtCapacityWithContendedClass(t *testing.T) {
	server := cpuGateServer(t)
	fixture := newCPUSlotsFixture().install(server, 4)
	fixture.set(testOuterA, 3, 3)
	fixture.set(testOuterB, 1, 1)
	response := admitOn(t, server, testOuterA)
	// This triple is load-bearing, not cosmetic: `contended` is the only class
	// that means "retry, containment preserved". `admission-unusable` would
	// make the supervisor run the whole suite UNCONFINED; `request-invalid`
	// would mark its remaining queue `unevaluated`.
	if response.State != runner.WorkerAdmitStateDenied ||
		response.Class != runner.WorkerAdmitClassContended ||
		response.Reason != runner.WorkerAdmitReasonCPUSlotsSaturated {
		t.Fatalf("want denied/contended/cpu-slots-saturated, got %+v", response)
	}
}

// verifies: AIRA-64 §4.5, §9.3 — THE LIVENESS FLOOR. Without this the change is
// a regression: an arriving job would stall instead of merely running slowly.
func TestCPUGateFloorGrantsToAScopeWithNoLiveWorker(t *testing.T) {
	server := cpuGateServer(t)
	fixture := newCPUSlotsFixture().install(server, 2)
	fixture.set(testOuterA, 5, 5) // incumbent already over capacity
	response := admitOn(t, server, testOuterB)
	if response.State != runner.WorkerAdmitStateGranted {
		t.Fatalf("a scope with no live worker must always get one: %+v", response)
	}
}

// verifies: AIRA-64 §4.4.2, §9.15 — Sol round 3 P0. The floor is rate-limited to
// once per outer scope per grace window, so a supervisor stalled between grant
// and placement cannot let a second one re-claim the floor without bound.
func TestCPUGateFloorIsRateLimitedPerOuterScope(t *testing.T) {
	server := cpuGateServer(t)
	now := time.Now()
	server.admitNow = func() time.Time { return now }
	fixture := newCPUSlotsFixture().install(server, 1)
	fixture.set(testOuterA, 4, 4)

	first := admitOn(t, server, testOuterB)
	if first.State != runner.WorkerAdmitStateGranted {
		t.Fatalf("first floor grant: %+v", first)
	}
	// Simulate the round-3 construction: the granted scope exists but has NOT
	// been placed into and has aged out, so liveForFloor reads zero again.
	fixture.set(testOuterB, 1, 0)

	second := admitOn(t, server, testOuterB)
	if second.State != runner.WorkerAdmitStateDenied || second.Reason != runner.WorkerAdmitReasonCPUSlotsSaturated {
		t.Fatalf("a second floor grant inside one grace window must be denied: %+v", second)
	}
	// Past the window the floor reopens -- the limit is a rate limit, not a
	// permanent lockout, or a genuinely abandoned scope would stall its job.
	now = now.Add(testGrace + time.Second)
	third := admitOn(t, server, testOuterB)
	if third.State != runner.WorkerAdmitStateGranted {
		t.Fatalf("past the grace window the floor must reopen: %+v", third)
	}
}

// verifies: AIRA-64 §9.9 — an unestablished CPU reading admits on the RAM
// decision alone AND says so. It is never a zero and never a silent pass.
func TestCPUGateUnestablishedReadingAdmitsAndReportsIt(t *testing.T) {
	t.Run("outer scope is not a confine scope", func(t *testing.T) {
		server := cpuGateServer(t)
		newCPUSlotsFixture().install(server, 1)
		response := admitOn(t, server, "/not-a-confine-scope")
		if response.State != runner.WorkerAdmitStateGranted {
			t.Fatalf("CPU-unevaluated must not deny (RAM decides): %+v", response)
		}
		if response.CPUSlots != runner.WorkerAdmitCPUSlotsUnevaluated {
			t.Fatalf("want cpu_slots=unevaluated, got %q", response.CPUSlots)
		}
	})
	t.Run("scan fails", func(t *testing.T) {
		server := cpuGateServer(t)
		fixture := newCPUSlotsFixture().install(server, 1)
		fixture.err = context.DeadlineExceeded
		response := admitOn(t, server, testOuterA)
		if response.State != runner.WorkerAdmitStateGranted || response.CPUSlots != runner.WorkerAdmitCPUSlotsUnevaluated {
			t.Fatalf("a failed scan must admit and report unevaluated: %+v", response)
		}
	})
	t.Run("capacity not established", func(t *testing.T) {
		server := cpuGateServer(t)
		fixture := newCPUSlotsFixture().install(server, 0)
		fixture.set(testOuterA, 99, 99)
		response := admitOn(t, server, testOuterA)
		if response.State != runner.WorkerAdmitStateGranted || response.CPUSlots != runner.WorkerAdmitCPUSlotsUnevaluated {
			t.Fatalf("an unestablished capacity is unevaluated, not zero room: %+v", response)
		}
	})
}

// verifies: AIRA-64 §4.8 — the gate REFUSES to count a slice the admission
// resolver does not recognise, rather than scanning whatever directory a caller
// happened to name the parent of.
//
// Added after mutation testing: removing the resolver call from cpuSlotsDecide
// SURVIVED the suite, because the only test touching the resolver called it
// directly rather than through the gate. Without it, a request naming
// `/anywhere/.aira-CONFINE-x` has the gate scan `/anywhere`, find nothing, and
// admit freely — a fail-open dressed as a governed grant.
func TestCPUGateRefusesASliceTheResolverDoesNotRecognise(t *testing.T) {
	server := cpuGateServer(t)
	fixture := newCPUSlotsFixture().install(server, 1)
	// The real slice is saturated, so anything that IS governed must be denied.
	fixture.set(testOuterA, 5, 5)
	if got := admitOn(t, server, testOuterA); got.Reason != runner.WorkerAdmitReasonCPUSlotsSaturated {
		t.Fatalf("precondition: the governed slice must be saturated, got %+v", got)
	}

	// Same confine-shaped basename, a parent the resolver rejects.
	response := admitOn(t, server, "/somewhere-else/.aira-CONFINE-a")
	if response.State != runner.WorkerAdmitStateGranted {
		t.Fatalf("an unresolvable slice is unevaluated, and unevaluated admits on RAM: %+v", response)
	}
	if response.CPUSlots != runner.WorkerAdmitCPUSlotsUnevaluated {
		t.Fatalf("a request whose slice the resolver rejects must report cpu_slots=unevaluated, "+
			"not a governed %q — otherwise the gate silently scans an unrelated directory, "+
			"finds nothing, and admits freely", response.CPUSlots)
	}
}

// verifies: AIRA-64 §4.8 — the slice the gate SCANS is the resolver's
// canonicalised answer, not the path derived from the caller's spelling.
//
// Added after mutation testing: calling the resolver and then ignoring its
// result SURVIVED, because the test fixture's resolver returned its input
// unchanged, so "candidate" and "resolved" were the same string. Here the
// resolver deliberately canonicalises to a DIFFERENT path that is the only one
// the tree fixture knows about, so scanning the unresolved candidate finds an
// empty slice and grants where it must deny.
func TestCPUGateScansTheResolverCanonicalisedSlice(t *testing.T) {
	const canonical = "/canonical-slice"
	server := cpuGateServer(t)
	fixture := newCPUSlotsFixture().install(server, 1)
	server.admitResolveSlice = func(slice string) (string, bool, string) {
		if slice == testSlice {
			return canonical, true, ""
		}
		return "", false, "slice-not-found"
	}
	// The tree exists ONLY under the canonical path, and it is saturated. The
	// scope key must be rebuilt from the canonical root too, or the floor lookup
	// misses and grants.
	fixture.set(canonical+"/.aira-CONFINE-a", 4, 4)

	response := admitOn(t, server, testOuterA)
	if response.State != runner.WorkerAdmitStateDenied ||
		response.Reason != runner.WorkerAdmitReasonCPUSlotsSaturated {
		t.Fatalf("the gate must scan the resolver's canonical slice (%s); scanning the "+
			"caller-derived path instead finds an empty tree and admits: %+v", canonical, response)
	}
}

// verifies: AIRA-64 §9.10 — the CPU gate composes ONE way: it may deny what RAM
// would grant, never grant what RAM would deny.
func TestCPUGateNeverTurnsARAMDenialIntoAGrant(t *testing.T) {
	server := cpuGateServer(t)
	newCPUSlotsFixture().install(server, 1000) // CPU wide open
	server.admitReadMemory = admitReadMemoryFixture(map[string]int64{}, 1000)
	response := evaluateWorkerAdmitForTest(t, server, workerAdmitRequest{
		jobID: "job", outerScope: testOuterA, estimatedBytes: 5000, maxWaitMS: 0,
	})
	if response.State == runner.WorkerAdmitStateGranted {
		t.Fatalf("a RAM denial must survive a wide-open CPU gate: %+v", response)
	}
}

// verifies: AIRA-64 §9.12 — Sol round 2 P1/P2. A SATURATED request must deny
// from the cached snapshot without forcing EITHER expensive rescan. Counting
// only one seam is what made the v2 version of this test porous.
func TestCPUGateSaturatedDenialForcesNoRescan(t *testing.T) {
	server := cpuGateServer(t)
	// Count the RAM seam independently: counting only ONE of the two scans is
	// what made the v2 version of this test porous (Sol round 2 P2).
	var ramScans atomic.Int64
	innerScan := server.workerScopeScan
	server.workerScopeScan = func(outer string) (workerScopeChildren, error) {
		ramScans.Add(1)
		return innerScan(outer)
	}
	fixture := newCPUSlotsFixture().install(server, 2)
	fixture.set(testOuterA, 3, 3)

	// Prime both caches with one admitted request under a DIFFERENT scope.
	fixture.set(testOuterB, 0, 0)
	if got := admitOn(t, server, testOuterB); got.State != runner.WorkerAdmitStateGranted {
		t.Fatalf("priming grant: %+v", got)
	}
	server.cpuSlotsScanInterval = time.Hour
	server.workerScopeScanInterval = time.Hour
	_ = admitOn(t, server, testOuterA) // populate the RAM cache for this scope

	ramBefore, cpuBefore := ramScans.Load(), fixture.scans.Load()
	for i := 0; i < 20; i++ {
		if got := admitOn(t, server, testOuterA); got.Reason != runner.WorkerAdmitReasonCPUSlotsSaturated {
			t.Fatalf("poll %d: want cpu-slots-saturated, got %+v", i, got)
		}
	}
	if ram := ramScans.Load() - ramBefore; ram != 0 {
		t.Fatalf("a saturated poll must force NO RAM rescan (AIRA-61 shape), got %d over 20 polls", ram)
	}
	if cpu := fixture.scans.Load() - cpuBefore; cpu != 0 {
		t.Fatalf("a saturated poll must force NO CPU rescan either, got %d over 20 polls", cpu)
	}
}

// verifies: AIRA-64 §9.13 — a GRANT is always evaluated against a FRESH
// snapshot, never the cache, because the cache can be stale-LOW exactly there:
// a worker scope created since the last scan is invisible to it.
//
// CORRECTED AFTER MUTATION TESTING. The first version of this test simply
// asserted that the scan counter increased across a granting request, and the
// mutant that drops `force` SURVIVED it — because the cached read performs a
// scan too whenever the cache is cold, so the counter rose either way. It
// proved nothing about `force`.
//
// This version makes the cache STALE-LOW on purpose and asserts on the VERDICT:
// the cache says there is room, the tree says there is not, and only a forced
// rescan can tell the difference. Against an implementation that trusts the
// cache here, this grants and fails.
func TestCPUGateGrantForcesAFreshSnapshotAndSeesAStaleLowCache(t *testing.T) {
	const capacity = 4
	server := cpuGateServer(t)
	fixture := newCPUSlotsFixture().install(server, capacity)
	fixture.set(testOuterA, 1, 1)

	// Warm the cache while there is genuinely room, and keep it warm (the scan
	// interval is an hour in this fixture).
	root, _, ok := cpuSlotsScanRoot(testOuterA)
	if !ok {
		t.Fatal("test scope must be a confine scope")
	}
	if _, err := server.cpuSlotsSnapshotFor(root, false); err != nil {
		t.Fatal(err)
	}

	// The tree fills up BEHIND the cache: no invalidation, no interval expiry.
	// The cached reading still says 1 worker; the truth is `capacity`.
	fixture.set(testOuterA, capacity, capacity)

	got := admitOn(t, server, testOuterA)
	if got.State != runner.WorkerAdmitStateDenied || got.Reason != runner.WorkerAdmitReasonCPUSlotsSaturated {
		t.Fatalf("a granting request must re-read the tree rather than trust a stale-low cache; "+
			"got %+v (the cache said 1 worker, the tree holds %d)", got, capacity)
	}
}

// verifies: AIRA-64 §9.14 — Sol P1-8 and round 2 P2. Racing requesters are each
// SEEDED with a live worker so none is floor-entitled, and the assertion is on
// the resulting tree rather than the count of returned grants.
func TestCPUGateHoldsCapacityUnderConcurrentAdmission(t *testing.T) {
	const capacity = 8
	server := cpuGateServer(t)
	fixture := newCPUSlotsFixture().install(server, capacity)
	scopes := []string{
		"/slice/.aira-CONFINE-1", "/slice/.aira-CONFINE-2", "/slice/.aira-CONFINE-3",
		"/slice/.aira-CONFINE-4", "/slice/.aira-CONFINE-5", "/slice/.aira-CONFINE-6",
	}
	// Seed each scope with one live worker: nobody is floor-entitled, so every
	// grant below must come from real capacity.
	for _, scope := range scopes {
		fixture.set(scope, 1, 1)
	}
	start := make(chan struct{})
	var group sync.WaitGroup
	for _, scope := range scopes {
		for i := 0; i < 4; i++ {
			group.Add(1)
			go func(scope string) {
				defer group.Done()
				<-start
				_, _ = server.evaluateWorkerAdmit(context.Background(), workerAdmitRequest{
					jobID: "job", outerScope: scope, estimatedBytes: 1 << 20, maxWaitMS: 0,
				})
			}(scope)
		}
	}
	close(start)
	group.Wait()

	fixture.mu.Lock()
	total := 0
	for _, count := range fixture.total {
		total += count
	}
	fixture.mu.Unlock()
	// Nobody was floor-entitled, so the bound here is exactly capacity.
	if total > capacity {
		t.Fatalf("concurrent admission exceeded capacity: %d live workers > %d", total, capacity)
	}
	if total < len(scopes) {
		t.Fatalf("nothing was admitted at all (%d) -- the test would pass vacuously", total)
	}
}

// verifies: AIRA-64 §9.16 — a speculative request (max_wait_ms == 0) never
// waits on a lock another job holds; it reports busy immediately.
func TestCPUGateSpeculativeRequestNeverBlocksOnTheGate(t *testing.T) {
	server := cpuGateServer(t)
	fixture := newCPUSlotsFixture().install(server, 100)
	fixture.set(testOuterA, 1, 1)
	// Hold the gate as another job would.
	server.cpuSlotsGate <- struct{}{}
	defer func() { <-server.cpuSlotsGate }()

	done := make(chan WorkerAdmitResponse, 1)
	go func() {
		response, _ := server.evaluateWorkerAdmit(context.Background(), workerAdmitRequest{
			jobID: "job", outerScope: testOuterA, estimatedBytes: 1 << 20, maxWaitMS: 0,
		})
		done <- response
	}()
	select {
	case response := <-done:
		if response.State != runner.WorkerAdmitStateDenied ||
			response.Class != runner.WorkerAdmitClassContended ||
			response.Reason != runner.WorkerAdmitReasonAdmitLocksBusy {
			t.Fatalf("want denied/contended/admit-locks-busy, got %+v", response)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("a speculative request blocked on a gate another job holds -- this freezes the supervisor's single-threaded dispatch loop")
	}
}

// verifies: AIRA-64 §13(b), AIRA-41 — a FREE gate is taken without consulting
// ctx, so the gate adds no new abort point between the RAM decision and
// CreateWorkerScope.
//
// This exists because the first implementation copied acquireWorkerScope's
// ctx-before-select pre-check into the gate, which made
// TestWorkerAdmitConnectionKeepsScopeChargedWhenResponseWriteFails flaky: that
// test pins AIRA-41's invariant that a grant whose response write fails still
// leaves its scope on the tree and charged, and a peer vanishing mid-evaluate
// would now abort before the scope existed. It failed in a full-suite run and
// passed in isolation.
//
// Mutation testing then showed why a regression test was needed rather than
// relying on that flake: reintroducing the pre-check SURVIVED the suite,
// because a race is not a reliable detector. This test removes the race by
// cancelling the peer at a DETERMINISTIC point — inside the outer-scope lock,
// after the RAM reads, immediately before the gate.
func TestCPUGateTakesAFreeGateWithoutConsultingContext(t *testing.T) {
	server := cpuGateServer(t)
	fixture := newCPUSlotsFixture().install(server, 100)
	fixture.set(testOuterA, 1, 1)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	// The supervisor-scope read runs inside the outer-scope lock and before the
	// CPU gate, so cancelling here lands the peer-gone state in exactly the
	// window the gate must not react to.
	inner := server.admitReadWorkerSupervisorMemory
	server.admitReadWorkerSupervisorMemory = func(path string) (int64, int64, bool, string) {
		cancel()
		return inner(path)
	}

	response, proceed := server.evaluateWorkerAdmit(ctx, workerAdmitRequest{
		jobID: "job", outerScope: testOuterA, estimatedBytes: 1 << 20, maxWaitMS: 30000,
	})
	if !proceed || response.State != runner.WorkerAdmitStateGranted {
		t.Fatalf("a peer that vanishes after the RAM decision must not make the CPU gate abort "+
			"before the scope is created (AIRA-41: the scope must exist and keep charging); "+
			"got proceed=%v response=%+v", proceed, response)
	}
}

// verifies: AIRA-64 §4.10.1(b) — a speculative request never blocks on the
// OUTER-SCOPE lock either, only on the CPU gate.
//
// Added after mutation testing showed that changing acquireWorkerScopeTry's
// call site back to blocking SURVIVED the suite: nothing held that lock while a
// zero-wait request was in flight, so both behaviours looked identical (Sol
// build-review). The outer-scope lock is held across a cgroupfs scan and a scope
// creation, so blocking on it is exactly the multi-second dispatch-loop freeze
// speculative admission exists to avoid.
func TestCPUGateSpeculativeRequestNeverBlocksOnTheOuterScopeLock(t *testing.T) {
	server := cpuGateServer(t)
	fixture := newCPUSlotsFixture().install(server, 100)
	fixture.set(testOuterA, 1, 1)

	// Hold the outer-scope lock the way a concurrent request for the same scope
	// would, and never release it.
	state := server.workerScopeFor(testOuterA)
	state.lock <- struct{}{}

	done := make(chan WorkerAdmitResponse, 1)
	go func() {
		response, _ := server.evaluateWorkerAdmit(context.Background(), workerAdmitRequest{
			jobID: "job", outerScope: testOuterA, estimatedBytes: 1 << 20, maxWaitMS: 0,
		})
		done <- response
	}()
	select {
	case response := <-done:
		if response.State != runner.WorkerAdmitStateDenied ||
			response.Class != runner.WorkerAdmitClassContended ||
			response.Reason != runner.WorkerAdmitReasonAdmitLocksBusy {
			t.Fatalf("want denied/contended/admit-locks-busy, got %+v", response)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("a speculative request blocked on the outer-scope lock -- that lock is held " +
			"across a cgroupfs scan and a scope creation, so this freezes the supervisor's " +
			"single-threaded dispatch loop behind another job")
	}
}

// verifies: AIRA-64 §4.10.1(b) — a NON-speculative request still waits on the
// outer-scope lock rather than reporting a spurious denial. The try-acquire is
// selected by max_wait_ms == 0, not applied unconditionally; without this, the
// mutant that makes every request try-only would look correct.
func TestNonSpeculativeRequestStillWaitsForTheOuterScopeLock(t *testing.T) {
	server := cpuGateServer(t)
	fixture := newCPUSlotsFixture().install(server, 100)
	fixture.set(testOuterA, 1, 1)

	state := server.workerScopeFor(testOuterA)
	state.lock <- struct{}{}

	done := make(chan WorkerAdmitResponse, 1)
	go func() {
		response, _ := server.evaluateWorkerAdmit(context.Background(), workerAdmitRequest{
			jobID: "job", outerScope: testOuterA, estimatedBytes: 1 << 20, maxWaitMS: 30000,
		})
		done <- response
	}()
	select {
	case response := <-done:
		t.Fatalf("a non-speculative request must WAIT for the lock, not report %+v", response)
	case <-time.After(150 * time.Millisecond):
	}
	<-state.lock // release; the waiter may now proceed
	select {
	case response := <-done:
		if response.State != runner.WorkerAdmitStateGranted {
			t.Fatalf("once the lock frees, the waiting request must proceed: %+v", response)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the waiting request never proceeded after the lock was released")
	}
}

// verifies: AIRA-64 §13(e) — Sol build-review P0. A CACHED read must not block
// behind another caller's in-progress scan.
//
// The first implementation held cpuSlotsMu across the whole cgroup walk. That
// mutex sits in FRONT of both try-acquired locks on the cached path, so a
// speculative request could still freeze the supervisor's single-threaded
// dispatch loop behind an unrelated job's slow scan — the try-acquire bought
// nothing. The mutex now guards only map access.
func TestCPUSlotsCachedReadDoesNotBlockBehindAnInProgressScan(t *testing.T) {
	server := cpuGateServer(t)
	fixture := newCPUSlotsFixture().install(server, 100)
	fixture.set(testOuterA, 1, 1)
	server.cpuSlotsScanInterval = time.Hour

	// Warm the cache while scanning is still free.
	if _, err := server.cpuSlotsSnapshotFor(testSlice, false); err != nil {
		t.Fatal(err)
	}

	// Now park every subsequent scan, and start one that will sit in it.
	release := make(chan struct{})
	fixture.release = release
	scanning := make(chan struct{})
	go func() {
		close(scanning)
		_, _ = server.cpuSlotsSnapshotFor(testSlice, true) // forced: enters the parked scan
	}()
	<-scanning
	// Give the scanning goroutine time to actually be inside the scan.
	time.Sleep(50 * time.Millisecond)

	done := make(chan struct{})
	go func() {
		_, _ = server.cpuSlotsSnapshotFor(testSlice, false) // cached: must not wait
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		close(release)
		t.Fatal("a cached CPU-slot read blocked behind another caller's in-progress scan; " +
			"that mutex sits in front of both try-acquired locks, so this is a dispatch-loop freeze")
	}
	close(release)
}

// verifies: AIRA-64 §4.7 — the gate is abandonable: a vanished peer and a
// stopping daemon both release a blocking waiter.
func TestCPUGateIsAbandonable(t *testing.T) {
	t.Run("peer cancelled", func(t *testing.T) {
		server := cpuGateServer(t)
		server.cpuSlotsGate <- struct{}{}
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		if server.acquireCPUSlotsGate(ctx, false) {
			t.Fatal("a cancelled peer must not acquire the gate")
		}
	})
	t.Run("daemon stopping", func(t *testing.T) {
		server := cpuGateServer(t)
		server.cpuSlotsGate <- struct{}{}
		server.stopping = make(chan struct{})
		close(server.stopping)
		if server.acquireCPUSlotsGate(context.Background(), false) {
			t.Fatal("a stopping daemon must not acquire the gate")
		}
	})
}
