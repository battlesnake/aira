//go:build linux

package runner_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
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

// TestNoProductionCodeWritesALiteralProtocolVersion closes the hole the pin
// above does not: a wire frame built with a hand-written NUMBER rather than the
// constant. internal/runner/governor_slot.go carried `Proto: 5` for exactly that
// reason and would have silently failed protocol negotiation the moment
// ProtocolVersion moved (found while bumping it for AIRA-39). A grep test needs
// no new tooling and runs under `go test`, per this repo's rule against gates
// that can skip silently.
//
// verifies: AIRA-83, AIRA-39
func TestNoProductionCodeWritesALiteralProtocolVersion(t *testing.T) {
	root, err := exec.Command("git", "rev-parse", "--show-toplevel").Output()
	if err != nil {
		t.Skipf("git rev-parse unavailable: %v", err)
	}
	repo := strings.TrimSpace(string(root))
	// --full-name so the listing is repo-relative rather than relative to this
	// package directory; the pattern is repo-rooted for the same reason.
	output, err := exec.Command("git", "ls-files", "--full-name", "--", ":(top)*.go").Output()
	if err != nil {
		t.Skipf("git ls-files unavailable: %v", err)
	}
	files := strings.Fields(string(output))
	if len(files) < 50 {
		t.Fatalf("git ls-files returned only %d Go files; this check would be vacuous", len(files))
	}
	literal := regexp.MustCompile(`Proto:\s*[0-9]`)
	for _, path := range files {
		if strings.HasSuffix(path, "_test.go") {
			// Tests legitimately fabricate OLD protocol numbers to exercise
			// mismatch handling.
			continue
		}
		data, err := os.ReadFile(filepath.Join(repo, path))
		if err != nil {
			continue
		}
		for index, line := range strings.Split(string(data), "\n") {
			if literal.MatchString(line) {
				t.Errorf("%s:%d writes a literal protocol version; use daemon.ProtocolVersion or runner.DaemonProtocolVersion so a bump cannot leave it behind:\n\t%s",
					path, index+1, strings.TrimSpace(line))
			}
		}
	}
}
