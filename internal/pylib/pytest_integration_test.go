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
	"strconv"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/unix"
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

func TestRealPytestIndependentProcessesRespectSlotCap(t *testing.T) {
	pytest := requireRealPytest(t)
	project, pythonDir := realPytestProject(t, "")
	slots := makeRealPytestSlots(t, 2)
	counter := filepath.Join(project, "counter.json")
	writeTestFile(t, project, "counter.json", `{"current": 0, "maximum": 0}`)
	writeTestFile(t, project, "test_cap.py", `
import fcntl
import json
import os
import time

COUNTER = os.environ["AIRA_TEST_COUNTER"]

def update(delta):
    with open(COUNTER + ".lock", "a+") as lock:
        fcntl.flock(lock.fileno(), fcntl.LOCK_EX)
        with open(COUNTER, encoding="utf-8") as source:
            state = json.load(source)
        state["current"] += delta
        state["maximum"] = max(state["maximum"], state["current"])
        with open(COUNTER, "w", encoding="utf-8") as destination:
            json.dump(state, destination)

def test_participating_process():
    update(1)
    try:
        time.sleep(0.35)
    finally:
        update(-1)
`)
	type invocation struct {
		command *exec.Cmd
		output  bytes.Buffer
	}
	invocations := make([]*invocation, 0, 8)
	for range 8 {
		call := &invocation{}
		call.command = exec.Command(pytest, "-q", "test_cap.py")
		call.command.Dir = project
		call.command.Env = realPytestEnv(pythonDir, map[string]string{
			"AIRA_CPU_SLOTS_DIR":     slots,
			"AIRA_CPU_POLL_INTERVAL": "0.02",
			"AIRA_CPU_MAX_WAIT":      "10",
			"AIRA_TEST_COUNTER":      counter,
		})
		call.command.Stdout, call.command.Stderr = &call.output, &call.output
		if err := call.command.Start(); err != nil {
			t.Fatal(err)
		}
		invocations = append(invocations, call)
	}
	for _, call := range invocations {
		if err := call.command.Wait(); err != nil {
			t.Fatalf("independent pytest failed: %v\n%s", err, call.output.String())
		}
	}
	var state struct {
		Current int `json:"current"`
		Maximum int `json:"maximum"`
	}
	data, err := os.ReadFile(counter)
	if err != nil || json.Unmarshal(data, &state) != nil {
		t.Fatalf("counter=%q err=%v", data, err)
	}
	if state.Current != 0 || state.Maximum != 2 {
		t.Fatalf("participating concurrency current=%d maximum=%d, want cap reached at 2", state.Current, state.Maximum)
	}
}

func TestRealPytestAcquireWaitExcludedFromPhaseDurations(t *testing.T) {
	pytest := requireRealPytest(t)
	project, pythonDir := realPytestProject(t, `
import json
import os
import aira_xdist_governor as governor

class ProbeGC:
    @staticmethod
    def collect():
        with open(os.environ["AIRA_TEST_GC_MARKER"], "w", encoding="utf-8") as marker:
            marker.write("sleeping")

governor.gc = ProbeGC
_aira_durations = []

def pytest_runtest_logreport(report):
    _aira_durations.append([report.when, report.duration])
    if report.when == "teardown":
        with open(os.environ["AIRA_TEST_DURATIONS"], "w", encoding="utf-8") as destination:
            json.dump(_aira_durations, destination)
`)
	writeTestFile(t, project, "test_duration.py", "def test_fast():\n    assert True\n")
	slots := makeRealPytestSlots(t, 1)
	holder := lockRealPytestSlot(t, filepath.Join(slots, "slot-0"))
	marker := filepath.Join(project, "gc-marker")
	durations := filepath.Join(project, "durations.json")
	command := exec.Command(pytest, "-q", "test_duration.py")
	command.Dir = project
	command.Env = realPytestEnv(pythonDir, map[string]string{
		"AIRA_CPU_SLOTS_DIR":     slots,
		"AIRA_CPU_POLL_INTERVAL": "0.02",
		"AIRA_CPU_MAX_WAIT":      "5",
		"AIRA_TEST_GC_MARKER":    marker,
		"AIRA_TEST_DURATIONS":    durations,
	})
	var output bytes.Buffer
	command.Stdout, command.Stderr = &output, &output
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- command.Wait() }()
	waitForRealPytestFile(t, marker, done)
	waited := time.Now()
	select {
	case err := <-done:
		t.Fatalf("pytest escaped held slot before release: %v\n%s", err, output.String())
	case <-time.After(800 * time.Millisecond):
	}
	if err := unix.Flock(int(holder.Fd()), unix.LOCK_UN); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatalf("pytest duration run failed: %v\n%s", err, output.String())
	}
	if elapsed := time.Since(waited); elapsed < 750*time.Millisecond {
		t.Fatalf("contention wait was too short to test duration exclusion: %s", elapsed)
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
		t.Fatalf("reported pytest phases included pre-yield wait: total=%fs reports=%v", phaseTotal, reports)
	}
}

