package daemon

import (
	"net"
	"testing"
	"time"
)

// These tests directly exercise the evaluator. Revert-check: replacing the
// floor selection or close-based wakeup logic makes the floor/loss and epoch
// tests below fail, while removing enforce preemption makes young-first fail.
func testGovernor(capacity int, mode governorMode) *governorSet {
	return &governorSet{workers: map[string]*governorWorker{}, jobAges: map[string]time.Time{}, capacity: capacity, mode: mode, kick: make(chan struct{}, 1)}
}

func putGovernorWorker(g *governorSet, id, job string, age time.Time, seq uint64, state governorWorkerState) *governorWorker {
	w := &governorWorker{workerUUID: id, jobID: job, jobAge: age, seq: seq, state: state, grant: make(chan struct{})}
	g.workers[id] = w
	g.jobAges[job] = age
	return w
}

func activeGovernorWorkers(g *governorSet) int {
	n := 0
	for _, w := range g.workers {
		if w.state == governorActive {
			n++
		}
	}
	return n
}

func testRAMGovernor(t *testing.T, capacity int, current, maximum int64, readable bool) *governorSet {
	t.Helper()
	return testRAMGovernorWithMemory(t, capacity, func(string) (int64, int64, int64, bool, string) {
		return current, maximum, 0, readable, "injected"
	})
}

func testRAMGovernorWithMemory(t *testing.T, capacity int, readMemory func(string) (int64, int64, int64, bool, string)) *governorSet {
	t.Helper()
	t.Setenv("AIRA_GOVERNOR_RAM_LOW_MARK", "20")
	t.Setenv("AIRA_GOVERNOR_RAM_HIGH_MARK", "40")
	s := &Server{
		admitQueues:                  map[string]*sliceQueue{},
		admitSliceHeadroomBase:       0,
		admitSliceHeadroomSupervisor: 0,
		admitReadMemory:              readMemory,
	}
	g := testGovernor(capacity, governorEnforce)
	g.server = s
	g.ramAware = map[string]bool{}
	return g
}

func putRAMGovernorWorker(g *governorSet, id, job string, age time.Time, seq uint64, state governorWorkerState, estimate int64) *governorWorker {
	w := putGovernorWorker(g, id, job, age, seq, state)
	w.slice, w.nextEst = "/slice", estimate
	return w
}

