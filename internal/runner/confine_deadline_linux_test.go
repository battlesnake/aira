//go:build linux

package runner

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"aira/internal/testdeadline"
)

// AIRA-138 — the ARBITRATED confine deadline, driven hermetically through the
// real confineWithDeps with a real child, real wait4 evidence and real kernel
// liveness. Only two things are controlled: which scope the launch is handed,
// and the moment the supervisor learns the child's outcome. Nothing about the
// evidence is faked.
//
// Determinism over soak, on measured grounds: AIRA-136's gate review recorded
// that its 800-iteration real-cgroup soak reached the arbitrated arm ZERO times
// on an idle box, so leaving this arm to chance is a known dead end.
//
// verifies: AIRA-138

// deadlineConfineScope is the confine test seam these tests share. It exists
// because livenessScope structurally CANNOT express the state AIRA-138's kill
// gate turns on: livenessScope.Empty() is derived from the same membersLocked()
// that Members() returns, so its two reads are coupled by construction and can
// never disagree. A real cgroup's two reads are independent sources —
// cgroup.procs (leaf) and cgroup.events `populated` (subtree-aware) — and it is
// exactly their disagreement that a leaf-only gate is blind to.
//
// Leaf membership is derived from the REAL child's real liveness, so a test that
// waits for the kernel to report the leader gone is waiting on a fact, not on a
// flag the test set.
type deadlineConfineScope struct {
	mu     sync.Mutex
	leader PIDIdentity
	// subtreePopulated, when non-nil, makes Empty() INDEPENDENT of the leaf read:
	// the job's pids live in a child cgroup it created inside its own scope
	// (the aitest / --delegate-ram / podman --cgroups=split shape). Nil couples
	// the two reads, which is the ordinary single-cgroup job.
	subtreePopulated *bool
	// forcedEmpty makes BOTH reads report empty regardless of the leader's real
	// liveness, modelling a leader that left the scope while staying alive (an
	// escape, or an unverified placement). It is how a test reaches "empty scope,
	// LIVE leader" — the one state whose killConfineScope result looks identical
	// to the arbitrated arm's and must NOT be arbitrated.
	forcedEmpty bool
	killCount   int
	terminated  bool
	emptyCalls  int
	// onFirstEmpty runs inside the FIRST Empty() call. killConfineScope is the
	// only caller of scope.Empty() before the wait completes (the membership
	// monitor polls Members(), and the teardown attestation runs afterwards), so
	// this hook fires at a precisely known point of the arbitration: after the two
	// reads, before processLive. It is how a test releases the wait, or perturbs
	// leader liveness, exactly where the production code looks.
	onFirstEmpty func()
	// livenessAtGate records what processLive said at that same instant, so a test
	// asserting "this arm was refused because the leader was alive" proves the
	// leader really was alive rather than assuming it.
	livenessAtGate processLiveness
}

func (*deadlineConfineScope) Reference() string { return "/aira138-confine-scope" }
func (*deadlineConfineScope) FD() int           { return -1 }

func (s *deadlineConfineScope) adopt(identity PIDIdentity) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.leader = identity
}

func (s *deadlineConfineScope) identity() PIDIdentity {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.leader
}

// leafMembersLocked is the LEAF cgroup.procs reading: the leader while the
// kernel still has it, nothing once it is gone.
func (s *deadlineConfineScope) leafMembersLocked() []int {
	if s.forcedEmpty || s.leader.PID <= 0 || s.leader.StartTick == 0 {
		return nil
	}
	if processStartTick(s.leader.PID) != s.leader.StartTick {
		return nil
	}
	return []int{s.leader.PID}
}

func (s *deadlineConfineScope) Members() ([]int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.leafMembersLocked(), nil
}

