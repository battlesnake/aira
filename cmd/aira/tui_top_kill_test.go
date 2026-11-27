package main

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/gdamore/tcell/v2"

	"aira/internal/core"
	"aira/internal/daemon"
	"aira/internal/runner"
	"aira/internal/testdeadline"
)

// AIRA-271. `aira top`: K pressed twice on the same row opens a confirmation to
// kill that job (or, for a row still waiting for admission, cancel it). The
// first K only arms; any other key disarms; the confirmation starts on Cancel;
// the request is the CLI's own `confine --kill` (confine-kill, never --steal),
// so the daemon's owner check decides; and the result says only what the reply
// carries.
//
// verifies: AIRA-271

const (
	killTestAlpha = "CONFINE-alpha-101-aa"
	killTestBravo = "CONFINE-bravo-102-bb"
	killTestZulu  = "CONFINE-zulu-201-cc@session-q"
)

// topKillState is a top view with two live jobs (alpha, bravo) and one queued
// job (zulu, owned by session-q), as caller.
func topKillState(t *testing.T, caller string) tuiState {
	t.Helper()
	model, tick := topViewModel(topTick{}, topQueuedListing(topQueuedTestRecord(killTestZulu, "zulu", 4*gib)))
	state := newTUIState(8)
	state.Active = viewTop
	state.ConfineOwner = caller
	state.Top = tick
	panel := state.Panels[viewTop]
	panel.Status = panelReady
	panel.Model = model
	state.Panels[viewTop] = panel
	return state
}

func topKillPress(t *testing.T, state tuiState, row int, keys string) (tuiState, []tuiCmd) {
	t.Helper()
	var commands, all []tuiCmd
	for _, key := range keys {
		state = onTopHighlight(state, row)
		state, commands = onTUIKey(state, key, core.New(nil).DispatchDescriptors())
		all = append(all, commands...)
	}
	return state, all
}

func assertNoKillOpened(t *testing.T, state tuiState, commands []tuiCmd, when string) {
	t.Helper()
	if state.PaletteConfirm != nil || state.InlineAction != nil {
		t.Fatalf("%s: a confirmation opened: confirm=%+v inline=%+v", when, state.PaletteConfirm, state.InlineAction)
	}
	for _, command := range commands {
		if command.Kind == cmdPalette {
			t.Fatalf("%s: a request was dispatched: %+v", when, command.Palette)
		}
	}
}

// verifies: AIRA-271 — one K arms and nothing else; the second K on the same
// row opens the confirmation for exactly that job; only confirming dispatches.
func TestTopKillTwoKsOnTheSameRowOpenTheConfirmation(t *testing.T) {
	state := topKillState(t, "session-a")
	state, commands := topKillPress(t, state, 1, "K")
	assertNoKillOpened(t, state, commands, "after one K")
	if state.PaletteOpen || state.TopKill.ScopeID != killTestAlpha || len(commands) != 0 {
		t.Fatalf("one K: open=%v armed=%+v commands=%v, want alpha armed and nothing else", state.PaletteOpen, state.TopKill, commands)
	}
	state, commands = topKillPress(t, state, 1, "K")
	if len(commands) != 0 {
		t.Fatalf("the second K dispatched %+v before any confirmation", commands)
	}
	confirm := state.PaletteConfirm
	if confirm == nil || confirm.Verb != "confine-kill" || !state.PaletteOpen || state.InlineAction == nil || state.InlineAction.Stage != inlineStageConfirm {
		t.Fatalf("KK: confirm=%+v open=%v inline=%+v, want the confine-kill confirmation", confirm, state.PaletteOpen, state.InlineAction)
	}
	if state.TopKill.ScopeID != "" {
		t.Fatalf("the arm outlived the confirmation it opened: %+v", state.TopKill)
	}
	if !paletteConfirmEnabled(confirm) {
		t.Fatal("the kill confirmation can never be confirmed")
	}
	state, commands = onPaletteConfirm(state)
	if len(commands) != 1 || commands[0].Kind != cmdPalette || commands[0].Palette.Verb != "confine-kill" || commands[0].Palette.Args["selector"] != killTestAlpha {
		t.Fatalf("confirming dispatched %+v, want one confine-kill of alpha", commands)
	}
	if !state.PaletteDispatching {
		t.Fatal("confirming did not mark the dispatch in flight")
	}
}

