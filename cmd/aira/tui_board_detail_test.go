package main

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"aira/internal/core"
	"aira/internal/daemon"
	"aira/internal/domain"
)

// TestBoardStripWindowFullWidth: the AIRA-254 full-width toggle collapses the
// strip to the focused column alone, and otherwise delegates to the normal fit.
func TestBoardStripWindowFullWidth(t *testing.T) {
	// Full width: exactly the focused column, at any width.
	for _, focus := range []int{0, 3, 6} {
		start, end := boardStripWindow(400, focus, 7, true)
		if start != focus || end != focus+1 {
			t.Fatalf("fullWidth focus=%d -> [%d,%d), want [%d,%d)", focus, start, end, focus, focus+1)
		}
	}
	// Full width clamps an out-of-range focus into a single valid column.
	if start, end := boardStripWindow(400, 99, 7, true); start != 6 || end != 7 {
		t.Fatalf("fullWidth clamp -> [%d,%d), want [6,7)", start, end)
	}
	// Not full width: identical to boardVisibleColumns.
	for _, tc := range []struct{ w, f, n int }{{40, 3, 7}, {400, 3, 7}, {15, 4, 7}} {
		gotS, gotE := boardStripWindow(tc.w, tc.f, tc.n, false)
		wantS, wantE := boardVisibleColumns(tc.w, tc.f, tc.n)
		if gotS != wantS || gotE != wantE {
			t.Fatalf("non-fullWidth(%d,%d,%d) = [%d,%d), want boardVisibleColumns [%d,%d)", tc.w, tc.f, tc.n, gotS, gotE, wantS, wantE)
		}
	}
	// Zero columns: empty in both modes.
	if s, e := boardStripWindow(80, 0, 0, true); s != 0 || e != 0 {
		t.Fatalf("fullWidth no-columns -> [%d,%d), want [0,0)", s, e)
	}
}

