package daemon

import (
	"os"
	"path/filepath"
	"testing"
)

// verifies: the cap classifier tolerates the real cgroup2 invariant that the mount
// ROOT (and controllerless cgroups) have NO memory.max file — an absent memory.max
// means "no cap here, continue the walk", not "unevaluated". Aborting on that ENOENT
// made the watchdog inert on every real host (build-review P1): it could never
// classify any process as uncapped, so it never selected or killed an offender.
// Proven RED against the abort-on-any-error impl; GREEN once ENOENT continues.
func TestWatchdogClassifierToleratesAbsentRootMemoryMax(t *testing.T) {
	root := t.TempDir() // stands in for /sys/fs/cgroup — deliberately NO memory.max here
	leaf := filepath.Join(root, "user.slice", "job.scope")
	if err := os.MkdirAll(leaf, 0o755); err != nil {
		t.Fatal(err)
	}
	// Leaf explicitly uncapped ("max"); a finite cap on the intermediate slice; the
	// mount root has no memory.max, exactly like a real host.
	if err := os.WriteFile(filepath.Join(leaf, "memory.max"), []byte("max\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "user.slice", "memory.max"), []byte("4096\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	cap, finite, evaluated := effectiveWatchdogCapEvaluated(root, leaf)
	if !evaluated {
		t.Fatal("classifier aborted (evaluated=false) on the mount root's absent memory.max — on every real cgroup2 host this makes the watchdog permanently inert")
	}
	if !finite || cap != 4096 {
		t.Fatalf("cap=%d finite=%v; want cap=4096 finite=true (the intermediate slice bounds the subtree)", cap, finite)
	}

	// And a genuinely uncapped subtree (no finite cap anywhere, root absent) must be
	// classified evaluated=true, finite=false → eligible.
	freeLeaf := filepath.Join(root, "free.slice", "job.scope")
	if err := os.MkdirAll(freeLeaf, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, d := range []string{filepath.Join(root, "free.slice"), freeLeaf} {
		if err := os.WriteFile(filepath.Join(d, "memory.max"), []byte("max\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if _, finite, evaluated := effectiveWatchdogCapEvaluated(root, freeLeaf); !evaluated || finite {
		t.Fatalf("uncapped subtree: evaluated=%v finite=%v; want evaluated=true finite=false", evaluated, finite)
	}
}