// verifies: AIRA-271 — any key other than K between the two presses disarms,
// so the next K only arms again; the same holds when the cursor has moved to
// another job (the arm names one scope id, re-read at each keypress).
func TestTopKillAnyOtherKeyOrRowDisarms(t *testing.T) {
	for _, other := range "kjr?1\tcqd" {
		state, commands := topKillPress(t, topKillState(t, "session-a"), 1, "K"+string(other))
		if state.TopKill.ScopeID != "" {
			t.Fatalf("K then %q left %+v armed", other, state.TopKill)
		}
		state.ShuttingDown = false
		state, more := topKillPress(t, state, 1, "K")
		assertNoKillOpened(t, state, append(commands, more...), "K "+string(other)+" K")
	}
	state, commands := topKillPress(t, topKillState(t, "session-a"), 1, "K")
	state, more := topKillPress(t, state, 2, "K")
	assertNoKillOpened(t, state, append(commands, more...), "K on alpha then K on bravo")
	if state.TopKill.ScopeID != killTestBravo {
		t.Fatalf("K on another row armed %+v, want bravo", state.TopKill)
	}
	state, _ = topKillPress(t, state, 2, "K")
	if state.PaletteConfirm == nil || state.PaletteConfirm.Request.Args["selector"] != killTestBravo {
		t.Fatalf("KK on bravo opened %+v, want bravo's confirmation", state.PaletteConfirm)
	}
	// No selection: nothing arms.
	state, commands = topKillPress(t, topKillState(t, "session-a"), 0, "KK")
	assertNoKillOpened(t, state, commands, "KK with no row selected")
	if state.TopKill.ScopeID != "" {
		t.Fatalf("KK with no selection armed %+v", state.TopKill)
	}
}

// verifies: AIRA-271 — K is not a key inside a dialog, the palette, the
// execute launcher or while a request is in flight: nothing arms or opens.
func TestTopKillIsInertInsideADialog(t *testing.T) {
	for name, set := range map[string]func(*tuiState){
		"palette open":     func(state *tuiState) { state.PaletteOpen = true },
		"dispatching":      func(state *tuiState) { state.PaletteDispatching = true },
		"execute launcher": func(state *tuiState) { state.ExecuteOpen = true },
	} {
		state := topKillState(t, "session-a")
		set(&state)
		state, commands := topKillPress(t, state, 1, "KK")
		if state.TopKill.ScopeID != "" || state.PaletteConfirm != nil || len(commands) != 0 {
			t.Fatalf("%s: KK armed=%+v confirm=%+v commands=%v", name, state.TopKill, state.PaletteConfirm, commands)
		}
	}
	// And not on any other view.
	state := topKillState(t, "session-a")
	state.Active = viewTickets
	state, commands := onTUIKey(state, 'K', nil)
	if state.TopKill.ScopeID != "" || len(commands) != 0 {
		t.Fatalf("K on the tickets view armed %+v", state.TopKill)
	}
}

// verifies: AIRA-271 — the request is always the CLI's confine-kill without
// --steal, sent as the caller, whoever owns the job: the daemon's owner check
// decides, and the page says so.
func TestTopKillNeverSendsSteal(t *testing.T) {
	for _, caller := range []string{"session-q", "session-a", "@cwd-somewhere", ""} {
		for _, row := range []int{1, 3} {
			state, _ := topKillPress(t, topKillState(t, caller), row, "KK")
			confirm := state.PaletteConfirm
			if confirm == nil {
				t.Fatalf("caller %q row %d: no confirmation", caller, row)
			}
			args := confirm.Request.Args
			wantOwner := caller
			if wantOwner == "" {
				wantOwner = runner.ConfineUnknownOwner
			}
			if confirm.Request.Verb != "confine-kill" || args["steal"] != false || args["owner"] != wantOwner || args["selector"] != state.InlineAction.TargetID {
				t.Fatalf("caller %q row %d: request=%+v, want confine-kill steal=false owner=%s", caller, row, confirm.Request, wantOwner)
			}
			if !strings.Contains(confirm.Summary, "without --steal") {
				t.Fatalf("caller %q: the page does not say the kill is sent without --steal: %q", caller, confirm.Summary)
			}
		}
	}
}

