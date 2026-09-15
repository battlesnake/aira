package main

// AIRA-252 `aira board` — the pure board reducer.
//
// The board's interactive state (focused column, per-column selection, the
// horizontal-scroll window, search, drill-in) lives here as a value that pure
// functions transform, mirroring the Elm discipline of the rest of the TUI. The
// runtime shell (tui_board.go) is a thin imperative face over these transitions,
// and every navigation / search decision is unit tested WITHOUT a terminal.

import (
	"regexp"
	"strconv"
	"strings"
)

// boardColMinWidth is one column's minimum terminal width, border included.
// Below this a card id + a badge or two no longer fits, so the horizontal-scroll
// window shows fewer columns rather than clamping every one to illegibility.
const boardColMinWidth = 20

type boardSearchResult struct {
	ID      string
	Snippet string
}

type boardSearchState struct {
	Active    bool
	Query     string
	Pending   bool
	State     string // "", "ok", "no-matches", "not-found", "unevaluated:<code>"
	Truncated bool   // grep hit the 50-cap: the result set is disclosed as partial
	Results   []boardSearchResult
	ResultIdx int             // cursor in the results overlay
	MatchIDs  map[string]bool // every match id (for card highlighting)
	// ContentIDs are matches resolved OUTSIDE the loaded cards — a grep content
	// hit, or a `show`-probe hit. They persist across a refresh (they were never a
	// loaded-card match), which is what lets boardApplyModel drop a former
	// client-match that has since scrolled out without dropping a real content hit.
	ContentIDs map[string]bool
}

type boardState struct {
	Model     boardModel
	HasData   bool
	Stale     bool
	ErrorCode string
	// WatchError is the live-refresh (event-watch) failure, kept SEPARATE from
	// Stale: a watch drop means "new mutations may not have arrived yet, press r",
	// not "the shown data is last-good" — the reads themselves are still fresh. It
	// therefore never marks columns stale, and is cleared on the next good batch.
	WatchError string
	FocusedCol int
	Selected   []int
	Search     boardSearchState
	DrillID    string
	Detail     string
	// ToOverview is the Increment-2 mode-transition flag (spec §12): set (with a
	// cmdQuit) when the operator presses `o`, so the outer runBoard loop, after
	// this runtime tears down, switches to the all-projects overview instead of
	// quitting. It is read only after run() returns (race-free — the UI goroutine
	// has exited), and defaults false so a signal/quit ends the loop.
	ToOverview bool
}

func newBoardState() *boardState {
	return &boardState{Search: boardSearchState{MatchIDs: map[string]bool{}}}
}

func cloneBoardState(source *boardState) *boardState {
	if source == nil {
		return nil
	}
	clone := *source
	clone.Selected = append([]int(nil), source.Selected...)
	clone.Model.Columns = append([]boardColumn(nil), source.Model.Columns...)
	for i := range clone.Model.Columns {
		clone.Model.Columns[i].Cards = append([]boardCard(nil), source.Model.Columns[i].Cards...)
	}
	clone.Model.Sessions = append([]boardSessionRow(nil), source.Model.Sessions...)
	clone.Model.Warnings = append([]string(nil), source.Model.Warnings...)
	clone.Search.Results = append([]boardSearchResult(nil), source.Search.Results...)
	clone.Search.MatchIDs = make(map[string]bool, len(source.Search.MatchIDs))
	for id, ok := range source.Search.MatchIDs {
		clone.Search.MatchIDs[id] = ok
	}
	clone.Search.ContentIDs = make(map[string]bool, len(source.Search.ContentIDs))
	for id, ok := range source.Search.ContentIDs {
		clone.Search.ContentIDs[id] = ok
	}
	return &clone
}

// boardApplyModel stores a freshly-fetched view-model and RECONCILES the
// interactive state against it: the selection is clamped into each column's new
// bounds and the focus into the column count, so a card that vanished on refresh
// never leaves a dangling selection. The last-good stale flag is cleared.
func boardApplyModel(state boardState, model boardModel) boardState {
	state.Model = model
	state.HasData = true
	state.Stale = false
	state.ErrorCode = ""
	if len(state.Selected) != len(model.Columns) {
		next := make([]int, len(model.Columns))
		copy(next, state.Selected)
		state.Selected = next
	}
	for i := range state.Selected {
		state.Selected[i] = clampIndex(state.Selected[i], len(model.Columns[i].Cards))
	}
	if len(model.Columns) == 0 {
		state.FocusedCol = 0
	} else if state.FocusedCol >= len(model.Columns) {
		state.FocusedCol = len(model.Columns) - 1
	} else if state.FocusedCol < 0 {
		state.FocusedCol = 0
	}
	if state.Search.Active {
		state = boardRecomputeSearchMatches(state)
	}
	return state
}

