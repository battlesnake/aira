package daemon

import (
	"context"
	"encoding/json"
	"net"
	"path/filepath"
	"testing"
	"time"

	"aira/internal/core"
	"aira/internal/runner"
	"aira/internal/store"
)

func TestConfineEstimatorAndOOMEscalationClamp(t *testing.T) {
	server := NewServer(Paths{})
	server.stopping = make(chan struct{})
	server.admitPeakP90 = func(context.Context) (int64, bool, error) { return 0, false, nil }
	server.admitPeakHistory = func(_ context.Context, signature string) (runner.PeakRSSStats, error) {
		switch signature {
		case "ordinary":
			return runner.PeakRSSStats{TotalCount: 3, SampleCount: 3, PeakMax: 15 << 30}, nil
		case "oom":
			return runner.PeakRSSStats{TotalCount: 4, SampleCount: 4, PeakMax: 40 << 30, OOMCount: 1, MaxOOMPeak: 40 << 30}, nil
		default:
			return runner.PeakRSSStats{}, nil
		}
	}
	ordinary, basis := server.resolveAdmitReserve(admitRequest{reserve: 4 << 30, signature: "ordinary"}, 60<<30)
	if ordinary != (15<<30)+(15<<30)*15/100 || basis[:9] != "estimate:" {
		t.Fatalf("ordinary reserve=%d basis=%q", ordinary, basis)
	}
	escalated, basis := server.resolveAdmitReserve(admitRequest{reserve: 4 << 30, signature: "oom"}, 55<<30)
	if escalated != 55<<30 || basis != "estimate:oom-escalated" {
		t.Fatalf("OOM reserve=%d basis=%q, want multiplicative result clamped to ceiling", escalated, basis)
	}
}

func TestConfineOOMAtCeilingIsGenuinelyTooLargeAndPinWins(t *testing.T) {
	server := NewServer(Paths{})
	server.stopping = make(chan struct{})
	server.admitPeakHistory = func(context.Context, string) (runner.PeakRSSStats, error) {
		return runner.PeakRSSStats{TotalCount: 1, SampleCount: 1, PeakMax: 10 << 30, OOMCount: 1, MaxOOMPeak: 10 << 30}, nil
	}
	server.admitPeakP90 = func(context.Context) (int64, bool, error) {
		t.Fatal("pinned request consulted history prior")
		return 0, false, nil
	}
	reserve, basis := server.resolveAdmitReserve(admitRequest{reserve: 4 << 30, signature: "oom"}, 10<<30)
	if reserve <= 10<<30 || basis != "estimate:oom-escalated" {
		t.Fatalf("OOM-at-ceiling reserve=%d basis=%q, want terminally too large", reserve, basis)
	}
	pinned, basis := server.resolveAdmitReserve(admitRequest{reserve: 6 << 30, signature: "oom", pinned: true}, 10<<30)
	if pinned != 6<<30 || basis != "pinned:client" {
		t.Fatalf("pinned reserve=%d basis=%q", pinned, basis)
	}
}

func TestConfineEstimatorP90PriorNotMedianOrFlat(t *testing.T) {
	server := NewServer(Paths{})
	server.stopping = make(chan struct{})
	server.admitPeakHistory = func(context.Context, string) (runner.PeakRSSStats, error) {
		return runner.PeakRSSStats{}, nil
	}
	server.admitPeakP90 = func(context.Context) (int64, bool, error) { return 50 << 30, true, nil }
	reserve, basis := server.resolveAdmitReserve(admitRequest{reserve: 4 << 30, signature: "novel"}, 63<<30)
	want := int64(50<<30) + int64(50<<30)*15/100
	if reserve != want || basis != "estimate:p90-prior" || reserve == 4<<30 {
		t.Fatalf("prior reserve=%d basis=%q want=%d", reserve, basis, want)
	}
}