func TestWriteGovernorFrameRefreshesExpiredWriteDeadline(t *testing.T) {
	server, client := net.Pipe()
	defer server.Close()
	defer client.Close()

	if err := server.SetWriteDeadline(time.Now().Add(-time.Second)); err != nil {
		t.Fatalf("set expired write deadline: %v", err)
	}
	if err := writeFrame(server, governorReplyFrame{State: "raw"}); err == nil {
		t.Fatal("raw writeFrame succeeded with an expired write deadline")
	} else if timeoutErr, ok := err.(interface{ Timeout() bool }); !ok || !timeoutErr.Timeout() {
		t.Fatalf("raw writeFrame error is not a timeout: %T %v", err, err)
	}

	received := make(chan governorReplyFrame, 1)
	errs := make(chan error, 1)
	go func() {
		var frame governorReplyFrame
		if err := readFrame(client, &frame); err != nil {
			errs <- err
			return
		}
		received <- frame
	}()

	if err := writeGovernorFrame(server, governorReplyFrame{State: "continue"}); err != nil {
		t.Fatalf("writeGovernorFrame: %v", err)
	}
	select {
	case err := <-errs:
		t.Fatalf("read reply: %v", err)
	case frame := <-received:
		if frame.State != "continue" {
			t.Fatalf("reply state = %q, want continue", frame.State)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for governor reply")
	}
}

func TestGovernorConnectionRefreshesExpiredWriteDeadline(t *testing.T) {
	serverConn, clientConn := net.Pipe()
	s := &Server{}
	s.admitResolveSlice = func(slice string) (string, bool, string) {
		if slice == "aira.slice" {
			return "/slice", true, ""
		}
		return "", false, "slice-not-found"
	}
	s.governor = newGovernorSet(1, governorEnforce, s)
	handlerDone := make(chan struct{})
	go func() {
		s.governorConnection(serverConn, nil)
		close(handlerDone)
	}()
	t.Cleanup(func() {
		_ = clientConn.Close()
		_ = serverConn.Close()
		select {
		case <-handlerDone:
		case <-time.After(time.Second):
			t.Error("governor connection did not exit")
		}
		s.governor.stopOnce.Do(func() { close(s.governor.stop) })
	})

	if err := clientConn.SetDeadline(time.Now().Add(time.Second)); err != nil {
		t.Fatalf("set client deadline: %v", err)
	}
	if err := writeFrame(clientConn, governorRequestFrame{Type: "acquire", WorkerUUID: "worker", JobID: "job", Slice: "aira.slice"}); err != nil {
		t.Fatalf("write acquire: %v", err)
	}
	var active governorReplyFrame
	if err := readFrame(clientConn, &active); err != nil {
		t.Fatalf("read acquire reply: %v", err)
	}
	if active.State != "active" {
		t.Fatalf("acquire reply state = %q, want active", active.State)
	}
	s.governor.mu.Lock()
	if worker := s.governor.workers["worker"]; worker == nil || worker.slice != "/slice" {
		s.governor.mu.Unlock()
		t.Fatalf("acquire did not resolve immutable slice: worker=%#v", worker)
	}
	s.governor.mu.Unlock()

	if err := serverConn.SetWriteDeadline(time.Now().Add(-time.Second)); err != nil {
		t.Fatalf("set expired server write deadline: %v", err)
	}
	if err := writeFrame(clientConn, governorRequestFrame{Type: "checkpoint", Slice: "other.slice"}); err != nil {
		t.Fatalf("write checkpoint: %v", err)
	}
	// Revert-check: raw writeFrame in governorConnection would inherit the
	// expired deadline above, so this handler-level continue reply would fail.
	var reply governorReplyFrame
	if err := readFrame(clientConn, &reply); err != nil {
		t.Fatalf("read checkpoint reply after expired server deadline: %v", err)
	}
	if reply.State != "continue" {
		t.Fatalf("checkpoint reply state = %q, want continue", reply.State)
	}
	s.governor.mu.Lock()
	if worker := s.governor.workers["worker"]; worker == nil || worker.slice != "/slice" {
		s.governor.mu.Unlock()
		t.Fatalf("checkpoint changed immutable slice: worker=%#v", worker)
	}
	s.governor.mu.Unlock()
}

func TestGovernorConnectionFailsOpenOnUnresolvableSlice(t *testing.T) {
	serverConn, clientConn := net.Pipe()
	s := &Server{}
	s.admitResolveSlice = func(slice string) (string, bool, string) {
		if slice == "aira.slice" {
			return "/slice", true, ""
		}
		return "", false, "slice-not-found"
	}
	s.governor = newGovernorSet(1, governorEnforce, s)
	handlerDone := make(chan struct{})
	go func() {
		s.governorConnection(serverConn, nil)
		close(handlerDone)
	}()
	t.Cleanup(func() {
		_ = clientConn.Close()
		_ = serverConn.Close()
		select {
		case <-handlerDone:
		case <-time.After(time.Second):
			t.Error("governor connection did not exit")
		}
		s.governor.stopOnce.Do(func() { close(s.governor.stop) })
	})

	if err := clientConn.SetDeadline(time.Now().Add(time.Second)); err != nil {
		t.Fatalf("set client deadline: %v", err)
	}
	if err := writeFrame(clientConn, governorRequestFrame{Type: "acquire", WorkerUUID: "worker", JobID: "job", Slice: "missing.slice"}); err != nil {
		t.Fatalf("write acquire: %v", err)
	}
	var reply governorReplyFrame
	if err := readFrame(clientConn, &reply); err != nil {
		t.Fatalf("read acquire reply: %v", err)
	}
	if reply.State != "continue" {
		t.Fatalf("acquire reply state = %q, want continue", reply.State)
	}
	s.governor.mu.Lock()
	defer s.governor.mu.Unlock()
	if len(s.governor.workers) != 0 {
		t.Fatalf("unresolvable acquire registered workers: %#v", s.governor.workers)
	}
}

func TestGovernorCapacityAndYoungestFirst(t *testing.T) {
	g := testGovernor(2, governorEnforce)
	old := time.Now().Add(-time.Hour)
	young := time.Now()
	oldFloor := putGovernorWorker(g, "old-floor", "old", old, 1, governorActive)
	oldExtra := putGovernorWorker(g, "old-extra", "old", old, 2, governorActive)
	newFloor := putGovernorWorker(g, "new-floor", "new", young, 3, governorParked)
	g.evaluate()
	if activeGovernorWorkers(g) != 2 || oldFloor.state != governorActive || oldExtra.state != governorActive || !oldExtra.parkRequested || newFloor.state != governorParked {
		t.Fatalf("preemption exceeded capacity before checkpoint: oldFloor=%v oldExtra=%v requested=%v new=%v active=%d", oldFloor.state, oldExtra.state, oldExtra.parkRequested, newFloor.state, activeGovernorWorkers(g))
	}
	parked, epoch, err := g.checkpoint(oldExtra, 0, 0)
	if err != nil || !parked || oldExtra.state != governorParked || oldExtra.parkRequested || epoch == nil {
		t.Fatalf("checkpoint did not complete requested park: parked=%v state=%v requested=%v epoch=%v err=%v", parked, oldExtra.state, oldExtra.parkRequested, epoch, err)
	}
	g.evaluate()
	if activeGovernorWorkers(g) != 2 || oldFloor.state != governorActive || newFloor.state != governorActive || oldExtra.state != governorParked {
		t.Fatalf("youngest-first handoff wrong: oldFloor=%v oldExtra=%v new=%v", oldFloor.state, oldExtra.state, newFloor.state)
	}
}

func TestGovernorFloorOversubscribesAndRepairsLoss(t *testing.T) {
	g := testGovernor(1, governorEnforce)
	age := time.Now()
	a := putGovernorWorker(g, "a", "a", age, 1, governorActive)
	b := putGovernorWorker(g, "b", "b", age, 2, governorParked)
	g.evaluate()
	if activeGovernorWorkers(g) != 2 || b.state != governorActive {
		t.Fatalf("hard floor did not oversubscribe: %d", activeGovernorWorkers(g))
	}
	// Loss leaves job a present only through a parked sibling. The next pass
	// must actively repair that floor; merely refusing last-worker parks fails.
	a2 := putGovernorWorker(g, "a2", "a", age, 3, governorParked)
	delete(g.workers, a.workerUUID)
	b.state = governorActive
	g.evaluate()
	if a2.state != governorActive {
		t.Fatal("floor-on-loss did not reactivate parked sibling")
	}
}

func TestGovernorParkReactivatesByClosingEpoch(t *testing.T) {
	g := testGovernor(2, governorEnforce)
	old := time.Now().Add(-time.Hour)
	young := time.Now()
	oldFloor := putGovernorWorker(g, "old-floor", "old", old, 1, governorActive)
	oldExtra := putGovernorWorker(g, "old-extra", "old", old, 2, governorActive)
	newFloor := putGovernorWorker(g, "new", "new", young, 3, governorActive)
	g.evaluate()
	if oldExtra.state != governorActive || !oldExtra.parkRequested {
		t.Fatal("expected above-floor old worker to be asked to park")
	}
	parked, epoch, err := g.checkpoint(oldExtra, 0, 0)
	if err != nil || !parked || oldExtra.state != governorParked || epoch == nil {
		t.Fatalf("checkpoint did not park requested worker: parked=%v state=%v epoch=%v err=%v", parked, oldExtra.state, epoch, err)
	}
	delete(g.workers, newFloor.workerUUID)
	g.evaluate()
	if oldExtra.state != governorActive {
		t.Fatal("freed capacity did not reactivate parked worker")
	}
	select {
	case <-epoch:
	default:
		t.Fatal("reactivation did not close the epoch channel")
	}
	_ = oldFloor
}

func TestGovernorCancelsPendingParkWhenWorkerBecomesDesired(t *testing.T) {
	g := testGovernor(2, governorEnforce)
	old := time.Now().Add(-time.Hour)
	oldFloor := putGovernorWorker(g, "old-floor", "old", old, 1, governorActive)
	oldExtra := putGovernorWorker(g, "old-extra", "old", old, 2, governorActive)
	newFloor := putGovernorWorker(g, "new", "new", time.Now(), 3, governorParked)
	g.evaluate()
	if !oldExtra.parkRequested {
		t.Fatal("expected pending park before capacity reopens")
	}
	delete(g.workers, newFloor.workerUUID)
	g.evaluate()
	if oldFloor.state != governorActive || oldExtra.state != governorActive || oldExtra.parkRequested {
		t.Fatalf("reopened slot did not cancel pending park: floor=%v extra=%v requested=%v", oldFloor.state, oldExtra.state, oldExtra.parkRequested)
	}
}

func TestGovernorObserveCapsFreshButDoesNotPreempt(t *testing.T) {
	g := testGovernor(2, governorObserve)
	age := time.Now().Add(-time.Hour)
	first := putGovernorWorker(g, "first", "old", age, 1, governorActive)
	second := putGovernorWorker(g, "second", "old", age, 2, governorActive)
	newcomer := putGovernorWorker(g, "new", "new", time.Now(), 3, governorParked)
	g.evaluate()
	if first.state != governorActive || second.state != governorActive || newcomer.state != governorParked {
		t.Fatal("observe mode preempted an active worker")
	}
}

func TestGovernorReacquireReplacementDoesNotDoubleRelease(t *testing.T) {
	g := testGovernor(1, governorEnforce)
	old := g.add("same", "job")
	newer := g.add("same", "job")
	if !old.released || g.workers["same"] != newer {
		t.Fatal("re-acquire did not replace stale worker")
	}
	g.release(old)
	if g.workers["same"] != newer {
		t.Fatal("stale deferred release removed replacement")
	}
}

func TestGovernorReacquireConcurrentOldDisconnectKeepsOneUUID(t *testing.T) {
	g := testGovernor(1, governorEnforce)
	old := g.add("same", "job")
	startOldDisconnect := make(chan struct{})
	disconnected := make(chan struct{})
	go func() {
		<-startOldDisconnect
		g.release(old)
		close(disconnected)
	}()
	newer := g.add("same", "job")
	close(startOldDisconnect)
	select {
	case <-disconnected:
	case <-time.After(2 * time.Second):
		t.Fatal("old disconnect did not complete")
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if len(g.workers) != 1 || g.workers["same"] != newer || newer.released {
		t.Fatalf("concurrent replacement left workers=%#v newer.released=%v", g.workers, newer.released)
	}
}

func TestGovernorDisconnectFreesOneSlotAndWakesOneWaiter(t *testing.T) {
	g := testGovernor(1, governorEnforce)
	age := time.Now()
	held := putGovernorWorker(g, "held", "job", age, 1, governorActive)
	woken := putGovernorWorker(g, "woken", "job", age, 2, governorParked)
	stillParked := putGovernorWorker(g, "still", "job", age, 3, governorParked)
	g.release(held)
	g.evaluate()
	if woken.state != governorActive || stillParked.state != governorParked || activeGovernorWorkers(g) != 1 {
		t.Fatalf("disconnect wake set wrong: woken=%v still=%v active=%d", woken.state, stillParked.state, activeGovernorWorkers(g))
	}
}

func TestGovernorStoresNextEstimateWithoutRankingOnIt(t *testing.T) {
	g := testGovernor(1, governorEnforce)
	age := time.Now()
	first := putGovernorWorker(g, "first", "job", age, 1, governorActive)
	second := putGovernorWorker(g, "second", "job", age, 2, governorParked)
	if _, _, err := g.checkpoint(first, 12, 1<<40); err != nil {
		t.Fatal(err)
	}
	g.evaluate()
	if first.state != governorActive || second.state != governorParked || first.nextEst != 1<<40 {
		t.Fatal("next estimate affected Slice 2 CPU selection")
	}
}

func TestGovernorRAMOrdersFittingParkedWorkerAheadOfBigWorker(t *testing.T) {
	// Revert-check: the pre-Slice-3 sequence-only fill chooses big (seq=2)
	// before small (seq=3). Near the ledger limit only small fits.
	g := testRAMGovernor(t, 3, 90, 100, true)
	age := time.Now()
	floor := putRAMGovernorWorker(g, "floor", "job", age, 1, governorActive, 1)
	big := putRAMGovernorWorker(g, "big", "job", age, 2, governorParked, 20)
	small := putRAMGovernorWorker(g, "small", "job", age, 3, governorParked, 5)
	g.evaluate()
	if floor.state != governorActive || small.state != governorActive || big.state != governorParked {
		t.Fatalf("RAM ordering selected floor=%v big=%v small=%v", floor.state, big.state, small.state)
	}
}

func TestGovernorRAMCandidateSortGroupsSlicesIntoTotalOrder(t *testing.T) {
	// Equal ages plus mixed RAM-aware slices used to make the comparator
	// non-transitive. The total order is age, slice, enabled-estimate, job ID,
	// then sequence: /alpha's job y wins before /beta's lexically earlier job a,
	// and /alpha's next estimates are ignored because that slice is RAM-off.
	for run := 0; run < 16; run++ {
		g := testRAMGovernorWithMemory(t, 4, func(slice string) (int64, int64, int64, bool, string) {
			if slice == "/beta" {
				return 90, 100, 0, true, "injected" // avail=10: RAM-on
			}
			return 10, 100, 0, true, "injected" // avail=90: RAM-off
		})
		age := time.Now()
		putRAMGovernorWorker(g, "floor-a", "a", age, 1, governorActive, 1)
		alphaZ := putRAMGovernorWorker(g, "alpha-z", "z", age, 2, governorParked, 1)
		alphaZ.slice = "/alpha"
		putRAMGovernorWorker(g, "floor-y", "y", age, 3, governorActive, 1)
		alphaY := putRAMGovernorWorker(g, "alpha-y", "y", age, 4, governorParked, 90)
		alphaY.slice = "/alpha"
		putRAMGovernorWorker(g, "floor-z", "z", age, 5, governorActive, 1)
		betaA := putRAMGovernorWorker(g, "beta-a", "a", age, 6, governorParked, 5)
		betaA.slice = "/beta"

		g.evaluate()
		if alphaY.state != governorActive || alphaZ.state != governorParked || betaA.state != governorParked {
			t.Fatalf("run %d: mixed-slice candidate ordering wrong: alpha-y=%v alpha-z=%v beta-a=%v", run, alphaY.state, alphaZ.state, betaA.state)
		}
	}
}

func TestGovernorRAMUnknownEstimateIsNotFreeNearLimit(t *testing.T) {
	// Revert-check: treating zero as a fit activates unknown immediately. With
	// no fitting competitor, RAM blocking must not create starvation credit.
	g := testRAMGovernor(t, 2, 90, 100, true)
	age := time.Now()
	putRAMGovernorWorker(g, "floor", "job", age, 1, governorActive, 1)
	unknown := putRAMGovernorWorker(g, "unknown", "job", age, 2, governorParked, 0)
	g.evaluate()
	if unknown.state != governorParked || unknown.ramSkips != 0 {
		t.Fatalf("unknown estimate was treated as free or credited without displacement: state=%v skips=%d", unknown.state, unknown.ramSkips)
	}
}

func TestGovernorRAMFloorIsNeverGated(t *testing.T) {
	// Revert-check: applying the fit test to the floor leaves the second job
	// parked forever when its estimate exceeds headroom.
	g := testRAMGovernor(t, 2, 90, 100, true)
	age := time.Now()
	putRAMGovernorWorker(g, "a", "a", age, 1, governorActive, 1)
	floor := putRAMGovernorWorker(g, "b", "b", age, 2, governorParked, 1<<40)
	g.evaluate()
	if floor.state != governorActive {
		t.Fatal("RAM-gated a job floor")
	}
}

func TestGovernorRAMNeverPreemptsAnActiveWorker(t *testing.T) {
	// Revert-check: applying RAM fit to active membership park-requests the
	// large worker below merely because a parked small worker fits.
	g := testRAMGovernor(t, 2, 90, 100, true)
	age := time.Now()
	putRAMGovernorWorker(g, "floor", "job", age, 1, governorActive, 1)
	active := putRAMGovernorWorker(g, "active", "job", age, 2, governorActive, 1<<40)
	putRAMGovernorWorker(g, "small", "job", age, 3, governorParked, 5)
	g.evaluate()
	if active.state != governorActive || active.parkRequested {
		t.Fatalf("RAM-preempted active worker: state=%v parkRequested=%v", active.state, active.parkRequested)
	}
}

func TestGovernorRAMCacheDiscountDoesNotPreemptActiveWorker(t *testing.T) {
	// The larger reclaimable-aware advisory value turns RAM ordering off; it
	// must not park a worker that is already running.
	g := testRAMGovernorWithMemory(t, 2, func(string) (int64, int64, int64, bool, string) {
		return 90, 100, 80, true, "injected"
	})
	if available, ok := g.server.admitAvailable("/slice"); !ok || available != 90 {
		t.Fatalf("cache-discount availability=%d ok=%v, want 90 true", available, ok)
	}
	age := time.Now()
	putRAMGovernorWorker(g, "floor", "job", age, 1, governorActive, 1)
	active := putRAMGovernorWorker(g, "active", "job", age, 2, governorActive, 1<<40)
	putRAMGovernorWorker(g, "small", "job", age, 3, governorParked, 5)
	g.evaluate()
	if active.state != governorActive || active.parkRequested {
		t.Fatalf("cache discount RAM-preempted active worker: state=%v parkRequested=%v", active.state, active.parkRequested)
	}
}

func TestGovernorRAMDoesNotForceWithoutFittingCompetitor(t *testing.T) {
	// Revert-check: crediting every evaluator kick forces this lone worker after
	// five event-driven passes, despite there being nobody that displaced it.
	g := testRAMGovernor(t, 2, 90, 100, true)
	age := time.Now()
	putRAMGovernorWorker(g, "floor", "job", age, 1, governorActive, 1)
	big := putRAMGovernorWorker(g, "big", "job", age, 2, governorParked, 20)
	for kick := 0; kick < 16; kick++ {
		g.signal()
		g.evaluate()
	}
	if big.state != governorParked || big.ramSkips != 0 {
		t.Fatalf("lone RAM-blocked worker was forced by evaluator churn: state=%v skips=%d", big.state, big.ramSkips)
	}
}

func TestGovernorRAMForcesOneWorkerAfterBoundedDisplacements(t *testing.T) {
	// Revert-check: without displacement-only credits this either forces on
	// daemon event volume or never lets a genuinely bypassed worker enter #69.
	t.Setenv("AIRA_GOVERNOR_RAM_SKIP_BOUND", "3")
	g := testRAMGovernor(t, 2, 90, 100, true)
	age := time.Now()
	putRAMGovernorWorker(g, "floor", "job", age, 1, governorActive, 1)
	big := putRAMGovernorWorker(g, "big", "job", age, 2, governorParked, 20)
	small := putRAMGovernorWorker(g, "small", "job", age, 3, governorParked, 5)
	for i := 0; i < 3; i++ {
		g.evaluate()
		if big.state != governorParked {
			t.Fatalf("worker forced before a fresh scheduling chance after skip bound at pass %d", i+1)
		}
		if small.state != governorActive {
			t.Fatalf("fitting worker was not selected at pass %d", i+1)
		}
		if big.ramSkips != i+1 {
			t.Fatalf("displacement credit after pass %d = %d, want %d", i+1, big.ramSkips, i+1)
		}
		// Model the fitting worker returning to the parked set before the next
		// selection, leaving the big worker continually bypassed by fit fill.
		// Re-parking must allocate a fresh epoch exactly like checkpoint() does,
		// so the next activation wakes the epoch rather than re-closing the
		// already-closed grant.
		small.state = governorParked
		small.epoch = make(chan struct{})
	}
	g.evaluate()
	if big.state != governorActive || big.ramSkips != 0 {
		t.Fatalf("worker not forced after exactly the skip bound: state=%v skips=%d", big.state, big.ramSkips)
	}
}

func TestGovernorRAMHeadroomDecrementsWithinTick(t *testing.T) {
	g := testRAMGovernor(t, 3, 90, 100, true) // avail = max-current = 10
	age := time.Now()
	floor := putRAMGovernorWorker(g, "floor", "job", age, 1, governorActive, 1)
	a := putRAMGovernorWorker(g, "a", "job", age, 2, governorParked, 6)
	b := putRAMGovernorWorker(g, "b", "job", age, 3, governorParked, 6)
	g.evaluate()
	activated := 0
	for _, w := range []*governorWorker{a, b} {
		if w.state == governorActive {
			activated++
		}
	}
	if floor.state != governorActive || activated != 1 {
		t.Fatalf("running-headroom decrement not enforced within tick: floor=%v a=%v b=%v", floor.state, a.state, b.state)
	}
}

func TestGovernorRAMOrderingTurnsOffAboveHighMark(t *testing.T) {
	// With avail=90 (> high mark 40), ordinary sequence ordering must win;
	// always-on RAM sorting would choose small instead.
	g := testRAMGovernor(t, 2, 10, 100, true)
	age := time.Now()
	putRAMGovernorWorker(g, "floor", "job", age, 1, governorActive, 1)
	big := putRAMGovernorWorker(g, "big", "job", age, 2, governorParked, 20)
	small := putRAMGovernorWorker(g, "small", "job", age, 3, governorParked, 5)
	g.evaluate()
	if big.state != governorActive || small.state != governorParked {
		t.Fatalf("RAM ordering remained on above high mark: big=%v small=%v", big.state, small.state)
	}
}

func TestGovernorRAMOrderingHysteresisTransition(t *testing.T) {
	current := int64(90) // avail=10: below low mark, RAM ordering turns on.
	g := testRAMGovernorWithMemory(t, 2, func(string) (int64, int64, int64, bool, string) {
		return current, 100, 0, true, "injected"
	})
	age := time.Now()
	putRAMGovernorWorker(g, "floor", "job", age, 1, governorActive, 1)
	big := putRAMGovernorWorker(g, "big", "job", age, 2, governorParked, 20)
	small := putRAMGovernorWorker(g, "small", "job", age, 3, governorParked, 5)
	g.evaluate()
	if small.state != governorActive || big.state != governorParked || !g.ramAware["/slice"] {
		t.Fatalf("low-headroom evaluation did not enable RAM ordering: big=%v small=%v aware=%v", big.state, small.state, g.ramAware["/slice"])
	}

	// The hysteresis band retains the enabled state, so re-parking small still
	// selects it before big even though availability rose to 30.
	small.state, small.epoch = governorParked, make(chan struct{})
	current = 70
	g.evaluate()
	if small.state != governorActive || big.state != governorParked || !g.ramAware["/slice"] {
		t.Fatalf("hysteresis band changed RAM ordering: big=%v small=%v aware=%v", big.state, small.state, g.ramAware["/slice"])
	}

	// Crossing high mark turns RAM ordering off and restores sequence order.
	small.state, small.epoch = governorParked, make(chan struct{})
	current = 10
	g.evaluate()
	if big.state != governorActive || small.state != governorParked || g.ramAware["/slice"] {
		t.Fatalf("high-headroom evaluation did not restore sequence ordering: big=%v small=%v aware=%v", big.state, small.state, g.ramAware["/slice"])
	}
}

func TestGovernorRAMUnreadableLedgerFailsOpen(t *testing.T) {
	// Revert-check: treating an unreadable ledger as zero headroom parks this
	// worker despite the advisory path's required fail-open behavior.
	g := testRAMGovernor(t, 2, 0, 0, false)
	age := time.Now()
	putRAMGovernorWorker(g, "floor", "job", age, 1, governorActive, 1)
	waiter := putRAMGovernorWorker(g, "waiter", "job", age, 2, governorParked, 1<<40)
	g.evaluate()
	if waiter.state != governorActive {
		t.Fatal("unreadable ledger did not fail open")
	}
}

func TestGovernorWholeChargedDelegateBypassesRAMOrderingNearCeiling(t *testing.T) {
	// Deliberately exercise the enabled regime: available=10 is below the
	// low mark (20), and a normal 50-byte estimate would be parked.
	g := testRAMGovernor(t, 2, 90, 100, true)
	queue := &sliceQueue{outstanding: 90, outstandingJobs: 1}
	g.server.admitQueues["/slice"] = queue
	age := time.Now()
	putRAMGovernorWorker(g, "floor", "ordinary", age, 1, governorActive, 1)
	whole := putRAMGovernorWorker(g, "whole", "CONFINE-@drc-suite-1-a", age, 2, governorParked, 50)
	blocked := putRAMGovernorWorker(g, "blocked", "ordinary", age, 3, governorParked, 50)
	g.evaluate()
	if whole.state != governorActive || blocked.state != governorParked || !g.ramAware["/slice"] {
		t.Fatalf("whole-charged worker was not exempt in RAM-aware regime: whole=%v blocked=%v aware=%v", whole.state, blocked.state, g.ramAware["/slice"])
	}
}

func TestGovernorRAMDoesNotChargeAdmissionLedger(t *testing.T) {
	// Revert-check: any Model-B-style governor reservation changes this exact
	// #67/#69 ledger snapshot while merely evaluating parked candidates.
	g := testRAMGovernor(t, 2, 90, 100, true)
	queue := &sliceQueue{outstanding: 7, outstandingJobs: 1, adopted: 3, adoptedJobs: 1}
	g.server.admitQueues["/slice"] = queue
	age := time.Now()
	putRAMGovernorWorker(g, "floor", "job", age, 1, governorActive, 1)
	putRAMGovernorWorker(g, "small", "job", age, 2, governorParked, 5)
	g.evaluate()
	queue.mu.Lock()
	defer queue.mu.Unlock()
	if queue.outstanding != 7 || queue.outstandingJobs != 1 || queue.adopted != 3 || queue.adoptedJobs != 1 {
		t.Fatalf("governor charged ledger: %+v", queue)
	}
}
