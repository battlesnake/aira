//go:build linux

package pylib

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const activatingConftest = `
try:
    import importlib
    import os
    import sys

    if aira_py_lib := os.environ.get("AIRA_PY_LIB"):
        sys.path.insert(0, aira_py_lib)
        importlib.import_module("aira_xdist_governor")
        pytest_plugins = ("aira_xdist_governor",)
except Exception:
    pass
`

func TestRealPytestEmbeddedPackageImports(t *testing.T) {
	pytest := requireRealPytest(t)
	project, pythonDir := realPytestProject(t, "")
	writeTestFile(t, project, "test_import.py", `
import sys
def test_plugin_was_registered():
    assert "aira_xdist_governor" in sys.modules
`)
	result := runPytest(t, pytest, project, pythonDir, nil)
	if result.err != nil {
		t.Fatalf("pytest import failed: %v\n%s", result.err, result.output)
	}
}

func TestRealPytestAcquireWaitExcludedFromPhaseDurations(t *testing.T) {
	pytest := requireRealPytest(t)
	project, pythonDir := realPytestProject(t, `
import json
import os
_aira_durations = []
def pytest_runtest_logreport(report):
    _aira_durations.append([report.when, report.duration])
    if report.when == "teardown":
        with open(os.environ["AIRA_TEST_DURATIONS"], "w", encoding="utf-8") as destination:
            json.dump(_aira_durations, destination)
`)
	durations := filepath.Join(project, "durations.json")
	helper := writeGovernorHelper(t, project, `
import sys, time
time.sleep(0.8)
print("active", flush=True)
for _ in sys.stdin:
    print("continue", flush=True)
`)
	writeTestFile(t, project, "test_duration.py", "def test_fast():\n    assert True\n")
	started := time.Now()
	result := runPytest(t, pytest, project, pythonDir, map[string]string{
		"AIRA_GOVERNOR_CMD":   helper,
		"AIRA_TEST_DURATIONS": durations,
	})
	if result.err != nil {
		t.Fatalf("pytest duration run failed: %v\n%s", result.err, result.output)
	}
	if elapsed := time.Since(started); elapsed < 700*time.Millisecond {
		t.Fatalf("fake governor did not create a meaningful pre-phase park: %s", elapsed)
	}
	var reports [][]any
	data, err := os.ReadFile(durations)
	if err != nil || json.Unmarshal(data, &reports) != nil {
		t.Fatalf("durations=%q err=%v", data, err)
	}
	var phaseTotal float64
	for _, report := range reports {
		phaseTotal += report[1].(float64)
	}
	if phaseTotal >= 0.4 {
		t.Fatalf("reported pytest phases included governor acquire park: total=%fs reports=%v", phaseTotal, reports)
	}
}

func TestRealPytestGCCollectsOnceOnlyWhenSleeping(t *testing.T) {
	pytest := requireRealPytest(t)
	for _, test := range []struct {
		name, helper, wantGC string
	}{
		{"immediate grant", `print("granted reserve=" + sys.argv[sys.argv.index("--bytes") + 1] + " basis=pinned:client", flush=True)`, ""},
		{"waiting reservation", `
deadline = time.monotonic() + 2
while not os.path.exists(os.environ["AIRA_TEST_GC_COUNT"]) and time.monotonic() < deadline:
    time.sleep(0.01)
print("granted reserve=" + sys.argv[sys.argv.index("--bytes") + 1] + " basis=pinned:client", flush=True)
sys.stdin.buffer.read()`, "1"},
	} {
		t.Run(test.name, func(t *testing.T) {
			project, pythonDir := realPytestProject(t, `
import os
import aira_xdist_governor as governor
class ProbeGC:
    @staticmethod
    def collect():
        path = os.environ["AIRA_TEST_GC_COUNT"]
        count = int(open(path).read()) if os.path.exists(path) else 0
        open(path, "w").write(str(count + 1))
governor.gc = ProbeGC
if os.environ.get("AIRA_TEST_GRANT_READY"):
    governor._grant_ready = lambda _: True
`)
			countPath := filepath.Join(project, "gc-count")
			helper := writeReserveHelper(t, project, "import os, sys, time\n"+test.helper)
			writeTestFile(t, project, "test_gc.py", "def test_runs(): pass")
			result := runPytest(t, pytest, project, pythonDir, map[string]string{
				"AIRA_TEST_MEM_GOVERNOR": "1", "AIRA_TEST_MEM_DEFAULT": "8M",
				"AIRA_CONFINE_RESERVE_CMD": helper, "AIRA_TEST_GC_COUNT": countPath,
				"AIRA_TEST_AFTER_TEST_GC_INTERVAL": "0",
				"AIRA_TEST_GRANT_READY":            map[bool]string{true: "1", false: ""}[test.name == "immediate grant"],
			})
			if result.err != nil {
				t.Fatalf("pytest GC case failed: %v\n%s", result.err, result.output)
			}
			data, err := os.ReadFile(countPath)
			if test.wantGC == "" && errors.Is(err, os.ErrNotExist) {
				return
			}
			if err != nil || string(data) != test.wantGC {
				t.Fatalf("gc count=%q want=%q err=%v", data, test.wantGC, err)
			}
		})
	}
}

