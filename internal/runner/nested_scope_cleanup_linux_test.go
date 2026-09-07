//go:build linux

package runner

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"syscall"
	"testing"
	"time"

	"aira/internal/cgrouptest"
	"aira/internal/testdeadline"
)

// AIRA-144 — the cgroup DIRECTORIES a nested workload created inside its own run
// scope survived the run.
//
// Scope.Remove() proved the subtree held no PROCESS (cgroup.events `populated`
// is subtree-aware) and then did a single rmdir of the scope itself. rmdir
// refuses a cgroup that still has a child cgroup, even an empty one, and
// cgroup.kill removes processes rather than directories — so every nested job
// left its whole tree on the slice, and the discarded error at each call site
// made that silent. AIRA-140's Fable review saw it on a real dogfood run
// (`.aira-RUN-8` and `.aira-RUN-8/.aira-nested`, both `populated 0`, rmdir'd by
// hand); the same thing already happened on the NORMAL-exit path, which is why
// the fix is in Remove() itself rather than at executeScopeKill's call site.
//
// The tests below cover the two production paths that call Remove() against a
// real kernel, the deepest-first property in isolation, and the direction the
// fix must NOT drift in:
//
//   - T1 the kill path (the ticket's trigger): a timeout ends a job whose
//     processes live two cgroups down, and nothing of the tree is left;
//   - T2 the normal-exit path (the same root cause, older than AIRA-140);
//   - T3 the walk is post-order, so a child that has children of its own is
//     removed rather than failing its rmdir — and non-directory entries (a
//     cgroup's interface files) are never touched;
//   - T4 the anti-over-correction: Remove() now DELETES directories, so its
//     emptiness gate has become load-bearing — a live job's child cgroups must
//     survive a refused teardown.
//
// verifies: AIRA-144

// nestedCgroupPayload builds, inside the run's OWN scope, a two-level cgroup
// tree and migrates the payload into the DEEPEST node before running tail.
//
// Two levels, not one: a depth-1 sweep would remove `.aira-nested` only if it
// were already empty, so a single level cannot distinguish a post-order walk
// from a flat one. The process is placed in the deepest node so the
// intermediate cgroup is a pure directory — exactly the entry an rmdir of the
// scope trips over.
//
// It exits 9 rather than running tail if the shape cannot be built, so an
// environment that cannot nest is reported as unavailable instead of quietly
// degrading into an ordinary un-nested run that would pass either way.
func nestedCgroupPayload(tail string) string {
	return `
set -e
root=/sys/fs/cgroup$(awk -F: '$1=="0"{print $3}' /proc/self/cgroup)
mkdir -p "$root/.aira-nested/.aira-deeper" || exit 9
echo $$ > "$root/.aira-nested/.aira-deeper/cgroup.procs" || exit 9
grep -q '^0::.*\.aira-deeper$' /proc/self/cgroup || exit 9
` + tail + `
`
}

// T1. THE TICKET'S TRIGGER. A `--timeout` kill against a job living two cgroups
// below its own run scope must leave NOTHING of that tree on the slice.
//
// verifies: AIRA-144
func TestAIRA144NestedKillRemovesTheChildCgroupTree(t *testing.T) {
	r := realRunner(t)
	record, err := r.Launch(context.Background(), Request{
		Argv:    []string{"/bin/sh", "-c", nestedCgroupPayload("exec sleep 30")},
		Timeout: testdeadline.Wait(300 * time.Millisecond),
	})
	if err != nil {
		t.Fatalf("nested-cgroup launch error=%v record=%+v", err, record)
	}
	if record.ExitCode != nil && *record.ExitCode == 9 {
		skipOrFailRealCgroup(t, "this environment cannot nest a child cgroup inside a run scope")
	}
	// The kill itself is AIRA-140's guarantee, and it is the precondition for
	// this ticket's cleanup: Remove() is only reached on a completed kill. Assert
	// it so a regression THERE cannot masquerade as a cleanup failure here.
	if record.Status != StatusKilled || !record.ScopeKill.Started || !record.ScopeKill.Completed {
		t.Fatalf("the nested job was not killed, so the cleanup this test covers was never reached: %+v", record)
	}
	assertRunScopeTreeGone(t, record.CgroupScope)
}

