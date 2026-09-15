package main

// AIRA-252 `aira board` — the read-only kanban view-model.
//
// Everything in this file is a PURE function over the raw read-verb envelopes
// (boardData) the executor fetched. It reads no clock, no cgroup, no filesystem:
// the daemon is the single source of truth for every value here, and that is
// also what makes the honesty rules — the ready/blocked/session badges — unit
// testable without a terminal.
//
// The honesty core lives HERE (spec §7): a truncated read must never fabricate a
// definite negative. A `ready` list cut at the 50-cap cannot prove a workable
// ticket is "not ready", and a truncated non-terminal column cannot prove a card
// is unblocked, so both degrade to an explicit UNEVALUATED badge, never to a
// blank or a zero.

import (
	"strconv"
	"strings"

	"aira/internal/domain"
	"aira/internal/runner"
	"aira/internal/store"

	"github.com/rivo/tview"
)

const (
	boardHoldBadge         = "⏸"
	boardReadyBadge        = "● ready"
	boardReadyUnevalBadge  = "? ready"
	boardBlockedUnevalGlyf = "⛔?"
)

// boardTitleMax bounds a card/snippet title so one pathological ticket cannot
// blow up a table row; the table clamps the VISIBLE width on top of this.
const boardTitleMax = 72

// boardColumnFetch is one status column's raw `list status:<s>` reply. Code is
// the per-column transport/decode failure, carried per-section (not one global
// code) so a single failed column never blanks the other six (spec §14).
type boardColumnFetch struct {
	Status    string
	Total     int
	Truncated bool
	Rows      []map[string]any
	Code      string
}

// boardData is the whole board fetch: seven columns + the ready overlay + the
// sessions signals. Each section carries its own code so the view-model can
// collapse to one banner on a shared transport failure yet surface a per-section
// error otherwise.
type boardData struct {
	Columns     []boardColumnFetch
	Ready       listEnvelope
	ReadyCode   string
	Leases      []store.HeldLeaseRow
	LeaseCode   string
	Confine     *runner.ConfineListResult
	ConfineCode string
	// Warnings is the deduped union of every read's Response.Warnings (e.g.
	// W_STALE_INDEX). It is a board-level banner, not a per-card marker, because
	// TicketRecord.Warnings is json:"-" and the warning rides the envelope (§5).
	Warnings []string
}

type boardCard struct {
	ID           string
	Title        string
	Severity     string
	Kind         string
	Status       string
	Hold         bool
	ReadyBadge   string
	BlockedBadge string
}

type boardColumn struct {
	Status    string
	Total     int
	Truncated bool
	Code      string
	Cards     []boardCard
}

type boardSessionRow struct {
	Text  string
	Style string
}

type boardModel struct {
	Columns  []boardColumn
	Sessions []boardSessionRow
	Warnings []string
}

// boardSatisfied and boardWorkable mirror store.satisfied/workable, which are
// unexported. They are the domain's own status ladder, so replicating the two
// tiny predicates here is not a second source of truth — it is the same one.
func boardSatisfied(status string) bool {
	switch domain.Status(status) {
	case domain.StatusDone, domain.StatusRetired, domain.StatusSuperseded:
		return true
	}
	return false
}

func boardWorkable(status string) bool {
	switch domain.Status(status) {
	case domain.StatusPlanned, domain.StatusInProgress:
		return true
	}
	return false
}

// boardNonTerminalStatus is the set whose truncation makes the blocked-by count
// unevaluated: a blocking prerequisite that is NOT satisfied must live in one of
// these, so if any is truncated an unloaded blocker could be hiding in its tail.
func boardNonTerminalStatus(status string) bool {
	switch domain.Status(status) {
	case domain.StatusDraft, domain.StatusPlanned, domain.StatusInProgress, domain.StatusInReview:
		return true
	}
	return false
}

