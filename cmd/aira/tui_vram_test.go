package main

import (
	"strings"
	"testing"

	"github.com/rivo/tview"

	"aira/internal/runner"
)

// verifies: AIRA-269 — the per-scope VRAM cell. nil is unevaluated (no daemon
// record); 0 is the POSITIVE "not a GPU job" and renders "—"; a positive value is
// a formatted size. The 0-vs-nil split must never collapse.
func TestTopVRAMCell(t *testing.T) {
	if got := topVRAMCell(nil); got != "unevaluated" {
		t.Fatalf("nil = %q, want unevaluated", got)
	}
	zero := int64(0)
	if got := topVRAMCell(&zero); got != "—" {
		t.Fatalf("0 (not a GPU job) = %q, want —", got)
	}
	four := 4 * gib
	if got := topVRAMCell(&four); got == "unevaluated" || got == "—" || got == "" {
		t.Fatalf("4G = %q, want a formatted size", got)
	}
}

// verifies: AIRA-269 — topVRAMBarFor draws the physical card: aira-reserved stacked
// left, used-outside-aira (desktop) anchored right, free, with budget + admit-fit
// markers. Outside = physical-used − aira-reserved; free closes to the card total.
func TestTopVRAMBarForSetState(t *testing.T) {
	reserve := &runner.ConfineSliceReserve{
		VRAMState: runner.VRAMStateSet, VRAMTotalBytes: 16 * gib, VRAMFreeBytes: 3 * gib,
		VRAMBudgetBytes: 14 * gib, VRAMHeadroomBytes: gib, VRAMOutstandingBytes: 8 * gib,
	}
	scopes := []topBarRegion{{Kind: topRegionScope, Slot: 0, Label: "train", Start: 0, Size: 8 * gib}}
	bar := topVRAMBarFor(reserve, scopes, 8*gib)
	if !bar.Evaluated || bar.Kind != topBarVRAM {
		t.Fatalf("bar not evaluated as VRAM: %+v", bar)
	}
	if bar.Total != 16*gib {
		t.Fatalf("Total=%d want 16G", bar.Total)
	}
	if bar.Outside != 5*gib { // physical used 13G − aira reserved 8G
		t.Fatalf("Outside=%d want 5G (desktop = 13G used − 8G aira-reserved)", bar.Outside)
	}
	if bar.Free != 3*gib { // 16 − 8 (claimed) − 5 (outside)
		t.Fatalf("Free=%d want 3G", bar.Free)
	}
	var budget, fit bool
	for _, m := range bar.Markers {
		if m.Label == "budget" && m.At == 14*gib {
			budget = true
		}
		if m.Label == "fit" && m.At == 2*gib { // min(14G budget, 3G free − 1G headroom)
			fit = true
		}
	}
	if !budget || !fit {
		t.Fatalf("markers = %+v, want budget@14G and fit@2G", bar.Markers)
	}
}

// verifies: AIRA-269 — the honesty states never draw a bar; each Reason NAMES its
// state so an operator is not told "GPU unreadable" when the truth is "no GPU work
// requested".
func TestTopVRAMBarForHonestyStates(t *testing.T) {
	for _, tc := range []struct{ state, wantIn string }{
		{runner.VRAMStateNoGPUWork, "no GPU work"},
		{runner.VRAMStateNoGPU, "unreadable"},
	} {
		bar := topVRAMBarFor(&runner.ConfineSliceReserve{VRAMState: tc.state}, nil, 0)
		if bar.Evaluated {
			t.Fatalf("state %q must render a Reason, not a bar", tc.state)
		}
		if !strings.Contains(bar.Reason, tc.wantIn) {
			t.Fatalf("state %q reason=%q, want it to mention %q", tc.state, bar.Reason, tc.wantIn)
		}
	}
}