func (s *deadlineConfineScope) Empty() (bool, error) {
	s.mu.Lock()
	first := s.emptyCalls == 0
	s.emptyCalls++
	hook := s.onFirstEmpty
	leader := s.leader
	s.mu.Unlock()
	if first && hook != nil {
		hook()
	}
	if first {
		live := processLive(leader)
		s.mu.Lock()
		s.livenessAtGate = live
		s.mu.Unlock()
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.forcedEmpty {
		return true, nil
	}
	if s.subtreePopulated != nil {
		return !*s.subtreePopulated, nil
	}
	return len(s.leafMembersLocked()) == 0, nil
}

// forceEmpty makes both reads report empty from here on. Called from the CPU
// reader on the sample that DECIDES to fire, so the state is established before
// the fire is even sent — strictly ordered inside the source goroutine, with no
// window for the arbitration to observe a different scope than the test set up.
func (s *deadlineConfineScope) forceEmpty() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.forcedEmpty = true
}

func (s *deadlineConfineScope) Terminate([]int) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.terminated = true
	return nil
}

// Kill models cgroup.kill's documented RECURSION: one write reaches the whole
// subtree, which is why the subtree is the correct unit for the gate too.
func (s *deadlineConfineScope) Kill() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.killCount++
	if s.subtreePopulated != nil {
		empty := false
		s.subtreePopulated = &empty
	}
	return nil
}

func (s *deadlineConfineScope) Remove() error { return nil }

func (s *deadlineConfineScope) kills() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.killCount
}

func (s *deadlineConfineScope) gateLiveness() processLiveness {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.livenessAtGate
}

// deadlineGatedStdin holds cmd.Wait() open past the fire WITHOUT touching the
// child. os/exec reaps the process FIRST and only then joins its stdin copy
// goroutine, so the child really exits, is really reaped and the scope really
// empties — exactly as in the production race — while the wait OUTCOME is still
// pending in waitCh when the select reaches the deadline branch.
//
// The backstop is not a timing assumption: it bounds a wrong assumption into a
// failed assertion instead of a hung test.
type deadlineGatedStdin struct {
	release  chan struct{}
	backstop time.Duration
}

func newDeadlineGatedStdin(hold time.Duration) *deadlineGatedStdin {
	return &deadlineGatedStdin{release: make(chan struct{}), backstop: hold}
}

func (g *deadlineGatedStdin) Read([]byte) (int, error) {
	timer := time.NewTimer(g.backstop)
	defer timer.Stop()
	select {
	case <-g.release:
	case <-timer.C:
	}
	return 0, io.EOF
}

func (g *deadlineGatedStdin) open() {
	select {
	case <-g.release:
	default:
		close(g.release)
	}
}

// deadlineConfineDeps is confineUnitDeps with the AIRA-138 scope and an
// identity-adopting start hook. Every other dependency is the same stub the
// existing confine unit tests use, so the code under test is the production
// launch path.
func deadlineConfineDeps(t *testing.T, scope *deadlineConfineScope) confineDeps {
	t.Helper()
	bootID, err := currentBootID()
	if err != nil || bootID == "" {
		t.Skipf("boot id unavailable, so leader liveness cannot be proved: %v", err)
	}
	return confineDeps{
		resolveSlicePath:    func(string) (string, bool, string) { return "/fake/finite.slice", true, "" },
		ensureDelegation:    func(string) (confineDelegation, error) { return confineDelegation{cpuWeight: true}, nil },
		writeScopeCPUWeight: func(Scope, int64) bool { return true },
		newBackend:          func(string) ScopeBackend { return confineFakeBackend{scope: scope} },
		admit: func(context.Context, string, ConfineRequest, int64) (admissionResult, error) {
			return admissionResult{state: "immediate"}, nil
		},
		writeOOMGroup:     func(Scope) error { return nil },
		writeScopeSwapCap: func(Scope) (string, error) { return WorkerAdmitSwapCapEnforced, nil },
		readCap:           func(string) (int64, bool) { return 64 << 30, true },
		start: func(command *confineCommand) error {
			command.cmd.Args[1] = "__confine-test-setup"
			command.cmd.SysProcAttr = nil
			if err := command.Start(); err != nil {
				return err
			}
			pid := command.cmd.Process.Pid
			identity := PIDIdentity{PID: pid, StartTick: processStartTick(pid), BootID: bootID}
			if identity.StartTick == 0 {
				return errors.New("process start tick unavailable")
			}
			scope.adopt(identity)
			return nil
		},
	}
}