// boardRecomputeSearchMatches re-derives the CLIENT-SIDE (id/title) match set
// against the freshly-loaded cards while PRESERVING grep-only hits (matches whose
// id is not a loaded card). Without this, a background refresh left the highlight
// set stale — pointing at cards that moved or vanished (P2.9). Pure and cheap.
func boardRecomputeSearchMatches(state boardState) boardState {
	if !state.Search.Active || state.Search.Query == "" {
		return state
	}
	// Keep the CONTENT hits (grep / probe) — never former client-matches, which
	// were only matches because their card was loaded and matched by title/id.
	kept := make([]boardSearchResult, 0, len(state.Search.Results))
	for _, result := range state.Search.Results {
		if state.Search.ContentIDs[result.ID] {
			kept = append(kept, result)
		}
	}
	client := boardClientSearch(boardState{Model: state.Model}, state.Search.Query).Search
	merged := append([]boardSearchResult(nil), client.Results...)
	matchIDs := map[string]bool{}
	for _, result := range client.Results {
		matchIDs[result.ID] = true
	}
	for _, result := range kept {
		if !matchIDs[result.ID] {
			matchIDs[result.ID] = true
			merged = append(merged, result)
		}
	}
	state.Search.Results = merged
	state.Search.MatchIDs = matchIDs
	state.Search.ResultIdx = clampIndex(state.Search.ResultIdx, len(merged))
	return state
}

// boardApplyError keeps the last-good columns and marks them stale rather than
// wiping the board on a transient read failure (spec §14).
func boardApplyError(state boardState, code string) boardState {
	state.Stale = true
	state.ErrorCode = code
	return state
}

func clampIndex(index, length int) int {
	if length == 0 {
		return 0
	}
	if index < 0 {
		return 0
	}
	if index >= length {
		return length - 1
	}
	return index
}

func boardMoveColumn(state boardState, delta int) boardState {
	if len(state.Model.Columns) == 0 {
		return state
	}
	next := state.FocusedCol + delta
	if next < 0 {
		next = 0
	}
	if next >= len(state.Model.Columns) {
		next = len(state.Model.Columns) - 1
	}
	state.FocusedCol = next
	return state
}

func boardMoveCard(state boardState, delta int) boardState {
	if len(state.Model.Columns) == 0 {
		return state
	}
	if len(state.Selected) != len(state.Model.Columns) {
		next := make([]int, len(state.Model.Columns))
		copy(next, state.Selected)
		state.Selected = next
	}
	column := state.Model.Columns[state.FocusedCol]
	state.Selected[state.FocusedCol] = clampIndex(state.Selected[state.FocusedCol]+delta, len(column.Cards))
	return state
}

// boardVisibleColumns is the WIDTH SEAM: a pure function of (width, focus, count)
// giving the [start,end) window of columns to draw, always keeping the focused
// column in view and always showing at least one column. It reads no screen; the
// runtime passes screen.Size() in.
func boardVisibleColumns(width, focus, count int) (start, end int) {
	if count <= 0 {
		return 0, 0
	}
	fit := width / boardColMinWidth
	if fit < 1 {
		fit = 1
	}
	if fit > count {
		fit = count
	}
	if focus < 0 {
		focus = 0
	}
	if focus >= count {
		focus = count - 1
	}
	start = focus - fit + 1
	if start < 0 {
		start = 0
	}
	// Prefer filling the window from the left once the focus is near the end.
	if start+fit > count {
		start = count - fit
	}
	if focus < start {
		start = focus
	}
	return start, start + fit
}

// boardSelectedCardID is the id under the cursor in the focused column, or "".
func boardSelectedCardID(state boardState) string {
	if len(state.Model.Columns) == 0 || state.FocusedCol >= len(state.Model.Columns) {
		return ""
	}
	column := state.Model.Columns[state.FocusedCol]
	if state.FocusedCol >= len(state.Selected) {
		return ""
	}
	row := state.Selected[state.FocusedCol]
	if row < 0 || row >= len(column.Cards) {
		return ""
	}
	return column.Cards[row].ID
}

