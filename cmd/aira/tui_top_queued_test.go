package main

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"aira/internal/runner"
)

// AIRA-308. `aira top` lists the jobs still waiting in the admission queue AFTER
// the live rows, greyed, without touching anything a live row or a bar is built
// from.
//
// verifies: AIRA-308

func topQueuedTestRecord(scopeID, name string, reserve int64) runner.ConfineRecord {
	pid := 7001
	age := int64(95)
	vram := int64(0)
	command := "go test ./..."
	cwd := "/work/queued"
	return runner.ConfineRecord{
		Name: name, Owner: "session-q", ScopeID: scopeID, SupervisorPID: &pid, AgeSeconds: &age,
		ReserveBytes: &reserve, VRAMBytes: &vram, Command: &command, Cwd: &cwd,
		UnevaluatedFields: []string{"populated", "rss", "cap", "cpu", "supervisor_live"},
	}
}

func topQueuedListing(queued ...runner.ConfineRecord) runner.ConfineListResult {
	frame := topTestFrame()
	frame.Queued = len(queued)
	frame.FreezePhase = "idle"
	listing := topTestListing(frame,
		topTestRecord("CONFINE-alpha-101-aa", "alpha", 2*gib, gib),
		topTestRecord("CONFINE-bravo-102-bb", "bravo", 3*gib, gib),
	)
	listing.Queued = queued
	return listing
}

// (c) The headline. Queued rows come after every live row, in the order given;
// each is SLOT "-", LIVE "queued", the request marked "req", the holds-nothing
// cells "-", and the one grey. Asserted cell by cell so a row that reused the live
// row builder (slot number, "no"/"yes", drawn zeros) fails a named assertion.
func TestTopViewModelListsQueuedJobsAfterTheLiveOnesGreyed(t *testing.T) {
	first := topQueuedTestRecord("CONFINE-zulu-201-cc@session-q", "zulu", 4*gib)
	second := topQueuedTestRecord("CONFINE-alpha-202-dd@session-q", "queuedb", 1*gib)
	vram := 6 * gib
	second.VRAMBytes = &vram
	model, _ := topViewModel(topTick{}, topQueuedListing(first, second))
	// A non-GPU queued job's VRAM cell is the live rows' own "not a GPU job" glyph.
	zero := int64(0)
	if topVRAMCell(&zero) != "—" {
		t.Fatalf("test premise: live non-GPU glyph is %q", topVRAMCell(&zero))
	}

	if len(model.Rows) != 4 {
		t.Fatalf("rows=%d, want 2 live + 2 queued", len(model.Rows))
	}
	for i, id := range []string{"CONFINE-alpha-101-aa", "CONFINE-bravo-102-bb"} {
		if model.Rows[i].ID != id || model.Rows[i].Cells[3] == "queued" {
			t.Fatalf("live row %d displaced: %+v", i, model.Rows[i])
		}
	}
	got := model.Rows[2]
	want := []string{"-", "zulu", "7001", "queued", "1m35s", "4096M req", "-", "-", topVRAMCell(&zero), "session-q", "go test ./..."}
	if !reflect.DeepEqual(got.Cells, want) {
		t.Fatalf("queued cells=%q\nwant         %q", got.Cells, want)
	}
	if got.Colour != topColourQueued {
		t.Fatalf("colour=%q, want the queued grey %q", got.Colour, topColourQueued)
	}
	if model.Rows[3].Cells[0] != "-" || model.Rows[3].Cells[5] != "1024M req" || model.Rows[3].Cells[8] != "6144M req" {
		t.Fatalf("second queued row cells=%q (reserve and requested VRAM must both read as requests)", model.Rows[3].Cells)
	}
	if model.Rows[3].Colour != topColourQueued {
		t.Fatalf("second row colour=%q", model.Rows[3].Colour)
	}
	// A queued row can never be an action target: the live rows' ID is a scope id.
	for _, row := range model.Rows[2:] {
		if row.ID != "" {
			t.Fatalf("queued row carries ID %q; a not-yet-admitted scope must not be selectable", row.ID)
		}
	}
	// The footer's queue text is unchanged and carries no truncation note.
	if !strings.Contains(model.Footer, "2 queued, freeze idle") || strings.Contains(model.Footer, "showing") {
		t.Fatalf("footer=%q", model.Footer)
	}
}