// overBudgetOnceLeaderIsDead is the CPU reader for the arbitrated arms. It reads
// an honest zero — established, never unevaluated — for the pre-start baseline
// and for every sample while the leader lives, and goes far over budget only
// once the KERNEL says the leader is gone. The fire therefore lands, by
// construction, in the state the arbitration exists for, rather than by luck.
func overBudgetOnceLeaderIsDead(scope *deadlineConfineScope) func(string) (time.Duration, bool) {
	return func(string) (time.Duration, bool) {
		if identity := scope.identity(); identity.PID > 0 && processLive(identity) == processDead {
			return time.Hour, true
		}
		return 0, true
	}
}

// overBudgetWhileLeaderLives is its mirror, for the arms that must fire against a
// job that is still RUNNING. It never fires after the leader dies, so a test that
// depends on a live leader cannot silently degrade into testing the other arm.
func overBudgetWhileLeaderLives(scope *deadlineConfineScope) func(string) (time.Duration, bool) {
	first := true
	var mu sync.Mutex
	return func(string) (time.Duration, bool) {
		mu.Lock()
		baseline := first
		first = false
		mu.Unlock()
		if baseline {
			return 0, true
		}
		if identity := scope.identity(); identity.PID > 0 && processLive(identity) == processAlive {
			scope.forceEmpty()
			return time.Hour, true
		}
		return 0, true
	}
}

const aira138CPUBudget = 50 * time.Millisecond

// T3 / the inverted danger proof. The deadline fires against a scope the child
// has ALREADY left, with the child's own real exit still pending in the wait.
// The naive draft in confine_deadline_danger_linux_test.go reports exit 137 and
// a deadline attribution for exactly this state; the arbitrated implementation
// must report the child's own exit 7 and `normal`, must have sent no signal, and
// must still record on the trailer that the bound fired and killed nothing.
//
// verifies: AIRA-138
func TestAIRA138DeadlineAgainstAlreadyExitedChildReportsTheRealExit(t *testing.T) {
	scope := &deadlineConfineScope{}
	gate := newDeadlineGatedStdin(aira126Scale(10 * time.Second))
	// Released inside killConfineScope, after the two reads. The child is already
	// reaped by then, so this only decides WHEN the supervisor learns of it.
	scope.onFirstEmpty = gate.open
	swapCPUReader(t, overBudgetOnceLeaderIsDead(scope))

	var stderr bytes.Buffer
	// exit 7, so a fabricated 137 cannot coincide with the real code and a passing
	// assertion cannot be a zero-value accident.
	result, err := confineWithDeps(context.Background(), ConfineRequest{
		Slice: "finite.slice", Argv: []string{"/bin/sh", "-c", "exit 7"},
		CPUTimeout: aira138CPUBudget, SelfPath: os.Args[0],
		Stdin: gate, Stderr: &stderr,
	}, deadlineConfineDeps(t, scope))
	if err != nil {
		t.Fatalf("confine: %v (stderr=%q)", err, stderr.String())
	}
	if scope.gateLiveness() != processDead {
		t.Fatalf("the leader was %v at the kill gate, so this run never reached the arbitrated state", scope.gateLiveness())
	}
	if result.Status.CPUTimeout != ConfineDeadlineFiredNotExecuted {
		t.Fatalf("cpu-timeout state = %q, want %q (stderr=%q)", result.Status.CPUTimeout, ConfineDeadlineFiredNotExecuted, stderr.String())
	}
	// THE FABRICATION THIS TICKET EXISTS TO PREVENT. The naive draft reports 137
	// and `deadline:...` here.
	if result.Exit != 7 {
		t.Fatalf("exit = %d, want the child's own 7: a deadline that killed nothing must never replace the job's exit code", result.Exit)
	}
	if result.Status.TerminatedBy != ConfineTerminatedNormal {
		t.Fatalf("terminated-by = %q, want %q: nothing was terminated, so nothing may be attributed",
			result.Status.TerminatedBy, ConfineTerminatedNormal)
	}
	// No signal was emitted. One kill remains — the deferred teardown, which runs
	// after the trailer and is not the deadline's.
	if got := scope.kills(); got != 1 {
		t.Fatalf("cgroup.kill was written %d times; want exactly 1 (the teardown), i.e. the deadline sent nothing", got)
	}
	// The fire-time diagnostic, in the signal handler's tense: past tense only for
	// what happened.
	if !strings.Contains(stderr.String(), "sent no signal") {
		t.Fatalf("the fire wrote no arm-correct diagnostic: %q", stderr.String())
	}
	if strings.Contains(stderr.String(), "killed scope") {
		t.Fatalf("the diagnostic claimed a kill that never happened: %q", stderr.String())
	}
	if !strings.Contains(stderr.String(), "cpu-timeout="+aira138CPUBudget.String()+":"+string(ConfineDeadlineFiredNotExecuted)) {
		t.Fatalf("the trailer lost the fired-but-ineffective bound: %q", stderr.String())
	}
	// The wall bound was never requested, so its field must not appear at all.
	if strings.Contains(stderr.String(), " timeout=") {
		t.Fatalf("a bound nobody asked for was rendered: %q", stderr.String())
	}
}

