//go:build linux

package runner

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"aira/internal/cgrouptest"
)

// verifies: AIRA-310 -- end to end on the real (enforced) path: a job that pinned
// 4 GiB, peaked at 300 MiB and exited 0 gets the hint on STDERR, after the
// trailer, never on stdout, with the exit code untouched. An unpinned (daemon
// history) reserve, a failed job, an env-inherited pin, a docker container and
// the opt-out get no hint.
func TestConfineReserveHintEndToEnd(t *testing.T) {
	type hintRun struct {
		reserve  int64
		pinned   bool
		fromEnv  bool
		argv     []string
		admitted *admissionResult // nil: the unit default (no daemon reserve)
	}
	run := func(t *testing.T, in hintRun) (string, string, int) {
		t.Helper()
		scope := &confineFakeScope{}
		deps := confineUnitDeps(scope)
		if in.admitted != nil {
			granted := *in.admitted
			deps.admit = func(context.Context, string, ConfineRequest, int64) (admissionResult, error) {
				return granted, nil
			}
		}
		peak := int64(300) << 20
		deps.readUsage = func(string) cgroupUsage { return cgroupUsage{PeakRSS: &peak} }
		deps.reportPeak = func(context.Context, ConfineRequest, ConfinePeakReport) error { return nil }
		// A fake scope has no cgroup FD; a declared reserve becomes the scope cap.
		deps.writeScopeMemoryCap = func(Scope, int64, int64, bool) error { return nil }
		argv := in.argv
		if argv == nil {
			argv = []string{"/bin/sh", "-c", "echo job-stdout"}
		}
		var stdout, stderr bytes.Buffer
		result, err := confineWithDeps(context.Background(), ConfineRequest{
			Slice: "finite.slice", Argv: argv,
			MemoryReserve: in.reserve, MemoryReservePinned: in.pinned, MemoryReserveFromEnv: in.fromEnv,
			SelfPath: os.Args[0], Stdout: &stdout, Stderr: &stderr,
		}, deps)
		if err != nil {
			t.Fatalf("confine: %v (stderr %q)", err, stderr.String())
		}
		if !strings.Contains(stderr.String(), "confine: slice=") {
			t.Fatalf("harness shape not reached: no trailer in %q", stderr.String())
		}
		return stdout.String(), stderr.String(), result.Exit
	}
	pinned4G := hintRun{reserve: 4 << 30, pinned: true}
	// A daemon grant of 4 GiB on a history estimate: large enough to clear the
	// 2 GiB minimum and the 60% line, so ONLY the pinned-provenance gate can keep
	// an unpinned run silent (build review P2: the 1 GiB default never reached it).
	historyGrant := &admissionResult{state: "immediate", release: &confineCountingCloser{}, reserve: 4 << 30, basis: "estimate:history"}
	const want = "confine: ACTION REQUIRED: over-reserved -- this run peaked at 300M, only 7% of the 4096M you pinned; an unused reserve like that can delay other jobs on the shared slice. Next run use --memory-reserve 512M (sized from this run alone; a command whose peak depends on a warm build cache needs its cold-run peak), or drop --memory-reserve and let aira estimate it from this command's history; also fix the script, Makefile or agent brief that pins 4096M"

	t.Run("pinned 4G hints on stderr only", func(t *testing.T) {
		t.Setenv(ConfineNoHintsEnv, "")
		stdout, stderr, exit := run(t, pinned4G)
		if exit != 0 {
			t.Fatalf("exit = %d, want 0", exit)
		}
		if !strings.Contains(stdout, "job-stdout") {
			t.Fatalf("harness shape not reached: job stdout = %q", stdout)
		}
		if strings.Contains(stdout, "ACTION REQUIRED") {
			t.Fatalf("hint leaked onto stdout: %q", stdout)
		}
		trailer := strings.Index(stderr, "terminated-by=normal")
		hint := strings.Index(stderr, want+"\n")
		if trailer < 0 || hint < 0 || hint < trailer {
			t.Fatalf("want the hint line after the trailer on stderr; trailer@%d hint@%d stderr=%q", trailer, hint, stderr)
		}
		if strings.Count(stderr, "confine: ACTION REQUIRED:") != 1 {
			t.Fatalf("want exactly one hint line: %q", stderr)
		}
	})
	t.Run("pinned 4G with a daemon grant of the same 4G still hints", func(t *testing.T) {
		// Positive control for the history-grant case below: the same admit
		// shape DOES reach the hint when the reserve was pinned.
		t.Setenv(ConfineNoHintsEnv, "")
		in := pinned4G
		in.admitted = historyGrant
		if _, stderr, _ := run(t, in); !strings.Contains(stderr, want) {
			t.Fatalf("pinned daemon-granted run did not hint: %q", stderr)
		}
	})
	t.Run("unpinned daemon-granted 4G history estimate never hints", func(t *testing.T) {
		t.Setenv(ConfineNoHintsEnv, "")
		_, stderr, _ := run(t, hintRun{admitted: historyGrant})
		if !strings.Contains(stderr, "reserve=4G") {
			t.Fatalf("harness shape not reached: charged reserve is not the 4G grant: %q", stderr)
		}
		if strings.Contains(stderr, "confine: ACTION REQUIRED:") {
			t.Fatalf("unpinned daemon-granted 4G hinted: %q", stderr)
		}
	})
	t.Run("failed job never hints", func(t *testing.T) {
		t.Setenv(ConfineNoHintsEnv, "")
		in := pinned4G
		in.argv = []string{"/bin/sh", "-c", "exit 3"}
		_, stderr, exit := run(t, in)
		if exit != 3 || !strings.Contains(stderr, "terminated-by=normal") {
			t.Fatalf("harness shape not reached: exit=%d stderr=%q", exit, stderr)
		}
		if strings.Contains(stderr, "confine: ACTION REQUIRED:") {
			t.Fatalf("failed job hinted: %q", stderr)
		}
	})
	t.Run("reserve pinned only by an inherited AIRA_CONFINE_RESERVE never hints", func(t *testing.T) {
		// An environment pin is shared by every nested confine under it, so one
		// per-job number would be the wrong advice for the variable.
		t.Setenv(ConfineNoHintsEnv, "")
		in := pinned4G
		in.fromEnv = true
		if _, stderr, _ := run(t, in); strings.Contains(stderr, "confine: ACTION REQUIRED:") {
			t.Fatalf("env-pinned run hinted: %q", stderr)
		}
	})
	t.Run("docker run never hints", func(t *testing.T) {
		// The container lives outside aira.slice, so peak-rss is the docker CLI
		// alone. A stand-in `docker` that just exits 0 reaches the same path.
		t.Setenv(ConfineNoHintsEnv, "")
		docker := filepath.Join(t.TempDir(), "docker")
		if err := os.WriteFile(docker, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
			t.Fatal(err)
		}
		in := pinned4G
		in.argv = []string{docker, "run", "img"}
		_, stderr, exit := run(t, in)
		if exit != 0 || !strings.Contains(stderr, "container="+ContainerPlacementDockerNotContained) {
			t.Fatalf("harness shape not reached: exit=%d stderr=%q", exit, stderr)
		}
		if strings.Contains(stderr, "confine: ACTION REQUIRED:") {
			t.Fatalf("docker run hinted: %q", stderr)
		}
	})
	t.Run("opt-out silences it", func(t *testing.T) {
		t.Setenv(ConfineNoHintsEnv, "1")
		if _, stderr, _ := run(t, pinned4G); strings.Contains(stderr, "confine: ACTION REQUIRED:") {
			t.Fatalf("opted-out run printed a hint: %q", stderr)
		}
	})
	t.Run("opt-out is exactly 1: any other value still hints", func(t *testing.T) {
		t.Setenv(ConfineNoHintsEnv, "0")
		if _, stderr, _ := run(t, pinned4G); !strings.Contains(stderr, want) {
			t.Fatalf("AIRA_CONFINE_NO_HINTS=0 silenced the hint: %q", stderr)
		}
	})
}

