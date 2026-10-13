//go:build linux

package runner

import (
	"bytes"
	"context"
	"os"
	"strings"
	"testing"
)

// verifies: AIRA-310 -- end to end on the real (enforced) path: a job that pinned
// 4 GiB, peaked at 300 MiB and exited 0 gets the hint on STDERR, after the
// trailer, never on stdout, with the exit code untouched. The unpinned run of the
// same job, and the same run with the opt-out set, get no hint.
func TestConfineReserveHintEndToEnd(t *testing.T) {
	run := func(t *testing.T, reserve int64, pinned bool) (string, string, int) {
		t.Helper()
		scope := &confineFakeScope{}
		deps := confineUnitDeps(scope)
		peak := int64(300) << 20
		deps.readUsage = func(string) cgroupUsage { return cgroupUsage{PeakRSS: &peak} }
		deps.reportPeak = func(context.Context, ConfineRequest, ConfinePeakReport) error { return nil }
		// A fake scope has no cgroup FD; a declared reserve becomes the scope cap.
		deps.writeScopeMemoryCap = func(Scope, int64, int64, bool) error { return nil }
		var stdout, stderr bytes.Buffer
		result, err := confineWithDeps(context.Background(), ConfineRequest{
			Slice: "finite.slice", Argv: []string{"/bin/sh", "-c", "echo job-stdout"},
			MemoryReserve: reserve, MemoryReservePinned: pinned,
			SelfPath: os.Args[0], Stdout: &stdout, Stderr: &stderr,
		}, deps)
		if err != nil {
			t.Fatalf("confine: %v (stderr %q)", err, stderr.String())
		}
		return stdout.String(), stderr.String(), result.Exit
	}
	const want = "confine: hint: peak 300M of 4096M reserved (7%); consider --memory-reserve 512M so more jobs fit"

	t.Run("pinned 4G hints on stderr only", func(t *testing.T) {
		t.Setenv(ConfineNoHintsEnv, "")
		stdout, stderr, exit := run(t, 4<<30, true)
		if exit != 0 {
			t.Fatalf("exit = %d, want 0", exit)
		}
		if !strings.Contains(stdout, "job-stdout") {
			t.Fatalf("harness shape not reached: job stdout = %q", stdout)
		}
		if strings.Contains(stdout, "hint") {
			t.Fatalf("hint leaked onto stdout: %q", stdout)
		}
		trailer := strings.Index(stderr, "terminated-by=normal")
		hint := strings.Index(stderr, want+"\n")
		if trailer < 0 || hint < 0 || hint < trailer {
			t.Fatalf("want the hint line after the trailer on stderr; trailer@%d hint@%d stderr=%q", trailer, hint, stderr)
		}
		if strings.Count(stderr, "confine: hint:") != 1 {
			t.Fatalf("want exactly one hint line: %q", stderr)
		}
	})
	t.Run("unpinned never hints", func(t *testing.T) {
		t.Setenv(ConfineNoHintsEnv, "")
		_, stderr, _ := run(t, 0, false)
		if strings.Contains(stderr, "confine: hint:") {
			t.Fatalf("unpinned run printed a hint: %q", stderr)
		}
	})
	t.Run("opt-out silences it", func(t *testing.T) {
		t.Setenv(ConfineNoHintsEnv, "1")
		_, stderr, _ := run(t, 4<<30, true)
		if !strings.Contains(stderr, "terminated-by=normal") {
			t.Fatalf("harness shape not reached: %q", stderr)
		}
		if strings.Contains(stderr, "confine: hint:") {
			t.Fatalf("opted-out run printed a hint: %q", stderr)
		}
	})
}
