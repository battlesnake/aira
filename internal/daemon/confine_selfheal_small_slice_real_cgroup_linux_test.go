//go:build linux

package daemon

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"aira/internal/cgrouptest"
	"aira/internal/runner"
)

// AIRA-153, at the kernel. AIRA-128 proved the cold-start self-heal against a
// REAL cgroup on a slice LARGER than the unpinned default. This file proves the
// two properties that only exist on a slice SMALLER than it, which is where the
// defect this ticket fixes lives and where AIRA-151 recorded the self-heal as
// broken:
//
//   - T9: the ladder converges. A novel command is admitted at the largest prior
//     the slice can grant, is OOM-killed there, and the very next identical
//     invocation succeeds with no operator action -- on a slice where master
//     refuses every unpinned job outright.
//   - T13: the ladder TERMINATES honestly. A command whose own recorded OOM peak
//     is already at or above what the slice can grant is refused immediately with
//     E_ADMIT_TOO_LARGE naming both numbers, rather than admitted onto an
//     ungrantable ceiling and left to wait out its whole window.
//
// aira.slice is never touched: the whole fixture lives under a throwaway
// cgrouptest.IsolatedScopeParent, torn down in t.Cleanup.
const (
	// The T9 fixture slice. DERIVED, not picked: one whole GiB BELOW
	// runner.DefaultConfineMemoryReserve, so the unpinned prior is unambiguously
	// over this slice's ceiling and the fit is what is being exercised. Its
	// margins are asserted by TestSmallSliceSelfHealFixtureActuallyExercisesTheFit
	// AndStaysOffTheCeiling rather than left to a reader.
	smallSliceMax = runner.DefaultConfineMemoryReserve - (1 << 30) // 3 GiB
	// The T9 entry ceiling and the reserve the fit produces for it, spelled out
	// so a change to either constant fails against a number rather than against
	// a recomputation of itself.
	smallSliceCeiling = int64(3179282432) // smallSliceMax - (32 MiB + 8 MiB)
	smallSliceFit     = int64(2764593419) // FIT(smallSliceCeiling)

	// The T13 fixture. A 640 MiB slice, whose ceiling is 629145600 and whose fit
	// is 547083130.
	overFitSliceMax = int64(671088640)
	overFitCeiling  = int64(629145600)
	overFitFit      = int64(547083130)
	// Phase A's PINNED cap: deliberately ABOVE the fit by 58.3 MiB and BELOW the
	// ceiling by 20 MiB, so the kernel really kills the job there and
	// RecordConfinePeak really records a MaxOOMPeak above what the slice can
	// grant. Pinned rather than driven from an unpinned first run because an
	// unpinned kill would land a page or two BELOW the fit (R4/G1) and make the
	// assertion non-deterministic.
	overFitPin = int64(608174080) // 580 MiB
	// What phase A's workload asks for: more than the slice itself holds, so the
	// cap is certainly reached.
	overFitWorkloadBytes = int64(700 << 20)
)

