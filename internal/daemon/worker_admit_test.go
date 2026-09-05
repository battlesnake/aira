package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"math"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"aira/internal/runner"
	"aira/internal/testdeadline"
)

// workerScopeTree is an in-memory stand-in for the outer scope's real
// `.aira-worker-*` children — the cgroupfs the worker-admit ledger now sums
// (AIRA-39). It backs both daemon seams (workerScopeScan, workerScopeCreate)
// from one structure, so a test that creates a scope through the daemon sees
// exactly that scope in the next scan, the way the real tree does.
type workerScopeTree struct {
	mu       sync.Mutex
	caps     map[string]map[string]int64 // outer scope -> child dir name -> memory.max
	scanErr  map[string]error
	scans    map[string]int
	createFn func(outer, workerID string) error
}

func newWorkerScopeTree() *workerScopeTree {
	return &workerScopeTree{
		caps:    map[string]map[string]int64{},
		scanErr: map[string]error{},
		scans:   map[string]int{},
	}
}

// install wires the tree into a server and returns it. Every worker-admit test
// needs it: with no seam the default scan reads a real directory that does not
// exist, which is honestly "unevaluated" but tells us nothing about the ledger.
//
// The scan throttle is set to one nanosecond so ledger-ARITHMETIC tests see the
// tree as it is, not as it was up to a second ago. The throttle's own behaviour
// is tested separately, by the tests that set workerScopeScanInterval
// themselves; leaving the production default here would silently turn every
// other test into a test of the cache instead.
func (tree *workerScopeTree) install(server *Server) *workerScopeTree {
	server.workerScopeScan = tree.scan
	server.workerScopeCreate = tree.create
	server.workerScopeScanInterval = time.Nanosecond
	return tree
}

// put adds a child cgroup directly, standing in for a scope that already
// existed before this daemon process did — the AIRA-39 restart case — or for
// one created by something other than this daemon.
func (tree *workerScopeTree) put(outerScope, childName string, memoryMax int64) {
	tree.mu.Lock()
	defer tree.mu.Unlock()
	if tree.caps[outerScope] == nil {
		tree.caps[outerScope] = map[string]int64{}
	}
	tree.caps[outerScope][childName] = memoryMax
}

func (tree *workerScopeTree) remove(outerScope, childName string) {
	tree.mu.Lock()
	defer tree.mu.Unlock()
	delete(tree.caps[outerScope], childName)
}

func (tree *workerScopeTree) failScan(outerScope string, err error) {
	tree.mu.Lock()
	defer tree.mu.Unlock()
	tree.scanErr[outerScope] = err
}

func (tree *workerScopeTree) scanCount(outerScope string) int {
	tree.mu.Lock()
	defer tree.mu.Unlock()
	return tree.scans[outerScope]
}

func (tree *workerScopeTree) scan(outerScope string) (workerScopeChildren, error) {
	tree.mu.Lock()
	defer tree.mu.Unlock()
	tree.scans[outerScope]++
	if err := tree.scanErr[outerScope]; err != nil {
		return workerScopeChildren{}, err
	}
	var children workerScopeChildren
	for name, value := range tree.caps[outerScope] {
		if !strings.HasPrefix(name, workerScopeChildPrefix) {
			continue
		}
		children.committed = addClamp(children.committed, value)
		children.count++
		if index, err := strconv.Atoi(strings.TrimPrefix(name, workerScopeChildPrefix)); err == nil && index > children.maxIndex {
			children.maxIndex = index
		}
	}
	return children, nil
}

func (tree *workerScopeTree) create(_ context.Context, outerScope, workerID string, memoryMax, memoryHigh int64) (string, error) {
	tree.mu.Lock()
	defer tree.mu.Unlock()
	if tree.createFn != nil {
		if err := tree.createFn(outerScope, workerID); err != nil {
			return "", err
		}
	}
	name := workerScopeChildPrefix + workerID
	if tree.caps[outerScope] == nil {
		tree.caps[outerScope] = map[string]int64{}
	}
	if _, exists := tree.caps[outerScope][name]; exists {
		// Exactly what os.Mkdir does through runner.CreateWorkerScope.
		return "", fmt.Errorf("aitest worker scope: create: mkdir %s: %w", name, fs.ErrExist)
	}
	if memoryHigh > 0 && memoryHigh >= memoryMax {
		return "", fmt.Errorf("aitest worker scope: memory_high (%d) must be below memory_max (%d)", memoryHigh, memoryMax)
	}
	tree.caps[outerScope][name] = memoryMax
	return runner.WorkerScopeChildPath(outerScope, "worker-"+workerID), nil
}

// evaluateWorkerAdmitForTest calls the evaluator with a live context and fails
// the test on the abandon path, which no unit test here intends to exercise.
func evaluateWorkerAdmitForTest(t *testing.T, server *Server, req workerAdmitRequest) WorkerAdmitResponse {
	t.Helper()
	response, proceed := server.evaluateWorkerAdmit(context.Background(), req)
	if !proceed {
		t.Fatalf("evaluateWorkerAdmit abandoned the outer-scope lock unexpectedly for %+v", req)
	}
	return response
}

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

// verifies: AIRA-58 — worker-admit REFUSES an over-ceiling wait instead of
// silently clamping it. This test previously asserted the clamp (wait=cap+1 ->
// want=cap), i.e. it encoded the bug and would have kept passing against it.
func TestValidateWorkerAdmitArgsRefusesOverCeilingMaxWaitAndFloorsNegative(t *testing.T) {
	base := map[string]any{
		"job_id": "job-1", "outer_scope": "/outer", "estimated_bytes": workerAdmitEstimatedBytesMin,
	}
	args := func(wait int64) map[string]any {
		out := make(map[string]any, len(base)+1)
		for key, value := range base {
			out[key] = value
		}
		out["max_wait_ms"] = wait
		return out
	}

	request, err := validateWorkerAdmitArgs(args(-1), workerAdmitWaitCeilingMs)
	if err != nil || request.maxWaitMS != 0 {
		t.Fatalf("negative wait: request=%+v err=%v, want maxWaitMS=0", request, err)
	}

	if request, err := validateWorkerAdmitArgs(args(workerAdmitWaitCeilingMs), workerAdmitWaitCeilingMs); err != nil || request.maxWaitMS != workerAdmitWaitCeilingMs {
		t.Fatalf("at ceiling: request=%+v err=%v, want honoured exactly", request, err)
	}

	_, err = validateWorkerAdmitArgs(args(workerAdmitWaitCeilingMs+1), workerAdmitWaitCeilingMs)
	if err == nil {
		t.Fatal("over-ceiling worker-admit wait was accepted; it must be refused, never clamped")
	}
	// The code must stay E_DAEMON_PROTOCOL: worker_admit_client_linux.go wraps any
	// non-OK response as E_CONFINE_UNAVAILABLE, and the aitest supervisor answers
	// "unavailable" by disabling daemon admission and running UNCONFINED. It
	// already classifies E_DAEMON_PROTOCOL as permanent, so this fails closed.
	if !strings.Contains(err.Error(), CodeProtocol) {
		t.Fatalf("worker-admit refusal = %v, want %s so the supervisor treats it as permanent", err, CodeProtocol)
	}
	if strings.Contains(err.Error(), CodeAdmitWaitTooLong) {
		t.Fatalf("worker-admit must NOT use %s: the supervisor does not know it and would run unconfined (err=%v)", CodeAdmitWaitTooLong, err)
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
		if _, err := validateWorkerAdmitArgs(args, workerAdmitWaitCeilingMs); err == nil {
			t.Fatalf("estimated_bytes=%d accepted below 1 MiB minimum", estimated)
		}
	}
	args := make(map[string]any, len(base)+1)
	for key, value := range base {
		args[key] = value
	}
	args["estimated_bytes"] = workerAdmitEstimatedBytesMin
	request, err := validateWorkerAdmitArgs(args, workerAdmitWaitCeilingMs)
	if err != nil || request.estimatedBytes != workerAdmitEstimatedBytesMin {
		t.Fatalf("request=%+v err=%v, want exact 1 MiB boundary accepted", request, err)
	}
}

func TestValidateWorkerAdmitArgsParsesAllFields(t *testing.T) {
	args := map[string]any{
		"job_id": "job-123", "outer_scope": "/outer/scope", "signature": "suite:abc123",
		"estimated_bytes": int64(4 * workerAdmitEstimatedBytesMin), "max_wait_ms": int64(1234),
	}
	request, err := validateWorkerAdmitArgs(args, workerAdmitWaitCeilingMs)
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
			if _, err := validateWorkerAdmitArgs(args, workerAdmitWaitCeilingMs); err == nil || !strings.Contains(err.Error(), test.wantField) {
				t.Fatalf("args=%v err=%v, want rejection mentioning %q", args, err, test.wantField)
			}
		})
	}
}

