package main

// AIRA-257 unit tests for the board's plain-English translation: the pure
// helpers (cache key, prompt, output parse, cache round-trip, display seam), the
// reducer transitions (request/toggle/retry/keyed-result/refresh-clear/overlay
// target), and the REAL translator's cache-and-fail behaviour driven over a temp
// dir with deterministic commands (never the network, never the owner's cache).

import (
	"context"
	"strings"
	"testing"
)

func TestBoardHumanizeCacheKey(t *testing.T) {
	base := boardHumanizeCacheKey("AIRA-1", "t", "b")
	if len(base) != 64 {
		t.Fatalf("expected a 64-hex sha256, got %q", base)
	}
	if boardHumanizeCacheKey("AIRA-1", "t", "b") != base {
		t.Fatal("cache key is not stable for identical input")
	}
	if boardHumanizeCacheKey("AIRA-1", "t2", "b") == base {
		t.Fatal("a title change must change the key (else a stale rewrite is shown)")
	}
	if boardHumanizeCacheKey("AIRA-1", "t", "b2") == base {
		t.Fatal("a body change must change the key")
	}
	if boardHumanizeCacheKey("AIRA-2", "t", "b") == base {
		t.Fatal("an id change must change the key")
	}
}

func TestBoardHumanizePrompt(t *testing.T) {
	p := boardHumanizePrompt("My Title", "My Body")
	for _, needle := range []string{"My Title", "My Body", "TITLE:", "BODY:"} {
		if !strings.Contains(p, needle) {
			t.Fatalf("prompt missing %q:\n%s", needle, p)
		}
	}
	// AIRA-258: the prompt must ask for the FULL rewrite (not a short summary) and
	// name the technical-reader audience — the owner's requirement.
	for _, needle := range []string{"FULL", "summary", "technical manager"} {
		if !strings.Contains(p, needle) {
			t.Fatalf("prompt no longer pins the full-rewrite/audience intent (missing %q):\n%s", needle, p)
		}
	}
	if !strings.Contains(boardHumanizePrompt("T", "   "), "(none)") {
		t.Fatal("an empty description must render (none), never a blank")
	}
}

func TestBoardParseHumanizeOutput(t *testing.T) {
	cases := []struct{ name, in, wantT, wantB string }{
		{"markers", "TITLE: Fix the thing\nBODY: It broke. Now it works.", "Fix the thing", "It broke. Now it works."},
		{"multiline body", "TITLE: A\nBODY: one\ntwo", "A", "one\ntwo"},
		{"code fenced", "```\nTITLE: A\nBODY: B\n```", "A", "B"},
		{"bold markers", "**TITLE:** A\n**BODY:** B", "A", "B"},
		{"no markers fallback", "Just a title line\nand a body line", "Just a title line", "and a body line"},
		{"empty", "", "", ""},
	}
	for _, c := range cases {
		gotT, gotB := boardParseHumanizeOutput(c.in)
		if gotT != c.wantT || gotB != c.wantB {
			t.Errorf("%s: got (%q, %q), want (%q, %q)", c.name, gotT, gotB, c.wantT, c.wantB)
		}
	}
}