// Everything built from LIVE scopes — slots, colours, the three bars, and the
// slot table carried to the next tick — is byte-identical with and without queued
// rows present. A queued job holds no memory, so drawing it in a bar (or letting
// it take a slot) is the defect this pins.
func TestTopViewModelQueuedJobsChangeNoSlotColourOrBar(t *testing.T) {
	queued := topQueuedTestRecord("CONFINE-zulu-201-cc@session-q", "zulu", 40*gib)
	vram := 8 * gib
	queued.VRAMBytes = &vram
	with := topQueuedListing(queued)
	without := topQueuedListing()
	without.SliceReserve.Queued = 0

	// Two ticks, so the CPU rate path (needs a previous sample) is exercised too.
	withModel, withTick := topViewModel(topTick{}, with)
	withoutModel, withoutTick := topViewModel(topTick{}, without)
	withModel, withTick = topViewModel(withTick, with)
	withoutModel, withoutTick = topViewModel(withoutTick, without)

	if !reflect.DeepEqual(withTick.Slots, withoutTick.Slots) {
		t.Fatalf("queued job moved the slot table: %v vs %v", withTick.Slots, withoutTick.Slots)
	}
	if !reflect.DeepEqual(withModel.Rows[:2], withoutModel.Rows) {
		t.Fatalf("live rows differ:\n%+v\n%+v", withModel.Rows[:2], withoutModel.Rows)
	}
	if !reflect.DeepEqual(withModel.Bar, withoutModel.Bar) || !reflect.DeepEqual(withModel.CPUBar, withoutModel.CPUBar) ||
		!reflect.DeepEqual(withModel.VRAMBar, withoutModel.VRAMBar) {
		t.Fatalf("a queued job reached a bar:\nRAM  %+v\nvs   %+v\nVRAM %+v\nvs   %+v",
			withModel.Bar, withoutModel.Bar, withModel.VRAMBar, withoutModel.VRAMBar)
	}
	for _, bar := range []*topBar{withModel.Bar, withModel.CPUBar, withModel.VRAMBar} {
		for _, region := range bar.Regions {
			if region.Label == "zulu" {
				t.Fatalf("queued job drawn in the %s bar: %+v", bar.Kind, region)
			}
		}
	}
}

// The last column honours the c toggle for queued rows exactly as for live ones,
// and both it and the command are untrusted text: control characters are escaped
// and tview colour tags neutralised, and an unreadable supervisor says so.
func TestTopViewModelQueuedRowsHonourTheDirectoryToggleAndEscapeHostileText(t *testing.T) {
	record := topQueuedTestRecord("CONFINE-zulu-201-cc@session-q", "zulu", gib)
	hostile := "rm\x1b[2J -rf [red]x\nsecond"
	record.Command = &hostile
	cwd := "/work/[blue]dir\x07"
	record.Cwd = &cwd

	byCommand, _ := topViewModel(topTick{}, topQueuedListing(record))
	cells := byCommand.Rows[2].Cells
	last := cells[len(cells)-1]
	if strings.ContainsAny(last, "\x1b\x07\n") {
		t.Fatalf("raw control characters reached a terminal-bound cell: %q", last)
	}
	if want := topUntrustedCell(&hostile); last != want {
		t.Fatalf("command cell=%q, want the shared untrusted-cell rendering %q", last, want)
	}
	byDir, _ := topViewModel(topTick{ShowCwd: true}, topQueuedListing(record))
	dirCells := byDir.Rows[2].Cells
	if want := topUntrustedCell(&cwd); dirCells[len(dirCells)-1] != want || strings.ContainsAny(dirCells[len(dirCells)-1], "\x07") {
		t.Fatalf("directory cell=%q, want %q", dirCells[len(dirCells)-1], want)
	}
	if byDir.Headers[len(byDir.Headers)-1] != "DIRECTORY" {
		t.Fatalf("headers=%v", byDir.Headers)
	}
	record.Command, record.Cwd = nil, nil
	unread, _ := topViewModel(topTick{}, topQueuedListing(record))
	if got := unread.Rows[2].Cells; got[len(got)-1] != "unevaluated" {
		t.Fatalf("unreadable command cell=%q, want unevaluated, never blank", got[len(got)-1])
	}
}

