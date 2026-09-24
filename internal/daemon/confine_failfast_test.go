package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"strconv"
	"sync"
	"testing"
	"time"

	"aira/internal/codes"
	"aira/internal/core"
	"aira/internal/runner"
)

// AIRA-247 fail-fast. These tests pin the four load-bearing properties and each
// names the mutation it reds against:
//   - a tripped slice REJECTS every queued waiter (would otherwise be granted);
//   - the latch is DURABLE across a queue prune (a queue-level flag would evaporate);
//   - a real-cgroup daemon NEVER acts on a trip (the cross-session safety gate);
//   - the sweep signals the SIBLINGS but SKIPS the trigger (which keeps its verdict).

// failfastDirectServer is a dev-mode server with effectively unbounded RAM and
// zero headroom, so a queued waiter is GRANTED on an ordinary pass unless the
// fail-fast trip refuses it first. That is what makes the trip-reject test a real
// mutation test: remove the trip loop and the same waiter is granted.
func failfastDirectServer(now time.Time) *Server {
	server := NewServer(Paths{})
	server.admitNow = func() time.Time { return now }
	server.admitSliceHeadroomBase = 0
	server.admitSliceHeadroomSupervisor = 0
	server.admitReadMemory = func(string) (int64, int64, int64, bool, string) {
		return 0, 1 << 50, 0, true, ""
	}
	return server
}

// testConfineScopeID builds a canonical scope id embedding a chosen supervisor
// pid, and asserts it round-trips through the real parser so the fixture cannot
// silently drift from the grammar the sweep depends on.
func testConfineScopeID(t *testing.T, name string, pid int) string {
	t.Helper()
	id := "CONFINE-" + name + "-" + strconv.Itoa(pid) + "-" + strconv.FormatInt(987654, 36)
	gotName, gotPID, _, _, ok := runner.ParseConfineScopeID(id)
	if !ok || gotName != name || gotPID != pid {
		t.Fatalf("test scope id %q did not round-trip (name=%q pid=%d ok=%v); fixture drifted from the scope-id grammar", id, gotName, gotPID, ok)
	}
	return id
}

// verifies: AIRA-247 — a tripped slice rejects every QUEUED waiter with
// admitOutcomeFailfast, closing its grantedCh, and grants nothing. The RAM ledger
// has ample room, so removing the trip-reject loop at the top of
// evaluateAdmitQueue would GRANT these waiters instead — this reds against that.
func TestFailfastTripRejectsQueuedWaiters(t *testing.T) {
	now := time.Unix(900_000, 0)
	server := failfastDirectServer(now)
	queued := []*admitWaiter{
		{seq: 1, reserve: 1 << 20, cpu: 1, state: admitQueued, grantedCh: make(chan struct{}), enqueued: now},
		{seq: 2, reserve: 1 << 20, cpu: 1, state: admitQueued, grantedCh: make(chan struct{}), enqueued: now},
	}
	queue := &sliceQueue{
		path: "/slice", server: server, kick: make(chan struct{}, 1),
		waiters: queued, failfastTripped: true,
	}
	server.evaluateAdmitQueue(queue)
	for i, waiter := range queued {
		if waiter.state != admitRejected {
			t.Fatalf("waiter %d state=%v, want admitRejected — a tripped slice must admit nothing (mutation: trip-reject loop removed → granted)", i, waiter.state)
		}
		if waiter.outcome != admitOutcomeFailfast {
			t.Fatalf("waiter %d outcome=%q, want %q — the render path keys the E_ADMIT_FAILFAST_TRIPPED code on this", i, waiter.outcome, admitOutcomeFailfast)
		}
		select {
		case <-waiter.grantedCh:
		default:
			t.Fatalf("waiter %d grantedCh not closed — its admitConnection would block forever", i)
		}
	}
}

