package main

import (
	"regexp"
	"time"

	"aira/internal/core"
	"aira/internal/store"
)

type tuiView string

const (
	viewTickets  tuiView = "tickets"
	viewReady    tuiView = "ready"
	viewLeases   tuiView = "leases"
	viewFindings tuiView = "findings"
	viewInsights tuiView = "insights"
	viewEvents   tuiView = "events"
)

var (
	allViews  = []tuiView{viewTickets, viewReady, viewLeases, viewFindings, viewInsights, viewEvents}
	dataViews = []tuiView{viewTickets, viewReady, viewLeases, viewFindings, viewInsights}
)

type panelStatus string

const (
	panelLoading panelStatus = "loading"
	panelReady   panelStatus = "ready"
	panelError   panelStatus = "error"
)

type tableRow struct {
	Cells        []string
	ID           string
	Style        string
	Hold         bool
	LeaseToken   string
	LeaseVersion int64
}

type gaugeTile struct {
	Name        string
	Value       string
	Direction   string
	Baseline    string
	Unevaluated bool
	Reason      string
	ErrorCode   string
}

type panelModel struct {
	Headers []string
	Rows    []tableRow
	Footer  string
	Detail  string
	Tiles   []gaugeTile
}

type panelState struct {
	Generation         int
	Status             panelStatus
	Model              panelModel
	ErrorCode          string
	InFlight           bool
	InFlightGeneration int
	Dirty              bool
	SelectedID         string
}

type tuiState struct {
	Active                tuiView
	Panels                map[tuiView]panelState
	Cursor                int64
	Events                []store.WatchEvent
	EventRingCapacity     int
	PendingRefresh        map[tuiView]bool
	PaletteOpen           bool
	PaletteConfirm        *paletteConfirm
	PaletteDispatching    bool
	PaletteDispatchedVerb string
	InlineAction          *inlineActionState
	InlineError           string
	CanExecute            bool
	ExecuteOpen           bool
	ExecuteSelected       *executeEntry
	ExecuteConfirm        *executeLaunch
	ExecuteRunning        bool
	ExecuteError          string
	DetachedReport        string
	ShuttingDown          bool
	ReconnectAttempt      int
}

// paletteConfirm is the immutable, resolved request snapshot displayed by the
// confirmation page. TypedID is the sole mutable confirmation buffer.
type paletteConfirm struct {
	Request              core.Request
	Summary              string
	Safety               core.SafetyClass
	Destructive          bool
	ConfirmIDTarget      string
	ConfirmBlockedReason string
	TypedID              string
	Verb                 string
}

var canonicalIDPattern = regexp.MustCompile(`^[A-Z][A-Z0-9]*-[1-9][0-9]*$`)

// isCanonicalID reports whether s is a concrete AIRA id (e.g. RANT-7, AIRA-12),
// the only form a destructive confirmation will bind its typed-id gate to.
func isCanonicalID(s string) bool { return canonicalIDPattern.MatchString(s) }

type paletteOutcome string

const (
	paletteApplied        paletteOutcome = "applied"
	paletteRejected       paletteOutcome = "rejected"
	paletteOutcomeUnknown paletteOutcome = "outcome-unknown"
)

type tuiCmdKind uint8

const (
	cmdFetch tuiCmdKind = iota + 1
	cmdScheduleRefresh
	cmdReconnect
	cmdQuit
	cmdPalette         // executor-only; never emitted by a controller transition
	cmdExecuteDetached // executor-only; never emitted by a controller transition
)

// tuiCmd contains only values (Palette is executor-only). The executor
// interprets it off the UI thread.
type tuiCmd struct {
	Kind       tuiCmdKind
	View       tuiView
	Generation int
	Backoff    time.Duration
	DetailID   string
	Palette    *core.Request
	Execute    *executeLaunch
}

type fetchResult struct {
	View       tuiView
	Generation int
	Model      panelModel
	Code       string
}

type detailResult struct {
	View       tuiView
	Generation int
	ID         string
	Detail     string
}

func newTUIState(eventCapacity int) tuiState {
	if eventCapacity < 1 {
		eventCapacity = 1
	}
	panels := make(map[tuiView]panelState, len(allViews))
	for _, view := range allViews {
		panels[view] = panelState{Status: panelLoading}
	}
	return tuiState{
		Active:            viewTickets,
		Panels:            panels,
		EventRingCapacity: eventCapacity,
		PendingRefresh:    make(map[tuiView]bool),
	}
}

