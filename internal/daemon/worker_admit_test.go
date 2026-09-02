package daemon

import (
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"aira/internal/runner"
)

// admitReadMemoryFixture stands in for readSliceMemory. evaluateWorkerAdmit
// now reads ONLY the OUTER scope's own live memory.current (hierarchical:
// already includes the supervisor plus every placed worker, spec 3.3) — it
// no longer sums per-worker grants separately (that summation both
// double-counted against, and could still under-count relative to, what
// the kernel's own memory.oom.group actually acts on). So this fixture
// answers any outer_scope path uniformly against current[path], defaulting
// to 0 (an idle scope) when unset, always readable.
func admitReadMemoryFixture(current map[string]int64, outerMax int64) func(string) (int64, int64, int64, bool, string) {
	return func(path string) (int64, int64, int64, bool, string) {
		return current[path], outerMax, 0, true, ""
	}
}

// admitReadWorkerSupervisorMemoryFixture stands in for
// readWorkerSupervisorMemory (the aggregate guard's supervisor-scope read,
// a SEPARATE seam from admitReadMemoryFixture above): unlike the outer-scope
// read, this one carries no memory.max/ceiling at all, matching the real
// supervisor scope's deliberately-uncapped memory.max ("max") — see
// readWorkerSupervisorMemory's own doc comment for why reusing the
// outer-scope reader here was a real bug (AIRA-38).
func admitReadWorkerSupervisorMemoryFixture(current map[string]int64) func(string) (int64, int64, bool, string) {
	return func(path string) (int64, int64, bool, string) {
		return current[path], 0, true, ""
	}
}

func TestValidateWorkerAdmitArgsClampsMaxWait(t *testing.T) {
	base := map[string]any{
		"job_id": "job-1", "outer_scope": "/outer", "estimated_bytes": workerAdmitEstimatedBytesMin,
	}
	for _, test := range []struct {
		name string
		wait int64
		want int64
	}{
		{name: "over cap", wait: admitWaitCapMs + 1, want: admitWaitCapMs},
		{name: "negative", wait: -1, want: 0},
	} {
		t.Run(test.name, func(t *testing.T) {
			args := make(map[string]any, len(base)+1)
			for key, value := range base {
				args[key] = value
			}
			args["max_wait_ms"] = test.wait
			request, err := validateWorkerAdmitArgs(args)
			if err != nil || request.maxWaitMS != test.want {
				t.Fatalf("request=%+v err=%v, want maxWaitMS=%d", request, err, test.want)
			}
		})
	}
}

func TestValidateWorkerAdmitArgsRejectsBelowMinimumEstimatedBytes(t *testing.T) {
	// Keep the wire protocol aligned with --memory-reserve: smaller values
	// can page-floor memory.max to zero and OOM a worker before it runs.
	base := map[string]any{
		"job_id": "job-1", "outer_scope": "/outer", "max_wait_ms": int64(0),
	}
	for _, estimated := range []int64{1, workerAdmitEstimatedBytesMin - 1} {
		args := make(map[string]any, len(base)+1)
		for key, value := range base {
			args[key] = value
		}
		args["estimated_bytes"] = estimated
		if _, err := validateWorkerAdmitArgs(args); err == nil {
			t.Fatalf("estimated_bytes=%d accepted below 1 MiB minimum", estimated)
		}
	}
	args := make(map[string]any, len(base)+1)
	for key, value := range base {
		args[key] = value
	}
	args["estimated_bytes"] = workerAdmitEstimatedBytesMin
	request, err := validateWorkerAdmitArgs(args)
	if err != nil || request.estimatedBytes != workerAdmitEstimatedBytesMin {
		t.Fatalf("request=%+v err=%v, want exact 1 MiB boundary accepted", request, err)
	}
}

func TestValidateWorkerAdmitArgsParsesAllFields(t *testing.T) {
	args := map[string]any{
		"job_id": "job-123", "outer_scope": "/outer/scope", "signature": "suite:abc123",
		"estimated_bytes": int64(4 * workerAdmitEstimatedBytesMin), "max_wait_ms": int64(1234),
	}
	request, err := validateWorkerAdmitArgs(args)
	if err != nil {
		t.Fatal(err)
	}
	if request.jobID != "job-123" || request.outerScope != "/outer/scope" || request.signature != "suite:abc123" || request.estimatedBytes != 4*workerAdmitEstimatedBytesMin || request.maxWaitMS != 1234 {
		t.Fatalf("request=%+v, want every valid wire field preserved", request)
	}
}

