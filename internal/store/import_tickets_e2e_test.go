package store

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// AIRA-237 Task 4 — allocator seed -> mint-forward E2E (the load-bearing
// round-trip shape-2 rests on) + the --allocated-max HWM seed. This is
// correctness-critical (ID allocation): import seeds the per-prefix cursor, the
// forward allocator mints past every imported id, an authored row materialises
// the mint, and nothing stomps the imported backlog.

// TestE2ESeedMintForwardRoundTrip is the single-worktree round-trip: import a
// pre-allocated backlog BL-1..BL-100, mint the next free id, author it (import
// materialises the pending state='allocated' row), mint again, and confirm no
// imported ticket was altered. It is a REGRESSION PIN (it greens against the
// Task 1-3 code), but its `first == FEE-BL-101` assertion reds a broken/absent
// counter seed (which would re-mint FEE-BL-1 and stomp the backlog).
func TestE2ESeedMintForwardRoundTrip(t *testing.T) {
	ctx := context.Background()
	s := namespacedStore(t, "FEE", "BL")

	// Import BL-1..BL-100 (bare ids; stored compound FEE-BL-N under id_prefix=FEE).
	var lines []string
	for n := 1; n <= 100; n++ {
		lines = append(lines, fmt.Sprintf(`{"id":"BL-%d","title":"item %d","status":"planned","kind":"chore","severity":"P2","body":"b"}`, n, n))
	}
	summary, err := s.ImportTicketsBytes(ctx, []byte(strings.Join(lines, "\n")), true, nil)
	if err != nil {
		t.Fatalf("import backlog: %v", err)
	}
	if len(summary.Created) != 100 {
		t.Fatalf("created=%d; want 100", len(summary.Created))
	}

	// Snapshot every imported file digest + allocation state (the "no stomp" oracle).
	digests := make(map[string]string, 100)
	for n := 1; n <= 100; n++ {
		id := fmt.Sprintf("FEE-BL-%d", n)
		d, derr := fileDigest(s.ticketPath(id))
		if derr != nil || d == "" {
			t.Fatalf("digest %s: %v (d=%q)", id, derr, d)
		}
		digests[id] = d
	}

	// `aira id BL` mints the next free number — the FEE-prefixed variant (type
	// BL-101, stored FEE-BL-101) — with no stomp on BL-1..100.
	first, err := s.AllocateID(ctx, "BL")
	if err != nil {
		t.Fatalf("AllocateID #1: %v", err)
	}
	if first != "FEE-BL-101" {
		t.Fatalf("first mint=%q; want FEE-BL-101 (next free, no stomp)", first)
	}

	// Author the minted id: importing the row materialises the pending
	// state='allocated' allocation (the shape-2 mint->author->import loop). A
	// plain CreateTicket would instead mint a FRESH number and leave FEE-BL-101
	// dangling, so the import path is what "create a ticket with that minted id"
	// means.
	if _, err := s.ImportTicketsBytes(ctx, []byte(`{"id":"BL-101","title":"minted","status":"planned","kind":"chore","severity":"P2","body":"b"}`), true, nil); err != nil {
		t.Fatalf("import the minted id: %v", err)
	}
	state, path, _, ok := allocationRowSuffix(t, s, "FEE-BL", 101, "")
	if !ok || state != "materialised" {
		t.Fatalf("FEE-BL-101 allocation state=%q ok=%v; want materialised", state, ok)
	}
	if filepath.Base(path) != "FEE-BL-101.md" {
		t.Fatalf("FEE-BL-101 path base=%q; want FEE-BL-101.md", filepath.Base(path))
	}

	// aira check is green at this clean point (every allocation materialised).
	report, err := s.Check(ctx)
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if report.Verdict == "fail" || report.Dimensions["allocated-id-file"] == "fail" {
		t.Fatalf("check fail after round-trip; findings=%+v", report.Findings)
	}

	// A second `aira id BL` gives the next number.
	second, err := s.AllocateID(ctx, "BL")
	if err != nil {
		t.Fatalf("AllocateID #2: %v", err)
	}
	if second != "FEE-BL-102" {
		t.Fatalf("second mint=%q; want FEE-BL-102", second)
	}

	// None of BL-1..100 were altered (files byte-identical, allocations intact).
	for n := 1; n <= 100; n++ {
		id := fmt.Sprintf("FEE-BL-%d", n)
		d, _ := fileDigest(s.ticketPath(id))
		if d != digests[id] {
			t.Fatalf("%s file altered by mint-forward (digest changed)", id)
		}
		st, _, _, ok := allocationRowSuffix(t, s, "FEE-BL", int64(n), "")
		if !ok || st != "materialised" {
			t.Fatalf("%s allocation state=%q ok=%v; want materialised (no stomp)", id, st, ok)
		}
	}
}

