package daemon

import (
	"context"
	"os"
	"os/exec"
	"sync/atomic"
	"testing"
	"time"

	"aira/internal/core"
	"aira/internal/runner"
)

// AIRA-119. `confine --list` was reported as naming an exclusive job that had
// already released, while the real drain belonged to an entirely different
// request. These tests pin the property that claim was about: the identity the
// daemon reports is DERIVED from the same waiter list the gate enforces, on
// every read, so it tracks a release-then-readmission exactly and can never lag
// behind a handover.
//
// They are written against the confine-list WIRE reply rather than the internal
// snapshot, because the report was about what an operator saw: a caching or
// carry-over bug anywhere between the waiter list and the wire is in scope here.

// exclusiveWireServer is a server whose slice path is a real (empty) directory,
// so runner.ListConfines — which confineManagement calls before the snapshot —
// has something to scan. Nothing touches the machine's real aira.slice.
func exclusiveWireServer(t *testing.T) (*Server, string) {
	t.Helper()
	slicePath := t.TempDir()
	var maximum atomic.Int64
	maximum.Store(1 << 40)
	server := admitTestServer(&maximum)
	server.admitResolveSlice = func(string) (string, bool, string) { return slicePath, true, "" }
	return server, slicePath
}

// exclusiveOnTheWire returns the exclusive state exactly as `confine --list`
// would receive it, or nil for a slice reporting no exclusivity.
func exclusiveOnTheWire(t *testing.T, server *Server) *runner.ConfineExclusiveState {
	t.Helper()
	response := server.confineManagement(context.Background(), core.Request{
		Verb: "confine-list",
		Args: map[string]any{"owner": "operator", "slice": "test"},
	})
	if !response.OK {
		t.Fatalf("confine-list failed: code=%s err=%s", response.Code, response.Error)
	}
	result, ok := response.Data.(runner.ConfineListResult)
	if !ok {
		t.Fatalf("confine-list returned %T, not a ConfineListResult", response.Data)
	}
	if result.SliceReserve == nil {
		t.Fatal("confine-list reported no slice reserve at all; the exclusive line could not be rendered")
	}
	return result.SliceReserve.Exclusive
}