func TestAdmitConcurrencyScaledHeadroomAndLifetimeCapacity(t *testing.T) {
	server := NewServer(Paths{})
	server.stopping = make(chan struct{})
	server.admitPollInterval = time.Hour
	server.admitReadMemory = func(string) (int64, int64, bool, string) { return 0, 64 << 30, true, "" }
	queue := &sliceQueue{path: "/slice", server: server}
	for index := 0; index < 5; index++ {
		queue.waiters = append(queue.waiters, &admitWaiter{seq: int64(index + 1), reserve: 15 << 30, state: admitQueued, grantedCh: make(chan struct{})})
	}
	server.evaluateAdmitQueue(queue)
	granted := 0
	for _, waiter := range queue.waiters {
		if waiter.state == admitGranted {
			granted++
		}
	}
	if granted != 4 || queue.outstanding != 60<<30 || queue.outstandingJobs != 4 {
		t.Fatalf("granted=%d outstanding=%d jobs=%d", granted, queue.outstanding, queue.outstandingJobs)
	}
	if got := server.admitSliceHeadroom(5); got != 2<<30+5*(64<<20) {
		t.Fatalf("five-job headroom=%d", got)
	}
}

// verifies: the concurrency-scaled per-supervisor headroom term changes the
// admission DECISION (the grant count), not merely the helper formula. With an
// amplified per-supervisor budget, the correct scaled headroom admits one FEWER
// job than any constant/base-only headroom mis-wire would — so this is RED
// against dropping the (outstandingJobs+1) scaling at the evaluateAdmitQueue
// call site (which the coarser 15 GiB / 64 GiB case cannot detect).
func TestAdmitScaledHeadroomDiscriminatesPerSupervisorTerm(t *testing.T) {
	server := NewServer(Paths{})
	server.stopping = make(chan struct{})
	server.admitPollInterval = time.Hour
	server.admitSliceHeadroomBase = 2 << 30
	server.admitSliceHeadroomSupervisor = 2 << 30
	server.admitReadMemory = func(string) (int64, int64, bool, string) { return 0, 64 << 30, true, "" }
	queue := &sliceQueue{path: "/slice", server: server}
	for index := 0; index < 6; index++ {
		queue.waiters = append(queue.waiters, &admitWaiter{seq: int64(index + 1), reserve: 10 << 30, state: admitQueued, grantedCh: make(chan struct{})})
	}
	server.evaluateAdmitQueue(queue)
	granted := 0
	for _, waiter := range queue.waiters {
		if waiter.state == admitGranted {
			granted++
		}
	}
	// Scaled headroom = 2G + jobs*2G admits exactly 5 (the 6th needs 10G but only
	// the slice cap minus a 14G headroom minus 50G outstanding, =0, is free). A
	// base-only or admitSliceHeadroom(1) constant headroom would admit all 6.
	if granted != 5 || queue.outstanding != 50<<30 || queue.outstandingJobs != 5 {
		t.Fatalf("granted=%d outstanding=%d jobs=%d, want scaled-headroom decision (5)", granted, queue.outstanding, queue.outstandingJobs)
	}
}

// verifies: when the slice memory read fails, evaluateAdmitQueue does NOT grant
// queued waiters (fail-closed). Granting them uncounted (no outstanding /
// outstandingJobs) would abandon the Σ(reserve) ≤ cap-headroom invariant and
// re-open the slice-cap random-victim OOM this milestone exists to prevent. The
// waiters stay queued and are granted, accounted, once the read recovers.
func TestAdmitReadFailureKeepsWaitersQueuedUncounted(t *testing.T) {
	server := NewServer(Paths{})
	server.stopping = make(chan struct{})
	server.admitPollInterval = time.Hour
	readable := false
	server.admitReadMemory = func(string) (int64, int64, bool, string) {
		if !readable {
			return 0, 0, false, "unbounded"
		}
		return 0, 64 << 30, true, ""
	}
	queue := &sliceQueue{path: "/slice", server: server}
	for index := 0; index < 3; index++ {
		queue.waiters = append(queue.waiters, &admitWaiter{seq: int64(index + 1), reserve: 10 << 30, state: admitQueued, grantedCh: make(chan struct{})})
	}
	server.evaluateAdmitQueue(queue)
	for _, waiter := range queue.waiters {
		if waiter.state != admitQueued {
			t.Fatalf("read failure granted a waiter uncounted: state=%d", waiter.state)
		}
	}
	if queue.outstanding != 0 || queue.outstandingJobs != 0 {
		t.Fatalf("read-failure accounting outstanding=%d jobs=%d, want zero", queue.outstanding, queue.outstandingJobs)
	}
	readable = true
	server.evaluateAdmitQueue(queue)
	granted := 0
	for _, waiter := range queue.waiters {
		if waiter.state == admitGranted {
			granted++
		}
	}
	if granted != 3 || queue.outstanding != 30<<30 || queue.outstandingJobs != 3 {
		t.Fatalf("post-recovery granted=%d outstanding=%d jobs=%d", granted, queue.outstanding, queue.outstandingJobs)
	}
}