func TestEvaluateWorkerAdmitGrantsWithinHeadroom(t *testing.T) {
	server := NewServer(Paths{})
	_ = newWorkerScopeTree().install(server)
	server.admitReadMemory = admitReadMemoryFixture(map[string]int64{}, 1000)
	server.admitReadWorkerSupervisorMemory = admitReadWorkerSupervisorMemoryFixture(map[string]int64{})
	server.workerAdmitHeadroom = 0
	response := evaluateWorkerAdmitForTest(t, server, workerAdmitRequest{jobID: "job-1", outerScope: "/outer", estimatedBytes: 400, maxWaitMS: 0})
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
	tree := newWorkerScopeTree().install(server)
	server.workerAdmitHeadroom = 0
	live := map[string]int64{}
	server.admitReadMemory = admitReadMemoryFixture(live, 1000)
	server.admitReadWorkerSupervisorMemory = admitReadWorkerSupervisorMemoryFixture(map[string]int64{})
	first := evaluateWorkerAdmitForTest(t, server, workerAdmitRequest{jobID: "job-1", outerScope: "/outer", estimatedBytes: 700, maxWaitMS: 0})
	if first.State != "granted" {
		t.Fatalf("first=%+v", first)
	}
	// Live usage on the outer scope stays low (both workers just started) --
	// the live-usage check alone would admit this, but 700+700 > 1000 means
	// the aggregate guard must deny it regardless.
	live["/outer"] = 100
	second := evaluateWorkerAdmitForTest(t, server, workerAdmitRequest{jobID: "job-1", outerScope: "/outer", estimatedBytes: 700, maxWaitMS: 0})
	if second.State != runner.WorkerAdmitStateDenied || second.Class != runner.WorkerAdmitClassContended || second.Reason != runner.WorkerAdmitReasonAggregateCapExceeded {
		t.Fatalf("second (aggregate-cap-guarded) =%+v", second)
	}
	// AIRA-39/AIRA-41: capacity is freed by the SCOPE going away (supervisor.py's
	// _retire_worker rmdirs it after reaping the worker), not by a connection
	// closing. Removing the child is what the previous releaseWorkerGrant call
	// stood for, and it is the only thing that may free the ledger now.
	tree.remove("/outer", workerScopeChildPrefix+first.WorkerID)
	third := evaluateWorkerAdmitForTest(t, server, workerAdmitRequest{jobID: "job-1", outerScope: "/outer", estimatedBytes: 700, maxWaitMS: 0})
	if third.State != "granted" {
		t.Fatalf("third (after the retired worker's scope is removed) =%+v", third)
	}
}

func TestEvaluateWorkerAdmitAggregateGuardHoldsUnderConcurrentAdmission(t *testing.T) {
	// The design spec's own test plan (section 7) requires proving
	// "Σ(worker sub-caps) never exceeds the outer scope's cap under
	// concurrent admission" -- every OTHER aggregate-guard test in this
	// file (including the sequential one directly above) calls
	// evaluateWorkerAdmit from a single goroutine, which cannot catch a
	// regression that moves job.mu.Lock() to AFTER the committed-cap
	// summation instead of wrapping it (found missing by Sol build-review,
	// AIRA-38 review wave): two truly simultaneous requests could then
	// both read committed=0 and both grant, defeating the guard exactly
	// the way AIRA-27/28/29 already fixed at whole-job granularity.
	server := NewServer(Paths{})
	_ = newWorkerScopeTree().install(server)
	server.workerAdmitHeadroom = 0
	server.admitReadMemory = admitReadMemoryFixture(map[string]int64{}, 1000)
	server.admitReadWorkerSupervisorMemory = admitReadWorkerSupervisorMemoryFixture(map[string]int64{})

	const concurrency = 8
	var wg sync.WaitGroup
	var start sync.WaitGroup
	start.Add(1)
	responses := make([]WorkerAdmitResponse, concurrency)
	for i := 0; i < concurrency; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			start.Wait() // maximize actual overlap rather than a staggered start
			responses[i] = evaluateWorkerAdmitForTest(t, server, workerAdmitRequest{jobID: "job-1", outerScope: "/outer", estimatedBytes: 700, maxWaitMS: 0})
		}(i)
	}
	start.Done()
	wg.Wait()

	granted := 0
	for _, response := range responses {
		if response.State == "granted" {
			granted++
		}
	}
	// Live usage stays at 0 throughout (the fixture never changes it), so
	// only the aggregate committed-cap guard -- not the live-usage check --
	// stands between "exactly one grant" and "every concurrent request
	// granted": 700*2=1400 exceeds the 1000-byte ceiling, so a second
	// concurrent grant would mean the guard was racy.
	if granted != 1 {
		t.Fatalf("granted=%d of %d concurrent 700-byte requests against a 1000-byte ceiling (Σcaps must never exceed it), want exactly 1", granted, concurrency)
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
	_ = newWorkerScopeTree().install(server)
	server.workerAdmitHeadroom = 0
	server.admitReadMemory = admitReadMemoryFixture(map[string]int64{}, 1000)
	supervisorLive := map[string]int64{runner.WorkerScopeChildPath("/outer", "supervisor"): 400}
	server.admitReadWorkerSupervisorMemory = admitReadWorkerSupervisorMemoryFixture(supervisorLive)
	// 700 alone fits under the outer ceiling (1000) and under the old
	// committed-only guard (committed=0 before any grant) -- but
	// supervisor(400)+700=1100 > 1000, so this must now be denied.
	denied := evaluateWorkerAdmitForTest(t, server, workerAdmitRequest{jobID: "job-1", outerScope: "/outer", estimatedBytes: 700, maxWaitMS: 0})
	if denied.State != runner.WorkerAdmitStateDenied || denied.Class != runner.WorkerAdmitClassContended || denied.Reason != runner.WorkerAdmitReasonAggregateCapExceeded {
		t.Fatalf("denied (supervisor-rss-guarded) =%+v", denied)
	}
	// A request that fits alongside the supervisor's footprint is granted.
	granted := evaluateWorkerAdmitForTest(t, server, workerAdmitRequest{jobID: "job-1", outerScope: "/outer", estimatedBytes: 500, maxWaitMS: 0})
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
	_ = newWorkerScopeTree().install(server)
	server.workerAdmitHeadroom = 0
	server.admitReadMemory = func(path string) (int64, int64, int64, bool, string) {
		if path == "/outer" {
			return 900, 1000, 500, true, ""
		}
		return 0, 1000, 0, true, ""
	}
	server.admitReadWorkerSupervisorMemory = admitReadWorkerSupervisorMemoryFixture(map[string]int64{})
	response := evaluateWorkerAdmitForTest(t, server, workerAdmitRequest{jobID: "job-1", outerScope: "/outer", estimatedBytes: 500})
	if response.State != "granted" {
		t.Fatalf("response=%+v, want reclaimable outer cache discounted", response)
	}
}

func TestEvaluateWorkerAdmitDiscountsReclaimableSupervisorCache(t *testing.T) {
	// Mirror the aggregate supervisor-RSS guard with a page-cache-heavy
	// supervisor: raw 900 + request 500 would deny, while the 500 bytes of
	// reclaimable cache leave effective supervisor use of 400 and permit it.
	server := NewServer(Paths{})
	_ = newWorkerScopeTree().install(server)
	server.workerAdmitHeadroom = 0
	supervisorScope := runner.WorkerScopeChildPath("/outer", "supervisor")
	server.admitReadMemory = admitReadMemoryFixture(map[string]int64{}, 1000)
	server.admitReadWorkerSupervisorMemory = func(path string) (int64, int64, bool, string) {
		if path == supervisorScope {
			return 900, 500, true, ""
		}
		return 0, 0, true, ""
	}
	response := evaluateWorkerAdmitForTest(t, server, workerAdmitRequest{jobID: "job-1", outerScope: "/outer", estimatedBytes: 500})
	if response.State != "granted" {
		t.Fatalf("response=%+v, want reclaimable supervisor cache discounted", response)
	}
}

func TestEvaluateWorkerAdmitFloorsReclaimableDiscountAtZero(t *testing.T) {
	// A malformed or racing stat read can report more reclaimable cache than
	// memory.current. subtractFloor must floor effective usage at zero rather
	// than turning the subtraction into invented negative memory headroom.
	server := NewServer(Paths{})
	_ = newWorkerScopeTree().install(server)
	server.workerAdmitHeadroom = 0
	server.admitReadMemory = func(path string) (int64, int64, int64, bool, string) {
		if path == "/outer" {
			return 100, 1000, 500, true, ""
		}
		return 0, 1000, 0, true, ""
	}
	server.admitReadWorkerSupervisorMemory = admitReadWorkerSupervisorMemoryFixture(map[string]int64{})
	response := evaluateWorkerAdmitForTest(t, server, workerAdmitRequest{jobID: "job-1", outerScope: "/outer", estimatedBytes: 1000})
	if response.State != "granted" {
		t.Fatalf("response=%+v, want reclaimable discount floored at zero", response)
	}
}

