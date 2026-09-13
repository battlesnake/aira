//go:build linux

// package pylib_test (external): imports aira/internal/daemon, which would cycle
// against an internal `package pylib` test file (daemon -> core -> pylib). The
// S16 e2e (pytest_aitest_e2e_test.go) established this fix; this file reuses its
// requireRealPytest, shortE2ERuntimeDir, environForRealDaemonAitest,
// e2eConfineScopeID and newRealDaemonAndCgroupTestHarness helpers directly (same
// package).
//
// These are the aitest v0.7 S2a topology BRANCH-EXIT gates (they cannot run in
// ci-shim CI, like the measurement and restart merge gates): the real-cgroup
// proof that sibling worker scopes dissolve AIRA-229 (no whole-suite kill on
// aggregate) and AIRA-232 (multi-supervisor safe), that a parent kill leaves no
// orphaned worker (fresh AND post-restart), and that the escape-attestation
// exemption fires on a real delegate run. Each is written NON-POROUS: a false-pass
// trap (the real test count ran) or a per-mechanism mutation red-before, recorded
// in each gate's own doc comment.
package pylib_test

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"aira/internal/runner"
	"aira/internal/testdeadline"
)

// readOuterMemoryEventCounter reads one counter (e.g. "oom_kill",
// "oom_group_kill") from a cgroup scope's memory.events. Per the AIRA honesty
// rule, an absent or unreadable file is NOT a zero: it FAILS the test
// ("unevaluated, never a fake pass") rather than silently passing a "no kill"
// assertion. memory.events is hierarchical, so an OOM anywhere in the scope's
// SUBTREE surfaces on its counter too — which is exactly the property the
// AIRA-229 gate needs: post-topology the workers are SIBLINGS (not in the parent
// scope's subtree), so a non-zero oom_group_kill on the parent means the parent's
// own oom.group fired, i.e. a whole-suite kill. (Kept from the T06-excised v7-1
// guard e2e per the plan's keep/rename note.)
func readOuterMemoryEventCounter(t *testing.T, scopeDir, key string) int64 {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(scopeDir, "memory.events"))
	if err != nil {
		t.Fatalf("cannot read scope memory.events (%q counter is unevaluated, not zero): %v", key, err)
	}
	for _, line := range strings.Split(string(data), "\n") {
		fields := strings.Fields(line)
		if len(fields) == 2 && fields[0] == key {
			value, parseErr := strconv.ParseInt(fields[1], 10, 64)
			if parseErr != nil {
				t.Fatalf("scope memory.events %q is unparseable %q: %v", key, fields[1], parseErr)
			}
			return value
		}
	}
	t.Fatalf("scope memory.events has no %q counter:\n%s", key, data)
	return -1
}

// readCgroupMemoryPeak reads a cgroup scope's memory.peak — the maximum, since the
// scope was created, of the instantaneous total memory charged to it AND its
// descendants (cgroup-v2, hierarchical). Per the AIRA honesty rule an unreadable
// counter FAILS the test (it is unevaluated, never a fabricated 0), because the
// aggregate proof rests on this number.
func readCgroupMemoryPeak(t *testing.T, scopeDir string) int64 {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(scopeDir, "memory.peak"))
	if err != nil {
		t.Fatalf("cannot read %q memory.peak (the aggregate proof is unevaluated, not zero): %v", scopeDir, err)
	}
	peak, err := strconv.ParseInt(strings.TrimSpace(string(data)), 10, 64)
	if err != nil {
		t.Fatalf("%q memory.peak is unparseable %q: %v", scopeDir, strings.TrimSpace(string(data)), err)
	}
	return peak
}

// readCgroupMemoryMax reads a cgroup scope's memory.max as a byte count, failing
// the test if it is unreadable or the literal "max" (unbounded): the caller uses it
// to confirm the harness wrote the finite parent cap the proof compares against, and
// an unbounded or unreadable value would make that check vacuous.
func readCgroupMemoryMax(t *testing.T, scopeDir string) int64 {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(scopeDir, "memory.max"))
	if err != nil {
		t.Fatalf("cannot read %q memory.max (the parent-cap match is unevaluated): %v", scopeDir, err)
	}
	text := strings.TrimSpace(string(data))
	if text == "max" {
		t.Fatalf("%q memory.max is unbounded (\"max\"): the harness must write a finite parent cap", scopeDir)
	}
	value, err := strconv.ParseInt(text, 10, 64)
	if err != nil {
		t.Fatalf("%q memory.max is unparseable %q: %v", scopeDir, text, err)
	}
	return value
}

