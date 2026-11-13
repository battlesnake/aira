package store

import (
	"context"
	"fmt"
	"testing"

	"aira/internal/domain"
)

// verifies: AIRA-327 — rant ls lists one honest page of a filtered set.

func rantIDs(rows []domain.Rant) []string {
	ids := make([]string, 0, len(rows))
	for _, row := range rows {
		ids = append(ids, row.ID)
	}
	return ids
}

func mustListRants(t *testing.T, s *Store, options domain.RantListOptions) RantPage {
	t.Helper()
	page, err := s.ListRants(options)
	if err != nil {
		t.Fatalf("ListRants(%+v): %v", options, err)
	}
	if page.Rows == nil {
		t.Fatalf("ListRants(%+v): nil rows, want an empty slice", options)
	}
	return page
}

func TestRantListDefaultIsTheUnreviewedWorkQueue(t *testing.T) {
	s, _ := rantTestStore(t)
	a := mustAddRant(t, s, domain.RantInput{Body: "a", Actor: "t"})
	b := mustAddRant(t, s, domain.RantInput{Body: "b", Actor: "t"})
	c := mustAddRant(t, s, domain.RantInput{Body: "c", Actor: "t"})
	if _, err := s.ReviewRant(context.Background(), b.Rant.ID, domain.RantReviewInput{Reviewer: "owner"}); err != nil {
		t.Fatal(err)
	}
	for name, tc := range map[string]struct {
		status string
		want   []string
	}{
		"zero value": {"", []string{a.ID, c.ID}},
		"unreviewed": {domain.RantStatusUnreviewed, []string{a.ID, c.ID}},
		"reviewed":   {domain.RantStatusReviewed, []string{b.ID}},
		"all":        {domain.RantStatusAll, []string{a.ID, b.ID, c.ID}},
	} {
		page := mustListRants(t, s, domain.RantListOptions{Status: tc.status})
		if got := rantIDs(page.Rows); fmt.Sprint(got) != fmt.Sprint(tc.want) || page.Total != len(tc.want) || page.Truncated || page.NextSince != nil {
			t.Fatalf("%s: rows=%v total=%d truncated=%v next=%v, want %v", name, got, page.Total, page.Truncated, page.NextSince, tc.want)
		}
	}
	if _, err := s.ListRants(domain.RantListOptions{Status: "bogus"}); ErrorCode(err) != domain.CodeRantInvalid {
		t.Fatalf("status bogus = %v, want %s", err, domain.CodeRantInvalid)
	}
}

