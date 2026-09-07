//go:build linux

package runner

import (
	"bytes"
	"context"
	"io"
	"strings"
	"testing"
)

// AIRA-102 composition tests.
//
// These run the REAL confineWithDeps launch path with fake cgroup/admission
// deps, and assert the (ledger charge, scope memory.max) PAIR plus the argv
// actually handed to the child. That composition is the only place the ledger
// defects can be seen: the pure ResolveReserve unit tests cannot observe the
// `scopeMemoryMax = admission.reserve` assignment, which is what silently turned
// a container-derived number into the job's own hard cap in plan v1.
//
// verifies: AIRA-102

type containerLaunchObservation struct {
	requestedReserve int64
	wroteScopeMax    int64
	targetArgv       []string
	status           ConfineStatus
	diagnostics      string
}

// runContainerLaunch drives a full confined launch and reports what production
// code decided. admission is the result the fake daemon returns.
func runContainerLaunch(t *testing.T, request ConfineRequest, admission admissionResult) containerLaunchObservation {
	t.Helper()
	scope := &confineFakeScope{}
	deps := confineUnitDeps(scope)

	observation := containerLaunchObservation{}
	deps.admit = func(_ context.Context, _ string, _ ConfineRequest, reserve int64) (admissionResult, error) {
		observation.requestedReserve = reserve
		return admission, nil
	}
	// The fake scope has no real memory.max; capture what production DECIDED to
	// write instead, which is the number under test.
	deps.writeScopeMemoryCap = func(_ Scope, maximum, _ int64, _ bool) error {
		observation.wroteScopeMax = maximum
		return nil
	}
	baseStart := deps.start
	deps.start = func(command *confineCommand) error {
		// confineSetupArgv appends the target after a literal "--", so everything
		// past it is the effective argv the container runtime would see.
		//
		// Then the target is REPLACED with /bin/true before the child is started.
		// This is load-bearing, not tidiness: confineUnitDeps' start actually
		// execs the target, so without this every case in this file would run a
		// REAL `docker run` / `podman run` on the developer's machine. It did --
		// build review (Fable P0) caught `go test ./...` spawning ~157 real
		// alpine containers in the shared docker store, on a box running 33
		// unrelated production containers, plus a registry pull attempt for the
		// nonexistent `qemu-image`. Unit tests here assert what confine DECIDED;
		// only the build-tagged real-podman file may launch a container, and it
		// launches podman only, with --rm.
		for index, argument := range command.cmd.Args {
			if argument == "--" {
				observation.targetArgv = append([]string(nil), command.cmd.Args[index+1:]...)
				command.cmd.Args = append(command.cmd.Args[:index+1:index+1], "/bin/true")
				break
			}
		}
		return baseStart(command)
	}

	diagnostics := &bytes.Buffer{}
	request.Slice = "finite.slice"
	request.Stderr = diagnostics
	request.Stdout = io.Discard
	result, err := confineWithDeps(context.Background(), request, deps)
	if err != nil {
		t.Fatalf("confine failed: %v (diagnostics: %s)", err, diagnostics.String())
	}
	observation.status = result.Status
	observation.diagnostics = diagnostics.String()
	return observation
}

// daemonGrant is the admission shape that makes `scopeMemoryMax =
// admission.reserve` fire: admitted, no flock lock, a real release.
func daemonGrant(reserve int64) admissionResult {
	return admissionResult{state: "immediate", reserve: reserve, release: io.NopCloser(nil), basis: "pinned:client"}
}

// flockFallback reports an "immediate" admission WITH a lock. It is a slice
// free-memory check, not a ledger charge -- keying the trailer's `:reserved` on
// admission.state rather than on the grant predicate would claim a charge that
// never happened.
func flockFallback() admissionResult {
	return admissionResult{state: "immediate", lock: &admitLock{}, basis: "fallback:daemon-unavailable"}
}