// boardIDShaped reports whether a query is a genuine ticket id (or id fragment)
// to resolve against loaded rows / `show`, rather than a content phrase for grep.
//
// It matches a bare number (247) or a prefixed id — AIRA-247, FEE-BL-12, STON-5:
// one or more letter-led segments joined by hyphens, ending in "-<digits>". It
// deliberately does NOT treat EVERY hyphen as id-shaped — a hyphenated CONTENT
// word (in-progress, no-TTY, cgroup-kill) must reach grep, or the board would
// fabricate "no matches" for it (P1.4). The old hyphen→FTS-hazard guard is dead:
// boardGrepPhrase quotes every query as a phrase and store.Search passes it to
// FTS `MATCH ?` unpreprocessed (search.go:196-200), so a hyphen no longer breaks it.
func boardIDShaped(query string) bool {
	return boardIDPattern.MatchString(strings.TrimSpace(query))
}

var boardIDPattern = regexp.MustCompile(`^([0-9]+|[A-Za-z][A-Za-z0-9]*(-[A-Za-z][A-Za-z0-9]*)*-[0-9]+)$`)

// boardGrepPhrase quotes the query as a single FTS phrase, doubling embedded
// quotes, so a query with FTS operators or punctuation is matched literally
// instead of parsed as syntax (spec §10).
func boardGrepPhrase(query string) string {
	return `"` + strings.ReplaceAll(query, `"`, `""`) + `"`
}

// boardClientSearch computes the CLIENT-SIDE match set — id and title substrings
// over the loaded cards — and opens the search. It sets no terminal State; the
// caller decides whether a grep dispatch will follow (non-id queries) or the
// search is client-only (id-shaped queries).
func boardClientSearch(state boardState, query string) boardState {
	needle := strings.ToLower(strings.TrimSpace(query))
	results := make([]boardSearchResult, 0)
	seen := map[string]bool{}
	for _, column := range state.Model.Columns {
		for _, card := range column.Cards {
			if card.ID == "" || seen[card.ID] {
				continue
			}
			if strings.Contains(strings.ToLower(card.ID), needle) || strings.Contains(strings.ToLower(card.Title), needle) {
				seen[card.ID] = true
				results = append(results, boardSearchResult{ID: card.ID, Snippet: card.Title})
			}
		}
	}
	state.Search = boardSearchState{Active: true, Query: query, Results: results, MatchIDs: seen, ContentIDs: map[string]bool{}}
	return state
}

// boardFinalizeState sets the terminal State label from the current results:
// "ok" when there are matches, "no-matches" when genuinely empty.
func boardFinalizeState(state boardState) boardState {
	if len(state.Search.Results) == 0 {
		state.Search.State = "no-matches"
	} else {
		state.Search.State = "ok"
	}
	return state
}

// boardMergeGrep merges grep content results into an OPEN search. A grep that
// could not be evaluated (E_INDEX_UNEVALUATED → unevaluated:true) or was refused
// (E_QUERY_INVALID and friends) yields the UNEVALUATED state — never "no
// results" (spec §10). Otherwise the rows are deduped into the client matches.
func boardMergeGrep(state boardState, fetch boardSearchFetch) boardState {
	state.Search.Pending = false
	state.Search.Truncated = fetch.Truncated
	if state.Search.MatchIDs == nil {
		state.Search.MatchIDs = map[string]bool{}
	}
	if state.Search.ContentIDs == nil {
		state.Search.ContentIDs = map[string]bool{}
	}
	for _, row := range fetch.Rows {
		id := textCell(row["id"])
		if id == "" {
			continue
		}
		state.Search.ContentIDs[id] = true // a grep content hit, persists across refresh
		if state.Search.MatchIDs[id] {
			continue
		}
		state.Search.MatchIDs[id] = true
		state.Search.Results = append(state.Search.Results, boardSearchResult{
			ID: id, Snippet: boardEscapeTruncate(textCell(row["snippet"])),
		})
	}
	if fetch.Code != "" {
		state.Search.State = "unevaluated:" + fetch.Code
		return state
	}
	if fetch.Unevaluated {
		state.Search.State = "unevaluated:E_INDEX_UNEVALUATED"
		return state
	}
	return boardFinalizeState(state)
}

