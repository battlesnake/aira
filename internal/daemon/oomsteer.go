package daemon

import (
	"context"
	"log"
	"path/filepath"
	"sort"
	"time"

	"aira/internal/runner"
)

// AIRA-113. Dynamic per-scope oom_score_adj steering: the residual left by
// declared-only admission accounting.
//
// THE FAILURE THIS EXISTS FOR. Admission bounds the DECLARED reserves — for a
// new admission Sigma(reserve) <= slice cap — but it does not measure a scope's
// LIVE usage: the ledger holds the declared reserve for a lease's whole lifetime
// (the AIRA-29 live charge was retired). So a scope that USES more than it
// declared — an under-declaration, or a burst past its reserve — is invisible to
// admission and can expand until aira.slice reaches its own cap and the kernel
// picks a victim, biased only by AIRA-27's STATIC class steering. (Physical
// over-use, up to a scope's own memory.max and above the DECLARED reserve the
// ledger holds, is possible for any scope whose cap exceeds its reserve or which
// runs uncapped, so under declared-only admission the per-scope memory.max and the
// MemAvailable watchdog are the backstop, not an aggregate over-subscription bound.
// PRE-S2a a `--delegate-ram` scope was the textbook example: its memory.max was an
// AIRA-15 containment ceiling many times its declared reserve. Post-S2a a delegate
// parent is an ordinary confine job whose memory.max IS its reserve, so it is no
// longer that case — see the fold note below.)
//
// That static bias picks the wrong victim in exactly the case that matters.
// oom_score_adj is worth adj/1000 of MACHINE total in badness, so on a 64 GiB
// box the delegate class's 800 outweighs the non-delegate's 500 by ~19 GiB of
// virtual badness. A compliant --delegate-ram suite sitting at 20 GiB therefore
// outscores a non-delegate job that has burst to 30 GiB past what admission
// accounts for: score 71 against 62, and the kernel kills the COMPLIANT
// neighbour. Raising the proven offender to 1000 makes it 94 against 71 and the
// kernel kills the offender.
//
// WHAT IT IS AND IS NOT. It is a BIAS, exactly as AIRA-27 is: the kernel still
// chooses, and a large enough compliant process can still outscore a small
// offender. It never lowers anything below its AIRA-27 class baseline, so it
// can only ever sharpen that containment, never weaken it. It writes no cgroup
// file, moves nothing kernel-enforced, and cannot pressure, throttle or refuse
// any job — the only thing it changes is which process the kernel prefers IF an
// OOM happens anyway.
//
// WHY A SEPARATE LOOP RATHER THAN A TERM IN evaluateAdmitQueue. The admission
// scan runs at <=1s and accounts only DECLARED reserves; it never reads a
// scope's live memory.current for charging. Catching a scope that is outrunning
// its declared reserve means sampling memory.current faster than a burst can
// drive an already-full slice into an OOM — hence a subsystem with its own
// faster cadence, its own state, and no admission lock held while it walks /proc.
//
// COST. The whole loop is one memory.current + one memory.stat read per tick
// while the slice is not full, which is ~always. Only when the aggregate is
// genuinely full does it snapshot the ledger, read one memory.current per
// charged scope, and write /proc only for scopes whose desired value CHANGED.
//
// RESIDUALS, stated rather than papered over:
//
//   - Post-daemon-restart survivors are not steered until they re-declare.
//     S11 reloads them as reserve-only leases and S12 deleted the scan-adoption
//     ledger that once tracked them per scope, so there is no over-budget
//     reading to steer on; once a survivor re-declares it is an ordinary
//     connection-held lease and is steered like any other. A bounded
//     post-restart gap, not a missing input.
//   - A scope raised to 1000 and still alive when the daemon stops keeps that
//     value for the rest of its life: the restore pass lives in the daemon.
//     That leaves a job which demonstrably outran its accounting as the
//     preferred victim, which is the safe direction, but it is a real
//     asymmetry.
//   - Only the default confine slice is steered, exactly as AIRA-103's ceiling
//     governs only that one.
//   - The fullness reading and the per-scope reading are taken at slightly
//     different instants; a burst inside that window is caught on the next tick.
//   - The ledger charge IS the scope's DECLARED reserve for its whole lifetime,
//     so a scope reads as over-budget whenever its live memory.current exceeds
//     that reserve by more than the overrun floor. That is the intended signal —
//     an under-declaration bias — not an error: over-declaring is a caller error
//     the ledger accepts, and a job that uses more than it declared is exactly
//     what this loop biases the OOM toward. Both the raise and the restore are
//     logged.

