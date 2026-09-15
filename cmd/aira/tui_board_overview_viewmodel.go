package main

// AIRA-252 `aira board` — the all-projects overview view-model (spec §11,
// Increment 2).
//
// Everything in this file is PURE. The overview is assembled in the UI layer
// from a pure-read registry file plus per-project app.Discover / dispatched
// reads — never a cross-project core verb (spec §2, §11, §19). The honesty core
// lives here: every registry project is shown WITH ITS STATE (available /
// ejected / unavailable), never silently dropped (§11.5, §14); a project whose
// lazy count/lease read has not arrived shows "…", never a fabricated "0"
// distribution (§14).

import (
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"aira/internal/daemon"
	"aira/internal/domain"
	"aira/internal/runner"
	"aira/internal/store"

	"github.com/rivo/tview"
)

// overviewGroup is one project's registry footprint after dedup-by-worktree and
// group-by-project: the canonical worktree whose card is shown, whether it is
// the real main checkout, the live-root count (disclosure), and every live
// worktree id (for owned-job correlation).
type overviewGroup struct {
	ProjectID       string
	Canonical       store.RegistryEntry
	CanonicalIsMain bool
	LiveRootCount   int
	WorktreeIDs     []string
}

// overviewIsMainCheckout reports whether a registry entry is a project's MAIN
// checkout: its git common-dir belongs to its own root (a main checkout's
// common-dir is "<root>/.git"; a linked worktree's points into the main repo,
// so the parent of its common-dir is NOT its own root). Spec §11.4.
func overviewIsMainCheckout(entry store.RegistryEntry) bool {
	if entry.CommonDir == "" || entry.Root == "" {
		return false
	}
	return filepath.Dir(filepath.Clean(entry.CommonDir)) == filepath.Clean(entry.Root)
}

// overviewGroupProjects turns the raw (append-only, per-cold-scope-duplicated)
// registry into the deduped, grouped, dead-root-skipped project set (spec
// §11.2–§11.4). rootExists is injected so the pure logic is tested without a
// filesystem; the live fetch passes an os.Stat-based predicate.
//
//   - DEDUP by WorktreeID (breadcrumbs append identical lines per cold scope),
//     first-seen wins; entries with an empty worktree id or root are skipped.
//   - DROP entries whose Root no longer exists (os.Stat false).
//   - GROUP by ProjectID.
//   - Per group pick the CANONICAL worktree: the main checkout, else the first
//     live root (deterministic: members are sorted by root), labelled not-main.
func overviewGroupProjects(entries []store.RegistryEntry, rootExists func(string) bool) []overviewGroup {
	seen := map[string]bool{}
	var live []store.RegistryEntry
	for _, entry := range entries {
		if entry.WorktreeID == "" || entry.Root == "" {
			continue
		}
		if seen[entry.WorktreeID] {
			continue
		}
		seen[entry.WorktreeID] = true
		if rootExists != nil && !rootExists(entry.Root) {
			continue
		}
		live = append(live, entry)
	}

	order := make([]string, 0)
	byProject := map[string][]store.RegistryEntry{}
	for _, entry := range live {
		if _, ok := byProject[entry.ProjectID]; !ok {
			order = append(order, entry.ProjectID)
		}
		byProject[entry.ProjectID] = append(byProject[entry.ProjectID], entry)
	}

	groups := make([]overviewGroup, 0, len(order))
	for _, projectID := range order {
		members := byProject[projectID]
		sort.Slice(members, func(i, j int) bool { return members[i].Root < members[j].Root })
		group := overviewGroup{ProjectID: projectID, LiveRootCount: len(members)}
		for _, entry := range members {
			group.WorktreeIDs = append(group.WorktreeIDs, entry.WorktreeID)
		}
		canonical := -1
		for i, entry := range members {
			if overviewIsMainCheckout(entry) {
				canonical = i
				break
			}
		}
		if canonical >= 0 {
			group.Canonical, group.CanonicalIsMain = members[canonical], true
		} else {
			group.Canonical, group.CanonicalIsMain = members[0], false
		}
		groups = append(groups, group)
	}
	return groups
}

// overviewDiscovery is the outcome of app.Discover for one project's canonical
// root: slug on success, or an error code (E_CONFIG_MISSING / E_CONFIG_INVALID
// / E_NOT_PROJECT / …) making the project UNAVAILABLE — never dropped (§11.5).
type overviewDiscovery struct {
	Slug     string
	Prefixes []string
	Code     string // "" == discovered (available); else the unavailable reason
}

