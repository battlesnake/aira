package main

import (
	"fmt"
	"strconv"
	"strings"

	"aira/internal/core"
)

type inlineValueSource uint8

const (
	inlineValueFixed inlineValueSource = iota + 1
	inlineValueEnum
	inlineValueBoolToggle
)

type inlineStage uint8

const (
	inlineStagePicker inlineStage = iota + 1
	inlineStageMiniForm
	inlineStageConfirm
)

type inlineAction struct {
	Key         rune
	Label       string
	Verb        string
	Operation   string
	Safety      core.SafetyClass
	Destructive bool
	ValueSource inlineValueSource
	ValueArg    string
	EnumVerb    string
	EnumArg     string
	Field       string
	Fixed       string
}

type inlineActionState struct {
	Action       inlineAction
	TargetID     string
	LeaseToken   string
	LeaseVersion int64
	Options      []string
	FormArgs     []string
	Values       map[string]string
	Stage        inlineStage
}

// inlineActionFor is the closed, panel-scoped row-action table. It describes
// only face affordances; validation and dispatch remain in the palette path.
func inlineActionFor(view tuiView, key rune) (inlineAction, bool) {
	var action inlineAction
	switch view {
	case viewTickets:
		switch key {
		case 's':
			action = inlineAction{Key: key, Label: "move status", Verb: "mv", Safety: core.SafetyMutate, ValueSource: inlineValueEnum, ValueArg: "status", EnumVerb: "mv", EnumArg: "status"}
		case 'h':
			action = inlineAction{Key: key, Label: "toggle hold", Verb: "set", Safety: core.SafetyMutate, ValueSource: inlineValueBoolToggle, ValueArg: "value", Field: "hold"}
		case 'v':
			action = inlineAction{Key: key, Label: "set severity", Verb: "set", Safety: core.SafetyMutate, ValueSource: inlineValueEnum, ValueArg: "value", EnumVerb: "create", EnumArg: "severity", Field: "severity"}
		}
	case viewFindings:
		if key == 'd' {
			action = inlineAction{Key: key, Label: "set disposition", Verb: "find", Operation: "set", Safety: core.SafetyMutate, ValueSource: inlineValueEnum, ValueArg: "disposition", EnumVerb: "find", EnumArg: "disposition"}
		}
	case viewLeases:
		switch key {
		case 'c':
			action = inlineAction{Key: key, Label: "claim", Verb: "claim", Safety: core.SafetyLease, ValueSource: inlineValueFixed}
		case 'k':
			action = inlineAction{Key: key, Label: "release", Verb: "release", Safety: core.SafetyLease, ValueSource: inlineValueFixed}
		case 'b':
			action = inlineAction{Key: key, Label: "heartbeat", Verb: "heartbeat", Safety: core.SafetyLease, ValueSource: inlineValueFixed}
		}
	case viewReady:
		switch key {
		case 'c':
			action = inlineAction{Key: key, Label: "claim", Verb: "claim", Safety: core.SafetyLease, ValueSource: inlineValueFixed}
		case 's':
			action = inlineAction{Key: key, Label: "start work", Verb: "mv", Safety: core.SafetyMutate, ValueSource: inlineValueFixed, ValueArg: "status", Fixed: "in-progress"}
		}
	}
	return action, action.Verb != ""
}

func onInlineActionStart(state tuiState, action inlineAction, descriptors []core.DispatchDescriptor) (tuiState, []tuiCmd) {
	state = cloneTUIState(state)
	if state.PaletteDispatching || state.PaletteOpen || action.Verb == "" {
		return state, nil
	}
	panel := state.Panels[state.Active]
	if panel.SelectedID == "" {
		return state, nil
	}
	row, ok := inlineSelectedRow(panel, panel.SelectedID)
	if !ok {
		return state, nil
	}
	pending := &inlineActionState{
		Action: action, TargetID: panel.SelectedID, LeaseToken: row.LeaseToken,
		LeaseVersion: row.LeaseVersion, Values: map[string]string{"selector": panel.SelectedID},
	}
	state.PaletteOpen = true
	state.InlineError = ""
	if action.Field != "" {
		pending.Values["field"] = action.Field
	}
	if (action.Verb == "release" || action.Verb == "heartbeat") && pending.LeaseToken != "" {
		pending.Values["token"] = pending.LeaseToken
	}
	if (action.Verb == "release" || action.Verb == "heartbeat") && pending.LeaseToken == "" {
		state.InlineError = "E_TUI_INLINE_LEASE_TOKEN: lease token snapshot is unavailable"
		state.InlineAction = nil
		return state, nil
	}
	switch action.ValueSource {
	case inlineValueEnum:
		options := inlineDescriptorEnum(descriptors, action.EnumVerb, action.EnumArg)
		if len(options) == 0 {
			state.InlineError = fmt.Sprintf("E_TUI_INLINE_ENUM: %s.%s has no enum in the live descriptor", action.EnumVerb, action.EnumArg)
			state.InlineAction = nil
			return state, nil
		}
		pending.Options = options
		pending.Stage = inlineStagePicker
		state.InlineAction = pending
		return state, nil
	case inlineValueBoolToggle:
		pending.Values[action.ValueArg] = strconv.FormatBool(!row.Hold)
	case inlineValueFixed:
		if action.ValueArg != "" {
			pending.Values[action.ValueArg] = action.Fixed
		}
	}
	state.InlineAction = pending
	return submitInlineAction(state, descriptors)
}

