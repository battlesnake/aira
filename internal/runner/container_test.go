package runner

import (
	"reflect"
	"strings"
	"testing"
)

// verifies: AIRA-102

func TestPlanContainerDetectionIsNarrow(t *testing.T) {
	for _, testCase := range []struct {
		name string
		argv []string
		want ContainerRuntime
	}{
		{"podman run", []string{"podman", "run", "alpine"}, ContainerRuntimePodman},
		{"docker run", []string{"docker", "run", "alpine"}, ContainerRuntimeDocker},
		{"absolute path podman", []string{"/usr/bin/podman", "run", "alpine"}, ContainerRuntimePodman},
		{"absolute path docker", []string{"/usr/bin/docker", "run", "alpine"}, ContainerRuntimeDocker},

		// Every one of these is explicitly OUT of scope (plan 3.1). They must
		// produce no detection at all -- no injection AND no warning.
		{"podman-remote", []string{"podman-remote", "run", "alpine"}, ContainerRuntimeNone},
		{"podman-compose", []string{"podman-compose", "up"}, ContainerRuntimeNone},
		{"docker-compose", []string{"docker-compose", "up"}, ContainerRuntimeNone},
		{"docker compose", []string{"docker", "compose", "up"}, ContainerRuntimeNone},
		{"docker container run", []string{"docker", "container", "run", "alpine"}, ContainerRuntimeNone},
		{"podman container run", []string{"podman", "container", "run", "alpine"}, ContainerRuntimeNone},
		{"global flag before run", []string{"docker", "--context", "x", "run", "alpine"}, ContainerRuntimeNone},
		{"podman cgroup-manager global", []string{"podman", "--cgroup-manager=cgroupfs", "run", "alpine"}, ContainerRuntimeNone},
		{"sudo podman run", []string{"sudo", "podman", "run", "alpine"}, ContainerRuntimeNone},
		{"shell wrapped", []string{"sh", "-c", "docker run alpine"}, ContainerRuntimeNone},
		{"podman exec", []string{"podman", "exec", "x", "true"}, ContainerRuntimeNone},
		{"podman build", []string{"podman", "build", "."}, ContainerRuntimeNone},
		{"podman pod", []string{"podman", "pod", "create"}, ContainerRuntimeNone},
		{"bare podman", []string{"podman"}, ContainerRuntimeNone},
		{"empty", nil, ContainerRuntimeNone},
		{"run uppercase is not run", []string{"docker", "RUN", "alpine"}, ContainerRuntimeNone},
		{"unrelated command", []string{"go", "test", "./..."}, ContainerRuntimeNone},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			if got := PlanContainerIntegration(testCase.argv).Runtime; got != testCase.want {
				t.Fatalf("runtime = %q, want %q", got, testCase.want)
			}
		})
	}
}

func TestPlanContainerMemoryEstablished(t *testing.T) {
	for _, testCase := range []struct {
		name string
		argv []string
		want int64
	}{
		{"long equals lower", []string{"podman", "run", "--memory=4g", "alpine"}, 4 << 30},
		{"long equals upper", []string{"podman", "run", "--memory=64M", "alpine"}, 64 << 20},
		{"long space", []string{"podman", "run", "--memory", "4g", "alpine"}, 4 << 30},
		{"short space", []string{"podman", "run", "-m", "4g", "alpine"}, 4 << 30},
		{"short inline", []string{"podman", "run", "-m4g", "alpine"}, 4 << 30},
		{"bare bytes", []string{"docker", "run", "--memory=4294967296", "alpine"}, 4 << 30},
		{"after other flags", []string{"docker", "run", "--rm", "-it", "--memory=2g", "alpine"}, 2 << 30},
		{"after a valued flag", []string{"docker", "run", "--name", "x", "--memory=2g", "alpine"}, 2 << 30},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			plan := PlanContainerIntegration(testCase.argv)
			if !plan.MemoryPresent {
				t.Fatalf("MemoryPresent = false, want true")
			}
			if !plan.MemoryEstablished {
				t.Fatalf("MemoryEstablished = false (reason %q), want true", plan.MemoryReason)
			}
			if plan.MemoryBytes != testCase.want {
				t.Fatalf("MemoryBytes = %d, want %d", plan.MemoryBytes, testCase.want)
			}
		})
	}
}

