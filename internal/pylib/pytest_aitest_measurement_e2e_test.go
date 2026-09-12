// AIRA-230 v0.7 S1 slice v7-4: the measurement harness's real-cgroup leg.
//
// This is a BRANCH-EXIT GATE test (it cannot run in ci-shim CI, exactly like the
// v7-1 outer-cap guard e2e): it stands up a real private protocol-11 daemon and a
// real delegated outer cgroup via newRealDaemonAndCgroupTestHarness, runs a real
// confined aitest pool with the measurement report ENABLED
// (AIRA_AITEST_MEASURE_DIR), and verifies the report channel surfaces real,
// HONEST numbers end to end -- a real value where the kernel exposes one, and
// "unevaluated" (never a fabricated 0) where it does not.
//
// It is the executable reproduction the operator harness
// (docs/dev/aira-230-v74-measurement-repro.sh) drives; the numbers it logs are
// what set v0.7's tunables (the 256 MiB default, the per-worker headroom, the
// outer-cap allowance base/per_relay + margin, the watermark fraction, the
// MAX_TESTS fork). It reuses the reads the pool already makes (no new sampler).
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

// measureWorkers is the --aitest-workers value, from AIRA_AITEST_MEASURE_WORKERS
// (default "1"), so the operator harness can run a FULL-POOL sub-run: the
// supervisor_peak_rss read is specified "at full pool" for the allowance
// base + per_relay terms (v7-1/§8), and the relays (plain Popen children that
// never leave .aira-supervisor) are charged there too -- so peak@N-workers minus
// peak@1-worker isolates the per-relay term. The window knobs
// (AIRA_AITEST_WORKER_MAX_SECONDS etc.) need no forwarding: they ride through on
// os.Environ() below, and exec dedups to the last value for a key, so the
// harness-set value wins with nothing special here.
func measureWorkers() string {
	if value := strings.TrimSpace(os.Getenv("AIRA_AITEST_MEASURE_WORKERS")); value != "" {
		return value
	}
	return "1"
}

// assertHonestByteValue fails only if a report field is present as a fabricated
// non-positive number: it must be either a positive byte count (a real reading)
// or the string "unevaluated" (the kernel exposed nothing). This is the AIRA
// honesty rule, checked on the real run rather than only on the unit doubles.
func assertHonestByteValue(t *testing.T, name string, value interface{}) {
	t.Helper()
	switch v := value.(type) {
	case string:
		if v != "unevaluated" {
			t.Fatalf("%s: a string value must be exactly \"unevaluated\", got %q", name, v)
		}
	case float64:
		if v <= 0 {
			t.Fatalf("%s: a numeric value must be a positive byte count, got %v (a fabricated 0 is forbidden)", name, v)
		}
	default:
		t.Fatalf("%s: unexpected JSON type %T (%v)", name, value, value)
	}
}

