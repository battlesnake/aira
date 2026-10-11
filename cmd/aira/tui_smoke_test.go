package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"aira/internal/core"
	"aira/internal/daemon"
	"aira/internal/testdeadline"

	"github.com/gdamore/tcell/v2"
	"github.com/rivo/tview"
)

type tuiSmokeDispatcher struct {
	blockList bool
	started   chan struct{}
	once      sync.Once
}

func (d *tuiSmokeDispatcher) Dispatch(ctx context.Context, _ daemon.WorktreeScope, request core.Request) core.Response {
	if request.Verb == "watch" {
		<-ctx.Done()
		return core.Response{Code: daemon.CodeUnavailable, Error: ctx.Err().Error()}
	}
	if request.Verb == "list" && d.blockList {
		d.once.Do(func() { close(d.started) })
		<-ctx.Done()
		return core.Response{Code: daemon.CodeUnavailable, Error: ctx.Err().Error()}
	}
	var raw string
	switch request.Verb {
	case "list", "ready", "find":
		raw = `{"total":0,"rows":[]}`
	case "lease":
		raw = `{"total":0,"rows":[]}`
	case "insights":
		raw = `[]`
	default:
		raw = `{}`
	}
	return core.Response{OK: true, Code: "OK", RawData: json.RawMessage(raw)}
}

func TestTUIKeypressAndQuitWhileFetchAndQueueUpdateAreInFlight(t *testing.T) {
	dispatcher := &tuiSmokeDispatcher{blockList: true, started: make(chan struct{})}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	screen := tcell.NewSimulationScreen("UTF-8")
	screen.SetSize(100, 30)
	runtime := newTUIRuntime(ctx, dispatcher, nil, daemon.WorktreeScope{}, nil, nil, nil, screen)
	keys := trackKeys(runtime, screen)
	drawn := make(chan struct{})
	var drawOnce sync.Once
	runtime.app.SetAfterDrawFunc(func(tcell.Screen) { drawOnce.Do(func() { close(drawn) }) })
	done := make(chan error, 1)
	go func() { done <- runtime.run() }()
	select {
	case <-drawn:
	case <-testdeadline.After(time.Second):
		t.Fatal("initial draw timed out")
	}
	select {
	case <-dispatcher.started:
	case <-testdeadline.After(time.Second):
		t.Fatal("ticket fetch did not start")
	}

	keys.press(t, runeKey('r'))
	var panel panelState
	eventuallyOnUI(t, runtime, time.Second, func() bool {
		panel = runtime.state.Panels[viewTickets]
		return panel.Dirty
	}, "the refresh key never marked the tickets panel dirty")
	if !panel.InFlight || !panel.Dirty || panel.Generation != 1 {
		t.Fatalf("inline refresh state=%#v", panel)
	}

	// Quit while a fetch is still in flight (tickets is InFlight+Dirty above). The
	// full quit path must complete cleanly: the q handler marks+cancels+returns on
	// the UI goroutine, and a SEPARATE coordinator joins the pump/executor then
	// Stops — it must never join on the UI goroutine (that would deadlock). The
	// pump-blocked-in-QueueUpdateDraw release is proven deterministically by
	// TestPumpExitsOnCancelWhenUILoopAbsent.
	screen.InjectKey(tcell.KeyRune, 'q', tcell.ModNone)
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-testdeadline.After(3 * time.Second):
		t.Fatal("TUI quit deadlocked or did not complete")
	}
}

