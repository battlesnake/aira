package daemon

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"aira/internal/runner"
)

// verifies: S15/AIRA-42 — every DETERMINISTIC worker-admit verdict carries a
// catalogued (state, class, reason) triple that survives the wire. A blocking claim
// that merely does not fit right now BLOCKS (no frame until EOF), so the reachable
// terminal/immediate verdicts are the ones tabled here.
func TestWorkerAdmitClassifiesDeterministicOutcomes(t *testing.T) {
	tests := []struct {
		name       string
		configure  func(*Server) map[string]any
		wantState  string
		wantClass  string
		wantReason string
	}{
		{
			name: "slice read failure is retriable",
			configure: func(s *Server) map[string]any {
				s.admitReadMemory = func(string) (int64, int64, int64, bool, string) { return 0, 0, 0, false, "read-error" }
				return workerArgs("/slice/.aira-suite", workerTestMiB, false, 0)
			},
			wantState: runner.WorkerAdmitStateUnevaluated, wantClass: runner.WorkerAdmitClassContended,
			wantReason: runner.WorkerAdmitReasonOuterScopeUnreadable,
		},
		{
			name:      "a request larger than the whole ceiling is permanent",
			configure: func(s *Server) map[string]any { return workerArgs("/slice/.aira-suite", 5*workerTestMiB, false, 0) },
			wantState: runner.WorkerAdmitStateDenied, wantClass: runner.WorkerAdmitClassRequestInvalid,
			wantReason: runner.WorkerAdmitReasonExceedsCeiling,
		},
		{
			name: "a worker-scope creation failure is terminal, not a fallback",
			configure: func(s *Server) map[string]any {
				s.workerScopeCreate = func(context.Context, string, string, int64) (string, string, error) {
					return "", "", errWorkerCreateBoom
				}
				return workerArgs("/slice/.aira-suite", workerTestMiB, false, 0)
			},
			wantState: runner.WorkerAdmitStateDenied, wantClass: runner.WorkerAdmitClassRequestInvalid,
			wantReason: runner.WorkerAdmitReasonWorkerScopeCreateFailed,
		},
		{
			name:      "a non-blocking probe reports a snapshot",
			configure: func(s *Server) map[string]any { return workerArgs("/slice/.aira-suite", workerTestMiB, true, 0) },
			wantState: runner.WorkerAdmitStateDenied, wantClass: runner.WorkerAdmitClassContended,
			wantReason: runner.WorkerAdmitReasonSnapshot,
		},
		{
			name:      "a grant carries the granted class",
			configure: func(s *Server) map[string]any { return workerArgs("/slice/.aira-suite", workerTestMiB, false, 0) },
			wantState: runner.WorkerAdmitStateGranted, wantClass: runner.WorkerAdmitClassGranted,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			server := workerAdmitServer(t, "/slice", 4*workerTestMiB)
			args := test.configure(server)
			resp, client, done := startWorkerAdmit(t, server, args)
			defer client.Close()
			if resp.State != test.wantState || resp.Class != test.wantClass ||
				(test.wantReason != "" && resp.Reason != test.wantReason) {
				t.Fatalf("resp=%+v, want state=%s class=%s reason=%s", resp, test.wantState, test.wantClass, test.wantReason)
			}
			if !runner.IsWorkerAdmitState(resp.State) || !runner.IsWorkerAdmitClass(resp.Class) {
				t.Fatalf("resp=%+v is outside the shared catalogue", resp)
			}
			_ = done
		})
	}
}

// verifies: AIRA-42 — the wire response carries the class (not just the reason), and
// the old "reject:"/"fallback:" prose-prefix convention is gone.
func TestWorkerAdmitResponseCarriesClassOnTheWire(t *testing.T) {
	server := workerAdmitServer(t, "/slice", 4*workerTestMiB)
	resp, client, done := startWorkerAdmit(t, server, workerArgs("/slice/.aira-suite", 5*workerTestMiB, false, 0))
	defer client.Close()
	encoded, err := json.Marshal(resp)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if !strings.Contains(string(encoded), `"class":"`+runner.WorkerAdmitClassRequestInvalid+`"`) {
		t.Fatalf("marshalled response lost its class: %s", encoded)
	}
	if strings.Contains(string(encoded), "reject:") || strings.Contains(string(encoded), "fallback:") {
		t.Fatalf("the prose prefix convention survived into the wire shape: %s", encoded)
	}
	_ = done
}

// verifies: AIRA-45, AIRA-83(b) — the structural discriminator the worker-admit
// client relies on. The daemon's protocol-VERSION mismatch frame carries its own
// protocol version; its per-request argument rejection does not.
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
}
