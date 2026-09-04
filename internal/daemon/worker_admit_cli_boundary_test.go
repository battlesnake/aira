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
)

func TestWorkerAdmitCLIStderrClassificationMatchesSupervisorBoundary(t *testing.T) {
	binary := filepath.Join(t.TempDir(), "aira")
	build := exec.Command("go", "build", "-o", binary, "aira/cmd/aira")
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build aira binary: %v\n%s", err, output)
	}

	paths := testPaths(t)
	server := NewServer(paths)
	// AIRA-39: the ledger sums the outer scope's real `.aira-worker-*` children
	// and the daemon creates the granted scope itself. The fixture scopes below
	// are not real cgroups, so both seams are stubbed; neither denial path in
	// this test reaches a create.
	_ = newWorkerScopeTree().install(server)
	server.workerAdmitHeadroom = 0
	server.workerAdmitPollInterval = time.Millisecond
	server.admitReadMemory = func(scope string) (int64, int64, int64, bool, string) {
		switch scope {
		case "/deny-ceiling":
			// 2 MiB requested bytes exceed this 1 MiB ceiling even at
			// zero usage, so workerAdmitConnection returns the permanent
			// reject:exceeds-ceiling denial without polling.
			return 0, workerAdmitEstimatedBytesMin, 0, true, ""
		case "/deny-timeout":
			// Full live occupancy leaves this otherwise valid request with
			// no headroom on every poll, forcing its own max-wait timeout.
			return 2 * workerAdmitEstimatedBytesMin, 2 * workerAdmitEstimatedBytesMin, 0, true, ""
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

	_, deniedStderr := runWorkerAdmit("/deny-ceiling", 2*workerAdmitEstimatedBytesMin, "5s")
	if !strings.Contains(deniedStderr, "worker-admit denied") || !strings.Contains(deniedStderr, "reject:exceeds-ceiling") {
		t.Fatalf("permanent denial stderr missing supervisor classifier text:\n%s", deniedStderr)
	}

	started := time.Now()
	_, timeoutStderr := runWorkerAdmit("/deny-timeout", workerAdmitEstimatedBytesMin, "50ms")
	if elapsed := time.Since(started); elapsed > 5*time.Second {
		t.Fatalf("timeout worker-admit took %v — looks like it ignored max-wait", elapsed)
	}
	if !strings.Contains(timeoutStderr, "worker-admit timeout") {
		t.Fatalf("timeout stderr missing supervisor classifier text:\n%s", timeoutStderr)
	}
	if strings.Contains(timeoutStderr, "reject:exceeds-ceiling") {
		t.Fatalf("timeout unexpectedly used the permanent sizing rejection:\n%s", timeoutStderr)
	}

	// AIRA-63, proven through the REAL client rather than the in-process
	// response struct: an admitSlots-saturated worker-admit must reach
	// supervisor.py as a RETRIABLE denial. Delivered as an error frame it would
	// arrive as "worker-admit request rejected", which matches none of the
	// classifier's denial substrings, falls through to WorkerAdmitUnavailable,
	// and makes _disable_daemon run the whole suite UNCONFINED.
	for i := 0; i < admitGlobalMax; i++ {
		server.admitSlots <- struct{}{}
	}
	_, saturatedStderr := runWorkerAdmit("/deny-ceiling", workerAdmitEstimatedBytesMin, "5s")
	for i := 0; i < admitGlobalMax; i++ {
		<-server.admitSlots
	}
	if !strings.Contains(saturatedStderr, "worker-admit denied") {
		t.Fatalf("saturation stderr does not match the supervisor's denial classifier at all:\n%s", saturatedStderr)
	}
	if !strings.Contains(saturatedStderr, "fallback:admit-slots-saturated") {
		t.Fatalf("saturation stderr missing the slot-saturation reason:\n%s", saturatedStderr)
	}
	if strings.Contains(saturatedStderr, "reject:") {
		t.Fatalf("saturation stderr is reject:-prefixed, so the supervisor marks the queue unevaluated instead of retrying once a slot frees:\n%s", saturatedStderr)
	}
	if strings.Contains(saturatedStderr, "request rejected") {
		t.Fatalf("saturation was delivered as an error frame; supervisor.py classifies that as unavailable and answers by running the suite unconfined:\n%s", saturatedStderr)
	}
}
