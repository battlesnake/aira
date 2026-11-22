package main

import (
	"strings"
	"testing"
	"unicode/utf8"

	"aira/internal/runner"
)

// AIRA-374. A supervisor's argv can be 4 KB (a test runner handed every path),
// and tview re-measures the full grapheme width of every visible cell on every
// draw, which it does after EVERY keypress: nine such commands made one arrow key
// cost ~17 ms of CPU. The cell is bounded to a width no terminal row reaches, the
// cut is marked, and everything before it is kept, in both the live and the
// queued rows and in both the COMMAND and DIRECTORY forms of the last column.
//
// verifies: AIRA-374
func TestTopLastColumnIsBoundedForADrawPerKeypress(t *testing.T) {
	head := "make test-engine TEST_PATHS="
	long := head + strings.Repeat("/home/user/tmp/wt/libs/engine/test_x.py ", 100)
	if utf8.RuneCountInString(long) < 4000 {
		t.Fatalf("fixture is %d runes, want a ~4 KB argv", utf8.RuneCountInString(long))
	}
	got := topCommandCell(&long)
	// A literal bound first, so this fails against an unbounded cell however the
	// constant below is later tuned.
	if n := utf8.RuneCountInString(got); n > 1024 {
		t.Fatalf("command cell is %d runes, want it bounded well under the 4 KB argv", n)
	}
	if n := utf8.RuneCountInString(got); n != topCellMaxRunes+1 {
		t.Fatalf("command cell is %d runes, want the %d-rune bound plus the cut mark", n, topCellMaxRunes)
	}
	if !strings.HasPrefix(got, string([]rune(long)[:topCellMaxRunes])) || !strings.HasSuffix(got, "…") {
		t.Fatalf("command cell %q..., want the argv's head kept and the cut marked", got[:40])
	}
	// At the bound exactly, nothing is cut and nothing is marked.
	exact := string([]rune(long)[:topCellMaxRunes])
	if got := topCommandCell(&exact); got != exact {
		t.Fatalf("a command of exactly %d runes was changed", topCellMaxRunes)
	}
	// The bound applies BEFORE escaping, so a cut can never split an escape: a
	// long argv of control characters still renders as visible escapes only.
	hostile := strings.Repeat("\x1b", 2*topCellMaxRunes)
	if got := topCommandCell(&hostile); strings.ContainsRune(got, '\x1b') || !strings.HasSuffix(got, "…") {
		t.Fatalf("a long hostile argv lost its escaping or its cut mark")
	}

	live := topTestRecord("CONFINE-live-1-a@s", "live", 1<<30, 1<<29)
	live.Command, live.Cwd = &long, &long
	queued := topTestRecord("CONFINE-queued-2-b@s", "queued", 1<<30, 0)
	queued.Command, queued.Cwd = &long, &long
	listing := topTestListing(topTestFrame(), live)
	listing.Queued = []runner.ConfineRecord{queued}
	for _, showCwd := range []bool{false, true} {
		model, _ := topViewModel(topTick{ShowCwd: showCwd}, listing)
		if len(model.Rows) != 2 {
			t.Fatalf("rows=%d, want one live and one queued", len(model.Rows))
		}
		for _, row := range model.Rows {
			last := row.Cells[len(row.Cells)-1]
			if n := utf8.RuneCountInString(last); n > topCellMaxRunes+1 {
				t.Fatalf("showCwd=%v row %s last cell is %d runes, want at most %d", showCwd, row.ID, n, topCellMaxRunes+1)
			}
		}
	}
}
