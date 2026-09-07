//go:build linux

package runner

import (
	"errors"
	"os"
	"strings"
	"syscall"
	"testing"
	"time"
)

// AIRA-138 — the PURE rules. Each is total over its inputs and each has a table
// with one row per conjunct, in both directions, so a dropped conjunct or a
// reordered precedence goes red rather than merely losing coverage.
//
// verifies: AIRA-138

// verifies: AIRA-138 — decideConfineDeadlineNotExecuted, one row per conjunct.
func TestAIRA138ConfineDeadlineNotExecutedRule(t *testing.T) {
	t.Parallel()
	refused := errors.New("cgroup.kill: permission denied")
	for _, test := range []struct {
		name    string
		killErr error
		attempt confineKillResult
		leader  processLiveness
		want    bool
		why     string
	}{
		{
			name:    "no signal, verified empty by both reads, leader proved dead",
			attempt: confineKillResult{Empty: true}, leader: processDead, want: true,
			why: "the whole rule: the kill returned BEFORE the cgroup.kill write and the leader was already gone",
		},
		{
			name: "an errored kill is unevaluated, never dismissed", killErr: refused,
			attempt: confineKillResult{Empty: true}, leader: processDead, want: false,
			why: "conjunct killErr == nil",
		},
		{
			name:    "a scope that was not verified empty",
			attempt: confineKillResult{}, leader: processDead, want: false,
			why: "conjunct Empty: a leaf-empty but subtree-POPULATED scope is a busy job and is killed instead",
		},
		{
			name:    "a kill that DID write cgroup.kill",
			attempt: confineKillResult{Empty: true, Started: true, Completed: true}, leader: processDead, want: false,
			why: "conjunct !Started: a signal was emitted, so this is arm A and not the arbitrated arm",
		},
		{
			name:    "a live leader",
			attempt: confineKillResult{Empty: true}, leader: processAlive, want: false,
			why: "conjunct leader == processDead: an empty scope alone is not proof the job finished",
		},
		{
			name:    "an unknown leader",
			attempt: confineKillResult{Empty: true}, leader: processUnknown, want: false,
			why: "conjunct leader == processDead, written as == and not as != processAlive",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			if got := decideConfineDeadlineNotExecuted(test.killErr, test.attempt, test.leader); got != test.want {
				t.Fatalf("decide = %v, want %v (%s)", got, test.want, test.why)
			}
		})
	}
}

