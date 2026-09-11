package daemon

import (
	"context"
	"testing"
	"time"

	"aira/internal/runner"
	"aira/internal/store"
)

// ciDumpTestServer builds the narrowest daemon a confine-dump test needs: a
// real (temp-file) state.db for the peak-history half, and the admission
// registry left nil/empty so a test injects exactly the queue state it wants
// rather than depending on a real cgroup. Mirrors budgetTestServer
// (confine_budget_test.go) and reDeclareTestServer (redeclare_handler_test.go).
func ciDumpTestServer(t *testing.T) *Server {
	t.Helper()
	server, _ := budgetTestServer(t)
	server.admitPollInterval = time.Hour
	return server
}

// injectCIDumpQueue registers a sliceQueue directly (bypassing enqueueAdmitInternal
// and any real cgroup/evaluator), for deterministic per-queue dump assertions.
// The evaluator goroutine is never started, so nothing mutates the queue
// concurrently with the test's own locked reads and the dump's own locked walk.
func injectCIDumpQueue(server *Server, path string, configure func(*sliceQueue)) *sliceQueue {
	queue := &sliceQueue{path: path, kick: make(chan struct{}, 1), stop: make(chan struct{}), stopped: make(chan struct{}), poll: time.Hour, server: server}
	if configure != nil {
		configure(queue)
	}
	server.admitRegistryMu.Lock()
	if server.admitQueues == nil {
		server.admitQueues = map[string]*sliceQueue{}
	}
	server.admitQueues[path] = queue
	server.admitRegistryMu.Unlock()
	return queue
}

// TestConfineDumpRefusesInvalidOwner pins the same owner-validation discipline
// every confine-management verb applies (confineBudget, confineReport, ...).
//
// verifies: AIRA (admission-counter rebuild) S18
func TestConfineDumpRefusesInvalidOwner(t *testing.T) {
	server := ciDumpTestServer(t)
	response := server.confineDump(map[string]any{"owner": "not a valid owner!"})
	if response.OK {
		t.Fatalf("expected a refusal, got %+v", response)
	}
	if response.Code != "E_CONFINE_ARGUMENT_INVALID" {
		t.Fatalf("code = %q, want E_CONFINE_ARGUMENT_INVALID", response.Code)
	}
}

// TestConfineDumpUnavailableWithNoDB pins fail-closed honesty: with no state
// database there is no peak history to read, so the verb must report
// unavailable rather than a confident empty dump.
//
// verifies: AIRA (admission-counter rebuild) S18
func TestConfineDumpUnavailableWithNoDB(t *testing.T) {
	server := NewServer(Paths{})
	response := server.confineDump(map[string]any{"owner": "session-a"})
	if response.OK {
		t.Fatalf("expected a refusal with no db, got %+v", response)
	}
	if response.Code != CodeUnavailable {
		t.Fatalf("code = %q, want %s", response.Code, CodeUnavailable)
	}
}

