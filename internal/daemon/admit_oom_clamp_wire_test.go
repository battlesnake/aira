package daemon

import (
	"encoding/json"
	"net"
	"testing"
	"time"

	"aira/internal/runner"
	"aira/internal/testdeadline"
)

// AIRA-151, end to end. An over-ceiling reserve the escalation did NOT
// determine is refused terminally at request entry instead of being clamped
// onto the ceiling and then waiting.
//
// Driven through the real admitConnection over net.Pipe, as
// TestSliceCeilingDoesNotReachTheOOMEscalationClamp is, because the claim is
// about what reaches the CLIENT and when. Calling resolveAdmitReserve directly
// establishes the value; it cannot establish that admit.go:1903 refuses before
// enqueueResolvedConfineAdmit, which is the whole behavioural consequence.
//
// The queue is pre-created WITHOUT its evaluator goroutine, so no pass can run:
// an assertion that the waiter list stayed empty is then a statement about the
// enqueue, not a race with a fast timeout.
//
// AIRA-153 RE-BASED this test's fixture, and the move is itself the executable
// evidence of what that ticket removed. It used to drive the unpinned 4 GiB
// CLIENT DEFAULT over a 1 GiB ceiling — AIRA-149 §3.1 row (e). A prior can no
// longer BE an over-ceiling value: it is fitted to FIT(ceiling) before anything
// reads it, so that shape now enqueues and is granted
// (TestSmallSliceUnpinnedRequestIsAdmittedAndGrantedInsteadOfRefused) and this
// test would have failed on its own first assertion.
//
// AIRA-151's claim is NOT weakened; only its route to an over-ceiling value
// moves, onto an ORDINARY per-signature ESTIMATE — this command's own measured
// evidence, which AIRA-153 deliberately never fits (I2), so it still reaches the
// `reserve > ceiling` boundary unchanged and is still refused terminally with
// both numbers. Five samples of a 2 GiB peak give 2469606195; the OOM record's
// 1.5x escalation (84541440) loses, so the escalation determined nothing and the
// clamp must not apply.
//
// verifies: AIRA-151 §3.4, I1, I5; AIRA-153 I2, U1
func TestOverCeilingUnescalatedReserveIsRefusedTerminallyInsteadOfClamped(t *testing.T) {
	// The same 1 GiB slice and 32 MiB + 8 MiB headroom as the AIRA-139 fixture.
	const (
		maximum      = int64(1) << 30
		wantRequired = int64(2469606195) // 2 GiB peak + 15%
		wantCeiling  = int64(1031798784) // maximum - 32 MiB - 8 MiB
		wantBasis    = "estimate:max=2147483648,n=5,f=115,oom-on-record"
	)
	server := saturatedServer(t)
	server.admitPeakHistory = staticPeakHistory(runner.PeakRSSStats{
		TotalCount: 5, SampleCount: 5, PeakMax: 2147483648, OOMCount: 1, MaxOOMPeak: 56360960,
	})
	server.admitReadMemory = func(string) (int64, int64, int64, bool, string) {
		return 4096, maximum, 0, true, ""
	}

	// Pre-created, evaluator-less: nothing can grant, reject or time this request
	// out, so the only frame it can possibly receive is an entry refusal.
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
		server.admitConnection(serverConn, map[string]any{
			"slice": "slice", "reserve": runner.DefaultConfineMemoryReserve,
			"max_wait_ms": int64(30_000), "signature": "oom",
		})
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

	var frame ResponseFrame
	select {
	case frame = <-frames:
	case err := <-errs:
		t.Fatalf("read: %v", err)
	case <-testdeadline.After(5 * time.Second):
		t.Fatal("no frame: the request was admitted and is waiting, which is exactly the outcome AIRA-151 removes")
	}

	if frame.Code != CodeAdmitTooLarge {
		t.Fatalf("code=%q, want %q — an over-ceiling reserve the escalation did not set must be refused, not clamped onto the ceiling",
			frame.Code, CodeAdmitTooLarge)
	}
	var rejection admitRejection
	if err := json.Unmarshal(frame.Data, &rejection); err != nil {
		t.Fatalf("rejection payload: %v", err)
	}
	if rejection.Required != wantRequired {
		t.Fatalf("required=%d, want the UNCLAMPED resolved reserve %d — an ordinary estimate is this command's own measurement and is never fitted or clamped",
			rejection.Required, wantRequired)
	}
	if rejection.Ceiling != wantCeiling {
		t.Fatalf("cap_minus_headroom=%d, want the request-entry ceiling %d", rejection.Ceiling, wantCeiling)
	}
	if rejection.Basis != wantBasis {
		t.Fatalf("basis=%q, want %q — the refusal must still name the OOM record it consulted", rejection.Basis, wantBasis)
	}

	// The load-bearing half: nothing was enqueued, so nothing was charged to the
	// ledger, no waiter existed and no scope was created. A fast timeout would
	// have produced a frame too; only this distinguishes an entry refusal.
	queue.mu.Lock()
	waiters := len(queue.waiters)
	queue.mu.Unlock()
	if waiters != 0 {
		t.Fatalf("%d waiter(s) enqueued; the refusal must happen BEFORE enqueueResolvedConfineAdmit so nothing is charged and no scope is created", waiters)
	}
}