// The arm-A counterpart of the same construction, and the hermetic form of the
// plan gate's P0: the leaf is empty while the SUBTREE is populated, which is the
// aitest / --delegate-ram shape. The gate must fall through to the recursive
// cgroup.kill rather than refusing, and the run must land in arm A.
//
// It also pins the §5.5 "graceful middle case": the child had already exited by
// itself, so `terminated-by=normal` and `exit 7` stand beside
// `cpu-timeout=...:fired-kill-completed` — the state describes the KILL
// OPERATION, `terminated-by` describes the cause of death, and the two are
// allowed to disagree.
//
// verifies: AIRA-138
func TestAIRA138DeadlineKillsALeafEmptySubtreePopulatedConfineJob(t *testing.T) {
	populated := true
	scope := &deadlineConfineScope{subtreePopulated: &populated}
	gate := newDeadlineGatedStdin(aira126Scale(10 * time.Second))
	scope.onFirstEmpty = gate.open
	swapCPUReader(t, overBudgetOnceLeaderIsDead(scope))

	var stderr bytes.Buffer
	result, err := confineWithDeps(context.Background(), ConfineRequest{
		Slice: "finite.slice", Argv: []string{"/bin/sh", "-c", "exit 7"},
		CPUTimeout: aira138CPUBudget, SelfPath: os.Args[0],
		Stdin: gate, Stderr: &stderr,
	}, deadlineConfineDeps(t, scope))
	if err != nil {
		t.Fatalf("confine: %v (stderr=%q)", err, stderr.String())
	}
	if result.Status.CPUTimeout != ConfineDeadlineFiredKillCompleted {
		t.Fatalf("cpu-timeout state = %q, want %q: a leaf-empty but subtree-POPULATED scope is a BUSY job and must be killed, "+
			"not shrugged at (stderr=%q)", result.Status.CPUTimeout, ConfineDeadlineFiredKillCompleted, stderr.String())
	}
	if got := scope.kills(); got != 2 {
		t.Fatalf("cgroup.kill was written %d times; want 2 (the deadline's, then the teardown's)", got)
	}
	if result.Status.TerminatedBy != ConfineTerminatedNormal || result.Exit != 7 {
		t.Fatalf("the child exited by itself between the read and the write, so its own evidence stands: "+
			"exit=%d terminated-by=%q", result.Exit, result.Status.TerminatedBy)
	}
	if !strings.Contains(stderr.String(), "killed scope") {
		t.Fatalf("the fire wrote no arm-A diagnostic: %q", stderr.String())
	}
}

