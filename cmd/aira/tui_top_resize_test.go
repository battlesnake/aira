package main

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"aira/internal/core"
	"aira/internal/daemon"
	"aira/internal/runner"
)

// AIRA-312. `aira top`'s `e` key lowers a RUNNING job's admission reserve: the
// new size is typed into the inline mini-form, then a confirmation names the
// job, its owner, current reserve and live usage, the proposed size, what a
// resize does not do, and whose owner check confirming overrides.
//
// verifies: AIRA-312

const topResizeJobID = "CONFINE-alpha-101-aa@session-a"

// topResizeState is `aira top` with two live rows (alpha, owned by session-a,
// reserving 8 GiB and using 1 GiB; bravo) and one queued row, the caller
// identity set to caller.
func topResizeState(t *testing.T, caller string) tuiState {
	t.Helper()
	frame := topTestFrame()
	frame.Queued = 1
	frame.FreezePhase = "idle"
	listing := topTestListing(frame,
		topTestRecord(topResizeJobID, "alpha", 8*gib, gib),
		topTestRecord("CONFINE-bravo-102-bb@session-b", "bravo", 3*gib, gib),
	)
	listing.Queued = []runner.ConfineRecord{topQueuedTestRecord("CONFINE-zulu-201-cc@session-q", "zulu", 4*gib)}
	model, _ := topViewModel(topTick{}, listing)
	state := newTUIState(8)
	state.Active = viewTop
	state.ConfineOwner = caller
	panel := state.Panels[viewTop]
	panel.Status = panelReady
	panel.Model = model
	state.Panels[viewTop] = panel
	return state
}

// topResizeOpen highlights the live alpha row and presses `e`.
func topResizeOpen(t *testing.T, state tuiState) tuiState {
	t.Helper()
	state = onTopHighlight(state, 1)
	if got := state.Panels[viewTop].SelectedID; got != topResizeJobID {
		t.Fatalf("test premise: selected %q, want %q", got, topResizeJobID)
	}
	state, commands := onTUIKey(state, 'e', core.New(nil).DispatchDescriptors())
	if len(commands) != 0 || state.InlineError != "" || state.PaletteConfirm != nil {
		t.Fatalf("e: commands=%v error=%q confirm=%+v, want only the size form", commands, state.InlineError, state.PaletteConfirm)
	}
	pending := state.InlineAction
	if pending == nil || pending.Stage != inlineStageMiniForm || pending.Action.Verb != "confine-resize" || !state.PaletteOpen ||
		len(pending.FormArgs) != 1 || pending.FormArgs[0] != "memory_reserve" || pending.TargetID != topResizeJobID {
		t.Fatalf("e opened %+v (palette open %v), want the confine-resize size form for %s", pending, state.PaletteOpen, topResizeJobID)
	}
	return state
}

// verifies: AIRA-312 test 7 — `e` on a running row opens the size form, the
// submit builds the confirmation with the exact request, and confirming
// dispatches exactly that request once.
func TestTopResizeKeyOnARunningRowConfirmsThenDispatchesTheExactRequest(t *testing.T) {
	descriptors := core.New(nil).DispatchDescriptors()
	state := topResizeOpen(t, topResizeState(t, "session-a"))
	if title := inlineMiniFormTitle(state.InlineAction); !strings.Contains(title, "alpha") || !strings.Contains(title, "current reserve 8192M") {
		t.Fatalf("size form title %q does not name the job and its reserve", title)
	}
	state, commands := onInlineMiniFormSubmit(state, map[string]string{"memory_reserve": " 2G "}, descriptors)
	if len(commands) != 0 || state.InlineError != "" {
		t.Fatalf("submit commands=%v error=%q, want the confirmation and no dispatch yet", commands, state.InlineError)
	}
	confirm := state.PaletteConfirm
	if confirm == nil || confirm.Verb != "confine-resize" || confirm.Safety != core.SafetyExecute || confirm.Destructive || !state.PaletteOpen {
		t.Fatalf("confirm=%+v, want the confine-resize confirmation", confirm)
	}
	want := map[string]any{"selector": topResizeJobID, "memory_reserve": "2G", "steal": false, "owner": "session-a", "slice": runner.ResolveConfineSlice("")}
	if confirm.Request.Verb != "confine-resize" || len(confirm.Request.Args) != len(want) {
		t.Fatalf("request=%+v, want exactly %v", confirm.Request, want)
	}
	for name, value := range want {
		if confirm.Request.Args[name] != value {
			t.Fatalf("request arg %s=%v, want %v (request %+v)", name, confirm.Request.Args[name], value, confirm.Request)
		}
	}
	if !paletteConfirmEnabled(confirm) {
		t.Fatal("the resize confirmation cannot be confirmed")
	}
	state, commands = onPaletteConfirm(state)
	if len(commands) != 1 || commands[0].Kind != cmdPalette || commands[0].Palette == nil || commands[0].Palette.Verb != "confine-resize" ||
		commands[0].Palette.Args["selector"] != topResizeJobID || commands[0].Palette.Args["memory_reserve"] != "2G" {
		t.Fatalf("confirm commands=%+v, want one confine-resize dispatch", commands)
	}
	if !state.PaletteDispatching {
		t.Fatal("confirming did not mark the dispatch in flight")
	}
}

