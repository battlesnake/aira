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
// production scan against a real cgroup tree.

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

// verifies: AIRA-64 §9.17 — THE ANTI-INERT TEST. Against a real slice holding
// real worker cgroups, and with RAM deliberately wide open, the gate actually
// denies at capacity with the exact reason the client acts on. A gate that
// silently reported `unevaluated` on every real host would pass every seam test
// in this package and fail this one.
func TestCPUGateFiresAgainstARealCgroupTree(t *testing.T) {
	slice, scopes := realSliceWithConfineScopes(t, "busy", "newcomer")
	busy, newcomer := scopes[0], scopes[1]

	const capacity = 2
	for i := 1; i <= capacity; i++ {
		if _, err := runner.CreateWorkerScope(context.Background(), busy, strconv.Itoa(i), 1<<20, 1<<19); err != nil {
			cgrouptest.SkipOrFailRealCgroup(t, "create worker scope: %v", err)
		}
	}

	// The scan must see the real tree before anything else is asserted:
	// otherwise a later "denied" could come from an unrelated cause and this
	// test would certify an inert gate.
	snapshot, err := scanSliceWorkerScopes(slice, time.Now(), cpuSlotsPlacementGraceDefault)
	if err != nil {
		t.Fatalf("the production scan could not read a real slice — the gate would be INERT here: %v", err)
	}
	if snapshot.total != capacity {
		t.Fatalf("real scan saw %d worker scopes, want %d (slice=%s)", snapshot.total, capacity, slice)
	}
	if snapshot.liveForFloor[busy] != capacity {
		t.Fatalf("freshly created worker scopes must count as live for the floor, got %d", snapshot.liveForFloor[busy])
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
}

// verifies: AIRA-64 §4.4.1, §9.18 — the exact cgroupfs mtime semantics the age
// gate depends on, measured rather than assumed.
//
// THIS TEST ALREADY EARNED ITS PLACE. The plan's first wording claimed a worker
// directory's mtime "is its creation time", and this test refuted that on the
// first run: a child-cgroup mkdir or rmdir inside the scope DOES move it. The
// mechanism survives, but only because the movement is one-directional in the
// safe sense, and that is what is pinned here rather than the false claim:
//
//   - population (a `cgroup.procs` write) does NOT move mtime;
//   - a control-file write does NOT move it;
//   - a child-cgroup mkdir/rmdir DOES.
//
// The age gate needs exactly one property, and these three give it: an
// ABANDONED scope's mtime is frozen. An abandoned scope has no process in it, so
// nothing can create or remove a child cgroup inside it, so it genuinely ages
// out and the liveness floor opens. A LIVE worker that nests cgroups only ever
// refreshes its own mtime, i.e. looks younger — and it is in fact live, so being
// counted live is correct.
func TestWorkerScopeMtimeMovesOnlyOnDirectoryEntryChanges(t *testing.T) {
	_, scopes := realSliceWithConfineScopes(t, "mtime")
	scope, err := runner.CreateWorkerScope(context.Background(), scopes[0], "1", 1<<20, 1<<19)
	if err != nil {
		cgrouptest.SkipOrFailRealCgroup(t, "create worker scope: %v", err)
	}
	mtime := func() time.Time {
		info, err := os.Stat(scope)
		if err != nil {
			t.Fatal(err)
		}
		return info.ModTime()
	}
	created := mtime()
	if age := time.Since(created); age < 0 || age > time.Minute {
		t.Fatalf("a just-created worker scope's mtime must read as ~now, got age %v", age)
	}

	sleeper := exec.Command("sleep", "60")
	if err := sleeper.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = sleeper.Process.Kill(); _, _ = sleeper.Process.Wait() }()
	time.Sleep(20 * time.Millisecond)
	if err := os.WriteFile(filepath.Join(scope, "cgroup.procs"), []byte(strconv.Itoa(sleeper.Process.Pid)), 0o644); err != nil {
		cgrouptest.SkipOrFailRealCgroup(t, "cannot place a process into %s: %v", scope, err)
	}
	if !mtime().Equal(created) {
		t.Fatalf("population moved mtime (%v -> %v); an abandoned scope's mtime would no longer be "+
			"frozen and the liveness floor could be withheld", created, mtime())
	}
	time.Sleep(20 * time.Millisecond)
	if err := os.WriteFile(filepath.Join(scope, "memory.high"), []byte("max"), 0o644); err != nil {
		cgrouptest.SkipOrFailRealCgroup(t, "cannot write memory.high: %v", err)
	}
	if !mtime().Equal(created) {
		t.Fatalf("a control-file write moved mtime (%v -> %v)", created, mtime())
	}

	// A child cgroup DOES move it. Pinned deliberately, as the documented and
	// safe-direction-only exception: it can only make a live scope look
	// younger, never make an abandoned one look older.
	time.Sleep(20 * time.Millisecond)
	if err := os.Mkdir(filepath.Join(scope, "child"), 0o755); err == nil {
		defer func() { _ = os.Remove(filepath.Join(scope, "child")) }()
		if mtime().Equal(created) {
			t.Fatal("expected a child mkdir to refresh mtime; if that changed, §4.4.1's " +
				"safe-direction argument needs re-deriving rather than silently holding")
		}
	}

	// A populated scope is live for the floor at ANY age.
	if !workerScopeLiveForFloor(scope, time.Now().Add(24*time.Hour), time.Second) {
		t.Fatal("a populated worker scope must count as live for the floor at any age")
	}
	// And that reading must be REAL, not a predicate that always says true:
	// once the process is gone the same aged scope must stop counting.
	_ = sleeper.Process.Kill()
	_, _ = sleeper.Process.Wait()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if !workerScopeLiveForFloor(scope, time.Now().Add(24*time.Hour), time.Second) {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("an aged, now-empty worker scope must stop counting as live, or an orphan withholds the floor forever")
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
	now := time.Now()
	for _, scope := range []string{first[0], second[0]} {
		root, ok := cpuSlotsScanRoot(scope)
		if !ok {
			t.Fatalf("cpuSlotsScanRoot rejected a real confine scope %s", scope)
		}
		snapshot, err := scanSliceWorkerScopes(root, now, cpuSlotsPlacementGraceDefault)
		if err != nil {
			t.Fatal(err)
		}
		if snapshot.total != 2 {
			t.Fatalf("slice %s: got %d workers, want 2 — a slice must not see another slice's workers "+
				"(this is the documented per-slice limit, not a machine-wide count)", root, snapshot.total)
		}
	}
}
