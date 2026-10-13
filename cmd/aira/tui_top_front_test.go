package main

import (
	"context"
	"strings"
	"testing"

	"github.com/gdamore/tcell/v2"

	"aira/internal/core"
	"aira/internal/runner"
)

// AIRA-309. `aira top`'s `f` key pushes a QUEUED job to the front of the
// admission queue, behind a confirmation that names the caller's identity and
// states whose owner check confirming overrides.
//
// verifies: AIRA-309

func topFrontState(t *testing.T, pushed bool) (tuiState, runner.ConfineRecord) {
	t.Helper()
	queued := topQueuedTestRecord("CONFINE-zulu-201-cc@session-q", "zulu", 4*gib)
	queued.Pushed = pushed
	model, _ := topViewModel(topTick{}, topQueuedListing(queued))
	state := newTUIState(8)
	state.Active = viewTop
	panel := state.Panels[viewTop]
	panel.Status = panelReady
	panel.Model = model
	state.Panels[viewTop] = panel
	return state, queued
}

// verifies: AIRA-309 test 16 — a queued row is selectable at the keypress (the
// highlighted table row), `f` opens the confirmation, and confirming dispatches
// confine-front with the row's scope id.
func TestTopFrontKeyOnAQueuedRowConfirmsThenDispatches(t *testing.T) {
	descriptors := core.New(nil).DispatchDescriptors()
	state, queued := topFrontState(t, false)
	// Rows: two live, then the queued one at table row 3 (row 0 is the header).
	state = onTopHighlight(state, 3)
	if got := state.Panels[viewTop].SelectedID; got != queued.ScopeID {
		t.Fatalf("highlighted queued row selected %q, want %q", got, queued.ScopeID)
	}
	state, commands := onTUIKey(state, 'f', descriptors)
	if len(commands) != 0 || state.InlineError != "" {
		t.Fatalf("f produced commands=%v error=%q before any confirmation", commands, state.InlineError)
	}
	confirm := state.PaletteConfirm
	if confirm == nil || confirm.Verb != "confine-front" || !state.PaletteOpen {
		t.Fatalf("confirm=%+v open=%v, want the confine-front confirmation", confirm, state.PaletteOpen)
	}
	args := confirm.Request.Args
	if confirm.Request.Verb != "confine-front" || args["selector"] != queued.ScopeID || args["steal"] != true || args["owner"] != runner.ConfineUnknownOwner {
		t.Fatalf("request=%+v, want confine-front of %s with steal=true as the unknown owner", confirm.Request, queued.ScopeID)
	}
	if !paletteConfirmEnabled(confirm) {
		t.Fatal("the push confirmation cannot be confirmed")
	}
	state, commands = onPaletteConfirm(state)
	if len(commands) != 1 || commands[0].Kind != cmdPalette || commands[0].Palette == nil || commands[0].Palette.Verb != "confine-front" || commands[0].Palette.Args["selector"] != queued.ScopeID {
		t.Fatalf("confirm commands=%+v, want one confine-front dispatch", commands)
	}
	if !state.PaletteDispatching {
		t.Fatal("confirming did not mark the dispatch in flight")
	}
}

// verifies: AIRA-309 test 19 — the confirmation states that `top` has no owner
// identity and that confirming overrides the owner check for the job's owner,
// and names the job, its request and its listed position.
func TestTopFrontConfirmationStatesTheSteal(t *testing.T) {
	state, _ := topFrontState(t, false)
	state = onTopHighlight(state, 3)
	state, _ = onTUIKey(state, 'f', core.New(nil).DispatchDescriptors())
	if state.PaletteConfirm == nil {
		t.Fatal("no confirmation")
	}
	summary := state.PaletteConfirm.Summary
	for _, want := range []string{"zulu", "owner session-q", "4096M req", "listed queue position 1", "aira top has no owner identity", "--steal", "session-q's job", "holds every job behind it"} {
		if !strings.Contains(summary, want) {
			t.Fatalf("confirmation %q does not say %q", summary, want)
		}
	}
}

// verifies: AIRA-309 tests 17 and 18 — the row-kind default-deny. `f` on a LIVE
// row is refused visibly; an action that did not opt in to queued rows (every
// action but `f`) is refused on a queued row.
func TestTopFrontRowKindDefaultDeny(t *testing.T) {
	descriptors := core.New(nil).DispatchDescriptors()
	state, queued := topFrontState(t, false)

	live := onTopHighlight(state, 1)
	if live.Panels[viewTop].SelectedID == "" {
		t.Fatal("test premise: the live row has an id")
	}
	refused, commands := onTUIKey(live, 'f', descriptors)
	if refused.PaletteConfirm != nil || len(commands) != 0 || !strings.HasPrefix(refused.InlineError, "E_TUI_INLINE_ROW_KIND:") {
		t.Fatalf("f on a live row: confirm=%+v commands=%v error=%q, want a visible row-kind refusal", refused.PaletteConfirm, commands, refused.InlineError)
	}

	onQueued := onTopHighlight(state, 3)
	liveOnly := inlineAction{Key: 'k', Label: "kill", Verb: "confine-kill", Safety: core.SafetyExecute, Destructive: true, ValueSource: inlineValueFixed}
	refused, commands = onInlineActionStart(onQueued, liveOnly, descriptors)
	if refused.PaletteConfirm != nil || len(commands) != 0 || refused.InlineAction != nil || !strings.HasPrefix(refused.InlineError, "E_TUI_INLINE_ROW_KIND:") {
		t.Fatalf("live-only action on queued row %s: confirm=%+v commands=%v error=%q, want refused", queued.ScopeID, refused.PaletteConfirm, commands, refused.InlineError)
	}
}