// TestPumpExitsOnCancelWhenUILoopAbsent guards the abnormal-app.Run deadlock fix
// directly at the pump. The tview event loop is NEVER started, so any
// QueueUpdateDraw the pump issues blocks forever — modelling an app.Run that
// returned abnormally (e.g. a screen-init failure) while the coordinator still
// needs to join the pump. A non-abandonable pump would block in QueueUpdateDraw
// and pumpDone would never close; the abandonable delivery must let cancel
// release it. Proven to hang against a synchronous-QueueUpdateDraw pump.
func TestPumpExitsOnCancelWhenUILoopAbsent(t *testing.T) {
	dispatcher := &tuiSmokeDispatcher{started: make(chan struct{})}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	screen := tcell.NewSimulationScreen("UTF-8")
	screen.SetSize(100, 30)
	runtime := newTUIRuntime(ctx, dispatcher, nil, daemon.WorktreeScope{}, nil, nil, nil, screen)
	started := make(chan struct{}, 1)
	runtime.queueUpdateStarted = started

	go runtime.pump() // no app.Run(): the UI loop is absent, so QueueUpdateDraw blocks.
	runtime.executor.messages <- tuiMessage{Kind: msgWatchBatch, Cursor: 1}
	select {
	case <-started:
	case <-testdeadline.After(2 * time.Second):
		t.Fatal("pump did not process the message")
	}
	cancel() // the pump is now stuck in QueueUpdateDraw; cancel must release it.
	select {
	case <-runtime.pumpDone:
	case <-testdeadline.After(2 * time.Second):
		t.Fatal("pump did not exit on cancel with the UI loop absent — shutdown would deadlock")
	}
}

func TestTUIPaletteOpensOnColon(t *testing.T) {
	dispatcher := &tuiSmokeDispatcher{started: make(chan struct{})}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	screen := tcell.NewSimulationScreen("UTF-8")
	screen.SetSize(100, 30)
	runtime := newTUIRuntime(ctx, dispatcher, nil, daemon.WorktreeScope{}, nil, nil, nil, screen)
	keys := trackKeys(runtime, screen)
	drawn := make(chan struct{}, 1)
	runtime.app.SetAfterDrawFunc(func(tcell.Screen) {
		select {
		case drawn <- struct{}{}:
		default:
		}
	})
	done := make(chan error, 1)
	go func() { done <- runtime.run() }()
	select {
	case <-drawn:
	case <-testdeadline.After(time.Second):
		t.Fatal("initial draw timed out")
	}
	keys.press(t, runeKey(':'))
	contents := waitForSimulationText(t, runtime, screen, "Command palette")
	if !strings.Contains(contents, "find ls") {
		t.Fatalf("palette not rendered:\n%s", contents)
	}
	screen.InjectKey(tcell.KeyEscape, 0, tcell.ModNone)
	screen.InjectKey(tcell.KeyEscape, 0, tcell.ModNone)
	screen.InjectKey(tcell.KeyRune, 'q', tcell.ModNone)
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-testdeadline.After(2 * time.Second):
		t.Fatal("palette TUI did not quit")
	}
}

type tuiMutationDispatcher struct {
	mu       sync.Mutex
	requests []core.Request
}

func (d *tuiMutationDispatcher) Dispatch(ctx context.Context, _ daemon.WorktreeScope, request core.Request) core.Response {
	if request.Verb == "watch" {
		<-ctx.Done()
		return core.Response{Code: daemon.CodeUnavailable, Error: ctx.Err().Error()}
	}
	if request.Verb == "set" || request.Verb == "count" || request.Verb == "rant" {
		d.mu.Lock()
		d.requests = append(d.requests, clonePaletteRequest(request))
		d.mu.Unlock()
	}
	return core.Response{OK: true, Code: "OK", RawData: json.RawMessage(`{"ok":true}`)}
}

func (d *tuiMutationDispatcher) count(verb string) int {
	d.mu.Lock()
	defer d.mu.Unlock()
	count := 0
	for _, request := range d.requests {
		if request.Verb == verb {
			count++
		}
	}
	return count
}