// overviewListData is the raw, impure list fetch (registry + Discover-all +
// machine-wide confine). It is turned into display cards by buildOverviewCards.
type overviewListData struct {
	Groups       []overviewGroup
	Discoveries  map[string]overviewDiscovery // by ProjectID
	Scopes       map[string]daemon.WorktreeScope
	Confine      *runner.ConfineListResult
	ConfineCode  string
	RegistryCode string // registry read failed (E_CONFIG_INVALID / io error) — the only whole-list error
}

// overviewIdentityMismatchCode marks a card whose canonical root, re-Discovered
// at card-fetch time, now hashes to a DIFFERENT project id than the registry
// entry the card was built from (the repo's git common-dir moved). The daemon's
// own registry pass treats this as a skip; the overview mirrors it by rendering
// the card unevaluated rather than attributing a foreign project's counts to it.
//
// It is E_TUI_-namespaced (like E_TUI_DECODE, its sibling in the same card
// Code field) because the TUI synthesises it locally: it is never a
// Response.Code, never crosses the daemon/MCP wire, and never maps to a process
// exit, so it is excused from the exit-code catalogue in internal/codes rather
// than published as part of the exit contract. The namespace keeps that excuse
// scoped to this surface (see producedNotCatalogued).
const overviewIdentityMismatchCode = "E_TUI_PROJECT_IDENTITY"

// overviewCard is one project's static display skeleton. The DYNAMIC per-card
// count/lease data lives separately in the reducer (overviewCardData) so it
// survives a list refresh; the render composes the two.
type overviewCard struct {
	ProjectID       string
	Slug            string
	Prefixes        []string
	CanonicalRoot   string
	CanonicalIsMain bool
	LiveRootCount   int
	State           string // "available" | "unavailable"
	StateCode       string // discover error code when unavailable
	OwnedJobs       int    // confine jobs owned by this project's worktrees (§9)
	Scope           daemon.WorktreeScope
	HasScope        bool
}

// overviewOwnedJobs counts confine jobs owned by any of the project's worktree
// ids. Correlation is per spec §9: ONLY an ATTESTED owner (a 64-hex worktree
// id) correlates; a label / @cwd-inferred / unknown owner is left uncorrelated
// (rendered in the jobs strip, never attributed to a card).
func overviewOwnedJobs(worktreeIDs []string, confine *runner.ConfineListResult) int {
	if confine == nil {
		return 0
	}
	owned := map[string]bool{}
	for _, id := range worktreeIDs {
		owned[id] = true
	}
	count := 0
	for _, record := range confine.Scopes {
		if runner.ConfineOwnerIsAttested(record.Owner) && owned[record.Owner] {
			count++
		}
	}
	return count
}

// buildOverviewCards is the pure list-model builder. Every group becomes a card
// carrying its Discover-derived state (available / unavailable+code) — none is
// dropped (§11.5). Cards are sorted by slug (then root) for a stable display.
func buildOverviewCards(data overviewListData) []overviewCard {
	cards := make([]overviewCard, 0, len(data.Groups))
	for _, group := range data.Groups {
		card := overviewCard{
			ProjectID:       group.ProjectID,
			CanonicalRoot:   group.Canonical.Root,
			CanonicalIsMain: group.CanonicalIsMain,
			LiveRootCount:   group.LiveRootCount,
			Prefixes:        group.Canonical.Prefixes,
			OwnedJobs:       overviewOwnedJobs(group.WorktreeIDs, data.Confine),
		}
		discovery := data.Discoveries[group.ProjectID]
		if discovery.Code != "" {
			card.State, card.StateCode = "unavailable", discovery.Code
			card.Slug = "(" + shortProjectID(group.ProjectID) + ")"
		} else {
			card.State = "available"
			card.Slug = discovery.Slug
			if len(discovery.Prefixes) > 0 {
				card.Prefixes = discovery.Prefixes
			}
			if scope, ok := data.Scopes[group.ProjectID]; ok {
				card.Scope, card.HasScope = scope, true
			}
		}
		if card.Slug == "" {
			card.Slug = "(" + shortProjectID(group.ProjectID) + ")"
		}
		cards = append(cards, card)
	}
	sort.SliceStable(cards, func(i, j int) bool {
		if left, right := strings.ToLower(cards[i].Slug), strings.ToLower(cards[j].Slug); left != right {
			return left < right
		}
		return cards[i].CanonicalRoot < cards[j].CanonicalRoot
	})
	return cards
}

