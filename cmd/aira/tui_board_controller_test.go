package main

import (
	"strconv"
	"strings"
	"testing"
)

func boardTUIState(bs *boardState) tuiState {
	state := newTUIStateForViews(16, boardViews, boardViews)
	state.Board = bs
	return state
}

// modelWithColumns builds a boardState whose columns carry the given card counts.
func stateWithColumns(counts ...int) boardState {
	model := boardModel{}
	for i, count := range counts {
		column := boardColumn{Status: "planned"}
		for c := 0; c < count; c++ {
			column.Cards = append(column.Cards, boardCard{ID: "AIRA-" + strconv.Itoa(i*100+c), Title: "card"})
		}
		model.Columns = append(model.Columns, column)
	}
	state := boardState{}
	return boardApplyModel(state, model)
}

// TestBoardVisibleColumns is the width-seam guard: a pure function of
// (width, focus, count) that always keeps the focus in view and shows ≥1 column.
func TestBoardVisibleColumns(t *testing.T) {
	cases := []struct {
		name               string
		width, focus, n    int
		wantStart, wantEnd int
	}{
		{"40col-focus-left", 40, 0, 7, 0, 2},
		{"40col-focus-right", 40, 6, 7, 5, 7},
		{"40col-focus-mid", 40, 3, 7, 2, 4},
		{"narrow-one-column", 15, 4, 7, 4, 5},
		{"zero-width-one-column", 0, 2, 7, 2, 3},
		{"wide-shows-all", 400, 3, 7, 0, 7},
		{"no-columns", 80, 0, 0, 0, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			start, end := boardVisibleColumns(tc.width, tc.focus, tc.n)
			if start != tc.wantStart || end != tc.wantEnd {
				t.Fatalf("boardVisibleColumns(%d,%d,%d) = [%d,%d), want [%d,%d)", tc.width, tc.focus, tc.n, start, end, tc.wantStart, tc.wantEnd)
			}
			if tc.n > 0 && (tc.focus < start || tc.focus >= end) {
				t.Fatalf("focus %d not in visible window [%d,%d)", tc.focus, start, end)
			}
			if tc.n > 0 && end <= start {
				t.Fatalf("window [%d,%d) shows no columns", start, end)
			}
		})
	}
}

func TestBoardMoveColumnClamps(t *testing.T) {
	state := stateWithColumns(1, 1, 1)
	if got := boardMoveColumn(state, -1).FocusedCol; got != 0 {
		t.Fatalf("left at 0 → %d, want 0", got)
	}
	state.FocusedCol = 2
	if got := boardMoveColumn(state, 1).FocusedCol; got != 2 {
		t.Fatalf("right at last → %d, want 2", got)
	}
	if got := boardMoveColumn(state, -1).FocusedCol; got != 1 {
		t.Fatalf("left from 2 → %d, want 1", got)
	}
}

func TestBoardMoveCardClamps(t *testing.T) {
	state := stateWithColumns(3, 0)
	state = boardMoveCard(state, 1)
	if state.Selected[0] != 1 {
		t.Fatalf("card down → %d, want 1", state.Selected[0])
	}
	state = boardMoveCard(boardMoveCard(state, 1), 1) // 2, then clamp at 2
	if state.Selected[0] != 2 {
		t.Fatalf("card down clamps at last → %d, want 2", state.Selected[0])
	}
	// An empty column never produces an out-of-range selection.
	state.FocusedCol = 1
	if got := boardMoveCard(state, 1).Selected[1]; got != 0 {
		t.Fatalf("card down in empty column → %d, want 0", got)
	}
}

func TestBoardApplyModelClampsSelection(t *testing.T) {
	state := stateWithColumns(5)
	state.Selected[0] = 4
	shrunk := boardApplyModel(state, boardModel{Columns: []boardColumn{{Status: "planned", Cards: []boardCard{{ID: "AIRA-0"}, {ID: "AIRA-1"}}}}})
	if shrunk.Selected[0] != 1 {
		t.Fatalf("selection not clamped after shrink → %d, want 1", shrunk.Selected[0])
	}
}

func TestBoardApplyErrorKeepsLastGood(t *testing.T) {
	good := stateWithColumns(2, 3)
	stale := boardApplyError(good, "E_DAEMON_UNREACHABLE")
	if len(stale.Model.Columns) != 2 || !stale.Stale || stale.ErrorCode != "E_DAEMON_UNREACHABLE" {
		t.Fatalf("apply-error dropped last-good or banner: %#v", stale)
	}
}

func TestBoardIDShaped(t *testing.T) {
	for _, q := range []string{"AIRA-247", "247", "FEE-BL-12", "aira-1"} {
		if !boardIDShaped(q) {
			t.Fatalf("%q should be id-shaped (must not reach grep)", q)
		}
	}
	for _, q := range []string{"parser", "fix the bug", "review"} {
		if boardIDShaped(q) {
			t.Fatalf("%q should NOT be id-shaped", q)
		}
	}
}

func TestBoardGrepPhraseQuotesAndEscapes(t *testing.T) {
	if got := boardGrepPhrase("parser bug"); got != `"parser bug"` {
		t.Fatalf("phrase = %q, want quoted", got)
	}
	if got := boardGrepPhrase(`a"b`); got != `"a""b"` {
		t.Fatalf("embedded quote not doubled: %q", got)
	}
}

