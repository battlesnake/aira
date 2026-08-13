package store

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"aira/internal/domain"
)

func insightFinding(t *testing.T, s *Store, source string, verdict domain.Verdict, category string) {
	t.Helper()
	_, _, err := s.AddFinding(context.Background(), domain.ReviewFindingInput{
		TicketID: "AIRA-1", Category: category, Severity: domain.SeverityP1,
		Verdict: verdict, Source: source, Message: source + " " + string(verdict) + " " + category,
	})
	if err != nil {
		t.Fatal(err)
	}
}

func insightI64(value int64) *int64 { return &value }

func TestInsightEmptyUniverseIsUnevaluatedWithUniverse(t *testing.T) {
	base := t.TempDir()
	s := testStore(t, base, base+"/common", base+"/state")
	results, err := s.ComputeAllGauges()
	if err != nil || len(results) != 6 {
		t.Fatalf("gauges=%#v err=%v", results, err)
	}
	for _, result := range results {
		if !result.Unevaluated || result.Universe.Count != 0 || result.Universe.Scope == "" || result.Universe.AsOf == nil || result.Universe.At == "" || result.Value != nil {
			t.Fatalf("empty gauge=%#v", result)
		}
	}
}

func TestReviewerVerdictDistinguishesRealZeroUnevaluatedAndAbsent(t *testing.T) {
	base := t.TempDir()
	s := testStore(t, base, base+"/common", base+"/state")
	insightFinding(t, s, "confirmed-only", domain.VerdictConfirmed, "bug")
	insightFinding(t, s, "plausible-only", domain.VerdictPlausible, "bug")
	result, err := s.ComputeGauge("reviewer-verdict-ratio")
	if err != nil {
		t.Fatal(err)
	}
	zero := result.Breakdown["confirmed-only"]
	if zero.Unevaluated || zero.Value != float64(0) || zero.Counts["refuted"] != 0 {
		t.Fatalf("real zero cell=%#v", zero)
	}
	unknown := result.Breakdown["plausible-only"]
	if !unknown.Unevaluated || unknown.Value != nil {
		t.Fatalf("plausible-only cell=%#v", unknown)
	}
	if _, present := result.Breakdown["never-seen"]; present {
		t.Fatal("absent source became a breakdown cell")
	}
}

func TestFlakyRateCountsIdentityCellsNotTestAggregates(t *testing.T) {
	base := t.TempDir()
	s := testStore(t, base, base+"/common", base+"/state")
	addJUnitEvidence(t, s, "pass", "commit-a", "suite-a", "cfg-a", "env-a", "1/1", 0)
	addJUnitEvidence(t, s, "fail", "commit-a", "suite-a", "cfg-a", "env-a", "1/1", 0)
	addJUnitEvidence(t, s, "pass", "commit-b", "suite-a", "cfg-a", "env-a", "1/1", 0)
	result, err := s.ComputeGauge("flaky-rate")
	if err != nil {
		t.Fatal(err)
	}
	if result.Unevaluated || result.Value != float64(1) || result.Universe.Count != 2 || result.Breakdown["flaky"].Count != 1 || result.Breakdown["unevaluated"].Count != 1 {
		t.Fatalf("cell-exact flaky=%#v", result)
	}
}

func TestAllAbsentComputeBucketsStayUnevaluatedAndWatermarkMoves(t *testing.T) {
	base := t.TempDir()
	s := testStore(t, base, base+"/common", base+"/state")
	_, err := s.AddComputeEvent(context.Background(), domain.ComputeEventInput{Phase: "plan", Model: "manual", Provider: "mystery", Source: "manual", Raw: domain.RawUsage{Buckets: &domain.ComputeBuckets{}}})
	if err != nil {
		t.Fatal(err)
	}
	first, err := s.ComputeGauge("review-loop-economics")
	if err != nil {
		t.Fatal(err)
	}
	if first.Unevaluated || !first.Breakdown["plan"].Unevaluated || first.Breakdown["plan"].Value != nil {
		t.Fatalf("all absent phase=%#v", first)
	}
	firstSeq := first.Universe.AsOf["compute_at_seq"]
	_, err = s.AddComputeEvent(context.Background(), domain.ComputeEventInput{Phase: "plan", Model: "manual", Provider: "mystery", Source: "manual", Raw: domain.RawUsage{Buckets: &domain.ComputeBuckets{Output: insightI64(4)}}})
	if err != nil {
		t.Fatal(err)
	}
	second, err := s.ComputeGauge("review-loop-economics")
	if err != nil {
		t.Fatal(err)
	}
	if second.Breakdown["plan"].Unevaluated || second.Universe.AsOf["compute_at_seq"] == firstSeq {
		t.Fatalf("live compute gauge first=%#v second=%#v", first, second)
	}
}

