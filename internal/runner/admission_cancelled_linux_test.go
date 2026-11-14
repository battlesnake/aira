//go:build linux

package runner

import (
	"bytes"
	"context"
	"errors"
	"net"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"aira/internal/codes"
)

// verifies: AIRA-328, client side. A queued job an operator cancelled with
// `aira confine --kill` is answered U_ADMIT_CANCELLED. The runner must treat it
// as a dedicated TERMINAL refusal: exactly ONE dial (it must never reconnect,
// which would re-queue the job the operator just cancelled), state "cancelled"
// for the trailer's admission= facet, and the code-prefixed error the exit map
// reads.
//
// Mutation: remove the U_ADMIT_CANCELLED block in admitExchangeOnce -> the code
// falls through to the anonymous refusal (state "refused", basis
// "reject:daemon-refused") and this test reds.
func TestAdmissionCancelledIsDistinctTerminalRefusal(t *testing.T) {
	runner := &Runner{
		memorySlice:      "/fake/finite.slice",
		memoryReserve:    DefaultConfineMemoryReserve,
		admissionMaxWait: time.Second,
		pollInterval:     time.Millisecond,
		clock:            newInstantClock(),
		sliceMemory:      func(string) (int64, int64, bool, string) { return 0, 64 << 30, true, "" },
	}
	var dials atomic.Int32
	runner.admitDialFn = func(context.Context, string) (net.Conn, error) {
		dials.Add(1)
		client, server := net.Pipe()
		go func() {
			defer server.Close()
			var frame runnerAdmitRequestFrame
			if err := readRunnerAdmitFrame(server, &frame); err != nil {
				return
			}
			_ = writeRunnerAdmitFrame(server, runnerAdmitResponseFrame{
				Code:  "U_ADMIT_CANCELLED",
				Error: "U_ADMIT_CANCELLED: this job was cancelled by `aira confine --kill` while it was still waiting for admission; nothing ran (basis=reject:cancelled)",
			})
		}()
		return client, nil
	}

	result, _, err := runner.admitThroughDaemon(context.Background(), Request{DaemonEstimateMemory: true}, DefaultConfineMemoryReserve)
	if err == nil {
		t.Fatalf("a cancel produced no error (result=%+v): it must be a terminal refusal", result)
	}
	if got := dials.Load(); got != 1 {
		t.Fatalf("dialled %d times, want exactly 1: a cancelled job must never reconnect into the queue", got)
	}
	if result.state != "cancelled" || result.basis != "reject:cancelled" {
		t.Fatalf("state=%q basis=%q, want cancelled/reject:cancelled", result.state, result.basis)
	}
	if confineErrorCode(err) != "U_ADMIT_CANCELLED" {
		t.Fatalf("error %q must lead with U_ADMIT_CANCELLED", err.Error())
	}
	// The never-ran trailer the operator and the scripts read.
	line := FormatConfineNeverRan(ConfineStatus{Slice: "aira.slice", Name: "job", AdmissionState: result.state}, err)
	if !strings.Contains(line, "ran=no code=U_ADMIT_CANCELLED") || !strings.HasSuffix(line, "admission=cancelled") {
		t.Fatalf("never-ran line %q, want ran=no code=U_ADMIT_CANCELLED ... admission=cancelled", line)
	}
	if got := codes.ExitForCode(confineErrorCode(err)); got != 3 {
		t.Fatalf("exit %d, want 3: a job deliberately not run has no verdict (U_, like U_CONFINE_DETACH_CANCELLED)", got)
	}
}

// verifies: AIRA-328 — the one launch funnel prints the never-ran line for a
// cancelled queued job, with the dedicated code and admission state, in place of
// the ordinary trailer.
func TestConfineFunnelPrintsTheCancelledNeverRanLine(t *testing.T) {
	status := ConfineStatus{Slice: "aira.slice", Name: "job", AdmissionState: "cancelled"}
	stubConfineImpl(t, ConfineResult{Status: status}, errors.New("U_ADMIT_CANCELLED: cancelled while queued"))
	var stderr bytes.Buffer
	if _, err := Confine(context.Background(), ConfineRequest{Argv: []string{"true"}, Stderr: &stderr}); confineErrorCode(err) != "U_ADMIT_CANCELLED" {
		t.Fatalf("err=%v", err)
	}
	if !strings.Contains(stderr.String(), "confine: ran=no code=U_ADMIT_CANCELLED slice=aira.slice name=job admission=cancelled") {
		t.Fatalf("stderr=%q lacks the never-ran line", stderr.String())
	}
}

// verifies: AIRA-328 review P1, client half of the lost-reply sequence. The
// first exchange loses its reply (the daemon side closes after reading the
// request: the client reads EOF), so the client reconnects; the reconnect
// carries the SAME scope id, which is what the daemon's cancelled-scope record
// keys on, and its U_ADMIT_CANCELLED answer ends the loop with no third dial.
// The daemon half (that the reconnect is answered U_ADMIT_CANCELLED rather
// than queued) is TestConfineKillQueuedCancelSurvivesALostReply. Mutation: mint
// the scope id per exchange -> the two dials carry different ids.
func TestAdmissionCancelAfterALostReplyEndsOnTheReconnect(t *testing.T) {
	runner := &Runner{
		memorySlice:      "/fake/finite.slice",
		memoryReserve:    DefaultConfineMemoryReserve,
		admissionMaxWait: time.Second,
		pollInterval:     time.Millisecond,
		clock:            newInstantClock(),
		sliceMemory:      func(string) (int64, int64, bool, string) { return 0, 64 << 30, true, "" },
	}
	const scopeID = "CONFINE-lost-7502-zz@s-1"
	var dials atomic.Int32
	scopes := make(chan any, 4)
	runner.admitDialFn = func(context.Context, string) (net.Conn, error) {
		n := dials.Add(1)
		client, server := net.Pipe()
		go func() {
			defer server.Close()
			var frame runnerAdmitRequestFrame
			if err := readRunnerAdmitFrame(server, &frame); err != nil {
				return
			}
			scopes <- frame.Request.Args["scope_id"]
			if n == 1 {
				return // the cancel reply is lost
			}
			_ = writeRunnerAdmitFrame(server, runnerAdmitResponseFrame{
				Code:  "U_ADMIT_CANCELLED",
				Error: "U_ADMIT_CANCELLED: this job was cancelled by `aira confine --kill` while it was still waiting for admission; nothing ran (basis=reject:cancelled)",
			})
		}()
		return client, nil
	}

	result, _, err := runner.admitThroughDaemon(context.Background(), Request{DaemonEstimateMemory: true, ConfineScopeID: scopeID}, DefaultConfineMemoryReserve)
	if err == nil || result.state != "cancelled" || confineErrorCode(err) != "U_ADMIT_CANCELLED" {
		t.Fatalf("state=%q err=%v, want a terminal cancelled refusal", result.state, err)
	}
	if got := dials.Load(); got != 2 {
		t.Fatalf("dialled %d times, want 2 (the lost exchange and the reconnect that learns of the cancel)", got)
	}
	for i := 0; i < 2; i++ {
		if got := <-scopes; got != scopeID {
			t.Fatalf("exchange %d carried scope_id %v, want %s on every reconnect", i+1, got, scopeID)
		}
	}
}
