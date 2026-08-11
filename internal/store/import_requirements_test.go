package store

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"

	"aira/internal/domain"
)

func writeRequirementImportFile(t *testing.T, path string, rows string) {
	t.Helper()
	data := "| ID | Requirement | Status | Implemented-by | Verified-by |\n" +
		"|---|---|---|---|---|\n" + rows
	if err := os.WriteFile(path, []byte(data), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestImportRequirementsFreshPreservesIDsAndAdvancesCounter(t *testing.T) {
	base := persistentTemp(t, "import-requirements-fresh")
	root := filepath.Join(base, "main")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	s := openStoreWithRequirementPrefix(t, root, filepath.Join(base, "common"), filepath.Join(base, "state"))
	input := filepath.Join(base, "REQUIREMENTS.md")
	writeRequirementImportFile(t, input,
		"| AR-1 | First requirement. | designed | — | — |\n"+
			"| AR-2 | Second requirement. | planned | — | — |\n"+
			"| AR-3 | Third requirement. | built | — | — |\n")

	summary, err := s.ImportRequirements(context.Background(), input)
	if err != nil {
		t.Fatalf("import: %v", err)
	}
	if !reflect.DeepEqual(summary.Created, []string{"AR-1", "AR-2", "AR-3"}) || summary.Total != 3 ||
		len(summary.Repaired) != 0 || len(summary.Unchanged) != 0 || len(summary.Updated) != 0 {
		t.Fatalf("summary=%+v", summary)
	}
	for _, id := range []string{"AR-1", "AR-2", "AR-3"} {
		data, err := os.ReadFile(filepath.Join(root, ".aira", "requirements", id+".md"))
		if err != nil {
			t.Fatalf("%s file: %v", id, err)
		}
		requirement, err := domain.ParseRequirement(data)
		if err != nil || requirement.ID != id {
			t.Fatalf("%s file=%#v err=%v", id, requirement, err)
		}
		var kind string
		if err := s.db.QueryRow(`SELECT kind FROM allocations WHERE prefix='AR' AND number=?`, numberOf(id)).Scan(&kind); err != nil {
			t.Fatal(err)
		}
		if kind != kindRequirement {
			t.Fatalf("%s allocation kind=%q", id, kind)
		}
		if _, err := s.GetRequirement(id); err != nil {
			t.Fatalf("%s index: %v", id, err)
		}
	}
	var next int64
	if err := s.db.QueryRow(`SELECT next_number FROM id_counters WHERE project_id=? AND prefix='AR'`, s.projectID).Scan(&next); err != nil {
		t.Fatal(err)
	}
	if next != 4 {
		t.Fatalf("AR next_number=%d, want 4", next)
	}
}

func TestImportRequirementsSecondRunIsUnchangedWithoutDuplicates(t *testing.T) {
	base := persistentTemp(t, "import-requirements-unchanged")
	root := filepath.Join(base, "main")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	s := openStoreWithRequirementPrefix(t, root, filepath.Join(base, "common"), filepath.Join(base, "state"))
	input := filepath.Join(base, "REQUIREMENTS.md")
	writeRequirementImportFile(t, input, "| AR-1 | Stable requirement. | designed | — | — |\n| AR-2 | Another stable requirement. | planned | — | — |\n")
	if _, err := s.ImportRequirements(context.Background(), input); err != nil {
		t.Fatal(err)
	}
	var allocations, events, outbox int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM allocations WHERE project_id=? AND kind=?`, s.projectID, kindRequirement).Scan(&allocations); err != nil {
		t.Fatal(err)
	}
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM events WHERE project_id=? AND verb='requirement.import'`, s.projectID).Scan(&events); err != nil {
		t.Fatal(err)
	}
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM outbox WHERE project_id=? AND verb='requirement.import'`, s.projectID).Scan(&outbox); err != nil {
		t.Fatal(err)
	}
	receiptsBefore := countImportReceipts(t, filepath.Join(base, "common", "aira", "receipts.jsonl"))

	summary, err := s.ImportRequirements(context.Background(), input)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(summary.Unchanged, []string{"AR-1", "AR-2"}) || len(summary.Created)+len(summary.Repaired)+len(summary.Updated) != 0 {
		t.Fatalf("summary=%+v", summary)
	}
	var got int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM allocations WHERE project_id=? AND kind=?`, s.projectID, kindRequirement).Scan(&got); err != nil {
		t.Fatal(err)
	}
	if got != allocations {
		t.Fatalf("allocations=%d after rerun, want %d", got, allocations)
	}
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM events WHERE project_id=? AND verb='requirement.import'`, s.projectID).Scan(&got); err != nil {
		t.Fatal(err)
	}
	if got != events {
		t.Fatalf("events=%d after rerun, want %d", got, events)
	}
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM outbox WHERE project_id=? AND verb='requirement.import'`, s.projectID).Scan(&got); err != nil {
		t.Fatal(err)
	}
	if got != outbox || countImportReceipts(t, filepath.Join(base, "common", "aira", "receipts.jsonl")) != receiptsBefore {
		t.Fatalf("durable import records changed: outbox=%d/%d receipts=%d/%d", got, outbox,
			countImportReceipts(t, filepath.Join(base, "common", "aira", "receipts.jsonl")), receiptsBefore)
	}
}

