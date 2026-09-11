package daemon

import (
	"net"
	"testing"
	"time"

	"aira/internal/runner"
)

// S4 (D4-RESOLVED): the RAM admission gate is MODE-DEPENDENT.
//
//   - The LEDGER check applies in BOTH modes: available = ceiling − Σleases,
//     SIGNED. It may go negative during the restart re-declare window; the next
//     NEW admission then waits until a release recovers it (un-clamp, not
//     clamp-at-zero).
//   - CI (ci-shim / advisory) mode: LEDGER-ONLY. The container memory.max (the
//     ceiling) + declared reserves are the truth; the physical current floor is
//     dropped (nothing runs outside the slice).
//   - DEV (real-cgroup daemon) mode: KEEP the physical floor AND the
//     MemAvailable-aware AIRA-103/106 pressure ceiling, so a NEW admission is
//     refused when the SYSTEM is low even if the slice ledger shows room.
//
// These three tests + their mutations pin each arm. See the S4 build record.

// buildS4Queue constructs a queue with a set of already-granted (accounted)
// leases plus one queued newcomer, then runs one evaluation pass and returns the
// newcomer. outstanding is set to the exact Σ of the granted reserves, which is
// also what rederiveLedgerLocked would compute over the same accounted waiters,
// so the manual value and any re-derivation agree.
func buildS4Queue(server *Server, path string, now time.Time, heldReserves []int64, newcomer int64) *admitWaiter {
	var waiters []*admitWaiter
	var outstanding int64
	seq := int64(0)
	for _, reserve := range heldReserves {
		seq++
		waiters = append(waiters, &admitWaiter{
			seq: seq, reserve: reserve, state: admitGranted, accounted: true,
			grantedCh: make(chan struct{}), grantedAt: now.Add(-time.Hour),
		})
		outstanding += reserve
	}
	seq++
	queued := &admitWaiter{
		seq: seq, reserve: newcomer, state: admitQueued,
		grantedCh: make(chan struct{}), enqueued: now,
	}
	waiters = append(waiters, queued)
	queue := &sliceQueue{
		path: path, server: server,
		waiters: waiters, outstanding: outstanding, outstandingJobs: len(heldReserves),
	}
	server.evaluateAdmitQueue(queue)
	return queued
}

// verifies: S4 test (i) — the signed ledger goes NEGATIVE when granted leases sum
// past the ceiling (the restart re-declare window), so the next NEW admission
// WAITS; a release that brings Σleases back under the ceiling recovers it. Holds
// in BOTH modes. Mutation-verified: re-adding the clamp-at-zero in checkedAvailable
// makes the reported grantable read 0 instead of the true negative, reding the
// signed assertion below.
func TestS4SignedLedgerNegativeWaitsThenRecoversOnRelease(t *testing.T) {
	const maximum = 64 * gib
	for _, mode := range []struct {
		name string
		ci   bool
	}{{"dev", false}, {"ci", true}} {
		t.Run(mode.name, func(t *testing.T) {
			now := time.Unix(700_000, 0)
			server := NewServer(Paths{})
			server.admitNow = func() time.Time { return now }
			server.admitSliceHeadroomBase = 0
			server.admitSliceHeadroomSupervisor = 0
			// current=0 in both modes: the DEV physical floor is idle, so the ONLY
			// thing driving available negative is the ledger (Σleases > ceiling).
			server.admitReadMemory = func(string) (int64, int64, int64, bool, string) {
				return 0, maximum, 0, true, ""
			}
			if mode.ci {
				server.SetConfineShimModeForTest(maximum, runner.ShimBudgetSourceDeclared, "")
			}

			// Over-subscribed: two 40 GiB leases on a 64 GiB ceiling => Σ=80 GiB,
			// available_ledger = 64 − 80 = −16 GiB.
			over := buildS4Queue(server, "/slice", now, []int64{40 * gib, 40 * gib}, 1*gib)
			if over.state != admitQueued {
				t.Fatalf("new admission must WAIT while the signed ledger is negative (state=%v)", over.state)
			}
			if over.lastGrantable == nil {
				t.Fatalf("the refused newcomer must have recorded a grantable figure")
			}
			if got := *over.lastGrantable; got != -16*gib {
				t.Fatalf("available must be SIGNED −16 GiB (ceiling 64 − Σleases 80), got %d; a clamp-at-zero would report 0", got)
			}

			// A release recovers it: one 40 GiB lease remains => Σ=40 GiB,
			// available = 64 − 40 = 24 GiB, so a 1 GiB newcomer now fits.
			recovered := buildS4Queue(server, "/slice", now, []int64{40 * gib}, 1*gib)
			if recovered.state != admitGranted {
				t.Fatalf("once a release brings Σleases under the ceiling the newcomer must be granted (state=%v)", recovered.state)
			}
		})
	}
}