func TestValidateWorkerAdmitArgsRejectsInvalidRequiredFields(t *testing.T) {
	base := map[string]any{
		"job_id": "job-1", "outer_scope": "/outer", "estimated_bytes": workerAdmitEstimatedBytesMin, "max_wait_ms": int64(0),
	}
	for _, test := range []struct {
		name      string
		change    func(map[string]any)
		wantField string
	}{
		{name: "missing job ID", change: func(args map[string]any) { delete(args, "job_id") }, wantField: "job_id"},
		{name: "missing outer scope", change: func(args map[string]any) { delete(args, "outer_scope") }, wantField: "outer_scope"},
		{name: "missing estimated bytes", change: func(args map[string]any) { delete(args, "estimated_bytes") }, wantField: "estimated_bytes"},
		{name: "non-string job ID", change: func(args map[string]any) { args["job_id"] = float64(1) }, wantField: "job_id"},
		{name: "non-string outer scope", change: func(args map[string]any) { args["outer_scope"] = int64(1) }, wantField: "outer_scope"},
		{name: "zero estimated bytes", change: func(args map[string]any) { args["estimated_bytes"] = int64(0) }, wantField: "estimated_bytes"},
		{name: "negative estimated bytes", change: func(args map[string]any) { args["estimated_bytes"] = int64(-1) }, wantField: "estimated_bytes"},
		{name: "estimated bytes above maximum", change: func(args map[string]any) { args["estimated_bytes"] = admitMaxReserve + 1 }, wantField: "estimated_bytes"},
		// 1e30 is intentionally far beyond math.MaxInt64: rejection proves
		// the float64 wire value cannot silently wrap or truncate into a
		// plausible small reserve before the upper-bound check sees it.
		{name: "overflowing estimated bytes float", change: func(args map[string]any) { args["estimated_bytes"] = 1e30 }, wantField: "estimated_bytes"},
	} {
		t.Run(test.name, func(t *testing.T) {
			args := make(map[string]any, len(base))
			for key, value := range base {
				args[key] = value
			}
			test.change(args)
			if _, err := validateWorkerAdmitArgs(args); err == nil || !strings.Contains(err.Error(), test.wantField) {
				t.Fatalf("args=%v err=%v, want rejection mentioning %q", args, err, test.wantField)
			}
		})
	}
}

func TestEvaluateWorkerAdmitGrantsWithinHeadroom(t *testing.T) {
	server := NewServer(Paths{})
	server.admitReadMemory = admitReadMemoryFixture(map[string]int64{}, 1000)
	server.admitReadWorkerSupervisorMemory = admitReadWorkerSupervisorMemoryFixture(map[string]int64{})
	server.workerAdmitHeadroom = 0
	response := server.evaluateWorkerAdmit(workerAdmitRequest{jobID: "job-1", outerScope: "/outer", estimatedBytes: 400, maxWaitMS: 0})
	if response.State != "granted" || response.WorkerID == "" || response.MemoryMax != 400 || response.MemoryHigh != 320 {
		t.Fatalf("response=%+v", response)
	}
	// Pin the invariant a real deployment depends on: the daemon computes
	// this scope path with WorkerScopeChildPath(outer, "worker-"+id), and
	// Task 7's CreateWorkerScope independently computes the SAME path via
	// backend.Create(ctx, "worker-"+id) + WorkerScopeChildPath — both sides
	// must derive from the identical id string, or the client creates a
	// scope the daemon then can't find to read memory.current from.
	if want := runner.WorkerScopeChildPath("/outer", "worker-"+response.WorkerID); response.ScopePath != want {
		t.Fatalf("ScopePath=%q want %q (daemon/client path convention diverged)", response.ScopePath, want)
	}
}

