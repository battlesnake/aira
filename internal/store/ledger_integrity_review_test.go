// verifies: AIRA-301
//
// Build-review regressions for the audit-log ownership rules: a claim needs a
// state that holds every line of the log (not just a counter above it), an
// unreadable log is refused rather than turned into a durable zero, Rebuild
// claims before it replays deferred journal lines, an unreadable marker refuses
// a fresh state, and an invalid-ID receipt still counts toward the counter.

package store

import (
	"bytes"
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"aira/internal/domain"
)

func appendLine(t *testing.T, path, line string) {
	t.Helper()
	file, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY|os.O_CREATE, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	if _, err := file.WriteString(line + "\n"); err != nil {
		t.Fatal(err)
	}
}

func (f ledgerFixture) receiptLine(project, id string, seq int64) string {
	return fmt.Sprintf(`{"project_id":%q,"worktree_id":"other-wt","id":%q,"path":%q,"seq":%d,"at":"2026-10-12T00:00:00Z","state":"allocated","kind":"ticket"}`,
		project, id, filepath.Join(f.root, ".aira", "tickets", id+".md"), seq)
}

func registryLines(t *testing.T, state string) int {
	t.Helper()
	return bytes.Count(readOptional(t, filepath.Join(state, "registry.jsonl")), []byte("\n"))
}

