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
	main     *tview.Flex // the outer FlexRow, kept so the info pane can be resized
	columns  []*tview.Table
	strip    *tview.Flex
	banner   *tview.TextView
	sessions *tview.TextView
	footer   *tview.TextView
	detail   *tview.TextView
	search   *tview.InputField
	results  *tview.List
	// AIRA-254 info pane: a bordered container whose meta|body area splits
	// side-by-side on a wide terminal (title+fields left, body right) and stacks
	// into one wrapping column on a narrow one. Both children are PROPORTIONAL —
	// no hand-computed row counts — so tview never receives a negative size, and
	// the title, rendered first, is the last thing to clip.
	info     *tview.Flex
	infoArea *tview.Flex
	infoMeta *tview.TextView // wide: title + fields (left). narrow: unused.
	infoBody *tview.TextView // wide: body (right). narrow: the whole stacked pane.
	// detailOuter/detailInner hold the expand overlay's centring flexes so the
	// fixed 100x30 box can be shrunk to fit a small screen each draw (tview does
	// not clamp a fixed item, so an oversized box would draw off-screen).
	detailOuter *tview.Flex
	detailInner *tview.Flex
	inputOpen   bool
	resultsOpen bool
	width       int
	height      int
	lastStart   int
	lastEnd     int
	laidOut     bool
	// info-pane relayout memo: the internal area split is rebuilt only when the
	// (wide, paneHeight) pair actually changes, so it is safe to call every draw.
	infoLaidOut    bool
	lastInfoWide   bool
	lastInfoHeight int
}

// runBoard is the `aira board` face and its Increment-2 mode-switching OUTER
// LOOP (spec §12). Each mode is its OWN independent runtime — the per-project
// kanban is watch-on, the all-projects overview is watch-less — built fresh via
// newTUIRuntimeForViews and run to a transition key, then fully torn down before
// the next mode starts. There is no scope-swap and no watch-rebind inside a
// runtime; the previously "delicate" concurrency seam is designed out.
//
// startOverview picks the initial mode (main.go resolves it from scopeForCWD:
// success → per-project board on that scope; E_NOT_PROJECT / E_CONFIG_MISSING →
// overview). Signals are wired ONCE here; each per-mode runtime gets its own
// signal loop bound to its own ctx, so the prior loop has already exited (its
// ctx cancelled) before the next mode starts. Every runtime routes through
// runTUIRuntime — never app.Run() — so a screen-init failure is an honest
// E_INTERNAL, not a panic (AIRA-134).
func runBoard(ctx context.Context, dispatcher Dispatcher, scope daemon.WorktreeScope, startOverview bool, stdin io.Reader, stdout, stderr io.Writer) int {
	signals := make(chan os.Signal, 2)
	signal.Notify(signals, syscall.SIGINT, syscall.SIGTERM)
	defer signal.Stop(signals)
	overview := startOverview
	for {
		if overview {
			chosen, code := runOverviewMode(ctx, dispatcher, signals, stdin, stdout, stderr, boardHopScreen())
			if chosen == nil {
				return code // `q`, a signal, or a screen-init failure — end the loop.
			}
			scope, overview = *chosen, false
			continue
		}
		toOverview, code := runProjectBoard(ctx, dispatcher, scope, signals, stdin, stdout, stderr, boardHopScreen())
		if !toOverview {
			return code // `q`, a signal, or a screen-init failure — end the loop.
		}
		overview = true
	}
}

// boardScreenFactory, if set, supplies the tcell.Screen for each per-mode runtime
// the outer loop builds (nil in production → a real terminal). Test-only: driving
// a full overview→board→overview→quit cycle through runBoard itself needs
// injectable simulation screens.
var boardScreenFactory func() tcell.Screen

func boardHopScreen() tcell.Screen {
	if boardScreenFactory != nil {
		return boardScreenFactory()
	}
	return nil
}

