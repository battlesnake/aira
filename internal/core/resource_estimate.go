package core

import (
	"context"
	"errors"

	"aira/internal/runner"
)

const maxEstimateReserve int64 = runner.MaxMemoryEstimateReserve
const minSamples = 3 // retained for the real-history fixture; estimator lives in runner

func resourceSignature(commandPrefix, reqPrefix, argv []string) (string, error) {
	return runner.ResourceSignature(commandPrefix, reqPrefix, argv)
}

func estimateReserve(stats runner.PeakRSSStats, headroom int64) (reserve int64, override bool, basis string) {
	return runner.EstimateMemoryReserve(stats, headroom)
}

func prepareMemoryEstimate(ctx context.Context, execution Runner, headroom int64, commandPrefix []string, request *runner.Request) {
	if request == nil {
		return
	}
	signature, err := resourceSignature(commandPrefix, request.Prefix, request.Argv)
	if err != nil {
		return
	}
	request.ResourceSignature = signature
	historian, ok := execution.(runner.PeakRSSHistorian)
	if !ok {
		request.MemoryReserveBasis = "fallback:read-error"
		return
	}
	stats, _, readErr := historian.PeakRSSHistory(ctx, signature)
	if readErr != nil {
		request.MemoryReserveBasis = "fallback:read-error"
		if errors.Is(readErr, context.DeadlineExceeded) {
			request.MemoryReserveBasis = "fallback:read-timeout"
		}
		return
	}
	reserve, override, basis := estimateReserve(stats, headroom)
	request.MemoryReserveBasis = basis
	if override {
		request.MemoryReserveOverride = &reserve
	}
}
