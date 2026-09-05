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
	// workerAdmitWaitCeilingMs deliberately stays at 30 minutes rather than
	// adopting the shared ceiling. AIRA-63 has now given workerAdmitConnection
	// the same admitSlots bound admitConnection has, so the ORIGINAL reason for
	// the split ("worker-admit is not gated at all, and a 24h ceiling would
	// permit unbounded concurrent retained connections") no longer holds — but
	// unifying the two ceilings is deliberately NOT part of that fix. Raising
	// worker-admit's ceiling 48x changes how long a saturated aitest run may sit
	// holding shared admission slots that ordinary `aira confine` admission also
	// draws from, which needs its own sizing decision rather than riding along
	// with a ledger change. Its only caller, the aitest supervisor, uses waits
	// two orders of magnitude smaller either way. Revisit deliberately, in its
	// own change, not as a "consistency" refactor.
	workerAdmitWaitCeilingMs            int64 = 30 * 60 * 1000
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
	delegateRAMAdoptionMargin           int64 = 64 << 20

	// admitExclusiveWaitCeilingDefault bounds how long an EXCLUSIVE request may
	// drain the slice. It is deliberately far below the shared 24-hour
	// AdmitWaitCeiling: an exclusive request holds up every other session on this
	// machine while it drains, so a day-long drain is not a wait, it is an outage.
	// Enforced by REFUSAL, never silent substitution (the AIRA-58 rule).
	admitExclusiveWaitCeilingDefault = 30 * time.Minute

	// admitExclusiveEstablishGrace is how long the confine scan may be failing
	// before a draining exclusive waiter is ABORTED rather than left holding the
	// slice. See sliceQueue.scanFailingSince.
	admitExclusiveEstablishGrace = 30 * time.Second
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

// admitOutcomeExclusiveUnestablished is the waiter outcome the unestablished-
// emptiness abort records. admitConnection branches on it so the abort reaches
// the client as its own code rather than as E_ADMIT_SATURATED — reporting "the
// slice was busy" for what is actually "the daemon could not read the slice"
// would be a fabricated diagnosis of exactly the kind the fail-closed rule exists
// to prevent.
const admitOutcomeExclusiveUnestablished = "exclusive-unestablished"

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
	seq          int64
	reserve      int64
	state        admitWaiterState
	grantedCh    chan struct{}
	enqueued     time.Time
	grantedAt    time.Time
	waited       bool
	accounted    bool
	outcome      string
	reason       string
	waitedMS     int64
	basis        string
	scopeID      string
	name         string
	owner        string
	scopeCeiling int64

	// AIRA-68. scopeSeen/scopeVanished record a TRANSITION observed by the
	// evaluator's own <=1s confine scan — the same authority the adopted ledger
	// already trusts — and are meaningful only for a scope-backed waiter
	// (scopeID != ""). Both are written ONLY inside evaluateAdmitQueue's
	// scan-success block, under queue.mu, and never on a failed scan.
	//
	// The pair exists because plain ABSENCE is not a safe reclaim signal and the
	// transition is. The pre-existing empty-scope reclaim is safe against a
	// launcher stalled before scope creation only because it is DESTRUCTIVE: it
	// removes the directory, so the launcher's next cgroupfs write fails
	// ENOENT/ENODEV and the launch aborts cleanly. Reclaiming on absence alone
	// has no such fence — the stalled launcher would lose its lease, then create
	// its scope and run entirely UNCHARGED, which is the #67 aggregate-OOM class.
	// A waiter that never created a scope never gets scopeSeen, so it is never a
	// candidate, and its treatment is unchanged.
	//
	// scopeVanished is deliberately CLEARED when the scope is observed again: a
	// scan is a fresh fact, not a latch, and a stale "vanished" must never
	// outlive the observation that produced it.
	//
	// Honest limits, all pre-existing and all in the safe direction:
	//   - A scope created and removed entirely between two scans never sets
	//     scopeSeen, so a lease stuck that way stays stuck (the empty-reap branch
	//     has the identical blind spot: it needs a directory to reap).
	//   - A scope id accepted by confineScopeIDPattern but rejected by the
	//     scanner's own parseConfineScopeID is omitted from every scan, so
	//     scopeSeen never becomes true. Never a false reclaim.
	//   - "Seen then gone" proves the scope held no processes at removal time. It
	//     does NOT prove the job is dead: a leader can migrate into a sibling
	//     cgroup and keep running (internal/runner/descendant_escape_linux_test.go).
	//     That is exactly the strength of the empty-reap branch's own proof, and
	//     why the reported counter is named `vanished`, never `ghost`.
	//   - Strictly, the scan observes a PATHNAME, and cgroup v2 permits renaming a
	//     cgroup within its parent — so an absent scope id means "no cgroup by
	//     that name", not unconditionally "that cgroup was removed". A renamed,
	//     still-populated scope would therefore be read as vanished and its lease
	//     reclaimed after the TTL while the job runs on, still contained. Both
	//     plan reviewers raised this independently and it is recorded, not fixed:
	//     nothing in AIRA renames a scope, closing it needs per-scope inode
	//     identity threaded through the scan (real machinery for an
	//     externally-injected scenario, which architectural-simplicity says to
	//     document rather than build), and the consequence is bounded the same way
	//     the migrated-leader case is — the release is LEDGER-ONLY, and a renamed
	//     cgroup is still inside the slice, so its memory is still charged through
	//     max(current - reclaimable, sum of reserves). Requiring a currently
	//     succeeding scan (see dischargeVanishedStaleLease) narrows the window but
	//     does not close it.
	scopeSeen     bool
	scopeVanished bool

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

