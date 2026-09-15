package main

import (
	"strings"
	"testing"

	"aira/internal/daemon"
	"aira/internal/store"
)

func overviewTUIState(ov *overviewState) tuiState {
	return tuiState{Overview: ov, Panels: map[tuiView]panelState{}, PendingRefresh: map[tuiView]bool{}}
}

func availableCard(projectID, slug, root string) overviewCard {
	return overviewCard{
		ProjectID: projectID, Slug: slug, CanonicalRoot: root, CanonicalIsMain: true,
		State: "available", HasScope: true, Scope: daemon.WorktreeScope{ProjectID: projectID, Slug: slug, Root: root},
	}
}

// TestOverviewApplyListPreservesLoadedData: a list refresh rebuilds the card
// skeleton but must KEEP an already-loaded project's count/lease (keyed by
// ProjectID) and DROP a vanished project's stale data.
func TestOverviewApplyListPreservesLoadedData(t *testing.T) {
	state := *newOverviewState()
	state.Data["P1"] = overviewCardData{Loaded: true, Total: 7}
	state.Data["GONE"] = overviewCardData{Loaded: true, Total: 3}

	data := overviewListData{
		Groups:      overviewGroupProjects([]store.RegistryEntry{ovEntry("P1", "wt1", "/p1", "/p1/.git")}, allRootsLive),
		Discoveries: map[string]overviewDiscovery{"P1": {Slug: "p1"}},
		Scopes:      map[string]daemon.WorktreeScope{"P1": {ProjectID: "P1"}},
	}
	state = overviewApplyList(state, data)
	if got, ok := state.Data["P1"]; !ok || got.Total != 7 {
		t.Fatalf("apply-list dropped a still-live project's loaded data: %+v", state.Data)
	}
	if _, ok := state.Data["GONE"]; ok {
		// Mutation guard: never deleting vanished data lets the map grow unboundedly.
		t.Fatalf("apply-list kept a vanished project's data")
	}
	if !state.HasData || state.Stale {
		t.Fatalf("apply-list should clear stale and set HasData")
	}
}

// cardBase builds an overview state whose Cards map each pid to root "/"+pid, so
// overviewApplyCard (keyed by root→registry pid) has something to resolve.
func cardBase(pids ...string) overviewState {
	base := *newOverviewState()
	for _, pid := range pids {
		base.Cards = append(base.Cards, availableCard(pid, pid, "/"+pid))
	}
	return base
}

// TestOverviewApplyCardStates: the lazy result maps to the honest per-card
// lifecycle — E_NOT_ADOPTED→ejected, other code→unevaluated, ok→distribution.
// It is ALWAYS keyed by the card's registry pid resolved from the requested root.
func TestOverviewApplyCardStates(t *testing.T) {
	base := cardBase("P1", "P2", "P3", "P4", "P5")

	ejected := overviewApplyCard(base, "/P1", &overviewCardResult{ProjectID: "P1", Code: "E_NOT_ADOPTED"})
	if data := ejected.Data["P1"]; !data.Loaded || !data.Ejected {
		t.Fatalf("E_NOT_ADOPTED should set Ejected: %+v", ejected.Data["P1"])
	}
	unevaluated := overviewApplyCard(base, "/P2", &overviewCardResult{ProjectID: "P2", Code: "E_TUI_DECODE"})
	if data := unevaluated.Data["P2"]; !data.Loaded || data.Ejected || data.Code != "E_TUI_DECODE" {
		// Mutation guard: mapping any non-empty code to Ejected would hide the difference.
		t.Fatalf("a non-ejected code should be unevaluated, not ejected: %+v", unevaluated.Data["P2"])
	}
	ok := overviewApplyCard(base, "/P3", &overviewCardResult{ProjectID: "P3", Distribution: map[string]int{"planned": 2}, Total: 2, LeaseCount: 1})
	if data := ok.Data["P3"]; !data.Loaded || data.Total != 2 || !data.LeaseKnown || data.LeaseCount != 1 {
		t.Fatalf("ok result: %+v", ok.Data["P3"])
	}
	// A lease read that fails E_NOT_ADOPTED downgrades the card to ejected too.
	leaseEjected := overviewApplyCard(base, "/P4", &overviewCardResult{ProjectID: "P4", Distribution: map[string]int{}, LeaseCode: "E_NOT_ADOPTED"})
	if !leaseEjected.Data["P4"].Ejected {
		t.Fatalf("lease E_NOT_ADOPTED should set Ejected: %+v", leaseEjected.Data["P4"])
	}
	leaseUneval := overviewApplyCard(base, "/P5", &overviewCardResult{ProjectID: "P5", Distribution: map[string]int{}, LeaseCode: "E_TIMEOUT"})
	if d := leaseUneval.Data["P5"]; d.LeaseKnown || d.LeaseCode != "E_TIMEOUT" {
		t.Fatalf("lease failure should be unevaluated activity, not a known 0: %+v", d)
	}
}

