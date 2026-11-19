package main

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/gdamore/tcell/v2"
	"github.com/rivo/tview"

	"aira/internal/core"
	"aira/internal/daemon"
	"aira/internal/runner"
	"aira/internal/testdeadline"
)

// AIRA-361. `aira top` shows the small-job lane (AIRA-350) and the cause of a
// throttled admission ceiling, display-only, from fields `confine --list`
// already carries.
//
// verifies: AIRA-361

// topLaneFrame is topTestFrame with an established ledger and the lane ON at
// the live configuration (8 GiB capacity, 1 GiB job maximum).
func topLaneFrame(used int64, jobs int) *runner.ConfineSliceReserve {
	frame := topTestFrame()
	frame.GrantedEstablished = true
	frame.LaneCapacityBytes = 8 * gib
	frame.LaneJobMaxBytes = gib
	frame.LaneUsedBytes = used
	frame.LaneJobs = jobs
	return frame
}

func topRenderBarTitle(t *testing.T, bar *topBar) (string, string) {
	t.Helper()
	target := tview.NewTextView().SetDynamicColors(true).SetWrap(false)
	target.SetBorder(true).SetTitle(topRAMPanelTitle(""))
	target.SetRect(0, 0, 80, topRAMBarHeight)
	r := &tuiRuntime{topBar: target}
	r.renderTopBar(target, bar, panelState{Status: panelReady})
	return target.GetTitle(), target.GetText(true)
}

// A refresh that FAILS after a good one clears the panel's model (the body
// reads ERROR); the RAM panel's title must lose the lane gauge with it, never
// keep stating the last reply's usage, job count and shut state.
//
// Mutation: rewriting the title only while a bar exists (the pre-fix guard) is
// red: the title keeps "OVER, shut: 9216M/8192M, 12 jobs".
func TestTopLaneTitleClearsWhenARefreshFails(t *testing.T) {
	target := tview.NewTextView().SetDynamicColors(true).SetWrap(false)
	target.SetBorder(true).SetTitle(topRAMPanelTitle(""))
	target.SetRect(0, 0, 80, topRAMBarHeight)
	r := &tuiRuntime{topBar: target}

	// Both refreshes go through the REAL reducer, so the test covers what the
	// reducer actually leaves in the panel after a failure, not an assumed nil.
	state := newTUIStateForViews(8, topOnlyViews, nil)
	deliver := func(state tuiState, result fetchResult) tuiState {
		state, _ = requestPanelRefresh(state, viewTop)
		result.View, result.Generation = viewTop, state.Panels[viewTop].InFlightGeneration
		state, _ = onTUIFetchResult(state, result)
		return state
	}
	listing := topTestListing(topLaneFrame(9*gib, 12))
	state = deliver(state, fetchResult{Top: &listing})
	good := state.Panels[viewTop]
	r.renderTopBar(target, good.Model.Bar, good)
	if title := target.GetTitle(); !strings.Contains(title, "OVER, shut: 9216M/8192M, 12 jobs") {
		t.Fatalf("good refresh: title=%q, want the lane gauge", title)
	}

	state = deliver(state, fetchResult{Code: "E_DAEMON_UNAVAILABLE"})
	failed := state.Panels[viewTop]
	r.renderTopBar(target, failed.Model.Bar, failed)
	if title, text := target.GetTitle(), target.GetText(true); title != " System RAM " || !strings.Contains(text, "ERROR E_DAEMON_UNAVAILABLE") {
		t.Fatalf("failed refresh: title=%q text=%q, want the bare title over the ERROR body", title, text)
	}
}