// TestPlanContainerMemoryUnevaluated is the honesty half: every case here has a
// memory-limit-shaped token, so Present must be true, but the VALUE must never
// be guessed. A regression that "helpfully" resolves any of these would charge
// the shared ledger for a number nothing established.
func TestPlanContainerMemoryUnevaluated(t *testing.T) {
	for _, testCase := range []struct {
		name string
		argv []string
	}{
		{"short cluster containing m", []string{"docker", "run", "-itm", "4g", "alpine"}},
		{"two occurrences", []string{"docker", "run", "--memory=4g", "-m", "2g", "alpine"}},
		{"trailing long with no value", []string{"docker", "run", "alpine", "--memory"}},
		{"trailing short with no value", []string{"docker", "run", "-m"}},
		{"petabyte unit AIRA does not parse", []string{"docker", "run", "--memory=1p", "alpine"}},
		{"garbage value", []string{"docker", "run", "--memory=garbage", "alpine"}},
		{"single dash long word", []string{"docker", "run", "-memory", "alpine"}},
		// -m 0 means UNLIMITED to docker. Establishing it as 0 would be a
		// category error, and a 0-byte "limit" must never reach the ledger.
		{"zero means unlimited", []string{"docker", "run", "-m", "0", "alpine"}},

		// The boundary-proof cases: both plan reviewers produced these as argv
		// where the scan would ACT WRONGLY rather than decline. The memory-shaped
		// token belongs to the CONTAINER'S OWN command, not to docker/podman.
		{"reviewer case: qemu -m after image", []string{"docker", "run", "--rm", "qemu-image", "qemu-system-x86_64", "-m", "4G"}},
		{"reviewer case: long form after image", []string{"docker", "run", "alpine", "echo", "--memory=8g"}},
		{"python -m after image", []string{"docker", "run", "alpine", "python", "-m", "http.server"}},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			plan := PlanContainerIntegration(testCase.argv)
			if !plan.MemoryPresent {
				t.Fatalf("MemoryPresent = false, want true (a memory-shaped token is present)")
			}
			if plan.MemoryEstablished {
				t.Fatalf("MemoryEstablished = true (bytes %d), want false: the value is not establishable", plan.MemoryBytes)
			}
			if plan.MemoryBytes != 0 {
				t.Fatalf("MemoryBytes = %d, want 0 when not established", plan.MemoryBytes)
			}
			if strings.TrimSpace(plan.MemoryReason) == "" {
				t.Fatalf("MemoryReason is empty; an unevaluated result must say why")
			}
		})
	}
}

// TestPlanContainerMemoryAbsent guards the OTHER direction: tokens that merely
// look memory-ish must not set Present at all, or confine would decline to
// inject a cap on ordinary invocations.
func TestPlanContainerMemoryAbsent(t *testing.T) {
	for _, testCase := range []struct {
		name string
		argv []string
	}{
		{"plain", []string{"podman", "run", "alpine", "true"}},
		{"memory-swap equals", []string{"podman", "run", "--memory-swap=2g", "alpine"}},
		{"memory-swap space", []string{"podman", "run", "--memory-swap", "2g", "alpine"}},
		{"memory-reservation", []string{"podman", "run", "--memory-reservation=1g", "alpine"}},
		{"memory-swappiness", []string{"podman", "run", "--memory-swappiness=0", "alpine"}},
		{"volume short with attached value containing m", []string{"podman", "run", "-v/home/mark:/x", "alpine"}},
		{"env short with attached value containing m", []string{"podman", "run", "-eTERM=xterm", "alpine"}},
		{"boolean cluster without m", []string{"podman", "run", "-it", "alpine"}},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			plan := PlanContainerIntegration(testCase.argv)
			if plan.MemoryPresent {
				t.Fatalf("MemoryPresent = true, want false (reason %q)", plan.MemoryReason)
			}
			if plan.MemoryEstablished {
				t.Fatalf("MemoryEstablished = true, want false")
			}
		})
	}
}

