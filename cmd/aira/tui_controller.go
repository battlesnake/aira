package main

import (
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
	Cells []string
	ID    string
	Style string
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
	Active            tuiView
	Panels            map[tuiView]panelState
	Cursor            int64
	Events            []store.WatchEvent
	EventRingCapacity int
	PendingRefresh    map[tuiView]bool
	PaletteOpen       bool
	ShuttingDown      bool
	ReconnectAttempt  int
}

type tuiCmdKind uint8

const (
	cmdFetch tuiCmdKind = iota + 1
	cmdScheduleRefresh
	cmdReconnect
	cmdQuit
	cmdPalette // executor-only; never emitted by a controller transition
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

func onTUIKey(state tuiState, key rune) (tuiState, []tuiCmd) {
	state = cloneTUIState(state)
	switch key {
	case 'q':
		state.ShuttingDown = true
		return state, []tuiCmd{{Kind: cmdQuit}}
	case 'r':
		return requestPanelRefresh(state, state.Active)
	case ':':
		state.PaletteOpen = true
		return state, nil
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
