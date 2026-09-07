//go:build linux

package runner

import (
	"context"
	"errors"
	"os/exec"
	"sync"
	"testing"
	"time"
)

// AIRA-143 — the PTY capture-teardown caller's OWN leaf-only pre-gate, the last
// of killScope's four callers still blind to a nested-cgroup descendant.
//
// AIRA-140 corrected killScope's own refusal gate to require TWO agreeing reads
// (leaf `cgroup.procs` via Members() AND subtree-aware `cgroup.events populated`
// via Empty()). Its Resolution claimed that one change fixed all four callers.
// It did not fix this one: the PTY branch of capture teardown short-circuited on
// `len(scope.Members()) > 0` — a LEAF read — BEFORE killScope was ever entered,
// so a job whose processes live in a child cgroup it created inside its own
// scope (aitest / `--delegate-ram` / `podman --cgroups=split`) read "nothing to
// kill" and its descendant was never reached. Two gates in front of one decision
// is what left the hole; the fix removes the outer one.
//
// REACHABILITY, and why this lane is a fake scope rather than a real cgroup.
// quiescePTYScope runs BEFORE this teardown and writes `cgroup.kill`
// unconditionally, and a real cgroup.kill is recursive — so on a healthy scope
// there is by construction nothing left for the teardown to reach, and a
// real-cgroup lane could not tell the old code from the new. The teardown exists
// precisely for the quiesce that did NOT work (its write failed, or its
// confirmation timed out, which is what sets ptyCleanupErr and forces the
// abandon). A scope whose FIRST cgroup.kill write fails is that state exactly,
// and it is the only honest way to put a live nested descendant in front of this
// call site. Everything else in these tests is the real Launch path: a real
// child on a real PTY, real drains, the real teardown branch.
//
// verifies: AIRA-143

// errPTYQuiesceKillWrite is the failed `cgroup.kill` write that leaves the PTY
// quiesce unfinished and hands the still-populated scope to the teardown.
var errPTYQuiesceKillWrite = errors.New("cgroup.kill write failed")

// ptyNestedScope is the ticket's shape exactly: "a descendant lingering in a
// CHILD cgroup after the PTY leader itself has exited".
//
// Its two reads are INDEPENDENT sources, as a real cgroup's are, and that is the
// whole point — nestedWorkloadScope proved the same disagreement for AIRA-138
// and AIRA-140, and `livenessScope` structurally cannot, because its Empty() is
// derived from the same membersLocked() its Members() returns:
//
//   - Members() is LEAF cgroup.procs: the leader while it is alive (adopted from
//     the real child, liveness-checked exactly as livenessScope does it), and
//     empty once it exits — which is why the leaf read alone says "nothing to
//     kill" at teardown time;
//   - Empty() is cgroup.events `populated`, SUBTREE-aware: false while the
//     nested descendant lives, whatever the leaf says.
//
// Keeping the leader in the leaf while it is alive also keeps the launch's
// membership monitor honest about it, so no leaf-empty-with-a-live-leader
// migration verdict is manufactured by the fake itself.
//
// The kill write is counted, and can be made to FAIL once, which is what leaves
// a live nested descendant in front of the capture teardown at all.
type ptyNestedScope struct {
	mu sync.Mutex
	// leader is the launched child's boot-aware identity, adopted by startFn.
	leader PIDIdentity
	// subtreePopulated is the `cgroup.events` reading: the descendant alive in a
	// CHILD cgroup of this scope, independent of the leaf.
	subtreePopulated bool
	failFirstKill    bool
	kills            int
	terminated       bool
}

func (s *ptyNestedScope) Reference() string { return "/aira143-pty-nested-scope" }
func (s *ptyNestedScope) FD() int           { return -1 }

func (s *ptyNestedScope) adopt(identity PIDIdentity) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.leader = identity
}