// TestRealPytestAitestNoWholeSuiteKillOnAggregate is Gate B (AIRA-229): several
// workers that each allocate AND HOLD ~100 MiB run concurrently under a 256 MiB
// parent scope. Under the sibling topology each worker charges the 16 GiB slice
// and its own 256 MiB cap, so the ~256 MiB parent (supervisor + relays only) is
// never approached and its oom.group never fires — every test PASSES.
//
// The positive, mutation-independent proof is a SOUND-BY-CONSTRUCTION witness of
// genuine CONCURRENCY, not just of aggregate volume: the harness-slice memory.peak
// MINUS the outer(parent) scope's memory.peak EXCEEDS the parent cap. cgroup-v2
// memory.peak on the slice records the maximum, over the run, of the INSTANTANEOUS
// total charged to the whole slice subtree (supervisor in the outer scope + every
// sibling worker); outer.peak is an upper bound on the supervisor's usage at that
// same instant, so (slice.peak − outer.peak) is a LOWER BOUND on the worker memory
// resident SIMULTANEOUSLY at the slice's peak instant. The review measured
// 374,214,656 − 49,565,696 = 325 MiB > 256 MiB.
//
// Why sequential workers CANNOT satisfy it (non-porosity by construction, not by
// luck): a worker scope's memory.max is perScopeCapBytes == parentCap, so with at
// most ONE worker ever concurrent the slice's instantaneous total is bounded by
// (outer usage) + parentCap, hence slice.peak ≤ outer.peak + parentCap and the
// strict `slice.peak − outer.peak > parentCap` is impossible. It can hold ONLY if
// ≥2 workers were charged to the slice at one instant — exactly the concurrent
// breach a shared 256 MiB parent cap would have oom.group-killed. The proof depends
// on worker cap ≤ parentCap (both are perScopeCapBytes here); the structural guard
// below confirms nothing but the outer scope and sibling worker scopes charges the
// slice, so slice.peak carries no third population.
//
// Non-porous:
//   - False-pass trap: assert all 6 alloc-hold tests actually ran and PASSED
//     (a gate that greened on zero workers / zero tests would prove nothing) and
//     scoped_workers >= 2 (the aggregate needs at least two live workers).
//   - Positive aggregate proof: slice.peak − outer.peak > parentCap (see above),
//     which sequential workers cannot satisfy — so the no-kill result witnesses a
//     real concurrent over-cap, not an artefact of a tiny or serialised workload.
//   - Slice population guard: every child cgroup of the slice is either the outer
//     scope or a sibling worker scope, so no unaccounted process inflates slice.peak.
//   - Corroboration: Σ(worker peak) > parentCap too — kept, but it is NOT the proof
//     (sequential workers also satisfy it), so it is a weaker, secondary signal.
//   - No worker self-OOM masquerading as a pass: oom_group_killed == false and
//     every per-worker sample oom != true (a self-OOM would report the test
//     `unevaluated`, not `passed`, which the "6 passed / 0 unevaluated" assertion
//     already excludes — checked explicitly too).
//   - Gate A fold: each granted scope_path is a DIRECT CHILD of the slice
//     (nesting returns a grandchild), so this run also anchors sibling placement.
//
// Mutation (restore nesting — worker created under the parent scope, not the
// slice): the concurrent worker RSS charges hierarchically up to the 256 MiB
// parent and its oom.group group-kills the whole suite. RECORDED CAVEAT: post-T7
// the supervisor runs directly in the parent scope, so reverting T3's
// create(parent=slice) to create(parent=outerScope) also violates cgroup-v2's
// no-internal-process rule when +memory is enabled on a populated parent — the
// mutation may red on a delegation/ENOENT `request-invalid` (suite unevaluated)
// BEFORE the oom.group can fire. Either way the gate reds; the actual observed
// mode is recorded in the build report. The positive aggregate proof above does
// not depend on the mutation.
func TestRealPytestAitestNoWholeSuiteKillOnAggregate(t *testing.T) {
	harness := newRealDaemonAndCgroupTestHarness(t) // parent(outer) memory.max = 256 MiB
	outerDir := harness.outerFile.Name()

	// ONE constant drives BOTH the per-worker cap (AIRA_AITEST_ESTIMATED_BYTES →
	// each worker scope's memory.max) AND the parent-cap yardstick the aggregate
	// proof below compares against. They must be the SAME number for the
	// sound-by-construction non-porosity argument to hold (worker cap ≤ parentCap):
	// see the doc comment. It also equals the outer scope's own memory.max, which
	// the harness writes as 256 MiB — asserted below so a harness drift is caught.
	const perScopeCapBytes = int64(256) << 20

	measureDir := filepath.Join(t.TempDir(), "measure")
	if err := os.MkdirAll(measureDir, 0o755); err != nil {
		t.Fatal(err)
	}

	// Six ~2.5 s alloc-hold tests over a 3-worker pool is ~5 s of real work at
	// full pool; generous slack for pool growth (~1 Hz) plus turnover.
	runCtx, cancelRun := context.WithTimeout(context.Background(), testdeadline.Wait(2*time.Minute))
	defer cancelRun()
	command := exec.CommandContext(runCtx, harness.pytest, "-q", "--aitest-workers=3", "test_alloc_hold.py")
	command.Dir = filepath.Join(harness.aitestDir, "testdata")
	command.WaitDelay = 15 * time.Second // load-bearing on a real-cgroup run (see the S16 e2e).
	command.SysProcAttr = &syscall.SysProcAttr{UseCgroupFD: true, CgroupFD: int(harness.outerFile.Fd())}
	command.Env = append(environForRealDaemonAitest(),
		"PYTHONPATH="+filepath.Dir(harness.aitestDir),
		"PYTHONDONTWRITEBYTECODE=1",
		"AIRA_AITEST_LIB="+harness.pythonDir,
		"AIRA_AITEST_OUTER_SCOPE="+harness.outerFile.Name(),
		"AIRA_AITEST_ADMISSION=cgroup-sub-scope",
		"AIRA_AITEST_WORKER_ADMIT_CMD="+harness.binary,
		// 256 MiB per-worker cap: ~100 MiB hold + interpreter baseline stays well
		// under it (no self-OOM), while 2-3 concurrent workers' peaks SUM past the
		// 256 MiB parent cap.
		"AIRA_AITEST_ESTIMATED_BYTES="+strconv.Itoa(int(perScopeCapBytes)),
		"AIRA_AITEST_MEASURE_DIR="+measureDir,
		"AIRA_REAL_CGROUP=1",
	)
	output, err := command.CombinedOutput()
	text := string(output)

	// Positive proof the run was CONFINED, not a silent fallback to unconfined
	// execution (a fallback pool has no worker scopes, so the whole gate would be
	// vacuous). Same disposition as the S16 pass/fail e2e.
	if strings.Contains(text, "falling back to") || strings.Contains(text, "UNCONFINED") {
		t.Fatalf("aggregate run unexpectedly fell back to unconfined execution:\n%s", text)
	}

	// FALSE-PASS TRAP: every alloc-hold test actually RAN and PASSED. A gate that
	// greened with zero workers spawned (or the suite dying) proves nothing.
	for i := 0; i < 6; i++ {
		want := "test_alloc_hold.py::test_a" + strconv.Itoa(i) + " passed"
		if !strings.Contains(text, want) {
			t.Fatalf("alloc-hold test did not run+pass (%q missing) — a worker self-OOM or a whole-suite kill:\nerr=%v\n%s", want, err, text)
		}
	}
	// The aggregate summary, driven by replayed reports: exactly 6 passed, none
	// unevaluated (a self-OOM'd worker renders its test unevaluated).
	if !strings.Contains(text, "6 passed") || !strings.Contains(text, "0 unevaluated") {
		t.Fatalf("aggregate run summary is not '6 passed / 0 unevaluated' — a worker was killed:\n%s", text)
	}

	// The parent scope's oom.group must NEVER have fired: with siblings there is no
	// shared parent cap for the aggregate to breach. (Hierarchical counter: an OOM
	// in the parent SUBTREE would surface here — but the workers are not in it.)
	if kills := readOuterMemoryEventCounter(t, outerDir, "oom_group_kill"); kills != 0 {
		t.Fatalf("parent scope oom_group_kill=%d, want 0 — a whole-suite aggregate kill fired (AIRA-229 regressed):\n%s", kills, text)
	}

	report := readPoolReport(t, measureDir, text)

	// scoped_workers >= 2: the aggregate is only meaningful with at least two live
	// workers (and it is the confined-path proof — a fallback pool scopes none).
	scoped := reportInt(t, report, "scoped_workers")
	if scoped < 2 {
		t.Fatalf("scoped_workers=%d, want >= 2 (the aggregate needs concurrent live workers; a CPU-saturated box "+
			"can starve pool growth — re-run on a quieter box before treating this as a defect):\n%s", scoped, report)
	}

	// No worker self-OOM masquerading as the no-kill result.
	if oom, ok := report["oom_group_killed"].(bool); !ok || oom {
		t.Fatalf("oom_group_killed=%v, want false (a per-worker self-OOM must not be read as a clean run):\n%s", report["oom_group_killed"], report)
	}

	samples := reportSamples(t, report)
	sliceDir := filepath.Dir(outerDir) // the harness slice (parent of the outer scope)
	var sumPeak int64
	for i, sample := range samples {
		// Gate A fold: each granted scope_path is a DIRECT child of the slice.
		scopePath, ok := sample["scope_path"].(string)
		if !ok || scopePath == "" {
			t.Fatalf("sample[%d] has no scope_path (sibling-placement anchor):\n%v", i, sample)
		}
		if got := filepath.Dir(scopePath); got != sliceDir {
			t.Fatalf("sample[%d].scope_path %q is not a direct child of the slice %q (parent dir %q) — "+
				"worker scopes must be siblings, not nested:\n%s", i, scopePath, sliceDir, got, report)
		}
		// No per-worker self-OOM.
		if oom, isBool := sample["oom"].(bool); isBool && oom {
			t.Fatalf("sample[%d] oom=true — a worker self-OOMed under its own cap:\n%s", i, report)
		}
		if peak, isNum := sample["peak"].(float64); isNum && peak > 0 {
			sumPeak += int64(peak)
		}
	}

	// parentCap is the yardstick: the memory.max a NESTED (pre-S2a) topology would
	// have shared across the workers. It is the SAME constant as the per-worker cap
	// (see perScopeCapBytes) — the sound-by-construction proof below needs worker
	// cap ≤ parentCap — and it must equal the outer scope's own memory.max, which
	// the harness writes. Read that and assert the match so a harness drift (a
	// changed outer cap that would silently invalidate parentCap) reds here.
	const parentCap = perScopeCapBytes
	if outerMax := readCgroupMemoryMax(t, outerDir); outerMax != parentCap {
		t.Fatalf("outer scope memory.max=%d != parentCap %d: the harness and this gate disagree about the "+
			"parent cap the proof compares against; keep them one number", outerMax, parentCap)
	}

	// SLICE POPULATION GUARD: the aggregate proof reads the slice's hierarchical
	// memory.peak, which is only a clean witness of WORKER memory if nothing else
	// charges the slice. The harness runs its daemon in-process (not a cgroup child
	// of the slice) and IsolatedScopeParent is a fresh MkdirTemp per test, so the
	// only children are the outer scope and the sibling worker scopes; assert that
	// so a stray population cannot inflate slice.peak. (Workers may already be
	// reaped by the time this runs — the relay cgroup.kills+rmdirs each on peer-EOF
	// — so this mainly rejects a PERSISTENT third scope; the per-sample Gate A fold
	// above already pins that the live workers were direct slice children.)
	entries, err := os.ReadDir(sliceDir)
	if err != nil {
		t.Fatalf("cannot enumerate the slice dir %q (slice-population guard is unevaluated): %v", sliceDir, err)
	}
	outerBase := filepath.Base(outerDir)
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		name := entry.Name()
		if name == outerBase || strings.HasPrefix(name, ".aira-CONFINE-") {
			continue // the outer scope, or a sibling confine/worker scope
		}
		t.Fatalf("slice %q has an unexpected child cgroup %q (neither the outer scope %q nor a `.aira-CONFINE-` "+
			"worker scope) — it would charge the slice's memory.peak and invalidate the aggregate proof",
			sliceDir, name, outerBase)
	}

	// POSITIVE AGGREGATE PROOF (sound-by-construction, concurrency-witnessing):
	// slice.peak − outer.peak > parentCap. slice.peak is the max over the run of the
	// instantaneous total charged to the whole slice subtree; outer.peak upper-bounds
	// the supervisor's usage at that instant; the difference is a lower bound on the
	// worker memory resident SIMULTANEOUSLY. Because a single worker is capped at
	// parentCap, only ≥2 concurrent workers can drive the difference strictly past
	// parentCap — the exact concurrent over-cap a shared parent would oom.group-kill,
	// which here did not fire (oom_group_kill == 0, asserted above). Sequential
	// workers cannot satisfy this (slice.peak ≤ outer.peak + parentCap), so it is
	// non-porous independently of the nesting mutation.
	slicePeak := readCgroupMemoryPeak(t, sliceDir)
	outerPeak := readCgroupMemoryPeak(t, outerDir)
	concurrentWorkerFloor := slicePeak - outerPeak
	if concurrentWorkerFloor <= parentCap {
		t.Fatalf("slice.peak(%d) − outer.peak(%d) = %d <= parentCap %d: the run never held more than the parent "+
			"cap in CONCURRENT worker memory, so 'no whole-suite kill' is not a meaningful result — raise the "+
			"hold or worker count, or re-run on a quieter box that lets the pool grow:\n%s",
			slicePeak, outerPeak, concurrentWorkerFloor, parentCap, report)
	}

	// Corroboration only (NOT the proof): Σ(per-worker peak) > parentCap. Sequential
	// workers also satisfy this — each peak is recorded independently, at possibly
	// different instants — so it witnesses aggregate volume, not concurrency. Kept as
	// a weaker secondary signal beside the sound witness above.
	if sumPeak <= parentCap {
		t.Fatalf("Σ(worker peak)=%d <= parent cap %d: the aggregate never reached a shared cap's worth of total "+
			"work — raise the hold or worker count:\n%s", sumPeak, parentCap, report)
	}
	t.Logf("AIRA-229 aggregate: %d scoped workers, slice.peak−outer.peak=%d > parentCap %d (concurrency witness), "+
		"Σ(peak)=%d (corroboration), parent oom_group_kill=0",
		scoped, concurrentWorkerFloor, parentCap, sumPeak)
}

