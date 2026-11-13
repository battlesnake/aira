package domain

import (
	"strings"
	"testing"
	"unicode/utf8"
)

// verifies: AIRA-327 — the compact ls row previews the body on a rune boundary.

func TestRantSummaryPreviewCutsOnARuneBoundary(t *testing.T) {
	// Two-byte runes: a byte-count cut at 240 would split nothing but yield
	// only 120 runes, and any odd byte cut would split a rune.
	body := strings.Repeat("é", RantPreviewRunes+60)
	summary := Rant{Body: body}.Summary()
	if !summary.BodyTruncated || summary.BodyBytes != len(body) {
		t.Fatalf("truncated=%v bytes=%d, want true/%d", summary.BodyTruncated, summary.BodyBytes, len(body))
	}
	if !utf8.ValidString(summary.Body) || utf8.RuneCountInString(summary.Body) != RantPreviewRunes || summary.Body != strings.Repeat("é", RantPreviewRunes) {
		t.Fatalf("preview has %d runes (valid=%v), want exactly %d", utf8.RuneCountInString(summary.Body), utf8.ValidString(summary.Body), RantPreviewRunes)
	}
	// A mixed body whose 240th rune is multi-byte.
	mixed := strings.Repeat("a", RantPreviewRunes-1) + "€€tail"
	if got := (Rant{Body: mixed}).Summary(); got.Body != strings.Repeat("a", RantPreviewRunes-1)+"€" || !got.BodyTruncated {
		t.Fatalf("mixed preview = %q truncated=%v", got.Body[len(got.Body)-8:], got.BodyTruncated)
	}
}

func TestRantSummaryKeepsABodyAtTheLimitWhole(t *testing.T) {
	for _, body := range []string{"short", strings.Repeat("ü", RantPreviewRunes)} {
		summary := Rant{Body: body}.Summary()
		if summary.Body != body || summary.BodyTruncated || summary.BodyBytes != 0 {
			t.Fatalf("body of %d runes: preview changed (truncated=%v bytes=%d)", utf8.RuneCountInString(body), summary.BodyTruncated, summary.BodyBytes)
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
