package runner

import (
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"sync"
	"testing"
	"time"

	"aira/internal/testdeadline"
)

// AIRA-126. A run-timeout deadline can fire against a scope its leader has
// already left. killWithIntent has then published a durable kill intent that
// killScope refuses to execute (it returns at len(pids)==0 BEFORE Terminate and
// before Kill), so the intent provably delivered no signal to anything, while
// the child's own wait result is still pending in waitCh. Before this change
// Launch reported that as a timed-out, non-terminal, reconcile-required record.
//
// The tests below cover the pure rules, the intent provenance, the terminal-CAS
// gate, both detached read sites, and hermetic end-to-end Launches in both
// directions. The committed real-cgroup reproduction of the original race is
// TestAIRA126RealCgroupDeadlineStraddleSoak at the bottom of this file.

func TestAIRA126TimeoutIntentNotExecutedRule(t *testing.T) {
	honouring := killAttempt{
		IntentPublished: true,
		IntentCreated:   true,
		Kill:            killResult{Empty: true},
	}
	if !decideTimeoutIntentNotExecuted(nil, honouring, processDead) {
		t.Fatal("the honouring row was refused")
	}
	// Every row below differs from the honouring row in EXACTLY ONE input, so no
	// row can pass for a reason other than the conjunct it removes.
	refused := []struct {
		name    string
		killErr error
		attempt killAttempt
		leader  processLiveness
	}{
		{"kill errored", errors.New("kill failed"), honouring, processDead},
		{"intent not published", nil, killAttempt{IntentCreated: true, Kill: killResult{Empty: true}}, processDead},
		{"foreign intent", nil, killAttempt{IntentPublished: true, Kill: killResult{Empty: true}}, processDead},
		{"a signal was sent", nil, killAttempt{IntentPublished: true, IntentCreated: true, Kill: killResult{Empty: true, Started: true}}, processDead},
		{"scope not proved empty", nil, killAttempt{IntentPublished: true, IntentCreated: true}, processDead},
		{"leader alive", nil, honouring, processAlive},
		{"leader unknown", nil, honouring, processUnknown},
	}
	for _, row := range refused {
		if decideTimeoutIntentNotExecuted(row.killErr, row.attempt, row.leader) {
			t.Fatalf("%s: arbitration was honoured on incomplete proof", row.name)
		}
	}
}

func TestAIRA126NotExecutedDispositionUsesPreMergeLedgerState(t *testing.T) {
	const published uint64 = 4
	ledgerIntent := KillIntent{Present: true, Sequence: published}
	if !decideNotExecutedDisposition(true, nil, ledgerIntent, published) {
		t.Fatal("the honouring row was refused")
	}
	refused := []struct {
		name      string
		arbitrate bool
		err       error
		intent    KillIntent
		sequence  uint64
	}{
		{"launch did not arbitrate", false, nil, ledgerIntent, published},
		{"ledger read failed", true, errors.New("torn ledger"), ledgerIntent, published},
		{"intent absent from the ledger", true, nil, KillIntent{}, published},
		{"intent already completed concurrently", true, nil, KillIntent{Present: true, Sequence: published, Completed: true}, published},
		{"ledger holds a different intent", true, nil, KillIntent{Present: true, Sequence: 9}, published},
	}
	for _, row := range refused {
		if decideNotExecutedDisposition(row.arbitrate, row.err, row.intent, row.sequence) {
			t.Fatalf("%s: disposition was honoured", row.name)
		}
	}
}

func TestAIRA126KillWithIntentReportsIntentCreatedOnlyForItsOwnIntent(t *testing.T) {
	r, scope := newMemoryRunner(t, nil)
	run := RunRecord{SchemaVersion: ledgerSchema, ID: "RUN-1", Status: StatusRunning, ScopeIntegrity: ScopeContained, CgroupScope: scope.Reference()}
	appendRunEvent(t, r, "starting", run)
	own, err := r.killWithIntent(context.Background(), "RUN-1", "run-timeout", killPolicy{})
	if err != nil {
		t.Fatal(err)
	}
	if !own.IntentPublished || !own.IntentCreated || own.IntentSequence == 0 {
		t.Fatalf("this call created the intent but did not report it: %+v", own)
	}

	r, scope = newMemoryRunner(t, nil)
	run = RunRecord{SchemaVersion: ledgerSchema, ID: "RUN-1", Status: StatusRunning, ScopeIntegrity: ScopeContained, CgroupScope: scope.Reference()}
	appendRunEvent(t, r, "starting", run)
	run.KillIntent = KillIntent{Present: true}
	appendRunEvent(t, r, "kill-intent", run)
	adopted, err := r.killWithIntent(context.Background(), "RUN-1", "run-timeout", killPolicy{})
	if err != nil {
		t.Fatal(err)
	}
	if !adopted.IntentPublished || adopted.IntentCreated {
		t.Fatalf("a pre-existing foreign intent was reported as created here: %+v", adopted)
	}
}