// sliceProvablyEmpty reports whether the daemon can POSITIVELY establish that
// nothing else is admitted or running in this slice. queue.mu must be held.
//
// Fail-closed in all three terms, and the third is the important one: a scan the
// daemon could not complete leaves liveScopesKnown false, and an unestablished
// emptiness must never be rendered as an empty slice. Telling a benchmark "you
// are alone" on a reading nobody has is the fabricated pass this codebase
// forbids everywhere else.
//
// KNOWN COVERAGE LIMITS of the emptiness reading, stated rather than discovered
// later. Each is fail-OPEN for the measurement and fail-SAFE for the machine —
// they can let an exclusive job be granted beside something, never wedge the
// slice:
//
//   - `aira run` scopes are named .aira-RUN-* under a project's own cgroup
//     parent, not .aira-CONFINE-* under this slice, so the confine scan does not
//     see them. While such a run holds its admission connection it is still
//     counted by outstandingJobs; only after a daemon restart does it become
//     invisible here.
//   - Anything not admitted through AIRA at all — a process placed into the slice
//     by hand — is outside this reading by construction, and so is a Docker
//     container, which lives under /system.slice/docker-<id>.scope entirely
//     outside this slice. `--exclusive`'s own help text says so; exclusivity must
//     never be read as a claim about those.
//
// outstandingJobs is strict, with NO discount for exempt sub-reservations. A
// discount would be unnecessary — a running job's own scoped lease already keeps
// the count at 1 or more, and a post-restart adopted parent is caught by
// subtree-aware liveScopes — and it would remove a belt-and-braces signal in the
// one case where it is the only thing still objecting: a live reservation whose
// parent job has escaped its scope or died with its socket held open.
func sliceProvablyEmpty(queue *sliceQueue) bool {
	return queue.outstandingJobs == 0 && queue.liveScopesKnown && queue.liveScopes == 0
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

// exclusiveDeniesWorkerAdmit reports whether a slice-exclusive HOLD forbids
// placing a worker under this outer scope (AIRA-101).
//
// A DRAIN deliberately does NOT deny. A worker is an already-running job's
// internal progress, not new work entering the slice, and denying it would stop
// running suites from finishing — which is exactly what the drain is waiting
// for, so blocking here would prevent the drain from ever converging. It is also
// structurally safe to allow: CreateWorkerScope only ever creates
// `.aira-worker-*` CHILDREN inside an already-existing outer scope, so
// worker-admit cannot introduce new top-level work into a draining slice however
// it is answered.
//
// A HOLD denies every outer scope that is not the holder's own work — the holder
// itself, or a nested `aira confine` launched from inside it carrying its token.
// By construction the slice is empty of other jobs when a hold begins, so this
// denies nothing that should exist: it is belt-and-braces enforcement of the
// invariant, not a load-bearing path.
//
// Lock order is the established admitRegistryMu -> queue.mu. The caller must
// hold neither the outer-scope lock nor the CPU-slots gate, so this adds no new
// nesting.
func (s *Server) exclusiveDeniesWorkerAdmit(outerScope string) bool {
	outerScope = strings.TrimSpace(outerScope)
	if outerScope == "" {
		return false
	}
	// The queue is keyed by resolveAdmitSlicePath's EvalSymlinks'd path, while
	// worker-admit only Cleans its outer_scope. Canonicalise the parent the same
	// way, or the lookup silently misses and this gate ships INERT — the failure
	// mode this project has shipped once already. Falling back to the cleaned path
	// keeps a scope removed mid-request working instead of turning a lookup miss
	// into an error.
	slicePath := filepath.Dir(filepath.Clean(outerScope))
	if resolved, err := filepath.EvalSymlinks(slicePath); err == nil {
		slicePath = resolved
	}
	scopeDir := filepath.Base(filepath.Clean(outerScope))

	s.admitRegistryMu.Lock()
	queue := s.admitQueues[slicePath]
	if queue == nil {
		s.admitRegistryMu.Unlock()
		return false
	}
	queue.mu.Lock()
	defer s.admitRegistryMu.Unlock()
	defer queue.mu.Unlock()
	gate := exclusiveGateLocked(queue)
	if gate.holder == nil {
		return false
	}
	// A scope's directory is ".aira-" + its scope id. Compare on that exact
	// mapping rather than a substring, so a scope whose name merely contains
	// another's id can never be mistaken for it.
	for scopeID := range gate.holderScopeIDs(queue) {
		if scopeDir == confineScopeDirName(scopeID) {
			return false
		}
	}
	return true
}

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
	mu                sync.Mutex
	path              string
	waiters           []*admitWaiter
	outstanding       int64
	outstandingJobs   int
	adopted           int64
	adoptedJobs       int
	adoptedAt         time.Time
	adoptedScanFailed bool
	seq               int64
	kick              chan struct{}
	stop              chan struct{}
	stopOnce          sync.Once
	// stopped is closed by runEvaluator as it exits, so a caller can establish
	// that the goroutine is GONE rather than merely asked to stop.
	//
	// It exists for the tests, and the reason is a real invariant rather than
	// convenience: evaluateAdmitQueue reads the scan throttle BEFORE taking
	// queue.mu, which is sound only under the single-writer property documented
	// there — in production this queue's own goroutine is the sole caller. A test
	// that drove passes directly while that goroutine was still live would break
	// the invariant and race, so it must be able to retire it and know when.
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

	// AIRA-101. liveScopes is the EMPTINESS reading, deliberately separate from
	// adopted/adoptedJobs, which are a RESERVE reading. adoptedJobs skips
	// non-finite-cap scopes and connection-held scopes on purpose — both correct
	// for reserve accounting and both wrong here, because a skipped scope is still
	// a running job. Reusing it would let an exclusive job be told it is alone
	// while a delegate-ram suite runs beside it.
	//
	// liveScopesKnown is the fail-closed half: it is true only when the scan that
	// produced liveScopes SUCCEEDED. Granting exclusivity on an unestablished
	// emptiness would state "you are alone" on a reading the daemon does not have,
	// which is the fabricated pass this codebase forbids everywhere else.
	liveScopes      int
	liveScopesKnown bool

	// scanFailingSince anchors how long the confine scan has been failing, in the
	// same derive-from-one-anchor shape as freezeArmedAt. It exists so a drain can
	// ABORT rather than stall the whole shared slice: with the fail-closed rule
	// above, a persistently unreadable slice would otherwise block the drain head
	// (cannot establish emptiness) AND every other waiter (blocked by the drain)
	// for the full wait ceiling — a machine-wide outage caused by a diagnostic
	// failure.
	//
	// Armed on the FIRST failure while zero and never renewed on later failures;
	// cleared on any success. Arming only "after a success" would never fire in
	// this rule's own primary case — a slice unreadable from the queue's very
	// first pass, which is the likeliest persistent failure, since a queue is
	// created on demand and its first scan is its first contact with the path.
	// Never renewing is the freezeArmedAt lesson: a renewed anchor postpones its
	// own deadline forever.
	scanFailingSince time.Time
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
	State        string `json:"state"`
	Reason       string `json:"reason,omitempty"`
	WaitedMS     int64  `json:"waited_ms"`
	Reserve      int64  `json:"reserve"`
	Basis        string `json:"basis"`
	ScopeCeiling int64  `json:"scope_ceiling,omitempty"`
}