// verifies: AIRA-269 — topViewModel WIRES the VRAM bar and column: a GPU scope's
// declared --vram becomes a bar region AND a table cell, and the Headers gain VRAM.
// A mutation dropping the model.VRAMBar assignment or the vramDrawn build reds this.
func TestTopViewModelWiresVRAMBar(t *testing.T) {
	reserve := topTestFrame()
	reserve.VRAMState = runner.VRAMStateSet
	reserve.VRAMTotalBytes = 16 * gib
	reserve.VRAMFreeBytes = 3 * gib
	reserve.VRAMBudgetBytes = 14 * gib
	reserve.VRAMHeadroomBytes = gib
	reserve.VRAMOutstandingBytes = 8 * gib
	rec := topTestRecord("CONFINE-train-1-aa", "train", 8*gib, 2*gib)
	v := 8 * gib
	rec.VRAMBytes = &v

	model, _ := topViewModel(topTick{}, topTestListing(reserve, rec))

	vramCol := -1
	for i, h := range model.Headers {
		if h == "VRAM" {
			vramCol = i
		}
	}
	if vramCol < 0 {
		t.Fatalf("Headers has no VRAM column: %v", model.Headers)
	}
	if model.VRAMBar == nil || !model.VRAMBar.Evaluated || model.VRAMBar.Kind != topBarVRAM {
		t.Fatalf("model.VRAMBar not wired/evaluated: %+v", model.VRAMBar)
	}
	var scopeRegion bool
	for _, region := range model.VRAMBar.Regions {
		if region.Kind == topRegionScope && region.Size == 8*gib {
			scopeRegion = true
		}
	}
	if !scopeRegion {
		t.Fatalf("the GPU scope's 8G --vram was not drawn as a bar region: %+v", model.VRAMBar.Regions)
	}
	if len(model.Rows) != 1 {
		t.Fatalf("rows=%d, want 1", len(model.Rows))
	}
	if cell := model.Rows[0].Cells[vramCol]; cell == "—" || cell == "unevaluated" || cell == "" {
		t.Fatalf("GPU job's VRAM cell = %q, want a formatted 8G reservation", cell)
	}
}

// verifies: AIRA-269 — when aira's reservation exceeds the card's physically-used
// bytes (a just-admitted, not-yet-ramped GPU job), "used outside aira" CLAMPS to 0
// (never negative), every region stays within the card, the widths close exactly,
// and a note says the desktop usage is hidden inside the reservation. Removing the
// topFloor clamp (Outside going negative, free running past the card) reds this.
func TestTopVRAMBarForReservedExceedsPhysicalUsed(t *testing.T) {
	reserve := &runner.ConfineSliceReserve{
		VRAMState: runner.VRAMStateSet, VRAMTotalBytes: 16 * gib, VRAMFreeBytes: 15 * gib,
		VRAMBudgetBytes: 14 * gib, VRAMHeadroomBytes: gib, VRAMOutstandingBytes: 8 * gib,
	}
	scopes := []topBarRegion{{Kind: topRegionScope, Slot: 0, Label: "train", Start: 0, Size: 8 * gib}}
	bar := topVRAMBarFor(reserve, scopes, 8*gib)
	if !bar.Evaluated {
		t.Fatalf("bar not evaluated: %+v", bar)
	}
	if bar.Outside != 0 { // physical-used = 16−15 = 1G, below the 8G reserved → clamp to 0
		t.Fatalf("Outside=%d want 0 (reserved 8G exceeds physical-used 1G — clamp, not negative)", bar.Outside)
	}
	if bar.Free != 8*gib { // 16 − 8 claimed − 0 outside
		t.Fatalf("Free=%d want 8G", bar.Free)
	}
	if bar.Claimed+bar.Outside+bar.Free != bar.Total {
		t.Fatalf("widths do not close: claimed %d + outside %d + free %d != total %d", bar.Claimed, bar.Outside, bar.Free, bar.Total)
	}
	for _, r := range bar.Regions {
		if r.Start+r.Size > bar.Total {
			t.Fatalf("region %q runs past the card: start %d + size %d > total %d", r.Label, r.Start, r.Size, bar.Total)
		}
	}
	var noted bool
	for _, n := range bar.Notes {
		if strings.Contains(n, "hidden inside aira's reservation") {
			noted = true
		}
	}
	if !noted {
		t.Fatalf("expected a note that out-of-aira usage is hidden inside the reservation; notes=%v", bar.Notes)
	}
}

