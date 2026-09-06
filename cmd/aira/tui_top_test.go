package main

import (
	"bytes"
	"context"
	"encoding/json"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"aira/internal/core"
	"aira/internal/daemon"
	"aira/internal/runner"

	"github.com/gdamore/tcell/v2"
)

// AIRA-127 viewmodel tests. Deliberately NOT terminal-rendering or
// golden-screenshot tests, matching how the rest of this TUI is tested: every
// assertion here is against the pure model that the renderer is a thin pass over.
//
// verifies: AIRA-127

const gib = int64(1) << 30

func topTestRecord(scopeID, name string, capBytes, rss int64) runner.ConfineRecord {
	capText := strconv.FormatInt(capBytes, 10)
	live := true
	pid := 4242
	return runner.ConfineRecord{
		Name: name, Owner: "session-a", ScopeID: scopeID, SupervisorPID: &pid,
		Cap: &capText, RSSBytes: &rss, SubtreePopulated: &live,
	}
}

func topTestListing(reserve *runner.ConfineSliceReserve, records ...runner.ConfineRecord) runner.ConfineListResult {
	return runner.ConfineListResult{Verdict: "pass", Scopes: records, SliceReserve: reserve}
}

// topTestFrame is a system/slice frame big enough that nothing in the slot tests
// hits an edge of the bar. The bar arithmetic itself is pinned by (d).
func topTestFrame() *runner.ConfineSliceReserve {
	return &runner.ConfineSliceReserve{
		SystemMemTotalBytes: 64 * gib, SystemMemAvailableBytes: 48 * gib,
		SliceCurrentBytes: 4 * gib, SliceMaxBytes: 32 * gib,
		SliceHighState: runner.ConfineSliceHighNone,
	}
}

func topRowSlots(model panelModel) map[string]string {
	slots := make(map[string]string, len(model.Rows))
	for _, row := range model.Rows {
		slots[row.ID] = row.Cells[0]
	}
	return slots
}

func topRowColours(model panelModel) map[string]string {
	colours := make(map[string]string, len(model.Rows))
	for _, row := range model.Rows {
		colours[row.ID] = row.Colour
	}
	return colours
}

// (a) A reservation keeps the same slot AND the same colour across refresh
// ticks while it stays admitted.
//
// The fixture is built so a size-sorting or usage-sorting implementation cannot
// pass it: across the three ticks the caps and the RSS figures reorder
// completely (B overtakes A, then A overtakes B again), while the scope ids stay
// in the same daemon-sorted order. Anything that re-derives position from a
// changing metric moves a row here; only a held slot does not.
func TestTopViewModelHoldsSlotAndColourAcrossTicks(t *testing.T) {
	ticks := []runner.ConfineListResult{
		topTestListing(topTestFrame(),
			topTestRecord("CONFINE-alpha-101-aa", "alpha", 2*gib, 1*gib),
			topTestRecord("CONFINE-bravo-102-bb", "bravo", 8*gib, 7*gib)),
		topTestListing(topTestFrame(),
			topTestRecord("CONFINE-alpha-101-aa", "alpha", 9*gib, 8*gib),
			topTestRecord("CONFINE-bravo-102-bb", "bravo", 1*gib, 512*1024*1024)),
		topTestListing(topTestFrame(),
			topTestRecord("CONFINE-alpha-101-aa", "alpha", 3*gib, 2*gib),
			topTestRecord("CONFINE-bravo-102-bb", "bravo", 6*gib, 5*gib)),
	}
	var slots []string
	var firstSlots, firstColours map[string]string
	for index, tick := range ticks {
		var model panelModel
		model, slots = topViewModel(slots, tick)
		gotSlots, gotColours := topRowSlots(model), topRowColours(model)
		if index == 0 {
			firstSlots, firstColours = gotSlots, gotColours
			if gotSlots["CONFINE-alpha-101-aa"] != "0" || gotSlots["CONFINE-bravo-102-bb"] != "1" {
				t.Fatalf("first tick slots=%v, want alpha=0 bravo=1", gotSlots)
			}
			if firstColours["CONFINE-alpha-101-aa"] == firstColours["CONFINE-bravo-102-bb"] {
				t.Fatalf("two live reservations share colour %q", firstColours["CONFINE-alpha-101-aa"])
			}
			continue
		}
		if !reflect.DeepEqual(gotSlots, firstSlots) {
			t.Fatalf("tick %d slots=%v, want held %v", index, gotSlots, firstSlots)
		}
		if !reflect.DeepEqual(gotColours, firstColours) {
			t.Fatalf("tick %d colours=%v, want held %v", index, gotColours, firstColours)
		}
	}
}

// (a2) Requirement 6, stated as its own assertion: the colour of a row and the
// colour of that reservation's region in the bar are the same value. A view that
// coloured the bar by position in Regions and the rows by position in Rows would
// pass (a) and still be wrong the moment a scope's cap is unevaluated and it
// therefore has a row but no region.
func TestTopViewModelRowAndBarRegionShareAColour(t *testing.T) {
	uncapped := "max"
	frame := topTestFrame()
	model, _ := topViewModel(nil, topTestListing(frame,
		topTestRecord("CONFINE-alpha-101-aa", "alpha", 2*gib, 1*gib),
		// A scope with no width: it has a row but no bar region, which shifts every
		// later region's index away from its row's index.
		runner.ConfineRecord{Name: "bravo", ScopeID: "CONFINE-bravo-102-bb", Cap: &uncapped},
		topTestRecord("CONFINE-charlie-103-cc", "charlie", 3*gib, 2*gib)))
	colours := topRowColours(model)
	byName := map[string]string{}
	for _, region := range model.Bar.Regions {
		if region.Kind == topRegionScope {
			byName[region.Label] = region.Colour
		}
	}
	if got, want := byName["alpha"], colours["CONFINE-alpha-101-aa"]; got != want {
		t.Fatalf("alpha region colour=%q, row colour=%q", got, want)
	}
	if got, want := byName["charlie"], colours["CONFINE-charlie-103-cc"]; got != want {
		t.Fatalf("charlie region colour=%q, row colour=%q", got, want)
	}
	if _, drawn := byName["bravo"]; drawn {
		t.Fatal("an uncapped scope was drawn a bar region; its claim has no width")
	}
	if len(model.Bar.Notes) == 0 {
		t.Fatal("an undrawn scope was not named in the bar's notes")
	}
}

