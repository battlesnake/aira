//go:build linux

package runner

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"aira/internal/testdeadline"
)

// confineImplWith routes the funnel through confineWithDeps with the given test
// deps, so a summary test exercises the real launch and the real funnel.
func confineImplWith(t *testing.T, deps confineDeps) {
	t.Helper()
	original := confineImpl
	t.Cleanup(func() { confineImpl = original })
	confineImpl = func(ctx context.Context, request ConfineRequest) (ConfineResult, error) {
		return confineWithDeps(ctx, request, deps)
	}
}

func summaryNumber(t *testing.T, values map[string]any, key string) int64 {
	t.Helper()
	number, ok := values[key].(json.Number)
	if !ok {
		t.Fatalf("%s = %#v, want a number", key, values[key])
	}
	n, err := number.Int64()
	if err != nil {
		t.Fatal(err)
	}
	return n
}

// verifies: AIRA-281 -- a REAL ci-shim launch (the confine_shim_linux_test harness,
// whose reportPeak/readUsage seams panic) reports the wait4 rusage under its OWN
// names and wall time, while peak_rss_bytes and cpu_*_us stay "unevaluated" and the
// containment is advisory.
//
// Mutations: forgetting *1024 (KiB -> bytes) fails the maxrss floor; copying the
// rusage into PeakRSS/CPUUser trips the shim's panic seam or the unevaluated
// assertions; RUSAGE_SELF of the supervisor reads the test binary's own CPU/RSS,
// not the child's, which the floors below do not tolerate on the CPU side.
func TestShimConfineSummaryReportsRusageUnderItsOwnNames(t *testing.T) {
	path := filepath.Join(t.TempDir(), "out.jsonl")
	confineImplWith(t, shimUnitDeps())
	script := `import time
b = bytearray(64 << 20)
for i in range(0, len(b), 4096):
    b[i] = 1
t = time.process_time()
x = 0
while time.process_time() - t < 0.25:
    for i in range(200000):
        x += i * i
`
	var stderr bytes.Buffer
	result, err := Confine(context.Background(), ConfineRequest{
		Argv: []string{"python3", "-c", script}, SelfPath: os.Args[0], Stderr: &stderr, Stdout: io.Discard,
		SummaryFile: path,
	})
	if err != nil || result.Exit != 0 {
		t.Fatalf("result=%+v err=%v stderr=%s", result, err, stderr.String())
	}
	lines := readSummaryLines(t, path)
	if len(lines) != 1 {
		t.Fatalf("%d lines, want 1", len(lines))
	}
	_, values := summaryParse(t, lines[0])
	if values["ran"] != true || values["terminated_by"] != "normal" || values["containment"] != string(ConfineContainmentAdvisory) {
		t.Fatalf("line = %v", values)
	}
	for _, key := range []string{"peak_rss_bytes", "cpu_user_us", "cpu_sys_us"} {
		if values[key] != "unevaluated" {
			t.Errorf("%s = %#v, want unevaluated in ci-shim mode (C10)", key, values[key])
		}
	}
	user := summaryNumber(t, values, "rusage_user_us")
	if user < 100_000 {
		t.Errorf("rusage_user_us = %d, want at least 100000 for a child that burned ~200ms CPU", user)
	}
	_ = summaryNumber(t, values, "rusage_sys_us")
	if maxrss := summaryNumber(t, values, "rusage_maxrss_largest_process_bytes"); maxrss < 64<<20 {
		t.Errorf("rusage_maxrss_largest_process_bytes = %d, want at least 64MiB (ru_maxrss is KiB)", maxrss)
	}
	if wall := summaryNumber(t, values, "wall_us"); wall <= 0 || wall < user/2 {
		t.Errorf("wall_us = %d (user %d)", wall, user)
	}
	// The trailer carries the same wall time but NOT the rusage (it stays focused).
	if !bytes.Contains(stderr.Bytes(), []byte(" wall=")) || bytes.Contains(stderr.Bytes(), []byte("wall=unevaluated")) {
		t.Errorf("trailer lacks an established wall=: %q", stderr.String())
	}
	if bytes.Contains(stderr.Bytes(), []byte("rusage")) {
		t.Errorf("trailer carries rusage: %q", stderr.String())
	}
	if !bytes.Contains(stderr.Bytes(), []byte("peak-rss=unevaluated cpu=unevaluated")) {
		t.Errorf("trailer peak-rss/cpu must stay unevaluated in ci-shim mode: %q", stderr.String())
	}
	if result.Status.PeakRSS != nil || result.Status.CPUUser != nil {
		t.Errorf("rusage leaked into PeakRSS/CPUUser: %+v", result.Status)
	}
}

