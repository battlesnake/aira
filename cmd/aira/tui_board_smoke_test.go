package main

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"aira/internal/core"
	"aira/internal/daemon"

	"github.com/gdamore/tcell/v2"
)

// boardSmokeDispatcher answers the board's composite fetch with fixtures chosen
// to exercise every honest badge on screen: AIRA-1 carries a hold (⏸) and an
// unsatisfied blocker (⛔1) and a tag-injecting title ([x]); AIRA-2 is ready (●);
// a held lease and a nil-command job populate the sessions strip.
type boardSmokeDispatcher struct{}

func (boardSmokeDispatcher) Dispatch(ctx context.Context, _ daemon.WorktreeScope, request core.Request) core.Response {
	if request.Verb == "watch" {
		<-ctx.Done()
		return core.Response{Code: daemon.CodeUnavailable, Error: ctx.Err().Error()}
	}
	raw := "{}"
	switch request.Verb {
	case "list":
		query, _ := request.Args["query"].(string)
		switch {
		case strings.Contains(query, "status:planned"):
			raw = `{"total":3,"rows":[` +
				`{"id":"AIRA-1","status":"planned","severity":"P0","kind":"bug","title":"boom","hold":true,"relations":[{"kind":"blocks","from":"AIRA-2","to":"AIRA-1"}]},` +
				`{"id":"AIRA-2","status":"planned","severity":"P1","kind":"feature","title":"prereq","hold":false},` +
				`{"id":"AIRA-3","status":"planned","severity":"P2","kind":"chore","title":"[x]","hold":false}]}`
		case strings.Contains(query, "status:done"):
			raw = `{"total":1,"rows":[{"id":"AIRA-50","status":"done","severity":"P2","kind":"chore","title":"finished","hold":false}]}`
		default:
			raw = `{"total":0,"rows":[]}`
		}
	case "ready":
		if _, ok := request.Args["selector"]; ok {
			raw = `{"id":"AIRA-1","ready":false,"blockers":[],"verdict":"pass"}`
		} else {
			raw = `{"total":1,"rows":[{"id":"AIRA-2","ready":true}]}`
		}
	case "lease":
		raw = `{"total":1,"rows":[{"ticket_id":"AIRA-9","actor":"opus","worktree_id":"wt9","generation":1,"ttl_ns":1,"expired":false,"age_note":"2m ago"}]}`
	case "confine-list":
		raw = `{"verdict":"ok","scopes":[{"name":"job-x","owner":"unknown","supervisor_pid":null,"scope_id":"s1","rss_bytes":null,"subtree_populated":null,"supervisor_live":null,"age_seconds":null,"cap":null,"command":null,"reserve_bytes":null}]}`
	case "link", "find", "show":
		raw = `{"ok":true}`
	}
	return core.Response{OK: true, Code: "OK", RawData: json.RawMessage(raw)}
}

// resizeBoardScreen resizes the simulation screen AFTER app.Run()'s Init() reset
// it to 80x25, then forces a redraw on the UI goroutine so the width seam relays
// out at the true size (tcell's SetSize posts no resize event, and Init hard-sets
// 80x25 — so a pre-Run SetSize never takes; P2.12).
func resizeBoardScreen(t *testing.T, runtime *tuiRuntime, screen tcell.SimulationScreen, w, h int) {
	t.Helper()
	done := make(chan struct{})
	go runtime.app.QueueUpdateDraw(func() { screen.SetSize(w, h); close(done) })
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("resize deadlocked")
	}
	time.Sleep(30 * time.Millisecond) // let the resize-driven relayout settle
}

func readBoardStripWindow(t *testing.T, runtime *tuiRuntime) (int, int) {
	t.Helper()
	result := make(chan [2]int, 1)
	go runtime.app.QueueUpdate(func() { result <- [2]int{runtime.boardUI.lastStart, runtime.boardUI.lastEnd} })
	select {
	case window := <-result:
		return window[0], window[1]
	case <-time.After(time.Second):
		t.Fatal("reading strip window deadlocked")
		return 0, 0
	}
}

