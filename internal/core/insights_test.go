package core

import (
	"context"
	"testing"

	"aira/internal/domain"
)

func TestInsightsFacesListSingleAndDefaultAll(t *testing.T) {
	s := coreTestStore(t)
	c := New(s)
	listed := c.Do(context.Background(), Request{Verb: "insights", Args: map[string]any{"subverb": "ls"}})
	if !listed.OK {
		t.Fatalf("insights ls=%#v", listed)
	}
	var rows []map[string]any
	marshalRoundTrip(t, listed.Data, &rows)
	if len(rows) != 6 {
		t.Fatalf("insights registry=%#v", rows)
	}
	shown := c.Do(context.Background(), Request{Verb: "insights", Args: map[string]any{"subverb": "show", "name": "reviewer-verdict-ratio"}})
	if !shown.OK || shown.Code != "UNEVALUATED" || shown.Exit != 3 {
		t.Fatalf("insights show=%#v", shown)
	}
	var one map[string]any
	marshalRoundTrip(t, shown.Data, &one)
	if one["unevaluated"] != true || one["universe"] == nil {
		t.Fatalf("single gauge=%#v", one)
	}
	all := c.Do(context.Background(), Request{Verb: "insights", Args: map[string]any{}})
	if !all.OK || all.Code != "UNEVALUATED" || all.Exit != 3 {
		t.Fatalf("insights default=%#v", all)
	}
}

func TestFindingDistributionFaceIsUncapped(t *testing.T) {
	s := coreTestStore(t)
	for i := 0; i < 60; i++ {
		if _, _, err := s.AddFinding(context.Background(), domain.ReviewFindingInput{
			TicketID: "AIRA-1", Category: "cat", Severity: domain.SeverityP1,
			Verdict: domain.VerdictConfirmed, Source: "source-" + string(rune('a'+i/26)) + string(rune('a'+i%26)), Message: "message",
		}); err != nil {
			t.Fatal(err)
		}
	}
	response := New(s).Do(context.Background(), Request{Verb: "find", Args: map[string]any{"subverb": "ls", "by": "source"}})
	if !response.OK {
		t.Fatalf("find distribution=%#v", response)
	}
	var data struct {
		Total        int            `json:"total"`
		Distribution map[string]int `json:"distribution"`
	}
	marshalRoundTrip(t, response.Data, &data)
	if data.Total != 60 || len(data.Distribution) != 60 {
		t.Fatalf("uncapped face=%#v", data)
	}
}