func TestRealPytestPeriodicAfterTestGC(t *testing.T) {
	pytest := requireRealPytest(t)
	count := func(t *testing.T, path string) string {
		t.Helper()
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		return string(data)
	}
	probe := `
import os, time
import aira_xdist_governor as governor
class ProbeGC:
    @staticmethod
    def collect():
        path = os.environ["AIRA_TEST_GC_COUNT"]
        value = int(open(path).read()) if os.path.exists(path) else 0
        open(path, "w").write(str(value + 1))
governor.gc = ProbeGC
governor._last_after_test_gc = time.monotonic() - 60
`
	t.Run("two fast tests collect once", func(t *testing.T) {
		project, pythonDir := realPytestProject(t, probe)
		countPath := filepath.Join(project, "gc-count")
		writeTestFile(t, project, "test_cadence.py", "def test_first(): pass\ndef test_second(): pass")
		result := runPytest(t, pytest, project, pythonDir, map[string]string{"AIRA_TEST_AFTER_TEST_GC_INTERVAL": "60", "AIRA_TEST_GC_COUNT": countPath})
		if result.err != nil {
			t.Fatalf("pytest cadence failed: %v\n%s", result.err, result.output)
		}
		if got := count(t, countPath); got != "1" {
			t.Fatalf("after-test gc count=%q, want 1", got)
		}
	})
	t.Run("RAM wait collect does not suppress cadence", func(t *testing.T) {
		project, pythonDir := realPytestProject(t, probe)
		countPath := filepath.Join(project, "gc-count")
		helper := writeReserveHelper(t, project, `
import os, sys, time
while not os.path.exists(os.environ["AIRA_TEST_GC_COUNT"]): time.sleep(0.01)
print("granted reserve=" + sys.argv[sys.argv.index("--bytes") + 1] + " basis=pinned:client", flush=True)
sys.stdin.buffer.read()`)
		writeTestFile(t, project, "test_independent.py", "def test_runs(): pass")
		result := runPytest(t, pytest, project, pythonDir, map[string]string{
			"AIRA_TEST_MEM_GOVERNOR": "1", "AIRA_TEST_MEM_DEFAULT": "8M", "AIRA_CONFINE_RESERVE_CMD": helper,
			"AIRA_TEST_AFTER_TEST_GC_INTERVAL": "60", "AIRA_TEST_GC_COUNT": countPath,
		})
		if result.err != nil {
			t.Fatalf("pytest independent timer failed: %v\n%s", result.err, result.output)
		}
		if got := count(t, countPath); got != "2" {
			t.Fatalf("gc count=%q, want one before RAM block plus one after test", got)
		}
	})
	t.Run("zero disables cadence", func(t *testing.T) {
		project, pythonDir := realPytestProject(t, probe)
		countPath := filepath.Join(project, "gc-count")
		writeTestFile(t, project, "test_disabled.py", "def test_runs(): pass")
		result := runPytest(t, pytest, project, pythonDir, map[string]string{"AIRA_TEST_AFTER_TEST_GC_INTERVAL": "0", "AIRA_TEST_GC_COUNT": countPath})
		if result.err != nil {
			t.Fatalf("pytest disabled cadence failed: %v\n%s", result.err, result.output)
		}
		if _, err := os.Stat(countPath); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("disabled cadence collected: %v", err)
		}
	})
	for _, value := range []string{"-1", "garbage"} {
		t.Run("invalid interval "+value+" is fail-open", func(t *testing.T) {
			assertRealPytestItemRuns(t, pytest, map[string]string{"AIRA_TEST_AFTER_TEST_GC_INTERVAL": value}, nil)
		})
	}
}

func TestRealPytestRAMWaitCollectsBeforeBlocking(t *testing.T) {
	pytest := requireRealPytest(t)
	project, pythonDir := realPytestProject(t, `
import os
import aira_xdist_governor as governor
class ProbeGC:
    @staticmethod
    def collect():
        path = os.environ["AIRA_TEST_GC_COUNT"]
        value = int(open(path).read()) if os.path.exists(path) else 0
        open(path, "w").write(str(value + 1))
governor.gc = ProbeGC
`)
	countPath := filepath.Join(project, "gc-count")
	helper := writeReserveHelper(t, project, `
import os, sys, time
deadline = time.monotonic() + 2
while not os.path.exists(os.environ["AIRA_TEST_GC_COUNT"]) and time.monotonic() < deadline: time.sleep(0.01)
estimate = sys.argv[sys.argv.index("--bytes") + 1]
print("granted reserve=%s basis=pinned:client" % estimate, flush=True)
sys.stdin.buffer.read()`)
	writeTestFile(t, project, "test_ram_wait.py", "def test_runs(): pass")
	result := runPytest(t, pytest, project, pythonDir, map[string]string{
		"AIRA_TEST_MEM_GOVERNOR": "1", "AIRA_TEST_MEM_DEFAULT": "8M", "AIRA_CONFINE_RESERVE_CMD": helper,
		"AIRA_TEST_GC_COUNT": countPath, "AIRA_TEST_AFTER_TEST_GC_INTERVAL": "0",
	})
	if result.err != nil {
		t.Fatalf("pytest RAM wait failed: %v\n%s", result.err, result.output)
	}
	if data, err := os.ReadFile(countPath); err != nil || string(data) != "1" {
		t.Fatalf("RAM wait gc count=%q err=%v, want 1", data, err)
	}
}