func TestEvaluateWorkerAdmitReturnsUnevaluatedWhenSupervisorScopeUnreadable(t *testing.T) {
	// Fail toward safety: an unreadable supervisor-scope read must never
	// silently admit (it could hide an arbitrarily large real footprint) --
	// same philosophy as the outer-scope-unreadable case below.
	server := NewServer(Paths{})
	_ = newWorkerScopeTree().install(server)
	server.workerAdmitHeadroom = 0
	server.admitReadMemory = admitReadMemoryFixture(map[string]int64{}, 1000)
	server.admitReadWorkerSupervisorMemory = func(string) (int64, int64, bool, string) {
		return 0, 0, false, "fallback:supervisor-scope-unreadable"
	}
	response := evaluateWorkerAdmitForTest(t, server, workerAdmitRequest{jobID: "job-1", outerScope: "/outer", estimatedBytes: 400, maxWaitMS: 0})
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
	_ = newWorkerScopeTree().install(server)
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
	denied := evaluateWorkerAdmitForTest(t, server, workerAdmitRequest{jobID: "job-1", outerScope: "/outer", estimatedBytes: 700, maxWaitMS: 0})
	if denied.State != runner.WorkerAdmitStateDenied || denied.Class != runner.WorkerAdmitClassContended || denied.Reason != runner.WorkerAdmitReasonAggregateCapExceeded {
		t.Fatalf("denied=%+v", denied)
	}
	granted := evaluateWorkerAdmitForTest(t, server, workerAdmitRequest{jobID: "job-1", outerScope: "/outer", estimatedBytes: 500, maxWaitMS: 0})
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
	_ = newWorkerScopeTree().install(server)
	server.workerAdmitHeadroom = 0
	server.admitReadMemory = func(string) (int64, int64, int64, bool, string) {
		return 0, 0, 0, false, "fallback:outer-scope-unreadable"
	}
	response := evaluateWorkerAdmitForTest(t, server, workerAdmitRequest{jobID: "job-1", outerScope: "/outer", estimatedBytes: 400, maxWaitMS: 0})
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
	_ = newWorkerScopeTree().install(server)
	server.workerAdmitHeadroom = 0
	server.admitReadMemory = admitReadMemoryFixture(map[string]int64{}, 1000)
	response := evaluateWorkerAdmitForTest(t, server, workerAdmitRequest{jobID: "job-1", outerScope: "/outer", estimatedBytes: 1001, maxWaitMS: 0})
	if response.State != runner.WorkerAdmitStateDenied || response.Class != runner.WorkerAdmitClassRequestInvalid || response.Reason != runner.WorkerAdmitReasonExceedsCeiling {
		t.Fatalf("response=%+v, want denied/request-invalid/exceeds-ceiling", response)
	}
}

// verifies: AIRA-41 — the ledger charges the SCOPE, not the relay connection.
// This replaces TestReleaseWorkerGrantIsIdempotent: releaseWorkerGrant is gone
// because a connection closing is no longer allowed to free capacity at all.
// The old test's own premise ("job.grants remains as worker-ID bookkeeping
// only") is exactly what AIRA-41 reported as the hole.
func TestWorkerAdmitLedgerKeepsChargingAfterRelayCloses(t *testing.T) {
	server := NewServer(Paths{})
	tree := newWorkerScopeTree().install(server)
	server.workerAdmitHeadroom = 0
	server.admitReadMemory = admitReadMemoryFixture(map[string]int64{}, 1000)
	server.admitReadWorkerSupervisorMemory = admitReadWorkerSupervisorMemoryFixture(map[string]int64{})
	granted := evaluateWorkerAdmitForTest(t, server, workerAdmitRequest{jobID: "job-1", outerScope: "/outer", estimatedBytes: 900, maxWaitMS: 0})
	if granted.State != "granted" {
		t.Fatalf("granted=%+v", granted)
	}
	// The relay dies (killed, crashed, or simply closed) while the worker
	// itself is still alive inside its still-capped scope. Nothing in the
	// daemon may free the 900 bytes: the scope is still there.
	second := evaluateWorkerAdmitForTest(t, server, workerAdmitRequest{jobID: "job-1", outerScope: "/outer", estimatedBytes: 900, maxWaitMS: 0})
	if second.State != runner.WorkerAdmitStateDenied || second.Class != runner.WorkerAdmitClassContended ||
		second.Reason != runner.WorkerAdmitReasonAggregateCapExceeded {
		t.Fatalf("second=%+v, want the killed relay's 900-byte scope still charged (AIRA-41)", second)
	}
	// Only the scope actually going away frees it — which supervisor.py does
	// after waitpid confirms the worker is gone.
	tree.remove("/outer", workerScopeChildPrefix+granted.WorkerID)
	third := evaluateWorkerAdmitForTest(t, server, workerAdmitRequest{jobID: "job-1", outerScope: "/outer", estimatedBytes: 900, maxWaitMS: 0})
	if third.State != "granted" {
		t.Fatalf("third=%+v, want a grant once the worker's scope is actually removed", third)
	}
}

// verifies: AIRA-39 — the committed sum is reconstructed from the cgroup tree,
// so a daemon that has just restarted (no in-memory state at all) still sees
// the live workers it never granted.
func TestWorkerAdmitLedgerReconstructsCommittedFromCgroupTreeAcrossRestart(t *testing.T) {
	server := NewServer(Paths{})
	tree := newWorkerScopeTree().install(server)
	server.workerAdmitHeadroom = 0
	// Live usage stays LOW: the workers have started but not yet grown, which
	// is precisely why the live-usage check alone does not catch this and the
	// worst-case aggregate guard has to.
	server.admitReadMemory = admitReadMemoryFixture(map[string]int64{"/outer": 50}, 1000)
	server.admitReadWorkerSupervisorMemory = admitReadWorkerSupervisorMemoryFixture(map[string]int64{})
	tree.put("/outer", workerScopeChildPrefix+"1", 400)
	tree.put("/outer", workerScopeChildPrefix+"2", 400)

	response := evaluateWorkerAdmitForTest(t, server, workerAdmitRequest{jobID: "job-1", outerScope: "/outer", estimatedBytes: 400, maxWaitMS: 0})
	if response.State != runner.WorkerAdmitStateDenied || response.Class != runner.WorkerAdmitClassContended ||
		response.Reason != runner.WorkerAdmitReasonAggregateCapExceeded {
		t.Fatalf("response=%+v, want denied/fallback:aggregate-cap-exceeded: two pre-existing 400-byte worker scopes plus a third would exceed the 1000-byte ceiling. Against a purely in-memory ledger this grants, and the outer scope's memory.oom.group kills the whole run (AIRA-39)", response)
	}
	// The same daemon still grants what genuinely fits alongside them.
	fits := evaluateWorkerAdmitForTest(t, server, workerAdmitRequest{jobID: "job-1", outerScope: "/outer", estimatedBytes: 200, maxWaitMS: 0})
	if fits.State != "granted" {
		t.Fatalf("fits=%+v, want a grant for a request that does fit alongside the reconstructed 800", fits)
	}
}

// verifies: AIRA-39 — worker ids are reconstructed from the tree too. Restarting
// at 1 would collide with an existing .aira-worker-1 on every restart.
func TestWorkerAdmitReconstructsNextWorkerIDFromExistingChildren(t *testing.T) {
	server := NewServer(Paths{})
	tree := newWorkerScopeTree().install(server)
	server.workerAdmitHeadroom = 0
	server.admitReadMemory = admitReadMemoryFixture(map[string]int64{}, 10000)
	server.admitReadWorkerSupervisorMemory = admitReadWorkerSupervisorMemoryFixture(map[string]int64{})
	for _, id := range []string{"1", "2", "3"} {
		tree.put("/outer", workerScopeChildPrefix+id, 100)
	}
	response := evaluateWorkerAdmitForTest(t, server, workerAdmitRequest{jobID: "job-1", outerScope: "/outer", estimatedBytes: 100, maxWaitMS: 0})
	if response.State != "granted" || response.WorkerID != "4" {
		t.Fatalf("response=%+v, want worker id 4 reconstructed from the existing .aira-worker-1..3", response)
	}
	if want := runner.WorkerScopeChildPath("/outer", "worker-4"); response.ScopePath != want {
		t.Fatalf("ScopePath=%q want %q", response.ScopePath, want)
	}
}

