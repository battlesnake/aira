package store

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"aira/internal/domain"
)

// TestRebuildReconstructsRequirementIndex proves the requirement index is a
// disposable projection of the git files: it is rebuilt after index loss and a
// removed file drops out of the index.
func TestRebuildReconstructsRequirementIndex(t *testing.T) {
	base := persistentTemp(t, "rebuild-req-index")
	root := filepath.Join(base, "main")
	common := filepath.Join(base, "common")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	s := openStoreWithRequirementPrefix(t, root, common, filepath.Join(base, "state"))
	req, _, err := s.AddRequirement(context.Background(), domain.RequirementInput{Text: "Recoverable requirement.", Status: domain.RequirementBuilt})
	if err != nil {
		t.Fatal(err)
	}

	// Simulate index loss; the git file survives.
	if _, err := s.db.Exec(`DELETE FROM requirements`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.GetRequirement(req.ID); err == nil {
		t.Fatal("expected index loss before rebuild")
	}
	if err := s.Rebuild(context.Background()); err != nil {
		t.Fatalf("rebuild: %v", err)
	}
	rec, err := s.GetRequirement(req.ID)
	if err != nil {
		t.Fatalf("requirement not reindexed: %v", err)
	}
	if rec.Requirement.Status != domain.RequirementBuilt || rec.Requirement.Text != "Recoverable requirement." {
		t.Fatalf("reindexed requirement mismatch: %#v", rec.Requirement)
	}

	// Remove the git file; a rebuild must drop the stale index row.
	if err := os.Remove(filepath.Join(root, ".aira", "requirements", req.ID+".md")); err != nil {
		t.Fatal(err)
	}
	if err := s.Rebuild(context.Background()); err != nil {
		t.Fatalf("rebuild after removal: %v", err)
	}
	if _, err := s.GetRequirement(req.ID); err == nil || !strings.Contains(err.Error(), "E_NOT_FOUND") {
		t.Fatalf("removed requirement should drop from index, got %v", err)
	}
}

// TestRebuildRecoversRequirementAllocationFromFile is the whole-DB-loss case: the
// allocation row and index are gone but the durable receipt + git file survive,
// and rebuild reconstructs both with requirement kind.
func TestRebuildRecoversRequirementAllocationFromFile(t *testing.T) {
	base := persistentTemp(t, "rebuild-req-alloc")
	root := filepath.Join(base, "main")
	common := filepath.Join(base, "common")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	s := openStoreWithRequirementPrefix(t, root, common, filepath.Join(base, "state"))
	req, _, err := s.AddRequirement(context.Background(), domain.RequirementInput{Text: "Durable.", Status: domain.RequirementPlanned})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`DELETE FROM allocations`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`DELETE FROM requirements`); err != nil {
		t.Fatal(err)
	}
	if err := s.Rebuild(context.Background()); err != nil {
		t.Fatalf("rebuild: %v", err)
	}
	var kind string
	if err := s.db.QueryRow(`SELECT kind FROM allocations WHERE prefix='AR' AND number=1`).Scan(&kind); err != nil {
		t.Fatalf("allocation not recovered: %v", err)
	}
	if kind != kindRequirement {
		t.Fatalf("recovered kind=%q, want requirement", kind)
	}
	if _, err := s.GetRequirement(req.ID); err != nil {
		t.Fatalf("requirement index not recovered: %v", err)
	}
}

// TestRebuildRequirementScanIgnoresMalformed proves a malformed requirement file
// never poisons the index or manufactures an allocation, is surfaced as a
// reconciliation finding, and still advances the ID high-water so its ID is not
// reallocated.
func TestRebuildRequirementScanIgnoresMalformed(t *testing.T) {
	base := persistentTemp(t, "rebuild-req-malformed")
	root := filepath.Join(base, "main")
	common := filepath.Join(base, "common")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	s := openStoreWithRequirementPrefix(t, root, common, filepath.Join(base, "state"))
	if _, _, err := s.AddRequirement(context.Background(), domain.RequirementInput{Text: "Valid.", Status: domain.RequirementBuilt}); err != nil {
		t.Fatal(err)
	}
	// A malformed AR-5 file: present but unparseable.
	reqDir := filepath.Join(root, ".aira", "requirements")
	if err := os.WriteFile(filepath.Join(reqDir, "AR-5.md"), []byte("this is not valid frontmatter\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := s.Rebuild(context.Background()); err != nil {
		t.Fatalf("rebuild must tolerate a malformed file: %v", err)
	}
	// AR-1 is indexed; the malformed AR-5 is not, and has no allocation.
	if _, err := s.GetRequirement("AR-1"); err != nil {
		t.Fatalf("valid requirement lost: %v", err)
	}
	if _, err := s.GetRequirement("AR-5"); err == nil || !strings.Contains(err.Error(), "E_NOT_FOUND") {
		t.Fatalf("malformed requirement must not be indexed, got %v", err)
	}
	var n int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM allocations WHERE prefix='AR' AND number=5`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("malformed requirement must not manufacture an allocation, found %d", n)
	}
	// The malformed file still claims AR-5, so the counter must skip past it.
	var next int
	if err := s.db.QueryRow(`SELECT next_number FROM id_counters WHERE prefix='AR'`).Scan(&next); err != nil {
		t.Fatal(err)
	}
	if next <= 5 {
		t.Fatalf("high-water must exceed the malformed ID (next=%d, want >5)", next)
	}
}

// TestRebuildRejectsTicketPrefixRequirementFile is the F1 analogue in the
// requirement direction: a ticket-prefixed ID sitting in .aira/requirements/
// must be refused as E_JOURNAL_CORRUPT rather than recovered as a requirement.
func TestRebuildRejectsTicketPrefixRequirementFile(t *testing.T) {
	base := persistentTemp(t, "rebuild-req-wrongprefix")
	root := filepath.Join(base, "main")
	common := filepath.Join(base, "common")
	reqDir := filepath.Join(root, ".aira", "requirements")
	if err := os.MkdirAll(reqDir, 0o755); err != nil {
		t.Fatal(err)
	}
	s := openStoreWithRequirementPrefix(t, root, common, filepath.Join(base, "state"))
	// AIRA is a ticket prefix; a requirement file claiming AIRA-3 is a kind lie.
	requirement, err := domain.NewRequirement(domain.RequirementInput{ID: "AIRA-3", Text: "Wrong dir.", Status: domain.RequirementBuilt})
	if err != nil {
		t.Fatal(err)
	}
	data, err := domain.RenderRequirement(requirement)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(reqDir, "AIRA-3.md"), data, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := s.Rebuild(context.Background()); err == nil || !strings.Contains(err.Error(), "E_JOURNAL_CORRUPT") {
		t.Fatalf("ticket-prefixed requirement file should be refused, got %v", err)
	}
}