func TestRealPytestRAMImmediateGrantSkipsCollect(t *testing.T) {
	pytest := requireRealPytest(t)
	project, pythonDir := realPytestProject(t, `
import os
import aira_xdist_governor as governor
class ProbeGC:
    @staticmethod
    def collect(): open(os.environ["AIRA_TEST_GC_COUNT"], "w").write("collected")
class Stdin:
    def close(self): pass
class ImmediateGrantProcess:
    def __init__(self, descriptor): self.stdin = Stdin(); self.stdout = os.fdopen(descriptor, "rb", buffering=0)
    def poll(self): return None
    def wait(self, timeout=None): return 0
    def terminate(self): pass
    def kill(self): pass
def immediate_grant_popen(command, **kwargs):
    descriptor, writer = os.pipe()
    estimate = command[command.index("--bytes") + 1]
    os.write(writer, ("granted reserve=%s basis=pinned:client\n" % estimate).encode("ascii")); os.close(writer)
    return ImmediateGrantProcess(descriptor)
governor.gc = ProbeGC
governor.subprocess.Popen = immediate_grant_popen
`)
	countPath := filepath.Join(project, "gc-count")
	writeTestFile(t, project, "test_ram_immediate.py", "def test_runs(): pass")
	result := runPytest(t, pytest, project, pythonDir, map[string]string{
		"AIRA_TEST_MEM_GOVERNOR": "1", "AIRA_TEST_MEM_DEFAULT": "8M", "AIRA_CONFINE_RESERVE_CMD": "fake-aira",
		"AIRA_TEST_GC_COUNT": countPath, "AIRA_TEST_AFTER_TEST_GC_INTERVAL": "0",
	})
	if result.err != nil {
		t.Fatalf("pytest immediate RAM grant failed: %v\n%s", result.err, result.output)
	}
	if _, err := os.Stat(countPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("immediate RAM grant collected: %v", err)
	}
}

func TestRealPytestRAMReservationPrimitives(t *testing.T) {
	pytest := requireRealPytest(t)
	project, pythonDir := realPytestProject(t, "")
	writeTestFile(t, project, "test_ram_reservation_primitives.py", `
import builtins
from unittest.mock import mock_open, patch
import aira_xdist_governor as governor
def test_statm_rss_uses_resident_pages():
    with patch.object(builtins, "open", mock_open(read_data="101 7 0 0 0 0 0\n")):
        with patch.object(governor.os, "sysconf", return_value=4096): assert governor._read_rss_bytes() == 7 * 4096
def test_malloc_trim_missing_symbol_is_best_effort(monkeypatch):
    class NoTrim: pass
    monkeypatch.setattr(governor.ctypes, "CDLL", lambda _: NoTrim())
    governor._collect_and_trim()`)
	if result := runPytest(t, pytest, project, pythonDir, map[string]string{"AIRA_TEST_MEM_DEFAULT": "10"}); result.err != nil {
		t.Fatalf("RAM reservation primitive pytest failed: %v\n%s", result.err, result.output)
	}
}

func TestRealPytestRAMReservationUsesMeasuredRSS(t *testing.T) {
	pytest := requireRealPytest(t)
	project, pythonDir := realPytestProject(t, "import aira_xdist_governor as governor\ngovernor._read_rss_bytes = lambda: 40")
	request := filepath.Join(project, "reservation-bytes")
	helper := writeReserveHelper(t, project, `
import os, sys
bytes_to_hold = sys.argv[sys.argv.index("--bytes") + 1]
open(os.environ["AIRA_TEST_RESERVATION_BYTES"], "w").write(bytes_to_hold)
print("granted reserve=%s basis=pinned:client" % bytes_to_hold, flush=True)
sys.stdin.buffer.read()`)
	writeTestFile(t, project, "test_rss_reservation.py", "import pytest\n@pytest.mark.aira_mem('10')\ndef test_reservation_uses_accumulated_rss(): pass")
	result := runPytest(t, pytest, project, pythonDir, map[string]string{
		"AIRA_TEST_MEM_GOVERNOR": "1", "AIRA_TEST_MEM_DEFAULT": "10", "AIRA_TEST_MEM_GROWTH_HEADROOM": "10",
		"AIRA_CONFINE_RESERVE_CMD": helper, "AIRA_TEST_RESERVATION_BYTES": request,
	})
	if result.err != nil {
		t.Fatalf("RSS-sized RAM reservation pytest failed: %v\n%s", result.err, result.output)
	}
	data, err := os.ReadFile(request)
	if err != nil {
		t.Fatal(err)
	}
	// The marker is 10, but the measured RSS + headroom is 40 + 10. Requesting
	// the raw marker instead regresses #69 and fails this discriminating check.
	if got, want := string(data), "50"; got != want {
		t.Fatalf("reservation bytes=%q want=%q", got, want)
	}
}

