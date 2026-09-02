//go:build linux

// package pylib_test (external), NOT package pylib: this file imports
// aira/internal/daemon, and internal/daemon transitively imports
// internal/core, which imports internal/pylib -- the SAME cycle-avoidance
// situation this plan already solved for Task 8's
// internal/runner/worker_admit_client_linux_test.go (see that task's Files
// note: "package runner_test -- an EXTERNAL test package, so it can import
// both aira/internal/runner and aira/internal/daemon without an import
// cycle ... internal/runner itself must never import internal/daemon").
// An internal `package pylib` test file importing daemon here hits the
// identical cycle (verified: `go vet ./internal/pylib/...` reports "imports
// aira/internal/daemon from pytest_aitest_e2e_test.go / imports
// aira/internal/core from admit.go / imports aira/internal/pylib from
// command.go: import cycle not allowed in test"), so this file follows the
// same established fix: external test package, qualified pylib.* calls, and
// its own local requireRealPytest (pylib's is unexported and lives in the
// internal pylib_test files, unreachable from here by design).
package pylib_test

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"aira/internal/cgrouptest"
	"aira/internal/daemon"
	"aira/internal/pylib"
)

func requireRealPytest(t *testing.T) string {
	t.Helper()
	path, err := exec.LookPath("pytest")
	if err == nil {
		return path
	}
	if os.Getenv("AIRA_REAL_PYTEST") == "1" {
		t.Fatalf("AIRA_REAL_PYTEST=1 but pytest is unavailable: %v", err)
	}
	t.Skipf("real pytest integration requires pytest: %v", err)
	return ""
}

func TestRealPytestAitestEndToEndFallback(t *testing.T) {
	pytest := requireRealPytest(t)
	aitestDir, err := filepath.Abs("aitest")
	if err != nil {
		t.Fatal(err)
	}
	pythonDir, err := pylib.ExtractAitest()
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("XDG_DATA_HOME", t.TempDir())

	command := exec.Command(pytest, "-q", "--aitest-workers=2")
	command.Dir = filepath.Join(aitestDir, "testdata")
	command.Env = append(os.Environ(),
		"PYTHONPATH="+filepath.Dir(aitestDir),
		"PYTHONDONTWRITEBYTECODE=1",
		"AIRA_AITEST_LIB="+pythonDir,
		"AIRA_AITEST_BOOTSTRAP_CMD="+filepath.Join(t.TempDir(), "missing-aira"),
	)
	output, err := command.CombinedOutput()
	text := string(output)
	// test_fail.py's deliberate failure makes this a nonzero pytest exit --
	// expected, not a Go test failure. A Go test failure is anything OTHER
	// than a clean, complete pytest run (a crash, a missing report line).
	if !strings.Contains(text, "test_pass.py::test_one passed") {
		t.Fatalf("pytest output missing expected pass line: %v\n%s", err, text)
	}
	if !strings.Contains(text, "test_fail.py::test_deliberate_failure failed") {
		t.Fatalf("pytest output missing expected fail line: %v\n%s", err, text)
	}
	if strings.Count(text, "aira aitest:") != 1 {
		t.Fatalf("expected exactly one fallback warning: %v\n%s", err, text)
	}
	// AIRA_REAL_CGROUP is unset in this fallback run, so test_oom.py skips
	// itself -- and a skip must be its own genuine outcome, never folded
	// into "unevaluated" (a check that could not establish a result). This
	// also makes the assertion below meaningful rather than vacuous: it only
	// holds if skip is genuinely NOT unevaluated.
	if !strings.Contains(text, "test_oom.py::test_deliberate_oom skipped") {
		t.Fatalf("pytest output missing expected skipped line: %v\n%s", err, text)
	}
	// Checking for the bare substring "unevaluated" is NOT viable here: the
	// aggregate summary line pytest_runtestloop always prints (Step 3,
	// __init__.py) is "aitest: %d passed, %d failed, %d skipped, %d
	// unevaluated" -- the word "unevaluated" appears in that line even when
	// the count is genuinely 0 (as literal text "0 unevaluated"), so a bare
	// Contains(text, "unevaluated") check is always true regardless of
	// outcome and can never actually catch a regression. Assert the
	// unambiguous positive form instead: with exactly 4 known nodeids in
	// this fixture suite (test_one, test_two, test_deliberate_failure,
	// test_deliberate_oom) all individually accounted for above/here as
	// passed/failed/skipped, the unevaluated count is deterministically 0.
	if !strings.Contains(text, "0 unevaluated") {
		t.Fatalf("fallback run unexpectedly reported a nonzero unevaluated count: %s", text)
	}
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) || exitErr.ExitCode() == 0 {
		t.Fatalf("fallback run error=%v, want nonzero pytest *exec.ExitError for the deliberate failure\n%s", err, text)
	}
}

