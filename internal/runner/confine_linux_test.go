//go:build linux

package runner

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"aira/internal/cgrouptest"
)

func TestResolveConfineSlicePrecedence(t *testing.T) {
	t.Setenv("AIRA_CONFINE_SLICE", "environment.slice")
	for _, test := range []struct {
		name string
		flag string
		want string
	}{
		{name: "flag", flag: "flag.slice", want: "flag.slice"},
		{name: "environment", want: "environment.slice"},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := ResolveConfineSlice(test.flag); got != test.want {
				t.Fatalf("ResolveConfineSlice(%q)=%q, want %q", test.flag, got, test.want)
			}
		})
	}
	t.Setenv("AIRA_CONFINE_SLICE", "")
	if got := ResolveConfineSlice(""); got != "whale.slice" {
		t.Fatalf("default slice=%q, want whale.slice", got)
	}
}

func TestParseConfineHandshakeRequiresCompleteSuccessfulResult(t *testing.T) {
	valid := []byte(`{"schema":1,"oom_score_adj":true,"nice":true,"ionice":true}` + "\n")
	if got, ok := parseConfineHandshake(valid); !ok || !got.applied() {
		t.Fatalf("valid handshake=%+v ok=%v", got, ok)
	}
	for name, payload := range map[string][]byte{
		"empty":          nil,
		"eof-no-newline": valid[:len(valid)-1],
		"partial":        valid[:len(valid)/2],
		"malformed":      []byte("not-json\n"),
		"trailing":       append(append([]byte(nil), valid...), 'x'),
		"failed-knob":    []byte(`{"schema":1,"oom_score_adj":true,"nice":false,"ionice":true}` + "\n"),
		"wrong-schema":   []byte(`{"schema":2,"oom_score_adj":true,"nice":true,"ionice":true}` + "\n"),
	} {
		t.Run(name, func(t *testing.T) {
			if result, ok := parseConfineHandshake(payload); ok || result.applied() {
				t.Fatalf("handshake %q unexpectedly verified: %+v ok=%v", payload, result, ok)
			}
		})
	}
}

func TestFormatConfineStatusReportsIndependentFacets(t *testing.T) {
	status := ConfineStatus{
		Slice:        "whale.slice",
		Cap:          ConfineCapEnforced,
		Admission:    ConfineAdmissionAdmitted,
		CapBytes:     64 << 30,
		ReserveBytes: 4 << 30,
		Scope:        ConfineScopePlaced,
		OOMGroup:     ConfineOOMGroupSet,
		Priorities:   ConfinePrioritiesUnverified,
	}
	line := FormatConfineStatus(status)
	for _, want := range []string{
		"slice=whale.slice", "cap=enforced", "reserve=4G", "scope=placed",
		"oom.group=set", "priorities=unverified",
	} {
		if !strings.Contains(line, want) {
			t.Fatalf("status %q lacks %q", line, want)
		}
	}
	if strings.Contains(line, "priorities=applied") {
		t.Fatalf("mixed status falsely claims priorities: %q", line)
	}

	unevaluated := FormatConfineStatus(ConfineStatus{
		Slice: "uncapped.slice", Admission: ConfineAdmissionUnevaluated,
		Scope: ConfineScopePlaced, OOMGroup: ConfineOOMGroupSet,
		Priorities: ConfinePrioritiesApplied,
	})
	if !strings.Contains(unevaluated, "cap=unevaluated") || strings.Contains(unevaluated, "cap=enforced") {
		t.Fatalf("uncapped status dishonest: %q", unevaluated)
	}
}

func TestConfineProbeFailureDoesNotLaunch(t *testing.T) {
	started := false
	deps := confineDeps{
		resolveSlicePath: func(string) (string, bool, string) { return "/cg/missing.slice", true, "" },
		newBackend:       func(string) ScopeBackend { return confineUnavailableBackend{} },
		start: func(*confineCommand) error {
			started = true
			return nil
		},
	}
	_, err := confineWithDeps(context.Background(), ConfineRequest{
		Slice: "missing.slice", Argv: []string{"must-not-run"},
		Stdin: strings.NewReader(""), Stdout: io.Discard, Stderr: io.Discard,
	}, deps)
	if err == nil || !strings.Contains(err.Error(), "E_CONFINE_UNAVAILABLE: slice missing.slice") {
		t.Fatalf("error=%v", err)
	}
	if started {
		t.Fatal("target launch reached after confinement probe failed")
	}
}

func TestConfineMissingSliceDoesNotLaunch(t *testing.T) {
	started := false
	deps := defaultConfineDeps()
	deps.resolveSlicePath = func(string) (string, bool, string) { return "", false, "slice-not-found" }
	deps.start = func(*confineCommand) error {
		started = true
		return errors.New("stop after detecting an unconfined launch attempt")
	}
	_, err := confineWithDeps(context.Background(), ConfineRequest{
		Slice: "absent.slice", Argv: []string{"must-not-run"}, Stderr: io.Discard,
	}, deps)
	if err == nil || !strings.Contains(err.Error(), "E_CONFINE_UNAVAILABLE: slice absent.slice") {
		t.Fatalf("error=%v", err)
	}
	if started {
		t.Fatal("target launch reached for absent slice")
	}
}

