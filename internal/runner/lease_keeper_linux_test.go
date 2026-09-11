//go:build linux

package runner

import (
	"bytes"
	"context"
	"errors"
	"net"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"aira/internal/redeclare"
)

// syncBuffer is a tiny mutex-guarded buffer: the exclusivity watcher writes the
// warning from its own goroutine while the test reads it.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// closeTrackingConn records whether Close was called, to prove a conn opened
// during a Close-race is not leaked.
type closeTrackingConn struct {
	net.Conn
	closed *atomic.Bool
}

func (c *closeTrackingConn) Close() error {
	c.closed.Store(true)
	return c.Conn.Close()
}

// verifies: design §4 (post-grant reconnect + ARDR re-declare). On the held lease's
// EOF a scope-bearing non-exclusive keeper reconnects and re-declares the lease
// VERBATIM — scope id byte-for-byte, RAM = the granted reserve, CPU = the declared
// core count, parent as declared — so the new daemon re-anchors the SAME lease with
// no second lease and no double-count (the single sharpest correctness point).
func TestLeaseKeeperReDeclaresOnEOFWithVerbatimFrame(t *testing.T) {
	got := make(chan redeclare.Record, 1)
	dial := func(context.Context, string) (net.Conn, error) {
		c, s := net.Pipe()
		go func() {
			defer s.Close()
			rec, err := redeclare.DecodeFrame(s)
			if err != nil {
				return
			}
			got <- rec
			_, _ = s.Write([]byte{redeclare.AckByte})
			var one [1]byte
			_, _ = s.Read(one[:]) // hold the re-anchored lease until the keeper closes
		}()
		return c, nil
	}
	initialClient, initialServer := net.Pipe()
	req := Request{ConfineScopeID: "aira.slice/aira-CONFINE-x.scope@session", ParentScopeID: "aira.slice/p.scope"}
	grant := runnerAdmitGrant{State: "immediate", Reserve: 5 << 30, Basis: "pinned:client"}
	k := newLeaseKeeper(initialClient, req, grant, dial, "/sock")
	defer k.Close()

	_ = initialServer.Close() // the daemon restarted: the held lease EOFs

	select {
	case rec := <-got:
		if rec.ScopeID != req.ConfineScopeID {
			t.Fatalf("re-declared scope_id=%q, want the VERBATIM %q", rec.ScopeID, req.ConfineScopeID)
		}
		if rec.RAMBytes != uint64(grant.Reserve) {
			t.Fatalf("re-declared ram=%d, want the granted reserve %d", rec.RAMBytes, grant.Reserve)
		}
		if rec.CPUCores != uint32(DefaultConfineCPUCores) {
			t.Fatalf("re-declared cpu=%d, want %d", rec.CPUCores, DefaultConfineCPUCores)
		}
		if rec.ParentScopeID != req.ParentScopeID {
			t.Fatalf("re-declared parent=%q, want %q", rec.ParentScopeID, req.ParentScopeID)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("keeper did not re-declare after the held lease EOF'd")
	}
}

// verifies: design §8 — a --delegate-ram grant declares 0 CPU cores, and the
// re-declare must reproduce that (charging it a core would double-count).
func TestLeaseKeeperDelegateRAMReDeclaresZeroCores(t *testing.T) {
	got := make(chan redeclare.Record, 1)
	dial := func(context.Context, string) (net.Conn, error) {
		c, s := net.Pipe()
		go func() {
			defer s.Close()
			rec, err := redeclare.DecodeFrame(s)
			if err != nil {
				return
			}
			got <- rec
			_, _ = s.Write([]byte{redeclare.AckByte})
			var one [1]byte
			_, _ = s.Read(one[:])
		}()
		return c, nil
	}
	initialClient, initialServer := net.Pipe()
	req := Request{ConfineScopeID: "aira.slice/suite.scope", DelegateRAM: true}
	grant := runnerAdmitGrant{State: "immediate", Reserve: 8 << 30, Basis: "pinned:client"}
	k := newLeaseKeeper(initialClient, req, grant, dial, "/sock")
	defer k.Close()
	_ = initialServer.Close()
	select {
	case rec := <-got:
		if rec.CPUCores != 0 {
			t.Fatalf("delegate re-declare cpu=%d, want 0", rec.CPUCores)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("keeper did not re-declare")
	}
}

// verifies: §15 P2-D — an exclusive lease is LOST across a restart (exclusivity
// cannot be re-established), so the keeper NEVER reconnects; it records the loss and
// warns. The stopped flag (not reconnect) is what it does on the held conn's EOF.
func TestLeaseKeeperExclusiveReportsLossAndNeverReconnects(t *testing.T) {
	var dialed atomic.Int64
	dial := func(context.Context, string) (net.Conn, error) {
		dialed.Add(1)
		return nil, errors.New("an exclusive keeper must never reconnect")
	}
	client, server := net.Pipe()
	req := Request{Exclusive: true, ConfineScopeID: "aira.slice/x.scope"}
	grant := runnerAdmitGrant{State: "immediate", Reserve: 1 << 30, Basis: "pinned:client"}
	k := newLeaseKeeper(client, req, grant, dial, "/sock")
	defer k.Close()
	var warn syncBuffer
	k.watchExclusive(&warn)

	_ = server.Close() // the daemon restarted: the exclusive lease is gone

	deadline := time.Now().Add(2 * time.Second)
	for !k.exclusiveWasLost() && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if !k.exclusiveWasLost() {
		t.Fatal("exclusive loss was not recorded after the lease closed")
	}
	if dialed.Load() != 0 {
		t.Fatalf("exclusive keeper reconnected %d times; it must never reconnect", dialed.Load())
	}
	if !strings.Contains(warn.String(), "exclusivity lost") {
		t.Fatalf("exclusive-loss warning not written: %q", warn.String())
	}
}

// verifies: a clean teardown (Close) must NOT be reported as an exclusive loss — the
// keeper's stopped flag is the S13 generalisation of the old teardownStarted guard.
func TestLeaseKeeperExclusiveTeardownIsNotALoss(t *testing.T) {
	dial := func(context.Context, string) (net.Conn, error) { return nil, errors.New("nope") }
	client, _ := net.Pipe()
	req := Request{Exclusive: true, ConfineScopeID: "aira.slice/x.scope"}
	grant := runnerAdmitGrant{State: "immediate", Reserve: 1 << 30, Basis: "pinned:client"}
	k := newLeaseKeeper(client, req, grant, dial, "/sock")
	var warn syncBuffer
	k.watchExclusive(&warn)
	_ = k.Close() // teardown closes the conn; the watcher must treat this as clean

	time.Sleep(100 * time.Millisecond)
	if k.exclusiveWasLost() {
		t.Fatal("a clean teardown was reported as exclusive=lost")
	}
	if strings.Contains(warn.String(), "exclusivity lost") {
		t.Fatalf("teardown wrote a spurious loss warning: %q", warn.String())
	}
}

// verifies: a scope-less confine-reserve lease has no ARDR key, so its keeper holds
// only — no reconnect goroutine, and the held conn closing triggers no dial.
func TestLeaseKeeperScopelessHoldsAndNeverReconnects(t *testing.T) {
	var dialed atomic.Int64
	dial := func(context.Context, string) (net.Conn, error) {
		dialed.Add(1)
		return nil, errors.New("scope-less keeper must not reconnect")
	}
	client, server := net.Pipe()
	req := Request{} // scope-less, non-exclusive
	grant := runnerAdmitGrant{State: "immediate", Reserve: 1 << 30, Basis: "pinned:client"}
	k := newLeaseKeeper(client, req, grant, dial, "/sock")
	_ = server.Close() // would drive a reconnect IF a goroutine were watching

	time.Sleep(100 * time.Millisecond)
	if dialed.Load() != 0 {
		t.Fatalf("scope-less keeper reconnected %d times; it must only hold", dialed.Load())
	}
	if err := k.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
}

// verifies: THE RACE (the concurrency reviewer's target). Close() firing while the
// keeper is mid-dial must close the conn the dial returns — otherwise it anchors a
// lease in the new daemon that nobody will ever release (a leaked lease).
func TestLeaseKeeperCloseDuringDialClosesDialedConn(t *testing.T) {
	initialClient, _ := net.Pipe()
	release := make(chan struct{})
	dialedClient, dialedServer := net.Pipe()
	defer dialedServer.Close()
	var dialedClosed atomic.Bool
	wrapped := &closeTrackingConn{Conn: dialedClient, closed: &dialedClosed}
	dial := func(context.Context, string) (net.Conn, error) {
		<-release // block so the test can call Close() while the dial is in flight
		return wrapped, nil
	}
	frame, err := redeclare.EncodeFrame(redeclare.Record{ScopeID: "x", RAMBytes: 1, CPUCores: 1})
	if err != nil {
		t.Fatal(err)
	}
	k := &leaseKeeper{
		conn: initialClient, done: make(chan struct{}),
		dial: dial, socketPath: "/s", frame: frame,
		dialTimeout: time.Second, reconnectGap: time.Millisecond, maxNoAck: 5,
	}
	loopDone := make(chan bool, 1)
	go func() { loopDone <- k.reconnectAndReDeclare() }()

	time.Sleep(50 * time.Millisecond) // let the loop reach the blocked dial
	_ = k.Close()                     // Close races the in-flight dial
	close(release)                    // the dial now returns wrapped

	select {
	case reAnchored := <-loopDone:
		if reAnchored {
			t.Fatal("reconnect reported re-anchored despite Close() — a lease was anchored during teardown")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("reconnect did not exit after Close()")
	}
	deadline := time.Now().Add(2 * time.Second)
	for !dialedClosed.Load() && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if !dialedClosed.Load() {
		t.Fatal("the conn dialled during Close() was leaked: it must be closed so it anchors no lease")
	}
}

// verifies: class-3 failure (S9 residual) — a refuse and a crash-mid-re-declare are
// indistinguishable (EOF/no ack), so the keeper retries, but BOUNDED: after maxNoAck
// consecutive connected-but-no-ack attempts it gives up, logs, and leaves the job
// running (never killed, never fallen open).
func TestLeaseKeeperBoundedRetryGivesUpAfterNoAck(t *testing.T) {
	var dials atomic.Int64
	dial := func(context.Context, string) (net.Conn, error) {
		dials.Add(1)
		c, s := net.Pipe()
		go func() {
			defer s.Close()
			_, _ = redeclare.DecodeFrame(s) // consume the frame
			_, _ = s.Write([]byte{0x00})    // a WRONG ack byte: connected, but no valid ack
		}()
		return c, nil
	}
	frame, err := redeclare.EncodeFrame(redeclare.Record{ScopeID: "x", RAMBytes: 1, CPUCores: 1})
	if err != nil {
		t.Fatal(err)
	}
	held, _ := net.Pipe()
	k := &leaseKeeper{
		conn: held, done: make(chan struct{}),
		dial: dial, socketPath: "/s", frame: frame,
		dialTimeout: time.Second, reconnectGap: time.Millisecond, maxNoAck: 3,
	}
	if k.reconnectAndReDeclare() {
		t.Fatal("expected the keeper to GIVE UP (return false) on persistent no-ack, not re-anchor")
	}
	if dials.Load() != 3 {
		t.Fatalf("dials=%d, want maxNoAck=3 consecutive connected-but-no-ack attempts then give-up", dials.Load())
	}
}

// verifies: a down daemon (dial refused) is retried INDEFINITELY — it is not a
// no-ack and never spends the give-up budget — until the daemon returns and acks.
func TestLeaseKeeperDownDaemonRetriesThenReAnchors(t *testing.T) {
	var dials atomic.Int64
	dial := func(context.Context, string) (net.Conn, error) {
		if dials.Add(1) <= 5 {
			return nil, errors.New("connection refused")
		}
		c, s := net.Pipe()
		go func() {
			defer s.Close()
			_, _ = redeclare.DecodeFrame(s)
			_, _ = s.Write([]byte{redeclare.AckByte})
			var one [1]byte
			_, _ = s.Read(one[:])
		}()
		return c, nil
	}
	frame, err := redeclare.EncodeFrame(redeclare.Record{ScopeID: "x", RAMBytes: 1, CPUCores: 1})
	if err != nil {
		t.Fatal(err)
	}
	held, _ := net.Pipe()
	k := &leaseKeeper{
		conn: held, done: make(chan struct{}),
		dial: dial, socketPath: "/s", frame: frame,
		dialTimeout: time.Second, reconnectGap: time.Millisecond, maxNoAck: 2,
	}
	defer k.Close()
	if !k.reconnectAndReDeclare() {
		t.Fatal("expected re-anchor (true) once the daemon returned, despite 5 refused dials")
	}
	if dials.Load() != 6 {
		t.Fatalf("dials=%d, want 6 (5 refused + 1 grant); refused dials must not count toward maxNoAck", dials.Load())
	}
}
