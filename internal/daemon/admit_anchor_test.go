package daemon

import (
	"net"
	"os"
	"sync/atomic"
	"testing"
	"time"

	"aira/internal/testdeadline"
)

// S8 — admitWaiter anchor state, the idempotent SET-by-scope-id re-anchor, and
// compare-and-release (design §3 Inv 4, §4). These tests drive the machinery
// directly (the existing enqueue path) rather than through the ARDR frame handler
// (S9); they force the reconnect-race lock orderings by explicit call sequence, not
// by timing. Compare-and-release keys on the live net.Conn identity (not a monotone
// generation), so a superseded connection's EOF releases nothing — the race has no
// capture window.

// closedCh returns an already-closed channel, the grantedCh a lease carries once it
// has been granted.
func closedCh() chan struct{} {
	ch := make(chan struct{})
	close(ch)
	return ch
}

// testAnchorConn returns a distinct net.Conn usable as a lease anchor identity. Each
// call returns a fresh connection object, so two anchors never compare equal. The
// pipe holds no OS resource and spawns no goroutine; the peer end is discarded.
func testAnchorConn() net.Conn {
	c, _ := net.Pipe()
	return c
}

// grantedAnchorLease registers a queue holding one GRANTED, accounted lease for
// scopeID, anchored to connA — exactly the state a fresh admitConnection leaves
// behind after its grant. It returns the queue, the lease, and the anchoring
// connection so a test can re-declare (SET) and release against them.
func grantedAnchorLease(server *Server, scopeID string, reserve int64) (*sliceQueue, *admitWaiter, net.Conn) {
	connA := testAnchorConn()
	lease := &admitWaiter{
		seq: 1, reserve: reserve, cpu: 1, state: admitGranted, accounted: true,
		grantedCh: closedCh(), scopeID: scopeID, basis: "pinned:client",
		anchor: connA,
	}
	queue := &sliceQueue{
		path: "/slice", server: server, kick: make(chan struct{}, 1), stop: make(chan struct{}),
		waiters:     []*admitWaiter{lease},
		outstanding: reserve, cpuOutstanding: lease.cpu, outstandingJobs: 1,
	}
	registerAdmitQueue(server, queue)
	return queue, lease, connA
}

func anchorTestServer() *Server {
	server := NewServer(Paths{})
	server.admitSliceHeadroomBase = 0
	server.admitSliceHeadroomSupervisor = 0
	return server
}

func queueLedger(queue *sliceQueue) (outstanding, cpu int64, jobs int) {
	queue.mu.Lock()
	defer queue.mu.Unlock()
	return queue.outstanding, queue.cpuOutstanding, queue.outstandingJobs
}

// TestReanchorSETRefreshesVectorAndReanchors pins the idempotent SET: a re-declare of
// an existing GRANTED lease re-anchors it in place (same waiter, one live lease) to
// the new connection and refreshes the resource vector in the ledger. It also pins the
// reorder — a re-anchor is accepted past the headroom ceiling (Inv 6), which a pre-SET
// fresh-admission path would have refused.
func TestReanchorSETRefreshesVectorAndReanchors(t *testing.T) {
	server := anchorTestServer()
	queue, lease, connA := grantedAnchorLease(server, "CONFINE-job-1-aaaa", 2*gib)
	connB := testAnchorConn()

	const newReserve = 5 * gib
	gotQueue, got, code, err := server.enqueueResolvedConfineAdmit("/slice", newReserve, "pinned:client", 4*gib /* < newReserve: a fresh admit would be refused */, admitRequest{
		scopeID: "CONFINE-job-1-aaaa", name: "job", owner: "session-a", cpu: 3, peerSameUID: true, conn: connB,
	})
	if err != nil || code != "" {
		t.Fatalf("re-declare SET refused: code=%q err=%v", code, err)
	}
	if got != lease {
		t.Fatalf("SET returned a new waiter %p, not the existing lease %p — it inserted instead of re-anchoring", got, lease)
	}
	if gotQueue != queue {
		t.Fatalf("SET returned a different queue")
	}
	if lease.anchor != connB {
		t.Fatalf("re-anchor must point the anchor at the NEW connection, not %v", lease.anchor == connA)
	}
	if lease.reserve != newReserve || lease.cpu != 3 {
		t.Fatalf("SET must refresh the resource vector: reserve=%d cpu=%d", lease.reserve, lease.cpu)
	}
	if n := len(queue.waiters); n != 1 {
		t.Fatalf("re-anchor must NOT add a waiter: %d waiters", n)
	}
	if out, cpu, jobs := queueLedger(queue); out != newReserve || cpu != 3 || jobs != 1 {
		t.Fatalf("ledger must re-derive the refreshed vector: outstanding=%d cpu=%d jobs=%d", out, cpu, jobs)
	}
}

