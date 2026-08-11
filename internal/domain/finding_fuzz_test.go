package domain

import "testing"

func FuzzParseFinding(f *testing.F) {
	seeds := []ReviewFindingInput{
		{TicketID: "AIRA-7", Category: "flaky-test", Severity: SeverityP1, Verdict: VerdictConfirmed, Source: "codex", Message: "retry is masking a race", File: "internal/worker.go", Line: 42},
		{TicketID: "AIRA-42", Category: "correctness", Severity: SeverityP0, Verdict: VerdictPlausible, Source: "reviewer", Message: "the state transition is incomplete", RequirementID: "REQ-9", Disposition: DispositionWaived, WaiverReason: "tracked separately", WaiverActor: "human"},
	}
	for _, input := range seeds {
		finding, err := NewReviewFinding(input)
		if err != nil {
			f.Fatal(err)
		}
		data, err := RenderFinding(finding)
		if err != nil {
			f.Fatal(err)
		}
		f.Add(string(data))
	}
	f.Add("garbage")
	f.Add("---\n{}\n---\n")

	f.Fuzz(func(t *testing.T, data string) {
		finding, err := ParseFinding([]byte(data))
		if err != nil {
			return
		}
		canonical, err := RenderFinding(finding)
		if err != nil {
			t.Fatalf("render successful parse: %v", err)
		}
		reparsed, err := ParseFinding(canonical)
		if err != nil {
			t.Fatalf("parse rendered finding: %v", err)
		}
		recanonical, err := RenderFinding(reparsed)
		if err != nil {
			t.Fatalf("re-render parsed finding: %v", err)
		}
		if string(canonical) != string(recanonical) {
			t.Fatal("finding did not round-trip through its canonical renderer")
		}
	})
}
