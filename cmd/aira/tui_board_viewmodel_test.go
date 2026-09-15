package main

import (
	"strings"
	"testing"

	"aira/internal/domain"
	"aira/internal/runner"
	"aira/internal/store"

	"github.com/rivo/tview"
)

// boardRow builds one `list` row (a decoded map[string]any) with optional
// stored relation edges.
func boardRow(id, status, severity, title string, hold bool, relations ...map[string]any) map[string]any {
	row := map[string]any{
		"id": id, "status": status, "severity": severity, "title": title,
		"kind": "feature", "hold": hold,
	}
	if len(relations) > 0 {
		anyRelations := make([]any, len(relations))
		for i, relation := range relations {
			anyRelations[i] = relation
		}
		row["relations"] = anyRelations
	}
	return row
}

func blocksEdge(from, to string) map[string]any {
	return map[string]any{"kind": "blocks", "from": from, "to": to}
}

// boardDataFrom assembles a full seven-column boardData from a status→column map,
// filling absent statuses with empty columns so the non-terminal-truncation rule
// sees every column.
func boardDataFrom(columns map[string]boardColumnFetch, ready listEnvelope) boardData {
	data := boardData{Ready: ready}
	for _, status := range domain.AllowedStatusStrings() {
		column := columns[status]
		column.Status = status
		data.Columns = append(data.Columns, column)
	}
	return data
}

func findCard(t *testing.T, model boardModel, id string) boardCard {
	t.Helper()
	for _, column := range model.Columns {
		for _, card := range column.Cards {
			if card.ID == id {
				return card
			}
		}
	}
	t.Fatalf("card %q not found in model", id)
	return boardCard{}
}

// TestBoardReadyBadgeTruncatedAbsentIsUnevaluated is the P1 false-pass guard: a
// workable card absent from a CUT ready scan must read "? ready" (unevaluated),
// never unbadged and never "not ready". Flips if the readyTruncated check is
// dropped.
func TestBoardReadyBadgeTruncatedAbsentIsUnevaluated(t *testing.T) {
	data := boardDataFrom(map[string]boardColumnFetch{
		"planned": {Rows: []map[string]any{boardRow("AIRA-1", "planned", "P1", "one", false)}},
	}, listEnvelope{Total: 60, Truncated: true, Rows: []map[string]any{
		{"id": "AIRA-99", "ready": true}, // AIRA-1 is NOT in the cut list
	}})
	model := buildBoardModel(data)
	if got := findCard(t, model, "AIRA-1").ReadyBadge; got != boardReadyUnevalBadge {
		t.Fatalf("truncated-absent workable card ready badge = %q, want %q", got, boardReadyUnevalBadge)
	}
}

// TestBoardReadyBadgeStates pins the positive and genuine-negative cases.
func TestBoardReadyBadgeStates(t *testing.T) {
	data := boardDataFrom(map[string]boardColumnFetch{
		"planned": {Rows: []map[string]any{
			boardRow("AIRA-2", "planned", "P1", "ready one", false),
			boardRow("AIRA-3", "planned", "P1", "clean-blocked", false),
		}},
		"done": {Rows: []map[string]any{boardRow("AIRA-4", "done", "P2", "finished", false)}},
	}, listEnvelope{Total: 1, Truncated: false, Rows: []map[string]any{
		{"id": "AIRA-2", "ready": true},
	}})
	model := buildBoardModel(data)
	if got := findCard(t, model, "AIRA-2").ReadyBadge; got != boardReadyBadge {
		t.Fatalf("present-true ready badge = %q, want %q", got, boardReadyBadge)
	}
	// Workable, absent, NOT truncated → genuinely not ready (blocked-but-clean),
	// so NO badge, never a fabricated negative.
	if got := findCard(t, model, "AIRA-3").ReadyBadge; got != "" {
		t.Fatalf("workable-absent-untruncated ready badge = %q, want empty", got)
	}
	// Satisfied ticket is never a ready candidate.
	if got := findCard(t, model, "AIRA-4").ReadyBadge; got != "" {
		t.Fatalf("satisfied card ready badge = %q, want empty", got)
	}
}