// boardEscapeTruncate escapes tview colour-tag syntax (a title/snippet is
// arbitrary user text and TableCell interprets `[…]`) and bounds the length.
func boardEscapeTruncate(text string) string {
	runes := []rune(text)
	if len(runes) > boardTitleMax {
		text = string(runes[:boardTitleMax]) + "…"
	}
	return tview.Escape(text)
}

// boardReadySets returns two id sets from the no-selector `ready` reply, both
// SKIPPING id-less finding rows ({path, ready:false}, core.go:3025-3026):
//
//   - present: every id the ready scan returned (any ready value). Absence from
//     this set is what a truncated scan cannot distinguish from "cut past the
//     50-cap", which is why absence + truncation is UNEVALUATED, not "not ready".
//   - ready: the ids whose row carried ready==true.
func boardReadySets(env listEnvelope) (present, ready map[string]bool) {
	present = make(map[string]bool)
	ready = make(map[string]bool)
	for _, row := range env.Rows {
		id := textCell(row["id"])
		if id == "" {
			continue // id-less finding row — never joined on
		}
		present[id] = true
		if value, ok := row["ready"].(bool); ok && value {
			ready[id] = true
		}
	}
	return present, ready
}

type boardBlocksEdge struct {
	From string
	To   string
}

// boardBlocksEdges collects every STORED `blocks` edge from the union of loaded
// list rows' relations[], and the id→status map of all loaded rows. A `blocks`
// edge is stored only on its canonical (lower-id) endpoint, so scanning EVERY
// loaded row — not just a given card's own row — is what finds edges owned by
// the other endpoint (spec §7).
func boardBlocksEdges(data boardData) (edges []boardBlocksEdge, statusByID map[string]string) {
	statusByID = make(map[string]string)
	for _, column := range data.Columns {
		for _, row := range column.Rows {
			id := textCell(row["id"])
			if id != "" {
				statusByID[id] = textCell(row["status"])
			}
			relations, ok := row["relations"].([]any)
			if !ok {
				continue
			}
			for _, raw := range relations {
				relation, ok := raw.(map[string]any)
				if !ok {
					continue
				}
				if textCell(relation["kind"]) != string(domain.RelationBlocks) {
					continue
				}
				edges = append(edges, boardBlocksEdge{From: textCell(relation["from"]), To: textCell(relation["to"])})
			}
		}
	}
	return edges, statusByID
}

// boardBlockedExact reports whether the blocked-by count can be exact: it can
// iff no NON-TERMINAL column was truncated. When one was, an unloaded blocking
// prerequisite could sit in its tail, so the count is unevaluated.
func boardBlockedExact(data boardData) bool {
	for _, column := range data.Columns {
		if boardNonTerminalStatus(column.Status) && column.Truncated {
			return false
		}
	}
	return true
}

// boardBlockedBadge is the honest blocked-by badge for one card.
//
//   - a satisfied (terminal) card gets NO badge: blocked-by is a workability
//     signal and its prerequisites are moot once the work is done.
//   - !exact → ⛔? (unevaluated): a blocking prerequisite might be in a truncated
//     non-terminal column's unloaded tail, so no definite count is possible.
//   - exact & N>0 → ⛔N. exact & N==0 → no badge (never a fabricated ⛔0).
//
// N counts edges (To==card, From loaded-and-not-satisfied). An unloaded From is
// skipped: when exact it is necessarily terminal (satisfied) and would not count
// anyway; when !exact the badge is ⛔? regardless.
func boardBlockedBadge(cardID, cardStatus string, exact bool, edges []boardBlocksEdge, statusByID map[string]string) string {
	if boardSatisfied(cardStatus) {
		return ""
	}
	if !exact {
		return boardBlockedUnevalGlyf
	}
	count := 0
	for _, edge := range edges {
		if edge.To != cardID {
			continue
		}
		status, ok := statusByID[edge.From]
		if !ok {
			continue
		}
		if !boardSatisfied(status) {
			count++
		}
	}
	if count == 0 {
		return ""
	}
	return "⛔" + strconv.Itoa(count)
}

