//go:build linux

package daemon

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"aira/internal/cgrouptest"
	"aira/internal/runner"
)

// AIRA-128. End-to-end proof, against a REAL cgroup and a REAL daemon, that a
// job OOM-killed because its cold-start reserve under-provisioned it is
//
//  1. reported honestly rather than as a clean run, and
//  2. attributed to its OWN command signature, so the very NEXT run of the same
//     command is admitted at an escalated reserve with no operator action.
//
// Why this test exists rather than another table row in confine_admit_test.go:
// every existing test of the escalation stubs admitPeakHistory with synthetic
// `OOMCount > 0` stats, which asserts the ARITHMETIC and nothing about the
// ATTRIBUTION. The reported incident's live hypothesis was precisely that the
// arithmetic was fine and the OOM never reached ConfinePeakHistory keyed to the
// signature the estimator reads (a sub-scope absorbing the kill, an OOM the
// teardown read missed). A synthetic-stats test cannot distinguish those two
// worlds; only a real kernel OOM travelling the whole path can:
//
//	real memory.max breach -> memory.events -> confine teardown read ->
//	reportPeak over the admit socket -> RecordConfinePeak -> ConfinePeakHistory
//	-> resolveAdmitReserve -> the next scope's memory.max
//
// The fixture reproduces the reported shape rather than an abstraction of it:
// an unpinned command with no history of its own is capped at the machine-wide
// p90 PRIOR (`estimate:p90-prior`, the basis the incident ran under), that cap
// is below what the command actually needs, and the job is group-killed.
//
// aira.slice is never touched: the whole fixture lives under a throwaway
// cgrouptest.IsolatedScopeParent, torn down in t.Cleanup.
//
// Non-porousness was established by mutation, and the surviving mutant is
// recorded rather than left implicit:
//
//   - reportPeak's `oom` argument forced to false -> RED at phase 3 (the
//     attribution leg).
//   - resolveAdmitReserve's `stats.OOMCount > 0` escalation branch disabled ->
//     RED at phase 3 (the resolution leg).
//   - classifyConfineTermination's OOM branch disabled -> RED at phase 2 with
//     `terminated-by=unattributed-sigkill` (the honesty leg).
//   - SURVIVES THIS TEST: swapping the hierarchical `memory.events` counter
//     reportPeak reads for the LOCAL one. In this fixture the workload is the
//     leader and lives directly in the confine scope, so both counters rise
//     together. The shape that separates them -- a victim one cgroup down (an
//     aitest worker sub-scope, a container at its own --memory) -- needs a
//     nested cgroup this fixture does not build. That mutant is killed instead
//     by TestConfineGrantedReserveIsScopeCapAndPeakIsReported
//     (internal/runner/confine_linux_test.go), which feeds reportPeak a usage
//     with ONLY the hierarchical OOMKill raised and asserts oom=true -- a
//     synthetic usage, not a real nested cgroup. (Build review: the
//     descendantOOM/drainedOOM rows of TestClassifyConfineTermination pin the
//     VERDICT's local read, the opposite direction, and do not cover this.)
//     The residual, accepted gap is therefore narrower than "uncovered": no
//     real-cgroup test drives a nested-victim OOM through reportPeak's
//     attribution end to end.
//
// A second accepted gap, named by AIRA-139 rather than introduced by it: what
// phase 3 pins is the escalation's ATTRIBUTION (the `,oom-on-record` token,
// reachable only through this signature's own OOM record), not the
// escalated VALUE. AIRA-149 made that distinction visible in the basis itself --
// this fixture's phase 3 now reads
// `fallback:insufficient-samples:n=1,oom-on-record`, which says both halves
// truthfully, where it used to read `estimate:oom-escalated` and name a
// provenance the number did not have.
// With one OOM sample there is no usable ordinary estimate, so
// resolveAdmitReserve's max(estimate, 1.5x OOM peak) keeps the unpinned client
// default (runner.DefaultConfineMemoryReserve, 4 GiB) -- far above the ~80 MiB
// 1.5x figure -- and that default is what the second run then succeeds at. It
// was already so before AIRA-139; that ticket only made the resolution stop
// landing on the fixture ceiling. Driving the ARITHMETIC would need an OOM peak
// above 2.7 GiB, i.e. a multi-gigabyte real workload on a shared box; the
// arithmetic is pinned instead, at unit cost, by
// TestConfineEstimatorAndOOMEscalationClamp and
// TestConfineOOMAtCeilingIsGenuinelyTooLargeAndPinWins.
//
// verifies: AIRA-128
const (
	// The fixture slice budget. A LIMIT, not an allocation: the workloads below
	// touch a few hundred MiB whatever this says, so its only job is to place
	// the fixture's admission ceiling.
	//
	// It is derived from runner.DefaultConfineMemoryReserve rather than picked,
	// and that is the AIRA-139 fix. This fixture's phase-3 request is UNPINNED,
	// so the reserve it carries to the daemon is exactly that default, and
	// resolveAdmitReserve's OOM-escalation branch takes max(estimate, 1.5x the
	// OOM peak): with a single OOM sample there is no usable ordinary estimate,
	// so the default (4 GiB) survives, dwarfs the 1.5x escalation (~80 MiB
	// here), and is then clamped to EXACTLY the ceiling by the branch's
	// too-large clamp. A reserve equal to the ceiling is grantable only while
	// the slice's own charge reads byte-exact zero -- checkedAvailable charges
	// max(current - reclaimable, outstanding) against that same ceiling -- which
	// on a slice five earlier confine runs just used is a coin flip on a single
	// residual 4 KiB page. Measured: the original 1 GiB budget passed only when
	// a poll happened to read current=0, and hung for the full 30s admission
	// wait (E_ADMIT_SATURATED, "queue position 1 of 1, 0B queued ahead" -- an
	// empty slice) when it read 4096 instead.
	//
	// Keeping the budget clear of the default keeps the resolution off that
	// edge: the reserve resolves to the default UNCLAMPED, with the whole margin
	// below spare. TestOOMSelfHealFixtureStaysOffTheCeilingClamp pins the
	// invariant so a future change to either constant fails loudly instead of
	// reintroducing the flake.
	oomSelfHealSliceMax = runner.DefaultConfineMemoryReserve + (2 << 30)
	// What the seeding runs touch. Their peak becomes the machine-wide p90
	// prior, which is what the target command's cold start is then capped at.
	// It must leave a Go runtime room to start, and must be far below the
	// target workload, or the cold start would not OOM at all.
	oomSelfHealSeedBytes = int64(40 << 20)
	// What the target command touches: several times its cold-start cap.
	oomSelfHealTargetBytes = int64(320 << 20)
	// Printed by the workload only after every page is resident. Its ABSENCE on
	// the OOM run is half the honesty assertion: a phantom-success line from a
	// job the kernel killed is the failure mode this ticket is about.
	oomSelfHealMarker = "AIRA-128-WORKLOAD-COMPLETED"
	oomSelfHealEnv    = "AIRA_OOM_SELFHEAL_HELPER"
	oomSelfHealBytes  = "AIRA_OOM_SELFHEAL_BYTES"
)