type oomSteerMode string

const (
	oomSteerOff     oomSteerMode = "off"
	oomSteerObserve oomSteerMode = "observe"
	oomSteerEnforce oomSteerMode = "enforce"
)

const (
	// defaultOOMSteerInterval must be FASTER than the admission scan cadence
	// (admitConfineScanIntervalDefault, <=1s). A loop no faster could not sample
	// a burst before it drives the slice into an OOM, and the admission scan
	// reads only declared reserves in any case.
	defaultOOMSteerInterval = 250 * time.Millisecond
	// oomSteerEnterPctDefault / oomSteerExitPctDefault are the fullness band, as
	// a percentage of the slice's own kernel-enforced memory.max. Steering below
	// the enter threshold would write /proc for jobs that are in no danger; a
	// single threshold would flap a scope between 500 and 1000 on ordinary
	// second-to-second jitter, so the exit is deliberately lower.
	oomSteerEnterPctDefault = int64(90)
	oomSteerExitPctDefault  = int64(80)
	// oomSteerOverrunFloorDefault is how far past its DECLARED reserve a scope's
	// live memory.current must be before it counts as an offender. The declared
	// reserve carries no built-in margin, so this floor is what separates a real
	// under-declaration from the torn read between the ledger snapshot and the
	// per-scope memory.current read.
	oomSteerOverrunFloorDefault = int64(64 << 20)
	// oomSteerUnevaluatedLogInterval rate-limits the "cannot establish" line so
	// a persistently unreadable slice does not write a log entry four times a
	// second.
	oomSteerUnevaluatedLogInterval = time.Minute
)

// oomSteerDeps is the subsystem's whole interface to the world, so every branch
// below is reachable from a test without a cgroup, a /proc write, or a daemon.
type oomSteerDeps struct {
	resolveSlice     func() (string, bool, string)
	readSliceParts   func(string) (current, reclaimable, maximum int64, ok bool, reason string)
	readScopeCurrent func(string) (int64, bool)
	// budgets reports, per scope id, the bytes the ADMISSION LEDGER currently
	// holds for that scope — the figure a scope is "over budget" relative to.
	budgets func(string) map[string]int64
	// classAdj is AIRA-27's class baseline for a scope id, read through the
	// launcher's own policy function so the two can never disagree.
	classAdj func(string) (int, error)
	apply    func(string, int) (runner.OOMScoreSteerResult, error)

	enterPct     int64
	exitPct      int64
	overrunFloor int64
	steeredAdj   int

	now   func() time.Time
	logf  func(string, ...any)
	sleep func(context.Context, time.Duration) bool
}

// oomSteerState is the loop's memory between ticks. applied records what this
// subsystem last WROTE for a scope; a scope absent from it is at its AIRA-27
// class baseline, which is a fact rather than an assumption — the confined child
// writes that value to its own /proc/self/oom_score_adj at exec and every
// descendant inherits it.
type oomSteerState struct {
	path            string
	full            bool
	applied         map[string]int
	unevaluatedAt   time.Time
	unevaluatedLast string
}

func newOOMSteerState(path string) oomSteerState {
	return oomSteerState{path: path, applied: map[string]int{}}
}

// oomSteerTarget is one scope's decision for this pass, resolved before
// anything is written so the write loop has no policy left in it.
type oomSteerTarget struct {
	scopeID string
	dir     string
	base    int
	want    int
	rss     int64
	budget  int64
}

// oomSteerFull applies the fullness band with hysteresis: cross enterPct to
// become full, fall below exitPct to stop being full, and hold the previous
// answer in between.
//
// anon is the slice's NON-reclaimable footprint (sliceCeilingAnon), not raw
// memory.current, because page cache is reclaimed before any OOM: a slice at
// memory.max entirely on file pages is not close to an OOM at all, and steering
// on it would raise the adj of healthy jobs on every large build.
func oomSteerFull(was bool, anon, maximum, enterPct, exitPct int64) bool {
	if maximum <= 0 || anon < 0 || enterPct <= 0 {
		return false
	}
	if anon >= pctClamp(maximum, enterPct) {
		return true
	}
	if !was {
		return false
	}
	return anon >= pctClamp(maximum, exitPct)
}

