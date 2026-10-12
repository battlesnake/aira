// verifies: AIRA-301
//
// Build-review round 5 regressions: a demoted state's Rebuild counts above the
// sequence numbers its own database holds BEFORE it mints any recovered
// allocation, each own-table member of that lift is pinned (an events-only
// burned number in particular), and the journal reader's same-file duplicate
// check uses the same line identity as every other journal comparison.

package store

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"aira/internal/domain"
)

// burnedRetitles is the round-4 demotion shape: owner X with two tickets (log
// seqs 1 and 2) whose two retitles were refused at the journal (the marker
// changed by hand), so seqs 3 and 4 live only in X's database; event 2 is then
// dropped from the database, so X's registration does not count the log as held
// and demotes its counter to 0.
func burnedRetitles(t *testing.T) (ledgerFixture, string, string) {
	t.Helper()
	f := newLedgerFixture(t)
	state := f.state("stateX")
	x, err := f.tryOpen(state)
	if err != nil {
		t.Fatal(err)
	}
	createTickets(t, x, 2)
	project := x.projectID
	writeMarker(t, f.marker(), `{"state_dir":"/somewhere/else"}`+"\n")
	for _, title := range []string{"burned 1", "burned 2"} {
		requireCode(t, retitle(x, "AIRA-1", title), "E_STATE_NOT_OWNER", "retitle over a changed marker")
	}
	if _, err := x.db.Exec(`DELETE FROM events WHERE project_id=? AND seq=2`, project); err != nil {
		t.Fatal(err)
	}
	_ = x.Close()
	if err := os.Remove(f.marker()); err != nil {
		t.Fatal(err)
	}
	return f, state, project
}

// Review round 5, P2-1: the counter must be above the database's own sequence
// numbers before Rebuild's scanned-file loops mint a recovered allocation, not
// only after them. A receipt-less ticket file in the demotion shape is minted at
// the first number above both the log's high water (2) and the database's own
// burned numbers (3, 4); minting from the log's high water alone hands out 3.
func TestARecoveredAllocationAfterADemotionIsMintedAboveItsOwnEvents(t *testing.T) {
	ctx := context.Background()
	f, state, project := burnedRetitles(t)
	f.writeReceiptlessTicket(t, "AIRA-2", "AIRA-3")

	s := f.open(t, state)
	if next, _ := eventCounter(t, s.db, project); next != 0 {
		t.Fatalf("fixture: counter after registration = %d, want demoted to 0", next)
	}
	var ownMax int64
	if err := s.db.QueryRow(`SELECT MAX(seq) FROM events WHERE project_id=?`, project).Scan(&ownMax); err != nil {
		t.Fatal(err)
	}
	if high := f.logHighWater(t, project); ownMax <= high {
		t.Fatalf("fixture: own max seq %d is not above the log's high water %d", ownMax, high)
	}
	if err := s.Rebuild(ctx); err != nil {
		t.Fatalf("Rebuild with a receipt-less file after a demotion: %v", err)
	}
	var allocationState string
	var seq int64
	if err := s.db.QueryRow(`SELECT state, seq FROM allocations WHERE project_id=? AND prefix='AIRA' AND number=3`, project).Scan(&allocationState, &seq); err != nil {
		t.Fatalf("recovered AIRA-3: %v", err)
	}
	if allocationState != "recovered" || seq <= ownMax {
		t.Fatalf("AIRA-3 state=%s seq=%d, want recovered above the database's own max %d", allocationState, seq, ownMax)
	}
	if got, want := markerStateDir(t, f), canonicalStateDir(t, state); got != want {
		t.Fatalf("marker %q, want %q", got, want)
	}
	if _, err := s.CreateTicket(ctx, domain.CreateTicketInput{Title: "after rebuild"}); err != nil {
		t.Fatalf("create after Rebuild: %v", err)
	}
	if err := s.Rebuild(ctx); err != nil {
		t.Fatalf("second Rebuild: %v", err)
	}
}