// verifies: AIRA-269 — a STALE GPU sample still DRAWS (last-good figures describe
// the card), but carries a note saying it is stale so an operator is not misled
// into trusting out-of-date figures. Deleting the stale-note append reds this.
func TestTopVRAMBarForStaleDrawsWithNote(t *testing.T) {
	reserve := &runner.ConfineSliceReserve{
		VRAMState: runner.VRAMStateStale, VRAMTotalBytes: 16 * gib, VRAMFreeBytes: 3 * gib,
		VRAMBudgetBytes: 14 * gib, VRAMHeadroomBytes: gib, VRAMOutstandingBytes: 4 * gib,
	}
	bar := topVRAMBarFor(reserve, nil, 4*gib)
	if !bar.Evaluated {
		t.Fatalf("a stale bar must still draw (last-good figures describe the card): %+v", bar)
	}
	var stale bool
	for _, n := range bar.Notes {
		if strings.Contains(n, "stale") {
			stale = true
		}
	}
	if !stale {
		t.Fatalf("a stale bar must carry a 'stale' note; notes=%v", bar.Notes)
	}
}

// verifies: AIRA-274 — with NO card reading (sampler never armed, or nvidia-smi
// unreadable) the panel still draws aira's RESERVATIONS against the configured
// budget: the scope spans stacked left, the rest as free, the total labelled as the
// budget (not the card), the card-side figures stated unknown, and a note NAMING
// which honesty state applies. Reverting to "UNEVALUATED" (the AIRA-269 behaviour)
// or drawing the budget as if it were the card total reds this.
func TestTopVRAMBarForReservationsOnlyWhenCardUnread(t *testing.T) {
	for _, tc := range []struct{ state, wantNote string }{
		{runner.VRAMStateNoGPUWork, "no GPU work: no --vram job has run"},
		{runner.VRAMStateNoGPU, "nvidia-smi"},
	} {
		reserve := &runner.ConfineSliceReserve{
			VRAMState: tc.state, VRAMBudgetBytes: 14 * gib, VRAMOutstandingBytes: 6 * gib,
		}
		scopes := []topBarRegion{{Kind: topRegionScope, Slot: 0, Label: "train", Start: 0, Size: 6 * gib}}
		bar := topVRAMBarFor(reserve, scopes, 6*gib)
		if !bar.Evaluated || bar.Kind != topBarVRAM {
			t.Fatalf("state %q: want a reservations-only bar, got %+v", tc.state, bar)
		}
		if bar.Total != 14*gib || !bar.TotalIsBudget {
			t.Fatalf("state %q: total=%d isBudget=%v, want the 14G budget flagged as a budget", tc.state, bar.Total, bar.TotalIsBudget)
		}
		if bar.Claimed != 6*gib || bar.Free != 8*gib {
			t.Fatalf("state %q: claimed=%d free=%d, want 6G/8G", tc.state, bar.Claimed, bar.Free)
		}
		if bar.OutsideKnown || bar.Outside != 0 {
			t.Fatalf("state %q: the card's outside usage is UNKNOWN, got known=%v outside=%d", tc.state, bar.OutsideKnown, bar.Outside)
		}
		var scope, free, outside bool
		for _, region := range bar.Regions {
			switch region.Kind {
			case topRegionScope:
				scope = region.Size == 6*gib
			case topRegionFree:
				free = region.Start == 6*gib && region.Size == 8*gib
			case topRegionOutside:
				outside = true
			}
		}
		if !scope || !free || outside {
			t.Fatalf("state %q: regions scope=%v free=%v outside=%v: %+v", tc.state, scope, free, outside, bar.Regions)
		}
		if len(bar.Markers) != 0 {
			t.Fatalf("state %q: no markers without a card reading, got %+v", tc.state, bar.Markers)
		}
		if len(bar.Notes) != 1 || !strings.Contains(bar.Notes[0], tc.wantNote) {
			t.Fatalf("state %q: notes=%v, want one naming %q", tc.state, bar.Notes, tc.wantNote)
		}
		legend := topBarLegend(bar)
		if !strings.Contains(legend, "budget 14") || strings.Contains(legend, "total") ||
			!strings.Contains(legend, "unreserved") || strings.Contains(legend, "free") ||
			!strings.Contains(legend, "rest of system unevaluated") {
			t.Fatalf("state %q: legend=%q must call the width a budget and leave the rest unevaluated", tc.state, legend)
		}
		if got := topMarkerLegend(bar); got != "" {
			t.Fatalf("state %q: marker legend=%q, want none (not 'no slice limit could be established')", tc.state, got)
		}
	}
}