// boardReadyBadgeFor is the honest ready badge (spec §7): positive-or-unevaluated,
// never a fabricated negative.
func boardReadyBadgeFor(cardID, cardStatus string, readyTruncated bool, present, ready map[string]bool) string {
	if ready[cardID] {
		return boardReadyBadge
	}
	if boardWorkable(cardStatus) && !present[cardID] && readyTruncated {
		// Absent from a CUT ready scan: genuinely unknowable, not "not ready".
		return boardReadyUnevalBadge
	}
	return ""
}

// buildBoardModel is the whole read-only view-model: seven columns of cards with
// honest badges, the sessions strip, and the staleness warnings.
func buildBoardModel(data boardData) boardModel {
	model := boardModel{}
	present, ready := boardReadySets(data.Ready)
	edges, statusByID := boardBlocksEdges(data)
	exact := boardBlockedExact(data)

	seenWarning := map[string]bool{}
	for _, warning := range data.Warnings {
		if warning != "" && !seenWarning[warning] {
			seenWarning[warning] = true
			model.Warnings = append(model.Warnings, warning)
		}
	}

	for _, columnData := range data.Columns {
		column := boardColumn{
			Status: columnData.Status, Total: columnData.Total,
			Truncated: columnData.Truncated, Code: columnData.Code,
		}
		for _, row := range columnData.Rows {
			id := textCell(row["id"])
			status := textCell(row["status"])
			hold, _ := row["hold"].(bool)
			card := boardCard{
				ID:       id,
				Title:    boardEscapeTruncate(textCell(row["title"])),
				Severity: textCell(row["severity"]),
				Kind:     textCell(row["kind"]),
				Status:   status,
				Hold:     hold,
			}
			card.ReadyBadge = boardReadyBadgeFor(id, status, data.Ready.Truncated, present, ready)
			card.BlockedBadge = boardBlockedBadge(id, status, exact, edges, statusByID)
			column.Cards = append(column.Cards, card)
		}
		model.Columns = append(model.Columns, column)
	}
	model.Sessions = boardSessionRows(data)
	return model
}

// boardSessionRows is the best-effort join of DECLARED activity signals, each
// graded by attestation (spec §9). aira has no session entity; unknowns are
// shown as unknown, never fabricated. A confine owner correlates to a ticket
// only when it is an ATTESTED worktree id that also holds a lease in THIS
// project — the cross-project registry walk is Increment 2.
func boardSessionRows(data boardData) []boardSessionRow {
	ticketByWorktree := map[string]string{}
	rows := make([]boardSessionRow, 0)
	for _, lease := range data.Leases {
		if lease.WorktreeID != "" && lease.TicketID != "" {
			ticketByWorktree[lease.WorktreeID] = lease.TicketID
		}
		style := ""
		state := "held"
		switch {
		case lease.AgeNote == "stale (prior boot)":
			style, state = "stale", "STALE"
		case lease.Expired:
			style, state = "expired", "EXPIRED"
		}
		actor := lease.Actor
		if actor == "" {
			actor = "aira"
		}
		age := lease.AgeNote
		if age == "" {
			age = "unevaluated"
		}
		rows = append(rows, boardSessionRow{
			Style: style,
			Text:  "lease " + lease.TicketID + " · " + state + " · " + tview.Escape(actor) + " · " + age,
		})
	}
	if data.Confine != nil && data.Confine.Verdict != "unevaluated" {
		for _, record := range data.Confine.Scopes {
			owner := record.Owner
			ticket := "ticket unknown"
			if runner.ConfineOwnerIsAttested(owner) {
				if id, ok := ticketByWorktree[owner]; ok {
					ticket = "ticket " + id
				}
			}
			rows = append(rows, boardSessionRow{
				Style: "",
				Text: "job " + tview.Escape(record.Name) + " · owner " + tview.Escape(owner) + " · " + ticket +
					" · ram " + topRAMCell(record.RSSBytes) + " · age " + topAgeCell(record.AgeSeconds) +
					" · live " + confineBoolYesNo(record.SupervisorLive) + " · " + topCommandCell(record.Command),
			})
		}
	}
	if len(rows) == 0 {
		rows = append(rows, boardSessionRow{Text: "no active claims"})
	}
	return rows
}

