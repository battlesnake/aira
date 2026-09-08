package runner

import (
	"math"
	"testing"
)

// TestSliceFittedReserve is AIRA-153 T1: the pure quantity, as a table.
//
// SliceFittedReserve is the ONE number AIRA-153 uses wherever the daemon sizes a
// value for ITSELF against a slice admission ceiling — the client's unpinned
// prior, the machine-wide p90 prior, and AIRA-151's OOM-escalation clamp. Every
// property the three call sites rely on is asserted here rather than at those
// sites, because they are properties of the quantity and not of any one use:
//
//   - it is the INVERSE of the estimator's own 15% growth margin, so no new
//     constant enters the codebase;
//   - it is STRICTLY below the ceiling, which is what keeps an auto-sized value
//     off AIRA-150's ungrantable equality;
//   - it DEGENERATES TO ZERO below MinPinnedScopeCap rather than handing a job a
//     sub-megabyte memory.max that would instant-OOM it on placement — the exact
//     hazard workerAdmitEstimatedBytesMin was minted for;
//   - it does not OVERFLOW, which the naive ceiling*100/115 does above ~92 PiB.
//
// verifies: AIRA-153 §3.2, I5, I7
func TestSliceFittedReserve(t *testing.T) {
	for _, test := range []struct {
		name    string
		ceiling int64
		want    int64
	}{
		// The plan §0.1 table, every row a real ceiling somewhere in this
		// codebase or on this box.
		{"640 MiB fixture slice (T13)", 629145600, 547083130},
		{"1 GiB AIRA-139/149/151 fixture", 1031798784, 897216333},
		{"U2's tie-row ceiling", 2684354560, 2334221356},
		{"3 GiB fixture slice (T9)", 3179282432, 2764593419},
		{"a DEFAULT install on an 8 GiB box", 4227858432, 3676398636},
		{"AIRA-149 facet-2b 4 GiB slice", 4253024256, 3698281961},
		{"a ceiling exactly at the unpinned default", 4294967296, 3734754170},
		{"AIRA-128's 6 GiB fixture", 6400507904, 5565659046},
		{"scope-test row (a)", 8589934592, 7469508340},
		{"confine_admit_test's 10 GiB", 10737418240, 9336885426},
		{"scope-test row (b), 55 GiB", 59055800320, 51352869843},
		{"production aira.slice, j = 0", 66504884224, 57830334107},
		{"the static 64 GiB (headroom 0)", 68719476736, 59756066726},
		{"row (e-malformed)'s 100 GiB", 107374182400, 93368854260},

		// The degenerate floor, on the byte. Below MinPinnedScopeCap the honest
		// answer is 0 ("this slice cannot grant a viable reserve at all"), which
		// leaves the existing terminal E_ADMIT_TOO_LARGE to answer with `required`
		// and `cap_minus_headroom` populated.
		{"one byte under the floor", 1205862, 0},
		{"exactly the floor", 1205863, MinPinnedScopeCap},

		// A ceiling subtractFloor already floored at zero, and a negative one no
		// caller should produce but which must not yield nonsense if one does.
		{"zero ceiling", 0, 0},
		{"negative ceiling", -1, 0},

		// The overflow row. `ceiling*100` wraps negative here, and the naive form
		// returns 0 — which would silently size every value to nothing.
		{"math.MaxInt64", math.MaxInt64, 8020323510308500701},
	} {
		t.Run(test.name, func(t *testing.T) {
			got := SliceFittedReserve(test.ceiling)
			if got != test.want {
				t.Fatalf("SliceFittedReserve(%d)=%d, want %d", test.ceiling, got, test.want)
			}
			// I5, on every row that fires: an auto-sized value must never land ON
			// the ceiling, because a reserve equal to the entry ceiling is granted
			// only inside AIRA-150's residual band.
			if got > 0 && got >= test.ceiling {
				t.Fatalf("SliceFittedReserve(%d)=%d is not STRICTLY below the ceiling; a reserve equal to it reintroduces AIRA-150",
					test.ceiling, got)
			}
			// I7: whatever it returns is either zero or a reserve the runner
			// boundary would itself accept as a real cap.
			if got > 0 && got < MinPinnedScopeCap {
				t.Fatalf("SliceFittedReserve(%d)=%d is below MinPinnedScopeCap (%d); a sub-megabyte memory.max instant-OOMs a job on placement",
					test.ceiling, got, MinPinnedScopeCap)
			}
		})
	}
}

// TestSliceFittedReserveInvertsTheEstimatorsOwnMargin is the rationale, asserted
// rather than argued: the fitted value grown by the estimator's 15% margin --
// the SAME arithmetic EstimateMemoryReserve applies to every history-derived
// figure -- still fits inside the ceiling, and one byte more would not.
//
// This is what makes the quantity mean something already meant elsewhere in this
// codebase instead of being an arbitrary fraction.
//
// verifies: AIRA-153 §3.2
func TestSliceFittedReserveInvertsTheEstimatorsOwnMargin(t *testing.T) {
	for _, ceiling := range []int64{
		1205863, 629145600, 1031798784, 2684354560, 3179282432,
		4227858432, 4253024256, 4294967296, 6400507904, 59055800320, 66504884224,
	} {
		fit := SliceFittedReserve(ceiling)
		if fit <= 0 {
			t.Fatalf("SliceFittedReserve(%d)=0; every row here is above the degenerate floor", ceiling)
		}
		grown := fit + fit*memoryEstimateSafetyPct/100
		if grown > ceiling {
			t.Fatalf("ceiling=%d fit=%d grows to %d, which does NOT fit; the quantity is not the inverse of the estimator margin",
				ceiling, fit, grown)
		}
		// And it is MAXIMAL in the exact rational sense the quantity is defined
		// by: the largest x with 115x <= 100*ceiling. Stated that way rather than
		// as "the largest x whose grown value fits", which is NOT true — the
		// estimator floors `peak*15/100`, so fit+1 sometimes still fits by a byte
		// or two. Overclaiming that here would be a false pin.
		if got := fit + 1; (100+memoryEstimateSafetyPct)*got <= 100*ceiling {
			t.Fatalf("ceiling=%d fit=%d is not maximal: 115*%d <= 100*%d", ceiling, fit, got, ceiling)
		}
	}
}