// verifies: AIRA-309 — the highlight is re-read at the keypress: a cursor
// outside the rows clears the selection, so `f` can never act on a job the
// cursor has left.
func TestTopHighlightOutsideTheRowsClearsTheSelection(t *testing.T) {
	state, _ := topFrontState(t, false)
	state = onTopHighlight(state, 3)
	for _, row := range []int{0, 99} {
		cleared := onTopHighlight(state, row)
		if got := cleared.Panels[viewTop].SelectedID; got != "" {
			t.Fatalf("table row %d left selection %q", row, got)
		}
		after, commands := onTUIKey(cleared, 'f', core.New(nil).DispatchDescriptors())
		if after.PaletteConfirm != nil || len(commands) != 0 {
			t.Fatalf("f with no selection opened %+v", after.PaletteConfirm)
		}
	}
}

// verifies: AIRA-309 — a pushed queued row says so in its LIVE column.
func TestTopQueuedRowMarksAPushedJob(t *testing.T) {
	state, _ := topFrontState(t, true)
	if cell := state.Panels[viewTop].Model.Rows[2].Cells[3]; cell != "pushed" {
		t.Fatalf("pushed row LIVE cell=%q, want pushed", cell)
	}
	plain, _ := topFrontState(t, false)
	if cell := plain.Panels[viewTop].Model.Rows[2].Cells[3]; cell != "queued" {
		t.Fatalf("unpushed row LIVE cell=%q, want queued", cell)
	}
}

// verifies: AIRA-309 review advisory — a push from `top` is attributed: the
// request carries the caller identity resolved at startup, so the daemon's
// audit line names the pusher. steal=false only for the caller's OWN job under
// an attested identity (what the shared guard passes without --steal); every
// other push sends steal=true and the page says whose job it overrides.
func TestTopFrontPushIsAttributedToTheCaller(t *testing.T) {
	descriptors := core.New(nil).DispatchDescriptors()
	for _, test := range []struct {
		name, caller, scopeID string
		wantOwner             string
		wantSteal             bool
		wantText, notText     string
	}{
		{name: "another session's job", caller: "session-a", scopeID: "CONFINE-zulu-201-cc@session-q",
			wantOwner: "session-a", wantSteal: true, wantText: "You are session-a; confirming overrides the owner check (--steal) for session-q's job"},
		{name: "own job", caller: "session-q", scopeID: "CONFINE-zulu-201-cc@session-q",
			wantOwner: "session-q", wantSteal: false, wantText: "It is your job (session-q)", notText: "--steal"},
		{name: "inferred caller", caller: "@cwd-session-q", scopeID: "CONFINE-zulu-201-cc@session-q",
			wantOwner: "@cwd-session-q", wantSteal: true, wantText: "You are @cwd-session-q; confirming overrides the owner check (--steal)"},
		{name: "unknown caller on an unknown-owned job", caller: "", scopeID: "CONFINE-zulu-201-cc@" + runner.ConfineUnknownOwner,
			wantOwner: runner.ConfineUnknownOwner, wantSteal: true, wantText: "aira top has no owner identity"},
	} {
		t.Run(test.name, func(t *testing.T) {
			queued := topQueuedTestRecord(test.scopeID, "zulu", 4*gib)
			model, _ := topViewModel(topTick{}, topQueuedListing(queued))
			state := newTUIState(8)
			state.Active = viewTop
			state.ConfineOwner = test.caller
			panel := state.Panels[viewTop]
			panel.Status = panelReady
			panel.Model = model
			state.Panels[viewTop] = panel
			state = onTopHighlight(state, 3)
			if got := state.Panels[viewTop].SelectedID; got != test.scopeID {
				t.Fatalf("test premise: selected %q, want %q", got, test.scopeID)
			}
			state, _ = onTUIKey(state, 'f', descriptors)
			confirm := state.PaletteConfirm
			if confirm == nil {
				t.Fatal("no confirmation")
			}
			args := confirm.Request.Args
			if args["owner"] != test.wantOwner || args["steal"] != test.wantSteal {
				t.Fatalf("owner=%v steal=%v, want owner=%s steal=%v", args["owner"], args["steal"], test.wantOwner, test.wantSteal)
			}
			if !strings.Contains(confirm.Summary, test.wantText) {
				t.Fatalf("confirmation %q does not say %q", confirm.Summary, test.wantText)
			}
			if test.notText != "" && strings.Contains(confirm.Summary, test.notText) {
				t.Fatalf("confirmation %q says %q for the caller's own job", confirm.Summary, test.notText)
			}
		})
	}
}

// verifies: AIRA-309 review advisory — the runtime resolves the caller identity
// once at startup by the CLI's own chain, so `top`'s push carries it.
func TestTopRuntimeResolvesTheConfineOwner(t *testing.T) {
	t.Setenv("AIRA_CONFINE_OWNER", "session-z")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	screen := tcell.NewSimulationScreen("UTF-8")
	runtime := newTopRuntime(ctx, &tuiSmokeDispatcher{started: make(chan struct{})}, nil, nil, nil, screen)
	if runtime.state.ConfineOwner != "session-z" {
		t.Fatalf("ConfineOwner=%q, want the AIRA_CONFINE_OWNER identity", runtime.state.ConfineOwner)
	}
}
