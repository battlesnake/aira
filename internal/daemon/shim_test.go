package daemon

import (
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"aira/internal/core"
	"aira/internal/runner"
	"aira/internal/testdeadline"
)

// shimTestServer is a ci-shim daemon whose ledger admits against a small
// synthetic budget. The slice resolver and the confine scan are left to the
// PRODUCTION shim implementations, so those two are genuinely exercised; only
// the live memory reading is injected, because a unit test has no container
// cgroup to read.
func shimTestServer(t *testing.T, budget int64) *Server {
	t.Helper()
	server := NewServer(Paths{})
	server.stopping = make(chan struct{})
	server.admitPollInterval = time.Hour
	server.admitSliceHeadroomBase = 0
	server.admitSliceHeadroomSupervisor = 0
	server.SetConfineShimModeForTest(budget, runner.ShimBudgetSourceDeclared, "")
	server.admitReadMemory = func(string) (int64, int64, int64, bool, string) {
		return 0, budget, 0, true, ""
	}
	return server
}

// enqueueShimAdmit enqueues on the SENTINEL path, which is the only queue a shim
// daemon has: resolveShimSlicePath maps every requested slice name onto it, and
// confineManagement looks the registry up by that same key.
func enqueueShimAdmit(t *testing.T, server *Server, reserve int64) (*sliceQueue, *admitWaiter) {
	t.Helper()
	queue, waiter, code, err := server.enqueueAdmit(runner.ShimConfineSlice, reserve)
	if err != nil {
		t.Fatalf("enqueue: code=%s err=%v", code, err)
	}
	return queue, waiter
}

// shimTestScopeID mints an id in the canonical grammar parseConfineScopeID
// accepts: an owner is an "@" SUFFIX, never another "-" segment.
func shimTestScopeID() string {
	return "CONFINE-gate-" + strconv.Itoa(os.Getpid()) + "-" +
		strconv.FormatInt(time.Now().UnixNano(), 36) + "@session-a"
}

func shimAdmitArgs(reserve, wait int64, scopeID string) map[string]any {
	args := map[string]any{
		"slice": runner.DefaultConfineSlice, "reserve": reserve, "max_wait_ms": wait,
		"signature": "", "pinned": true,
	}
	if scopeID != "" {
		args["scope_id"] = scopeID
		args["name"] = "gate"
		args["owner"] = "session-a"
	}
	return args
}

// verifies: AIRA-121 requirement 3, ticket test (b)
//
// The advisory ledger REALLY GATES: a second job whose reserve does not fit
// stays queued while the first holds the budget, and is admitted the moment the
// first releases.
//
// The NEGATIVE half is what makes this non-porous. A no-op ledger — one that
// admits everything — passes the positive half on its own; only the assertion
// that job 2's grant channel does NOT close while job 1 holds the budget fails
// against it.
func TestShimLedgerQueuesASecondJobUntilTheFirstReleases(t *testing.T) {
	const budget = int64(1000)
	server := shimTestServer(t, budget)

	queueOne, first := enqueueShimAdmit(t, server, 700)
	waitAdmitGrant(t, first)

	queueTwo, second := enqueueShimAdmit(t, server, 700)
	if queueOne != queueTwo {
		t.Fatalf("shim mode resolved two different queues (%p, %p); every slice name must map to the one container budget", queueOne, queueTwo)
	}
	select {
	case <-second.grantedCh:
		t.Fatal("the second job was admitted while the first held the whole budget: the ci-shim ledger is not gating anything")
	case <-time.After(testdeadline.Wait(200 * time.Millisecond)):
	}

	server.releaseAdmitWaiter(queueOne, first)
	waitAdmitGrant(t, second)
}