// readBoardSelectedID reads the reducer's authoritative selected-card id on the UI
// goroutine — the ground truth that Home/End must keep in sync with the cursor.
func readBoardSelectedID(t *testing.T, runtime *tuiRuntime) string {
	t.Helper()
	result := make(chan string, 1)
	go runtime.app.QueueUpdate(func() { result <- boardSelectedCardID(*runtime.state.Board) })
	select {
	case id := <-result:
		return id
	case <-time.After(time.Second):
		t.Fatal("reading selected card deadlocked")
		return ""
	}
}

// readBoardFocusedCards reads the focused column's card ids in order (ground truth
// for first/last, so the test needn't assume how the board orders cards).
func readBoardFocusedCards(t *testing.T, runtime *tuiRuntime) []string {
	t.Helper()
	result := make(chan []string, 1)
	go runtime.app.QueueUpdate(func() {
		b := runtime.state.Board
		var ids []string
		if b != nil && b.FocusedCol >= 0 && b.FocusedCol < len(b.Model.Columns) {
			for _, c := range b.Model.Columns[b.FocusedCol].Cards {
				ids = append(ids, c.ID)
			}
		}
		result <- ids
	})
	select {
	case ids := <-result:
		return ids
	case <-time.After(time.Second):
		t.Fatal("reading focused cards deadlocked")
		return nil
	}
}

// TestBoardHomeEndUpdateSelection is the AIRA-256 regression on the real tview
// runtime: Home/End must move the reducer's selection (not just the tview Table
// cursor), so the NEXT arrow moves from the Home/End position rather than snapping
// back to the pre-jump selection. The prior tests never pressed Home/End, which is
// how the desync shipped.
func TestBoardHomeEndUpdateSelection(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	screen := tcell.NewSimulationScreen("UTF-8")
	runtime := newBoardRuntime(ctx, boardSmokeDispatcher{}, daemon.WorktreeScope{}, nil, nil, nil, screen)
	done := make(chan error, 1)
	go func() { done <- runtime.run() }()

	waitForSimulationText(t, runtime, screen, "AIRA-1")
	resizeBoardScreen(t, runtime, screen, 120, 40)
	waitForSimulationText(t, runtime, screen, "AIRA-1")
	// Focus the planned column (draft -> planned); the smoke fixture gives it 3 cards.
	screen.InjectKey(tcell.KeyRune, 'l', tcell.ModNone)
	time.Sleep(30 * time.Millisecond)
	cards := readBoardFocusedCards(t, runtime)
	if len(cards) < 3 {
		t.Fatalf("expected the planned column to have >=3 cards, got %v", cards)
	}

	// Move off the first card, then Home: the selection must be the first card.
	screen.InjectKey(tcell.KeyDown, 0, tcell.ModNone)
	time.Sleep(20 * time.Millisecond)
	screen.InjectKey(tcell.KeyHome, 0, tcell.ModNone)
	time.Sleep(20 * time.Millisecond)
	if id := readBoardSelectedID(t, runtime); id != cards[0] {
		t.Fatalf("Home did not select the first card: got %q, want %q", id, cards[0])
	}
	// The next Down must move to the SECOND card, not snap back to the pre-Home one.
	screen.InjectKey(tcell.KeyDown, 0, tcell.ModNone)
	time.Sleep(20 * time.Millisecond)
	if id := readBoardSelectedID(t, runtime); id != cards[1] {
		t.Fatalf("Down after Home snapped back instead of moving from the first card: got %q, want %q", id, cards[1])
	}
	// End -> last card; the next Up moves to the second-to-last.
	screen.InjectKey(tcell.KeyEnd, 0, tcell.ModNone)
	time.Sleep(20 * time.Millisecond)
	if id := readBoardSelectedID(t, runtime); id != cards[len(cards)-1] {
		t.Fatalf("End did not select the last card: got %q, want %q", id, cards[len(cards)-1])
	}
	screen.InjectKey(tcell.KeyUp, 0, tcell.ModNone)
	time.Sleep(20 * time.Millisecond)
	if id := readBoardSelectedID(t, runtime); id != cards[len(cards)-2] {
		t.Fatalf("Up after End snapped back instead of moving from the last card: got %q, want %q", id, cards[len(cards)-2])
	}

	screen.InjectKey(tcell.KeyRune, 'q', tcell.ModNone)
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("board Home/End smoke did not quit")
	}
}

