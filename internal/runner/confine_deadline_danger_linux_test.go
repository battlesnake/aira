//go:build linux

package runner

import (
	"context"
	"os/exec"
	"reflect"
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
// T1 witnesses the gap (there is no bound to express today) and T2 is the
// load-bearing danger proof. Both are written to be INVERTED by the AIRA-138
// implementation, not deleted: see
// docs/superpowers/plans/2026-09-07-aira138-confine-deadline-plan.md §8.
//
// verifies: AIRA-138 (reproduction phase)

// TestAIRA138ConfineHasNoJobDeadlineToday is the gap witness. It asserts the
// exact absence the ticket rests on, at the one place that absence is
// structural: `ConfineRequest` carries no bound for the JOB. `AdmissionMaxWait`
// exists and is deliberately named in the negative assertion below, because it
// is the field an operator (and a careless implementer) most easily mistakes
// for a job deadline — it bounds the ADMISSION WAIT and nothing else.
//
// The fix INVERTS this test: when `Timeout` and `CPUTimeout` land on
// `ConfineRequest`, this becomes the positive assertion that both exist and are
// `time.Duration`.
func TestAIRA138ConfineHasNoJobDeadlineToday(t *testing.T) {
	requestType := reflect.TypeOf(ConfineRequest{})
	for _, name := range []string{"Timeout", "CPUTimeout", "Deadline", "CPUBudget"} {
		if _, present := requestType.FieldByName(name); present {
			t.Fatalf("ConfineRequest.%s exists: AIRA-138's premise (confine has no job deadline) no longer holds; "+
				"invert this test rather than deleting it", name)
		}
	}
	// The near-miss field, asserted positively so the distinction is pinned
	// rather than implied.
	admit, present := requestType.FieldByName("AdmissionMaxWait")
	if !present || admit.Type != reflect.TypeOf(time.Duration(0)) {
		t.Fatalf("AdmissionMaxWait is not a time.Duration on ConfineRequest: %+v present=%v", admit, present)
	}
	statusType := reflect.TypeOf(ConfineStatus{})
	for _, name := range []string{"Timeout", "CPUTimeout", "Deadline"} {
		if _, present := statusType.FieldByName(name); present {
			t.Fatalf("ConfineStatus.%s exists: the trailer already names a bound, so AIRA-138's "+
				"trailer-field question is already answered; invert this test", name)
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
			TerminatedBy: classifyConfineTermination(out.Term, cgroupUsage{}, nil),
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
	if honest := classifyConfineTermination(real.Term, cgroupUsage{}, nil); honest != ConfineTerminatedNormal {
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