func TestRealPytestAitestMeasurementReport(t *testing.T) {
	harness := newRealDaemonAndCgroupTestHarness(t)

	measureDir := filepath.Join(t.TempDir(), "measure")
	if err := os.MkdirAll(measureDir, 0o755); err != nil {
		t.Fatal(err)
	}

	// test_slow_passing.py: 8 tests that each sleep ~1.5 s. With --aitest-workers=1
	// one worker runs them in sequence, so its per-test memory.current sidecar is a
	// multi-sample RESIDUE curve and its retirement peak is a real per-worker datapoint.
	// Under the short-window sub-run (AIRA_AITEST_WORKER_MAX_SECONDS=10 from the
	// operator harness) the worker recycles mid-suite, exercising turnover too.
	runCtx, cancelRun := context.WithTimeout(context.Background(), testdeadline.Wait(90*time.Second))
	defer cancelRun()
	workers := measureWorkers()
	t.Logf("measurement run: --aitest-workers=%s (AIRA_AITEST_MEASURE_WORKERS)", workers)
	command := exec.CommandContext(runCtx, harness.pytest, "-q", "--aitest-workers="+workers, "test_slow_passing.py")
	command.Dir = filepath.Join(harness.aitestDir, "testdata")
	command.WaitDelay = 15 * time.Second // load-bearing on a real-cgroup run (see the pass/fail e2e).
	command.SysProcAttr = &syscall.SysProcAttr{UseCgroupFD: true, CgroupFD: int(harness.outerFile.Fd())}
	command.Env = append(os.Environ(),
		"PYTHONPATH="+filepath.Dir(harness.aitestDir),
		"PYTHONDONTWRITEBYTECODE=1",
		"AIRA_AITEST_LIB="+harness.pythonDir,
		// S2a: outer scope + admission grade published in the environment.
		"AIRA_AITEST_OUTER_SCOPE="+harness.outerFile.Name(),
		"AIRA_AITEST_ADMISSION=cgroup-sub-scope",
		"AIRA_AITEST_WORKER_ADMIT_CMD="+harness.binary,
		"AIRA_AITEST_ESTIMATED_BYTES="+strconv.Itoa(32<<20),
		"AIRA_AITEST_MEASURE_DIR="+measureDir,
		"AIRA_REAL_CGROUP=1",
	)

	output, runErr := command.CombinedOutput()
	text := string(output)
	// A non-zero exit is not itself a measurement failure (the report-written
	// assertions below are the real check), but it is logged so a flake is
	// attributable.
	t.Logf("pytest measurement run (exit err=%v) output:\n%s", runErr, text)

	// Positive proof the run was CONFINED, not a silent fallback (a fallback pool
	// has no cgroup scopes, so every value would be unevaluated and the run would
	// measure nothing). Same disposition as the pass/fail e2e.
	if strings.Contains(text, "falling back to") || strings.Contains(text, "UNCONFINED") {
		t.Fatalf("measurement run unexpectedly fell back to unconfined execution (nothing to measure):\n%s", text)
	}

	reportPath := filepath.Join(measureDir, "pool-report.json")
	raw, readErr := os.ReadFile(reportPath)
	if readErr != nil {
		t.Fatalf("measurement report was not written to %s: %v\nrun output:\n%s", reportPath, readErr, text)
	}
	var report map[string]interface{}
	if err := json.Unmarshal(raw, &report); err != nil {
		t.Fatalf("measurement report is not valid JSON: %v\n%s", err, raw)
	}
	t.Logf("pool-report.json:\n%s", raw)

	// The ONE new read, and the per-worker peak the pool already made: both must be
	// HONEST (a positive byte count, or "unevaluated" -- never a fabricated 0).
	for _, field := range []string{"supervisor_peak_rss", "worker_peak_rss_max", "worker_budget"} {
		value, ok := report[field]
		if !ok {
			t.Fatalf("measurement report is missing %q: %s", field, raw)
		}
		assertHonestByteValue(t, field, value)
	}

	// P1 regression gate (S2a/T4). The honest-value check above ACCEPTS
	// worker_peak_rss_max == "unevaluated", so it cannot see a silently-zeroed
	// pool-peak channel. Post-T4 the daemon kill+rmdirs the worker scope on the
	// relay-EOF that _retire_worker's stdin.close() triggers; if the supervisor's
	// memory.peak read is placed after that close it races the sub-ms rmdir and
	// loses (measured: sample_count 1 -> 0). A confined run MUST fold at least one
	// real per-worker retirement peak, so require it -- this reds against that
	// read-ordering regression and greens with the fixed ordering.
	sampleCount, ok := report["worker_peak_rss_sample_count"]
	if !ok {
		t.Fatalf("measurement report is missing %q: %s", "worker_peak_rss_sample_count", raw)
	}
	if n, isNum := sampleCount.(float64); !isNum || n < 1 {
		t.Fatalf("worker_peak_rss_sample_count = %v on a confined run; want >= 1 "+
			"(the pool-peak channel was silently zeroed -- see _retire_worker's peak-read ordering vs the relay close):\n%s",
			sampleCount, raw)
	}

	// The per-test memory.current sidecar(s): log every line so the residue curve
	// is visible in the run output, and confirm a confined run produced at least
	// one real (non-"unevaluated") reading somewhere (else the report is vacuous).
	tsvs, _ := filepath.Glob(filepath.Join(measureDir, "worker-*.tsv"))
	if len(tsvs) == 0 {
		t.Fatalf("no per-worker memory.current sidecar was written under %s", measureDir)
	}
	sawRealCurrent := false
	for _, tsv := range tsvs {
		body, _ := os.ReadFile(tsv)
		t.Logf("%s:\n%s", filepath.Base(tsv), body)
		for _, line := range strings.Split(strings.TrimSpace(string(body)), "\n") {
			fields := strings.Split(line, "\t")
			if len(fields) == 3 && fields[1] != "unevaluated" {
				sawRealCurrent = true
			}
		}
	}
	if !sawRealCurrent {
		t.Fatalf("every per-test memory.current was 'unevaluated' on a confined run -- "+
			"the read or the scope path is wrong (sidecars: %v)", tsvs)
	}
}
