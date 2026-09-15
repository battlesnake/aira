package main

// AIRA-252 `aira board` — the read-only kanban runtime shell (spec §16,
// Increment 1: the single-project board).
//
// It is a THIRD entry point over the existing tview/tcell runtime, alongside
// runTUI and runTop. It reuses the off-UI executor, the project event-watch loop,
// the debounced refresh scheduler, the Elm reducer's fetch/watch transitions, and
// — critically — the AIRA-134 no-TTY coordinator (run/pump/coordinateShutdown),
// which stay UNTOUCHED. Only the layout, the render path, and the input capture
// differ, and those live entirely here behind the tuiRuntime.isBoard flag.

import (
	"context"
	"io"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"

	"aira/internal/daemon"
	"aira/internal/domain"

	"github.com/gdamore/tcell/v2"
	"github.com/rivo/tview"
)

const (
	boardMainPage    = "board-main"
	boardDetailPage  = "board-detail"
	boardSearchPage  = "board-search"
	boardResultsPage = "board-results"
)

// boardWidgets is the kanban's whole widget set, held off the shared tuiRuntime
// struct so the board layout is self-contained in this file.
type boardWidgets struct {
	columns     []*tview.Table
	strip       *tview.Flex
	banner      *tview.TextView
	sessions    *tview.TextView
	footer      *tview.TextView
	detail      *tview.TextView
	search      *tview.InputField
	results     *tview.List
	inputOpen   bool
	resultsOpen bool
	width       int
	lastStart   int
	lastEnd     int
	laidOut     bool
}

// runBoard is the `aira board` face. It is project-scoped (scope resolved by the
// caller via scopeForCWD, exactly as `aira tui`) and watch-on. It never calls
// app.Run() directly — the shared runTUIRuntime routes through the no-TTY
// coordinator so a screen-init failure is an honest E_INTERNAL, not a panic.
func runBoard(ctx context.Context, dispatcher Dispatcher, scope daemon.WorktreeScope, stdin io.Reader, stdout, stderr io.Writer) int {
	runtime := newBoardRuntime(ctx, dispatcher, scope, stdin, stdout, stderr, nil)
	signals := make(chan os.Signal, 2)
	signal.Notify(signals, syscall.SIGINT, syscall.SIGTERM)
	defer signal.Stop(signals)
	go runTUISignalLoop(runtime.ctx, signals, runtime.executeRunning.Load, runtime.cancel, nil)
	return runTUIRuntime(runtime, stderr)
}

// newBoardRuntime builds the board runtime: one project-scoped kanban view,
// watch-on (a ticket mutation invalidates the board), no execute dispatcher
// (read-only), and its own board reducer state.
func newBoardRuntime(parent context.Context, dispatcher Dispatcher, scope daemon.WorktreeScope, stdin io.Reader, stdout, stderr io.Writer, screen tcell.Screen) *tuiRuntime {
	return newTUIRuntimeForViews(parent, dispatcher, nil, scope, stdin, stdout, stderr, screen, boardViews, boardViews, true, newBoardState())
}