// runProjectBoard runs ONE per-project board runtime to a transition. It returns
// (toOverview, exitCode): toOverview==true means the operator pressed `o` (or
// back past the top level) to return to the overview and the loop continues;
// false means quit (`q`, a signal, or a screen-init failure) and the loop ends.
//
// The runtime's executor (watch INCLUDED) is fully joined by run() →
// coordinateShutdown → executor.wait() BEFORE runTUIRuntime returns, so reading
// runtime.state afterwards is race-free and no watch goroutine outlives the mode.
// screen is nil in production (a real terminal); tests inject a simulation screen.
//
// The transition gate is the reducer FLAG alone, not the exit code: the flag is
// set ONLY by a key press (which requires a live screen), so a screen-init
// failure or a signal cancel — either of which returns before any key — leaves
// it false and the loop quits (spec §12 / advisor).
func runProjectBoard(ctx context.Context, dispatcher Dispatcher, scope daemon.WorktreeScope, signals <-chan os.Signal, stdin io.Reader, stdout, stderr io.Writer, screen tcell.Screen) (bool, int) {
	runtime := newBoardRuntime(ctx, dispatcher, scope, stdin, stdout, stderr, screen)
	if boardHopObserver != nil {
		boardHopObserver(runtime)
	}
	signalDone := runHopSignalLoop(runtime, signals)
	code := runTUIRuntime(runtime, stderr)
	<-signalDone // join before returning: never two loops live at once (P2 review fix)
	if runtime.state.Board != nil && runtime.state.Board.ToOverview {
		return true, 0
	}
	return false, code
}

// runOverviewMode runs ONE overview runtime to a transition. It returns
// (chosenScope, exitCode): a non-nil scope means the operator pressed Enter on a
// project row and the loop should open that project's board; nil means quit. The
// gate is the Chosen flag alone, for the reason runProjectBoard's is.
func runOverviewMode(ctx context.Context, dispatcher Dispatcher, signals <-chan os.Signal, stdin io.Reader, stdout, stderr io.Writer, screen tcell.Screen) (*daemon.WorktreeScope, int) {
	runtime := newOverviewRuntime(ctx, dispatcher, stdin, stdout, stderr, screen)
	if boardHopObserver != nil {
		boardHopObserver(runtime)
	}
	signalDone := runHopSignalLoop(runtime, signals)
	code := runTUIRuntime(runtime, stderr)
	<-signalDone // join before returning: never two loops live at once (P2 review fix)
	if runtime.state.Overview != nil && runtime.state.Overview.Chosen != nil {
		chosen := *runtime.state.Overview.Chosen
		return &chosen, 0
	}
	return nil, code
}

// runHopSignalLoop starts a per-hop signal loop bound to the runtime's ctx and
// returns a channel that closes when it exits. run() cancels runtime.ctx before
// runTUIRuntime returns, so the caller joins on this channel to guarantee the
// loop is fully gone before the hop returns — no two signal loops are ever live
// at once across a mode transition, and none outlives its hop (P2 review fix).
func runHopSignalLoop(runtime *tuiRuntime, signals <-chan os.Signal) chan struct{} {
	done := make(chan struct{})
	go func() {
		defer close(done)
		runTUISignalLoop(runtime.ctx, signals, runtime.executeRunning.Load, runtime.cancel, nil)
	}()
	return done
}

// boardHopObserver, if set, receives each runtime the mode-switch hop functions
// build, right after construction. Test-only (nil in production): it lets a test
// drive the runtime a hop builds internally and then assert the hop's returned
// transition tuple, so the flag→return-value glue is exercised, not just the
// reducer flag in isolation.
var boardHopObserver func(*tuiRuntime)

// newBoardRuntime builds the board runtime: one project-scoped kanban view,
// watch-on (a ticket mutation invalidates the board), no execute dispatcher
// (read-only), and its own board reducer state.
func newBoardRuntime(parent context.Context, dispatcher Dispatcher, scope daemon.WorktreeScope, stdin io.Reader, stdout, stderr io.Writer, screen tcell.Screen) *tuiRuntime {
	return newTUIRuntimeForViews(parent, dispatcher, nil, scope, stdin, stdout, stderr, screen, boardViews, boardViews, true, newBoardState(), nil)
}

