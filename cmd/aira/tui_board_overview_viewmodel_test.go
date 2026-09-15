package main

import (
	"strings"
	"testing"

	"aira/internal/daemon"
	"aira/internal/runner"
	"aira/internal/store"
)

// entry builds one registry line.
func ovEntry(projectID, worktreeID, root, commonDir string, prefixes ...string) store.RegistryEntry {
	return store.RegistryEntry{ProjectID: projectID, WorktreeID: worktreeID, Root: root, CommonDir: commonDir, Prefixes: prefixes}
}

func allRootsLive(string) bool { return true }

func findGroup(t *testing.T, groups []overviewGroup, projectID string) overviewGroup {
	t.Helper()
	for _, group := range groups {
		if group.ProjectID == projectID {
			return group
		}
	}
	t.Fatalf("group %q not found in %d groups", projectID, len(groups))
	return overviewGroup{}
}

// TestOverviewDedupByWorktree: the append-only registry duplicates a line per
// cold scope; the same worktree_id must collapse to one, not inflate the count.
func TestOverviewDedupByWorktree(t *testing.T) {
	entries := []store.RegistryEntry{
		ovEntry("P1", "wtA", "/repo/main", "/repo/main/.git", "AIRA"),
		ovEntry("P1", "wtA", "/repo/main", "/repo/main/.git", "AIRA"), // duplicate breadcrumb
	}
	groups := overviewGroupProjects(entries, allRootsLive)
	if len(groups) != 1 {
		t.Fatalf("dedup: want 1 group, got %d", len(groups))
	}
	group := groups[0]
	if group.LiveRootCount != 1 || len(group.WorktreeIDs) != 1 {
		// Mutation guard: dropping the seen[worktree_id] dedup makes this 2.
		t.Fatalf("dedup: LiveRootCount=%d worktrees=%v, want 1 / one", group.LiveRootCount, group.WorktreeIDs)
	}
}

// TestOverviewGroupByProject: distinct worktrees of ONE project group into one
// card carrying every worktree; distinct projects stay distinct.
func TestOverviewGroupByProject(t *testing.T) {
	entries := []store.RegistryEntry{
		ovEntry("P1", "wtMain", "/p1/main", "/p1/main/.git"),
		ovEntry("P1", "wtFeat", "/p1/feat", "/p1/main/.git"), // linked worktree of P1
		ovEntry("P2", "wtP2", "/p2/main", "/p2/main/.git"),
	}
	groups := overviewGroupProjects(entries, allRootsLive)
	if len(groups) != 2 {
		t.Fatalf("group-by-project: want 2 groups, got %d", len(groups))
	}
	p1 := findGroup(t, groups, "P1")
	if p1.LiveRootCount != 2 || len(p1.WorktreeIDs) != 2 {
		// Mutation guard: grouping by worktree instead of project makes P1 two groups.
		t.Fatalf("group-by-project: P1 LiveRootCount=%d worktrees=%v, want 2", p1.LiveRootCount, p1.WorktreeIDs)
	}
}

// TestOverviewDeadRootSkip: an entry whose root no longer exists is dropped —
// but its live siblings, and other projects, remain (never over-drop).
func TestOverviewDeadRootSkip(t *testing.T) {
	entries := []store.RegistryEntry{
		ovEntry("P1", "wtDead", "/p1/gone", "/p1/gone/.git"),
		ovEntry("P1", "wtLive", "/p1/live", "/p1/live/.git"),
		ovEntry("P2", "wtP2", "/p2/only", "/p2/only/.git"),
	}
	rootExists := func(root string) bool { return root != "/p1/gone" }
	groups := overviewGroupProjects(entries, rootExists)
	if len(groups) != 2 {
		t.Fatalf("dead-root: want 2 groups, got %d", len(groups))
	}
	p1 := findGroup(t, groups, "P1")
	if p1.LiveRootCount != 1 || p1.Canonical.Root != "/p1/live" {
		// Mutation guard: skipping the rootExists filter keeps the dead root.
		t.Fatalf("dead-root: P1 live=%d canonical=%q, want 1 / /p1/live", p1.LiveRootCount, p1.Canonical.Root)
	}
}