// verifies: AIRA-281 -- on the real path wall time runs from the RELEASE write to
// the return of cmd.Wait(), not from invocation and not to the reap.
//
// (a) a 1.5 s admission wait is excluded (starting the clock at invocation reds);
// (b) a 0.3 s target is included (so is not zero); (c) a descendant that keeps the
// job's stderr open after the job exits extends wall to its close, pinning the
// documented "release to wait completion" meaning (stamping before Wait returns
// reds this arm).
func TestRealPathWallClockIsReleaseToWaitCompletion(t *testing.T) {
	run := func(t *testing.T, admitDelay time.Duration, argv []string) ConfineStatus {
		t.Helper()
		scope := &confineFakeScope{}
		deps := confineUnitDeps(scope)
		deps.admit = func(ctx context.Context, _ string, _ ConfineRequest, _ int64) (admissionResult, error) {
			select {
			case <-time.After(admitDelay):
			case <-ctx.Done():
			}
			return admissionResult{state: "immediate"}, nil
		}
		deps.readUsage = func(string) cgroupUsage { return cgroupUsage{} }
		deps.reportPeak = func(context.Context, ConfineRequest, ConfinePeakReport) error { return nil }
		result, err := confineWithDeps(context.Background(), ConfineRequest{
			Slice: "finite.slice", Argv: argv, SelfPath: os.Args[0], Stderr: &bytes.Buffer{}, Stdout: io.Discard,
		}, deps)
		if err != nil {
			t.Fatal(err)
		}
		if result.Status.WallUS == nil {
			t.Fatalf("WallUS not established: %+v", result.Status)
		}
		return result.Status
	}
	t.Run("admission wait is excluded", func(t *testing.T) {
		status := run(t, 1500*time.Millisecond, []string{"/bin/true"})
		if *status.WallUS >= time.Second.Microseconds() {
			t.Fatalf("WallUS = %dus includes the 1.5s admission wait", *status.WallUS)
		}
	})
	t.Run("the job runtime is included", func(t *testing.T) {
		status := run(t, 0, []string{"/bin/sh", "-c", "sleep 0.3"})
		if *status.WallUS < 300_000 {
			t.Fatalf("WallUS = %dus, want at least 300ms", *status.WallUS)
		}
	})
	t.Run("a descendant holding stderr extends wait completion", func(t *testing.T) {
		status := run(t, 0, []string{"/bin/sh", "-c", "sleep 0.3 >/dev/null & exit 0"})
		if *status.WallUS < 300_000 {
			t.Fatalf("WallUS = %dus, want at least 300ms (Wait drains the held stderr)", *status.WallUS)
		}
	})
}

type summaryCopyFailWriter struct{}

func (summaryCopyFailWriter) Write([]byte) (int, error) {
	return 0, errors.New("synthetic copy failure")
}

