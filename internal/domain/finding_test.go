package domain

import (
	"strings"
	"testing"
)

func validReviewInput() ReviewFindingInput {
	return ReviewFindingInput{
		TicketID: "AIRA-7", Category: "flaky-test", Severity: SeverityP1,
		Verdict: VerdictConfirmed, Source: "codex", Message: "retry is masking a race",
		File: "internal/worker/../worker.go", Line: 42, RequirementID: "REQ-9",
	}
}

func TestReviewFindingConstructorRejectsIllegalStates(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*ReviewFindingInput)
	}{
		{"empty ticket", func(in *ReviewFindingInput) { in.TicketID = "" }},
		{"bad category", func(in *ReviewFindingInput) { in.Category = "Flaky Test" }},
		{"bad source", func(in *ReviewFindingInput) { in.Source = "codex/ci" }},
		{"bad severity", func(in *ReviewFindingInput) { in.Severity = Severity("P3") }},
		{"bad verdict", func(in *ReviewFindingInput) { in.Verdict = Verdict("unknown") }},
		{"empty message", func(in *ReviewFindingInput) { in.Message = "  " }},
		{"line without file", func(in *ReviewFindingInput) { in.File, in.Line = "", 2 }},
		{"non-positive line", func(in *ReviewFindingInput) { in.File, in.Line = "x.go", 0 }},
		{"waived without reason", func(in *ReviewFindingInput) { in.Disposition = DispositionWaived }},
		{"waiver fields on open", func(in *ReviewFindingInput) { in.WaiverReason, in.WaiverActor = "because", "human" }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			input := validReviewInput()
			tc.mutate(&input)
			if _, err := NewReviewFinding(input); err == nil {
				t.Fatal("constructor accepted illegal review finding")
			}
		})
	}
}

func TestFindingKeyCanonicalisesLocusAndExcludesMutableContent(t *testing.T) {
	first := validReviewInput()
	left, err := NewReviewFinding(first)
	if err != nil {
		t.Fatal(err)
	}
	second := first
	second.File = "internal/worker.go"
	second.Message = "different message"
	second.Severity = SeverityP0
	second.Verdict = VerdictPlausible
	right, err := NewReviewFinding(second)
	if err != nil {
		t.Fatal(err)
	}
	if left.Key != right.Key || left.File != right.File {
		t.Fatalf("canonical identity changed: left=%#v right=%#v", left, right)
	}
	if !strings.HasPrefix(left.Key, "f-AIRA-7-codex-flaky-test-") || len(left.Key) < len("f-AIRA-7-codex-flaky-test-")+16 {
		t.Fatalf("finding key is not readable and collision-resistant: %q", left.Key)
	}
	if left.Key != "f-AIRA-7-codex-flaky-test-753fa496258585c253ee0906a3dbd635e74dcc618325ced4c87ab3f2c657e9aa" {
		t.Fatalf("finding key derivation changed: %q", left.Key)
	}
	if left.Key == "" {
		t.Fatal("finding key is empty")
	}
}

func TestReconciliationFindingHasSeparateShape(t *testing.T) {
	finding, err := NewReconciliationFinding(ReconciliationFindingInput{
		Code: "E_GIT_SCAN", Subject: ".git", Details: "worktree is unavailable",
	})
	if err != nil {
		t.Fatal(err)
	}
	if finding.Subtype != FindingSubtypeReconciliation || finding.Code != "E_GIT_SCAN" || finding.TicketID != "" {
		t.Fatalf("reconciliation finding = %#v", finding)
	}
	if finding.Category != "" || finding.Source != "" || finding.Verdict != "" || finding.Severity != "" {
		t.Fatalf("reconciliation finding leaked review fields: %#v", finding)
	}
	if _, err := NewReconciliationFinding(ReconciliationFindingInput{Code: "E_GIT_SCAN", Details: "details"}); err == nil {
		t.Fatal("accepted reconciliation finding without subject")
	}
}
