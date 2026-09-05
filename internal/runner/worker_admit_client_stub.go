//go:build !linux

package runner

import (
	"context"
	"time"
)

type WorkerAdmitLease struct {
	WorkerID   string
	ScopePath  string
	MemoryMax  int64
	MemoryHigh int64
	CPUSlots   string
}

func (l *WorkerAdmitLease) Close() error { return nil }

type WorkerAdmitClientRequest struct {
	SocketPath     string
	JobID          string
	OuterScope     string
	Signature      string
	EstimatedBytes int64
	MaxWait        time.Duration
}

// RequestWorkerAdmit reports the same classified outcome shape as the Linux
// implementation. cgroup admission does not exist off Linux, so the honest
// classification is "daemon-backed admission is not usable here" — a
// structural, permanent fact, not an unclassified error.
func RequestWorkerAdmit(ctx context.Context, req WorkerAdmitClientRequest) WorkerAdmitOutcome {
	return WorkerAdmitOutcome{
		State: WorkerAdmitStateUnavailable, Class: WorkerAdmitClassAdmissionUnusable,
		Reason: WorkerAdmitReasonDialFailed, Detail: "aitest worker-admit: unsupported on this platform",
	}
}
