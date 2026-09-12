package daemon

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"math"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"aira/internal/core"
	"aira/internal/runner"
)

const (
	// admitWaitCeilingMs is runner.AdmitWaitCeiling expressed in milliseconds —
	// ONE shared ceiling across CLI, runner and daemon (AIRA-58). It replaced a
	// hardcoded 30-minute cap applied by SILENT SUBSTITUTION here and in two
	// other places; over-ceiling requests are now refused and told the bound.
	admitWaitCeilingMs = int64(runner.AdmitWaitCeiling / time.Millisecond)
	// S15 deleted workerAdmitWaitCeilingMs: a worker-admit CLAIM is a blocking
	// lease with no daemon-side timeout (design §4/§6), exactly like the confine
	// admit path, so there is no wait to ceiling-check.
	admitMaxWaiters                           = 256
	admitGlobalMax                            = 1024
	admitMaxReserve                     int64 = 1 << 50
	admitWriteTimeout                         = 5 * time.Second
	admitHistoryTimeout                       = 250 * time.Millisecond
	admitPriorRefresh                         = time.Minute
	admitConfineScanIntervalDefault           = time.Second
	admitSliceHeadroomBaseDefault       int64 = 2 << 30
	admitSliceHeadroomSupervisorDefault int64 = 64 << 20
	delegateRAMScopeMinDefault          int64 = 4 << 30
	delegateRAMScopeSafetyPct           int64 = 15

	// admitExclusiveWaitCeilingDefault bounds how long an EXCLUSIVE request may
	// drain the slice. It is deliberately far below the shared 24-hour
	// AdmitWaitCeiling: an exclusive request holds up every other session on this
	// machine while it drains, so a day-long drain is not a wait, it is an outage.
	// Enforced by REFUSAL, never silent substitution (the AIRA-58 rule).
	admitExclusiveWaitCeilingDefault = 30 * time.Minute
)

// admitExclusiveWaitCeiling is the effective exclusive ceiling. It can never
// exceed the shared ceiling: an override above it would be meaningless (the
// general validation refuses first) and would misreport the bound in its own
// refusal message.
func admitExclusiveWaitCeiling() time.Duration {
	ceiling := admitExclusiveWaitCeilingDefault
	if raw := strings.TrimSpace(os.Getenv("AIRA_ADMIT_EXCLUSIVE_WAIT_CEILING")); raw != "" {
		parsed, err := time.ParseDuration(raw)
		switch {
		case err != nil || parsed <= 0:
			// SAY SO rather than silently falling back. A silent substitution is
			// exactly the defect AIRA-58 removed from this path: an operator who set
			// a value and got a different one had no way to learn it.
			admitExclusiveCeilingWarnOnce.Do(func() {
				log.Printf("aira daemon: AIRA_ADMIT_EXCLUSIVE_WAIT_CEILING=%q is not a positive duration; using the %s default", raw, admitExclusiveWaitCeilingDefault)
			})
		case parsed > runner.AdmitWaitCeiling:
			admitExclusiveCeilingWarnOnce.Do(func() {
				log.Printf("aira daemon: AIRA_ADMIT_EXCLUSIVE_WAIT_CEILING=%s exceeds the shared admission ceiling %s; using the shared ceiling", parsed, runner.AdmitWaitCeiling)
			})
			ceiling = runner.AdmitWaitCeiling
		default:
			ceiling = parsed
		}
	}
	if ceiling > runner.AdmitWaitCeiling {
		return runner.AdmitWaitCeiling
	}
	return ceiling
}

var admitExclusiveCeilingWarnOnce sync.Once

// Exclusive-state vocabulary, shared by the rejection payload, the `--list`
// summary and the blocked launcher's progress line. Exact-match tokens.
const (
	admitExclusiveDraining = "draining"
	admitExclusiveHeld     = "held"
)

// exclusiveStateOf names which half of the exclusive lifecycle a waiter is in.
// Empty for a waiter that is not asserting exclusivity at all, so callers can
// treat "" as an honest absence rather than a third state.
func exclusiveStateOf(waiter *admitWaiter) string {
	if !waiter.exclusiveActive() {
		return ""
	}
	if waiter.state == admitGranted {
		return admitExclusiveHeld
	}
	return admitExclusiveDraining
}

type admitWaiterState uint8

const (
	admitQueued admitWaiterState = iota
	admitGranted
	admitReleased
	admitRejected
)

type admitWaiter struct {
	seq     int64
	reserve int64
	// S5. cpu is this lease's declared CPU-core reservation, the second ledger
	// resource beside reserve (RAM). {reserve, cpu} is the resource vector admission
	// gates conjunctively (design §7). CPU is accounting-only: it charges the
	// per-slice queue.cpuOutstanding ledger and NO cpu.max is ever written — the
	// kernel time-shares on cpu.weight. Zero for a lease that declared no cores (a
	// delegate SUITE reserves 0 cores; §8); the confine client's default is one core.
	cpu       int64
	state     admitWaiterState
	grantedCh chan struct{}
	enqueued  time.Time
	grantedAt time.Time
	waited    bool
	accounted bool
	outcome   string
	reason    string
	waitedMS  int64
	basis     string
	scopeID   string
	name      string
	owner     string
	// scopeCeiling is the delegate-ram scope's resolved memory.max (AIRA-15),
	// zero for every other class. Set at construction under queue.mu — see
	// admitRequest.scopeCeiling for why that moved — and carried to the launcher
	// on the grant response (AdmitResponse.ScopeCeiling) so it sizes the scope cap.
	scopeCeiling int64

	// AIRA-108. A BOUNDED copy of the client's own resource signature, retained
	// purely so `confine --list` can NAME a scope-less reservation rather than
	// report it only as one more unit in an aggregate. It takes part in NO
	// admission decision — resolveAdmitReserve reads request.signature directly,
	// and always did — so a malformed or hostile value can only ever affect what
	// an operator is shown.
	//
	// BOUNDED is load-bearing, not tidiness (found by Sol build-review).
	// validateAdmitArgs accepts `signature` as any string, so its only real limit
	// is the 16 MiB admit frame. Retaining it verbatim would put an
	// attacker-chosen multi-megabyte string on a LONG-LIVED waiter (up to
	// admitMaxWaiters of them) and, worse, into every confine-list reply: a
	// handful of such rows exceeds MaxFrameBytes, writeFrame refuses the WHOLE
	// response, and `confine --list` stops working for every job on that slice —
	// an availability defect introduced by a diagnostic. Truncation happens HERE,
	// at the one place the value is retained, so the memory and the wire are
	// bounded together; the renderer's escaping is a second, narrower bound for
	// the terminal.
	signature string

	// AIRA-101. Exclusivity is a DERIVED property of this waiter, never a
	// standalone flag on the queue or the server. That is the whole crash-safety
	// argument: `draining` is "some waiter has exclusive && state == admitQueued",
	// `held` is "some waiter has exclusive && state == admitGranted", both
	// recomputed from queue.waiters on every pass. So exclusivity cannot outlive
	// the waiter; the waiter cannot outlive its admission connection
	// (admitConnection's deferred release runs on every return path) or its own
	// max_wait; and a daemon restart begins with an empty admitQueues map, so
	// nothing survives it — fail-OPEN. There is deliberately no state an operator
	// could ever need to clear, because a wedge of this machine-wide slice must be
	// UNREPRESENTABLE, not merely avoided by careful coding.
	//
	// exclusiveHolder is the nesting token: a scope id naming the exclusive holder
	// this request belongs to. It exempts the holder's OWN nested `aira confine`
	// calls from the hold they would otherwise deadlock against — CLAUDE.md
	// requires every heavy command be confined, and a nested call resolves to the
	// same slice, so without this a benchmark blocks on its own exclusivity.
	//
	// parentScopeID marks a SUB-RESERVATION (`aira confine-reserve`) as an
	// already-running job's internal progress. It is what keeps a drain
	// converging: without it a drain blocks every per-test reservation of every
	// running --delegate-ram suite, so those suites cannot finish and the drain
	// never completes. It cannot be replaced by "has no scope id" — `aira run` is
	// also scope-less and IS new job-level work that a drain must block.
	exclusive       bool
	exclusiveHolder string
	parentScopeID   string

	// AIRA-185. exclusiveReason is the holder's own free-text label for WHY the
	// slice is held ("deploy: slice-ceiling flip"), which the identity fields
	// cannot carry: `name` must be a valid confine identity and must match the
	// scope id. Set at construction under queue.mu, like every other waiter field,
	// and read only by the snapshot walk.
	//
	// It is DIAGNOSTIC ONLY, on exactly the terms `signature` above is: no
	// admission, gate, emptiness or reaping decision reads it, so a malformed or
	// hostile value can only change what an operator is shown. Bounded at the one
	// place it is retained (validateAdmitArgs -> boundedAdmitReason), so the
	// memory and the wire are bounded together and a multi-megabyte label cannot
	// push a `confine --list` reply past MaxFrameBytes.
	exclusiveReason string

	// AIRA-149. DIAGNOSIS ONLY: neither field is read by any admission or grant
	// decision, and both are written ONLY inside evaluateAdmitQueue's existing
	// refusal branches, under queue.mu -- the same single-writer discipline as
	// every other evaluator-maintained field on this struct.
	//
	// contention is LATCHED ACROSS THE WHOLE WAIT, never sampled at the instant
	// of rejection, and that is the point of it existing at all: a waiter blocked
	// behind a real job for 29 of its 30 seconds and alone when the timer fires
	// must still be told the truth. It holds the monotone lattice below, joined
	// with max() at every refusal, so `observed` is sticky, a single
	// unestablished pass forbids `none-observed` for the whole wait, and
	// `none-observed` survives only if EVERY pass positively established
	// solitude.
	//
	// lastGrantable is the checkedAvailable figure the capacity gate last
	// computed for THIS waiter -- the number that actually explains the refusal.
	// A pointer, so a waiter no pass ever evaluated reports nothing rather than a
	// fabricated zero.
	contention    int
	lastGrantable *int64

	// S8 (restart/anchor state — design §3 compare-and-release, §4 restart).
	//
	// anchor is the connection that CURRENTLY owns this lease (design §3 Inv 4): a
	// lease is released ONLY by the EOF of its current-anchor connection. Each
	// (re-)anchor through anchorLeaseLocked OVERWRITES it under queue.mu, so a
	// re-declare on a new connection makes a stale old connection's later EOF a
	// no-op — releaseAdmitWaiterLockedAnchored discharges only when the releasing
	// connection still IS the anchor. The compare is direct identity on the live
	// net.Conn the handler holds for the lease's whole lifetime: a handler never
	// releases a conn it is not still holding, so an equal-or-recycled value cannot
	// arise, and there is NO separate "which generation am I" value for the
	// releasing connection to capture in a second critical section. (A monotone
	// generation — bumped on re-anchor, captured by each connection for its later
	// release — was the builder's first cut; Fable found it reintroduced the very
	// lost-lease race it meant to close, because the capture and the bump were
	// separate critical sections. Conn identity is the plan's own wording,
	// "discharge only if anchor==thisConn", and has no capture window.)
	//
	// clientPID / processStartTick are the peer's pid and /proc start-tick, read
	// from the connection at anchor time (zero when the peer credential is unreadable,
	// e.g. a net.Pipe test connection with no injected credential seam). Since S13
	// deleted the dump/reload layer they are diagnostic only — no restart path reads
	// them back — but a granted lease is always anchored to a live connection, so they
	// remain the identity of that connection's peer.
	anchor           net.Conn
	clientPID        int
	processStartTick uint64
}

// ledgerCharge is what this waiter contributes to queue.outstanding: its
// DECLARED reserve, for the whole lifetime of the lease. The admission model is
// declared-only (there is no live-usage tracking) -- over-declaring is a caller
// error, and the peak-RSS estimate sizes the declared reserve for a known
// command. queue.mu must be held.
//
// Every ledger site goes through this -- rederiveLedgerLocked's sum after each
// grant and release, and admitSliceSnapshotFor's sums -- so
// "outstanding == sum of ledgerCharge over granted && accounted waiters" is one
// statement about one function, not an agreement between call sites that must
// be maintained by hand.
func (w *admitWaiter) ledgerCharge() int64 {
	if w == nil {
		return 0
	}
	return w.reserve
}

// rederiveLedgerLocked recomputes the queue's ledger from its waiters: the sum
// of ledgerCharge() over every granted && accounted waiter, and the count of
// them. queue.mu must be held.
//
// It is the ONE writer of queue.outstanding / queue.outstandingJobs, called
// after every grant and every release. Those fields are therefore a re-derived
// CACHE of the waiter set -- `available = ceiling - Σleases` derived from the
// leases themselves (design §2) -- never a running total mutated in two places.
// A grant that flips a waiter to granted && accounted, or a release that removes
// it (and marks it admitReleased), changes exactly one term of this sum, so the
// derived figure equals what the old `+=` / `-=` counter produced for any real
// waiter set; what changes is only that nothing can now set the scalar out of
// step with the waiters it is supposed to describe.
//
// The scope-id keying the design names IS the waiter set: enqueueAdmitInternal
// refuses a queued/rejected duplicate scope id and re-anchors a granted one in place
// (see leaseByScopeIDLocked), so at most one non-released waiter carries any scope id.
// A separate per-queue map keyed by scope id would
// be a second copy of that fact to keep in sync -- the double-mutated state §2
// exists to remove -- and scope-less `confine-reserve` waiters (scopeID == "")
// have no key at all, so the walk over waiters is the honest ledger here.
//
// S5. It re-derives BOTH ledger resources in the one walk: outstanding (RAM, via
// ledgerCharge) and cpu (Σ lease cores). Both are PER-SLICE caches the caller stores
// on the queue (queue.outstanding and queue.cpuOutstanding); CPU is gated per slice
// on the one-slice (aira.slice) assertion (D1). jobs counts either resource's
// granted && accounted waiters (they are the same set).
func rederiveLedgerLocked(queue *sliceQueue) (outstanding int64, cpu int64, jobs int) {
	for _, waiter := range queue.waiters {
		if waiter == nil || waiter.state != admitGranted || !waiter.accounted {
			continue
		}
		outstanding += waiter.ledgerCharge()
		cpu = addClamp(cpu, waiter.cpu)
		jobs++
	}
	return outstanding, cpu, jobs
}

// exclusiveActive reports whether this waiter currently asserts exclusivity.
//
// The two states are named explicitly rather than spelled `!= admitReleased`,
// and that is load-bearing in both uses (the derived drain/hold predicates, and
// enqueueAdmitInternal's single-exclusive refusal). A REJECTED waiter — timed
// out, or aborted by the unestablished-emptiness rule — whose handler has not yet
// returned to run its deferred release would otherwise keep asserting
// exclusivity. In the refusal path that would reject every future exclusive
// request on the slice until a daemon restart: an unbounded feature-level wedge,
// reintroduced by the very guard meant to simplify things.
func (w *admitWaiter) exclusiveActive() bool {
	return w != nil && w.exclusive && (w.state == admitQueued || w.state == admitGranted)
}

// isSubReservation reports whether this waiter is an already-running job's
// internal sub-reservation rather than new job-level work. See parentScopeID.
func (w *admitWaiter) isSubReservation() bool {
	return w != nil && w.parentScopeID != ""
}

// exclusiveGate is the derived drain/hold state of one queue, recomputed under
// queue.mu on every evaluator pass and never stored between passes.
type exclusiveGate struct {
	holder   *admitWaiter
	draining *admitWaiter
}

// exclusiveGateLocked derives the gate. queue.mu must be held.
//
// At most one waiter can match either slot, because enqueueAdmitInternal refuses
// a second exclusive request while one is active — so this cannot silently pick
// an arbitrary one of several.
func exclusiveGateLocked(queue *sliceQueue) exclusiveGate {
	var gate exclusiveGate
	for _, waiter := range queue.waiters {
		if !waiter.exclusiveActive() {
			continue
		}
		if waiter.state == admitGranted {
			if gate.holder == nil {
				gate.holder = waiter
			}
			continue
		}
		if gate.draining == nil {
			gate.draining = waiter
		}
	}
	return gate
}

// belongsToHolder reports whether waiter W is the exclusive holder or was
// launched from inside it (carrying its token). Used both for the admission gate
// and, via the scope-id, for the worker-admit gate. queue.mu must be held.
func (g exclusiveGate) belongsToHolder(waiter *admitWaiter) bool {
	if g.holder == nil || waiter == nil {
		return false
	}
	return waiter == g.holder || (waiter.exclusiveHolder != "" && waiter.exclusiveHolder == g.holder.scopeID)
}

// holderScopeIDs is the set of scope ids that count as the holder's own work:
// the holder itself plus every granted waiter carrying its token (a nested `aira
// confine` launched from inside the benchmark). queue.mu must be held.
func (g exclusiveGate) holderScopeIDs(queue *sliceQueue) map[string]struct{} {
	ids := make(map[string]struct{}, 2)
	if g.holder == nil {
		return ids
	}
	if g.holder.scopeID != "" {
		ids[g.holder.scopeID] = struct{}{}
	}
	for _, waiter := range queue.waiters {
		if waiter == nil || waiter.state != admitGranted || waiter.scopeID == "" {
			continue
		}
		if waiter.exclusiveHolder != "" && waiter.exclusiveHolder == g.holder.scopeID {
			ids[waiter.scopeID] = struct{}{}
		}
	}
	return ids
}

// sliceProvablyEmpty reports whether the slice holds no other admitted job:
// Σleases == 0, i.e. no granted && accounted waiter. queue.mu must be held.
//
// S14 rewired this from a cgroup-scan reading (the deleted subtree-population
// counters) to the signed ledger. Emptiness is now exactly "the daemon
// holds no lease for this slice", derived from the same connection-held ledger
// `available = ceiling − Σleases` is (design §2/§3). outstandingJobs is
// rederiveLedgerLocked's count of granted && accounted waiters, so this equals
// Σleases == 0 verbatim: a grant sets state == admitGranted and accounted == true
// together (nothing ever sets accounted false on a granted waiter), so the count
// can never lag the leases it describes.
//
// ACCEPTED COVERAGE GAP (D5 — owner 2026-09-11, accepted rather than engineered
// around). Emptiness is a claim about LEASES, never about resident RAM:
//
//   - An orphaned RAM holder that lost its lease — a SIGKILLed supervisor whose
//     worker reparents and survives (design §3) — reads as empty here, so
//     `--exclusive` could be granted beside it. Bounded, not airtight: oom.group
//     fires on memory pressure, not supervisor death, so the MemAvailable
//     watchdog and per-scope OOM backstop are the only net, bounded by the
//     orphan's lifetime (design §3, §11).
//   - Anything not admitted through AIRA at all — a process placed in the slice
//     by hand, or a Docker container under /system.slice/docker-<id>.scope — is
//     outside the ledger by construction. `--exclusive`'s own help text says so;
//     exclusivity is never a claim about those.
//
// This is a deliberate LOOSENING from the pre-S14 scan reading, which could see a
// running scope that held no lease (a post-restart survivor before it
// re-declared) and refuse exclusivity beside it. Under the socket-liveness model
// a survivor re-declares and re-anchors its lease inside the restart freeze
// (design §4), so the ledger counts it; the only residue is the orphan gap above.
func sliceProvablyEmpty(queue *sliceQueue) bool {
	return queue.outstandingJobs == 0
}

// AIRA-149. The three-valued contention lattice, and the ONE reading that
// writes it. Diagnosis only: nothing here takes part in any admission or grant
// decision, and this file's readings are consumed, never computed or altered.
//
// The lattice is MONOTONE and joined with max(), which is the whole transition
// rule:
//
//	observed > unevaluated > none-observed
//
// so `observed` is sticky (a wait spent behind a real job is reported as such
// even if the slice is empty when the timer fires), a single pass that could
// not establish solitude forbids `none-observed` for the whole wait, and
// `none-observed` survives only when EVERY pass positively established it.
const (
	contentionUnset        = 0
	contentionNoneObserved = 1
	contentionUnevaluated  = 2
	contentionObserved     = 3
)

