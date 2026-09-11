//go:build linux

package daemon

import (
	"context"
	"io"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"aira/internal/cgrouptest"
	"aira/internal/core"
	"aira/internal/runner"
	"aira/internal/testdeadline"
)

// S13 5b — the NO-DUMP restart merge gate (design §4, gate P1-A). End-to-end proof,
// against a REAL cgroup and REAL daemons, that after a daemon restart WITH NO LEASE DUMP
// the correctness is carried entirely by the client keeper's reconnect + re-declare:
//
//	daemon A holds leases -> A graceful-stops (writes NO dump) -> daemon B starts EMPTY on
//	the same socket with the 2s freeze armed -> each SURVIVING keeper reconnects and
//	re-declares (establish-granted, S9) -> B's ledger holds EXACTLY the survivors, each
//	charged EXACTLY its grant reserve.
//
// The client is the REAL runner.Confine (real admit -> real leaseKeeper -> real cgroup
// child), driven in-process so the keeper runs here: a keeper-source mutation is picked up
// by recompilation, which is what makes the three correctness mutations below RED.
//
// "Absent" is now KEEPER-DRIVEN, not kill-probe-driven (S13 deleted the kill-probe): a
// lease is absent in B purely because ITS KEEPER NEVER RE-DECLARED. The keeper watches the
// DAEMON connection, not the cgroup child, so a non-survivor is modelled by tearing its
// confine down (ctx-cancel -> supervisor teardown -> keeper Close()) so it stops
// re-declaring — NOT by killing its child (a live keeper would re-declare a dead-child
// lease into B).
//
// MUTATIONS that must RED this gate (client keeper, internal/runner/lease_keeper_linux.go),
// verified by hand and reverted:
//   - (a) re-add a fail-open on daemon EOF (reconnectLoop returns instead of
//     reconnectAndReDeclare) -> the survivor never re-declares -> B missing it -> RED.
//   - (b) disable the keeper reconnect (reconnectAndReDeclare returns false immediately)
//     -> survivor absent -> Σ wrong -> RED.
//   - (c) the keeper sends a TRANSFORMED scope_id (not verbatim req.ConfineScopeID) -> a
//     lease establishes in B under the WRONG key -> the key-set assertion RED. (In the
//     no-dump world B starts EMPTY, so a phantom lands under a wrong key with the SAME
//     reserve — Σ stays equal; only the verbatim-key assertion catches it. This is why the
//     gate asserts BOTH Σ and the exact scope-id key-set — a deviation from the "Σ doubles"
//     framing, which held only when a correct-key lease pre-existed to double against.)
const (
	mergeGateChildEnv = "AIRA_MERGE_GATE_CHILD"
	// The pinned reserve is the scope memory.max for a non-delegate confine, so it must be
	// comfortably above a sleeping re-exec'd test binary's working set. 256 MiB is ample
	// (the OOM-selfheal seeds ran a 40 MiB workload under a 256 MiB cap).
	mergeGateReserve = int64(256 << 20)
	// A LIMIT, not an allocation (the throwaway isolated slice's memory.max): generous
	// enough for two pinned reserves plus the scaled headroom, with the whole margin spare.
	mergeGateSliceMax = int64(2 << 30)
)

// TestMergeGateChildSleeps is the re-exec'd confine workload (the TestSliceCeilingAllocHelper
// pattern). It stays resident so its lease is held across the daemon restart; the test ends
// it by cancelling its confine ctx (cgroup.kill), and the bounded sleep is only a safety net.
func TestMergeGateChildSleeps(t *testing.T) {
	if os.Getenv(mergeGateChildEnv) != "1" {
		return
	}
	time.Sleep(90 * time.Second)
	os.Exit(0)
}

func mergeGateSlice(t *testing.T) string {
	t.Helper()
	parent := cgrouptest.IsolatedScopeParent(t)
	if err := os.WriteFile(filepath.Join(parent, "cgroup.subtree_control"), []byte("+memory"), 0o644); err != nil {
		cgrouptest.SkipOrFailRealCgroup(t, "memory controller not delegated to %s: %v", parent, err)
	}
	slice := filepath.Join(parent, "slice")
	if err := os.Mkdir(slice, 0o755); err != nil {
		cgrouptest.SkipOrFailRealCgroup(t, "create fixture slice cgroup: %v", err)
	}
	if err := os.WriteFile(filepath.Join(slice, "memory.max"), []byte(strconv.FormatInt(mergeGateSliceMax, 10)), 0o644); err != nil {
		cgrouptest.SkipOrFailRealCgroup(t, "fixture slice memory.max is not writable: %v", err)
	}
	return slice
}