// The over-widening guard: the scope reads empty by BOTH reads, but the leader is
// still ALIVE. processLive reads alive, the arbitration must refuse, and the run
// must land in arm C. This is what stops "an empty scope means the job finished"
// from becoming the rule.
//
// verifies: AIRA-138
func TestAIRA138DeadlineWithLiveLeaderNeverArbitrates(t *testing.T) {
	scope := &deadlineConfineScope{}
	gate := newDeadlineGatedStdin(aira126Scale(10 * time.Second))
	scope.onFirstEmpty = gate.open
	// The reader empties the scope on the very sample that decides to fire, so the
	// arbitration meets an empty scope and a genuinely LIVE leader.
	swapCPUReader(t, overBudgetWhileLeaderLives(scope))

	var stderr bytes.Buffer
	result, err := confineWithDeps(context.Background(), ConfineRequest{
		Slice: "finite.slice", Argv: []string{"/bin/sh", "-c", "sleep 1; exit 7"},
		CPUTimeout: aira138CPUBudget, SelfPath: os.Args[0],
		Stdin: gate, Stderr: &stderr,
	}, deadlineConfineDeps(t, scope))
	if err != nil {
		t.Fatalf("confine: %v (stderr=%q)", err, stderr.String())
	}
	if scope.gateLiveness() != processAlive {
		t.Skipf("the child was %v at the kill gate rather than alive; this box did not reach the state under test", scope.gateLiveness())
	}
	if result.Status.CPUTimeout != ConfineDeadlineFiredUnevaluated {
		t.Fatalf("cpu-timeout state = %q, want %q: an empty scope with a LIVE leader is unevaluated, never arbitrated away",
			result.Status.CPUTimeout, ConfineDeadlineFiredUnevaluated)
	}
	if result.Exit != 7 {
		t.Fatalf("exit = %d, want the child's own 7", result.Exit)
	}
	if got := scope.kills(); got != 1 {
		t.Fatalf("cgroup.kill was written %d times; want exactly 1 (the teardown)", got)
	}
	if !strings.Contains(stderr.String(), "kill unevaluated") {
		t.Fatalf("the fire wrote no arm-C diagnostic: %q", stderr.String())
	}
}

// processUnknown must refuse the arbitration exactly as processAlive does. A
// guard written as `!= processAlive` instead of `== processDead` passes the
// previous test and fails this one.
//
// verifies: AIRA-138
func TestAIRA138DeadlineWithUnknownLeaderStaysUnevaluated(t *testing.T) {
	scope := &deadlineConfineScope{}
	gate := newDeadlineGatedStdin(aira126Scale(10 * time.Second))
	// A stat that READS but cannot be parsed: processLive cannot establish
	// liveness either way, which is processUnknown and not processDead. The
	// swapped reader is INSTALLED before the launch, so the write to the package
	// variable happens-before every goroutine the launch creates (the membership
	// monitor reads it through processLive concurrently with the kill gate), and
	// it is ARMED inside the gate through an atomic flag, so the launch's own
	// identity establishment (which runs long before) still sees the real stat.
	// Build review: writing the variable inside the hook was a data race against
	// the monitor goroutine and turned the CI race lane red.
	var malformed atomic.Bool
	originalStat := readProcStatFn
	readProcStatFn = func(pid int) ([]byte, error) {
		if malformed.Load() {
			return []byte("malformed"), nil
		}
		return originalStat(pid)
	}
	t.Cleanup(func() { readProcStatFn = originalStat })
	scope.onFirstEmpty = func() {
		malformed.Store(true)
		gate.open()
	}
	swapCPUReader(t, overBudgetOnceLeaderIsDead(scope))

	var stderr bytes.Buffer
	result, err := confineWithDeps(context.Background(), ConfineRequest{
		Slice: "finite.slice", Argv: []string{"/bin/sh", "-c", "exit 7"},
		CPUTimeout: aira138CPUBudget, SelfPath: os.Args[0],
		Stdin: gate, Stderr: &stderr,
	}, deadlineConfineDeps(t, scope))
	if err != nil {
		t.Fatalf("confine: %v (stderr=%q)", err, stderr.String())
	}
	if scope.gateLiveness() != processUnknown {
		t.Fatalf("the leader read %v at the kill gate, not processUnknown; the state under test was not reached", scope.gateLiveness())
	}
	if result.Status.CPUTimeout != ConfineDeadlineFiredUnevaluated {
		t.Fatalf("cpu-timeout state = %q, want %q: an unestablished leader liveness must refuse the arbitration",
			result.Status.CPUTimeout, ConfineDeadlineFiredUnevaluated)
	}
	if result.Exit != 7 {
		t.Fatalf("exit = %d, want the child's own 7", result.Exit)
	}
}