// newOverviewRuntime builds the all-projects overview runtime: a watch-less,
// project-less face (like `aira top`) over the overview view set, with its own
// overview reducer state. paths for the per-project scopes are resolved inside
// the fetch (daemon.PathsFromEnv), so the runtime needs none.
func newOverviewRuntime(parent context.Context, dispatcher Dispatcher, stdin io.Reader, stdout, stderr io.Writer, screen tcell.Screen) *tuiRuntime {
	return newTUIRuntimeForViews(parent, dispatcher, nil, daemon.WorktreeScope{}, stdin, stdout, stderr, screen, overviewViews, overviewViews, false, nil, newOverviewState())
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
	// AIRA-254 info pane. Plain text (dynamic colors OFF) so a ticket title/body
	// containing "[…]" renders literally rather than as a colour tag, and the raw
	// full title needs no escaping. Both content widgets wrap; the body scrolls.
	ui.infoMeta = tview.NewTextView().SetWrap(true).SetDynamicColors(false)
	ui.infoBody = tview.NewTextView().SetWrap(true).SetScrollable(true).SetDynamicColors(false)
	ui.infoArea = tview.NewFlex()
	ui.info = tview.NewFlex().SetDirection(tview.FlexRow).
		AddItem(ui.infoArea, 0, 1, false)
	ui.info.SetBorder(true).SetTitle(" Ticket ")
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
		AddItem(ui.info, boardInfoPaneHeight(0), 0, false).
		AddItem(ui.strip, 0, 1, true).
		AddItem(ui.sessions, 6, 0, false).
		AddItem(ui.footer, 1, 0, false)
	ui.main = main
	// The expand overlay is built with resizable centring flexes (not the fixed
	// centeredPrimitive) so beforeDraw can shrink its 100x30 box to fit a small
	// screen — at 80x24 a fixed 100x30 box would be placed off-screen and its top
	// border, header and first title line clipped (the overlay is now the readable
	// full-detail surface, so that clip would hide real content).
	ui.detailInner = tview.NewFlex().SetDirection(tview.FlexRow).
		AddItem(nil, 0, 1, false).
		AddItem(ui.detail, 30, 0, true).
		AddItem(nil, 0, 1, false)
	ui.detailOuter = tview.NewFlex().
		AddItem(nil, 0, 1, false).
		AddItem(ui.detailInner, 100, 0, true).
		AddItem(nil, 0, 1, false)
	r.outerPages = tview.NewPages().
		AddPage(boardMainPage, main, true, true).
		AddPage(boardDetailPage, ui.detailOuter, true, false).
		AddPage(boardResultsPage, centeredPrimitive(ui.results, 90, 24), true, false).
		AddPage(boardSearchPage, centeredPrimitive(ui.search, 70, 3), true, false)
	r.app.SetRoot(r.outerPages, true)
	r.app.SetInputCapture(r.captureBoardInput)
	// The width seam: screen.Size() is honestly available here, on the UI
	// goroutine, before each draw. Column fit is a PURE function of (width, focus)
	// and the strip is rebuilt only when the visible window actually changes.
	r.app.SetBeforeDrawFunc(func(screen tcell.Screen) bool {
		width, height := screen.Size()
		if width != r.boardUI.width || height != r.boardUI.height {
			r.boardUI.width = width
			r.boardUI.height = height
			r.layoutBoardStrip()
			r.layoutBoardOverlay()
			// Re-COMPOSE the info pane, not just re-lay-it-out: renderBoardInfo's
			// text assignment (and the "+N ↵" clip disclosure) is width-dependent —
			// wide puts title+fields left / body right, narrow stacks the lot into
			// one widget. A tcell resize fires this hook but NOT render() (that needs
			// a reducer message), so without re-composing here a wide↔narrow resize
			// with no follow-up keypress would leave stale text in the wrong widget
			// (title+fields vanish, or a false clip disclosure). renderBoardInfo
			// calls the idempotent layoutBoardInfo itself.
			if r.state.Board != nil {
				r.renderBoardInfo(r.state.Board)
			} else {
				r.layoutBoardInfo()
			}
		}
		return false
	})
}

