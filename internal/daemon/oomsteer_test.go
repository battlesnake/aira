package daemon

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"aira/internal/runner"
)

// AIRA-113 seam tier. Every test here drives the PRODUCTION evaluateOOMSteer
// through oomSteerDeps, so the policy is exercised without a cgroup or a /proc
// write. The anti-INERT half — that the production walker actually finds and
// moves a real process's oom_score_adj, and that the flip is the one the kernel
// would act on — is oomsteer_real_cgroup_linux_test.go, and nothing here
// substitutes for it.

const (
	steerNonDelegateScope = "CONFINE-builder-4242-abcdef"
	steerDelegateScope    = "CONFINE-@dr-suite-4243-abcdeg"
)

type steerWrite struct {
	dir string
	adj int
}

type steerHarness struct {
	slicePath   string
	sliceOK     bool
	sliceReason string

	current     int64
	reclaimable int64
	maximum     int64
	partsOK     bool
	partsReason string

	budgets  map[string]int64
	rss      map[string]int64
	applyErr map[string]error

	classErr error

	writes []steerWrite
	logs   []string
	now    time.Time
}

func newSteerHarness() *steerHarness {
	return &steerHarness{
		slicePath: "/sys/fs/cgroup/aira.slice",
		sliceOK:   true,
		maximum:   64 << 30,
		partsOK:   true,
		budgets:   map[string]int64{},
		rss:       map[string]int64{},
		applyErr:  map[string]error{},
		now:       time.Unix(1_000_000, 0),
	}
}

func (h *steerHarness) scopeDir(scopeID string) string {
	return filepath.Join(h.slicePath, confineScopeDirName(scopeID))
}

// full sets memory.current so the slice's non-reclaimable footprint is the given
// percentage of memory.max. reclaimable stays zero, so anon == current.
func (h *steerHarness) atPercent(pct int64) {
	h.current = h.maximum * pct / 100
	h.reclaimable = 0
}

func (h *steerHarness) deps() oomSteerDeps {
	return oomSteerDeps{
		resolveSlice: func() (string, bool, string) { return h.slicePath, h.sliceOK, h.sliceReason },
		readSliceParts: func(string) (int64, int64, int64, bool, string) {
			return h.current, h.reclaimable, h.maximum, h.partsOK, h.partsReason
		},
		readScopeCurrent: func(dir string) (int64, bool) {
			value, ok := h.rss[dir]
			return value, ok
		},
		budgets: func(string) map[string]int64 { return h.budgets },
		classAdj: func(scopeID string) (int, error) {
			if h.classErr != nil {
				return 0, h.classErr
			}
			return runner.ConfineClassOOMScoreAdj(scopeID)
		},
		apply: func(dir string, adj int) (runner.OOMScoreSteerResult, error) {
			if err := h.applyErr[dir]; err != nil {
				return runner.OOMScoreSteerResult{}, err
			}
			h.writes = append(h.writes, steerWrite{dir: dir, adj: adj})
			return runner.OOMScoreSteerResult{Cgroups: 1, PIDs: 1, Written: 1}, nil
		},
		enterPct:     oomSteerEnterPctDefault,
		exitPct:      oomSteerExitPctDefault,
		overrunFloor: oomSteerOverrunFloorDefault,
		steeredAdj:   runner.ConfineMaxOOMScoreAdj,
		now:          func() time.Time { return h.now },
		logf:         func(format string, args ...any) { h.logs = append(h.logs, fmt.Sprintf(format, args...)) },
		sleep:        func(context.Context, time.Duration) bool { return false },
	}
}

func (h *steerHarness) takeWrites() []steerWrite {
	writes := h.writes
	h.writes = nil
	return writes
}

func (h *steerHarness) assertWrites(t *testing.T, context string, want ...steerWrite) {
	t.Helper()
	got := h.takeWrites()
	if len(got) != len(want) {
		t.Fatalf("%s: %d writes %v, want %d %v", context, len(got), got, len(want), want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("%s: write %d = %+v, want %+v", context, i, got[i], want[i])
		}
	}
}