// (b) A new admission is APPENDED. No existing entry's slot moves.
//
// The new scope's id sorts FIRST (CONFINE-aaron-...), which is what makes this
// non-porous: an implementation that rebuilds the table from the daemon's sorted
// listing each tick gives the newcomer slot 0 and pushes both incumbents down.
func TestTopViewModelAppendsANewAdmissionWithoutMovingExistingSlots(t *testing.T) {
	first, slots := topViewModel(nil, topTestListing(topTestFrame(),
		topTestRecord("CONFINE-mike-101-aa", "mike", 2*gib, 1*gib),
		topTestRecord("CONFINE-november-102-bb", "november", 3*gib, 2*gib)))
	before := topRowSlots(first)
	beforeColours := topRowColours(first)

	second, slots := topViewModel(slots, topTestListing(topTestFrame(),
		topTestRecord("CONFINE-aaron-103-cc", "aaron", 4*gib, 3*gib),
		topTestRecord("CONFINE-mike-101-aa", "mike", 2*gib, 1*gib),
		topTestRecord("CONFINE-november-102-bb", "november", 3*gib, 2*gib)))
	after := topRowSlots(second)
	afterColours := topRowColours(second)

	for id, slot := range before {
		if after[id] != slot {
			t.Fatalf("%s moved from slot %s to %s on a new admission", id, slot, after[id])
		}
		if afterColours[id] != beforeColours[id] {
			t.Fatalf("%s changed colour from %s to %s on a new admission", id, beforeColours[id], afterColours[id])
		}
	}
	if after["CONFINE-aaron-103-cc"] != "2" {
		t.Fatalf("new admission took slot %s, want the appended slot 2", after["CONFINE-aaron-103-cc"])
	}
	if want := []string{"CONFINE-mike-101-aa", "CONFINE-november-102-bb", "CONFINE-aaron-103-cc"}; !reflect.DeepEqual(slots, want) {
		t.Fatalf("slot table=%v, want %v", slots, want)
	}
}

// (c) An exit FREES its slot, leaving a hole. The survivors do not reflow, and
// the hole is filled only by a LATER new admission.
//
// Non-porous in both directions: a compacting implementation moves charlie from
// slot 2 to slot 1 at the exit tick, and an append-only implementation (one that
// never reuses a freed slot) gives delta slot 3 instead of the freed 1.
func TestTopViewModelFreesAnExitedSlotForALaterAdmissionOnly(t *testing.T) {
	alpha := topTestRecord("CONFINE-alpha-101-aa", "alpha", 2*gib, 1*gib)
	bravo := topTestRecord("CONFINE-bravo-102-bb", "bravo", 3*gib, 2*gib)
	charlie := topTestRecord("CONFINE-charlie-103-cc", "charlie", 4*gib, 3*gib)

	_, slots := topViewModel(nil, topTestListing(topTestFrame(), alpha, bravo, charlie))
	if want := []string{"CONFINE-alpha-101-aa", "CONFINE-bravo-102-bb", "CONFINE-charlie-103-cc"}; !reflect.DeepEqual(slots, want) {
		t.Fatalf("initial slot table=%v, want %v", slots, want)
	}

	exited, slots := topViewModel(slots, topTestListing(topTestFrame(), alpha, charlie))
	held := topRowSlots(exited)
	if held["CONFINE-alpha-101-aa"] != "0" || held["CONFINE-charlie-103-cc"] != "2" {
		t.Fatalf("after bravo exited slots=%v, want alpha=0 charlie=2 (no reflow)", held)
	}
	if want := []string{"CONFINE-alpha-101-aa", topSlotFree, "CONFINE-charlie-103-cc"}; !reflect.DeepEqual(slots, want) {
		t.Fatalf("slot table after exit=%v, want a hole at 1: %v", slots, want)
	}

	delta := topTestRecord("CONFINE-delta-104-dd", "delta", 5*gib, 4*gib)
	reused, slots := topViewModel(slots, topTestListing(topTestFrame(), alpha, charlie, delta))
	after := topRowSlots(reused)
	if after["CONFINE-alpha-101-aa"] != "0" || after["CONFINE-charlie-103-cc"] != "2" {
		t.Fatalf("survivors moved on the later admission: %v", after)
	}
	if after["CONFINE-delta-104-dd"] != "1" {
		t.Fatalf("later admission took slot %s, want the freed slot 1", after["CONFINE-delta-104-dd"])
	}
	if want := []string{"CONFINE-alpha-101-aa", "CONFINE-delta-104-dd", "CONFINE-charlie-103-cc"}; !reflect.DeepEqual(slots, want) {
		t.Fatalf("slot table after reuse=%v, want %v", slots, want)
	}
	// The rows are emitted in SLOT order, not in the daemon's id order, or the
	// bar's stack and the list would disagree about which region is which row.
	gotOrder := []string{reused.Rows[0].ID, reused.Rows[1].ID, reused.Rows[2].ID}
	if want := []string{"CONFINE-alpha-101-aa", "CONFINE-delta-104-dd", "CONFINE-charlie-103-cc"}; !reflect.DeepEqual(gotOrder, want) {
		t.Fatalf("row order=%v, want slot order %v", gotOrder, want)
	}
}

