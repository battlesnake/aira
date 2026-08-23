package main

import (
	"strings"
	"testing"

	"aira/internal/store"
)

func TestTicketViewModelShowsConditionalTruncationAndDistribution(t *testing.T) {
	model := ticketListViewModel(listEnvelope{
		Total: 51, Truncated: true,
		Rows:         []map[string]any{{"id": "AIRA-1", "status": "planned", "kind": "feature", "severity": "P1", "assignee": "alice", "milestone": "M1"}},
		Distribution: map[string]int{"planned": 51},
	})
	if len(model.Rows) != 1 || !strings.Contains(model.Footer, "TRUNCATED") || !strings.Contains(model.Footer, "planned=51") {
		t.Fatalf("ticket model=%#v", model)
	}
	plain := ticketListViewModel(listEnvelope{Total: 1, Rows: []map[string]any{{"id": "AIRA-1"}}})
	if plain.Footer != "" {
		t.Fatalf("missing conditional keys fabricated footer %q", plain.Footer)
	}
}

func TestFindingViewModelDoesNotRenderUnevaluatedAsZero(t *testing.T) {
	model := findingListViewModel(listEnvelope{Rows: []map[string]any{
		{"id": "f-1", "ticket": "AIRA-1", "severity": "P1", "disposition": "open", "unevaluated": true, "error": "index unestablished"},
	}})
	if len(model.Rows) != 1 || model.Rows[0].Style != "unevaluated" || !strings.Contains(model.Rows[0].Cells[4], "UNEVALUATED") || strings.Contains(model.Rows[0].Cells[4], "0") {
		t.Fatalf("finding model=%#v", model)
	}
}

func TestGaugeTilesRenderValueUnevaluatedAndPerTileError(t *testing.T) {
	tiles := insightViewModel([]gaugeFetch{
		{Result: store.GaugeResult{Name: "wip", Kind: store.GaugeKindCount, Value: 0, Direction: "lower-is-better", Baseline: 2}},
		{Result: store.GaugeResult{Name: "flaky", Kind: store.GaugeKindRate, Unevaluated: true, UnevaluatedReason: "no reports"}},
		{Name: "quota", ErrorCode: "E_CONFIG_INVALID"},
	}).Tiles
	if len(tiles) != 3 || tiles[0].Value != "0 count" || tiles[0].Direction != "lower-is-better" || tiles[0].Baseline != "2" {
		t.Fatalf("value tile=%#v", tiles)
	}
	if tiles[1].Value != "UNEVALUATED" || tiles[1].Reason != "no reports" || !tiles[1].Unevaluated {
		t.Fatalf("unevaluated tile=%#v", tiles[1])
	}
	if tiles[2].ErrorCode != "E_CONFIG_INVALID" || tiles[2].Value != "ERROR" {
		t.Fatalf("error tile=%#v", tiles[2])
	}
}

func TestLeaseViewModelDistinguishesExpiredAndPriorBoot(t *testing.T) {
	model := leaseListViewModel([]store.HeldLeaseRow{
		{TicketID: "AIRA-1", Actor: "alice", WorktreeID: "wt-a", Generation: 2, TTLNanos: 100, Expired: true, AgeNote: "250ns"},
		{TicketID: "AIRA-2", Actor: "bob", WorktreeID: "wt-b", Generation: 3, TTLNanos: 100, Expired: true, AgeNote: "stale (prior boot)"},
	}, map[string]string{"AIRA-1": "captured-token"})
	if model.Rows[0].Cells[5] != "EXPIRED" || !strings.Contains(model.Rows[0].Cells[6], "as of last refresh") {
		t.Fatalf("expired row=%#v", model.Rows[0])
	}
	if model.Rows[1].Cells[5] != "STALE" || !strings.Contains(model.Rows[1].Cells[6], "stale (prior boot)") || strings.Contains(model.Rows[1].Cells[6], "0s") {
		t.Fatalf("prior boot row=%#v", model.Rows[1])
	}
	if model.Rows[0].LeaseToken != "captured-token" || model.Rows[0].LeaseVersion != 2 {
		t.Fatalf("lease action snapshot=%#v", model.Rows[0])
	}
}
