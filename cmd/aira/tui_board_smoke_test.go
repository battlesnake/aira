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

func boardStripWindow(t *testing.T, runtime *tuiRuntime) (int, int) {
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
	if start, end := boardStripWindow(t, runtime); start != 0 || end != 2 {
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
	waitForSimulationText(t, runtime, screen, "Search results")
	if text := simulationTextOnUI(t, runtime, screen); !strings.Contains(text, "AIRA-2") {
		t.Fatalf("results overlay did not list the AIRA-2 match:\n%s", text)
	}
	screen.InjectKey(tcell.KeyEnter, 0, tcell.ModNone) // open → jump to AIRA-2's column
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