// leafLocked is livenessScope's own leaf derivation: the leader is in
// cgroup.procs for exactly as long as that boot-aware identity is alive.
func (s *ptyNestedScope) leafLocked() []int {
	if s.leader.PID <= 0 || s.leader.StartTick == 0 {
		return nil
	}
	data, err := readProcStatFn(s.leader.PID)
	if err != nil {
		return nil
	}
	tick, ok := processStartTickFromStat(data)
	if !ok || tick != s.leader.StartTick {
		return nil
	}
	return []int{s.leader.PID}
}

func (s *ptyNestedScope) Members() ([]int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.leafLocked(), nil
}

// Empty is the SUBTREE-aware read and is deliberately NOT derived from
// leafLocked alone: the nested descendant keeps it populated after the leader
// has gone, which is the disagreement the whole ticket is about.
func (s *ptyNestedScope) Empty() (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.leafLocked()) == 0 && !s.subtreePopulated, nil
}

func (s *ptyNestedScope) Terminate([]int) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.terminated = true
	return nil
}

// Kill models cgroup.kill's documented RECURSION — one write reaches the whole
// subtree — with a first write that can fail, which is the state that leaves
// something for the capture teardown to find.
func (s *ptyNestedScope) Kill() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.kills++
	if s.failFirstKill && s.kills == 1 {
		return errPTYQuiesceKillWrite
	}
	s.subtreePopulated = false
	return nil
}

func (s *ptyNestedScope) Remove() error { return nil }

func (s *ptyNestedScope) state() (kills int, terminated, populated bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.kills, s.terminated, s.subtreePopulated
}

// aira143BootID is the boot-aware half of the leader identity the fake adopts.
func aira143BootID(t *testing.T) string {
	t.Helper()
	bootID, err := readBootIDFn()
	if err != nil || bootID == "" {
		t.Skipf("kernel boot id unavailable: %v", err)
	}
	return bootID
}

type ptyNestedBackend struct{ scope *ptyNestedScope }

func (b *ptyNestedBackend) Probe(context.Context) error                   { return nil }
func (b *ptyNestedBackend) Create(context.Context, string) (Scope, error) { return b.scope, nil }
func (b *ptyNestedBackend) Open(context.Context, string) (Scope, error)   { return b.scope, nil }

