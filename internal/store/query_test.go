package store

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"aira/internal/domain"
)

func TestGetAndFindUsePhaseOneSelectors(t *testing.T) {
	s := queryTestStore(t)
	first, err := s.CreateTicket(context.Background(), testCreateInput("Alpha queue", "queue body"))
	if err != nil {
		t.Fatal(err)
	}
	second, err := s.CreateTicket(context.Background(), testCreateInput("Beta bug", "other body"))
	if err != nil {
		t.Fatal(err)
	}
	if err := s.UpdateTicket(context.Background(), second.ID, func(ticket domain.Ticket) (domain.Ticket, error) {
		ticket.Kind = domain.KindBug
		ticket.Severity = domain.SeverityP1
		ticket.Hold = true
		return ticket, nil
	}); err != nil {
		t.Fatal(err)
	}

	got, err := s.Get(first.ID)
	if err != nil || got.Ticket.ID != first.ID || got.Body != "queue body\n" {
		t.Fatalf("Get(%q) = %#v, %v", first.ID, got, err)
	}
	anchor, err := s.Get(".aira/tickets/" + first.ID + ".md")
	if err != nil || anchor.Ticket.ID != first.ID {
		t.Fatalf("anchor lookup = %#v, %v", anchor, err)
	}

	rows, err := s.Find(`kind:bug severity:P1`)
	if err != nil || len(rows) != 1 || rows[0].Ticket.ID != second.ID {
		t.Fatalf("field query = %#v, %v", rows, err)
	}
	rows, err = s.Find(`text:"queue body"`)
	if err != nil || len(rows) != 1 || rows[0].Ticket.ID != first.ID {
		t.Fatalf("text query = %#v, %v", rows, err)
	}
	if _, err := s.Find("status:"); codeOf(err) != "E_SELECTOR_INVALID" {
		t.Fatalf("invalid query code = %q, err=%v", codeOf(err), err)
	}
	if _, err := s.Find(`kind=bug`); codeOf(err) != "E_SELECTOR_INVALID" {
		t.Fatalf("equals query code = %q, err=%v", codeOf(err), err)
	}
	// AIRA-249 deliberately makes `hold` a selectable field so the held backlog
	// is reviewable (`aira list hold:true`). It was previously refused here; the
	// exclusion was incidental (hold was already a distribution field, and the
	// nullable milestone/assignee fields are selectable) rather than principled.
	holdRows, err := s.Find(`hold:true`)
	if err != nil || len(holdRows) != 1 || holdRows[0].Ticket.ID != second.ID {
		t.Fatalf("hold:true query = %#v, %v", holdRows, err)
	}
	unheldRows, err := s.Find(`hold:false`)
	if err != nil || len(unheldRows) != 1 || unheldRows[0].Ticket.ID != first.ID {
		t.Fatalf("hold:false query = %#v, %v", unheldRows, err)
	}
	if _, err := s.Find(`text:queue`); codeOf(err) != "E_SELECTOR_INVALID" {
		t.Fatalf("unquoted text query code = %q, err=%v", codeOf(err), err)
	}
}