// verifies: AIRA-274 — an idle box (nothing reserved) with a budget shows an EMPTY
// reservation stack against the budget: a positive "nothing reserved", the point of
// the fix. And with NO budget configured and no card reading there is no honest
// width, so it is a Reason — which still names what IS reserved.
func TestTopVRAMBarForReservationsOnlyEdges(t *testing.T) {
	idle := topVRAMBarFor(&runner.ConfineSliceReserve{
		VRAMState: runner.VRAMStateNoGPUWork, VRAMBudgetBytes: 14 * gib,
	}, nil, 0)
	if !idle.Evaluated || idle.Claimed != 0 || idle.Free != 14*gib {
		t.Fatalf("idle box with a budget: want evaluated, claimed 0, free 14G; got %+v", idle)
	}

	noBudget := topVRAMBarFor(&runner.ConfineSliceReserve{
		VRAMState: runner.VRAMStateNoGPU, VRAMOutstandingBytes: 6 * gib,
	}, nil, 6*gib)
	if noBudget.Evaluated {
		t.Fatalf("no budget and no card: there is no width to draw, got %+v", noBudget)
	}
	if !strings.Contains(noBudget.Reason, "unreadable") || !strings.Contains(noBudget.Reason, topFormatQuantity(topBarVRAM, 6*gib)) {
		t.Fatalf("reason=%q must name the state and the reserved amount", noBudget.Reason)
	}
}

// verifies: AIRA-274 — the whole chain, listing → topViewModel → renderTopBar text:
// a box with a budget, one 6G GPU job and NO card reading prints a drawn bar (not
// "UNEVALUATED"), the budget legend, the card-unread note — inside the panel height.
func TestTopVRAMPanelRendersReservationsWithoutCardReading(t *testing.T) {
	reserve := topTestFrame()
	reserve.VRAMState = runner.VRAMStateNoGPUWork
	reserve.VRAMBudgetBytes = 14 * gib
	reserve.VRAMOutstandingBytes = 6 * gib
	rec := topTestRecord("CONFINE-train-1-aa", "train", 8*gib, 2*gib)
	v := 6 * gib
	rec.VRAMBytes = &v

	model, _ := topViewModel(topTick{}, topTestListing(reserve, rec))

	target := tview.NewTextView().SetDynamicColors(true).SetWrap(false)
	target.SetBorder(true)
	target.SetRect(0, 0, 100, topVRAMBarHeight)
	(&tuiRuntime{}).renderTopBar(target, model.VRAMBar, panelState{Status: panelReady})
	text := target.GetText(true)
	if strings.Contains(text, "UNEVALUATED") {
		t.Fatalf("panel must show the reservations, not UNEVALUATED: %q", text)
	}
	for _, want := range []string{"budget 14", "reserved 6144M", "unreserved 8192M", "rest of system unevaluated", "configured budget"} {
		if !strings.Contains(text, want) {
			t.Fatalf("panel text missing %q: %q", want, text)
		}
	}
	if lines := strings.Split(text, "\n"); len(lines) > topVRAMBarHeight-2 {
		t.Fatalf("%d lines overflow the panel (%d inner): %q", len(lines), topVRAMBarHeight-2, text)
	}
}

