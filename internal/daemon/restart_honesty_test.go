package daemon

import (
	"context"
	"testing"
	"time"

	"aira/internal/core"
	"aira/internal/runner"
)

// verifies: S11 / AIRA-220 honesty (design §4). GrantedEstablished (the confine --list
// "granted pair is trustworthy" bit) reads FALSE during the restart freeze AND while any
// reloaded lease is still unanchored — the granted total is not yet settled — and only
// reads true once the freeze is over and every reloaded lease has been re-declared. It is
// DERIVED from snapshot.present + the freeze + the unanchored count (not hardcoded), so
// it survives S12 deleting the cgroup-scan adoption.
//
// MUTATION: revert to GrantedEstablished: snapshot.present -> the freeze/unanchored arms
// read true and this reds.
func TestGrantedEstablishedFalseWhileFrozenOrUnanchored(t *testing.T) {
	const maximum = int64(16 << 30)
	path := t.TempDir()
	now := time.Unix(9000, 0)
	server := NewServer(Paths{})
	server.admitNow = func() time.Time { return now }
	server.admitResolveSlice = func(string) (string, bool, string) { return path, true, "" }
	server.admitSliceHeadroomBase = 0
	server.admitSliceHeadroomSupervisor = 0
	server.shimReadMemTotal = func() (int64, bool) { return 48 << 30, true }
	server.shimReadMemAvailable = func() (int64, bool, string) { return 20 << 30, true, "" }
	server.admitReadMemory = func(string) (int64, int64, int64, bool, string) {
		return 5 << 30, maximum, 1 << 30, true, ""
	}

	conn := testAnchorConn()
	defer conn.Close()
	seeded := &admitWaiter{
		seq: 1, reserve: 2 << 30, cpu: 1, state: admitGranted, accounted: true,
		grantedCh: closedCh(), scopeID: "CONFINE-g@s", basis: "reload", anchor: nil, unanchored: true,
	}
	queue := &sliceQueue{
		path: path, server: server, kick: make(chan struct{}, 1), stop: make(chan struct{}),
		waiters: []*admitWaiter{seeded},
	}
	queue.outstanding, queue.cpuOutstanding, queue.outstandingJobs = rederiveLedgerLocked(queue)
	registerAdmitQueue(server, queue)

	established := func() bool {
		resp := server.confineManagement(context.Background(),
			core.Request{Verb: "confine-list", Args: map[string]any{"slice": "test.slice", "owner": "session-a"}})
		result, ok := resp.Data.(runner.ConfineListResult)
		if !resp.OK || !ok || result.SliceReserve == nil {
			t.Fatalf("confine-list response=%+v", resp)
		}
		return result.SliceReserve.GrantedEstablished
	}

	server.armRestartFreeze(now)
	if established() {
		t.Fatal("GrantedEstablished must be FALSE during the restart freeze — the granted total is still settling")
	}

	now = now.Add(3 * time.Second) // past freeze-end, but the lease is STILL unanchored
	if established() {
		t.Fatal("GrantedEstablished must stay FALSE while a reloaded lease is unanchored")
	}

	// Re-declare re-anchors the seeded lease (clears unanchored); freeze is over.
	if _, _, code, err := server.enqueueReDeclare(path, 2<<30, "redeclare", admitRequest{
		scopeID: "CONFINE-g@s", peerSameUID: true, conn: conn, cpu: 1,
	}); err != nil || code != "" {
		t.Fatalf("re-declare refused: code=%q err=%v", code, err)
	}
	if !established() {
		t.Fatal("GrantedEstablished must be TRUE once the freeze is over and no unanchored lease remains")
	}
}

// verifies: S11 dump-predicate skip (design §4). A reloaded lease never re-declared is
// still unanchored; snapshotLeaseDump MUST skip it so it is not re-dumped on the next
// graceful shutdown (a ghost would otherwise chain forward through every restart). Once
// re-declared (anchored), it IS dumped like any held lease.
//
// MUTATION: drop the `if waiter.unanchored { continue }` skip -> the ghost is re-dumped
// and the first assertion reds.
func TestDumpSkipsUnanchoredLeases(t *testing.T) {
	server := NewServer(Paths{})
	conn := testAnchorConn()
	defer conn.Close()
	held := &admitWaiter{
		seq: 1, reserve: 1 << 30, cpu: 1, state: admitGranted, accounted: true,
		grantedCh: closedCh(), scopeID: "CONFINE-held@s", anchor: testAnchorConn(),
		clientPID: 111, processStartTick: 5,
	}
	ghost := &admitWaiter{
		seq: 2, reserve: 2 << 30, cpu: 1, state: admitGranted, accounted: true,
		grantedCh: closedCh(), scopeID: "CONFINE-ghost@s", anchor: nil, unanchored: true,
		clientPID: 222, processStartTick: 6,
	}
	queue := &sliceQueue{
		path: "/slice", server: server, kick: make(chan struct{}, 1), stop: make(chan struct{}),
		waiters: []*admitWaiter{held, ghost},
	}
	queue.outstanding, queue.cpuOutstanding, queue.outstandingJobs = rederiveLedgerLocked(queue)
	registerAdmitQueue(server, queue)

	recs := server.snapshotLeaseDump()
	if len(recs) != 1 || recs[0].Frame.ScopeID != "CONFINE-held@s" {
		t.Fatalf("dump captured %d records, want only the anchored held lease (an unanchored reloaded lease must not re-dump and chain a ghost)", len(recs))
	}

	// Re-declare re-anchors the ghost; now it is a held lease and IS dumped.
	if _, _, code, err := server.enqueueReDeclare("/slice", 2<<30, "redeclare", admitRequest{
		scopeID: "CONFINE-ghost@s", peerSameUID: true, conn: conn, cpu: 1,
	}); err != nil || code != "" {
		t.Fatalf("re-declare refused: code=%q err=%v", code, err)
	}
	if recs := server.snapshotLeaseDump(); len(recs) != 2 {
		t.Fatalf("after re-declare the re-anchored lease must be dumped: %d records, want 2", len(recs))
	}
}
