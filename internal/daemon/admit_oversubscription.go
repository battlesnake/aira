package daemon

import (
	"math"

	"aira/internal/runner"
)

// AIRA-114 — the aggregate over-subscription bound.
//
// WHAT THIS FIXES. Before AIRA-29 the non-delegate confine class was airtight:
// reserve == the scope's own memory.max, and Sigma(reserve) <= cap - headroom,
// so Sigma(memory.max) could not exceed the slice and the kernel could not let
// the aggregate overrun. Charging LIVE usage broke that property, deliberately
// and with the owner's ruling: ten jobs each holding a 20G estimate but using
// 1G all fit a 64G slice, and could then demand 200G of it. That residual was
// recorded as AIRA-29 §4e and filed here. The concrete failure it names is
// several scopes expanding between scans until aira.slice reaches its own cap,
// producing a memcg OOM inside the slice biased only by AIRA-27's 500/800
// oom_score_adj class steering.
//
// This bounds that aggregate. It does NOT restore the airtight property, and
// says so plainly: the worst case becomes a KNOWN MULTIPLE of the slice ceiling
// instead of an unbounded one.
//
// WHY IT WAS DROPPED FROM AIRA-29, AND WHAT CHANGED. The v4 draft was refused
// on two grounded objections, both of which are prerequisites this file
// actually meets rather than argues around:
//
//  1. "The existing scan cannot supply the cap total correctly." True of the
//     ADOPTION loop, which gates on record.Populated — the scope's LEAF
//     cgroup.procs count — and so skips every busy aitest outer scope, whose
//     pids are drained into .aira-supervisor. A Sigma(cap) derived from that
//     loop silently under-counts and the bound would not bind. So this is a
//     SECOND, independent accounting, and its liveness reading is SUBTREE-aware
//     (ConfineRecord.SubtreePopulated, the kernel's own cgroup.events signal,
//     already produced by the scan since AIRA-101). It also counts the
//     connection-held population the adoption loop deliberately excludes.
//  2. "Every conservative treatment of the residual cases is worse." Also true
//     of the two treatments that draft considered. Counting a locally-uncapped
//     scope as unbounded WEDGES the shared slice; counting it as zero makes the
//     bound porous. The policy here is neither: an uncapped or unreadable-cap
//     live scope contributes its MEASURED memory.current, which is a reading the
//     scan already has, is never infinite, and is never zero for a scope that is
//     actually using memory. See recordScopeCapContribution.
//
// THE HONEST STATEMENT OF WHAT IS BOUNDED. Every confine launch requires a
// finite EFFECTIVE (ancestor-inclusive) cap, and every ADMITTED scope
// additionally gets its own finite LOCAL memory.max; the flock-fallback scopes
// that launch when the daemon is unavailable have only the former. So this
// gate bounds
//
//	Sigma(local memory.max over capped live scopes)
//	  + Sigma(memory.current over uncapped live scopes)   <=  factor * ceiling
//
// and the uncapped population's residual GROWTH above its measured usage is
// bounded, as it always was, by the slice's own finite cap and by
// checkedAvailable's physical `current` term. That is a real bound and a
// weaker one than "Sigma of every cap", and it is stated rather than rounded up
// into a claim the accounting cannot support.

// oversubscriptionLimit is factor * ceiling for this pass, or 0 when the bound
// is disabled or the ceiling could not be established.
//
// The ceiling is the SAME pressure-throttled effective maximum the reserve
// check uses (AIRA-103), not the static slice cap, so the two gates can never
// disagree about how big the slice is at this instant. A consequence worth
// naming: under pressure throttling the aggregate bound tightens with the
// ceiling, which is the direction that subsystem exists to move in.
func (s *Server) oversubscriptionLimit(effectiveMaximum int64) int64 {
	if s.oversubscriptionFactorPct <= 0 || effectiveMaximum <= 0 {
		return 0
	}
	return scaleByPercent(effectiveMaximum, s.oversubscriptionFactorPct)
}

// scaleByPercent returns value*pct/100 without overflowing, saturating at
// MaxInt64. Distinct from pctClamp, which caps pct at 100 because it computes a
// FRACTION of a job's peak; this one is a MULTIPLE of the slice ceiling and pct
// is always >= 100.
//
// Saturation is the correct direction here and is unreachable in practice: the
// result is a LIMIT, so a saturated one only ever makes the gate looser, never
// a wedge. Reaching it needs a ceiling above 92 PiB at the default factor.
func scaleByPercent(value, pct int64) int64 {
	if value <= 0 || pct <= 0 {
		return 0
	}
	if value > math.MaxInt64/pct {
		return math.MaxInt64
	}
	return value * pct / 100
}