// TestOOMSteerRaisesOnlyTheOverBudgetScope is the core discrimination: on a full
// slice the scope outrunning its ledger charge is raised to 1000 and its
// compliant neighbour is not written AT ALL.
//
// Guard: asserting only "the offender was raised" would pass against an
// implementation that raised every scope on a full slice — which is the exact
// build that would leave the kernel's choice as arbitrary as before, since a
// uniform bias is no bias. Asserting the EXACT write list is what kills it.
func TestOOMSteerRaisesOnlyTheOverBudgetScope(t *testing.T) {
	h := newSteerHarness()
	h.atPercent(95)
	h.budgets[steerNonDelegateScope] = 8 << 30
	h.budgets[steerDelegateScope] = 32 << 30
	h.rss[h.scopeDir(steerNonDelegateScope)] = 30 << 30 // 22 GiB past its charge
	h.rss[h.scopeDir(steerDelegateScope)] = 20 << 30    // comfortably inside its charge
	state := newOOMSteerState("")

	evaluateOOMSteer(oomSteerEnforce, &state, h.deps())

	h.assertWrites(t, "first pass",
		steerWrite{dir: h.scopeDir(steerNonDelegateScope), adj: runner.ConfineMaxOOMScoreAdj})
	if state.applied[steerNonDelegateScope] != runner.ConfineMaxOOMScoreAdj {
		t.Fatalf("applied[offender] = %d, want %d", state.applied[steerNonDelegateScope], runner.ConfineMaxOOMScoreAdj)
	}
	if _, recorded := state.applied[steerDelegateScope]; recorded {
		t.Fatalf("the compliant delegate scope was recorded as steered: %v", state.applied)
	}

	// Idempotence: the same conditions must not re-write /proc four times a
	// second. A subsystem that rewrote on every tick would be a measurable cost
	// on a busy box for no change in outcome.
	evaluateOOMSteer(oomSteerEnforce, &state, h.deps())
	h.assertWrites(t, "second pass with unchanged conditions")
}

// TestOOMSteerDoesNothingWhileTheSliceHasRoom is the "when the aggregate is
// genuinely full" half of the ticket. A scope may sit past its ledger charge for
// perfectly good reasons; until the slice is near its own cap there is no OOM to
// steer and nothing is written.
func TestOOMSteerDoesNothingWhileTheSliceHasRoom(t *testing.T) {
	h := newSteerHarness()
	h.atPercent(50)
	h.budgets[steerNonDelegateScope] = 1 << 30
	h.rss[h.scopeDir(steerNonDelegateScope)] = 30 << 30
	state := newOOMSteerState("")

	evaluateOOMSteer(oomSteerEnforce, &state, h.deps())

	h.assertWrites(t, "half-full slice")
	if len(state.applied) != 0 {
		t.Fatalf("state recorded %v on a slice with room", state.applied)
	}
}

// TestOOMSteerFullnessIgnoresReclaimablePageCache: a slice at 99% of memory.max
// that is almost entirely page cache is nowhere near an OOM — the kernel drops
// that cache first. Steering on raw memory.current would raise the adj of every
// job on the box during any large build.
//
// Guard: the first half alone passes against an implementation with no
// fullness gate at all, so the second half re-runs the identical scope with the
// SAME memory.current and only the reclaimable split changed.
func TestOOMSteerFullnessIgnoresReclaimablePageCache(t *testing.T) {
	h := newSteerHarness()
	h.current = h.maximum * 99 / 100
	h.reclaimable = h.current - (1 << 30) // 1 GiB genuinely anonymous
	h.budgets[steerNonDelegateScope] = 1 << 20
	h.rss[h.scopeDir(steerNonDelegateScope)] = 30 << 30
	state := newOOMSteerState("")

	evaluateOOMSteer(oomSteerEnforce, &state, h.deps())
	h.assertWrites(t, "99% of memory.max, but reclaimable")

	h.reclaimable = 0
	evaluateOOMSteer(oomSteerEnforce, &state, h.deps())
	h.assertWrites(t, "the same memory.current, now anonymous",
		steerWrite{dir: h.scopeDir(steerNonDelegateScope), adj: runner.ConfineMaxOOMScoreAdj})
}

