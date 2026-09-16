package main

// AIRA-254 `aira board` info pane — the READABLE ticket detail that replaces the
// former json.MarshalIndent dump (the owner: the Enter drill-in "just gives me a
// JSON dump, nothing useful"). fetchBoardDetail composes the fetched sections
// into a boardDetailModel of plain values, each with its own evaluation code so
// a section that could not be read renders "unevaluated (CODE)" for exactly that
// part and never a fabricated blank (spec §14 honesty). The instant header /
// title / badges are NOT fetched — they come from the already-loaded card at
// render time, so the pane is legible the moment the cursor lands.

import (
	"context"
	"strconv"
	"strings"

	"aira/internal/core"
	"aira/internal/daemon"
	"aira/internal/domain"
)

// boardDetailModel is the fetched, readable detail of one ticket. Empty string
// fields mean "the ticket genuinely has none" ONLY when the owning section's
// code is ""; a non-empty section code means that part is unevaluated and the
// renderer must say so rather than show the empty value.
type boardDetailModel struct {
	// Title/Status/Severity/Kind come from `show` too, so the EXPAND overlay is
	// self-contained: it renders a full header even for a ticket that is not in a
	// loaded column (an opened grep-only search hit). The pane itself takes these
	// from the loaded card instead, so it is legible before the fetch returns.
	Title    string
	Status   string
	Severity string
	Kind     string

	Assignee  string   // "" when the ticket has none
	Labels    string   // comma-joined; "" when none
	Milestone string   // "" when none
	Body      string   // the ticket body text (trailing blank lines trimmed)
	Relations []string // readable lines, e.g. "blocks AIRA-253"
	Findings  string   // count as text, e.g. "0"

	ShowCode string // `show` failed: core fields + body unevaluated
	LinkCode string // `link ls` failed: relations unevaluated
	FindCode string // `find ls` failed: findings unevaluated
}

// fetchBoardDetail reads the three sections the pane shows — the ticket itself,
// its relations, and its finding count — and returns them as a readable model.
// Each dispatch's failure is recorded on its own *Code field; a failure never
// aborts the others, so a relations-read error still lets the body render.
func fetchBoardDetail(ctx context.Context, dispatcher Dispatcher, scope daemon.WorktreeScope, id string) boardDetailModel {
	model := boardDetailModel{}

	var show struct {
		Title     string   `json:"title"`
		Status    string   `json:"status"`
		Severity  string   `json:"severity"`
		Kind      string   `json:"kind"`
		Assignee  *string  `json:"assignee"`
		Labels    []string `json:"labels"`
		Milestone *string  `json:"milestone"`
		Body      string   `json:"body"`
	}
	if code := dispatchTUIData(ctx, dispatcher, scope, core.Request{Verb: "show", Args: map[string]any{"selector": id}}, &show); code != "" {
		model.ShowCode = code
	} else {
		model.Title = show.Title
		model.Status = show.Status
		model.Severity = show.Severity
		model.Kind = show.Kind
		if show.Assignee != nil {
			model.Assignee = strings.TrimSpace(*show.Assignee)
		}
		model.Labels = strings.Join(show.Labels, ", ")
		if show.Milestone != nil {
			model.Milestone = strings.TrimSpace(*show.Milestone)
		}
		model.Body = strings.TrimRight(show.Body, "\n")
	}

	var rels []domain.RelationView
	if code := dispatchTUIData(ctx, dispatcher, scope, core.Request{Verb: "link", Args: map[string]any{"list": true, "selector": id}}, &rels); code != "" {
		model.LinkCode = code
	} else {
		model.Relations = boardFormatRelations(rels)
	}

	var find struct {
		Total int `json:"total"`
	}
	if code := dispatchTUIData(ctx, dispatcher, scope, core.Request{Verb: "find", Args: map[string]any{"subverb": "ls", "query": "ticket:" + id}}, &find); code != "" {
		model.FindCode = code
	} else {
		model.Findings = strconv.Itoa(find.Total)
	}

	return model
}

// boardFormatRelations turns the resolved relation edges into readable lines from
// THIS ticket's perspective: "<kind> <other-id>". The store's RelationView always
// carries From = the queried subject and To = the other end (internal/store/
// relation_ready.go:324-326,518), with Kind already directional and pre-inverted
// for incoming edges — so the other end is ALWAYS To, and no From-based flip is
// needed. A flip keyed on `From != id` would also misfire under a namespaced
// project, where the fetched id (a compound search-hit id) and the display-form
// From differ textually even though From is still the subject.
func boardFormatRelations(views []domain.RelationView) []string {
	lines := make([]string, 0, len(views))
	for _, view := range views {
		kind := string(view.Kind)
		if kind == "" {
			kind = "relates"
		}
		if view.To == "" {
			lines = append(lines, kind)
			continue
		}
		lines = append(lines, kind+" "+view.To)
	}
	return lines
}

// boardDetailHeaderLine is the instant one-line header from the LOADED card
// (no fetch): "<id> · <status> · <severity> · <kind>", empty parts dropped.
func boardDetailHeaderLine(card boardCard) string {
	parts := make([]string, 0, 4)
	parts = append(parts, card.ID)
	if card.Status != "" {
		parts = append(parts, card.Status)
	}
	if card.Severity != "" {
		parts = append(parts, card.Severity)
	}
	if card.Kind != "" {
		parts = append(parts, card.Kind)
	}
	return strings.Join(parts, " · ")
}

