//go:build linux

package runner

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"aira/internal/cgrouptest"
	"golang.org/x/sys/unix"
)

// verifies: task-57 a foreground cap-write failure is terminal and never starts.
func TestRunScopeMemoryCapFailureDoesNotLaunch(t *testing.T) {
	scope := &confineFakeScope{}
	r, err := New(Config{CommonDir: t.TempDir(), Backend: confineFakeBackend{scope: scope}})
	if err != nil {
		t.Fatal(err)
	}
	started := false
	r.startFn = func(*exec.Cmd) error {
		started = true
		return nil
	}
	r.writeScopeMemoryCapFn = func(Scope, int64, int64, bool) error {
		return errors.New("memory.max unavailable")
	}
	record, err := r.Launch(context.Background(), Request{Argv: []string{"/bin/true"}, NoAdmit: true, ScopeMemoryMax: 32 << 20})
	if err == nil || !strings.Contains(err.Error(), "E_RUN_CAP_UNAVAILABLE") {
		t.Fatalf("record=%+v err=%v", record, err)
	}
	if started {
		t.Fatal("target started after cap enforcement failure")
	}
	if !scope.removed {
		t.Fatal("empty failed scope was not removed")
	}
}

// verifies: task-57 detached cap failure uses the terminal pre-launch path and
// never calls Start.
func TestDetachedScopeMemoryCapFailureDoesNotLaunch(t *testing.T) {
	r, scope := newMemoryRunner(t, nil)
	r.reserveIDFn = func() (string, error) { return "RUN-cap-failure", nil }
	started := false
	r.startFn = func(*exec.Cmd) error {
		started = true
		return nil
	}
	r.writeScopeMemoryCapFn = func(_ Scope, maximum, high int64, setOOMGroup bool) error {
		if maximum != 32<<20 || high != 16<<20 || !setOOMGroup {
			t.Fatalf("cap args max=%d high=%d oom=%v", maximum, high, setOOMGroup)
		}
		return errors.New("memory.max unavailable")
	}
	readyR, readyW, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	ackR, ackW, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	req := Request{
		Argv: []string{"/bin/true"}, Detach: true, NoAdmit: true,
		ScopeMemoryMax: 32 << 20, ScopeMemoryHigh: 16 << 20,
	}
	done := make(chan error, 1)
	readyFD, err := unix.Dup(int(readyW.Fd()))
	if err != nil {
		t.Fatal(err)
	}
	ackFD, err := unix.Dup(int(ackR.Fd()))
	if err != nil {
		t.Fatal(err)
	}
	_ = readyW.Close()
	_ = ackR.Close()
	go func() {
		_, launchErr := r.SuperviseRequest(context.Background(), req, readyFD, ackFD)
		done <- launchErr
	}()
	var ready detachReadyMessage
	if err := json.NewDecoder(readyR).Decode(&ready); err != nil || ready.Code != "E_RUN_CAP_UNAVAILABLE" || ready.ID != "" {
		t.Fatalf("ready=%+v err=%v", ready, err)
	}
	_ = ackW.Close()
	if err := <-done; err == nil || !strings.Contains(err.Error(), "E_RUN_CAP_UNAVAILABLE") {
		t.Fatalf("error=%v", err)
	}
	if started {
		t.Fatal("detached target started after cap enforcement failure")
	}
	if !scope.removed {
		t.Fatal("detached failed scope was not removed")
	}
	record, err := r.Get("RUN-cap-failure")
	if err != nil {
		t.Fatal(err)
	}
	if !containsString(record.ErrorCodes, "E_RUN_CAP_UNAVAILABLE") || record.PIDIdentity.PID != 0 {
		t.Fatalf("terminal record=%+v", record)
	}
}

