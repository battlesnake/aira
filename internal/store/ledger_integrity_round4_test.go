// verifies: AIRA-301
//
// Build-review round 4 regressions for the audit-log ownership rules: a log
// line is held only when the database holds the same line (actor and time
// included), each sub-check of holdsAuditLog is pinned in both directions, a
// state that does not hold the log is demoted before it can write anything,
// a demoted state's Rebuild counts above its own events, and 'aira check' is
// the remedy its refusal texts say it is.

package store

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"aira/internal/domain"
)

// editJournalLine rewrites the one current-project journal line at seq through
// edit (drop returns false to delete it).
func (f ledgerFixture) editJournalLine(t *testing.T, project string, seq int64, edit func(map[string]any) bool) {
	t.Helper()
	var out []string
	hit := 0
	for _, line := range strings.Split(strings.TrimRight(string(readOptional(t, f.journal())), "\n"), "\n") {
		var m map[string]any
		if err := json.Unmarshal([]byte(line), &m); err != nil {
			t.Fatal(err)
		}
		if n, ok := m["seq"].(float64); ok && int64(n) == seq && m["project_id"] == project {
			hit++
			if !edit(m) {
				continue
			}
			b, err := json.Marshal(m)
			if err != nil {
				t.Fatal(err)
			}
			line = string(b)
		}
		out = append(out, line)
	}
	if hit != 1 {
		t.Fatalf("fixture: %d journal lines at seq %d", hit, seq)
	}
	if err := os.WriteFile(f.journal(), []byte(strings.Join(out, "\n")+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
}

func (f ledgerFixture) journalLine(project string, seq int64, actor, verb, target string) string {
	return fmt.Sprintf(`{"project_id":%q,"seq":%d,"at":"2026-10-12T00:00:00Z","actor":%q,"verb":%q,"target":%q,"payload_digest":"sha256:%s"}`,
		project, seq, actor, verb, target, strings.Repeat("0", 64))
}

// unmarkedOwner: owner X created two tickets (seqs 1 and 2, counter 3), then
// the marker was removed (a pre-upgrade repository).
func unmarkedOwner(t *testing.T) (ledgerFixture, string, string) {
	t.Helper()
	f := newLedgerFixture(t)
	state := f.state("stateX")
	x := f.open(t, state)
	createTickets(t, x, 2)
	project := x.projectID
	_ = x.Close()
	if err := os.Remove(f.marker()); err != nil {
		t.Fatal(err)
	}
	return f, state, project
}

func markerAbsent(t *testing.T, f ledgerFixture) bool {
	t.Helper()
	_, err := os.Lstat(f.marker())
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	return os.IsNotExist(err)
}

// Review round 4, finding 5 (and finding 3, the receipt half): each comparison
// holdsAuditLog makes is pinned. A log line the database does not hold, in any
// one field, stops the claim; lines it must ignore do not.
func TestEveryHoldsAuditLogComparisonIsPinned(t *testing.T) {
	refused := []struct {
		name  string
		setup func(t *testing.T, f ledgerFixture, state, project string)
	}{
		{"receipt for an ID the state lacks", func(t *testing.T, f ledgerFixture, state, project string) {
			appendLine(t, f.receipts(), f.receiptLine(project, "AIRA-7", 1))
		}},
		{"receipt at another seq", func(t *testing.T, f ledgerFixture, state, project string) {
			appendLine(t, f.receipts(), f.receiptLine(project, "AIRA-1", 2))
		}},
		{"journal digest", func(t *testing.T, f ledgerFixture, state, project string) {
			f.editJournalLine(t, project, 2, func(m map[string]any) bool { m["payload_digest"] = "sha256:" + strings.Repeat("a", 64); return true })
		}},
		{"journal verb", func(t *testing.T, f ledgerFixture, state, project string) {
			f.editJournalLine(t, project, 2, func(m map[string]any) bool { m["verb"] = "ticket.update"; return true })
		}},
		{"journal target", func(t *testing.T, f ledgerFixture, state, project string) {
			f.editJournalLine(t, project, 2, func(m map[string]any) bool { m["target"] = "AIRA-9"; return true })
		}},
		{"journal actor", func(t *testing.T, f ledgerFixture, state, project string) {
			f.editJournalLine(t, project, 2, func(m map[string]any) bool { m["actor"] = "someone-else"; return true })
		}},
		{"journal time", func(t *testing.T, f ledgerFixture, state, project string) {
			f.editJournalLine(t, project, 2, func(m map[string]any) bool { m["at"] = "2001-01-01T00:00:00Z"; return true })
		}},
		{"journal line with no event row", func(t *testing.T, f ledgerFixture, state, project string) {
			db, err := OpenDB(filepath.Join(state, "state.db"), filepath.Join(state, "registry.jsonl"))
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			if _, err := db.db.Exec(`UPDATE event_counters SET next_seq=10 WHERE project_id=?`, project); err != nil {
				t.Fatal(err)
			}
			appendLine(t, f.journal(), f.journalLine(project, 5, "other", "ticket.update", "AIRA-1"))
		}},
	}
	for _, tc := range refused {
		t.Run("refused/"+tc.name, func(t *testing.T) {
			f, state, project := unmarkedOwner(t)
			tc.setup(t, f, state, project)
			s, err := f.tryOpen(state)
			if err == nil {
				_ = s.Close()
			}
			if !markerAbsent(t, f) {
				t.Fatalf("claimed a log its database does not hold (%s): marker %q (open err=%v)", tc.name, readOptional(t, f.marker()), err)
			}
		})
	}
	claimed := []struct {
		name  string
		setup func(t *testing.T, f ledgerFixture, project string)
	}{
		{"receipt at seq 0", func(t *testing.T, f ledgerFixture, project string) {
			appendLine(t, f.receipts(), f.receiptLine(project, "AIRA-8", 0))
		}},
		{"another project's lines", func(t *testing.T, f ledgerFixture, project string) {
			other := "otherproject0000"
			appendLine(t, f.receipts(), f.receiptLine(other, "AIRA-7", 1))
			appendLine(t, f.journal(), f.journalLine(other, 1, "other", "ticket.update", "AIRA-7"))
		}},
	}
	for _, tc := range claimed {
		t.Run("claimed/"+tc.name, func(t *testing.T) {
			f, state, project := unmarkedOwner(t)
			tc.setup(t, f, project)
			f.open(t, state)
			if got, want := markerStateDir(t, f), canonicalStateDir(t, state); got != want {
				t.Fatalf("marker %q, want %q", got, want)
			}
		})
	}
}

// Review round 4, finding 1: two updates of the same ticket at the same
// sequence number by two states share verb, target and digest. A state whose
// own update was refused at the journal must not count the other state's line
// as held: it neither claims at registration nor through Rebuild, which names
// the conflicting line instead.
func TestABurnedUpdateOfTheSameTicketDoesNotProveAbsorption(t *testing.T) {
	ctx := context.Background()
	f := newLedgerFixture(t)
	stateX, stateY := f.state("stateX"), f.state("stateY")
	x, err := f.tryOpen(stateX)
	if err != nil {
		t.Fatal(err)
	}
	createTickets(t, x, 2)
	project := x.projectID
	writeMarker(t, f.marker(), `{"state_dir":"/somewhere/else"}`+"\n")
	requireCode(t, retitle(x, "AIRA-1", "X title"), "E_STATE_NOT_OWNER", "X retitle over a changed marker")
	_ = x.Close()
	if err := os.Remove(f.marker()); err != nil {
		t.Fatal(err)
	}
	y := f.open(t, stateY)
	if err := y.Rebuild(ctx); err != nil {
		t.Fatalf("Y Rebuild: %v", err)
	}
	if err := retitle(y, "AIRA-1", "Y title"); err != nil {
		t.Fatalf("Y retitle: %v", err)
	}
	_ = y.Close()
	if err := os.Remove(f.marker()); err != nil {
		t.Fatal(err)
	}
	if high, counter := f.logHighWater(t, project), counterOf(t, stateX, project); counter <= high {
		t.Fatalf("fixture: X's counter %d is not above the log's high water %d", counter, high)
	}

	before := f.audit(t)
	x2, err := f.tryOpen(stateX)
	if err != nil {
		t.Fatalf("X re-register: %v", err)
	}
	if !markerAbsent(t, f) {
		t.Fatalf("X, whose seq 3 is not Y's seq 3, claimed the repository: marker %q", readOptional(t, f.marker()))
	}
	if _, err := x2.CreateTicket(ctx, domain.CreateTicketInput{Title: "from X"}); err == nil {
		t.Fatal("X created a ticket in a log it has not absorbed")
	}
	requireCode(t, x2.Rebuild(ctx), "E_JOURNAL_CORRUPT", "X Rebuild over Y's update at X's burned seq")
	if !markerAbsent(t, f) {
		t.Fatalf("X's failed Rebuild claimed the repository: marker %q", readOptional(t, f.marker()))
	}
	f.requireAuditUnchanged(t, before, "a state that has not absorbed the log")
	_ = x2.Close()

	// False-fail twin: Y holds every line and claims at its next registration.
	y2 := f.open(t, stateY)
	if got, want := markerStateDir(t, f), canonicalStateDir(t, stateY); got != want {
		t.Fatalf("marker names %q after Y re-registered, want %q", got, want)
	}
	if err := y2.Rebuild(ctx); err != nil {
		t.Fatalf("Y Rebuild after claiming: %v", err)
	}
}

// Review round 4, finding 4: spec section 1's incident shape on a pre-upgrade
// log. The owner's counter is above the log's high water but its database does
// not hold the two foreign receipts. Registration demotes it, so its writes are
// refused before any side effect (no burned ID, no edited ticket file); Rebuild
// names the receipt; after the hand repair Rebuild claims.
func TestAStateThatDoesNotHoldTheLogIsDemotedBeforeItWrites(t *testing.T) {
	ctx := context.Background()
	f := newLedgerFixture(t)
	state := f.state("stateX")
	x := f.open(t, state)
	ids := createTickets(t, x, 3)
	if err := retitle(x, ids[0], "u1"); err != nil {
		t.Fatal(err)
	}
	if err := retitle(x, ids[0], "u2"); err != nil {
		t.Fatal(err)
	}
	createTickets(t, x, 2)
	project := x.projectID
	_ = x.Close()
	if err := os.Remove(f.marker()); err != nil {
		t.Fatal(err)
	}
	clean := readOptional(t, f.receipts())
	appendLine(t, f.receipts(), f.receiptLine(project, "AIRA-4", 4))
	appendLine(t, f.receipts(), f.receiptLine(project, "AIRA-5", 5))
	if high, counter := f.logHighWater(t, project), counterOf(t, state, project); counter <= high {
		t.Fatalf("fixture: counter %d is not above the log's high water %d", counter, high)
	}

	s := f.open(t, state)
	if !markerAbsent(t, f) {
		t.Fatalf("claimed a log holding receipts it lacks: marker %q", readOptional(t, f.marker()))
	}
	if next, _ := eventCounter(t, s.db, project); next != 0 {
		t.Fatalf("counter after registration = %d, want demoted to 0", next)
	}
	before := f.audit(t)
	var allocationsBefore int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM allocations WHERE project_id=?`, project).Scan(&allocationsBefore); err != nil {
		t.Fatal(err)
	}
	ticketPath := filepath.Join(f.root, ".aira", "tickets", ids[1]+".md")
	fileBefore := readOptional(t, ticketPath)

	_, err := s.CreateTicket(ctx, domain.CreateTicketInput{Title: "after cutover"})
	requireCode(t, err, "E_RECONCILE_REQUIRED", "create by a state that does not hold the log")
	requireCode(t, retitle(s, ids[1], "edited after cutover"), "E_RECONCILE_REQUIRED", "update by a state that does not hold the log")
	var allocationsAfter int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM allocations WHERE project_id=?`, project).Scan(&allocationsAfter); err != nil {
		t.Fatal(err)
	}
	if allocationsAfter != allocationsBefore {
		t.Fatalf("refused create left an allocation: %d -> %d rows", allocationsBefore, allocationsAfter)
	}
	if got := readOptional(t, ticketPath); string(got) != string(fileBefore) {
		t.Fatalf("refused update changed the ticket file")
	}
	f.requireAuditUnchanged(t, before, "refused writes")

	err = s.Rebuild(ctx)
	requireCode(t, err, "E_JOURNAL_CORRUPT", "Rebuild over the foreign receipts")
	if !strings.Contains(err.Error(), "AIRA-4") {
		t.Fatalf("Rebuild refusal does not name the receipt: %v", err)
	}
	// The hand repair: drop the foreign receipts.
	if err := os.WriteFile(f.receipts(), clean, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := s.Rebuild(ctx); err != nil {
		t.Fatalf("Rebuild after the repair: %v", err)
	}
	if got, want := markerStateDir(t, f), canonicalStateDir(t, state); got != want {
		t.Fatalf("marker %q, want %q", got, want)
	}
	if _, err := s.CreateTicket(ctx, domain.CreateTicketInput{Title: "after repair"}); err != nil {
		t.Fatalf("create after the repair: %v", err)
	}
}

// Review round 4, finding 4 caveat: a demoted state's Rebuild must leave its
// counter above the sequence numbers its own database already holds, not only
// above the log's high water: its own deferred lines are appended after the
// claim, and the next number must not collide with them.
func TestRebuildAfterADemotionCountsAboveItsOwnEvents(t *testing.T) {
	ctx := context.Background()
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
	// The database no longer holds the event of log line 2, so registration
	// does not count the log as held and demotes the counter.
	if _, err := x.db.Exec(`DELETE FROM events WHERE project_id=? AND seq=2`, project); err != nil {
		t.Fatal(err)
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
	if got, want := markerStateDir(t, f), canonicalStateDir(t, state); got != want {
		t.Fatalf("marker %q, want %q", got, want)
	}
	high := f.logHighWater(t, project)
	if next, _ := eventCounter(t, s.db, project); next <= high {
		t.Fatalf("counter after Rebuild = %d, not above the log's high water %d (its own deferred lines were appended after the claim)", next, high)
	}
	if _, err := s.CreateTicket(ctx, domain.CreateTicketInput{Title: "after rebuild"}); err != nil {
		t.Fatalf("create after Rebuild: %v", err)
	}
	if err := s.Rebuild(ctx); err != nil {
		t.Fatalf("second Rebuild: %v", err)
	}
}

// deferredLineShape: owner X with three tickets, the last one's journal line
// deferred (materialised, not journaled), the marker removed and a foreign
// receipt at seq 20 appended, so X's registration demotes its counter.
func deferredLineShape(t *testing.T) (ledgerFixture, string, string) {
	t.Helper()
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
	return f, state, project
}

// Review round 4, finding 2: 'aira check' is the remedy the refusal texts name.
// In an unmarked repository with a deferred row it claims, through Check (not
// only through a direct Rebuild).
func TestCheckClaimsAnUnmarkedRepositoryWithADeferredLine(t *testing.T) {
	ctx := context.Background()
	f, state, project := deferredLineShape(t)
	s := f.open(t, state)
	if next, _ := eventCounter(t, s.db, project); next != 0 {
		t.Fatalf("fixture: counter after registration = %d, want demoted to 0", next)
	}
	report, err := s.Check(ctx)
	if err != nil {
		t.Fatalf("Check in an unmarked repository with a deferred line: %v", err)
	}
	if got, want := markerStateDir(t, f), canonicalStateDir(t, state); got != want {
		t.Fatalf("marker %q, want %q", got, want)
	}
	for _, dimension := range []string{"reconcile-integrity", "rebuild-integrity"} {
		if report.Dimensions[dimension] != "pass" {
			t.Fatalf("%s = %q, want pass: %#v", dimension, report.Dimensions[dimension], report.Findings)
		}
	}
	var pending int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM outbox WHERE project_id=? AND (materialised=0 OR journaled=0)`, project).Scan(&pending); err != nil {
		t.Fatal(err)
	}
	if pending != 0 {
		t.Fatalf("%d pending rows left after Check claimed", pending)
	}
	if _, err := s.CreateTicket(ctx, domain.CreateTicketInput{Title: "after check"}); err != nil {
		t.Fatalf("create after Check: %v", err)
	}
}

// ...and when the claim is impossible, Check reports a verdict naming the line
// that stops it rather than a bare marker refusal from its reconcile step.
func TestCheckNamesTheLineThatStopsTheClaim(t *testing.T) {
	ctx := context.Background()
	f := newLedgerFixture(t)
	stateX, stateY := f.state("stateX"), f.state("stateY")
	x, err := f.tryOpen(stateX)
	if err != nil {
		t.Fatal(err)
	}
	createTickets(t, x, 2)
	writeMarker(t, f.marker(), `{"state_dir":"/somewhere/else"}`+"\n")
	requireCode(t, retitle(x, "AIRA-1", "X title"), "E_STATE_NOT_OWNER", "X retitle over a changed marker")
	_ = x.Close()
	if err := os.Remove(f.marker()); err != nil {
		t.Fatal(err)
	}
	y := f.open(t, stateY)
	if err := y.Rebuild(ctx); err != nil {
		t.Fatalf("Y Rebuild: %v", err)
	}
	if _, err := y.CreateTicket(ctx, domain.CreateTicketInput{Title: "Y create"}); err != nil {
		t.Fatalf("Y create: %v", err)
	}
	_ = y.Close()
	if err := os.Remove(f.marker()); err != nil {
		t.Fatal(err)
	}

	before := f.audit(t)
	x2 := f.open(t, stateX)
	report, err := x2.Check(ctx)
	if err != nil {
		t.Fatalf("Check returned an error instead of a verdict: %v", err)
	}
	if report.Verdict != "fail" {
		t.Fatalf("verdict %q, want fail", report.Verdict)
	}
	found := false
	for _, finding := range report.Findings {
		if finding.Code == "E_JOURNAL_CORRUPT" && strings.Contains(finding.Message, "/3") {
			found = true
		}
	}
	if !found {
		t.Fatalf("no finding names the conflicting line at seq 3: %#v", report.Findings)
	}
	if !markerAbsent(t, f) {
		t.Fatalf("Check claimed a log it could not absorb: marker %q", readOptional(t, f.marker()))
	}
	f.requireAuditUnchanged(t, before, "a Check that could not claim")
}