// T2. THE OLDER, WIDER CASE. The same leftover tree on the NORMAL-exit path,
// which predates AIRA-140 entirely — the ticket's "same root cause" note. This
// is why the fix lives in Remove() rather than at executeScopeKill.
//
// verifies: AIRA-144
func TestAIRA144NestedNormalExitRemovesTheChildCgroupTree(t *testing.T) {
	r := realRunner(t)
	record, err := r.Launch(context.Background(), Request{
		Argv: []string{"/bin/sh", "-c", nestedCgroupPayload("exit 0")},
	})
	if err != nil {
		t.Fatalf("nested-cgroup launch error=%v record=%+v", err, record)
	}
	if record.ExitCode != nil && *record.ExitCode == 9 {
		skipOrFailRealCgroup(t, "this environment cannot nest a child cgroup inside a run scope")
	}
	if record.Status != StatusExited || record.ExitCode == nil || *record.ExitCode != 0 {
		t.Fatalf("the nested job did not exit normally, so the cleanup this test covers was never reached: %+v", record)
	}
	assertRunScopeTreeGone(t, record.CgroupScope)
}

// assertRunScopeTreeGone reads the real filesystem back: the run's own scope
// directory must no longer exist, and — if it somehow does — no cgroup
// directory may remain inside it. Both are stated because they fail differently
// and the second names the culprit: with the defect present the scope survives
// BECAUSE `.aira-nested` is still inside it, and the message says so rather than
// reporting a bare "still there".
//
// Every Remove() on both paths under test runs synchronously inside Launch, so
// there is nothing to wait for; a poll here would only be able to hide a late
// removal, never to create one.
func assertRunScopeTreeGone(t *testing.T, scopePath string) {
	t.Helper()
	if scopePath == "" {
		t.Fatal("the record carries no cgroup scope path, so the cleanup cannot be checked at all")
	}
	if _, err := os.Stat(scopePath); os.IsNotExist(err) {
		return // Nothing can remain under a directory that is itself gone.
	} else if err != nil {
		t.Fatalf("cannot stat the run scope %s: %v", scopePath, err)
	}
	t.Errorf("the run scope survived its own teardown: %s", scopePath)
	for _, name := range childCgroupNames(t, scopePath) {
		t.Errorf("  leftover child cgroup: %s (rmdir of the scope fails EBUSY while this exists)", filepath.Join(scopePath, name))
	}
}

func childCgroupNames(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Errorf("cannot list %s: %v", dir, err)
		return nil
	}
	var names []string
	for _, entry := range entries {
		if entry.IsDir() {
			names = append(names, entry.Name())
		}
	}
	sort.Strings(names)
	return names
}

