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
		"PYTHONDONTWRITEBYTECODE": true, "PYTEST_DISABLE_PLUGIN_AUTOLOAD": true,
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