// TestOverviewApplyCardFailureNotPerpetualLoading pins the P1 review fix: a
// card-time Discover failure (nil result, or a result with an empty ProjectID +
// a Code) and a fresh-id≠registry-id mismatch must ALL land a definite
// unevaluated/ejected state keyed by the CARD's registry pid — never leave the
// card perpetually loading (which would re-dispatch on every focus).
func TestOverviewApplyCardFailureNotPerpetualLoading(t *testing.T) {
	base := cardBase("P1")

	// (a) fetchOverviewCard returns a result with an EMPTY ProjectID when Discover
	// fails at card time. Keyed by root, it must still land as unevaluated.
	discoverFailed := overviewApplyCard(base, "/P1", &overviewCardResult{ProjectID: "", Code: "E_CONFIG_MISSING"})
	data := discoverFailed.Data["P1"]
	if !data.Loaded {
		// Mutation guard: keying by result.ProjectID ("") drops it → card stays "…".
		t.Fatalf("a card-time Discover failure must land loaded/unevaluated, got %+v (Data=%v)", data, discoverFailed.Data)
	}
	if data.Code != "E_CONFIG_MISSING" || data.Ejected {
		t.Fatalf("card-time failure should be unevaluated(E_CONFIG_MISSING): %+v", data)
	}
	// The card no longer needs a fetch → no re-dispatch on every focus.
	if overviewCardNeedsFetch(discoverFailed, discoverFailed.Cards[0]) {
		t.Fatalf("a landed (failed) card must NOT keep needing a fetch")
	}

	// (b) a nil result (decode failure) lands unevaluated, never perpetual "…".
	nilResult := overviewApplyCard(base, "/P1", nil)
	if d := nilResult.Data["P1"]; !d.Loaded || d.Code == "" {
		t.Fatalf("a nil card result must land unevaluated, got %+v", d)
	}

	// (c) a fresh Discover id that disagrees with the registry id is not attributed
	// (mirrors the daemon's skip) — unevaluated with the identity code.
	mismatch := overviewApplyCard(base, "/P1", &overviewCardResult{ProjectID: "DIFFERENT", Distribution: map[string]int{"done": 9}, Total: 9})
	if d := mismatch.Data["P1"]; !d.Loaded || d.Code != overviewIdentityMismatchCode || d.Total != 0 {
		t.Fatalf("identity mismatch must be unevaluated, not attributed: %+v", d)
	}
}

// TestOverviewApplyCardStale pins the P2 review fix: W_STALE_INDEX on the count
// reply surfaces as a per-card stale marker, not an authoritative distribution.
func TestOverviewApplyCardStale(t *testing.T) {
	base := cardBase("P1")
	stale := overviewApplyCard(base, "/P1", &overviewCardResult{ProjectID: "P1", Distribution: map[string]int{"planned": 4}, Total: 4, Stale: true})
	if d := stale.Data["P1"]; !d.Loaded || !d.Stale {
		t.Fatalf("W_STALE_INDEX should set the card stale flag: %+v", d)
	}
	label := overviewStateLabel(base.Cards[0], stale.Data["P1"], true)
	if !strings.Contains(label, "stale") {
		// Mutation guard: dropping the stale render makes a reconcile-pending count read authoritative.
		t.Fatalf("stale card label must disclose staleness: %q", label)
	}
}

// TestOverviewCardNeedsFetch: only an available, scoped, not-yet-loaded card is a
// lazy-fetch candidate (the laziness gate, spec §11.6).
func TestOverviewCardNeedsFetch(t *testing.T) {
	state := *newOverviewState()
	avail := availableCard("P1", "p1", "/p1")
	if !overviewCardNeedsFetch(state, avail) {
		t.Fatalf("an unloaded available card should need a fetch")
	}
	state.Data["P1"] = overviewCardData{Loaded: true}
	if overviewCardNeedsFetch(state, avail) {
		// Mutation guard: refetching an already-loaded card breaks laziness.
		t.Fatalf("a loaded card should NOT need a fetch")
	}
	if overviewCardNeedsFetch(*newOverviewState(), overviewCard{State: "unavailable"}) {
		t.Fatalf("an unavailable card should never be fetched")
	}
	if overviewCardNeedsFetch(*newOverviewState(), overviewCard{State: "available", HasScope: false}) {
		t.Fatalf("an available card with no scope should never be fetched")
	}
}