// (d) The bar's arithmetic and its column offsets, over representative
// RAM/limit configurations.
func TestTopViewModelBarGeometry(t *testing.T) {
	type wantRegion struct {
		kind       topBarRegionKind
		startBytes int64
		bytes      int64
		startCol   int
		endCol     int
	}
	cases := []struct {
		name    string
		reserve *runner.ConfineSliceReserve
		records []runner.ConfineRecord
		width   int

		wantClaimed      int64
		wantOutside      int64
		wantOutsideKnown bool
		wantFree         int64
		wantOvercommit   bool
		wantRegions      []wantRegion
		wantMarkers      map[string]int64
		wantMarkerCols   map[string]int
	}{
		{
			// 64 GiB machine, 1 GiB per column at width 64.
			name: "throttled-slice-with-scopeless-reservations",
			reserve: &runner.ConfineSliceReserve{
				SystemMemTotalBytes: 64 * gib, SystemMemAvailableBytes: 40 * gib,
				SliceCurrentBytes: 10 * gib, SliceReclaimableBytes: 2 * gib,
				SliceMaxBytes: 32 * gib, SliceHighBytes: 28 * gib, SliceHighState: runner.ConfineSliceHighSet,
				CeilingEffectiveBytes: 24 * gib,
				ReservationJobs:       3, ReservationBytes: 2 * gib,
			},
			records: []runner.ConfineRecord{
				topTestRecord("CONFINE-alpha-101-aa", "alpha", 4*gib, 3*gib),
				topTestRecord("CONFINE-bravo-102-bb", "bravo", 6*gib, 5*gib),
			},
			width: 64,
			// stacked reservations: 4 + 6 scope caps, plus the 2 GiB scope-less hold.
			wantClaimed: 12 * gib,
			// system used (64-40=24) minus the slice's non-reclaimable share (10-2=8).
			wantOutside: 16 * gib, wantOutsideKnown: true,
			wantFree: 36 * gib,
			wantRegions: []wantRegion{
				{topRegionScope, 0, 4 * gib, 0, 4},
				{topRegionScope, 4 * gib, 6 * gib, 4, 10},
				{topRegionScopeless, 10 * gib, 2 * gib, 10, 12},
				{topRegionFree, 12 * gib, 36 * gib, 12, 48},
				{topRegionOutside, 48 * gib, 16 * gib, 48, 64},
			},
			wantMarkers:    map[string]int64{topMarkerSoft: 28 * gib, topMarkerHard: 32 * gib, topMarkerCeiling: 24 * gib},
			wantMarkerCols: map[string]int{topMarkerSoft: 28, topMarkerHard: 32, topMarkerCeiling: 24},
		},
		{
			// 16 GiB machine at width 16: the caps handed out plus what the rest of
			// the system is holding exceed total RAM, which is a real state on a
			// slice with an over-subscription allowance and must be named.
			name: "over-subscribed-no-soft-limit-no-ceiling",
			reserve: &runner.ConfineSliceReserve{
				SystemMemTotalBytes: 16 * gib, SystemMemAvailableBytes: 2 * gib,
				SliceCurrentBytes: 6 * gib,
				SliceMaxBytes:     12 * gib, SliceHighState: runner.ConfineSliceHighNone,
			},
			records: []runner.ConfineRecord{
				topTestRecord("CONFINE-alpha-101-aa", "alpha", 8*gib, 7*gib),
				topTestRecord("CONFINE-bravo-102-bb", "bravo", 6*gib, 5*gib),
			},
			width:       16,
			wantClaimed: 14 * gib,
			wantOutside: 8 * gib, wantOutsideKnown: true,
			wantFree: 0, wantOvercommit: true,
			wantRegions: []wantRegion{
				{topRegionScope, 0, 8 * gib, 0, 8},
				{topRegionScope, 8 * gib, 6 * gib, 8, 14},
				{topRegionOutside, 8 * gib, 8 * gib, 8, 16},
			},
			// No soft limit is CONFIGURED (state "none"), and the ceiling subsystem
			// is off, so neither produces a marker. A tick at column 0 for either
			// would state a slice throttled to nothing.
			wantMarkers:    map[string]int64{topMarkerHard: 12 * gib},
			wantMarkerCols: map[string]int{topMarkerHard: 12},
		},
		{
			// MemAvailable could not be established. The stack is still real, the
			// grey is NOT drawn, and the gap must not silently absorb it.
			name: "mem-available-unevaluated",
			reserve: &runner.ConfineSliceReserve{
				SystemMemTotalBytes: 8 * gib,
				SliceCurrentBytes:   2 * gib,
				SliceMaxBytes:       4 * gib, SliceHighState: runner.ConfineSliceHighUnevaluated,
			},
			records:     []runner.ConfineRecord{topTestRecord("CONFINE-alpha-101-aa", "alpha", 2*gib, 1*gib)},
			width:       8,
			wantClaimed: 2 * gib,
			wantOutside: 0, wantOutsideKnown: false,
			wantFree: 6 * gib,
			wantRegions: []wantRegion{
				{topRegionScope, 0, 2 * gib, 0, 2},
				{topRegionFree, 2 * gib, 6 * gib, 2, 8},
			},
			wantMarkers:    map[string]int64{topMarkerHard: 4 * gib},
			wantMarkerCols: map[string]int{topMarkerHard: 4},
		},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			model, _ := topViewModel(nil, topTestListing(testCase.reserve, testCase.records...))
			bar := model.Bar
			if bar == nil || !bar.Evaluated {
				t.Fatalf("bar=%+v, want an evaluated bar", bar)
			}
			if bar.TotalBytes != testCase.reserve.SystemMemTotalBytes {
				t.Fatalf("total=%d, want %d", bar.TotalBytes, testCase.reserve.SystemMemTotalBytes)
			}
			if bar.ClaimedBytes != testCase.wantClaimed {
				t.Fatalf("claimed=%d, want %d", bar.ClaimedBytes, testCase.wantClaimed)
			}
			if bar.OutsideKnown != testCase.wantOutsideKnown || bar.OutsideBytes != testCase.wantOutside {
				t.Fatalf("outside=%d known=%v, want %d known=%v", bar.OutsideBytes, bar.OutsideKnown, testCase.wantOutside, testCase.wantOutsideKnown)
			}
			if bar.FreeBytes != testCase.wantFree {
				t.Fatalf("free=%d, want %d", bar.FreeBytes, testCase.wantFree)
			}
			if bar.Overcommitted != testCase.wantOvercommit {
				t.Fatalf("overcommitted=%v, want %v", bar.Overcommitted, testCase.wantOvercommit)
			}
			if len(bar.Regions) != len(testCase.wantRegions) {
				t.Fatalf("regions=%+v, want %d of them", bar.Regions, len(testCase.wantRegions))
			}
			for index, want := range testCase.wantRegions {
				got := bar.Regions[index]
				if got.Kind != want.kind || got.StartBytes != want.startBytes || got.Bytes != want.bytes {
					t.Fatalf("region %d = %+v, want kind=%d start=%d bytes=%d", index, got, want.kind, want.startBytes, want.bytes)
				}
				startCol := topBarColumn(got.StartBytes, bar.TotalBytes, testCase.width)
				endCol := topBarColumn(got.StartBytes+got.Bytes, bar.TotalBytes, testCase.width)
				if startCol != want.startCol || endCol != want.endCol {
					t.Fatalf("region %d columns=[%d,%d), want [%d,%d)", index, startCol, endCol, want.startCol, want.endCol)
				}
			}
			gotMarkers := make(map[string]int64, len(bar.Markers))
			for _, marker := range bar.Markers {
				gotMarkers[marker.Name] = marker.Bytes
			}
			if !reflect.DeepEqual(gotMarkers, testCase.wantMarkers) {
				t.Fatalf("markers=%v, want %v", gotMarkers, testCase.wantMarkers)
			}
			cells := topBarCells(bar, testCase.width)
			if len(cells) != testCase.width {
				t.Fatalf("cells=%d, want %d", len(cells), testCase.width)
			}
			gotCols := make(map[string]int, len(bar.Markers))
			for column, cell := range cells {
				if cell.Marker != "" {
					gotCols[cell.Marker] = column
				}
			}
			if !reflect.DeepEqual(gotCols, testCase.wantMarkerCols) {
				t.Fatalf("marker columns=%v, want %v", gotCols, testCase.wantMarkerCols)
			}
			// The grey region is anchored to the RIGHT EDGE, which is the property
			// that makes the visible gap readable as free RAM. Asserted on the
			// painted cells, because a right-anchored model painted left-to-right
			// from the stack would still satisfy the byte assertions above.
			if testCase.wantOutsideKnown && testCase.wantOutside > 0 {
				if last := cells[testCase.width-1]; last.Kind != topRegionOutside && last.Marker == "" {
					t.Fatalf("last column=%+v, want the out-of-slice region anchored to the right edge", last)
				}
			}
		})
	}
}

