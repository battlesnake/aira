package main

import (
	"context"
	"reflect"
	"strings"
	"testing"

	"aira/internal/core"
	"aira/internal/daemon"
)

func inlineReadyState(view tuiView, selected string, rows ...tableRow) tuiState {
	state := newTUIState(8)
	state.Active = view
	panel := state.Panels[view]
	panel.Status = panelReady
	panel.SelectedID = selected
	panel.Model.Rows = append([]tableRow(nil), rows...)
	state.Panels[view] = panel
	return state
}

func descriptorEnum(t *testing.T, descriptors []core.DispatchDescriptor, verb, arg string) []string {
	t.Helper()
	for _, descriptor := range descriptors {
		if descriptor.Name != verb {
			continue
		}
		for _, spec := range descriptor.Args {
			if spec.Name == arg {
				return append([]string(nil), spec.Enum...)
			}
		}
	}
	t.Fatalf("descriptor enum %s.%s missing", verb, arg)
	return nil
}

// covers: inline enum actions are modal, descriptor-driven, and anchor the
// exact selected id before a watch-driven row reorder can change the table.
func TestInlineStatusPickerUsesLiveDescriptorAndAnchorsID(t *testing.T) {
	descriptors := core.New(nil).DispatchDescriptors()
	state := inlineReadyState(viewTickets, "AIRA-7",
		tableRow{ID: "AIRA-7"}, tableRow{ID: "AIRA-9"})
	action, ok := inlineActionFor(viewTickets, 's')
	if !ok {
		t.Fatal("tickets s inline action missing")
	}
	state, commands := onInlineActionStart(state, action, descriptors)
	if len(commands) != 0 || !state.PaletteOpen || state.InlineAction == nil {
		t.Fatalf("start state=%#v commands=%#v", state.InlineAction, commands)
	}
	if got, want := state.InlineAction.Options, descriptorEnum(t, descriptors, "mv", "status"); !reflect.DeepEqual(got, want) {
		t.Fatalf("status options=%v, want live descriptor %v", got, want)
	}
	if state.InlineAction.TargetID != "AIRA-7" {
		t.Fatalf("captured id=%q", state.InlineAction.TargetID)
	}

	// Reorder and change the live selection after opening. The dispatched request
	// must retain the immutable action-start id, never re-resolve the row index.
	panel := state.Panels[viewTickets]
	panel.SelectedID = "AIRA-9"
	panel.Model.Rows = []tableRow{{ID: "AIRA-9"}, {ID: "AIRA-7"}}
	state.Panels[viewTickets] = panel
	state, commands = onInlineActionPick(state, "planned", descriptors)
	if len(commands) != 0 || state.PaletteConfirm == nil {
		t.Fatalf("pick did not enter confirmation: state=%#v commands=%#v", state, commands)
	}
	want := core.Request{Verb: "mv", Args: map[string]any{"selector": "AIRA-7", "status": "planned"}}
	if !reflect.DeepEqual(state.PaletteConfirm.Request, want) {
		t.Fatalf("anchored request=%#v, want %#v", state.PaletteConfirm.Request, want)
	}
	state, commands = onPaletteConfirm(state)
	assertSinglePaletteCommand(t, commands, want)
	if !state.PaletteDispatching {
		t.Fatal("inline confirmation did not inherit palette exactly-once gate")
	}
	if _, repeated := onPaletteConfirm(state); len(repeated) != 0 {
		t.Fatalf("repeated inline confirmation dispatched: %#v", repeated)
	}
}