func TestConfineOOMGroupFailureDoesNotLaunch(t *testing.T) {
	scope := &confineFakeScope{}
	started := false
	deps := confineUnitDeps(scope)
	deps.writeOOMGroup = func(Scope) error { return errors.New("memory controller unavailable") }
	deps.start = func(*confineCommand) error {
		started = true
		return errors.New("stop after detecting an unconfined launch attempt")
	}
	_, err := confineWithDeps(context.Background(), ConfineRequest{
		Slice: "nodeleg.slice", Argv: []string{"must-not-run"}, Stderr: io.Discard,
	}, deps)
	if err == nil || !strings.Contains(err.Error(), "E_CONFINE_UNAVAILABLE: slice nodeleg.slice") || !strings.Contains(err.Error(), "memory.oom.group") {
		t.Fatalf("error=%v", err)
	}
	if started {
		t.Fatal("target launch reached after memory.oom.group failed")
	}
}

func TestConfineMembershipFailureDoesNotReleaseTarget(t *testing.T) {
	for _, test := range []struct {
		name       string
		membersErr error
		omitPID    bool
	}{
		{name: "members-error", membersErr: errors.New("membership unavailable")},
		{name: "pid-absent", omitPID: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			marker := filepath.Join(t.TempDir(), "ran")
			scope := &confineFakeScope{membersErr: test.membersErr, omitPID: test.omitPID}
			_, err := confineWithDeps(context.Background(), ConfineRequest{
				Slice:    "finite.slice",
				Argv:     []string{"/bin/sh", "-c", "echo ran > \"$1\"", "sh", marker},
				SelfPath: os.Args[0], Stderr: io.Discard,
			}, confineUnitDeps(scope))
			if _, statErr := os.Stat(marker); !errors.Is(statErr, os.ErrNotExist) {
				t.Fatalf("unverified target ran: marker err=%v", statErr)
			}
			if err == nil || !strings.Contains(err.Error(), "E_CONFINE_UNAVAILABLE") {
				t.Fatalf("error=%v", err)
			}
		})
	}
}

func TestConfineCreateFailureDoesNotLaunchMarker(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "ran")
	started := false
	deps := confineUnitDeps(&confineFakeScope{})
	deps.newBackend = func(string) ScopeBackend { return confineCreateFailBackend{} }
	deps.start = func(*confineCommand) error {
		started = true
		return nil
	}
	_, err := confineWithDeps(context.Background(), ConfineRequest{
		Slice:    "finite.slice",
		Argv:     []string{"/bin/sh", "-c", "echo ran > \"$1\"", "sh", marker},
		SelfPath: os.Args[0], Stderr: io.Discard,
	}, deps)
	if err == nil || !strings.Contains(err.Error(), "E_CONFINE_UNAVAILABLE") {
		t.Fatalf("error=%v", err)
	}
	if started {
		t.Fatal("start reached after Create failure")
	}
	if _, statErr := os.Stat(marker); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("target ran after Create failure: marker err=%v", statErr)
	}
}

func TestConfineSignalHandlerInstalledBeforeStartCleansScope(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "ran")
	scope := &confineFakeScope{}
	deps := confineUnitDeps(scope)
	signals := make(chan os.Signal, 1)
	deps.signalSource = func() (<-chan os.Signal, func()) { return signals, func() {} }
	start := deps.start
	deps.start = func(command *confineCommand) error {
		if err := start(command); err != nil {
			return err
		}
		signals <- syscall.SIGTERM
		deadline := time.Now().Add(time.Second)
		for {
			scope.mu.Lock()
			cleaned := scope.killed && scope.removed
			scope.mu.Unlock()
			if cleaned {
				return nil
			}
			if time.Now().After(deadline) {
				return errors.New("signal handler was not active during Start")
			}
			time.Sleep(time.Millisecond)
		}
	}
	_, err := confineWithDeps(context.Background(), ConfineRequest{
		Slice:    "finite.slice",
		Argv:     []string{"/bin/sh", "-c", "echo ran > \"$1\"", "sh", marker},
		SelfPath: os.Args[0], Stderr: io.Discard,
	}, deps)
	if err == nil || !strings.Contains(err.Error(), "E_CONFINE_UNAVAILABLE") {
		t.Fatalf("error=%v", err)
	}
	scope.mu.Lock()
	killed, removed := scope.killed, scope.removed
	scope.mu.Unlock()
	if !killed || !removed {
		t.Fatalf("scope cleanup killed=%v removed=%v", killed, removed)
	}
	if _, statErr := os.Stat(marker); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("target ran across pre-release signal: marker err=%v", statErr)
	}
}

func TestConfineAdmissionReleaseExactlyOnce(t *testing.T) {
	for _, test := range []struct {
		name       string
		createFail bool
	}{
		{name: "pre-start-error", createFail: true},
		{name: "successful-start"},
	} {
		t.Run(test.name, func(t *testing.T) {
			closer := &confineCountingCloser{}
			scope := &confineFakeScope{}
			deps := confineUnitDeps(scope)
			deps.admit = func(context.Context, string, ConfineRequest, int64) (admissionResult, error) {
				return admissionResult{state: "immediate", release: closer}, nil
			}
			if test.createFail {
				deps.newBackend = func(string) ScopeBackend { return confineCreateFailBackend{} }
			}
			_, _ = confineWithDeps(context.Background(), ConfineRequest{
				Slice: "finite.slice", Argv: []string{"/bin/true"}, SelfPath: os.Args[0], Stderr: io.Discard,
			}, deps)
			if closer.count != 1 {
				t.Fatalf("admission release count=%d, want 1", closer.count)
			}
		})
	}
}