// onBoardGetResult resolves an id-shaped query that matched no loaded card by a
// `show` probe (P2.5): a found ticket becomes an openable result; a genuine
// E_NOT_FOUND is an honest "not found" state, never conflated with "no matches"
// (the ticket could have been in a truncated column tail).
func onBoardGetResult(state tuiState, fetch boardGetFetch) (tuiState, []tuiCmd) {
	state = cloneTUIState(state)
	if state.Board == nil || !state.Board.Search.Active || state.Board.Search.Query != fetch.Query {
		return state, nil
	}
	search := &state.Board.Search
	search.Pending = false
	if !fetch.Found {
		search.State = "not-found"
		return state, nil
	}
	if search.MatchIDs == nil {
		search.MatchIDs = map[string]bool{}
	}
	if search.ContentIDs == nil {
		search.ContentIDs = map[string]bool{}
	}
	search.ContentIDs[fetch.Query] = true // a probe-resolved id, persists across refresh
	if !search.MatchIDs[fetch.Query] {
		search.MatchIDs[fetch.Query] = true
		search.Results = append(search.Results, boardSearchResult{ID: fetch.Query, Snippet: boardEscapeTruncate(fetch.Title)})
	}
	*state.Board = boardFinalizeState(*state.Board)
	return state, nil
}

// boardSearchLabel renders the honest result state, never conflating an
// unevaluated search with an empty one.
func boardSearchLabel(search boardSearchState) string {
	if !search.Active || search.Query == "" {
		return "" // open but nothing submitted yet — no fabricated "0 matches"
	}
	if search.Pending {
		return "searching…"
	}
	if strings.HasPrefix(search.State, "unevaluated:") {
		return "search unevaluated: " + strings.TrimPrefix(search.State, "unevaluated:")
	}
	if search.State == "no-matches" {
		return "no matches"
	}
	if search.State == "not-found" {
		return "not found"
	}
	label := strconv.Itoa(len(search.Results)) + " match" + plural(len(search.Results))
	if search.Truncated {
		// grep hit the 50-cap: disclose that the count is a floor, not the total.
		label = strconv.Itoa(len(search.Results)) + "+ matches (truncated)"
	}
	return label
}

func plural(n int) string {
	if n == 1 {
		return ""
	}
	return "es"
}

// boardAction is the normalised keypress the runtime maps every board key into,
// so the pure reducer never sees a raw tcell key (arrows and letters both arrive
// as one action) and the transitions stay terminal-free and testable.
type boardAction int

const (
	boardActNone boardAction = iota
	boardActColLeft
	boardActColRight
	boardActCardUp
	boardActCardDown
	boardActDrillIn
	boardActBack
	boardActRefresh
	boardActQuit
	boardActToOverview // AIRA-252 Increment 2: `o` returns to the all-projects overview
)

// onBoardAction is the tuiState-level board reducer wrapper: it clones state,
// applies the pure boardState transition, and emits any command (a drill-in
// detail fetch, a refresh, a quit). Navigation is pure local state and emits
// none.
func onBoardAction(state tuiState, action boardAction) (tuiState, []tuiCmd) {
	state = cloneTUIState(state)
	if state.Board == nil {
		return state, nil
	}
	switch action {
	case boardActQuit:
		state.ShuttingDown = true
		return state, []tuiCmd{{Kind: cmdQuit}}
	case boardActToOverview:
		// Transition, not quit: the outer loop reads ToOverview after teardown and
		// opens the overview. cmdQuit stops THIS runtime cleanly (spec §12).
		state.Board.ToOverview = true
		state.ShuttingDown = true
		return state, []tuiCmd{{Kind: cmdQuit}}
	case boardActRefresh:
		return requestPanelRefresh(state, viewBoard)
	case boardActColLeft:
		*state.Board = boardMoveColumn(*state.Board, -1)
	case boardActColRight:
		*state.Board = boardMoveColumn(*state.Board, +1)
	case boardActCardUp:
		*state.Board = boardMoveCard(*state.Board, -1)
	case boardActCardDown:
		*state.Board = boardMoveCard(*state.Board, +1)
	case boardActDrillIn:
		id := boardSelectedCardID(*state.Board)
		if id == "" {
			return state, nil
		}
		state.Board.DrillID = id
		state.Board.Detail = "loading…"
		panel := state.Panels[viewBoard]
		return state, []tuiCmd{{Kind: cmdFetch, View: viewBoard, Generation: panel.Generation, DetailID: id}}
	case boardActBack:
		switch {
		case state.Board.DrillID != "":
			state.Board.DrillID = ""
			state.Board.Detail = ""
		case state.Board.Search.Active:
			state.Board.Search = boardSearchState{MatchIDs: map[string]bool{}}
		}
	}
	return state, nil
}