// livenessScope models cgroup.procs against REAL kernel state instead of a
// static list: a task is a member exactly while its /proc entry still exists
// with the recorded start tick, so a zombie is still listed (as a real cgroup
// lists it) and a reaped task is not. That fidelity is what makes the AIRA-126
// launch state reachable without cgroups — Launch positively verifies the leader
// in the scope and appends `running`, and the scope then genuinely empties when
// the leader is reaped, so killScope really returns at len(pids)==0 with no
// signal sent.
type livenessScope struct {
	mu sync.Mutex
	// leader is the launched child's boot-aware identity, adopted by the
	// harness's startFn hook.
	leader PIDIdentity
	// emptyAfterFirstMembers makes the scope report empty from the second
	// Members() call onward, modelling a leader that left the scope while
	// staying alive (an escape, or an unverified placement). It is how the
	// mutation guard reaches "empty scope, live leader" — the one state whose
	// killScope result looks identical to AIRA-126's and must NOT arbitrate.
	emptyAfterFirstMembers bool

	firstMembersDone bool
	forcedEmpty      bool
	killed           bool
	terminated       bool
}

func (s *livenessScope) Reference() string { return "/aira126-scope" }
func (s *livenessScope) FD() int           { return -1 }

func (s *livenessScope) adopt(identity PIDIdentity) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.leader = identity
}

func (s *livenessScope) signalled() (bool, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.terminated, s.killed
}

func (s *livenessScope) membersLocked() []int {
	if s.forcedEmpty || s.leader.PID <= 0 || s.leader.StartTick == 0 {
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

func (s *livenessScope) Members() ([]int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	members := s.membersLocked()
	if !s.firstMembersDone {
		s.firstMembersDone = true
		if s.emptyAfterFirstMembers {
			s.forcedEmpty = true
		}
	}
	return members, nil
}

func (s *livenessScope) Empty() (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.membersLocked()) == 0, nil
}

func (s *livenessScope) Terminate([]int) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.terminated, s.forcedEmpty = true, true
	return nil
}

func (s *livenessScope) Kill() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.killed, s.forcedEmpty = true, true
	return nil
}

func (s *livenessScope) Remove() error { return nil }

type livenessBackend struct{ scope *livenessScope }

func (b *livenessBackend) Probe(context.Context) error                   { return nil }
func (b *livenessBackend) Create(context.Context, string) (Scope, error) { return b.scope, nil }
func (b *livenessBackend) Open(context.Context, string) (Scope, error)   { return b.scope, nil }

// gatedStdin holds Cmd.Wait open past the deadline WITHOUT touching the child.
// os/exec reaps the process first and only then joins its stdin copy goroutine,
// so the child really exits, is really reaped, and the scope really empties —
// exactly as in the production race — while the wait OUTCOME is still pending in
// waitCh when Launch's select reaches the timer branch. No evidence is faked;
// only the moment Launch learns of it is controlled, which is the one thing the
// race decides by chance and a test cannot leave to chance.
type gatedStdin struct{ hold time.Duration }

func (g gatedStdin) Read([]byte) (int, error) {
	time.Sleep(g.hold)
	return 0, io.EOF
}

// aira126Scale stretches the harness's wall-clock margins the same way
// testdeadline stretches a backstop, so -race and a loaded box widen the
// windows rather than collapsing them. The child's own sleep is a real
// out-of-process interval and is deliberately NOT scaled: leaving it fixed while
// the deadline grows only widens the margin the test depends on.
func aira126Scale(d time.Duration) time.Duration {
	scaled := time.Duration(float64(d) * testdeadline.Scale())
	if scaled < d {
		return d
	}
	return scaled
}

type aira126Harness struct {
	t     *testing.T
	r     *Runner
	scope *livenessScope

	mu     sync.Mutex
	child  *os.Process
	seeded chan error
}