func cloneTUIState(state tuiState) tuiState {
	copyState := state
	if state.PaletteConfirm != nil {
		confirm := *state.PaletteConfirm
		confirm.Request = clonePaletteRequest(confirm.Request)
		copyState.PaletteConfirm = &confirm
	}
	if state.InlineAction != nil {
		inline := *state.InlineAction
		inline.Options = append([]string(nil), state.InlineAction.Options...)
		inline.FormArgs = append([]string(nil), state.InlineAction.FormArgs...)
		inline.Values = make(map[string]string, len(state.InlineAction.Values))
		for name, value := range state.InlineAction.Values {
			inline.Values[name] = value
		}
		copyState.InlineAction = &inline
	}
	if state.ExecuteSelected != nil {
		selected := *state.ExecuteSelected
		copyState.ExecuteSelected = &selected
	}
	if state.ExecuteConfirm != nil {
		confirm := cloneExecuteLaunch(*state.ExecuteConfirm)
		copyState.ExecuteConfirm = &confirm
	}
	copyState.Panels = make(map[tuiView]panelState, len(state.Panels))
	for view, panel := range state.Panels {
		panel.Model.Headers = append([]string(nil), panel.Model.Headers...)
		panel.Model.Rows = append([]tableRow(nil), panel.Model.Rows...)
		panel.Model.Tiles = append([]gaugeTile(nil), panel.Model.Tiles...)
		copyState.Panels[view] = panel
	}
	copyState.Events = append([]store.WatchEvent(nil), state.Events...)
	copyState.PendingRefresh = make(map[tuiView]bool, len(state.PendingRefresh))
	for view, pending := range state.PendingRefresh {
		copyState.PendingRefresh[view] = pending
	}
	return copyState
}

func clonePaletteRequest(request core.Request) core.Request {
	copyRequest := request
	copyRequest.Content = append([]byte(nil), request.Content...)
	copyRequest.Args = make(map[string]any, len(request.Args))
	for name, value := range request.Args {
		copyRequest.Args[name] = clonePaletteValue(value)
	}
	if request.GitContext != nil {
		gitContext := *request.GitContext
		copyRequest.GitContext = &gitContext
	}
	return copyRequest
}

func clonePaletteValue(value any) any {
	switch value := value.(type) {
	case []string:
		return append([]string(nil), value...)
	case []byte:
		return append([]byte(nil), value...)
	case map[string]any:
		copyMap := make(map[string]any, len(value))
		for name, item := range value {
			copyMap[name] = clonePaletteValue(item)
		}
		return copyMap
	case []any:
		copySlice := make([]any, len(value))
		for index, item := range value {
			copySlice[index] = clonePaletteValue(item)
		}
		return copySlice
	default:
		return value
	}
}

func onPaletteSubmit(state tuiState, entry paletteEntry, request core.Request) (tuiState, []tuiCmd) {
	state = cloneTUIState(state)
	if state.PaletteDispatching {
		return state, nil
	}
	request = clonePaletteRequest(request)
	if entry.Safety == core.SafetyRead {
		return state, []tuiCmd{{Kind: cmdPalette, Palette: &request}}
	}
	verb := entry.Verb
	if entry.Operation != "" {
		verb += "." + entry.Operation
	}
	target, blockedReason := "", ""
	if entry.Destructive {
		// Bind the typed-id gate to a CANONICAL id, never a shorthand/alias that
		// could resolve elsewhere, and never a non-string selector (Sol build-review
		// P1). A non-canonical selector leaves the target empty AND records a reason,
		// so the confirmation is explicitly blocked, not silently un-confirmable.
		// Validate the selector EXACTLY as it will be dispatched — do not trim a
		// copy, or the gate would bind to a different value than the request carries
		// (" RANT-7 " confirmed as RANT-7 yet dispatched with spaces; Sol confirm P1).
		selector, ok := request.Args["selector"].(string)
		if ok && isCanonicalID(selector) {
			target = selector
		} else {
			blockedReason = "destructive confirmation requires a canonical id selector (e.g. RANT-7)"
		}
	}
	state.PaletteConfirm = &paletteConfirm{
		Request: request, Summary: entry.Summary, Safety: entry.Safety,
		Destructive: entry.Destructive, ConfirmIDTarget: target, ConfirmBlockedReason: blockedReason, Verb: verb,
	}
	return state, nil
}