func TestBoardSmokeRendersBadgesAndDrillIn(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	screen := tcell.NewSimulationScreen("UTF-8")
	runtime := newBoardRuntime(ctx, boardSmokeDispatcher{}, daemon.WorktreeScope{}, nil, nil, nil, screen)
	done := make(chan error, 1)
	go func() { done <- runtime.run() }()

	// Wait for the app to be live at the Init 80x25, then widen so the seven
	// columns are each wide enough to show a full card line (id + sev + badges +
	// kind + the escaped title), making the [x] escape verifiable on screen.
	waitForSimulationText(t, runtime, screen, "AIRA-1")
	resizeBoardScreen(t, runtime, screen, 220, 40)
	text := waitForSimulationText(t, runtime, screen, "[x]")
	for _, needle := range []string{"⏸", "⛔", "●", "[x]", "bug", "lease AIRA-9", "planned"} {
		if !strings.Contains(text, needle) {
			t.Fatalf("board render missing %q:\n%s", needle, text)
		}
	}
	// A blank hold/ready/blocked negative is never fabricated: AIRA-50 (done) has
	// no badges. Its column shows once scrolled to, but the honesty is unit-tested;
	// here we drill into AIRA-1.
	screen.InjectKey(tcell.KeyRight, 0, tcell.ModNone) // focus draft→planned
	time.Sleep(20 * time.Millisecond)
	screen.InjectKey(tcell.KeyEnter, 0, tcell.ModNone) // drill into AIRA-1
	if detail := waitForSimulationText(t, runtime, screen, "Esc to close"); !strings.Contains(detail, "AIRA-1") {
		t.Fatalf("drill-in detail did not open for AIRA-1:\n%s", detail)
	}
	screen.InjectKey(tcell.KeyEscape, 0, tcell.ModNone) // close detail
	screen.InjectKey(tcell.KeyRune, 'q', tcell.ModNone)
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("board smoke did not quit")
	}
}

// TestBoardSmokeHorizontalScroll is the width-seam case: at 40 columns only a
// couple of the seven status columns fit, and moving the focus right must scroll
// the strip to bring later columns (the done card) into view.
func TestBoardSmokeHorizontalScroll(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	screen := tcell.NewSimulationScreen("UTF-8")
	runtime := newBoardRuntime(ctx, boardSmokeDispatcher{}, daemon.WorktreeScope{}, nil, nil, nil, screen)
	done := make(chan error, 1)
	go func() { done <- runtime.run() }()

	// Init reset the screen to 80x25; resize to a TRUE 40 columns so the seam is
	// exercised at 40 (fit=2), not 80 (fit=4).
	waitForSimulationText(t, runtime, screen, "AIRA-1")
	resizeBoardScreen(t, runtime, screen, 40, 30)
	if start, end := readBoardStripWindow(t, runtime); start != 0 || end != 2 {
		t.Fatalf("at 40 columns the visible window = [%d,%d), want [0,2) (fit=2)", start, end)
	}
	// planned (column 1) is in the [0,2) window; AIRA-50 (done, column 4) is not.
	initial := waitForSimulationText(t, runtime, screen, "AIRA-1")
	if strings.Contains(initial, "AIRA-50") {
		t.Fatalf("done column should not fit at 40 columns initially:\n%s", initial)
	}
	// Move focus right toward `done`, scrolling the strip.
	for i := 0; i < 4; i++ {
		screen.InjectKey(tcell.KeyRune, 'l', tcell.ModNone)
		time.Sleep(15 * time.Millisecond)
	}
	if scrolled := waitForSimulationText(t, runtime, screen, "AIRA-50"); !strings.Contains(scrolled, "AIRA-50") {
		t.Fatalf("done card never scrolled into view at 40 columns:\n%s", scrolled)
	}
	screen.InjectKey(tcell.KeyRune, 'q', tcell.ModNone)
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("board scroll smoke did not quit")
	}
}