// verifies: every enum comes from the live descriptor projection; severity is
// sourced from create because set deliberately has no enum, and absence fails closed.
func TestInlineEnumsUseDescriptorsAndFailClosed(t *testing.T) {
	descriptors := core.New(nil).DispatchDescriptors()
	tests := []struct {
		view tuiView
		key  rune
		id   string
		verb string
		arg  string
	}{
		{view: viewTickets, key: 's', id: "AIRA-1", verb: "mv", arg: "status"},
		{view: viewTickets, key: 'v', id: "AIRA-1", verb: "create", arg: "severity"},
		{view: viewFindings, key: 'd', id: "f-1", verb: "find", arg: "disposition"},
	}
	for _, test := range tests {
		t.Run(string(test.view)+"/"+string(test.key), func(t *testing.T) {
			action, ok := inlineActionFor(test.view, test.key)
			if !ok {
				t.Fatal("action missing")
			}
			state, commands := onInlineActionStart(inlineReadyState(test.view, test.id, tableRow{ID: test.id}), action, descriptors)
			if len(commands) != 0 || state.InlineAction == nil || state.InlineError != "" {
				t.Fatalf("start state=%#v error=%q commands=%#v", state.InlineAction, state.InlineError, commands)
			}
			want := descriptorEnum(t, descriptors, test.verb, test.arg)
			if !reflect.DeepEqual(state.InlineAction.Options, want) {
				t.Fatalf("options=%v want=%v", state.InlineAction.Options, want)
			}
		})
	}

	// Remove mv.status's enum while retaining the descriptor and argument. An
	// enum action must surface an explicit error and offer no free-text fallback.
	broken := core.New(nil).DispatchDescriptors()
	for i := range broken {
		if broken[i].Name == "mv" {
			for j := range broken[i].Args {
				if broken[i].Args[j].Name == "status" {
					broken[i].Args[j].Enum = nil
				}
			}
		}
	}
	action, _ := inlineActionFor(viewTickets, 's')
	state, commands := onInlineActionStart(inlineReadyState(viewTickets, "AIRA-1", tableRow{ID: "AIRA-1"}), action, broken)
	if len(commands) != 0 || state.InlineAction != nil || state.InlineError == "" || !state.PaletteOpen {
		t.Fatalf("missing enum did not fail closed: inline=%#v error=%q commands=%#v open=%v", state.InlineAction, state.InlineError, commands, state.PaletteOpen)
	}
	duplicate := append(core.New(nil).DispatchDescriptors(), brokenDescriptor(t, descriptors, "mv"))
	state, commands = onInlineActionStart(inlineReadyState(viewTickets, "AIRA-1", tableRow{ID: "AIRA-1"}), action, duplicate)
	if len(commands) != 0 || state.InlineAction != nil || state.InlineError == "" {
		t.Fatalf("ambiguous enum did not fail closed: inline=%#v error=%q commands=%#v", state.InlineAction, state.InlineError, commands)
	}
}

func brokenDescriptor(t *testing.T, descriptors []core.DispatchDescriptor, name string) core.DispatchDescriptor {
	t.Helper()
	for _, descriptor := range descriptors {
		if descriptor.Name == name {
			return descriptor
		}
	}
	t.Fatalf("descriptor %s missing", name)
	return core.DispatchDescriptor{}
}

func TestInlineSelectorAndLeaseTokenSnapshotsAreVerbatim(t *testing.T) {
	descriptors := core.New(nil).DispatchDescriptors()
	selector := " AIRA-7 "
	state := inlineReadyState(viewTickets, selector, tableRow{ID: selector})
	action, _ := inlineActionFor(viewTickets, 's')
	state, _ = onInlineActionStart(state, action, descriptors)
	state, _ = onInlineActionPick(state, "planned", descriptors)
	if got := state.PaletteConfirm.Request.Args["selector"]; got != selector {
		t.Fatalf("selector snapshot=%q want verbatim %q", got, selector)
	}

	token := " token-at-open "
	state = inlineReadyState(viewLeases, "AIRA-7", tableRow{ID: "AIRA-7", LeaseToken: token, LeaseVersion: 3})
	action, _ = inlineActionFor(viewLeases, 'b')
	state, _ = onInlineActionStart(state, action, descriptors)
	if got := state.PaletteConfirm.Request.Args["token"]; got != token {
		t.Fatalf("lease token snapshot=%q want verbatim %q", got, token)
	}
}

func TestInlineHoldToggleReadsProjectedCurrentValue(t *testing.T) {
	model := ticketListViewModel(listEnvelope{Rows: []map[string]any{{"id": "AIRA-2", "hold": true}}})
	if len(model.Rows) != 1 || !model.Rows[0].Hold {
		t.Fatalf("ticket hold was not projected: %#v", model.Rows)
	}
	state := inlineReadyState(viewTickets, "AIRA-2", model.Rows[0])
	action, _ := inlineActionFor(viewTickets, 'h')
	state, commands := onInlineActionStart(state, action, core.New(nil).DispatchDescriptors())
	if len(commands) != 0 || state.PaletteConfirm == nil || !state.PaletteOpen {
		t.Fatalf("hold start state=%#v commands=%#v", state, commands)
	}
	want := core.Request{Verb: "set", Args: map[string]any{"selector": "AIRA-2", "field": "hold", "value": "false"}}
	if !reflect.DeepEqual(state.PaletteConfirm.Request, want) {
		t.Fatalf("hold toggle request=%#v want=%#v", state.PaletteConfirm.Request, want)
	}
}

