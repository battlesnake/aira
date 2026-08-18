package store

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"aira/internal/domain"
	"aira/internal/gitcontext"
)

func TestRantIdempotencyByteIdentityAndGitContext(t *testing.T) {
	s, root := rantTestStore(t)
	ticket, err := s.CreateTicket(context.Background(), domain.CreateTicketInput{Title: "rant reference", Kind: domain.KindFeature, Severity: domain.SeverityP2})
	if err != nil {
		t.Fatal(err)
	}
	body := "  fake-token ghp_not_a_real_secret\nkeep trailing  \n"
	ctx := gitcontext.GitContext{
		RepoRoot:     gitcontext.Field{Value: root, Status: gitcontext.StatusValue},
		WorktreePath: gitcontext.Field{Value: root, Status: gitcontext.StatusValue},
		WorktreeID:   gitcontext.Field{Value: "wt-rant", Status: gitcontext.StatusValue},
		HeadHash:     gitcontext.Field{Value: strings.Repeat("a", 40), Status: gitcontext.StatusValue},
		HeadRef:      gitcontext.Field{Value: "refs/heads/main", Status: gitcontext.StatusValue},
		RemoteURL:    gitcontext.Field{Value: "https://example.test/repo.git", Status: gitcontext.StatusValue},
		ObservedAt:   "2026-08-18T10:00:00Z", ResolverVersion: gitcontext.ResolverVersion,
	}
	refs := []domain.RantRef{{Kind: domain.RantRefTicket, ID: ticket.ID}}
	input := domain.RantInput{Body: body, Tags: []string{"Slow_Tests", "infra"}, Severity: domain.RantSeverityBlocker, Refs: refs, IdempotencyKey: "attempt-1", Actor: "terra"}
	first, err := s.AddRant(context.Background(), input, ctx)
	if err != nil {
		t.Fatal(err)
	}
	second, err := s.AddRant(context.Background(), domain.RantInput{Body: body, Tags: []string{"infra", "slow tests"}, Severity: domain.RantSeverityBlocker, Refs: refs, IdempotencyKey: "attempt-1", Actor: "different-retry-metadata"}, gitcontext.GitContext{})
	if err != nil {
		t.Fatal(err)
	}
	if !second.Idempotent || second.Rant.ID != first.Rant.ID {
		t.Fatalf("retry = %#v, first = %#v", second, first)
	}
	got, err := s.GetRant(first.Rant.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Body != body || got.GitContext != ctx {
		t.Fatalf("stored identity changed: body=%q context=%#v", got.Body, got.GitContext)
	}
	conflicts := map[string]domain.RantInput{
		"body":     {Body: body + "changed", Tags: input.Tags, Severity: input.Severity, Refs: refs, IdempotencyKey: input.IdempotencyKey},
		"tags":     {Body: body, Tags: []string{"other"}, Severity: input.Severity, Refs: refs, IdempotencyKey: input.IdempotencyKey},
		"severity": {Body: body, Tags: input.Tags, Severity: domain.RantSeverityAnnoyance, Refs: refs, IdempotencyKey: input.IdempotencyKey},
		"refs":     {Body: body, Tags: input.Tags, Severity: input.Severity, IdempotencyKey: input.IdempotencyKey},
	}
	for name, conflict := range conflicts {
		t.Run("conflicting-"+name, func(t *testing.T) {
			_, err := s.AddRant(context.Background(), conflict, ctx)
			if ErrorCode(err) != domain.CodeRantIdempotencyConflict {
				t.Fatalf("conflict error = %v", err)
			}
		})
	}
}

func TestRantReviewsAppendAndReviewedIsDerived(t *testing.T) {
	s, _ := rantTestStore(t)
	added := mustAddRant(t, s, domain.RantInput{Body: "review me", Tags: []string{"slow-tests"}, Actor: "terra"})
	if added.Rant.Reviewed {
		t.Fatal("new rant reported reviewed")
	}
	first, err := s.ReviewRant(context.Background(), added.Rant.ID, domain.RantReviewInput{Reviewer: "owner", Outcome: domain.RantOutcomePlanned, Note: "make a ticket"})
	if err != nil {
		t.Fatal(err)
	}
	second, err := s.ReviewRant(context.Background(), added.Rant.ID, domain.RantReviewInput{Reviewer: "owner", Outcome: domain.RantOutcomeNeedsEvidence})
	if err != nil {
		t.Fatal(err)
	}
	if first.Review.ID == second.Review.ID {
		t.Fatal("review was overwritten instead of appended")
	}
	got, err := s.GetRant(added.Rant.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !got.Reviewed || len(got.Reviews) != 2 || got.Reviews[0].Outcome != domain.RantOutcomePlanned || got.Reviews[1].Outcome != domain.RantOutcomeNeedsEvidence {
		t.Fatalf("reviews = %#v", got.Reviews)
	}
	if _, err := s.db.Exec(`UPDATE rant_reviews SET outcome='actioned' WHERE project_id=? AND rant_id=?`, s.projectID, added.Rant.ID); err == nil {
		t.Fatal("database allowed an existing review to be updated")
	}
	if _, err := s.db.Exec(`DELETE FROM rant_reviews WHERE project_id=? AND rant_id=?`, s.projectID, added.Rant.ID); err == nil {
		t.Fatal("database allowed an existing review to be deleted")
	}
}

func TestRantJournalCarriesEventHeadersOnly(t *testing.T) {
	s, _ := rantTestStore(t)
	body := "journal-must-not-carry-this-rant-body"
	note := "journal-must-not-carry-this-review-note"
	added := mustAddRant(t, s, domain.RantInput{Body: body, Actor: "terra"})
	if _, err := s.ReviewRant(context.Background(), added.Rant.ID, domain.RantReviewInput{Reviewer: "owner", Outcome: domain.RantOutcomeActioned, Note: note}); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(s.auditDir, "journal.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	journal := string(data)
	if !strings.Contains(journal, `"verb":"rant.create"`) || !strings.Contains(journal, `"verb":"rant.reviewed"`) {
		t.Fatalf("missing rant event headers: %s", journal)
	}
	if strings.Contains(journal, body) || strings.Contains(journal, note) {
		t.Fatalf("journal leaked DB-only rant prose: %s", journal)
	}
}

func TestRantLoopCloserContainsNoProse(t *testing.T) {
	s, _ := rantTestStore(t)
	prior := mustAddRant(t, s, domain.RantInput{Body: "secret fake token ghp_dont_surface", Tags: []string{"slow-tests"}, Actor: "one"})
	if _, err := s.ReviewRant(context.Background(), prior.Rant.ID, domain.RantReviewInput{Reviewer: "owner", Outcome: domain.RantOutcomeWontFix, Note: "private note prose"}); err != nil {
		t.Fatal(err)
	}
	current := mustAddRant(t, s, domain.RantInput{Body: "again", Tags: []string{"slow_tests"}, Actor: "two"})
	data, err := json.Marshal(current.SharingRecordedTags)
	if err != nil {
		t.Fatal(err)
	}
	text := string(data)
	if strings.Contains(text, "secret") || strings.Contains(text, "private note") || strings.Contains(text, "ghp_") {
		t.Fatalf("loop closer leaked prose: %s", text)
	}
	if len(current.SharingRecordedTags) != 1 || current.SharingRecordedTags[0].RantID != prior.Rant.ID || current.SharingRecordedTags[0].Tag != "slow-tests" || current.SharingRecordedTags[0].Outcome != domain.RantOutcomeWontFix {
		t.Fatalf("loop closer = %#v", current.SharingRecordedTags)
	}
}

func TestRantRedactKeepsIdentityProvenanceAndContext(t *testing.T) {
	s, root := rantTestStore(t)
	ctx := gitcontext.GitContext{RepoRoot: gitcontext.Field{Value: root, Status: gitcontext.StatusValue}, ObservedAt: "2026-08-18T10:00:00Z", ResolverVersion: "v-test"}
	added, err := s.AddRant(context.Background(), domain.RantInput{Body: "paste contained a secret", Tags: []string{"security"}, Actor: "terra"}, ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.RedactRant(context.Background(), added.Rant.ID); err != nil {
		t.Fatal(err)
	}
	got, err := s.GetRant(added.Rant.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.ID != added.Rant.ID || got.Body != domain.RedactedRantBody || !got.Redacted || got.GitContext.RepoRoot != ctx.RepoRoot || got.GitContext.ObservedAt != ctx.ObservedAt || got.GitContext.ResolverVersion != ctx.ResolverVersion || got.Actor != "terra" {
		t.Fatalf("redacted rant = %#v", got)
	}
}

func TestRantListUnreviewedSinceAndCountByActor(t *testing.T) {
	s, _ := rantTestStore(t)
	one := mustAddRant(t, s, domain.RantInput{Body: "one", Tags: []string{"infra"}, Actor: "terra"})
	two := mustAddRant(t, s, domain.RantInput{Body: "two", Tags: []string{"infra"}, Actor: "opus"})
	if _, err := s.ReviewRant(context.Background(), one.Rant.ID, domain.RantReviewInput{Reviewer: "owner"}); err != nil {
		t.Fatal(err)
	}
	rows, err := s.ListRants(domain.RantListOptions{Unreviewed: true, Since: one.Rant.Seq})
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].ID != two.Rant.ID {
		t.Fatalf("rows = %#v", rows)
	}
	count, err := s.CountRants("", "actor")
	if err != nil {
		t.Fatal(err)
	}
	if count.Total != 2 || count.Groups["terra"].Rants != 1 || count.Groups["opus"].DistinctActors != 1 {
		t.Fatalf("count = %#v", count)
	}
}

func TestRantBoundsAndInProjectReferences(t *testing.T) {
	s, _ := rantTestStore(t)
	tooManyTags := make([]string, domain.MaxRantTags+1)
	for i := range tooManyTags {
		tooManyTags[i] = "tag-" + strings.Repeat("x", i+1)
	}
	if _, err := s.AddRant(context.Background(), domain.RantInput{Body: "body", Tags: tooManyTags}, gitcontext.GitContext{}); ErrorCode(err) != domain.CodeRantInvalid {
		t.Fatalf("tag bound error = %v", err)
	}
	if _, err := s.AddRant(context.Background(), domain.RantInput{Body: "body", Refs: []domain.RantRef{{Kind: domain.RantRefTicket, ID: "AIRA-999"}}}, gitcontext.GitContext{}); ErrorCode(err) != domain.CodeRantRefInvalid {
		t.Fatalf("missing ref error = %v", err)
	}
}

func TestRantBodiesSurviveEverySearchIndexRebuild(t *testing.T) {
	s, _ := rantTestStore(t)
	added := mustAddRant(t, s, domain.RantInput{Body: "distinctive phosphorescent failure", Tags: []string{"search"}})
	for attempt := 0; attempt < 2; attempt++ {
		rows, err := s.Search(context.Background(), "phosphorescent", "rant")
		if err != nil {
			t.Fatal(err)
		}
		if len(rows) != 1 || rows[0].ID != added.Rant.ID || rows[0].Kind != "rant" {
			t.Fatalf("search attempt %d = %#v", attempt, rows)
		}
	}
}

func rantTestStore(t *testing.T) (*Store, string) {
	t.Helper()
	base := t.TempDir()
	root := filepath.Join(base, "repo")
	common := filepath.Join(root, ".git")
	if err := os.MkdirAll(common, 0o755); err != nil {
		t.Fatal(err)
	}
	s, err := Open(context.Background(), Options{Root: root, CommonDir: common, DBPath: filepath.Join(base, "state.db"), RegistryPath: filepath.Join(base, "registry.jsonl"), ProjectID: "project-rant", WorktreeID: "wt-rant", ProjectSlug: "rant", Prefixes: []string{"AIRA"}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s, root
}

func mustAddRant(t *testing.T, s *Store, input domain.RantInput) RantAddResult {
	t.Helper()
	result, err := s.AddRant(context.Background(), input, gitcontext.GitContext{})
	if err != nil {
		t.Fatal(err)
	}
	return result
}