// TestSmallSliceOOMSelfHealConvergesOnTheFittedBaseline is AIRA-153 T9: the
// ticket's headline property, proven at the kernel rather than in unit
// arithmetic.
//
// On master phase 3 is refused E_ADMIT_TOO_LARGE (`confine: ran=no`), because
// the unpinned 4 GiB prior alone exceeds this slice's ceiling, so the completion
// marker never appears -- the exact self-heal AIRA-151 recorded as broken and
// deferred to this ticket.
//
// verifies: AIRA-153 §6, §3.6
func TestSmallSliceOOMSelfHealConvergesOnTheFittedBaseline(t *testing.T) {
	slice := newSmallSliceFixture(t, smallSliceMax)
	paths := testPaths(t)
	server := NewServer(paths)
	// The AIRA-128 fixture's scaled headroom. Scaled, NOT disabled: every
	// admission here is still sized against a real maximum-minus-headroom
	// ceiling, and a request over it is still terminally refused.
	server.admitSliceHeadroomBase = 32 << 20
	server.admitSliceHeadroomSupervisor = 8 << 20
	startServer(t, server)

	run := smallSliceRunner(t, slice, paths)

	// PHASE 1 -- establish a machine-wide p90 prior by actually running things.
	for i := 0; i < 3; i++ {
		seed := run("small-seed", oomSelfHealSeedBytes, 256<<20, 60*time.Second)
		if seed.err != nil {
			cgrouptest.SkipOrFailRealCgroup(t, "seeding confine run %d unavailable: %v (stderr %q)", i, seed.err, seed.stderr)
		}
		if seed.result.Exit != 0 || !strings.Contains(seed.stdout, oomSelfHealMarker) {
			t.Fatalf("seed run %d: exit=%d stdout=%q stderr=%q, want a completed workload", i, seed.result.Exit, seed.stdout, seed.stderr)
		}
	}
	server.admitPriorMu.Lock()
	server.admitPriorAt = time.Time{}
	server.admitPriorMu.Unlock()

	// PHASE 2 -- the cold start, capped at the machine-wide prior, far below what
	// the target needs. The prior itself is well under this slice's ceiling, so
	// it is NOT fitted: the basis must still be the bare `estimate:p90-prior`.
	first := run("small-target", oomSelfHealTargetBytes, 0, 90*time.Second)
	if first.err != nil {
		cgrouptest.SkipOrFailRealCgroup(t, "cold-start confine run unavailable: %v (stderr %q)", first.err, first.stderr)
	}
	if first.result.Status.ReserveBasis != "estimate:p90-prior" {
		t.Fatalf("cold-start reserve-basis=%q reserve=%d, want the UNFITTED p90 prior — a prior below the ceiling must not be touched (I4) (stderr %q)",
			first.result.Status.ReserveBasis, first.result.Status.ReserveBytes, first.stderr)
	}
	if strings.Contains(first.stdout, oomSelfHealMarker) {
		t.Fatalf("cold-start run printed its completion marker despite being capped at %d bytes: stdout=%q",
			first.result.Status.ScopeMemoryMax, first.stdout)
	}
	if first.result.Status.TerminatedBy != runner.ConfineTerminatedOOM || first.result.Exit != 137 {
		t.Fatalf("cold-start terminated-by=%q exit=%d, want %q/137 (stderr %q)",
			first.result.Status.TerminatedBy, first.result.Exit, runner.ConfineTerminatedOOM, first.stderr)
	}

	// The entry ceiling is `maximum - admitSliceHeadroom(outstandingJobs + 1)`,
	// so it is a function of CONCURRENT OCCUPANCY, and the previous phase's
	// admission is released asynchronously when its supervisor's connection
	// closes. Waiting for the ledger to drain is what makes phase 3's ceiling --
	// and therefore the exact fitted reserve asserted below -- deterministic
	// rather than a race with that release. (Measured: without this, phase 3
	// sometimes arrives with one job still outstanding and resolves against
	// 3170893824 instead of 3179282432.)
	waitForDrainedAdmitLedger(t, server)

	// PHASE 3 -- the self-heal. Same argv, therefore the same signature, and no
	// operator action of any kind between the two runs.
	//
	// The unpinned prior is over this slice's ceiling, so on master this request
	// never reaches a scope at all. Here it is fitted to the largest prior the
	// slice can grant, which is 8.6x what the workload needs, and the job runs.
	second := run("small-target", oomSelfHealTargetBytes, 0, 90*time.Second)
	if second.err != nil {
		t.Fatalf("second confine run: %v (stderr %q) — master refuses this with E_ADMIT_TOO_LARGE, which is the defect", second.err, second.stderr)
	}
	const wantBasis = "fallback:insufficient-samples:n=1,oom-on-record,ceiling-fitted"
	if second.result.Status.ReserveBasis != wantBasis {
		t.Fatalf("second-run reserve-basis=%q (reserve=%d), want %q",
			second.result.Status.ReserveBasis, second.result.Status.ReserveBytes, wantBasis)
	}
	if second.result.Status.ReserveBytes != smallSliceFit {
		t.Fatalf("second-run reserve=%d, want the fitted prior %d", second.result.Status.ReserveBytes, smallSliceFit)
	}
	// cgroup-v2 stores memory.max as (bytes / PAGE_SIZE) * PAGE_SIZE, so the
	// kernel-enforced cap is the fitted reserve floored to a page — the same
	// floor runner.floorMemoryPage applies. Asserting the raw figure here would
	// false-fail on every page size.
	wantCap := smallSliceFit &^ (int64(os.Getpagesize()) - 1)
	if second.result.Status.ScopeMemoryMax != wantCap {
		t.Fatalf("second-run scope memory.max=%d, want the fitted reserve %d floored to a page (%d) — the resolved reserve BECOMES the kernel-enforced cap",
			second.result.Status.ScopeMemoryMax, smallSliceFit, wantCap)
	}
	if second.result.Status.ScopeMemoryMax >= smallSliceCeiling {
		t.Fatalf("second-run cap %d is not strictly below the entry ceiling %d; a reserve equal to it is AIRA-150",
			second.result.Status.ScopeMemoryMax, smallSliceCeiling)
	}
	if second.result.Exit != 0 || second.result.Status.TerminatedBy != "normal" || !strings.Contains(second.stdout, oomSelfHealMarker) {
		t.Fatalf("second run exit=%d terminated-by=%q stdout=%q stderr=%q, want the same command to succeed at the fitted reserve",
			second.result.Exit, second.result.Status.TerminatedBy, second.stdout, second.stderr)
	}
}