func TestContainerLedgerChargeAndScopeCap(t *testing.T) {
	const hint = DefaultConfineMemoryReserve // 4 GiB client no-history hint

	for _, testCase := range []struct {
		name           string
		request        ConfineRequest
		admission      admissionResult
		wantReserve    int64 // exact ledger charge requested of the daemon
		wantScopeMax   int64 // exact scope memory.max
		wantMemoryFrag string
	}{
		{
			// THE podman over-book case. v1 charged 8G here while the kernel binds
			// the nested container at 2G -- 6 GiB of a shared 64 GiB slice reserved
			// for memory the job cannot use.
			name: "podman declared cap is never raised by a larger container limit",
			request: ConfineRequest{
				Argv: []string{"podman", "run", "-m", "8g", "alpine"}, ScopeMemoryMax: 2 << 30,
			},
			admission:    daemonGrant(2 << 30),
			wantReserve:  2 << 30,
			wantScopeMax: 2 << 30,
			// The container limit is reported, and reported as NOT charged.
			wantMemoryFrag: "caller=8589934592:reserve-skipped:podman",
		},
		{
			// The other half of "podman never raises": unpinned, so v1 would have
			// pinned 8G and made it the scope cap, REPLACING the daemon estimate.
			name: "podman unpinned is unaffected by a container limit",
			request: ConfineRequest{
				Argv: []string{"podman", "run", "-m", "8g", "alpine"},
			},
			admission:      daemonGrant(1 << 30),
			wantReserve:    hint,
			wantScopeMax:   1 << 30,
			wantMemoryFrag: "caller=8589934592:reserve-skipped:podman",
		},
		{
			name: "docker declared confine limit stays authoritative",
			request: ConfineRequest{
				Argv: []string{"docker", "run", "-m", "8g", "alpine"}, ScopeMemoryMax: 2 << 30,
			},
			admission:      daemonGrant(2 << 30),
			wantReserve:    2 << 30,
			wantScopeMax:   2 << 30,
			wantMemoryFrag: "caller=8589934592:reserve-skipped:declared",
		},
		{
			// The ticket's own Part 2 target case: docker declares, confine does
			// not, so the footprint AIRA already knows about is charged.
			name: "docker unpinned is raised to the container limit",
			request: ConfineRequest{
				Argv: []string{"docker", "run", "-m", "8g", "alpine"},
			},
			admission:      daemonGrant(8 << 30),
			wantReserve:    8 << 30,
			wantScopeMax:   8 << 30,
			wantMemoryFrag: "caller=8589934592:reserved",
		},
		{
			// Accepted over-book: below the hint the charge stays at the hint, so
			// the scope-cap leak can never squeeze the docker CLI itself.
			name: "docker container limit below the hint keeps the hint",
			request: ConfineRequest{
				Argv: []string{"docker", "run", "-m", "512m", "alpine"},
			},
			admission:      daemonGrant(hint),
			wantReserve:    hint,
			wantScopeMax:   hint,
			wantMemoryFrag: "caller=536870912:reserved",
		},
		{
			// The flock fallback: an "immediate" state WITH a lock. The charge was
			// requested but never made, and the trailer must say so.
			name: "docker on the flock fallback reports reserve-requested",
			request: ConfineRequest{
				Argv: []string{"docker", "run", "-m", "8g", "alpine"},
			},
			admission:      flockFallback(),
			wantReserve:    8 << 30,
			wantScopeMax:   0,
			wantMemoryFrag: "caller=8589934592:reserve-requested",
		},
		{
			// Build review (Sol P2): the flock case alone does not pin the
			// predicate -- `admission.lock == nil` would pass it while still
			// mislabelling these. A timeout/unevaluated admission holds no lock
			// AND no release, so only the full grant predicate rejects it.
			name: "docker on an unevaluated admission reports reserve-requested",
			request: ConfineRequest{
				Argv: []string{"docker", "run", "-m", "8g", "alpine"},
			},
			admission:      admissionResult{state: "unevaluated", basis: "unevaluated:daemon"},
			wantReserve:    8 << 30,
			wantScopeMax:   0,
			wantMemoryFrag: "caller=8589934592:reserve-requested",
		},
		{
			// Build review (Sol P1), end to end: a caller-placed podman container
			// is NOT nested, so it must be charged exactly like docker.
			name: "podman with a caller cgroup-parent is charged like an escapee",
			request: ConfineRequest{
				Argv: []string{"podman", "run", "--cgroup-parent=aira.slice", "-m", "8g", "alpine"},
			},
			admission:      daemonGrant(8 << 30),
			wantReserve:    8 << 30,
			wantScopeMax:   8 << 30,
			wantMemoryFrag: "caller=8589934592:reserved",
		},
		{
			name: "delegate-ram is never raised",
			request: ConfineRequest{
				Argv: []string{"docker", "run", "-m", "8g", "alpine"}, DelegateRAM: true, ScopeMemoryMax: 16 << 30,
			},
			admission:      daemonGrant(DefaultDelegateRAMOverhead),
			wantReserve:    DefaultDelegateRAMOverhead,
			wantScopeMax:   16 << 30,
			wantMemoryFrag: "caller=8589934592:reserve-skipped:delegate-ram",
		},
		{
			// The reviewers' phantom-limit argv: the `-m` belongs to qemu, not to
			// docker. Nothing may be charged, and the trailer must not claim the
			// CALLER passed a memory flag -- the runtime never saw one.
			name: "a phantom limit after the image charges nothing and claims nothing",
			request: ConfineRequest{
				Argv: []string{"docker", "run", "--rm", "qemu-image", "qemu-system-x86_64", "-m", "4G"},
			},
			admission:      daemonGrant(hint),
			wantReserve:    hint,
			wantScopeMax:   hint,
			wantMemoryFrag: "none",
		},
		{
			// Build review (Sol P2): a declared --memory-reserve is an injection
			// source, and nothing exercised that path end to end.
			name: "a declared memory-reserve is the injected cap",
			request: ConfineRequest{
				Argv: []string{"podman", "run", "alpine"}, MemoryReserve: 2 << 30, MemoryReservePinned: true,
			},
			admission:      daemonGrant(2 << 30),
			wantReserve:    2 << 30,
			wantScopeMax:   2 << 30,
			wantMemoryFrag: "injected=2147483648",
		},
		{
			// Build review (Fable P1), the regression the P0-1 fix introduced:
			// under --delegate-ram a declared --memory-reserve is the pinned
			// FRAMEWORK OVERHEAD, not a cap, so injecting it would OOM-kill the
			// container at 512M inside a scope that allows the delegate ceiling.
			name: "delegate-ram never injects the framework overhead as a container cap",
			request: ConfineRequest{
				Argv: []string{"podman", "run", "alpine"}, DelegateRAM: true,
				MemoryReserve: 512 << 20, MemoryReservePinned: true,
			},
			admission:      admissionResult{state: "immediate", reserve: 512 << 20, release: io.NopCloser(nil), scopeCeiling: 16 << 30},
			wantReserve:    512 << 20,
			wantScopeMax:   16 << 30,
			wantMemoryFrag: "none",
		},
		{
			// Build review (Fable P1): a limit larger than the whole slice must be
			// reported and skipped, never pinned into a terminal rejection that
			// would REFUSE the launch.
			name: "a container limit larger than the slice is skipped, not charged",
			request: ConfineRequest{
				Argv: []string{"docker", "run", "-m", "70g", "alpine"},
			},
			admission:      daemonGrant(hint),
			wantReserve:    hint,
			wantScopeMax:   hint,
			wantMemoryFrag: "caller=75161927680:reserve-skipped:exceeds-slice-cap",
		},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			observation := runContainerLaunch(t, testCase.request, testCase.admission)
			if observation.requestedReserve != testCase.wantReserve {
				t.Errorf("ledger charge = %d, want exactly %d", observation.requestedReserve, testCase.wantReserve)
			}
			if observation.wroteScopeMax != testCase.wantScopeMax {
				t.Errorf("scope memory.max = %d, want exactly %d", observation.wroteScopeMax, testCase.wantScopeMax)
			}
			if observation.status.ContainerMemory != testCase.wantMemoryFrag {
				t.Errorf("container-memory facet = %q, want %q", observation.status.ContainerMemory, testCase.wantMemoryFrag)
			}
		})
	}
}

