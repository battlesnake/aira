package main

import (
	"strings"
	"testing"

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
		VRAMBudgetBytes: 14 * gib, VRAMHeadroomBytes: gib, VRAMOutstandingBytes: 8 * gib, VRAMJobs: 1,
	}
	scopes := []topBarRegion{{Kind: topRegionScope, Slot: 0, Label: "train", Start: 0, Size: 8 * gib}}
	bar := topVRAMBarFor(reserve, scopes, 8*gib, nil)
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
		bar := topVRAMBarFor(&runner.ConfineSliceReserve{VRAMState: tc.state}, nil, 0, nil)
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
	reserve.VRAMJobs = 1
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
