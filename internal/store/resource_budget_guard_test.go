package store

import "testing"

// TestResourceBudgetRequestWithoutAFamilyIsUnevaluated: a pre-flight request
// whose basis names no cap:/reserve: family must fail honestly and by name, not
// silently exclude every row as a family mismatch and then blame the sample
// count. This is the false-pass direction of the family partition — the shape
// where the surface would look like "not enough evidence" while the real
// problem is that the caller's own quantity is uncomparable.
//
// verifies: AIRA-180
func TestResourceBudgetRequestWithoutAFamilyIsUnevaluated(t *testing.T) {
	rows := ResourceBudgetSubjectRows{Kind: ResourcePeakKindConfine, Signature: "s", Samples: []ResourceBudgetSample{
		capSample(100, 4096, false), capSample(100, 4096, false), capSample(100, 4096, false),
	}}
	verdict := ClassifyResourceBudget(rows, &ResourceBudgetRequest{Budget: 4096, Basis: "pinned:client"})
	if !verdict.Unevaluated {
		t.Fatalf("verdict=%+v", verdict)
	}
	if verdict.UnevaluatedReason == "fallback:insufficient-samples:n=0" {
		t.Fatal("an uncomparable basis must not be reported as a sample-count problem")
	}
	if verdict.Buckets["basis_family_mismatch"] != 3 {
		t.Fatalf("buckets=%v, want every row named as excluded", verdict.Buckets)
	}
	if verdict.RecommendedBudget != nil {
		t.Fatalf("recommended=%v", *verdict.RecommendedBudget)
	}
}

// TestResourceBudgetBucketsAccountForEveryRow: the bucket counts are the
// surface's evidence, and a reader must be able to sum them against
// TotalSamples. A row counted twice, or not at all, silently breaks that.
//
// verifies: AIRA-180
func TestResourceBudgetBucketsAccountForEveryRow(t *testing.T) {
	const g = int64(1) << 30
	verdict := classify(
		capSample(20*g, 40*g, false),                         // >=2.0
		capSample(20*g, 40*g, false),                         // >=2.0
		capSample(20*g, 40*g, false),                         // >=2.0
		capSample(g, g, true),                                // oom_below_current
		capSample(0, 40*g, false),                            // missing_peak
		budgetSample(100, 0, true, ""),                       // budget_unknown (an OOM, counted ONCE)
		budgetSample(100, g, false, "reserve:pinned:client"), // basis_family_mismatch
	)
	total := 0
	for _, count := range verdict.Buckets {
		total += count
	}
	if total != verdict.TotalSamples {
		t.Fatalf("buckets=%v sum=%d, want exactly TotalSamples=%d", verdict.Buckets, total, verdict.TotalSamples)
	}
}
