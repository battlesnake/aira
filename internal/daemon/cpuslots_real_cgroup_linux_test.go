//go:build linux

package daemon

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"aira/internal/cgrouptest"
	"aira/internal/runner"
)

// AIRA-64 anti-INERT tier.
//
// These are the most important tests in this change. Every other CPU-gate test
// runs against a seam, so all of them would keep passing if the real scan could
// never establish a reading on a real host — which is exactly how the AIRA-59
// watchdog shipped INERT: correct arithmetic, and it never fired. These run the
// production scan and the production population reading against a real cgroup
// tree.
//
// This tier has already earned its place twice. It refuted the plan's original
// directory-mtime claim on its first run, and the salvaged version of that
// argument was then refuted by this same file's own ability to create a child
// cgroup inside a scope it is not a member of. The age gate is gone as a result;
// the placement window is closed by daemon-owned state instead.

// realSliceWithConfineScopes builds <parent>/.aira-CONFINE-<name> scopes shaped
// the way `aira confine` leaves them, so cpuSlotsScanRoot accepts them and the
// production scan walks a real tree.
func realSliceWithConfineScopes(t *testing.T, names ...string) (slice string, scopes []string) {
	t.Helper()
	slice = cgrouptest.IsolatedScopeParent(t)
	if err := os.WriteFile(filepath.Join(slice, "cgroup.subtree_control"), []byte("+memory"), 0o644); err != nil {
		cgrouptest.SkipOrFailRealCgroup(t, "memory controller not delegated to %s: %v", slice, err)
	}
	for _, name := range names {
		scope := filepath.Join(slice, ".aira-CONFINE-"+name)
		if err := os.Mkdir(scope, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(scope, "cgroup.subtree_control"), []byte("+memory"), 0o644); err != nil {
			cgrouptest.SkipOrFailRealCgroup(t, "cannot delegate memory into %s: %v", scope, err)
		}
		scopes = append(scopes, scope)
	}
	return slice, scopes
}