// covers: the form-submit Enter, y, Cancel-default Enter, and key repeat are
// non-affirmative; only navigating to Confirm and pressing Enter dispatches.
func TestTUIConfirmationRequiresFocusedConfirmEnterAndDispatchesOnce(t *testing.T) {
	dispatcher := &tuiMutationDispatcher{}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	screen := tcell.NewSimulationScreen("UTF-8")
	screen.SetSize(110, 36)
	runtime := newTUIRuntime(ctx, dispatcher, nil, daemon.WorktreeScope{}, nil, nil, nil, screen)
	keys := trackKeys(runtime, screen)
	done := make(chan error, 1)
	go func() { done <- runtime.run() }()
	waitForSimulationText(t, runtime, screen, "1:Tickets")

	entry := paletteEntry{
		Verb: "set", Summary: "set a ticket field", Safety: core.SafetyMutate,
		Args: []paletteArg{{Spec: core.ArgSpec{Name: "selector", Kind: core.ArgKindString}, Required: true}},
	}
	openPaletteEntryOnUI(t, runtime, entry)
	keys.typeText("AIRA-1")
	keys.press(t, specialKey(tcell.KeyTab))
	waitForButtonFocusOnUI(t, runtime, runtime.paletteSubmitButton)
	keys.press(t, specialKey(tcell.KeyEnter)) // form submit only.
	waitForConfirmationOnUI(t, runtime)
	waitForSimulationText(t, runtime, screen, "Confirm mutation")
	assertConfirmCancelFocused(t, runtime)
	assertNotDispatched(t, runtime, dispatcher, "set", confirmationOpen, "form-submit Enter")
	keys.press(t, runeKey('y'))
	assertNotDispatched(t, runtime, dispatcher, "set", confirmationOpen, "single-key y")
	keys.press(t, specialKey(tcell.KeyEnter)) // default Cancel.
	assertNotDispatched(t, runtime, dispatcher, "set", confirmationClosed, "default-focus Enter")

	openPaletteEntryOnUI(t, runtime, entry)
	keys.typeText("AIRA-1")
	keys.press(t, specialKey(tcell.KeyTab))
	waitForButtonFocusOnUI(t, runtime, runtime.paletteSubmitButton)
	keys.press(t, specialKey(tcell.KeyEnter))
	waitForConfirmationOnUI(t, runtime)
	waitForSimulationText(t, runtime, screen, "Confirm mutation")
	assertConfirmCancelFocused(t, runtime)
	keys.press(t, specialKey(tcell.KeyTab)) // Cancel -> Confirm.
	for i := 0; i < 9; i++ {
		screen.InjectKey(tcell.KeyEnter, 0, tcell.ModNone) // the Confirm Enter, then 8 repeats
	}
	testdeadline.Eventually(t, time.Second, func() bool { return dispatcher.count("set") >= 1 }, "focused Confirm never dispatched")
	// Every Enter has been handled once the barrier returns, so any duplicate has been
	// enqueued; the short settle below is a labelled negative wait (contention can
	// only make it pass).
	keys.barrier(t)
	time.Sleep(20 * time.Millisecond) // negative wait: a duplicate dispatch must not appear
	if got := dispatcher.count("set"); got != 1 {
		t.Fatalf("focused Confirm with repeat dispatched %d requests, want exactly 1", got)
	}

	// A read retains the v1 one-step path and never creates confirmation state.
	openPaletteEntryOnUI(t, runtime, paletteEntry{Verb: "count", Summary: "count", Safety: core.SafetyRead})
	testdeadline.Eventually(t, time.Second, func() bool { return dispatcher.count("count") >= 1 }, "read entry never dispatched")
	confirm := onUI(t, runtime, func() *paletteConfirm { return runtime.state.PaletteConfirm })
	if got := dispatcher.count("count"); got != 1 || confirm != nil {
		t.Fatalf("read dispatch count=%d confirm=%#v", got, confirm)
	}

	screen.InjectKey(tcell.KeyEscape, 0, tcell.ModNone)
	screen.InjectKey(tcell.KeyRune, 'q', tcell.ModNone)
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-testdeadline.After(2 * time.Second):
		t.Fatal("confirmation TUI did not quit")
	}
}

// confirmationExpectation is what a "must not dispatch" key is expected to leave of
// the palette confirmation. confirmationEither is for a key whose only required
// property is that it does not dispatch.
type confirmationExpectation int

const (
	confirmationEither confirmationExpectation = iota
	confirmationOpen
	confirmationClosed
)

// assertNotDispatched asserts, after a key has been handled, that nothing was
// dispatched: the synchronous state on the UI goroutine (no dispatch in flight, and
// the confirmation still open or already closed as the caller expects), then the
// dispatcher's own count as a labelled negative wait.
func assertNotDispatched(t *testing.T, runtime *tuiRuntime, dispatcher *tuiMutationDispatcher, verb string, confirmation confirmationExpectation, what string) {
	t.Helper()
	type state struct {
		dispatching bool
		confirm     bool
	}
	got := onUI(t, runtime, func() state {
		return state{dispatching: runtime.state.PaletteDispatching, confirm: runtime.state.PaletteConfirm != nil}
	})
	if got.dispatching {
		t.Fatalf("%s started a dispatch", what)
	}
	if confirmation == confirmationOpen && !got.confirm || confirmation == confirmationClosed && got.confirm {
		t.Fatalf("%s left the confirmation open=%v, want %v", what, got.confirm, confirmation == confirmationOpen)
	}
	time.Sleep(20 * time.Millisecond) // negative wait: contention can only make this pass
	if n := dispatcher.count(verb); n != 0 {
		t.Fatalf("%s dispatched %d requests", what, n)
	}
}

