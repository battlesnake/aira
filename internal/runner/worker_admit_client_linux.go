//go:build linux

package runner

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"time"
)

// WorkerAdmitLease is a granted worker-admit connection, held open as the
// daemon-side lease until Close releases it (the daemon frees the ledger
// entry when it detects the peer disconnect — see the worker-admit design).
type WorkerAdmitLease struct {
	WorkerID   string
	ScopePath  string
	MemoryMax  int64
	MemoryHigh int64
	conn       net.Conn
}

func (l *WorkerAdmitLease) Close() error {
	if l == nil || l.conn == nil {
		return nil
	}
	return l.conn.Close()
}

type WorkerAdmitClientRequest struct {
	SocketPath     string
	JobID          string
	OuterScope     string
	Signature      string
	EstimatedBytes int64
	MaxWait        time.Duration
}

type workerAdmitGrant struct {
	State      string `json:"state"`
	Reason     string `json:"reason,omitempty"`
	WorkerID   string `json:"worker_id,omitempty"`
	ScopePath  string `json:"scope_path,omitempty"`
	MemoryMax  int64  `json:"memory_max,omitempty"`
	MemoryHigh int64  `json:"memory_high,omitempty"`
}

// RequestWorkerAdmit dials the daemon and sends one worker-admit request,
// reusing admitThroughDaemon's proven local wire types/framing (this package
// may not import internal/daemon — see admission_linux.go). On "granted" the
// returned lease holds the connection open; Close releases the daemon-side
// ledger entry.
func RequestWorkerAdmit(ctx context.Context, req WorkerAdmitClientRequest) (*WorkerAdmitLease, error) {
	var dialer net.Dialer
	conn, err := dialer.DialContext(ctx, "unix", req.SocketPath)
	if err != nil {
		return nil, fmt.Errorf("E_CONFINE_UNAVAILABLE: dial daemon: %w", err)
	}
	// The daemon's own poll loop is bounded by max_wait_ms and degrades to a
	// clean "timeout" response within that budget -- but nothing protected
	// THIS side if the daemon hung before ever writing a response: the CLI's
	// actual ctx (runWorkerAdmitCommand's signalCtx, cmd/aira/main.go) is
	// built from context.Background() via signal.NotifyContext, which adds
	// cancellation on a signal but never a deadline, so `ctx.Deadline()`
	// below was never ok and no socket deadline was ever set (found by Sol
	// build-review). Mirror admitThroughDaemon's own transport-deadline
	// pattern (admission_linux.go, same package): grant the daemon its full
	// declared wait budget plus a fixed grace margin to answer even at the
	// very edge of that budget, then bound the read regardless of what ctx
	// itself carries -- still honoring an EARLIER caller deadline if one is
	// present.
	deadlineWait := req.MaxWait
	if deadlineWait > time.Duration(mathMaxInt64)-admitTransportGrace {
		deadlineWait = time.Duration(mathMaxInt64) - admitTransportGrace
	}
	transportDeadline := time.Now().Add(deadlineWait + admitTransportGrace)
	if ctxDeadline, ok := ctx.Deadline(); ok && ctxDeadline.Before(transportDeadline) {
		transportDeadline = ctxDeadline
	}
	_ = conn.SetDeadline(transportDeadline)
	frame := runnerAdmitRequestFrame{Proto: DaemonProtocolVersion, Scope: map[string]any{}}
	frame.Request.Verb = "worker-admit"
	frame.Request.Args = map[string]any{
		"job_id": req.JobID, "outer_scope": req.OuterScope, "signature": req.Signature,
		"estimated_bytes": req.EstimatedBytes, "max_wait_ms": req.MaxWait.Milliseconds(),
	}
	if err := writeRunnerAdmitFrame(conn, frame); err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("E_CONFINE_UNAVAILABLE: send worker-admit request: %w", err)
	}
	var response runnerAdmitResponseFrame
	if err := readRunnerAdmitFrame(conn, &response); err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("E_CONFINE_UNAVAILABLE: read worker-admit response: %w", err)
	}
	if response.Code != "OK" {
		_ = conn.Close()
		reason := response.Error
		if reason == "" {
			reason = response.Code
		}
		return nil, fmt.Errorf("E_CONFINE_UNAVAILABLE: worker-admit request rejected: %s", reason)
	}
	var grant workerAdmitGrant
	if err := json.Unmarshal(response.Data, &grant); err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("E_CONFINE_UNAVAILABLE: malformed worker-admit response: %w", err)
	}
	if grant.State != "granted" {
		_ = conn.Close()
		reason := grant.Reason
		if reason == "" {
			reason = response.Error
		}
		return nil, fmt.Errorf("E_CONFINE_UNAVAILABLE: worker-admit %s: %s", grant.State, reason)
	}
	return &WorkerAdmitLease{WorkerID: grant.WorkerID, ScopePath: grant.ScopePath, MemoryMax: grant.MemoryMax, MemoryHigh: grant.MemoryHigh, conn: conn}, nil
}
