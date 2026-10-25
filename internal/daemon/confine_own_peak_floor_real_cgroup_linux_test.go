//go:build linux

package daemon

import (
	"bytes"
	"context"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"aira/internal/cgrouptest"
	"aira/internal/runner"
)

// ownPeakFloorTargetBytes is what the RANT-55 replay's workload touches: ~1.1x
// the 1 GiB unpinned default, as anonymous, touched memory (never page cache), so
// a run capped at the default is OOM-killed and a run at 1.15x its own recorded
// peak is not.
const ownPeakFloorTargetBytes = int64(1181116006)

// TestOwnPeakFloorRealCgroup is AIRA-314 T15, the RANT-55 shape end to end
// against a REAL cgroup and a REAL daemon: a command run once PINNED, cleanly, at
// a peak above the 1 GiB unpinned default, then re-run unpinned with nothing
// changed. Before AIRA-314 the re-run was sized at the 1 GiB default (one sample
// is "insufficient") and OOM-killed; with the floor it is sized from its own
// recorded peak and completes.
//
// No machine-wide prior is seeded, so the unfloored resolution is the 1 GiB
// default, exactly as on a box where the p90 read times out (spec §1).
// Reuses the AIRA-128 fixture's workload, slice and headroom.
//
// verifies: AIRA-314 I1, T15
func TestOwnPeakFloorRealCgroup(t *testing.T) {
	slice := newOOMSelfHealSlice(t)
	paths := testPaths(t)
	server := NewServer(paths)
	server.admitSliceHeadroomBase = 32 << 20
	server.admitSliceHeadroomSupervisor = 8 << 20
	startServer(t, server)

	run := func(reserve int64, pinned bool) oomSelfHealRun {
		t.Helper()
		var stdout, stderr bytes.Buffer
		ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
		defer cancel()
		result, err := runner.Confine(ctx, runner.ConfineRequest{
			Slice: slice, RuntimeDir: paths.RuntimeDir, AdmitSocketPath: paths.SocketPath,
			SelfPath: os.Args[0], Argv: oomSelfHealArgv("own-peak-floor"),
			Env: append(os.Environ(),
				oomSelfHealEnv+"=1",
				oomSelfHealBytes+"="+strconv.FormatInt(ownPeakFloorTargetBytes, 10),
			),
			MemoryReserve: reserve, MemoryReservePinned: pinned,
			AdmissionMaxWait: 30 * time.Second,
			Stdin:            strings.NewReader(""), Stdout: &stdout, Stderr: &stderr,
		})
		return oomSelfHealRun{result: result, err: err, stdout: stdout.String(), stderr: stderr.String()}
	}

	// The pinned clean run, as RANT-55's operator ran it.
	first := run(2<<30, true)
	if first.err != nil {
		cgrouptest.SkipOrFailRealCgroup(t, "pinned confine run unavailable: %v (stderr %q)", first.err, first.stderr)
	}
	if first.result.Exit != 0 || first.result.Status.TerminatedBy != runner.ConfineTerminatedNormal || !strings.Contains(first.stdout, oomSelfHealMarker) {
		t.Fatalf("pinned run exit=%d terminated-by=%q stdout=%q stderr=%q, want a completed workload",
			first.result.Exit, first.result.Status.TerminatedBy, first.stdout, first.stderr)
	}
	if first.result.Status.PeakRSS == nil || *first.result.Status.PeakRSS <= runner.DefaultConfineMemoryReserve {
		t.Fatalf("pinned run peak=%v, want an established peak above the 1 GiB default — the fixture does not model RANT-55", first.result.Status.PeakRSS)
	}
	peak := *first.result.Status.PeakRSS

	// The unpinned re-run of the identical argv: no reserve at all, so the client
	// sends the 1 GiB default UNPINNED and the daemon sizes the grant.
	second := run(0, false)
	if second.err != nil {
		t.Fatalf("unpinned re-run: %v (stderr %q)", second.err, second.stderr)
	}
	if !strings.HasPrefix(second.result.Status.ReserveBasis, "estimate:own-max=") {
		t.Fatalf("re-run reserve-basis=%q reserve=%d, want estimate:own-max=... — the own recorded peak %d was ignored (stderr %q)",
			second.result.Status.ReserveBasis, second.result.Status.ReserveBytes, peak, second.stderr)
	}
	if floor := runner.GrowByEstimatorMargin(peak); second.result.Status.ReserveBytes < floor {
		t.Fatalf("re-run reserve=%d, want at least 1.15x the recorded peak (%d)", second.result.Status.ReserveBytes, floor)
	}
	if second.result.Exit != 0 || second.result.Status.TerminatedBy != runner.ConfineTerminatedNormal || !strings.Contains(second.stdout, oomSelfHealMarker) {
		t.Fatalf("re-run exit=%d terminated-by=%q stdout=%q stderr=%q, want the identical command to complete instead of being OOM-killed at the default",
			second.result.Exit, second.result.Status.TerminatedBy, second.stdout, second.stderr)
	}
}
