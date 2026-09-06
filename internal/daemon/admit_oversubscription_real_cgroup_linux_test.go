//go:build linux

package daemon

import (
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"aira/internal/cgrouptest"
)

// AIRA-114 anti-INERT tier.
//
// Every other AIRA-114 test drives evaluateAdmitQueue through the
// admitConfineScan SEAM, so all of them would keep passing against a build
// whose PRODUCTION scan can never establish a cap or a subtree-liveness reading
// on a real cgroup tree. That is exactly how the AIRA-59 watchdog shipped
// inert: correct arithmetic that never fired.
//
// So this one builds a real cgroup-v2 slice, gives two real .aira-CONFINE-*
// scopes real memory.max values whose sum exceeds the bound, and — the part
// that matters most — DRAINS every process out of each scope's own
// cgroup.procs into a child cgroup, exactly as BootstrapAitestSupervisor and
// `podman run --cgroups=split` do. Those scopes therefore read leaf-EMPTY to
// the kernel while being fully alive, which is the precise condition that made
// v4's adoption-derived version unbuildable.
//
// It then runs the production runner.ListConfines and readSliceMemory and
// asserts the aggregate equals the kernel's OWN two memory.max values, and that
// the bound refuses a job the reserve ledger had ample room for.
//
// verifies: aggregateScopeCap and scopeRecordIsLive against a real cgroup tree.

// drainedScopeWithCap creates a real confine scope, writes a real memory.max on
// it, and parks a live process in a CHILD cgroup so the scope's own
// cgroup.procs is empty while cgroup.events reports the subtree populated.
func drainedScopeWithCap(t *testing.T, slice, name string, capBytes int64) string {
	t.Helper()
	scopePath, scopeID := realConfineScope(t, slice, name)
	if err := os.WriteFile(filepath.Join(scopePath, "memory.max"), []byte(strconv.FormatInt(capBytes, 10)), 0o644); err != nil {
		cgrouptest.SkipOrFailRealCgroup(t, "cannot set memory.max on %s: %v", scopePath, err)
	}
	child := filepath.Join(scopePath, ".aira-supervisor")
	if err := os.Mkdir(child, 0o755); err != nil {
		cgrouptest.SkipOrFailRealCgroup(t, "cannot create the drain child %s: %v", child, err)
	}
	worker := exec.Command("sh", "-c", "sleep 300")
	if err := worker.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = worker.Process.Kill()
		_, _ = worker.Process.Wait()
	})
	if err := os.WriteFile(filepath.Join(child, "cgroup.procs"),
		[]byte(strconv.Itoa(worker.Process.Pid)), 0o644); err != nil {
		cgrouptest.SkipOrFailRealCgroup(t, "cannot place a process into %s: %v", child, err)
	}
	// Establish the trap this test exists for, rather than assuming it: the
	// scope's OWN cgroup.procs must be empty while the kernel reports the
	// subtree populated. If the drain did not take, the test would prove the
	// weaker leaf-visible case and say nothing about the real one.
	if own, err := os.ReadFile(filepath.Join(scopePath, "cgroup.procs")); err != nil {
		cgrouptest.SkipOrFailRealCgroup(t, "cannot read %s/cgroup.procs: %v", scopePath, err)
	} else if len(own) != 0 {
		cgrouptest.SkipOrFailRealCgroup(t, "scope %s still holds processes in its own cgroup.procs; the drain did not take", scopePath)
	}
	return scopeID
}

func TestRealCgroupAggregateBoundCountsDrainedScopesAndRefusesAJob(t *testing.T) {
	const (
		sliceMax = 1024 << 20
		scopeCap = 600 << 20 // two of these total 1200 MiB, past a 1x bound
		newcomer = 64 << 20
	)

	slice := cgrouptest.IsolatedScopeParent(t)
	if err := os.WriteFile(filepath.Join(slice, "cgroup.subtree_control"), []byte("+memory"), 0o644); err != nil {
		cgrouptest.SkipOrFailRealCgroup(t, "memory controller not delegated to %s: %v", slice, err)
	}
	if err := os.WriteFile(filepath.Join(slice, "memory.max"), []byte(strconv.Itoa(sliceMax)), 0o644); err != nil {
		cgrouptest.SkipOrFailRealCgroup(t, "cannot set memory.max on %s: %v", slice, err)
	}
	first := drainedScopeWithCap(t, slice, "aggone", scopeCap)
	second := drainedScopeWithCap(t, slice, "aggtwo", scopeCap)

	now := time.Unix(2_000_000, 0)
	build := func(factorPct int64) (*Server, *sliceQueue, *admitWaiter) {
		server := NewServer(Paths{})
		server.admitNow = func() time.Time { return now }
		server.admitConfineScanInterval = time.Nanosecond
		server.admitSliceHeadroomBase = 0
		server.admitSliceHeadroomSupervisor = 0
		server.oversubscriptionFactorPct = factorPct
		// admitConfineScan and admitReadMemory stay at their PRODUCTION defaults
		// (runner.ListConfines and readSliceMemory). Replacing either would put
		// this test back in the seam tier it exists to escape.
		waiter := queuedScopeWaiter(1, "CONFINE-newcomer-1-a", newcomer, now)
		queue := &sliceQueue{path: slice, server: server, waiters: []*admitWaiter{waiter}}
		return server, queue, waiter
	}

	if _, _, _, ok, reason := readSliceMemory(slice); !ok {
		cgrouptest.SkipOrFailRealCgroup(t, "production readSliceMemory cannot read %s: %s", slice, reason)
	}

	// Control arm FIRST, so "the bound refused it" below is a statement about
	// the bound and not about a slice that was full anyway.
	unbounded, unboundedQueue, unboundedWaiter := build(0)
	unbounded.evaluateAdmitQueue(unboundedQueue)
	if unboundedWaiter.state != admitGranted {
		t.Fatalf("with the bound disabled the newcomer must be admitted; it was not (state=%v, outstanding=%d)",
			unboundedWaiter.state, unboundedQueue.outstanding)
	}

	// 1x the slice ceiling: the two 600 MiB caps already exceed it.
	server, queue, waiter := build(100)
	server.evaluateAdmitQueue(queue)

	if !queue.capAggregateKnown {
		t.Fatalf("the production scan never established a cap aggregate for %s and %s; the mechanism is INERT against a real tree", first, second)
	}
	kernelFirst, okFirst := readChargeCgroupInt(filepath.Join(slice, ".aira-"+first), "memory.max")
	kernelSecond, okSecond := readChargeCgroupInt(filepath.Join(slice, ".aira-"+second), "memory.max")
	if !okFirst || !okSecond {
		t.Fatal("could not read the scopes' memory.max back")
	}
	if want := kernelFirst + kernelSecond; queue.capAggregate != want {
		t.Fatalf("cap aggregate = %d, want exactly the kernel's own %d + %d = %d -- two LEAF-EMPTY but subtree-live scopes must both be counted",
			queue.capAggregate, kernelFirst, kernelSecond, want)
	}
	if waiter.state != admitQueued {
		t.Fatalf("the newcomer was admitted (state=%v); %d of scope caps already exceeds the %d bound",
			waiter.state, queue.capAggregate, int64(sliceMax))
	}
}
