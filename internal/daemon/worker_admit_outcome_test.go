package daemon

import (
	"bytes"
	"encoding/json"
	"io"
	"net"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"aira/internal/runner"
)

// verifies: AIRA-42 — every daemon-side verdict carries a catalogued
// (state, class, reason) triple. The table is the whole reachable surface of
// evaluateWorkerAdmit, so a new verdict added without a class fails here.
func TestEvaluateWorkerAdmitClassifiesEveryOutcome(t *testing.T) {
	tests := []struct {
		name       string
		configure  func(*Server)
		request    workerAdmitRequest
		wantState  string
		wantClass  string
		wantReason string
	}{
		{
			name: "outer scope read failure is retriable",
			configure: func(s *Server) {
				s.admitReadMemory = func(string) (int64, int64, int64, bool, string) {
					return 0, 0, 0, false, "read-error"
				}
			},
			request:   workerAdmitRequest{jobID: "j", outerScope: "/outer", estimatedBytes: 400},
			wantState: runner.WorkerAdmitStateUnevaluated, wantClass: runner.WorkerAdmitClassContended,
			wantReason: runner.WorkerAdmitReasonOuterScopeUnreadable,
		},
		{
			name: "outer scope parse failure is retriable",
			configure: func(s *Server) {
				s.admitReadMemory = func(string) (int64, int64, int64, bool, string) {
					return 0, 0, 0, false, "parse-error"
				}
			},
			request:   workerAdmitRequest{jobID: "j", outerScope: "/outer", estimatedBytes: 400},
			wantState: runner.WorkerAdmitStateUnevaluated, wantClass: runner.WorkerAdmitClassContended,
			wantReason: runner.WorkerAdmitReasonOuterScopeUnreadable,
		},
		{
			// The one structurally-permanent unevaluated: an outer scope
			// with no finite memory.max is not a real daemon-admitted
			// scope, and waiting can never make it one. Before AIRA-42 the
			// Python side told this apart from the two rows above by
			// looking for the word "unbounded" inside the sentence.
			name: "an unbounded outer scope is structural, not transient",
			configure: func(s *Server) {
				s.admitReadMemory = func(string) (int64, int64, int64, bool, string) {
					return 0, 0, 0, false, "unbounded"
				}
			},
			request:   workerAdmitRequest{jobID: "j", outerScope: "/outer", estimatedBytes: 400},
			wantState: runner.WorkerAdmitStateUnevaluated, wantClass: runner.WorkerAdmitClassAdmissionUnusable,
			wantReason: runner.WorkerAdmitReasonOuterScopeUnbounded,
		},
		{
			name: "a request larger than the whole ceiling is permanent",
			configure: func(s *Server) {
				s.admitReadMemory = admitReadMemoryFixture(map[string]int64{}, 1000)
			},
			request:   workerAdmitRequest{jobID: "j", outerScope: "/outer", estimatedBytes: 1001},
			wantState: runner.WorkerAdmitStateDenied, wantClass: runner.WorkerAdmitClassRequestInvalid,
			wantReason: runner.WorkerAdmitReasonExceedsCeiling,
		},
		{
			name: "no live headroom right now is retriable",
			configure: func(s *Server) {
				s.admitReadMemory = admitReadMemoryFixture(map[string]int64{"/outer": 950}, 1000)
			},
			request:   workerAdmitRequest{jobID: "j", outerScope: "/outer", estimatedBytes: 900},
			wantState: runner.WorkerAdmitStateDenied, wantClass: runner.WorkerAdmitClassContended,
			wantReason: runner.WorkerAdmitReasonInsufficientHeadroom,
		},
		{
			name: "an unreadable supervisor scope is retriable",
			configure: func(s *Server) {
				s.admitReadMemory = admitReadMemoryFixture(map[string]int64{}, 1000)
				s.admitReadWorkerSupervisorMemory = func(string) (int64, int64, bool, string) {
					return 0, 0, false, "read-error"
				}
			},
			request:   workerAdmitRequest{jobID: "j", outerScope: "/outer", estimatedBytes: 400},
			wantState: runner.WorkerAdmitStateUnevaluated, wantClass: runner.WorkerAdmitClassContended,
			wantReason: runner.WorkerAdmitReasonSupervisorScopeUnreadable,
		},
		{
			name: "a grant carries the granted class",
			configure: func(s *Server) {
				s.admitReadMemory = admitReadMemoryFixture(map[string]int64{}, 1000)
				s.admitReadWorkerSupervisorMemory = admitReadWorkerSupervisorMemoryFixture(map[string]int64{})
			},
			request:   workerAdmitRequest{jobID: "j", outerScope: "/outer", estimatedBytes: 400},
			wantState: runner.WorkerAdmitStateGranted, wantClass: runner.WorkerAdmitClassGranted,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			server := NewServer(Paths{})
			server.workerAdmitHeadroom = 0
			server.admitReadWorkerSupervisorMemory = admitReadWorkerSupervisorMemoryFixture(map[string]int64{})
			test.configure(server)
			response := server.evaluateWorkerAdmit(test.request)
			if response.State != test.wantState || response.Class != test.wantClass || response.Reason != test.wantReason {
				t.Fatalf("response=%+v, want state=%s class=%s reason=%s",
					response, test.wantState, test.wantClass, test.wantReason)
			}
			if !runner.IsWorkerAdmitState(response.State) || !runner.IsWorkerAdmitClass(response.Class) {
				t.Fatalf("response=%+v is outside the shared catalogue", response)
			}
		})
	}
}

