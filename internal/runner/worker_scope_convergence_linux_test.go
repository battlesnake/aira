//go:build linux

package runner

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"aira/internal/cgrouptest"
	"aira/internal/testdeadline"
)

// workerScopeConvergenceBudget is the bound the AIRA-35 regression rests on.
//
// Measured on the development host (WSL2 6.18.33.2, 20 GiB swap active), time
// from launch to the worker scope's own memory.events oom_group_kill, with
// memory.swap.max=0 and no memory.high:
//
//	32 MiB cap:  0.029 / 0.031 / 0.033 s
//	512 MiB cap: 0.361 / 0.481 s
//
// against the two variants this fix rejected:
//
//	memory.high = 80% of max: did NOT converge in 420 s at a 32 MiB cap
//	                          (5475 memory.high events, ZERO memory.max events)
//	memory.high = 95% of max: 0.73–1.71 s at 32 MiB, but 16.42 / 18.42 s at
//	                          512 MiB — the delay tracks the ABSOLUTE width of
//	                          the throttle window, so it grows with the cap
//	no memory.swap.max:       never killed at all, at any cap size; the child
//	                          exits 0 with its overage paged out to swap
//
// 5 s is ~150x the 32 MiB measurement and ~10x the 512 MiB one — loose enough
// to survive a heavily loaded shared machine, tight enough that every rejected
// variant fails it at the production cap size.
const workerScopeConvergenceBudget = 5 * time.Second

// workerScopeConvergenceCeiling is the hard upper bound on that budget after
// testdeadline scaling, and it is what keeps this a DISCRIMINATING assertion
// rather than a vacuous one.
//
// testdeadline.Wait multiplies by Scale(), which includes raceScale = 4 under
// the race detector (and AIRA_TEST_DEADLINE_SCALE on top). A scaled 5 s becomes
// 20 s under -race — ABOVE the 16-18 s that `memory.high` at 95% takes at the
// 512 MiB cap, which is the single measurement this whole change rests on for
// rejecting that variant. The test would then pass against the fix it exists to
// rule out. AIRA-20 is an open ticket to re-add -race to CI, so this is a live
// hazard, not a hypothetical one.
//
// Scaling is still honoured up to this ceiling, because a loaded host is real.
// The ceiling is safe to impose because the quantity being timed is mostly NOT
// race-instrumented: it is the kernel's time to OOM-kill a separate, ordinary
// python3 child, with this Go test contributing only a 10 ms poll loop.
// 10 s keeps 2x of scaling headroom over the unscaled budget while staying
// below even the 11.5 s that 95% takes at a 256 MiB cap.
const workerScopeConvergenceCeiling = 10 * time.Second

