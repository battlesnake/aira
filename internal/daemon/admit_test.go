package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"math"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"aira/internal/core"
)

func admitTestServer(maximum *atomic.Int64) *Server {
	server := NewServer(Paths{})
	server.stopping = make(chan struct{})
	server.admitPollInterval = time.Hour
	server.admitSliceHeadroomBase = 0
	server.admitSliceHeadroomSupervisor = 0
	server.admitResolveSlice = func(string) (string, bool, string) { return "/slice", true, "" }
	server.admitConfineScan = noConfinesScan
	server.admitReadMemory = func(string) (int64, int64, int64, bool, string) {
		return 0, maximum.Load(), 0, true, ""
	}
	return server
}

func enqueueAdmitTest(t *testing.T, server *Server, reserve int64) (*sliceQueue, *admitWaiter) {
	t.Helper()
	queue, waiter, code, err := server.enqueueAdmit("/slice", reserve)
	if err != nil {
		t.Fatalf("enqueue: code=%s err=%v", code, err)
	}
	return queue, waiter
}

func waitAdmitGrant(t *testing.T, waiter *admitWaiter) {
	t.Helper()
	select {
	case <-waiter.grantedCh:
	case <-time.After(time.Second):
		t.Fatalf("waiter %d was not granted", waiter.seq)
	}
}

func requireAdmitQueued(t *testing.T, waiter *admitWaiter) {
	t.Helper()
	select {
	case <-waiter.grantedCh:
		t.Fatalf("waiter %d unexpectedly granted", waiter.seq)
	default:
	}
}

func TestAdmitFIFOOrderAndMidstreamArrival(t *testing.T) {
	var maximum atomic.Int64
	server := admitTestServer(&maximum)
	queue, a := enqueueAdmitTest(t, server, 60)
	_, b := enqueueAdmitTest(t, server, 60)
	_, c := enqueueAdmitTest(t, server, 60)
	maximum.Store(100)
	queue.signal()
	waitAdmitGrant(t, a)
	requireAdmitQueued(t, b)
	requireAdmitQueued(t, c)
	_, d := enqueueAdmitTest(t, server, 60)
	if !(a.seq < b.seq && b.seq < c.seq && c.seq < d.seq) {
		t.Fatalf("arrival sequence=%d,%d,%d,%d", a.seq, b.seq, c.seq, d.seq)
	}
	for _, pair := range []struct {
		release *admitWaiter
		next    *admitWaiter
	}{{a, b}, {b, c}, {c, d}} {
		server.releaseAdmitWaiter(queue, pair.release)
		waitAdmitGrant(t, pair.next)
	}
	server.releaseAdmitWaiter(queue, d)
}

func TestValidateAdmitArgsRejectsTraversalShapedConfineScopeID(t *testing.T) {
	base := map[string]any{
		"slice": "aira.slice", "reserve": int64(1), "max_wait_ms": int64(1),
		"name": "job", "owner": "session-a",
	}
	for _, scopeID := range []string{"../CONFINE-job-1-a", "CONFINE-../job-1-a", "CONFINE-job/child-1-a", ".aira-CONFINE-job-1-a", "CONFINE-job-x-a", "CONFINE-job-1-A"} {
		args := make(map[string]any, len(base)+1)
		for key, value := range base {
			args[key] = value
		}
		args["scope_id"] = scopeID
		if _, err := validateAdmitArgs(args); err == nil {
			t.Fatalf("scope_id %q accepted", scopeID)
		}
	}
	valid := make(map[string]any, len(base)+1)
	for key, value := range base {
		valid[key] = value
	}
	valid["scope_id"] = "CONFINE-job-123-abc9"
	request, err := validateAdmitArgs(valid)
	if err != nil || request.scopeID != valid["scope_id"] || request.owner != "session-a" {
		t.Fatalf("request=%+v err=%v", request, err)
	}
	valid["scope_id"] = "CONFINE-@dr-job-with-dash-123-abc9"
	valid["name"] = "job-with-dash"
	valid["delegate_ram"] = true
	request, err = validateAdmitArgs(valid)
	if err != nil || !request.delegateRAM || request.name != "job-with-dash" {
		t.Fatalf("marked request=%+v err=%v", request, err)
	}
}

