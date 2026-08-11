package store

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"aira/internal/domain"
)

// TestCheckAllocatedRequirementFile is the constraint-A unit check: a
// requirement allocation is resolved against a requirement file, never parsed as
// a ticket.
func TestCheckAllocatedRequirementFile(t *testing.T) {
	base := persistentTemp(t, "check-alloc-req")
	root := filepath.Join(base, "main")
	reqDir := filepath.Join(root, ".aira", "requirements")
	if err := os.MkdirAll(reqDir, 0o755); err != nil {
		t.Fatal(err)
	}
	s := openStoreWithRequirementPrefix(t, root, filepath.Join(base, "common"), filepath.Join(base, "state"))

	valid, err := domain.NewRequirement(domain.RequirementInput{ID: "AR-1", Text: "R.", Status: domain.RequirementBuilt})
	if err != nil {
		t.Fatal(err)
	}
	data, err := domain.RenderRequirement(valid)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(reqDir, "AR-1.md")
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}

	// A present, valid requirement file resolves the allocation cleanly.
	report := CheckReport{Dimensions: map[string]string{}}
	if err := s.checkAllocatedRequirementFile(&report, "AR-1", path); err != nil {
		t.Fatalf("hard error: %v", err)
	}
	if len(report.Findings) != 0 || report.Dimensions["allocated-id-file"] == "fail" {
		t.Fatalf("valid requirement file should resolve cleanly: %#v", report)
	}

	// A missing file is E_ID_UNRESOLVED, and names the requirement file — proving
	// it is not a ticket-parse false fail.
	report = CheckReport{Dimensions: map[string]string{}}
	if err := s.checkAllocatedRequirementFile(&report, "AR-2", filepath.Join(reqDir, "AR-2.md")); err != nil {
		t.Fatal(err)
	}
	if report.Dimensions["allocated-id-file"] != "fail" || len(report.Findings) != 1 || report.Findings[0].Code != "E_ID_UNRESOLVED" {
		t.Fatalf("missing requirement file: %#v", report)
	}
	if !strings.Contains(report.Findings[0].Message, "requirement file") {
		t.Fatalf("expected requirement-file message, got %q", report.Findings[0].Message)
	}

	// A file whose content is a different ID fails as E_ID_UNRESOLVED.
	other, err := domain.NewRequirement(domain.RequirementInput{ID: "AR-9", Text: "X.", Status: domain.RequirementBuilt})
	if err != nil {
		t.Fatal(err)
	}
	otherData, err := domain.RenderRequirement(other)
	if err != nil {
		t.Fatal(err)
	}
	mismatchPath := filepath.Join(reqDir, "AR-3.md")
	if err := os.WriteFile(mismatchPath, otherData, 0o644); err != nil {
		t.Fatal(err)
	}
	report = CheckReport{Dimensions: map[string]string{}}
	if err := s.checkAllocatedRequirementFile(&report, "AR-3", mismatchPath); err != nil {
		t.Fatal(err)
	}
	if report.Dimensions["allocated-id-file"] != "fail" || len(report.Findings) != 1 {
		t.Fatalf("mismatched requirement id: %#v", report)
	}
}

// TestCheckResolvesCrashWindowRequirementAllocation is the constraint-A
// end-to-end regression: an allocated-but-not-yet-flipped requirement whose file
// is present must resolve through Check. The old ticket-only branch would parse
// the requirement file as a ticket and falsely report E_ID_UNRESOLVED.
func TestCheckResolvesCrashWindowRequirementAllocation(t *testing.T) {
	base := persistentTemp(t, "check-crashwindow-req")
	root := filepath.Join(base, "main")
	if err := os.MkdirAll(filepath.Join(root, ".aira", "requirements"), 0o755); err != nil {
		t.Fatal(err)
	}
	s := openStoreWithRequirementPrefix(t, root, filepath.Join(base, "common"), filepath.Join(base, "state"))

	// Reserve AR-1 (state='allocated'), then write its file — the crash window
	// between writeAtomic and markMaterialised.
	if _, err := s.AllocateID(context.Background(), "AR"); err != nil {
		t.Fatal(err)
	}
	valid, err := domain.NewRequirement(domain.RequirementInput{ID: "AR-1", Text: "Crash window.", Status: domain.RequirementBuilt})
	if err != nil {
		t.Fatal(err)
	}
	data, err := domain.RenderRequirement(valid)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, ".aira", "requirements", "AR-1.md"), data, 0o644); err != nil {
		t.Fatal(err)
	}

	report, err := s.Check(context.Background())
	if err != nil {
		t.Fatalf("check: %v", err)
	}
	for _, f := range report.Findings {
		if f.Subject == "AR-1" && f.Code == "E_ID_UNRESOLVED" {
			t.Fatalf("crash-window requirement falsely unresolved: %#v", f)
		}
	}
	if report.Dimensions["allocated-id-file"] == "fail" {
		t.Fatalf("allocated-id-file must not fail for a resolvable requirement: %#v", report.Dimensions)
	}
}