// TestConfineDumpAdmissionRecordsAreHonestlyUnevaluated is the S18 honesty
// mutation pin (AIRA's hard rule: an unmeasured field is `unevaluated`, never
// a fabricated 0). It exercises both directions: a sample WITH a declared
// reserve/peak must report the real bytes, and one WITHOUT must report a nil
// pointer (JSON null on the wire, per confine_dump_test.go in package
// runner) rather than 0 -- and every row's Outcome/WaitMS, which
// confine_peak_history structurally cannot carry (§12: only confine-report's
// post-run peak_rss/oom, never grant/deny/fail-fast or a wait duration), must
// be the honest sentinel/nil in every case.
//
// A mutant that defaulted an absent *int64 to a zero value, or that invented
// "granted" for Outcome, must turn this test red.
//
// verifies: AIRA (admission-counter rebuild) S18
func TestConfineDumpAdmissionRecordsAreHonestlyUnevaluated(t *testing.T) {
	server, db := budgetTestServer(t)
	server.admitPollInterval = time.Hour
	at := time.Date(2026, 9, 11, 0, 0, 0, 0, time.UTC)
	measured := int64(256 << 20)
	if err := db.RecordConfinePeak(context.Background(), store.ResourcePeakObservation{
		Kind: store.ResourcePeakKindConfine, Signature: "measured-cmd",
		Peak: &measured, Budget: &measured, BudgetBasis: "cap:operator:--memory-reserve",
		OOM: false, At: at,
	}); err != nil {
		t.Fatal(err)
	}
	if err := db.RecordConfinePeak(context.Background(), store.ResourcePeakObservation{
		Kind: store.ResourcePeakKindConfine, Signature: "unmeasured-cmd",
		OOM: true, At: at,
	}); err != nil {
		t.Fatal(err)
	}

	response := server.confineDump(map[string]any{"owner": "session-a"})
	if !response.OK {
		t.Fatalf("response=%+v", response)
	}
	result, ok := response.Data.(runner.ConfineDumpResult)
	if !ok {
		t.Fatalf("Data is %T, want runner.ConfineDumpResult", response.Data)
	}
	if len(result.Admissions) != 2 {
		t.Fatalf("want 2 admission records, got %d: %+v", len(result.Admissions), result.Admissions)
	}
	byline := map[string]runner.ConfineDumpAdmissionRow{}
	for _, row := range result.Admissions {
		byline[row.Signature] = row
	}
	measuredRow, ok := byline["measured-cmd"]
	if !ok {
		t.Fatalf("missing measured-cmd row: %+v", result.Admissions)
	}
	if measuredRow.RecordType != runner.ConfineDumpRecordAdmission {
		t.Fatalf("record_type = %q", measuredRow.RecordType)
	}
	if measuredRow.DeclaredReserveBytes == nil || *measuredRow.DeclaredReserveBytes != measured {
		t.Fatalf("declared reserve = %+v, want %d", measuredRow.DeclaredReserveBytes, measured)
	}
	if measuredRow.ObservedPeakBytes == nil || *measuredRow.ObservedPeakBytes != measured {
		t.Fatalf("observed peak = %+v, want %d", measuredRow.ObservedPeakBytes, measured)
	}
	if measuredRow.Outcome != runner.ConfineDumpUnevaluated {
		t.Fatalf("outcome = %q, want the honest sentinel %q (confine_peak_history carries no admission outcome)", measuredRow.Outcome, runner.ConfineDumpUnevaluated)
	}
	if measuredRow.WaitMS != nil {
		t.Fatalf("wait_ms = %v, want nil (unevaluated): confine_peak_history carries no wait duration", *measuredRow.WaitMS)
	}

	unmeasuredRow, ok := byline["unmeasured-cmd"]
	if !ok {
		t.Fatalf("missing unmeasured-cmd row: %+v", result.Admissions)
	}
	if unmeasuredRow.DeclaredReserveBytes != nil {
		t.Fatalf("declared reserve = %v, want nil (never recorded) -- not a fabricated 0", *unmeasuredRow.DeclaredReserveBytes)
	}
	if unmeasuredRow.ObservedPeakBytes != nil {
		t.Fatalf("observed peak = %v, want nil (OOM truncated it) -- not a fabricated 0", *unmeasuredRow.ObservedPeakBytes)
	}
	if !unmeasuredRow.OOM {
		t.Fatal("oom must be true: this IS a measured, real fact (confine-report requires it)")
	}
}

// TestConfineDumpQueueReportsNegativeAvailableExcursion pins design §12's
// "negative-available excursions (restart-window over-subscription)": a
// queue whose signed ledger has been driven past its ceiling (the design §4
// late re-declare case) must report a NEGATIVE available, never a
// clamped-at-zero or fabricated-false reading.
//
// verifies: AIRA (admission-counter rebuild) S18
func TestConfineDumpQueueReportsNegativeAvailableExcursion(t *testing.T) {
	server := ciDumpTestServer(t)
	// Zeroed headroom, matching reDeclareTestServer's own convention: the
	// default headroom (2GiB+64MiB, admit.go) would otherwise exceed this
	// test's small ceiling and trip checkedAvailable's OWN "degenerate
	// ceiling" 0 branch (maximum <= headroom), which is unrelated to what
	// this test pins.
	server.admitSliceHeadroomBase = 0
	server.admitSliceHeadroomSupervisor = 0
	const ceiling = int64(1) << 30 // 1 GiB
	server.admitReadMemory = func(string) (int64, int64, int64, bool, string) {
		return 0, ceiling, 0, true, ""
	}
	// Simulate the signed ledger already over its ceiling (e.g. a late S9
	// re-declare landed after new admissions committed -- design §4/§2: this
	// is a legitimate, non-erroneous state, not a bug to clamp away).
	injectCIDumpQueue(server, "/aira.slice", func(queue *sliceQueue) {
		queue.outstanding = ceiling + (256 << 20)
	})

	response := server.confineDump(map[string]any{"owner": "session-a"})
	if !response.OK {
		t.Fatalf("response=%+v", response)
	}
	result := response.Data.(runner.ConfineDumpResult)
	if len(result.Queues) != 1 {
		t.Fatalf("want 1 queue record, got %d: %+v", len(result.Queues), result.Queues)
	}
	row := result.Queues[0]
	if row.RecordType != runner.ConfineDumpRecordQueue {
		t.Fatalf("record_type = %q", row.RecordType)
	}
	if row.Slice != "/aira.slice" {
		t.Fatalf("slice = %q", row.Slice)
	}
	if row.AvailableBytes == nil {
		t.Fatal("available_bytes must be established: the memory reader succeeded")
	}
	if *row.AvailableBytes >= 0 {
		t.Fatalf("available = %d, want negative (outstanding %d exceeds ceiling %d)", *row.AvailableBytes, queueOutstandingFor(server, "/aira.slice"), ceiling)
	}
	if !row.NegativeAvailable {
		t.Fatal("negative_available must be true")
	}
	if row.RAMOutstandingBytes != ceiling+(256<<20) {
		t.Fatalf("ram_outstanding_bytes = %d", row.RAMOutstandingBytes)
	}
}