func TestPlanContainerCallerCgroupFlags(t *testing.T) {
	for _, testCase := range []struct {
		name      string
		argv      []string
		wantFlag  string
		wantSplit bool
	}{
		{"cgroups equals split", []string{"podman", "run", "--cgroups=split", "alpine"}, "--cgroups", true},
		{"cgroups equals other", []string{"podman", "run", "--cgroups=disabled", "alpine"}, "--cgroups", false},
		{"cgroups space split", []string{"podman", "run", "--cgroups", "split", "alpine"}, "--cgroups", true},
		{"cgroup-parent equals", []string{"podman", "run", "--cgroup-parent=aira.slice", "alpine"}, "--cgroup-parent", false},
		{"cgroup-parent space", []string{"podman", "run", "--cgroup-parent", "aira.slice", "alpine"}, "--cgroup-parent", false},
		{"pod", []string{"podman", "run", "--pod", "p", "alpine"}, "--pod", false},
		{"pod-id-file", []string{"podman", "run", "--pod-id-file=/x", "alpine"}, "--pod-id-file", false},

		// Exact-token matching only: a prefix match would catch these and
		// withhold split placement for no reason (plan 3.2).
		{"cgroupns is not a cgroup placement flag", []string{"podman", "run", "--cgroupns=host", "alpine"}, "", false},
		{"cgroup-conf is not a cgroup placement flag", []string{"podman", "run", "--cgroup-conf=memory.high=1", "alpine"}, "", false},
		{"none", []string{"podman", "run", "alpine"}, "", false},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			plan := PlanContainerIntegration(testCase.argv)
			if plan.CallerCgroupFlag != testCase.wantFlag {
				t.Fatalf("CallerCgroupFlag = %q, want %q", plan.CallerCgroupFlag, testCase.wantFlag)
			}
			if plan.CallerSplit != testCase.wantSplit {
				t.Fatalf("CallerSplit = %v, want %v", plan.CallerSplit, testCase.wantSplit)
			}
		})
	}
}

func TestPlanContainerDetach(t *testing.T) {
	for _, testCase := range []struct {
		name string
		argv []string
		want bool
	}{
		{"short", []string{"podman", "run", "-d", "alpine"}, true},
		{"long", []string{"podman", "run", "--detach", "alpine"}, true},
		{"long equals true", []string{"podman", "run", "--detach=true", "alpine"}, true},
		{"in a cluster", []string{"podman", "run", "-dit", "alpine"}, true},
		{"absent", []string{"podman", "run", "-it", "alpine"}, false},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			if got := PlanContainerIntegration(testCase.argv).Detach; got != testCase.want {
				t.Fatalf("Detach = %v, want %v", got, testCase.want)
			}
		})
	}
}

