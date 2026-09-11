package daemon

import (
	"os"
	"sync/atomic"
	"testing"
	"time"

	"aira/internal/testdeadline"
)

func waitForLeaseCount(server *Server, want int, timeout time.Duration) bool {
	deadline := time.Now().Add(testdeadline.Wait(timeout))
	for time.Now().Before(deadline) {
		if len(reloadQueueWaiters(server)) == want {
			return true
		}
		time.Sleep(time.Millisecond)
	}
	return len(reloadQueueWaiters(server)) == want
}

// verifies: S11 mutation (a), THE KEY ONE (design §4 gate P1-A). A reloaded lease whose
// recorded pid is ALIVE (this process, with its REAL start-tick) is KEPT by the
// kill-probe, so ONLY the end-of-freeze+grace drop timer can remove it. This is the
// retired-worker-under-a-live-supervisor-pid case: kill -0 alone leaks it forever, and
// the drop timer is the actual safety. Driven through Serve + the restartAfter seam (not
// the direct dropUnanchoredLeases call) so disabling the TIMER reds it.
//
// MUTATION: remove s.dropUnanchoredLeases() from runRestartFreeze -> the live-pid lease
// is never dropped -> the final wait times out and this reds.
func TestRestartTimerDropsLiveSupervisorPidLease(t *testing.T) {
	tick, ok, _ := readProcStartTime(os.Getpid())
	if !ok {
		t.Skip("cannot read this process's /proc start-tick; the kill-probe keep-path needs it")
	}
	server, paths := serveDumpTestServer(t)

	// Control both timer phases deterministically via the restartAfter seam.
	phase1 := make(chan time.Time)
	phase2 := make(chan time.Time)
	var calls int32
	server.restartAfter = func(time.Duration) <-chan time.Time {
		if atomic.AddInt32(&calls, 1) == 1 {
			return phase1
		}
		return phase2
	}

	// A dump with one ALIVE-pid, real-tick lease, never re-declared.
	rec := leaseDumpRecord{
		Frame:            reDeclareRecord{ScopeID: "CONFINE-drop@s", RAMBytes: 2 << 30, CPUCores: 1},
		ClientPID:        os.Getpid(),
		ProcessStartTick: tick,
	}
	data, err := encodeLeaseDump(time.Now(), []leaseDumpRecord{rec})
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	// Serve creates RuntimeDir, but the reload reads the dump before that; create it here.
	if err := os.MkdirAll(paths.RuntimeDir, 0o700); err != nil {
		t.Fatalf("mkdir runtime dir: %v", err)
	}
	if err := os.WriteFile(leaseDumpPath(paths), data, 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}

	cancel, done := startServeInline(t, server)
	defer func() { cancel(); awaitShutdown(t, done) }()

	// Ready: the lease was reloaded and KEPT (a live pid + matching tick).
	if !waitForLeaseCount(server, 1, 2*time.Second) {
		t.Fatal("reloaded lease was not seeded/kept by the probe before the timer fired")
	}
	// Fire freeze-end, then the grace -> the drop scan.
	close(phase1)
	close(phase2)
	// The still-unanchored live-pid lease MUST be dropped: the timer is the safety.
	if !waitForLeaseCount(server, 0, 2*time.Second) {
		t.Fatal("the live-supervisor-pid lease was NOT dropped at end-of-freeze+grace; kill -0 keeps it, so the DROP TIMER must be the safety")
	}
}

// dropRaceQueue registers a queue holding one seeded UNANCHORED lease plus one anchored
// lease (so the queue is never emptied+pruned by the drop, keeping the test free of a
// background evaluator goroutine). Returns the queue and the seeded unanchored lease.
func dropRaceQueue(t *testing.T, server *Server, scopeID string, reserve int64) (*sliceQueue, *admitWaiter) {
	t.Helper()
	keep := &admitWaiter{
		seq: 1, reserve: 1 * gib, cpu: 1, state: admitGranted, accounted: true,
		grantedCh: closedCh(), scopeID: "CONFINE-keep@s", basis: "reload",
		anchor: testAnchorConn(),
	}
	seeded := &admitWaiter{
		seq: 2, reserve: reserve, cpu: 1, state: admitGranted, accounted: true,
		grantedCh: closedCh(), scopeID: scopeID, basis: "reload",
		anchor: nil, unanchored: true,
	}
	queue := &sliceQueue{
		path: "/slice", server: server, kick: make(chan struct{}, 1), stop: make(chan struct{}),
		waiters: []*admitWaiter{keep, seeded},
	}
	queue.outstanding, queue.cpuOutstanding, queue.outstandingJobs = rederiveLedgerLocked(queue)
	registerAdmitQueue(server, queue)
	return queue, seeded
}

