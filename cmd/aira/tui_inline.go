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
	// Job is the target row's job captured AT THE KEYPRESS (AIRA-312/342):
	// `aira top` refreshes the table every tick, so the job's reserve and live
	// usage shown on the size form and the confirmation are the figures the
	// operator acted on, not a later tick's. Display only.
	Job *topJob
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
//
// AIRA-342: the page is worded for a person (the job, what it asked for, what a
// push does, whose check it overrides); the exact request is behind `d`.
func topFrontConfirm(state tuiState, row tableRow) (tuiState, []tuiCmd) {
	caller := topConfineCaller(state)
	own, clause := topOwnerCheckClause(caller, row.ID, "push")
	request := core.Request{Verb: "confine-front", Args: map[string]any{
		"selector": row.ID, "steal": !own,
		"slice": runner.ResolveConfineSlice(""), "owner": caller,
	}}
	var summary strings.Builder
	if job := row.Job; job != nil {
		fmt.Fprintf(&summary, "Job: %s, owner %s\n", job.Name, topJobOwner(job))
		fmt.Fprintf(&summary, "Requested: %s · listed queue position %d\n\n", topJobSize(job.Reserve), job.Position)
		fits := "it fits"
		if job.Reserve != nil {
			fits = humanSize(*job.Reserve) + " fits"
		}
		fmt.Fprintf(&summary, "Pushing it moves it to the front of the admission queue: it is admitted as soon as %s, and it holds every job behind it until then.\n", fits)
	} else {
		summary.WriteString("Job: " + row.ID + "\n\nPushing it moves it to the front of the admission queue, and it holds every job behind it until it fits.\n")
	}
	summary.WriteString(clause)
	state.PaletteOpen = true
	state.PaletteConfirm = &paletteConfirm{
		Request: request, Summary: summary.String(), Safety: core.SafetyExecute, Verb: "confine-front",
		Title: "Push to the front of the queue?", ArgsInDetails: true,
	}
	return state, nil
}

