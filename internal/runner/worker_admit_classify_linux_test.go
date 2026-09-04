//go:build linux

package runner

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"os"
	"path/filepath"
	"reflect"
	"syscall"
	"testing"
	"time"
)

type fakeNetTimeout struct{}

func (fakeNetTimeout) Error() string   { return "i/o timeout" }
func (fakeNetTimeout) Timeout() bool   { return true }
func (fakeNetTimeout) Temporary() bool { return true }

// verifies: AIRA-42 — the transport classifier sorts failures by TYPE. Each
// row would previously have been re-derived on the far side by matching words
// like "i/o timeout" or "EOF" out of a wrapped error sentence.
func TestClassifyWorkerAdmitReadFailureSortsByType(t *testing.T) {
	tests := []struct {
		name       string
		err        error
		wantState  string
		wantClass  string
		wantReason string
	}{
		{
			"socket deadline", fakeNetTimeout{},
			WorkerAdmitStateTimeout, WorkerAdmitClassContended, WorkerAdmitReasonResponseTimeout,
		},
		{
			"frame size sentinel", errWorkerAdmitFrameSize,
			WorkerAdmitStateUnevaluated, WorkerAdmitClassContractViolation, WorkerAdmitReasonMalformedResponse,
		},
		{
			"wrapped frame size sentinel", errors.Join(errors.New("read frame"), errWorkerAdmitFrameSize),
			WorkerAdmitStateUnevaluated, WorkerAdmitClassContractViolation, WorkerAdmitReasonMalformedResponse,
		},
		{
			"json syntax error", &json.SyntaxError{},
			WorkerAdmitStateUnevaluated, WorkerAdmitClassContractViolation, WorkerAdmitReasonMalformedResponse,
		},
		{
			"json type error", &json.UnmarshalTypeError{Value: "string", Type: reflect.TypeOf(0)},
			WorkerAdmitStateUnevaluated, WorkerAdmitClassContractViolation, WorkerAdmitReasonMalformedResponse,
		},
		{
			"eof", io.EOF,
			WorkerAdmitStateUnevaluated, WorkerAdmitClassContended, WorkerAdmitReasonResponseInterrupted,
		},
		{
			"unexpected eof", io.ErrUnexpectedEOF,
			WorkerAdmitStateUnevaluated, WorkerAdmitClassContended, WorkerAdmitReasonResponseInterrupted,
		},
		{
			"connection reset", &net.OpError{Op: "read", Err: syscall.ECONNRESET},
			WorkerAdmitStateUnevaluated, WorkerAdmitClassContended, WorkerAdmitReasonResponseInterrupted,
		},
		{
			"broken pipe", &net.OpError{Op: "read", Err: syscall.EPIPE},
			WorkerAdmitStateUnevaluated, WorkerAdmitClassContended, WorkerAdmitReasonResponseInterrupted,
		},
		{
			// Unreachable by construction (readRunnerAdmitFrame produces
			// only the classes above) but pinned so the total function
			// keeps a defined, non-retriable answer rather than drifting
			// into an indefinite-retry hang.
			"unenumerated", errors.New("something else entirely"),
			WorkerAdmitStateUnavailable, WorkerAdmitClassAdmissionUnusable, WorkerAdmitReasonResponseFailed,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			outcome := classifyWorkerAdmitReadFailure(test.err)
			if outcome.State != test.wantState || outcome.Class != test.wantClass || outcome.Reason != test.wantReason {
				t.Fatalf("outcome=%+v, want state=%s class=%s reason=%s",
					outcome, test.wantState, test.wantClass, test.wantReason)
			}
		})
	}
}

