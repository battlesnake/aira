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
// events is non-empty, a cgroup.events file with that content. age backdates
// the directory's mtime.
func makeWorkerScope(t *testing.T, slice, confine, id, events string, age time.Duration) string {
	t.Helper()
	outer := filepath.Join(slice, confine)
	child := filepath.Join(outer, workerScopeChildPrefix+id)
	if err := os.MkdirAll(child, 0o755); err != nil {
		t.Fatal(err)
	}
	if events != "" {
		if err := os.WriteFile(filepath.Join(child, "cgroup.events"), []byte(events), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	stamp := time.Now().Add(-age)
	if err := os.Chtimes(child, stamp, stamp); err != nil {
		t.Fatal(err)
	}
	return child
}

func newTestSlice(t *testing.T) string {
	t.Helper()
	slice := t.TempDir()
	return slice
}

func TestWorkerScopeLiveForFloorPopulatedChildIsLive(t *testing.T) {
	slice := newTestSlice(t)
	child := makeWorkerScope(t, slice, ".aira-CONFINE-a", "1", "populated 1\nfrozen 0\n", 10*time.Minute)
	if !workerScopeLiveForFloor(child, time.Now(), testGrace) {
		t.Fatal("a populated worker scope must count as live for the floor")
	}
}

// verifies: AIRA-64 §9.4 — Sol round 1 P0-2. An aged, empty orphan must NOT
// hold the floor closed, or a job stalls forever instead of merely running slowly.
func TestWorkerScopeLiveForFloorAgedEmptyOrphanIsNotLive(t *testing.T) {
	slice := newTestSlice(t)
	child := makeWorkerScope(t, slice, ".aira-CONFINE-a", "1", "populated 0\nfrozen 0\n", 10*time.Minute)
	if workerScopeLiveForFloor(child, time.Now(), testGrace) {
		t.Fatal("an aged empty orphan must not count as live: it would permanently withhold the floor")
	}
}

// verifies: AIRA-64 §9.5 — Sol round 2 P0-2. A scope created moments ago and
// not yet placed into MUST count as live, or N supervisors paused between grant
// and placement each take a floor grant.
func TestWorkerScopeLiveForFloorYoungEmptyScopeIsLive(t *testing.T) {
	slice := newTestSlice(t)
	child := makeWorkerScope(t, slice, ".aira-CONFINE-a", "1", "populated 0\nfrozen 0\n", time.Second)
	if !workerScopeLiveForFloor(child, time.Now(), testGrace) {
		t.Fatal("a young unplaced scope must count as live: the placement window is not an open floor")
	}
}

// verifies: AIRA-64 §9.7, §9.18 — population that cannot be established, and a
// stat failure, both count the child as live. Never a fabricated open floor.
func TestWorkerScopeLiveForFloorUnestablishedPopulationIsLive(t *testing.T) {
	slice := newTestSlice(t)
	now := time.Now()
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
			child := makeWorkerScope(t, slice, ".aira-CONFINE-"+testCase.name, "1", testCase.events, 10*time.Minute)
			if !workerScopeLiveForFloor(child, now, testGrace) {
				t.Fatal("an unestablished population must count as live, never as an open floor")
			}
		})
	}
	t.Run("stat failure", func(t *testing.T) {
		if !workerScopeLiveForFloor(filepath.Join(slice, "does-not-exist"), now, testGrace) {
			t.Fatal("an unreadable directory must count as live")
		}
	})
}

// verifies: AIRA-64 §9.18 — clock skew. An mtime in the future must read as
// YOUNG (live), never as aged out.
func TestWorkerScopeLiveForFloorFutureMtimeIsYoung(t *testing.T) {
	slice := newTestSlice(t)
	child := makeWorkerScope(t, slice, ".aira-CONFINE-a", "1", "populated 0\n", -2*time.Hour)
	if !workerScopeLiveForFloor(child, time.Now(), testGrace) {
		t.Fatal("a future mtime must read as young, not as aged out")
	}
}

// verifies: AIRA-64 §9.6 — the cap count and the floor count are different
// numbers over the same tree, each failing in its own safe direction.
func TestScanSliceWorkerScopesCountsDirectoriesForCapAndLiveForFloor(t *testing.T) {
	slice := newTestSlice(t)
	makeWorkerScope(t, slice, ".aira-CONFINE-a", "1", "populated 1\n", time.Minute)
	makeWorkerScope(t, slice, ".aira-CONFINE-a", "2", "populated 0\n", 10*time.Minute) // aged orphan
	makeWorkerScope(t, slice, ".aira-CONFINE-b", "1", "populated 0\n", time.Second)    // young, unplaced

	snapshot, err := scanSliceWorkerScopes(slice, time.Now(), testGrace)
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
		t.Fatalf("scope a: the aged orphan must not count for the floor: got %d, want 1", got)
	}
	if got := snapshot.liveForFloor[filepath.Join(slice, ".aira-CONFINE-b")]; got != 1 {
		t.Fatalf("scope b: the young unplaced scope must count for the floor: got %d, want 1", got)
	}
}

// verifies: AIRA-64 §4.3 — only .aira-CONFINE-*/.aira-worker-* is counted.
// Nothing else on the slice may inflate the machine's busyness.
func TestScanSliceWorkerScopesIgnoresForeignEntries(t *testing.T) {
	slice := newTestSlice(t)
	makeWorkerScope(t, slice, ".aira-CONFINE-a", "1", "populated 1\n", time.Minute)
	if err := os.MkdirAll(filepath.Join(slice, "some-other.scope", ".aira-worker-9"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(slice, ".aira-CONFINE-a", ".aira-supervisor"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(slice, ".aira-CONFINE-a", "memory.max"), []byte("max\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	snapshot, err := scanSliceWorkerScopes(slice, time.Now(), testGrace)
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
	if _, err := scanSliceWorkerScopes(filepath.Join(t.TempDir(), "absent"), time.Now(), testGrace); err == nil {
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
			got, ok := cpuSlotsScanRoot(testCase.outer)
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
