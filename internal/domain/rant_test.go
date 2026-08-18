package domain

import (
	"strings"
	"testing"
)

func TestRantInputPreservesBodyAndNormalisesTags(t *testing.T) {
	body := "  line one\nline two  \n"
	input := RantInput{Body: body, Tags: []string{"Slow_Tests", "slow tests", "INFRA"}, Severity: RantSeverityAnnoyance, IdempotencyKey: " retry-1 "}
	normalised, err := input.Normalised()
	if err != nil {
		t.Fatal(err)
	}
	if normalised.Body != body {
		t.Fatalf("body changed: %q", normalised.Body)
	}
	if got := strings.Join(normalised.Tags, ","); got != "infra,slow-tests" {
		t.Fatalf("tags = %q", got)
	}
	if normalised.IdempotencyKey != "retry-1" {
		t.Fatalf("idempotency key = %q", normalised.IdempotencyKey)
	}
}

func TestRantBodyBoundIsBytesAndRejectsNULAndEmpty(t *testing.T) {
	for name, test := range map[string]struct{ body, code string }{
		"bytes": {strings.Repeat("é", MaxRantBodyBytes/2+1), CodeRantTooLarge},
		"nul":   {"bad\x00body", CodeRantInvalid},
		"empty": {" \n\t ", CodeRantInvalid},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := (RantInput{Body: test.body}).Normalised()
			if err == nil || !strings.HasPrefix(err.Error(), test.code+":") {
				t.Fatalf("error = %v, want %s", err, test.code)
			}
		})
	}
}

func TestRantTagAndReferenceBoundsAreEnforced(t *testing.T) {
	if _, err := (RantInput{Body: "body", Tags: []string{strings.Repeat("x", MaxRantTagBytes+1)}}).Normalised(); err == nil {
		t.Fatal("overlong tag was accepted")
	}
	refs := make([]RantRef, MaxRantRefs+1)
	for i := range refs {
		refs[i] = RantRef{Kind: RantRefRun, ID: "RUN-1"}
	}
	if _, err := (RantInput{Body: "body", Refs: refs}).Normalised(); err == nil {
		t.Fatal("too many references were accepted")
	}
}

func TestRantReviewOutcomeIsClosed(t *testing.T) {
	if err := (RantReviewInput{Outcome: RantOutcomePlanned}).Validate(); err != nil {
		t.Fatal(err)
	}
	if err := (RantReviewInput{Outcome: RantOutcome("fixed")}).Validate(); err == nil {
		t.Fatal("invalid final-sounding outcome was accepted")
	}
}