// verifies: AIRA-35 — a worker scope built by the PRODUCTION CreateWorkerScope
// converges to its own oom.group kill in seconds, not minutes, and does so
// without a swap detour.
//
// This is the committed reproduction of the plan's measurement
// (docs/superpowers/specs/2026-09-06-aira35-worker-oom-convergence-plan.md).
// It runs UN-GATED, unlike the AIRA-30-era e2e it replaces as the convergence
// guard: the multi-minute hang that forced that gate lived in the memory.high
// reclaim-throttle path this change removes.
//
// The cap TABLE is load-bearing, not thoroughness. A 32 MiB-only test would
// pass against memory.high=95%, the runner-up fix this change rejected — that
// rejection rests entirely on the 512 MiB row, which is the cap this product
// actually ships (internal/pylib/aitest/__init__.py's _resolve_estimated_bytes
// returns a flat 512 MiB), so the test has to exercise it.
func TestWorkerScopeOOMGroupKillConvergesPromptly(t *testing.T) {
	for _, test := range []struct {
		name      string
		memoryMax int64
		allocate  int64
	}{
		{"small cap", 32 << 20, 512 << 20},
		// The shipped production default, and an ACCEPTED COST rather than a
		// free row. cgrouptest.IsolatedScopeParent places the scope under the
		// ambient cgroup's PARENT, so under `aira confine` it is a sibling of
		// the confine scope: this 512 MiB is charged to the shared aira.slice
		// and is invisible to the reserve ledger, which counts only
		// `.aira-CONFINE-*`. That is the AIRA-27 slice-pressure shape, so it is
		// named here rather than glossed.
		//
		// Kept anyway, because the alternative is worse: 512 MiB is the cap
		// this product actually ships, the rejection of the runner-up fix
		// (memory.high at 95%) rests entirely on this row's 16-18 s, and a
		// 32 MiB-only test passes against that variant. The cost is bounded and
		// brief — residency is capped by the scope's own memory.max and the
		// measured run is ~240 ms — against a 64 GiB slice ceiling. The
		// allocation is 640 MiB, not 1 GiB: it only has to exceed the cap, and
		// the extra work bought nothing.
		{"production default cap", 512 << 20, 640 << 20},
	} {
		t.Run(test.name, func(t *testing.T) {
			outer := newWorkerConvergenceOuterScope(t)
			scopePath, swapCap, err := CreateWorkerScope(context.Background(), outer, "1", test.memoryMax)
			if err != nil {
				t.Fatalf("CreateWorkerScope: %v", err)
			}
			if swapCap != WorkerAdmitSwapCapEnforced {
				// Not a failure of the code under test: on a kernel that
				// cannot bound swap, an over-allocating worker legitimately
				// pages out instead of dying, and asserting convergence here
				// would be asserting something the host cannot deliver.
				// Reported with its reason rather than passing silently.
				t.Skipf("host cannot bound worker swap (swap_cap=%q); convergence is not "+
					"establishable here — this is the coverage gap, not a pass", swapCap)
			}

			start := time.Now()
			child := startWorkerScopeAllocator(t, scopePath, test.allocate)
			budget := testdeadline.Wait(workerScopeConvergenceBudget)
			if budget > workerScopeConvergenceCeiling {
				budget = workerScopeConvergenceCeiling
			}
			deadline := time.Now().Add(budget)
			for {
				if kills := readWorkerScopeOOMGroupKills(t, scopePath); kills > 0 {
					t.Logf("%s: oom_group_kill after %v", test.name, time.Since(start))
					break
				}
				if time.Now().After(deadline) {
					t.Fatalf("worker scope did not reach an oom.group kill within %v "+
						"(memory.events: %s). A worker that over-allocates must self-contain "+
						"PROMPTLY: the AIRA-35 regressions this bound catches are re-adding "+
						"memory.high (which pinned the cgroup below its own hard cap for over "+
						"420 s at 80%%, and 16-18 s at 95%% at this cap size) or dropping "+
						"memory.swap.max=0 (which stops the kill happening at all)",
						budget, readWorkerScopeFile(t, scopePath, "memory.events"))
				}
				time.Sleep(10 * time.Millisecond)
			}
			_ = child.Process.Kill()
			_, _ = child.Process.Wait()

			// Corroborating, not load-bearing: the kill came from the cap, not
			// after a swap detour. memory.swap.peak arrived in ~6.5, so on an
			// older kernel this is reported UNEVALUATED rather than asserted
			// against a file that is not there or silently skipped.
			raw, err := os.ReadFile(filepath.Join(scopePath, "memory.swap.peak"))
			if err != nil {
				t.Logf("memory.swap.peak unavailable on this kernel (%v); the no-swap-detour "+
					"corroboration is unevaluated for this run", err)
				return
			}
			if strings.TrimSpace(string(raw)) != "0" {
				t.Fatalf("memory.swap.peak=%q, want 0 — the worker reached swap despite "+
					"memory.swap.max=0, so the cap did not hold", strings.TrimSpace(string(raw)))
			}
		})
	}
}

