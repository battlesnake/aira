package daemon

import (
	"context"
	"fmt"
	"strconv"
	"sync"

	"aira/internal/runner"
)

// AIRA-123. Ledger-only worker admission for ci-shim mode.
//
// THE SEPARATION THIS FILE EXISTS TO MAKE. AIRA-121 refused every shim-mode
// worker-admit because worker-admit did one indivisible thing: create a
// kernel-enforced cgroup sub-scope and grant against the sum of those scopes'
// caps. Those are two different jobs wearing one coat. A cgroup gives
// ENFORCEMENT (a kill backstop when a worker exceeds its cap); a ledger gives
// ADMISSION (queue or allow, against a declared-vs-available budget), and only
// the first needs a cgroup. Over-subscription — several workers admitted whose
// declared needs together exceed the RAM available — is the cause of most real
// OOMs, and a ledger prevents it with no cgroup at all.
//
// WHAT IS GENUINELY LOST, stated plainly because the whole ticket turns on not
// hiding it: nothing kills a worker that exceeds what it declared. A worker that
// asks for 500 MiB and grows to 5 GiB is booked at 500 MiB in this ledger and
// runs to whatever the container's own OOM killer does about it. That trade is
// deliberate and is right on a single-tenant CI runner (a disposable VM, no
// desktop to protect, no sibling sessions to collaterally kill: losing the
// backstop costs one job rerun) and would be wrong on a shared desktop, which is
// why it is reachable only in ci-shim mode and is reported as
// `advisory(ci-shim,no-cgroup,no-kill-backstop)` on every grant.
//
// WHAT STILL GOVERNS, and it is not nothing: the live-usage check below reads
// the container's real memory.current (or MemTotal-MemAvailable), so an
// over-grown worker still shows up as pressure and still stops the NEXT
// admission. Enforcement is gone; observation is not.

// THE LEASE IS THE CONNECTION, and that is the one structural difference from
// the real path's ledger worth understanding before changing anything here.
// AIRA-41 deliberately stopped the real ledger being connection-scoped, because
// a killed relay would free capacity while its worker was still alive under a
// still-intact cap — and it could do that because the CGROUP was available as a
// better source of truth (Σ memory.max over the outer scope's real children,
// which survives a daemon restart and a killed relay alike).
//
// In shim mode that source of truth does not exist: there is no cgroup, and the
// daemon never learns the forked worker's pid (the fork happens after the
// grant, in the supervisor). The connection the grant is delivered on, which
// `aira worker-admit` holds open for the worker's whole life, is the only
// lifetime signal there is. So a relay killed out from under a live worker DOES
// free this worker's booking early. That is a real, named weakness of the
// degraded mode rather than an oversight; the live-usage term is what stops it
// becoming unbounded, since the un-booked worker's own RSS still counts against
// the very next admission.

// shimWorkerLedger is the in-daemon sum of currently-admitted workers' declared
// needs, machine-wide (or, more precisely, container-wide: one container, one
// budget, one ledger).
//
// It is keyed by lease id and NOT partitioned per job, deliberately. On the real
// path each job has its own outer scope with its own finite cap, so per-outer-
// scope ledger cells are the natural unit. Here every job in the container draws
// on ONE budget, so two aitest suites running side by side must be counted
// together or each would separately believe it had the whole container.
//
// It does NOT survive a daemon restart, and the real path's ledger does. Said
// plainly rather than papered over: after a restart the sum is zero while the
// already-admitted workers keep running, which opens an over-admission window
// until they retire. The mitigations are the same two the rest of this file
// leans on — the live-usage check still sees those workers' RSS, and every one
// of their relays lost its connection when the daemon went away — and the honest
// statement is that this is weaker than AIRA-74's reserve reconstruction on the
// real path, which had cgroup memory.max values to reconstruct FROM.
//
// A booking records its SIZE and nothing else — no job id, no timestamp. That is
// not an omission to be filled in later: with one container, one budget and one
// queue, "which job does this booking belong to" is a question nothing here can
// act on, and state recorded for a diagnostic that does not exist is state that
// can silently go stale.
type shimWorkerLedger struct {
	mu     sync.Mutex
	seq    uint64
	leases map[uint64]int64
	total  int64
}

// admit books bytes and returns the lease id, or reports fits=false with the
// committed total that refused it. The check and the booking happen under ONE
// lock hold: this is the whole "read the sum, then add to it" atomicity the real
// path gets from its per-outer-scope lock, and splitting it would let two
// concurrent requests both read the same pre-booking sum and both grant.
func (l *shimWorkerLedger) admit(bytes, ceiling int64) (uint64, int64, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.leases == nil {
		l.leases = make(map[uint64]int64)
	}
	// addClamp, never `ceiling - total`: the subtractive form wraps POSITIVE
	// once a term saturates, which is the AIRA-39 over-admit shape.
	if addClamp(l.total, bytes) > ceiling {
		return 0, l.total, false
	}
	l.seq++
	id := l.seq
	l.leases[id] = bytes
	l.total = addClamp(l.total, bytes)
	return id, l.total, true
}

// release drops one lease. Idempotent: the connection handler releases on every
// exit path, and a double release must not double-credit the ledger.
func (l *shimWorkerLedger) release(id uint64) {
	if id == 0 {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	bytes, ok := l.leases[id]
	if !ok {
		return
	}
	delete(l.leases, id)
	l.total -= bytes
	if l.total < 0 {
		l.total = 0
	}
}

// snapshot reports the committed total and the number of live leases, for tests
// and for diagnostics.
func (l *shimWorkerLedger) snapshot() (int64, int) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.total, len(l.leases)
}

