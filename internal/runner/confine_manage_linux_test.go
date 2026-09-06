//go:build linux

package runner

import (
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"aira/internal/cgrouptest"
	"aira/internal/testdeadline"

	"golang.org/x/sys/unix"
)

func confineTestScopeID(name string, pid int, stamp int64) string {
	return "CONFINE-" + name + "-" + strconv.Itoa(pid) + "-" + strconv.FormatInt(stamp, 36)
}

// confineTestOwnedScopeID mints AIRA-52's owner-carrying form. It is the shape
// every real launch produces once an owner is resolved, and the shape the kill
// guard now reads ownership out of.
func confineTestOwnedScopeID(name, owner string, pid int, stamp int64) string {
	return confineTestScopeID(name, pid, stamp) + "@" + owner
}

func confineTestDelegateScopeID(name string, pid int, stamp int64) string {
	return "CONFINE-" + delegateRAMScopeIDMarker + "-" + name + "-" + strconv.Itoa(pid) + "-" + strconv.FormatInt(stamp, 36)
}

func writeConfineTestScope(t *testing.T, slice, scopeID, procs string) string {
	t.Helper()
	path := filepath.Join(slice, ".aira-"+scopeID)
	if err := os.Mkdir(path, 0o755); err != nil {
		t.Fatal(err)
	}
	for name, data := range map[string]string{
		"cgroup.procs": procs, "cgroup.events": "populated 1\n", "memory.current": "4096\n", "memory.max": "8192\n", "cgroup.kill": "",
	} {
		if procs == "" && name == "cgroup.events" {
			data = "populated 0\n"
		}
		if err := os.WriteFile(filepath.Join(path, name), []byte(data), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return path
}

func TestConfineScanUnionDeduplicatesAndRegistryOwnerWins(t *testing.T) {
	slice := t.TempDir()
	now := time.Now()
	owned := confineTestOwnedScopeID("build-name.with-dash", "session-a", 4101, now.Add(-time.Minute).UnixNano())
	scanOnly := confineTestScopeID("fallback", 4102, now.Add(-2*time.Minute).UnixNano())
	marked := confineTestDelegateScopeID("ceiling-suite", 4105, now.Add(-3*time.Minute).UnixNano())
	pending := confineTestOwnedScopeID("pending", "session-a", 4103, now.UnixNano())
	writeConfineTestScope(t, slice, owned, "51\n52\n")
	writeConfineTestScope(t, slice, scanOnly, "61\n")
	writeConfineTestScope(t, slice, marked, "62\n")
	regular := confineTestScopeID("not-a-directory", 4104, now.UnixNano())
	if err := os.WriteFile(filepath.Join(slice, ".aira-"+regular), []byte("not a cgroup"), 0o644); err != nil {
		t.Fatal(err)
	}

	result, err := listConfinesWithDeps(context.Background(), slice, []ConfineRegistryEntry{{ScopeID: owned}, {ScopeID: pending}}, confineScanDeps{
		now: time.Now, readField: readConfineScopeField, waitEmpty: waitEmpty,
	})
	if err != nil || result.Verdict != "pass" || len(result.Scopes) != 4 {
		t.Fatalf("result=%+v err=%v", result, err)
	}
	seen := map[string]ConfineRecord{}
	for _, record := range result.Scopes {
		seen[record.ScopeID] = record
	}
	if record := seen[owned]; record.Owner != "session-a" || record.Name != "build-name.with-dash" || record.SupervisorPID == nil || *record.SupervisorPID != 4101 || record.Populated == nil || *record.Populated != 2 {
		t.Fatalf("owned record=%+v", record)
	}
	if record := seen[scanOnly]; record.Owner != ConfineUnknownOwner {
		t.Fatalf("scan-only record=%+v", record)
	}
	if record := seen[marked]; record.Name != "ceiling-suite" || !validConfineScopeID(record.ScopeID) {
		t.Fatalf("marked record=%+v", record)
	}
	if record := seen[pending]; !record.Pending || record.Owner != "session-a" || record.Populated != nil {
		t.Fatalf("pending record=%+v", record)
	}
}

func TestConfineListHonestyUnreadablePerFieldAndHusk(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "absent")
	result, err := ListConfines(context.Background(), missing, nil)
	if err != nil || result.Verdict != "unevaluated" || result.Reason == "" || result.Scopes == nil {
		t.Fatalf("unreadable result=%+v err=%v", result, err)
	}
	if kill, killErr := KillConfine(context.Background(), missing, "job", "owner", true, nil); killErr == nil || !strings.HasPrefix(killErr.Error(), CodeConfineKillUnconfirmed+":") || kill.Status == "killed" {
		t.Fatalf("unreadable kill=%+v err=%v", kill, killErr)
	}
	reap, reapErr := ReapOrphanedConfineScopes(context.Background(), missing, time.Minute, nil, nil)
	if reapErr != nil || reap.Verdict != "unevaluated" || reap.Reason == "" || reap.Reaped == nil {
		t.Fatalf("unreadable reap=%+v err=%v", reap, reapErr)
	}

	slice := t.TempDir()
	scopeID := confineTestScopeID("husk", 4201, time.Now().Add(-time.Second).UnixNano())
	path := writeConfineTestScope(t, slice, scopeID, "")
	if err := os.Remove(filepath.Join(path, "memory.current")); err != nil {
		t.Fatal(err)
	}
	result, err = ListConfines(context.Background(), slice, nil)
	if err != nil || len(result.Scopes) != 1 {
		t.Fatalf("result=%+v err=%v", result, err)
	}
	record := result.Scopes[0]
	if record.Populated == nil || *record.Populated != 0 || record.RSSBytes != nil || !confineContainsString(record.UnevaluatedFields, "rss") {
		t.Fatalf("husk record=%+v", record)
	}
}

func TestConfineKillOwnershipGuardAndSteal(t *testing.T) {
	slice := t.TempDir()
	scopeID := confineTestOwnedScopeID("owned", "session-a", 4301, time.Now().UnixNano())
	path := writeConfineTestScope(t, slice, scopeID, "71\n")
	registry := []ConfineRegistryEntry{{ScopeID: scopeID}}
	deps := defaultConfineScanDeps()
	deps.waitEmpty = func(_ context.Context, scope Scope, _ time.Duration) error {
		return os.WriteFile(filepath.Join(scope.Reference(), "cgroup.events"), []byte("populated 0\n"), 0o644)
	}
	if _, err := killConfineWithDeps(context.Background(), slice, scopeID, "session-b", false, registry, time.Second, deps); err == nil || !strings.HasPrefix(err.Error(), CodeConfineOwnerUnverified+":") {
		t.Fatalf("foreign kill err=%v", err)
	}
	if data, _ := os.ReadFile(filepath.Join(path, "cgroup.kill")); len(data) != 0 {
		t.Fatalf("ownership refusal wrote kill=%q", data)
	}
	result, err := killConfineWithDeps(context.Background(), slice, scopeID, "session-b", true, registry, time.Second, deps)
	if err != nil || result.Status != "killed" || result.Owner != "session-a" {
		t.Fatalf("result=%+v err=%v", result, err)
	}
	if data, _ := os.ReadFile(filepath.Join(path, "cgroup.kill")); string(data) != "1" {
		t.Fatalf("kill file=%q", data)
	}
}

// TestConfineKillOwnerSurvivesADaemonWithNoMemoryOfTheJob is AIRA-52's
// regression test. The daemon's restart-adoption path rebuilds aggregate reserve
// scalars from a cgroup scan and never recreates per-job waiters, so owner used
// to be gone forever for any job whose lifetime spanned a restart -- forcing an
// unnecessary --steal to kill your OWN job. An empty registry and no daemon
// memory whatsoever is exactly that post-restart state.
//
// verifies: AIRA-52
func TestConfineKillOwnerSurvivesADaemonWithNoMemoryOfTheJob(t *testing.T) {
	slice := t.TempDir()
	scopeID := confineTestOwnedScopeID("survivor", "session-a", 4310, time.Now().UnixNano())
	writeConfineTestScope(t, slice, scopeID, "73\n")
	deps := defaultConfineScanDeps()
	deps.waitEmpty = func(_ context.Context, scope Scope, _ time.Duration) error {
		return os.WriteFile(filepath.Join(scope.Reference(), "cgroup.events"), []byte("populated 0\n"), 0o644)
	}
	listed, err := listConfinesWithDeps(context.Background(), slice, nil, deps)
	if err != nil || len(listed.Scopes) != 1 || listed.Scopes[0].Owner != "session-a" {
		t.Fatalf("scan-only list=%+v err=%v, want owner recovered from the scope id", listed, err)
	}
	result, err := killConfineWithDeps(context.Background(), slice, scopeID, "session-a", false, nil, time.Second, deps)
	if err != nil || result.Status != "killed" {
		t.Fatalf("owner must be able to kill its own job with no --steal after a restart: result=%+v err=%v", result, err)
	}
}

// TestConfineKillRefusesAnInferredOwnerWithoutSteal pins AIRA-23's safety
// boundary: an inferred owner is published so --list is actionable, and is NEVER
// ownership proof. Two sessions in one directory infer the same string, so
// honouring it would let either kill the other's job without --steal.
//
// verifies: AIRA-23
func TestConfineKillRefusesAnInferredOwnerWithoutSteal(t *testing.T) {
	inferred := InferConfineOwner("/home/someone/worktree-a")
	if !strings.HasPrefix(inferred, ConfineInferredOwnerPrefix) || ConfineOwnerIsAttested(inferred) {
		t.Fatalf("inferred=%q must be marked and unattested", inferred)
	}
	slice := t.TempDir()
	scopeID := confineTestOwnedScopeID("inferred", inferred, 4311, time.Now().UnixNano())
	path := writeConfineTestScope(t, slice, scopeID, "74\n")
	listed, err := listConfinesWithDeps(context.Background(), slice, nil, defaultConfineScanDeps())
	if err != nil || len(listed.Scopes) != 1 || listed.Scopes[0].Owner != inferred {
		t.Fatalf("list=%+v err=%v, want the inferred owner surfaced instead of %q", listed, err, ConfineUnknownOwner)
	}
	// Even the SAME inferred identity must not open the guard.
	if _, err := KillConfine(context.Background(), slice, scopeID, inferred, false, nil); err == nil || !strings.HasPrefix(err.Error(), CodeConfineOwnerUnverified+":") {
		t.Fatalf("an inferred owner opened the kill guard: err=%v", err)
	}
	if data, _ := os.ReadFile(filepath.Join(path, "cgroup.kill")); len(data) != 0 {
		t.Fatalf("refusal wrote kill=%q", data)
	}
}

// TestConfineKillTakesOwnerFromTheScopeIDNotTheRegistry: the scope directory
// name is the single source of ownership truth. A registry entry naming the same
// scope contributes only its existence (a Pending row), never an identity.
func TestConfineKillTakesOwnerFromTheScopeIDNotTheRegistry(t *testing.T) {
	slice := t.TempDir()
	scopeID := confineTestOwnedScopeID("fresh", "session-a", 4302, time.Now().UnixNano())
	writeConfineTestScope(t, slice, scopeID, "72\n")
	deps := defaultConfineScanDeps()
	deps.waitEmpty = func(_ context.Context, scope Scope, _ time.Duration) error {
		return os.WriteFile(filepath.Join(scope.Reference(), "cgroup.events"), []byte("populated 0\n"), 0o644)
	}
	result, err := killConfineWithDeps(context.Background(), slice, scopeID, "session-a", false,
		[]ConfineRegistryEntry{{ScopeID: scopeID}}, time.Second, deps)
	if err != nil || result.Status != "killed" || result.Owner != "session-a" {
		t.Fatalf("result=%+v err=%v", result, err)
	}
}

// verifies: the kill observes SUBTREE population (cgroup.events populated), not
// leaf-only cgroup.procs, so a workload that migrated into a child cgroup inside
// its own scope is still killed (cgroup.kill is recursive) instead of mis-reported
// as not-launched and left uncancellable. RED against a leaf-only Members() observe.
func TestConfineKillObservesSubtreePopulationNotLeafOnly(t *testing.T) {
	slice := t.TempDir()
	id := confineTestOwnedScopeID("nested", "owner", 4601, time.Now().UnixNano())
	path := filepath.Join(slice, ".aira-"+id)
	if err := os.Mkdir(path, 0o755); err != nil {
		t.Fatal(err)
	}
	// Leaf cgroup.procs is EMPTY (the workload migrated into a self-created child
	// cgroup) while the SUBTREE is populated per cgroup.events.
	for name, data := range map[string]string{
		"cgroup.procs": "", "cgroup.events": "populated 1\n", "memory.current": "4096\n", "memory.max": "8192\n", "cgroup.kill": "",
	} {
		if err := os.WriteFile(filepath.Join(path, name), []byte(data), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	deps := defaultConfineScanDeps()
	deps.waitEmpty = func(_ context.Context, scope Scope, _ time.Duration) error {
		return os.WriteFile(filepath.Join(scope.Reference(), "cgroup.events"), []byte("populated 0\n"), 0o644)
	}
	result, err := killConfineWithDeps(context.Background(), slice, id, "owner", false, []ConfineRegistryEntry{{ScopeID: id}}, time.Second, deps)
	if err != nil || result.Status != "killed" {
		t.Fatalf("subtree-populated kill result=%+v err=%v (a leaf-only observe would mis-report not-launched)", result, err)
	}
	if data, _ := os.ReadFile(filepath.Join(path, "cgroup.kill")); string(data) != "1" {
		t.Fatalf("kill file=%q", data)
	}
}

// verifies: the ownership guard refuses an unknown/scan-only owner AND an unknown
// CALLER without --steal — the daemon-down / AIRA_CONFINE_OWNER-unset incident
// class where BOTH resolve to "unknown". A guard that dropped the !known /
// caller-unknown clauses would let "unknown"=="unknown" bypass and kill a foreign
// scope with no --steal (the exact cross-session bypass this feature prevents).
func TestConfineKillScanOnlyAndUnknownCallerRequireSteal(t *testing.T) {
	newScope := func(t *testing.T) (string, string) {
		slice := t.TempDir()
		id := confineTestScopeID("scanonly", 4701, time.Now().UnixNano())
		writeConfineTestScope(t, slice, id, "77\n")
		return slice, id
	}
	assertRefusedNoKill := func(t *testing.T, slice, id, caller string) {
		t.Helper()
		// scan-only: nil registry AND nil lookup → owner resolves to unknown.
		result, err := KillConfine(context.Background(), slice, id, caller, false, nil)
		if err == nil || !strings.HasPrefix(err.Error(), CodeConfineOwnerUnverified+":") || result.Status == "killed" {
			t.Fatalf("caller=%q result=%+v err=%v, want owner-unverified refusal", caller, result, err)
		}
		if data, _ := os.ReadFile(filepath.Join(slice, ".aira-"+id, "cgroup.kill")); len(data) != 0 {
			t.Fatalf("caller=%q refusal wrote kill=%q", caller, data)
		}
	}
	t.Run("scan-only-foreign-concrete-caller", func(t *testing.T) {
		slice, id := newScope(t)
		assertRefusedNoKill(t, slice, id, "session-b")
	})
	t.Run("scan-only-and-unknown-caller", func(t *testing.T) {
		slice, id := newScope(t)
		assertRefusedNoKill(t, slice, id, ConfineUnknownOwner)
	})
	t.Run("scan-only-steal-kills", func(t *testing.T) {
		slice, id := newScope(t)
		deps := defaultConfineScanDeps()
		deps.waitEmpty = func(_ context.Context, scope Scope, _ time.Duration) error {
			return os.WriteFile(filepath.Join(scope.Reference(), "cgroup.events"), []byte("populated 0\n"), 0o644)
		}
		result, err := killConfineWithDeps(context.Background(), slice, id, ConfineUnknownOwner, true, nil, time.Second, deps)
		if err != nil || result.Status != "killed" {
			t.Fatalf("scan-only steal result=%+v err=%v", result, err)
		}
	})
}

func TestConfineKillEmptyAndUnconfirmedNeverReportKilled(t *testing.T) {
	t.Run("empty-not-launched", func(t *testing.T) {
		slice := t.TempDir()
		id := confineTestOwnedScopeID("pending", "owner", 4401, time.Now().UnixNano())
		path := writeConfineTestScope(t, slice, id, "")
		result, err := KillConfine(context.Background(), slice, id, "owner", false, []ConfineRegistryEntry{{ScopeID: id}})
		if err == nil || !strings.HasPrefix(err.Error(), CodeConfineNotLaunched+":") || result.Status == "killed" {
			t.Fatalf("result=%+v err=%v", result, err)
		}
		if data, _ := os.ReadFile(filepath.Join(path, "cgroup.kill")); len(data) != 0 {
			t.Fatalf("empty scope kill=%q", data)
		}
	})
	t.Run("write-error", func(t *testing.T) {
		slice := t.TempDir()
		id := confineTestOwnedScopeID("writefail", "owner", 4402, time.Now().UnixNano())
		path := writeConfineTestScope(t, slice, id, "81\n")
		if err := os.Remove(filepath.Join(path, "cgroup.kill")); err != nil {
			t.Fatal(err)
		}
		if err := os.Mkdir(filepath.Join(path, "cgroup.kill"), 0o755); err != nil {
			t.Fatal(err)
		}
		result, err := KillConfine(context.Background(), slice, id, "owner", false, []ConfineRegistryEntry{{ScopeID: id}})
		if err == nil || !strings.HasPrefix(err.Error(), CodeConfineKillUnconfirmed+":") || result.Status == "killed" {
			t.Fatalf("result=%+v err=%v", result, err)
		}
	})
	t.Run("wait-timeout", func(t *testing.T) {
		slice := t.TempDir()
		id := confineTestOwnedScopeID("timeout", "owner", 4403, time.Now().UnixNano())
		writeConfineTestScope(t, slice, id, "91\n")
		deps := defaultConfineScanDeps()
		deps.waitEmpty = func(context.Context, Scope, time.Duration) error { return errors.New("timeout") }
		result, err := killConfineWithDeps(context.Background(), slice, id, "owner", false, []ConfineRegistryEntry{{ScopeID: id}}, time.Millisecond, deps)
		if err == nil || !strings.HasPrefix(err.Error(), CodeConfineKillUnconfirmed+":") || result.Status == "killed" {
			t.Fatalf("result=%+v err=%v", result, err)
		}
	})
}

func TestConfineKillSelectorAmbiguousAndNotFound(t *testing.T) {
	slice := t.TempDir()
	stamp := time.Now().UnixNano()
	for _, id := range []string{confineTestScopeID("same", 4501, stamp), confineTestScopeID("same", 4502, stamp+1), confineTestScopeID("other", 4501, stamp+2)} {
		writeConfineTestScope(t, slice, id, "")
	}
	for _, selector := range []string{"same", "4501"} {
		if _, err := KillConfine(context.Background(), slice, selector, "owner", true, nil); err == nil || !strings.HasPrefix(err.Error(), "E_SELECTOR_AMBIGUOUS:") {
			t.Fatalf("selector=%q err=%v", selector, err)
		}
	}
	if _, err := KillConfine(context.Background(), slice, "missing", "owner", true, nil); err == nil || !strings.HasPrefix(err.Error(), CodeConfineNotFound+":") {
		t.Fatalf("not-found err=%v", err)
	}
}

func TestConfineMidLaunchKillDoesNotFabricateOrReleaseAndLaunchContinues(t *testing.T) {
	slice := t.TempDir()
	fake := &confineFakeScope{}
	closer := &confineCountingCloser{}
	deps := confineUnitDeps(fake)
	deps.resolveSlicePath = func(string) (string, bool, string) { return slice, true, "" }
	var scopeID string
	deps.newBackend = func(string) ScopeBackend {
		return confineBackendFunc{create: func(_ context.Context, id string) (Scope, error) {
			scopeID = id
			writeConfineTestScope(t, slice, id, "")
			return fake, nil
		}}
	}
	deps.admit = func(_ context.Context, _ string, request ConfineRequest, _ int64) (admissionResult, error) {
		if request.ScopeID == "" || request.Owner != "session-a" {
			t.Fatalf("admit request=%+v", request)
		}
		return admissionResult{state: "immediate", release: closer}, nil
	}
	innerStart := deps.start
	deps.start = func(command *confineCommand) error {
		result, killErr := KillConfine(context.Background(), slice, scopeID, "session-a", false, []ConfineRegistryEntry{{ScopeID: scopeID}})
		if killErr == nil || !strings.HasPrefix(killErr.Error(), CodeConfineNotLaunched+":") || result.Status == "killed" {
			t.Fatalf("mid-launch result=%+v err=%v", result, killErr)
		}
		if closer.count != 0 {
			t.Fatalf("mid-launch kill released lease count=%d", closer.count)
		}
		return innerStart(command)
	}
	result, err := confineWithDeps(context.Background(), ConfineRequest{Slice: "finite.slice", Name: "midlaunch", Owner: "session-a", Argv: []string{"/bin/true"}, SelfPath: os.Args[0], Stderr: io.Discard}, deps)
	if err != nil || result.Exit != 0 {
		t.Fatalf("launch result=%+v err=%v", result, err)
	}
	fake.mu.Lock()
	started := fake.started
	fake.mu.Unlock()
	if !started || closer.count != 1 {
		t.Fatalf("started=%v final lease releases=%d", started, closer.count)
	}
}

func TestConfineRealScopeKillSurvivesLauncherIdentityLoss(t *testing.T) {
	parent := cgrouptest.IsolatedScopeParent(t)
	if _, finite := effectiveConfineCap(parent); !finite {
		cgrouptest.SkipOrFailRealCgroup(t, "confine kill requires a capped cgroup ancestor (run under aira confine)")
	}
	backend := newDefaultBackend(parent)
	if err := backend.Probe(context.Background()); err != nil {
		cgrouptest.SkipOrFailRealCgroup(t, "real confine kill backend unavailable: %v", err)
	}
	id := confineTestScopeID("reparented", os.Getpid(), time.Now().UnixNano())
	scope, err := backend.Create(context.Background(), id)
	if err != nil {
		cgrouptest.SkipOrFailRealCgroup(t, "real confine kill scope unavailable: %v", err)
	}
	defer func() { _ = scope.Kill(); _ = waitEmpty(context.Background(), scope, time.Second); _ = scope.Remove() }()
	// The short-lived launcher exits while its descendant remains in the scope,
	// reproducing the identity-loss/reparenting shape that PID-directed cleanup
	// cannot handle.
	command := exec.Command("/bin/sh", "-c", "sleep 60 &")
	command.SysProcAttr = &syscall.SysProcAttr{UseCgroupFD: true, CgroupFD: scope.FD()}
	if err := command.Start(); err != nil {
		cgrouptest.SkipOrFailRealCgroup(t, "start real confined workload: %v", err)
	}
	if err := command.Wait(); err != nil {
		t.Fatalf("launcher exit: %v", err)
	}
	deadline := time.Now().Add(testdeadline.Wait(time.Second))
	for {
		members, memberErr := scope.Members()
		if memberErr == nil && len(members) > 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("workload never populated scope: members=%v err=%v", members, memberErr)
		}
		time.Sleep(10 * time.Millisecond)
	}
	result, err := KillConfine(context.Background(), parent, id, "session-b", true, nil)
	if err != nil || result.Status != "killed" {
		t.Fatalf("result=%+v err=%v", result, err)
	}
	if empty, emptyErr := scope.Empty(); emptyErr != nil || !empty {
		t.Fatalf("reparented workload survived cgroup.kill: empty=%v err=%v", empty, emptyErr)
	}
}

func TestReapOrphanedConfineScopesRealCgroupSafetyGates(t *testing.T) {
	parent := cgrouptest.IsolatedScopeParent(t)
	backend := newDefaultBackend(parent)
	if err := backend.Probe(context.Background()); err != nil {
		cgrouptest.SkipOrFailRealCgroup(t, "real confine reaper backend unavailable: %v", err)
	}

	exited := exec.Command("/bin/true")
	if err := exited.Run(); err != nil {
		t.Fatal(err)
	}
	deadPID := exited.ProcessState.Pid()
	now := time.Now()
	oldStamp := now.Add(-10 * time.Second).UnixNano()
	oldOrphan := confineTestScopeID("orphan", deadPID, oldStamp)
	live := confineTestScopeID("live", os.Getpid(), oldStamp+1)
	populated := confineTestScopeID("populated", deadPID, oldStamp+2)
	young := confineTestScopeID("young", deadPID, now.UnixNano())
	// leased is empty + dead-PID + old like oldOrphan, but a live daemon admit
	// lease (hasLiveLease) must keep it — the PID-namespace-independent guard.
	leased := confineTestScopeID("leased", deadPID, oldStamp+3)

	for _, id := range []string{oldOrphan, live, populated, young, leased} {
		scope, err := backend.Create(context.Background(), id)
		if err != nil {
			cgrouptest.SkipOrFailRealCgroup(t, "create real reaper scope %s: %v", id, err)
		}
		if err := scope.(*linuxScope).fd.Close(); err != nil {
			t.Fatal(err)
		}
	}

	populatedPath := filepath.Join(parent, ".aira-"+populated)
	populatedFD, err := os.OpenFile(populatedPath, os.O_RDONLY|syscall.O_DIRECTORY|syscall.O_CLOEXEC, 0)
	if err != nil {
		t.Fatal(err)
	}
	sleeper := exec.Command("/bin/sleep", "60")
	sleeper.SysProcAttr = &syscall.SysProcAttr{UseCgroupFD: true, CgroupFD: int(populatedFD.Fd())}
	if err := sleeper.Start(); err != nil {
		_ = populatedFD.Close()
		cgrouptest.SkipOrFailRealCgroup(t, "start populated reaper scope workload: %v", err)
	}
	_ = populatedFD.Close()
	t.Cleanup(func() {
		_ = sleeper.Process.Kill()
		_ = sleeper.Wait()
	})

	result, err := ReapOrphanedConfineScopes(context.Background(), parent, 5*time.Second, func(pid int) bool {
		return pid == deadPID
	}, func(scopeID string) bool {
		return scopeID == leased
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.Verdict != "pass" || !reflect.DeepEqual(result.Reaped, []string{oldOrphan}) {
		t.Fatalf("result=%+v, want only %s reaped", result, oldOrphan)
	}
	if _, err := os.Stat(filepath.Join(parent, ".aira-"+oldOrphan)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("old orphan still exists: %v", err)
	}
	for _, id := range []string{live, populated, young, leased} {
		if _, err := os.Stat(filepath.Join(parent, ".aira-"+id)); err != nil {
			t.Fatalf("guarded scope %s removed: %v", id, err)
		}
	}
}

func TestReapOrphanedConfineScopesReapsNestedEmptyTree(t *testing.T) {
	parent, deadPID := reaperTestParentAndDeadPID(t)
	id := confineTestScopeID("nested-empty", deadPID, time.Now().Add(-10*time.Second).UnixNano())
	root := createReaperTestScope(t, parent, id)
	child := mkdirReaperTestCgroup(t, root, "child")
	mkdirReaperTestCgroup(t, child, "grandchild")

	result, err := ReapOrphanedConfineScopes(context.Background(), parent, time.Second, reaperTestDead(deadPID), nil)
	if err != nil || !reflect.DeepEqual(result.Reaped, []string{id}) || result.Skipped != 0 {
		t.Fatalf("result=%+v err=%v", result, err)
	}
	assertReaperTestMissing(t, root)
}

// This is deliberately not a lone live child: a naive post-order walk would
// retain the live branch but still strip the empty sibling. The root's
// cgroup.events populated proof must preserve the entire tree.
func TestReapOrphanedConfineScopesKeepsEmptySiblingOfLiveNestedBranch(t *testing.T) {
	parent, deadPID := reaperTestParentAndDeadPID(t)
	id := confineTestScopeID("nested-live", deadPID, time.Now().Add(-10*time.Second).UnixNano())
	root := createReaperTestScope(t, parent, id)
	// A naive post-order walk (no cgroup.events proof) aborts on the first child
	// whose rmdir fails (the live branch), so whether an empty sibling is stripped
	// depends on kernfs enumeration order (which is NOT creation order). Create
	// SEVERAL empty siblings plus a deep one so at least one is near-certain to be
	// enumerated before the live branch: a Phase-1 regression would then strip it
	// and fail this test, while the real code skips the whole non-empty scope
	// order-independently.
	survivors := []string{root}
	for _, n := range []string{"empty0", "empty1", "empty2", "empty3", "empty4", "empty5"} {
		survivors = append(survivors, mkdirReaperTestCgroup(t, root, n))
	}
	deep := mkdirReaperTestCgroup(t, root, "deep")
	survivors = append(survivors, deep, mkdirReaperTestCgroup(t, deep, "grandchild"))
	live := mkdirReaperTestCgroup(t, root, "live")
	survivors = append(survivors, live)
	sleeper := startReaperTestSleeper(t, live)
	t.Cleanup(func() { stopReaperTestSleeper(sleeper) })

	result, err := ReapOrphanedConfineScopes(context.Background(), parent, time.Second, reaperTestDead(deadPID), nil)
	if err != nil || len(result.Reaped) != 0 || result.Skipped != 1 {
		t.Fatalf("result=%+v err=%v", result, err)
	}
	for _, path := range survivors {
		if _, statErr := os.Stat(path); statErr != nil {
			t.Fatalf("nested live tree changed at %s: %v", path, statErr)
		}
	}
}

func TestReapOrphanedConfineScopesDeepTreeSafety(t *testing.T) {
	t.Run("empty", func(t *testing.T) {
		parent, deadPID := reaperTestParentAndDeadPID(t)
		id := confineTestScopeID("deep-empty", deadPID, time.Now().Add(-10*time.Second).UnixNano())
		root := createReaperTestScope(t, parent, id)
		one := mkdirReaperTestCgroup(t, root, "one")
		two := mkdirReaperTestCgroup(t, one, "two")
		mkdirReaperTestCgroup(t, two, "three")
		result, err := ReapOrphanedConfineScopes(context.Background(), parent, time.Second, reaperTestDead(deadPID), nil)
		if err != nil || !reflect.DeepEqual(result.Reaped, []string{id}) || result.Skipped != 0 {
			t.Fatalf("result=%+v err=%v", result, err)
		}
		assertReaperTestMissing(t, root)
	})
	t.Run("deepest-live", func(t *testing.T) {
		parent, deadPID := reaperTestParentAndDeadPID(t)
		id := confineTestScopeID("deep-live", deadPID, time.Now().Add(-10*time.Second).UnixNano())
		root := createReaperTestScope(t, parent, id)
		one := mkdirReaperTestCgroup(t, root, "one")
		two := mkdirReaperTestCgroup(t, one, "two")
		three := mkdirReaperTestCgroup(t, two, "three")
		sleeper := startReaperTestSleeper(t, three)
		t.Cleanup(func() { stopReaperTestSleeper(sleeper) })
		result, err := ReapOrphanedConfineScopes(context.Background(), parent, time.Second, reaperTestDead(deadPID), nil)
		if err != nil || len(result.Reaped) != 0 || result.Skipped != 1 {
			t.Fatalf("result=%+v err=%v", result, err)
		}
		for _, path := range []string{root, one, two, three} {
			if _, statErr := os.Stat(path); statErr != nil {
				t.Fatalf("deep live tree changed at %s: %v", path, statErr)
			}
		}
	})
}

func TestReapOrphanedConfineScopesRepopulationAfterProof(t *testing.T) {
	parent, deadPID := reaperTestParentAndDeadPID(t)
	id := confineTestScopeID("repopulate", deadPID, time.Now().Add(-10*time.Second).UnixNano())
	root := createReaperTestScope(t, parent, id)
	empty := mkdirReaperTestCgroup(t, root, "empty")
	live := mkdirReaperTestCgroup(t, root, "live")
	var sleeper *exec.Cmd
	deps := defaultConfineScanDeps()
	deps.afterReapEmptyProof = func() { sleeper = startReaperTestSleeper(t, live) }
	result, err := reapOrphanedConfineScopesWithDeps(context.Background(), parent, time.Second, reaperTestDead(deadPID), nil, deps)
	if sleeper != nil {
		t.Cleanup(func() { stopReaperTestSleeper(sleeper) })
	}
	if err != nil || len(result.Reaped) != 0 || result.Skipped != 1 {
		t.Fatalf("result=%+v err=%v", result, err)
	}
	for _, path := range []string{root, live} {
		if _, statErr := os.Stat(path); statErr != nil {
			t.Fatalf("repopulated node or ancestor removed at %s: %v", path, statErr)
		}
	}
	// The empty sibling may have been removed before the live node became busy.
	if _, statErr := os.Stat(empty); statErr != nil && !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("empty sibling stat: %v", statErr)
	}
}

func TestReapOrphanedConfineScopesPlainLeafUnchanged(t *testing.T) {
	parent, deadPID := reaperTestParentAndDeadPID(t)
	id := confineTestScopeID("plain-leaf", deadPID, time.Now().Add(-10*time.Second).UnixNano())
	root := createReaperTestScope(t, parent, id)
	result, err := ReapOrphanedConfineScopes(context.Background(), parent, time.Second, reaperTestDead(deadPID), nil)
	if err != nil || !reflect.DeepEqual(result.Reaped, []string{id}) || result.Skipped != 0 {
		t.Fatalf("result=%+v err=%v", result, err)
	}
	assertReaperTestMissing(t, root)
}

// verifies: ReapScopeIfEmpty's success is the AIRA-49 release gate, so a
// genuinely empty scope must actually be removed from disk — a gate that never
// succeeds would silently make the whole stale-lease sweep inert.
func TestReapScopeIfEmptyRemovesAGenuinelyEmptyScope(t *testing.T) {
	parent, deadPID := reaperTestParentAndDeadPID(t)
	id := confineTestScopeID("single-empty", deadPID, time.Now().Add(-10*time.Second).UnixNano())
	root := createReaperTestScope(t, parent, id)

	reaped, err := ReapScopeIfEmpty(parent, id, nil)
	if err != nil || !reaped {
		t.Fatalf("ReapScopeIfEmpty=(%v, %v), want (true, nil)", reaped, err)
	}
	assertReaperTestMissing(t, root)
}

// verifies: the emptiness proof is SUBTREE-aware, not leaf-only. The candidate's
// own cgroup.procs is empty here while a nested child holds a live process —
// AIRA-49 v1's leaf-only Populated check would have released the ledger lease of
// a job that is still running.
func TestReapScopeIfEmptyLeavesALiveNestedChildAlone(t *testing.T) {
	parent, deadPID := reaperTestParentAndDeadPID(t)
	id := confineTestScopeID("single-nested-live", deadPID, time.Now().Add(-10*time.Second).UnixNano())
	root := createReaperTestScope(t, parent, id)
	live := mkdirReaperTestCgroup(t, root, "live")
	sleeper := startReaperTestSleeper(t, live)
	t.Cleanup(func() { stopReaperTestSleeper(sleeper) })

	reaped, err := ReapScopeIfEmpty(parent, id, nil)
	if reaped {
		t.Fatalf("ReapScopeIfEmpty=(%v, %v), want a refusal for a live nested child", reaped, err)
	}
	for _, path := range []string{root, live} {
		if _, statErr := os.Stat(path); statErr != nil {
			t.Fatalf("live nested tree changed at %s: %v", path, statErr)
		}
	}
}

// verifies: "reap succeeded" is TOCTOU-IMMUNE, not merely TOCTOU-unlikely. A
// process is injected into the scope after the empty proof has already passed
// but before the Unlinkat, and the kernel itself must still refuse the removal.
// This is the direct proof behind AIRA-49's release-only-on-confirmed-reap gate.
func TestReapScopeIfEmptyDoesNotRemoveAScopeRepopulatedAfterTheEmptyProof(t *testing.T) {
	parent, deadPID := reaperTestParentAndDeadPID(t)
	id := confineTestScopeID("single-repopulate", deadPID, time.Now().Add(-10*time.Second).UnixNano())
	root := createReaperTestScope(t, parent, id)
	empty := mkdirReaperTestCgroup(t, root, "empty")
	live := mkdirReaperTestCgroup(t, root, "live")
	var sleeper *exec.Cmd

	reaped, err := ReapScopeIfEmpty(parent, id, func() { sleeper = startReaperTestSleeper(t, live) })
	if sleeper != nil {
		t.Cleanup(func() { stopReaperTestSleeper(sleeper) })
	}
	if reaped {
		t.Fatalf("ReapScopeIfEmpty=(%v, %v), want a refusal after post-proof repopulation", reaped, err)
	}
	for _, path := range []string{root, live} {
		if _, statErr := os.Stat(path); statErr != nil {
			t.Fatalf("repopulated node or ancestor removed at %s: %v", path, statErr)
		}
	}
	// The empty sibling may have been removed before the live node became busy.
	if _, statErr := os.Stat(empty); statErr != nil && !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("empty sibling stat: %v", statErr)
	}
}

// verifies: a scope id is validated before any path is built from it, so a
// traversal-shaped id can never reach Openat/Unlinkat.
func TestReapScopeIfEmptyRejectsAnInvalidScopeID(t *testing.T) {
	ok, err := ReapScopeIfEmpty(t.TempDir(), "../not-a-real-scope-id", nil)
	if ok || err == nil {
		t.Fatalf("got (%v, %v), want a rejection", ok, err)
	}
}

func TestConfineReapWalkerUsesNoFollowDirectoryOpens(t *testing.T) {
	parent := t.TempDir()
	root := mkdirReaperTestCgroup(t, parent, "scope")
	mkdirReaperTestCgroup(t, root, "child")
	for _, path := range []string{root, filepath.Join(root, "child")} {
		if err := os.WriteFile(filepath.Join(path, "cgroup.events"), []byte("populated 0\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	originalOpenat := confineReapOpenat
	t.Cleanup(func() { confineReapOpenat = originalOpenat })
	var flags []int
	confineReapOpenat = func(dirfd int, path string, flag int, perm uint32) (int, error) {
		flags = append(flags, flag)
		return unix.Openat(dirfd, path, flag, perm)
	}
	parentFD, err := unix.Open(parent, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer unix.Close(parentFD)
	// On a real cgroupfs, rmdir removes a cgroup despite its interface files;
	// the walker (correctly) only rmdirs directories and never unlinks interface
	// files (unlinking cgroup.events would EPERM on a real cgroup). A tmpdir
	// cannot replicate that, so drop the simulated interface files after the
	// empty-proof — mirroring the kernel — so the post-order rmdir can complete
	// and the O_NOFOLLOW open flags (this test's actual subject) are exercised.
	mimicCgroupfsRemoval := func() {
		_ = os.Remove(filepath.Join(root, "child", "cgroup.events"))
		_ = os.Remove(filepath.Join(root, "cgroup.events"))
	}
	if reaped, err := reapEmptyConfineScopeTree(parentFD, "scope", mimicCgroupfsRemoval); err != nil || !reaped {
		t.Fatalf("reaped=%v err=%v", reaped, err)
	}
	if len(flags) < 2 {
		t.Fatalf("directory opens=%d, want root and child", len(flags))
	}
	for _, flag := range flags {
		if flag&(unix.O_NOFOLLOW|unix.O_DIRECTORY|unix.O_CLOEXEC) != unix.O_NOFOLLOW|unix.O_DIRECTORY|unix.O_CLOEXEC {
			t.Fatalf("open flags %#x lack O_NOFOLLOW|O_DIRECTORY|O_CLOEXEC", flag)
		}
	}
}

func reaperTestParentAndDeadPID(t *testing.T) (string, int) {
	t.Helper()
	parent := cgrouptest.IsolatedScopeParent(t)
	backend := newDefaultBackend(parent)
	if err := backend.Probe(context.Background()); err != nil {
		cgrouptest.SkipOrFailRealCgroup(t, "real confine reaper backend unavailable: %v", err)
	}
	exited := exec.Command("/bin/true")
	if err := exited.Run(); err != nil {
		t.Fatal(err)
	}
	return parent, exited.ProcessState.Pid()
}

func createReaperTestScope(t *testing.T, parent, id string) string {
	t.Helper()
	scope, err := newDefaultBackend(parent).Create(context.Background(), id)
	if err != nil {
		cgrouptest.SkipOrFailRealCgroup(t, "create real reaper scope %s: %v", id, err)
	}
	if err := scope.(*linuxScope).fd.Close(); err != nil {
		t.Fatal(err)
	}
	return filepath.Join(parent, ".aira-"+id)
}

func mkdirReaperTestCgroup(t *testing.T, parent, name string) string {
	t.Helper()
	path := filepath.Join(parent, name)
	if err := os.Mkdir(path, 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

func startReaperTestSleeper(t *testing.T, cgroupPath string) *exec.Cmd {
	t.Helper()
	fd, err := os.OpenFile(cgroupPath, os.O_RDONLY|syscall.O_DIRECTORY|syscall.O_CLOEXEC, 0)
	if err != nil {
		t.Fatal(err)
	}
	command := exec.Command("/bin/sleep", "60")
	command.SysProcAttr = &syscall.SysProcAttr{UseCgroupFD: true, CgroupFD: int(fd.Fd())}
	if err := command.Start(); err != nil {
		_ = fd.Close()
		cgrouptest.SkipOrFailRealCgroup(t, "start real reaper workload: %v", err)
	}
	if err := fd.Close(); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(testdeadline.Wait(time.Second))
	for {
		data, readErr := os.ReadFile(filepath.Join(cgroupPath, "cgroup.events"))
		if readErr == nil && strings.Contains(string(data), "populated 1") {
			break
		}
		if time.Now().After(deadline) {
			stopReaperTestSleeper(command)
			t.Fatalf("workload never populated %s: events=%q err=%v", cgroupPath, data, readErr)
		}
		time.Sleep(10 * time.Millisecond)
	}
	return command
}

func stopReaperTestSleeper(command *exec.Cmd) {
	if command == nil || command.Process == nil {
		return
	}
	_ = command.Process.Kill()
	_ = command.Wait()
}

func reaperTestDead(deadPID int) func(int) bool {
	return func(pid int) bool { return pid == deadPID }
}

func assertReaperTestMissing(t *testing.T, path string) {
	t.Helper()
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("path still exists %s: %v", path, err)
	}
}

type confineBackendFunc struct {
	create func(context.Context, string) (Scope, error)
}

func (confineBackendFunc) Probe(context.Context) error { return nil }
func (b confineBackendFunc) Create(ctx context.Context, id string) (Scope, error) {
	return b.create(ctx, id)
}
func (confineBackendFunc) Open(context.Context, string) (Scope, error) {
	return nil, errors.New("unused")
}

func confineContainsString(values []string, wanted string) bool {
	for _, value := range values {
		if value == wanted {
			return true
		}
	}
	return false
}

// AIRA-135 (a)+(b). The scan reads the supervisor's own argv live from /proc and
// splits it at the `--` separator, and reports the field as UNEVALUATED — never
// as a fabricated placeholder — when that read cannot be made.
//
// The populated case runs against a REAL process through the REAL /proc reader
// (no seam), so the production read itself is exercised and not just the parser
// behind it. The failure case goes through the seam, because "the supervisor
// exited under the read" cannot be staged deterministically with a real PID.
//
// verifies: AIRA-135
func TestConfineScanReadsTheSupervisorCommandAndSaysUnevaluatedWhenItCannot(t *testing.T) {
	slice := t.TempDir()
	now := time.Now()

	// A real child whose argv carries the `aira confine ... -- <job>` shape. The
	// body is a COMPOUND command on purpose: a shell handed a single simple
	// command implicitly execs it, replacing the very argv this test reads.
	live := exec.Command("/bin/sh", "-c", "while :; do sleep 1; done", "--", "held-job", "--flag-of-the-job")
	if err := live.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = live.Process.Kill()
		_, _ = live.Process.Wait()
	})
	livePID := live.Process.Pid
	liveScope := confineTestScopeID("live-command", livePID, now.Add(-time.Minute).UnixNano())
	writeConfineTestScope(t, slice, liveScope, strconv.Itoa(livePID)+"\n")

	result, err := ListConfines(context.Background(), slice, nil)
	if err != nil || result.Verdict != "pass" || len(result.Scopes) != 1 {
		t.Fatalf("result=%+v err=%v", result, err)
	}
	record := result.Scopes[0]
	if record.Command == nil {
		t.Fatalf("live supervisor record=%+v, want a command read from /proc", record)
	}
	// Everything after the FIRST bare `--` and nothing before it: the shell's own
	// flags stand in for aira's, and an implementation that returned the whole
	// cmdline fails here.
	if want := "held-job --flag-of-the-job"; *record.Command != want {
		t.Fatalf("command=%q, want %q", *record.Command, want)
	}
	if confineContainsString(record.UnevaluatedFields, "command") {
		t.Fatalf("an established command was also named unevaluated: %+v", record)
	}

	// The same scope, with the supervisor no longer readable. Every other facet
	// still reads, so this pins that a failed argv read costs the command field
	// and nothing else -- and costs it as a NAMED absence.
	failed, err := listConfinesWithDeps(context.Background(), slice, nil, confineScanDeps{
		now: time.Now, readField: readConfineScopeField, waitEmpty: waitEmpty,
		readCmdline: func(int) ([]byte, error) { return nil, errors.New("no such process") },
	})
	if err != nil || len(failed.Scopes) != 1 {
		t.Fatalf("failed-read result=%+v err=%v", failed, err)
	}
	dead := failed.Scopes[0]
	if dead.Command != nil {
		t.Fatalf("unreadable supervisor record=%+v, want a nil command", dead)
	}
	if !confineContainsString(dead.UnevaluatedFields, "command") {
		t.Fatalf("unreadable command was not named unevaluated: %+v", dead)
	}
	if dead.RSSBytes == nil || dead.Cap == nil {
		t.Fatalf("a failed argv read also lost a cgroup facet: %+v", dead)
	}

	// A supervisor whose cmdline is readable but establishes NOTHING (a `--` with
	// no command after it) is unevaluated too, never an empty command cell.
	empty, err := listConfinesWithDeps(context.Background(), slice, nil, confineScanDeps{
		now: time.Now, readField: readConfineScopeField, waitEmpty: waitEmpty,
		readCmdline: func(int) ([]byte, error) { return []byte("aira\x00confine\x00--\x00"), nil },
	})
	if err != nil || len(empty.Scopes) != 1 {
		t.Fatalf("empty-argv result=%+v err=%v", empty, err)
	}
	if record := empty.Scopes[0]; record.Command != nil || !confineContainsString(record.UnevaluatedFields, "command") {
		t.Fatalf("empty-argv record=%+v, want a nil command named unevaluated", record)
	}
}

// A registry-only (Pending) row performs no live read at all, so its command is
// named unevaluated rather than left silently absent. This is the row shape the
// ci-shim daemon builds its ENTIRE listing from.
//
// verifies: AIRA-135
func TestConfinePendingRowNamesTheCommandUnevaluated(t *testing.T) {
	pending := confineTestOwnedScopeID("pending-command", "session-a", 4901, time.Now().UnixNano())
	listed := ShimConfineList([]ConfineRegistryEntry{{ScopeID: pending}})
	if len(listed.Scopes) != 1 {
		t.Fatalf("listed=%+v", listed)
	}
	record := listed.Scopes[0]
	if !record.Pending || record.Command != nil || !confineContainsString(record.UnevaluatedFields, "command") {
		t.Fatalf("pending record=%+v, want a nil command named unevaluated", record)
	}
}