// A bar the model cannot evaluate is NOT drawn as an empty one: an empty bar
// states that the machine is idle, which is a fabricated fact.
func TestTopViewModelRefusesToDrawAnUnevaluatedBar(t *testing.T) {
	for _, testCase := range []struct {
		name    string
		listing runner.ConfineListResult
	}{
		{"no-slice-reserve", topTestListing(nil, topTestRecord("CONFINE-alpha-101-aa", "alpha", gib, gib))},
		{"no-mem-total", topTestListing(&runner.ConfineSliceReserve{SliceMaxBytes: 4 * gib})},
		{"ci-shim", topTestListing(&runner.ConfineSliceReserve{
			SystemMemTotalBytes: 64 * gib, Containment: string(runner.ConfineContainmentAdvisory)})},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			model, _ := topViewModel(nil, testCase.listing)
			if model.Bar == nil || model.Bar.Evaluated {
				t.Fatalf("bar=%+v, want an unevaluated bar", model.Bar)
			}
			if model.Bar.Reason == "" {
				t.Fatal("an unevaluated bar carried no reason")
			}
			if cells := topBarCells(model.Bar, 40); cells != nil {
				t.Fatalf("cells=%v, want none for an unevaluated bar", cells)
			}
		})
	}
}

// An unevaluated LISTING must not free every slot: a transient read failure is
// not evidence that anything exited, and freeing on it would recolour the whole
// view and then recolour it back.
func TestTopViewModelCarriesSlotsThroughAnUnevaluatedListing(t *testing.T) {
	_, slots := topViewModel(nil, topTestListing(topTestFrame(),
		topTestRecord("CONFINE-alpha-101-aa", "alpha", 2*gib, gib),
		topTestRecord("CONFINE-bravo-102-bb", "bravo", 3*gib, gib)))
	model, after := topViewModel(slots, runner.ConfineListResult{Verdict: "unevaluated", Reason: "read-error"})
	if !reflect.DeepEqual(after, slots) {
		t.Fatalf("slots after an unevaluated listing=%v, want them carried forward %v", after, slots)
	}
	if model.Bar == nil || model.Bar.Evaluated || model.Footer == "" {
		t.Fatalf("model=%+v, want an unevaluated bar and a stated footer", model)
	}
}

// assignTopSlots is the mechanism the three slot tests above exercise through
// the viewmodel. This pins its contract directly, including the append path once
// the table has no hole left.
func TestAssignTopSlots(t *testing.T) {
	cases := []struct {
		name     string
		previous []string
		live     []string
		want     []string
	}{
		{"first-fill", nil, []string{"a", "b"}, []string{"a", "b"}},
		{"hold", []string{"a", "b"}, []string{"b", "a"}, []string{"a", "b"}},
		{"free-leaves-a-hole", []string{"a", "b", "c"}, []string{"a", "c"}, []string{"a", topSlotFree, "c"}},
		{"lowest-hole-first", []string{topSlotFree, "b", topSlotFree, "d"}, []string{"b", "d", "x", "y"}, []string{"x", "b", "y", "d"}},
		{"append-when-full", []string{"a", "b"}, []string{"a", "b", "c"}, []string{"a", "b", "c"}},
		{"trailing-frees-trimmed", []string{"a", "b", "c"}, []string{"a"}, []string{"a"}},
		{"all-gone", []string{"a", "b"}, nil, nil},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			got := assignTopSlots(testCase.previous, testCase.live)
			if len(got) == 0 && len(testCase.want) == 0 {
				return
			}
			if !reflect.DeepEqual(got, testCase.want) {
				t.Fatalf("assignTopSlots(%v, %v)=%v, want %v", testCase.previous, testCase.live, got, testCase.want)
			}
		})
	}
}

// The top view shows every quantity in one fixed unit (M) rather than the
// shared confine formatter's auto-scaling T/G/M/K-or-bytes, because a live
// cgroup/RSS reading is essentially never round in any unit and would mostly
// fall through to raw bytes there. Rounds to the nearest MiB; never negative,
// never a fabricated non-zero for a non-positive input.
func TestTopFormatMegabytesRoundsToTheNearestMiBInOneFixedUnit(t *testing.T) {
	const mib = 1 << 20
	for _, testCase := range []struct {
		name  string
		value int64
		want  string
	}{
		{"zero", 0, "0M"},
		{"negative", -1, "0M"},
		{"exact", 2 * mib, "2M"},
		{"rounds down", 2*mib + mib/2 - 1, "2M"},
		{"rounds up", 2*mib + mib/2, "3M"},
		{"large, not a round G/T value", 47*mib*1024 + 12345, "48128M"},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			if got := topFormatMegabytes(testCase.value); got != testCase.want {
				t.Fatalf("topFormatMegabytes(%d)=%q, want %q", testCase.value, got, testCase.want)
			}
		})
	}
}

