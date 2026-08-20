package main

import (
	"testing"
	"time"

	"aira/internal/core"
	"aira/internal/store"
)

func TestControllerManualRefreshDuringFetchQueuesOneTrailingFetch(t *testing.T) {
	state := newTUIState(8)
	state.Active = viewTickets
	state, cmds := requestPanelRefresh(state, viewTickets)
	assertSingleFetch(t, cmds, viewTickets, 1)

	state, cmds = onTUIKey(state, 'r')
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
			if got := invalidatedViews(descriptor.Name, descriptors); len(got) == 0 {
				t.Fatalf("non-read descriptor %s has no invalidation", descriptor.Name)
			}
			continue
		}
		for _, operation := range descriptor.Operations {
			if operation.Safety != core.SafetyRead {
				verb := descriptor.Name + "." + operation.Name
				if got := invalidatedViews(verb, descriptors); len(got) == 0 {
					t.Fatalf("non-read operation %s has no invalidation", verb)
				}
			}
		}
	}
}

func TestControllerReadEventDoesNotInvalidate(t *testing.T) {
	descriptors := core.New(nil).DispatchDescriptors()
	for _, verb := range []string{"list", "find.ls", "rant.get", "lease.ls"} {
		if got := invalidatedViews(verb, descriptors); len(got) != 0 {
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
