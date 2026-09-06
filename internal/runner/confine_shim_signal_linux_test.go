//go:build linux

package runner

import (
	"context"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

// shimSignalScript writes a job that forks a GRANDCHILD and then parks.
//
// The grandchild's SIGTERM handler deliberately takes MEASURABLE TIME before it
// writes its marker (AIRA-121 gate condition C9). That is what makes the test
// non-porous against the specific bug the gate names: the real path's onSignal
// kills the scope BEFORE the received signal is forwarded, so a shim
// implementation copying that shape would SIGKILL the group first and the
// handler would never complete. A handler that returned instantly would pass
// either way.
func shimSignalScript(t *testing.T, dir string, detach, ignore bool) string {
	t.Helper()
	// Two FILES rather than one nested `sh -c '...'` string: the grandchild's
	// handler itself contains quotes, and nesting them inside a -c argument is
	// how this test first failed to launch its grandchild at all.
	grandchildTrap := `trap 'sleep 0.3; : > "` + dir + `/grandchild-term"; exit 0' TERM`
	parentTrap := `trap 'exit 0' TERM`
	if ignore {
		grandchildTrap, parentTrap = `trap "" TERM`, `trap "" TERM`
	}
	grandchild := filepath.Join(dir, "grandchild.sh")
	if err := os.WriteFile(grandchild, []byte("#!/bin/sh\n"+grandchildTrap+"\n"+
		`: > "`+dir+`/grandchild-ready"`+"\nwhile :; do sleep 0.05; done\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	spawn := grandchild + " &"
	if detach {
		spawn = "setsid " + grandchild + " &"
	}
	parent := filepath.Join(dir, "job.sh")
	if err := os.WriteFile(parent, []byte("#!/bin/sh\n"+spawn+"\n"+parentTrap+"\n"+
		`: > "`+dir+`/parent-ready"`+"\nwhile :; do sleep 0.05; done\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	return parent
}

// runShimSignalJob launches a shim confine job, waits for its grandchild to be
// ready, then delivers `sig` through the SAME injected signal source the
// production handler consumes.
func runShimSignalJob(t *testing.T, dir, script string, sig os.Signal) (ConfineResult, error) {
	t.Helper()
	events := make(chan os.Signal, 1)
	deps := shimUnitDeps()
	deps.signalSource = func() (<-chan os.Signal, func()) { return events, func() {} }
	go func() {
		// The signal is sent even when readiness never arrives, so a broken job
		// script fails this test with a clear assertion instead of parking it in
		// an unkillable loop until the package timeout.
		if !waitForFile(t, filepath.Join(dir, "grandchild-ready"), 10*time.Second) {
			t.Errorf("grandchild never reported ready")
		}
		events <- sig
	}()
	return confineWithDeps(context.Background(), ConfineRequest{
		Argv: []string{script}, SelfPath: os.Args[0], Stderr: io.Discard, Stdout: io.Discard,
	}, deps)
}

// verifies: AIRA-121 requirement 8, ticket test (g), gate condition C9
//
// Counterexample: today's real path signals the DIRECT CHILD only
// (`child.Signal(received)`), which the grandchild never sees. This test fails
// against that, and against a SIGKILL-first teardown.
func TestShimConfineSignalReachesAGrandchildInTheProcessGroup(t *testing.T) {
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("no /bin/sh")
	}
	dir := t.TempDir()
	script := shimSignalScript(t, dir, false, false)
	result, err := runShimSignalJob(t, dir, script, syscall.SIGTERM)
	if err != nil {
		t.Fatalf("shim confine err=%v result=%+v", err, result)
	}
	if !waitForFile(t, filepath.Join(dir, "grandchild-term"), 10*time.Second) {
		t.Fatal("the grandchild never observed the forwarded SIGTERM: a process-GROUP signal is what reaches it, and its handler must have time to run before any escalation")
	}
}

// verifies: AIRA-121 requirement 8, ticket test (g) negative case
//
// THE DOCUMENTED EXCEPTION, asserted rather than papered over. A descendant that
// setsid()s has left the process group and is unreachable by kill(-pgid, ...).
// No non-cgroup mechanism reaches it, so shim mode does NOT have parity with
// cgroup.kill, and this test is what keeps that claim honest in both directions.
func TestShimConfineSignalDoesNotReachASetsidDescendant(t *testing.T) {
	if _, err := exec.LookPath("setsid"); err != nil {
		t.Skip("setsid(1) is unavailable, so the documented escape cannot be exercised here")
	}
	dir := t.TempDir()
	script := shimSignalScript(t, dir, true, false)
	if _, err := runShimSignalJob(t, dir, script, syscall.SIGTERM); err != nil {
		t.Fatalf("shim confine err=%v", err)
	}
	// Give the escape at least as long as the reachable case needed to prove
	// itself, so "not reached" is a real observation rather than an early read.
	if waitForFile(t, filepath.Join(dir, "grandchild-term"), 2*time.Second) {
		t.Fatal("a setsid'd descendant received the group signal; if that is genuinely now possible, the documented parity gap in SKILL.md and confine_shim_linux.go must be corrected too")
	}
	// Leave nothing behind: the escaped grandchild is, by the very property under
	// test, not reachable through the group.
	_ = exec.Command("pkill", "-f", filepath.Join(dir, "job.sh")).Run()
}

// verifies: AIRA-121 requirement 8
//
// The bounded escalation: a group that IGNORES SIGTERM is SIGKILLed after the
// grace. Without it a shim job that traps and ignores the signal would never
// die, and a Batch preemption would hang until the platform's own harder kill.
func TestShimConfineEscalatesToSIGKILLAfterTheGrace(t *testing.T) {
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("no /bin/sh")
	}
	dir := t.TempDir()
	script := shimSignalScript(t, dir, false, true)
	start := time.Now()
	result, err := runShimSignalJob(t, dir, script, syscall.SIGTERM)
	if err != nil {
		t.Fatalf("shim confine err=%v", err)
	}
	if result.Exit != 128+int(syscall.SIGKILL) {
		t.Fatalf("exit=%d, want %d (SIGKILL escalation)", result.Exit, 128+int(syscall.SIGKILL))
	}
	if elapsed := time.Since(start); elapsed < shimTeardownGrace {
		t.Fatalf("the job died after %s, before the %s grace elapsed: the grace must never be skipped, or a job that WOULD have shut down gracefully is killed anyway",
			elapsed, shimTeardownGrace)
	}
	if result.Status.TerminatedBy != ConfineTerminatedSupervisorSignalPrefix+"SIGTERM" {
		t.Fatalf("terminated-by=%q, want the supervisor-signal attribution", result.Status.TerminatedBy)
	}
}