// TestOOMSteerHysteresisHoldsBetweenTheThresholds pins BOTH halves of the band.
//
// Guard: a single-threshold implementation passes the "restores below the exit"
// half on its own. The middle assertion — that 85% (below enter, above exit)
// keeps a raised scope raised — is the only one that fails against it, and it is
// what stops a scope flapping between 500 and 1000 several times a second on
// ordinary jitter.
func TestOOMSteerHysteresisHoldsBetweenTheThresholds(t *testing.T) {
	h := newSteerHarness()
	h.atPercent(95)
	h.budgets[steerNonDelegateScope] = 1 << 30
	h.rss[h.scopeDir(steerNonDelegateScope)] = 30 << 30
	state := newOOMSteerState("")

	evaluateOOMSteer(oomSteerEnforce, &state, h.deps())
	h.assertWrites(t, "at 95%",
		steerWrite{dir: h.scopeDir(steerNonDelegateScope), adj: runner.ConfineMaxOOMScoreAdj})

	h.atPercent(85)
	evaluateOOMSteer(oomSteerEnforce, &state, h.deps())
	h.assertWrites(t, "at 85%, inside the band")
	if state.applied[steerNonDelegateScope] != runner.ConfineMaxOOMScoreAdj {
		t.Fatalf("the scope was un-steered inside the hysteresis band: %v", state.applied)
	}

	h.atPercent(70)
	evaluateOOMSteer(oomSteerEnforce, &state, h.deps())
	h.assertWrites(t, "at 70%, below the exit",
		steerWrite{dir: h.scopeDir(steerNonDelegateScope), adj: runner.ConfineOOMScoreAdj})
	if len(state.applied) != 0 {
		t.Fatalf("state still records %v after a restore", state.applied)
	}
}

// TestOOMSteerRestoresWhenTheScopeReturnsWithinItsBudget is the restore-down
// pass on the OTHER trigger: the slice stays full, but the scope stops being an
// offender (its charge caught up with its usage at the next admission refresh).
func TestOOMSteerRestoresWhenTheScopeReturnsWithinItsBudget(t *testing.T) {
	h := newSteerHarness()
	h.atPercent(95)
	h.budgets[steerDelegateScope] = 1 << 30
	h.rss[h.scopeDir(steerDelegateScope)] = 30 << 30
	state := newOOMSteerState("")

	evaluateOOMSteer(oomSteerEnforce, &state, h.deps())
	h.assertWrites(t, "over budget",
		steerWrite{dir: h.scopeDir(steerDelegateScope), adj: runner.ConfineMaxOOMScoreAdj})

	// The ledger caught up: the charge now covers the usage.
	h.budgets[steerDelegateScope] = 31 << 30
	evaluateOOMSteer(oomSteerEnforce, &state, h.deps())
	h.assertWrites(t, "back within budget",
		steerWrite{dir: h.scopeDir(steerDelegateScope), adj: runner.ConfineDelegateOOMScoreAdj})
	if len(state.applied) != 0 {
		t.Fatalf("state still records %v after the restore", state.applied)
	}
}

// TestOOMSteerRestoresToTheSCOPESOWNClassBaseline: a delegate scope must come
// back to 800, never to the non-delegate 500.
//
// Guard: restoring every scope to one hardcoded baseline is a plausible-looking
// implementation that would silently PROMOTE every steered --delegate-ram suite
// into the protected class for the rest of its life — a weakening of AIRA-27
// dressed up as a restore. The delegate assertion is what kills it.
func TestOOMSteerRestoresToTheScopesOwnClassBaseline(t *testing.T) {
	for _, test := range []struct {
		name    string
		scopeID string
		want    int
	}{
		{"non-delegate", steerNonDelegateScope, runner.ConfineOOMScoreAdj},
		{"delegate", steerDelegateScope, runner.ConfineDelegateOOMScoreAdj},
	} {
		t.Run(test.name, func(t *testing.T) {
			h := newSteerHarness()
			h.atPercent(95)
			h.budgets[test.scopeID] = 1 << 30
			h.rss[h.scopeDir(test.scopeID)] = 30 << 30
			state := newOOMSteerState("")
			evaluateOOMSteer(oomSteerEnforce, &state, h.deps())
			h.assertWrites(t, "raise", steerWrite{dir: h.scopeDir(test.scopeID), adj: runner.ConfineMaxOOMScoreAdj})

			h.atPercent(10)
			evaluateOOMSteer(oomSteerEnforce, &state, h.deps())
			h.assertWrites(t, "restore", steerWrite{dir: h.scopeDir(test.scopeID), adj: test.want})
		})
	}
}

