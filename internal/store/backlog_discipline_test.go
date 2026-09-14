package store

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"aira/internal/domain"
)

// AIRA-249. The backlog/release-discipline surface: hold and milestone are
// createable, settable, list-selectable, and hold keeps a ticket out of ready.

func backlogTestStore(t *testing.T) *Store {
	t.Helper()
	base := persistentTemp(t, "backlog")
	root := filepath.Join(base, "main")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	return testStore(t, root, filepath.Join(base, "common"), filepath.Join(base, "state"))
}

func TestCreateHonoursHoldAndMilestone(t *testing.T) {
	s := backlogTestStore(t)
	ctx := context.Background()

	ticket, err := s.CreateTicket(ctx, domain.CreateTicketInput{
		Title: "Held with a target release", Kind: domain.KindFeature, Severity: domain.SeverityP2,
		Hold: true, Milestone: "v0.9",
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if !ticket.Hold {
		t.Fatal("created ticket is not held")
	}
	if ticket.Milestone == nil || *ticket.Milestone != "v0.9" {
		t.Fatalf("milestone = %v, want v0.9", ticket.Milestone)
	}

	// A blank milestone must stay nil, not an empty-string pointer, so
	// `list milestone:` never matches an unmilestoned ticket.
	plain, err := s.CreateTicket(ctx, domain.CreateTicketInput{
		Title: "No milestone", Kind: domain.KindFeature, Severity: domain.SeverityP2, Milestone: "   ",
	})
	if err != nil {
		t.Fatalf("create plain: %v", err)
	}
	if plain.Milestone != nil {
		t.Fatalf("blank milestone became %q, want nil", *plain.Milestone)
	}
	if plain.Hold {
		t.Fatal("default create must not be held")
	}
}

func TestSetTicketMilestoneSetsAndClears(t *testing.T) {
	s := backlogTestStore(t)
	ctx := context.Background()
	ticket, err := s.CreateTicket(ctx, domain.CreateTicketInput{
		Title: "Milestone via set", Kind: domain.KindChore, Severity: domain.SeverityP3,
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	if _, err := s.SetTicket(ctx, ticket.ID, "milestone", "v0.9"); err != nil {
		t.Fatalf("set milestone: %v", err)
	}
	got, err := s.Get(ticket.ID)
	if err != nil {
		t.Fatalf("get after set: %v", err)
	}
	if got.Ticket.Milestone == nil || *got.Ticket.Milestone != "v0.9" {
		t.Fatalf("milestone after set = %v, want v0.9", got.Ticket.Milestone)
	}

	// Empty value clears it (the one field besides body/title that may be set empty).
	if _, err := s.SetTicket(ctx, ticket.ID, "milestone", ""); err != nil {
		t.Fatalf("clear milestone: %v", err)
	}
	got, err = s.Get(ticket.ID)
	if err != nil {
		t.Fatalf("get after clear: %v", err)
	}
	if got.Ticket.Milestone != nil {
		t.Fatalf("milestone after clear = %q, want nil", *got.Ticket.Milestone)
	}
}

func TestListSelectsByHoldAndMilestone(t *testing.T) {
	s := backlogTestStore(t)
	ctx := context.Background()
	held, err := s.CreateTicket(ctx, domain.CreateTicketInput{
		Title: "Backlog item", Kind: domain.KindFeature, Severity: domain.SeverityP2, Hold: true, Milestone: "later",
	})
	if err != nil {
		t.Fatalf("create held: %v", err)
	}
	active, err := s.CreateTicket(ctx, domain.CreateTicketInput{
		Title: "Current work", Kind: domain.KindFeature, Severity: domain.SeverityP2, Milestone: "v0.9",
	})
	if err != nil {
		t.Fatalf("create active: %v", err)
	}

	assertOnly := func(selector, wantID string) {
		t.Helper()
		rows, err := s.List(selector)
		if err != nil {
			t.Fatalf("list %q: %v", selector, err)
		}
		if len(rows) != 1 || rows[0].Ticket.ID != wantID {
			ids := make([]string, len(rows))
			for i, r := range rows {
				ids[i] = r.Ticket.ID
			}
			t.Fatalf("list %q = %v, want [%s]", selector, ids, wantID)
		}
	}
	assertOnly("hold:true", held.ID)
	assertOnly("hold:false", active.ID)
	assertOnly("milestone:later", held.ID)
	assertOnly("milestone:v0.9", active.ID)
}

func TestHeldTicketIsExcludedFromReady(t *testing.T) {
	s := backlogTestStore(t)
	ctx := context.Background()
	held, err := s.CreateTicket(ctx, domain.CreateTicketInput{
		Title: "Do not build me", Kind: domain.KindFeature, Severity: domain.SeverityP2, Hold: true,
	})
	if err != nil {
		t.Fatalf("create held: %v", err)
	}
	active, err := s.CreateTicket(ctx, domain.CreateTicketInput{
		Title: "Build me", Kind: domain.KindFeature, Severity: domain.SeverityP2,
	})
	if err != nil {
		t.Fatalf("create active: %v", err)
	}

	rows, err := s.Ready("")
	if err != nil {
		t.Fatalf("ready: %v", err)
	}
	sawActiveReady := false
	for _, row := range rows {
		if row.Ticket.Ticket.ID == held.ID && row.Ready {
			t.Fatalf("held ticket %s reported ready", held.ID)
		}
		if row.Ticket.Ticket.ID == active.ID && row.Ready {
			sawActiveReady = true
		}
	}
	if !sawActiveReady {
		t.Fatalf("unheld ticket %s was not reported ready", active.ID)
	}
}