func (r *tuiRuntime) buildBoardWidgets() {
	statuses := domain.AllowedStatusStrings()
	ui := &boardWidgets{columns: make([]*tview.Table, len(statuses))}
	for i := range statuses {
		table := tview.NewTable().SetSelectable(true, false)
		table.SetBorder(true).SetTitle(" " + statuses[i] + " ")
		ui.columns[i] = table
	}
	ui.strip = tview.NewFlex().SetDirection(tview.FlexColumn)
	ui.banner = tview.NewTextView().SetDynamicColors(true).SetWrap(true)
	ui.sessions = tview.NewTextView().SetDynamicColors(true).SetWrap(false).SetScrollable(true)
	ui.sessions.SetBorder(true).SetTitle(" Sessions ")
	ui.footer = tview.NewTextView().SetDynamicColors(true).SetWrap(false)
	ui.detail = tview.NewTextView().SetWrap(true).SetScrollable(true)
	ui.detail.SetBorder(true).SetTitle(" Detail ")
	ui.search = tview.NewInputField().SetLabel("/ ")
	ui.search.SetDoneFunc(func(key tcell.Key) {
		switch key {
		case tcell.KeyEnter:
			query := ui.search.GetText()
			var commands []tuiCmd
			r.state, commands = onBoardSearchSubmit(r.state, query)
			r.boardUI.inputOpen = false
			r.outerPages.HidePage(boardSearchPage)
			// Move to the results overlay: matches are navigable and Enter opens
			// each one — the headline search UX (spec §10).
			r.boardUI.resultsOpen = true
			r.render()
			r.submitCommands(commands)
		case tcell.KeyEscape:
			r.state, _ = onBoardAction(r.state, boardActBack)
			r.closeBoardSearch()
			r.render()
		}
	})
	ui.results = tview.NewList().ShowSecondaryText(true)
	ui.results.SetBorder(true).SetTitle(" Search results ")
	r.boardUI = ui

	main := tview.NewFlex().SetDirection(tview.FlexRow).
		AddItem(ui.banner, 1, 0, false).
		AddItem(ui.strip, 0, 1, true).
		AddItem(ui.sessions, 6, 0, false).
		AddItem(ui.footer, 1, 0, false)
	r.outerPages = tview.NewPages().
		AddPage(boardMainPage, main, true, true).
		AddPage(boardDetailPage, centeredPrimitive(ui.detail, 100, 30), true, false).
		AddPage(boardResultsPage, centeredPrimitive(ui.results, 90, 24), true, false).
		AddPage(boardSearchPage, centeredPrimitive(ui.search, 70, 3), true, false)
	r.app.SetRoot(r.outerPages, true)
	r.app.SetInputCapture(r.captureBoardInput)
	// The width seam: screen.Size() is honestly available here, on the UI
	// goroutine, before each draw. Column fit is a PURE function of (width, focus)
	// and the strip is rebuilt only when the visible window actually changes.
	r.app.SetBeforeDrawFunc(func(screen tcell.Screen) bool {
		width, _ := screen.Size()
		if width != r.boardUI.width {
			r.boardUI.width = width
			r.layoutBoardStrip()
		}
		return false
	})
}

// layoutBoardStrip rebuilds the visible-column window. It is idempotent: it only
// touches the Flex when the [start,end) window changed, so it is safe to call on
// every render and every draw.
func (r *tuiRuntime) layoutBoardStrip() {
	if r.boardUI == nil || r.state.Board == nil {
		return
	}
	focus := r.state.Board.FocusedCol
	start, end := boardVisibleColumns(r.boardUI.width, focus, len(r.boardUI.columns))
	if r.boardUI.laidOut && start == r.boardUI.lastStart && end == r.boardUI.lastEnd {
		return
	}
	r.boardUI.lastStart, r.boardUI.lastEnd, r.boardUI.laidOut = start, end, true
	r.boardUI.strip.Clear()
	for i := start; i < end; i++ {
		r.boardUI.strip.AddItem(r.boardUI.columns[i], 0, 1, false)
	}
}