// admitContentionToken renders a latched reading as its wire token. An
// UNSET latch -- a waiter no pass ever evaluated -- is `unevaluated`, never
// "nothing was in the way": absence of a reading is not a reading.
func admitContentionToken(contention int) string {
	switch contention {
	case contentionObserved:
		return "observed"
	case contentionNoneObserved:
		return "none-observed"
	default:
		return "unevaluated"
	}
}

// joinContentionLocked raises this waiter's latch to at most the given reading.
// queue.mu must be held.
func (w *admitWaiter) joinContentionLocked(reading int) {
	if w != nil && reading > w.contention {
		w.contention = reading
	}
}

// noteGrantableLocked records the capacity the gate just computed for this
// waiter. queue.mu must be held. Written only where `available` was actually
// computed, so an absent value means "no pass ever measured this".
func (w *admitWaiter) noteGrantableLocked(available int64) {
	if w == nil {
		return
	}
	value := available
	w.lastGrantable = &value
}

// soloReadingLocked is the emptiness reading for ONE refusal pass, feeding the
// AIRA-149 contention diagnosis. queue.mu must be held.
//
// It reads the same ledger emptiness (sliceProvablyEmpty == Σleases == 0) the
// exclusive gate does. S14 removed the cgroup scan, so there is no longer an
// "emptiness the daemon could not establish" case: the ledger is always readable
// under queue.mu, so this returns observed or none-observed and never
// unevaluated. (A waiter no pass ever evaluated still renders `unevaluated`
// through its UNSET latch — a different absence, upstream of this reading.)
//
// Job counts, not bytes: a residual page in the slice is NOT another job, which
// is exactly the misdiagnosis AIRA-149 is about (the measured case had
// current=4096 with zero jobs).
func soloReadingLocked(queue *sliceQueue, queuedAhead int) int {
	if !sliceProvablyEmpty(queue) {
		return contentionObserved
	}
	// AHEAD is the load-bearing word. queuedAhead counts still-queued waiters
	// already examined in THIS pass, i.e. genuinely ahead of this request; a
	// waiter granted earlier in the same pass has already incremented
	// outstandingJobs, so sliceProvablyEmpty catches it.
	if queuedAhead > 0 {
		return contentionObserved
	}
	return contentionNoneObserved
}

// exclusiveGateStateLocked renders the queue's exclusive state as its wire
// token, or "" when no exclusivity is active. queue.mu must be held. The empty
// string is an honest absence — derived from the same waiter list as everything
// else — never "unknown".
func exclusiveGateStateLocked(queue *sliceQueue) string {
	gate := exclusiveGateLocked(queue)
	if gate.holder != nil {
		return admitExclusiveHeld
	}
	if gate.draining != nil {
		return admitExclusiveDraining
	}
	return ""
}

// confineScopeDirName maps a scope id to its cgroup directory name. Defined once
// so the worker-admit gate and the scanner cannot disagree about the mapping.
func confineScopeDirName(scopeID string) string {
	return ".aira-" + scopeID
}

// S15 deleted the bespoke exclusiveDeniesWorkerAdmit gate. A worker is now an
// ordinary sub-reservation lease on the unified ledger (parentScopeID = the suite
// scope-id, from the explicit parent_scope_id wire field), so the shared exclusivity
// gate handles it:
// exclusiveGate.blocks exempts a sub-reservation from a DRAIN unconditionally and
// from a HOLD when its parent is the holder's own work — the exact behaviour this
// gate provided, now expressed once for every lease rather than duplicated.

// blocks reports whether the exclusivity gate requires this queued waiter to
// stay queued. queue.mu must be held.
//
// It never grants anything: a false return only means exclusivity has no
// objection, after which the waiter still faces every pre-existing check.
func (g exclusiveGate) blocks(queue *sliceQueue, waiter *admitWaiter) bool {
	if g.holder == nil && g.draining == nil {
		return false
	}
	// A SUB-RESERVATION is an already-running job's internal progress, not new
	// work entering the slice. Blocking these is what would stop a drain from
	// ever converging: every per-test reservation of every running --delegate-ram
	// suite would stall for its full wait and then run UNCHARGED, so those suites
	// could not finish, so the slice could not drain.
	if waiter.isSubReservation() {
		if g.holder == nil {
			// Draining only: exempt unconditionally.
			return false
		}
		// Held: only the holder's own sub-reservations pass, which is what lets
		// `--exclusive --delegate-ram -- pytest` reserve for its own tests.
		_, own := g.holderScopeIDs(queue)[waiter.parentScopeID]
		return !own
	}
	if g.holder != nil {
		return !g.belongsToHolder(waiter)
	}
	// Draining. Only the drain head may proceed, and only onto a slice whose
	// emptiness the daemon can positively establish.
	if waiter != g.draining {
		return true
	}
	return !sliceProvablyEmpty(queue)
}

type sliceQueue struct {
	mu      sync.Mutex
	path    string
	waiters []*admitWaiter
	// outstanding / cpuOutstanding / outstandingJobs are a DERIVED CACHE of the
	// ledger, not a running total: their only writer is rederiveLedgerLocked, called
	// after every grant and release to re-sum over the granted && accounted waiters
	// (design §2, `available = ceiling - Σleases`). Never mutate them directly;
	// change the waiter set and re-derive.
	//
	// S5. cpuOutstanding is the PER-SLICE CPU sum (Σ lease cores), the second ledger
	// resource beside RAM's `outstanding`, maintained and consulted exactly as RAM
	// is. CPU is treated PER SLICE — this asserts ONE slice (aira.slice) in practice
	// (D1, resolved to per-slice). Cores are a machine-wide resource; if multiple
	// concurrent slices are ever introduced, CPU accounting must become machine-wide
	// (sum across slices) — revisit then. Per-slice keeps it parallel to RAM and
	// makes cross-slice ledger-clobber unrepresentable.
	outstanding     int64
	cpuOutstanding  int64
	outstandingJobs int
	seq             int64
	kick            chan struct{}
	stop            chan struct{}
	stopOnce        sync.Once
	// stopped is closed by runEvaluator as it exits, so a caller can establish
	// that the goroutine is GONE rather than merely asked to stop.
	//
	// It exists for the tests, and the reason is a real invariant rather than
	// convenience: in production this queue's own goroutine is the sole caller of
	// evaluateAdmitQueue (the single-writer property its state relies on). A test
	// that drove passes directly while that goroutine was still live would race
	// that writer, so it must be able to retire the goroutine and know when.
	stopped chan struct{}
	poll    time.Duration
	server  *Server

	// AIRA-59 fairness-freeze duty cycle, held as a SINGLE anchor instant so the
	// phase is DERIVED, never stored: idle while zero, hold for the first
	// maxHold after it, yield for the second, idle again after.
	//
	// There are exactly TWO writes, and neither can lengthen a hold:
	//   1. the arm, guarded by derived phase == idle;
	//   2. the completed-cycle clear, taken ONLY when the derived phase is already
	//      idle (the cycle is over), which forces one backfilling pass before the
	//      next arm. Because it fires only from derived-idle it can shorten the
	//      gap between cycles, never extend a freeze.
	//
	// That representation is load-bearing, not stylistic. With a stored phase plus
	// a mutable deadline, three separate defects were expressible and had to be
	// forbidden in prose: renewing an active hold every pass (freeze becomes
	// permanent one tick at a time), clearing the phase when the head happens to
	// fit, and re-anchoring on holder change — each of which silently restores the
	// ~100% freeze this bounds. Derived from one anchor, none of them can be
	// written. The phase is QUEUE-LEVEL and independent of which waiter is
	// protected: anchoring to a holder let a stream of unfittable heads (staggered
	// merge-gates, or the retry loop AIRA-58 forced on callers) each claim a fresh
	// full hold.
	//
	// Lifetime note: an empty queue is deleted by pruneAdmitQueue, so this anchor
	// lives only as long as the queue. A yield cut short by quiescence is not a
	// fairness leak — an empty queue has nobody to starve — but it does mean the
	// 50% bound is over intervals where the queue is continuously non-empty.
	freezeArmedAt   time.Time
	freezeHolderSeq int64            // diagnostics only; never affects timing
	freezeLogged    admitFreezePhase // last phase logged, so logs are transitions
}

// admitFreezePhaseAt derives the duty-cycle phase from the anchor instant. Held
// separate and pure so it is directly testable and so no caller can invent a
// fourth state. maxHold <= 0 means the duty cycle is disabled entirely.
func admitFreezePhaseAt(armedAt, now time.Time, maxHold time.Duration) admitFreezePhase {
	if maxHold <= 0 || armedAt.IsZero() {
		return admitFreezeIdle
	}
	elapsed := now.Sub(armedAt)
	if elapsed < maxHold {
		// Includes a negative elapsed (clock moved backwards): treat as
		// just-armed rather than silently skipping the protective hold.
		return admitFreezeHold
	}
	// Subtracting first avoids overflowing on 2*maxHold for an absurd setting.
	if elapsed-maxHold < maxHold {
		return admitFreezeYield
	}
	return admitFreezeIdle
}

type admitFreezePhase uint8

const (
	admitFreezeIdle admitFreezePhase = iota
	admitFreezeHold
	admitFreezeYield
)

func (p admitFreezePhase) String() string {
	switch p {
	case admitFreezeHold:
		return "hold"
	case admitFreezeYield:
		return "yield"
	default:
		return "idle"
	}
}

var sliceMemoryStatDegradeOnce sync.Once

// AdmitResponse is the one grant payload sent before the daemon holds the
// connection as the reservation lease.
type AdmitResponse struct {
	State    string `json:"state"`
	Reason   string `json:"reason,omitempty"`
	WaitedMS int64  `json:"waited_ms"`
	Reserve  int64  `json:"reserve"`
	// S5. Cpu echoes the granted CPU-core reservation the ledger charged. Purely
	// informational (the client applies no cpu.max); it keeps the grant wire
	// symmetric with the {ram, cpu} request vector. omitempty, so a 0-core grant (a
	// delegate suite; §8) carries no field and older readers are unaffected.
	Cpu          int64  `json:"cpu,omitempty"`
	Basis        string `json:"basis"`
	ScopeCeiling int64  `json:"scope_ceiling,omitempty"`
}

type admitRequest struct {
	slice   string
	reserve int64
	// S5. cpu is the declared CPU-core reservation, the second ledger resource. It
	// is optional on the wire (absent → 0; a lease that declares no cores is charged
	// none — the confine client sends DefaultConfineCPUCores). A value that exceeds
	// 2×NumCPU is impossible on this box and is refused fail-fast in admitConnection
	// before any enqueue (design §7 "RequestInvalid").
	cpu     int64
	maxWait int64
	// nonBlocking is set when max_wait_ms is present on the wire AND equals 0 (design
	// §6 non-blocking mode): the request does not wait — a zero deadline returns the
	// current snapshot at once. max_wait_ms ABSENT (the S13 client sends none) means an
	// ordinary BLOCKING wait with NO timeout: only a grant, daemon stop, or the client
	// closing its connection ends it (§4/§6). A positive max_wait_ms is accepted but no
	// longer imposes a timeout (the request blocks); the wait ceilings still validate
	// it (a §6 collision flagged for the owner — the ceilings are now vestigial).
	nonBlocking bool
	signature   string
	pinned      bool
	scopeID     string
	name        string
	owner       string
	delegateRAM bool
	exclusive   bool
	// AIRA-185. The holder's own free-text label for why the slice is held.
	// DIAGNOSTIC ONLY and already bounded by validateAdmitArgs.
	exclusiveReason string
	exclusiveHolder string
	parentScopeID   string

	// scopeCeiling is NOT a client field: admitConnection resolves it and puts it
	// here so the waiter can be constructed with it already set, under queue.mu.
	//
	// It used to be assigned onto the waiter AFTER enqueue, with no lock held,
	// while the evaluator goroutine was already free to read that waiter. It is
	// set where every other waiter field is written, under queue.mu, rather than
	// contorting a later lock-free assignment around a concurrent read.
	scopeCeiling int64

	// S8 (anchor). conn / clientPID / processStartTick / peerSameUID are resolved by
	// admitConnection from the connection BEFORE the enqueue lock and carried here so
	// enqueueAdmitInternal can anchor the lease (anchorLeaseLocked) atomically with the
	// idempotent SET. None is a wire field.
	//
	// peerSameUID is a BOOL, not a uid, and that is deliberate: its zero value (false)
	// fails the re-declare same-uid gate closed, so a build or test that never resolves
	// it cannot accidentally authorise a re-anchor — whereas a zero uid int equals
	// root's euid and would fail OPEN if anything ever ran as root. It is true only when
	// the SO_PEERCRED read succeeded AND the peer uid equals the daemon's euid (design
	// §4 gate P2-C: SO_PEERCRED same-uid only, NO cgroup-membership check — both confine
	// and aitest holders live outside their own scope).
	conn             net.Conn
	clientPID        int
	processStartTick uint64
	peerSameUID      bool

	// S9. reDeclare marks the request as an ARDR re-declare (serveReDeclare), NOT a
	// fresh admit. It changes exactly ONE thing in enqueueAdmitInternal: when the lease
	// is ABSENT it ESTABLISHES it granted (the crash-restart-no-dump case, design §4)
	// instead of inserting a fresh QUEUED waiter. The present-lease SET/re-anchor is
	// identical either way, so a plain dup-scope admit (reDeclare false) stays
	// absent→queued — a genuine fresh admission — which is what keeps establish-granted
	// scoped to the re-declare entrypoint alone. Set only by enqueueReDeclare; no wire
	// field.
	reDeclare bool
}

type admitRejection struct {
	Required int64  `json:"required,omitempty"`
	Ceiling  int64  `json:"cap_minus_headroom"`
	Basis    string `json:"basis"`
	// AIRA-101. Set when the rejection happened while this slice was draining for
	// or held by an exclusive job, so a blocked operator can tell "a benchmark has
	// the slice" from ordinary saturation. Additive: Basis keeps its exact
	// "reject:saturated" spelling, which validRunnerAdmitRejection pins.
	Exclusive string `json:"exclusive,omitempty"`

	// AIRA-149. DIAGNOSIS ONLY: neither field is consulted by any admission or
	// grant decision, and both are additive beside the pinned Basis spelling.
	//
	// Contention is the LATCHED three-valued reading of "was anything else ever
	// in the way", as one of "observed" / "none-observed" / "unevaluated". The
	// daemon always sets one of the three on this path, so an EMPTY value at the
	// client strictly means "not reported by this build" and lands on the
	// unchanged pre-AIRA-149 wording.
	//
	// Grantable is the checkedAvailable figure the capacity gate last computed
	// for THIS waiter. The pointer is load-bearing: `charge >= ceiling` yields a
	// genuine zero — "not one byte was grantable at the last evaluation" — and an
	// omitempty scalar would erase that real reading into "the daemon did not
	// report this", which is the exact conflation this change exists to remove.
	Contention string `json:"contention,omitempty"`
	Grantable  *int64 `json:"grantable_bytes,omitempty"`
}

func subtractFloor(value, subtract int64) int64 {
	if value <= 0 || subtract < 0 || subtract >= value {
		return 0
	}
	return value - subtract
}

func addClamp(a, b int64) int64 {
	if a < 0 || b < 0 || a > math.MaxInt64-b {
		return math.MaxInt64
	}
	return a + b
}

// pctClamp returns value*pct/100 without ever overflowing. It is a shared
// percentage helper (the oomsteer fullness band uses it).
//
// pct is capped at 100: a margin larger than the job's whole size is not a
// meaningful setting, and that cap is also what BOUNDS the overflow branch's
// error instead of leaving it merely non-negative. With pct <= 100,
// value/100*pct <= value, so the fallback cannot overflow either, and it
// under-reports the exact answer by a remainder strictly smaller than pct --
// tens of bytes on a gigabyte figure. Not exact, and the tests assert that
// bound rather than exactness. An earlier version returned MaxInt64/100*pct
// here, which is non-negative but roughly twelve times LARGER than the true
// answer: safe in direction, wrong as arithmetic, and invisible to a test that
// only checked the sign.
func pctClamp(value, pct int64) int64 {
	if value <= 0 || pct <= 0 {
		return 0
	}
	if pct > 100 {
		pct = 100
	}
	if value > math.MaxInt64/pct {
		return value / 100 * pct
	}
	return value * pct / 100
}

func addJobCountClamp(a, b int) int {
	maxInt := int(^uint(0) >> 1)
	if a < 0 || b < 0 || a > maxInt-b {
		return maxInt
	}
	return a + b
}

func (s *Server) admitSliceHeadroom(jobs int) int64 {
	if jobs < 0 {
		jobs = 0
	}
	base := s.admitSliceHeadroomBase
	perJob := s.admitSliceHeadroomSupervisor
	if base < 0 || perJob < 0 || jobs > 0 && perJob > (math.MaxInt64-base)/int64(jobs) {
		return math.MaxInt64
	}
	return base + int64(jobs)*perJob
}

func (s *Server) admitOutstandingJobs(path string) int {
	s.admitRegistryMu.Lock()
	queue := s.admitQueues[path]
	if queue == nil {
		s.admitRegistryMu.Unlock()
		return 0
	}
	queue.mu.Lock()
	jobs := queue.outstandingJobs
	queue.mu.Unlock()
	s.admitRegistryMu.Unlock()
	return jobs
}