// verifies: AIRA-271 — the page names the exact job (name, owner, command head)
// as of the first K; a running job is killed, a queued one cancelled.
func TestTopKillConfirmationNamesTheJob(t *testing.T) {
	state, _ := topKillPress(t, topKillState(t, "session-a"), 3, "KK")
	summary := state.PaletteConfirm.Summary
	for _, want := range []string{"Cancel zulu (owner session-q, command go test ./...)", "still waiting for admission", "never starts"} {
		if !strings.Contains(summary, want) {
			t.Fatalf("queued confirmation %q does not say %q", summary, want)
		}
	}
	if strings.HasPrefix(summary, "Kill") {
		t.Fatalf("queued confirmation worded as a kill: %q", summary)
	}

	// The control character is inside the head that is kept, so only the
	// escaping (not the clip) can keep it off the screen.
	long := "\x1b[2J" + strings.Repeat("x", 200)
	record := topTestRecord(killTestAlpha, "alpha", 2*gib, gib)
	record.Command = &long
	model, _ := topViewModel(topTick{}, topTestListing(topTestFrame(), record))
	live := topKillState(t, "session-a")
	panel := live.Panels[viewTop]
	panel.Model = model
	live.Panels[viewTop] = panel
	live, _ = topKillPress(t, live, 1, "KK")
	summary = live.PaletteConfirm.Summary
	if !strings.HasPrefix(summary, "Kill alpha (owner session-a, command '\\x1b'[2J"+strings.Repeat("x", 51)+"…)") || strings.Contains(summary, "\x1b") {
		t.Fatalf("live confirmation %q does not name alpha with a clipped, printable command head", summary)
	}
	if !strings.Contains(summary, "cgroup.kill") {
		t.Fatalf("live confirmation %q does not say how it kills", summary)
	}
}

// verifies: AIRA-271 — the arm times out after five top refreshes (about 5 s),
// and drops at once when its job leaves the listing.
func TestTopKillArmExpires(t *testing.T) {
	const wantTicks = 5
	listing := topQueuedListing(topQueuedTestRecord(killTestZulu, "zulu", 4*gib))
	deliver := func(state tuiState, listing runner.ConfineListResult) tuiState {
		state, _ = requestPanelRefresh(state, viewTop)
		state, _ = onTUIFetchResult(state, fetchResult{View: viewTop, Generation: state.Panels[viewTop].InFlightGeneration, Top: &listing})
		return state
	}
	state, _ := topKillPress(t, topKillState(t, "session-a"), 1, "K")
	for tick := 1; tick < wantTicks; tick++ {
		state = deliver(state, listing)
		if state.TopKill.ScopeID != killTestAlpha {
			t.Fatalf("the arm dropped after %d refreshes, want %d", tick, wantTicks)
		}
	}
	state = deliver(state, listing)
	if state.TopKill.ScopeID != "" {
		t.Fatalf("the arm outlived %d refreshes: %+v", wantTicks, state.TopKill)
	}

	state, _ = topKillPress(t, topKillState(t, "session-a"), 1, "K")
	without := topTestListing(topTestFrame(), topTestRecord(killTestBravo, "bravo", 3*gib, gib))
	if state = deliver(state, without); state.TopKill.ScopeID != "" {
		t.Fatalf("the arm outlived its job: %+v", state.TopKill)
	}
}

// verifies: AIRA-271 review round 2 (Sol P2) — a refresh that fails, or lands
// without a listing, drops the arm at once: the listing that named the job can
// no longer be re-read, so the arm cannot age on it. After K, any number of
// failed refreshes and then a good one, a K arms afresh and opens nothing.
func TestTopKillArmDropsOnAFailedRefresh(t *testing.T) {
	listing := topQueuedListing(topQueuedTestRecord(killTestZulu, "zulu", 4*gib))
	land := func(state tuiState, result fetchResult) tuiState {
		state, _ = requestPanelRefresh(state, viewTop)
		result.View, result.Generation = viewTop, state.Panels[viewTop].InFlightGeneration
		state, _ = onTUIFetchResult(state, result)
		return state
	}
	for _, failed := range []struct {
		name   string
		result fetchResult
	}{
		{"a daemon error", fetchResult{Code: daemon.CodeUnavailable}},
		{"a reply without a listing", fetchResult{}},
	} {
		state, _ := topKillPress(t, topKillState(t, "session-a"), 1, "K")
		if state.TopKill.ScopeID != killTestAlpha {
			t.Fatalf("%s: K did not arm alpha: %+v", failed.name, state.TopKill)
		}
		state = land(state, failed.result)
		if state.TopKill.ScopeID != "" {
			t.Fatalf("%s: the arm survived the failed refresh: %+v", failed.name, state.TopKill)
		}
		for range topKillArmTicks {
			state = land(state, failed.result)
		}
		state = land(state, fetchResult{Top: &listing})
		state, commands := topKillPress(t, state, 1, "K")
		assertNoKillOpened(t, state, commands, failed.name+", then a good refresh, then K")
		if state.TopKill.ScopeID != killTestAlpha || state.TopKill.Ticks != 0 {
			t.Fatalf("%s: K after the failures did not arm afresh: %+v", failed.name, state.TopKill)
		}
	}
}