func TestContainerLaunchInjectsSplitAndMemory(t *testing.T) {
	observation := runContainerLaunch(t,
		ConfineRequest{Argv: []string{"podman", "run", "alpine", "true"}, ScopeMemoryMax: 2 << 30},
		daemonGrant(2<<30))

	want := []string{"podman", "run", "--cgroups=split", "--memory=2147483648", "alpine", "true"}
	if strings.Join(observation.targetArgv, " ") != strings.Join(want, " ") {
		t.Fatalf("target argv =\n  %q\nwant\n  %q", observation.targetArgv, want)
	}
	if observation.status.Container != ContainerPlacementPodmanSplitInjected {
		t.Fatalf("container facet = %q", observation.status.Container)
	}
}

// TestContainerLaunchNeverInjectsAnEstimatedCap is the P0 regression guard from
// the build review (Fable). On an unpinned daemon grant the job's scope cap is a
// peak-RSS ESTIMATE, and for docker that estimate tracks the CLI rather than the
// container -- so injecting it would cap the user's container at tens of
// megabytes, OOM-kill it inside docker where this scope cannot see the kill, and
// never self-heal because the history keeps recording a tiny, OOM-free peak.
// Only a caller-DECLARED limit may be imposed on someone's container.
func TestContainerLaunchNeverInjectsAnEstimatedCap(t *testing.T) {
	for _, runtime := range []string{"docker", "podman"} {
		t.Run(runtime, func(t *testing.T) {
			observation := runContainerLaunch(t,
				ConfineRequest{Argv: []string{runtime, "run", "myimage", "bench"}},
				// A realistic CLI-derived estimate: ~34 MiB.
				daemonGrant(36175872))

			for _, argument := range observation.targetArgv {
				if strings.HasPrefix(argument, "--memory") {
					t.Fatalf("injected an estimated cap %q into the caller's container: %q",
						argument, observation.targetArgv)
				}
			}
			if observation.status.ContainerMemory != "none" {
				t.Fatalf("container-memory facet = %q, want \"none\" (nothing was declared)",
					observation.status.ContainerMemory)
			}
		})
	}
}

