// verifies: AR-5, AR-6

package domain

import "testing"

func TestTicketRoundTripAndCanonicalRelationOrder(t *testing.T) {
	ticket := Ticket{
		ID:       "AIRA-42",
		Project:  "aira",
		Title:    "Implement the ready queue",
		Status:   StatusPlanned,
		Kind:     KindFeature,
		Severity: SeverityP2,
		Labels:   []string{"queue", "phase1"},
		Relations: []Relation{
			{Kind: RelationRelates, From: "AIRA-42", To: "AIRA-44"},
			{Kind: RelationBlocks, From: "AIRA-42", To: "AIRA-43"},
		},
	}
	data, err := RenderTicket(ticket, "Body\n")
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	got, body, err := ParseTicket(data)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if got.ID != ticket.ID || got.Title != ticket.Title || got.Kind != KindFeature {
		t.Fatalf("round trip mismatch: %#v", got)
	}
	if body != "Body\n" {
		t.Fatalf("body = %q", body)
	}
	if len(got.Relations) != 2 || got.Relations[0].Kind != RelationBlocks {
		t.Fatalf("relations were not canonicalised: %#v", got.Relations)
	}
}

func TestStatusTransitionGraph(t *testing.T) {
	allowed := [][2]Status{
		{StatusDraft, StatusPlanned},
		{StatusPlanned, StatusInProgress},
		{StatusInProgress, StatusInReview},
		{StatusInReview, StatusDone},
		{StatusDone, StatusRetired},
	}
	for _, edge := range allowed {
		if err := ValidateTransition(edge[0], edge[1]); err != nil {
			t.Errorf("expected %s -> %s to be allowed: %v", edge[0], edge[1], err)
		}
	}
	if err := ValidateTransition(StatusDone, StatusInProgress); err == nil {
		t.Fatal("backward transition unexpectedly allowed")
	}
}

func TestRenderTicketDeduplicatesSortedLabels(t *testing.T) {
	ticket := Ticket{Schema: 1, ID: "AIRA-1", Project: "aira", Title: "labels", Status: StatusPlanned, Kind: KindFeature, Severity: SeverityP2, Labels: []string{"z", "a", "z", "a"}}
	data, err := RenderTicket(ticket, "body")
	if err != nil {
		t.Fatal(err)
	}
	parsed, _, err := ParseTicket(data)
	if err != nil {
		t.Fatal(err)
	}
	if len(parsed.Labels) != 2 || parsed.Labels[0] != "a" || parsed.Labels[1] != "z" {
		t.Fatalf("labels = %#v", parsed.Labels)
	}
}
