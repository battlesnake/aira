//go:build linux

package daemon

import (
	"bytes"
	"context"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"aira/internal/runner"
)

// TestWorkerAdmitCLIOutcomeChannelMatchesTheSupervisorBoundary drives the real
// `aira worker-admit` binary against a real daemon and asserts the SINGLE
// structured stdout line the aitest supervisor parses.
//
// This test used to assert stderr prose ("worker-admit denied" plus
// "reject:exceeds-ceiling"), which was the boundary contract before AIRA-42:
// two channels, with the load-bearing classification carried as a substring of
// a human sentence. It now asserts what the supervisor actually consumes — one
// line, exact enum values — and that stderr carries no classification at all.
//
// verifies: AIRA-42
func TestWorkerAdmitCLIOutcomeChannelMatchesTheSupervisorBoundary(t *testing.T) {
	binary := filepath.Join(t.TempDir(), "aira")
	build := exec.Command("go", "build", "-o", binary, "aira/cmd/aira")
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build aira binary: %v\n%s", err, output)
	}

	paths := testPaths(t)
	server := NewServer(paths)
	server.workerAdmitHeadroom = 0
	server.workerAdmitPollInterval = time.Millisecond
	server.admitReadMemory = func(scope string) (int64, int64, int64, bool, string) {
		switch scope {
		case "/deny-ceiling":
			// 2 MiB requested bytes exceed this 1 MiB ceiling even at
			// zero usage, so workerAdmitConnection returns the permanent
			// request-invalid denial without polling.
			return 0, workerAdmitEstimatedBytesMin, 0, true, ""
		case "/deny-timeout":
			// Full live occupancy leaves this otherwise valid request with
			// no headroom on every poll, forcing its own max-wait timeout.
			return 2 * workerAdmitEstimatedBytesMin, 2 * workerAdmitEstimatedBytesMin, 0, true, ""
		case "/unbounded":
			return 0, 0, 0, false, "unbounded"
		default:
			return 0, 0, 0, false, "unexpected scope in test fixture"
		}
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
	case <-time.After(5 * time.Second):
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
		_ = command.Run() // denied and timeout are both expected nonzero exits.
		return stdout.String(), stderr.String()
	}

	assertOutcome := func(t *testing.T, stdout, stderr, wantState, wantClass, wantReason string) {
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
		// stderr is a human diagnostic. Nothing may need to read it, and in
		// particular the classification must not depend on it.
		if strings.TrimSpace(stderr) == "" {
			t.Fatal("a declined worker-admit must still leave a human diagnostic on stderr")
		}
	}

	t.Run("permanent rejection", func(t *testing.T) {
		stdout, stderr := runWorkerAdmit("/deny-ceiling", 2*workerAdmitEstimatedBytesMin, "5s")
		assertOutcome(t, stdout, stderr,
			runner.WorkerAdmitStateDenied, runner.WorkerAdmitClassRequestInvalid,
			runner.WorkerAdmitReasonExceedsCeiling)
	})

	t.Run("timeout is contended, never a permanent verdict", func(t *testing.T) {
		started := time.Now()
		stdout, stderr := runWorkerAdmit("/deny-timeout", workerAdmitEstimatedBytesMin, "50ms")
		if elapsed := time.Since(started); elapsed > 5*time.Second {
			t.Fatalf("timeout worker-admit took %v — looks like it ignored max-wait", elapsed)
		}
		assertOutcome(t, stdout, stderr,
			runner.WorkerAdmitStateTimeout, runner.WorkerAdmitClassContended,
			runner.WorkerAdmitReasonSaturated)
	})

	t.Run("an unbounded outer scope is the one structural unevaluated", func(t *testing.T) {
		stdout, stderr := runWorkerAdmit("/unbounded", workerAdmitEstimatedBytesMin, "50ms")
		assertOutcome(t, stdout, stderr,
			runner.WorkerAdmitStateUnevaluated, runner.WorkerAdmitClassAdmissionUnusable,
			runner.WorkerAdmitReasonOuterScopeUnbounded)
	})

	t.Run("a transient unevaluated read stays retriable", func(t *testing.T) {
		stdout, stderr := runWorkerAdmit("/no-such-fixture", workerAdmitEstimatedBytesMin, "50ms")
		assertOutcome(t, stdout, stderr,
			runner.WorkerAdmitStateUnevaluated, runner.WorkerAdmitClassContended,
			runner.WorkerAdmitReasonOuterScopeUnreadable)
	})

	t.Run("a client argument mistake never reaches the daemon", func(t *testing.T) {
		// The floor rejection happens pre-dial. Before AIRA-42 it produced
		// only a rendered stderr error, which the supervisor could read
		// only as "the relay produced nothing" -> run unconfined.
		stdout, stderr := runWorkerAdmit("/deny-ceiling", 1024, "5s")
		assertOutcome(t, stdout, stderr,
			runner.WorkerAdmitStateArgumentInvalid, runner.WorkerAdmitClassRequestInvalid,
			runner.WorkerAdmitReasonEstimatedBytesOutOfRange)
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
			runner.WorkerAdmitReasonArgumentsInvalid)
	})
}
