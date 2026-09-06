package main

import (
	"reflect"
	"testing"
	"time"

	"aira/internal/core"
	"aira/internal/store"
)

func TestControllerPaletteReadIsOneStepAndMutationRequiresConfirmation(t *testing.T) {
	state := newTUIState(8)
	read := paletteEntry{Verb: "find", Operation: "ls", Safety: core.SafetyRead}
	readRequest := core.Request{Verb: "find", Args: map[string]any{"subverb": "ls"}}
	state, commands := onPaletteSubmit(state, read, readRequest)
	assertSinglePaletteCommand(t, commands, readRequest)
	if state.PaletteConfirm != nil || state.PaletteDispatching {
		t.Fatalf("read unexpectedly entered confirmation: %#v", state.PaletteConfirm)
	}

	mutation := paletteEntry{Verb: "spend", Operation: "add", Summary: "record spend", Safety: core.SafetyMutate}
	mutationRequest := core.Request{Verb: "spend", Args: map[string]any{"subverb": "add", "provider": "openai"}}
	state, commands = onPaletteSubmit(state, mutation, mutationRequest)
	if len(commands) != 0 || state.PaletteConfirm == nil || state.PaletteDispatching {
		t.Fatalf("mutation submit state=%#v commands=%#v", state.PaletteConfirm, commands)
	}
	if state.PaletteConfirm.Verb != "spend.add" || state.PaletteConfirm.Summary != "record spend" {
		t.Fatalf("confirmation snapshot=%#v", state.PaletteConfirm)
	}
}

// covers: confirmation consumes the immutable pending request atomically.
func TestControllerPaletteConfirmIsExactlyOnceAndSnapshotIsImmutable(t *testing.T) {
	state := newTUIState(8)
	entry := paletteEntry{Verb: "find", Operation: "set", Safety: core.SafetyMutate}
	request := core.Request{Verb: "find", Args: map[string]any{"subverb": "set", "selector": "f-1", "labels": []string{"before"}}}
	state, commands := onPaletteSubmit(state, entry, request)
	if len(commands) != 0 {
		t.Fatalf("form submission dispatched: %#v", commands)
	}
	request.Args["selector"] = "f-CHANGED"
	request.Args["labels"].([]string)[0] = "changed"

	state, commands = onPaletteConfirm(state)
	want := core.Request{Verb: "find", Args: map[string]any{"subverb": "set", "selector": "f-1", "labels": []string{"before"}}}
	assertSinglePaletteCommand(t, commands, want)
	if !state.PaletteDispatching || state.PaletteConfirm != nil {
		t.Fatalf("accepted confirmation state dispatching=%v confirm=%#v", state.PaletteDispatching, state.PaletteConfirm)
	}
	for i := 0; i < 8; i++ {
		var repeated []tuiCmd
		state, repeated = onPaletteConfirm(state)
		if len(repeated) != 0 {
			t.Fatalf("repeat %d re-dispatched: %#v", i, repeated)
		}
	}
}

func TestControllerPaletteCancelAndDestructiveTypedIDGate(t *testing.T) {
	state := newTUIState(8)
	entry := paletteEntry{Verb: "rant", Operation: "redact", Summary: "redact", Safety: core.SafetyMutate, Destructive: true}
	request := core.Request{Verb: "rant", Args: map[string]any{"subverb": "redact", "selector": "RANT-7"}}
	state, commands := onPaletteSubmit(state, entry, request)
	if len(commands) != 0 || state.PaletteConfirm == nil || state.PaletteConfirm.ConfirmIDTarget != "RANT-7" {
		t.Fatalf("destructive submit state=%#v commands=%#v", state.PaletteConfirm, commands)
	}
	for _, typed := range []string{"", "RANT-8", " RANT-7 "} {
		state = onPaletteConfirmTypedID(state, typed)
		if paletteConfirmEnabled(state.PaletteConfirm) {
			t.Fatalf("typed id %q enabled destructive confirmation", typed)
		}
		var rejected []tuiCmd
		state, rejected = onPaletteConfirm(state)
		if len(rejected) != 0 {
			t.Fatalf("typed id %q dispatched: %#v", typed, rejected)
		}
	}
	state = onPaletteConfirmTypedID(state, "RANT-7")
	if !paletteConfirmEnabled(state.PaletteConfirm) {
		t.Fatal("exact resolved id did not enable destructive confirmation")
	}
	state, commands = onPaletteCancel(state)
	if len(commands) != 0 || state.PaletteConfirm != nil || state.PaletteDispatching {
		t.Fatalf("cancel state=%#v commands=%#v", state, commands)
	}
}