// readPoolReport reads and parses the measurement pool-report.json a confined run
// with AIRA_AITEST_MEASURE_DIR writes, failing loudly (with the run output) if it
// is absent — its absence means the pool never scoped a worker.
func readPoolReport(t *testing.T, measureDir, runOutput string) map[string]interface{} {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(measureDir, "pool-report.json"))
	if err != nil {
		t.Fatalf("pool-report.json was not written (the pool scoped no worker?): %v\nrun output:\n%s", err, runOutput)
	}
	var report map[string]interface{}
	if err := json.Unmarshal(raw, &report); err != nil {
		t.Fatalf("pool-report.json is not valid JSON: %v\n%s", err, raw)
	}
	t.Logf("pool-report.json:\n%s", raw)
	return report
}

func reportInt(t *testing.T, report map[string]interface{}, field string) int {
	t.Helper()
	value, ok := report[field]
	if !ok {
		t.Fatalf("pool-report.json missing %q", field)
	}
	n, ok := value.(float64)
	if !ok {
		t.Fatalf("pool-report.json %q is not numeric: %v", field, value)
	}
	return int(n)
}

func reportSamples(t *testing.T, report map[string]interface{}) []map[string]interface{} {
	t.Helper()
	raw, ok := report["worker_peak_rss_samples"].([]interface{})
	if !ok || len(raw) == 0 {
		t.Fatalf("worker_peak_rss_samples is missing/empty on a confined run: %v", report)
	}
	out := make([]map[string]interface{}, 0, len(raw))
	for i, entry := range raw {
		sample, ok := entry.(map[string]interface{})
		if !ok {
			t.Fatalf("worker_peak_rss_samples[%d] is not an object: %v", i, entry)
		}
		out = append(out, sample)
	}
	return out
}

