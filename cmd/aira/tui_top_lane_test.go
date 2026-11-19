package main

import (
	"strings"
	"testing"

	"github.com/rivo/tview"

	"aira/internal/runner"
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

func topRenderBarText(t *testing.T, bar *topBar, height int) string {
	t.Helper()
	target := tview.NewTextView().SetDynamicColors(true).SetWrap(false)
	target.SetBorder(true)
	target.SetRect(0, 0, 80, height)
	r := &tuiRuntime{}
	r.renderTopBar(target, bar, panelState{Status: panelReady})
	return target.GetText(true)
}

// The lane line is the RAM bar's, present exactly while the lane is on, and it
// reads used / capacity, the job count and the job maximum. A lane-off reply
// (every lane field absent) renders no lane text at all: never a fake
// "0M / 0M". An on lane with no small leases is a REAL zero and says so.
//
// Mutations: rendering the line whenever the frame exists (lane-off case red);
// dropping the line or any figure (lane-on cases red).
func TestTopLaneLineOnlyWhileTheLaneIsOn(t *testing.T) {
	cases := []struct {
		name  string
		frame *runner.ConfineSliceReserve
		want  string
	}{
		{"on", topLaneFrame(4608<<20, 10), "small-job lane: 4608M / 8192M used, 10 jobs (max 1024M each)"},
		{"on, one job", topLaneFrame(512<<20, 1), "small-job lane: 512M / 8192M used, 1 job (max 1024M each)"},
		{"on, empty", topLaneFrame(0, 0), "small-job lane: 0M / 8192M used, 0 jobs (max 1024M each)"},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			model, _ := topViewModel(topTick{}, topTestListing(testCase.frame))
			if model.Bar.Lane != testCase.want {
				t.Fatalf("lane line=%q, want %q", model.Bar.Lane, testCase.want)
			}
			if text := topRenderBarText(t, model.Bar, topRAMBarHeight); !strings.Contains(text, testCase.want) {
				t.Fatalf("rendered RAM bar lacks the lane line %q:\n%s", testCase.want, text)
			}
		})
	}
	off := topTestFrame()
	off.GrantedEstablished = true
	model, _ := topViewModel(topTick{}, topTestListing(off))
	if model.Bar.Lane != "" {
		t.Fatalf("lane off: lane line=%q, want none", model.Bar.Lane)
	}
	if text := topRenderBarText(t, model.Bar, topRAMBarHeight); strings.Contains(strings.ToLower(text), "lane") {
		t.Fatalf("lane off: rendered RAM bar mentions the lane:\n%s", text)
	}
}

// LaneUsedBytes can exceed the capacity (main grants of small jobs, a resize, a
// restart re-booking pins); the lane is then shut. The line states both real
// figures and says so, never clamping used to the capacity.
//
// Mutations: clamping used to capacity, or dropping the OVER clause, are red;
// emitting OVER at used == capacity is red (the at-capacity case).
func TestTopLaneLineReadsOverCapacityAsShut(t *testing.T) {
	over := topLaneFrame(9*gib, 12)
	if got, want := topLaneLine(over), "small-job lane: 9216M / 8192M used, 12 jobs (max 1024M each) OVER: shut"; got != want {
		t.Fatalf("over-capacity lane line=%q, want %q", got, want)
	}
	at := topLaneFrame(8*gib, 8)
	if got := topLaneLine(at); strings.Contains(got, "OVER") || !strings.Contains(got, "8192M / 8192M") {
		t.Fatalf("at-capacity lane line=%q, want the figures and no OVER", got)
	}
}

// The lane's used figure and job count are meaningful only with an established
// ledger (they come from the same locked pass as GrantedBytes). Unestablished,
// the line keeps the configured capacity and says the use is unevaluated, never
// "0M / 8192M used, 0 jobs".
func TestTopLaneLineUnestablishedIsUnevaluatedNotZero(t *testing.T) {
	frame := topLaneFrame(0, 0)
	frame.GrantedEstablished = false
	got := topLaneLine(frame)
	if want := "small-job lane: 8192M (max 1024M each), use unevaluated"; got != want {
		t.Fatalf("unestablished lane line=%q, want %q", got, want)
	}
	if topLaneLine(nil) != "" {
		t.Fatal("nil slice reserve renders a lane line")
	}
}

// The lane line is a RAM-panel line that must not cost an honesty line: it is
// drawn LAST, so with the panel's worst case (OVER-SUBSCRIBED and a note) the
// two honesty lines stay inside the panel and only the lane line can clip, and
// in the common case (a note but no over-subscription) everything fits.
func TestTopLaneLineNeverPushesAnHonestyLineOffThePanel(t *testing.T) {
	inner := topRAMBarHeight - 2
	bar := &topBar{
		Kind: topBarRAM, Evaluated: true, Total: 10 * gib, Claimed: 9 * gib,
		Outside: 2 * gib, OutsideKnown: true, Overcommitted: true,
		Notes: []string{"one scope not drawn"},
		Lane:  "small-job lane: 0M / 8192M used, 0 jobs (max 1024M each)",
	}
	lines := strings.Split(topRenderBarText(t, bar, topRAMBarHeight), "\n")
	if len(lines) > inner {
		lines = lines[:inner]
	}
	visible := strings.Join(lines, "\n")
	if !strings.Contains(visible, "OVER-SUBSCRIBED") || !strings.Contains(visible, "one scope not drawn") {
		t.Fatalf("an honesty line fell off the %d-line panel:\n%s", inner, visible)
	}
	bar.Overcommitted = false
	text := topRenderBarText(t, bar, topRAMBarHeight)
	if lines := strings.Split(text, "\n"); len(lines) > inner || !strings.Contains(text, bar.Lane) {
		t.Fatalf("common case does not fit with the lane line (%d lines, want <= %d):\n%s", len(lines), inner, text)
	}
	// An unevaluated RAM frame still states the lane, which does not depend on it.
	unevaluated := &topBar{Kind: topBarRAM, Reason: "total system RAM is unevaluated", Lane: bar.Lane}
	if text := topRenderBarText(t, unevaluated, topRAMBarHeight); !strings.Contains(text, "UNEVALUATED") || !strings.Contains(text, bar.Lane) {
		t.Fatalf("unevaluated bar text=%q, want the reason and the lane line", text)
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
// keep precedence (their contract, AIRA-309/340); an unknown token from a newer
// daemon reads as the plain "queued", never a guess.
//
// Mutations: dropping any case of the switch, reordering held-by-head below
// lane_wait, or letting lane words override pushed/high, are red.
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
		{"pushed wins", "full", true, true, "", "pushed"},
		{"high wins", "full", true, false, runner.ConfinePriorityHigh, "high"},
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
// mode applies nothing, so its marker (if any) must not claim a cause.
func TestTopCeilingMarkerNamesItsCause(t *testing.T) {
	cases := []struct {
		name, mode, state, basis, want string
	}{
		{"pressure", "enforce", "throttled", "system-pressure", "admission ceiling (system pressure)"},
		{"machine reserve", "enforce", "throttled", "machine-reserve", "admission ceiling (machine reserve)"},
		{"unknown basis", "enforce", "throttled", "new-term", "admission ceiling"},
		{"observe", "observe", "throttled", "system-pressure", "admission ceiling"},
		{"unthrottled", "enforce", "unthrottled", "", "admission ceiling"},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			frame := topTestFrame()
			frame.CeilingEffectiveBytes = 20 * gib
			frame.CeilingMode, frame.CeilingState, frame.CeilingBasis = testCase.mode, testCase.state, testCase.basis
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