// verifies: AIRA-281 -- when cmd.Wait() returns an output-copy error the process
// HAS been reaped: waitConfineCommand still reports exit 3 with an undecoded
// status, but the rusage and the wait-return stamp are filled on that arm too, and
// the summary says exit:3, terminated_by:"unevaluated" with numeric rusage_*.
// Filling the rusage only on the decoded arms reds this.
func TestWaitConfineCommandKeepsRusageWhenOutputCopyFails(t *testing.T) {
	cmd := exec.Command("/bin/sh", "-c", "echo x >&2; exit 0")
	cmd.Stderr = summaryCopyFailWriter{}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	releasedAt := time.Now()
	exit, term := waitConfineCommand(cmd)
	if cmd.ProcessState == nil {
		t.Fatal("harness shape not reached: no ProcessState after a copy error")
	}
	if exit != 3 || term.Decoded {
		t.Fatalf("exit=%d decoded=%v, want 3/false for an output-copy error", exit, term.Decoded)
	}
	if term.Rusage == nil || term.WaitReturnedAt.IsZero() {
		t.Fatalf("rusage/stamp not filled on the undecoded arm: %+v", term)
	}
	var status ConfineStatus
	status.Slice, status.Name = "aira.slice", "n"
	status.TerminatedBy = classifyConfineTermination(term, cgroupUsage{}, nil, deadlineKindUnset)
	applyConfineRusage(&status, releasedAt, term)
	line, err := FormatConfineSummary(ConfineRequest{Argv: []string{"x"}}, ConfineResult{Status: status, Exit: exit}, nil)
	if err != nil {
		t.Fatal(err)
	}
	_, values := summaryParse(t, line)
	if values["exit"] != json.Number("3") || values["terminated_by"] != "unevaluated" {
		t.Fatalf("line = %v", values)
	}
	_ = summaryNumber(t, values, "rusage_user_us")
	_ = summaryNumber(t, values, "rusage_sys_us")
}

// verifies: AIRA-281 -- a job that traps SIGTERM and exits 0 after the supervisor
// was signalled is written as ran:true, exit:0, terminated_by:
// "supervisor-signal:SIGTERM". exit 0 alone is NOT a clean pass; rendering
// terminated_by as "normal" for exit 0, or omitting it, reds this.
func TestConfineSummaryTrappedSIGTERMIsNotNormal(t *testing.T) {
	path := filepath.Join(t.TempDir(), "out.jsonl")
	marker := filepath.Join(t.TempDir(), "ready")
	scope := &confineFakeScope{}
	deps := confineUnitDeps(scope)
	signals := make(chan os.Signal, 1)
	deps.signalSource = func(bool) (<-chan os.Signal, func()) { return signals, func() {} }
	deps.readUsage = func(string) cgroupUsage { return cgroupUsage{} }
	deps.reportPeak = func(context.Context, ConfineRequest, ConfinePeakReport) error { return nil }
	confineImplWith(t, deps)
	go func() {
		deadline := time.Now().Add(testdeadline.Wait(10 * time.Second))
		for time.Now().Before(deadline) {
			if _, err := os.Stat(marker); err == nil {
				signals <- syscall.SIGTERM
				return
			}
			time.Sleep(2 * time.Millisecond)
		}
	}()
	var diagnostics bytes.Buffer
	result, err := Confine(context.Background(), ConfineRequest{
		Slice:    "finite.slice",
		Argv:     []string{"/bin/sh", "-c", `trap 'exit 0' TERM; echo ready > "$1"; while :; do sleep 0.05; done`, "sh", marker},
		SelfPath: os.Args[0], Stderr: &diagnostics, SummaryFile: path,
	})
	if err != nil || result.Exit != 0 {
		t.Fatalf("probe shape not reached: exit=%d err=%v diagnostics=%q", result.Exit, err, diagnostics.String())
	}
	lines := readSummaryLines(t, path)
	if len(lines) != 1 {
		t.Fatalf("%d lines, want 1", len(lines))
	}
	_, values := summaryParse(t, lines[0])
	if values["ran"] != true || values["exit"] != json.Number("0") || values["terminated_by"] != "supervisor-signal:SIGTERM" {
		t.Fatalf("line = %v", values)
	}
}