func TestRealPytestTotalFailOpen(t *testing.T) {
	pytest := requireRealPytest(t)
	t.Run("RAM governor disabled", func(t *testing.T) { assertRealPytestItemRuns(t, pytest, nil, nil) })
	t.Run("reserve command unset", func(t *testing.T) {
		assertRealPytestItemRuns(t, pytest, map[string]string{"AIRA_TEST_MEM_GOVERNOR": "1", "AIRA_TEST_MEM_DEFAULT": "8M"}, nil)
	})
	t.Run("invalid RAM default", func(t *testing.T) {
		assertRealPytestItemRuns(t, pytest, map[string]string{"AIRA_TEST_MEM_GOVERNOR": "1", "AIRA_TEST_MEM_DEFAULT": "nonsense"}, nil)
	})
	t.Run("invalid RAM headroom", func(t *testing.T) {
		assertRealPytestItemRuns(t, pytest, map[string]string{"AIRA_TEST_MEM_GOVERNOR": "1", "AIRA_TEST_MEM_DEFAULT": "8M", "AIRA_TEST_MEM_GROWTH_HEADROOM": "nonsense", "AIRA_CONFINE_RESERVE_CMD": "missing"}, nil)
	})
	t.Run("missing reserve command", func(t *testing.T) {
		project := t.TempDir()
		assertRealPytestItemRuns(t, pytest, map[string]string{"AIRA_TEST_MEM_GOVERNOR": "1", "AIRA_TEST_MEM_DEFAULT": "8M", "AIRA_CONFINE_RESERVE_CMD": filepath.Join(project, "missing")}, nil)
	})
	t.Run("invalid reserve grant", func(t *testing.T) {
		project := t.TempDir()
		helper := writeReserveHelper(t, project, "print('not a grant', flush=True)")
		assertRealPytestItemRuns(t, pytest, map[string]string{"AIRA_TEST_MEM_GOVERNOR": "1", "AIRA_TEST_MEM_DEFAULT": "8M", "AIRA_CONFINE_RESERVE_CMD": helper}, nil)
	})
}

func TestRealPytestRAMMarkerPrecedencePinnedAndRegistered(t *testing.T) {
	pytest := requireRealPytest(t)
	project, pythonDir := realPytestProject(t, "import aira_xdist_governor as governor\ngovernor._read_rss_bytes = lambda: 0")
	requests := filepath.Join(project, "requests.jsonl")
	helper := writeReserveHelper(t, project, `
import json, os, sys
with open(os.environ["AIRA_TEST_REQUESTS"], "a") as target: target.write(json.dumps(sys.argv[1:]) + "\n")
estimate = sys.argv[sys.argv.index("--bytes") + 1]
print("granted reserve=%s basis=pinned:client" % estimate, flush=True)
sys.stdin.buffer.read()`)
	writeTestFile(t, project, "test_ram_marks.py", `
import pytest
pytestmark = pytest.mark.aira_mem("64M")
def test_module_scope(): pass
@pytest.mark.aira_mem("32M")
class TestClassScope:
    def test_class_scope(self): pass
    @pytest.mark.aira_mem("16M")
    def test_test_scope(self): pass
@pytest.mark.aira_mem("4GB")
def test_full_unit_spelling(): pass
@pytest.mark.aira_mem("1.5G")
def test_decimal_marker(): pass`)
	writeTestFile(t, project, "test_memory_size.py", `
import pytest
import aira_xdist_governor as governor
@pytest.mark.parametrize(("raw", "expected"), [("1", 1), ("4GiB", 4294967296), ("1.5GB", 1610612736), ("0.5G", 536870912), ("1.05G", 1127428915), ("1.3K", 1331)])
def test_parse_memory_size(raw, expected): assert governor._parse_memory_size(raw) == expected
@pytest.mark.parametrize("raw", ["", "0", "1.", ".5G", "1.2.3", "1,5", "-1", "nonnumeric"])
def test_parse_memory_size_rejects_invalid_input(raw):
    with pytest.raises(ValueError): governor._parse_memory_size(raw)`)
	result := runPytest(t, pytest, project, pythonDir, map[string]string{
		"AIRA_TEST_MEM_GOVERNOR": "1", "AIRA_TEST_MEM_DEFAULT": "8M", "AIRA_TEST_MEM_GROWTH_HEADROOM": "1",
		"AIRA_CONFINE_RESERVE_CMD": helper, "AIRA_TEST_REQUESTS": requests, "PYTEST_ADDOPTS": "--strict-markers",
	})
	if result.err != nil {
		t.Fatalf("RAM marker pytest failed: %v\n%s", result.err, result.output)
	}
	data, err := os.ReadFile(requests)
	if err != nil {
		t.Fatal(err)
	}
	wants := map[string]string{"test_module_scope": "67108864", "test_class_scope": "33554432", "test_test_scope": "16777216", "test_full_unit_spelling": "4294967296", "test_decimal_marker": "1610612736"}
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		var argv []string
		if err := json.Unmarshal([]byte(line), &argv); err != nil {
			t.Fatal(err)
		}
		joined := strings.Join(argv, " ")
		if !strings.Contains(joined, "--pinned") {
			t.Fatalf("unpinned helper argv=%v", argv)
		}
		for node, estimate := range wants {
			if strings.Contains(joined, node) {
				if got := argv[indexOf(t, argv, "--bytes")+1]; got != estimate {
					t.Fatalf("%s estimate=%s want=%s argv=%v", node, got, estimate, argv)
				}
				delete(wants, node)
			}
		}
	}
	if len(wants) != 0 || strings.Contains(result.output, "invalid aira_mem marker") || strings.Contains(result.output, "PytestUnknownMarkWarning") {
		t.Fatalf("missing=%v output=%s requests=%s", wants, result.output, data)
	}
}