// verifies: AIRA-39 — a `.aira-worker-*` child whose suffix is NOT numeric is
// still CHARGED (CreateWorkerScope accepts any slashless id), while it must not
// perturb id allocation. Charging only numeric suffixes silently under-counts.
//
// Layer note (a mutation run made this worth spelling out): this test drives the
// EVALUATOR through the fake tree, so it pins that the evaluator honours whatever
// the scan charges. It does NOT reach sumWorkerScopeChildren, where the
// charge-every-suffix decision actually lives — restricting that to numeric
// suffixes leaves this test green. TestScanWorkerScopeChildrenSumsCappedWorkerChildrenOnly
// (worker_scope_scan_test.go) is what kills that mutation; the two are a pair.
func TestWorkerAdmitChargesNonNumericWorkerScopeChildren(t *testing.T) {
	server := NewServer(Paths{})
	tree := newWorkerScopeTree().install(server)
	server.workerAdmitHeadroom = 0
	server.admitReadMemory = admitReadMemoryFixture(map[string]int64{}, 1000)
	server.admitReadWorkerSupervisorMemory = admitReadWorkerSupervisorMemoryFixture(map[string]int64{})
	tree.put("/outer", workerScopeChildPrefix+"foo", 800)

	denied := evaluateWorkerAdmitForTest(t, server, workerAdmitRequest{jobID: "job-1", outerScope: "/outer", estimatedBytes: 400, maxWaitMS: 0})
	if denied.State != runner.WorkerAdmitStateDenied || denied.Class != runner.WorkerAdmitClassContended ||
		denied.Reason != runner.WorkerAdmitReasonAggregateCapExceeded {
		t.Fatalf("denied=%+v, want the non-numeric .aira-worker-foo child's 800-byte cap charged", denied)
	}
	granted := evaluateWorkerAdmitForTest(t, server, workerAdmitRequest{jobID: "job-1", outerScope: "/outer", estimatedBytes: 100, maxWaitMS: 0})
	if granted.State != "granted" || granted.WorkerID != "1" {
		t.Fatalf("granted=%+v, want worker id 1: a non-numeric suffix contributes nothing to id allocation", granted)
	}
}

func TestWorkerJobLedgerIsBoundToJobIDAndOuterScopeTogether(t *testing.T) {
	// A job_id is caller-supplied and only as unique as the caller's own
	// pid-reuse window — two concurrent requests that happen to reuse the
	// same job_id with DIFFERENT outer_scope values must never get their
	// scope accounting mixed together.
	server := NewServer(Paths{})
	_ = newWorkerScopeTree().install(server)
	server.workerAdmitHeadroom = 0
	live := map[string]int64{"/outer-a": 900, "/outer-b": 0}
	server.admitReadMemory = admitReadMemoryFixture(live, 1000)
	server.admitReadWorkerSupervisorMemory = admitReadWorkerSupervisorMemoryFixture(map[string]int64{})
	// /outer-a is nearly saturated; /outer-b (same job_id!) is empty.
	denied := evaluateWorkerAdmitForTest(t, server, workerAdmitRequest{jobID: "job-1", outerScope: "/outer-a", estimatedBytes: 500, maxWaitMS: 0})
	if denied.State != "denied" {
		t.Fatalf("denied=%+v", denied)
	}
	granted := evaluateWorkerAdmitForTest(t, server, workerAdmitRequest{jobID: "job-1", outerScope: "/outer-b", estimatedBytes: 500, maxWaitMS: 0})
	if granted.State != "granted" {
		t.Fatalf("same job_id, different outer_scope must not inherit the other scope's saturation: %+v", granted)
	}
}

// verifies: AIRA-39 — deleting workerScopeOwner is only safe because the sum is
// now over the SCOPE. Two job ids sharing one outer scope (a real case: a second
// aitest-enabled pytest run inside one confine job) must be counted TOGETHER,
// not refused. Replaces TestWorkerAdmitOuterScopeIsOwnedByFirstJob, whose
// terminal reject:outer-scope-owned-by-another-job no longer exists.
func TestWorkerAdmitTwoJobsShareOneOuterScopeAndAreCountedTogether(t *testing.T) {
	server := NewServer(Paths{})
	tree := newWorkerScopeTree().install(server)
	server.workerAdmitHeadroom = 0
	server.admitReadMemory = admitReadMemoryFixture(map[string]int64{}, 1000)
	server.admitReadWorkerSupervisorMemory = admitReadWorkerSupervisorMemoryFixture(map[string]int64{})
	first := evaluateWorkerAdmitForTest(t, server, workerAdmitRequest{jobID: "job-1", outerScope: "/outer", estimatedBytes: 700, maxWaitMS: 0})
	if first.State != "granted" {
		t.Fatalf("first=%+v", first)
	}
	// A DIFFERENT job id on the same outer scope. It must be denied for the
	// arithmetic reason (700+700 > 1000), never with an ownership rejection —
	// and the denial must carry the RETRIABLE disposition, so it clears once
	// job-1's worker retires. AIRA-42: that disposition is now the `contended`
	// CLASS; it used to be a "fallback:" prefix on the reason string.
	other := evaluateWorkerAdmitForTest(t, server, workerAdmitRequest{jobID: "job-2", outerScope: "/outer", estimatedBytes: 700, maxWaitMS: 0})
	if other.State != runner.WorkerAdmitStateDenied || other.Class != runner.WorkerAdmitClassContended || other.Reason != runner.WorkerAdmitReasonAggregateCapExceeded {
		t.Fatalf("other=%+v, want the second job counted against the first job's scope, not rejected for ownership", other)
	}
	if strings.Contains(other.Reason, "owned-by-another-job") {
		t.Fatalf("ownership rejection resurrected: %+v", other)
	}
	// What genuinely fits alongside is granted to the second job, and gets its
	// own distinct worker id from the same per-scope sequence.
	fits := evaluateWorkerAdmitForTest(t, server, workerAdmitRequest{jobID: "job-2", outerScope: "/outer", estimatedBytes: 200, maxWaitMS: 0})
	if fits.State != "granted" || fits.WorkerID == first.WorkerID {
		t.Fatalf("fits=%+v (first=%+v), want a grant with a distinct worker id from the shared per-scope sequence", fits, first)
	}
	tree.remove("/outer", workerScopeChildPrefix+first.WorkerID)
	again := evaluateWorkerAdmitForTest(t, server, workerAdmitRequest{jobID: "job-2", outerScope: "/outer", estimatedBytes: 700, maxWaitMS: 0})
	if again.State != "granted" {
		t.Fatalf("again=%+v, want job-2 admitted once job-1's worker scope is gone", again)
	}
}

func TestWorkerAdmitConnectionGrantsThenHoldsUntilPeerCloses(t *testing.T) {
	server := NewServer(Paths{})
	_ = newWorkerScopeTree().install(server)
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
	case <-testdeadline.After(time.Second):
		t.Fatal("connection did not release after peer close")
	}
	// The lease closing must not leave the daemon in a broken state that then
	// rejects everything. It also must not FREE the grant: the worker's scope
	// is still on the tree (AIRA-41), so what still fits is 4 MiB minus the
	// 1 MiB already charged.
	response := evaluateWorkerAdmitForTest(t, server, workerAdmitRequest{jobID: "job-1", outerScope: "/outer", estimatedBytes: 3 * workerAdmitEstimatedBytesMin, maxWaitMS: 0})
	if response.State != "granted" {
		t.Fatalf("post-release admission unexpectedly broken: %+v", response)
	}
	overCommitted := evaluateWorkerAdmitForTest(t, server, workerAdmitRequest{jobID: "job-1", outerScope: "/outer", estimatedBytes: workerAdmitEstimatedBytesMin, maxWaitMS: 0})
	if overCommitted.State != "denied" {
		t.Fatalf("post-release admission=%+v, want the closed lease's scope still charged (AIRA-41)", overCommitted)
	}
}

func TestWorkerAdmitConnectionPollLoopReEvaluatesAndGrantsBeforeDeadline(t *testing.T) {
	server := NewServer(Paths{})
	_ = newWorkerScopeTree().install(server)
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
	case <-testdeadline.After(time.Second):
		t.Fatal("worker-admit connection did not grant after live usage cleared")
	}
	var response WorkerAdmitResponse
	if err := json.Unmarshal(frame.Data, &response); err != nil || response.State != "granted" {
		t.Fatalf("frame=%+v err=%v", frame, err)
	}
	_ = clientConn.Close()
	select {
	case <-done:
	case <-testdeadline.After(time.Second):
		t.Fatal("connection did not release after peer close")
	}
}

