package daemon

import (
	"encoding/json"
	"net"
	"testing"
	"time"

	"aira/internal/runner"
	"aira/internal/testdeadline"
)

// AIRA-153, end to end through the real admitConnection. The unit tests
// establish the VALUE; only the wire path can establish that a small slice's
// unpinned job is ENQUEUED and GRANTED where master refused it at the door, and
// that an over-fit OOM escalation is refused at the door where master enqueued
// it onto an ungrantable ceiling and left it to wait out its window.

// fittedEntryRefusal drives one admitConnection against a pre-created,
// EVALUATOR-LESS queue: nothing can grant, reject or time the request out, so
// the only frame it can possibly receive is an entry refusal, and an assertion
// that the waiter list stayed empty is a statement about the enqueue rather than
// a race with a fast timeout.
func fittedEntryRefusal(t *testing.T, server *Server, args map[string]any) (ResponseFrame, *sliceQueue) {
	t.Helper()
	queue := &sliceQueue{
		path: "/slice", kick: make(chan struct{}, 1), stop: make(chan struct{}),
		stopped: make(chan struct{}), poll: time.Hour, server: server,
	}
	server.admitRegistryMu.Lock()
	server.admitQueues = map[string]*sliceQueue{"/slice": queue}
	server.admitRegistryMu.Unlock()
	// A wait window that never fires: a rejection produced by the deadline rather
	// than by the ceiling boundary would be a different fact.
	server.admitAfter = func(time.Duration) <-chan time.Time { return make(chan time.Time) }

	serverConn, clientConn := net.Pipe()
	done := make(chan struct{})
	go func() {
		defer close(done)
		defer serverConn.Close()
		server.admitConnection(serverConn, args)
	}()
	t.Cleanup(func() {
		_ = clientConn.Close()
		<-done
	})

	frames := make(chan ResponseFrame, 1)
	errs := make(chan error, 1)
	go func() {
		var frame ResponseFrame
		if err := readFrame(clientConn, &frame); err != nil {
			errs <- err
			return
		}
		frames <- frame
	}()

	select {
	case frame := <-frames:
		return frame, queue
	case err := <-errs:
		t.Fatalf("read: %v", err)
	case <-testdeadline.After(5 * time.Second):
		t.Fatal("no frame: the request was admitted and is waiting, not refused at entry")
	}
	return ResponseFrame{}, nil
}

func fittedRejection(t *testing.T, frame ResponseFrame) admitRejection {
	t.Helper()
	if frame.Code != CodeAdmitTooLarge {
		t.Fatalf("code=%q, want %q: %+v", frame.Code, CodeAdmitTooLarge, frame)
	}
	var rejection admitRejection
	if err := json.Unmarshal(frame.Data, &rejection); err != nil {
		t.Fatalf("rejection payload: %v", err)
	}
	return rejection
}

func fittedNoWaiters(t *testing.T, queue *sliceQueue) {
	t.Helper()
	queue.mu.Lock()
	waiters := len(queue.waiters)
	queue.mu.Unlock()
	if waiters != 0 {
		t.Fatalf("%d waiter(s) enqueued; the refusal must happen BEFORE enqueueResolvedConfineAdmit so nothing is charged and no scope is created", waiters)
	}
}

// TestSmallSliceUnpinnedRequestIsAdmittedAndGrantedInsteadOfRefused is AIRA-153
// T5: §1.2's shape end to end, at the AIRA-139/149/151 fixture ceiling.
//
// This is the negative of AIRA-151's own "no waiter enqueued" assertion, on the
// same fixture: master answers an immediate E_ADMIT_TOO_LARGE and never
// enqueues, because the unpinned 4 GiB prior alone exceeds a 1 GiB slice's
// ceiling. The fitted prior is not only admissible but GRANTABLE against the
// fixture's own residual 4 KiB page -- which the ceiling itself would not have
// been (AIRA-150).
//
// verifies: AIRA-153 §1.2, §3.6, I11
func TestSmallSliceUnpinnedRequestIsAdmittedAndGrantedInsteadOfRefused(t *testing.T) {
	const (
		maximum    = int64(1) << 30
		wantResrve = int64(897216333) // FIT(1031798784)
		wantBasis  = "fallback:insufficient-samples:n=1,oom-on-record,ceiling-fitted"
	)
	server := saturatedServer(t)
	server.admitPeakHistory = staticPeakHistory(runner.PeakRSSStats{
		TotalCount: 1, SampleCount: 1, PeakMax: 56360960, OOMCount: 1, MaxOOMPeak: 56360960,
	})
	// The AIRA-150 shape exactly: one residual 4 KiB page, which is what made a
	// reserve equal to the ceiling ungrantable for 107 consecutive passes.
	server.admitReadMemory = func(string) (int64, int64, int64, bool, string) {
		return 4096, maximum, 0, true, ""
	}

	run := startSaturatedAdmit(t, server, maximum,
		saturatedArgs(runner.DefaultConfineMemoryReserve, "oom"))
	if run.ceiling != 1031798784 {
		t.Fatalf("fixture ceiling=%d, want 1031798784 — the fixture no longer models the measured slice", run.ceiling)
	}
	if run.waiter.reserve != wantResrve || run.waiter.basis != wantBasis {
		t.Fatalf("waiter reserve=%d basis=%q, want %d/%q — the waiter is built from the RESOLVED reserve",
			run.waiter.reserve, run.waiter.basis, wantResrve, wantBasis)
	}
	run.pass()

	select {
	case frame := <-run.frames:
		grant := admitGrantData(t, frame)
		if grant.Reserve != wantResrve || grant.Basis != wantBasis {
			t.Fatalf("grant reserve=%d basis=%q, want %d/%q", grant.Reserve, grant.Basis, wantResrve, wantBasis)
		}
		if grant.Reserve >= run.ceiling {
			t.Fatalf("granted reserve %d is not strictly below the ceiling %d", grant.Reserve, run.ceiling)
		}
	case err := <-run.errs:
		t.Fatalf("read: %v", err)
	case <-testdeadline.After(5 * time.Second):
		t.Fatal("no grant after one evaluator pass: the fitted reserve was admissible but not grantable, which is AIRA-150 at a new site")
	}
}