func TestRealPytestRAMWeightedConcurrencySharesBudgetAcrossSuites(t *testing.T) {
	pytest := requireRealPytest(t)
	project, pythonDir := realPytestProject(t, "import aira_xdist_governor as governor\ngovernor._read_rss_bytes = lambda: 0")
	state := filepath.Join(project, "ram-state.json")
	writeTestFile(t, project, "ram-state.json", `{"current":0,"maximum":0,"count":0,"max_count":0,"heavy":0,"heavy_max":0}`)
	helper := writeReserveHelper(t, project, `
import fcntl, json, os, sys, time
state_path = os.environ["AIRA_RAM_STATE"]; estimate = int(sys.argv[sys.argv.index("--bytes") + 1]); budget = int(os.environ["AIRA_RAM_BUDGET"])
while True:
    with open(state_path + ".lock", "a+") as lock:
        fcntl.flock(lock.fileno(), fcntl.LOCK_EX)
        with open(state_path) as source: state = json.load(source)
        if state["current"] + estimate <= budget:
            state["current"] += estimate; state["maximum"] = max(state["maximum"], state["current"]); state["count"] += 1; state["max_count"] = max(state["max_count"], state["count"])
            if estimate > budget // 2: state["heavy"] += 1; state["heavy_max"] = max(state["heavy_max"], state["heavy"])
            with open(state_path, "w") as target: json.dump(state, target)
            break
    time.sleep(0.01)
print("granted reserve=%d basis=pinned:client" % estimate, flush=True)
sys.stdin.buffer.read()
with open(state_path + ".lock", "a+") as lock:
    fcntl.flock(lock.fileno(), fcntl.LOCK_EX)
    with open(state_path) as source: state = json.load(source)
    state["current"] -= estimate; state["count"] -= 1
    if estimate > budget // 2: state["heavy"] -= 1
    with open(state_path, "w") as target: json.dump(state, target)`)
	writeTestFile(t, project, "test_ram_cap.py", `
import os, pytest, time
@pytest.mark.aira_mem(os.environ["AIRA_CASE_ESTIMATE"])
def test_participating_suite(): time.sleep(0.2)`)
	type invocation struct {
		command *exec.Cmd
		output  bytes.Buffer
	}
	estimates := []string{"70", "70", "20", "20", "20", "20"}
	invocations := make([]*invocation, 0, len(estimates))
	for _, estimate := range estimates {
		call := &invocation{command: exec.Command(pytest, "-q", "test_ram_cap.py")}
		call.command.Dir = project
		call.command.Env = realPytestEnv(pythonDir, map[string]string{
			"AIRA_TEST_MEM_GOVERNOR": "1", "AIRA_TEST_MEM_DEFAULT": "10", "AIRA_TEST_MEM_GROWTH_HEADROOM": "1", "AIRA_CONFINE_RESERVE_CMD": helper,
			"AIRA_RAM_STATE": state, "AIRA_RAM_BUDGET": "100", "AIRA_CASE_ESTIMATE": estimate,
		})
		call.command.Stdout, call.command.Stderr = &call.output, &call.output
		if err := call.command.Start(); err != nil {
			t.Fatal(err)
		}
		invocations = append(invocations, call)
	}
	for _, call := range invocations {
		if err := call.command.Wait(); err != nil {
			t.Fatalf("pytest failed: %v\n%s", err, call.output.String())
		}
	}
	var observed struct {
		Current  int `json:"current"`
		Maximum  int `json:"maximum"`
		Count    int `json:"count"`
		MaxCount int `json:"max_count"`
		Heavy    int `json:"heavy"`
		HeavyMax int `json:"heavy_max"`
	}
	data, err := os.ReadFile(state)
	if err != nil || json.Unmarshal(data, &observed) != nil {
		t.Fatalf("state=%q err=%v", data, err)
	}
	if observed.Current != 0 || observed.Count != 0 || observed.Maximum < 70 || observed.HeavyMax != 1 || observed.MaxCount < 2 {
		t.Fatalf("weighted shared state=%+v", observed)
	}
}

