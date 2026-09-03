package runner

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"time"
)

const DefaultConfineReserveMaxWait = 300 * time.Second

// ConfineReserveRequest describes one daemon-only pinned lease against the
// shared confine admission ledger. It deliberately has no flock fallback.
type ConfineReserveRequest struct {
	Slice           string
	AdmitSocketPath string
	Bytes           int64
	Pinned          bool
	Signature       string
	MaxWait         time.Duration
}

type ConfineReservation struct {
	State       string
	Reserve     int64
	Basis       string
	WaitedMS    int64
	ClampedFrom int64

	mu      sync.Mutex
	release io.Closer
}

func (reservation *ConfineReservation) Close() error {
	if reservation == nil {
		return nil
	}
	reservation.mu.Lock()
	release := reservation.release
	reservation.release = nil
	reservation.mu.Unlock()
	if release == nil {
		return nil
	}
	return release.Close()
}

func validateConfineReserveRequest(request ConfineReserveRequest) error {
	if request.Bytes <= 0 {
		return errors.New("E_CONFINE_ARGUMENT_INVALID: reserve bytes must be positive")
	}
	if !request.Pinned {
		return errors.New("E_CONFINE_ARGUMENT_INVALID: v1 confine reservations must be pinned")
	}
	if strings.TrimSpace(request.Signature) == "" {
		return errors.New("E_CONFINE_ARGUMENT_INVALID: reserve signature must be non-empty")
	}
	if request.MaxWait < 0 {
		return errors.New("E_CONFINE_ARGUMENT_INVALID: reserve max wait must be non-negative")
	}
	// AIRA-58: confine-reserve reaches admitThroughDaemon directly, so it does not
	// inherit Runner.admit's ceiling check. Without this, a programmatic
	// over-ceiling MaxWait is refused by the daemon and then wrapped as
	// E_CONFINE_UNAVAILABLE, which the pytest governor treats as fail-open — the
	// test would run unreserved. Refuse at this boundary too, against the same
	// shared constant every other caller uses.
	if request.MaxWait > AdmitWaitCeiling {
		return fmt.Errorf("E_CONFINE_ARGUMENT_INVALID: reserve max wait %s exceeds the ceiling of %s", request.MaxWait, AdmitWaitCeiling)
	}
	return nil
}

// ConfineReserve obtains a daemon-backed lease and never enters the legacy
// machine-wide flock fallback. Closing the result releases the lease.
func ConfineReserve(ctx context.Context, request ConfineReserveRequest) (*ConfineReservation, error) {
	return confineReserve(ctx, request)
}
