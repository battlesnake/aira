package store

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"aira/internal/domain"
)

func installAlternatingScanHook(t *testing.T, path string, first, second []byte) {
	t.Helper()
	count := 0
	scanReadHook = func() {
		count++
		payload := second
		if count%2 == 0 {
			payload = first
		}
		if err := os.WriteFile(path, payload, 0o644); err != nil {
			t.Fatalf("alternate scan payload: %v", err)
		}
	}
	t.Cleanup(func() { scanReadHook = nil })
}

func TestStableReadRetriesTransientTear(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ticket.md")
	first, second := []byte("first"), []byte("second")
	if err := os.WriteFile(path, first, 0o644); err != nil {
		t.Fatal(err)
	}
	count := 0
	scanReadHook = func() {
		count++
		if count == 1 {
			if err := os.WriteFile(path, second, 0o644); err != nil {
				t.Fatal(err)
			}
		} else if count == 2 {
			if err := os.WriteFile(path, first, 0o644); err != nil {
				t.Fatal(err)
			}
		}
	}
	t.Cleanup(func() { scanReadHook = nil })
	data, outcome, err := stableReadFile(path)
	if err != nil || outcome != scanReadStable || string(data) != string(first) {
		t.Fatalf("stable retry = %q, %v, %v", data, outcome, err)
	}
}

func TestStableReadPersistentTearIsInconclusive(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ticket.md")
	first, second := []byte("first"), []byte("second")
	if err := os.WriteFile(path, first, 0o644); err != nil {
		t.Fatal(err)
	}
	installAlternatingScanHook(t, path, first, second)
	data, outcome, err := stableReadFile(path)
	if err != nil || outcome != scanReadInconclusive || data != nil {
		t.Fatalf("persistent tear = %q, %v, %v", data, outcome, err)
	}
}

func TestStablePartialReadDocumentsAcceptedResidual(t *testing.T) {
	path := filepath.Join(t.TempDir(), "partial.md")
	partial := []byte("---\npartial")
	if err := os.WriteFile(path, partial, 0o644); err != nil {
		t.Fatal(err)
	}
	// This is intentionally not a fail-before discriminator. If an external
	// writer leaves the same partial prefix in place longer than the bounded
	// retry window, the double-read cannot distinguish it from stable content.
	// Eliminating that residual requires atomic replacement by the external
	// writer, which working-tree authority cannot mandate.
	data, outcome, err := stableReadFile(path)
	if err != nil || outcome != scanReadStable || string(data) != string(partial) {
		t.Fatalf("stable partial residual = %q, %v, %v", data, outcome, err)
	}
}

func TestRebuildGenuineReadErrorsDoNotBecomeInvalidFindings(t *testing.T) {
	for _, kind := range []string{"finding", "requirement"} {
		t.Run(kind, func(t *testing.T) {
			base := t.TempDir()
			root := filepath.Join(base, "main")
			s := testStore(t, root, filepath.Join(base, "common"), filepath.Join(base, "state"))
			var path string
			if kind == "finding" {
				path = filepath.Join(root, ".aira", "findings", "f-io.md")
			} else {
				path = filepath.Join(root, ".aira", "requirements", "AR-io.md")
			}
			if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, []byte("unreadable"), 0o000); err != nil {
				t.Fatal(err)
			}
			defer os.Chmod(path, 0o644)
			if err := s.Rebuild(context.Background()); err == nil {
				t.Fatal("Rebuild unexpectedly succeeded on unreadable entity")
			}
			code := "E_FINDING_INVALID"
			if kind == "requirement" {
				code = "E_REQUIREMENT_INVALID"
			}
			var count int
			if err := s.db.QueryRow("SELECT count(*) FROM findings WHERE project_id=? AND worktree_id=? AND code=?", s.projectID, s.worktreeID, code).Scan(&count); err != nil {
				t.Fatal(err)
			}
			if count != 0 {
				t.Fatalf("genuine %s IO error fabricated %s finding: %d", kind, code, count)
			}
		})
	}
}

