package runner

import "time"

// ReconcileDecision is pure lifecycle policy. Kernel actions are deliberately
// represented as requirements so reconcile can never infer a kill merely from
// an empty scope.
type ReconcileDecision struct {
	Terminal       Status
	NeedsKill      bool
	KillProven     bool
	NeedsLost      bool
	PreserveOpen   bool
	ScopeIntegrity ScopeIntegrity
}

func decideReconcile(waitObserved, killIntent, scopeEmpty, killProven bool) ReconcileDecision {
	if waitObserved {
		return ReconcileDecision{PreserveOpen: true}
	}
	if killIntent {
		if killProven {
			return ReconcileDecision{Terminal: StatusKilled, KillProven: true}
		}
		if scopeEmpty {
			return ReconcileDecision{Terminal: StatusLost, NeedsLost: true}
		}
		return ReconcileDecision{NeedsKill: true}
	}
	if scopeEmpty {
		return ReconcileDecision{Terminal: StatusLost, NeedsLost: true}
	}
	return ReconcileDecision{PreserveOpen: true}
}

// decideTimeoutIntentNotExecuted reports whether the run-timeout kill intent
// THIS launch published was provably never delivered to anything, so the
// child's own pending wait result is the established outcome (AIRA-126).
//
// Each conjunct carries its own weight and none may be dropped:
//   - killErr == nil: an errored kill is unevaluated, never dismissed.
//   - IntentPublished && IntentCreated: the intent is ours, created by this
//     timeout, not adopted from a concurrent external kill. A foreign intent is
//     never dispositioned here.
//   - Kill.Empty && !Kill.Started: the only killScope return shape that proves
//     no signal was emitted (it returned before Terminate and before Kill) AND
//     that the scope was verified empty by two independent reads — leaf
//     cgroup.procs and subtree-aware cgroup.events, which must AGREE. A scope
//     that is leaf-empty but subtree-POPULATED is a busy nested job: since
//     AIRA-140 killScope kills it and returns Started:true, so this conjunct
//     fails and the run keeps the ordinary killed-by-timeout outcome, which is
//     the correct one. A scope repopulated between the two reads is handled the
//     same way.
//   - leader == processDead: kernel proof that the leader was already gone at
//     the instant the kill found nothing to signal. This is what separates
//     "already exited before any signal" from "still running past its deadline
//     and unkillable"; processAlive and processUnknown both refuse.
//
// It never reports true for an empty scope alone: emptiness is not proof that
// any kill won, which is exactly the inference killScope refuses.
func decideTimeoutIntentNotExecuted(killErr error, attempt killAttempt, leader processLiveness) bool {
	return killErr == nil &&
		attempt.IntentPublished && attempt.IntentCreated &&
		attempt.Kill.Empty && !attempt.Kill.Started &&
		leader == processDead
}

// decideNotExecutedDisposition is the terminal-CAS gate for AIRA-126. It is
// evaluated against the PRE-merge ledger state under the terminal lock:
// mergeEvidence replaces the base KillIntent wholesale whenever the candidate's
// is Present, so a post-merge read could see neither a concurrent actor's
// Completed:true nor an intent sequence other than the merged one.
//
// ledgerIntent is what the ledger holds for this run, publishedSequence is the
// sequence of the intent this launch's own timeout published. A read error, an
// absent intent, an intent another actor has already completed, or a sequence
// that is not the one this launch published all refuse the disposition and
// leave the unchanged reconcile-required arm in charge.
func decideNotExecutedDisposition(intentNotExecuted bool, ledgerErr error, ledgerIntent KillIntent, publishedSequence uint64) bool {
	return intentNotExecuted && ledgerErr == nil &&
		ledgerIntent.Present && !ledgerIntent.Completed &&
		ledgerIntent.Sequence == publishedSequence
}

// decideCPUBudgetExceeded is the whole CPU-budget comparison (AIRA-136),
// isolated so it cannot silently become an absolute-counter test or an
// off-by-one. consumed is measured from the run's own baseline, so this is never
// the scope's absolute counter. An unestablished sample never reaches it — the
// sampler skips — and a non-positive budget means no budget was requested.
func decideCPUBudgetExceeded(consumed, budget time.Duration) bool {
	return budget > 0 && consumed >= budget
}

// decideFinalCPUConsumed is the run's final consumed CPU-time, from the
// authoritative teardown total and the pre-start baseline (AIRA-136).
//
// When the pre-start baseline read FAILED there is no established baseline to
// subtract, and the sampler's own adopted baseline is deliberately private to
// its goroutine (it is a lower bound taken after the child was already running,
// and publishing it back would be shared mutable state carrying no evidence).
// The absolute counter is used instead. That is an UPPER BOUND on what this run
// consumed, so it errs only toward reporting U_RUN_CPU_BUDGET_UNENFORCED — the
// honest "AIRA cannot assert your bound held" direction — and never toward a
// kill, because this value is not an input to any kill decision.
func decideFinalCPUConsumed(total, baseline time.Duration, baselineEstablished bool) time.Duration {
	if !baselineEstablished {
		return total
	}
	return total - baseline
}