// verifies: AIRA-312 test 7 — the confirmation states the job, its owner,
// current reserve and live usage, the proposed size, and every honesty caveat.
func TestTopResizeConfirmationStatesTheJobAndTheCaveats(t *testing.T) {
	state := topResizeOpen(t, topResizeState(t, "session-a"))
	state, _ = onInlineMiniFormSubmit(state, map[string]string{"memory_reserve": "2G"}, core.New(nil).DispatchDescriptors())
	if state.PaletteConfirm == nil {
		t.Fatalf("no confirmation (error %q)", state.InlineError)
	}
	summary := state.PaletteConfirm.Summary
	for _, want := range []string{
		"alpha", "owner session-a", "current reserve 8192M", "live usage 1024M", "to 2G (2147483648 bytes)",
		"memory.max cap is unchanged", "OOM-steering budget drops", "any re-declare", "restores the original reserve",
		"below the job's live memory use", "It is your job (session-a)",
	} {
		if !strings.Contains(summary, want) {
			t.Fatalf("confirmation %q does not say %q", summary, want)
		}
	}
}

// verifies: AIRA-312 test 7 — the steal wording and the steal flag follow the
// one rule topFrontConfirm uses: steal=false only for the caller's own job under
// an attested identity; another session's job, an inferred caller and an
// unknown caller all send steal=true, and the page says whose check it overrides.
func TestTopResizeStealWording(t *testing.T) {
	for _, test := range []struct {
		name, caller string
		wantOwner    string
		wantSteal    bool
		wantText     string
	}{
		{name: "own job", caller: "session-a", wantOwner: "session-a", wantSteal: false, wantText: "It is your job (session-a)"},
		{name: "another session's job", caller: "session-z", wantOwner: "session-z", wantSteal: true,
			wantText: "You are session-z; confirming overrides the owner check (--steal) for session-a's job, and the resize is logged as yours"},
		{name: "inferred caller", caller: "@cwd-session-a", wantOwner: "@cwd-session-a", wantSteal: true, wantText: "overrides the owner check (--steal) for session-a's job"},
		{name: "unknown caller", caller: "", wantOwner: runner.ConfineUnknownOwner, wantSteal: true, wantText: "aira top has no owner identity"},
	} {
		t.Run(test.name, func(t *testing.T) {
			state := topResizeOpen(t, topResizeState(t, test.caller))
			state, _ = onInlineMiniFormSubmit(state, map[string]string{"memory_reserve": "2G"}, core.New(nil).DispatchDescriptors())
			confirm := state.PaletteConfirm
			if confirm == nil {
				t.Fatalf("no confirmation (error %q)", state.InlineError)
			}
			if confirm.Request.Args["owner"] != test.wantOwner || confirm.Request.Args["steal"] != test.wantSteal {
				t.Fatalf("owner=%v steal=%v, want %s/%v", confirm.Request.Args["owner"], confirm.Request.Args["steal"], test.wantOwner, test.wantSteal)
			}
			if !strings.Contains(confirm.Summary, test.wantText) {
				t.Fatalf("confirmation %q does not say %q", confirm.Summary, test.wantText)
			}
			if !test.wantSteal && strings.Contains(confirm.Summary, "--steal") {
				t.Fatalf("confirmation %q mentions --steal for the caller's own job", confirm.Summary)
			}
		})
	}
}

// verifies: AIRA-312 test 7 — a size the shared parser refuses (empty, zero,
// malformed) never builds a confirmation: the error is shown and nothing is
// dispatched.
func TestTopResizeRefusesABadSizeBeforeAnyConfirmation(t *testing.T) {
	for _, raw := range []string{"", "  ", "0", "1023K", "lots"} {
		state := topResizeOpen(t, topResizeState(t, "session-a"))
		state, commands := onInlineMiniFormSubmit(state, map[string]string{"memory_reserve": raw}, core.New(nil).DispatchDescriptors())
		if state.PaletteConfirm != nil || len(commands) != 0 || !strings.HasPrefix(state.InlineError, "E_CONFINE_ARGUMENT_INVALID:") {
			t.Fatalf("size %q: confirm=%+v commands=%v error=%q, want a visible refusal", raw, state.PaletteConfirm, commands, state.InlineError)
		}
	}
}