// verifies: task-57 no-cap foreground launches never touch the new writer seam.
func TestRunWithoutScopeMemoryCapDoesNotWriteCap(t *testing.T) {
	r, _ := newMemoryRunner(t, nil)
	called := false
	r.writeScopeMemoryCapFn = func(Scope, int64, int64, bool) error {
		called = true
		return errors.New("must not be called")
	}
	r.startFn = func(*exec.Cmd) error { return errors.New("stop after launch preparation") }
	_, _ = r.Launch(context.Background(), Request{Argv: []string{"/bin/true"}, NoAdmit: true})
	if called {
		t.Fatal("no-cap run called the scope memory writer")
	}
}

// covers: task-57 writer uses the scope FD and writes oom.group before a later
// memory.max failure.
func TestWriteScopeMemoryCapUsesScopeFDAndOOMGroupFirst(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "memory.oom.group"), []byte("0\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	opened, err := os.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer opened.Close()
	scope := &memoryCapFDScope{confineFakeScope: confineFakeScope{}, fd: int(opened.Fd()), reference: filepath.Join(t.TempDir(), "wrong-reference")}
	if err := writeScopeMemoryCap(scope, 32<<20, 0, true); err == nil || !strings.Contains(err.Error(), "memory.max") {
		t.Fatalf("missing memory.max error=%v", err)
	}
	data, err := os.ReadFile(filepath.Join(dir, "memory.oom.group"))
	if err != nil || strings.TrimSpace(string(data)) != "1" {
		t.Fatalf("oom.group data=%q err=%v", data, err)
	}
}

