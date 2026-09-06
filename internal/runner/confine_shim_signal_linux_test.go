//go:build linux

package runner

import (
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
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
	// The pid is recorded BEFORE the ready marker, so a reader that has seen
	// readiness is guaranteed a complete pid file. It is what the setsid case
	// cleans up with: a descendant that has left the process group is, by the
	// property that test asserts, reachable by NOTHING except its pid.
	if err := os.WriteFile(grandchild, []byte("#!/bin/sh\n"+grandchildTrap+"\n"+
		`echo $$ > "`+dir+`/grandchild-pid"`+"\n"+
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
	// Leave nothing behind, and PROVE it. The escaped grandchild is by the very
	// property just asserted unreachable through the process group, so the group
	// teardown does not take it with it. The previous cleanup here, `pkill -f
	// <dir>/job.sh`, matched only the PARENT -- which the group signal had already
	// killed -- and left the grandchild's `while :; do sleep 0.05; done` running
	// forever: one leaked busy-looping process per `go test` run, invisible only
	// because an outer `aira confine` scope kill happens to sweep it up.
	//
	// So it is killed by the pid IT recorded, and the kill is then CONFIRMED. An
	// unverified best-effort kill is precisely how the leak went unnoticed, and
	// this assertion is what fails against that cleanup rather than merely
	// improving on it.
	pid := recordedGrandchildPID(t, dir)
	if err := syscall.Kill(pid, syscall.SIGKILL); err != nil && !errors.Is(err, syscall.ESRCH) {
		t.Fatalf("kill escaped grandchild %d: %v", pid, err)
	}
	if !waitProcessGone(pid, 10*time.Second) {
		t.Fatalf("the escaped grandchild (pid %d) is still running at the end of the test: it is out of reach of the process group, so it must be killed by pid or it leaks on every run", pid)
	}
}

// recordedGrandchildPID reads the pid the grandchild wrote before announcing
// readiness. Readiness has been observed by every caller, so an absent or
// unparsable file here is a broken test rather than a race.
func recordedGrandchildPID(t *testing.T, dir string) int {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(dir, "grandchild-pid"))
	if err != nil {
		t.Fatalf("read the grandchild's recorded pid: %v", err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil || pid <= 1 {
		t.Fatalf("the grandchild recorded %q, which is not a usable pid (%v)", string(data), err)
	}
	return pid
}

// waitProcessGone answers whether pid has left the process table within the
// budget. A ZOMBIE counts as gone: the process is dead and holds no memory or
// CPU; whether its parent has reaped it yet is that parent's business, not this
// test's, and on a host whose reaper is slow a zombie must not be reported as a
// surviving busy-loop.
func waitProcessGone(pid int, budget time.Duration) bool {
	deadline := time.Now().Add(budget)
	for {
		if err := syscall.Kill(pid, 0); errors.Is(err, syscall.ESRCH) {
			return true
		}
		if processIsZombie(pid) {
			return true
		}
		if !time.Now().Before(deadline) {
			return false
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// processIsZombie reads /proc/<pid>/stat's state field. The comm field can
// itself contain spaces and parentheses, so the parse starts after the LAST
// ')' -- the standard way to read this file without being fooled by a process
// named `sh (x)`. An unreadable stat means the process is gone.
func processIsZombie(pid int) bool {
	stat, err := os.ReadFile(filepath.Join("/proc", strconv.Itoa(pid), "stat"))
	if err != nil {
		return true
	}
	index := strings.LastIndex(string(stat), ")")
	if index < 0 {
		return false
	}
	fields := strings.Fields(string(stat)[index+1:])
	return len(fields) > 0 && fields[0] == "Z"
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
