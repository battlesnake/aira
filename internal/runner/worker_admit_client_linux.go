//go:build linux

package runner

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"aira/internal/redeclare"
)

// WorkerAdmitLease is a granted worker-admit connection, held open by a leaseKeeper
// until Close releases it. The daemon parks its side on the same connection and
// releases the ledger charge when it observes the peer disconnect (S15, the AIRA-41
// reversal: EOF releases the lease). One connection per worker lease.
//
// S15: for an ENFORCED grant the keeper RECONNECTS and re-declares the lease (its
// ARDR frame keyed on the scope DIRNAME) across a daemon restart, re-anchoring the worker's
// RAM and core in the fresh daemon's ledger — exactly like the confine lease keeper.
// A shim ADVISORY grant has no scope path and no ARDR key, so its keeper HOLDS only
// (no reconnect), the same rule the confine keeper applies to a scope-less lease.
//
// Closing the lease stops the keeper and closes the connection, which the daemon
// sees as the lease-releasing EOF.
type WorkerAdmitLease struct {
	WorkerID  string
	ScopePath string
	MemoryMax int64
	// Containment (AIRA-123) is WorkerAdmitContainmentEnforced or
	// WorkerAdmitContainmentAdvisory. It is NOT diagnostic: it decides whether
	// ScopePath/MemoryMax mean anything at all, and a lease whose containment
	// this client does not recognise never reaches a caller.
	Containment string
	// Reserved (AIRA-123) is the advisory grant's booked reservation. Positive
	// exactly when Containment is advisory.
	Reserved int64
	// SwapCap (AIRA-35) carries the daemon's proof about whether this worker's
	// swap could be bounded -- see runner.WorkerAdmitSwapCap* and
	// CreateWorkerScope. It replaces MemoryHigh, which AIRA-35 retired along
	// with the memory.high write it named.
	SwapCap string
	keeper  *leaseKeeper
}

func (l *WorkerAdmitLease) Close() error {
	if l == nil || l.keeper == nil {
		return nil
	}
	return l.keeper.Close()
}

type WorkerAdmitClientRequest struct {
	SocketPath string
	JobID      string
	OuterScope string
	// ParentScopeID is the suite confine scope id this worker is a sub-reservation
	// of (design §16d): the supervisor's own AIRA_CONFINE_SCOPE_ID (the ci-shim
	// sentinel in shim mode), NOT the outer-scope path. The daemon REFUSES an empty
	// one, so the relay always sends what the launcher published.
	ParentScopeID  string
	Signature      string
	EstimatedBytes int64
	// MaxWait == 0 is a non-blocking PROBE (report current available, reserve
	// nothing); any positive value is a blocking CLAIM (wait until granted, no
	// daemon-side or transport timeout — bounded only by ctx). The positive value
	// itself is not sent: a claim omits max_wait_ms, which the daemon reads as
	// "block" (design §4/§6, the S13 confine precedent).
	MaxWait time.Duration
}

type workerAdmitGrant struct {
	State         string `json:"state"`
	Class         string `json:"class"`
	Reason        string `json:"reason,omitempty"`
	Detail        string `json:"detail,omitempty"`
	WorkerID      string `json:"worker_id,omitempty"`
	ScopePath     string `json:"scope_path,omitempty"`
	MemoryMax     int64  `json:"memory_max,omitempty"`
	Containment   string `json:"containment,omitempty"`
	Reserved      int64  `json:"reserved,omitempty"`
	SwapCap       string `json:"swap_cap,omitempty"`
	ParentScopeID string `json:"parent_scope_id,omitempty"`
	// AvailableBytes / AvailableCPU carry the non-blocking probe's current headroom
	// (design §6/§8): what the unified ledger would admit RIGHT NOW, no reservation
	// taken. Meaningful only on a snapshot (state=denied reason=snapshot); zero on a
	// blocking-claim denial or a grant. The daemon marks them omitempty, so a genuine
	// 0 arrives here as 0 — which is correct: reason=snapshot, not the field's
	// presence, is what says "this is a real headroom figure", and a 0 there means
	// "no room" (see RequestWorkerAdmit's snapshot pass-through and the S15 protocol
	// pin, which refuses a daemon too old to speak reason=snapshot at all).
	AvailableBytes int64 `json:"available_bytes,omitempty"`
	AvailableCPU   int64 `json:"available_cpu,omitempty"`
}