// TestContainerLaunchLeavesOrdinaryJobsUntouched is the regression guard that
// the whole feature is invisible to every job that is not a container run.
func TestContainerLaunchLeavesOrdinaryJobsUntouched(t *testing.T) {
	observation := runContainerLaunch(t,
		ConfineRequest{Argv: []string{"true", "run", "whatever"}},
		daemonGrant(1<<30))

	want := []string{"true", "run", "whatever"}
	if strings.Join(observation.targetArgv, " ") != strings.Join(want, " ") {
		t.Fatalf("argv was rewritten: %q, want %q", observation.targetArgv, want)
	}
	if observation.status.Container != "" || observation.status.ContainerMemory != "" {
		t.Fatalf("facets leaked onto a non-container job: %q / %q",
			observation.status.Container, observation.status.ContainerMemory)
	}
	if strings.Contains(observation.diagnostics, "container") {
		t.Fatalf("container advisory leaked onto a non-container job: %s", observation.diagnostics)
	}
}

func TestContainerLaunchAdvisories(t *testing.T) {
	for _, testCase := range []struct {
		name    string
		request ConfineRequest
		want    []string
		absent  []string
	}{
		{
			// The unconditional docker warning, on the neither-side-specifies case
			// -- the ticket's own bullet 4. Injecting nothing changes nothing about
			// the underlying escape, so the warning must still appear.
			name:    "docker warns even when nothing was injected or reserved",
			request: ConfineRequest{Argv: []string{"docker", "run", "alpine"}},
			want:    []string{"OUTSIDE aira.slice", "AIRA cannot contain it", "podman run"},
		},
		{
			name:    "docker container limit above the job limit is called out",
			request: ConfineRequest{Argv: []string{"docker", "run", "-m", "8g", "alpine"}, ScopeMemoryMax: 2 << 30},
			want:    []string{"exceeds this job's own limit", "is NOT bound by"},
		},
		{
			// The podman mirror: the caller's belief is equally wrong, so the line
			// exists on both sides -- and it names the BASIS, because this job's
			// limit is very often an AIRA estimate rather than a declared cap.
			name:    "podman container limit above the cap names the binding cap and its basis",
			request: ConfineRequest{Argv: []string{"podman", "run", "-m", "8g", "alpine"}, ScopeMemoryMax: 2 << 30},
			want:    []string{"exceeds this job's cap", "the kernel binds the container at", "pinned:client"},
			absent:  []string{"OUTSIDE aira.slice"},
		},
		{
			name:    "detached podman container is warned that it dies with the job",
			request: ConfineRequest{Argv: []string{"podman", "run", "-d", "alpine"}},
			want:    []string{"will be killed when the confine job exits"},
		},
		{
			name:    "detached docker container is warned that the reservation does not cover it",
			request: ConfineRequest{Argv: []string{"docker", "run", "-d", "alpine"}},
			want:    []string{"outlives the confine job", "does not cover the container's lifetime"},
		},
		{
			// Build review (Sol P2): a caller-supplied --cgroups=split nests the
			// container exactly as an injected one does, so it dies with the job
			// and must get the same warning.
			name:    "detached podman with a caller split still gets the lifetime warning",
			request: ConfineRequest{Argv: []string{"podman", "run", "--cgroups=split", "-d", "alpine"}},
			want:    []string{"will be killed when the confine job exits"},
		},
		{
			// Build review (Fable P2): `>` must not drift to `>=`. When the two
			// numbers are EQUAL nothing is exceeded and the loud line is a lie.
			name:    "an equal container limit produces no exceeds line",
			request: ConfineRequest{Argv: []string{"docker", "run", "-m", "2g", "alpine"}, ScopeMemoryMax: 2 << 30},
			absent:  []string{"exceeds this job"},
		},
		{
			// Build review (Sol P1): AIRA must not imply it governs a container
			// whose placement the caller chose.
			name:    "caller-placed podman container is not claimed as contained",
			request: ConfineRequest{Argv: []string{"podman", "run", "--cgroup-parent=aira.slice", "alpine"}},
			want:    []string{"placement was left to your own --cgroup-parent", "cannot establish"},
			absent:  []string{"the kernel binds the container at"},
		},
		{
			name:    "caller-placed podman with a larger limit does not claim the kernel binds it",
			request: ConfineRequest{Argv: []string{"podman", "run", "--cgroup-parent=aira.slice", "-m", "8g", "alpine"}, ScopeMemoryMax: 2 << 30},
			want:    []string{"exceeds this job's own limit", "depends on the placement you chose"},
			absent:  []string{"the kernel binds the container at"},
		},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			observation := runContainerLaunch(t, testCase.request, daemonGrant(2<<30))
			for _, fragment := range testCase.want {
				if !strings.Contains(observation.diagnostics, fragment) {
					t.Errorf("missing %q in diagnostics:\n%s", fragment, observation.diagnostics)
				}
			}
			for _, fragment := range testCase.absent {
				if strings.Contains(observation.diagnostics, fragment) {
					t.Errorf("unexpected %q in diagnostics:\n%s", fragment, observation.diagnostics)
				}
			}
		})
	}
}