func TestBoardClientSearchMatchesIDAndTitle(t *testing.T) {
	model := boardModel{Columns: []boardColumn{{Status: "planned", Cards: []boardCard{
		{ID: "AIRA-1", Title: "rewrite the parser"},
		{ID: "AIRA-2", Title: "unrelated"},
	}}}}
	state := boardClientSearch(boardState{Model: model}, "parser")
	if len(state.Search.Results) != 1 || state.Search.Results[0].ID != "AIRA-1" {
		t.Fatalf("title match = %#v, want AIRA-1", state.Search.Results)
	}
	byID := boardClientSearch(boardState{Model: model}, "aira-2")
	if len(byID.Search.Results) != 1 || byID.Search.Results[0].ID != "AIRA-2" {
		t.Fatalf("id match = %#v, want AIRA-2", byID.Search.Results)
	}
}

func TestBoardSearchIDShapedDoesNotDispatchGrep(t *testing.T) {
	model := boardModel{Columns: []boardColumn{{Status: "planned", Cards: []boardCard{{ID: "AIRA-1", Title: "one"}}}}}
	state, commands := onBoardSearchSubmit(boardTUIState(&boardState{Model: model}), "AIRA-1")
	for _, command := range commands {
		if command.Kind == cmdBoardSearch {
			t.Fatalf("id-shaped query dispatched grep (hyphen would break FTS)")
		}
	}
	if state.Board.Search.State == "" {
		t.Fatalf("id-shaped search was not finalised: %#v", state.Board.Search)
	}
}

func TestBoardSearchContentDispatchesGrep(t *testing.T) {
	state, commands := onBoardSearchSubmit(boardTUIState(newBoardState()), "parser")
	found := false
	for _, command := range commands {
		if command.Kind == cmdBoardSearch && command.Search == "parser" {
			found = true
		}
	}
	if !found {
		t.Fatalf("content query did not dispatch a grep command: %#v", commands)
	}
	if !state.Board.Search.Pending {
		t.Fatalf("content search not marked pending")
	}
}

// TestBoardMergeGrepUnevaluated is the false-pass guard: a grep that could not
// be evaluated must read "search unevaluated", NEVER "no matches" (spec §10).
func TestBoardMergeGrepUnevaluated(t *testing.T) {
	base := boardState{Search: boardSearchState{Active: true, Query: "parser", MatchIDs: map[string]bool{}}}

	indexUneval := boardMergeGrep(base, boardSearchFetch{Query: "parser", Unevaluated: true})
	if label := boardSearchLabel(indexUneval.Search); !strings.Contains(label, "search unevaluated") || strings.Contains(label, "no matches") {
		t.Fatalf("index-unevaluated label = %q, want 'search unevaluated', not 'no matches'", label)
	}

	queryInvalid := boardMergeGrep(base, boardSearchFetch{Query: "parser", Code: "E_QUERY_INVALID"})
	if label := boardSearchLabel(queryInvalid.Search); !strings.Contains(label, "search unevaluated") || !strings.Contains(label, "E_QUERY_INVALID") {
		t.Fatalf("query-invalid label = %q, want 'search unevaluated: E_QUERY_INVALID'", label)
	}
}

func TestBoardMergeGrepDedupAndEmpty(t *testing.T) {
	withClient := boardState{Search: boardSearchState{
		Active: true, Query: "parser",
		Results:  []boardSearchResult{{ID: "AIRA-1", Snippet: "one"}},
		MatchIDs: map[string]bool{"AIRA-1": true},
	}}
	merged := boardMergeGrep(withClient, boardSearchFetch{Query: "parser", Rows: []map[string]any{
		{"id": "AIRA-1", "snippet": "dup"}, // already a client match — must not duplicate
		{"id": "AIRA-2", "snippet": "new [term]"},
	}})
	if len(merged.Search.Results) != 2 {
		t.Fatalf("merge/dedup = %d results, want 2", len(merged.Search.Results))
	}
	if boardSearchLabel(merged.Search) == "no matches" {
		t.Fatalf("non-empty merged search reported no matches")
	}

	empty := boardMergeGrep(boardState{Search: boardSearchState{Active: true, Query: "zzz", MatchIDs: map[string]bool{}}},
		boardSearchFetch{Query: "zzz"})
	if boardSearchLabel(empty.Search) != "no matches" {
		t.Fatalf("genuinely empty search label = %q, want 'no matches'", boardSearchLabel(empty.Search))
	}
}

func TestBoardActionDrillInEmitsDetailFetch(t *testing.T) {
	model := boardModel{Columns: []boardColumn{{Status: "planned", Cards: []boardCard{{ID: "AIRA-1", Title: "one"}}}}}
	state := boardTUIState(boardApplyModelPtr(model))
	next, commands := onBoardAction(state, boardActDrillIn)
	if next.Board.DrillID != "AIRA-1" {
		t.Fatalf("drill-in DrillID = %q, want AIRA-1", next.Board.DrillID)
	}
	found := false
	for _, command := range commands {
		if command.Kind == cmdFetch && command.View == viewBoard && command.DetailID == "AIRA-1" {
			found = true
		}
	}
	if !found {
		t.Fatalf("drill-in did not emit a board detail fetch: %#v", commands)
	}
}

func TestBoardActionBackClearsDrillThenSearch(t *testing.T) {
	bs := newBoardState()
	bs.DrillID = "AIRA-1"
	bs.Detail = "detail"
	bs.Search.Active = true
	state := boardTUIState(bs)
	afterDrill, _ := onBoardAction(state, boardActBack)
	if afterDrill.Board.DrillID != "" || afterDrill.Board.Detail != "" {
		t.Fatalf("Back did not close drill-in first: %#v", afterDrill.Board)
	}
	afterSearch, _ := onBoardAction(afterDrill, boardActBack)
	if afterSearch.Board.Search.Active {
		t.Fatalf("Back did not clear search after drill-in closed")
	}
}

func boardApplyModelPtr(model boardModel) *boardState {
	bs := boardApplyModel(*newBoardState(), model)
	return &bs
}
