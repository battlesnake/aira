// verifies: AIRA-73 — a conflicted pending outbox intent has an explicit retire
// path, and that path refuses everything reconcile could still complete.

package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"aira/internal/domain"
)

// ---------------------------------------------------------------------------
// fixtures
// ---------------------------------------------------------------------------

type retireFixture struct {
	store        *Store
	root         string
	base         string
	ticketID     string
	path         string
	precondition string
	original     []byte
	intended     []byte
	seq          int64
}

// newRetireFixture builds the exact state AIRA-73 is about: one pending intent
// whose path a third party has taken. It returns the fixture with the intent's
// sequence already resolved.
func newRetireFixture(t *testing.T) retireFixture {
	t.Helper()
	base := t.TempDir()
	root := filepath.Join(base, "main")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	s := testStore(t, root, filepath.Join(base, "common"), filepath.Join(base, "state"))
	ctx := context.Background()
	ticket, err := s.CreateTicket(ctx, domain.CreateTicketInput{
		Title: "conflicted", Kind: domain.KindFeature, Severity: domain.SeverityP2,
	})
	if err != nil {
		t.Fatalf("create ticket: %v", err)
	}
	path := filepath.Join(root, ".aira", "tickets", ticket.ID+".md")
	original, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	precondition := digestBytes(original)
	// The intended bytes must be a VALID ticket render: reconcile's
	// already-written and replayable branches both materialise them, and a
	// materialisation parses the frontmatter. A junk payload would make those
	// branches fail for a reason that has nothing to do with what is under test.
	parsed, body, err := domain.ParseTicket(original)
	if err != nil {
		t.Fatal(err)
	}
	parsed.Title = "intended title, never written"
	intended, err := domain.RenderTicket(parsed, body)
	if err != nil {
		t.Fatal(err)
	}
	intent, err := s.preparePathMutation(ctx, path, precondition, intended, "ticket.update")
	if err != nil {
		t.Fatalf("prepare the intent: %v", err)
	}
	if err := os.WriteFile(path, []byte("--- a third party got here first ---\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return retireFixture{store: s, root: root, base: base, ticketID: ticket.ID, path: path,
		precondition: precondition, original: original, intended: intended, seq: intent.Seq}
}

func (f retireFixture) pendingCount(t *testing.T) int {
	t.Helper()
	var pending int
	if err := f.store.db.QueryRowContext(context.Background(),
		`SELECT count(*) FROM outbox WHERE project_id=? AND materialised=0`, f.store.projectID).Scan(&pending); err != nil {
		t.Fatal(err)
	}
	return pending
}

func (f retireFixture) findingKey() string {
	return reconcileFindingKey(f.store.worktreeID, f.seq)
}

// durableSnapshot is every durable artefact a refused retire must leave alone.
// Without it a refusal test can pass merely because the expected error came
// back after a destructive mutation had already happened.
type durableSnapshot struct {
	outbox      []string
	events      []string
	allocations []string
	findings    []string
	receipts    string
	journal     string
}

func snapshotDurableState(t *testing.T, s *Store) durableSnapshot {
	t.Helper()
	ctx := context.Background()
	read := func(query string) []string {
		rows, err := s.db.QueryContext(ctx, query, s.projectID)
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		columns, err := rows.Columns()
		if err != nil {
			t.Fatal(err)
		}
		var out []string
		for rows.Next() {
			cells := make([]any, len(columns))
			for i := range cells {
				cells[i] = new(any)
			}
			if err := rows.Scan(cells...); err != nil {
				t.Fatal(err)
			}
			parts := make([]string, len(cells))
			for i, cell := range cells {
				parts[i] = fmt.Sprintf("%v", *(cell.(*any)))
			}
			out = append(out, strings.Join(parts, "\x1f"))
		}
		if err := rows.Err(); err != nil {
			t.Fatal(err)
		}
		sort.Strings(out)
		return out
	}
	readFile := func(name string) string {
		data, err := os.ReadFile(filepath.Join(s.auditDir, name))
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			t.Fatal(err)
		}
		return string(data)
	}
	return durableSnapshot{
		outbox:      read(`SELECT seq,worktree_id,path,verb,precondition_digest,intended_digest,materialised,journaled,allocation_id FROM outbox WHERE project_id=?`),
		events:      read(`SELECT seq,actor,verb,target,payload_digest,journaled FROM events WHERE project_id=?`),
		allocations: read(`SELECT prefix,number,worktree_id,state,path,seq,kind FROM allocations WHERE project_id=?`),
		findings:    read(`SELECT finding_key,subtype,code,subject FROM findings WHERE project_id=?`),
		receipts:    readFile("receipts.jsonl"),
		journal:     readFile("journal.jsonl"),
	}
}

func assertDurableStateUnchanged(t *testing.T, before, after durableSnapshot, what string) {
	t.Helper()
	compare := func(name string, a, b []string) {
		if strings.Join(a, "\n") != strings.Join(b, "\n") {
			t.Fatalf("%s changed %s:\nbefore=%v\nafter=%v", what, name, a, b)
		}
	}
	compare("outbox", before.outbox, after.outbox)
	compare("events", before.events, after.events)
	compare("allocations", before.allocations, after.allocations)
	compare("findings", before.findings, after.findings)
	if before.receipts != after.receipts {
		t.Fatalf("%s changed receipts.jsonl:\nbefore=%q\nafter=%q", what, before.receipts, after.receipts)
	}
	if before.journal != after.journal {
		t.Fatalf("%s changed journal.jsonl:\nbefore=%q\nafter=%q", what, before.journal, after.journal)
	}
}

func assertCode(t *testing.T, err error, want string) {
	t.Helper()
	if err == nil {
		t.Fatalf("expected %s, got nil error", want)
	}
	if got := ErrorCode(err); got != want {
		t.Fatalf("error code = %s (%v), want %s", got, err, want)
	}
}

// ---------------------------------------------------------------------------
// the classifier — the single decision retire and reconcile share
// ---------------------------------------------------------------------------