// environForRealDaemonAitestWithScopeID is environForRealDaemonAitest with an
// EXPLICIT AIRA_CONFINE_SCOPE_ID (filtering any inherited one), so two concurrent
// supervisors in the AIRA-232 gate publish DISTINCT parent scope-ids — with
// distinct pid slots — and their worker scope NAMES (which embed that pid, Task 1)
// are attributable to the supervisor that spawned them.
func environForRealDaemonAitestWithScopeID(scopeID string) []string {
	env := os.Environ()
	filtered := make([]string, 0, len(env)+1)
	for _, entry := range env {
		if strings.HasPrefix(entry, "AIRA_CONFINE_SCOPE_ID=") {
			continue
		}
		filtered = append(filtered, entry)
	}
	return append(filtered, "AIRA_CONFINE_SCOPE_ID="+scopeID)
}

// leasePidSlot extracts the parent-supervisor pid embedded in a worker lease's
// scope-id (CONFINE-aitest-w<seq>-<parentPid>-<stamp>, Task 1) via the exported
// parser. Returns 0 when the key is not a parseable worker scope-id.
func leasePidSlot(scopeID string) int {
	_, pid, _, _, ok := runner.ParseConfineScopeID(scopeID)
	if !ok {
		return 0
	}
	return pid
}

// readCgroupProcs returns the PIDs currently in a cgroup scope's cgroup.procs.
// A non-empty result on a worker scope is positive proof the scope is POPULATED
// (a live process is running its test there), which is what makes the parent-kill
// gate's orphan check meaningful — a kill of an already-empty scope would prove
// nothing.
func readCgroupProcs(dir string) []int {
	data, err := os.ReadFile(filepath.Join(dir, "cgroup.procs"))
	if err != nil {
		return nil
	}
	var pids []int
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		if line == "" {
			continue
		}
		if pid, convErr := strconv.Atoi(line); convErr == nil {
			pids = append(pids, pid)
		}
	}
	return pids
}

// processAlive reports whether a pid is a live process (signal 0 probe). Used
// only as corroboration of the primary "scope dir is gone" proof — pid reuse
// makes it weaker, so the dir-removal check is authoritative (rmdir requires an
// empty cgroup, so a removed worker scope PROVES no process survived in it).
func processAlive(pid int) bool {
	return syscall.Kill(pid, 0) == nil
}