// TestOOMSteerNeverGoesBelowTheClassBaseline: whatever steer value is
// configured, a scope is never made LESS killable than AIRA-27 made it. This
// subsystem may sharpen that containment and must never weaken it.
//
// Guard: `want = deps.steeredAdj` with no floor is the obvious implementation
// and passes every other test in this file, because 1000 is above both
// baselines. It fails here.
func TestOOMSteerNeverGoesBelowTheClassBaseline(t *testing.T) {
	h := newSteerHarness()
	h.atPercent(95)
	h.budgets[steerDelegateScope] = 1 << 30
	h.rss[h.scopeDir(steerDelegateScope)] = 30 << 30
	deps := h.deps()
	deps.steeredAdj = 600 // below the delegate class baseline of 800
	state := newOOMSteerState("")

	evaluateOOMSteer(oomSteerEnforce, &state, deps)

	h.assertWrites(t, "a steer value under the class baseline")
	if len(state.applied) != 0 {
		t.Fatalf("state recorded %v; a delegate scope must never be lowered toward 600", state.applied)
	}
}

// TestOOMSteerDoesNotSteerAnAitestParentWhoseChildrenHoldTheCharge is the
// double-book guard, and it is the finding AIRA-29's own build review made in
// the opposite direction.
//
// A --delegate-ram suite's own waiter charges only the pinned framework
// overhead; its per-test `aira confine-reserve` sub-reservations carry the real
// bytes. The parent's memory.current is HIERARCHICAL and already includes every
// byte they allocated. Comparing 30 GiB of hierarchical usage against a 512 MiB
// overhead would mark the most compliant job on the machine as the offender —
// on EVERY full slice, which is exactly when getting it wrong costs a kill.
func TestOOMSteerDoesNotSteerAnAitestParentWhoseChildrenHoldTheCharge(t *testing.T) {
	server := NewServer(Paths{})
	queue := &sliceQueue{path: "/slice", server: server}
	queue.waiters = []*admitWaiter{
		{seq: 1, state: admitGranted, accounted: true, scopeID: steerDelegateScope, reserve: 512 << 20},
		{seq: 2, state: admitGranted, accounted: true, parentScopeID: steerDelegateScope, reserve: 16 << 30},
		{seq: 3, state: admitGranted, accounted: true, parentScopeID: steerDelegateScope, reserve: 16 << 30},
	}
	server.admitQueues["/slice"] = queue

	budgets := server.admitScopeBudgets("/slice")
	want := int64(512<<20) + int64(32<<30)
	if budgets[steerDelegateScope] != want {
		t.Fatalf("budget = %d, want %d (the parent's own overhead plus both sub-reservations)", budgets[steerDelegateScope], want)
	}

	h := newSteerHarness()
	h.slicePath = "/slice"
	h.atPercent(95)
	h.budgets = budgets
	h.rss[h.scopeDir(steerDelegateScope)] = 30 << 30
	state := newOOMSteerState("")

	evaluateOOMSteer(oomSteerEnforce, &state, h.deps())

	h.assertWrites(t, "a compliant suite whose children hold the charge")
}

// TestAdmitScopeBudgetsCountsOnlyWhatItCanEstablish covers the populations the
// budget map must NOT invent an entry for.
func TestAdmitScopeBudgetsCountsOnlyWhatItCanEstablish(t *testing.T) {
	server := NewServer(Paths{})
	queue := &sliceQueue{path: "/slice", server: server}
	orphanParent := "CONFINE-elsewhere-99-zzz"
	queue.waiters = []*admitWaiter{
		// Granted, accounted, scope-backed: the one population that counts.
		{seq: 1, state: admitGranted, accounted: true, scopeID: steerNonDelegateScope, reserve: 4 << 30},
		// Granted but NOT accounted: contributes nothing to the ledger, so it has
		// no budget to be over.
		{seq: 2, state: admitGranted, scopeID: "CONFINE-unaccounted-2-b", reserve: 4 << 30},
		// Still queued: not running, no scope.
		{seq: 3, state: admitQueued, scopeID: "CONFINE-queued-3-c", reserve: 4 << 30},
		// A plain `aira admit` waiter: creates no cgroup at all.
		{seq: 4, state: admitGranted, accounted: true, reserve: 4 << 30},
		// A sub-reservation whose parent is not a scope-backed waiter here.
		{seq: 5, state: admitGranted, accounted: true, parentScopeID: orphanParent, reserve: 4 << 30},
	}
	server.admitQueues["/slice"] = queue

	budgets := server.admitScopeBudgets("/slice")
	if len(budgets) != 1 {
		t.Fatalf("budgets = %v, want exactly the one granted scope-backed waiter", budgets)
	}
	if budgets[steerNonDelegateScope] != 4<<30 {
		t.Fatalf("budgets[%s] = %d, want %d", steerNonDelegateScope, budgets[steerNonDelegateScope], int64(4<<30))
	}
	if _, invented := budgets[orphanParent]; invented {
		t.Fatalf("a sub-reservation invented a budget for an absent parent: %v", budgets)
	}
	if got := server.admitScopeBudgets("/no-such-slice"); got != nil {
		t.Fatalf("budgets for an unknown slice = %v, want nil", got)
	}
}