// verifies: AIRA-271 — the reply is said from its own fields: a kill and a
// queued cancel by status and phase, an unconfirmed kill as an unknown outcome
// (never refused, never done), a refusal with its code, a missing field never
// made up.
func TestTopKillResultSaysOnlyWhatTheReplyCarries(t *testing.T) {
	reply := func(result runner.ConfineKillResult) paletteResult {
		data, _ := json.Marshal(result)
		return classifyPaletteDispatch(paletteDispatchAttempt{Response: core.Response{OK: true, Code: "OK", RawData: data}})
	}
	killed := reply(runner.ConfineKillResult{Status: "killed", Phase: "running", ScopeID: killTestAlpha, Name: "alpha", Owner: "session-a"})
	if killed.Outcome != paletteApplied || killed.Text != "APPLIED: the daemon accepted the request.\nName: alpha (owner session-a)\nStatus: killed\nPhase: running" {
		t.Fatalf("killed = %#v", killed)
	}
	cancelled := reply(runner.ConfineKillResult{Status: "cancelled", Phase: "queued", ScopeID: killTestZulu, Name: "zulu", Owner: "session-q"})
	if !strings.Contains(cancelled.Text, "Status: cancelled\nPhase: queued") {
		t.Fatalf("queued cancel = %q", cancelled.Text)
	}
	bare := classifyPaletteDispatch(paletteDispatchAttempt{Response: core.Response{OK: true, Code: "OK", RawData: json.RawMessage(`{"name":"alpha"}`)}})
	if bare.Text != "APPLIED: the daemon accepted the request.\nName: alpha" {
		t.Fatalf("a reply without status or phase = %q, want neither made up", bare.Text)
	}

	unconfirmed := classifyPaletteDispatch(paletteDispatchAttempt{Response: core.Response{
		Code: runner.CodeConfineKillUnconfirmed, Error: runner.CodeConfineKillUnconfirmed + ": scope " + killTestAlpha + " empty state unconfirmed: timeout", Exit: 3,
	}})
	if unconfirmed.Outcome != paletteOutcomeUnknown || !strings.HasPrefix(unconfirmed.Text, "UNEVALUATED — outcome unknown\n"+runner.CodeConfineKillUnconfirmed+": scope") {
		t.Fatalf("unconfirmed kill = %#v, want an unknown outcome with the daemon's code", unconfirmed)
	}
	for _, claim := range []string{"REJECTED", "refused", "APPLIED", "killed"} {
		if strings.Contains(unconfirmed.Text, claim) {
			t.Fatalf("unconfirmed kill claims %q: %q", claim, unconfirmed.Text)
		}
	}

	refusalErr := runner.CheckConfineOwner(killTestAlpha, "session-a", "session-b", false)
	refused := classifyPaletteDispatch(paletteDispatchAttempt{Response: core.Response{Code: runner.CodeConfineOwnerUnverified, Error: refusalErr.Error(), Exit: 1}})
	if refused.Outcome != paletteRejected || refused.Text != "REJECTED: the daemon refused the request.\n"+refusalErr.Error() {
		t.Fatalf("owner refusal = %#v", refused)
	}
	// A not-yet-launched job is a refusal too: nothing was killed.
	notLaunched := classifyPaletteDispatch(paletteDispatchAttempt{Response: core.Response{Code: runner.CodeConfineNotLaunched, Error: runner.CodeConfineNotLaunched + ": nothing to kill yet; retry", Exit: 3}})
	if notLaunched.Outcome != paletteRejected {
		t.Fatalf("not launched = %#v, want rejected", notLaunched)
	}
}

