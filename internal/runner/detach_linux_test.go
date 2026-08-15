//go:build linux

package runner

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

func TestMain(m *testing.M) {
	if len(os.Args) > 1 && os.Args[1] == "__supervise" && os.Getenv("AIRA_M20_FAKE_SUPERVISOR") != "" {
		os.Exit(runM20FakeSupervisor())
	}
	os.Exit(m.Run())
}

func runM20FakeSupervisor() int {
	if len(os.Args) >= 4 && os.Args[2] == "--control" {
		_ = os.Remove(os.Args[3])
	}
	ready := os.NewFile(uintptr(3), "ready")
	ack := os.NewFile(uintptr(4), "ack")
	if ready == nil || ack == nil {
		return 2
	}
	defer ack.Close()
	switch os.Getenv("AIRA_M20_FAKE_SUPERVISOR") {
	case "timeout":
		time.Sleep(250 * time.Millisecond)
		_ = ready.Close()
		return 0
	case "failure":
		_ = json.NewEncoder(ready).Encode(detachReadyMessage{Code: "E_RUN_ARGUMENT_INVALID", Error: "injected"})
		_ = ready.Close()
		return 0
	default:
		_ = json.NewEncoder(ready).Encode(detachReadyMessage{ID: "RUN-helper"})
		_ = ready.Close()
		buffer := make([]byte, 1)
		n, _ := ack.Read(buffer)
		value := "0"
		if n == 1 {
			value = "1"
		}
		if path := os.Getenv("AIRA_M20_ACK_PATH"); path != "" {
			_ = os.WriteFile(path, []byte(value), 0o600)
		}
		return 0
	}
}

func intPointer(value int) *int { return &value }

func TestM20CancelledIsTerminal(t *testing.T) {
	if !StatusCancelled.Terminal() {
		t.Fatal("cancelled must be terminal")
	}
}

func TestM20MergeEvidenceCarriesDetachedLeaseAndWriteOnceExitZero(t *testing.T) {
	zero := 0
	base := RunRecord{ID: "RUN-1"}
	candidate := RunRecord{
		ID: "RUN-1", Detached: true,
		SupervisorPID:      PIDIdentity{PID: 41, StartTick: 99, BootID: "boot-a"},
		LeaderExitObserved: true, ExitCode: &zero,
	}
	got := mergeEvidence(base, candidate)
	if !got.Detached || got.SupervisorPID != candidate.SupervisorPID {
		t.Fatalf("detach lease was not carried: %+v", got)
	}
	got = mergeEvidence(got, RunRecord{ID: "RUN-1", SupervisorPID: PIDIdentity{PID: 88, StartTick: 100, BootID: "boot-b"}})
	if got.SupervisorPID != candidate.SupervisorPID {
		t.Fatalf("supervisor lease was overwritten: %+v", got.SupervisorPID)
	}
	if !got.LeaderExitObserved || got.ExitCode == nil || *got.ExitCode != 0 {
		t.Fatalf("exit zero evidence was not carried: %+v", got)
	}

	// A presence-bearing re-claim with no payload cannot erase the first waitpid.
	got = mergeEvidence(got, RunRecord{ID: "RUN-1", LeaderExitObserved: true})
	if got.ExitCode == nil || *got.ExitCode != 0 || got.Signal != "" {
		t.Fatalf("nil re-claim erased exit zero: %+v", got)
	}

	// A genuinely different second payload is diagnosed and the first remains.
	got = mergeEvidence(got, RunRecord{ID: "RUN-1", LeaderExitObserved: true, ExitCode: intPointer(7)})
	if got.ExitCode == nil || *got.ExitCode != 0 || !containsString(got.ErrorCodes, "U_RUN_EXIT_CONFLICT") {
		t.Fatalf("conflicting exit was not preserved+diagnosed: %+v", got)
	}
}

func TestM20ReplayLeaderExitedIsPresenceBearingAndWriteOnce(t *testing.T) {
	zero := 0
	run := RunRecord{SchemaVersion: ledgerSchema, ID: "RUN-1", Status: StatusRunning}
	events := []ledgerEvent{
		{Kind: "starting", Run: run},
		{Kind: "leader-exited", Run: run, LeaderExitObserved: true, ExitCode: &zero},
		{Kind: "leader-exited", Run: run, LeaderExitObserved: true},
	}
	for i := range events {
		events[i].Sequence = uint64(i + 1)
	}
	runs, err := replay(events)
	if err != nil {
		t.Fatal(err)
	}
	got := runs["RUN-1"]
	if !got.LeaderExitObserved || got.ExitCode == nil || *got.ExitCode != 0 {
		t.Fatalf("replay lost clean exit evidence: %+v", got)
	}
}