func TestSingularSelectorsRefuseZeroAndMultipleMatches(t *testing.T) {
	s := queryTestStore(t)
	for _, title := range []string{"one", "two"} {
		if _, err := s.CreateTicket(context.Background(), testCreateInput(title, "")); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.Get("AIRA-999"); codeOf(err) != "E_NOT_FOUND" {
		t.Fatalf("missing selector code = %q, err=%v", codeOf(err), err)
	}
	for _, selector := range []string{"kind:feature", ""} {
		if _, err := s.Get(selector); codeOf(err) != "E_SELECTOR_INVALID" {
			t.Fatalf("singular selector %q code = %q, err=%v", selector, codeOf(err), err)
		}
	}
}

func TestAnchorReadUsesCurrentFileWithoutIndex(t *testing.T) {
	s := queryTestStore(t)
	ticket := domain.Ticket{Schema: 1, ID: "AIRA-77", Project: "query-project", Title: "hand edited", Status: domain.StatusPlanned, Kind: domain.KindFeature, Severity: domain.SeverityP2}
	data, err := domain.RenderTicket(ticket, "from file")
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(s.root, ".aira", "tickets", "AIRA-77.md")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := s.Get(".aira/tickets/AIRA-77.md")
	if err != nil || got.Ticket.ID != "AIRA-77" || got.Body != "from file\n" {
		t.Fatalf("unindexed anchor = %#v, %v", got, err)
	}
}

func TestReadSurfacesStaleIndexWarningFromCurrentFile(t *testing.T) {
	s := queryTestStore(t)
	ticket, err := s.CreateTicket(context.Background(), testCreateInput("indexed", "original"))
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(s.root, ".aira", "tickets", ticket.ID+".md")
	updated := ticket
	updated.Title = "hand edited"
	data, err := domain.RenderTicket(updated, "changed")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := s.Get(ticket.ID)
	if err != nil || len(got.Warnings) != 1 || got.Warnings[0] != "W_STALE_INDEX" || got.Ticket.Title != "hand edited" {
		t.Fatalf("stale read = %#v, %v", got, err)
	}
}

func TestRecordsSkipLocalTicketFindingsAndUseCanonicalIDOrder(t *testing.T) {
	s := queryTestStore(t)
	for _, id := range []string{"AIRA-10", "AIRA-2", "AIRA-1"} {
		ticket := domain.Ticket{Schema: 1, ID: id, Project: "query-project", Title: id, Status: domain.StatusPlanned, Kind: domain.KindFeature, Severity: domain.SeverityP2}
		data, err := domain.RenderTicket(ticket, "body")
		if err != nil {
			t.Fatal(err)
		}
		path := filepath.Join(s.root, ".aira", "tickets", id+".md")
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, data, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	rows, err := s.List("")
	if err != nil || len(rows) != 3 || rows[0].Ticket.ID != "AIRA-1" || rows[1].Ticket.ID != "AIRA-2" || rows[2].Ticket.ID != "AIRA-10" {
		t.Fatalf("canonical rows = %#v, %v", rows, err)
	}
	outside := filepath.Join(t.TempDir(), "outside.md")
	if err := os.WriteFile(outside, []byte("secret"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(s.root, ".aira", "tickets", "AIRA-99.md")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(s.root, ".aira", "tickets", "notes.md"), []byte("notes\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	rows, err = s.List("")
	if err != nil || len(rows) != 3 {
		t.Fatalf("local invalid files wedged list: rows=%#v err=%v", rows, err)
	}
	rows, err = s.List("AIRA-99")
	if err != nil || len(rows) != 0 {
		t.Fatalf("malformed plural exact selector = rows=%#v err=%v", rows, err)
	}
	if _, err := s.Get("AIRA-99"); codeOf(err) != "E_CONFIG_INVALID" {
		t.Fatalf("singular malformed selector = %v", err)
	}
	duplicate, err := domain.RenderTicket(domain.Ticket{Schema: 1, ID: "AIRA-1", Project: "query-project", Title: "duplicate", Status: domain.StatusPlanned, Kind: domain.KindFeature, Severity: domain.SeverityP2}, "body")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(s.root, ".aira", "tickets", "duplicate.md"), duplicate, 0o644); err != nil {
		t.Fatal(err)
	}
	rows, err = s.Find("")
	if err != nil || len(rows) != 2 {
		t.Fatalf("duplicate pair find = rows=%#v err=%v", rows, err)
	}
	count, err := s.Count("", "status")
	if err != nil || count.Total != 2 {
		t.Fatalf("duplicate pair count = %#v err=%v", count, err)
	}
}

func TestCountAgreesWithListWhenIndexIsStale(t *testing.T) {
	s := queryTestStore(t)
	first, err := s.CreateTicket(context.Background(), testCreateInput("first", "body"))
	if err != nil {
		t.Fatal(err)
	}
	second, err := s.CreateTicket(context.Background(), testCreateInput("second", "body"))
	if err != nil {
		t.Fatal(err)
	}
	_, err = s.CreateTicket(context.Background(), testCreateInput("third", "body"))
	if err != nil {
		t.Fatal(err)
	}
	if err := s.UpdateTicket(context.Background(), first.ID, func(ticket domain.Ticket) (domain.Ticket, error) {
		ticket.Status = domain.StatusInProgress
		return ticket, nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(s.root, ".aira", "tickets", second.ID+".md")); err != nil {
		t.Fatal(err)
	}
	unindexed := domain.Ticket{Schema: 1, ID: "AIRA-90", Project: "query-project", Title: "new", Status: domain.StatusDone, Kind: domain.KindBug, Severity: domain.SeverityP1}
	data, err := domain.RenderTicket(unindexed, "body")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(s.root, ".aira", "tickets", unindexed.ID+".md"), data, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(s.root, ".aira", "tickets", "AIRA-91.md"), []byte("malformed\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	rows, err := s.List("")
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]int{"in-progress": 1, "planned": 1, "done": 1}
	listDistribution := map[string]int{}
	for _, row := range rows {
		listDistribution[string(row.Ticket.Status)]++
		if len(row.Warnings) != 1 || row.Warnings[0] != "W_STALE_INDEX" {
			t.Fatalf("stale list row = %#v", row)
		}
	}
	if len(rows) != 3 || !reflect.DeepEqual(listDistribution, want) {
		t.Fatalf("stale list distribution = %#v, rows=%#v", listDistribution, rows)
	}
	count, err := s.Count("", "status")
	if err != nil || count.Total != 3 || !reflect.DeepEqual(count.Distribution, want) || len(count.Warnings) != 1 || count.Warnings[0] != "W_STALE_INDEX" {
		t.Fatalf("stale count/list disagreement: rows=%d count=%#v err=%v", len(rows), count, err)
	}
	if exact, err := s.List(unindexed.ID); err != nil || len(exact) != 1 || len(exact[0].Warnings) != 1 {
		t.Fatalf("unindexed exact selector = %#v, %v", exact, err)
	}
	if exact, err := s.List("AIRA-91"); err != nil || len(exact) != 0 {
		t.Fatalf("malformed exact selector in stale test = %#v, %v", exact, err)
	}
}

func queryTestStore(t *testing.T) *Store {
	t.Helper()
	base := t.TempDir()
	s, err := Open(context.Background(), Options{
		Root: base, CommonDir: base + "/common", DBPath: base + "/state/state.db",
		RegistryPath: base + "/state/registry.jsonl", ProjectID: "project-query",
		WorktreeID: "main", ProjectSlug: "query-project", Prefixes: []string{"AIRA"},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func testCreateInput(title, body string) domain.CreateTicketInput {
	return domain.CreateTicketInput{Title: title, Body: body, Kind: domain.KindFeature, Severity: domain.SeverityP2}
}

func codeOf(err error) string {
	if err == nil {
		return ""
	}
	for _, code := range []string{"E_NOT_FOUND", "E_SELECTOR_INVALID", "E_SELECTOR_AMBIGUOUS", "E_CONFIG_INVALID"} {
		if strings.Contains(err.Error(), code) {
			return code
		}
	}
	return ""
}

func mustInsertAllocation(t *testing.T, s *Store, prefix string, number int64, kind, state, path string) {
	t.Helper()
	if _, err := s.db.Exec(
		`INSERT INTO allocations(project_id, prefix, number, worktree_id, state, path, seq, kind, suffix)
		 VALUES(?,?,?,?,?,?,?,?,'')`,
		s.projectID, prefix, number, s.worktreeID, state, path, 900000+number, kind); err != nil {
		t.Fatalf("insert allocation %s-%d: %v", prefix, number, err)
	}
}

// TestTicketAllocationOriginDiagnosesLedgerPresence pins the AIRA-270 get-verb
// absence predicate: it reports a ticket that was minted in this project (so the
// get verb can say E_TICKET_NOT_IN_WORKTREE instead of a bare not-found), and it
// deliberately EXCLUDES rows the honesty surface must not claim — a non-ticket
// (requirement) allocation, and a terminal-state (retired) allocation, both of
// which legitimately have no working-tree file and are NOT "not in this worktree".
func TestTicketAllocationOriginDiagnosesLedgerPresence(t *testing.T) {
	s := queryTestStore(t)
	ticket, err := s.CreateTicket(context.Background(), testCreateInput("real", "body"))
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	// A materialised ticket allocation is reported, with its recorded path.
	id, path, ok, err := s.TicketAllocationOrigin(ticket.ID)
	if err != nil || !ok || id != ticket.ID {
		t.Fatalf("origin(materialised ticket) = (%q,%q,%v,%v), want ok id=%s", id, path, ok, err, ticket.ID)
	}
	if want := filepath.Join(".aira", "tickets", ticket.ID+".md"); !strings.HasSuffix(path, want) {
		t.Fatalf("recorded path %q must end in %q (it is the historical origin)", path, want)
	}

	// A never-allocated id is not reported (-> the get verb keeps bare E_NOT_FOUND).
	if _, _, ok, err := s.TicketAllocationOrigin("AIRA-999"); err != nil || ok {
		t.Fatalf("origin(never-allocated) ok=%v err=%v, want not-ok", ok, err)
	}

	// A FILE-ANCHOR selector no-ops even for an allocated ticket: it names a path,
	// not "where is this id", so `aira get <path>` keeps bare E_NOT_FOUND. Drop the
	// ExactPath!="" guard and this reddens (the anchor's derived id would match).
	if _, _, ok, err := s.TicketAllocationOrigin(".aira/tickets/" + ticket.ID + ".md"); err != nil || ok {
		t.Fatalf("origin(file-anchor for %s) ok=%v err=%v, want not-ok", ticket.ID, ok, err)
	}

	// A materialised REQUIREMENT-kind allocation must NOT match (get is ticket-only;
	// drop the kind filter and this reddens).
	mustInsertAllocation(t, s, "AIRA", 500, "requirement", "materialised", ".aira/requirements/AIRA-500.md")
	if _, _, ok, _ := s.TicketAllocationOrigin("AIRA-500"); ok {
		t.Fatal("origin matched a requirement allocation; the kind='ticket' filter is missing")
	}

	// A RETIRED-state ticket allocation must NOT match (a terminal state is not
	// "not in this worktree"; broaden the state filter and this reddens).
	mustInsertAllocation(t, s, "AIRA", 501, "ticket", "retired", ".aira/tickets/AIRA-501.md")
	if _, _, ok, _ := s.TicketAllocationOrigin("AIRA-501"); ok {
		t.Fatal("origin matched a retired allocation; the state IN (allocated,materialised) filter is missing")
	}
}

// TestTicketAllocationOriginReportsTheCreatingWorktreePath pins the cross-worktree
// truth the single-worktree test cannot: a ticket created in worktree A, queried
// from a DIFFERENT worktree B that shares the ledger, is reported by B with A's
// recorded path — the "originally recorded at ..." historical fact, distinct from
// B's own would-be path. This is what reveals the removed worktree to the caller.
func TestTicketAllocationOriginReportsTheCreatingWorktreePath(t *testing.T) {
	base := t.TempDir()
	rootA := filepath.Join(base, "A")
	rootB := filepath.Join(base, "B")
	for _, r := range []string{rootA, rootB} {
		if err := os.MkdirAll(filepath.Join(r, ".aira", "tickets"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	common := filepath.Join(base, "common")
	state := filepath.Join(base, "state")

	a := openTestStore(t, rootA, common, state, "A", "AIRA")
	if _, err := a.CreateTicket(context.Background(), testCreateInput("born in A", "body")); err != nil {
		t.Fatalf("create in A: %v", err)
	}

	b := openTestStore(t, rootB, common, state, "B", "AIRA")
	id, path, ok, err := b.TicketAllocationOrigin("AIRA-1")
	if err != nil || !ok || id != "AIRA-1" {
		t.Fatalf("origin from B = (%q,%q,%v,%v), want ok AIRA-1", id, path, ok, err)
	}
	// The reported path is A's (the creating worktree), NOT B's current path.
	if !strings.HasPrefix(path, rootA) {
		t.Fatalf("reported path %q must be under the CREATING worktree %q", path, rootA)
	}
	if strings.HasPrefix(path, rootB) {
		t.Fatalf("reported path %q must not be B's own path", path)
	}
}