func TestDirectoryShapedEntitiesAreNotSilentlyDropped(t *testing.T) {
	t.Run("ticket", func(t *testing.T) {
		base := t.TempDir()
		root := filepath.Join(base, "main")
		s := testStore(t, root, filepath.Join(base, "common"), filepath.Join(base, "state"))
		ticket, err := s.CreateTicket(context.Background(), domain.CreateTicketInput{Title: "directory ticket", Kind: domain.KindFeature, Severity: domain.SeverityP2})
		if err != nil {
			t.Fatal(err)
		}
		path := s.ticketPath(ticket.ID)
		if err := os.Remove(path); err != nil {
			t.Fatal(err)
		}
		if err := os.Mkdir(path, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := s.Rebuild(context.Background()); err != nil {
			t.Fatal(err)
		}
		var count int
		if err := s.db.QueryRow("SELECT count(*) FROM findings WHERE project_id=? AND worktree_id=? AND code='E_CONFIG_INVALID'", s.projectID, s.worktreeID).Scan(&count); err != nil {
			t.Fatal(err)
		}
		if count == 0 {
			t.Fatal("directory-shaped ticket was silently dropped")
		}
		rows, err := s.Ready(ticket.ID)
		if err != nil {
			t.Fatal(err)
		}
		if len(rows) == 0 || rows[0].Ready {
			t.Fatalf("directory-shaped ticket false-passed Ready: %#v", rows)
		}
	})

	t.Run("finding", func(t *testing.T) {
		base := t.TempDir()
		root := filepath.Join(base, "main")
		s := testStore(t, root, filepath.Join(base, "common"), filepath.Join(base, "state"))
		ticket, err := s.CreateTicket(context.Background(), domain.CreateTicketInput{Title: "finding owner", Kind: domain.KindFeature, Severity: domain.SeverityP2})
		if err != nil {
			t.Fatal(err)
		}
		finding, _, err := s.AddFinding(context.Background(), domain.ReviewFindingInput{TicketID: ticket.ID, Category: "bug", Severity: domain.SeverityP2, Verdict: domain.VerdictConfirmed, Source: "test", Message: "directory"})
		if err != nil {
			t.Fatal(err)
		}
		path := s.findingPath(finding.Key)
		if err := os.Remove(path); err != nil {
			t.Fatal(err)
		}
		if err := os.Mkdir(path, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := s.Rebuild(context.Background()); err != nil {
			t.Fatal(err)
		}
		var count int
		if err := s.db.QueryRow("SELECT count(*) FROM findings WHERE project_id=? AND worktree_id=? AND code='E_FINDING_INVALID'", s.projectID, s.worktreeID).Scan(&count); err != nil {
			t.Fatal(err)
		}
		if count == 0 {
			t.Fatal("directory-shaped finding was silently dropped")
		}
	})
}

func TestRebuildPersistentTornTicketPreservesIndexAndFindingState(t *testing.T) {
	base := t.TempDir()
	root := filepath.Join(base, "main")
	s := testStore(t, root, filepath.Join(base, "common"), filepath.Join(base, "state"))
	ticket, err := s.CreateTicket(context.Background(), domain.CreateTicketInput{Title: "stable", Kind: domain.KindFeature, Severity: domain.SeverityP2})
	if err != nil {
		t.Fatal(err)
	}
	path := s.ticketPath(ticket.ID)
	first, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	second := []byte("not a ticket")
	installAlternatingScanHook(t, path, first, second)
	if err := s.Rebuild(context.Background()); ErrorCode(err) != "U_INDEX_UNESTABLISHED" {
		t.Fatalf("torn rebuild error = %v", err)
	}
	var tickets, findings int
	if err := s.db.QueryRow(`SELECT count(*) FROM tickets WHERE project_id=? AND worktree_id=?`, s.projectID, s.worktreeID).Scan(&tickets); err != nil {
		t.Fatal(err)
	}
	if err := s.db.QueryRow(`SELECT count(*) FROM findings WHERE project_id=? AND worktree_id=? AND finding_key LIKE 'scan:%'`, s.projectID, s.worktreeID).Scan(&findings); err != nil {
		t.Fatal(err)
	}
	if tickets != 1 || findings != 0 {
		t.Fatalf("torn rebuild changed durable state: tickets=%d findings=%d", tickets, findings)
	}
}

func TestStableMalformedTicketStillRebuildsAsFindingAndSelfHeals(t *testing.T) {
	base := t.TempDir()
	root := filepath.Join(base, "main")
	s := testStore(t, root, filepath.Join(base, "common"), filepath.Join(base, "state"))
	path := filepath.Join(root, ".aira", "tickets", "AIRA-99.md")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("stable malformed"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := s.Rebuild(context.Background()); err != nil {
		t.Fatal(err)
	}
	var count int
	if err := s.db.QueryRow(`SELECT count(*) FROM findings WHERE project_id=? AND worktree_id=? AND subtype='reconciliation' AND code='E_CONFIG_INVALID'`, s.projectID, s.worktreeID).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("stable malformed finding count=%d", count)
	}
	writeTicketFile(t, path, "AIRA-99")
	if err := s.Rebuild(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := s.db.QueryRow(`SELECT count(*) FROM findings WHERE project_id=? AND worktree_id=? AND subtype='reconciliation' AND substr(finding_key,1,?)=?`, s.projectID, s.worktreeID, len("scan:"+s.worktreeID+":"), "scan:"+s.worktreeID+":").Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatalf("self-healed scan findings remain=%d", count)
	}
}

func TestVanishedTicketDuringStableReadIsUnestablished(t *testing.T) {
	base := t.TempDir()
	root := filepath.Join(base, "main")
	s := testStore(t, root, filepath.Join(base, "common"), filepath.Join(base, "state"))
	ticket, err := s.CreateTicket(context.Background(), domain.CreateTicketInput{Title: "vanish", Kind: domain.KindFeature, Severity: domain.SeverityP2})
	if err != nil {
		t.Fatal(err)
	}
	path := s.ticketPath(ticket.ID)
	scanReadHook = func() {
		_ = os.Remove(path)
	}
	t.Cleanup(func() { scanReadHook = nil })
	if err := s.Rebuild(context.Background()); ErrorCode(err) != "U_INDEX_UNESTABLISHED" {
		t.Fatalf("vanished rebuild error = %v", err)
	}
}

func TestReadyPersistentTearDoesNotFalsePass(t *testing.T) {
	base := t.TempDir()
	root := filepath.Join(base, "main")
	s := testStore(t, root, filepath.Join(base, "common"), filepath.Join(base, "state"))
	from, err := s.CreateTicket(context.Background(), domain.CreateTicketInput{Title: "blocker", Kind: domain.KindFeature, Severity: domain.SeverityP2})
	if err != nil {
		t.Fatal(err)
	}
	to, err := s.CreateTicket(context.Background(), domain.CreateTicketInput{Title: "blocked", Kind: domain.KindFeature, Severity: domain.SeverityP2})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Link(context.Background(), from.ID, domain.RelationBlocks, to.ID); err != nil {
		t.Fatal(err)
	}
	path := s.ticketPath(from.ID)
	first, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	ticket, body, err := domain.ParseTicket(first)
	if err != nil {
		t.Fatal(err)
	}
	ticket.Relations = nil
	second, err := domain.RenderTicket(ticket, body)
	if err != nil {
		t.Fatal(err)
	}
	installAlternatingScanHook(t, path, first, second)
	if _, err := s.Ready(to.ID); ErrorCode(err) != "U_RELATION_GRAPH_UNESTABLISHED" {
		t.Fatalf("torn ready error = %v", err)
	}
}

func TestReadyDirectoryAddIsUnestablished(t *testing.T) {
	base := t.TempDir()
	root := filepath.Join(base, "main")
	s := testStore(t, root, filepath.Join(base, "common"), filepath.Join(base, "state"))
	ticket, err := s.CreateTicket(context.Background(), domain.CreateTicketInput{Title: "existing", Kind: domain.KindFeature, Severity: domain.SeverityP2})
	if err != nil {
		t.Fatal(err)
	}
	path := s.ticketPath(ticket.ID)
	addPath := filepath.Join(root, ".aira", "tickets", "AIRA-2.md")
	scanReadHook = func() { writeTicketFile(t, addPath, "AIRA-2") }
	t.Cleanup(func() { scanReadHook = nil })
	if _, err := s.Ready(ticket.ID); ErrorCode(err) != "U_RELATION_GRAPH_UNESTABLISHED" {
		t.Fatalf("directory-add ready error = %v", err)
	}
	_ = path
}

func TestSearchAndListTornEntityNeverReturnFakeZero(t *testing.T) {
	s := queryTestStore(t)
	ticket, err := s.CreateTicket(context.Background(), testCreateInput("searchable", "needle"))
	if err != nil {
		t.Fatal(err)
	}
	path := s.ticketPath(ticket.ID)
	first, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	installAlternatingScanHook(t, path, first, []byte("torn"))
	if _, err := s.List(""); ErrorCode(err) != "U_INDEX_UNESTABLISHED" {
		t.Fatalf("torn list error = %v", err)
	}
	if _, err := s.Search(context.Background(), "needle", "ticket"); ErrorCode(err) != "U_INDEX_UNESTABLISHED" {
		t.Fatalf("torn search error = %v", err)
	}
}

func TestRebuildPhaseBFailureRollsBackProjectionAndFindings(t *testing.T) {
	base := t.TempDir()
	root := filepath.Join(base, "main")
	s := testStore(t, root, filepath.Join(base, "common"), filepath.Join(base, "state"))
	ticket, err := s.CreateTicket(context.Background(), domain.CreateTicketInput{Title: "prior", Kind: domain.KindFeature, Severity: domain.SeverityP2})
	if err != nil {
		t.Fatal(err)
	}
	other, err := s.CreateTicket(context.Background(), domain.CreateTicketInput{Title: "relation target", Kind: domain.KindFeature, Severity: domain.SeverityP2})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Link(context.Background(), ticket.ID, domain.RelationBlocks, other.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Search(context.Background(), "prior", "ticket"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec("INSERT INTO requirements(project_id,worktree_id,id,path,digest,status,text) VALUES(?,?,?,?,?,?,?)", s.projectID, s.worktreeID, "AR-1", ".aira/requirements/AR-1.md", "digest", "planned", "pre-existing requirement"); err != nil {
		t.Fatal(err)
	}
	if err := s.withImmediate(context.Background(), func(conn *sql.Conn) error {
		return upsertReconciliationFinding(context.Background(), conn, s.projectID, s.worktreeID, "compute:keep", "E_COMPUTE_CONSERVATION", "compute", "pre-existing reconciliation")
	}); err != nil {
		t.Fatal(err)
	}
	var beforeRelations, beforeFTS, beforeRequirements int
	if err := s.db.QueryRow("SELECT count(*) FROM relations WHERE project_id=? AND worktree_id=?", s.projectID, s.worktreeID).Scan(&beforeRelations); err != nil {
		t.Fatal(err)
	}
	if err := s.db.QueryRow("SELECT count(*) FROM search_fts WHERE project_id=? AND worktree_id=?", s.projectID, s.worktreeID).Scan(&beforeFTS); err != nil {
		t.Fatal(err)
	}
	if err := s.db.QueryRow("SELECT count(*) FROM requirements WHERE project_id=? AND worktree_id=?", s.projectID, s.worktreeID).Scan(&beforeRequirements); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, ".aira", "tickets", "AIRA-88.md")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("malformed"), 0o644); err != nil {
		t.Fatal(err)
	}
	s.rebuildPhaseBHook = func() error { return errors.New("injected phase B failure") }
	if err := s.Rebuild(context.Background()); err == nil || !strings.Contains(err.Error(), "injected phase B failure") {
		t.Fatalf("phase B error = %v", err)
	}
	var count int
	if err := s.db.QueryRow(`SELECT count(*) FROM tickets WHERE project_id=? AND worktree_id=? AND id=?`, s.projectID, s.worktreeID, ticket.ID).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("prior ticket projection was not preserved: %d", count)
	}
	if err := s.db.QueryRow(`SELECT count(*) FROM findings WHERE project_id=? AND worktree_id=? AND code='E_CONFIG_INVALID'`, s.projectID, s.worktreeID).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatalf("deferred finding escaped rollback: %d", count)
	}
	var afterRelations, afterFTS, afterRequirements int
	if err := s.db.QueryRow("SELECT count(*) FROM relations WHERE project_id=? AND worktree_id=?", s.projectID, s.worktreeID).Scan(&afterRelations); err != nil {
		t.Fatal(err)
	}
	if err := s.db.QueryRow("SELECT count(*) FROM search_fts WHERE project_id=? AND worktree_id=?", s.projectID, s.worktreeID).Scan(&afterFTS); err != nil {
		t.Fatal(err)
	}
	if err := s.db.QueryRow("SELECT count(*) FROM requirements WHERE project_id=? AND worktree_id=?", s.projectID, s.worktreeID).Scan(&afterRequirements); err != nil {
		t.Fatal(err)
	}
	if afterRelations != beforeRelations || afterFTS != beforeFTS || afterRequirements != beforeRequirements {
		t.Fatalf("projection rollback incomplete: relations %d/%d FTS %d/%d requirements %d/%d", afterRelations, beforeRelations, afterFTS, beforeFTS, afterRequirements, beforeRequirements)
	}
	var details string
	if err := s.db.QueryRow("SELECT details FROM findings WHERE project_id=? AND worktree_id=? AND finding_key=?", s.projectID, s.worktreeID, "compute:keep").Scan(&details); err != nil {
		t.Fatal(err)
	}
	if details != "pre-existing reconciliation" {
		t.Fatalf("pre-existing reconciliation row changed: %q", details)
	}
}

