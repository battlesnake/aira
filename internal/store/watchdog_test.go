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