// TestAdmitScopeBudgetsUsesTheDynamicChargeNotTheFrozenReserve: the budget must
// be what the ledger actually holds right now. A waiter whose AIRA-29 charge has
// fallen to 2 GiB is NOT entitled to the 33 GiB it was granted on.
//
// Guard: reading waiter.reserve is the obvious implementation and passes every
// other test here, because those waiters are untracked and ledgerCharge()
// returns the reserve for them.
func TestAdmitScopeBudgetsUsesTheDynamicChargeNotTheFrozenReserve(t *testing.T) {
	server := NewServer(Paths{})
	queue := &sliceQueue{path: "/slice", server: server}
	queue.waiters = []*admitWaiter{{
		seq: 1, state: admitGranted, accounted: true, scopeID: steerNonDelegateScope,
		reserve: 33 << 30, effectiveCharge: 2 << 30, chargeTracked: true,
	}}
	server.admitQueues["/slice"] = queue

	if got := server.admitScopeBudgets("/slice")[steerNonDelegateScope]; got != 2<<30 {
		t.Fatalf("budget = %d, want the tracked charge %d, not the frozen reserve", got, int64(2<<30))
	}
}

// TestOOMSteerHoldsEverythingWhenAReadIsUnevaluated: an unestablished reading
// must never be turned into a restore. Restoring on a failed read would undo a
// correct raise on the strength of no evidence at all — the fabricated-fact
// failure mode AIRA forbids.
func TestOOMSteerHoldsEverythingWhenAReadIsUnevaluated(t *testing.T) {
	for _, test := range []struct {
		name   string
		break_ func(*steerHarness)
	}{
		{"slice unresolvable", func(h *steerHarness) { h.sliceOK, h.sliceReason = false, "slice-not-found" }},
		{"slice memory unevaluated", func(h *steerHarness) { h.partsOK, h.partsReason = false, "read-error" }},
		{"scope memory.current unevaluated", func(h *steerHarness) { delete(h.rss, h.scopeDir(steerNonDelegateScope)) }},
	} {
		t.Run(test.name, func(t *testing.T) {
			h := newSteerHarness()
			h.atPercent(95)
			h.budgets[steerNonDelegateScope] = 1 << 30
			h.rss[h.scopeDir(steerNonDelegateScope)] = 30 << 30
			state := newOOMSteerState("")
			evaluateOOMSteer(oomSteerEnforce, &state, h.deps())
			h.assertWrites(t, "raise", steerWrite{dir: h.scopeDir(steerNonDelegateScope), adj: runner.ConfineMaxOOMScoreAdj})

			test.break_(h)
			evaluateOOMSteer(oomSteerEnforce, &state, h.deps())
			h.assertWrites(t, "unevaluated pass")
			if state.applied[steerNonDelegateScope] != runner.ConfineMaxOOMScoreAdj {
				t.Fatalf("an unevaluated reading dropped the established steer: %v", state.applied)
			}
		})
	}
}