func counterOf(t *testing.T, state, project string) int64 {
	t.Helper()
	db, err := OpenDB(filepath.Join(state, "state.db"), filepath.Join(state, "registry.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	next, ok := eventCounter(t, db.db, project)
	if !ok {
		t.Fatalf("no counter row in %s", state)
	}
	return next
}

// Review finding 1: sequence numbers a state burned in its own database while
// its appends were refused put its counter above the log's high water without
// it having read the lines another state wrote meanwhile. Such a state must not
// claim the repository or append to its log; the state that did absorb the log
// still claims.
func TestABurnedCounterDoesNotProveAbsorption(t *testing.T) {
	ctx := context.Background()
	f := newLedgerFixture(t)
	stateX, stateY := f.state("stateX"), f.state("stateY")
	x, err := f.tryOpen(stateX)
	if err != nil {
		t.Fatal(err)
	}
	createTickets(t, x, 2)
	project := x.projectID
	// The marker is changed while X's session is live: X's creates commit their
	// allocations and are refused at the receipt append.
	writeMarker(t, f.marker(), `{"state_dir":"/somewhere/else"}`+"\n")
	for i := 0; i < 2; i++ {
		_, err := x.CreateTicket(ctx, domain.CreateTicketInput{Title: fmt.Sprintf("burned %d", i)})
		requireCode(t, err, "E_STATE_NOT_OWNER", "create over a changed marker")
	}
	_ = x.Close()
	if err := os.Remove(f.marker()); err != nil {
		t.Fatal(err)
	}

	// Y takes the repository the documented way: register, rebuild, write.
	y := f.open(t, stateY)
	if err := y.Rebuild(ctx); err != nil {
		t.Fatalf("Y Rebuild: %v", err)
	}
	if got, want := markerStateDir(t, f), canonicalStateDir(t, stateY); got != want {
		t.Fatalf("marker names %q after Y's Rebuild, want %q", got, want)
	}
	if err := retitle(y, "AIRA-1", "written by Y"); err != nil {
		t.Fatalf("Y retitle: %v", err)
	}
	_ = y.Close()
	if err := os.Remove(f.marker()); err != nil {
		t.Fatal(err)
	}
	high := f.logHighWater(t, project)
	if counter := counterOf(t, stateX, project); counter <= high {
		t.Fatalf("fixture: X's counter %d is not above the log's high water %d", counter, high)
	}

	before := f.audit(t)
	x2, err := f.tryOpen(stateX)
	if err == nil {
		defer x2.Close()
		if _, statErr := os.Lstat(f.marker()); statErr == nil {
			t.Fatalf("X, which never read Y's line, claimed the repository: marker %q", readOptional(t, f.marker()))
		}
		_, err = x2.CreateTicket(ctx, domain.CreateTicketInput{Title: "from X"})
		if err == nil {
			t.Fatalf("X created a ticket in a log it has not absorbed")
		}
	}
	f.requireAuditUnchanged(t, before, "a state that has not absorbed the log")

	// False-fail twin: Y holds every line, so it claims at its next registration.
	y2 := f.open(t, stateY)
	if got, want := markerStateDir(t, f), canonicalStateDir(t, stateY); got != want {
		t.Fatalf("marker names %q after Y re-registered, want %q", got, want)
	}
	if _, err := y2.CreateTicket(ctx, domain.CreateTicketInput{Title: "from Y"}); err != nil {
		t.Fatalf("Y create after claiming: %v", err)
	}
	if err := y2.Rebuild(ctx); err != nil {
		t.Fatalf("Y Rebuild after claiming: %v", err)
	}
}

// Review finding 2: a log that cannot be read leaves the high water unknown.
// Registration in an unmarked repository refuses with the read error and writes
// nothing; it does not turn the unknown into a durable counter of 0.
func TestAnUnreadableLogRefusesRegistrationAndKeepsTheCounter(t *testing.T) {
	f := newLedgerFixture(t)
	state := f.state("stateX")
	owner := f.open(t, state)
	createTickets(t, owner, 3)
	project := owner.projectID
	_ = owner.Close()
	counterBefore := counterOf(t, state, project)
	if err := os.Remove(f.marker()); err != nil {
		t.Fatal(err)
	}
	first := strings.SplitN(string(readOptional(t, f.journal())), "\n", 2)[0]
	if !strings.Contains(first, `"seq":1`) {
		t.Fatalf("fixture: first journal line is not seq 1: %s", first)
	}
	appendLine(t, f.journal(), fmt.Sprintf(`{"project_id":%q,"seq":1,"at":"2026-10-12T00:00:00Z","actor":"other","verb":"ticket.update","target":"AIRA-1","payload_digest":"sha256:%s"}`,
		project, strings.Repeat("0", 64)))
	registryBefore := registryLines(t, state)

	s, err := f.tryOpen(state)
	if err == nil {
		_ = s.Close()
	}
	requireCode(t, err, "E_JOURNAL_CORRUPT", "registration over an unreadable log")
	if !strings.Contains(err.Error(), "Nothing was written") {
		t.Fatalf("refusal %q does not say nothing was written", err)
	}
	if got := counterOf(t, state, project); got != counterBefore {
		t.Fatalf("counter %d -> %d over an unreadable log", counterBefore, got)
	}
	if got := registryLines(t, state); got != registryBefore {
		t.Fatalf("registry %d -> %d lines over an unreadable log", registryBefore, got)
	}
	if _, err := os.Lstat(f.marker()); !os.IsNotExist(err) {
		t.Fatalf("marker created over an unreadable log: %v", err)
	}
}

// Review finding 3: in an unmarked repository a demoted state with a deferred
// (materialised, not yet journaled) event must still be able to rebuild: Rebuild
// absorbs the log, claims, and only then replays the deferred line.
func TestRebuildClaimsBeforeItReplaysDeferredJournalLines(t *testing.T) {
	ctx := context.Background()
	f := newLedgerFixture(t)
	state := f.state("stateX")
	owner := f.open(t, state)
	createTickets(t, owner, 3)
	project := owner.projectID
	res, err := owner.db.Exec(`UPDATE outbox SET journaled=0 WHERE project_id=? AND materialised=1 AND seq=(SELECT MAX(seq) FROM outbox WHERE project_id=? AND materialised=1)`, project, project)
	if err != nil {
		t.Fatal(err)
	}
	if n, _ := res.RowsAffected(); n != 1 {
		t.Fatalf("fixture: %d outbox rows marked unjournaled, want 1", n)
	}
	_ = owner.Close()
	if err := os.Remove(f.marker()); err != nil {
		t.Fatal(err)
	}
	appendLine(t, f.receipts(), f.receiptLine(project, "AIRA-9", 20))

	s := f.open(t, state)
	if next, _ := eventCounter(t, s.db, project); next != 0 {
		t.Fatalf("fixture: counter after registration = %d, want demoted to 0", next)
	}
	if err := s.Rebuild(ctx); err != nil {
		t.Fatalf("Rebuild of a demoted state with a deferred journal line: %v", err)
	}
	if got, want := markerStateDir(t, f), canonicalStateDir(t, state); got != want {
		t.Fatalf("marker names %q, want %q", got, want)
	}
	if next, _ := eventCounter(t, s.db, project); next <= 20 {
		t.Fatalf("counter after Rebuild = %d, want above the log's high water 20", next)
	}
	var pending int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM outbox WHERE project_id=? AND materialised=1 AND journaled=0`, project).Scan(&pending); err != nil {
		t.Fatal(err)
	}
	if pending != 0 {
		t.Fatalf("%d deferred journal lines left after Rebuild claimed", pending)
	}
	if _, err := s.CreateTicket(ctx, domain.CreateTicketInput{Title: "after rebuild"}); err != nil {
		t.Fatalf("create after Rebuild: %v", err)
	}
}

// Review finding 4 (invariant 6): an unreadable marker refuses a state that has
// never registered here, and that state writes no breadcrumb and no rows.
func TestAnUnreadableMarkerRefusesAFreshState(t *testing.T) {
	f := newLedgerFixture(t)
	owner := f.open(t, f.state("stateX"))
	createTickets(t, owner, 1)
	project := owner.projectID
	writeMarker(t, f.marker(), "{not json\n")
	before := f.audit(t)

	stateY := f.state("stateY")
	y, err := f.tryOpen(stateY)
	if err == nil {
		_ = y.Close()
	}
	requireCode(t, err, "E_STATE_NOT_OWNER", "fresh Open over an unreadable marker")
	if data := readOptional(t, filepath.Join(stateY, "registry.jsonl")); len(bytes.TrimSpace(data)) != 0 {
		t.Fatalf("refused state wrote a breadcrumb: %q", data)
	}
	ydb, err := OpenDB(filepath.Join(stateY, "state.db"), filepath.Join(stateY, "registry.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	defer ydb.Close()
	for table, n := range projectRowCounts(t, ydb.db, project) {
		if n != 0 {
			t.Fatalf("refused state's %s holds %d rows", table, n)
		}
	}
	f.requireAuditUnchanged(t, before, "a refused fresh state")
	if got := string(readOptional(t, f.marker())); got != "{not json\n" {
		t.Fatalf("marker changed to %q", got)
	}
}

// Review finding 5: a current-project receipt with an invalid ID still counts
// toward Rebuild's counter, so an unmarked log can be claimed after Rebuild...
func TestAnInvalidIDReceiptAtTheHighWaterStillLetsRebuildClaim(t *testing.T) {
	ctx := context.Background()
	f := newLedgerFixture(t)
	state := f.state("stateX")
	owner := f.open(t, state)
	createTickets(t, owner, 1)
	project := owner.projectID
	_ = owner.Close()
	if err := os.Remove(f.marker()); err != nil {
		t.Fatal(err)
	}
	high := f.logHighWater(t, project)
	appendLine(t, f.receipts(), f.receiptLine(project, "not an id", high+1))
	removeStateDB(t, state)

	s := f.open(t, state)
	if err := s.Rebuild(ctx); err != nil {
		t.Fatalf("Rebuild with an invalid-ID receipt at the high water: %v", err)
	}
	if got, want := markerStateDir(t, f), canonicalStateDir(t, state); got != want {
		t.Fatalf("marker names %q, want %q", got, want)
	}
}

// ...and, in a marked repository, its sequence number is never handed out again.
func TestAnInvalidIDReceiptSequenceNumberIsNotReissued(t *testing.T) {
	ctx := context.Background()
	f := newLedgerFixture(t)
	state := f.state("stateX")
	owner := f.open(t, state)
	createTickets(t, owner, 1)
	project := owner.projectID
	_ = owner.Close()
	high := f.logHighWater(t, project)
	appendLine(t, f.receipts(), f.receiptLine(project, "not an id", high+1))
	removeStateDB(t, state)

	s := f.open(t, state)
	if err := s.Rebuild(ctx); err != nil {
		t.Fatalf("Rebuild: %v", err)
	}
	ticket, err := s.CreateTicket(ctx, domain.CreateTicketInput{Title: "after rebuild"})
	if err != nil {
		t.Fatalf("create after Rebuild: %v", err)
	}
	prefix, number, _ := splitTicketID(ticket.ID)
	var seq int64
	if err := s.db.QueryRow(`SELECT seq FROM allocations WHERE project_id=? AND prefix=? AND number=?`, project, prefix, number).Scan(&seq); err != nil && err != sql.ErrNoRows {
		t.Fatal(err)
	}
	if seq <= high+1 {
		t.Fatalf("%s minted seq %d, reusing the log's seq %d", ticket.ID, seq, high+1)
	}
}