// T3. THE PROPERTY, IN ISOLATION. removeChildCgroups must be POST-ORDER: a
// depth-1 sweep fails its rmdir on `a` (ENOTEMPTY, because `a/b` is still
// there) and would report an error instead of an empty scope. It must also
// leave non-directory entries alone — a real cgroup directory is mostly
// interface files, and unlinking those is not this walk's business.
//
// This runs on an ordinary filesystem rather than cgroupfs, so it is not gated
// on real-cgroup delegation: the walk under test is plain openat/unlinkat over
// directory entries, and rmdir's refusal of a non-empty directory is the same
// kernel rule that makes the production case fail.
//
// verifies: AIRA-144
func TestAIRA144RemoveChildCgroupsIsPostOrderAndLeavesInterfaceFiles(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "a", "b", "c"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(root, "flat"), 0o755); err != nil {
		t.Fatal(err)
	}
	// Stand-ins for a cgroup's interface files, at the level the walk sweeps.
	for _, name := range []string{"cgroup.procs", "cgroup.events"} {
		if err := os.WriteFile(filepath.Join(root, name), []byte("populated 0\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	fd, err := os.OpenFile(root, os.O_RDONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer fd.Close()
	scope := &linuxScope{path: root, fd: fd}

	if err := scope.removeChildCgroups(); err != nil {
		t.Fatalf("removeChildCgroups failed on a nested tree: %v — a single-level sweep cannot rmdir a child that has children of its own", err)
	}

	if names := childCgroupNames(t, root); len(names) != 0 {
		t.Fatalf("child cgroup directories survived: %v", names)
	}
	for _, name := range []string{"cgroup.procs", "cgroup.events"} {
		if _, err := os.Stat(filepath.Join(root, name)); err != nil {
			t.Errorf("the walk removed a non-directory entry %s: %v", name, err)
		}
	}
	// The scope's own directory is Remove()'s business, not the walk's.
	if _, err := os.Stat(root); err != nil {
		t.Fatalf("removeChildCgroups removed the scope itself: %v", err)
	}
	// And the scope's fd must survive, because Remove() closes it afterwards and
	// a failed walk must leave the scope usable.
	if _, err := fd.Stat(); err != nil {
		t.Fatalf("removeChildCgroups closed the scope's own directory fd: %v", err)
	}
}

// T4. THE ANTI-OVER-CORRECTION, AGAINST A REAL KERNEL. Remove() used to be a
// single rmdir, which the kernel itself refuses on a live cgroup, so its
// Empty() gate was belt-and-braces. It now DELETES directories first, which the
// kernel will happily do while the scope around them is busy — so the gate has
// become load-bearing and must be shown to be.
//
// The shape is the one that isolates it, and it is not contrived: a scope with a
// live process in its own leaf and an EMPTY child cgroup beside it is aitest's
// outer scope with a worker cgroup created but not yet populated — the exact
// mid-launch window ReapScopeIfEmpty's doc comment warns about. Every conjunct
// here is the kernel's to refuse EXCEPT the child directory: without the gate,
// removeChildCgroups rmdirs that worker cgroup out from under a running job,
// AND the fd close that follows leaves the scope unusable for the retry.
//
// verifies: AIRA-144
func TestAIRA144RemoveRefusesALiveScopeAndStripsNoChildCgroup(t *testing.T) {
	parent := cgrouptest.IsolatedScopeParent(t)
	backend := newDefaultBackend(parent)
	if err := backend.Probe(context.Background()); err != nil {
		cgrouptest.SkipOrFailRealCgroup(t, "real cgroup backend unavailable: %v", err)
	}
	// A plain run-shaped scope id, not confineTestScopeID's `.aira-CONFINE-*`:
	// this is a run scope, and production's confine scans key on that prefix.
	scopeID := fmt.Sprintf("AIRA144-live-%d-%d", os.Getpid(), time.Now().UnixNano())
	scope, err := backend.Create(context.Background(), scopeID)
	if err != nil {
		cgrouptest.SkipOrFailRealCgroup(t, "real scope unavailable: %v", err)
	}
	defer func() { _ = scope.Kill(); _ = waitEmpty(context.Background(), scope, time.Second); _ = scope.Remove() }()

	// The live process goes in the scope's OWN leaf, so the empty child cgroup
	// beside it is the only thing the walk could reach.
	scopeFD, err := syscall.Open(scope.Reference(), syscall.O_RDONLY|syscall.O_DIRECTORY|syscall.O_CLOEXEC, 0)
	if err != nil {
		cgrouptest.SkipOrFailRealCgroup(t, "scope fd unavailable: %v", err)
	}
	defer syscall.Close(scopeFD)
	command := exec.Command("/bin/sh", "-c", "sleep 60")
	command.SysProcAttr = &syscall.SysProcAttr{UseCgroupFD: true, CgroupFD: scopeFD}
	if err := command.Start(); err != nil {
		cgrouptest.SkipOrFailRealCgroup(t, "start workload in scope: %v", err)
	}
	defer func() { _ = command.Process.Kill(); _, _ = command.Process.Wait() }()

	worker := filepath.Join(scope.Reference(), ".aira-worker-1")
	if err := os.Mkdir(worker, 0o755); err != nil {
		cgrouptest.SkipOrFailRealCgroup(t, "nested cgroup unavailable: %v", err)
	}

	if err := scope.Remove(); err == nil {
		t.Fatal("Remove() reported success against a scope holding a live process")
	}
	if _, err := os.Stat(worker); err != nil {
		t.Fatalf("Remove() stripped a LIVE job's empty child cgroup %s instead of refusing at its gate: %v", worker, err)
	}
	if _, err := os.Stat(scope.Reference()); err != nil {
		t.Fatalf("Remove() removed a populated scope: %v", err)
	}
	// And the refusal must not have cost the scope its own fd: a caller that
	// retries the teardown later still needs a usable scope.
	if _, err := scope.Members(); err != nil {
		t.Fatalf("a refused Remove() left the scope unusable: %v", err)
	}
}