func TestInlineWaivedDispositionRequiresReasonActorMiniForm(t *testing.T) {
	descriptors := core.New(nil).DispatchDescriptors()
	state := inlineReadyState(viewFindings, "f-7", tableRow{ID: "f-7"})
	action, _ := inlineActionFor(viewFindings, 'd')
	state, _ = onInlineActionStart(state, action, descriptors)
	state, commands := onInlineActionPick(state, "waived", descriptors)
	if len(commands) != 0 || state.InlineAction == nil || state.InlineAction.Stage != inlineStageMiniForm {
		t.Fatalf("waived pick=%#v commands=%#v", state.InlineAction, commands)
	}
	if !reflect.DeepEqual(state.InlineAction.FormArgs, []string{"reason", "actor"}) {
		t.Fatalf("waived fields=%v", state.InlineAction.FormArgs)
	}
	state, commands = onInlineMiniFormSubmit(state, map[string]string{"reason": "accepted risk", "actor": "human"}, descriptors)
	if len(commands) != 0 || state.PaletteConfirm == nil {
		t.Fatalf("waived form state=%#v commands=%#v", state, commands)
	}
	want := core.Request{Verb: "find", Args: map[string]any{
		"subverb": "set", "selector": "f-7", "disposition": "waived", "reason": "accepted risk", "actor": "human",
	}}
	if !reflect.DeepEqual(state.PaletteConfirm.Request, want) {
		t.Fatalf("waived request=%#v want=%#v", state.PaletteConfirm.Request, want)
	}
}

func TestInlineLeaseCapturesVersionAndReadyUsesFixedStatus(t *testing.T) {
	descriptors := core.New(nil).DispatchDescriptors()
	lease := tableRow{ID: "AIRA-4", LeaseToken: "token-at-open", LeaseVersion: 17}
	state := inlineReadyState(viewLeases, "AIRA-4", lease)
	action, _ := inlineActionFor(viewLeases, 'k')
	state, _ = onInlineActionStart(state, action, descriptors)
	if state.InlineAction == nil || state.InlineAction.LeaseToken != "token-at-open" || state.InlineAction.LeaseVersion != 17 {
		t.Fatalf("lease snapshot=%#v", state.InlineAction)
	}
	// Change the row after action start; the confirmation must retain the snapshot.
	panel := state.Panels[viewLeases]
	panel.Model.Rows[0].LeaseToken, panel.Model.Rows[0].LeaseVersion = "new-token", 18
	state.Panels[viewLeases] = panel
	if state.PaletteConfirm == nil || state.PaletteConfirm.Request.Args["token"] != "token-at-open" {
		t.Fatalf("release did not retain captured token: %#v", state.PaletteConfirm)
	}

	ready := inlineReadyState(viewReady, "AIRA-9", tableRow{ID: "AIRA-9"})
	action, _ = inlineActionFor(viewReady, 's')
	ready, _ = onInlineActionStart(ready, action, descriptors)
	want := core.Request{Verb: "mv", Args: map[string]any{"selector": "AIRA-9", "status": "in-progress"}}
	if ready.PaletteConfirm == nil || !reflect.DeepEqual(ready.PaletteConfirm.Request, want) {
		t.Fatalf("ready fixed status request=%#v want=%#v", ready.PaletteConfirm, want)
	}
}

func TestInlineLeaseReleaseAndHeartbeatFailClosedWithoutTokenSnapshot(t *testing.T) {
	for _, key := range []rune{'k', 'b'} {
		state := inlineReadyState(viewLeases, "AIRA-4", tableRow{ID: "AIRA-4", LeaseVersion: 17})
		action, _ := inlineActionFor(viewLeases, key)
		state, commands := onInlineActionStart(state, action, core.New(nil).DispatchDescriptors())
		if len(commands) != 0 || state.PaletteConfirm != nil || state.InlineAction != nil || !state.PaletteOpen || !strings.Contains(state.InlineError, "LEASE_TOKEN") {
			t.Fatalf("key %q did not fail closed: state=%#v commands=%#v", key, state, commands)
		}
	}
}