// TestSmallSliceOOMAboveTheFittedCapIsTerminalAtTheKernel is AIRA-153 T13: the
// other end of the ladder, also proven at the kernel.
//
// A job whose own recorded OOM peak is at or above FIT(ceiling) has already been
// killed at the largest cap this slice can grant. Master clamps its escalation
// onto the entry ceiling, admits it, and the request then waits out its whole
// window before being refused E_ADMIT_SATURATED -- which tells an agent it is
// "owed a RETRY, nothing about the request is wrong". Because the job never
// runs, no new peak is recorded and the state is permanent. With the guard at the
// fit it is refused immediately with `required` and `cap_minus_headroom`.
//
// verifies: AIRA-153 §2.5, §3.3, §3.4 case 2
func TestSmallSliceOOMAboveTheFittedCapIsTerminalAtTheKernel(t *testing.T) {
	slice := newSmallSliceFixture(t, overFitSliceMax)
	paths := testPaths(t)
	server := NewServer(paths)
	server.admitSliceHeadroomBase = 32 << 20
	server.admitSliceHeadroomSupervisor = 8 << 20
	startServer(t, server)

	run := smallSliceRunner(t, slice, paths)

	// PHASE A -- a real kernel OOM at a PINNED cap deliberately above the fit.
	first := run("overfit-target", overFitWorkloadBytes, overFitPin, 90*time.Second)
	if first.err != nil {
		cgrouptest.SkipOrFailRealCgroup(t, "pinned confine run unavailable: %v (stderr %q)", first.err, first.stderr)
	}
	if first.result.Status.TerminatedBy != runner.ConfineTerminatedOOM || first.result.Exit != 137 {
		t.Fatalf("phase A terminated-by=%q exit=%d, want %q/137 — the fixture did not produce a real kernel OOM (stderr %q)",
			first.result.Status.TerminatedBy, first.result.Exit, runner.ConfineTerminatedOOM, first.stderr)
	}
	if first.result.Status.PeakRSS == nil {
		t.Fatalf("phase A peak RSS unestablished: the observation phase B's refusal is computed from never reached the daemon")
	}
	if peak := *first.result.Status.PeakRSS; peak <= overFitFit {
		t.Fatalf("phase A recorded peak %d, want it ABOVE the fit %d — the fixture is not modelling an OOM at or above what the slice can grant",
			peak, overFitFit)
	}

	// PHASE B -- the IDENTICAL argv, UNPINNED. The escalation from that peak
	// exceeds the ceiling and the guard refuses to cut it down, so this must be a
	// terminal refusal, immediately, and never a wait that ends in `saturated`.
	started := time.Now()
	second := run("overfit-target", overFitWorkloadBytes, 0, 30*time.Second)
	elapsed := time.Since(started)
	if second.err == nil {
		t.Fatalf("phase B was ADMITTED (exit=%d basis=%q cap=%d); a job already killed at or above what this slice can grant must be refused",
			second.result.Exit, second.result.Status.ReserveBasis, second.result.Status.ScopeMemoryMax)
	}
	if !strings.Contains(second.err.Error(), "E_ADMIT_TOO_LARGE") {
		t.Fatalf("phase B err=%v, want E_ADMIT_TOO_LARGE — E_ADMIT_SATURATED tells the agent to retry a request that cannot ever be satisfied", second.err)
	}
	if second.result.Status.ReserveBasis != "reject:too-large" {
		t.Fatalf("phase B reserve-basis=%q, want %q", second.result.Status.ReserveBasis, "reject:too-large")
	}
	if !strings.Contains(second.stderr, "ran=no") || !strings.Contains(second.stderr, "admission=too_large") {
		t.Fatalf("phase B stderr=%q, want the never-ran envelope naming admission=too_large", second.stderr)
	}
	if elapsed > 15*time.Second {
		t.Fatalf("phase B took %s against a 30s admission window; the refusal must be immediate, never a wait", elapsed)
	}
	if strings.Contains(second.stdout, oomSelfHealMarker) {
		t.Fatalf("phase B produced a completion marker although it was refused: stdout=%q", second.stdout)
	}
}