func TestRealPytestRAMHelperFailureIsInstantAndFailOpen(t *testing.T) {
	pytest := requireRealPytest(t)
	started := time.Now()
	project, pythonDir := realPytestProject(t, "")
	marker := filepath.Join(project, "ran")
	writeTestFile(t, project, "test_fail_open_ram.py", fmt.Sprintf("def test_runs(): open(%q, 'w').write('yes')", marker))
	result := runPytest(t, pytest, project, pythonDir, map[string]string{"AIRA_TEST_MEM_GOVERNOR": "1", "AIRA_TEST_MEM_DEFAULT": "8M", "AIRA_CONFINE_RESERVE_CMD": filepath.Join(project, "missing-aira")})
	if result.err != nil || time.Since(started) > 2*time.Second || !strings.Contains(result.output, "aira RAM governor disabled") {
		t.Fatalf("result=%v elapsed=%s output=%s", result.err, time.Since(started), result.output)
	}
	if data, err := os.ReadFile(marker); err != nil || string(data) != "yes" {
		t.Fatalf("test did not run: data=%q err=%v", data, err)
	}
}

func TestRealPytestRAMForkDoesNotPinHelperStdin(t *testing.T) {
	pytest := requireRealPytest(t)
	project, pythonDir := realPytestProject(t, "")
	released, childDone := filepath.Join(project, "released"), filepath.Join(project, "child-done")
	helper := writeReserveHelper(t, project, `
import os, sys, time
estimate = sys.argv[sys.argv.index("--bytes") + 1]
print("granted reserve=%s basis=pinned:client" % estimate, flush=True)
sys.stdin.buffer.read()
open(os.environ["AIRA_RELEASED"], "w").write(str(time.time_ns()))`)
	writeTestFile(t, project, "test_ram_fork.py", `
import os, time
def test_fork_child_does_not_hold_reservation():
    child = os.fork()
    if child == 0:
        os.close(1); os.close(2); time.sleep(1.0)
        open(os.environ["AIRA_CHILD_DONE"], "w").write(str(time.time_ns()))
        os._exit(0)`)
	result := runPytest(t, pytest, project, pythonDir, map[string]string{"AIRA_TEST_MEM_GOVERNOR": "1", "AIRA_TEST_MEM_DEFAULT": "8M", "AIRA_CONFINE_RESERVE_CMD": helper, "AIRA_RELEASED": released, "AIRA_CHILD_DONE": childDone})
	if result.err != nil {
		t.Fatalf("fork pytest failed: %v\n%s", result.err, result.output)
	}
	releaseTime, childTime := waitForRealPytestPath(t, released), waitForRealPytestPath(t, childDone)
	if string(releaseTime) >= string(childTime) {
		t.Fatalf("reservation release=%s child done=%s", releaseTime, childTime)
	}
}

func TestRealPytestAiraMemShimIsInertWithoutPlugin(t *testing.T) {
	pytest := requireRealPytest(t)
	project := t.TempDir()
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	pythonDir, err := ExtractPyLib()
	if err != nil {
		t.Fatal(err)
	}
	writeTestFile(t, project, "test_shim.py", `
import sys
sys.path.insert(0, __import__("os").environ["AIRA_PY_LIB"])
from aira_xdist_governor.shim import aira_mem
@aira_mem("4G")
def test_inert_shim(): pass`)
	result := runPytest(t, pytest, project, pythonDir, map[string]string{"PYTEST_ADDOPTS": "--strict-markers"})
	if result.err != nil || strings.Contains(result.output, "UnknownMark") {
		t.Fatalf("inert shim failed: %v\n%s", result.err, result.output)
	}
}

func TestRealPytestOptOutWithoutConftestIsInactive(t *testing.T) {
	pytest := requireRealPytest(t)
	project := t.TempDir()
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	pythonDir, err := ExtractPyLib()
	if err != nil {
		t.Fatal(err)
	}
	writeTestFile(t, project, "test_optout.py", `
import sys
def test_governor_was_not_imported(): assert "aira_xdist_governor" not in sys.modules`)
	result := runPytest(t, pytest, project, pythonDir, map[string]string{"AIRA_TEST_MEM_GOVERNOR": "1", "AIRA_TEST_MEM_DEFAULT": "8M"})
	if result.err != nil {
		t.Fatalf("opt-out pytest failed: %v\n%s", result.err, result.output)
	}
}