// TestConfineOOMSelfHealWorkload is the re-exec'd workload (the
// TestSliceCeilingAllocHelper pattern). It TOUCHES every page it allocates, so
// the memory is genuinely resident and genuinely non-reclaimable -- an
// untouched allocation would never breach memory.max and the fixture would
// prove nothing.
func TestConfineOOMSelfHealWorkload(t *testing.T) {
	if os.Getenv(oomSelfHealEnv) != "1" {
		return
	}
	size, err := strconv.ParseInt(os.Getenv(oomSelfHealBytes), 10, 64)
	if err != nil || size <= 0 {
		os.Exit(2)
	}
	block := make([]byte, size)
	for i := int64(0); i < size; i += 4096 {
		block[i] = 1
	}
	runtime.KeepAlive(block)
	_, _ = os.Stdout.WriteString(oomSelfHealMarker + "\n")
	os.Exit(0)
}

type oomSelfHealRun struct {
	result runner.ConfineResult
	err    error
	stdout string
	stderr string
}

func TestRealOOMAttributesToItsSignatureAndEscalatesTheNextAdmission(t *testing.T) {
	slice := newOOMSelfHealSlice(t)
	paths := testPaths(t)
	server := NewServer(paths)
	// Fixture-scale headroom. Production headroom is 2 GiB + 64 MiB per job,
	// which against this fixture's budget would eat a third of it. Scaled, NOT
	// disabled: every admission here is still sized against a real
	// maximum-minus-headroom ceiling, and a request over it is still terminally
	// refused. See oomSelfHealSliceMax for why the ceiling must stay clear of
	// runner.DefaultConfineMemoryReserve (AIRA-139).
	server.admitSliceHeadroomBase = 32 << 20
	server.admitSliceHeadroomSupervisor = 8 << 20
	startServer(t, server)

	run := func(token string, bytesWanted int64, declaredCap int64) oomSelfHealRun {
		t.Helper()
		var stdout, stderr bytes.Buffer
		ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
		defer cancel()
		result, err := runner.Confine(ctx, runner.ConfineRequest{
			Slice: slice, RuntimeDir: paths.RuntimeDir, AdmitSocketPath: paths.SocketPath,
			SelfPath: os.Args[0], Argv: oomSelfHealArgv(token),
			Env: append(os.Environ(),
				oomSelfHealEnv+"=1",
				oomSelfHealBytes+"="+strconv.FormatInt(bytesWanted, 10),
			),
			ScopeMemoryMax:   declaredCap,
			AdmissionMaxWait: 30 * time.Second,
			Stdin:            strings.NewReader(""), Stdout: &stdout, Stderr: &stderr,
		})
		return oomSelfHealRun{result: result, err: err, stdout: stdout.String(), stderr: stderr.String()}
	}

	// PHASE 1 -- establish a machine-wide p90 prior, the way a real box does:
	// by actually running things. Three real runs of one seed signature are the
	// minimum ConfinePeakP90 will consider (COUNT(peak_rss) >= 3). They declare
	// their own small cap only so they fit this fixture's ceiling; the peak they
	// record is measured, not declared.
	for i := 0; i < 3; i++ {
		seed := run("seed", oomSelfHealSeedBytes, 256<<20)
		if seed.err != nil {
			cgrouptest.SkipOrFailRealCgroup(t, "seeding confine run %d unavailable: %v (stderr %q)", i, seed.err, seed.stderr)
		}
		if seed.result.Exit != 0 || !strings.Contains(seed.stdout, oomSelfHealMarker) {
			t.Fatalf("seed run %d: exit=%d stdout=%q stderr=%q, want a completed workload", i, seed.result.Exit, seed.stdout, seed.stderr)
		}
	}
	// The daemon caches the p90 for a minute. Expiring the cache is not faking
	// the prior -- the value still comes from the three real observations above;
	// it only spares the test a 60s sleep.
	server.admitPriorMu.Lock()
	server.admitPriorAt = time.Time{}
	server.admitPriorMu.Unlock()

	// PHASE 2 -- the cold start. The target command has no history of its own,
	// so it is capped at the prior, which is far below what it needs.
	first := run("target", oomSelfHealTargetBytes, 0)
	if first.err != nil {
		cgrouptest.SkipOrFailRealCgroup(t, "cold-start confine run unavailable: %v (stderr %q)", first.err, first.stderr)
	}
	if first.result.Status.ReserveBasis != "estimate:p90-prior" {
		t.Fatalf("cold-start reserve-basis=%q reserve=%d, want the p90 prior — the fixture is not reproducing the reported shape (stderr %q)",
			first.result.Status.ReserveBasis, first.result.Status.ReserveBytes, first.stderr)
	}
	// The honesty half. A run the kernel killed must not look like a run that
	// finished: no completion marker, a signalled exit, and a verdict that names
	// the OOM rather than leaving the reader to guess.
	if strings.Contains(first.stdout, oomSelfHealMarker) {
		t.Fatalf("cold-start run printed its completion marker despite being capped at %d bytes below its need: stdout=%q",
			first.result.Status.ScopeMemoryMax, first.stdout)
	}
	if first.result.Status.TerminatedBy != runner.ConfineTerminatedOOM {
		t.Fatalf("cold-start terminated-by=%q, want %q — an OOM-killed run reported as anything else is the phantom-failure shape this ticket is about (exit=%d stderr=%q)",
			first.result.Status.TerminatedBy, runner.ConfineTerminatedOOM, first.result.Exit, first.stderr)
	}
	if first.result.Exit != 137 {
		t.Fatalf("cold-start exit=%d, want 137 (128+SIGKILL) so a consumer's own wrapper sees a failure, never a clean exit", first.result.Exit)
	}
	if !strings.Contains(first.stderr, "OOM-killed at its memory cap") {
		t.Fatalf("cold-start stderr=%q, want the operator-facing OOM advisory", first.stderr)
	}
	if first.result.Status.PeakRSS == nil || *first.result.Status.PeakRSS <= 0 {
		t.Fatalf("cold-start peak RSS unestablished (%v): the observation the escalation is computed from never reached the daemon", first.result.Status.PeakRSS)
	}
	oomPeak := *first.result.Status.PeakRSS

	// PHASE 3 -- the self-heal. Same argv, therefore the same signature, and no
	// operator action of any kind between the two runs.
	second := run("target", oomSelfHealTargetBytes, 0)
	if second.err != nil {
		t.Fatalf("second confine run: %v (stderr %q)", second.err, second.stderr)
	}
	// The load-bearing assertion, and AIRA-149 states precisely what it proves and
	// what it does not.
	//
	// PROVES (attribution): the ",oom-on-record" token is reachable ONLY through
	// stats.OOMCount > 0 && stats.MaxOOMPeak > 0 for THIS exact signature, which
	// is only true if the kernel's OOM kill was observed at teardown, reported
	// over the wire, and durably recorded against the signature the estimator
	// reads back. A signature-attribution gap of any kind leaves this at a bare
	// fallback basis with no OOM token at all.
	//
	// DOES NOT PROVE (provenance): that the escalation produced the number. With
	// ONE OOM sample there is no usable ordinary estimate and the ~80 MiB 1.5x
	// figure is far below the unpinned 4 GiB client default, so the default is
	// what survives and what the second run then succeeds at. Before AIRA-149 this
	// line asserted "estimate:oom-escalated", which named a provenance the number
	// did not have -- the exact defect that ticket is about, sitting inside the
	// test that was meant to be the proof.
	const wantSelfHealBasis = "fallback:insufficient-samples:n=1,oom-on-record"
	if second.result.Status.ReserveBasis != wantSelfHealBasis {
		t.Fatalf("second-run reserve-basis=%q (reserve=%d), want %q — the real OOM was not attributed to this command's own signature, so nothing self-heals and every re-run repeats the kill",
			second.result.Status.ReserveBasis, second.result.Status.ReserveBytes, wantSelfHealBasis)
	}
	wantFloor := oomPeak + oomPeak/2
	if second.result.Status.ScopeMemoryMax < wantFloor {
		t.Fatalf("second-run scope memory.max=%d, want at least the 1.5x escalation floor %d over the observed OOM peak %d",
			second.result.Status.ScopeMemoryMax, wantFloor, oomPeak)
	}
	if second.result.Status.ScopeMemoryMax <= first.result.Status.ScopeMemoryMax {
		t.Fatalf("second-run cap %d did not rise above the cold-start cap %d that killed the job",
			second.result.Status.ScopeMemoryMax, first.result.Status.ScopeMemoryMax)
	}
	// And the consequence that actually matters to a caller: the identical
	// command, re-run with nothing changed, now completes.
	if second.result.Exit != 0 || second.result.Status.TerminatedBy != "normal" || !strings.Contains(second.stdout, oomSelfHealMarker) {
		t.Fatalf("second run exit=%d terminated-by=%q stdout=%q stderr=%q, want the same command to succeed at the escalated reserve",
			second.result.Exit, second.result.Status.TerminatedBy, second.stdout, second.stderr)
	}
}