// evaluateOOMSteer runs one pass. queue.mu is taken only inside deps.budgets,
// and is released before any /proc read or write.
func evaluateOOMSteer(mode oomSteerMode, state *oomSteerState, deps oomSteerDeps) {
	path, ok, reason := deps.resolveSlice()
	if !ok {
		// HOLD, never restore. An unresolvable slice establishes nothing about
		// whether a scope is still over budget, and restoring on it would undo a
		// correct raise on the strength of a failed read.
		oomSteerReportUnevaluated(state, deps, "slice unresolvable: "+reason)
		return
	}
	if state.path != path {
		// A sample is a fact about a SLICE. Nothing carries across a change of
		// the governed path.
		*state = newOOMSteerState(path)
	}
	current, reclaimable, maximum, ok, reason := deps.readSliceParts(path)
	if !ok {
		oomSteerReportUnevaluated(state, deps, "slice memory unevaluated: "+reason)
		return
	}
	anon := sliceCeilingAnon(current, reclaimable)
	// DELIBERATELY the kernel-enforced memory.max, NOT admitEffectiveMaximum.
	// AIRA-103's published ceiling is a figure admission believes in; the OOM
	// this steers is a kernel event at the real cap, and asking "how close is the
	// kernel to killing something" against a fiction would answer a different
	// question. The two gates are allowed to disagree here precisely because they
	// are measuring different things.
	state.full = oomSteerFull(state.full, anon, maximum, deps.enterPct, deps.exitPct)

	budgets := deps.budgets(path)
	live := make(map[string]struct{}, len(budgets))
	targets := make([]oomSteerTarget, 0, len(budgets))
	for scopeID, budget := range budgets {
		base, err := deps.classAdj(scopeID)
		if err != nil {
			// The class policy itself is unusable (a malformed override). Steering
			// against a baseline the launcher did not use would be steering against
			// the wrong number, so this scope is left entirely alone — and left out
			// of `live`, so a previously raised scope is not restored on the
			// strength of the same unusable policy either.
			continue
		}
		live[scopeID] = struct{}{}
		dir := filepath.Join(path, confineScopeDirName(scopeID))
		if !state.full {
			targets = append(targets, oomSteerTarget{scopeID: scopeID, dir: dir, base: base, want: base, budget: budget})
			continue
		}
		rss, readOK := deps.readScopeCurrent(dir)
		if !readOK {
			// Hold whatever is already applied: an unreadable scope has not been
			// shown to be back within its budget.
			continue
		}
		if budget < 0 {
			budget = 0
		}
		want := base
		if rss-budget > deps.overrunFloor {
			want = deps.steeredAdj
		}
		// NEVER below the class baseline, whatever the configured steer value is.
		// AIRA-27's bias is containment this subsystem may sharpen and must not
		// weaken, and a steer value misconfigured under a class baseline would
		// otherwise make a delegate scope LESS killable than the launcher made it.
		if want < base {
			want = base
		}
		if want > runner.ConfineMaxOOMScoreAdj {
			want = runner.ConfineMaxOOMScoreAdj
		}
		targets = append(targets, oomSteerTarget{scopeID: scopeID, dir: dir, base: base, want: want, rss: rss, budget: budget})
	}
	// Scopes this subsystem raised that have since left the ledger: restore them
	// once, then forget them. A scope whose directory is gone fails the write and
	// is forgotten by the same path.
	for scopeID, applied := range state.applied {
		if _, stillLive := live[scopeID]; stillLive {
			continue
		}
		base, err := deps.classAdj(scopeID)
		if err != nil {
			// The same unusable class policy as above, and the same answer: HOLD.
			// Deleting the entry here would silently abandon a live raise — the
			// scope would keep its 1000 with nothing left that knows to restore it.
			continue
		}
		if applied == base {
			delete(state.applied, scopeID)
			continue
		}
		targets = append(targets, oomSteerTarget{
			scopeID: scopeID, dir: filepath.Join(path, confineScopeDirName(scopeID)), base: base, want: base,
		})
	}
	// Deterministic order, so a log line and a test read the same sequence
	// whatever the map iteration produced.
	sort.Slice(targets, func(i, j int) bool { return targets[i].scopeID < targets[j].scopeID })
	for _, target := range targets {
		applied, known := state.applied[target.scopeID]
		if !known {
			applied = target.base
		}
		if applied == target.want {
			if !known || target.want == target.base {
				delete(state.applied, target.scopeID)
			}
			continue
		}
		verb := "steering"
		if target.want == target.base {
			verb = "restoring"
		}
		if mode != oomSteerEnforce {
			deps.logf("aira daemon: oom-steer (%s): would be %s %s to oom_score_adj %d (class baseline %d, rss=%d ledger-charge=%d slice=%s)",
				mode, verb, target.scopeID, target.want, target.base, target.rss, target.budget, state.path)
			state.applied[target.scopeID] = target.want
			continue
		}
		result, err := deps.apply(target.dir, target.want)
		if err != nil {
			// The scope is gone (or unreadable). Forget it rather than retrying at
			// the tick rate; if it comes back it re-enters at its class baseline.
			delete(state.applied, target.scopeID)
			deps.logf("aira daemon: oom-steer: cannot %s %s: %v", verb, target.scopeID, err)
			continue
		}
		if target.want == target.base {
			delete(state.applied, target.scopeID)
		} else {
			state.applied[target.scopeID] = target.want
		}
		deps.logf("aira daemon: oom-steer: %s %s to oom_score_adj %d (class baseline %d, rss=%d ledger-charge=%d, %d/%d pids written across %d cgroups, %d failed, %d skipped)",
			verb, target.scopeID, target.want, target.base, target.rss, target.budget,
			result.Written, result.PIDs, result.Cgroups, result.Failed, result.Skipped)
	}
}

