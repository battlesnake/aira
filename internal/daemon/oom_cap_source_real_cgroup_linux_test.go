//go:build linux

package daemon

import (
	"bytes"
	"context"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"aira/internal/cgrouptest"
	"aira/internal/runner"
)

// Filename note. This file is named oom_cap_source_..., not the more obvious
// confine_cap_source_..., so it sorts (and therefore runs, within this
// package's one test binary) AFTER confine_oom_selfheal_real_cgroup_linux_test.go.
// That ordering matters: AIRA-128's TestRealOOMAttributesToItsSignatureAndEscalatesTheNextAdmission
// in that file has a real, pre-existing, unrelated bug (filed as AIRA-139) —
// it reliably fails whenever it is NOT the first real Server+slice sequence in
// the test binary process (reproduced even against ITSELF at -count=2, with no
// other file involved). Naming this file to run after it, rather than before,
// avoids tripping that bug; it does not fix it. See AIRA-139 for the
// investigation (ruled out: this file's own memory footprint, and host
// MemAvailable settling — neither made any difference) and for whoever
// eventually finds the real root cause.
//
// AIRA-133. Against a REAL cgroup, a REAL daemon and a REAL kernel OOM kill:
// two jobs killed the same way, at caps of the same order, report
// DISTINGUISHABLY different provenance and are told to do different things.
//
// Why this is an end-to-end fixture rather than another row in the pure
// formatter table (internal/runner/confine_linux_test.go's
// TestFormatConfineReserveAdvisory, which pins the wording per source): the
// formatter table proves only that DIFFERENT INPUTS produce different text. The
// defect this ticket is about is upstream of that -- the two shapes were
// indistinguishable because nothing on the launch path ever recorded WHICH cap
// had been written. A table test cannot see that gap; it would pass just as
// happily against a build that hard-codes one source at the single call site.
// Only a real launch, whose cap is chosen by the same branch that records the
// provenance, closes it.
//
// The two runs deliberately differ in ONE thing -- who chose the cap:
//
//	AUTO     : unpinned, no flags. The daemon resolves the cap from history
//	           (here the machine-wide p90 prior), which is below what the
//	           workload needs, and the kernel kills it.
//	OPERATOR : the same workload under an explicit --memory-max the caller
//	           typed, also below what the workload needs, killed the same way.
//
// Both are `terminated-by=oom` with a real `memory.events` kill behind them, so
// nothing here is simulated and nothing is distinguished by the kill itself.
//
// aira.slice is never touched: the whole fixture lives under a throwaway
// cgrouptest.IsolatedScopeParent (newOOMSelfHealSlice), torn down in t.Cleanup.
//
// verifies: AIRA-133
const (
	// Deliberately a FRACTION of the AIRA-128 fixture's sizes next door. This
	// test needs only "the workload outgrows its cap"; it does not need the
	// reported incident's magnitudes. Sized to stay well inside its own fixture
	// slice while still being several times the cap that kills it.
	capSourceSeedBytes   = int64(40 << 20)
	capSourceTargetBytes = int64(120 << 20)
)

