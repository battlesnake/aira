package domain

import "testing"

func TestTestReportInputValidationRequiresStrictShardAndOutcome(t *testing.T) {
	base := TestReportInput{Format: "go-json", Shard: "1/1", Results: []TestResult{{Name: "pkg/Test", Outcome: OutcomePass}}}
	if err := base.Validate(); err != nil {
		t.Fatalf("valid input: %v", err)
	}
	for _, shard := range []string{"", "0/1", "1/0", "one/1", "1/1/1"} {
		candidate := base
		candidate.Shard = shard
		if err := candidate.Validate(); err == nil {
			t.Fatalf("shard %q accepted", shard)
		}
	}
	candidate := base
	candidate.Results = []TestResult{{Name: "pkg/Test", Outcome: "bogus"}}
	if err := candidate.Validate(); err == nil {
		t.Fatal("unknown outcome accepted")
	}
}

func TestFlakyStatesAreClosed(t *testing.T) {
	for _, state := range []FlakyState{FlakyStateFlaky, FlakyStateClean, FlakyStateUnevaluated} {
		if err := (FlakyTest{Name: "pkg/Test", State: state}).Validate(); err != nil {
			t.Fatalf("state %q: %v", state, err)
		}
	}
	if err := (FlakyTest{Name: "pkg/Test", State: "not-a-state"}).Validate(); err == nil {
		t.Fatal("unknown state accepted")
	}
}