// verifies: AIRA-312 test 7 — row-kind default-deny both ways: `e` on a QUEUED
// row is refused visibly (a waiting job holds no reserve to lower), and `f` (a
// queued-row action) is still refused on a live row.
func TestTopResizeRowKindDefaultDeny(t *testing.T) {
	descriptors := core.New(nil).DispatchDescriptors()
	state := topResizeState(t, "session-a")

	queued := onTopHighlight(state, 3)
	if !strings.HasPrefix(queued.Panels[viewTop].SelectedID, "CONFINE-zulu") {
		t.Fatalf("test premise: selected %q, want the queued row", queued.Panels[viewTop].SelectedID)
	}
	refused, commands := onTUIKey(queued, 'e', descriptors)
	if refused.PaletteConfirm != nil || refused.InlineAction != nil || len(commands) != 0 || !strings.HasPrefix(refused.InlineError, "E_TUI_INLINE_ROW_KIND:") {
		t.Fatalf("e on a queued row: confirm=%+v inline=%+v commands=%v error=%q, want a visible row-kind refusal", refused.PaletteConfirm, refused.InlineAction, commands, refused.InlineError)
	}

	live := onTopHighlight(state, 1)
	refused, commands = onTUIKey(live, 'f', descriptors)
	if refused.PaletteConfirm != nil || len(commands) != 0 || !strings.HasPrefix(refused.InlineError, "E_TUI_INLINE_ROW_KIND:") {
		t.Fatalf("f on a live row: confirm=%+v commands=%v error=%q, want refused", refused.PaletteConfirm, commands, refused.InlineError)
	}

	// `e` is bound only on the top view, and no other view gains it.
	for _, view := range []tuiView{viewTickets, viewFindings, viewLeases, viewReady} {
		if action, ok := inlineActionFor(view, 'e'); ok {
			t.Fatalf("view %v binds e to %+v", view, action)
		}
	}
}

// verifies: AIRA-312 — the description is captured at the keypress: a refresh
// between `e` and the size submit (the table rebuilds every tick) does not
// change the figures the confirmation names, and the request still targets the
// job's scope id.
func TestTopResizeDescriptionIsCapturedAtTheKeypress(t *testing.T) {
	state := topResizeOpen(t, topResizeState(t, "session-a"))
	panel := state.Panels[viewTop]
	panel.Model = panelModel{}
	state.Panels[viewTop] = panel
	state, _ = onInlineMiniFormSubmit(state, map[string]string{"memory_reserve": "2G"}, core.New(nil).DispatchDescriptors())
	if state.PaletteConfirm == nil || !strings.Contains(state.PaletteConfirm.Summary, "current reserve 8192M") || state.PaletteConfirm.Request.Args["selector"] != topResizeJobID {
		t.Fatalf("confirm=%+v, want the keypress-time description and the original scope id", state.PaletteConfirm)
	}
}

// verifies: AIRA-312 — the `aira top` resize reaches the daemon through the
// palette's management arm (confine-resize is client-routed, which the palette
// otherwise refuses), as one project-less exchange; a daemon refusal is shown
// as a rejection carrying its code, without the old-daemon hint.
func TestTopResizeReachesTheDaemonThroughThePalette(t *testing.T) {
	state := topResizeOpen(t, topResizeState(t, "session-a"))
	state, _ = onInlineMiniFormSubmit(state, map[string]string{"memory_reserve": "2G"}, core.New(nil).DispatchDescriptors())
	if state.PaletteConfirm == nil {
		t.Fatal("no confirmation")
	}
	request := state.PaletteConfirm.Request

	var sent []daemon.RequestFrame
	reply := daemon.ResponseFrame{OK: true, Code: "OK"}
	d := &daemonDispatcher{}
	d.exchange = func(_ context.Context, _ string, frame daemon.RequestFrame) (daemon.ResponseFrame, error) {
		sent = append(sent, frame)
		return reply, nil
	}
	data, _ := json.Marshal(runner.ConfineResizeResult{Status: "resized", ScopeID: topResizeJobID, OldReserveBytes: 8 * gib, NewReserveBytes: 2 * gib})
	reply.Data = data
	result := executePaletteRequest(context.Background(), d, daemon.WorktreeScope{}, request)
	if len(sent) != 1 {
		t.Fatalf("resize sent %d frames (result %+v), want exactly one exchange", len(sent), result)
	}
	args := sent[0].Request.Args
	if sent[0].Request.Verb != "confine-resize" || args["selector"] != topResizeJobID || args["memory_reserve"] != "2G" || sent[0].Scope.Root != "" {
		t.Fatalf("frame=%+v, want a project-less confine-resize of %s to 2G", sent[0], topResizeJobID)
	}
	if result.Outcome != paletteApplied {
		t.Fatalf("outcome=%v text=%q, want applied", result.Outcome, result.Text)
	}

	reply = daemon.ResponseFrame{OK: false, Code: runner.CodeConfineResizeBelowUsage, Error: runner.CodeConfineResizeBelowUsage + ": using more"}
	result = executePaletteRequest(context.Background(), d, daemon.WorktreeScope{}, request)
	if result.Outcome != paletteRejected || !strings.Contains(result.Text, runner.CodeConfineResizeBelowUsage) || strings.Contains(result.Text, "predate") {
		t.Fatalf("refusal outcome=%v text=%q, want rejected with %s and no old-daemon hint", result.Outcome, result.Text, runner.CodeConfineResizeBelowUsage)
	}
}
