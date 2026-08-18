package store

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"aira/internal/domain"
	"aira/internal/gate"
)

func TestOpenReadOnlyDoesNotRegisterReadsWALAndRejectsWritesLoudly(t *testing.T) {
	base := t.TempDir()
	root := filepath.Join(base, "root")
	common := filepath.Join(base, "common")
	gitDir := filepath.Join(common, "worktrees", "main")
	for _, path := range []string{root, common, gitDir} {
		if err := os.MkdirAll(path, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	projectID, worktreeID, err := CanonicalScopeIdentity(common, gitDir)
	if err != nil {
		t.Fatal(err)
	}
	dbPath := filepath.Join(base, "state", "state.db")
	registryPath := filepath.Join(base, "state", "registry.jsonl")
	db, err := OpenDB(dbPath, registryPath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	opts := ScopeOptions{
		Root: root, CommonDir: common, GitDir: gitDir, ProjectID: projectID, WorktreeID: worktreeID,
		ProjectSlug: "demo", Prefixes: []string{"DEMO"}, ConfigDigest: "fixture",
	}
	definition := gate.GateDefinition{
		SchemaVersion: 1, ID: "traceability", Name: "Traceability", Kind: gate.KindCheckable,
		AppliesTo: gate.AppliesTo{All: true}, Lane: gate.Lane{Name: "local", Checker: "check-dimension", EvaluatorVersion: "1"},
		ProofPolicy: gate.ProofPolicy{Mode: gate.ProofRequired, MaxAgeSecs: 604800, RequireCurrentCanary: true},
		CanaryIDs:   []string{"fixture"}, Checkable: &gate.Checkable{Dimension: "traceability"}, Enabled: true,
	}
	definitionData, err := gate.RenderGate(definition)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, ".aira", "gates"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, ".aira", "gates", "traceability.json"), definitionData, 0o644); err != nil {
		t.Fatal(err)
	}
	ro, err := OpenReadOnly(dbPath, opts)
	if err != nil {
		t.Fatal(err)
	}
	defer ro.Close()
	var projects int
	if err := ro.db.QueryRow(`SELECT COUNT(*) FROM projects`).Scan(&projects); err != nil {
		t.Fatal(err)
	}
	if projects != 0 {
		t.Fatalf("read-only open registered %d projects", projects)
	}
	gates, err := ro.ListGates()
	if err != nil || len(gates) != 1 || gates[0].ID != definition.ID {
		t.Fatalf("read-only gate definitions=%+v err=%v", gates, err)
	}
	if reportContext := ro.TestReportContext(context.Background()); reportContext.WorktreeID != worktreeID {
		t.Fatalf("read-only report context=%+v", reportContext)
	}
	if _, err := os.Stat(registryPath); !os.IsNotExist(err) {
		t.Fatalf("read-only open created registry: %v", err)
	}
	writer, err := NewScope(db, opts)
	if err != nil {
		t.Fatal(err)
	}
	zero, wall := int64(0), int64(9)
	if _, err := writer.AddCommandEvent(context.Background(), domain.CommandEventInput{
		Key: "go test", KeySource: domain.CommandKeyProgramSubcommand, Program: "go",
		ArgvDigest: strings.Repeat("a", 64), Status: domain.CommandExited, ExitCode: &zero, WallMS: &wall,
	}); err != nil {
		t.Fatal(err)
	}
	rows, err := ro.ListCommandEvents("")
	if err != nil || len(rows) != 1 || rows[0].WallMS == nil || *rows[0].WallMS != wall {
		t.Fatalf("WAL read rows=%+v err=%v", rows, err)
	}
	if err := ro.Register(context.Background()); err == nil {
		t.Fatal("read-only Register succeeded")
	}
	if _, err := ro.AddCommandEvent(context.Background(), domain.CommandEventInput{
		Key: "go vet", KeySource: domain.CommandKeyProgramSubcommand, Program: "go",
		ArgvDigest: strings.Repeat("b", 64), Status: domain.CommandExited, ExitCode: &zero, WallMS: &wall,
	}); err == nil {
		t.Fatal("read-only AddCommandEvent succeeded")
	}
	if _, err := ro.Check(context.Background()); err == nil {
		t.Fatal("read-only Check succeeded even though check refreshes projections")
	}
	if rows, err := writer.ListCommandEvents(""); err != nil || len(rows) != 1 {
		t.Fatalf("read-only write changed rows=%+v err=%v", rows, err)
	}
}

func TestOpenReadOnlyRequiresExistingDatabase(t *testing.T) {
	base := t.TempDir()
	root, common, gitDir := filepath.Join(base, "root"), filepath.Join(base, "common"), filepath.Join(base, "git")
	for _, path := range []string{root, common, gitDir} {
		if err := os.MkdirAll(path, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	_, err := OpenReadOnly(filepath.Join(base, "missing.db"), ScopeOptions{Root: root, CommonDir: common, GitDir: gitDir, ProjectSlug: "demo"})
	if err == nil {
		t.Fatal("missing read-only DB was created")
	}
	if _, statErr := os.Stat(filepath.Join(base, "missing.db")); !os.IsNotExist(statErr) {
		t.Fatalf("missing DB side effect = %v", statErr)
	}
}

func TestOpenReadOnlyServesRatchetTestReportReadsFromWAL(t *testing.T) {
	base, root := t.TempDir(), filepath.Join(t.TempDir(), "root")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	definition := ratchetTestGate(t, root)
	gitRun(t, root, "init", "-q")
	gitRun(t, root, "config", "user.email", "aira@example.test")
	gitRun(t, root, "config", "user.name", "AIRA")
	gitRun(t, root, "add", ".")
	gitRun(t, root, "commit", "-qm", "ratchet read-only fixture")
	common, gitDir := filepath.Join(root, ".git"), filepath.Join(root, ".git")
	projectID, worktreeID, err := CanonicalScopeIdentity(common, gitDir)
	if err != nil {
		t.Fatal(err)
	}
	dbPath := filepath.Join(base, "state", "state.db")
	db, err := OpenDB(dbPath, filepath.Join(base, "state", "registry.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	opts := ScopeOptions{
		Root: root, CommonDir: common, GitDir: gitDir, ProjectID: projectID, WorktreeID: worktreeID,
		ProjectSlug: "demo", Prefixes: []string{"AIRA"}, ConfigDigest: "fixture",
	}
	writer, err := NewScope(db, opts)
	if err != nil {
		t.Fatal(err)
	}
	commit := writer.gitValue(context.Background(), "HEAD")
	baseline := addRatchetReportWithResults(t, writer, commit, []domain.TestResult{{Name: "A", Outcome: domain.OutcomeFail}})
	if _, err := writer.PinGateBaseline(context.Background(), definition.ID, []string{baseline.ID}, "test", "read-only relay fixture"); err != nil {
		t.Fatal(err)
	}
	reader, err := OpenReadOnly(dbPath, opts)
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	addRatchetReportInput(t, writer, domain.TestReportInput{
		Format: "junit", Commit: commit, SuiteID: "unit", Config: "default", EnvDigest: "env", Shard: "1/1",
		ParserComplete: true, SourceDigest: "current-after-reader-open",
		Results: []domain.TestResult{{Name: "A", Outcome: domain.OutcomeFail}, {Name: "B", Outcome: domain.OutcomePass}},
	})
	evaluation, err := reader.evaluateRatchet(context.Background(), definition, root)
	if err != nil || evaluation.Predicate != gate.PredicatePass || !evaluation.Evidence {
		t.Fatalf("read-only ratchet evaluation=%+v err=%v", evaluation, err)
	}
}