// waitScopePopulated polls a worker scope's cgroup.procs until it holds at least
// one process (the worker forked in the parent leaf and place_self'd into its
// sibling scope, then pulled a nodeid and began its blocked test — a small window
// after the lease is established). Returns the pids, or fails if the scope never
// populates.
func waitScopePopulated(t *testing.T, dir string, timeout time.Duration) []int {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		if pids := readCgroupProcs(dir); len(pids) > 0 {
			return pids
		}
		if time.Now().After(deadline) {
			t.Fatalf("worker scope %q never became populated (no process migrated into it) — cannot prove orphan cleanup on an empty scope", dir)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// populatedWorkerScope is one live worker sibling scope captured before the
// parent kill: its directory (WorkerScopeChildPath == <slice>/.aira-<id>) and the
// pids it held while populated.
type populatedWorkerScope struct {
	id   string
	dir  string
	pids []int
}

// capturePopulatedWorkerScopes waits for each granted worker lease's sibling scope
// to become populated and records its dir + pids — the pre-kill state the orphan
// check is asserted against.
func capturePopulatedWorkerScopes(t *testing.T, leases map[string]int64, slice string) []populatedWorkerScope {
	t.Helper()
	out := make([]populatedWorkerScope, 0, len(leases))
	for id := range leases {
		dir := runner.WorkerScopeChildPath(slice, id)
		pids := waitScopePopulated(t, dir, testdeadline.Wait(15*time.Second))
		out = append(out, populatedWorkerScope{id: id, dir: dir, pids: pids})
	}
	return out
}

// assertNoOrphanAfterParentKill cgroup.kills the outer (parent) scope — which
// kills the supervisor and its relays, EOFing every worker-admit relay connection
// — then polls until every worker scope directory is GONE (the daemon
// cgroup.kill+rmdir'd it on the relay's peer-EOF, §16b) and every captured worker
// pid is dead. A directory that stays is a leaked worker scope: the exact orphan
// §16b exists to prevent. The daemon must be ALIVE at kill time (its peer-EOF
// hook does the kill), so callers keep the server running across this call.
func assertNoOrphanAfterParentKill(t *testing.T, outer string, workers []populatedWorkerScope) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(outer, "cgroup.kill"), []byte("1"), 0o644); err != nil {
		t.Fatalf("cgroup.kill the parent (outer) scope %q: %v", outer, err)
	}
	deadline := time.Now().Add(testdeadline.Wait(25 * time.Second))
	for {
		remaining := []string{}
		for _, w := range workers {
			if _, err := os.Stat(w.dir); !os.IsNotExist(err) {
				remaining = append(remaining, w.dir)
			}
		}
		if len(remaining) == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("after the parent kill %d worker scope dir(s) were NOT removed — the daemon did not "+
				"kill+rmdir them on peer-EOF (orphaned workers, §16b regressed): %v", len(remaining), remaining)
		}
		time.Sleep(50 * time.Millisecond)
	}
	// Corroboration: every process we saw in a worker scope is now dead. (The
	// dir-removal above is the authoritative proof — rmdir needs an empty cgroup.)
	for _, w := range workers {
		for _, pid := range w.pids {
			if processAlive(pid) {
				t.Fatalf("worker %s process %d survived the parent kill (its scope dir was removed but the pid is still alive — pid reuse, or a real leak)", w.id, pid)
			}
		}
	}
}

// startBlockingAitestPool launches a real pytest aitest pool whose fixture tests
// BLOCK on a never-created sentinel, so the workers stay populated (mid-test) —
// the state the parent-kill orphan gate requires. Returns the run channel; the
// caller cancels the run in cleanup (the pool is killed, not released).
func startBlockingAitestPool(t *testing.T, h *restartGateHarness, runCtx context.Context, workers int) chan runOutcome {
	t.Helper()
	// A sentinel path that is NEVER created: every fixture test blocks on it until
	// the generous inner timeout, so the workers are populated when we kill.
	sentinel := filepath.Join(t.TempDir(), "never-released")
	command := exec.CommandContext(runCtx, h.pytest, "-q", "--aitest-workers="+strconv.Itoa(workers))
	command.Dir = filepath.Join(h.aitestDir, "restart_gate_testdata")
	command.WaitDelay = 15 * time.Second
	command.SysProcAttr = &syscall.SysProcAttr{UseCgroupFD: true, CgroupFD: int(h.outerFile.Fd())}
	command.Env = append(environForRealDaemonAitest(),
		"PYTHONPATH="+filepath.Dir(h.aitestDir),
		"PYTHONDONTWRITEBYTECODE=1",
		"AIRA_AITEST_LIB="+h.pythonDir,
		"AIRA_AITEST_OUTER_SCOPE="+h.outer,
		"AIRA_AITEST_ADMISSION=cgroup-sub-scope",
		"AIRA_AITEST_WORKER_ADMIT_CMD="+h.binary,
		"AIRA_AITEST_ESTIMATED_BYTES="+strconv.Itoa(restartGatePytestReserve),
		"AIRA_REAL_CGROUP=1",
		"AIRA_AITEST_RESTART_SENTINEL="+sentinel,
		"AIRA_AITEST_RESTART_BLOCK_TIMEOUT=120",
	)
	runCh := make(chan runOutcome, 1)
	go func() {
		out, err := command.CombinedOutput()
		runCh <- runOutcome{out, err}
	}()
	return runCh
}

// TestRealPytestAitestParentKillLeavesNoOrphanFresh is Gate D (fresh path, P1-1):
// with two workers populated mid-test, killing the parent (cgroup.kill the outer
// scope, which kills the supervisor + relays) must leave NO orphaned worker — the
// daemon cgroup.kills + rmdirs each sibling worker scope on the relay's peer-EOF.
//
// Mutation: comment out `s.killWorkerScope(path, scopeID)` in worker_admit.go's
// fresh relay peer-EOF branch (~:549) → the worker scope dir is never removed →
// this gate reds (orphan). Recorded in the build report.
func TestRealPytestAitestParentKillLeavesNoOrphanFresh(t *testing.T) {
	h := newRestartGateHarness(t, true)
	serverA, cancelA, doneA := h.startServer(t)
	defer func() { cancelA(); awaitServerShutdown(t, doneA) }()

	runCtx, cancelRun := context.WithTimeout(context.Background(), testdeadline.Wait(2*time.Minute))
	defer cancelRun()
	runCh := startBlockingAitestPool(t, h, runCtx, 2)

	leasesA := waitLeaseCount(t, serverA, 2, testdeadline.Wait(45*time.Second), runCh)
	if len(leasesA) != 2 {
		cancelRun()
		o := <-runCh
		t.Fatalf("pool did not fill to 2 worker leases (got %d: %v)\n%s", len(leasesA), leasesA, o.output)
	}
	workers := capturePopulatedWorkerScopes(t, leasesA, h.parent)

	assertNoOrphanAfterParentKill(t, h.outer, workers)

	// The run was killed with the parent; drain it so the goroutine does not leak.
	cancelRun()
	select {
	case <-runCh:
	case <-testdeadline.After(20 * time.Second):
	}
}