// verifies: AIRA-45, AIRA-83(b) — a protocol-VERSION mismatch and a
// per-request argument rejection share one error code and used to share one
// classifier bucket ("cannot be admitted at this sizing"). They are now split
// by the response frame's own proto field, a structural fact rather than a
// phrase inside the error sentence.
func TestClassifyWorkerAdmitDaemonErrorSplitsVersionSkewFromArgumentRejection(t *testing.T) {
	tests := []struct {
		name       string
		response   runnerAdmitResponseFrame
		wantState  string
		wantClass  string
		wantReason string
	}{
		{
			name: "version skew: the daemon names its own protocol",
			response: runnerAdmitResponseFrame{
				Code: "E_DAEMON_PROTOCOL", Proto: DaemonProtocolVersion + 1,
				Error: "E_DAEMON_PROTOCOL: daemon protocol is 7, client requested 6",
			},
			wantState: WorkerAdmitStateUnavailable, wantClass: WorkerAdmitClassAdmissionUnusable,
			wantReason: WorkerAdmitReasonProtocolVersionMismatch,
		},
		{
			name: "older daemon, same split",
			response: runnerAdmitResponseFrame{
				Code: "E_DAEMON_PROTOCOL", Proto: DaemonProtocolVersion - 1,
				Error: "E_DAEMON_PROTOCOL: daemon protocol is 5, client requested 6",
			},
			wantState: WorkerAdmitStateUnavailable, wantClass: WorkerAdmitClassAdmissionUnusable,
			wantReason: WorkerAdmitReasonProtocolVersionMismatch,
		},
		{
			name: "argument rejection: same code, no proto",
			response: runnerAdmitResponseFrame{
				Code: "E_DAEMON_PROTOCOL",
				Error: "E_DAEMON_PROTOCOL: worker-admit estimated_bytes must be at least " +
					"1048576 bytes and no larger than 1125899906842624",
			},
			wantState: WorkerAdmitStateDenied, wantClass: WorkerAdmitClassRequestInvalid,
			wantReason: WorkerAdmitReasonRequestRejected,
		},
		{
			name: "matching proto is not a skew",
			response: runnerAdmitResponseFrame{
				Code: "E_DAEMON_PROTOCOL", Proto: DaemonProtocolVersion,
				Error: "E_DAEMON_PROTOCOL: worker-admit job_id is required",
			},
			wantState: WorkerAdmitStateDenied, wantClass: WorkerAdmitClassRequestInvalid,
			wantReason: WorkerAdmitReasonRequestRejected,
		},
		{
			name:      "a code this client does not know is a contract violation, not a fallback",
			response:  runnerAdmitResponseFrame{Code: "E_DAEMON_INTERNAL", Error: "E_DAEMON_INTERNAL: recovered request panic"},
			wantState: WorkerAdmitStateUnevaluated, wantClass: WorkerAdmitClassContractViolation,
			wantReason: WorkerAdmitReasonDaemonError,
		},
		{
			name:      "an unknown code with no error text still classifies",
			response:  runnerAdmitResponseFrame{Code: "E_DAEMON_BUSY"},
			wantState: WorkerAdmitStateUnevaluated, wantClass: WorkerAdmitClassContractViolation,
			wantReason: WorkerAdmitReasonDaemonError,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			outcome := classifyWorkerAdmitDaemonError(test.response)
			if outcome.State != test.wantState || outcome.Class != test.wantClass || outcome.Reason != test.wantReason {
				t.Fatalf("outcome=%+v, want state=%s class=%s reason=%s",
					outcome, test.wantState, test.wantClass, test.wantReason)
			}
			if outcome.Detail == "" {
				t.Fatal("every classified outcome must carry a human detail")
			}
		})
	}
}

// serveOneWorkerAdmit answers exactly one worker-admit request with respond's
// frame and returns the socket path.
func serveOneWorkerAdmit(t *testing.T, respond func() runnerAdmitResponseFrame) string {
	t.Helper()
	socket := filepath.Join(t.TempDir(), "daemon.sock")
	listener, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	go func() {
		conn, err := listener.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		var request runnerAdmitRequestFrame
		if err := readRunnerAdmitFrame(conn, &request); err != nil {
			return
		}
		_ = writeRunnerAdmitFrame(conn, respond())
	}()
	return socket
}