func TestTUIDestructiveConfirmationRequiresExactResolvedID(t *testing.T) {
	dispatcher := &tuiMutationDispatcher{}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	screen := tcell.NewSimulationScreen("UTF-8")
	screen.SetSize(110, 36)
	runtime := newTUIRuntime(ctx, dispatcher, nil, daemon.WorktreeScope{}, nil, nil, nil, screen)
	keys := trackKeys(runtime, screen)
	done := make(chan error, 1)
	go func() { done <- runtime.run() }()
	waitForSimulationText(t, runtime, screen, "1:Tickets")

	entry := paletteEntryNamed(t, buildPalette(runtime.descriptors), "rant", "redact")
	openPaletteEntryOnUI(t, runtime, entry)
	keys.typeText("RANT-7")
	// This entry comes from the REAL dispatch table, so how many fields sit
	// between the first one and Submit follows the rant descriptor — AIRA-179
	// added the optional --project/--prefix target selector after the required
	// rant id. Tab until Submit actually has focus rather than assuming a count,
	// so a later argument addition changes nothing here.
	tabToPaletteSubmit(t, runtime, keys)
	keys.press(t, specialKey(tcell.KeyEnter))
	eventuallyOnUI(t, runtime, time.Second, func() bool { return runtime.paletteConfirmForm != nil }, "the destructive confirmation form was never built")
	// The modal is 90 columns wide: resize so it is not clipped, then require the
	// "Type <ID>:" field itself to be visible (focus starts on Cancel, so a form
	// that is too short scrolls that row off screen).
	resizeBoardScreen(t, runtime, screen, 110, 36)
	waitForSimulationText(t, runtime, screen, "Type RANT-7")

	checkDisabled := func(want bool) {
		t.Helper()
		got := onUI(t, runtime, func() bool { return runtime.paletteConfirmForm.GetButton(0).IsDisabled() })
		if got != want {
			t.Fatalf("Confirm disabled=%v, want %v", got, want)
		}
	}
	checkDisabled(true)
	setConfirmIDOnUI(t, runtime, "RANT-8")
	checkDisabled(true)
	keys.press(t, runeKey('y'))
	assertNotDispatched(t, runtime, dispatcher, "rant", confirmationOpen, "wrong destructive id")
	setConfirmIDOnUI(t, runtime, "RANT-7")
	checkDisabled(false)

	keys.press(t, specialKey(tcell.KeyEscape))
	cancelled := onUIDraw(t, runtime, func() bool {
		cancelled := runtime.state.PaletteConfirm == nil
		runtime.closePalette()
		return cancelled
	})
	if !cancelled {
		t.Fatal("Escape did not cancel destructive confirmation")
	}
	screen.InjectKey(tcell.KeyRune, 'q', tcell.ModNone)
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-testdeadline.After(2 * time.Second):
		t.Fatal("destructive confirmation TUI did not quit")
	}
}

func TestTUIPaletteResultDoesNotPopAfterFlowWasClosed(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	runtime := newTUIRuntime(ctx, &tuiMutationDispatcher{}, nil, daemon.WorktreeScope{}, nil, nil, nil, nil)
	runtime.state.PaletteOpen = false
	runtime.state.PaletteDispatching = true
	runtime.state.PaletteDispatchedVerb = "spend.add"
	runtime.applyAsync(tuiMessage{Kind: msgPaletteResult, PaletteOutcome: paletteApplied, PaletteResult: "APPLIED"})
	if page, _ := runtime.outerPages.GetFrontPage(); page != dashboardPage {
		t.Fatalf("late palette result popped page %q over dashboard", page)
	}
	cancel()
	runtime.executor.wait()
}

