package store

import (
	"context"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"aira/internal/domain"
)

func computeI64(value int64) *int64 { return &value }

func computeInput(raw domain.RawUsage) domain.ComputeEventInput {
	return domain.ComputeEventInput{Model: "gpt-test", Provider: "openai", Source: "manual", Raw: raw}
}

func TestRunResourceOnlyComputeEventPersistsNilTokenBuckets(t *testing.T) {
	base := t.TempDir()
	s := testStore(t, base, filepath.Join(base, "common"), filepath.Join(base, "state"))
	resources := domain.ResourceUsage{WallMS: computeI64(1250), CPUUser: computeI64(12), CPUSys: computeI64(3), PeakRSS: computeI64(4096)}
	result, err := s.AddComputeEvent(context.Background(), domain.ComputeEventInput{Model: "codex", Source: "run", Raw: domain.RawUsage{Resources: resources}})
	if err != nil {
		t.Fatal(err)
	}
	if result.Event.Provider != "" || result.Event.Conservation != domain.ConservationUnevaluated || result.Event.Buckets.FreshInput != nil || result.Event.Buckets.CacheRead != nil || result.Event.Buckets.CacheWrite != nil || result.Event.Buckets.Output != nil || result.Event.Buckets.Reasoning != nil {
		t.Fatalf("resource-only event fabricated token authority: %+v", result.Event)
	}
	rows, err := s.ListComputeEvents("")
	if err != nil || len(rows) != 1 || !reflect.DeepEqual(rows[0].Resources, resources) {
		t.Fatalf("stored resources=%+v err=%v", rows, err)
	}
}