func TestConfineFiniteCapFacetIsIndependentOfAdmissionTimeout(t *testing.T) {
	path := t.TempDir()
	if err := os.WriteFile(filepath.Join(path, "memory.current"), []byte("0\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(path, "memory.max"), []byte(strconv.FormatInt(64<<30, 10)+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	scope := &confineFakeScope{}
	deps := confineUnitDeps(scope)
	deps.resolveSlicePath = func(string) (string, bool, string) { return path, true, "" }
	deps.readCap = readConfineCap
	deps.admit = func(context.Context, string, ConfineRequest, int64) (admissionResult, error) {
		return admissionResult{state: "timeout"}, nil
	}
	var stderr bytes.Buffer
	result, err := confineWithDeps(context.Background(), ConfineRequest{
		Slice: "finite.slice", Argv: []string{"/bin/true"}, SelfPath: os.Args[0], Stderr: &stderr,
	}, deps)
	if err != nil || result.Exit != 0 {
		t.Fatalf("result=%+v err=%v", result, err)
	}
	if !strings.Contains(stderr.String(), "cap=enforced(64G)") || !strings.Contains(stderr.String(), "admission=timeout") || strings.Contains(stderr.String(), "finite-not-admitted") {
		t.Fatalf("status=%q", stderr.String())
	}
}

// verifies: an uncapped slice (no finite effective memory.max) is refused before
// the target is ever started — never launched with only a host-OOM backstop.
func TestConfineUncappedSliceRefusesLaunch(t *testing.T) {
	scope := &confineFakeScope{}
	deps := confineUnitDeps(scope)
	deps.admit = func(context.Context, string, ConfineRequest, int64) (admissionResult, error) {
		t.Fatalf("admit must not run for an uncapped slice")
		return admissionResult{}, nil
	}
	deps.readCap = func(string) (int64, bool) { return 0, false }
	var stderr bytes.Buffer
	result, err := confineWithDeps(context.Background(), ConfineRequest{
		Slice: "uncapped.slice", Argv: []string{"/bin/true"}, SelfPath: os.Args[0], Stderr: &stderr,
	}, deps)
	if err == nil || !strings.Contains(err.Error(), "E_CONFINE_UNAVAILABLE") {
		t.Fatalf("want E_CONFINE_UNAVAILABLE, got result=%+v err=%v", result, err)
	}
	scope.mu.Lock()
	started := scope.started
	scope.mu.Unlock()
	if started {
		t.Fatalf("target was started despite an uncapped slice")
	}
	if result.Status.Cap != ConfineCapUnevaluated {
		t.Fatalf("cap facet=%v, want unevaluated", result.Status.Cap)
	}
}

// verifies: the capped-ancestor safety check (used by the helper self-check and
// the parent gate) sees a finite memory.max anywhere in the ancestry, and reports
// uncapped only when no finite cap bounds the subtree.
func TestHasFiniteCapAncestorAndEffectiveMin(t *testing.T) {
	mount := t.TempDir()
	slice := filepath.Join(mount, "whale.slice")
	scope := filepath.Join(slice, ".aira-CONFINE-1")
	if err := os.MkdirAll(scope, 0o755); err != nil {
		t.Fatal(err)
	}
	writeMax := func(dir, v string) {
		if err := os.WriteFile(filepath.Join(dir, "memory.max"), []byte(v+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	// Uncapped everywhere: no finite cap bounds the leaf.
	writeMax(mount, "max")
	writeMax(slice, "max")
	writeMax(scope, "max")
	if hasFiniteCapAncestor(mount, scope) {
		t.Fatalf("uncapped ancestry reported as capped")
	}
	if _, ok := effectiveCapFrom(mount, scope); ok {
		t.Fatalf("effective cap found where none exists")
	}

	// A finite cap on the slice (an ancestor of the leaf) bounds the subtree.
	writeMax(slice, strconv.FormatInt(64<<30, 10))
	if !hasFiniteCapAncestor(mount, scope) {
		t.Fatalf("finite ancestor cap not detected")
	}

	// The effective ceiling is the MINIMUM finite cap across the ancestry.
	writeMax(scope, strconv.FormatInt(8<<30, 10))
	if got, ok := effectiveCapFrom(mount, scope); !ok || got != 8<<30 {
		t.Fatalf("effective cap=%d ok=%v, want min 8GiB", got, ok)
	}
}

func TestReadConfineCapDoesNotDependOnMemoryCurrent(t *testing.T) {
	path := t.TempDir()
	if err := os.WriteFile(filepath.Join(path, "memory.max"), []byte(strconv.FormatInt(64<<30, 10)+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	maximum, finite := readConfineCap(path)
	if !finite || maximum != 64<<30 {
		t.Fatalf("cap snapshot maximum=%d finite=%v", maximum, finite)
	}
}

func TestConfineAdmissionTimeoutStillLaunchesAndReportsFacetMix(t *testing.T) {
	scope := &confineFakeScope{}
	deps := confineUnitDeps(scope)
	deps.admit = func(context.Context, string, ConfineRequest, int64) (admissionResult, error) {
		return admissionResult{state: "timeout", waitedMS: 10}, nil
	}
	deps.readHandshake = func(*os.File, time.Duration) ([]byte, error) {
		return nil, errors.New("forced handshake failure")
	}
	var stderr bytes.Buffer
	result, err := confineWithDeps(context.Background(), ConfineRequest{
		Slice: "finite.slice", Argv: []string{"/bin/sh", "-c", "exit 23"},
		SelfPath: os.Args[0], Stderr: &stderr,
	}, deps)
	if err != nil {
		t.Fatal(err)
	}
	if result.Exit != 23 || result.Status.Admission != ConfineAdmissionTimeout || result.Status.Priorities != ConfinePrioritiesUnverified {
		t.Fatalf("result=%+v stderr=%q", result, stderr.String())
	}
	if !scope.started || !strings.Contains(stderr.String(), "cap=enforced") || !strings.Contains(stderr.String(), "admission=timeout") || !strings.Contains(stderr.String(), "priorities=unverified") || strings.Contains(stderr.String(), "priorities=applied") {
		t.Fatalf("timeout launch/status dishonest: scope=%+v stderr=%q", scope, stderr.String())
	}
}

func TestConfineMixedFiniteCapAndFailedPriorityResult(t *testing.T) {
	scope := &confineFakeScope{}
	deps := confineUnitDeps(scope)
	deps.readHandshake = func(*os.File, time.Duration) ([]byte, error) {
		return []byte(`{"schema":1,"oom_score_adj":true,"nice":false,"ionice":true}` + "\n"), nil
	}
	var stderr bytes.Buffer
	result, err := confineWithDeps(context.Background(), ConfineRequest{
		Slice: "finite.slice", Argv: []string{"/bin/true"}, SelfPath: os.Args[0], Stderr: &stderr,
	}, deps)
	if err != nil || result.Exit != 0 {
		t.Fatalf("result=%+v err=%v", result, err)
	}
	if result.Status.Admission != ConfineAdmissionAdmitted || result.Status.Priorities != ConfinePrioritiesUnverified {
		t.Fatalf("mixed facets=%+v", result.Status)
	}
	if !strings.Contains(stderr.String(), "cap=enforced") || !strings.Contains(stderr.String(), "priorities=unverified") || strings.Contains(stderr.String(), "priorities=applied") {
		t.Fatalf("mixed status=%q", stderr.String())
	}
}

func TestConfineHandshakeAppliesPrioritiesAndInheritsStdio(t *testing.T) {
	scope := &confineFakeScope{}
	deps := confineUnitDeps(scope)
	var stdout, stderr bytes.Buffer
	result, err := confineWithDeps(context.Background(), ConfineRequest{
		Slice: "finite.slice", Argv: []string{"/bin/sh", "-c", "read value; printf 'stdio:%s oom:' \"$value\"; cat /proc/self/oom_score_adj"},
		SelfPath: os.Args[0], Stdin: strings.NewReader("inherited\n"), Stdout: &stdout, Stderr: &stderr,
	}, deps)
	if err != nil {
		t.Fatal(err)
	}
	if result.Exit != 0 || result.Status.Scope != ConfineScopePlaced || result.Status.OOMGroup != ConfineOOMGroupSet || result.Status.Priorities != ConfinePrioritiesApplied {
		t.Fatalf("result=%+v stderr=%q", result, stderr.String())
	}
	if got := strings.TrimSpace(stdout.String()); got != "stdio:inherited oom:500" {
		t.Fatalf("stdout=%q", stdout.String())
	}
	if !strings.Contains(stderr.String(), "cap=enforced") || !strings.Contains(stderr.String(), "scope=placed") || !strings.Contains(stderr.String(), "priorities=applied") {
		t.Fatalf("status=%q", stderr.String())
	}
}

func TestConfineKillingSignalMapsToShellExit(t *testing.T) {
	scope := &confineFakeScope{}
	result, err := confineWithDeps(context.Background(), ConfineRequest{
		Slice: "finite.slice", Argv: []string{"/bin/sh", "-c", "kill -TERM $$"},
		SelfPath: os.Args[0], Stderr: io.Discard,
	}, confineUnitDeps(scope))
	if err != nil {
		t.Fatal(err)
	}
	if result.Exit != 128+15 {
		t.Fatalf("signal exit=%d, want 143", result.Exit)
	}
}

func TestConfineInjectsCPUGovernorEnvironment(t *testing.T) {
	runtimeDir := t.TempDir()
	scope := &confineFakeScope{}
	var stdout bytes.Buffer
	result, err := confineWithDeps(context.Background(), ConfineRequest{
		Slice: "finite.slice", Argv: []string{"/bin/sh", "-c", "printf '%s' \"$AIRA_CPU_SLOTS_DIR\""},
		RuntimeDir: runtimeDir, SelfPath: os.Args[0], Stdout: &stdout, Stderr: io.Discard,
	}, confineUnitDeps(scope))
	if err != nil || result.Exit != 0 {
		t.Fatalf("result=%+v err=%v", result, err)
	}
	if got, want := stdout.String(), runtimeDir+"/cpuslots"; got != want {
		t.Fatalf("AIRA_CPU_SLOTS_DIR=%q, want %q", got, want)
	}
}

func TestConfineWritesNoLedgerOrRunRecord(t *testing.T) {
	working := t.TempDir()
	t.Chdir(working)
	before, err := os.ReadDir(working)
	if err != nil {
		t.Fatal(err)
	}
	scope := &confineFakeScope{}
	result, err := confineWithDeps(context.Background(), ConfineRequest{
		Slice: "finite.slice", Argv: []string{"/bin/true"}, SelfPath: os.Args[0], Stderr: io.Discard,
	}, confineUnitDeps(scope))
	if err != nil || result.Exit != 0 {
		t.Fatalf("result=%+v err=%v", result, err)
	}
	after, err := os.ReadDir(working)
	if err != nil {
		t.Fatal(err)
	}
	if len(before) != 0 || len(after) != 0 {
		t.Fatalf("confine wrote project artifacts: before=%v after=%v", before, after)
	}
}

// verifies: the production launch path wires CLONE_INTO_CGROUP placement
// (SysProcAttr.UseCgroupFD + CgroupFD = the scope's FD) onto the command before it
// starts. This runs unconditionally (no real cgroup needed), so a regression that
// drops placement is caught even when the gated real-cgroup tests skip.
func TestConfineLaunchWiresCgroupPlacement(t *testing.T) {
	scope := &confineFakeScope{}
	deps := confineUnitDeps(scope)
	var placed bool
	inner := deps.start
	deps.start = func(command *confineCommand) error {
		sp := command.cmd.SysProcAttr
		placed = sp != nil && sp.UseCgroupFD && sp.CgroupFD == scope.FD()
		return inner(command)
	}
	if _, err := confineWithDeps(context.Background(), ConfineRequest{
		Slice: "finite.slice", Argv: []string{"/bin/true"}, SelfPath: os.Args[0], Stderr: io.Discard,
	}, deps); err != nil {
		t.Fatalf("confine: %v", err)
	}
	if !placed {
		t.Fatalf("production path did not set UseCgroupFD+CgroupFD=scope.FD() before start")
	}
}

func confineUnitDeps(scope *confineFakeScope) confineDeps {
	return confineDeps{
		resolveSlicePath: func(string) (string, bool, string) { return "/fake/finite.slice", true, "" },
		newBackend:       func(string) ScopeBackend { return confineFakeBackend{scope: scope} },
		admit: func(context.Context, string, ConfineRequest, int64) (admissionResult, error) {
			return admissionResult{state: "immediate"}, nil
		},
		writeOOMGroup: func(Scope) error { return nil },
		readCap: func(string) (int64, bool) {
			return 64 << 30, true
		},
		start: func(command *confineCommand) error {
			command.cmd.Args[1] = "__confine-test-setup"
			command.cmd.SysProcAttr = nil
			if err := command.Start(); err != nil {
				return err
			}
			scope.mu.Lock()
			scope.members = []int{command.cmd.Process.Pid}
			scope.started = true
			scope.mu.Unlock()
			return nil
		},
	}
}

type confineFakeBackend struct{ scope Scope }

func (confineFakeBackend) Probe(context.Context) error                     { return nil }
func (b confineFakeBackend) Create(context.Context, string) (Scope, error) { return b.scope, nil }
func (b confineFakeBackend) Open(context.Context, string) (Scope, error)   { return b.scope, nil }

type confineFakeScope struct {
	mu         sync.Mutex
	members    []int
	membersErr error
	omitPID    bool
	started    bool
	killed     bool
	removed    bool
}

func (*confineFakeScope) Reference() string  { return "/fake/scope" }
func (*confineFakeScope) FD() int            { return -1 }
func (*confineFakeScope) EventsPath() string { return "/fake/scope/cgroup.events" }
func (s *confineFakeScope) Members() ([]int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.membersErr != nil {
		return nil, s.membersErr
	}
	if s.omitPID {
		return nil, nil
	}
	return append([]int(nil), s.members...), nil
}
func (s *confineFakeScope) Empty() (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.members) == 0, nil
}
func (*confineFakeScope) Terminate([]int) error { return nil }
func (s *confineFakeScope) Kill() error {
	s.mu.Lock()
	s.members = nil
	s.killed = true
	s.mu.Unlock()
	return nil
}
func (s *confineFakeScope) Remove() error {
	s.mu.Lock()
	s.removed = true
	s.mu.Unlock()
	return nil
}

type confineCountingCloser struct{ count int }

func (closer *confineCountingCloser) Close() error {
	closer.count++
	return nil
}

type confineCreateFailBackend struct{}

func (confineCreateFailBackend) Probe(context.Context) error { return nil }
func (confineCreateFailBackend) Create(context.Context, string) (Scope, error) {
	return nil, errors.New("injected create failure")
}
func (confineCreateFailBackend) Open(context.Context, string) (Scope, error) {
	return nil, errors.New("injected open failure")
}

func TestWriteConfineOOMGroupVerifiesThroughScopeFD(t *testing.T) {
	fdDir := t.TempDir()
	referenceDir := t.TempDir()
	if err := os.Symlink("/dev/null", filepath.Join(fdDir, "memory.oom.group")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(referenceDir, "memory.oom.group"), []byte("1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	dir, err := os.Open(fdDir)
	if err != nil {
		t.Fatal(err)
	}
	defer dir.Close()
	scope := &confineReferenceMismatchScope{confineFakeScope: confineFakeScope{}, fd: int(dir.Fd()), reference: referenceDir}
	if err := writeConfineOOMGroup(scope); err == nil {
		t.Fatal("mismatched reference falsely verified a different memory.oom.group")
	}
}

type confineReferenceMismatchScope struct {
	confineFakeScope
	fd        int
	reference string
}

func (scope *confineReferenceMismatchScope) FD() int           { return scope.fd }
func (scope *confineReferenceMismatchScope) Reference() string { return scope.reference }

// verifies: memory.oom.group is both read back as set and effective against a
// two-child partial-fleet survival fixture.
func TestConfineRealSetupHandshakeWriteFailureNeverExecsTarget(t *testing.T) {
	scope := confineRealSetupScope(t, true)
	defer cleanupConfineScope(scope, true)
	marker := filepath.Join(t.TempDir(), "ran")
	invalidHandshake, err := os.Open("/dev/null")
	if err != nil {
		t.Fatal(err)
	}
	defer invalidHandshake.Close()
	releaseRead, releaseWrite, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer releaseRead.Close()
	defer releaseWrite.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, os.Args[0], confineSetupArgv([]string{
		"/bin/sh", "-c", "echo ran > \"$1\"", "sh", marker,
	})...)
	cmd.ExtraFiles = []*os.File{invalidHandshake, releaseRead}
	cmd.SysProcAttr = &syscall.SysProcAttr{UseCgroupFD: true, CgroupFD: scope.FD()}
	cmd.Stderr = io.Discard
	if err := cmd.Start(); err != nil {
		cgrouptest.SkipOrFailRealCgroup(t, "start hidden setup in real scope: %v", err)
	}
	_ = releaseRead.Close()
	_, _ = releaseWrite.Write([]byte{1})
	_ = releaseWrite.Close()
	if err := cmd.Wait(); err == nil {
		t.Fatal("hidden setup unexpectedly succeeded with an unwritable handshake fd")
	}
	if _, statErr := os.Stat(marker); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("target ran after handshake delivery failure: marker err=%v", statErr)
	}
}

func TestConfineRealStandaloneSetupOutsideOOMGroupNeverExecsTarget(t *testing.T) {
	scope := confineRealSetupScope(t, false)
	defer cleanupConfineScope(scope, true)
	marker := filepath.Join(t.TempDir(), "ran")
	handshakeRead, handshakeWrite, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer handshakeRead.Close()
	defer handshakeWrite.Close()
	releaseRead, releaseWrite, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer releaseRead.Close()
	defer releaseWrite.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, os.Args[0], confineSetupArgv([]string{
		"/bin/sh", "-c", "echo ran > \"$1\"", "sh", marker,
	})...)
	cmd.ExtraFiles = []*os.File{handshakeWrite, releaseRead}
	cmd.SysProcAttr = &syscall.SysProcAttr{UseCgroupFD: true, CgroupFD: scope.FD()}
	cmd.Stderr = io.Discard
	if err := cmd.Start(); err != nil {
		cgrouptest.SkipOrFailRealCgroup(t, "start standalone hidden setup in real scope: %v", err)
	}
	_ = handshakeWrite.Close()
	_ = releaseRead.Close()
	_, _ = releaseWrite.Write([]byte{1})
	_ = releaseWrite.Close()
	payload, readErr := readConfineHandshake(handshakeRead, time.Second)
	if readErr != nil {
		t.Fatalf("failure handshake: %v", readErr)
	}
	if handshake, verified := parseConfineHandshake(payload); verified || handshake.applied() {
		t.Fatalf("standalone setup falsely reported verified: %q", payload)
	}
	if err := cmd.Wait(); err == nil {
		t.Fatal("standalone hidden setup unexpectedly succeeded outside an oom.group scope")
	}
	if _, statErr := os.Stat(marker); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("standalone target ran outside oom.group: marker err=%v", statErr)
	}
}

func TestConfineRealSetupClosedReleaseNeverExecsTarget(t *testing.T) {
	scope := confineRealSetupScope(t, true)
	defer cleanupConfineScope(scope, true)
	marker := filepath.Join(t.TempDir(), "ran")
	handshakeRead, handshakeWrite, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer handshakeRead.Close()
	defer handshakeWrite.Close()
	releaseRead, releaseWrite, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer releaseRead.Close()
	defer releaseWrite.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, os.Args[0], confineSetupArgv([]string{
		"/bin/sh", "-c", "echo ran > \"$1\"", "sh", marker,
	})...)
	cmd.ExtraFiles = []*os.File{handshakeWrite, releaseRead}
	cmd.SysProcAttr = &syscall.SysProcAttr{UseCgroupFD: true, CgroupFD: scope.FD()}
	cmd.Stderr = io.Discard
	if err := cmd.Start(); err != nil {
		cgrouptest.SkipOrFailRealCgroup(t, "start hidden setup for closed release: %v", err)
	}
	_ = handshakeWrite.Close()
	_ = releaseRead.Close()
	payload, readErr := readConfineHandshake(handshakeRead, time.Second)
	if readErr != nil {
		t.Fatalf("successful setup handshake: %v", readErr)
	}
	if handshake, verified := parseConfineHandshake(payload); !verified || !handshake.applied() {
		t.Fatalf("setup handshake=%q verified=%v", payload, verified)
	}
	_ = releaseWrite.Close()
	if err := cmd.Wait(); err == nil {
		t.Fatal("hidden setup unexpectedly succeeded after release EOF")
	}
	if _, statErr := os.Stat(marker); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("target ran without a release byte: marker err=%v", statErr)
	}
}

func TestConfineRealTwoWayGatePlacesBeforeTargetMarker(t *testing.T) {
	parent := confineMemoryParent(t, "134217728")
	marker := filepath.Join(t.TempDir(), "ran")
	result, err := Confine(context.Background(), ConfineRequest{
		Slice: parent, MemoryReserve: 1,
		Argv:     []string{"/bin/sh", "-c", "echo ran > \"$1\"", "sh", marker},
		SelfPath: os.Args[0], Stderr: io.Discard,
	})
	if err != nil {
		cgrouptest.SkipOrFailRealCgroup(t, "real two-way placement gate unavailable: %v", err)
	}
	if result.Exit != 0 || result.Status.Scope != ConfineScopePlaced || result.Status.OOMGroup != ConfineOOMGroupSet {
		t.Fatalf("result=%+v", result)
	}
	if data, readErr := os.ReadFile(marker); readErr != nil || strings.TrimSpace(string(data)) != "ran" {
		t.Fatalf("released target marker data=%q err=%v", data, readErr)
	}
}

func confineRealSetupScope(t *testing.T, oomGroup bool) Scope {
	t.Helper()
	parent := confineMemoryParent(t, "max")
	backend := newDefaultBackend(parent)
	if err := backend.Probe(context.Background()); err != nil {
		cgrouptest.SkipOrFailRealCgroup(t, "real setup backend probe: %v", err)
	}
	scope, err := backend.Create(context.Background(), confineScopeID("setup-test"))
	if err != nil {
		cgrouptest.SkipOrFailRealCgroup(t, "real setup scope create: %v", err)
	}
	if oomGroup {
		if err := writeConfineOOMGroup(scope); err != nil {
			cleanupConfineScope(scope, false)
			cgrouptest.SkipOrFailRealCgroup(t, "real setup oom.group: %v", err)
		}
	}
	return scope
}

func TestConfineRealOOMGroupWrittenAndEffective(t *testing.T) {
	if _, err := exec.LookPath("python3"); err != nil {
		cgrouptest.SkipOrFailRealCgroup(t, "python3 is unavailable: %v", err)
	}
	parent := confineMemoryParent(t, "67108864")
	marker := filepath.Join(t.TempDir(), "survived")
	observation := &confineScopeObservation{}
	deps := defaultConfineDeps()
	deps.newBackend = func(path string) ScopeBackend {
		return confineObservingBackend{ScopeBackend: newDefaultBackend(path), observation: observation}
	}
	result, err := confineWithDeps(context.Background(), ConfineRequest{
		Slice: parent, MemoryReserve: 1 << 20, AdmissionMaxWait: 2 * time.Second, PollInterval: 10 * time.Millisecond,
		Argv:     []string{"/bin/sh", "-c", `(sleep 2; echo survived > "$1") & python3 -c 'x=bytearray(256*1024*1024); x[-1]=1'; wait`, "sh", marker},
		SelfPath: os.Args[0], Stderr: io.Discard,
	}, deps)
	if err != nil {
		cgrouptest.SkipOrFailRealCgroup(t, "confine real OOM fixture unavailable: %v", err)
	}
	if result.Exit != 137 {
		t.Fatalf("OOM group leader exit=%d, want 137", result.Exit)
	}
	if observation.oomGroup != "1" {
		t.Fatalf("memory.oom.group=%q, want 1", observation.oomGroup)
	}
	time.Sleep(2200 * time.Millisecond)
	if _, err := os.Stat(marker); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("non-OOMing sibling survived group OOM: marker err=%v", err)
	}
}

// verifies: under a real capped slice the target launches, the cap is reported
// enforced, and the priority knobs (oom_score_adj=500) are applied and inherited.
func TestConfineRealPrioritiesUnderCappedSlice(t *testing.T) {
	parent := confineMemoryParent(t, "67108864")
	var stdout, stderr bytes.Buffer
	result, err := Confine(context.Background(), ConfineRequest{
		Slice: parent, MemoryReserve: 1 << 20,
		Argv:     []string{"/bin/sh", "-c", `cat /proc/self/oom_score_adj; /bin/sh -c 'cat /proc/self/oom_score_adj'`},
		SelfPath: os.Args[0], Stdout: &stdout, Stderr: &stderr,
	})
	if err != nil {
		cgrouptest.SkipOrFailRealCgroup(t, "confine real priority fixture unavailable: %v", err)
	}
	if result.Exit != 0 || result.Status.Cap != ConfineCapEnforced || result.Status.Scope != ConfineScopePlaced || result.Status.OOMGroup != ConfineOOMGroupSet || result.Status.Priorities != ConfinePrioritiesApplied {
		t.Fatalf("result=%+v stdout=%q stderr=%q", result, stdout.String(), stderr.String())
	}
	if fields := strings.Fields(stdout.String()); !reflect.DeepEqual(fields, []string{"500", "500"}) {
		t.Fatalf("oom_score_adj leader/child=%q", stdout.String())
	}
	if !strings.Contains(stderr.String(), "cap=enforced") {
		t.Fatalf("capped status=%q", stderr.String())
	}
}

// verifies: a real slice with memory.max=max (no finite cap in the ancestry) is
// refused, the target never runs, and the failure is E_CONFINE_UNAVAILABLE — the
// child self-check is the defence-in-depth mirror of the parent gate.
func TestConfineRealUncappedSliceRefuses(t *testing.T) {
	parent := confineMemoryParent(t, "max")
	if _, ok := effectiveConfineCap(parent); ok {
		t.Skip("test ancestry already carries a finite memory.max cap; cannot exercise the uncapped path")
	}
	marker := filepath.Join(t.TempDir(), "ran")
	var stderr bytes.Buffer
	result, err := Confine(context.Background(), ConfineRequest{
		Slice: parent, MemoryReserve: 1 << 20,
		Argv:     []string{"/bin/sh", "-c", `: > "$1"`, "sh", marker},
		SelfPath: os.Args[0], Stderr: &stderr,
	})
	if err == nil || !strings.Contains(err.Error(), "E_CONFINE_UNAVAILABLE") {
		t.Fatalf("want E_CONFINE_UNAVAILABLE for an uncapped slice, got result=%+v err=%v stderr=%q", result, err, stderr.String())
	}
	if _, statErr := os.Stat(marker); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("target ran under an uncapped slice: marker err=%v", statErr)
	}
}

