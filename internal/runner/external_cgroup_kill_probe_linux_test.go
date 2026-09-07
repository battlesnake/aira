//go:build linux

package runner

import (
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// AIRA-91's synthetic reproduction, committed per this repo's CLAUDE.md rule
// that a published measurement must have a committed, executable reproduction.
//
// It replaces the untracked `~/tmp/aira91-probe/` Makefile (targets `selfkill`,
// `groupkill`, `groupkill_dr`, `hog`) that the AIRA-91 investigation actually
// ran. The mechanism those probes established, and that this test pins:
//
//	an EXTERNAL whole-cgroup kill -- `systemd-oomd`'s `cgroup.kill`, a peer
//	session's `aira confine --kill`, or any direct write to the scope's own
//	`cgroup.kill` -- SIGKILLs every process in the scope while the kernel's
//	`memory.events` `oom_kill` counter stays at ZERO, because a userspace
//	`cgroup.kill` is not a memcg OOM event.
//
// Everything the confine trailer's post-run diagnostic looks at is derived from
// that counter (formatConfineReserveAdvisory takes `oom bool`, computed as
// `usage.OOMKill != nil && *usage.OOMKill > 0` in launchConfined), so the whole
// class is structurally invisible to it: the run prints the same trailer it
// would print after a clean exit.
//
// Fix 5 of the backlog-remediation plan (AIRA-70 unified with AIRA-91 Part A)
// EXTENDED this probe with the assertion that the new `terminated-by=` status
// facet reports this shape. It was committed ahead of that fix, and ahead of
// Fix 1 (AIRA-39), so both could be verified against a reproduction that lives
// in the repository rather than in one machine's `~/tmp`.
//
// VOCABULARY CORRECTION, made deliberately when Fix 5 landed: this file
// originally predicted the facet value would be `external-cgroup-kill`. It is
// `unattributed-sigkill` instead. A plan review established that
// `external-cgroup-kill` overclaims -- an external `kill -9` on the leader PID,
// a job that SIGKILLs ITSELF (the `selfkill` probe above), and an
// ancestor-cgroup OOM (memcg events propagate UPWARD, so a slice-level OOM
// leaves this scope's counter at zero) all produce this identical signature.
// What is positively establishable is only: SIGKILL, not sent by this
// supervisor, with no OOM recorded on this scope. The candidate mechanisms are
// named on the trailer's own candidates line instead of asserted by the facet.
// See docs/superpowers/plans/2026-09-05-aira70-91a-terminated-by-trailer-plan.md
// section 3.1.

// externalCgroupKillProbe runs one process inside a real, memory-controlled
// cgroup scope, kills the whole scope from outside via `cgroup.kill`, and
// returns the child's wait status together with the scope's own post-mortem
// usage counters. Shared entry point for the assertions below (and for Fix 5's
// `terminated-by=` extension), so every probe observes the identical mechanism.
func externalCgroupKillProbe(t *testing.T, scopeMemoryMax int64) (syscall.WaitStatus, cgroupUsage, string) {
	t.Helper()
	sleeper, err := exec.LookPath("sleep")
	if err != nil {
		skipOrFailRealCgroup(t, "sleep is unavailable for the external-kill fixture: %v", err)
	}
	// A +memory parent, so the child scope exposes memory.max/memory.events at
	// all: without the controller delegated, memory.events is simply absent and
	// the probe would prove nothing about the counter's VALUE.
	parent := writableMemoryParent(t, "max")
	scope := filepath.Join(parent, ".aira-probe-external-kill")
	if err := os.Mkdir(scope, 0o755); err != nil {
		skipOrFailRealCgroup(t, "cannot create probe scope under %s: %v", parent, err)
	}
	// Registered AFTER writableMemoryParent's own cleanup, so it runs BEFORE it
	// (t.Cleanup is LIFO): a surviving child directory would make the parent's
	// rmdir fail and leak a cgroup onto the live slice.
	t.Cleanup(func() {
		_ = os.WriteFile(filepath.Join(scope, "cgroup.kill"), []byte("1"), 0o644)
		for i := 0; i < 200; i++ {
			if os.Remove(scope) == nil {
				return
			}
			time.Sleep(10 * time.Millisecond)
		}
		t.Errorf("probe scope leaked (not removed within budget): %s", scope)
	})
	if _, err := os.Stat(filepath.Join(scope, "cgroup.kill")); err != nil {
		skipOrFailRealCgroup(t, "cgroup.kill unavailable (needs kernel >= 5.14): %v", err)
	}
	if err := os.WriteFile(filepath.Join(scope, "memory.max"), []byte(strconv.FormatInt(scopeMemoryMax, 10)), 0o644); err != nil {
		skipOrFailRealCgroup(t, "memory.max not writable on the probe scope: %v", err)
	}

	cmd := exec.Command(sleeper, "60")
	if err := cmd.Start(); err != nil {
		t.Fatalf("start probe child: %v", err)
	}
	killed := false
	defer func() {
		if !killed {
			_ = cmd.Process.Kill()
			_, _ = cmd.Process.Wait()
		}
	}()
	if err := os.WriteFile(filepath.Join(scope, "cgroup.procs"), []byte(strconv.Itoa(cmd.Process.Pid)), 0o644); err != nil {
		skipOrFailRealCgroup(t, "cannot migrate probe child into %s: %v", scope, err)
	}
	// Prove the child is really IN the scope before killing it. Otherwise a
	// migration that silently failed would leave this test killing an empty
	// cgroup and observing an unrelated exit -- a false pass in exactly the
	// direction that matters.
	procs, err := os.ReadFile(filepath.Join(scope, "cgroup.procs"))
	if err != nil || !cgroupProcsContainsPID(string(procs), cmd.Process.Pid) {
		t.Fatalf("probe child pid %d is not in %s/cgroup.procs (%q, err=%v)", cmd.Process.Pid, scope, procs, err)
	}

	// The external kill. This is the exact mechanism systemd-oomd uses under
	// PSI pressure and `aira confine --kill` uses across sessions.
	if err := os.WriteFile(filepath.Join(scope, "cgroup.kill"), []byte("1"), 0o644); err != nil {
		t.Fatalf("write cgroup.kill: %v", err)
	}
	killed = true
	waitErr := cmd.Wait()
	status, ok := cmd.ProcessState.Sys().(syscall.WaitStatus)
	if !ok {
		t.Fatalf("wait status of unexpected type %T (err=%v)", cmd.ProcessState.Sys(), waitErr)
	}
	return status, readCgroupUsage(scope), scope
}

func cgroupProcsContainsPID(procs string, pid int) bool {
	want := strconv.Itoa(pid)
	for _, line := range strings.Fields(procs) {
		if line == want {
			return true
		}
	}
	return false
}

// verifies: AIRA-91 -- an external whole-cgroup kill is signal-derived (SIGKILL,
// the 137 exit shape a shell reports) and leaves memory.events oom_kill at
// exactly ZERO. Both halves matter: the signal proves the job really died
// abnormally, and the zero counter proves the kernel does not attribute a
// userspace cgroup.kill to the memcg OOM killer.
func TestExternalCgroupKillIsSignalledWithZeroOOMKillCounter(t *testing.T) {
	const scopeMemoryMax = 128 << 20
	status, usage, scope := externalCgroupKillProbe(t, scopeMemoryMax)

	if !status.Signaled() || status.Signal() != syscall.SIGKILL {
		t.Fatalf("probe child wait status = %v (signaled=%v signal=%v), want SIGKILL from cgroup.kill",
			status, status.Signaled(), status.Signal())
	}
	// The shell-visible form of the same fact -- 128+SIGKILL -- which is what
	// the AIRA-91 incident artifact and the ~/tmp/aira91-probe selfkill log both
	// show. Pinned so a future change that starts reporting exit 0 for a
	// group-killed job is caught here, not in production.
	if got := 128 + int(status.Signal()); got != 137 {
		t.Fatalf("shell-visible exit code = %d, want 137", got)
	}

	if usage.OOMKill == nil {
		t.Fatalf("memory.events unreadable at %s: oom_kill is unevaluated, so this probe cannot establish the counter's value", scope)
	}
	if *usage.OOMKill != 0 {
		t.Fatalf("memory.events oom_kill = %d after an external cgroup.kill, want 0: the kernel would then be attributing a userspace group kill to the memcg OOM killer, and AIRA-91's premise no longer holds", *usage.OOMKill)
	}
}

// verifies: AIRA-91 -- the confine trailer's post-run diagnostic is driven
// SOLELY by the oom_kill counter above, so an external whole-cgroup kill
// produces no OOM diagnostic at all. This is the reported gap, captured as an
// executable reproduction rather than prose.
//
// Two separate claims, deliberately:
//
//  1. The DURABLE invariant: an external cgroup kill must never be misreported
//     as a kernel OOM kill at its memory cap. That stays true after Fix 5.
//  2. That this advisory STAYS silent for the case. Fix 5 (AIRA-70 + AIRA-91
//     Part A) closed the reporting gap on the STATUS line via the
//     `terminated-by=` facet (see TestExternalCgroupKillIsReportedOnTheTrailer
//     below), deliberately NOT by changing this advisory, whose cap gate and
//     OOM wording are unchanged -- if a later change makes
//     formatConfineReserveAdvisory itself speak for this case, update assertion
//     2 deliberately rather than deleting it.
func TestExternalCgroupKillProducesNoOOMAdvisory(t *testing.T) {
	const scopeMemoryMax = 128 << 20
	_, usage, _ := externalCgroupKillProbe(t, scopeMemoryMax)

	// Exactly launchConfined's own derivation (confine_linux.go): the boolean
	// the trailer is built from, recomputed here from the real counters so the
	// probe tracks the production rule instead of restating it.
	oom := usage.OOMKill != nil && *usage.OOMKill > 0
	if oom {
		t.Fatalf("probe precondition failed: oom flag is true after an external cgroup.kill (usage=%+v)", usage)
	}
	advisory := formatConfineReserveAdvisory(scopeMemoryMax, usage.PeakRSS, oom, ConfineCapSourceMemoryMax)

	if strings.Contains(advisory, "OOM-killed") {
		t.Fatalf("external cgroup kill misreported as a kernel OOM kill: %q", advisory)
	}
	// A `sleep` peaks at a few hundred KiB against a 128 MiB cap, far below the
	// 90% mark, so the near-cap branch cannot fire either -- the advisory is
	// empty, i.e. the run's trailer is byte-identical to a clean run's.
	if advisory != "" {
		t.Fatalf("AIRA-91's gap has changed shape: advisory = %q, want empty. If this is Fix 5 landing, update this assertion (and the comment above it) deliberately.", advisory)
	}

	// Guard against the opposite false pass: a formatConfineReserveAdvisory that
	// had been mutated into never returning anything would satisfy the empty
	// assertion above for the wrong reason. A positive oom_kill count must still
	// produce the kernel-OOM advisory.
	oomCount := int64(1)
	if reported := formatConfineReserveAdvisory(scopeMemoryMax, usage.PeakRSS, oomCount > 0, ConfineCapSourceMemoryMax); !strings.Contains(reported, "OOM-killed") {
		t.Fatalf("kernel-OOM advisory no longer fires for oom_kill>0: %q", reported)
	}
}

// verifies: AIRA-70 + AIRA-91 Part A -- against a REAL cgroup killed by a REAL
// external `cgroup.kill` write, the trailer's terminal facet reports
// `unattributed-sigkill` and the run is no longer indistinguishable from a
// clean one. This is Fix 5's reviewer-reproducible verification: it drives the
// production classifier from the production wait status and the production
// memory.events counters, not from a fixture.
//
// COVERAGE SPLIT, written down rather than left implicit (build-review round 2,
// P2). This test cannot run the whole of confineWithDeps, because a real
// external cgroup kill needs a real scope while confineWithDeps' unit harness
// substitutes a fake one. So the verification is deliberately in two halves:
//
//   - THIS test proves the MECHANISM and the CLASSIFICATION against real
//     kernel state -- a real cgroup.kill, real memory.events(.local).
//   - TestConfineTrailerReportsTerminationFacet proves the WIRING: it runs a
//     real child all the way through confineWithDeps and asserts the facet and
//     the candidates line appear in the diagnostics it actually printed, for
//     this same SIGKILL-with-zero-local-counter shape.
//
// Neither half alone is sufficient, and a change that breaks the join between
// them (a confineWithDeps that stopped assigning Status.TerminatedBy) fails the
// second, which the mutation set confirms.
func TestExternalCgroupKillIsReportedOnTheTrailer(t *testing.T) {
	const scopeMemoryMax = 128 << 20
	status, usage, scope := externalCgroupKillProbe(t, scopeMemoryMax)

	term := confineTermination{Decoded: true, Signaled: status.Signaled(), Signal: status.Signal()}
	verdict := classifyConfineTermination(term, usage, nil)
	if verdict != "unattributed-sigkill" {
		t.Fatalf("real external cgroup.kill on %s classified as %q, want unattributed-sigkill (status=%v usage=%+v)",
			scope, verdict, status, usage)
	}

	line := FormatConfineStatus(ConfineStatus{Slice: "finite.slice", TerminatedBy: verdict})
	if !strings.Contains(line, "terminated-by=unattributed-sigkill") {
		t.Fatalf("the verdict did not reach the operator-facing line: %q", line)
	}
	// The whole point of AIRA-91: this run must no longer read like a clean one.
	clean := FormatConfineStatus(ConfineStatus{Slice: "finite.slice", TerminatedBy: "normal"})
	if line == clean {
		t.Fatalf("an externally group-killed run still prints the same status line as a clean run: %q", line)
	}
	advisory := formatConfineTerminationAdvisory(verdict, usage)
	for _, want := range []string{"systemd-oomd", "aira confine --kill", "cgroup.kill"} {
		if !strings.Contains(advisory, want) {
			t.Fatalf("candidates line %q does not name %q, which AIRA-91 Part A asks for", advisory, want)
		}
	}
}