// onBoardSearchSubmit resolves a query the three ways of spec §10:
//   - id-shaped with a loaded match → client-side, done.
//   - id-shaped with NO loaded match → a `show` probe (cmdBoardGet), so a ticket
//     in a truncated column tail still resolves rather than reading "no matches".
//   - content → client-side title/id filter AND a grep phrase search, merged.
func onBoardSearchSubmit(state tuiState, query string) (tuiState, []tuiCmd) {
	state = cloneTUIState(state)
	if state.Board == nil {
		return state, nil
	}
	if strings.TrimSpace(query) == "" {
		state.Board.Search = boardSearchState{MatchIDs: map[string]bool{}}
		return state, nil
	}
	*state.Board = boardClientSearch(*state.Board, query)
	if boardIDShaped(query) {
		if len(state.Board.Search.Results) > 0 {
			*state.Board = boardFinalizeState(*state.Board)
			return state, nil
		}
		state.Board.Search.Pending = true
		return state, []tuiCmd{{Kind: cmdBoardGet, Search: query}}
	}
	state.Board.Search.Pending = true
	return state, []tuiCmd{{Kind: cmdBoardSearch, Search: query}}
}

// boardResultMove moves the results-overlay cursor, clamped.
func boardResultMove(state boardState, delta int) boardState {
	state.Search.ResultIdx = clampIndex(state.Search.ResultIdx+delta, len(state.Search.Results))
	return state
}

// boardSelectedResultID is the id under the results-overlay cursor, or "".
func boardSelectedResultID(state boardState) string {
	idx := state.Search.ResultIdx
	if idx < 0 || idx >= len(state.Search.Results) {
		return ""
	}
	return state.Search.Results[idx].ID
}

// boardCardLoaded reports whether an id is a currently-loaded card (so opening it
// is a jump-to-column, not a detail drill-in for an unloaded grep-only hit).
func boardCardLoaded(state boardState, id string) bool {
	for _, column := range state.Model.Columns {
		for _, card := range column.Cards {
			if card.ID == id {
				return true
			}
		}
	}
	return false
}

// boardJumpToCard focuses the column+row holding id and closes the search. Used
// when a results-overlay selection is a loaded card.
func boardJumpToCard(state boardState, id string) boardState {
	for ci, column := range state.Model.Columns {
		for ri, card := range column.Cards {
			if card.ID == id {
				state.FocusedCol = ci
				if len(state.Selected) != len(state.Model.Columns) {
					next := make([]int, len(state.Model.Columns))
					copy(next, state.Selected)
					state.Selected = next
				}
				state.Selected[ci] = ri
				state.Search = boardSearchState{MatchIDs: map[string]bool{}}
				return state
			}
		}
	}
	return state
}

// onBoardResultOpen opens the selected result: a LOADED card is jumped to; an
// unloaded id (a grep-only content hit in a truncated column, or a probed id) is
// opened as a detail drill-in via the same fetchTicketDetail path (P2.7).
func onBoardResultOpen(state tuiState) (tuiState, []tuiCmd) {
	state = cloneTUIState(state)
	if state.Board == nil {
		return state, nil
	}
	id := boardSelectedResultID(*state.Board)
	if id == "" {
		return state, nil
	}
	if boardCardLoaded(*state.Board, id) {
		*state.Board = boardJumpToCard(*state.Board, id)
		return state, nil
	}
	state.Board.DrillID = id
	state.Board.Detail = "loading…"
	panel := state.Panels[viewBoard]
	return state, []tuiCmd{{Kind: cmdFetch, View: viewBoard, Generation: panel.Generation, DetailID: id}}
}

// onBoardSearchOpen opens the search overlay with a clean slate.
func onBoardSearchOpen(state tuiState) tuiState {
	state = cloneTUIState(state)
	if state.Board != nil {
		state.Board.Search = boardSearchState{Active: true, MatchIDs: map[string]bool{}}
	}
	return state
}

// onBoardSearchResult merges a grep reply into the open search. A result for a
// query the operator has since replaced is dropped.
func onBoardSearchResult(state tuiState, fetch boardSearchFetch) (tuiState, []tuiCmd) {
	state = cloneTUIState(state)
	if state.Board == nil || !state.Board.Search.Active || state.Board.Search.Query != fetch.Query {
		return state, nil
	}
	*state.Board = boardMergeGrep(*state.Board, fetch)
	return state, nil
}