func TestImportRequirementsRepairsMissingFileAndIndex(t *testing.T) {
	base := persistentTemp(t, "import-requirements-repair")
	root := filepath.Join(base, "main")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	s := openStoreWithRequirementPrefix(t, root, filepath.Join(base, "common"), filepath.Join(base, "state"))
	input := filepath.Join(base, "REQUIREMENTS.md")
	writeRequirementImportFile(t, input, "| AR-1 | Keep me. | designed | — | — |\n| AR-2 | Restore me. | planned | — | — |\n")
	if _, err := s.ImportRequirements(context.Background(), input); err != nil {
		t.Fatal(err)
	}
	var allocations int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM allocations WHERE project_id=?`, s.projectID).Scan(&allocations); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(root, ".aira", "requirements", "AR-2.md")); err != nil {
		t.Fatal(err)
	}
	summary, err := s.ImportRequirements(context.Background(), input)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(summary.Repaired, []string{"AR-2"}) {
		t.Fatalf("summary=%+v", summary)
	}
	if _, err := os.Stat(filepath.Join(root, ".aira", "requirements", "AR-2.md")); err != nil {
		t.Fatalf("repaired file: %v", err)
	}
	if _, err := s.GetRequirement("AR-2"); err != nil {
		t.Fatalf("repaired index: %v", err)
	}
	var got int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM allocations WHERE project_id=?`, s.projectID).Scan(&got); err != nil {
		t.Fatal(err)
	}
	if got != allocations {
		t.Fatalf("allocations=%d after repair, want %d", got, allocations)
	}
	if _, err := s.db.Exec(`DELETE FROM requirements WHERE project_id=? AND worktree_id=? AND id='AR-2'`, s.projectID, s.worktreeID); err != nil {
		t.Fatal(err)
	}
	summary, err = s.ImportRequirements(context.Background(), input)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(summary.Repaired, []string{"AR-2"}) {
		t.Fatalf("index repair summary=%+v", summary)
	}
	if _, err := s.GetRequirement("AR-2"); err != nil {
		t.Fatalf("index was not restored: %v", err)
	}
}

func TestImportRequirementsUpdatesChangedContentWithoutReplacementID(t *testing.T) {
	base := persistentTemp(t, "import-requirements-update")
	root := filepath.Join(base, "main")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	s := openStoreWithRequirementPrefix(t, root, filepath.Join(base, "common"), filepath.Join(base, "state"))
	input := filepath.Join(base, "REQUIREMENTS.md")
	writeRequirementImportFile(t, input, "| AR-1 | Original text. | designed | — | — |\n")
	if _, err := s.ImportRequirements(context.Background(), input); err != nil {
		t.Fatal(err)
	}
	var allocations int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM allocations WHERE project_id=?`, s.projectID).Scan(&allocations); err != nil {
		t.Fatal(err)
	}
	writeRequirementImportFile(t, input, "| AR-1 | Revised text. | built | — | — |\n")
	summary, err := s.ImportRequirements(context.Background(), input)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(summary.Updated, []string{"AR-1"}) {
		t.Fatalf("summary=%+v", summary)
	}
	data, err := os.ReadFile(filepath.Join(root, ".aira", "requirements", "AR-1.md"))
	if err != nil {
		t.Fatal(err)
	}
	requirement, err := domain.ParseRequirement(data)
	if err != nil {
		t.Fatal(err)
	}
	if requirement.ID != "AR-1" || requirement.Text != "Revised text." || requirement.Status != domain.RequirementBuilt {
		t.Fatalf("updated requirement=%#v", requirement)
	}
	if _, err := s.GetRequirement("AR-1"); err != nil {
		t.Fatal(err)
	}
	var got int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM allocations WHERE project_id=?`, s.projectID).Scan(&got); err != nil {
		t.Fatal(err)
	}
	if got != allocations {
		t.Fatalf("allocations=%d after update, want %d", got, allocations)
	}
}