// TestOOMSteerRestoresAScopeThatLeavesTheLedger: a job whose lease is released
// while it is still alive (its scope directory outliving its waiter) must be put
// back to its class baseline rather than left at 1000 forever.
func TestOOMSteerRestoresAScopeThatLeavesTheLedger(t *testing.T) {
	h := newSteerHarness()
	h.atPercent(95)
	h.budgets[steerNonDelegateScope] = 1 << 30
	h.rss[h.scopeDir(steerNonDelegateScope)] = 30 << 30
	state := newOOMSteerState("")
	evaluateOOMSteer(oomSteerEnforce, &state, h.deps())
	h.takeWrites()

	delete(h.budgets, steerNonDelegateScope)
	evaluateOOMSteer(oomSteerEnforce, &state, h.deps())
	h.assertWrites(t, "after the waiter left the ledger",
		steerWrite{dir: h.scopeDir(steerNonDelegateScope), adj: runner.ConfineOOMScoreAdj})
	if len(state.applied) != 0 {
		t.Fatalf("state still records %v", state.applied)
	}
}

// TestOOMSteerForgetsAVanishedScope: when the scope directory itself is gone the
// walk fails, and the subsystem must forget the scope rather than retry the
// write four times a second forever.
func TestOOMSteerForgetsAVanishedScope(t *testing.T) {
	h := newSteerHarness()
	h.atPercent(95)
	h.budgets[steerNonDelegateScope] = 1 << 30
	h.rss[h.scopeDir(steerNonDelegateScope)] = 30 << 30
	h.applyErr[h.scopeDir(steerNonDelegateScope)] = errors.New("no such file or directory")
	state := newOOMSteerState("")

	evaluateOOMSteer(oomSteerEnforce, &state, h.deps())

	if len(state.applied) != 0 {
		t.Fatalf("state recorded %v for a scope whose walk failed", state.applied)
	}
	if len(h.logs) != 1 || !strings.Contains(h.logs[0], "cannot steering") {
		t.Fatalf("logs = %v, want one line reporting the failed walk", h.logs)
	}
}

// TestOOMSteerObserveModeWritesNothing: observe must report the decision it
// would have made and touch no process. It also reports each transition ONCE,
// not on every tick, or a 4 Hz loop would be a log flood.
func TestOOMSteerObserveModeWritesNothing(t *testing.T) {
	h := newSteerHarness()
	h.atPercent(95)
	h.budgets[steerNonDelegateScope] = 1 << 30
	h.rss[h.scopeDir(steerNonDelegateScope)] = 30 << 30
	state := newOOMSteerState("")

	evaluateOOMSteer(oomSteerObserve, &state, h.deps())
	evaluateOOMSteer(oomSteerObserve, &state, h.deps())

	h.assertWrites(t, "observe mode")
	if len(h.logs) != 1 {
		t.Fatalf("logs = %v, want exactly one transition line", h.logs)
	}
	if !strings.Contains(h.logs[0], "would be steering") {
		t.Fatalf("log = %q, want it to say the write did not happen", h.logs[0])
	}
}

// TestOOMSteerForgetsEverythingWhenTheGovernedSliceChanges: a sample is a fact
// about a slice, never about the machine.
func TestOOMSteerForgetsEverythingWhenTheGovernedSliceChanges(t *testing.T) {
	h := newSteerHarness()
	h.atPercent(95)
	h.budgets[steerNonDelegateScope] = 1 << 30
	h.rss[h.scopeDir(steerNonDelegateScope)] = 30 << 30
	state := newOOMSteerState("")
	evaluateOOMSteer(oomSteerEnforce, &state, h.deps())
	h.takeWrites()

	h.slicePath = "/sys/fs/cgroup/other.slice"
	h.rss[h.scopeDir(steerNonDelegateScope)] = 30 << 30
	evaluateOOMSteer(oomSteerEnforce, &state, h.deps())

	if state.path != "/sys/fs/cgroup/other.slice" {
		t.Fatalf("state.path = %q, want the new slice", state.path)
	}
	h.assertWrites(t, "after the slice changed",
		steerWrite{dir: h.scopeDir(steerNonDelegateScope), adj: runner.ConfineMaxOOMScoreAdj})
}