// admitSliceSnapshot reads the ledger AND the queue diagnostics in ONE locked
// pass. Taking them in two rounds would let `confine --list` report a granted
// total and a queued count from different moments — a self-inconsistent picture
// in exactly the situation an operator reaches for it.
type admitSnapshot struct {
	outstanding     int64
	outstandingJobs int
	queued          int
	phase           string
	present         bool

	// S13 (design §4 / AIRA-220 honesty). restartFrozen is whether the restart
	// new-admission freeze is active at the snapshot instant, taken in the same locked,
	// single-clock walk as `present`, so the GrantedEstablished honesty bit can read
	// FALSE while the granted total is still settling (survivors may still re-declare)
	// without a second, possibly-inconsistent read. (S13 removed the unanchoredLeases
	// count with the dump layer: no granted lease is ever unanchored now.)
	restartFrozen bool

	// AIRA-24. One waiter's own place in the queue, answered only when a
	// caller named its own scope id. queuePosition is 1-based and counts ONLY
	// queued waiters, in enqueue-sequence (evaluation) order; queuedAheadBytes
	// is the sum of the reserves of the queued waiters ahead of it. Zero is
	// "no position established" — the scope id is not a queued waiter here
	// (granted, released, unknown, or never asked) — never "position zero".
	//
	// Derived in the same locked walk as `queued` so the pair cannot describe
	// two different instants, which is the whole reason admitSnapshot exists.
	queuePosition    int
	queuedAheadBytes int64
	// AIRA-186. That same waiter's OWN resolved reserve — `waiter.reserve`, the
	// figure admission is gating on, which is also the quantity queuedAheadBytes
	// sums for the waiters in front. Taken at the SAME match, so "how much is
	// ahead of me" and "how much am I asking for" cannot come from two different
	// instants.
	//
	// Zero is "not established", on the same discipline as the position.
	queuedReserveBytes int64

	// AIRA-68. outstandingJobs fuses TWO structurally different populations, while
	// `confine --list`'s table above the summary lists only SCOPES:
	//
	//   scopeJobs        connection-held `aira confine` jobs   -> a table row
	//   reservationJobs  connection-held `aira confine-reserve` reservations,
	//                    which create no cgroup scope at all   -> NO table row
	//
	// (A third population, S12-deleted: scan-adopted scopes. S11's reload +
	// re-declare re-seeds post-restart survivors as connection-held leases, so
	// they now count under scopeJobs.)
	//
	// So "N admitted jobs" is not comparable with the row count, and reading it
	// that way is precisely what produced AIRA-68's P0 misdiagnosis: 20 of 23
	// "admitted jobs" were healthy per-test reservations from a running
	// --delegate-ram pytest suite. The split is derived in the SAME locked pass as
	// the totals so the two can never describe different instants.
	scopeJobs        int
	scopeBytes       int64
	reservationJobs  int
	reservationBytes int64

	// AIRA-101. The slice's exclusive state, derived in the SAME locked walk as
	// everything above so `confine --list` and a blocked launcher's progress line
	// can never render an exclusive holder alongside counts from another instant.
	//
	// exclusiveState is "" when nothing is exclusive. That is a POSITIVE fact —
	// the walk established it — not an unevaluated reading, and consumers must
	// render it as "none" rather than as unknown.
	exclusiveState   string
	exclusiveName    string
	exclusiveOwner   string
	exclusiveScopeID string
	// AIRA-185. The holder's own free-text reason, taken from the SAME waiter the
	// identity above came from so the label and the job it qualifies can never
	// come from different instants. Empty is a positive "none supplied".
	exclusiveReason string
	// exclusiveWaiting counts the queued waiters actually held up behind the
	// exclusive job. It excludes the exclusive waiter itself.
	exclusiveWaiting int
	// AIRA-119. How long the reported state has been in effect, taken from the
	// SAME waiter the identity above came from and the SAME clock reading as every
	// other age in this snapshot. Zero is an unestablished age, never "0s" — see
	// runner.ConfineExclusiveState.SinceMS for why the age is load-bearing rather
	// than decorative.
	exclusiveSinceMS int64

	// AIRA-108. One row per GRANTED, accounted, scope-less waiter — the population
	// reservationJobs/reservationBytes above can only count. Gathered in the same
	// pass and under the same lock as everything else here, so an operator can
	// never be shown rows from one instant beside totals from another.
	//
	// Held as VALUES copied out under the lock, never as *admitWaiter pointers:
	// sorting, capping and rendering all happen after the lock is dropped, and a
	// pointer would let a released waiter's fields be read unsynchronised.
	reservations []admitReservationRow

	// AIRA-191/AIRA-192. scope id -> the reserve this ledger charges that scope
	// RIGHT NOW: the connection-held waiters' ledgerCharge, the same quantity
	// scopeBytes sums. (Before S12 a second source, scan-adopted scopes'
	// reconstructed reserve, also fed this map; S12 deleted adoption, so the map
	// now carries only connection-held scopes.) Gathered in the same locked pass
	// as every total above, so rows and totals always describe one instant, and
	// reconciling: over one snapshot the values sum to scopeBytes.
	//
	// It is deliberately NOT the scope's memory.max: a delegate scope's
	// memory.max is an AIRA-15 containment ceiling many times its declared
	// reserve, and publishing it is how `aira top` came to draw 93 GiB of claims
	// against a 40 GiB ledger. For a connection-held waiter this equals its
	// declared reserve.
	//
	// A scope ABSENT from the map is one this ledger charges nothing for and knows
	// nothing about; the wire renders that as unevaluated, never as a cap.
	//
	// Copied out as a fresh map, never a reference to queue state, for exactly the
	// reason reservations above are copied by value.
	scopeReserves map[string]int64
}

// boundedAdmitSignature bounds the DIAGNOSTIC copy of a client-supplied
// signature. See admitWaiter.signature for why this is required rather than
// cosmetic.
//
// The bound is on RUNES, and truncation is MARKED with an ellipsis, so a reader
// can always tell a real short signature from a clipped long one. Cutting on a
// rune boundary keeps the value valid UTF-8, which a byte slice would not: a
// JSON encoder turns a split rune into U+FFFD, so the wire would carry a
// corrupted string the operator could not match against a real test id.
//
// The ORIGINAL is untouched — admission reads request.signature — so nothing
// about which jobs are admitted, or at what reserve, changes here.
func boundedAdmitSignature(signature string) string {
	const limit = runner.ConfineReservationSignatureWireLimit
	count := 0
	for index := range signature {
		if count == limit {
			return signature[:index] + "…"
		}
		count++
	}
	return signature
}

// boundedAdmitReason bounds the DIAGNOSTIC copy of a client-supplied exclusive
// hold reason (AIRA-185), on exactly the terms boundedAdmitSignature bounds a
// signature and for the same availability reason: the admit protocol accepts a
// string of any length up to the 16 MiB frame, and an unbounded copy retained on
// a long-lived waiter would ride into every `confine --list` reply until the
// response exceeded MaxFrameBytes and the verb stopped working for every job on
// the slice.
//
// Surrounding whitespace is trimmed first, so a label that is only whitespace
// becomes the empty string — an ABSENT reason, which every renderer omits —
// rather than a blank clause claiming a purpose was given.
func boundedAdmitReason(reason string) string {
	trimmed := strings.TrimSpace(reason)
	const limit = runner.ConfineExclusiveReasonWireLimit
	count := 0
	for index := range trimmed {
		if count == limit {
			return trimmed[:index] + "…"
		}
		count++
	}
	return trimmed
}

// admitReservationRow is one scope-less reservation, copied out of a waiter
// under queue.mu.
type admitReservationRow struct {
	signature string
	reserve   int64
	heldMS    int64
}

// residualJobs and residualBytes cross-check the re-derived ledger cache
// (queue.outstanding / outstandingJobs, recomputed by rederiveLedgerLocked at
// the last grant or release) against an INDEPENDENT walk of queue.waiters taken
// in this snapshot (scopeJobs+reservationJobs, scopeBytes+reservationBytes).
// They are equal by construction: both count `admitGranted && accounted`
// waiters and sum the same ledgerCharge(), so a non-zero residual is a real
// defect — a grant or release that changed the waiter set without re-deriving,
// or a second hand-maintained mutation site added beside the one accessor — not
// noise.
//
// The two are reported INDEPENDENTLY and SIGNED. The single most plausible
// regression — a release path that removes a waiter but skips the re-derive, so
// outstanding keeps a discharged lease's bytes — is byte-only, and a job-only
// residual would report a perfectly consistent ledger while the slice silently
// filled. A negative residual (more discharged than was ever charged) is just
// as real a defect as a positive one and must never be floored away.
//
// What they do NOT detect: a stuck waiter that is consistently present in BOTH
// accountings — a lease held while its job has gone. S14 deleted the cgroup scan
// that used to surface that population as `vanished`; the socket-EOF release
// (design §3) and the physical-reap stale-lease backstop are the reclaim paths now.
func (snapshot admitSnapshot) residualJobs() int {
	return snapshot.outstandingJobs - (snapshot.scopeJobs + snapshot.reservationJobs)
}

func (snapshot admitSnapshot) residualBytes() int64 {
	return snapshot.outstanding - (snapshot.scopeBytes + snapshot.reservationBytes)
}

// admitSliceSnapshot is the aggregate read: no caller identity, so no
// per-waiter position is computed. Every existing caller wants exactly this.
func (s *Server) admitSliceSnapshot(path string) admitSnapshot {
	return s.admitSliceSnapshotFor(path, "")
}

// admitSliceSnapshotFor additionally locates ONE queued waiter by scope id
// (AIRA-24). queuedScopeID is only ever compared for equality against the
// scope ids the daemon itself minted into its waiter list, so an unknown or
// malformed value simply matches nothing and leaves the position unestablished.
func (s *Server) admitSliceSnapshotFor(path, queuedScopeID string) admitSnapshot {
	// With the duty cycle off a freeze may be actively blocking this queue, so
	// reporting "idle" would state the opposite of the truth.
	phase := admitFreezeIdle.String()
	if s.admitFreezeMaxHold <= 0 {
		phase = "disabled"
	}
	s.admitRegistryMu.Lock()
	queue := s.admitQueues[path]
	if queue == nil {
		s.admitRegistryMu.Unlock()
		// An absent queue positively establishes that nothing is WAITING and that
		// there is no exclusive holder — the diagnostics half (queued/phase) is a
		// genuine idle zero and callers render it as such. But it says NOTHING
		// about the granted LEDGER: pruneAdmitQueue deletes the queue once nothing
		// is connection-held, so an absent queue is exactly "the ledger was never
		// built or has been pruned". present stays false here precisely so a caller
		// reports the granted pair unevaluated rather than as a fabricated empty
		// slice (AIRA-220). CeilingBytes is an independent memory read and is
		// unaffected.
		return admitSnapshot{phase: phase}
	}
	queue.mu.Lock()
	snapshot := admitSnapshot{
		outstanding: queue.outstanding, outstandingJobs: queue.outstandingJobs,
		phase: phase, present: true,
		scopeReserves: make(map[string]int64, len(queue.waiters)),
	}
	queuedBytes := int64(0)
	// ONE reading of the clock for the whole walk (AIRA-108): ages taken per-row
	// would drift across a long waiter list, so two rows could report an ordering
	// the queue never had.
	now := s.admitNowTime()
	// S11. The freeze state at this same instant — part of the GrantedEstablished bit.
	snapshot.restartFrozen = s.restartFrozenAt(now)
	for _, waiter := range queue.waiters {
		if waiter == nil {
			continue
		}
		if waiter.state == admitQueued {
			snapshot.queued++
			// AIRA-24. The position is an index among QUEUED waiters only, so it
			// is taken here and nowhere else: counting granted or released
			// waiters would report a place in a line that no longer exists. The
			// first match wins — enqueueAdmitInternal refuses a second queued (or
			// rejected) waiter for a scope id (CodeProtocol) and re-anchors a granted
			// one in place rather than adding a waiter, so a second match is not
			// reachable.
			if queuedScopeID != "" && snapshot.queuePosition == 0 && waiter.scopeID == queuedScopeID {
				snapshot.queuePosition = snapshot.queued
				snapshot.queuedAheadBytes = queuedBytes
				// AIRA-186. Taken BEFORE queuedBytes absorbs this waiter's own
				// reserve, from the matched waiter and not from the running sum:
				// this is what THIS job is asking for, never what is ahead of it.
				snapshot.queuedReserveBytes = waiter.reserve
			}
			queuedBytes = addClamp(queuedBytes, waiter.reserve)
			continue
		}
		// The classifier is scopeID and nothing else. name/owner cannot be used:
		// validateAdmitArgs requires the scope_id/name/owner tuple to be supplied
		// together, so they co-occur and would make the split look right while
		// classifying on the wrong fact.
		if waiter.state != admitGranted || !waiter.accounted {
			continue
		}
		// These three sum ledgerCharge(), the same quantity the ledger itself
		// carries. They must move with queue.outstanding or residualBytes() -- a
		// real lost/double-decrement detector surfaced by `confine --list` --
		// would report a fabricated ledger defect.
		if waiter.scopeID == "" {
			snapshot.reservationJobs++
			// Goes through ledgerCharge() rather than reading waiter.reserve
			// directly, so this stays equal to what queue.outstanding carries and
			// residualBytes() keeps meaning what it says -- every ledger site uses
			// the one accessor, keeping that a property of the code rather than a
			// coincidence to be rediscovered.
			snapshot.reservationBytes = addClamp(snapshot.reservationBytes, waiter.ledgerCharge())
			// AIRA-108. Name it, in the same pass. heldMS is derived from grantedAt
			// — the daemon's own record of the exact grant moment — and NOT from
			// `enqueued`, which would conflate ordinary admission-queue contention
			// with the hold this row exists to describe. A grantedAt that is somehow
			// unset or in the future reports 0 rather than a negative or fabricated
			// duration: an unestablished age, never a wrong one.
			held := int64(0)
			if !waiter.grantedAt.IsZero() {
				if elapsed := now.Sub(waiter.grantedAt); elapsed > 0 {
					held = elapsed.Milliseconds()
				}
			}
			snapshot.reservations = append(snapshot.reservations, admitReservationRow{
				signature: waiter.signature, reserve: waiter.reserve, heldMS: held,
			})
			continue
		}
		snapshot.scopeJobs++
		snapshot.scopeBytes = addClamp(snapshot.scopeBytes, waiter.ledgerCharge())
		// AIRA-191/AIRA-192. The same charge, NAMED, from the same accessor and
		// under the same `admitGranted && accounted` guard the sum above uses — so
		// a waiter that contributes to scopeBytes contributes a row and one that
		// does not contributes neither, and the two can never drift apart.
		snapshot.scopeReserves[waiter.scopeID] = waiter.ledgerCharge()
	}
	if s.admitFreezeMaxHold > 0 {
		snapshot.phase = admitFreezePhaseAt(queue.freezeArmedAt, s.admitNowTime(), s.admitFreezeMaxHold).String()
	}
	// AIRA-101, in the same locked pass as the counts above.
	if gate := exclusiveGateLocked(queue); gate.holder != nil || gate.draining != nil {
		exclusive := gate.holder
		snapshot.exclusiveState = admitExclusiveHeld
		if exclusive == nil {
			exclusive, snapshot.exclusiveState = gate.draining, admitExclusiveDraining
		}
		snapshot.exclusiveName = exclusive.name
		snapshot.exclusiveOwner = exclusive.owner
		snapshot.exclusiveScopeID = exclusive.scopeID
		// AIRA-185, from the same waiter as the identity above.
		snapshot.exclusiveReason = exclusive.exclusiveReason
		// Only waiters actually held up behind it: the exclusive waiter is never
		// counted as waiting for itself.
		snapshot.exclusiveWaiting = subtractJobCount(snapshot.queued, 1)
		if exclusive.state != admitQueued {
			snapshot.exclusiveWaiting = snapshot.queued
		}
		// AIRA-119. The age of THIS state, from THIS waiter, against the one clock
		// reading `now` already took for the whole walk.
		//
		// The anchor is per-state and that is load-bearing, not tidiness. A HELD
		// job's age must run from its grant — the daemon's own record of the moment
		// it was let in — because a benchmark that queued twenty minutes and has now
		// been running alone for five seconds has held the slice for five seconds,
		// and reporting twenty minutes would libel it as the wedge. A DRAINING job
		// has no grant yet, so its age necessarily runs from its enqueue, which is
		// exactly the quantity an operator wants: how long this drain has been
		// failing to converge. This is the AIRA-49 v3 conflation (enqueued read as
		// grantedAt) in the one place it would say the opposite of the truth.
		//
		// elapsedMilliseconds floors a backwards clock at zero, and a zero anchor is
		// left at zero, so an age is either established or absent — never negative
		// and never fabricated.
		anchor := exclusive.grantedAt
		if snapshot.exclusiveState == admitExclusiveDraining {
			anchor = exclusive.enqueued
		}
		if !anchor.IsZero() {
			snapshot.exclusiveSinceMS = elapsedMilliseconds(anchor, now)
		}
	}
	queue.mu.Unlock()
	s.admitRegistryMu.Unlock()
	return snapshot
}

// admitQueueDiagnostics is the diagnostics half of admitSliceSnapshot.
func (s *Server) admitQueueDiagnostics(path string) (queued int, phase string) {
	snapshot := s.admitSliceSnapshot(path)
	return snapshot.queued, snapshot.phase
}

func (s *Server) admitCeiling(path string, maximum int64) int64 {
	return subtractFloor(maximum, s.admitSliceHeadroom(s.admitOutstandingJobs(path)+1))
}

