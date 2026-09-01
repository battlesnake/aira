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
	if deadline, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(deadline)
	}
	frame := runnerAdmitRequestFrame{Proto: runnerDaemonProtocolVersion, Scope: map[string]any{}}
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
