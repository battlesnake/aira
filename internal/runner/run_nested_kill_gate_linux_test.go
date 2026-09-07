//go:build linux

package runner

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"aira/internal/testdeadline"
)

// AIRA-140 — `aira run`'s own kill gate, the twin of the defect AIRA-138 fixed
// for `aira confine`.
//
// Runner.killScope gated its refusal to signal on the LEAF read alone
// (`Members()` == cgroup.procs). Scope.Empty() reads cgroup.events `populated`,
// which is SUBTREE-aware, and the two legitimately disagree in one direction: a
// job whose processes live in child cgroups it created inside its own scope
// reads leaf-empty while fully busy. That is the shape of every
// `--delegate-ram`/aitest job, of `podman --cgroups=split`, and of any nested
// cgroup workload — so `--timeout` / `--cpu-timeout` (AIRA-136) wrote NO signal
// at all against exactly the heavy job a bound is most wanted for.
//
// These tests reuse AIRA-138's `nestedWorkloadScope` fake
// (confine_deadline_danger_linux_test.go), which is the only fake in the package
// that can express leaf-empty/subtree-populated at all: `livenessScope`'s
// Empty() is derived from the same membersLocked() as its Members(), so its two
// reads are coupled by construction and could never have surfaced this.
//
// They bracket the gate from BOTH sides, exactly as AIRA-138's mutations 5 and 6
// did, because the correction must not become a widening:
//
//   - T1 a leaf-empty, subtree-POPULATED scope is now killed (re-narrowing the
//     gate to `len(pids)==0` fails it);
//   - T2 a scope BOTH reads call empty still returns before any write, with the
//     `Empty && !Started` shape AIRA-126's decideTimeoutIntentNotExecuted
//     consumes (widening the gate to always-kill fails it);
//   - T3 an Empty() error is unevaluated, with no write attempted;
//   - T4 the leaf-populated arm keeps its full TERM-grace-then-KILL escalation,
//     untouched.
//
// verifies: AIRA-140

// nestedKillGateRunner is the minimum Runner killScope actually reads: the two
// graces. No ledger, no backend, no cgroup — killScope takes the Scope as a
// parameter, so the gate can be exercised in isolation from the launch path.
func nestedKillGateRunner() *Runner {
	return &Runner{termGrace: 20 * time.Millisecond, grace: time.Second}
}

// T1. THE DEFECT. A fully busy nested workload — leaf cgroup.procs empty,
// cgroup.events populated — must be KILLED, not shrugged at. Restoring the
// leaf-only gate makes this fail with no cgroup.kill written at all.
//
// verifies: AIRA-140
func TestAIRA140KillScopeKillsALeafEmptySubtreePopulatedRunScope(t *testing.T) {
	r := nestedKillGateRunner()
	busy := &nestedWorkloadScope{subtreePopulated: true}

	result, err := r.killScope(context.Background(), busy, "RUN-1", "run-timeout")
	if err != nil {
		t.Fatalf("killScope errored against a busy nested workload: %v", err)
	}
	if !result.Started || !result.Completed || !result.Empty {
		t.Fatalf("killScope did not kill a busy nested workload: %+v — a leaf-only gate signals nothing "+
			"against the aitest / --delegate-ram / podman --cgroups=split shape, so --timeout is inert there", result)
	}
	terminated, killed := busy.signalled()
	if !killed {
		t.Fatal("killScope reported Started without recording a cgroup.kill write")
	}
	// The ticket's explicit care point: Terminate takes LEAF pids, which are
	// empty in this shape. "Terminate nothing, wait termGrace, then kill" would
	// be a pure delay in front of the only signal that reaches the job, so the
	// nested arm must go STRAIGHT to the recursive cgroup.kill.
	if terminated {
		t.Fatal("killScope called Terminate on a leaf-empty scope: it signalled nothing and then waited the TERM grace before the kill that actually reaches the job")
	}

	// AIRA-126's arbitration must read this as a DELIVERED kill, not as the
	// "intent published, nothing signalled" shape — otherwise the fix would stop
	// at the gate and the run would still report the kill as not-executed.
	attempt := killAttempt{IntentPublished: true, IntentCreated: true, Kill: result}
	if decideTimeoutIntentNotExecuted(nil, attempt, processDead) {
		t.Fatalf("a killed nested workload was arbitrated as intent-not-executed: %+v", attempt)
	}
}

