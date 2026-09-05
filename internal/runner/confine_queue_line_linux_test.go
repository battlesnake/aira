//go:build linux

package runner

import (
	"bytes"
	"context"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"aira/internal/testdeadline"
)

// waitForConfineDiagnostic runs a confine launch whose admission blocks until
// the returned release function is called, and returns everything the launch
// wrote to stderr once at least one progress line has appeared.
func waitForConfineDiagnostic(t *testing.T, deps confineDeps, request ConfineRequest) string {
	t.Helper()
	var mu sync.Mutex
	var buf bytes.Buffer
	sink := writerFunc(func(p []byte) (int, error) {
		mu.Lock()
		defer mu.Unlock()
		return buf.Write(p)
	})
	request.Stderr = sink
	request.SelfPath = os.Args[0]
	if len(request.Argv) == 0 {
		request.Argv = []string{"/bin/true"}
	}
	if request.Slice == "" {
		request.Slice = "finite.slice"
	}
	proceed := make(chan struct{})
	inner := deps.admit
	deps.admit = func(ctx context.Context, path string, req ConfineRequest, reserve int64) (admissionResult, error) {
		<-proceed // block, as a reserve-contended daemon wait would
		if inner != nil {
			return inner(ctx, path, req, reserve)
		}
		return admissionResult{state: "waited"}, nil
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _ = confineWithDeps(context.Background(), request, deps)
	}()
	deadline := time.Now().Add(testdeadline.Wait(5 * time.Second))
	got := ""
	for time.Now().Before(deadline) {
		mu.Lock()
		got = buf.String()
		mu.Unlock()
		if strings.Contains(got, "waiting for memory admission") {
			break
		}
		time.Sleep(2 * time.Millisecond)
	}
	close(proceed)
	<-done
	mu.Lock()
	defer mu.Unlock()
	return buf.String()
}

// AIRA-24. The dogfooded complaint was a 30-minute blind wait: the progress
// line proved the job was alive but said nothing about whether it was next or
// last, so "wait" and "give up" were indistinguishable decisions. The line
// carries the waiter's own position and the reserve queued ahead of it
// whenever the daemon can establish them.
//
// verifies: the admission-wait progress line reports the caller's queue
// position, the queue size, and the bytes reserved ahead of it.
func TestConfineAdmissionWaitLineCarriesTheQueuePosition(t *testing.T) {
	scope := &confineFakeScope{}
	deps := confineUnitDeps(scope)
	deps.admitWaitDiagInterval = 5 * time.Millisecond
	var asked atomic.Int64
	deps.queuePosition = func(_ context.Context, request ConfineRequest, slicePath string) (confineQueuePosition, bool) {
		asked.Add(1)
		if request.ScopeID == "" {
			t.Error("the probe must be given the job's own scope id")
		}
		if slicePath != "/fake/finite.slice" {
			t.Errorf("probe slice=%q, want the resolved slice path", slicePath)
		}
		return confineQueuePosition{position: 2, queued: 3, aheadBytes: 8 << 30}, true
	}
	got := waitForConfineDiagnostic(t, deps, ConfineRequest{MemoryReserve: 4 << 30, MemoryReservePinned: true})
	if !strings.Contains(got, "queue position 2 of 3 by enqueue order") {
		t.Fatalf("progress line does not state the caller's own place in the queue; stderr=%q", got)
	}
	// "queued ahead", not "reserved ahead": the figure counts the queued
	// waiters in front, NOT the much larger reserve already granted to running
	// jobs. Build review found the looser wording invited exactly that misread.
	if !strings.Contains(got, "8G queued ahead") {
		t.Fatalf("progress line does not state the reserve queued ahead; stderr=%q", got)
	}
	if strings.Contains(got, "reserved ahead") {
		t.Fatalf("the ahead-figure must not read as all reserve ahead of the job; stderr=%q", got)
	}
	if !strings.Contains(got, "reserve 4G") {
		t.Fatalf("the existing pinned wording must survive; stderr=%q", got)
	}
	if asked.Load() == 0 {
		t.Fatal("the position was never asked for")
	}
}

