package main

import (
	"context"
	"testing"
	"time"

	"aira/internal/daemon"
	"aira/internal/testdeadline"

	"github.com/gdamore/tcell/v2"
)

// verifies: AIRA-253 -- a cancellation that lands BEFORE app.Run() has a screen
// still ends Run() deterministically. The coordinator used to call app.Stop()
// from its own goroutine; tview's Stop() is a no-op before Run() (and discards
// an injected screen), so Run() then built a NEW screen that nothing would stop.
//
// The TERM is made unresolvable so the failure is observable without a pty: if
// the injected screen is discarded, Run() tries to open a real terminal and
// returns the terminfo error instead of nil.
func TestTUICancelBeforeRunStillExits(t *testing.T) {
	t.Setenv("TERM", "aira-test-no-such-terminal")
	if probe, err := tcell.NewScreen(); err == nil {
		_ = probe
		t.Skip("unevaluated: tcell.NewScreen() succeeds with an unresolvable TERM here, so a discarded injected screen would not be observable; AIRA-253 is NOT covered on this machine")
	}
	dispatcher := &tuiSmokeDispatcher{started: make(chan struct{})}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	screen := tcell.NewSimulationScreen("UTF-8")
	screen.SetSize(100, 30)
	runtime := newTUIRuntime(ctx, dispatcher, nil, daemon.WorktreeScope{}, nil, nil, nil, screen)
	// Cancel, and wait for the coordinator to run to completion, BEFORE Run().
	runtime.beforeAppRun = func() {
		cancel()
		select {
		case <-runtime.coordDone:
		case <-testdeadline.After(5 * time.Second):
			t.Error("shutdown coordinator did not finish before Run()")
		}
	}
	done := make(chan error, 1)
	go func() { done <- runtime.run() }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run() after an early cancel returned %v; the injected screen was discarded and Run() tried to open a real terminal", err)
		}
	case <-testdeadline.After(10 * time.Second):
		t.Fatal("Run() never returned after a cancel that landed before Run(): the live screen has nothing to stop it")
	}
}
