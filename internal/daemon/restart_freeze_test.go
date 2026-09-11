package daemon

import (
	"sync/atomic"
	"testing"
	"time"
)

// verifies: S11 restart freeze (design §4). During the ~2s window a NEW admission
// WAITS (never grants, never fail-opens), while a re-declare of a seeded UNANCHORED
// lease is accepted IMMEDIATELY and re-anchors it — re-declares never route through the
// frozen evaluator grant pass. After the freeze ends the new admission is granted.
func TestRestartFreezeBlocksNewAdmissionWhileReDeclareAnchors(t *testing.T) {
	var maximum atomic.Int64
	maximum.Store(100 << 30)
	now := time.Unix(5000, 0)
	server := admitTestServer(&maximum)
	server.admitNow = func() time.Time { return now }

	// A seeded UNANCHORED lease (as the reload leaves it) + a fresh NEW admission queued,
	// in a queue the test drives directly (no evaluator goroutine).
	connA := testAnchorConn()
	defer connA.Close()
	seeded := &admitWaiter{
		seq: 1, reserve: 2 * gib, cpu: 0, state: admitGranted, accounted: true,
		grantedCh: closedCh(), scopeID: "CONFINE-s@x", basis: "reload",
		anchor: nil, unanchored: true,
	}
	newAdmit := queuedWaiter(2, 1*gib, now)
	queue := &sliceQueue{
		path: "/slice", server: server, kick: make(chan struct{}, 1), stop: make(chan struct{}),
		waiters:     []*admitWaiter{seeded, newAdmit},
		outstanding: 2 * gib, cpuOutstanding: 0, outstandingJobs: 1,
	}
	registerAdmitQueue(server, queue)

	server.armRestartFreeze(now) // freeze-end = now + restartFreeze (2s)

	// Frozen: the new admission stays queued, the seeded lease stays granted+unanchored.
	server.evaluateAdmitQueue(queue)
	requireAdmitQueued(t, newAdmit)
	if seeded.state != admitGranted || !seeded.unanchored {
		t.Fatalf("seeded lease state=%d unanchored=%v, want granted+unanchored during the freeze", seeded.state, seeded.unanchored)
	}

	// A re-declare of the seeded scope is accepted IMMEDIATELY during the freeze and
	// re-anchors it in place (unanchored cleared), proving re-declares are never frozen.
	_, got, code, err := server.enqueueReDeclare("/slice", 2*gib, "redeclare", admitRequest{
		scopeID: "CONFINE-s@x", peerSameUID: true, conn: connA, clientPID: 4321, processStartTick: 99,
	})
	if err != nil || code != "" {
		t.Fatalf("re-declare during freeze refused: code=%q err=%v", code, err)
	}
	if got != seeded || seeded.unanchored || seeded.anchor != connA {
		t.Fatalf("re-declare must re-anchor the seeded lease in place: got==seeded=%v unanchored=%v anchor==connA=%v",
			got == seeded, seeded.unanchored, seeded.anchor == connA)
	}
	requireAdmitQueued(t, newAdmit) // still frozen — the re-declare did not unfreeze

	// Past freeze-end: the new admission is now granted (the seeded 2Gi leaves 98Gi).
	now = now.Add(3 * time.Second)
	server.evaluateAdmitQueue(queue)
	waitAdmitGrant(t, newAdmit)
}

// verifies: S11 honesty (design §6/§10 Inv 6). A NON-BLOCKING admit (max_wait_ms==0)
// that times out DURING the freeze reports Contention "unevaluated", NOT a fabricated
// "saturated" solitude (the slice is frozen, not contended) and NEVER a grant-shaped
// "unevaluated" (which the runner launches uncapped). The freeze gate joins no
// contention latch, so the unset latch renders "unevaluated".
//
// (§6's advisory "return current available" non-blocking mode is S15's worker-admit
// rebuild; S11 pins only that the freeze fabricates no reading and never fail-opens.)
func TestRestartFreezeNonBlockingReturnsUnevaluated(t *testing.T) {
	const maximum = int64(8) << 30
	now := time.Unix(6000, 0)
	server := saturatedServer(t)
	server.admitNow = func() time.Time { return now }
	server.admitReadMemory = func(string) (int64, int64, int64, bool, string) {
		return 0, maximum, 0, true, ""
	}
	server.armRestartFreeze(now)

	// A non-blocking (max_wait_ms==0), pinned request: it enqueues, a frozen pass
	// leaves it queued recording no contention, then the zero deadline fires.
	args := map[string]any{"slice": "slice", "reserve": int64(1) << 30, "max_wait_ms": int64(0), "pinned": true}
	run := startSaturatedAdmit(t, server, maximum, args)
	run.pass()
	rejection := run.reject()

	if rejection.Contention != "unevaluated" {
		t.Fatalf("frozen non-blocking rejection Contention=%q, want \"unevaluated\" — a frozen slice is not saturated, and a fabricated reading is the honesty defect Inv 6 forbids", rejection.Contention)
	}
	if rejection.Grantable != nil {
		t.Fatalf("frozen rejection carries a Grantable figure (%d); the freeze gate must record none", *rejection.Grantable)
	}
}

// verifies: S11 — restartFrozenAt is a pure wall-clock predicate; unarmed (0) is never
// frozen, and it stops exactly at the freeze-end instant.
func TestRestartFrozenAtBoundary(t *testing.T) {
	server := NewServer(Paths{})
	now := time.Unix(7000, 0)
	if server.restartFrozenAt(now) {
		t.Fatal("unarmed (0) must never be frozen")
	}
	server.admitNow = func() time.Time { return now }
	server.armRestartFreeze(now)
	if !server.restartFrozenAt(now) {
		t.Fatal("at arm time the freeze must be active")
	}
	if !server.restartFrozenAt(now.Add(server.restartFreeze - time.Nanosecond)) {
		t.Fatal("one tick before freeze-end must still be frozen")
	}
	if server.restartFrozenAt(now.Add(server.restartFreeze)) {
		t.Fatal("at freeze-end the freeze must be over")
	}
	// A zero restartFreeze arms nothing.
	server2 := NewServer(Paths{})
	server2.restartFreeze = 0
	server2.armRestartFreeze(now)
	if server2.restartFrozenAt(now) {
		t.Fatal("restartFreeze<=0 must arm no freeze")
	}
}