// TestBoardSearchOverlayUnevaluated drives the real tview path: `/` opens the
// search, a content query dispatches grep, and an E_INDEX_UNEVALUATED reply
// renders "search unevaluated" — never "no matches".
func TestBoardSearchOverlayUnevaluated(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	screen := tcell.NewSimulationScreen("UTF-8")
	runtime := newBoardRuntime(ctx, boardSearchDispatcher{}, daemon.WorktreeScope{}, nil, nil, nil, screen)
	done := make(chan error, 1)
	go func() { done <- runtime.run() }()
	waitForSimulationText(t, runtime, screen, "AIRA-1")
	resizeBoardScreen(t, runtime, screen, 120, 40) // the results overlay is 90 wide

	screen.InjectKey(tcell.KeyRune, '/', tcell.ModNone)
	time.Sleep(20 * time.Millisecond)
	for _, ch := range "parser" {
		screen.InjectKey(tcell.KeyRune, ch, tcell.ModNone)
	}
	screen.InjectKey(tcell.KeyEnter, 0, tcell.ModNone)
	if text := waitForSimulationText(t, runtime, screen, "search unevaluated"); strings.Contains(text, "no matches") {
		t.Fatalf("unevaluated grep rendered 'no matches':\n%s", text)
	}
	screen.InjectKey(tcell.KeyRune, 'q', tcell.ModNone)
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("board search smoke did not quit")
	}
}

// TestBoardSearchResultsJump drives the headline search UX (spec §10, P2.7): `/`
// → a content query → the results overlay lists the match → Enter jumps to the
// card. The stub returns a grep hit for AIRA-2 (a loaded planned card).
func TestBoardSearchResultsJump(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	screen := tcell.NewSimulationScreen("UTF-8")
	runtime := newBoardRuntime(ctx, boardResultsDispatcher{}, daemon.WorktreeScope{}, nil, nil, nil, screen)
	done := make(chan error, 1)
	go func() { done <- runtime.run() }()
	waitForSimulationText(t, runtime, screen, "AIRA-1")
	resizeBoardScreen(t, runtime, screen, 120, 40)

	screen.InjectKey(tcell.KeyRune, '/', tcell.ModNone)
	time.Sleep(20 * time.Millisecond)
	for _, ch := range "prereq" {
		screen.InjectKey(tcell.KeyRune, ch, tcell.ModNone)
	}
	screen.InjectKey(tcell.KeyEnter, 0, tcell.ModNone) // submit → results overlay
	// "Search:" is the results-overlay title prefix — it appears only when the
	// overlay is open (the board itself never prints it).
	waitForSimulationText(t, runtime, screen, "Search:")
	screen.InjectKey(tcell.KeyEnter, 0, tcell.ModNone) // open selected result → jump to AIRA-2's column
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		focus := make(chan int, 1)
		go runtime.app.QueueUpdate(func() { focus <- runtime.state.Board.FocusedCol })
		if <-focus == 1 { // planned column
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	focus := make(chan int, 1)
	go runtime.app.QueueUpdate(func() { focus <- runtime.state.Board.FocusedCol })
	if got := <-focus; got != 1 {
		t.Fatalf("Enter on a loaded result did not jump to its column: focus=%d", got)
	}
	screen.InjectKey(tcell.KeyRune, 'q', tcell.ModNone)
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("board results smoke did not quit")
	}
}

// boardInfoLongTitle is well over boardTitleMax (72 runes) with a distinctive
// tail word: the column cell truncates it (so ENDMARKER never appears in a
// column), but the info pane and the expand overlay must show it in FULL — so
// ENDMARKER on screen proves the full, untruncated title is rendered there.
const boardInfoLongTitle = "The board info pane must display a genuinely long ticket title in full and wrap it rather than cutting it off ENDMARKER"

// boardInfoDispatcher gives the selected card (AIRA-1) a long title and a rich,
// fully-established detail (assignee, labels, milestone, a relation, a finding
// count and a two-line body) so the info-pane render can be driven end-to-end.
type boardInfoDispatcher struct{ boardSmokeDispatcher }

