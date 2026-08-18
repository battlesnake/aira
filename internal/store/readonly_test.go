package store

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"aira/internal/domain"
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