// TestOOMSteerLeavesAScopeAloneWhenTheClassPolicyIsUnusable: a malformed
// AIRA_CONFINE_OOM_SCORE_ADJ override means the daemon cannot know what baseline
// the launcher used. Steering against a guess would be steering against the
// wrong number, and restoring against one would be worse.
func TestOOMSteerLeavesAScopeAloneWhenTheClassPolicyIsUnusable(t *testing.T) {
	h := newSteerHarness()
	h.atPercent(95)
	h.budgets[steerNonDelegateScope] = 1 << 30
	h.rss[h.scopeDir(steerNonDelegateScope)] = 30 << 30
	state := newOOMSteerState("")
	evaluateOOMSteer(oomSteerEnforce, &state, h.deps())
	h.assertWrites(t, "raise", steerWrite{dir: h.scopeDir(steerNonDelegateScope), adj: runner.ConfineMaxOOMScoreAdj})

	h.classErr = errors.New("E_CONFINE_ARGUMENT_INVALID: unusable override")
	evaluateOOMSteer(oomSteerEnforce, &state, h.deps())
	h.assertWrites(t, "with an unusable class policy")
	if state.applied[steerNonDelegateScope] != runner.ConfineMaxOOMScoreAdj {
		t.Fatalf("an unusable class policy dropped the established steer: %v", state.applied)
	}
}

// TestOOMSteerUnevaluatedLogIsRateLimited: the loop runs at 4 Hz, so a
// persistently unreadable slice must not write four log lines a second.
func TestOOMSteerUnevaluatedLogIsRateLimited(t *testing.T) {
	h := newSteerHarness()
	h.partsOK, h.partsReason = false, "read-error"
	state := newOOMSteerState("")

	for i := 0; i < 4; i++ {
		evaluateOOMSteer(oomSteerEnforce, &state, h.deps())
		h.now = h.now.Add(time.Second)
	}
	if len(h.logs) != 1 {
		t.Fatalf("logs = %v, want one line inside the rate-limit window", h.logs)
	}
	h.now = h.now.Add(2 * oomSteerUnevaluatedLogInterval)
	evaluateOOMSteer(oomSteerEnforce, &state, h.deps())
	if len(h.logs) != 2 {
		t.Fatalf("logs = %v, want a second line after the window elapsed", h.logs)
	}
}

func TestOOMSteerFullBand(t *testing.T) {
	const maximum = int64(1000)
	for _, test := range []struct {
		name string
		was  bool
		anon int64
		want bool
	}{
		{"below both, was not full", false, 700, false},
		{"below both, was full", true, 700, false},
		{"inside the band, was not full", false, 850, false},
		{"inside the band, was full", true, 850, true},
		{"at the enter threshold", false, 900, true},
		{"at the exit threshold, was full", true, 800, true},
		{"unestablished maximum", false, 900, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			max := maximum
			if test.name == "unestablished maximum" {
				max = 0
			}
			if got := oomSteerFull(test.was, test.anon, max, 90, 80); got != test.want {
				t.Fatalf("oomSteerFull(%v, %d, %d) = %v, want %v", test.was, test.anon, max, got, test.want)
			}
		})
	}
}

func TestRunOOMSteerParksWhenOffOrMisconfigured(t *testing.T) {
	for _, test := range []struct {
		name     string
		mode     oomSteerMode
		interval time.Duration
		mutate   func(*oomSteerDeps)
	}{
		{"off", oomSteerOff, defaultOOMSteerInterval, nil},
		{"empty mode", "", defaultOOMSteerInterval, nil},
		{"non-positive interval", oomSteerEnforce, 0, nil},
		{"exit above enter", oomSteerEnforce, defaultOOMSteerInterval, func(d *oomSteerDeps) { d.exitPct = 95 }},
		{"steer below the class floor", oomSteerEnforce, defaultOOMSteerInterval, func(d *oomSteerDeps) { d.steeredAdj = 100 }},
		{"missing apply", oomSteerEnforce, defaultOOMSteerInterval, func(d *oomSteerDeps) { d.apply = nil }},
		{"missing budgets", oomSteerEnforce, defaultOOMSteerInterval, func(d *oomSteerDeps) { d.budgets = nil }},
	} {
		t.Run(test.name, func(t *testing.T) {
			h := newSteerHarness()
			h.atPercent(95)
			h.budgets[steerNonDelegateScope] = 1 << 30
			h.rss[h.scopeDir(steerNonDelegateScope)] = 30 << 30
			deps := h.deps()
			if test.mutate != nil {
				test.mutate(&deps)
			}
			ctx, cancel := context.WithCancel(context.Background())
			cancel()
			runOOMSteer(ctx, test.mode, test.interval, deps)
			if len(h.writes) != 0 {
				t.Fatalf("a parked loop wrote %v", h.writes)
			}
		})
	}
}

