package main

// AIRA-252 `aira board` — the all-projects overview reducer (spec §11, §12,
// Increment 2).
//
// The overview's interactive state (the project card list, the selection, the
// card-filter search, the lazily-fetched per-project data, and the mode
// transition into a chosen project) lives here as a value that pure functions
// transform, mirroring the board's Elm discipline. The runtime shell
// (tui_board_overview.go) is a thin imperative face over these transitions.

import (
	"aira/internal/daemon"
	"aira/internal/runner"
)

// overviewState is the overview runtime's whole reducer state.
type overviewState struct {
	// Cards is the static display skeleton (one per project group), rebuilt on
	// every list refresh. Data is the DYNAMIC per-project lazy result, keyed by
	// ProjectID so it SURVIVES a list rebuild (a refreshed registry must not drop
	// a card's already-loaded distribution). Groups is kept for jobs correlation.
	Cards    []overviewCard
	Data     map[string]overviewCardData
	Groups   []overviewGroup
	Jobs     []boardSessionRow
	Selected int
	Search   overviewSearchState
	HasData  bool
	Stale    bool
	// ErrorCode is the last list-fetch failure (kept as a banner while last-good
	// cards stay on screen, spec §14). RegistryCode is the pure-read registry
	// failure specifically (a torn/corrupt registry.jsonl).
	ErrorCode    string
	RegistryCode string
	Warnings     []string
	// Chosen is set (with cmdQuit) when the operator opens a project row: the
	// outer runBoard loop reads it after the runtime tears down and switches into
	// that project's board (spec §12). Quit ends the whole loop.
	Chosen *daemon.WorktreeScope
	Quit   bool
}

type overviewSearchState struct {
	Active bool
	Query  string
}

func newOverviewState() *overviewState {
	return &overviewState{Data: map[string]overviewCardData{}}
}

func cloneOverviewState(source *overviewState) *overviewState {
	if source == nil {
		return nil
	}
	clone := *source
	clone.Cards = append([]overviewCard(nil), source.Cards...)
	clone.Groups = append([]overviewGroup(nil), source.Groups...)
	clone.Jobs = append([]boardSessionRow(nil), source.Jobs...)
	clone.Warnings = append([]string(nil), source.Warnings...)
	clone.Data = make(map[string]overviewCardData, len(source.Data))
	for id, data := range source.Data {
		clone.Data[id] = data
	}
	if source.Chosen != nil {
		chosen := *source.Chosen
		clone.Chosen = &chosen
	}
	return &clone
}

// overviewVisibleCards is the filtered card slice under the current search — the
// SAME set the render draws and every selection/open transition indexes into,
// so a filtered selection can never open a card that is not on screen.
func overviewVisibleCards(state overviewState) []overviewCard {
	if !state.Search.Active || state.Search.Query == "" {
		return state.Cards
	}
	visible := make([]overviewCard, 0, len(state.Cards))
	for _, card := range state.Cards {
		if overviewMatches(card, state.Search.Query) {
			visible = append(visible, card)
		}
	}
	return visible
}

// overviewApplyList stores a freshly-fetched card list and reconciles the
// selection into the new (filtered) bounds. Already-loaded per-project Data is
// preserved by ProjectID; a project that has vanished drops its stale Data so
// the map cannot grow without bound across refreshes.
func overviewApplyList(state overviewState, data overviewListData) overviewState {
	cards := buildOverviewCards(data)
	state.Cards = cards
	state.Groups = data.Groups
	state.HasData = true
	state.Stale = false
	state.ErrorCode = ""
	state.RegistryCode = data.RegistryCode
	state.Warnings = append([]string(nil), data.Warnings...)

	live := map[string]bool{}
	for _, card := range cards {
		live[card.ProjectID] = true
	}
	for id := range state.Data {
		if !live[id] {
			delete(state.Data, id)
		}
	}
	// The jobs strip is machine-wide confine, correlated to owning projects.
	state.Jobs = overviewJobsRows(data.Confine, data.ConfineCode, overviewOwnerProject(cards, data.Groups))
	state.Selected = clampIndex(state.Selected, len(overviewVisibleCards(state)))
	return state
}

// overviewApplyJobs refreshes ONLY the machine-wide jobs strip (the confine
// tick), leaving the cards untouched (spec §13: the jobs tick is separate from
// the heavier registry/Discover list fetch).
func overviewApplyJobs(state overviewState, confine *runner.ConfineListResult, confineCode string) overviewState {
	state.Jobs = overviewJobsRows(confine, confineCode, overviewOwnerProject(state.Cards, state.Groups))
	return state
}

// overviewApplyCard merges one project's lazily-fetched count/lease result into
// the Data map (spec §11.6). An E_NOT_ADOPTED dispatch code marks the project
// EJECTED (its registry entries persist, §11.5); any other read failure is
// UNEVALUATED, never a fabricated "0" distribution (§14).
func overviewApplyCard(state overviewState, result overviewCardResult) overviewState {
	if result.ProjectID == "" {
		return state
	}
	data := overviewCardData{Loaded: true}
	switch {
	case result.Code == "E_NOT_ADOPTED":
		data.Ejected = true
	case result.Code != "":
		data.Code = result.Code
	default:
		data.Distribution = result.Distribution
		data.Total = result.Total
		if result.LeaseCode == "E_NOT_ADOPTED" {
			data.Ejected = true
		} else if result.LeaseCode != "" {
			data.LeaseCode = result.LeaseCode
		} else {
			data.LeaseCount, data.LeaseKnown = result.LeaseCount, true
		}
	}
	if state.Data == nil {
		state.Data = map[string]overviewCardData{}
	}
	state.Data[result.ProjectID] = data
	return state
}