// verifies: AIRA-42 — the poll loop breaks on the CLASS, not on how the reason
// happens to be spelled. This is the mutation-kill for the deleted
// `strings.HasPrefix(response.Reason, "reject:")`: a permanent verdict whose
// reason token does not read like a rejection must still break immediately,
// and a transient one whose token happens to read like a rejection must still
// poll.
func TestWorkerAdmitPollLoopBreaksOnClassNotOnReasonSpelling(t *testing.T) {
	tests := []struct {
		name          string
		outerMax      int64
		used          int64
		estimated     int64
		wantState     string
		wantClass     string
		wantImmediate bool
	}{
		{
			// request-invalid, and its reason token ("exceeds-ceiling") no
			// longer carries the old permanent-verdict prose prefix. A
			// mutant that still matched that prefix would poll this out to
			// a timeout instead of breaking on the first evaluation.
			name: "permanent class breaks immediately", outerMax: 4 << 20, used: 0, estimated: 5 << 20,
			wantState: runner.WorkerAdmitStateDenied, wantClass: runner.WorkerAdmitClassRequestInvalid,
			wantImmediate: true,
		},
		{
			// contended: must keep polling to its own deadline. A mutant
			// that broke on every denial would answer after one evaluation.
			name: "transient class keeps polling", outerMax: 4 << 20, used: 4<<20 - 1, estimated: 2 << 20,
			wantState: runner.WorkerAdmitStateTimeout, wantClass: runner.WorkerAdmitClassContended,
			wantImmediate: false,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			server := NewServer(Paths{})
			server.workerAdmitHeadroom = 0
			server.workerAdmitPollInterval = time.Millisecond
			var evaluations atomic.Int64
			server.admitReadMemory = func(string) (int64, int64, int64, bool, string) {
				evaluations.Add(1)
				return test.used, test.outerMax, 0, true, ""
			}
			server.admitReadWorkerSupervisorMemory = admitReadWorkerSupervisorMemoryFixture(map[string]int64{})

			client, daemonSide := net.Pipe()
			defer client.Close()
			var received bytes.Buffer
			drained := make(chan struct{})
			go func() { defer close(drained); _, _ = io.Copy(&received, client) }()
			done := make(chan struct{})
			go func() {
				defer close(done)
				defer daemonSide.Close()
				server.workerAdmitConnection(daemonSide, map[string]any{
					"job_id": "j", "outer_scope": "/outer",
					"estimated_bytes": float64(test.estimated), "max_wait_ms": float64(300),
				})
			}()
			select {
			case <-done:
			case <-time.After(10 * time.Second):
				t.Fatal("workerAdmitConnection never terminated")
			}
			<-drained
			if test.wantImmediate && evaluations.Load() != 1 {
				t.Fatalf("a permanent verdict evaluated %d times; it must break on the first one",
					evaluations.Load())
			}
			if !test.wantImmediate && evaluations.Load() < 2 {
				t.Fatalf("a transient verdict evaluated %d time(s); it must keep polling to its deadline",
					evaluations.Load())
			}
			// The evaluation count alone would not distinguish "polled twice
			// then broke early" from "polled to the deadline" (Sol
			// build-review). The wantState assertion below is what actually
			// pins that: only reaching the deadline produces state=timeout —
			// an early break returns the denial itself, state=denied. The
			// count is the complementary half, catching a break on the FIRST
			// evaluation, which would leave state=denied too but with one
			// evaluation rather than many. Deliberately a lower bound and not
			// a tight one: a wall-clock-tight assertion here is the AIRA-20
			// flake class.
			payload := received.String()
			if !strings.Contains(payload, `"state":"`+test.wantState+`"`) ||
				!strings.Contains(payload, `"class":"`+test.wantClass+`"`) {
				t.Fatalf("response payload=%q, want state=%s class=%s", payload, test.wantState, test.wantClass)
			}
		})
	}
}

