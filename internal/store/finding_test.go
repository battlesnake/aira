package store

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"aira/internal/domain"
)

func reviewFindingInput(message string) domain.ReviewFindingInput {
	return domain.ReviewFindingInput{TicketID: "AIRA-1", Category: "flaky-test", Severity: domain.SeverityP1,
		Verdict: domain.VerdictConfirmed, Source: "codex", Message: message, File: "internal/worker.go", Line: 42}
}

func TestFindingAddDeduplicatesByIdentityAndUpdatesContent(t *testing.T) {
	base := persistentTemp(t, "finding-dedup")
	root := filepath.Join(base, "main")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	s := testStore(t, root, filepath.Join(base, "common"), filepath.Join(base, "state"))

	first, firstEvent, err := s.AddFinding(context.Background(), reviewFindingInput("old message"))
	if err != nil {
		t.Fatal(err)
	}
	secondInput := reviewFindingInput("corrected message")
	secondInput.Severity, secondInput.Verdict = domain.SeverityP0, domain.VerdictPlausible
	second, secondEvent, err := s.AddFinding(context.Background(), secondInput)
	if err != nil {
		t.Fatal(err)
	}
	if first.Key != second.Key || firstEvent.Seq == secondEvent.Seq {
		t.Fatalf("dedup result=%#v %#v", first, second)
	}
	files, err := filepath.Glob(filepath.Join(root, ".aira", "findings", "*.md"))
	if err != nil || len(files) != 1 {
		t.Fatalf("finding files=%v err=%v", files, err)
	}
	var rows int
	if err := s.db.QueryRow(`SELECT count(*) FROM findings WHERE project_id=? AND subtype=?`, s.projectID, "review").Scan(&rows); err != nil {
		t.Fatal(err)
	}
	if rows != 1 {
		t.Fatalf("review index rows=%d", rows)
	}
	got, err := s.GetFinding(first.Key)
	if err != nil {
		t.Fatal(err)
	}
	if got.Finding.Message != "corrected message" || got.Finding.Severity != domain.SeverityP0 {
		t.Fatalf("stored finding=%#v", got)
	}
}

func TestFindingIntentDispatchNeverTouchesTicketPath(t *testing.T) {
	base := persistentTemp(t, "finding-dispatch")
	root := filepath.Join(base, "main")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	s := testStore(t, root, filepath.Join(base, "common"), filepath.Join(base, "state"))
	finding, err := domain.NewReviewFinding(reviewFindingInput("dispatch"))
	if err != nil {
		t.Fatal(err)
	}
	data, err := domain.RenderFinding(finding)
	if err != nil {
		t.Fatal(err)
	}
	intent, err := s.prepareFindingMutation(context.Background(), s.findingPath(finding.Key), "", data, "finding.add", finding)
	if err != nil {
		t.Fatal(err)
	}
	if intent.Kind != IntentKindFindingFile {
		t.Fatalf("intent kind=%q", intent.Kind)
	}
	if err := s.materialiseIntent(context.Background(), intent); err != nil {
		t.Fatal(err)
	}
	var allocations, tickets, reviewRows int
	if err := s.db.QueryRow(`SELECT count(*) FROM allocations WHERE project_id=?`, s.projectID).Scan(&allocations); err != nil {
		t.Fatal(err)
	}
	if err := s.db.QueryRow(`SELECT count(*) FROM tickets WHERE project_id=?`, s.projectID).Scan(&tickets); err != nil {
		t.Fatal(err)
	}
	if err := s.db.QueryRow(`SELECT count(*) FROM findings WHERE project_id=? AND subtype='review'`, s.projectID).Scan(&reviewRows); err != nil {
		t.Fatal(err)
	}
	if allocations != 0 || tickets != 0 || reviewRows != 1 {
		t.Fatalf("finding dispatch allocations=%d tickets=%d reviewRows=%d", allocations, tickets, reviewRows)
	}
}