// RequestWorkerAdmit dials the daemon and sends one worker-admit request,
// reusing admitThroughDaemon's proven local wire types/framing (this package
// may not import internal/daemon — see admission_linux.go).
//
// It returns a CLASSIFIED outcome and no error. That is deliberate and is the
// structural half of AIRA-42's fix: with no error return there is no
// unclassified path, so the maximally-unsafe disposition
// (WorkerAdmitClassAdmissionUnusable, which makes the aitest supervisor run
// the rest of the suite with no per-worker RAM containment) can only be
// reached by a branch that explicitly names its own evidence. Every
// classification below is made from a STRUCTURAL fact — a dial error, a typed
// transport error, a response code, a catalogued enum value — never from the
// text of a message.
func RequestWorkerAdmit(ctx context.Context, req WorkerAdmitClientRequest) WorkerAdmitOutcome {
	dial := func(dctx context.Context, socket string) (net.Conn, error) {
		var dialer net.Dialer
		return dialer.DialContext(dctx, "unix", socket)
	}
	conn, err := dial(ctx, req.SocketPath)
	if err != nil {
		return classifyWorkerAdmitDialFailure(err)
	}

	// PROBE (MaxWait == 0): a bounded request/response snapshot — set a transport
	// deadline so a stalled daemon cannot hang the probe. CLAIM (MaxWait > 0): a
	// blocking lease with NO transport deadline (design §4/§6, the S13 confine
	// precedent) — the wait is bounded only by ctx, which closes the connection on
	// cancel. On a grant the ctx-close is DETACHED and the keeper owns the lifetime.
	probe := req.MaxWait == 0
	var stopCtxClose func() bool
	if probe {
		deadlineWait := req.MaxWait
		if deadlineWait > time.Duration(mathMaxInt64)-admitTransportGrace {
			deadlineWait = time.Duration(mathMaxInt64) - admitTransportGrace
		}
		transportDeadline := time.Now().Add(deadlineWait + admitTransportGrace)
		if ctxDeadline, ok := ctx.Deadline(); ok && ctxDeadline.Before(transportDeadline) {
			transportDeadline = ctxDeadline
		}
		_ = conn.SetDeadline(transportDeadline)
	} else {
		stopCtxClose = context.AfterFunc(ctx, func() { _ = conn.Close() })
	}
	closeConn := func() {
		if stopCtxClose != nil {
			stopCtxClose()
		}
		_ = conn.Close()
	}

	frame := runnerAdmitRequestFrame{Proto: DaemonProtocolVersion, Scope: map[string]any{}}
	frame.Request.Verb = "worker-admit"
	frame.Request.Args = map[string]any{
		"job_id": req.JobID, "outer_scope": req.OuterScope, "signature": req.Signature,
		"estimated_bytes": req.EstimatedBytes, "parent_scope_id": req.ParentScopeID,
	}
	if probe {
		// PRESENT and zero → non-blocking probe. A CLAIM omits it → the daemon blocks.
		frame.Request.Args["max_wait_ms"] = int64(0)
	}
	if err := writeRunnerAdmitFrame(conn, frame); err != nil {
		closeConn()
		return WorkerAdmitOutcome{
			State: WorkerAdmitStateUnavailable, Class: WorkerAdmitClassAdmissionUnusable,
			Reason: WorkerAdmitReasonRequestSendFailed, Detail: "send worker-admit request: " + err.Error(),
		}
	}
	var response runnerAdmitResponseFrame
	if err := readRunnerAdmitFrame(conn, &response); err != nil {
		closeConn()
		return classifyWorkerAdmitReadFailure(err)
	}
	if response.Code != "OK" {
		closeConn()
		return classifyWorkerAdmitDaemonError(response)
	}
	var grant workerAdmitGrant
	if err := json.Unmarshal(response.Data, &grant); err != nil {
		closeConn()
		// An OK frame whose payload is not a worker-admit response is the
		// daemon and this client disagreeing about the channel itself.
		// Terminal and loud, never a silent unconfined fallback.
		return WorkerAdmitOutcome{
			State: WorkerAdmitStateUnevaluated, Class: WorkerAdmitClassContractViolation,
			Reason: WorkerAdmitReasonMalformedResponse, Detail: "malformed worker-admit response: " + err.Error(),
		}
	}
	if !IsWorkerAdmitState(grant.State) || !IsWorkerAdmitClass(grant.Class) ||
		(grant.State == WorkerAdmitStateGranted) != (grant.Class == WorkerAdmitClassGranted) {
		closeConn()
		// Protocol versions matched (or we would not be here), so this is
		// not skew: the daemon produced an outcome outside the catalogue,
		// or one that contradicts itself. Refusing it is the point.
		return WorkerAdmitOutcome{
			State: WorkerAdmitStateUnevaluated, Class: WorkerAdmitClassContractViolation,
			Reason: WorkerAdmitReasonUnknownDaemonOutcome,
			Detail: "daemon reported state=" + grant.State + " class=" + grant.Class,
		}
	}
	if grant.State != WorkerAdmitStateGranted {
		closeConn()
		// The daemon's own classification passes through unchanged (a denial, a
		// timeout, or a probe SNAPSHOT — the latter carrying available_bytes/cpu the
		// daemon reported). This is the one place a class crosses a process boundary
		// without being re-derived, the property AIRA-42 asked for. AvailableBytes/
		// AvailableCPU are meaningful only on a snapshot; on every other non-grant the
		// daemon leaves them zero and WorkerAdmitOutcomeLine emits nothing for them.
		return WorkerAdmitOutcome{
			State: grant.State, Class: grant.Class, Reason: grant.Reason, Detail: grant.Detail,
			AvailableBytes: grant.AvailableBytes, AvailableCPU: grant.AvailableCPU,
		}
	}
	if problem := workerAdmitGrantProblem(grant); problem != "" {
		closeConn()
		// A grant whose placement coordinates are unusable is a CONTRACT problem: the
		// daemon and this client disagree about what a grant is. A pure contract guard
		// against a daemon that is buggy or out of lockstep, moving the outcome AWAY
		// from a containment-stripping class.
		return WorkerAdmitOutcome{
			State: WorkerAdmitStateUnevaluated, Class: WorkerAdmitClassContractViolation,
			Reason: WorkerAdmitReasonMalformedGrant, Detail: problem,
		}
	}
	// GRANTED. Hand the connection to a keeper: for an ENFORCED grant it reconnects
	// and re-declares (ARDR frame keyed on the scope DIRNAME — see below) across a daemon
	// restart; an ADVISORY (shim) grant has no scope path and no ARDR key, so it HOLDS only.
	// Detach the ctx-close first — the keeper now owns the connection's lifetime, and
	// the CLI closes the lease (stopping the keeper) on stdin EOF or signal.
	if stopCtxClose != nil {
		stopCtxClose()
	}
	_ = conn.SetDeadline(time.Time{})
	var reDeclareFrame []byte
	if grant.Containment == WorkerAdmitContainmentEnforced && grant.ScopePath != "" {
		// A minted scope path is valid utf8 and the reserve is > 0, so this cannot
		// fail in practice; if it ever does, hold WITHOUT reconnect (the worker runs
		// under its cgroup cap) rather than refuse the grant.
		// S2a §16.1 (P1-A): re-declare keyed by the worker scope's DIRNAME
		// (TrimPrefix(Base(ScopePath), ".aira-")), NOT the full path. The daemon's
		// fresh-admit ledger keys the lease by the minted scope-id (the dirname), so a
		// path key here would land the post-restart re-anchor under a DIFFERENT key —
		// the survivor would read as a new lease and the original as an orphan (the
		// restart merge-gate's exact-key-set assertion catches this). This is the same
		// key Task 1's dirname alignment (`confineScopeDirName` / the reaper's
		// `hasLiveLease`) uses, so the whole worker-lease path lines up across a restart.
		reDeclareFrame, _ = redeclare.EncodeFrame(redeclare.Record{
			ScopeID:       strings.TrimPrefix(filepath.Base(grant.ScopePath), ".aira-"),
			RAMBytes:      uint64(grant.MemoryMax),
			CPUCores:      uint32(DefaultConfineCPUCores),
			ParentScopeID: grant.ParentScopeID,
		})
	}
	return WorkerAdmitOutcome{
		State: WorkerAdmitStateGranted, Class: WorkerAdmitClassGranted,
		Lease: &WorkerAdmitLease{
			WorkerID: grant.WorkerID, ScopePath: grant.ScopePath,
			MemoryMax: grant.MemoryMax, SwapCap: grant.SwapCap,
			Containment: grant.Containment, Reserved: grant.Reserved,
			keeper: newLeaseKeeperFrame(conn, reDeclareFrame, grant.ScopePath, dial, req.SocketPath),
		},
	}
}