// TestOverviewCanonicalMainCheckout: the main checkout (common-dir is
// <root>/.git) is picked as canonical even when a linked worktree sorts first.
func TestOverviewCanonicalMainCheckout(t *testing.T) {
	entries := []store.RegistryEntry{
		// "/aaa/linked" sorts before "/zzz/main" but is a LINKED worktree.
		ovEntry("P1", "wtLinked", "/aaa/linked", "/zzz/main/.git"),
		ovEntry("P1", "wtMain", "/zzz/main", "/zzz/main/.git"),
	}
	groups := overviewGroupProjects(entries, allRootsLive)
	p1 := findGroup(t, groups, "P1")
	if !p1.CanonicalIsMain || p1.Canonical.Root != "/zzz/main" {
		// Mutation guard: picking members[0] blindly (dropping the main-checkout
		// search) would choose the linked "/aaa/linked".
		t.Fatalf("canonical: IsMain=%v root=%q, want true / /zzz/main", p1.CanonicalIsMain, p1.Canonical.Root)
	}
}

// TestOverviewCanonicalFallbackLabelled: with NO main checkout, the first live
// root is chosen but LABELLED not-main, so the disclosure is honest.
func TestOverviewCanonicalFallbackLabelled(t *testing.T) {
	entries := []store.RegistryEntry{
		ovEntry("P1", "wtA", "/p1/wtA", "/elsewhere/.git"),
		ovEntry("P1", "wtB", "/p1/wtB", "/elsewhere/.git"),
	}
	groups := overviewGroupProjects(entries, allRootsLive)
	p1 := findGroup(t, groups, "P1")
	if p1.CanonicalIsMain {
		t.Fatalf("canonical fallback: IsMain=true, want false (no main checkout)")
	}
	if p1.Canonical.Root != "/p1/wtA" { // deterministic: sorted by root
		t.Fatalf("canonical fallback: root=%q, want the first sorted /p1/wtA", p1.Canonical.Root)
	}
	card := buildOneCard(t, p1, overviewDiscovery{Slug: "p1", Prefixes: []string{"AIRA"}})
	if line := overviewCheckoutText(card); !strings.Contains(line, "no main checkout") {
		t.Fatalf("canonical fallback disclosure missing: %q", line)
	}
}

func buildOneCard(t *testing.T, group overviewGroup, discovery overviewDiscovery) overviewCard {
	t.Helper()
	data := overviewListData{
		Groups:      []overviewGroup{group},
		Discoveries: map[string]overviewDiscovery{group.ProjectID: discovery},
		Scopes:      map[string]daemon.WorktreeScope{group.ProjectID: {ProjectID: group.ProjectID, Slug: discovery.Slug}},
	}
	cards := buildOverviewCards(data)
	if len(cards) != 1 {
		t.Fatalf("buildOneCard: want 1 card, got %d", len(cards))
	}
	return cards[0]
}

// TestOverviewStateAvailable: a discovered project reads available and carries a
// dispatchable scope.
func TestOverviewStateAvailable(t *testing.T) {
	group := overviewGroupProjects([]store.RegistryEntry{ovEntry("P1", "wt", "/p1", "/p1/.git", "AIRA")}, allRootsLive)[0]
	card := buildOneCard(t, group, overviewDiscovery{Slug: "myproj", Prefixes: []string{"AIRA"}})
	if card.State != "available" || card.Slug != "myproj" || !card.HasScope {
		t.Fatalf("available: state=%q slug=%q hasScope=%v", card.State, card.Slug, card.HasScope)
	}
	// A not-yet-loaded card shows "…", NEVER a fabricated "0 tickets" (§14).
	if label := overviewStateLabel(card, overviewCardData{}, false); !strings.Contains(label, "…") {
		t.Fatalf("loading state: want '…', got %q", label)
	}
	if label := overviewStateLabel(card, overviewCardData{}, false); strings.Contains(label, "0 tickets") {
		// Mutation guard: treating absent data as an empty distribution fabricates "0".
		t.Fatalf("loading state fabricated a zero distribution: %q", label)
	}
}

