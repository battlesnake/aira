//go:build linux

package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
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

// verifies: AIRA-123
//
// In ci-shim mode the bootstrap verb SUCCEEDS with the ledger-only grade instead
// of failing, and names no cgroup of any kind.
//
// AIRA-121 made this branch exit non-zero, which supervisor.py answers by
// calling _disable_daemon and running the whole suite on its ungoverned
// bare-fork pool. That is the behaviour this ticket replaces, and this test
// fails against it on the exit code alone.
//
// The three absences are the honesty half: no `supervisor_scope` (there is no
// such cgroup, and emitting one would have _cleanup_supervisor_scope rmdir an
// invented path), an `outer=` that is the sentinel rather than any path, and
// `admission=ledger-only` rather than silence — which the supervisor requires,
// since a bootstrap that states no grade is refused as out of lockstep.
func TestAitestBootstrapInShimModeReportsLedgerOnlyAdmission(t *testing.T) {
	record := filepath.Join(t.TempDir(), "install-mode.json")
	if err := runner.WriteInstallModeRecord(record, runner.InstallModeRecord{
		Schema: 1, Mode: runner.ConfineModeShim, UID: os.Getuid(),
		ShimBudgetBytes: 4 << 30, ShimBudgetSource: runner.ShimBudgetSourceDeclared,
	}); err != nil {
		t.Fatal(err)
	}
	t.Setenv(runner.InstallModeFileEnv, record)
	runner.ResetConfineModeCacheForTest()
	t.Cleanup(runner.ResetConfineModeCacheForTest)

	var stdout, stderr bytes.Buffer
	if exit := runAitestBootstrapCommand(context.Background(), map[string]string{"supervisor-pid": "1"}, &stdout, &stderr); exit != 0 {
		t.Fatalf("exit=%d stderr=%q; ci-shim bootstrap must SUCCEED, because ledger-only worker admission functions here — failing drops the whole suite to the ungoverned fallback pool",
			exit, stderr.String())
	}
	fields := map[string]string{}
	for _, token := range strings.Fields(stdout.String()) {
		key, value, ok := strings.Cut(token, "=")
		if ok {
			fields[key] = value
		}
	}
	if fields["outer"] != runner.ShimConfineSlice {
		t.Fatalf("outer=%q, want the ci-shim sentinel %q; anything path-shaped would be read as a cgroup", fields["outer"], runner.ShimConfineSlice)
	}
	if fields["admission"] != runner.AitestAdmissionLedgerOnly {
		t.Fatalf("admission=%q, want %q", fields["admission"], runner.AitestAdmissionLedgerOnly)
	}
	if scope, present := fields["supervisor_scope"]; present {
		t.Fatalf("supervisor_scope=%q; there is no supervisor cgroup in ci-shim mode and naming one would have the supervisor rmdir an invented path", scope)
	}
	if !strings.Contains(stderr.String(), "LEDGER-ONLY") {
		t.Fatalf("stderr=%q must say once that this run's governance is admission-only", stderr.String())
	}
}