func TestRealPytestAitestEndToEndFallbackAllPassingExitsZero(t *testing.T) {
	pytest := requireRealPytest(t)
	aitestDir, err := filepath.Abs("aitest")
	if err != nil {
		t.Fatal(err)
	}
	pythonDir, err := pylib.ExtractAitest()
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("XDG_DATA_HOME", t.TempDir())

	// Passing test_pass.py explicitly prevents pytest from collecting the
	// deliberately failing and real-cgroup-only fixture files alongside it.
	command := exec.Command(pytest, "-q", "--aitest-workers=2", "test_pass.py")
	command.Dir = filepath.Join(aitestDir, "testdata")
	command.Env = append(os.Environ(),
		"PYTHONPATH="+filepath.Dir(aitestDir),
		"PYTHONDONTWRITEBYTECODE=1",
		"AIRA_AITEST_LIB="+pythonDir,
		"AIRA_AITEST_BOOTSTRAP_CMD="+filepath.Join(t.TempDir(), "missing-aira"),
	)
	output, err := command.CombinedOutput()
	text := string(output)
	if err != nil {
		t.Fatalf("all-passing fallback run exited nonzero: %v\n%s", err, text)
	}
	if !strings.Contains(text, "test_pass.py::test_one passed") || !strings.Contains(text, "test_pass.py::test_two passed") {
		t.Fatalf("pytest output missing expected passing lines:\n%s", text)
	}
	// The summary always contains the word "unevaluated", even at zero;
	// assert its numeric form so this genuinely catches lost outcomes.
	if !strings.Contains(text, "0 unevaluated") {
		t.Fatalf("all-passing fallback run unexpectedly reported unevaluated work: %s", text)
	}
}

func shortE2ERuntimeDir(t *testing.T) string {
	t.Helper()
	// A daemon socket path must fit the 108-byte AF_UNIX sun_path limit;
	// t.TempDir() embeds the (long) test name. Mirrors
	// internal/daemon/server_test.go's shortRuntimeDir exactly.
	dir, err := os.MkdirTemp("", "e2e")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return dir
}

type realDaemonAndCgroupTestHarness struct {
	pytest    string
	binary    string
	outerFile *os.File
	aitestDir string
	pythonDir string
}