// boardColumnHeader is the disclosed column title: status + count, with a
// PERSISTENT truncation marker whenever the authoritative total exceeds the
// 50-row cap, so the omission is never silent (spec §6).
func boardColumnHeader(column boardColumn) string {
	if column.Code != "" {
		return column.Status + " · ERROR " + column.Code
	}
	if column.Truncated {
		return column.Status + " · " + strconv.Itoa(len(column.Cards)) + "/" + strconv.Itoa(column.Total) + " (lowest ids)"
	}
	return column.Status + " · " + strconv.Itoa(column.Total)
}

// boardCardLine composes a card's single table-row cell: id, severity, the
// honest badges (hold/ready/blocked), then the already-escaped title. The badges
// are structured fields on boardCard (asserted directly in tests); this is the
// display join, and the smoke test verifies the glyphs reach the screen.
func boardCardLine(card boardCard) string {
	line := card.ID
	if card.Severity != "" {
		line += " " + card.Severity
	}
	badges := make([]string, 0, 3)
	if card.Hold {
		badges = append(badges, boardHoldBadge)
	}
	if card.ReadyBadge != "" {
		badges = append(badges, card.ReadyBadge)
	}
	if card.BlockedBadge != "" {
		badges = append(badges, card.BlockedBadge)
	}
	if len(badges) > 0 {
		line += " " + strings.Join(badges, " ")
	}
	if card.Title != "" {
		line += "  " + card.Title
	}
	return line
}

// boardSessionsText renders the sessions strip, colouring expired/stale/unknown
// rows. Row text is already tview.Escape'd for its user-supplied portions.
func boardSessionsText(rows []boardSessionRow) string {
	lines := make([]string, 0, len(rows))
	for _, row := range rows {
		switch row.Style {
		case "expired":
			lines = append(lines, "[red]"+row.Text+"[-]")
		case "stale":
			lines = append(lines, "[orange]"+row.Text+"[-]")
		case "unevaluated":
			lines = append(lines, "[yellow]"+row.Text+"[-]")
		default:
			lines = append(lines, row.Text)
		}
	}
	return strings.Join(lines, "\n")
}

// boardColumnTitle is the honest column header across the fetch lifecycle. Before
// the FIRST successful fetch a column has no authoritative count, so it reads
// "loading…" (or "unevaluated (ERROR …)" if that first fetch failed) rather than
// the fabricated "· 0" that empty Model.Columns would otherwise produce. Once
// data has arrived, the count is real and a stale refresh is disclosed.
func boardColumnTitle(status string, column boardColumn, hasData, stale bool, errorCode string) string {
	if !hasData {
		if stale && errorCode != "" {
			return status + " · unevaluated (ERROR " + errorCode + ")"
		}
		return status + " · loading…"
	}
	title := boardColumnHeader(column)
	if stale {
		title += " · stale"
	}
	return title
}

// boardHasTransportBanner collapses a shared transport failure across every
// section into one board-level banner rather than seven per-section errors
// (spec §14). It returns the shared code when EVERY fetched section failed with
// the same code, else "".
func boardHasTransportBanner(data boardData) string {
	codes := make([]string, 0, len(data.Columns)+3)
	for _, column := range data.Columns {
		codes = append(codes, column.Code)
	}
	codes = append(codes, data.ReadyCode, data.LeaseCode)
	shared := ""
	for i, code := range codes {
		if code == "" {
			return ""
		}
		if i == 0 {
			shared = code
			continue
		}
		if code != shared {
			return ""
		}
	}
	return shared
}
