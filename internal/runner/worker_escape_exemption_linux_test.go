//go:build linux

package runner

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"
)

// S2a §16c/§16.1/§16.2: an aitest worker forks inside the parent (supervisor)
// confine scope, then place_self's into its OWN first-class sibling worker
// scope directly under the slice. That migration is expected, not an escape.
// The exemption lives in the single witnessedEscape chokepoint and is a purely
// LOCAL positive check: the migrated process's cgroup basename parses to an
// aitest worker scope id whose embedded pid is THIS monitor's own pid
// (os.Getpid() — the pid Task 1 minted into every worker name for this
// supervisor). No lease table, no daemon round-trip.

// TestWitnessedEscapeExemptsOwnAitestWorkerMigration is the core exemption
// check: a live process observed in its own sibling worker scope is NOT a
// witnessed escape, and escapedObservation (which sets record.DescendantEscape)
// therefore returns nil for it.
func TestWitnessedEscapeExemptsOwnAitestWorkerMigration(t *testing.T) {
	slice := "/sys/fs/cgroup/aira.slice"
	// The parent supervisor scope; the worker is a SIBLING under the slice, so
	// it is not an ancestor of the worker cgroup (a genuine sibling by path).
	parentScope := filepath.Join(slice, ".aira-CONFINE-parent-"+strconv.Itoa(os.Getpid())+"-parent")
	// Mint a REAL worker id so the grammar (and any drift in it) is exercised;
	// the pid slot is this monitor's own pid, per Task 1.
	workerCgroup := filepath.Join(slice, ".aira-"+MintWorkerScopeID(1, os.Getpid(), ""))

	worker := &processCgroupObservation{
		// The worker's OWN process pid is unrelated to the pid in its scope name
		// (which is the supervisor's) — the exemption keys on the NAME's pid.
		Identity:        PIDIdentity{PID: 4242, StartTick: 100, BootID: "boot"},
		Live:            processAlive,
		Readable:        true,
		StartTickBefore: 100, StartTickAfter: 100,
		Cgroup: workerCgroup,
	}
	if witnessedEscape(parentScope, worker) {
		t.Fatalf("worker migration into own sibling scope %q was witnessed as an escape", workerCgroup)
	}
	if obs := escapedObservation(parentScope, worker, nil); obs != nil {
		t.Fatalf("escapedObservation returned %+v for an own-worker migration; want nil (no descendant_escape)", obs)
	}
}

// TestClassifyLaunchScopeIntegrityWorkerMigrationIsUnverifiedNotEscaped locks
// the GATE-3 verdict: with the worker migration exempted, a delegate run reads
// scope-integrity=UNVERIFIED with NO descendant_escape — never
// descendant-escaped/migrated, and never contained (contained is leader-only by
// the #20 attestation design: a pytest supervisor always has relay descendants →
// HadDescendants → unverified). Covers BOTH the monitor-sample escape and the
// teardown escape (the --kill/--timeout path) via the classifier's two
// witnessedEscape calls.
func TestClassifyLaunchScopeIntegrityWorkerMigrationIsUnverifiedNotEscaped(t *testing.T) {
	slice := "/sys/fs/cgroup/aira.slice"
	parentScope := filepath.Join(slice, ".aira-CONFINE-parent-"+strconv.Itoa(os.Getpid())+"-parent")
	workerObs := &processCgroupObservation{
		Identity:        PIDIdentity{PID: 5151, StartTick: 200, BootID: "boot"},
		Live:            processAlive,
		Readable:        true,
		StartTickBefore: 200, StartTickAfter: 200,
		Cgroup: filepath.Join(slice, ".aira-"+MintWorkerScopeID(3, os.Getpid(), "")),
	}
	base := launchScopeFacts{
		ScopeVerified: true, PlacementGuaranteed: true, IdentityValid: true, WaitObserved: true,
		ScopePath: parentScope,
		// A real supervisor always has relay descendants.
		Monitor:  scopeMonitorResult{HadDescendants: true},
		Teardown: scopeTeardownResult{Observed: true, Empty: true},
	}

	monitorCase := base
	monitorCase.Monitor.Escape = workerObs
	if got, _, code := classifyLaunchScopeIntegrity(monitorCase); got != ScopeUnverified || code != "" {
		t.Fatalf("monitor-path worker migration = %q code=%q, want %q with no escape code", got, code, ScopeUnverified)
	}

	// Teardown-path (the killed-run case): relays die, the lease releases, and a
	// worker is still alive in its sibling scope for the sub-second before the
	// daemon kills it. The pid in its name is still os.Getpid(), so it must not
	// false-flag an escape.
	teardownCase := base
	teardownCase.Teardown.Escape = workerObs
	if got, _, code := classifyLaunchScopeIntegrity(teardownCase); got != ScopeUnverified || code != "" {
		t.Fatalf("teardown-path worker migration = %q code=%q, want %q with no escape code", got, code, ScopeUnverified)
	}
}

