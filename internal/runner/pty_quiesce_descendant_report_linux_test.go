//go:build linux

package runner

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

// AIRA-146 — quiescePTYScope's descendant REPORT, the last leaf-only read left
// in the PTY path after AIRA-140 (killScope's own gate) and AIRA-143 (the PTY
// capture-teardown caller's pre-gate).
//
// quiescePTYScope is not a kill gate: it writes cgroup.kill unconditionally, and
// a real cgroup.kill is recursive, so a descendant living in a child cgroup of
// the run scope genuinely IS reclaimed there. What its returned bool decides is
// only whether that reclamation is RECORDED (ScopeDescendantKilled +
// E_RUN_DESCENDANT_KILLED). Deriving that bool from a LEAF cgroup.procs read
// alone made it false for exactly the workload shape whose processes live in
// child cgroups it created inside its own scope (aitest / --delegate-ram /
// podman --cgroups=split), which reads leaf-empty while fully busy — so a PTY
// run that silently reclaimed a live nested descendant published a CLEAN
// SUCCESS. Not a missed kill: a record that lies by omission.
//
// The fix mirrors AIRA-140's two-agreeing-reads pattern, for reporting rather
// than for gating: either read observing a population reports it, only two
// readable agreeing empty reads may conclude nothing was here, and no positive
// evidence with an unreadable read is unevaluated rather than clean.
//
// verifies: AIRA-146

// aira146LaunchQuiescedPTY runs a trivial PTY job over the injected scope and
// returns the published record.
//
// The PRECONDITION of every full-launch test here is that the quiesce SUCCEEDED
// — its cgroup.kill write and its emptiness confirmation both completed. That is
// what makes this the quiesce's own REPORT under test rather than AIRA-143's
// failed-quiesce capture teardown, which is reached only when ptyCleanupErr is
// set (U_RUN_RECONCILE_REQUIRED) and which writes its own second cgroup.kill.
// Asserting the precondition explicitly is what stops these tests from passing
// vacuously through the neighbouring path.
func aira146LaunchQuiescedPTY(t *testing.T, scope *ptyNestedScope) *RunRecord {
	t.Helper()
	record, err := aira143PTYRunner(t, scope).Launch(context.Background(), Request{Argv: []string{"/bin/true"}, PTY: true})
	if record == nil {
		t.Fatalf("no record was published: err=%v", err)
	}
	if containsString(record.ErrorCodes, "U_RUN_RECONCILE_REQUIRED") {
		t.Fatalf("vacuous: the PTY quiesce did not complete, so this is AIRA-143's capture-teardown path and not the quiesce report under test: err=%v record=%+v", err, record)
	}
	if kills, _, _ := scope.state(); kills != 1 {
		t.Fatalf("vacuous: cgroup.kill writes=%d, want exactly 1 (the quiesce's own) — a second write means the capture teardown, not the quiesce, is what this test observed: record=%+v", kills, record)
	}
	return record
}

// T1. THE DEFECT. The PTY leader has exited (leaf cgroup.procs empty) while a
// descendant is still alive in a CHILD cgroup (cgroup.events populated). The
// quiesce's unconditional, recursive cgroup.kill reclaims it — and the record
// must SAY SO.
//
// Against the old leaf-only read this fails on the report assertions: exactly
// one kill write is still made and the descendant is still reclaimed (the write
// was never gated), but hadDescendants is false, so the run publishes a clean
// success with no ScopeKill and no E_RUN_DESCENDANT_KILLED — a live nested
// descendant killed and never mentioned.
//
// verifies: AIRA-146
func TestAIRA146PTYQuiesceReportsAReclaimedNestedDescendant(t *testing.T) {
	scope := &ptyNestedScope{subtreePopulated: true}
	record := aira146LaunchQuiescedPTY(t, scope)

	if _, _, populated := scope.state(); populated {
		t.Fatalf("vacuous: the nested descendant was never reclaimed, so there was nothing to report: record=%+v", record)
	}
	if record.ScopeIntegrity != ScopeDescendantKilled || !containsString(record.ErrorCodes, "E_RUN_DESCENDANT_KILLED") {
		t.Fatalf("a reclaimed nested descendant was not reported: integrity=%q codes=%v — a leaf-only read calls a busy nested workload empty, so its reclamation is silent", record.ScopeIntegrity, record.ErrorCodes)
	}
	if !record.ScopeKill.Requested || !record.ScopeKill.Started || !record.ScopeKill.Completed {
		t.Fatalf("a delivered kill was not attested: %+v", record.ScopeKill)
	}
	if record.CleanSuccess() {
		t.Fatalf("a run that killed a live descendant published a clean success: %+v", record)
	}
}

// T2. THE ANTI-OVER-CORRECTION. Same successful quiesce, same unconditional
// cgroup.kill write, but the scope is empty by BOTH reads. Consulting the
// subtree read must not turn every PTY teardown into a claimed descendant kill:
// a run that reclaimed nothing must still publish a clean success.
//
// Without this, "always report a descendant" would pass T1 while fabricating
// E_RUN_DESCENDANT_KILLED on every ordinary PTY run.
//
// verifies: AIRA-146
func TestAIRA146PTYQuiesceReportsNoDescendantWhenBothReadsCallTheScopeEmpty(t *testing.T) {
	scope := &ptyNestedScope{}
	record := aira146LaunchQuiescedPTY(t, scope)

	if record.ScopeIntegrity == ScopeDescendantKilled || containsString(record.ErrorCodes, "E_RUN_DESCENDANT_KILLED") {
		t.Fatalf("a descendant kill was fabricated for a scope that had nothing in it: integrity=%q codes=%v", record.ScopeIntegrity, record.ErrorCodes)
	}
	if record.ScopeKill.Requested || record.ScopeKill.Started || record.ScopeKill.Completed {
		t.Fatalf("a kill that delivered nothing was attested: %+v", record.ScopeKill)
	}
	if !record.CleanSuccess() {
		t.Fatalf("an ordinary PTY run stopped being a clean success: %+v", record)
	}
}