func TestOOMTrailerDistinguishesAnEstimatedCapFromAnOperatorSuppliedOne(t *testing.T) {
	slice := newOOMSelfHealSlice(t)
	paths := testPaths(t)
	server := NewServer(paths)
	// Fixture-scale headroom, matching the AIRA-128 fixture next door: the slice
	// is 1 GiB, while production headroom would leave it no ceiling at all.
	server.admitSliceHeadroomBase = 32 << 20
	server.admitSliceHeadroomSupervisor = 8 << 20
	startServer(t, server)

	run := func(token string, bytesWanted int64, declaredCap int64, declaredReserve int64) oomSelfHealRun {
		t.Helper()
		var stdout, stderr bytes.Buffer
		ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
		defer cancel()
		request := runner.ConfineRequest{
			Slice: slice, RuntimeDir: paths.RuntimeDir, AdmitSocketPath: paths.SocketPath,
			SelfPath: os.Args[0], Argv: oomSelfHealArgv(token),
			Env: append(os.Environ(),
				oomSelfHealEnv+"=1",
				oomSelfHealBytes+"="+strconv.FormatInt(bytesWanted, 10),
			),
			ScopeMemoryMax:   declaredCap,
			AdmissionMaxWait: 30 * time.Second,
			Stdin:            strings.NewReader(""), Stdout: &stdout, Stderr: &stderr,
		}
		if declaredReserve > 0 {
			request.MemoryReserve = declaredReserve
			request.MemoryReservePinned = true
		}
		result, err := runner.Confine(ctx, request)
		return oomSelfHealRun{result: result, err: err, stdout: stdout.String(), stderr: stderr.String()}
	}

	// Establish a machine-wide p90 prior the way a real box does: by running
	// things. Without it an unpinned cold start has no resolvable reserve at all
	// and the AUTO leg below would never reach a daemon-chosen cap.
	for i := 0; i < 3; i++ {
		seed := run("cap-source-seed", capSourceSeedBytes, 128<<20, 0)
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

	// --- the AUTO leg -------------------------------------------------------
	// Runs FIRST, and under its own signature token, so no observation made by
	// the operator leg below can have influenced the cap it is given.
	auto := run("cap-source-auto", capSourceTargetBytes, 0, 0)
	if auto.err != nil {
		cgrouptest.SkipOrFailRealCgroup(t, "auto-capped confine run unavailable: %v (stderr %q)", auto.err, auto.stderr)
	}
	if auto.result.Status.TerminatedBy != runner.ConfineTerminatedOOM {
		t.Fatalf("auto leg terminated-by=%q, want %q — this leg must be a REAL OOM at an AIRA-chosen cap or it proves nothing (exit=%d cap=%d stderr=%q)",
			auto.result.Status.TerminatedBy, runner.ConfineTerminatedOOM, auto.result.Exit, auto.result.Status.ScopeMemoryMax, auto.stderr)
	}
	if auto.result.Status.ScopeMemoryCapSource != runner.ConfineCapSourceDaemonReserve {
		t.Fatalf("auto leg cap-source=%q (cap=%d reserve-basis=%q), want %q — an AIRA-chosen cap reported as anything else sends the reader after a flag they never set",
			auto.result.Status.ScopeMemoryCapSource, auto.result.Status.ScopeMemoryMax, auto.result.Status.ReserveBasis, runner.ConfineCapSourceDaemonReserve)
	}
	if runner.ConfineCapSourceIsOperator(auto.result.Status.ScopeMemoryCapSource) {
		t.Fatalf("auto leg's cap classified as operator-supplied: %q", auto.result.Status.ScopeMemoryCapSource)
	}

	// --- the OPERATOR leg ---------------------------------------------------
	// A cap the caller typed, deliberately of the same order as the one the
	// daemon chose above, so the two legs cannot be told apart by their numbers.
	const operatorCap = int64(64 << 20)
	operator := run("cap-source-operator", capSourceTargetBytes, operatorCap, 0)
	if operator.err != nil {
		t.Fatalf("operator-capped confine run: %v (stderr %q)", operator.err, operator.stderr)
	}
	if operator.result.Status.TerminatedBy != runner.ConfineTerminatedOOM {
		t.Fatalf("operator leg terminated-by=%q, want %q (exit=%d cap=%d stderr=%q)",
			operator.result.Status.TerminatedBy, runner.ConfineTerminatedOOM, operator.result.Exit, operator.result.Status.ScopeMemoryMax, operator.stderr)
	}
	if operator.result.Status.ScopeMemoryCapSource != runner.ConfineCapSourceMemoryMax {
		t.Fatalf("operator leg cap-source=%q (cap=%d), want %q — an operator's own --memory-max reported as an AIRA estimate tells them to re-run a command that cannot self-heal",
			operator.result.Status.ScopeMemoryCapSource, operator.result.Status.ScopeMemoryMax, runner.ConfineCapSourceMemoryMax)
	}

	// --- the discrimination the ticket is about -----------------------------
	// Both legs are the same verdict at a cap of the same order. Before this
	// change their trailers were byte-identical in every field a reader could
	// have used to tell them apart.
	autoTrailer := runner.FormatConfineStatus(auto.result.Status)
	operatorTrailer := runner.FormatConfineStatus(operator.result.Status)
	if !strings.Contains(autoTrailer, "cap-source="+runner.ConfineCapSourceDaemonReserve) {
		t.Fatalf("auto trailer lacks its cap-source: %q", autoTrailer)
	}
	if !strings.Contains(operatorTrailer, "cap-source="+runner.ConfineCapSourceMemoryMax) {
		t.Fatalf("operator trailer lacks its cap-source: %q", operatorTrailer)
	}

	// And the operator-facing consequence: the two stderr streams name DIFFERENT
	// next steps, each derived from the recorded provenance rather than guessed.
	// Asserted in both directions, because a build that emitted the re-run advice
	// unconditionally would satisfy a one-sided check while being exactly as
	// misleading as the silence it replaced.
	const reRun = "RE-RUN THE IDENTICAL COMMAND"
	const yourOwn = "this cap is YOUR OWN --memory-max"
	if !strings.Contains(auto.stderr, reRun) {
		t.Fatalf("auto leg stderr does not name the re-run as the next step: %q", auto.stderr)
	}
	if strings.Contains(auto.stderr, yourOwn) {
		t.Fatalf("auto leg stderr blames a flag the caller never passed: %q", auto.stderr)
	}
	if !strings.Contains(operator.stderr, yourOwn) {
		t.Fatalf("operator leg stderr does not say the cap is the caller's own: %q", operator.stderr)
	}
	if strings.Contains(operator.stderr, reRun) {
		t.Fatalf("operator leg stderr tells the caller to re-run a command whose cap they fixed themselves: %q", operator.stderr)
	}

	// --- --memory-reserve is the operator's OTHER flag ----------------------
	// Same half of the split as --memory-max (re-running cannot change it), but a
	// separate branch on the launch path, so it gets its own real launch rather
	// than being assumed to follow.
	reserved := run("cap-source-reserve", capSourceTargetBytes, 0, operatorCap)
	if reserved.err != nil {
		t.Fatalf("declared-reserve confine run: %v (stderr %q)", reserved.err, reserved.stderr)
	}
	if reserved.result.Status.TerminatedBy != runner.ConfineTerminatedOOM {
		t.Fatalf("declared-reserve leg terminated-by=%q, want %q (exit=%d cap=%d stderr=%q)",
			reserved.result.Status.TerminatedBy, runner.ConfineTerminatedOOM, reserved.result.Exit, reserved.result.Status.ScopeMemoryMax, reserved.stderr)
	}
	if reserved.result.Status.ScopeMemoryCapSource != runner.ConfineCapSourceMemoryReserve {
		t.Fatalf("declared-reserve leg cap-source=%q (cap=%d reserve-basis=%q), want %q",
			reserved.result.Status.ScopeMemoryCapSource, reserved.result.Status.ScopeMemoryMax,
			reserved.result.Status.ReserveBasis, runner.ConfineCapSourceMemoryReserve)
	}
	if !strings.Contains(reserved.stderr, "this cap is YOUR OWN --memory-reserve") {
		t.Fatalf("declared-reserve leg stderr does not name the caller's own flag: %q", reserved.stderr)
	}
	if strings.Contains(reserved.stderr, reRun) {
		t.Fatalf("declared-reserve leg stderr tells the caller to re-run: %q", reserved.stderr)
	}
}