// The reserve is shown only as established: a daemon that did not publish one
// reads "unevaluated", never "0M req".
func TestTopViewModelQueuedRowNeverFabricatesAReserve(t *testing.T) {
	record := topQueuedTestRecord("CONFINE-zulu-201-cc@session-q", "zulu", gib)
	record.ReserveBytes, record.VRAMBytes = nil, nil
	model, _ := topViewModel(topTick{}, topQueuedListing(record))
	cells := model.Rows[2].Cells
	if cells[5] != "unevaluated" || cells[8] != "unevaluated" {
		t.Fatalf("reserve/vram cells=%q,%q, want unevaluated", cells[5], cells[8])
	}
	zero := int64(0)
	record.ReserveBytes = &zero
	model, _ = topViewModel(topTick{}, topQueuedListing(record))
	if got := model.Rows[2].Cells[5]; got != "0M req" {
		t.Fatalf("an ESTABLISHED zero request must render as one, got %q", got)
	}
}

// Truncation is stated only when there are fewer rows than the queue's own count,
// and then with both numbers and a reason that does not claim to be exhaustive.
// It must not hang off FreezePhase: a reply with no phase still has a count.
func TestTopFooterStatesQueuedRowTruncation(t *testing.T) {
	record := topQueuedTestRecord("CONFINE-zulu-201-cc@session-q", "zulu", gib)
	listing := topQueuedListing(record)
	listing.SliceReserve.Queued = 70

	truncated := topFooter(listing)
	if !strings.Contains(truncated, "70 queued, freeze idle") || !strings.Contains(truncated, "showing 1 of 70 queued") {
		t.Fatalf("footer=%q, want the unchanged queue text plus a showing-1-of-70 note", truncated)
	}
	if !strings.Contains(truncated, "some queued jobs have no row") {
		t.Fatalf("footer=%q, want the non-exhaustive reason", truncated)
	}
	listing.SliceReserve.FreezePhase = ""
	if noPhase := topFooter(listing); !strings.Contains(noPhase, "showing 1 of 70 queued") {
		t.Fatalf("footer=%q, want the truncation note even with no freeze phase", noPhase)
	}
	listing.SliceReserve.Queued = 1
	if complete := topFooter(listing); strings.Contains(complete, "showing") {
		t.Fatalf("a complete list carries a truncation note: %q", complete)
	}
}

// (d) Wire. The field round-trips; omitted when empty; and a reply from a daemon
// that predates it decodes cleanly into a view with no queued rows and no error.
func TestConfineListQueuedFieldWireRoundTripAndOldDaemon(t *testing.T) {
	record := topQueuedTestRecord("CONFINE-zulu-201-cc@session-q", "zulu", 4*gib)
	sent := topQueuedListing(record)
	data, err := json.Marshal(sent)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), `"queued":[`) {
		t.Fatalf("queued rows absent from the wire: %s", data)
	}
	var received runner.ConfineListResult
	if err := json.Unmarshal(data, &received); err != nil {
		t.Fatal(err)
	}
	if len(received.Queued) != 1 || received.Queued[0].ScopeID != record.ScopeID ||
		received.Queued[0].ReserveBytes == nil || *received.Queued[0].ReserveBytes != 4*gib {
		t.Fatalf("round trip lost the row: %+v", received.Queued)
	}

	empty, _ := json.Marshal(topQueuedListing())
	if strings.Contains(string(empty), `"queued":[`) || strings.Contains(string(empty), `"queued":null`) {
		t.Fatalf("an empty queue must omit the field (the wire an old client already knows): %s", empty)
	}

	// An old daemon's reply: no "queued" key at all, but a non-zero count.
	var old runner.ConfineListResult
	oldWire := `{"verdict":"pass","scopes":[],"slice_reserve":{"queued":2,"freeze_phase":"idle","granted_established":true}}`
	if err := json.Unmarshal([]byte(oldWire), &old); err != nil {
		t.Fatal(err)
	}
	model, _ := topViewModel(topTick{}, old)
	if len(model.Rows) != 0 {
		t.Fatalf("rows from a reply with no queued field: %+v", model.Rows)
	}
	if !strings.Contains(model.Footer, "2 queued, freeze idle") {
		t.Fatalf("footer=%q", model.Footer)
	}
}

// The grey must not be mistaken for anything else in the palette: not a slot
// colour (a queued row would read as a running job) and not the bar's
// rest-of-system grey (it would read as mapping to a span that does not exist).
func TestTopQueuedColourIsDistinctFromEveryOtherTopColour(t *testing.T) {
	others := append([]string{topColourScopeless, topColourOutside, topColourMarker}, topSlotColours...)
	for _, colour := range others {
		if strings.EqualFold(colour, topColourQueued) {
			t.Fatalf("queued colour %s collides with %s", topColourQueued, colour)
		}
	}
}
