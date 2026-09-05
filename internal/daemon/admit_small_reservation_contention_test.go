package daemon

import (
	"fmt"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"aira/internal/testdeadline"
)

// verifies: AIRA-59 end-to-end at the REAL entry point, reproducing the live
// incident rather than a resemblance of it.
//
// Shape taken from the observed failure: one unrelated 32G-class head waiter on
// the shared machine-wide aira.slice queue, and behind it many ~1G pinned
// small reservations — each a genuine `aira confine-reserve --bytes ~1G --pinned
// --signature pytest:<nodeid> --max-wait 300s`, which reaches the daemon as the
// `admit` verb and lands in this same sliceQueue (there is no separate
// small-reservation admission path).
//
// AIRA-33 note: the original incident's small reservations came from the pytest
// RAM governor, one per test, and this file was called
// admit_governor_contention_test.go for that reason. That plugin is deleted; the
// FAIRNESS DEFECT it exposed is not, and neither is the shape — any caller
// queueing many small pinned reservations behind one large head reproduces it
// (`aira confine-reserve` is still a supported verb). The file and test were
// renamed off the governor precisely so a future cleanup sweeping up
// governor-named files cannot delete a live AIRA-59 regression by mistake.
//
// Before the duty bound, the freeze re-armed on every pass for as long as the
// head stayed queued, so those per-test reservations were blocked for the head's
// entire timeout — minutes each, on a slice with multi-GB of headroom they would
// each have fitted in. That is the reported "idle box, everything queued".
//
// Driven concurrently through admitConnection with real goroutines and the real
// connection lifecycle, because the freeze's damage is only observable across
// the enqueue/grant/release path, not by calling the evaluator directly.
func TestSmallPinnedReservationsAreNotStalledByALargeNeighbourHead(t *testing.T) {
	const gib = int64(1) << 30

	var maximum atomic.Int64
	maximum.Store(64 * gib)
	server := admitTestServer(&maximum)
	server.admitSliceHeadroomBase = 0
	server.admitSliceHeadroomSupervisor = 0
	server.admitBackfillGrace = 10 * time.Second
	server.admitFreezeMaxHold = time.Minute
	server.admitPollInterval = time.Hour // passes are driven explicitly below

	var clockMu sync.Mutex
	now := time.Unix(30000, 0)
	server.admitNow = func() time.Time {
		clockMu.Lock()
		defer clockMu.Unlock()
		return now
	}
	advance := func(step time.Duration) {
		clockMu.Lock()
		now = now.Add(step)
		clockMu.Unlock()
	}

	// 34G already charged against a 64G slice: ~30G genuinely free — exactly the
	// "abundant headroom while everything queues" condition that was observed.
	var evaluations atomic.Int64
	server.admitReadMemory = func(string) (int64, int64, int64, bool, string) {
		evaluations.Add(1)
		return 34 * gib, maximum.Load(), 0, true, ""
	}

	// A granted admission does NOT return from admitConnection: the daemon holds
	// the connection open as the reservation lease. So grants are observed on the
	// production admitBeforeWrite seam, which fires exactly once per delivered
	// grant, rather than by waiting for the handler to exit.
	var grantMu sync.Mutex
	grantsByReserve := map[int64]int{}
	server.admitBeforeWrite = func(waiter *admitWaiter) {
		grantMu.Lock()
		grantsByReserve[waiter.reserve]++
		grantMu.Unlock()
	}
	grantsOf := func(reserve int64) int {
		grantMu.Lock()
		defer grantMu.Unlock()
		return grantsByReserve[reserve]
	}

	type call struct {
		name string
		conn net.Conn
	}
	start := func(name string, args map[string]any) *call {
		serverConn, clientConn := net.Pipe()
		item := &call{name: name, conn: clientConn}
		go func() { server.admitConnection(serverConn, args) }()
		// Drain the client so a delivered grant is never blocked on the pipe.
		go func() {
			buffer := make([]byte, 4096)
			for {
				if _, err := clientConn.Read(buffer); err != nil {
					return
				}
			}
		}()
		return item
	}

	// Force an evaluator pass and wait for it to actually run.
	settle := func() {
		t.Helper()
		server.admitRegistryMu.Lock()
		queues := make([]*sliceQueue, 0, len(server.admitQueues))
		for _, queue := range server.admitQueues {
			queues = append(queues, queue)
		}
		server.admitRegistryMu.Unlock()
		before := evaluations.Load()
		for _, queue := range queues {
			queue.signal()
		}
		deadline := time.Now().Add(testdeadline.Wait(2 * time.Second))
		for evaluations.Load() == before && time.Now().Before(deadline) {
			time.Sleep(time.Millisecond)
		}
		time.Sleep(20 * time.Millisecond)
	}

	// The unrelated neighbour: a 32G-class request that cannot fit in ~30G.
	head := start("merge-gate-32G", validAdmitArgs(32*gib, int64(30*time.Minute/time.Millisecond)))
	defer head.conn.Close()
	settle()

	// Past the backfill grace the freeze arms and protects the head.
	advance(11 * time.Second)
	settle()

	// The small pinned reservations arrive behind it. Each would fit comfortably
	// in the ~30G of real headroom.
	perTest := make([]*call, 0, 8)
	for index := 0; index < 8; index++ {
		perTest = append(perTest, start(
			fmt.Sprintf("pytest-reserve-%d", index),
			map[string]any{
				"slice": "slice", "reserve": gib, "pinned": true,
				"signature":   fmt.Sprintf("pytest:test_module.py::test_case_%d", index),
				"max_wait_ms": int64(300 * time.Second / time.Millisecond),
			}))
	}
	for _, item := range perTest {
		defer item.conn.Close()
	}
	settle()

	// All eight must actually be ENQUEUED before the clock advances, or scheduling
	// could let them arrive during the yield and the test would prove nothing
	// about the freeze at all.
	queuedNow := func() int {
		server.admitRegistryMu.Lock()
		queue := server.admitQueues["/slice"]
		server.admitRegistryMu.Unlock()
		if queue == nil {
			return 0
		}
		queue.mu.Lock()
		defer queue.mu.Unlock()
		count := 0
		for _, waiter := range queue.waiters {
			if waiter != nil && waiter.state == admitQueued && waiter.reserve == gib {
				count++
			}
		}
		return count
	}
	deadline := time.Now().Add(testdeadline.Wait(2 * time.Second))
	for queuedNow() < len(perTest) && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
		settle()
	}
	if got := queuedNow(); got != len(perTest) {
		t.Fatalf("only %d/%d per-test reservations were queued before the clock advanced; the freeze was never actually exercised", got, len(perTest))
	}

	// While the freeze holds, they are blocked. This is the CORRECT half of the
	// mechanism — head-of-line protection — and must survive the fix.
	if granted := grantsOf(gib); granted != 0 {
		t.Fatalf("%d per-test reservations were admitted during the protective hold; head-of-line protection was removed, not bounded", granted)
	}

	// Once the hold is spent the queue yields and every fitting per-test
	// reservation is admitted, on a slice that had the room all along.
	advance(61 * time.Second)
	settle()
	settle()

	if granted := grantsOf(gib); granted != len(perTest) {
		t.Fatalf("after the freeze yielded only %d/%d per-test reservations were admitted; they were still stalled behind an unrelated head on a slice with ~30G free — the AIRA-59 stall",
			granted, len(perTest))
	}

	// The head is still queued (it genuinely does not fit) and, crucially, the
	// ledger never exceeded cap-headroom while backfilling around it.
	if grantsOf(32*gib) != 0 {
		t.Fatal("the 32G head was granted although it never fitted")
	}

	server.admitRegistryMu.Lock()
	queue := server.admitQueues["/slice"]
	server.admitRegistryMu.Unlock()
	if queue == nil {
		t.Fatal("admission queue disappeared")
	}
	queue.mu.Lock()
	outstanding, jobs := queue.outstanding, queue.outstandingJobs
	queue.mu.Unlock()

	if want := int64(len(perTest)) * gib; outstanding != want {
		t.Fatalf("outstanding=%d, want exactly the %d granted per-test reserves (%d)", outstanding, len(perTest), want)
	}
	if ceiling := maximum.Load() - server.admitSliceHeadroom(jobs+1); outstanding > ceiling {
		t.Fatalf("yield over-admitted: outstanding=%d exceeds cap-headroom %d", outstanding, ceiling)
	}
}