func TestQuotaBurnPartialFieldsAndDirection(t *testing.T) {
	base := t.TempDir()
	s := testStore(t, base, base+"/common", base+"/state")
	_, err := s.AddQuotaSnapshot(context.Background(), domain.QuotaSnapshotInput{Provider: "openai", Source: "manual", Used: insightI64(10)})
	if err != nil {
		t.Fatal(err)
	}
	one, err := s.ComputeGauge("quota-burn")
	if err != nil || one.Breakdown["openai"].Direction != "unevaluated" || one.Breakdown["openai"].Unevaluated || !one.Breakdown["openai"].Fields["limit"].Unevaluated || one.Breakdown["openai"].Fields["used"].Value != int64(10) {
		t.Fatalf("one quota snapshot=%#v err=%v", one, err)
	}
	_, err = s.AddQuotaSnapshot(context.Background(), domain.QuotaSnapshotInput{Provider: "openai", Source: "manual", Used: insightI64(15), Limit: insightI64(100), Remaining: insightI64(85)})
	if err != nil {
		t.Fatal(err)
	}
	two, err := s.ComputeGauge("quota-burn")
	if err != nil || two.Breakdown["openai"].Direction != "up" || two.Breakdown["openai"].Fields["burn"].Value != int64(5) {
		t.Fatalf("two quota snapshots=%#v err=%v", two, err)
	}
}

func TestWIPIncludesEveryNonTerminalStatus(t *testing.T) {
	base := t.TempDir()
	s := testStore(t, base, base+"/common", base+"/state")
	for _, status := range []domain.Status{domain.StatusDraft, domain.StatusPlanned, domain.StatusInProgress, domain.StatusInReview} {
		ticket, err := s.CreateTicket(context.Background(), domain.CreateTicketInput{Title: string(status), Kind: domain.KindFeature, Severity: domain.SeverityP2})
		if err != nil {
			t.Fatal(err)
		}
		switch status {
		case domain.StatusDraft:
			data, renderErr := domain.RenderTicket(domain.Ticket{Schema: ticket.Schema, ID: ticket.ID, Project: ticket.Project, Title: ticket.Title, Status: status, Kind: ticket.Kind, Severity: ticket.Severity, Labels: ticket.Labels, Relations: ticket.Relations}, "")
			if renderErr != nil {
				t.Fatal(renderErr)
			}
			if writeErr := os.WriteFile(s.ticketPath(ticket.ID), data, 0o644); writeErr != nil {
				t.Fatal(writeErr)
			}
		case domain.StatusInProgress:
			if _, err := s.MoveTicket(context.Background(), ticket.ID, status); err != nil {
				t.Fatal(err)
			}
		case domain.StatusInReview:
			if _, err := s.MoveTicket(context.Background(), ticket.ID, domain.StatusInProgress); err != nil {
				t.Fatal(err)
			}
			if _, err := s.MoveTicket(context.Background(), ticket.ID, status); err != nil {
				t.Fatal(err)
			}
		}
	}
	done, err := s.CreateTicket(context.Background(), domain.CreateTicketInput{Title: "done", Kind: domain.KindFeature, Severity: domain.SeverityP2})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.MoveTicket(context.Background(), done.ID, domain.StatusInProgress); err != nil {
		t.Fatal(err)
	}
	if _, err := s.MoveTicket(context.Background(), done.ID, domain.StatusInReview); err != nil {
		t.Fatal(err)
	}
	if _, err := s.MoveTicket(context.Background(), done.ID, domain.StatusDone); err != nil {
		t.Fatal(err)
	}
	result, err := s.ComputeGauge("wip")
	if err != nil || result.Universe.Count != 4 {
		t.Fatalf("wip=%#v err=%v", result, err)
	}
	for _, status := range []domain.Status{domain.StatusDraft, domain.StatusPlanned, domain.StatusInProgress, domain.StatusInReview} {
		if result.Breakdown[string(status)].Count != 1 {
			t.Fatalf("missing status %s: %#v", status, result)
		}
	}
}