// verifies: AIRA-35 finding (A) — the PLATFORM fact the production change rests
// on: with swap available and unbounded, memory.max alone does NOT contain a
// runaway. It is reclaimed into swap instead of being killed.
//
// This is deliberately not built through CreateWorkerScope, which always writes
// the cap now: the claim under test is about cgroup-v2, not about AIRA code.
// Without it, a future reader has no way to tell whether memory.swap.max=0 is
// load-bearing or cargo cult, and nothing fails if a kernel change ever makes
// the cap redundant.
//
// It asserts swap USE (positive evidence the allocation escaped memory.max),
// not the absence of an OOM kill: a swapless host, or an exhausted shared swap
// budget, would kill the child for reasons that say nothing about this claim,
// and a test that reports fail where it cannot establish its result is exactly
// what AIRA forbids. Kept small (8 MiB cap, 64 MiB allocation) because it
// writes to swap on a shared machine.
func TestUncappedSwapLetsAWorkerEscapeItsMemoryMax(t *testing.T) {
	outer := newWorkerConvergenceOuterScope(t)
	scopePath := filepath.Join(outer, ".aira-swap-control")
	if err := os.Mkdir(scopePath, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, write := range []struct{ file, value string }{
		{"memory.oom.group", "1"},
		{"memory.max", strconv.Itoa(8 << 20)},
	} {
		if err := os.WriteFile(filepath.Join(scopePath, write.file), []byte(write.value), 0o644); err != nil {
			cgrouptest.SkipOrFailRealCgroup(t, "cannot write %s: %v", write.file, err)
		}
	}
	requireUsableSwap(t, scopePath)

	child := startWorkerScopeAllocator(t, scopePath, 64<<20)
	t.Cleanup(func() { _ = child.Process.Kill(); _, _ = child.Process.Wait() })

	// The child signals once it is holding its allocation, so the observation
	// window is parent-controlled rather than a race against a sampler.
	deadline := time.Now().Add(testdeadline.Wait(30 * time.Second))
	var peak int64
	for time.Now().Before(deadline) {
		if current := readWorkerScopeInt(t, scopePath, "memory.swap.current"); current > peak {
			peak = current
		}
		if peak > 0 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	// memory.swap.peak is authoritative where it exists (kernel >= ~6.5) and
	// cannot miss a window the poll above might straddle.
	if reported := readWorkerScopeInt(t, scopePath, "memory.swap.peak"); reported > peak {
		peak = reported
	}
	if peak <= 0 {
		// This test's own contract (above) is that it must never report FAIL
		// where it cannot establish its result. "No swap was used" has two
		// causes, and only one of them is a finding: either the platform fact
		// changed, or there was no free swap to use. `/proc/swaps` listing a
		// device does not mean the device has room — the shared aira.slice
		// budget is 8 GiB and other sessions draw on it — so distinguish them
		// before deciding which to report.
		if free := readProcMeminfoKiB(t, "SwapFree"); free >= 0 && free < 128*1024 {
			t.Skipf("only %d KiB of swap free machine-wide; the allocation could not have been "+
				"paged out for reasons unrelated to this claim, so it is unevaluated here", free)
		}
		t.Fatalf("a 64 MiB allocation inside an 8 MiB memory.max used NO swap despite free swap "+
			"being available (memory.events: %s). If memory.max alone now contains a runaway on "+
			"this platform, the production memory.swap.max=0 in CreateWorkerScope may no longer "+
			"be needed — re-measure before assuming either way",
			readWorkerScopeFile(t, scopePath, "memory.events"))
	}
	t.Logf("uncapped-swap control: %d bytes of swap used inside an 8 MiB memory.max — "+
		"memory.max bounds memory, not memory+swap", peak)
}

// newWorkerConvergenceOuterScope builds a delegated outer scope with NO
// ancestor swap cap.
//
// The absence is the point. The AIRA-30 e2e harness wrote memory.swap.max=0 on
// its own ancestor, which meant it proved the HARNESS rather than the product:
// it passed identically whether or not production capped worker swap. Nothing
// here pre-empts the production mechanism, so these tests fail if
// CreateWorkerScope stops writing the cap.
func newWorkerConvergenceOuterScope(t *testing.T) string {
	t.Helper()
	parent := cgrouptest.IsolatedScopeParent(t)
	if err := os.WriteFile(filepath.Join(parent, "cgroup.subtree_control"), []byte("+memory"), 0o644); err != nil {
		cgrouptest.SkipOrFailRealCgroup(t, "memory controller not delegated to %s: %v", parent, err)
	}
	outer := filepath.Join(parent, ".aira-outer-test")
	if err := os.Mkdir(outer, 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := ensureConfineDelegation(outer); err != nil {
		cgrouptest.SkipOrFailRealCgroup(t, "cannot delegate outer scope: %v", err)
	}
	return outer
}

// requireUsableSwap skips with its reason unless this host can actually swap
// the control scope's pages: an active device in /proc/swaps, and no ancestor
// swap cap that would pre-empt the very behaviour under observation.
func requireUsableSwap(t *testing.T, scopePath string) {
	t.Helper()
	raw, err := os.ReadFile("/proc/swaps")
	if err != nil {
		t.Skipf("cannot read /proc/swaps (%v); whether this host can swap is unevaluated", err)
	}
	lines := strings.Split(strings.TrimSpace(string(raw)), "\n")
	if len(lines) < 2 {
		t.Skip("no active swap device on this host; 'memory.max does not bound swap' is not observable here")
	}
	if _, err := os.Stat(filepath.Join(scopePath, "memory.swap.max")); err != nil {
		t.Skipf("this kernel exposes no memory.swap.max (%v); the control cannot be set up", err)
	}
	if data, err := os.ReadFile(filepath.Join(scopePath, "memory.swap.max")); err == nil {
		if value := strings.TrimSpace(string(data)); value != "max" {
			t.Skipf("an ancestor already caps swap for this scope (memory.swap.max=%q); the "+
				"platform fact under test is pre-empted here", value)
		}
	}
}

// startWorkerScopeAllocator launches a child DIRECTLY into scopePath via
// clone3(CLONE_INTO_CGROUP) — the same UseCgroupFD mechanism confine_linux.go
// uses — so there is no unplaced window at all, and touches every page so the
// allocation is real resident memory rather than an untouched mapping.
func startWorkerScopeAllocator(t *testing.T, scopePath string, bytes int64) *exec.Cmd {
	t.Helper()
	dir, err := os.Open(scopePath)
	if err != nil {
		t.Fatalf("open worker scope: %v", err)
	}
	t.Cleanup(func() { _ = dir.Close() })
	script := fmt.Sprintf(`
import sys, time
block = bytearray(%d)
for i in range(0, len(block), 4096):
    block[i] = 1
sys.stdout.write("held\n")
sys.stdout.flush()
time.sleep(600)
`, bytes)
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Skipf("python3 unavailable (%v); no allocator to place into the scope", err)
	}
	command := exec.Command(python, "-c", script)
	command.SysProcAttr = &syscall.SysProcAttr{UseCgroupFD: true, CgroupFD: int(dir.Fd())}
	if err := command.Start(); err != nil {
		t.Fatalf("start allocator: %v", err)
	}
	return command
}

func readWorkerScopeOOMGroupKills(t *testing.T, scopePath string) int64 {
	t.Helper()
	for _, line := range strings.Split(readWorkerScopeFile(t, scopePath, "memory.events"), "\n") {
		fields := strings.Fields(line)
		if len(fields) == 2 && fields[0] == "oom_group_kill" {
			value, err := strconv.ParseInt(fields[1], 10, 64)
			if err != nil {
				t.Fatalf("oom_group_kill=%q is not a number: %v", fields[1], err)
			}
			return value
		}
	}
	return 0
}

func readWorkerScopeFile(t *testing.T, scopePath, name string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(scopePath, name))
	if err != nil {
		return "<unreadable: " + err.Error() + ">"
	}
	return strings.TrimSpace(string(data))
}

// readProcMeminfoKiB returns a /proc/meminfo field in KiB, or -1 if it cannot
// be established — the caller then does NOT get to claim anything from it.
func readProcMeminfoKiB(t *testing.T, field string) int64 {
	t.Helper()
	data, err := os.ReadFile("/proc/meminfo")
	if err != nil {
		return -1
	}
	for _, line := range strings.Split(string(data), "\n") {
		fields := strings.Fields(line)
		if len(fields) >= 2 && strings.TrimSuffix(fields[0], ":") == field {
			value, err := strconv.ParseInt(fields[1], 10, 64)
			if err != nil {
				return -1
			}
			return value
		}
	}
	return -1
}

func readWorkerScopeInt(t *testing.T, scopePath, name string) int64 {
	t.Helper()
	value, err := strconv.ParseInt(readWorkerScopeFile(t, scopePath, name), 10, 64)
	if err != nil {
		return 0
	}
	return value
}