// T2. THE ANTI-OVER-CORRECTION. A scope BOTH reads call empty must still return
// BEFORE any write. That `Empty && !Started` shape is the sole input to
// AIRA-126's decideTimeoutIntentNotExecuted, so a gate widened to "always kill"
// would silently delete the not-executed arm and bring the AIRA-126 fabrication
// back.
//
// verifies: AIRA-140
func TestAIRA140KillScopeStillRefusesToSignalAScopeBothReadsCallEmpty(t *testing.T) {
	r := nestedKillGateRunner()
	quiet := &nestedWorkloadScope{subtreePopulated: false}

	result, err := r.killScope(context.Background(), quiet, "RUN-1", "run-timeout")
	if err != nil {
		t.Fatalf("killScope errored against an empty scope: %v", err)
	}
	if !result.Empty || result.Started || result.Completed {
		t.Fatalf("killScope did not refuse an empty scope: %+v", result)
	}
	if terminated, killed := quiet.signalled(); terminated || killed {
		t.Fatalf("killScope signalled a scope both reads called empty: terminated=%v killed=%v", terminated, killed)
	}

	// The refusal is now verified by TWO agreeing reads rather than one, which is
	// strictly stronger evidence for the same proof — the proof itself is
	// unchanged and must still hold.
	attempt := killAttempt{IntentPublished: true, IntentCreated: true, Kill: result}
	if !decideTimeoutIntentNotExecuted(nil, attempt, processDead) {
		t.Fatalf("AIRA-126's not-executed arm no longer reachable through killScope: %+v", attempt)
	}
}

// nestedScopeEmptyRead wraps AIRA-138's fake with a failing population read: the
// one state in which AIRA cannot establish whether there was anything to kill.
type nestedScopeEmptyRead struct {
	*nestedWorkloadScope
	err error
}

func (s *nestedScopeEmptyRead) Empty() (bool, error) { return false, s.err }

// T3. An Empty() error is UNEVALUATED: the zero result plus the error, and no
// write attempted. It must never be folded into a half-populated result, and it
// must never fall through to a kill just because `empty` came back false
// alongside the error.
//
// verifies: AIRA-140
func TestAIRA140KillScopeTreatsAnUnreadablePopulationAsUnevaluated(t *testing.T) {
	r := nestedKillGateRunner()
	readErr := errors.New("cgroup.events unreadable")
	scope := &nestedScopeEmptyRead{nestedWorkloadScope: &nestedWorkloadScope{subtreePopulated: true}, err: readErr}

	result, err := r.killScope(context.Background(), scope, "RUN-1", "run-timeout")
	if !errors.Is(err, readErr) {
		t.Fatalf("a failed population read did not surface as an error: %v", err)
	}
	if result.Empty || result.Started || result.Completed {
		t.Fatalf("a failed population read produced a populated result: %+v", result)
	}
	if terminated, killed := scope.signalled(); terminated || killed {
		t.Fatalf("killScope signalled on an unevaluated population read: terminated=%v killed=%v", terminated, killed)
	}
	// killErr != nil is its own refusal conjunct in AIRA-126; it must stay so.
	if decideTimeoutIntentNotExecuted(err, killAttempt{IntentPublished: true, IntentCreated: true, Kill: result}, processDead) {
		t.Fatal("an errored kill was arbitrated as intent-not-executed")
	}
}

// leafPopulatedNestedScope reports a LEAF member, so killScope takes its
// original arm. Terminate does not end the workload here (nothing obliges a job
// to honour SIGTERM), so the TERM grace must elapse and the recursive kill must
// still follow.
type leafPopulatedNestedScope struct {
	*nestedWorkloadScope
}

func (s *leafPopulatedNestedScope) Members() ([]int, error) { return []int{4242}, nil }

// T4. The leaf-populated arm is UNTOUCHED by the fix: SIGTERM first, then the
// grace, then cgroup.kill. The correction may only add a signal where the gate
// previously refused one — it may not remove the escalation where the gate
// already signalled.
//
// verifies: AIRA-140
func TestAIRA140KillScopeKeepsTheTermGraceEscalationForALeafPopulatedScope(t *testing.T) {
	r := nestedKillGateRunner()
	scope := &leafPopulatedNestedScope{nestedWorkloadScope: &nestedWorkloadScope{subtreePopulated: true}}

	started := time.Now()
	result, err := r.killScope(context.Background(), scope, "RUN-1", "run-kill")
	elapsed := time.Since(started)
	if err != nil {
		t.Fatalf("killScope errored against a leaf-populated scope: %v", err)
	}
	if !result.Started || !result.Completed || !result.Empty {
		t.Fatalf("killScope did not complete against a leaf-populated scope: %+v", result)
	}
	terminated, killed := scope.signalled()
	if !terminated || !killed {
		t.Fatalf("the leaf-populated arm lost its TERM-then-KILL escalation: terminated=%v killed=%v", terminated, killed)
	}
	if elapsed < r.termGrace {
		t.Fatalf("the TERM grace was skipped on the leaf-populated arm: %s < %s", elapsed, r.termGrace)
	}
}

