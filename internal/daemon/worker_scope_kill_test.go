package daemon

import (
	"context"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"aira/internal/runner"
)

// S2a §16b / §16.1 / §16.2 — the daemon kills + rmdirs a worker's SIBLING scope on
// that worker relay's peer-EOF, so a relay death (Ctrl-C, --kill, parent oom.group,
// crash) that frees the ledger lease no longer leaves the worker PROCESS running in a
// scope nothing above it can reach. Gated three ways: never on a daemon restart
// (s.stopping — live workers re-declare), never in shim mode (no cgroup), and — the
// subtle one — only when THIS connection's anchored release actually discharged
// (§16.2 P1-B: a late-ack redial re-anchors the live worker onto conn2, so conn1's
// stale EOF must not kill a mid-test worker; a release is idempotent, a kill is not).

// killRecorder is the injectable workerScopeKill seam for these tests. The kill runs
// inline in the relay's handler goroutine while the test goroutine reads the tally, so
// the mutex is load-bearing under -race even though `done` orders the two.
type killRecorder struct {
	mu    sync.Mutex
	calls []killCall
}

type killCall struct {
	slice   string
	scopeID string
}

func (k *killRecorder) fn() func(context.Context, string, string) error {
	return func(_ context.Context, slice, scopeID string) error {
		k.mu.Lock()
		defer k.mu.Unlock()
		k.calls = append(k.calls, killCall{slice: slice, scopeID: scopeID})
		return nil
	}
}

func (k *killRecorder) count() int {
	k.mu.Lock()
	defer k.mu.Unlock()
	return len(k.calls)
}

func (k *killRecorder) last() killCall {
	k.mu.Lock()
	defer k.mu.Unlock()
	return k.calls[len(k.calls)-1]
}

// verifies: S2a §16b — a worker relay's peer-EOF makes the daemon cgroup.kill + rmdir
// that worker's sibling scope. MUTATION: drop the killWorkerScope call from the fresh
// relay's peerCtx branch → the scope is never killed and this REDS.
func TestWorkerAdmitPeerEOFKillsWorkerScope(t *testing.T) {
	server := workerAdmitServer(t, "/slice", 4*workerTestMiB)
	rec := &killRecorder{}
	server.workerScopeKill = rec.fn()

	resp, client, done := startWorkerAdmit(t, server, workerArgs(workerTestOuterScope, workerTestMiB, false, 0))
	if resp.State != runner.WorkerAdmitStateGranted {
		t.Fatalf("resp=%+v, want granted", resp)
	}
	scopeID := strings.TrimPrefix(filepath.Base(resp.ScopePath), ".aira-")

	// The relay closes (worker retired, killed, or crashed). The daemon must tear down
	// the sibling worker scope on that EOF.
	_ = client.Close()
	awaitReturn(t, done, "worker handler to return on EOF")

	if rec.count() != 1 {
		t.Fatalf("kill called %d times after peer-EOF, want exactly 1 (the daemon kills+rmdirs the sibling worker scope on relay EOF, §16b)", rec.count())
	}
	if got := rec.last(); got.slice != "/slice" || got.scopeID != scopeID {
		t.Fatalf("kill called with (slice=%q scope=%q), want (%q %q) — the resolved slice + the minted worker scope-id", got.slice, got.scopeID, "/slice", scopeID)
	}
}

// verifies: S2a §16b — a daemon RESTART must NOT kill live workers. close(stopping)
// EOFs every relay handler (the daemon does not close the live conn first), but the
// worker survives and re-declares onto the fresh daemon, so the handler returns WITHOUT
// killing. MUTATION: fire the kill unconditionally on the peerCtx branch without the
// stopping re-check → a restart re-orphans/kills mid-test workers and this REDS.
func TestWorkerAdmitStoppingDoesNotKillWorkerScope(t *testing.T) {
	server := workerAdmitServer(t, "/slice", 4*workerTestMiB)
	rec := &killRecorder{}
	server.workerScopeKill = rec.fn()

	resp, client, done := startWorkerAdmit(t, server, workerArgs(workerTestOuterScope, workerTestMiB, false, 0))
	defer client.Close()
	if resp.State != runner.WorkerAdmitStateGranted {
		t.Fatalf("resp=%+v, want granted", resp)
	}

	// A restart closes stopping WITHOUT closing the live relay's connection (server.go
	// returns on <-s.stopping, then defers the close). The worker must not be killed.
	close(server.stopping)
	awaitReturn(t, done, "worker handler to return on stopping")

	if rec.count() != 0 {
		t.Fatalf("kill called %d times on daemon stopping, want 0 (a restart must not kill live workers — they re-declare, §16b)", rec.count())
	}
}