const (
	// boardInfoWideThreshold is the width (columns) at or above which the info
	// pane splits title+fields | body side-by-side; below it they stack.
	boardInfoWideThreshold = 90
	// boardInfoReservedRows is the fixed vertical cost the info pane must leave for
	// the rest of the board: banner(1) + sessions(6) + footer(1) + a 3-row minimum
	// column strip. The pane never grows past screenHeight - this.
	boardInfoReservedRows = 11
	// boardInfoMinRows is the smallest pane worth showing (border + ~3 content
	// rows). Below it the pane is hidden entirely so the strip keeps the board
	// usable on a short terminal (a fixed pane once pushed the usable minimum to
	// 18 rows and drove the strip to a negative size).
	boardInfoMinRows = 5
)

// boardInfoPaneHeight is the info pane's outer height (0 = hidden): about a
// quarter of the screen, floored so it shows a useful amount, capped at 14, and
// then capped again so it never eats the columns (screenHeight-reserved). If that
// leaves less than a useful pane, it returns 0 and the strip takes the space. It
// depends ONLY on screen height, so the pane does not resize as the cursor moves.
func boardInfoPaneHeight(screenHeight int) int {
	height := screenHeight / 4
	if height < 7 {
		height = 7
	}
	if height > 14 {
		height = 14
	}
	if max := screenHeight - boardInfoReservedRows; height > max {
		height = max
	}
	if height < boardInfoMinRows {
		return 0
	}
	return height
}

// layoutBoardInfo sizes the info pane and rebuilds its internal split. The pane
// takes a quarter of the screen (0 = hidden on a short terminal). The area is
// ALL PROPORTIONAL — no fixed row counts — so tview never gets a negative size:
// wide terminals split title+fields | body (~40% | ~60%, body the larger share
// per the owner's ask), narrow terminals put everything in one wrapping column.
// Idempotent: it only touches the Flexes when (wide, paneHeight) changes.
func (r *tuiRuntime) layoutBoardInfo() {
	if r.boardUI == nil || r.state.Board == nil {
		return
	}
	paneHeight := boardInfoPaneHeight(r.boardUI.height)
	wide := r.boardUI.width >= boardInfoWideThreshold
	if r.boardUI.infoLaidOut && wide == r.boardUI.lastInfoWide && paneHeight == r.boardUI.lastInfoHeight {
		return
	}
	r.boardUI.lastInfoWide, r.boardUI.lastInfoHeight, r.boardUI.infoLaidOut = wide, paneHeight, true

	r.boardUI.main.ResizeItem(r.boardUI.info, paneHeight, 0)
	r.boardUI.infoArea.Clear()
	if paneHeight == 0 {
		return // hidden: nothing to lay out inside
	}
	if wide {
		r.boardUI.infoArea.SetDirection(tview.FlexColumn)
		r.boardUI.infoArea.AddItem(r.boardUI.infoMeta, 0, 2, false) // ~40%: title + fields
		r.boardUI.infoArea.AddItem(r.boardUI.infoBody, 0, 3, false) // ~60%: body
	} else {
		r.boardUI.infoArea.SetDirection(tview.FlexRow)
		r.boardUI.infoArea.AddItem(r.boardUI.infoBody, 0, 1, false) // one wrapping column
	}
}

// layoutBoardOverlay shrinks the expand overlay's fixed 100x30 box to fit the
// screen (tview does not clamp a fixed Flex item, so an oversized box is placed
// off-screen and clipped). Idempotent via the same width/height memo as the
// strip/info layouts (its caller only runs on a size change).
func (r *tuiRuntime) layoutBoardOverlay() {
	if r.boardUI == nil || r.boardUI.detailOuter == nil {
		return
	}
	width := clampOverlayExtent(100, r.boardUI.width)
	height := clampOverlayExtent(30, r.boardUI.height)
	r.boardUI.detailOuter.ResizeItem(r.boardUI.detailInner, width, 0)
	r.boardUI.detailInner.ResizeItem(r.boardUI.detail, height, 0)
}