// verifies: AIRA-274 (review) — only the two KNOWN unread states take the budget
// bar. An empty/unknown state (an older daemon with no vram_state) stays
// UNEVALUATED even when a budget figure is present, and a real card bar is never
// labelled a "budget".
func TestTopVRAMBarForUnknownStateAndCardBarLabels(t *testing.T) {
	for _, state := range []string{"", "bogus"} {
		bar := topVRAMBarFor(&runner.ConfineSliceReserve{
			VRAMState: state, VRAMBudgetBytes: 14 * gib, VRAMOutstandingBytes: 6 * gib,
		}, nil, 6*gib)
		if bar.Evaluated || bar.TotalIsBudget || !strings.Contains(bar.Reason, "unevaluated") {
			t.Fatalf("state %q: want UNEVALUATED, got %+v", state, bar)
		}
	}
	for _, state := range []string{runner.VRAMStateSet, runner.VRAMStateStale} {
		bar := topVRAMBarFor(&runner.ConfineSliceReserve{
			VRAMState: state, VRAMTotalBytes: 16 * gib, VRAMFreeBytes: 3 * gib,
			VRAMBudgetBytes: 14 * gib, VRAMHeadroomBytes: gib, VRAMOutstandingBytes: 8 * gib,
		}, nil, 8*gib)
		legend := topBarLegend(bar)
		if bar.TotalIsBudget || !strings.Contains(legend, "total 16") || strings.Contains(legend, "budget") ||
			strings.Contains(legend, "unreserved") {
			t.Fatalf("state %q: a card bar must say total/free, legend=%q isBudget=%v", state, legend, bar.TotalIsBudget)
		}
	}
}

// verifies: AIRA-274 (review) — reservations beyond the budget (budget lowered
// under running jobs): free floors at 0 with no free region, the bar says
// OVER-SUBSCRIBED in BUDGET words (not "exceed the VRAM total"); and a ledger
// charge the per-job rows do not account for is named, not silently drawn short.
func TestTopVRAMBarForReservationsOnlyOvercommitAndLedgerGap(t *testing.T) {
	over := topVRAMBarFor(&runner.ConfineSliceReserve{
		VRAMState: runner.VRAMStateNoGPU, VRAMBudgetBytes: 4 * gib, VRAMOutstandingBytes: 10 * gib,
	}, []topBarRegion{{Kind: topRegionScope, Slot: 0, Label: "train", Start: 0, Size: 10 * gib}}, 10*gib)
	if !over.Evaluated || over.Free != 0 || !over.Overcommitted {
		t.Fatalf("want evaluated, free 0, overcommitted: %+v", over)
	}
	for _, region := range over.Regions {
		if region.Kind == topRegionFree {
			t.Fatalf("no free region when over budget: %+v", over.Regions)
		}
	}
	target := tview.NewTextView().SetDynamicColors(true).SetWrap(false)
	target.SetBorder(true)
	target.SetRect(0, 0, 100, topVRAMBarHeight)
	(&tuiRuntime{}).renderTopBar(target, over, panelState{Status: panelReady})
	if text := target.GetText(true); !strings.Contains(text, "OVER-SUBSCRIBED: reserved VRAM exceeds the budget") ||
		strings.Contains(text, "VRAM total") {
		t.Fatalf("over-budget wording: %q", text)
	}

	gap := topVRAMBarFor(&runner.ConfineSliceReserve{
		VRAMState: runner.VRAMStateNoGPU, VRAMBudgetBytes: 14 * gib, VRAMOutstandingBytes: 6 * gib,
	}, nil, 0)
	var named bool
	for _, note := range gap.Notes {
		named = named || strings.Contains(note, "admission ledger reserves")
	}
	if !gap.Evaluated || !named {
		t.Fatalf("a ledger charge with no per-job row must be named: %+v", gap)
	}

	idle := topVRAMBarFor(&runner.ConfineSliceReserve{VRAMState: runner.VRAMStateNoGPU}, nil, 0)
	if idle.Evaluated || strings.Contains(idle.Reason, "aira has reserved") {
		t.Fatalf("nothing reserved and no budget: Reason must not claim a reservation: %q", idle.Reason)
	}
}
