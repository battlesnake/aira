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
// verifies: AIRA-151 §3.4, I1, I5
func TestOverCeilingUnescalatedReserveIsRefusedTerminallyInsteadOfClamped(t *testing.T) {
	// The §0.1 measured shape, byte for byte: the AIRA-139 fixture's 1 GiB slice
	// and 32 MiB + 8 MiB headroom, one OOM sample of 56360960, and the unpinned
	// 4 GiB client default.
	const (
		maximum      = int64(1) << 30
		wantRequired = int64(4294967296) // runner.DefaultConfineMemoryReserve
		wantCeiling  = int64(1031798784) // maximum - 32 MiB - 8 MiB
		wantBasis    = "fallback:insufficient-samples:n=1,oom-on-record"
	)
	server := saturatedServer(t)
	server.admitPeakHistory = staticPeakHistory(runner.PeakRSSStats{
		TotalCount: 1, SampleCount: 1, PeakMax: 56360960, OOMCount: 1, MaxOOMPeak: 56360960,
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
		t.Fatalf("required=%d, want the UNCLAMPED resolved reserve %d", rejection.Required, wantRequired)
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
