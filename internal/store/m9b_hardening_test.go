package store

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"aira/internal/domain"
)

// TestRebuildRejectsMisKindedEventWhenReceiptMissing is the Sol finding-1
// regression: when an allocation row survives but its receipt is absent, the
// scanned-file else-branch must still validate the allocation event against the
// reconciled kind, so a downgraded (ticket-form) requirement event cannot pass a
// rebuild undetected.
func TestRebuildRejectsMisKindedEventWhenReceiptMissing(t *testing.T) {
	base := persistentTemp(t, "rebuild-miskind-noreceipt")
	root := filepath.Join(base, "main")
	common := filepath.Join(base, "common")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	s := openStoreWithRequirementPrefix(t, root, common, filepath.Join(base, "state"))
	if _, _, err := s.AddRequirement(context.Background(), domain.RequirementInput{Text: "R.", Status: domain.RequirementBuilt}); err != nil {
		t.Fatal(err)
	}

	// Downgrade the journal event to the ticket (D0) digest, remove the receipt,
	// and clear the events index so the generic replay accepts the D0 event; the
	// allocation row survives so the else-branch is exercised.
	tamperJournalEventDigest(t, filepath.Join(common, "aira", "journal.jsonl"),
		"requirement.create", "AR-1", digestBytes([]byte("requirement.create\x00AR-1")))
	removeReceiptLine(t, filepath.Join(common, "aira", "receipts.jsonl"), "AR-1")
	if _, err := s.db.Exec(`DELETE FROM events`); err != nil {
		t.Fatal(err)
	}
	if err := s.Rebuild(context.Background()); err == nil || !strings.Contains(err.Error(), "E_JOURNAL_CORRUPT") {
		t.Fatalf("rebuild should reject a mis-kinded event even with the receipt missing, got %v", err)
	}
}

// TestCheckRejectsKindPathMismatchedAllocation is the Sol finding-2 regression:
// an allocation whose recorded kind disagrees with its path directory is an
// integrity fault, not something to resolve against the wrong entity type.
func TestCheckRejectsKindPathMismatchedAllocation(t *testing.T) {
	base := persistentTemp(t, "check-kindpath-mismatch")
	root := filepath.Join(base, "main")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	s := openStoreWithRequirementPrefix(t, root, filepath.Join(base, "common"), filepath.Join(base, "state"))
	if _, err := s.AllocateID(context.Background(), "AR"); err != nil {
		t.Fatal(err)
	}
	// Tamper only the allocations index path into the ticket directory while the
	// kind stays requirement (the receipt remains consistent, so rebuild's receipt
	// reconciliation passes and the mismatch survives to the check loop).
	ticketPath := filepath.Join(root, ".aira", "tickets", "AR-1.md")
	if _, err := s.db.Exec(`UPDATE allocations SET path=? WHERE prefix='AR' AND number=1`, ticketPath); err != nil {
		t.Fatal(err)
	}
	report, err := s.Check(context.Background())
	if err != nil {
		t.Fatalf("check: %v", err)
	}
	found := false
	for _, f := range report.Findings {
		if f.Subject == "AR-1" && f.Code == "E_JOURNAL_CORRUPT" {
			found = true
		}
	}
	if !found {
		t.Fatalf("check must flag the kind/path mismatch as E_JOURNAL_CORRUPT: %#v", report.Findings)
	}
	if report.Dimensions["allocated-id-file"] != "fail" {
		t.Fatalf("allocated-id-file dimension must fail: %#v", report.Dimensions)
	}
}

// TestRebuildFlagsRequirementDirectory is the Sol finding-3 regression: a
// directory shaped like an entity file must be flagged and reserve its ID, never
// silently skipped.
func TestRebuildFlagsRequirementDirectory(t *testing.T) {
	base := persistentTemp(t, "rebuild-req-dir")
	root := filepath.Join(base, "main")
	common := filepath.Join(base, "common")
	reqDir := filepath.Join(root, ".aira", "requirements")
	if err := os.MkdirAll(reqDir, 0o755); err != nil {
		t.Fatal(err)
	}
	s := openStoreWithRequirementPrefix(t, root, common, filepath.Join(base, "state"))
	if _, _, err := s.AddRequirement(context.Background(), domain.RequirementInput{Text: "Valid.", Status: domain.RequirementBuilt}); err != nil {
		t.Fatal(err)
	}
	// A directory named like a requirement file.
	if err := os.MkdirAll(filepath.Join(reqDir, "AR-5.md"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := s.Rebuild(context.Background()); err != nil {
		t.Fatalf("rebuild must tolerate a directory-shaped entry: %v", err)
	}
	// AR-5 is not indexed and has no allocation, but the high-water skipped past it.
	if _, err := s.GetRequirement("AR-5"); err == nil || !strings.Contains(err.Error(), "E_NOT_FOUND") {
		t.Fatalf("directory must not be indexed as a requirement, got %v", err)
	}
	var n int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM allocations WHERE prefix='AR' AND number=5`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("directory must not manufacture an allocation, found %d", n)
	}
	var next int
	if err := s.db.QueryRow(`SELECT next_number FROM id_counters WHERE prefix='AR'`).Scan(&next); err != nil {
		t.Fatal(err)
	}
	if next <= 5 {
		t.Fatalf("high-water must skip past the directory ID (next=%d, want >5)", next)
	}
}

func removeReceiptLine(t *testing.T, path, id string) {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		if strings.Contains(line, "\""+id+"\"") {
			continue
		}
		out = append(out, line)
	}
	body := ""
	if len(out) > 0 {
		body = strings.Join(out, "\n") + "\n"
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}
