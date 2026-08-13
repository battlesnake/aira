//go:build linux

package runner

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestRealCgroupOOMUsageClassifiesPositiveOOM(t *testing.T) {
	parent := writableMemoryParent(t, "16777216")
	r, err := New(Config{CommonDir: t.TempDir(), CgroupParent: parent, Grace: time.Second, TermGrace: 100 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	if err := r.Probe(context.Background()); err != nil {
		t.Skipf("real writable cgroup-v2 delegation unavailable: %v", err)
	}
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("python3 is unavailable for the OOM fixture")
	}
	record, err := r.Launch(context.Background(), Request{Argv: []string{"python3", "-c", "x=bytearray(64*1024*1024); x[0]=1"}})
	if err != nil {
		t.Fatal(err)
	}
	if record.Status != StatusOOMKilled || record.PeakRSS == nil || *record.PeakRSS <= 0 {
		t.Fatalf("OOM discriminator record=%+v", record)
	}
}

func TestRealCgroupCPUUsageIsUserTime(t *testing.T) {
	r := realRunner(t)
	record, err := r.Launch(context.Background(), Request{Argv: []string{"/bin/sh", "-c", "i=0; while [ $i -lt 10000000 ]; do i=$((i+1)); done"}})
	if err != nil {
		t.Fatal(err)
	}
	if record.CPUUser == nil || *record.CPUUser <= 0 {
		t.Fatalf("CPU user time was not recorded: %+v", record)
	}
}

func TestRealCgroupPeakRSSUsageIsPositive(t *testing.T) {
	// peak_rss requires the memory controller enabled on the run cgroup. The
	// default ambient parent (e.g. a whale-run scope) may not delegate +memory to
	// its children, in which case memory.peak is absent and PeakRSS is honestly
	// nil (see the plan's memory-controller deferral). Here we run under an
	// unbounded memory-enabled parent so the reading itself is exercised.
	parent := writableMemoryParent(t, "max")
	r, err := New(Config{CommonDir: t.TempDir(), CgroupParent: parent, Grace: time.Second, TermGrace: 100 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	if err := r.Probe(context.Background()); err != nil {
		t.Skipf("real writable cgroup-v2 delegation unavailable: %v", err)
	}
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("python3 is unavailable for the memory fixture")
	}
	record, err := r.Launch(context.Background(), Request{Argv: []string{"python3", "-c", "x=bytearray(8*1024*1024); x[-1]=1"}})
	if err != nil {
		t.Fatal(err)
	}
	if record.PeakRSS == nil || *record.PeakRSS <= 0 {
		t.Fatalf("peak RSS was not recorded: %+v", record)
	}
}

func TestRealCgroupRunKillSnapshotsUsageWithoutOOMMisclassification(t *testing.T) {
	r := realRunner(t)
	outcome := launchAsync(t, r, Request{Argv: []string{"/bin/sh", "-c", "while :; do :; done"}})
	_, killErr := r.Kill(context.Background(), "RUN-1")
	if killErr != nil && !strings.Contains(killErr.Error(), "U_RUN_RECONCILE_REQUIRED") {
		t.Fatal(killErr)
	}
	result := <-outcome
	if result.err != nil && !strings.Contains(result.err.Error(), "U_RUN_RECONCILE_REQUIRED") {
		t.Fatal(result.err)
	}
	record, err := r.Get("RUN-1")
	if err != nil {
		t.Fatal(err)
	}
	if record.Status != StatusKilled || record.Status == StatusOOMKilled {
		t.Fatalf("run-kill status=%+v", record)
	}
	// Unsupported memory.peak or an individual read failure is explicitly
	// allowed to leave usage nil; when CPU accounting is available it must be
	// retained across the kill-before-remove path.
	if record.CPUUser != nil && *record.CPUUser < 0 {
		t.Fatalf("invalid retained CPU usage=%+v", record)
	}
}

func TestRealCgroupUsageRetainedAcrossTerminalRaces(t *testing.T) {
	tests := []struct {
		name string
		req  Request
		act  func(*testing.T, *Runner)
	}{
		{name: "normal", req: Request{Argv: []string{"/bin/sh", "-c", "i=0; while [ $i -lt 3000000 ]; do i=$((i+1)); done"}}},
		{name: "timeout", req: Request{Argv: []string{"/bin/sh", "-c", "while :; do :; done"}, Timeout: 50 * time.Millisecond}},
		{name: "reconcile", req: Request{Argv: []string{"/bin/sh", "-c", "i=0; while [ $i -lt 3000000 ]; do i=$((i+1)); done"}}, act: func(t *testing.T, r *Runner) {
			for i := 0; i < 20; i++ {
				if _, err := r.Reconcile(context.Background()); err != nil && !errors.Is(err, context.Canceled) {
					t.Fatal(err)
				}
				time.Sleep(time.Millisecond)
			}
		}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			r := realRunner(t)
			if tc.act == nil {
				record, err := r.Launch(context.Background(), tc.req)
				if err != nil {
					t.Fatal(err)
				}
				if record.CPUUser == nil || *record.CPUUser <= 0 {
					t.Fatalf("%s lost CPU usage: %+v", tc.name, record)
				}
				return
			}
			outcome := launchAsync(t, r, tc.req)
			tc.act(t, r)
			result := <-outcome
			if result.err != nil {
				t.Fatal(result.err)
			}
			if result.record == nil || result.record.CPUUser == nil || *result.record.CPUUser <= 0 {
				t.Fatalf("%s lost usage: %+v", tc.name, result.record)
			}
		})
	}
}

// writableMemoryParent creates a memory-controller-enabled parent cgroup so a
// child run gets memory.peak/memory.events. memMax is the memory.max value
// ("max" = unbounded, just enables the controller for peak measurement; a low
// byte count constrains a child into a kernel OOM). The parent name is unique
// per test so concurrent/adjacent tests never collide on a fixed dir.
func writableMemoryParent(t *testing.T, memMax string) string {
	t.Helper()
	mount, err := unifiedMount()
	if err != nil {
		t.Skipf("cgroup-v2 unavailable: %v", err)
	}
	current, err := currentCgroupPath(mount)
	if err != nil {
		t.Skipf("current cgroup unavailable: %v", err)
	}
	// The test process lives in `current`, so cgroup-v2's no-internal-process rule
	// forbids enabling +memory in current's OWN subtree_control. Create the parent
	// under current's parent (e.g. whale.slice), which already delegates +memory to
	// its children and holds no direct processes; then delegate +memory from the
	// parent to the run cgroup that will be its child.
	host := filepath.Dir(current)
	name := ".aira-m16-" + strings.NewReplacer("/", "_", " ", "_").Replace(t.Name())
	parent := filepath.Join(host, name)
	if err := os.Mkdir(parent, 0o755); err != nil {
		t.Skipf("cannot create memory parent under %s: %v", host, err)
	}
	t.Cleanup(func() {
		_ = os.WriteFile(filepath.Join(parent, "cgroup.kill"), []byte("1"), 0o644)
		_ = os.Remove(parent)
	})
	if err := os.WriteFile(filepath.Join(parent, "cgroup.subtree_control"), []byte("+memory"), 0o644); err != nil {
		t.Skipf("memory controller not available under %s: %v", host, err)
	}
	// Disable swap so a constrained child that exceeds memory.max is OOM-killed
	// rather than silently swapping (WSL2 has swap). Best-effort: harmless for the
	// unbounded ("max") peak case, and if the swap controller is unavailable the
	// constrained OOM test skips below on a non-OOM outcome.
	_ = os.WriteFile(filepath.Join(parent, "memory.swap.max"), []byte("0"), 0o644)
	if err := os.WriteFile(filepath.Join(parent, "memory.max"), []byte(memMax), 0o644); err != nil {
		t.Skipf("memory.max is not writable: %v", err)
	}
	return parent
}
