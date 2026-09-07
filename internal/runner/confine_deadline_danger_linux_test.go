//go:build linux

package runner

import (
	"context"
	"os/exec"
	"reflect"
	"sync"
	"syscall"
	"testing"
	"time"
)

// AIRA-138 REPRODUCTION / DANGER PROOF.
//
// Why this file is not an ordinary reproduction of a defect in shipped code.
//
// `aira confine` has NO job deadline of any kind today — no flag, no timer, no
// select, no kill trigger. Its wait is the unconditional
// `waitConfineCommand(cmd)` at confine_linux.go's single call site. There is
// therefore no existing code path that can be made to misbehave: a test that
// "proves the bug" against today's tree would have nothing to run.
//
// The honest strongest artifact for that starting point is a proof that the
// DANGER is real rather than theoretical — that the obvious first draft of a
// confine deadline reproduces, in confine's own currency, exactly the
// fabrication AIRA-126 spent a full two-loop cycle removing from `aira run`.
// AIRA-126's fabrication was a durable ledger record asserting a kill that
// never happened. confine has no ledger, so its currency is its EXIT CODE and
// its trailer — and those are just as capable of asserting a termination that
// did not occur.
//
// POST-IMPLEMENTATION STATE (plan §8). The gap witness is INVERTED rather than
// deleted: it now asserts that both bounds and both trailer facets exist. The
// two danger proofs and their draft implementations are KEPT, deliberately:
//
//   - `naiveConfineDeadlineDraft` is mutation #1's executed target. Forcing
//     `decideConfineDeadlineNotExecuted` to false must make the real arbitration
//     produce exactly the signature this draft produces here (exit 137, a
//     deadline attribution), which is what proves the real test is not porous.
//   - `leafOnlyKillDraft` is mutation #5's executed target, for the plan gate's
//     P0. Restoring `killConfineScope`'s gate to the leaf-only form must make the
//     nested-workload tests go red, and this file already runs that gate against
//     the state it is blind to.
//
// The honest, arbitrated behaviour of the SHIPPED code is asserted in
// confine_deadline_linux_test.go (hermetic) and confine_deadline_real_linux_test.go
// (real kernel cgroups).
//
// verifies: AIRA-138

// TestAIRA138ConfineHasAJobDeadline is the INVERTED gap witness (plan §8). In
// the reproduction phase it asserted the absence AIRA-138 rests on; now it
// asserts the presence, at the one place the fix is structural: `ConfineRequest`
// carries both JOB bounds, and `ConfineStatus` carries both trailer facets.
//
// `AdmissionMaxWait` stays asserted positively beside them, because it is the
// field an operator (and a careless implementer) most easily mistakes for a job
// deadline — it bounds the ADMISSION WAIT and nothing else, and the whole point
// of AIRA-138's naming decision is that the three are now distinguishable.
//
// verifies: AIRA-138
func TestAIRA138ConfineHasAJobDeadline(t *testing.T) {
	duration := reflect.TypeOf(time.Duration(0))
	requestType := reflect.TypeOf(ConfineRequest{})
	for _, name := range []string{"Timeout", "CPUTimeout", "AdmissionMaxWait"} {
		field, present := requestType.FieldByName(name)
		if !present || field.Type != duration {
			t.Fatalf("ConfineRequest.%s is not a time.Duration: %+v present=%v", name, field, present)
		}
	}
	statusType := reflect.TypeOf(ConfineStatus{})
	stateType := reflect.TypeOf(ConfineDeadlineState(""))
	for _, name := range []string{"Timeout", "CPUTimeout"} {
		field, present := statusType.FieldByName(name)
		if !present || field.Type != stateType {
			t.Fatalf("ConfineStatus.%s is not a ConfineDeadlineState: %+v present=%v", name, field, present)
		}
	}
	// The BUDGET travels with the state, or the trailer could say a bound fired
	// without naming the number the operator would have to change.
	for _, name := range []string{"TimeoutBudget", "CPUTimeoutBudget"} {
		field, present := statusType.FieldByName(name)
		if !present || field.Type != duration {
			t.Fatalf("ConfineStatus.%s is not a time.Duration: %+v present=%v", name, field, present)
		}
	}
}