// TestOOMSelfHealFixtureStaysOffTheCeilingClamp pins the fixture invariant the
// AIRA-139 flake violated, at unit cost and with no cgroup: the phase-3
// resolution must land CLEAR of this fixture's admission ceiling, never on it.
//
// It exists because the failure it guards is invisible in the fixture itself.
// Landing exactly on the ceiling does not fail the assertions above; it makes
// the second run's admission depend on the slice's residual charge reading
// byte-exact zero at the moment a poll looks, so the fixture passes and fails
// on the same code, decided by one 4 KiB page. Asserting the RESOLUTION here
// converts that into a deterministic, self-describing failure at the two
// constants that can reintroduce it -- oomSelfHealSliceMax and
// runner.DefaultConfineMemoryReserve -- or at the reserve policy itself.
//
// The margin asserted is one whole target workload rather than a token gap: it
// says the fixture's ceiling can hold the phase-3 reserve even while the slice
// still carries everything the previous phase touched, which is the condition
// the flake actually broke.
//
// verifies: AIRA-139
func TestOOMSelfHealFixtureStaysOffTheCeilingClamp(t *testing.T) {
	server := NewServer(Paths{})
	server.stopping = make(chan struct{})
	server.admitSliceHeadroomBase = 32 << 20
	server.admitSliceHeadroomSupervisor = 8 << 20
	server.admitPeakP90 = func(context.Context) (int64, bool, error) {
		t.Fatal("the OOM-escalation branch must resolve before the machine-wide prior is consulted")
		return 0, false, nil
	}
	// Exactly the history the cold-start OOM leaves for the target signature:
	// ONE observation, which is one OOM and no usable ordinary estimate. The
	// peak's exact value is immaterial to what is asserted -- any peak whose
	// 1.5x escalation is below the unpinned default reproduces the phase-3
	// resolution -- so it is sized from the seed bytes the prior is built from
	// rather than pinned to one host's measurement.
	peak := oomSelfHealSeedBytes + oomSelfHealSeedBytes/2
	server.admitPeakHistory = func(context.Context, string) (runner.PeakRSSStats, error) {
		return runner.PeakRSSStats{TotalCount: 1, SampleCount: 1, PeakMax: peak, OOMCount: 1, MaxOOMPeak: peak}, nil
	}
	// One job's worth of headroom: the phase-3 request is alone on the slice,
	// which is the ceiling admitConnection computes for it.
	ceiling := oomSelfHealSliceMax - server.admitSliceHeadroom(1)
	reserve, basis := server.resolveAdmitReserve(
		admitRequest{reserve: runner.DefaultConfineMemoryReserve, signature: "target"}, ceiling)
	// AIRA-149: the phase-3 resolution is row (d) -- one sample, so no usable
	// ordinary estimate, and an escalation below the unpinned client default,
	// which therefore survives unclamped. The basis names that, and the
	// ",oom-on-record" half is what still proves the OOM record was consulted.
	if basis != "fallback:insufficient-samples:n=1,oom-on-record" {
		t.Fatalf("phase-3 basis=%q reserve=%d, want %q — the fixture no longer models the run it guards",
			basis, reserve, "fallback:insufficient-samples:n=1,oom-on-record")
	}
	if slack := ceiling - reserve; slack < oomSelfHealTargetBytes {
		t.Fatalf("phase-3 reserve=%d leaves %d below the fixture ceiling %d, want at least %d — "+
			"a reserve at or near the ceiling is grantable only while the slice's own charge reads zero, "+
			"which is the AIRA-139 flake; raise oomSelfHealSliceMax clear of runner.DefaultConfineMemoryReserve (%d)",
			reserve, slack, ceiling, oomSelfHealTargetBytes, runner.DefaultConfineMemoryReserve)
	}
}

