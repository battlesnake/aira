package daemon

import (
	"encoding/json"
	"net"
	"testing"
	"time"

	"aira/internal/runner"
	"aira/internal/testdeadline"
)

// shimWorkerServer is a ci-shim daemon whose worker ledger admits against a
// fixed synthetic budget with `used` under the caller's control.
//
// Only the LIVE MEMORY READING is injected — the same single seam
// shimTestServer overrides, and for the same reason: a unit test has no
// container cgroup to read. The mode gate, the ledger arithmetic, the booking
// and the release are all the production ones.
func shimWorkerServer(t *testing.T, budget int64, used *int64) *Server {
	t.Helper()
	server := NewServer(Paths{})
	server.stopping = make(chan struct{})
	server.workerAdmitHeadroom = 0
	server.workerAdmitPollInterval = time.Millisecond
	server.SetConfineShimModeForTest(budget, runner.ShimBudgetSourceDeclared, "")
	server.admitReadMemory = func(path string) (int64, int64, int64, bool, string) {
		if path != runner.ShimConfineSlice {
			t.Errorf("shim ledger read path=%q, want the ci-shim sentinel; nothing here may read a cgroup", path)
		}
		var current int64
		if used != nil {
			current = *used
		}
		return current, budget, 0, true, ""
	}
	return server
}

func shimWorkerRequest(bytes int64) workerAdmitRequest {
	return workerAdmitRequest{
		jobID: "job-1", outerScope: runner.ShimConfineSlice,
		estimatedBytes: bytes, maxWaitMS: 0,
	}
}

// verifies: AIRA-123
//
// The headline behaviour: a ci-shim worker-admit GRANTS, and the grant is
// honestly labelled admission-only rather than dressed up as the real thing.
//
// The three negative assertions are the non-porous half and they are what this
// whole ticket's honesty requirement reduces to. An implementation that granted
// while leaving ScopePath or MemoryMax populated — the obvious "just reuse the
// enforced grant shape" shortcut — passes the positive assertion on its own and
// fails here; and it would fail in production too, because
// WorkerAdmitOutcomeLine refuses to render that combination at all.
func TestShimWorkerAdmitGrantsAdvisoryAdmissionWithNoScopeAndNoCap(t *testing.T) {
	server := shimWorkerServer(t, 1<<30, nil)
	response, proceed := server.evaluateWorkerAdmit(t.Context(), shimWorkerRequest(100<<20))
	if !proceed {
		t.Fatal("evaluateWorkerAdmit abandoned the request")
	}
	if response.State != runner.WorkerAdmitStateGranted || response.Class != runner.WorkerAdmitClassGranted {
		t.Fatalf("state=%q class=%q, want a real grant: AIRA-123 exists because refusing here drops the whole suite to an ungoverned pool",
			response.State, response.Class)
	}
	if response.Containment != runner.WorkerAdmitContainmentAdvisory {
		t.Fatalf("containment=%q, want %q", response.Containment, runner.WorkerAdmitContainmentAdvisory)
	}
	if response.Reserved != 100<<20 {
		t.Fatalf("reserved=%d, want the declared 100MiB booking", response.Reserved)
	}
	if response.ScopePath != "" {
		t.Fatalf("scope_path=%q; there is no cgroup in ledger-only admission and naming one is exactly the lie this ticket forbids", response.ScopePath)
	}
	if response.MemoryMax != 0 {
		t.Fatalf("memory_max=%d; nothing enforces a cap here, so reporting one would be read as containment that does not exist", response.MemoryMax)
	}
	if response.CPUSlots != runner.WorkerAdmitCPUSlotsUnevaluated {
		t.Fatalf("cpu_slots=%q, want %q: the CPU gate counts populated worker cgroups, which cannot exist here",
			response.CPUSlots, runner.WorkerAdmitCPUSlotsUnevaluated)
	}
	// And the line the supervisor actually reads must render, which is the
	// end-to-end proof that the response shape and the channel agree.
	line, err := runner.WorkerAdmitOutcomeLine(
		runner.WorkerAdmitOutcome{State: response.State, Class: response.Class},
		&runner.WorkerAdmitGrantFields{
			WorkerID: response.WorkerID, Containment: response.Containment,
			Reserved: response.Reserved, CPUSlots: response.CPUSlots,
		})
	if err != nil {
		t.Fatalf("the daemon produced a grant its own channel refuses to render: %v", err)
	}
	if _, err := runner.ParseWorkerAdmitOutcomeLine(line); err != nil {
		t.Fatalf("rendered line does not parse: %v (%s)", err, line)
	}
}

