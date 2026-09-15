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

// boardCmdsHaveDetailDebounce reports whether a detail re-arm was emitted.
func boardCmdsHaveDetailDebounce(cmds []tuiCmd) bool {
	for _, c := range cmds {
		if c.Kind == cmdBoardDetailDebounce {
			return true
		}
	}
	return false
}

// TestBoardActionCardFirstLast pins AIRA-256: Home/End jump the SELECTION to the
// first/last card of the focused column (updating the authoritative bs.Selected,
// not just the tview cursor) and re-arm the info-pane detail like any card move.
func TestBoardActionCardFirstLast(t *testing.T) {
	cards := []boardCard{{ID: "AIRA-1"}, {ID: "AIRA-2"}, {ID: "AIRA-3"}, {ID: "AIRA-4"}}
	model := boardModel{Columns: []boardColumn{{Status: "planned", Cards: cards}}}

	// End (from a fresh state, no timer pending): jump to the last card, retarget
	// the detail to it, and arm the debounce.
	endStart := boardApplyModelPtr(model)
	endStart.Selected[0] = 2 // on AIRA-3
	end, cmds := onBoardAction(boardTUIState(endStart), boardActCardLast)
	if end.Board.Selected[0] != 3 {
		t.Fatalf("End -> selection %d, want 3 (last card)", end.Board.Selected[0])
	}
	if end.Board.Detail.ID != "AIRA-4" {
		t.Fatalf("End did not retarget the detail to the last card: %q", end.Board.Detail.ID)
	}
	if !boardCmdsHaveDetailDebounce(cmds) {
		t.Fatalf("End did not re-arm the info-pane detail: %#v", cmds)
	}

	// Home (from a fresh state): jump to the first card, retarget, arm. (Chaining
	// Home after End would NOT re-emit — one timer already coalesces the moves.)
	homeStart := boardApplyModelPtr(model)
	homeStart.Selected[0] = 2 // on AIRA-3
	home, cmds := onBoardAction(boardTUIState(homeStart), boardActCardFirst)
	if home.Board.Selected[0] != 0 {
		t.Fatalf("Home -> selection %d, want 0 (first card)", home.Board.Selected[0])
	}
	if home.Board.Detail.ID != "AIRA-1" {
		t.Fatalf("Home did not retarget the detail to the first card: %q", home.Board.Detail.ID)
	}
	if !boardCmdsHaveDetailDebounce(cmds) {
		t.Fatalf("Home did not re-arm the info-pane detail: %#v", cmds)
	}
}

// TestBoardActionCardPage pins PgUp/PgDn: move the selection by boardCardPageStep,
// clamped to the column ends.
func TestBoardActionCardPage(t *testing.T) {
	cards := make([]boardCard, 25)
	for i := range cards {
		cards[i] = boardCard{ID: "AIRA-" + strconv.Itoa(i)}
	}
	model := boardModel{Columns: []boardColumn{{Status: "planned", Cards: cards}}}

	mid := boardApplyModelPtr(model)
	mid.Selected[0] = 5
	down, _ := onBoardAction(boardTUIState(mid), boardActCardPageDown)
	// Literal (5 + a 10-card page), NOT 5+boardCardPageStep: a self-referential
	// expectation would shift in lockstep with the constant and silently miss a
	// change to the page size. This also pins boardCardPageStep == 10.
	if down.Board.Selected[0] != 15 {
		t.Fatalf("PgDn -> %d, want 15 (5 + a 10-card page)", down.Board.Selected[0])
	}
	up, _ := onBoardAction(down, boardActCardPageUp)
	if up.Board.Selected[0] != 5 {
		t.Fatalf("PgUp -> %d, want 5", up.Board.Selected[0])
	}

	nearTop := boardApplyModelPtr(model)
	nearTop.Selected[0] = 1
	top, _ := onBoardAction(boardTUIState(nearTop), boardActCardPageUp)
	if got := top.Board.Selected[0]; got != 0 {
		t.Fatalf("PgUp near the top -> %d, want 0 (clamped)", got)
	}
	nearBottom := boardApplyModelPtr(model)
	nearBottom.Selected[0] = 23
	bottom, _ := onBoardAction(boardTUIState(nearBottom), boardActCardPageDown)
	if got := bottom.Board.Selected[0]; got != 24 {
		t.Fatalf("PgDn near the bottom -> %d, want 24 (clamped last)", got)
	}
}

