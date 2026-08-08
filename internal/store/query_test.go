package store

import (
	"context"
	"os"
	"path/filepath"
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
	if _, err := s.Find(`hold:true`); codeOf(err) != "E_SELECTOR_INVALID" {
		t.Fatalf("hold query code = %q, err=%v", codeOf(err), err)
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
	if _, err := s.List("AIRA-999"); err != nil {
		t.Fatalf("missing plural exact selector = %v", err)
	}
}

func TestCountAgreesWithListWhenIndexIsStale(t *testing.T) {
	s := queryTestStore(t)
	ticket, err := s.CreateTicket(context.Background(), testCreateInput("count me", "body"))
	if err != nil {
		t.Fatal(err)
	}
	data, err := domain.RenderTicket(func() domain.Ticket { ticket.Title = "edited"; return ticket }(), "body")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(s.root, ".aira", "tickets", ticket.ID+".md"), data, 0o644); err != nil {
		t.Fatal(err)
	}
	rows, err := s.List("")
	if err != nil {
		t.Fatal(err)
	}
	count, err := s.Count("", "status")
	if err != nil || count.Total != len(rows) || count.Warnings == nil {
		t.Fatalf("stale count/list disagreement: rows=%d count=%#v err=%v", len(rows), count, err)
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
