package runner

import (
	"fmt"
	"strings"
)

const MaxMemoryEstimateReserve int64 = 1 << 50

const (
	memoryEstimateMinSamples = 3
	memoryEstimateSafetyPct  = int64(15)
)

// SliceFittedReserve is the largest reserve a slice whose admission ceiling is
// `ceiling` can actually GRANT one job, and it is the ONE quantity AIRA-153 uses
// wherever the daemon sizes a value for itself against that ceiling.
//
// The quantity is the INVERSE of the estimator's own growth margin: the largest
// figure that, grown by memoryEstimateSafetyPct the way every history-derived
// estimate here is, still fits inside `ceiling`. So no new constant enters the
// codebase, and the number means something already meant elsewhere.
//
// It must be strictly BELOW the ceiling and it must never degenerate to zero,
// for one reason each:
//   - the ceiling is the largest ADMISSIBLE reserve, not the largest GRANTABLE
//     one. A reserve equal to it is granted only while the slice's own charge
//     sits inside a narrow residual band (AIRA-150), which on a slice the
//     request entered empty is byte-exact zero. Sizing TO the ceiling — which
//     is what the OOM clamp used to do — is the wedge, not the fix.
//   - a margin expressed in the configurable headroom terms is zero whenever
//     those are configured to zero, which is a margin only by luck.
//
// A proportional margin has neither failure: it is ~13.04% of the ceiling on a
// 1 GiB fixture slice and on a 64 GiB production slice alike.
//
// Returns 0 — "this slice cannot grant a viable reserve at all" — when:
//   - `ceiling <= 0`, which is what subtractFloor yields for a slice smaller
//     than its own headroom;
//   - the fitted value would be below MinPinnedScopeCap. That is the DEGENERATE
//     case, and the honest answer there is the existing terminal
//     E_ADMIT_TOO_LARGE naming `required` and `cap_minus_headroom`, not a
//     sub-megabyte memory.max that would instant-OOM the job on placement —
//     the exact hazard workerAdmitEstimatedBytesMin was minted for.
//
// Callers apply their own trigger: a PRIOR is fitted only when it is at or over
// the ceiling; the OOM clamp fires only where AIRA-151 already said it should.
//
// covers: AIRA-153 §3.2
func SliceFittedReserve(ceiling int64) int64 {
	if ceiling <= 0 {
		return 0
	}
	// Overflow-free exact floor of ceiling*100/(100+pct): writing it as one
	// multiply would overflow int64 above ~92 PiB of ceiling, and a wrapped
	// negative here would silently size every value to nonsense. The identity
	// c = 115q + r  =>  floor(100c/115) = 100q + floor(100r/115) is exact.
	const scale = 100 + memoryEstimateSafetyPct
	fitted := ceiling/scale*100 + ceiling%scale*100/scale
	if fitted < MinPinnedScopeCap {
		return 0
	}
	return fitted
}

// ResourceSignature is the exact effective launch argv joined without lossy
// shell rendering. It is kept beside the estimator so launch faces share one
// signature implementation.
func ResourceSignature(commandPrefix, requestPrefix, argv []string) (string, error) {
	selected, err := EffectivePrefix(commandPrefix, requestPrefix)
	if err != nil {
		return "", err
	}
	effective, err := EffectiveArgv(selected, argv)
	if err != nil {
		return "", err
	}
	return strings.Join(effective, "\x00"), nil
}

// EstimateMemoryReserve is the #50 conservative peak-RSS estimator. override
// is false when callers must retain their fixed fallback headroom.
func EstimateMemoryReserve(stats PeakRSSStats, headroom int64) (reserve int64, override bool, basis string) {
	if stats.SampleCount < memoryEstimateMinSamples {
		switch {
		case stats.TotalCount == 0:
			return 0, false, "fallback:no-history"
		case stats.SampleCount == 0:
			return 0, false, "fallback:capture-unavailable"
		default:
			return 0, false, fmt.Sprintf("fallback:insufficient-samples:n=%d", stats.SampleCount)
		}
	}
	peak := stats.PeakMax
	if peak <= 0 {
		return 0, false, "fallback:malformed"
	}
	capped := false
	if peak > MaxMemoryEstimateReserve {
		reserve, capped = MaxMemoryEstimateReserve, true
	} else {
		reserve = peak + peak*memoryEstimateSafetyPct/100
		if reserve > MaxMemoryEstimateReserve {
			reserve, capped = MaxMemoryEstimateReserve, true
		}
	}
	if stats.OOMCount > 0 {
		if headroom > reserve {
			reserve = headroom
		}
		basis = fmt.Sprintf("estimate:oom:max=%d,n=%d,oom=%d,f=115", peak, stats.SampleCount, stats.OOMCount)
	} else if capped {
		basis = "estimate:capped"
	} else {
		basis = fmt.Sprintf("estimate:max=%d,n=%d,f=115", peak, stats.SampleCount)
	}
	if reserve > MaxMemoryEstimateReserve {
		reserve = MaxMemoryEstimateReserve
	}
	if reserve <= 0 {
		return 0, false, "fallback:malformed"
	}
	return reserve, true, basis
}