func TestImportRequirementsUpdateUsesKindInclusiveEventDigest(t *testing.T) {
	base := persistentTemp(t, "import-requirements-event-digest")
	root := filepath.Join(base, "main")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	s := openStoreWithRequirementPrefix(t, root, filepath.Join(base, "common"), filepath.Join(base, "state"))
	input := filepath.Join(base, "REQUIREMENTS.md")
	writeRequirementImportFile(t, input, "| AR-1 | Original. | designed | — | — |\n")
	if _, err := s.ImportRequirements(context.Background(), input); err != nil {
		t.Fatal(err)
	}
	writeRequirementImportFile(t, input, "| AR-1 | Revised. | built | — | — |\n")
	if _, err := s.ImportRequirements(context.Background(), input); err != nil {
		t.Fatal(err)
	}

	var payload string
	if err := s.db.QueryRow(`SELECT payload_digest FROM events WHERE project_id=? AND verb='requirement.import' AND target='AR-1' ORDER BY seq DESC LIMIT 1`, s.projectID).Scan(&payload); err != nil {
		t.Fatal(err)
	}
	want := allocationEventDigest("requirement.import", "AR-1", kindRequirement)
	if payload != want {
		t.Fatalf("update payload digest=%q, want D1 %q", payload, want)
	}
	if err := s.Rebuild(context.Background()); err != nil {
		t.Fatalf("rebuild after update event: %v", err)
	}
}

func TestRegisterImportedRequirementAdvancesCounterInRegistrationTransaction(t *testing.T) {
	base := persistentTemp(t, "import-requirements-counter-crash")
	root := filepath.Join(base, "main")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	s := openStoreWithRequirementPrefix(t, root, filepath.Join(base, "common"), filepath.Join(base, "state"))
	rows, err := parseRequirementTable([]byte("| ID | Requirement | Status | Implemented-by | Verified-by |\n|---|---|---|---|---|\n| AR-1 | One. | designed | — | — |\n| AR-2 | Two. | designed | — | — |\n| AR-3 | Three. | designed | — | — |\n"))
	if err != nil {
		t.Fatal(err)
	}
	for _, row := range rows {
		if _, _, err := s.registerImportedRequirement(context.Background(), row, s.requirementPath(row.ID)); err != nil {
			t.Fatal(err)
		}
	}
	var next int64
	if err := s.db.QueryRow(`SELECT next_number FROM id_counters WHERE project_id=? AND prefix='AR'`, s.projectID).Scan(&next); err != nil {
		t.Fatal(err)
	}
	if next < 4 {
		t.Fatalf("counter next_number=%d after per-row registration, want >=4", next)
	}
}

