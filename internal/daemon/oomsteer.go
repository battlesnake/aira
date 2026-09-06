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
// AIRA-29 and bounded, but not removed, by AIRA-114.
//
// THE FAILURE THIS EXISTS FOR. Before AIRA-29 the non-delegate confine class was
// airtight — reserve == the scope's own memory.max, and Sigma(reserve) <= slice
// cap — so the aggregate could not overrun and no memcg OOM could fire inside
// aira.slice. AIRA-29 charges LIVE usage instead, on the owner's explicit
// ruling, and AIRA-114 bounds the resulting over-subscription to a known
// multiple of the ceiling rather than restoring the airtight property. What
// remains (AIRA-29 residual 4e) is a genuine aggregate-full case: several scopes
// expand between scans until aira.slice reaches its own cap and the kernel picks
// a victim, biased only by AIRA-27's STATIC class steering.
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
// WHY A SEPARATE LOOP RATHER THAN A TERM IN evaluateAdmitQueue. AIRA-29 §3.6
// scoped this out and named the reason precisely: inside the admission scan the
// charge is computed from the same reading the trigger would use, and
// `rss <= peakSoFar` by the ratchet, so `rss - charge > 0` is reachable there
// only in the narrow window where memory.current transiently exceeds
// memory.max. Catching the population this is aimed at — a scope outrunning its
// accounting BETWEEN charge refreshes — requires reading RSS faster than the
// <=1s charge refresh. Hence a subsystem with its own cadence, its own state,
// and no admission lock held while it walks /proc.
//
// COST. The whole loop is one memory.current + one memory.stat read per tick
// while the slice is not full, which is ~always. Only when the aggregate is
// genuinely full does it snapshot the ledger, read one memory.current per
// charged scope, and write /proc only for scopes whose desired value CHANGED.
//
// RESIDUALS, stated rather than papered over:
//
//   - Adopted (post-daemon-restart) scopes are not steered. The ledger keeps
//     them as an AGGREGATE (queue.adopted), not per scope, so there is no
//     per-scope budget to compare against — and an adopted scope's charge is
//     re-derived from its own memory.current on every scan anyway, so it cannot
//     read as over-budget by construction.
//   - A scope raised to 1000 and still alive when the daemon stops keeps that
//     value for the rest of its life: the restore pass lives in the daemon.
//     That leaves a job which demonstrably outran its accounting as the
//     preferred victim, which is the safe direction, but it is a real
//     asymmetry.
//   - Only the default confine slice is steered, exactly as AIRA-103's ceiling
//     governs only that one.
//   - The fullness reading and the per-scope reading are taken at slightly
//     different instants; a burst inside that window is caught on the next tick.

type oomSteerMode string

const (
	oomSteerOff     oomSteerMode = "off"
	oomSteerObserve oomSteerMode = "observe"
	oomSteerEnforce oomSteerMode = "enforce"
)

const (
	// defaultOOMSteerInterval must be FASTER than the <=1s admission charge
	// refresh (admitConfineScanIntervalDefault). A loop no faster than the
	// charge would only ever see RSS readings the charge had already absorbed,
	// which is the exact inertness AIRA-29 §3.6 refused to ship.
	defaultOOMSteerInterval = 250 * time.Millisecond
	// oomSteerEnterPctDefault / oomSteerExitPctDefault are the fullness band, as
	// a percentage of the slice's own kernel-enforced memory.max. Steering below
	// the enter threshold would write /proc for jobs that are in no danger; a
	// single threshold would flap a scope between 500 and 1000 on ordinary
	// second-to-second jitter, so the exit is deliberately lower.
	oomSteerEnterPctDefault = int64(90)
	oomSteerExitPctDefault  = int64(80)
	// oomSteerOverrunFloorDefault is how far past its accounted charge a scope
	// must be before it counts as an offender. The charge already carries a
	// margin of at least chargeMarginFloorDefault, so crossing it at all means
	// the scope has outgrown its whole margin since the last refresh; this floor
	// only absorbs the torn read between the ledger snapshot and the per-scope
	// memory.current read.
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
			resolve := s.admitResolveSlice
			if resolve == nil {
				resolve = resolveAdmitSlicePath
			}
			return resolve(runner.DefaultConfineSlice)
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
// aitest `--delegate-ram` suite's own waiter charges only the small pinned
// FRAMEWORK OVERHEAD, because its per-test `aira confine-reserve`
// sub-reservations are separate scope-less waiters in this same queue that carry
// the real charge (the double-book AIRA-29's build review found, from the other
// direction). The parent's memory.current is HIERARCHICAL and already contains
// every byte those children allocated, so comparing it against the parent's own
// 512 MiB overhead would mark a perfectly compliant 30 GiB suite as an offender
// on every full slice. Summing the children into the parent is what makes the
// comparison apples-to-apples.
//
// A sub-reservation whose parent is not a scope-backed waiter here adds nothing:
// without the parent's own charge there is no budget to add it to, and inventing
// one would be a number nobody established. `confine-reserve` defaults its slice
// independently of its parent (confine_reserve_linux.go), so that case is real
// and is left as an under-count of the budget — which can only ever make a scope
// look MORE like an offender, so it is checked against the same fullness gate
// and the same overrun floor as everything else, and is stated here rather than
// hidden.
//
// Lock order is the established admitRegistryMu -> queue.mu, and the map is
// copied out so nothing is read from the queue afterwards.
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
