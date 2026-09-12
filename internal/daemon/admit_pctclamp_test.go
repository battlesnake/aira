package daemon

import (
	"math"
	"testing"
)

// TestPctClampNeverOverflows pins pctClamp, a shared helper KEPT after the
// AIRA-29 dynamic charge was retired: the oomsteer fullness band
// (oomsteer.go) depends on its overflow bound. Relocated here from the deleted
// dynamic-charge test file so the coverage survives that deletion.
func TestPctClampNeverOverflows(t *testing.T) {
	if got := pctClamp(1000, 12); got != 120 {
		t.Fatalf("pctClamp(1000, 12) = %d, want 120", got)
	}
	// The overflow branch must still be ARITHMETIC, not merely non-negative. An
	// earlier version returned MaxInt64/100*pct here -- non-negative, and about
	// 12x LARGER than the true answer. "got >= 0" passed against it.
	huge := pctClamp(math.MaxInt64, 12)
	if huge <= 0 {
		t.Fatalf("pctClamp(MaxInt64, 12) = %d, want a positive result", huge)
	}
	if huge > math.MaxInt64/100*12+100 {
		t.Fatalf("pctClamp(MaxInt64, 12) = %d, far above 12%% of the input; the overflow branch is not computing a percentage", huge)
	}
	if huge > math.MaxInt64/8 {
		t.Fatalf("pctClamp(MaxInt64, 12) = %d, above an eighth of the input; 12%% cannot exceed that", huge)
	}
	// A percentage of at most 100 can never exceed the value it is taken from,
	// on either branch.
	for _, value := range []int64{1000, 1 << 40, math.MaxInt64 / 3, math.MaxInt64} {
		for _, pct := range []int64{1, 12, 100, 250} {
			if got := pctClamp(value, pct); got > value {
				t.Fatalf("pctClamp(%d, %d) = %d, above the value it is a percentage of", value, pct, got)
			}
		}
	}
	// The overflow branch must agree with the exact answer to within its stated
	// error, a remainder smaller than pct. Computed here in big-integer-free form
	// by splitting the multiply, so the assertion does not simply restate the
	// implementation. Broad bounds alone leave a materially wrong overflow branch
	// undetected -- which is how a 12x-too-large version survived a first pass.
	for _, pct := range []int64{1, 12, 100} {
		for _, value := range []int64{math.MaxInt64, math.MaxInt64 - 1, math.MaxInt64/pct + 1} {
			if value <= 0 || value <= math.MaxInt64/pct {
				continue // not the overflow branch
			}
			exact := value/100*pct + (value%100)*pct/100
			got := pctClamp(value, pct)
			if diff := exact - got; diff < 0 || diff >= pct {
				t.Fatalf("pctClamp(%d, %d) = %d, exact %d, off by %d -- the stated error bound is a remainder below pct",
					value, pct, got, exact, diff)
			}
		}
	}
	if got := pctClamp(-5, 12); got != 0 {
		t.Fatalf("pctClamp(-5, 12) = %d, want 0", got)
	}
	if got := pctClamp(1000, 0); got != 0 {
		t.Fatalf("pctClamp(1000, 0) = %d, want 0", got)
	}
}