// populateScope puts a real process into scopePath and returns once the kernel
// reports the cgroup populated, so a test never races its own setup.
func populateScope(t *testing.T, scopePath string) {
	t.Helper()
	sleeper := exec.Command("sleep", "120")
	if err := sleeper.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sleeper.Process.Kill(); _, _ = sleeper.Process.Wait() })
	if err := os.WriteFile(filepath.Join(scopePath, "cgroup.procs"),
		[]byte(strconv.Itoa(sleeper.Process.Pid)), 0o644); err != nil {
		cgrouptest.SkipOrFailRealCgroup(t, "cannot place a process into %s: %v", scopePath, err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for !workerScopeLiveForFloor(scopePath) {
		if time.Now().After(deadline) {
			t.Fatalf("%s never reported populated after a real process joined it", scopePath)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// verifies: AIRA-64 §9.17 — THE ANTI-INERT TEST. Against a real slice holding
// real, really-populated worker cgroups, and with RAM deliberately wide open,
// the gate actually denies at capacity with the exact reason the client acts
// on. A gate that silently reported `unevaluated` on every real host would pass
// every seam test in this package and fail this one.
func TestCPUGateFiresAgainstARealCgroupTree(t *testing.T) {
	slice, scopes := realSliceWithConfineScopes(t, "busy", "newcomer")
	busy, newcomer := scopes[0], scopes[1]

	const capacity = 2
	for i := 1; i <= capacity; i++ {
		scope, err := runner.CreateWorkerScope(context.Background(), busy, strconv.Itoa(i), 1<<20, 1<<19)
		if err != nil {
			cgrouptest.SkipOrFailRealCgroup(t, "create worker scope: %v", err)
		}
		populateScope(t, scope)
	}

	// The production scan must see the real tree before anything else is
	// asserted: otherwise a later "denied" could come from an unrelated cause
	// and this test would certify an inert gate.
	snapshot, err := scanSliceWorkerScopes(slice)
	if err != nil {
		t.Fatalf("the production scan could not read a real slice — the gate would be INERT here: %v", err)
	}
	if snapshot.total != capacity {
		t.Fatalf("real scan saw %d worker scopes, want %d (slice=%s)", snapshot.total, capacity, slice)
	}
	if snapshot.liveForFloor[busy] != capacity {
		t.Fatalf("really-populated worker scopes must count as live for the floor, got %d",
			snapshot.liveForFloor[busy])
	}
	if snapshot.liveForFloor[newcomer] != 0 {
		t.Fatalf("a scope with no workers must have no live count, got %d", snapshot.liveForFloor[newcomer])
	}

	server := NewServer(Paths{})
	server.workerAdmitHeadroom = 0
	server.cpuSlotsCapacity = capacity
	server.cpuSlotsScanInterval = time.Nanosecond
	// RAM wide open, so any denial below is unambiguously the CPU gate's.
	server.admitReadMemory = func(string) (int64, int64, int64, bool, string) { return 0, 1 << 40, 0, true, "" }
	server.admitReadWorkerSupervisorMemory = func(string) (int64, int64, bool, string) { return 0, 0, true, "" }

	denied, _ := server.evaluateWorkerAdmit(context.Background(), workerAdmitRequest{
		jobID: "busy", outerScope: busy, estimatedBytes: 1 << 20, maxWaitMS: 0,
	})
	if denied.State != runner.WorkerAdmitStateDenied ||
		denied.Class != runner.WorkerAdmitClassContended ||
		denied.Reason != runner.WorkerAdmitReasonCPUSlotsSaturated {
		t.Fatalf("at capacity on a REAL tree with ample RAM the gate must deny "+
			"denied/contended/cpu-slots-saturated, got %+v", denied)
	}

	// And the floor must still hold on that same real tree, or this change
	// converts "slow" into "stalled" for every arriving job.
	granted, _ := server.evaluateWorkerAdmit(context.Background(), workerAdmitRequest{
		jobID: "newcomer", outerScope: newcomer, estimatedBytes: 1 << 20, maxWaitMS: 0,
	})
	if granted.State != runner.WorkerAdmitStateGranted {
		t.Fatalf("a scope with no worker must always get its floor worker, got %+v", granted)
	}
	if granted.CPUSlots != runner.WorkerAdmitCPUSlotsOK {
		t.Fatalf("a governed grant must report cpu_slots=ok, got %q", granted.CPUSlots)
	}

	// The floor is spent: the same scope must NOT take a second floor grant
	// inside one grace window, even though its brand-new scope is still
	// unpopulated. This is the grant-to-placement window, closed by
	// lastGrantAt rather than by any filesystem timestamp — verified here on a
	// real tree rather than only through the seam.
	again, _ := server.evaluateWorkerAdmit(context.Background(), workerAdmitRequest{
		jobID: "newcomer", outerScope: newcomer, estimatedBytes: 1 << 20, maxWaitMS: 0,
	})
	if again.State != runner.WorkerAdmitStateDenied || again.Reason != runner.WorkerAdmitReasonCPUSlotsSaturated {
		t.Fatalf("a just-granted, not-yet-placed scope must not re-claim the floor, got %+v", again)
	}
}

// verifies: AIRA-64 §4.4 — the population reading is REAL on a real cgroup, in
// both directions. Without the second half this would pass against a predicate
// that always returns true, which is precisely the direction that silently
// withholds a job's floor worker forever.
func TestWorkerScopeLiveForFloorTracksRealPopulation(t *testing.T) {
	_, scopes := realSliceWithConfineScopes(t, "pop")
	scope, err := runner.CreateWorkerScope(context.Background(), scopes[0], "1", 1<<20, 1<<19)
	if err != nil {
		cgrouptest.SkipOrFailRealCgroup(t, "create worker scope: %v", err)
	}
	if workerScopeLiveForFloor(scope) {
		t.Fatal("a freshly created, empty worker scope must not read as populated")
	}

	sleeper := exec.Command("sleep", "120")
	if err := sleeper.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = sleeper.Process.Kill(); _, _ = sleeper.Process.Wait() }()
	if err := os.WriteFile(filepath.Join(scope, "cgroup.procs"),
		[]byte(strconv.Itoa(sleeper.Process.Pid)), 0o644); err != nil {
		cgrouptest.SkipOrFailRealCgroup(t, "cannot place a process into %s: %v", scope, err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for !workerScopeLiveForFloor(scope) {
		if time.Now().After(deadline) {
			t.Fatal("a scope holding a live process must read as populated")
		}
		time.Sleep(10 * time.Millisecond)
	}

	_ = sleeper.Process.Kill()
	_, _ = sleeper.Process.Wait()
	deadline = time.Now().Add(5 * time.Second)
	for workerScopeLiveForFloor(scope) {
		if time.Now().After(deadline) {
			t.Fatal("an emptied worker scope must stop reading as populated, " +
				"or an orphan withholds this job's floor worker forever")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// verifies: AIRA-64 §9.19 — the guarantee is PER-SLICE, and that limit is
// pinned so it cannot change silently. Two outer scopes under different slices
// are bounded separately; a deployment with one `aira.slice` (what `aira
// install` bakes) therefore gets a machine-wide bound, and a multi-slice one
// does not.
func TestCPUGateBoundsEachSliceSeparately(t *testing.T) {
	_, first := realSliceWithConfineScopes(t, "one")
	_, second := realSliceWithConfineScopes(t, "two")
	for i := 1; i <= 2; i++ {
		if _, err := runner.CreateWorkerScope(context.Background(), first[0], strconv.Itoa(i), 1<<20, 1<<19); err != nil {
			cgrouptest.SkipOrFailRealCgroup(t, "create worker scope: %v", err)
		}
		if _, err := runner.CreateWorkerScope(context.Background(), second[0], strconv.Itoa(i), 1<<20, 1<<19); err != nil {
			cgrouptest.SkipOrFailRealCgroup(t, "create worker scope: %v", err)
		}
	}
	for _, scope := range []string{first[0], second[0]} {
		root, _, ok := cpuSlotsScanRoot(scope)
		if !ok {
			t.Fatalf("cpuSlotsScanRoot rejected a real confine scope %s", scope)
		}
		snapshot, err := scanSliceWorkerScopes(root)
		if err != nil {
			t.Fatal(err)
		}
		if snapshot.total != 2 {
			t.Fatalf("slice %s: got %d workers, want 2 — a slice must not see another slice's workers "+
				"(this is the documented per-slice limit, not a machine-wide count)", root, snapshot.total)
		}
	}
}

// verifies: AIRA-64 §4.8 — the derived scan root resolves through the real
// admission-slice resolver against a real cgroup path, and a path outside the
// cgroup mount does not. This is what stops a caller naming
// `/anywhere/.aira-CONFINE-x` from having the gate count an unrelated directory
// (and therefore admit freely).
func TestCPUSlotsScanRootResolvesOnlyRealCgroupSlices(t *testing.T) {
	_, scopes := realSliceWithConfineScopes(t, "resolve")
	root, _, ok := cpuSlotsScanRoot(scopes[0])
	if !ok {
		t.Fatalf("cpuSlotsScanRoot rejected a real confine scope %s", scopes[0])
	}
	if _, resolved, reason := resolveAdmitSlicePath(root); !resolved {
		t.Fatalf("a real cgroup slice must resolve, got reason %q for %s", reason, root)
	}
	outside := filepath.Join(t.TempDir(), "not-a-cgroup")
	if err := os.MkdirAll(outside, 0o755); err != nil {
		t.Fatal(err)
	}
	if _, resolved, _ := resolveAdmitSlicePath(outside); resolved {
		t.Fatalf("%s is outside the cgroup2 mount and must not resolve as a slice", outside)
	}
}