// classifyWorkerAdmitDialFailure splits a dial error by errno.
//
// A dial that failed because THIS process is out of file descriptors or memory
// says nothing about the daemon, and those conditions peak under exactly the
// contention this path exists for (found by Sol build-review). Reporting it as
// admission-unusable would strip RAM containment for the whole remaining run
// over a momentary local resource pinch against a perfectly healthy daemon —
// the same misdiagnosis supervisor.py already avoids for an EAGAIN/ENOMEM fork
// failure when launching this very relay. Every other dial error (ENOENT,
// ECONNREFUSED) IS evidence there is no daemon to talk to.
func classifyWorkerAdmitDialFailure(err error) WorkerAdmitOutcome {
	if errors.Is(err, syscall.EMFILE) || errors.Is(err, syscall.ENFILE) ||
		errors.Is(err, syscall.ENOMEM) || errors.Is(err, syscall.EAGAIN) {
		return WorkerAdmitOutcome{
			State: WorkerAdmitStateUnevaluated, Class: WorkerAdmitClassContended,
			Reason: WorkerAdmitReasonDialResourceExhausted, Detail: "dial daemon: " + err.Error(),
		}
	}
	return WorkerAdmitOutcome{
		State: WorkerAdmitStateUnavailable, Class: WorkerAdmitClassAdmissionUnusable,
		Reason: WorkerAdmitReasonDialFailed, Detail: "dial daemon: " + err.Error(),
	}
}