// prospectiveScopeCap is the local memory.max this waiter's scope carries, or
// will carry once the launcher creates it. It is what the daemon ITSELF chose,
// never a measurement, and it is the only honest figure available in the
// grant -> backend.Create window that every launch has.
//
// The two arms mirror the launcher exactly (internal/runner/confine_linux.go):
// a delegate-ram scope is capped at the daemon's resolved scope ceiling, and
// every other admitted scope is capped at its resolved reserve.
//
// Two limits, both in the UNDER-count direction and both bounded by one scan
// interval, because the aggregate is re-derived from the kernel's own
// memory.max on every scan:
//
//   - A client-side `--memory-max` larger than the reserve overrides both arms
//     at launch. The daemon is not told, so this figure is low until the first
//     scan reads the real cap.
//   - resolveDelegateRAMScopeCeiling returns 0 when the slice ceiling is
//     unusable, and the launcher then falls back to its own client-side
//     delegate default. Such a waiter reads as its pinned framework reserve
//     here, which is much smaller.
//
// A scope-less waiter (plain `aira admit`, `aira confine-reserve`) creates no
// cgroup and therefore contributes no cap at all: zero here is a positive fact,
// not an unevaluated reading.
func (w *admitWaiter) prospectiveScopeCap() int64 {
	if w == nil || w.scopeID == "" || w.isSubReservation() {
		return 0
	}
	if w.scopeCeiling > 0 {
		return w.scopeCeiling
	}
	if w.reserve > 0 {
		return w.reserve
	}
	return 0
}

// contributesScopeCap reports whether this waiter will own a capped cgroup
// scope, and so belongs in the aggregate at all. The gate, the grant-time
// increment and the derive must all agree on this predicate or the running
// total and the re-derived total would disagree by construction.
func (w *admitWaiter) contributesScopeCap() bool {
	return w != nil && w.scopeID != "" && !w.isSubReservation()
}

// recordScopeCapContribution is THE uncapped-scope policy, and the reason this
// change was buildable when v4's was not. It returns the bytes one scan record
// contributes to the aggregate, and whether that could be established at all.
//
//   - A finite local memory.max is the enforced ceiling. Count it.
//   - A scope positively established to have NO local cap ("max") is the
//     flock-fallback population: it never went through admission, so AIRA never
//     chose its cap and cannot refuse it retroactively. Counting it as
//     unbounded would WEDGE this machine-wide slice on a job the daemon did not
//     admit — the worst failure mode in this subsystem, and the one AIRA-101's
//     whole design exists to make unrepresentable. Counting it as zero would
//     make the bound porous. It contributes its MEASURED memory.current: never
//     infinite, never zero while it is actually using memory, and re-read every
//     scan so it tracks the job rather than a guess.
//   - An UNREADABLE memory.max is treated the same way, for the same reason and
//     with the same measurement, rather than being fused with either of the
//     above. A diagnostic failure must not silently become an unbounded cap.
//   - A record with neither a cap nor a usable memory.current establishes
//     NOTHING, and says so. See aggregateScopeCap for what the caller does with
//     that.
func recordScopeCapContribution(record runner.ConfineRecord) (int64, bool) {
	if capBytes, finite, known := confineRecordCap(record); known && finite {
		return capBytes, true
	}
	if record.RSSBytes != nil && *record.RSSBytes >= 0 {
		return *record.RSSBytes, true
	}
	return 0, false
}

// scopeRecordIsLive reports whether a scan record should be counted. Liveness
// is SUBTREE-aware, and that is the load-bearing half of this file.
//
// record.Populated is the scope's LEAF cgroup.procs count. Every busy aitest
// outer scope reads 0 there, because BootstrapAitestSupervisor drains every pid
// into a child cgroup, and `podman run --cgroups=split` does the same. Gating
// on it — as the adoption loop does, correctly, for its own purposes — would
// drop precisely the largest-capped scopes on the machine out of the aggregate,
// and the bound would not bind. SubtreePopulated is the kernel's own
// cgroup.events reading and is the honest answer.
//
// UNEVALUATED IS NOT EMPTY: a scope whose population could not be read counts
// as live, so an unreadable scope can only ever tighten this gate, never
// loosen it.
func scopeRecordIsLive(record runner.ConfineRecord) bool {
	return record.SubtreePopulated == nil || *record.SubtreePopulated
}