func TestRealPytestGovernorOffDoesNotSpawnRelay(t *testing.T) {
	pytest := requireRealPytest(t)
	project, pythonDir := realPytestProject(t, "")
	started := filepath.Join(project, "relay-started")
	helper := writeGovernorHelper(t, project, `
import os, sys
open(os.environ["AIRA_RELAY_STARTED"], "w").write("started")
print("active", flush=True)
sys.stdin.buffer.read()
`)
	writeTestFile(t, project, "test_off.py", "def test_runs(): assert True")
	result := runPytest(t, pytest, project, pythonDir, map[string]string{
		"AIRA_GOVERNOR": "off", "AIRA_GOVERNOR_CMD": helper,
		"AIRA_RELAY_STARTED": started,
	})
	if result.err != nil {
		t.Fatalf("off suite failed: %v\n%s", result.err, result.output)
	}
	if _, err := os.Stat(started); !os.IsNotExist(err) {
		t.Fatalf("off mode spawned relay: %v", err)
	}
}

func TestRealPytestGovernorRelayCheckpointAndTeardown(t *testing.T) {
	pytest := requireRealPytest(t)
	project, pythonDir := realPytestProject(t, `
import aira_xdist_governor as governor
def pytest_runtest_teardown(item):
    # Make the second protocol call a checkpoint without slowing this test by
    # the production ten-second cadence.
    governor._last_governor_checkpoint = 0
`)
	started := filepath.Join(project, "started")
	checkpoint := filepath.Join(project, "checkpoint")
	released := filepath.Join(project, "released")
	helper := writeGovernorHelper(t, project, `
import os, sys
open(os.environ["AIRA_RELAY_STARTED"], "w").write("started")
print("active", flush=True)
for line in sys.stdin:
    if line.startswith("checkpoint "):
        open(os.environ["AIRA_RELAY_CHECKPOINT"], "w").write(line)
    print("continue", flush=True)
open(os.environ["AIRA_RELAY_RELEASED"], "w").write("released")
`)
	writeTestFile(t, project, "test_relay.py", "def test_one(): pass\ndef test_two(): pass")
	result := runPytest(t, pytest, project, pythonDir, map[string]string{
		"AIRA_GOVERNOR_CMD": helper, "AIRA_RELAY_STARTED": started,
		"AIRA_RELAY_CHECKPOINT": checkpoint, "AIRA_RELAY_RELEASED": released,
	})
	if result.err != nil {
		t.Fatalf("relay suite failed: %v\n%s", result.err, result.output)
	}
	for _, path := range []string{started, checkpoint, released} {
		if data, err := os.ReadFile(path); err != nil || len(data) == 0 {
			t.Fatalf("relay did not write %s: data=%q err=%v", path, data, err)
		}
	}
	if data, _ := os.ReadFile(checkpoint); !strings.HasPrefix(string(data), "checkpoint ") {
		t.Fatalf("checkpoint=%q", data)
	}
}

func TestRealPytestGovernorFailureIsFailOpen(t *testing.T) {
	pytest := requireRealPytest(t)
	project, pythonDir := realPytestProject(t, "")
	marker := filepath.Join(project, "ran")
	writeTestFile(t, project, "test_fail_open.py", fmt.Sprintf("def test_runs():\n    open(%q, 'w').write('ran')\n", marker))
	// This is discriminating: if the protocol lets a relay spawn/read error
	// escape, pytest fails before the item can write its marker.
	result := runPytest(t, pytest, project, pythonDir, map[string]string{
		"AIRA_GOVERNOR_CMD": filepath.Join(project, "missing-aira"),
	})
	if result.err != nil || !strings.Contains(result.output, "aira CPU governor disabled") {
		t.Fatalf("failed relay did not fail open: err=%v output=%s", result.err, result.output)
	}
	if data, err := os.ReadFile(marker); err != nil || string(data) != "ran" {
		t.Fatalf("test did not run after relay failure: data=%q err=%v", data, err)
	}
}

func TestRealPytestForkDoesNotHoldGovernorRelay(t *testing.T) {
	pytest := requireRealPytest(t)
	project, pythonDir := realPytestProject(t, "")
	released := filepath.Join(project, "released")
	childDone := filepath.Join(project, "child-done")
	helper := writeGovernorHelper(t, project, `
import os, sys, time
print("active", flush=True)
sys.stdin.buffer.read()
open(os.environ["AIRA_RELAY_RELEASED"], "w").write(str(time.time_ns()))
`)
	writeTestFile(t, project, "test_fork.py", `
import os, time
def test_child_does_not_hold_relay():
    child = os.fork()
    if child == 0:
        time.sleep(1.0)
        open(os.environ["AIRA_CHILD_DONE"], "w").write(str(time.time_ns()))
        os._exit(0)
`)
	result := runPytest(t, pytest, project, pythonDir, map[string]string{
		"AIRA_GOVERNOR_CMD": helper, "AIRA_RELAY_RELEASED": released,
		"AIRA_CHILD_DONE": childDone,
	})
	if result.err != nil {
		t.Fatalf("fork suite failed: %v\n%s", result.err, result.output)
	}
	releaseTime := waitForRealPytestPath(t, released)
	childTime := waitForRealPytestPath(t, childDone)
	if string(releaseTime) >= string(childTime) {
		t.Fatalf("relay release=%s child done=%s", releaseTime, childTime)
	}
}

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