// TestReconnectRaceBothLockOrders is the reconnect-race interleaving pin. The two
// critical sections — a re-declare's re-anchoring SET and the stale old connection's
// release — both take queue.mu; the test forces BOTH orders by explicit call sequence
// (deterministic, not timing) and asserts the invariant in each: exactly one live lease
// survives, and the stale connection never discharges the lease the re-declare holds.
func TestReconnectRaceBothLockOrders(t *testing.T) {
	t.Run("re-declare first, stale EOF no-ops", func(t *testing.T) {
		server := anchorTestServer()
		queue, lease, connA := grantedAnchorLease(server, "CONFINE-job-2-bbbb", 2*gib)
		connB := testAnchorConn()

		// Connection B re-declares: re-anchors the lease to connB.
		_, got, code, err := server.enqueueResolvedConfineAdmit("/slice", 2*gib, "pinned:client", 64*gib, admitRequest{
			scopeID: "CONFINE-job-2-bbbb", name: "job", owner: "session-a", cpu: 1, peerSameUID: true, conn: connB,
		})
		if err != nil || code != "" || got != lease || lease.anchor != connB {
			t.Fatalf("re-anchor failed: code=%q err=%v got==lease=%v anchored-to-B=%v", code, err, got == lease, lease.anchor == connB)
		}

		// Connection A's stale EOF arrives AFTER the re-declare: it must release NOTHING
		// (A is no longer the anchor), leaving the re-anchored lease intact.
		server.releaseAdmitWaiterAnchored(queue, lease, connA)
		if lease.state != admitGranted {
			t.Fatalf("stale EOF discharged a re-anchored lease (state=%v) — compare-and-release failed", lease.state)
		}
		if n := len(queue.waiters); n != 1 {
			t.Fatalf("exactly one live lease must survive, got %d", n)
		}
		if out, _, jobs := queueLedger(queue); out != 2*gib || jobs != 1 {
			t.Fatalf("ledger perturbed by a stale no-op release: outstanding=%d jobs=%d", out, jobs)
		}

		// The current anchor's EOF (connB) then releases it.
		server.releaseAdmitWaiterAnchored(queue, lease, connB)
		if lease.state != admitReleased {
			t.Fatalf("the current-anchor EOF must release the lease, state=%v", lease.state)
		}
		if n := len(queue.waiters); n != 0 {
			t.Fatalf("lease not removed after its anchor EOF: %d waiters", n)
		}
	})

	t.Run("old EOF first, re-declare re-inserts fresh", func(t *testing.T) {
		server := anchorTestServer()
		queue, lease, connA := grantedAnchorLease(server, "CONFINE-job-3-cccc", 2*gib)
		// A filler lease keeps the queue from being pruned (and its evaluator untouched)
		// after the scope-3 lease is released, so the re-declare below lands in THIS
		// queue deterministically with no evaluator auto-granting.
		filler := &admitWaiter{
			seq: 2, reserve: gib, cpu: 1, state: admitGranted, accounted: true,
			grantedCh: closedCh(), scopeID: "CONFINE-filler-9-dddd", anchor: testAnchorConn(),
		}
		queue.mu.Lock()
		queue.waiters = append(queue.waiters, filler)
		queue.outstanding, queue.cpuOutstanding, queue.outstandingJobs = rederiveLedgerLocked(queue)
		queue.mu.Unlock()

		// Connection A's EOF arrives BEFORE any re-declare: A is still the current
		// anchor, so it legitimately releases the lease.
		server.releaseAdmitWaiterAnchored(queue, lease, connA)
		if lease.state != admitReleased {
			t.Fatalf("A's EOF (the current anchor) must release the lease, state=%v", lease.state)
		}

		// Connection B then re-declares the SAME scope. The lease is gone, so the SET
		// finds nothing to re-anchor and inserts a FRESH queued waiter — it must NEVER
		// re-anchor the released lease object.
		_, got, code, err := server.enqueueResolvedConfineAdmit("/slice", 2*gib, "pinned:client", 64*gib, admitRequest{
			scopeID: "CONFINE-job-3-cccc", name: "job", owner: "session-a", cpu: 1, peerSameUID: true, conn: testAnchorConn(),
		})
		if err != nil || code != "" {
			t.Fatalf("re-declare after release must insert fresh, not refuse: code=%q err=%v", code, err)
		}
		if got == lease {
			t.Fatalf("SET re-anchored a RELEASED lease object — insert-if-absent was bypassed")
		}
		if got.state != admitQueued {
			t.Fatalf("a fresh insert must be queued (awaiting grant), state=%v", got.state)
		}
		// Exactly one live lease for the scope (the fresh one), plus the filler.
		if n := leaseByScopeIDLocked(queue, "CONFINE-job-3-cccc"); n != got {
			t.Fatalf("exactly one live lease must key the scope")
		}
	})
}

