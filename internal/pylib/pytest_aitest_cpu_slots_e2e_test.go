//go:build linux

package pylib_test

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"aira/internal/daemon"
	"aira/internal/runner"
	"aira/internal/testdeadline"
)

// cpuSlotsBarrierTests is the number of fixture tests that must run AT ONCE for
// the suite to pass. With the daemon pinned to 2 cores and R=3 recorded, the CPU
// ceiling is 6 slots: the outer `aira confine` job holds 1, leaving 5 for
// workers. The default R=2 (ceiling 4) leaves at most 3-4, and the pre-AIRA-283
// `auto` (os.cpu_count(), pinned to 1 by the fixture conftest) runs exactly 1.
const cpuSlotsBarrierTests = 5

// writeCPUSlotsFixture writes a pytest suite of cpuSlotsBarrierTests tests that
// each wait at a file barrier until ALL of them are running concurrently, plus a
// guarded conftest that registers aitest exactly as the skill documents and pins
// os.cpu_count() to 1.
func writeCPUSlotsFixture(t *testing.T) (string, string) {
	t.Helper()
	dir := t.TempDir()
	barrier := filepath.Join(t.TempDir(), "barrier")
	if err := os.Mkdir(barrier, 0o755); err != nil {
		t.Fatal(err)
	}
	conftest := `import importlib, os, sys

# Pinned so the pre-AIRA-283 meaning of --aitest-workers=auto (os.cpu_count())
# would run exactly ONE worker: this suite can then only pass if auto is uncapped.
os.cpu_count = lambda: 1

_lib = os.environ.get("AIRA_AITEST_LIB")
if _lib:
    sys.path.insert(0, _lib)
    importlib.import_module("aitest")
    pytest_plugins = ("aitest",)
`
	if err := os.WriteFile(filepath.Join(dir, "conftest.py"), []byte(conftest), 0o644); err != nil {
		t.Fatal(err)
	}
	var tests strings.Builder
	tests.WriteString("import os, time\n\nBARRIER = " + strconv.Quote(barrier) + "\nWANT = " + strconv.Itoa(cpuSlotsBarrierTests) + "\n\n")
	tests.WriteString(`def _meet(name):
    open(os.path.join(BARRIER, name), "w").close()
    deadline = time.monotonic() + 30
    while time.monotonic() < deadline:
        if len(os.listdir(BARRIER)) >= WANT:
            return
        time.sleep(0.05)
    raise AssertionError("only %d of %d tests ever ran at once" % (len(os.listdir(BARRIER)), WANT))

`)
	for i := 0; i < cpuSlotsBarrierTests; i++ {
		tests.WriteString("def test_concurrent_" + strconv.Itoa(i) + "():\n    _meet(\"" + strconv.Itoa(i) + "\")\n\n")
	}
	if err := os.WriteFile(filepath.Join(dir, "test_concurrent.py"), []byte(tests.String()), 0o644); err != nil {
		t.Fatal(err)
	}
	return dir, barrier
}