func TestM20ProcessLivenessBootZombieAndUnknown(t *testing.T) {
	oldBoot, oldStat := readBootIDFn, readProcStatFn
	t.Cleanup(func() { readBootIDFn, readProcStatFn = oldBoot, oldStat })
	identity := PIDIdentity{PID: 123, StartTick: 77, BootID: "boot-a"}
	readBootIDFn = func() (string, error) { return "boot-a", nil }
	readProcStatFn = func(int) ([]byte, error) { return procStatForTest('S', 77), nil }
	if got := processLive(identity); got != processAlive {
		t.Fatalf("matching live process = %v", got)
	}
	readBootIDFn = func() (string, error) { return "boot-b", nil }
	if got := processLive(identity); got != processDead {
		t.Fatalf("cross-boot identity = %v", got)
	}
	readBootIDFn = func() (string, error) { return "boot-a", nil }
	readProcStatFn = func(int) ([]byte, error) { return procStatForTest('Z', 77), nil }
	if got := processLive(identity); got != processDead {
		t.Fatalf("zombie identity = %v", got)
	}
	readBootIDFn = func() (string, error) { return "", errors.New("unreadable") }
	if got := processLive(identity); got != processUnknown {
		t.Fatalf("unreadable boot id = %v", got)
	}
	readBootIDFn = func() (string, error) { return "boot-a", nil }
	if got := processLive(PIDIdentity{PID: 123, StartTick: 77}); got != processUnknown {
		t.Fatalf("legacy identity = %v", got)
	}
	if got := processLive(PIDIdentity{PID: 123, BootID: "boot-a"}); got != processUnknown {
		t.Fatalf("incomplete identity = %v", got)
	}
	readProcStatFn = func(int) ([]byte, error) { return []byte("malformed"), nil }
	if got := processLive(identity); got != processUnknown {
		t.Fatalf("malformed proc stat = %v", got)
	}
}

func TestM20IdentityCreationFailsWithoutBootID(t *testing.T) {
	oldBoot := readBootIDFn
	t.Cleanup(func() { readBootIDFn = oldBoot })
	readBootIDFn = func() (string, error) { return "", errors.New("unreadable") }
	_, err := currentPIDIdentity()
	var launch *LaunchError
	if !errors.As(err, &launch) || launch.Code != "E_RUN_IDENTITY_UNAVAILABLE" {
		t.Fatalf("identity error = %v", err)
	}
	r, _ := newMemoryRunner(t, nil)
	reserved := false
	r.reserveIDFn = func() (string, error) { reserved = true; return "RUN-1", nil }
	_, err = r.Launch(context.Background(), Request{Argv: []string{"/bin/true"}, Detach: true})
	if !errors.As(err, &launch) || launch.Code != "E_RUN_IDENTITY_UNAVAILABLE" || reserved {
		t.Fatalf("launch identity error=%v reserved=%v", err, reserved)
	}
}

func TestM20MergedDetachedCaptureUsesOneOpenFileDescription(t *testing.T) {
	dir := t.TempDir()
	paths, files, err := openOutputs(dir, "RUN-1", true)
	if err != nil {
		t.Fatal(err)
	}
	defer closeOutputFiles(files)
	stdout, stderr, err := detachedOutputFiles(files, true)
	if err != nil {
		t.Fatal(err)
	}
	if stdout != stderr || stdout != files["log"] || len(paths) != 1 {
		t.Fatalf("merged capture does not share one OFD: stdout=%p stderr=%p files=%v", stdout, stderr, files)
	}
}

func TestM20ControlFileIs0600AndConsumedBeforeLaunch(t *testing.T) {
	dir := t.TempDir()
	path, err := writeDetachControl(dir, Request{Argv: []string{"/bin/true"}, Detach: true})
	if err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("control mode = %o", info.Mode().Perm())
	}
	req, err := consumeDetachControl(path)
	if err != nil {
		t.Fatal(err)
	}
	if !req.Detach || len(req.Argv) != 1 || req.Argv[0] != "/bin/true" {
		t.Fatalf("control request = %+v", req)
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("control file still exists: %v", err)
	}
	if entries, err := os.ReadDir(dir); err != nil || len(entries) != 0 {
		t.Fatalf("control residue entries=%v err=%v", entries, err)
	}
}