type aira126Options struct {
	// hold is how long after Start the child's wait outcome is withheld from
	// Launch. It must exceed the request timeout so the timer branch is the
	// branch Launch takes.
	hold time.Duration
	// grace is the runner's evidence grace; it also floors the arbitration's
	// anti-hang receive bound through arbitrationWaitBound.
	grace time.Duration
	// emptyAfterFirstMembers reaches "empty scope, live leader".
	emptyAfterFirstMembers bool
	// seedForeignIntent publishes a kill intent for this run from OUTSIDE the
	// timeout path and before the deadline fires, so killWithIntent adopts it
	// instead of creating it.
	seedForeignIntent bool
}

func newAIRA126Harness(t *testing.T, opts aira126Options) *aira126Harness {
	t.Helper()
	bootID, err := readBootIDFn()
	if err != nil || bootID == "" {
		t.Skipf("kernel boot id unavailable: %v", err)
	}
	scope := &livenessScope{emptyAfterFirstMembers: opts.emptyAfterFirstMembers}
	grace := opts.grace
	if grace <= 0 {
		grace = aira126Scale(2 * time.Second)
	}
	r, err := New(Config{CommonDir: t.TempDir(), Backend: &livenessBackend{scope: scope}, Grace: grace, TermGrace: 20 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	h := &aira126Harness{t: t, r: r, scope: scope}
	r.startFn = func(cmd *exec.Cmd) error {
		// livenessScope has no cgroup fd, so clone3's CLONE_INTO_CGROUP must be
		// stripped here or Start fails outright.
		cmd.SysProcAttr.UseCgroupFD, cmd.SysProcAttr.CgroupFD = false, 0
		if opts.hold > 0 {
			cmd.Stdin = gatedStdin{hold: opts.hold}
		}
		if err := cmd.Start(); err != nil {
			return err
		}
		scope.adopt(PIDIdentity{PID: cmd.Process.Pid, StartTick: processStartTick(cmd.Process.Pid), BootID: bootID})
		h.mu.Lock()
		h.child = cmd.Process
		h.mu.Unlock()
		return nil
	}
	if opts.seedForeignIntent {
		h.seeded = make(chan error, 1)
		go func() { h.seeded <- h.seedForeignIntentAfterRunning() }()
	}
	// A refused arbitration deliberately leaves a genuinely live child that the
	// empty fake scope cannot kill, so a failing run must not leak a process
	// into the test slice.
	t.Cleanup(h.killChild)
	return h
}

func (h *aira126Harness) killChild() {
	h.mu.Lock()
	child := h.child
	h.mu.Unlock()
	if child != nil {
		_ = child.Kill()
	}
}

// seedForeignIntentAfterRunning waits for Launch's `running` event — a plain
// event kind replaces the replayed record wholesale, so an intent seeded before
// it would simply be erased — and then publishes a kill intent under the run
// lock, as an external `aira run kill` would.
func (h *aira126Harness) seedForeignIntentAfterRunning() error {
	backstop := time.Now().Add(testdeadline.Wait(20 * time.Second))
	for time.Now().Before(backstop) {
		events, err := h.r.ledger.read()
		if err != nil {
			return err
		}
		id := ""
		for _, event := range events {
			if event.Kind == "running" {
				id = event.Run.ID
			}
		}
		if id == "" {
			time.Sleep(testdeadline.PollInterval)
			continue
		}
		lock, lockErr := lockFile(filepath.Join(filepath.Dir(h.r.ledger.ledger), id+".lock"))
		if lockErr != nil {
			return lockErr
		}
		current, currentErr := h.r.ledger.current(id)
		if currentErr == nil {
			current.KillIntent = KillIntent{Present: true}
			_, currentErr = h.r.ledger.append(ledgerEvent{Kind: "kill-intent", Run: current})
		}
		_ = unlockFile(lock)
		return currentErr
	}
	return errors.New("the run never published a running event to seed against")
}

// killIntent reports the run id and sequence of the published intent, once one
// exists in the ledger.
func (h *aira126Harness) killIntent() (string, uint64, bool) {
	events, err := h.r.ledger.read()
	if err != nil {
		return "", 0, false
	}
	for _, event := range events {
		if event.Kind == "kill-intent" && event.Run.KillIntent.Present {
			return event.Run.ID, event.Run.KillIntent.Sequence, true
		}
	}
	return "", 0, false
}

func TestAIRA126TimeoutAgainstAlreadyExitedLeaderArbitratesToExited(t *testing.T) {
	const iterations = 6
	arbitrated := 0
	for i := 0; i < iterations; i++ {
		h := newAIRA126Harness(t, aira126Options{hold: aira126Scale(900 * time.Millisecond)})
		record, err := h.r.Launch(context.Background(), Request{
			Argv:    []string{"/bin/sleep", "0.03"},
			Timeout: aira126Scale(300 * time.Millisecond),
		})
		if err != nil {
			t.Fatalf("iteration %d: launch error=%v record=%+v", i, err, record)
		}
		if record.Status != StatusExited || record.ExitCode == nil || *record.ExitCode != 0 {
			t.Fatalf("iteration %d: record=%+v", i, record)
		}
		if got := terminalRecords(t, h.r); got != 1 {
			t.Fatalf("iteration %d: terminal records=%d", i, got)
		}
		if !record.KillIntent.Present {
			continue
		}
		arbitrated++
		// The deadline DID fire here: the published intent must be dispositioned
		// not-executed, never completed, no signal may be recorded as sent, and
		// no termination may be asserted.
		if !record.KillIntent.NotExecuted || record.KillIntent.Completed || record.ScopeKill.Started {
			t.Fatalf("iteration %d: undisposed or over-claimed intent: %+v", i, record)
		}
		if containsString(record.ErrorCodes, "E_RUN_TIMEOUT") || containsString(record.ErrorCodes, "U_RUN_RECONCILE_REQUIRED") {
			t.Fatalf("iteration %d: a kill that delivered nothing was reported as a timeout: %+v", i, record)
		}
		if terminated, killed := h.scope.signalled(); terminated || killed {
			t.Fatalf("iteration %d: a signal was sent after all (terminate=%v kill=%v)", i, terminated, killed)
		}
		// The disposition must be durable, not present only on the return path.
		current, err := h.r.Get(record.ID)
		if err != nil {
			t.Fatal(err)
		}
		if !current.KillIntent.NotExecuted || current.KillIntent.Completed || !current.Status.Terminal() {
			t.Fatalf("iteration %d: ledger disagrees with the returned record: %+v", i, current)
		}
	}
	if arbitrated == 0 {
		t.Fatalf("vacuous: the timer branch never fired against an already-empty scope in %d iterations", iterations)
	}
	t.Logf("timer branch fired against an already-empty scope in %d/%d iterations", arbitrated, iterations)
}

func TestAIRA126LiveLeaderAtDeadlineStillTimesOut(t *testing.T) {
	// The scope empties while the leader stays alive, so killScope returns the
	// same {Empty:true, Started:false} shape as the AIRA-126 state — and the run
	// must NOT arbitrate to exited, because the deadline was genuinely breached
	// and AIRA genuinely killed nothing. This is the test that stops the fix
	// being widened into "an empty scope means the run finished". It is also the
	// undisposed-intent guard: the terminal CAS must still block.
	h := newAIRA126Harness(t, aira126Options{
		emptyAfterFirstMembers: true,
		grace:                  aira126Scale(300 * time.Millisecond),
	})
	record, err := h.r.Launch(context.Background(), Request{
		Argv:    []string{"/bin/sleep", "30"},
		Timeout: aira126Scale(200 * time.Millisecond),
	})
	var launchError *LaunchError
	if !errors.As(err, &launchError) || launchError.Code != "U_RUN_RECONCILE_REQUIRED" {
		t.Fatalf("live leader at the deadline did not stay unevaluated: err=%v record=%+v", err, record)
	}
	if record == nil {
		t.Fatal("no record was published")
	}
	if record.Status == StatusExited || record.TerminalComplete || record.KillIntent.NotExecuted {
		t.Fatalf("a live, unkilled leader was arbitrated away: %+v", record)
	}
	if !record.KillIntent.Present || record.KillIntent.Completed {
		t.Fatalf("the deadline's intent evidence was lost: %+v", record)
	}
	if !containsString(record.ErrorCodes, "E_RUN_TIMEOUT") || !containsString(record.ErrorCodes, "U_RUN_RECONCILE_REQUIRED") {
		t.Fatalf("the breached deadline lost its evidence codes: %+v", record)
	}
	if got := terminalRecords(t, h.r); got != 0 {
		t.Fatalf("terminal records=%d", got)
	}
}

func TestAIRA126ForeignKillIntentIsNotDismissed(t *testing.T) {
	h := newAIRA126Harness(t, aira126Options{hold: aira126Scale(900 * time.Millisecond), seedForeignIntent: true})
	record, err := h.r.Launch(context.Background(), Request{
		Argv:    []string{"/bin/sleep", "0.03"},
		Timeout: aira126Scale(300 * time.Millisecond),
	})
	if seedErr := <-h.seeded; seedErr != nil {
		t.Fatal(seedErr)
	}
	var launchError *LaunchError
	if !errors.As(err, &launchError) || launchError.Code != "U_RUN_RECONCILE_REQUIRED" {
		t.Fatalf("a foreign intent was dismissed: err=%v record=%+v", err, record)
	}
	if record == nil {
		t.Fatal("no record was published")
	}
	if record.KillIntent.NotExecuted {
		t.Fatalf("a launch dispositioned an intent it did not create: %+v", record)
	}
	if got := terminalRecords(t, h.r); got != 0 {
		t.Fatalf("terminal records=%d", got)
	}
}

func TestAIRA126CompletedIntentIsNotDowngradedByNotExecuted(t *testing.T) {
	h := newAIRA126Harness(t, aira126Options{hold: aira126Scale(900 * time.Millisecond)})
	type completion struct {
		intent KillIntent
		err    error
	}
	done := make(chan completion, 1)
	stop := make(chan struct{})
	defer close(stop)
	go func() {
		// Complete the intent durably in the window between the deadline's
		// kill-intent append and Launch's terminal CAS — exactly the concurrent
		// actor the pre-merge read exists to protect.
		backstop := time.After(testdeadline.Wait(20 * time.Second))
		for {
			select {
			case <-stop:
				return
			case <-backstop:
				done <- completion{err: errors.New("no kill intent was ever published")}
				return
			default:
			}
			id, sequence, ok := h.killIntent()
			if !ok {
				time.Sleep(testdeadline.PollInterval)
				continue
			}
			intent := KillIntent{Present: true, Sequence: sequence, Completed: true, Empty: true}
			lock, err := lockFile(filepath.Join(filepath.Dir(h.r.ledger.ledger), id+".lock"))
			if err == nil {
				var current RunRecord
				current, err = h.r.ledger.current(id)
				if err == nil {
					current.KillIntent = intent
					current.ScopeKill = ScopeKill{Requested: true, Started: true, Completed: true, Actor: "concurrent-killer"}
					_, err = h.r.ledger.append(ledgerEvent{Kind: "kill-completed", Run: current})
				}
				_ = unlockFile(lock)
			}
			done <- completion{intent: intent, err: err}
			return
		}
	}()

	record, launchErr := h.r.Launch(context.Background(), Request{
		Argv:    []string{"/bin/sleep", "0.03"},
		Timeout: aira126Scale(300 * time.Millisecond),
	})
	outcome := <-done
	if outcome.err != nil {
		t.Fatal(outcome.err)
	}
	if launchErr != nil {
		t.Fatalf("launch error=%v record=%+v", launchErr, record)
	}
	// The concurrent actor's completed intent must survive byte for byte in
	// whatever event the CAS appended, not merely on the return path.
	current, err := h.r.Get(record.ID)
	if err != nil {
		t.Fatal(err)
	}
	if current.KillIntent != outcome.intent {
		t.Fatalf("ledger intent=%+v want %+v", current.KillIntent, outcome.intent)
	}
	if !current.ScopeKill.Completed {
		t.Fatalf("the concurrent actor's scope-kill evidence was lost: %+v", current)
	}
}

func TestAIRA126DetachedFinaliserDoesNotReportKilledForNotExecutedIntent(t *testing.T) {
	r, _ := newMemoryRunner(t, nil)
	zero := 0
	run := RunRecord{
		SchemaVersion: ledgerSchema, ID: "RUN-1", Status: StatusRunning, Detached: true,
		ScopeIntegrity: ScopeContained, OutputRefs: map[string]OutputRef{},
		KillIntent: KillIntent{Present: true, Sequence: 3, NotExecuted: true},
	}
	appendRunEvent(t, r, "starting", run)
	if _, err := r.ledger.append(ledgerEvent{Kind: "leader-exited", Run: run, LeaderExitObserved: true, ExitCode: &zero}); err != nil {
		t.Fatal(err)
	}
	final, err := r.finalizeDetachedTerminal(context.Background(), "RUN-1", nil)
	if err != nil {
		t.Fatal(err)
	}
	if final.Status != StatusExited || final.KillIntent.Completed || containsString(final.ErrorCodes, "E_RUN_KILLED") {
		t.Fatalf("a not-executed intent was read as a kill: %+v", final)
	}
	if !final.KillIntent.NotExecuted {
		t.Fatalf("the disposition was dropped: %+v", final)
	}
}

func TestAIRA126DetachedNoChildDoesNotReportKilledForNotExecutedIntent(t *testing.T) {
	r, _ := newMemoryRunner(t, nil)
	run := RunRecord{
		SchemaVersion: ledgerSchema, ID: "RUN-1", Status: StatusStarting, Detached: true,
		ScopeIntegrity: ScopeContained,
		KillIntent:     KillIntent{Present: true, Sequence: 3, NotExecuted: true},
	}
	appendRunEvent(t, r, "starting", run)
	final, _ := r.terminalizeDetachedNoChild(context.Background(), run, false, "U_RUN_DETACH_CANCELLED", errors.New("cancelled"))
	if final == nil {
		t.Fatal("no record was published")
	}
	if final.Status == StatusKilled || final.KillIntent.Completed || containsString(final.ErrorCodes, "E_RUN_KILLED") {
		t.Fatalf("a not-executed intent was read as a kill: %+v", final)
	}
}

// TestAIRA126RealCgroupDeadlineStraddleSoak is the committed, executable
// reproduction of the original race (plan §10.6): a deliberately
// deadline-straddling real-cgroup run, looped, asserting the arbitration
// invariant on every iteration and refusing to pass unless at least one
// iteration actually saw the deadline fire.
//
// The AIRA-126 state reproduces at roughly 2% per iteration in isolation, so a
// loop short enough for the standard suite would pass vacuously most of the
// time — which is precisely the porous shape the review policy rejects. It is
// therefore opt-in rather than silently weak: set AIRA126_SOAK=1 (and
// optionally AIRA126_SOAK_ITERATIONS, default 800) to run it. The always-on
// coverage of the same arbitration is the hermetic pair above, whose device
// makes the timer branch deterministic rather than lucky.
func TestAIRA126RealCgroupDeadlineStraddleSoak(t *testing.T) {
	if os.Getenv("AIRA126_SOAK") != "1" {
		t.Skip("set AIRA126_SOAK=1 to run the AIRA-126 deadline-straddle reproduction")
	}
	iterations := 800
	if raw := os.Getenv("AIRA126_SOAK_ITERATIONS"); raw != "" {
		parsed, err := strconv.Atoi(raw)
		if err != nil || parsed <= 0 {
			t.Fatalf("AIRA126_SOAK_ITERATIONS=%q is not a positive integer", raw)
		}
		iterations = parsed
	}
	straddled, notExecuted := 0, 0
	for i := 0; i < iterations; i++ {
		r := realRunner(t)
		record, err := r.Launch(context.Background(), Request{Argv: []string{"/bin/sleep", "0.04"}, Timeout: 50 * time.Millisecond})
		if err != nil {
			t.Fatalf("iteration %d: launch error=%v record=%+v", i, err, record)
		}
		if got := terminalRecords(t, r); got != 1 {
			t.Fatalf("iteration %d: terminal records=%d record=%+v", i, got, record)
		}
		switch record.Status {
		case StatusExited:
			if record.ExitCode == nil || *record.ExitCode != 0 || !record.CleanSuccess() {
				t.Fatalf("iteration %d: clean exit evidence=%+v", i, record)
			}
			if record.KillIntent.Present {
				straddled, notExecuted = straddled+1, notExecuted+1
				if !record.KillIntent.NotExecuted || record.KillIntent.Completed || record.ScopeKill.Started {
					t.Fatalf("iteration %d: undisposed or over-claimed intent: %+v", i, record)
				}
				if containsString(record.ErrorCodes, "E_RUN_TIMEOUT") {
					t.Fatalf("iteration %d: a kill that delivered nothing was reported as a timeout: %+v", i, record)
				}
			}
		case StatusKilled:
			straddled++
			if !containsString(record.ErrorCodes, "E_RUN_TIMEOUT") || !record.KillIntent.Completed || !record.ScopeKill.Completed {
				t.Fatalf("iteration %d: timeout evidence=%+v", i, record)
			}
		default:
			t.Fatalf("iteration %d: run did not arbitrate to a terminal state: %+v", i, record)
		}
	}
	if straddled == 0 {
		t.Fatalf("vacuous: the deadline never fired in %d iterations; raise AIRA126_SOAK_ITERATIONS", iterations)
	}
	t.Logf("deadline fired in %d/%d iterations; %d of those found the scope already empty and dispositioned the intent not-executed", straddled, iterations, notExecuted)
}