// TestDefaultInstallCeilingAdmitsANovelUnpinnedCommand is AIRA-153 T6: the
// ordinary-machine case this ticket exists for.
//
// `aira install` with no --memory-max sizes the slice at
// MemTotal - min(MemTotal/4, 16 GiB), floored to whole GiB. On an 8 GiB box that
// is a 6 GiB slice, whose entry ceiling under PRODUCTION headroom (2 GiB +
// 64 MiB, NOT the fixture's) is 4227858432 -- below the 4 GiB unpinned default.
// So on master every unpinned `aira confine -- <cmd>` on a default install of an
// ordinary box is refused E_ADMIT_TOO_LARGE, and SKILL.md mandates exactly that
// unpinned form for every heavy command.
//
// The command here is NOVEL: no per-signature history and no machine-wide p90 at
// all, so this row never touches the OOM branch.
//
// verifies: AIRA-153 §1.2, §6
func TestDefaultInstallCeilingAdmitsANovelUnpinnedCommand(t *testing.T) {
	const (
		maximum     = int64(6) << 30
		wantCeiling = int64(4227858432)
		wantReserve = int64(3676398636) // FIT(4227858432)
		wantBasis   = "fallback:no-history,ceiling-fitted"
	)
	server := saturatedServer(t)
	// PRODUCTION headroom, not the fixture's: the claim is about a real install.
	server.admitSliceHeadroomBase = admitSliceHeadroomBaseDefault
	server.admitSliceHeadroomSupervisor = admitSliceHeadroomSupervisorDefault
	server.admitPeakHistory = staticPeakHistory(runner.PeakRSSStats{})
	server.admitReadMemory = func(string) (int64, int64, int64, bool, string) {
		return 4096, maximum, 0, true, ""
	}

	run := startSaturatedAdmit(t, server, maximum,
		saturatedArgs(runner.DefaultConfineMemoryReserve, "novel"))
	if run.ceiling != wantCeiling {
		t.Fatalf("ceiling=%d, want %d — the fixture no longer models a default install on an 8 GiB box", run.ceiling, wantCeiling)
	}
	run.pass()

	select {
	case frame := <-run.frames:
		grant := admitGrantData(t, frame)
		if grant.Reserve != wantReserve || grant.Basis != wantBasis {
			t.Fatalf("grant reserve=%d basis=%q, want %d/%q", grant.Reserve, grant.Basis, wantReserve, wantBasis)
		}
	case err := <-run.errs:
		t.Fatalf("read: %v", err)
	case <-testdeadline.After(5 * time.Second):
		t.Fatal("no grant: an ordinary 8 GiB box still cannot run an unpinned confine job")
	}
}