func (s *Server) resolveAdmitReserve(request admitRequest, ceiling int64) (int64, string) {
	if request.pinned {
		return request.reserve, "pinned:client"
	}
	// AIRA-153. ONE quantity governs every auto-sized value this function can
	// place against `ceiling`: the largest reserve the slice can actually GRANT
	// one job. `ceiling` is only the largest ADMISSIBLE one — the grant gate is
	// strictly tighter (AIRA-150) — so anything AIRA sizes for ITSELF is bounded
	// by `fit`, never by `ceiling`.
	//
	// Three sites, below: the client's unpinned prior, the machine-wide p90
	// prior, and AIRA-151's OOM-escalation clamp. `fit` is 0 when the slice is
	// too small for any viable reserve, and every site then leaves the value
	// alone so the existing terminal E_ADMIT_TOO_LARGE answers, naming both
	// numbers.
	fit := runner.SliceFittedReserve(ceiling)

	// SITE 1 — the client's unpinned prior. runner.ResolveConfineReserve hands
	// the daemon a compiled-in constant with no relationship to this command or
	// to this slice, and that function is pure and portable precisely so the
	// reserve decision has ONE home (AIRA-62); the daemon is the only party that
	// knows the ceiling, so the bounding happens here.
	//
	// GATED on `>= ceiling`: only a prior that is REFUSED today (`>`), or that
	// lands exactly on the ungrantable ceiling (`==`, AIRA-150 route 3), is
	// touched. Where the default already fits and is already granted, this
	// function returns byte-for-byte what it returns today — lowering a working
	// job's kernel-enforced memory.max by 13% would be silent under-provisioning.
	//
	// Applied BEFORE anything reads `request.reserve`, which is load-bearing in
	// two directions: all six sites that return the hint verbatim inherit it, and
	// the OOM escalation's `escalated > reserve` comparison sees the fitted value
	// as its FLOOR, so the escalation can still raise the reserve to whatever
	// this command's own OOM evidence justifies. Fitting AFTER the resolution
	// instead would let a blind prior beat an escalation the job's own kill
	// earned, sizing the job BELOW what its own evidence says it needs.
	//
	// `request` is a value copy (see the signature), so this cannot escape.
	fitted := ""
	if fit > 0 && request.reserve >= ceiling {
		request.reserve, fitted = fit, ",ceiling-fitted"
	}
	readCtx, cancel := context.WithTimeout(context.Background(), admitHistoryTimeout)
	defer cancel()
	historyUnavailable := false
	insufficientSamples := false
	if request.signature != "" {
		read := s.admitPeakHistory
		if read == nil && s.db != nil {
			read = s.db.ConfinePeakHistory
		}
		if read != nil {
			stats, err := read(readCtx, request.signature)
			if err != nil {
				// The read itself could not be established (timeout / DB error).
				// Distinguish this from genuine absence of history so the basis is
				// honest; the reserve value still falls back safely below.
				historyUnavailable = true
			} else {
				reserve := request.reserve
				ordinary := stats
				ordinary.OOMCount = 0
				// AIRA-149 (D4). The estimator's OWN basis is kept in BOTH arms. This
				// local used to start at a hardcoded "fallback:insufficient-samples"
				// that survived whenever ok == false, which is a false label the moment
				// the real reason was something else (fallback:malformed, reachable
				// through the injected history seam). The VALUE path is unchanged:
				// reserve still moves only when the estimate is usable.
				estimated, estimateUsable, basis := runner.EstimateMemoryReserve(ordinary, 0)
				// AIRA-153. The fit token travels by PROVENANCE, never by comparing
				// numbers: `suffix` carries it only while `reserve` still holds the
				// fitted PRIOR, and is cleared the moment this command's own measured
				// evidence takes over. An ORDINARY ESTIMATE is never fitted — over the
				// ceiling it is refused terminally, naming both numbers, because a
				// measurement may not be silently reduced to fit an established fact
				// the way a guess may. (AIRA-149's rule: a label must name the term
				// that acted, and two terms can coincide on a value.)
				suffix := fitted
				if estimateUsable {
					reserve, suffix = estimated, ""
				}
				if stats.TotalCount > 0 {
					// Some observations exist but did not yield a usable estimate
					// (fewer than three usable samples): not genuine "no history".
					insufficientSamples = true
				}
				if stats.OOMCount > 0 && stats.MaxOOMPeak > 0 {
					escalated := stats.MaxOOMPeak
					if escalated > math.MaxInt64-escalated/2 {
						escalated = math.MaxInt64
					} else {
						escalated += escalated / 2
					}
					// AIRA-149 (D1). reserve-basis names the provenance of the number
					// actually RETURNED, not the branch that was entered.
					//
					// "estimate:oom-escalated" used to be returned unconditionally from
					// here, which is true of exactly ONE of this branch's five outcomes.
					// In the commonest state right after a first OOM -- one sample, so no
					// usable ordinary estimate, and a 1.5x escalation far below the
					// unpinned 4 GiB client default -- the value returned is the client's
					// own default (or the ceiling it was clamped to), and nothing derived
					// from the OOM peak appears in it.
					//
					// ",oom-on-record" is NOT decoration. "estimate:oom-escalated" carried
					// two meanings welded together: ATTRIBUTION ("an OOM record for THIS
					// signature was found and consulted") and PROVENANCE ("the number is
					// 1.5x the OOM peak"). Only the provenance half was ever false, and the
					// attribution half is load-bearing: AIRA-128's real-cgroup fixture uses
					// it as the proof that a real kernel OOM travelled memory.events ->
					// confine teardown -> RecordConfinePeak -> ConfinePeakHistory -> here.
					// Reporting a bare fallback basis would have deleted a verified
					// property while fixing a false one.
					//
					// The tie-break is stated rather than accidental: the escalation is
					// deemed to have determined the value only when it STRICTLY raised it,
					// which is the existing condition, unchanged. On an exact tie both
					// terms produce the same number and the source basis is reported.
					//
					// The grammar is the existing one: family:name[:params] with
					// COMMA-separated params, as estimate:max=%d,n=%d,f=115 already uses.
					// No space, which the trailer's key=value field forbids.
					oomBasis := basis + ",oom-on-record"
					if escalated > reserve {
						reserve = escalated
						oomBasis = "estimate:oom-escalated"
						// AIRA-151. The clamp lives INSIDE this branch, which is
						// the whole change.
						//
						// An OOM observed at the present ceiling is genuinely too
						// large. Earlier censored caps are allowed to climb to the
						// ceiling so a runnable job is never permanently wedged.
						//
						// That justification is about a value DERIVED FROM THE OOM
						// PEAK. It does not apply to the blind unpinned client
						// default, nor to an ordinary peak-history estimate: for
						// those the no-OOM path already refuses an over-ceiling
						// value terminally with E_ADMIT_TOO_LARGE (the reserve >
						// ceiling boundary in admitConnection), naming both
						// numbers, and an OOM record must not make the same number
						// behave differently. A clamped reserve is exactly the
						// ENTRY ceiling, and such a reserve is grantable only while
						// the slice's charge stays inside a band of one per-job
						// headroom term per job the request entered behind --
						// byte-exact zero when it entered an empty slice
						// (AIRA-150) -- so what the clamp bought those rows was
						// usually a wait that ends in a refusal anyway, not a run.
						//
						// Nesting rather than an escalationDetermined flag is
						// deliberate: it makes "clamped without the escalation
						// having set the value" unrepresentable rather than merely
						// untrue, and keeps ONE condition governing both the basis
						// and the value, so the label and the number can never
						// disagree about which term acted. The comparison stays
						// STRICT: on an exact tie the escalation raised nothing.
						//
						// SITE 3 — AIRA-153 retargets both halves of this clamp
						// from `ceiling` to `fit`, and changes nothing else about
						// it. The rule, the nesting, the strict tie-break and the
						// STATIC ceiling (AIRA-103) are AIRA-151's, untouched; the
						// condition to ENTER is still `reserve > ceiling`, so an
						// escalation that lands in (fit, ceiling] is left exactly
						// as it is — it is this command's own evidence and it is
						// admissible.
						//
						// The TARGET moves because AIRA-151 kept this clamp so
						// "earlier censored caps are allowed to climb ... so a
						// runnable job is never permanently wedged", and a value
						// equal to the entry ceiling is not one such a job can be
						// GRANTED. Every clamped value is now simultaneously
						// strictly above the OOM peak that produced it (the guard)
						// and strictly below the ceiling by ~13% (the quantity), so
						// the rung is a real one.
						//
						// The GUARD moves because a job OOM-killed AT a fitted cap
						// records MaxOOMPeak ~= fit, and FIT(c)/c = 0.8696 lies
						// inside the clamp band (2/3, 1) BY CONSTRUCTION. Left at
						// `ceiling` it would clamp that job onto the ceiling every
						// time, where it is ungrantable, so it waits out the default
						// 30-minute window, is refused E_ADMIT_SATURATED ("owed a
						// RETRY, nothing about the request is wrong"), never runs,
						// and therefore records no new peak — a permanent wedge, not
						// a rung. Its meaning is unchanged in words: an OOM already
						// observed at or above what this slice can give is genuinely
						// too large, so do not pretend otherwise.
						if fit > 0 && stats.MaxOOMPeak < fit && reserve > ceiling {
							reserve = fit
							oomBasis += ",ceiling-clamped"
						}
						return reserve, oomBasis
					}
					return reserve, oomBasis + suffix
				}
				if stats.SampleCount >= 3 && reserve > 0 {
					return reserve, basis + suffix
				}
			}
		}
	}
	if peak, ok := s.cachedAdmitPeakP90(readCtx); ok {
		stats := runner.PeakRSSStats{TotalCount: 3, SampleCount: 3, PeakMax: peak}
		if reserve, usable, _ := runner.EstimateMemoryReserve(stats, 0); usable {
			// SITE 2 — AIRA-153. The p90 is a PRIOR about commands OTHER than this
			// one — it is consulted precisely because this signature has no history
			// — so it is bounded exactly as the client's default is, and only where
			// it would otherwise be refused or land on the ceiling.
			//
			// Without this, AIRA-128's cold start (which the agent guide teaches as
			// `estimate:p90-prior`) is still terminally refused on any slice whose
			// ceiling is below the box's p90 — the ordinary shape of a small ci-shim
			// budget beside a large aira.slice, since one machine-wide state.db
			// serves both.
			if fit > 0 && reserve >= ceiling {
				return fit, "estimate:p90-prior,ceiling-fitted"
			}
			return reserve, "estimate:p90-prior"
		}
	}
	// The four post-block fallbacks each return the client's own hint verbatim,
	// so each inherits SITE 1's fit and must name it (AIRA-153 §3.5).
	if request.signature == "" {
		return request.reserve, "fallback:no-signature" + fitted
	}
	if historyUnavailable {
		return request.reserve, "fallback:history-unavailable" + fitted
	}
	if insufficientSamples {
		return request.reserve, "fallback:insufficient-samples" + fitted
	}
	return request.reserve, "fallback:no-history" + fitted
}

// resolveDelegateRAMScopeCeiling is intentionally separate from reserve
// resolution: delegate-ram reserves are pinned framework overhead, while this
// value is a whole-scope containment backstop. In particular, pinned:client
// must still consult the scope's own peak history.
func (s *Server) resolveDelegateRAMScopeCeiling(request admitRequest, maximum, headroom int64) int64 {
	upper := subtractFloor(maximum, headroom)
	if upper <= 0 {
		return 0
	}
	minimum := delegateRAMScopeMinimum()
	if minimum > upper {
		minimum = upper
	}
	candidate := delegateRAMScopeDefault()
	if request.signature != "" {
		readCtx, cancel := context.WithTimeout(context.Background(), admitHistoryTimeout)
		defer cancel()
		read := s.admitPeakHistory
		if read == nil && s.db != nil {
			read = s.db.ConfinePeakHistory
		}
		if read != nil {
			if stats, err := read(readCtx, request.signature); err == nil && stats.PeakMax > 0 {
				candidate = delegateRAMScopeWithSafety(stats.PeakMax)
				if stats.OOMCount > 0 && stats.MaxOOMPeak > 0 {
					candidate = delegateRAMScopeOOMEscalation(stats.MaxOOMPeak)
				}
			}
		}
	}
	if candidate < minimum {
		return minimum
	}
	if candidate > upper {
		return upper
	}
	return candidate
}

func delegateRAMScopeMinimum() int64 {
	if parsed, err := runner.ParseMemorySize(strings.TrimSpace(os.Getenv("AIRA_DELEGATE_RAM_SCOPE_MIN"))); err == nil && parsed > 0 {
		return parsed
	}
	return delegateRAMScopeMinDefault
}

func delegateRAMScopeDefault() int64 {
	if parsed, err := runner.ParseMemorySize(strings.TrimSpace(os.Getenv("AIRA_DELEGATE_RAM_SCOPE_DEFAULT"))); err == nil && parsed > 0 {
		return parsed
	}
	return runner.DefaultDelegateRAMScopeCeiling
}

func delegateRAMScopeWithSafety(peak int64) int64 {
	if peak <= 0 || peak > math.MaxInt64/delegateRAMScopeSafetyPct {
		return math.MaxInt64
	}
	return addClamp(peak, peak*delegateRAMScopeSafetyPct/100)
}

func delegateRAMScopeOOMEscalation(peak int64) int64 {
	if peak <= 0 || peak > math.MaxInt64-peak/2 {
		return math.MaxInt64
	}
	return peak + peak/2
}

func (s *Server) cachedAdmitPeakP90(ctx context.Context) (int64, bool) {
	now := s.admitNowTime()
	s.admitPriorMu.Lock()
	if !s.admitPriorAt.IsZero() && now.Sub(s.admitPriorAt) < admitPriorRefresh {
		peak, ok := s.admitPriorPeak, s.admitPriorOK
		s.admitPriorMu.Unlock()
		return peak, ok
	}
	s.admitPriorMu.Unlock()
	read := s.admitPeakP90
	if read == nil && s.db != nil {
		read = s.db.ConfinePeakP90
	}
	if read == nil {
		return 0, false
	}
	peak, ok, err := read(ctx)
	if err != nil {
		return 0, false
	}
	s.admitPriorMu.Lock()
	s.admitPriorPeak, s.admitPriorOK, s.admitPriorAt = peak, ok, now
	s.admitPriorMu.Unlock()
	return peak, ok
}

// acquireAdmitSlot takes one of the admitGlobalMax concurrency slots, or
// reports false when they are all held. Shared by admitConnection and (since
// AIRA-63) workerAdmitConnection, which previously had no bound at all —
// factored out rather than duplicated so the two paths can never drift on
// which of them is bounded. Each caller renders saturation in ITS OWN client's
// vocabulary: admit answers CodeBusy, worker-admit answers a retriable
// "denied" (see workerAdmitConnection for why an error frame is unsafe there).
func (s *Server) acquireAdmitSlot() bool {
	if s.admitSlots == nil {
		s.admitRegistryMu.Lock()
		if s.admitSlots == nil {
			s.admitSlots = make(chan struct{}, admitGlobalMax)
		}
		s.admitRegistryMu.Unlock()
	}
	select {
	case s.admitSlots <- struct{}{}:
		return true
	default:
		return false
	}
}

func (s *Server) releaseAdmitSlot() { <-s.admitSlots }

func (s *Server) admitConnection(conn net.Conn, args map[string]any) {
	if !s.acquireAdmitSlot() {
		s.writeAdmitError(conn, CodeBusy, CodeBusy+": too many concurrent admission requests")
		return
	}
	defer s.releaseAdmitSlot()

	request, err := validateAdmitArgs(args, admitWaitCeilingMs)
	if err != nil {
		// The code is carried BY the error, not hardcoded here: a wait-ceiling
		// refusal must reach the client as CodeAdmitWaitTooLong, which the runner
		// treats as terminal. Sending it as CodeProtocol would make the runner fall
		// through to the flock fallback and launch the job outside the ledger.
		s.writeAdmitError(conn, admitErrorCode(err), err.Error())
		return
	}
	// S5 fail-fast (design §7 "RequestInvalid"): a request for more cores than this
	// box can EVER provide (cpu > 2×NumCPU) is permanently impossible — retrying
	// never helps — so refuse it up front, before any enqueue, rather than queue a
	// waiter that can never fit and would sit until its max_wait. Placed here rather
	// than in validateAdmitArgs because the ceiling is machine-specific (2×NumCPU via
	// the cpuCoreCounter seam), which the pure validator does not have — the same
	// split reserve uses (RANGE in the validator, CEILING here).
	//
	// Reported as a CLIENT-TERMINAL E_ADMIT_TOO_LARGE with a structured rejection
	// payload, NOT CodeProtocol. CodeProtocol routes the runner's fail() straight to
	// the flock fallback (admission_linux.go), launching the job OUTSIDE the ledger —
	// the AIRA-222 fail-open class this whole rebuild closes. E_ADMIT_TOO_LARGE is in
	// the runner's terminal pre-payload set; the {Required, Ceiling, Basis} payload
	// is what validRunnerAdmitRejection requires so the client refuses instead of
	// degrading. (The figures are cores, rendered by the generic too-large message —
	// cosmetically byte-flavoured, but this is a hand-crafted-request-only guard:
	// real clients send DefaultConfineCPUCores.)
	if request.cpu > s.cpuCeiling() {
		s.writeAdmitRejection(conn, CodeAdmitTooLarge, admitRejection{
			Required: request.cpu, Ceiling: s.cpuCeiling(), Basis: "reject:cpu-too-large",
		})
		return
	}
	// AIRA-121 gate condition C6. --exclusive is refused HERE, before the request
	// is ever queued, and that placement is the whole mechanism.
	//
	// In shim mode the ledger holds no lease for an unconfined job, so
	// sliceProvablyEmpty (Σleases == 0) would read the slice as empty and grant
	// exclusivity on the strength of an emptiness that says nothing about what else
	// is running in this container — there are no cgroup scopes here at all.
	// Refusing --exclusive up front is the whole mechanism: a benchmark demanding
	// solitude must run on a real-slice install where the ledger actually accounts
	// for its neighbours.
	//
	// CodeAdmitExclusiveUnestablished is reused rather than a new code minted: its
	// established meaning -- "an empty slice could not be established" -- is
	// exactly, literally true here.
	if s.shimMode() && request.exclusive {
		s.writeAdmitError(conn, CodeAdmitExclusiveUnestablished,
			CodeAdmitExclusiveUnestablished+": ci-shim mode has no cgroup scopes, so an empty slice cannot be established and exclusivity cannot be granted; run the benchmark on a real-slice install")
		return
	}
	resolve := s.sliceResolver()
	path, ok, reason := resolve(request.slice)
	if !ok {
		// S4 (D4 / Invariant 6): an unresolvable slice is "no readable budget" too —
		// fail CLOSED, symmetric with the unreadable-memory path below, rather than
		// the pre-S4 grant-shaped `unevaluated`.
		s.writeAdmitFailClosed(conn, request.exclusive, reason)
		return
	}
	readMemory := s.memoryReader()
	_, maximum, _, ok, reason := readMemory(path)
	if !ok {
		// S4 (D4 / Invariant 6): FAIL CLOSED. With no readable slice/container
		// budget the ceiling cannot be established, so a NEW admission must be
		// REFUSED, never granted — matching evaluateAdmitQueue's own !ok branch,
		// which leaves waiters queued and grants nothing. The pre-S4 code returned a
		// grant-shaped `unevaluated` here, which the runner treated as a real grant
		// and LAUNCHED THE JOB UNCAPPED (admission_linux.go: "an ordinary job
		// proceeds on it uncapped-but-launched").
		//
		// INTERIM GAP (recorded, not fixed here — the S13 client-flock-fallback
		// delete owns it): an ORDINARY refusal without --require-admission still
		// routes through the runner's fail() to the flock fallback and launches
		// ungoverned until S13; --require-admission already fails closed (it refuses
		// any non-admitted state). Post-S13 fail() becomes reconnect + re-request.
		s.writeAdmitFailClosed(conn, request.exclusive, reason)
		return
	}
	jobs := s.admitOutstandingJobs(path)
	headroom := s.admitSliceHeadroom(jobs + 1)
	ceiling := subtractFloor(maximum, headroom)
	if request.delegateRAM {
		// Resolved BEFORE the enqueue so the waiter is constructed with it, under
		// queue.mu. See admitRequest.scopeCeiling.
		request.scopeCeiling = s.resolveDelegateRAMScopeCeiling(request, maximum, headroom)
	}
	reserve, basis := s.resolveAdmitReserve(request, ceiling)
	if reserve > ceiling {
		s.writeAdmitRejection(conn, CodeAdmitTooLarge, admitRejection{Required: reserve, Ceiling: ceiling, Basis: basis})
		return
	}
	// S8 anchor identity: read the peer credential BEFORE the enqueue lock (mirrors the
	// supervisor-lease handler — no getsockopt/proc read under queue.mu) and carry it on
	// the request so enqueueAdmitInternal can anchor the lease atomically with the SET.
	// A fresh admit does NOT gate on uid (behaviour preserved; net.Pipe test connections
	// with no injected credential seam simply anchor with pid 0). peerSameUID is set true
	// only for a same-uid peer and is the gate the idempotent re-declare SET checks.
	request.conn = conn
	if uid, pid, credErr := s.peerCredentialOf(conn); credErr == nil {
		request.peerSameUID = uid == os.Geteuid()
		if pid > 0 {
			request.clientPID = pid
			if tick, ok, _ := readProcStartTime(pid); ok {
				request.processStartTick = tick
			}
		}
	}
	queue, waiter, code, enqueueErr := s.enqueueResolvedConfineAdmit(path, reserve, basis, maximum, request)
	if enqueueErr != nil {
		if code == CodeAdmitTooLarge {
			s.writeAdmitRejection(conn, code, admitRejection{Required: reserve, Ceiling: s.admitCeiling(path, maximum), Basis: basis})
		} else {
			s.writeAdmitError(conn, code, enqueueErr.Error())
		}
		return
	}
	peerCtx, cancelPeer := watchPeerEOF(conn)
	defer cancelPeer()

	// alreadyGranted is true when the enqueue re-anchored an existing granted lease
	// (the idempotent SET): its grantedCh is already closed, so there is no wait and
	// the deadline/grantedCh select below is skipped rather than resolved by the
	// runtime's random ready-case choice. This read is in a critical section separate
	// from the enqueue, which is benign: if the evaluator granted in the gap, grantedCh
	// is already closed and the select returns immediately. The RELEASE below captures
	// NO such value — it compares this handler's own conn against the live anchor, so
	// the reconnect race has no capture window (unlike a per-connection generation).
	queue.mu.Lock()
	alreadyGranted := waiter.state == admitGranted
	queue.mu.Unlock()

	released := false
	release := func() {
		if released {
			return
		}
		released = true
		s.releaseAdmitWaiterAnchored(queue, waiter, conn)
	}
	defer release()

	if !alreadyGranted {
		// S13: a BLOCKING wait has NO deadline (design §4/§6: no timeout — a wait ends
		// only on a grant, daemon stop, or the client closing its connection). A nil
		// deadline channel never fires, so a blocked request never self-expires. Only a
		// NON-BLOCKING request (max_wait_ms==0) installs a ZERO deadline, returning the
		// current snapshot at once via timeoutAdmitWaiter. The per-waiter admitAfter
		// seam is retained so the non-blocking return stays test-drivable.
		var timer *time.Timer
		var deadline <-chan time.Time
		if request.nonBlocking {
			if s.admitAfter != nil {
				deadline = s.admitAfter(0)
			} else {
				timer = time.NewTimer(0)
				deadline = timer.C
			}
		}
		defer stopTimer(timer)
		select {
		case <-waiter.grantedCh:
		case <-deadline:
			s.timeoutAdmitWaiter(queue, waiter)
		case <-s.stopping:
			return
		case <-peerCtx.Done():
			return
		}
	}

	queue.mu.Lock()
	if waiter.state == admitRejected {
		// The EXCLUSIVE requester's own expiry. Its state is already admitRejected
		// by the time the gate is re-derived, so it no longer matches the drain
		// predicate and would otherwise be reported as plain saturation — "the slice
		// was contended" — when what actually happened is "my drain did not complete
		// in the budget I set". Naming it keeps the two apart for the one caller who
		// most needs the difference (found by build review).
		if waiter.exclusive {
			diagnosis := saturatedDiagnosisLocked(waiter, reserve, ceiling)
			diagnosis.Exclusive = admitExclusiveDraining
			queue.mu.Unlock()
			s.writeAdmitRejection(conn, CodeAdmitSaturated, diagnosis)
			return
		}
		// Report WHETHER the wait expired under an exclusive drain or hold, so a
		// blocked operator can tell "a benchmark has the slice" from ordinary
		// saturation. Basis keeps its exact "reject:saturated" spelling, which
		// validRunnerAdmitRejection pins, so this is purely additive.
		exclusiveState := exclusiveGateStateLocked(queue)
		diagnosis := saturatedDiagnosisLocked(waiter, reserve, ceiling)
		diagnosis.Exclusive = exclusiveState
		queue.mu.Unlock()
		s.writeAdmitRejection(conn, CodeAdmitSaturated, diagnosis)
		return
	}
	if waiter.state != admitGranted {
		queue.mu.Unlock()
		return
	}
	grant := AdmitResponse{State: waiter.outcome, Reason: waiter.reason, WaitedMS: waiter.waitedMS, Reserve: waiter.reserve, Cpu: waiter.cpu, Basis: waiter.basis, ScopeCeiling: waiter.scopeCeiling}
	queue.mu.Unlock()

	if s.admitBeforeWrite != nil {
		s.admitBeforeWrite(waiter)
	}
	_ = conn.SetWriteDeadline(time.Now().Add(admitWriteTimeout))
	write := s.admitWriteFrame
	if write == nil {
		write = func(conn net.Conn, value any) error { return writeFrame(conn, value) }
	}
	if err := write(conn, responseFrame(core.Response{OK: true, Code: "OK", Data: grant})); err != nil {
		return
	}

	// A successfully delivered grant remains reserved until the client closes
	// its lease (confine does so after scope teardown), or shutdown cancels it.
	select {
	case <-peerCtx.Done():
	case <-s.stopping:
	}
}

