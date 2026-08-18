package store

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"aira/internal/domain"
	"aira/internal/gitcontext"
)

func TestRantIdempotencyByteIdentityAndGitContext(t *testing.T) {
	s, root := rantTestStore(t)
	ticket, err := s.CreateTicket(context.Background(), domain.CreateTicketInput{Title: "rant reference", Kind: domain.KindFeature, Severity: domain.SeverityP2})
	if err != nil {
		t.Fatal(err)
	}
	body := "  fake-token ghp_not_a_real_secret\nkeep trailing  \n"
	ctx := gitcontext.GitContext{
		RepoRoot:     gitcontext.Field{Value: root, Status: gitcontext.StatusValue},
		WorktreePath: gitcontext.Field{Value: root, Status: gitcontext.StatusValue},
		WorktreeID:   gitcontext.Field{Value: "wt-rant", Status: gitcontext.StatusValue},
		HeadHash:     gitcontext.Field{Value: strings.Repeat("a", 40), Status: gitcontext.StatusValue},
		HeadRef:      gitcontext.Field{Value: "refs/heads/main", Status: gitcontext.StatusValue},
		RemoteURL:    gitcontext.Field{Value: "https://example.test/repo.git", Status: gitcontext.StatusValue},
		ObservedAt:   "2026-08-18T10:00:00Z", ResolverVersion: gitcontext.ResolverVersion,
	}
	refs := []domain.RantRef{{Kind: domain.RantRefTicket, ID: ticket.ID}}
	input := domain.RantInput{Body: body, Tags: []string{"Slow_Tests", "infra"}, Severity: domain.RantSeverityBlocker, Refs: refs, IdempotencyKey: "attempt-1", Actor: "terra", Session: "sess-1", Model: "opus"}
	first, err := s.AddRant(context.Background(), input, ctx)
	if err != nil {
		t.Fatal(err)
	}
	// An honest retry repeats every caller field and observes the same context,
	// differing only in tag spelling that normalises identically and in the
	// resolver's fresh observed_at (a volatile envelope field). Idempotent.
	retry := input
	retry.Tags = []string{"infra", "slow tests"}
	retryCtx := ctx
	retryCtx.ObservedAt = "2026-08-18T10:05:00Z"
	second, err := s.AddRant(context.Background(), retry, retryCtx)
	if err != nil {
		t.Fatal(err)
	}
	if !second.Idempotent || second.Rant.ID != first.Rant.ID {
		t.Fatalf("retry = %#v, first = %#v", second, first)
	}
	got, err := s.GetRant(first.Rant.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Body != body || got.GitContext != ctx {
		t.Fatalf("stored identity changed: body=%q context=%#v", got.Body, got.GitContext)
	}
	// Volatile Git state is excluded: a retry after HEAD moved between attempts
	// is still the same submission and stays idempotent.
	movedHead := ctx
	movedHead.HeadHash = gitcontext.Field{Value: strings.Repeat("b", 40), Status: gitcontext.StatusValue}
	if third, err := s.AddRant(context.Background(), input, movedHead); err != nil || !third.Idempotent || third.Rant.ID != first.Rant.ID {
		t.Fatalf("moved-HEAD retry not idempotent: %#v err=%v", third, err)
	}
	// Every caller-supplied field, plus the stable repository/worktree scope,
	// is a discriminator: a reused key describing a different caller or a
	// different location must conflict, not silently alias the original rant.
	mk := func(mut func(*domain.RantInput)) domain.RantInput {
		c := input
		c.Tags = append([]string(nil), input.Tags...)
		c.Refs = append([]domain.RantRef(nil), input.Refs...)
		mut(&c)
		return c
	}
	otherWorktree := ctx
	otherWorktree.WorktreeID = gitcontext.Field{Value: "wt-different", Status: gitcontext.StatusValue}
	conflicts := []struct {
		name  string
		input domain.RantInput
		ctx   gitcontext.GitContext
	}{
		{"body", mk(func(i *domain.RantInput) { i.Body += "changed" }), ctx},
		{"tags", mk(func(i *domain.RantInput) { i.Tags = []string{"other"} }), ctx},
		{"severity", mk(func(i *domain.RantInput) { i.Severity = domain.RantSeverityAnnoyance }), ctx},
		{"refs", mk(func(i *domain.RantInput) { i.Refs = nil }), ctx},
		{"actor", mk(func(i *domain.RantInput) { i.Actor = "different-agent" }), ctx},
		{"session", mk(func(i *domain.RantInput) { i.Session = "sess-2" }), ctx},
		{"model", mk(func(i *domain.RantInput) { i.Model = "sonnet" }), ctx},
		{"worktree-scope", mk(func(i *domain.RantInput) {}), otherWorktree},
	}
	for _, tc := range conflicts {
		t.Run("conflicting-"+tc.name, func(t *testing.T) {
			_, err := s.AddRant(context.Background(), tc.input, tc.ctx)
			if ErrorCode(err) != domain.CodeRantIdempotencyConflict {
				t.Fatalf("conflict error = %v", err)
			}
		})
	}
}

func TestRantIdempotencyConcurrentMultiStore(t *testing.T) {
	// Two independent stores over one DB race the same key. BEGIN IMMEDIATE
	// serialises them: identical input yields exactly one rant, observed
	// idempotently by the loser; a different caller field yields a conflict.
	base := t.TempDir()
	root := filepath.Join(base, "repo")
	s1 := openRantStore(t, base, root)
	s2 := openRantStore(t, base, root)
	ctxv := gitcontext.GitContext{RepoRoot: gitcontext.Field{Value: root, Status: gitcontext.StatusValue}, WorktreePath: gitcontext.Field{Value: root, Status: gitcontext.StatusValue}, WorktreeID: gitcontext.Field{Value: "wt-rant", Status: gitcontext.StatusValue}, ObservedAt: "2026-08-18T10:00:00Z", ResolverVersion: "v"}
	input := domain.RantInput{Body: "same body", Tags: []string{"infra"}, IdempotencyKey: "race-1", Actor: "terra"}
	type res struct {
		out RantAddResult
		err error
	}
	ch := make(chan res, 2)
	start := make(chan struct{})
	for _, st := range []*Store{s1, s2} {
		st := st
		go func() {
			<-start
			out, err := st.AddRant(context.Background(), input, ctxv)
			ch <- res{out, err}
		}()
	}
	close(start)
	a, b := <-ch, <-ch
	if a.err != nil || b.err != nil {
		t.Fatalf("same-input race errored: %v / %v", a.err, b.err)
	}
	if a.out.Rant.ID != b.out.Rant.ID {
		t.Fatalf("same-input race produced two rants: %s vs %s", a.out.Rant.ID, b.out.Rant.ID)
	}
	if a.out.Idempotent == b.out.Idempotent {
		t.Fatalf("expected one insert and one idempotent hit, got %v/%v", a.out.Idempotent, b.out.Idempotent)
	}
	if _, err := s2.AddRant(context.Background(), domain.RantInput{Body: "same body", Tags: []string{"infra"}, IdempotencyKey: "race-1", Actor: "other"}, ctxv); ErrorCode(err) != domain.CodeRantIdempotencyConflict {
		t.Fatalf("different-caller reuse = %v", err)
	}
}

func TestRantReviewsAppendAndReviewedIsDerived(t *testing.T) {
	s, _ := rantTestStore(t)
	added := mustAddRant(t, s, domain.RantInput{Body: "review me", Tags: []string{"slow-tests"}, Actor: "terra"})
	if added.Rant.Reviewed {
		t.Fatal("new rant reported reviewed")
	}
	first, err := s.ReviewRant(context.Background(), added.Rant.ID, domain.RantReviewInput{Reviewer: "owner", Outcome: domain.RantOutcomePlanned, Note: "make a ticket"})
	if err != nil {
		t.Fatal(err)
	}
	second, err := s.ReviewRant(context.Background(), added.Rant.ID, domain.RantReviewInput{Reviewer: "owner", Outcome: domain.RantOutcomeNeedsEvidence})
	if err != nil {
		t.Fatal(err)
	}
	if first.Review.ID == second.Review.ID {
		t.Fatal("review was overwritten instead of appended")
	}
	got, err := s.GetRant(added.Rant.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !got.Reviewed || len(got.Reviews) != 2 || got.Reviews[0].Outcome != domain.RantOutcomePlanned || got.Reviews[1].Outcome != domain.RantOutcomeNeedsEvidence {
		t.Fatalf("reviews = %#v", got.Reviews)
	}
	if _, err := s.db.Exec(`UPDATE rant_reviews SET outcome='actioned' WHERE project_id=? AND rant_id=?`, s.projectID, added.Rant.ID); err == nil {
		t.Fatal("database allowed an existing review to be updated")
	}
	if _, err := s.db.Exec(`DELETE FROM rant_reviews WHERE project_id=? AND rant_id=?`, s.projectID, added.Rant.ID); err == nil {
		t.Fatal("database allowed an existing review to be deleted")
	}
}

func TestRantJournalCarriesEventHeadersOnly(t *testing.T) {
	s, _ := rantTestStore(t)
	body := "journal-must-not-carry-this-rant-body"
	note := "journal-must-not-carry-this-review-note"
	added := mustAddRant(t, s, domain.RantInput{Body: body, Actor: "terra"})
	if _, err := s.ReviewRant(context.Background(), added.Rant.ID, domain.RantReviewInput{Reviewer: "owner", Outcome: domain.RantOutcomeActioned, Note: note}); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(s.auditDir, "journal.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	journal := string(data)
	if !strings.Contains(journal, `"verb":"rant.create"`) || !strings.Contains(journal, `"verb":"rant.reviewed"`) {
		t.Fatalf("missing rant event headers: %s", journal)
	}
	if strings.Contains(journal, body) || strings.Contains(journal, note) {
		t.Fatalf("journal leaked DB-only rant prose: %s", journal)
	}
}

func TestRantLoopCloserContainsNoProse(t *testing.T) {
	s, _ := rantTestStore(t)
	prior := mustAddRant(t, s, domain.RantInput{Body: "secret fake token ghp_dont_surface", Tags: []string{"slow-tests"}, Actor: "one"})
	if _, err := s.ReviewRant(context.Background(), prior.Rant.ID, domain.RantReviewInput{Reviewer: "owner", Outcome: domain.RantOutcomeWontFix, Note: "private note prose"}); err != nil {
		t.Fatal(err)
	}
	current := mustAddRant(t, s, domain.RantInput{Body: "again", Tags: []string{"slow_tests"}, Actor: "two"})
	data, err := json.Marshal(current.SharingRecordedTags)
	if err != nil {
		t.Fatal(err)
	}
	text := string(data)
	if strings.Contains(text, "secret") || strings.Contains(text, "private note") || strings.Contains(text, "ghp_") {
		t.Fatalf("loop closer leaked prose: %s", text)
	}
	if len(current.SharingRecordedTags) != 1 || current.SharingRecordedTags[0].RantID != prior.Rant.ID || current.SharingRecordedTags[0].Tag != "slow-tests" || current.SharingRecordedTags[0].Outcome != domain.RantOutcomeWontFix {
		t.Fatalf("loop closer = %#v", current.SharingRecordedTags)
	}
}

func TestRantRedactErasesEveryProseSurfaceKeepsProvenance(t *testing.T) {
	s, root := rantTestStore(t)
	const secret = "ghp_super_secret_leaked_token_zzz"
	ticket, err := s.CreateTicket(context.Background(), domain.CreateTicketInput{Title: "ref", Kind: domain.KindFeature, Severity: domain.SeverityP2})
	if err != nil {
		t.Fatal(err)
	}
	ctx := gitcontext.GitContext{RepoRoot: gitcontext.Field{Value: root, Status: gitcontext.StatusValue}, ObservedAt: "2026-08-18T10:00:00Z", ResolverVersion: "v-test"}
	added, err := s.AddRant(context.Background(), domain.RantInput{Body: "paste contained " + secret, Tags: []string{"security"}, Refs: []domain.RantRef{{Kind: domain.RantRefTicket, ID: ticket.ID}}, Actor: "terra"}, ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.ReviewRant(context.Background(), added.Rant.ID, domain.RantReviewInput{Reviewer: "owner", Outcome: domain.RantOutcomePlanned, Note: "reviewer echoed " + secret}); err != nil {
		t.Fatal(err)
	}
	// Populate the disposable FTS index so the secret is present there too; a
	// correct redaction must delete those rows now, not wait for a rebuild.
	if rows, err := s.Search(context.Background(), "leaked", "rant"); err != nil || len(rows) != 1 {
		t.Fatalf("pre-redaction search = %v %#v", err, rows)
	}
	if _, err := s.RedactRant(context.Background(), added.Rant.ID); err != nil {
		t.Fatal(err)
	}
	// (1) canonical body scrubbed; provenance + audit skeleton retained.
	got, err := s.GetRant(added.Rant.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.ID != added.Rant.ID || got.Body != domain.RedactedRantBody || !got.Redacted {
		t.Fatalf("body not redacted: %#v", got)
	}
	if got.GitContext.RepoRoot != ctx.RepoRoot || got.GitContext.ObservedAt != ctx.ObservedAt || got.GitContext.ResolverVersion != ctx.ResolverVersion || got.Actor != "terra" || len(got.Tags) != 1 || got.Tags[0] != "security" || len(got.Refs) != 1 || got.Refs[0].ID != ticket.ID {
		t.Fatalf("redaction dropped provenance/skeleton: %#v", got)
	}
	if len(got.Reviews) != 1 || got.Reviews[0].Outcome != domain.RantOutcomePlanned || got.Reviews[0].Reviewer != "owner" || got.Reviews[0].Note != domain.RedactedRantBody {
		t.Fatalf("review skeleton/scrub wrong: %#v", got.Reviews)
	}
	// (2) no raw table retains a trace of the secret.
	for _, q := range []struct{ label, query string }{
		{"rants.body", `SELECT COUNT(*) FROM rants WHERE project_id=? AND instr(body,?)>0`},
		{"rant_reviews.note", `SELECT COUNT(*) FROM rant_reviews WHERE project_id=? AND instr(note,?)>0`},
		{"search_fts.content", `SELECT COUNT(*) FROM search_fts WHERE project_id=? AND instr(content,?)>0`},
	} {
		var n int
		if err := s.db.QueryRow(q.query, s.projectID, secret).Scan(&n); err != nil {
			t.Fatalf("%s scan: %v", q.label, err)
		}
		if n != 0 {
			t.Fatalf("secret survived in %s", q.label)
		}
	}
	// (3) grep no longer surfaces it, even after the reconciling rebuild.
	if rows, err := s.Search(context.Background(), "leaked", "rant"); err != nil || len(rows) != 0 {
		t.Fatalf("post-redaction search = %v %#v", err, rows)
	}
	// (4/5) responses were prose-free and the journal is metadata-only.
	data, err := os.ReadFile(filepath.Join(s.auditDir, "journal.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), secret) {
		t.Fatalf("journal leaked the secret: %s", string(data))
	}
	// The append-only redaction exception is exactly as wide as it must be:
	// nothing but the note→sentinel scrub is permitted.
	if _, err := s.db.Exec(`UPDATE rant_reviews SET outcome='actioned' WHERE project_id=? AND rant_id=?`, s.projectID, added.Rant.ID); err == nil {
		t.Fatal("redaction exception allowed an outcome rewrite")
	}
	if domain.RedactedRantBody != "[redacted]" {
		t.Fatalf("sentinel drift: the trigger literal must match domain.RedactedRantBody=%q", domain.RedactedRantBody)
	}
	// (6) A redacted rant refuses fresh prose — a new note would re-open a
	// prose surface — but still accepts structured triage.
	if _, err := s.ReviewRant(context.Background(), added.Rant.ID, domain.RantReviewInput{Reviewer: "owner", Outcome: domain.RantOutcomeWontFix, Note: "late " + secret}); ErrorCode(err) != domain.CodeRantRedacted {
		t.Fatalf("note on a redacted rant = %v, want E_RANT_REDACTED", err)
	}
	if _, err := s.ReviewRant(context.Background(), added.Rant.ID, domain.RantReviewInput{Reviewer: "owner", Outcome: domain.RantOutcomeWontFix}); err != nil {
		t.Fatalf("structured triage on a redacted rant rejected: %v", err)
	}
	// (7) The secret does not linger as raw bytes in the database or its WAL:
	// secure_delete overwrote the freed cells and the redaction checkpoint
	// folded and truncated the WAL.
	for _, suffix := range []string{"", "-wal"} {
		raw, err := os.ReadFile(s.dbPath + suffix)
		if errors.Is(err, os.ErrNotExist) && suffix != "" {
			continue // WAL truncated away entirely
		}
		if err != nil {
			t.Fatalf("read %s: %v", s.dbPath+suffix, err)
		}
		if bytes.Contains(raw, []byte(secret)) {
			t.Fatalf("secret survived as raw bytes in %s", s.dbPath+suffix)
		}
	}
}

func TestRantRedactReportsIncompletePhysicalErasureWhenWALHeld(t *testing.T) {
	base := t.TempDir()
	root := filepath.Join(base, "repo")
	s := openRantStore(t, base, root)
	const secret = "ghp_wal_held_secret_marker_zzz"
	added, err := s.AddRant(context.Background(), domain.RantInput{Body: "leak " + secret, Actor: "terra"}, gitcontext.GitContext{})
	if err != nil {
		t.Fatal(err)
	}
	// A second connection holds an open read transaction, pinning the WAL so a
	// TRUNCATE checkpoint cannot complete.
	reader := openRantStore(t, base, root)
	tx, err := reader.db.BeginTx(context.Background(), &sql.TxOptions{ReadOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	var n int
	if err := tx.QueryRow(`SELECT COUNT(*) FROM rants`).Scan(&n); err != nil {
		_ = tx.Rollback()
		t.Fatal(err)
	}
	// Redaction commits logically but must NOT falsely claim physical erasure.
	if _, err := s.RedactRant(context.Background(), added.Rant.ID); ErrorCode(err) != domain.CodeRantRedactionIncomplete {
		_ = tx.Rollback()
		t.Fatalf("redaction with a held WAL = %v, want E_RANT_REDACTION_INCOMPLETE", err)
	}
	// The logical scrub is still durable despite the incomplete physical purge.
	if got, err := s.GetRant(added.Rant.ID); err != nil || got.Body != domain.RedactedRantBody || !got.Redacted {
		t.Fatalf("logical redaction not committed: %#v err=%v", got, err)
	}
	// Releasing the reader and re-running redaction (idempotent) purges the WAL.
	if err := tx.Rollback(); err != nil {
		t.Fatal(err)
	}
	if _, err := s.RedactRant(context.Background(), added.Rant.ID); err != nil {
		t.Fatalf("redaction after releasing the reader: %v", err)
	}
	for _, suffix := range []string{"", "-wal"} {
		raw, err := os.ReadFile(s.dbPath + suffix)
		if errors.Is(err, os.ErrNotExist) && suffix != "" {
			continue
		}
		if err != nil {
			t.Fatal(err)
		}
		if bytes.Contains(raw, []byte(secret)) {
			t.Fatalf("secret survived as raw bytes in %s after purge", s.dbPath+suffix)
		}
	}
}

func TestRantRedactWorksAfterUpgradingStaleReviewTrigger(t *testing.T) {
	base := t.TempDir()
	root := filepath.Join(base, "repo")
	s := openRantStore(t, base, root)
	// Simulate a pre-fix database: swap the current trigger for the old
	// unconditional one that blocks ANY update to rant_reviews.
	if _, err := s.db.Exec(`DROP TRIGGER rant_reviews_no_update`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`CREATE TRIGGER rant_reviews_no_update BEFORE UPDATE ON rant_reviews BEGIN SELECT RAISE(ABORT,'rant reviews are append-only'); END`); err != nil {
		t.Fatal(err)
	}
	// Reopening must upgrade the stale trigger so redaction of a reviewed rant
	// scrubs the note instead of aborting the whole redaction.
	s2 := openRantStore(t, base, root)
	added, err := s2.AddRant(context.Background(), domain.RantInput{Body: "secret to scrub", Actor: "terra"}, gitcontext.GitContext{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s2.ReviewRant(context.Background(), added.Rant.ID, domain.RantReviewInput{Reviewer: "owner", Outcome: domain.RantOutcomePlanned, Note: "a note"}); err != nil {
		t.Fatal(err)
	}
	if _, err := s2.RedactRant(context.Background(), added.Rant.ID); err != nil {
		t.Fatalf("redaction aborted against a non-upgraded trigger: %v", err)
	}
	got, err := s2.GetRant(added.Rant.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Reviews) != 1 || got.Reviews[0].Note != domain.RedactedRantBody {
		t.Fatalf("note not scrubbed after trigger upgrade: %#v", got.Reviews)
	}
}

func TestRantListUnreviewedSinceAndCountByActor(t *testing.T) {
	s, _ := rantTestStore(t)
	one := mustAddRant(t, s, domain.RantInput{Body: "one", Tags: []string{"infra"}, Actor: "terra"})
	two := mustAddRant(t, s, domain.RantInput{Body: "two", Tags: []string{"infra"}, Actor: "opus"})
	if _, err := s.ReviewRant(context.Background(), one.Rant.ID, domain.RantReviewInput{Reviewer: "owner"}); err != nil {
		t.Fatal(err)
	}
	rows, err := s.ListRants(domain.RantListOptions{Unreviewed: true, Since: one.Rant.Seq})
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].ID != two.Rant.ID {
		t.Fatalf("rows = %#v", rows)
	}
	count, err := s.CountRants("", "actor")
	if err != nil {
		t.Fatal(err)
	}
	if count.Total != 2 || count.Groups["terra"].Rants != 1 || count.Groups["opus"].DistinctActors != 1 {
		t.Fatalf("count = %#v", count)
	}
}

func TestRantBoundsAndInProjectReferences(t *testing.T) {
	s, _ := rantTestStore(t)
	tooManyTags := make([]string, domain.MaxRantTags+1)
	for i := range tooManyTags {
		tooManyTags[i] = "tag-" + strings.Repeat("x", i+1)
	}
	if _, err := s.AddRant(context.Background(), domain.RantInput{Body: "body", Tags: tooManyTags}, gitcontext.GitContext{}); ErrorCode(err) != domain.CodeRantInvalid {
		t.Fatalf("tag bound error = %v", err)
	}
	if _, err := s.AddRant(context.Background(), domain.RantInput{Body: "body", Refs: []domain.RantRef{{Kind: domain.RantRefTicket, ID: "AIRA-999"}}}, gitcontext.GitContext{}); ErrorCode(err) != domain.CodeRantRefInvalid {
		t.Fatalf("missing ref error = %v", err)
	}
}

func TestRantBodiesSurviveEverySearchIndexRebuild(t *testing.T) {
	s, _ := rantTestStore(t)
	added := mustAddRant(t, s, domain.RantInput{Body: "distinctive phosphorescent failure", Tags: []string{"search"}})
	for attempt := 0; attempt < 2; attempt++ {
		rows, err := s.Search(context.Background(), "phosphorescent", "rant")
		if err != nil {
			t.Fatal(err)
		}
		if len(rows) != 1 || rows[0].ID != added.Rant.ID || rows[0].Kind != "rant" {
			t.Fatalf("search attempt %d = %#v", attempt, rows)
		}
	}
}

func rantTestStore(t *testing.T) (*Store, string) {
	t.Helper()
	base := t.TempDir()
	root := filepath.Join(base, "repo")
	return openRantStore(t, base, root), root
}

// openRantStore opens a store for the fixed rant project/worktree over the DB
// under base. Calling it twice with the same base yields two independent
// handles onto one database, modelling cross-process contention.
func openRantStore(t *testing.T, base, root string) *Store {
	t.Helper()
	common := filepath.Join(root, ".git")
	if err := os.MkdirAll(common, 0o755); err != nil {
		t.Fatal(err)
	}
	s, err := Open(context.Background(), Options{Root: root, CommonDir: common, DBPath: filepath.Join(base, "state.db"), RegistryPath: filepath.Join(base, "registry.jsonl"), ProjectID: "project-rant", WorktreeID: "wt-rant", ProjectSlug: "rant", Prefixes: []string{"AIRA"}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func mustAddRant(t *testing.T, s *Store, input domain.RantInput) RantAddResult {
	t.Helper()
	result, err := s.AddRant(context.Background(), input, gitcontext.GitContext{})
	if err != nil {
		t.Fatal(err)
	}
	return result
}
