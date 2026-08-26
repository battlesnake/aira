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
	server.admitReadMemory = func(string) (int64, int64, bool, string) {
		return 0, maximum.Load(), true, ""
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
}

func TestAdmitPrefixConcurrencyAndNoJumpAhead(t *testing.T) {
	var maximum atomic.Int64
	maximum.Store(100)
	server := admitTestServer(&maximum)
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
	server.admitReadMemory = func(string) (int64, int64, bool, string) { return 10, 10, true, "" }
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
	server.admitReadMemory = func(string) (int64, int64, bool, string) { return 10, 10, true, "" }
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
		if got := checkedAvailable(test.current, test.maximum, test.outstanding, test.headroom); got != test.want {
			t.Fatalf("checkedAvailable(%d,%d,%d,%d)=%d want=%d", test.current, test.maximum, test.outstanding, test.headroom, got, test.want)
		}
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
