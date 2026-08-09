package store

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"aira/internal/domain"
)

func TestSearchCoversCurrentTicketsAndReviewFindings(t *testing.T) {
	s := queryTestStore(t)
	ticket, err := s.CreateTicket(context.Background(), testCreateInput("Searchable ticket", "the lunar keyword is here"))
	if err != nil {
		t.Fatal(err)
	}
	finding, _, err := s.AddFinding(context.Background(), domain.ReviewFindingInput{
		TicketID: ticket.ID, Category: "bug", Severity: domain.SeverityP1, Verdict: domain.VerdictConfirmed,
		Source: "review", Message: "the lunar keyword is also in this finding",
	})
	if err != nil {
		t.Fatal(err)
	}
	rows, err := s.Search(context.Background(), "lunar", "")
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 || rows[0].Kind == rows[1].Kind {
		t.Fatalf("search rows = %#v", rows)
	}
	if rows[0].Snippet == "" || rows[1].Snippet == "" {
		t.Fatalf("search snippets = %#v", rows)
	}
	if rows[0].Rank > rows[1].Rank {
		t.Fatalf("search is not bm25 best-first: %#v", rows)
	}
	if got, err := s.Search(context.Background(), "lunar", "ticket"); err != nil || len(got) != 1 || got[0].ID != ticket.ID {
		t.Fatalf("ticket filter = %#v, %v", got, err)
	}
	if got, err := s.Search(context.Background(), "lunar", "finding"); err != nil || len(got) != 1 || got[0].ID != finding.Key {
		t.Fatalf("finding filter = %#v, %v", got, err)
	}
	if got, err := s.Search(context.Background(), "absent", ""); err != nil || len(got) != 0 {
		t.Fatalf("empty search result = %#v, %v", got, err)
	}
}

func TestSearchSupportsFTSQueriesAndFreshMutations(t *testing.T) {
	s := queryTestStore(t)
	ticket, err := s.CreateTicket(context.Background(), testCreateInput("alpha beta", "prefixable unique"))
	if err != nil {
		t.Fatal(err)
	}
	for _, query := range []string{`"alpha beta"`, "prefix*", "alpha AND beta", "alpha OR absent", "alpha NOT absent"} {
		rows, searchErr := s.Search(context.Background(), query, "")
		if searchErr != nil || len(rows) != 1 || rows[0].ID != ticket.ID {
			t.Fatalf("query %q = %#v, %v", query, rows, searchErr)
		}
	}
	if _, err := s.SetTicket(context.Background(), ticket.ID, "body", "fresh mutation"); err != nil {
		t.Fatal(err)
	}
	if rows, err := s.Search(context.Background(), "fresh", ""); err != nil || len(rows) != 1 {
		t.Fatalf("mutated ticket search = %#v, %v", rows, err)
	}
	if rows, err := s.Search(context.Background(), "unique", ""); err != nil || len(rows) != 0 {
		t.Fatalf("old ticket content remains searchable = %#v, %v", rows, err)
	}
	second, err := s.CreateTicket(context.Background(), testCreateInput("rare", "rare rare rare"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateTicket(context.Background(), testCreateInput("rare", "rare")); err != nil {
		t.Fatal(err)
	}
	rows, err := s.Search(context.Background(), "rare", "ticket")
	if err != nil || len(rows) != 2 || rows[0].ID != second.ID {
		t.Fatalf("bm25 ordering fixture = %#v, %v", rows, err)
	}
}

func TestSearchIsWorktreeScopedAndRebuildRemovesStaleRows(t *testing.T) {
	main := queryTestStore(t)
	otherRoot := t.TempDir()
	other, err := Open(context.Background(), Options{
		Root: otherRoot, CommonDir: main.commonDir, DBPath: main.dbPath, RegistryPath: main.registryPath,
		ProjectID: main.projectID, WorktreeID: "other", ProjectSlug: main.projectSlug, Prefixes: []string{"AIRA"},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = other.Close() })
	otherTicket, err := other.CreateTicket(context.Background(), testCreateInput("other worktree secret", "secret"))
	if err != nil {
		t.Fatal(err)
	}
	if rows, err := main.Search(context.Background(), "secret", ""); err != nil || len(rows) != 0 {
		t.Fatalf("cross-worktree search = %#v, %v", rows, err)
	}
	if rows, err := other.Search(context.Background(), "secret", ""); err != nil || len(rows) != 1 || rows[0].ID != otherTicket.ID {
		t.Fatalf("other-worktree search = %#v, %v", rows, err)
	}
	if _, err := os.Stat(filepath.Join(other.root, ".aira", "tickets", otherTicket.ID+".md")); err != nil {
		t.Fatal(err)
	}
	if _, err := main.db.Exec(`INSERT INTO search_fts(kind,ref_id,worktree_id,content) VALUES('ticket','stale','main','stale row')`); err != nil {
		t.Fatal(err)
	}
	if err := main.Rebuild(context.Background()); err != nil {
		t.Fatal(err)
	}
	var count int
	if err := main.db.QueryRow(`SELECT count(*) FROM search_fts WHERE worktree_id='main' AND ref_id='stale'`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatalf("stale FTS row survived rebuild: %d", count)
	}
}

func TestRebuildReconstructsSearchRowsAfterCanonicalRemoval(t *testing.T) {
	s := queryTestStore(t)
	ticket, err := s.CreateTicket(context.Background(), testCreateInput("remove me", "ephemeral needle"))
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.AddFinding(context.Background(), domain.ReviewFindingInput{
		TicketID: ticket.ID, Category: "bug", Severity: domain.SeverityP1, Verdict: domain.VerdictConfirmed,
		Source: "review", Message: "ephemeral finding needle",
	}); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(s.root, ".aira", "tickets", ticket.ID+".md")); err != nil {
		t.Fatal(err)
	}
	findings, err := scanFindingFiles(s.root, s.worktreeID)
	if err != nil || len(findings.valid) != 1 {
		t.Fatalf("finding scan before removal = %#v, %v", findings, err)
	}
	if err := os.Remove(filepath.Join(s.root, ".aira", "findings", findings.valid[0].Finding.Key+".md")); err != nil {
		t.Fatal(err)
	}
	if err := s.Rebuild(context.Background()); err != nil {
		t.Fatal(err)
	}
	var count int
	if err := s.db.QueryRow(`SELECT count(*) FROM search_fts WHERE worktree_id=?`, s.worktreeID).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatalf("removed canonical entities survived rebuild: %d", count)
	}
}

func TestSearchRejectsMalformedFTSQuery(t *testing.T) {
	s := queryTestStore(t)
	if _, err := s.Search(context.Background(), `"unterminated`, ""); ErrorCode(err) != "E_QUERY_INVALID" {
		t.Fatalf("malformed query error = %v", err)
	}
}
