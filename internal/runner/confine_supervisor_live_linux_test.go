//go:build linux

package runner

import (
	"context"
	"os"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

// AIRA-183. The PRODUCER of ConfineRecord.SupervisorLive — the reading that lets
// `confine --list` tell a genuinely orphaned scope from an alive-but-idle one.
//
// verifies: AIRA-183

// The tri-state decision, exhaustively, separated from the syscall so it is
// pinned against errno classes rather than against whichever PIDs a test host
// happens to have free.
func TestAIRA183SupervisorLivenessFromSignalError(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name string
		err  error
		want *bool
	}{
		{"signallable", nil, boolPtr(true)},
		// A PID the caller may not signal is still a PID that is TAKEN. Reading
		// EPERM as death is how a scope owned by another user would be declared an
		// orphan while its job is running.
		{"exists but not ours", unix.EPERM, boolPtr(true)},
		{"no such process", unix.ESRCH, boolPtr(false)},
		{"anything else", unix.EINVAL, nil},
		{"wrapped no such process", &os.SyscallError{Syscall: "kill", Err: unix.ESRCH}, boolPtr(false)},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			got := supervisorLiveFromSignalError(test.err)
			switch {
			case test.want == nil && got != nil:
				t.Fatalf("errno %v must leave liveness unestablished, got %v", test.err, *got)
			case test.want != nil && got == nil:
				t.Fatalf("errno %v must establish liveness %v, got unevaluated", test.err, *test.want)
			case test.want != nil && *got != *test.want:
				t.Fatalf("errno %v: liveness %v, want %v", test.err, *got, *test.want)
			}
		})
	}
}

// The probe itself, on the one PID every test host is guaranteed to have, plus
// the PIDs that are not PIDs at all: kill(0, 0) signals the caller's own PROCESS
// GROUP and would answer "alive" about something that is not the supervisor, and
// a negative PID addresses a group too. Both must be refused before the syscall.
func TestAIRA183ProbeSupervisorLive(t *testing.T) {
	t.Parallel()
	if live := probeSupervisorLive(os.Getpid()); live == nil || !*live {
		t.Fatalf("this very process must probe alive, got %v", live)
	}
	for _, pid := range []int{0, -1, -os.Getpid()} {
		if live := probeSupervisorLive(pid); live != nil {
			t.Fatalf("pid %d is not a supervisor pid and must probe unevaluated, got %v", pid, *live)
		}
	}
}

// The scan fills the field from the seam, per record, and names it unevaluated
// rather than guessing when the reading fails. The false-pass direction is the
// one that matters: a scan that left SupervisorLive nil on every row would make
// every renderer assertion above it vacuous, so each of the three outcomes is
// asserted on a DIFFERENT scope in the same pass.
func TestAIRA183ConfineScanRecordsSupervisorLiveness(t *testing.T) {
	t.Parallel()
	slice := t.TempDir()
	now := time.Now()
	alive := confineTestScopeID("alive", 5101, now.Add(-time.Minute).UnixNano())
	orphan := confineTestScopeID("orphan", 5102, now.Add(-time.Minute).UnixNano())
	opaque := confineTestScopeID("opaque", 5103, now.Add(-time.Minute).UnixNano())
	writeConfineTestScope(t, slice, alive, "")
	writeConfineTestScope(t, slice, orphan, "")
	writeConfineTestScope(t, slice, opaque, "")

	deps := defaultConfineScanDeps()
	deps.supervisorLive = func(pid int) *bool {
		switch pid {
		case 5101:
			return boolPtr(true)
		case 5102:
			return boolPtr(false)
		default:
			return nil
		}
	}
	result, err := listConfinesWithDeps(context.Background(), slice, nil, deps)
	if err != nil || result.Verdict != "pass" || len(result.Scopes) != 3 {
		t.Fatalf("result=%+v err=%v", result, err)
	}
	seen := map[string]ConfineRecord{}
	for _, record := range result.Scopes {
		seen[record.ScopeID] = record
	}
	if record := seen[alive]; record.SupervisorLive == nil || !*record.SupervisorLive {
		t.Fatalf("a live supervisor was not recorded: %+v", record)
	}
	if record := seen[orphan]; record.SupervisorLive == nil || *record.SupervisorLive {
		t.Fatalf("a dead supervisor was not recorded: %+v", record)
	}
	if record := seen[opaque]; record.SupervisorLive != nil {
		t.Fatalf("an unreadable supervisor must stay unevaluated, got %v", *record.SupervisorLive)
	} else if !confineContainsString(record.UnevaluatedFields, "supervisor_live") {
		t.Fatalf("an unevaluated supervisor reading must be NAMED as unevaluated: %+v", record)
	}
}