func TestFindingIntentCrashReplayUsesPersistedKind(t *testing.T) {
	base := persistentTemp(t, "finding-replay")
	root := filepath.Join(base, "main")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	s := testStore(t, root, filepath.Join(base, "common"), filepath.Join(base, "state"))
	finding, err := domain.NewReviewFinding(reviewFindingInput("replay"))
	if err != nil {
		t.Fatal(err)
	}
	data, err := domain.RenderFinding(finding)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.prepareFindingMutation(context.Background(), s.findingPath(finding.Key), "", data, "finding.add", finding); err != nil {
		t.Fatal(err)
	}
	if err := s.Reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(s.findingPath(finding.Key)); err != nil {
		t.Fatal(err)
	}
	got, err := s.GetFinding(finding.Key)
	if err != nil {
		t.Fatal(err)
	}
	if got.Finding.Message != "replay" {
		t.Fatalf("replayed finding=%#v", got)
	}
}

func TestFindingSetRewritesAuthoritativeFileAndReindexes(t *testing.T) {
	base := persistentTemp(t, "finding-set")
	root := filepath.Join(base, "main")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	s := testStore(t, root, filepath.Join(base, "common"), filepath.Join(base, "state"))
	finding, _, err := s.AddFinding(context.Background(), reviewFindingInput("waive me"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.SetFinding(context.Background(), finding.Key, domain.DispositionWaived, "accepted risk", "reviewer"); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(s.findingPath(finding.Key))
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := domain.ParseFinding(data)
	if err != nil {
		t.Fatal(err)
	}
	if parsed.Disposition != domain.DispositionWaived || parsed.WaiverReason != "accepted risk" || parsed.WaiverActor != "reviewer" {
		t.Fatalf("file finding=%#v", parsed)
	}
	got, err := s.GetFinding(finding.Key)
	if err != nil {
		t.Fatal(err)
	}
	if got.Finding.Disposition != domain.DispositionWaived {
		t.Fatalf("index/readback finding=%#v", got)
	}
	if _, err := s.SetFinding(context.Background(), finding.Key, domain.DispositionWaived, "", "reviewer"); !errors.Is(err, ErrWaiverReasonRequired) {
		t.Fatalf("missing reason error=%v", err)
	}
}

func TestFindingQueryHasDistinctFieldsAndSubtypeWidening(t *testing.T) {
	base := persistentTemp(t, "finding-query")
	root := filepath.Join(base, "main")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	s := testStore(t, root, filepath.Join(base, "common"), filepath.Join(base, "state"))
	if _, _, err := s.AddFinding(context.Background(), reviewFindingInput("race in worker")); err != nil {
		t.Fatal(err)
	}
	if err := s.recordRebuildFinding(context.Background(), registryEntry{Root: root, WorktreeID: s.worktreeID}, "broken root"); err != nil {
		t.Fatal(err)
	}
	rows, err := s.ListFindings(`subtype:review category:flaky-test text:"worker"`)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].Finding.Subtype != domain.FindingSubtypeReview {
		t.Fatalf("review query=%#v", rows)
	}
	rows, err = s.ListFindings(`subtype:reconciliation`)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) == 0 || rows[0].Finding.Subtype != domain.FindingSubtypeReconciliation {
		t.Fatalf("reconciliation query=%#v", rows)
	}
	rows, err = s.ListFindings(`subtype:any`)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) < 2 {
		t.Fatalf("any query=%#v", rows)
	}
	if _, err := s.ListFindings(`severity:P0`); err != nil {
		t.Fatal(err)
	}
}