// verifies: AIRA-247 — the fail-fast latch is DURABLE across a queue prune. The
// Server-level map outlives any single sliceQueue, so a queue rebuilt after the
// trip emptied and pruned the old one is BORN tripped. Mutation: move the latch
// onto the queue only (drop the queue-create copy) → the fresh queue is untripped
// and the late leg is admitted.
func TestFailfastLatchSurvivesQueuePrune(t *testing.T) {
	now := time.Unix(900_000, 0)
	server := failfastDirectServer(now)
	// The durable latch is set (as tripFailfast sets it); the old queue has been
	// pruned, so admitQueues holds nothing for this path.
	server.failfastTripped = map[string]bool{"/slice": true}

	queue, _, code, err := server.enqueueAdmitInternal("/slice", 1<<20, "", 0, false, admitRequest{})
	if err != nil || code != "" {
		t.Fatalf("enqueue of a fresh waiter failed: code=%q err=%v", code, err)
	}
	defer stopEvaluator(t, queue)

	queue.mu.Lock()
	tripped := queue.failfastTripped
	queue.mu.Unlock()
	if !tripped {
		t.Fatal("a queue rebuilt after a fail-fast trip must be born tripped (durable Server latch); mutation: queue-create copy removed → a late leg is admitted after the trip pruned the queue")
	}
}

// verifies: AIRA-247 — the CROSS-SESSION SAFETY GATE. A real-cgroup daemon owns
// the shared box slice, so it NEVER acts on a trip: confineFailfast returns
// Applied=false with mode=real, signals no supervisor, and does not latch the
// queue. Mutation: remove the shimMode gate in confineFailfast → it trips and
// signals on the shared slice.
func TestFailfastRealModeIsNoOp(t *testing.T) {
	now := time.Unix(900_000, 0)
	server := failfastDirectServer(now) // confineMode "" => real
	server.admitResolveSlice = func(string) (string, bool, string) { return "/slice", true, "" }

	var mu sync.Mutex
	var signalled []int
	server.failfastSignalPID = func(pid int) error {
		mu.Lock()
		defer mu.Unlock()
		signalled = append(signalled, pid)
		return nil
	}

	// A live granted job on the slice, so a mutated (gate-removed) trip would have
	// a victim to signal.
	queue := &sliceQueue{
		path: "/slice", server: server, kick: make(chan struct{}, 1),
		waiters: []*admitWaiter{{
			seq: 1, reserve: 1 << 20, state: admitGranted, accounted: true,
			grantedCh: make(chan struct{}), scopeID: testConfineScopeID(t, "victim", 4321),
		}},
		outstanding: 1 << 20, outstandingJobs: 1,
	}
	server.admitRegistryMu.Lock()
	server.admitQueues = map[string]*sliceQueue{"/slice": queue}
	server.admitRegistryMu.Unlock()

	resp := server.confineFailfast(map[string]any{"slice": "test", "scope_id": ""})
	result, ok := resp.Data.(runner.FailfastTripResult)
	if !ok {
		t.Fatalf("confineFailfast Data=%T, want runner.FailfastTripResult", resp.Data)
	}
	if result.Applied {
		t.Fatal("a real-cgroup daemon must NOT apply a fail-fast trip (shared box slice); mutation: shimMode gate removed")
	}
	if result.Mode != "real" {
		t.Fatalf("mode=%q, want real", result.Mode)
	}
	mu.Lock()
	got := append([]int(nil), signalled...)
	mu.Unlock()
	if len(got) != 0 {
		t.Fatalf("a real-mode trip signalled %v — it must signal nothing on the shared slice", got)
	}
	queue.mu.Lock()
	tripped := queue.failfastTripped
	queue.mu.Unlock()
	if tripped {
		t.Fatal("a real-mode trip must not latch the queue")
	}
}