// TestContainerInjectArgvExact asserts the EXACT full argv. An assertion that
// merely checked "contains --cgroups=split" would pass against an
// implementation that injected at the wrong position, injected the wrong
// memory value, or dropped a caller token.
func TestContainerInjectArgvExact(t *testing.T) {
	for _, testCase := range []struct {
		name           string
		argv           []string
		scopeMemoryMax int64
		wantArgv       []string
		wantPlacement  string
		wantMemory     string
	}{
		{
			name: "podman gains split and memory",
			argv: []string{"podman", "run", "alpine", "true"}, scopeMemoryMax: 2 << 30,
			wantArgv:      []string{"podman", "run", "--cgroups=split", "--memory=2147483648", "alpine", "true"},
			wantPlacement: ContainerPlacementPodmanSplitInjected, wantMemory: "injected=2147483648",
		},
		{
			name: "podman keeps a caller memory limit and reports it",
			argv: []string{"podman", "run", "-m", "1g", "alpine"}, scopeMemoryMax: 2 << 30,
			wantArgv:      []string{"podman", "run", "--cgroups=split", "-m", "1g", "alpine"},
			wantPlacement: ContainerPlacementPodmanSplitInjected, wantMemory: "caller=1073741824",
		},
		{
			name: "podman with a caller cgroup flag gets no placement injection",
			argv: []string{"podman", "run", "--cgroup-parent=aira.slice", "alpine"}, scopeMemoryMax: 2 << 30,
			wantArgv:      []string{"podman", "run", "--memory=2147483648", "--cgroup-parent=aira.slice", "alpine"},
			wantPlacement: ContainerPlacementPodmanCallerCgroup, wantMemory: "injected=2147483648",
		},
		{
			name: "podman caller split is reported as the caller's",
			argv: []string{"podman", "run", "--cgroups=split", "alpine"}, scopeMemoryMax: 0,
			wantArgv:      []string{"podman", "run", "--cgroups=split", "alpine"},
			wantPlacement: ContainerPlacementPodmanCallerSplit, wantMemory: "none",
		},
		{
			name: "docker never gets a placement flag",
			argv: []string{"docker", "run", "alpine"}, scopeMemoryMax: 2 << 30,
			wantArgv:      []string{"docker", "run", "--memory=2147483648", "alpine"},
			wantPlacement: ContainerPlacementDockerNotContained, wantMemory: "injected=2147483648",
		},
		{
			name: "below the runtime floor nothing is injected",
			argv: []string{"docker", "run", "alpine"}, scopeMemoryMax: 1 << 20,
			wantArgv:      []string{"docker", "run", "alpine"},
			wantPlacement: ContainerPlacementDockerNotContained, wantMemory: "not-injected:below-runtime-minimum",
		},
		{
			name: "no confine cap means no memory injection",
			argv: []string{"docker", "run", "alpine"}, scopeMemoryMax: 0,
			wantArgv:      []string{"docker", "run", "alpine"},
			wantPlacement: ContainerPlacementDockerNotContained, wantMemory: "none",
		},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			plan := PlanContainerIntegration(testCase.argv)
			injection := plan.Inject(testCase.argv, testCase.scopeMemoryMax)
			if !reflect.DeepEqual(injection.Argv, testCase.wantArgv) {
				t.Fatalf("argv =\n  %q\nwant\n  %q", injection.Argv, testCase.wantArgv)
			}
			if injection.Placement != testCase.wantPlacement {
				t.Fatalf("placement = %q, want %q", injection.Placement, testCase.wantPlacement)
			}
			if injection.MemoryFacet != testCase.wantMemory {
				t.Fatalf("memory facet = %q, want %q", injection.MemoryFacet, testCase.wantMemory)
			}
		})
	}
}

// TestContainerInjectLeavesUndetectedArgvByteIdentical is the "never touch what
// we did not detect" guard.
func TestContainerInjectLeavesUndetectedArgvByteIdentical(t *testing.T) {
	for _, argv := range [][]string{
		{"go", "test", "./..."},
		{"sh", "-c", "docker run alpine"},
		{"docker", "compose", "up"},
		{"sudo", "podman", "run", "alpine"},
	} {
		plan := PlanContainerIntegration(argv)
		injection := plan.Inject(argv, 2<<30)
		if !reflect.DeepEqual(injection.Argv, argv) {
			t.Fatalf("undetected argv was rewritten:\n got %q\nwant %q", injection.Argv, argv)
		}
		if injection.Placement != "" || injection.MemoryFacet != "" {
			t.Fatalf("undetected argv produced facets placement=%q memory=%q", injection.Placement, injection.MemoryFacet)
		}
	}
}