func TestControllerPaletteResultForcesRefreshForAppliedAndUnknownOnly(t *testing.T) {
	descriptors := core.New(nil).DispatchDescriptors()
	for _, outcome := range []paletteOutcome{paletteApplied, paletteOutcomeUnknown} {
		state := newTUIState(8)
		for _, view := range dataViews {
			panel := state.Panels[view]
			panel.Status = panelReady
			state.Panels[view] = panel
		}
		state.PaletteDispatching = true
		state.PaletteDispatchedVerb = "spend.add" // spend add emits no watch event.
		state, commands := onPaletteResult(state, outcome, descriptors)
		if state.PaletteDispatching || state.PaletteDispatchedVerb != "" {
			t.Fatalf("outcome %s retained dispatch state: %#v", outcome, state)
		}
		if len(commands) != len(dataViews) {
			t.Fatalf("outcome %s commands=%#v, want one forced fetch per data view", outcome, commands)
		}
		for index, view := range dataViews {
			if commands[index].Kind != cmdFetch || commands[index].View != view {
				t.Fatalf("outcome %s command[%d]=%#v", outcome, index, commands[index])
			}
		}
	}

	state := newTUIState(8)
	state.PaletteDispatching = true
	state.PaletteDispatchedVerb = "spend.add"
	state, commands := onPaletteResult(state, paletteRejected, descriptors)
	if len(commands) != 0 || state.PaletteDispatching || state.PaletteDispatchedVerb != "" {
		t.Fatalf("definite rejection refreshed or retained dispatch state: state=%#v commands=%#v", state, commands)
	}
}

func assertSinglePaletteCommand(t *testing.T, commands []tuiCmd, want core.Request) {
	t.Helper()
	if len(commands) != 1 || commands[0].Kind != cmdPalette || commands[0].Palette == nil || !reflect.DeepEqual(*commands[0].Palette, want) {
		t.Fatalf("commands=%#v, want one palette command %#v", commands, want)
	}
}

func TestControllerManualRefreshDuringFetchQueuesOneTrailingFetch(t *testing.T) {
	state := newTUIState(8)
	state.Active = viewTickets
	state, cmds := requestPanelRefresh(state, viewTickets)
	assertSingleFetch(t, cmds, viewTickets, 1)

	state, cmds = onTUIKey(state, 'r', core.New(nil).DispatchDescriptors())
	if len(cmds) != 0 || !state.Panels[viewTickets].Dirty || state.Panels[viewTickets].Generation != 1 {
		t.Fatalf("refresh during fetch state=%#v cmds=%#v", state.Panels[viewTickets], cmds)
	}
	state, cmds = onTUIFetchResult(state, fetchResult{View: viewTickets, Generation: 1, Model: panelModel{Footer: "first"}})
	assertSingleFetch(t, cmds, viewTickets, 2)
	panel := state.Panels[viewTickets]
	if !panel.InFlight || panel.InFlightGeneration != 2 || panel.Dirty {
		t.Fatalf("trailing fetch state=%#v", panel)
	}
	state, cmds = onTUIFetchResult(state, fetchResult{View: viewTickets, Generation: 2, Model: panelModel{Footer: "final"}})
	if len(cmds) != 0 || state.Panels[viewTickets].InFlight || state.Panels[viewTickets].Model.Footer != "final" {
		t.Fatalf("quiescent state=%#v cmds=%#v", state.Panels[viewTickets], cmds)
	}
}

func TestControllerLateResultDoesNotClearNewerInFlightFetch(t *testing.T) {
	state := newTUIState(8)
	// Dirty is deliberately TRUE: a superseded (stale-generation) result must not
	// clear InFlight, must not clear Dirty (dropping the pending trailing refresh),
	// and must not fold its model/error. It must change NOTHING.
	state.Panels[viewTickets] = panelState{Generation: 2, InFlight: true, InFlightGeneration: 2, Status: panelLoading, Dirty: true, SelectedID: "AIRA-9", Model: panelModel{Footer: "current"}}
	state, cmds := onTUIFetchResult(state, fetchResult{View: viewTickets, Generation: 1, Code: "E_STALE", Model: panelModel{Footer: "stale"}})
	panel := state.Panels[viewTickets]
	if len(cmds) != 0 {
		t.Fatalf("stale result emitted commands: %#v", cmds)
	}
	if !panel.InFlight || panel.InFlightGeneration != 2 || !panel.Dirty || panel.Generation != 2 ||
		panel.Status != panelLoading || panel.ErrorCode != "" || panel.SelectedID != "AIRA-9" || panel.Model.Footer != "current" {
		t.Fatalf("stale result mutated the newer in-flight fetch: %#v", panel)
	}
}

func TestControllerDropsStaleSelectedDetail(t *testing.T) {
	state := newTUIState(8)
	state.Panels[viewTickets] = panelState{Generation: 3, Model: panelModel{Detail: "current"}}
	state, commands := onTUISelect(state, viewTickets, "AIRA-2")
	if len(commands) != 1 || commands[0].DetailID != "AIRA-2" || commands[0].Generation != 3 {
		t.Fatalf("selection commands=%#v", commands)
	}
	state, _ = onTUIDetailResult(state, detailResult{View: viewTickets, Generation: 2, ID: "AIRA-2", Detail: "stale"})
	if state.Panels[viewTickets].Model.Detail != "current" {
		t.Fatalf("stale detail was folded: %#v", state.Panels[viewTickets])
	}
	state, _ = onTUIDetailResult(state, detailResult{View: viewTickets, Generation: 3, ID: "AIRA-2", Detail: "selected"})
	if state.Panels[viewTickets].Model.Detail != "selected" {
		t.Fatalf("current detail was not folded: %#v", state.Panels[viewTickets])
	}
}