// TestReanchorThenStaleReleaseAcrossThreeConnections is the lost-lease-race pin that the
// monotone-generation design could NOT express. With a generation, the releasing
// connection captured "its" generation in a critical section separate from the SET that
// bumped it, so a connection that had been superseded could observe a later generation
// and discharge a live lease. Conn-identity compare-and-release has no capture: connA,
// then B re-declares, then C re-declares. Both A's and B's later EOFs must be no-ops (C
// is the anchor); only C's EOF discharges. Dropping the anchor compare reds here.
func TestReanchorThenStaleReleaseAcrossThreeConnections(t *testing.T) {
	server := anchorTestServer()
	queue, lease, connA := grantedAnchorLease(server, "CONFINE-job-9-jjjj", 2*gib)
	connB := testAnchorConn()
	connC := testAnchorConn()

	reDeclare := func(conn net.Conn) {
		t.Helper()
		_, got, code, err := server.enqueueResolvedConfineAdmit("/slice", 2*gib, "pinned:client", 64*gib, admitRequest{
			scopeID: "CONFINE-job-9-jjjj", name: "job", owner: "session-a", cpu: 1, peerSameUID: true, conn: conn,
		})
		if err != nil || code != "" || got != lease {
			t.Fatalf("re-declare failed: code=%q err=%v got==lease=%v", code, err, got == lease)
		}
	}
	reDeclare(connB) // anchor: connA -> connB
	reDeclare(connC) // anchor: connB -> connC
	if lease.anchor != connC {
		t.Fatalf("the last re-declare must be the anchor")
	}

	// connA's EOF (superseded twice) — no-op.
	server.releaseAdmitWaiterAnchored(queue, lease, connA)
	// connB's EOF (superseded by C) — MUST be a no-op. The generation design's bug: if B
	// had captured the generation AFTER C's SET, B's value would equal C's and B would
	// discharge C's live lease. Conn identity has no such capture window.
	server.releaseAdmitWaiterAnchored(queue, lease, connB)
	if lease.state != admitGranted {
		t.Fatalf("a superseded connection's EOF discharged the live lease (state=%v) — compare-and-release lost a lease", lease.state)
	}
	if out, _, jobs := queueLedger(queue); out != 2*gib || jobs != 1 {
		t.Fatalf("ledger perturbed by a superseded no-op release: outstanding=%d jobs=%d", out, jobs)
	}

	// Only connC, the current anchor, releases it.
	server.releaseAdmitWaiterAnchored(queue, lease, connC)
	if lease.state != admitReleased {
		t.Fatalf("the current-anchor EOF must release the lease, state=%v", lease.state)
	}
	if n := len(queue.waiters); n != 0 {
		t.Fatalf("lease not removed after its anchor EOF: %d waiters", n)
	}
}