// naiveConfineDeadlineOutcome is what the naive draft reports.
type naiveConfineDeadlineOutcome struct {
	Exit         int
	TerminatedBy string
	KillWritten  bool
}

// naiveConfineWaitOutcome is the child's own, real outcome as
// `waitConfineCommand` establishes it. The naive draft ABANDONS this on the
// fire branch; the test drains it afterwards to show what was thrown away.
type naiveConfineWaitOutcome struct {
	Exit int
	Term confineTermination
}

// naiveConfineDeadlineDraft is the OBVIOUS first draft of a confine deadline,
// written the way an implementer who had not read AIRA-126 would write it. It
// is deliberately built from confine's REAL primitives — the real
// `waitConfineCommand`, the real `classifyConfineTermination`, a real
// `Scope.Kill()` — so that what it fabricates is what a real first draft would
// fabricate, not an artefact of the harness.
//
// The defect is the whole point and is stated rather than hidden: on the fire
// branch it writes cgroup.kill and then REPORTS a kill, without ever asking the
// two questions AIRA-126 established are load-bearing — did that kill deliver a
// signal to anything, and was the leader already dead when it found nothing to
// signal.
// The second return is the SAME wait channel the draft raced. `cmd.Wait` may be
// called exactly once, so the test cannot re-wait the child to learn its real
// outcome; it drains this channel instead. That is not a harness convenience —
// it is the point. The real evidence is sitting unread in a buffered channel at
// the exact moment the draft reports a kill, which is precisely AIRA-126's
// "Launch also still holds the pending wait outcome in waitCh at that point and
// discards it".
func naiveConfineDeadlineDraft(scope Scope, cmd *exec.Cmd, fire <-chan deadlineFire) (naiveConfineDeadlineOutcome, <-chan naiveConfineWaitOutcome) {
	waitCh := make(chan naiveConfineWaitOutcome, 1)
	go func() {
		exit, term := waitConfineCommand(cmd)
		waitCh <- naiveConfineWaitOutcome{exit, term}
	}()
	select {
	case out := <-waitCh:
		return naiveConfineDeadlineOutcome{
			Exit:         out.Exit,
			TerminatedBy: classifyConfineTermination(out.Term, cgroupUsage{}, nil, deadlineKindUnset),
		}, waitCh
	case fired := <-fire:
		killErr := scope.Kill()
		return naiveConfineDeadlineOutcome{
			Exit:         128 + int(syscall.SIGKILL),
			TerminatedBy: "deadline:" + fired.Code,
			KillWritten:  killErr == nil,
		}, waitCh
	}
}

