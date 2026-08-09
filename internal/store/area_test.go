package store

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestOpenMigratesPreTouchAreaHintsGeneration(t *testing.T) {
	base := persistentTemp(t, "area-hints-migration")
	root := filepath.Join(base, "main")
	state := filepath.Join(base, "state")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(state, 0o755); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", filepath.Join(state, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`CREATE TABLE area_hints (project_id TEXT NOT NULL, ticket_id TEXT NOT NULL, worktree_id TEXT NOT NULL, glob TEXT NOT NULL, PRIMARY KEY(project_id,ticket_id,worktree_id,glob))`); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	s := openTestStore(t, root, filepath.Join(base, "common"), state, "main", "AIRA")
	if _, err := s.areaOverlapWarnings(context.Background()); err != nil {
		t.Fatalf("area overlap query on pre-touch database: %v", err)
	}
}

func TestAreaGlobOverlapTable(t *testing.T) {
	cases := []struct {
		left, right string
		overlap     bool
	}{
		{"src/file.go", "src/file.go", true},
		{"src/**", "src/foo/bar.go", true},
		{"src/*.go", "src/foo.go", true},
		{"src/**", "test/**", false},
		{"a/b/*", "a/*/c", true}, // a/b/c is a witness.
		{"src/*.go", "src/foo/bar.go", false},
		{"src/foo", "src/bar", false},
		{"docs/**/README.md", "docs/v1/README.md", true},
		{"a/[bc].go", "a/b.go", true},
		{"a/[bc].go", "a/d.go", false},
	}
	for _, tc := range cases {
		t.Run(tc.left+"__"+tc.right, func(t *testing.T) {
			got, err := AreaGlobsOverlap(tc.left, tc.right)
			if err != nil {
				t.Fatalf("AreaGlobsOverlap(%q, %q): %v", tc.left, tc.right, err)
			}
			if got != tc.overlap {
				t.Fatalf("AreaGlobsOverlap(%q, %q)=%v, want %v", tc.left, tc.right, got, tc.overlap)
			}
		})
	}
}

func TestNormalizeAreaGlobSortsDeduplicatesAndRejectsInvalid(t *testing.T) {
	got, err := NormalizeAreaGlobs([]string{"./z/file.go", "src/**", "src/**", "./a/b"})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"a/b", "src/**", "z/file.go"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("normalized globs=%#v, want %#v", got, want)
	}
	for _, glob := range []string{"", "../secret", "/absolute", "a/[broken", `C:\repo\secret`, `C:secret`, `a\b`} {
		if _, err := NormalizeAreaGlobs([]string{glob}); ErrorCode(err) != "E_GLOB_INVALID" {
			t.Fatalf("invalid glob %q error=%v, want E_GLOB_INVALID", glob, err)
		}
	}
}

func TestAreaWarningsForClaimMatchesExactEndpoint(t *testing.T) {
	claims := []liveAreaClaim{
		{ticketID: "AIRA-1", worktree: "w", globs: []string{"src/**"}},
		{ticketID: "XAIRA-1", worktree: "w", globs: []string{"src/**"}},
		{ticketID: "OTHER-1", worktree: "other", globs: []string{"src/file.go"}},
	}

	warnings := areaWarningsForClaim(claims, "AIRA-1", "w")
	if len(warnings) != 1 || warnings[0].Subject != "AIRA-1@w <-> OTHER-1@other" {
		t.Fatalf("warnings=%#v, want only the exact AIRA-1@w endpoint", warnings)
	}
}

func TestTouchRejectsTokenFromNonHolderWorktreeWithoutWritingHint(t *testing.T) {
	s, clock, base := m3Store(t)
	other, err := Open(context.Background(), Options{
		Root: base, CommonDir: filepath.Join(base, "common"), DBPath: filepath.Join(base, "state", "state.db"),
		RegistryPath: filepath.Join(base, "state", "registry.jsonl"), LeaseStateDir: filepath.Join(base, "other-state"),
		ProjectID: s.projectID, WorktreeID: "worktree-b", ProjectSlug: "aira", Prefixes: []string{"AIRA"}, Clock: clock, LeaseTTLNS: 900,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = other.Close() })
	ticket := m3Ticket(t, s, "holder worktree")
	claim, err := s.Claim(context.Background(), ticket.ID, false, "owner")
	if err != nil {
		t.Fatal(err)
	}

	if _, err := other.Touch(context.Background(), ticket.ID, claim.Token, []string{"src/**"}); ErrorCode(err) != "E_LEASE_TOKEN" {
		t.Fatalf("cross-worktree token error=%v, want E_LEASE_TOKEN", err)
	}
	var count int
	if err := s.db.QueryRow(`SELECT count(*) FROM area_hints WHERE project_id=? AND ticket_id=?`, s.projectID, ticket.ID).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatalf("cross-worktree Touch wrote %d orphaned hint rows", count)
	}
}