// TestRealPytestAitestParentKillLeavesNoOrphanPostRestart is Gate D (post-restart
// path, P1-A): the SAME orphan-free guarantee must hold after a daemon restart.
// Two workers fill on A; A is restarted to B; the survivors' relay keepers
// reconnect and RE-DECLARE into B (serveReDeclare); THEN the parent is killed —
// and B must kill+rmdir the re-declared workers' scopes on the re-declare
// connection's peer-EOF. This exercises serveReDeclare's kill hook, which the
// fresh path does not.
//
// Mutation: comment out `s.killWorkerScope(path, charge.ScopeID)` in
// serveReDeclare's peer-EOF branch (server.go ~:1066) → after the restart the
// re-declared worker scopes are orphaned on the parent kill → this gate reds
// while the fresh gate above stays green. Recorded in the build report.
func TestRealPytestAitestParentKillLeavesNoOrphanPostRestart(t *testing.T) {
	h := newRestartGateHarness(t, true)
	serverA, cancelA, doneA := h.startServer(t)

	runCtx, cancelRun := context.WithTimeout(context.Background(), testdeadline.Wait(2*time.Minute))
	defer cancelRun()
	runCh := startBlockingAitestPool(t, h, runCtx, 2)

	leasesA := waitLeaseCount(t, serverA, 2, testdeadline.Wait(45*time.Second), runCh)
	if len(leasesA) != 2 {
		cancelRun()
		o := <-runCh
		t.Fatalf("pool did not fill to 2 worker leases on A (got %d: %v)\n%s", len(leasesA), leasesA, o.output)
	}
	wantKeys := make(map[string]int64, len(leasesA))
	for id, reserve := range leasesA {
		wantKeys[id] = reserve
	}

	// Restart A -> B with a daemon-down gap; the survivors' relay keepers reconnect
	// (2/sec) and re-declare into B's empty-started ledger.
	cancelA()
	awaitServerShutdown(t, doneA)
	time.Sleep(testdeadline.Wait(3 * time.Second))
	serverB, cancelB, doneB := h.startServer(t)
	defer func() { cancelB(); awaitServerShutdown(t, doneB) }()

	leasesB := waitLeaseCount(t, serverB, 2, testdeadline.Wait(30*time.Second), runCh)
	if len(leasesB) != 2 {
		cancelRun()
		o := <-runCh
		t.Fatalf("B holds %d worker leases after the restart, want exactly the 2 survivors re-anchored: %v\n%s", len(leasesB), leasesB, o.output)
	}
	// The re-declared leases must carry the VERBATIM survivor scope-ids (else the
	// scope-dir the kill hook targets would not exist).
	for id := range wantKeys {
		if _, present := leasesB[id]; !present {
			cancelRun()
			t.Fatalf("B's ledger %v is missing the survivor's verbatim scope-id %q (a transformed re-declare key would misdirect the kill)", leasesB, id)
		}
	}
	workers := capturePopulatedWorkerScopes(t, leasesB, h.parent)

	// B is alive; killing the parent now must reach B's serveReDeclare peer-EOF hook.
	assertNoOrphanAfterParentKill(t, h.outer, workers)

	cancelRun()
	select {
	case <-runCh:
	case <-testdeadline.After(20 * time.Second):
	}
}