// The reserve column reads the cap `confine --list` already reports, and keeps
// its three states apart: a number, an uncapped `max`, and an unreadable field.
// Collapsing the last two would draw an unknown claim as an unlimited one.
func TestTopReserveKeepsItsThreeStatesApart(t *testing.T) {
	numeric, uncapped, junk := "2147483648", "max", "not-a-number"
	for _, testCase := range []struct {
		name   string
		record runner.ConfineRecord
		want   topReserve
		text   string
	}{
		{"set", runner.ConfineRecord{Cap: &numeric}, topReserve{Bytes: 2 * gib, State: topReserveSet}, "2048M"},
		{"uncapped", runner.ConfineRecord{Cap: &uncapped}, topReserve{State: topReserveUncapped}, "max (uncapped)"},
		{"nil", runner.ConfineRecord{}, topReserve{State: topReserveUnevaluated}, "unevaluated"},
		{"unparsable", runner.ConfineRecord{Cap: &junk}, topReserve{State: topReserveUnevaluated}, "unevaluated"},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			got := topReserveFor(testCase.record)
			if got != testCase.want {
				t.Fatalf("topReserveFor=%+v, want %+v", got, testCase.want)
			}
			if got.String() != testCase.text {
				t.Fatalf("rendered %q, want %q", got.String(), testCase.text)
			}
		})
	}
}

// The slot table survives the REDUCER, not just the viewmodel function: the top
// panel's model is rebuilt on every fetch result, and the slots have to come
// from state each time or requirement 7 holds only in the unit test.
func TestTopControllerHoldsSlotsAcrossFetchResults(t *testing.T) {
	state := newTUIStateForViews(8, topOnlyViews, nil)
	if state.Active != viewTop {
		t.Fatalf("active=%s, want the top view", state.Active)
	}
	// One turn of the REAL loop: the pending tick fires (or the first fetch is
	// requested), the reply lands, and the reducer decides whether to schedule the
	// next tick. Driving it any other way would leave the one-tick-in-flight rule
	// untested and let a build that queues a backlog of fetches pass.
	deliver := func(state tuiState, listing runner.ConfineListResult) (tuiState, []tuiCmd) {
		if state.PendingRefresh[viewTop] {
			state, _ = onTUIRefreshDue(state, viewTop)
		} else {
			state, _ = requestPanelRefresh(state, viewTop)
		}
		panel := state.Panels[viewTop]
		return onTUIFetchResult(state, fetchResult{View: viewTop, Generation: panel.InFlightGeneration, Top: &listing})
	}
	alpha := topTestRecord("CONFINE-alpha-101-aa", "alpha", 2*gib, gib)
	bravo := topTestRecord("CONFINE-bravo-102-bb", "bravo", 3*gib, gib)
	state, _ = deliver(state, topTestListing(topTestFrame(), alpha, bravo))
	first := topRowSlots(state.Panels[viewTop].Model)

	// bravo exits, then a NEW scope whose id sorts first arrives.
	state, _ = deliver(state, topTestListing(topTestFrame(), alpha))
	aaron := topTestRecord("CONFINE-aaron-103-cc", "aaron", 4*gib, gib)
	state, commands := deliver(state, topTestListing(topTestFrame(), aaron, alpha))
	after := topRowSlots(state.Panels[viewTop].Model)
	if after["CONFINE-alpha-101-aa"] != first["CONFINE-alpha-101-aa"] {
		t.Fatalf("alpha moved through the reducer: %v then %v", first, after)
	}
	if after["CONFINE-aaron-103-cc"] != "1" {
		t.Fatalf("the later admission took slot %s, want the freed slot 1", after["CONFINE-aaron-103-cc"])
	}
	// And the live tick keeps itself going while the view is on screen.
	if len(commands) != 1 || commands[0].Kind != cmdScheduleRefresh || commands[0].View != viewTop || commands[0].Backoff != topRefreshInterval {
		t.Fatalf("commands=%+v, want one %v tick for the top view", commands, topRefreshInterval)
	}
}

// The tick is bounded by what the operator is looking at. A panel nobody is
// watching must not poll the daemon once a second for a cgroup scan.
func TestTopControllerTicksOnlyWhileTheViewIsActive(t *testing.T) {
	state := newTUIState(8)
	state.Active = viewTickets
	state, _ = requestPanelRefresh(state, viewTop)
	listing := topTestListing(topTestFrame(), topTestRecord("CONFINE-alpha-101-aa", "alpha", gib, gib))
	state, commands := onTUIFetchResult(state, fetchResult{
		View: viewTop, Generation: state.Panels[viewTop].InFlightGeneration, Top: &listing,
	})
	if len(commands) != 0 {
		t.Fatalf("commands=%+v, want no tick while another view is active", commands)
	}
	// Selecting it starts it.
	state, commands = onTUIKey(state, '7', nil)
	if state.Active != viewTop {
		t.Fatalf("active=%s, want the top view on key 7", state.Active)
	}
	if len(commands) != 1 || commands[0].Kind != cmdFetch || commands[0].View != viewTop {
		t.Fatalf("commands=%+v, want the top view's first fetch on activation", commands)
	}
}

