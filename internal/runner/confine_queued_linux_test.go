//go:build linux

package runner

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"
)

// AIRA-308. Queued jobs as listing rows: built from the scope id the client minted
// plus the two figures the daemon holds, with the supervisor's command, cwd and
// tmux window read live through the SAME helper and seams the scope scan uses.
//
// verifies: AIRA-308

func queuedTestDeps(now time.Time) confineScanDeps {
	deps := defaultConfineScanDeps()
	deps.now = func() time.Time { return now }
	deps.readCmdline = func(pid int) ([]byte, error) {
		if pid == 7001 {
			return []byte("aira\x00confine\x00--name\x00x\x00--\x00go\x00test\x00./...\x00"), nil
		}
		return nil, errors.New("no such process")
	}
	deps.readCwd = func(pid int) (string, error) {
		if pid == 7001 {
			return "/work/alpha", nil
		}
		return "", errors.New("no such process")
	}
	deps.readEnviron = func(pid int) ([]byte, error) {
		if pid == 7001 {
			return []byte("HOME=/home/x\x00"), nil // established: not launched from tmux
		}
		return nil, errors.New("no such process")
	}
	deps.tmuxNames = func(context.Context, string) (map[string]string, error) { return nil, errors.New("no tmux") }
	return deps
}

// (b) The headline. Two queued jobs, one with a readable supervisor and one whose
// pid is gone, plus an undecodable id. Every outcome is asserted on a DIFFERENT
// entry in one pass, so a builder that read nothing, read everything as readable,
// or fabricated a value for the unreadable one each fails a distinct assertion.
func TestQueuedConfineRowsReadSupervisorFacetsLiveAndNameWhatTheyCannotRead(t *testing.T) {
	t.Parallel()
	now := time.Unix(1_800_000_000, 0)
	readable := confineTestOwnedScopeID("alpha", "session-a", 7001, now.Add(-90*time.Second).UnixNano())
	gone := confineTestOwnedScopeID("beta", "session-b", 7002, now.Add(-10*time.Second).UnixNano())
	entries := []ConfineQueuedEntry{
		{ScopeID: readable, ReserveBytes: 3 << 30, VRAMBytes: 2 << 30},
		{ScopeID: "not-a-scope-id", ReserveBytes: 1 << 30},
		{ScopeID: gone, ReserveBytes: 5 << 30},
	}
	rows := listQueuedConfinesWithDeps(context.Background(), entries, queuedTestDeps(now))
	if len(rows) != 2 || rows[0].ScopeID != readable || rows[1].ScopeID != gone {
		t.Fatalf("rows=%+v, want exactly the two decodable entries in the order given", rows)
	}

	a, b := rows[0], rows[1]
	if a.Name != "alpha" || a.Owner != "session-a" || a.SupervisorPID == nil || *a.SupervisorPID != 7001 {
		t.Fatalf("identity not decoded from the scope id: %+v", a)
	}
	if a.ReserveBytes == nil || *a.ReserveBytes != 3<<30 || a.VRAMBytes == nil || *a.VRAMBytes != 2<<30 {
		t.Fatalf("requested reserve/vram not carried: reserve=%v vram=%v", a.ReserveBytes, a.VRAMBytes)
	}
	if b.ReserveBytes == nil || *b.ReserveBytes != 5<<30 || b.VRAMBytes == nil || *b.VRAMBytes != 0 {
		t.Fatalf("second row reserve/vram wrong (0 vram is an ESTABLISHED not-a-GPU-job): reserve=%v vram=%v", b.ReserveBytes, b.VRAMBytes)
	}
	if a.AgeSeconds == nil || *a.AgeSeconds != 90 || b.AgeSeconds == nil || *b.AgeSeconds != 10 {
		t.Fatalf("age must run from the scope id's launch stamp: a=%v b=%v", a.AgeSeconds, b.AgeSeconds)
	}
	if a.Command == nil || *a.Command != "go test ./..." {
		t.Fatalf("command=%v, want the argv after `--`", a.Command)
	}
	if a.Cwd == nil || *a.Cwd != "/work/alpha" {
		t.Fatalf("cwd=%v", a.Cwd)
	}
	if a.Pane == nil || *a.Pane != "" {
		t.Fatalf("pane=%v, want the established not-in-tmux empty string", a.Pane)
	}
	// The unreadable supervisor: nil values, each NAMED — never a blank or a
	// fabricated placeholder that a renderer would print as a fact.
	if b.Command != nil || b.Cwd != nil || b.Pane != nil {
		t.Fatalf("unreadable supervisor produced values: command=%v cwd=%v pane=%v", b.Command, b.Cwd, b.Pane)
	}
	for _, facet := range []string{"command", "cwd", "pane"} {
		if !confineContainsString(b.UnevaluatedFields, facet) {
			t.Fatalf("unreadable %s not named unevaluated: %v", facet, b.UnevaluatedFields)
		}
		if confineContainsString(a.UnevaluatedFields, facet) {
			t.Fatalf("readable %s named unevaluated: %v", facet, a.UnevaluatedFields)
		}
	}
	// No scope exists, so no cgroup facet is established and none is invented.
	for _, record := range rows {
		if record.Pending || record.RSSBytes != nil || record.Cap != nil || record.Populated != nil ||
			record.SubtreePopulated != nil || record.CPUUsageUsec != nil || record.SupervisorLive != nil {
			t.Fatalf("queued row carries a scope-derived value: %+v", record)
		}
		for _, facet := range []string{"populated", "rss", "cap", "cpu", "supervisor_live"} {
			if !confineContainsString(record.UnevaluatedFields, facet) {
				t.Fatalf("%s not named unevaluated on %s: %v", facet, record.ScopeID, record.UnevaluatedFields)
			}
		}
	}
}