// TestE2ESplitSuffixHWMFloor pins the interaction of split-suffix ids (Task 3)
// with the numeric-only forward-mint floor: a suffixed child contributes its
// NUMERIC part to the HWM and the allocator never mints a suffix.
func TestE2ESplitSuffixHWMFloor(t *testing.T) {
	ctx := context.Background()

	// (a) A high plain number alongside a low split-suffix child: the allocator
	// floors on the NUMERIC max (100), above the child's number (10).
	t.Run("plain-max-dominates", func(t *testing.T) {
		s := namespacedStore(t, "FEE", "BL")
		if _, err := s.ImportTicketsBytes(ctx, []byte(strings.Join([]string{
			`{"id":"BL-100","title":"hundred","status":"planned","kind":"chore","severity":"P2","body":"b"}`,
			`{"id":"BL-10a","title":"child a","status":"planned","kind":"chore","severity":"P2","body":"b"}`,
		}, "\n")), true, nil); err != nil {
			t.Fatalf("import: %v", err)
		}
		minted, err := s.AllocateID(ctx, "BL")
		if err != nil {
			t.Fatalf("AllocateID: %v", err)
		}
		if minted != "FEE-BL-101" {
			t.Fatalf("mint=%q; want FEE-BL-101 (numeric max 100 + 1)", minted)
		}
	})

	// (b) ONLY a split-suffix child: the number-only HWM must still see 10 through
	// splitTicketID, so the allocator mints 11. A suffixed id whose number parsed
	// to 0 (the pre-Task-3 Atoi bug) would leave the counter at 1 and mint
	// FEE-BL-1 — reddening this assertion.
	t.Run("suffixed-child-seeds-numeric-counter", func(t *testing.T) {
		s := namespacedStore(t, "FEE", "BL")
		if _, err := s.ImportTicketsBytes(ctx, []byte(`{"id":"BL-10a","title":"child a","status":"planned","kind":"chore","severity":"P2","body":"b"}`), true, nil); err != nil {
			t.Fatalf("import: %v", err)
		}
		minted, err := s.AllocateID(ctx, "BL")
		if err != nil {
			t.Fatalf("AllocateID: %v", err)
		}
		if minted != "FEE-BL-11" {
			t.Fatalf("mint=%q; want FEE-BL-11 (numeric part 10 + 1)", minted)
		}
	})
}

// TestE2ECrossWorktreeSeedMintImport is the NORMAL shape-2 flow (design point
// 3): `aira id` runs in a feature worktree (recording a state='allocated' row
// with that worktree's path) and `aira import` of the authored row runs
// elsewhere. It exercises Task 2's cross-worktree adoption — the path where
// ImportRequirements' path-equality refusal would break — and asserts the
// importing worktree's check is green.
func TestE2ECrossWorktreeSeedMintImport(t *testing.T) {
	ctx := context.Background()
	base := t.TempDir()
	if err := os.MkdirAll(filepath.Join(base, "state"), 0o755); err != nil {
		t.Fatal(err)
	}
	a := namespacedStoreSharing(t, base, "wt-a", "FEE", "BL")
	b := namespacedStoreSharing(t, base, "wt-b", "FEE", "BL")

	// A mints FEE-BL-1 (state='allocated', A's path).
	minted, err := a.AllocateID(ctx, "BL")
	if err != nil {
		t.Fatalf("A.AllocateID: %v", err)
	}
	if minted != "FEE-BL-1" {
		t.Fatalf("A minted %q; want FEE-BL-1", minted)
	}
	if st, ap, _, ok := allocationRowSuffix(t, a, "FEE-BL", 1, ""); !ok || st != "allocated" || !strings.Contains(ap, filepath.Join("wt-a", ".aira")) {
		t.Fatalf("post-mint allocation state=%q ok=%v path=%q; want allocated in A", st, ok, ap)
	}

	// B imports the same id (authored elsewhere) → materialise HERE, path rewritten
	// to B.
	if _, err := b.ImportTicketsBytes(ctx, []byte(`{"id":"BL-1","title":"adopted","status":"planned","kind":"chore","severity":"P2","body":"b"}`), true, nil); err != nil {
		t.Fatalf("B import cross-worktree mint: %v", err)
	}
	state, path, _, ok := allocationRowSuffix(t, b, "FEE-BL", 1, "")
	if !ok || state != "materialised" {
		t.Fatalf("allocation state=%q ok=%v; want materialised", state, ok)
	}
	if !strings.Contains(path, filepath.Join("wt-b", ".aira")) {
		t.Fatalf("allocation path=%q; want rewritten to B's worktree", path)
	}
	if _, err := b.Get("FEE-BL-1"); err != nil {
		t.Fatalf("Get in B after materialise: %v", err)
	}

	// aira check is green in the importing worktree B (the shape-2 target).
	report, err := b.Check(ctx)
	if err != nil {
		t.Fatalf("B.Check: %v", err)
	}
	if report.Verdict == "fail" || report.Dimensions["allocated-id-file"] == "fail" {
		t.Fatalf("B check fail after cross-worktree materialise; findings=%+v", report.Findings)
	}

	// Probe A.Check (the stale minting worktree) BEFORE any further dangling mint:
	// the plan targets the importing worktree, but a now-materialised row must not
	// fabricate a failure in A either.
	if ra, aerr := a.Check(ctx); aerr != nil {
		t.Logf("A.Check errored (stale minting worktree; plan targets B): %v", aerr)
	} else if ra.Verdict == "fail" || ra.Dimensions["allocated-id-file"] == "fail" {
		t.Errorf("A.Check (stale minting worktree) = fail; a materialised cross-worktree row must not fabricate a finding. findings=%+v", ra.Findings)
	}

	// A subsequent mint (shared counter) does NOT re-issue BL-1.
	next, err := b.AllocateID(ctx, "BL")
	if err != nil {
		t.Fatalf("B.AllocateID: %v", err)
	}
	if next != "FEE-BL-2" {
		t.Fatalf("subsequent mint=%q; want FEE-BL-2 (no re-issue of the imported id)", next)
	}
}

