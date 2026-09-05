//go:build linux

package daemon

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// verifies: AIRA-64 §4.4 — the two counts are genuinely different and each
// fails in its own safe direction.

const testGrace = 60 * time.Second

// makeWorkerScope creates <slice>/<confine>/.aira-worker-<id> and, when
// events is non-empty, a cgroup.events file with that content.
func makeWorkerScope(t *testing.T, slice, confine, id, events string) string {
	t.Helper()
	child := filepath.Join(slice, confine, workerScopeChildPrefix+id)
	if err := os.MkdirAll(child, 0o755); err != nil {
		t.Fatal(err)
	}
	if events != "" {
		if err := os.WriteFile(filepath.Join(child, "cgroup.events"), []byte(events), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return child
}

func newTestSlice(t *testing.T) string {
	t.Helper()
	return t.TempDir()
}

func TestWorkerScopeLiveForFloorPopulatedChildIsLive(t *testing.T) {
	slice := newTestSlice(t)
	child := makeWorkerScope(t, slice, ".aira-CONFINE-a", "1", "populated 1\nfrozen 0\n")
	if !workerScopeLiveForFloor(child) {
		t.Fatal("a populated worker scope must count as live for the floor")
	}
}

// verifies: AIRA-64 §9.4 — Sol round 1 P0-2. An empty orphan must NOT hold the
// floor closed, or a job stalls forever instead of merely running slowly.
func TestWorkerScopeLiveForFloorEmptyOrphanIsNotLive(t *testing.T) {
	slice := newTestSlice(t)
	child := makeWorkerScope(t, slice, ".aira-CONFINE-a", "1", "populated 0\nfrozen 0\n")
	if workerScopeLiveForFloor(child) {
		t.Fatal("an empty orphan must not count as live: it would permanently withhold the floor")
	}
}

// verifies: AIRA-64 §9.7 — population that cannot be established counts the
// child as LIVE. Never a fabricated open floor.
func TestWorkerScopeLiveForFloorUnestablishedPopulationIsLive(t *testing.T) {
	slice := newTestSlice(t)
	for _, testCase := range []struct {
		name   string
		events string
	}{
		{"missing cgroup.events", ""},
		{"malformed cgroup.events", "this is not a cgroup.events file\n"},
		{"populated field absent", "frozen 0\n"},
		{"populated not an integer", "populated yes\n"},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			child := makeWorkerScope(t, slice, ".aira-CONFINE-"+testCase.name, "1", testCase.events)
			if !workerScopeLiveForFloor(child) {
				t.Fatal("an unestablished population must count as live, never as an open floor")
			}
		})
	}
	t.Run("unreadable directory", func(t *testing.T) {
		if !workerScopeLiveForFloor(filepath.Join(slice, "does-not-exist")) {
			t.Fatal("an unreadable scope must count as live")
		}
	})
}

// verifies: AIRA-64 §9.6 — the cap count and the floor count are different
// numbers over the same tree, each failing in its own safe direction.
func TestScanSliceWorkerScopesCountsDirectoriesForCapAndLiveForFloor(t *testing.T) {
	slice := newTestSlice(t)
	makeWorkerScope(t, slice, ".aira-CONFINE-a", "1", "populated 1\n")
	makeWorkerScope(t, slice, ".aira-CONFINE-a", "2", "populated 0\n") // orphan
	makeWorkerScope(t, slice, ".aira-CONFINE-b", "1", "populated 1\n")

	snapshot, err := scanSliceWorkerScopes(slice)
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.total != 3 {
		t.Fatalf("cap count must count every worker DIRECTORY (never undercount): got %d, want 3", snapshot.total)
	}
	if snapshot.scopes != 2 {
		t.Fatalf("J must count confine scopes holding >=1 worker child: got %d, want 2", snapshot.scopes)
	}
	if got := snapshot.liveForFloor[filepath.Join(slice, ".aira-CONFINE-a")]; got != 1 {
		t.Fatalf("scope a: the empty orphan must not count for the floor: got %d, want 1", got)
	}
	if got := snapshot.liveForFloor[filepath.Join(slice, ".aira-CONFINE-b")]; got != 1 {
		t.Fatalf("scope b: got %d, want 1", got)
	}
}

