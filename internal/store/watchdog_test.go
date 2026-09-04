package store

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestAppendWatchdogEventAllocatesVisibleSequence(t *testing.T) {
	base := t.TempDir()
	root := filepath.Join(base, "root")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	s := testStore(t, root, filepath.Join(base, "common"), filepath.Join(base, "state"))
	if err := s.AppendWatchdogEvent(context.Background(), "watchdog.intent", `{"pid":42,"facet":"signal_sent"}`); err != nil {
		t.Fatal(err)
	}
	events, next, err := s.EventsSince(context.Background(), 0, 10)
	if err != nil {
		t.Fatal(err)
	}
	if next != 1 || len(events) != 1 || events[0].Actor != "aira-watchdog" || events[0].Verb != "watchdog.intent" || events[0].Target != `{"pid":42,"facet":"signal_sent"}` {
		t.Fatalf("events=%+v next=%d", events, next)
	}
}

// TestWatchdogEventsAreWatchVisibleAndNeverJournaled pins AIRA-75's resolution as
// an INVARIANT rather than leaving it as a comment. The ticket read the
// journaled=0 rows as a defect voiding gap-detection; the decision is that they
// are unjournaled by design, and both halves of that decision have to hold at
// once or the resolution is wrong in one direction or the other:
//
//   - watch-visible, because `aira watch` reads events by seq and these rows are
//     the only surface on which a session sees a host watchdog kill. A change
//     that "stopped minting a sequence number" by demoting them to a daemon log
//     line would silently remove that visibility.
//   - never journaled, because a host-global decision broadcast verbatim into
//     every ready project has no per-project provenance to record; a journal
//     entry would fabricate one.
//
// verifies: AIRA-75
func TestWatchdogEventsAreWatchVisibleAndNeverJournaled(t *testing.T) {
	base := t.TempDir()
	root := filepath.Join(base, "root")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	s := testStore(t, root, filepath.Join(base, "common"), filepath.Join(base, "state"))
	for _, verb := range []string{"watchdog.trip", "watchdog.intent", "watchdog.outcome", "watchdog.recovered"} {
		if err := s.AppendWatchdogEvent(context.Background(), verb, "target"); err != nil {
			t.Fatalf("%s: %v", verb, err)
		}
	}

	events, _, err := s.EventsSince(context.Background(), 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 4 {
		t.Fatalf("watch stream has %d of 4 watchdog events: %+v", len(events), events)
	}

	var journaled int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM events WHERE project_id=? AND actor='aira-watchdog' AND journaled=1`, s.projectID).Scan(&journaled); err != nil {
		t.Fatal(err)
	}
	if journaled != 0 {
		t.Fatalf("%d watchdog events were journaled; a host-global decision has no per-project provenance to record", journaled)
	}
}