// workerAdmitGrantProblem states why a granted payload is unusable, or "" if
// it is fine. AIRA-123 moved the rules themselves into
// workerAdmitGrantShapeProblem, which the OUTCOME RENDERER also calls, so this
// client and every producer enforce one definition of a well-formed grant
// rather than two that can drift. What it enforces is unchanged in substance
// for an enforced grant (a worker id, a scope path to place into, a positive
// memory_max) and adds the advisory grade's mirror rules.
//
// AIRA-35 removed the two memory_high checks that used to live here (positive,
// and strictly below memory_max) together with the memory.high write they
// guarded: with no such field on the wire there is nothing left to validate,
// and re-adding a check for a field this protocol no longer defines would
// reject every correct grant.
//
// Since AIRA-39 the daemon creates the scope, so a grant that violates these
// should be impossible to produce — which is the point: reaching this branch
// means the daemon and this client disagree about the channel, and the honest
// report is contract-violation (terminal, loud) rather than any class that
// blames a local mechanism or strips containment.
func workerAdmitGrantProblem(grant workerAdmitGrant) string {
	return workerAdmitGrantShapeProblem(WorkerAdmitGrantFields{
		ScopePath: grant.ScopePath, WorkerID: grant.WorkerID, MemoryMax: grant.MemoryMax,
		Containment: grant.Containment, Reserved: grant.Reserved,
	})
}

// classifyWorkerAdmitReadFailure sorts a failed response read by TYPE, never
// by message text. readRunnerAdmitFrame can fail in exactly three ways, all
// enumerated here:
//
//  1. io.ReadFull on the socket — a net.Error timeout, io.EOF /
//     io.ErrUnexpectedEOF, or a syscall errno for a broken connection. None of
//     these establishes that the daemon is gone: it was dialled and the
//     request WAS sent, only the reply was late or cut short. They are
//     retriable, and a genuinely dead daemon disambiguates itself for free on
//     the very next attempt, which fails at the DIAL instead and is classified
//     admission-unusable above. (AIRA-92 established this reasoning for the
//     timeout case specifically; it holds identically for a severed
//     connection.)
//  2. errWorkerAdmitFrameSize — a length header of 0 or > 16 MiB.
//  3. a json.Unmarshal failure on the frame body.
//
// (2) and (3) are a live daemon emitting something unintelligible. Retrying
// cannot help and falling back would silently strip containment, so they are
// contract violations: terminal and loud.
//
// The final branch is unreachable by construction given the enumeration above
// and exists only so the function is total. It is classed admission-unusable,
// preserving the behaviour a pre-AIRA-42 client had for every unrecognised
// transport error, rather than inventing a new indefinite-retry hang class
// for a case no code path produces.
func classifyWorkerAdmitReadFailure(err error) WorkerAdmitOutcome {
	var netErr net.Error
	switch {
	case errors.As(err, &netErr) && netErr.Timeout():
		return WorkerAdmitOutcome{
			State: WorkerAdmitStateTimeout, Class: WorkerAdmitClassContended,
			Reason: WorkerAdmitReasonResponseTimeout, Detail: "read worker-admit response: " + err.Error(),
		}
	case errors.Is(err, errWorkerAdmitFrameSize):
		return WorkerAdmitOutcome{
			State: WorkerAdmitStateUnevaluated, Class: WorkerAdmitClassContractViolation,
			Reason: WorkerAdmitReasonMalformedResponse, Detail: "read worker-admit response: " + err.Error(),
		}
	case isWorkerAdmitJSONError(err):
		return WorkerAdmitOutcome{
			State: WorkerAdmitStateUnevaluated, Class: WorkerAdmitClassContractViolation,
			Reason: WorkerAdmitReasonMalformedResponse, Detail: "read worker-admit response: " + err.Error(),
		}
	case isWorkerAdmitConnectionBroken(err):
		return WorkerAdmitOutcome{
			State: WorkerAdmitStateUnevaluated, Class: WorkerAdmitClassContended,
			Reason: WorkerAdmitReasonResponseInterrupted, Detail: "read worker-admit response: " + err.Error(),
		}
	default:
		return WorkerAdmitOutcome{
			State: WorkerAdmitStateUnavailable, Class: WorkerAdmitClassAdmissionUnusable,
			Reason: WorkerAdmitReasonResponseFailed, Detail: "read worker-admit response: " + err.Error(),
		}
	}
}