// verifies: AIRA-64 §13(d) — Sol build-review P0. An outer scope this scan
// CANNOT READ must fail the whole snapshot, not be silently skipped.
//
// Skipping it was a real fail-open: one persistently unreadable BUSY scope made
// the slice look emptier than it is, so the gate admitted past capacity while
// still reporting cpu_slots=ok — a false claim of governance, undetectable from
// outside. Only a scope PROVEN to have vanished may be skipped.
func TestScanSliceWorkerScopesFailsOnAnUnreadableConfineScope(t *testing.T) {
	slice := newTestSlice(t)
	makeWorkerScope(t, slice, ".aira-CONFINE-busy", "1", "populated 1\n")
	blocked := filepath.Join(slice, ".aira-CONFINE-blocked")
	if err := os.MkdirAll(filepath.Join(blocked, workerScopeChildPrefix+"1"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(blocked, 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(blocked, 0o755) })
	if os.Geteuid() == 0 {
		t.Skip("running as root: mode 000 does not deny the listing")
	}

	if _, err := scanSliceWorkerScopes(slice); err == nil {
		t.Fatal("an unreadable confine scope must fail the snapshot; silently skipping it lets a " +
			"busy slice read as idle while the grant still claims cpu_slots=ok")
	}
}

// verifies: AIRA-64 — a scope that PROVABLY vanished between the two listings
// is the benign teardown race and is skipped, not turned into an error that
// would report the whole machine unevaluated whenever a job is exiting.
func TestScanSliceWorkerScopesSkipsAVanishedConfineScope(t *testing.T) {
	slice := newTestSlice(t)
	makeWorkerScope(t, slice, ".aira-CONFINE-a", "1", "populated 1\n")
	gone := filepath.Join(slice, ".aira-CONFINE-gone")
	if err := os.Mkdir(gone, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(gone); err != nil {
		t.Fatal(err)
	}
	snapshot, err := scanSliceWorkerScopes(slice)
	if err != nil {
		t.Fatalf("a vanished scope is the benign race, not an error: %v", err)
	}
	if snapshot.total != 1 {
		t.Fatalf("total=%d, want 1", snapshot.total)
	}
}

// verifies: AIRA-64 §4.3 — only .aira-CONFINE-*/.aira-worker-* is counted.
// Nothing else on the slice may inflate the machine's busyness.
func TestScanSliceWorkerScopesIgnoresForeignEntries(t *testing.T) {
	slice := newTestSlice(t)
	makeWorkerScope(t, slice, ".aira-CONFINE-a", "1", "populated 1\n")
	if err := os.MkdirAll(filepath.Join(slice, "some-other.scope", ".aira-worker-9"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(slice, ".aira-CONFINE-a", ".aira-supervisor"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(slice, ".aira-CONFINE-a", "memory.max"), []byte("max\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	snapshot, err := scanSliceWorkerScopes(slice)
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.total != 1 {
		t.Fatalf("only .aira-CONFINE-*/.aira-worker-* counts: got %d, want 1", snapshot.total)
	}
}

// verifies: AIRA-64 §4.9 — a slice that cannot be read is an error, never a
// fabricated zero (which would read as "the machine is idle" and admit freely).
func TestScanSliceWorkerScopesUnreadableSliceIsAnError(t *testing.T) {
	if _, err := scanSliceWorkerScopes(filepath.Join(t.TempDir(), "absent")); err == nil {
		t.Fatal("an unreadable slice must be an error, never a zero count")
	}
}

// verifies: AIRA-64 §4.8 — the scan root is derived from the outer scope and
// accepted only for a real .aira-CONFINE-* child.
func TestCPUSlotsScanRootAcceptsOnlyConfineScopes(t *testing.T) {
	for _, testCase := range []struct {
		name  string
		outer string
		want  string
		ok    bool
	}{
		{"confine scope", "/sys/fs/cgroup/aira.slice/.aira-CONFINE-job-1-abc@owner", "/sys/fs/cgroup/aira.slice", true},
		{"not a confine scope", "/sys/fs/cgroup/aira.slice/some.scope", "", false},
		{"bare root", "/", "", false},
		{"relative", ".aira-CONFINE-x", "", false},
		{"empty", "", "", false},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			got, _, ok := cpuSlotsScanRoot(testCase.outer)
			if ok != testCase.ok || got != testCase.want {
				t.Fatalf("cpuSlotsScanRoot(%q) = (%q, %v), want (%q, %v)", testCase.outer, got, ok, testCase.want, testCase.ok)
			}
		})
	}
}

// verifies: AIRA-64 §4.4.2 — the daemon's grace tracks the CLIENT's own
// placement-ack timeout, so an operator override cannot silently drift the two
// halves apart.
func TestCPUSlotsPlacementGraceFollowsTheClientSetting(t *testing.T) {
	t.Setenv("AIRA_AITEST_PLACEMENT_ACK_TIMEOUT", "")
	if got := cpuSlotsPlacementGrace(); got != cpuSlotsPlacementGraceDefault {
		t.Fatalf("unset: got %v, want %v", got, cpuSlotsPlacementGraceDefault)
	}
	t.Setenv("AIRA_AITEST_PLACEMENT_ACK_TIMEOUT", "120")
	if got := cpuSlotsPlacementGrace(); got != 120*time.Second {
		t.Fatalf("override: got %v, want 120s", got)
	}
	for _, bad := range []string{"nonsense", "-5", "0"} {
		t.Setenv("AIRA_AITEST_PLACEMENT_ACK_TIMEOUT", bad)
		if got := cpuSlotsPlacementGrace(); got != cpuSlotsPlacementGraceDefault {
			t.Fatalf("invalid %q must fall back to the default, got %v", bad, got)
		}
	}
}
