package store

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"aira/internal/domain"
)

func TestAddRequirementMaterialisesFileAndIndex(t *testing.T) {
	base := persistentTemp(t, "add-requirement")
	root := filepath.Join(base, "main")
	common := filepath.Join(base, "common")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	s := openStoreWithRequirementPrefix(t, root, common, filepath.Join(base, "state"))

	req, _, err := s.AddRequirement(context.Background(), domain.RequirementInput{Text: "The system must X.", Status: domain.RequirementBuilt})
	if err != nil {
		t.Fatalf("add requirement: %v", err)
	}
	if req.ID != "AR-1" {
		t.Fatalf("id=%s, want AR-1", req.ID)
	}

	// The git file exists under .aira/requirements/ and round-trips.
	data, err := os.ReadFile(filepath.Join(root, ".aira", "requirements", "AR-1.md"))
	if err != nil {
		t.Fatalf("requirement file: %v", err)
	}
	parsed, err := domain.ParseRequirement(data)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if parsed.ID != "AR-1" || parsed.Status != domain.RequirementBuilt || parsed.Text != "The system must X." {
		t.Fatalf("file mismatch: %#v", parsed)
	}

	// The allocation is materialised and requirement-kind.
	var state, kind string
	if err := s.db.QueryRow(`SELECT state, kind FROM allocations WHERE prefix='AR' AND number=1`).Scan(&state, &kind); err != nil {
		t.Fatal(err)
	}
	if state != "materialised" || kind != kindRequirement {
		t.Fatalf("allocation state=%q kind=%q", state, kind)
	}

	// Get/List reflect the index.
	rec, err := s.GetRequirement("AR-1")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if rec.Requirement.Status != domain.RequirementBuilt || rec.Requirement.Text != "The system must X." {
		t.Fatalf("get mismatch: %#v", rec.Requirement)
	}
	list, err := s.ListRequirements()
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 || list[0].Requirement.ID != "AR-1" {
		t.Fatalf("list=%#v", list)
	}

	// The counter advances for the next requirement.
	req2, _, err := s.AddRequirement(context.Background(), domain.RequirementInput{Text: "Second.", Status: domain.RequirementPlanned})
	if err != nil {
		t.Fatal(err)
	}
	if req2.ID != "AR-2" {
		t.Fatalf("second id=%s, want AR-2", req2.ID)
	}
}

func TestAddRequirementRejectsInvalid(t *testing.T) {
	base := persistentTemp(t, "add-requirement-invalid")
	root := filepath.Join(base, "main")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	s := openStoreWithRequirementPrefix(t, root, filepath.Join(base, "common"), filepath.Join(base, "state"))
	if _, _, err := s.AddRequirement(context.Background(), domain.RequirementInput{Text: "  ", Status: domain.RequirementBuilt}); err == nil || !strings.Contains(err.Error(), "E_REQUIREMENT_INVALID") {
		t.Fatalf("empty text: expected E_REQUIREMENT_INVALID, got %v", err)
	}
	if _, _, err := s.AddRequirement(context.Background(), domain.RequirementInput{Text: "x", Status: "bogus"}); err == nil || !strings.Contains(err.Error(), "E_REQUIREMENT_INVALID") {
		t.Fatalf("bad status: expected E_REQUIREMENT_INVALID, got %v", err)
	}
}

func TestSetRequirementChangesStatusPreservingText(t *testing.T) {
	base := persistentTemp(t, "set-requirement")
	root := filepath.Join(base, "main")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	s := openStoreWithRequirementPrefix(t, root, filepath.Join(base, "common"), filepath.Join(base, "state"))

	req, _, err := s.AddRequirement(context.Background(), domain.RequirementInput{Text: "Line one.\nLine two.", Status: domain.RequirementPlanned})
	if err != nil {
		t.Fatalf("add: %v", err)
	}

	if _, err := s.SetRequirement(context.Background(), req.ID, domain.RequirementBuilt); err != nil {
		t.Fatalf("set: %v", err)
	}

	// Index reflects the new status; text is preserved verbatim.
	rec, err := s.GetRequirement(req.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if rec.Requirement.Status != domain.RequirementBuilt {
		t.Fatalf("status=%q, want built", rec.Requirement.Status)
	}
	if rec.Requirement.Text != "Line one.\nLine two." {
		t.Fatalf("text mutated: %q", rec.Requirement.Text)
	}

	// The git file round-trips with the new status and same body.
	data, err := os.ReadFile(filepath.Join(root, ".aira", "requirements", req.ID+".md"))
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := domain.ParseRequirement(data)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if parsed.Status != domain.RequirementBuilt || parsed.Text != "Line one.\nLine two." {
		t.Fatalf("file mismatch: %#v", parsed)
	}

	// A set does not allocate: the allocation stays materialised, requirement-kind.
	var state, kind string
	if err := s.db.QueryRow(`SELECT state, kind FROM allocations WHERE prefix='AR' AND number=1`).Scan(&state, &kind); err != nil {
		t.Fatal(err)
	}
	if state != "materialised" || kind != kindRequirement {
		t.Fatalf("allocation state=%q kind=%q", state, kind)
	}
}

func TestSetRequirementRejectsInvalidAndMissing(t *testing.T) {
	base := persistentTemp(t, "set-requirement-invalid")
	root := filepath.Join(base, "main")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	s := openStoreWithRequirementPrefix(t, root, filepath.Join(base, "common"), filepath.Join(base, "state"))

	req, _, err := s.AddRequirement(context.Background(), domain.RequirementInput{Text: "Requirement.", Status: domain.RequirementPlanned})
	if err != nil {
		t.Fatalf("add: %v", err)
	}
	if _, err := s.SetRequirement(context.Background(), req.ID, "bogus"); err == nil || !strings.Contains(err.Error(), "E_REQUIREMENT_INVALID") {
		t.Fatalf("bad status: expected E_REQUIREMENT_INVALID, got %v", err)
	}
	if _, err := s.SetRequirement(context.Background(), "AR-99", domain.RequirementBuilt); err == nil || !strings.Contains(err.Error(), "E_NOT_FOUND") {
		t.Fatalf("missing: expected E_NOT_FOUND, got %v", err)
	}
	// The rejected set left the requirement untouched.
	rec, err := s.GetRequirement(req.ID)
	if err != nil {
		t.Fatal(err)
	}
	if rec.Requirement.Status != domain.RequirementPlanned {
		t.Fatalf("status changed to %q despite rejected set", rec.Requirement.Status)
	}
}

func TestGetRequirementNotFound(t *testing.T) {
	base := persistentTemp(t, "get-requirement-nf")
	root := filepath.Join(base, "main")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	s := openStoreWithRequirementPrefix(t, root, filepath.Join(base, "common"), filepath.Join(base, "state"))
	if _, err := s.GetRequirement("AR-99"); err == nil || !strings.Contains(err.Error(), "E_NOT_FOUND") {
		t.Fatalf("expected E_NOT_FOUND, got %v", err)
	}
}
