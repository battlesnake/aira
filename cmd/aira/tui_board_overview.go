package main

// AIRA-252 `aira board` — the all-projects overview runtime shell (spec §11,
// §12, Increment 2).
//
// This is the imperative face over the pure overview reducer/view-model. Like
// the board face, it lives entirely behind a tuiRuntime flag (isOverview) and
// reuses the shared runtime shell, the off-UI executor, the debounced refresh
// scheduler, and — critically — the AIRA-134 no-TTY coordinator, all UNTOUCHED.
// It is watch-less and project-less (an `aira top`-shaped runtime), because the
// overview resolves no single project; each project's reads are dispatched with
// a per-project scope built inside the fetch (spec §11.6).

import (
	"strconv"
	"strings"

	"github.com/gdamore/tcell/v2"
	"github.com/rivo/tview"
)

const (
	overviewMainPage   = "overview-main"
	overviewSearchPage = "overview-search"
)

// overviewWidgets is the overview's whole widget set, held off the shared
// tuiRuntime struct so the layout is self-contained in this file.
type overviewWidgets struct {
	table     *tview.Table
	banner    *tview.TextView
	jobs      *tview.TextView
	footer    *tview.TextView
	search    *tview.InputField
	inputOpen bool
}

func (r *tuiRuntime) buildOverviewWidgets() {
	ui := &overviewWidgets{}
	ui.table = tview.NewTable().SetSelectable(true, false)
	ui.table.SetBorder(true).SetTitle(" Projects ")
	ui.banner = tview.NewTextView().SetDynamicColors(true).SetWrap(true)
	ui.jobs = tview.NewTextView().SetDynamicColors(true).SetWrap(false).SetScrollable(true)
	ui.jobs.SetBorder(true).SetTitle(" Jobs ")
	ui.footer = tview.NewTextView().SetDynamicColors(true).SetWrap(false)
	ui.search = tview.NewInputField().SetLabel("/ ")
	ui.search.SetDoneFunc(func(key tcell.Key) {
		switch key {
		case tcell.KeyEnter:
			query := ui.search.GetText()
			var commands []tuiCmd
			r.state, commands = onOverviewSearchSubmit(r.state, query)
			r.overviewUI.inputOpen = false
			r.outerPages.HidePage(overviewSearchPage)
			r.render()
			r.submitCommands(commands)
		case tcell.KeyEscape:
			r.overviewUI.inputOpen = false
			r.state, _ = onOverviewSearchSubmit(r.state, "")
			r.outerPages.HidePage(overviewSearchPage)
			r.render()
		}
	})
	r.overviewUI = ui

	main := tview.NewFlex().SetDirection(tview.FlexRow).
		AddItem(ui.banner, 1, 0, false).
		AddItem(ui.table, 0, 1, true).
		AddItem(ui.jobs, 6, 0, false).
		AddItem(ui.footer, 1, 0, false)
	r.outerPages = tview.NewPages().
		AddPage(overviewMainPage, main, true, true).
		AddPage(overviewSearchPage, centeredPrimitive(ui.search, 70, 3), true, false)
	r.app.SetRoot(r.outerPages, true)
	r.app.SetInputCapture(r.captureOverviewInput)
}

func (r *tuiRuntime) renderOverview() {
	ov := r.state.Overview
	if ov == nil || r.overviewUI == nil {
		return
	}
	r.overviewUI.banner.SetText(overviewBannerText(ov))
	visible := overviewVisibleCards(*ov)
	r.overviewUI.table.Clear()
	for row, card := range visible {
		data, present := ov.Data[card.ProjectID]
		cell := tview.NewTableCell(overviewCardLine(card, data, present))
		if card.State == "unavailable" {
			cell.SetTextColor(tcell.ColorGray)
		} else if present && data.Ejected {
			cell.SetTextColor(tcell.ColorOrange)
		}
		r.overviewUI.table.SetCell(row, 0, cell)
	}
	if len(visible) == 0 {
		r.overviewUI.table.SetCell(0, 0, tview.NewTableCell(overviewEmptyText(ov)).SetSelectable(false))
	} else {
		r.overviewUI.table.Select(clampIndex(ov.Selected, len(visible)), 0)
	}
	r.overviewUI.table.SetTitle(" Projects (" + strconv.Itoa(len(visible)) + ") ")
	r.overviewUI.jobs.SetText(boardSessionsText(ov.Jobs))
	r.overviewUI.footer.SetText(overviewFooterText(ov))
	if r.overviewUI.inputOpen {
		r.app.SetFocus(r.overviewUI.search)
	} else {
		r.app.SetFocus(r.overviewUI.table)
	}
}