// The measured failure (2026-11-14): 131 rants, only the first 50 visible and
// nothing said so. A 130-rant walk by next_since must see every rant exactly
// once, in order, with the total counted beyond the limit on every page.
func TestRantListPagingWalksEveryRantExactlyOnce(t *testing.T) {
	s, _ := rantTestStore(t)
	const n = 130
	want := make([]string, 0, n)
	for i := 0; i < n; i++ {
		want = append(want, mustAddRant(t, s, domain.RantInput{Body: fmt.Sprintf("rant %d", i), Actor: "t"}).ID)
	}
	var seen []string
	since, pages := int64(0), 0
	for {
		page := mustListRants(t, s, domain.RantListOptions{Since: since})
		pages++
		if remaining := n - len(seen); page.Total != remaining {
			t.Fatalf("page %d: total=%d, want %d (every match after the cursor, before the limit)", pages, page.Total, remaining)
		}
		seen = append(seen, rantIDs(page.Rows)...)
		if page.Truncated != (page.Total > len(page.Rows)) {
			t.Fatalf("page %d: truncated=%v with total=%d returned=%d", pages, page.Truncated, page.Total, len(page.Rows))
		}
		if !page.Truncated {
			if page.NextSince != nil {
				t.Fatalf("final page carries next_since=%d", *page.NextSince)
			}
			break
		}
		if len(page.Rows) != domain.DefaultRantListLimit {
			t.Fatalf("page %d: %d rows, want the default limit %d", pages, len(page.Rows), domain.DefaultRantListLimit)
		}
		if page.NextSince == nil || *page.NextSince != page.Rows[len(page.Rows)-1].Seq {
			t.Fatalf("page %d: next_since=%v, want the last row's seq", pages, page.NextSince)
		}
		since = *page.NextSince
		if pages > 10 {
			t.Fatal("paging did not terminate")
		}
	}
	if pages != 3 || fmt.Sprint(seen) != fmt.Sprint(want) {
		t.Fatalf("walked %d pages, saw %d rants; want 3 pages and every rant once in order", pages, len(seen))
	}

	// --newest: the LAST limit matches, displayed ascending, no cursor.
	newest := mustListRants(t, s, domain.RantListOptions{Newest: true, Limit: 5})
	if got := rantIDs(newest.Rows); fmt.Sprint(got) != fmt.Sprint(want[n-5:]) {
		t.Fatalf("newest 5 = %v, want %v", got, want[n-5:])
	}
	if newest.Total != n || !newest.Truncated || newest.NextSince != nil {
		t.Fatalf("newest: total=%d truncated=%v next=%v, want %d/true/nil", newest.Total, newest.Truncated, newest.NextSince, n)
	}
	// An explicit limit pages too, and the maximum is honoured.
	if page := mustListRants(t, s, domain.RantListOptions{Limit: domain.MaxRantListLimit}); len(page.Rows) != n || page.Truncated {
		t.Fatalf("limit 200: %d rows truncated=%v, want all %d", len(page.Rows), page.Truncated, n)
	}
	if page := mustListRants(t, s, domain.RantListOptions{Limit: 1}); len(page.Rows) != 1 || page.Rows[0].ID != want[0] || *page.NextSince != page.Rows[0].Seq {
		t.Fatalf("limit 1: %#v", page)
	}
}

func TestRantListLimitBounds(t *testing.T) {
	s, _ := rantTestStore(t)
	for _, limit := range []int{-1, domain.MaxRantListLimit + 1} {
		if _, err := s.ListRants(domain.RantListOptions{Limit: limit}); ErrorCode(err) != domain.CodeRantInvalid {
			t.Fatalf("limit %d = %v, want %s", limit, err, domain.CodeRantInvalid)
		}
	}
}

