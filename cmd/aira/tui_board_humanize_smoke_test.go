package main

// AIRA-257 end-to-end smoke: 't' translates the selected ticket on the real tview
// runtime (SimulationScreen), the pane and the expand overlay both show the plain
// rewrite labelled as an AI paraphrase, the executor feeds the translator the
// ticket's FETCHED title+body (proving it went through `show`, not the card
// fields), and a second 't' restores the original. The fake translator is injected
// through boardTranslatorFactory so the test never shells out or hits the network.

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"aira/internal/daemon"

	"github.com/gdamore/tcell/v2"
)

// recordingTranslator is a fake board translator: it returns a fixed plain rewrite
// and records the (id, title, body) it was called with, so the test can assert the
// executor fetched the ticket's real body rather than passing the card fields.
type recordingTranslator struct {
	mu      sync.Mutex
	gotBody string
	called  bool
}

func (r *recordingTranslator) translate(_ context.Context, id, title, body string) boardHumanizeResult {
	r.mu.Lock()
	r.gotBody, r.called = body, true
	r.mu.Unlock()
	return boardHumanizeResult{PlainTitle: "REWRITTENTITLE", PlainBody: "REWRITTENBODY"}
}

func (r *recordingTranslator) recorded() (string, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.gotBody, r.called
}

func TestBoardHumanizeToggleSmoke(t *testing.T) {
	rec := &recordingTranslator{}
	prev := boardTranslatorFactory
	boardTranslatorFactory = func() boardTranslator { return rec.translate }
	t.Cleanup(func() { boardTranslatorFactory = prev })

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	screen := tcell.NewSimulationScreen("UTF-8")
	// The factory is read at runtime construction, so it must be set BEFORE this.
	runtime := newBoardRuntime(ctx, boardInfoDispatcher{}, daemon.WorktreeScope{}, nil, nil, nil, screen)
	done := make(chan error, 1)
	go func() { done <- runtime.run() }()

	waitForSimulationText(t, runtime, screen, "AIRA-1")
	resizeBoardScreen(t, runtime, screen, 120, 56)
	waitForSimulationText(t, runtime, screen, "AIRA-1")
	screen.InjectKey(tcell.KeyRune, 'l', tcell.ModNone) // draft→planned selects AIRA-1
	// The ORIGINAL body renders before any translation.
	waitForSimulationText(t, runtime, screen, "BODYWORD")

	// 't' shows the plain-English rewrite, labelled as an AI paraphrase.
	screen.InjectKey(tcell.KeyRune, 't', tcell.ModNone)
	shown := waitForSimulationText(t, runtime, screen, "REWRITTENBODY")
	if !strings.Contains(shown, "plain-English (AI)") {
		t.Fatalf("the plain view is not labelled as an AI paraphrase:\n%s", shown)
	}
	// The executor fed the translator the ticket's FETCHED body (the loaded card
	// carries no body), proving it went through `show` — not the card fields/empties.
	if body, called := rec.recorded(); !called || !strings.Contains(body, "BODYWORD") {
		t.Fatalf("translator was not called with the fetched body: called=%v body=%q", called, body)
	}

	// Enter opens the expand overlay, which carries the same plain rewrite + label.
	screen.InjectKey(tcell.KeyEnter, 0, tcell.ModNone)
	overlay := waitForSimulationText(t, runtime, screen, "Esc to close")
	if !strings.Contains(overlay, "REWRITTENBODY") || !strings.Contains(overlay, "plain-English (AI)") {
		t.Fatalf("the expand overlay did not carry the plain rewrite:\n%s", overlay)
	}
	screen.InjectKey(tcell.KeyEscape, 0, tcell.ModNone)
	time.Sleep(20 * time.Millisecond)

	// 't' again restores the original — the owner's "original always one keystroke
	// away".
	screen.InjectKey(tcell.KeyRune, 't', tcell.ModNone)
	back := waitForSimulationText(t, runtime, screen, "BODYWORD")
	if strings.Contains(back, "REWRITTENBODY") {
		t.Fatalf("a second 't' did not restore the original:\n%s", back)
	}

	screen.InjectKey(tcell.KeyRune, 'q', tcell.ModNone)
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("humanize smoke did not quit")
	}
}
