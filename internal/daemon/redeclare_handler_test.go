package daemon

import (
	"context"
	"errors"
	"io"
	"net"
	"os"
	"sync"
	"testing"
	"time"

	"aira/internal/testdeadline"
)

// S9 — the ARDR re-declare HANDLER (serveReDeclare): sniffed frame → resolve →
// SET-or-ESTABLISH the lease → 1-byte ack → HOLD the connection; released only by the
// connection's own EOF (design §3 compare-and-release, §4 daemon-restart). These tests
// drive the real handler over net.Pipe with injected SO_PEERCRED and slice seams — no
// S9 test may reach the real host slice resolver (the box itself runs under aira.slice).

// reDeclareTestServer builds a daemon with the seams a re-declare needs: a same-uid
// credential, a resolvable slice, a fail-closed memory reader (a re-declare never reads
// the ceiling — this also pins that a MUTANT routing an establish to the queued fresh
// insert cannot then be auto-granted, so Σleases stays 0 and the assertions red), no
// real confine scan, and a long poll so the evaluator does not churn.
func reDeclareTestServer() *Server {
	server := NewServer(Paths{StateID: "state"})
	server.stopping = make(chan struct{})
	server.admitPollInterval = time.Hour
	server.admitSliceHeadroomBase = 0
	server.admitSliceHeadroomSupervisor = 0
	server.admitResolveSlice = func(string) (string, bool, string) { return "/slice", true, "" }
	server.admitReadMemory = func(string) (int64, int64, int64, bool, string) {
		return 0, 0, 0, false, "no-memory (a re-declare skips the ceiling)"
	}
	server.peerCredential = func(net.Conn) (int, int, error) { return os.Geteuid(), os.Getpid(), nil }
	// These tests exercise the ledger/ack, not the S2a §16b worker teardown, and some
	// frames (the golden one: scope "child", parent "parent") have the worker-lease
	// shape that arms the peer-EOF kill. Stub it to a no-op so they do not invoke the
	// real cgroupfs helper against a synthetic scope id.
	server.workerScopeKill = func(context.Context, string, string) error { return nil }
	return server
}

// driveReDeclare feeds frame through serveConnection (so the magic sniff routes it) on a
// fresh net.Pipe and returns the client end and a done channel closed when the handler
// returns.
func driveReDeclare(t *testing.T, server *Server, frame []byte) (net.Conn, chan struct{}) {
	t.Helper()
	serverConn, clientConn := net.Pipe()
	done := make(chan struct{})
	go func() { defer close(done); server.serveConnection(context.Background(), serverConn) }()
	go func() { _, _ = clientConn.Write(frame) }()
	return clientConn, done
}

// readReDeclareAck asserts the frozen 1-byte ack is what the peer reads next.
func readReDeclareAck(t *testing.T, conn net.Conn) {
	t.Helper()
	var ack [1]byte
	if _, err := io.ReadFull(conn, ack[:]); err != nil {
		t.Fatalf("re-declare produced no ack: %v", err)
	}
	if ack[0] != reDeclareAckByte {
		t.Fatalf("re-declare ack = 0x%02x, want the frozen 0x%02x", ack[0], reDeclareAckByte)
	}
}

// reDeclareQueue returns the /slice admit queue, or nil if it was never built / was pruned.
func reDeclareQueue(server *Server) *sliceQueue {
	server.admitRegistryMu.Lock()
	defer server.admitRegistryMu.Unlock()
	return server.admitQueues["/slice"]
}

// withReDeclareLease runs fn against the one live lease for scopeID UNDER queue.mu, so
// field reads are race-clean against a concurrently held connection.
func withReDeclareLease(t *testing.T, server *Server, scopeID string, fn func(*admitWaiter)) {
	t.Helper()
	queue := reDeclareQueue(server)
	if queue == nil {
		t.Fatalf("no admit queue at /slice")
	}
	queue.mu.Lock()
	defer queue.mu.Unlock()
	w := leaseByScopeIDLocked(queue, scopeID)
	if w == nil {
		t.Fatalf("no live lease for scope %q", scopeID)
	}
	fn(w)
}