// TestBoardFormatRelations renders edges from THIS ticket's perspective. The
// store always sets From = the queried subject and To = the other end, with Kind
// pre-inverted for incoming edges, so the other end is always To — including the
// incoming blocked-by edge below, whose From is still the subject (AIRA-1), not
// the far party. Using From:AIRA-9 there would be a shape the store never emits.
func TestBoardFormatRelations(t *testing.T) {
	views := []domain.RelationView{
		{Kind: domain.RelationBlocks, From: "AIRA-1", To: "AIRA-2"},    // outgoing
		{Kind: domain.RelationBlockedBy, From: "AIRA-1", To: "AIRA-9"}, // incoming (pre-inverted): other end is To
		{Kind: "", From: "AIRA-1", To: "AIRA-3"},                       // empty kind -> relates
	}
	got := boardFormatRelations(views)
	want := []string{"blocks AIRA-2", "blocked-by AIRA-9", "relates AIRA-3"}
	if len(got) != len(want) {
		t.Fatalf("relations = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("relation[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}

// TestBoardDetailFieldHonesty: an unread section wins (→ unevaluated), an empty
// value shows the placeholder, and a real value shows itself. This is the single
// honesty seam, so it is tested directly.
func TestBoardDetailFieldHonesty(t *testing.T) {
	if got := boardDetailField("", "E_X", "—"); got != "unevaluated (E_X)" {
		t.Fatalf("code should win: got %q", got)
	}
	if got := boardDetailField("value", "E_X", "—"); got != "unevaluated (E_X)" {
		t.Fatalf("code must win even over a value: got %q", got)
	}
	if got := boardDetailField("", "", "—"); got != "—" {
		t.Fatalf("empty value should show placeholder: got %q", got)
	}
	if got := boardDetailField("mark", "", "—"); got != "mark" {
		t.Fatalf("value should show itself: got %q", got)
	}
}

// TestBoardDetailMetaLinesLoading: while the fetch is in flight the pane shows
// the instant badges plus a single "loading…" — never a fabricated field.
func TestBoardDetailMetaLinesLoading(t *testing.T) {
	// Use the REAL badge constant (not a "?" stand-in) so the test can actually
	// catch the "ready ready" word-doubling regression.
	card := boardCard{ID: "AIRA-1", Hold: true, ReadyBadge: boardReadyUnevalBadge, BlockedBadge: "⛔?"}
	lines := boardDetailMetaLines(card, boardDetailState{ID: "AIRA-1", State: "loading"})
	joined := strings.Join(lines, "\n")
	if !strings.Contains(joined, "hold") || !strings.Contains(joined, boardReadyUnevalBadge) || !strings.Contains(joined, "⛔? blocked") {
		t.Fatalf("loading meta missing instant badges:\n%s", joined)
	}
	if strings.Contains(joined, "ready ready") {
		t.Fatalf("ready badge word is doubled:\n%s", joined)
	}
	if !strings.Contains(joined, "loading…") {
		t.Fatalf("loading meta missing loading marker:\n%s", joined)
	}
	if strings.Contains(joined, "assignee:") || strings.Contains(joined, "findings:") {
		t.Fatalf("loading meta fabricated fetched fields:\n%s", joined)
	}
}

// TestBoardDetailMetaLinesHonesty: once ready, each section renders its value or
// "unevaluated (CODE)" — independently, so one failed section never blanks or
// fabricates another.
func TestBoardDetailMetaLinesHonesty(t *testing.T) {
	// Everything read: real values, no unevaluated, no leftover loading.
	ok := boardDetailState{ID: "AIRA-1", State: "ready", Model: boardDetailModel{
		Assignee: "mark", Labels: "ui", Milestone: "v0.11", Relations: []string{"blocks AIRA-2"}, Findings: "3",
	}}
	joined := strings.Join(boardDetailMetaLines(boardCard{ID: "AIRA-1"}, ok), "\n")
	for _, want := range []string{"assignee: mark", "labels: ui", "milestone: v0.11", "blocks AIRA-2", "findings: 3"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("ready meta missing %q:\n%s", want, joined)
		}
	}
	if strings.Contains(joined, "loading") || strings.Contains(joined, "unevaluated") {
		t.Fatalf("ready meta should be clean:\n%s", joined)
	}

	// show failed -> assignee/labels/milestone unevaluated, but relations/findings
	// from their own (clean) sections still render.
	partial := boardDetailState{ID: "AIRA-1", State: "ready", Model: boardDetailModel{
		Assignee: "mark", ShowCode: "E_TUI_CANCELLED",
		Relations: []string{"blocks AIRA-2"}, Findings: "3",
	}}
	joined = strings.Join(boardDetailMetaLines(boardCard{ID: "AIRA-1"}, partial), "\n")
	if !strings.Contains(joined, "assignee: unevaluated (E_TUI_CANCELLED)") {
		t.Fatalf("show failure must mark assignee unevaluated:\n%s", joined)
	}
	if strings.Contains(joined, "assignee: mark") {
		t.Fatalf("show failure must NOT show the stale assignee value:\n%s", joined)
	}
	if !strings.Contains(joined, "blocks AIRA-2") || !strings.Contains(joined, "findings: 3") {
		t.Fatalf("an independent section must still render:\n%s", joined)
	}

	// link failed -> relations unevaluated (not "—").
	linkBad := boardDetailState{ID: "AIRA-1", State: "ready", Model: boardDetailModel{LinkCode: "E_INDEX"}}
	joined = strings.Join(boardDetailMetaLines(boardCard{ID: "AIRA-1"}, linkBad), "\n")
	if !strings.Contains(joined, "relations: unevaluated (E_INDEX)") {
		t.Fatalf("link failure must mark relations unevaluated:\n%s", joined)
	}
	if strings.Contains(joined, "relations: —") {
		t.Fatalf("link failure must not read as 'no relations':\n%s", joined)
	}

	// find failed -> findings unevaluated (not the fabricated "0").
	findBad := boardDetailState{ID: "AIRA-1", State: "ready", Model: boardDetailModel{FindCode: "E_INDEX"}}
	joined = strings.Join(boardDetailMetaLines(boardCard{ID: "AIRA-1"}, findBad), "\n")
	if !strings.Contains(joined, "findings: unevaluated (E_INDEX)") || strings.Contains(joined, "findings: 0") {
		t.Fatalf("find failure must not fabricate a zero count:\n%s", joined)
	}
}

// TestBoardDetailBodyText covers the body lifecycle: loading, unevaluated, the
// explicit empty state, and a real body.
func TestBoardDetailBodyText(t *testing.T) {
	if got := boardDetailBodyText(boardDetailState{State: "loading"}); got != "loading…" {
		t.Fatalf("loading body = %q", got)
	}
	if got := boardDetailBodyText(boardDetailState{State: "ready", Model: boardDetailModel{ShowCode: "E_X"}}); got != "unevaluated (E_X)" {
		t.Fatalf("failed body = %q", got)
	}
	if got := boardDetailBodyText(boardDetailState{State: "ready", Model: boardDetailModel{Body: "  \n"}}); got != "(no description)" {
		t.Fatalf("empty body = %q", got)
	}
	if got := boardDetailBodyText(boardDetailState{State: "ready", Model: boardDetailModel{Body: "hello"}}); got != "hello" {
		t.Fatalf("body = %q", got)
	}
}

// TestBoardDetailOverlayText: the overlay is self-contained from the model (so an
// unloaded id still renders), and stays honest when the ticket is unevaluated.
func TestBoardDetailOverlayText(t *testing.T) {
	ready := boardDetailState{ID: "AIRA-9", State: "ready", Model: boardDetailModel{
		Title: "the title", Status: "planned", Severity: "P1", Kind: "bug", Body: "body text",
		Relations: []string{"blocks AIRA-2"}, Findings: "0",
	}}
	got := boardDetailOverlayText("AIRA-9", ready, boardHumanizeState{})
	for _, want := range []string{"AIRA-9 · planned · P1 · bug", "the title", "blocks AIRA-2", "body text"} {
		if !strings.Contains(got, want) {
			t.Fatalf("overlay missing %q:\n%s", want, got)
		}
	}
	// Loading overlay.
	if got := boardDetailOverlayText("AIRA-9", boardDetailState{State: "loading"}, boardHumanizeState{}); got != "loading…" {
		t.Fatalf("loading overlay = %q", got)
	}
	// Ticket unevaluated: the header falls back to the bare id and says so.
	bad := boardDetailState{ID: "AIRA-9", State: "ready", Model: boardDetailModel{ShowCode: "E_X"}}
	got = boardDetailOverlayText("AIRA-9", bad, boardHumanizeState{})
	if !strings.Contains(got, "AIRA-9") || !strings.Contains(got, "ticket unevaluated: E_X") {
		t.Fatalf("unevaluated overlay missing honest marker:\n%s", got)
	}
}

// TestBoardDetailOverlayTextHumanizeIsIDGated pins the AIRA-257 honesty seam at the
// overlay's call site: this gate is LOAD-BEARING because the overlay can show an
// unloaded search hit whose id differs from the ticket a held rewrite is for. A
// rewrite keyed to a DIFFERENT ticket must never render under this overlay; only a
// same-id rewrite is shown, labelled as an AI paraphrase. (A mutation passing
// Humanize.ID instead of the overlay id — so the rewrite renders under every
// ticket — reds this test.)
func TestBoardDetailOverlayTextHumanizeIsIDGated(t *testing.T) {
	ready := boardDetailState{ID: "AIRA-9", State: "ready", Model: boardDetailModel{
		Title: "the original title", Status: "planned", Body: "the original body",
	}}
	// A rewrite held for a DIFFERENT ticket must not leak into AIRA-9's overlay.
	foreign := boardHumanizeState{ID: "AIRA-1", State: "ready", PlainTitle: "REWRITTEN", PlainBody: "REWRITTEN BODY", Shown: true}
	got := boardDetailOverlayText("AIRA-9", ready, foreign)
	if strings.Contains(got, "REWRITTEN") || strings.Contains(got, "plain-English (AI)") {
		t.Fatalf("a rewrite for AIRA-1 leaked into AIRA-9's overlay:\n%s", got)
	}
	if !strings.Contains(got, "the original body") {
		t.Fatalf("overlay for a foreign rewrite should show the original:\n%s", got)
	}
	// A same-id rewrite IS shown, labelled.
	own := boardHumanizeState{ID: "AIRA-9", State: "ready", PlainTitle: "REWRITTEN TITLE", PlainBody: "REWRITTEN BODY", Shown: true}
	got = boardDetailOverlayText("AIRA-9", ready, own)
	if !strings.Contains(got, "REWRITTEN BODY") || !strings.Contains(got, "plain-English (AI)") {
		t.Fatalf("a same-id rewrite should render, labelled:\n%s", got)
	}
}

// TestBoardInfoPaneHeight pins the pane-height policy: a quarter of the screen,
// floored at 7, capped at 14, capped again so it never eats the columns
// (screenHeight-reserved), and 0 (hidden) when what remains is too small.
func TestBoardInfoPaneHeight(t *testing.T) {
	cases := []struct{ screen, want int }{
		{0, 0},    // construction default: nothing known yet -> hidden
		{100, 14}, // 25 -> capped at 14
		{48, 12},  // 48/4 = 12
		{24, 7},   // 6 -> floored at 7; 24-11=13 leaves room
		{16, 5},   // floor 7 but 16-11=5 caps it to 5 (still >= min)
		{15, 0},   // 15-11=4 < min 5 -> hidden so the strip stays usable
		{11, 0},   // reserved rows alone -> hidden
	}
	for _, c := range cases {
		if got := boardInfoPaneHeight(c.screen); got != c.want {
			t.Fatalf("boardInfoPaneHeight(%d) = %d, want %d", c.screen, got, c.want)
		}
	}
}

// TestBoardInfoOverflow pins the clip-disclosure count: wrapped display-rows
// beyond the available rows, blank separators included, 0 when it fits or on a
// degenerate geometry.
func TestBoardInfoOverflow(t *testing.T) {
	if got := boardInfoOverflow("a\nb", 10, 5); got != 0 {
		t.Fatalf("two short lines in 5 rows -> %d, want 0", got)
	}
	if got := boardInfoOverflow("aa\nbb\ncc\ndd\nee\nff", 10, 3); got != 3 {
		t.Fatalf("6 lines in 3 rows -> %d, want 3", got)
	}
	if got := boardInfoOverflow("a\n\nb", 10, 2); got != 1 {
		t.Fatalf("blank separator must occupy a row: %d, want 1", got)
	}
	if got := boardInfoOverflow("aaaaa aaaaa aaaaa", 5, 1); got < 2 {
		t.Fatalf("a line that word-wraps 3x in 1 row should overflow >=2: got %d", got)
	}
	if got := boardInfoOverflow("x", 0, 5); got != 0 {
		t.Fatalf("zero width -> %d, want 0", got)
	}
	if got := boardInfoOverflow("x", 5, 0); got != 0 {
		t.Fatalf("zero rows -> %d, want 0", got)
	}
}

// TestBoardInfoColumnWidths pins the wide-split estimate: body gets the larger
// share (the owner's ask: >=50% for the body), and degenerate widths never go <1.
func TestBoardInfoColumnWidths(t *testing.T) {
	if l, b := boardInfoLeftWidth(102), boardInfoBodyWidth(102); l != 40 || b != 60 {
		t.Fatalf("wide split at 102 = left %d / body %d, want 40 / 60", l, b)
	}
	if b := boardInfoBodyWidth(102); b <= boardInfoLeftWidth(102) {
		t.Fatalf("body column must be the larger share")
	}
	if l, b := boardInfoLeftWidth(2), boardInfoBodyWidth(2); l < 1 || b < 1 {
		t.Fatalf("degenerate width must not go below 1: left %d body %d", l, b)
	}
}

// boardDetailDispatcher is a fake for fetchBoardDetail: it answers show/link/find
// with fixtures, and can be told to fail a chosen verb to prove per-section
// honesty.
type boardDetailDispatcher struct {
	failVerb string
}

func (d boardDetailDispatcher) Dispatch(_ context.Context, _ daemon.WorktreeScope, request core.Request) core.Response {
	if request.Verb == d.failVerb {
		return core.Response{Code: "E_TUI_UNKNOWN", Error: "forced failure"}
	}
	switch request.Verb {
	case "show":
		return core.Response{OK: true, Code: "OK", RawData: json.RawMessage(
			`{"id":"AIRA-1","title":"the title","status":"planned","severity":"P1","kind":"bug","assignee":"mark","labels":["ui","tui"],"milestone":"v0.11","body":"line one\nline two\n\n"}`)}
	case "link":
		return core.Response{OK: true, Code: "OK", RawData: json.RawMessage(
			`[{"kind":"blocks","from":"AIRA-1","to":"AIRA-2"}]`)}
	case "find":
		return core.Response{OK: true, Code: "OK", RawData: json.RawMessage(`{"total":2,"rows":[]}`)}
	}
	return core.Response{OK: true, Code: "OK", RawData: json.RawMessage(`{}`)}
}

// TestFetchBoardDetailDecodes: a clean fetch decodes every section readably.
func TestFetchBoardDetailDecodes(t *testing.T) {
	model := fetchBoardDetail(context.Background(), boardDetailDispatcher{}, daemon.WorktreeScope{}, "AIRA-1")
	if model.Title != "the title" || model.Status != "planned" || model.Severity != "P1" || model.Kind != "bug" {
		t.Fatalf("core fields wrong: %#v", model)
	}
	if model.Assignee != "mark" || model.Labels != "ui, tui" || model.Milestone != "v0.11" {
		t.Fatalf("meta fields wrong: %#v", model)
	}
	if model.Body != "line one\nline two" { // trailing blanks trimmed
		t.Fatalf("body = %q", model.Body)
	}
	if len(model.Relations) != 1 || model.Relations[0] != "blocks AIRA-2" {
		t.Fatalf("relations = %v", model.Relations)
	}
	if model.Findings != "2" {
		t.Fatalf("findings = %q", model.Findings)
	}
	if model.ShowCode != "" || model.LinkCode != "" || model.FindCode != "" {
		t.Fatalf("clean fetch set an error code: %#v", model)
	}
}

// TestFetchBoardDetailPerSectionHonesty: a failure in one section sets ONLY that
// section's code and never aborts the others.
func TestFetchBoardDetailPerSectionHonesty(t *testing.T) {
	showFail := fetchBoardDetail(context.Background(), boardDetailDispatcher{failVerb: "show"}, daemon.WorktreeScope{}, "AIRA-1")
	if showFail.ShowCode == "" {
		t.Fatalf("show failure did not set ShowCode")
	}
	if showFail.Title != "" || showFail.Assignee != "" || showFail.Body != "" {
		t.Fatalf("show failure left fabricated core fields: %#v", showFail)
	}
	if showFail.LinkCode != "" || len(showFail.Relations) != 1 || showFail.FindCode != "" || showFail.Findings != "2" {
		t.Fatalf("show failure aborted the independent sections: %#v", showFail)
	}

	linkFail := fetchBoardDetail(context.Background(), boardDetailDispatcher{failVerb: "link"}, daemon.WorktreeScope{}, "AIRA-1")
	if linkFail.LinkCode == "" || len(linkFail.Relations) != 0 {
		t.Fatalf("link failure honesty wrong: %#v", linkFail)
	}
	if linkFail.ShowCode != "" || linkFail.Title == "" {
		t.Fatalf("link failure must not disturb show: %#v", linkFail)
	}

	findFail := fetchBoardDetail(context.Background(), boardDetailDispatcher{failVerb: "find"}, daemon.WorktreeScope{}, "AIRA-1")
	if findFail.FindCode == "" || findFail.Findings != "" {
		t.Fatalf("find failure must not fabricate a count: %#v", findFail)
	}
}

// TestBoardArmDetail: a selection change arms exactly one debounce; a same-id
// call is a no-op; a change while a timer is armed does NOT request a second one.
func TestBoardArmDetail(t *testing.T) {
	model := boardModel{Columns: []boardColumn{{Status: "planned", Cards: []boardCard{{ID: "AIRA-1"}, {ID: "AIRA-2"}}}}}
	bs := boardApplyModel(*newBoardState(), model) // selection at AIRA-1

	armed, want := boardArmDetail(bs)
	if !want || armed.Detail.ID != "AIRA-1" || armed.Detail.State != "loading" || !armed.Detail.Armed {
		t.Fatalf("first arm wrong: want-timer=%v detail=%#v", want, armed.Detail)
	}
	// Same id again: no new timer, no state churn.
	if again, want := boardArmDetail(armed); want {
		t.Fatalf("re-arming the same id requested another timer: %#v", again.Detail)
	}
	// Move the cursor to AIRA-2 while a timer is armed: retarget, but NO second timer.
	armed.Selected[0] = 1
	retargeted, want := boardArmDetail(armed)
	if want {
		t.Fatalf("retarget while armed requested a second timer")
	}
	if retargeted.Detail.ID != "AIRA-2" || !retargeted.Detail.Armed {
		t.Fatalf("retarget did not move to AIRA-2: %#v", retargeted.Detail)
	}
	// Empty selection clears the detail.
	empty, want := boardArmDetail(boardApplyModel(*newBoardState(), boardModel{Columns: []boardColumn{{Status: "planned"}}}))
	if want || empty.Detail.ID != "" {
		t.Fatalf("empty selection should clear detail: want=%v detail=%#v", want, empty.Detail)
	}
}

// TestOnBoardDetailDueFetchesCurrentSelection: when the debounce fires it clears
// Armed and dispatches the fetch for the CURRENT selection (which may have moved
// on since the timer was armed).
func TestOnBoardDetailDueFetchesCurrentSelection(t *testing.T) {
	model := boardModel{Columns: []boardColumn{{Status: "planned", Cards: []boardCard{{ID: "AIRA-1"}, {ID: "AIRA-2"}}}}}
	bs := boardApplyModel(*newBoardState(), model)
	bs.Detail = boardDetailState{ID: "AIRA-1", State: "loading", Armed: true}
	bs.Selected[0] = 1 // the cursor moved to AIRA-2 during the debounce window
	state := boardTUIState(&bs)

	next, commands := onBoardDetailDue(state)
	if next.Board.Detail.Armed {
		t.Fatalf("due did not clear the armed flag")
	}
	if next.Board.Detail.ID != "AIRA-2" {
		t.Fatalf("due targeted %q, want the current selection AIRA-2", next.Board.Detail.ID)
	}
	found := false
	for _, command := range commands {
		if command.Kind == cmdFetch && command.View == viewBoard && command.DetailID == "AIRA-2" {
			found = true
		}
	}
	if !found {
		t.Fatalf("due did not fetch the current selection: %#v", commands)
	}
}

// TestOnTUIDetailResultBoardKeying: a result for the held id lands as ready; a
// stale result for a since-changed selection is dropped.
func TestOnTUIDetailResultBoardKeying(t *testing.T) {
	bs := newBoardState()
	bs.Detail = boardDetailState{ID: "AIRA-1", State: "loading"}
	state := boardTUIState(bs)

	// Matching id -> stored, ready.
	landed, _ := onTUIDetailResult(state, detailResult{View: viewBoard, ID: "AIRA-1", Board: boardDetailModel{Title: "t"}})
	if landed.Board.Detail.State != "ready" || landed.Board.Detail.Model.Title != "t" {
		t.Fatalf("matching detail did not land: %#v", landed.Board.Detail)
	}

	// Stale id -> dropped (still loading, no model).
	stale, _ := onTUIDetailResult(state, detailResult{View: viewBoard, ID: "AIRA-OTHER", Board: boardDetailModel{Title: "wrong"}})
	if stale.Board.Detail.State == "ready" || stale.Board.Detail.Model.Title == "wrong" {
		t.Fatalf("stale detail was shown over the wrong ticket: %#v", stale.Board.Detail)
	}
}

// TestBoardInfoDetailGuardsMismatch pins the render honesty seam: the pane may
// only render a detail that actually describes the SELECTED card. A ready detail
// held for a different ticket (e.g. just after closing an overlay opened on an
// unloaded search hit) is collapsed to a loading placeholder keyed to the card,
// so its fields can never appear under the selected card's title (invariant 2).
func TestBoardInfoDetailGuardsMismatch(t *testing.T) {
	card := boardCard{ID: "AIRA-1", Title: "one"}

	// A ready detail for a DIFFERENT ticket must be guarded away.
	mismatch := boardDetailState{ID: "AIRA-777", State: "ready", Model: boardDetailModel{Assignee: "owner-of-777", Body: "body of 777"}}
	got := boardInfoDetail(card, mismatch)
	if got.ID != "AIRA-1" || got.State == "ready" {
		t.Fatalf("mismatched detail was not guarded: %#v", got)
	}
	meta := strings.Join(boardDetailMetaLines(card, got), "\n")
	if strings.Contains(meta, "owner-of-777") {
		t.Fatalf("guarded pane leaked the wrong ticket's assignee:\n%s", meta)
	}
	if body := boardDetailBodyText(got); body != "loading…" {
		t.Fatalf("guarded pane body = %q, want loading…", body)
	}

	// A ready detail that DOES describe the selected card passes through untouched.
	match := boardDetailState{ID: "AIRA-1", State: "ready", Model: boardDetailModel{Assignee: "mark", Body: "real body"}}
	if got := boardInfoDetail(card, match); got.State != "ready" || got.Model.Assignee != "mark" {
		t.Fatalf("matching detail was disturbed: %#v", got)
	}
}