func openPaletteEntryOnUI(t *testing.T, runtime *tuiRuntime, entry paletteEntry) {
	t.Helper()
	onUIDraw(t, runtime, func() struct{} {
		runtime.state.PaletteOpen = true
		runtime.openPaletteEntry(entry)
		return struct{}{}
	})
}

// waitForConfirmationOnUI waits until the palette's confirmation state exists.
func waitForConfirmationOnUI(t *testing.T, runtime *tuiRuntime) {
	t.Helper()
	eventuallyOnUI(t, runtime, time.Second, func() bool { return runtime.state.PaletteConfirm != nil }, "the palette confirmation never opened")
}

func assertConfirmCancelFocused(t *testing.T, runtime *tuiRuntime) {
	t.Helper()
	type focusState struct {
		buttons [2]bool
		focus   string
	}
	focused := onUI(t, runtime, func() focusState {
		return focusState{buttons: [2]bool{runtime.paletteConfirmForm.GetButton(0).HasFocus(), runtime.paletteConfirmForm.GetButton(1).HasFocus()}, focus: fmt.Sprintf("%T", runtime.app.GetFocus())}
	})
	if !focused.buttons[1] {
		t.Fatalf("confirmation default focus is not Cancel (Confirm=%v Cancel=%v app=%s)", focused.buttons[0], focused.buttons[1], focused.focus)
	}
}

// tabToPaletteSubmit advances focus with Tab until the palette form's Submit
// button holds it, so a test that opens a REAL dispatch-table entry does not
// encode how many argument fields that entry happens to declare today. Each Tab is
// followed by the key barrier, so the next focus read cannot overtake it.
func tabToPaletteSubmit(t *testing.T, runtime *tuiRuntime, keys *keyTracker) {
	t.Helper()
	for attempt := 0; attempt < 16; attempt++ {
		if onUI(t, runtime, func() bool { return runtime.paletteSubmitButton.HasFocus() }) {
			return
		}
		keys.press(t, specialKey(tcell.KeyTab))
	}
	t.Fatal("palette submit button never received focus")
}

func waitForButtonFocusOnUI(t *testing.T, runtime *tuiRuntime, button *tview.Button) {
	t.Helper()
	eventuallyOnUI(t, runtime, time.Second, func() bool { return button.HasFocus() }, "form submit button did not receive focus")
}

func setConfirmIDOnUI(t *testing.T, runtime *tuiRuntime, value string) {
	t.Helper()
	onUIDraw(t, runtime, func() struct{} {
		runtime.paletteConfirmForm.GetFormItem(0).(*tview.InputField).SetText(value)
		return struct{}{}
	})
}

type tuiRenderDispatcher struct{}

func (tuiRenderDispatcher) Dispatch(ctx context.Context, _ daemon.WorktreeScope, request core.Request) core.Response {
	if request.Verb == "watch" {
		<-ctx.Done()
		return core.Response{Code: daemon.CodeUnavailable}
	}
	if request.Verb == "find" {
		return core.Response{Code: "E_FINDING_INVALID", Error: "bad finding index"}
	}
	var raw string
	switch request.Verb {
	case "list":
		raw = `{"total":51,"rows":[{"id":"AIRA-1","status":"planned"}],"distribution":{"planned":51},"truncated":true}`
	case "ready", "lease":
		raw = `{"total":0,"rows":[]}`
	case "insights":
		if request.Args["subverb"] == "ls" {
			raw = `[{"name":"flaky-rate"}]`
		} else {
			raw = `{"name":"flaky-rate","kind":"rate","unevaluated":true,"unevaluated_reason":"no reports"}`
		}
	default:
		raw = `{}`
	}
	return core.Response{OK: true, Code: "OK", RawData: json.RawMessage(raw)}
}