// TestReDeclareGateEstablishesLedgerFromGoldenFrame is the S9 GATE TEST (plan item 7):
// it drives the HAND-WRITTEN golden ARDR frame through serveConnection against a REAL
// ledger and asserts the LEDGER (not charge()) holds EXACTLY {scope:"child", ram:5 GiB,
// cpu:2, parent:"parent"} and Σleases moved by exactly that.
//
// MUTATION: the handler ignores parent_scope_id (establish sets parentScopeID="") → the
// parent assertion reds. A second, independent mutation — routing the absent-lease
// re-declare to the QUEUED fresh insert — reds the granted/Σ assertions here too (the
// fail-closed memory reader keeps the mutant's queued waiter ungranted, so Σ stays 0).
func TestReDeclareGateEstablishesLedgerFromGoldenFrame(t *testing.T) {
	server := reDeclareTestServer()
	clientConn, done := driveReDeclare(t, server, goldenReDeclareFrame)
	readReDeclareAck(t, clientConn)

	withReDeclareLease(t, server, "child", func(w *admitWaiter) {
		if w.state != admitGranted || !w.accounted {
			t.Fatalf("established lease must be granted && accounted, state=%v accounted=%v", w.state, w.accounted)
		}
		if w.reserve != 5<<30 {
			t.Fatalf("ledger RAM = %d, want 5 GiB", w.reserve)
		}
		if w.cpu != 2 {
			t.Fatalf("ledger CPU = %d, want 2", w.cpu)
		}
		if w.parentScopeID != "parent" {
			t.Fatalf("ledger parent_scope_id = %q, want %q (mutation tell: the handler ignores parent_scope_id)", w.parentScopeID, "parent")
		}
		// A grant framed to a runner must pass validRunnerAdmitGrant: basis non-empty and
		// outcome in the closed set {immediate,waited,unevaluated}. An establish minting a
		// novel spelling here would fail-open the client to an ungoverned launch.
		if w.basis == "" {
			t.Fatalf("established lease has an empty basis (validRunnerAdmitGrant requires non-empty)")
		}
		if w.outcome != "immediate" {
			t.Fatalf("established outcome = %q, want the closed-set-safe %q", w.outcome, "immediate")
		}
	})

	if out, cpu, jobs := queueLedger(reDeclareQueue(server)); out != 5<<30 || cpu != 2 || jobs != 1 {
		t.Fatalf("Σleases = {ram:%d cpu:%d jobs:%d}, want {5 GiB, 2, 1}", out, cpu, jobs)
	}

	_ = clientConn.Close()
	waitClosed(t, done, "re-declare handler to return")
}

// TestReDeclareReanchorParentMismatchNotGated pins the §4 decision that a re-anchor LEAVES
// the established parent_scope_id alone and a MISMATCH is logged, NOT gated: a second
// re-declare of the same scope with a DIFFERENT parent is ACCEPTED, refreshes the resource
// vector, and leaves the original parent in the ledger.
func TestReDeclareReanchorParentMismatchNotGated(t *testing.T) {
	server := reDeclareTestServer()
	c1, d1 := driveReDeclare(t, server, goldenReDeclareFrame) // scope "child", parent "parent"
	readReDeclareAck(t, c1)

	frame2, err := encodeReDeclareFrame(reDeclareRecord{ScopeID: "child", RAMBytes: 3 << 30, CPUCores: 1, ParentScopeID: "other-parent"})
	if err != nil {
		t.Fatal(err)
	}
	c2, d2 := driveReDeclare(t, server, frame2)
	readReDeclareAck(t, c2) // ACCEPTED despite the parent disagreement — not gated

	withReDeclareLease(t, server, "child", func(w *admitWaiter) {
		if w.parentScopeID != "parent" {
			t.Fatalf("a re-anchor must KEEP the established parent %q, got %q", "parent", w.parentScopeID)
		}
		if w.reserve != 3<<30 || w.cpu != 1 {
			t.Fatalf("a re-anchor must refresh the vector: reserve=%d cpu=%d", w.reserve, w.cpu)
		}
	})
	if _, _, jobs := queueLedger(reDeclareQueue(server)); jobs != 1 {
		t.Fatalf("a re-anchor must not add a second lease, jobs=%d", jobs)
	}

	_ = c1.Close()
	_ = c2.Close()
	waitClosed(t, d1, "first handler")
	waitClosed(t, d2, "second handler")
}