type admitRequest struct {
	slice           string
	reserve         int64
	maxWait         int64
	signature       string
	pinned          bool
	scopeID         string
	name            string
	owner           string
	delegateRAM     bool
	exclusive       bool
	exclusiveHolder string
	parentScopeID   string
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

func (s *Server) admitOutstandingReserve(path string) (outstanding int64, outstandingJobs int, adopted int64, adoptedJobs int, ok bool) {
	snapshot := s.admitSliceSnapshot(path)
	return snapshot.outstanding, snapshot.outstandingJobs, snapshot.adopted, snapshot.adoptedJobs, snapshot.present
}

// admitSliceSnapshot reads the ledger AND the queue diagnostics in ONE locked
// pass. Taking them in two rounds would let `confine --list` report a granted
// total and a queued count from different moments — a self-inconsistent picture
// in exactly the situation an operator reaches for it.
type admitSnapshot struct {
	outstanding     int64
	outstandingJobs int
	adopted         int64
	adoptedJobs     int
	queued          int
	phase           string
	present         bool

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

	// AIRA-68. outstandingJobs fuses TWO structurally different populations, and
	// the reported job total adds a third — while `confine --list`'s table above
	// the summary lists only SCOPES:
	//
	//   scopeJobs        connection-held `aira confine` jobs   -> a table row
	//   reservationJobs  connection-held `aira confine-reserve` reservations,
	//                    which create no cgroup scope at all   -> NO table row
	//   adoptedJobs      scan-adopted scopes                   -> a table row
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

	// vanishedJobs/vanishedBytes are a SUBSET of scopeJobs/scopeBytes, never a
	// fourth population — the split must keep summing to the totals or the
	// residual below would cry wolf on every vanished lease.
	vanishedJobs  int
	vanishedBytes int64

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
	// exclusiveWaiting counts the queued waiters actually held up behind the
	// exclusive job. It excludes the exclusive waiter itself.
	exclusiveWaiting int
}

// residualJobs and residualBytes cross-check the DERIVED split (a walk of
// queue.waiters) against the INCREMENTAL counters. They are equal by
// construction: a waiter is `admitGranted && accounted` if and only if it was
// counted, and releaseAdmitWaiter discharges under exactly that guard. A
// non-zero residual is therefore a real lost or double decrement, not noise.
//
// adoptedJobs/adopted appear on both sides of the reported total and cancel, so
// these are stated over the connection-held ledger alone.
//
// The two are reported INDEPENDENTLY and SIGNED. The single most plausible
// regression in releaseAdmitWaiter — dropping `outstanding -= waiter.reserve`
// while keeping `outstandingJobs--` — is byte-only, and a job-only residual
// would report a perfectly consistent ledger while the slice silently filled.
// A negative residual (more discharged than was ever charged) is just as real a
// defect as a positive one and must never be floored away.
//
// What they do NOT detect: a stuck waiter that is consistently present in BOTH
// accountings. That is what vanishedJobs is for, for the population where an
// answer is physically possible.
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
		// An absent queue is a genuine idle zero, not an unevaluated read: a queue
		// exists only while it has waiters, so its absence positively establishes
		// that nothing is waiting. Callers must not render this as "unknown".
		return admitSnapshot{phase: phase}
	}
	queue.mu.Lock()
	snapshot := admitSnapshot{
		outstanding: queue.outstanding, outstandingJobs: queue.outstandingJobs,
		adopted: queue.adopted, adoptedJobs: queue.adoptedJobs,
		phase: phase, present: true,
	}
	queuedBytes := int64(0)
	for _, waiter := range queue.waiters {
		if waiter == nil {
			continue
		}
		if waiter.state == admitQueued {
			snapshot.queued++
			// AIRA-24. The position is an index among QUEUED waiters only, so it
			// is taken here and nowhere else: counting granted or released
			// waiters would report a place in a line that no longer exists. The
			// first match wins — enqueueAdmitInternal refuses a duplicate scope
			// id (CodeProtocol), so a second match is not reachable.
			if queuedScopeID != "" && snapshot.queuePosition == 0 && waiter.scopeID == queuedScopeID {
				snapshot.queuePosition = snapshot.queued
				snapshot.queuedAheadBytes = queuedBytes
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
		if waiter.scopeID == "" {
			snapshot.reservationJobs++
			snapshot.reservationBytes = addClamp(snapshot.reservationBytes, waiter.reserve)
			continue
		}
		snapshot.scopeJobs++
		snapshot.scopeBytes = addClamp(snapshot.scopeBytes, waiter.reserve)
		if waiter.scopeVanished {
			snapshot.vanishedJobs++
			snapshot.vanishedBytes = addClamp(snapshot.vanishedBytes, waiter.reserve)
		}
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
		// Only waiters actually held up behind it: the exclusive waiter is never
		// counted as waiting for itself.
		snapshot.exclusiveWaiting = subtractJobCount(snapshot.queued, 1)
		if exclusive.state != admitQueued {
			snapshot.exclusiveWaiting = snapshot.queued
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

// admitAvailable is the governor's advisory, read-only view of the same
// headroom calculation used by an immediate admission grant. It deliberately
// does not create a queue: a read-only governor lookup must not start an idle
// queue evaluator. The global lock order is governorSet.mu ->
// admitRegistryMu -> sliceQueue.mu; no admission path takes governorSet.mu.
func (s *Server) admitAvailable(slicePath string) (int64, bool) {
	var outstanding, adopted int64
	var outstandingJobs, adoptedJobs int
	s.admitRegistryMu.Lock()
	queue := s.admitQueues[slicePath]
	if queue != nil {
		queue.mu.Lock()
		outstanding, adopted = queue.outstanding, queue.adopted
		outstandingJobs, adoptedJobs = queue.outstandingJobs, queue.adoptedJobs
		queue.mu.Unlock()
	}
	s.admitRegistryMu.Unlock()

	readMemory := s.admitReadMemory
	if readMemory == nil {
		readMemory = readSliceMemory
	}
	current, maximum, reclaimable, ok, _ := readMemory(slicePath)
	if !ok {
		return 0, false
	}
	jobs := addJobCountClamp(addJobCountClamp(outstandingJobs, adoptedJobs), 1)
	headroom := s.admitSliceHeadroom(jobs)
	// AIRA-103. A CAPACITY question, so it takes the pressure-throttled maximum.
	// See admitEffectiveMaximum for the three sites this may appear at and the
	// four it must never reach.
	effective := s.admitEffectiveMaximum(slicePath, maximum)
	return checkedAvailable(current, effective, reclaimable, addClamp(outstanding, adopted), headroom), true
}

func (s *Server) admitCeiling(path string, maximum int64) int64 {
	return subtractFloor(maximum, s.admitSliceHeadroom(s.admitOutstandingJobs(path)+1))
}

func (s *Server) resolveAdmitReserve(request admitRequest, ceiling int64) (int64, string) {
	if request.pinned {
		return request.reserve, "pinned:client"
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
				basis := "fallback:insufficient-samples"
				ordinary := stats
				ordinary.OOMCount = 0
				if estimated, ok, estimatedBasis := runner.EstimateMemoryReserve(ordinary, 0); ok {
					reserve, basis = estimated, estimatedBasis
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
					if escalated > reserve {
						reserve = escalated
					}
					// An OOM observed at the present ceiling is genuinely too
					// large. Earlier censored caps are allowed to climb to the
					// ceiling so a runnable job is never permanently wedged.
					if stats.MaxOOMPeak < ceiling && reserve > ceiling {
						reserve = ceiling
					}
					return reserve, "estimate:oom-escalated"
				}
				if stats.SampleCount >= 3 && reserve > 0 {
					return reserve, basis
				}
			}
		}
	}
	if peak, ok := s.cachedAdmitPeakP90(readCtx); ok {
		stats := runner.PeakRSSStats{TotalCount: 3, SampleCount: 3, PeakMax: peak}
		if reserve, usable, _ := runner.EstimateMemoryReserve(stats, 0); usable {
			return reserve, "estimate:p90-prior"
		}
	}
	if request.signature == "" {
		return request.reserve, "fallback:no-signature"
	}
	if historyUnavailable {
		return request.reserve, "fallback:history-unavailable"
	}
	if insufficientSamples {
		return request.reserve, "fallback:insufficient-samples"
	}
	return request.reserve, "fallback:no-history"
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
	resolve := s.admitResolveSlice
	if resolve == nil {
		resolve = resolveAdmitSlicePath
	}
	path, ok, reason := resolve(request.slice)
	if !ok {
		s.writeAdmitGrant(conn, AdmitResponse{State: "unevaluated", Reason: reason})
		return
	}
	readMemory := s.admitReadMemory
	if readMemory == nil {
		readMemory = readSliceMemory
	}
	_, maximum, _, ok, reason := readMemory(path)
	if !ok {
		// The daemon answered; only the slice's live usage was unreadable. Report
		// that honestly (NOT "daemon-unavailable") so the operator-facing basis is
		// truthful. A non-delegate scope is left uncapped here (state != admitted);
		// a delegate-ram scope gets no daemon scope_ceiling and so falls back to the
		// finite client-side default (AIRA-15) — never uncapped.
		s.writeAdmitGrant(conn, AdmitResponse{State: "unevaluated", Reason: reason, Reserve: request.reserve, Basis: "fallback:slice-unreadable"})
		return
	}
	jobs := s.admitOutstandingJobs(path)
	headroom := s.admitSliceHeadroom(jobs + 1)
	ceiling := subtractFloor(maximum, headroom)
	scopeCeiling := int64(0)
	if request.delegateRAM {
		scopeCeiling = s.resolveDelegateRAMScopeCeiling(request, maximum, headroom)
	}
	reserve, basis := s.resolveAdmitReserve(request, ceiling)
	if reserve > ceiling {
		s.writeAdmitRejection(conn, CodeAdmitTooLarge, admitRejection{Required: reserve, Ceiling: ceiling, Basis: basis})
		return
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
	waiter.scopeCeiling = scopeCeiling
	peerCtx, cancelPeer := context.WithCancel(context.Background())
	defer cancelPeer()
	go func() {
		var one [1]byte
		_, _ = conn.Read(one[:])
		cancelPeer()
	}()

	released := false
	release := func() {
		if released {
			return
		}
		released = true
		s.releaseAdmitWaiter(queue, waiter)
	}
	defer release()

	remaining := time.Duration(request.maxWait)*time.Millisecond - s.admitNowTime().Sub(waiter.enqueued)
	if remaining < 0 {
		remaining = 0
	}
	var timer *time.Timer
	var deadline <-chan time.Time
	if s.admitAfter != nil {
		deadline = s.admitAfter(remaining)
	} else {
		timer = time.NewTimer(remaining)
		deadline = timer.C
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

	queue.mu.Lock()
	if waiter.state == admitRejected {
		// AIRA-101. Branch on the OUTCOME rather than hardcoding saturation for
		// every rejected waiter. An unestablished-emptiness abort is not a busy
		// slice — it is a slice the daemon could not read — and reporting it as
		// E_ADMIT_SATURATED would be a fabricated diagnosis of exactly the kind the
		// fail-closed emptiness rule exists to prevent.
		if waiter.outcome == admitOutcomeExclusiveUnestablished {
			queue.mu.Unlock()
			s.writeAdmitError(conn, CodeAdmitExclusiveUnestablished,
				CodeAdmitExclusiveUnestablished+": the confine scan is failing, so an empty slice could not be established for an exclusive request")
			return
		}
		// The EXCLUSIVE requester's own expiry. Its state is already admitRejected
		// by the time the gate is re-derived, so it no longer matches the drain
		// predicate and would otherwise be reported as plain saturation — "the slice
		// was contended" — when what actually happened is "my drain did not complete
		// in the budget I set". Naming it keeps the two apart for the one caller who
		// most needs the difference (found by build review).
		if waiter.exclusive {
			queue.mu.Unlock()
			s.writeAdmitRejection(conn, CodeAdmitSaturated, admitRejection{Basis: "reject:saturated", Exclusive: admitExclusiveDraining})
			return
		}
		// Report WHETHER the wait expired under an exclusive drain or hold, so a
		// blocked operator can tell "a benchmark has the slice" from ordinary
		// saturation. Basis keeps its exact "reject:saturated" spelling, which
		// validRunnerAdmitRejection pins, so this is purely additive.
		exclusiveState := exclusiveGateStateLocked(queue)
		queue.mu.Unlock()
		s.writeAdmitRejection(conn, CodeAdmitSaturated, admitRejection{Basis: "reject:saturated", Exclusive: exclusiveState})
		return
	}
	if waiter.state != admitGranted {
		queue.mu.Unlock()
		return
	}
	grant := AdmitResponse{State: waiter.outcome, Reason: waiter.reason, WaitedMS: waiter.waitedMS, Reserve: waiter.reserve, Basis: waiter.basis, ScopeCeiling: waiter.scopeCeiling}
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

func (s *Server) enqueueAdmit(path string, reserve int64) (*sliceQueue, *admitWaiter, string, error) {
	return s.enqueueAdmitInternal(path, reserve, "", 0, false, admitRequest{})
}

func (s *Server) enqueueResolvedAdmit(path string, reserve int64, basis string, maximum int64) (*sliceQueue, *admitWaiter, string, error) {
	return s.enqueueAdmitInternal(path, reserve, basis, maximum, true, admitRequest{})
}

func (s *Server) enqueueResolvedConfineAdmit(path string, reserve int64, basis string, maximum int64, request admitRequest) (*sliceQueue, *admitWaiter, string, error) {
	return s.enqueueAdmitInternal(path, reserve, basis, maximum, true, request)
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
	if len(queue.waiters) >= admitMaxWaiters {
		return nil, nil, CodeBusy, fmt.Errorf("%s: too many admission waiters for slice", CodeBusy)
	}
	if enforceCeiling && reserve > subtractFloor(maximum, s.admitSliceHeadroom(queue.outstandingJobs+1)) {
		return nil, nil, CodeAdmitTooLarge, fmt.Errorf("%s: required reserve exceeds cap minus headroom", CodeAdmitTooLarge)
	}
	if request.scopeID != "" {
		for _, existing := range queue.waiters {
			if existing != nil && existing.state != admitReleased && existing.scopeID == request.scopeID {
				return nil, nil, CodeProtocol, fmt.Errorf("%s: confine scope_id is already registered", CodeProtocol)
			}
		}
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
	waiter := &admitWaiter{seq: queue.seq, reserve: reserve, basis: basis, state: admitQueued, grantedCh: make(chan struct{}), enqueued: s.admitNowTime(), scopeID: request.scopeID, name: request.name, owner: request.owner, exclusive: request.exclusive, exclusiveHolder: request.exclusiveHolder, parentScopeID: request.parentScopeID}
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
	// Production has exactly one caller: this queue's runEvaluator goroutine.
	// That single-writer property permits the throttle read before queue.mu;
	// other goroutines only read the adopted ledger while holding queue.mu.
	now := s.admitNowTime()
	refreshInterval := s.admitConfineScanInterval
	if refreshInterval <= 0 {
		refreshInterval = admitConfineScanIntervalDefault
	}
	refreshAdopted := queue.adoptedAt.IsZero() || now.Sub(queue.adoptedAt) >= refreshInterval
	var scanResult runner.ConfineListResult
	var scanErr error
	if refreshAdopted {
		scan := s.admitConfineScan
		if scan == nil {
			scan = func(path string) (runner.ConfineListResult, error) {
				return runner.ListConfines(context.Background(), path, nil)
			}
		}
		scanResult, scanErr = scan(queue.path)
		if scanErr == nil && scanResult.Verdict == "unevaluated" {
			reason := strings.TrimSpace(scanResult.Reason)
			if reason == "" {
				reason = "confine scan unevaluated"
			}
			scanErr = errors.New(reason)
		}
	}

	queue.mu.Lock()
	defer queue.mu.Unlock()
	if refreshAdopted {
		// adoptedAt is the last scan attempt, successful or not, so a failing
		// filesystem does not turn every queue kick into another scan.
		queue.adoptedAt = now
		if scanErr != nil {
			if !queue.adoptedScanFailed {
				log.Printf("aira daemon: confine reserve scan failed: %v", scanErr)
			}
			queue.adoptedScanFailed = true
			// AIRA-101. liveScopes is only meaningful alongside a successful scan;
			// clearing the KNOWN bit is what makes the exclusive gate fail closed
			// rather than reading a stale emptiness as current fact.
			queue.liveScopesKnown = false
			// Arm the abort anchor on the FIRST failure while it is zero, and never
			// renew it on later failures. Arming only after a prior success would
			// never fire in this rule's own primary case — a slice unreadable from
			// the queue's very first pass — and renewing would let the anchor
			// postpone its own deadline forever (the freezeArmedAt lesson).
			if queue.scanFailingSince.IsZero() {
				queue.scanFailingSince = now
			}
		} else {
			queue.scanFailingSince = time.Time{}
			// AIRA-68. listConfines enumerates EVERY .aira-CONFINE-* directory
			// under the slice, irrespective of population or cap, so scan
			// membership is an authoritative presence test — and this block runs
			// only when the scan SUCCEEDED, so a failed scan writes no bit at all.
			present := make(map[string]struct{}, len(scanResult.Scopes))
			for _, record := range scanResult.Scopes {
				present[record.ScopeID] = struct{}{}
			}
			held := make(map[string]struct{})
			for _, waiter := range queue.waiters {
				if waiter == nil || waiter.state != admitGranted || waiter.scopeID == "" {
					continue
				}
				held[waiter.scopeID] = struct{}{}
				// The seen -> gone TRANSITION, recorded on the waiter. See the
				// scopeSeen/scopeVanished comment on admitWaiter for why the
				// transition, and not plain absence, is what the stale-lease sweep
				// is allowed to reclaim on.
				if _, exists := present[waiter.scopeID]; exists {
					waiter.scopeSeen, waiter.scopeVanished = true, false
					continue
				}
				if waiter.scopeSeen {
					waiter.scopeVanished = true
				}
			}
			// AIRA-101. The EMPTINESS reading, computed in the same successful scan
			// but deliberately NOT derived from adopted/adoptedJobs below.
			//
			// The adopted loop skips scopes on purpose — unpopulated ones,
			// non-finite-cap ones, and connection-held ones — and every one of those
			// exclusions is correct for RESERVE accounting and wrong for EMPTINESS,
			// because a skipped scope is still a running job. Reusing it would let an
			// exclusive job be told it is alone while a suite runs beside it.
			//
			// Liveness is SUBTREE-aware. Leaf cgroup.procs is not usable here:
			// BootstrapAitestSupervisor drains EVERY pid out of an aitest outer scope
			// into <outer>/.aira-supervisor, so a running suite's outer scope reads
			// leaf-empty. Before a daemon restart its connection-held lease still
			// keeps outstandingJobs >= 1, but after one it is merely an adopted scope
			// — and a leaf-only reading would then declare the slice empty and hand a
			// benchmark a fabricated "you are alone" while the suite ran on.
			liveScopes := 0
			for _, record := range scanResult.Scopes {
				// Unevaluated is NOT empty. A scope whose population could not be read
				// counts as live, so an unreadable scope can only ever delay an
				// exclusive grant, never fake one.
				if record.SubtreePopulated == nil || *record.SubtreePopulated {
					liveScopes++
				}
			}
			queue.liveScopes = liveScopes
			queue.liveScopesKnown = true
			adopted := int64(0)
			adoptedJobs := 0
			for _, record := range scanResult.Scopes {
				// Populated is the scope's LEAF cgroup.procs count, not the
				// subtree-aware cgroup.events populated the #72 reaper uses. A live
				// workload nested in a child cgroup it created reads empty here and is
				// SKIPPED — the safe direction (its reserve is under-counted → over-
				// admit, exactly as a fully forgotten pre-restart ledger, never worse).
				// Subtree-aware liveness for adopted is a v2 item.
				if record.Populated == nil || *record.Populated <= 0 {
					continue
				}
				if _, connectionHeld := held[record.ScopeID]; connectionHeld {
					continue
				}
				// A non-finite cap (delegate-ram "max", nil, malformed, negative)
				// contributes NEITHER reserve bytes NOR a headroom-job: such a scope is
				// unreconstructable, left as a safe under-count (its actual RSS is still
				// charged via `current`). Counting only finite-cap scopes keeps adopted
				// and adoptedJobs consistent — never a new wrongful-wait.
				if record.Cap == nil {
					continue
				}
				cap, err := strconv.ParseInt(strings.TrimSpace(*record.Cap), 10, 64)
				if err != nil || cap < 0 {
					continue
				}
				if runner.IsDelegateRAMScopeID(record.ScopeID) {
					// AIRA-15 ceiling caps are containment backstops, never a
					// whole-job reservation. After daemon restart only the dirname
					// survives, so its positional marker selects current+margin
					// reconstruction instead of adopting the generous ceiling.
					if record.RSSBytes == nil || *record.RSSBytes < 0 {
						continue
					}
					currentWithMargin := addClamp(*record.RSSBytes, delegateRAMAdoptionMargin)
					if currentWithMargin < cap {
						cap = currentWithMargin
					}
				}
				adopted = addClamp(adopted, cap)
				adoptedJobs = addJobCountClamp(adoptedJobs, 1)
			}
			queue.adopted = adopted
			queue.adoptedJobs = adoptedJobs
			queue.adoptedScanFailed = false
		}
	}
	readMemory := s.admitReadMemory
	if readMemory == nil {
		readMemory = readSliceMemory
	}
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
	// below uses, so hold and yield shift symmetrically if an adopted-confine scan
	// delays the pass.
	phase := admitFreezePhaseAt(queue.freezeArmedAt, now, maxHold)
	if maxHold > 0 && phase == admitFreezeIdle && !queue.freezeArmedAt.IsZero() {
		// A completed cycle must YIELD AT LEAST ONE EVALUATION before re-arming.
		// The phase is derived from wall time, but grants only happen during an
		// evaluator pass, so a yield window that elapses entirely BETWEEN passes
		// would let the queue go hold -> idle -> re-armed in a single pass and
		// backfill nothing at all — freezing forever while looking well-behaved.
		// That happens whenever maxHold approaches the poll interval (any positive
		// duration is accepted) or a slow adopted-confine scan delays a pass past
		// a whole cycle. Clearing the anchor and treating THIS pass as a yield
		// makes the guarantee "at least one backfilling pass per cycle", which is
		// what actually admits waiters, rather than merely "some wall time spent
		// nominally yielding".
		queue.freezeArmedAt = time.Time{}
		phase = admitFreezeYield
	}
	// AIRA-101. Abort a drain the daemon cannot establish emptiness for, BEFORE
	// the grant loop, so the drain lifts in the same pass rather than one later.
	// Without this the fail-closed emptiness rule would stall the whole shared
	// slice for the full ceiling on a persistently unreadable slice: the drain
	// head cannot be granted, and every other waiter is blocked by the drain.
	//
	// The abort takes the identical path as timeoutAdmitWaiter — state becomes
	// admitRejected, grantedCh closes, and the handler's deferred release removes
	// the waiter — so it leaks nothing, and exclusiveActive() stops matching the
	// instant the state changes, which is what lifts the drain.
	if gate := exclusiveGateLocked(queue); gate.draining != nil && !queue.scanFailingSince.IsZero() &&
		now.Sub(queue.scanFailingSince) >= admitExclusiveEstablishGrace {
		waiter := gate.draining
		waiter.state = admitRejected
		waiter.outcome = admitOutcomeExclusiveUnestablished
		waiter.waitedMS = elapsedMilliseconds(waiter.enqueued, s.admitNowTime())
		close(waiter.grantedCh)
		log.Printf("aira daemon: exclusive admission aborted on %s: confine scan failing for %s, cannot establish an empty slice (scope=%s)",
			queue.path, now.Sub(queue.scanFailingSince).Round(time.Second), waiter.scopeID)
	}
	gate := exclusiveGateLocked(queue)
	// AIRA-103. The one place the pressure throttle actually gates admission.
	// Hoisted out of the waiter loop: it is a per-pass fact, and a pure leaf-lock
	// lookup that must not be repeated per waiter while queue.mu is held.
	// DELIBERATELY not applied to the ceiling admitConnection computes from the
	// same file: that one decides the TERMINAL E_ADMIT_TOO_LARGE and sizes a
	// job's own hard scope cap, so a job too large for the throttled ceiling must
	// WAIT here rather than be refused there.
	effectiveMaximum := s.admitEffectiveMaximum(queue.path, maximum)
	frozen := false
	for _, waiter := range queue.waiters {
		if waiter.state != admitQueued {
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
			continue
		}
		jobs := addJobCountClamp(addJobCountClamp(queue.outstandingJobs, queue.adoptedJobs), 1)
		headroom := s.admitSliceHeadroom(jobs)
		available := checkedAvailable(current, effectiveMaximum, reclaimable, addClamp(queue.outstanding, queue.adopted), headroom)
		if frozen {
			waiter.waited = true
			continue
		}
		if waiter.reserve > available {
			waiter.waited = true
			// now is pass-start time, so a slow adopted-confine scan can defer this freeze by its duration.
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
		// grantedAt is the one moment in this system that authoritatively marks
		// "the daemon just decided this job may proceed". It is deliberately
		// separate from enqueued (set once, at waiter creation): a waiter that
		// queued for a long time under contention is granted here, now, and
		// measuring its lease age from enqueued would conflate ordinary
		// admission-queue contention with launch abandonment — the AIRA-49 v3
		// defect. Nothing but this line may ever set it.
		waiter.state = admitGranted
		waiter.grantedAt = s.admitNowTime()
		waiter.accounted = true
		queue.outstanding += waiter.reserve
		queue.outstandingJobs++
		if waiter.waited {
			waiter.outcome = "waited"
			waiter.waitedMS = elapsedMilliseconds(waiter.enqueued, s.admitNowTime())
		} else {
			waiter.outcome = "immediate"
		}
		close(waiter.grantedCh)
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
	if charge >= ceiling {
		return 0
	}
	return ceiling - charge
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

// releaseAdmitWaiterLocked is the ledger discharge itself, with queue.mu ALREADY
// HELD by the caller. It reports whether THIS call performed the transition, so
// a caller can never log or count a reclaim that a concurrent release had
// already done.
//
// AIRA-68 split this out of releaseAdmitWaiter so the stale-lease sweep can make
// its final validation and its discharge ONE critical section. Validating under
// the lock, dropping it, and then discharging leaves a window in which the
// evaluator re-observes the scope and clears scopeVanished — after which the
// sweep would still discharge a lease whose reclaim proof had just evaporated.
// Both plan reviewers found that window independently.
//
// The caller must run afterAdmitRelease once it has dropped queue.mu, and only
// when this returned true.
func releaseAdmitWaiterLocked(queue *sliceQueue, waiter *admitWaiter) bool {
	if waiter.state == admitReleased {
		return false
	}
	if waiter.state == admitGranted && waiter.accounted {
		queue.outstanding -= waiter.reserve
		queue.outstandingJobs--
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
	return true
}

// afterAdmitRelease runs the post-discharge work that must NOT hold queue.mu:
// pruneAdmitQueue takes admitRegistryMu then queue.mu, so calling it under
// queue.mu would invert the one fixed lock order in this file.
func (s *Server) afterAdmitRelease(queue *sliceQueue) {
	queue.signal()
	// A ledger release can make a parked RAM-aware worker fit. This is only the
	// coalescing, lock-free signal: evaluate must never be called synchronously
	// from this release path because it previously held queue.mu.
	if s.governor != nil {
		s.governor.signal()
	}
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

func (s *Server) writeAdmitError(conn net.Conn, code, message string) {
	_ = conn.SetWriteDeadline(time.Now().Add(admitWriteTimeout))
	write := s.admitWriteFrame
	if write == nil {
		write = func(conn net.Conn, value any) error { return writeFrame(conn, value) }
	}
	_ = write(conn, errorFrame(code, message))
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
	if len(args) < 3 || len(args) > 12 {
		return admitRequest{}, fmt.Errorf("%s: admit requires slice, reserve, max_wait_ms, optional signature/pinned/delegate_ram/exclusive/exclusive_holder/parent_scope_id, and an optional complete scope_id/name/owner tuple", CodeProtocol)
	}
	for name := range args {
		if name != "slice" && name != "reserve" && name != "max_wait_ms" && name != "signature" && name != "pinned" && name != "delegate_ram" && name != "scope_id" && name != "name" && name != "owner" && name != "exclusive" && name != "exclusive_holder" && name != "parent_scope_id" {
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
	maxWait, ok := exactAdmitInt64(args["max_wait_ms"])
	if !ok {
		return admitRequest{}, fmt.Errorf("%s: admit max_wait_ms must be an integer", CodeProtocol)
	}
	if maxWait < 0 {
		maxWait = 0
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
		// invisible to every scan, adoption pass and reaper.
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
		return admitRequest{slice: slice, reserve: reserve, maxWait: maxWait, signature: signature, pinned: pinned, delegateRAM: delegateRAM, scopeID: scopeText, name: nameText, owner: ownerText, exclusive: exclusive, exclusiveHolder: exclusiveHolder, parentScopeID: parentScopeID}, nil
	}
	// An exclusive request MUST carry the scope tuple. Exclusivity is attributed
	// to, reported by, and reaped through the holder's scope id: a scope-less
	// exclusive could never be named in `confine --list`, never matched by a
	// nested job's holder token, and never reclaimed by the stale-lease sweep.
	if exclusive {
		return admitRequest{}, fmt.Errorf("%s: admit exclusive requires the scope_id, name and owner tuple", CodeProtocol)
	}
	return admitRequest{slice: slice, reserve: reserve, maxWait: maxWait, signature: signature, pinned: pinned, delegateRAM: delegateRAM, exclusiveHolder: exclusiveHolder, parentScopeID: parentScopeID}, nil
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

func readSliceMemory(path string) (cur, max, reclaimable int64, ok bool, reason string) {
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
	if strings.TrimSpace(string(maxData)) == "max" {
		return 0, 0, 0, false, "unbounded"
	}
	limit, valid := parseAdmitMemory(maxData)
	if !valid {
		return 0, 0, 0, false, "parse-error"
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