func TestEvaluateWorkerAdmitDeniesWhenAggregateCapsWouldExceedCeiling(t *testing.T) {
	// CORRECTED by build-review: an earlier version of this test asserted
	// the OPPOSITE as correct -- that a low live-usage reading alone was
	// enough to grant a second 700-byte worker under a 1000-byte ceiling
	// even with 700 already committed to a sibling. That is a real
	// aggregate-OOM hazard: if both workers later grow to their own full
	// caps simultaneously, their sum (1400) exceeds the outer ceiling
	// (1000), and the outer scope's own memory.oom.group can then kill the
	// whole run -- supervisor and every sibling worker, not just the one
	// that grew -- directly contradicting the design spec's Goal 2
	// ("a leaking or mis-annotated test cannot threaten a sibling worker
	// or the run as a whole"). evaluateWorkerAdmit now guards the WORST
	// CASE (sum of already-granted memory.max, not just current live
	// usage) alongside the live-usage check.
	server := NewServer(Paths{})
	server.workerAdmitHeadroom = 0
	live := map[string]int64{}
	server.admitReadMemory = admitReadMemoryFixture(live, 1000)
	server.admitReadWorkerSupervisorMemory = admitReadWorkerSupervisorMemoryFixture(map[string]int64{})
	first := server.evaluateWorkerAdmit(workerAdmitRequest{jobID: "job-1", outerScope: "/outer", estimatedBytes: 700, maxWaitMS: 0})
	if first.State != "granted" {
		t.Fatalf("first=%+v", first)
	}
	// Live usage on the outer scope stays low (both workers just started) --
	// the live-usage check alone would admit this, but 700+700 > 1000 means
	// the aggregate guard must deny it regardless.
	live["/outer"] = 100
	second := server.evaluateWorkerAdmit(workerAdmitRequest{jobID: "job-1", outerScope: "/outer", estimatedBytes: 700, maxWaitMS: 0})
	if second.State != "denied" || second.Reason != "fallback:aggregate-cap-exceeded" {
		t.Fatalf("second (aggregate-cap-guarded) =%+v", second)
	}
	// The denial is pollable, not permanent: releasing the first grant
	// frees its committed share, and the identical request now fits.
	server.releaseWorkerGrant("job-1", "/outer", first.WorkerID)
	third := server.evaluateWorkerAdmit(workerAdmitRequest{jobID: "job-1", outerScope: "/outer", estimatedBytes: 700, maxWaitMS: 0})
	if third.State != "granted" {
		t.Fatalf("third (after release) =%+v", third)
	}
}

func TestEvaluateWorkerAdmitAggregateGuardAccountsForSupervisorRSS(t *testing.T) {
	// Found by a second review round: the first version of the aggregate
	// guard summed only worker memory.max caps, entirely omitting the
	// supervisor's own live footprint -- a warm-imported pytest
	// supervisor (spec 3.1/3.2: COW-shared interpreter state is the whole
	// design premise) can routinely hold hundreds of MiB, far more than
	// the default 64MiB headroom alone budgets for. Even the VERY FIRST
	// grant (committed == 0, so the old check trivially passed) must be
	// denied here once the supervisor's own usage is accounted for.
	server := NewServer(Paths{})
	server.workerAdmitHeadroom = 0
	server.admitReadMemory = admitReadMemoryFixture(map[string]int64{}, 1000)
	supervisorLive := map[string]int64{runner.WorkerScopeChildPath("/outer", "supervisor"): 400}
	server.admitReadWorkerSupervisorMemory = admitReadWorkerSupervisorMemoryFixture(supervisorLive)
	// 700 alone fits under the outer ceiling (1000) and under the old
	// committed-only guard (committed=0 before any grant) -- but
	// supervisor(400)+700=1100 > 1000, so this must now be denied.
	denied := server.evaluateWorkerAdmit(workerAdmitRequest{jobID: "job-1", outerScope: "/outer", estimatedBytes: 700, maxWaitMS: 0})
	if denied.State != "denied" || denied.Reason != "fallback:aggregate-cap-exceeded" {
		t.Fatalf("denied (supervisor-rss-guarded) =%+v", denied)
	}
	// A request that fits alongside the supervisor's footprint is granted.
	granted := server.evaluateWorkerAdmit(workerAdmitRequest{jobID: "job-1", outerScope: "/outer", estimatedBytes: 500, maxWaitMS: 0})
	if granted.State != "granted" {
		t.Fatalf("granted (fits alongside supervisor rss) =%+v", granted)
	}
}