func oomSteerReportUnevaluated(state *oomSteerState, deps oomSteerDeps, reason string) {
	now := deps.now()
	if state.unevaluatedLast == reason && now.Sub(state.unevaluatedAt) < oomSteerUnevaluatedLogInterval {
		return
	}
	state.unevaluatedLast, state.unevaluatedAt = reason, now
	deps.logf("aira daemon: oom-steer unevaluated: %s", reason)
}

func runOOMSteer(ctx context.Context, mode oomSteerMode, interval time.Duration, deps oomSteerDeps) {
	if mode == oomSteerOff || mode == "" {
		<-ctx.Done()
		return
	}
	if interval <= 0 || !validOOMSteerDeps(deps) {
		// Say so rather than parking silently: a subsystem asked for in enforce
		// mode that quietly does nothing is indistinguishable from one that found
		// no pressure, which is the wrong thing to be indistinguishable from.
		log.Printf("aira daemon: oom-steer disabled: invalid configuration (interval=%s enterPct=%d exitPct=%d overrunFloor=%d steeredAdj=%d)",
			interval, deps.enterPct, deps.exitPct, deps.overrunFloor, deps.steeredAdj)
		<-ctx.Done()
		return
	}
	state := newOOMSteerState("")
	for {
		select {
		case <-ctx.Done():
			return
		default:
		}
		evaluateOOMSteer(mode, &state, deps)
		if !deps.sleep(ctx, interval) {
			return
		}
	}
}

func validOOMSteerDeps(deps oomSteerDeps) bool {
	return deps.enterPct > 0 && deps.enterPct <= 100 && deps.exitPct >= 0 && deps.exitPct <= deps.enterPct &&
		deps.overrunFloor >= 0 && deps.steeredAdj >= runner.ConfineOOMScoreAdj && deps.steeredAdj <= runner.ConfineMaxOOMScoreAdj &&
		deps.resolveSlice != nil && deps.readSliceParts != nil && deps.readScopeCurrent != nil &&
		deps.budgets != nil && deps.classAdj != nil && deps.apply != nil &&
		deps.now != nil && deps.logf != nil && deps.sleep != nil
}

func (s *Server) runOOMSteer(ctx context.Context, mode oomSteerMode, interval time.Duration, deps oomSteerDeps) {
	runOOMSteer(ctx, mode, interval, deps)
}

func realOOMSteerDeps(s *Server) oomSteerDeps {
	return oomSteerDeps{
		resolveSlice: func() (string, bool, string) {
			return s.sliceResolver()(runner.DefaultConfineSlice)
		},
		readSliceParts:   readSliceCeilingParts,
		readScopeCurrent: readSliceCeilingCurrent,
		budgets:          s.admitScopeBudgets,
		classAdj:         runner.ConfineClassOOMScoreAdj,
		apply:            runner.SetSubtreeOOMScoreAdj,
		enterPct:         oomSteerEnterPctDefault,
		exitPct:          oomSteerExitPctDefault,
		overrunFloor:     oomSteerOverrunFloorDefault,
		steeredAdj:       runner.ConfineMaxOOMScoreAdj,
		now:              time.Now,
		logf:             log.Printf,
		sleep:            watchdogSleep,
	}
}

