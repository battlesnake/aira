//go:build linux

package runner

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestClassifyLaunchScopeIntegrityDistinguishesSubtreeFromSiblingEscape(t *testing.T) {
	identity := PIDIdentity{PID: 42, StartTick: 100, BootID: "boot"}
	base := launchScopeFacts{
		ScopeVerified:       true,
		PlacementGuaranteed: true,
		IdentityValid:       true,
		WaitObserved:        true,
		ScopePath:           "/sys/fs/cgroup/work/.aira-RUN-1",
		Monitor: scopeMonitorResult{
			HadDescendants: true,
		},
		Teardown: scopeTeardownResult{Observed: true, Empty: true},
	}

	nested := base
	nested.Monitor.Escape = &processCgroupObservation{
		Identity: identity, Live: processAlive, Readable: true,
		StartTickBefore: 100, StartTickAfter: 100,
		Cgroup: "/sys/fs/cgroup/work/.aira-RUN-1/nested",
	}
	if got, _, _ := classifyLaunchScopeIntegrity(nested); got == ScopeDescendantEscaped || got != ScopeUnverified {
		t.Fatalf("nested member classified as %q, want unverified and never escaped", got)
	}

	sibling := base
	sibling.Monitor.Escape = &processCgroupObservation{
		Identity: identity, Live: processAlive, Readable: true,
		StartTickBefore: 100, StartTickAfter: 100,
		Cgroup: "/sys/fs/cgroup/work/sibling",
	}
	if got, _, code := classifyLaunchScopeIntegrity(sibling); got != ScopeDescendantEscaped || code != "E_RUN_SCOPE_MIGRATION" {
		t.Fatalf("sibling member classified as %q code=%q, want escaped migration evidence", got, code)
	}
}

func TestClassifyLaunchScopeIntegrityTeardownHonesty(t *testing.T) {
	base := launchScopeFacts{
		ScopeVerified: true, PlacementGuaranteed: true, IdentityValid: true, WaitObserved: true,
		ScopePath: "/sys/fs/cgroup/work/.aira-RUN-1",
		Teardown:  scopeTeardownResult{Observed: true, Empty: true},
	}
	if got, _, _ := classifyLaunchScopeIntegrity(base); got != ScopeContained {
		t.Fatalf("leader-only clean teardown=%q, want contained", got)
	}

	multi := base
	multi.Monitor.HadDescendants = true
	if got, _, _ := classifyLaunchScopeIntegrity(multi); got != ScopeUnverified {
		t.Fatalf("multi-process clean teardown=%q, want unverified", got)
	}

	killed := multi
	killed.Teardown = scopeTeardownResult{Observed: true, DescendantKilled: true}
	if got, _, code := classifyLaunchScopeIntegrity(killed); got != ScopeDescendantKilled || code != "E_RUN_DESCENDANT_KILLED" {
		t.Fatalf("reclaimed descendant=%q code=%q, want descendant-killed", got, code)
	}

	unreadable := base
	unreadable.Teardown = scopeTeardownResult{Gap: true}
	if got, _, _ := classifyLaunchScopeIntegrity(unreadable); got != ScopeUnverified {
		t.Fatalf("unreadable teardown=%q, want residual unverified", got)
	}
}

func TestClassifyLaunchScopeIntegrityRequiresRaceSafeEscapeProof(t *testing.T) {
	identity := PIDIdentity{PID: 42, StartTick: 100, BootID: "boot"}
	base := launchScopeFacts{
		ScopeVerified: true, PlacementGuaranteed: true, IdentityValid: true, WaitObserved: true,
		ScopePath: "/sys/fs/cgroup/work/.aira-RUN-1",
		Monitor:   scopeMonitorResult{HadDescendants: true},
		Teardown:  scopeTeardownResult{Observed: true, Empty: true},
	}
	base.Monitor.Escape = &processCgroupObservation{
		Identity: identity, Live: processAlive, Readable: true,
		StartTickBefore: 101, StartTickAfter: 101,
		Cgroup: "/sys/fs/cgroup/work/sibling",
	}
	if got, _, _ := classifyLaunchScopeIntegrity(base); got == ScopeDescendantEscaped || got != ScopeUnverified {
		t.Fatalf("PID-reuse observation classified as %q, want unverified and never escaped", got)
	}

	base.Monitor.Escape = &processCgroupObservation{
		Identity: identity, Live: processAlive, Readable: false,
		StartTickBefore: 100, StartTickAfter: 100,
	}
	if got, _, _ := classifyLaunchScopeIntegrity(base); got == ScopeDescendantEscaped || got != ScopeUnverified {
		t.Fatalf("unreadable cgroup observation classified as %q, want unverified and never escaped", got)
	}
}