// verifies: AIRA-123
//
// THE LEDGER REALLY GATES. Three workers of 400MiB fit a 1GiB ceiling; the
// fourth does not, and is refused RETRIABLY (contended) rather than terminally,
// because another worker retiring genuinely makes room.
//
// THE NON-POROUS HALF is the fourth request. A no-op ledger — one that books
// nothing and admits everything, which is what "ledger-only admission" degrades
// to if the booking is dropped — passes every assertion above it and fails here.
// The RELEASE half is the second counterexample: a ledger that books but never
// releases passes the denial and fails the re-admission.
func TestShimWorkerAdmitLedgerRefusesOverSubscriptionAndFreesOnRelease(t *testing.T) {
	server := shimWorkerServer(t, 1<<30, nil)
	const each = 400 << 20
	var granted []uint64
	for i := 0; i < 2; i++ {
		response, _ := server.evaluateWorkerAdmit(t.Context(), shimWorkerRequest(each))
		if response.State != runner.WorkerAdmitStateGranted {
			t.Fatalf("worker %d: state=%q reason=%q, want granted (2x400MiB fits 1GiB)", i, response.State, response.Reason)
		}
		granted = append(granted, response.leaseID)
	}
	if committed, count := server.ShimWorkerLedgerForTest(); committed != 2*each || count != 2 {
		t.Fatalf("ledger committed=%d leases=%d, want %d over 2 leases", committed, count, int64(2*each))
	}
	third, _ := server.evaluateWorkerAdmit(t.Context(), shimWorkerRequest(each))
	if third.State != runner.WorkerAdmitStateDenied {
		t.Fatalf("third worker state=%q reason=%q; 3x400MiB exceeds a 1GiB ceiling and a ledger that admits it is not a ledger",
			third.State, third.Reason)
	}
	if third.Class != runner.WorkerAdmitClassContended {
		t.Fatalf("third worker class=%q, want %q: a terminal class here strips containment for the whole suite over a transient full ledger",
			third.Class, runner.WorkerAdmitClassContended)
	}
	if third.Reason != runner.WorkerAdmitReasonLedgerBudgetExceeded {
		t.Fatalf("third worker reason=%q, want %q", third.Reason, runner.WorkerAdmitReasonLedgerBudgetExceeded)
	}
	if third.leaseID != 0 {
		t.Fatal("a denied request booked a lease")
	}

	server.releaseShimWorkerLease(granted[0])
	if committed, count := server.ShimWorkerLedgerForTest(); committed != each || count != 1 {
		t.Fatalf("after one release: committed=%d leases=%d, want %d over 1 lease", committed, count, int64(each))
	}
	retried, _ := server.evaluateWorkerAdmit(t.Context(), shimWorkerRequest(each))
	if retried.State != runner.WorkerAdmitStateGranted {
		t.Fatalf("state=%q reason=%q after a release; the freed booking must be re-admittable or a suite stalls forever",
			retried.State, retried.Reason)
	}

	// Idempotent release: the connection handler releases on every exit path,
	// and a double release must not credit the ledger twice.
	server.releaseShimWorkerLease(granted[0])
	server.releaseShimWorkerLease(granted[0])
	if committed, _ := server.ShimWorkerLedgerForTest(); committed != 2*each {
		t.Fatalf("committed=%d after a double release of one lease, want %d unchanged", committed, int64(2*each))
	}
}

// verifies: AIRA-123
//
// The container's LIVE usage still gates admission even with an empty ledger.
// This is what stops the degraded mode being blind to a worker that has grown
// past what it declared: enforcement is gone, observation is not.
func TestShimWorkerAdmitDeniesOnLiveContainerUsageWithAnEmptyLedger(t *testing.T) {
	used := int64(900 << 20)
	server := shimWorkerServer(t, 1<<30, &used)
	response, _ := server.evaluateWorkerAdmit(t.Context(), shimWorkerRequest(200<<20))
	if committed, _ := server.ShimWorkerLedgerForTest(); committed != 0 {
		t.Fatalf("ledger committed=%d; this test's whole point is that the LEDGER is empty", committed)
	}
	if response.State != runner.WorkerAdmitStateDenied || response.Class != runner.WorkerAdmitClassContended {
		t.Fatalf("state=%q class=%q, want a retriable denial: 900MiB in use leaves no room for 200MiB under a 1GiB ceiling",
			response.State, response.Class)
	}
	if response.Reason != runner.WorkerAdmitReasonInsufficientHeadroom {
		t.Fatalf("reason=%q, want %q", response.Reason, runner.WorkerAdmitReasonInsufficientHeadroom)
	}
}