func TestAdmitPrefixConcurrencyAndNoJumpAhead(t *testing.T) {
	var maximum atomic.Int64
	maximum.Store(100)
	server := admitTestServer(&maximum)
	server.admitBackfillGrace = 0
	queue := &sliceQueue{path: "/slice", server: server}
	a := &admitWaiter{seq: 1, reserve: 40, state: admitQueued, grantedCh: make(chan struct{}), enqueued: time.Now()}
	b := &admitWaiter{seq: 2, reserve: 40, state: admitQueued, grantedCh: make(chan struct{}), enqueued: time.Now()}
	c := &admitWaiter{seq: 3, reserve: 40, state: admitQueued, grantedCh: make(chan struct{}), enqueued: time.Now()}
	queue.waiters = []*admitWaiter{a, b, c}
	server.evaluateAdmitQueue(queue)
	waitAdmitGrant(t, a)
	waitAdmitGrant(t, b)
	requireAdmitQueued(t, c)
	if queue.outstanding != 80 {
		t.Fatalf("outstanding=%d, want 80", queue.outstanding)
	}

	large := &admitWaiter{seq: 4, reserve: 120, state: admitQueued, grantedCh: make(chan struct{}), enqueued: time.Now()}
	small := &admitWaiter{seq: 5, reserve: 1, state: admitQueued, grantedCh: make(chan struct{}), enqueued: time.Now()}
	blocked := &sliceQueue{path: "/slice", server: server, waiters: []*admitWaiter{large, small}}
	server.evaluateAdmitQueue(blocked)
	requireAdmitQueued(t, large)
	requireAdmitQueued(t, small)
	maximum.Store(121)
	server.evaluateAdmitQueue(blocked)
	waitAdmitGrant(t, large)
	waitAdmitGrant(t, small)
}

func TestAdmitBackfillsSmallWaitersPastBlockedHeadAndAccountsExactly(t *testing.T) {
	var maximum atomic.Int64
	maximum.Store(100)
	server := admitTestServer(&maximum)
	now := time.Unix(1000, 0)
	server.admitNow = func() time.Time { return now }
	server.admitBackfillGrace = time.Minute
	server.admitSliceHeadroomBase = 10
	head := &admitWaiter{seq: 1, reserve: 101, state: admitQueued, grantedCh: make(chan struct{}), enqueued: now}
	smallA := &admitWaiter{seq: 2, reserve: 30, state: admitQueued, grantedCh: make(chan struct{}), enqueued: now}
	smallB := &admitWaiter{seq: 3, reserve: 40, state: admitQueued, grantedCh: make(chan struct{}), enqueued: now}
	queue := &sliceQueue{path: "/slice", server: server, waiters: []*admitWaiter{head, smallA, smallB}}

	server.evaluateAdmitQueue(queue)

	requireAdmitQueued(t, head)
	waitAdmitGrant(t, smallA)
	waitAdmitGrant(t, smallB)
	if queue.outstanding != 70 || queue.outstandingJobs != 2 {
		t.Fatalf("backfill ledger outstanding=%d jobs=%d, want exact granted sum 70/2", queue.outstanding, queue.outstandingJobs)
	}
	if ceiling := maximum.Load() - server.admitSliceHeadroom(queue.outstandingJobs+1); queue.outstanding > ceiling {
		t.Fatalf("backfill exceeded ceiling: outstanding=%d ceiling=%d", queue.outstanding, ceiling)
	}
}

func TestAdmitBackfillFreezesForAnOldBlockedHead(t *testing.T) {
	var maximum atomic.Int64
	maximum.Store(100)
	server := admitTestServer(&maximum)
	now := time.Unix(2000, 0)
	current := int64(50)
	server.admitNow = func() time.Time { return now }
	server.admitBackfillGrace = 10 * time.Second
	server.admitReadMemory = func(string) (int64, int64, int64, bool, string) { return current, maximum.Load(), 0, true, "" }
	head := &admitWaiter{seq: 1, reserve: 60, state: admitQueued, grantedCh: make(chan struct{}), enqueued: now.Add(-10 * time.Second)}
	small := &admitWaiter{seq: 2, reserve: 30, state: admitQueued, grantedCh: make(chan struct{}), enqueued: now}
	queue := &sliceQueue{path: "/slice", server: server, waiters: []*admitWaiter{head, small}}

	server.evaluateAdmitQueue(queue)

	requireAdmitQueued(t, head)
	requireAdmitQueued(t, small)
	if queue.outstanding != 0 || queue.outstandingJobs != 0 {
		t.Fatalf("freeze spent reserve ahead of head: outstanding=%d jobs=%d", queue.outstanding, queue.outstandingJobs)
	}
	current = 0
	server.evaluateAdmitQueue(queue)
	waitAdmitGrant(t, head)
}

func TestAdmitBackfillGraceZeroAndDisabledAreStrictFIFO(t *testing.T) {
	for _, test := range []struct {
		name, value string
	}{{name: "zero", value: "0"}, {name: "disabled", value: "disabled"}} {
		t.Run(test.name, func(t *testing.T) {
			t.Setenv("AIRA_DAEMON_ADMIT_BACKFILL_GRACE", test.value)
			grace, err := admitBackfillGraceFromEnv()
			if err != nil || grace != 0 {
				t.Fatalf("grace=%v err=%v, want disabled", grace, err)
			}
			var maximum atomic.Int64
			maximum.Store(100)
			server := admitTestServer(&maximum)
			server.admitBackfillGrace = grace
			now := time.Unix(3000, 0)
			server.admitNow = func() time.Time { return now }
			head := &admitWaiter{seq: 1, reserve: 101, state: admitQueued, grantedCh: make(chan struct{}), enqueued: now}
			small := &admitWaiter{seq: 2, reserve: 1, state: admitQueued, grantedCh: make(chan struct{}), enqueued: now}
			queue := &sliceQueue{path: "/slice", server: server, waiters: []*admitWaiter{head, small}}

			server.evaluateAdmitQueue(queue)

			requireAdmitQueued(t, head)
			requireAdmitQueued(t, small)
			if queue.outstanding != 0 || queue.outstandingJobs != 0 {
				t.Fatalf("strict FIFO granted past blocked head: outstanding=%d jobs=%d", queue.outstanding, queue.outstandingJobs)
			}
		})
	}
}

