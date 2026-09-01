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