// verifies: S11 mutation (d), the S8 drop-race. Reading waiter.unanchored AND dropping
// are ONE queue.mu critical section, and anchorLeaseLocked clears the bit under the same
// lock, so a re-declare and the drop scan serialise. Both orderings are pinned:
//
//   - re-declare THEN drop: the re-anchored lease (unanchored cleared) SURVIVES.
//   - drop THEN re-declare: the drop removes the unanchored lease; the re-declare then
//     establishes it fresh (S9 absent-lease establish). Either way exactly one live
//     lease for the scope, and the ledger is exact (no double-count, no lost charge).
//
// MUTATION: drop regardless of the unanchored bit -> ordering 1 wrongly drops the
// re-anchored lease and reds.
func TestDropUnanchoredSerialisesWithReDeclare(t *testing.T) {
	var maximum atomic.Int64
	maximum.Store(100 << 30)

	t.Run("re-declare before drop survives", func(t *testing.T) {
		server := admitTestServer(&maximum)
		conn := testAnchorConn()
		defer conn.Close()
		queue, seeded := dropRaceQueue(t, server, "CONFINE-r@s", 2*gib)

		_, got, code, err := server.enqueueReDeclare("/slice", 2*gib, "redeclare", admitRequest{
			scopeID: "CONFINE-r@s", peerSameUID: true, conn: conn, cpu: 1,
		})
		if err != nil || code != "" {
			t.Fatalf("re-declare refused: code=%q err=%v", code, err)
		}
		if got != seeded || seeded.unanchored {
			t.Fatalf("re-declare must re-anchor the seeded lease in place (unanchored cleared): got==seeded=%v unanchored=%v", got == seeded, seeded.unanchored)
		}

		server.dropUnanchoredLeases()

		waiters := reloadQueueWaiters(server)
		if len(waiters) != 2 {
			t.Fatalf("after re-declare+drop: %d leases, want 2 (keep + the re-anchored r) — the drop must NOT remove a lease re-anchored mid-scan", len(waiters))
		}
		if out, _, jobs := queueLedger(queue); out != 3*gib || jobs != 2 {
			t.Fatalf("ledger outstanding=%d jobs=%d, want 3Gi/2 (no lost charge)", out, jobs)
		}
	})

	t.Run("drop before re-declare re-establishes without double-count", func(t *testing.T) {
		server := admitTestServer(&maximum)
		conn := testAnchorConn()
		defer conn.Close()
		queue, _ := dropRaceQueue(t, server, "CONFINE-r@s", 2*gib)

		server.dropUnanchoredLeases() // removes the unanchored r; keep survives
		if out, _, jobs := queueLedger(queue); out != 1*gib || jobs != 1 {
			t.Fatalf("after drop: outstanding=%d jobs=%d, want 1Gi/1 (only keep)", out, jobs)
		}

		_, _, code, err := server.enqueueReDeclare("/slice", 2*gib, "redeclare", admitRequest{
			scopeID: "CONFINE-r@s", peerSameUID: true, conn: conn, cpu: 1,
		})
		if err != nil || code != "" {
			t.Fatalf("re-declare after drop refused: code=%q err=%v", code, err)
		}
		waiters := reloadQueueWaiters(server)
		if len(waiters) != 2 {
			t.Fatalf("after drop+re-declare: %d leases, want 2 (keep + freshly established r), no double", len(waiters))
		}
		if out, _, jobs := queueLedger(queue); out != 3*gib || jobs != 2 {
			t.Fatalf("ledger outstanding=%d jobs=%d, want 3Gi/2 (re-established once, not doubled)", out, jobs)
		}
	})
}