func TestM20LauncherDefersACKAndBoundsReadiness(t *testing.T) {
	defaults, _ := newMemoryRunner(t, nil)
	if defaults.detachReadyTimeout != 60*time.Second {
		t.Fatalf("default detach readiness timeout=%s", defaults.detachReadyTimeout)
	}
	t.Run("handle before ack", func(t *testing.T) {
		r, _ := newMemoryRunner(t, nil)
		ackPath := filepath.Join(t.TempDir(), "ack")
		t.Setenv("AIRA_M20_FAKE_SUPERVISOR", "success")
		t.Setenv("AIRA_M20_ACK_PATH", ackPath)
		launch, err := r.LaunchDetached(context.Background(), Request{Argv: []string{"/bin/true"}, Detach: true})
		if err != nil || launch.Record.ID != "RUN-helper" || launch.Record.Status != StatusStarting {
			t.Fatalf("launch=%+v err=%v", launch, err)
		}
		if _, err := os.Stat(ackPath); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("ACK arrived before handle completion: %v", err)
		}
		if err := launch.Complete(true); err != nil {
			t.Fatal(err)
		}
		deadline := time.Now().Add(time.Second)
		for time.Now().Before(deadline) {
			if data, err := os.ReadFile(ackPath); err == nil {
				if string(data) != "1" {
					t.Fatalf("ack=%q", data)
				}
				return
			}
			time.Sleep(5 * time.Millisecond)
		}
		t.Fatal("fake supervisor did not observe ACK")
	})

	t.Run("readiness timeout cancels", func(t *testing.T) {
		r, _ := newMemoryRunner(t, nil)
		r.detachReadyTimeout = 20 * time.Millisecond
		t.Setenv("AIRA_M20_FAKE_SUPERVISOR", "timeout")
		started := time.Now()
		launch, err := r.LaunchDetached(context.Background(), Request{Argv: []string{"/bin/true"}, Detach: true})
		var typed *LaunchError
		if launch != nil || !errors.As(err, &typed) || typed.Code != "E_RUN_DETACH_FAILED" || time.Since(started) > time.Second {
			t.Fatalf("launch=%+v err=%v elapsed=%s", launch, err, time.Since(started))
		}
	})
}

func TestM20BoundedRunLockTimesOutHonestly(t *testing.T) {
	path := filepath.Join(t.TempDir(), "RUN-1.lock")
	held, err := lockFile(path)
	if err != nil {
		t.Fatal(err)
	}
	defer unlockFile(held)
	flags, err := unix.FcntlInt(held.Fd(), unix.F_GETFD, 0)
	if err != nil || flags&unix.FD_CLOEXEC == 0 {
		t.Fatalf("run lock is inheritable: flags=%d err=%v", flags, err)
	}
	started := time.Now()
	_, err = lockFileBounded(path, 20*time.Millisecond)
	var launch *LaunchError
	if !errors.As(err, &launch) || launch.Code != "U_RUN_LAUNCH_STALLED" {
		t.Fatalf("bounded lock error = %v", err)
	}
	if time.Since(started) > time.Second {
		t.Fatalf("bounded lock did not return promptly")
	}
}

func TestM20MissingACKCancelsWithoutStartingChildAndKillIntentWins(t *testing.T) {
	for name, withIntent := range map[string]bool{"cancelled": false, "kill intent": true} {
		t.Run(name, func(t *testing.T) {
			r, scope := newMemoryRunner(t, nil)
			readyR, readyW, err := os.Pipe()
			if err != nil {
				t.Fatal(err)
			}
			ackR, ackW, err := os.Pipe()
			if err != nil {
				t.Fatal(err)
			}
			req := Request{Argv: []string{"/bin/true"}, Detach: true, detachReady: &detachSignal{file: readyW}, detachAck: ackR}
			cwd, bootID := t.TempDir(), mustBootID(t)
			result := make(chan error, 1)
			go func() {
				_, launchErr := r.launchDetachedValidated(context.Background(), req, nil, cwd, []string{}, "digest", "none", req.Argv, bootID)
				result <- launchErr
			}()
			var ready detachReadyMessage
			if err := json.NewDecoder(readyR).Decode(&ready); err != nil || ready.ID == "" {
				t.Fatalf("readiness=%+v err=%v", ready, err)
			}
			if withIntent {
				lock, lockErr := lockFile(filepath.Join(filepath.Dir(r.ledger.ledger), ready.ID+".lock"))
				if lockErr != nil {
					t.Fatal(lockErr)
				}
				current, currentErr := r.ledger.current(ready.ID)
				if currentErr != nil {
					t.Fatal(currentErr)
				}
				current.KillIntent.Present = true
				if _, appendErr := r.append(ledgerEvent{Kind: "kill-intent", Run: current}); appendErr != nil {
					t.Fatal(appendErr)
				}
				_ = unlockFile(lock)
			}
			_ = ackW.Close()
			if err := <-result; err == nil {
				t.Fatal("missing ACK unexpectedly succeeded")
			}
			current, err := r.ledger.current(ready.ID)
			if err != nil {
				t.Fatal(err)
			}
			want := StatusCancelled
			if withIntent {
				want = StatusKilled
			}
			if current.Status != want || current.PIDIdentity.PID != 0 || len(scope.members) != 0 {
				t.Fatalf("missing ACK record=%+v scope=%+v", current, scope)
			}
		})
	}
}