// TestSmallSliceSelfHealFixtureActuallyExercisesTheFitAndStaysOffTheCeiling is
// AIRA-153 T10: the unit guard on T9's and T13's fixture constants, the way
// TestOOMSelfHealFixtureStaysOffTheCeilingClamp guards AIRA-128's.
//
// It exists because both failures it guards are invisible in the fixtures
// themselves. A T9 slice that drifted ABOVE the unpinned default would stop
// exercising the fit at all and would still pass every assertion in T9 except
// the basis; a T9 ceiling too close to its own fit would make phase 3's
// admission depend on the slice's residual charge, which is the AIRA-139 flake
// in this fixture's terms. Both margins are named in the failure message.
//
// verifies: AIRA-153 §7.1 T10
func TestSmallSliceSelfHealFixtureActuallyExercisesTheFitAndStaysOffTheCeiling(t *testing.T) {
	server := NewServer(Paths{})
	server.stopping = make(chan struct{})
	server.admitSliceHeadroomBase = 32 << 20
	server.admitSliceHeadroomSupervisor = 8 << 20
	server.admitPeakP90 = func(context.Context) (int64, bool, error) {
		t.Fatal("the OOM branch must resolve before the machine-wide prior is consulted")
		return 0, false, nil
	}

	ceiling := smallSliceMax - server.admitSliceHeadroom(1)
	if ceiling != smallSliceCeiling {
		t.Fatalf("fixture ceiling=%d, want %d — the declared constant no longer matches the fixture's own arithmetic", ceiling, smallSliceCeiling)
	}
	fit := runner.SliceFittedReserve(ceiling)
	if fit != smallSliceFit {
		t.Fatalf("FIT(%d)=%d, want %d", ceiling, fit, smallSliceFit)
	}

	// (i) the fit actually fires: the ceiling is at or below the unpinned prior,
	// with a whole GiB of drift margin before it stops doing so.
	if ceiling > runner.DefaultConfineMemoryReserve {
		t.Fatalf("fixture ceiling %d is ABOVE the unpinned default %d, so the fit never fires and T9 proves nothing; drift margin was %d bytes",
			ceiling, runner.DefaultConfineMemoryReserve, runner.DefaultConfineMemoryReserve-smallSliceCeiling)
	}

	// (ii) the fitted reserve stays clear of the ceiling by more than one whole
	// target workload, so admission can never come to depend on the slice's
	// residual charge.
	if slack := ceiling - fit; slack < oomSelfHealTargetBytes {
		t.Fatalf("fitted reserve %d leaves %d below the fixture ceiling %d, want at least one whole target workload %d "+
			"(the fixture's designed margin is %d bytes, i.e. %d bytes of slack)",
			fit, slack, ceiling, oomSelfHealTargetBytes,
			smallSliceCeiling-smallSliceFit, (smallSliceCeiling-smallSliceFit)-oomSelfHealTargetBytes)
	}

	// (iii) the phase-3 resolution is exactly what T9 asserts, basis included.
	peak := oomSelfHealSeedBytes + oomSelfHealSeedBytes/2
	server.admitPeakHistory = func(context.Context, string) (runner.PeakRSSStats, error) {
		return runner.PeakRSSStats{TotalCount: 1, SampleCount: 1, PeakMax: peak, OOMCount: 1, MaxOOMPeak: peak}, nil
	}
	reserve, basis := server.resolveAdmitReserve(
		admitRequest{reserve: runner.DefaultConfineMemoryReserve, signature: "small-target"}, ceiling)
	if reserve != fit || basis != "fallback:insufficient-samples:n=1,oom-on-record,ceiling-fitted" {
		t.Fatalf("phase-3 resolution=%d/%q, want %d/%q — the fixture no longer models the run it guards",
			reserve, basis, fit, "fallback:insufficient-samples:n=1,oom-on-record,ceiling-fitted")
	}

	// And T13's own fixture arithmetic, so its two deliberate margins are pinned
	// where a reader can see them rather than recomputed in a comment.
	if got := overFitSliceMax - server.admitSliceHeadroom(1); got != overFitCeiling {
		t.Fatalf("T13 ceiling=%d, want %d", got, overFitCeiling)
	}
	if got := runner.SliceFittedReserve(overFitCeiling); got != overFitFit {
		t.Fatalf("FIT(%d)=%d, want %d", overFitCeiling, got, overFitFit)
	}
	if overFitPin <= overFitFit || overFitPin >= overFitCeiling {
		t.Fatalf("T13's pinned cap %d must sit strictly between the fit %d and the ceiling %d: above the fit so the recorded peak fails the guard, below the ceiling so the pinned request is admissible",
			overFitPin, overFitFit, overFitCeiling)
	}
	if overFitWorkloadBytes <= overFitPin {
		t.Fatalf("T13's workload asks for %d against a %d cap; it must exceed the cap or the kernel never kills it", overFitWorkloadBytes, overFitPin)
	}
}