func TestAdmitBlockedHeadRejectsSaturatedAtWaitCap(t *testing.T) {
	var maximum atomic.Int64
	maximum.Store(100)
	server := admitTestServer(&maximum)
	now := time.Unix(4000, 0)
	var nowMu sync.Mutex
	advanceNow := func(wait time.Duration) time.Time {
		nowMu.Lock()
		defer nowMu.Unlock()
		now = now.Add(wait)
		return now
	}
	server.admitNow = func() time.Time {
		nowMu.Lock()
		defer nowMu.Unlock()
		return now
	}
	server.admitBackfillGrace = 10 * time.Second
	var evaluations atomic.Int64
	server.admitReadMemory = func(string) (int64, int64, int64, bool, string) {
		evaluations.Add(1)
		return 50, 100, 0, true, ""
	}
	timerReady := make(chan struct{})
	var deadline chan time.Time
	var deadlineAt time.Time
	server.admitAfter = func(wait time.Duration) <-chan time.Time {
		if wait != time.Duration(admitWaitCapMs)*time.Millisecond {
			t.Fatalf("deadline wait=%s, want cap", wait)
		}
		nowMu.Lock()
		deadline = make(chan time.Time, 1)
		deadlineAt = now.Add(wait)
		nowMu.Unlock()
		close(timerReady)
		return deadline
	}
	serverConn, clientConn := net.Pipe()
	done := make(chan struct{})
	go func() {
		defer close(done)
		defer serverConn.Close()
		server.admitConnection(serverConn, validAdmitArgs(60, admitWaitCapMs))
	}()
	select {
	case <-timerReady:
	case <-time.After(time.Second):
		t.Fatal("blocked head did not install its deadline")
	}

	// At nine seconds the blocked head is still within its backfill grace, so
	// this first fitting waiter must be admitted past it.
	advanceNow(9 * time.Second)
	queue, preFreeze := enqueueAdmitTest(t, server, 30)
	waitAdmitGrant(t, preFreeze)

	// Once the head reaches its grace age, later fitting waiters must remain
	// queued on this and subsequent evaluator passes.
	advanceNow(time.Second)
	_, laterA := enqueueAdmitTest(t, server, 10)
	_, laterB := enqueueAdmitTest(t, server, 10)
	requireAdmitQueued(t, laterA)
	requireAdmitQueued(t, laterB)
	previousEvaluations := evaluations.Load()
	queue.signal()
	deadlineForPass := time.Now().Add(time.Second)
	for evaluations.Load() == previousEvaluations && time.Now().Before(deadlineForPass) {
		time.Sleep(time.Millisecond)
	}
	if evaluations.Load() == previousEvaluations {
		t.Fatal("frozen queue did not run a subsequent evaluator pass")
	}
	requireAdmitQueued(t, laterA)
	requireAdmitQueued(t, laterB)

	nowMu.Lock()
	now = deadlineAt
	expiredAt := now
	nowMu.Unlock()
	deadline <- expiredAt
	var frame ResponseFrame
	if err := readFrame(clientConn, &frame); err != nil {
		t.Fatal(err)
	}
	if frame.Code != CodeAdmitSaturated {
		t.Fatalf("blocked head frame=%+v, want saturated", frame)
	}
	_ = clientConn.Close()
	<-done
	server.releaseAdmitWaiter(queue, preFreeze)
	server.releaseAdmitWaiter(queue, laterA)
	server.releaseAdmitWaiter(queue, laterB)
}

func TestAdmitWeightedReservationsBoundConcurrentSumAcrossSuites(t *testing.T) {
	var maximum atomic.Int64
	maximum.Store(100)
	server := admitTestServer(&maximum)
	queue, suiteAHeavy := enqueueAdmitTest(t, server, 70)
	_, suiteBHeavy := enqueueAdmitTest(t, server, 70)
	waitAdmitGrant(t, suiteAHeavy)
	requireAdmitQueued(t, suiteBHeavy)
	queue.mu.Lock()
	if queue.outstanding > maximum.Load() || queue.outstanding != 70 {
		t.Fatalf("heavy outstanding=%d cap=%d", queue.outstanding, maximum.Load())
	}
	queue.mu.Unlock()
	server.releaseAdmitWaiter(queue, suiteAHeavy)
	waitAdmitGrant(t, suiteBHeavy)
	server.releaseAdmitWaiter(queue, suiteBHeavy)

	var lights []*admitWaiter
	for index := 0; index < 5; index++ {
		lightQueue, waiter := enqueueAdmitTest(t, server, 20)
		if index == 0 {
			queue = lightQueue
		}
		lights = append(lights, waiter)
	}
	for _, waiter := range lights {
		waitAdmitGrant(t, waiter)
	}
	queue.mu.Lock()
	if queue.outstanding != 100 || queue.outstanding > maximum.Load() || queue.outstandingJobs != 5 {
		t.Fatalf("light outstanding=%d jobs=%d cap=%d", queue.outstanding, queue.outstandingJobs, maximum.Load())
	}
	queue.mu.Unlock()
	for _, waiter := range lights {
		server.releaseAdmitWaiter(queue, waiter)
	}
}

