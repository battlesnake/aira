package daemon

import (
	"context"
	"errors"
	"testing"
	"time"

	"aira/internal/store"
)

// TestDumpHistoryTimeoutIsGenerousAndDistinctFromAdmitHotPath pins the
// AIRA-242 constant relationship: the diagnostic/batch history-read deadline
// must be its OWN constant, and comfortably larger than the admit hot path's
// 250ms — never the reverse, and never re-collapsed back onto
// admitHistoryTimeout by a future edit.
//
// verifies: AIRA-242
func TestDumpHistoryTimeoutIsGenerousAndDistinctFromAdmitHotPath(t *testing.T) {
	if dumpHistoryTimeout <= admitHistoryTimeout {
		t.Fatalf("dumpHistoryTimeout (%s) must exceed the admit hot-path admitHistoryTimeout (%s)", dumpHistoryTimeout, admitHistoryTimeout)
	}
	if dumpHistoryTimeout < 5*time.Second {
		t.Fatalf("dumpHistoryTimeout = %s is not generous enough for a batch/diagnostic read", dumpHistoryTimeout)
	}
}

// seedLargeConfinePeakHistory grows confine_peak_history to a volume that
// reliably makes a full-table ResourceBudgetSubjects read exceed
// admitHistoryTimeout (250ms) on ordinary hardware — the exact AIRA-242
// mechanism (a long-lived daemon's history has no total-row cap, only a
// newest-20-per-signature retention). Calibrated at 200,000 distinct
// signatures (1 row each, so none is evicted by that per-signature
// retention): consistently ~550-750ms unbounded on this development box,
// comfortably over the 250ms hot-path deadline.
func seedLargeConfinePeakHistory(t *testing.T, db *store.DB) {
	t.Helper()
	const n = 200000
	if err := store.SeedConfinePeakHistoryBulk(db, n); err != nil {
		t.Fatalf("seed %d confine_peak_history rows: %v", n, err)
	}
}

// TestConfineDumpSucceedsOnHistoryThatWouldTripTheAdmitHotPathDeadline is the
// AIRA-242 regression pin: `aira confine --dump` reads the WHOLE retained
// confine_peak_history table (unlike the admit hot path, which reads one
// subject's window), so on a long-lived daemon that read legitimately runs
// past 250ms. Before the fix this handler reused admitHistoryTimeout for that
// read and failed with a bare context-deadline error; the fix (dumpHistoryTimeout)
// must make it succeed on the SAME seeded volume.
//
// The RED half (the old 250ms deadline actually would have tripped on this
// exact seeded volume) is asserted too, but as a skip-safe soft check: on a
// faster machine than this one, the underlying read might complete inside
// 250ms even unbounded, in which case the RED half proves nothing and is
// skipped rather than spuriously failing the build — the GREEN half below it
// (the handler succeeds under the real, generous deadline) is unconditional
// and is what actually guards the fix.
//
// verifies: AIRA-242
func TestConfineDumpSucceedsOnHistoryThatWouldTripTheAdmitHotPathDeadline(t *testing.T) {
	server := ciDumpTestServer(t)
	seedLargeConfinePeakHistory(t, server.db)

	t.Run("RED: the old 250ms admit hot-path deadline would have tripped this read", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), admitHistoryTimeout)
		defer cancel()
		_, err := server.db.ResourceBudgetSubjects(ctx)
		if err == nil {
			t.Skip("this environment's disk/CPU is fast enough that the seeded volume did not exceed 250ms; the RED half of this pin is inconclusive here, not disproven")
		}
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("expected a deadline-exceeded error, got: %v", err)
		}
	})

	response := server.confineDump(map[string]any{"owner": "session-a"})
	if !response.OK {
		t.Fatalf("confineDump on a large (200,000-row) history must succeed under the generous dumpHistoryTimeout: response=%+v", response)
	}
}

// TestConfineBudgetSucceedsOnHistoryThatWouldTripTheAdmitHotPathDeadline is
// confineBudget's half of the same AIRA-242 pin: it wraps the IDENTICAL
// ResourceBudgetSubjects(ctx) read as confineDump, so it must be fixed and
// pinned the same way.
//
// verifies: AIRA-242
func TestConfineBudgetSucceedsOnHistoryThatWouldTripTheAdmitHotPathDeadline(t *testing.T) {
	server, db := budgetTestServer(t)
	seedLargeConfinePeakHistory(t, db)

	response := server.confineBudget(map[string]any{"owner": "session-a"})
	if !response.OK {
		t.Fatalf("confineBudget on a large (200,000-row) history must succeed under the generous dumpHistoryTimeout: response=%+v", response)
	}
}