func TestControllerWatchInvalidationIsDefaultOnAndCoalesced(t *testing.T) {
	state := newTUIState(8)
	event := store.WatchEvent{Seq: 1, Verb: "future.writer", Target: "AIRA-1"}
	state, cmds := onTUIWatchBatch(state, []store.WatchEvent{event}, 1, core.New(nil).DispatchDescriptors())
	if len(cmds) != len(dataViews) {
		t.Fatalf("schedule commands=%#v, want one per data panel", cmds)
	}
	state, cmds = onTUIWatchBatch(state, []store.WatchEvent{{Seq: 2, Verb: "future.writer"}}, 2, core.New(nil).DispatchDescriptors())
	if len(cmds) != 0 {
		t.Fatalf("duplicate invalidation was not coalesced: %#v", cmds)
	}
	for _, view := range dataViews {
		state, cmds = onTUIRefreshDue(state, view)
		assertSingleFetch(t, cmds, view, 1)
	}
	state, cmds = onTUIWatchBatch(state, []store.WatchEvent{{Seq: 3, Verb: "future.writer"}}, 3, core.New(nil).DispatchDescriptors())
	if len(cmds) != 0 {
		t.Fatalf("in-flight invalidation emitted commands: %#v", cmds)
	}
	for _, view := range dataViews {
		if !state.Panels[view].Dirty {
			t.Fatalf("view %s was not marked dirty", view)
		}
	}
}

func TestEveryNonReadDescriptorInvalidatesAtLeastOnePanel(t *testing.T) {
	descriptors := core.New(nil).DispatchDescriptors()
	for _, descriptor := range descriptors {
		if len(descriptor.Operations) == 0 {
			if descriptor.Safety == core.SafetyRead || descriptor.Name == "help" {
				continue
			}
			if got := invalidatedViews(tuiState{}, descriptor.Name, descriptors); len(got) == 0 {
				t.Fatalf("non-read descriptor %s has no invalidation", descriptor.Name)
			}
			continue
		}
		for _, operation := range descriptor.Operations {
			if operation.Safety != core.SafetyRead {
				verb := descriptor.Name + "." + operation.Name
				if got := invalidatedViews(tuiState{}, verb, descriptors); len(got) == 0 {
					t.Fatalf("non-read operation %s has no invalidation", verb)
				}
			}
		}
	}
}

func TestControllerReadEventDoesNotInvalidate(t *testing.T) {
	descriptors := core.New(nil).DispatchDescriptors()
	for _, verb := range []string{"list", "find.ls", "rant.get", "lease.ls"} {
		if got := invalidatedViews(tuiState{}, verb, descriptors); len(got) != 0 {
			t.Fatalf("read event %s invalidated %v", verb, got)
		}
	}
}

func TestControllerRingBoundAndEOFReconnect(t *testing.T) {
	state := newTUIState(2)
	descriptors := core.New(nil).DispatchDescriptors()
	state, _ = onTUIWatchBatch(state, []store.WatchEvent{{Seq: 1}, {Seq: 2}, {Seq: 3}}, 3, descriptors)
	if len(state.Events) != 2 || state.Events[0].Seq != 2 || state.Events[1].Seq != 3 || state.Cursor != 3 {
		t.Fatalf("bounded events=%#v cursor=%d", state.Events, state.Cursor)
	}
	state, cmds := onTUIEOF(state)
	if len(cmds) != 1 || cmds[0].Kind != cmdReconnect || cmds[0].Backoff < 50*time.Millisecond || state.ReconnectAttempt != 1 {
		t.Fatalf("EOF state=%#v cmds=%#v", state, cmds)
	}
	state, cmds = onTUIEOF(state)
	if cmds[0].Backoff <= 50*time.Millisecond || state.ReconnectAttempt != 2 {
		t.Fatalf("second EOF state=%#v cmds=%#v", state, cmds)
	}
	state, _ = onTUIWatchBatch(state, nil, 4, descriptors)
	if state.ReconnectAttempt != 0 {
		t.Fatalf("successful watch batch did not reset reconnect backoff: %#v", state)
	}
}

func assertSingleFetch(t *testing.T, cmds []tuiCmd, view tuiView, generation int) {
	t.Helper()
	if len(cmds) != 1 || cmds[0].Kind != cmdFetch || cmds[0].View != view || cmds[0].Generation != generation {
		t.Fatalf("commands=%#v, want fetch(%s,%d)", cmds, view, generation)
	}
}
