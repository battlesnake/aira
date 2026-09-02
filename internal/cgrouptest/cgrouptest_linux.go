//go:build linux

// Package cgrouptest provides shared test-support for real cgroup-v2 tests across
// packages. It is imported only from _test.go files; nothing in production depends
// on it, so the testing package never links into the release binary.
package cgrouptest

import (
	"bufio"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// IsolatedScopeParent returns a freshly created, delegated cgroup-v2 directory to
// pass as runner.Config.CgroupParent, unique across processes.
//
// Every package's real-cgroup tests run inside one ambient cgroup (e.g. the shared
// whale-run scope), and a runner left on the default ambient parent derives its run
// scope name from the run id alone — RUN-1 for a fresh store. Two packages' runners
// then race to `mkdir <ambient>/.aira-RUN-1` and one fails with "file exists".
// Giving each test's runs a private parent removes that cross-package collision.
//
// The name is allocated with os.MkdirTemp, whose kernel-atomic create-with-retry is
// unique across processes and across repeated calls in one test — so a test that
// builds several runners, and concurrent package test binaries, never collide on the
// parent name.
//
// No resource controller is enabled — a bare scoped kill needs none; a memory-metric
// test must build its own +memory parent. The subtree is killed and removed at test
// end. On a host without real cgroup-v2 delegation the test skips, unless
// AIRA_REAL_CGROUP=1 (the mode that MUST exercise real containment), where an
// unavailable cgroup is a hard failure rather than a silent skip.
func IsolatedScopeParent(t *testing.T) string {
	t.Helper()
	mount, err := unifiedMount()
	if err != nil {
		SkipOrFailRealCgroup(t, "cgroup-v2 unavailable: %v", err)
	}
	current, err := currentCgroupPath(mount)
	if err != nil {
		SkipOrFailRealCgroup(t, "current cgroup unavailable: %v", err)
	}
	// current holds the test process (cgroup-v2's no-internal-process rule), so place
	// the parent under current's parent, which delegates to children and holds no
	// direct processes of its own.
	host := filepath.Dir(current)
	prefix := ".aira-test-" + strings.NewReplacer("/", "_", " ", "_").Replace(t.Name()) + "-"
	parent, err := os.MkdirTemp(host, prefix)
	if err != nil {
		SkipOrFailRealCgroup(t, "cannot create scope parent under %s: %v", host, err)
	}
	t.Cleanup(func() { removeScopeTree(t, parent) })
	return parent
}

// SkipOrFailRealCgroup applies the real-cgroup test policy: a host without real
// cgroup-v2 delegation skips, but under AIRA_REAL_CGROUP=1 — the mode that MUST
// exercise real containment — an unavailable cgroup is a hard failure, never a
// silent skip. Call it from a runner Probe-failure branch too, so mandatory-real
// mode cannot false-green on a clone3/kernel/parent-usability failure that the
// parent mkdir alone does not catch.
func SkipOrFailRealCgroup(t *testing.T, format string, args ...any) {
	t.Helper()
	if os.Getenv("AIRA_REAL_CGROUP") == "1" {
		t.Fatalf(format, args...)
	}
	t.Skipf(format, args...)
}

// removeScopeTree kills every process in the subtree, then removes any lingering
// child run-scope dirs before removing the parent itself. cgroup.kill races task
// exit against the cgroup.procs drain, and a non-empty parent rmdir fails, so the
// removal retries briefly. A subtree that never drains is a real cgroup leak: it is
// surfaced (hard-failed under mandatory-real mode) rather than left silently green.
func removeScopeTree(t *testing.T, parent string) {
	t.Helper()
	_ = os.WriteFile(filepath.Join(parent, "cgroup.kill"), []byte("1"), 0o644)
	for i := 0; i < 200; i++ {
		removeScopeTreeChildren(parent)
		if os.Remove(parent) == nil {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	if os.Getenv("AIRA_REAL_CGROUP") == "1" {
		t.Errorf("cgroup scope parent leaked (not removed within budget): %s", parent)
	} else {
		t.Logf("cgroup scope parent leaked (not removed within budget): %s", parent)
	}
}

// removeScopeTreeChildren makes one best-effort DEPTH-FIRST pass, removing a
// directory's own children before the directory itself -- a cgroup directory
// with live children can never be removed by a plain os.Remove. Found live
// by Fable's final build-review gate (AIRA-30): the previous single-level
// version only ever tried removing removeScopeTree's immediate children,
// which could never drain a nested scope tree like aitest's own
// outer -> .aira-supervisor / .aira-worker-N layout -- every real-cgroup
// aitest test hard-FAILed under AIRA_REAL_CGROUP=1 ("cgroup scope parent
// leaked"), and every default-tier run silently leaked the tree into the
// live shared aira.slice instead.
func removeScopeTreeChildren(dir string) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		child := filepath.Join(dir, e.Name())
		removeScopeTreeChildren(child)
		_ = os.Remove(child)
	}
}

// unifiedMount and currentCgroupPath mirror the runner's production discovery (a
// stable-ABI parse of /proc/self/mountinfo and /proc/self/cgroup); duplicated here
// so this test-support package depends on no other package's internals.
func unifiedMount() (string, error) {
	f, err := os.Open("/proc/self/mountinfo")
	if err != nil {
		return "", err
	}
	defer f.Close()
	s := bufio.NewScanner(f)
	for s.Scan() {
		parts := strings.SplitN(s.Text(), " - ", 2)
		if len(parts) != 2 {
			continue
		}
		post := strings.Fields(parts[1])
		pre := strings.Fields(parts[0])
		if len(post) < 1 || len(pre) < 5 || post[0] != "cgroup2" {
			continue
		}
		return unescapeMount(pre[4]), nil
	}
	if err := s.Err(); err != nil {
		return "", err
	}
	return "", errors.New("cgroup-v2 unified mount not found")
}

func unescapeMount(s string) string {
	// mountinfo uses octal escapes for spaces, tabs, and backslashes.
	for _, item := range []struct{ from, to string }{{"\\040", " "}, {"\\011", "\t"}, {"\\134", "\\"}} {
		s = strings.ReplaceAll(s, item.from, item.to)
	}
	return s
}

func currentCgroupPath(mount string) (string, error) {
	data, err := os.ReadFile("/proc/self/cgroup")
	if err != nil {
		return "", err
	}
	for _, line := range strings.Split(string(data), "\n") {
		if strings.HasPrefix(line, "0::") {
			rel := strings.TrimPrefix(line, "0::")
			return filepath.Join(mount, strings.TrimPrefix(rel, "/")), nil
		}
	}
	return "", errors.New("unified cgroup membership not found")
}