// waitForDrainedAdmitLedger blocks until every slice queue this daemon knows
// about reports no outstanding job and no waiter, so the NEXT request's entry
// ceiling is `maximum - admitSliceHeadroom(1)` and not one headroom term
// narrower. It waits on the daemon's own ledger rather than on a sleep, so it
// cannot pass by luck.
func waitForDrainedAdmitLedger(t *testing.T, server *Server) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		server.admitRegistryMu.Lock()
		queues := make([]*sliceQueue, 0, len(server.admitQueues))
		for _, queue := range server.admitQueues {
			queues = append(queues, queue)
		}
		server.admitRegistryMu.Unlock()
		drained := true
		for _, queue := range queues {
			queue.mu.Lock()
			if queue.outstandingJobs != 0 || queue.outstanding != 0 || len(queue.waiters) != 0 {
				drained = false
			}
			queue.mu.Unlock()
		}
		if drained {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("the previous phase's admission was never released, so the next phase's entry ceiling is not the one this test asserts against")
}

type smallSliceRun = oomSelfHealRun

// smallSliceRunner builds the launch closure both real-cgroup tests above use.
// It reuses TestConfineOOMSelfHealWorkload (AIRA-128's re-exec'd allocator),
// which TOUCHES every page it allocates, so a cap is genuinely breached rather
// than merely declared.
func smallSliceRunner(t *testing.T, slice string, paths Paths) func(token string, bytesWanted, declaredCap int64, timeout time.Duration) smallSliceRun {
	t.Helper()
	return func(token string, bytesWanted, declaredCap int64, timeout time.Duration) smallSliceRun {
		t.Helper()
		var stdout, stderr bytes.Buffer
		ctx, cancel := context.WithTimeout(context.Background(), timeout)
		defer cancel()
		result, err := runner.Confine(ctx, runner.ConfineRequest{
			Slice: slice, RuntimeDir: paths.RuntimeDir, AdmitSocketPath: paths.SocketPath,
			SelfPath: os.Args[0], Argv: oomSelfHealArgv(token),
			Env: append(os.Environ(),
				oomSelfHealEnv+"=1",
				oomSelfHealBytes+"="+strconv.FormatInt(bytesWanted, 10),
			),
			ScopeMemoryMax:   declaredCap,
			AdmissionMaxWait: timeout / 3,
			Stdin:            strings.NewReader(""), Stdout: &stdout, Stderr: &stderr,
		})
		return smallSliceRun{result: result, err: err, stdout: stdout.String(), stderr: stderr.String()}
	}
}

// newSmallSliceFixture builds the throwaway cgroup these tests use as their
// "slice": a finite memory.max with the memory controller delegated to it.
func newSmallSliceFixture(t *testing.T, maximum int64) string {
	t.Helper()
	parent := cgrouptest.IsolatedScopeParent(t)
	if err := os.WriteFile(filepath.Join(parent, "cgroup.subtree_control"), []byte("+memory"), 0o644); err != nil {
		cgrouptest.SkipOrFailRealCgroup(t, "memory controller not delegated to %s: %v", parent, err)
	}
	slice := filepath.Join(parent, "slice")
	if err := os.Mkdir(slice, 0o755); err != nil {
		cgrouptest.SkipOrFailRealCgroup(t, "create fixture slice cgroup: %v", err)
	}
	if err := os.WriteFile(filepath.Join(slice, "memory.max"), []byte(strconv.FormatInt(maximum, 10)), 0o644); err != nil {
		cgrouptest.SkipOrFailRealCgroup(t, "fixture slice memory.max is not writable: %v", err)
	}
	return slice
}