func onPaletteConfirmTypedID(state tuiState, typed string) tuiState {
	state = cloneTUIState(state)
	if state.PaletteConfirm != nil && !state.PaletteDispatching {
		state.PaletteConfirm.TypedID = typed
	}
	return state
}

func paletteConfirmEnabled(confirm *paletteConfirm) bool {
	if confirm == nil {
		return false
	}
	return !confirm.Destructive || confirm.ConfirmIDTarget != "" && confirm.TypedID == confirm.ConfirmIDTarget
}

func onPaletteConfirm(state tuiState) (tuiState, []tuiCmd) {
	state = cloneTUIState(state)
	if state.PaletteDispatching || !paletteConfirmEnabled(state.PaletteConfirm) {
		return state, nil
	}
	request := clonePaletteRequest(state.PaletteConfirm.Request)
	state.PaletteDispatching = true
	state.PaletteDispatchedVerb = state.PaletteConfirm.Verb
	state.PaletteConfirm = nil
	return state, []tuiCmd{{Kind: cmdPalette, Palette: &request}}
}

func onPaletteCancel(state tuiState) (tuiState, []tuiCmd) {
	state = cloneTUIState(state)
	if state.PaletteDispatching {
		return state, nil
	}
	state.PaletteConfirm = nil
	state.InlineAction = nil
	state.InlineError = ""
	return state, nil
}

// onPaletteResult clears the atomic dispatch gate. A definite daemon rejection
// cannot have changed state, so it deliberately skips refresh; applied and
// outcome-unknown results force source-of-truth fetches because either may have
// committed without producing a watch event.
func onPaletteResult(state tuiState, outcome paletteOutcome, descriptors []core.DispatchDescriptor) (tuiState, []tuiCmd) {
	state = cloneTUIState(state)
	if !state.PaletteDispatching {
		return state, nil
	}
	verb := state.PaletteDispatchedVerb
	state.PaletteDispatching = false
	state.PaletteDispatchedVerb = ""
	state.InlineAction = nil
	state.InlineError = ""
	if outcome == paletteRejected {
		return state, nil
	}
	var commands []tuiCmd
	for _, view := range invalidatedViews(verb, descriptors) {
		var refresh []tuiCmd
		state, refresh = requestPanelRefresh(state, view)
		commands = append(commands, refresh...)
	}
	return state, commands
}

// requestPanelRefresh is the only transition that begins a panel fetch.
func requestPanelRefresh(state tuiState, view tuiView) (tuiState, []tuiCmd) {
	state = cloneTUIState(state)
	panel := state.Panels[view]
	if panel.InFlight {
		panel.Dirty = true
		state.Panels[view] = panel
		return state, nil
	}
	panel.Generation++
	panel.InFlight = true
	panel.InFlightGeneration = panel.Generation
	panel.Status = panelLoading
	panel.ErrorCode = ""
	state.Panels[view] = panel
	return state, []tuiCmd{{Kind: cmdFetch, View: view, Generation: panel.Generation}}
}

func onTUIKey(state tuiState, key rune, descriptors []core.DispatchDescriptor) (tuiState, []tuiCmd) {
	state = cloneTUIState(state)
	if !state.PaletteOpen && !state.PaletteDispatching && state.Panels[state.Active].SelectedID != "" {
		if action, ok := inlineActionFor(state.Active, key); ok {
			return onInlineActionStart(state, action, descriptors)
		}
	}
	switch key {
	case 'q':
		state.ShuttingDown = true
		return state, []tuiCmd{{Kind: cmdQuit}}
	case 'r':
		return requestPanelRefresh(state, state.Active)
	case ':':
		state.PaletteOpen = true
		return state, nil
	case 'x':
		if !state.PaletteOpen && !state.PaletteDispatching && !state.ExecuteRunning {
			return onExecuteOpen(state), nil
		}
	case '\t':
		for i, view := range allViews {
			if view == state.Active {
				state.Active = allViews[(i+1)%len(allViews)]
				return state, nil
			}
		}
	case '1', '2', '3', '4', '5', '6':
		state.Active = allViews[int(key-'1')]
	}
	return state, nil
}