func TestTUIRendersTruncatedErrorAndUnevaluatedStates(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	screen := tcell.NewSimulationScreen("UTF-8")
	screen.SetSize(100, 30)
	runtime := newTUIRuntime(ctx, tuiRenderDispatcher{}, nil, daemon.WorktreeScope{}, nil, nil, nil, screen)
	keys := trackKeys(runtime, screen)
	done := make(chan error, 1)
	go func() { done <- runtime.run() }()
	eventuallyOnUI(t, runtime, time.Second, func() bool {
		return [3]panelStatus{runtime.state.Panels[viewTickets].Status, runtime.state.Panels[viewFindings].Status, runtime.state.Panels[viewInsights].Status} ==
			[3]panelStatus{panelReady, panelError, panelReady}
	}, "canned panel states did not settle")

	if text := simulationTextOnUI(t, runtime, screen); !strings.Contains(text, "TRUNCATED") || !strings.Contains(text, "planned=51") {
		t.Fatalf("truncated state not rendered:\n%s", text)
	}
	keys.press(t, runeKey('4'))
	if text := waitForSimulationText(t, runtime, screen, "ERROR E_FINDING_INVALID"); !strings.Contains(text, "ERROR E_FINDING_INVALID") {
		t.Fatalf("error state not rendered:\n%s", text)
	}
	keys.press(t, runeKey('5'))
	if text := waitForSimulationText(t, runtime, screen, "UNEVALUATED"); !strings.Contains(text, "no reports") {
		t.Fatalf("unevaluated reason not rendered:\n%s", text)
	}
	screen.InjectKey(tcell.KeyRune, 'q', tcell.ModNone)
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-testdeadline.After(2 * time.Second):
		t.Fatal("render smoke did not quit")
	}
}

// simulationText must be called on the tview UI goroutine. SimulationScreen's
// GetContents result aliases its backing cells after the internal lock drops.
func simulationText(screen tcell.SimulationScreen) string {
	cells, width, _ := screen.GetContents()
	var out strings.Builder
	for index, cell := range cells {
		if len(cell.Runes) == 0 {
			out.WriteByte(' ')
		} else {
			out.WriteRune(cell.Runes[0])
		}
		if width > 0 && (index+1)%width == 0 {
			out.WriteByte('\n')
		}
	}
	return out.String()
}

// verifies: Space at the confirmation modal's DEFAULT (Cancel) focus does not
// dispatch — it activates Cancel and closes the modal (Sol confirm P1: the
// Space/mouse activation path was previously unregressed). Any affirmative from
// the default focus is safe; only deliberate navigation to Confirm can dispatch.
func TestTUIConfirmationSpaceAtDefaultFocusDoesNotDispatch(t *testing.T) {
	dispatcher := &tuiMutationDispatcher{}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	screen := tcell.NewSimulationScreen("UTF-8")
	screen.SetSize(110, 36)
	runtime := newTUIRuntime(ctx, dispatcher, nil, daemon.WorktreeScope{}, nil, nil, nil, screen)
	keys := trackKeys(runtime, screen)
	done := make(chan error, 1)
	go func() { done <- runtime.run() }()
	waitForSimulationText(t, runtime, screen, "1:Tickets")

	entry := paletteEntry{
		Verb: "set", Summary: "set a ticket field", Safety: core.SafetyMutate,
		Args: []paletteArg{{Spec: core.ArgSpec{Name: "selector", Kind: core.ArgKindString}, Required: true}},
	}
	openPaletteEntryOnUI(t, runtime, entry)
	keys.typeText("AIRA-1")
	keys.press(t, specialKey(tcell.KeyTab))
	waitForButtonFocusOnUI(t, runtime, runtime.paletteSubmitButton)
	keys.press(t, specialKey(tcell.KeyEnter)) // form submit -> confirmation
	waitForConfirmationOnUI(t, runtime)
	waitForSimulationText(t, runtime, screen, "Confirm mutation")
	assertConfirmCancelFocused(t, runtime)

	// Space at the default (Cancel) focus must NOT dispatch. A tview Button activates
	// on Enter only, so Space is inert here and the modal simply stays open; closing it
	// would be just as safe, so only the absence of a dispatch is asserted. (The screen
	// text the old test waited on, "1:Tickets", was on screen before and after the key,
	// so it proved nothing.)
	keys.press(t, runeKey(' '))
	assertNotDispatched(t, runtime, dispatcher, "set", confirmationEither, "Space at default focus")

	cancel()
	select {
	case <-done:
	case <-testdeadline.After(5 * time.Second):
		t.Fatal("runtime did not exit after cancel")
	}
}