// TestClassifyPendingIntentMatchesReconcile pins that the classifier's verdict
// and reconcile's real behaviour agree for every disposition. Without this, the
// classifier could drift from the loop it was extracted from, and the whole
// "retire never discards work reconcile could still complete" guarantee would
// become an unverified comment.
//
// The precondition == digest(intended) row is the one that makes branch ORDER
// observable: both "already written" and "replayable" match, and reconcile
// takes the already-written branch.
func TestClassifyPendingIntentMatchesReconcile(t *testing.T) {
	intended := []byte("intended\n")
	intendedDigest := digestBytes(intended)
	for _, tc := range []struct {
		name        string
		intended    []byte
		precondtion string
		onDisk      string
		want        intentDisposition
	}{
		{"no bytes to write", nil, "", "", dispositionReceiptOnly},
		{"empty bytes to write", []byte{}, "before", "whatever", dispositionReceiptOnly},
		{"file already holds the intended bytes", intended, "before", intendedDigest, dispositionAlreadyWritten},
		{"file still matches the precondition", intended, "before", "before", dispositionReplayable},
		{"no-op write prefers already-written", intended, intendedDigest, intendedDigest, dispositionAlreadyWritten},
		{"absent file with an empty precondition is replayable", intended, "", "", dispositionReplayable},
		{"third party owns the path", intended, "before", "third-party", dispositionConflicted},
	} {
		if got := classifyPendingIntent(tc.intended, tc.precondtion, tc.onDisk); got != tc.want {
			t.Errorf("%s: classify = %v, want %v", tc.name, got, tc.want)
		}
	}

	// And the same four dispositions against a real store, so the table above
	// cannot quietly describe something reconcile does not do.
	t.Run("reconcile completes the already-written case", func(t *testing.T) {
		f := newRetireFixture(t)
		if err := os.WriteFile(f.path, f.intended, 0o644); err != nil {
			t.Fatal(err)
		}
		if err := f.store.reconcile(context.Background()); err != nil {
			t.Fatalf("reconcile = %v, want nil", err)
		}
		if got := f.pendingCount(t); got != 0 {
			t.Fatalf("pending=%d, want 0", got)
		}
	})
	t.Run("reconcile replays the precondition case", func(t *testing.T) {
		f := newRetireFixture(t)
		if err := os.WriteFile(f.path, f.original, 0o644); err != nil {
			t.Fatal(err)
		}
		if err := f.store.reconcile(context.Background()); err != nil {
			t.Fatalf("reconcile = %v, want nil", err)
		}
		written, err := os.ReadFile(f.path)
		if err != nil {
			t.Fatal(err)
		}
		if string(written) != string(f.intended) {
			t.Fatalf("reconcile did not replay the intended bytes, file=%q", written)
		}
	})
	t.Run("reconcile finalises a receipt-only intent", func(t *testing.T) {
		f := newRetireFixture(t)
		ctx := context.Background()
		if _, err := f.store.db.ExecContext(ctx, `UPDATE outbox SET intended_bytes=NULL WHERE project_id=? AND seq=?`,
			f.store.projectID, f.seq); err != nil {
			t.Fatal(err)
		}
		// And an empty-path row of the kind lease events park, which reconcile
		// selects project-wide and must also finalise without reading any file.
		if _, err := f.store.db.ExecContext(ctx, `INSERT INTO outbox(project_id,seq,worktree_id,path,verb,precondition_digest,intended_digest,intended_bytes)
			VALUES(?,?,?,'','lease.claim','','',NULL)`, f.store.projectID, f.seq+500, f.store.worktreeID); err != nil {
			t.Fatal(err)
		}
		if _, err := f.store.db.ExecContext(ctx, `INSERT INTO events(project_id,seq,at_wall,actor,verb,target,payload_digest)
			VALUES(?,?,'2026-01-01T00:00:00Z','aira','lease.claim','',?)`,
			f.store.projectID, f.seq+500, digestBytes([]byte("lease.claim\x00"))); err != nil {
			t.Fatal(err)
		}
		if err := f.store.reconcile(ctx); err != nil {
			t.Fatalf("reconcile = %v, want nil", err)
		}
		if got := f.pendingCount(t); got != 0 {
			t.Fatalf("pending=%d, want 0 — reconcile must finalise every receipt-only intent", got)
		}
	})
	t.Run("reconcile conflicts on a third party", func(t *testing.T) {
		f := newRetireFixture(t)
		if err := f.store.reconcile(context.Background()); !errors.Is(err, ErrWriteConflict) {
			t.Fatalf("reconcile = %v, want ErrWriteConflict", err)
		}
		if got := f.pendingCount(t); got != 1 {
			t.Fatalf("pending=%d, want 1", got)
		}
	})
}

// ---------------------------------------------------------------------------
// fail-closed guards
// ---------------------------------------------------------------------------

// TestRetireRefusedWhenDigestEqualsPrecondition proves the refusal protected
// REAL work: after the refusal, reconcile replays the intent and the intended
// bytes land. A test that only asserted the error code would pass against an
// implementation that refused for the wrong reason.
func TestRetireRefusedWhenDigestEqualsPrecondition(t *testing.T) {
	f := newRetireFixture(t)
	if err := os.WriteFile(f.path, f.original, 0o644); err != nil {
		t.Fatal(err)
	}
	before := snapshotDurableState(t, f.store)
	_, err := f.store.RetireIntent(context.Background(), fmt.Sprint(f.seq))
	assertCode(t, err, "E_INTENT_REPLAYABLE")
	assertDurableStateUnchanged(t, before, snapshotDurableState(t, f.store), "a replayable refusal")

	if err := f.store.reconcile(context.Background()); err != nil {
		t.Fatalf("reconcile after the refusal = %v, want nil", err)
	}
	written, err := os.ReadFile(f.path)
	if err != nil {
		t.Fatal(err)
	}
	if string(written) != string(f.intended) {
		t.Fatalf("the refused intent did not complete; file=%q", written)
	}
}

// TestRetireRefusedWhenDigestEqualsIntended covers the other completable case:
// a third party wrote exactly the intended bytes, so reconcile will finalise it.
func TestRetireRefusedWhenDigestEqualsIntended(t *testing.T) {
	f := newRetireFixture(t)
	if err := os.WriteFile(f.path, f.intended, 0o644); err != nil {
		t.Fatal(err)
	}
	before := snapshotDurableState(t, f.store)
	_, err := f.store.RetireIntent(context.Background(), fmt.Sprint(f.seq))
	assertCode(t, err, "E_INTENT_REPLAYABLE")
	assertDurableStateUnchanged(t, before, snapshotDurableState(t, f.store), "an already-written refusal")

	if err := f.store.reconcile(context.Background()); err != nil {
		t.Fatalf("reconcile after the refusal = %v, want nil", err)
	}
	if got := f.pendingCount(t); got != 0 {
		t.Fatalf("pending=%d after reconcile, want 0", got)
	}
}

// TestRetireRefusedForReceiptOnlyIntent covers an intent with nothing to write.
// reconcile finalises such an intent unconditionally, so it is never a conflict
// and must never be retirable.
func TestRetireRefusedForReceiptOnlyIntent(t *testing.T) {
	f := newRetireFixture(t)
	ctx := context.Background()
	if _, err := f.store.db.ExecContext(ctx, `UPDATE outbox SET intended_bytes=NULL WHERE project_id=? AND seq=?`,
		f.store.projectID, f.seq); err != nil {
		t.Fatal(err)
	}
	before := snapshotDurableState(t, f.store)
	_, err := f.store.RetireIntent(ctx, fmt.Sprint(f.seq))
	assertCode(t, err, "E_INTENT_REPLAYABLE")
	assertDurableStateUnchanged(t, before, snapshotDurableState(t, f.store), "a receipt-only refusal")
}

// TestRetireRefusedForAlreadyMaterialisedRow exercises the cheap pre-check.
func TestRetireRefusedForAlreadyMaterialisedRow(t *testing.T) {
	f := newRetireFixture(t)
	ctx := context.Background()
	if _, err := f.store.db.ExecContext(ctx, `UPDATE outbox SET materialised=1 WHERE project_id=? AND seq=?`,
		f.store.projectID, f.seq); err != nil {
		t.Fatal(err)
	}
	before := snapshotDurableState(t, f.store)
	_, err := f.store.RetireIntent(ctx, fmt.Sprint(f.seq))
	assertCode(t, err, "E_INTENT_NOT_PENDING")
	assertDurableStateUnchanged(t, before, snapshotDurableState(t, f.store), "a materialised refusal")
}