func TestCountFindingsIsUncappedForDrilldown(t *testing.T) {
	base := t.TempDir()
	s := testStore(t, base, base+"/common", base+"/state")
	for i := 0; i < 60; i++ {
		insightFinding(t, s, "source-"+string(rune('a'+i/26))+string(rune('a'+i%26)), domain.VerdictConfirmed, "recurring")
	}
	result, err := s.CountFindings("", "source")
	if err != nil || result.Total != 60 || len(result.Distribution) != 60 {
		t.Fatalf("uncapped findings=%#v err=%v", result, err)
	}
}

// TestEconomicsCostOnlyPhaseTokenCellUnevaluated verifies Sol build-review P1-1:
// a phase whose events carry cost but no token buckets has an unevaluated token
// cell (never a fake 0 tokens) while cost stays an independently-evaluated field.
func TestEconomicsCostOnlyPhaseTokenCellUnevaluated(t *testing.T) {
	base := t.TempDir()
	s := testStore(t, base, base+"/common", base+"/state")
	cost := 1.25
	if _, err := s.AddComputeEvent(context.Background(), domain.ComputeEventInput{
		Phase: "plan", Model: "manual", Provider: "mystery", Source: "manual",
		Raw: domain.RawUsage{Buckets: &domain.ComputeBuckets{}}, CostUSD: &cost,
	}); err != nil {
		t.Fatal(err)
	}
	result, err := s.ComputeGauge("review-loop-economics")
	if err != nil {
		t.Fatal(err)
	}
	cell := result.Breakdown["plan"]
	if !cell.Unevaluated || cell.UnevaluatedReason == "" {
		t.Fatalf("cost-only phase token cell must be unevaluated: %#v", cell)
	}
	if cell.Fields["cost_usd"].Unevaluated || cell.Fields["cost_usd"].Value != cost {
		t.Fatalf("cost field must be evaluated: %#v", cell.Fields["cost_usd"])
	}
}

// TestReviewerReadsFindingsExactlyOnce verifies Sol P1-2 discriminatingly: the
// reviewer gauge scans finding files EXACTLY ONCE. The pre-fix code scanned once
// for the source distribution plus once per source for verdicts (N+1 scans),
// which a concurrent mutation could tear; this asserts the single-scan fix and
// fails against that multi-scan implementation.
func TestReviewerReadsFindingsExactlyOnce(t *testing.T) {
	base := t.TempDir()
	s := testStore(t, base, base+"/common", base+"/state")
	insightFinding(t, s, "codex", domain.VerdictConfirmed, "c1")
	insightFinding(t, s, "codex", domain.VerdictRefuted, "c2")
	insightFinding(t, s, "fable", domain.VerdictConfirmed, "c3")
	scans := 0
	s.findingScanHook = func() { scans++ }
	defer func() { s.findingScanHook = nil }()
	result, err := s.ComputeGauge("reviewer-verdict-ratio")
	if err != nil {
		t.Fatal(err)
	}
	if scans != 1 {
		t.Fatalf("reviewer gauge scanned findings %d times, want exactly 1 (the pre-fix multi-scan was N+1)", scans)
	}
	sum := 0
	for _, cell := range result.Breakdown {
		sum += cell.Count
	}
	if sum != result.Universe.Count || result.Universe.Count != 3 {
		t.Fatalf("universe %d != sum of source cell counts %d", result.Universe.Count, sum)
	}
}

