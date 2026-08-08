package store

import (
	"context"
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

	rows, err := s.Find(`kind:bug severity:P1 hold:true`)
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
	if _, err := s.Get(`kind:feature`); codeOf(err) != "E_SELECTOR_AMBIGUOUS" {
		t.Fatalf("ambiguous selector code = %q, err=%v", codeOf(err), err)
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
	for _, code := range []string{"E_NOT_FOUND", "E_SELECTOR_INVALID", "E_SELECTOR_AMBIGUOUS"} {
		if strings.Contains(err.Error(), code) {
			return code
		}
	}
	return ""
}