// TestRetireRefusedWhenMaterialisedAfterResolve is the test the in-transaction
// re-read exists for. The cheap pre-check above passes here — the row is still
// pending when the selector resolves — and only the authoritative read inside
// the guard transaction can catch the completion that happened underneath.
//
// Without this test, deleting that re-read leaves the suite green.
func TestRetireRefusedWhenMaterialisedAfterResolve(t *testing.T) {
	f := newRetireFixture(t)
	ctx := context.Background()
	fired := 0
	f.store.afterRetireResolve = func(EventKey) {
		fired++
		if _, err := f.store.db.ExecContext(ctx, `UPDATE outbox SET materialised=1 WHERE project_id=? AND seq=?`,
			f.store.projectID, f.seq); err != nil {
			t.Error(err)
		}
	}
	_, err := f.store.RetireIntent(ctx, fmt.Sprint(f.seq))
	assertCode(t, err, "E_INTENT_NOT_PENDING")
	if fired != 1 {
		t.Fatalf("afterRetireResolve fired %d times, want 1 — the seam did not run", fired)
	}
	var rows int
	if err := f.store.db.QueryRowContext(ctx, `SELECT count(*) FROM outbox WHERE project_id=? AND seq=?`,
		f.store.projectID, f.seq).Scan(&rows); err != nil {
		t.Fatal(err)
	}
	if rows != 1 {
		t.Fatalf("the completed intent was deleted anyway (rows=%d)", rows)
	}
}

// TestRetireRefusedForForeignWorktreeIntent uses a BARE SEQUENCE selector on
// purpose. A path selector already filters by worktree in its own query, so it
// would pass even with the guard removed; only the seq form reaches the guard.
func TestRetireRefusedForForeignWorktreeIntent(t *testing.T) {
	f := newRetireFixture(t)
	ctx := context.Background()
	if _, err := f.store.db.ExecContext(ctx, `UPDATE outbox SET worktree_id='sibling' WHERE project_id=? AND seq=?`,
		f.store.projectID, f.seq); err != nil {
		t.Fatal(err)
	}
	before := snapshotDurableState(t, f.store)
	_, err := f.store.RetireIntent(ctx, fmt.Sprint(f.seq))
	assertCode(t, err, "E_SELECTOR_INVALID")
	assertDurableStateUnchanged(t, before, snapshotDurableState(t, f.store), "a foreign-worktree refusal")
}

// TestRetireReportsUnevaluatedDigest pins the honesty rule: when the on-disk
// state cannot be read, the intent's disposition is NOT established, so the
// answer is an unevaluated U_ code — never "conflicted" (a fake fail that would
// authorise a destructive retire) and never a success.
func TestRetireReportsUnevaluatedDigest(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores directory permissions, so the unreadable path cannot be constructed")
	}
	f := newRetireFixture(t)
	dir := filepath.Dir(f.path)
	if err := os.Chmod(dir, 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o755) })

	_, err := f.store.RetireIntent(context.Background(), fmt.Sprint(f.seq))
	assertCode(t, err, "U_INTENT_UNEVALUATED")

	if err := os.Chmod(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if got := f.pendingCount(t); got != 1 {
		t.Fatalf("pending=%d after an unevaluated read, want 1", got)
	}
}

// TestRetireRefusesBlankSelector keeps an empty selector out of path
// resolution, where it would otherwise become the worktree root.
func TestRetireRefusesBlankSelector(t *testing.T) {
	f := newRetireFixture(t)
	for _, selector := range []string{"", "   ", "\t"} {
		_, err := f.store.RetireIntent(context.Background(), selector)
		assertCode(t, err, "E_SELECTOR_INVALID")
	}
}

// TestRetireRefusesAMalformedAllocationID guards prefixOf/numberOf, which slice
// on the last '-' and PANIC without one.
func TestRetireRefusesAMalformedAllocationID(t *testing.T) {
	f := newRetireFixture(t)
	ctx := context.Background()
	if _, err := f.store.db.ExecContext(ctx, `UPDATE outbox SET allocation_id='nodash' WHERE project_id=? AND seq=?`,
		f.store.projectID, f.seq); err != nil {
		t.Fatal(err)
	}
	_, err := f.store.RetireIntent(ctx, fmt.Sprint(f.seq))
	assertCode(t, err, "E_JOURNAL_CORRUPT")
	if got := f.pendingCount(t); got != 1 {
		t.Fatalf("pending=%d, want 1", got)
	}
}

// ---------------------------------------------------------------------------
// concurrency
// ---------------------------------------------------------------------------

// TestRetireCannotRaceAConcurrentMaterialise is the reason retire takes the
// physical path lock.
//
// materialiseIntent holds that lock from before its digest read until after it
// marks the row materialised, and takes the database only at the end — so a
// BEGIN IMMEDIATE transaction alone does NOT exclude a writer that is
// mid-materialisation. Without the path lock, a retire could read the digest a
// moment before the writer's rename, call it a conflict, and delete a row whose
// file was about to be written: the file write stands and the intent is gone.
//
// The contender is a direct materialiseIntent, NOT reconcile: reconcile holds
// the finding-mutation lock for its whole pass, so a retire would block on THAT
// even with the path lock removed, and the test would prove nothing.
func TestRetireCannotRaceAConcurrentMaterialise(t *testing.T) {
	base := t.TempDir()
	root := filepath.Join(base, "main")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	common, state := filepath.Join(base, "common"), filepath.Join(base, "state")
	writer := testStore(t, root, common, state)
	retirer := openTestStore(t, root, common, state, filepath.Base(root), "AIRA")
	ctx := context.Background()

	ticket, err := writer.CreateTicket(ctx, domain.CreateTicketInput{
		Title: "raced", Kind: domain.KindFeature, Severity: domain.SeverityP2,
	})
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, ".aira", "tickets", ticket.ID+".md")

	inside := make(chan struct{})
	release := make(chan struct{})
	writer.beforeMaterialise = func(Intent) error {
		close(inside)
		<-release
		return nil
	}

	var writerErr error
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		_, writerErr = writer.UpdateTicketContent(ctx, ticket.ID, func(tk domain.Ticket, body string) (domain.Ticket, string, error) {
			tk.Title = "written by the racer"
			return tk, body, nil
		})
	}()

	// The writer is now parked inside materialiseIntent holding the path lock,
	// with the file still at its precondition.
	<-inside

	retireDone := make(chan error, 1)
	retireStarted := make(chan struct{})
	retirer.afterRetireResolve = func(EventKey) { close(retireStarted) }
	go func() {
		_, err := retirer.RetireIntent(ctx, path)
		retireDone <- err
	}()
	// Release the writer only once the retire is demonstrably past its advisory
	// resolve and about to take the path lock. Without this ordering a
	// scheduler delay could let the retire finish first and the missing-lock
	// mutant would survive.
	<-retireStarted
	select {
	case err := <-retireDone:
		t.Fatalf("retire completed while a materialise held the path lock: %v", err)
	default:
	}
	close(release)
	wg.Wait()
	if writerErr != nil {
		t.Fatalf("the racing materialise failed: %v", writerErr)
	}

	if err := receiveWithin(t, retireDone, "retire"); ErrorCode(err) != "E_INTENT_NOT_PENDING" {
		t.Fatalf("retire = %v (code %s), want E_INTENT_NOT_PENDING", err, ErrorCode(err))
	}
	written, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(written), "written by the racer") {
		t.Fatalf("the racing write was lost; file=%q", written)
	}
}