func (r *tuiRuntime) renderBoard() {
	bs := r.state.Board
	if bs == nil || r.boardUI == nil {
		return
	}
	r.boardUI.banner.SetText(boardBannerText(bs))
	statuses := domain.AllowedStatusStrings()
	for i, table := range r.boardUI.columns {
		table.Clear()
		column := boardColumn{Status: statuses[i]}
		if i < len(bs.Model.Columns) {
			column = bs.Model.Columns[i]
		}
		table.SetTitle(" " + boardColumnTitle(statuses[i], column, bs.HasData, bs.Stale, bs.ErrorCode) + " ")
		for row, card := range column.Cards {
			cell := tview.NewTableCell(boardCardLine(card))
			if bs.Search.Active {
				if bs.Search.MatchIDs[card.ID] {
					cell.SetTextColor(tcell.ColorYellow)
				} else {
					cell.SetTextColor(tcell.ColorGray)
				}
			}
			table.SetCell(row, 0, cell)
		}
		if i < len(bs.Selected) && len(column.Cards) > 0 {
			table.Select(clampIndex(bs.Selected[i], len(column.Cards)), 0)
		}
	}
	r.boardUI.sessions.SetText(boardSessionsText(bs.Model.Sessions))
	// Disclose the true session count in the strip title: the strip is a fixed
	// height, so extra rows clip, and the count keeps that omission from being
	// silent (the same honesty rule the column headers follow).
	sessionCount := len(bs.Model.Sessions)
	if sessionCount == 1 && bs.Model.Sessions[0].Text == "no active claims" {
		sessionCount = 0
	}
	r.boardUI.sessions.SetTitle(" Sessions (" + strconv.Itoa(sessionCount) + ") ")
	r.boardUI.footer.SetText(boardFooterText(bs))

	// Overlay precedence: detail drill-in > search input > results list. Exactly
	// one may be visible, so hide the others every render to avoid a stale overlay.
	detailShown := bs.DrillID != ""
	resultsShown := !detailShown && !r.boardUI.inputOpen && r.boardUI.resultsOpen && bs.Search.Active
	if detailShown {
		r.boardUI.detail.SetTitle(" " + bs.DrillID + " (Esc to close) ")
		r.boardUI.detail.SetText(bs.Detail)
		r.outerPages.ShowPage(boardDetailPage)
	} else {
		r.outerPages.HidePage(boardDetailPage)
	}
	if resultsShown {
		r.populateBoardResults(bs)
		r.outerPages.ShowPage(boardResultsPage)
	} else {
		r.outerPages.HidePage(boardResultsPage)
	}
	if !r.boardUI.inputOpen {
		r.outerPages.HidePage(boardSearchPage)
	}
	r.layoutBoardStrip()
	switch {
	case r.boardUI.inputOpen:
		r.app.SetFocus(r.boardUI.search)
	case detailShown:
		r.app.SetFocus(r.boardUI.detail)
	case resultsShown:
		r.app.SetFocus(r.boardUI.results)
	default:
		if focus := bs.FocusedCol; focus >= 0 && focus < len(r.boardUI.columns) {
			r.app.SetFocus(r.boardUI.columns[focus])
		}
	}
}

// populateBoardResults fills the results list from the merged match set and puts
// the cursor on the reducer's ResultIdx (the reducer stays authoritative; the
// list is a pure view). The title carries the honest result-state label.
func (r *tuiRuntime) populateBoardResults(bs *boardState) {
	r.boardUI.results.Clear()
	title := " Search results "
	if label := boardSearchLabel(bs.Search); label != "" {
		title = " Search: " + label + " (Enter open · Esc close) "
	}
	r.boardUI.results.SetTitle(title)
	for _, result := range bs.Search.Results {
		secondary := result.Snippet
		if !boardCardLoaded(*bs, result.ID) {
			secondary = "(not in a loaded column) " + secondary
		}
		r.boardUI.results.AddItem(result.ID, secondary, 0, nil)
	}
	if idx := bs.Search.ResultIdx; idx >= 0 && idx < len(bs.Search.Results) {
		r.boardUI.results.SetCurrentItem(idx)
	}
}