// TestAIRA138NaiveConfineDeadlineFabricatesAKill is the danger proof.
//
// It constructs the exact state AIRA-126 exists for — the deadline fires
// against a scope the child has ALREADY left, with the child's own real exit
// still pending in the wait — and shows that the naive draft reports `exit 137`
// and a deadline termination for a job that exited 7 of its own accord, having
// signalled nothing.
//
// Determinism, not a soak. `gatedStdin` (AIRA-126's own helper, reused
// verbatim) holds `cmd.Wait()` open past the fire WITHOUT touching the child:
// os/exec reaps the process first and only then joins the stdin copy goroutine,
// so the child really exits, is really reaped, and the scope really empties,
// while the wait OUTCOME is still pending. Nothing about the evidence is faked;
// only the moment the supervisor learns of it is controlled, which is the one
// thing the production race decides by chance. AIRA-136's own soak reached this
// arm 0 times in 800 iterations on an idle box (recorded in its gate review), so
// leaving it to chance is a known, measured dead end.
//
// The test also asserts, positively, that every input the honest arbitration
// needs was AVAILABLE at the instant of the fire. That is what makes this a
// reproduction of a real design hazard rather than a demonstration that a
// deliberately-broken function is broken: the draft is not missing evidence, it
// is ignoring it.
func TestAIRA138NaiveConfineDeadlineFabricatesAKill(t *testing.T) {
	bootID, err := currentBootID()
	if err != nil || bootID == "" {
		t.Skipf("boot id unavailable, so leader liveness cannot be proved: %v", err)
	}
	scope := &livenessScope{}

	// exit 7, chosen so the fabricated 137 cannot coincide with the real code and
	// so a passing assertion cannot be a zero-value accident.
	cmd := exec.Command("/bin/sh", "-c", "exit 7")
	cmd.Stdin = gatedStdin{hold: aira126Scale(2 * time.Second)}
	if err := cmd.Start(); err != nil {
		t.Fatalf("start child: %v", err)
	}
	identity := PIDIdentity{PID: cmd.Process.Pid, StartTick: processStartTick(cmd.Process.Pid), BootID: bootID}
	if identity.StartTick == 0 {
		t.Fatalf("process start tick unavailable for pid %d", identity.PID)
	}
	scope.adopt(identity)

	type draftResult struct {
		outcome naiveConfineDeadlineOutcome
		pending <-chan naiveConfineWaitOutcome
	}
	fire := make(chan deadlineFire, 1)
	done := make(chan draftResult, 1)
	go func() {
		outcome, pending := naiveConfineDeadlineDraft(scope, cmd, fire)
		done <- draftResult{outcome, pending}
	}()

	// Wait until the leader is PROVABLY gone. This is the state the fire must
	// land in, and it is established by the kernel, not assumed.
	deadline := time.Now().Add(aira126Scale(5 * time.Second))
	for processLive(identity) != processDead {
		if time.Now().After(deadline) {
			t.Fatal("the child never became provably dead; the reproduction never reached its state")
		}
		time.Sleep(time.Millisecond)
	}

	// EVERY input the honest arbitration needs, established BEFORE the fire.
	members, membersErr := scope.Members()
	if membersErr != nil || len(members) != 0 {
		t.Fatalf("the scope was not empty at the fire: members=%v err=%v", members, membersErr)
	}
	empty, emptyErr := scope.Empty()
	if emptyErr != nil || !empty {
		t.Fatalf("scope emptiness was not established at the fire: empty=%v err=%v", empty, emptyErr)
	}
	if live := processLive(identity); live != processDead {
		t.Fatalf("the leader was not proved dead at the fire: liveness=%v", live)
	}
	if terminated, killed := scope.signalled(); terminated || killed {
		t.Fatalf("a signal had already been sent before the fire (terminate=%v kill=%v)", terminated, killed)
	}

	fire <- deadlineFire{
		Actor: "run-cpu-timeout", Code: "E_RUN_CPU_TIMEOUT",
		Budget: 100 * time.Millisecond, Observed: 150 * time.Millisecond,
	}
	var result draftResult
	select {
	case result = <-done:
	case <-time.After(aira126Scale(10 * time.Second)):
		t.Fatal("the naive draft never returned")
	}
	outcome := result.outcome

	// THE FABRICATION, asserted as the reproduction's subject. When AIRA-138
	// lands, the arbitrated implementation must produce exit 7 and `normal`
	// here; this block is what §8 of the plan inverts.
	if outcome.Exit != 137 {
		t.Fatalf("the naive draft did not take its deadline branch (exit=%d); the reproduction is vacuous", outcome.Exit)
	}
	if outcome.TerminatedBy != "deadline:E_RUN_CPU_TIMEOUT" {
		t.Fatalf("the naive draft did not attribute the death to the deadline: terminated-by=%q", outcome.TerminatedBy)
	}
	if !outcome.KillWritten {
		t.Fatal("the naive draft did not write cgroup.kill, so it is not the draft the plan argues against")
	}

	// The kill it wrote reached NOTHING: the scope was already empty and stayed
	// empty. `livenessScope.Kill` records the write; membership proves it had no
	// members to signal. This is confine's exact analogue of AIRA-126's
	// `killScope` returning `{Empty:true, Started:false}` — the state in which a
	// reported kill is a fabrication.
	if _, killed := scope.signalled(); !killed {
		t.Fatal("cgroup.kill was not recorded by the scope")
	}

	// The job's REAL outcome, drained from the channel the draft abandoned. It
	// was always there.
	var real naiveConfineWaitOutcome
	select {
	case real = <-result.pending:
	case <-time.After(aira126Scale(10 * time.Second)):
		t.Fatal("the abandoned wait never produced the child's real outcome")
	}
	if real.Exit != 7 || !real.Term.Decoded || real.Term.Signaled {
		t.Fatalf("the child's real outcome is not the one the reproduction depends on: exit=%d term=%+v", real.Exit, real.Term)
	}
	if honest := classifyConfineTermination(real.Term, cgroupUsage{}, nil, deadlineKindUnset); honest != ConfineTerminatedNormal {
		t.Fatalf("the honest verdict for this evidence is not %q: got %q", ConfineTerminatedNormal, honest)
	}

	// The gap, stated so the log line IS the finding.
	t.Logf("AIRA-138 danger reproduced: the child exited %d (%s), and the naive deadline draft reported exit %d (%s)",
		real.Exit, ConfineTerminatedNormal, outcome.Exit, outcome.TerminatedBy)
}

