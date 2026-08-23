package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"aira/internal/core"
	"aira/internal/daemon"
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
	drawn := make(chan struct{})
	var drawOnce sync.Once
	runtime.app.SetAfterDrawFunc(func(tcell.Screen) { drawOnce.Do(func() { close(drawn) }) })
	done := make(chan error, 1)
	go func() { done <- runtime.run() }()
	select {
	case <-drawn:
	case <-time.After(time.Second):
		t.Fatal("initial draw timed out")
	}
	select {
	case <-dispatcher.started:
	case <-time.After(time.Second):
		t.Fatal("ticket fetch did not start")
	}

	screen.InjectKey(tcell.KeyRune, 'r', tcell.ModNone)
	var panel panelState
	deadline := time.Now().Add(time.Second)
waitDirty:
	for time.Now().Before(deadline) {
		panelResult := make(chan panelState, 1)
		go runtime.app.QueueUpdateDraw(func() { panelResult <- runtime.state.Panels[viewTickets] })
		select {
		case panel = <-panelResult:
			if panel.Dirty {
				break waitDirty
			}
		case <-time.After(100 * time.Millisecond):
			t.Fatal("keypress deadlocked the tview loop")
		}
		time.Sleep(5 * time.Millisecond)
	}
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
	case <-time.After(3 * time.Second):
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
	case <-time.After(2 * time.Second):
		t.Fatal("pump did not process the message")
	}
	cancel() // the pump is now stuck in QueueUpdateDraw; cancel must release it.
	select {
	case <-runtime.pumpDone:
	case <-time.After(2 * time.Second):
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
	case <-time.After(time.Second):
		t.Fatal("initial draw timed out")
	}
	screen.InjectKey(tcell.KeyRune, ':', tcell.ModNone)
	deadline := time.Now().Add(time.Second)
	contents := ""
	for time.Now().Before(deadline) {
		contents = simulationTextOnUI(t, runtime, screen)
		if strings.Contains(contents, "Command palette") {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if !strings.Contains(contents, "Command palette") || !strings.Contains(contents, "find ls") {
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
	case <-time.After(2 * time.Second):
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
	done := make(chan error, 1)
	go func() { done <- runtime.run() }()
	waitForSimulationText(t, runtime, screen, "1:Tickets")

	entry := paletteEntry{
		Verb: "set", Summary: "set a ticket field", Safety: core.SafetyMutate,
		Args: []paletteArg{{Spec: core.ArgSpec{Name: "selector", Kind: core.ArgKindString}, Required: true}},
	}
	openPaletteEntryOnUI(t, runtime, entry)
	screen.InjectKey(tcell.KeyRune, 'A', tcell.ModNone)
	for _, ch := range "IRA-1" {
		screen.InjectKey(tcell.KeyRune, ch, tcell.ModNone)
	}
	screen.InjectKey(tcell.KeyTab, 0, tcell.ModNone)
	time.Sleep(20 * time.Millisecond)
	screen.InjectKey(tcell.KeyEnter, 0, tcell.ModNone) // form submit only.
	time.Sleep(20 * time.Millisecond)
	waitForSimulationText(t, runtime, screen, "Confirm mutation")
	assertConfirmCancelFocused(t, runtime)
	if got := dispatcher.count("set"); got != 0 {
		t.Fatalf("form-submit Enter dispatched %d requests", got)
	}
	screen.InjectKey(tcell.KeyRune, 'y', tcell.ModNone)
	time.Sleep(20 * time.Millisecond)
	if got := dispatcher.count("set"); got != 0 {
		t.Fatalf("single-key y dispatched %d requests", got)
	}
	screen.InjectKey(tcell.KeyEnter, 0, tcell.ModNone) // default Cancel.
	time.Sleep(20 * time.Millisecond)
	if got := dispatcher.count("set"); got != 0 {
		t.Fatalf("default-focus Enter dispatched %d requests", got)
	}

	openPaletteEntryOnUI(t, runtime, entry)
	for _, ch := range "AIRA-1" {
		screen.InjectKey(tcell.KeyRune, ch, tcell.ModNone)
	}
	screen.InjectKey(tcell.KeyTab, 0, tcell.ModNone)
	time.Sleep(20 * time.Millisecond)
	screen.InjectKey(tcell.KeyEnter, 0, tcell.ModNone)
	time.Sleep(20 * time.Millisecond)
	waitForSimulationText(t, runtime, screen, "Confirm mutation")
	assertConfirmCancelFocused(t, runtime)
	screen.InjectKey(tcell.KeyTab, 0, tcell.ModNone) // Cancel -> Confirm.
	time.Sleep(20 * time.Millisecond)
	screen.InjectKey(tcell.KeyEnter, 0, tcell.ModNone)
	for i := 0; i < 8; i++ {
		screen.InjectKey(tcell.KeyEnter, 0, tcell.ModNone)
	}
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) && dispatcher.count("set") == 0 {
		time.Sleep(5 * time.Millisecond)
	}
	if got := dispatcher.count("set"); got != 1 {
		t.Fatalf("focused Confirm with repeat dispatched %d requests, want exactly 1", got)
	}

	// A read retains the v1 one-step path and never creates confirmation state.
	openPaletteEntryOnUI(t, runtime, paletteEntry{Verb: "count", Summary: "count", Safety: core.SafetyRead})
	deadline = time.Now().Add(time.Second)
	for time.Now().Before(deadline) && dispatcher.count("count") == 0 {
		time.Sleep(5 * time.Millisecond)
	}
	confirmState := make(chan *paletteConfirm, 1)
	go runtime.app.QueueUpdate(func() { confirmState <- runtime.state.PaletteConfirm })
	confirm := <-confirmState
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
	case <-time.After(2 * time.Second):
		t.Fatal("confirmation TUI did not quit")
	}
}