// verifies: AIRA-271 — the real dispatcher delivers the kill as ONE
// project-less confine-kill exchange (the palette refuses client-routed verbs,
// so without its own arm the key would reach "not daemon-routed"), and with
// the daemon unreachable it says the request was not sent and never falls back
// to a local kill, as the CLI's daemon-down path does.
func TestTopKillReachesTheDaemonThroughThePalette(t *testing.T) {
	state, _ := topKillPress(t, topKillState(t, "session-a"), 1, "KK")
	request := state.PaletteConfirm.Request

	var sent []daemon.RequestFrame
	reply := daemon.ResponseFrame{OK: true, Code: "OK"}
	d := &daemonDispatcher{}
	d.resolveConfineSlice = func(string) (string, string, error) {
		t.Fatal("the TUI kill took the daemon-down local path")
		return "", "", nil
	}
	d.exchange = func(_ context.Context, _ string, frame daemon.RequestFrame) (daemon.ResponseFrame, error) {
		sent = append(sent, frame)
		return reply, nil
	}
	reply.Data, _ = json.Marshal(runner.ConfineKillResult{Status: "killed", Phase: "running", ScopeID: killTestAlpha, Name: "alpha"})
	result := executePaletteRequest(context.Background(), d, daemon.WorktreeScope{}, request)
	if len(sent) != 1 || sent[0].Request.Verb != "confine-kill" || sent[0].Request.Args["selector"] != killTestAlpha || sent[0].Request.Args["steal"] != false || sent[0].Scope.Root != "" {
		t.Fatalf("frames=%+v, want one project-less confine-kill of alpha without steal", sent)
	}
	if result.Outcome != paletteApplied || !strings.Contains(result.Text, "Status: killed") {
		t.Fatalf("result=%#v, want applied", result)
	}
	// A refusal code the old-daemon hint does not know keeps the daemon's words.
	reply = daemon.ResponseFrame{OK: false, Code: "E_SOMETHING", Error: "E_SOMETHING: no"}
	if result = executePaletteRequest(context.Background(), d, daemon.WorktreeScope{}, request); strings.Contains(result.Text, "--front") || strings.Contains(result.Text, "predate") {
		t.Fatalf("a kill refusal carries the front/resize old-daemon hint: %q", result.Text)
	}

	sent = nil
	d.exchange = func(_ context.Context, _ string, frame daemon.RequestFrame) (daemon.ResponseFrame, error) {
		sent = append(sent, frame)
		return daemon.ResponseFrame{}, &daemon.RequestNotSentError{Err: errors.New(daemon.CodeUnavailable + ": dial: no daemon")}
	}
	result = executePaletteRequest(context.Background(), d, daemon.WorktreeScope{}, request)
	if len(sent) != 1 || result.Outcome != paletteRejected || !strings.HasPrefix(result.Text, "REJECTED (not sent)") {
		t.Fatalf("daemon down: frames=%d result=%#v, want one attempt, not sent, no fallback", len(sent), result)
	}
}

// verifies: AIRA-271 — the generated agent guide names the key.
func TestSkillNamesTheTopKillKey(t *testing.T) {
	artifacts, err := core.GenerateSkillArtifacts(core.New(nil).DispatchDescriptors())
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(artifacts.SkillMD), "in `aira top` it is `K` pressed twice") {
		t.Fatal("the skill does not say how `aira top` kills a job")
	}
}

// startTopKillRuntime is startTopDialogRuntime with row (1-based, among the
// model's rows) selected.
func startTopKillRuntime(t *testing.T, dispatcher *topDialogDispatcher, width, height, row int) (*tuiRuntime, tcell.SimulationScreen, *keyTracker) {
	t.Helper()
	runtime, screen, keys := startTopDialogRuntime(t, dispatcher, width, height)
	eventuallyOnUI(t, runtime, testdeadline.Wait(10*time.Second), func() bool {
		if len(runtime.state.Panels[viewTop].Model.Rows) < row {
			return false
		}
		runtime.tables[viewTop].Select(row, 0)
		return true
	}, "row %d never reached the top model", row)
	return runtime, screen, keys
}

func frontPage(t *testing.T, runtime *tuiRuntime) string {
	t.Helper()
	return onUI(t, runtime, func() string { name, _ := runtime.outerPages.GetFrontPage(); return name })
}