func TestObserveProcessCgroupUnreadableAndPIDReuseNeverProveEscape(t *testing.T) {
	oldBoot, oldStat, oldCgroup := readBootIDFn, readProcStatFn, readProcCgroupFn
	t.Cleanup(func() { readBootIDFn, readProcStatFn, readProcCgroupFn = oldBoot, oldStat, oldCgroup })
	readBootIDFn = func() (string, error) { return "boot", nil }
	identity := PIDIdentity{PID: 42, StartTick: 77, BootID: "boot"}
	scopePath := "/sys/fs/cgroup/work/.aira-RUN-1"

	readProcStatFn = func(int) ([]byte, error) { return procStatForTest('S', 77), nil }
	readProcCgroupFn = func(int) ([]byte, error) { return nil, errors.New("hidden") }
	unreadable := observeProcessCgroup(identity, scopePath)
	if unreadable.Readable || witnessedEscape(scopePath, &unreadable) {
		t.Fatalf("unreadable proc cgroup proved escape: %+v", unreadable)
	}

	reads := 0
	readProcStatFn = func(int) ([]byte, error) {
		reads++
		if reads >= 3 {
			return procStatForTest('S', 78), nil
		}
		return procStatForTest('S', 77), nil
	}
	readProcCgroupFn = func(int) ([]byte, error) { return []byte("0::/work/sibling\n"), nil }
	reused := observeProcessCgroup(identity, scopePath)
	if reused.StartTickAfter == identity.StartTick || witnessedEscape(scopePath, &reused) {
		t.Fatalf("reused PID proved escape: %+v", reused)
	}
}

func TestClassifyLaunchScopeIntegrityKeepsLeaderMigrationPrecedence(t *testing.T) {
	facts := launchScopeFacts{
		ScopeVerified: true, PlacementGuaranteed: true, IdentityValid: true, WaitObserved: true,
		ScopePath: "/sys/fs/cgroup/work/.aira-RUN-1",
		Monitor:   scopeMonitorResult{LeaderMigrated: true, Gap: true, HadDescendants: true},
		Teardown:  scopeTeardownResult{Observed: true, Empty: true},
	}
	if got, _, code := classifyLaunchScopeIntegrity(facts); got != ScopeMigrated || code != "E_RUN_SCOPE_MIGRATION" {
		t.Fatalf("leader migration classified as %q code=%q", got, code)
	}
}

func TestClassifyLaunchScopeIntegrityWithoutTeardownPreservesPTYLeaderVerdict(t *testing.T) {
	facts := launchScopeFacts{
		ScopeVerified: true, PlacementGuaranteed: true, IdentityValid: true, WaitObserved: true,
		ScopePath: "/sys/fs/cgroup/work/.aira-RUN-1",
		Monitor:   scopeMonitorResult{HadDescendants: true},
	}
	if got, _, _ := classifyLaunchScopeIntegrity(facts); got != ScopeContained {
		t.Fatalf("already-quiesced PTY leader verdict=%q, want unchanged contained", got)
	}
	if merged := mergeEvidence(RunRecord{ID: "RUN-1", ScopeIntegrity: ScopeDescendantKilled}, RunRecord{ID: "RUN-1", ScopeIntegrity: ScopeContained}); merged.ScopeIntegrity != ScopeDescendantKilled {
		t.Fatalf("PTY descendant-killed evidence was reclassified: %+v", merged)
	}
}

func TestClassifyLaunchScopeIntegrityMembershipReadFailureStillCannotClaimContained(t *testing.T) {
	facts := launchScopeFacts{
		PlacementGuaranteed: true, IdentityValid: true, WaitObserved: true,
		MemberErr: errors.New("cgroup.procs read failed"),
	}
	if got, unobserved, code := classifyLaunchScopeIntegrity(facts); got != ScopeHandoffUnverified || unobserved || code != "E_RUN_SCOPE_INVALID" {
		t.Fatalf("membership failure=%q unobserved=%v code=%q", got, unobserved, code)
	}
}

type migratingTeardownScope struct {
	path   string
	pid    int
	killed bool
}