// verifies: AIRA-334 -- the RANT-73 shape on a real cgroup. A job that writes and
// streams a file several times its pinned cap reads peak-rss at the cap (the
// scope's memory.peak counts page cache, and the kernel holds usage at
// memory.max by reclaiming it) yet exits 0 and was never OOM-killed. Such a run
// needed no more than its cap (swap is off on every scope), so the trailer must
// carry no "reached N% of the reserved cap" advisory.
//
// The peak-at-cap precondition is asserted, not assumed: on a host where the
// stream did not drive the peak to the cap, the deleted line would not have
// fired either, and the test would prove nothing.
func TestConfineRealFileStreamAtCapGetsNoReserveAdvisory(t *testing.T) {
	parent := confineMemoryParent(t, strconv.FormatInt(1<<30, 10))
	const capBytes = int64(64) << 20
	file := filepath.Join(t.TempDir(), "stream")
	deps := defaultConfineDeps()
	deps.admit = func(context.Context, string, ConfineRequest, int64) (admissionResult, error) {
		return admissionResult{state: "immediate"}, nil
	}
	// Never write this test's peak into the machine's shared reserve history.
	deps.reportPeak = func(context.Context, ConfineRequest, ConfinePeakReport) error { return nil }
	var stderr bytes.Buffer
	// Written INSIDE the scope, so its page cache is charged to the scope rather
	// than to the test process; then read back twice. 256 MiB is 4x the cap.
	script := "dd if=/dev/zero of=\"$1\" bs=1M count=256 status=none && cat \"$1\" \"$1\" >/dev/null"
	result, err := confineWithDeps(context.Background(), ConfineRequest{
		Slice: parent, Argv: []string{"/bin/sh", "-c", script, "sh", file},
		MemoryReserve: capBytes, MemoryReservePinned: true,
		SelfPath: os.Args[0], Stdout: &bytes.Buffer{}, Stderr: &stderr,
		AdmissionMaxWait: 2 * time.Second, PollInterval: 10 * time.Millisecond,
	}, deps)
	if err != nil {
		cgrouptest.SkipOrFailRealCgroup(t, "confine file-stream fixture unavailable: %v", err)
	}
	if result.Exit != 0 || result.Status.TerminatedBy != ConfineTerminatedNormal {
		t.Fatalf("exit=%d terminated-by=%q, want a clean run; stderr=%q", result.Exit, result.Status.TerminatedBy, stderr.String())
	}
	if result.Status.ScopeMemoryMax != capBytes {
		t.Fatalf("scope-memory.max=%d, want the pinned %d; stderr=%q", result.Status.ScopeMemoryMax, capBytes, stderr.String())
	}
	if result.Status.PeakRSS == nil || *result.Status.PeakRSS < capBytes-capBytes/10 {
		t.Fatalf("precondition: peak-rss=%v did not reach 90%% of the %d cap, so this run cannot show the at-cap line is gone; stderr=%q",
			result.Status.PeakRSS, capBytes, stderr.String())
	}
	if strings.Contains(stderr.String(), "reserved cap") || strings.Contains(stderr.String(), "consider a higher --memory-reserve") {
		t.Fatalf("a clean, non-OOM run at its cap printed a reserve advisory: %q", stderr.String())
	}
}
