//go:build linux

package runner

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"math"
	"net"
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
	"aira/internal/pylib"
	"aira/internal/testdeadline"
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
	// AIRA-115 keeps this assertion LIVE and unconditional. An earlier cut of this
	// ticket neutered it to `false && got != ""` while the emitted parent-slice
	// coordinate still shared AIRA_CONFINE_SLICE's name; that masked the exact
	// regression the rename exists to prevent, so it is restored here and pinned
	// by TestInheritedParentSliceIsNotAnExplicitSliceInput, which asserts the same
	// "" through pylib.ConfineParentSliceEnv.
	if got := ResolveConfineSlice(""); got != "" {
		t.Fatalf("portable default slice=%q, want empty for linux probing", got)
	}
}

func TestDefaultConfineResolutionDistinguishesDeadAnchorFromNeverInstalled(t *testing.T) {
	for _, test := range []struct {
		name        string
		managed     bool
		managedErr  error
		airaPath    string
		airaErr     error
		whalePath   string
		whaleErr    error
		wantName    string
		wantPath    string
		wantErrText string
		wantWhale   bool
	}{
		{
			name:    "managed unit present but cgroup absent is dead anchor",
			managed: true, airaErr: fs.ErrNotExist,
			whalePath: "/cg/whale.slice", wantName: "aira.slice",
			wantErrText: "aira.slice installed but not active — anchor dead? re-run aira install",
		},
		{
			name:    "never installed falls back",
			managed: false, airaErr: fs.ErrNotExist,
			whalePath: "/cg/whale.slice", wantName: "whale.slice", wantPath: "/cg/whale.slice", wantWhale: true,
		},
		{
			name:    "unreadable aira cgroup never falls back",
			managed: false, airaErr: fs.ErrPermission,
			whalePath: "/cg/whale.slice", wantName: "aira.slice", wantErrText: "cannot evaluate aira.slice",
		},
		{
			name:       "unit state unreadable never falls back",
			managedErr: fs.ErrPermission, airaErr: fs.ErrNotExist,
			whalePath: "/cg/whale.slice", wantName: "aira.slice", wantErrText: "cannot evaluate aira.slice unit",
		},
		{
			// cgroup is active (airaErr==nil) but no aira-managed unit file: an
			// active-yet-unmanaged aira.slice must be REFUSED (never confine into
			// it, never fall back to whale — an active cgroup is not definite absence).
			name:    "active cgroup without managed unit refuses and never falls back",
			managed: false, airaPath: "/cg/aira.slice", airaErr: nil,
			whalePath: "/cg/whale.slice", wantName: "aira.slice", wantPath: "",
			wantErrText: "aira-managed unit is absent", wantWhale: false,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			whaleCalled := false
			deps := confineDeps{
				managedUnitPresent: func(string) (bool, error) { return test.managed, test.managedErr },
				resolveSlicePathExact: func(name string) (string, error) {
					if name == "aira.slice" {
						return test.airaPath, test.airaErr
					}
					whaleCalled = true
					return test.whalePath, test.whaleErr
				},
			}
			name, path, err := resolveDefaultConfineSlice(deps)
			if name != test.wantName || path != test.wantPath {
				t.Fatalf("resolution=(%q,%q,%v), want (%q,%q)", name, path, err, test.wantName, test.wantPath)
			}
			if test.wantErrText == "" && err != nil || test.wantErrText != "" && (err == nil || !strings.Contains(err.Error(), test.wantErrText)) {
				t.Fatalf("error=%v, want substring %q", err, test.wantErrText)
			}
			if whaleCalled != test.wantWhale {
				t.Fatalf("whale fallback called=%v, want %v", whaleCalled, test.wantWhale)
			}
		})
	}
}

func TestDefaultConfinePresentButUncappedFailsOnAIRA(t *testing.T) {
	deps := confineUnitDeps(&confineFakeScope{})
	deps.managedUnitPresent = func(string) (bool, error) { return true, nil }
	deps.resolveSlicePathExact = func(name string) (string, error) {
		if name != "aira.slice" {
			t.Fatalf("unexpected fallback to %s", name)
		}
		return "/cg/aira.slice", nil
	}
	deps.readCap = func(string) (int64, bool) { return 0, false }
	result, err := confineWithDeps(context.Background(), ConfineRequest{Argv: []string{"must-not-run"}, Stderr: io.Discard}, deps)
	if err == nil || !strings.Contains(err.Error(), "E_CONFINE_UNAVAILABLE: slice aira.slice") || !strings.Contains(err.Error(), "uncapped") {
		t.Fatalf("result=%+v error=%v", result, err)
	}
	if result.Status.Slice != "aira.slice" {
		t.Fatalf("failure status slice=%q", result.Status.Slice)
	}
}

func TestConfineDelegationResetFailsClosedBeforeCreate(t *testing.T) {
	created := false
	started := false
	deps := confineDeps{
		resolveSlicePath: func(string) (string, bool, string) { return "/cg/aira.slice", true, "" },
		ensureDelegation: func(string) (confineDelegation, error) {
			return confineDelegation{}, errors.New("memory missing from cgroup.subtree_control")
		},
		newBackend: func(string) ScopeBackend { return confineCreateTrackingBackend{created: &created} },
		readCap:    func(string) (int64, bool) { return 16 << 30, true },
		start:      func(*confineCommand) error { started = true; return nil },
	}
	_, err := confineWithDeps(context.Background(), ConfineRequest{Slice: "aira.slice", Argv: []string{"must-not-run"}, Stderr: io.Discard}, deps)
	if err == nil || !strings.Contains(err.Error(), "E_CONFINE_UNAVAILABLE") || !strings.Contains(err.Error(), "subtree_control") {
		t.Fatalf("error=%v", err)
	}
	if created || started {
		t.Fatalf("delegation failure reached create=%v start=%v", created, started)
	}
}

type confineCreateTrackingBackend struct{ created *bool }

