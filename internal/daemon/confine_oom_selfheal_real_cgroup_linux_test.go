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
// verifies: AIRA-128
const (
	// The fixture slice budget. Small on purpose -- the headroom fields below
	// are scaled to match, so the ceiling arithmetic is exercised at fixture
	// scale rather than being switched off.
	oomSelfHealSliceMax = int64(1 << 30)
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
	// Fixture-scale headroom. The slice above is 1 GiB, while production
	// headroom is 2 GiB + 64 MiB per job, which would leave this fixture no
	// ceiling at all. Scaled, NOT disabled: the ceiling clamp on the escalated
	// reserve is still a live part of the path under test.
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
	// The load-bearing assertion. This basis is reachable ONLY through
	// stats.OOMCount > 0 for THIS exact signature, which is only true if the
	// kernel's OOM kill was observed at teardown, reported over the wire, and
	// durably recorded against the signature the estimator reads back. A
	// signature-attribution gap of any kind leaves this at a fallback basis.
	if second.result.Status.ReserveBasis != "estimate:oom-escalated" {
		t.Fatalf("second-run reserve-basis=%q (reserve=%d), want %q — the real OOM was not attributed to this command's own signature, so nothing self-heals and every re-run repeats the kill",
			second.result.Status.ReserveBasis, second.result.Status.ReserveBytes, "estimate:oom-escalated")
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