// verifies: S4 test (ii) — CI (ci-shim) mode is LEDGER-ONLY: physical over-use
// (a large memory.current) does NOT reduce available, so a newcomer that fits the
// ledger is admitted regardless of current. Mutation-verified: making the CI arm
// consult the physical floor (checkedAvailable with the real current) drops
// available to ceiling−current and refuses the 32 GiB newcomer.
func TestS4CIModeLedgerOnlyIgnoresPhysicalOveruse(t *testing.T) {
	const (
		maximum = 64 * gib
		current = 60 * gib // physical over-use that WOULD refuse under a floor
		reserve = 32 * gib // fits the ledger (Σ=0) but not ceiling−current (=4 GiB)
	)
	now := time.Unix(710_000, 0)
	server := NewServer(Paths{})
	server.admitNow = func() time.Time { return now }
	server.admitSliceHeadroomBase = 0
	server.admitSliceHeadroomSupervisor = 0
	server.SetConfineShimModeForTest(maximum, runner.ShimBudgetSourceDeclared, "")
	server.admitReadMemory = func(string) (int64, int64, int64, bool, string) {
		return current, maximum, 0, true, ""
	}

	queued := buildS4Queue(server, "/slice", now, nil, reserve)
	if queued.state != admitGranted {
		t.Fatalf("CI ledger-only: a 32 GiB reserve fitting the empty ledger must be admitted despite %d bytes of physical use (state=%v); consulting the floor here is the over-conservative dev behaviour", int64(current), queued.state)
	}
}

// verifies: S4 — the DEV arm of the mode-gate KEEPS the physical floor: physical
// over-use (memory.current above ceiling − Σleases) refuses a newcomer that fits
// the ledger. The dev mirror of the CI test above, holding current identical; the
// opposite verdict (refused vs granted) is the mode split. Mutation-verified by
// the same seam as test (ii): making the CI arm consult the floor makes CI behave
// like this; making the DEV arm ledger-only would grant here.
func TestS4DevModeFloorRefusesOnPhysicalOveruse(t *testing.T) {
	const (
		maximum = 64 * gib
		current = 60 * gib // physical over-use: ceiling − current = 4 GiB
		reserve = 32 * gib // fits the empty ledger but not the physical floor
	)
	now := time.Unix(715_000, 0)
	server := NewServer(Paths{}) // dev mode (real-cgroup): no SetConfineShimModeForTest
	server.admitNow = func() time.Time { return now }
	server.admitSliceHeadroomBase = 0
	server.admitSliceHeadroomSupervisor = 0
	server.admitReadMemory = func(string) (int64, int64, int64, bool, string) {
		return current, maximum, 0, true, ""
	}

	queued := buildS4Queue(server, "/slice", now, nil, reserve)
	if queued.state != admitQueued {
		t.Fatalf("DEV mode must REFUSE a 32 GiB reserve when physical use (%d bytes) leaves only ceiling−current room, even though the ledger is empty (state=%v)", int64(current), queued.state)
	}
}