// TestE2EAllocatedMaxFencepost is the --allocated-max HWM seed + the FENCEPOST
// the plan-review caught: the flag value is the LAST-allocated number, so the
// forward allocator must mint EXACTLY N+1. An off-by-one (next_number=N) mints
// 1217 and reds `first`.
func TestE2EAllocatedMaxFencepost(t *testing.T) {
	ctx := context.Background()
	s := namespacedStore(t, "FEE", "BL")

	// Import a small backlog (max BL-3), then seed from the LAST-allocated counter
	// value (1217) via --allocated-max — which EXCEEDS the backlog max because ids
	// minted on unmerged branches are not in the imported rows.
	if _, err := s.ImportTicketsBytes(ctx, []byte(strings.Join([]string{
		`{"id":"BL-1","title":"one","status":"planned","kind":"chore","severity":"P2","body":"b"}`,
		`{"id":"BL-2","title":"two","status":"planned","kind":"chore","severity":"P2","body":"b"}`,
		`{"id":"BL-3","title":"three","status":"planned","kind":"chore","severity":"P2","body":"b"}`,
	}, "\n")), true, map[string]int64{"BL": 1217}); err != nil {
		t.Fatalf("import + --allocated-max: %v", err)
	}

	first, err := s.AllocateID(ctx, "BL")
	if err != nil {
		t.Fatalf("AllocateID #1: %v", err)
	}
	if first != "FEE-BL-1218" {
		t.Fatalf("first mint after --allocated-max BL=1217 = %q; want FEE-BL-1218 (N+1 fencepost)", first)
	}
	second, err := s.AllocateID(ctx, "BL")
	if err != nil {
		t.Fatalf("AllocateID #2: %v", err)
	}
	if second != "FEE-BL-1219" {
		t.Fatalf("second mint = %q; want FEE-BL-1219", second)
	}

	// DURABILITY is an ACCEPTED, DOCUMENTED gap: the seed lives in id_counters (DB
	// only). A state.db loss + Rebuild recomputes maxima from receipts, which lack
	// unmerged-branch ids, so a bounded re-mint window exists (backstop:
	// fastest.ee's own check_no_duplicate_ids gate; re-applying --allocated-max is
	// one idempotent command). This test deliberately makes NO "never re-minted
	// across a DB loss" claim.
}

// TestE2EAllocatedMaxUnownedPrefixIsZeroWrite pins the validate-BEFORE-write
// placement: an unowned --allocated-max prefix is a hard E_IMPORT_INVALID and
// the batch's rows are NOT written (an operator typo aborts cleanly).
func TestE2EAllocatedMaxUnownedPrefixIsZeroWrite(t *testing.T) {
	ctx := context.Background()
	s := namespacedStore(t, "FEE", "BL")
	_, err := s.ImportTicketsBytes(ctx, []byte(`{"id":"BL-1","title":"one","status":"planned","kind":"chore","severity":"P2","body":"b"}`), true, map[string]int64{"ZZ": 50})
	if err == nil || !strings.Contains(err.Error(), "E_IMPORT_INVALID") {
		t.Fatalf("unowned --allocated-max prefix should be E_IMPORT_INVALID; got %v", err)
	}
	if _, gerr := s.Get("FEE-BL-1"); ErrorCode(gerr) != "E_NOT_FOUND" {
		t.Fatalf("a bad --allocated-max must be zero-write; FEE-BL-1 exists: %v", gerr)
	}
}