// errAIRA146PopulationRead is an unreadable cgroup.events.
var errAIRA146PopulationRead = errors.New("cgroup.events read failed")

// aira146ReadScope answers quiescePTYScope's two PRE-kill reads from a fixed
// script and every LATER read as a settled, empty scope — which is exactly what
// a real recursive cgroup.kill produces — so a test can put one read shape in
// front of the function without waitEmpty's confirmation failing for an
// unrelated reason.
type aira146ReadScope struct {
	mu         sync.Mutex
	members    []int
	membersErr error
	empty      bool
	emptyErr   error
	killed     bool
	kills      int
}

func (*aira146ReadScope) Reference() string { return "/aira146-read-scope" }
func (*aira146ReadScope) FD() int           { return -1 }

func (s *aira146ReadScope) Members() ([]int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.killed {
		return nil, nil
	}
	return s.members, s.membersErr
}

func (s *aira146ReadScope) Empty() (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.killed {
		return true, nil
	}
	return s.empty, s.emptyErr
}

func (*aira146ReadScope) Terminate([]int) error { return nil }

func (s *aira146ReadScope) Kill() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.kills++
	s.killed = true
	return nil
}

func (*aira146ReadScope) Remove() error { return nil }

func (s *aira146ReadScope) writes() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.kills
}

// T3. THE READ MATRIX, at the function itself. The full-launch lanes above pin
// the two shapes that matter end to end; this pins the whole decision, including
// the two directions a record can never show: that the cgroup.kill write stays
// UNCONDITIONAL whatever the reads say (it is not a gate), and that an
// unestablished answer is an error — which the caller turns into
// ScopeHandoffUnverified + U_RUN_RECONCILE_REQUIRED — rather than a fabricated
// "no descendants".
//
// verifies: AIRA-146
func TestAIRA146QuiescePTYScopeDescendantReportReadMatrix(t *testing.T) {
	for _, tc := range []struct {
		name            string
		members         []int
		membersErr      error
		empty           bool
		emptyErr        error
		wantDescendants bool
		wantErr         error
	}{
		{
			name:            "leaf populated is positive evidence",
			members:         []int{4321},
			empty:           false,
			wantDescendants: true,
		},
		{
			// The defect's shape: the nested workload every other read calls empty.
			name:            "leaf empty but subtree populated is positive evidence",
			empty:           false,
			wantDescendants: true,
		},
		{
			name:            "two agreeing empty reads report no descendant",
			empty:           true,
			wantDescendants: false,
		},
		{
			// The honesty direction: without the subtree read there is no evidence
			// left for "nothing was here", so the answer is unevaluated, not clean.
			name:            "an unreadable population with an empty leaf is unevaluated",
			empty:           true,
			emptyErr:        errAIRA146PopulationRead,
			wantDescendants: false,
			wantErr:         errAIRA146PopulationRead,
		},
		{
			// A populated leaf settles it on its own; the failed second read costs
			// nothing, exactly as a populated subtree settles it past a failed leaf.
			name:            "an unreadable population with a populated leaf still reports",
			members:         []int{4321},
			emptyErr:        errAIRA146PopulationRead,
			wantDescendants: true,
		},
		{
			// AIRA-143's inherited behaviour: an unreadable LEAF with no population
			// evidence stays unevaluated.
			name:            "an unreadable leaf with an empty subtree is unevaluated",
			membersErr:      errAIRA146LeafRead,
			empty:           true,
			wantDescendants: false,
			wantErr:         errAIRA146LeafRead,
		},
		{
			// The one widening: the subtree read establishes the population the
			// unreadable leaf could not, so the reclamation is reported rather than
			// downgraded to an unverified handoff.
			name:            "an unreadable leaf with a populated subtree reports",
			membersErr:      errAIRA146LeafRead,
			empty:           false,
			wantDescendants: true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			scope := &aira146ReadScope{members: tc.members, membersErr: tc.membersErr, empty: tc.empty, emptyErr: tc.emptyErr}
			hadDescendants, err := (&Runner{grace: aira126Scale(2 * time.Second)}).quiescePTYScope(context.Background(), scope)
			if hadDescendants != tc.wantDescendants {
				t.Fatalf("hadDescendants=%v want %v (err=%v)", hadDescendants, tc.wantDescendants, err)
			}
			if tc.wantErr == nil && err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if tc.wantErr != nil && !errors.Is(err, tc.wantErr) {
				t.Fatalf("err=%v, want %v — an answer the reads could not establish must be unevaluated, never a clean 'no descendants'", err, tc.wantErr)
			}
			// The write is not gated on any of this, in any row.
			if writes := scope.writes(); writes != 1 {
				t.Fatalf("cgroup.kill writes=%d, want exactly 1: the quiesce write is unconditional, and making it conditional on a read would leave a descendant alive", writes)
			}
		})
	}
}

// errAIRA146LeafRead is an unreadable cgroup.procs.
var errAIRA146LeafRead = errors.New("cgroup.procs read failed")