// topJobOwner is a job's owner for a dialog, never an empty string.
func topJobOwner(job *topJob) string {
	if job.Owner == "" {
		return runner.ConfineUnknownOwner
	}
	return job.Owner
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
// live usage" at the moment of the resize (the form only warns about those, from
// the keypress-time figures, because a stale figure must not refuse a request
// the daemon would accept). The page names the job, its owner, current reserve
// and live usage as of the keypress, the proposed size and its effect, what a
// resize does NOT do, and the --steal acknowledgement, exactly as
// topFrontConfirm states it.
func topResizeConfirm(state tuiState, raw string) (tuiState, []tuiCmd) {
	pending := state.InlineAction
	raw = strings.TrimSpace(raw)
	if raw == "" {
		state.InlineError = "E_CONFINE_ARGUMENT_INVALID: enter the new reserve, e.g. 2G"
		return state, nil
	}
	size, err := runner.ParseConfineResizeReserve(raw)
	if err != nil {
		state.InlineError = "E_CONFINE_ARGUMENT_INVALID: " + topResizeParseError(raw, err)
		return state, nil
	}
	caller := topConfineCaller(state)
	own, clause := topOwnerCheckClause(caller, pending.TargetID, "resize")
	request := core.Request{Verb: "confine-resize", Args: map[string]any{
		"selector": pending.TargetID, topResizeMemoryArg: raw, "steal": !own,
		"slice": runner.ResolveConfineSlice(""), "owner": caller,
	}}
	var summary strings.Builder
	job := pending.Job
	if job != nil {
		fmt.Fprintf(&summary, "Job: %s, owner %s\n", job.Name, topJobOwner(job))
		fmt.Fprintf(&summary, "Reserve: %s now, live use %s\n", topJobSize(job.Reserve), topJobSize(job.Usage))
	} else {
		summary.WriteString("Job: " + pending.TargetID + "\n")
	}
	_, effect := topResizePreview(job, raw)
	summary.WriteString("Change: " + effect + "\n\n")
	fmt.Fprintf(&summary, "Only the admission ledger changes. The job's memory.max cap is unchanged; its OOM-steering budget drops to %s, so going past that makes it the preferred OOM victim under pressure; and any re-declare (a daemon reconnect or restart) restores the original reserve. ", humanSize(size))
	summary.WriteString("The daemon refuses a size that is not smaller than the current reserve, or is below the job's live memory use at that moment.\n")
	summary.WriteString(clause)
	pending.Stage = inlineStageConfirm
	state.InlineError = ""
	state.PaletteOpen = true
	state.PaletteConfirm = &paletteConfirm{
		Request: request, Summary: summary.String(), Safety: core.SafetyExecute, Verb: "confine-resize",
		Title: "Lower this job's reserve?", ArgsInDetails: true,
	}
	return state, nil
}

// topResizeParseError words a size the shared parser refused. A bare number is
// bytes to that parser, so "2" is refused for being under 1M; say that a unit is
// what is missing rather than quoting the minimum.
func topResizeParseError(raw string, err error) string {
	if strings.Trim(raw, "0123456789") == "" {
		return strconv.Quote(raw) + " is a number of bytes: add a unit, e.g. " + raw + "G or " + raw + "M"
	}
	return "new reserve " + strconv.Quote(raw) + ": " + err.Error()
}

// dialogTone is how a form's status line reads: a neutral prompt, a value that
// is fine, one the daemon is expected to refuse, or one that cannot be sent.
type dialogTone uint8

const (
	toneInfo dialogTone = iota
	toneOK
	toneWarn
	toneError
)

// topResizePreview is the `e` size form's live status line and the
// confirmation's "Change:" line (AIRA-342): what the typed size would do to the
// job's reserve as of the keypress. A value the parser refuses is an error (it
// cannot be sent); a value the daemon is expected to refuse is only a warning,
// because the daemon decides against its own figures at the moment of the resize.
func topResizePreview(job *topJob, raw string) (dialogTone, string) {
	var current, usage *int64
	if job != nil {
		current, usage = job.Reserve, job.Usage
	}
	raw = strings.TrimSpace(raw)
	if raw == "" {
		if current != nil {
			return toneInfo, "enter a size smaller than " + humanSize(*current) + ", with a unit: 512M, 2G, 1.5G"
		}
		return toneInfo, "enter the new reserve, with a unit: 512M, 2G, 1.5G"
	}
	size, err := runner.ParseConfineResizeReserve(raw)
	if err != nil {
		return toneError, topResizeParseError(raw, err)
	}
	switch {
	case current == nil:
		return toneOK, "new reserve " + humanSize(size) + " (the current reserve is unevaluated; the daemon checks it)"
	case size >= *current:
		return toneWarn, humanSize(*current) + " → " + humanSize(size) + " is not smaller: the daemon will refuse it (a resize only lowers a reserve)"
	case usage != nil && size < *usage:
		return toneWarn, humanSize(size) + " is below the job's live use of " + humanSize(*usage) + ": the daemon will refuse it"
	default:
		return toneOK, humanSize(*current) + " → " + humanSize(size) + ", frees " + humanSize(*current-size) + " for queued jobs"
	}
}

// inlineMiniFormTitle, inlineMiniFormContext and inlineMiniFormLabel word the
// inline mini-form for the action that opened it: `find set waived`'s
// reason/actor, or `aira top`'s `e` new-reserve field (AIRA-312/342).
func inlineMiniFormTitle(pending *inlineActionState) string {
	if pending.Action.Verb == "confine-resize" {
		return "Lower reserve"
	}
	return "Waive " + pending.TargetID
}

func inlineMiniFormContext(pending *inlineActionState) string {
	if pending.Action.Verb != "confine-resize" {
		return "A waived finding needs a reason and who waived it."
	}
	job := pending.Job
	if job == nil {
		return "Job: " + pending.TargetID
	}
	return fmt.Sprintf("Job: %s, owner %s\nReserve: %s now, live use %s\nA lower reserve hands the difference back to jobs waiting in the queue.",
		job.Name, topJobOwner(job), topJobSize(job.Reserve), topJobSize(job.Usage))
}

func inlineMiniFormLabel(pending *inlineActionState, name string) string {
	if pending.Action.Verb == "confine-resize" && name == topResizeMemoryArg {
		return "New reserve: "
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
		pending.Job = row.Job
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