// verifies: AIRA-123
//
// A request larger than the whole budget is TERMINAL-but-daemon-healthy, not a
// poll loop: no amount of waiting makes a 2GiB worker fit a 1GiB container.
func TestShimWorkerAdmitRefusesARequestLargerThanTheWholeBudget(t *testing.T) {
	server := shimWorkerServer(t, 1<<30, nil)
	response, _ := server.evaluateWorkerAdmit(t.Context(), shimWorkerRequest(2<<30))
	if response.State != runner.WorkerAdmitStateDenied || response.Class != runner.WorkerAdmitClassRequestInvalid {
		t.Fatalf("state=%q class=%q, want denied/%s", response.State, response.Class, runner.WorkerAdmitClassRequestInvalid)
	}
	if response.Reason != runner.WorkerAdmitReasonExceedsCeiling {
		t.Fatalf("reason=%q, want %q", response.Reason, runner.WorkerAdmitReasonExceedsCeiling)
	}
}

// verifies: AIRA-123
//
// An unreadable ledger is RETRIABLE, and the direction is load-bearing. The
// recorded budget cannot be absent (a shim daemon refuses to start without a
// positive one), so a failed reading here is transient. Classing it
// admission-unusable would fire _disable_daemon on a momentarily unreadable
// /proc/meminfo and run the rest of the suite with no governance at all —
// AIRA-63's regression shape, one subsystem over.
func TestShimWorkerAdmitReportsAnUnreadableLedgerAsRetriable(t *testing.T) {
	server := shimWorkerServer(t, 1<<30, nil)
	server.admitReadMemory = func(string) (int64, int64, int64, bool, string) {
		return 0, 0, 0, false, "meminfo-unreadable"
	}
	response, _ := server.evaluateWorkerAdmit(t.Context(), shimWorkerRequest(1<<20))
	if response.State != runner.WorkerAdmitStateUnevaluated {
		t.Fatalf("state=%q, want %q: a reading that could not be established is never a zero and never a grant",
			response.State, runner.WorkerAdmitStateUnevaluated)
	}
	if response.Class != runner.WorkerAdmitClassContended {
		t.Fatalf("class=%q, want %q; %q here strips governance for the whole run over a transient read failure",
			response.Class, runner.WorkerAdmitClassContended, runner.WorkerAdmitClassAdmissionUnusable)
	}
	if response.Reason != runner.WorkerAdmitReasonLedgerBudgetUnreadable {
		t.Fatalf("reason=%q, want %q", response.Reason, runner.WorkerAdmitReasonLedgerBudgetUnreadable)
	}
	if committed, _ := server.ShimWorkerLedgerForTest(); committed != 0 {
		t.Fatalf("committed=%d; an unestablished reading must never book", committed)
	}
}

// verifies: AIRA-123
//
// The mode-agreement gate, BOTH directions. A client and a daemon that resolved
// different install-mode records must not transact: a real-mode request served
// by the shim ledger would be admitted with no cgroup while its supervisor tried
// to place workers into one, and the shim sentinel down the real path would be
// read as a cgroup path and fail somewhere far less legible.
//
// Terminal (admission-unusable), because waiting cannot make two durable records
// agree — the one condition in this file where AIRA-121's original disposition
// is still the right one.
func TestWorkerAdmitRefusesAConfineModeMismatchInBothDirections(t *testing.T) {
	shim := shimWorkerServer(t, 1<<30, nil)
	response, _ := shim.evaluateWorkerAdmit(t.Context(), workerAdmitRequest{
		jobID: "job-1", outerScope: "/sys/fs/cgroup/aira.slice/.aira-CONFINE-x", estimatedBytes: 1 << 20,
	})
	if response.State != runner.WorkerAdmitStateUnavailable || response.Class != runner.WorkerAdmitClassAdmissionUnusable {
		t.Fatalf("shim daemon, real-mode request: state=%q class=%q, want unavailable/%s",
			response.State, response.Class, runner.WorkerAdmitClassAdmissionUnusable)
	}
	if response.Reason != runner.WorkerAdmitReasonConfineModeMismatch {
		t.Fatalf("reason=%q, want %q", response.Reason, runner.WorkerAdmitReasonConfineModeMismatch)
	}
	if committed, _ := shim.ShimWorkerLedgerForTest(); committed != 0 {
		t.Fatalf("committed=%d; a refused mismatch must never book", committed)
	}

	real := NewServer(Paths{})
	real.stopping = make(chan struct{})
	_ = newWorkerScopeTree().install(real)
	real.admitReadMemory = func(string) (int64, int64, int64, bool, string) { return 0, 1 << 30, 0, true, "" }
	reverse, _ := real.evaluateWorkerAdmit(t.Context(), shimWorkerRequest(1<<20))
	if reverse.Reason != runner.WorkerAdmitReasonConfineModeMismatch {
		t.Fatalf("real daemon, ci-shim sentinel: state=%q class=%q reason=%q, want the same refusal — a real-mode daemon must never treat %q as a cgroup path",
			reverse.State, reverse.Class, reverse.Reason, runner.ShimConfineSlice)
	}
}