func (confineCreateTrackingBackend) Probe(context.Context) error { return nil }
func (b confineCreateTrackingBackend) Create(context.Context, string) (Scope, error) {
	*b.created = true
	return nil, errors.New("unexpected create")
}
func (confineCreateTrackingBackend) Open(context.Context, string) (Scope, error) {
	return nil, errors.New("unused")
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

func TestParseConfineSetupArgsAcceptsOnlyBestEffortIOClass(t *testing.T) {
	argv := []string{
		"--handshake-fd", "3", "--release-fd", "4", "--oom-score-adj", "500", "--nice", "19", "--ionice-class", "2", "--", "/bin/true",
	}
	_, _, _, _, ioClass, target, err := parseConfineSetupArgs(argv)
	if err != nil {
		t.Fatalf("parse class 2: %v", err)
	}
	if ioClass != 2 || !reflect.DeepEqual(target, []string{"/bin/true"}) {
		t.Fatalf("parse result class=%d target=%q", ioClass, target)
	}

	for _, ioClass := range []string{"0", "1", "3", "4"} {
		t.Run("class-"+ioClass, func(t *testing.T) {
			invalid := append([]string(nil), argv...)
			invalid[9] = ioClass
			if _, _, _, _, _, _, err := parseConfineSetupArgs(invalid); err == nil {
				t.Fatalf("class %s unexpectedly accepted", ioClass)
			}
		})
	}
}

func TestConfineIOPriorityEncodingIsLowestBestEffort(t *testing.T) {
	if confineIOPriorityClass != 2 {
		t.Fatalf("I/O priority class=%d, want best-effort class 2", confineIOPriorityClass)
	}
	if confineIOPriorityData != 7 {
		t.Fatalf("I/O priority data=%d, want lowest best-effort priority 7", confineIOPriorityData)
	}
	if got := confineIOPriority(confineIOPriorityClass); got != 16391 {
		t.Fatalf("encoded ioprio=%d, want 16391", got)
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
		"oom.group=set", "priorities=unverified", "cpu-weight=unavailable",
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

// verifies: AIRA-104's whole-subtree resource counters render as unevaluated,
// never a fabricated zero or a silent omission, when the teardown read could
// not establish them.
func TestFormatConfineStatusReportsResourceFacetsAsUnevaluatedWhenNil(t *testing.T) {
	line := FormatConfineStatus(ConfineStatus{Slice: "finite.slice"})
	if !strings.Contains(line, "peak-rss=unevaluated") {
		t.Fatalf("expected peak-rss=unevaluated, got: %q", line)
	}
	if !strings.Contains(line, "cpu=unevaluated") {
		t.Fatalf("expected cpu=unevaluated, got: %q", line)
	}
	if strings.Contains(line, "peak-rss=0") || strings.Contains(line, "cpu=0") {
		t.Fatalf("an unestablished counter must never render as a fabricated zero: %q", line)
	}
}

// verifies: AIRA-104's peak-rss/cpu render the actually-captured values, with
// cpu naming user and system time separately (mirroring RunRecord's existing
// CPUUser/CPUSys split rather than a single combined figure).
func TestFormatConfineStatusReportsResourceFacetsWhenEstablished(t *testing.T) {
	peak := int64(2 << 30)
	user := int64(1_500_000) // 1.5s
	system := int64(250_000) // 0.25s
	line := FormatConfineStatus(ConfineStatus{
		Slice: "finite.slice", PeakRSS: &peak, CPUUser: &user, CPUSys: &system,
	})
	for _, want := range []string{"peak-rss=2G", "cpu=1.5s+250ms"} {
		if !strings.Contains(line, want) {
			t.Fatalf("status %q lacks %q", line, want)
		}
	}
}

// verifies: a partially-established CPU read (one of user/system missing,
// which cpu.stat's own key-value parse can in principle produce) must not
// render a half-true figure -- the whole cpu= facet reads as unevaluated
// rather than fabricating the missing half as zero.
func TestFormatConfineStatusReportsCPUUnevaluatedWhenOnlyOneHalfEstablished(t *testing.T) {
	user := int64(1_000_000)
	line := FormatConfineStatus(ConfineStatus{Slice: "finite.slice", CPUUser: &user})
	if !strings.Contains(line, "cpu=unevaluated") {
		t.Fatalf("half-established cpu read must read unevaluated, got: %q", line)
	}
	if strings.Contains(line, "cpu=1s+") {
		t.Fatalf("must not fabricate the missing system-time half: %q", line)
	}
}

// verifies: the resource facets are placed adjacent to (immediately after)
// the exclusive facet block, so a granted exclusive run's resource numbers
// sit next to its exclusivity attestation -- the AIRA-101 benchmarking use
// case this ticket exists to serve.
func TestFormatConfineStatusPlacesResourceFacetsAfterExclusive(t *testing.T) {
	peak := int64(1 << 20)
	line := FormatConfineStatus(ConfineStatus{
		Slice: "finite.slice", TerminatedBy: ConfineTerminatedNormal,
		Exclusive: ConfineExclusiveGranted, ExclusiveDrainedMS: 5000,
		PeakRSS: &peak,
	})
	exclusiveIdx := strings.Index(line, "exclusive=granted")
	peakIdx := strings.Index(line, "peak-rss=")
	if exclusiveIdx == -1 || peakIdx == -1 || peakIdx < exclusiveIdx {
		t.Fatalf("expected peak-rss to follow exclusive on the trailer, got: %q", line)
	}
}

// verifies: CPU aging is an independent honesty facet. A successful memory
// confine must not fabricate aging when cpu delegation or its initial write
// could not be established.
func TestFormatConfineStatusReportsCPUWeightFacet(t *testing.T) {
	aging := FormatConfineStatus(ConfineStatus{CPUWeight: ConfineCPUWeightAging})
	if !strings.Contains(aging, "cpu-weight=aging") || strings.Contains(aging, "cpu-weight=unavailable") {
		t.Fatalf("aging status=%q", aging)
	}
	unavailable := FormatConfineStatus(ConfineStatus{})
	if !strings.Contains(unavailable, "cpu-weight=unavailable") || strings.Contains(unavailable, "cpu-weight=aging") {
		t.Fatalf("unavailable status=%q", unavailable)
	}
}

// verifies: the real delegation policy keeps a missing CPU controller
// fail-open, while a failing memory enable remains fail-closed. The injected
// file operations exercise ensureConfineDelegation's production decision path,
// rather than stubbing it out at the launch layer.
func TestEnsureConfineDelegationSeparatesCPUFailOpenFromMemoryFailClosed(t *testing.T) {
	const parent = "/cg/parent"
	newOps := func(controllers string, memoryWriteErr error) confineControllerOps {
		subtree := ""
		return confineControllerOps{
			readFile: func(path string) ([]byte, error) {
				switch path {
				case filepath.Join(parent, "cgroup.controllers"):
					return []byte(controllers), nil
				case filepath.Join(parent, "cgroup.subtree_control"):
					return []byte(subtree), nil
				default:
					return nil, fmt.Errorf("unexpected cgroup path %q", path)
				}
			},
			writeFile: func(path string, value []byte) error {
				if path != filepath.Join(parent, "cgroup.subtree_control") {
					return fmt.Errorf("unexpected subtree path %q", path)
				}
				if string(value) == "+memory\n" && memoryWriteErr != nil {
					return memoryWriteErr
				}
				subtree = strings.TrimPrefix(string(value), "+")
				return nil
			},
		}
	}

	t.Run("cpu absent is unavailable, not fatal", func(t *testing.T) {
		delegation, err := ensureConfineDelegationWithOps(parent, newOps("memory io", nil))
		if err != nil || delegation != (confineDelegation{}) {
			t.Fatalf("delegation=%+v err=%v, want cpuWeight=false and nil", delegation, err)
		}
	})
	t.Run("memory enable failure is fatal", func(t *testing.T) {
		delegation, err := ensureConfineDelegationWithOps(parent, newOps("memory cpu", errors.New("injected memory write failure")))
		if err == nil || delegation != (confineDelegation{}) {
			t.Fatalf("delegation=%+v err=%v, want memory delegation error", delegation, err)
		}
	})
}

// verifies: even after successful +cpu delegation, an initial cpu.weight
// write failure is informational only. This is deliberately unlike the memory
// cap writer, whose failure correctly prevents launch.
func TestConfineCPUWeightInitialWriteFailureIsFailOpen(t *testing.T) {
	scope := &confineFakeScope{}
	deps := confineUnitDeps(scope)
	deps.writeScopeCPUWeight = func(Scope, int64) bool { return false }
	result, err := confineWithDeps(context.Background(), ConfineRequest{
		Slice: "finite.slice", Argv: []string{"/bin/true"}, SelfPath: os.Args[0], Stderr: io.Discard,
	}, deps)
	if err != nil || result.Exit != 0 || result.Status.CPUWeight != ConfineCPUWeightUnavailable {
		t.Fatalf("result=%+v err=%v", result, err)
	}
}

// verifies: the parent writes the initial weight before starting the child,
// which keeps the child-side setup limited to nice/ionice and avoids a
// post-clone race.
func TestConfineInitialCPUWeightWritePrecedesStart(t *testing.T) {
	scope := &confineFakeScope{}
	deps := confineUnitDeps(scope)
	written := false
	deps.writeScopeCPUWeight = func(_ Scope, weight int64) bool {
		if weight != 100 {
			t.Fatalf("initial cpu.weight=%d, want 100", weight)
		}
		written = true
		return true
	}
	innerStart := deps.start
	deps.start = func(command *confineCommand) error {
		if !written {
			t.Fatal("target start preceded parent cpu.weight write")
		}
		return innerStart(command)
	}
	result, err := confineWithDeps(context.Background(), ConfineRequest{
		Slice: "finite.slice", Argv: []string{"/bin/true"}, SelfPath: os.Args[0], Stderr: io.Discard,
	}, deps)
	if err != nil || result.Exit != 0 || result.Status.CPUWeight != ConfineCPUWeightAging {
		t.Fatalf("result=%+v err=%v", result, err)
	}
}

// verifies: decay uses the elapsed schedule, never raises a weight, and
// reaches the floor. Removing the timer writes makes the final-floor assertion
// fail, rather than merely testing static configuration.
func TestCPUWeightDecayScheduleIsMonotoneAndReachesFloor(t *testing.T) {
	steps := []cpuWeightStep{{After: time.Millisecond, Weight: 70}, {After: 2 * time.Millisecond, Weight: 10}}
	weights := make(chan int64, len(steps))
	stop := startCPUWeightDecay(nil, steps, func(_ Scope, weight int64) bool {
		weights <- weight
		return true
	})
	defer stop()
	got := make([]int64, 0, len(steps))
	deadline := testdeadline.After(time.Second)
	for len(got) < len(steps) {
		select {
		case weight := <-weights:
			got = append(got, weight)
		case <-deadline:
			t.Fatalf("decay writes=%v, want schedule through floor", got)
		}
	}
	if got[len(got)-1] != 10 {
		t.Fatalf("aged scope weight=%d, want floor 10", got[len(got)-1])
	}
	for index := 1; index < len(got); index++ {
		if got[index] > got[index-1] {
			t.Fatalf("weights rose: %v", got)
		}
	}
}

func TestCPUWeightDefaultCurve(t *testing.T) {
	t.Setenv("AIRA_CONFINE_CPUWEIGHT_START", "")
	t.Setenv("AIRA_CONFINE_CPUWEIGHT_FLOOR", "")
	t.Setenv("AIRA_CONFINE_CPUWEIGHT_SCHEDULE", "")
	config := confineCPUWeightConfig()
	want := []cpuWeightStep{
		{After: 10 * time.Second, Weight: 100},
		{After: 30 * time.Second, Weight: 70},
		{After: time.Minute, Weight: 50},
		{After: 5 * time.Minute, Weight: 30},
		{After: 10 * time.Minute, Weight: 20},
		{After: 30 * time.Minute, Weight: 10},
	}
	if config.Start != 100 || config.Floor != 10 || !reflect.DeepEqual(config.Steps, want) {
		t.Fatalf("default curve=%+v, want start=100 floor=10 steps=%+v", config, want)
	}
	for index := 1; index < len(config.Steps); index++ {
		if config.Steps[index].Weight > config.Steps[index-1].Weight {
			t.Fatalf("normal curve rose: %+v", config.Steps)
		}
	}
}

func TestCPUWeightConfigClampsStartFloorAndScheduleBounds(t *testing.T) {
	t.Setenv("AIRA_CONFINE_CPUWEIGHT_START", "99999")
	t.Setenv("AIRA_CONFINE_CPUWEIGHT_FLOOR", "0")
	t.Setenv("AIRA_CONFINE_CPUWEIGHT_SCHEDULE", "1s:99999,2s:0")
	config := confineCPUWeightConfig()
	if config.Start != 10000 || config.Floor != 1 {
		t.Fatalf("high-start/low-floor config=%+v, want start=10000 floor=1", config)
	}
	if want := []cpuWeightStep{{After: time.Second, Weight: 10000}, {After: 2 * time.Second, Weight: 1}}; !reflect.DeepEqual(config.Steps, want) {
		t.Fatalf("out-of-range schedule=%+v, want %+v", config.Steps, want)
	}

	t.Setenv("AIRA_CONFINE_CPUWEIGHT_FLOOR", "99999")
	config = confineCPUWeightConfig()
	if config.Start != 10000 || config.Floor != 10000 {
		t.Fatalf("high-floor config=%+v, want start=floor=10000", config)
	}

	t.Setenv("AIRA_CONFINE_CPUWEIGHT_START", "0")
	t.Setenv("AIRA_CONFINE_CPUWEIGHT_FLOOR", "0")
	config = confineCPUWeightConfig()
	if config.Start != 1 || config.Floor != 1 {
		t.Fatalf("low-start/low-floor config=%+v, want start=floor=1", config)
	}
}

// verifies: task-57 status distinguishes an unrequested scope cap from a verified
// cap and labels memory.high only as reclaim pressure.
func TestFormatConfineStatusReportsScopeMemoryFacet(t *testing.T) {
	plain := FormatConfineStatus(ConfineStatus{Slice: "finite.slice"})
	if !strings.Contains(plain, "scope-memory.max=not-requested") || strings.Contains(plain, "binding=") {
		t.Fatalf("plain status=%q", plain)
	}
	capped := FormatConfineStatus(ConfineStatus{
		Slice: "finite.slice", ScopeMemoryMax: 32 << 20, ScopeMemoryHigh: 16 << 20,
		ScopeMemoryBinding: "scope-limited", ScopeMemoryEffective: 32 << 20,
	})
	for _, want := range []string{
		"scope-memory.max=enforced=33554432", "binding=scope-limited", "effective=33554432",
		"memory.high=reclaim-pressure=16777216",
	} {
		if !strings.Contains(capped, want) {
			t.Fatalf("capped status %q lacks %q", capped, want)
		}
	}
	if strings.Contains(capped, "memory.high=cap") {
		t.Fatalf("memory.high falsely labelled a cap: %q", capped)
	}
}

// verifies: a whole-job reserve advisory is emitted only for an actually
// enforced scope cap, and never fabricates a peak when it cannot be read.
func TestFormatConfineReserveAdvisory(t *testing.T) {
	peak90 := int64(90)
	peak95 := int64(95)
	peak89 := int64(89)
	peak40G := int64(40 << 30)
	halfMax := int64(math.MaxInt64 / 2)
	maxPeak := int64(math.MaxInt64)
	almostMax := int64(math.MaxInt64 - 1)
	for _, test := range []struct {
		name     string
		cap      int64
		peak     *int64
		oom      bool
		source   string
		sliceCap int64
		want     string
	}{
		// AIRA-133. The rows below are the whole point of the source argument:
		// the same cap, the same peak and the same kill produce DIFFERENT next
		// steps, each keyed on recorded provenance rather than on anything
		// re-derived from the numbers. The pairing that matters is the operator
		// rows against the auto rows — an operator-supplied cap must never be
		// answered with "re-run", and an AIRA-chosen cap must never be answered
		// with "raise your own flag".
		{
			// AIRA-184's false-pass direction: a slice cap IS available here, and an
			// operator-pinned cap must not borrow one word of the estimated-cap
			// wording -- not the "auto-estimate" claim, not the slice figure, not a
			// pin suggestion for a flag the caller already set themselves.
			name: "oom against an operator --memory-max says re-running will not help", cap: 100, peak: &peak95, oom: true,
			source: ConfineCapSourceMemoryMax, sliceCap: 64 << 30,
			want: "confine: job OOM-killed at its memory cap 100 (peak RSS 95); cap-source=operator:--memory-max — " +
				"this cap is YOUR OWN --memory-max, not an AIRA estimate, so re-running the identical command will not change it. " +
				"Raise that flag, or split heavy work.",
		},
		{
			name: "oom against an operator --memory-reserve says re-running will not help", cap: 100, peak: &peak95, oom: true,
			source: ConfineCapSourceMemoryReserve, sliceCap: 64 << 30,
			want: "confine: job OOM-killed at its memory cap 100 (peak RSS 95); cap-source=operator:--memory-reserve — " +
				"this cap is YOUR OWN --memory-reserve, not an AIRA estimate, so re-running the identical command will not change it. " +
				"Raise that flag, or split heavy work.",
		},
		{
			// AIRA-184. The estimated-cap case the ticket is about: the line says in
			// so many words that the cap was AIRA's own auto-estimate, names the
			// slice's own cap so the reader can see the ceiling was not what killed
			// the job, and hands over a concrete higher --memory-reserve rather than
			// leaving the reader to derive one. The old wording additionally claimed
			// the number came "from this command's peak-RSS history", which is false
			// for the commonest shape here -- a cold start at the machine-wide
			// `estimate:p90-prior`, where this command has no history at all.
			name: "oom against a daemon-resolved reserve names the estimate, the slice cap and a pin", cap: 100, peak: &peak95, oom: true,
			source: ConfineCapSourceDaemonReserve, sliceCap: 64 << 30,
			want: "confine: job OOM-killed at its memory cap 100 (peak RSS 95); cap-source=auto:daemon-reserve — " +
				"this cap is AIRA's OWN AUTO-ESTIMATE of what this command needs, not a limit you set, and this slice's own cap " +
				"is 64G, so what bound this job was the estimate and not the slice ceiling. The kill is now recorded against this " +
				"command's signature: RE-RUN THE IDENTICAL COMMAND and the next admission is sized higher on its own, or pin " +
				"--memory-reserve 1M now to skip the cycle. If an identical re-run is killed at the same cap again, that is a " +
				"genuine bug worth reporting.",
		},
		{
			// AIRA-184, the honesty direction: with no slice cap established there
			// is no headroom claim to make, and the line says so rather than
			// asserting room it cannot see. The pin and the re-run stay: neither
			// depends on knowing the slice.
			name: "oom against a daemon-resolved reserve with no slice cap claims no headroom", cap: 100, peak: &peak95, oom: true,
			source: ConfineCapSourceDaemonReserve,
			want: "confine: job OOM-killed at its memory cap 100 (peak RSS 95); cap-source=auto:daemon-reserve — " +
				"this cap is AIRA's OWN AUTO-ESTIMATE of what this command needs, not a limit you set; this slice's own cap could " +
				"not be established, so whether the slice had room above the estimate is unevaluated. The kill is now recorded " +
				"against this command's signature: RE-RUN THE IDENTICAL COMMAND and the next admission is sized higher on its own, " +
				"or pin --memory-reserve 1M now to skip the cycle. If an identical re-run is killed at the same cap again, that is " +
				"a genuine bug worth reporting.",
		},
		{
			// AIRA-184, the OTHER false-pass direction, and the one an unconditional
			// "the slice had headroom" would get wrong: a job killed at a cap whose
			// 1.5x escalation the slice cannot hold has no PIN to be handed, so the
			// line must not name one. `sliceCap` here is exactly the suggested pin,
			// which is the entry boundary: a pin requires the slice to be strictly
			// larger.
			//
			// The build review's confirmed BLOCK is the OTHER half of this row: the
			// wording used to go on to assert that a re-run "is refused
			// E_ADMIT_TOO_LARGE rather than run" and dropped the re-run advice. That
			// generalised skill.go's narrow rule (an OOM AT what the slice can give)
			// to every peak at or above two-thirds of the slice cap, which does not
			// imply refusal at all -- the daemon clamps an unpinned over-ceiling
			// escalation DOWN to FIT(ceiling) and admits it (admit.go's
			// `MaxOOMPeak < fit` guard). The re-run advice therefore stays in every
			// daemon-reserve outcome, and the refusal is stated as the condition it
			// actually is, which only admission can decide.
			name: "oom against a daemon-resolved reserve above the slice cap offers no pin but keeps the re-run", cap: 100, peak: &peak95, oom: true,
			source: ConfineCapSourceDaemonReserve, sliceCap: 1 << 20,
			want: "confine: job OOM-killed at its memory cap 100 (peak RSS 95); cap-source=auto:daemon-reserve — " +
				"this cap is AIRA's OWN AUTO-ESTIMATE of what this command needs, not a limit you set, but the pin AIRA would " +
				"otherwise suggest — 1.5x this run's own figure, 1M — is at or above this slice's own cap of 1M, so no pin is " +
				"offered. The kill is now recorded against this command's signature: RE-RUN THE IDENTICAL COMMAND — admission " +
				"sizes the next run itself, fitting it under this slice's cap where the recorded peak leaves room, and refusing " +
				"it E_ADMIT_TOO_LARGE (naming both required and cap_minus_headroom) only where that peak is already at what " +
				"this slice can give. Split heavy work, or run where the slice is larger, only if it is in fact refused.",
		},
		{
			// AIRA-184 build review, the confirmed counterexample, taken from the
			// daemon's OWN fixture (internal/daemon/confine_admit_test.go, the
			// signature `oom` in TestConfineEstimatorAndOOMEscalationClamp): a job
			// OOM-killed at a 40G cap with a 40G peak on a slice whose cap is 56G.
			// The 1.5x pin is 60G, which is over the slice cap and so cannot be
			// named -- but the identical re-run is NOT refused: the daemon clamps
			// the escalation to FIT(ceiling) and admits it, exactly as that fixture
			// asserts (reserve 51352869843, basis
			// `estimate:oom-escalated,ceiling-clamped`). The 1M row above cannot
			// establish this, because SliceFittedReserve returns 0 below
			// MinPinnedScopeCap, so on a 1M slice there is no fit to admit at.
			name: "oom at a cap the daemon would still admit a re-run under keeps the re-run", cap: 40 << 30, peak: &peak40G, oom: true,
			source: ConfineCapSourceDaemonReserve, sliceCap: 56 << 30,
			want: "confine: job OOM-killed at its memory cap 40G (peak RSS 40G); cap-source=auto:daemon-reserve — " +
				"this cap is AIRA's OWN AUTO-ESTIMATE of what this command needs, not a limit you set, but the pin AIRA would " +
				"otherwise suggest — 1.5x this run's own figure, 60G — is at or above this slice's own cap of 56G, so no pin is " +
				"offered. The kill is now recorded against this command's signature: RE-RUN THE IDENTICAL COMMAND — admission " +
				"sizes the next run itself, fitting it under this slice's cap where the recorded peak leaves room, and refusing " +
				"it E_ADMIT_TOO_LARGE (naming both required and cap_minus_headroom) only where that peak is already at what " +
				"this slice can give. Split heavy work, or run where the slice is larger, only if it is in fact refused.",
		},
		{
			// An unrecorded source is never resolved to either party's choice:
			// the line names both possibilities instead of guessing one. A known
			// slice cap does not change that -- an unestablished provenance may not
			// borrow the estimated-cap wording either.
			name: "oom with observed peak", cap: 100, peak: &peak95, oom: true, sliceCap: 64 << 30,
			want: "confine: job OOM-killed at its memory cap 100 (peak RSS 95); cap-source=unevaluated — " +
				"where this cap came from could not be established. If you set --memory-max/--memory-reserve yourself, raise it; " +
				"if AIRA estimated it, re-running the identical command admits at a higher reserve. " +
				"Either way, splitting heavy work also helps.",
		},
		{
			name: "oom with unreadable peak", cap: 100, oom: true,
			want: "confine: job OOM-killed at its memory cap 100 (peak RSS unknown); cap-source=unevaluated — " +
				"where this cap came from could not be established. If you set --memory-max/--memory-reserve yourself, raise it; " +
				"if AIRA estimated it, re-running the identical command admits at a higher reserve. " +
				"Either way, splitting heavy work also helps.",
		},
		{
			// Negative control: removing the cap==0 guard would falsely advise
			// delegate-ram/unevaluated jobs and make this case fail.
			name: "oom without enforced whole-job cap", cap: 0, peak: &peak95, oom: true,
		},
		{
			name: "exactly ninety percent is near cap", cap: 100, peak: &peak90,
			want: "confine: peak RSS 90 reached 90% of the reserved cap 100; consider a higher --memory-reserve or --delegate-ram for suites",
		},
		{
			name: "above ninety percent reports floored percentage", cap: 100, peak: &peak95,
			want: "confine: peak RSS 95 reached 95% of the reserved cap 100; consider a higher --memory-reserve or --delegate-ram for suites",
		},
		{
			// Negative control: removing the threshold guard would make this
			// sub-ninety-percent observation emit a dishonest advisory.
			name: "below ninety percent is quiet", cap: 100, peak: &peak89,
		},
		{name: "unreadable peak without oom is quiet", cap: 100},
		{name: "zero cap is always quiet", cap: 0, peak: &peak95},
		{
			// Non-round cap: 90/101 = 89.1% must stay quiet under the conservative
			// `cap - cap/10` threshold; the old `cap*9/10` floor wrongly fired here.
			name: "just below ninety percent with a non-round cap is quiet", cap: 101, peak: &peak90,
		},
		{
			// Overflow safety: a cap near MaxInt64 must not overflow the threshold
			// nor fabricate a near-cap warning for a sub-threshold peak.
			name: "huge sub-threshold cap stays quiet without overflow", cap: math.MaxInt64, peak: &halfMax,
		},
		{
			// Overflow safety: peak == cap near MaxInt64 reports 100% without
			// overflowing the percentage multiply.
			name: "huge near-cap reports safe percentage", cap: math.MaxInt64, peak: &maxPeak,
			want: fmt.Sprintf("confine: peak RSS %s reached 100%% of the reserved cap %s; consider a higher --memory-reserve or --delegate-ram for suites", FormatConfineBytes(math.MaxInt64), FormatConfineBytes(math.MaxInt64)),
		},
		{
			// Overflow safety, discriminating: peak = cap-1 near MaxInt64 has a huge
			// remainder, so peak%cap*100 overflows int64; the 128-bit path must still
			// floor to 99%. A split-division would wrap here and report a wrong value.
			name: "huge just-below-cap reports 99 percent without overflow", cap: math.MaxInt64, peak: &almostMax,
			want: fmt.Sprintf("confine: peak RSS %s reached 99%% of the reserved cap %s; consider a higher --memory-reserve or --delegate-ram for suites", FormatConfineBytes(math.MaxInt64-1), FormatConfineBytes(math.MaxInt64)),
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := formatConfineReserveAdvisory(test.cap, test.peak, test.oom, test.source, test.sliceCap); got != test.want {
				t.Fatalf("formatConfineReserveAdvisory(%d, %v, %v, %q, %d) = %q, want %q", test.cap, test.peak, test.oom, test.source, test.sliceCap, got, test.want)
			}
		})
	}
}

// verifies: AIRA-184 build review -- EVERY daemon-reserve OOM outcome keeps the
// re-run advice, and none of them asserts a refusal only admission can decide.
//
// The confirmed counterexample is the daemon's own fixture: cap 40G, peak 40G,
// slice cap 56G. The advisory's no-pin condition here is `1.5 x peak >= slice
// cap`, i.e. peak >= two-thirds of the cap, which is much WIDER than the rule
// skill.go documents ("its own measured peak is then already at what the slice
// can give"). Inside that gap the daemon clamps the over-ceiling escalation down
// to FIT(ceiling) ~= 0.87 x (cap - headroom) and ADMITS it (admit.go's
// `fit > 0 && stats.MaxOOMPeak < fit && reserve > ceiling`), so the identical
// re-run runs with zero operator action. A client that told the caller it would
// be refused would be asserting something it cannot establish -- it knows
// neither the headroom term nor the fit -- which is precisely the class of claim
// this project's honesty rule forbids.
//
// This is kept as a PROPERTY beside the exact-wording table rows so that a later
// rewording cannot quietly restore the defect while the table is updated to
// match itself.
func TestEstimatedCapOOMAdvisoryNeverAssertsARefusalItCannotEstablish(t *testing.T) {
	for _, test := range []struct {
		name     string
		cap      int64
		peak     int64
		sliceCap int64
	}{
		// The daemon's own fixture (internal/daemon/confine_admit_test.go,
		// signature `oom`): the escalation is clamped to FIT and admitted.
		{name: "the daemon's own clamped-and-admitted fixture", cap: 40 << 30, peak: 40 << 30, sliceCap: 56 << 30},
		// The degenerate boundary: suggested pin == slice cap exactly.
		{name: "the suggested pin exactly at the slice cap", cap: 100, peak: 95, sliceCap: 1 << 20},
		// Room to spare: the ordinary shape, already covered by the table, kept
		// here so the property is asserted across the branch boundary rather than
		// on one side of it.
		{name: "the estimate well under the slice cap", cap: 96 << 20, peak: 100 << 20, sliceCap: 64 << 30},
		// No slice cap established at all.
		{name: "no slice cap established", cap: 96 << 20, peak: 100 << 20, sliceCap: 0},
	} {
		t.Run(test.name, func(t *testing.T) {
			peak := test.peak
			got := formatConfineReserveAdvisory(test.cap, &peak, true, ConfineCapSourceDaemonReserve, test.sliceCap)
			if !strings.Contains(got, "RE-RUN THE IDENTICAL COMMAND") {
				t.Fatalf("a daemon-estimated cap OOM dropped the re-run advice, which is the ONE remedy that needs no operator action: %q", got)
			}
			if strings.Contains(got, "refused E_ADMIT_TOO_LARGE rather than run") {
				t.Fatalf("the advisory asserted a terminal refusal it cannot establish -- the daemon clamps an unpinned escalation to FIT(ceiling) and admits it: %q", got)
			}
		})
	}
}

// verifies: AIRA-184 -- the pin the estimated-cap advisory hands over is a
// figure the reader can paste, is never BELOW what this run already proved it
// needs, and is a value `--memory-reserve` will actually accept.
func TestConfineSuggestedReserve(t *testing.T) {
	const mib = int64(1) << 20
	peakUnder := int64(80 << 20)
	// The reported incident's own numbers (AIRA-184): a job that overshot its
	// estimated cap by ~12 KiB and was killed for it.
	incidentPeak := int64(1257902080)
	// An overshoot large enough to change the answer, which the incident's own
	// ~12 KiB is not: the escalation must be taken from the PEAK, because a
	// suggestion built from the cap alone sits below the footprint the run
	// already demonstrated.
	peakOver := int64(101) << 20
	maxPeak := int64(math.MaxInt64)
	nearMax := int64(math.MaxInt64 - 7)
	for _, test := range []struct {
		name string
		cap  int64
		peak *int64
		want int64
	}{
		{name: "1.5x the cap when the peak is under it", cap: 96 << 20, peak: &peakUnder, want: 144 << 20},
		{
			// Documentation, not discrimination: the incident's own overshoot is
			// ~12 KiB, which the round-up to a whole MiB absorbs, so cap and peak
			// give the same answer here. The row below is the one that separates
			// them.
			name: "the reported incident's own numbers", cap: 1257889792, peak: &incidentPeak, want: 1800 * mib,
		},
		{
			// The false-pass this row exists for: an overshoot big enough to cross
			// a MiB boundary. Escalating the CAP gives 150MiB; escalating the peak
			// the kernel actually measured gives 151.5MiB -> 152MiB. A suggestion
			// built from the cap alone is below the footprint this run proved.
			name: "1.5x the peak when the peak overshot the cap", cap: 100 * mib, peak: &peakOver, want: 152 * mib,
		},
		// A single byte over a round cap: 1.5x is 150MiB+1, which must round UP to
		// 151MiB. Rounding down would suggest a reserve below the escalation, and
		// not rounding at all would render as a raw byte count.
		{name: "rounds up to a whole MiB", cap: 100*mib + 1, peak: nil, want: 151 * mib},
		{
			// --memory-reserve refuses anything below 1MiB, so a suggestion below
			// its own floor would be advice the CLI rejects.
			name: "never below the 1MiB --memory-reserve floor", cap: 100, peak: nil, want: mib,
		},
		{name: "an unestablished peak and an absent cap still suggest the floor", cap: 0, peak: nil, want: mib},
		{
			// Overflow safety, both directions: the 1.5x multiply and the
			// round-up must each saturate rather than wrap negative.
			name: "saturates at the top of the range", cap: math.MaxInt64, peak: &maxPeak, want: (math.MaxInt64 / mib) * mib,
		},
		{name: "round-up near MaxInt64 does not wrap", cap: nearMax, peak: nil, want: (math.MaxInt64 / mib) * mib},
	} {
		t.Run(test.name, func(t *testing.T) {
			got := confineSuggestedReserve(test.cap, test.peak)
			if got != test.want {
				t.Fatalf("confineSuggestedReserve(%d, %v) = %d, want %d", test.cap, test.peak, got, test.want)
			}
			if got < mib {
				t.Fatalf("suggestion %d is below the 1MiB --memory-reserve floor", got)
			}
			if got%mib != 0 {
				t.Fatalf("suggestion %d is not a whole MiB, so it renders as a raw byte count", got)
			}
			if value, err := ParseMemorySize(FormatConfineBytes(got)); err != nil || value != got {
				t.Fatalf("the rendered suggestion %q does not round-trip through --memory-reserve's own parser: %d, %v",
					FormatConfineBytes(got), value, err)
			}
		})
	}
}

// verifies: AIRA-184 -- the slice cap the advisory names is the one the LAUNCH
// PATH established, wired from the same status field the trailer renders.
//
// A table test over the formatter cannot see this: it would pass just as
// happily against a call site that passed the job's own cap, or zero, for the
// slice figure -- which would turn the headroom half of the line into a
// fabrication. Only a launch whose slice cap is read by deps.readCap and whose
// scope cap is chosen by the daemon-reserve branch closes that gap.
func TestOOMAdvisoryNamesTheSliceCapEstablishedAtLaunch(t *testing.T) {
	scope := &confineFakeScope{}
	deps := confineUnitDeps(scope)
	// Unpinned, admitted, daemon-granted: the one branch that records
	// cap-source=auto:daemon-reserve. confineUnitDeps' readCap reports a 64G
	// slice, which is the figure the advisory must name.
	deps.admit = func(context.Context, string, ConfineRequest, int64) (admissionResult, error) {
		return admissionResult{state: "immediate", reserve: 96 << 20, basis: "estimate:p90-prior", release: &confineCountingCloser{}}, nil
	}
	deps.writeScopeMemoryCap = func(Scope, int64, int64, bool) error { return nil }
	// The incident shape: the peak overshoots the estimated cap, and the kill is
	// this scope's OWN limit (a positive local declaration, which is what
	// confineOwnCapAdviceWarranted requires before this line may claim a cap).
	peak := int64(100 << 20)
	deps.readUsage = func(string) cgroupUsage {
		return cgroupUsage{
			PeakRSS: &peak, OOMKill: int64ptr(1), OOMKillLocal: int64ptr(1),
			OOMGroupKillLocal: int64ptr(1), OOMLocal: int64ptr(1),
		}
	}
	deps.reportPeak = func(context.Context, ConfineRequest, ConfinePeakReport) error { return nil }
	var diagnostics bytes.Buffer
	if _, err := confineWithDeps(context.Background(), ConfineRequest{
		Slice: "finite.slice", Argv: []string{"/bin/true"}, SelfPath: os.Args[0], Stderr: &diagnostics,
	}, deps); err != nil {
		t.Fatalf("confine: %v (diagnostics=%q)", err, diagnostics.String())
	}
	printed := diagnostics.String()
	for _, want := range []string{
		"cap-source=auto:daemon-reserve",
		"this cap is AIRA's OWN AUTO-ESTIMATE",
		"this slice's own cap is 64G",
		"pin --memory-reserve 150M",
	} {
		if !strings.Contains(printed, want) {
			t.Fatalf("the estimated-cap OOM advisory lacks %q: %q", want, printed)
		}
	}
	// False-pass guards. The headroom half must come from the SLICE cap, never
	// from the job's own cap restated, and an estimated cap must never be
	// answered with an operator-pin explanation.
	if strings.Contains(printed, "this slice's own cap is 96M") {
		t.Fatalf("the advisory named the JOB's cap as the slice's, so its headroom claim is a restatement: %q", printed)
	}
	if strings.Contains(printed, "could not be established, so whether the slice had room") {
		t.Fatalf("a slice cap the launch path established was reported as unevaluated: %q", printed)
	}
	if strings.Contains(printed, "YOUR OWN") {
		t.Fatalf("an AIRA-estimated cap was blamed on a flag the caller never passed: %q", printed)
	}
}

// verifies: task-57 a requested confine cap that cannot be written never starts.
func TestConfineScopeMemoryCapFailureDoesNotLaunch(t *testing.T) {
	scope := &confineFakeScope{}
	started := false
	deps := confineUnitDeps(scope)
	deps.writeScopeMemoryCap = func(Scope, int64, int64, bool) error {
		return errors.New("memory.max unavailable")
	}
	deps.start = func(*confineCommand) error {
		started = true
		return nil
	}
	_, err := confineWithDeps(context.Background(), ConfineRequest{
		Slice: "nodeleg.slice", Argv: []string{"must-not-run"}, ScopeMemoryMax: 32 << 20, Stderr: io.Discard,
	}, deps)
	if err == nil || !strings.Contains(err.Error(), "E_CONFINE_UNAVAILABLE") || !strings.Contains(err.Error(), "memory.max") {
		t.Fatalf("error=%v", err)
	}
	if started {
		t.Fatal("target launch reached after scope cap failure")
	}
	if !scope.removed {
		t.Fatal("empty failed scope was not removed")
	}
}

// verifies: task-57 a no-cap confine keeps the existing oom.group behavior but
// does not invoke the new scope-cap writer.
func TestConfineWithoutScopeMemoryCapDoesNotWriteCap(t *testing.T) {
	scope := &confineFakeScope{}
	deps := confineUnitDeps(scope)
	called := false
	deps.writeScopeMemoryCap = func(Scope, int64, int64, bool) error {
		called = true
		return errors.New("must not be called")
	}
	result, err := confineWithDeps(context.Background(), ConfineRequest{
		Slice: "finite.slice", Argv: []string{"/bin/true"}, SelfPath: os.Args[0], Stderr: io.Discard,
	}, deps)
	if err != nil || result.Exit != 0 {
		t.Fatalf("result=%+v err=%v", result, err)
	}
	if called {
		t.Fatal("no-cap confine called the scope memory writer")
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
		deadline := time.Now().Add(testdeadline.Wait(time.Second))
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

func TestConfineRejectedAdmissionCreatesNoScopeAndStartsNoChild(t *testing.T) {
	for _, code := range []string{"E_ADMIT_TOO_LARGE", "E_ADMIT_SATURATED"} {
		t.Run(code, func(t *testing.T) {
			created, started := false, false
			deps := confineDeps{
				resolveSlicePath: func(string) (string, bool, string) { return "/fake/finite.slice", true, "" },
				ensureDelegation: func(string) (confineDelegation, error) { return confineDelegation{}, nil },
				readCap:          func(string) (int64, bool) { return 64 << 30, true },
				newBackend:       func(string) ScopeBackend { return confineCreateTrackingBackend{created: &created} },
				start:            func(*confineCommand) error { started = true; return nil },
			}
			deps.admit = func(ctx context.Context, path string, request ConfineRequest, reserve int64) (admissionResult, error) {
				r := &Runner{memorySlice: path, memoryReserve: reserve, admissionMaxWait: time.Second, pollInterval: time.Millisecond, clock: newInstantClock(), sliceMemory: func(string) (int64, int64, bool, string) { return 0, 64 << 30, true, "" }}
				client, server := net.Pipe()
				r.admitDialFn = func(context.Context, string) (net.Conn, error) { return client, nil }
				go func() {
					defer server.Close()
					var frame runnerAdmitRequestFrame
					_ = readRunnerAdmitFrame(server, &frame)
					rejection := runnerAdmitRejection{Basis: "reject:saturated"}
					if code == "E_ADMIT_TOO_LARGE" {
						rejection = runnerAdmitRejection{Required: 70 << 30, Ceiling: 61 << 30, Basis: "estimate:max=1,n=3,f=115"}
					}
					data, _ := json.Marshal(rejection)
					_ = writeRunnerAdmitFrame(server, runnerAdmitResponseFrame{Code: code, Error: code + ": rejected", Data: data})
				}()
				return r.admit(ctx, Request{ResourceSignature: request.ResourceSignature, DaemonEstimateMemory: true})
			}
			result, err := confineWithDeps(context.Background(), ConfineRequest{Slice: "finite.slice", Argv: []string{"must-not-run"}, Stderr: io.Discard}, deps)
			if err == nil || !strings.Contains(err.Error(), code) || created || started {
				t.Fatalf("result=%+v err=%v created=%v started=%v", result, err, created, started)
			}
			if code == "E_ADMIT_SATURATED" && (!strings.Contains(err.Error(), "slice contended") || !strings.Contains(err.Error(), "reserve")) {
				t.Fatalf("saturated rejection is not explicit: %v", err)
			}
			if result.Status.ReserveBasis != map[string]string{"E_ADMIT_TOO_LARGE": "reject:too-large", "E_ADMIT_SATURATED": "reject:saturated"}[code] {
				t.Fatalf("rejection basis=%q", result.Status.ReserveBasis)
			}
		})
	}
}

// TestConfineDaemonAdmissionSendsNoMaxWait pins the S13 wire change: the confine
// client sends NO max_wait_ms. The admission wait no longer self-expires (design
// §4/§6) — the client blocks until granted, reconnects across a daemon restart, and
// bounds the wait by ctx cancellation, never by a daemon-side timeout. The former
// TestConfineDaemonAdmissionTimeoutUsesRequestedOrDefaultWait (which asserted the
// requested/default wait was propagated to the wire, AIRA-58) is superseded. A
// well-formed saturated rejection is still handled terminally.
func TestConfineDaemonAdmissionSendsNoMaxWait(t *testing.T) {
	for _, test := range []struct {
		name string
		wait time.Duration
		want time.Duration
	}{
		{name: "positive", wait: 25 * time.Millisecond, want: 25 * time.Millisecond},
		{name: "default", want: 30 * time.Minute},
		{name: "large", wait: 2 * time.Hour, want: 2 * time.Hour},
	} {
		t.Run(test.name, func(t *testing.T) {
			socket := filepath.Join(t.TempDir(), "admit.sock")
			listener, err := net.Listen("unix", socket)
			if err != nil {
				t.Fatal(err)
			}
			defer listener.Close()
			frames := make(chan runnerAdmitRequestFrame, 1)
			go func() {
				conn, acceptErr := listener.Accept()
				if acceptErr != nil {
					return
				}
				defer conn.Close()
				var frame runnerAdmitRequestFrame
				if readErr := readRunnerAdmitFrame(conn, &frame); readErr != nil {
					return
				}
				frames <- frame
				data, _ := json.Marshal(runnerAdmitRejection{Basis: "reject:saturated", Ceiling: 8 << 30})
				_ = writeRunnerAdmitFrame(conn, runnerAdmitResponseFrame{Code: "E_ADMIT_SATURATED", Error: "E_ADMIT_SATURATED: capacity remained full", Data: data})
			}()
			deps := confineUnitDeps(&confineFakeScope{})
			deps.admit = admitConfine
			started := time.Now()
			_, err = confineWithDeps(context.Background(), ConfineRequest{
				Slice: "finite.slice", MemoryReserve: 4 << 20, Argv: []string{"must-not-run"},
				AdmitSocketPath: socket, AdmissionMaxWait: test.wait, Stderr: io.Discard,
			}, deps)
			if err == nil || !strings.Contains(err.Error(), "E_ADMIT_SATURATED") {
				t.Fatalf("err=%v, want terminal saturated rejection", err)
			}
			if elapsed := time.Since(started); testdeadline.Exceeded(elapsed, test.want+time.Second) {
				t.Fatalf("saturated daemon rejection took %s, want within requested wait %s", elapsed, test.want)
			}
			select {
			case frame := <-frames:
				if raw, present := frame.Request.Args["max_wait_ms"]; present {
					t.Fatalf("client sent max_wait_ms=%v; S13 sends none — the client blocks/reconnects and bounds the wait by ctx, not a daemon timeout", raw)
				}
			case <-testdeadline.After(time.Second):
				t.Fatal("daemon did not receive admission request")
			}
		})
	}
}

type confineOrderingCloser struct {
	t     *testing.T
	scope *confineFakeScope
	count int
}

func (closer *confineOrderingCloser) Close() error {
	closer.count++
	closer.scope.mu.Lock()
	removed := closer.scope.removed
	closer.scope.mu.Unlock()
	if !removed {
		closer.t.Error("daemon lease released before scope teardown")
	}
	return nil
}

func TestConfineDaemonLeaseHeldUntilScopeTeardown(t *testing.T) {
	scope := &confineFakeScope{}
	closer := &confineOrderingCloser{t: t, scope: scope}
	deps := confineUnitDeps(scope)
	deps.admit = func(context.Context, string, ConfineRequest, int64) (admissionResult, error) {
		return admissionResult{state: "immediate", reserve: 32 << 20, basis: "estimate:max=1,n=3,f=115", release: closer}, nil
	}
	deps.writeScopeMemoryCap = func(Scope, int64, int64, bool) error { return nil }
	deps.readHandshake = func(reader *os.File, timeout time.Duration) ([]byte, error) {
		if closer.count != 0 {
			t.Fatal("daemon lease was released while the started child was at the handshake gate")
		}
		return readConfineHandshake(reader, timeout)
	}
	deps.readUsage = func(string) cgroupUsage {
		if closer.count != 0 {
			t.Fatal("daemon lease was not held for the running lifetime")
		}
		return cgroupUsage{}
	}
	deps.reportPeak = func(context.Context, ConfineRequest, ConfinePeakReport) error { return nil }
	if _, err := confineWithDeps(context.Background(), ConfineRequest{Slice: "finite.slice", Argv: []string{"/bin/true"}, SelfPath: os.Args[0], Stderr: io.Discard}, deps); err != nil {
		t.Fatal(err)
	}
	if closer.count != 1 {
		t.Fatalf("lease closes=%d", closer.count)
	}
}

func TestConfineGrantedReserveIsScopeCapAndPeakIsReported(t *testing.T) {
	scope := &confineFakeScope{}
	closer := &confineCountingCloser{}
	deps := confineUnitDeps(scope)
	deps.admit = func(context.Context, string, ConfineRequest, int64) (admissionResult, error) {
		return admissionResult{state: "immediate", reserve: 96 << 20, basis: "estimate:p90-prior", release: closer}, nil
	}
	var capWritten int64
	deps.writeScopeMemoryCap = func(_ Scope, maximum, high int64, setOOM bool) error {
		capWritten = maximum
		if high != 0 || setOOM {
			t.Fatalf("cap args maximum=%d high=%d oom=%v", maximum, high, setOOM)
		}
		return nil
	}
	peak, oomKill := int64(80<<20), int64(1)
	deps.readUsage = func(string) cgroupUsage { return cgroupUsage{PeakRSS: &peak, OOMKill: &oomKill} }
	reported, reportedBudget := false, false
	deps.reportPeak = func(_ context.Context, _ ConfineRequest, report ConfinePeakReport) error {
		reported = report.Signature != "" && report.Peak != nil && *report.Peak == peak && report.OOM
		// AIRA-180: the budget term travels with the sample, and it is the
		// ENFORCED cap here (96 MiB was written), never a fabricated zero.
		reportedBudget = report.Budget != nil && *report.Budget == 96<<20 &&
			strings.HasPrefix(report.BudgetBasis, ConfineBudgetFamilyCap)
		return nil
	}
	result, err := confineWithDeps(context.Background(), ConfineRequest{Slice: "finite.slice", Argv: []string{"/bin/true"}, SelfPath: os.Args[0], Stderr: io.Discard}, deps)
	if err != nil || capWritten != 96<<20 || result.Status.ScopeMemoryMax != 96<<20 || result.Status.PeakRSS == nil || *result.Status.PeakRSS != peak || !reported || !reportedBudget {
		t.Fatalf("result=%+v err=%v cap=%d reported=%v reportedBudget=%v", result, err, capWritten, reported, reportedBudget)
	}
}

// verifies: AIRA-104 -- CPUUser/CPUSys reach result.Status from the SAME
// deps.readUsage call that already yields PeakRSS, with no second read
// introduced. Also pins that a genuinely-zero CPU reading is preserved
// (unlike PeakRSS's own <=0 clamp a few lines above it in production code):
// a real idle-but-scheduled subtree can read zero user or system usec, and
// that is an observation, not a bad read.
func TestConfineCPUTimeReachesStatusFromTheSameTeardownRead(t *testing.T) {
	scope := &confineFakeScope{}
	deps := confineUnitDeps(scope)
	deps.admit = func(context.Context, string, ConfineRequest, int64) (admissionResult, error) {
		return admissionResult{state: "immediate", reserve: 4 << 30, basis: "fallback:daemon-unavailable", release: &confineCountingCloser{}}, nil
	}
	deps.writeScopeMemoryCap = func(_ Scope, maximum, high int64, setOOM bool) error { return nil }
	user, sys := int64(1_500_000), int64(0)
	deps.readUsage = func(string) cgroupUsage { return cgroupUsage{CPUUser: &user, CPUSys: &sys} }
	deps.reportPeak = func(context.Context, ConfineRequest, ConfinePeakReport) error { return nil }
	result, err := confineWithDeps(context.Background(), ConfineRequest{Slice: "finite.slice", Argv: []string{"/bin/true"}, SelfPath: os.Args[0], Stderr: io.Discard}, deps)
	if err != nil {
		t.Fatal(err)
	}
	if result.Status.CPUUser == nil || *result.Status.CPUUser != user {
		t.Fatalf("CPUUser did not reach result.Status: %+v", result.Status)
	}
	if result.Status.CPUSys == nil || *result.Status.CPUSys != 0 {
		t.Fatalf("a genuine zero CPUSys must be preserved, not dropped: %+v", result.Status)
	}
}

// S2a Task 7 (spec §4/§16): a `--delegate-ram` job is an ordinary confine job, so
// its scope memory.max comes from the ordinary daemon-reserve grant (or an explicit
// --memory-max), NEVER a delegate-specific 48 GiB ceiling. The old
// TestConfineDelegateRAMAlwaysUsesCeilingCap encoded the retired ceiling model.
func TestConfineDelegateRAMTakesOrdinaryScopeCap(t *testing.T) {
	t.Run("unpinned delegate takes the daemon reserve as its cap, not a delegate ceiling", func(t *testing.T) {
		scope := &confineFakeScope{}
		deps := confineUnitDeps(scope)
		closer := &confineCountingCloser{}
		var gotReserve int64
		var gotPinned bool
		deps.admit = func(_ context.Context, _ string, request ConfineRequest, reserve int64) (admissionResult, error) {
			gotReserve, gotPinned = reserve, request.MemoryReservePinned
			// An ordinary admitted grant: a history estimate, unpinned.
			return admissionResult{state: "immediate", reserve: 2 << 30, basis: "estimate:p90-prior", release: closer}, nil
		}
		var written int64
		deps.writeScopeMemoryCap = func(_ Scope, maximum, high int64, setOOM bool) error {
			written = maximum
			if high != 0 || setOOM {
				t.Fatalf("cap args maximum=%d high=%d oom=%v", maximum, high, setOOM)
			}
			return nil
		}
		result, err := confineWithDeps(context.Background(), ConfineRequest{
			Slice: "finite.slice", DelegateRAM: true, Argv: []string{"/bin/true"}, SelfPath: os.Args[0], Stderr: io.Discard,
		}, deps)
		if err != nil {
			t.Fatal(err)
		}
		// The whole-job charge is the ORDINARY unpinned no-history default (not a
		// pinned framework overhead), and the scope cap is the daemon-granted reserve.
		if gotReserve != DefaultConfineMemoryReserve || gotPinned {
			t.Fatalf("delegate no-reserve charged %d pinned=%v, want %d unpinned", gotReserve, gotPinned, DefaultConfineMemoryReserve)
		}
		if written != 2<<30 || result.Status.ScopeMemoryMax != 2<<30 || result.Status.ScopeMemoryCapSource != ConfineCapSourceDaemonReserve {
			t.Fatalf("cap=%d ScopeMemoryMax=%d source=%q, want 2G/daemon-reserve", written, result.Status.ScopeMemoryMax, result.Status.ScopeMemoryCapSource)
		}
	})

	t.Run("delegate --memory-max sets the cap AND is charged as the reserve, like any confine job", func(t *testing.T) {
		scope := &confineFakeScope{}
		deps := confineUnitDeps(scope)
		var admittedReserve, written int64
		deps.admit = func(_ context.Context, _ string, _ ConfineRequest, reserve int64) (admissionResult, error) {
			admittedReserve = reserve
			return admissionResult{state: "immediate", reserve: reserve, basis: "pinned:client", release: &confineCountingCloser{}}, nil
		}
		deps.writeScopeMemoryCap = func(_ Scope, maximum, _ int64, _ bool) error { written = maximum; return nil }
		result, err := confineWithDeps(context.Background(), ConfineRequest{
			Slice: "finite.slice", DelegateRAM: true, ScopeMemoryMax: 2 << 30, Argv: []string{"/bin/true"}, SelfPath: os.Args[0], Stderr: io.Discard,
		}, deps)
		if err != nil {
			t.Fatal(err)
		}
		// The retired `--delegate-ram --memory-reserve 512M` idiom: --memory-max now
		// SETS the reserve to the cap, exactly as on a non-delegate job.
		if admittedReserve != 2<<30 || written != 2<<30 || result.Status.ScopeMemoryCapSource != ConfineCapSourceMemoryMax {
			t.Fatalf("reserve=%d cap=%d source=%q, want 2G/2G/memory-max", admittedReserve, written, result.Status.ScopeMemoryCapSource)
		}
	})

	t.Run("finite cap remains a precondition", func(t *testing.T) {
		admitted, oomWritten, started := false, false, false
		deps := confineUnitDeps(&confineFakeScope{})
		deps.readCap = func(string) (int64, bool) { return 0, false }
		deps.admit = func(context.Context, string, ConfineRequest, int64) (admissionResult, error) {
			admitted = true
			return admissionResult{}, nil
		}
		deps.writeOOMGroup = func(Scope) error { oomWritten = true; return nil }
		deps.start = func(*confineCommand) error { started = true; return nil }
		_, err := confineWithDeps(context.Background(), ConfineRequest{
			Slice: "finite.slice", DelegateRAM: true, Argv: []string{"must-not-run"}, Stderr: io.Discard,
		}, deps)
		if err == nil || !strings.Contains(err.Error(), "uncapped") || admitted || oomWritten || started {
			t.Fatalf("err=%v admitted=%v oomWritten=%v started=%v", err, admitted, oomWritten, started)
		}
	})
}

// verifies: a daemon grant whose admission could not be evaluated (state
// "unevaluated" — the daemon answered but the slice's live usage was unreadable)
// carries only the flat fallback reserve and was never accounted, so it must NOT
// become a hard scope memory.max sub-cap. Sub-capping it at the flat 4 GiB would
// false-fail a heavy job although admission was never actually evaluated (design
// §6: that basis ⇒ no sub-cap). Only an immediate/waited (accounted) grant caps.
func TestConfineUnevaluatedDaemonGrantIsNotSubCapped(t *testing.T) {
	scope := &confineFakeScope{}
	closer := &confineCountingCloser{}
	deps := confineUnitDeps(scope)
	deps.admit = func(context.Context, string, ConfineRequest, int64) (admissionResult, error) {
		return admissionResult{state: "unevaluated", reserve: 4 << 30, basis: "fallback:slice-unreadable", release: closer}, nil
	}
	deps.writeScopeMemoryCap = func(Scope, int64, int64, bool) error {
		t.Fatalf("unevaluated daemon grant must not write a scope memory cap")
		return nil
	}
	deps.readUsage = func(string) cgroupUsage { return cgroupUsage{} }
	deps.reportPeak = func(context.Context, ConfineRequest, ConfinePeakReport) error { return nil }
	result, err := confineWithDeps(context.Background(), ConfineRequest{Slice: "finite.slice", Argv: []string{"/bin/true"}, SelfPath: os.Args[0], Stderr: io.Discard}, deps)
	if err != nil || result.Status.ScopeMemoryMax != 0 {
		t.Fatalf("result=%+v err=%v, want no sub-cap for an unevaluated grant", result, err)
	}
}

func TestConfineZeroPeakIsReportedAsUnknown(t *testing.T) {
	scope := &confineFakeScope{}
	deps := confineUnitDeps(scope)
	zero := int64(0)
	deps.readUsage = func(string) cgroupUsage { return cgroupUsage{PeakRSS: &zero} }
	reportedUnknown := false
	deps.reportPeak = func(_ context.Context, _ ConfineRequest, report ConfinePeakReport) error {
		reportedUnknown = report.Peak == nil
		return nil
	}
	result, err := confineWithDeps(context.Background(), ConfineRequest{Slice: "finite.slice", Argv: []string{"/bin/true"}, SelfPath: os.Args[0], Stderr: io.Discard}, deps)
	if err != nil || result.Status.PeakRSS != nil || !reportedUnknown {
		t.Fatalf("result=%+v err=%v reportedUnknown=%v", result, err, reportedUnknown)
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

// S13 removed the flock "timeout" admission state. An UNEVALUATED admission (the
// no-daemon case, or a slice AIRA could not read) is now the state that launches
// ungoverned-but-warned, so this test pins the same facet-honesty property on it:
// the job still launches, its exit code passes through, and the trailer reports
// the mix honestly (cap enforced, admission unevaluated, priorities unverified
// after the forced handshake failure).
func TestConfineUnevaluatedAdmissionStillLaunchesAndReportsFacetMix(t *testing.T) {
	scope := &confineFakeScope{}
	deps := confineUnitDeps(scope)
	deps.admit = func(context.Context, string, ConfineRequest, int64) (admissionResult, error) {
		return admissionResult{state: "unevaluated", reason: "no-daemon"}, nil
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
	if result.Exit != 23 || result.Status.Admission != ConfineAdmissionUnevaluated || result.Status.Priorities != ConfinePrioritiesUnverified {
		t.Fatalf("result=%+v stderr=%q", result, stderr.String())
	}
	if !scope.started || !strings.Contains(stderr.String(), "cap=enforced") || !strings.Contains(stderr.String(), "admission=unevaluated") || !strings.Contains(stderr.String(), "priorities=unverified") || strings.Contains(stderr.String(), "priorities=applied") {
		t.Fatalf("unevaluated launch/status dishonest: scope=%+v stderr=%q", scope, stderr.String())
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

func TestConfineDelegateRAMSetupAppliesOOMScoreAdjAndInherits(t *testing.T) {
	t.Setenv("AIRA_CONFINE_OOM_SCORE_ADJ", "")
	scope := &confineFakeScope{}
	deps := confineUnitDeps(scope)
	deps.writeScopeMemoryCap = func(Scope, int64, int64, bool) error { return nil }
	var stdout bytes.Buffer
	result, err := confineWithDeps(context.Background(), ConfineRequest{
		Slice: "finite.slice", DelegateRAM: true,
		Argv:     []string{"/bin/sh", "-c", `cat /proc/self/oom_score_adj; /bin/sh -c 'cat /proc/self/oom_score_adj'`},
		SelfPath: os.Args[0], Stdout: &stdout, Stderr: io.Discard,
	}, deps)
	if err != nil || result.Exit != 0 || result.Status.Priorities != ConfinePrioritiesApplied {
		t.Fatalf("result=%+v err=%v", result, err)
	}
	// S2a: a delegate job is an ordinary confine job, so leader and child carry the
	// single confine-class baseline (500), not the retired delegate 800.
	if fields := strings.Fields(stdout.String()); !reflect.DeepEqual(fields, []string{"500", "500"}) {
		t.Fatalf("delegate oom_score_adj leader/child=%q", stdout.String())
	}
}

// S2a: a single confine oom-class. confineSetupArgv honours the one
// AIRA_CONFINE_OOM_SCORE_ADJ override, and an unparseable/out-of-range value is a
// clear rejection rather than a silent fallback. There is no delegate env or
// class-ordering invariant any more.
func TestConfineSetupArgvOOMScoreAdjOverridesAndRejection(t *testing.T) {
	t.Run("a valid override is applied", func(t *testing.T) {
		t.Setenv("AIRA_CONFINE_OOM_SCORE_ADJ", "600")
		argv, err := confineSetupArgv([]string{"/bin/true"})
		if err != nil {
			t.Fatal(err)
		}
		_, _, oomAdj, _, _, _, err := parseConfineSetupArgs(argv[1:])
		if err != nil || oomAdj != 600 {
			t.Fatalf("argv=%q oom_score_adj=%d err=%v, want 600", argv, oomAdj, err)
		}
	})

	for _, test := range []struct {
		name, override, want string
	}{
		{name: "non-integer", override: "not-an-integer", want: "AIRA_CONFINE_OOM_SCORE_ADJ"},
		{name: "below desktop floor", override: "499", want: "AIRA_CONFINE_OOM_SCORE_ADJ"},
		{name: "above kernel maximum", override: "1001", want: "AIRA_CONFINE_OOM_SCORE_ADJ"},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Setenv("AIRA_CONFINE_OOM_SCORE_ADJ", test.override)
			if _, err := confineSetupArgv([]string{"/bin/true"}); err == nil || !strings.Contains(err.Error(), "E_CONFINE_ARGUMENT_INVALID") || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("argv build error=%v, want clear rejection containing %q", err, test.want)
			}
		})
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

// TestConfineExportsTheScopeIDWithoutARuntimeDir pins BOTH halves of what
// AIRA-33 left of the confine child environment.
//
// It replaces TestConfineInjectsDaemonGovernorEnvironment, which asserted
// AIRA_CONFINE_SCOPE_ID and AIRA_GOVERNOR_CMD together. The second is deleted;
// the first is not, and it is load-bearing: runner.InheritedConfineScopeID reads
// it and confine-reserve uses it as ParentScopeID, which is what stops a
// sub-reservation being charged to the slice as new work.
//
// The RuntimeDir-less launch is the POINT, not incidental. Before AIRA-33 the
// scope id was published by a helper that returned early when RuntimeDir was
// empty (the sidecar had nowhere to be extracted to), so this launch shape
// silently exported nothing. Removing the extraction removed that unrelated
// gate. Asserting it here is what keeps the improvement from being re-broken by
// someone re-introducing a RuntimeDir precondition.
//
// verifies: AIRA-33
func TestConfineExportsTheScopeIDWithoutARuntimeDir(t *testing.T) {
	scope := &confineFakeScope{}
	var stdout bytes.Buffer
	deps := confineUnitDeps(scope)
	// DelegateRAM triggers the #67/AIRA-15 scope-cap write; the fake scope has no
	// real memory.max fd, so stub it as the other delegate-ram unit tests do.
	deps.writeScopeMemoryCap = func(Scope, int64, int64, bool) error { return nil }
	result, err := confineWithDeps(context.Background(), ConfineRequest{
		Slice: "finite.slice", DelegateRAM: true, Name: "pytest",
		Argv:     []string{"/bin/sh", "-c", "printf '%s|%s' \"$AIRA_CONFINE_SCOPE_ID\" \"$AIRA_GOVERNOR_CMD\""},
		Env:      []string{"PATH=" + os.Getenv("PATH"), "AIRA_GOVERNOR_CMD=/stale/aira"},
		SelfPath: os.Args[0], Stdout: &stdout, Stderr: io.Discard,
	}, deps)
	if err != nil || result.Exit != 0 {
		t.Fatalf("result=%+v err=%v", result, err)
	}
	parts := strings.Split(stdout.String(), "|")
	if len(parts) != 2 {
		t.Fatalf("child environment report=%q", stdout.String())
	}
	if parts[0] == "" {
		t.Fatal("AIRA_CONFINE_SCOPE_ID absent on a RuntimeDir-less launch: confine-reserve's ParentScopeID depends on it")
	}
	if parts[1] != "" {
		t.Fatalf("retired AIRA_GOVERNOR_CMD survived the strip as %q", parts[1])
	}
}

func TestConfineNonDelegateLaunchStripsInheritedAitestEnvironment(t *testing.T) {
	// Regression test for a real leak (Fable build-review, final gate):
	// AppendAitestChildEnvironment (which strips stale AIRA_AITEST_*
	// coordinates before optionally re-adding fresh ones) used to be
	// called ONLY inside the `if request.DelegateRAM` branch, so a
	// non-delegate launch's cmd.Env kept whatever AIRA_AITEST_* it
	// inherited from its own parent process untouched -- e.g. a shell or
	// test inside a delegate-RAM aitest job launching `aira confine --
	// ...` without --delegate-ram would hand its child stale coordinates
	// pointing at the outer job's (possibly since-deleted) extraction dir
	// and relay binary.
	scope := &confineFakeScope{}
	var stdout bytes.Buffer
	result, err := confineWithDeps(context.Background(), ConfineRequest{
		Slice: "finite.slice", DelegateRAM: false,
		Env:      []string{"PATH=" + os.Getenv("PATH"), "AIRA_AITEST_LIB=/stale/path/from/parent"},
		Argv:     []string{"/bin/sh", "-c", "printf 'lib=[%s]' \"$AIRA_AITEST_LIB\""},
		SelfPath: os.Args[0], Stdout: &stdout, Stderr: io.Discard,
	}, confineUnitDeps(scope))
	if err != nil || result.Exit != 0 {
		t.Fatalf("result=%+v err=%v", result, err)
	}
	if got := stdout.String(); got != "lib=[]" {
		t.Fatalf("child saw AIRA_AITEST_LIB=%q, want it stripped on a non-delegate launch", got)
	}
}

// aitestCoordinateKeys is the full set of coordinates a supervisor needs to
// reach worker-admit; a partial check would let three of four leak.
var aitestCoordinateKeys = []string{
	"AIRA_AITEST_LIB",
	"AIRA_AITEST_WORKER_ADMIT_CMD",
	"AIRA_AITEST_ADMISSION",
	"AIRA_AITEST_MAX_WORKERS_FALLBACK",
	"AIRA_AITEST_OUTER_SCOPE",
}

// reportChildEnv builds a shell command printing each key's value, "|"-joined
// in order, so one confined launch reports a whole environment slice.
func reportChildEnv(keys ...string) []string {
	verbs := make([]string, len(keys))
	args := make([]string, len(keys))
	for i, key := range keys {
		verbs[i] = "%s"
		args[i] = `"$` + key + `"`
	}
	return []string{"/bin/sh", "-c", "printf '" + strings.Join(verbs, "|") + "' " + strings.Join(args, " ")}
}

// TestConfineNonDelegateWithPopulatedRuntimeDirDeliversNoAitestCoordinates
// pins the contract AIRA-71's wrong documentation claimed to satisfy: a PLAIN
// `aira confine -- pytest --aitest-workers=auto` delivers aitest nothing.
//
// This is deliberately NOT a duplicate of
// TestConfineNonDelegateLaunchStripsInheritedAitestEnvironment above. That one
// passes RuntimeDir:"", and AppendAitestChildEnvironment early-returns right
// after stripping when runtimeDir is empty (internal/pylib/env.go:53) -- so it
// passes identically whether the DelegateRAM gate at confine_linux.go:757
// exists or is removed. It cannot distinguish the two candidate fixes for
// AIRA-71, and therefore cannot pin this contract. With a POPULATED
// RuntimeDir, removing that gate makes this test fail.
func TestConfineNonDelegateWithPopulatedRuntimeDirDeliversNoAitestCoordinates(t *testing.T) {
	dataHome := t.TempDir()
	t.Setenv("XDG_DATA_HOME", dataHome)
	scope := &confineFakeScope{}
	var stdout bytes.Buffer
	result, err := confineWithDeps(context.Background(), ConfineRequest{
		Slice: "finite.slice", DelegateRAM: false, Name: "pytest",
		Env: []string{
			"PATH=" + os.Getenv("PATH"),
			"XDG_DATA_HOME=" + dataHome,
			// Stale coordinates as an outer delegate-ram aitest job would leak
			// them into a nested plain `aira confine` (the leak the
			// StripAitestEnvironment call on the non-delegate branch of
			// confineWithDeps exists to stop). They must not survive, and must
			// not be replaced by fresh ones either.
			"AIRA_AITEST_LIB=/stale/lib",
			"AIRA_AITEST_WORKER_ADMIT_CMD=/stale/aira",
			"AIRA_AITEST_ADMISSION=stale-grade",
			"AIRA_AITEST_MAX_WORKERS_FALLBACK=999",
			"AIRA_AITEST_OUTER_SCOPE=/stale/scope",
		},
		Argv:       reportChildEnv(append(append([]string{}, aitestCoordinateKeys...), "PATH", "AIRA_CONFINE_SCOPE_ID")...),
		RuntimeDir: t.TempDir(), SelfPath: os.Args[0], Stdout: &stdout, Stderr: io.Discard,
	}, confineUnitDeps(scope))
	if err != nil || result.Exit != 0 {
		t.Fatalf("result=%+v err=%v", result, err)
	}
	fields := strings.Split(stdout.String(), "|")
	if len(fields) != len(aitestCoordinateKeys)+2 {
		t.Fatalf("child environment report=%q", stdout.String())
	}
	for i, key := range aitestCoordinateKeys {
		if fields[i] != "" {
			t.Fatalf("non-delegate launch delivered %s=%q, want empty", key, fields[i])
		}
	}
	// Anti-porosity: prove the child environment was populated at all. A launch
	// that handed the child an empty environment would satisfy every absence
	// assertion above for entirely the wrong reason. (Before AIRA-33 this
	// witness was AIRA_PY_LIB, which the same launch used to export; PATH is the
	// surviving proof-of-life now that it does not.)
	if fields[len(aitestCoordinateKeys)] == "" {
		t.Fatal("PATH absent: the child environment was empty, so the aitest-coordinate assertions above prove nothing")
	}
	if fields[len(aitestCoordinateKeys)+1] == "" {
		t.Fatal("AIRA_CONFINE_SCOPE_ID absent: the confine child environment was not populated")
	}
}

// TestConfineDelegateRAMDeliversAitestCoordinates is the positive half of the
// pair: --delegate-ram is what actually wires aitest, so SKILL.md must
// recommend it (AIRA-71). Together the two tests bracket the gate in both the
// false-fail and false-pass directions.
func TestConfineDelegateRAMDeliversAitestCoordinates(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	scope := &confineFakeScope{}
	var stdout bytes.Buffer
	deps := confineUnitDeps(scope)
	// DelegateRAM triggers the #67/AIRA-15 scope-cap write; the fake scope has
	// no real memory.max fd, so stub it as the other delegate-ram unit tests do.
	deps.writeScopeMemoryCap = func(Scope, int64, int64, bool) error { return nil }
	result, err := confineWithDeps(context.Background(), ConfineRequest{
		Slice: "finite.slice", DelegateRAM: true, Name: "pytest",
		Argv:       reportChildEnv(aitestCoordinateKeys...),
		RuntimeDir: t.TempDir(), SelfPath: os.Args[0], Stdout: &stdout, Stderr: io.Discard,
	}, deps)
	if err != nil || result.Exit != 0 {
		t.Fatalf("result=%+v err=%v", result, err)
	}
	fields := strings.Split(stdout.String(), "|")
	if len(fields) != len(aitestCoordinateKeys) {
		t.Fatalf("child environment report=%q", stdout.String())
	}
	for i, key := range aitestCoordinateKeys {
		if fields[i] == "" {
			t.Fatalf("delegate-ram launch delivered no %s", key)
		}
	}
	// The coordinate must point at a real extracted tree, not merely be set:
	// a supervisor cannot import a plugin from a path that does not exist.
	if _, err := os.Stat(filepath.Join(fields[0], "aitest", "__init__.py")); err != nil {
		t.Fatalf("AIRA_AITEST_LIB=%q is not a real extracted aitest tree: %v", fields[0], err)
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
		resolveSlicePath:    func(string) (string, bool, string) { return "/fake/finite.slice", true, "" },
		ensureDelegation:    func(string) (confineDelegation, error) { return confineDelegation{cpuWeight: true}, nil },
		writeScopeCPUWeight: func(Scope, int64) bool { return true },
		newBackend:          func(string) ScopeBackend { return confineFakeBackend{scope: scope} },
		admit: func(context.Context, string, ConfineRequest, int64) (admissionResult, error) {
			return admissionResult{state: "immediate"}, nil
		},
		writeOOMGroup: func(Scope) error { return nil },
		// A fake scope has no cgroup FD, so the production swap-cap writer would
		// fail closed on EBADF here. Stubbed to the disposition a real capped
		// scope reports; TestConfineRealScopeBoundsSwapToZero exercises the real
		// writer against a real cgroup.
		writeScopeSwapCap: func(Scope) (string, error) { return WorkerAdmitSwapCapEnforced, nil },
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

func mustConfineSetupArgv(t *testing.T, target []string) []string {
	t.Helper()
	argv, err := confineSetupArgv(target)
	if err != nil {
		t.Fatal(err)
	}
	return argv
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
	live := s.members[:0]
	for _, pid := range s.members {
		if processStartTick(pid) != 0 {
			live = append(live, pid)
		}
	}
	s.members = live
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

// verifies: the optional decay writer is FD-anchored and a scope that vanishes
// before a timer tick is harmless. Replacing it with the fail-closed memory
// writer would turn this teardown race into a launch/lifecycle failure.
func TestWriteScopeCPUWeightFailOpenToleratesRemovedScope(t *testing.T) {
	dirPath := t.TempDir()
	dir, err := os.Open(dirPath)
	if err != nil {
		t.Fatal(err)
	}
	defer dir.Close()
	if err := os.Remove(dirPath); err != nil {
		t.Fatal(err)
	}
	reference := t.TempDir()
	sentinel := filepath.Join(reference, "cpu.weight")
	if err := os.WriteFile(sentinel, []byte("777\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	scope := &confineReferenceMismatchScope{confineFakeScope: confineFakeScope{}, fd: int(dir.Fd()), reference: reference}
	if writeScopeCPUWeightFailOpen(scope, 10) {
		t.Fatal("removed scope unexpectedly accepted cpu.weight write")
	}
	if data, err := os.ReadFile(sentinel); err != nil || string(data) != "777\n" {
		t.Fatalf("reference sentinel changed to %q (err=%v): writer followed Reference instead of scope FD", data, err)
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
	ctx, cancel := context.WithTimeout(context.Background(), testdeadline.Wait(2*time.Second))
	defer cancel()
	cmd := exec.CommandContext(ctx, os.Args[0], mustConfineSetupArgv(t, []string{
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
	ctx, cancel := context.WithTimeout(context.Background(), testdeadline.Wait(2*time.Second))
	defer cancel()
	cmd := exec.CommandContext(ctx, os.Args[0], mustConfineSetupArgv(t, []string{
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
	ctx, cancel := context.WithTimeout(context.Background(), testdeadline.Wait(2*time.Second))
	defer cancel()
	cmd := exec.CommandContext(ctx, os.Args[0], mustConfineSetupArgv(t, []string{
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
	if _, ok := effectiveConfineCap(parent); !ok {
		// These real-setup assertions (priority handshake, delegation repair) only
		// hold when the ambient ancestry is capped — i.e. the suite runs under
		// `aira confine`. Bare (uncapped) sessions can't exercise them; skip
		// normally, hard-fail only under mandatory-real mode (AIRA_REAL_CGROUP=1).
		cgrouptest.SkipOrFailRealCgroup(t, "confine real setup requires a capped cgroup ancestor (run under `aira confine`); parent %s is uncapped", parent)
	}
	backend := newDefaultBackend(parent)
	if err := backend.Probe(context.Background()); err != nil {
		cgrouptest.SkipOrFailRealCgroup(t, "real setup backend probe: %v", err)
	}
	scope, err := backend.Create(context.Background(), confineScopeID("setup-test", ""))
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
	parent := confineMemoryParent(t, "134217728")
	marker := filepath.Join(t.TempDir(), "survived")
	observation := &confineScopeObservation{}
	deps := defaultConfineDeps()
	deps.newBackend = func(path string) ScopeBackend {
		return confineObservingBackend{ScopeBackend: newDefaultBackend(path), observation: observation}
	}
	result, err := confineWithDeps(context.Background(), ConfineRequest{
		Slice: parent, MemoryReserve: 1 << 20, ScopeMemoryMax: 32 << 20, AdmissionMaxWait: 2 * time.Second, PollInterval: 10 * time.Millisecond,
		Argv:     []string{"/bin/sh", "-c", `(sleep 2; echo survived > "$1") & python3 -c 'x=bytearray(256*1024*1024); x[-1]=1'; wait`, "sh", marker},
		SelfPath: os.Args[0], Stderr: io.Discard,
	}, deps)
	if err != nil {
		cgrouptest.SkipOrFailRealCgroup(t, "confine real OOM fixture unavailable: %v", err)
	}
	if result.Exit != 137 || result.Status.ScopeMemoryMax != 32<<20 || result.Status.PeakRSS == nil {
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
	t.Setenv("AIRA_CONFINE_OOM_SCORE_ADJ", "")
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

// verifies: a daemonless delegate-ram confine selects the higher setup value
// and its descendant inherits it. The fallback cap keeps this real-cgroup test
// independent of daemon admission.
func TestConfineRealDelegateRAMPrioritiesUnderCappedSlice(t *testing.T) {
	t.Setenv("AIRA_CONFINE_OOM_SCORE_ADJ", "")
	parent := confineMemoryParent(t, "67108864")
	deps := defaultConfineDeps()
	deps.admit = func(context.Context, string, ConfineRequest, int64) (admissionResult, error) {
		return admissionResult{state: "unevaluated"}, nil
	}
	var stdout, stderr bytes.Buffer
	result, err := confineWithDeps(context.Background(), ConfineRequest{
		Slice: parent, DelegateRAM: true, MemoryReserve: 1 << 20,
		Argv:     []string{"/bin/sh", "-c", `cat /proc/self/oom_score_adj; /bin/sh -c 'cat /proc/self/oom_score_adj'`},
		SelfPath: os.Args[0], Stdout: &stdout, Stderr: &stderr,
	}, deps)
	if err != nil {
		cgrouptest.SkipOrFailRealCgroup(t, "delegate-ram confine real priority fixture unavailable: %v", err)
	}
	if result.Exit != 0 || result.Status.Cap != ConfineCapEnforced || result.Status.Scope != ConfineScopePlaced || result.Status.OOMGroup != ConfineOOMGroupSet || result.Status.Priorities != ConfinePrioritiesApplied {
		t.Fatalf("result=%+v stdout=%q stderr=%q", result, stdout.String(), stderr.String())
	}
	// S2a: a delegate job is an ordinary confine job, so leader and child carry the
	// single confine-class baseline (500), not the retired delegate 800.
	if fields := strings.Fields(stdout.String()); !reflect.DeepEqual(fields, []string{"500", "500"}) {
		t.Fatalf("delegate oom_score_adj leader/child=%q", stdout.String())
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

// TestConfineRealAdmissionWaitsThenProceedsDaemonDown was removed in S13. It pinned
// the flock fallback's end-to-end behavior — a confine launch with a down daemon
// WAITING on raw slice memory and then PROCEEDING (AdmissionState "waited"/"immediate")
// without any daemon. S13 deletes the flock fallback: a configured-but-unreachable
// daemon now makes the client RECONNECT indefinitely (fail closed by waiting), never
// proceed ungoverned (design §4/§6). The new reconnect/terminal/block behavior is
// pinned by the admission_linux_test.go S13 tests; the real restart-under-load merge
// gate (S13 exit) covers the live reconnect + re-declare.

func TestConfineRealMissingSubtreeDelegationIsRepairedBeforeLaunch(t *testing.T) {
	parent := cgrouptest.IsolatedScopeParent(t)
	if _, ok := effectiveConfineCap(parent); !ok {
		// Delegation-repair can only be exercised with a capped ancestor (confine
		// refuses to launch into an uncapped slice). Skip in bare sessions; the
		// suite exercises this under `aira confine`.
		cgrouptest.SkipOrFailRealCgroup(t, "subtree-delegation-repair requires a capped cgroup ancestor (run under `aira confine`); parent %s is uncapped", parent)
	}
	marker := filepath.Join(t.TempDir(), "ran")
	result, err := Confine(context.Background(), ConfineRequest{
		Slice: parent, MemoryReserve: 1, Argv: []string{"/bin/sh", "-c", "echo ran > \"$1\"", "sh", marker},
		SelfPath: os.Args[0], Stderr: io.Discard,
	})
	if err != nil || result.Exit != 0 {
		t.Fatalf("confine did not repair delegation: result=%+v err=%v", result, err)
	}
	if _, statErr := os.Stat(marker); statErr != nil {
		t.Fatalf("confined target did not run: %v", statErr)
	}
	if data, readErr := os.ReadFile(filepath.Join(parent, "cgroup.subtree_control")); readErr != nil || !confineHasToken(data, "memory") {
		t.Fatalf("memory delegation was not repaired: %q err=%v", data, readErr)
	}
}

// verifies: after the per-launch delegation repair, a fresh real scope has
// cpu.weight=100 and honestly reports aging. This mitigates contention with
// long-running, decayed scopes; simultaneous fresh scopes remain Slice 2.
func TestConfineRealCPUWeightStartsAging(t *testing.T) {
	parent := confineMemoryParent(t, "134217728")
	controllers, err := os.ReadFile(filepath.Join(parent, "cgroup.controllers"))
	if err != nil || !confineHasToken(controllers, "cpu") {
		cgrouptest.SkipOrFailRealCgroup(t, "cpu controller unavailable to %s: %q err=%v", parent, controllers, err)
	}
	// Start from a parent where cpu is deliberately not delegated. A fresh child
	// must not expose cpu.weight until this launch's ensureConfineDelegation
	// repairs +cpu; otherwise this test would pass with the repair removed.
	if err := os.WriteFile(filepath.Join(parent, "cgroup.subtree_control"), []byte("-cpu"), 0o644); err != nil {
		cgrouptest.SkipOrFailRealCgroup(t, "cannot de-delegate cpu from %s: %v", parent, err)
	}
	wouldBeScope := filepath.Join(parent, "without-cpu")
	if err := os.Mkdir(wouldBeScope, 0o755); err != nil {
		cgrouptest.SkipOrFailRealCgroup(t, "cannot create undelegated cpu probe scope: %v", err)
	}
	if _, err := os.Stat(filepath.Join(wouldBeScope, "cpu.weight")); !errors.Is(err, fs.ErrNotExist) {
		_ = os.Remove(wouldBeScope)
		t.Fatalf("cpu.weight exists before +cpu repair (err=%v)", err)
	}
	if err := os.Remove(wouldBeScope); err != nil {
		t.Fatal(err)
	}
	observation := &confineScopeObservation{}
	deps := defaultConfineDeps()
	deps.newBackend = func(path string) ScopeBackend {
		return confineObservingBackend{ScopeBackend: newDefaultBackend(path), observation: observation}
	}
	result, err := confineWithDeps(context.Background(), ConfineRequest{
		Slice: parent, MemoryReserve: 1, Argv: []string{"/bin/true"}, SelfPath: os.Args[0], Stderr: io.Discard,
	}, deps)
	if err != nil {
		cgrouptest.SkipOrFailRealCgroup(t, "CPU-weight confine fixture unavailable: %v", err)
	}
	if result.Exit != 0 || result.Status.CPUWeight != ConfineCPUWeightAging || observation.cpuWeight != "100" {
		t.Fatalf("result=%+v cpu.weight=%q", result, observation.cpuWeight)
	}
}

type confineScopeObservation struct {
	oomGroup  string
	cpuWeight string
	swapMax   string
}

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
	if data, err := os.ReadFile(filepath.Join(scope.Reference(), "cpu.weight")); err == nil {
		scope.observation.cpuWeight = strings.TrimSpace(string(data))
	}
	// AIRA-110: read while the scope still exists. It is torn down here, so a
	// test that waited until Confine returned could only assert on a directory
	// that is already gone.
	if data, err := os.ReadFile(filepath.Join(scope.Reference(), "memory.swap.max")); err == nil {
		scope.observation.swapMax = strings.TrimSpace(string(data))
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

// A blocked admission must emit a periodic "waiting for memory admission"
// diagnostic so a legitimate reserve-contended wait (queued behind other
// sessions' in-flight jobs under the shared slice cap) is never mistaken for a
// hang. RED against a Confine that blocks on admit without any progress output.
func TestConfineAdmissionWaitEmitsProgressDiagnostic(t *testing.T) {
	var mu sync.Mutex
	var buf bytes.Buffer
	sink := writerFunc(func(p []byte) (int, error) {
		mu.Lock()
		defer mu.Unlock()
		return buf.Write(p)
	})
	proceed := make(chan struct{})
	scope := &confineFakeScope{}
	deps := confineUnitDeps(scope)
	deps.admitWaitDiagInterval = 5 * time.Millisecond
	deps.admit = func(context.Context, string, ConfineRequest, int64) (admissionResult, error) {
		<-proceed // block, as a reserve-contended daemon wait would
		return admissionResult{state: "immediate"}, nil
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _ = confineWithDeps(context.Background(), ConfineRequest{
			Slice: "finite.slice", MemoryReserve: 4 << 30, Argv: []string{"/bin/true"},
			SelfPath: os.Args[0], Stderr: sink,
		}, deps)
	}()
	deadline := time.Now().Add(testdeadline.Wait(3 * time.Second))
	seen := false
	for time.Now().Before(deadline) {
		mu.Lock()
		got := buf.String()
		mu.Unlock()
		if strings.Contains(got, "waiting for memory admission") {
			seen = true
			break
		}
		time.Sleep(2 * time.Millisecond)
	}
	close(proceed)
	<-done
	if !seen {
		t.Fatalf("no admission-wait progress diagnostic emitted; stderr=%q", buf.String())
	}
	got := buf.String()
	if !strings.Contains(got, "reserve 4G") {
		t.Fatalf("pinned request must keep the exact granted-figure wording; stderr=%q", got)
	}
	if strings.Contains(got, "unpinned") {
		t.Fatalf("pinned request must not carry the unpinned hedge; stderr=%q", got)
	}
}

// AIRA-51: an UNPINNED request's admission-wait line must not present the
// client's no-history fallback hint (DefaultConfineMemoryReserve) as if it
// were the reserve the daemon is actually contending over — the daemon
// resolves the real, admission-gating reserve from history/estimation before
// queueing and can grant a wildly different figure (observed ~17x smaller in
// dogfooding), which the client only learns on the final admit response. RED
// against a message that states the unresolved hint as a bare "reserve".
func TestConfineAdmissionWaitDiagnosticHedgesUnpinnedReserve(t *testing.T) {
	var mu sync.Mutex
	var buf bytes.Buffer
	sink := writerFunc(func(p []byte) (int, error) {
		mu.Lock()
		defer mu.Unlock()
		return buf.Write(p)
	})
	proceed := make(chan struct{})
	scope := &confineFakeScope{}
	deps := confineUnitDeps(scope)
	deps.admitWaitDiagInterval = 5 * time.Millisecond
	deps.admit = func(context.Context, string, ConfineRequest, int64) (admissionResult, error) {
		<-proceed
		return admissionResult{state: "waited", reserve: 232 << 20, basis: "estimate:p90-prior"}, nil
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		// No MemoryReserve/MemoryReservePinned set: this is the unpinned,
		// no-signature-history default path that falls back to
		// DefaultConfineMemoryReserve (4G) purely as a request hint.
		_, _ = confineWithDeps(context.Background(), ConfineRequest{
			Slice: "finite.slice", Argv: []string{"/bin/true"},
			SelfPath: os.Args[0], Stderr: sink,
		}, deps)
	}()
	deadline := time.Now().Add(testdeadline.Wait(3 * time.Second))
	seen := false
	for time.Now().Before(deadline) {
		mu.Lock()
		got := buf.String()
		mu.Unlock()
		if strings.Contains(got, "waiting for memory admission") {
			seen = true
			break
		}
		time.Sleep(2 * time.Millisecond)
	}
	close(proceed)
	<-done
	if !seen {
		t.Fatalf("no admission-wait progress diagnostic emitted; stderr=%q", buf.String())
	}
	got := buf.String()
	if !strings.Contains(got, "requested reserve 4G") {
		t.Fatalf("unpinned line must label the figure as a requested hint, not a bare reserve; stderr=%q", got)
	}
	if !strings.Contains(got, "unpinned") {
		t.Fatalf("unpinned line must say so explicitly; stderr=%q", got)
	}
	if !strings.Contains(got, "daemon resolves the actual grant") {
		t.Fatalf("unpinned line must warn the daemon's grant may differ; stderr=%q", got)
	}
}

type writerFunc func([]byte) (int, error)

func (f writerFunc) Write(p []byte) (int, error) { return f(p) }

// legacyGovernorEnvironmentKeys are the coordinates AIRA-33 retired with the
// aira_xdist_governor plugin. AIRA still STRIPS them from every launch (so a
// child of a still-running pre-deletion job cannot inherit a live one), but it
// must never SET one again.
var legacyGovernorEnvironmentKeys = []string{
	"AIRA_PY_LIB",
	"AIRA_GOVERNOR",
	"AIRA_GOVERNOR_CMD",
	"AIRA_GOVERNOR_MAX_WAIT",
	"AIRA_GOVERNOR_SLICE",
	"AIRA_TEST_MEM_GOVERNOR",
	"AIRA_TEST_MEM_DEFAULT",
	"AIRA_TEST_MEM_GROWTH_HEADROOM",
	"AIRA_CONFINE_RESERVE_CMD",
}

// TestConfineDelegateRAMDeliversNoLegacyGovernorCoordinates is AIRA-33's
// behaviour-level guard on the env surgery.
//
// --delegate-ram is the ONLY launch shape that ever exported these, and it is
// also the shape that must keep exporting AIRA_CONFINE_SCOPE_ID (which
// confine-reserve's ParentScopeID depends on) and the AIRA_AITEST_* coordinates.
// Asserting all three in one launch is the point: the edit that removes the nine
// is the same edit that could drop the two that must survive, and a test that
// only checked the absence would pass just as happily on a launch that exported
// nothing at all.
//
// The AIRA_CONFINE_SCOPE_ID assertion is therefore ALSO the anti-porosity guard:
// without it, an appendChildEnvironment that returned its input untouched would
// satisfy every absence check above for entirely the wrong reason.
//
// verifies: AIRA-33
func TestConfineDelegateRAMDeliversNoLegacyGovernorCoordinates(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	// Set every retired key in the PARENT environment too. Absence must be the
	// result of a strip, not merely of nothing having set them.
	for _, key := range legacyGovernorEnvironmentKeys {
		t.Setenv(key, "/stale-"+key)
	}
	scope := &confineFakeScope{}
	var stdout bytes.Buffer
	deps := confineUnitDeps(scope)
	deps.writeScopeMemoryCap = func(Scope, int64, int64, bool) error { return nil }
	reported := append(append([]string{}, legacyGovernorEnvironmentKeys...), "AIRA_CONFINE_SCOPE_ID", "AIRA_AITEST_LIB")
	result, err := confineWithDeps(context.Background(), ConfineRequest{
		Slice: "finite.slice", DelegateRAM: true, Name: "pytest",
		Argv:       reportChildEnv(reported...),
		Env:        os.Environ(),
		RuntimeDir: t.TempDir(), SelfPath: os.Args[0], Stdout: &stdout, Stderr: io.Discard,
	}, deps)
	if err != nil || result.Exit != 0 {
		t.Fatalf("result=%+v err=%v", result, err)
	}
	fields := strings.Split(stdout.String(), "|")
	if len(fields) != len(reported) {
		t.Fatalf("child environment report=%q", stdout.String())
	}
	for i, key := range legacyGovernorEnvironmentKeys {
		if fields[i] != "" {
			t.Errorf("delegate-ram launch delivered retired coordinate %s=%q; AIRA-33 deleted the plugin that read it", key, fields[i])
		}
	}
	if fields[len(legacyGovernorEnvironmentKeys)] == "" {
		t.Fatal("AIRA_CONFINE_SCOPE_ID absent: confine-reserve's ParentScopeID depends on it, and its absence also makes every assertion above vacuous")
	}
	if fields[len(legacyGovernorEnvironmentKeys)+1] == "" {
		t.Fatal("AIRA_AITEST_LIB absent: the same edit must not take the aitest coordinates with it")
	}
}

// TestConfineChildReceivesTheResolvedSlice is the behaviour half of AIRA-115.
// The scope id alone was never enough for `aira confine-reserve` running inside
// the job: it identified WHICH job the sub-reservation belonged to but not WHERE
// that job lives, so the reserve defaulted its slice to aira.slice and charged a
// slice whose cgroup does not hold that memory.
//
// The exported value is the RESOLVED slice PATH, not the name the caller typed
// ("finite.slice" here resolves to "/fake/finite.slice"). That distinction is
// the point: it is the same value admitConfine keys the job's own admission on,
// so the sub-reservation lands in its parent's daemon queue rather than in
// whatever a bare name re-resolves to from the DAEMON's own cgroup ancestry.
//
// The stale inherited values in Env are what make this an upsert assertion: a
// nested confine must publish ITS slice, not carry its grandparent's through.
// The AIRA_CONFINE_SCOPE_ID assertion is the anti-porosity witness — an empty
// child environment would satisfy a slice-only check for the wrong reason.
//
// The third reported key is the guard this ticket's own build review forced: the
// operator's AIRA_CONFINE_SLICE must reach the child UNCHANGED. The first cut of
// this fix published the resolved path under that name, which made every nested
// `aira confine` read its parent's absolute cgroup path as an operator-declared
// explicit --slice (see TestInheritedParentSliceIsNotAnExplicitSliceInput).
//
// verifies: AIRA-115
func TestConfineChildReceivesTheResolvedSlice(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	scope := &confineFakeScope{}
	var stdout bytes.Buffer
	result, err := confineWithDeps(context.Background(), ConfineRequest{
		Slice: "finite.slice", Name: "pytest",
		Env: []string{
			"PATH=" + os.Getenv("PATH"),
			pylib.ConfineParentSliceEnv + "=/stale/grandparent.slice",
			"AIRA_CONFINE_SLICE=operator.slice",
			"AIRA_CONFINE_SCOPE_ID=stale-scope",
		},
		Argv:     reportChildEnv(pylib.ConfineParentSliceEnv, "AIRA_CONFINE_SCOPE_ID", "AIRA_CONFINE_SLICE"),
		SelfPath: os.Args[0], Stdout: &stdout, Stderr: io.Discard,
	}, confineUnitDeps(scope))
	if err != nil || result.Exit != 0 {
		t.Fatalf("result=%+v err=%v", result, err)
	}
	fields := strings.Split(stdout.String(), "|")
	if len(fields) != 3 {
		t.Fatalf("child environment report=%q", stdout.String())
	}
	if fields[0] != "/fake/finite.slice" {
		t.Fatalf("%s=%q, want the resolved slice path /fake/finite.slice (confine-reserve inside this job has no other way to learn where it runs)", pylib.ConfineParentSliceEnv, fields[0])
	}
	if fields[1] == "" || fields[1] == "stale-scope" {
		t.Fatalf("AIRA_CONFINE_SCOPE_ID=%q: the confine child environment was not populated, so the slice assertion above proves nothing", fields[1])
	}
	if fields[2] != "operator.slice" {
		t.Fatalf("AIRA_CONFINE_SLICE=%q, want the operator's explicit-slice input carried through untouched: overwriting it hands every nested confine a forged --slice", fields[2])
	}
}

// TestInheritedParentSliceIsNotAnExplicitSliceInput is the F1 regression from
// this ticket's build review, and it is the reason the published coordinate has
// its own variable name at all.
//
// AIRA-115's first cut published the parent job's resolved cgroup PATH under
// AIRA_CONFINE_SLICE — which is already the OPERATOR's explicit-slice input
// (ResolveConfineSlice; install-owned-slice design §4: `--slice` >
// `$AIRA_CONFINE_SLICE` > `aira.slice`, and an explicit value never falls back).
// Nested `aira confine` is the norm on a dogfooding box, so every nested job then
// took its parent's absolute path as an operator-declared --slice: default
// resolution — with its managed-unit guard and whale fallback — was skipped, the
// status line and the detach record named the raw path, and the same value
// reached the daemon's management-slice resolution and any daemon it spawned.
// It is reproducible as a red test suite: `TestDefaultConfinePresentButUncapped-
// FailsOnAIRA` fails under a parent confined by such a binary.
//
// Both halves are asserted through the CONSTANT, so this is RED for exactly the
// wrong behaviour: point pylib.ConfineParentSliceEnv back at AIRA_CONFINE_SLICE
// and the resolver starts honouring the emitted coordinate again.
//
// verifies: AIRA-115
func TestInheritedParentSliceIsNotAnExplicitSliceInput(t *testing.T) {
	const parentPath = "/sys/fs/cgroup/user.slice/user-1000.slice/user@1000.service/aira.slice"
	t.Setenv("AIRA_CONFINE_SLICE", "")
	t.Setenv(pylib.ConfineParentSliceEnv, parentPath)

	// The portable resolver: only --slice and the operator's own variable feed it.
	if got := ResolveConfineSlice(""); got != "" {
		t.Fatalf("ResolveConfineSlice(\"\")=%q with only the emitted parent coordinate set, want \"\": an emitted coordinate is not an operator override", got)
	}

	// And the launch path it feeds. This is TestDefaultConfinePresentButUncapped-
	// FailsOnAIRA's fixture with the parent coordinate published — precisely the
	// case that went red post-deploy — so the uncapped refusal is only the vehicle:
	// what is asserted is that DEFAULT resolution ran and named aira.slice.
	deps := confineUnitDeps(&confineFakeScope{})
	deps.managedUnitPresent = func(string) (bool, error) { return true, nil }
	deps.resolveSlicePathExact = func(name string) (string, error) {
		if name != DefaultConfineSlice {
			t.Fatalf("default resolution asked for %q, want %q: the inherited parent path was taken as an explicit slice", name, DefaultConfineSlice)
		}
		return "/cg/aira.slice", nil
	}
	deps.readCap = func(string) (int64, bool) { return 0, false }
	result, err := confineWithDeps(context.Background(), ConfineRequest{
		Argv: []string{"must-not-run"}, Env: []string{"PATH=" + os.Getenv("PATH")}, Stderr: io.Discard,
	}, deps)
	if result.Status.Slice != DefaultConfineSlice {
		t.Fatalf("nested confine resolved slice=%q err=%v, want %q (the inherited parent coordinate was taken as an explicit --slice)", result.Status.Slice, err, DefaultConfineSlice)
	}
	if err == nil || !strings.Contains(err.Error(), "E_CONFINE_UNAVAILABLE: slice "+DefaultConfineSlice) {
		t.Fatalf("err=%v, want the uncapped refusal to name %q rather than the inherited path", err, DefaultConfineSlice)
	}
}
