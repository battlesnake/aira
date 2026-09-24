//go:build linux

package runner

import (
	"context"
	"net"
	"strings"
	"testing"
	"time"
)

// AIRA-247, client side. A daemon that answers E_ADMIT_FAILFAST_TRIPPED must be
// handled by the dedicated terminal-refusal block, NOT swept into the anonymous
// post-S13 daemon-refused fallthrough: the CI classifier reads the distinct
// state/basis to tell a leg refused because a sibling failed from one refused for
// capacity, and the code-prefixed message drives the exit-status map to bucket 1.
//
// Mutation: remove the E_ADMIT_FAILFAST_TRIPPED block in admitThroughDaemon → the
// code falls through to the generic refusal (state "refused", basis
// "reject:daemon-refused") and this test reds.
func TestAdmissionFailfastTrippedIsDistinctTerminalRefusal(t *testing.T) {
	runner := &Runner{
		memorySlice:      "/fake/finite.slice",
		memoryReserve:    DefaultConfineMemoryReserve,
		admissionMaxWait: time.Second,
		pollInterval:     time.Millisecond,
		clock:            newInstantClock(),
		sliceMemory:      func(string) (int64, int64, bool, string) { return 0, 64 << 30, true, "" },
	}
	client, server := net.Pipe()
	runner.admitDialFn = func(context.Context, string) (net.Conn, error) { return client, nil }
	go func() {
		defer server.Close()
		var frame runnerAdmitRequestFrame
		if err := readRunnerAdmitFrame(server, &frame); err != nil {
			return
		}
		_ = writeRunnerAdmitFrame(server, runnerAdmitResponseFrame{
			Code:  "E_ADMIT_FAILFAST_TRIPPED",
			Error: "E_ADMIT_FAILFAST_TRIPPED: a --fail-fast task in this slice failed; the CI gate is aborting, so no further jobs are admitted (basis=reject:failfast)",
		})
	}()

	result, _, err := runner.admitThroughDaemon(context.Background(), Request{DaemonEstimateMemory: true}, DefaultConfineMemoryReserve)
	if err == nil {
		t.Fatalf("a fail-fast trip produced no error (result=%+v) — it must be a terminal refusal", result)
	}
	if result.state != "failfast_tripped" {
		t.Fatalf("state=%q, want failfast_tripped (this becomes the trailer's admission= facet)", result.state)
	}
	if result.basis != "reject:failfast" {
		t.Fatalf("basis=%q, want reject:failfast (a trip must not borrow the saturated basis)", result.basis)
	}
	if !strings.Contains(err.Error(), "E_ADMIT_FAILFAST_TRIPPED") {
		t.Fatalf("error %q must carry the code so confineErrorCode extracts it (exit 1)", err.Error())
	}
}