// admitScopeBudgets reports what the admission ledger currently holds for each
// scope-backed job on this slice — the number a scope's live memory.current is
// compared against to decide whether it is outrunning its own accounting.
//
// TWO POPULATIONS, ONE BUDGET, and getting this wrong is the difference between
// steering the offender and steering the most compliant job on the box. An
// aitest `--delegate-ram` suite's own waiter charges only its OWN (ordinary)
// parent-scope reserve — sized for the supervisor and framework, not the whole
// suite — because its per-worker sub-reservations are separate waiters in this
// same queue that carry the real charge (the double-book AIRA-29's build review
// found, from the other direction). Under S2a those workers are first-class
// SIBLING scopes directly under the slice, NOT nested under the parent, so the
// parent's memory.current does NOT contain their bytes: it is the parent's own
// RSS alone. The workers themselves are never keys in this map — the loop below
// continues past every sub-reservation — and oomsteer's consumer iterates only
// these keys, so a sibling worker scope is never steered here at all; only its
// parent is (each worker is bounded by its own memory.oom.group instead). The
// fold below (summing each sub-reservation's charge into its parent's budget) is
// T3-inherited; post-collapse it makes the parent's budget an OVER-count relative
// to that sibling-free memory.current, which can only ever make a parent look
// LESS like an offender — an under-detection, never a false offender, so it is the
// safe direction. T10-S2 landed WITHOUT reconciling the fold to the sibling
// topology: it is left as an ACCEPTED RESIDUE, and it is inert by construction, not
// merely safe-direction. A parent that appears here at all is a granted, accounted,
// scope-backed waiter, so its scope memory.max IS its ledger charge (post-S2a a
// delegate parent is an ordinary confine job — its memory.max is its --memory-max,
// its declared reserve, or its granted reserve, with no delegate ceiling above the
// reserve). memory.current can never exceed memory.max, so a parent's live usage
// never exceeds even its OWN unfolded charge, let alone the larger folded budget —
// the over-count can therefore never flip it to an offender on any path, so
// reconciling the fold buys nothing. Stated here rather than hidden.
//
// A sub-reservation whose parent is not a scope-backed waiter here adds nothing:
// without the parent's own charge there is no budget to add it to, and inventing
// one would be a number nobody established. AIRA-115 removed the case that made
// this common: `confine-reserve` no longer defaults its slice independently of
// its parent, it inherits the parent job's RESOLVED slice — the same value that
// keyed the parent's own admission — or refuses. A sub-reservation taken inside
// a confine job therefore now lands in the same queue as its parent. The residue
// is still real, just narrower: an explicit `--slice` naming a different slice, a
// parent whose own waiter has already left this queue, or a caller that is not
// inside a confine job at all. It is left as an under-count of the budget — which
// can only ever make a scope look MORE like an offender, so it is checked against
// the same fullness gate and the same overrun floor as everything else, and is
// stated here rather than hidden.
//
// The two admission locks are taken SEQUENTIALLY, never nested, so this cannot
// participate in a lock-order inversion at all; the queue pointer stays valid
// after admitRegistryMu is released, and the map is copied out so nothing is
// read from the queue afterwards.
func (s *Server) admitScopeBudgets(path string) map[string]int64 {
	s.admitRegistryMu.Lock()
	queue := s.admitQueues[path]
	s.admitRegistryMu.Unlock()
	if queue == nil {
		return nil
	}
	queue.mu.Lock()
	defer queue.mu.Unlock()
	budgets := map[string]int64{}
	children := map[string]int64{}
	for _, waiter := range queue.waiters {
		if waiter == nil || waiter.state != admitGranted || !waiter.accounted {
			continue
		}
		if waiter.isSubReservation() {
			children[waiter.parentScopeID] = addClamp(children[waiter.parentScopeID], waiter.ledgerCharge())
			continue
		}
		if waiter.scopeID == "" {
			// A plain `aira admit` waiter creates no cgroup, so there is no scope
			// to steer and no memory.current to compare against. Absent, not zero.
			continue
		}
		budgets[waiter.scopeID] = addClamp(budgets[waiter.scopeID], waiter.ledgerCharge())
	}
	for scopeID, total := range children {
		if _, ok := budgets[scopeID]; !ok {
			continue
		}
		budgets[scopeID] = addClamp(budgets[scopeID], total)
	}
	return budgets
}