// verifies: AIRA-271 review round 2 (Sol P2) — the `aira top` key legend is one
// row, so KK must be on screen at ordinary widths, not clipped off the end: at
// 80 columns the whole legend fits, and at 60 KK still leads the top keys.
func TestTopKillLegendFitsOrdinaryWidths(t *testing.T) {
	for _, width := range []struct {
		columns int
		want    []string
	}{
		{80, []string{"KK kill", "f to front", "e lower reserve", "c cmd/dir", "r refresh", "q quit"}},
		{60, []string{"KK kill"}},
	} {
		runtime, screen, _ := startTopDialogRuntime(t, &topDialogDispatcher{}, width.columns, 24)
		text := simulationTextOnUI(t, runtime, screen)
		for _, want := range width.want {
			if !strings.Contains(text, want) {
				t.Fatalf("at %d columns the legend does not show %q:\n%s", width.columns, want, text)
			}
		}
	}
}

// verifies: AIRA-381 — above 80 columns the `aira top` legend also offers
// `t tier`, where the whole line fits, without displacing a key the 80-column
// legend shows; at 80 it is absent (that legend is full). A resize alone, with
// no refresh or key press, recomposes the line (the before-draw width seam).
// Mutations: `t tier` never added; added at every width (the 80-column legend
// then clips `q quit`); added at every width above 80 without the fit check
// (84 columns clips `q quit`); the seam not recomposing (the line keeps the
// width it was composed for).
func TestTopLegendShowsTierWhenWide(t *testing.T) {
	runtime, screen, _ := startTopDialogRuntime(t, &topDialogDispatcher{}, 100, 24)
	text := simulationTextOnUI(t, runtime, screen)
	for _, want := range []string{"KK kill", "f to front", "e lower reserve", "t tier", "c cmd/dir", "r refresh", "q quit"} {
		if !strings.Contains(text, want) {
			t.Fatalf("at 100 columns the legend does not show %q:\n%s", want, text)
		}
	}
	resizeBoardScreen(t, runtime, screen, 80, 24)
	if text := simulationTextOnUI(t, runtime, screen); strings.Contains(text, "t tier") || !strings.Contains(text, "q quit") {
		t.Fatalf("at 80 columns the legend shows `t tier` or lost `q quit`:\n%s", text)
	}
	// 84 columns is above 80 but too narrow for the line with `t tier` (87 on
	// `aira top`): the key is left out rather than clipping `q quit`.
	resizeBoardScreen(t, runtime, screen, 84, 24)
	if text := simulationTextOnUI(t, runtime, screen); strings.Contains(text, "t tier") || !strings.Contains(text, "q quit") {
		t.Fatalf("at 84 columns the legend shows `t tier` or lost `q quit`:\n%s", text)
	}
	resizeBoardScreen(t, runtime, screen, 120, 24)
	if text := simulationTextOnUI(t, runtime, screen); !strings.Contains(text, "t tier") {
		t.Fatalf("a resize back to 120 columns did not bring `t tier` back:\n%s", text)
	}
}