func (r *tuiRuntime) captureOverviewInput(event *tcell.EventKey) *tcell.EventKey {
	if r.overviewUI != nil && r.overviewUI.inputOpen {
		return event // the search InputField owns the keyboard while it is open
	}
	action := overviewActNone
	switch event.Key() {
	case tcell.KeyUp:
		action = overviewActUp
	case tcell.KeyDown:
		action = overviewActDown
	case tcell.KeyEnter:
		action = overviewActOpen
	case tcell.KeyEscape:
		action = overviewActClearSearch
	case tcell.KeyRune:
		switch event.Rune() {
		case 'k':
			action = overviewActUp
		case 'j':
			action = overviewActDown
		case 'r':
			action = overviewActRefresh
		case 'q':
			action = overviewActQuit
		case '/':
			r.openOverviewSearch()
			return nil
		}
	}
	if action == overviewActNone {
		return event
	}
	var commands []tuiCmd
	r.state, commands = onOverviewAction(r.state, action)
	r.render()
	r.submitCommands(commands)
	return nil
}

func (r *tuiRuntime) openOverviewSearch() {
	r.state = onOverviewSearchOpen(r.state)
	r.overviewUI.inputOpen = true
	r.overviewUI.search.SetText("")
	r.outerPages.ShowPage(overviewSearchPage)
	r.app.SetFocus(r.overviewUI.search)
	r.render()
}

// overviewBannerText is the honest overview-level banner (spec §14): a registry
// read failure, a stale last-good marker, and the deduped warnings. Empty when
// the overview is fresh.
func overviewBannerText(ov *overviewState) string {
	parts := make([]string, 0, 3)
	if ov.Stale && ov.ErrorCode != "" {
		if ov.HasData {
			parts = append(parts, "[red]daemon unreachable — showing last-good (ERROR "+ov.ErrorCode+")[-]")
		} else {
			parts = append(parts, "[red]overview unavailable (ERROR "+ov.ErrorCode+")[-]")
		}
	}
	if ov.RegistryCode != "" {
		parts = append(parts, "[orange]registry read incomplete (ERROR "+tview.Escape(ov.RegistryCode)+")[-]")
	}
	for _, warning := range ov.Warnings {
		parts = append(parts, "[orange]"+tview.Escape(warning)+"[-]")
	}
	return strings.Join(parts, "  |  ")
}

// overviewEmptyText distinguishes "no aira projects registered" from "the
// filter matched none" — never conflating them (§14 discipline).
func overviewEmptyText(ov *overviewState) string {
	if ov.Search.Active && ov.Search.Query != "" {
		return "no project matches \"" + tview.Escape(ov.Search.Query) + "\""
	}
	if !ov.HasData {
		return "loading…"
	}
	return "no aira projects registered on this machine"
}

// overviewFooterText is the keybinding legend plus the search-filter state.
func overviewFooterText(ov *overviewState) string {
	keys := "↑/↓ j/k project · Enter open · / filter · r refresh · q quit"
	if ov.Search.Active && ov.Search.Query != "" {
		return "filter \"" + tview.Escape(ov.Search.Query) + "\": " + strconv.Itoa(len(overviewVisibleCards(*ov))) +
			" shown   ·   Esc clears   ·   " + keys
	}
	return "search filters project cards by slug/prefix; open a project for full ticket search   ·   " + keys
}