// TestFormatConfineStatusUnchangedWithoutContainer compares against a LITERAL
// expected string rather than the function's own prior output, so a change to
// the shared trailer cannot be ratified by the test that is supposed to catch it.
func TestFormatConfineStatusUnchangedWithoutContainer(t *testing.T) {
	status := ConfineStatus{
		Slice: "aira.slice", Containment: ConfineContainmentEnforced,
		Cap: ConfineCapEnforced, CapBytes: 64 << 30,
		ReserveBytes: 2 << 30, ReserveBasis: "pinned:client",
		AdmissionState: "immediate", Scope: ConfineScopePlaced,
		ScopeIntegrity: ScopeContained, OOMGroup: ConfineOOMGroupSet,
		Priorities: ConfinePrioritiesApplied, CPUWeight: ConfineCPUWeightAging,
		TerminatedBy: ConfineTerminatedNormal,
	}
	// AIRA-121 inserted `containment=` immediately after the slice, on the same
	// always-rendered discipline as terminated-by and scope-swap.max: a trailer
	// silent about WHAT KIND of containment it had cannot be told apart from a
	// ci-shim job that had none at all.
	const base = "confine: slice=aira.slice containment=enforced cap=enforced(64G) reserve=2G reserve-basis=pinned:client " +
		"admission=immediate scope=placed scope-integrity=contained oom.group=set priorities=applied " +
		// AIRA-110's scope-swap.max renders on EVERY trailer, on the same
		// always-rendered discipline as terminated-by: a trailer silent about swap
		// is indistinguishable from one whose cap really is the whole footprint
		// bound. This status was built without a ScopeSwapCap, so it reads
		// unevaluated -- never as a claim that swap is bounded.
		"cpu-weight=aging scope-memory.max=not-requested scope-swap.max=unevaluated terminated-by=normal"
	// AIRA-104's resource facets render on every trailer, container or not, and
	// (per FormatConfineStatus's own ordering) land AFTER container/container-memory
	// -- both nil here, so both read as unevaluated.
	const resources = " peak-rss=unevaluated cpu=unevaluated"
	const want = base + resources
	if got := FormatConfineStatus(status); got != want {
		t.Fatalf("trailer drifted for a non-container job:\n got %q\nwant %q", got, want)
	}

	status.Container = ContainerPlacementPodmanSplitInjected
	status.ContainerMemory = "injected=2147483648"
	const wantContainer = base + " container=podman:split-injected container-memory=injected=2147483648" + resources
	if got := FormatConfineStatus(status); got != wantContainer {
		t.Fatalf("container trailer:\n got %q\nwant %q", got, wantContainer)
	}
}