// TestAIRA138DeadlineSourcePrimitivesAreReusableFromConfine pins the ticket's
// claim that AIRA-136's primitives can be reused rather than reinvented, by
// USING them from a confine-shaped call site with no `Runner`, no ledger and no
// `Request` anywhere in sight. If a future change couples `startDeadlineSource`
// or `readCgroupCPUFn` to the run ledger, this goes red and the plan's §4
// (reuse, verbatim) stops being true.
func TestAIRA138DeadlineSourcePrimitivesAreReusableFromConfine(t *testing.T) {
	reader := (&scriptedCPUReader{}).push(0, true).push(time.Hour, true)
	source := startDeadlineSource(deadlineConfig{
		CPU:       50 * time.Millisecond,
		CPUBase:   0,
		CPUBaseOK: true,
		ScopePath: "/aira138-not-a-real-scope",
		Interval:  time.Millisecond,
		ReadCPU:   reader.read,
	})
	if source == nil {
		t.Fatal("startDeadlineSource returned nil for a requested CPU budget")
	}
	defer source.halt()
	select {
	case fired := <-source.C:
		if fired.Code != "E_RUN_CPU_TIMEOUT" || fired.Budget != 50*time.Millisecond {
			t.Fatalf("unexpected fire: %+v", fired)
		}
		// The discriminator problem AIRA-138 §4.2 must solve, pinned here: a
		// confine call site can only tell the two bounds apart by string-matching
		// a RUN-flavoured error code. `Observed` is not a discriminator either —
		// its own doc says it is zero for the wall bound, and reading a
		// zero-valued field as evidence is the fake-evidence pattern this
		// package refuses everywhere else.
		if fired.Observed <= 0 {
			t.Fatalf("the CPU bound reported no observed consumption: %+v", fired)
		}
	case <-time.After(aira126Scale(5 * time.Second)):
		t.Fatal("the CPU budget never fired")
	}
	// Nothing above needed a Runner, a ledger, a RunRecord or a Request: the
	// reuse the ticket asserts is real.
	_ = context.Background()
}

// ---------------------------------------------------------------------------
// AIRA-138 DANGER PROOF 2 (added at plan-fix, in answer to the plan gate's P0).
//
// The first danger proof above reproduces AIRA-126's fabrication in confine's
// currency. The plan gate then found a SECOND, independent hazard in the fix
// that was proposed for it: the draft `killConfineScope` copied `aira run`'s
// then-current `killScope` gate verbatim, which refuses to write `cgroup.kill`
// whenever LEAF `cgroup.procs` is empty. (`aira run` carried the same defect and
// was fixed separately, in AIRA-140, reusing this file's fake and this test's
// bracketing shape; `leafOnlyKillDraft` below therefore no longer mirrors any
// production gate and stands purely as the historical hazard.)
//
// That gate is inert against confine's flagship heavy-job shape. An aitest /
// --delegate-ram job drains EVERY pid out of the outer scope into
// `<outer>/.aira-supervisor` and `.aira-worker-N` (BootstrapAitestSupervisor);
// `podman --cgroups=split` and any nested-cgroup workload do the same. Such a
// job is LEAF-EMPTY WHILE FULLY BUSY. The repository already knows this and says
// so twice, in the two places that had to get it right:
//
//   - confine_manage_linux.go (killConfine): "Leaf-only cgroup.procs would miss a
//     workload that migrated into a child cgroup it created inside its own scope
//     ... cgroup.kill is itself recursive, so the whole subtree is the correct
//     unit for both the gate and the confirmation."
//   - confine_manage.go (ConfineRecord.SubtreePopulated): "a fully busy suite
//     reads Populated == 0 while SubtreePopulated is true. Reading a running job
//     as empty is how an exclusive benchmark would be handed a fabricated 'you
//     are alone'."
//
// This test makes that hazard EXECUTABLE rather than argued, so the plan's
// mutation #5 has something to go red. It is inverted into T15 by the
// implementation: see the plan's §7 and §8.
//
// verifies: AIRA-138 (reproduction phase)