// TestSETGatesOnGrantedNotRejected is the mutation pin for the admitGranted gate: a
// lease that has been REJECTED (its deferred release not yet run) is still
// `!= admitReleased`, so leaseByScopeIDLocked finds it — but the SET must REFUSE, never
// re-anchor a dying lease. Flipping the gate to `!= admitReleased` reds here.
func TestSETGatesOnGrantedNotRejected(t *testing.T) {
	server := anchorTestServer()
	connR := testAnchorConn()
	// A rejected-but-not-yet-removed lease: still in queue.waiters, state rejected.
	rejected := &admitWaiter{
		seq: 1, reserve: 2 * gib, cpu: 1, state: admitRejected,
		grantedCh: closedCh(), scopeID: "CONFINE-job-4-eeee", anchor: connR,
	}
	queue := &sliceQueue{
		path: "/slice", server: server, kick: make(chan struct{}, 1), stop: make(chan struct{}),
		waiters: []*admitWaiter{rejected},
	}
	registerAdmitQueue(server, queue)

	_, got, code, err := server.enqueueResolvedConfineAdmit("/slice", 2*gib, "pinned:client", 64*gib, admitRequest{
		scopeID: "CONFINE-job-4-eeee", name: "job", owner: "session-a", cpu: 1, peerSameUID: true, conn: testAnchorConn(),
	})
	if err == nil || code != CodeProtocol {
		t.Fatalf("re-declare onto a REJECTED lease must be refused with %s, got code=%q err=%v", CodeProtocol, code, err)
	}
	if got != nil {
		t.Fatalf("a refused SET must return no waiter")
	}
	if rejected.anchor != connR {
		t.Fatalf("a refused SET must NOT re-anchor the rejected lease")
	}
}

// TestReDeclareExclusiveRefused pins P2-D: an exclusive lease is lost on reconnect and
// is never re-declared, so a re-declare carrying exclusive is a protocol violation —
// refused rather than silently re-anchored as a non-exclusive lease. Dropping the
// explicit exclusive refuse on the SET path reds here (the SET would return the lease
// re-anchored, err nil).
func TestReDeclareExclusiveRefused(t *testing.T) {
	server := anchorTestServer()
	queue, lease, connA := grantedAnchorLease(server, "CONFINE-job-11-llll", 2*gib)

	_, got, code, err := server.enqueueResolvedConfineAdmit("/slice", 2*gib, "pinned:client", 64*gib, admitRequest{
		scopeID: "CONFINE-job-11-llll", name: "job", owner: "session-a", cpu: 1, peerSameUID: true, exclusive: true, conn: testAnchorConn(),
	})
	if err == nil || code != CodeProtocol {
		t.Fatalf("an exclusive re-declare must be refused with %s, got code=%q err=%v", CodeProtocol, code, err)
	}
	if got != nil {
		t.Fatalf("a refused exclusive re-declare must return no waiter")
	}
	if lease.anchor != connA {
		t.Fatalf("a refused exclusive re-declare must leave the lease anchored to A")
	}
	if n := len(queue.waiters); n != 1 {
		t.Fatalf("a refused exclusive re-declare must not add a waiter: %d", n)
	}
}