func (r *tuiRuntime) captureBoardInput(event *tcell.EventKey) *tcell.EventKey {
	if r.boardUI != nil && r.boardUI.inputOpen {
		return event // the search InputField owns the keyboard while it is open
	}
	if r.state.Board != nil && r.state.Board.DrillID != "" {
		if event.Key() == tcell.KeyEscape {
			r.applyBoardAction(boardActBack)
			return nil
		}
		return event // let the detail pane scroll
	}
	// Results overlay: the reducer's ResultIdx is authoritative, so every key is
	// consumed here and the list is only a view (populated in render).
	if r.boardUI != nil && r.boardUI.resultsOpen && r.state.Board != nil && r.state.Board.Search.Active {
		switch {
		case event.Key() == tcell.KeyRune && event.Rune() == 'q':
			r.applyBoardAction(boardActQuit) // q quits globally, even from the overlay
		case event.Key() == tcell.KeyEscape:
			r.state, _ = onBoardAction(r.state, boardActBack) // clears the search
			r.boardUI.resultsOpen = false
			r.render()
		case event.Key() == tcell.KeyEnter:
			var commands []tuiCmd
			r.state, commands = onBoardResultOpen(r.state)
			if r.state.Board == nil || !r.state.Board.Search.Active {
				r.boardUI.resultsOpen = false // a jump cleared the search
			}
			r.render()
			r.submitCommands(commands)
		case event.Key() == tcell.KeyUp || (event.Key() == tcell.KeyRune && event.Rune() == 'k'):
			r.state = cloneTUIState(r.state)
			*r.state.Board = boardResultMove(*r.state.Board, -1)
			r.render()
		case event.Key() == tcell.KeyDown || (event.Key() == tcell.KeyRune && event.Rune() == 'j'):
			r.state = cloneTUIState(r.state)
			*r.state.Board = boardResultMove(*r.state.Board, +1)
			r.render()
		}
		return nil
	}
	action := boardActNone
	switch event.Key() {
	case tcell.KeyLeft:
		action = boardActColLeft
	case tcell.KeyRight:
		action = boardActColRight
	case tcell.KeyUp:
		action = boardActCardUp
	case tcell.KeyDown:
		action = boardActCardDown
	case tcell.KeyEnter:
		action = boardActDrillIn
	case tcell.KeyEscape:
		action = boardActBack
	case tcell.KeyRune:
		switch event.Rune() {
		case 'h':
			action = boardActColLeft
		case 'l':
			action = boardActColRight
		case 'k':
			action = boardActCardUp
		case 'j':
			action = boardActCardDown
		case 'r':
			action = boardActRefresh
		case 'q':
			action = boardActQuit
		case '/':
			r.openBoardSearch()
			return nil
		}
	}
	if action == boardActNone {
		return event
	}
	r.applyBoardAction(action)
	return nil
}

func (r *tuiRuntime) applyBoardAction(action boardAction) {
	var commands []tuiCmd
	r.state, commands = onBoardAction(r.state, action)
	r.render()
	r.submitCommands(commands)
}

func (r *tuiRuntime) openBoardSearch() {
	r.state = onBoardSearchOpen(r.state)
	r.boardUI.inputOpen = true
	r.boardUI.resultsOpen = false
	r.boardUI.search.SetText("")
	r.outerPages.ShowPage(boardSearchPage)
	r.app.SetFocus(r.boardUI.search)
	r.render()
}

func (r *tuiRuntime) closeBoardSearch() {
	r.boardUI.inputOpen = false
	r.boardUI.resultsOpen = false
	r.outerPages.HidePage(boardSearchPage)
}

// boardBannerText renders the honest board-level banner (spec §14): the
// staleness warning (index reconcile pending, counts still correct) and the
// last-good stale marker when a refresh failed. Empty when the board is fresh.
func boardBannerText(bs *boardState) string {
	if bs == nil {
		return ""
	}
	parts := make([]string, 0, 3)
	if bs.Stale && bs.ErrorCode != "" {
		if bs.HasData {
			parts = append(parts, "[red]daemon unreachable — showing last-good (ERROR "+bs.ErrorCode+")[-]")
		} else {
			// No successful fetch yet: there is no last-good to show (P2.10).
			parts = append(parts, "[red]board unavailable (ERROR "+bs.ErrorCode+")[-]")
		}
	}
	if bs.WatchError != "" {
		parts = append(parts, "[orange]live refresh unavailable (ERROR "+bs.WatchError+") — press r to refresh[-]")
	}
	for _, warning := range bs.Model.Warnings {
		if warning == "W_STALE_INDEX" {
			parts = append(parts, "[orange]index stale — reconcile pending (counts are read from canonical files and are correct)[-]")
			continue
		}
		parts = append(parts, "[orange]"+tview.Escape(warning)+"[-]")
	}
	return strings.Join(parts, "  |  ")
}

// boardFooterText is the keybinding legend plus the honest search result label.
func boardFooterText(bs *boardState) string {
	keys := "←/→ h/l column · ↑/↓ j/k card · Enter detail · / search · r refresh · q quit"
	if bs != nil && bs.Search.Active {
		if label := boardSearchLabel(bs.Search); label != "" {
			return "search \"" + tview.Escape(bs.Search.Query) + "\": " + label + "   ·   Esc clears   ·   " + keys
		}
	}
	return keys
}