// TestAttestScopeTeardownExemptsOwnAitestWorkerMigration drives the REAL
// attestScopeTeardown (the state a --kill/--timeout run actually produces at
// runner_linux.go:1948) rather than synthetic facts: a member left alive in its
// own worker scope after Kill() must attest neither escaped (Escape==nil) nor
// descendant-killed. It reads as a residual read-gap, which is honest.
func TestAttestScopeTeardownExemptsOwnAitestWorkerMigration(t *testing.T) {
	mount, err := unifiedMount()
	if err != nil {
		t.Skip(err)
	}
	oldBoot, oldStat, oldCgroup := readBootIDFn, readProcStatFn, readProcCgroupFn
	t.Cleanup(func() { readBootIDFn, readProcStatFn, readProcCgroupFn = oldBoot, oldStat, oldCgroup })

	const memberPID = 4242
	const tick = 4242
	readBootIDFn = func() (string, error) { return "boot", nil }
	readProcStatFn = func(int) ([]byte, error) { return procStatForTest('S', tick), nil }
	// The still-alive member's /proc/<pid>/cgroup places it in its own sibling
	// worker scope under the slice, whose name embeds this monitor's pid.
	workerRel := "/aira.slice/.aira-" + MintWorkerScopeID(1, os.Getpid(), "")
	readProcCgroupFn = func(int) ([]byte, error) { return []byte("0::" + workerRel + "\n"), nil }

	parentScope := filepath.Join(mount, "aira.slice", ".aira-CONFINE-parent-"+strconv.Itoa(os.Getpid())+"-parent")
	scope := &migratingTeardownScope{path: parentScope, pid: memberPID}
	result := attestScopeTeardown(context.Background(), scope, 0, 100*time.Millisecond)
	if result.Escape != nil {
		t.Fatalf("own-worker member attested as escape: %+v", result.Escape)
	}
	if result.DescendantKilled {
		t.Fatalf("own-worker member attested as descendant-killed: %+v", result)
	}
	// An exempted-but-still-alive member is a residual read-gap: attestable
	// neither reclaimed nor escaped. Locking Gap here guards the "never
	// contained" property on the teardown path — a late-joiner worker the
	// monitor never sampled (HadDescendants=false) that is alive in its sibling
	// scope at teardown must NOT collapse to ScopeContained. If a future refactor
	// made the exempted case skip the else-branch, Observed && !Gap && Empty &&
	// !HadDescendants && ScopeVerified would falsely read contained.
	if !result.Gap {
		t.Fatalf("own-worker member left alive at teardown did not record a read-gap: %+v", result)
	}
}

// TestWitnessedEscapeStillWitnessesGenuineEscapes is the anti-over-exemption
// guard: the exemption is narrow. A process in an UNRELATED cgroup, a worker
// scope carrying a DIFFERENT supervisor's pid, and a non-worker confine job
// (name not aitest-w<digits>) are all still witnessed as escapes.
func TestWitnessedEscapeStillWitnessesGenuineEscapes(t *testing.T) {
	slice := "/sys/fs/cgroup/aira.slice"
	parentScope := filepath.Join(slice, ".aira-CONFINE-parent-"+strconv.Itoa(os.Getpid())+"-parent")
	obs := func(cgroup string) *processCgroupObservation {
		return &processCgroupObservation{
			Identity:        PIDIdentity{PID: 4242, StartTick: 100, BootID: "boot"},
			Live:            processAlive,
			Readable:        true,
			StartTickBefore: 100, StartTickAfter: 100,
			Cgroup: cgroup,
		}
	}

	// (a) An unrelated cgroup with no aitest-w name is a genuine escape.
	if !witnessedEscape(parentScope, obs("/sys/fs/cgroup/user.slice/leaked")) {
		t.Fatalf("a genuine escape to an unrelated cgroup was wrongly exempted")
	}

	// (b) A worker scope whose embedded pid is a DIFFERENT supervisor's (not this
	// monitor) is a genuine escape — the pid facet, not the name alone, gates it.
	foreign := filepath.Join(slice, ".aira-"+MintWorkerScopeID(1, os.Getpid()+1, ""))
	if !witnessedEscape(parentScope, obs(foreign)) {
		t.Fatalf("a foreign supervisor's worker scope (embedded pid %d != monitor %d) was wrongly exempted", os.Getpid()+1, os.Getpid())
	}

	// (c) A non-worker confine job named "aitest-wrapper" (suffix not all digits)
	// carrying THIS pid is a genuine escape — the name gate is worker-specific.
	notWorker := filepath.Join(slice, ".aira-"+confineScopeIDWithPID("aitest-wrapper", "", os.Getpid()))
	if !witnessedEscape(parentScope, obs(notWorker)) {
		t.Fatalf("a non-worker confine job named aitest-wrapper was wrongly exempted")
	}
}