func TestComputeMismatchIsStoredAndRaisesWarningFinding(t *testing.T) {
	base := t.TempDir()
	s := testStore(t, base, filepath.Join(base, "common"), filepath.Join(base, "state"))
	result, err := s.AddComputeEvent(context.Background(), computeInput(domain.RawUsage{PromptTokens: computeI64(100), CachedTokens: computeI64(0), CompletionTokens: computeI64(20), TotalTokens: computeI64(10)}))
	if err != nil {
		t.Fatalf("mismatched ingest failed: %v", err)
	}
	if result.Event.Conservation != domain.ConservationMismatch || result.ID != "CE-1" {
		t.Fatalf("result = %#v", result)
	}
	events, err := s.ListComputeEvents("")
	if err != nil || len(events) != 1 {
		t.Fatalf("events = %#v err=%v", events, err)
	}
	findings, err := s.ListFindings("subtype:reconciliation")
	if err != nil || len(findings) != 1 || findings[0].Finding.Code != domain.ComputeCodeConservation || findings[0].Finding.Subject != "compute:CE-1" {
		t.Fatalf("findings = %#v err=%v", findings, err)
	}
	report, err := s.Check(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if report.Verdict != "pass" || len(report.Warnings) == 0 || report.Warnings[0].Code != domain.ComputeCodeConservation {
		t.Fatalf("check = %#v", report)
	}
}

func TestCodexDriftStoresCacheWriteOnMismatchEvent(t *testing.T) {
	base := t.TempDir()
	s := testStore(t, base, filepath.Join(base, "common"), filepath.Join(base, "state"))
	result, err := s.AddComputeEvent(context.Background(), domain.ComputeEventInput{
		Model: "codex-test", Provider: "codex", Source: "codex", Raw: domain.RawUsage{
			CodexInputTokens: computeI64(100), CodexCachedInputTokens: computeI64(120),
			CodexCacheWriteInputTokens: computeI64(4), CodexOutputTokens: computeI64(20), CodexTotalTokens: computeI64(0),
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.Event.Conservation != domain.ConservationMismatch || result.Event.Buckets.CacheWrite == nil || *result.Event.Buckets.CacheWrite != 4 {
		t.Fatalf("codex drift event=%#v", result.Event)
	}
	rows, err := s.ListComputeEvents("")
	if err != nil || len(rows) != 1 || rows[0].Buckets.CacheWrite == nil || *rows[0].Buckets.CacheWrite != 4 {
		t.Fatalf("stored codex drift rows=%#v err=%v", rows, err)
	}
}

func TestExplicitAdditiveReasoningFindingUsesStoredContract(t *testing.T) {
	base := t.TempDir()
	s := testStore(t, base, filepath.Join(base, "common"), filepath.Join(base, "state"))
	_, err := s.AddComputeEvent(context.Background(), computeInput(domain.RawUsage{
		Buckets:       &domain.ComputeBuckets{Output: computeI64(10), Reasoning: computeI64(5)},
		ReportedTotal: computeI64(10), ReasoningSubset: false,
	}))
	if err != nil {
		t.Fatal(err)
	}
	findings, err := s.ListFindings("subtype:reconciliation")
	if err != nil || len(findings) != 1 {
		t.Fatalf("findings=%#v err=%v", findings, err)
	}
	if !strings.Contains(findings[0].Finding.Details, "present_sum=15") {
		t.Fatalf("finding details=%q", findings[0].Finding.Details)
	}
}

func TestComputeAddsAreNonIdempotentAndRetainByAtSeq(t *testing.T) {
	base := t.TempDir()
	s, err := Open(context.Background(), Options{Root: base, CommonDir: filepath.Join(base, "common"), DBPath: filepath.Join(base, "state", "state.db"), RegistryPath: filepath.Join(base, "state", "registry.jsonl"), ProjectID: "project-aira", WorktreeID: "main", ProjectSlug: "aira", Prefixes: []string{"AIRA"}, MaxComputeEvents: 2})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	one, err := s.AddComputeEvent(context.Background(), computeInput(domain.RawUsage{PromptTokens: computeI64(10), CachedTokens: computeI64(0), CompletionTokens: computeI64(1), TotalTokens: computeI64(11)}))
	if err != nil {
		t.Fatal(err)
	}
	two, err := s.AddComputeEvent(context.Background(), computeInput(domain.RawUsage{PromptTokens: computeI64(10), CachedTokens: computeI64(0), CompletionTokens: computeI64(1), TotalTokens: computeI64(11)}))
	if err != nil {
		t.Fatal(err)
	}
	if one.ID == two.ID || one.Event.AtSeq == two.Event.AtSeq || one.ID != "CE-1" || two.ID != "CE-2" {
		t.Fatalf("duplicate adds = %#v %#v", one, two)
	}
	three, err := s.AddComputeEvent(context.Background(), computeInput(domain.RawUsage{PromptTokens: computeI64(10), CachedTokens: computeI64(0), CompletionTokens: computeI64(1), TotalTokens: computeI64(11)}))
	if err != nil {
		t.Fatal(err)
	}
	if three.EvictedCount != 1 || three.Remaining != 2 {
		t.Fatalf("retention result = %#v", three)
	}
	events, err := s.ListComputeEvents("")
	if err != nil || len(events) != 2 || events[0].ID != "CE-3" || events[1].ID != "CE-2" {
		t.Fatalf("retained events = %#v err=%v", events, err)
	}
}

func TestComputeFindingDisappearsWhenEventEvicted(t *testing.T) {
	base := t.TempDir()
	s, err := Open(context.Background(), Options{Root: base, CommonDir: filepath.Join(base, "common"), DBPath: filepath.Join(base, "state", "state.db"), RegistryPath: filepath.Join(base, "state", "registry.jsonl"), ProjectID: "project-aira", WorktreeID: "main", ProjectSlug: "aira", Prefixes: []string{"AIRA"}, MaxComputeEvents: 1})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if _, err := s.AddComputeEvent(context.Background(), computeInput(domain.RawUsage{PromptTokens: computeI64(100), CachedTokens: computeI64(0), CompletionTokens: computeI64(20), TotalTokens: computeI64(10)})); err != nil {
		t.Fatal(err)
	}
	findings, err := s.ListFindings("subtype:reconciliation")
	if err != nil || len(findings) != 1 {
		t.Fatalf("initial finding = %#v err=%v", findings, err)
	}
	if _, err := s.AddComputeEvent(context.Background(), computeInput(domain.RawUsage{PromptTokens: computeI64(10), CachedTokens: computeI64(0), CompletionTokens: computeI64(1), TotalTokens: computeI64(11)})); err != nil {
		t.Fatal(err)
	}
	findings, err = s.ListFindings("subtype:reconciliation")
	if err != nil || len(findings) != 0 {
		t.Fatalf("evicted finding = %#v err=%v", findings, err)
	}
}

func TestQuotaSnapshotRoundTripPreservesAbsentFields(t *testing.T) {
	base := t.TempDir()
	s := testStore(t, base, filepath.Join(base, "common"), filepath.Join(base, "state"))
	result, err := s.AddQuotaSnapshot(context.Background(), domain.QuotaSnapshotInput{Provider: "openai", Source: "manual", Used: computeI64(4), Limit: computeI64(10)})
	if err != nil {
		t.Fatal(err)
	}
	if result.ID != "QS-1" || result.Snapshot.AtSeq != 1 {
		t.Fatalf("add = %#v", result)
	}
	rows, err := s.ListQuotaSnapshots("")
	if err != nil || len(rows) != 1 {
		t.Fatalf("rows = %#v err=%v", rows, err)
	}
	if rows[0].Used == nil || *rows[0].Used != 4 || rows[0].Limit == nil || *rows[0].Limit != 10 || rows[0].Remaining != nil {
		t.Fatalf("round trip = %#v", rows[0])
	}
}