// The lane gauge is the RAM panel's, present exactly while the lane is on, and
// it reads used / capacity, the job count and the job maximum. A lane-off reply
// (every lane field absent) renders no lane text at all: never a fake "0M/0M".
// An on lane with no small leases is a REAL zero and says so.
//
// Mutations: rendering the gauge whenever the frame exists (lane-off case red);
// dropping the gauge or any figure (lane-on cases red).
func TestTopLaneLineOnlyWhileTheLaneIsOn(t *testing.T) {
	cases := []struct {
		name  string
		frame *runner.ConfineSliceReserve
		want  string
	}{
		{"on", topLaneFrame(4608<<20, 10), "small-job lane 4608M/8192M, 10 jobs, max 1024M each"},
		{"on, one job", topLaneFrame(512<<20, 1), "small-job lane 512M/8192M, 1 job, max 1024M each"},
		{"on, empty", topLaneFrame(0, 0), "small-job lane 0M/8192M, 0 jobs, max 1024M each"},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			model, _ := topViewModel(topTick{}, topTestListing(testCase.frame))
			if model.Bar.Lane != testCase.want {
				t.Fatalf("lane line=%q, want %q", model.Bar.Lane, testCase.want)
			}
			if title, _ := topRenderBarTitle(t, model.Bar); title != " System RAM · "+testCase.want+" " {
				t.Fatalf("RAM panel title=%q, want the lane gauge %q", title, testCase.want)
			}
		})
	}
	off := topTestFrame()
	off.GrantedEstablished = true
	model, _ := topViewModel(topTick{}, topTestListing(off))
	if model.Bar.Lane != "" {
		t.Fatalf("lane off: lane line=%q, want none", model.Bar.Lane)
	}
	title, text := topRenderBarTitle(t, model.Bar)
	if title != " System RAM " || strings.Contains(strings.ToLower(text), "lane") {
		t.Fatalf("lane off: title=%q text=%q, want no lane anywhere", title, text)
	}
}

// LaneUsedBytes can exceed the capacity (main grants of small jobs, a resize, a
// restart re-booking pins); the lane is then shut. The gauge states both real
// figures and says so, FIRST, never clamping used to the capacity.
//
// Mutations: clamping used to capacity, or dropping the OVER clause, are red;
// emitting OVER at used == capacity is red (the at-capacity case).
func TestTopLaneLineReadsOverCapacityAsShut(t *testing.T) {
	over := topLaneFrame(9*gib, 12)
	if got, want := topLaneLine(over), "small-job lane OVER, shut: 9216M/8192M, 12 jobs, max 1024M each"; got != want {
		t.Fatalf("over-capacity lane line=%q, want %q", got, want)
	}
	at := topLaneFrame(8*gib, 8)
	if got := topLaneLine(at); strings.Contains(got, "OVER") || !strings.Contains(got, "8192M/8192M") {
		t.Fatalf("at-capacity lane line=%q, want the figures and no OVER", got)
	}
}

// The lane's used figure and job count are meaningful only with an established
// ledger (they come from the same locked pass as GrantedBytes). Unestablished,
// the gauge keeps the configured capacity and says the use is unevaluated, never
// "0M/8192M, 0 jobs".
func TestTopLaneLineUnestablishedIsUnevaluatedNotZero(t *testing.T) {
	frame := topLaneFrame(0, 0)
	frame.GrantedEstablished = false
	got := topLaneLine(frame)
	if want := "small-job lane use unevaluated, 8192M, max 1024M each"; got != want {
		t.Fatalf("unestablished lane line=%q, want %q", got, want)
	}
	if topLaneLine(nil) != "" {
		t.Fatal("nil slice reserve renders a lane line")
	}
	// An unevaluated RAM frame still states the lane, which does not depend on it.
	unevaluated := &topBar{Kind: topBarRAM, Reason: "total system RAM is unevaluated", Lane: got}
	if title, text := topRenderBarTitle(t, unevaluated); !strings.Contains(text, "UNEVALUATED") || !strings.Contains(title, got) {
		t.Fatalf("unevaluated bar title=%q text=%q, want the reason and the lane gauge", title, text)
	}
}