// A viewTop result carrying no listing is a MALFORMED fetch, not an empty slice.
// Reporting it as an error and keeping the previous model and slots is the
// honest response; rendering an empty table would state that the slice is idle.
func TestTopControllerMalformedFetchKeepsTheLastModelAndSlots(t *testing.T) {
	state := newTUIStateForViews(8, topOnlyViews, nil)
	state, _ = requestPanelRefresh(state, viewTop)
	listing := topTestListing(topTestFrame(), topTestRecord("CONFINE-alpha-101-aa", "alpha", 2*gib, gib))
	state, _ = onTUIFetchResult(state, fetchResult{View: viewTop, Generation: state.Panels[viewTop].InFlightGeneration, Top: &listing})
	before, slots := state.Panels[viewTop].Model, append([]string(nil), state.TopSlots...)

	state, _ = requestPanelRefresh(state, viewTop)
	state, _ = onTUIFetchResult(state, fetchResult{View: viewTop, Generation: state.Panels[viewTop].InFlightGeneration})
	panel := state.Panels[viewTop]
	if panel.Status != panelError || panel.ErrorCode != tuiDecodeError {
		t.Fatalf("panel=%+v, want an error naming %s", panel, tuiDecodeError)
	}
	if !reflect.DeepEqual(panel.Model.Rows, before.Rows) || !reflect.DeepEqual(state.TopSlots, slots) {
		t.Fatalf("a malformed fetch discarded the last model/slots: rows=%+v slots=%v", panel.Model.Rows, state.TopSlots)
	}
}

// `aira top` refuses --json and positional arguments BEFORE it touches a
// dispatcher or a project, so both refusals are reachable without a daemon.
func TestTopCLIRefusesJSONAndPositionals(t *testing.T) {
	for _, testCase := range []struct {
		name string
		argv []string
	}{
		{"json", []string{"top", "--json"}},
		{"positional", []string{"top", "extra"}},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			if exit := runWithInput(testCase.argv, &stdout, &stderr, strings.NewReader("")); exit == 0 {
				t.Fatalf("exit=0 for %v; stdout=%q stderr=%q", testCase.argv, stdout.String(), stderr.String())
			}
		})
	}
}

// `top` resolves no project, so it must REFUSE --scope-dir rather than accept
// and discard it (AIRA-82's rule, applied to the new verb).
func TestTopCLIRefusesScopeDir(t *testing.T) {
	if verbAcceptsScopeDir("top") {
		t.Fatal("top accepts --scope-dir, but it resolves no project scope")
	}
}

// topSmokeDispatcher answers exactly what `aira top` asks for and refuses
// everything else, so a runtime that dispatched a project-scoped verb (or opened
// the event-watch loop it has no project for) fails here rather than silently
// spinning a reconnect backoff behind the view.
type topSmokeDispatcher struct {
	mu       sync.Mutex
	calls    int
	unwanted []string
}

func (d *topSmokeDispatcher) Dispatch(_ context.Context, scope daemon.WorktreeScope, request core.Request) core.Response {
	d.mu.Lock()
	defer d.mu.Unlock()
	if request.Verb != "confine-list" || scope.WorktreeID != "" || scope.Root != "" {
		d.unwanted = append(d.unwanted, request.Verb)
		return core.Response{Code: "E_UNKNOWN_VERB", Error: "unexpected " + request.Verb}
	}
	d.calls++
	raw, err := json.Marshal(topTestListing(&runner.ConfineSliceReserve{
		SystemMemTotalBytes: 64 * gib, SystemMemAvailableBytes: 40 * gib,
		SliceCurrentBytes: 8 * gib, SliceMaxBytes: 32 * gib,
		SliceHighBytes: 28 * gib, SliceHighState: runner.ConfineSliceHighSet,
		GrantedBytes: 10 * gib, CeilingBytes: 20 * gib, Jobs: 2, ScopeJobs: 2, FreezePhase: "idle",
	}, topTestRecord("CONFINE-alpha-101-aa", "alpha", 4*gib, 3*gib),
		topTestRecord("CONFINE-bravo-102-bb", "bravo", 6*gib, 5*gib)))
	if err != nil {
		return core.Response{Code: "E_INTERNAL", Error: err.Error()}
	}
	return core.Response{OK: true, Code: "OK", RawData: raw}
}

func (d *topSmokeDispatcher) snapshot() (int, []string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.calls, append([]string(nil), d.unwanted...)
}