// The bounded drain: when the arbitrated arm's wait does not arrive within
// confineArbitrationWaitBound the run degrades to `fired-unevaluated`, NEVER to
// a kill claim. An expired bound can only produce an honest "unevaluated".
//
// verifies: AIRA-138
func TestAIRA138DeadlineDrainBoundExpiryDegradesToUnevaluated(t *testing.T) {
	scope := &deadlineConfineScope{}
	// Deliberately NOT released at the gate, and held far beyond the 250ms drain
	// bound, so the bound is what expires rather than the harness.
	gate := newDeadlineGatedStdin(aira126Scale(3 * time.Second))
	swapCPUReader(t, overBudgetOnceLeaderIsDead(scope))

	var stderr bytes.Buffer
	started := time.Now()
	result, err := confineWithDeps(context.Background(), ConfineRequest{
		Slice: "finite.slice", Argv: []string{"/bin/sh", "-c", "exit 7"},
		CPUTimeout: aira138CPUBudget, SelfPath: os.Args[0],
		Stdin: gate, Stderr: &stderr,
	}, deadlineConfineDeps(t, scope))
	if err != nil {
		t.Fatalf("confine: %v (stderr=%q)", err, stderr.String())
	}
	if scope.gateLiveness() != processDead {
		t.Fatalf("the leader was %v at the kill gate; the arbitrated arm was never entered", scope.gateLiveness())
	}
	if elapsed := time.Since(started); elapsed < confineArbitrationWaitBound {
		t.Fatalf("the run finished in %s, faster than the drain bound %s: the bound cannot have expired",
			elapsed, confineArbitrationWaitBound)
	}
	if result.Status.CPUTimeout != ConfineDeadlineFiredUnevaluated {
		t.Fatalf("cpu-timeout state = %q, want %q after the drain bound expired", result.Status.CPUTimeout, ConfineDeadlineFiredUnevaluated)
	}
	// The exit code is still the child's own: arms A and C drain unconditionally
	// afterwards, so nothing is lost, only the arbitration's claim is withdrawn.
	if result.Exit != 7 {
		t.Fatalf("exit = %d, want the child's own 7", result.Exit)
	}
	if got := scope.kills(); got != 1 {
		t.Fatalf("cgroup.kill was written %d times; want exactly 1 (the teardown): an expired bound must never become a kill claim", got)
	}
}

// blockingDiagnostics blocks the ONE deadline line until the test releases it,
// and passes everything else straight through. It is how "the kill is not gated
// on the write" becomes an executed assertion rather than a code-reading claim:
// `diagnostics` is the confineLockedWriter shared with the child's stderr pump
// and can block behind a stalled reader, so an implementation that logged before
// killing would never kill at all here.
type blockingDiagnostics struct {
	mu       sync.Mutex
	buf      bytes.Buffer
	blockOn  string
	released chan struct{}
	seen     chan struct{}
	once     sync.Once
}

func (w *blockingDiagnostics) Write(p []byte) (int, error) {
	if strings.Contains(string(p), w.blockOn) {
		w.once.Do(func() { close(w.seen) })
		<-w.released
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.buf.Write(p)
}

func (w *blockingDiagnostics) String() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.buf.String()
}

// verifies: AIRA-138 — act first, log second. The kill must already have
// happened when the fire-time diagnostic starts blocking.
func TestAIRA138DeadlineKillIsNotGatedOnItsDiagnostic(t *testing.T) {
	populated := true
	scope := &deadlineConfineScope{subtreePopulated: &populated}
	gate := newDeadlineGatedStdin(aira126Scale(10 * time.Second))
	scope.onFirstEmpty = gate.open
	swapCPUReader(t, overBudgetOnceLeaderIsDead(scope))

	diagnostics := &blockingDiagnostics{
		blockOn: "fired;", released: make(chan struct{}), seen: make(chan struct{}),
	}
	done := make(chan ConfineResult, 1)
	failed := make(chan error, 1)
	go func() {
		result, err := confineWithDeps(context.Background(), ConfineRequest{
			Slice: "finite.slice", Argv: []string{"/bin/sh", "-c", "exit 7"},
			CPUTimeout: aira138CPUBudget, SelfPath: os.Args[0],
			Stdin: gate, Stderr: diagnostics,
		}, deadlineConfineDeps(t, scope))
		if err != nil {
			failed <- err
			return
		}
		done <- result
	}()
	select {
	case <-diagnostics.seen:
	case err := <-failed:
		t.Fatalf("confine: %v", err)
	case <-testdeadline.After(20 * time.Second):
		t.Fatal("the deadline never wrote its fire-time diagnostic")
	}
	// The write is blocked RIGHT NOW. The kill must already be done.
	if got := scope.kills(); got < 1 {
		t.Fatal("the cgroup.kill write had not happened when the diagnostic blocked: the kill is gated on a writer that can stall")
	}
	close(diagnostics.released)
	select {
	case result := <-done:
		if result.Status.CPUTimeout != ConfineDeadlineFiredKillCompleted {
			t.Fatalf("cpu-timeout state = %q, want %q", result.Status.CPUTimeout, ConfineDeadlineFiredKillCompleted)
		}
	case err := <-failed:
		t.Fatalf("confine: %v", err)
	case <-testdeadline.After(20 * time.Second):
		t.Fatal("confine never returned after the diagnostic was released")
	}
}