// verifies: AIRA-121 gate condition C6
//
// --exclusive is REFUSED before the request is queued, and — the half the gate
// condition specifically calls for — WITHOUT the confine scan having to fail:
// no "confine reserve scan failed" log line, no armed abort anchor.
func TestShimRefusesExclusiveWithoutFailingTheConfineScan(t *testing.T) {
	server := shimTestServer(t, 1<<30)
	serverConn, clientConn := net.Pipe()
	go func() {
		defer serverConn.Close()
		// validateAdmitArgs requires the identity tuple for an exclusive request,
		// so it is supplied: the refusal under test must come from the SHIM check
		// and not from argument validation.
		args := shimAdmitArgs(100, 1000, shimTestScopeID())
		args["exclusive"] = true
		server.admitConnection(serverConn, args)
	}()
	defer clientConn.Close()
	var frame ResponseFrame
	if err := readFrame(clientConn, &frame); err != nil {
		t.Fatal(err)
	}
	if frame.OK || frame.Code != CodeAdmitExclusiveUnestablished {
		t.Fatalf("frame=%+v, want a %s refusal", frame, CodeAdmitExclusiveUnestablished)
	}
	if !strings.Contains(frame.Error, "ci-shim") {
		t.Fatalf("refusal %q does not say why exclusivity cannot be established here", frame.Error)
	}

	// The scan path must be untouched by the refusal: run one evaluator pass and
	// confirm it SUCCEEDED, leaving no scan-failure state behind. An
	// implementation that forced the scan to report unevaluated in order to keep
	// sliceProvablyEmpty false would set adoptedScanFailed and arm the anchor.
	queue, _ := enqueueShimAdmit(t, server, 10)
	deadline := time.Now().Add(testdeadline.Wait(2 * time.Second))
	for time.Now().Before(deadline) {
		queue.mu.Lock()
		settled := !queue.adoptedAt.IsZero()
		failed, anchored := queue.adoptedScanFailed, !queue.scanFailingSince.IsZero()
		queue.mu.Unlock()
		if settled {
			if failed || anchored {
				t.Fatalf("the ci-shim confine scan reported a failure (failed=%v anchor-armed=%v); it must report an honest EMPTY SUCCESS instead", failed, anchored)
			}
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("the evaluator never ran a scan pass")
}

// verifies: AIRA-121 gate condition C1
//
// worker-admit answers UNAVAILABLE / ADMISSION-UNUSABLE, which supervisor.py
// maps to WorkerAdmitUnavailable -> _disable_daemon -> the bare-fork pool.
//
// The counterexample is the plan's own original design: state=unevaluated
// carries class=contended, which is RETRIABLE, and
// _wait_for_admission_or_disable would poll a reachable-but-never-granting
// daemon forever. This test fails against that pair.
func TestShimWorkerAdmitIsTerminalAndUnusableNotRetriable(t *testing.T) {
	server := shimTestServer(t, 1<<30)
	response, proceed := server.evaluateWorkerAdmit(t.Context(), workerAdmitRequest{outerScope: "/whatever"})
	if !proceed {
		t.Fatal("evaluateWorkerAdmit abandoned the request")
	}
	if response.State != runner.WorkerAdmitStateUnavailable {
		t.Fatalf("state=%q, want %q; anything retriable makes the aitest supervisor wait forever",
			response.State, runner.WorkerAdmitStateUnavailable)
	}
	if response.Class != runner.WorkerAdmitClassAdmissionUnusable {
		t.Fatalf("class=%q, want %q; contended is retriable and would hang the suite",
			response.Class, runner.WorkerAdmitClassAdmissionUnusable)
	}
	if response.Class == runner.WorkerAdmitClassContended {
		t.Fatal("a contended class here is the exact hang gate condition C1 names")
	}
}

// verifies: AIRA-121 gate condition C2
//
// `confine --list` works in shim mode and carries the advisory wording on its
// reserve summary, and `--kill` refuses rather than fabricating a kill.
func TestShimConfineManagementListsAndRefusesToKill(t *testing.T) {
	server := shimTestServer(t, 4<<30)
	scopeID := shimTestScopeID()
	queue, waiter := enqueueShimAdmit(t, server, 1<<20)
	waitAdmitGrant(t, waiter)
	queue.mu.Lock()
	waiter.scopeID = scopeID
	queue.mu.Unlock()

	listed := server.confineManagement(t.Context(), core.Request{Verb: "confine-list", Args: map[string]any{"owner": "session-a"}})
	if !listed.OK || listed.Code != "OK" {
		t.Fatalf("confine-list response=%+v; ci-shim --list must not answer UNEVALUATED", listed)
	}
	data, _ := json.Marshal(listed.Data)
	var result runner.ConfineListResult
	if err := json.Unmarshal(data, &result); err != nil {
		t.Fatal(err)
	}
	if len(result.Scopes) != 1 || result.Scopes[0].ScopeID != scopeID {
		t.Fatalf("scopes=%+v, want the one granted waiter rendered from the registry", result.Scopes)
	}
	if result.SliceReserve == nil {
		t.Fatal("no slice reserve summary")
	}
	if result.SliceReserve.Containment != string(runner.ConfineContainmentAdvisory) {
		t.Fatalf("summary containment=%q, want the advisory wording: the number and the strength of the guarantee behind it must never be readable apart",
			result.SliceReserve.Containment)
	}
	if result.SliceReserve.BudgetSource != runner.ShimBudgetSourceDeclared {
		t.Fatalf("summary budget source=%q, want %q", result.SliceReserve.BudgetSource, runner.ShimBudgetSourceDeclared)
	}

	killed := server.confineManagement(t.Context(), core.Request{
		Verb: "confine-kill", Args: map[string]any{"owner": "session-a", "selector": "gate"},
	})
	if killed.OK {
		t.Fatalf("confine-kill response=%+v; ci-shim has no cgroup.kill and must never report a kill", killed)
	}
	if !strings.Contains(killed.Error, runner.CodeConfineKillUnconfirmed) || !strings.Contains(killed.Error, "kill ") {
		t.Fatalf("kill refusal %q must carry %s and name the supervisor pid to signal", killed.Error, runner.CodeConfineKillUnconfirmed)
	}
}

// verifies: AIRA-121 requirement 4
//
// The recorded budget is a BOUND, never a raise: a runtime that widened the
// container's memory.max after install must not widen the ledger.
func TestReadShimMemoryClampsToTheRecordedBudget(t *testing.T) {
	dir := t.TempDir()
	write := func(name, value string) {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(value), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write("memory.current", "1000")
	write("memory.max", "9999999")
	write("memory.stat", "file 0\n")
	server := NewServer(Paths{})
	server.SetConfineShimModeForTest(4096, runner.ShimBudgetSourceDeclared, dir)
	current, maximum, _, ok, reason := server.readShimMemory(runner.ShimConfineSlice)
	if !ok {
		t.Fatalf("readShimMemory not ok: %s", reason)
	}
	if current != 1000 {
		t.Fatalf("current=%d, want the real kernel reading 1000", current)
	}
	if maximum != 4096 {
		t.Fatalf("max=%d, want the recorded budget 4096; a widened container limit must never widen the ledger", maximum)
	}
}

// verifies: AIRA-121 requirement 4, finding F1
//
// THE HEADLINE CASE --memory-max exists for: a DECLARED budget over a container
// whose own cgroup has memory.max = `max` (several tasks on one node, as GCP
// Batch with taskCountPerNode > 1). The container's memory.current is still a
// real, namespaced reading and MUST be the ledger's `current`.
//
// Counterexample this fails against: routing through readSliceMemory, which
// refuses an unbounded memory.max as "unbounded". readShimMemory then fell
// through to host-wide MemTotal-MemAvailable — unnamespaced, so on a big node it
// dwarfs the declared budget, checkedAvailable answers 0, and EVERY job in the
// container gets E_ADMIT_TOO_LARGE with cap_minus_headroom=0 for the container's
// whole life. Fail-closed, but inoperable, and the refusal reads as
// misconfiguration.
func TestReadShimMemoryUsesTheOwnCgroupWhenItsLimitIsUnbounded(t *testing.T) {
	dir := t.TempDir()
	for name, value := range map[string]string{
		"memory.current": "1000", "memory.max": "max", "memory.stat": "file 0\n",
	} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(value), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	server := NewServer(Paths{})
	server.SetConfineShimModeForTest(4<<30, runner.ShimBudgetSourceDeclared, dir)
	current, maximum, _, ok, reason := server.readShimMemory(runner.ShimConfineSlice)
	if !ok {
		t.Fatalf("readShimMemory refused a container with a declared budget and no memory.max: %s", reason)
	}
	if current != 1000 {
		t.Fatalf("current=%d, want the container's own memory.current 1000; a host-wide meminfo reading here books the whole NODE against this container's budget", current)
	}
	if maximum != 4<<30 {
		t.Fatalf("max=%d, want the declared budget %d: an unbounded cgroup limit is not a smaller one", maximum, int64(4)<<30)
	}
	if available := checkedAvailable(current, maximum, 0, 0, 0); available <= 0 {
		t.Fatalf("available=%d: with 1000 bytes in use against a 4GiB declared budget every job would answer E_ADMIT_TOO_LARGE", available)
	}

	// The real path is NOT relaxed by the same change: an unbounded slice there
	// still has no ceiling to book against and is still refused.
	if _, _, _, ok, reason := readSliceMemory(dir); ok || reason != "unbounded" {
		t.Fatalf("readSliceMemory ok=%v reason=%q, want the unchanged unbounded refusal", ok, reason)
	}
}

// verifies: AIRA-121 requirement 4, finding F1
//
// With NO own-cgroup reading available at all — a cgroup-v1-only or cgroup-less
// container, where install recorded no path — meminfo is still the fall-back
// for a budget whose OWN source is host-wide (ShimBudgetSourceMemTotal: a
// host-wide budget paired with a host-wide live reading is the same scope on
// both sides), and it is still bounded by the recorded budget. The fix must
// not turn the fall-back off, only stop it being reached by a readable cgroup
// or by a container-scoped budget (see the F3 test below for that half).
//
// The meminfo pair is INJECTED (SetShimMeminfoForTest) rather than read from
// this host's real /proc/meminfo, per the F3 round-2 finding: a test that
// skips whenever the host happens to be memory-pressured, and asserts nothing
// about admission actually working, cannot catch a regression that zeroes
// availability. The sibling F1 assertion style — checkedAvailable(...) > 0 —
// is used here for the same reason.
func TestReadShimMemoryFallsBackToMeminfoWithNoOwnCgroup(t *testing.T) {
	server := NewServer(Paths{})
	server.SetConfineShimModeForTest(4<<30, runner.ShimBudgetSourceMemTotal, "")
	server.SetShimMeminfoForTest(
		func() (int64, bool) { return 8 << 30, true },
		func() (int64, bool, string) { return 6 << 30, true, "" },
	)
	current, maximum, reclaimable, ok, reason := server.readShimMemory(runner.ShimConfineSlice)
	if !ok {
		t.Fatalf("readShimMemory not ok: %s", reason)
	}
	if maximum != 4<<30 {
		t.Fatalf("max=%d, want the recorded budget", maximum)
	}
	if current != 2<<30 {
		t.Fatalf("current=%d, want MemTotal(8GiB)-MemAvailable(6GiB)=2GiB", current)
	}
	if reclaimable != 0 {
		t.Fatalf("reclaimable=%d: MemAvailable already credits reclaimable page cache, so the discount must stay zero here", reclaimable)
	}
	if available := checkedAvailable(current, maximum, reclaimable, 0, 0); available <= 0 {
		t.Fatalf("available=%d: a host-wide budget matched with a host-wide reading below it must not zero admission", available)
	}
}

// verifies: AIRA-121 finding F3
//
// THE BUG: a DECLARED --memory-max budget with no own-cgroup usage reading to
// pair it with (CgroupPath=="" from the probe -- the cgroup-v1-only host case,
// e.g. Amazon Linux 2 / legacy Fargate for AWS Batch) must NOT fall through to
// host-wide meminfo. On a real multi-tenant node, host-wide
// MemTotal-MemAvailable routinely dwarfs a small per-container declared
// budget; checkedAvailable's charge=max(current,outstanding) then pins to that
// inflated current, available collapses to 0, and EVERY job in the container
// answers E_ADMIT_TOO_LARGE cap_minus_headroom=0 for the container's ENTIRE
// LIFE. Fail-closed, but silently and permanently inoperable, not a transient
// contention wait.
//
// Counterexample this fails against: the OLD readShimMemory, which routed on
// "is there a cgroup path" alone and fell through to
// current=MemTotal-MemAvailable regardless of the budget's own source. Here
// the injected host usage (56GiB) vastly exceeds the declared 8GiB container
// budget, so the old code's current=56GiB makes checkedAvailable(...)==0.
//
// The budget is 8GiB, not the review round-3 finding's original 2GiB: round-4
// (finding F4) added an install-time 4GiB floor on every declared/cgroup-derived
// shim budget, so a 2GiB budget is no longer an installable value at all and
// would no longer be a realistic fixture for this unit-level test of
// readShimMemory's routing, which bypasses install and calls the seam directly.
func TestReadShimMemoryReportsBookedReserveOnlyForADeclaredBudgetWithNoOwnCgroup(t *testing.T) {
	const budget = int64(8) << 30 // 8 GiB declared container budget, above the F4 floor
	server := NewServer(Paths{})
	server.SetConfineShimModeForTest(budget, runner.ShimBudgetSourceDeclared, "")
	// A busy multi-tenant host: 64GiB total, only 8GiB free -- 56GiB "in use"
	// host-wide, still dwarfing the 8GiB container budget several times over.
	server.SetShimMeminfoForTest(
		func() (int64, bool) { return 64 << 30, true },
		func() (int64, bool, string) { return 8 << 30, true, "" },
	)
	current, maximum, reclaimable, ok, reason := server.readShimMemory(runner.ShimConfineSlice)
	if !ok {
		t.Fatalf("readShimMemory not ok: %s", reason)
	}
	if maximum != budget {
		t.Fatalf("max=%d, want the declared budget %d", maximum, budget)
	}
	if current != 0 || reclaimable != 0 {
		t.Fatalf("current=%d reclaimable=%d, want 0/0 (booked-reserve-only): host-wide meminfo must never stand in for a container-scoped budget's usage", current, reclaimable)
	}
	if available := checkedAvailable(current, maximum, reclaimable, 0, 0); available <= 0 {
		t.Fatalf("available=%d: a declared per-container budget must not be permanently zeroed by unrelated host-wide usage", available)
	}
}

// verifies: AIRA-121 finding F3
//
// The same booked-reserve-only fix must also fire when a cgroup path WAS
// recorded but its usage is unreadable -- the cgroup-v1-only host shape where
// the probe found a cgroup directory with no memory controller file in it --
// not only when CgroupPath is empty outright.
//
// Budget bumped to 8GiB for the same F4-floor reason as the sibling test above.
func TestReadShimMemoryReportsBookedReserveOnlyWhenTheRecordedCgroupHasNoMemoryController(t *testing.T) {
	const budget = int64(8) << 30 // above the F4 4GiB install-time floor
	dir := t.TempDir()            // no memory.current file: simulates a v1-only cgroup dir
	server := NewServer(Paths{})
	server.SetConfineShimModeForTest(budget, runner.ShimBudgetSourceCgroupMemoryMax, dir)
	server.SetShimMeminfoForTest(
		func() (int64, bool) { return 64 << 30, true },
		func() (int64, bool, string) { return 8 << 30, true, "" },
	)
	current, maximum, _, ok, reason := server.readShimMemory(runner.ShimConfineSlice)
	if !ok {
		t.Fatalf("readShimMemory not ok: %s", reason)
	}
	if maximum != budget || current != 0 {
		t.Fatalf("current=%d max=%d, want current=0 max=%d: an unreadable recorded cgroup must fall to booked-reserve-only, not host-wide meminfo", current, maximum, budget)
	}
}

// verifies: AIRA-121 requirement 3
//
// A budget that cannot be established fails CLOSED — waiters stay queued rather
// than being admitted against an unknown ceiling.
func TestReadShimMemoryFailsClosedWithoutABudget(t *testing.T) {
	server := NewServer(Paths{})
	server.SetConfineShimModeForTest(0, runner.ShimBudgetSourceDeclared, "")
	if _, _, _, ok, reason := server.readShimMemory(runner.ShimConfineSlice); ok {
		t.Fatal("readShimMemory reported a usable ceiling with no budget recorded")
	} else if reason == "" {
		t.Fatal("no reason given for the unevaluated ceiling")
	}
}

// verifies: AIRA-121 gate condition C5
//
// EVERY daemon launch path in a shim-installed home yields a shim daemon,
// because the daemon reads the durable record itself. The dispatcher's
// `/proc/self/exe daemon` spawn and a hand-run `aira daemon serve` transcribe no
// environment at all, and against a real-mode daemon a shim client's sentinel
// slice would fail to resolve and the job would launch UNGATED.
func TestDaemonResolvesShimModeFromTheDurableRecordWithNoEnvironment(t *testing.T) {
	stateHome := t.TempDir()
	record := runner.InstallModeRecord{
		Schema: 1, Mode: runner.ConfineModeShim, ShimBudgetBytes: 8 << 30,
		ShimBudgetSource: runner.ShimBudgetSourceCgroupMemoryMax,
	}
	if err := runner.WriteInstallModeRecord(runner.InstallModePathFor(stateHome), record); err != nil {
		t.Fatal(err)
	}
	t.Setenv("AIRA_DAEMON_CONFINE_MODE", "")
	mode, budget, err := resolveDaemonConfineMode(Paths{StateHome: stateHome})
	if err != nil {
		t.Fatal(err)
	}
	if mode != runner.ConfineModeShim {
		t.Fatalf("mode=%q, want ci-shim: the daemon must learn its mode from the record, not from whoever launched it", mode)
	}
	if budget.Bytes != 8<<30 || budget.Source != runner.ShimBudgetSourceCgroupMemoryMax {
		t.Fatalf("budget=%+v", budget)
	}

	// No record at all: every already-installed box stays on the real path.
	mode, _, err = resolveDaemonConfineMode(Paths{StateHome: t.TempDir()})
	if err != nil || mode != runner.ConfineModeReal {
		t.Fatalf("mode=%q err=%v, want real-slice for a box with no record", mode, err)
	}
}

// verifies: AIRA-121 gate condition C5
//
// A malformed override is a CONFIG ERROR at daemon start, not a runtime
// surprise, and never a silent fall-through to the real path.
func TestShimBudgetEnvironmentOverrideIsValidated(t *testing.T) {
	t.Setenv("AIRA_DAEMON_CONFINE_MODE", runner.ConfineModeShim)
	t.Setenv("AIRA_DAEMON_SHIM_BUDGET_BYTES", "not-a-number")
	if _, _, err := resolveDaemonConfineMode(Paths{StateHome: t.TempDir()}); err == nil ||
		!strings.Contains(err.Error(), "E_CONFIG_INVALID") {
		t.Fatalf("err=%v, want E_CONFIG_INVALID", err)
	}
	t.Setenv("AIRA_DAEMON_SHIM_BUDGET_BYTES", "1024")
	t.Setenv("AIRA_DAEMON_SHIM_BUDGET_SOURCE", "guessed")
	if _, _, err := resolveDaemonConfineMode(Paths{StateHome: t.TempDir()}); err == nil ||
		!strings.Contains(err.Error(), "E_CONFIG_INVALID") {
		t.Fatalf("err=%v, want E_CONFIG_INVALID for an uncatalogued source", err)
	}
	t.Setenv("AIRA_DAEMON_CONFINE_MODE", "sideways")
	if _, _, err := resolveDaemonConfineMode(Paths{StateHome: t.TempDir()}); err == nil ||
		!strings.Contains(err.Error(), "E_CONFIG_INVALID") {
		t.Fatalf("err=%v, want E_CONFIG_INVALID for an unknown mode", err)
	}
}

// verifies: AIRA-121 requirement 3
//
// Real mode is untouched by every seam change above: with no record and no
// environment, the resolvers are the production real-path ones.
func TestRealModeResolversAreUnchanged(t *testing.T) {
	server := NewServer(Paths{})
	if server.shimMode() {
		t.Fatal("a freshly constructed server is in shim mode")
	}
	var scanned atomic.Bool
	server.admitConfineScan = func(string) (runner.ConfineListResult, error) {
		scanned.Store(true)
		return runner.ConfineListResult{Verdict: "ok"}, nil
	}
	if _, err := server.admitConfineScan("/slice"); err != nil {
		t.Fatal(err)
	}
	if !scanned.Load() {
		t.Fatal("a test-injected scan seam was bypassed")
	}
}
