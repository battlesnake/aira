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
)

func confineTestScopeID(name string, pid int, stamp int64) string {
	return "CONFINE-" + name + "-" + strconv.Itoa(pid) + "-" + strconv.FormatInt(stamp, 36)
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
	owned := confineTestScopeID("build-name.with-dash", 4101, now.Add(-time.Minute).UnixNano())
	scanOnly := confineTestScopeID("fallback", 4102, now.Add(-2*time.Minute).UnixNano())
	pending := confineTestScopeID("pending", 4103, now.UnixNano())
	writeConfineTestScope(t, slice, owned, "51\n52\n")
	writeConfineTestScope(t, slice, scanOnly, "61\n")
	regular := confineTestScopeID("not-a-directory", 4104, now.UnixNano())
	if err := os.WriteFile(filepath.Join(slice, ".aira-"+regular), []byte("not a cgroup"), 0o644); err != nil {
		t.Fatal(err)
	}

	result, err := listConfinesWithDeps(context.Background(), slice, []ConfineRegistryEntry{{ScopeID: owned, Name: "build-name.with-dash", Owner: "session-a"}, {ScopeID: pending, Name: "pending", Owner: "session-a"}}, confineScanDeps{
		now: time.Now, readField: readConfineScopeField, waitEmpty: waitEmpty,
	})
	if err != nil || result.Verdict != "pass" || len(result.Scopes) != 3 {
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
	if kill, killErr := KillConfine(context.Background(), missing, "job", "owner", true, nil, nil); killErr == nil || !strings.HasPrefix(killErr.Error(), CodeConfineKillUnconfirmed+":") || kill.Status == "killed" {
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

func TestConfineKillOwnershipGuardStealAndFreshLookup(t *testing.T) {
	slice := t.TempDir()
	scopeID := confineTestScopeID("owned", 4301, time.Now().UnixNano())
	path := writeConfineTestScope(t, slice, scopeID, "71\n")
	registry := []ConfineRegistryEntry{{ScopeID: scopeID, Name: "owned", Owner: "stale-owner"}}
	freshCalls := 0
	lookup := func(string) (string, bool) { freshCalls++; return "session-a", true }
	deps := defaultConfineScanDeps()
	deps.waitEmpty = func(_ context.Context, scope Scope, _ time.Duration) error {
		return os.WriteFile(filepath.Join(scope.Reference(), "cgroup.events"), []byte("populated 0\n"), 0o644)
	}
	if _, err := killConfineWithDeps(context.Background(), slice, scopeID, "session-b", false, registry, lookup, time.Second, deps); err == nil || !strings.HasPrefix(err.Error(), CodeConfineOwnerUnverified+":") {
		t.Fatalf("foreign kill err=%v", err)
	}
	if data, _ := os.ReadFile(filepath.Join(path, "cgroup.kill")); len(data) != 0 {
		t.Fatalf("ownership refusal wrote kill=%q", data)
	}
	result, err := killConfineWithDeps(context.Background(), slice, scopeID, "session-b", true, registry, lookup, time.Second, deps)
	if err != nil || result.Status != "killed" || freshCalls != 2 {
		t.Fatalf("result=%+v err=%v freshCalls=%d", result, err, freshCalls)
	}
	if data, _ := os.ReadFile(filepath.Join(path, "cgroup.kill")); string(data) != "1" {
		t.Fatalf("kill file=%q", data)
	}
}

func TestConfineKillUsesFreshRegistryOwnerNotListSnapshot(t *testing.T) {
	slice := t.TempDir()
	scopeID := confineTestScopeID("fresh", 4302, time.Now().UnixNano())
	writeConfineTestScope(t, slice, scopeID, "72\n")
	deps := defaultConfineScanDeps()
	deps.waitEmpty = func(_ context.Context, scope Scope, _ time.Duration) error {
		return os.WriteFile(filepath.Join(scope.Reference(), "cgroup.events"), []byte("populated 0\n"), 0o644)
	}
	result, err := killConfineWithDeps(context.Background(), slice, scopeID, "session-a", false,
		[]ConfineRegistryEntry{{ScopeID: scopeID, Name: "fresh", Owner: "stale-owner"}},
		func(string) (string, bool) { return "session-a", true }, time.Second, deps)
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
	id := confineTestScopeID("nested", 4601, time.Now().UnixNano())
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
	result, err := killConfineWithDeps(context.Background(), slice, id, "owner", false, []ConfineRegistryEntry{{ScopeID: id, Name: "nested", Owner: "owner"}}, func(string) (string, bool) { return "owner", true }, time.Second, deps)
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
		result, err := KillConfine(context.Background(), slice, id, caller, false, nil, nil)
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
		result, err := killConfineWithDeps(context.Background(), slice, id, ConfineUnknownOwner, true, nil, nil, time.Second, deps)
		if err != nil || result.Status != "killed" {
			t.Fatalf("scan-only steal result=%+v err=%v", result, err)
		}
	})
}

func TestConfineKillEmptyAndUnconfirmedNeverReportKilled(t *testing.T) {
	t.Run("empty-not-launched", func(t *testing.T) {
		slice := t.TempDir()
		id := confineTestScopeID("pending", 4401, time.Now().UnixNano())
		path := writeConfineTestScope(t, slice, id, "")
		result, err := KillConfine(context.Background(), slice, id, "owner", false, []ConfineRegistryEntry{{ScopeID: id, Name: "pending", Owner: "owner"}}, func(string) (string, bool) { return "owner", true })
		if err == nil || !strings.HasPrefix(err.Error(), CodeConfineNotLaunched+":") || result.Status == "killed" {
			t.Fatalf("result=%+v err=%v", result, err)
		}
		if data, _ := os.ReadFile(filepath.Join(path, "cgroup.kill")); len(data) != 0 {
			t.Fatalf("empty scope kill=%q", data)
		}
	})
	t.Run("write-error", func(t *testing.T) {
		slice := t.TempDir()
		id := confineTestScopeID("writefail", 4402, time.Now().UnixNano())
		path := writeConfineTestScope(t, slice, id, "81\n")
		if err := os.Remove(filepath.Join(path, "cgroup.kill")); err != nil {
			t.Fatal(err)
		}
		if err := os.Mkdir(filepath.Join(path, "cgroup.kill"), 0o755); err != nil {
			t.Fatal(err)
		}
		result, err := KillConfine(context.Background(), slice, id, "owner", false, []ConfineRegistryEntry{{ScopeID: id, Name: "writefail", Owner: "owner"}}, func(string) (string, bool) { return "owner", true })
		if err == nil || !strings.HasPrefix(err.Error(), CodeConfineKillUnconfirmed+":") || result.Status == "killed" {
			t.Fatalf("result=%+v err=%v", result, err)
		}
	})
	t.Run("wait-timeout", func(t *testing.T) {
		slice := t.TempDir()
		id := confineTestScopeID("timeout", 4403, time.Now().UnixNano())
		writeConfineTestScope(t, slice, id, "91\n")
		deps := defaultConfineScanDeps()
		deps.waitEmpty = func(context.Context, Scope, time.Duration) error { return errors.New("timeout") }
		result, err := killConfineWithDeps(context.Background(), slice, id, "owner", false, []ConfineRegistryEntry{{ScopeID: id, Name: "timeout", Owner: "owner"}}, func(string) (string, bool) { return "owner", true }, time.Millisecond, deps)
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
		if _, err := KillConfine(context.Background(), slice, selector, "owner", true, nil, nil); err == nil || !strings.HasPrefix(err.Error(), "E_SELECTOR_AMBIGUOUS:") {
			t.Fatalf("selector=%q err=%v", selector, err)
		}
	}
	if _, err := KillConfine(context.Background(), slice, "missing", "owner", true, nil, nil); err == nil || !strings.HasPrefix(err.Error(), CodeConfineNotFound+":") {
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
		result, killErr := KillConfine(context.Background(), slice, scopeID, "session-a", false, []ConfineRegistryEntry{{ScopeID: scopeID, Name: "midlaunch", Owner: "session-a"}}, func(string) (string, bool) { return "session-a", true })
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
	deadline := time.Now().Add(time.Second)
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
	result, err := KillConfine(context.Background(), parent, id, "session-b", true, nil, nil)
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