func TestAdmitReservationAtomicProgressUnderChurn(t *testing.T) {
	var maximum atomic.Int64
	maximum.Store(100)
	server := admitTestServer(&maximum)
	queue, a := enqueueAdmitTest(t, server, 100)
	waitAdmitGrant(t, a)
	_, b := enqueueAdmitTest(t, server, 100)
	requireAdmitQueued(t, b)

	var churn sync.WaitGroup
	extraWaiter := make(chan *admitWaiter, 1)
	churn.Add(3)
	go func() { defer churn.Done(); server.releaseAdmitWaiter(queue, a) }()
	go func() {
		defer churn.Done()
		for index := 0; index < 100; index++ {
			queue.signal()
		}
	}()
	go func() {
		defer churn.Done()
		_, extra, _, err := server.enqueueAdmit("/slice", 100)
		if err != nil {
			return
		}
		for index := 0; index < 100; index++ {
			queue.signal()
		}
		extraWaiter <- extra
	}()
	churn.Wait()
	waitAdmitGrant(t, b)
	queue.mu.Lock()
	if queue.outstanding != 100 {
		t.Fatalf("outstanding=%d, want one reservation", queue.outstanding)
	}
	queue.mu.Unlock()
	server.releaseAdmitWaiter(queue, b)
	select {
	case extra := <-extraWaiter:
		waitAdmitGrant(t, extra)
		server.releaseAdmitWaiter(queue, extra)
	default:
	}
}

func TestAdmitReleaseAndDeathLifecycle(t *testing.T) {
	var maximum atomic.Int64
	server := admitTestServer(&maximum)
	queue, queued := enqueueAdmitTest(t, server, 10)
	server.releaseAdmitWaiter(queue, queued)
	if queued.state != admitReleased {
		t.Fatalf("queued death state=%v", queued.state)
	}
	requireAdmitQueued(t, queued)

	maximum.Store(10)
	queue, granted := enqueueAdmitTest(t, server, 10)
	waitAdmitGrant(t, granted)
	server.releaseAdmitWaiter(queue, granted)
	server.releaseAdmitWaiter(queue, granted)
	if queue.outstanding != 0 || granted.state != admitReleased {
		t.Fatalf("after granted death outstanding=%d state=%v", queue.outstanding, granted.state)
	}
	server.admitRegistryMu.Lock()
	if len(server.admitQueues) != 0 {
		t.Fatalf("empty registry not pruned: %d", len(server.admitQueues))
	}
	server.admitRegistryMu.Unlock()
}

func TestAdmitFailedWriteAndCloseBetweenCommitAndWriteReleaseOnce(t *testing.T) {
	for _, test := range []struct {
		name      string
		peerClose bool
	}{
		{name: "failed write"},
		{name: "close between commit and write", peerClose: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			var maximum atomic.Int64
			maximum.Store(100)
			server := admitTestServer(&maximum)
			serverConn, clientConn := net.Pipe()
			defer clientConn.Close()
			if test.peerClose {
				server.admitBeforeWrite = func(*admitWaiter) { _ = clientConn.Close() }
			} else {
				server.admitWriteFrame = func(net.Conn, any) error { return io.ErrUnexpectedEOF }
			}
			done := make(chan struct{})
			go func() {
				defer close(done)
				defer serverConn.Close()
				server.admitConnection(serverConn, validAdmitArgs(50, 1000))
			}()
			select {
			case <-done:
			case <-time.After(time.Second):
				t.Fatal("admit handler did not release after failed write")
			}
			server.admitRegistryMu.Lock()
			if len(server.admitQueues) != 0 {
				t.Fatalf("reservation leaked: queues=%d", len(server.admitQueues))
			}
			server.admitRegistryMu.Unlock()
		})
	}
}