// aira143PTYRunner is a real Runner over the injected scope. startFn strips
// clone3's CLONE_INTO_CGROUP because the fake has no cgroup fd; nothing else
// about the launch is substituted.
func aira143PTYRunner(t *testing.T, scope *ptyNestedScope) *Runner {
	t.Helper()
	bootID := aira143BootID(t)
	r, err := New(Config{
		CommonDir: t.TempDir(),
		Backend:   &ptyNestedBackend{scope: scope},
		Grace:     aira126Scale(2 * time.Second),
		TermGrace: 20 * time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	r.startFn = func(cmd *exec.Cmd) error {
		cmd.SysProcAttr.UseCgroupFD, cmd.SysProcAttr.CgroupFD = false, 0
		if err := cmd.Start(); err != nil {
			return err
		}
		scope.adopt(PIDIdentity{PID: cmd.Process.Pid, StartTick: processStartTick(cmd.Process.Pid), BootID: bootID})
		return nil
	}
	return r
}

// aira143LaunchPTY runs a trivial PTY job to completion and returns the
// published record. The launch reports U_RUN_RECONCILE_REQUIRED because the
// quiesce write failed; that error is the PRECONDITION of these tests, not the
// thing under test, so its absence is the failure worth shouting about.
func aira143LaunchPTY(t *testing.T, r *Runner) *RunRecord {
	t.Helper()
	record, err := r.Launch(context.Background(), Request{Argv: []string{"/bin/true"}, PTY: true})
	if record == nil {
		t.Fatalf("no record was published: err=%v", err)
	}
	if err == nil || !containsString(record.ErrorCodes, "U_RUN_RECONCILE_REQUIRED") {
		t.Fatalf("vacuous: the PTY quiesce did not fail, so the capture-teardown branch was never entered with a live scope: err=%v record=%+v", err, record)
	}
	return record
}

// T1. THE DEFECT. The PTY quiesce's cgroup.kill write fails, leaving a fully
// busy nested workload — leaf cgroup.procs empty, cgroup.events populated —
// in front of the capture teardown. The teardown must reach it.
//
// Against the old leaf-only pre-check this fails on the FIRST assertion: the
// scope records exactly one kill write (the quiesce's failed one), the teardown
// never entered killScope at all, and the record carries E_RUN_SCOPE_HANDOFF
// with no ScopeKill instead of a reclaimed descendant.
//
// verifies: AIRA-143
func TestAIRA143PTYCaptureTeardownKillsALeafEmptySubtreePopulatedScope(t *testing.T) {
	scope := &ptyNestedScope{subtreePopulated: true, failFirstKill: true}
	record := aira143LaunchPTY(t, aira143PTYRunner(t, scope))

	kills, terminated, populated := scope.state()
	if kills < 2 {
		t.Fatalf("the PTY capture teardown never reached the scope: cgroup.kill writes=%d (only the failed quiesce write) — a leaf-only pre-check reads a nested job as 'nothing to kill', so the descendant survives the run: record=%+v", kills, record)
	}
	if populated {
		t.Fatalf("the nested descendant outlived the capture teardown: record=%+v", record)
	}
	// The ticket's care point, inherited from AIRA-140: Terminate takes LEAF
	// pids, which are empty in this shape, so the nested arm must go straight to
	// the recursive cgroup.kill rather than signal nothing and wait the grace.
	if terminated {
		t.Fatalf("the teardown called Terminate on a leaf-empty scope: it signalled nothing and then waited the TERM grace before the kill that actually reaches the job")
	}
	if !record.ScopeKill.Requested || !record.ScopeKill.Started || !record.ScopeKill.Completed {
		t.Fatalf("a delivered kill was not attested: %+v", record.ScopeKill)
	}
	if record.ScopeIntegrity != ScopeDescendantKilled || !containsString(record.ErrorCodes, "E_RUN_DESCENDANT_KILLED") {
		t.Fatalf("a reclaimed nested descendant was not reported: integrity=%q codes=%v", record.ScopeIntegrity, record.ErrorCodes)
	}
	if record.CleanSuccess() {
		t.Fatalf("a run that had to kill a descendant reported a clean success: %+v", record)
	}
}

// T2. THE ANTI-OVER-CORRECTION. Same failed quiesce, same forced abandon, but
// the scope is empty by BOTH reads. Dropping the pre-check must not turn the
// teardown into an unconditional kill-and-claim: no second write may be made,
// and no descendant kill may be attested for a scope that had nothing in it.
// Without this, "the teardown always reports a reclaimed descendant" would pass
// T1 while fabricating evidence on every forced PTY abandon.
//
// verifies: AIRA-143
func TestAIRA143PTYCaptureTeardownStillRefusesToSignalAScopeBothReadsCallEmpty(t *testing.T) {
	scope := &ptyNestedScope{subtreePopulated: false, failFirstKill: true}
	record := aira143LaunchPTY(t, aira143PTYRunner(t, scope))

	kills, terminated, _ := scope.state()
	if kills != 1 || terminated {
		t.Fatalf("the teardown signalled a scope both reads call empty: cgroup.kill writes=%d (want 1, the quiesce's own) terminated=%v", kills, terminated)
	}
	if record.ScopeKill.Started || record.ScopeKill.Completed || containsString(record.ErrorCodes, "E_RUN_DESCENDANT_KILLED") {
		t.Fatalf("a kill that delivered nothing was reported as a reclaimed descendant: ScopeKill=%+v codes=%v", record.ScopeKill, record.ErrorCodes)
	}
	// The old pre-check's `else if forced` arm: a forced capture abandon over a
	// scope we cannot attest is a handoff we cannot verify. It must survive the
	// pre-check's removal.
	if record.ScopeIntegrity == ScopeDescendantKilled || !containsString(record.ErrorCodes, "E_RUN_SCOPE_HANDOFF") {
		t.Fatalf("the unverified-handoff arm was lost with the pre-check: integrity=%q codes=%v", record.ScopeIntegrity, record.ErrorCodes)
	}
}