// verifies: AIRA-138 — decideConfineDeadlineState over EVERY row of the plan's
// §5.6.1, including the four cross-bound cases the plan gate named and both
// routes into `unenforced`.
func TestAIRA138ConfineDeadlineStateRule(t *testing.T) {
	t.Parallel()
	failed := errors.New("scope did not become empty")
	for _, test := range []struct {
		name          string
		requested     bool
		fired         bool
		attempt       confineKillResult
		killErr       error
		notExecuted   bool
		cpuUnenforced bool
		want          ConfineDeadlineState
		why           string
	}{
		{
			name: "a bound nobody asked for renders nothing", want: "",
			why: "the empty string is the only value that omits the field",
		},
		{
			name: "requested, never fired, nothing to report", requested: true, want: ConfineDeadlineNotReached,
			why: "a wall timer that did not fire is a one-sided fact needing no measurement",
		},
		{
			name: "fired and the kill completed", requested: true, fired: true,
			attempt: confineKillResult{Empty: true, Started: true, Completed: true},
			want:    ConfineDeadlineFiredKillCompleted,
			why:     "the write happened and emptiness was confirmed afterwards",
		},
		{
			name: "fired, wrote, emptiness unconfirmed", requested: true, fired: true,
			attempt: confineKillResult{Started: true}, killErr: failed,
			want: ConfineDeadlineFiredKillUnconfirmed,
			why:  "Started without Completed is a separate, weaker fact and says so",
		},
		{
			name: "fired and provably delivered nothing", requested: true, fired: true,
			attempt: confineKillResult{Empty: true}, notExecuted: true,
			want: ConfineDeadlineFiredNotExecuted,
			why:  "the arbitrated arm",
		},
		{
			name: "fired and AIRA cannot establish what the kill did", requested: true, fired: true,
			killErr: failed, want: ConfineDeadlineFiredUnevaluated,
			why: "an errored read or write, or a leader whose liveness is unestablished",
		},
		{
			name:      "PRECEDENCE: a fired-not-executed bound is never shadowed by unenforced",
			requested: true, fired: true, attempt: confineKillResult{Empty: true},
			notExecuted: true, cpuUnenforced: true,
			want: ConfineDeadlineFiredNotExecuted,
			why:  "fired-not-executed ENTAILS unenforced and says more; rendering unenforced would delete the fact that AIRA acted",
		},
		{
			name:      "gate case 1: the OTHER bound killed, this CPU budget was reached and never enforced",
			requested: true, fired: false, cpuUnenforced: true,
			want: ConfineDeadlineUnenforced,
			why:  "this bound never fired, so its own measurement decides",
		},
		{
			name:      "gate case 3: CPU requested, did not fire, teardown cpu.stat unreadable",
			requested: true, fired: false, cpuUnenforced: true,
			want: ConfineDeadlineUnenforced,
			why:  "never `not-reached`, whose definition demands the two-sided proof",
		},
		{
			name:      "gate case 4: the WALL bound fired into arm C while the CPU bound held",
			requested: true, fired: false, cpuUnenforced: false,
			want: ConfineDeadlineNotReached,
			why:  "the two fields are decided independently",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			got := decideConfineDeadlineState(test.requested, test.fired, test.attempt, test.killErr,
				test.notExecuted, test.cpuUnenforced)
			if got != test.want {
				t.Fatalf("state = %q, want %q (%s)", got, test.want, test.why)
			}
		})
	}
}

