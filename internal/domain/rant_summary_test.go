package domain

import (
	"strings"
	"testing"
	"unicode/utf8"
)

// verifies: AIRA-327 — the compact ls row previews the body on a rune boundary.

// The preview length is the ticket's literal 240 runes, asserted here
// independently of RantPreviewRunes so a changed constant is caught.
const previewRunes = 240

func TestRantPreviewIsTwoHundredFortyRunes(t *testing.T) {
	if RantPreviewRunes != previewRunes {
		t.Fatalf("RantPreviewRunes = %d, want %d", RantPreviewRunes, previewRunes)
	}
}

func TestRantSummaryPreviewCutsOnARuneBoundary(t *testing.T) {
	// Two-byte runes: a byte-count cut at 240 would yield only 120 runes, and
	// any odd byte cut would split a rune.
	body := strings.Repeat("é", previewRunes+60)
	summary := Rant{Body: body}.Summary()
	if !summary.BodyTruncated || summary.BodyBytes != len(body) || summary.BodyBytes != 600 {
		t.Fatalf("truncated=%v bytes=%d, want true/600", summary.BodyTruncated, summary.BodyBytes)
	}
	if !utf8.ValidString(summary.Body) || utf8.RuneCountInString(summary.Body) != previewRunes || len(summary.Body) != 480 || summary.Body != strings.Repeat("é", previewRunes) {
		t.Fatalf("preview has %d runes / %d bytes (valid=%v), want exactly 240 / 480", utf8.RuneCountInString(summary.Body), len(summary.Body), utf8.ValidString(summary.Body))
	}
	// A mixed body whose 240th rune is multi-byte.
	mixed := strings.Repeat("a", 239) + "€€tail"
	if got := (Rant{Body: mixed}).Summary(); got.Body != strings.Repeat("a", 239)+"€" || !got.BodyTruncated {
		t.Fatalf("mixed preview = %q truncated=%v", got.Body[len(got.Body)-8:], got.BodyTruncated)
	}
}

func TestRantSummaryKeepsABodyAtTheLimitWhole(t *testing.T) {
	for _, body := range []string{"short", strings.Repeat("ü", previewRunes), strings.Repeat("a", previewRunes)} {
		summary := Rant{Body: body}.Summary()
		if summary.Body != body || summary.BodyTruncated || summary.BodyBytes != 0 {
			t.Fatalf("body of %d runes: preview changed (truncated=%v bytes=%d)", utf8.RuneCountInString(body), summary.BodyTruncated, summary.BodyBytes)
		}
	}
	// One rune past the limit is cut.
	if summary := (Rant{Body: strings.Repeat("a", previewRunes+1)}).Summary(); !summary.BodyTruncated || len(summary.Body) != previewRunes {
		t.Fatalf("241-rune body: truncated=%v preview=%d bytes", summary.BodyTruncated, len(summary.Body))
	}
}

// verifies: AIRA-327 — --grep is a literal, Unicode case-insensitive
// substring; whitespace is searched as given.
func TestRantGrepMatchesUnicodeCaseInsensitively(t *testing.T) {
	for _, tc := range []struct {
		body, pattern string
		want          bool
	}{
		{"TÖÖ is slow", "töö", true},
		{"töö is slow", "TÖÖ", true},
		{"ÅNGSTRÖM units", "ångström", true},
		{"ΣΊΣΥΦΟΣ", "σίσυφος", true},   // final sigma folds with Σ and σ
		{"Kelvin 5\u212a", "5k", true}, // KELVIN SIGN folds with k
		{"flaky AGENT", "aky ag", true},
		{"one  two", "  ", true},
		{"one two", "  ", false},
		{"too slow", "töö", false},   // no accent stripping
		{"strasse", "straße", false}, // simple folding only: ß is not ss
		{"100% done", "0%", true},
		{"1000 done", "0%", false}, // % is literal, not a wildcard
		{"a_b", "a_b", true},
		{"axb", "a_b", false}, // _ is literal
	} {
		if got := RantGrepMatches(tc.body, tc.pattern); got != tc.want {
			t.Fatalf("RantGrepMatches(%q, %q) = %v, want %v", tc.body, tc.pattern, got, tc.want)
		}
	}
}

func TestRantSummaryOutcomeIsTheLatestTypedOutcome(t *testing.T) {
	for name, tc := range map[string]struct {
		reviews []RantReview
		want    RantOutcome
	}{
		"no reviews":             {nil, ""},
		"untyped review":         {[]RantReview{{ID: 1}}, ""},
		"latest typed wins":      {[]RantReview{{ID: 1, Outcome: RantOutcomePlanned}, {ID: 2, Outcome: RantOutcomeDuplicate}}, RantOutcomeDuplicate},
		"later untyped is moot":  {[]RantReview{{ID: 1, Outcome: RantOutcomePlanned}, {ID: 2, Note: "seen"}}, RantOutcomePlanned},
		"reviewed flag survives": {[]RantReview{{ID: 1, Outcome: RantOutcomeWontFix}}, RantOutcomeWontFix},
	} {
		rant := Rant{ID: "RANT-1", Seq: 7, Reviewed: len(tc.reviews) > 0, Reviews: tc.reviews, Tags: []string{"x"}, Refs: []RantRef{{Kind: RantRefTicket, ID: "AIRA-1"}}, Actor: "a", Severity: RantSeverityBlocker, ReceivedAt: "now", Redacted: true, OriginProjectID: "other"}
		summary := rant.Summary()
		if summary.Outcome != tc.want {
			t.Fatalf("%s: outcome=%q, want %q", name, summary.Outcome, tc.want)
		}
		if summary.ID != rant.ID || summary.Seq != rant.Seq || summary.Reviewed != rant.Reviewed || summary.Actor != "a" || summary.Severity != RantSeverityBlocker || summary.ReceivedAt != "now" || summary.OriginProjectID != "other" || !summary.Redacted || len(summary.Tags) != 1 || len(summary.Refs) != 1 {
			t.Fatalf("%s: metadata not carried: %#v", name, summary)
		}
	}
}

func TestRantListOptionsValidateNamesTheFieldAndValue(t *testing.T) {
	for options, want := range map[RantListOptions]string{
		{Status: "open"}:                          `--status "open"`,
		{Severity: "P1"}:                          `--severity "P1"`,
		{Limit: MaxRantListLimit + 1}:             "--limit 201",
		{Limit: -1}:                               "--limit -1",
		{Since: -2}:                               "--since -2",
		{Status: RantStatusAll}:                   "",
		{Severity: RantSeverityUnset}:             "",
		{Limit: MaxRantListLimit}:                 "",
		{Status: RantStatusReviewed}:              "",
		{Severity: string(RantSeverityAnnoyance)}: "",
	} {
		err := options.Validate()
		if want == "" {
			if err != nil {
				t.Fatalf("%+v: %v, want accepted", options, err)
			}
			continue
		}
		if err == nil || !strings.HasPrefix(err.Error(), CodeRantInvalid+":") || !strings.Contains(err.Error(), want) {
			t.Fatalf("%+v: %v, want %s naming %s", options, err, CodeRantInvalid, want)
		}
	}
	// Capture refuses the listing-only "unset" severity, naming it.
	if _, err := (RantInput{Body: "x", Severity: RantSeverityUnset}).Normalised(); err == nil || !strings.Contains(err.Error(), `"unset"`) {
		t.Fatalf("capture severity unset = %v, want a refusal naming it", err)
	}
}