// TestBoardReadySetsSkipIDlessRows guards the join against id-less finding rows
// ({path, ready:false}) that the no-selector ready reply emits (core.go:3025).
func TestBoardReadySetsSkipIDlessRows(t *testing.T) {
	present, ready, unevaluated := boardReadySets(listEnvelope{Rows: []map[string]any{
		{"path": "tickets/x.md", "ready": false}, // id-less — must be skipped
		{"id": "AIRA-7", "ready": true},
		{"id": "AIRA-8", "ready": false, "verdict": "unevaluated"},
	}})
	if present[""] || ready[""] || unevaluated[""] {
		t.Fatalf("id-less finding row leaked an empty id into the ready sets")
	}
	if !present["AIRA-7"] || !ready["AIRA-7"] {
		t.Fatalf("real ready row missing from the sets present=%v ready=%v", present, ready)
	}
	if !unevaluated["AIRA-8"] || ready["AIRA-8"] {
		t.Fatalf("per-row unevaluated verdict not captured: unevaluated=%v", unevaluated)
	}
}

// TestBoardReadyBadgeUnevaluatedStates is the P1.1 false-pass guard for the
// section-unknown states Fable flagged: a workable card ABSENT from a ready scan
// that FAILED (ReadyCode) or arrived envelope-UNEVALUATED → "? ready"; and a card
// PRESENT whose per-row verdict is "unevaluated" → "? ready", never unbadged.
func TestBoardReadyBadgeUnevaluatedStates(t *testing.T) {
	// Ready fetch FAILED: absent workable card is unknowable, not "not ready".
	failed := boardDataFrom(map[string]boardColumnFetch{
		"planned": {Rows: []map[string]any{boardRow("AIRA-1", "planned", "P1", "one", false)}},
	}, listEnvelope{})
	failed.ReadyCode = "E_TUI_CANCELLED"
	if got := findCard(t, buildBoardModel(failed), "AIRA-1").ReadyBadge; got != boardReadyUnevalBadge {
		t.Fatalf("failed-ready absent workable card = %q, want %q", got, boardReadyUnevalBadge)
	}

	// Ready envelope UNEVALUATED: same.
	envUneval := boardDataFrom(map[string]boardColumnFetch{
		"planned": {Rows: []map[string]any{boardRow("AIRA-1", "planned", "P1", "one", false)}},
	}, listEnvelope{})
	envUneval.ReadyUnevaluated = true
	if got := findCard(t, buildBoardModel(envUneval), "AIRA-1").ReadyBadge; got != boardReadyUnevalBadge {
		t.Fatalf("envelope-unevaluated absent workable card = %q, want %q", got, boardReadyUnevalBadge)
	}

	// PRESENT but per-row verdict unevaluated (e.g. a relation-graph finding): the
	// card's own readiness could not be evaluated → "? ready", never silently blank.
	presentUneval := boardDataFrom(map[string]boardColumnFetch{
		"planned": {Rows: []map[string]any{boardRow("AIRA-1", "planned", "P1", "one", false)}},
	}, listEnvelope{Rows: []map[string]any{{"id": "AIRA-1", "ready": false, "verdict": "unevaluated"}}})
	if got := findCard(t, buildBoardModel(presentUneval), "AIRA-1").ReadyBadge; got != boardReadyUnevalBadge {
		t.Fatalf("present-but-unevaluated card = %q, want %q", got, boardReadyUnevalBadge)
	}
}