// ---------------------------------------------------------------------------
// idempotence and selector forms
// ---------------------------------------------------------------------------

// TestRetireIsIdempotentWithAStableNotFoundCode pins that a second retire says
// so honestly instead of reporting a second success it did not perform.
func TestRetireIsIdempotentWithAStableNotFoundCode(t *testing.T) {
	f := newRetireFixture(t)
	ctx := context.Background()
	if _, err := f.store.RetireIntent(ctx, fmt.Sprint(f.seq)); err != nil {
		t.Fatalf("first retire: %v", err)
	}
	_, err := f.store.RetireIntent(ctx, fmt.Sprint(f.seq))
	assertCode(t, err, "E_NOT_FOUND")
	after := snapshotDurableState(t, f.store)
	_, err = f.store.RetireIntent(ctx, fmt.Sprint(f.seq))
	assertCode(t, err, "E_NOT_FOUND")
	assertDurableStateUnchanged(t, after, snapshotDurableState(t, f.store), "a repeated not-found retire")
}

// TestRetireSelectorForms uses an INDEPENDENT fixture per form: one intent
// cannot be retired four times, so a single-fixture version would only ever
// test the first form.
func TestRetireSelectorForms(t *testing.T) {
	for _, tc := range []struct {
		name     string
		selector func(retireFixture) string
	}{
		{"reconciliation finding key", func(f retireFixture) string { return f.findingKey() }},
		{"bare outbox sequence", func(f retireFixture) string { return fmt.Sprint(f.seq) }},
		{"absolute path", func(f retireFixture) string { return f.path }},
		{"root-relative path", func(f retireFixture) string {
			relative, err := filepath.Rel(f.root, f.path)
			if err != nil {
				t.Fatal(err)
			}
			return relative
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newRetireFixture(t)
			result, err := f.store.RetireIntent(context.Background(), tc.selector(f))
			if err != nil {
				t.Fatalf("retire by %s: %v", tc.name, err)
			}
			if result.Seq != f.seq {
				t.Fatalf("retired seq=%d, want %d", result.Seq, f.seq)
			}
			if got := f.pendingCount(t); got != 0 {
				t.Fatalf("pending=%d, want 0", got)
			}
		})
	}

	t.Run("a finding key for another worktree is refused", func(t *testing.T) {
		f := newRetireFixture(t)
		_, err := f.store.RetireIntent(context.Background(), fmt.Sprintf("reconcile:sibling:%d", f.seq))
		assertCode(t, err, "E_SELECTOR_INVALID")
		if got := f.pendingCount(t); got != 1 {
			t.Fatalf("pending=%d, want 1", got)
		}
	})
}

// TestRetireAmbiguousPathSelectorIsRefused seeds the state the partial unique
// index normally makes unreachable. AIRA refuses an ambiguous selector rather
// than picking one, so if that index is ever lost this must fail closed instead
// of retiring an arbitrary row.
func TestRetireAmbiguousPathSelectorIsRefused(t *testing.T) {
	f := newRetireFixture(t)
	ctx := context.Background()
	if _, err := f.store.db.ExecContext(ctx, `DROP INDEX unresolved_path_intent`); err != nil {
		t.Fatal(err)
	}
	if _, err := f.store.db.ExecContext(ctx, `INSERT INTO outbox(project_id,seq,worktree_id,path,verb,precondition_digest,intended_digest,intended_bytes)
		VALUES(?,?,?,?,'ticket.update','before','after',?)`,
		f.store.projectID, f.seq+1000, f.store.worktreeID, f.path, []byte("duplicate")); err != nil {
		t.Fatal(err)
	}
	before := snapshotDurableState(t, f.store)
	_, err := f.store.RetireIntent(ctx, f.path)
	assertCode(t, err, "E_SELECTOR_AMBIGUOUS")
	assertDurableStateUnchanged(t, before, snapshotDurableState(t, f.store), "an ambiguous refusal")
}

// ---------------------------------------------------------------------------
// allocation-bearing intents
// ---------------------------------------------------------------------------

// allocationFixture is a conflicted intent that carries an allocation, built the
// way the crash matrix describes: the intent commits, and the path is taken by
// a third party before the file is written.
type allocationFixture struct {
	store  *Store
	root   string
	base   string
	common string
	state  string
	intent Intent
}