// verifies: task-57 max then high writes are read back through the scope FD.
func TestWriteScopeMemoryCapWritesAndVerifiesMaxAndHigh(t *testing.T) {
	dir := t.TempDir()
	for _, file := range []string{"memory.max", "memory.high"} {
		if err := os.WriteFile(filepath.Join(dir, file), nil, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	opened, err := os.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer opened.Close()
	scope := &memoryCapFDScope{confineFakeScope: confineFakeScope{}, fd: int(opened.Fd()), reference: filepath.Join(t.TempDir(), "wrong-reference")}
	if err := writeScopeMemoryCap(scope, 32<<20, 16<<20, false); err != nil {
		t.Fatal(err)
	}
	for file, want := range map[string]string{"memory.max": "33554432", "memory.high": "16777216"} {
		data, err := os.ReadFile(filepath.Join(dir, file))
		if err != nil || strings.TrimSpace(string(data)) != want {
			t.Fatalf("%s=%q err=%v want %s", file, data, err, want)
		}
	}
}

// verifies: task-57 a non-page-aligned request verifies against the kernel's
// 4 KiB-floored read-back instead of false-failing strict textual equality.
func TestRealWriteScopeMemoryCapPageTruncation(t *testing.T) {
	parent := writableMemoryParent(t, strconv.FormatInt(512<<20, 10))
	backend := newDefaultBackend(parent)
	if err := backend.Probe(context.Background()); err != nil {
		cgrouptest.SkipOrFailRealCgroup(t, "page-floor backend unavailable: %v", err)
	}
	scope, err := backend.Create(context.Background(), "page-floor")
	if err != nil {
		cgrouptest.SkipOrFailRealCgroup(t, "page-floor scope unavailable: %v", err)
	}
	defer scope.Remove()
	request := int64(1025 << 10)
	if err := writeScopeMemoryCap(scope, request, 0, true); err != nil {
		t.Fatalf("non-page-aligned cap false-failed: %v", err)
	}
	data, err := os.ReadFile(filepath.Join(scope.Reference(), "memory.max"))
	if err != nil || strings.TrimSpace(string(data)) != strconv.FormatInt(floorMemoryPage(request), 10) {
		t.Fatalf("memory.max=%q err=%v want %d", data, err, floorMemoryPage(request))
	}
}

type memoryCapFDScope struct {
	confineFakeScope
	fd        int
	reference string
}

func (scope *memoryCapFDScope) FD() int           { return scope.fd }
func (scope *memoryCapFDScope) Reference() string { return scope.reference }

// verifies: task-57 a scope-limited OOM kills the whole capped job while a
// sibling scope survives, and the kernel read-back is the 4 KiB floor.
func TestRealRunScopeMemoryCapIsolation(t *testing.T) {
	if _, err := exec.LookPath("python3"); err != nil {
		cgrouptest.SkipOrFailRealCgroup(t, "python3 is unavailable: %v", err)
	}
	parent := writableMemoryParent(t, strconv.FormatInt(512<<20, 10))
	if ancestor, finite := effectiveConfineCap(parent); !finite || ancestor <= 32<<20 {
		cgrouptest.SkipOrFailRealCgroup(t, "test ancestry cap=%d finite=%v is not strictly above 32MiB", ancestor, finite)
	}
	base := newDefaultBackend(parent)
	if err := base.Probe(context.Background()); err != nil {
		cgrouptest.SkipOrFailRealCgroup(t, "real capped backend unavailable: %v", err)
	}
	sibling, err := base.Create(context.Background(), "cap-sibling")
	if err != nil {
		cgrouptest.SkipOrFailRealCgroup(t, "create sibling scope: %v", err)
	}
	siblingCmd := exec.Command("/bin/sleep", "10")
	siblingCmd.SysProcAttr = &syscall.SysProcAttr{UseCgroupFD: true, CgroupFD: sibling.FD()}
	if err := siblingCmd.Start(); err != nil {
		_ = sibling.Remove()
		cgrouptest.SkipOrFailRealCgroup(t, "start sibling: %v", err)
	}
	defer func() {
		_ = sibling.Kill()
		_ = siblingCmd.Wait()
		_ = sibling.Remove()
	}()

	observation := &scopeMemoryObservation{}
	r, err := New(Config{CommonDir: t.TempDir(), Backend: &scopeMemoryObservingBackend{ScopeBackend: base, observation: observation}, Grace: time.Second, TermGrace: 100 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(t.TempDir(), "same-scope-survivor")
	record, err := r.Launch(context.Background(), Request{
		Argv:    []string{"/bin/sh", "-c", `(sleep 2; echo survived > "$1") & python3 -c 'x=bytearray(256*1024*1024); x[-1]=1'; wait`, "sh", marker},
		NoAdmit: true, ScopeMemoryMax: 32 << 20,
	})
	if err != nil {
		t.Fatal(err)
	}
	if record.Status != StatusOOMKilled || record.ScopeMemoryMax == nil || *record.ScopeMemoryMax != 32<<20 {
		t.Fatalf("capped OOM record=%+v", record)
	}
	if observation.maximum != strconv.FormatInt(32<<20, 10) || observation.oomGroup != "1" {
		t.Fatalf("scope read-back=%+v", observation)
	}
	time.Sleep(2200 * time.Millisecond)
	if _, err := os.Stat(marker); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("same-scope child survived group OOM: %v", err)
	}
	if err := siblingCmd.Process.Signal(syscall.Signal(0)); err != nil {
		t.Fatalf("sibling scope did not survive capped job OOM: %v", err)
	}
}

// verifies: task-57 confine reports ancestor-limited honestly when its verified
// scope cap is not below the effective parent cap.
func TestRealConfineScopeMemoryCapAncestorLimitedReporting(t *testing.T) {
	parent := confineMemoryParent(t, strconv.FormatInt(64<<20, 10))
	ancestor, finite := effectiveConfineCap(parent)
	if !finite {
		cgrouptest.SkipOrFailRealCgroup(t, "parent has no finite effective cap")
	}
	requestMax := ancestor + 16<<20
	result, err := Confine(context.Background(), ConfineRequest{
		Slice: parent, Argv: []string{"/bin/true"}, SelfPath: os.Args[0], Stderr: io.Discard,
		MemoryReserve: 1 << 20, ScopeMemoryMax: requestMax,
	})
	if err != nil {
		cgrouptest.SkipOrFailRealCgroup(t, "ancestor-limited confine: %v", err)
	}
	if result.Status.ScopeMemoryBinding != "ancestor-limited" || result.Status.ScopeMemoryEffective != ancestor || result.Status.ScopeMemoryMax != floorMemoryPage(requestMax) {
		t.Fatalf("ancestor-limited status=%+v ancestor=%d", result.Status, ancestor)
	}
}

// verifies: task-57 detached supervision writes and records the cap before the
// child starts running.
func TestRealDetachedScopeMemoryCap(t *testing.T) {
	parent := writableMemoryParent(t, strconv.FormatInt(512<<20, 10))
	r, err := New(Config{CommonDir: t.TempDir(), CgroupParent: parent, Grace: 100 * time.Millisecond, TermGrace: 50 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	if err := r.Probe(context.Background()); err != nil {
		cgrouptest.SkipOrFailRealCgroup(t, "detached capped backend unavailable: %v", err)
	}
	id, outcome := startRealDetached(t, r, Request{Argv: []string{"/bin/sleep", "10"}, NoAdmit: true, ScopeMemoryMax: 32 << 20})
	running := waitForRunState(t, r, id, func(record RunRecord) bool { return record.Status == StatusRunning })
	if running.ScopeMemoryMax == nil || *running.ScopeMemoryMax != 32<<20 {
		t.Fatalf("detached running record=%+v", running)
	}
	for file, want := range map[string]string{"memory.max": strconv.FormatInt(32<<20, 10), "memory.oom.group": "1"} {
		data, err := os.ReadFile(filepath.Join(running.CgroupScope, file))
		if err != nil || strings.TrimSpace(string(data)) != want {
			t.Fatalf("detached %s=%q err=%v want %s", file, data, err, want)
		}
	}
	if _, err := r.Kill(context.Background(), id, false); err != nil && !strings.Contains(err.Error(), "U_RUN_RECONCILE_REQUIRED") {
		t.Fatal(err)
	}
	result := <-outcome
	if result.err != nil && !strings.Contains(result.err.Error(), "U_RUN_RECONCILE_REQUIRED") {
		t.Fatal(result.err)
	}
}

// verifies: task-57 missing +memory delegation fails closed for confine,
// foreground run, and detached run; none can create its marker.
func TestRealScopeMemoryCapDelegationBehavior(t *testing.T) {
	newParent := func(t *testing.T) string {
		t.Helper()
		parent := cgrouptest.IsolatedScopeParent(t)
		if err := os.WriteFile(filepath.Join(parent, "memory.max"), []byte(strconv.FormatInt(512<<20, 10)), 0o644); err != nil {
			cgrouptest.SkipOrFailRealCgroup(t, "parent memory.max unavailable: %v", err)
		}
		return parent
	}
	t.Run("confine repairs delegation", func(t *testing.T) {
		marker := filepath.Join(t.TempDir(), "ran")
		result, err := Confine(context.Background(), ConfineRequest{
			Slice: newParent(t), MemoryReserve: 1, ScopeMemoryMax: 32 << 20,
			Argv: []string{"/bin/sh", "-c", `: > "$1"`, "sh", marker}, SelfPath: os.Args[0], Stderr: io.Discard,
		})
		if err != nil || result.Exit != 0 {
			t.Fatalf("result=%+v error=%v", result, err)
		}
		if _, statErr := os.Stat(marker); statErr != nil {
			t.Fatalf("target did not run: %v", statErr)
		}
	})
	t.Run("foreground", func(t *testing.T) {
		marker := filepath.Join(t.TempDir(), "ran")
		r, err := New(Config{CommonDir: t.TempDir(), CgroupParent: newParent(t)})
		if err != nil {
			t.Fatal(err)
		}
		_, err = r.Launch(context.Background(), Request{Argv: []string{"/bin/sh", "-c", `: > "$1"`, "sh", marker}, NoAdmit: true, ScopeMemoryMax: 32 << 20})
		if err == nil || !strings.Contains(err.Error(), "E_RUN_CAP_UNAVAILABLE") {
			t.Fatalf("error=%v", err)
		}
		assertMarkerAbsent(t, marker)
	})
	t.Run("detached", func(t *testing.T) {
		marker := filepath.Join(t.TempDir(), "ran")
		r, err := New(Config{CommonDir: t.TempDir(), CgroupParent: newParent(t)})
		if err != nil {
			t.Fatal(err)
		}
		readyR, readyW, pipeErr := os.Pipe()
		if pipeErr != nil {
			t.Fatal(pipeErr)
		}
		ackR, ackW, pipeErr := os.Pipe()
		if pipeErr != nil {
			t.Fatal(pipeErr)
		}
		done := make(chan error, 1)
		readyFD, dupErr := unix.Dup(int(readyW.Fd()))
		if dupErr != nil {
			t.Fatal(dupErr)
		}
		ackFD, dupErr := unix.Dup(int(ackR.Fd()))
		if dupErr != nil {
			t.Fatal(dupErr)
		}
		_ = readyW.Close()
		_ = ackR.Close()
		go func() {
			_, superviseErr := r.SuperviseRequest(context.Background(), Request{Argv: []string{"/bin/sh", "-c", `: > "$1"`, "sh", marker}, NoAdmit: true, ScopeMemoryMax: 32 << 20}, readyFD, ackFD)
			done <- superviseErr
		}()
		var ready detachReadyMessage
		if err := json.NewDecoder(readyR).Decode(&ready); err != nil || ready.Code != "E_RUN_CAP_UNAVAILABLE" || ready.ID != "" {
			t.Fatalf("ready=%+v err=%v", ready, err)
		}
		_ = ackW.Close()
		if err := <-done; err == nil || !strings.Contains(err.Error(), "E_RUN_CAP_UNAVAILABLE") {
			t.Fatalf("error=%v", err)
		}
		assertMarkerAbsent(t, marker)
	})
}

// verifies: task-57 an uncapped run leaves memory.oom.group at its default and
// omits both cap fields from the record.
func TestRealRunWithoutScopeMemoryCapLeavesOOMGroupUnset(t *testing.T) {
	parent := writableMemoryParent(t, strconv.FormatInt(512<<20, 10))
	observation := &scopeMemoryObservation{}
	r, err := New(Config{CommonDir: t.TempDir(), Backend: &scopeMemoryObservingBackend{ScopeBackend: newDefaultBackend(parent), observation: observation}})
	if err != nil {
		t.Fatal(err)
	}
	record, err := r.Launch(context.Background(), Request{Argv: []string{"/bin/true"}, NoAdmit: true})
	if err != nil {
		cgrouptest.SkipOrFailRealCgroup(t, "uncapped real run: %v", err)
	}
	if observation.oomGroup != "0" || record.ScopeMemoryMax != nil || record.ScopeMemoryHigh != nil {
		t.Fatalf("observation=%+v record=%+v", observation, record)
	}
}

func assertMarkerAbsent(t *testing.T, marker string) {
	t.Helper()
	if _, err := os.Stat(marker); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("target marker exists: %v", err)
	}
}

type scopeMemoryObservation struct {
	maximum  string
	high     string
	oomGroup string
}

type scopeMemoryObservingBackend struct {
	ScopeBackend
	observation *scopeMemoryObservation
}

func (backend *scopeMemoryObservingBackend) Create(ctx context.Context, id string) (Scope, error) {
	scope, err := backend.ScopeBackend.Create(ctx, id)
	if err != nil {
		return nil, err
	}
	return &scopeMemoryObservingScope{Scope: scope, observation: backend.observation}, nil
}

type scopeMemoryObservingScope struct {
	Scope
	observation *scopeMemoryObservation
}

func (scope *scopeMemoryObservingScope) Remove() error {
	read := func(name string) string {
		data, _ := os.ReadFile(filepath.Join(scope.Reference(), name))
		return strings.TrimSpace(string(data))
	}
	scope.observation.maximum = read("memory.max")
	scope.observation.high = read("memory.high")
	scope.observation.oomGroup = read("memory.oom.group")
	return scope.Scope.Remove()
}
