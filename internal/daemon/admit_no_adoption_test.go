package daemon

import (
	"math"
	"testing"
)

// TestAddClampSaturatesWithoutFabricatingHeadroom pins the saturating add the
// admission ledger uses throughout (queuedBytes and the grant-path sum).
// Relocated in S12 from the deleted admit_reconstruction_test.go; addClamp
// outlives the adoption sum that was one of its callers. (S12's scan-double-count
// guard TestAdmitDoesNotDoubleCountAScanVisibleGrantedLease was retired in S14
// with the periodic cgroup scan: with no scan there is no second accounting to
// double-count against, so a single-ledger fit is now structural.)
func TestAddClampSaturatesWithoutFabricatingHeadroom(t *testing.T) {
	for _, test := range []struct {
		a, b int64
		want int64
	}{
		{40, 2, 42},
		{math.MaxInt64 - 5, 5, math.MaxInt64},
		{math.MaxInt64 - 5, 6, math.MaxInt64},
		{math.MaxInt64, 1, math.MaxInt64},
		{-1, 1, math.MaxInt64},
		{1, -1, math.MaxInt64},
	} {
		if got := addClamp(test.a, test.b); got != test.want {
			t.Fatalf("addClamp(%d,%d)=%d, want %d", test.a, test.b, got, test.want)
		}
	}
}