// The centrepiece: a released exclusive job must never be named again — not
// while the slice is idle, not while an unrelated job runs, and above all not
// once a DIFFERENT exclusive request has taken over the gate. That last case is
// the field report's exact shape: the wire named one job while the drain
// belonged to another.
//
// The slice is deliberately kept OCCUPIED across the whole handover — there is
// always at least one non-exclusive waiter on the queue — and that is what makes
// the test non-porous rather than tidy. A queue with no waiters is pruned and its
// object discarded, so any per-queue carry-over of the identity would be thrown
// away with it and a cached-identity defect would go unnoticed. Keeping the SAME
// sliceQueue alive from A's hold through B's is the only shape in which such a
// defect is observable; verified by mutation (caching the first identity on the
// queue fails this test and passes without the keeper).
//
// verifies: AIRA-119
func TestExclusiveIdentityOnTheWireFollowsTheGateAcrossReleaseAndReadmission(t *testing.T) {
	server, slicePath := exclusiveWireServer(t)
	aScope := exclusiveScopeID(t, "joba", 4101)
	bScope := exclusiveScopeID(t, "jobb", 4102)

	// Whatever else changes, A's three identifying fields must never reappear on
	// the wire once A has released.
	departed := func(what string, state *runner.ConfineExclusiveState) {
		t.Helper()
		if state == nil {
			return
		}
		if state.Name == "joba" || state.Owner == "alice" || state.ScopeID == aScope {
			t.Fatalf("%s: the wire named the RELEASED exclusive job A: %+v", what, *state)
		}
	}
	enqueue := func(what string, request admitRequest) (*sliceQueue, *admitWaiter) {
		t.Helper()
		queue, waiter, code, err := server.enqueueAdmitInternal(slicePath, 10, "", 0, false, request)
		if err != nil {
			t.Fatalf("enqueue %s: code=%s err=%v", what, code, err)
		}
		return queue, waiter
	}

	// An ordinary job runs and keeps the slice non-empty, so A DRAINS: the state
	// the field report was taken in. It also keeps the queue object alive.
	queue, keeper := enqueue("the first keeper", admitRequest{})
	evaluate(t, server, queue)
	requireGranted(t, queue, keeper, "the first keeper")

	_, a := enqueue("A", admitRequest{exclusive: true, scopeID: aScope, name: "joba", owner: "alice"})
	evaluate(t, server, queue)
	requireStillQueued(t, queue, a, "exclusive requester A behind the keeper")
	drainingA := exclusiveOnTheWire(t, server)
	if drainingA == nil || drainingA.State != admitExclusiveDraining ||
		drainingA.Name != "joba" || drainingA.Owner != "alice" || drainingA.ScopeID != aScope {
		t.Fatalf("a live drain must be named on the wire, got %+v", drainingA)
	}

	// The keeper finishes, so A is let in and now HOLDS the slice.
	server.releaseAdmitWaiter(queue, keeper)
	evaluate(t, server, queue)
	requireGranted(t, queue, a, "exclusive requester A once the slice emptied")
	heldA := exclusiveOnTheWire(t, server)
	if heldA == nil || heldA.State != admitExclusiveHeld || heldA.ScopeID != aScope {
		t.Fatalf("the wire must name A as the holder, got %+v", heldA)
	}

	// A second ordinary job arrives; A's hold keeps it queued, which keeps the
	// queue alive across A's departure below.
	_, next := enqueue("the second keeper", admitRequest{})
	evaluate(t, server, queue)
	requireStillQueued(t, queue, next, "an ordinary waiter behind A's hold")

	// A releases. Nothing on this queue has been discarded.
	server.releaseAdmitWaiter(queue, a)
	afterRelease := exclusiveOnTheWire(t, server)
	departed("immediately after A released", afterRelease)
	if afterRelease != nil {
		t.Fatalf("exclusivity survived its holder's release: %+v", *afterRelease)
	}
	evaluate(t, server, queue)
	requireGranted(t, queue, next, "the queued ordinary waiter once A's hold ended")
	departed("while an unrelated job runs", exclusiveOnTheWire(t, server))

	// A DIFFERENT session now asks for the slice, on that same live queue.
	_, b := enqueue("B", admitRequest{exclusive: true, scopeID: bScope, name: "jobb", owner: "bob"})
	evaluate(t, server, queue)
	requireStillQueued(t, queue, b, "exclusive requester B behind a running neighbour")
	drainingB := exclusiveOnTheWire(t, server)
	departed("while B drains", drainingB)
	if drainingB == nil || drainingB.State != admitExclusiveDraining ||
		drainingB.Name != "jobb" || drainingB.Owner != "bob" || drainingB.ScopeID != bScope {
		t.Fatalf("the wire must name the CURRENT drain head B, got %+v", drainingB)
	}

	// And through B's own grant, where the state changes but the identity does not.
	server.releaseAdmitWaiter(queue, next)
	evaluate(t, server, queue)
	requireGranted(t, queue, b, "exclusive requester B once the slice drained")
	heldB := exclusiveOnTheWire(t, server)
	departed("once B holds the slice", heldB)
	if heldB == nil || heldB.State != admitExclusiveHeld || heldB.ScopeID != bScope {
		t.Fatalf("the wire must name B as the holder, got %+v", heldB)
	}
}