// TestOverviewStateUnavailableNotDropped: a project whose Discover fails is shown
// with its error code, NEVER silently dropped (spec §11.5, §14).
func TestOverviewStateUnavailableNotDropped(t *testing.T) {
	data := overviewListData{
		Groups:      overviewGroupProjects([]store.RegistryEntry{ovEntry("P1", "wt", "/p1", "/p1/.git")}, allRootsLive),
		Discoveries: map[string]overviewDiscovery{"P1": {Code: "E_CONFIG_INVALID"}},
	}
	cards := buildOverviewCards(data)
	if len(cards) != 1 {
		// Mutation guard: dropping unavailable projects makes this 0.
		t.Fatalf("unavailable: want the project shown (1 card), got %d", len(cards))
	}
	if cards[0].State != "unavailable" || cards[0].StateCode != "E_CONFIG_INVALID" {
		t.Fatalf("unavailable: state=%q code=%q", cards[0].State, cards[0].StateCode)
	}
	if label := overviewStateLabel(cards[0], overviewCardData{}, false); !strings.Contains(label, "unavailable (E_CONFIG_INVALID)") {
		t.Fatalf("unavailable label=%q", label)
	}
}

// TestOverviewStateEjected: an ejected project (Discover still succeeds — its
// .aira/config persists — but the dispatch returns E_NOT_ADOPTED) reads
// "ejected", not "0 tickets" (spec §11.5).
func TestOverviewStateEjected(t *testing.T) {
	group := overviewGroupProjects([]store.RegistryEntry{ovEntry("P1", "wt", "/p1", "/p1/.git")}, allRootsLive)[0]
	card := buildOneCard(t, group, overviewDiscovery{Slug: "ejectedproj"})
	label := overviewStateLabel(card, overviewCardData{Loaded: true, Ejected: true}, true)
	if label != "ejected" {
		t.Fatalf("ejected label=%q, want 'ejected'", label)
	}
	if strings.Contains(label, "0") {
		t.Fatalf("ejected fabricated a count: %q", label)
	}
}

// TestOverviewStateUnevaluatedNotZero: a non-ejected dispatch failure reads
// unevaluated, never a fabricated "0 tickets" (§14).
func TestOverviewStateUnevaluatedNotZero(t *testing.T) {
	group := overviewGroupProjects([]store.RegistryEntry{ovEntry("P1", "wt", "/p1", "/p1/.git")}, allRootsLive)[0]
	card := buildOneCard(t, group, overviewDiscovery{Slug: "p1"})
	label := overviewStateLabel(card, overviewCardData{Loaded: true, Code: "E_TUI_DECODE"}, true)
	if !strings.Contains(label, "unevaluated (E_TUI_DECODE)") {
		t.Fatalf("unevaluated label=%q", label)
	}
}

// TestOverviewLoadedDistribution: once loaded, the distribution renders in
// canonical order, omitting zero buckets.
func TestOverviewLoadedDistribution(t *testing.T) {
	group := overviewGroupProjects([]store.RegistryEntry{ovEntry("P1", "wt", "/p1", "/p1/.git")}, allRootsLive)[0]
	card := buildOneCard(t, group, overviewDiscovery{Slug: "p1"})
	data := overviewCardData{Loaded: true, Distribution: map[string]int{"planned": 3, "done": 5}, Total: 8, LeaseKnown: true, LeaseCount: 2}
	label := overviewStateLabel(card, data, true)
	if !strings.Contains(label, "planned:3") || !strings.Contains(label, "done:5") {
		t.Fatalf("distribution label=%q", label)
	}
	if strings.Contains(label, "draft:0") {
		t.Fatalf("distribution showed a zero bucket: %q", label)
	}
	if activity := overviewActivityText(card, data, true); activity != "act 2" {
		t.Fatalf("activity=%q, want 'act 2' (2 leases + 0 owned jobs)", activity)
	}
}

