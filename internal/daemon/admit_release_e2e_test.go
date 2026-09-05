package daemon

import (
	"errors"
	"net"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"aira/internal/testdeadline"
)

// AIRA-68's own claim, as a standing regression: reserves granted for jobs that
// no longer exist must not accumulate. The ledger's ONE discharge is
// releaseAdmitWaiter, reached from admitConnection's deferred release when the
// client's connection goes away, so this drives real admitConnection goroutines
// over real connections and asserts the ledger returns to exactly zero.
//
// Both ledger populations are represented, because only one of them has any
// cgroup artifact to fall back on: a scope-backed `aira confine` grant carries a
// scope_id, a scope-less `aira confine-reserve` grant does not and can ONLY ever
// be released by its connection closing.

func e2eConfineAdmitArgs(reserve int64, scopeID, name string) map[string]any {
	args := map[string]any{"slice": "slice", "reserve": reserve, "max_wait_ms": int64(60000), "signature": "", "pinned": true}
	if scopeID != "" {
		// AIRA-52: the owner is carried by the scope id, and admission binds the
		// two, so the fixture must mint the same pair a real launcher does.
		args["scope_id"], args["name"], args["owner"] = scopeID+"@session-e2e", name, "session-e2e"
	}
	return args
}

func e2eDrainGrant(conn net.Conn) {
	var frame ResponseFrame
	_ = readFrame(conn, &frame)
}