// boardDetailField renders one fetched field honestly: its section's error code
// wins (→ "unevaluated (CODE)"), then an empty value shows emptyLabel (e.g. "—"),
// else the value itself. This is the single place the honesty rule for a fetched
// scalar lives, so no caller can accidentally show "" as if it were established.
func boardDetailField(value, code, emptyLabel string) string {
	if code != "" {
		return "unevaluated (" + code + ")"
	}
	if strings.TrimSpace(value) == "" {
		return emptyLabel
	}
	return value
}

// boardDetailFetchedLines renders the fetched metadata block — assignee, labels,
// milestone, relations, findings — each honouring its own section code so an
// unread section shows "unevaluated (CODE)" and never a fabricated value. Shared
// by the pane and the expand overlay so they cannot drift.
func boardDetailFetchedLines(model boardDetailModel) []string {
	lines := make([]string, 0, 7)
	lines = append(lines, "assignee: "+boardDetailField(model.Assignee, model.ShowCode, "—"))
	lines = append(lines, "labels: "+boardDetailField(model.Labels, model.ShowCode, "—"))
	lines = append(lines, "milestone: "+boardDetailField(model.Milestone, model.ShowCode, "—"))
	switch {
	case model.LinkCode != "":
		lines = append(lines, "relations: unevaluated ("+model.LinkCode+")")
	case len(model.Relations) == 0:
		lines = append(lines, "relations: —")
	default:
		lines = append(lines, "relations:")
		for _, relation := range model.Relations {
			lines = append(lines, "  "+relation)
		}
	}
	lines = append(lines, "findings: "+boardDetailField(model.Findings, model.FindCode, "0"))
	return lines
}

// boardDetailMetaLines is the pane's metadata block: the instant badges from the
// loaded card, then the fetched fields once ready. While the fetch is in flight
// (state "" or "loading") the fetched fields collapse to a single "loading…" so
// the badges still show immediately.
func boardDetailMetaLines(card boardCard, detail boardDetailState) []string {
	lines := make([]string, 0, 8)

	badges := make([]string, 0, 3)
	if card.Hold {
		badges = append(badges, "hold")
	}
	if card.ReadyBadge != "" {
		// The ready badge ALREADY carries the word ("● ready" / "? ready"); appending
		// " ready" would double it. The blocked badge is a bare glyph+count ("⛔3"),
		// so it does take the word.
		badges = append(badges, card.ReadyBadge)
	}
	if card.BlockedBadge != "" {
		badges = append(badges, card.BlockedBadge+" blocked")
	}
	if len(badges) > 0 {
		lines = append(lines, strings.Join(badges, "  "))
	}

	if detail.State != "ready" {
		return append(lines, "loading…")
	}
	return append(lines, boardDetailFetchedLines(detail.Model)...)
}

// boardInfoDetail is the render honesty seam for the pane: it returns the held
// detail ONLY when that detail actually describes the selected card, else a
// loading placeholder keyed to the card. This is the single place that stops the
// pane rendering one ticket's fetched fields under another ticket's title — the
// mismatch a reducer path could otherwise leave behind (e.g. closing an overlay
// opened on an unloaded search hit, before the re-armed fetch lands). The overlay
// is deliberately NOT gated this way: it renders the detail it was opened for.
func boardInfoDetail(card boardCard, detail boardDetailState) boardDetailState {
	if detail.ID != card.ID {
		return boardDetailState{ID: card.ID, State: "loading"}
	}
	return detail
}

// boardDetailOverlayText is the full readable render for the Enter-expand
// overlay. It is self-contained from the fetched model plus the id, so it works
// for a ticket that is not in a loaded column (an opened grep-only search hit);
// it reuses the pane's field and body renderers, laid out top-to-bottom for
// scrolling. No badges here — those are a card affordance the overlay lacks.
func boardDetailOverlayText(id string, detail boardDetailState, humanize boardHumanizeState) string {
	if detail.State != "ready" {
		return "loading…"
	}
	model := detail.Model
	// AIRA-257: swap in the plain-English rewrite (with a header label) when it is
	// this ticket's and toggled on; else the original. The rewrite comes from an
	// independent fetch, so it can be shown even when this overlay's own metadata
	// read failed — the label keeps it honest.
	dispTitle, dispBody, label := boardHumanizeDisplay(id, model.Title, boardDetailBodyText(detail), humanize)
	var b strings.Builder

	header := id
	if model.ShowCode == "" {
		parts := []string{id}
		if model.Status != "" {
			parts = append(parts, model.Status)
		}
		if model.Severity != "" {
			parts = append(parts, model.Severity)
		}
		if model.Kind != "" {
			parts = append(parts, model.Kind)
		}
		header = strings.Join(parts, " · ")
	}
	if label != "" {
		header += " · " + label
	}
	b.WriteString(header + "\n")
	switch {
	case model.ShowCode != "":
		b.WriteString("(ticket unevaluated: " + model.ShowCode + ")\n")
	case strings.TrimSpace(dispTitle) != "":
		b.WriteString(dispTitle + "\n")
	}
	b.WriteString("\n")
	for _, line := range boardDetailFetchedLines(model) {
		b.WriteString(line + "\n")
	}
	b.WriteString("\n")
	b.WriteString(dispBody)
	return b.String()
}

// boardDetailBodyText is the pane/overlay body: the fetch lifecycle first
// (loading… / unevaluated), then the body, then an explicit "(no description)"
// so an empty body is never an ambiguous blank pane.
func boardDetailBodyText(detail boardDetailState) string {
	if detail.State != "ready" {
		return "loading…"
	}
	if detail.Model.ShowCode != "" {
		return "unevaluated (" + detail.Model.ShowCode + ")"
	}
	if strings.TrimSpace(detail.Model.Body) == "" {
		return "(no description)"
	}
	return detail.Model.Body
}