// TestOverviewActivityCountsOwnedJobs: an attested confine owner adds to the
// project's activity; a non-attested owner does NOT (spec §9).
func TestOverviewActivityOwnedJobs(t *testing.T) {
	mine := strings.Repeat("a", 64)  // a 64-hex worktree id, this project, attested
	other := strings.Repeat("b", 64) // an attested id of ANOTHER project (not owned)
	confine := &runner.ConfineListResult{Verdict: "ok", Scopes: []runner.ConfineRecord{
		{Name: "j1", Owner: mine},           // owned + attested → counts
		{Name: "j2", Owner: other},          // attested but another project → must NOT count
		{Name: "j3", Owner: "stoner-task5"}, // a label — not attested → must NOT count
	}}
	got := overviewOwnedJobs([]string{mine}, confine)
	if got != 1 {
		// Mutation guard: dropping the owned-set check counts j2 (a foreign project's
		// job) → 2; dropping the attested grading counts j3 too.
		t.Fatalf("owned jobs=%d, want 1 (only this project's attested job)", got)
	}
}

// TestOverviewCardLineEscapes: a slug/prefix carrying tview tag syntax renders
// literally, never as a colour tag (§14).
func TestOverviewCardLineEscapes(t *testing.T) {
	card := overviewCard{Slug: "[red]evil", Prefixes: []string{"P[x]"}, State: "available", CanonicalIsMain: true}
	line := overviewCardLine(card, overviewCardData{}, false)
	if !strings.Contains(line, "[red[]evil") { // tview.Escape inserts the "[]" escape
		t.Fatalf("slug not escaped: %q", line)
	}
	if !strings.Contains(line, "P[x[]") {
		t.Fatalf("prefix not escaped: %q", line)
	}
}

// TestOverviewMatchesFiltersBySlugPrefix: the overview search filters by slug and
// prefix only (client-side), never dispatching.
func TestOverviewMatchesFiltersBySlugPrefix(t *testing.T) {
	card := overviewCard{Slug: "aira", Prefixes: []string{"AIRA", "RANT"}, ProjectID: "abc123"}
	if !overviewMatches(card, "air") || !overviewMatches(card, "rant") || !overviewMatches(card, "abc1") {
		t.Fatalf("overviewMatches should match slug/prefix/projectID fragments")
	}
	if overviewMatches(card, "zzz") {
		t.Fatalf("overviewMatches matched a non-substring")
	}
	if !overviewMatches(card, "") {
		t.Fatalf("empty query should match all")
	}
}

// TestOverviewJobsRowsUnevaluated: a nil / failed / unevaluated confine read is
// disclosed, never silently empty; nil Command/SupervisorLive render unevaluated.
func TestOverviewJobsRowsHonesty(t *testing.T) {
	if rows := overviewJobsRows(nil, "E_TUI_DECODE", nil); len(rows) != 1 || rows[0].Style != "unevaluated" {
		t.Fatalf("failed confine: rows=%v", rows)
	}
	if rows := overviewJobsRows(nil, "", nil); rows[0].Style != "unevaluated" {
		t.Fatalf("nil confine should be unevaluated, got %v", rows[0])
	}
	unevaluated := &runner.ConfineListResult{Verdict: "unevaluated", Reason: "no slice"}
	if rows := overviewJobsRows(unevaluated, "", nil); !strings.Contains(rows[0].Text, "unevaluated") {
		t.Fatalf("unevaluated verdict: %v", rows[0])
	}
	// A job with nil Command renders "unevaluated", never a blank command.
	live := &runner.ConfineListResult{Verdict: "ok", Scopes: []runner.ConfineRecord{{Name: "j1", Owner: "unknown"}}}
	rows := overviewJobsRows(live, "", nil)
	if !strings.Contains(rows[0].Text, "unevaluated") {
		t.Fatalf("nil command should render unevaluated: %q", rows[0].Text)
	}
	// Empty & successful → the honest empty state, not a fabricated row.
	empty := &runner.ConfineListResult{Verdict: "ok"}
	if rows := overviewJobsRows(empty, "", nil); rows[0].Text != "no running jobs" {
		t.Fatalf("empty confine: %q", rows[0].Text)
	}
}