func TestAdmitSaturationRejectUsesUnequalDeadlines(t *testing.T) {
	var maximum atomic.Int64
	maximum.Store(10)
	server := admitTestServer(&maximum)
	server.admitReadMemory = func(string) (int64, int64, int64, bool, string) { return 10, 10, 0, true, "" }
	aServer, aClient := net.Pipe()
	bServer, bClient := net.Pipe()
	defer aClient.Close()
	defer bClient.Close()
	aDone, bDone := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(aDone)
		defer aServer.Close()
		server.admitConnection(aServer, validAdmitArgs(10, 200))
	}()
	time.Sleep(time.Millisecond)
	go func() {
		defer close(bDone)
		defer bServer.Close()
		server.admitConnection(bServer, validAdmitArgs(10, 10))
	}()
	var bFrame ResponseFrame
	if err := readFrame(bClient, &bFrame); err != nil {
		t.Fatal(err)
	}
	if bFrame.Code != CodeAdmitSaturated {
		t.Fatalf("short waiter frame=%+v", bFrame)
	}
	_ = bClient.Close()
	_ = aClient.SetReadDeadline(time.Now().Add(20 * time.Millisecond))
	var early ResponseFrame
	if err := readFrame(aClient, &early); err == nil {
		t.Fatal("long waiter received a frame before its own deadline")
	}
	_ = aClient.SetReadDeadline(time.Time{})
	var aFrame ResponseFrame
	if err := readFrame(aClient, &aFrame); err != nil {
		t.Fatal(err)
	}
	if aFrame.Code != CodeAdmitSaturated {
		t.Fatalf("long waiter frame=%+v", aFrame)
	}
	_ = aClient.Close()
	<-aDone
	<-bDone
}

func TestAdmitOneShotGrantHandoffCannotDrop(t *testing.T) {
	var maximum atomic.Int64
	maximum.Store(1)
	server := admitTestServer(&maximum)
	queue, waiter := enqueueAdmitTest(t, server, 1)
	// Let the evaluator commit before this test starts receiving.
	time.Sleep(10 * time.Millisecond)
	waitAdmitGrant(t, waiter)
	server.releaseAdmitWaiter(queue, waiter)
}

func TestAdmitGrantTimeoutRaceCommitsExactlyOnce(t *testing.T) {
	for iteration := 0; iteration < 100; iteration++ {
		var maximum atomic.Int64
		maximum.Store(1)
		server := admitTestServer(&maximum)
		waiter := &admitWaiter{seq: 1, reserve: 1, state: admitQueued, grantedCh: make(chan struct{}), enqueued: time.Now()}
		queue := &sliceQueue{path: "/slice", server: server, waiters: []*admitWaiter{waiter}, kick: make(chan struct{}, 1)}
		var racers sync.WaitGroup
		racers.Add(2)
		go func() { defer racers.Done(); server.evaluateAdmitQueue(queue) }()
		go func() { defer racers.Done(); server.timeoutAdmitWaiter(queue, waiter) }()
		racers.Wait()
		waitAdmitGrant(t, waiter)
		queue.mu.Lock()
		if waiter.state != admitGranted && waiter.state != admitRejected || queue.outstanding != 0 && queue.outstanding != 1 {
			t.Fatalf("iteration %d state=%v outstanding=%d", iteration, waiter.state, queue.outstanding)
		}
		queue.mu.Unlock()
		server.releaseAdmitWaiter(queue, waiter)
		if queue.outstanding != 0 {
			t.Fatalf("iteration %d release outstanding=%d", iteration, queue.outstanding)
		}
	}
}

func TestAdmitPeerCloseFreesNextWithoutWaitingForPoll(t *testing.T) {
	var maximum atomic.Int64
	maximum.Store(100)
	server := admitTestServer(&maximum)
	aServer, aClient := net.Pipe()
	bServer, bClient := net.Pipe()
	aDone, bDone := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(aDone)
		defer aServer.Close()
		server.admitConnection(aServer, validAdmitArgs(100, 1000))
	}()
	var aFrame ResponseFrame
	if err := readFrame(aClient, &aFrame); err != nil {
		t.Fatal(err)
	}
	go func() {
		defer close(bDone)
		defer bServer.Close()
		server.admitConnection(bServer, validAdmitArgs(100, 1000))
	}()
	_ = bClient.SetReadDeadline(time.Now().Add(20 * time.Millisecond))
	var early ResponseFrame
	if err := readFrame(bClient, &early); err == nil {
		t.Fatal("second waiter granted before first peer close")
	}
	_ = bClient.SetReadDeadline(time.Time{})
	closedAt := time.Now()
	_ = aClient.Close()
	var bFrame ResponseFrame
	if err := readFrame(bClient, &bFrame); err != nil {
		t.Fatal(err)
	}
	if elapsed := time.Since(closedAt); elapsed >= defaultAdmitPollInterval {
		t.Fatalf("peer-close handoff=%v poll=%v", elapsed, defaultAdmitPollInterval)
	}
	_ = bClient.Close()
	<-aDone
	<-bDone
}