func TestEvaluateWorkerAdmitDiscountsReclaimableOuterCache(t *testing.T) {
	// 900 bytes of raw outer memory.current would reject a 500-byte request
	// under a 1000-byte ceiling. Of that, 500 bytes are reclaimable file
	// cache, leaving 400 bytes of effective use and 100 bytes of real room
	// after the request; admission must not treat cache as pinned RSS.
	server := NewServer(Paths{})
	server.workerAdmitHeadroom = 0
	server.admitReadMemory = func(path string) (int64, int64, int64, bool, string) {
		if path == "/outer" {
			return 900, 1000, 500, true, ""
		}
		return 0, 1000, 0, true, ""
	}
	server.admitReadWorkerSupervisorMemory = admitReadWorkerSupervisorMemoryFixture(map[string]int64{})
	response := server.evaluateWorkerAdmit(workerAdmitRequest{jobID: "job-1", outerScope: "/outer", estimatedBytes: 500})
	if response.State != "granted" {
		t.Fatalf("response=%+v, want reclaimable outer cache discounted", response)
	}
}

func TestEvaluateWorkerAdmitDiscountsReclaimableSupervisorCache(t *testing.T) {
	// Mirror the aggregate supervisor-RSS guard with a page-cache-heavy
	// supervisor: raw 900 + request 500 would deny, while the 500 bytes of
	// reclaimable cache leave effective supervisor use of 400 and permit it.
	server := NewServer(Paths{})
	server.workerAdmitHeadroom = 0
	supervisorScope := runner.WorkerScopeChildPath("/outer", "supervisor")
	server.admitReadMemory = admitReadMemoryFixture(map[string]int64{}, 1000)
	server.admitReadWorkerSupervisorMemory = func(path string) (int64, int64, bool, string) {
		if path == supervisorScope {
			return 900, 500, true, ""
		}
		return 0, 0, true, ""
	}
	response := server.evaluateWorkerAdmit(workerAdmitRequest{jobID: "job-1", outerScope: "/outer", estimatedBytes: 500})
	if response.State != "granted" {
		t.Fatalf("response=%+v, want reclaimable supervisor cache discounted", response)
	}
}

func TestEvaluateWorkerAdmitFloorsReclaimableDiscountAtZero(t *testing.T) {
	// A malformed or racing stat read can report more reclaimable cache than
	// memory.current. subtractFloor must floor effective usage at zero rather
	// than turning the subtraction into invented negative memory headroom.
	server := NewServer(Paths{})
	server.workerAdmitHeadroom = 0
	server.admitReadMemory = func(path string) (int64, int64, int64, bool, string) {
		if path == "/outer" {
			return 100, 1000, 500, true, ""
		}
		return 0, 1000, 0, true, ""
	}
	server.admitReadWorkerSupervisorMemory = admitReadWorkerSupervisorMemoryFixture(map[string]int64{})
	response := server.evaluateWorkerAdmit(workerAdmitRequest{jobID: "job-1", outerScope: "/outer", estimatedBytes: 1000})
	if response.State != "granted" {
		t.Fatalf("response=%+v, want reclaimable discount floored at zero", response)
	}
}

func TestEvaluateWorkerAdmitReturnsUnevaluatedWhenSupervisorScopeUnreadable(t *testing.T) {
	// Fail toward safety: an unreadable supervisor-scope read must never
	// silently admit (it could hide an arbitrarily large real footprint) --
	// same philosophy as the outer-scope-unreadable case below.
	server := NewServer(Paths{})
	server.workerAdmitHeadroom = 0
	server.admitReadMemory = admitReadMemoryFixture(map[string]int64{}, 1000)
	server.admitReadWorkerSupervisorMemory = func(string) (int64, int64, bool, string) {
		return 0, 0, false, "fallback:supervisor-scope-unreadable"
	}
	response := server.evaluateWorkerAdmit(workerAdmitRequest{jobID: "job-1", outerScope: "/outer", estimatedBytes: 400, maxWaitMS: 0})
	if response.State != "unevaluated" {
		t.Fatalf("response=%+v", response)
	}
}