// The veto, and the reason it exists. A supervisor PID that is namespace-local
// probes ESRCH from outside its namespace while its job runs on — the exact
// misjudgement the orphan reaper guards against with its own live-lease check,
// which is why the reaper does not reap such a scope. The listing must not
// NAME it an orphan either: an operator reading "orphaned" stops looking.
func TestAIRA183ALiveAdmitLeaseVetoesADeadSupervisorReading(t *testing.T) {
	t.Parallel()
	slice := t.TempDir()
	now := time.Now()
	leased := confineTestScopeID("leased", 5201, now.Add(-time.Minute).UnixNano())
	unleased := confineTestScopeID("unleased", 5202, now.Add(-time.Minute).UnixNano())
	writeConfineTestScope(t, slice, leased, "")
	writeConfineTestScope(t, slice, unleased, "")

	deps := defaultConfineScanDeps()
	deps.supervisorLive = func(int) *bool { return boolPtr(false) }
	result, err := listConfinesWithDeps(context.Background(), slice, []ConfineRegistryEntry{{ScopeID: leased}}, deps)
	if err != nil || len(result.Scopes) != 2 {
		t.Fatalf("result=%+v err=%v", result, err)
	}
	seen := map[string]ConfineRecord{}
	for _, record := range result.Scopes {
		seen[record.ScopeID] = record
	}
	if record := seen[leased]; record.SupervisorLive != nil {
		t.Fatalf("a dead probe contradicted by a live admit lease must stay unevaluated, got %v", *record.SupervisorLive)
	} else if !confineContainsString(record.UnevaluatedFields, "supervisor_live") {
		t.Fatalf("the vetoed reading must be named unevaluated: %+v", record)
	}
	// The other half of the veto, without which the test would also pass against
	// an implementation that simply never recorded a dead supervisor at all.
	if record := seen[unleased]; record.SupervisorLive == nil || *record.SupervisorLive {
		t.Fatalf("an unleased dead supervisor is a genuine orphan and must be recorded as one: %+v", record)
	}
	// And the veto must not go the other way: a live lease is grounds for
	// refusing to call a scope orphaned, never grounds for asserting life.
	deps.supervisorLive = func(int) *bool { return nil }
	vetoed, err := listConfinesWithDeps(context.Background(), slice, []ConfineRegistryEntry{{ScopeID: leased}}, deps)
	if err != nil {
		t.Fatal(err)
	}
	for _, record := range vetoed.Scopes {
		if record.SupervisorLive != nil {
			t.Fatalf("an admit lease fabricated a liveness reading for %s: %v", record.ScopeID, *record.SupervisorLive)
		}
	}
}

// A caller that builds confineScanDeps field-by-field (several existing tests
// do) must keep the production probe rather than silently losing the field to a
// nil func — the same nil-fallback discipline readCmdline already has, and the
// direction a nil-pointer panic would come from.
func TestAIRA183ScanFallsBackToTheRealProbeWhenTheSeamIsUnset(t *testing.T) {
	t.Parallel()
	slice := t.TempDir()
	scopeID := confineTestScopeID("self", os.Getpid(), time.Now().Add(-time.Minute).UnixNano())
	writeConfineTestScope(t, slice, scopeID, "")
	result, err := listConfinesWithDeps(context.Background(), slice, nil, confineScanDeps{
		now: time.Now, readField: readConfineScopeField, waitEmpty: waitEmpty,
	})
	if err != nil || len(result.Scopes) != 1 {
		t.Fatalf("result=%+v err=%v", result, err)
	}
	if record := result.Scopes[0]; record.SupervisorLive == nil || !*record.SupervisorLive {
		t.Fatalf("the unset seam did not fall back to the real probe: %+v", record)
	}
}

// The shim/pending path names the field unevaluated rather than borrowing the
// admit lease it was built from as evidence of life.
func TestAIRA183PendingRowsLeaveSupervisorLivenessUnevaluated(t *testing.T) {
	t.Parallel()
	byID := map[string]ConfineRecord{}
	scopeID := confineTestScopeID("pending", 5301, time.Now().UnixNano())
	mergeConfineRegistry(byID, []ConfineRegistryEntry{{ScopeID: scopeID}})
	record, ok := byID[scopeID]
	if !ok {
		t.Fatalf("no pending row was produced: %+v", byID)
	}
	if record.SupervisorLive != nil {
		t.Fatalf("a pending row asserted supervisor liveness from its lease alone: %v", *record.SupervisorLive)
	}
	if !confineContainsString(record.UnevaluatedFields, "supervisor_live") {
		t.Fatalf("a pending row must name the supervisor reading unevaluated: %+v", record)
	}
}

func boolPtr(value bool) *bool { return &value }