func TestBoardHumanizeDisplay(t *testing.T) {
	ready := boardHumanizeState{ID: "AIRA-1", State: "ready", PlainTitle: "PT", PlainBody: "PB", Shown: true}

	if ti, bo, la := boardHumanizeDisplay("AIRA-2", "OT", "OB", ready); ti != "OT" || bo != "OB" || la != "" {
		t.Fatalf("id mismatch must show the original with no label: %q %q %q", ti, bo, la)
	}
	hidden := boardHumanizeState{ID: "AIRA-1", State: "ready", PlainTitle: "PT", PlainBody: "PB", Shown: false}
	if ti, bo, la := boardHumanizeDisplay("AIRA-1", "OT", "OB", hidden); ti != "OT" || bo != "OB" || la != "" {
		t.Fatalf("hidden must show the original: %q %q %q", ti, bo, la)
	}
	if ti, bo, la := boardHumanizeDisplay("AIRA-1", "OT", "OB", ready); ti != "PT" || bo != "PB" || la != "plain-English (AI)" {
		t.Fatalf("ready+shown must show the plain rewrite + label: %q %q %q", ti, bo, la)
	}
	partial := boardHumanizeState{ID: "AIRA-1", State: "ready", PlainTitle: "", PlainBody: "PB", Shown: true}
	if ti, bo, _ := boardHumanizeDisplay("AIRA-1", "OT", "OB", partial); ti != "OT" || bo != "PB" {
		t.Fatalf("a partial parse must fall back to the original for the missing half: %q %q", ti, bo)
	}
	loading := boardHumanizeState{ID: "AIRA-1", State: "loading", Shown: true}
	if ti, _, la := boardHumanizeDisplay("AIRA-1", "OT", "OB", loading); ti != "OT" || la != "translating…" {
		t.Fatalf("loading must keep the original and say translating: %q %q", ti, la)
	}
	failed := boardHumanizeState{ID: "AIRA-1", State: "unevaluated:E_X", Shown: true}
	if ti, _, la := boardHumanizeDisplay("AIRA-1", "OT", "OB", failed); ti != "OT" || la != "translation unavailable (E_X)" {
		t.Fatalf("a failure must keep the original and say unavailable: %q %q", ti, la)
	}
}

func TestBoardHumanizeCacheRoundTrip(t *testing.T) {
	dir := t.TempDir()
	if _, ok := boardHumanizeLoadCache(dir, "missing"); ok {
		t.Fatal("an absent cache file must be a miss")
	}
	boardHumanizeStoreCache(dir, "k", boardHumanizeResult{PlainTitle: "PT", PlainBody: "PB"})
	got, ok := boardHumanizeLoadCache(dir, "k")
	if !ok || got.PlainTitle != "PT" || got.PlainBody != "PB" {
		t.Fatalf("round-trip failed: %+v ok=%v", got, ok)
	}
	// A parseable-but-empty file (a corrupt/partial write) is a miss, never a blank
	// render.
	boardHumanizeStoreCache(dir, "blank", boardHumanizeResult{})
	if _, ok := boardHumanizeLoadCache(dir, "blank"); ok {
		t.Fatal("an empty-content cache entry must be a miss")
	}
}

// TestAgentmuxTranslatorCachesInstantly is the owner's core requirement — "instant
// next time". A first call over a temp dir with a working command produces and
// stores the rewrite; a SECOND translator over the SAME dir with a deliberately
// failing command still returns the cached rewrite, proving the disk cache is the
// source, not a re-run. printf's `%.0s` swallows the appended prompt so the output
// is fixed and shell-free.
func TestAgentmuxTranslatorCachesInstantly(t *testing.T) {
	dir := t.TempDir()
	ok := newAgentmuxTranslator(dir, []string{"printf", "TITLE: PLAINT\nBODY: PLAINB\n%.0s"})
	got := ok(context.Background(), "AIRA-1", "orig title", "orig body")
	if got.Code != "" {
		t.Fatalf("first call failed unexpectedly: %+v", got)
	}
	if got.PlainTitle != "PLAINT" || got.PlainBody != "PLAINB" {
		t.Fatalf("parsed rewrite wrong: %+v", got)
	}
	fail := newAgentmuxTranslator(dir, []string{"false"})
	cached := fail(context.Background(), "AIRA-1", "orig title", "orig body")
	if cached.Code != "" {
		t.Fatalf("cache miss: the failing command ran (%q) instead of the cache being hit", cached.Code)
	}
	if cached.PlainTitle != "PLAINT" || cached.PlainBody != "PLAINB" {
		t.Fatalf("cached rewrite wrong: %+v", cached)
	}
}