func (s *migratingTeardownScope) Reference() string { return s.path }
func (*migratingTeardownScope) FD() int             { return -1 }
func (s *migratingTeardownScope) Members() ([]int, error) {
	if s.killed {
		return nil, nil
	}
	return []int{s.pid}, nil
}
func (s *migratingTeardownScope) Empty() (bool, error) { return s.killed, nil }
func (*migratingTeardownScope) Terminate([]int) error  { return nil }
func (s *migratingTeardownScope) Kill() error          { s.killed = true; return nil }
func (*migratingTeardownScope) Remove() error          { return nil }

func TestTeardownMemberAliveOutsideAfterKillIsEscapeNotKilled(t *testing.T) {
	mount, err := unifiedMount()
	if err != nil {
		t.Skip(err)
	}
	current, err := currentCgroupPath(mount)
	if err != nil {
		t.Skip(err)
	}
	scope := &migratingTeardownScope{path: filepath.Join(current, "not-the-current-scope"), pid: os.Getpid()}
	result := attestScopeTeardown(context.Background(), scope, 0, 100*time.Millisecond)
	if result.DescendantKilled || !witnessedEscape(scope.Reference(), result.Escape) {
		t.Fatalf("alive-outside teardown=%+v, want escape and never descendant-killed", result)
	}
}

// TestTeardownAliveMemberUnreadableCgroupIsGapNotContained guards the honesty
// invariant that a snapshotted member still alive at teardown, whose
// /proc/<pid>/cgroup cannot be read, is a residual read-gap — attestable neither
// as reclaimed (it is alive) nor as escaped (its cgroup is unreadable) — and
// must never yield a clean containment claim. Before the processAlive branch set
// result.Gap for a non-escape, this narrow interleaving (a late joiner the
// monitor never sampled, so HadDescendants=false, that leaves the subtree before
// Kill so the scope reads empty) produced ScopeContained + CleanSuccess: the
// cardinal false-pass this milestone exists to prevent.
func TestTeardownAliveMemberUnreadableCgroupIsGapNotContained(t *testing.T) {
	oldBoot, oldStat, oldCgroup := readBootIDFn, readProcStatFn, readProcCgroupFn
	t.Cleanup(func() { readBootIDFn, readProcStatFn, readProcCgroupFn = oldBoot, oldStat, oldCgroup })
	readBootIDFn = func() (string, error) { return "boot", nil }
	// Genuinely alive (stable start tick) but the cgroup read fails transiently.
	readProcStatFn = func(int) ([]byte, error) { return procStatForTest('S', 55), nil }
	readProcCgroupFn = func(int) ([]byte, error) { return nil, errors.New("hidepid") }

	scope := &migratingTeardownScope{path: "/sys/fs/cgroup/work/.aira-RUN-1", pid: 4242}
	result := attestScopeTeardown(context.Background(), scope, 0, 50*time.Millisecond)
	if !result.Gap {
		t.Fatalf("alive member with an unreadable cgroup must be a residual gap: %+v", result)
	}
	if result.Escape != nil {
		t.Fatalf("an unreadable cgroup must not be treated as a witnessed escape: %+v", result)
	}
	got, _, _ := classifyLaunchScopeIntegrity(launchScopeFacts{
		ScopeVerified: true, PlacementGuaranteed: true, IdentityValid: true, WaitObserved: true,
		ScopePath: scope.Reference(),
		Monitor:   scopeMonitorResult{HadDescendants: false},
		Teardown:  result,
	})
	if got == ScopeContained {
		t.Fatalf("an unattested live former member must not be classified contained (got %q)", got)
	}
}

func TestScopeIntegrityMergeUsesTotalPrecedence(t *testing.T) {
	states := []ScopeIntegrity{
		ScopeContained,
		ScopeHandoffUnverified,
		ScopeUnverified,
		ScopeDescendantKilled,
		ScopeMigrated,
		ScopeDescendantEscaped,
	}
	for high := range states {
		for low := 0; low <= high; low++ {
			base := RunRecord{ID: "RUN-1", ScopeIntegrity: states[high]}
			candidate := RunRecord{ID: "RUN-1", ScopeIntegrity: states[low]}
			if got := mergeEvidence(base, candidate).ScopeIntegrity; got != states[high] {
				t.Fatalf("merge %q then %q=%q, want %q", states[high], states[low], got, states[high])
			}
		}
	}
}