// verifies: S2a §16.2 (P1-B) — the anchor gate. A late-ack redial re-declares the SAME
// worker lease on conn2, re-anchoring the LIVE waiter; conn1 (the stale fresh-admit
// handler) then EOFs. conn1's anchored release is a no-op (the anchor moved), so it
// discharges NOTHING and must NOT kill the mid-test worker. Only conn2's EOF — the
// current anchor, which DOES discharge — kills+rmdirs the scope (via serveReDeclare's
// matching hook, exercising the restart-path half too).
//
// MUTATION / red-before-the-gate: remove the `if discharged` gate (kill on bare
// peerCtx.Done) → conn1's EOF kills a mid-test worker and the first assertion REDS.
func TestWorkerReanchorThenStaleEOFDoesNotKillButAnchorEOFDoes(t *testing.T) {
	server := workerAdmitServer(t, "/slice", 4*workerTestMiB)
	// The re-declare's same-uid gate (admit.go) fails closed without a credential seam.
	server.peerCredential = func(net.Conn) (int, int, error) { return os.Geteuid(), os.Getpid(), nil }
	rec := &killRecorder{}
	server.workerScopeKill = rec.fn()

	resp, conn1, done1 := startWorkerAdmit(t, server, workerArgs(workerTestOuterScope, workerTestMiB, false, 0))
	if resp.State != runner.WorkerAdmitStateGranted {
		t.Fatalf("resp=%+v, want granted", resp)
	}
	scopeID := strings.TrimPrefix(filepath.Base(resp.ScopePath), ".aira-")

	// Re-declare the same worker lease on conn2 (the reconnect after a late ack), which
	// re-anchors the still-live waiter to conn2.
	frame, err := encodeReDeclareFrame(reDeclareRecord{
		ScopeID:       scopeID,
		RAMBytes:      uint64(workerTestMiB),
		CPUCores:      uint32(runner.DefaultConfineCPUCores),
		ParentScopeID: workerTestParentScopeID,
	})
	if err != nil {
		t.Fatal(err)
	}
	conn2, done2 := driveReDeclare(t, server, frame)
	readReDeclareAck(t, conn2)

	// conn1 (stale) EOFs: its release finds the anchor is conn2 → discharges nothing →
	// the mid-test worker must survive, unkilled.
	_ = conn1.Close()
	awaitReturn(t, done1, "stale conn1 handler to return")
	if rec.count() != 0 {
		t.Fatalf("kill called %d times on the STALE conn1 EOF, want 0 — the lease was re-anchored to a live conn2, so conn1's release discharged nothing and must not kill a mid-test worker (§16.2 P1-B)", rec.count())
	}
	if ids := grantedWaiterScopeIDs(t, server, "/slice"); len(ids) != 1 {
		t.Fatalf("granted leases after the stale conn1 EOF = %v, want exactly the 1 lease re-anchored on conn2 (still live)", ids)
	}

	// conn2 (the live anchor) EOFs: its release DISCHARGES → the worker IS torn down.
	_ = conn2.Close()
	awaitReturn(t, done2, "re-declare handler to return on conn2 EOF")
	if rec.count() != 1 {
		t.Fatalf("kill called %d times after the ANCHOR conn2 EOF, want exactly 1 (a discharging anchored release kills+rmdirs the worker scope)", rec.count())
	}
	if got := rec.last(); got.slice != "/slice" || got.scopeID != scopeID {
		t.Fatalf("kill called with (slice=%q scope=%q), want (%q %q)", got.slice, got.scopeID, "/slice", scopeID)
	}
}

// verifies: S2a §16.1 — serveReDeclare installs the peer-EOF kill ONLY for a worker
// lease shape (scopeID != "" AND parentScopeID != ""). A scoped ORDINARY confine admit
// re-declares with a scopeID but NO parent_scope_id, so its EOF must NOT kill anything.
// MUTATION: drop the `charge.ParentScopeID != ""` clause → an ordinary confine
// re-declare's EOF kills its scope and this REDS.
func TestReDeclareOrdinaryScopedAdmitPeerEOFDoesNotKill(t *testing.T) {
	server := workerAdmitServer(t, "/slice", 4*workerTestMiB)
	server.peerCredential = func(net.Conn) (int, int, error) { return os.Geteuid(), os.Getpid(), nil }
	rec := &killRecorder{}
	server.workerScopeKill = rec.fn()

	// An ORDINARY scoped confine re-declare: a scope id, but empty parent_scope_id.
	frame, err := encodeReDeclareFrame(reDeclareRecord{
		ScopeID:  "CONFINE-ordinary-222222-abc",
		RAMBytes: uint64(workerTestMiB),
		CPUCores: uint32(runner.DefaultConfineCPUCores),
	})
	if err != nil {
		t.Fatal(err)
	}
	conn, done := driveReDeclare(t, server, frame)
	readReDeclareAck(t, conn)

	_ = conn.Close()
	awaitReturn(t, done, "ordinary re-declare handler to return")
	if rec.count() != 0 {
		t.Fatalf("kill called %d times on an ORDINARY scoped confine re-declare's EOF, want 0 — only a worker lease (with a parent_scope_id) gets the peer-EOF kill (§16.1)", rec.count())
	}
}
