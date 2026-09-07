package runner

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"aira/internal/testdeadline"
)

// AIRA-131 is AIRA-126's arbitration on the DETACHED launch path.
//
// A detached child is placed with CLONE_INTO_CGROUP while its supervisor stays
// outside the run scope, so the scope can be genuinely empty when the detached
// wall-clock deadline fires: killWithIntent publishes a durable intent,
// killScope returns at len(pids)==0 BEFORE Terminate and before Kill
// (Started:false, Empty:true, nil error), and attempt.Kill.Completed is false.
//
// launchDetachedValidated's timeout branch is STRICTER than the foreground one
// ever was: it returns U_RUN_RECONCILE_REQUIRED without even draining waitCh,
// so the leader's real, already-available exit evidence is discarded rather
// than merely unread.
//
// This is the committed reproduction. It is deliberately DETERMINISTIC rather
// than a soak: gatedStdin (see timeout_arbitration_linux_test.go) withholds the
// wait OUTCOME from the supervisor while os/exec has already reaped the child,
// so the timer branch is the branch the select takes, the scope really is
// empty, and processLive really reports the leader dead — the exact production
// state, with only the moment the supervisor LEARNS of the exit controlled.
func TestAIRA131DetachedTimeoutAgainstAlreadyExitedLeaderArbitratesToExited(t *testing.T) {
	bootID, err := readBootIDFn()
	if err != nil || bootID == "" {
		t.Skipf("kernel boot id unavailable: %v", err)
	}
	scope := &livenessScope{}
	r, err := New(Config{
		CommonDir: t.TempDir(),
		Backend:   &livenessBackend{scope: scope},
		Grace:     aira126Scale(2 * time.Second),
		TermGrace: 20 * time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	var child *os.Process
	r.startFn = func(cmd *exec.Cmd) error {
		// livenessScope has no cgroup fd, so clone3's CLONE_INTO_CGROUP must be
		// stripped here or Start fails outright.
		cmd.SysProcAttr.UseCgroupFD, cmd.SysProcAttr.CgroupFD = false, 0
		cmd.Stdin = gatedStdin{hold: aira126Scale(900 * time.Millisecond)}
		if err := cmd.Start(); err != nil {
			return err
		}
		scope.adopt(PIDIdentity{PID: cmd.Process.Pid, StartTick: processStartTick(cmd.Process.Pid), BootID: bootID})
		child = cmd.Process
		return nil
	}
	t.Cleanup(func() {
		if child != nil {
			_ = child.Kill()
		}
	})

	readyR, readyW, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	ackR, ackW, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	req := Request{
		Argv:        []string{"/bin/sleep", "0.03"},
		Detach:      true,
		Timeout:     aira126Scale(300 * time.Millisecond),
		detachReady: &detachSignal{file: readyW},
		detachAck:   ackR,
	}
	type outcome struct {
		record *RunRecord
		err    error
	}
	done := make(chan outcome, 1)
	go func() {
		record, launchError := r.launchDetachedValidated(context.Background(), req, nil, t.TempDir(), []string{}, "digest", "none", req.Argv, bootID)
		done <- outcome{record: record, err: launchError}
	}()
	var ready detachReadyMessage
	if err := json.NewDecoder(readyR).Decode(&ready); err != nil || ready.ID == "" {
		t.Fatalf("readiness=%+v err=%v", ready, err)
	}
	if _, err := ackW.Write([]byte{1}); err != nil {
		t.Fatal(err)
	}
	_ = ackW.Close()

	result := <-done
	if result.err != nil {
		t.Fatalf("the detached deadline discarded a real exit: err=%v record=%+v", result.err, result.record)
	}
	record := result.record
	if record == nil {
		t.Fatal("no record was published")
	}
	if record.Status != StatusExited || record.ExitCode == nil || *record.ExitCode != 0 {
		t.Fatalf("the leader's own exit was not honoured: %+v", record)
	}
	if !record.KillIntent.Present {
		t.Fatal("vacuous: the detached timer branch never fired, so nothing was arbitrated")
	}
	// The deadline DID fire: the published intent must be dispositioned
	// not-executed, never completed, no signal may be recorded as sent, and no
	// termination or timeout may be asserted.
	if !record.KillIntent.NotExecuted || record.KillIntent.Completed || record.ScopeKill.Started {
		t.Fatalf("undisposed or over-claimed intent: %+v", record)
	}
	if containsString(record.ErrorCodes, "E_RUN_TIMEOUT") || containsString(record.ErrorCodes, "U_RUN_RECONCILE_REQUIRED") {
		t.Fatalf("a kill that delivered nothing was reported as a timeout: %+v", record)
	}
	if terminated, killed := scope.signalled(); terminated || killed {
		t.Fatalf("a signal was sent after all (terminate=%v kill=%v)", terminated, killed)
	}
	if got := terminalRecords(t, r); got != 1 {
		t.Fatalf("terminal records=%d", got)
	}
	// The disposition must be durable, not present only on the return path.
	current, err := r.Get(record.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !current.KillIntent.NotExecuted || current.KillIntent.Completed || !current.Status.Terminal() {
		t.Fatalf("ledger disagrees with the returned record: %+v", current)
	}
	if current.ExitCode == nil || *current.ExitCode != 0 || current.Status != StatusExited {
		t.Fatalf("the durable record lost the leader's exit: %+v", current)
	}
	var typed *LaunchError
	if errors.As(result.err, &typed) {
		t.Fatalf("unexpected typed launch error: %v", typed)
	}
}

// TestAIRA131DetachedLiveLeaderAtDeadlineStillTimesOut is T2. The scope reports
// empty from its second Members() call onward (an escape / unverified
// placement shape) while the real leader stays alive, so killScope returns the
// SAME {Empty:true, Started:false} shape T1 arbitrates on — but processLive
// must report the leader ALIVE here, and the fix must not arbitrate. This is
// the test that stops AIRA-131 being widened into "an empty scope means the
// run finished".
func TestAIRA131DetachedLiveLeaderAtDeadlineStillTimesOut(t *testing.T) {
	bootID, err := readBootIDFn()
	if err != nil || bootID == "" {
		t.Skipf("kernel boot id unavailable: %v", err)
	}
	scope := &livenessScope{emptyAfterFirstMembers: true}
	r, err := New(Config{
		CommonDir: t.TempDir(),
		Backend:   &livenessBackend{scope: scope},
		Grace:     aira126Scale(300 * time.Millisecond),
		TermGrace: 20 * time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	var child *os.Process
	r.startFn = func(cmd *exec.Cmd) error {
		cmd.SysProcAttr.UseCgroupFD, cmd.SysProcAttr.CgroupFD = false, 0
		if err := cmd.Start(); err != nil {
			return err
		}
		scope.adopt(PIDIdentity{PID: cmd.Process.Pid, StartTick: processStartTick(cmd.Process.Pid), BootID: bootID})
		child = cmd.Process
		return nil
	}
	t.Cleanup(func() {
		if child != nil {
			_ = child.Kill()
		}
	})

	readyR, readyW, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	ackR, ackW, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	req := Request{
		Argv:        []string{"/bin/sleep", "30"},
		Detach:      true,
		Timeout:     aira126Scale(200 * time.Millisecond),
		detachReady: &detachSignal{file: readyW},
		detachAck:   ackR,
	}
	type outcome struct {
		record *RunRecord
		err    error
	}
	done := make(chan outcome, 1)
	go func() {
		record, launchError := r.launchDetachedValidated(context.Background(), req, nil, t.TempDir(), []string{}, "digest", "none", req.Argv, bootID)
		done <- outcome{record: record, err: launchError}
	}()
	var ready detachReadyMessage
	if err := json.NewDecoder(readyR).Decode(&ready); err != nil || ready.ID == "" {
		t.Fatalf("readiness=%+v err=%v", ready, err)
	}
	if _, err := ackW.Write([]byte{1}); err != nil {
		t.Fatal(err)
	}
	_ = ackW.Close()

	result := <-done
	var launchError *LaunchError
	if !errors.As(result.err, &launchError) || launchError.Code != "U_RUN_RECONCILE_REQUIRED" {
		t.Fatalf("live leader at the deadline did not stay unevaluated: err=%v record=%+v", result.err, result.record)
	}
	if result.record != nil {
		t.Fatalf("the unevaluated arm published a record instead of nil: %+v", result.record)
	}
	current, err := r.Get(ready.ID)
	if err != nil {
		t.Fatal(err)
	}
	if current.Status == StatusExited || current.TerminalComplete || current.KillIntent.NotExecuted {
		t.Fatalf("a live, unkilled leader was arbitrated away: %+v", current)
	}
	if !current.KillIntent.Present || current.KillIntent.Completed {
		t.Fatalf("the deadline's intent evidence was lost: %+v", current)
	}
	if got := terminalRecords(t, r); got != 0 {
		t.Fatalf("terminal records=%d", got)
	}
}

// TestAIRA131DetachedForeignKillIntentIsNotDismissed is T3. A kill intent is
// seeded from OUTSIDE the timeout path, after the run's "running" event and
// before the deadline, so killWithIntent ADOPTS it instead of creating it.
// IntentCreated is then false, so decideTimeoutIntentNotExecuted must refuse —
// a launch may never disposition an intent it did not itself publish.
func TestAIRA131DetachedForeignKillIntentIsNotDismissed(t *testing.T) {
	bootID, err := readBootIDFn()
	if err != nil || bootID == "" {
		t.Skipf("kernel boot id unavailable: %v", err)
	}
	scope := &livenessScope{}
	r, err := New(Config{
		CommonDir: t.TempDir(),
		Backend:   &livenessBackend{scope: scope},
		Grace:     aira126Scale(2 * time.Second),
		TermGrace: 20 * time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	var child *os.Process
	r.startFn = func(cmd *exec.Cmd) error {
		cmd.SysProcAttr.UseCgroupFD, cmd.SysProcAttr.CgroupFD = false, 0
		cmd.Stdin = gatedStdin{hold: aira126Scale(900 * time.Millisecond)}
		if err := cmd.Start(); err != nil {
			return err
		}
		scope.adopt(PIDIdentity{PID: cmd.Process.Pid, StartTick: processStartTick(cmd.Process.Pid), BootID: bootID})
		child = cmd.Process
		return nil
	}
	t.Cleanup(func() {
		if child != nil {
			_ = child.Kill()
		}
	})

	readyR, readyW, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	ackR, ackW, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	req := Request{
		Argv:        []string{"/bin/sleep", "0.03"},
		Detach:      true,
		Timeout:     aira126Scale(300 * time.Millisecond),
		detachReady: &detachSignal{file: readyW},
		detachAck:   ackR,
	}
	type outcome struct {
		record *RunRecord
		err    error
	}
	done := make(chan outcome, 1)
	go func() {
		record, launchError := r.launchDetachedValidated(context.Background(), req, nil, t.TempDir(), []string{}, "digest", "none", req.Argv, bootID)
		done <- outcome{record: record, err: launchError}
	}()
	var ready detachReadyMessage
	if err := json.NewDecoder(readyR).Decode(&ready); err != nil || ready.ID == "" {
		t.Fatalf("readiness=%+v err=%v", ready, err)
	}
	if _, err := ackW.Write([]byte{1}); err != nil {
		t.Fatal(err)
	}
	_ = ackW.Close()

	seeded := make(chan error, 1)
	go func() { seeded <- seedForeignDetachedKillIntent(r, ready.ID) }()
	if err := <-seeded; err != nil {
		t.Fatal(err)
	}

	result := <-done
	var launchError *LaunchError
	if !errors.As(result.err, &launchError) || launchError.Code != "U_RUN_RECONCILE_REQUIRED" {
		t.Fatalf("a foreign intent was dismissed: err=%v record=%+v", result.err, result.record)
	}
	current, err := r.Get(ready.ID)
	if err != nil {
		t.Fatal(err)
	}
	if current.KillIntent.NotExecuted {
		t.Fatalf("a launch dispositioned an intent it did not create: %+v", current)
	}
	if got := terminalRecords(t, r); got != 0 {
		t.Fatalf("terminal records=%d", got)
	}
}

// seedForeignDetachedKillIntent waits for the run's "running" event (a plain
// event replaces the replayed record wholesale, so an intent seeded before it
// would simply be erased) and publishes a kill intent under the run lock, as an
// external `aira run kill` would.
func seedForeignDetachedKillIntent(r *Runner, id string) error {
	backstop := time.Now().Add(testdeadline.Wait(20 * time.Second))
	for time.Now().Before(backstop) {
		events, err := r.ledger.read()
		if err != nil {
			return err
		}
		seenRunning := false
		for _, event := range events {
			if event.Kind == "running" && event.Run.ID == id {
				seenRunning = true
			}
		}
		if !seenRunning {
			time.Sleep(testdeadline.PollInterval)
			continue
		}
		lock, lockErr := lockFile(filepath.Join(filepath.Dir(r.ledger.ledger), id+".lock"))
		if lockErr != nil {
			return lockErr
		}
		current, currentErr := r.ledger.current(id)
		if currentErr == nil {
			current.KillIntent = KillIntent{Present: true}
			_, currentErr = r.ledger.append(ledgerEvent{Kind: "kill-intent", Run: current})
		}
		_ = unlockFile(lock)
		return currentErr
	}
	return errors.New("the run never published a running event to seed against")
}

// TestAIRA131DetachedCompletedIntentIsNotDowngradedByNotExecuted is T4. It
// pins the answer to the plan's §5 open design question: appendDetachedEvidenceLocked's
// Status.Terminal()-only guard is NOT the CAS in force here. A concurrent
// actor appends a NON-terminal kill-completed record — carrying
// KillIntent.Completed=true — into the window between killWithIntent's
// kill-intent publish and appendDetachedNotExecutedLocked's own read+append.
// The concurrent writer must be a DIRECT locked append, not a real Kill(): in
// the arbitrated state the scope is empty by construction, so a real Kill()
// finds nothing and finishDetachedKill appends nothing — a Kill()-driven T4
// would be vacuous. This simulates exactly what finishDetachedKill writes when
// the scope is populated.
func TestAIRA131DetachedCompletedIntentIsNotDowngradedByNotExecuted(t *testing.T) {
	bootID, err := readBootIDFn()
	if err != nil || bootID == "" {
		t.Skipf("kernel boot id unavailable: %v", err)
	}
	scope := &livenessScope{}
	r, err := New(Config{
		CommonDir: t.TempDir(),
		Backend:   &livenessBackend{scope: scope},
		Grace:     aira126Scale(2 * time.Second),
		TermGrace: 20 * time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	var child *os.Process
	r.startFn = func(cmd *exec.Cmd) error {
		cmd.SysProcAttr.UseCgroupFD, cmd.SysProcAttr.CgroupFD = false, 0
		cmd.Stdin = gatedStdin{hold: aira126Scale(900 * time.Millisecond)}
		if err := cmd.Start(); err != nil {
			return err
		}
		scope.adopt(PIDIdentity{PID: cmd.Process.Pid, StartTick: processStartTick(cmd.Process.Pid), BootID: bootID})
		child = cmd.Process
		return nil
	}
	t.Cleanup(func() {
		if child != nil {
			_ = child.Kill()
		}
	})

	readyR, readyW, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	ackR, ackW, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	req := Request{
		Argv:        []string{"/bin/sleep", "0.03"},
		Detach:      true,
		Timeout:     aira126Scale(300 * time.Millisecond),
		detachReady: &detachSignal{file: readyW},
		detachAck:   ackR,
	}
	type outcome struct {
		record *RunRecord
		err    error
	}
	done := make(chan outcome, 1)
	go func() {
		record, launchError := r.launchDetachedValidated(context.Background(), req, nil, t.TempDir(), []string{}, "digest", "none", req.Argv, bootID)
		done <- outcome{record: record, err: launchError}
	}()
	var ready detachReadyMessage
	if err := json.NewDecoder(readyR).Decode(&ready); err != nil || ready.ID == "" {
		t.Fatalf("readiness=%+v err=%v", ready, err)
	}
	if _, err := ackW.Write([]byte{1}); err != nil {
		t.Fatal(err)
	}
	_ = ackW.Close()

	type completion struct {
		intent KillIntent
		err    error
	}
	completed := make(chan completion, 1)
	go func() { completed <- concurrentlyCompleteDetachedIntent(r, ready.ID) }()

	result := <-done
	outcome2 := <-completed
	if outcome2.err != nil {
		t.Fatal(outcome2.err)
	}
	var launchError *LaunchError
	if !errors.As(result.err, &launchError) || launchError.Code != "U_RUN_RECONCILE_REQUIRED" {
		t.Fatalf("the concurrent completion was not honoured: err=%v record=%+v", result.err, result.record)
	}
	current, err := r.Get(ready.ID)
	if err != nil {
		t.Fatal(err)
	}
	if current.KillIntent != outcome2.intent {
		t.Fatalf("the concurrent actor's completed intent was downgraded: ledger=%+v want=%+v", current.KillIntent, outcome2.intent)
	}
	if !current.ScopeKill.Completed {
		t.Fatalf("the concurrent actor's scope-kill evidence was lost: %+v", current)
	}
}

// concurrentlyCompleteDetachedIntent polls for the kill-intent this launch's
// own timeout publishes, then appends a NON-terminal kill-completed record
// under the run lock — the same shape finishDetachedKill writes for a real
// concurrent Kill() against a populated scope.
func concurrentlyCompleteDetachedIntent(r *Runner, id string) (result struct {
	intent KillIntent
	err    error
}) {
	backstop := time.Now().Add(testdeadline.Wait(20 * time.Second))
	for time.Now().Before(backstop) {
		events, err := r.ledger.read()
		if err != nil {
			result.err = err
			return result
		}
		sequence := uint64(0)
		found := false
		for _, event := range events {
			if event.Kind == "kill-intent" && event.Run.ID == id && event.Run.KillIntent.Present {
				sequence = event.Run.KillIntent.Sequence
				found = true
			}
		}
		if !found {
			time.Sleep(testdeadline.PollInterval)
			continue
		}
		intent := KillIntent{Present: true, Sequence: sequence, Completed: true, Empty: true}
		lock, lockErr := lockFile(filepath.Join(filepath.Dir(r.ledger.ledger), id+".lock"))
		if lockErr != nil {
			result.err = lockErr
			return result
		}
		current, currentErr := r.ledger.current(id)
		if currentErr == nil {
			current.KillIntent = intent
			current.ScopeKill = ScopeKill{Requested: true, Started: true, Completed: true, Actor: "concurrent-killer"}
			_, currentErr = r.ledger.append(ledgerEvent{Kind: "kill-completed", Run: current})
		}
		_ = unlockFile(lock)
		result.intent, result.err = intent, currentErr
		return result
	}
	result.err = errors.New("no kill intent was ever published to complete concurrently")
	return result
}

// TestAIRA131DetachedArbitrationDrainBoundLeavesNoDisposition is T5. Grace sits
// at arbitrationWaitFloor and the stdin hold is far beyond it, so the bounded
// drain expires before gatedStdin ever delivers. Expiry must leave waitCh
// UNTOUCHED and publish no durable disposition at all — the timed-out case
// stays byte-for-byte today's.
func TestAIRA131DetachedArbitrationDrainBoundLeavesNoDisposition(t *testing.T) {
	bootID, err := readBootIDFn()
	if err != nil || bootID == "" {
		t.Skipf("kernel boot id unavailable: %v", err)
	}
	scope := &livenessScope{}
	r, err := New(Config{
		CommonDir: t.TempDir(),
		Backend:   &livenessBackend{scope: scope},
		// Below arbitrationWaitFloor so arbitrationWaitBound clamps to the floor —
		// the drain bound this test needs the stdin hold to outlast.
		Grace:     aira126Scale(10 * time.Millisecond),
		TermGrace: 20 * time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	var child *os.Process
	r.startFn = func(cmd *exec.Cmd) error {
		cmd.SysProcAttr.UseCgroupFD, cmd.SysProcAttr.CgroupFD = false, 0
		// Held far beyond arbitrationWaitFloor (250ms) so the drain bound expires
		// first, deterministically, even under -race / a loaded box.
		cmd.Stdin = gatedStdin{hold: aira126Scale(2 * time.Second)}
		if err := cmd.Start(); err != nil {
			return err
		}
		scope.adopt(PIDIdentity{PID: cmd.Process.Pid, StartTick: processStartTick(cmd.Process.Pid), BootID: bootID})
		child = cmd.Process
		return nil
	}
	t.Cleanup(func() {
		if child != nil {
			_ = child.Kill()
		}
	})

	readyR, readyW, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	ackR, ackW, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	req := Request{
		Argv:        []string{"/bin/sleep", "0.03"},
		Detach:      true,
		Timeout:     aira126Scale(300 * time.Millisecond),
		detachReady: &detachSignal{file: readyW},
		detachAck:   ackR,
	}
	type outcome struct {
		record *RunRecord
		err    error
	}
	done := make(chan outcome, 1)
	go func() {
		record, launchError := r.launchDetachedValidated(context.Background(), req, nil, t.TempDir(), []string{}, "digest", "none", req.Argv, bootID)
		done <- outcome{record: record, err: launchError}
	}()
	var ready detachReadyMessage
	if err := json.NewDecoder(readyR).Decode(&ready); err != nil || ready.ID == "" {
		t.Fatalf("readiness=%+v err=%v", ready, err)
	}
	if _, err := ackW.Write([]byte{1}); err != nil {
		t.Fatal(err)
	}
	_ = ackW.Close()

	start := time.Now()
	result := <-done
	elapsed := time.Since(start)
	var launchError *LaunchError
	if !errors.As(result.err, &launchError) || launchError.Code != "U_RUN_RECONCILE_REQUIRED" {
		t.Fatalf("a drain-bound expiry did not stay unevaluated: err=%v record=%+v", result.err, result.record)
	}
	// The drain bound (arbitrationWaitFloor=250ms), not the full 2s stdin hold,
	// must be what the launch waited on.
	if elapsed >= aira126Scale(1500*time.Millisecond) {
		t.Fatalf("the launch waited past the drain bound: elapsed=%v", elapsed)
	}
	current, err := r.Get(ready.ID)
	if err != nil {
		t.Fatal(err)
	}
	if current.KillIntent.NotExecuted {
		t.Fatalf("a drain that expired still published a disposition: %+v", current)
	}
	events, err := r.ledger.read()
	if err != nil {
		t.Fatal(err)
	}
	for _, event := range events {
		if event.Kind == "kill-not-executed" {
			t.Fatalf("a drain-bound expiry published a kill-not-executed event: %+v", event)
		}
	}
	if got := terminalRecords(t, r); got != 0 {
		t.Fatalf("terminal records=%d", got)
	}
}

// signallingScope extends livenessScope so Terminate/Kill genuinely reach the
// adopted leader — T6 needs the real child to actually exit once "killed",
// unlike every other test here where the scope model alone is enough.
type signallingScope struct{ livenessScope }

func (s *signallingScope) Terminate(pids []int) error {
	for _, pid := range pids {
		_ = syscall.Kill(pid, syscall.SIGTERM)
	}
	return s.livenessScope.Terminate(pids)
}

func (s *signallingScope) Kill() error {
	s.mu.Lock()
	pid := s.leader.PID
	s.mu.Unlock()
	if pid > 0 {
		_ = syscall.Kill(pid, syscall.SIGKILL)
	}
	return s.livenessScope.Kill()
}

// TestAIRA131DetachedTimeoutAgainstLiveLeaderStillKills is T6, the pre-existing
// "proven-kill" arm: a genuinely populated scope whose Terminate/Kill really
// signal the leader. This closes the plan's §2.2(2) coverage gap (the detached
// timeout branch had ZERO tests before AIRA-131) and pins that this ticket's
// change does not regress the branch's other arm.
func TestAIRA131DetachedTimeoutAgainstLiveLeaderStillKills(t *testing.T) {
	bootID, err := readBootIDFn()
	if err != nil || bootID == "" {
		t.Skipf("kernel boot id unavailable: %v", err)
	}
	scope := &signallingScope{}
	r, err := New(Config{
		CommonDir: t.TempDir(),
		Backend:   &signallingBackend{scope: scope},
		Grace:     aira126Scale(2 * time.Second),
		TermGrace: aira126Scale(50 * time.Millisecond),
	})
	if err != nil {
		t.Fatal(err)
	}
	var child *os.Process
	r.startFn = func(cmd *exec.Cmd) error {
		cmd.SysProcAttr.UseCgroupFD, cmd.SysProcAttr.CgroupFD = false, 0
		if err := cmd.Start(); err != nil {
			return err
		}
		scope.adopt(PIDIdentity{PID: cmd.Process.Pid, StartTick: processStartTick(cmd.Process.Pid), BootID: bootID})
		child = cmd.Process
		return nil
	}
	t.Cleanup(func() {
		if child != nil {
			_ = child.Kill()
		}
	})

	readyR, readyW, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	ackR, ackW, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	req := Request{
		Argv:        []string{"/bin/sleep", "30"},
		Detach:      true,
		Timeout:     aira126Scale(200 * time.Millisecond),
		detachReady: &detachSignal{file: readyW},
		detachAck:   ackR,
	}
	type outcome struct {
		record *RunRecord
		err    error
	}
	done := make(chan outcome, 1)
	go func() {
		record, launchError := r.launchDetachedValidated(context.Background(), req, nil, t.TempDir(), []string{}, "digest", "none", req.Argv, bootID)
		done <- outcome{record: record, err: launchError}
	}()
	var ready detachReadyMessage
	if err := json.NewDecoder(readyR).Decode(&ready); err != nil || ready.ID == "" {
		t.Fatalf("readiness=%+v err=%v", ready, err)
	}
	if _, err := ackW.Write([]byte{1}); err != nil {
		t.Fatal(err)
	}
	_ = ackW.Close()

	result := <-done
	if result.err != nil {
		t.Fatalf("the proven-kill arm returned an error: err=%v record=%+v", result.err, result.record)
	}
	record := result.record
	if record == nil {
		t.Fatal("no record was published")
	}
	if record.Status != StatusKilled {
		t.Fatalf("a genuinely killed leader was not reported killed: %+v", record)
	}
	if !record.KillIntent.Completed || record.KillIntent.NotExecuted {
		t.Fatalf("the proven kill lost its intent evidence: %+v", record)
	}
	if !record.ScopeKill.Completed || record.ScopeKill.Actor != "run-timeout" {
		t.Fatalf("the proven kill lost its scope-kill evidence: %+v", record)
	}
	if !containsString(record.ErrorCodes, "E_RUN_TIMEOUT") {
		t.Fatalf("a genuine timeout kill lost E_RUN_TIMEOUT: %+v", record)
	}
	if got := terminalRecords(t, r); got != 1 {
		t.Fatalf("terminal records=%d", got)
	}
}

// signallingBackend hands out the SAME *signallingScope every time, exactly as
// livenessBackend does for *livenessScope.
type signallingBackend struct{ scope *signallingScope }

func (b *signallingBackend) Probe(context.Context) error                   { return nil }
func (b *signallingBackend) Create(context.Context, string) (Scope, error) { return b.scope, nil }
func (b *signallingBackend) Open(context.Context, string) (Scope, error)   { return b.scope, nil }

// TestAIRA131DetachedStdinConnectRefusedDispositionLeavesLedgerIntact is T7,
// pinning the plan gate's blocking finding: under --stdin-connect, a drain that
// SUCCEEDS followed by a REFUSED disposition (T4's shape) must not strand the
// input-plane abort defer (detach_linux.go:374-392) into its 2-second stall and
// unlocked input-abort-incomplete append, which would durably erase the
// concurrent actor's completed kill-intent. leaderReaped=true at the drain
// receive (plan §6.3) is what closes this.
func TestAIRA131DetachedStdinConnectRefusedDispositionLeavesLedgerIntact(t *testing.T) {
	bootID, err := readBootIDFn()
	if err != nil || bootID == "" {
		t.Skipf("kernel boot id unavailable: %v", err)
	}
	scope := &livenessScope{}
	runtimeDir := newRunInputRuntimeDir(t)
	r, err := New(Config{
		CommonDir:       t.TempDir(),
		Backend:         &livenessBackend{scope: scope},
		Grace:           aira126Scale(2 * time.Second),
		TermGrace:       20 * time.Millisecond,
		InputRuntimeDir: runtimeDir,
	})
	if err != nil {
		t.Fatal(err)
	}
	hold := aira126Scale(900 * time.Millisecond)
	var child *os.Process
	r.startFn = func(cmd *exec.Cmd) error {
		cmd.SysProcAttr.UseCgroupFD, cmd.SysProcAttr.CgroupFD = false, 0
		// T1's device: overwrite production's *os.File stdin (which os/exec would
		// dup rather than copy, leaving no goroutine to hold Wait() open) with the
		// gated reader, restoring the reaped-but-outcome-pending window this test
		// depends on. The StdinConnect arm — and its :374-392 defer — is still
		// genuinely taken; only what fills cmd.Stdin is swapped.
		cmd.Stdin = gatedStdin{hold: hold}
		if err := cmd.Start(); err != nil {
			return err
		}
		scope.adopt(PIDIdentity{PID: cmd.Process.Pid, StartTick: processStartTick(cmd.Process.Pid), BootID: bootID})
		child = cmd.Process
		return nil
	}
	t.Cleanup(func() {
		if child != nil {
			_ = child.Kill()
		}
	})

	readyR, readyW, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	ackR, ackW, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	req := Request{
		Argv:         []string{"/bin/sleep", "0.03"},
		Detach:       true,
		StdinConnect: true,
		Timeout:      aira126Scale(300 * time.Millisecond),
		detachReady:  &detachSignal{file: readyW},
		detachAck:    ackR,
	}
	type outcome struct {
		record *RunRecord
		err    error
	}
	done := make(chan outcome, 1)
	go func() {
		record, launchError := r.launchDetachedValidated(context.Background(), req, nil, t.TempDir(), []string{}, "digest", "none", req.Argv, bootID)
		done <- outcome{record: record, err: launchError}
	}()
	var ready detachReadyMessage
	if err := json.NewDecoder(readyR).Decode(&ready); err != nil || ready.ID == "" {
		t.Fatalf("readiness=%+v err=%v", ready, err)
	}
	if _, err := ackW.Write([]byte{1}); err != nil {
		t.Fatal(err)
	}
	_ = ackW.Close()

	type completion struct {
		intent KillIntent
		err    error
	}
	completed := make(chan completion, 1)
	go func() { completed <- concurrentlyCompleteDetachedIntent(r, ready.ID) }()

	start := time.Now()
	result := <-done
	elapsed := time.Since(start)
	outcome2 := <-completed
	if outcome2.err != nil {
		t.Fatal(outcome2.err)
	}
	var launchError *LaunchError
	if !errors.As(result.err, &launchError) || launchError.Code != "U_RUN_RECONCILE_REQUIRED" {
		t.Fatalf("the refused disposition was not reported: err=%v record=%+v", result.err, result.record)
	}
	// Must return once the (scaled) hold delivers, not after the abort defer's
	// own extra stall — a FIXED, UNSCALED 2s in production (detach_linux.go:383)
	// — on top of it. The margin is unscaled and well under that 2s so this
	// stays a tight regression signal at any testdeadline.Scale().
	if bound := hold + time.Second; elapsed >= bound {
		t.Fatalf("the launch stalled past the drain bound (input-plane defer regression?): elapsed=%v want<%v", elapsed, bound)
	}
	events, err := r.ledger.read()
	if err != nil {
		t.Fatal(err)
	}
	for _, event := range events {
		if event.Kind == "input-abort-incomplete" {
			t.Fatalf("the input-plane abort defer fired despite the drain succeeding: %+v", event)
		}
	}
	current, err := r.Get(ready.ID)
	if err != nil {
		t.Fatal(err)
	}
	if current.KillIntent != outcome2.intent {
		t.Fatalf("the concurrent actor's kill-intent did not survive byte for byte: ledger=%+v want=%+v", current.KillIntent, outcome2.intent)
	}
	if !current.ScopeKill.Completed || current.ScopeKill.Actor != "concurrent-killer" {
		t.Fatalf("the concurrent actor's scope-kill evidence was lost: %+v", current)
	}
	if got := terminalRecords(t, r); got != 0 {
		t.Fatalf("terminal records=%d", got)
	}
}

// TestAIRA131DetachedEscapedLeaderExitingInsideDrainWindowStaysUnevaluated is
// T8, added by the build review. It pins the liveness read AT THE CALL SITE —
// processLive(record.PIDIdentity), evaluated at the instant the kill found
// nothing — which T2 cannot: T2's leader lives 30s, so a mis-wired liveness
// input (a stale or forced processDead) is masked there by the drain bound
// expiring first and falling into the same U_RUN_RECONCILE_REQUIRED arm.
//
// Here the escaped leader is ALIVE at the deadline but exits INSIDE the drain
// window. "The drain succeeded" then only proves the leader was dead by the
// time the drain succeeded, NOT at the deadline — and only the conjunct
// decides. Correct behaviour: the run stays unevaluated; it must not be
// arbitrated to Exited because the leader happened to exit a moment later.
// Verified red against both `processDead` forced at the call site and the
// guard removed entirely, with T2 green under the same mutations.
//
// The leader's life is scaled with the deadline (the reverse of T1's device,
// where the child must be dead BEFORE the deadline): it must outlive the scaled
// deadline and die before the scaled drain bound at any testdeadline.Scale().
func TestAIRA131DetachedEscapedLeaderExitingInsideDrainWindowStaysUnevaluated(t *testing.T) {
	bootID, err := readBootIDFn()
	if err != nil || bootID == "" {
		t.Skipf("kernel boot id unavailable: %v", err)
	}
	scope := &livenessScope{emptyAfterFirstMembers: true}
	deadline := aira126Scale(200 * time.Millisecond)
	r, err := New(Config{
		CommonDir: t.TempDir(),
		Backend:   &livenessBackend{scope: scope},
		// The drain bound (arbitrationWaitBound(Grace)) must outlast the leader.
		Grace:     aira126Scale(2 * time.Second),
		TermGrace: 20 * time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	var child *os.Process
	r.startFn = func(cmd *exec.Cmd) error {
		cmd.SysProcAttr.UseCgroupFD, cmd.SysProcAttr.CgroupFD = false, 0
		if err := cmd.Start(); err != nil {
			return err
		}
		scope.adopt(PIDIdentity{PID: cmd.Process.Pid, StartTick: processStartTick(cmd.Process.Pid), BootID: bootID})
		child = cmd.Process
		return nil
	}
	t.Cleanup(func() {
		if child != nil {
			_ = child.Kill()
		}
	})

	readyR, readyW, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	ackR, ackW, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	// Alive at the deadline (3x margin), dead well inside the drain bound.
	req := Request{
		Argv:        []string{"/bin/sleep", fmt.Sprintf("%.3f", (3 * deadline).Seconds())},
		Detach:      true,
		Timeout:     deadline,
		detachReady: &detachSignal{file: readyW},
		detachAck:   ackR,
	}
	type outcome struct {
		record *RunRecord
		err    error
	}
	done := make(chan outcome, 1)
	go func() {
		record, launchError := r.launchDetachedValidated(context.Background(), req, nil, t.TempDir(), []string{}, "digest", "none", req.Argv, bootID)
		done <- outcome{record: record, err: launchError}
	}()
	var ready detachReadyMessage
	if err := json.NewDecoder(readyR).Decode(&ready); err != nil || ready.ID == "" {
		t.Fatalf("readiness=%+v err=%v", ready, err)
	}
	if _, err := ackW.Write([]byte{1}); err != nil {
		t.Fatal(err)
	}
	_ = ackW.Close()

	result := <-done
	var launchError *LaunchError
	if !errors.As(result.err, &launchError) || launchError.Code != "U_RUN_RECONCILE_REQUIRED" {
		t.Fatalf("a leader ALIVE at the deadline was arbitrated away because it exited inside the drain window: err=%v record=%+v", result.err, result.record)
	}
	if result.record != nil {
		t.Fatalf("the unevaluated arm published a record instead of nil: %+v", result.record)
	}
	current, err := r.Get(ready.ID)
	if err != nil {
		t.Fatal(err)
	}
	if current.KillIntent.NotExecuted || current.Status.Terminal() {
		t.Fatalf("a leader alive at the deadline was dispositioned not-executed: %+v", current)
	}
	if !current.KillIntent.Present || current.KillIntent.Completed {
		t.Fatalf("the deadline's intent evidence was lost: %+v", current)
	}
	if got := terminalRecords(t, r); got != 0 {
		t.Fatalf("terminal records=%d", got)
	}
}