// verifies: AIRA-138 — the classifier's new step 7, in BOTH directions, and that
// `oom` and `supervisor-signal:` still outrank it. Inserting the branch above
// step 2 or step 3 fails this test.
func TestAIRA138ClassifyConfineTerminationDeadlineOrder(t *testing.T) {
	t.Parallel()
	// The rendered vocabulary is pinned LITERALLY, not just against the constants
	// the rows below use, because `terminated-by=` is an operator- and
	// agent-facing contract: a rename that kept every constant reference
	// consistent would slip past a test that only compares symbol to symbol.
	// `terminated-by` names the QUANTITY exceeded (a cause), while the
	// `timeout=`/`cpu-timeout=` fields on the same line name the FLAG (a knob).
	for literal, got := range map[string]string{
		"deadline:wall": ConfineTerminatedDeadlinePrefix + ConfineDeadlineBoundWall,
		"deadline:cpu":  ConfineTerminatedDeadlinePrefix + ConfineDeadlineBoundCPU,
		"timeout":       ConfineDeadlineLabelWall,
		"cpu-timeout":   ConfineDeadlineLabelCPU,
	} {
		if got != literal {
			t.Fatalf("the rendered vocabulary moved: got %q, want %q", got, literal)
		}
	}
	sigkilled := confineTermination{Decoded: true, Signaled: true, Signal: syscall.SIGKILL}
	readableNoOOM := cgroupUsage{OOMKill: int64ptr(0), OOMKillLocal: int64ptr(0), OOMGroupKillLocal: int64ptr(0)}
	localOOM := cgroupUsage{OOMKill: int64ptr(1), OOMKillLocal: int64ptr(1), OOMGroupKillLocal: int64ptr(0)}
	for _, test := range []struct {
		name       string
		term       confineTermination
		usage      cgroupUsage
		supervisor os.Signal
		kill       deadlineKind
		want       string
		why        string
	}{
		{
			name: "a SIGKILL after a wall-deadline kill is attributed to the deadline",
			term: sigkilled, usage: readableNoOOM, kill: deadlineKindWall,
			want: ConfineTerminatedDeadlinePrefix + ConfineDeadlineBoundWall,
			why:  "step 7: leaving this in step 8 would make formatConfineTerminationAdvisory's 'it sent no signal itself' a lie",
		},
		{
			name: "and to the CPU bound when that is what fired",
			term: sigkilled, usage: readableNoOOM, kill: deadlineKindCPU,
			want: ConfineTerminatedDeadlinePrefix + ConfineDeadlineBoundCPU,
			why:  "the two bounds are distinguishable on the verdict, not merged into one label",
		},
		{
			name: "with no deadline kill the same evidence is unattributed",
			term: sigkilled, usage: readableNoOOM, kill: deadlineKindUnset,
			want: ConfineTerminatedUnattributedSIGKILL,
			why:  "the other direction: step 8 is unchanged for every run that had no deadline",
		},
		{
			name: "a LOCAL OOM still outranks the deadline",
			term: sigkilled, usage: localOOM, kill: deadlineKindCPU,
			want: ConfineTerminatedOOM,
			why:  "step 2 before 7: cgroup.kill never increments oom_kill, so a positive local counter is never our doing",
		},
		{
			name: "an operator's signal still outranks the deadline",
			term: sigkilled, usage: readableNoOOM, supervisor: syscall.SIGINT, kill: deadlineKindCPU,
			want: ConfineTerminatedSupervisorSignalPrefix + "SIGINT",
			why:  "step 3 before 7: a Ctrl-C racing a deadline makes both true and AIRA-70 exists so the operator's signal is never invisible",
		},
		{
			name: "a child that exited normally is not claimed by a deadline kill",
			term: confineTermination{Decoded: true}, usage: readableNoOOM, kill: deadlineKindCPU,
			want: ConfineTerminatedNormal,
			why:  "step 4 before 7: the §5.5 graceful middle case — the child left between the read and the write",
		},
		{
			name: "an unreadable counter stays unevaluated even with a deadline kill",
			term: sigkilled, usage: cgroupUsage{}, kill: deadlineKindCPU,
			want: ConfineTerminatedUnevaluated,
			why:  "step 6 before 7: an OOM and a deadline kill are indistinguishable without the counter, and claiming either is a fabricated zero",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			if got := classifyConfineTermination(test.term, test.usage, test.supervisor, test.kill); got != test.want {
				t.Fatalf("classify = %q, want %q (%s)", got, test.want, test.why)
			}
		})
	}
}

// verifies: AIRA-138 — deadlineFire.Kind is ADDITIVE. Both send sites still set
// the same Actor and Code `aira run`'s Launch reads, and Kind agrees with the
// Code rather than replacing it, so nothing about run's behaviour moves.
func TestAIRA138DeadlineFireKindDoesNotChangeRun(t *testing.T) {
	t.Parallel()
	wall := startDeadlineSource(deadlineConfig{Wall: time.Millisecond})
	if wall == nil {
		t.Fatal("startDeadlineSource returned nil for a requested wall bound")
	}
	defer wall.halt()
	fired := receiveFire(t, wall)
	// Launch reads Actor and Code and NOTHING else; both must be untouched.
	if fired.Actor != "run-timeout" || fired.Code != "E_RUN_TIMEOUT" || fired.Budget != time.Millisecond {
		t.Fatalf("the wall send site changed: %+v", fired)
	}
	if fired.Kind != deadlineKindWall {
		t.Fatalf("the wall fire did not carry its typed kind: %+v", fired)
	}
	// Observed stays zero for the wall bound, which is exactly why it is NOT a
	// discriminator and Kind had to exist.
	if fired.Observed != 0 {
		t.Fatalf("the wall bound reported an observed consumption: %+v", fired)
	}

	reader := (&scriptedCPUReader{}).push(0, true).push(time.Hour, true)
	cpu := startDeadlineSource(deadlineConfig{
		CPU: 50 * time.Millisecond, CPUBase: 0, CPUBaseOK: true,
		ScopePath: "/aira138-not-a-real-scope", Interval: time.Millisecond, ReadCPU: reader.read,
	})
	if cpu == nil {
		t.Fatal("startDeadlineSource returned nil for a requested CPU budget")
	}
	defer cpu.halt()
	firedCPU := receiveFire(t, cpu)
	if firedCPU.Actor != "run-cpu-timeout" || firedCPU.Code != "E_RUN_CPU_TIMEOUT" {
		t.Fatalf("the CPU send site changed: %+v", firedCPU)
	}
	if firedCPU.Kind != deadlineKindCPU {
		t.Fatalf("the CPU fire did not carry its typed kind: %+v", firedCPU)
	}
	// The zero value must never be read as either bound: that is the whole reason
	// this is a typed discriminator rather than an inference from a zero field.
	if deadlineKindUnset == deadlineKindWall || deadlineKindUnset == deadlineKindCPU {
		t.Fatal("deadlineKindUnset collides with a real bound")
	}
}