// The seams are optional: a deps value with only a clock must fall back to the
// real /proc readers rather than nil-panic, as the scan's own deps do. The pid is
// this test process, whose /proc entries are readable, so a fallback that read
// nothing would leave Command nil and fail here.
func TestQueuedConfineRowsFallBackToTheRealReadersWhenSeamsAreUnset(t *testing.T) {
	t.Parallel()
	now := time.Now()
	id := confineTestScopeID("self", os.Getpid(), now.Add(-time.Minute).UnixNano())
	rows := listQueuedConfinesWithDeps(context.Background(), []ConfineQueuedEntry{{ScopeID: id, ReserveBytes: 1}},
		confineScanDeps{now: func() time.Time { return now }, tmuxNames: func(context.Context, string) (map[string]string, error) { return nil, errors.New("no tmux") }})
	if len(rows) != 1 {
		t.Fatalf("rows=%+v", rows)
	}
	if rows[0].Command == nil || rows[0].Cwd == nil {
		t.Fatalf("real readers not used: %+v", rows[0])
	}
}

// One id queued twice (a retried probe, a hostile client) is one row: the first
// wins, so the table can never show the same scope id twice.
func TestQueuedConfineRowsDropARepeatedScopeID(t *testing.T) {
	t.Parallel()
	now := time.Unix(1_800_000_000, 0)
	id := confineTestScopeID("alpha", 7001, now.Add(-time.Second).UnixNano())
	rows := listQueuedConfinesWithDeps(context.Background(), []ConfineQueuedEntry{
		{ScopeID: id, ReserveBytes: 1 << 30}, {ScopeID: id, ReserveBytes: 9 << 30},
	}, queuedTestDeps(now))
	if len(rows) != 1 || *rows[0].ReserveBytes != 1<<30 {
		t.Fatalf("rows=%+v, want the first entry only", rows)
	}
}

// Shim mode / non-Linux: NO live read of any kind. Counting the readers invoked
// is the pin, because a builder that reused the live path would "work" here too
// (the pids are fakes, so every read would fail into the same unevaluated names).
func TestShimQueuedConfinesMakeNoLiveRead(t *testing.T) {
	t.Parallel()
	now := time.Unix(1_800_000_000, 0)
	id := confineTestOwnedScopeID("gate", "session-a", os.Getpid(), now.Add(-5*time.Second).UnixNano())
	rows := shimQueuedConfines([]ConfineQueuedEntry{{ScopeID: id, ReserveBytes: 2 << 30}, {ScopeID: "garbage"}}, now)
	if len(rows) != 1 {
		t.Fatalf("rows=%+v", rows)
	}
	row := rows[0]
	if row.Command != nil || row.Cwd != nil || row.Pane != nil {
		t.Fatalf("shim row read /proc for this very (readable) process: %+v", row)
	}
	if row.ReserveBytes == nil || *row.ReserveBytes != 2<<30 || row.AgeSeconds == nil || *row.AgeSeconds != 5 {
		t.Fatalf("shim row lost its non-live facets: %+v", row)
	}
	for _, facet := range []string{"command", "cwd", "pane", "populated", "rss", "cap", "cpu", "supervisor_live"} {
		if !confineContainsString(row.UnevaluatedFields, facet) {
			t.Fatalf("%s not named unevaluated: %v", facet, row.UnevaluatedFields)
		}
	}
	if row.Name != "gate" {
		t.Fatalf("name=%q", row.Name)
	}
}