// overviewProjectForRoot resolves a requested canonical root back to the card's
// registry ProjectID, so a lazy per-card result is always keyed by the display
// id — never by a fresh Discover id that may be empty (card-time failure) or
// disagree with the registry (P1 review fix). Returns false if the card vanished.
func overviewProjectForRoot(cards []overviewCard, root string) (string, bool) {
	for _, card := range cards {
		if card.CanonicalRoot == root {
			return card.ProjectID, true
		}
	}
	return "", false
}

// shortProjectID is a stable human fragment of a project id hash for a project
// with no readable slug (unavailable). It never fabricates a name.
func shortProjectID(projectID string) string {
	if len(projectID) > 12 {
		return projectID[:12]
	}
	if projectID == "" {
		return "unknown project"
	}
	return projectID
}

// overviewCardData is the reducer's per-card DYNAMIC state (spec §11.6): the
// lazily-dispatched count distribution and lease/activity, plus the honest
// loading / ejected / unevaluated lifecycle. A card with Loaded==false shows
// "…", never a fabricated "0" distribution (§14).
type overviewCardData struct {
	Loaded       bool
	Ejected      bool   // dispatch returned E_NOT_ADOPTED (registry persists, §11.5)
	Code         string // a non-ejected read failure → unevaluated
	Distribution map[string]int
	Total        int
	Stale        bool // the count reply carried W_STALE_INDEX (reconcile pending, §14)
	LeaseCount   int
	LeaseKnown   bool
	LeaseCode    string
}

// overviewStateLabel is the honest per-card state/summary line, composing the
// static Discover state with the dynamic lazy data. It NEVER prints a
// distribution or activity the fetch has not returned (§14).
func overviewStateLabel(card overviewCard, data overviewCardData, present bool) string {
	if card.State == "unavailable" {
		return "unavailable (" + card.StateCode + ")"
	}
	if !present || !data.Loaded {
		return "available · …"
	}
	if data.Ejected {
		return "ejected"
	}
	if data.Code != "" {
		return "unevaluated (" + data.Code + ")"
	}
	label := "available · " + strconv.Itoa(data.Total) + " tickets"
	if breakdown := overviewDistributionText(data.Distribution); breakdown != "" {
		label += " · " + breakdown
	}
	if data.Stale {
		// Reconcile pending: the count is read from canonical files and is correct,
		// but the derived index is behind (spec §14). Disclose it, never silently.
		label += " · stale"
	}
	return label
}

// overviewDistributionText renders the status distribution in canonical column
// order, omitting zero buckets, so the summary matches the board's columns. It
// returns "" for an empty distribution — the "N tickets" total already covers a
// zero-ticket project, so no fabricated "0 tickets" breakdown is emitted.
func overviewDistributionText(distribution map[string]int) string {
	parts := make([]string, 0, len(distribution))
	for _, status := range boardStatusOrder() {
		if count := distribution[status]; count > 0 {
			parts = append(parts, status+":"+strconv.Itoa(count))
		}
	}
	return strings.Join(parts, " ")
}

// overviewActivityText is the honest activity cell: "…" until the lazy lease
// read has arrived, then open leases + owned confine jobs (§11.6). It is never
// shown for an unavailable/ejected project (no work to have activity on).
func overviewActivityText(card overviewCard, data overviewCardData, present bool) string {
	if card.State == "unavailable" {
		return ""
	}
	if !present || !data.Loaded || data.Ejected {
		return ""
	}
	if data.Code != "" {
		return ""
	}
	if !data.LeaseKnown {
		if data.LeaseCode != "" {
			return "act unevaluated (" + data.LeaseCode + ")"
		}
		return "act …"
	}
	return "act " + strconv.Itoa(data.LeaseCount+card.OwnedJobs)
}

// overviewCheckoutText discloses WHICH checkout the card shows (spec §11.4):
// the main checkout, or a labelled fallback, plus the live-root count when a
// project spans more than one worktree (per-worktree truth differs).
func overviewCheckoutText(card overviewCard) string {
	label := "main"
	if !card.CanonicalIsMain {
		label = "no main checkout — showing " + tview.Escape(card.CanonicalRoot)
	}
	if card.LiveRootCount > 1 {
		label += " · " + strconv.Itoa(card.LiveRootCount) + " worktrees"
	}
	return "(" + label + ")"
}