func TestEvaluateWorkerAdmitAccountsForSupervisorRSSWhenSupervisorScopeIsUncapped(t *testing.T) {
	// Real-world regression (AIRA-38, found live via the real-cgroup e2e
	// test): the supervisor's own child scope is NEVER given a memory.max
	// by BootstrapAitestSupervisor -- it stays at the cgroup default "max"
	// (uncapped) by design, since it is meant to be contained transitively
	// by the OUTER scope's cap, not individually. Before this fix,
	// evaluateWorkerAdmit reused the OUTER-scope reader (readSliceMemory)
	// for the supervisor-scope read too, and that reader treats an
	// uncapped memory.max as a hard read failure (a correct precondition
	// for the outer scope, which IS always explicitly capped by
	// construction -- but wrong here) -- so the aggregate guard reported
	// "unevaluated" on EVERY real invocation, and the granted (confined)
	// path was never actually reachable outside a mocked unit test. Pin
	// the real, unbounded-supervisor-scope shape directly here via
	// readWorkerSupervisorMemory (the real function, not a fixture) over a
	// real temp-directory cgroupfs-shaped layout.
	server := NewServer(Paths{})
	server.workerAdmitHeadroom = 0
	server.admitReadMemory = admitReadMemoryFixture(map[string]int64{}, 1000)
	supervisorScope := t.TempDir()
	if err := os.WriteFile(filepath.Join(supervisorScope, "memory.current"), []byte("400"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(supervisorScope, "memory.max"), []byte("max"), 0o644); err != nil {
		t.Fatal(err)
	}
	server.admitReadWorkerSupervisorMemory = func(path string) (int64, int64, bool, string) {
		if path != runner.WorkerScopeChildPath("/outer", "supervisor") {
			t.Fatalf("unexpected supervisor scope path %q", path)
		}
		return readWorkerSupervisorMemory(supervisorScope)
	}
	// supervisor(400)+700=1100 > 1000 ceiling: must be denied, not
	// unevaluated -- an uncapped memory.max is a normal, expected read,
	// not a failure.
	denied := server.evaluateWorkerAdmit(workerAdmitRequest{jobID: "job-1", outerScope: "/outer", estimatedBytes: 700, maxWaitMS: 0})
	if denied.State != "denied" || denied.Reason != "fallback:aggregate-cap-exceeded" {
		t.Fatalf("denied=%+v", denied)
	}
	granted := server.evaluateWorkerAdmit(workerAdmitRequest{jobID: "job-1", outerScope: "/outer", estimatedBytes: 500, maxWaitMS: 0})
	if granted.State != "granted" {
		t.Fatalf("granted=%+v", granted)
	}
}

func TestEvaluateWorkerAdmitReturnsUnevaluatedWhenOuterScopeLiveUsageUnreadable(t *testing.T) {
	// Fail toward safety, ported to the single-read model: admission no
	// longer reads individual worker-scope paths at all (dropped along
	// with the per-worker summation), so the one signal that can still be
	// unreadable is the OUTER scope's own memory.current/memory.max read
	// itself — that must never silently admit.
	server := NewServer(Paths{})
	server.workerAdmitHeadroom = 0
	server.admitReadMemory = func(string) (int64, int64, int64, bool, string) {
		return 0, 0, 0, false, "fallback:outer-scope-unreadable"
	}
	response := server.evaluateWorkerAdmit(workerAdmitRequest{jobID: "job-1", outerScope: "/outer", estimatedBytes: 400, maxWaitMS: 0})
	if response.State != "unevaluated" {
		t.Fatalf("response=%+v, want unevaluated when the outer scope's own live usage cannot be read", response)
	}
}

func TestEvaluateWorkerAdmitDeniesImmediatelyWhenRequestExceedsCeilingEvenAtZeroUsage(t *testing.T) {
	// A request that could never fit even with the WHOLE ceiling free right
	// now is a stable "never going to work" fact about the request, not a
	// transient contention moment — this is the one case Slice 1 makes
	// "denied" genuinely reachable for (see workerAdmitConnection, Task 5):
	// everything else that isn't available right now polls/retries and
	// eventually becomes "timeout" instead.
	server := NewServer(Paths{})
	server.workerAdmitHeadroom = 0
	server.admitReadMemory = admitReadMemoryFixture(map[string]int64{}, 1000)
	response := server.evaluateWorkerAdmit(workerAdmitRequest{jobID: "job-1", outerScope: "/outer", estimatedBytes: 1001, maxWaitMS: 0})
	if response.State != "denied" || response.Reason != "reject:exceeds-ceiling" {
		t.Fatalf("response=%+v, want denied/reject:exceeds-ceiling", response)
	}
}

func TestReleaseWorkerGrantIsIdempotent(t *testing.T) {
	// A worker-admit decision no longer depends on job.grants bookkeeping
	// for its arithmetic (admission now reads the outer scope's own live
	// memory.current directly, spec 3.3) — job.grants remains as worker-ID
	// bookkeeping only. What matters here is the property Task 5's fixed
	// workerAdmitConnection depends on: releaseWorkerGrant is safe to call
	// more than once (a write-failure path there defers a release that may
	// race a normal lease-close release of the same grant).
	server := NewServer(Paths{})
	server.workerAdmitHeadroom = 0
	server.admitReadMemory = admitReadMemoryFixture(map[string]int64{}, 1000)
	server.admitReadWorkerSupervisorMemory = admitReadWorkerSupervisorMemoryFixture(map[string]int64{})
	granted := server.evaluateWorkerAdmit(workerAdmitRequest{jobID: "job-1", outerScope: "/outer", estimatedBytes: 900, maxWaitMS: 0})
	if granted.State != "granted" {
		t.Fatalf("granted=%+v", granted)
	}
	server.releaseWorkerGrant("job-1", "/outer", granted.WorkerID)
	server.releaseWorkerGrant("job-1", "/outer", granted.WorkerID) // must not panic
	again := server.evaluateWorkerAdmit(workerAdmitRequest{jobID: "job-1", outerScope: "/outer", estimatedBytes: 900, maxWaitMS: 0})
	if again.State != "granted" {
		t.Fatalf("again=%+v", again)
	}
}

func TestWorkerJobLedgerIsBoundToJobIDAndOuterScopeTogether(t *testing.T) {
	// A job_id is caller-supplied and only as unique as the caller's own
	// pid-reuse window — two concurrent requests that happen to reuse the
	// same job_id with DIFFERENT outer_scope values must never get their
	// scope accounting mixed together.
	server := NewServer(Paths{})
	server.workerAdmitHeadroom = 0
	live := map[string]int64{"/outer-a": 900, "/outer-b": 0}
	server.admitReadMemory = admitReadMemoryFixture(live, 1000)
	server.admitReadWorkerSupervisorMemory = admitReadWorkerSupervisorMemoryFixture(map[string]int64{})
	// /outer-a is nearly saturated; /outer-b (same job_id!) is empty.
	denied := server.evaluateWorkerAdmit(workerAdmitRequest{jobID: "job-1", outerScope: "/outer-a", estimatedBytes: 500, maxWaitMS: 0})
	if denied.State != "denied" {
		t.Fatalf("denied=%+v", denied)
	}
	granted := server.evaluateWorkerAdmit(workerAdmitRequest{jobID: "job-1", outerScope: "/outer-b", estimatedBytes: 500, maxWaitMS: 0})
	if granted.State != "granted" {
		t.Fatalf("same job_id, different outer_scope must not inherit the other scope's saturation: %+v", granted)
	}
}

func TestWorkerAdmitOuterScopeIsOwnedByFirstJob(t *testing.T) {
	// The aggregate-cap guard only sums grants for one (job_id, outer_scope)
	// ledger. A second caller-chosen job_id must not build a separate ledger
	// against an outer scope already claimed by the first job.
	server := NewServer(Paths{})
	server.workerAdmitHeadroom = 0
	server.admitReadMemory = admitReadMemoryFixture(map[string]int64{}, 1000)
	server.admitReadWorkerSupervisorMemory = admitReadWorkerSupervisorMemoryFixture(map[string]int64{})
	first := server.evaluateWorkerAdmit(workerAdmitRequest{jobID: "job-1", outerScope: "/outer", estimatedBytes: 200, maxWaitMS: 0})
	if first.State != "granted" {
		t.Fatalf("first=%+v", first)
	}
	other := server.evaluateWorkerAdmit(workerAdmitRequest{jobID: "job-2", outerScope: "/outer", estimatedBytes: 200, maxWaitMS: 0})
	if other.State != "denied" || other.Reason != "reject:outer-scope-owned-by-another-job" {
		t.Fatalf("other=%+v", other)
	}
	again := server.evaluateWorkerAdmit(workerAdmitRequest{jobID: "job-1", outerScope: "/outer", estimatedBytes: 200, maxWaitMS: 0})
	if again.State != "granted" {
		t.Fatalf("again=%+v", again)
	}
}

func TestWorkerAdmitConnectionGrantsThenHoldsUntilPeerCloses(t *testing.T) {
	server := NewServer(Paths{})
	server.admitReadMemory = admitReadMemoryFixture(map[string]int64{}, 4*workerAdmitEstimatedBytesMin)
	server.admitReadWorkerSupervisorMemory = admitReadWorkerSupervisorMemoryFixture(map[string]int64{})
	server.workerAdmitHeadroom = 0
	server.workerAdmitPollInterval = time.Millisecond
	now := time.Unix(1000, 0)
	nowCalls := 0
	server.admitNow = func() time.Time {
		nowCalls++
		if nowCalls >= 3 {
			return now.Add(7 * time.Millisecond)
		}
		return now
	}

	serverConn, clientConn := net.Pipe()
	done := make(chan struct{})
	go func() {
		defer close(done)
		defer serverConn.Close()
		server.workerAdmitConnection(serverConn, map[string]any{
			"job_id": "job-1", "outer_scope": "/outer", "estimated_bytes": float64(workerAdmitEstimatedBytesMin), "max_wait_ms": float64(0),
		})
	}()

	var frame ResponseFrame
	if err := readFrame(clientConn, &frame); err != nil {
		t.Fatal(err)
	}
	var grant WorkerAdmitResponse
	if err := json.Unmarshal(frame.Data, &grant); err != nil || grant.State != "granted" {
		t.Fatalf("frame=%+v err=%v", frame, err)
	}
	if grant.WaitedMS != 7 {
		t.Fatalf("grant waited_ms=%d, want deterministic 7ms elapsed wait", grant.WaitedMS)
	}
	select {
	case <-done:
		t.Fatal("connection released before peer closed")
	case <-time.After(20 * time.Millisecond):
	}
	_ = clientConn.Close()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("connection did not release after peer close")
	}
	// Releasing must not leave the daemon in a broken state that then
	// rejects everything.
	response := server.evaluateWorkerAdmit(workerAdmitRequest{jobID: "job-1", outerScope: "/outer", estimatedBytes: 4 * workerAdmitEstimatedBytesMin, maxWaitMS: 0})
	if response.State != "granted" {
		t.Fatalf("post-release admission unexpectedly broken: %+v", response)
	}
}

