package main

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"

	"aira/internal/core"
	"aira/internal/daemon"
	"github.com/gdamore/tcell/v2"
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
	runtime := newTUIRuntime(ctx, dispatcher, daemon.WorktreeScope{}, screen)
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
	runtime := newTUIRuntime(ctx, dispatcher, daemon.WorktreeScope{}, screen)
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
	runtime := newTUIRuntime(ctx, dispatcher, daemon.WorktreeScope{}, screen)
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
		if strings.Contains(contents, "Read-only palette") {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if !strings.Contains(contents, "Read-only palette") || !strings.Contains(contents, "find ls") {
		t.Fatalf("palette not rendered:\n%s", contents)
	}
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
	runtime := newTUIRuntime(ctx, tuiRenderDispatcher{}, daemon.WorktreeScope{}, screen)
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
	deadline := time.Now().Add(time.Second)
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