// TestConfineDumpQueueLeavesAvailableUnevaluatedOnADegenerateCeiling pins the
// advisor-flagged case checkedAvailable's own doc comment names: a ceiling at
// or below the configured headroom is an UNUSABLE reading (not "zero
// available"), and checkedAvailable/ledgerAvailable both collapse that case
// to the SAME 0 a genuinely-exhausted slice would produce -- so this dump
// must not trust that 0 at face value. The ceiling figure itself, in
// contrast, IS still a real, established reading and must still be reported.
//
// verifies: AIRA (admission-counter rebuild) S18
func TestConfineDumpQueueLeavesAvailableUnevaluatedOnADegenerateCeiling(t *testing.T) {
	server := ciDumpTestServer(t)
	// Default headroom (2GiB+64MiB, admit.go) intentionally left in place and
	// deliberately NOT zeroed here (unlike the negative-available test above):
	// a 1GiB ceiling is smaller than it, which is exactly the degenerate case.
	const ceiling = int64(1) << 30
	server.admitReadMemory = func(string) (int64, int64, int64, bool, string) {
		return 0, ceiling, 0, true, ""
	}
	injectCIDumpQueue(server, "/aira.slice", nil)

	response := server.confineDump(map[string]any{"owner": "session-a"})
	if !response.OK {
		t.Fatalf("response=%+v", response)
	}
	row := response.Data.(runner.ConfineDumpResult).Queues[0]
	if row.RAMCeilingBytes == nil || *row.RAMCeilingBytes != ceiling {
		t.Fatalf("ram_ceiling_bytes = %+v, want %d (still a real reading)", row.RAMCeilingBytes, ceiling)
	}
	if row.AvailableBytes != nil {
		t.Fatalf("available_bytes = %v, want nil: the ceiling is below the configured headroom, an unusable reading -- not a fabricated 0", *row.AvailableBytes)
	}
	if row.NegativeAvailable {
		t.Fatal("negative_available must stay false when availability could not be established (a fabricated true is exactly as dishonest as a fabricated false)")
	}
}

func queueOutstandingFor(server *Server, path string) int64 {
	server.admitRegistryMu.Lock()
	defer server.admitRegistryMu.Unlock()
	queue := server.admitQueues[path]
	if queue == nil {
		return 0
	}
	queue.mu.Lock()
	defer queue.mu.Unlock()
	return queue.outstanding
}

// TestConfineDumpQueueOldestBlockedWait pins design §12's "oldest-blocked
// wait" using the SAME clock seam the admission evaluator itself uses
// (admitNowTime), so the figure is a real, deterministic measurement rather
// than a flaky wall-clock read.
//
// verifies: AIRA (admission-counter rebuild) S18
func TestConfineDumpQueueOldestBlockedWait(t *testing.T) {
	server := ciDumpTestServer(t)
	now := time.Date(2026, 9, 11, 12, 0, 0, 0, time.UTC)
	server.admitNow = func() time.Time { return now }
	server.admitReadMemory = func(string) (int64, int64, int64, bool, string) {
		return 0, 0, 0, false, "no memory reader configured for this test"
	}
	injectCIDumpQueue(server, "/aira.slice", func(queue *sliceQueue) {
		queue.waiters = []*admitWaiter{
			{state: admitQueued, enqueued: now.Add(-30 * time.Second), reserve: 1 << 20},
			{state: admitQueued, enqueued: now.Add(-5 * time.Second), reserve: 1 << 20},
			{state: admitGranted, accounted: true, enqueued: now.Add(-90 * time.Second), reserve: 4 << 20},
		}
	})

	response := server.confineDump(map[string]any{"owner": "session-a"})
	if !response.OK {
		t.Fatalf("response=%+v", response)
	}
	result := response.Data.(runner.ConfineDumpResult)
	if len(result.Queues) != 1 {
		t.Fatalf("want 1 queue record, got %d", len(result.Queues))
	}
	row := result.Queues[0]
	if row.QueuedCount != 2 {
		t.Fatalf("queued_count = %d, want 2 (the granted waiter must not be counted)", row.QueuedCount)
	}
	if row.OldestBlockedWaitMS == nil {
		t.Fatal("oldest_blocked_wait_ms must be established: 2 waiters are queued")
	}
	if *row.OldestBlockedWaitMS != 30_000 {
		t.Fatalf("oldest_blocked_wait_ms = %d, want 30000 (the 30s-queued waiter, not the 90s-old GRANTED one)", *row.OldestBlockedWaitMS)
	}
}