func TestAgentmuxTranslatorFailureIsUnavailable(t *testing.T) {
	got := newAgentmuxTranslator(t.TempDir(), []string{"false"})(context.Background(), "AIRA-1", "t", "b")
	if got.Code != "E_TUI_TRANSLATE_UNAVAILABLE" {
		t.Fatalf("a failed command must be unavailable, got %q", got.Code)
	}
}

func TestAgentmuxTranslatorEmptyOutputIsDecode(t *testing.T) {
	got := newAgentmuxTranslator(t.TempDir(), []string{"true"})(context.Background(), "AIRA-1", "t", "b")
	if got.Code != "E_TUI_DECODE" {
		t.Fatalf("empty output must be a decode failure, got %q", got.Code)
	}
}

// --- reducer transitions ---

func newTranslateBoardState(t *testing.T) tuiState {
	t.Helper()
	state := newTUIStateForViews(64, boardViews, boardViews)
	board := newBoardState()
	*board = boardApplyModel(*board, boardModel{Columns: []boardColumn{{
		Status: "planned",
		Cards:  []boardCard{{ID: "AIRA-1", Title: "orig one"}, {ID: "AIRA-2", Title: "orig two"}},
	}}})
	state.Board = board
	return state
}

func TestBoardActTranslateRequestsThenToggles(t *testing.T) {
	state := newTranslateBoardState(t)

	state, cmds := onBoardAction(state, boardActTranslate)
	if h := state.Board.Humanize; h.ID != "AIRA-1" || h.State != "loading" || !h.Shown {
		t.Fatalf("first press should request AIRA-1: %+v", h)
	}
	if len(cmds) != 1 || cmds[0].Kind != cmdBoardTranslate || cmds[0].TranslateID != "AIRA-1" {
		t.Fatalf("first press should emit one cmdBoardTranslate for AIRA-1: %+v", cmds)
	}

	state, cmds = onBoardAction(state, boardActTranslate)
	if state.Board.Humanize.Shown {
		t.Fatal("second press while loading should hide the plain view")
	}
	if len(cmds) != 0 {
		t.Fatalf("a toggle must not re-request: %+v", cmds)
	}

	state, _ = onBoardTranslateResult(state, translateResult{ID: "AIRA-1", Result: boardHumanizeResult{PlainTitle: "PT", PlainBody: "PB"}})
	if h := state.Board.Humanize; h.State != "ready" || h.PlainBody != "PB" {
		t.Fatalf("result should land ready: %+v", h)
	}
	state, cmds = onBoardAction(state, boardActTranslate)
	if !state.Board.Humanize.Shown {
		t.Fatal("pressing on a ready-but-hidden rewrite should show it")
	}
	if len(cmds) != 0 {
		t.Fatalf("showing a cached rewrite must not re-request: %+v", cmds)
	}
}

func TestOnBoardTranslateResultKeyedByID(t *testing.T) {
	state := newTranslateBoardState(t)
	state, _ = onBoardAction(state, boardActTranslate) // Humanize.ID = AIRA-1, loading

	before := state.Board.Humanize
	state, _ = onBoardTranslateResult(state, translateResult{ID: "AIRA-999", Result: boardHumanizeResult{PlainTitle: "X"}})
	if state.Board.Humanize != before {
		t.Fatalf("a result for a since-changed ticket must be dropped: %+v", state.Board.Humanize)
	}

	state, _ = onBoardTranslateResult(state, translateResult{ID: "AIRA-1", Result: boardHumanizeResult{PlainTitle: "PT", PlainBody: "PB"}})
	if h := state.Board.Humanize; h.State != "ready" || h.PlainTitle != "PT" {
		t.Fatalf("a matching result must apply: %+v", h)
	}

	failState := newTranslateBoardState(t)
	failState, _ = onBoardAction(failState, boardActTranslate)
	failState, _ = onBoardTranslateResult(failState, translateResult{ID: "AIRA-1", Result: boardHumanizeResult{Code: "E_TUI_TRANSLATE_UNAVAILABLE"}})
	if failState.Board.Humanize.State != "unevaluated:E_TUI_TRANSLATE_UNAVAILABLE" {
		t.Fatalf("a failed result must land unevaluated: %+v", failState.Board.Humanize)
	}
}