// Review round 5, P2-2: the 'events' member of the own-table lift is pinned on
// its own. Watchdog events are the one producer whose sequence numbers live only
// in the events table (no outbox row, no allocation, never journaled), so in
// the demotion shape only that member keeps the counter above them.
func TestRebuildAfterADemotionCountsAboveItsOwnWatchdogEvents(t *testing.T) {
	ctx := context.Background()
	f := newLedgerFixture(t)
	state := f.state("stateX")
	x, err := f.tryOpen(state)
	if err != nil {
		t.Fatal(err)
	}
	createTickets(t, x, 2)
	project := x.projectID
	for _, verb := range []string{"watchdog.trip", "watchdog.recovered"} {
		if err := x.AppendWatchdogEvent(ctx, verb, "job"); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := x.db.Exec(`DELETE FROM events WHERE project_id=? AND seq=2`, project); err != nil {
		t.Fatal(err)
	}
	var watchdogMax, otherMax int64
	if err := x.db.QueryRow(`SELECT MAX(seq) FROM events WHERE project_id=? AND actor='aira-watchdog'`, project).Scan(&watchdogMax); err != nil {
		t.Fatal(err)
	}
	if err := x.db.QueryRow(`SELECT MAX(m) FROM (SELECT COALESCE(MAX(seq),0) AS m FROM outbox WHERE project_id=? UNION ALL SELECT COALESCE(MAX(seq),0) FROM allocations WHERE project_id=?)`, project, project).Scan(&otherMax); err != nil {
		t.Fatal(err)
	}
	if high := f.logHighWater(t, project); watchdogMax <= high || watchdogMax <= otherMax {
		t.Fatalf("fixture: watchdog max %d must be above the log's high water %d and the outbox/allocation max %d", watchdogMax, high, otherMax)
	}
	_ = x.Close()
	if err := os.Remove(f.marker()); err != nil {
		t.Fatal(err)
	}

	s := f.open(t, state)
	if next, _ := eventCounter(t, s.db, project); next != 0 {
		t.Fatalf("fixture: counter after registration = %d, want demoted to 0", next)
	}
	if err := s.Rebuild(ctx); err != nil {
		t.Fatalf("Rebuild: %v", err)
	}
	if next, _ := eventCounter(t, s.db, project); next <= watchdogMax {
		t.Fatalf("counter after Rebuild = %d, not above the database's own watchdog event %d", next, watchdogMax)
	}
	if _, err := s.CreateTicket(ctx, domain.CreateTicketInput{Title: "after rebuild"}); err != nil {
		t.Fatalf("create after Rebuild: %v", err)
	}
}

// Review round 5, P3: two journal lines at one project and sequence number that
// differ only in actor or time are two different lines, as appendEventIfMissing,
// holdsAuditLog and Rebuild's event replay already treat them (sameJournalLine).
// The reader refuses them like any other same-seq conflict; a byte-identical
// repeat is still accepted.
func TestReadJournalUsesTheFullLineIdentity(t *testing.T) {
	f := ledgerFixture{}
	project := "project0000000000"
	base := f.journalLine(project, 1, "aira", "ticket.update", "AIRA-1")
	for name, second := range map[string]string{
		"actor": f.journalLine(project, 1, "someone-else", "ticket.update", "AIRA-1"),
		"time":  strings.Replace(base, "2026-10-12T00:00:00Z", "2001-01-01T00:00:00Z", 1),
	} {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "journal.jsonl")
			if err := os.WriteFile(path, []byte(base+"\n"+second+"\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			_, err := readJournal(path)
			requireCode(t, err, "E_JOURNAL_CORRUPT", "two lines at one seq differing only in "+name)
		})
	}
	path := filepath.Join(t.TempDir(), "journal.jsonl")
	if err := os.WriteFile(path, []byte(base+"\n"+base+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := readJournal(path); err != nil {
		t.Fatalf("a repeated identical line: %v", err)
	}
}
