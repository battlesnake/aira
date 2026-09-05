//go:build linux

package runner

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"strconv"
	"syscall"
	"time"
)

// WorkerAdmitLease is a granted worker-admit connection, held open until Close
// releases it. The daemon parks its side on the same connection and returns
// when it observes the peer disconnect.
//
// It does NOT free ledger capacity, and this comment used to say it did —
// "the daemon frees the ledger entry when it detects the peer disconnect",
// true only of the pre-AIRA-39/41 in-memory grants map. AIRA-41 made the
// ledger Σ memory.max over the outer scope's real `.aira-worker-*` children
// precisely so a killed or exited relay could no longer free capacity while
// its worker was still alive under a still-intact cap; capacity is released by
// REMOVING the scope (supervisor.py's _forget_worker_scope, after it has
// reaped the worker), never by closing this. The stale wording outlived the
// fix long enough to be restated as fact in AIRA-43's own problem statement,
// so it is corrected here rather than left to mislead again.
//
// What closing this lease does release is the daemon-side connection and the
// shared admission slot workerAdmitConnection holds for a granted connection's
// whole lifetime.
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
	Class      string `json:"class"`
	Reason     string `json:"reason,omitempty"`
	Detail     string `json:"detail,omitempty"`
	WorkerID   string `json:"worker_id,omitempty"`
	ScopePath  string `json:"scope_path,omitempty"`
	MemoryMax  int64  `json:"memory_max,omitempty"`
	MemoryHigh int64  `json:"memory_high,omitempty"`
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
	var dialer net.Dialer
	conn, err := dialer.DialContext(ctx, "unix", req.SocketPath)
	if err != nil {
		return classifyWorkerAdmitDialFailure(err)
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
		return WorkerAdmitOutcome{
			State: WorkerAdmitStateUnavailable, Class: WorkerAdmitClassAdmissionUnusable,
			Reason: WorkerAdmitReasonRequestSendFailed, Detail: "send worker-admit request: " + err.Error(),
		}
	}
	var response runnerAdmitResponseFrame
	if err := readRunnerAdmitFrame(conn, &response); err != nil {
		_ = conn.Close()
		return classifyWorkerAdmitReadFailure(err)
	}
	if response.Code != "OK" {
		_ = conn.Close()
		return classifyWorkerAdmitDaemonError(response)
	}
	var grant workerAdmitGrant
	if err := json.Unmarshal(response.Data, &grant); err != nil {
		_ = conn.Close()
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
		_ = conn.Close()
		// Protocol versions matched (or we would not be here), so this is
		// not skew: the daemon produced an outcome outside the catalogue,
		// or one that contradicts itself. Refusing it is the point — the
		// old code's equivalent situation fell through to "daemon
		// unavailable" and stripped containment for the whole run.
		return WorkerAdmitOutcome{
			State: WorkerAdmitStateUnevaluated, Class: WorkerAdmitClassContractViolation,
			Reason: WorkerAdmitReasonUnknownDaemonOutcome,
			Detail: "daemon reported state=" + grant.State + " class=" + grant.Class,
		}
	}
	if grant.State != WorkerAdmitStateGranted {
		_ = conn.Close()
		// The daemon's own classification passes through unchanged. This is
		// the one place a class crosses a process boundary without being
		// re-derived, which is exactly the property AIRA-42 asked for.
		return WorkerAdmitOutcome{State: grant.State, Class: grant.Class, Reason: grant.Reason, Detail: grant.Detail}
	}
	if problem := workerAdmitGrantProblem(grant); problem != "" {
		_ = conn.Close()
		// A grant whose placement coordinates are unusable is a CONTRACT
		// problem: the daemon and this client disagree about what a grant is.
		//
		// Found by Sol build-review against the pre-AIRA-39 shape, where the
		// CLI called CreateWorkerScope itself and a grant with
		// memory_high >= memory_max sailed through to it, failed there, and
		// was reported `placement-failed` — one of the two classes that make
		// the supervisor run the rest of the suite UNCONFINED. AIRA-39 moved
		// that creation into the daemon, so this is no longer the difference
		// between two classes on a live path; it is a pure contract guard
		// against a daemon that is buggy or out of lockstep with this client.
		// Kept for that reason, and because the check is nearly free and only
		// ever moves an outcome AWAY from a containment-stripping class.
		return WorkerAdmitOutcome{
			State: WorkerAdmitStateUnevaluated, Class: WorkerAdmitClassContractViolation,
			Reason: WorkerAdmitReasonMalformedGrant, Detail: problem,
		}
	}
	return WorkerAdmitOutcome{
		State: WorkerAdmitStateGranted, Class: WorkerAdmitClassGranted,
		Lease: &WorkerAdmitLease{
			WorkerID: grant.WorkerID, ScopePath: grant.ScopePath,
			MemoryMax: grant.MemoryMax, MemoryHigh: grant.MemoryHigh, conn: conn,
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
// it is fine. It enforces exactly the invariants every consumer of a grant
// depends on: a worker id and scope path to place into, both limits positive,
// and memory_high below memory_max (spec 3.3's soft-throttle-below-hard-cap
// rule, which runner.CreateWorkerScope itself refuses to violate and
// supervisor.py's own grant validation re-checks).
//
// Since AIRA-39 the daemon creates the scope, so a grant that violates these
// should be impossible to produce — which is the point: reaching this branch
// means the daemon and this client disagree about the channel, and the honest
// report is contract-violation (terminal, loud) rather than any class that
// blames a local mechanism or strips containment.
func workerAdmitGrantProblem(grant workerAdmitGrant) string {
	switch {
	case grant.WorkerID == "":
		return "granted outcome carries no worker_id"
	case grant.ScopePath == "":
		return "granted outcome carries no scope_path"
	case grant.MemoryMax <= 0:
		return "granted outcome memory_max must be positive, got " + strconv.FormatInt(grant.MemoryMax, 10)
	case grant.MemoryHigh <= 0:
		return "granted outcome memory_high must be positive, got " + strconv.FormatInt(grant.MemoryHigh, 10)
	case grant.MemoryHigh >= grant.MemoryMax:
		return "granted outcome memory_high (" + strconv.FormatInt(grant.MemoryHigh, 10) +
			") must be below memory_max (" + strconv.FormatInt(grant.MemoryMax, 10) + ")"
	default:
		return ""
	}
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