func (d boardInfoDispatcher) Dispatch(ctx context.Context, scope daemon.WorktreeScope, request core.Request) core.Response {
	ok := func(raw string) core.Response {
		return core.Response{OK: true, Code: "OK", RawData: json.RawMessage(raw)}
	}
	switch request.Verb {
	case "list":
		query, _ := request.Args["query"].(string)
		if strings.Contains(query, "status:planned") {
			return ok(`{"total":1,"rows":[{"id":"AIRA-1","status":"planned","severity":"P0","kind":"bug","title":"` + boardInfoLongTitle + `","hold":false}]}`)
		}
		return ok(`{"total":0,"rows":[]}`)
	case "show":
		return ok(`{"title":"` + boardInfoLongTitle + `","status":"planned","severity":"P0","kind":"bug","assignee":"opus","labels":["ui","board"],"milestone":"v0.11","body":"BODYWORD is the first line of the body.\nA second body line follows here."}`)
	case "link":
		return ok(`[{"kind":"blocks","from":"AIRA-1","to":"AIRA-2"}]`)
	case "find":
		return ok(`{"total":3}`)
	}
	return d.boardSmokeDispatcher.Dispatch(ctx, scope, request)
}

// TestBoardInfoPaneRendersFullDetail is the load-bearing geometry test (the seams
// #24 found statically unpinned: renderBoardInfo, layoutBoardInfo,
// boardInfoPaneHeight, the 'f' toggle, the responsive overlay). It drives the
// real tview runtime on a SimulationScreen and asserts the owner's requirements
// at real sizes: the FULL title is never truncated in the pane, the body and
// fields render when there is room, the overlay is not clipped at 80x24, and 'f'
// collapses to a single column.
func TestBoardInfoPaneRendersFullDetail(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	screen := tcell.NewSimulationScreen("UTF-8")
	runtime := newBoardRuntime(ctx, boardInfoDispatcher{}, daemon.WorktreeScope{}, nil, nil, nil, screen)
	done := make(chan error, 1)
	go func() { done <- runtime.run() }()

	waitForSimulationText(t, runtime, screen, "AIRA-1")
	// Wide and tall so the whole detail fits: full title, all fields, body.
	resizeBoardScreen(t, runtime, screen, 120, 56)
	waitForSimulationText(t, runtime, screen, "AIRA-1") // let the resize settle before input
	// Focus draft→planned so AIRA-1 is selected and the pane fetches its detail.
	screen.InjectKey(tcell.KeyRune, 'l', tcell.ModNone)
	time.Sleep(30 * time.Millisecond)
	// Wait on a body word — unique to the loaded pane detail (unlike "opus", which
	// also appears in the sessions strip's lease row).
	text := waitForSimulationText(t, runtime, screen, "BODYWORD")
	for _, needle := range []string{"ENDMARKER", "opus", "v0.11", "blocks AIRA-2", "findings: 3", "BODYWORD"} {
		if !strings.Contains(text, needle) {
			t.Fatalf("wide info pane missing %q:\n%s", needle, text)
		}
	}
	// The column cell truncates the same title, so the tail is pane-only: the whole
	// title must not be sitting in a column row.
	if strings.Count(text, "ENDMARKER") != 1 {
		t.Fatalf("ENDMARKER should appear once (the pane), not in a truncated column cell:\n%s", text)
	}

	// Full-width toggle: 'f' collapses the strip to the single focused column.
	screen.InjectKey(tcell.KeyRune, 'f', tcell.ModNone)
	time.Sleep(30 * time.Millisecond)
	if start, end := readBoardStripWindow(t, runtime); end-start != 1 {
		t.Fatalf("'f' full-width did not collapse to one column: window [%d,%d)", start, end)
	}
	screen.InjectKey(tcell.KeyRune, 'f', tcell.ModNone) // back to multi-column
	time.Sleep(30 * time.Millisecond)

	// Shrink to the common 80x24 and open the expand overlay: its box must be
	// clamped to fit, so its border title and the full title/body are on-screen
	// (a fixed 100x30 box would be placed off-screen and clip all three).
	resizeBoardScreen(t, runtime, screen, 80, 24)
	screen.InjectKey(tcell.KeyEnter, 0, tcell.ModNone)
	overlay := waitForSimulationText(t, runtime, screen, "Esc to close")
	for _, needle := range []string{"AIRA-1", "ENDMARKER", "BODYWORD"} {
		if !strings.Contains(overlay, needle) {
			t.Fatalf("expand overlay at 80x24 missing %q (clipped?):\n%s", needle, overlay)
		}
	}
	screen.InjectKey(tcell.KeyEscape, 0, tcell.ModNone)

	screen.InjectKey(tcell.KeyRune, 'q', tcell.ModNone)
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("board info pane smoke did not quit")
	}
}