// TestBoardBlockedUnevaluatedWhenNonTerminalColumnFailed is P1.2: a non-terminal
// column whose fetch FAILED (Code!=", Rows=nil) loaded no rows, so a real blocker
// whose From row lives there would silently not count. Must be ⛔?, not unblocked.
func TestBoardBlockedUnevaluatedWhenNonTerminalColumnFailed(t *testing.T) {
	data := boardDataFrom(map[string]boardColumnFetch{
		// AIRA-250's blocker AIRA-240 lives in the FAILED planned column.
		"in-progress": {Rows: []map[string]any{boardRow("AIRA-250", "in-progress", "P1", "blocked", false, blocksEdge("AIRA-240", "AIRA-250"))}},
		"planned":     {Code: "E_TUI_CANCELLED"}, // failed fetch: Rows=nil, Truncated=false
	}, listEnvelope{})
	if got := findCard(t, buildBoardModel(data), "AIRA-250").BlockedBadge; got != boardBlockedUnevalGlyf {
		t.Fatalf("blocked badge with a failed non-terminal column = %q, want %q", got, boardBlockedUnevalGlyf)
	}
}

// TestBoardBlockedUnevaluatedWhenNonTerminalTruncated is the P1 false-pass guard:
// a blocking prerequisite that could sit in a truncated non-terminal column's
// unloaded tail must read ⛔? — never ⛔0/no-badge. Here the blocker AIRA-9 is
// UNLOADED and the in-progress column is truncated, so exact counting cannot
// prove the card unblocked.
func TestBoardBlockedUnevaluatedWhenNonTerminalTruncated(t *testing.T) {
	data := boardDataFrom(map[string]boardColumnFetch{
		// AIRA-1 is canonical (< AIRA-9), so the blocks edge is stored on its row.
		"planned":     {Rows: []map[string]any{boardRow("AIRA-1", "planned", "P1", "blocked", false, blocksEdge("AIRA-9", "AIRA-1"))}},
		"in-progress": {Total: 60, Truncated: true, Rows: []map[string]any{boardRow("AIRA-2", "in-progress", "P1", "wip", false)}},
	}, listEnvelope{})
	if got := findCard(t, buildBoardModel(data), "AIRA-1").BlockedBadge; got != boardBlockedUnevalGlyf {
		t.Fatalf("blocked badge under non-terminal truncation = %q, want %q", got, boardBlockedUnevalGlyf)
	}
}

// TestBoardBlockedExactCounts pins the exact counting when nothing is truncated:
// an unsatisfied loaded blocker counts (⛔1), a satisfied blocker does not (no
// fabricated ⛔0), and no blockers gives no badge.
func TestBoardBlockedExactCounts(t *testing.T) {
	data := boardDataFrom(map[string]boardColumnFetch{
		"planned": {Rows: []map[string]any{
			boardRow("AIRA-1", "planned", "P1", "blocked-active", false, blocksEdge("AIRA-2", "AIRA-1")),
			boardRow("AIRA-2", "planned", "P1", "prereq-active", false),
			boardRow("AIRA-3", "planned", "P1", "blocked-satisfied", false, blocksEdge("AIRA-8", "AIRA-3")),
			boardRow("AIRA-5", "planned", "P1", "unblocked", false),
		}},
		"done": {Rows: []map[string]any{boardRow("AIRA-8", "done", "P2", "prereq-done", false)}},
	}, listEnvelope{})
	model := buildBoardModel(data)
	if got := findCard(t, model, "AIRA-1").BlockedBadge; got != "⛔1" {
		t.Fatalf("one unsatisfied blocker badge = %q, want ⛔1", got)
	}
	if got := findCard(t, model, "AIRA-3").BlockedBadge; got != "" {
		t.Fatalf("satisfied-prereq blocked badge = %q, want empty (no fabricated ⛔0)", got)
	}
	if got := findCard(t, model, "AIRA-5").BlockedBadge; got != "" {
		t.Fatalf("unblocked card blocked badge = %q, want empty", got)
	}
}

// TestBoardWarningsBecomeBanner is the false-pass guard for staleness plumbing:
// Response.Warnings must reach the model and render a banner (spec §5/§14).
func TestBoardWarningsBecomeBanner(t *testing.T) {
	data := boardDataFrom(map[string]boardColumnFetch{}, listEnvelope{})
	data.Warnings = []string{"W_STALE_INDEX"}
	model := buildBoardModel(data)
	found := false
	for _, warning := range model.Warnings {
		if warning == "W_STALE_INDEX" {
			found = true
		}
	}
	if !found {
		t.Fatalf("W_STALE_INDEX did not reach model.Warnings: %v", model.Warnings)
	}
	banner := boardBannerText(&boardState{Model: model})
	if !strings.Contains(banner, "index stale") {
		t.Fatalf("banner did not render the stale-index warning: %q", banner)
	}
}