// decideCPUBudgetUnenforced reports whether a REQUESTED CPU-time budget cannot
// be asserted to have been applied (AIRA-136). Two states reach it:
//
//   - nothing was ever established: no cpu.stat read succeeded during the run
//     AND the teardown read did not succeed either, so AIRA has no idea how much
//     CPU the job used;
//   - the final total DID reach the budget and no CPU-budget kill was executed:
//     sampling was degraded for long enough to miss the crossing, or another
//     bound ended the run first.
//
// A run whose final established total is under the budget is fully evaluated
// even if the sampler was blind for part of it — the teardown read is a
// two-sided proof that the bound held, and AIRA already collects it.
//
// killedByCPUBudget, NOT "the CPU deadline fired", is the suppressing input, and
// that distinction is the whole honesty of the rule. A fire that killed nothing
// — AIRA-126's arbitrated exit, where the scope was already empty and the leader
// already dead — leaves the breach a two-sided established fact (the final total
// reached the budget) with no enforcement against it, which is exactly what this
// code is for. Suppressing on the fire alone would make the record depend on
// sampler phase: the same job, burning the same CPU, would report a clean
// success if a tick happened to land after its exit and an unenforced budget if
// it did not. Only an executed CPU-budget kill suppresses the code; an
// arbitrated exit, a wall-clock kill, and a plain exit are all decided by the
// teardown total.
func decideCPUBudgetUnenforced(budget time.Duration, killedByCPUBudget bool, finalConsumed time.Duration, finalEstablished bool) bool {
	if budget <= 0 || killedByCPUBudget {
		return false
	}
	if !finalEstablished {
		return true
	}
	return finalConsumed >= budget
}

// decideConfineDeadlineNotExecuted is AIRA-126's IDEA in a supervisor with no
// ledger (AIRA-138). It keeps AIRA-126's two EVIDENCE conjuncts verbatim and
// drops its two LEDGER conjuncts, which have no referent in confine: confine
// publishes nothing before it kills, so there is no intent to be foreign and
// none to have been concurrently completed. Dropping them is sound BECAUSE the
// artifact they guard does not exist; it is not a relaxation of the evidence
// bar. Passing `true, true` for conjuncts confine has no referent for would be a
// lie wearing the costume of reuse.
//
// What confine has instead is an exit code and a trailer, and those are just as
// capable of asserting a termination that did not occur — with LESS recourse,
// because ConfineResult has no ErrorCodes array and no reconcile pass to correct
// the record later. A confine fabrication is final the instant it is printed.
//
//   - killErr == nil: an errored kill is unevaluated, never dismissed.
//   - Empty && !Started: the only killConfineScope shape that proves no signal
//     was emitted (it returned before the cgroup.kill write) AND that the scope
//     was verified empty by TWO INDEPENDENT READS — leaf cgroup.procs and the
//     subtree-aware cgroup.events `populated`. A leaf-empty but
//     subtree-POPULATED scope is a busy job living in child cgroups it created
//     inside its own scope (the aitest / --delegate-ram / podman --cgroups=split
//     shape), and it never reaches this state: it is killed instead.
//   - leader == processDead: kernel proof the leader was already gone at the
//     instant the kill found nothing to signal. This separates "already exited
//     before any signal" from "still running past its deadline and unkillable";
//     processAlive and processUnknown both refuse.
//
// It never reports true for an empty scope alone: emptiness is not proof that
// any kill won, which is exactly the inference killScope refuses.
func decideConfineDeadlineNotExecuted(killErr error, attempt confineKillResult, leader processLiveness) bool {
	return killErr == nil && attempt.Empty && !attempt.Started && leader == processDead
}

// decideConfineDeadlineState is the TOTAL function from arbitration evidence to
// the one state ONE bound's trailer field renders (AIRA-138). It is called once
// per requested bound, and the caller passes, FOR THAT BOUND:
//
//	requested      : this bound was asked for (> 0). False renders nothing.
//	firedThisBound : the deadline source fired AND fired.Kind is this bound's.
//	attempt        : killConfineScope's result for that fire (zero if !fired).
//	killErr        : killConfineScope's error for that fire (nil if !fired).
//	notExecuted    : decideConfineDeadlineNotExecuted(killErr, attempt, leader).
//	cpuUnenforced  : decideCPUBudgetUnenforced(...) for the CPU bound; ALWAYS
//	                 false for the wall bound, which has no such state.
//
// PRECEDENCE, stated because two true things can describe one run:
//
// For the bound that FIRED, the fire-derived state wins. It is strictly more
// informative than `unenforced`: `fired-not-executed` says the budget was
// reached AND the bound fired AND the kill reached nothing, which ENTAILS "not
// enforced" and additionally says why. Rendering `unenforced` there would delete
// the fact that AIRA acted.
//
// For a bound that did NOT fire there is no kill to describe, so the measurement
// decides: the CPU bound renders `unenforced` when decideCPUBudgetUnenforced is
// true (the final total reached the budget with no executed CPU kill, or nothing
// was ever measured) and `not-reached` otherwise; the wall bound always renders
// `not-reached`, since a wall timer that did not fire is a one-sided fact
// needing no measurement.
//
// At most ONE bound can be fire-derived per run: the select consumes exactly one
// deadlineFire and deadlines.halt() discards any second, so the two fields can
// never both claim a fire.
func decideConfineDeadlineState(
	requested, firedThisBound bool,
	attempt confineKillResult, killErr error,
	notExecuted, cpuUnenforced bool,
) ConfineDeadlineState {
	switch {
	case !requested:
		return ""
	case firedThisBound && notExecuted:
		return ConfineDeadlineFiredNotExecuted
	case firedThisBound && attempt.Started && attempt.Completed && killErr == nil:
		return ConfineDeadlineFiredKillCompleted
	case firedThisBound && attempt.Started:
		return ConfineDeadlineFiredKillUnconfirmed
	case firedThisBound:
		return ConfineDeadlineFiredUnevaluated
	case cpuUnenforced:
		return ConfineDeadlineUnenforced
	default:
		return ConfineDeadlineNotReached
	}
}

func classifyMembership(initialVerified, processStillAlive, memberNow bool) (ScopeIntegrity, bool) {
	if !initialVerified {
		return ScopeHandoffUnverified, false
	}
	if processStillAlive && !memberNow {
		return ScopeMigrated, true
	}
	return ScopeContained, false
}
