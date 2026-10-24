package main

import (
	"fmt"
	"strconv"
	"strings"

	"aira/internal/core"
	"aira/internal/runner"
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
	// QueuedRow is the ROW KIND this action targets (AIRA-309). False — the
	// default every action gets by omission — means LIVE rows only; true means
	// queued (`aira top` waiting-job) rows only. onInlineActionStart refuses a
	// mismatch, so a queued row can never become the target of an action written
	// for running jobs: AIRA-308's "a future row action must guard on an empty ID"
	// became "an action targets queued rows only by opting in".
	QueuedRow bool
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
	// Summary is the target row's ConfirmText captured AT THE KEYPRESS
	// (AIRA-312): `aira top` refreshes the table every tick, so the job's
	// reserve and live usage shown on the confirmation page are the figures the
	// operator acted on, not a later tick's. Display only.
	Summary string
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
	case viewTop:
		// AIRA-309. The only queued-row action: push the waiting job to the front
		// of the admission queue, behind a confirmation (topFrontConfirm).
		// AIRA-312. `e` (edit reserve) is a LIVE-row action: lower a running job's
		// admission reserve, the new size typed into the inline mini-form, then a
		// confirmation (topResizeConfirm). Not `r`: that is the global refresh.
		switch key {
		case 'f':
			action = inlineAction{Key: key, Label: "push to front", Verb: "confine-front", Safety: core.SafetyExecute, ValueSource: inlineValueFixed, QueuedRow: true}
		case 'e':
			action = inlineAction{Key: key, Label: "lower reserve", Verb: "confine-resize", Safety: core.SafetyExecute, ValueSource: inlineValueFixed}
		}
	}
	return action, action.Verb != ""
}

// topFrontConfirm builds the confirmation page for `aira top`'s `f` key
// (AIRA-309) directly, rather than through the palette: confine-front is a
// client-routed SafetyExecute verb, which the palette deliberately never admits
// (paletteOperationAdmitted), and it addresses the machine-wide slice rather
// than a project.
//
// The push is sent as state.ConfineOwner (the CLI's owner chain, resolved at
// startup), so the daemon's audit line names the pusher. It sends steal=false
// only for the caller's OWN job under an attested identity, exactly the case the
// shared guard passes without --steal; every other push (another session's job,
// an inferred "@cwd-…" or unknown caller) sends steal=true, and the page says so
// in plain words. That page is the explicit steal acknowledgement a human gives
// in `top`; the CLI and MCP still need --steal for another session's job.
func topFrontConfirm(state tuiState, row tableRow) (tuiState, []tuiCmd) {
	caller := topConfineCaller(state)
	own, clause := topOwnerCheckClause(caller, row.ID, "push")
	request := core.Request{Verb: "confine-front", Args: map[string]any{
		"selector": row.ID, "steal": !own,
		"slice": runner.ResolveConfineSlice(""), "owner": caller,
	}}
	summary := row.ConfirmText
	if summary == "" {
		summary = "push " + row.ID + " to the front of the admission queue"
	}
	summary += ". It then holds every job behind it until it fits. " + clause
	state.PaletteOpen = true
	state.PaletteConfirm = &paletteConfirm{Request: request, Summary: summary, Safety: core.SafetyExecute, Verb: "confine-front"}
	return state, nil
}

// topConfineCaller is the identity a confine write from `aira top` is sent as:
// the CLI's owner chain resolved at startup, or the unknown marker.
func topConfineCaller(state tuiState) string {
	if state.ConfineOwner == "" {
		return runner.ConfineUnknownOwner
	}
	return state.ConfineOwner
}

// topOwnerCheckClause is the ONE rule `aira top`'s confine writes (`f` push,
// `e` resize) apply to ownership: steal=false (own) only for the caller's OWN
// job under an attested identity, exactly the case the shared daemon guard
// passes without --steal; every other write sends steal=true, and the returned
// sentence says so in plain words. action names the write in the
// logged-as-yours clause.
func topOwnerCheckClause(caller, scopeID, action string) (bool, string) {
	jobOwner := ""
	if _, _, _, parsed, ok := runner.ParseConfineScopeID(scopeID); ok {
		jobOwner = parsed
	}
	owner := jobOwner
	if owner == "" {
		owner = "an unknown owner"
	}
	own := runner.ConfineOwnerIsAttested(caller) && caller == jobOwner
	switch {
	case own:
		return true, "It is your job (" + caller + ")."
	case caller == runner.ConfineUnknownOwner:
		return false, "aira top has no owner identity; confirming overrides the owner check (--steal) for " + owner + "'s job."
	default:
		return false, "You are " + caller + "; confirming overrides the owner check (--steal) for " + owner + "'s job, and the " + action + " is logged as yours."
	}
}

// topResizeMemoryArg is the one mini-form field `e` asks for: the new reserve,
// sent as the confine-resize verb's memory_reserve argument.
const topResizeMemoryArg = "memory_reserve"