// evaluateShimWorkerAdmit is the whole degraded decision. It mirrors the real
// evaluateWorkerAdmit's ORDER and its verdict classes deliberately — the same
// conditions get the same disposition — so that a reader comparing the two sees
// exactly one difference: nothing here creates, reads, or names a cgroup.
func (s *Server) evaluateShimWorkerAdmit(ctx context.Context, req workerAdmitRequest) (WorkerAdmitResponse, bool) {
	select {
	case <-ctx.Done():
		return WorkerAdmitResponse{}, false
	case <-s.stopping:
		return WorkerAdmitResponse{}, false
	default:
	}
	// The ledger's live reading, through the SAME seam ordinary shim admission
	// uses (readShimMemory): the container's own cgroup memory.current when
	// install recorded one, else /proc/meminfo, with `max` already clamped to the
	// recorded budget. Nothing is reimplemented here and nothing is estimated.
	used, budget, reclaimable, ok, reason := s.memoryReader()(runner.ShimConfineSlice)
	if !ok {
		// CONTENDED, i.e. retriable, and the reasoning is the opposite of
		// AIRA-121's terminal refusal: a shim daemon cannot start without a
		// positive recorded budget, so an unreadable ledger here is a transient
		// read failure and waiting really can change the answer. The caller's own
		// max_wait still converts a persistent failure into a timeout.
		return WorkerAdmitResponse{
			State:  runner.WorkerAdmitStateUnevaluated,
			Class:  runner.WorkerAdmitClassContended,
			Reason: runner.WorkerAdmitReasonLedgerBudgetUnreadable,
			Detail: "ci-shim ledger unreadable: " + reason,
		}, true
	}
	if reclaimable < 0 {
		reclaimable = 0
	}
	used = subtractFloor(used, reclaimable)
	headroom := s.workerAdmitHeadroom
	if headroom < 0 {
		headroom = 0
	}
	ceiling := subtractFloor(budget, headroom)
	if req.estimatedBytes > ceiling {
		// Could never fit even against an empty ledger and an idle container: a
		// stable fact about THIS request. request-invalid is terminal-but-
		// daemon-healthy, exactly as on the real path, so the supervisor fails
		// the affected work loudly instead of polling for room that cannot exist.
		return WorkerAdmitResponse{
			State:  runner.WorkerAdmitStateDenied,
			Class:  runner.WorkerAdmitClassRequestInvalid,
			Reason: runner.WorkerAdmitReasonExceedsCeiling,
			Detail: fmt.Sprintf("estimated %d bytes exceeds the ci-shim budget's %d-byte ceiling", req.estimatedBytes, ceiling),
		}, true
	}
	// LIVE-USAGE check, first and separately from the ledger check below —
	// deliberately NOT summed with it. The two measure overlapping things (a
	// worker that has grown into its booking is in both `used` and the committed
	// total), so adding them would double-charge every settled worker and stall a
	// perfectly healthy suite. The real path splits them for the same reason.
	if addClamp(req.estimatedBytes, used) > ceiling {
		return WorkerAdmitResponse{
			State:  runner.WorkerAdmitStateDenied,
			Class:  runner.WorkerAdmitClassContended,
			Reason: runner.WorkerAdmitReasonInsufficientHeadroom,
			Detail: fmt.Sprintf("container usage %d + estimated %d exceeds the %d-byte ceiling", used, req.estimatedBytes, ceiling),
		}, true
	}
	// THE LEDGER CHECK, and the booking, under one lock hold.
	leaseID, committed, fits := s.shimWorkers.admit(req.estimatedBytes, ceiling)
	if !fits {
		// Retriable: another worker retiring releases its booking and this
		// request then fits — the same disposition (and the same reasoning) as
		// the real path's aggregate-cap-exceeded.
		return WorkerAdmitResponse{
			State:  runner.WorkerAdmitStateDenied,
			Class:  runner.WorkerAdmitClassContended,
			Reason: runner.WorkerAdmitReasonLedgerBudgetExceeded,
			Detail: fmt.Sprintf("admitted workers already book %d of the %d-byte ceiling", committed, ceiling),
		}, true
	}
	return WorkerAdmitResponse{
		State: runner.WorkerAdmitStateGranted, Class: runner.WorkerAdmitClassGranted,
		WorkerID:    strconv.FormatUint(leaseID, 10),
		Containment: runner.WorkerAdmitContainmentAdvisory,
		Reserved:    req.estimatedBytes,
		// ScopePath and MemoryMax stay ZERO, and the outcome renderer REFUSES an
		// advisory grant that carries either. There is no cgroup to name and no
		// cap to report.
		//
		// CPUSlots is unevaluated rather than absent or ok, and that is the
		// honest reading rather than a shortcut: the CPU gate counts POPULATED
		// worker cgroups under the slice (cpuSlotsDecide), which structurally
		// cannot exist here, so CPU-concurrency governance genuinely did not
		// happen for this grant. supervisor.py says so once on the run's own
		// output. SwapCap is left EMPTY, which the channel defines as "this
		// daemon has nothing to say about swap" — the alternative, `unavailable`,
		// is a claim that a swap cap was attempted and could not be established,
		// and nothing here attempts one.
		CPUSlots: runner.WorkerAdmitCPUSlotsUnevaluated,
		leaseID:  leaseID,
	}, true
}

// releaseShimWorkerLease drops a granted advisory booking. Called from the
// connection handler's single deferred exit, so every path — a clean worker
// retirement, a killed relay, a failed response write, a stopping daemon —
// releases exactly once.
func (s *Server) releaseShimWorkerLease(id uint64) { s.shimWorkers.release(id) }

// ShimWorkerLedgerForTest reports the committed total and live lease count.
func (s *Server) ShimWorkerLedgerForTest() (int64, int) { return s.shimWorkers.snapshot() }