// The unpinned line keeps its AIRA-51 hedge — the queue note is additional
// information, never a replacement for saying the figure is a request hint.
//
// verifies: the unpinned progress line carries both the hedge and the position.
func TestConfineAdmissionWaitLineKeepsTheUnpinnedHedgeWithAPosition(t *testing.T) {
	scope := &confineFakeScope{}
	deps := confineUnitDeps(scope)
	deps.admitWaitDiagInterval = 5 * time.Millisecond
	deps.queuePosition = func(context.Context, ConfineRequest, string) (confineQueuePosition, bool) {
		return confineQueuePosition{position: 1, queued: 4, aheadBytes: 0}, true
	}
	got := waitForConfineDiagnostic(t, deps, ConfineRequest{})
	if !strings.Contains(got, "unpinned") || !strings.Contains(got, "requested reserve 4G") {
		t.Fatalf("unpinned hedge lost; stderr=%q", got)
	}
	if !strings.Contains(got, "queue position 1 of 4") {
		t.Fatalf("unpinned line lost its queue position; stderr=%q", got)
	}
	// Head of the queue: nothing is ahead, and that is a fact worth stating as
	// 0B rather than omitting, since the absent case means something else.
	if !strings.Contains(got, "0B queued ahead") {
		t.Fatalf("head-of-queue line must state an empty ahead-figure; stderr=%q", got)
	}
}

// A position the daemon cannot establish (daemon down, older daemon, this
// scope not queued) must leave the line EXACTLY as it was. The absence of a
// position is not "position unknown" noise on every tick, and it is certainly
// never a fabricated zero — printing "queue position 0 of 0" during a real
// wait would state that nothing is queued while this very job waits.
//
// verifies: an unestablished position removes the queue note entirely and
// changes nothing else about the line.
func TestConfineAdmissionWaitLineOmitsAnUnestablishedPosition(t *testing.T) {
	scope := &confineFakeScope{}
	deps := confineUnitDeps(scope)
	deps.admitWaitDiagInterval = 5 * time.Millisecond
	deps.queuePosition = func(context.Context, ConfineRequest, string) (confineQueuePosition, bool) {
		return confineQueuePosition{}, false
	}
	got := waitForConfineDiagnostic(t, deps, ConfineRequest{MemoryReserve: 4 << 30, MemoryReservePinned: true})
	if !strings.Contains(got, "confine: waiting for memory admission on finite.slice (reserve 4G, waited ") {
		t.Fatalf("the daemon-less line must be exactly the pre-AIRA-24 line; stderr=%q", got)
	}
	if strings.Contains(got, "queue position") || strings.Contains(got, "queued ahead") {
		t.Fatalf("an unestablished position must print nothing at all; stderr=%q", got)
	}
}