// TestASliceTooSmallForAnyViableReserveStillRefusesTerminally is AIRA-153 T8,
// invariant I7.
//
// GREEN by construction. Its RED direction is a fit with NO floor, which would
// hand a job a sub-megabyte memory.max and instant-OOM it on placement -- the
// exact hazard workerAdmitEstimatedBytesMin was minted for, in this codebase's
// own words. Below runner.MinPinnedScopeCap the honest answer is the existing
// terminal refusal naming `required` and `cap_minus_headroom`.
//
// verifies: AIRA-153 §3.4, I7
func TestASliceTooSmallForAnyViableReserveStillRefusesTerminally(t *testing.T) {
	t.Run("one byte under the degenerate floor", func(t *testing.T) {
		// FIT(1205862) = 1048575, one byte under MinPinnedScopeCap, against
		// FIT(1205863) = 1048576 which fires.
		const wantCeiling = int64(1205862)
		server := saturatedServer(t)
		maximum := wantCeiling + server.admitSliceHeadroom(1)
		server.admitPeakHistory = staticPeakHistory(runner.PeakRSSStats{})
		server.admitReadMemory = func(string) (int64, int64, int64, bool, string) {
			return 0, maximum, 0, true, ""
		}
		frame, queue := fittedEntryRefusal(t, server,
			saturatedArgs(runner.DefaultConfineMemoryReserve, "novel"))
		rejection := fittedRejection(t, frame)
		if rejection.Required != runner.DefaultConfineMemoryReserve {
			t.Fatalf("required=%d, want the UNFITTED prior %d", rejection.Required, runner.DefaultConfineMemoryReserve)
		}
		if rejection.Ceiling != wantCeiling {
			t.Fatalf("cap_minus_headroom=%d, want %d", rejection.Ceiling, wantCeiling)
		}
		fittedNoWaiters(t, queue)
	})

	t.Run("a slice smaller than its own headroom", func(t *testing.T) {
		server := saturatedServer(t)
		server.admitPeakHistory = staticPeakHistory(runner.PeakRSSStats{})
		server.admitReadMemory = func(string) (int64, int64, int64, bool, string) {
			return 0, 1 << 20, 0, true, ""
		}
		frame, queue := fittedEntryRefusal(t, server,
			saturatedArgs(runner.DefaultConfineMemoryReserve, "novel"))
		rejection := fittedRejection(t, frame)
		if rejection.Required != runner.DefaultConfineMemoryReserve || rejection.Ceiling != 0 {
			t.Fatalf("rejection=%+v, want required=%d ceiling=0 — subtractFloor floors at zero and no site may fire there",
				rejection, runner.DefaultConfineMemoryReserve)
		}
		fittedNoWaiters(t, queue)
	})
}

// TestAnOOMAtTheFittedCapIsRefusedImmediatelyAndNeverEnqueues is AIRA-153 T12:
// the executable form of §2.5's derivation, and the reason AIRA-151's clamp had
// to be retargeted in the SAME change rather than deferred.
//
// A job admitted at a fitted cap and OOM-killed there records MaxOOMPeak ~= the
// fit. FIT(c)/c = 0.8696 lies inside AIRA-151's clamp band (2/3, 1) BY
// CONSTRUCTION, so on master the next admission escalates to 1.304c, the guard
// `MaxOOMPeak < ceiling` holds, and the reserve becomes EXACTLY the entry
// ceiling -- which is AIRA-150, ungrantable at 4096 bytes of charge. The request
// then waits out the default 30-minute window and is refused E_ADMIT_SATURATED,
// whose documented meaning to an agent is "owed a RETRY, nothing about the
// request is wrong". Because the job never runs, no new peak is recorded and the
// state is PERMANENT.
//
// With the guard at the fit it is refused immediately, with both numbers.
//
// verifies: AIRA-153 §2.5, §3.3, I11
func TestAnOOMAtTheFittedCapIsRefusedImmediatelyAndNeverEnqueues(t *testing.T) {
	const (
		maximum      = int64(1) << 30
		fit          = int64(897216333)  // FIT(1031798784)
		wantRequired = int64(1345824499) // fit + fit/2, unclamped
		wantCeiling  = int64(1031798784)
	)
	server := saturatedServer(t)
	server.admitPeakHistory = staticPeakHistory(runner.PeakRSSStats{
		TotalCount: 1, SampleCount: 1, PeakMax: fit, OOMCount: 1, MaxOOMPeak: fit,
	})
	server.admitReadMemory = func(string) (int64, int64, int64, bool, string) {
		return 4096, maximum, 0, true, ""
	}
	frame, queue := fittedEntryRefusal(t, server,
		saturatedArgs(runner.DefaultConfineMemoryReserve, "oom"))
	rejection := fittedRejection(t, frame)
	if rejection.Required != wantRequired {
		t.Fatalf("required=%d, want the UNCLAMPED escalation %d — a clamp onto the ceiling here is the 30-minute wedge",
			rejection.Required, wantRequired)
	}
	if rejection.Ceiling != wantCeiling {
		t.Fatalf("cap_minus_headroom=%d, want %d", rejection.Ceiling, wantCeiling)
	}
	if rejection.Basis != "estimate:oom-escalated" {
		t.Fatalf("basis=%q, want the unclamped escalation's own basis", rejection.Basis)
	}
	fittedNoWaiters(t, queue)
}