// verifies: AIRA-247 — the sweep SIGUSR1s the sibling supervisors but SKIPS the
// trigger (the failed --fail-fast job, which keeps its real verdict), derives
// each supervisor pid from the granted waiter's scope id, and latches the queue.
// Mutation: drop the `waiter.scopeID == triggerScopeID` skip → the trigger's pid
// is signalled and it gets restamped failfast-cancelled, destroying the one
// distinction the CI classifier needs.
func TestFailfastSweepSkipsTriggerSignalsSiblings(t *testing.T) {
	now := time.Unix(900_000, 0)
	server := failfastDirectServer(now)
	server.confineMode = runner.ConfineModeShim
	server.shimBudget = shimBudget{Bytes: 1 << 40, Source: "test"}

	triggerID := testConfineScopeID(t, "trigger", 1111)
	siblingA := testConfineScopeID(t, "siblinga", 2222)
	siblingB := testConfineScopeID(t, "siblingb", 3333)

	queue := &sliceQueue{
		path: "/slice", server: server, kick: make(chan struct{}, 1),
		waiters: []*admitWaiter{
			{seq: 1, reserve: 1 << 20, state: admitGranted, accounted: true, grantedCh: make(chan struct{}), scopeID: triggerID},
			{seq: 2, reserve: 1 << 20, state: admitGranted, accounted: true, grantedCh: make(chan struct{}), scopeID: siblingA},
			{seq: 3, reserve: 1 << 20, state: admitGranted, accounted: true, grantedCh: make(chan struct{}), scopeID: siblingB},
		},
		outstanding: 3 << 20, outstandingJobs: 3,
	}
	server.admitRegistryMu.Lock()
	server.admitQueues = map[string]*sliceQueue{"/slice": queue}
	server.admitRegistryMu.Unlock()

	var mu sync.Mutex
	var signalled []int
	server.failfastSignalPID = func(pid int) error {
		mu.Lock()
		defer mu.Unlock()
		signalled = append(signalled, pid)
		return nil
	}

	count := server.tripFailfast("/slice", triggerID)

	mu.Lock()
	got := map[int]bool{}
	for _, pid := range signalled {
		got[pid] = true
	}
	mu.Unlock()

	if count != 2 {
		t.Fatalf("tripFailfast signalled %d, want 2 (the two siblings)", count)
	}
	if got[1111] {
		t.Fatal("the trigger's supervisor (pid 1111) was signalled — it must be skipped so it keeps its real verdict (mutation: trigger-skip removed)")
	}
	if !got[2222] || !got[3333] {
		t.Fatalf("both siblings must be signalled; got %v", got)
	}
	queue.mu.Lock()
	tripped := queue.failfastTripped
	queue.mu.Unlock()
	if !tripped {
		t.Fatal("tripFailfast must latch the queue so the evaluator refuses new admissions")
	}
	if !server.failfastTripped["/slice"] {
		t.Fatal("tripFailfast must set the durable Server latch")
	}
}

// verifies: AIRA-247 — the wire render answers E_ADMIT_FAILFAST_TRIPPED with basis
// reject:failfast (NOT E_ADMIT_SATURATED / reject:saturated), so the runner routes
// it to its dedicated terminal-refusal block and the CI classifier can tell a
// fail-fast refusal from a capacity one. Mutation: remove the failfast arm in the
// admitRejected render block → it falls to the saturated arm (CodeAdmitSaturated).
func TestFailfastTripRendersDedicatedCode(t *testing.T) {
	const maximum = int64(1) << 30
	server := saturatedServer(t)
	// Readable slice with ample room, so the request reaches the queue (it neither
	// fails at entry nor is refused as too-large) and would be GRANTED on a pass —
	// the trip is the only thing that refuses it.
	server.admitReadMemory = func(string) (int64, int64, int64, bool, string) { return 0, maximum, 0, true, "" }
	// A pinned small reserve, so reserve resolution needs no peak history.
	args := map[string]any{"slice": "slice", "reserve": int64(1) << 20, "pinned": true}
	run := startSaturatedAdmit(t, server, maximum, args)

	// Trip the slice, then run one pass as the sole evaluator: the trip-reject loop
	// closes the waiter's grantedCh, which wakes its admitConnection to render.
	run.queue.mu.Lock()
	run.queue.failfastTripped = true
	run.queue.mu.Unlock()
	run.pass()

	select {
	case frame := <-run.frames:
		if frame.Code != CodeAdmitFailfastTripped {
			t.Fatalf("frame code=%q, want %s (mutation: failfast render arm removed → falls to saturated)", frame.Code, CodeAdmitFailfastTripped)
		}
		var rejection admitRejection
		if err := json.Unmarshal(frame.Data, &rejection); err != nil {
			t.Fatalf("rejection payload: %v", err)
		}
		if rejection.Basis != "reject:failfast" {
			t.Fatalf("basis=%q, want reject:failfast — a fail-fast trip must not borrow the saturated basis", rejection.Basis)
		}
	case err := <-run.errs:
		t.Fatalf("read: %v", err)
	case <-time.After(5 * time.Second):
		t.Fatal("no rejection frame after the trip pass")
	}
}