// TestBoardActionCardEdgeEmptyColumnSafe pins that all four jump/page actions are
// safe on an empty focused column (no panic, selection stays 0).
func TestBoardActionCardEdgeEmptyColumnSafe(t *testing.T) {
	model := boardModel{Columns: []boardColumn{{Status: "planned"}}} // no cards
	for _, act := range []boardAction{boardActCardFirst, boardActCardLast, boardActCardPageUp, boardActCardPageDown} {
		next, _ := onBoardAction(boardTUIState(boardApplyModelPtr(model)), act)
		if next.Board.Selected[0] != 0 {
			t.Fatalf("action %d on an empty column -> %d, want 0", act, next.Board.Selected[0])
		}
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

func TestBoardActionExpandEmitsDetailFetch(t *testing.T) {
	model := boardModel{Columns: []boardColumn{{Status: "planned", Cards: []boardCard{{ID: "AIRA-1", Title: "one"}}}}}
	state := boardTUIState(boardApplyModelPtr(model))
	next, commands := onBoardAction(state, boardActExpand)
	if !next.Board.Expanded {
		t.Fatalf("expand did not open the overlay")
	}
	if next.Board.Detail.ID != "AIRA-1" {
		t.Fatalf("expand Detail.ID = %q, want AIRA-1", next.Board.Detail.ID)
	}
	found := false
	for _, command := range commands {
		if command.Kind == cmdFetch && command.View == viewBoard && command.DetailID == "AIRA-1" {
			found = true
		}
	}
	if !found {
		t.Fatalf("expand did not emit a board detail fetch: %#v", commands)
	}
}

func TestBoardActionBackClosesExpandThenSearch(t *testing.T) {
	bs := newBoardState()
	bs.Expanded = true
	bs.Detail = boardDetailState{ID: "AIRA-1", State: "ready"}
	bs.Search.Active = true
	state := boardTUIState(bs)
	afterExpand, _ := onBoardAction(state, boardActBack)
	if afterExpand.Board.Expanded {
		t.Fatalf("Back did not close the expand overlay first: %#v", afterExpand.Board)
	}
	afterSearch, _ := onBoardAction(afterExpand, boardActBack)
	if afterSearch.Board.Search.Active {
		t.Fatalf("Back did not clear search after the overlay closed")
	}
}

func boardApplyModelPtr(model boardModel) *boardState {
	bs := boardApplyModel(*newBoardState(), model)
	return &bs
}

// TestBoardActionBackReArmsPaneAfterUnloadedOverlay pins the AIRA-254 honesty
// fix: opening an UNLOADED search hit in the overlay, letting its detail land,
// then closing the overlay with Esc must re-target the pane to the SELECTED card
// (loading) and request a fetch — never keep the overlay ticket's detail, which
// the pane would otherwise show under the selected card's title.
func TestBoardActionBackReArmsPaneAfterUnloadedOverlay(t *testing.T) {
	model := boardModel{Columns: []boardColumn{{Status: "planned", Cards: []boardCard{{ID: "AIRA-1", Title: "one"}}}}}
	bs := boardApplyModelPtr(model)
	// AIRA-777 is a grep-only hit not present in any loaded column.
	bs.Search = boardSearchState{Active: true, Query: "x", Results: []boardSearchResult{{ID: "AIRA-777"}}, MatchIDs: map[string]bool{"AIRA-777": true}}
	state := boardTUIState(bs)

	opened, _ := onBoardResultOpen(state)
	if !opened.Board.Expanded || opened.Board.Detail.ID != "AIRA-777" {
		t.Fatalf("opening the unloaded hit did not target the overlay: %#v", opened.Board.Detail)
	}
	landed, _ := onTUIDetailResult(opened, detailResult{View: viewBoard, ID: "AIRA-777", Board: boardDetailModel{Assignee: "owner-of-777", Body: "body of 777"}})
	if landed.Board.Detail.State != "ready" {
		t.Fatalf("overlay detail did not land ready: %#v", landed.Board.Detail)
	}

	back, cmds := onBoardAction(landed, boardActBack)
	selected := boardSelectedCardID(*back.Board)
	if selected != "AIRA-1" {
		t.Fatalf("selection after Esc = %q, want AIRA-1", selected)
	}
	if back.Board.Detail.ID != selected {
		t.Fatalf("pane still holds detail for %q after closing overlay on selected %q", back.Board.Detail.ID, selected)
	}
	if back.Board.Detail.State == "ready" {
		t.Fatalf("re-armed pane must be loading, not ready: %#v", back.Board.Detail)
	}
	armed := false
	for _, c := range cmds {
		if c.Kind == cmdBoardDetailDebounce {
			armed = true
		}
	}
	if !armed {
		t.Fatalf("closing the overlay did not re-arm a pane fetch: %#v", cmds)
	}
	// Belt: even before the re-armed fetch lands, the pane render for the selected
	// card must not leak the overlay ticket's fields.
	card, _ := boardSelectedCard(*back.Board)
	meta := strings.Join(boardDetailMetaLines(card, boardInfoDetail(card, back.Board.Detail)), "\n")
	if strings.Contains(meta, "owner-of-777") {
		t.Fatalf("pane leaked the overlay ticket's assignee:\n%s", meta)
	}
}

// TestBoardExpandedOverlayNotClobberedByRefresh pins that while the expand
// overlay is open on an UNLOADED hit (Detail.ID != selection), a background data
// refresh must NOT retarget the pane's Detail — the overlay owns it. Without the
// Expanded guard the reconcile would rewrite the open overlay to the cursor card.
func TestBoardExpandedOverlayNotClobberedByRefresh(t *testing.T) {
	data := boardDataFrom(map[string]boardColumnFetch{
		"planned": {Rows: []map[string]any{boardRow("AIRA-1", "planned", "P1", "one", false)}},
	}, listEnvelope{})
	bs := boardApplyModelPtr(buildBoardModel(data)) // selection lands on AIRA-1
	bs.Expanded = true
	bs.Detail = boardDetailState{ID: "AIRA-777", State: "ready", Model: boardDetailModel{Assignee: "owner-of-777", Body: "body 777"}}
	state := boardTUIState(bs)
	panel := state.Panels[viewBoard]
	panel.InFlight = true
	panel.InFlightGeneration = 7
	state.Panels[viewBoard] = panel

	next, cmds := onTUIFetchResult(state, fetchResult{View: viewBoard, Generation: 7, Board: &data})
	if next.Board.Detail.ID != "AIRA-777" || next.Board.Detail.State != "ready" || next.Board.Detail.Model.Assignee != "owner-of-777" {
		t.Fatalf("refresh clobbered the open overlay's detail: %#v", next.Board.Detail)
	}
	for _, c := range cmds {
		if c.Kind == cmdBoardDetailDebounce {
			t.Fatalf("refresh armed a pane fetch while the overlay was open: %#v", cmds)
		}
	}
}

// TestOnBoardDetailDueSkipsWhileExpanded pins the hazard the reviewer flagged: a
// due timer firing while the overlay is open must NOT retarget/refetch (the
// overlay owns Detail), but MUST clear Armed — otherwise Armed stays true with no
// timer in flight and the pane sticks on "loading…" after the overlay closes.
func TestOnBoardDetailDueSkipsWhileExpanded(t *testing.T) {
	model := boardModel{Columns: []boardColumn{{Status: "planned", Cards: []boardCard{{ID: "AIRA-1", Title: "one"}}}}}
	bs := boardApplyModelPtr(model)
	bs.Expanded = true
	bs.Detail = boardDetailState{ID: "AIRA-777", State: "ready", Armed: true, Model: boardDetailModel{Assignee: "owner-of-777"}}
	next, cmds := onBoardDetailDue(boardTUIState(bs))
	if next.Board.Detail.ID != "AIRA-777" || next.Board.Detail.Model.Assignee != "owner-of-777" {
		t.Fatalf("due clobbered the open overlay's detail: %#v", next.Board.Detail)
	}
	if next.Board.Detail.Armed {
		t.Fatalf("due must clear Armed even when skipping (else pane sticks on loading after Back)")
	}
	for _, c := range cmds {
		if c.Kind == cmdFetch {
			t.Fatalf("due refetched while the overlay was open: %#v", cmds)
		}
	}
}

// TestBoardActionExpandDispatchesWhenDetailNotReady pins the re-review P1 fix:
// pressing Enter INSIDE the 250ms debounce window — when a nav has armed a timer
// for the selected card but no fetch has been dispatched yet (Detail.ID == id,
// State "loading", Armed) — must dispatch the overlay's fetch IMMEDIATELY. It
// must NOT rely on the pending timer, which onBoardDetailDue drops while Expanded,
// else the overlay sticks on "loading…" forever.
func TestBoardActionExpandDispatchesWhenDetailNotReady(t *testing.T) {
	model := boardModel{Columns: []boardColumn{{Status: "planned", Cards: []boardCard{{ID: "AIRA-1", Title: "one"}}}}}
	bs := boardApplyModelPtr(model)
	// State as just after a nav: the timer is armed for the selection but the fetch
	// has not been dispatched (the debounce has not fired).
	bs.Detail = boardDetailState{ID: "AIRA-1", State: "loading", Armed: true}
	next, cmds := onBoardAction(boardTUIState(bs), boardActExpand)
	if !next.Board.Expanded {
		t.Fatalf("expand did not open the overlay")
	}
	found := false
	for _, c := range cmds {
		if c.Kind == cmdFetch && c.View == viewBoard && c.DetailID == "AIRA-1" {
			found = true
		}
	}
	if !found {
		t.Fatalf("expand on a not-ready detail did not dispatch a fetch — the overlay would strand on loading: %#v", cmds)
	}
}

// TestBoardActionExpandReusesReadyDetail pins the other side: when the detail is
// already ready for the selected id, expand reuses it and dispatches no fetch.
func TestBoardActionExpandReusesReadyDetail(t *testing.T) {
	model := boardModel{Columns: []boardColumn{{Status: "planned", Cards: []boardCard{{ID: "AIRA-1", Title: "one"}}}}}
	bs := boardApplyModelPtr(model)
	bs.Detail = boardDetailState{ID: "AIRA-1", State: "ready", Model: boardDetailModel{Assignee: "mark"}}
	next, cmds := onBoardAction(boardTUIState(bs), boardActExpand)
	for _, c := range cmds {
		if c.Kind == cmdFetch {
			t.Fatalf("expand refetched an already-ready detail: %#v", cmds)
		}
	}
	if next.Board.Detail.Model.Assignee != "mark" || next.Board.Detail.State != "ready" {
		t.Fatalf("expand disturbed the ready detail: %#v", next.Board.Detail)
	}
}

// TestBoardActionRefreshResetsPaneDetail pins that an explicit refresh does not
// leave a stale pane body: it drops the held detail (so the onData reconcile
// re-arms a fresh fetch) while still starting the panel refresh.
func TestBoardActionRefreshResetsPaneDetail(t *testing.T) {
	model := boardModel{Columns: []boardColumn{{Status: "planned", Cards: []boardCard{{ID: "AIRA-1", Title: "one"}}}}}
	bs := boardApplyModelPtr(model)
	bs.Detail = boardDetailState{ID: "AIRA-1", State: "ready", Model: boardDetailModel{Body: "stale body"}}
	next, cmds := onBoardAction(boardTUIState(bs), boardActRefresh)
	if next.Board.Detail.State == "ready" || next.Board.Detail.Model.Body == "stale body" {
		t.Fatalf("refresh kept a stale ready detail: %#v", next.Board.Detail)
	}
	// The panel refresh must still be requested.
	refresh := false
	for _, c := range cmds {
		if c.Kind == cmdFetch && c.View == viewBoard && c.DetailID == "" {
			refresh = true
		}
	}
	if !refresh {
		t.Fatalf("refresh did not start the panel fetch: %#v", cmds)
	}
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
	sawDebounce := false
	for _, command := range commands {
		if command.Kind == cmdFetch {
			t.Fatalf("jump to a loaded card must not dispatch an immediate detail fetch")
		}
		if command.Kind == cmdBoardDetailDebounce {
			sawDebounce = true
		}
	}
	if !sawDebounce {
		t.Fatalf("jump did not arm the info pane for the jumped-to card: %#v", commands)
	}
	// Unloaded result (grep-only content hit) → open the self-contained overlay.
	bs2 := boardApplyModelPtr(model)
	bs2.Search = boardSearchState{Active: true, Query: "x", Results: []boardSearchResult{{ID: "AIRA-777"}}, MatchIDs: map[string]bool{"AIRA-777": true}}
	opened, commands2 := onBoardResultOpen(boardTUIState(bs2))
	if !opened.Board.Expanded || opened.Board.Detail.ID != "AIRA-777" {
		t.Fatalf("unloaded result did not open the overlay: Expanded=%v Detail.ID=%q", opened.Board.Expanded, opened.Board.Detail.ID)
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