// oomSelfHealArgv is the workload's launch argv. The trailing token is what
// separates the seed signature from the target signature: ResourceSignature is
// the effective argv joined, so two runs differ as signatures if and only if
// their argv differs.
func oomSelfHealArgv(token string) []string {
	return []string{os.Args[0], "-test.run=^TestConfineOOMSelfHealWorkload$", token}
}

// newOOMSelfHealSlice builds the throwaway cgroup this fixture uses as its
// "slice": a finite memory.max (confine refuses an unbounded parent) with the
// memory controller delegated to it, so the confine scope created inside can
// carry a memory.max of its own.
func newOOMSelfHealSlice(t *testing.T) string {
	t.Helper()
	parent := cgrouptest.IsolatedScopeParent(t)
	if err := os.WriteFile(filepath.Join(parent, "cgroup.subtree_control"), []byte("+memory"), 0o644); err != nil {
		cgrouptest.SkipOrFailRealCgroup(t, "memory controller not delegated to %s: %v", parent, err)
	}
	slice := filepath.Join(parent, "slice")
	if err := os.Mkdir(slice, 0o755); err != nil {
		cgrouptest.SkipOrFailRealCgroup(t, "create fixture slice cgroup: %v", err)
	}
	if err := os.WriteFile(filepath.Join(slice, "memory.max"), []byte(strconv.FormatInt(oomSelfHealSliceMax, 10)), 0o644); err != nil {
		cgrouptest.SkipOrFailRealCgroup(t, "fixture slice memory.max is not writable: %v", err)
	}
	return slice
}
