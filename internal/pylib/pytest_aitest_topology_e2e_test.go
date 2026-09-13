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

// TestRealPytestAitestNoWholeSuiteKillOnAggregate is Gate B (AIRA-229): several
// workers that each allocate AND HOLD ~100 MiB run concurrently under a 256 MiB
// parent scope. Under the sibling topology each worker charges the 16 GiB slice
// and its own 256 MiB cap, so the ~256 MiB parent (supervisor + relays only) is
// never approached and its oom.group never fires — every test PASSES. The
// positive, mutation-independent proof is that the SUM of the per-worker peaks
// EXCEEDS the parent cap: a shared parent cap of that size WOULD have been
// breached, yet the parent's oom_group_kill stays 0.
//
// Non-porous:
//   - False-pass trap: assert all 6 alloc-hold tests actually ran and PASSED
//     (a gate that greened on zero workers / zero tests would prove nothing) and
//     scoped_workers >= 2 (the aggregate needs at least two live workers).
//   - Positive aggregate proof: Σ(worker peak) > parent memory.max, so the
//     no-kill result is meaningful (the load was genuinely capable of breaching a
//     shared cap), not an artefact of a tiny workload.
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
		"AIRA_AITEST_ESTIMATED_BYTES="+strconv.Itoa(256<<20),
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

	// POSITIVE AGGREGATE PROOF: the sum of the per-worker peaks EXCEEDS the parent
	// cap, so the no-kill result is meaningful — the workload genuinely could have
	// breached a shared 256 MiB parent cap, and did not because the workers are
	// siblings under the slice. This holds independently of the nesting mutation.
	const parentCap = int64(256) << 20
	if sumPeak <= parentCap {
		t.Fatalf("Σ(worker peak)=%d <= parent cap %d: the aggregate never actually exceeded a shared cap, so "+
			"'no whole-suite kill' is not a meaningful result here — raise the hold or worker count:\n%s", sumPeak, parentCap, report)
	}
	t.Logf("AIRA-229 aggregate: %d scoped workers, Σ(peak)=%d bytes > parent cap %d bytes, parent oom_group_kill=0",
		scoped, sumPeak, parentCap)
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