// TestReviewerExcludesMalformedFindingNoNoneSource verifies Sol P1-3: a
// malformed finding file is excluded from the distribution (never fabricates a
// "(none)"/empty source cell) and is surfaced as an overall reason.
func TestReviewerExcludesMalformedFindingNoNoneSource(t *testing.T) {
	base := t.TempDir()
	s := testStore(t, base, base+"/common", base+"/state")
	insightFinding(t, s, "codex", domain.VerdictConfirmed, "good")
	dir := filepath.Join(base, ".aira", "findings")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "f-broken.md"), []byte("not valid frontmatter\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	result, err := s.ComputeGauge("reviewer-verdict-ratio")
	if err != nil {
		t.Fatal(err)
	}
	for source := range result.Breakdown {
		if source == "" || source == "(none)" {
			t.Fatalf("malformed finding fabricated a source cell %q", source)
		}
	}
	if _, ok := result.Breakdown["codex"]; !ok {
		t.Fatalf("valid codex finding missing: %#v", result.Breakdown)
	}
	if result.Universe.Count != 1 || result.UnevaluatedReason == "" {
		t.Fatalf("malformed exclusion not surfaced: universe=%d reason=%q", result.Universe.Count, result.UnevaluatedReason)
	}
}

// TestQuotaBurnUniverseCountsProviders verifies Sol P2-1: the quota universe
// counts distinct providers (what the gauge evaluates), not raw snapshots.
func TestQuotaBurnUniverseCountsProviders(t *testing.T) {
	base := t.TempDir()
	s := testStore(t, base, base+"/common", base+"/state")
	if _, err := s.AddQuotaSnapshot(context.Background(), domain.QuotaSnapshotInput{Provider: "openai", Source: "manual", Used: insightI64(10)}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AddQuotaSnapshot(context.Background(), domain.QuotaSnapshotInput{Provider: "openai", Source: "manual", Used: insightI64(20)}); err != nil {
		t.Fatal(err)
	}
	result, err := s.ComputeGauge("quota-burn")
	if err != nil {
		t.Fatal(err)
	}
	if result.Universe.Count != 1 {
		t.Fatalf("two snapshots one provider => universe should be 1 provider, got %d", result.Universe.Count)
	}
}

// TestInsightDrilldownReproducesValueUncapped verifies Sol P2-2: each gauge's
// emitted drilldown reproduces the gauge's value via the uncapped read, with
// >50 rows (a 50-capped ls would diverge).
func TestInsightDrilldownReproducesValueUncapped(t *testing.T) {
	base := t.TempDir()
	s := testStore(t, base, base+"/common", base+"/state")
	confirmed, refuted := 0, 0
	for i := 0; i < 60; i++ {
		v := domain.VerdictConfirmed
		if i%2 == 0 {
			v, refuted = domain.VerdictRefuted, refuted+1
		} else {
			confirmed++
		}
		insightFinding(t, s, "codex", v, fmt.Sprintf("cat-%02d", i))
	}
	result, err := s.ComputeGauge("reviewer-verdict-ratio")
	if err != nil {
		t.Fatal(err)
	}
	cell := result.Breakdown["codex"]
	q := strings.Fields(cell.Drilldown.Query) // "source:codex --by verdict"
	drill, err := s.CountFindings(q[0], q[2])
	if err != nil {
		t.Fatal(err)
	}
	if drill.Total <= 50 {
		t.Fatalf("expected >50 rows to prove the uncapped path, got %d", drill.Total)
	}
	if drill.Distribution["confirmed"] != cell.Counts["confirmed"] || drill.Distribution["refuted"] != cell.Counts["refuted"] {
		t.Fatalf("drilldown %#v does not reproduce cell counts %#v", drill.Distribution, cell.Counts)
	}
	if cell.Counts["confirmed"] != confirmed || cell.Counts["refuted"] != refuted {
		t.Fatalf("cell counts %#v want confirmed=%d refuted=%d", cell.Counts, confirmed, refuted)
	}
}