// verifies: AIRA-45, AIRA-83(b) — the structural discriminator the worker-admit
// client relies on. The daemon's protocol-VERSION mismatch frame carries its
// own protocol version; its per-request argument rejection does not. Pinning
// both directions here is what makes the client's `proto != 0` test sound
// rather than incidental.
func TestProtocolMismatchFrameCarriesProtoAndArgumentRejectionDoesNot(t *testing.T) {
	mismatch := protocolMismatchFrame("E_DAEMON_PROTOCOL: daemon protocol is 6, client requested 5")
	if mismatch.Proto != ProtocolVersion {
		t.Fatalf("protocolMismatchFrame proto=%d, want %d", mismatch.Proto, ProtocolVersion)
	}
	if mismatch.Code != CodeProtocol {
		t.Fatalf("protocolMismatchFrame code=%q", mismatch.Code)
	}
	rejection := errorFrame(CodeProtocol, "E_DAEMON_PROTOCOL: worker-admit job_id is required")
	if rejection.Proto != 0 {
		t.Fatalf("an argument rejection must not carry a proto version, got %d — "+
			"the worker-admit client would then read it as a version skew", rejection.Proto)
	}
	if !strings.Contains(rejection.Error, "job_id") {
		t.Fatalf("rejection=%+v", rejection)
	}
	// The runner's own copy of the code must match, or the client's switch
	// silently stops recognising either case.
	if runnerProtocolCode := "E_DAEMON_PROTOCOL"; CodeProtocol != runnerProtocolCode {
		t.Fatalf("daemon.CodeProtocol=%q, runner's copy=%q", CodeProtocol, runnerProtocolCode)
	}
}

// verifies: AIRA-42 — the wire response really carries the class, so the
// client is not left re-deriving it from the reason after a JSON round trip.
func TestWorkerAdmitResponseCarriesClassOnTheWire(t *testing.T) {
	server := NewServer(Paths{})
	server.workerAdmitHeadroom = 0
	server.admitReadMemory = admitReadMemoryFixture(map[string]int64{}, 1000)
	response := server.evaluateWorkerAdmit(workerAdmitRequest{jobID: "j", outerScope: "/outer", estimatedBytes: 1001})
	encoded, err := json.Marshal(response)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if !strings.Contains(string(encoded), `"class":"`+runner.WorkerAdmitClassRequestInvalid+`"`) {
		t.Fatalf("marshalled response lost its class: %s", encoded)
	}
	if strings.Contains(string(encoded), "reject:") || strings.Contains(string(encoded), "fallback:") {
		t.Fatalf("the prose prefix convention survived into the wire shape: %s", encoded)
	}
}
