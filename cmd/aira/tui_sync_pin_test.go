package main

// Pins for the synchronisation helpers in tui_sync_test.go (AIRA-204). A helper that
// silently stopped synchronising would turn every TUI test back into a flake, so each
// guarantee has a test that goes red against the wrong implementation.

import (
	"context"
	"fmt"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"aira/internal/daemon"
	"aira/internal/testdeadline"

	"github.com/gdamore/tcell/v2"
)

// TestTUIKeyBarrierReturnsOnlyAfterEveryEarlierHandlerReturned holds a key HANDLER,
// not the loop. A raw key X is injected outside press and its handler is held; then
// press(Y) is called. press must not return until X's handler AND Y's have returned.
//
// A press that only injects returns while X is held; one that signals on the first
// key it sees returns at X's capture, before X's handler. Both leave doneAtReturn
// false. The hold is bounded (a labelled negative window): a mutated press can only
// false-pass if its goroutine is descheduled for the whole hold, never false-fail.
func TestTUIKeyBarrierReturnsOnlyAfterEveryEarlierHandlerReturned(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	screen := tcell.NewSimulationScreen("UTF-8")
	screen.SetSize(100, 30)
	rt := newTUIRuntime(ctx, &tuiSmokeDispatcher{started: make(chan struct{})}, nil, daemon.WorktreeScope{}, nil, nil, nil, screen)
	drawn := make(chan struct{}, 1)
	rt.app.SetAfterDrawFunc(func(tcell.Screen) {
		select {
		case drawn <- struct{}{}:
		default:
		}
	})

	entered := make(chan struct{})
	pressReturned := make(chan struct{})
	var handledX, handledY atomic.Bool
	rt.app.SetInputCapture(func(event *tcell.EventKey) *tcell.EventKey {
		switch event.Rune() {
		case 'X':
			close(entered)
			select {
			case <-pressReturned:
			case <-time.After(200 * time.Millisecond): // negative window: the hold
			}
			handledX.Store(true)
		case 'Y':
			time.Sleep(50 * time.Millisecond) // a held handler: a barrier that returns at Y's capture is caught
			handledY.Store(true)
		}
		return nil
	})
	keys := trackKeys(rt, screen)
	done := make(chan error, 1)
	go func() { done <- rt.run() }()
	select {
	case <-drawn:
	case <-testdeadline.After(time.Second):
		t.Fatal("initial draw timed out")
	}

	screen.InjectKey(tcell.KeyRune, 'X', tcell.ModNone) // raw: not through press
	select {
	case <-entered:
	case <-testdeadline.After(time.Second):
		t.Fatal("X's handler never started")
	}
	var doneAtReturn atomic.Bool
	go func() {
		keys.press(t, runeKey('Y'))
		doneAtReturn.Store(handledX.Load() && handledY.Load())
		close(pressReturned)
	}()
	select {
	case <-pressReturned:
	case <-testdeadline.After(2 * time.Second):
		t.Fatal("press never returned")
	}
	if !doneAtReturn.Load() {
		t.Fatalf("press returned before every earlier handler returned: X=%v Y=%v", handledX.Load(), handledY.Load())
	}

	screen.InjectKey(tcell.KeyRune, 'q', tcell.ModNone)
	rt.app.Stop()
	<-done
}

// TestTUIUIReadsTolerateASlowLoop stalls the loop for 300 ms (a fault injection)
// and reads state through eventuallyOnUI. A read helper with a short inner fatal
// (the old 100 ms shape) fails here; a generous backstop just waits. Like the stall
// itself the RED can only false-pass (if the reader is descheduled for 200 ms).
func TestTUIUIReadsTolerateASlowLoop(t *testing.T) {
	dispatcher := &tuiSmokeDispatcher{blockList: true, started: make(chan struct{})}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	screen := tcell.NewSimulationScreen("UTF-8")
	screen.SetSize(100, 30)
	rt := newTUIRuntime(ctx, dispatcher, nil, daemon.WorktreeScope{}, nil, nil, nil, screen)
	drawn := make(chan struct{}, 1)
	rt.app.SetAfterDrawFunc(func(tcell.Screen) {
		select {
		case drawn <- struct{}{}:
		default:
		}
	})
	done := make(chan error, 1)
	go func() { done <- rt.run() }()
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

	stalled := make(chan struct{})
	go rt.app.QueueUpdate(func() {
		close(stalled)
		time.Sleep(300 * time.Millisecond) // fault injection: a stalled loop
	})
	<-stalled
	var panel panelState
	// Raw, and read straight away: the read is queued BEHIND the stall, which is
	// exactly what a short inner timeout cannot survive.
	screen.InjectKey(tcell.KeyRune, 'r', tcell.ModNone)
	eventuallyOnUI(t, rt, time.Second, func() bool {
		panel = rt.state.Panels[viewTickets]
		return panel.Dirty
	}, "the refresh key never marked the tickets panel dirty")
	if !panel.InFlight || panel.Generation != 1 {
		t.Fatalf("inline refresh state=%#v", panel)
	}
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

// recordingTB captures a Fatal so a helper's failure path can be asserted.
type recordingTB struct {
	testing.TB
	message string
}

func (r *recordingTB) Helper() {}
func (r *recordingTB) Fatal(args ...any) {
	r.message = fmt.Sprint(args...)
	runtime.Goexit()
}
func (r *recordingTB) Fatalf(format string, args ...any) {
	r.message = fmt.Sprintf(format, args...)
	runtime.Goexit()
}

// TestWaitForSimulationTextFailsWithTheScreenWhenTheNeedleNeverAppears pins the
// silent-return bug: a wait that returned on timeout let a caller go on to
// dereference a palette confirmation form that was never built, which panicked the
// whole cmd/aira binary.
func TestWaitForSimulationTextFailsWithTheScreenWhenTheNeedleNeverAppears(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	screen := tcell.NewSimulationScreen("UTF-8")
	screen.SetSize(100, 30)
	rt := newTUIRuntime(ctx, &tuiSmokeDispatcher{started: make(chan struct{})}, nil, daemon.WorktreeScope{}, nil, nil, nil, screen)
	done := make(chan error, 1)
	go func() { done <- rt.run() }()
	waitForSimulationText(t, rt, screen, "1:Tickets")

	recorder := &recordingTB{TB: t}
	finished := make(chan struct{})
	go func() {
		defer close(finished)
		waitForSimulationTextWithin(recorder, rt, screen, "NEEDLE-THAT-IS-NEVER-DRAWN", 200*time.Millisecond)
		recorder.message = "returned without failing"
	}()
	select {
	case <-finished:
	case <-testdeadline.After(2 * time.Second):
		t.Fatal("the wait neither returned nor failed")
	}
	if recorder.message == "returned without failing" || !strings.Contains(recorder.message, "NEEDLE-THAT-IS-NEVER-DRAWN") || !strings.Contains(recorder.message, "1:Tickets") {
		t.Fatalf("a wait that never saw its needle must fail with the screen dump, got: %q", recorder.message)
	}
	screen.InjectKey(tcell.KeyRune, 'q', tcell.ModNone)
	select {
	case <-done:
	case <-testdeadline.After(3 * time.Second):
		t.Fatal("TUI did not quit")
	}
}