func TestAdmitGrantCommittedAtShutdownDeliveredOrReleasedOnce(t *testing.T) {
	var maximum atomic.Int64
	maximum.Store(1)
	server := admitTestServer(&maximum)
	var once sync.Once
	server.admitBeforeWrite = func(*admitWaiter) { once.Do(func() { close(server.stopping) }) }
	serverConn, clientConn := net.Pipe()
	done := make(chan struct{})
	go func() {
		defer close(done)
		defer serverConn.Close()
		server.admitConnection(serverConn, validAdmitArgs(1, 1000))
	}()
	var frame ResponseFrame
	if err := readFrame(clientConn, &frame); err != nil {
		t.Fatal(err)
	}
	if grant := admitGrantData(t, frame); grant.State != "immediate" {
		t.Fatalf("grant=%+v", grant)
	}
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("shutdown did not release committed grant")
	}
	_ = clientConn.Close()
	server.pruneAdmitRegistry()
	server.admitRegistryMu.Lock()
	if len(server.admitQueues) != 0 {
		t.Fatalf("shutdown left %d queues", len(server.admitQueues))
	}
	server.admitRegistryMu.Unlock()
}

func TestAdmitShutdownHandlerOwnsReleaseAndPrunesAfterDrain(t *testing.T) {
	var maximum atomic.Int64
	maximum.Store(10)
	server := admitTestServer(&maximum)
	server.admitReadMemory = func(string) (int64, int64, int64, bool, string) { return 10, 10, 0, true, "" }
	serverConn, clientConn := net.Pipe()
	done := make(chan struct{})
	go func() {
		defer close(done)
		defer serverConn.Close()
		server.admitConnection(serverConn, validAdmitArgs(10, 1000))
	}()
	time.Sleep(10 * time.Millisecond)
	close(server.stopping)
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("shutdown did not cancel queued handler")
	}
	var frame ResponseFrame
	if err := readFrame(clientConn, &frame); !errors.Is(err, io.EOF) {
		t.Fatalf("queued client read=%v, want EOF", err)
	}
	_ = clientConn.Close()
	server.pruneAdmitRegistry()
	server.admitRegistryMu.Lock()
	if len(server.admitQueues) != 0 {
		t.Fatalf("shutdown registry=%d", len(server.admitQueues))
	}
	server.admitRegistryMu.Unlock()
}

func TestAdmitUnevaluatedImmediateWithoutEnqueue(t *testing.T) {
	var maximum atomic.Int64
	server := admitTestServer(&maximum)
	server.admitResolveSlice = func(string) (string, bool, string) { return "", false, "slice-not-found" }
	serverConn, clientConn := net.Pipe()
	done := make(chan struct{})
	go func() {
		defer close(done)
		defer serverConn.Close()
		server.admitConnection(serverConn, validAdmitArgs(1, 100))
	}()
	var frame ResponseFrame
	if err := readFrame(clientConn, &frame); err != nil {
		t.Fatal(err)
	}
	grant := admitGrantData(t, frame)
	if grant.State != "unevaluated" || grant.Reason != "slice-not-found" {
		t.Fatalf("grant=%+v", grant)
	}
	<-done
	server.admitRegistryMu.Lock()
	if len(server.admitQueues) != 0 {
		t.Fatalf("unevaluated enqueued: %d", len(server.admitQueues))
	}
	server.admitRegistryMu.Unlock()
}

func TestAdmitValidationAndCaps(t *testing.T) {
	if request, err := validateAdmitArgs(map[string]any{"slice": " x ", "reserve": int64(1), "max_wait_ms": admitWaitCapMs + 1, "signature": "a\x00b", "pinned": true}); err != nil || request.reserve != 1 || request.maxWait != admitWaitCapMs || request.signature != "a\x00b" || !request.pinned {
		t.Fatalf("valid/clamped request=%+v err=%v", request, err)
	}
	for name, args := range map[string]map[string]any{
		"missing":         {"slice": "x", "reserve": 1},
		"negative":        {"slice": "x", "reserve": -1, "max_wait_ms": 1},
		"reserve ceiling": {"slice": "x", "reserve": admitMaxReserve + 1, "max_wait_ms": 1},
		"fractional":      {"slice": "x", "reserve": 1.5, "max_wait_ms": 1},
		"extra":           {"slice": "x", "reserve": 1, "max_wait_ms": 1, "extra": true},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := validateAdmitArgs(args); err == nil {
				t.Fatal("hostile request accepted")
			}
		})
	}

	var maximum atomic.Int64
	server := admitTestServer(&maximum)
	queue, first := enqueueAdmitTest(t, server, 1)
	for index := 1; index < admitMaxWaiters; index++ {
		enqueueAdmitTest(t, server, 1)
	}
	if _, _, code, err := server.enqueueAdmit("/slice", 1); err == nil || code != CodeBusy {
		t.Fatalf("per-slice cap code=%q err=%v", code, err)
	}
	for {
		queue.mu.Lock()
		if len(queue.waiters) == 0 {
			queue.mu.Unlock()
			break
		}
		waiter := queue.waiters[0]
		queue.mu.Unlock()
		server.releaseAdmitWaiter(queue, waiter)
	}
	_ = first

	for index := 0; index < admitGlobalMax; index++ {
		server.admitSlots <- struct{}{}
	}
	serverConn, clientConn := net.Pipe()
	done := make(chan struct{})
	go func() {
		defer close(done)
		defer serverConn.Close()
		server.admitConnection(serverConn, validAdmitArgs(1, 1))
	}()
	var frame ResponseFrame
	if err := readFrame(clientConn, &frame); err != nil || frame.Code != CodeBusy {
		t.Fatalf("global cap frame=%+v err=%v", frame, err)
	}
	<-done
	for index := 0; index < admitGlobalMax; index++ {
		<-server.admitSlots
	}
}

