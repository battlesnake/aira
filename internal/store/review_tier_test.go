package store

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"aira/internal/domain"
)

func reviewTierPtr(value int) *int { return &value }

func reviewTierPolicy(defaultTier int, rules ...ReviewPathTier) ReviewPolicy {
	return ReviewPolicy{Configured: true, DefaultTier: reviewTierPtr(defaultTier), PathTiers: rules}
}

func TestRecommendReviewTierUsesMaximumAcrossPaths(t *testing.T) {
	policy := reviewTierPolicy(2,
		ReviewPathTier{Glob: "docs/**", Tier: 0},
		ReviewPathTier{Glob: "internal/store/**", Tier: 3},
	)
	got, err := RecommendReviewTier([]string{"docs/x.md", "internal/store/g.go"}, "feature", "P2", policy)
	if err != nil || got.Tier != 3 {
		t.Fatalf("recommendation=%#v err=%v, want tier 3", got, err)
	}
	if len(got.Basis) == 0 || !sortReviewBasis(got.Basis) {
		t.Fatalf("basis=%#v is not non-empty and sorted", got.Basis)
	}
}

func TestRecommendReviewTierDefaultsUnmatchedAndNoPathsUp(t *testing.T) {
	policy := reviewTierPolicy(2, ReviewPathTier{Glob: "docs/**", Tier: 0})
	for name, paths := range map[string][]string{
		"unmatched": {"internal/unknown.go"},
		"none":      {},
	} {
		t.Run(name, func(t *testing.T) {
			got, err := RecommendReviewTier(paths, "feature", "P2", policy)
			if err != nil || got.Tier != 2 {
				t.Fatalf("recommendation=%#v err=%v, want tier 2", got, err)
			}
			want := "no-paths ⇒ default_tier"
			if name == "unmatched" {
				want = "path internal/unknown.go unmatched ⇒ default_tier"
			}
			if !containsReviewBasis(got.Basis, want) {
				t.Fatalf("basis=%#v, missing %q", got.Basis, want)
			}
		})
	}
}

func TestRecommendReviewTierFloorsOnlyRaise(t *testing.T) {
	policy := reviewTierPolicy(1,
		ReviewPathTier{Glob: "docs/**", Tier: 0},
		ReviewPathTier{Glob: "internal/**", Tier: 3},
	)
	policy.KindFloor = map[domain.Kind]int{domain.KindBug: 2, domain.KindChore: 0}
	policy.SeverityFloor = map[domain.Severity]int{domain.SeverityP0: 3}
	got, err := RecommendReviewTier([]string{"docs/x.md"}, "bug", "P2", policy)
	if err != nil || got.Tier != 2 {
		t.Fatalf("kind floor recommendation=%#v err=%v", got, err)
	}
	got, err = RecommendReviewTier([]string{"internal/x.go"}, "chore", "P2", policy)
	if err != nil || got.Tier != 3 {
		t.Fatalf("high path recommendation=%#v err=%v", got, err)
	}
	got, err = RecommendReviewTier([]string{"docs/x.md"}, "feature", "P0", policy)
	if err != nil || got.Tier != 3 {
		t.Fatalf("severity floor recommendation=%#v err=%v", got, err)
	}
}

func TestRecommendReviewTierAbsentPolicyDefaultsUp(t *testing.T) {
	got, err := RecommendReviewTier([]string{"docs/x.md"}, "feature", "P2", ReviewPolicy{})
	if err != nil || got.Tier != 3 || !containsReviewBasis(got.Basis, "no-policy-configured") {
		t.Fatalf("recommendation=%#v err=%v", got, err)
	}
}

func TestRecommendReviewTierUnknownKindAndSeverityDefaultUpIndependently(t *testing.T) {
	policy := reviewTierPolicy(2, ReviewPathTier{Glob: "docs/**", Tier: 0})
	for _, tc := range []struct {
		name, kind, severity, basis string
	}{
		{"kind", "grbg", "P2", "unrecognised-kind ⇒ default_tier"},
		{"empty-kind", "", "P2", "unrecognised-kind ⇒ default_tier"},
		{"severity", "feature", "P9", "unrecognised-severity ⇒ default_tier"},
		{"empty-severity", "feature", "", "unrecognised-severity ⇒ default_tier"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := RecommendReviewTier([]string{"docs/x.md"}, tc.kind, tc.severity, policy)
			if err != nil || got.Tier != 2 || !containsReviewBasis(got.Basis, tc.basis) {
				t.Fatalf("recommendation=%#v err=%v", got, err)
			}
		})
	}
}