func TestM20LaunchFlockIsHeldThroughStartAttempt(t *testing.T) {
	r, _ := newMemoryRunner(t, nil)
	r.runLockTimeout = 15 * time.Millisecond
	var checked bool
	r.startFn = func(*exec.Cmd) error {
		_, err := r.boundedRunLock(filepath.Join(filepath.Dir(r.ledger.ledger), "RUN-1.lock"))
		var launch *LaunchError
		if !errors.As(err, &launch) || launch.Code != "U_RUN_LAUNCH_STALLED" {
			return fmt.Errorf("launch lock was not held at Start: %v", err)
		}
		checked = true
		return errors.New("injected start failure")
	}
	readyR, readyW, _ := os.Pipe()
	ackR, ackW, _ := os.Pipe()
	req := Request{Argv: []string{"/bin/true"}, Detach: true, detachReady: &detachSignal{file: readyW}, detachAck: ackR}
	cwd, bootID := t.TempDir(), mustBootID(t)
	done := make(chan error, 1)
	go func() {
		_, err := r.launchDetachedValidated(context.Background(), req, nil, cwd, []string{}, "digest", "none", req.Argv, bootID)
		done <- err
	}()
	var ready detachReadyMessage
	if err := json.NewDecoder(readyR).Decode(&ready); err != nil {
		t.Fatal(err)
	}
	if _, err := ackW.Write([]byte{1}); err != nil {
		t.Fatal(err)
	}
	_ = ackW.Close()
	if err := <-done; err == nil || !checked {
		t.Fatalf("launch err=%v checked=%v", err, checked)
	}
}

func TestM20ReconcileAndKillBoundedLockNeverFabricateTerminal(t *testing.T) {
	r, scope := newMemoryRunner(t, []int{42})
	r.runLockTimeout = 15 * time.Millisecond
	run := detachedRunForTest(scope, processAlive)
	appendRunEvent(t, r, "starting", run)
	lock, err := lockFile(filepath.Join(filepath.Dir(r.ledger.ledger), run.ID+".lock"))
	if err != nil {
		t.Fatal(err)
	}
	defer unlockFile(lock)
	runs, err := r.Reconcile(context.Background())
	if err != nil || len(runs) != 1 || !containsString(runs[0].ErrorCodes, "U_RUN_LAUNCH_STALLED") || runs[0].Status.Terminal() {
		t.Fatalf("reconcile runs=%+v err=%v", runs, err)
	}
	if killed, err := r.Kill(context.Background(), run.ID, false); err == nil || killed != nil {
		t.Fatalf("bounded kill record=%+v err=%v", killed, err)
	}
	current, err := r.ledger.current(run.ID)
	if err != nil || current.Status.Terminal() || current.KillIntent.Present {
		t.Fatalf("bounded operations mutated lifecycle: %+v err=%v", current, err)
	}
}

type auditScope struct {
	members    []int
	beforeKill func()
	killErr    error
	clearOnErr bool
	removed    bool
	reference  string
}

func (s *auditScope) Reference() string {
	if s.reference != "" {
		return s.reference
	}
	return "/audit-scope"
}
func (s *auditScope) FD() int                 { return -1 }
func (s *auditScope) Members() ([]int, error) { return append([]int(nil), s.members...), nil }
func (s *auditScope) Empty() (bool, error)    { return len(s.members) == 0, nil }
func (s *auditScope) Terminate([]int) error   { return nil }
func (s *auditScope) Kill() error {
	if s.beforeKill != nil {
		s.beforeKill()
	}
	if s.killErr != nil {
		if s.clearOnErr {
			s.members = nil
		}
		return s.killErr
	}
	s.members = nil
	return nil
}