// covers: the real tview path opens the disjoint execute launcher, parses the
// one-field argv, confirms once, suspends, calls only the fake execute dispatcher,
// resumes, and returns to a live dashboard.
func TestTUIExecuteLauncherSuspendsDispatchesAndResumes(t *testing.T) {
	dashboard := &tuiSmokeDispatcher{started: make(chan struct{})}
	execute := &executeRouteRecorder{response: core.Response{OK: true, Code: "OK"}}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	screen := tcell.NewSimulationScreen("UTF-8")
	screen.SetSize(110, 36)
	var stdout, stderr bytes.Buffer
	runtime := newTUIRuntime(ctx, dashboard, execute, daemon.WorktreeScope{}, strings.NewReader("\n"), &stdout, &stderr, screen)
	done := make(chan error, 1)
	go func() { done <- runtime.run() }()
	waitForSimulationText(t, runtime, screen, "1:Tickets")

	screen.InjectKey(tcell.KeyRune, 'x', tcell.ModNone)
	text := waitForSimulationText(t, runtime, screen, "Foreground execute")
	if !strings.Contains(text, "subprocess in an owned scope") || strings.Contains(text, "run-kill") {
		t.Fatalf("execute launcher contents:\n%s", text)
	}
	screen.InjectKey(tcell.KeyEnter, 0, tcell.ModNone) // run
	waitForSimulationText(t, runtime, screen, "(include --)")
	for _, ch := range "-- true" {
		screen.InjectKey(tcell.KeyRune, ch, tcell.ModNone)
	}
	screen.InjectKey(tcell.KeyTab, 0, tcell.ModNone)
	screen.InjectKey(tcell.KeyEnter, 0, tcell.ModNone)
	waitForSimulationText(t, runtime, screen, "Confirm foreground action")
	screen.InjectKey(tcell.KeyTab, 0, tcell.ModNone) // Cancel -> Confirm.
	screen.InjectKey(tcell.KeyEnter, 0, tcell.ModNone)

	testdeadline.Eventually(t, 3*time.Second, func() bool {
		dispatch, palette, route := execute.counts()
		return dispatch == 1 && palette == 0 && route == core.RouteClient && !runtime.executeRunning.Load()
	}, "the foreground execute never dispatched exactly once through the client route")
	dispatch, palette, route := execute.counts()
	if dispatch != 1 || palette != 0 || route != core.RouteClient {
		t.Fatalf("execute routes dispatch=%d palette=%d route=%v stdout=%q stderr=%q", dispatch, palette, route, stdout.String(), stderr.String())
	}
	if runtime.executeRunning.Load() {
		t.Fatal("execute atomic guard remained set after resume")
	}
	screen.InjectKey(tcell.KeyRune, 'q', tcell.ModNone)
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-testdeadline.After(3 * time.Second):
		t.Fatal("execute smoke TUI did not quit")
	}
}

