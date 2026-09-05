//go:build linux

package runner

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"aira/internal/cgrouptest"
	"aira/internal/testdeadline"
)

// AIRA-101. Tests for the PRODUCER of ConfineRecord.SubtreePopulated.
//
// The consumer (the exclusive-admission emptiness rule) is tested in
// internal/daemon against a fake scan, which cannot catch a producer that reads
// the wrong file, inverts the sense of `populated`, or regresses to leaf-only.
// That last one is the exact defect the plan gate found in v1: a running aitest
// outer scope drains EVERY pid into <outer>/.aira-supervisor, so it reads
// leaf-EMPTY while fully busy, and a leaf-only reading would declare such a
// slice empty and hand a benchmark a fabricated "you are alone".
//
// So the producer needs its own tests, and they must assert the LEAF and the
// SUBTREE readings disagree — an assertion only on SubtreePopulated would pass
// against an implementation that simply copied Populated.

// The synthetic tree exercises the parse and the sense of the reading
// deterministically. It is the same shape confine-kill's own subtree test uses.
func TestListConfinesReportsSubtreePopulationSeparatelyFromLeafPopulation(t *testing.T) {
	slice := t.TempDir()
	id := confineTestOwnedScopeID("suite", "mark", 7401, time.Now().UnixNano())
	path := filepath.Join(slice, ".aira-"+id)
	if err := os.Mkdir(path, 0o755); err != nil {
		t.Fatal(err)
	}
	// The real aitest layout: the leaf holds no pids because they were all
	// drained into a child scope, while the SUBTREE is populated.
	for name, data := range map[string]string{
		"cgroup.procs": "", "cgroup.events": "populated 1\n",
		"memory.current": "4096\n", "memory.max": "8192\n",
	} {
		if err := os.WriteFile(filepath.Join(path, name), []byte(data), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	result, err := ListConfines(context.Background(), slice, nil)
	if err != nil || len(result.Scopes) != 1 {
		t.Fatalf("list result=%+v err=%v", result, err)
	}
	record := result.Scopes[0]
	if record.Populated == nil || *record.Populated != 0 {
		t.Fatalf("leaf population should read empty for this layout, got %v", record.Populated)
	}
	if record.SubtreePopulated == nil {
		t.Fatal("subtree population was not established; an unevaluated reading must never be mistaken for an empty scope")
	}
	if !*record.SubtreePopulated {
		t.Fatal("a leaf-empty but subtree-populated scope was reported as unpopulated: this is the reading that would tell an exclusive benchmark it was alone while a suite ran")
	}
}

// The opposite direction, so the test above cannot be satisfied by hardcoding
// true. A genuinely empty scope must report SubtreePopulated false, not nil and
// not true — otherwise an exclusive job could never be granted at all.
func TestListConfinesReportsAnEmptySubtreeAsUnpopulated(t *testing.T) {
	slice := t.TempDir()
	id := confineTestOwnedScopeID("idle", "mark", 7402, time.Now().UnixNano())
	path := filepath.Join(slice, ".aira-"+id)
	if err := os.Mkdir(path, 0o755); err != nil {
		t.Fatal(err)
	}
	for name, data := range map[string]string{
		"cgroup.procs": "", "cgroup.events": "populated 0\n",
		"memory.current": "0\n", "memory.max": "8192\n",
	} {
		if err := os.WriteFile(filepath.Join(path, name), []byte(data), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	result, err := ListConfines(context.Background(), slice, nil)
	if err != nil || len(result.Scopes) != 1 {
		t.Fatalf("list result=%+v err=%v", result, err)
	}
	if record := result.Scopes[0]; record.SubtreePopulated == nil || *record.SubtreePopulated {
		t.Fatalf("an empty subtree must read unpopulated, got %v", record.SubtreePopulated)
	}
}

// An unreadable reading must stay nil — never false. The emptiness rule treats
// nil as LIVE, so a fabricated false here would be the fail-open direction on
// exactly the check that must fail closed.
func TestListConfinesLeavesSubtreePopulationUnevaluatedWhenUnreadable(t *testing.T) {
	slice := t.TempDir()
	id := confineTestOwnedScopeID("opaque", "mark", 7403, time.Now().UnixNano())
	path := filepath.Join(slice, ".aira-"+id)
	if err := os.Mkdir(path, 0o755); err != nil {
		t.Fatal(err)
	}
	// No cgroup.events at all: the reading cannot be established.
	for name, data := range map[string]string{
		"cgroup.procs": "", "memory.current": "0\n", "memory.max": "8192\n",
	} {
		if err := os.WriteFile(filepath.Join(path, name), []byte(data), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	result, err := ListConfines(context.Background(), slice, nil)
	if err != nil || len(result.Scopes) != 1 {
		t.Fatalf("list result=%+v err=%v", result, err)
	}
	record := result.Scopes[0]
	if record.SubtreePopulated != nil {
		t.Fatalf("an unreadable subtree population must stay unevaluated, got %v", *record.SubtreePopulated)
	}
	found := false
	for _, field := range record.UnevaluatedFields {
		if field == "subtree_populated" {
			found = true
		}
	}
	if !found {
		t.Fatalf("an unestablished reading must be NAMED unevaluated, fields=%v", record.UnevaluatedFields)
	}
}

// The same property against a REAL kernel, with a real process in a real child
// cgroup — the aitest layout for actual rather than by construction. This is
// what proves the cgroup.events semantics are what the synthetic tests assume.
func TestListConfinesRealScopeReportsSubtreePopulationForANestedWorkload(t *testing.T) {
	parent := cgrouptest.IsolatedScopeParent(t)
	if _, finite := effectiveConfineCap(parent); !finite {
		cgrouptest.SkipOrFailRealCgroup(t, "requires a capped cgroup ancestor (run under aira confine)")
	}
	backend := newDefaultBackend(parent)
	if err := backend.Probe(context.Background()); err != nil {
		cgrouptest.SkipOrFailRealCgroup(t, "real cgroup backend unavailable: %v", err)
	}
	id := confineTestScopeID("nested-suite", os.Getpid(), time.Now().UnixNano())
	scope, err := backend.Create(context.Background(), id)
	if err != nil {
		cgrouptest.SkipOrFailRealCgroup(t, "real scope unavailable: %v", err)
	}
	defer func() { _ = scope.Kill(); _ = waitEmpty(context.Background(), scope, time.Second); _ = scope.Remove() }()

	// A child cgroup inside the scope, then the workload placed INTO it — so the
	// outer scope's own leaf cgroup.procs stays empty, exactly as
	// BootstrapAitestSupervisor leaves a running aitest job.
	child := filepath.Join(scope.Reference(), ".aira-supervisor")
	if err := os.Mkdir(child, 0o755); err != nil {
		cgrouptest.SkipOrFailRealCgroup(t, "nested cgroup unavailable: %v", err)
	}
	childFD, err := syscall.Open(child, syscall.O_RDONLY|syscall.O_DIRECTORY|syscall.O_CLOEXEC, 0)
	if err != nil {
		cgrouptest.SkipOrFailRealCgroup(t, "nested cgroup fd unavailable: %v", err)
	}
	defer syscall.Close(childFD)

	command := exec.Command("/bin/sh", "-c", "sleep 60")
	command.SysProcAttr = &syscall.SysProcAttr{UseCgroupFD: true, CgroupFD: childFD}
	if err := command.Start(); err != nil {
		cgrouptest.SkipOrFailRealCgroup(t, "start nested workload: %v", err)
	}
	defer func() { _ = command.Process.Kill(); _, _ = command.Process.Wait() }()

	deadline := time.Now().Add(testdeadline.Wait(2 * time.Second))
	for {
		result, listErr := ListConfines(context.Background(), parent, nil)
		if listErr == nil {
			for _, record := range result.Scopes {
				if record.ScopeID != id {
					continue
				}
				// The LEAF must read empty and the SUBTREE populated. Asserting both
				// is what makes this a real regression test: an implementation that
				// copied Populated into SubtreePopulated would satisfy neither.
				if record.Populated != nil && *record.Populated == 0 &&
					record.SubtreePopulated != nil && *record.SubtreePopulated {
					return
				}
				if time.Now().After(deadline) {
					t.Fatalf("real nested workload: leaf=%v subtree=%v — a leaf-only reading would report this busy scope as an empty slice",
						record.Populated, record.SubtreePopulated)
				}
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("scope %s never appeared in the scan (err=%v)", id, listErr)
		}
		time.Sleep(20 * time.Millisecond)
	}
}