func newRealDaemonAndCgroupTestHarness(t *testing.T) realDaemonAndCgroupTestHarness {
	t.Helper()
	pytest := requireRealPytest(t)
	parent := cgrouptest.IsolatedScopeParent(t)
	if err := os.WriteFile(filepath.Join(parent, "cgroup.subtree_control"), []byte("+memory"), 0o644); err != nil {
		cgrouptest.SkipOrFailRealCgroup(t, "memory controller not delegated to %s: %v", parent, err)
	}
	// Disable swap on the ancestor so a worker that exceeds its memory.max is
	// actually OOM-killed rather than silently swapped out -- with swap
	// available (verified present on this dev host), memory.max alone only
	// forces reclaim, and test_oom.py's allocation would just page out to
	// swap and report "passed" instead of ever being killed, contradicting
	// design spec section 7's stated invariant ("a worker that genuinely
	// exceeds its memory.max is actually OOM-killed"). Mirrors the existing,
	// already-committed precedent for this exact host condition:
	// internal/runner/usage_real_test.go's "Disable swap so a constrained
	// child that exceeds memory.max is OOM-killed rather than silently
	// swapping (WSL2 has swap)" and internal/runner/confine_linux_test.go's
	// identical write. Best-effort (ignored error, like both of those) and
	// harmless if this host has no swap at all. cgroup v2 charges swap usage
	// hierarchically, so capping it here on the ancestor also constrains
	// every scope nested under it (outer, and each worker sub-scope below
	// outer) without touching production cgroup-creation code.
	_ = os.WriteFile(filepath.Join(parent, "memory.swap.max"), []byte("0"), 0o644)

	outer := filepath.Join(parent, ".aira-outer-e2e")
	if err := os.Mkdir(outer, 0o755); err != nil {
		t.Fatal(err)
	}
	// A real, small, numeric cap -- readSliceMemory treats a bare "max" as
	// unreadable ("unbounded"), so admission needs this written explicitly.
	if err := os.WriteFile(filepath.Join(outer, "memory.max"), []byte("268435456"), 0o644); err != nil {
		cgrouptest.SkipOrFailRealCgroup(t, "cannot set outer memory.max: %v", err)
	}

	binary := filepath.Join(t.TempDir(), "aira")
	// aira confine -- wraps this build, per this plan's own Global
	// Constraints ("every go build/go test ... MUST be prefixed with aira
	// confine --"). It nests under the confinement the outer `aira confine
	// -- go test ...` invocation (Step 2/4/5's Run: lines) already applies
	// to this whole test binary -- a normal, supported nested scope, not a
	// circular dependency: this is the machine's already-installed `aira`
	// on PATH wrapping a build of a FRESH `aira` binary into a temp dir.
	build := exec.Command("aira", "confine", "--", "go", "build", "-o", binary, "aira/cmd/aira")
	if buildOutput, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build aira binary: %v\n%s", err, buildOutput)
	}

	t.Setenv("XDG_STATE_HOME", filepath.Join(t.TempDir(), "state"))
	t.Setenv("XDG_RUNTIME_DIR", shortE2ERuntimeDir(t))
	paths, err := daemon.PathsFromEnv()
	if err != nil {
		t.Fatal(err)
	}
	server := daemon.NewServer(paths)
	server.SetWorkerAdmitHeadroomForTest(0)
	ready := make(chan struct{}, 1)
	server.Ready = ready
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- server.Serve(ctx) }()
	t.Cleanup(func() { cancel(); <-done })
	<-ready

	aitestDir, err := filepath.Abs("aitest")
	if err != nil {
		t.Fatal(err)
	}
	pythonDir, err := pylib.ExtractAitest()
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("XDG_DATA_HOME", t.TempDir())

	outerFile, err := os.Open(outer)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = outerFile.Close() })
	return realDaemonAndCgroupTestHarness{
		pytest: pytest, binary: binary, outerFile: outerFile, aitestDir: aitestDir, pythonDir: pythonDir,
	}
}

func TestRealPytestAitestEndToEndRealDaemonAndCgroupPassFailOnly(t *testing.T) {
	// The default tier previously had no real daemon/cgroup coverage because
	// the existing full-fixture test below includes test_oom.py. AIRA-35's
	// host-dependent OOM convergence is orthogonal to bootstrap, admission,
	// delegation, and worker placement, so exclude only that hazardous file.
	harness := newRealDaemonAndCgroupTestHarness(t)

	runCtx, cancelRun := context.WithTimeout(context.Background(), time.Minute)
	defer cancelRun()
	command := exec.CommandContext(runCtx, harness.pytest, "-q", "--aitest-workers=2", "test_pass.py", "test_fail.py")
	command.Dir = filepath.Join(harness.aitestDir, "testdata")
	command.SysProcAttr = &syscall.SysProcAttr{UseCgroupFD: true, CgroupFD: int(harness.outerFile.Fd())}
	command.Env = append(os.Environ(),
		"PYTHONPATH="+filepath.Dir(harness.aitestDir),
		"PYTHONDONTWRITEBYTECODE=1",
		"AIRA_AITEST_LIB="+harness.pythonDir,
		"AIRA_AITEST_BOOTSTRAP_CMD="+harness.binary,
		"AIRA_AITEST_WORKER_ADMIT_CMD="+harness.binary,
		"AIRA_AITEST_ESTIMATED_BYTES="+strconv.Itoa(32<<20),
		"AIRA_REAL_CGROUP=1",
	)
	output, err := command.CombinedOutput()
	text := string(output)
	if !strings.Contains(text, "test_pass.py::test_one passed") {
		t.Fatalf("pytest output missing expected pass line: %v\n%s", err, text)
	}
	if !strings.Contains(text, "test_fail.py::test_deliberate_failure failed") {
		t.Fatalf("pytest output missing expected fail line: %v\n%s", err, text)
	}
}