// topPressureListing is the RAM panel's worst realistic case on one screen: the
// slice OVER-SUBSCRIBED, a note (a scope with no reserve), the lane OVER and
// shut, and an enforced ceiling throttled by system pressure and HELD at its last
// known value; plus a high-priority queued job the lane refused.
func topPressureListing(laneUsed int64) runner.ConfineListResult {
	frame := topLaneFrame(laneUsed, 12)
	frame.SliceHighBytes, frame.SliceHighState = 28*gib, runner.ConfineSliceHighSet
	frame.CeilingEffectiveBytes = 20 * gib
	frame.CeilingMode, frame.CeilingState, frame.CeilingBasis, frame.CeilingHeld = "enforce", "throttled", "system-pressure", true
	frame.SystemMemAvailableBytes = 2 * gib
	frame.GrantedBytes = 30 * gib
	frame.Queued = 1
	big := topTestRecord("CONFINE-big-103-cc", "big", 30*gib, gib)
	small := topSmallRecord(topTestRecord("CONFINE-small-101-aa", "small", 512<<20, 100<<20))
	undrawn := topTestRecord("CONFINE-undrawn-104-ee", "undrawn", gib, gib)
	undrawn.ReserveBytes = nil
	queued := topQueuedTestRecord("CONFINE-q-201-dd@session-q", "q", gib)
	queued.LaneWait, queued.Priority = "full", runner.ConfinePriorityHigh
	listing := topTestListing(frame, big, small, undrawn)
	listing.Queued = []runner.ConfineRecord{queued}
	return listing
}

func topScreenRow(screen, needle string) string {
	for _, row := range strings.Split(screen, "\n") {
		if strings.Contains(row, needle) {
			return row
		}
	}
	return ""
}

// On a real 80x24 terminal, read from the DRAWN cells: under the worst case
// every RAM-panel fact survives at once. The lane gauge (title) keeps "OVER,
// shut" and both figures, the ceiling marker keeps its figure and its cause and
// hold, and neither displaces OVER-SUBSCRIBED or the note. Asserting the widget
// text instead would pass with any of these clipped off the panel.
//
// Mutations: drawing the lane as an interior line again (it, or the note, falls
// off the 5-row panel: red); restoring the long marker labels (the ceiling
// figure clips at 80 columns: red); dropping the hold qualifier (red).
func TestTopRAMPanelKeepsEveryFactAt80x24(t *testing.T) {
	screen := topScreenCapture(t, 80, 24, topPressureListing(9*gib))
	for _, want := range []string{
		"System RAM · small-job lane OVER, shut: 9216M/8192M, 12 jobs, max 1024M each",
		"╎ ceiling (pressure, last known) 20480M",
		"OVER-SUBSCRIBED",
		"1 scope with an unevaluated reservation not drawn",
	} {
		if topScreenRow(screen, want) == "" {
			t.Fatalf("80x24 screen lacks %q:\n%s", want, screen)
		}
	}
	// Six-digit lane figures widen the title past 80 columns: the clip takes the
	// job maximum at the END, never the shut state or the used figure.
	wide := topPressureListing(123456 << 20)
	wide.SliceReserve.LaneCapacityBytes = 102400 << 20
	if row := topScreenRow(topScreenCapture(t, 80, 24, wide), "System RAM"); !strings.Contains(row, "OVER, shut: 123456M/102400M, 12 jobs") {
		t.Fatalf("wide lane title row=%q, want the state and both figures kept", row)
	}
	// Lane off: no lane text anywhere on the screen.
	off := topPressureListing(0)
	off.SliceReserve.LaneCapacityBytes, off.SliceReserve.LaneJobMaxBytes, off.SliceReserve.LaneJobs = 0, 0, 0
	off.Scopes[1].Small, off.Queued[0].LaneWait = false, ""
	if screen := topScreenCapture(t, 80, 40, off); strings.Contains(strings.ToLower(screen), "lane") {
		t.Fatalf("lane-off screen mentions the lane:\n%s", screen)
	}
}

// The queued row's combined LIVE cell and a small lease's marker reach the
// drawn table at 80 columns (at 24 rows the three bar panels leave the table no
// row, a pre-existing layout limit this ticket does not change).
func TestTopTableShowsLaneFactsAt80Columns(t *testing.T) {
	screen := topScreenCapture(t, 80, 40, topPressureListing(9*gib))
	if row := topScreenRow(screen, "7001"); !strings.Contains(row, "high, lane full") {
		t.Fatalf("queued row=%q, want LIVE 'high, lane full':\n%s", row, screen)
	}
	if row := topScreenRow(screen, " small "); !strings.Contains(row, "512M lane") {
		t.Fatalf("small row=%q, want RESERVATION '512M lane':\n%s", row, screen)
	}
}