func TestWorkerAdmitConnectionPollLoopReEvaluatesAndGrantsBeforeDeadline(t *testing.T) {
	server := NewServer(Paths{})
	const outerScope = "/outer"
	var liveUsage int64 = 2 * workerAdmitEstimatedBytesMin
	server.admitReadMemory = func(path string) (int64, int64, int64, bool, string) {
		if path == outerScope {
			return atomic.LoadInt64(&liveUsage), 2 * workerAdmitEstimatedBytesMin, 0, true, ""
		}
		return 0, 2 * workerAdmitEstimatedBytesMin, 0, true, ""
	}
	server.admitReadWorkerSupervisorMemory = admitReadWorkerSupervisorMemoryFixture(map[string]int64{})
	server.workerAdmitHeadroom = 0
	server.workerAdmitPollInterval = 5 * time.Millisecond

	serverConn, clientConn := net.Pipe()
	defer clientConn.Close()
	done := make(chan struct{})
	go func() {
		defer close(done)
		defer serverConn.Close()
		server.workerAdmitConnection(serverConn, map[string]any{
			"job_id": "job-1", "outer_scope": outerScope, "estimated_bytes": float64(workerAdmitEstimatedBytesMin), "max_wait_ms": float64(2000),
		})
	}()

	// Keep the fixture saturated across several real poll intervals. A
	// connection that cached only its first denial would remain stuck even
	// after the atomically published live-usage change below.
	time.Sleep(30 * time.Millisecond)
	atomic.StoreInt64(&liveUsage, 0)

	frameReady := make(chan error, 1)
	var frame ResponseFrame
	go func() { frameReady <- readFrame(clientConn, &frame) }()
	select {
	case err := <-frameReady:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("worker-admit connection did not grant after live usage cleared")
	}
	var response WorkerAdmitResponse
	if err := json.Unmarshal(frame.Data, &response); err != nil || response.State != "granted" {
		t.Fatalf("frame=%+v err=%v", frame, err)
	}
	_ = clientConn.Close()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("connection did not release after peer close")
	}
}

