package main

import (
	"strings"
	"testing"

	"aira/internal/runner"
)

// AIRA-315 (RANT-47 item 2a). The human `confine --list` table gains a PEAK
// column: the scope's lifetime memory.peak, which --json already carried as
// peak_rss. An unread peak renders "unevaluated" (never 0, never RSS standing
// in), and a read peak renders as read (never clamped to RESERVE).

// confineListCell returns the cell under header column `column` in the row
// whose NAME is `name`, locating the column by the header's own position so a
// value printed in any other column cannot satisfy the assertion.
func confineListCell(t *testing.T, output, name, column string) string {
	t.Helper()
	lines := strings.Split(output, "\n")
	index := -1
	for _, line := range lines {
		fields := strings.Fields(line)
		if len(fields) > 0 && fields[0] == "NAME" {
			for i, field := range fields {
				if field == column {
					index = i
				}
			}
			break
		}
	}
	if index < 0 {
		t.Fatalf("no %s column in the header:\n%s", column, output)
	}
	for _, line := range lines {
		fields := strings.Fields(line)
		if len(fields) > index && fields[0] == name {
			return fields[index]
		}
	}
	t.Fatalf("no row named %q in:\n%s", name, output)
	return ""
}

// verifies: AIRA-315
func TestConfineListPeakColumnSaysUnevaluatedNeverZero(t *testing.T) {
	t.Parallel()
	live := true
	unread := confineLiveRecord(t, "unread", 4242, &live, &live)
	unread.PeakRSS = nil
	reserve := int64(1 << 20)
	unread.ReserveBytes = &reserve
	read := confineLiveRecord(t, "read", 4243, &live, &live)
	peak := int64(3 << 20)
	read.PeakRSS = &peak
	read.ReserveBytes = &reserve
	output := renderConfineList(t, []runner.ConfineRecord{unread, read})
	if got := confineListCell(t, output, "unread", "PEAK"); got != "unevaluated" {
		t.Fatalf("an unread peak rendered %q, want unevaluated:\n%s", got, output)
	}
	if got := confineListCell(t, output, "read", "PEAK"); got != "3145728" {
		t.Fatalf("a read peak above RESERVE rendered %q, want 3145728 unclamped:\n%s", got, output)
	}
}