func TestTouchHintsAreBoundToLeaseGeneration(t *testing.T) {
	s, clock, base := m3Store(t)
	other, err := Open(context.Background(), Options{
		Root: base, CommonDir: filepath.Join(base, "common"), DBPath: filepath.Join(base, "state", "state.db"),
		RegistryPath: filepath.Join(base, "state", "registry.jsonl"), LeaseStateDir: filepath.Join(base, "other-state"),
		ProjectID: s.projectID, WorktreeID: "worktree-b", ProjectSlug: "aira", Prefixes: []string{"AIRA"}, Clock: clock, LeaseTTLNS: 900,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = other.Close() })
	oldOwner, newOwner := m3Ticket(t, s, "old owner"), m3Ticket(t, other, "new owner")
	oldClaim, err := s.Claim(context.Background(), oldOwner.ID, false, "old")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Touch(context.Background(), oldOwner.ID, oldClaim.Token, []string{"src/**"}); err != nil {
		t.Fatal(err)
	}
	oldHeld, _ := oldClaim.Lease.Held()
	var storedGeneration int64
	if err := s.db.QueryRow(`SELECT generation FROM area_hints WHERE project_id=? AND ticket_id=?`, s.projectID, oldOwner.ID).Scan(&storedGeneration); err != nil {
		t.Fatal(err)
	}
	if storedGeneration != int64(oldHeld.Generation()) {
		t.Fatalf("stored hint generation=%d, want %d", storedGeneration, oldHeld.Generation())
	}
	if _, err := s.Release(context.Background(), oldOwner.ID, oldClaim.Token); err != nil {
		t.Fatal(err)
	}
	clock.mono = 200
	newClaim, err := s.Claim(context.Background(), oldOwner.ID, false, "new")
	if err != nil {
		t.Fatal(err)
	}
	newHeld, _ := newClaim.Lease.Held()
	if newHeld.Generation() == oldHeld.Generation() {
		t.Fatalf("reclaim reused lease generation %d", newHeld.Generation())
	}

	otherClaim, err := other.Claim(context.Background(), newOwner.ID, false, "other")
	if err != nil {
		t.Fatal(err)
	}
	result, err := other.Touch(context.Background(), newOwner.ID, otherClaim.Token, []string{"src/main.go"})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Warnings) != 0 {
		t.Fatalf("old-generation hint revived after reclaim: %#v", result.Warnings)
	}

	if _, err := s.Touch(context.Background(), oldOwner.ID, newClaim.Token, []string{"src/**"}); err != nil {
		t.Fatal(err)
	}
	result, err = other.Touch(context.Background(), newOwner.ID, otherClaim.Token, []string{"src/main.go"})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Warnings) != 1 || result.Warnings[0].Code != "W_AREA_OVERLAP" {
		t.Fatalf("current-generation overlap warnings=%#v", result.Warnings)
	}
}