func TestTUIDestructiveConfirmationRequiresExactResolvedID(t *testing.T) {
	dispatcher := &tuiMutationDispatcher{}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	screen := tcell.NewSimulationScreen("UTF-8")
	screen.SetSize(110, 36)
	runtime := newTUIRuntime(ctx, dispatcher, nil, daemon.WorktreeScope{}, nil, nil, nil, screen)
	done := make(chan error, 1)
	go func() { done <- runtime.run() }()
	waitForSimulationText(t, runtime, screen, "1:Tickets")

	entry := paletteEntryNamed(t, buildPalette(runtime.descriptors), "rant", "redact")
	openPaletteEntryOnUI(t, runtime, entry)
	for _, ch := range "RANT-7" {
		screen.InjectKey(tcell.KeyRune, ch, tcell.ModNone)
	}
	screen.InjectKey(tcell.KeyTab, 0, tcell.ModNone)
	time.Sleep(20 * time.Millisecond)
	screen.InjectKey(tcell.KeyEnter, 0, tcell.ModNone)
	time.Sleep(20 * time.Millisecond)
	waitForSimulationText(t, runtime, screen, "Type RANT-7")

	checkDisabled := func(want bool) {
		t.Helper()
		result := make(chan bool, 1)
		go runtime.app.QueueUpdate(func() { result <- runtime.paletteConfirmForm.GetButton(0).IsDisabled() })
		if got := <-result; got != want {
			t.Fatalf("Confirm disabled=%v, want %v", got, want)
		}
	}
	checkDisabled(true)
	setConfirmIDOnUI(t, runtime, "RANT-8")
	checkDisabled(true)
	screen.InjectKey(tcell.KeyRune, 'y', tcell.ModNone)
	if got := dispatcher.count("rant"); got != 0 {
		t.Fatalf("wrong destructive id dispatched %d requests", got)
	}
	setConfirmIDOnUI(t, runtime, "RANT-7")
	checkDisabled(false)

	screen.InjectKey(tcell.KeyEscape, 0, tcell.ModNone)
	time.Sleep(20 * time.Millisecond)
	cancelled := make(chan bool, 1)
	go runtime.app.QueueUpdateDraw(func() {
		cancelled <- runtime.state.PaletteConfirm == nil
		runtime.closePalette()
	})
	if !<-cancelled {
		t.Fatal("Escape did not cancel destructive confirmation")
	}
	screen.InjectKey(tcell.KeyRune, 'q', tcell.ModNone)
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
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
	done := make(chan struct{}, 1)
	go runtime.app.QueueUpdateDraw(func() {
		runtime.state.PaletteOpen = true
		runtime.openPaletteEntry(entry)
		done <- struct{}{}
	})
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("opening palette entry deadlocked")
	}
}

func assertConfirmCancelFocused(t *testing.T, runtime *tuiRuntime) {
	t.Helper()
	type focusState struct {
		buttons [2]bool
		focus   string
	}
	result := make(chan focusState, 1)
	go runtime.app.QueueUpdate(func() {
		result <- focusState{buttons: [2]bool{runtime.paletteConfirmForm.GetButton(0).HasFocus(), runtime.paletteConfirmForm.GetButton(1).HasFocus()}, focus: fmt.Sprintf("%T", runtime.app.GetFocus())}
	})
	select {
	case focused := <-result:
		if !focused.buttons[1] {
			t.Fatalf("confirmation default focus is not Cancel (Confirm=%v Cancel=%v app=%s)", focused.buttons[0], focused.buttons[1], focused.focus)
		}
	case <-time.After(time.Second):
		t.Fatal("reading confirmation focus deadlocked")
	}
}