// leaseByScopeIDLocked returns the one live waiter currently anchoring scopeID
// on this queue, or nil. queue.mu must be held.
//
// It is the lookup half of the idempotent SET-by-scope-id the signed ledger is
// keyed on (design §2): because enqueueAdmitInternal refuses a second queued/rejected
// waiter for a scope id and re-anchors a granted one in place, at most one
// non-released waiter can carry any scope id, so this is a point read of that scope's
// lease. enqueueAdmitInternal uses it both to REFUSE a queued/rejected duplicate and
// to find the granted lease its re-anchoring SET updates. An empty scopeID keys
// nothing (scope-less `confine-reserve` waiters) and matches no lease.
//
// CAUTION: this lookup matches on `!= admitReleased` (any live-or-dying waiter is a
// duplicate to refuse), but enqueueAdmitInternal's re-anchoring SET narrows to
// state == admitGranted. A waiter that has been REJECTED (timed out, or aborted by the
// unestablished-emptiness rule) but whose deferred release has not yet run is still
// `!= admitReleased`; re-anchoring onto it would SET a dead lease that is about to be
// discharged. The broad predicate is correct for the refusal and wrong for the SET.
func leaseByScopeIDLocked(queue *sliceQueue, scopeID string) *admitWaiter {
	if scopeID == "" {
		return nil
	}
	for _, existing := range queue.waiters {
		if existing != nil && existing.state != admitReleased && existing.scopeID == scopeID {
			return existing
		}
	}
	return nil
}

// anchorLeaseLocked (re-)anchors w to conn (design §3, Inv 4). It is the ONE place a
// lease is anchored, called identically for a fresh insert and for a re-declare
// re-anchor, so the anchor identity is established uniformly. It overwrites the anchor
// connection and records the peer pid and process start-tick (diagnostic identity of the
// anchoring connection's peer since S13 deleted the dump/reload/kill-probe layer that
// used to read them back).
//
// It MUST run inside enqueueAdmitInternal's queue.mu critical section, atomically with
// the idempotent SET: the overwrite and the SET being one critical section is what makes
// compare-and-release correct under the reconnect race. A stale old connection's EOF
// then either already ran (and the SET finds no lease, inserting a fresh one) or finds
// the anchor now points at the re-declaring connection and no-ops — never discharges a
// lease the re-declare holds. queue.mu must be held.
func anchorLeaseLocked(w *admitWaiter, conn net.Conn, pid int, startTick uint64) {
	w.anchor = conn
	w.clientPID = pid
	w.processStartTick = startTick
}

// newEstablishedWaiter builds a GRANTED + accounted lease (grantedCh already
// closed, outcome "immediate") for the establish-granted path: S9's absent-lease
// ARDR re-declare (the sole caller since S13 deleted the restart-dump reload). The
// caller appends it to queue.waiters, anchors it to the live connection via
// anchorLeaseLocked, and re-derives the ledger, under queue.mu.
//
// grantedCh is closed immediately so the "granted ⇒ grantedCh closed" invariant
// every other granted waiter holds is preserved (nothing waits on it on these
// paths, but a later reader must not block). outcome is "immediate", NOT a novel
// spelling: validRunnerAdmitGrant accepts only {immediate,waited,unevaluated}, so a
// later plain dup-scope admit that re-anchors and frames this lease stays valid.
func newEstablishedWaiter(seq, reserve, cpu int64, basis string, request admitRequest, now time.Time) *admitWaiter {
	grantedCh := make(chan struct{})
	close(grantedCh)
	return &admitWaiter{
		seq: seq, reserve: reserve, cpu: cpu, basis: basis,
		state: admitGranted, accounted: true, grantedCh: grantedCh,
		enqueued: now, grantedAt: now, outcome: "immediate",
		scopeID: request.scopeID, name: request.name, owner: request.owner,
		signature:     boundedAdmitSignature(request.signature),
		parentScopeID: request.parentScopeID, scopeCeiling: request.scopeCeiling,
	}
}

// watchPeerEOF starts the peer-EOF liveness watcher shared by every lease-bearing
// connection (design §3): a goroutine blocks reading one byte from conn and cancels
// the returned context the instant the read returns — peer close, or any error. The
// holder needs this connection anyway; holder death ⇒ connection EOF ⇒ the context
// fires, which the caller keys its release on. Extracted from the byte-identical
// inline goroutines in admitConnection and workerAdmitConnection so both establish
// liveness the same way; S9's re-declare handler and S15's worker lease reuse it.
func watchPeerEOF(conn net.Conn) (context.Context, context.CancelFunc) {
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		var one [1]byte
		_, _ = conn.Read(one[:])
		cancel()
	}()
	return ctx, cancel
}

// peerCredentialOf reads the connecting peer's uid and pid through the SO_PEERCRED
// seam (s.peerCredential, defaulting to unixPeerCredential), the same mechanism the
// supervisor-lease handler uses. Read BEFORE taking queue.mu (no getsockopt under the
// hot lock). A failure (e.g. a net.Pipe test connection with no injected seam) returns
// an error the caller tolerates on the fresh-admit path; only the re-declare same-uid
// gate depends on it, and an unreadable credential there fails closed (peerSameUID
// stays false).
func (s *Server) peerCredentialOf(conn net.Conn) (uid, pid int, err error) {
	credential := s.peerCredential
	if credential == nil {
		credential = unixPeerCredential
	}
	return credential(conn)
}

func (s *Server) enqueueAdmit(path string, reserve int64) (*sliceQueue, *admitWaiter, string, error) {
	return s.enqueueAdmitInternal(path, reserve, "", 0, false, admitRequest{})
}

func (s *Server) enqueueResolvedAdmit(path string, reserve int64, basis string, maximum int64) (*sliceQueue, *admitWaiter, string, error) {
	return s.enqueueAdmitInternal(path, reserve, basis, maximum, true, admitRequest{})
}

func (s *Server) enqueueResolvedConfineAdmit(path string, reserve int64, basis string, maximum int64, request admitRequest) (*sliceQueue, *admitWaiter, string, error) {
	return s.enqueueAdmitInternal(path, reserve, basis, maximum, true, request)
}

// enqueueReDeclare is the S9 ARDR re-declare entrypoint into the idempotent SET. It
// re-anchors a PRESENT granted lease (the S8 SET — identical to a dup-scope admit) or,
// when the lease is ABSENT (the crash-restart-no-dump case), ESTABLISHES it granted
// directly under queue.mu, SKIPPING the ceiling gates (design §4 re-declare window;
// `available` may go negative). request.reDeclare is set HERE, so this is the only path
// that establishes-granted on absence — a plain admit's enqueueResolvedConfineAdmit
// leaves it false and an absent lease stays a fresh QUEUED insert.
//
// enforceCeiling is FALSE, not `maximum` with true: both re-declare branches return
// before the ceiling check, so `maximum` is dead — false documents "a re-declare never
// enforces the ceiling" rather than relying on a latent 0 that a future fall-through
// could read as a wrong refusal.
func (s *Server) enqueueReDeclare(path string, reserve int64, basis string, request admitRequest) (*sliceQueue, *admitWaiter, string, error) {
	request.reDeclare = true
	return s.enqueueAdmitInternal(path, reserve, basis, 0, false, request)
}

func (s *Server) enqueueAdmitInternal(path string, reserve int64, basis string, maximum int64, enforceCeiling bool, request admitRequest) (*sliceQueue, *admitWaiter, string, error) {
	s.admitRegistryMu.Lock()
	if s.admitQueues == nil {
		s.admitQueues = make(map[string]*sliceQueue)
	}
	queue := s.admitQueues[path]
	if queue == nil {
		poll := s.admitPollInterval
		if poll <= 0 {
			poll = defaultAdmitPollInterval
		}
		queue = &sliceQueue{path: path, kick: make(chan struct{}, 1), stop: make(chan struct{}), stopped: make(chan struct{}), poll: poll, server: s}
		s.admitQueues[path] = queue
		go queue.runEvaluator()
	}
	queue.mu.Lock()
	defer queue.mu.Unlock()
	defer s.admitRegistryMu.Unlock()
	// S8 idempotent SET-by-scope-id (design §2/§4) — FIRST, before the maxWaiters,
	// too-large, single-exclusive and seq guards below. A re-declare of an existing
	// GRANTED lease re-anchors it in place: it adds no waiter (so maxWaiters must not
	// refuse it) and Inv 6 says a re-declare is always accepted even if it pushes
	// available negative (so the too-large ceiling check must not refuse it). S9's
	// ARDR re-declare handler routes to this same SET, so the early return here is what
	// keeps S9 free of those fresh-admission gates.
	if request.scopeID != "" {
		if existing := leaseByScopeIDLocked(queue, request.scopeID); existing != nil {
			if existing.state == admitGranted {
				// Gate on admitGranted, NOT leaseByScopeIDLocked's `!= admitReleased`
				// (see its CAUTION): a REJECTED-but-not-yet-removed lease is still
				// `!= admitReleased`, and re-anchoring onto it would SET a dead lease
				// whose deferred release is about to discharge it. A granted lease is
				// always accounted (both set together at the grant), so the SET
				// preserves granted && accounted and the re-derive below keeps counting
				// it.
				//
				// Same-uid gate (design §4 gate P2-C): SO_PEERCRED same-uid only, NO
				// cgroup-membership check — both confine and aitest lease holders live
				// OUTSIDE their own scope, so a scope→cgroup-membership check would
				// reject every legitimate re-declare. peerSameUID fails closed when the
				// credential was unreadable.
				if !request.peerSameUID {
					return nil, nil, CodeProtocol, fmt.Errorf("%s: re-declare peer is not the lease owner", CodeProtocol)
				}
				// P2-D: an exclusive lease is LOST on reconnect — the client reports
				// exclusive=lost and does NOT re-declare it. Refuse the re-declare if
				// EITHER side is exclusive: `request.exclusive` (a request that claims it)
				// OR `existing.exclusive` (the live lease IS exclusive). The lease-side
				// check is the REACHABLE one on the S9 ARDR path — the frame has no
				// exclusive field, so request.exclusive is always false there; without the
				// existing.exclusive guard a non-exclusive re-declare would silently
				// re-anchor a live exclusive holder, transferring the slice-wide HOLD to a
				// connection that never asked for it (and slipping past the
				// single-exclusive-per-slice guard below, since the SET returns first).
				if request.exclusive || existing.exclusive {
					return nil, nil, CodeProtocol, fmt.Errorf("%s: an exclusive lease is never re-declared (exclusive=lost on reconnect)", CodeProtocol)
				}
				// S9 parent_scope_id (design §4): a re-anchor LEAVES the established
				// parentScopeID alone — the lease's parent is a property of the original
				// establish, not of the reconnecting client. A mismatch is LOGGED, never
				// gated (gating would refuse a legitimate re-declare over a cosmetic
				// disagreement). Only logged when BOTH are non-empty, so an empty parent on
				// either side (legal by design) is silent.
				if existing.parentScopeID != "" && request.parentScopeID != "" && existing.parentScopeID != request.parentScopeID {
					log.Printf("aira daemon: re-declare for scope %q carries parent_scope_id %q but the established lease has %q; keeping the established value (not gated)",
						request.scopeID, request.parentScopeID, existing.parentScopeID)
				}
				// Idempotent SET of the resource vector + re-anchor to the new
				// connection. reserve and cpu are the two ledger resources (§2); the
				// re-derive folds the refreshed vector back into the per-slice ledger.
				// enqueued is NOT reset (it is the FIFO position, and the lease is
				// already granted), and exclusive is left untouched (refused above).
				// anchorLeaseLocked overwrites the anchor atomically with this SET, which
				// is what makes a concurrent stale old-connection EOF a no-op.
				existing.reserve = reserve
				existing.cpu = request.cpu
				anchorLeaseLocked(existing, request.conn, request.clientPID, request.processStartTick)
				queue.outstanding, queue.cpuOutstanding, queue.outstandingJobs = rederiveLedgerLocked(queue)
				// A re-declare that SHRINKS the vector frees capacity; wake the queue so
				// a waiter that now fits is not stalled to the next poll tick (the
				// fresh-insert path signals for the same reason).
				queue.signal()
				return queue, existing, "", nil
			}
			// A queued lease means the original connection is still establishing this
			// scope's lease, and a rejected-dying one is about to be torn down: both are
			// genuine duplicates, refused exactly as before (CodeProtocol, behaviour
			// preserved — TestConfineRegistryRejectsDuplicateScopeID pins this). Only a
			// live GRANTED lease re-anchors.
			return nil, nil, CodeProtocol, fmt.Errorf("%s: confine scope_id is already registered", CodeProtocol)
		} else if request.reDeclare {
			// ESTABLISH-GRANTED (design §4, the load-bearing addition). The lease is
			// ABSENT and this is an ARDR re-declare (S9). It establishes the lease GRANTED
			// directly here, accounted and anchored to the live connection, SKIPPING the
			// ceiling AND maxWaiters gates below: Invariant 6 says a re-declare is always
			// accepted, even past the ceiling — `reserve > ceiling` just establishes with
			// `available` NEGATIVE (§4's re-declare window), which the signed ledger
			// absorbs and the next NEW admission waits on.
			//
			// The driver is ANY restart (crash or graceful): S13 deleted the dump, so the
			// new daemon always opens an EMPTY ledger and EVERY live client's keeper
			// re-declare is absent-lease. They MUST re-establish GRANTED — queuing them
			// behind the new-admission freeze would time them out and drop live leases.
			// SAME-UID gated (fail-closed: peerSameUID is false on an unreadable
			// credential, so a build that never resolves it cannot establish a lease for a
			// peer it could not authenticate — design §4 gate P2-C).
			//
			// A plain dup-scope admit (reDeclare false) falls through to the fresh QUEUED
			// insert, a genuine new admission.
			if !request.peerSameUID {
				return nil, nil, CodeProtocol, fmt.Errorf("%s: re-declare peer is not the lease owner", CodeProtocol)
			}
			if queue.seq == math.MaxInt64 {
				return nil, nil, CodeProtocol, fmt.Errorf("%s: admission arrival sequence overflow", CodeProtocol)
			}
			queue.seq++
			waiter := newEstablishedWaiter(queue.seq, reserve, request.cpu, basis, request, s.admitNowTime())
			anchorLeaseLocked(waiter, request.conn, request.clientPID, request.processStartTick)
			queue.waiters = append(queue.waiters, waiter)
			// DERIVED, not incremented (the one ledger writer): folds in this lease's RAM
			// and cores. An establish CONSUMES capacity, so no signal() is needed — unlike
			// a re-anchor that may SHRINK the vector and free room for a waiter.
			queue.outstanding, queue.cpuOutstanding, queue.outstandingJobs = rederiveLedgerLocked(queue)
			return queue, waiter, "", nil
		}
	}
	if len(queue.waiters) >= admitMaxWaiters {
		return nil, nil, CodeBusy, fmt.Errorf("%s: too many admission waiters for slice", CodeBusy)
	}
	if enforceCeiling && reserve > subtractFloor(maximum, s.admitSliceHeadroom(queue.outstandingJobs+1)) {
		return nil, nil, CodeAdmitTooLarge, fmt.Errorf("%s: required reserve exceeds cap minus headroom", CodeAdmitTooLarge)
	}
	// AIRA-101. At most ONE exclusive waiter per slice, refused here under
	// queue.mu so the check is race-free beside the duplicate-scope-id check
	// above.
	//
	// This makes "fairness among multiple simultaneous exclusive requesters"
	// unrepresentable rather than something to arbitrate. Without it, exclusive
	// requesters chain — a second becomes the drain head the instant the first
	// releases — and ordinary waiters on the slice starve indefinitely.
	//
	// exclusiveActive() and NOT `state != admitReleased`: see its doc comment. A
	// rejected-but-not-yet-removed waiter matching here would refuse every future
	// exclusive request on this slice until a daemon restart.
	if request.exclusive {
		for _, existing := range queue.waiters {
			if existing.exclusiveActive() {
				return nil, nil, CodeAdmitExclusiveActive, fmt.Errorf(
					"%s: another exclusive request is already active on this slice (%s); retry when it completes",
					CodeAdmitExclusiveActive, exclusiveStateOf(existing))
			}
		}
	}
	if queue.seq == math.MaxInt64 {
		return nil, nil, CodeProtocol, fmt.Errorf("%s: admission arrival sequence overflow", CodeProtocol)
	}
	queue.seq++
	waiter := &admitWaiter{seq: queue.seq, reserve: reserve, cpu: request.cpu, basis: basis, state: admitQueued, grantedCh: make(chan struct{}), enqueued: s.admitNowTime(), scopeID: request.scopeID, name: request.name, owner: request.owner, signature: boundedAdmitSignature(request.signature), exclusive: request.exclusive, exclusiveReason: request.exclusiveReason, exclusiveHolder: request.exclusiveHolder, parentScopeID: request.parentScopeID, scopeCeiling: request.scopeCeiling}
	// Anchor the fresh lease to its connection through the same helper a re-declare
	// uses, so the anchor identity is set uniformly. The connection's EOF release
	// compares its own conn against this anchor.
	anchorLeaseLocked(waiter, request.conn, request.clientPID, request.processStartTick)
	queue.waiters = append(queue.waiters, waiter)
	queue.signal()
	return queue, waiter, "", nil
}

func (q *sliceQueue) runEvaluator() {
	// Closed on exit so a caller can establish this goroutine is GONE, not merely
	// asked to stop. See sliceQueue.stopped.
	defer func() {
		if q.stopped != nil {
			close(q.stopped)
		}
	}()
	ticker := time.NewTicker(q.poll)
	defer ticker.Stop()
	for {
		select {
		case <-q.kick:
			q.server.evaluateAdmitQueue(q)
		case <-ticker.C:
			q.signal()
		case <-q.stop:
			return
		}
	}
}

func (q *sliceQueue) signal() {
	select {
	case q.kick <- struct{}{}:
	default:
	}
}