type inlineRouteRecorder struct {
	dispatchCalls int
	paletteCalls  int
	attempt       paletteDispatchAttempt
}

func (r *inlineRouteRecorder) Dispatch(context.Context, daemon.WorktreeScope, core.Request) core.Response {
	r.dispatchCalls++
	return core.Response{Code: "E_WRONG_ROUTE"}
}

func (r *inlineRouteRecorder) DispatchPalette(context.Context, daemon.WorktreeScope, core.Request) paletteDispatchAttempt {
	r.paletteCalls++
	return r.attempt
}

// verifies: inline actions inherit the v2 palette transport classifier exactly:
// definite daemon rejections skip refresh, while committed-then-lost is unknown
// and forces source-of-truth refresh. They never use the read Dispatch seam.
func TestInlineDispatchUsesPaletteClassifierForRejectedDeletedAndUnknown(t *testing.T) {
	descriptors := core.New(nil).DispatchDescriptors()
	for _, test := range []struct {
		name    string
		attempt paletteDispatchAttempt
		want    paletteOutcome
		refresh bool
	}{
		{name: "illegal transition", attempt: paletteDispatchAttempt{Response: core.Response{Code: "E_TRANSITION_INVALID", Error: "illegal"}, Send: paletteSendMayHaveBeenSent}, want: paletteRejected},
		{name: "deleted id", attempt: paletteDispatchAttempt{Response: core.Response{Code: "E_NOT_FOUND", Error: "gone"}, Send: paletteSendMayHaveBeenSent}, want: paletteRejected},
		{name: "committed then lost", attempt: paletteDispatchAttempt{Err: context.DeadlineExceeded, Send: paletteSendMayHaveBeenSent}, want: paletteOutcomeUnknown, refresh: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			state := inlineReadyState(viewTickets, "AIRA-7", tableRow{ID: "AIRA-7"})
			action, _ := inlineActionFor(viewTickets, 's')
			state, _ = onInlineActionStart(state, action, descriptors)
			state, _ = onInlineActionPick(state, "done", descriptors)
			state, commands := onPaletteConfirm(state)
			if len(commands) != 1 || commands[0].Kind != cmdPalette {
				t.Fatalf("inline did not emit cmdPalette: %#v", commands)
			}
			recorder := &inlineRouteRecorder{attempt: test.attempt}
			result := executePaletteRequest(context.Background(), recorder, daemon.WorktreeScope{}, *commands[0].Palette)
			if result.Outcome != test.want || recorder.paletteCalls != 1 || recorder.dispatchCalls != 0 {
				t.Fatalf("result=%#v routes dispatch=%d palette=%d", result, recorder.dispatchCalls, recorder.paletteCalls)
			}
			state, refresh := onPaletteResult(state, result.Outcome, descriptors)
			if got := len(refresh) > 0; got != test.refresh {
				t.Fatalf("refresh=%#v want refresh=%v state=%#v", refresh, test.refresh, state)
			}
			if test.want == paletteRejected && !strings.Contains(result.Text, "REJECTED") {
				t.Fatalf("rejection text=%q", result.Text)
			}
		})
	}
}

func TestInlineActionBindingsAndEmptySelectionGate(t *testing.T) {
	want := map[tuiView]map[rune]string{
		viewTickets:  {'s': "mv", 'h': "set", 'v': "set"},
		viewFindings: {'d': "find"},
		viewLeases:   {'c': "claim", 'k': "release", 'b': "heartbeat"},
		viewReady:    {'c': "claim", 's': "mv"},
	}
	for view, keys := range want {
		for key, verb := range keys {
			action, ok := inlineActionFor(view, key)
			if !ok || action.Verb != verb {
				t.Fatalf("binding %s/%q=%#v ok=%v", view, key, action, ok)
			}
		}
	}
	state := newTUIState(8)
	state.Active = viewTickets
	state, commands := onTUIKey(state, 's', core.New(nil).DispatchDescriptors())
	if len(commands) != 0 || state.InlineAction != nil || state.PaletteOpen {
		t.Fatalf("empty selection opened inline action: state=%#v commands=%#v", state, commands)
	}
}