// newAllocationFixture builds the conflict from prepareCreate DIRECTLY, and
// optionally skips the receipt append.
//
// The direct call is load-bearing: CreateTicketWithEvent appends the receipt
// BEFORE materialiseIntent, so the beforeMaterialise seam is already too late
// to construct a pre-receipt crash. A test that used that seam would leave the
// missing-receipt repair untested and its mutant green.
func newAllocationFixture(t *testing.T, withReceipt bool) allocationFixture {
	t.Helper()
	base := t.TempDir()
	root := filepath.Join(base, "main")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	common, state := filepath.Join(base, "common"), filepath.Join(base, "state")
	s := testStore(t, root, common, state)
	ctx := context.Background()

	intent, err := s.prepareCreate(ctx, domain.CreateTicketInput{
		Title: "allocation conflict", Kind: domain.KindFeature, Severity: domain.SeverityP2,
	})
	if err != nil {
		t.Fatalf("prepare create: %v", err)
	}
	if withReceipt {
		if err := s.appendReceiptIfMissing(intent.Receipt); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.MkdirAll(filepath.Dir(intent.Path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(intent.Path, []byte("--- not a ticket, written by a third party ---\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return allocationFixture{store: s, root: root, base: base, common: common, state: state, intent: intent}
}

func allocationState(t *testing.T, s *Store, id string) string {
	t.Helper()
	prefix, number := splitTicketID(id)
	var state string
	err := s.db.QueryRowContext(context.Background(),
		`SELECT state FROM allocations WHERE project_id=? AND prefix=? AND number=?`, s.projectID, prefix, number).Scan(&state)
	if err != nil {
		t.Fatalf("read allocation state for %s: %v", id, err)
	}
	return state
}

// TestRetiringAnAllocationIntentClearsTheCheckWedge is the C4 proof. The
// BEFORE half is what stops it being vacuous: without it, an implementation
// that never touched allocations at all would still "pass" the after-check on a
// project whose dimension was never failing.
//
// The assertion is on the allocated-id-file DIMENSION rather than on a globally
// clean check: the third party's file is still on disk after the retire (retire
// never touches the working tree) and legitimately raises its own scan finding.
func TestRetiringAnAllocationIntentClearsTheCheckWedge(t *testing.T) {
	f := newAllocationFixture(t, true)
	ctx := context.Background()

	before, err := f.store.Check(ctx)
	if err != nil {
		t.Fatalf("check before: %v", err)
	}
	if before.Dimensions["allocated-id-file"] != "fail" {
		t.Fatalf("allocated-id-file before retire = %q, want fail — the wedge this test claims to close is not present",
			before.Dimensions["allocated-id-file"])
	}

	result, err := f.store.RetireIntent(ctx, fmt.Sprint(f.intent.Seq))
	if err != nil {
		t.Fatalf("retire: %v", err)
	}
	if result.AllocationID != f.intent.AllocationID {
		t.Fatalf("retire reported allocation %q, want %q", result.AllocationID, f.intent.AllocationID)
	}
	if got := allocationState(t, f.store, f.intent.AllocationID); got != "retired" {
		t.Fatalf("allocation state = %q, want retired", got)
	}

	after, err := f.store.Check(ctx)
	if err != nil {
		t.Fatalf("check after: %v", err)
	}
	if after.Dimensions["allocated-id-file"] == "fail" {
		t.Fatalf("allocated-id-file still fails after retire: %+v", after.Findings)
	}
}

// TestRetireCannotRetireAMaterialisedAllocation covers the live half of the
// allocated -> retired narrowing: a retire must never be able to silence a
// materialised allocation's integrity finding.
func TestRetireCannotRetireAMaterialisedAllocation(t *testing.T) {
	f := newAllocationFixture(t, true)
	ctx := context.Background()
	prefix, number := splitTicketID(f.intent.AllocationID)
	if _, err := f.store.db.ExecContext(ctx, `UPDATE allocations SET state='materialised'
		WHERE project_id=? AND prefix=? AND number=?`, f.store.projectID, prefix, number); err != nil {
		t.Fatal(err)
	}
	_, err := f.store.RetireIntent(ctx, fmt.Sprint(f.intent.Seq))
	assertCode(t, err, "E_INTERNAL")
	// The whole transaction must have rolled back, outbox row included.
	var pending int
	if err := f.store.db.QueryRowContext(ctx, `SELECT count(*) FROM outbox WHERE project_id=? AND seq=? AND materialised=0`,
		f.store.projectID, f.intent.Seq).Scan(&pending); err != nil {
		t.Fatal(err)
	}
	if pending != 1 {
		t.Fatalf("the refused retire still deleted the intent (pending=%d)", pending)
	}
	if got := allocationState(t, f.store, f.intent.AllocationID); got != "materialised" {
		t.Fatalf("allocation state = %q, want materialised", got)
	}
}

// TestRetireRepairsAMissingAllocationReceiptAndTheIDIsNeverReused is C5.
//
// receipts.jsonl is the durable ID high-water source Rebuild reads after a
// database loss. If a create crashed before its receipt append, the pending
// outbox row is the ONLY thing that would ever have caused the receipt to be
// written — so deleting that row without repairing the receipt forgets the ID
// entirely and lets it be re-minted.
//
// A counter-only assertion would pass against a no-op retire, so this drives the
// whole path: missing receipt -> retire -> database loss -> rebuild -> allocate.
func TestRetireRepairsAMissingAllocationReceiptAndTheIDIsNeverReused(t *testing.T) {
	f := newAllocationFixture(t, false)
	ctx := context.Background()

	if receipts := readReceiptIDs(t, f.store); contains(receipts, f.intent.AllocationID) {
		t.Fatalf("the fixture already wrote a receipt for %s; the missing-receipt case is not being tested", f.intent.AllocationID)
	}
	if _, err := f.store.RetireIntent(ctx, fmt.Sprint(f.intent.Seq)); err != nil {
		t.Fatalf("retire: %v", err)
	}
	if receipts := readReceiptIDs(t, f.store); !contains(receipts, f.intent.AllocationID) {
		t.Fatalf("retire did not repair the missing receipt; receipts=%v", receipts)
	}

	rebuilt := reopenAfterDatabaseLoss(t, f.root, f.common, f.state, "AIRA")
	if err := rebuilt.Rebuild(ctx); err != nil {
		t.Fatalf("rebuild after database loss: %v", err)
	}
	if got := allocationState(t, rebuilt, f.intent.AllocationID); got != "retired" {
		t.Fatalf("allocation state after rebuild = %q, want retired — the journal replay did not carry the retirement", got)
	}
	next, err := rebuilt.AllocateID(ctx, "AIRA")
	if err != nil {
		t.Fatalf("allocate after rebuild: %v", err)
	}
	_, retiredNumber := splitTicketID(f.intent.AllocationID)
	_, nextNumber := splitTicketID(next)
	if nextNumber <= retiredNumber {
		t.Fatalf("retired %s but reallocated %s — a retired ID must never be re-minted", f.intent.AllocationID, next)
	}
}

// TestRebuildResurrectsARetiredIntentOnlyAsMaterialised pins what Rebuild
// actually does to a retired allocation's outbox row.
//
// ensureAllocationEvent ends with an INSERT OR IGNORE that re-creates the row
// from the receipt, so the deleted row DOES come back — with materialised=1.
// That is benign, because the busy probe, the eject guard and the partial unique
// index all key on materialised=0. It is asserted rather than assumed, and the
// row count is asserted too so this cannot pass vacuously on zero rows.
func TestRebuildResurrectsARetiredIntentOnlyAsMaterialised(t *testing.T) {
	f := newAllocationFixture(t, true)
	ctx := context.Background()
	if _, err := f.store.RetireIntent(ctx, fmt.Sprint(f.intent.Seq)); err != nil {
		t.Fatalf("retire: %v", err)
	}
	if err := f.store.Rebuild(ctx); err != nil {
		t.Fatalf("rebuild: %v", err)
	}

	var rows, materialised int
	if err := f.store.db.QueryRowContext(ctx,
		`SELECT count(*), coalesce(sum(materialised),0) FROM outbox WHERE project_id=? AND seq=?`,
		f.store.projectID, f.intent.Seq).Scan(&rows, &materialised); err != nil {
		t.Fatal(err)
	}
	if rows != 1 || materialised != 1 {
		t.Fatalf("rebuild reconstructed rows=%d materialised=%d for the retired intent, want exactly 1 materialised row",
			rows, materialised)
	}
	var pending int
	if err := f.store.db.QueryRowContext(ctx, `SELECT count(*) FROM outbox WHERE project_id=? AND materialised=0`,
		f.store.projectID).Scan(&pending); err != nil {
		t.Fatal(err)
	}
	if pending != 0 {
		t.Fatalf("eject's durability guard counts %d pending intents after rebuild, want 0", pending)
	}
	// And the path is still free for a new writer.
	if _, err := f.store.preparePathMutation(ctx, f.intent.Path, digestBytes([]byte("x")), []byte("y"), "ticket.update"); err != nil {
		t.Fatalf("the path is still busy after rebuild: %v", err)
	}
}

// TestRebuildSkipsAMalformedRetireTarget: a retire of a plain path mutation
// targets a path, not an ID, and must be skipped rather than treated as an
// error that fails the whole rebuild.
func TestRebuildSkipsAMalformedRetireTarget(t *testing.T) {
	f := newRetireFixture(t)
	ctx := context.Background()
	if _, err := f.store.RetireIntent(ctx, fmt.Sprint(f.seq)); err != nil {
		t.Fatalf("retire: %v", err)
	}
	journal, err := os.ReadFile(filepath.Join(f.store.auditDir, "journal.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(journal), "intent.retire") {
		t.Fatal("the retire event never reached the journal")
	}
	if err := f.store.Rebuild(ctx); err != nil {
		t.Fatalf("rebuild with a path-targeted retire event: %v", err)
	}
}

// TestRebuildRetireReplayCannotRetireAMaterialisedAllocation is the rebuild half
// of the allocated -> retired narrowing. A forged journal frame naming a live
// allocation must not be able to flip it and hide a genuine E_ID_UNRESOLVED.
func TestRebuildRetireReplayCannotRetireAMaterialisedAllocation(t *testing.T) {
	base := t.TempDir()
	root := filepath.Join(base, "main")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	s := testStore(t, root, filepath.Join(base, "common"), filepath.Join(base, "state"))
	ctx := context.Background()
	ticket, err := s.CreateTicket(ctx, domain.CreateTicketInput{
		Title: "live", Kind: domain.KindFeature, Severity: domain.SeverityP2,
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := allocationState(t, s, ticket.ID); got != "materialised" {
		t.Fatalf("allocation state = %q, want materialised", got)
	}

	// A forged frame: well-formed digest, naming a materialised allocation.
	forged := eventRecord{ProjectID: s.projectID, Seq: 90001, At: "2026-01-01T00:00:00Z", Actor: "aira",
		Verb: "intent.retire", Target: ticket.ID, PayloadDigest: digestBytes([]byte("intent.retire\x00" + ticket.ID))}
	if err := appendEventIfMissing(filepath.Join(s.auditDir, "journal.jsonl"), forged,
		filepath.Join(s.auditDir, "journal.lock")); err != nil {
		t.Fatal(err)
	}
	if err := s.Rebuild(ctx); err != nil {
		t.Fatalf("rebuild: %v", err)
	}
	if got := allocationState(t, s, ticket.ID); got != "materialised" {
		t.Fatalf("a forged retire frame flipped a materialised allocation to %q", got)
	}
}

// ---------------------------------------------------------------------------
// requirement-kind allocations
// ---------------------------------------------------------------------------

func requirementStore(t *testing.T, root, common, state string) *Store {
	t.Helper()
	s, err := Open(context.Background(), Options{
		Root: root, CommonDir: common, DBPath: filepath.Join(state, "state.db"),
		RegistryPath: filepath.Join(state, "registry.jsonl"), ProjectID: "project-aira",
		WorktreeID: filepath.Base(root), ProjectSlug: "aira",
		Prefixes: []string{"AIRA"}, RequirementPrefixes: []string{"AR"},
	})
	if err != nil {
		t.Fatalf("open requirement store: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

// TestRetireWritesTheRequirementKindWithoutReconcile is the test that pins
// RETIRE'S OWN receipt kind.
//
// It deliberately never runs reconcile: reconcile would append the receipt
// first, and appendReceiptIfMissing dedupes on (project, id, seq) without
// looking at Kind, so retire's receipt would be discarded and an
// implementation that hardcoded "ticket" would still pass. Here the receipt on
// disk is the one retire wrote, and a wrong kind fails the rebuild below with
// E_JOURNAL_CORRUPT.
func TestRetireWritesTheRequirementKindWithoutReconcile(t *testing.T) {
	base := t.TempDir()
	root := filepath.Join(base, "main")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	common, state := filepath.Join(base, "common"), filepath.Join(base, "state")
	s := requirementStore(t, root, common, state)
	ctx := context.Background()

	intent, _, err := s.prepareCreateRequirement(ctx, domain.RequirementInput{
		Text: "The retire path must carry the requirement kind.", Status: domain.RequirementPlanned,
	})
	if err != nil {
		t.Fatalf("prepare requirement: %v", err)
	}
	if err := os.MkdirAll(filepath.Dir(intent.Path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(intent.Path, []byte("--- third party ---\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := s.RetireIntent(ctx, fmt.Sprint(intent.Seq)); err != nil {
		t.Fatalf("retire the requirement intent: %v", err)
	}
	if got := allocationState(t, s, intent.AllocationID); got != "retired" {
		t.Fatalf("requirement allocation state = %q, want retired", got)
	}

	rebuilt := reopenRequirementStoreAfterDatabaseLoss(t, root, common, state)
	if err := rebuilt.Rebuild(ctx); err != nil {
		t.Fatalf("rebuild after a requirement retire: %v — the repaired receipt does not carry the requirement kind", err)
	}
	if got := allocationState(t, rebuilt, intent.AllocationID); got != "retired" {
		t.Fatalf("requirement allocation state after rebuild = %q, want retired", got)
	}
}

// TestReconcileRepairsARequirementReceiptWithTheRightKind covers the OTHER
// producer of a repaired receipt.
//
// reconcile omitted Kind entirely, and normaliseKind("") is "ticket", so a
// repaired requirement receipt claimed ticket-kind while its path sat under
// .aira/requirements/ — and the NEXT Rebuild failed wholesale with
// "E_JOURNAL_CORRUPT: allocation kind ticket disagrees with path ...". That is
// on the guaranteed path into a retire, because an operator only learns an
// intent is conflicted because reconcile said so.
func TestReconcileRepairsARequirementReceiptWithTheRightKind(t *testing.T) {
	base := t.TempDir()
	root := filepath.Join(base, "main")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	common, state := filepath.Join(base, "common"), filepath.Join(base, "state")
	s := requirementStore(t, root, common, state)
	ctx := context.Background()

	intent, _, err := s.prepareCreateRequirement(ctx, domain.RequirementInput{
		Text: "Reconcile must repair a requirement receipt with its own kind.", Status: domain.RequirementPlanned,
	})
	if err != nil {
		t.Fatalf("prepare requirement: %v", err)
	}
	if err := os.MkdirAll(filepath.Dir(intent.Path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(intent.Path, []byte("--- third party ---\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// reconcile writes the receipt, then records the conflict finding.
	if err := s.reconcile(ctx); !errors.Is(err, ErrWriteConflict) {
		t.Fatalf("reconcile = %v, want ErrWriteConflict", err)
	}
	if _, err := s.RetireIntent(ctx, fmt.Sprint(intent.Seq)); err != nil {
		t.Fatalf("retire after reconcile: %v", err)
	}

	rebuilt := reopenRequirementStoreAfterDatabaseLoss(t, root, common, state)
	if err := rebuilt.Rebuild(ctx); err != nil {
		t.Fatalf("rebuild after reconcile+retire: %v — reconcile's repaired receipt lost the requirement kind", err)
	}
	if got := allocationState(t, rebuilt, intent.AllocationID); got != "retired" {
		t.Fatalf("requirement allocation state after rebuild = %q, want retired", got)
	}
}

// ---------------------------------------------------------------------------
// findings
// ---------------------------------------------------------------------------

// TestRetireClearsTheReconciliationFinding: an operator reads the wedge through
// this finding, so leaving it behind reports a conflict that no longer exists.
// The unrelated finding seeded alongside proves the delete is KEYED and not a
// sweep of every reconciliation finding in the project.
func TestRetireClearsTheReconciliationFinding(t *testing.T) {
	f := newRetireFixture(t)
	ctx := context.Background()
	if err := f.store.reconcile(ctx); !errors.Is(err, ErrWriteConflict) {
		t.Fatalf("reconcile = %v, want ErrWriteConflict", err)
	}
	if err := f.store.withImmediate(ctx, func(conn *sql.Conn) error {
		return upsertReconciliationFinding(ctx, conn, f.store.projectID, f.store.worktreeID,
			"scan:unrelated:keepme", "E_GIT_SCAN", "unrelated", "must survive a retire")
	}); err != nil {
		t.Fatal(err)
	}
	if !hasFindingKey(t, f.store, f.findingKey()) {
		t.Fatalf("reconcile did not record %s", f.findingKey())
	}
	if _, err := f.store.RetireIntent(ctx, fmt.Sprint(f.seq)); err != nil {
		t.Fatalf("retire: %v", err)
	}
	if hasFindingKey(t, f.store, f.findingKey()) {
		t.Fatalf("%s survived the retire", f.findingKey())
	}
	if !hasFindingKey(t, f.store, "scan:unrelated:keepme") {
		t.Fatal("the retire swept an unrelated reconciliation finding")
	}
}

// TestRetireHoldsTheFindingLockAgainstAConcurrentReconcile: without the
// finding-mutation lock, a reconcile that is about to record the conflict can
// re-create the finding immediately after a retire deleted it, leaving the
// operator staring at a wedge that no longer exists.
func TestRetireHoldsTheFindingLockAgainstAConcurrentReconcile(t *testing.T) {
	base := t.TempDir()
	root := filepath.Join(base, "main")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	common, state := filepath.Join(base, "common"), filepath.Join(base, "state")
	reconciler := testStore(t, root, common, state)
	retirer := openTestStore(t, root, common, state, filepath.Base(root), "AIRA")
	ctx := context.Background()

	ticket, err := reconciler.CreateTicket(ctx, domain.CreateTicketInput{
		Title: "locked", Kind: domain.KindFeature, Severity: domain.SeverityP2,
	})
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, ".aira", "tickets", ticket.ID+".md")
	original, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	parsed, body, err := domain.ParseTicket(original)
	if err != nil {
		t.Fatal(err)
	}
	parsed.Title = "never written"
	intended, err := domain.RenderTicket(parsed, body)
	if err != nil {
		t.Fatal(err)
	}
	intent, err := reconciler.preparePathMutation(ctx, path, digestBytes(original), intended, "ticket.update")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("--- third party ---\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	inside := make(chan struct{})
	release := make(chan struct{})
	reconciler.beforeRecordConflictFinding = func() {
		close(inside)
		<-release
	}
	reconcileDone := make(chan error, 1)
	go func() { reconcileDone <- reconciler.reconcile(ctx) }()
	<-inside

	retireDone := make(chan error, 1)
	retireStarted := make(chan struct{})
	retirer.afterRetireResolve = func(EventKey) { close(retireStarted) }
	go func() {
		_, err := retirer.RetireIntent(ctx, fmt.Sprint(intent.Seq))
		retireDone <- err
	}()
	// Without this handshake the 2s window below could elapse before the
	// goroutine had even resolved its selector, and a missing lock would go
	// unnoticed on a loaded scheduler.
	select {
	case <-retireStarted:
	case <-time.After(30 * time.Second):
		t.Fatal("the retire goroutine never reached its resolve seam")
	}

	// The load-bearing assertion is the FINAL state, not the timing: while the
	// lock is held the retire cannot run, so it deletes the finding this
	// reconcile pass wrote. Without the lock the retire runs here — before the
	// finding exists — deletes nothing, and reconcile then leaves a stale
	// conflict finding for an intent that no longer exists.
	//
	// The bounded wait is only how a missing lock is given its chance to show.
	// Its flake direction is safe: a slow machine lets the wait expire and the
	// test pass; it can never turn correct code red.
	select {
	case err := <-retireDone:
		t.Fatalf("retire completed while reconcile held the finding lock: %v", err)
	case <-time.After(2 * time.Second):
	}
	close(release)
	if err := receiveWithin(t, reconcileDone, "reconcile"); !errors.Is(err, ErrWriteConflict) {
		t.Fatalf("reconcile = %v, want ErrWriteConflict", err)
	}
	if err := receiveWithin(t, retireDone, "retire"); err != nil {
		t.Fatalf("retire after the reconcile finished: %v", err)
	}
	if hasFindingKey(t, retirer, reconcileFindingKey(retirer.worktreeID, intent.Seq)) {
		t.Fatal("a stale reconciliation finding survived the retire")
	}
	if got := retirerPendingCount(t, retirer); got != 0 {
		t.Fatalf("pending=%d after the retire, want 0", got)
	}
}

func retirerPendingCount(t *testing.T, s *Store) int {
	t.Helper()
	var pending int
	if err := s.db.QueryRowContext(context.Background(),
		`SELECT count(*) FROM outbox WHERE project_id=? AND materialised=0`, s.projectID).Scan(&pending); err != nil {
		t.Fatal(err)
	}
	return pending
}

// TestRetireCrashOnAnAllocationIntentDoesNotPoisonTheReceipts is the test the
// parked retire row's EMPTY allocation_id exists for.
//
// reconcile's receipt-repair branch uses a row's own path and seq. The retire
// event's row has an empty path, so if it carried the allocation id, a crash
// before the journal append followed by a reconcile would append a receipt with
// an EMPTY path — and the next Rebuild would fail wholesale on
// reconcileAllocationKind's "path outside the entity directories". Retiring a
// project would therefore make it unrebuildable.
//
// Reaching that needs all three: an allocation-bearing intent, a crash that
// leaves the retire row unjournaled, and a reconcile pass before the rebuild.
func TestRetireCrashOnAnAllocationIntentDoesNotPoisonTheReceipts(t *testing.T) {
	f := newAllocationFixture(t, true)
	ctx := context.Background()
	f.store.afterRetireJournalCommit = func() error { return errors.New("E_RECEIPT_IO: simulated crash") }
	if _, err := f.store.RetireIntent(ctx, fmt.Sprint(f.intent.Seq)); err == nil {
		t.Fatal("the injected crash did not surface")
	}
	f.store.afterRetireJournalCommit = nil

	// reconcile drives the parked retire row, including its receipt-repair
	// branch if the row wrongly carries the allocation id.
	if err := f.store.reconcile(ctx); err != nil {
		t.Fatalf("reconcile after the crash = %v, want nil", err)
	}
	receipts, err := readReceipts(filepath.Join(f.store.auditDir, "receipts.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	for _, receipt := range receipts {
		if receipt.Path == "" {
			t.Fatalf("a receipt with an empty path was written: %+v — the retire row carried the allocation id", receipt)
		}
	}
	if err := f.store.Rebuild(ctx); err != nil {
		t.Fatalf("rebuild after a crashed allocation retire: %v", err)
	}
}

// ---------------------------------------------------------------------------
// crash recovery
// ---------------------------------------------------------------------------

// TestRetireJournalCrashIsReplayed proves the parked retire-event outbox row is
// what closes the window between the guard transaction's commit and the journal
// append — through the EXISTING recovery paths, with no new machinery.
func TestRetireJournalCrashIsReplayed(t *testing.T) {
	for _, tc := range []struct {
		name  string
		drive func(*testing.T, *Store) error
	}{
		{"reconcile", func(t *testing.T, s *Store) error { return s.reconcile(context.Background()) }},
		{"replayUnjournaledEvents", func(t *testing.T, s *Store) error {
			return s.replayUnjournaledEvents(context.Background())
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newRetireFixture(t)
			ctx := context.Background()
			f.store.afterRetireJournalCommit = func() error { return errors.New("E_RECEIPT_IO: simulated crash") }
			_, err := f.store.RetireIntent(ctx, fmt.Sprint(f.seq))
			if err == nil {
				t.Fatal("the injected crash did not surface")
			}
			f.store.afterRetireJournalCommit = nil

			// The delete is durable, but the journal has not seen the event.
			if got := f.pendingCount(t); got != 0 {
				t.Fatalf("pending=%d after the crash, want 0 — the delete must have committed", got)
			}
			journal, readErr := os.ReadFile(filepath.Join(f.store.auditDir, "journal.jsonl"))
			if readErr != nil && !errors.Is(readErr, os.ErrNotExist) {
				t.Fatal(readErr)
			}
			if strings.Contains(string(journal), "intent.retire") {
				t.Fatal("the crash was injected too late: the journal already has the retire event")
			}
			var unjournaled int
			if err := f.store.db.QueryRowContext(ctx,
				`SELECT count(*) FROM outbox WHERE project_id=? AND verb='intent.retire' AND materialised=1 AND journaled=0`,
				f.store.projectID).Scan(&unjournaled); err != nil {
				t.Fatal(err)
			}
			if unjournaled != 1 {
				t.Fatalf("the retire event parked %d replayable rows, want 1", unjournaled)
			}

			if err := tc.drive(t, f.store); err != nil {
				t.Fatalf("%s: %v", tc.name, err)
			}
			journal, err = os.ReadFile(filepath.Join(f.store.auditDir, "journal.jsonl"))
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(string(journal), "intent.retire") {
				t.Fatalf("%s did not append the retire event; journal=%q", tc.name, journal)
			}
			if err := f.store.db.QueryRowContext(ctx,
				`SELECT count(*) FROM outbox WHERE project_id=? AND verb='intent.retire' AND journaled=0`,
				f.store.projectID).Scan(&unjournaled); err != nil {
				t.Fatal(err)
			}
			if unjournaled != 0 {
				t.Fatalf("%s left %d unjournaled retire rows", tc.name, unjournaled)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// shared helpers
// ---------------------------------------------------------------------------

func hasFindingKey(t *testing.T, s *Store, key string) bool {
	t.Helper()
	var count int
	if err := s.db.QueryRowContext(context.Background(),
		`SELECT count(*) FROM findings WHERE project_id=? AND finding_key=?`, s.projectID, key).Scan(&count); err != nil {
		t.Fatal(err)
	}
	return count > 0
}

func readReceiptIDs(t *testing.T, s *Store) []string {
	t.Helper()
	receipts, err := readReceipts(filepath.Join(s.auditDir, "receipts.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	ids := make([]string, 0, len(receipts))
	for _, receipt := range receipts {
		ids = append(ids, receipt.ID)
	}
	return ids
}

func contains(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

// reopenAfterDatabaseLoss deletes the state database and opens a fresh store on
// the same working tree and common directory, which is the disaster-recovery
// path Rebuild exists for.
func reopenAfterDatabaseLoss(t *testing.T, root, common, state string, prefixes ...string) *Store {
	t.Helper()
	removeDatabase(t, state)
	return openTestStore(t, root, common, state, filepath.Base(root), prefixes...)
}

func reopenRequirementStoreAfterDatabaseLoss(t *testing.T, root, common, state string) *Store {
	t.Helper()
	removeDatabase(t, state)
	return requirementStore(t, root, common, state)
}

func removeDatabase(t *testing.T, state string) {
	t.Helper()
	for _, suffix := range []string{"", "-wal", "-shm"} {
		if err := os.Remove(filepath.Join(state, "state.db"+suffix)); err != nil && !errors.Is(err, os.ErrNotExist) {
			t.Fatal(err)
		}
	}
}

// receiveWithin bounds a channel receive so a regression that deadlocks fails
// with a named error instead of hanging the package until the go test timeout.
func receiveWithin(t *testing.T, ch <-chan error, what string) error {
	t.Helper()
	select {
	case err := <-ch:
		return err
	case <-time.After(60 * time.Second):
		t.Fatalf("%s never completed", what)
		return nil
	}
}

// TestRetiringAnAllocationWhoseFileValidlyClaimsTheIDIsAnAcceptedGap pins a
// residue the AIRA-73 build review found, so it is executable rather than prose.
//
// If the third party who took the path wrote a file that is itself a VALID
// entity claiming the very ID being allocated, the retire still succeeds and
// records `allocations.state='retired'` — even though that ID now demonstrably
// resolves to a live ticket. The record is stale.
//
// It is accepted rather than fixed, deliberately:
//
//   - REFUSING the retire here (the reviewer's proposed fix) would leave the
//     path in E_PATH_INTENT_BUSY and `eject` in E_EJECT_UNVERIFIED forever,
//     with no built alternative — reintroducing exactly the permanent wedge
//     this verb exists to remove, in the one case where the operator's content
//     is already correct.
//   - Recording `recovered` instead of `retired` would be honest in the live
//     database but would not survive a database loss: Rebuild reconstructs the
//     state from the receipt (`allocated`) and then applies the journal's
//     retire replay, so keeping it would need new rebuild machinery for a
//     residue with no observable consequence.
//
// The residue really is unobservable through every face: `check` skips retired
// allocations, the ticket itself is still scanned, indexed and readable, the ID
// can never be reallocated, and no face renders `allocations.state`. This test
// asserts all of that, so the day the residue starts to matter, it fails.
//
// Same shape as the test this build replaced: when force-materialise is built
// (the real resolution for this case), this test must be changed deliberately.
func TestRetiringAnAllocationWhoseFileValidlyClaimsTheIDIsAnAcceptedGap(t *testing.T) {
	f := newAllocationFixture(t, true)
	ctx := context.Background()

	valid, err := domain.RenderTicket(domain.Ticket{
		Schema: 1, ID: f.intent.AllocationID, Project: "aira", Title: "the third party's own ticket",
		Status: domain.StatusPlanned, Kind: domain.KindFeature, Severity: domain.SeverityP2,
		Labels: []string{}, Relations: []domain.Relation{},
	}, "body")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(f.intent.Path, valid, 0o644); err != nil {
		t.Fatal(err)
	}

	if _, err := f.store.RetireIntent(ctx, fmt.Sprint(f.intent.Seq)); err != nil {
		t.Fatalf("retire = %v; refusing here would re-wedge the path, so it must succeed", err)
	}
	// The stale half of the record, pinned.
	if got := allocationState(t, f.store, f.intent.AllocationID); got != "retired" {
		t.Fatalf("allocation state = %q, want retired — this test no longer pins the gap it claims to", got)
	}

	// Every consequence that would make the residue matter is asserted absent.
	if err := f.store.Rebuild(ctx); err != nil {
		t.Fatalf("rebuild: %v", err)
	}
	record, err := f.store.Get(f.intent.AllocationID)
	if err != nil {
		t.Fatalf("the ID must still resolve to a live ticket: %v", err)
	}
	if record.Ticket.Title != "the third party's own ticket" {
		t.Fatalf("indexed ticket = %q", record.Ticket.Title)
	}
	report, err := f.store.Check(ctx)
	if err != nil {
		t.Fatalf("check: %v", err)
	}
	if report.Dimensions["allocated-id-file"] == "fail" {
		t.Fatalf("check fails after the retire: %+v", report.Findings)
	}
	next, err := f.store.AllocateID(ctx, "AIRA")
	if err != nil {
		t.Fatal(err)
	}
	if next == f.intent.AllocationID {
		t.Fatalf("%s was reallocated", next)
	}
}
