package runner

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"testing"
	"time"
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
//
// It is env-gated ONLY so that this plan-phase commit does not leave a red test
// in the tree for other agents; AIRA-131's implementation phase deletes the
// gate and this becomes an always-on test. See
// docs/superpowers/plans/2026-09-06-aira131-detached-timeout-arbitration-plan.md §4.
func TestAIRA131DetachedTimeoutAgainstAlreadyExitedLeaderArbitratesToExited(t *testing.T) {
	if os.Getenv("AIRA131_REPRO") != "1" {
		t.Skip("set AIRA131_REPRO=1 to run the AIRA-131 detached-timeout reproduction (red until the fix lands)")
	}
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
