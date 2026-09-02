//go:build !linux

package runner

import (
	"context"
	"errors"
	"time"
)

type WorkerAdmitLease struct {
	WorkerID   string
	ScopePath  string
	MemoryMax  int64
	MemoryHigh int64
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

func RequestWorkerAdmit(ctx context.Context, req WorkerAdmitClientRequest) (*WorkerAdmitLease, error) {
	return nil, errors.New("aitest worker-admit: unsupported on this platform")
}