// End-to-end over the REAL runtime on a simulation screen: `aira top` starts,
// fetches, paints a bar and a row table, and quits. It is the one test here that
// crosses the face boundary, and it exists because everything above it asserts a
// model that a broken widget wiring would never render.
func TestTopRuntimeRendersAndQuits(t *testing.T) {
	dispatcher := &topSmokeDispatcher{}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	screen := tcell.NewSimulationScreen("UTF-8")
	screen.SetSize(120, 30)
	runtime := newTopRuntime(ctx, dispatcher, nil, nil, nil, screen)
	drawn := make(chan struct{})
	var once sync.Once
	runtime.app.SetAfterDrawFunc(func(tcell.Screen) { once.Do(func() { close(drawn) }) })
	done := make(chan error, 1)
	go func() { done <- runtime.run() }()
	select {
	case <-drawn:
	case <-time.After(2 * time.Second):
		t.Fatal("initial draw timed out")
	}
	deadline := time.Now().Add(2 * time.Second)
	var rendered string
	for time.Now().Before(deadline) {
		text := make(chan string, 1)
		go runtime.app.QueueUpdateDraw(func() { text <- runtime.topBar.GetText(true) })
		select {
		case rendered = <-text:
		case <-time.After(200 * time.Millisecond):
			t.Fatal("the tview loop stopped answering")
		}
		if strings.Contains(rendered, "total ") {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if !strings.Contains(rendered, "total 65536M") || !strings.Contains(rendered, "soft (memory.high) 28672M") {
		t.Fatalf("bar text=%q, want the totals and the soft-limit legend", rendered)
	}
	if !strings.Contains(rendered, "█") {
		t.Fatalf("bar text=%q, want painted regions", rendered)
	}
	rows := make(chan int, 1)
	go runtime.app.QueueUpdateDraw(func() { rows <- runtime.tables[viewTop].GetRowCount() })
	select {
	case count := <-rows:
		if count != 3 { // one header plus two scopes
			t.Fatalf("table rows=%d, want a header and two scopes", count)
		}
	case <-time.After(time.Second):
		t.Fatal("table read timed out")
	}
	screen.InjectKey(tcell.KeyRune, 'q', tcell.ModNone)
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("run=%v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("quit timed out")
	}
	calls, unwanted := dispatcher.snapshot()
	if calls == 0 {
		t.Fatal("the top view never dispatched confine-list")
	}
	if len(unwanted) != 0 {
		t.Fatalf("top dispatched %v with a project scope or an unexpected verb; it resolves no project", unwanted)
	}
}

// AIRA-135 (e). The column SET, and its values, against data that would have been
// truncated or meaningless under AIRA-127's columns.
//
// The record is deliberately built to blow every old hard-sized column: a 42 GiB
// reservation whose M-suffixed value does not fit a narrow RESERVE column, a long
// hex-looking scope id and owner, and a real wrapped command. What is asserted is
// the exact header list and the exact cell list -- not "contains" -- because the
// whole ticket is that the OLD columns had to GO, and a widening-only change
// would satisfy any weaker assertion.
//
// verifies: AIRA-135
func TestTopViewModelColumnsAreSlotNamePIDLiveReservationCommand(t *testing.T) {
	command := "go test ./... -count=1"
	record := topTestRecord("CONFINE-heavy-suite-31415-9f3ac1de@session-9f3ac1de", "heavy-suite", 42160*(1<<20), 9*gib)
	record.Command = &command
	model, _ := topViewModel(nil, topTestListing(topTestFrame(), record))

	wantHeaders := []string{"SLOT", "NAME", "PID", "LIVE", "RESERVATION", "COMMAND"}
	if !reflect.DeepEqual(model.Headers, wantHeaders) {
		t.Fatalf("headers=%v, want %v", model.Headers, wantHeaders)
	}
	if len(model.Rows) != 1 {
		t.Fatalf("rows=%+v, want one", model.Rows)
	}
	wantCells := []string{"0", "heavy-suite", "4242", "yes", "42160M", command}
	if !reflect.DeepEqual(model.Rows[0].Cells, wantCells) {
		t.Fatalf("cells=%v, want %v", model.Rows[0].Cells, wantCells)
	}
	// Every cell is present in full: nothing in the viewmodel may pre-truncate a
	// value, because the table is the only thing that knows the terminal's width.
	for index, cell := range model.Rows[0].Cells {
		if cell != wantCells[index] {
			t.Fatalf("cell %d=%q, want the untruncated %q", index, cell, wantCells[index])
		}
	}
	// The dropped columns must be gone from the header row AND from the data, or
	// the hex is still on screen under a different name.
	for _, gone := range []string{"OWNER", "SCOPE-ID", "RSS", "AGE"} {
		if containsString(model.Headers, gone) {
			t.Fatalf("headers still carry the dropped column %s: %v", gone, model.Headers)
		}
	}
	for _, gone := range []string{record.Owner, record.ScopeID} {
		if containsString(model.Rows[0].Cells, gone) {
			t.Fatalf("row still renders the dropped value %q: %v", gone, model.Rows[0].Cells)
		}
	}
}

// A command that could not be established renders as "unevaluated", never as a
// blank cell -- a blank would read as a job running nothing at all.
//
// verifies: AIRA-135
func TestTopCommandCellSaysUnevaluatedAndIsTerminalSafe(t *testing.T) {
	if got := topCommandCell(nil); got != "unevaluated" {
		t.Fatalf("topCommandCell(nil)=%q, want unevaluated", got)
	}
	plain := "go build ./..."
	if got := topCommandCell(&plain); got != plain {
		t.Fatalf("topCommandCell=%q, want %q unchanged", got, plain)
	}
	// The value is another process's argv reaching a terminal. A control
	// character that could rewrite the line, and tview's own colour-tag syntax,
	// must both survive as VISIBLE TEXT rather than being executed or swallowed.
	hostile := "sh -c \x1b[2Kwiped\ttab [red]not-a-tag"
	got := topCommandCell(&hostile)
	if strings.ContainsAny(got, "\x1b\t") {
		t.Fatalf("topCommandCell=%q, want the control characters escaped", got)
	}
	// tview parses colour tags in a table cell, so an ARGUMENT that looks like one
	// must arrive in tview's escaped form (`[red[]`) and never as a live tag.
	if strings.Contains(got, "[red]") {
		t.Fatalf("topCommandCell=%q, want tview's tag syntax neutralised", got)
	}
	if !strings.Contains(got, "[red[]") {
		t.Fatalf("topCommandCell=%q, want the argument preserved as escaped text", got)
	}
	if !strings.Contains(got, "not-a-tag") {
		t.Fatalf("topCommandCell=%q, want the rest of the argv intact", got)
	}
}

// AIRA-135 (c)+(d). The used/unused split INSIDE one reservation's region.
//
// Every case shares one frame so the byte-to-column mapping is a constant 1 GiB
// per column, and each asserts three separate things: the region's total width is
// still the RESERVATION (the next region must not move), the bright span is
// exactly the live usage, and the darkened span is the rest.
//
// verifies: AIRA-135
func TestTopBarRegionSplitsUsedFromReservedButUnused(t *testing.T) {
	const width = 64
	frame := &runner.ConfineSliceReserve{
		SystemMemTotalBytes: 64 * gib, SystemMemAvailableBytes: 64 * gib,
		SliceMaxBytes: 32 * gib, SliceHighState: runner.ConfineSliceHighNone,
	}
	for _, testCase := range []struct {
		name       string
		reserved   int64
		rss        *int64
		wantUsed   int64
		wantKnown  bool
		wantBright int
		wantShaded int
	}{
		{"half-used", 8 * gib, int64Pointer(4 * gib), 4 * gib, true, 4, 4},
		{"fully-used", 8 * gib, int64Pointer(8 * gib), 8 * gib, true, 8, 0},
		// A monitoring-lag overshoot right before an OOM. The used shade is
		// CLAMPED to this region; it must never bleed into the next slot's.
		{"used-exceeds-the-reservation", 8 * gib, int64Pointer(11 * gib), 8 * gib, true, 8, 0},
		// An established zero is not an absence: the whole region is darkened.
		{"used-is-zero", 8 * gib, int64Pointer(0), 0, true, 0, 8},
		// Unevaluated usage draws ONE undivided shade rather than a fabricated
		// 0%-used split, which is what a nil-means-zero build would paint.
		{"usage-unevaluated", 8 * gib, nil, 0, false, 8, 0},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			record := topTestRecord("CONFINE-alpha-101-aa", "alpha", testCase.reserved, 0)
			record.RSSBytes = testCase.rss
			// A second scope AFTER it, to pin that the split cannot move the next
			// region's start or steal its columns.
			next := topTestRecord("CONFINE-bravo-102-bb", "bravo", 4*gib, 1*gib)
			model, _ := topViewModel(nil, topTestListing(frame, record, next))
			region := model.Bar.Regions[0]
			if region.Bytes != testCase.reserved || region.StartBytes != 0 {
				t.Fatalf("region=%+v, want the whole reservation from 0", region)
			}
			if region.UsedKnown != testCase.wantKnown || region.UsedBytes != testCase.wantUsed {
				t.Fatalf("region used=%d known=%v, want %d known=%v",
					region.UsedBytes, region.UsedKnown, testCase.wantUsed, testCase.wantKnown)
			}
			if after := model.Bar.Regions[1]; after.StartBytes != testCase.reserved {
				t.Fatalf("next region starts at %d, want %d; the split moved it", after.StartBytes, testCase.reserved)
			}

			cells := topBarCells(model.Bar, width)
			bright, shaded := 0, 0
			for column := 0; column < int(testCase.reserved/gib); column++ {
				cell := cells[column]
				if cell.Kind != topRegionScope || cell.Slot != 0 {
					t.Fatalf("column %d=%+v, want slot 0's own region", column, cell)
				}
				if cell.Shaded {
					shaded++
					if cell.Colour != region.ShadeColour {
						t.Fatalf("shaded column %d colour=%q, want the darkened %q", column, cell.Colour, region.ShadeColour)
					}
					continue
				}
				bright++
				if cell.Colour != region.Colour {
					t.Fatalf("bright column %d colour=%q, want the slot colour %q", column, cell.Colour, region.Colour)
				}
			}
			if bright != testCase.wantBright || shaded != testCase.wantShaded {
				t.Fatalf("bright=%d shaded=%d, want %d and %d", bright, shaded, testCase.wantBright, testCase.wantShaded)
			}
			// The bright and darkened shades are both derived from ONE slot colour,
			// and the row shares it, so a reservation stays identifiable.
			if region.ShadeColour == region.Colour {
				t.Fatalf("the darkened shade equals the bright one (%q); the split would be invisible", region.Colour)
			}
			// The NEXT region is untouched by this one's split.
			for column := int(testCase.reserved / gib); column < int(testCase.reserved/gib)+4; column++ {
				if cells[column].Slot != 1 {
					t.Fatalf("column %d=%+v, want the next slot's region", column, cells[column])
				}
			}
		})
	}
}