// TestReDeclareSameUIDGate pins the SO_PEERCRED same-uid gate on the re-anchoring SET
// (design §4 P2-C): an other-uid peer is refused and the lease is untouched; a same-uid
// peer re-anchors. No cgroup-membership check is involved.
func TestReDeclareSameUIDGate(t *testing.T) {
	t.Run("other uid refused", func(t *testing.T) {
		server := anchorTestServer()
		queue, lease, connA := grantedAnchorLease(server, "CONFINE-job-5-ffff", 2*gib)
		_, got, code, err := server.enqueueResolvedConfineAdmit("/slice", 3*gib, "pinned:client", 64*gib, admitRequest{
			scopeID: "CONFINE-job-5-ffff", name: "job", owner: "session-a", cpu: 1, peerSameUID: false, conn: testAnchorConn(),
		})
		if err == nil || code != CodeProtocol {
			t.Fatalf("an other-uid re-declare must be refused with %s, got code=%q err=%v", CodeProtocol, code, err)
		}
		if got != nil {
			t.Fatalf("a refused re-declare must return no waiter")
		}
		if lease.anchor != connA || lease.reserve != 2*gib {
			t.Fatalf("a refused re-declare must leave the lease untouched: anchored-to-A=%v reserve=%d", lease.anchor == connA, lease.reserve)
		}
		if n := len(queue.waiters); n != 1 {
			t.Fatalf("a refused re-declare must not add a waiter: %d", n)
		}
	})
	t.Run("same uid re-anchors", func(t *testing.T) {
		server := anchorTestServer()
		_, lease, _ := grantedAnchorLease(server, "CONFINE-job-6-gggg", 2*gib)
		connB := testAnchorConn()
		_, got, code, err := server.enqueueResolvedConfineAdmit("/slice", 3*gib, "pinned:client", 64*gib, admitRequest{
			scopeID: "CONFINE-job-6-gggg", name: "job", owner: "session-a", cpu: 1, peerSameUID: true, conn: connB,
		})
		if err != nil || code != "" || got != lease || lease.anchor != connB {
			t.Fatalf("a same-uid re-declare must re-anchor: code=%q err=%v got==lease=%v anchored-to-B=%v", code, err, got == lease, lease.anchor == connB)
		}
	})
}

// waitClosed blocks until ch closes or the test deadline elapses.
func waitClosed(t *testing.T, ch <-chan struct{}, what string) {
	t.Helper()
	select {
	case <-ch:
	case <-testdeadline.After(5 * time.Second):
		t.Fatalf("timed out waiting for %s", what)
	}
}