// verifies: AIRA-271 — on a real `aira top` runtime: the legend offers KK, one
// K says what is armed on the status line and opens nothing, an arrow key in
// between disarms, the confirmation starts on Cancel, and Enter on Cancel, Esc,
// `n` and `y` send nothing; only Tab then Enter (Confirm) sends the one kill.
func TestTopKillRuntimeSendsNothingUnlessConfirmed(t *testing.T) {
	data, _ := json.Marshal(runner.ConfineKillResult{Status: "killed", Phase: "running", ScopeID: dialogTestJob, Name: "alpha", Owner: "session-a"})
	dispatcher := &topDialogDispatcher{reply: core.Response{OK: true, Code: "OK", RawData: data}}
	runtime, screen, keys := startTopKillRuntime(t, dispatcher, 100, 30, 1)
	if text := simulationTextOnUI(t, runtime, screen); !strings.Contains(text, "KK kill") {
		t.Fatalf("the legend does not offer KK:\n%s", text)
	}
	keys.press(t, runeKey('K'))
	if text := simulationTextOnUI(t, runtime, screen); !strings.Contains(text, "KILL ARMED") || !strings.Contains(text, "alpha (owner session-a") || frontPage(t, runtime) != dashboardPage {
		t.Fatalf("one K did not show the arm on the status line, or opened a page:\n%s", text)
	}
	keys.press(t, specialKey(tcell.KeyDown), runeKey('K'))
	if text := simulationTextOnUI(t, runtime, screen); frontPage(t, runtime) != dashboardPage || !strings.Contains(text, "KILL ARMED") {
		t.Fatalf("K, Down, K opened %q instead of arming again:\n%s", frontPage(t, runtime), text)
	}
	keys.press(t, specialKey(tcell.KeyEscape))
	if text := simulationTextOnUI(t, runtime, screen); strings.Contains(text, "KILL ARMED") {
		t.Fatalf("Esc did not disarm:\n%s", text)
	}

	open := func() {
		t.Helper()
		keys.press(t, runeKey('K'), runeKey('K'))
		waitForSimulationText(t, runtime, screen, "Confirm DESTRUCTIVE")
	}
	open()
	if text := simulationTextOnUI(t, runtime, screen); !strings.Contains(text, "Kill alpha (owner session-a") {
		t.Fatalf("the confirmation does not name alpha:\n%s", text)
	}
	assertFocusedButton(t, runtime, screen, "Cancel", "Confirm", "initial focus")
	for _, dismiss := range []tuiKey{specialKey(tcell.KeyEnter), specialKey(tcell.KeyEscape), runeKey('n')} {
		keys.press(t, dismiss)
		if writes := dispatcher.writes(); len(writes) != 0 || frontPage(t, runtime) != dashboardPage {
			t.Fatalf("dismissing with %+v sent %+v or left %q open", dismiss, writes, frontPage(t, runtime))
		}
		open()
	}
	keys.press(t, runeKey('y'))
	if writes := dispatcher.writes(); len(writes) != 0 {
		t.Fatalf("y sent %+v", writes)
	}
	keys.press(t, specialKey(tcell.KeyTab), specialKey(tcell.KeyEnter))
	text := waitForSimulationText(t, runtime, screen, "APPLIED")
	writes := dispatcher.writes()
	if len(writes) != 1 || writes[0].Verb != "confine-kill" || writes[0].Args["selector"] != dialogTestJob || writes[0].Args["steal"] != false || writes[0].Args["owner"] != "session-a" {
		t.Fatalf("Confirm sent %+v, want one confine-kill of alpha without steal", writes)
	}
	if !strings.Contains(text, "Status: killed") || !strings.Contains(text, "Phase: running") {
		t.Fatalf("the result does not say the reply's status and phase:\n%s", text)
	}
}

// verifies: AIRA-271 — another session's job: the kill goes out without
// --steal and the daemon's own owner refusal is what the screen shows; nothing
// says the job was killed.
func TestTopKillRuntimeShowsTheOwnerRefusal(t *testing.T) {
	refusal := runner.CheckConfineOwner(dialogTestJob, "session-a", "session-b", false)
	dispatcher := &topDialogDispatcher{caller: "session-b", reply: core.Response{Code: runner.CodeConfineOwnerUnverified, Error: refusal.Error(), Exit: 1}}
	runtime, screen, keys := startTopKillRuntime(t, dispatcher, 100, 30, 1)
	keys.press(t, runeKey('K'), runeKey('K'))
	waitForSimulationText(t, runtime, screen, "Confirm DESTRUCTIVE")
	keys.press(t, specialKey(tcell.KeyTab), specialKey(tcell.KeyEnter))
	text := waitForSimulationText(t, runtime, screen, "REJECTED")
	if strings.Contains(text, "APPLIED") || strings.Contains(text, "killed") {
		t.Fatalf("the refusal claims a kill:\n%s", text)
	}
	// Sol round 3: placed, not merely present. A daemon refusal is AIRA-342's
	// shared result (its notice line holds only a refused form value): the
	// Result page, the REJECTED head, then the daemon's code on the very next
	// line, exactly as `f` and `e` show theirs.
	if page := frontPage(t, runtime); page != paletteResultPage {
		t.Fatalf("the refusal is on page %q, want the shared Result page %q", page, paletteResultPage)
	}
	const head = "REJECTED: the daemon refused the request."
	lines := strings.Split(text, "\n")
	at := slices.IndexFunc(lines, func(line string) bool { return strings.Contains(line, head) })
	if at < 0 || at+1 >= len(lines) || !strings.Contains(lines[at+1], runner.CodeConfineOwnerUnverified+":") {
		t.Fatalf("the refusal is not the REJECTED head followed by the daemon's code line:\n%s", text)
	}
	if writes := dispatcher.writes(); len(writes) != 1 || writes[0].Args["steal"] != false || writes[0].Args["owner"] != "session-b" {
		t.Fatalf("sent %+v, want one kill as session-b without steal", writes)
	}
}