func TestMutationAbortsBeforeLinkWriteOnTornRead(t *testing.T) {
	base := t.TempDir()
	root := filepath.Join(base, "main")
	s := testStore(t, root, filepath.Join(base, "common"), filepath.Join(base, "state"))
	from, err := s.CreateTicket(context.Background(), domain.CreateTicketInput{Title: "from", Kind: domain.KindFeature, Severity: domain.SeverityP2})
	if err != nil {
		t.Fatal(err)
	}
	to, err := s.CreateTicket(context.Background(), domain.CreateTicketInput{Title: "to", Kind: domain.KindFeature, Severity: domain.SeverityP2})
	if err != nil {
		t.Fatal(err)
	}
	ownerPath := s.ticketPath(from.ID)
	ownerBefore, err := os.ReadFile(ownerPath)
	if err != nil {
		t.Fatal(err)
	}
	tornPath := s.ticketPath(to.ID)
	tornBefore, err := os.ReadFile(tornPath)
	if err != nil {
		t.Fatal(err)
	}
	installAlternatingScanHook(t, tornPath, tornBefore, []byte("torn"))
	if _, err := s.Link(context.Background(), from.ID, domain.RelationBlocks, to.ID); ErrorCode(err) != "U_INDEX_UNESTABLISHED" {
		t.Fatalf("torn link error = %v", err)
	}
	var count int
	if err := s.db.QueryRow(`SELECT count(*) FROM outbox WHERE project_id=? AND verb='relation.add'`, s.projectID).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatalf("link wrote an outbox intent before abort: %d", count)
	}
	ownerAfterLink, err := os.ReadFile(ownerPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(ownerAfterLink) != string(ownerBefore) {
		t.Fatal("canonical owner ticket bytes changed despite aborted Link")
	}
	installAlternatingScanHook(t, ownerPath, ownerBefore, []byte("torn"))
	if _, err := s.UpdateTicketContent(context.Background(), from.ID, func(ticket domain.Ticket, body string) (domain.Ticket, string, error) {
		return ticket, body + " update", nil
	}); ErrorCode(err) != "U_INDEX_UNESTABLISHED" {
		t.Fatalf("torn ticket-update error = %v", err)
	}
	if err := s.db.QueryRow(`SELECT count(*) FROM outbox WHERE project_id=? AND verb='ticket.update'`, s.projectID).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatalf("ticket update wrote an outbox intent before abort: %d", count)
	}
}