func isWorkerAdmitJSONError(err error) bool {
	var syntaxErr *json.SyntaxError
	var typeErr *json.UnmarshalTypeError
	return errors.As(err, &syntaxErr) || errors.As(err, &typeErr)
}

func isWorkerAdmitConnectionBroken(err error) bool {
	return errors.Is(err, net.ErrClosed) ||
		errors.Is(err, syscall.ECONNRESET) || errors.Is(err, syscall.ECONNABORTED) ||
		errors.Is(err, syscall.EPIPE) || errors.Is(err, syscall.ENOTCONN) ||
		errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF)
}

// classifyWorkerAdmitDaemonError sorts a non-OK response frame by CODE and by
// the frame's own proto field — never by the error sentence.
//
// The E_DAEMON_PROTOCOL split is AIRA-45/AIRA-83(b). The daemon uses that one
// code for two unrelated conditions: its protocol-VERSION check
// (protocolMismatchFrame, which is the only frame that sets a non-zero proto)
// and validateWorkerAdmitArgs's per-request argument rejection (errorFrame,
// proto zero). The old classifier bucketed both as "this request can never be
// admitted at this sizing", so the first daemon/client version skew after a
// ProtocolVersion bump would have marked every queued test unevaluated with a
// diagnostic that named the wrong problem and no remedy. A skew is now
// admission-unusable — the same disposition `aira confine`'s own admission
// client already takes for it — and says how to fix it.
func classifyWorkerAdmitDaemonError(response runnerAdmitResponseFrame) WorkerAdmitOutcome {
	detail := response.Error
	if detail == "" {
		detail = response.Code
	}
	switch {
	case response.Code == daemonProtocolCode && response.Proto != 0 && response.Proto != DaemonProtocolVersion:
		return WorkerAdmitOutcome{
			State: WorkerAdmitStateUnavailable, Class: WorkerAdmitClassAdmissionUnusable,
			Reason: WorkerAdmitReasonProtocolVersionMismatch,
			Detail: detail + " -- this client speaks protocol " + strconv.Itoa(DaemonProtocolVersion) +
				"; reinstall with `aira install` and restart aira-daemon.service",
		}
	case response.Code == daemonProtocolCode:
		return WorkerAdmitOutcome{
			State: WorkerAdmitStateDenied, Class: WorkerAdmitClassRequestInvalid,
			Reason: WorkerAdmitReasonRequestRejected, Detail: detail,
		}
	default:
		// A code this client does not know is the two sides disagreeing
		// about the channel. Terminal and loud rather than a silent
		// unconfined fallback (the pre-AIRA-42 treatment for every
		// unrecognised code).
		return WorkerAdmitOutcome{
			State: WorkerAdmitStateUnevaluated, Class: WorkerAdmitClassContractViolation,
			Reason: WorkerAdmitReasonDaemonError, Detail: detail,
		}
	}
}

// daemonProtocolCode mirrors daemon.CodeProtocol; internal/runner may not
// import internal/daemon (the daemon imports the runner). Pinned equal by
// TestRunnerDaemonProtocolCodeMatchesTheDaemon in the external runner_test
// package, alongside the existing DaemonProtocolVersion pin.
const daemonProtocolCode = "E_DAEMON_PROTOCOL"