// TestRealPytestAitestMultiSupervisorSafe is Gate C (AIRA-232): two independent
// aitest supervisors (the `make -j` shape) under one slice, driven by one daemon,
// cannot jointly breach the slice ledger. The ledger is pinned so exactly TWO
// pytest workers fit; both supervisors continuously demand workers (8 slow tests
// each), so their combined demand far exceeds the ceiling. The daemon serialises
// them against the ONE ledger: at every sampled instant the combined granted
// worker set is <= 2 and Σ(reserve) <= 2 workers' worth — there is no client
// aggregate, no sibling-sum, no cross-process TOCTOU that could let each
// supervisor admit up to its own limit independently.
//
// Non-porous:
//   - EXACT serialisation bound sampled THROUGHOUT the run: combined granted
//     leases <= 2 and Σ(reserve) <= 2*restartGatePytestReserve at every poll. If
//     AIRA-232 regressed to per-supervisor guarding, two supervisors would each
//     admit up to the ceiling → combined 3-4+ workers → this reds.
//   - Both supervisors actually competed: worker leases from BOTH distinct pid
//     slots (each supervisor's own AIRA_CONFINE_SCOPE_ID pid) were observed, and
//     every worker scope-id parses (Task 1 naming). A gate where only one
//     supervisor ever ran would prove nothing about multi-supervisor safety.
//   - Both suites completed with every test PASSED (the count trap) and neither
//     parent scope's oom.group ever fired.
func TestRealPytestAitestMultiSupervisorSafe(t *testing.T) {
	h := newRestartGateHarness(t, true)

	// A SECOND parent (outer) scope under the same slice for the second supervisor,
	// created exactly like the harness's first (finite memory.max, no +memory
	// pre-delegation — the pytest supervisor runs directly in it, no sub-scopes).
	outer2 := filepath.Join(h.parent, ".aira-outer-restartgate-2")
	if err := os.Mkdir(outer2, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(outer2, "memory.max"), []byte("805306368"), 0o644); err != nil {
		t.Fatalf("cannot set outer2 memory.max: %v", err)
	}
	outer2File, err := os.Open(outer2)
	if err != nil {
		t.Fatal(err)
	}
	defer outer2File.Close()

	server, cancel, done := h.startServer(t)
	defer func() { cancel(); awaitServerShutdown(t, done) }()

	const (
		scopeIDA = "CONFINE-e2e-outerA-111111-1"
		scopeIDB = "CONFINE-e2e-outerB-222222-1"
		pidA     = 111111
		pidB     = 222222
	)

	runCtx, cancelRun := context.WithTimeout(context.Background(), testdeadline.Wait(3*time.Minute))
	defer cancelRun()

	launch := func(scopeID string, outerFile *os.File, outerPath string) chan runOutcome {
		command := exec.CommandContext(runCtx, h.pytest, "-q", "--aitest-workers=3", "test_slow_passing.py")
		command.Dir = filepath.Join(h.aitestDir, "testdata")
		command.WaitDelay = 15 * time.Second
		command.SysProcAttr = &syscall.SysProcAttr{UseCgroupFD: true, CgroupFD: int(outerFile.Fd())}
		command.Env = append(environForRealDaemonAitestWithScopeID(scopeID),
			"PYTHONPATH="+filepath.Dir(h.aitestDir),
			"PYTHONDONTWRITEBYTECODE=1",
			"AIRA_AITEST_LIB="+h.pythonDir,
			"AIRA_AITEST_OUTER_SCOPE="+outerPath,
			"AIRA_AITEST_ADMISSION=cgroup-sub-scope",
			"AIRA_AITEST_WORKER_ADMIT_CMD="+h.binary,
			"AIRA_AITEST_ESTIMATED_BYTES="+strconv.Itoa(restartGatePytestReserve),
			"AIRA_REAL_CGROUP=1",
		)
		ch := make(chan runOutcome, 1)
		go func() {
			out, runErr := command.CombinedOutput()
			ch <- runOutcome{out, runErr}
		}()
		return ch
	}

	// Sample the ledger THROUGHOUT both runs: the max combined granted count, the
	// max combined Σ(reserve), and the union of pid slots seen. Stops when both
	// runs have completed.
	const ceiling = int64(2) * restartGatePytestReserve
	var maxCount int
	var maxSum int64
	pidSlots := map[int]bool{}
	sampleDone := make(chan struct{})
	sampleStopped := make(chan struct{})
	go func() {
		defer close(sampleStopped)
		violation := ""
		for {
			leases := server.GrantedLeasesForTest()
			var sum int64
			for id, reserve := range leases {
				sum += reserve
				if slot := leasePidSlot(id); slot != 0 {
					pidSlots[slot] = true
				}
			}
			if len(leases) > maxCount {
				maxCount = len(leases)
			}
			if sum > maxSum {
				maxSum = sum
			}
			// Record the FIRST breach so a regression is reported with the offending
			// snapshot rather than only a max.
			if (len(leases) > 2 || sum > ceiling) && violation == "" {
				violation = leasesSnapshotString(leases, sum)
				t.Errorf("AIRA-232 BREACH: the daemon granted %d workers (Σ reserve %d) across two supervisors, "+
					"exceeding the 2-worker / %d-byte ceiling — two supervisors jointly breached the one slice: %s",
					len(leases), sum, ceiling, violation)
			}
			select {
			case <-sampleDone:
				return
			case <-time.After(30 * time.Millisecond):
			}
		}
	}()

	chA := launch(scopeIDA, h.outerFile, h.outer)
	chB := launch(scopeIDB, outer2File, outer2)

	var outA, outB runOutcome
	got := 0
	for got < 2 {
		select {
		case outA = <-chA:
			chA = nil
			got++
		case outB = <-chB:
			chB = nil
			got++
		case <-testdeadline.After(3 * time.Minute):
			cancelRun()
			close(sampleDone)
			<-sampleStopped
			t.Fatalf("multi-supervisor run did not complete (A done=%v B done=%v)", chA == nil, chB == nil)
		}
	}
	close(sampleDone)
	<-sampleStopped

	// Both suites completed with every slow test passed (the count trap) and no
	// silent fallback to unconfined execution on either.
	for name, o := range map[string]runOutcome{"A": outA, "B": outB} {
		text := string(o.output)
		if strings.Contains(text, "falling back to") || strings.Contains(text, "UNCONFINED") {
			t.Fatalf("supervisor %s fell back to unconfined execution (containment stripped):\n%s", name, text)
		}
		if !strings.Contains(text, "8 passed") {
			t.Fatalf("supervisor %s did not report all 8 slow tests passed:\nerr=%v\n%s", name, o.err, text)
		}
		if o.err != nil {
			t.Fatalf("supervisor %s exited nonzero: %v\n%s", name, o.err, text)
		}
	}

	// Both supervisors genuinely competed for workers under the one daemon: worker
	// leases from BOTH pid slots were observed.
	if !pidSlots[pidA] || !pidSlots[pidB] {
		t.Fatalf("did not observe worker leases from BOTH supervisors (pid slots seen: %v; want %d and %d) — "+
			"only one supervisor ever ran, so multi-supervisor serialisation was not exercised", pidSlots, pidA, pidB)
	}

	// Neither parent scope's oom.group fired.
	for name, dir := range map[string]string{"A": h.outer, "B": outer2} {
		if kills := readOuterMemoryEventCounter(t, dir, "oom_group_kill"); kills != 0 {
			t.Fatalf("parent scope %s oom_group_kill=%d, want 0 — a whole-suite kill fired", name, kills)
		}
	}
	t.Logf("AIRA-232: two supervisors serialised — max combined granted workers=%d, max Σ(reserve)=%d (ceiling %d), pid slots=%v",
		maxCount, maxSum, ceiling, pidSlots)
}

// leasesSnapshotString renders a ledger snapshot for a breach report.
func leasesSnapshotString(leases map[string]int64, sum int64) string {
	parts := make([]string, 0, len(leases))
	for id, reserve := range leases {
		parts = append(parts, id+"="+strconv.FormatInt(reserve, 10))
	}
	return "Σ=" + strconv.FormatInt(sum, 10) + " {" + strings.Join(parts, ", ") + "}"
}

// confineTrailerLine extracts the single `confine: slice=...` operator trailer
// FormatConfineStatus emits at job end (it carries scope-integrity, and any
// escaped-pid/escaped-cgroup). Fails if absent — the job never produced a
// trailer means it never ran a placed scope, and a scope-integrity assertion
// against a missing trailer would be a false pass.
func confineTrailerLine(t *testing.T, text string) string {
	t.Helper()
	for _, line := range strings.Split(text, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "confine: slice=") {
			return strings.TrimSpace(line)
		}
	}
	t.Fatalf("no `confine: slice=...` trailer in the delegate run output (the job never ran a placed scope):\n%s", text)
	return ""
}