// A live small lease carries "lane" in its RESERVATION cell, after the pin when
// both apply; a non-small lease carries nothing new.
//
// Mutations: marking every row, or none, is red.
func TestTopReservationCellMarksASmallLease(t *testing.T) {
	small := topTestRecord("CONFINE-small-101-aa", "small", 512<<20, 100<<20)
	small.Small = true
	pinned := topDelegateRecord("CONFINE-pinned-102-bb", "pinned", gib, 900<<20, 100<<20)
	pin := gib
	pinned.PinBytes = &pin
	pinned.Small = true
	big := topTestRecord("CONFINE-big-103-cc", "big", 4*gib, gib)
	model, _ := topViewModel(topTick{}, topTestListing(topLaneFrame(1412<<20, 2), small, pinned, big))
	cells := topReservationCells(model)
	for id, want := range map[string]string{
		"CONFINE-small-101-aa":  "512M lane",
		"CONFINE-pinned-102-bb": "900M (pin 1024M) lane",
		"CONFINE-big-103-cc":    "4096M",
	} {
		if got := cells[id]; got != want {
			t.Fatalf("%s RESERVATION cell=%q, want %q", id, got, want)
		}
	}
}

// A queued row's LIVE cell names why the lane did not take it, in plain words,
// only when the daemon said so. An operator push and a --priority high launch
// stay first (their contract, AIRA-309/340) and the lane reason follows them
// rather than being hidden; an unknown token from a newer daemon names nothing.
//
// Mutations: dropping any case of the switch, reordering held-by-head below
// lane_wait, returning pushed/high before the lane reason, or letting lane words
// replace pushed/high, are red.
func TestTopQueuedRowNamesTheLaneReason(t *testing.T) {
	cases := []struct {
		name     string
		laneWait string
		held     bool
		pushed   bool
		priority string
		want     string
	}{
		{"no lane diagnosis", "", false, false, "", "queued"},
		{"full", "full", false, false, "", "lane full"},
		{"kept", "kept", false, false, "", "kept back"},
		{"behind a push", "pushed", false, false, "", "behind push"},
		{"no room", "no-room", false, false, "", "no room"},
		{"held by head", "", true, false, "", "behind head"},
		{"held by head wins", "no-room", true, false, "", "behind head"},
		{"unknown token", "someday", false, false, "", "queued"},
		{"pushed first, then the lane", "full", false, true, "", "pushed, lane full"},
		{"high first, then the lane", "full", false, false, runner.ConfinePriorityHigh, "high, lane full"},
		{"high, held by head", "no-room", true, false, runner.ConfinePriorityHigh, "high, behind head"},
		{"pushed, no lane diagnosis", "", false, true, "", "pushed"},
		{"high, no lane diagnosis", "", false, false, runner.ConfinePriorityHigh, "high"},
		{"high, unknown token", "someday", false, false, runner.ConfinePriorityHigh, "high"},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			record := topQueuedTestRecord("CONFINE-q-201-dd@session-q", "q", gib)
			record.LaneWait, record.HeldByHead, record.Pushed, record.Priority = testCase.laneWait, testCase.held, testCase.pushed, testCase.priority
			if got := topQueuedRow(record, false, 1).Cells[3]; got != testCase.want {
				t.Fatalf("LIVE cell=%q, want %q", got, testCase.want)
			}
		})
	}
}