// startGateServer starts a daemon on paths with real slice resolution (runner.Confine
// creates a real cgroup, so the daemon must read the real slice) and fixture-scaled
// headroom. It returns the server plus a manual cancel+done so the test controls the A->B
// restart itself; the caller is responsible for shutting it down.
func startGateServer(t *testing.T, paths Paths) (*Server, context.CancelFunc, <-chan error) {
	t.Helper()
	server := NewServer(paths)
	server.admitSliceHeadroomBase = 32 << 20
	server.admitSliceHeadroomSupervisor = 8 << 20
	server.admitPollInterval = 5 * time.Millisecond
	ctx, cancel := context.WithCancel(context.Background())
	ready := make(chan struct{}, 1)
	server.Ready = ready
	done := make(chan error, 1)
	go func() { done <- server.Serve(ctx) }()
	select {
	case <-ready:
	case err := <-done:
		cancel()
		t.Fatalf("daemon exited before ready: %v", err)
	case <-testdeadline.After(5 * time.Second):
		cancel()
		t.Fatal("daemon did not become ready")
	}
	return server, cancel, done
}

func awaitGateShutdown(t *testing.T, done <-chan error) {
	t.Helper()
	select {
	case <-done:
	case <-testdeadline.After(10 * time.Second):
		t.Fatal("daemon did not shut down")
	}
}

// gateLeases walks every admit queue and returns the granted+accounted leases as
// scope_id -> reserve. The gate uses one slice, so this is that slice's ledger.
func gateLeases(server *Server) map[string]int64 {
	server.admitRegistryMu.Lock()
	queues := make([]*sliceQueue, 0, len(server.admitQueues))
	for _, q := range server.admitQueues {
		queues = append(queues, q)
	}
	server.admitRegistryMu.Unlock()
	out := map[string]int64{}
	for _, q := range queues {
		q.mu.Lock()
		for _, w := range q.waiters {
			if w != nil && w.state == admitGranted && w.accounted {
				out[w.scopeID] = w.reserve
			}
		}
		q.mu.Unlock()
	}
	return out
}

func waitGateLeaseCount(server *Server, want int, timeout time.Duration) map[string]int64 {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if leases := gateLeases(server); len(leases) == want {
			return leases
		}
		time.Sleep(10 * time.Millisecond)
	}
	return gateLeases(server)
}

// gateFreshAdmitRefusedDuringFreeze dials a fresh NEW admission at B during the restart
// freeze and asserts it is NOT granted. It uses NON-BLOCKING mode (max_wait_ms==0): since
// S13 3/5 a BLOCKING admit has no daemon deadline (it would simply wait through the 2s
// freeze and then grant), so a non-blocking probe is the deterministic way to observe the
// freeze — during the window it returns a saturated/unevaluated REJECTION, never a grant
// (TestRestartFreezeNonBlockingReturnsUnevaluated pins the same). Only re-declares are
// freeze-exempt; a fresh admit must never be granted inside the window.
func gateFreshAdmitRefusedDuringFreeze(t *testing.T, socket, slice string) {
	t.Helper()
	conn, err := net.Dial("unix", socket)
	if err != nil {
		t.Fatalf("dial B for the fresh admit: %v", err)
	}
	defer conn.Close()
	args := map[string]any{"slice": slice, "reserve": int64(8 << 20), "max_wait_ms": int64(0), "signature": "", "pinned": true}
	frame := RequestFrame{Proto: ProtocolVersion, Request: core.Request{Verb: "admit", Args: args}}
	if err := writeFrame(conn, frame); err != nil {
		t.Fatalf("write the fresh admit frame: %v", err)
	}
	var resp ResponseFrame
	if err := readFrame(conn, &resp); err != nil {
		t.Fatalf("read the fresh admit response: %v", err)
	}
	if resp.OK {
		t.Fatal("a fresh NEW admission was GRANTED during the restart freeze; the freeze must hold new admits (only re-declares are freeze-exempt)")
	}
}