func onTUIFetchResult(state tuiState, result fetchResult) (tuiState, []tuiCmd) {
	state = cloneTUIState(state)
	panel := state.Panels[result.View]
	if result.Generation != panel.InFlightGeneration {
		return state, nil
	}
	panel.InFlight = false
	if result.Code != "" {
		panel.Status = panelError
		panel.ErrorCode = result.Code
		panel.Model = panelModel{}
	} else {
		panel.Status = panelReady
		panel.ErrorCode = ""
		panel.Model = result.Model
	}
	dirty := panel.Dirty
	panel.Dirty = false
	state.Panels[result.View] = panel
	if dirty {
		return requestPanelRefresh(state, result.View)
	}
	return state, nil
}

func onTUISelect(state tuiState, view tuiView, id string) (tuiState, []tuiCmd) {
	state = cloneTUIState(state)
	panel := state.Panels[view]
	if id == "" || panel.SelectedID == id {
		return state, nil
	}
	panel.SelectedID = id
	state.Panels[view] = panel
	if view != viewTickets && view != viewFindings {
		return state, nil
	}
	return state, []tuiCmd{{Kind: cmdFetch, View: view, Generation: panel.Generation, DetailID: id}}
}

func onTUIDetailResult(state tuiState, result detailResult) (tuiState, []tuiCmd) {
	state = cloneTUIState(state)
	panel := state.Panels[result.View]
	if result.Generation != panel.Generation || result.ID != panel.SelectedID {
		return state, nil
	}
	panel.Model.Detail = result.Detail
	state.Panels[result.View] = panel
	return state, nil
}

func onTUIWatchBatch(state tuiState, events []store.WatchEvent, cursor int64, descriptors []core.DispatchDescriptor) (tuiState, []tuiCmd) {
	state = cloneTUIState(state)
	state.Events = append(state.Events, events...)
	if overflow := len(state.Events) - state.EventRingCapacity; overflow > 0 {
		state.Events = append([]store.WatchEvent(nil), state.Events[overflow:]...)
	}
	state.Cursor = cursor
	state.ReconnectAttempt = 0
	affected := make(map[tuiView]bool)
	for _, event := range events {
		for _, view := range invalidatedViews(event.Verb, descriptors) {
			affected[view] = true
		}
	}
	cmds := make([]tuiCmd, 0, len(affected))
	for _, view := range dataViews {
		if !affected[view] {
			continue
		}
		if state.Panels[view].InFlight {
			state, _ = requestPanelRefresh(state, view)
			continue
		}
		if state.PendingRefresh[view] {
			continue
		}
		state.PendingRefresh[view] = true
		cmds = append(cmds, tuiCmd{Kind: cmdScheduleRefresh, View: view})
	}
	return state, cmds
}

func onTUIRefreshDue(state tuiState, view tuiView) (tuiState, []tuiCmd) {
	state = cloneTUIState(state)
	delete(state.PendingRefresh, view)
	return requestPanelRefresh(state, view)
}

func onTUIEOF(state tuiState) (tuiState, []tuiCmd) {
	state = cloneTUIState(state)
	state.ReconnectAttempt++
	backoff := 50 * time.Millisecond
	for i := 1; i < state.ReconnectAttempt && backoff < 2*time.Second; i++ {
		backoff *= 2
	}
	if backoff > 2*time.Second {
		backoff = 2 * time.Second
	}
	return state, []tuiCmd{{Kind: cmdReconnect, Backoff: backoff}}
}

func invalidatedViews(verb string, descriptors []core.DispatchDescriptor) []tuiView {
	reads := make(map[string]bool)
	for _, descriptor := range descriptors {
		if len(descriptor.Operations) == 0 {
			if descriptor.Safety == core.SafetyRead {
				reads[descriptor.Name] = true
			}
			continue
		}
		for _, operation := range descriptor.Operations {
			if operation.Safety == core.SafetyRead {
				reads[descriptor.Name+"."+operation.Name] = true
			}
		}
	}
	if reads[verb] {
		return nil
	}
	return append([]tuiView(nil), dataViews...)
}