// TestBoardTitleIsEscaped is the tag-injection guard: a `[x]`-bearing title must
// be tview.Escape'd so it renders literally, not swallowed as a colour tag.
func TestBoardTitleIsEscaped(t *testing.T) {
	raw := "[x] rewrite parser"
	data := boardDataFrom(map[string]boardColumnFetch{
		"planned": {Rows: []map[string]any{boardRow("AIRA-1", "planned", "P1", raw, false)}},
	}, listEnvelope{})
	card := findCard(t, buildBoardModel(data), "AIRA-1")
	if card.Title != tview.Escape(raw) {
		t.Fatalf("title = %q, want escaped %q", card.Title, tview.Escape(raw))
	}
	if card.Title == raw {
		t.Fatalf("title was not escaped: %q", card.Title)
	}
}

// TestBoardSessionRowsHonesty pins §9: a nil confine Command renders
// "unevaluated" (never blank), an attested owner correlates to its project
// lease's ticket, and an unattested owner is "ticket unknown".
func TestBoardSessionRowsHonesty(t *testing.T) {
	yes := true
	rss := int64(1 << 20)
	age := int64(42)
	data := boardData{
		Leases: []store.HeldLeaseRow{{TicketID: "AIRA-9", WorktreeID: "wtattested", Actor: "opus", AgeNote: "2m ago"}},
		Confine: &runner.ConfineListResult{Verdict: "ok", Scopes: []runner.ConfineRecord{
			// RSS/Age NON-NIL so only the nil Command can produce the trailing
			// "· unevaluated"; a non-escaping/blank impl then fails (P2.11 — the
			// old test passed vacuously because RSS/Age also rendered "unevaluated").
			{Name: "job-attested", Owner: "wtattested", SupervisorLive: &yes, RSSBytes: &rss, AgeSeconds: &age, Command: nil},
			{Name: "job-unknown", Owner: runner.ConfineUnknownOwner, SupervisorLive: &yes, RSSBytes: &rss, AgeSeconds: &age, Command: nil},
		}},
	}
	rows := boardSessionRows(data)
	joined := ""
	for _, row := range rows {
		joined += row.Text + "\n"
	}
	if !strings.Contains(joined, "· unevaluated\n") {
		t.Fatalf("nil confine Command was not rendered as the trailing unevaluated segment:\n%s", joined)
	}
	if !strings.Contains(joined, "job-attested") || !strings.Contains(joined, "ticket AIRA-9") {
		t.Fatalf("attested owner did not correlate to its lease ticket:\n%s", joined)
	}
	if !strings.Contains(joined, "job-unknown · owner unknown · ticket unknown") {
		t.Fatalf("unattested owner was not shown as ticket unknown:\n%s", joined)
	}
}

func TestBoardSessionRowsEmptyState(t *testing.T) {
	// Genuine empty ONLY when both sections succeeded (codes empty, confine present
	// and evaluated with zero scopes).
	rows := boardSessionRows(boardData{Confine: &runner.ConfineListResult{Verdict: "ok"}})
	if len(rows) != 1 || rows[0].Text != "no active claims" {
		t.Fatalf("empty sessions state = %#v, want the first-class 'no active claims' row", rows)
	}
}