func TestFindingDivergenceIsReportedBeforeRebuildHealsIt(t *testing.T) {
	base := persistentTemp(t, "finding-divergence")
	root := filepath.Join(base, "main")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	s := testStore(t, root, filepath.Join(base, "common"), filepath.Join(base, "state"))
	finding, _, err := s.AddFinding(context.Background(), reviewFindingInput("canonical"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`UPDATE findings SET message=? WHERE project_id=? AND finding_key=?`, "hand divergent", s.projectID, finding.Key); err != nil {
		t.Fatal(err)
	}
	report, err := s.Check(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, item := range report.Findings {
		if item.Code == "E_FINDING_INDEX_DIVERGENCE" {
			found = true
		}
	}
	if !found {
		t.Fatalf("divergence was healed before reporting: %#v", report)
	}
	got, err := s.GetFinding(finding.Key)
	if err != nil {
		t.Fatal(err)
	}
	if got.Finding.Message != "canonical" {
		t.Fatalf("rebuild did not heal after reporting: %#v", got)
	}
}

func TestLegacyFindingsMigrationIsIdempotentAndTyped(t *testing.T) {
	base := persistentTemp(t, "finding-migration")
	root := filepath.Join(base, "main")
	state := filepath.Join(base, "state")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(state, 0o755); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", filepath.Join(state, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	_, err = db.Exec(`
		CREATE TABLE projects (project_id TEXT PRIMARY KEY, slug TEXT NOT NULL, common_dir TEXT NOT NULL, config_digest TEXT NOT NULL DEFAULT '', created_at TEXT NOT NULL);
		INSERT INTO projects VALUES ('project-aira','aira','/legacy','', 'now');
		CREATE TABLE findings (project_id TEXT NOT NULL, finding_key TEXT NOT NULL, code TEXT NOT NULL, subject TEXT NOT NULL, details TEXT NOT NULL, created_at TEXT NOT NULL, PRIMARY KEY(project_id,finding_key));
		INSERT INTO findings VALUES ('project-aira','legacy','E_OLD','subject','details','now')`)
	if err != nil {
		t.Fatal(err)
	}
	_ = db.Close()
	s := openTestStore(t, root, filepath.Join(base, "common"), state, "main", "AIRA")
	var subtype, message string
	if err := s.db.QueryRow(`SELECT subtype,message FROM findings WHERE finding_key='legacy'`).Scan(&subtype, &message); err != nil {
		t.Fatal(err)
	}
	if subtype != "reconciliation" || message != "details" {
		t.Fatalf("migrated row subtype=%q message=%q", subtype, message)
	}
	_ = s.Close()
	if _, err := Open(context.Background(), Options{Root: root, CommonDir: filepath.Join(base, "common"), DBPath: filepath.Join(state, "state.db"), RegistryPath: filepath.Join(state, "registry2.jsonl"), ProjectID: "project-aira", WorktreeID: "main", ProjectSlug: "aira", Prefixes: []string{"AIRA"}}); err != nil {
		t.Fatal(err)
	}
}

func TestFindingsMigrationCrashAfterCreateIsAtomicAndReentrant(t *testing.T) {
	base := persistentTemp(t, "finding-migration-crash-create")
	root := filepath.Join(base, "main")
	state := filepath.Join(base, "state")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	createLegacyFindingsDB(t, filepath.Join(state, "state.db"))

	db, err := sql.Open("sqlite", filepath.Join(state, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	s := &Store{db: db, findingsMigrationHook: func(statement string) error {
		if statement == "after-create" {
			return errors.New("injected migration crash after create")
		}
		return nil
	}}
	err = s.ensureFindingsSchema(context.Background())
	if err == nil {
		t.Fatal("migration unexpectedly completed; crash hook was not exercised")
	}
	_ = s.Close()

	reopened := openTestStore(t, root, filepath.Join(base, "common"), state, "main", "AIRA")
	assertLegacyFindingSurvivedMigration(t, reopened)
}

func TestFindingsMigrationCrashAfterDropIsAtomicAndReentrant(t *testing.T) {
	base := persistentTemp(t, "finding-migration-crash-drop")
	root := filepath.Join(base, "main")
	state := filepath.Join(base, "state")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	createLegacyFindingsDB(t, filepath.Join(state, "state.db"))

	db, err := sql.Open("sqlite", filepath.Join(state, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	s := &Store{db: db, findingsMigrationHook: func(statement string) error {
		if statement == "after-drop" {
			return errors.New("injected migration crash after drop")
		}
		return nil
	}}
	err = s.ensureFindingsSchema(context.Background())
	if err == nil {
		t.Fatal("migration unexpectedly completed; crash hook was not exercised")
	}
	_ = s.Close()

	reopened := openTestStore(t, root, filepath.Join(base, "common"), state, "main", "AIRA")
	assertLegacyFindingSurvivedMigration(t, reopened)
}

func createLegacyFindingsDB(t *testing.T, path string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(`
		CREATE TABLE projects (project_id TEXT PRIMARY KEY, slug TEXT NOT NULL, common_dir TEXT NOT NULL, config_digest TEXT NOT NULL DEFAULT '', created_at TEXT NOT NULL);
		INSERT INTO projects VALUES ('project-aira','aira','/legacy','', 'now');
		CREATE TABLE findings (project_id TEXT NOT NULL, finding_key TEXT NOT NULL, code TEXT NOT NULL, subject TEXT NOT NULL, details TEXT NOT NULL, created_at TEXT NOT NULL, PRIMARY KEY(project_id,finding_key));
		INSERT INTO findings VALUES ('project-aira','legacy','E_OLD','subject','details','now')`); err != nil {
		t.Fatal(err)
	}
}

func assertLegacyFindingSurvivedMigration(t *testing.T, s *Store) {
	t.Helper()
	var count int
	if err := s.db.QueryRow(`SELECT count(*) FROM findings WHERE finding_key='legacy'`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("legacy row lost after migration crash recovery: count=%d", count)
	}
	if err := s.db.QueryRow(`SELECT count(*) FROM sqlite_master WHERE type='table' AND name='findings_m5'`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatalf("orphan findings_m5 table remains after migration recovery")
	}
}

func TestFindingReadbackRejectsCorruptReviewRow(t *testing.T) {
	base := persistentTemp(t, "finding-readback")
	root := filepath.Join(base, "main")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	s := testStore(t, root, filepath.Join(base, "common"), filepath.Join(base, "state"))
	_, err := s.db.Exec(`INSERT INTO findings(project_id,worktree_id,finding_key,subtype,code,subject,details,created_at,ticket_id,category,severity,verdict,disposition,source,file,line,requirement_id,waiver_reason,waiver_actor,canonical_file,message) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`, s.projectID, s.worktreeID, "bad", "review", "", "", "", "now", "AIRA-1", "bad category", "P0", "confirmed", "open", "codex", "x.go", 1, "", "", "", "x.go", "message")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.GetFinding("bad"); ErrorCode(err) != "E_FINDING_INVALID" {
		t.Fatalf("corrupt readback error=%v", err)
	}
}

func TestFindingReadbackRejectsSubtypeSpecificFields(t *testing.T) {
	base := persistentTemp(t, "finding-subtype-readback")
	root := filepath.Join(base, "main")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	s := testStore(t, root, filepath.Join(base, "common"), filepath.Join(base, "state"))
	insert := func(key, subtype, code, subject, details, category string) {
		t.Helper()
		_, err := s.db.Exec(`INSERT INTO findings(project_id,worktree_id,finding_key,subtype,code,subject,details,created_at,ticket_id,category,severity,verdict,disposition,source,file,line,requirement_id,waiver_reason,waiver_actor,canonical_file,message) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
			s.projectID, s.worktreeID, key, subtype, code, subject, details, "now", "", category, "", "", "open", "", "", 0, "", "", "", "", details)
		if err != nil {
			t.Fatal(err)
		}
	}
	review, err := domain.NewReviewFinding(reviewFindingInput("details"))
	if err != nil {
		t.Fatal(err)
	}
	_, err = s.db.Exec(`INSERT INTO findings(project_id,worktree_id,finding_key,subtype,code,subject,details,created_at,ticket_id,category,severity,verdict,disposition,source,file,line,requirement_id,waiver_reason,waiver_actor,canonical_file,message) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		s.projectID, s.worktreeID, review.Key, "review", "E_GIT_SCAN", ".git", "details", "now", review.TicketID, review.Category, review.Severity, review.Verdict, review.Disposition, review.Source, review.File, review.Line, review.RequirementID, review.WaiverReason, review.WaiverActor, review.File, review.Message)
	if err != nil {
		t.Fatal(err)
	}
	insert("r-bad-reconciliation-fields", "reconciliation", "E_GIT_SCAN", ".git", "details", "category")
	if _, err := s.indexedFinding(review.Key, s.worktreeID); ErrorCode(err) != "E_FINDING_INVALID" {
		t.Fatalf("indexedFinding(review) error=%v, want E_FINDING_INVALID", err)
	}
	if _, err := s.GetFinding("r-bad-reconciliation-fields"); ErrorCode(err) != "E_FINDING_INVALID" {
		t.Fatalf("GetFinding(reconciliation) error=%v, want E_FINDING_INVALID", err)
	}
}

func TestFindingMutationSerialisesWithRebuildScanAndReconstruct(t *testing.T) {
	base := persistentTemp(t, "finding-rebuild-race")
	root := filepath.Join(base, "main")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	s := testStore(t, root, filepath.Join(base, "common"), filepath.Join(base, "state"))
	old, _, err := s.AddFinding(context.Background(), reviewFindingInput("old content"))
	if err != nil {
		t.Fatal(err)
	}
	scanned := make(chan struct{})
	release := make(chan struct{})
	s.beforeRebuildFindingReconstruct = func() {
		close(scanned)
		<-release
	}
	rebuildDone := make(chan error, 1)
	go func() { rebuildDone <- s.Rebuild(context.Background()) }()
	select {
	case <-scanned:
	case <-time.After(5 * time.Second):
		t.Fatal("Rebuild did not reach the finding scan/reconstruct boundary")
	}
	mutationDone := make(chan error, 1)
	updatedInput := reviewFindingInput("new content")
	go func() {
		_, _, mutationErr := s.AddFinding(context.Background(), updatedInput)
		mutationDone <- mutationErr
	}()
	select {
	case mutationErr := <-mutationDone:
		close(release)
		<-rebuildDone
		t.Fatalf("finding mutation completed while Rebuild held the scan snapshot: %v", mutationErr)
	case <-time.After(150 * time.Millisecond):
	}
	close(release)
	if err := <-rebuildDone; err != nil {
		t.Fatal(err)
	}
	if err := <-mutationDone; err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(s.findingPath(old.Key))
	if err != nil {
		t.Fatal(err)
	}
	gotFile, err := domain.ParseFinding(data)
	if err != nil {
		t.Fatal(err)
	}
	if gotFile.Message != "new content" {
		t.Fatalf("canonical finding message=%q", gotFile.Message)
	}
	var indexedMessage string
	if err := s.db.QueryRow(`SELECT message FROM findings WHERE project_id=? AND worktree_id=? AND finding_key=?`, s.projectID, s.worktreeID, old.Key).Scan(&indexedMessage); err != nil {
		t.Fatal(err)
	}
	if indexedMessage != "new content" {
		t.Fatalf("indexed finding message=%q", indexedMessage)
	}
}

func TestRebuildKeepsSameReviewKeySeparatePerWorktree(t *testing.T) {
	base := persistentTemp(t, "finding-worktrees")
	root := filepath.Join(base, "main")
	sibling := filepath.Join(base, "sibling")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(sibling, ".aira", "findings"), 0o755); err != nil {
		t.Fatal(err)
	}
	s := testStore(t, root, filepath.Join(base, "common"), filepath.Join(base, "state"))
	finding, _, err := s.AddFinding(context.Background(), reviewFindingInput("main content"))
	if err != nil {
		t.Fatal(err)
	}
	other := reviewFindingInput("sibling content")
	otherFinding, err := domain.NewReviewFinding(other)
	if err != nil {
		t.Fatal(err)
	}
	data, err := domain.RenderFinding(otherFinding)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sibling, ".aira", "findings", finding.Key+".md"), data, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := s.RegisterWorktree(context.Background(), "sibling", sibling); err != nil {
		t.Fatal(err)
	}
	if err := s.Rebuild(context.Background()); err != nil {
		t.Fatal(err)
	}
	var count int
	if err := s.db.QueryRow(`SELECT count(*) FROM findings WHERE project_id=? AND subtype='review' AND finding_key=?`, s.projectID, finding.Key).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 2 {
		t.Fatalf("same key across worktrees count=%d", count)
	}
	var mainMessage, siblingMessage string
	if err := s.db.QueryRow(`SELECT message FROM findings WHERE project_id=? AND subtype='review' AND finding_key=? AND worktree_id=?`, s.projectID, finding.Key, s.worktreeID).Scan(&mainMessage); err != nil {
		t.Fatal(err)
	}
	if err := s.db.QueryRow(`SELECT message FROM findings WHERE project_id=? AND subtype='review' AND finding_key=? AND worktree_id=?`, s.projectID, finding.Key, "sibling").Scan(&siblingMessage); err != nil {
		t.Fatal(err)
	}
	if mainMessage != "main content" || siblingMessage != "sibling content" {
		t.Fatalf("worktree contents main=%q sibling=%q", mainMessage, siblingMessage)
	}
}

func TestMalformedFindingIsUnevaluatedAndSurvivesRebuild(t *testing.T) {
	base := persistentTemp(t, "finding-malformed")
	root := filepath.Join(base, "main")
	path := filepath.Join(root, ".aira", "findings", "f-bad.md")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	s := testStore(t, root, filepath.Join(base, "common"), filepath.Join(base, "state"))
	if err := os.WriteFile(path, []byte("not frontmatter\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	report, err := s.Check(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if report.Verdict != "unevaluated" || len(report.UnevaluatedFindings) == 0 {
		t.Fatalf("malformed finding report=%#v", report)
	}
	rows, err := s.ListFindings("")
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, row := range rows {
		if row.Unevaluated && row.Path == ".aira/findings/f-bad.md" {
			found = true
		}
	}
	if !found {
		t.Fatalf("malformed finding disappeared from query: %#v", rows)
	}
}

func TestMalformedFindingMatchesExplicitReviewFilter(t *testing.T) {
	base := persistentTemp(t, "finding-malformed-review-filter")
	root := filepath.Join(base, "main")
	path := filepath.Join(root, ".aira", "findings", "f-bad.md")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	s := testStore(t, root, filepath.Join(base, "common"), filepath.Join(base, "state"))
	if err := os.WriteFile(path, []byte("not frontmatter\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	rows, err := s.ListFindings("subtype:review")
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || !rows[0].Unevaluated || rows[0].Path != ".aira/findings/f-bad.md" {
		t.Fatalf("malformed review finding hidden by subtype filter: %#v", rows)
	}
}

func TestGetFindingWarnsOnDispositionOnlyIndexDivergence(t *testing.T) {
	base := persistentTemp(t, "finding-stale-disposition")
	root := filepath.Join(base, "main")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	s := testStore(t, root, filepath.Join(base, "common"), filepath.Join(base, "state"))
	finding, _, err := s.AddFinding(context.Background(), reviewFindingInput("same content"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`UPDATE findings SET disposition=? WHERE project_id=? AND worktree_id=? AND finding_key=?`, domain.DispositionFixed, s.projectID, s.worktreeID, finding.Key); err != nil {
		t.Fatal(err)
	}
	got, err := s.GetFinding(finding.Key)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Warnings) != 1 || got.Warnings[0] != "W_STALE_INDEX" {
		t.Fatalf("disposition-only index divergence warnings=%#v", got.Warnings)
	}
}