// TestAdmitConnectionReDeclareReleaseLifecycle drives the whole machinery through
// admitConnection over real net.Pipe connections: connection A admits a scope and is
// granted; connection B re-declares the SAME scope and is granted (the SET re-anchors,
// proving admitConnection routes a dup-scope admit to the SET rather than the old
// CodeProtocol refusal). Closing A — now the STALE connection — must release NOTHING
// (it caught the wrong release variant: the unconditional release would discharge B's
// lease). Closing B then releases it. The doneA/doneB channels synchronise on each
// handler having run its release, so the assertions are deterministic rather than timed.
func TestAdmitConnectionReDeclareReleaseLifecycle(t *testing.T) {
	var maximum atomic.Int64
	maximum.Store(64 * gib)
	server := admitTestServer(&maximum)
	// Same-uid peer through the SO_PEERCRED seam (as supervisor_lease_test does), so B's
	// re-declare passes the same-uid gate.
	server.peerCredential = func(net.Conn) (int, int, error) { return os.Geteuid(), os.Getpid(), nil }

	args := func() map[string]any {
		return map[string]any{
			"slice": "slice", "reserve": 2 * gib, "max_wait_ms": int64(30 * time.Second / time.Millisecond),
			"signature": "", "pinned": true,
			"scope_id": "CONFINE-job-7-hhhh", "name": "job", "owner": "session-a",
		}
	}

	admit := func() (net.Conn, chan struct{}) {
		serverConn, clientConn := net.Pipe()
		done := make(chan struct{})
		go func() { defer close(done); server.admitConnection(serverConn, args()) }()
		var frame ResponseFrame
		if err := readFrame(clientConn, &frame); err != nil {
			t.Fatalf("reading grant frame: %v", err)
		}
		admitGrantData(t, frame) // asserts OK/Code=="OK"
		return clientConn, done
	}

	connA, doneA := admit() // fresh admit, granted (anchored to A)
	connB, doneB := admit() // re-declare SAME scope, re-anchored + granted (anchored to B)

	server.admitRegistryMu.Lock()
	queue := server.admitQueues["/slice"]
	server.admitRegistryMu.Unlock()
	if queue == nil {
		t.Fatal("no admit queue after two admits")
	}
	if _, _, jobs := queueLedger(queue); jobs != 1 {
		t.Fatalf("two connections on one scope must be ONE lease, jobs=%d", jobs)
	}

	// Close A — the STALE connection. Its release must be a no-op.
	_ = connA.Close()
	waitClosed(t, doneA, "connection A handler to return")
	if _, _, jobs := queueLedger(queue); jobs != 1 {
		t.Fatalf("closing the STALE connection discharged the lease (jobs=%d) — admitConnection used the unconditional release, not compare-and-release", jobs)
	}

	// Close B — the current anchor. Its release discharges the lease.
	_ = connB.Close()
	waitClosed(t, doneB, "connection B handler to return")
	if _, _, jobs := queueLedger(queue); jobs != 0 {
		t.Fatalf("closing the current-anchor connection must release the lease, jobs=%d", jobs)
	}
}

// TestAdmitConnectionReDeclareOtherUIDRefused exercises the same-uid gate through the
// REAL SO_PEERCRED seam (not a hand-set peerSameUID): with an other-uid peer, the fresh
// admit A still succeeds (a fresh admit has no uid gate — behaviour preserved), but B's
// re-declare of the SAME scope is REFUSED with CodeProtocol and leaves A's lease
// untouched. This is the different-uid fail direction of the one honesty gate this slice
// adds: a build that hardcoded peerSameUID=true in admitConnection survives every other
// test but reds here.
func TestAdmitConnectionReDeclareOtherUIDRefused(t *testing.T) {
	var maximum atomic.Int64
	maximum.Store(64 * gib)
	server := admitTestServer(&maximum)
	server.peerCredential = func(net.Conn) (int, int, error) { return os.Geteuid() + 1, os.Getpid(), nil }

	args := map[string]any{
		"slice": "slice", "reserve": 2 * gib, "max_wait_ms": int64(30 * time.Second / time.Millisecond),
		"signature": "", "pinned": true,
		"scope_id": "CONFINE-job-8-iiii", "name": "job", "owner": "session-a",
	}

	// A: fresh admit from an other-uid peer — granted (no uid gate on a fresh admit).
	serverA, clientA := net.Pipe()
	doneA := make(chan struct{})
	go func() { defer close(doneA); server.admitConnection(serverA, args) }()
	var frameA ResponseFrame
	if err := readFrame(clientA, &frameA); err != nil {
		t.Fatalf("reading A's grant frame: %v", err)
	}
	admitGrantData(t, frameA)

	server.admitRegistryMu.Lock()
	queue := server.admitQueues["/slice"]
	server.admitRegistryMu.Unlock()
	if queue == nil {
		t.Fatal("no admit queue after A's admit")
	}

	// B: re-declare the SAME scope from the other-uid peer — REFUSED. Its handler returns
	// on the enqueue-error path before any release closure exists.
	serverB, clientB := net.Pipe()
	doneB := make(chan struct{})
	go func() { defer close(doneB); server.admitConnection(serverB, args) }()
	var frameB ResponseFrame
	if err := readFrame(clientB, &frameB); err != nil {
		t.Fatalf("reading B's refusal frame: %v", err)
	}
	if frameB.OK || frameB.Code != CodeProtocol {
		t.Fatalf("an other-uid re-declare must be refused with %s, got frame=%+v", CodeProtocol, frameB)
	}
	waitClosed(t, doneB, "connection B handler to return")
	if _, _, jobs := queueLedger(queue); jobs != 1 {
		t.Fatalf("a refused re-declare must leave A's lease intact, jobs=%d", jobs)
	}

	// A still owns the only lease; its EOF releases it.
	_ = clientA.Close()
	waitClosed(t, doneA, "connection A handler to return")
	if _, _, jobs := queueLedger(queue); jobs != 0 {
		t.Fatalf("closing A must release the lease, jobs=%d", jobs)
	}
}