// writeReloadDump writes a single-record restart dump (alive pid + real tick) for a
// Serve-driven reload test, creating RuntimeDir (the reload reads the dump before Serve's
// own MkdirAll would run — see server.go).
func writeReloadDump(t *testing.T, paths Paths, scopeID string) {
	t.Helper()
	tick, ok, _ := readProcStartTime(os.Getpid())
	if !ok {
		t.Skip("cannot read this process's /proc start-tick; the kill-probe keep-path needs it")
	}
	rec := leaseDumpRecord{
		Frame:            reDeclareRecord{ScopeID: scopeID, RAMBytes: 2 << 30, CPUCores: 1},
		ClientPID:        os.Getpid(),
		ProcessStartTick: tick,
	}
	data, err := encodeLeaseDump(time.Now(), []leaseDumpRecord{rec})
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	if err := os.MkdirAll(paths.RuntimeDir, 0o700); err != nil {
		t.Fatalf("mkdir runtime dir: %v", err)
	}
	if err := os.WriteFile(leaseDumpPath(paths), data, 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
}

// verifies: S11 C2 / Fable P2-1 — the unanchored-drop SAFETY is NOT coupled to the freeze
// being armed. With restartFreeze=0 (freeze disabled) a reloaded dump still seeds
// unanchored leases (reloadLeaseDump is not gated on the freeze), which must still be
// dropped at grace. runRestartFreeze runs UNCONDITIONALLY.
//
// MUTATION: restore the `if restartFreezeUntilNanos.Load()==0 { return }` guard in
// runRestartFreeze -> with the freeze disabled the drop never runs -> the live-pid lease
// is never dropped -> the final wait times out and this reds.
func TestRestartDropRunsWithFreezeDisabled(t *testing.T) {
	server, paths := serveDumpTestServer(t)
	server.restartFreeze = 0 // freeze DISABLED; the drop safety must still run
	phase1 := make(chan time.Time)
	phase2 := make(chan time.Time)
	var calls int32
	server.restartAfter = func(time.Duration) <-chan time.Time {
		if atomic.AddInt32(&calls, 1) == 1 {
			return phase1
		}
		return phase2
	}
	writeReloadDump(t, paths, "CONFINE-nofreeze@s")

	cancel, done := startServeInline(t, server)
	defer func() { cancel(); awaitShutdown(t, done) }()

	if !waitForLeaseCount(server, 1, 2*time.Second) {
		t.Fatal("lease was not seeded with the freeze disabled — reloadLeaseDump must not be gated on the freeze")
	}
	close(phase1)
	close(phase2)
	if !waitForLeaseCount(server, 0, 2*time.Second) {
		t.Fatal("the unanchored lease was NOT dropped with the freeze disabled — the drop safety is wrongly coupled to the freeze arm")
	}
}

// verifies: S11 C1 (concurrency review) — a reloaded UNANCHORED lease never re-declared
// must not leak its queue's evaluator goroutine across shutdown. It is the only waiter
// class with no connection handler, so without the shutdown-time drop its queue stays
// non-empty, pruneAdmitRegistry never closes queue.stop, and runEvaluator outlives Serve.
// A graceful shutdown BEFORE the freeze+grace drop timer fires must still stop it.
//
// MUTATION: remove s.dropUnanchoredLeases() from the shutdown path in Serve -> queue.stop
// is never closed -> queue.stopped never closes -> this reds (the bounded wait times out).
func TestRestartShutdownBeforeGraceStopsEvaluator(t *testing.T) {
	server, paths := serveDumpTestServer(t)
	// Never fire the grace seam: the timer's own drop must NOT be what saves us.
	server.restartAfter = func(time.Duration) <-chan time.Time { return make(chan time.Time) }
	writeReloadDump(t, paths, "CONFINE-leak@s")

	cancel, done := startServeInline(t, server)
	if !waitForLeaseCount(server, 1, 2*time.Second) {
		cancel()
		awaitShutdown(t, done)
		t.Fatal("reloaded lease was not seeded")
	}
	// Capture the queue before shutdown prunes it from the registry.
	server.admitRegistryMu.Lock()
	queue := server.admitQueues["/slice"]
	server.admitRegistryMu.Unlock()
	if queue == nil {
		cancel()
		awaitShutdown(t, done)
		t.Fatal("no /slice queue after reload")
	}

	// Shut down BEFORE the grace timer fires: only the shutdown-path drop can empty the
	// queue and let pruneAdmitRegistry close queue.stop so runEvaluator exits.
	cancel()
	awaitShutdown(t, done)

	select {
	case <-queue.stopped:
	case <-testdeadline.After(2 * time.Second):
		t.Fatal("the reloaded-lease queue's evaluator did not stop after shutdown — a never-re-declared unanchored lease leaked its evaluator (queue.stop never closed)")
	}
}
