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
	slice, err := resolveConfineReserveSlice(request.Slice)
	if err != nil {
		return nil, err
	}
	request.Slice = slice
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

// resolveConfineReserveSlice decides which slice a reservation is CHARGED to.
//
// AIRA-115. This used to be `if request.Slice == "" { request.Slice =
// DefaultConfineSlice }` — the scope id was inherited from the parent confine
// job but the slice was defaulted independently, so a job confined to a
// non-default slice had its per-test sub-reservations booked against aira.slice.
// The reserving slice was then over-charged for memory it does not host (healthy
// jobs there wait behind a phantom reservation), while the hosting slice
// under-counted. The two halves of one fact must travel together.
//
// Precedence, and why:
//
//  1. An EXPLICIT --slice wins. AIRA-58 is about never silently SUBSTITUTING for
//     a caller's declared value; a caller who names a slice gets that slice.
//  2. Otherwise the parent job's resolved slice, if this process is running
//     inside one.
//  3. Otherwise DefaultConfineSlice — the unconfined caller, which is what the
//     default was always actually for.
//
// The refusal is the AIRA-58 rule applied to the one remaining gap: if a parent
// scope id IS present but its slice cannot be established, the environment is
// telling us this reservation belongs to a running job while withholding where
// that job lives. Defaulting there is precisely the silent mis-attribution this
// change removes, so it is refused instead. E_CONFINE_UNAVAILABLE, because
// callers treat that as fail-open: the test then runs UNRESERVED, which
// under-counts a slice rather than over-charging an unrelated one.
//
// What this deliberately does NOT do: consult ResolveConfineSlice, i.e. the
// operator's `$AIRA_CONFINE_SLICE`. An unconfined `aira confine-reserve` with
// that variable set still charges DefaultConfineSlice, exactly as before
// AIRA-115. Honouring it would be defensible — it is the operator saying where
// AIRA's jobs live — but it is a separate behaviour change to a separate input,
// outside this fix, and left out on purpose rather than by omission.
// InheritedConfineSlice reads the coordinate the PARENT JOB emitted; nothing
// here reads the operator's launch setting.
func resolveConfineReserveSlice(requested string) (string, error) {
	if slice := strings.TrimSpace(requested); slice != "" {
		return slice, nil
	}
	if inherited := InheritedConfineSlice(); inherited != "" {
		return inherited, nil
	}
	if parent := InheritedConfineScopeID(); parent != "" {
		return "", fmt.Errorf(
			"E_CONFINE_UNAVAILABLE: confine scope %s is inherited but its slice is absent or unusable; refusing to charge the default slice %s",
			parent, DefaultConfineSlice)
	}
	return DefaultConfineSlice, nil
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