// nestedWorkloadScope models the one shape `livenessScope` structurally cannot
// express, and the reason a second fake is needed rather than a parameter on the
// first: `livenessScope.Empty()` is DERIVED from the same `membersLocked()` that
// `Members()` returns, so its two reads are coupled by construction and can
// never disagree. A real cgroup's two reads are independent sources —
// `cgroup.procs` (leaf) and `cgroup.events` `populated` (subtree-aware) — and it
// is exactly their disagreement that the leaf-only gate is blind to.
type nestedWorkloadScope struct {
	mu sync.Mutex
	// subtreePopulated is the `cgroup.events` reading: true while the workload is
	// alive in a CHILD cgroup of this scope.
	subtreePopulated bool
	killed           bool
	terminated       bool
}

func (s *nestedWorkloadScope) Reference() string { return "/aira138-nested-scope" }
func (s *nestedWorkloadScope) FD() int           { return -1 }

// Members reads LEAF cgroup.procs and is therefore ALWAYS empty here: every pid
// of this job lives in a child cgroup the job created inside its own scope.
func (s *nestedWorkloadScope) Members() ([]int, error) { return nil, nil }

func (s *nestedWorkloadScope) Empty() (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return !s.subtreePopulated, nil
}

func (s *nestedWorkloadScope) Terminate([]int) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.terminated = true
	return nil
}

// Kill models cgroup.kill's documented RECURSION: one write reaches the whole
// subtree, which is why the subtree is the correct unit for the gate too.
func (s *nestedWorkloadScope) Kill() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.killed = true
	s.subtreePopulated = false
	return nil
}

func (s *nestedWorkloadScope) Remove() error { return nil }

func (s *nestedWorkloadScope) signalled() (bool, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.terminated, s.killed
}

// draftKillResult is the plan's `confineKillResult` under a test-local name, so
// this reproduction depends on no production type that does not exist yet.
type draftKillResult struct {
	Empty     bool
	Started   bool
	Completed bool
}

// leafOnlyKillDraft is the plan's ORIGINAL §5.2 gate: `aira run`'s
// then-current `killScope` refusal discipline transplanted verbatim, refusing to
// write cgroup.kill on an empty LEAF read. It is the mutation target for the
// plan's mutation #5. Since AIRA-140 it mirrors no production gate — `killScope`
// carries the two-read form too — and is kept as the executable statement of the
// hazard both fixes answer.
func leafOnlyKillDraft(ctx context.Context, scope Scope) (draftKillResult, error) {
	pids, err := scope.Members()
	if err != nil {
		return draftKillResult{}, err
	}
	if len(pids) == 0 {
		empty, emptyErr := scope.Empty()
		return draftKillResult{Empty: empty && emptyErr == nil}, emptyErr
	}
	if err := scope.Kill(); err != nil {
		return draftKillResult{}, err
	}
	if err := waitEmpty(ctx, scope, time.Second); err != nil {
		return draftKillResult{Started: true}, err
	}
	return draftKillResult{Started: true, Completed: true, Empty: true}, nil
}

// twoReadKillDraft is the CORRECTED gate the plan-fix adopts: the no-signal
// refusal requires BOTH reads to agree that the scope is empty. A leaf-empty,
// subtree-populated scope falls through to the recursive kill, which is the
// whole point; a scope both reads call empty still returns before any write,
// which is what keeps arm B's `Empty && !Started` proof intact.
func twoReadKillDraft(ctx context.Context, scope Scope) (draftKillResult, error) {
	pids, err := scope.Members()
	if err != nil {
		return draftKillResult{}, err
	}
	if len(pids) == 0 {
		empty, emptyErr := scope.Empty()
		if emptyErr != nil {
			return draftKillResult{}, emptyErr
		}
		if empty {
			return draftKillResult{Empty: true}, nil
		}
		// leaf-empty, subtree-populated: fall through and kill.
	}
	if err := scope.Kill(); err != nil {
		return draftKillResult{}, err
	}
	if err := waitEmpty(ctx, scope, time.Second); err != nil {
		return draftKillResult{Started: true}, err
	}
	return draftKillResult{Started: true, Completed: true, Empty: true}, nil
}

