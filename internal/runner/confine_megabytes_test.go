package runner

import (
	"math"
	"testing"
)

// verifies: AIRA-193 -- one fixed unit, rounded to the nearest MiB (half up), never
// negative, never a fabricated non-zero for a non-positive input, and no overflow
// near MaxInt64. Moved from `aira top`, whose rule this is.
func TestFormatConfineMegabytesRoundsToTheNearestMiBInOneFixedUnit(t *testing.T) {
	const mib = int64(1) << 20
	for _, testCase := range []struct {
		name  string
		value int64
		want  string
	}{
		{"zero", 0, "0M"},
		{"negative", -1, "0M"},
		{"exact", 2 * mib, "2M"},
		{"rounds down", 2*mib + mib/2 - 1, "2M"},
		{"rounds up exactly at half a MiB", 2*mib + mib/2, "3M"},
		{"large, not a round G/T value", 47*mib*1024 + 12345, "48128M"},
		// The first values the old (value+mib/2)/mib form overflowed on: it printed a
		// NEGATIVE figure for a positive input.
		{"MaxInt64", math.MaxInt64, "8796093022208M"},
		{"MaxInt64 - 2^19 + 1, the first value the sum form overflows", math.MaxInt64 - mib/2 + 1, "8796093022208M"},
		{"just under the overflow edge", math.MaxInt64 - mib/2, "8796093022207M"},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			if got := FormatConfineMegabytes(testCase.value); got != testCase.want {
				t.Fatalf("FormatConfineMegabytes(%d)=%q, want %q", testCase.value, got, testCase.want)
			}
		})
	}
}