func TestTouchAuthenticatesHolderReplacesHintsAndDoesNotExtendLease(t *testing.T) {
	s, clock, _ := m3Store(t)
	ticket := m3Ticket(t, s, "touch")
	claim, err := s.Claim(context.Background(), ticket.ID, false, "owner")
	if err != nil {
		t.Fatal(err)
	}
	before, err := s.GetLease(context.Background(), ticket.ID)
	if err != nil {
		t.Fatal(err)
	}
	var beforeSeq int64
	if err := s.db.QueryRow(`SELECT next_seq FROM event_counters WHERE project_id=?`, s.projectID).Scan(&beforeSeq); err != nil {
		t.Fatal(err)
	}
	journalPath := filepath.Join(s.commonDir, "aira", "journal.jsonl")
	journalBefore, err := os.ReadFile(journalPath)
	if err != nil {
		t.Fatal(err)
	}

	if _, err := s.Touch(context.Background(), ticket.ID, "wrong", []string{"src/**"}); ErrorCode(err) != "E_LEASE_TOKEN" {
		t.Fatalf("wrong token error=%v, want E_LEASE_TOKEN", err)
	}
	clock.mono = 200
	result, err := s.Touch(context.Background(), ticket.ID, claim.Token, []string{"./z/file.go", "src/**", "src/**"})
	if err != nil {
		t.Fatalf("touch: %v", err)
	}
	if !reflect.DeepEqual(result.Hints, []string{"src/**", "z/file.go"}) {
		t.Fatalf("touch hints=%#v", result.Hints)
	}
	after, err := s.GetLease(context.Background(), ticket.ID)
	if err != nil {
		t.Fatal(err)
	}
	beforeHeld, _ := before.Held()
	afterHeld, _ := after.Held()
	if afterHeld.LastHeartbeatMonoNS() != beforeHeld.LastHeartbeatMonoNS() {
		t.Fatalf("touch extended lease: before=%d after=%d", beforeHeld.LastHeartbeatMonoNS(), afterHeld.LastHeartbeatMonoNS())
	}
	var afterSeq int64
	if err := s.db.QueryRow(`SELECT next_seq FROM event_counters WHERE project_id=?`, s.projectID).Scan(&afterSeq); err != nil {
		t.Fatal(err)
	}
	journalAfter, err := os.ReadFile(journalPath)
	if err != nil {
		t.Fatal(err)
	}
	if afterSeq != beforeSeq || string(journalAfter) != string(journalBefore) {
		t.Fatalf("touch journal side effect: seq %d->%d journal changed=%v", beforeSeq, afterSeq, string(journalAfter) != string(journalBefore))
	}

	if _, err := s.Touch(context.Background(), ticket.ID, claim.Token, []string{"a/*", "a/*"}); err != nil {
		t.Fatal(err)
	}
	cleared, err := s.Touch(context.Background(), ticket.ID, claim.Token, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(cleared.Hints) != 0 {
		t.Fatalf("empty touch did not clear hints: %#v", cleared.Hints)
	}
	var count int
	if err := s.db.QueryRow(`SELECT count(*) FROM area_hints WHERE project_id=? AND ticket_id=?`, s.projectID, ticket.ID).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatalf("area hint rows after clear=%d, want 0", count)
	}
}

func TestTouchRequiresLiveLeaseAndExpiredHintsAreIgnored(t *testing.T) {
	s, clock, base := m3Store(t)
	other, err := Open(context.Background(), Options{
		Root: base, CommonDir: filepath.Join(base, "common"), DBPath: filepath.Join(base, "state", "state.db"),
		RegistryPath: filepath.Join(base, "state", "registry.jsonl"), LeaseStateDir: filepath.Join(base, "lease-state"),
		ProjectID: s.projectID, WorktreeID: "worktree-b", ProjectSlug: "aira", Prefixes: []string{"AIRA"}, Clock: clock, LeaseTTLNS: 900,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = other.Close() })
	first, second := m3Ticket(t, s, "expired owner"), m3Ticket(t, s, "live owner")
	firstClaim, err := s.Claim(context.Background(), first.ID, false, "first")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Touch(context.Background(), first.ID, firstClaim.Token, []string{"src/**"}); err != nil {
		t.Fatal(err)
	}
	clock.mono += 900
	if _, err := s.Touch(context.Background(), first.ID, firstClaim.Token, []string{"src/**"}); ErrorCode(err) != "E_LEASE_EXPIRED" {
		t.Fatalf("expired touch error=%v, want E_LEASE_EXPIRED", err)
	}
	secondClaim, err := other.Claim(context.Background(), second.ID, false, "second")
	if err != nil {
		t.Fatal(err)
	}
	result, err := other.Touch(context.Background(), second.ID, secondClaim.Token, []string{"src/main.go"})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Warnings) != 0 {
		t.Fatalf("expired hint produced overlap warnings: %#v", result.Warnings)
	}

	// The schema round-trips the owning worktree as part of the key.
	var worktree, glob string
	if err := s.db.QueryRow(`SELECT worktree_id, glob FROM area_hints WHERE project_id=? AND ticket_id=?`, s.projectID, second.ID).Scan(&worktree, &glob); !errors.Is(err, sql.ErrNoRows) {
		// second's hint is owned by the other store, so query through its DB view.
		if err != nil {
			t.Fatal(err)
		}
	}
	if err := other.db.QueryRow(`SELECT worktree_id, glob FROM area_hints WHERE project_id=? AND ticket_id=?`, other.projectID, second.ID).Scan(&worktree, &glob); err != nil {
		t.Fatal(err)
	}
	if worktree != "worktree-b" || glob != "src/main.go" {
		t.Fatalf("area_hints row=(%q,%q)", worktree, glob)
	}
}