func TestBoardActTranslateRetriesAfterFailure(t *testing.T) {
	state := newTranslateBoardState(t)
	state, _ = onBoardAction(state, boardActTranslate)
	state, _ = onBoardTranslateResult(state, translateResult{ID: "AIRA-1", Result: boardHumanizeResult{Code: "E_TUI_TRANSLATE_UNAVAILABLE"}})

	state, cmds := onBoardAction(state, boardActTranslate)
	if state.Board.Humanize.State != "loading" {
		t.Fatalf("pressing after a failure should re-request, not toggle a dead state: %+v", state.Board.Humanize)
	}
	if len(cmds) != 1 || cmds[0].Kind != cmdBoardTranslate {
		t.Fatalf("retry should emit a fresh cmdBoardTranslate: %+v", cmds)
	}
}

func TestBoardActTranslateTargetsOverlayTicketWhenExpanded(t *testing.T) {
	state := newTranslateBoardState(t)
	state.Board.Expanded = true
	// The overlay is showing an UNLOADED search hit (Detail.ID != the cursor card).
	state.Board.Detail = boardDetailState{ID: "AIRA-777", State: "ready"}

	state, cmds := onBoardAction(state, boardActTranslate)
	if state.Board.Humanize.ID != "AIRA-777" {
		t.Fatalf("an expanded translate must target the overlay ticket, got %q", state.Board.Humanize.ID)
	}
	if len(cmds) != 1 || cmds[0].TranslateID != "AIRA-777" {
		t.Fatalf("expanded translate cmds: %+v", cmds)
	}
}

// TestBoardNavClearsHumanize is the AIRA-257 P2 regression (Fable): moving the
// selection to a different ticket drops the held plain-English rewrite, so a nav
// round-trip can never leave a stale rewrite (of the previous ticket's possibly
// now-changed text) rendering beside the new selection. Pressing 't' re-derives it
// from the current content (instant from the disk cache when unchanged).
func TestBoardNavClearsHumanize(t *testing.T) {
	state := newTranslateBoardState(t) // AIRA-1 selected, plus AIRA-2 below it
	state, _ = onBoardAction(state, boardActTranslate)
	state, _ = onBoardTranslateResult(state, translateResult{ID: "AIRA-1", Result: boardHumanizeResult{PlainTitle: "PT", PlainBody: "PB"}})
	if state.Board.Humanize.State != "ready" {
		t.Fatalf("precondition: AIRA-1 rewrite should be ready: %+v", state.Board.Humanize)
	}
	// Move down to AIRA-2: the AIRA-1 rewrite must be dropped, not carried over.
	state, _ = onBoardAction(state, boardActCardDown)
	if state.Board.Humanize != (boardHumanizeState{}) {
		t.Fatalf("nav to a different ticket must clear the held rewrite: %+v", state.Board.Humanize)
	}
}

func TestBoardActRefreshClearsHumanize(t *testing.T) {
	state := newTranslateBoardState(t)
	state, _ = onBoardAction(state, boardActTranslate)
	state, _ = onBoardTranslateResult(state, translateResult{ID: "AIRA-1", Result: boardHumanizeResult{PlainTitle: "PT", PlainBody: "PB"}})

	state, _ = onBoardAction(state, boardActRefresh)
	if state.Board.Humanize != (boardHumanizeState{}) {
		t.Fatalf("refresh must drop the held rewrite so it cannot sit next to refreshed meta: %+v", state.Board.Humanize)
	}
}