// TestReDeclareAbsentLeaseEstablishesGrantedAvailableNegative is the absent-lease
// establish test (plan item 8): an empty ledger plus N ARDR re-declares whose declared
// reserves SUM ABOVE the ceiling all ESTABLISH GRANTED, Σleases equals their sum, and
// `available` (ceiling − Σleases) goes NEGATIVE — the §4 re-declare window made explicit.
//
// MUTATION: route the absent-lease re-declare to the QUEUED fresh insert instead of
// establishing granted → each waiter is admitQueued (not granted && accounted), so
// rederiveLedgerLocked counts nothing, Σ stays 0, available stays positive, and the
// granted assertion reds.
func TestReDeclareAbsentLeaseEstablishesGrantedAvailableNegative(t *testing.T) {
	server := reDeclareTestServer()
	const ceiling = 4 * gib
	reserves := []int64{3 * gib, 3 * gib} // sum 6 GiB > 4 GiB ceiling
	for i, reserve := range reserves {
		_, waiter, code, err := server.enqueueReDeclare("/slice", reserve, "redeclare", admitRequest{
			scopeID: "scope-" + formatInt64(int64(i)), cpu: 1, peerSameUID: true, conn: testAnchorConn(),
		})
		if err != nil || code != "" {
			t.Fatalf("absent-lease re-declare %d refused: code=%q err=%v", i, code, err)
		}
		if waiter.state != admitGranted || !waiter.accounted {
			t.Fatalf("absent-lease re-declare %d must ESTABLISH GRANTED, state=%v accounted=%v (mutation: routed to the queued fresh insert)", i, waiter.state, waiter.accounted)
		}
	}

	out, cpu, jobs := queueLedger(reDeclareQueue(server))
	sum := reserves[0] + reserves[1]
	if out != sum || jobs != len(reserves) {
		t.Fatalf("Σleases = %d over %d jobs, want %d over %d", out, jobs, sum, len(reserves))
	}
	if cpu != int64(len(reserves)) {
		t.Fatalf("Σcpu = %d, want %d", cpu, len(reserves))
	}
	if available := int64(ceiling) - out; available >= 0 {
		t.Fatalf("available = %d, want NEGATIVE: sum %d exceeds ceiling %d (§4 re-declare window)", available, sum, ceiling)
	}
}

// TestReDeclareAcceptedForHolderOutsideOwnScope pins design §4 gate P2-C: the ONLY gate
// is SO_PEERCRED same-uid — NO cgroup-membership check — because a confine/aitest holder
// lawfully lives OUTSIDE the scope it holds a lease for. A same-uid re-declare for a scope
// the peer is demonstrably not a member of (this test process is in no aira scope) is
// ACCEPTED. A build that added a scope→cgroup-membership check would reject it.
func TestReDeclareAcceptedForHolderOutsideOwnScope(t *testing.T) {
	server := reDeclareTestServer()
	frame, err := encodeReDeclareFrame(reDeclareRecord{ScopeID: "confine-outside-scope", RAMBytes: 2 << 30, CPUCores: 1})
	if err != nil {
		t.Fatal(err)
	}
	clientConn, done := driveReDeclare(t, server, frame)
	readReDeclareAck(t, clientConn) // ACCEPTED, holder outside its scope
	withReDeclareLease(t, server, "confine-outside-scope", func(w *admitWaiter) {
		if w.state != admitGranted {
			t.Fatalf("a same-uid holder outside its scope must be granted, state=%v", w.state)
		}
	})
	_ = clientConn.Close()
	waitClosed(t, done, "handler")
}

// TestReDeclareOtherUIDRefusedNoAck is the fail-closed direction of the same-uid gate (so
// the accept test above is not vacuous): an other-uid peer's re-declare is REFUSED — no
// ack (the peer reads EOF), no lease established, and the handler returns cleanly without
// a nil-waiter release (the REFUSE path never reaches the release closure).
func TestReDeclareOtherUIDRefusedNoAck(t *testing.T) {
	server := reDeclareTestServer()
	server.peerCredential = func(net.Conn) (int, int, error) { return os.Geteuid() + 1, os.Getpid(), nil }
	frame, err := encodeReDeclareFrame(reDeclareRecord{ScopeID: "other-uid-scope", RAMBytes: 2 << 30, CPUCores: 1})
	if err != nil {
		t.Fatal(err)
	}
	clientConn, done := driveReDeclare(t, server, frame)
	t.Cleanup(func() { _ = clientConn.Close() })

	var ack [1]byte
	if n, err := io.ReadFull(clientConn, ack[:]); err == nil {
		t.Fatalf("an other-uid re-declare must be refused without an ack, got %d byte(s) 0x%02x", n, ack[0])
	}
	waitClosed(t, done, "handler to return on refuse")
	if queue := reDeclareQueue(server); queue != nil {
		if _, _, jobs := queueLedger(queue); jobs != 0 {
			t.Fatalf("a refused re-declare must establish no lease, jobs=%d", jobs)
		}
	}
}