// verifies: S13 5b restart merge gate (design §4).
func TestRestartMergeGateSurvivorReDeclaresNonSurvivorAbsent(t *testing.T) {
	slice := mergeGateSlice(t)
	paths := testPaths(t)

	serverA, cancelA, doneA := startGateServer(t, paths)

	confineReq := func(name string) runner.ConfineRequest {
		return runner.ConfineRequest{
			Slice: slice, RuntimeDir: paths.RuntimeDir, AdmitSocketPath: paths.SocketPath,
			Owner: "session-gate", Name: name,
			SelfPath: os.Args[0], Argv: []string{os.Args[0], "-test.run=^TestMergeGateChildSleeps$", name},
			Env:                 append(os.Environ(), mergeGateChildEnv+"=1"),
			MemoryReserve:       mergeGateReserve,
			MemoryReservePinned: true,
			AdmissionMaxWait:    30 * time.Second,
			Stdin:               strings.NewReader(""), Stdout: io.Discard, Stderr: io.Discard,
		}
	}

	survCtx, survCancel := context.WithCancel(context.Background())
	defer survCancel()
	nonCtx, nonCancel := context.WithCancel(context.Background())
	defer nonCancel()

	survErr := make(chan error, 1)
	go func() { _, err := runner.Confine(survCtx, confineReq("survivor")); survErr <- err }()
	nonErr := make(chan error, 1)
	go func() { _, err := runner.Confine(nonCtx, confineReq("nonsurvivor")); nonErr <- err }()

	// Both leases established on A — or skip if the real cgroup is unavailable (a confine
	// that cannot create its scope returns promptly with the reason).
	deadline := time.Now().Add(testdeadline.Wait(20 * time.Second))
	for len(gateLeases(serverA)) != 2 {
		select {
		case err := <-survErr:
			cancelA()
			cgrouptest.SkipOrFailRealCgroup(t, "survivor confine unavailable: %v", err)
		case err := <-nonErr:
			cancelA()
			cgrouptest.SkipOrFailRealCgroup(t, "non-survivor confine unavailable: %v", err)
		default:
		}
		if time.Now().After(deadline) {
			cancelA()
			t.Fatalf("both confine leases did not establish on A: got %v", gateLeases(serverA))
		}
		time.Sleep(10 * time.Millisecond)
	}

	// NON-SURVIVOR: tear its confine down BEFORE the restart (ctx-cancel -> keeper Close()),
	// so it stops re-declaring. A's ledger drops to just the survivor.
	nonCancel()
	select {
	case <-nonErr:
	case <-testdeadline.After(15 * time.Second):
		cancelA()
		t.Fatal("the non-survivor confine did not tear down after ctx-cancel")
	}
	leasesA := waitGateLeaseCount(serverA, 1, 10*time.Second)
	if len(leasesA) != 1 {
		cancelA()
		t.Fatalf("A holds %d leases after the non-survivor torn down, want exactly the 1 survivor: %v", len(leasesA), leasesA)
	}
	var survScope string
	var survReserve int64
	for id, r := range leasesA {
		survScope, survReserve = id, r
	}
	if survReserve != mergeGateReserve {
		cancelA()
		t.Fatalf("survivor reserve on A = %d, want the pinned %d", survReserve, mergeGateReserve)
	}

	// Graceful-stop A. It writes NO dump now; close(stopping) releases the survivor's
	// (anchored) lease and closes its connection, which its keeper reads as the restart EOF.
	cancelA()
	awaitGateShutdown(t, doneA)

	// Daemon B on the SAME socket: an EMPTY ledger with the 2s freeze armed at listen-ready.
	serverB, cancelB, doneB := startGateServer(t, paths)
	defer func() { cancelB(); awaitGateShutdown(t, doneB) }()

	if pre := gateLeases(serverB); len(pre) != 0 {
		t.Fatalf("B started with a non-empty ledger %v; the no-dump restart must open empty", pre)
	}

	// The freeze holds a fresh NEW admit (it is not granted inside the window).
	gateFreshAdmitRefusedDuringFreeze(t, paths.SocketPath, slice)

	// The survivor's keeper reconnects to B and re-declares -> establish-granted (freeze-exempt).
	leasesB := waitGateLeaseCount(serverB, 1, 20*time.Second)

	// CORE ASSERTIONS.
	if len(leasesB) != 1 {
		t.Fatalf("B holds %d leases, want exactly the 1 survivor: %v", len(leasesB), leasesB)
	}
	bReserve, present := leasesB[survScope]
	if !present {
		t.Fatalf("B's ledger key-set %v does not carry the survivor's VERBATIM scope_id %q — a transformed scope_id (mutation c) establishes a phantom under a different key", leasesB, survScope)
	}
	if bReserve != survReserve {
		t.Fatalf("survivor reserve in B = %d, want exactly %d (no drift, no double-count)", bReserve, survReserve)
	}
	var sum int64
	for _, r := range leasesB {
		sum += r
	}
	if sum != survReserve {
		t.Fatalf("Σleases(B) = %d, want exactly the survivor's %d — the non-survivor must be ABSENT and the survivor never double-counted", sum, survReserve)
	}

	// End the survivor cleanly (cgroup.kill via ctx-cancel) so no child leaks.
	survCancel()
	select {
	case <-survErr:
	case <-testdeadline.After(15 * time.Second):
		t.Error("the survivor confine did not return after ctx-cancel")
	}
}