// topResizeConfirm builds the confirmation page for `aira top`'s `e` key
// (AIRA-312) once the new size has been typed into the inline mini-form. It is
// built directly, not through the palette, for the reason topFrontConfirm is:
// confine-resize is a client-routed SafetyExecute verb the palette never admits.
//
// The size is validated here by the parser the CLI and the daemon apply
// (runner.ParseConfineResizeReserve), so a typo is refused before any page
// claims a request; the daemon still decides "smaller than now" and "not below
// live usage" at the moment of the resize. The page names the job, its owner,
// current reserve and live usage as of the keypress, the proposed size, what a
// resize does NOT do, and the --steal acknowledgement, exactly as
// topFrontConfirm states it.
func topResizeConfirm(state tuiState, raw string) (tuiState, []tuiCmd) {
	pending := state.InlineAction
	raw = strings.TrimSpace(raw)
	if raw == "" {
		state.InlineError = "E_CONFINE_ARGUMENT_INVALID: lower reserve requires the new reserve, e.g. 2G"
		return state, nil
	}
	size, err := runner.ParseConfineResizeReserve(raw)
	if err != nil {
		state.InlineError = "E_CONFINE_ARGUMENT_INVALID: new reserve " + strconv.Quote(raw) + ": " + err.Error()
		return state, nil
	}
	caller := topConfineCaller(state)
	own, clause := topOwnerCheckClause(caller, pending.TargetID, "resize")
	request := core.Request{Verb: "confine-resize", Args: map[string]any{
		"selector": pending.TargetID, topResizeMemoryArg: raw, "steal": !own,
		"slice": runner.ResolveConfineSlice(""), "owner": caller,
	}}
	job := pending.Summary
	if job == "" {
		job = pending.TargetID
	}
	summary := fmt.Sprintf("lower the admission reserve of %s to %s (%d bytes). ", job, raw, size) +
		"This edits the admission ledger only: the job's memory.max cap is unchanged; its OOM-steering budget drops to the new reserve, so an overrun past it makes the job the preferred OOM victim under pressure; and any re-declare (a daemon reconnect or restart) restores the original reserve. " +
		"The daemon refuses a size that is not smaller than the current reserve or is below the job's live memory use at that moment. " + clause
	pending.Stage = inlineStageConfirm
	state.InlineError = ""
	state.PaletteOpen = true
	state.PaletteConfirm = &paletteConfirm{Request: request, Summary: summary, Safety: core.SafetyExecute, Verb: "confine-resize"}
	return state, nil
}

// inlineMiniFormTitle and inlineMiniFormLabel word the inline mini-form for
// the action that opened it: `find set waived`'s reason/actor, or `aira top`'s
// `e` new-reserve field (AIRA-312).
func inlineMiniFormTitle(pending *inlineActionState) string {
	if pending.Action.Verb == "confine-resize" {
		job := pending.Summary
		if job == "" {
			job = pending.TargetID
		}
		return " lower reserve: " + job + " "
	}
	return " waive " + pending.TargetID + " "
}

func inlineMiniFormLabel(pending *inlineActionState, name string) string {
	if pending.Action.Verb == "confine-resize" && name == topResizeMemoryArg {
		return "new reserve, e.g. 2G *: "
	}
	return name + " *: "
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
	// AIRA-309 row-kind default-deny: an action reaches a queued row only if it
	// opted in, and a queued-row action never reaches a live one. Refused
	// visibly, never silently: the selected job may have been admitted between
	// selection and keypress, and the operator must see why nothing happened.
	if row.Queued != action.QueuedRow {
		kind := "running (admitted) jobs"
		if action.QueuedRow {
			kind = "jobs still waiting in the admission queue"
		}
		state.InlineError = fmt.Sprintf("E_TUI_INLINE_ROW_KIND: %s applies only to %s", action.Label, kind)
		state.InlineAction = nil
		return state, nil
	}
	pending := &inlineActionState{
		Action: action, TargetID: panel.SelectedID, LeaseToken: row.LeaseToken,
		LeaseVersion: row.LeaseVersion, Values: map[string]string{"selector": panel.SelectedID},
	}
	if action.Verb == "confine-front" {
		state.InlineError = ""
		state.InlineAction = pending
		pending.Stage = inlineStageConfirm
		return topFrontConfirm(state, row)
	}
	if action.Verb == "confine-resize" {
		// AIRA-312. The new size is typed into the existing inline mini-form (the
		// one `find set waived` uses for its reason/actor); its submit builds the
		// confirmation (onInlineMiniFormSubmit -> topResizeConfirm). The row's
		// description is captured now, at the keypress.
		state.InlineError = ""
		state.PaletteOpen = true
		pending.Summary = row.ConfirmText
		pending.Stage = inlineStageMiniForm
		pending.FormArgs = []string{topResizeMemoryArg}
		state.InlineAction = pending
		return state, nil
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
	if pending.Action.Verb == "confine-resize" {
		return topResizeConfirm(state, values[topResizeMemoryArg])
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