// TestRealPytestAitestDelegateRunScopeIntegrityNoEscape is Gate E (P1-2,
// §16.2) — the load-bearing escape-attestation gate, on a REAL `aira confine
// --delegate-ram` run of the built binary (NOT the CgroupFD harness, which has no
// runner attestation): the supervisor runs in the parent confine scope, its
// workers fork there and place_self into first-class SIBLING scopes under the
// slice. That migration must attest scope-integrity=UNVERIFIED with NO
// descendant_escape — never descendant-escaped/migrated (and never `contained`,
// which is leader-only by the #20 design: a supervisor always has relay
// descendants).
//
// The observation is made RELIABLE (not a sampler race) by the delegate_escape_
// testdata fixture: it registers an os.register_at_fork(after_in_child) handler
// that dwells each forked worker ~250 ms in the SUPERVISOR's scope before
// place_self (a documented window — worker.py notes at-fork handlers run in the
// child before os.fork() returns to aitest, while it is still unplaced), and each
// test sleeps ~400 ms so the migrated worker stays alive in its sibling scope. The
// confine supervisor's 50 ms membership sampler therefore reliably catches the
// worker in-scope during the dwell AND alive in its sibling on a later sample —
// the exact migration the exemption must not witness as an escape. Without the
// dwell the sub-millisecond fork→place_self window slips between samples and the
// migration is never observed, so this gate would pass VACUOUSLY (verified: with
// the plain blocking fixture the drop-exemption mutation did NOT surface an escape
// — the teardown attestation enumerates only IN-SCOPE members, and migrated
// siblings are not in the scope). The confine job runs against the ISOLATED
// harness slice (--slice <isolated parent>, the same parent the harness daemon
// admits workers against), never the production aira.slice.
//
// NON-VACUITY is proven by the drop-exemption mutation (recorded in the build
// report, run as a discovery): revert T5's isOwnAitestWorkerScopePath exemption
// (return false) → with the dwell fixture this run reads scope-integrity=
// descendant-escaped with escaped-cgroup=<a .aira-CONFINE-aitest-w... path> →
// the gate reds. (The chokepoint also has DETERMINISTIC unit coverage in
// worker_escape_exemption_linux_test.go, which reds under the same mutation with
// no sampler dependence; this is the real-run end-to-end complement.)
func TestRealPytestAitestDelegateRunScopeIntegrityNoEscape(t *testing.T) {
	harness := newRealDaemonAndCgroupTestHarness(t)
	slice := filepath.Dir(harness.outerFile.Name()) // the isolated parent = the daemon's resolved slice

	runCtx, cancelRun := context.WithTimeout(context.Background(), testdeadline.Wait(2*time.Minute))
	defer cancelRun()
	command := exec.CommandContext(runCtx, harness.binary, "confine",
		"--slice", slice, "--delegate-ram",
		"--", harness.pytest, "-q", "--aitest-workers=2", "test_escape.py")
	command.Dir = filepath.Join(harness.aitestDir, "delegate_escape_testdata")
	command.WaitDelay = 20 * time.Second
	// aira confine --delegate-ram publishes the aitest coordinates itself
	// (AIRA_AITEST_LIB via ExtractAitest, WORKER_ADMIT_CMD=self, OUTER_SCOPE=its own
	// scope, ADMISSION), and mints its own AIRA_CONFINE_SCOPE_ID (whose pid slot ==
	// the confine supervisor's os.Getpid(), the monitor) — so the worker names embed
	// the monitor's own pid and the local exemption fires. The dwell / real-cgroup
	// keys pass through (not in the fixed strip set). XDG_* (daemon socket, state)
	// ride in on os.Environ() from the harness's t.Setenv.
	command.Env = append(os.Environ(),
		"PYTHONDONTWRITEBYTECODE=1",
		"AIRA_REAL_CGROUP=1",
		"AIRA_AITEST_ESCAPE_FORK_DWELL_MS=250",
	)
	output, err := command.CombinedOutput()
	text := string(output)

	// It must NOT have fallen back to an unconfined pool: a fallback pool forks
	// workers in-scope and never migrates, so there would be no migration to
	// attest and the gate would be vacuous.
	if strings.Contains(text, "falling back to") || strings.Contains(text, "UNCONFINED") {
		t.Fatalf("delegate run fell back to unconfined execution (no sibling migration to attest):\n%s", text)
	}
	// The workers actually ran (they forked, migrated, and ran real tests): all
	// four fixture tests passed. A run where no worker ever spawned would prove
	// nothing about the migration exemption.
	if !strings.Contains(text, "4 passed") {
		t.Fatalf("delegate run did not report all 4 fixture tests passed (workers must have spawned+migrated+run):\nerr=%v\n%s", err, text)
	}

	trailer := confineTrailerLine(t, text)
	// The scope was actually placed (not an early admission/resolution error, which
	// would leave scope-integrity unset).
	if !strings.Contains(trailer, "scope=placed") {
		t.Fatalf("delegate run did not place its scope (early error?) — scope-integrity would be meaningless:\ntrailer: %s\n%s", trailer, text)
	}
	// The honest verdict: unverified (the supervisor has relay descendants), with
	// NO escape. NOT descendant-escaped/migrated, NOT an escaped-pid, NOT contained.
	if !strings.Contains(trailer, "scope-integrity=unverified") {
		t.Fatalf("delegate run scope-integrity is not 'unverified' (the worker migration must attest unverified-with-no-escape):\ntrailer: %s\n%s", trailer, text)
	}
	for _, forbidden := range []string{"scope-integrity=descendant-escaped", "scope-integrity=migrated", "escaped-pid=", "escaped-cgroup="} {
		if strings.Contains(trailer, forbidden) {
			t.Fatalf("delegate run wrongly reported %q — the worker fork->place_self migration into its own sibling scope must be EXEMPTED, not witnessed as an escape:\ntrailer: %s", forbidden, trailer)
		}
	}
	t.Logf("Gate E: delegate run attested %q", trailer)
}