func TestRealCgroupCleanMultiProcessRunIsUnverified(t *testing.T) {
	r := realRunner(t)
	record, err := r.Launch(context.Background(), Request{Argv: []string{"/bin/sh", "-c", "sleep 0.05 & wait"}})
	if err != nil {
		t.Fatal(err)
	}
	if record.ScopeIntegrity != ScopeUnverified || record.CleanSuccess() {
		t.Fatalf("clean multi-process run=%+v, want unverified and non-green", record)
	}
}

func TestRealCgroupOutlivingInScopeDescendantIsKilledAndAttested(t *testing.T) {
	r := realRunner(t)
	proof := t.TempDir()
	script := `set -eu
sh -c 'printf "%s\n" "$$" > "$1/pid"; exec 1>&- 2>&-; exec sleep 30' sh "$1" &
while [ ! -s "$1/pid" ]; do sleep 0.001; done
sleep 0.05
exit 0`
	record, err := r.Launch(context.Background(), Request{Argv: []string{"/bin/sh", "-c", script, "sh", proof}})
	if err != nil {
		t.Fatal(err)
	}
	if record.ScopeIntegrity != ScopeDescendantKilled || !containsString(record.ErrorCodes, "E_RUN_DESCENDANT_KILLED") {
		t.Fatalf("outliving in-scope descendant=%+v, want descendant-killed", record)
	}
	if !record.ScopeKill.Completed || record.CleanSuccess() {
		t.Fatalf("reclamation evidence incomplete or green: %+v", record)
	}
}

func TestRealCgroupNestedDescendantIsNeverEscaped(t *testing.T) {
	r := realRunner(t)
	proof := t.TempDir()
	script := `set -eu
scope=/sys/fs/cgroup$(awk -F: '$1=="0" {print $3}' /proc/self/cgroup)
nested="$scope/nested"
mkdir "$nested"
printf '%s\n' "$nested" > "$1/nested"
sh -c 'sleep 0.03; echo $$ > "$1/cgroup.procs"; printf nested > "$2/moved"; exec 1>&- 2>&-; exec sleep 30' sh "$nested" "$1" &
while [ ! -s "$1/moved" ]; do sleep 0.001; done
sleep 0.03
exit 0`
	record, err := r.Launch(context.Background(), Request{Argv: []string{"/bin/sh", "-c", script, "sh", proof}})
	if err != nil {
		t.Fatal(err)
	}
	nested := strings.TrimSpace(string(waitForFixtureFile(t, filepath.Join(proof, "nested"))))
	t.Cleanup(func() {
		_ = os.Remove(nested)
		_ = os.Remove(record.CgroupScope)
	})
	if record.ScopeIntegrity == ScopeDescendantEscaped || record.DescendantEscape != nil {
		t.Fatalf("nested contained cgroup was falsely called escaped: %+v", record)
	}
	if record.ScopeIntegrity != ScopeUnverified && record.ScopeIntegrity != ScopeDescendantKilled {
		t.Fatalf("nested descendant integrity=%q, want residual/reclaimed and never escaped", record.ScopeIntegrity)
	}
}

func TestRealCgroupLeaderOnlyRunRemainsContained(t *testing.T) {
	r := realRunner(t)
	record, err := r.Launch(context.Background(), Request{Argv: []string{"/bin/sleep", "0.03"}})
	if err != nil {
		t.Fatal(err)
	}
	if record.ScopeIntegrity != ScopeContained || !record.CleanSuccess() {
		t.Fatalf("leader-only classification regressed: %+v", record)
	}
}

func TestRealCgroupForkAndMigrateBeforeFirstSampleIsNeverContained(t *testing.T) {
	r := realRunner(t)
	backend, ok := r.backend.(*linuxScopeBackend)
	if !ok {
		t.Fatalf("real runner backend=%T", r.backend)
	}
	target, err := os.MkdirTemp(backend.parent, ".aira-adversarial-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = os.WriteFile(filepath.Join(target, "cgroup.kill"), []byte("1"), 0o644)
		deadline := time.Now().Add(time.Second)
		for time.Now().Before(deadline) {
			if os.Remove(target) == nil {
				return
			}
			time.Sleep(time.Millisecond)
		}
	})
	proof := t.TempDir()
	script := `set -eu
sh -c 'echo $$ > "$2/pid"; echo $$ > "$1/cgroup.procs"; printf moved > "$2/moved"; exec 1>&- 2>&-; exec sleep 30' sh "$1" "$2" &
while [ ! -s "$2/moved" ]; do :; done
exit 0`
	record, err := r.Launch(context.Background(), Request{Argv: []string{"/bin/sh", "-c", script, "sh", target, proof}})
	if err != nil {
		t.Fatal(err)
	}
	if record.ScopeIntegrity == ScopeContained || record.CleanSuccess() {
		t.Fatalf("pre-sample descendant escape was falsely contained: %+v", record)
	}
	if record.ScopeIntegrity != ScopeUnverified && record.ScopeIntegrity != ScopeDescendantEscaped {
		t.Fatalf("pre-sample descendant escape integrity=%q, want unverified or witnessed escaped", record.ScopeIntegrity)
	}
}