// TestAIRA138LeafOnlyKillGateIsInertAgainstANestedWorkload is the second danger
// proof. It shows, against the same busy job, that the leaf-only gate emits no
// signal at all while the corrected two-read gate kills it — and that the
// correction does NOT weaken arm B, whose refusal must survive untouched.
func TestAIRA138LeafOnlyKillGateIsInertAgainstANestedWorkload(t *testing.T) {
	ctx := context.Background()

	// 1. THE HAZARD. A fully busy nested workload, seen through the leaf-only gate.
	busy := &nestedWorkloadScope{subtreePopulated: true}
	attempt, err := leafOnlyKillDraft(ctx, busy)
	if err != nil {
		t.Fatalf("the leaf-only gate errored, so this is not the state under test: %v", err)
	}
	if attempt.Started {
		t.Fatal("the leaf-only gate wrote cgroup.kill; this reproduction no longer reproduces anything")
	}
	if _, killed := busy.signalled(); killed {
		t.Fatal("cgroup.kill was recorded despite Started=false")
	}
	if empty, _ := busy.Empty(); empty {
		t.Fatal("the workload is not populated, so the reproduction never reached its state")
	}
	// The bound fired, the job is still running, and NOTHING was signalled. The
	// arbitration cannot rescue this either: with Empty=false the plan's arm-B
	// conjunct is false, so the run reports `fired-unevaluated` and then waits for
	// the job it was supposed to end.
	if attempt.Empty {
		t.Fatalf("a busy subtree reported Empty=true: %+v", attempt)
	}
	t.Logf("AIRA-138 P0 reproduced: leaf-empty/subtree-populated job, deadline fired, "+
		"kill attempt = %+v, cgroup.kill written = false, job still running", attempt)

	// 2. THE FIX. The same busy job, seen through the two-read gate.
	busy2 := &nestedWorkloadScope{subtreePopulated: true}
	fixed, err := twoReadKillDraft(ctx, busy2)
	if err != nil {
		t.Fatalf("the two-read gate errored against a busy nested workload: %v", err)
	}
	if !fixed.Started || !fixed.Completed {
		t.Fatalf("the two-read gate did not kill a busy nested workload: %+v", fixed)
	}
	if _, killed := busy2.signalled(); !killed {
		t.Fatal("the two-read gate reported Started without recording a cgroup.kill write")
	}

	// 3. THE ANTI-OVER-CORRECTION. A scope BOTH reads call empty must still
	// return before any write: that `Empty && !Started` shape is the sole input
	// to decideConfineDeadlineNotExecuted, and widening the kill would silently
	// delete arm B.
	quiet := &nestedWorkloadScope{subtreePopulated: false}
	refused, err := twoReadKillDraft(ctx, quiet)
	if err != nil {
		t.Fatalf("the two-read gate errored against an empty scope: %v", err)
	}
	if !refused.Empty || refused.Started || refused.Completed {
		t.Fatalf("the two-read gate did not refuse an empty scope: %+v", refused)
	}
	if _, killed := quiet.signalled(); killed {
		t.Fatal("the two-read gate wrote cgroup.kill against a scope both reads called empty")
	}

	// 4. WHY A SECOND FAKE EXISTS, pinned rather than asserted in prose:
	// livenessScope's two reads are coupled, so it can never produce the state
	// above and could never have surfaced this defect.
	coupled := &livenessScope{}
	members, membersErr := coupled.Members()
	empty, emptyErr := coupled.Empty()
	if membersErr != nil || emptyErr != nil {
		t.Fatalf("livenessScope reads errored: members=%v empty=%v", membersErr, emptyErr)
	}
	if len(members) != 0 || !empty {
		t.Fatalf("livenessScope did not start empty: members=%v empty=%v", members, empty)
	}
	if len(members) == 0 && !empty {
		t.Fatal("livenessScope expressed leaf-empty/subtree-populated; the second fake is now redundant")
	}
}