// verifies: AIRA-271 — a queued row is cancelled in queued words, at 60x24.
func TestTopKillRuntimeCancelsAQueuedJob(t *testing.T) {
	data, _ := json.Marshal(runner.ConfineKillResult{Status: "cancelled", Phase: "queued", ScopeID: killTestZulu, Name: "zulu", Owner: "session-q"})
	dispatcher := &topDialogDispatcher{caller: "session-q", queued: []runner.ConfineRecord{topQueuedTestRecord(killTestZulu, "zulu", 4*gib)},
		reply: core.Response{OK: true, Code: "OK", RawData: data}}
	runtime, screen, keys := startTopKillRuntime(t, dispatcher, 60, 24, 2)
	keys.press(t, runeKey('K'))
	if text := simulationTextOnUI(t, runtime, screen); !strings.Contains(text, "CANCEL ARMED") || !strings.Contains(text, "zulu") {
		t.Fatalf("the status line does not say the queued cancel is armed at 60 columns:\n%s", text)
	}
	keys.press(t, runeKey('K'))
	waitForSimulationText(t, runtime, screen, "Confirm DESTRUCTIVE")
	keys.press(t, specialKey(tcell.KeyTab), specialKey(tcell.KeyEnter))
	text := waitForSimulationText(t, runtime, screen, "APPLIED")
	if !strings.Contains(text, "Status: cancelled") || !strings.Contains(text, "Phase: queued") {
		t.Fatalf("the queued cancel result:\n%s", text)
	}
	if writes := dispatcher.writes(); len(writes) != 1 || writes[0].Args["selector"] != killTestZulu || writes[0].Args["steal"] != false {
		t.Fatalf("sent %+v, want one kill of zulu without steal", writes)
	}
}

// verifies: AIRA-271 review (Sol P1) — a pasted block is text for the focused
// widget, never keys: pasting "KK<Tab><Enter>" on the table arms nothing,
// opens nothing and sends nothing.
func TestTopKillPasteIsNotKeys(t *testing.T) {
	dispatcher := &topDialogDispatcher{reply: core.Response{OK: true, Code: "OK", RawData: json.RawMessage(`{}`)}}
	runtime, screen, keys := startTopKillRuntime(t, dispatcher, 100, 30, 1)
	if err := screen.PostEvent(tcell.NewEventPaste(true)); err != nil {
		t.Fatal(err)
	}
	keys.typeText("KK")
	screen.InjectKey(tcell.KeyTab, 0, tcell.ModNone)
	screen.InjectKey(tcell.KeyEnter, 0, tcell.ModNone)
	if err := screen.PostEvent(tcell.NewEventPaste(false)); err != nil {
		t.Fatal(err)
	}
	keys.barrier(t)
	keys.barrier(t)
	if writes := dispatcher.writes(); len(writes) != 0 || frontPage(t, runtime) != dashboardPage {
		t.Fatalf("a paste sent %+v or opened %q", writes, frontPage(t, runtime))
	}
	if armed := onUI(t, runtime, func() string { return runtime.state.TopKill.ScopeID }); armed != "" {
		t.Fatalf("a paste armed %q", armed)
	}
}

// verifies: AIRA-271 review (Sol P2) — on the full dashboard, whose tab row is
// long, the armed kill starts the status line, so it is on screen at any width.
func TestTopKillArmedStatusLeadsTheDashboardLine(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	runtime := newTUIRuntimeForViews(ctx, &topDialogDispatcher{}, nil, daemon.WorktreeScope{}, nil, nil, nil,
		tcell.NewSimulationScreen("UTF-8"), allViews, dataViews, false, nil, nil)
	runtime.state.Active = viewTop
	runtime.state.TopKill = topKillArm{ScopeID: killTestAlpha, Job: "alpha (owner session-a, command go test)"}
	runtime.render()
	if line := runtime.tabs.GetText(true); !strings.HasPrefix(line, "KILL ARMED: alpha (owner session-a") {
		t.Fatalf("the dashboard status line does not lead with the arm: %q", line)
	}
	runtime.state.TopKill = topKillArm{}
	runtime.render()
	if line := runtime.tabs.GetText(true); !strings.Contains(line, "KK kill") || strings.Contains(line, "ARMED") {
		t.Fatalf("the unarmed dashboard line: %q", line)
	}
}