func realPytestProject(t *testing.T, extraConftest string) (string, string) {
	t.Helper()
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	pythonDir, err := ExtractPyLib()
	if err != nil {
		t.Fatal(err)
	}
	project := t.TempDir()
	writeTestFile(t, project, "conftest.py", activatingConftest+extraConftest)
	return project, pythonDir
}

func writeTestFile(t *testing.T, directory, name, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(directory, name), []byte(strings.TrimSpace(content)+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
}

type pytestResult struct {
	output string
	err    error
}

func runPytest(t *testing.T, pytest, project, pythonDir string, overrides map[string]string) pytestResult {
	t.Helper()
	command := exec.Command(pytest, "-q")
	command.Dir, command.Env = project, realPytestEnv(pythonDir, overrides)
	output, err := command.CombinedOutput()
	return pytestResult{output: string(output), err: err}
}

func realPytestEnv(pythonDir string, overrides map[string]string) []string {
	blocked := map[string]bool{
		"AIRA_PY_LIB": true, "AIRA_GOVERNOR": true, "AIRA_GOVERNOR_CMD": true,
		"AIRA_CONFINE_SCOPE_ID": true, "PYTHONPATH": true, "PYTEST_ADDOPTS": true,
		"AIRA_CONFINE_RESERVE_CMD": true, "PYTEST_PLUGINS": true, "PYTHONDONTWRITEBYTECODE": true,
		"PYTEST_DISABLE_PLUGIN_AUTOLOAD": true,
	}
	env := make([]string, 0, len(os.Environ())+len(overrides)+3)
	for _, entry := range os.Environ() {
		key, _, ok := strings.Cut(entry, "=")
		if ok && !blocked[key] && !strings.HasPrefix(key, "AIRA_TEST_") {
			env = append(env, entry)
		}
	}
	env = append(env, "AIRA_PY_LIB="+pythonDir, "PYTHONDONTWRITEBYTECODE=1", "PYTEST_DISABLE_PLUGIN_AUTOLOAD=1")
	for key, value := range overrides {
		env = append(env, key+"="+value)
	}
	return env
}

func writeGovernorHelper(t *testing.T, project, body string) string {
	t.Helper()
	path := filepath.Join(project, "fake-aira")
	writeTestFile(t, project, filepath.Base(path), "#!/usr/bin/env python3\n"+strings.TrimSpace(body))
	if err := os.Chmod(path, 0o700); err != nil {
		t.Fatal(err)
	}
	return path
}

func writeReserveHelper(t *testing.T, project, body string) string {
	t.Helper()
	path := filepath.Join(project, "fake-aira-reserve")
	writeTestFile(t, project, filepath.Base(path), "#!/usr/bin/env python3\n"+strings.TrimSpace(body))
	if err := os.Chmod(path, 0o700); err != nil {
		t.Fatal(err)
	}
	return path
}

func indexOf(t *testing.T, values []string, wanted string) int {
	t.Helper()
	for index, value := range values {
		if value == wanted {
			return index
		}
	}
	t.Fatalf("%q not found in %v", wanted, values)
	return -1
}

func waitForRealPytestFile(t *testing.T, path string, done <-chan error) {
	t.Helper()
	deadline := time.NewTimer(5 * time.Second)
	defer deadline.Stop()
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case err := <-done:
			t.Fatalf("pytest exited before contention marker: %v", err)
		case <-ticker.C:
			if _, err := os.Stat(path); err == nil {
				return
			}
		case <-deadline.C:
			t.Fatalf("timed out waiting for pytest marker %s", path)
		}
	}
}

func assertRealPytestItemRuns(t *testing.T, pytest string, overrides map[string]string, extraConftest []byte) {
	t.Helper()
	project, pythonDir := realPytestProject(t, string(extraConftest))
	marker := filepath.Join(project, "item-ran")
	writeTestFile(t, project, "test_fail_open.py", fmt.Sprintf("def test_runs():\n    open(%q, 'w').write('ran')\n", marker))
	result := runPytest(t, pytest, project, pythonDir, overrides)
	if result.err != nil {
		t.Fatalf("fail-open pytest failed: %v\n%s", result.err, result.output)
	}
	if data, err := os.ReadFile(marker); err != nil || string(data) != "ran" {
		t.Fatalf("fail-open item did not run: data=%q err=%v output=%s", data, err, result.output)
	}
}

func waitForRealPytestPath(t *testing.T, path string) []byte {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if data, err := os.ReadFile(path); err == nil {
			return data
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", path)
	return nil
}