func (s *Server) evaluateAdmitQueue(queue *sliceQueue) {
	// Production has exactly one caller: this queue's runEvaluator goroutine, the
	// single writer of queue state; other goroutines read it only under queue.mu.
	// S14 deleted the periodic cgroup scan that used to run here — emptiness is now
	// derived from the signed ledger (sliceProvablyEmpty), so a pass touches only
	// queue state.
	now := s.admitNowTime()

	queue.mu.Lock()
	defer queue.mu.Unlock()
	readMemory := s.memoryReader()
	current, maximum, reclaimable, ok, _ := readMemory(queue.path)
	if !ok {
		// Fail CLOSED: without a slice-memory read the ceiling cannot be
		// established, so granting queued waiters would be uncounted (no
		// outstanding/outstandingJobs) and abandon the Σ(reserve) ≤ cap-headroom
		// invariant — re-opening the slice-cap random-victim OOM this design
		// prevents. Leave the waiters queued; the poll ticker re-evaluates when
		// the read recovers, and each waiter's own maxWait deadline still fires
		// (timeoutAdmitWaiter → E_ADMIT_SATURATED) if it never does.
		return
	}
	// AIRA-59 duty cycle. Deliberately placed AFTER the fail-closed slice-read
	// return above, so a transient unreadable-slice pass can never advance or
	// restart a phase — a blip must not hand anyone a fresh exclusive window.
	maxHold := s.admitFreezeMaxHold
	// Derived, not stored. Uses pass-start `now`, the same instant the grace check
	// below uses, so hold and yield shift symmetrically if anything delays the pass.
	phase := admitFreezePhaseAt(queue.freezeArmedAt, now, maxHold)
	if maxHold > 0 && phase == admitFreezeIdle && !queue.freezeArmedAt.IsZero() {
		// A completed cycle must YIELD AT LEAST ONE EVALUATION before re-arming.
		// The phase is derived from wall time, but grants only happen during an
		// evaluator pass, so a yield window that elapses entirely BETWEEN passes
		// would let the queue go hold -> idle -> re-armed in a single pass and
		// backfill nothing at all — freezing forever while looking well-behaved.
		// That happens whenever maxHold approaches the poll interval (any positive
		// duration is accepted) or anything else delays a pass past a whole cycle.
		// Clearing the anchor and treating THIS pass as a yield
		// makes the guarantee "at least one backfilling pass per cycle", which is
		// what actually admits waiters, rather than merely "some wall time spent
		// nominally yielding".
		queue.freezeArmedAt = time.Time{}
		phase = admitFreezeYield
	}
	// S14: the drain head no longer needs an abort path. Emptiness is ledger-
	// derived and always readable under queue.mu, so a drain converges as leases
	// release (sliceProvablyEmpty) and is otherwise ended by the waiter's own
	// max_wait or connection close — there is no "unreadable slice" case left to
	// stall it.
	gate := exclusiveGateLocked(queue)
	// AIRA-103. The one place the pressure throttle actually gates admission.
	// Hoisted out of the waiter loop: it is a per-pass fact, and a pure leaf-lock
	// lookup that must not be repeated per waiter while queue.mu is held.
	// DELIBERATELY not applied to the ceiling admitConnection computes from the
	// same file: that one decides the TERMINAL E_ADMIT_TOO_LARGE and sizes a
	// job's own hard scope cap, so a job too large for the throttled ceiling must
	// WAIT here rather than be refused there.
	// S4 (D4): the AIRA-103/106 pressure ceiling is MemAvailable-aware and part of
	// the DEV system-aware gate ONLY. CI (ci-shim / advisory) is ledger-only —
	// nothing runs outside the container, so the container memory.max IS the
	// ceiling; there is no slice pressure to throttle against and the sampler does
	// not run. Mode-gating it here (rather than relying on the shim snapshot being
	// inert) is the explicit seam the "dev skips the system-RAM check" mutation
	// flips.
	effectiveMaximum := maximum
	if !s.shimMode() {
		effectiveMaximum = s.admitEffectiveMaximum(queue.path, maximum)
	}
	frozen := false
	// S11 restart freeze (design §4): a per-pass fact like `phase`. While active, NO
	// NEW admission is granted — survivors must re-declare (re-anchor / establish) their
	// leases before a new admission can take space they are about to re-claim. Re-declares
	// do NOT come through here: they SET/establish directly under queue.mu in
	// enqueueAdmitInternal, so they are never frozen (Invariant 6).
	restartFrozen := s.restartFrozenAt(now)
	// AIRA-149. Still-queued waiters already examined in THIS pass, i.e.
	// genuinely AHEAD of any waiter reached later in it. Diagnosis only.
	queuedAhead := 0
	for _, waiter := range queue.waiters {
		if waiter.state != admitQueued {
			continue
		}
		// S11 restart-freeze gate (design §4). Placed BEFORE the exclusivity gate and the
		// fit/grant so a NEW admission during the freeze simply WAITS — it never grants
		// (never fail-opens), never arms the AIRA-59 fairness anchor (that arm lives in the
		// refused-on-capacity block below, which this skips), and records NO contention and
		// NO grantable. The no-contention part is load-bearing honesty: a non-blocking
		// (max_wait_ms==0) admit that times out during the freeze must then read
		// Contention "unevaluated" (the unset AIRA-149 latch) — the slice is not saturated,
		// it is frozen — rather than a fabricated "saturated" solitude or a grant-shaped
		// "unevaluated" (which the runner launches UNCAPPED). A blocking waiter re-evaluates
		// at freeze-end, woken by the restart timer's signal or the next poll tick.
		if restartFrozen {
			waiter.waited = true
			continue
		}
		// AIRA-101, the exclusivity gate. Placed before the RAM fit check because
		// exclusivity is an ADDITIONAL orthogonal gate: a waiter it lets through
		// still has to pass every existing check on its own merits.
		//
		// Blocked waiters `continue` from here, BEFORE the freeze branch below, so
		// a drain never arms or advances the AIRA-59 fairness anchor: that duty
		// cycle exists to stop backfill starvation of a head, and during a drain
		// there is no backfill to stop.
		if gate.blocks(queue, waiter) {
			waiter.waited = true
			// AIRA-149. A waiter that is not the drain head is blocked because
			// another waiter is exclusively holding or draining the slice --
			// something else is in the way BY CONSTRUCTION, so it latches observed
			// directly. The drain head itself is blocked by !sliceProvablyEmpty
			// (Σleases > 0), so it takes the shared ledger reading: `observed` while
			// a lease is held, `none-observed` once the slice is empty. At render
			// time the AIRA-101 Exclusive arm wins the wording, but the stored
			// contention value must still be the truthful one.
			if gate.draining != nil && waiter == gate.draining {
				waiter.joinContentionLocked(soloReadingLocked(queue, queuedAhead))
			} else {
				waiter.joinContentionLocked(contentionObserved)
			}
			queuedAhead++
			continue
		}
		jobs := addJobCountClamp(queue.outstandingJobs, 1)
		headroom := s.admitSliceHeadroom(jobs)
		// S4 (D4): the LEDGER check (ceiling − Σleases, signed) applies in BOTH
		// modes; the physical current/reclaimable floor is DEV-only. CI drops it
		// (ledger-only) because nothing runs outside the container; DEV keeps it,
		// so a slice already over its declared reserve is gated on the real bytes,
		// alongside the MemAvailable-aware effectiveMaximum above.
		//
		// S12: a single accounting — the connection-held ledger (queue.outstanding).
		// The AIRA-74 scan-adoption addend is gone; a post-restart survivor is
		// counted ONCE via S11's reload + re-declare, never a second time via a
		// parallel scan-reconstructed reserve.
		outstanding := queue.outstanding
		var available int64
		if s.shimMode() {
			available = ledgerAvailable(effectiveMaximum, outstanding, headroom)
		} else {
			available = checkedAvailable(current, effectiveMaximum, reclaimable, outstanding, headroom)
		}
		if frozen {
			waiter.waited = true
			// AIRA-149. `frozen` is only ever set by a waiter AHEAD in this same
			// pass that was refused on capacity, so queuedAhead is already >= 1 and
			// the shared reading cannot return none-observed here.
			waiter.joinContentionLocked(soloReadingLocked(queue, queuedAhead))
			waiter.noteGrantableLocked(available)
			queuedAhead++
			continue
		}
		// S5 CONJUNCTIVE FIT (design §7): admit only if EVERY resource fits — RAM
		// AND CPU. Both are PER-SLICE ledgers: `available` is RAM (ceiling − Σreserve),
		// and CPU is this queue's own cpuOutstanding against the 2×NumCPU ceiling. CPU
		// is treated per slice on the one-slice (aira.slice) assertion (D1); cores are
		// machine-wide, so if concurrent slices are ever introduced this must become a
		// sum across slices. grantedAt marks "the daemon just decided this job may
		// proceed", deliberately separate from enqueued (a long queue wait is not launch
		// abandonment — the AIRA-49 v3 defect); nothing but the grant below sets it.
		ramFits := waiter.reserve <= available
		cpuFits := waiter.cpu <= cpuAvailable(s.cpuCeiling(), queue.cpuOutstanding)
		if ramFits && cpuFits {
			waiter.state = admitGranted
			waiter.grantedAt = s.admitNowTime()
			waiter.accounted = true
			// The ledger is DERIVED, not incremented: this waiter is now granted &&
			// accounted, so re-deriving over the waiter set folds in BOTH its RAM
			// ledgerCharge() and its cores. Done here, before the next queued waiter is
			// evaluated, so a later grant in this same pass reads this one at the
			// fit-check above — exactly as the old `outstanding +=` did, now for two
			// resources at once.
			queue.outstanding, queue.cpuOutstanding, queue.outstandingJobs = rederiveLedgerLocked(queue)
			if waiter.waited {
				waiter.outcome = "waited"
				waiter.waitedMS = elapsedMilliseconds(waiter.enqueued, s.admitNowTime())
			} else {
				waiter.outcome = "immediate"
			}
			close(waiter.grantedCh)
			continue
		}
		{
			// Refused on capacity — RAM short OR CPU short — takes the same FIFO tail:
			// record the RAM grantable figure and arm the AIRA-59 backfill freeze so a
			// later smaller waiter cannot jump the head.
			//
			// ACCEPTED GAP (S5, out of scope): noteGrantableLocked records the RAM
			// `available` figure even for a CPU-only refusal, and there is no CPU
			// diagnostic field — so a job blocked purely on CPU is rejected with
			// E_ADMIT_SATURATED rendering the RAM-flavoured "no memory admission within
			// the wait". Diagnosis only (no admission decision reads it); a dedicated CPU
			// diagnostic is deferred. The contention latch below still tells the honest
			// truth (observed, never a fabricated solitude).
			waiter.waited = true
			// AIRA-149 contention latch, S5-aware. A RAM refusal (ramFits == false) is a
			// fact about THIS slice's ledger, so it takes the honest solo reading — which
			// may legitimately be none-observed (the residual-page case). A CPU refusal
			// (ramFits but !cpuFits) latches `observed` directly: this slice's own cores
			// are held, so something IS in the way by construction. (It would read
			// `observed` via soloReadingLocked anyway — a CPU-full slice has
			// outstandingJobs ≥ 1, so it is never provablyEmpty — but stating it here is
			// the direct, intent-revealing guard against ever rendering a fabricated
			// RAM-solitude diagnosis for a CPU refusal.) soloReadingLocked is the ONLY
			// site that may latch none-observed, and only the RAM arm may reach it.
			if ramFits {
				waiter.joinContentionLocked(contentionObserved)
			} else {
				waiter.joinContentionLocked(soloReadingLocked(queue, queuedAhead))
			}
			waiter.noteGrantableLocked(available)
			queuedAhead++
			// now is pass-start time, so anything delaying the pass defers this freeze by its duration.
			if s.admitBackfillGrace <= 0 || now.Sub(waiter.enqueued) >= s.admitBackfillGrace {
				switch {
				case maxHold <= 0:
					// Duty cycle disabled: freeze exactly as before, and write NO phase
					// state, so the anchor stays meaningless in this mode rather than
					// accumulating values nothing reads.
					//
					// Note this branch is behaviourally equivalent to falling through
					// (admitFreezePhaseAt returns idle when maxHold <= 0); it exists to
					// keep the anchor untouched, not because freezing differs. An
					// earlier comment here claimed disabled mode differed by protecting
					// a successor younger than the backfill grace — that was wrong: the
					// grace check above gates this switch in EVERY mode, so a young head
					// never freezes either way.
					frozen = true
				case phase != admitFreezeYield:
					frozen = true
					if phase == admitFreezeIdle {
						// The arm: one of the two anchor writes (see the struct). Guarded
						// by idle, so an active hold cannot renew itself and a departing
						// holder cannot buy a fresh window.
						queue.freezeArmedAt = now
						phase = admitFreezeHold
					}
					queue.freezeHolderSeq = waiter.seq
				}
			}
			continue
		}
	}
	// Log BEFORE clearing the diagnostics holder: a hold->yield transition is
	// exactly the moment an operator wants to see WHICH waiter was being
	// protected, and clearing first would strip that from the one line reporting it.
	if maxHold > 0 && phase != queue.freezeLogged {
		s.logAdmitFreezeTransition(queue, phase, now)
		queue.freezeLogged = phase
	}
	if !frozen {
		// The head fitted, was granted, or left. Clear only the DIAGNOSTICS seq —
		// the anchor deliberately survives, because clearing it here would let
		// repeated holder-fit churn restart fresh holds, which is the same
		// unbounded-freeze defect as re-anchoring, by another route.
		queue.freezeHolderSeq = 0
	}
}

// logAdmitFreezeTransition reports ONLY phase transitions, never a steady state:
// evaluator passes run at up to 4/s, so logging an ongoing freeze every pass
// would itself be a regression on a busy box. Called with queue.mu held.
func (s *Server) logAdmitFreezeTransition(queue *sliceQueue, phase admitFreezePhase, now time.Time) {
	queued := 0
	var holder *admitWaiter
	for _, waiter := range queue.waiters {
		if waiter == nil {
			continue
		}
		if waiter.state == admitQueued {
			queued++
		}
		if queue.freezeHolderSeq != 0 && waiter.seq == queue.freezeHolderSeq {
			holder = waiter
		}
	}
	if holder == nil {
		log.Printf("aira daemon: admission fairness-freeze %s on %s (%d queued)", phase, queue.path, queued)
		return
	}
	log.Printf("aira daemon: admission fairness-freeze %s on %s: head seq=%d reserve=%d queued-for=%s (%d also queued)",
		phase, queue.path, holder.seq, holder.reserve,
		now.Sub(holder.enqueued).Round(time.Second), subtractJobCount(queued, 1))
}

func subtractJobCount(value, subtract int) int {
	if value <= subtract {
		return 0
	}
	return value - subtract
}

// checkedAvailable is the DEV (real-cgroup) physical-floor availability:
// ceiling − max(effectiveCurrent, Σleases). The charge is the LARGER of the
// slice's own physical use (memory.current less the AIRA-21 reclaimable discount)
// and the declared ledger, so a slice already over its declared reserve is gated
// on the real bytes.
//
// S4 (D4): the result is SIGNED. A charge that exceeds a VALID ceiling — the
// ledger driven past the ceiling during the restart re-declare window (§4), or a
// physical over-use — yields a NEGATIVE available, which makes the next NEW
// admission wait until a release recovers it, rather than a clamp-at-zero that
// hides the deficit. Only INVALID inputs and a degenerate ceiling (headroom >=
// maximum) report 0: those are an unusable reading, not a legitimately
// over-subscribed ledger.
func checkedAvailable(current, maximum, reclaimable, outstanding, headroom int64) int64 {
	if current < 0 || maximum < 0 || outstanding < 0 || headroom < 0 || maximum <= headroom {
		return 0
	}
	if reclaimable < 0 {
		reclaimable = 0
	}
	effectiveCurrent := subtractFloor(current, reclaimable)
	ceiling := maximum - headroom
	charge := outstanding
	if effectiveCurrent > charge {
		charge = effectiveCurrent
	}
	// SIGNED: ceiling and charge are both non-negative, so ceiling − charge cannot
	// overflow, and a charge past the ceiling is a legitimate negative available.
	return ceiling - charge
}

// ledgerAvailable is the CI (ci-shim / advisory) mode availability: the signed
// ledger ceiling − Σleases, with NO physical floor. In ci-shim mode nothing runs
// outside the container, so declared reserves + the container memory.max (the
// ceiling) are the whole truth (D4, design §7) — a large host-wide memory.current
// says nothing about this container and must not gate it. It reuses
// checkedAvailable with a zero physical reading, so the ceiling/headroom guards
// and the signed result are identical to the dev path's ledger term.
func ledgerAvailable(maximum, outstanding, headroom int64) int64 {
	return checkedAvailable(0, maximum, 0, outstanding, headroom)
}

// cpuCeiling is the CPU ceiling: 2 × NumCPU cores (design §7). It is an INTEGER
// derived purely from the core count — no cgroup read, and no cpu.max is ever
// written; the 2× over-provision caps admission busyness while the kernel
// time-shares on cpu.weight. The core count comes through the cpuCoreCounter seam
// so a test can pin a deterministic ceiling. This is the ONLY per-resource code
// CPU adds — admit/available/fit/release/wake are otherwise resource-agnostic.
//
// The ceiling is applied PER SLICE against queue.cpuOutstanding (D1, resolved to
// per-slice on the one-slice aira.slice assertion). Cores are a machine-wide
// resource; a second concurrent slice would let Σ across slices exceed 2×NumCPU,
// so if concurrent slices are ever introduced this must become machine-wide.
func (s *Server) cpuCeiling() int64 {
	return 2 * int64(s.cpuCoreCounter()())
}

// cpuAvailable is the signed CPU-ledger availability, the sibling of
// checkedAvailable/ledgerAvailable for the CPU resource: ceiling − Σ(this slice's
// live lease cores). Signed like the RAM ledger — a slice momentarily over its CPU
// ceiling simply makes the next new admission wait. Cores are small integers, so
// the subtraction cannot overflow.
func cpuAvailable(ceiling, outstanding int64) int64 {
	return ceiling - outstanding
}

func (s *Server) timeoutAdmitWaiter(queue *sliceQueue, waiter *admitWaiter) {
	queue.mu.Lock()
	if waiter.state != admitQueued {
		queue.mu.Unlock()
		return
	}
	waiter.state = admitRejected
	waiter.outcome = "saturated"
	waiter.waitedMS = elapsedMilliseconds(waiter.enqueued, s.admitNowTime())
	close(waiter.grantedCh)
	queue.mu.Unlock()
	queue.signal()
}

func (s *Server) releaseAdmitWaiter(queue *sliceQueue, waiter *admitWaiter) {
	queue.mu.Lock()
	released := releaseAdmitWaiterLocked(queue, waiter)
	queue.mu.Unlock()
	if released {
		s.afterAdmitRelease(queue)
	}
}

// releaseAdmitWaiterAnchored is the socket-EOF release: the connection that anchored
// the lease discharges it on its own EOF, via the compare-and-release gate. conn is the
// releasing handler's own connection. See releaseAdmitWaiterLockedAnchored. It runs
// afterAdmitRelease only when it performed the discharge, and RETURNS whether it did.
//
// The returned bool is the S2a §16.2 (P1-B) anchor gate: a worker relay's peer-EOF
// kills+rmdirs its sibling scope ONLY when its own anchored release actually
// discharged — a stale connection whose lease was re-anchored to a live redial gets
// false here, so it never kills a mid-test worker (a release is idempotent; a kill
// is not).
func (s *Server) releaseAdmitWaiterAnchored(queue *sliceQueue, waiter *admitWaiter, conn net.Conn) bool {
	queue.mu.Lock()
	released := releaseAdmitWaiterLockedAnchored(queue, waiter, conn)
	queue.mu.Unlock()
	if released {
		s.afterAdmitRelease(queue)
	}
	return released
}