// TestReDeclareClearsReadDeadlineSoHeldLeaseSurvives pins the subtle P1 hazard: the
// read deadline covering the frame body MUST be cleared before the hold, or the held
// connection's blocking 1-byte EOF read times out at the deadline, fires peerCtx, and
// drops a LIVE lease. With a deliberately short Connect deadline, a held (never-closed)
// re-declare connection must keep its lease well past that deadline.
//
// MUTATION: remove the read-deadline clear → the held read times out at ~100 ms → the
// lease is released → the lease lookup reds.
func TestReDeclareClearsReadDeadlineSoHeldLeaseSurvives(t *testing.T) {
	server := reDeclareTestServer()
	server.deadlines.Connect = 100 * time.Millisecond
	frame, err := encodeReDeclareFrame(reDeclareRecord{ScopeID: "held-scope", RAMBytes: 2 << 30, CPUCores: 1})
	if err != nil {
		t.Fatal(err)
	}
	clientConn, done := driveReDeclare(t, server, frame)
	t.Cleanup(func() { _ = clientConn.Close() })
	readReDeclareAck(t, clientConn)

	// The client HOLDS the connection (never closes). Wait well past the former 100 ms
	// read deadline; with the clear in place the lease is held indefinitely. The correct
	// path has NO timing dependency — only the mutant needs the deadline to elapse.
	time.Sleep(500 * time.Millisecond)

	withReDeclareLease(t, server, "held-scope", func(w *admitWaiter) {
		if w.state != admitGranted {
			t.Fatalf("held lease dropped before its EOF, state=%v (mutation: the read-deadline clear is missing)", w.state)
		}
	})
	if _, _, jobs := queueLedger(reDeclareQueue(server)); jobs != 1 {
		t.Fatalf("held lease must survive past the former read deadline, jobs=%d", jobs)
	}

	_ = clientConn.Close()
	waitClosed(t, done, "handler after EOF")
}

// ackErrConn wraps a net.Conn so the single ack Write fails while Read still delegates.
// wroteAck signals the ack attempt; the sync.Once makes the close idempotent in case any
// second write is ever attempted (the panic writer is suppressed by wrote=true, but the
// guard is cheap insurance).
type ackErrConn struct {
	net.Conn
	wroteAck chan struct{}
	once     sync.Once
}

func (c *ackErrConn) Write([]byte) (int, error) {
	c.once.Do(func() { close(c.wroteAck) })
	return 0, errors.New("forced ack write error")
}

// TestReDeclarePostSetErrorHoldsLeaseEOFReleases is the retargeted POST-SET-ERROR / REFUSE
// split: after a SUCCESSFUL establish, the 1-byte ack write FAILS. The lease is already
// anchored to this connection, so the handler must HOLD it and let EOF decide — the ack
// error must NOT release it.
//
// MUTATION: make the handler `return` on the ack-write error → its deferred release runs
// immediately and discharges the established lease → the handler returns (done closes)
// before any EOF. This test catches that return. It is non-vacuous: under correct
// behaviour the handler blocks in its hold (done stays open) and the lease survives until
// the client's EOF, which is the ONLY release path.
func TestReDeclarePostSetErrorHoldsLeaseEOFReleases(t *testing.T) {
	server := reDeclareTestServer()
	serverConn, clientConn := net.Pipe()
	t.Cleanup(func() { _ = clientConn.Close() })
	wrapped := &ackErrConn{Conn: serverConn, wroteAck: make(chan struct{})}
	done := make(chan struct{})
	go func() { defer close(done); server.serveConnection(context.Background(), wrapped) }()
	frame, err := encodeReDeclareFrame(reDeclareRecord{ScopeID: "post-set-err", RAMBytes: 2 << 30, CPUCores: 1})
	if err != nil {
		t.Fatal(err)
	}
	go func() { _, _ = clientConn.Write(frame) }()

	select {
	case <-wrapped.wroteAck:
	case <-testdeadline.After(5 * time.Second):
		t.Fatal("handler never attempted the ack write")
	}

	// The establish succeeded before the ack, so the ack error must NOT end the handler.
	select {
	case <-done:
		t.Fatal("handler RETURNED after the ack-write error — the error path released the established lease instead of holding it (EOF must release, not the write error)")
	case <-testdeadline.After(250 * time.Millisecond):
	}
	withReDeclareLease(t, server, "post-set-err", func(w *admitWaiter) {
		if w.state != admitGranted {
			t.Fatalf("established lease dropped on the ack error, state=%v", w.state)
		}
	})

	// The peer goes away: EOF (the ONLY release path) discharges the lease and the
	// handler returns.
	_ = clientConn.Close()
	waitClosed(t, done, "handler after EOF")
	if queue := reDeclareQueue(server); queue != nil {
		if _, _, jobs := queueLedger(queue); jobs != 0 {
			t.Fatalf("EOF must release the established lease, jobs=%d", jobs)
		}
	}
}

