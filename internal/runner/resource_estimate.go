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
