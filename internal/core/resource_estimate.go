package core

import (
	"fmt"
	"strings"

	"aira/internal/runner"
)

const maxEstimateReserve int64 = 1 << 50 // mirrors daemon.admitMaxReserve

const (
	minSamples = 3
	safetyPct  = int64(15)
)

func nulJoin(argv []string) string {
	return strings.Join(argv, "\x00")
}

func resourceSignature(commandPrefix, reqPrefix, argv []string) (string, error) {
	selected, err := runner.EffectivePrefix(commandPrefix, reqPrefix)
	if err != nil {
		return "", err
	}
	effective, err := runner.EffectiveArgv(selected, argv)
	if err != nil {
		return "", err
	}
	return nulJoin(effective), nil
}

func estimateReserve(stats runner.PeakRSSStats, headroom int64) (reserve int64, override bool, basis string) {
	if stats.SampleCount < minSamples {
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
	var estimate int64
	if peak > maxEstimateReserve {
		estimate = maxEstimateReserve
		capped = true
	} else {
		estimate = peak + peak*safetyPct/100
		if estimate > maxEstimateReserve {
			estimate = maxEstimateReserve
			capped = true
		}
	}

	reserve = estimate
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
	// The reserve is bounded to (0, cap] by construction (peak>0, cap clamp, and
	// config rejecting headroom>cap). Degrade honestly rather than crashing an
	// advisory estimate if that invariant is ever violated: clamp an over-cap
	// value, and fall back on a non-positive one instead of emitting a reserve
	// the runner would (correctly) refuse to enforce.
	if reserve > maxEstimateReserve {
		reserve = maxEstimateReserve
	}
	if reserve <= 0 {
		return 0, false, "fallback:malformed"
	}
	return reserve, true, basis
}
