//go:build linux

package daemon

import (
	"bytes"
	"context"
	"os/exec"
	"strconv"
	"strings"
	"testing"
	"time"

	"aira/internal/runner"
	"aira/internal/testdeadline"
)

// TestWorkerAdmitCLIOutcomeChannelMatchesTheSupervisorBoundary drives the real
// `aira worker-admit` binary against a real daemon and asserts the SINGLE
// structured stdout line the aitest supervisor parses, plus that stderr carries no
// classification. It covers the reachable DETERMINISTIC verdicts: under S15 a
// blocking claim no longer self-expires (no timeout verdict), and a zero max-wait is
// a non-blocking snapshot.
//
// verifies: AIRA-42, S15
func TestWorkerAdmitCLIOutcomeChannelMatchesTheSupervisorBoundary(t *testing.T) {
	binary := buildAiraBinary(t)

	paths := testPaths(t)
	server := NewServer(paths)
	server.restartFreeze = 0
	// The unified ledger's slice resolves deterministically; the ceiling is 1 MiB so
	// a 2 MiB request exceeds it. No subtest below reaches a scope create.
	server.admitSliceHeadroomBase = 0
	server.admitSliceHeadroomSupervisor = 0
	server.admitResolveSlice = func(string) (string, bool, string) { return "/test-slice", true, "" }
	server.admitReadMemory = func(string) (int64, int64, int64, bool, string) {
		return 0, workerAdmitEstimatedBytesMin, 0, true, ""
	}
	ready := make(chan struct{}, 1)
	server.Ready = ready
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- server.Serve(ctx) }()
	select {
	case <-ready:
	case err := <-done:
		t.Fatalf("daemon exited before ready: %v", err)
	case <-testdeadline.After(5 * time.Second):
		t.Fatal("daemon did not become ready")
	}
	t.Cleanup(func() {
		cancel()
		if err := <-done; err != nil {
			t.Error(err)
		}
	})

	runWorkerAdmit := func(outerScope string, estimatedBytes int64, maxWait string) (string, string) {
		command := exec.Command(binary, "worker-admit", "--job-id", "job-1", "--outer-scope", outerScope,
			"--estimated-bytes", strconv.FormatInt(estimatedBytes, 10), "--max-wait", maxWait)
		command.Stdin = strings.NewReader("")
		var stdout, stderr bytes.Buffer
		command.Stdout = &stdout
		command.Stderr = &stderr
		_ = command.Run()
		return stdout.String(), stderr.String()
	}

	assertOutcome := func(t *testing.T, stdout, stderr, wantState, wantClass, wantReason string, wantStderrDiagnostic bool) {
		t.Helper()
		lines := strings.Split(strings.TrimSpace(stdout), "\n")
		if len(lines) != 1 {
			t.Fatalf("worker-admit must write exactly one stdout line, got %d:\n%s", len(lines), stdout)
		}
		fields, err := runner.ParseWorkerAdmitOutcomeLine(lines[0])
		if err != nil {
			t.Fatalf("parse %q: %v", lines[0], err)
		}
		if fields["state"] != wantState || fields["class"] != wantClass || fields["reason"] != wantReason {
			t.Fatalf("outcome=%v, want state=%s class=%s reason=%s", fields, wantState, wantClass, wantReason)
		}
		if wantStderrDiagnostic && strings.TrimSpace(stderr) == "" {
			t.Fatal("a declined worker-admit must still leave a human diagnostic on stderr")
		}
	}

	t.Run("permanent rejection (exceeds ceiling)", func(t *testing.T) {
		stdout, stderr := runWorkerAdmit("/slice/.aira-suite", 2*workerAdmitEstimatedBytesMin, "5s")
		assertOutcome(t, stdout, stderr,
			runner.WorkerAdmitStateDenied, runner.WorkerAdmitClassRequestInvalid,
			runner.WorkerAdmitReasonExceedsCeiling, true)
	})

	// A zero max-wait is a non-blocking PROBE (S15): it reports a snapshot and reserves
	// nothing, never a timeout. The daemon has always accepted zero; before AIRA-64
	// only the CLI refused it.
	t.Run("a zero max-wait is a non-blocking snapshot", func(t *testing.T) {
		stdout, stderr := runWorkerAdmit("/slice/.aira-suite", workerAdmitEstimatedBytesMin, "0")
		assertOutcome(t, stdout, stderr,
			runner.WorkerAdmitStateDenied, runner.WorkerAdmitClassContended,
			runner.WorkerAdmitReasonSnapshot, true)
	})

	t.Run("a negative max-wait is refused client-side", func(t *testing.T) {
		stdout, stderr := runWorkerAdmit("/slice/.aira-suite", workerAdmitEstimatedBytesMin, "-1s")
		assertOutcome(t, stdout, stderr,
			runner.WorkerAdmitStateArgumentInvalid, runner.WorkerAdmitClassRequestInvalid,
			runner.WorkerAdmitReasonMaxWaitInvalid, true)
	})

	t.Run("a client estimated-bytes mistake never reaches the daemon", func(t *testing.T) {
		stdout, stderr := runWorkerAdmit("/slice/.aira-suite", 1024, "5s")
		assertOutcome(t, stdout, stderr,
			runner.WorkerAdmitStateArgumentInvalid, runner.WorkerAdmitClassRequestInvalid,
			runner.WorkerAdmitReasonEstimatedBytesOutOfRange, true)
	})

	t.Run("a pre-dispatch argument error still speaks the channel", func(t *testing.T) {
		command := exec.Command(binary, "worker-admit", "--job-id", "job-1", "--not-an-option", "x")
		command.Stdin = strings.NewReader("")
		var stdout, stderr bytes.Buffer
		command.Stdout = &stdout
		command.Stderr = &stderr
		_ = command.Run()
		assertOutcome(t, stdout.String(), stderr.String(),
			runner.WorkerAdmitStateArgumentInvalid, runner.WorkerAdmitClassRequestInvalid,
			runner.WorkerAdmitReasonArgumentsInvalid, true)
	})

	// AIRA-63, proven through the REAL client: an admitSlots-saturated worker-admit
	// must reach supervisor.py as a RETRIABLE denial, not an error frame (which would
	// arrive as a terminal contract violation). Kept LAST and not parallel: it drains
	// the shared admitSlots semaphore.
	t.Run("slot saturation is a retriable denial, not an error frame", func(t *testing.T) {
		for i := 0; i < admitGlobalMax; i++ {
			server.admitSlots <- struct{}{}
		}
		stdout, stderr := runWorkerAdmit("/slice/.aira-suite", workerAdmitEstimatedBytesMin, "5s")
		for i := 0; i < admitGlobalMax; i++ {
			<-server.admitSlots
		}
		assertOutcome(t, stdout, stderr,
			runner.WorkerAdmitStateDenied, runner.WorkerAdmitClassContended,
			runner.WorkerAdmitReasonAdmitSlotsSaturated, true)
	})
}