// verifies: AIRA-247 — a trip leaves GRANTED leases (running jobs) untouched: the
// evaluator only rejects QUEUED waiters, so the ledger is unchanged (rejecting a
// queued waiter is ledger-neutral) and a granted lease keeps its state. Running
// jobs are torn down by the SIGUSR1 sweep, NOT by the evaluator, and their leases
// release on their own connection close. Mutation: drop the `state != admitQueued`
// skip in the trip-reject loop → the granted lease is rejected too, disturbing the
// ledger and the running job's state.
func TestFailfastTripLeavesGrantedLeaseIntact(t *testing.T) {
	now := time.Unix(900_000, 0)
	server := failfastDirectServer(now)
	granted := &admitWaiter{
		seq: 1, reserve: 1 << 20, state: admitGranted, accounted: true,
		grantedCh: make(chan struct{}), scopeID: testConfineScopeID(t, "running", 5555),
	}
	close(granted.grantedCh) // a granted lease's grantedCh was closed at grant
	queued := &admitWaiter{seq: 2, reserve: 1 << 20, state: admitQueued, grantedCh: make(chan struct{}), enqueued: now}
	queue := &sliceQueue{
		path: "/slice", server: server, kick: make(chan struct{}, 1),
		waiters: []*admitWaiter{granted, queued}, outstanding: 1 << 20, outstandingJobs: 1,
		failfastTripped: true,
	}

	server.evaluateAdmitQueue(queue)

	if granted.state != admitGranted {
		t.Fatalf("granted lease state=%v, want admitGranted — a trip must NOT reject a running job in the evaluator (that is the sweep's job)", granted.state)
	}
	if queue.outstanding != 1<<20 || queue.outstandingJobs != 1 {
		t.Fatalf("trip disturbed the ledger: outstanding=%d jobs=%d, want 1MiB/1 (rejecting queued waiters is ledger-neutral)", queue.outstanding, queue.outstandingJobs)
	}
	if queued.state != admitRejected || queued.outcome != admitOutcomeFailfast {
		t.Fatalf("queued waiter state=%v outcome=%q, want admitRejected/failfast", queued.state, queued.outcome)
	}
}