func onInlineActionPick(state tuiState, value string, descriptors []core.DispatchDescriptor) (tuiState, []tuiCmd) {
	state = cloneTUIState(state)
	pending := state.InlineAction
	if pending == nil || pending.Stage != inlineStagePicker || !containsString(pending.Options, value) {
		return state, nil
	}
	pending.Values[pending.Action.ValueArg] = value
	if pending.Action.Verb == "find" && pending.Action.Operation == "set" && value == "waived" {
		if !inlineDescriptorArgsPresent(descriptors, "find", "reason", "actor") {
			state.InlineError = "E_TUI_INLINE_FORM: find set reason/actor arguments are absent from the live descriptor"
			state.InlineAction = nil
			return state, nil
		}
		pending.Stage = inlineStageMiniForm
		pending.FormArgs = []string{"reason", "actor"}
		return state, nil
	}
	return submitInlineAction(state, descriptors)
}

func onInlineMiniFormSubmit(state tuiState, values map[string]string, descriptors []core.DispatchDescriptor) (tuiState, []tuiCmd) {
	state = cloneTUIState(state)
	pending := state.InlineAction
	if pending == nil || pending.Stage != inlineStageMiniForm {
		return state, nil
	}
	for _, name := range pending.FormArgs {
		value := values[name]
		if strings.TrimSpace(value) == "" {
			state.InlineError = fmt.Sprintf("E_SELECTOR_INVALID: waived disposition requires %s", name)
			return state, nil
		}
		pending.Values[name] = value
	}
	return submitInlineAction(state, descriptors)
}

func onInlineActionCancel(state tuiState) (tuiState, []tuiCmd) {
	state = cloneTUIState(state)
	if state.PaletteDispatching {
		return state, nil
	}
	state.InlineAction = nil
	state.InlineError = ""
	state.PaletteConfirm = nil
	state.PaletteOpen = false
	return state, nil
}

func submitInlineAction(state tuiState, descriptors []core.DispatchDescriptor) (tuiState, []tuiCmd) {
	pending := state.InlineAction
	if pending == nil {
		return state, nil
	}
	entry, ok := inlinePaletteEntry(descriptors, pending.Action.Verb, pending.Action.Operation)
	if !ok || entry.Safety != pending.Action.Safety || entry.Destructive != pending.Action.Destructive {
		state.InlineError = fmt.Sprintf("E_TUI_INLINE_DESCRIPTOR: %s is unavailable", entryKey(paletteEntry{Verb: pending.Action.Verb, Operation: pending.Action.Operation}))
		state.InlineAction = nil
		return state, nil
	}
	request, err := parsePaletteRequest(entry, pending.Values)
	if err != nil {
		state.InlineError = err.Error()
		return state, nil
	}
	// parsePaletteRequest normalises free-form palette text. Row actions bind to
	// the exact immutable row identity/token captured at action start.
	request.Args["selector"] = pending.TargetID
	if pending.LeaseToken != "" && (pending.Action.Verb == "release" || pending.Action.Verb == "heartbeat") {
		request.Args["token"] = pending.LeaseToken
	}
	pending.Stage = inlineStageConfirm
	state, commands := onPaletteSubmit(state, entry, request)
	state.PaletteOpen = true
	return state, commands
}

func inlineSelectedRow(panel panelState, id string) (tableRow, bool) {
	for _, row := range panel.Model.Rows {
		if row.ID == id {
			return row, true
		}
	}
	return tableRow{}, false
}

func inlineDescriptorEnum(descriptors []core.DispatchDescriptor, verb, argName string) []string {
	matches := 0
	var options []string
	for _, descriptor := range descriptors {
		if descriptor.Name != verb {
			continue
		}
		for _, arg := range descriptor.Args {
			if arg.Name == argName {
				matches++
				options = append([]string(nil), arg.Enum...)
			}
		}
	}
	if matches != 1 || len(options) == 0 {
		return nil
	}
	return options
}

func inlineDescriptorArgsPresent(descriptors []core.DispatchDescriptor, verb string, names ...string) bool {
	want := make(map[string]bool, len(names))
	for _, name := range names {
		want[name] = false
	}
	for _, descriptor := range descriptors {
		if descriptor.Name != verb {
			continue
		}
		for _, arg := range descriptor.Args {
			if _, ok := want[arg.Name]; ok {
				want[arg.Name] = true
			}
		}
		break
	}
	for _, present := range want {
		if !present {
			return false
		}
	}
	return true
}

func inlinePaletteEntry(descriptors []core.DispatchDescriptor, verb, operation string) (paletteEntry, bool) {
	for _, entry := range buildPalette(descriptors) {
		if entry.Verb == verb && entry.Operation == operation {
			return entry, true
		}
	}
	return paletteEntry{}, false
}