func TestConfineStatusReportsTeardownIntegrityFacet(t *testing.T) {
	line := FormatConfineStatus(ConfineStatus{
		Slice: "finite.slice", Scope: ConfineScopePlaced, ScopeIntegrity: ScopeDescendantEscaped,
		DescendantEscape: &DescendantEscapeEvidence{PIDIdentity: PIDIdentity{PID: 42, StartTick: 100}, Cgroup: "/sys/fs/cgroup/sibling"},
	})
	for _, want := range []string{"scope-integrity=descendant-escaped", "escaped-pid=42", "escaped-cgroup=/sys/fs/cgroup/sibling"} {
		if !strings.Contains(line, want) {
			t.Fatalf("confine status %q lacks %q", line, want)
		}
	}
}

func TestConfineSingleProcessTeardownRemainsContained(t *testing.T) {
	scope := &confineFakeScope{}
	result, err := confineWithDeps(context.Background(), ConfineRequest{
		Slice: "finite.slice", Argv: []string{"/bin/true"}, SelfPath: os.Args[0], Stderr: io.Discard,
	}, confineUnitDeps(scope))
	if err != nil {
		t.Fatal(err)
	}
	if result.Status.ScopeIntegrity != ScopeContained {
		t.Fatalf("single-process confine integrity=%q result=%+v", result.Status.ScopeIntegrity, result)
	}
}

func TestRealCgroupConfineOutlivingDescendantIsKilled(t *testing.T) {
	parent := confineMemoryParent(t, "67108864")
	proof := t.TempDir()
	script := `set -eu
sh -c 'echo $$ > "$1/pid"; exec 1>&- 2>&-; exec sleep 30' sh "$1" &
while [ ! -s "$1/pid" ]; do sleep 0.001; done
sleep 0.05
exit 0`
	result, err := Confine(context.Background(), ConfineRequest{
		Slice: parent, MemoryReserve: 1, Argv: []string{"/bin/sh", "-c", script, "sh", proof},
		SelfPath: os.Args[0], Stderr: io.Discard,
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.Exit != 0 || result.Status.ScopeIntegrity != ScopeDescendantKilled {
		t.Fatalf("confine lingering descendant result=%+v, want descendant-killed", result)
	}
}

func TestRealCgroupConfineWitnessesSiblingEscape(t *testing.T) {
	parent := confineMemoryParent(t, "67108864")
	target, err := os.MkdirTemp(parent, ".aira-confine-escape-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = os.WriteFile(filepath.Join(target, "cgroup.kill"), []byte("1"), 0o644)
		deadline := time.Now().Add(time.Second)
		for time.Now().Before(deadline) {
			if os.Remove(target) == nil {
				return
			}
			time.Sleep(time.Millisecond)
		}
	})
	proof := t.TempDir()
	script := `set -eu
sh -c 'sleep 0.03; echo $$ > "$1/cgroup.procs"; printf moved > "$2/moved"; exec 1>&- 2>&-; exec sleep 30' sh "$1" "$2" &
while [ ! -s "$2/moved" ]; do sleep 0.001; done
sleep 0.03
exit 0`
	result, err := Confine(context.Background(), ConfineRequest{
		Slice: parent, MemoryReserve: 1, Argv: []string{"/bin/sh", "-c", script, "sh", target, proof},
		SelfPath: os.Args[0], Stderr: io.Discard,
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.Exit != 0 || result.Status.ScopeIntegrity != ScopeDescendantEscaped || result.Status.DescendantEscape == nil {
		t.Fatalf("confine sibling escape result=%+v", result)
	}
	if result.Status.DescendantEscape.Cgroup != target {
		t.Fatalf("confine escape evidence=%+v want cgroup=%q", result.Status.DescendantEscape, target)
	}
}