// clampOverlayExtent bounds an overlay dimension to at most (screen-2) so its
// border stays on-screen, and at least 1.
func clampOverlayExtent(want, screen int) int {
	if fit := screen - 2; want > fit {
		want = fit
	}
	if want < 1 {
		want = 1
	}
	return want
}

// boardInfoOverflow reports how many wrapped display-rows of content spill past
// the rows the pane can show, so the border can disclose a silent clip (spec §14:
// an omission of established values is never silent). It wraps each line the way
// the pane's TextView does — word-wrap, measured in display cells (wide runes
// count 2) — and counts blank separator lines too. 0 means everything fits.
func boardInfoOverflow(content string, width, rows int) int {
	if width < 1 || rows < 1 {
		return 0
	}
	total := 0
	for _, line := range strings.Split(content, "\n") {
		wrapped := tview.WordWrap(line, width)
		if len(wrapped) == 0 {
			total++ // a blank line still occupies a row
			continue
		}
		total += len(wrapped)
	}
	if total > rows {
		return total - rows
	}
	return 0
}

// boardInfoLeftWidth and boardInfoBodyWidth are the content widths of the wide
// pane's two columns (title+fields | body, split ~40/60 inside the pane border).
// They are used only to estimate the clip disclosure, so approximate rounding is
// fine — the exact tview Flex split need not be reproduced.
func boardInfoLeftWidth(width int) int {
	inner := width - 2
	if inner < 2 {
		return 1
	}
	return inner * 2 / 5
}

func boardInfoBodyWidth(width int) int {
	inner := width - 2
	if inner < 2 {
		return 1
	}
	return inner - inner*2/5
}

// layoutBoardStrip rebuilds the visible-column window. It is idempotent: it only
// touches the Flex when the [start,end) window changed, so it is safe to call on
// every render and every draw.
func (r *tuiRuntime) layoutBoardStrip() {
	if r.boardUI == nil || r.state.Board == nil {
		return
	}
	focus := r.state.Board.FocusedCol
	start, end := boardStripWindow(r.boardUI.width, focus, len(r.boardUI.columns), r.state.Board.FullWidth)
	if r.boardUI.laidOut && start == r.boardUI.lastStart && end == r.boardUI.lastEnd {
		return
	}
	r.boardUI.lastStart, r.boardUI.lastEnd, r.boardUI.laidOut = start, end, true
	r.boardUI.strip.Clear()
	for i := start; i < end; i++ {
		r.boardUI.strip.AddItem(r.boardUI.columns[i], 0, 1, false)
	}
}