// TestBoardInfoPaneSurvivesResize is the re-review P2/P3 regression: a resize
// across the wide/narrow threshold with NO follow-up keypress must re-compose the
// pane (renderBoardInfo is width-dependent and a tcell resize fires beforeDraw but
// not render()). The full title and fields must not vanish, and the "+N ↵" clip
// disclosure must be conditional — present only when content actually overflows.
func TestBoardInfoPaneSurvivesResize(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	screen := tcell.NewSimulationScreen("UTF-8")
	runtime := newBoardRuntime(ctx, boardInfoDispatcher{}, daemon.WorktreeScope{}, nil, nil, nil, screen)
	done := make(chan error, 1)
	go func() { done <- runtime.run() }()

	waitForSimulationText(t, runtime, screen, "AIRA-1")
	resizeBoardScreen(t, runtime, screen, 120, 56)
	waitForSimulationText(t, runtime, screen, "AIRA-1")
	screen.InjectKey(tcell.KeyRune, 'l', tcell.ModNone) // select AIRA-1
	time.Sleep(30 * time.Millisecond)
	wide := waitForSimulationText(t, runtime, screen, "BODYWORD")
	if strings.Contains(wide, "↵") {
		t.Fatalf("false clip disclosure at 120x56 where everything fits:\n%s", wide)
	}

	// Wide→narrow with NO keypress: content must re-compose (not vanish), and the
	// now-clipped pane must disclose the overflow.
	resizeBoardScreen(t, runtime, screen, 80, 24)
	narrow := waitForSimulationText(t, runtime, screen, "ENDMARKER")
	if !strings.Contains(narrow, "ENDMARKER") {
		t.Fatalf("full title vanished after a wide→narrow resize with no keypress (P2):\n%s", narrow)
	}
	if !strings.Contains(narrow, "↵") {
		t.Fatalf("clipped narrow pane did not disclose the overflow (P3):\n%s", narrow)
	}

	// Narrow→wide with NO keypress: content re-splits (title+fields back), and the
	// false disclosure must be gone.
	resizeBoardScreen(t, runtime, screen, 120, 56)
	back := waitForSimulationText(t, runtime, screen, "BODYWORD")
	if !strings.Contains(back, "ENDMARKER") || !strings.Contains(back, "opus") {
		t.Fatalf("title/fields not restored after a narrow→wide resize with no keypress (P2):\n%s", back)
	}
	if strings.Contains(back, "↵") {
		t.Fatalf("false clip disclosure after narrow→wide resize (P2):\n%s", back)
	}

	screen.InjectKey(tcell.KeyRune, 'q', tcell.ModNone)
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("board info resize smoke did not quit")
	}
}

type boardResultsDispatcher struct{ boardSmokeDispatcher }

func (d boardResultsDispatcher) Dispatch(ctx context.Context, scope daemon.WorktreeScope, request core.Request) core.Response {
	if request.Verb == "grep" {
		return core.Response{OK: true, Code: "OK", RawData: json.RawMessage(`{"total":1,"rows":[{"id":"AIRA-2","snippet":"…prereq…"}]}`)}
	}
	return d.boardSmokeDispatcher.Dispatch(ctx, scope, request)
}

type boardSearchDispatcher struct{ boardSmokeDispatcher }

func (d boardSearchDispatcher) Dispatch(ctx context.Context, scope daemon.WorktreeScope, request core.Request) core.Response {
	if request.Verb == "grep" {
		return core.Response{OK: true, Code: "OK", RawData: json.RawMessage(`{"total":0,"rows":[],"unevaluated":true}`)}
	}
	return d.boardSmokeDispatcher.Dispatch(ctx, scope, request)
}