func TestCheckedAvailableClampsWithoutOverflow(t *testing.T) {
	for _, test := range []struct {
		current, maximum, outstanding, headroom, want int64
	}{
		{0, 100, 25, 0, 75}, {20, 100, 25, 10, 65}, {80, 100, 25, 10, 10},
		{100, 100, 0, 0, 0}, {101, 100, 0, 0, 0},
		{0, math.MaxInt64, math.MaxInt64, 0, 0}, {-1, math.MaxInt64, 0, 0, 0},
	} {
		if got := checkedAvailable(test.current, test.maximum, 0, test.outstanding, test.headroom); got != test.want {
			t.Fatalf("checkedAvailable(%d,%d,%d,%d,%d)=%d want=%d", test.current, test.maximum, 0, test.outstanding, test.headroom, got, test.want)
		}
	}
}

// verifies: Slice 1 relies on the honest effective-current floor, not the
// porous outstanding>=current assertion. In particular, outstanding may be
// below current after a worker grows, but checkedAvailable still never exposes
// headroom once effectiveCurrent reaches the cap-minus-headroom ceiling.
func TestCheckedAvailablePreservesEffectiveCurrentFloorBelowOutstanding(t *testing.T) {
	for _, outstanding := range []int64{0, 1, 50} {
		if got := checkedAvailable(100, 100, 1, outstanding, 1); got != 0 {
			t.Fatalf("outstanding=%d available=%d, want zero: effectiveCurrent reaches the ceiling", outstanding, got)
		}
	}
}

// verifies: reduced per-worker pads stop their sum from falsely gating a
// small waiter while the honest effective current remains well below the cap.
// The old rss+512 MiB grants would leave no availability in this same queue.
func TestEvaluateAdmitQueueReducedPerWorkerPadAvoidsFalseBlock(t *testing.T) {
	var maximum atomic.Int64
	maximum.Store(1 << 30)
	server := admitTestServer(&maximum)
	server.admitReadMemory = func(string) (int64, int64, int64, bool, string) {
		return 200 << 20, maximum.Load(), 0, true, ""
	}
	rss := int64(1 << 20)
	workers := int64(4)
	reducedOutstanding := workers * (rss + (128 << 20))
	oldOutstanding := workers * (rss + (512 << 20))
	small := int64(64 << 20)
	if oldAvailable := checkedAvailable(200<<20, maximum.Load(), 0, oldOutstanding, 0); oldAvailable >= small {
		t.Fatalf("old 512MiB-pad grants unexpectedly admit: available=%d reserve=%d", oldAvailable, small)
	}
	waiter := &admitWaiter{seq: 1, reserve: small, state: admitQueued, grantedCh: make(chan struct{}), enqueued: time.Now()}
	queue := &sliceQueue{path: "/slice", server: server, outstanding: reducedOutstanding, outstandingJobs: int(workers), waiters: []*admitWaiter{waiter}}
	server.evaluateAdmitQueue(queue)
	waitAdmitGrant(t, waiter)
}

func TestAdmitAvailableMatchesGrantHeadroomWithoutCreatingQueue(t *testing.T) {
	// Revert-check: omitting the +1 prospective job term gives 46 rather than
	// the grant-time 44 here; creating a missing queue changes the registry.
	server := &Server{
		admitQueues:                  map[string]*sliceQueue{},
		admitSliceHeadroomBase:       10,
		admitSliceHeadroomSupervisor: 2,
		admitReadMemory: func(string) (int64, int64, int64, bool, string) {
			return 20, 100, 0, true, ""
		},
	}
	queue := &sliceQueue{outstanding: 30, outstandingJobs: 1, adopted: 10, adoptedJobs: 1}
	server.admitQueues["/slice"] = queue
	if available, ok := server.admitAvailable("/slice"); !ok || available != 44 {
		t.Fatalf("available=%d ok=%v, want 44 true", available, ok)
	}
	if available, ok := server.admitAvailable("/missing"); !ok || available != 68 {
		t.Fatalf("missing queue available=%d ok=%v, want 68 true", available, ok)
	}
	if len(server.admitQueues) != 1 || server.admitQueues["/missing"] != nil {
		t.Fatalf("read-only lookup created a queue: %#v", server.admitQueues)
	}
	server.admitReadMemory = func(string) (int64, int64, int64, bool, string) { return 0, 0, 0, false, "read-error" }
	if _, ok := server.admitAvailable("/slice"); ok {
		t.Fatal("unreadable or uncapped slice was not reported uncertain")
	}
}