func TestRantListFiltersAloneAndCombined(t *testing.T) {
	s, _ := rantTestStore(t)
	add := func(body, actor string, severity domain.RantSeverity, tags ...string) string {
		return mustAddRant(t, s, domain.RantInput{Body: body, Actor: actor, Severity: severity, Tags: tags}).ID
	}
	blockerInfra := add("Flaky infra test", "terra", domain.RantSeverityBlocker, "infra")
	papercutLint := add("lint noise", "opus", domain.RantSeverityPapercut, "lint")
	unsetInfra := add("slow flaky build", "opus", "", "infra")
	annoyance := add("confusing help", "terra", domain.RantSeverityAnnoyance)
	reviewedBlocker := add("flaky again", "terra", domain.RantSeverityBlocker, "infra")
	if _, err := s.ReviewRant(context.Background(), reviewedBlocker, domain.RantReviewInput{Reviewer: "owner", Outcome: domain.RantOutcomePlanned}); err != nil {
		t.Fatal(err)
	}
	all := domain.RantStatusAll
	for name, tc := range map[string]struct {
		options domain.RantListOptions
		want    []string
	}{
		"severity blocker (default unreviewed)": {domain.RantListOptions{Severity: "blocker"}, []string{blockerInfra}},
		"severity blocker, all":                 {domain.RantListOptions{Status: all, Severity: "blocker"}, []string{blockerInfra, reviewedBlocker}},
		"severity papercut":                     {domain.RantListOptions{Severity: "papercut"}, []string{papercutLint}},
		"severity annoyance":                    {domain.RantListOptions{Severity: "annoyance"}, []string{annoyance}},
		"severity unset":                        {domain.RantListOptions{Status: all, Severity: "unset"}, []string{unsetInfra}},
		"tag":                                   {domain.RantListOptions{Status: all, Tag: "infra"}, []string{blockerInfra, unsetInfra, reviewedBlocker}},
		"actor":                                 {domain.RantListOptions{Status: all, Actor: "opus"}, []string{papercutLint, unsetInfra}},
		"actor is exact":                        {domain.RantListOptions{Status: all, Actor: "op"}, []string{}},
		"grep case-insensitive":                 {domain.RantListOptions{Status: all, Grep: "FLAKY"}, []string{blockerInfra, unsetInfra, reviewedBlocker}},
		"grep substring":                        {domain.RantListOptions{Status: all, Grep: "aky ag"}, []string{reviewedBlocker}},
		"tag+actor":                             {domain.RantListOptions{Status: all, Tag: "infra", Actor: "terra"}, []string{blockerInfra, reviewedBlocker}},
		"tag+grep+severity":                     {domain.RantListOptions{Status: all, Tag: "infra", Grep: "flaky", Severity: "unset"}, []string{unsetInfra}},
		"reviewed+tag+grep":                     {domain.RantListOptions{Status: domain.RantStatusReviewed, Tag: "infra", Grep: "flaky"}, []string{reviewedBlocker}},
		"everything+since":                      {domain.RantListOptions{Status: all, Tag: "infra", Grep: "flaky", Actor: "terra", Severity: "blocker", Since: 1}, []string{reviewedBlocker}},
	} {
		page := mustListRants(t, s, tc.options)
		if got := rantIDs(page.Rows); fmt.Sprint(got) != fmt.Sprint(tc.want) || page.Total != len(tc.want) {
			t.Fatalf("%s: rows=%v total=%d, want %v", name, got, page.Total, tc.want)
		}
		if page.UnsearchedRedacted != nil && tc.options.Grep == "" {
			t.Fatalf("%s: unsearched_redacted set without --grep", name)
		}
	}
	// The cursor in "everything+since" must be the first rant's seq for the
	// case to mean what it says.
	if first := mustListRants(t, s, domain.RantListOptions{Status: all, Limit: 1}); first.Rows[0].Seq != 1 {
		t.Fatalf("first rant seq = %d, the since case above assumes 1", first.Rows[0].Seq)
	}
	if _, err := s.ListRants(domain.RantListOptions{Severity: "P1"}); ErrorCode(err) != domain.CodeRantInvalid {
		t.Fatalf("severity P1 = %v, want %s", err, domain.CodeRantInvalid)
	}
}

