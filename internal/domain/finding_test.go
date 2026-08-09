package domain

import (
	"bytes"
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
	if left.Key != "f-AIRA-7-codex-flaky-test-65bf04c3febc28bac3bb33be3023da9fa5c2a27de8311ed85fc86f7636e4344d" {
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

func TestParseFindingRejectsTrailingFrontmatterContent(t *testing.T) {
	finding, err := NewReviewFinding(validReviewInput())
	if err != nil {
		t.Fatal(err)
	}
	data, err := RenderFinding(finding)
	if err != nil {
		t.Fatal(err)
	}
	marker := []byte("\n---\n")
	for _, trailing := range [][]byte{[]byte("junk"), []byte(`{"schema":1}`)} {
		bad := bytes.Replace(data, marker, append(append([]byte{}, trailing...), marker...), 1)
		if _, err := ParseFinding(bad); err == nil {
			t.Fatalf("ParseFinding accepted trailing frontmatter content %q", trailing)
		}
	}
}

func TestFindingKeysUseUnambiguousComponentEncoding(t *testing.T) {
	left := ReviewFindingKey(ReviewFindingInput{TicketID: "AIRA-1", Source: "s\x00c", Category: "d"})
	right := ReviewFindingKey(ReviewFindingInput{TicketID: "AIRA-1", Source: "s", Category: "c\x00d"})
	leftHash := left[strings.LastIndexByte(left, '-')+1:]
	rightHash := right[strings.LastIndexByte(right, '-')+1:]
	if leftHash == rightHash {
		t.Fatalf("NUL-separated review identity hashes collided: %q", leftHash)
	}
}

func TestReconciliationKeysUseUnambiguousComponentEncoding(t *testing.T) {
	left := reconciliationFindingKey("E_X\x00Y", "E_Z", "")
	right := reconciliationFindingKey("E_X", "Y\x00E_Z", "")
	if left == right {
		t.Fatalf("NUL-separated reconciliation identities collided: %q", left)
	}
}

func TestReconciliationFindingRejectsControlCode(t *testing.T) {
	left, leftErr := NewReconciliationFinding(ReconciliationFindingInput{Code: "E_X\x00Y", Subject: "E_Z", Details: "details"})
	right, rightErr := NewReconciliationFinding(ReconciliationFindingInput{Code: "E_X", Subject: "Y\x00E_Z", Details: "details"})
	if leftErr == nil && rightErr == nil && left.Key == right.Key {
		t.Fatalf("NUL-separated reconciliation identities collided: %q", left.Key)
	}
	if leftErr == nil {
		t.Fatalf("accepted reconciliation code with control character: %q", "E_X\x00Y")
	}
}