// TestOverviewMoveTriggersLazyFetch: moving onto an unloaded available card emits
// exactly the per-card fetch for its root, and none for a loaded one.
func TestOverviewMoveTriggersLazyFetch(t *testing.T) {
	ov := newOverviewState()
	ov.Cards = []overviewCard{availableCard("P1", "aaa", "/p1"), availableCard("P2", "bbb", "/p2")}
	ov.Data["P1"] = overviewCardData{Loaded: true} // focused card already loaded
	state := overviewTUIState(ov)

	state, cmds := onOverviewAction(state, overviewActDown) // focus P2 (unloaded)
	if state.Overview.Selected != 1 {
		t.Fatalf("down should move to index 1, got %d", state.Overview.Selected)
	}
	if len(cmds) != 1 || cmds[0].Kind != cmdFetch || cmds[0].View != overviewCardView("/p2") {
		t.Fatalf("down onto an unloaded card should fetch its root, got %+v", cmds)
	}
	// Moving back onto the loaded P1 must NOT re-fetch.
	state, cmds = onOverviewAction(state, overviewActUp)
	if len(cmds) != 0 {
		t.Fatalf("moving onto a loaded card must not fetch, got %+v", cmds)
	}
}

// TestOverviewOpenSetsChosenScope: Enter on an AVAILABLE row sets the chosen
// scope and quits (the outer loop then opens that project's board, spec §12);
// Enter on an unavailable / ejected / scope-less row is a no-op.
func TestOverviewOpenSetsChosenScope(t *testing.T) {
	ov := newOverviewState()
	ov.Cards = []overviewCard{availableCard("P1", "p1", "/p1")}
	state := overviewTUIState(ov)
	state, cmds := onOverviewAction(state, overviewActOpen)
	if state.Overview.Chosen == nil || state.Overview.Chosen.ProjectID != "P1" {
		t.Fatalf("open should set the chosen project scope, got %+v", state.Overview.Chosen)
	}
	if len(cmds) != 1 || cmds[0].Kind != cmdQuit || !state.ShuttingDown {
		t.Fatalf("open should emit cmdQuit + ShuttingDown, got cmds=%+v shutdown=%v", cmds, state.ShuttingDown)
	}

	// Unavailable row → no transition.
	un := newOverviewState()
	un.Cards = []overviewCard{{ProjectID: "PX", State: "unavailable", StateCode: "E_CONFIG_INVALID"}}
	unState, unCmds := onOverviewAction(overviewTUIState(un), overviewActOpen)
	if unState.Overview.Chosen != nil || len(unCmds) != 0 {
		t.Fatalf("opening an unavailable project must be a no-op, got chosen=%v cmds=%v", unState.Overview.Chosen, unCmds)
	}

	// Ejected available row → no board to open, no transition.
	ej := newOverviewState()
	ej.Cards = []overviewCard{availableCard("PE", "pe", "/pe")}
	ej.Data["PE"] = overviewCardData{Loaded: true, Ejected: true}
	ejState, ejCmds := onOverviewAction(overviewTUIState(ej), overviewActOpen)
	if ejState.Overview.Chosen != nil || len(ejCmds) != 0 {
		t.Fatalf("opening an ejected project must be a no-op, got chosen=%v", ejState.Overview.Chosen)
	}
}

// TestOverviewQuit: `q` sets ShuttingDown + cmdQuit and sets NO chosen scope, so
// the hop returns (nil,_) and the whole outer loop ends (never falls through to
// the board). The transition gate is Chosen==nil, so a write-only Quit flag was
// removed (P3 review fix); this pins the quit path via the observable signals.
func TestOverviewQuit(t *testing.T) {
	ov := newOverviewState()
	state, cmds := onOverviewAction(overviewTUIState(ov), overviewActQuit)
	if !state.ShuttingDown || len(cmds) != 1 || cmds[0].Kind != cmdQuit {
		t.Fatalf("quit should set ShuttingDown+cmdQuit, got cmds=%v shutdown=%v", cmds, state.ShuttingDown)
	}
	if state.Overview.Chosen != nil {
		t.Fatalf("quit must not set a chosen scope (else the loop would transition, not quit)")
	}
}