func workerAdmitTestRequest(socket string) WorkerAdmitClientRequest {
	return WorkerAdmitClientRequest{
		SocketPath: socket, JobID: "job-1", OuterScope: "/outer",
		EstimatedBytes: 1 << 20, MaxWait: 2 * time.Second,
	}
}

// verifies: AIRA-42 — every end-to-end path returns a classified outcome, and
// no path returns an unclassified error the caller has to interpret.
func TestRequestWorkerAdmitClassifiesEndToEnd(t *testing.T) {
	t.Run("dial failure", func(t *testing.T) {
		outcome := RequestWorkerAdmit(context.Background(), workerAdmitTestRequest(
			filepath.Join(t.TempDir(), "absent.sock")))
		if outcome.Class != WorkerAdmitClassAdmissionUnusable || outcome.Reason != WorkerAdmitReasonDialFailed {
			t.Fatalf("outcome=%+v", outcome)
		}
		if outcome.Lease != nil {
			t.Fatal("a dial failure must not carry a lease")
		}
	})

	// verifies: AIRA-42 — a dial that failed on THIS process's own resource
	// limits is retriable, not evidence that admission is unusable (Sol
	// build-review). supervisor.py already applies the identical reasoning to
	// an EAGAIN/ENOMEM fork failure when launching this relay.
	t.Run("local resource exhaustion on dial is retriable", func(t *testing.T) {
		outcome := classifyWorkerAdmitDialFailure(&net.OpError{Op: "dial", Err: syscall.EMFILE})
		if outcome.Class != WorkerAdmitClassContended || outcome.Reason != WorkerAdmitReasonDialResourceExhausted {
			t.Fatalf("outcome=%+v", outcome)
		}
		refused := classifyWorkerAdmitDialFailure(&net.OpError{Op: "dial", Err: syscall.ECONNREFUSED})
		if refused.Class != WorkerAdmitClassAdmissionUnusable || refused.Reason != WorkerAdmitReasonDialFailed {
			t.Fatalf("outcome=%+v: a refused connection IS evidence there is no daemon", refused)
		}
	})

	t.Run("the daemon's own classification passes through verbatim", func(t *testing.T) {
		data, _ := json.Marshal(workerAdmitGrant{
			State: WorkerAdmitStateDenied, Class: WorkerAdmitClassRequestInvalid,
			Reason: WorkerAdmitReasonExceedsCeiling, Detail: "estimated 2 bytes exceeds 1",
		})
		socket := serveOneWorkerAdmit(t, func() runnerAdmitResponseFrame {
			return runnerAdmitResponseFrame{OK: false, Code: "OK", Data: data}
		})
		outcome := RequestWorkerAdmit(context.Background(), workerAdmitTestRequest(socket))
		if outcome.State != WorkerAdmitStateDenied || outcome.Class != WorkerAdmitClassRequestInvalid ||
			outcome.Reason != WorkerAdmitReasonExceedsCeiling || outcome.Detail != "estimated 2 bytes exceeds 1" {
			t.Fatalf("outcome=%+v: the daemon's classification must not be re-derived", outcome)
		}
	})

	t.Run("an uncatalogued daemon state is a contract violation", func(t *testing.T) {
		data, _ := json.Marshal(map[string]any{"state": "wat", "class": WorkerAdmitClassContended})
		socket := serveOneWorkerAdmit(t, func() runnerAdmitResponseFrame {
			return runnerAdmitResponseFrame{Code: "OK", Data: data}
		})
		outcome := RequestWorkerAdmit(context.Background(), workerAdmitTestRequest(socket))
		if outcome.Class != WorkerAdmitClassContractViolation || outcome.Reason != WorkerAdmitReasonUnknownDaemonOutcome {
			t.Fatalf("outcome=%+v: an unrecognised outcome must be terminal, never a silent fallback", outcome)
		}
	})

	t.Run("an uncatalogued daemon class is a contract violation", func(t *testing.T) {
		data, _ := json.Marshal(map[string]any{"state": WorkerAdmitStateDenied, "class": "wat"})
		socket := serveOneWorkerAdmit(t, func() runnerAdmitResponseFrame {
			return runnerAdmitResponseFrame{Code: "OK", Data: data}
		})
		outcome := RequestWorkerAdmit(context.Background(), workerAdmitTestRequest(socket))
		if outcome.Class != WorkerAdmitClassContractViolation {
			t.Fatalf("outcome=%+v", outcome)
		}
	})

	t.Run("a daemon claiming granted with a non-granted class is refused", func(t *testing.T) {
		data, _ := json.Marshal(map[string]any{
			"state": WorkerAdmitStateGranted, "class": WorkerAdmitClassContended,
			"worker_id": "1", "scope_path": "/outer/.aira-worker-1",
		})
		socket := serveOneWorkerAdmit(t, func() runnerAdmitResponseFrame {
			return runnerAdmitResponseFrame{Code: "OK", Data: data}
		})
		outcome := RequestWorkerAdmit(context.Background(), workerAdmitTestRequest(socket))
		if outcome.Class != WorkerAdmitClassContractViolation || outcome.Lease != nil {
			t.Fatalf("outcome=%+v: a self-contradicting grant must not be honoured", outcome)
		}
	})

	t.Run("a protocol-version mismatch is not a sizing verdict", func(t *testing.T) {
		socket := serveOneWorkerAdmit(t, func() runnerAdmitResponseFrame {
			return runnerAdmitResponseFrame{
				Code: "E_DAEMON_PROTOCOL", Proto: DaemonProtocolVersion + 1,
				Error: "E_DAEMON_PROTOCOL: daemon protocol is 7, client requested 6",
			}
		})
		outcome := RequestWorkerAdmit(context.Background(), workerAdmitTestRequest(socket))
		if outcome.Class == WorkerAdmitClassRequestInvalid {
			t.Fatal("a stale-binary skew must not be reported as a per-request rejection (AIRA-45)")
		}
		if outcome.Class != WorkerAdmitClassAdmissionUnusable || outcome.Reason != WorkerAdmitReasonProtocolVersionMismatch {
			t.Fatalf("outcome=%+v", outcome)
		}
	})

	// verifies: AIRA-42 — a grant whose placement coordinates are unusable is
	// a CONTRACT violation, not a local placement failure. Found by Sol
	// build-review: memory_high >= memory_max is exactly what
	// CreateWorkerScope refuses (worker_scope_linux.go:29), so without this
	// check such a grant produced `placement-failed` — one of the two classes
	// that make the supervisor run the rest of the suite UNCONFINED.
	for _, bad := range []struct {
		name  string
		grant workerAdmitGrant
	}{
		{"memory_high at memory_max", workerAdmitGrant{WorkerID: "1", ScopePath: "/s", MemoryMax: 400, MemoryHigh: 400}},
		{"memory_high above memory_max", workerAdmitGrant{WorkerID: "1", ScopePath: "/s", MemoryMax: 400, MemoryHigh: 500}},
		{"memory_high absent", workerAdmitGrant{WorkerID: "1", ScopePath: "/s", MemoryMax: 400}},
		{"memory_max absent", workerAdmitGrant{WorkerID: "1", ScopePath: "/s", MemoryHigh: 320}},
		{"no worker id", workerAdmitGrant{ScopePath: "/s", MemoryMax: 400, MemoryHigh: 320}},
		{"no scope path", workerAdmitGrant{WorkerID: "1", MemoryMax: 400, MemoryHigh: 320}},
	} {
		t.Run("an unusable grant is a contract violation: "+bad.name, func(t *testing.T) {
			bad.grant.State = WorkerAdmitStateGranted
			bad.grant.Class = WorkerAdmitClassGranted
			data, _ := json.Marshal(bad.grant)
			socket := serveOneWorkerAdmit(t, func() runnerAdmitResponseFrame {
				return runnerAdmitResponseFrame{OK: true, Code: "OK", Data: data}
			})
			outcome := RequestWorkerAdmit(context.Background(), workerAdmitTestRequest(socket))
			if outcome.Class == WorkerAdmitClassPlacementFailed {
				t.Fatal("an unusable grant must not be blamed on local placement — that class strips containment")
			}
			if outcome.Class != WorkerAdmitClassContractViolation || outcome.Reason != WorkerAdmitReasonMalformedGrant {
				t.Fatalf("outcome=%+v", outcome)
			}
			if outcome.Lease != nil {
				t.Fatal("an unusable grant must not produce a lease")
			}
		})
	}

	t.Run("a granted response yields a lease", func(t *testing.T) {
		data, _ := json.Marshal(workerAdmitGrant{
			State: WorkerAdmitStateGranted, Class: WorkerAdmitClassGranted,
			WorkerID: "3", ScopePath: "/outer/.aira-worker-3", MemoryMax: 400, MemoryHigh: 320,
		})
		socket := serveOneWorkerAdmit(t, func() runnerAdmitResponseFrame {
			return runnerAdmitResponseFrame{OK: true, Code: "OK", Data: data}
		})
		outcome := RequestWorkerAdmit(context.Background(), workerAdmitTestRequest(socket))
		if !outcome.Granted() || outcome.Lease == nil {
			t.Fatalf("outcome=%+v", outcome)
		}
		if outcome.Lease.WorkerID != "3" || outcome.Lease.MemoryMax != 400 || outcome.Lease.MemoryHigh != 320 {
			t.Fatalf("lease=%+v", outcome.Lease)
		}
		_ = outcome.Lease.Close()
	})

	t.Run("a daemon that answers with garbage is a contract violation", func(t *testing.T) {
		socket := filepath.Join(t.TempDir(), "daemon.sock")
		listener, err := net.Listen("unix", socket)
		if err != nil {
			t.Fatalf("listen: %v", err)
		}
		t.Cleanup(func() { _ = listener.Close() })
		go func() {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			defer conn.Close()
			var request runnerAdmitRequestFrame
			if err := readRunnerAdmitFrame(conn, &request); err != nil {
				return
			}
			// A valid length header followed by bytes that are not JSON.
			_, _ = conn.Write([]byte{0, 0, 0, 4, 'n', 'o', 'p', 'e'})
		}()
		outcome := RequestWorkerAdmit(context.Background(), workerAdmitTestRequest(socket))
		if outcome.Class != WorkerAdmitClassContractViolation || outcome.Reason != WorkerAdmitReasonMalformedResponse {
			t.Fatalf("outcome=%+v", outcome)
		}
	})

	t.Run("a daemon that hangs up before answering is retriable", func(t *testing.T) {
		socket := filepath.Join(t.TempDir(), "daemon.sock")
		listener, err := net.Listen("unix", socket)
		if err != nil {
			t.Fatalf("listen: %v", err)
		}
		t.Cleanup(func() { _ = listener.Close() })
		go func() {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			var request runnerAdmitRequestFrame
			_ = readRunnerAdmitFrame(conn, &request)
			_ = conn.Close()
		}()
		outcome := RequestWorkerAdmit(context.Background(), workerAdmitTestRequest(socket))
		if outcome.Class != WorkerAdmitClassContended {
			t.Fatalf("outcome=%+v: a severed connection proves nothing about the daemon's existence", outcome)
		}
	})
}

// verifies: AIRA-42 — the outcome is total. Nothing in RequestWorkerAdmit can
// return a value outside the catalogues, which is what makes "unavailable"
// impossible to reach by fallthrough.
func TestRequestWorkerAdmitAlwaysReturnsACataloguedOutcome(t *testing.T) {
	socket := filepath.Join(t.TempDir(), "absent.sock")
	outcome := RequestWorkerAdmit(context.Background(), workerAdmitTestRequest(socket))
	if !IsWorkerAdmitState(outcome.State) || !IsWorkerAdmitClass(outcome.Class) {
		t.Fatalf("outcome=%+v is outside the catalogue", outcome)
	}
	if _, err := os.Stat(socket); !os.IsNotExist(err) {
		t.Fatalf("the fixture socket must not exist: %v", err)
	}
}