func TestM20ForceAttemptDoesNotClaimDescendantKillWhenScopeEmptiesNaturally(t *testing.T) {
	scope := &auditScope{members: []int{44}, killErr: errors.New("kill failed"), clearOnErr: true}
	r, err := New(Config{CommonDir: t.TempDir(), Backend: &auditBackend{scope: scope}, Grace: 10 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	run := RunRecord{SchemaVersion: ledgerSchema, ID: "RUN-1", Status: StatusRunning, Detached: true, CgroupScope: scope.Reference(), ScopeIntegrity: ScopeContained, OutputRefs: map[string]OutputRef{}}
	appendRunEvent(t, r, "starting", run)
	zero := 0
	if err := r.appendDetachedLeaderExit(run.ID, &zero, ""); err != nil {
		t.Fatal(err)
	}
	if result, forceErr := r.forceDetachedQuiesce(context.Background(), run.ID, run, scope); result != nil || forceErr != nil {
		t.Fatalf("force-attempt result=%+v err=%v", result, forceErr)
	}
	final, err := r.finalizeDetachedTerminal(context.Background(), run.ID, scope)
	if err != nil {
		t.Fatal(err)
	}
	if final.Status != StatusKilled || final.QuiesceKillProven || final.ScopeIntegrity == ScopeDescendantKilled || !containsString(final.ErrorCodes, "U_RUN_QUIESCE_FORCED") || final.CleanSuccess() {
		t.Fatalf("force-attempt was over-claimed: %+v", final)
	}
}
func (s *auditScope) Remove() error { s.removed = true; return nil }

func TestM20ForcedQuiesceIsDurableBeforeKillAndClassificationIsHonest(t *testing.T) {
	for name, killErr := range map[string]error{"proven": nil, "attempt failed": errors.New("kill failed")} {
		t.Run(name, func(t *testing.T) {
			scope := &auditScope{members: []int{44}, killErr: killErr}
			r, err := New(Config{CommonDir: t.TempDir(), Backend: &memoryBackend{scope: &memoryScope{}}, Grace: 10 * time.Millisecond})
			if err != nil {
				t.Fatal(err)
			}
			run := RunRecord{SchemaVersion: ledgerSchema, ID: "RUN-1", Status: StatusRunning, Detached: true, CgroupScope: scope.Reference(), ScopeIntegrity: ScopeContained, OutputRefs: map[string]OutputRef{}}
			appendRunEvent(t, r, "starting", run)
			zero := 0
			if _, err := r.ledger.append(ledgerEvent{Kind: "leader-exited", Run: RunRecord{ID: run.ID}, LeaderExitObserved: true, ExitCode: &zero}); err != nil {
				t.Fatal(err)
			}
			scope.beforeKill = func() {
				events, readErr := r.ledger.read()
				if readErr != nil || !hasEvent(events, run.ID, "quiesce-forced") {
					t.Fatalf("kill preceded durable forced event: events=%+v err=%v", events, readErr)
				}
			}
			_, forceErr := r.forceDetachedQuiesce(context.Background(), run.ID, run, scope)
			current, currentErr := r.ledger.current(run.ID)
			if currentErr != nil {
				t.Fatal(currentErr)
			}
			if !current.QuiesceForced || current.Status.Terminal() {
				t.Fatalf("forced evidence/lifecycle=%+v", current)
			}
			if killErr != nil {
				if forceErr == nil || current.QuiesceKillProven || current.ScopeIntegrity == ScopeDescendantKilled {
					t.Fatalf("failed kill was over-claimed: record=%+v err=%v", current, forceErr)
				}
				return
			}
			if forceErr != nil || !current.QuiesceKillProven || current.ScopeIntegrity != ScopeDescendantKilled {
				t.Fatalf("proven forced kill record=%+v err=%v", current, forceErr)
			}
			final, err := r.finalizeDetachedTerminal(context.Background(), run.ID, scope)
			if err != nil {
				t.Fatal(err)
			}
			if final.Status != StatusKilled || final.ExitCode == nil || *final.ExitCode != 0 || final.CleanSuccess() || !containsString(final.ErrorCodes, "U_RUN_QUIESCE_FORCED") {
				t.Fatalf("forced terminal=%+v", final)
			}
		})
	}
}

func TestM20ForcedQuiescePublishesResultBeforeRacingReconcile(t *testing.T) {
	scope := &auditScope{members: []int{44}}
	r, err := New(Config{CommonDir: t.TempDir(), Backend: &memoryBackend{scope: &memoryScope{}}, Grace: 10 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	r.backend = &auditBackend{scope: scope}
	run := RunRecord{SchemaVersion: ledgerSchema, ID: "RUN-1", Status: StatusRunning, Detached: true, CgroupScope: scope.Reference(), ScopeIntegrity: ScopeContained, OutputRefs: map[string]OutputRef{}}
	appendRunEvent(t, r, "starting", run)
	zero := 0
	if err := r.appendDetachedLeaderExit(run.ID, &zero, ""); err != nil {
		t.Fatal(err)
	}
	reconciled := make(chan RunRecord, 1)
	scope.beforeKill = func() {
		go func() {
			runs, reconcileErr := r.Reconcile(context.Background())
			if reconcileErr == nil && len(runs) == 1 {
				reconciled <- runs[0]
			}
		}()
	}
	if result, forceErr := r.forceDetachedQuiesce(context.Background(), run.ID, run, scope); result != nil || forceErr != nil {
		t.Fatalf("forced result=%+v err=%v", result, forceErr)
	}
	select {
	case final := <-reconciled:
		if final.Status != StatusKilled || !final.QuiesceKillProven || final.ScopeIntegrity != ScopeDescendantKilled || !containsString(final.ErrorCodes, "U_RUN_QUIESCE_FORCED") {
			t.Fatalf("racing finalizer lost forced result: %+v", final)
		}
	case <-time.After(time.Second):
		t.Fatal("racing reconcile did not finish")
	}
}

type auditBackend struct{ scope Scope }

func (b *auditBackend) Probe(context.Context) error                   { return nil }
func (b *auditBackend) Create(context.Context, string) (Scope, error) { return b.scope, nil }
func (b *auditBackend) Open(context.Context, string) (Scope, error)   { return b.scope, nil }

func TestM20DetachedRunKillWaitsForPreScopeSupervisorTerminal(t *testing.T) {
	r, scope := newMemoryRunner(t, nil)
	r.pollInterval = time.Millisecond
	run := detachedRunForTest(scope, processAlive)
	run.KillIntent.Present = true
	appendRunEvent(t, r, "starting", run)
	if _, err := r.append(ledgerEvent{Kind: "kill-intent", Run: run}); err != nil {
		t.Fatal(err)
	}
	go func() {
		time.Sleep(10 * time.Millisecond)
		_, _ = r.terminalizeDetachedNoChild(context.Background(), run, true, "E_RUN_KILLED", errors.New("injected admission-window kill"))
	}()
	final, err := r.finishDetachedKill(context.Background(), run.ID, killAttempt{Current: run, IntentPublished: true})
	if err != nil || final == nil || final.Status != StatusKilled || !final.KillIntent.Present {
		t.Fatalf("pre-scope kill final=%+v err=%v", final, err)
	}
}

func TestM20DetachedFinalizerPreservesExitUsageDigestAndOOMEvidence(t *testing.T) {
	for name, oomKills := range map[string]string{"exit-n": "0", "oom": "1"} {
		t.Run(name, func(t *testing.T) {
			scopeDir := t.TempDir()
			for file, data := range map[string]string{
				"memory.peak": "321\n", "cpu.stat": "user_usec 7\nsystem_usec 8\n", "memory.events": "oom_kill " + oomKills + "\n",
			} {
				if err := os.WriteFile(filepath.Join(scopeDir, file), []byte(data), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			scope := &auditScope{reference: scopeDir}
			r, err := New(Config{CommonDir: t.TempDir(), Backend: &auditBackend{scope: scope}})
			if err != nil {
				t.Fatal(err)
			}
			output := filepath.Join(t.TempDir(), "RUN-1.out")
			if err := os.WriteFile(output, []byte("faithful"), 0o600); err != nil {
				t.Fatal(err)
			}
			run := RunRecord{SchemaVersion: ledgerSchema, ID: "RUN-1", Status: StatusRunning, Detached: true, CgroupScope: scope.Reference(), ScopeIntegrity: ScopeContained, OutputRefs: map[string]OutputRef{"out": {Path: output, State: OutputPartial}}}
			appendRunEvent(t, r, "starting", run)
			exit := 23
			if err := r.appendDetachedLeaderExit(run.ID, &exit, ""); err != nil {
				t.Fatal(err)
			}
			final, err := r.finalizeDetachedTerminal(context.Background(), run.ID, scope)
			if err != nil {
				t.Fatal(err)
			}
			wantStatus := StatusExited
			if oomKills == "1" {
				wantStatus = StatusOOMKilled
			}
			ref := final.OutputRefs["out"]
			if final.Status != wantStatus || final.ExitCode == nil || *final.ExitCode != 23 || final.PeakRSS == nil || *final.PeakRSS != 321 || final.CPUUser == nil || *final.CPUUser != 7 || final.CPUSys == nil || *final.CPUSys != 8 || ref.State != OutputComplete || ref.Digest == "" || !final.TerminalComplete {
				t.Fatalf("complete terminal evidence=%+v", final)
			}
		})
	}
}

func mustBootID(t *testing.T) string {
	t.Helper()
	bootID, err := currentBootID()
	if err != nil {
		t.Fatal(err)
	}
	return bootID
}

func startRealDetached(t *testing.T, r *Runner, req Request) (string, <-chan launchOutcome) {
	t.Helper()
	readyR, readyW, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	ackR, ackW, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	req.Detach, req.detachReady, req.detachAck = true, &detachSignal{file: readyW}, ackR
	result := make(chan launchOutcome, 1)
	go func() {
		record, launchErr := r.Launch(context.Background(), req)
		result <- launchOutcome{record: record, err: launchErr}
	}()
	var ready detachReadyMessage
	if err := json.NewDecoder(readyR).Decode(&ready); err != nil || ready.ID == "" {
		t.Fatalf("real detach readiness=%+v err=%v", ready, err)
	}
	_ = readyR.Close()
	if _, err := ackW.Write([]byte{1}); err != nil {
		t.Fatal(err)
	}
	_ = ackW.Close()
	return ready.ID, result
}

func waitForRunState(t *testing.T, r *Runner, id string, accept func(RunRecord) bool) RunRecord {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		record, err := r.ledger.current(id)
		if err == nil && accept(record) {
			return record
		}
		time.Sleep(10 * time.Millisecond)
	}
	record, _ := r.ledger.current(id)
	t.Fatalf("run %s did not reach expected state: %+v", id, record)
	return RunRecord{}
}

func TestM20RealDetachReturnsWhileChildLivesAndSupervisorIsOutsideScope(t *testing.T) {
	r := realRunner(t)
	id, result := startRealDetached(t, r, Request{Argv: []string{"/bin/sh", "-c", "sleep 30"}})
	running := waitForRunState(t, r, id, func(record RunRecord) bool { return record.Status == StatusRunning })
	scope, err := r.backend.Open(context.Background(), running.CgroupScope)
	if err != nil {
		t.Fatal(err)
	}
	members, err := scope.Members()
	if err != nil || len(members) == 0 || containsPID(members, running.SupervisorPID.PID) {
		t.Fatalf("shim/child scope members=%v supervisor=%+v err=%v", members, running.SupervisorPID, err)
	}
	killed, err := r.Kill(context.Background(), id, false)
	if err != nil || killed.Status != StatusKilled {
		t.Fatalf("run-kill record=%+v err=%v", killed, err)
	}
	select {
	case outcome := <-result:
		if outcome.err != nil || outcome.record == nil || outcome.record.Status != StatusKilled {
			t.Fatalf("supervisor outcome=%+v", outcome)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("supervisor did not finish after run-kill")
	}
}

func TestM20RealDetachedDup2MergeIsFaithfulAndCompleteAfterQuiesce(t *testing.T) {
	r := realRunner(t)
	id, result := startRealDetached(t, r, Request{Argv: []string{"/bin/sh", "-c", "for i in 1 2 3; do printf o$i; printf e$i >&2; done"}, Merge: true})
	outcome := <-result
	if outcome.err != nil || outcome.record == nil {
		t.Fatalf("detach outcome=%+v", outcome)
	}
	record := outcome.record
	ref := record.OutputRefs["log"]
	data, err := os.ReadFile(ref.Path)
	if err != nil {
		t.Fatal(err)
	}
	if id != record.ID || string(data) != "o1e1o2e2o3e3" || ref.State != OutputComplete || ref.Digest == "" || !record.CaptureComplete || !record.TerminalComplete {
		t.Fatalf("record=%+v data=%q", record, data)
	}
}

func TestM20RealForcedQuiescePreservesLeaderExitAndIsNotClean(t *testing.T) {
	r := realRunner(t)
	r.grace = 100 * time.Millisecond
	_, result := startRealDetached(t, r, Request{Argv: []string{"/bin/sh", "-c", "sleep 30 & exit 0"}, Merge: true})
	outcome := <-result
	if outcome.err != nil || outcome.record == nil {
		t.Fatalf("detach outcome=%+v", outcome)
	}
	record := outcome.record
	if record.Status != StatusKilled || record.ExitCode == nil || *record.ExitCode != 0 || !record.LeaderExitObserved || !record.QuiesceForced ||
		!record.QuiesceKillProven || record.ScopeIntegrity != ScopeDescendantKilled || !containsString(record.ErrorCodes, "U_RUN_QUIESCE_FORCED") || record.CleanSuccess() {
		t.Fatalf("forced quiesce record=%+v", record)
	}
}

func TestM20FinalizeNeverTerminalizesPopulatedScope(t *testing.T) {
	r, scope := newMemoryRunner(t, []int{77})
	run := detachedRunForTest(scope, processDead)
	appendRunEvent(t, r, "starting", run)
	zero := 0
	if _, err := r.ledger.append(ledgerEvent{Kind: "leader-exited", Run: RunRecord{ID: run.ID}, LeaderExitObserved: true, ExitCode: &zero}); err != nil {
		t.Fatal(err)
	}
	lock, err := lockFile(filepath.Join(filepath.Dir(r.ledger.ledger), run.ID+".lock"))
	if err != nil {
		t.Fatal(err)
	}
	_, err = r.finalizeDetachedTerminalLocked(context.Background(), run.ID, scope)
	_ = unlockFile(lock)
	var launch *LaunchError
	if !errors.As(err, &launch) || launch.Code != "U_RUN_CAPTURE_INCOMPLETE" {
		t.Fatalf("finalize error = %v", err)
	}
	current, getErr := r.Get(run.ID)
	if getErr != nil {
		t.Fatal(getErr)
	}
	if current.Status.Terminal() {
		t.Fatalf("populated scope was terminalized: %+v", current)
	}
}

func TestM20ReconcileDetachedSupervisorLeaseAndExitEvidence(t *testing.T) {
	oldBoot, oldStat := readBootIDFn, readProcStatFn
	t.Cleanup(func() { readBootIDFn, readProcStatFn = oldBoot, oldStat })
	readBootIDFn = func() (string, error) { return "boot-a", nil }
	readProcStatFn = func(int) ([]byte, error) { return procStatForTest('S', 77), nil }

	t.Run("alive queued supervisor is preserved", func(t *testing.T) {
		r, scope := newMemoryRunner(t, []int{10})
		run := detachedRunForTest(scope, processAlive)
		appendRunEvent(t, r, "starting", run)
		got := reconcileOne(t, r)
		if got.Status.Terminal() {
			t.Fatalf("alive supervisor was lost: %+v", got)
		}
	})

	t.Run("exit evidence plus empty finalizes exact exit and digest", func(t *testing.T) {
		r, scope := newMemoryRunner(t, nil)
		run := detachedRunForTest(scope, processAlive)
		path := filepath.Join(t.TempDir(), "RUN-1.out")
		if err := os.WriteFile(path, []byte("faithful"), 0o600); err != nil {
			t.Fatal(err)
		}
		run.OutputRefs = map[string]OutputRef{"out": {Path: path, State: OutputPartial}}
		appendRunEvent(t, r, "starting", run)
		zero := 0
		if _, err := r.ledger.append(ledgerEvent{Kind: "leader-exited", Run: RunRecord{ID: run.ID}, LeaderExitObserved: true, ExitCode: &zero}); err != nil {
			t.Fatal(err)
		}
		got := reconcileOne(t, r)
		ref := got.OutputRefs["out"]
		if got.Status != StatusExited || got.ExitCode == nil || *got.ExitCode != 0 || ref.State != OutputComplete || ref.Digest == "" || !got.TerminalComplete {
			t.Fatalf("evidence finalization incomplete: %+v", got)
		}
	})

	t.Run("dead supervisor empty without evidence becomes lost", func(t *testing.T) {
		r, scope := newMemoryRunner(t, nil)
		run := detachedRunForTest(scope, processDead)
		appendRunEvent(t, r, "starting", run)
		got := reconcileOne(t, r)
		if got.Status != StatusLost || !containsString(got.ErrorCodes, "U_RUN_EXIT_UNKNOWN") {
			t.Fatalf("dead supervisor outcome: %+v", got)
		}
	})

	t.Run("alive supervisor empty without evidence is stalled not terminal", func(t *testing.T) {
		r, scope := newMemoryRunner(t, nil)
		run := detachedRunForTest(scope, processAlive)
		appendRunEvent(t, r, "starting", run)
		got := reconcileOne(t, r)
		if got.Status.Terminal() || !containsString(got.ErrorCodes, "U_RUN_SUPERVISOR_STALLED") {
			t.Fatalf("stalled supervisor outcome: %+v", got)
		}
	})

	t.Run("unknown supervisor liveness preserves", func(t *testing.T) {
		r, scope := newMemoryRunner(t, nil)
		run := detachedRunForTest(scope, processUnknown)
		appendRunEvent(t, r, "starting", run)
		got := reconcileOne(t, r)
		if got.Status.Terminal() {
			t.Fatalf("unknown supervisor was terminalized: %+v", got)
		}
	})
}

func detachedRunForTest(scope Scope, state processLiveness) RunRecord {
	bootID := "boot-a"
	if state == processDead {
		bootID = "boot-b"
	}
	if state == processUnknown {
		bootID = ""
	}
	return RunRecord{
		SchemaVersion: ledgerSchema, ID: "RUN-1", Status: StatusStarting, Detached: true,
		CgroupScope: scope.Reference(), ScopeIntegrity: ScopeContained, OutputRefs: map[string]OutputRef{},
		SupervisorPID: PIDIdentity{PID: 123, StartTick: 77, BootID: bootID},
	}
}

func reconcileOne(t *testing.T, r *Runner) RunRecord {
	t.Helper()
	runs, err := r.Reconcile(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(runs) != 1 {
		t.Fatalf("reconcile returned %d runs: %+v", len(runs), runs)
	}
	return runs[0]
}

func procStatForTest(state byte, start uint64) []byte {
	// The parser includes state as field zero; starttime is parser field 19.
	fields := make([]string, 19)
	for i := range fields {
		fields[i] = "0"
	}
	fields[18] = fmtUint(start)
	return []byte("123 (test) " + string(state) + " " + strings.Join(fields, " "))
}

func fmtUint(value uint64) string {
	return strconv.FormatUint(value, 10)
}