// overviewApplyError keeps the last-good cards and marks them stale rather than
// wiping the overview on a transient list-read failure (spec §14).
func overviewApplyError(state overviewState, code string) overviewState {
	state.Stale = true
	state.ErrorCode = code
	return state
}

// overviewSelectedCard is the card under the cursor in the FILTERED list, or a
// zero card with ok==false.
func overviewSelectedCard(state overviewState) (overviewCard, bool) {
	visible := overviewVisibleCards(state)
	if state.Selected < 0 || state.Selected >= len(visible) {
		return overviewCard{}, false
	}
	return visible[state.Selected], true
}

// overviewCardNeedsFetch reports whether an available card's lazy count/lease
// has not yet been dispatched — the laziness gate (spec §11.6): fetch only the
// focused card, never eagerly all N on open.
func overviewCardNeedsFetch(state overviewState, card overviewCard) bool {
	if card.State != "available" || !card.HasScope {
		return false
	}
	_, present := state.Data[card.ProjectID]
	return !present
}

// overviewAction is the normalised overview keypress the runtime maps every key
// into, so the pure reducer never sees a raw tcell key.
type overviewAction int

const (
	overviewActNone overviewAction = iota
	overviewActUp
	overviewActDown
	overviewActOpen
	overviewActRefresh
	overviewActQuit
	overviewActClearSearch
)

// onOverviewAction is the overview reducer. Navigation emits a LAZY per-card
// fetch for the newly-focused available card if its data has not arrived
// (spec §11.6). Open sets the chosen project's scope + quits so the outer loop
// switches into its board (spec §12); it is a no-op on an unavailable/ejected
// row (no scope to open).
func onOverviewAction(state tuiState, action overviewAction) (tuiState, []tuiCmd) {
	state = cloneTUIState(state)
	if state.Overview == nil {
		return state, nil
	}
	switch action {
	case overviewActQuit:
		state.Overview.Quit = true
		state.ShuttingDown = true
		return state, []tuiCmd{{Kind: cmdQuit}}
	case overviewActRefresh:
		return overviewRefreshAll(state)
	case overviewActClearSearch:
		state.Overview.Search = overviewSearchState{}
		state.Overview.Selected = 0
		return state, nil
	case overviewActUp:
		*state.Overview = overviewMove(*state.Overview, -1)
	case overviewActDown:
		*state.Overview = overviewMove(*state.Overview, +1)
	case overviewActOpen:
		card, ok := overviewSelectedCard(*state.Overview)
		if !ok || card.State != "available" || !card.HasScope {
			return state, nil
		}
		if data, present := state.Overview.Data[card.ProjectID]; present && (data.Ejected) {
			return state, nil // an ejected project has no board to open
		}
		scope := card.Scope
		state.Overview.Chosen = &scope
		state.ShuttingDown = true
		return state, []tuiCmd{{Kind: cmdQuit}}
	}
	return overviewLazyFetchFocused(state)
}

// overviewMove clamps the selection into the filtered card bounds.
func overviewMove(state overviewState, delta int) overviewState {
	visible := overviewVisibleCards(state)
	state.Selected = clampIndex(state.Selected+delta, len(visible))
	return state
}

// overviewLazyFetchFocused emits a per-card count/lease fetch for the focused
// available card when its data has not arrived. The fetch view is a DYNAMIC
// tuiView carrying the project root (spec §12 / the viewTop precedent): a plain
// cmdFetch with a non-constant View reaches fetchTUIView untouched, so no
// executor change is needed.
func overviewLazyFetchFocused(state tuiState) (tuiState, []tuiCmd) {
	card, ok := overviewSelectedCard(*state.Overview)
	if !ok || !overviewCardNeedsFetch(*state.Overview, card) {
		return state, nil
	}
	return requestPanelRefresh(state, overviewCardView(card.CanonicalRoot))
}

// overviewRefreshAll re-fetches the list, the jobs strip, and every
// ALREADY-LOADED card (leaving unloaded cards lazy), on the operator's `r`.
func overviewRefreshAll(state tuiState) (tuiState, []tuiCmd) {
	var commands []tuiCmd
	state, cmds := requestPanelRefresh(state, viewOverview)
	commands = append(commands, cmds...)
	state, cmds = requestPanelRefresh(state, viewOverviewJobs)
	commands = append(commands, cmds...)
	// Re-fetch cards whose data has already been loaded (a refresh of what the
	// operator is actually looking at); unloaded cards stay lazy.
	for _, card := range state.Overview.Cards {
		if card.State != "available" || !card.HasScope {
			continue
		}
		if _, present := state.Overview.Data[card.ProjectID]; !present {
			continue
		}
		state, cmds = requestPanelRefresh(state, overviewCardView(card.CanonicalRoot))
		commands = append(commands, cmds...)
	}
	return state, commands
}

// onOverviewSearchSubmit sets the client-side card filter (slug/prefix). It
// dispatches NOTHING (spec §11): full ticket-content search needs a focused
// project. An empty query clears the filter.
func onOverviewSearchSubmit(state tuiState, query string) (tuiState, []tuiCmd) {
	state = cloneTUIState(state)
	if state.Overview == nil {
		return state, nil
	}
	state.Overview.Search = overviewSearchState{Active: query != "", Query: query}
	state.Overview.Selected = clampIndex(0, len(overviewVisibleCards(*state.Overview)))
	// Focusing the first filtered card may reveal an unloaded available project.
	return overviewLazyFetchFocused(state)
}

// onOverviewSearchOpen opens the search input with a clean slate.
func onOverviewSearchOpen(state tuiState) tuiState {
	state = cloneTUIState(state)
	if state.Overview != nil {
		state.Overview.Search = overviewSearchState{Active: true}
	}
	return state
}