func TestPinnedTooLargeRejectedBeforeQueue(t *testing.T) {
	server := NewServer(Paths{})
	server.stopping = make(chan struct{})
	server.admitResolveSlice = func(string) (string, bool, string) { return "/slice", true, "" }
	server.admitReadMemory = func(string) (int64, int64, bool, string) { return 0, 8 << 30, true, "" }
	serverConn, clientConn := net.Pipe()
	done := make(chan struct{})
	go func() {
		defer close(done)
		defer serverConn.Close()
		server.admitConnection(serverConn, map[string]any{"slice": "slice", "reserve": int64(7 << 30), "max_wait_ms": int64(1), "signature": "sig", "pinned": true})
	}()
	var frame ResponseFrame
	if err := readFrame(clientConn, &frame); err != nil {
		t.Fatal(err)
	}
	if frame.Code != CodeAdmitTooLarge {
		t.Fatalf("frame=%+v", frame)
	}
	var rejection admitRejection
	if err := json.Unmarshal(frame.Data, &rejection); err != nil || rejection.Required != 7<<30 || rejection.Basis != "pinned:client" {
		t.Fatalf("rejection=%+v err=%v", rejection, err)
	}
	_ = clientConn.Close()
	<-done
	server.admitRegistryMu.Lock()
	queues := len(server.admitQueues)
	server.admitRegistryMu.Unlock()
	if queues != 0 {
		t.Fatalf("too-large request created %d queues", queues)
	}
}

func TestConfineReportPersistsKnownAndUnknownPeaks(t *testing.T) {
	dir := t.TempDir()
	db, err := store.OpenDB(filepath.Join(dir, "state.db"), filepath.Join(dir, "registry.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	server := NewServer(Paths{})
	server.db = db
	server.stopping = make(chan struct{})
	known := map[string]any{"signature": "sig", "peak_rss": int64(123), "oom": true}
	unknown := map[string]any{"signature": "sig", "oom": false}
	serverConn, clientConn := net.Pipe()
	done := make(chan struct{})
	go func() {
		defer close(done)
		server.serveConnection(context.Background(), serverConn)
	}()
	if err := writeFrame(clientConn, RequestFrame{Proto: ProtocolVersion, Request: core.Request{Verb: "confine-report", Args: known}}); err != nil {
		t.Fatal(err)
	}
	var frame ResponseFrame
	if err := readFrame(clientConn, &frame); err != nil || !frame.OK {
		t.Fatalf("known frame=%+v err=%v", frame, err)
	}
	_ = clientConn.Close()
	<-done
	if response := server.confineReport(unknown); !response.OK {
		t.Fatalf("unknown response=%+v", response)
	}
	stats, err := db.ConfinePeakHistory(context.Background(), "sig")
	if err != nil || stats.TotalCount != 2 || stats.SampleCount != 1 || stats.PeakMax != 123 || stats.OOMCount != 1 || stats.MaxOOMPeak != 123 {
		t.Fatalf("stats=%+v err=%v", stats, err)
	}
}
