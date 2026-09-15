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
	for _, q := range []string{"AIRA-247", "247", "FEE-BL-12", "aira-1", "STON-5"} {
		if !boardIDShaped(q) {
			t.Fatalf("%q should be id-shaped (resolved client-side / via show)", q)
		}
	}
	// P1.4: hyphenated CONTENT words must NOT be id-shaped, or they never reach
	// grep and the board fabricates "no matches" for them.
	for _, q := range []string{"parser", "fix the bug", "review", "in-progress", "no-TTY", "cgroup-kill", "AIRA", "blocked-by"} {
		if boardIDShaped(q) {
			t.Fatalf("%q should NOT be id-shaped (it is content and must reach grep)", q)
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

// TestBoardSearchIDNoMatchProbesGet is P2.5: an id-shaped query with no loaded
// match dispatches a `show` probe rather than reporting "no matches".
func TestBoardSearchIDNoMatchProbesGet(t *testing.T) {
	model := boardModel{Columns: []boardColumn{{Status: "planned", Cards: []boardCard{{ID: "AIRA-1", Title: "one"}}}}}
	state, commands := onBoardSearchSubmit(boardTUIState(boardApplyModelPtr(model)), "AIRA-999")
	found := false
	for _, command := range commands {
		if command.Kind == cmdBoardGet && command.Search == "AIRA-999" {
			found = true
		}
		if command.Kind == cmdBoardSearch {
			t.Fatalf("id-shaped query dispatched grep instead of a show probe")
		}
	}
	if !found {
		t.Fatalf("id-shaped no-match query did not probe `show`: %#v", commands)
	}
	if !state.Board.Search.Pending {
		t.Fatalf("get probe not marked pending")
	}
}

// TestBoardGetResultFoundAndNotFound is P2.5's honest resolution: found → an
// openable result + "ok"; genuine E_NOT_FOUND → "not found", never "no matches".
func TestBoardGetResultFoundAndNotFound(t *testing.T) {
	base := func() tuiState {
		bs := newBoardState()
		bs.Search = boardSearchState{Active: true, Query: "AIRA-999", Pending: true, MatchIDs: map[string]bool{}}
		return boardTUIState(bs)
	}
	found, _ := onBoardGetResult(base(), boardGetFetch{Query: "AIRA-999", Found: true, Title: "deep in the tail"})
	if len(found.Board.Search.Results) != 1 || found.Board.Search.Results[0].ID != "AIRA-999" {
		t.Fatalf("found probe did not add an openable result: %#v", found.Board.Search.Results)
	}
	if boardSearchLabel(found.Board.Search) == "no matches" || boardSearchLabel(found.Board.Search) == "not found" {
		t.Fatalf("found probe label = %q, want a match count", boardSearchLabel(found.Board.Search))
	}
	notFound, _ := onBoardGetResult(base(), boardGetFetch{Query: "AIRA-999", Found: false})
	if got := boardSearchLabel(notFound.Board.Search); got != "not found" {
		t.Fatalf("not-found probe label = %q, want 'not found' (never 'no matches')", got)
	}
}

// TestBoardSearchTruncatedDisclosed is P2.6: a grep that hit the 50-cap discloses
// the count is a floor, not the complete total.
func TestBoardSearchTruncatedDisclosed(t *testing.T) {
	base := boardState{Search: boardSearchState{Active: true, Query: "the", MatchIDs: map[string]bool{}}}
	merged := boardMergeGrep(base, boardSearchFetch{Query: "the", Truncated: true, Rows: []map[string]any{{"id": "AIRA-1", "snippet": "s"}}})
	if label := boardSearchLabel(merged.Search); !strings.Contains(label, "truncated") {
		t.Fatalf("truncated grep label = %q, want a truncation disclosure", label)
	}
}

// TestBoardResultOpenJumpsOrDrills is P2.7: opening a LOADED result jumps to its
// card (focus + selection, search cleared); opening an UNLOADED result drills in
// via a detail fetch.
func TestBoardResultOpenJumpsOrDrills(t *testing.T) {
	model := boardModel{Columns: []boardColumn{
		{Status: "planned", Cards: []boardCard{{ID: "AIRA-1", Title: "one"}}},
		{Status: "in-progress", Cards: []boardCard{{ID: "AIRA-2", Title: "two"}}},
	}}
	// Loaded result → jump.
	bs := boardApplyModelPtr(model)
	bs.Search = boardSearchState{Active: true, Query: "x", Results: []boardSearchResult{{ID: "AIRA-2"}}, MatchIDs: map[string]bool{"AIRA-2": true}}
	jumped, commands := onBoardResultOpen(boardTUIState(bs))
	if jumped.Board.FocusedCol != 1 || jumped.Board.Selected[1] != 0 {
		t.Fatalf("jump did not focus AIRA-2's column/row: col=%d sel=%v", jumped.Board.FocusedCol, jumped.Board.Selected)
	}
	if jumped.Board.Search.Active {
		t.Fatalf("jump did not close the search")
	}
	for _, command := range commands {
		if command.Kind == cmdFetch {
			t.Fatalf("jump to a loaded card must not dispatch a detail fetch")
		}
	}
	// Unloaded result (grep-only content hit) → drill-in.
	bs2 := boardApplyModelPtr(model)
	bs2.Search = boardSearchState{Active: true, Query: "x", Results: []boardSearchResult{{ID: "AIRA-777"}}, MatchIDs: map[string]bool{"AIRA-777": true}}
	drilled, commands2 := onBoardResultOpen(boardTUIState(bs2))
	if drilled.Board.DrillID != "AIRA-777" {
		t.Fatalf("unloaded result did not drill in: DrillID=%q", drilled.Board.DrillID)
	}
	found := false
	for _, command := range commands2 {
		if command.Kind == cmdFetch && command.DetailID == "AIRA-777" {
			found = true
		}
	}
	if !found {
		t.Fatalf("unloaded result did not dispatch a detail fetch: %#v", commands2)
	}
}

// TestBoardApplyModelRecomputesSearch is P2.9: a background refresh re-derives the
// client match set against the new cards while preserving grep-only hits.
func TestBoardApplyModelRecomputesSearch(t *testing.T) {
	bs := boardApplyModel(*newBoardState(), boardModel{Columns: []boardColumn{
		{Status: "planned", Cards: []boardCard{{ID: "AIRA-1", Title: "parser"}}},
	}})
	bs.Search = boardSearchState{Active: true, Query: "parser", MatchIDs: map[string]bool{"AIRA-1": true, "AIRA-9": true},
		ContentIDs: map[string]bool{"AIRA-9": true}, // AIRA-9 is a grep-only content hit
		Results:    []boardSearchResult{{ID: "AIRA-1", Snippet: "parser"}, {ID: "AIRA-9", Snippet: "grep-only"}}}
	// Refresh: AIRA-1 gone, AIRA-2 "parser" arrives; the grep-only AIRA-9 persists.
	refreshed := boardApplyModel(bs, boardModel{Columns: []boardColumn{
		{Status: "planned", Cards: []boardCard{{ID: "AIRA-2", Title: "parser rewrite"}}},
	}})
	if refreshed.Search.MatchIDs["AIRA-1"] {
		t.Fatalf("stale client match AIRA-1 survived a refresh")
	}
	if !refreshed.Search.MatchIDs["AIRA-2"] {
		t.Fatalf("new client match AIRA-2 not recomputed on refresh")
	}
	if !refreshed.Search.MatchIDs["AIRA-9"] {
		t.Fatalf("grep-only hit AIRA-9 was dropped on refresh")
	}
}