func TestRealPytestGCCollectsOnceOnlyWhenSleeping(t *testing.T) {
	pytest := requireRealPytest(t)
	for _, test := range []struct {
		name      string
		contended bool
		wantGC    string
	}{
		{name: "free slot", wantGC: ""},
		{name: "contended until max wait", contended: true, wantGC: "1"},
	} {
		t.Run(test.name, func(t *testing.T) {
			project, pythonDir := realPytestProject(t, `
import os
import aira_xdist_governor as governor

class ProbeGC:
    @staticmethod
    def collect():
        path = os.environ["AIRA_TEST_GC_COUNT"]
        count = 0
        if os.path.exists(path):
            with open(path, encoding="utf-8") as source:
                count = int(source.read())
        with open(path, "w", encoding="utf-8") as destination:
            destination.write(str(count + 1))

governor.gc = ProbeGC
`)
			writeTestFile(t, project, "test_gc.py", "def test_runs():\n    assert True\n")
			slots := makeRealPytestSlots(t, 1)
			if test.contended {
				_ = lockRealPytestSlot(t, filepath.Join(slots, "slot-0"))
			}
			countPath := filepath.Join(project, "gc-count")
			result := runPytest(t, pytest, project, pythonDir, map[string]string{
				"AIRA_CPU_SLOTS_DIR":     slots,
				"AIRA_CPU_POLL_INTERVAL": "0.02",
				"AIRA_CPU_MAX_WAIT":      "0.15",
				"AIRA_TEST_GC_COUNT":     countPath,
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
			t.Fatalf("read gc count: %v", err)
		}
		return string(data)
	}
	probe := `
import os
import time
import aira_xdist_governor as governor

class ProbeGC:
    @staticmethod
    def collect():
        path = os.environ["AIRA_TEST_GC_COUNT"]
        value = int(open(path, encoding="utf-8").read()) if os.path.exists(path) else 0
        with open(path, "w", encoding="utf-8") as target:
            target.write(str(value + 1))

governor.gc = ProbeGC
governor._last_after_test_gc = time.monotonic() - 60
`
	t.Run("two fast tests collect once", func(t *testing.T) {
		project, pythonDir := realPytestProject(t, probe)
		countPath := filepath.Join(project, "gc-count")
		writeTestFile(t, project, "test_cadence.py", "def test_first(): pass\ndef test_second(): pass")
		result := runPytest(t, pytest, project, pythonDir, map[string]string{
			"AIRA_TEST_AFTER_TEST_GC_INTERVAL": "60", "AIRA_TEST_GC_COUNT": countPath,
		})
		if result.err != nil {
			t.Fatalf("pytest cadence failed: %v\n%s", result.err, result.output)
		}
		if got := count(t, countPath); got != "1" {
			t.Fatalf("after-test gc count=%q, want exactly one for tests within interval", got)
		}
	})
	t.Run("CPU wait collect does not suppress cadence", func(t *testing.T) {
		project, pythonDir := realPytestProject(t, probe)
		countPath := filepath.Join(project, "gc-count")
		writeTestFile(t, project, "test_independent.py", "def test_runs(): pass")
		slots := makeRealPytestSlots(t, 1)
		_ = lockRealPytestSlot(t, filepath.Join(slots, "slot-0"))
		result := runPytest(t, pytest, project, pythonDir, map[string]string{
			"AIRA_CPU_SLOTS_DIR": slots, "AIRA_CPU_POLL_INTERVAL": "0.02", "AIRA_CPU_MAX_WAIT": "0.15",
			"AIRA_TEST_AFTER_TEST_GC_INTERVAL": "60", "AIRA_TEST_GC_COUNT": countPath,
		})
		if result.err != nil {
			t.Fatalf("pytest independent timer failed: %v\n%s", result.err, result.output)
		}
		if got := count(t, countPath); got != "2" {
			t.Fatalf("gc count=%q, want one before CPU block plus one after test", got)
		}
	})
	t.Run("zero disables cadence", func(t *testing.T) {
		project, pythonDir := realPytestProject(t, probe)
		countPath := filepath.Join(project, "gc-count")
		writeTestFile(t, project, "test_disabled.py", "def test_runs(): pass")
		result := runPytest(t, pytest, project, pythonDir, map[string]string{
			"AIRA_TEST_AFTER_TEST_GC_INTERVAL": "0", "AIRA_TEST_GC_COUNT": countPath,
		})
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
        value = int(open(path, encoding="utf-8").read()) if os.path.exists(path) else 0
        with open(path, "w", encoding="utf-8") as target:
            target.write(str(value + 1))

governor.gc = ProbeGC
`)
	countPath := filepath.Join(project, "gc-count")
	helper := writeReserveHelper(t, project, `
import os, sys, time
deadline = time.monotonic() + 0.5
while not os.path.exists(os.environ["AIRA_TEST_GC_COUNT"]) and time.monotonic() < deadline:
    time.sleep(0.01)
estimate = sys.argv[sys.argv.index("--bytes") + 1]
print("granted reserve=%s basis=pinned:client" % estimate, flush=True)
sys.stdin.buffer.read()
`)
	writeTestFile(t, project, "test_ram_wait.py", "def test_runs(): pass")
	result := runPytest(t, pytest, project, pythonDir, map[string]string{
		"AIRA_CPU_SLOTS_DIR":     makeRealPytestSlots(t, 1),
		"AIRA_TEST_MEM_GOVERNOR": "1", "AIRA_TEST_MEM_DEFAULT": "8M",
		"AIRA_CONFINE_RESERVE_CMD": helper, "AIRA_TEST_GC_COUNT": countPath,
		"AIRA_TEST_AFTER_TEST_GC_INTERVAL": "0",
	})
	if result.err != nil {
		t.Fatalf("pytest RAM wait failed: %v\n%s", result.err, result.output)
	}
	data, err := os.ReadFile(countPath)
	if err != nil || string(data) != "1" {
		t.Fatalf("RAM wait gc count=%q err=%v, want exactly one pre-collect", data, err)
	}
}

func TestRealPytestRAMImmediateGrantSkipsCollect(t *testing.T) {
	pytest := requireRealPytest(t)
	project, pythonDir := realPytestProject(t, `
import os
import aira_xdist_governor as governor

class ProbeGC:
    @staticmethod
    def collect():
        with open(os.environ["AIRA_TEST_GC_COUNT"], "w", encoding="utf-8") as target:
            target.write("collected")

class Stdin:
    def close(self):
        pass

class ImmediateGrantProcess:
    def __init__(self, descriptor):
        self.stdin = Stdin()
        self.stdout = os.fdopen(descriptor, "rb", buffering=0)
    def poll(self):
        return None
    def wait(self, timeout=None):
        return 0
    def terminate(self):
        pass
    def kill(self):
        pass

def immediate_grant_popen(command, **kwargs):
    descriptor, writer = os.pipe()
    estimate = command[command.index("--bytes") + 1]
    os.write(writer, ("granted reserve=%s basis=pinned:client\\n" % estimate).encode("ascii"))
    os.close(writer)
    return ImmediateGrantProcess(descriptor)

governor.gc = ProbeGC
governor.subprocess.Popen = immediate_grant_popen
`)
	countPath := filepath.Join(project, "gc-count")
	writeTestFile(t, project, "test_ram_immediate.py", "def test_runs(): pass")
	result := runPytest(t, pytest, project, pythonDir, map[string]string{
		"AIRA_CPU_SLOTS_DIR":     makeRealPytestSlots(t, 1),
		"AIRA_TEST_MEM_GOVERNOR": "1", "AIRA_TEST_MEM_DEFAULT": "8M",
		"AIRA_CONFINE_RESERVE_CMD": "fake-aira", "AIRA_TEST_GC_COUNT": countPath,
		"AIRA_TEST_AFTER_TEST_GC_INTERVAL": "0",
	})
	if result.err != nil {
		t.Fatalf("pytest immediate RAM grant failed: %v\n%s", result.err, result.output)
	}
	if _, err := os.Stat(countPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("immediate RAM grant collected: %v", err)
	}
}

func TestRealPytestTotalFailOpen(t *testing.T) {
	pytest := requireRealPytest(t)
	t.Run("unset directory", func(t *testing.T) {
		assertRealPytestItemRuns(t, pytest, nil, nil)
	})
	t.Run("absent directory", func(t *testing.T) {
		assertRealPytestItemRuns(t, pytest, map[string]string{
			"AIRA_CPU_SLOTS_DIR": filepath.Join(t.TempDir(), "absent"),
		}, nil)
	})
	t.Run("malformed poll interval", func(t *testing.T) {
		assertRealPytestItemRuns(t, pytest, map[string]string{
			"AIRA_CPU_SLOTS_DIR":     makeRealPytestSlots(t, 1),
			"AIRA_CPU_POLL_INTERVAL": "soon",
		}, nil)
	})
	t.Run("malformed maximum wait", func(t *testing.T) {
		assertRealPytestItemRuns(t, pytest, map[string]string{
			"AIRA_CPU_SLOTS_DIR": makeRealPytestSlots(t, 1),
			"AIRA_CPU_MAX_WAIT":  "forever",
		}, nil)
	})
	t.Run("permission error", func(t *testing.T) {
		if os.Geteuid() == 0 {
			t.Skip("root bypasses directory permission bits")
		}
		slots := makeRealPytestSlots(t, 1)
		if err := os.Chmod(slots, 0); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = os.Chmod(slots, 0o700) })
		assertRealPytestItemRuns(t, pytest, map[string]string{"AIRA_CPU_SLOTS_DIR": slots}, nil)
	})
	t.Run("maximum wait", func(t *testing.T) {
		slots := makeRealPytestSlots(t, 1)
		_ = lockRealPytestSlot(t, filepath.Join(slots, "slot-0"))
		started := time.Now()
		assertRealPytestItemRuns(t, pytest, map[string]string{
			"AIRA_CPU_SLOTS_DIR":     slots,
			"AIRA_CPU_POLL_INTERVAL": "0.02",
			"AIRA_CPU_MAX_WAIT":      "0.15",
		}, nil)
		if elapsed := time.Since(started); elapsed > 1500*time.Millisecond {
			t.Fatalf("max-wait fail-open stalled for %s", elapsed)
		}
	})
	t.Run("removed while waiting", func(t *testing.T) {
		slots := makeRealPytestSlots(t, 1)
		slot := filepath.Join(slots, "slot-0")
		_ = lockRealPytestSlot(t, slot)
		marker := filepath.Join(t.TempDir(), "sleeping")
		extra := `
import os
import aira_xdist_governor as governor

class ProbeGC:
    @staticmethod
    def collect():
        with open(os.environ["AIRA_TEST_GC_MARKER"], "w", encoding="utf-8") as target:
            target.write("sleeping")

governor.gc = ProbeGC
`
		project, pythonDir := realPytestProject(t, extra)
		itemMarker := filepath.Join(project, "item-ran")
		writeTestFile(t, project, "test_fail_open.py", fmt.Sprintf("def test_runs():\n    open(%q, 'w').write('ran')\n", itemMarker))
		command := exec.Command(pytest, "-q", "test_fail_open.py")
		command.Dir = project
		command.Env = realPytestEnv(pythonDir, map[string]string{
			"AIRA_CPU_SLOTS_DIR":     slots,
			"AIRA_CPU_POLL_INTERVAL": "0.02",
			"AIRA_CPU_MAX_WAIT":      "5",
			"AIRA_TEST_GC_MARKER":    marker,
		})
		var output bytes.Buffer
		command.Stdout, command.Stderr = &output, &output
		if err := command.Start(); err != nil {
			t.Fatal(err)
		}
		done := make(chan error, 1)
		go func() { done <- command.Wait() }()
		waitForRealPytestFile(t, marker, done)
		if err := os.Remove(slot); err != nil {
			t.Fatal(err)
		}
		if err := os.Remove(slots); err != nil {
			t.Fatal(err)
		}
		if err := <-done; err != nil {
			t.Fatalf("removed-dir case failed: %v\n%s", err, output.String())
		}
		if data, err := os.ReadFile(itemMarker); err != nil || string(data) != "ran" {
			t.Fatalf("item did not run after directory removal: data=%q err=%v", data, err)
		}
	})
	t.Run("incomplete existing population", func(t *testing.T) {
		slots := t.TempDir()
		slot := filepath.Join(slots, "slot-1")
		if err := os.WriteFile(slot, nil, 0o600); err != nil {
			t.Fatal(err)
		}
		_ = lockRealPytestSlot(t, slot)
		started := time.Now()
		assertRealPytestItemRuns(t, pytest, map[string]string{
			"AIRA_CPU_SLOTS_DIR":     slots,
			"AIRA_CPU_POLL_INTERVAL": "0.02",
			"AIRA_CPU_MAX_WAIT":      "2",
		}, nil)
		if elapsed := time.Since(started); elapsed > 1200*time.Millisecond {
			t.Fatalf("invalid loser population did not fail open promptly: %s", elapsed)
		}
	})
}

func TestRealPytestForkDoesNotPinSlot(t *testing.T) {
	pytest := requireRealPytest(t)
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	pythonDir, err := ExtractPyLib()
	if err != nil {
		t.Fatal(err)
	}
	project := t.TempDir()
	slots := makeRealPytestSlots(t, 1)
	writeTestFile(t, project, "test_fork_slot.py", `
import os
import sys

sys.path.insert(0, os.environ["AIRA_PY_LIB"])
import aira_xdist_governor as governor

def test_fork_does_not_pin_slot():
    slot = os.path.join(os.environ["AIRA_CPU_SLOTS_DIR"], "slot-0")
    descriptor = governor._try_slots([slot])
    assert descriptor is not None
    ready_read, ready_write = os.pipe()
    release_read, release_write = os.pipe()
    child = os.fork()
    if child == 0:
        os.close(ready_read)
        os.close(release_write)
        os.write(ready_write, b"1")
        os.close(ready_write)
        os.read(release_read, 1)
        os.close(release_read)
        os._exit(0)
    os.close(ready_write)
    os.close(release_read)
    reacquired = None
    try:
        assert os.read(ready_read, 1) == b"1"
        getattr(governor, "_release_slot", os.close)(descriptor)
        descriptor = None
        reacquired = governor._try_slots([slot])
        assert reacquired is not None
    finally:
        if descriptor is not None:
            getattr(governor, "_release_slot", os.close)(descriptor)
        if reacquired is not None:
            getattr(governor, "_release_slot", os.close)(reacquired)
        os.close(ready_read)
        os.write(release_write, b"1")
        os.close(release_write)
        waited, status = os.waitpid(child, 0)
        assert waited == child
        assert os.waitstatus_to_exitcode(status) == 0
`)
	result := runPytest(t, pytest, project, pythonDir, map[string]string{"AIRA_CPU_SLOTS_DIR": slots})
	if result.err != nil {
		t.Fatalf("forked child pinned CPU slot: %v\n%s", result.err, result.output)
	}
}

func TestRealPytestRAMMarkerPrecedencePinnedAndRegistered(t *testing.T) {
	pytest := requireRealPytest(t)
	project, pythonDir := realPytestProject(t, "")
	requests := filepath.Join(project, "requests.jsonl")
	helper := writeReserveHelper(t, project, `
import json, os, sys
with open(os.environ["AIRA_TEST_REQUESTS"], "a", encoding="utf-8") as target:
    target.write(json.dumps(sys.argv[1:]) + "\n")
estimate = sys.argv[sys.argv.index("--bytes") + 1]
print("granted reserve=%s basis=pinned:client" % estimate, flush=True)
sys.stdin.buffer.read()
`)
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
def test_invalid_uses_default(): pass
`)
	result := runPytest(t, pytest, project, pythonDir, map[string]string{
		"AIRA_TEST_MEM_GOVERNOR": "1", "AIRA_TEST_MEM_DEFAULT": "8M",
		"AIRA_CONFINE_RESERVE_CMD": helper, "AIRA_TEST_REQUESTS": requests,
		"PYTEST_ADDOPTS": "--strict-markers",
	})
	if result.err != nil {
		t.Fatalf("RAM marker pytest failed: %v\n%s", result.err, result.output)
	}
	data, err := os.ReadFile(requests)
	if err != nil {
		t.Fatal(err)
	}
	wants := map[string]string{
		"test_module_scope": "67108864", "test_class_scope": "33554432",
		"test_test_scope": "16777216",
		// "4GB" is now a valid full-unit spelling (== 4GiB == 4<<30); a float
		// ("1.5G") is still rejected and falls to the 8M default with one log.
		"test_full_unit_spelling": "4294967296", "test_invalid_uses_default": "8388608",
	}
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		var argv []string
		if err := json.Unmarshal([]byte(line), &argv); err != nil {
			t.Fatal(err)
		}
		joined := strings.Join(argv, " ")
		if !strings.Contains(joined, " --pinned ") && !strings.Contains(joined, "--pinned") {
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
	if len(wants) != 0 || strings.Count(result.output, "invalid aira_mem marker") != 1 || strings.Contains(result.output, "PytestUnknownMarkWarning") {
		t.Fatalf("missing=%v output=%s requests=%s", wants, result.output, data)
	}
}

func TestRealPytestRAMWeightedConcurrencySharesBudgetAcrossSuites(t *testing.T) {
	pytest := requireRealPytest(t)
	project, pythonDir := realPytestProject(t, "")
	state := filepath.Join(project, "ram-state.json")
	writeTestFile(t, project, "ram-state.json", `{"current":0,"maximum":0,"count":0,"max_count":0,"heavy":0,"heavy_max":0}`)
	helper := writeReserveHelper(t, project, `
import fcntl, json, os, sys, time
state_path = os.environ["AIRA_RAM_STATE"]
estimate = int(sys.argv[sys.argv.index("--bytes") + 1])
budget = int(os.environ["AIRA_RAM_BUDGET"])
while True:
    with open(state_path + ".lock", "a+") as lock:
        fcntl.flock(lock.fileno(), fcntl.LOCK_EX)
        with open(state_path, encoding="utf-8") as source: state = json.load(source)
        if state["current"] + estimate <= budget:
            state["current"] += estimate
            state["maximum"] = max(state["maximum"], state["current"])
            state["count"] += 1
            state["max_count"] = max(state["max_count"], state["count"])
            if estimate > budget // 2:
                state["heavy"] += 1
                state["heavy_max"] = max(state["heavy_max"], state["heavy"])
            with open(state_path, "w", encoding="utf-8") as target: json.dump(state, target)
            break
    time.sleep(0.01)
print("granted reserve=%d basis=pinned:client" % estimate, flush=True)
sys.stdin.buffer.read()
with open(state_path + ".lock", "a+") as lock:
    fcntl.flock(lock.fileno(), fcntl.LOCK_EX)
    with open(state_path, encoding="utf-8") as source: state = json.load(source)
    state["current"] -= estimate
    state["count"] -= 1
    if estimate > budget // 2: state["heavy"] -= 1
    with open(state_path, "w", encoding="utf-8") as target: json.dump(state, target)
`)
	writeTestFile(t, project, "test_ram_cap.py", `
import os, pytest, time

@pytest.mark.aira_mem(os.environ["AIRA_CASE_ESTIMATE"])
def test_participating_suite():
    time.sleep(0.2)
`)
	type invocation struct {
		command *exec.Cmd
		output  bytes.Buffer
	}
	estimates := []string{"70", "70", "20", "20", "20", "20"}
	invocations := make([]*invocation, 0, len(estimates))
	for _, estimate := range estimates {
		call := &invocation{}
		call.command = exec.Command(pytest, "-q", "test_ram_cap.py")
		call.command.Dir = project
		call.command.Env = realPytestEnv(pythonDir, map[string]string{
			"AIRA_TEST_MEM_GOVERNOR": "1", "AIRA_TEST_MEM_DEFAULT": "10",
			"AIRA_CONFINE_RESERVE_CMD": helper, "AIRA_RAM_STATE": state,
			"AIRA_RAM_BUDGET": "100", "AIRA_CASE_ESTIMATE": estimate,
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
	// The budget ceiling is enforced by the fake helper (production admission is
	// tested in Go); the load-bearing plugin behaviour is that reservations are
	// held then released (Current/Count back to 0), heavy tests serialise
	// (HeavyMax==1), light tests parallelise (MaxCount>=2), and the observed peak
	// weight packed near the budget (Maximum>=70). ("Maximum>100" was dead — the
	// helper caps at 100 — so it is dropped.)
	if observed.Current != 0 || observed.Count != 0 || observed.Maximum < 70 || observed.HeavyMax != 1 || observed.MaxCount < 2 {
		t.Fatalf("weighted shared state=%+v", observed)
	}
}

func TestRealPytestRAMHelperFailureIsInstantAndFailOpen(t *testing.T) {
	pytest := requireRealPytest(t)
	started := time.Now()
	project, pythonDir := realPytestProject(t, "")
	marker := filepath.Join(project, "ran")
	writeTestFile(t, project, "test_fail_open_ram.py", fmt.Sprintf("def test_runs(): open(%q, 'w').write('yes')\n", marker))
	result := runPytest(t, pytest, project, pythonDir, map[string]string{
		"AIRA_TEST_MEM_GOVERNOR": "1", "AIRA_TEST_MEM_DEFAULT": "8M",
		"AIRA_CONFINE_RESERVE_CMD": filepath.Join(project, "missing-aira"),
	})
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
	released := filepath.Join(project, "released")
	childDone := filepath.Join(project, "child-done")
	helper := writeReserveHelper(t, project, `
import os, sys, time
estimate = sys.argv[sys.argv.index("--bytes") + 1]
print("granted reserve=%s basis=pinned:client" % estimate, flush=True)
sys.stdin.buffer.read()
with open(os.environ["AIRA_RELEASED"], "w", encoding="utf-8") as target: target.write(str(time.time_ns()))
`)
	writeTestFile(t, project, "test_ram_fork.py", `
import os, time
def test_fork_child_does_not_hold_reservation():
    child = os.fork()
    if child == 0:
        os.close(1); os.close(2)
        time.sleep(1.0)
        with open(os.environ["AIRA_CHILD_DONE"], "w", encoding="utf-8") as target: target.write(str(time.time_ns()))
        os._exit(0)
`)
	result := runPytest(t, pytest, project, pythonDir, map[string]string{
		"AIRA_TEST_MEM_GOVERNOR": "1", "AIRA_TEST_MEM_DEFAULT": "8M",
		"AIRA_CONFINE_RESERVE_CMD": helper, "AIRA_RELEASED": released, "AIRA_CHILD_DONE": childDone,
	})
	if result.err != nil {
		t.Fatalf("fork pytest failed: %v\n%s", result.err, result.output)
	}
	waitForPath := func(path string) []byte {
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
	releaseTime, childTime := waitForPath(released), waitForPath(childDone)
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
def test_inert_shim(): pass
`)
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
	slots := makeRealPytestSlots(t, 1)
	_ = lockRealPytestSlot(t, filepath.Join(slots, "slot-0"))
	writeTestFile(t, project, "test_optout.py", `
import sys

def test_governor_was_not_imported():
    assert "aira_xdist_governor" not in sys.modules
`)
	started := time.Now()
	result := runPytest(t, pytest, project, pythonDir, map[string]string{
		"AIRA_CPU_SLOTS_DIR": slots,
		"AIRA_CPU_MAX_WAIT":  "2",
	})
	if result.err != nil {
		t.Fatalf("opt-out pytest failed: %v\n%s", result.err, result.output)
	}
	if elapsed := time.Since(started); elapsed > 1200*time.Millisecond {
		t.Fatalf("opt-out run was governed: %s", elapsed)
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

func makeRealPytestSlots(t *testing.T, count int) string {
	t.Helper()
	directory := t.TempDir()
	for index := 0; index < count; index++ {
		if err := os.WriteFile(filepath.Join(directory, "slot-"+strconv.Itoa(index)), nil, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return directory
}

func lockRealPytestSlot(t *testing.T, path string) *os.File {
	t.Helper()
	file, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	if err := unix.Flock(int(file.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		_ = file.Close()
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = file.Close() })
	return file
}

type pytestResult struct {
	output string
	err    error
}

func runPytest(t *testing.T, pytest, project, pythonDir string, overrides map[string]string) pytestResult {
	t.Helper()
	command := exec.Command(pytest, "-q")
	command.Dir = project
	command.Env = realPytestEnv(pythonDir, overrides)
	output, err := command.CombinedOutput()
	return pytestResult{output: string(output), err: err}
}

func realPytestEnv(pythonDir string, overrides map[string]string) []string {
	blocked := map[string]bool{
		"AIRA_PY_LIB": true, "AIRA_CPU_SLOTS_DIR": true,
		"AIRA_CPU_POLL_INTERVAL": true, "AIRA_CPU_MAX_WAIT": true,
		"PYTHONPATH": true, "PYTEST_ADDOPTS": true, "PYTEST_PLUGINS": true,
		"AIRA_CONFINE_RESERVE_CMD": true,
		"PYTHONDONTWRITEBYTECODE":  true, "PYTEST_DISABLE_PLUGIN_AUTOLOAD": true,
	}
	env := make([]string, 0, len(os.Environ())+len(overrides)+3)
	for _, entry := range os.Environ() {
		key, _, ok := strings.Cut(entry, "=")
		if ok && !blocked[key] && !strings.HasPrefix(key, "AIRA_TEST_") {
			env = append(env, entry)
		}
	}
	env = append(env,
		"AIRA_PY_LIB="+pythonDir,
		"PYTHONDONTWRITEBYTECODE=1",
		"PYTEST_DISABLE_PLUGIN_AUTOLOAD=1",
	)
	for key, value := range overrides {
		env = append(env, key+"="+value)
	}
	return env
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