// covers: the real tview path submits detach through the executor, releases the
// supervisor before rendering acceptance, and returns directly to a live
// dashboard without suspending or touching foreground terminal streams.
func TestTUIDetachedExecuteRendersAcceptedWithoutSuspend(t *testing.T) {
	dashboard := &tuiSmokeDispatcher{started: make(chan struct{})}
	var acknowledged atomic.Bool
	execute := &executeRouteRecorder{response: core.Response{
		OK: true, Code: "OK", RawData: []byte(`{"id":"RUN-61","status":"starting"}`),
		AfterWrite: func(delivered bool) error {
			acknowledged.Store(delivered)
			return nil
		},
	}}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	screen := tcell.NewSimulationScreen("UTF-8")
	screen.SetSize(130, 38)
	var stdout, stderr bytes.Buffer
	runtime := newTUIRuntime(ctx, dashboard, execute, daemon.WorktreeScope{}, strings.NewReader("\n"), &stdout, &stderr, screen)
	var suspended atomic.Bool
	runtime.suspend = func(func()) bool {
		suspended.Store(true)
		return false
	}
	done := make(chan error, 1)
	go func() { done <- runtime.run() }()
	waitForSimulationText(t, runtime, screen, "1:Tickets")

	screen.InjectKey(tcell.KeyRune, 'x', tcell.ModNone)
	waitForSimulationText(t, runtime, screen, "Foreground execute")
	screen.InjectKey(tcell.KeyEnter, 0, tcell.ModNone) // run
	waitForSimulationText(t, runtime, screen, "(include --)")
	for _, ch := range "--detach -- true" {
		screen.InjectKey(tcell.KeyRune, ch, tcell.ModNone)
	}
	screen.InjectKey(tcell.KeyTab, 0, tcell.ModNone)
	screen.InjectKey(tcell.KeyEnter, 0, tcell.ModNone)
	waitForSimulationText(t, runtime, screen, "Confirm detached action")
	screen.InjectKey(tcell.KeyTab, 0, tcell.ModNone) // Cancel -> Confirm.
	screen.InjectKey(tcell.KeyEnter, 0, tcell.ModNone)
	text := waitForSimulationText(t, runtime, screen, "detached run accepted: RUN-61")
	if !strings.Contains(text, "supervisor released;") || !strings.Contains(text, "admission + outcome") || strings.Contains(text, "child started") {
		t.Fatalf("detached status was dishonest:\n%s", text)
	}
	if !acknowledged.Load() || suspended.Load() || runtime.executeRunning.Load() || runtime.state.ExecuteRunning {
		t.Fatalf("detached seams ack=%v suspended=%v atomic=%v state=%v", acknowledged.Load(), suspended.Load(), runtime.executeRunning.Load(), runtime.state.ExecuteRunning)
	}
	if stdout.Len() != 0 || stderr.Len() != 0 {
		t.Fatalf("detached path wrote terminal stdout=%q stderr=%q", stdout.String(), stderr.String())
	}
	dispatch, palette, route := execute.counts()
	if dispatch != 1 || palette != 0 || route != core.RouteClient {
		t.Fatalf("detached routes dispatch=%d palette=%d route=%v", dispatch, palette, route)
	}

	screen.InjectKey(tcell.KeyRune, 'q', tcell.ModNone)
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-testdeadline.After(3 * time.Second):
		t.Fatal("detached execute smoke TUI did not quit")
	}
}

func TestTUIExecuteCapabilityAbsentIsVisible(t *testing.T) {
	dispatcher := &tuiSmokeDispatcher{started: make(chan struct{})}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	screen := tcell.NewSimulationScreen("UTF-8")
	screen.SetSize(110, 36)
	runtime := newTUIRuntime(ctx, dispatcher, nil, daemon.WorktreeScope{}, nil, nil, nil, screen)
	done := make(chan error, 1)
	go func() { done <- runtime.run() }()
	waitForSimulationText(t, runtime, screen, "1:Tickets")
	screen.InjectKey(tcell.KeyRune, 'x', tcell.ModNone)
	if text := waitForSimulationText(t, runtime, screen, "unavailable: no terminal dispatcher"); !strings.Contains(text, "unavailable: no terminal dispatcher") {
		t.Fatalf("capability absence was silent:\n%s", text)
	}
	screen.InjectKey(tcell.KeyEscape, 0, tcell.ModNone)
	screen.InjectKey(tcell.KeyRune, 'q', tcell.ModNone)
	select {
	case <-done:
	case <-testdeadline.After(3 * time.Second):
		t.Fatal("capability smoke TUI did not quit")
	}
}

func TestTUILeaseAndReadyTablesPublishSelection(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	runtime := newTUIRuntime(ctx, &tuiSmokeDispatcher{started: make(chan struct{})}, nil, daemon.WorktreeScope{}, nil, nil, nil, nil)
	for _, test := range []struct {
		view tuiView
		id   string
	}{{view: viewLeases, id: "AIRA-lease"}, {view: viewReady, id: "AIRA-ready"}} {
		panel := runtime.state.Panels[test.view]
		panel.Model = panelModel{Headers: []string{"ID"}, Rows: []tableRow{{ID: test.id, Cells: []string{test.id}}}}
		runtime.state.Panels[test.view] = panel
		runtime.renderTable(test.view, panel.Model, panel)
		runtime.tables[test.view].Select(1, 0)
		if got := runtime.state.Panels[test.view].SelectedID; got != test.id {
			t.Fatalf("%s selected id=%q want=%q", test.view, got, test.id)
		}
	}
	cancel()
	runtime.executor.wait()
}
