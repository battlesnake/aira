package store

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"aira/internal/codes"
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
	// The snippet must come from the CONTENT column and actually show the match,
	// not merely be non-empty. The old query snippeted column index 3, which is
	// worktree_id — always a non-empty string, so the previous `!= ""` check
	// passed while every snippet carried the literal worktree name instead of
	// the matched text. This is the assertion that tells the two apart.
	for _, row := range rows {
		if !strings.Contains(strings.ToLower(row.Snippet), "lunar") {
			t.Fatalf("snippet %q does not show the match; it is not taken from the content column", row.Snippet)
		}
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
	if _, err := main.db.Exec(`INSERT INTO search_fts(project_id,kind,ref_id,worktree_id,content) VALUES(?,?,?,?,?)`, main.projectID, "ticket", "stale", "main", "stale row"); err != nil {
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

func TestSearchFTSIsProjectScopedAcrossRebuildAndMatch(t *testing.T) {
	base := t.TempDir()
	common := filepath.Join(base, "common")
	state := filepath.Join(base, "state")
	projectA, err := Open(context.Background(), Options{
		Root: filepath.Join(base, "project-a"), CommonDir: common, DBPath: filepath.Join(state, "state.db"),
		RegistryPath: filepath.Join(state, "registry.jsonl"), ProjectID: "project-a", WorktreeID: "main",
		ProjectSlug: "project-a", Prefixes: []string{"AIRA"},
	})
	if err != nil {
		t.Fatal(err)
	}
	projectB, err := Open(context.Background(), Options{
		Root: filepath.Join(base, "project-b"), CommonDir: common, DBPath: filepath.Join(state, "state.db"),
		RegistryPath: filepath.Join(state, "registry.jsonl"), ProjectID: "project-b", WorktreeID: "main",
		ProjectSlug: "project-b", Prefixes: []string{"BIRA"},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = projectA.Close(); _ = projectB.Close() })
	if _, err := projectA.CreateTicket(context.Background(), testCreateInput("A ticket", "projectaonly")); err != nil {
		t.Fatal(err)
	}
	if _, err := projectB.CreateTicket(context.Background(), domain.CreateTicketInput{Title: "B ticket", Body: "projectbonly", Kind: domain.KindFeature, Severity: domain.SeverityP2}); err != nil {
		t.Fatal(err)
	}
	if _, err := projectB.Search(context.Background(), "projectbonly", ""); err != nil {
		t.Fatal(err)
	}
	if err := projectA.Rebuild(context.Background()); err != nil {
		t.Fatal(err)
	}
	var projectBRows int
	if err := projectA.db.QueryRow(`SELECT count(*) FROM search_fts WHERE project_id=?`, projectB.projectID).Scan(&projectBRows); err != nil {
		t.Fatalf("project-scoped FTS schema/query: %v", err)
	}
	if projectBRows != 1 {
		t.Fatalf("project A rebuild erased project B FTS rows: %d", projectBRows)
	}

	called := false
	projectA.beforeSearchQuery = func() error {
		called = true
		_, err := projectA.db.Exec(`INSERT INTO search_fts(project_id,kind,ref_id,worktree_id,content) VALUES(?,?,?,?,?)`,
			projectB.projectID, "ticket", "BIRA-1", projectB.worktreeID, "crossprojectonly")
		return err
	}
	rows, err := projectA.Search(context.Background(), "crossprojectonly", "")
	if err != nil {
		t.Fatal(err)
	}
	if !called {
		t.Fatal("search query seam was not reached")
	}
	if len(rows) != 0 {
		t.Fatalf("project A grep returned project B rows: %#v", rows)
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
	findings, inconclusive, err := scanFindingFiles(s.root, s.worktreeID)
	if inconclusive {
		t.Fatal("finding scan unexpectedly inconclusive")
	}
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
	for _, query := range []string{`"unterminated`, `nosuch:term`, `alpha AND (`} {
		if _, err := s.Search(context.Background(), query, ""); ErrorCode(err) != "E_QUERY_INVALID" || codes.ExitForCode(ErrorCode(err)) != 2 {
			t.Fatalf("malformed query %q error = %v", query, err)
		}
	}
}

func TestSearchKeepsOneGrepSnapshotAgainstMutation(t *testing.T) {
	s := queryTestStore(t)
	ticket, err := s.CreateTicket(context.Background(), testCreateInput("snapshot", "oldsearchcontent"))
	if err != nil {
		t.Fatal(err)
	}

	mutationAtWrite := make(chan struct{})
	allowWrite := make(chan struct{})
	mutationDone := make(chan error, 1)
	allowClosed := false
	s.beforeMaterialise = func(intent Intent) error {
		if intent.Kind != IntentKindTicketFile || intent.Ticket.ID != ticket.ID {
			return nil
		}
		close(mutationAtWrite)
		<-allowWrite
		return nil
	}
	interleaved := false
	s.beforeSearchReconcileCommit = func() {
		go func() {
			_, mutationErr := s.SetTicket(context.Background(), ticket.ID, "body", "newsearchcontent")
			mutationDone <- mutationErr
		}()
		select {
		case <-mutationAtWrite:
			interleaved = true
			close(allowWrite)
			allowClosed = true
			if mutationErr := <-mutationDone; mutationErr != nil {
				t.Errorf("concurrent mutation: %v", mutationErr)
			}
		case <-time.After(250 * time.Millisecond):
		}
	}
	rows, err := s.Search(context.Background(), "oldsearchcontent", "")
	if err != nil {
		t.Fatal(err)
	}
	select {
	case mutationErr := <-mutationDone:
		if mutationErr != nil {
			t.Fatal(mutationErr)
		}
	default:
		if !allowClosed {
			close(allowWrite)
		}
		if mutationErr := <-mutationDone; mutationErr != nil {
			t.Fatal(mutationErr)
		}
	}
	if interleaved {
		t.Fatalf("mutation interleaved with grep snapshot; rows=%#v", rows)
	}
	if len(rows) != 1 || rows[0].ID != ticket.ID {
		t.Fatalf("snapshot result = %#v", rows)
	}
	s.beforeSearchReconcileCommit = nil
	if rows, err := s.Search(context.Background(), "newsearchcontent", ""); err != nil || len(rows) != 1 {
		t.Fatalf("next grep did not reflect eventual mutation: %#v, %v", rows, err)
	}
}

func TestSearchRealIndexFailureIsUnevaluated(t *testing.T) {
	s := queryTestStore(t)
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Search(context.Background(), "anything", ""); ErrorCode(err) != "E_INDEX_UNEVALUATED" || codes.ExitForCode(ErrorCode(err)) != 3 {
		t.Fatalf("closed search store error = %v", err)
	}
}