func TestImportRequirementsAcceptsAlignedSeparator(t *testing.T) {
	base := persistentTemp(t, "import-requirements-aligned-separator")
	root := filepath.Join(base, "main")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	s := openStoreWithRequirementPrefix(t, root, filepath.Join(base, "common"), filepath.Join(base, "state"))
	input := filepath.Join(base, "REQUIREMENTS.md")
	if err := os.WriteFile(input, []byte("| ID | Requirement | Status | Implemented-by | Verified-by |\n|:---:|:---|---:|:---:|:---|\n| AR-1 | Aligned. | designed | — | — |\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	summary, err := s.ImportRequirements(context.Background(), input)
	if err != nil {
		t.Fatalf("aligned separator import: %v", err)
	}
	if !reflect.DeepEqual(summary.Created, []string{"AR-1"}) {
		t.Fatalf("summary=%+v", summary)
	}
}

func TestKindForPathUsesImmediateEntityDirectory(t *testing.T) {
	root := filepath.Join(string(filepath.Separator), "tmp", ".aira", "requirements", "nested-repo")
	ticketPath := filepath.Join(root, ".aira", "tickets", "AIRA-1.md")
	if got := kindForPath(ticketPath); got != kindTicket {
		t.Fatalf("kindForPath(%q)=%q, want %q", ticketPath, got, kindTicket)
	}
	s := &Store{prefixes: map[string]string{"AIRA": kindTicket}}
	if got, err := s.reconcileAllocationKind("AIRA", kindTicket, ticketPath); err != nil || got != kindTicket {
		t.Fatalf("reconcileAllocationKind=%q, err=%v", got, err)
	}
	if got := kindForPath(filepath.Join(root, ".aira", "requirements", "AR-1.md")); got != kindRequirement {
		t.Fatalf("requirement kindForPath=%q, want %q", got, kindRequirement)
	}
}

func TestImportRequirementsRepairsMissingIndexWithoutNewEvent(t *testing.T) {
	base := persistentTemp(t, "import-requirements-index-only")
	root := filepath.Join(base, "main")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	s := openStoreWithRequirementPrefix(t, root, filepath.Join(base, "common"), filepath.Join(base, "state"))
	input := filepath.Join(base, "REQUIREMENTS.md")
	writeRequirementImportFile(t, input, "| AR-1 | Keep file and outbox. | designed | — | — |\n")
	if _, err := s.ImportRequirements(context.Background(), input); err != nil {
		t.Fatal(err)
	}
	var before int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM events WHERE project_id=? AND verb='requirement.import' AND target='AR-1'`, s.projectID).Scan(&before); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`DELETE FROM requirements WHERE project_id=? AND worktree_id=? AND id='AR-1'`, s.projectID, s.worktreeID); err != nil {
		t.Fatal(err)
	}
	summary, err := s.ImportRequirements(context.Background(), input)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(summary.Repaired, []string{"AR-1"}) {
		t.Fatalf("summary=%+v", summary)
	}
	var after int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM events WHERE project_id=? AND verb='requirement.import' AND target='AR-1'`, s.projectID).Scan(&after); err != nil {
		t.Fatal(err)
	}
	if after != before {
		t.Fatalf("requirement.import events=%d after index repair, want %d", after, before)
	}
	if _, err := s.GetRequirement("AR-1"); err != nil {
		t.Fatalf("index was not restored: %v", err)
	}
}

func TestImportRequirementsMalformedRowMakesNoWrites(t *testing.T) {
	base := persistentTemp(t, "import-requirements-malformed")
	root := filepath.Join(base, "main")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	s := openStoreWithRequirementPrefix(t, root, filepath.Join(base, "common"), filepath.Join(base, "state"))
	input := filepath.Join(base, "REQUIREMENTS.md")
	writeRequirementImportFile(t, input,
		"| AR-1 | This valid row must not be partially imported. | designed | — | — |\n"+
			"| AR-2 | Invalid status. | not-a-status | — | — |\n")
	if _, err := s.ImportRequirements(context.Background(), input); err == nil || !strings.Contains(err.Error(), "E_IMPORT_INVALID") {
		t.Fatalf("malformed import error=%v", err)
	}
	var allocations, requirements, events int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM allocations WHERE project_id=?`, s.projectID).Scan(&allocations); err != nil {
		t.Fatal(err)
	}
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM requirements WHERE project_id=?`, s.projectID).Scan(&requirements); err != nil {
		t.Fatal(err)
	}
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM events WHERE project_id=?`, s.projectID).Scan(&events); err != nil {
		t.Fatal(err)
	}
	if allocations != 0 || requirements != 0 || events != 0 {
		t.Fatalf("malformed import partially wrote allocations=%d requirements=%d events=%d", allocations, requirements, events)
	}
	if _, err := os.Stat(filepath.Join(root, ".aira", "requirements")); !os.IsNotExist(err) {
		t.Fatalf("malformed import created requirement directory, stat err=%v", err)
	}
}

func TestParseRealRequirementsRegistry(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("..", "..", "REQUIREMENTS.md"))
	if err != nil {
		t.Fatal(err)
	}
	rows, err := parseRequirementTable(data)
	if err != nil {
		t.Fatalf("parse seed registry: %v", err)
	}
	if len(rows) != 7 {
		t.Fatalf("parsed %d rows, want 7", len(rows))
	}
	for i, row := range rows {
		wantID := "AR-" + strconv.Itoa(i+1)
		if row.ID != wantID || row.Status != domain.RequirementDesigned {
			t.Fatalf("row %d=%+v, want %s designed", i, row, wantID)
		}
	}
}

func TestParseRequirementTableRejectsLiteralPipe(t *testing.T) {
	data := []byte("| ID | Requirement | Status | Implemented-by | Verified-by |\n|---|---|---|---|---|\n| AR-1 | text with | a pipe | designed | — | — |\n")
	if _, err := parseRequirementTable(data); err == nil || !strings.Contains(err.Error(), "E_IMPORT_INVALID") || !strings.Contains(err.Error(), "literal '|'") {
		t.Fatalf("literal pipe error=%v", err)
	}
}

func countImportReceipts(t *testing.T, path string) int {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	count := 0
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		if strings.TrimSpace(line) != "" {
			count++
		}
	}
	return count
}