// overviewCardLine composes one card's display row. All user text (slug,
// prefixes, root) is escaped: tview TableCell interprets "[…]" colour tags.
func overviewCardLine(card overviewCard, data overviewCardData, present bool) string {
	line := tview.Escape(card.Slug)
	if len(card.Prefixes) > 0 {
		line += " [" + tview.Escape(strings.Join(card.Prefixes, ",")) + "]"
	}
	line += "  " + overviewStateLabel(card, data, present)
	if activity := overviewActivityText(card, data, present); activity != "" {
		line += "  " + activity
	}
	line += "  " + overviewCheckoutText(card)
	return line
}

// overviewMatches reports whether a card matches an overview search (spec §11,
// §10): a client-side filter over slug and prefixes ONLY — no dispatch. Full
// ticket-content search needs a focused project (grep is per-project).
func overviewMatches(card overviewCard, query string) bool {
	needle := strings.ToLower(strings.TrimSpace(query))
	if needle == "" {
		return true
	}
	if strings.Contains(strings.ToLower(card.Slug), needle) {
		return true
	}
	for _, prefix := range card.Prefixes {
		if strings.Contains(strings.ToLower(prefix), needle) {
			return true
		}
	}
	return strings.Contains(strings.ToLower(card.ProjectID), needle)
}

// overviewJobsRows renders the machine-wide confine jobs strip (spec §11.6,
// §9). Correlation to a project is by ATTESTED owner only; nil Command /
// SupervisorLive render as "unevaluated", never 0/dead/blank (§14). A failed or
// unevaluated confine read is disclosed, never silently empty.
func overviewJobsRows(confine *runner.ConfineListResult, confineCode string, ownerProject map[string]string) []boardSessionRow {
	rows := make([]boardSessionRow, 0)
	switch {
	case confineCode != "":
		rows = append(rows, boardSessionRow{Style: "unevaluated", Text: "jobs unevaluated (ERROR " + confineCode + ")"})
	case confine == nil:
		rows = append(rows, boardSessionRow{Style: "unevaluated", Text: "jobs unevaluated"})
	case confine.Verdict == "unevaluated":
		reason := strings.TrimSpace(confine.Reason)
		if reason == "" {
			reason = "the daemon could not enumerate the slice"
		}
		rows = append(rows, boardSessionRow{Style: "unevaluated", Text: "jobs unevaluated: " + tview.Escape(reason)})
	default:
		for _, record := range confine.Scopes {
			project := "project unknown"
			if runner.ConfineOwnerIsAttested(record.Owner) {
				if slug, ok := ownerProject[record.Owner]; ok {
					project = "project " + tview.Escape(slug)
				}
			}
			rows = append(rows, boardSessionRow{
				Text: "job " + tview.Escape(record.Name) + " · owner " + tview.Escape(record.Owner) + " · " + project +
					" · ram " + topRAMCell(record.RSSBytes) + " · age " + topAgeCell(record.AgeSeconds) +
					" · live " + confineBoolYesNo(record.SupervisorLive) + " · " + topCommandCell(record.Command),
			})
		}
	}
	if len(rows) == 0 {
		rows = append(rows, boardSessionRow{Text: "no running jobs"})
	}
	return rows
}

// overviewOwnerProject maps every project worktree id to its display slug, so
// the jobs strip can name the owning project of an attested confine job.
func overviewOwnerProject(cards []overviewCard, groups []overviewGroup) map[string]string {
	slugByProject := map[string]string{}
	for _, card := range cards {
		slugByProject[card.ProjectID] = card.Slug
	}
	ownerProject := map[string]string{}
	for _, group := range groups {
		slug := slugByProject[group.ProjectID]
		if slug == "" {
			slug = shortProjectID(group.ProjectID)
		}
		for _, id := range group.WorktreeIDs {
			ownerProject[id] = slug
		}
	}
	return ownerProject
}

// boardStatusOrder is the canonical column order (spec §6), reused so the
// overview distribution reads left-to-right in the same order as the board.
func boardStatusOrder() []string { return domain.AllowedStatusStrings() }