// TestOverviewSearchFilters: submitting a query filters the visible cards; an
// empty query clears the filter.
func TestOverviewSearchFilters(t *testing.T) {
	ov := newOverviewState()
	ov.Cards = []overviewCard{
		availableCard("P1", "alpha", "/a"),
		availableCard("P2", "beta", "/b"),
	}
	state := overviewTUIState(ov)
	state, _ = onOverviewSearchSubmit(state, "alph")
	if visible := overviewVisibleCards(*state.Overview); len(visible) != 1 || visible[0].Slug != "alpha" {
		t.Fatalf("search 'alph' should show only alpha, got %+v", visible)
	}
	state, _ = onOverviewSearchSubmit(state, "")
	if visible := overviewVisibleCards(*state.Overview); len(visible) != 2 {
		t.Fatalf("empty query should clear the filter, got %d", len(visible))
	}
}

// TestOverviewClearSearchEmitsLazyFetch pins the P2 review fix's contract: when
// clearing the filter re-focuses an UNLOADED available card, the reducer EMITS a
// lazy fetch — so the shell's Escape branch must submit those commands (a
// discarded cmdFetch wedges the panel InFlight forever). Mutation guard: if
// clearing did not re-trigger the lazy fetch, there'd be no command and the shell
// bug would be invisible.
func TestOverviewClearSearchEmitsLazyFetch(t *testing.T) {
	ov := newOverviewState()
	ov.Cards = []overviewCard{availableCard("P1", "alpha", "/a")} // unloaded (no Data)
	ov.Search = overviewSearchState{Active: true, Query: "zzz"}   // filters everything out
	state := overviewTUIState(ov)
	state, cmds := onOverviewSearchSubmit(state, "")
	if len(cmds) != 1 || cmds[0].Kind != cmdFetch || cmds[0].View != overviewCardView("/a") {
		t.Fatalf("clearing the filter onto an unloaded card must emit its lazy fetch (the shell must submit it), got %+v", cmds)
	}
}

// TestOverviewBannerNoFabricatedCause pins the P2 review fix: the only whole-list
// failure is the project-list read, so the banner names THAT cause and never
// fabricates "daemon unreachable".
func TestOverviewBannerNoFabricatedCause(t *testing.T) {
	fresh := newOverviewState()
	if b := overviewBannerText(fresh); b != "" {
		t.Fatalf("a fresh overview should show no banner, got %q", b)
	}
	staleWithData := &overviewState{HasData: true, Stale: true, ErrorCode: "E_TUI_DECODE"}
	banner := overviewBannerText(staleWithData)
	if !strings.Contains(banner, "project list read failed") || !strings.Contains(banner, "E_TUI_DECODE") {
		t.Fatalf("banner should name the project-list read failure, got %q", banner)
	}
	if strings.Contains(banner, "daemon unreachable") {
		t.Fatalf("banner must not fabricate a 'daemon unreachable' cause, got %q", banner)
	}
	firstLoad := &overviewState{HasData: false, Stale: true, ErrorCode: "E_TUI_DECODE"}
	if b := overviewBannerText(firstLoad); !strings.Contains(b, "unavailable") {
		t.Fatalf("no-last-good banner should read unavailable, got %q", b)
	}
}

// TestBoardToOverviewTransition: `o` in the per-project board sets the ToOverview
// flag (with cmdQuit), the signal the outer loop reads to switch to the overview.
func TestBoardToOverviewTransition(t *testing.T) {
	state := tuiState{Board: newBoardState(), Panels: map[tuiView]panelState{}, PendingRefresh: map[tuiView]bool{}}
	state, cmds := onBoardAction(state, boardActToOverview)
	if !state.Board.ToOverview || !state.ShuttingDown {
		t.Fatalf("board 'o' should set ToOverview + ShuttingDown, got %+v", state.Board)
	}
	if len(cmds) != 1 || cmds[0].Kind != cmdQuit {
		t.Fatalf("board 'o' should emit cmdQuit, got %+v", cmds)
	}
	// Plain quit must NOT set ToOverview (else a `q` would loop back to overview).
	q := tuiState{Board: newBoardState(), Panels: map[tuiView]panelState{}, PendingRefresh: map[tuiView]bool{}}
	q, _ = onBoardAction(q, boardActQuit)
	if q.Board.ToOverview {
		t.Fatalf("plain quit must not set ToOverview")
	}
}
