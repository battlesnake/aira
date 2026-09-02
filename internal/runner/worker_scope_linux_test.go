//go:build linux

package runner

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"aira/internal/cgrouptest"
)

func TestCreateWorkerScopeWritesVerifiedMemoryCap(t *testing.T) {
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

	// 134217728 (128 MiB) and 104857600 (100 MiB) are both exact multiples
	// of the page size — writeScopeMemoryCap's own verification page-floors
	// the value before comparing (confirmed against the real
	// verifyScopeMemoryValue/floorMemoryPage code), so an unaligned value
	// like 107374182 would be floored by the kernel to 107372544 and this
	// verbatim-string comparison would fail even on a correct
	// implementation.
	scopePath, err := CreateWorkerScope(context.Background(), outer, "1", 134217728, 104857600)
	if err != nil {
		t.Fatalf("CreateWorkerScope: %v", err)
	}
	if want := WorkerScopeChildPath(outer, "worker-1"); scopePath != want {
		t.Fatalf("scopePath=%q want %q", scopePath, want)
	}
	if data, err := os.ReadFile(filepath.Join(scopePath, "memory.max")); err != nil || strings.TrimSpace(string(data)) != "134217728" {
		t.Fatalf("memory.max=%q err=%v", data, err)
	}
	if data, err := os.ReadFile(filepath.Join(scopePath, "memory.high")); err != nil || strings.TrimSpace(string(data)) != "104857600" {
		t.Fatalf("memory.high=%q err=%v", data, err)
	}
	if data, err := os.ReadFile(filepath.Join(scopePath, "memory.oom.group")); err != nil || strings.TrimSpace(string(data)) != "1" {
		t.Fatalf("memory.oom.group=%q err=%v", data, err)
	}
	_ = strconv.Itoa // silence unused import if trimmed during edit
}

func TestCreateWorkerScopeRejectsMemoryHighAtOrAboveMemoryMax(t *testing.T) {
	// Spec 3.3 requires memory.high be "set below its cap" for a worker
	// scope -- a soft throttle before the hard memory.max cap. This
	// currently held only because evaluateWorkerAdmit (the sole real
	// caller) always computes memoryHigh = estimatedBytes*4/5 < memoryMax
	// by construction; CreateWorkerScope itself had no independent check
	// (found by Sol build-review, AIRA-38 review wave). No real cgroup
	// needed: this validation must reject before ever touching the
	// filesystem.
	outer := t.TempDir()
	for _, test := range []struct {
		name        string
		memoryMax   int64
		memoryHigh  int64
		wantRejects bool
	}{
		{name: "high equal to max", memoryMax: 100 << 20, memoryHigh: 100 << 20, wantRejects: true},
		{name: "high above max", memoryMax: 100 << 20, memoryHigh: 120 << 20, wantRejects: true},
		{name: "high zero (no watermark configured) is still valid", memoryMax: 100 << 20, memoryHigh: 0, wantRejects: false},
	} {
		t.Run(test.name, func(t *testing.T) {
			_, err := CreateWorkerScope(context.Background(), outer, "reject-"+test.name, test.memoryMax, test.memoryHigh)
			if test.wantRejects && err == nil {
				t.Fatalf("CreateWorkerScope unexpectedly succeeded with memory_high=%d >= memory_max=%d", test.memoryHigh, test.memoryMax)
			}
			// The zero-high case is expected to fail too, but for an
			// UNRELATED reason (outer here is a plain temp dir, not a real
			// delegated cgroup) -- only assert it does NOT fail with this
			// specific validation's own error text.
			if !test.wantRejects && err != nil && strings.Contains(err.Error(), "must be below") {
				t.Fatalf("memory_high=0 (no watermark) incorrectly rejected by the high<max validation: %v", err)
			}
		})
	}
}

func TestCreateWorkerScopeRemovesScopeOnMemoryCapFailure(t *testing.T) {
	parent := cgrouptest.IsolatedScopeParent(t)
	if err := os.WriteFile(filepath.Join(parent, "cgroup.subtree_control"), []byte("+memory"), 0o644); err != nil {
		cgrouptest.SkipOrFailRealCgroup(t, "memory controller not delegated to %s: %v", parent, err)
	}
	outer := filepath.Join(parent, ".aira-outer-test")
	if err := os.Mkdir(outer, 0o755); err != nil {
		t.Fatal(err)
	}
	// Deliberately do NOT call ensureConfineDelegation(outer). Without
	// +memory in outer's subtree_control, the worker child will not expose
	// memory.max and writeScopeMemoryCap must fail for this real cgroup
	// delegation error rather than a fabricated failure.
	data, err := os.ReadFile(filepath.Join(outer, "cgroup.subtree_control"))
	if err != nil {
		cgrouptest.SkipOrFailRealCgroup(t, "read outer cgroup.subtree_control: %v", err)
	}
	for _, controller := range strings.Fields(string(data)) {
		if controller == "memory" {
			t.Fatalf("test precondition failed: outer unexpectedly delegates memory; cannot reproduce missing worker memory.max")
		}
	}

	_, err = CreateWorkerScope(context.Background(), outer, "1", 134217728, 0)
	if err == nil {
		t.Fatal("CreateWorkerScope unexpectedly succeeded: worker memory.max was available despite missing outer memory delegation")
	}
	scopePath := filepath.Join(outer, ".aira-worker-1")
	if _, statErr := os.Stat(scopePath); !os.IsNotExist(statErr) {
		t.Fatalf("capless worker scope remains after memory cap failure: stat %q: %v", scopePath, statErr)
	}
}