// TestConfineDumpQueueWithNoWaitersLeavesOldestWaitUnevaluated pins the
// converse of the above: a queue nobody is blocked on must report
// oldest_blocked_wait_ms absent, never a fabricated 0 standing in for "nobody
// is waiting" vs. "somebody waited exactly 0ms".
//
// verifies: AIRA (admission-counter rebuild) S18
func TestConfineDumpQueueWithNoWaitersLeavesOldestWaitUnevaluated(t *testing.T) {
	server := ciDumpTestServer(t)
	server.admitReadMemory = func(string) (int64, int64, int64, bool, string) {
		return 0, 0, 0, false, "no memory reader configured for this test"
	}
	injectCIDumpQueue(server, "/aira.slice", nil)

	response := server.confineDump(map[string]any{"owner": "session-a"})
	if !response.OK {
		t.Fatalf("response=%+v", response)
	}
	result := response.Data.(runner.ConfineDumpResult)
	row := result.Queues[0]
	if row.QueuedCount != 0 {
		t.Fatalf("queued_count = %d, want 0", row.QueuedCount)
	}
	if row.OldestBlockedWaitMS != nil {
		t.Fatalf("oldest_blocked_wait_ms = %v, want nil (nobody is queued)", *row.OldestBlockedWaitMS)
	}
	if row.AvailableBytes != nil {
		t.Fatalf("available_bytes = %v, want nil: the memory reader was configured to fail", *row.AvailableBytes)
	}
	if row.NegativeAvailable {
		t.Fatal("negative_available must stay false when availability could not be established")
	}
}

// TestConfineDumpQueueReportsCPUCeiling pins the per-resource (CPU)
// utilisation half of design §12, using the cpuCoreCounter seam so the
// ceiling is deterministic.
//
// verifies: AIRA (admission-counter rebuild) S18
func TestConfineDumpQueueReportsCPUCeiling(t *testing.T) {
	server := ciDumpTestServer(t)
	server.readCPUCores = func() int { return 4 }
	server.admitReadMemory = func(string) (int64, int64, int64, bool, string) {
		return 0, 0, 0, false, "no memory reader configured for this test"
	}
	injectCIDumpQueue(server, "/aira.slice", func(queue *sliceQueue) {
		queue.cpuOutstanding = 3
	})

	response := server.confineDump(map[string]any{"owner": "session-a"})
	if !response.OK {
		t.Fatalf("response=%+v", response)
	}
	result := response.Data.(runner.ConfineDumpResult)
	row := result.Queues[0]
	if row.CPUCeilingCores != 8 {
		t.Fatalf("cpu_ceiling_cores = %d, want 8 (2x4)", row.CPUCeilingCores)
	}
	if row.CPUOutstandingCores != 3 {
		t.Fatalf("cpu_outstanding_cores = %d, want 3", row.CPUOutstandingCores)
	}
}

// TestConfineDumpWritesNoQuota asserts confine-dump is a pure reader: it must
// not write to the database at all (it is layered strictly under
// confine-report/confine-budget's own read paths). Mirrors the write-count
// assertion pattern resource_budget_guard_test.go already uses for Face 2.
//
// verifies: AIRA (admission-counter rebuild) S18
func TestConfineDumpWritesNoQuota(t *testing.T) {
	server, db := budgetTestServer(t)
	server.admitPollInterval = time.Hour
	if err := db.RecordConfinePeak(context.Background(), store.ResourcePeakObservation{
		Kind: store.ResourcePeakKindConfine, Signature: "x", OOM: false, At: time.Now(),
	}); err != nil {
		t.Fatal(err)
	}
	before := store.TotalRowChanges(db)
	if response := server.confineDump(map[string]any{"owner": "session-a"}); !response.OK {
		t.Fatalf("response=%+v", response)
	}
	after := store.TotalRowChanges(db)
	if before < 0 || after < 0 {
		t.Fatalf("row-change count unevaluated: before=%d after=%d", before, after)
	}
	if after != before {
		t.Fatalf("confine-dump wrote to the database: total_changes %d -> %d", before, after)
	}
}