// T5. THE REAL-CGROUP LANE. A real `aira run` whose payload discovers its own
// cgroup, nests a child under it, migrates itself in and only then sleeps. Its
// scope reads leaf-empty while fully busy, so with the leaf-only gate the
// deadline fires, writes nothing, and the record carries an unevaluated
// `U_RUN_RECONCILE_REQUIRED` kill while the sleep runs on to its full 30s.
//
// The record is checked AND the kernel is checked: a status alone would not
// distinguish a delivered kill from a fabricated one, so the scope's own
// subtree-aware cgroup.events is read back independently after the launch
// returns.
//
// verifies: AIRA-140
func TestAIRA140RealCgroupTimeoutKillsARunLivingInAChildCgroup(t *testing.T) {
	r := realRunner(t)
	// Exit 9 rather than sleeping if the shape cannot be built, so an environment
	// that cannot nest is reported as unavailable instead of quietly degrading
	// into an ordinary leaf-populated timeout that would pass either way.
	const payload = `
set -e
root=/sys/fs/cgroup$(awk -F: '$1=="0"{print $3}' /proc/self/cgroup)
mkdir -p "$root/.aira-nested" || exit 9
echo $$ > "$root/.aira-nested/cgroup.procs" || exit 9
grep -q '^0::.*\.aira-nested$' /proc/self/cgroup || exit 9
exec sleep 30
`
	record, err := r.Launch(context.Background(), Request{
		Argv:    []string{"/bin/sh", "-c", payload},
		Timeout: testdeadline.Wait(300 * time.Millisecond),
	})
	if err != nil {
		t.Fatalf("nested-cgroup launch error=%v record=%+v", err, record)
	}
	if record.ExitCode != nil && *record.ExitCode == 9 {
		skipOrFailRealCgroup(t, "this environment cannot nest a child cgroup inside a run scope")
	}
	if record.Status != StatusKilled || !containsString(record.ErrorCodes, "E_RUN_TIMEOUT") {
		t.Fatalf("nested-cgroup timeout record=%+v", record)
	}
	if !record.ScopeKill.Started || !record.ScopeKill.Completed {
		t.Fatalf("the deadline wrote no signal against a leaf-empty, subtree-populated run scope: %+v", record)
	}
	if !record.KillIntent.Present || !record.KillIntent.Completed {
		t.Fatalf("the kill intent was published but never executed: %+v", record)
	}
	if containsString(record.ErrorCodes, "U_RUN_RECONCILE_REQUIRED") {
		t.Fatalf("the timeout landed in the unevaluated arm against a job it was supposed to end: %+v", record)
	}
	// Independent of every field above: ask the kernel whether anything of this
	// run survives. `populated` is subtree-aware, so it covers the child cgroup
	// the payload moved itself into.
	if alive, why := runScopeSubtreeAlive(t, record.CgroupScope); alive {
		t.Fatalf("the nested job outlived its deadline: %s (record=%+v)", why, record)
	}
	if got := terminalRecords(t, r); got != 1 {
		t.Fatalf("terminal records=%d", got)
	}
}

// runScopeSubtreeAlive reads the scope's own cgroup.events `populated` back from
// the kernel. A removed scope directory is proof of emptiness (rmdir refuses a
// populated cgroup); an unreadable one is reported as NOT alive so a read
// failure can never manufacture a test failure that the code did not cause.
func runScopeSubtreeAlive(t *testing.T, scopePath string) (bool, string) {
	t.Helper()
	if scopePath == "" {
		return false, ""
	}
	deadline := time.Now().Add(testdeadline.Wait(5 * time.Second))
	last := ""
	for {
		data, err := os.ReadFile(filepath.Join(scopePath, "cgroup.events"))
		if os.IsNotExist(err) {
			return false, ""
		}
		if err != nil {
			return false, ""
		}
		last = strings.TrimSpace(string(data))
		populated := false
		for _, line := range strings.Split(last, "\n") {
			if strings.TrimSpace(line) == "populated 1" {
				populated = true
			}
		}
		if !populated {
			return false, ""
		}
		if !time.Now().Before(deadline) {
			return true, "the scope's cgroup.events still reads " + strings.ReplaceAll(last, "\n", "; ")
		}
		time.Sleep(20 * time.Millisecond)
	}
}
