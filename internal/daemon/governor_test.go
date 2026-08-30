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