// TestAdmitConnectionReDeclareUnreadableCredentialFailsClosed is the UNREADABLE-credential
// fail direction of the same-uid gate (the companion to TestAdmitConnectionReDeclareOtherUIDRefused,
// which covers a readable-but-different uid). With NO peerCredential seam, peerCredentialOf
// falls back to the real SO_PEERCRED, which ERRORS on a net.Pipe (not a unix socket), so
// peerSameUID must fail CLOSED (stay false) and the re-declare is refused. A build that set
// peerSameUID on a credential error, or defaulted it true, survives every seam-injected test
// but reds here.
func TestAdmitConnectionReDeclareUnreadableCredentialFailsClosed(t *testing.T) {
	var maximum atomic.Int64
	maximum.Store(64 * gib)
	server := admitTestServer(&maximum)
	// Deliberately NO server.peerCredential seam: the real unixPeerCredential runs and
	// errors on a net.Pipe.

	args := map[string]any{
		"slice": "slice", "reserve": 2 * gib, "max_wait_ms": int64(30 * time.Second / time.Millisecond),
		"signature": "", "pinned": true,
		"scope_id": "CONFINE-job-10-kkkk", "name": "job", "owner": "session-a",
	}

	// A: fresh admit — granted (a fresh admit has no uid gate, so an unreadable
	// credential does not block it).
	serverA, clientA := net.Pipe()
	doneA := make(chan struct{})
	go func() { defer close(doneA); server.admitConnection(serverA, args) }()
	var frameA ResponseFrame
	if err := readFrame(clientA, &frameA); err != nil {
		t.Fatalf("reading A's grant frame: %v", err)
	}
	admitGrantData(t, frameA)

	server.admitRegistryMu.Lock()
	queue := server.admitQueues["/slice"]
	server.admitRegistryMu.Unlock()
	if queue == nil {
		t.Fatal("no admit queue after A's admit")
	}

	// B: re-declare the SAME scope — the credential is unreadable, so peerSameUID is
	// false (fail-closed) and the SET's same-uid gate refuses with CodeProtocol.
	serverB, clientB := net.Pipe()
	doneB := make(chan struct{})
	go func() { defer close(doneB); server.admitConnection(serverB, args) }()
	var frameB ResponseFrame
	if err := readFrame(clientB, &frameB); err != nil {
		t.Fatalf("reading B's refusal frame: %v", err)
	}
	if frameB.OK || frameB.Code != CodeProtocol {
		t.Fatalf("an unreadable-credential re-declare must fail CLOSED and be refused with %s, got frame=%+v", CodeProtocol, frameB)
	}
	waitClosed(t, doneB, "connection B handler to return")
	if _, _, jobs := queueLedger(queue); jobs != 1 {
		t.Fatalf("a refused re-declare must leave A's lease intact, jobs=%d", jobs)
	}

	// A still owns the only lease; its EOF releases it.
	_ = clientA.Close()
	waitClosed(t, doneA, "connection A handler to return")
	if _, _, jobs := queueLedger(queue); jobs != 0 {
		t.Fatalf("closing A must release the lease, jobs=%d", jobs)
	}
}
