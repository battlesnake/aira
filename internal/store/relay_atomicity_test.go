package store

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"aira/internal/domain"
)

func TestRelayedAppendCancellationBeforeCommitLeavesNoRow(t *testing.T) {
	base := t.TempDir()
	s := testStore(t, base, filepath.Join(base, "common"), filepath.Join(base, "state"))
	ctx, cancel := context.WithCancel(context.Background())
	s.beforeCommit = cancel
	zero, wall := int64(0), int64(1)
	_, err := s.AddCommandEvent(ctx, domain.CommandEventInput{
		Key: "go test", KeySource: domain.CommandKeyProgramSubcommand, Program: "go",
		ArgvDigest: strings.Repeat("a", 64), Status: domain.CommandExited, ExitCode: &zero, WallMS: &wall,
	})
	if err == nil {
		t.Fatal("cancelled append committed")
	}
	var rows int
	if queryErr := s.db.QueryRow(`SELECT COUNT(*) FROM command_events WHERE project_id=?`, s.projectID).Scan(&rows); queryErr != nil {
		t.Fatal(queryErr)
	}
	if rows != 0 {
		t.Fatalf("cancelled append left %d rows", rows)
	}
}

func TestCancelledRebuildPreservesPriorProjectionAndSubsequentRebuildRecovers(t *testing.T) {
	base := t.TempDir()
	root := filepath.Join(base, "root")
	if err := os.MkdirAll(filepath.Join(root, ".aira", "tickets"), 0o755); err != nil {
		t.Fatal(err)
	}
	gitRun(t, root, "init", "-q")
	writeTicketFile(t, filepath.Join(root, ".aira", "tickets", "AIRA-1.md"), "AIRA-1")
	s := testStore(t, root, filepath.Join(base, "common"), filepath.Join(base, "state"))
	if err := s.Rebuild(context.Background()); err != nil {
		t.Fatal(err)
	}
	writeTicketFile(t, filepath.Join(root, ".aira", "tickets", "AIRA-2.md"), "AIRA-2")
	ctx, cancel := context.WithCancel(context.Background())
	s.beforeCommit = cancel
	if err := s.Rebuild(ctx); err == nil {
		t.Fatal("cancelled rebuild committed")
	}
	var prior, added int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM tickets WHERE project_id=? AND id='AIRA-1'`, s.projectID).Scan(&prior); err != nil {
		t.Fatal(err)
	}
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM tickets WHERE project_id=? AND id='AIRA-2'`, s.projectID).Scan(&added); err != nil {
		t.Fatal(err)
	}
	if prior != 1 || added != 0 {
		t.Fatalf("cancelled rebuild tore projection: prior=%d added=%d", prior, added)
	}
	s.beforeCommit = nil
	if err := s.Rebuild(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM tickets WHERE project_id=? AND id='AIRA-2'`, s.projectID).Scan(&added); err != nil {
		t.Fatal(err)
	}
	if added != 1 {
		t.Fatalf("recovery rebuild did not reach fixed point: added=%d", added)
	}
}