func TestReleaseAdmitWaiterOnlySignalsGovernor(t *testing.T) {
	// Revert-check: a synchronous evaluate here would activate the parked
	// worker below. Keeping this path signal-only avoids queue.mu ->
	// governorSet.mu -> sliceQueue.mu lock inversion.
	g := testGovernor(1, governorEnforce)
	parked := putGovernorWorker(g, "parked", "job", time.Now(), 1, governorParked)
	server := &Server{admitQueues: map[string]*sliceQueue{}, governor: g}
	queue := &sliceQueue{path: "/slice", waiters: []*admitWaiter{{state: admitGranted, accounted: true, reserve: 1}}, outstanding: 1, outstandingJobs: 1}
	server.releaseAdmitWaiter(queue, queue.waiters[0])
	if parked.state != governorParked {
		t.Fatal("admission release synchronously evaluated governor")
	}
	select {
	case <-g.kick:
	default:
		t.Fatal("admission release did not signal governor")
	}
}

func validAdmitArgs(reserve, wait int64) map[string]any {
	return map[string]any{"slice": "slice", "reserve": reserve, "max_wait_ms": wait, "signature": "", "pinned": true}
}

func admitGrantData(t *testing.T, frame ResponseFrame) AdmitResponse {
	t.Helper()
	if !frame.OK || frame.Code != "OK" {
		t.Fatalf("admit frame=%+v", frame)
	}
	var grant AdmitResponse
	if err := json.Unmarshal(frame.Data, &grant); err != nil {
		t.Fatal(err)
	}
	return grant
}

func TestAdmitServeConnectionInterceptsClientOnlyVerb(t *testing.T) {
	var maximum atomic.Int64
	maximum.Store(10)
	server := admitTestServer(&maximum)
	serverConn, clientConn := net.Pipe()
	done := make(chan struct{})
	go func() { defer close(done); server.serveConnection(context.Background(), serverConn) }()
	if err := writeFrame(clientConn, RequestFrame{Proto: ProtocolVersion, Request: coreRequestAdmit(validAdmitArgs(1, 100))}); err != nil {
		t.Fatal(err)
	}
	var frame ResponseFrame
	if err := readFrame(clientConn, &frame); err != nil {
		t.Fatal(err)
	}
	if grant := admitGrantData(t, frame); grant.State != "immediate" {
		t.Fatalf("state=%q", grant.State)
	}
	_ = clientConn.Close()
	<-done
}

func coreRequestAdmit(args map[string]any) core.Request {
	return core.Request{Verb: "admit", Args: args}
}

// verifies: grantedAt records the moment the daemon GRANTS a lease, never the
// moment the request first arrived in the queue. A waiter that sits in the
// admission queue under contention and is granted much later must show
// grantedAt at the grant moment; reading enqueued instead (AIRA-49's v3
// defect, found independently by Sol and Fable) makes ordinary
// admission-queue contention look identical to launch abandonment to the
// stale-lease sweep that consumes this field.
func TestGrantSetsGrantedAtDistinctFromEnqueuedUnderQueueingDelay(t *testing.T) {
	var maximum atomic.Int64
	maximum.Store(100)
	server := admitTestServer(&maximum)
	enqueuedAt := time.Unix(5000, 0)
	now := enqueuedAt
	server.admitNow = func() time.Time { return now }
	server.admitBackfillGrace = 0
	current := int64(90)
	server.admitReadMemory = func(string) (int64, int64, int64, bool, string) {
		return current, maximum.Load(), 0, true, ""
	}
	waiter := &admitWaiter{seq: 1, reserve: 60, state: admitQueued, grantedCh: make(chan struct{}), enqueued: enqueuedAt}
	queue := &sliceQueue{path: "/slice", server: server, waiters: []*admitWaiter{waiter}}

	// Contention: the slice has no room, so the waiter stays queued and no
	// grant moment exists yet.
	server.evaluateAdmitQueue(queue)
	requireAdmitQueued(t, waiter)
	if !waiter.grantedAt.IsZero() {
		t.Fatalf("grantedAt=%v recorded while still queued, want the zero value", waiter.grantedAt)
	}

	// The queueing delay elapses, then room appears and the grant fires.
	const queueingDelay = 17 * time.Minute
	now = enqueuedAt.Add(queueingDelay)
	current = 0
	server.evaluateAdmitQueue(queue)
	waitAdmitGrant(t, waiter)

	if !waiter.grantedAt.Equal(now) {
		t.Fatalf("grantedAt=%v, want the grant moment %v", waiter.grantedAt, now)
	}
	if !waiter.enqueued.Equal(enqueuedAt) {
		t.Fatalf("enqueued=%v moved, want the original arrival %v", waiter.enqueued, enqueuedAt)
	}
	if delay := waiter.grantedAt.Sub(waiter.enqueued); delay != queueingDelay {
		t.Fatalf("grantedAt-enqueued=%s, want the simulated queueing delay %s", delay, queueingDelay)
	}
}