// releaseAdmitWaiterLockedAnchored is compare-and-release (design §3, Inv 4), with
// queue.mu ALREADY HELD: it discharges the lease ONLY if the EOF is from the connection
// that is CURRENTLY the anchor — i.e. waiter.anchor still IS conn. A re-declare on a new
// connection overwrote the anchor (anchorLeaseLocked), so this stale connection's later
// EOF releases nothing. The compare is direct identity on the live net.Conn the handler
// holds for the lease's lifetime — NO generation value is captured, so none can be
// captured in a critical section separate from the SET that set it (the reconnect-race
// lost-lease bug this avoids by construction).
//
// This is the primary release path: every lease is released by its current-anchor
// connection's EOF (S15's worker-lease EOF reuses THIS variant). The unconditional
// releaseAdmitWaiterLocked is retained for the remaining reclaim paths that are NOT
// keyed on a releasing connection — the operator `confine --kill`, the exclusive
// unwedge, the physical-reap stale-lease backstop (S14 deleted the scan-derived
// vanished branch), and the tests. A socket-EOF release must NOT route through the
// unconditional form, or the anchor gate is bypassed.
//
// The caller runs afterAdmitRelease once it has dropped queue.mu, and only when this
// returned true.
func releaseAdmitWaiterLockedAnchored(queue *sliceQueue, waiter *admitWaiter, conn net.Conn) bool {
	if waiter.state == admitReleased {
		return false
	}
	// conn == nil is refused defensively: an anchored compare-and-release must match a
	// REAL connection, never release on a nil==nil coincidence. Since S13 deleted the
	// dump/reload layer every granted lease is anchored to a live connection (no nil
	// anchors exist), so this is belt-and-braces rather than a reachable guard, but it
	// keeps the illegal nil match unrepresentable.
	if conn == nil || waiter.anchor != conn {
		return false
	}
	return releaseAdmitWaiterLocked(queue, waiter)
}

// releaseAdmitWaiterLocked is the ledger discharge itself, with queue.mu ALREADY
// HELD by the caller. It reports whether THIS call performed the transition, so
// a caller can never log or count a reclaim that a concurrent release had
// already done.
//
// AIRA-68 split this out of releaseAdmitWaiter so the stale-lease sweep can make
// its final validation and its discharge ONE critical section: validating under
// the lock, dropping it, and then discharging would leave a window in which the
// facts the reclaim proof rests on change under the sweep. The physical-reap
// backstop (the only stale-lease reclaim path left after S14 deleted the scan)
// still relies on that single-critical-section discharge.
//
// The caller must run afterAdmitRelease once it has dropped queue.mu, and only
// when this returned true.
func releaseAdmitWaiterLocked(queue *sliceQueue, waiter *admitWaiter) bool {
	if waiter.state == admitReleased {
		return false
	}
	for index, candidate := range queue.waiters {
		if candidate == waiter {
			copy(queue.waiters[index:], queue.waiters[index+1:])
			queue.waiters[len(queue.waiters)-1] = nil
			queue.waiters = queue.waiters[:len(queue.waiters)-1]
			break
		}
	}
	waiter.state = admitReleased
	// Discharge is a re-derive, not a subtraction: this waiter is now removed
	// from queue.waiters AND marked admitReleased, so re-deriving over the
	// survivors drops its ledgerCharge() exactly when it drops any granted &&
	// accounted lease -- which is what makes "outstanding returns to exactly zero"
	// true rather than approximate. The grant and this release are the ledger's
	// only two mutation points, both re-deriving through the one accessor under
	// this lock.
	// S5. Re-derives BOTH resources: the released lease's RAM (outstanding) and its
	// cores (cpuOutstanding) drop together, so the per-slice CPU sum the fit-check
	// reads returns the freed cores immediately on the next pass.
	queue.outstanding, queue.cpuOutstanding, queue.outstandingJobs = rederiveLedgerLocked(queue)
	return true
}

// afterAdmitRelease runs the post-discharge work that must NOT hold queue.mu:
// pruneAdmitQueue takes admitRegistryMu then queue.mu, so calling it under
// queue.mu would invert the one fixed lock order in this file.
func (s *Server) afterAdmitRelease(queue *sliceQueue) {
	// This is only the coalescing, lock-free signal: evaluateAdmitQueue must
	// never be called synchronously from this release path, which has just held
	// queue.mu. It is also the ONLY thing making a freed reserve visible to a
	// waiter immediately rather than at the next 250ms poll, so it is pinned by
	// TestAfterAdmitReleaseKicksTheQueue (AIRA-33 deleted the test that used to
	// carry that assertion alongside a governor one).
	//
	// S5: a CPU release is per-slice (D1, resolved to per-slice), exactly like a RAM
	// release — it frees cores only in THIS slice's ledger, so kicking this queue is
	// sufficient to wake its own CPU-blocked waiters. No cross-queue signal: there is
	// no machine-wide CPU ledger for another slice to be waiting on.
	queue.signal()
	s.pruneAdmitQueue(queue)
}

func (s *Server) pruneAdmitQueue(queue *sliceQueue) {
	// Fixed lock order: registry then slice. Callers never retain queue.mu.
	s.admitRegistryMu.Lock()
	queue.mu.Lock()
	if len(queue.waiters) == 0 && s.admitQueues[queue.path] == queue {
		delete(s.admitQueues, queue.path)
		queue.stopOnce.Do(func() { close(queue.stop) })
	}
	queue.mu.Unlock()
	s.admitRegistryMu.Unlock()
}

func (s *Server) pruneAdmitRegistry() {
	s.admitRegistryMu.Lock()
	for path, queue := range s.admitQueues {
		queue.mu.Lock()
		if len(queue.waiters) == 0 {
			delete(s.admitQueues, path)
			queue.stopOnce.Do(func() { close(queue.stop) })
		}
		queue.mu.Unlock()
	}
	s.admitRegistryMu.Unlock()
}

func (s *Server) writeAdmitGrant(conn net.Conn, grant AdmitResponse) {
	_ = conn.SetWriteDeadline(time.Now().Add(admitWriteTimeout))
	write := s.admitWriteFrame
	if write == nil {
		write = func(conn net.Conn, value any) error { return writeFrame(conn, value) }
	}
	_ = write(conn, responseFrame(core.Response{OK: true, Code: "OK", Data: grant}))
}

// writeAdmitFailClosed refuses a NEW admission the daemon cannot evaluate
// (Invariant 6): an unresolvable slice or an unreadable slice/container budget.
// It NEVER emits a grant — a grant-shaped `unevaluated` here is launched UNCAPPED
// by the runner.
//
// An exclusive request gets the specific U_ADMIT_EXCLUSIVE_UNESTABLISHED (the
// same code the ci-shim exclusive refusal and the drain-abort use) purely for
// HONESTY: the runner's fail() already refuses ANY exclusive request before the
// flock fallback (admission_linux.go), so both codes refuse exclusivity — this
// one just carries the precise reason to the client instead of the generic
// "exchange did not complete". An ordinary refusal routes through the runner's
// fail() to the flock fallback in the interim (S13 replaces that with reconnect +
// re-request); --require-admission already fails closed on any non-admitted state.
func (s *Server) writeAdmitFailClosed(conn net.Conn, exclusive bool, reason string) {
	if strings.TrimSpace(reason) == "" {
		reason = "slice budget unreadable"
	}
	if exclusive {
		s.writeAdmitError(conn, CodeAdmitExclusiveUnestablished,
			CodeAdmitExclusiveUnestablished+": "+reason+" — an empty slice could not be established for an exclusive request")
		return
	}
	s.writeAdmitError(conn, CodeUnavailable, CodeUnavailable+": "+reason)
}

func (s *Server) writeAdmitError(conn net.Conn, code, message string) {
	_ = conn.SetWriteDeadline(time.Now().Add(admitWriteTimeout))
	write := s.admitWriteFrame
	if write == nil {
		write = func(conn net.Conn, value any) error { return writeFrame(conn, value) }
	}
	_ = write(conn, errorFrame(code, message))
}

// saturatedDiagnosisLocked builds the AIRA-149 diagnosis half of a saturated
// rejection. queue.mu must be held, which is where the latch fields are written.
//
// Every value is one this request already established: the DAEMON-RESOLVED
// reserve (not the client's own unresolved request, which is what the message
// used to print under the word "reserve"), the request-entry ceiling, the
// latched contention reading, and the capacity the gate last computed for this
// waiter. Nothing here is consulted by any decision, and Basis keeps its exact
// "reject:saturated" spelling -- validRunnerAdmitRejection pins it, and a
// mismatch would drop the client into the unaccounted flock fallback.
//
// The grantable figure is COPIED out of the waiter rather than aliased, so the
// payload cannot observe a later write once queue.mu is released.
func saturatedDiagnosisLocked(waiter *admitWaiter, reserve, ceiling int64) admitRejection {
	rejection := admitRejection{
		Required: reserve,
		Ceiling:  ceiling,
		Basis:    "reject:saturated",
	}
	if waiter == nil {
		rejection.Contention = admitContentionToken(contentionUnset)
		return rejection
	}
	rejection.Contention = admitContentionToken(waiter.contention)
	if waiter.lastGrantable != nil {
		grantable := *waiter.lastGrantable
		rejection.Grantable = &grantable
	}
	return rejection
}

// tooLargeRefusalAdvice is the case-specific half of the E_ADMIT_TOO_LARGE
// message (AIRA-165, carried from AIRA-151 G3 / AIRA-153 G6). The numbers half
// is unchanged and unconditional: `required`, `cap_minus_headroom` and `basis`
// are printed in EVERY case, and this only says what to DO about them.
//
// This line named no escape hatch at all, and the one place that did -- the
// generated agent guide -- gives a SINGLE instruction for the whole population:
// "pin --memory-reserve at or below the printed cap_minus_headroom". That is
// exactly the wrong thing to tell an operator whose request arrived with the
// reserve already pinned. AIRA-153 narrowed the population enough for the split
// to be tractable: after it, a terminal refusal is an over-ceiling per-signature
// ESTIMATE, an OOM ESCALATION at or above FIT(ceiling), or a CLIENT-PINNED
// reserve over the ceiling.
//
// A fourth arm exists because AIRA-153 §3.2 routes one more population here
// deliberately -- a PRIOR on a slice too small for any viable fitted reserve
// (fit == 0), and the same prior refused at the enqueue re-check when the
// ceiling tightened behind it -- and telling that operator that "this command's
// own measurement" is too large would be a fabricated cause: a prior is a guess,
// and this function must never claim it measured anything.
//
// The case is decided by the basis TERM, never by comparing numbers, on the
// AIRA-149 rule that a label names the term that ACTED: `pinned:client` is
// returned at resolveAdmitReserve's first line and nowhere else,
// `estimate:oom-escalated` only where the escalation strictly set the value, and
// `estimate:p90-prior` is a machine-wide prior rather than this command's own
// history despite its `estimate:` family. Trailing tokens (`,oom-on-record`,
// `,ceiling-fitted`, `,ceiling-clamped`) qualify the term, they do not replace
// it, so they are trimmed before the match rather than pattern-matched around.
//
// An UNRECOGNISED basis gets no advice at all rather than a plausible-looking
// guess: the refusal still names both numbers and the basis, and AIRA does not
// invent a cause it cannot establish.
//
// That same rule bounds what the `pinned:client` arm may SAY, and it is the one
// arm whose cause the daemon genuinely cannot establish (build review, Fable
// BLOCK). The only fact at this call site is the wire flag, and `pinned=true`
// arrives on live paths where the operator passed no flag at all:
//
//   - EVERY `aira run` admission. internal/runner/admission_linux.go sends
//     `pinned: !req.DaemonEstimateMemory || req.MemoryReservePinned`, and the sole
//     setter of DaemonEstimateMemory is confine's own launch path, so `aira run`
//     is ALWAYS `pinned:client` -- carrying a `run.memory_reserve` from
//     .aira/config, or core's own peak-RSS estimate, neither of which is a flag
//     (and `aira run` has no --memory-reserve flag to pass).
//   - `aira confine -- docker run --memory=X` on an UNPINNED job.
//     runner.ContainerPlan.ResolveReserve charges the container's own limit and
//     re-marks the request pinned, so an operator who passed nothing can be
//     refused here.
//   - `aira confine-reserve`, which pins the pytest governor's default-sized
//     per-test reservation.
//
// So the arm asserts only what the wire establishes -- the reserve was pinned
// CLIENT-SIDE, so AIRA neither sized it nor fitted it to this slice -- and names
// the possible origins AS possibilities. Naming a confine flag as the cause
// would be exactly the fabrication the default arm's empty return exists to
// avoid. What survives unchanged is the ACTION (a reserve of at most
// cap_minus_headroom, or a larger slice) and the property that this arm never
// tells the operator to pin, which is the instruction the whole ticket exists to
// stop giving to a request that is already pinned.
//
// covers: AIRA-165
func tooLargeRefusalAdvice(basis string) string {
	term := basis
	if comma := strings.IndexByte(term, ','); comma >= 0 {
		term = term[:comma]
	}
	switch {
	case term == "pinned:client":
		return "this reserve was PINNED on the client side, so AIRA neither sized it nor fitted it to this slice, and it is larger than this slice can grant; which pin is not established here -- it may be a --memory-reserve, --memory-max or --delegate-ram passed to confine, a `docker run --memory` limit AIRA charged for an otherwise unpinned job, or an `aira run` reserve (run.memory_reserve, or AIRA's own estimate, both of which aira run sends pinned); give it a reserve of at most cap_minus_headroom -- lower the one you passed, or set one -- or run where the slice is larger"
	case term == "estimate:oom-escalated":
		return "this command was OOM-killed here, and the reserve its own recorded peak justifies is larger than this slice can grant; it does not fit on this slice -- run where the slice is larger rather than retrying it unchanged"
	case term == "estimate:p90-prior":
		return "this number is a machine-wide PRIOR about other commands, not a measurement of this one, and this slice cannot grant even that; pin a --memory-reserve you know this command fits in, or run where the slice is larger"
	case strings.HasPrefix(term, "estimate:"):
		return "this is AIRA's estimate from this command's OWN measured peak history, and it exceeds what this slice can grant; pin a smaller --memory-reserve only if you know the real need is smaller, otherwise this command cannot run on this slice"
	case strings.HasPrefix(term, "fallback:"):
		return "this number is AIRA's blind default for a command it has not measured, not a measurement of this one, and this slice cannot grant even that; pin a --memory-reserve you know this command fits in, or run where the slice is larger"
	default:
		return ""
	}
}

func (s *Server) writeAdmitRejection(conn net.Conn, code string, rejection admitRejection) {
	_ = conn.SetWriteDeadline(time.Now().Add(admitWriteTimeout))
	write := s.admitWriteFrame
	if write == nil {
		write = func(conn net.Conn, value any) error { return writeFrame(conn, value) }
	}
	message := code + ": " + rejection.Basis
	if code == CodeAdmitTooLarge {
		message = fmt.Sprintf("%s: required=%d cap_minus_headroom=%d basis=%s", code, rejection.Required, rejection.Ceiling, rejection.Basis)
		// Appended, never substituted: the numbers and the basis keep their exact
		// spelling and position, so every existing reader of this line is unaffected
		// and the advice is additive.
		if advice := tooLargeRefusalAdvice(rejection.Basis); advice != "" {
			message += " -- " + advice
		}
	}
	frame := errorFrame(code, message)
	frame.Data, _ = json.Marshal(rejection)
	_ = write(conn, frame)
}

func (s *Server) admitNowTime() time.Time {
	if s.admitNow != nil {
		return s.admitNow()
	}
	return time.Now()
}

func elapsedMilliseconds(start, end time.Time) int64 {
	if end.Before(start) {
		return 0
	}
	return end.Sub(start).Milliseconds()
}

// admitCodedError carries the wire code a validation failure must be reported
// with. Callers hardcoded CodeProtocol before AIRA-58; that is wrong for a
// wait-ceiling refusal, which the runner only treats as terminal when it arrives
// as CodeAdmitWaitTooLong. Anything without an explicit code stays CodeProtocol.
type admitCodedError struct {
	code string
	err  error
}

func (e admitCodedError) Error() string { return e.err.Error() }

func (e admitCodedError) Unwrap() error { return e.err }

func admitErrorCode(err error) string {
	var coded admitCodedError
	if errors.As(err, &coded) {
		return coded.code
	}
	return CodeProtocol
}