// e2eWaitLedger waits for the ledger to reach an exactly-stated state, and
// insists the queue is STILL PRESENT while it reads it.
//
// The `present` requirement is load-bearing, not decoration. releaseAdmitWaiter
// removes the waiter from queue.waiters OUTSIDE the `accounted` guard that does
// the arithmetic, and pruneAdmitQueue then deletes a queue whose waiter list is
// empty — so once the last client goes away the queue is gone and
// admitSliceSnapshot honestly reports the absent-queue zero. An earlier version
// of these tests waited for (0 jobs, 0 bytes) after closing every client, which
// that absent-queue zero satisfies WITHOUT the discharge ever running: mutation
// testing (deleting `queue.outstanding -= waiter.reserve; queue.outstandingJobs--`
// outright) left both tests green. They were reading the ledger's absence as
// proof of its correctness — the exact leak this ticket alleges would have
// slipped straight through.
//
// The tests below therefore keep one PINNED client open throughout, so the queue
// can never be pruned, and assert the discharged ledger equals the pin's own
// contribution rather than zero.
func e2eWaitLedger(t *testing.T, server *Server, wantJobs int, wantBytes int64) {
	t.Helper()
	deadline := time.Now().Add(testdeadline.Wait(3 * time.Second))
	var snapshot admitSnapshot
	for time.Now().Before(deadline) {
		snapshot = server.admitSliceSnapshot("/slice")
		if snapshot.present && snapshot.outstandingJobs == wantJobs && snapshot.outstanding == wantBytes {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("ledger settled at present=%v %d jobs / %d bytes, want present=true %d / %d (split: scope=%d/%d reservation=%d/%d)",
		snapshot.present, snapshot.outstandingJobs, snapshot.outstanding, wantJobs, wantBytes,
		snapshot.scopeJobs, snapshot.scopeBytes, snapshot.reservationJobs, snapshot.reservationBytes)
}

// e2ePinQueue holds one granted lease open for the whole test so the queue is
// never pruned out from under the assertions. Returns the pin's reserve and a
// closer.
func e2ePinQueue(t *testing.T, server *Server) (int64, func()) {
	t.Helper()
	const pinReserve = int64(1 << 24)
	serverConn, clientConn := net.Pipe()
	go func() {
		defer serverConn.Close()
		server.admitConnection(serverConn, e2eConfineAdmitArgs(pinReserve, "CONFINE-pin-5101-zz", "pin"))
	}()
	go e2eDrainGrant(clientConn)
	e2eWaitLedger(t, server, 1, pinReserve)
	return pinReserve, func() { _ = clientConn.Close() }
}

// verifies: every granted lease of both populations is discharged when its
// client's connection closes, and the ledger returns to a true zero — not merely
// to "smaller". A lost decrement in releaseAdmitWaiter is exactly the leak this
// ticket alleged, and it would leave a permanent residue here.
func TestAdmitLedgerReturnsToZeroWhenEveryClientConnectionCloses(t *testing.T) {
	var maximum atomic.Int64
	maximum.Store(1 << 40)
	server := admitTestServer(&maximum)
	server.admitPollInterval = 5 * time.Millisecond

	// Pinned first: it keeps the queue alive so the post-release assertions read
	// a REAL discharged ledger rather than an absent one.
	pinReserve, closePin := e2ePinQueue(t, server)
	defer closePin()

	const scopeBacked, scopeless = 3, 5
	clients := make([]net.Conn, 0, scopeBacked+scopeless)
	for index := 0; index < scopeBacked+scopeless; index++ {
		serverConn, clientConn := net.Pipe()
		args := e2eConfineAdmitArgs(1<<20, "", "")
		if index < scopeBacked {
			name := "job" + strconv.Itoa(index)
			args = e2eConfineAdmitArgs(1<<20, "CONFINE-"+name+"-5101-"+strconv.FormatInt(int64(index)+1, 36), name)
		}
		go func() {
			defer serverConn.Close()
			server.admitConnection(serverConn, args)
		}()
		// Draining the grant frame is what a real client does; net.Pipe is
		// unbuffered, so without a reader the daemon's write blocks and no lease is
		// ever fully established.
		go e2eDrainGrant(clientConn)
		clients = append(clients, clientConn)
	}

	// +1 job / +pinReserve throughout: the pin is a genuine granted lease and is
	// counted like any other.
	e2eWaitLedger(t, server, scopeBacked+scopeless+1, int64(scopeBacked+scopeless)<<20+pinReserve)
	snapshot := server.admitSliceSnapshot("/slice")
	if snapshot.scopeJobs != scopeBacked+1 || snapshot.reservationJobs != scopeless {
		t.Fatalf("populations = %d scope-backed / %d scope-less, want %d / %d",
			snapshot.scopeJobs, snapshot.reservationJobs, scopeBacked+1, scopeless)
	}

	for _, conn := range clients {
		_ = conn.Close()
	}
	// Down to exactly the pin — a POSITIVE statement about what remains charged,
	// which an absent queue cannot satisfy and a lost decrement cannot reach.
	e2eWaitLedger(t, server, 1, pinReserve)

	final := server.admitSliceSnapshot("/slice")
	if !final.present {
		t.Fatalf("the pinned queue vanished; the assertions above proved nothing")
	}
	if final.scopeJobs != 1 || final.scopeBytes != pinReserve || final.reservationJobs != 0 || final.reservationBytes != 0 {
		t.Fatalf("split did not return to the pin alone: %+v", final)
	}
	if final.residualJobs() != 0 || final.residualBytes() != 0 {
		t.Fatalf("residual after full release: jobs=%d bytes=%d", final.residualJobs(), final.residualBytes())
	}
}

var errAbruptPeer = errors.New("connection reset by peer")

// abruptConn delivers a NON-EOF read error, which is what the daemon actually
// sees when a client dies abnormally — SIGKILL of a supervisor, a cgroup.kill of
// the whole job tree, a transport reset — as opposed to the clean io.EOF of an
// orderly close. net.Pipe alone cannot express the difference: closing either
// end gives the peer a clean EOF.
type abruptConn struct {
	net.Conn
	once sync.Once
	dead chan struct{}
}

func (c *abruptConn) kill() { c.once.Do(func() { close(c.dead) }) }

func (c *abruptConn) Read(p []byte) (int, error) {
	type result struct {
		n   int
		err error
	}
	done := make(chan result, 1)
	go func() {
		n, err := c.Conn.Read(p)
		done <- result{n, err}
	}()
	select {
	case <-c.dead:
		return 0, errAbruptPeer
	case got := <-done:
		return got.n, got.err
	}
}

// verifies: the discharge does not depend on a GRACEFUL close. The filed failure
// shape is a client that dies — killed, crashed, group-killed with its scope — so
// the daemon must reclaim on any read failure, not only on a clean io.EOF. A
// peer-close watcher that acted on io.EOF alone would hold the reserve of every
// abnormally terminated job for the daemon's remaining uptime, which is exactly
// the accumulation AIRA-68 describes.
func TestAdmitLedgerReleasesWhenAClientDiesWithoutClosingCleanly(t *testing.T) {
	var maximum atomic.Int64
	maximum.Store(1 << 40)
	server := admitTestServer(&maximum)
	server.admitPollInterval = 5 * time.Millisecond

	// Pinned, for the same reason as the test above: without it the killed
	// client's release empties the queue, pruneAdmitQueue deletes it, and a
	// ledger that never discharged at all reads as a clean zero.
	pinReserve, closePin := e2ePinQueue(t, server)
	defer closePin()

	rawServer, clientConn := net.Pipe()
	serverConn := &abruptConn{Conn: rawServer, dead: make(chan struct{})}
	go func() {
		defer rawServer.Close()
		server.admitConnection(serverConn, e2eConfineAdmitArgs(4<<20, "", ""))
	}()
	go e2eDrainGrant(clientConn)
	e2eWaitLedger(t, server, 2, pinReserve+4<<20)

	serverConn.kill()
	e2eWaitLedger(t, server, 1, pinReserve)
	if final := server.admitSliceSnapshot("/slice"); final.reservationJobs != 0 || final.reservationBytes != 0 {
		t.Fatalf("the killed client's scope-less reservation was not discharged: %+v", final)
	}
	defer clientConn.Close()
}