func TestReadyUsesOneCoherentTicketSnapshot(t *testing.T) {
	base := t.TempDir()
	root := filepath.Join(base, "main")
	s := testStore(t, root, filepath.Join(base, "common"), filepath.Join(base, "state"))
	ticket, err := s.CreateTicket(context.Background(), domain.CreateTicketInput{Title: "one scan", Kind: domain.KindFeature, Severity: domain.SeverityP2})
	if err != nil {
		t.Fatal(err)
	}
	count := 0
	scanTicketsHook = func() { count++ }
	t.Cleanup(func() { scanTicketsHook = nil })
	if _, err := s.Ready(ticket.ID); err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("Ready ticket scans=%d, want one", count)
	}
}

func TestCheckLiveReadersTornAreUnevaluated(t *testing.T) {
	base := t.TempDir()
	root := filepath.Join(base, "main")
	s := testStore(t, root, filepath.Join(base, "common"), filepath.Join(base, "state"))
	ticket, err := s.CreateTicket(context.Background(), domain.CreateTicketInput{Title: "check", Kind: domain.KindFeature, Severity: domain.SeverityP2})
	if err != nil {
		t.Fatal(err)
	}
	ticketPath := s.ticketPath(ticket.ID)
	first, err := os.ReadFile(ticketPath)
	if err != nil {
		t.Fatal(err)
	}
	installAlternatingScanHook(t, ticketPath, first, []byte("torn"))
	report := CheckReport{Dimensions: map[string]string{"stale-index": "pass"}}
	if err := s.checkStaleIndex(&report); err != nil {
		t.Fatal(err)
	}
	if report.Dimensions["stale-index"] != "unevaluated" || !hasFinding(report.UnevaluatedFindings, "U_INDEX_UNESTABLISHED") {
		t.Fatalf("stale reader report=%#v", report)
	}

	requirementPath := filepath.Join(root, ".aira", "requirements", "AR-1.md")
	if err := os.MkdirAll(filepath.Dir(requirementPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(requirementPath, []byte("stable requirement"), 0o644); err != nil {
		t.Fatal(err)
	}
	firstReq, err := os.ReadFile(requirementPath)
	if err != nil {
		t.Fatal(err)
	}
	installAlternatingScanHook(t, requirementPath, firstReq, []byte("torn requirement"))
	report = CheckReport{Dimensions: map[string]string{"allocated-id-file": "pass"}}
	if err := s.checkAllocatedRequirementFile(&report, "AR-1", requirementPath); err != nil {
		t.Fatal(err)
	}
	if report.Dimensions["allocated-id-file"] != "unevaluated" || !hasFinding(report.UnevaluatedFindings, "U_INDEX_UNESTABLISHED") {
		t.Fatalf("requirement reader report=%#v", report)
	}
}

func TestCheckLiveScanDimensionsPropagateTornOutcome(t *testing.T) {
	base := t.TempDir()
	root := filepath.Join(base, "main")
	s := testStore(t, root, filepath.Join(base, "common"), filepath.Join(base, "state"))
	ticket, err := s.CreateTicket(context.Background(), domain.CreateTicketInput{Title: "dimensions", Kind: domain.KindFeature, Severity: domain.SeverityP2})
	if err != nil {
		t.Fatal(err)
	}
	ticketPath := s.ticketPath(ticket.ID)
	first, err := os.ReadFile(ticketPath)
	if err != nil {
		t.Fatal(err)
	}
	installAlternatingScanHook(t, ticketPath, first, []byte("torn"))
	if _, err := scanRelationSnapshotAt(s.root, s.worktreeID, s.projectSlug); ErrorCode(err) != "U_RELATION_GRAPH_UNESTABLISHED" {
		t.Fatalf("relation divergence scan error=%v", err)
	}
	if _, err := s.relationFindings(); ErrorCode(err) != "U_RELATION_GRAPH_UNESTABLISHED" {
		t.Fatalf("relation findings scan error=%v", err)
	}
	report := CheckReport{Dimensions: map[string]string{"duplicate-id": "pass"}}
	if err := s.checkDuplicateIDs(context.Background(), &report); err != nil {
		t.Fatal(err)
	}
	if report.Dimensions["duplicate-id"] != "unevaluated" {
		t.Fatalf("duplicate id report=%#v", report)
	}

	// The finding-index dimension has its own live finding-file scan.
	scanReadHook = nil
	finding, _, err := s.AddFinding(context.Background(), domain.ReviewFindingInput{TicketID: ticket.ID, Category: "bug", Severity: domain.SeverityP2, Verdict: domain.VerdictConfirmed, Source: "test", Message: "dimension"})
	if err != nil {
		t.Fatal(err)
	}
	findingPath := s.findingPath(finding.Key)
	firstFinding, err := os.ReadFile(findingPath)
	if err != nil {
		t.Fatal(err)
	}
	installAlternatingScanHook(t, findingPath, firstFinding, []byte("torn finding"))
	if _, err := s.findingIndexDivergence(); ErrorCode(err) != "U_INDEX_UNESTABLISHED" {
		t.Fatalf("finding divergence scan error=%v", err)
	}
}

func TestRebuildDefersGitScanFindingUntilStablePhase(t *testing.T) {
	base := t.TempDir()
	mainRoot := filepath.Join(base, "z-main")
	badRoot := filepath.Join(base, "a-bad")
	s := testStore(t, mainRoot, filepath.Join(base, "common"), filepath.Join(base, "state"))
	if err := os.MkdirAll(badRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := s.RegisterWorktree(context.Background(), "bad", badRoot); err != nil {
		t.Fatal(err)
	}
	ticket, err := s.CreateTicket(context.Background(), domain.CreateTicketInput{Title: "later", Kind: domain.KindFeature, Severity: domain.SeverityP2})
	if err != nil {
		t.Fatal(err)
	}
	path := s.ticketPath(ticket.ID)
	first, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	installAlternatingScanHook(t, path, first, []byte("torn"))
	if err := s.Rebuild(context.Background()); ErrorCode(err) != "U_INDEX_UNESTABLISHED" {
		t.Fatalf("abort error=%v", err)
	}
	var count int
	if err := s.db.QueryRow(`SELECT count(*) FROM findings WHERE project_id=? AND code='E_GIT_SCAN'`, s.projectID).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatalf("E_GIT_SCAN escaped aborted Phase A: %d", count)
	}
	scanReadHook = nil
	if err := s.Rebuild(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := s.db.QueryRow(`SELECT count(*) FROM findings WHERE project_id=? AND code='E_GIT_SCAN'`, s.projectID).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count == 0 {
		t.Fatalf("clean rebuild recorded no E_GIT_SCAN finding")
	}
	cleanCount := count
	var active int
	if err := s.db.QueryRow(`SELECT active FROM worktrees WHERE project_id=? AND worktree_id=?`, s.projectID, "bad").Scan(&active); err != nil {
		t.Fatal(err)
	}
	if active != 0 {
		t.Fatalf("invalid git worktree remained active: %d", active)
	}
	if err := s.Rebuild(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := s.db.QueryRow(`SELECT count(*) FROM findings WHERE project_id=? AND code='E_GIT_SCAN'`, s.projectID).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != cleanCount {
		t.Fatalf("rebuild replay/active pass was not idempotent: before=%d after=%d", cleanCount, count)
	}
}