func TestOOMSteerConfigFromEnv(t *testing.T) {
	t.Setenv("AIRA_DAEMON_OOM_STEER_MODE", "")
	os.Unsetenv("AIRA_DAEMON_OOM_STEER_MODE")
	t.Setenv("AIRA_DAEMON_OOM_STEER_INTERVAL", "")
	os.Unsetenv("AIRA_DAEMON_OOM_STEER_INTERVAL")
	mode, interval, err := oomSteerConfigFromEnv()
	if err != nil || mode != oomSteerOff || interval != defaultOOMSteerInterval {
		t.Fatalf("unset = (%q, %s, %v), want (off, %s, nil)", mode, interval, err, defaultOOMSteerInterval)
	}

	// A malformed interval must NOT refuse to start a daemon that never asked for
	// the subsystem: off is exactly today's behaviour, and it must stay reachable.
	t.Setenv("AIRA_DAEMON_OOM_STEER_INTERVAL", "not-a-duration")
	if mode, _, err := oomSteerConfigFromEnv(); err != nil || mode != oomSteerOff {
		t.Fatalf("off with a malformed interval = (%q, %v), want (off, nil)", mode, err)
	}

	t.Setenv("AIRA_DAEMON_OOM_STEER_MODE", "enforce")
	if _, _, err := oomSteerConfigFromEnv(); err == nil || !strings.Contains(err.Error(), "E_CONFIG_INVALID") {
		t.Fatalf("enforce with a malformed interval = %v, want a stable E_CONFIG_INVALID code", err)
	}
	// At or above the admission charge refresh the loop could only ever see
	// readings the ledger had already absorbed, so it is refused rather than
	// silently accepted.
	t.Setenv("AIRA_DAEMON_OOM_STEER_INTERVAL", "1s")
	if _, _, err := oomSteerConfigFromEnv(); err == nil {
		t.Fatal("a 1s interval was accepted, but it cannot outrun the charge refresh it exists to beat")
	}
	t.Setenv("AIRA_DAEMON_OOM_STEER_INTERVAL", "100ms")
	mode, interval, err = oomSteerConfigFromEnv()
	if err != nil || mode != oomSteerEnforce || interval != 100*time.Millisecond {
		t.Fatalf("enforce/100ms = (%q, %s, %v)", mode, interval, err)
	}

	t.Setenv("AIRA_DAEMON_OOM_STEER_MODE", "sometimes")
	if _, _, err := oomSteerConfigFromEnv(); err == nil || !strings.Contains(err.Error(), "E_CONFIG_INVALID") {
		t.Fatalf("an unrecognised mode = %v, want a stable E_CONFIG_INVALID code", err)
	}
}

func TestConfineClassOOMScoreAdjFollowsTheLauncherPolicy(t *testing.T) {
	nonDelegate, err := runner.ConfineClassOOMScoreAdj(steerNonDelegateScope)
	if err != nil || nonDelegate != runner.ConfineOOMScoreAdj {
		t.Fatalf("non-delegate = (%d, %v), want %d", nonDelegate, err, runner.ConfineOOMScoreAdj)
	}
	delegate, err := runner.ConfineClassOOMScoreAdj(steerDelegateScope)
	if err != nil || delegate != runner.ConfineDelegateOOMScoreAdj {
		t.Fatalf("delegate = (%d, %v), want %d", delegate, err, runner.ConfineDelegateOOMScoreAdj)
	}
	t.Setenv("AIRA_CONFINE_OOM_SCORE_ADJ", "600")
	t.Setenv("AIRA_CONFINE_OOM_SCORE_ADJ_DELEGATE", "900")
	if got, err := runner.ConfineClassOOMScoreAdj(steerNonDelegateScope); err != nil || got != 600 {
		t.Fatalf("overridden non-delegate = (%d, %v), want 600", got, err)
	}
	if got, err := runner.ConfineClassOOMScoreAdj(steerDelegateScope); err != nil || got != 900 {
		t.Fatalf("overridden delegate = (%d, %v), want 900", got, err)
	}
	t.Setenv("AIRA_CONFINE_OOM_SCORE_ADJ_DELEGATE", "550")
	if _, err := runner.ConfineClassOOMScoreAdj(steerDelegateScope); err == nil {
		t.Fatal("a delegate baseline below the non-delegate one was accepted; the class ordering is the containment")
	}
}