func validateAdmitArgs(args map[string]any, waitCeilingMs int64) (admitRequest, error) {
	// AIRA-185 widened the count to 13 and added `reason` to the allowlist below.
	// S5 widened it to 14 and added `cpu`. All are ADDITIVE: no existing field
	// changed meaning, and (bar cpu, the second ledger resource) no admission, gate
	// or emptiness decision reads the new ones.
	if len(args) < 3 || len(args) > 14 {
		return admitRequest{}, fmt.Errorf("%s: admit requires slice, reserve, optional max_wait_ms/cpu/signature/pinned/delegate_ram/exclusive/exclusive_holder/parent_scope_id/reason, and an optional complete scope_id/name/owner tuple", CodeProtocol)
	}
	for name := range args {
		if name != "slice" && name != "reserve" && name != "cpu" && name != "max_wait_ms" && name != "signature" && name != "pinned" && name != "delegate_ram" && name != "scope_id" && name != "name" && name != "owner" && name != "exclusive" && name != "exclusive_holder" && name != "parent_scope_id" && name != "reason" {
			return admitRequest{}, fmt.Errorf("%s: unexpected admit field %q", CodeProtocol, name)
		}
	}
	slice, ok := args["slice"].(string)
	slice = strings.TrimSpace(slice)
	if !ok || slice == "" {
		return admitRequest{}, fmt.Errorf("%s: admit slice must be a non-empty string", CodeProtocol)
	}
	reserve, ok := exactAdmitInt64(args["reserve"])
	if !ok || reserve < 0 || reserve > admitMaxReserve {
		return admitRequest{}, fmt.Errorf("%s: admit reserve must be in [0,%d]", CodeProtocol, admitMaxReserve)
	}
	// S5. cpu is optional (absent → 0 cores, charged nothing). Only STRUCTURAL
	// validation here — a non-integer or negative value is malformed. The
	// machine-specific "impossible on this box" refusal (cpu > 2×NumCPU) is
	// fail-fast in admitConnection, which has the core count; done there, exactly as
	// reserve's RANGE is checked here but its CEILING is checked in admitConnection.
	cpu := int64(0)
	if raw, exists := args["cpu"]; exists {
		cpu, ok = exactAdmitInt64(raw)
		if !ok || cpu < 0 {
			return admitRequest{}, fmt.Errorf("%s: admit cpu must be a non-negative integer", CodeProtocol)
		}
	}
	// S13: max_wait_ms is OPTIONAL. The confine client no longer sends it (a launch
	// blocks until granted or reconnects on a daemon restart, never self-expiring —
	// design §4/§6). Present-and-zero selects NON-BLOCKING mode. Absent → a blocking
	// wait with no timeout.
	maxWait := int64(0)
	nonBlocking := false
	if raw, exists := args["max_wait_ms"]; exists {
		parsed, ok := exactAdmitInt64(raw)
		if !ok {
			return admitRequest{}, fmt.Errorf("%s: admit max_wait_ms must be an integer", CodeProtocol)
		}
		if parsed < 0 {
			parsed = 0
		}
		nonBlocking = parsed == 0
		maxWait = parsed
	}
	// AIRA-58: REFUSE, never silently substitute. The old behaviour clamped to a
	// hardcoded 30 minutes with no error, no warning, and no field in
	// AdmitResponse in which an effective value could have been reported — and
	// because the response is only written at GRANT time, a clamped caller could
	// not have learned the truth until after waiting the wrong duration. Refusing
	// here tells them synchronously, before anything is enqueued.
	if maxWait > waitCeilingMs {
		return admitRequest{}, admitCodedError{
			code: CodeAdmitWaitTooLong,
			err: fmt.Errorf("%s: admit max_wait_ms %d exceeds the ceiling of %d ms (%s)",
				CodeAdmitWaitTooLong, maxWait, waitCeilingMs, time.Duration(waitCeilingMs)*time.Millisecond),
		}
	}
	signature := ""
	if raw, exists := args["signature"]; exists {
		var valid bool
		signature, valid = raw.(string)
		if !valid {
			return admitRequest{}, fmt.Errorf("%s: admit signature must be a string", CodeProtocol)
		}
	}
	pinned := false
	if raw, exists := args["pinned"]; exists {
		var valid bool
		pinned, valid = raw.(bool)
		if !valid {
			return admitRequest{}, fmt.Errorf("%s: admit pinned must be boolean", CodeProtocol)
		}
	}
	delegateRAM := false
	if raw, exists := args["delegate_ram"]; exists {
		var valid bool
		delegateRAM, valid = raw.(bool)
		if !valid {
			return admitRequest{}, fmt.Errorf("%s: admit delegate_ram must be boolean", CodeProtocol)
		}
	}
	// AIRA-101.
	exclusive := false
	if raw, exists := args["exclusive"]; exists {
		var valid bool
		exclusive, valid = raw.(bool)
		if !valid {
			return admitRequest{}, fmt.Errorf("%s: admit exclusive must be boolean", CodeProtocol)
		}
	}
	// AIRA-185. A free-text label for WHY this slice is being held, which the
	// identity fields cannot carry: `name` must be a valid confine identity (no
	// spaces, no colons) and must match the scope id, so "deploy: slice-ceiling
	// flip" satisfies neither constraint.
	//
	// DIAGNOSTIC ONLY. It is retained on the waiter, reported in the snapshot, and
	// read by nothing that decides anything — not sliceProvablyEmpty, not the
	// exclusive gate, not the reaper — so a malformed or hostile value can only
	// change what an operator is shown. Bounded here for the same availability
	// reason `signature` is (see boundedAdmitSignature).
	exclusiveReason := ""
	if raw, exists := args["reason"]; exists {
		text, valid := raw.(string)
		if !valid {
			return admitRequest{}, fmt.Errorf("%s: admit reason must be a string", CodeProtocol)
		}
		exclusiveReason = boundedAdmitReason(text)
	}
	exclusiveHolder := ""
	if raw, exists := args["exclusive_holder"]; exists {
		text, valid := raw.(string)
		if !valid {
			return admitRequest{}, fmt.Errorf("%s: admit exclusive_holder must be a string", CodeProtocol)
		}
		exclusiveHolder = strings.TrimSpace(text)
		// ONE scope-id grammar and ONE parser. The value is only ever compared for
		// equality, so a malformed one would be harmless in practice — but this
		// codebase already had a second, looser acceptance path admit ids the
		// scanner could then not see, and the fix was to stop having two.
		if exclusiveHolder != "" {
			if _, _, _, _, parsed := runner.ParseConfineScopeID(exclusiveHolder); !parsed {
				return admitRequest{}, fmt.Errorf("%s: admit exclusive_holder is not a canonical scope id", CodeProtocol)
			}
		}
	}
	parentScopeID := ""
	if raw, exists := args["parent_scope_id"]; exists {
		text, valid := raw.(string)
		if !valid {
			return admitRequest{}, fmt.Errorf("%s: admit parent_scope_id must be a string", CodeProtocol)
		}
		parentScopeID = strings.TrimSpace(text)
		if parentScopeID != "" {
			if _, _, _, _, parsed := runner.ParseConfineScopeID(parentScopeID); !parsed {
				return admitRequest{}, fmt.Errorf("%s: admit parent_scope_id is not a canonical scope id", CodeProtocol)
			}
		}
	}
	// A SCOPED request is job-level work by definition, so it may not also declare
	// itself somebody's sub-reservation: that combination would let a hand-crafted
	// request claim the drain exemption while being exactly the new job-level work
	// a drain exists to hold back. The blast radius is only a degraded
	// measurement, like the holder token, but the refusal is one line.
	if parentScopeID != "" {
		if _, hasScope := args["scope_id"]; hasScope {
			return admitRequest{}, fmt.Errorf("%s: admit parent_scope_id is for sub-reservations and cannot accompany scope_id", CodeProtocol)
		}
	}
	// AIRA-185. `reason` is reported ONLY through the exclusive state, so on a
	// non-exclusive request there is nowhere to attribute it and nothing would
	// ever render it. REFUSED rather than accepted-and-discarded, on the AIRA-82
	// discipline: a field the caller asked for and silently did not get is the
	// confidently-wrong reporting this codebase exists to refuse. Every real
	// producer (`aira drain wait`) always sets exclusive, and the runner drops a
	// stray reason before it reaches the wire, so this refusal is reachable only
	// by a hand-crafted request.
	if exclusiveReason != "" && !exclusive {
		return admitRequest{}, fmt.Errorf("%s: admit reason describes an exclusive hold and requires exclusive", CodeProtocol)
	}
	if exclusive {
		// A nested exclusive inside a hold could never satisfy the emptiness rule
		// (its own holder keeps outstandingJobs >= 1), so it would sit blocked until
		// its ceiling. Refuse synchronously instead of accepting a request that
		// cannot succeed.
		if exclusiveHolder != "" {
			return admitRequest{}, fmt.Errorf("%s: admit exclusive cannot be combined with exclusive_holder", CodeProtocol)
		}
		if parentScopeID != "" {
			return admitRequest{}, fmt.Errorf("%s: admit exclusive cannot be combined with parent_scope_id", CodeProtocol)
		}
		// An exclusive request gets its own, much lower ceiling than the shared one:
		// it holds up every other session on this machine while it drains. REFUSED,
		// never clamped (AIRA-58), and with the terminal code the client already
		// handles before the structured-payload branch, so it can never degrade into
		// the unaccounted flock fallback.
		if exclusiveCeiling := admitExclusiveWaitCeiling(); maxWait > exclusiveCeiling.Milliseconds() {
			return admitRequest{}, admitCodedError{
				code: CodeAdmitWaitTooLong,
				err: fmt.Errorf("%s: admit max_wait_ms %d exceeds the exclusive-request ceiling of %d ms (%s)",
					CodeAdmitWaitTooLong, maxWait, exclusiveCeiling.Milliseconds(), exclusiveCeiling),
			}
		}
	}
	scopeID, hasScope := args["scope_id"]
	name, hasName := args["name"]
	owner, hasOwner := args["owner"]
	if hasScope || hasName || hasOwner {
		if !hasScope || !hasName || !hasOwner {
			return admitRequest{}, fmt.Errorf("%s: admit scope_id, name, and owner must be supplied together", CodeProtocol)
		}
		scopeText, scopeOK := scopeID.(string)
		nameText, nameOK := name.(string)
		ownerText, ownerOK := owner.(string)
		// ONE parser, runner's own, rather than a regex restating the grammar
		// beside it: the two drifted apart (build-review, Sol) and a scope id the
		// regex admitted but the scanner's parser rejected was admitted and then
		// invisible to every scan and reaper.
		embeddedName, _, _, embeddedOwner, parsed := runner.ParseConfineScopeID(scopeText)
		if !scopeOK || !parsed {
			return admitRequest{}, fmt.Errorf("%s: admit scope_id is not canonical", CodeProtocol)
		}
		if !nameOK || runner.ValidateConfineIdentity(nameText) != nil {
			return admitRequest{}, fmt.Errorf("%s: admit name is invalid", CodeProtocol)
		}
		if embeddedName != nameText {
			return admitRequest{}, fmt.Errorf("%s: admit name does not match scope_id", CodeProtocol)
		}
		if !ownerOK || runner.ValidateConfineOwner(ownerText) != nil {
			return admitRequest{}, fmt.Errorf("%s: admit owner is invalid", CodeProtocol)
		}
		// BIND a persisted owner to the claimed one (AIRA-52 hardening,
		// build-review, Sol). The scope id is the durable ownership record, so an
		// unbound pair would let a client persist one owner while the daemon
		// accounted another — "scope_id=...@victim" with "owner=me" — and after a
		// restart the scan would read victim as an ATTESTED owner nobody claimed.
		//
		// ASYMMETRIC on purpose. A tail that DISAGREES with the claim is
		// impersonation and is refused. A MISSING tail is not: it means the client
		// persisted no claim at all, which is exactly the pre-AIRA-52 behaviour —
		// the daemon accounts the owner in memory and the job reads as unowned
		// after a restart, so the kill guard demands --steal. Refusing that case
		// too would buy no safety and would hard-break every session whose
		// installed binary predates this change the moment the daemon restarts,
		// with no protocol-version bump to signal it.
		expectedOwner := ownerText
		if expectedOwner == runner.ConfineUnknownOwner {
			expectedOwner = ""
		}
		if embeddedOwner != "" && embeddedOwner != expectedOwner {
			return admitRequest{}, fmt.Errorf("%s: admit owner does not match scope_id", CodeProtocol)
		}
		return admitRequest{slice: slice, reserve: reserve, cpu: cpu, maxWait: maxWait, nonBlocking: nonBlocking, signature: signature, pinned: pinned, delegateRAM: delegateRAM, scopeID: scopeText, name: nameText, owner: ownerText, exclusive: exclusive, exclusiveReason: exclusiveReason, exclusiveHolder: exclusiveHolder, parentScopeID: parentScopeID}, nil
	}
	// An exclusive request MUST carry the scope tuple. Exclusivity is attributed
	// to, reported by, and reaped through the holder's scope id: a scope-less
	// exclusive could never be named in `confine --list`, never matched by a
	// nested job's holder token, and never reclaimed by the stale-lease sweep.
	if exclusive {
		return admitRequest{}, fmt.Errorf("%s: admit exclusive requires the scope_id, name and owner tuple", CodeProtocol)
	}
	// exclusiveReason is provably empty on this path (it requires exclusive, and
	// exclusive requires the tuple refused just above), and it is transcribed
	// anyway so that relaxing either rule later cannot silently drop the field
	// instead of failing a test.
	return admitRequest{slice: slice, reserve: reserve, cpu: cpu, maxWait: maxWait, nonBlocking: nonBlocking, signature: signature, pinned: pinned, delegateRAM: delegateRAM, exclusiveReason: exclusiveReason, exclusiveHolder: exclusiveHolder, parentScopeID: parentScopeID}, nil
}

func exactAdmitInt64(value any) (int64, bool) {
	switch value := value.(type) {
	case int:
		return int64(value), true
	case int64:
		return value, true
	case float64:
		if value < math.MinInt64 || value > math.MaxInt64 || value != math.Trunc(value) {
			return 0, false
		}
		return int64(value), true
	case json.Number:
		parsed, err := value.Int64()
		return parsed, err == nil
	case string:
		parsed, err := strconv.ParseInt(value, 10, 64)
		return parsed, err == nil
	default:
		return 0, false
	}
}

// readSliceMemory is the REAL path's reading: a slice whose memory.max is `max`
// has no ceiling to book against, so it is refused as unevaluated. Unchanged
// behaviour, now expressed as the refusal it is on top of the usage read.
func readSliceMemory(path string) (cur, max, reclaimable int64, ok bool, reason string) {
	current, limit, reclaim, ok, reason := readSliceMemoryUsage(path)
	if !ok {
		return 0, 0, 0, false, reason
	}
	if limit <= 0 {
		return 0, 0, 0, false, "unbounded"
	}
	return current, limit, reclaim, true, ""
}

// readSliceMemoryUsage reads a cgroup's LIVE usage and, separately, whatever
// limit it declares — reporting limit==0 for `max` rather than refusing.
//
// The split exists because the two callers legitimately disagree about what an
// unbounded memory.max means (AIRA-121 F1). On the real path a slice with no
// limit has no ceiling and admission must refuse. In ci-shim mode the ceiling is
// the RECORDED BUDGET, not the cgroup's own limit, so an unbounded memory.max is
// not a refusal at all — it is precisely the multi-container-per-node case
// --memory-max exists to serve (GCP Batch with taskCountPerNode > 1), where the
// container's own cgroup still has a perfectly real memory.current to book
// against. Refusing there sent readShimMemory to host-wide meminfo, whose
// MemTotal-MemAvailable is not namespaced, and every job in the container then
// answered E_ADMIT_TOO_LARGE for its whole life.
func readSliceMemoryUsage(path string) (cur, max, reclaimable int64, ok bool, reason string) {
	currentData, err := os.ReadFile(filepath.Join(path, "memory.current"))
	if err != nil {
		return 0, 0, 0, false, "read-error"
	}
	maxData, err := os.ReadFile(filepath.Join(path, "memory.max"))
	if err != nil {
		return 0, 0, 0, false, "read-error"
	}
	current, valid := parseAdmitMemory(currentData)
	if !valid {
		return 0, 0, 0, false, "parse-error"
	}
	var limit int64
	if strings.TrimSpace(string(maxData)) != "max" {
		limit, valid = parseAdmitMemory(maxData)
		if !valid {
			return 0, 0, 0, false, "parse-error"
		}
	}
	// current and limit are already guaranteed >= 0 by parseAdmitMemory (valid
	// implies non-negative), and checkedAvailable independently guards current<0
	// before the reclaimable discount — so no further negative check is needed here.
	statData, err := os.ReadFile(filepath.Join(path, "memory.stat"))
	if err == nil {
		// The slab figure is deliberately DISCARDED here: this is AIRA-21's
		// reclaimable page-cache discount and its meaning must not change.
		// AIRA-103's ceiling signal is the only consumer of slab_reclaimable.
		reclaimable, _, valid = parseSliceMemoryStat(statData)
	}
	if err != nil || !valid {
		sliceMemoryStatDegradeOnce.Do(func() {
			log.Printf("aira daemon: slice memory.stat unavailable or incomplete; using raw memory.current")
		})
		reclaimable = 0
	}
	return current, limit, reclaimable, true, ""
}

// readSliceMemoryHigh reads a cgroup's memory.high SOFT limit for AIRA-127's
// operator surface. It is a REPORTING read and nothing else: no admission
// decision consults memory.high, which is reclaim pressure rather than a bound.
//
// It returns three distinguishable outcomes and never collapses them, because
// they call for different words on the bar. A parsed number is "set". The
// literal `max` is "none" — a POSITIVE fact that no soft limit is configured,
// which is the normal state of an aira.slice installed without --memory-high.
// An unreadable or unparsable file is "unevaluated", and its zero must never be
// drawn as a limit sitting at the bar's origin.
func readSliceMemoryHigh(path string) (int64, string) {
	data, err := os.ReadFile(filepath.Join(path, "memory.high"))
	if err != nil {
		return 0, runner.ConfineSliceHighUnevaluated
	}
	text := strings.TrimSpace(string(data))
	if text == "max" {
		return 0, runner.ConfineSliceHighNone
	}
	value, valid := parseAdmitMemory([]byte(text))
	if !valid {
		return 0, runner.ConfineSliceHighUnevaluated
	}
	return value, runner.ConfineSliceHighSet
}

// parseSliceMemoryStat returns the file-LRU reclaimable total — AIRA-21's
// admission discount, whose meaning is unchanged — and, SEPARATELY,
// slab_reclaimable.
//
// The split is load-bearing rather than tidy. AIRA-103's ceiling signal must
// subtract slab as well (MemAvailable credits most of GLOBAL reclaimable slab,
// so leaving the slice's share inside its "non-reclaimable footprint"
// double-counts it, permissively), while admission's own discount must keep
// counting exactly what it counted before. Returning slab as a third value
// rather than folding it into the first keeps both true without a second parser
// over the same file. An absent slab_reclaimable line reports zero rather than a
// failed parse: it is optional for the signal and required by neither caller.
func parseSliceMemoryStat(data []byte) (reclaimable, slabReclaimable int64, ok bool) {
	var inactiveFile, activeFile int64
	var inactiveFound, activeFound bool
	for _, line := range strings.Split(string(data), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		value, err := strconv.ParseInt(fields[1], 10, 64)
		if err != nil || value < 0 {
			continue
		}
		switch fields[0] {
		case "inactive_file":
			inactiveFile, inactiveFound = value, true
		case "active_file":
			activeFile, activeFound = value, true
		case "slab_reclaimable":
			slabReclaimable = value
		}
	}
	if !inactiveFound || !activeFound {
		return 0, 0, false
	}
	return addClamp(inactiveFile, activeFile), slabReclaimable, true
}

func parseAdmitMemory(data []byte) (int64, bool) {
	text := strings.TrimSpace(string(data))
	if text == "" || len(strings.Fields(text)) != 1 {
		return 0, false
	}
	value, err := strconv.ParseInt(text, 10, 64)
	return value, err == nil && value >= 0
}

func resolveAdmitSlicePath(slice string) (string, bool, string) {
	mount, err := admitUnifiedMount()
	if err != nil {
		return "", false, "slice-not-found"
	}
	current, err := admitCurrentCgroupPath(mount)
	if err != nil {
		return "", false, "slice-not-found"
	}
	return resolveAdmitSlicePathAt(slice, mount, current)
}

func resolveAdmitSlicePathAt(slice, mount, current string) (string, bool, string) {
	slice = strings.TrimSpace(slice)
	if slice == "" || admitHasParentComponent(slice) {
		return "", false, "slice-not-found"
	}
	mountAbs, err := filepath.Abs(mount)
	if err != nil {
		return "", false, "slice-not-found"
	}
	mountCanonical, err := filepath.EvalSymlinks(mountAbs)
	if err != nil {
		return "", false, "slice-not-found"
	}
	var candidates []string
	if filepath.IsAbs(slice) {
		candidates = []string{slice}
	} else if !strings.ContainsRune(slice, filepath.Separator) && strings.HasSuffix(slice, ".slice") {
		for cursor := filepath.Clean(current); admitPathWithin(mountCanonical, cursor); cursor = filepath.Dir(cursor) {
			if filepath.Base(cursor) == slice {
				candidates = append(candidates, cursor)
			}
			candidates = append(candidates, filepath.Join(cursor, slice))
			if filepath.Clean(cursor) == filepath.Clean(mountCanonical) {
				break
			}
		}
	} else {
		candidates = []string{filepath.Join(mountCanonical, slice)}
	}
	for _, candidate := range candidates {
		candidateAbs, absErr := filepath.Abs(candidate)
		if absErr != nil || !admitPathWithin(mountCanonical, candidateAbs) {
			continue
		}
		canonical, evalErr := filepath.EvalSymlinks(candidateAbs)
		if evalErr != nil || !admitPathWithin(mountCanonical, canonical) {
			continue
		}
		if stat, statErr := os.Stat(canonical); statErr == nil && stat.IsDir() {
			return canonical, true, ""
		}
	}
	return "", false, "slice-not-found"
}

func admitUnifiedMount() (string, error) {
	file, err := os.Open("/proc/self/mountinfo")
	if err != nil {
		return "", err
	}
	defer file.Close()
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		parts := strings.SplitN(scanner.Text(), " - ", 2)
		if len(parts) != 2 {
			continue
		}
		post, pre := strings.Fields(parts[1]), strings.Fields(parts[0])
		if len(post) < 1 || len(pre) < 5 || post[0] != "cgroup2" {
			continue
		}
		mount := pre[4]
		for _, item := range []struct{ from, to string }{{"\\040", " "}, {"\\011", "\t"}, {"\\134", "\\"}} {
			mount = strings.ReplaceAll(mount, item.from, item.to)
		}
		return mount, nil
	}
	if err := scanner.Err(); err != nil {
		return "", err
	}
	return "", errors.New("cgroup-v2 unified mount not found")
}

func admitCurrentCgroupPath(mount string) (string, error) {
	data, err := os.ReadFile("/proc/self/cgroup")
	if err != nil {
		return "", err
	}
	for _, line := range strings.Split(string(data), "\n") {
		if strings.HasPrefix(line, "0::") {
			return filepath.Join(mount, strings.TrimPrefix(strings.TrimPrefix(line, "0::"), "/")), nil
		}
	}
	return "", errors.New("unified cgroup membership not found")
}

func admitHasParentComponent(path string) bool {
	for _, component := range strings.FieldsFunc(filepath.ToSlash(path), func(r rune) bool { return r == '/' }) {
		if component == ".." {
			return true
		}
	}
	return false
}

func admitPathWithin(root, path string) bool {
	rel, err := filepath.Rel(filepath.Clean(root), filepath.Clean(path))
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}