// TestBoardSessionRowsUnknownSections is P1.3: an unknown lease or job section
// must surface an UNEVALUATED row and NEVER read "no active claims" over it.
func TestBoardSessionRowsUnknownSections(t *testing.T) {
	joinRows := func(rows []boardSessionRow) string {
		out := ""
		for _, row := range rows {
			out += row.Text + "|" + row.Style + "\n"
		}
		return out
	}

	leaseFail := joinRows(boardSessionRows(boardData{LeaseCode: "E_TUI_CANCELLED", Confine: &runner.ConfineListResult{Verdict: "ok"}}))
	if !strings.Contains(leaseFail, "leases unevaluated (ERROR E_TUI_CANCELLED)|unevaluated") || strings.Contains(leaseFail, "no active claims") {
		t.Fatalf("failed lease read: %q", leaseFail)
	}

	confineFail := joinRows(boardSessionRows(boardData{ConfineCode: "E_TUI_CANCELLED"}))
	if !strings.Contains(confineFail, "jobs unevaluated (ERROR E_TUI_CANCELLED)|unevaluated") || strings.Contains(confineFail, "no active claims") {
		t.Fatalf("failed confine read: %q", confineFail)
	}

	confineUneval := joinRows(boardSessionRows(boardData{Confine: &runner.ConfineListResult{Verdict: "unevaluated", Reason: "daemon down"}}))
	if !strings.Contains(confineUneval, "jobs unevaluated: daemon down|unevaluated") || strings.Contains(confineUneval, "no active claims") {
		t.Fatalf("unevaluated confine verdict: %q", confineUneval)
	}

	// Confine nil with no code (defensive): still unevaluated, never empty-state.
	confineNil := joinRows(boardSessionRows(boardData{}))
	if !strings.Contains(confineNil, "jobs unevaluated") || strings.Contains(confineNil, "no active claims") {
		t.Fatalf("nil confine without code: %q", confineNil)
	}
}

// TestBoardColumnTitleLifecycle is the fabricated-zero guard: before the first
// fetch a column reads "loading…" (never "· 0"); a failed first fetch reads
// "unevaluated (ERROR …)"; only real data shows a count.
func TestBoardColumnTitleLifecycle(t *testing.T) {
	if got := boardColumnTitle("draft", boardColumn{Status: "draft"}, false, false, ""); got != "draft · loading…" {
		t.Fatalf("pre-fetch title = %q, want 'draft · loading…' (never a fabricated · 0)", got)
	}
	if got := boardColumnTitle("draft", boardColumn{Status: "draft"}, false, true, "E_DAEMON_UNREACHABLE"); !strings.Contains(got, "unevaluated") || strings.Contains(got, "· 0") {
		t.Fatalf("first-fetch-failure title = %q, want 'unevaluated', never '· 0'", got)
	}
	if got := boardColumnTitle("draft", boardColumn{Status: "draft", Total: 3}, true, false, ""); got != "draft · 3" {
		t.Fatalf("loaded title = %q, want 'draft · 3'", got)
	}
	if got := boardColumnTitle("draft", boardColumn{Status: "draft", Total: 3}, true, true, "E_X"); got != "draft · 3 · stale" {
		t.Fatalf("stale-refresh title = %q, want 'draft · 3 · stale'", got)
	}
}

func TestBoardWatchErrorBanner(t *testing.T) {
	banner := boardBannerText(&boardState{HasData: true, WatchError: "E_DAEMON_UNREACHABLE"})
	if !strings.Contains(banner, "live refresh unavailable") || !strings.Contains(banner, "E_DAEMON_UNREACHABLE") {
		t.Fatalf("watch-error banner = %q, want a live-refresh-unavailable disclosure", banner)
	}
}

// TestBoardColumnHeaderTruncationDisclosed pins the persistent truncation marker
// so a >50 column never silently hides its omission (spec §6).
func TestBoardColumnHeaderTruncationDisclosed(t *testing.T) {
	column := boardColumn{Status: "done", Total: 193, Truncated: true, Cards: make([]boardCard, 50)}
	header := boardColumnHeader(column)
	if !strings.Contains(header, "50/193") || !strings.Contains(header, "lowest ids") {
		t.Fatalf("truncated header = %q, want a 50/193 lowest-ids disclosure", header)
	}
	if got := boardColumnHeader(boardColumn{Status: "draft", Total: 3}); got != "draft · 3" {
		t.Fatalf("untruncated header = %q, want 'draft · 3'", got)
	}
}