// The probe takes time, and the grant can land inside that window. A "still
// waiting" line composed before the grant and printed after it states
// something that stopped being true while it was being written — the same
// staleness class this whole feature exists to remove, reintroduced at the
// last step. Raised by the DeepSeek build-review lineage.
//
// verifies: when admission is granted while a probe is in flight, that tick
// prints no line at all.
func TestConfineAdmissionWaitLineIsNotPrintedAfterTheGrant(t *testing.T) {
	scope := &confineFakeScope{}
	deps := confineUnitDeps(scope)
	deps.admitWaitDiagInterval = 5 * time.Millisecond
	deps.admitQueueProbeTimeout = 30 * time.Second
	probing := make(chan struct{})
	var once sync.Once
	// The probe holds the tick open until its context is cancelled, and the
	// only thing that cancels it here is the end of the admission wait itself
	// — so this stub returning is PROOF that the grant already landed. No
	// sleeps and no cross-goroutine timing assumption.
	deps.queuePosition = func(ctx context.Context, _ ConfineRequest, _ string) (confineQueuePosition, bool) {
		once.Do(func() { close(probing) })
		<-ctx.Done()
		return confineQueuePosition{position: 2, queued: 3, aheadBytes: 8 << 30}, true
	}
	var mu sync.Mutex
	var buf bytes.Buffer
	sink := writerFunc(func(p []byte) (int, error) {
		mu.Lock()
		defer mu.Unlock()
		return buf.Write(p)
	})
	proceed := make(chan struct{})
	deps.admit = func(context.Context, string, ConfineRequest, int64) (admissionResult, error) {
		<-proceed
		return admissionResult{state: "waited"}, nil
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _ = confineWithDeps(context.Background(), ConfineRequest{
			Slice: "finite.slice", MemoryReserve: 4 << 30, MemoryReservePinned: true,
			Argv: []string{"/bin/true"}, SelfPath: os.Args[0], Stderr: sink,
		}, deps)
	}()
	select {
	case <-probing:
	case <-testdeadline.After(5 * time.Second):
		close(proceed)
		<-done
		t.Fatal("the probe was never attempted")
	}
	// Admission is granted while the probe is still in flight; the probe then
	// answers with a perfectly valid position, too late to be worth printing.
	close(proceed)
	select {
	case <-done:
	case <-testdeadline.After(10 * time.Second):
		t.Fatal("the launch never finished")
	}
	mu.Lock()
	got := buf.String()
	mu.Unlock()
	if strings.Contains(got, "waiting for memory admission") {
		t.Fatalf("a wait line was printed after admission was granted; stderr=%q", got)
	}
}

// The probe must never become a second thing the launch waits for: a daemon
// that accepts the query and never answers it must not hold the job past its
// own grant. The wait-end cancels the in-flight probe, and the diagnostic
// goroutine is still joined before the launch continues.
//
// verifies: a probe that only returns when its context is cancelled does not
// delay the launch beyond the grant.
func TestConfineAdmissionWaitProbeIsCancelledByTheGrant(t *testing.T) {
	scope := &confineFakeScope{}
	deps := confineUnitDeps(scope)
	deps.admitWaitDiagInterval = 5 * time.Millisecond
	// Far above this test's own patience: if the launch finishes promptly it is
	// because the GRANT cancelled the probe, never because the probe's own
	// timeout ran out.
	deps.admitQueueProbeTimeout = 30 * time.Second
	entered := make(chan struct{})
	var once sync.Once
	deps.queuePosition = func(ctx context.Context, _ ConfineRequest, _ string) (confineQueuePosition, bool) {
		once.Do(func() { close(entered) })
		<-ctx.Done() // a wedged daemon: answers only when the wait ends
		return confineQueuePosition{}, false
	}
	var mu sync.Mutex
	var buf bytes.Buffer
	sink := writerFunc(func(p []byte) (int, error) {
		mu.Lock()
		defer mu.Unlock()
		return buf.Write(p)
	})
	proceed := make(chan struct{})
	deps.admit = func(context.Context, string, ConfineRequest, int64) (admissionResult, error) {
		<-proceed
		return admissionResult{state: "waited"}, nil
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _ = confineWithDeps(context.Background(), ConfineRequest{
			Slice: "finite.slice", MemoryReserve: 4 << 30, MemoryReservePinned: true,
			Argv: []string{"/bin/true"}, SelfPath: os.Args[0], Stderr: sink,
		}, deps)
	}()
	select {
	case <-entered:
	case <-testdeadline.After(5 * time.Second):
		close(proceed)
		<-done
		t.Fatal("the probe was never attempted")
	}
	close(proceed)
	select {
	case <-done:
	case <-testdeadline.After(5 * time.Second):
		t.Fatal("the launch was held by an in-flight queue-position probe")
	}
}