func TestWorkerAdmitConnectionTimesOutWhenSaturated(t *testing.T) {
	server := NewServer(Paths{})
	_ = newWorkerScopeTree().install(server)
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
	_ = newWorkerScopeTree().install(server)
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
	if elapsed := time.Since(started); testdeadline.Exceeded(elapsed, time.Second) {
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

// verifies: AIRA-39 — a grant whose response write fails keeps its scope on the
// tree, and keeps charging. Replaces
// TestWorkerAdmitConnectionReleasesGrantWhenResponseWriteFails, whose in-memory
// release no longer exists. The daemon deliberately does NOT rmdir the scope
// here: writeFrameBytes discards n on error, so a fully delivered frame followed
// by a write error is possible, and removing a scope a live client is about to
// fork into hits WorkerPlacementFailed -> _disable_daemon -> the whole suite
// unconfined. Over-charging produces a loud, retriable denial instead.
func TestWorkerAdmitConnectionKeepsScopeChargedWhenResponseWriteFails(t *testing.T) {
	server := NewServer(Paths{})
	tree := newWorkerScopeTree().install(server)
	server.admitReadMemory = admitReadMemoryFixture(map[string]int64{}, 2*workerAdmitEstimatedBytesMin)
	server.admitReadWorkerSupervisorMemory = admitReadWorkerSupervisorMemoryFixture(map[string]int64{})
	server.workerAdmitHeadroom = 0
	server.workerAdmitPollInterval = time.Millisecond

	serverConn, clientConn := net.Pipe()
	// Close the CLIENT side before the server ever gets to write its response —
	// a peer-vanished-in-the-exact-window race. The scope has already been
	// created by then.
	_ = clientConn.Close()
	server.workerAdmitConnection(serverConn, map[string]any{
		"job_id": "job-1", "outer_scope": "/outer", "estimated_bytes": float64(workerAdmitEstimatedBytesMin), "max_wait_ms": float64(0),
	})
	_ = serverConn.Close()

	if got := tree.scanCount("/outer"); got == 0 {
		t.Fatalf("the ledger never scanned the outer scope at all (scans=%d)", got)
	}
	children, err := tree.scan("/outer")
	if err != nil {
		t.Fatal(err)
	}
	if children.count != 1 || children.committed != workerAdmitEstimatedBytesMin {
		t.Fatalf("children=%+v, want the undelivered grant's scope still present and still charged", children)
	}
	// 2 MiB ceiling, 1 MiB already charged: a second 2 MiB request cannot fit.
	again := evaluateWorkerAdmitForTest(t, server, workerAdmitRequest{jobID: "job-1", outerScope: "/outer", estimatedBytes: 2 * workerAdmitEstimatedBytesMin, maxWaitMS: 0})
	if again.State != "denied" {
		t.Fatalf("again=%+v, want the undelivered grant still charged against the ledger", again)
	}
}

// verifies: AIRA-63 — worker-admit is bounded by admitSlots, and saturation is
// delivered as a RETRIABLE denial. Emitting it as an error frame (the way
// admitConnection's CodeBusy does) would reach RequestWorkerAdmit's
// `Code != "OK"` branch, match none of supervisor.py's denial substrings, and
// make _disable_daemon run the whole suite UNCONFINED.
func TestWorkerAdmitConnectionDeniesRetriablyWhenAdmitSlotsSaturated(t *testing.T) {
	server := NewServer(Paths{})
	_ = newWorkerScopeTree().install(server)
	server.admitReadMemory = admitReadMemoryFixture(map[string]int64{}, 2*workerAdmitEstimatedBytesMin)
	server.admitReadWorkerSupervisorMemory = admitReadWorkerSupervisorMemoryFixture(map[string]int64{})
	server.workerAdmitHeadroom = 0
	for i := 0; i < admitGlobalMax; i++ {
		server.admitSlots <- struct{}{}
	}
	t.Cleanup(func() {
		for i := 0; i < admitGlobalMax; i++ {
			<-server.admitSlots
		}
	})

	serverConn, clientConn := net.Pipe()
	go func() {
		defer serverConn.Close()
		server.workerAdmitConnection(serverConn, map[string]any{
			"job_id": "job-1", "outer_scope": "/outer", "estimated_bytes": float64(workerAdmitEstimatedBytesMin), "max_wait_ms": float64(0),
		})
	}()
	var frame ResponseFrame
	if err := readFrame(clientConn, &frame); err != nil {
		t.Fatal(err)
	}
	_ = clientConn.Close()
	// Code MUST stay "OK" so RequestWorkerAdmit parses the payload as a grant
	// decision rather than wrapping it as "request rejected".
	if frame.Code != "OK" {
		t.Fatalf("saturation frame code=%q, want %q: any other code makes the client report 'request rejected', which supervisor.py classifies as unavailable and answers by running unconfined", frame.Code, "OK")
	}
	var response WorkerAdmitResponse
	if err := json.Unmarshal(frame.Data, &response); err != nil {
		t.Fatalf("frame=%+v err=%v", frame, err)
	}
	if response.State != runner.WorkerAdmitStateDenied || response.Class != runner.WorkerAdmitClassContended ||
		response.Reason != runner.WorkerAdmitReasonAdmitSlotsSaturated {
		t.Fatalf("response=%+v, want denied/contended/admit-slots-saturated", response)
	}
	// The load-bearing half, kept as its own assertion rather than left
	// implied by the equality above: what matters is the CONSEQUENCE, that
	// supervisor.py raises the retriable WorkerAdmitDenied rather than the
	// terminal WorkerAdmitRequestInvalid. AIRA-42 moved that property from
	// "the reason is not reject:-prefixed" (which is what this used to check)
	// to the class, so the check moves with it. It is deliberately phrased
	// against the terminal class, so a future rewording of the reason token
	// cannot quietly change what the supervisor does with this verdict.
	if response.Class == runner.WorkerAdmitClassRequestInvalid {
		t.Fatalf("saturation class=%q is the terminal class, which makes supervisor.py mark the queue unevaluated instead of retrying once a slot frees", response.Class)
	}
	if strings.Contains(response.Reason, "reject:") || strings.Contains(response.Reason, "fallback:") {
		t.Fatalf("the retired prose prefix convention reappeared in reason %q", response.Reason)
	}
}

// verifies: AIRA-63 — the bound really is the shared semaphore, i.e. a
// worker-admit waiter consumes and returns a slot. Without the release, one
// worker-admit call would permanently cost a slot.
func TestWorkerAdmitConnectionReleasesItsAdmitSlot(t *testing.T) {
	server := NewServer(Paths{})
	_ = newWorkerScopeTree().install(server)
	server.admitReadMemory = admitReadMemoryFixture(map[string]int64{}, workerAdmitEstimatedBytesMin)
	server.workerAdmitHeadroom = 0
	serverConn, clientConn := net.Pipe()
	go func() {
		defer serverConn.Close()
		// A permanent reject:, so the call returns without holding a lease.
		server.workerAdmitConnection(serverConn, map[string]any{
			"job_id": "job-1", "outer_scope": "/outer", "estimated_bytes": float64(2 * workerAdmitEstimatedBytesMin), "max_wait_ms": float64(0),
		})
	}()
	var frame ResponseFrame
	if err := readFrame(clientConn, &frame); err != nil {
		t.Fatal(err)
	}
	_ = clientConn.Close()
	deadline := time.Now().Add(testdeadline.Wait(2 * time.Second))
	for len(server.admitSlots) != 0 {
		if time.Now().After(deadline) {
			t.Fatalf("admitSlots still holds %d token(s) after worker-admit returned", len(server.admitSlots))
		}
		time.Sleep(time.Millisecond)
	}
}

// verifies: AIRA-39 — the cgroupfs scan is throttled. evaluateWorkerAdmit runs
// once per 200ms poll per waiter, so an unbounded per-poll walk is the AIRA-61
// CPU-regression class (25-65% supervisor CPU before af407be fixed it once).
func TestWorkerAdmitScanIsThrottledToAtMostOncePerInterval(t *testing.T) {
	server := NewServer(Paths{})
	tree := newWorkerScopeTree().install(server)
	server.workerScopeScanInterval = time.Second
	server.workerAdmitHeadroom = 0
	now := time.Unix(1000, 0)
	server.admitNow = func() time.Time { return now }
	// Saturated: every evaluation takes the CONTENDED path, which is the one
	// that must not scan on each poll.
	server.admitReadMemory = admitReadMemoryFixture(map[string]int64{}, 1000)
	server.admitReadWorkerSupervisorMemory = admitReadWorkerSupervisorMemoryFixture(map[string]int64{})
	tree.put("/outer", workerScopeChildPrefix+"1", 900)

	for i := 0; i < 20; i++ {
		if response := evaluateWorkerAdmitForTest(t, server, workerAdmitRequest{jobID: "job-1", outerScope: "/outer", estimatedBytes: 400}); response.State != "denied" {
			t.Fatalf("poll %d response=%+v, want a contended denial", i, response)
		}
	}
	if got := tree.scanCount("/outer"); got != 1 {
		t.Fatalf("scans=%d over 20 polls inside one interval, want exactly 1 (an unthrottled per-poll cgroupfs walk is the AIRA-61 regression)", got)
	}
	// Past the interval, one more scan — not twenty.
	now = now.Add(2 * time.Second)
	if response := evaluateWorkerAdmitForTest(t, server, workerAdmitRequest{jobID: "job-1", outerScope: "/outer", estimatedBytes: 400}); response.State != "denied" {
		t.Fatalf("response=%+v", response)
	}
	if got := tree.scanCount("/outer"); got != 2 {
		t.Fatalf("scans=%d after the interval elapsed, want 2", got)
	}
}

// verifies: AIRA-39 — a FAILING scan is throttled exactly like a succeeding one.
// The first implementation gated the throttle on `scanned`, which a failure
// clears, so every 200ms poll rescanned a filesystem that was already
// misbehaving: the AIRA-61 per-poll O(tree) regression, on the worst possible
// path. Found independently by Sol and DeepSeek build-review.
func TestWorkerAdmitFailingScanIsThrottledLikeASuccessfulOne(t *testing.T) {
	server := NewServer(Paths{})
	tree := newWorkerScopeTree().install(server)
	server.workerScopeScanInterval = time.Second
	server.workerAdmitHeadroom = 0
	now := time.Unix(1000, 0)
	server.admitNow = func() time.Time { return now }
	server.admitReadMemory = admitReadMemoryFixture(map[string]int64{}, 1000)
	server.admitReadWorkerSupervisorMemory = admitReadWorkerSupervisorMemoryFixture(map[string]int64{})
	tree.failScan("/outer", errors.New("worker scope /outer/.aira-worker-1: read memory.max: input/output error"))

	for i := 0; i < 20; i++ {
		response := evaluateWorkerAdmitForTest(t, server, workerAdmitRequest{jobID: "job-1", outerScope: "/outer", estimatedBytes: 400})
		// Every poll must still answer honestly — the throttle replays the
		// error, it never falls back to a stale or fabricated sum.
		if response.State != "unevaluated" {
			t.Fatalf("poll %d response=%+v, want unevaluated on every poll", i, response)
		}
		if !strings.Contains(response.Detail, "input/output error") {
			t.Fatalf("poll %d detail=%q, want the underlying failure replayed", i, response.Detail)
		}
	}
	if got := tree.scanCount("/outer"); got != 1 {
		t.Fatalf("scans=%d over 20 failing polls inside one interval, want exactly 1", got)
	}
	// Past the interval it retries — a throttle, not a latch.
	now = now.Add(2 * time.Second)
	if response := evaluateWorkerAdmitForTest(t, server, workerAdmitRequest{jobID: "job-1", outerScope: "/outer", estimatedBytes: 400}); response.State != "unevaluated" {
		t.Fatalf("response=%+v", response)
	}
	if got := tree.scanCount("/outer"); got != 2 {
		t.Fatalf("scans=%d after the interval elapsed, want 2: the throttle must expire, not latch", got)
	}
}

// verifies: AIRA-39, AIRA-42 — operator-controlled diagnostic text can never be
// mistaken for a verdict.
//
// This test was TestWorkerAdmitUnevaluatedReasonNeverCarriesTheUnboundedToken,
// and it guarded a workaround rather than a property. supervisor.py used to
// disable daemon admission entirely — the WHOLE suite UNCONFINED — for any
// "worker-admit unevaluated" message that merely CONTAINED the token
// "unbounded", so the daemon defensively mangled that token to "un-bounded"
// wherever free-form text (cgroup paths carrying the operator's own
// `--name`, raw memory.max bytes) could reach the wire. Found independently
// by Sol and DeepSeek build-review on AIRA-39.
//
// AIRA-42 removes the hazard at its source instead of neutralising its
// symptom: the verdict lives in `class`/`reason`, `detail` is not parsed by
// anything, and the mangling is deleted. So the assertions INVERT — the
// diagnostic is now allowed to say "unbounded" verbatim, and what must hold is
// that saying it changes NO part of the classification. A regression to
// substring classification, or a class slip from `contended` to
// `admission-unusable`, fails here.
func TestWorkerAdmitHostileDiagnosticTextCannotForgeAVerdict(t *testing.T) {
	for _, test := range []struct {
		name string
		err  error
	}{
		// The operator named the confine job "unbounded-suite"; the path is
		// echoed into the diagnostic.
		{name: "job name in the cgroup path",
			err: errors.New("worker scope /sys/fs/cgroup/user.slice/.aira-CONFINE-unbounded-suite-1234-abc/.aira-worker-2: read memory.max: permission denied")},
		// A corrupt memory.max whose bytes are echoed back through %q.
		{name: "corrupt memory.max contents",
			err: errors.New(`worker scope /outer/.aira-worker-2: memory.max is not a finite byte count ("unbounded")`)},
	} {
		t.Run(test.name, func(t *testing.T) {
			server := NewServer(Paths{})
			tree := newWorkerScopeTree().install(server)
			server.workerAdmitHeadroom = 0
			server.admitReadMemory = admitReadMemoryFixture(map[string]int64{}, 1000)
			server.admitReadWorkerSupervisorMemory = admitReadWorkerSupervisorMemoryFixture(map[string]int64{})
			tree.failScan("/outer", test.err)

			response := evaluateWorkerAdmitForTest(t, server, workerAdmitRequest{jobID: "job-1", outerScope: "/outer", estimatedBytes: 100})
			// The load-bearing assertion: a scan failure is RETRIABLE, no
			// matter what its text happens to say. The condition whose token
			// this text imitates (outer-scope-unbounded) is
			// admission-unusable — the containment-stripping class — so a
			// classifier that read the text would land here with the wrong
			// disposition and run the rest of the suite unconfined.
			if response.State != runner.WorkerAdmitStateUnevaluated ||
				response.Class != runner.WorkerAdmitClassContended ||
				response.Reason != runner.WorkerAdmitReasonWorkerScopesUnreadable {
				t.Fatalf("response=%+v, want unevaluated/contended/worker-scopes-unreadable: "+
					"diagnostic text that merely mentions \"unbounded\" must not be read as an "+
					"uncapped outer scope, which is the containment-stripping verdict", response)
			}
			// The diagnostic gets to be ACCURATE again — the whole cost of the
			// old mangling. The offending child is named, verbatim.
			if !strings.Contains(response.Detail, ".aira-worker-2") {
				t.Fatalf("detail=%q, want the offending child still named", response.Detail)
			}
			if !strings.Contains(response.Detail, "unbounded") || strings.Contains(response.Detail, "un-bounded") {
				t.Fatalf("detail=%q, want the operator's own text unmangled: the structured channel "+
					"is what makes it safe to report accurately", response.Detail)
			}
			// End to end through the real renderer and parser: hostile detail
			// survives tokenisation without displacing a single field.
			line, err := runner.WorkerAdmitOutcomeLine(runner.WorkerAdmitOutcome{
				State: response.State, Class: response.Class,
				Reason: response.Reason, Detail: response.Detail,
			}, nil)
			if err != nil {
				t.Fatalf("render: %v", err)
			}
			fields, err := runner.ParseWorkerAdmitOutcomeLine(line)
			if err != nil {
				t.Fatalf("parse %q: %v", line, err)
			}
			if fields["class"] != runner.WorkerAdmitClassContended || fields["reason"] != runner.WorkerAdmitReasonWorkerScopesUnreadable {
				t.Fatalf("round trip changed the verdict: %v", fields)
			}
		})
	}
}

// verifies: AIRA-39 — a CACHED sum may never be the basis of a GRANT. A child
// that appeared since the last scan is invisible to the cache and need not
// collide with the id about to be allocated, so the grant path forces a fresh
// scan. Against a cache-only implementation this test grants and over-admits.
func TestWorkerAdmitGrantAlwaysScansFreshBeforeGranting(t *testing.T) {
	server := NewServer(Paths{})
	tree := newWorkerScopeTree().install(server)
	server.workerScopeScanInterval = time.Hour // the cache can never expire here
	server.workerAdmitHeadroom = 0
	now := time.Unix(1000, 0)
	server.admitNow = func() time.Time { return now }
	server.admitReadMemory = admitReadMemoryFixture(map[string]int64{}, 1000)
	server.admitReadWorkerSupervisorMemory = admitReadWorkerSupervisorMemoryFixture(map[string]int64{})

	// One worker already exists, and the cache is WARM and VALID for exactly it.
	// Seeding that cache by GRANTING would not do: a grant invalidates on its
	// way out, so the next evaluation would rescan for that reason alone and the
	// forced scan would prove nothing. This test did seed by granting, and
	// passed against a grant path that used the cached sum, until the mutation
	// run caught it — so the warm state is now installed directly.
	tree.put("/outer", workerScopeChildPrefix+"1", 100)
	state := server.workerScopeFor("/outer")
	state.committed, state.maxIndex, state.scanned, state.committedAt = 100, 1, true, now

	// A worker scope the warm cache knows nothing about, whose NON-NUMERIC name
	// cannot collide with the next id either — so EEXIST can never catch it.
	// Only a forced scan on the grant path can.
	tree.put("/outer", workerScopeChildPrefix+"external", 850)
	scansBefore := tree.scanCount("/outer")

	response := evaluateWorkerAdmitForTest(t, server, workerAdmitRequest{jobID: "job-1", outerScope: "/outer", estimatedBytes: 100})
	if response.State != runner.WorkerAdmitStateDenied || response.Class != runner.WorkerAdmitClassContended ||
		response.Reason != runner.WorkerAdmitReasonAggregateCapExceeded {
		t.Fatalf("response=%+v, want denied: 100(existing)+850(external)+100 > 1000. A cached sum was used to admit", response)
	}
	// Exactly one: the contended DENIAL path must still read the cache (the
	// AIRA-61 cadence bound), and only the grant path may force a refresh.
	if got := tree.scanCount("/outer"); got != scansBefore+1 {
		t.Fatalf("scans=%d (was %d), want exactly one forced scan on the grant path and none from the cached check", got, scansBefore)
	}
}

// verifies: AIRA-39 — EEXIST proves the cached sum was stale-LOW, so it must
// invalidate and deny RETRIABLY. "Take the next id and grant instead" would
// admit against a sum omitting the colliding child: the exact over-admit.
func TestWorkerAdmitEEXISTInvalidatesCacheAndDeniesRetriably(t *testing.T) {
	server := NewServer(Paths{})
	tree := newWorkerScopeTree().install(server)
	server.workerScopeScanInterval = time.Hour
	server.workerAdmitHeadroom = 0
	now := time.Unix(1000, 0)
	server.admitNow = func() time.Time { return now }
	server.admitReadMemory = admitReadMemoryFixture(map[string]int64{}, 1000)
	server.admitReadWorkerSupervisorMemory = admitReadWorkerSupervisorMemoryFixture(map[string]int64{})

	// The scan seam reports an EMPTY tree while the create seam sees an
	// existing .aira-worker-1 — precisely a child that appeared after the scan.
	hidden := map[string]int64{workerScopeChildPrefix + "1": 900}
	server.workerScopeScan = func(outerScope string) (workerScopeChildren, error) {
		children, err := tree.scan(outerScope)
		return children, err
	}
	server.workerScopeCreate = func(ctx context.Context, outerScope, workerID string, memoryMax, memoryHigh int64) (string, error) {
		if _, exists := hidden[workerScopeChildPrefix+workerID]; exists {
			return "", fmt.Errorf("aitest worker scope: create: mkdir: %w", fs.ErrExist)
		}
		return tree.create(ctx, outerScope, workerID, memoryMax, memoryHigh)
	}

	first := evaluateWorkerAdmitForTest(t, server, workerAdmitRequest{jobID: "job-1", outerScope: "/outer", estimatedBytes: 400})
	if first.State != runner.WorkerAdmitStateDenied || first.Class != runner.WorkerAdmitClassContended ||
		first.Reason != runner.WorkerAdmitReasonWorkerScopeIDCollision {
		t.Fatalf("first=%+v, want a retriable denied/contended/worker-scope-id-collision. Advancing to the next id and granting would admit 400 against a sum that omits the colliding 900-byte child", first)
	}
	// The retriability is now the class, not a "fallback:" reason prefix
	// (AIRA-42). Asserted against the terminal class so a reason rewording
	// cannot silently turn re-evaluation into run termination.
	if first.Class == runner.WorkerAdmitClassRequestInvalid {
		t.Fatalf("collision denial class=%q is terminal and would end the run instead of re-evaluating", first.Class)
	}
	// The collision invalidated the cache; the retry now sees the real tree and
	// denies for the honest arithmetic reason.
	for name, value := range hidden {
		tree.put("/outer", name, value)
	}
	second := evaluateWorkerAdmitForTest(t, server, workerAdmitRequest{jobID: "job-1", outerScope: "/outer", estimatedBytes: 400})
	if second.State != runner.WorkerAdmitStateDenied || second.Class != runner.WorkerAdmitClassContended ||
		second.Reason != runner.WorkerAdmitReasonAggregateCapExceeded {
		t.Fatalf("second=%+v, want the re-evaluation to see the collided child and deny on the sum", second)
	}
}

// verifies: AIRA-39, AIRA-42 — an EEXIST collision SELF-HEALS on the retry: it
// does not collide identically forever.
//
// Written to answer a DeepSeek build-review P1 on the AIRA-42 merge, which read
// the `contended` (retriable) class on this row as retry-forever and argued the
// row should be terminal, on the grounds that "invalidating the cached sum does
// not clear the existing cgroup". True but beside the point — the retry does not
// need the child GONE, it needs it COUNTED. `state.invalidate()` forces the next
// evaluation to rescan, the rescan lifts `maxIndex` past the colliding child,
// and the id therefore advances rather than repeating.
//
// The existing test above only shows the retry DENYING on the corrected
// aggregate, which does not distinguish the two readings. This one shows the id
// advancing and the request being GRANTED, which does. Kept as a permanent test
// because the property is load-bearing for the retriable classification and was
// not previously pinned anywhere.
func TestWorkerAdmitEEXISTRetryAdvancesTheWorkerIDInsteadOfRepeating(t *testing.T) {
	server := NewServer(Paths{})
	tree := newWorkerScopeTree().install(server)
	server.workerAdmitHeadroom = 0
	// Deliberately roomy, so nothing but the collision can deny: the point of
	// this test is the ID, not the arithmetic.
	server.admitReadMemory = admitReadMemoryFixture(map[string]int64{}, 10000)
	server.admitReadWorkerSupervisorMemory = admitReadWorkerSupervisorMemoryFixture(map[string]int64{})

	// A child invisible to the scan but present to create: exactly the
	// stale-low cache this branch exists for.
	hidden := map[string]int64{workerScopeChildPrefix + "1": 100}
	server.workerScopeCreate = func(ctx context.Context, outer, workerID string, memoryMax, memoryHigh int64) (string, error) {
		if _, exists := hidden[workerScopeChildPrefix+workerID]; exists {
			return "", fmt.Errorf("aitest worker scope: create: mkdir: %w", fs.ErrExist)
		}
		return tree.create(ctx, outer, workerID, memoryMax, memoryHigh)
	}

	first := evaluateWorkerAdmitForTest(t, server, workerAdmitRequest{jobID: "job-1", outerScope: "/outer", estimatedBytes: 400})
	if first.Class != runner.WorkerAdmitClassContended || first.Reason != runner.WorkerAdmitReasonWorkerScopeIDCollision {
		t.Fatalf("first=%+v, want the retriable collision verdict", first)
	}
	// The invalidation is what makes the next scan see the child.
	for name, value := range hidden {
		tree.put("/outer", name, value)
	}
	second := evaluateWorkerAdmitForTest(t, server, workerAdmitRequest{jobID: "job-1", outerScope: "/outer", estimatedBytes: 400})
	if second.State != runner.WorkerAdmitStateGranted {
		t.Fatalf("second=%+v, want a GRANT: a collision that cannot clear would make `contended` a "+
			"retry-forever stall, which is the reading this test exists to refute", second)
	}
	if second.WorkerID == "1" {
		t.Fatalf("second=%+v reused the colliding id: the rescan must lift maxIndex past it", second)
	}
}

// verifies: AIRA-39, AIRA-42 — a create failure that is NOT a collision is
// fail-closed and terminal: no grant is recorded, and the verdict carries the
// TERMINAL class so supervisor.py marks the queue unevaluated rather than
// retrying forever against broken daemon-side cgroupfs access. The disposition
// used to be spelled as a "reject:" prefix on the reason; it is now the class.
func TestWorkerAdmitDeniesTerminallyWhenScopeCreateFails(t *testing.T) {
	server := NewServer(Paths{})
	tree := newWorkerScopeTree().install(server)
	server.workerAdmitHeadroom = 0
	server.admitReadMemory = admitReadMemoryFixture(map[string]int64{}, 1000)
	server.admitReadWorkerSupervisorMemory = admitReadWorkerSupervisorMemoryFixture(map[string]int64{})
	tree.createFn = func(string, string) error { return errors.New("permission denied") }

	response := evaluateWorkerAdmitForTest(t, server, workerAdmitRequest{jobID: "job-1", outerScope: "/outer", estimatedBytes: 400})
	if response.State != runner.WorkerAdmitStateDenied {
		t.Fatalf("response=%+v, want a denial: a grant whose scope could not be created must never be issued", response)
	}
	if response.Class != runner.WorkerAdmitClassRequestInvalid || response.Reason != runner.WorkerAdmitReasonWorkerScopeCreateFailed {
		t.Fatalf("response=%+v, want the terminal request-invalid class so supervisor.py terminates instead of retrying indefinitely", response)
	}
	// NOT admission-unusable: the daemon is answering fine, and that class
	// would strip RAM containment for the whole remaining run.
	if response.Class == runner.WorkerAdmitClassAdmissionUnusable {
		t.Fatalf("response=%+v: a daemon-side cgroupfs failure must not be reported as unusable admission — that runs the rest of the suite unconfined", response)
	}
	if !strings.Contains(response.Detail, "permission denied") {
		t.Fatalf("detail=%q, want the underlying cause named", response.Detail)
	}
	// And nothing was charged: the ledger has no phantom entry.
	children, err := tree.scan("/outer")
	if err != nil {
		t.Fatal(err)
	}
	if children.count != 0 {
		t.Fatalf("children=%+v, want no scope created", children)
	}
}

// verifies: AIRA-39 — the granted scope path is the one the daemon actually
// created, and no grant is possible without a create.
func TestWorkerAdmitCreatesWorkerScopeBeforeGranting(t *testing.T) {
	server := NewServer(Paths{})
	tree := newWorkerScopeTree().install(server)
	server.workerAdmitHeadroom = 0
	server.admitReadMemory = admitReadMemoryFixture(map[string]int64{}, 1000)
	server.admitReadWorkerSupervisorMemory = admitReadWorkerSupervisorMemoryFixture(map[string]int64{})
	var created []string
	inner := server.workerScopeCreate
	server.workerScopeCreate = func(ctx context.Context, outerScope, workerID string, memoryMax, memoryHigh int64) (string, error) {
		path, err := inner(ctx, outerScope, workerID, memoryMax, memoryHigh)
		if err == nil {
			created = append(created, path)
		}
		return path, err
	}
	response := evaluateWorkerAdmitForTest(t, server, workerAdmitRequest{jobID: "job-1", outerScope: "/outer", estimatedBytes: 400})
	if response.State != "granted" {
		t.Fatalf("response=%+v", response)
	}
	if len(created) != 1 || created[0] != response.ScopePath {
		t.Fatalf("created=%v, granted ScopePath=%q: the grant must name the scope the daemon created", created, response.ScopePath)
	}
	// The scope exists in the tree BEFORE the response is returned, so the
	// grant->creation window is closed by construction.
	children, err := tree.scan("/outer")
	if err != nil {
		t.Fatal(err)
	}
	if children.count != 1 || children.committed != 400 {
		t.Fatalf("children=%+v, want the created scope present and charged", children)
	}
}

// verifies: AIRA-39 — the aggregate guard must not WRAP once a tree-derived
// committed sum saturates. The old subtractive form (`estimated >
// ceiling-committed-supervisorUsed`) wraps positive and GRANTS.
func TestWorkerAdmitAggregateGuardDoesNotWrapOnSaturatedCommitted(t *testing.T) {
	// The subtractive form `estimatedBytes <= ceiling-committed-supervisorUsed`
	// only wraps when ceiling-MaxInt64-supervisorUsed underflows past MinInt64,
	// which needs supervisorUsed > ceiling+1. This test originally used
	// ceiling=1000 with supervisorUsed=2, which does NOT reach that (the result
	// is merely hugely negative, so the subtractive form denies too) — it passed
	// against the very bug it claims to pin until a mutation run caught it.
	// estimatedBytes must also stay <= ceiling, or the earlier
	// reject:exceeds-ceiling branch short-circuits before the aggregate guard.
	// var, not const: Go evaluates constant arithmetic exactly and REFUSES to
	// compile the overflow this test is about, so the wrap has to happen at
	// runtime the way it does in production.
	var ceiling, supervisorUsed, requested int64 = 100, 200, 100
	if wrapped := ceiling - int64(math.MaxInt64) - supervisorUsed; wrapped <= 0 {
		t.Fatalf("test setup never reaches the wrap: ceiling-MaxInt64-supervisorUsed = %d, want a POSITIVE (wrapped) value, or this test cannot distinguish the two forms", wrapped)
	}
	server := NewServer(Paths{})
	_ = newWorkerScopeTree().install(server)
	server.workerAdmitHeadroom = 0
	server.admitReadMemory = admitReadMemoryFixture(map[string]int64{}, ceiling)
	server.admitReadWorkerSupervisorMemory = admitReadWorkerSupervisorMemoryFixture(
		map[string]int64{runner.WorkerScopeChildPath("/outer", "supervisor"): supervisorUsed})
	server.workerScopeScan = func(string) (workerScopeChildren, error) {
		return workerScopeChildren{committed: math.MaxInt64, count: 1, maxIndex: 1}, nil
	}
	response := evaluateWorkerAdmitForTest(t, server, workerAdmitRequest{jobID: "job-1", outerScope: "/outer", estimatedBytes: requested})
	if response.State != runner.WorkerAdmitStateDenied || response.Class != runner.WorkerAdmitClassContended ||
		response.Reason != runner.WorkerAdmitReasonAggregateCapExceeded {
		t.Fatalf("response=%+v, want a denial: a saturated committed sum must never wrap into apparent headroom", response)
	}
}

// verifies: AIRA-39 — a scan the daemon cannot establish is "unevaluated",
// never a fabricated zero (which is exactly the AIRA-39 over-admit).
func TestWorkerAdmitReturnsUnevaluatedWhenChildCapUnreadable(t *testing.T) {
	server := NewServer(Paths{})
	tree := newWorkerScopeTree().install(server)
	server.workerAdmitHeadroom = 0
	server.admitReadMemory = admitReadMemoryFixture(map[string]int64{}, 1000)
	server.admitReadWorkerSupervisorMemory = admitReadWorkerSupervisorMemoryFixture(map[string]int64{})
	tree.failScan("/outer", errors.New(`worker scope /outer/.aira-worker-1: memory.max is not a finite byte count ("max")`))

	response := evaluateWorkerAdmitForTest(t, server, workerAdmitRequest{jobID: "job-1", outerScope: "/outer", estimatedBytes: 100})
	if response.State != "unevaluated" {
		t.Fatalf("response=%+v, want unevaluated: an unreadable child cap must never be treated as zero committed", response)
	}
	if !strings.Contains(response.Detail, "memory.max") {
		t.Fatalf("detail=%q, want the underlying scan failure named", response.Detail)
	}
}

// verifies: AIRA-39 — outer-scope identity is canonicalised before anything
// keys on it, so `/outer`, `/outer/` and `/outer/.` cannot take three different
// ledger cells while mutating one cgroup.
func TestValidateWorkerAdmitArgsCleansAndRequiresAbsoluteOuterScope(t *testing.T) {
	args := func(outer string) map[string]any {
		return map[string]any{
			"job_id": "job-1", "outer_scope": outer,
			"estimated_bytes": workerAdmitEstimatedBytesMin, "max_wait_ms": int64(0),
		}
	}
	for _, outer := range []string{"/outer", "/outer/", "/outer/.", "/a/../outer"} {
		request, err := validateWorkerAdmitArgs(args(outer), workerAdmitWaitCeilingMs)
		if err != nil {
			t.Fatalf("outer_scope=%q: %v", outer, err)
		}
		if request.outerScope != "/outer" {
			t.Fatalf("outer_scope=%q normalised to %q, want %q: aliases must share one ledger cell and one lock", outer, request.outerScope, "/outer")
		}
	}
	if _, err := validateWorkerAdmitArgs(args("relative/scope"), workerAdmitWaitCeilingMs); err == nil {
		t.Fatal("a relative outer_scope was accepted; it must be refused rather than resolved against the daemon's own working directory")
	}
}

// verifies: AIRA-63/AIRA-39 — a waiter abandoned while queued on the outer
// scope's lock leaves the token untaken, so the next acquirer proceeds. A
// sync.Mutex here would block it uninterruptibly while holding an admit slot.
func TestWorkerScopeLockReleasesOnCancelledWaiter(t *testing.T) {
	server := NewServer(Paths{})
	state := server.workerScopeFor("/outer")
	if !server.acquireWorkerScope(context.Background(), state) {
		t.Fatal("first acquire failed")
	}
	ctx, cancel := context.WithCancel(context.Background())
	blocked := make(chan bool, 1)
	go func() { blocked <- server.acquireWorkerScope(ctx, state) }()
	cancel()
	select {
	case got := <-blocked:
		if got {
			t.Fatal("a cancelled waiter reported that it acquired the lock")
		}
	case <-testdeadline.After(2 * time.Second):
		t.Fatal("a cancelled waiter stayed blocked on the outer-scope lock")
	}
	state.release()
	if !server.acquireWorkerScope(context.Background(), state) {
		t.Fatal("the next acquirer could not take the lock: the cancelled waiter consumed the token")
	}
	state.release()
}

// verifies: AIRA-39 — an ALREADY-cancelled caller must never acquire the outer
// scope lock, deterministically. select picks uniformly at random among ready
// cases, so with a free lock and a done context the bare select took the lock
// about half the time and went on to create a worker scope for a peer that was
// already gone (CreateWorkerScope does not observe ctx) -- an orphan scope
// charging the ledger until job teardown. Found by Sol build-review round 2.
// The loop is what makes this a real check: a single attempt passes ~50% of the
// time against the unfixed code, which is exactly the kind of coin-flip
// "evidence" this repo treats as no evidence at all.
func TestWorkerScopeLockIsNeverTakenByAnAlreadyCancelledCaller(t *testing.T) {
	server := NewServer(Paths{})
	state := server.workerScopeFor("/outer")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	for i := 0; i < 200; i++ {
		if server.acquireWorkerScope(ctx, state) {
			t.Fatalf("attempt %d: an already-cancelled caller took the lock and would go on to create an orphan worker scope", i)
		}
	}
	// The lock is still free for a live caller — the refusals took no token.
	if !server.acquireWorkerScope(context.Background(), state) {
		t.Fatal("the lock was left held after the cancelled attempts")
	}
	state.release()
}