// verifies: AIRA-138 — the fire-time diagnostic is arm-correct, names the flag
// and the budget, and never claims an action that did not happen.
func TestAIRA138DeadlineWritesOneDiagnosticLinePerArm(t *testing.T) {
	t.Parallel()
	fire := deadlineFire{Actor: "run-cpu-timeout", Code: "E_RUN_CPU_TIMEOUT", Kind: deadlineKindCPU, Budget: 10 * time.Minute}
	wallFire := deadlineFire{Actor: "run-timeout", Code: "E_RUN_TIMEOUT", Kind: deadlineKindWall, Budget: 30 * time.Minute}
	failed := errors.New("scope did not become empty")
	for _, test := range []struct {
		name     string
		fire     deadlineFire
		attempt  confineKillResult
		killErr  error
		notExec  bool
		contains []string
		absent   []string
	}{
		{
			name: "arm A, confirmed", fire: fire,
			attempt:  confineKillResult{Empty: true, Started: true, Completed: true},
			contains: []string{"cpu-timeout", "10m0s", "fired;", "killed scope", "SCOPE-1", "aira.slice"},
			absent:   []string{"sent no signal", "unevaluated"},
		},
		{
			name: "arm A, unconfirmed", fire: fire,
			attempt: confineKillResult{Started: true}, killErr: failed,
			contains: []string{"wrote cgroup.kill", "emptiness unconfirmed", failed.Error()},
			absent:   []string{"killed scope", "sent no signal"},
		},
		{
			name: "arm B", fire: fire, attempt: confineKillResult{Empty: true}, notExec: true,
			contains: []string{"sent no signal", "already empty", "leader already dead", "reporting the job's own exit"},
			absent:   []string{"killed scope", "wrote cgroup.kill"},
		},
		{
			name: "arm C, with an error", fire: wallFire, killErr: failed,
			contains: []string{"timeout", "30m0s", "kill unevaluated", failed.Error()},
			absent:   []string{"killed scope", "sent no signal"},
		},
		{
			name: "arm C, with no error to name", fire: wallFire, attempt: confineKillResult{Empty: true},
			contains: []string{"kill unevaluated", "could not be proved dead"},
			absent:   []string{"killed scope"},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			line := formatConfineDeadlineAdvisory(test.fire, "SCOPE-1", "aira.slice", test.attempt, test.killErr, test.notExec)
			for _, want := range test.contains {
				if !strings.Contains(line, want) {
					t.Fatalf("line %q omits %q", line, want)
				}
			}
			for _, forbidden := range test.absent {
				if strings.Contains(line, forbidden) {
					t.Fatalf("line %q claims %q, which did not happen on this arm", line, forbidden)
				}
			}
			// A diagnostic that trails off after a colon reads as a truncated
			// failure rather than as the state it describes.
			if line == "" || line[len(line)-1] == ':' || line[len(line)-1] == ' ' {
				t.Fatalf("line %q is empty or unterminated", line)
			}
		})
	}
}