// --grep is literal text: % and _ (LIKE wildcards) and \ match only
// themselves. Each probe matches exactly one body.
func TestRantListGrepTreatsLikeWildcardsLiterally(t *testing.T) {
	s, _ := rantTestStore(t)
	percent := mustAddRant(t, s, domain.RantInput{Body: "coverage stuck at 100% done", Actor: "t"}).ID
	mustAddRant(t, s, domain.RantInput{Body: "coverage stuck at 1000 done", Actor: "t"})
	underscore := mustAddRant(t, s, domain.RantInput{Body: "the a_b flag", Actor: "t"}).ID
	mustAddRant(t, s, domain.RantInput{Body: "the axb flag", Actor: "t"})
	backslash := mustAddRant(t, s, domain.RantInput{Body: `path C:\tmp is wrong`, Actor: "t"}).ID
	mustAddRant(t, s, domain.RantInput{Body: `path C:tmp is wrong`, Actor: "t"})
	for grep, want := range map[string][]string{
		"0% d":    {percent},
		"%":       {percent},
		"a_b":     {underscore},
		"_":       {underscore},
		`C:\tmp`:  {backslash},
		`\`:       {backslash},
		"nothing": {},
	} {
		page := mustListRants(t, s, domain.RantListOptions{Grep: grep})
		if got := rantIDs(page.Rows); fmt.Sprint(got) != fmt.Sprint(want) || page.Total != len(want) {
			t.Fatalf("grep %q = %v (total %d), want %v", grep, got, page.Total, want)
		}
	}
}

// --grep folds Unicode case (SQLite LIKE folds ASCII only), searches a
// whitespace pattern as given, and its total and cursor count every match
// beyond the limit, not just the page.
func TestRantListGrepFoldsUnicodeAndPagesHonestly(t *testing.T) {
	s, _ := rantTestStore(t)
	var matches []domain.Rant
	for i := range 5 {
		matches = append(matches, mustAddRant(t, s, domain.RantInput{Body: fmt.Sprintf("TÖÖ number %d is slow", i), Actor: "t"}).Rant)
		mustAddRant(t, s, domain.RantInput{Body: fmt.Sprintf("too number %d", i), Actor: "t"})
	}
	spaced := mustAddRant(t, s, domain.RantInput{Body: "one  two", Actor: "t"}).ID
	page := mustListRants(t, s, domain.RantListOptions{Grep: "töö", Limit: 2})
	if got := rantIDs(page.Rows); fmt.Sprint(got) != fmt.Sprint([]string{matches[0].ID, matches[1].ID}) || page.Total != 5 || !page.Truncated || page.NextSince == nil || *page.NextSince != matches[1].Seq {
		t.Fatalf("grep töö limit 2 = %v total=%d truncated=%v next=%v", got, page.Total, page.Truncated, page.NextSince)
	}
	next := mustListRants(t, s, domain.RantListOptions{Grep: "töö", Limit: 2, Since: *page.NextSince})
	if got := rantIDs(next.Rows); fmt.Sprint(got) != fmt.Sprint([]string{matches[2].ID, matches[3].ID}) || next.Total != 3 {
		t.Fatalf("grep töö page 2 = %v total=%d", got, next.Total)
	}
	newest := mustListRants(t, s, domain.RantListOptions{Grep: "TÖÖ", Limit: 2, Newest: true})
	if got := rantIDs(newest.Rows); fmt.Sprint(got) != fmt.Sprint([]string{matches[3].ID, matches[4].ID}) || newest.Total != 5 || !newest.Truncated || newest.NextSince != nil {
		t.Fatalf("grep TÖÖ newest 2 = %v total=%d", got, newest.Total)
	}
	if got := rantIDs(mustListRants(t, s, domain.RantListOptions{Grep: "  "}).Rows); fmt.Sprint(got) != fmt.Sprint([]string{spaced}) {
		t.Fatalf("grep two spaces = %v, want only %s", got, spaced)
	}
}

// A redacted rant's body is the sentinel; --grep must neither match the
// sentinel text nor report the scrubbed rant as a miss. It is counted as
// unsearched, and only on a --grep listing.
func TestRantListGrepCountsRedactedRantsAsUnsearched(t *testing.T) {
	s, _ := rantTestStore(t)
	secret := mustAddRant(t, s, domain.RantInput{Body: "my token is hunter2", Actor: "t", Tags: []string{"auth"}})
	mustAddRant(t, s, domain.RantInput{Body: "token refresh is slow", Actor: "t"})
	if _, err := s.RedactRant(context.Background(), secret.ID); err != nil {
		t.Fatal(err)
	}
	page := mustListRants(t, s, domain.RantListOptions{Grep: "token"})
	if len(page.Rows) != 1 || page.Rows[0].ID == secret.ID || page.UnsearchedRedacted == nil || *page.UnsearchedRedacted != 1 {
		t.Fatalf("grep token: rows=%v unsearched=%v, want the live rant and 1 unsearched", rantIDs(page.Rows), page.UnsearchedRedacted)
	}
	// The sentinel text itself is not a match.
	sentinel := mustListRants(t, s, domain.RantListOptions{Grep: domain.RedactedRantBody})
	if len(sentinel.Rows) != 0 || sentinel.Total != 0 || *sentinel.UnsearchedRedacted != 1 {
		t.Fatalf("grep sentinel: rows=%v total=%d", rantIDs(sentinel.Rows), sentinel.Total)
	}
	// The unsearched count honours the other filters.
	if other := mustListRants(t, s, domain.RantListOptions{Grep: "token", Tag: "nope"}); *other.UnsearchedRedacted != 0 {
		t.Fatalf("grep+tag nope: unsearched=%d, want 0", *other.UnsearchedRedacted)
	}
	// Without --grep the redacted rant is listed like any other.
	if plain := mustListRants(t, s, domain.RantListOptions{}); len(plain.Rows) != 2 || plain.UnsearchedRedacted != nil {
		t.Fatalf("plain: rows=%v unsearched=%v", rantIDs(plain.Rows), plain.UnsearchedRedacted)
	}
}