// verifies: S4 test (iii) — DEV mode refuses a NEW admission when SYSTEM-available
// RAM is low even though the slice LEDGER shows room. The MemAvailable-aware
// AIRA-103/106 pressure ceiling shrinks the effective maximum; a reserve that fits
// the raw slice ceiling but not the shrunk one is refused. Mutation-verified:
// making the dev arm use the raw maximum (skipping admitEffectiveMaximum) admits it.
func TestS4DevModeRefusesWhenSystemAvailableRAMLow(t *testing.T) {
	const (
		slicePath = "/sys/fs/cgroup/aira.slice"
		maximum   = 64 * gib // the slice ledger has ample room
		shrunk    = 8 * gib  // pressure ceiling under low system MemAvailable
		reserve   = 16 * gib // fits maximum, not the shrunk system-aware ceiling
	)
	now := time.Unix(720_000, 0)
	server := NewServer(Paths{})
	server.admitNow = func() time.Time { return now }
	server.admitSliceHeadroomBase = 0
	server.admitSliceHeadroomSupervisor = 0
	// Slice usage is low (current=0) and the ledger is empty, so the ONLY refusal
	// source is the system-aware pressure ceiling.
	server.admitReadMemory = func(string) (int64, int64, int64, bool, string) {
		return 0, maximum, 0, true, ""
	}
	// A low system MemAvailable published as a shrunk enforce ceiling.
	server.publishSliceCeilingSnapshot(sliceCeilingSnapshot{
		Mode: sliceCeilingEnforce, SlicePath: slicePath,
		State: sliceCeilingThrottled, Ceiling: shrunk, StaticMax: maximum,
	})

	queued := buildS4Queue(server, slicePath, now, nil, reserve)
	if queued.state != admitQueued {
		t.Fatalf("DEV mode must REFUSE a %d GiB admission when the system-aware ceiling is %d GiB, even though the slice ledger (max %d GiB) shows room (state=%v)",
			int64(reserve)/gib, int64(shrunk)/gib, int64(maximum)/gib, queued.state)
	}
}

// verifies: S4 Invariant 6 — the admitConnection !ok path fails CLOSED: an
// unreadable slice/container budget refuses a NEW admission (E_DAEMON_UNAVAILABLE)
// rather than emitting a grant-shaped `unevaluated` the runner would launch
// uncapped. Matches evaluateAdmitQueue's own !ok branch (which grants nothing).
// Mutation-verified: restoring the grant-shaped unevaluated response makes the
// reply an OK grant, reding the refusal assertion.
func TestS4AdmitConnectionFailsClosedWhenSliceUnreadable(t *testing.T) {
	// Ordinary and exclusive requests both fail CLOSED on an unreadable slice, with
	// their own honest codes. The exclusive code is code-HONESTY only, not
	// launch-prevention: the runner's fail() refuses ANY exclusive request before
	// the flock fallback (admission_linux.go:447), so E_DAEMON_UNAVAILABLE would ALSO
	// refuse exclusivity — U_ADMIT_EXCLUSIVE_UNESTABLISHED just carries the precise
	// reason (handled by the runner's pre-payload exclusive switch) instead of the
	// generic "exchange did not complete". This case pins that specific code.
	for _, tc := range []struct {
		name      string
		exclusive bool
		wantCode  string
	}{
		{"ordinary", false, CodeUnavailable},
		{"exclusive", true, CodeAdmitExclusiveUnestablished},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := NewServer(Paths{})
			server.stopping = make(chan struct{})
			server.admitPollInterval = time.Hour
			server.admitResolveSlice = func(string) (string, bool, string) { return "/slice", true, "" }
			server.admitReadMemory = func(string) (int64, int64, int64, bool, string) {
				return 0, 0, 0, false, "slice-unreadable"
			}
			args := validAdmitArgs(1, 100)
			if tc.exclusive {
				// An exclusive request must carry a complete identity tuple; the
				// validator refuses a bare exclusive flag.
				scope := exclusiveScopeID(t, "bench", 4242)
				delete(args, "pinned")
				args["exclusive"] = true
				args["scope_id"] = scope
				args["name"] = "bench"
				args["owner"] = exclusiveScopeOwner(scope)
			}

			serverConn, clientConn := net.Pipe()
			done := make(chan struct{})
			go func() {
				defer close(done)
				defer serverConn.Close()
				server.admitConnection(serverConn, args)
			}()
			var frame ResponseFrame
			if err := readFrame(clientConn, &frame); err != nil {
				t.Fatal(err)
			}
			if frame.Code != tc.wantCode {
				t.Fatalf("an unreadable slice must fail CLOSED with %s, not a grant-shaped response (code=%q data=%s)", tc.wantCode, frame.Code, frame.Data)
			}
			<-done
			server.admitRegistryMu.Lock()
			if len(server.admitQueues) != 0 {
				t.Fatalf("a fail-closed refusal must not enqueue anything: %d", len(server.admitQueues))
			}
			server.admitRegistryMu.Unlock()
		})
	}
}