func setConfirmIDOnUI(t *testing.T, runtime *tuiRuntime, value string) {
	t.Helper()
	done := make(chan struct{}, 1)
	go runtime.app.QueueUpdateDraw(func() {
		runtime.paletteConfirmForm.GetFormItem(0).(*tview.InputField).SetText(value)
		done <- struct{}{}
	})
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("setting confirmation id deadlocked")
	}
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
	done := make(chan error, 1)
	go func() { done <- runtime.run() }()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		states := make(chan [3]panelStatus, 1)
		go runtime.app.QueueUpdate(func() {
			states <- [3]panelStatus{runtime.state.Panels[viewTickets].Status, runtime.state.Panels[viewFindings].Status, runtime.state.Panels[viewInsights].Status}
		})
		select {
		case status := <-states:
			if status == [3]panelStatus{panelReady, panelError, panelReady} {
				goto ready
			}
		case <-time.After(100 * time.Millisecond):
			t.Fatal("renderer update queue deadlocked")
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("canned panel states did not settle")

ready:
	if text := simulationTextOnUI(t, runtime, screen); !strings.Contains(text, "TRUNCATED") || !strings.Contains(text, "planned=51") {
		t.Fatalf("truncated state not rendered:\n%s", text)
	}
	screen.InjectKey(tcell.KeyRune, '4', tcell.ModNone)
	if text := waitForSimulationText(t, runtime, screen, "ERROR E_FINDING_INVALID"); !strings.Contains(text, "ERROR E_FINDING_INVALID") {
		t.Fatalf("error state not rendered:\n%s", text)
	}
	screen.InjectKey(tcell.KeyRune, '5', tcell.ModNone)
	if text := waitForSimulationText(t, runtime, screen, "UNEVALUATED"); !strings.Contains(text, "no reports") {
		t.Fatalf("unevaluated reason not rendered:\n%s", text)
	}
	screen.InjectKey(tcell.KeyRune, 'q', tcell.ModNone)
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("render smoke did not quit")
	}
}

func waitForSimulationText(t *testing.T, runtime *tuiRuntime, screen tcell.SimulationScreen, needle string) string {
	t.Helper()
	// Generous under -race + full-suite contention on a loaded box: the tview event
	// loop can lag well past a second while heavy integration tests run in the same
	// binary. The wait is a poll, so a fast path still returns immediately.
	deadline := time.Now().Add(10 * time.Second)
	text := ""
	for time.Now().Before(deadline) {
		text = simulationTextOnUI(t, runtime, screen)
		if strings.Contains(text, needle) {
			return text
		}
		time.Sleep(5 * time.Millisecond)
	}
	return text
}

func simulationTextOnUI(t *testing.T, runtime *tuiRuntime, screen tcell.SimulationScreen) string {
	t.Helper()
	result := make(chan string, 1)
	go runtime.app.QueueUpdate(func() { result <- simulationText(screen) })
	select {
	case text := <-result:
		return text
	case <-time.After(time.Second):
		t.Fatal("screen snapshot deadlocked")
		return ""
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
	done := make(chan error, 1)
	go func() { done <- runtime.run() }()
	waitForSimulationText(t, runtime, screen, "1:Tickets")

	entry := paletteEntry{
		Verb: "set", Summary: "set a ticket field", Safety: core.SafetyMutate,
		Args: []paletteArg{{Spec: core.ArgSpec{Name: "selector", Kind: core.ArgKindString}, Required: true}},
	}
	openPaletteEntryOnUI(t, runtime, entry)
	for _, ch := range "AIRA-1" {
		screen.InjectKey(tcell.KeyRune, ch, tcell.ModNone)
	}
	screen.InjectKey(tcell.KeyTab, 0, tcell.ModNone)
	time.Sleep(20 * time.Millisecond)
	screen.InjectKey(tcell.KeyEnter, 0, tcell.ModNone) // form submit -> confirmation
	time.Sleep(20 * time.Millisecond)
	waitForSimulationText(t, runtime, screen, "Confirm mutation")
	assertConfirmCancelFocused(t, runtime)

	// Space with default (Cancel) focus must NOT dispatch (it cancels).
	screen.InjectKey(tcell.KeyRune, ' ', tcell.ModNone)
	time.Sleep(40 * time.Millisecond)
	if got := dispatcher.count("set"); got != 0 {
		t.Fatalf("Space at default focus dispatched %d requests", got)
	}
	waitForSimulationText(t, runtime, screen, "1:Tickets") // modal closed back to dashboard

	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
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
	waitForSimulationText(t, runtime, screen, "arguments (include --)")
	for _, ch := range "-- true" {
		screen.InjectKey(tcell.KeyRune, ch, tcell.ModNone)
	}
	screen.InjectKey(tcell.KeyTab, 0, tcell.ModNone)
	screen.InjectKey(tcell.KeyEnter, 0, tcell.ModNone)
	waitForSimulationText(t, runtime, screen, "Confirm foreground action")
	screen.InjectKey(tcell.KeyTab, 0, tcell.ModNone) // Cancel -> Confirm.
	screen.InjectKey(tcell.KeyEnter, 0, tcell.ModNone)

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		dispatch, palette, route := execute.counts()
		if dispatch == 1 && palette == 0 && route == core.RouteClient && strings.Contains(stdout.String(), "execution: completed") {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
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
	case <-time.After(3 * time.Second):
		t.Fatal("execute smoke TUI did not quit")
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
	case <-time.After(3 * time.Second):
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
