//go:build linux

package runner_test

import (
	"testing"

	"aira/internal/daemon"
	"aira/internal/runner"
)

// TestRunnerDaemonProtocolVersionMatchesTheDaemon pins the runner's hand-written
// admission protocol constant equal to the daemon's own (AIRA-83 item 3). The
// runner cannot import internal/daemon (daemon imports runner), so the constant
// cannot be derived; without this test a bump on one side alone makes the
// runner's admission path fail protocol negotiation against a correct daemon,
// with no build or test failure to catch it.
//
// verifies: AIRA-83
func TestRunnerDaemonProtocolVersionMatchesTheDaemon(t *testing.T) {
	if runner.DaemonProtocolVersion != daemon.ProtocolVersion {
		t.Fatalf("runner.DaemonProtocolVersion=%d daemon.ProtocolVersion=%d: bump both together",
			runner.DaemonProtocolVersion, daemon.ProtocolVersion)
	}
}