func TestConfineOOMAttribution(t *testing.T) {
	int64p := func(value int64) *int64 { return &value }
	for _, testCase := range []struct {
		name  string
		usage cgroupUsage
		want  ConfineOOMAttribution
	}{
		{"no oom", cgroupUsage{}, ConfineOOMNone},
		{"hierarchical zero", cgroupUsage{OOMKill: int64p(0)}, ConfineOOMNone},
		{
			// THE case this change exists for: a container OOM-killed at its own
			// --memory. Reporting it as the job's own cap sends an operator to
			// raise a cap that was never the binding one.
			name: "descendant own cap",
			usage: cgroupUsage{
				OOMKill: int64p(1), OOMKillLocal: int64p(0), OOMGroupKillLocal: int64p(0), OOMLocal: int64p(0),
			},
			want: ConfineOOMDescendant,
		},
		{
			name: "our own cap fired",
			usage: cgroupUsage{
				OOMKill: int64p(1), OOMKillLocal: int64p(1), OOMGroupKillLocal: int64p(0), OOMLocal: int64p(1),
			},
			want: ConfineOOMOwnLimit,
		},
		{
			// AIRA-27 slice collateral: our processes died, but our limit never
			// declared the breach.
			name: "ancestor cap fired and we were collateral",
			usage: cgroupUsage{
				OOMKill: int64p(1), OOMKillLocal: int64p(1), OOMGroupKillLocal: int64p(0), OOMLocal: int64p(0),
			},
			want: ConfineOOMAncestor,
		},
		{
			name:  "local counters unreadable",
			usage: cgroupUsage{OOMKill: int64p(1)},
			want:  ConfineOOMUnestablished,
		},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			if got := classifyConfineOOM(testCase.usage); got != testCase.want {
				t.Fatalf("attribution = %q, want %q", got, testCase.want)
			}
		})
	}
}