func TestRealPytestAitestEndToEndRealDaemonAndCgroup(t *testing.T) {
	// Opt-in only, NOT part of the default `go test ./...` path: verified
	// live on this exact host that the worker's deliberate over-allocation
	// can get caught in the kernel's mem_cgroup_handle_over_high throttle
	// for MINUTES before memory.max's hard kill ever fires (the current
	// 80%/100% memory.high/memory.max split, internal/daemon/worker_admit.go,
	// leaves a wide reclaim-throttle window under this kernel/host's
	// characteristics) -- and a process stuck in that kernel path can enter
	// an UNKILLABLE D-state that SIGKILL cannot wake, a real hazard on a
	// machine other sessions share. The containment invariant itself is
	// NOT broken -- dmesg confirms the correct scope's oom.group eventually
	// fires -- only its convergence speed on this host is impractical for
	// an unattended test run. Tracked as AIRA-35 for a real fix (retune
	// the memory.high/memory.max split, or add an escalation path) rather
	// than silently worked around here. Run explicitly with
	// AIRA_AITEST_SLOW_E2E=1 once that's resolved, or to manually
	// re-verify this exact finding.
	if os.Getenv("AIRA_AITEST_SLOW_E2E") != "1" {
		t.Skip("slow/host-dependent OOM-convergence e2e (AIRA-35) -- set AIRA_AITEST_SLOW_E2E=1 to run explicitly")
	}
	harness := newRealDaemonAndCgroupTestHarness(t)

	// Hard bound: this must NEVER be able to hang the test run indefinitely,
	// regardless of how slowly the OOM-convergence path completes -- see
	// this function's own opt-in-gate comment above for why that can be
	// slow on this host. 4 minutes is generous relative to the pass/fail
	// parts (well under a second, per repeated real runs) while still
	// bounding the worst case observed (multi-minute throttle convergence)
	// to a finite, diagnosable failure instead of an unbounded hang.
	runCtx, cancelRun := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancelRun()
	command := exec.CommandContext(runCtx, harness.pytest, "-q", "--aitest-workers=2")
	command.Dir = filepath.Join(harness.aitestDir, "testdata")
	// Places the pytest subprocess (the supervisor) directly into outer's
	// cgroup AT process creation -- the same clone3(CLONE_INTO_CGROUP)
	// mechanism confine_linux.go already uses for a top-level confine
	// launch (UseCgroupFD/CgroupFD), reused here instead of the full
	// aira-confine argv/handshake machinery, since this test starts one
	// level in: "an outer scope already exists and is about to run its
	// supervisor", not "aira confine itself parses argv and creates it".
	command.SysProcAttr = &syscall.SysProcAttr{UseCgroupFD: true, CgroupFD: int(harness.outerFile.Fd())}
	command.Env = append(os.Environ(),
		"PYTHONPATH="+filepath.Dir(harness.aitestDir),
		"PYTHONDONTWRITEBYTECODE=1",
		"AIRA_AITEST_LIB="+harness.pythonDir,
		"AIRA_AITEST_BOOTSTRAP_CMD="+harness.binary,
		"AIRA_AITEST_WORKER_ADMIT_CMD="+harness.binary,
		"AIRA_AITEST_ESTIMATED_BYTES="+strconv.Itoa(32<<20),
		"AIRA_REAL_CGROUP=1",
	)
	output, err := command.CombinedOutput()
	text := string(output)

	if !strings.Contains(text, "test_pass.py::test_one passed") {
		t.Fatalf("pytest output missing expected pass line: %v\n%s", err, text)
	}
	if !strings.Contains(text, "test_fail.py::test_deliberate_failure failed") {
		t.Fatalf("pytest output missing expected fail line: %v\n%s", err, text)
	}
	// The OOM test exceeds its 32 MiB worker cap deterministically on both
	// the original attempt and the one requeue-once retry (Task 15) --
	// design spec 3.4/4: never passed, never failed, always unevaluated.
	if !strings.Contains(text, "test_oom.py::test_deliberate_oom unevaluated") {
		t.Fatalf("pytest output missing expected OOM-contained unevaluated line: %v\n%s", err, text)
	}
}
