package store

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"aira/internal/domain"
	"aira/internal/gitcontext"
)

func computeI64(value int64) *int64 { return &value }

func computeInput(raw domain.RawUsage) domain.ComputeEventInput {
	return domain.ComputeEventInput{Model: "gpt-test", Provider: "openai", Source: "manual", Raw: raw}
}

func TestComputeEventDBRejectsIllegalGitPairings(t *testing.T) {
	s := testStore(t, t.TempDir(), filepath.Join(t.TempDir(), "common"), t.TempDir())
	insert := `INSERT INTO compute_events(project_id,id,model,provider,at,source,conservation,at_seq,head_hash,head_hash_status,head_ref,head_ref_status,worktree_id,worktree_id_status)
		VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?)`
	tests := []struct {
		name, column, value, status string
	}{
		{name: "head-hash-value-empty", column: "head_hash", status: "value"},
		{name: "head-hash-mismatch-empty", column: "head_hash", status: "mismatch"},
		{name: "head-hash-none-nonempty", column: "head_hash", value: "x", status: "none"},
		{name: "head-hash-unevaluated-nonempty", column: "head_hash", value: "x", status: "unevaluated"},
		{name: "head-ref-value-empty", column: "head_ref", status: "value"},
		{name: "head-ref-mismatch-empty", column: "head_ref", status: "mismatch"},
		{name: "head-ref-none-nonempty", column: "head_ref", value: "x", status: "none"},
		{name: "head-ref-unevaluated-nonempty", column: "head_ref", value: "x", status: "unevaluated"},
		{name: "worktree-value-empty", column: "worktree_id", status: "value"},
		{name: "worktree-mismatch-empty", column: "worktree_id", status: "mismatch"},
		{name: "worktree-none-nonempty", column: "worktree_id", value: "x", status: "none"},
		{name: "worktree-unevaluated-nonempty", column: "worktree_id", value: "x", status: "unevaluated"},
	}
	for i, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			values := map[string]gitcontext.Field{
				"head_hash":   {Status: gitcontext.StatusUnevaluated},
				"head_ref":    {Status: gitcontext.StatusUnevaluated},
				"worktree_id": {Status: gitcontext.StatusUnevaluated},
			}
			values[test.column] = gitcontext.Field{Value: test.value, Status: gitcontext.Status(test.status)}
			_, err := s.db.Exec(insert, s.projectID, fmt.Sprintf("CE-illegal-%d", i), "gpt", "openai", "2026-08-19T00:00:00Z", "manual", "unevaluated", i+1,
				values["head_hash"].Value, values["head_hash"].Status, values["head_ref"].Value, values["head_ref"].Status, values["worktree_id"].Value, values["worktree_id"].Status)
			if err == nil || !strings.Contains(err.Error(), "CHECK constraint failed") {
				t.Fatalf("raw insert error=%v, want CHECK rejection", err)
			}
		})
	}
}

func TestComputeGitContextRoundTripPreservesFourStates(t *testing.T) {
	base := t.TempDir()
	s := testStore(t, base, filepath.Join(base, "common"), filepath.Join(base, "state"))
	input := computeInput(domain.RawUsage{})
	input.GitContext = gitcontext.GitContext{
		RepoRoot:     gitcontext.Field{Value: base, Status: gitcontext.StatusValue},
		WorktreePath: gitcontext.Field{Value: base, Status: gitcontext.StatusValue},
		HeadHash:     gitcontext.Field{Value: "abc123", Status: gitcontext.StatusValue},
		HeadRef:      gitcontext.Field{Value: "refs/heads/other", Status: gitcontext.StatusMismatch},
		WorktreeID:   gitcontext.Field{Status: gitcontext.StatusNone},
	}
	added, err := s.AddComputeEvent(context.Background(), input)
	if err != nil {
		t.Fatal(err)
	}
	want := domain.ComputeGitContextFrom(input.GitContext)
	if !reflect.DeepEqual(added.Event.GitContext, want) {
		t.Fatalf("add git context=%#v want=%#v", added.Event.GitContext, want)
	}
	rows, err := s.ListComputeEvents("")
	if err != nil || len(rows) != 1 || !reflect.DeepEqual(rows[0].GitContext, want) {
		t.Fatalf("round trip rows=%#v err=%v want=%#v", rows, err, want)
	}

	unevaluated := computeInput(domain.RawUsage{})
	if _, err := s.AddComputeEvent(context.Background(), unevaluated); err != nil {
		t.Fatal(err)
	}
	rows, err = s.ListComputeEvents("")
	if err != nil || len(rows) != 2 || rows[0].GitContext.HeadHash.Status != gitcontext.StatusUnevaluated ||
		rows[0].GitContext.HeadRef.Status != gitcontext.StatusUnevaluated || rows[0].GitContext.WorktreeID.Status != gitcontext.StatusUnevaluated {
		t.Fatalf("unstamped context was not normalized to unevaluated: rows=%#v err=%v", rows, err)
	}
}