// aggregateScopeCap sums every live scope's own local memory.max across the
// slice, and reports whether the total could be established. queue.mu must be
// held, and present/held must be the maps this pass's SUCCESSFUL scan built.
//
// The population is partitioned EXACTLY ONCE, on the same predicate `held` was
// built with (granted && scopeID != ""), so nothing is counted twice and
// nothing falls between the two loops. The `!isSubReservation()` half of
// contributesScopeCap cannot open a gap in that partition, because the admit
// protocol REFUSES a request carrying both scope_id and parent_scope_id
// (parseAdmitArgs), so no waiter can be in `held` and also be a
// sub-reservation:
//
//   - Connection-held waiters: scopes AIRA itself capped, or is about to. Their
//     contribution is always establishable, because the daemon knows the cap it
//     chose even before the scope exists (prospectiveScopeCap). This loop is
//     what closes the grant -> backend.Create window; a scan-only accounting
//     would read zero for a burst of fresh grants, which is the one under-count
//     direction this bound exists to prevent.
//   - Everything else the scan can see: post-restart orphans and
//     flock-fallback scopes launched while the daemon was unavailable.
//
// An unestablished total is returned as such and NEVER as a number. See
// oversubscriptionBlocks for the fail-open decision that follows from it.
//
// Two over-counts are accepted rather than engineered away, both bounded by one
// scan interval and both in the direction that withholds capacity:
//
//   - A granted waiter whose scope the scan has seen and then seen gone
//     (scopeVanished) keeps contributing its cap until its lease is reclaimed,
//     exactly as it keeps contributing to the reserve ledger. "Its scope is
//     gone" is not "the job is dead" — a leader can migrate into a sibling
//     cgroup and run on — so dropping it here would be a stronger claim than
//     the scan supports.
//   - Nothing decrements the running total on RELEASE; only the derive removes
//     a departed scope. A decrement could not be symmetric with the increment
//     anyway (the grant adds a prospective cap, while by release time the total
//     holds the kernel's own), and the derive is at most one second away.
func (s *Server) aggregateScopeCap(queue *sliceQueue, scopes []runner.ConfineRecord, present map[string]runner.ConfineRecord, held map[string]struct{}) (int64, bool) {
	total := int64(0)
	established := true
	for _, waiter := range queue.waiters {
		if waiter == nil || waiter.state != admitGranted || !waiter.contributesScopeCap() {
			continue
		}
		total = addClamp(total, s.heldScopeCapContribution(waiter, present))
	}
	for _, record := range scopes {
		if _, connectionHeld := held[record.ScopeID]; connectionHeld {
			continue
		}
		if !scopeRecordIsLive(record) {
			continue
		}
		contribution, ok := recordScopeCapContribution(record)
		if !ok {
			established = false
			continue
		}
		total = addClamp(total, contribution)
	}
	return total, established
}

// heldScopeCapContribution is the cap of one connection-held waiter's scope.
// The kernel's own reading wins when the scan has one; otherwise the daemon
// falls back to the cap it chose for that scope, which is why this arm can
// never be unestablished.
//
// A "max" or unreadable cap on a connection-held scope also lands on the
// fallback rather than on a measurement, and deliberately: unlike the
// flock-fallback population, this scope's cap IS a number AIRA picked, so the
// honest reading is that number, not the job's current usage. In practice the
// case is near-unreachable — an admitted non-delegate scope is capped at its
// reserve and a delegate one is refused a launch without a finite cap — so this
// is a fail-safe, not a routine path.
func (s *Server) heldScopeCapContribution(waiter *admitWaiter, present map[string]runner.ConfineRecord) int64 {
	if record, exists := present[waiter.scopeID]; exists {
		if capBytes, finite, known := confineRecordCap(record); known && finite {
			return capBytes
		}
	}
	return waiter.prospectiveScopeCap()
}

// oversubscriptionBlocks reports whether admitting this queued waiter would
// push the aggregate of live scope caps past the bound. queue.mu must be held.
//
// Every early return is a REFUSAL TO GATE, and each is a deliberate,
// individually argued fail-open:
//
//   - The bound is switched off (factor 0), or the slice ceiling could not be
//     established, so there is no limit to compare against.
//   - The aggregate is UNESTABLISHED — the scan failed, or some live scope had
//     neither a readable memory.max nor a readable memory.current. Gating on a
//     number nobody established is the fabricated reading this codebase refuses
//     everywhere; and failing CLOSED here would stall every job on a
//     machine-wide slice because of a diagnostic failure, which is the outage
//     AIRA-101's drain-abort rule exists to prevent. The asymmetry with
//     liveScopesKnown (which fails CLOSED) is intended: granting exclusivity
//     asserts "you are alone" and must be provable, whereas this bound only
//     ever withholds capacity.
//   - The aggregate is zero, so this waiter would be the only capped scope on
//     the slice. THE BOUND MUST NEVER WEDGE AN IDLE SLICE: a job admitted at
//     the very ceiling has a scope cap at the ceiling, and with a hostile
//     factor that alone could exceed the limit. Together with the parser's
//     refusal of a factor below 1, this makes "the head of an otherwise-idle
//     queue is always admissible" structural rather than a tuning property.
//   - The waiter creates no cgroup scope (plain `aira admit`) or is a
//     sub-reservation. A sub-reservation is a RUNNING job's internal progress
//     inside a cap already counted in the aggregate; blocking those would stall
//     the very suites whose exit is what relieves the bound.
func (s *Server) oversubscriptionBlocks(queue *sliceQueue, waiter *admitWaiter, limit int64) bool {
	if limit <= 0 || !queue.capAggregateKnown || queue.capAggregate <= 0 {
		return false
	}
	if !waiter.contributesScopeCap() {
		return false
	}
	return addClamp(queue.capAggregate, waiter.prospectiveScopeCap()) > limit
}