// The admission-ceiling marker names what cut the ceiling, only when an
// ENFORCED ceiling is throttled and the basis is one this binary knows. Observe
// mode applies nothing, so its marker (if any) must not claim a cause. A HELD
// ceiling (the newest reading failed) says "last known" whatever its cause.
//
// Mutations: ignoring CeilingHeld (held cases red); gating the hold on a known
// cause (held, unknown basis red); naming a cause in observe mode (red).
func TestTopCeilingMarkerNamesItsCause(t *testing.T) {
	cases := []struct {
		name, mode, state, basis string
		held                     bool
		want                     string
	}{
		{"pressure", "enforce", "throttled", "system-pressure", false, "ceiling (pressure)"},
		{"machine reserve", "enforce", "throttled", "machine-reserve", false, "ceiling (machine reserve)"},
		{"unknown basis", "enforce", "throttled", "new-term", false, "ceiling"},
		{"observe", "observe", "throttled", "system-pressure", false, "ceiling"},
		{"unthrottled", "enforce", "unthrottled", "", false, "ceiling"},
		{"pressure, held", "enforce", "throttled", "system-pressure", true, "ceiling (pressure, last known)"},
		{"unknown basis, held", "enforce", "throttled", "new-term", true, "ceiling (last known)"},
		{"observe, held", "observe", "throttled", "system-pressure", true, "ceiling (last known)"},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			frame := topTestFrame()
			frame.CeilingEffectiveBytes = 20 * gib
			frame.CeilingMode, frame.CeilingState, frame.CeilingBasis, frame.CeilingHeld = testCase.mode, testCase.state, testCase.basis, testCase.held
			var label string
			for _, marker := range topMarkersFor(frame) {
				if marker.Name == topMarkerCeiling {
					label = marker.Label
				}
			}
			if label != testCase.want {
				t.Fatalf("ceiling marker label=%q, want %q", label, testCase.want)
			}
		})
	}
}

// A HELD ceiling says "last known" in the footer even when the effective ceiling
// equals the hard limit (no ceiling marker is drawn then); a live one does not.
//
// Mutation: dropping the footer qualifier (held cases red).
func TestTopFooterQualifiesAHeldCeiling(t *testing.T) {
	for _, testCase := range []struct {
		name      string
		effective int64
		held      bool
	}{
		{"held below hard", 20 * gib, true},
		{"held at hard limit", 32 * gib, true},
		{"live at hard limit", 32 * gib, false},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			reserve := topTestFrame()
			reserve.SliceMaxBytes = 32 * gib
			reserve.CeilingEffectiveBytes = testCase.effective
			reserve.CeilingMode, reserve.CeilingState, reserve.CeilingHeld = "enforce", "unthrottled", testCase.held
			footer := topFooter(runner.ConfineListResult{SliceReserve: reserve})
			if got := strings.Contains(footer, "(ceiling last known)"); got != testCase.held {
				t.Fatalf("footer=%q: has qualifier=%v, want %v", footer, got, testCase.held)
			}
		})
	}
}

// topScreenDispatcher answers `aira top`'s confine-list with one fixed listing.
type topScreenDispatcher struct{ listing runner.ConfineListResult }

func (d topScreenDispatcher) Dispatch(_ context.Context, _ daemon.WorktreeScope, request core.Request) core.Response {
	if request.Verb != "confine-list" {
		return core.Response{Code: "E_UNKNOWN_VERB", Error: "unexpected " + request.Verb}
	}
	raw, err := json.Marshal(d.listing)
	if err != nil {
		return core.Response{Code: "E_INTERNAL", Error: err.Error()}
	}
	return core.Response{OK: true, Code: "OK", RawData: raw}
}

// topScreenCapture runs the REAL `aira top` runtime on a width x height
// simulation screen over listing and returns the DRAWN cells, row by row, once
// the RAM panel shows its totals. It reads the screen, not a widget's text, so a
// line the panel clips or a row it has no room for is absent here as it is on a
// real terminal.
func topScreenCapture(t *testing.T, width, height int, listing runner.ConfineListResult) string {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	screen := tcell.NewSimulationScreen("UTF-8")
	runtime := newTopRuntime(ctx, topScreenDispatcher{listing: listing}, nil, nil, nil, screen)
	done := make(chan error, 1)
	go func() { done <- runtime.run() }()
	var text string
	eventuallyOnUI(t, runtime, 2*time.Second, func() bool {
		text = simulationText(screen)
		return strings.Contains(text, "total ")
	}, "the RAM panel never drew its totals")
	// The simulation screen's Init resets it to 80x25, so size it now.
	resizeBoardScreen(t, runtime, screen, width, height)
	text = onUI(t, runtime, func() string { return simulationText(screen) })
	screen.InjectKey(tcell.KeyRune, 'q', tcell.ModNone)
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("run=%v", err)
		}
	case <-testdeadline.After(5 * time.Second):
		t.Fatal("quit timed out")
	}
	return text
}