func TestComputeGitContextCrossCheckIsHonest(t *testing.T) {
	base := t.TempDir()
	s := testStore(t, base, filepath.Join(base, "common"), filepath.Join(base, "state"))
	observed := gitcontext.GitContext{
		RepoRoot:     gitcontext.Field{Value: base, Status: gitcontext.StatusValue},
		WorktreePath: gitcontext.Field{Value: filepath.Join(base, "elsewhere"), Status: gitcontext.StatusValue},
		WorktreeID:   gitcontext.Field{Value: "wrong", Status: gitcontext.StatusValue},
		HeadHash:     gitcontext.Field{Value: "abc123", Status: gitcontext.StatusValue},
		HeadRef:      gitcontext.Field{Status: gitcontext.StatusNone},
	}
	got := s.crossCheckGitContext(observed)
	if got.RepoRoot.Status != gitcontext.StatusValue || got.WorktreePath.Status != gitcontext.StatusMismatch ||
		got.WorktreeID.Status != gitcontext.StatusMismatch || got.HeadHash.Status != gitcontext.StatusValue || got.HeadRef.Status != gitcontext.StatusNone {
		t.Fatalf("cross-check states=%#v", got)
	}
	if got.RemoteURL.Status != gitcontext.StatusUnevaluated || got.RemoteURL.Value != "" {
		t.Fatalf("missing field was fabricated: %#v", got.RemoteURL)
	}
}

const legacyComputeEventsDDL = `CREATE TABLE compute_events (
	project_id TEXT NOT NULL, id TEXT NOT NULL, ticket_id TEXT NOT NULL DEFAULT '',
	phase TEXT NOT NULL DEFAULT '', model TEXT NOT NULL, provider TEXT NOT NULL,
	at TEXT NOT NULL, session TEXT NOT NULL DEFAULT '', agent TEXT NOT NULL DEFAULT '',
	source TEXT NOT NULL, fresh_input INTEGER, cache_read INTEGER, cache_write INTEGER,
	output INTEGER, reasoning INTEGER, reported_total INTEGER, cost_usd REAL,
	conservation TEXT NOT NULL, reasoning_subset INTEGER NOT NULL DEFAULT 0,
	wall_ms INTEGER, cpu_user INTEGER, cpu_sys INTEGER, peak_rss INTEGER,
	at_seq INTEGER NOT NULL, PRIMARY KEY(project_id,id))`

func legacyComputeDB(t *testing.T, partial bool) (string, string) {
	t.Helper()
	base := t.TempDir()
	state := filepath.Join(base, "state")
	if err := os.MkdirAll(state, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(state, "state.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(legacyComputeEventsDDL); err != nil {
		t.Fatal(err)
	}
	if partial {
		if _, err := db.Exec(`ALTER TABLE compute_events ADD COLUMN head_hash TEXT NOT NULL DEFAULT ''`); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := db.Exec(`INSERT INTO compute_events(project_id,id,model,provider,at,source,conservation,at_seq) VALUES('project-aira','CE-old','gpt','openai','2026-08-18T00:00:00Z','manual','unevaluated',1)`); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	return base, path
}

func TestComputeGitContextMigrationPreservesOldRowsAndResumesPartialPair(t *testing.T) {
	for _, partial := range []bool{false, true} {
		t.Run(map[bool]string{false: "legacy", true: "partial-head-hash-pair"}[partial], func(t *testing.T) {
			base, dbPath := legacyComputeDB(t, partial)
			s, err := Open(context.Background(), Options{
				Root: base, CommonDir: filepath.Join(base, "common"), DBPath: dbPath,
				RegistryPath: filepath.Join(base, "state", "registry.jsonl"), ProjectID: "project-aira",
				WorktreeID: "main", ProjectSlug: "aira", Prefixes: []string{"AIRA"},
			})
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = s.Close() })
			for _, column := range []string{"head_hash", "head_hash_status", "head_ref", "head_ref_status", "worktree_id", "worktree_id_status"} {
				if !hasTableColumn(context.Background(), s.db, "compute_events", column) {
					t.Fatalf("migration omitted %s", column)
				}
			}
			rows, err := s.ListComputeEvents("")
			if err != nil || len(rows) != 1 || rows[0].ID != "CE-old" || rows[0].GitContext.HeadHash.Status != gitcontext.StatusUnevaluated ||
				rows[0].GitContext.HeadRef.Status != gitcontext.StatusUnevaluated || rows[0].GitContext.WorktreeID.Status != gitcontext.StatusUnevaluated {
				t.Fatalf("migrated rows=%#v err=%v", rows, err)
			}
		})
	}
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
