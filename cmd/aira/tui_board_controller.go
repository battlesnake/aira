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
	Active   bool
	Query    string
	Pending  bool
	State    string // "", "ok", "no-matches", "unevaluated:<code>"
	Results  []boardSearchResult
	MatchIDs map[string]bool
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

// boardIDShaped reports whether a search query must be resolved CLIENT-SIDE
// rather than sent to grep. Any query carrying a hyphen is id-shaped: the hyphen
// is an FTS-syntax hazard (E_QUERY_INVALID), so a hyphenated selector like
// AIRA-247 or FEE-BL-12 must never reach grep (spec §10). A bare number is an id
// fragment too.
func boardIDShaped(query string) bool {
	query = strings.TrimSpace(query)
	if query == "" {
		return false
	}
	if strings.ContainsRune(query, '-') {
		return true
	}
	return boardAllDigits.MatchString(query)
}

var boardAllDigits = regexp.MustCompile(`^[0-9]+$`)

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
	state.Search = boardSearchState{Active: true, Query: query, Results: results, MatchIDs: seen}
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
	if state.Search.MatchIDs == nil {
		state.Search.MatchIDs = map[string]bool{}
	}
	for _, row := range fetch.Rows {
		id := textCell(row["id"])
		if id == "" || state.Search.MatchIDs[id] {
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
	return strconv.Itoa(len(search.Results)) + " match" + plural(len(search.Results))
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

// onBoardSearchSubmit resolves a query the three ways of spec §10. An id-shaped
// query (any hyphen, or a bare number) is resolved CLIENT-SIDE only and never
// sent to grep; any other query filters loaded rows client-side AND dispatches a
// grep content search whose result is merged by onBoardSearchResult.
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
		*state.Board = boardFinalizeState(*state.Board)
		return state, nil
	}
	state.Board.Search.Pending = true
	return state, []tuiCmd{{Kind: cmdBoardSearch, Search: query}}
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