// The age reported beside the identity must be the age of THAT STATE, taken from
// the right anchor. A benchmark that queued for twenty minutes and has now been
// running alone for five seconds has held the slice for five seconds; reporting
// its enqueue age would name it as the wedge an operator is hunting. This is the
// AIRA-49 v3 conflation (enqueue read as grant) in the one place it would say the
// opposite of the truth.
//
// verifies: AIRA-119
func TestExclusiveAgeAnchorsOnEnqueueWhileDrainingAndOnTheGrantOnceHeld(t *testing.T) {
	server, slicePath := exclusiveWireServer(t)
	clock := time.Date(2026, 9, 6, 4, 0, 0, 0, time.UTC)
	server.admitNow = func() time.Time { return clock }

	scope := exclusiveScopeID(t, "bench", 4103)
	// A neighbour holds the slice, so the exclusive request drains.
	queue, busy, code, err := server.enqueueAdmitInternal(slicePath, 10, "", 0, false, admitRequest{})
	if err != nil {
		t.Fatalf("enqueue neighbour: code=%s err=%v", code, err)
	}
	evaluate(t, server, queue)
	requireGranted(t, queue, busy, "an unrelated neighbour")

	_, exclusive, code, err := server.enqueueAdmitInternal(slicePath, 10, "", 0, false, admitRequest{
		exclusive: true, scopeID: scope, name: "bench", owner: "mark",
	})
	if err != nil {
		t.Fatalf("enqueue exclusive: code=%s err=%v", code, err)
	}

	clock = clock.Add(20 * time.Minute)
	evaluate(t, server, queue)
	requireStillQueued(t, queue, exclusive, "the exclusive drain head")

	draining := exclusiveOnTheWire(t, server)
	if draining == nil || draining.State != admitExclusiveDraining {
		t.Fatalf("expected a drain, got %+v", draining)
	}
	if draining.SinceMS != (20 * time.Minute).Milliseconds() {
		t.Fatalf("a drain's age must run from its ENQUEUE — how long it has failed to converge; got %d ms", draining.SinceMS)
	}

	// The slice empties and the benchmark is let in. Its HOLD is seconds old even
	// though its request is twenty minutes old.
	server.releaseAdmitWaiter(queue, busy)
	evaluate(t, server, queue)
	requireGranted(t, queue, exclusive, "the exclusive waiter once the slice emptied")
	clock = clock.Add(5 * time.Second)

	held := exclusiveOnTheWire(t, server)
	if held == nil || held.State != admitExclusiveHeld {
		t.Fatalf("expected a hold, got %+v", held)
	}
	if held.SinceMS != (5 * time.Second).Milliseconds() {
		t.Fatalf("a hold's age must run from its GRANT, not its enqueue: got %d ms", held.SinceMS)
	}
}

// AIRA-101's un-wedge proof covered the HELD half only: a SIGKILLed exclusive
// HOLDER. The half the field report was actually taken in — a DRAIN HEAD, still
// queued, whose requester dies — had no coverage at all, and it is the harder
// case to reason about because a queued waiter owns no cgroup scope, so NONE of
// the daemon's scope-based backstops (the stale-lease sweep, the orphan reaper)
// can ever see it. Its only liveness signal is the admission socket.
//
// verifies: AIRA-119
// verifies: AIRA-101
func TestSIGKILLingTheExclusiveDrainHeadStopsNamingIt(t *testing.T) {
	if testing.Short() {
		t.Skip("spawns a helper process")
	}
	server, socket, slicePath := startExclusiveTestDaemon(t)

	// One granted ordinary job is enough to keep the slice non-empty, so the
	// helper's exclusive request DRAINS instead of being granted.
	_, ordinary, code, err := server.enqueueAdmitInternal(slicePath, 1024, "", 0, false, admitRequest{})
	if err != nil {
		t.Fatalf("enqueue ordinary: code=%s err=%v", code, err)
	}
	waitAdmitGrant(t, ordinary)

	scopeID := exclusiveScopeID(t, "bench", os.Getpid()%100000+7)
	helper := exec.Command(os.Args[0], "-test.run", "^TestHelperExclusiveClaimant$", "-test.v")
	helper.Env = append(os.Environ(),
		exclusiveHelperEnv+"="+socket,
		"AIRA_TEST_EXCLUSIVE_SCOPE_ID="+scopeID,
	)
	if err := helper.Start(); err != nil {
		t.Fatalf("start helper: %v", err)
	}
	defer func() {
		_ = helper.Process.Kill()
		_, _ = helper.Process.Wait()
	}()

	waitForExclusiveState(t, server, slicePath, admitExclusiveDraining)
	if snapshot := server.admitSliceSnapshot(slicePath); snapshot.exclusiveScopeID != scopeID {
		t.Fatalf("the drain head must be named by its own scope id, got %q", snapshot.exclusiveScopeID)
	}

	// The uncleanest death available: nothing is told to the daemon, and a queued
	// waiter has no scope for any sweep to reap.
	if err := helper.Process.Kill(); err != nil {
		t.Fatalf("kill helper: %v", err)
	}
	_, _ = helper.Process.Wait()

	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		if server.admitSliceSnapshot(slicePath).exclusiveState == "" {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	final := server.admitSliceSnapshot(slicePath)
	t.Fatalf("a SIGKILLed DRAIN HEAD is still named as the current drain: state=%q name=%q owner=%q scope=%q",
		final.exclusiveState, final.exclusiveName, final.exclusiveOwner, final.exclusiveScopeID)
}