// verifies: AIRA-138 — a confine that asked for no bound renders a trailer with
// no deadline field at all, so every pre-AIRA-138 trailer is byte-identical.
func TestAIRA138TrailerUnchangedWithoutABound(t *testing.T) {
	t.Parallel()
	base := ConfineStatus{
		Slice: "aira.slice", Containment: ConfineContainmentEnforced, Cap: ConfineCapEnforced,
		Scope: ConfineScopePlaced, OOMGroup: ConfineOOMGroupSet, Priorities: ConfinePrioritiesApplied,
		TerminatedBy: ConfineTerminatedNormal,
	}
	unbounded := FormatConfineStatus(base)
	for _, forbidden := range []string{"timeout=", "cpu-timeout="} {
		if strings.Contains(unbounded, forbidden) {
			t.Fatalf("a run that asked for no bound rendered %q: %s", forbidden, unbounded)
		}
	}
	// And a requested bound APPENDS exactly one field, leaving every other byte
	// of the trailer untouched.
	bounded := base
	bounded.CPUTimeoutBudget = 10 * time.Minute
	bounded.CPUTimeout = ConfineDeadlineNotReached
	want := strings.Replace(unbounded, " terminated-by="+ConfineTerminatedNormal,
		" terminated-by="+ConfineTerminatedNormal+" cpu-timeout=10m0s:not-reached", 1)
	if got := FormatConfineStatus(bounded); got != want {
		t.Fatalf("bounded trailer\n got %q\nwant %q", got, want)
	}
	// A requested bound whose state was never established renders `unevaluated`
	// rather than vanishing: silence there would be indistinguishable from never
	// having asked.
	unevaluated := base
	unevaluated.TimeoutBudget = 30 * time.Minute
	if got := FormatConfineStatus(unevaluated); !strings.Contains(got, " timeout=30m0s:unevaluated") {
		t.Fatalf("a requested bound with no established state did not render unevaluated: %s", got)
	}
}

// verifies: AIRA-138 — ci-shim mode REFUSES both bounds, fail-closed. There is
// no cpu.stat to measure a CPU budget against and no cgroup.kill to enforce a
// wall bound with, so a bound accepted there would be a silent no-op the
// operator would read as enforcement.
func TestAIRA138ShimModeRefusesBothBounds(t *testing.T) {
	for _, test := range []struct {
		name    string
		request ConfineRequest
	}{
		{"wall", ConfineRequest{Timeout: time.Minute}},
		{"cpu", ConfineRequest{CPUTimeout: time.Minute}},
		{"both", ConfineRequest{Timeout: time.Minute, CPUTimeout: time.Minute}},
	} {
		t.Run(test.name, func(t *testing.T) {
			request := test.request
			request.Argv = []string{"/bin/true"}
			request.SelfPath = os.Args[0]
			request.Stderr = io.Discard
			_, err := confineWithDeps(context.Background(), request, shimUnitDeps())
			if err == nil || !strings.Contains(err.Error(), "E_CONFINE_ARGUMENT_INVALID") {
				t.Fatalf("ci-shim accepted a bound it cannot honour: err=%v", err)
			}
		})
	}
}