func TestConfineRealHandshakeFailureIsUnverified(t *testing.T) {
	parent := confineMemoryParent(t, "max")
	deps := defaultConfineDeps()
	deps.readHandshake = func(*os.File, time.Duration) ([]byte, error) { return nil, errors.New("forced") }
	var stderr bytes.Buffer
	result, err := confineWithDeps(context.Background(), ConfineRequest{
		Slice: parent, Argv: []string{"/bin/true"}, SelfPath: os.Args[0], Stderr: &stderr,
	}, deps)
	if err != nil {
		cgrouptest.SkipOrFailRealCgroup(t, "confine real handshake fixture unavailable: %v", err)
	}
	if result.Exit != 0 || result.Status.Priorities != ConfinePrioritiesUnverified || !strings.Contains(stderr.String(), "priorities=unverified") || strings.Contains(stderr.String(), "priorities=applied") {
		t.Fatalf("result=%+v stderr=%q", result, stderr.String())
	}
}

func TestConfineRealAdmissionWaitsThenProceedsDaemonDown(t *testing.T) {
	if _, err := exec.LookPath("python3"); err != nil {
		cgrouptest.SkipOrFailRealCgroup(t, "python3 is unavailable: %v", err)
	}
	const reserve = int64(64 << 20)
	parent := confineMemoryParent(t, "134217728")
	filler, err := New(Config{CommonDir: t.TempDir(), CgroupParent: parent, Grace: time.Second, TermGrace: 100 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	if err := filler.Probe(context.Background()); err != nil {
		cgrouptest.SkipOrFailRealCgroup(t, "filler scope unavailable: %v", err)
	}
	fillerDone := make(chan error, 1)
	go func() {
		_, launchErr := filler.Launch(context.Background(), Request{Argv: []string{"python3", "-c", "import time; x=bytearray(80*1024*1024); x[-1]=1; time.sleep(0.5)"}})
		fillerDone <- launchErr
	}()
	deadline := time.Now().Add(2 * time.Second)
	for {
		current, maximum, ok, reason := readSliceMemory(parent)
		if !ok {
			t.Fatalf("slice memory: %s", reason)
		}
		if maximum-current < reserve {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("filler did not create admission pressure")
		}
		time.Sleep(5 * time.Millisecond)
	}
	result, err := Confine(context.Background(), ConfineRequest{
		Slice: parent, MemoryReserve: reserve, AdmissionMaxWait: 2 * time.Second, PollInterval: 10 * time.Millisecond,
		AdmitSocketPath: filepath.Join(t.TempDir(), "daemon-down.sock"),
		Argv:            []string{"/bin/true"}, SelfPath: os.Args[0], Stderr: io.Discard,
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.Exit != 0 || result.Status.AdmissionState != "waited" || result.Status.AdmissionWaitedMS <= 0 {
		t.Fatalf("admission result=%+v", result)
	}
	if fillerErr := <-fillerDone; fillerErr != nil {
		t.Fatalf("filler: %v", fillerErr)
	}
	immediate, err := Confine(context.Background(), ConfineRequest{
		Slice: parent, MemoryReserve: reserve, AdmissionMaxWait: 2 * time.Second, PollInterval: 10 * time.Millisecond,
		AdmitSocketPath: filepath.Join(t.TempDir(), "daemon-still-down.sock"),
		Argv:            []string{"/bin/true"}, SelfPath: os.Args[0], Stderr: io.Discard,
	})
	if err != nil || immediate.Exit != 0 || immediate.Status.AdmissionState != "immediate" {
		t.Fatalf("free-slice admission result=%+v err=%v", immediate, err)
	}
}

func TestConfineRealNoMemoryDelegationFailsWithoutLaunch(t *testing.T) {
	parent := cgrouptest.IsolatedScopeParent(t)
	marker := filepath.Join(t.TempDir(), "ran")
	_, err := Confine(context.Background(), ConfineRequest{
		Slice: parent, MemoryReserve: 1, Argv: []string{"/bin/sh", "-c", "echo ran > \"$1\"", "sh", marker},
		SelfPath: os.Args[0], Stderr: io.Discard,
	})
	if _, statErr := os.Stat(marker); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("unconfinable target ran: marker err=%v", statErr)
	}
	if err == nil || !strings.Contains(err.Error(), "E_CONFINE_UNAVAILABLE") || !strings.Contains(err.Error(), "memory.oom.group") {
		t.Fatalf("error=%v", err)
	}
}

type confineScopeObservation struct{ oomGroup string }

type confineObservingBackend struct {
	ScopeBackend
	observation *confineScopeObservation
}

func (backend confineObservingBackend) Create(ctx context.Context, id string) (Scope, error) {
	scope, err := backend.ScopeBackend.Create(ctx, id)
	if err != nil {
		return nil, err
	}
	return &confineObservingScope{Scope: scope, observation: backend.observation}, nil
}

type confineObservingScope struct {
	Scope
	observation *confineScopeObservation
}

func (scope *confineObservingScope) Remove() error {
	if data, err := os.ReadFile(filepath.Join(scope.Reference(), "memory.oom.group")); err == nil {
		scope.observation.oomGroup = strings.TrimSpace(string(data))
	}
	return scope.Scope.Remove()
}

func confineMemoryParent(t *testing.T, maximum string) string {
	t.Helper()
	parent := cgrouptest.IsolatedScopeParent(t)
	if err := os.WriteFile(filepath.Join(parent, "cgroup.subtree_control"), []byte("+memory"), 0o644); err != nil {
		cgrouptest.SkipOrFailRealCgroup(t, "memory controller not delegated to %s: %v", parent, err)
	}
	_ = os.WriteFile(filepath.Join(parent, "memory.swap.max"), []byte("0"), 0o644)
	if err := os.WriteFile(filepath.Join(parent, "memory.max"), []byte(maximum), 0o644); err != nil {
		cgrouptest.SkipOrFailRealCgroup(t, "memory.max is not writable: %v", err)
	}
	return parent
}

type confineUnavailableBackend struct{}

func (confineUnavailableBackend) Probe(context.Context) error { return errors.New("delegation denied") }
func (confineUnavailableBackend) Create(context.Context, string) (Scope, error) {
	return nil, errors.New("must not create")
}
func (confineUnavailableBackend) Open(context.Context, string) (Scope, error) {
	return nil, errors.New("must not open")
}