// renderBoardInfo populates the info pane for the current selection. The border
// carries the instant header (id · status · severity · kind) from the loaded
// card plus a "+N ↵" clip disclosure when content spills; the FULL raw title
// leads the content (so it is the last thing to clip, never truncated); then the
// fetched meta/body — "loading…" until it lands, "unevaluated (CODE)" if a
// section could not be read. Wide splits title+fields | body; narrow stacks them
// into the one wrapping column. An empty column shows an explicit "no ticket".
func (r *tuiRuntime) renderBoardInfo(bs *boardState) {
	r.layoutBoardInfo()
	card, ok := boardSelectedCard(*bs)
	if !ok {
		r.boardUI.info.SetTitle(" Ticket ")
		r.boardUI.infoMeta.SetText("")
		r.boardUI.infoBody.SetText("no ticket selected")
		return
	}
	// Only render a detail that actually describes THIS card; a detail held for a
	// different ticket collapses to "loading…" rather than showing under the wrong
	// title (boardInfoDetail is the honesty seam).
	detail := boardInfoDetail(card, bs.Detail)
	// AIRA-257: the toggle swaps in the plain-English rewrite (with a header label)
	// when it is this card's and shown; else the original, so the original is always
	// one 't' away and nothing fabricated is shown. Swap BEFORE the overflow calc so
	// the "+N ↵" disclosure measures what is actually rendered.
	title, body, humanizeLabel := boardHumanizeDisplay(card.ID, card.Title, boardDetailBodyText(detail), bs.Humanize)
	meta := strings.Join(boardDetailMetaLines(card, detail), "\n")

	innerRows := boardInfoPaneHeight(r.boardUI.height) - 2
	overflow := 0
	if r.boardUI.width >= boardInfoWideThreshold {
		left := title + "\n\n" + meta
		r.boardUI.infoMeta.SetText(left)
		r.boardUI.infoBody.SetText(body)
		overflow = boardInfoOverflow(left, boardInfoLeftWidth(r.boardUI.width), innerRows)
		if o := boardInfoOverflow(body, boardInfoBodyWidth(r.boardUI.width), innerRows); o > overflow {
			overflow = o
		}
	} else {
		stacked := title + "\n\n" + meta + "\n\n" + body
		r.boardUI.infoMeta.SetText("")
		r.boardUI.infoBody.SetText(stacked)
		overflow = boardInfoOverflow(stacked, r.boardUI.width-2, innerRows)
	}

	header := boardDetailHeaderLine(card)
	if humanizeLabel != "" {
		header += " · " + humanizeLabel // AIRA-257: mark the pane as an AI paraphrase
	}
	if overflow > 0 {
		// Disclose the clip rather than silently drop established values (spec §14);
		// Enter opens the full detail.
		header += " · +" + strconv.Itoa(overflow) + " ↵"
	}
	r.boardUI.info.SetTitle(" " + header + " ")
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
	r.renderBoardInfo(bs)

	// Overlay precedence: expand overlay > search input > results list. Exactly
	// one may be visible, so hide the others every render to avoid a stale overlay.
	detailShown := bs.Expanded
	resultsShown := !detailShown && !r.boardUI.inputOpen && r.boardUI.resultsOpen && bs.Search.Active
	if detailShown {
		r.boardUI.detail.SetTitle(" " + bs.Detail.ID + " (Esc to close) ")
		r.boardUI.detail.SetText(boardDetailOverlayText(bs.Detail.ID, bs.Detail, bs.Humanize))
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
	if r.state.Board != nil && r.state.Board.Expanded {
		switch {
		case event.Key() == tcell.KeyRune && event.Rune() == 'q':
			r.applyBoardAction(boardActQuit) // q quits globally, even from the overlay
			return nil
		case event.Key() == tcell.KeyEscape:
			r.applyBoardAction(boardActBack)
			return nil
		case event.Key() == tcell.KeyRune && event.Rune() == 't':
			// AIRA-257: 't' toggles the plain-English rewrite in the overlay too. The
			// reducer targets Detail.ID while Expanded, so it rewrites the ticket the
			// overlay is showing (which may be an unloaded search hit).
			r.applyBoardAction(boardActTranslate)
			return nil
		}
		return event // let the expand overlay scroll (↑/↓/PgUp/PgDn)
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
	case tcell.KeyHome:
		action = boardActCardFirst
	case tcell.KeyEnd:
		action = boardActCardLast
	case tcell.KeyPgUp:
		action = boardActCardPageUp
	case tcell.KeyPgDn:
		action = boardActCardPageDown
	case tcell.KeyEnter:
		action = boardActExpand
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
		case 'o':
			action = boardActToOverview
		case 'f':
			action = boardActFullWidth
		case 't':
			action = boardActTranslate
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
	keys := "←/→ h/l column · ↑/↓ j/k card · Enter expand · t plain-English · f full-width · / search · o overview · r refresh · q quit"
	if bs != nil && bs.Search.Active {
		if label := boardSearchLabel(bs.Search); label != "" {
			return "search \"" + tview.Escape(bs.Search.Query) + "\": " + label + "   ·   Esc clears   ·   " + keys
		}
	}
	return keys
}