// The scope-less aggregate, the free gap and the out-of-slice grey carry no
// per-scope usage reading, so none of them is ever split.
//
// verifies: AIRA-135
func TestTopBarNonScopeRegionsAreNeverSplit(t *testing.T) {
	frame := &runner.ConfineSliceReserve{
		SystemMemTotalBytes: 64 * gib, SystemMemAvailableBytes: 40 * gib,
		SliceCurrentBytes: 10 * gib, SliceReclaimableBytes: 2 * gib,
		SliceMaxBytes: 32 * gib, SliceHighState: runner.ConfineSliceHighNone,
		ReservationJobs: 3, ReservationBytes: 2 * gib,
	}
	model, _ := topViewModel(nil, topTestListing(frame,
		topTestRecord("CONFINE-alpha-101-aa", "alpha", 4*gib, 1*gib)))
	for _, region := range model.Bar.Regions {
		if region.Kind == topRegionScope {
			continue
		}
		if region.UsedKnown || region.ShadeColour != "" {
			t.Fatalf("region %+v carries a used/unused split it has no reading for", region)
		}
	}
	for _, cell := range topBarCells(model.Bar, 64) {
		if cell.Shaded && cell.Kind != topRegionScope {
			t.Fatalf("cell %+v is shaded but belongs to no scope region", cell)
		}
	}
}

// topShadeColour darkens a slot colour and refuses anything it cannot parse,
// because a shade indistinguishable from the bright one would present a split
// that is not actually being drawn.
//
// verifies: AIRA-135
func TestTopShadeColourDarkensEverySlotColourAndRefusesTheRest(t *testing.T) {
	for _, colour := range topSlotColours {
		shade := topShadeColour(colour)
		if shade == "" || shade == colour {
			t.Fatalf("topShadeColour(%q)=%q, want a distinct darkened colour", colour, shade)
		}
		if len(shade) != 7 || shade[0] != '#' {
			t.Fatalf("topShadeColour(%q)=%q, want the #rrggbb form", colour, shade)
		}
		bright, err := strconv.ParseInt(colour[1:], 16, 64)
		if err != nil {
			t.Fatal(err)
		}
		dark, err := strconv.ParseInt(shade[1:], 16, 64)
		if err != nil {
			t.Fatal(err)
		}
		if dark >= bright {
			t.Fatalf("topShadeColour(%q)=%q is not darker", colour, shade)
		}
	}
	for _, bad := range []string{"", "red", "#12345", "#gggggg", "5fafff"} {
		if got := topShadeColour(bad); got != "" {
			t.Fatalf("topShadeColour(%q)=%q, want no shade at all", bad, got)
		}
	}
}

func int64Pointer(value int64) *int64 { return &value }

// The two shades get a key on the summary line, and ONLY when a split is really
// drawn: a key beside an undivided bar would describe something that is not on
// screen.
//
// verifies: AIRA-135
func TestTopShadeLegendAppearsOnlyWhenASplitIsDrawn(t *testing.T) {
	frame := topTestFrame()
	split, _ := topViewModel(nil, topTestListing(frame,
		topTestRecord("CONFINE-alpha-101-aa", "alpha", 4*gib, 1*gib)))
	if got := topShadeLegend(split.Bar); got == "" {
		t.Fatalf("a drawn split carried no key: %+v", split.Bar.Regions)
	}
	// Usage unevaluated for every drawn scope: one undivided shade, no key.
	unknown := topTestRecord("CONFINE-alpha-101-aa", "alpha", 4*gib, 0)
	unknown.RSSBytes = nil
	undivided, _ := topViewModel(nil, topTestListing(frame, unknown))
	if got := topShadeLegend(undivided.Bar); got != "" {
		t.Fatalf("an undivided bar carried the split key %q", got)
	}
	if got := topShadeLegend(nil); got != "" {
		t.Fatalf("a nil bar carried the split key %q", got)
	}
}