func TestWorkerAdmitConnectionTimesOutWhenSaturated(t *testing.T) {
	server := NewServer(Paths{})
	// The outer scope's own live usage already consumes the entire
	// ceiling — under the live-occupancy model (spec 3.3) there is no
	// per-worker-grant summation to "saturate" separately; a prior grant
	// alone does not change what a later admission decision sees unless
	// the outer scope's own live memory.current reflects it, exactly like
	// production (the daemon never tracks a synthetic reserve here).
	server.admitReadMemory = admitReadMemoryFixture(map[string]int64{"/outer": 2 * workerAdmitEstimatedBytesMin}, 2*workerAdmitEstimatedBytesMin)
	server.workerAdmitHeadroom = 0
	server.workerAdmitPollInterval = time.Millisecond
	now := time.Unix(1000, 0)
	nowCalls := 0
	server.admitNow = func() time.Time {
		nowCalls++
		switch nowCalls {
		case 1, 2, 3:
			return now
		default:
			return now.Add(5 * time.Millisecond)
		}
	}

	serverConn, clientConn := net.Pipe()
	go func() {
		defer serverConn.Close()
		server.workerAdmitConnection(serverConn, map[string]any{
			"job_id": "job-1", "outer_scope": "/outer", "estimated_bytes": float64(workerAdmitEstimatedBytesMin), "max_wait_ms": float64(5),
		})
	}()
	var frame ResponseFrame
	if err := readFrame(clientConn, &frame); err != nil {
		t.Fatal(err)
	}
	var response WorkerAdmitResponse
	if err := json.Unmarshal(frame.Data, &response); err != nil || response.State != "timeout" {
		t.Fatalf("frame=%+v err=%v", frame, err)
	}
	if response.WaitedMS != 5 {
		t.Fatalf("timeout waited_ms=%d, want deterministic 5ms elapsed wait", response.WaitedMS)
	}
	_ = clientConn.Close()
}