// verifies: AIRA-283 §5 wiring — install flag -> record -> Serve -> ceiling ->
// a pool run with `auto` grows past cpu_count, through real binaries and a real
// daemon, with no stubbed probe. `aira install --ci=shim --stage=build
// --cpu-slots-per-core=3` writes the record; the daemon is started EXACTLY as
// spawnShimDaemon starts it (AIRA_DAEMON_CONFINE_MODE=ci-shim, so E1's own
// unconditional record read is what supplies R), pinned to 2 cores; then
// `aira confine --delegate-ram -- pytest --aitest-workers=auto` must run all five
// barrier tests at once. Hardcoding the ceiling at 2 x NumCPU, reading R only
// inside resolveDaemonConfineMode, or capping auto at os.cpu_count() each leave
// fewer than five running, and the barrier fails.
func TestRealPytestAitestAutoGrowsToTheInstalledCPUSlotsCeiling(t *testing.T) {
	pytest := requireRealPytest(t)

	// A FRESH binary of this tree: the confine client, the worker-admit relay and
	// the embedded aitest it extracts must all be the code under test. Built under
	// the machine's own `aira confine` where one is installed (the harness
	// convention), else directly -- this test process is the confined job then.
	binary := filepath.Join(t.TempDir(), "aira")
	build := exec.Command("go", "build", "-o", binary, "aira/cmd/aira")
	if _, lookErr := exec.LookPath("aira"); lookErr == nil {
		build = exec.Command("aira", "confine", "--", "go", "build", "-o", binary, "aira/cmd/aira")
	}
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build aira binary: %v\n%s", err, output)
	}

	home := t.TempDir()
	t.Setenv("XDG_STATE_HOME", filepath.Join(home, "state"))
	t.Setenv("XDG_RUNTIME_DIR", shortE2ERuntimeDir(t))
	t.Setenv("XDG_DATA_HOME", filepath.Join(home, "data"))
	// Everything this process spawns from here on sees the isolated state and
	// runtime dirs; the confine client resolves its mode from the record under
	// XDG_STATE_HOME. HOME is deliberately left alone: a ci-shim build stage
	// writes nothing under it, and the real pytest lives in HOME's user site.
	t.Setenv(runner.InstallModeFileEnv, "")

	install := exec.Command(binary, "install", "--ci=shim", "--stage=build", "--memory-max=64G", "--cpu-slots-per-core=3")
	installOutput, err := install.CombinedOutput()
	if err != nil {
		t.Fatalf("aira install --ci=shim --stage=build --cpu-slots-per-core=3: %v\n%s", err, installOutput)
	}
	if !strings.Contains(string(installOutput), "cpu slots per core: 3") {
		t.Fatalf("install did not report the recorded ratio:\n%s", installOutput)
	}

	paths, err := daemon.PathsFromEnv()
	if err != nil {
		t.Fatal(err)
	}
	// The same overrides spawnShimDaemon hands a real shim daemon (E1).
	t.Setenv("AIRA_DAEMON_CONFINE_MODE", runner.ConfineModeShim)
	t.Setenv("AIRA_DAEMON_SHIM_BUDGET_BYTES", strconv.FormatInt(64<<30, 10))
	t.Setenv("AIRA_DAEMON_SHIM_BUDGET_SOURCE", runner.ShimBudgetSourceDeclared)
	server := daemon.NewServer(paths)
	server.SetCPUFrameForTest(nil, func() int { return 2 })
	ready := make(chan struct{}, 1)
	server.Ready = ready
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- server.Serve(ctx) }()
	t.Cleanup(func() { cancel(); <-done })
	select {
	case <-ready:
	case err := <-done:
		t.Fatalf("daemon exited before ready: %v", err)
	case <-testdeadline.After(10 * time.Second):
		t.Fatal("daemon did not become ready")
	}
	if live := daemon.Status(paths).Lock.CPUSlotsPerCore; live != 3 {
		t.Fatalf("the daemon adopted cpu_slots_per_core=%d from the installed record, want 3", live)
	}

	fixture, _ := writeCPUSlotsFixture(t)
	runCtx, cancelRun := context.WithTimeout(context.Background(), testdeadline.Wait(2*time.Minute))
	defer cancelRun()
	command := exec.CommandContext(runCtx, binary, "confine", "--delegate-ram", "--",
		pytest, "-q", "-p", "no:cacheprovider", "--aitest-workers=auto", "test_concurrent.py")
	command.Dir = fixture
	command.WaitDelay = 15 * time.Second
	command.Env = append(environWithoutAiraRealCgroup(), "PYTHONDONTWRITEBYTECODE=1")
	output, err := command.CombinedOutput()
	text := string(output)
	if err != nil {
		t.Fatalf("auto pool did not run %d tests at once under R=3 x 2 cores: %v\n%s", cpuSlotsBarrierTests, err, text)
	}
	for i := 0; i < cpuSlotsBarrierTests; i++ {
		if !strings.Contains(text, "test_concurrent.py::test_concurrent_"+strconv.Itoa(i)+" passed") {
			t.Fatalf("missing pass line for test_concurrent_%d:\n%s", i, text)
		}
	}
	if !strings.Contains(text, "containment=advisory(ci-shim") {
		t.Fatalf("the run did not go through the ci-shim confine path:\n%s", text)
	}
	if strings.Contains(text, "falling back to") {
		t.Fatalf("the pool fell back to unconfined workers instead of using the daemon's ledgers:\n%s", text)
	}
}
