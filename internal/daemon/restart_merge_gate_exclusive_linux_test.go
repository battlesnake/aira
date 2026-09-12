//go:build linux

package daemon

import (
	"context"
	"io"
	"os"
	"strings"
	"testing"
	"time"

	"aira/internal/cgrouptest"
	"aira/internal/runner"
	"aira/internal/testdeadline"
)

// S18 merge gate — exclusive=lost across a daemon restart (design §4 / §15 P2-D).
//
// This is the confine-side sibling of the worker-path merge gate
// (internal/pylib/restart_merge_gate_worker_e2e_test.go). It reuses the S13
// restart-gate scaffolding verbatim (startGateServer / mergeGateSlice /
// gateLeases / waitGateLeaseCount / TestMergeGateChildSleeps) and drives a REAL
// runner.Confine `--exclusive` holder in-process, against a REAL cgroup and REAL
// daemons.
//
// The one property under test is the exclusive branch of the keeper: unlike a
// non-exclusive survivor (whose keeper reconnects + re-declares its ARDR frame
// to re-anchor into the new daemon), an EXCLUSIVE lease is deliberately LOST on
// a restart — exclusivity cannot be re-established, since the new daemon starts
// empty and has no way to know this holder was scheduled alone (§15 P2-D). So
// its keeper (watchExclusive) records exclusive=lost on the held connection's
// restart-EOF and NEVER reconnects. Two observable consequences, both asserted:
//
//  1. the exclusive lease is ABSENT in daemon B's ledger (it never re-declares);
//  2. runner.Confine reports Status.Exclusive == ConfineExclusiveLost.
//
// MUTATION that must RED this gate: make the exclusive keeper reconnect like a
// normal survivor (newLeaseKeeper stops treating req.Exclusive as hold-only, so
// it builds an ARDR frame and starts the reconnect loop). Its lease would then
// re-anchor in B and Status.Exclusive would stay granted — both assertions here
// flip. Verified by hand and reverted (see the worker-path gate's mutation log).
//
// verifies: S18 restart merge gate, exclusive=lost across restart (design §4).
func TestRestartMergeGateExclusiveLostAcrossRestart(t *testing.T) {
	slice := mergeGateSlice(t)
	paths := testPaths(t)

	serverA, cancelA, doneA := startGateServer(t, paths)

	exCtx, exCancel := context.WithCancel(context.Background())
	defer exCancel()
	type confineOutcome struct {
		result runner.ConfineResult
		err    error
	}
	outCh := make(chan confineOutcome, 1)
	go func() {
		r, err := runner.Confine(exCtx, runner.ConfineRequest{
			Slice: slice, RuntimeDir: paths.RuntimeDir, AdmitSocketPath: paths.SocketPath,
			Owner: "session-gate", Name: "exclusive-holder",
			SelfPath: os.Args[0], Argv: []string{os.Args[0], "-test.run=^TestMergeGateChildSleeps$", "exclusive-holder"},
			Env:                 append(os.Environ(), mergeGateChildEnv+"=1"),
			MemoryReserve:       mergeGateReserve,
			MemoryReservePinned: true,
			Exclusive:           true,
			AdmissionMaxWait:    30 * time.Second,
			Stdin:               strings.NewReader(""), Stdout: io.Discard, Stderr: io.Discard,
		})
		outCh <- confineOutcome{r, err}
	}()

	// The exclusive lease establishes on A — or skip if the real cgroup is
	// unavailable (a confine that cannot create its scope returns promptly).
	deadline := time.Now().Add(testdeadline.Wait(20 * time.Second))
	for len(gateLeases(serverA)) != 1 {
		select {
		case o := <-outCh:
			cancelA()
			cgrouptest.SkipOrFailRealCgroup(t, "exclusive confine unavailable: %v", o.err)
		default:
		}
		if time.Now().After(deadline) {
			cancelA()
			t.Fatalf("exclusive lease did not establish on A: got %v", gateLeases(serverA))
		}
		time.Sleep(10 * time.Millisecond)
	}
	var exScope string
	for id := range gateLeases(serverA) {
		exScope = id
	}

	// Graceful-stop A. close(stopping) releases the lease and closes the parked
	// connection, which the exclusive keeper's watchExclusive reads as the
	// restart-EOF: it records exclusive=lost and does NOT reconnect.
	cancelA()
	awaitGateShutdown(t, doneA)

	// Daemon B on the SAME socket: an EMPTY ledger with the 2s freeze armed. The
	// exclusive holder never re-declares, so its lease must be ABSENT here.
	serverB, cancelB, doneB := startGateServer(t, paths)
	defer func() { cancelB(); awaitGateShutdown(t, doneB) }()

	if pre := gateLeases(serverB); len(pre) != 0 {
		t.Fatalf("B started with a non-empty ledger %v; the no-dump restart must open empty", pre)
	}

	// Give a (wrong) reconnect the same real-time window a non-exclusive
	// survivor's re-anchor gets. waitGateLeaseCount returns the instant the count
	// is reached, so a mutant that DID re-anchor is caught fast; the full wait
	// elapses only in the correct case, where the ledger stays empty.
	leasesB := waitGateLeaseCount(serverB, 1, testdeadline.Wait(3*time.Second))
	if len(leasesB) != 0 {
		t.Fatalf("B holds %d leases; an exclusive holder must NOT re-anchor across a restart: %v", len(leasesB), leasesB)
	}
	if _, present := leasesB[exScope]; present {
		t.Fatalf("B re-anchored the exclusive scope %q; exclusivity cannot be re-established across a restart", exScope)
	}

	// End the child (cgroup.kill via ctx-cancel); runner.Confine returns and must
	// report exclusive=lost — the lease closed on the restart and was never
	// re-established.
	exCancel()
	select {
	case o := <-outCh:
		if o.result.Status.Exclusive != runner.ConfineExclusiveLost {
			t.Fatalf("exclusive status = %q, want %q (the admission lease closed on the daemon restart and never re-anchored)",
				o.result.Status.Exclusive, runner.ConfineExclusiveLost)
		}
	case <-testdeadline.After(15 * time.Second):
		t.Fatal("the exclusive confine did not return after ctx-cancel")
	}
}