// verifies: AIRA-123
//
// THE LEASE IS THE CONNECTION, end to end through the real connection handler.
// The booking exists while the relay holds its side and is released when the
// peer disconnects.
//
// This is the one deliberate divergence from the real path, whose ledger is
// pointedly NOT connection-scoped (AIRA-41: a killed relay must not free
// capacity while its worker is still alive under a still-intact cap). That fix
// could rely on the cgroup as a better source of truth; there is none here, and
// the connection is the only lifetime signal that exists. The test pins the
// behaviour so the divergence is a recorded decision rather than a discovery.
func TestShimWorkerAdmitConnectionReleasesTheBookingWhenThePeerDisconnects(t *testing.T) {
	server := shimWorkerServer(t, 1<<30, nil)
	serverConn, clientConn := net.Pipe()
	done := make(chan struct{})
	go func() {
		defer close(done)
		defer serverConn.Close()
		server.workerAdmitConnection(serverConn, map[string]any{
			"job_id": "job-1", "outer_scope": runner.ShimConfineSlice,
			"estimated_bytes": float64(100 << 20), "max_wait_ms": float64(0),
		})
	}()
	var frame ResponseFrame
	if err := readFrame(clientConn, &frame); err != nil {
		t.Fatal(err)
	}
	var grant WorkerAdmitResponse
	if err := json.Unmarshal(frame.Data, &grant); err != nil {
		t.Fatal(err)
	}
	if grant.State != runner.WorkerAdmitStateGranted {
		t.Fatalf("grant=%+v, want granted", grant)
	}
	if grant.Containment != runner.WorkerAdmitContainmentAdvisory || grant.ScopePath != "" {
		t.Fatalf("wire grant=%+v; the advisory grade and the absence of a scope must both survive JSON", grant)
	}
	if committed, count := server.ShimWorkerLedgerForTest(); committed != 100<<20 || count != 1 {
		t.Fatalf("while the lease is held: committed=%d leases=%d, want the booking charged", committed, count)
	}
	select {
	case <-done:
		t.Fatal("the connection released before its peer closed; the booking would outlive nothing")
	case <-time.After(20 * time.Millisecond):
	}
	_ = clientConn.Close()
	select {
	case <-done:
	case <-testdeadline.After(time.Second):
		t.Fatal("connection did not release after peer close")
	}
	if committed, count := server.ShimWorkerLedgerForTest(); committed != 0 || count != 0 {
		t.Fatalf("after peer close: committed=%d leases=%d, want 0; a booking nothing releases leaks the container's whole budget one worker at a time",
			committed, count)
	}
}

// verifies: AIRA-123
//
// The wire-level validator accepts the ci-shim sentinel as an outer scope and
// does NOT filepath.Clean it into something that looks like a path — while
// still refusing every other non-absolute value.
func TestValidateWorkerAdmitArgsAcceptsTheShimSentinelUncleaned(t *testing.T) {
	req, err := validateWorkerAdmitArgs(map[string]any{
		"job_id": "job-1", "outer_scope": runner.ShimConfineSlice,
		"estimated_bytes": float64(workerAdmitEstimatedBytesMin), "max_wait_ms": float64(0),
	}, workerAdmitWaitCeilingMs)
	if err != nil {
		t.Fatalf("the sentinel was refused: %v", err)
	}
	if req.outerScope != runner.ShimConfineSlice {
		t.Fatalf("outer_scope=%q, want the sentinel verbatim", req.outerScope)
	}
	if _, err := validateWorkerAdmitArgs(map[string]any{
		"job_id": "job-1", "outer_scope": "relative/path",
		"estimated_bytes": float64(workerAdmitEstimatedBytesMin), "max_wait_ms": float64(0),
	}, workerAdmitWaitCeilingMs); err == nil {
		t.Fatal("a relative outer_scope was accepted; only the one sentinel is exempt from the absolute-path rule")
	}
}