// TestReDeclareUnreadableCredentialFailsClosedNoAck is the UNREADABLE-credential fail
// direction of the same-uid gate at the HANDLER level — the companion to
// TestReDeclareOtherUIDRefusedNoAck, which only covers a readable-but-different uid. When
// the SO_PEERCRED read ERRORS, peerSameUID must stay false BY OMISSION (fail-closed) and
// the re-declare is refused: no ack, no lease. A build that set peerSameUID=true, or that
// assigned it from the (zeroed) uid of a failed read, would establish a lease it could not
// authenticate — and survives every other handler test, since only a credential ERROR (not
// a wrong uid) exercises this path.
func TestReDeclareUnreadableCredentialFailsClosedNoAck(t *testing.T) {
	server := reDeclareTestServer()
	server.peerCredential = func(net.Conn) (int, int, error) { return 0, 0, errors.New("unreadable credential") }
	frame, err := encodeReDeclareFrame(reDeclareRecord{ScopeID: "unreadable-cred-scope", RAMBytes: 2 << 30, CPUCores: 1})
	if err != nil {
		t.Fatal(err)
	}
	clientConn, done := driveReDeclare(t, server, frame)
	t.Cleanup(func() { _ = clientConn.Close() })

	var ack [1]byte
	if n, err := io.ReadFull(clientConn, ack[:]); err == nil {
		t.Fatalf("an unreadable-credential re-declare must fail CLOSED (no ack), got %d byte(s) 0x%02x", n, ack[0])
	}
	waitClosed(t, done, "handler to return on fail-closed refuse")
	if queue := reDeclareQueue(server); queue != nil {
		if _, _, jobs := queueLedger(queue); jobs != 0 {
			t.Fatalf("a fail-closed re-declare must establish no lease, jobs=%d", jobs)
		}
	}
}

// TestReDeclareCrashRestartFloodSkipsMaxWaiters pins that the establish arm runs BEFORE the
// maxWaiters(admitMaxWaiters) gate. The driver for establish-granted is a CRASH restart with
// no dump: the new daemon opens an EMPTY ledger, and EVERY live survivor's re-declare is
// absent-lease — there can be MORE than admitMaxWaiters of them. They must ALL establish
// granted; a single CodeBusy refusal drops a live lease (a survivor launched under admission
// that the daemon now denies, losing its governance). N deliberately exceeds admitMaxWaiters.
//
// MUTATION: move the `else if request.reDeclare` establish arm BELOW the maxWaiters check in
// enqueueAdmitInternal → the (admitMaxWaiters+1)th re-declare is refused CodeBusy → reds here.
func TestReDeclareCrashRestartFloodSkipsMaxWaiters(t *testing.T) {
	server := reDeclareTestServer()
	n := admitMaxWaiters + 44 // comfortably past the gate
	for i := 0; i < n; i++ {
		_, waiter, code, err := server.enqueueReDeclare("/slice", 1*gib, "redeclare", admitRequest{
			scopeID: "flood-" + formatInt64(int64(i)), cpu: 1, peerSameUID: true, conn: testAnchorConn(),
		})
		if err != nil || code != "" {
			t.Fatalf("crash-restart re-declare %d/%d refused (code=%q err=%v) — a survivor past maxWaiters was dropped", i, n, code, err)
		}
		if waiter.state != admitGranted || !waiter.accounted {
			t.Fatalf("crash-restart re-declare %d must establish granted, state=%v accounted=%v", i, waiter.state, waiter.accounted)
		}
	}
	if _, _, jobs := queueLedger(reDeclareQueue(server)); jobs != n {
		t.Fatalf("all %d crash-restart survivors must establish, jobs=%d", n, jobs)
	}
}