func TestWorkerAdmitConnectionDeniesImmediatelyWithoutWaitingOutMaxWait(t *testing.T) {
	server := NewServer(Paths{})
	server.admitReadMemory = admitReadMemoryFixture(map[string]int64{}, workerAdmitEstimatedBytesMin)
	server.workerAdmitHeadroom = 0
	server.workerAdmitPollInterval = time.Millisecond
	now := time.Unix(1000, 0)
	server.admitNow = func() time.Time { return now }

	serverConn, clientConn := net.Pipe()
	started := time.Now()
	go func() {
		defer serverConn.Close()
		server.workerAdmitConnection(serverConn, map[string]any{
			// 2 MiB can never fit under a 1 MiB outer ceiling no
			// matter how long we wait -- must come back "denied" well
			// before the (deliberately long) max_wait_ms elapses.
			"job_id": "job-1", "outer_scope": "/outer", "estimated_bytes": float64(2 * workerAdmitEstimatedBytesMin), "max_wait_ms": float64(60000),
		})
	}()
	var frame ResponseFrame
	if err := readFrame(clientConn, &frame); err != nil {
		t.Fatal(err)
	}
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Fatalf("denial took %v — looks like it waited out max_wait_ms instead of denying immediately", elapsed)
	}
	var response WorkerAdmitResponse
	if err := json.Unmarshal(frame.Data, &response); err != nil || response.State != "denied" {
		t.Fatalf("frame=%+v err=%v", frame, err)
	}
	if response.WaitedMS != 0 {
		t.Fatalf("immediate denial waited_ms=%d, want zero", response.WaitedMS)
	}
	_ = clientConn.Close()
}

func TestWorkerAdmitConnectionReleasesGrantWhenResponseWriteFails(t *testing.T) {
	server := NewServer(Paths{})
	server.admitReadMemory = admitReadMemoryFixture(map[string]int64{}, 2*workerAdmitEstimatedBytesMin)
	server.admitReadWorkerSupervisorMemory = admitReadWorkerSupervisorMemoryFixture(map[string]int64{})
	server.workerAdmitHeadroom = 0
	server.workerAdmitPollInterval = time.Millisecond

	serverConn, clientConn := net.Pipe()
	// Close the CLIENT side before the server ever gets to write its
	// response -- a peer-vanished-in-the-exact-window race.
	// evaluateWorkerAdmit already inserted the grant into the ledger by
	// this point; the subsequent writeFrame on serverConn must then fail,
	// and that grant must still be released rather than leaking against
	// the job's ledger forever (the bug: the old code just `return`ed on a
	// write failure with no release at all).
	_ = clientConn.Close()
	server.workerAdmitConnection(serverConn, map[string]any{
		"job_id": "job-1", "outer_scope": "/outer", "estimated_bytes": float64(workerAdmitEstimatedBytesMin), "max_wait_ms": float64(0),
	})
	_ = serverConn.Close()

	again := server.evaluateWorkerAdmit(workerAdmitRequest{jobID: "job-1", outerScope: "/outer", estimatedBytes: workerAdmitEstimatedBytesMin, maxWaitMS: 0})
	if again.State != "granted" {
		t.Fatalf("write-failure path leaked the grant: %+v", again)
	}
}