// TestContainerInjectDoesNotAliasCallerArgv: Inject must build a FRESH slice.
// `append(argv[:2], ...)` would write through the caller's backing array and
// silently corrupt the durable record's copy of the original argv.
func TestContainerInjectDoesNotAliasCallerArgv(t *testing.T) {
	argv := []string{"podman", "run", "alpine", "true"}
	// Give the slice spare capacity, which is exactly the condition under which
	// an append-in-place bug writes through instead of reallocating.
	spacious := make([]string, len(argv), len(argv)+8)
	copy(spacious, argv)

	plan := PlanContainerIntegration(spacious)
	injection := plan.Inject(spacious, 2<<30)
	if len(injection.Argv) <= len(spacious) {
		t.Fatalf("expected injection to add tokens, got %q", injection.Argv)
	}
	if !reflect.DeepEqual(spacious, argv) {
		t.Fatalf("caller argv was mutated: got %q, want %q", spacious, argv)
	}
}

func TestContainerReserveDecision(t *testing.T) {
	const hint = int64(4 << 30)
	for _, testCase := range []struct {
		name        string
		argv        []string
		resolved    int64
		pinned      bool
		delegateRAM bool
		wantReserve int64
		wantPinned  bool
		wantSkip    string
	}{
		{
			// Podman NEVER raises: the container is nested inside this job's own
			// scope, so raising would over-book past a binding cap or replace the
			// daemon estimate.
			name: "podman never raises", argv: []string{"podman", "run", "-m", "8g", "alpine"},
			resolved: hint, wantReserve: hint, wantPinned: false, wantSkip: ContainerReserveSkipPodman,
		},
		{
			name: "podman with a declared cap never raises", argv: []string{"podman", "run", "-m", "8g", "alpine"},
			resolved: 2 << 30, pinned: true, wantReserve: 2 << 30, wantPinned: true, wantSkip: ContainerReserveSkipPodman,
		},
		{
			name: "docker unpinned raises to the container limit", argv: []string{"docker", "run", "-m", "8g", "alpine"},
			resolved: hint, wantReserve: 8 << 30, wantPinned: true,
		},
		{
			// Accepted over-book: the charge stays at the hint so the scope-cap
			// leak can never cap the docker CLI below it.
			name: "docker unpinned below the hint keeps the hint", argv: []string{"docker", "run", "-m", "512m", "alpine"},
			resolved: hint, wantReserve: hint, wantPinned: true,
		},
		{
			name: "docker with a declared limit is authoritative", argv: []string{"docker", "run", "-m", "8g", "alpine"},
			resolved: 2 << 30, pinned: true, wantReserve: 2 << 30, wantPinned: true, wantSkip: ContainerReserveSkipDeclared,
		},
		{
			name: "delegate-ram is never raised", argv: []string{"docker", "run", "-m", "8g", "alpine"},
			resolved: 512 << 20, pinned: true, delegateRAM: true, wantReserve: 512 << 20, wantPinned: true, wantSkip: ContainerReserveSkipDelegateRAM,
		},
		{
			name: "an unestablished limit never raises", argv: []string{"docker", "run", "alpine", "echo", "--memory=8g"},
			resolved: hint, wantReserve: hint, wantPinned: false,
		},
		{
			name: "no container limit at all", argv: []string{"docker", "run", "alpine"},
			resolved: hint, wantReserve: hint, wantPinned: false,
		},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			plan := PlanContainerIntegration(testCase.argv)
			reserve, pinned, skip := plan.ResolveReserve(testCase.resolved, testCase.pinned, testCase.delegateRAM)
			if reserve != testCase.wantReserve {
				t.Fatalf("reserve = %d, want %d", reserve, testCase.wantReserve)
			}
			if pinned != testCase.wantPinned {
				t.Fatalf("pinned = %v, want %v", pinned, testCase.wantPinned)
			}
			if skip != testCase.wantSkip {
				t.Fatalf("skip = %q, want %q", skip, testCase.wantSkip)
			}
		})
	}
}