// verifies: AIRA-247 — the sweep is BEST-EFFORT and defensive: a signal that
// errors is skipped and NOT counted (never claims a confirmed kill), and a granted
// waiter whose scope id does not parse to a supervisor pid is skipped rather than
// crashing the sweep. Mutation: count the errored signal → count wrong; or fail to
// guard the unparseable scope id → panic/wrong pid.
func TestFailfastSweepIsBestEffortAndSkipsUnparseable(t *testing.T) {
	now := time.Unix(900_000, 0)
	server := failfastDirectServer(now)
	server.confineMode = runner.ConfineModeShim

	good := testConfineScopeID(t, "good", 2222)
	failing := testConfineScopeID(t, "failing", 3333)
	queue := &sliceQueue{
		path: "/slice", server: server, kick: make(chan struct{}, 1),
		waiters: []*admitWaiter{
			{seq: 1, reserve: 1 << 20, state: admitGranted, accounted: true, grantedCh: make(chan struct{}), scopeID: good},
			{seq: 2, reserve: 1 << 20, state: admitGranted, accounted: true, grantedCh: make(chan struct{}), scopeID: failing},
			// A granted lease whose scope id cannot yield a supervisor pid: skipped,
			// never signalled, never a crash.
			{seq: 3, reserve: 1 << 20, state: admitGranted, accounted: true, grantedCh: make(chan struct{}), scopeID: "not-a-valid-scope-id"},
		},
		outstanding: 3 << 20, outstandingJobs: 3,
	}
	server.admitRegistryMu.Lock()
	server.admitQueues = map[string]*sliceQueue{"/slice": queue}
	server.admitRegistryMu.Unlock()

	var mu sync.Mutex
	var signalled []int
	server.failfastSignalPID = func(pid int) error {
		if pid == 3333 {
			return errors.New("supervisor already gone")
		}
		mu.Lock()
		defer mu.Unlock()
		signalled = append(signalled, pid)
		return nil
	}

	count := server.tripFailfast("/slice", "")

	if count != 1 {
		t.Fatalf("tripFailfast counted %d, want 1 — only the good victim; an errored signal is best-effort (not counted) and an unparseable scope id is skipped", count)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(signalled) != 1 || signalled[0] != 2222 {
		t.Fatalf("signalled=%v, want [2222] — the good victim signalled, the failing one attempted-then-skipped, the unparseable one never reached the signaller", signalled)
	}
}

// verifies: AIRA-247 — the confine-failfast VERB is routed through the real
// serveConnection dispatch to confineFailfast, NOT to confineManagement (whose
// owner gate would reject a trip carrying no owner) and NOT to an unknown-verb
// refusal. Sent with no owner, in real mode, so a correct route answers OK with a
// FailfastTripResult{mode:real}; a mis-wired verb reds with E_UNKNOWN_VERB and a
// route into confineManagement reds with E_CONFINE_ARGUMENT_INVALID.
func TestFailfastVerbRoutesToConfineFailfast(t *testing.T) {
	server := failfastDirectServer(time.Unix(900_000, 0)) // real mode
	server.stopping = make(chan struct{})

	serverConn, clientConn := net.Pipe()
	done := make(chan struct{})
	go func() { defer close(done); server.serveConnection(context.Background(), serverConn) }()
	go func() {
		_ = writeFrame(clientConn, RequestFrame{
			Proto:   ProtocolVersion,
			Request: core.Request{Verb: "confine-failfast", Args: map[string]any{"scope_id": "trigger"}},
		})
	}()

	var resp ResponseFrame
	if err := readFrame(clientConn, &resp); err != nil {
		t.Fatalf("no response to confine-failfast: %v", err)
	}
	if resp.Code != "OK" {
		t.Fatalf("confine-failfast routed to code=%q, want OK — E_UNKNOWN_VERB means the route is missing; E_CONFINE_ARGUMENT_INVALID means it went through confineManagement's owner gate", resp.Code)
	}
	var result runner.FailfastTripResult
	if err := json.Unmarshal(resp.Data, &result); err != nil {
		t.Fatalf("response data is not a FailfastTripResult: %v", err)
	}
	if result.Mode != "real" || result.Applied {
		t.Fatalf("route did not reach confineFailfast in real mode: applied=%v mode=%q", result.Applied, result.Mode)
	}
	_ = clientConn.Close()
	<-done
}

// verifies: AIRA-247 — the code is catalogued at exit bucket 1 (a durable
// state-conflict refusal), consistent with the exit confineErrorCode+ExitForCode
// produce from the E_ADMIT_FAILFAST_TRIPPED-prefixed message.
func TestFailfastCodeExitsOne(t *testing.T) {
	if got := codes.ExitForCode(CodeAdmitFailfastTripped); got != 1 {
		t.Fatalf("ExitForCode(%s)=%d, want 1", CodeAdmitFailfastTripped, got)
	}
}