func TestRecommendReviewTierGlobOverlapAndDeterminism(t *testing.T) {
	policy := reviewTierPolicy(1, ReviewPathTier{Glob: "internal/store/**", Tier: 3})
	first, err := RecommendReviewTier([]string{"internal/store/**"}, "feature", "P2", policy)
	if err != nil || first.Tier != 3 {
		t.Fatalf("glob overlap recommendation=%#v err=%v", first, err)
	}
	second, err := RecommendReviewTier([]string{"internal/runner/x.go"}, "feature", "P2", policy)
	if err != nil || second.Tier != 1 {
		t.Fatalf("unrelated recommendation=%#v err=%v", second, err)
	}
	repeat, err := RecommendReviewTier([]string{"internal/store/**"}, "feature", "P2", policy)
	if err != nil || !reflect.DeepEqual(first, repeat) {
		t.Fatalf("recommendation is not deterministic: first=%#v repeat=%#v err=%v", first, repeat, err)
	}
}

func TestReviewPolicyLoadRejectsMalformedPresenceAndDefaultsAbsent(t *testing.T) {
	absent, err := LoadReviewPolicy(nil)
	if err != nil || absent.DefaultTier == nil || *absent.DefaultTier != 3 || absent.Configured {
		t.Fatalf("absent policy=%#v err=%v", absent, err)
	}
	cases := []string{
		`{"default_tier":0}`,
		`{"default_tier":4}`,
		`{"default_tier":2,"path_tiers":null}`,
		`{"default_tier":2,"kind_floor":{"bug":null}}`,
		`{"default_tier":2,"kind_floor":{"typo":1}}`,
		`{"default_tier":2,"path_tiers":[{"glob":"[","tier":1}]}`,
		`{"path_tiers":[]}`,
	}
	for _, raw := range cases {
		t.Run(raw, func(t *testing.T) {
			if _, err := LoadReviewPolicy(json.RawMessage(raw)); err == nil || !strings.HasPrefix(err.Error(), "E_CONFIG_INVALID") {
				t.Fatalf("LoadReviewPolicy(%s) err=%v", raw, err)
			}
		})
	}
	if _, err := LoadReviewPolicy(json.RawMessage(`null`)); err == nil {
		t.Fatal("review:null was accepted")
	}
}

func TestReviewPolicyValidationRejectsZeroDefault(t *testing.T) {
	if _, err := ValidateReviewPolicy(ReviewPolicy{Configured: true, DefaultTier: reviewTierPtr(0)}); err == nil || ErrorCode(err) != "E_CONFIG_INVALID" {
		t.Fatalf("zero default err=%v", err)
	}
}

func TestOpenRejectsMalformedReviewPolicyEagerly(t *testing.T) {
	base := t.TempDir()
	root := filepath.Join(base, "root")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	_, err := Open(context.Background(), Options{
		Root: root, CommonDir: filepath.Join(base, "common"), DBPath: filepath.Join(base, "state", "state.db"),
		RegistryPath: filepath.Join(base, "state", "registry.jsonl"), ProjectID: "project-aira", WorktreeID: "main", ProjectSlug: "aira",
		Prefixes: []string{"AIRA"}, ReviewPolicy: ReviewPolicy{Configured: true, DefaultTier: reviewTierPtr(0)},
	})
	if err == nil || ErrorCode(err) != "E_CONFIG_INVALID" {
		t.Fatalf("Open malformed review policy err=%v", err)
	}
}

func TestTicketAreaGlobsReturnsDistinctCrossGenerationUnion(t *testing.T) {
	base := t.TempDir()
	root := filepath.Join(base, "root")
	s := testStore(t, root, filepath.Join(base, "common"), filepath.Join(base, "state"))
	ticket, err := s.CreateTicket(context.Background(), domain.CreateTicketInput{Title: "hints", Kind: domain.KindFeature, Severity: domain.SeverityP2})
	if err != nil {
		t.Fatal(err)
	}
	for _, row := range []struct {
		worktree   string
		generation int
		glob       string
	}{
		{"w2", 2, "internal/store/**"},
		{"w1", 1, "docs/**"},
		{"w1", 3, "internal/store/**"},
	} {
		if _, err := s.db.Exec(`INSERT INTO area_hints(project_id,ticket_id,worktree_id,generation,glob) VALUES(?,?,?,?,?)`, s.projectID, ticket.ID, row.worktree, row.generation, row.glob); err != nil {
			t.Fatal(err)
		}
	}
	got, err := s.TicketAreaGlobs(ticket.ID)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"docs/**", "internal/store/**"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("TicketAreaGlobs=%#v, want %#v", got, want)
	}
}

func containsReviewBasis(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

func sortReviewBasis(values []string) bool {
	for i := 1; i < len(values); i++ {
		if values[i-1] > values[i] {
			return false
		}
	}
	return true
}
