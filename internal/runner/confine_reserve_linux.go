//go:build linux

package runner

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
)

func confineReserve(ctx context.Context, request ConfineReserveRequest) (*ConfineReservation, error) {
	if err := validateConfineReserveRequest(request); err != nil {
		return nil, err
	}
	if strings.TrimSpace(request.Slice) == "" {
		request.Slice = DefaultConfineSlice
	}
	if request.MaxWait == 0 {
		request.MaxWait = DefaultConfineReserveMaxWait
	}
	r := &Runner{
		memorySlice: request.Slice, memoryReserve: request.Bytes,
		admissionMaxWait: request.MaxWait, pollInterval: 2 * time.Second,
		clock: systemClock{}, admitSocketPath: request.AdmitSocketPath,
	}
	return confineReserveWithRunner(ctx, request, r)
}

func confineReserveWithRunner(ctx context.Context, request ConfineReserveRequest, r *Runner) (*ConfineReservation, error) {
	if err := validateConfineReserveRequest(request); err != nil {
		return nil, err
	}
	reserve := request.Bytes
	clampedFrom := int64(0)
	for attempt := 0; attempt < 2; attempt++ {
		result, answered, err := r.admitThroughDaemon(ctx, Request{
			ResourceSignature:   request.Signature,
			MemoryReservePinned: true,
			// AIRA-101. Identify this as a SUB-RESERVATION of an already-running
			// job, read from the scope id the job exported into its own environment.
			//
			// It is what keeps an exclusive drain converging. A per-test reservation
			// is the running job's internal progress, not new work entering the
			// slice; blocking these would stall every test of every running
			// --delegate-ram suite for its full wait and then let it run UNCHARGED,
			// so those suites could never finish and the drain could never complete.
			//
			// A scope-less admission alone is NOT a usable signal for that — `aira
			// run` is also scope-less and IS new job-level work a drain must block —
			// which is why this is an explicit marker rather than an inference.
			ParentScopeID: InheritedConfineScopeID(),
		}, reserve)
		if !answered {
			if err != nil {
				return nil, err
			}
			return nil, errors.New("E_CONFINE_UNAVAILABLE: daemon admission unavailable")
		}
		if err != nil {
			if result.state == "too_large" && result.ceiling > 0 && result.ceiling < reserve && attempt == 0 {
				clampedFrom = reserve
				reserve = result.ceiling
				continue
			}
			return nil, fmt.Errorf("E_CONFINE_UNAVAILABLE: %w", err)
		}
		if result.state != "immediate" && result.state != "waited" {
			result.releaseAdmission()
			return nil, fmt.Errorf("E_CONFINE_UNAVAILABLE: daemon admission %s", result.state)
		}
		if result.reserve <= 0 || result.basis != "pinned:client" || result.release == nil {
			result.releaseAdmission()
			return nil, errors.New("E_CONFINE_UNAVAILABLE: daemon returned an invalid pinned grant")
		}
		return &ConfineReservation{
			State: result.state, Reserve: result.reserve, Basis: result.basis,
			WaitedMS: result.waitedMS, ClampedFrom: clampedFrom, release: result.release,
		}, nil
	}
	return nil, errors.New("E_CONFINE_UNAVAILABLE: pinned reserve exceeds daemon ceiling")
}
