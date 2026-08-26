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