func TestTouchOverlapWarnsAcrossWorktreesButNotWithinOne(t *testing.T) {
	s, clock, base := m3Store(t)
	other, err := Open(context.Background(), Options{
		Root: base, CommonDir: filepath.Join(base, "common"), DBPath: filepath.Join(base, "state", "state.db"),
		RegistryPath: filepath.Join(base, "state", "registry.jsonl"), LeaseStateDir: filepath.Join(base, "other-state"),
		ProjectID: s.projectID, WorktreeID: "worktree-b", ProjectSlug: "aira", Prefixes: []string{"AIRA"}, Clock: clock, LeaseTTLNS: 900,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = other.Close() })
	one, two := m3Ticket(t, s, "one"), m3Ticket(t, s, "two")
	claimOne, err := s.Claim(context.Background(), one.ID, false, "one")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Touch(context.Background(), one.ID, claimOne.Token, []string{"src/**"}); err != nil {
		t.Fatal(err)
	}
	claimTwo, err := other.Claim(context.Background(), two.ID, false, "two")
	if err != nil {
		t.Fatal(err)
	}
	result, err := other.Touch(context.Background(), two.ID, claimTwo.Token, []string{"src/foo.go"})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Warnings) != 1 || result.Warnings[0].Code != "W_AREA_OVERLAP" {
		t.Fatalf("cross-worktree warnings=%#v", result.Warnings)
	}
	warning := result.Warnings[0]
	for _, want := range []string{one.ID, "worktree-a", two.ID, "worktree-b", "src/**", "src/foo.go"} {
		if !strings.Contains(warning.Subject, want) && !strings.Contains(warning.Message, want) {
			t.Fatalf("warning=%#v does not name %q", warning, want)
		}
	}

	// A same-worktree claim pair is not a collision.
	third := m3Ticket(t, s, "three")
	claimThree, err := s.Claim(context.Background(), third.ID, false, "three")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := other.Touch(context.Background(), two.ID, claimTwo.Token, nil); err != nil {
		t.Fatal(err)
	}
	result, err = s.Touch(context.Background(), third.ID, claimThree.Token, []string{"src/foo.go"})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Warnings) != 0 {
		t.Fatalf("same-worktree warnings=%#v", result.Warnings)
	}
}

func TestCheckReportsAreaOverlapAsWarningOnly(t *testing.T) {
	s, clock, base := m3Store(t)
	other, err := Open(context.Background(), Options{
		Root: base, CommonDir: filepath.Join(base, "common"), DBPath: filepath.Join(base, "state", "state.db"),
		RegistryPath: filepath.Join(base, "state", "registry.jsonl"), LeaseStateDir: filepath.Join(base, "other-state"),
		ProjectID: s.projectID, WorktreeID: "worktree-b", ProjectSlug: "aira", Prefixes: []string{"AIRA"}, Clock: clock, LeaseTTLNS: 900,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = other.Close() })
	one, two := m3Ticket(t, s, "check one"), m3Ticket(t, s, "check two")
	claimOne, _ := s.Claim(context.Background(), one.ID, false, "one")
	claimTwo, _ := other.Claim(context.Background(), two.ID, false, "two")
	if _, err := s.Touch(context.Background(), one.ID, claimOne.Token, []string{"pkg/**"}); err != nil {
		t.Fatal(err)
	}
	if _, err := other.Touch(context.Background(), two.ID, claimTwo.Token, []string{"pkg/a.go"}); err != nil {
		t.Fatal(err)
	}
	report, err := s.Check(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if report.Verdict != "pass" || report.Dimensions["area-overlap"] != "warning" {
		t.Fatalf("overlap check report=%#v", report)
	}
	found := false
	for _, warning := range report.Warnings {
		if warning.Code == "W_AREA_OVERLAP" {
			found = true
		}
	}
	if !found {
		t.Fatalf("overlap check warnings=%#v", report.Warnings)
	}
}

func TestCheckMarksAreaOverlapUnevaluatedWhenClockUnavailable(t *testing.T) {
	s, clock, _ := m3Store(t)
	m3Ticket(t, s, "clock unavailable area")
	clock.err = errors.New("no monotonic clock")

	report, err := s.Check(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if report.Dimensions["area-overlap"] != "unevaluated" {
		t.Fatalf("area-overlap dimension=%q, want unevaluated; report=%#v", report.Dimensions["area-overlap"], report)
	}
	found := false
	for _, finding := range report.UnevaluatedFindings {
		if finding.Code == "E_CLOCK_UNAVAILABLE" && finding.Subject == "area-overlap" {
			found = true
		}
	}
	if !found {
		t.Fatalf("missing area-overlap clock finding: %#v", report.UnevaluatedFindings)
	}
}