// TestConfineOOMAttributionAdvisoryIsNeverSilent guards the half of the fix that
// is easy to lose: suppressing the false claim must not leave an OOM producing
// no output at all, and these lines must NOT inherit the reserve advisory's
// scopeMemoryMax>0 gate (an uncapped job would go silent again).
func TestConfineOOMAttributionAdvisoryIsNeverSilent(t *testing.T) {
	int64p := func(value int64) *int64 { return &value }
	usage := cgroupUsage{OOMKill: int64p(3), OOMKillLocal: int64p(0), OOMGroupKillLocal: int64p(0), OOMLocal: int64p(0)}

	// The false claim is gone -- even with a cap in play, which is when the old
	// code printed it.
	if confineOwnCapAdviceWarranted(usage) {
		t.Fatalf("a descendant OOM still warrants own-cap advice")
	}
	if advisory := formatConfineReserveAdvisory(32<<20, nil, confineOwnCapAdviceWarranted(usage), ConfineCapSourceMemoryMax); strings.Contains(advisory, "OOM-killed at its memory cap") {
		t.Fatalf("descendant OOM still reported as the job's own cap: %q", advisory)
	}
	// ...and it was not replaced by silence, with no cap in play at all.
	advisory := formatConfineOOMAttributionAdvisory(ConfineTerminatedNormal, classifyConfineOOM(usage), usage)
	for _, fragment := range []string{"3 OOM kill(s) beneath this scope", "container's --memory", "killed nothing belonging to this scope itself"} {
		if !strings.Contains(advisory, fragment) {
			t.Fatalf("descendant advisory missing %q: %q", fragment, advisory)
		}
	}
	for _, testCase := range []struct {
		attribution ConfineOOMAttribution
		fragment    string
	}{
		{ConfineOOMAncestor, "an ancestor's cap fired"},
		{ConfineOOMUnestablished, "could not be established"},
		// Own-limit is already owned by the reserve advisory, and every
		// attribution is already owned by the existing advisories on the `oom`
		// and unattributed-SIGKILL verdicts, so this line stays out of their way.
		{ConfineOOMOwnLimit, ""},
		{ConfineOOMNone, ""},
	} {
		got := formatConfineOOMAttributionAdvisory(ConfineTerminatedNormal, testCase.attribution, usage)
		if testCase.fragment == "" {
			if got != "" {
				t.Fatalf("%q should produce no line here, got %q", testCase.attribution, got)
			}
			continue
		}
		if !strings.Contains(got, testCase.fragment) {
			t.Fatalf("%q advisory missing %q: %q", testCase.attribution, testCase.fragment, got)
		}
	}

	// And it must NOT duplicate the advisories that already own those verdicts.
	for _, verdict := range []string{ConfineTerminatedOOM, ConfineTerminatedUnattributedSIGKILL} {
		if got := formatConfineOOMAttributionAdvisory(verdict, ConfineOOMDescendant, usage); got != "" {
			t.Fatalf("verdict %q already has its own advisory; got a duplicate: %q", verdict, got)
		}
	}
}

// TestConfineOwnCapAdviceWarranted pins the reserve advisory's gate directly.
// The capped-OOM case with an unreadable own-limit counter must KEEP its advice:
// suppressing the false descendant claim must not turn into withholding useful
// guidance from a job that really was OOM-killed.
func TestConfineOwnCapAdviceWarranted(t *testing.T) {
	int64p := func(value int64) *int64 { return &value }
	for _, testCase := range []struct {
		name  string
		usage cgroupUsage
		want  bool
	}{
		{"descendant own cap", cgroupUsage{OOMKill: int64p(1), OOMKillLocal: int64p(0), OOMGroupKillLocal: int64p(0), OOMLocal: int64p(0)}, false},
		{"our own cap", cgroupUsage{OOMKill: int64p(1), OOMKillLocal: int64p(1), OOMGroupKillLocal: int64p(0), OOMLocal: int64p(1)}, true},
		{"ancestor collateral", cgroupUsage{OOMKill: int64p(1), OOMKillLocal: int64p(1), OOMGroupKillLocal: int64p(0), OOMLocal: int64p(0)}, false},
		// Build review (Sol P1) corrected this expectation. An earlier revision
		// returned true here on the reasoning "we know this job was OOM-killed,
		// so give the advice anyway" -- but the LINE it enables says "OOM-killed
		// at its memory cap <cap>", which asserts WHOSE limit fired, and an
		// unreadable memory.events.local establishes no such thing. The advice is
		// not lost: formatConfineOOMAttributionAdvisory covers this case with the
		// same guidance and without the claim (asserted in the trailer test in
		// confine_termination_linux_test.go).
		{"our procs died, own-limit counter unreadable", cgroupUsage{OOMKill: int64p(1), OOMKillLocal: int64p(1), OOMGroupKillLocal: int64p(0)}, false},
		{"nothing readable", cgroupUsage{OOMKill: int64p(1)}, false},
		{"no oom at all", cgroupUsage{}, false},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			if got := confineOwnCapAdviceWarranted(testCase.usage); got != testCase.want {
				t.Fatalf("confineOwnCapAdviceWarranted = %v, want %v", got, testCase.want)
			}
		})
	}
}
