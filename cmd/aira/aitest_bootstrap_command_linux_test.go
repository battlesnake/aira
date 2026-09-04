//go:build linux

package main

import (
	"bytes"
	"context"
	"os"
	"strings"
	"testing"

	"aira/internal/runner"
)

// The aitest bootstrap verb's OUTER-SCOPE SELECTION, tested at the CLI seam
// rather than only at the runner seam (build-review, Sol: the runner-level
// idempotency test passes the correct outer directly, so it cannot notice the
// env preference being removed).
//
// Both cases use pid 1 as the supervisor pid. It is always alive and never a
// member of this test's cgroup, so BootstrapAitestSupervisor's membership guard
// — which runs before anything is moved — always refuses, and the refusal names
// the path that was selected. Nothing is mutated: this is a pure read path, and
// deliberately so, because the alternative (a pid that IS a member) would drain
// the test binary's own cgroup.
//
// verifies: AIRA-44
func TestAitestBootstrapPrefersTheLauncherPublishedOuterScope(t *testing.T) {
	const published = "/sys/fs/cgroup/aira-44-published-outer-scope-probe"
	t.Setenv("AIRA_AITEST_OUTER_SCOPE", published)
	var stdout, stderr bytes.Buffer
	if exit := runAitestBootstrapCommand(context.Background(), map[string]string{"supervisor-pid": "1"}, &stdout, &stderr); exit == 0 {
		t.Fatalf("expected refusal, got exit 0; stdout=%q", stdout.String())
	}
	if !strings.Contains(stderr.String(), published) {
		t.Fatalf("stderr=%q does not name the published outer scope %q — the verb self-discovered instead", stderr.String(), published)
	}
}

func TestAitestBootstrapFallsBackToSelfDiscoveryWhenUnpublished(t *testing.T) {
	t.Setenv("AIRA_AITEST_OUTER_SCOPE", "")
	current, err := runner.CurrentCgroupPath()
	if err != nil {
		t.Skipf("unevaluated: cannot read this process's own cgroup: %v", err)
	}
	var stdout, stderr bytes.Buffer
	if exit := runAitestBootstrapCommand(context.Background(), map[string]string{"supervisor-pid": "1"}, &stdout, &stderr); exit == 0 {
		t.Fatalf("expected refusal, got exit 0; stdout=%q", stdout.String())
	}
	if !strings.Contains(stderr.String(), current) {
		t.Fatalf("stderr=%q does not name the self-discovered cgroup %q", stderr.String(), current)
	}
}

// A relative coordinate is refused outright rather than resolved against
// whatever working directory pytest happened to have, which would mutate one
// cgroup and report an `outer=` the daemon resolves elsewhere.
func TestAitestBootstrapRefusesARelativeOuterScope(t *testing.T) {
	t.Setenv("AIRA_AITEST_OUTER_SCOPE", "relative/outer")
	var stdout, stderr bytes.Buffer
	exit := runAitestBootstrapCommand(context.Background(), map[string]string{"supervisor-pid": "1"}, &stdout, &stderr)
	if exit == 0 || !strings.Contains(stderr.String(), "must be an absolute cgroup path") {
		t.Fatalf("exit=%d stderr=%q", exit, stderr.String())
	}
	if _, err := os.Stat("relative"); err == nil {
		t.Fatal("a relative coordinate was resolved against the working directory")
	}
}
