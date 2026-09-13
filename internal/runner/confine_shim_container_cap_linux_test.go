//go:build linux

package runner

import (
	"context"
	"io"
	"os"
	"strings"
	"testing"
)

// aitest v0.7 S2a T09 — the ci-shim half of the container-cap injection.
//
// The T07 delegate-RAM collapse removed the `!request.DelegateRAM` guard from
// BOTH the real path (confine_linux.go) and the ci-shim path
// (confine_shim_linux.go:231): under the new "delegate is an ordinary confine
// job" model a declared --memory-reserve is the container cap exactly as on any
// confine job. The real path's removal is covered end to end by
// container_confine_linux_test.go's "delegate declared reserve is injected as the
// container cap" case; the SHIM path's removal was left untested. This mirrors
// that assertion for the shim launch (confineShimLaunch), which is the only
// container-integration path a ci-shim install exercises.
//
// Non-porous: the mutation is to restore the deleted guard at
// confine_shim_linux.go:231 —
//
//	if declaredContainerCap <= 0 && declaredReserve && !request.DelegateRAM {
//
// which withholds the injection on the --delegate-ram arm; TestShimDelegate
// ReserveIsInjectedAsTheContainerCap then reds (no --memory=, facet "none").
//
// verifies: aitest v0.7 S2a T07 shim container-cap open item

type shimContainerObservation struct {
	targetArgv []string
	status     ConfineStatus
}

// runShimContainerLaunch drives a real ci-shim confineShimLaunch (via
// confineWithDeps with shimUnitDeps' every-cgroup-seam-panics deps) and captures
// the argv the container runtime WOULD have seen, replacing the target with
// /bin/true before the child starts so no real `podman run` is ever executed
// (the same guard container_confine_linux_test.go's runContainerLaunch documents,
// for the same reason). The container-integration decision is made BEFORE start
// (confine_shim_linux.go:234-242), so both the injected argv and the
// ContainerMemory facet are observable here.
func runShimContainerLaunch(t *testing.T, request ConfineRequest) shimContainerObservation {
	t.Helper()
	deps := shimUnitDeps()
	observation := shimContainerObservation{}
	baseStart := deps.start
	deps.start = func(command *confineCommand) error {
		// confineSetupArgv appends the target after a literal "--"; everything past
		// it is the effective argv the container runtime would receive. Capture it,
		// then REPLACE the target with /bin/true so shimUnitDeps' base start (which
		// execs the target through __confine-test-setup) never launches podman.
		for index, argument := range command.cmd.Args {
			if argument == "--" {
				observation.targetArgv = append([]string(nil), command.cmd.Args[index+1:]...)
				command.cmd.Args = append(command.cmd.Args[:index+1:index+1], "/bin/true")
				break
			}
		}
		return baseStart(command)
	}
	request.SelfPath = os.Args[0]
	request.Stderr = io.Discard
	request.Stdout = io.Discard
	if request.RuntimeDir == "" {
		request.RuntimeDir = t.TempDir()
	}
	result, err := confineWithDeps(context.Background(), request, deps)
	if err != nil {
		t.Fatalf("shim confine failed: %v", err)
	}
	observation.status = result.Status
	return observation
}

func shimTargetHasMemoryFlag(argv []string, want string) bool {
	for _, argument := range argv {
		if argument == want {
			return true
		}
	}
	return false
}

// TestShimDelegateReserveIsInjectedAsTheContainerCap is the load-bearing case: a
// ci-shim `--delegate-ram --memory-reserve 512M -- podman run alpine` must inject
// --memory=536870912 into podman, exactly as the real path does, because a
// delegate job is an ordinary confine job and its declared reserve is its
// container cap. Asserts BOTH the actual injected argv and the ContainerMemory
// facet (the two the real-path test asserts).
func TestShimDelegateReserveIsInjectedAsTheContainerCap(t *testing.T) {
	observation := runShimContainerLaunch(t, ConfineRequest{
		Argv:        []string{"podman", "run", "alpine"},
		DelegateRAM: true, MemoryReserve: 512 << 20, MemoryReservePinned: true,
	})
	if !shimTargetHasMemoryFlag(observation.targetArgv, "--memory=536870912") {
		t.Fatalf("ci-shim delegate --memory-reserve 512M was NOT injected as the podman container cap: argv=%q "+
			"(the removed !request.DelegateRAM guard at confine_shim_linux.go:231 must stay removed)", observation.targetArgv)
	}
	if observation.status.ContainerMemory != "injected=536870912" {
		t.Fatalf("container-memory facet = %q, want \"injected=536870912\"", observation.status.ContainerMemory)
	}
	// The split placement is injected too (podman nests the container in the job's
	// own scope) — same as the real path's TestContainerLaunchInjectsSplitAndMemory.
	if !shimTargetHasMemoryFlag(observation.targetArgv, "--cgroups=split") {
		t.Fatalf("ci-shim podman launch did not inject --cgroups=split: argv=%q", observation.targetArgv)
	}
}

// TestShimNonDelegateReserveIsAlsoInjected is the twin that proves the injection
// is not delegate-specific — a plain (non-delegate) --memory-reserve is the
// container cap too. Together with the delegate case above it pins that the
// removed guard made the two arms behave IDENTICALLY (the whole point of the
// collapse); a re-added guard would split them and red only the delegate case.
func TestShimNonDelegateReserveIsAlsoInjected(t *testing.T) {
	observation := runShimContainerLaunch(t, ConfineRequest{
		Argv:          []string{"podman", "run", "alpine"},
		MemoryReserve: 512 << 20, MemoryReservePinned: true,
	})
	if !shimTargetHasMemoryFlag(observation.targetArgv, "--memory=536870912") {
		t.Fatalf("ci-shim non-delegate --memory-reserve 512M was not injected: argv=%q", observation.targetArgv)
	}
	if observation.status.ContainerMemory != "injected=536870912" {
		t.Fatalf("container-memory facet = %q, want \"injected=536870912\"", observation.status.ContainerMemory)
	}
}

// TestShimDelegateWithoutADeclaredReserveInjectsNothing is the anti-over-injection
// guard: with no declared cap (delegate resolves an ordinary estimate, unpinned),
// nothing may be imposed on the caller's container — the same rule the real path's
// TestContainerLaunchNeverInjectsAnEstimatedCap enforces. This keeps the case
// above honest: it is the DECLARED reserve, not the mere presence of --delegate-ram,
// that injects.
func TestShimDelegateWithoutADeclaredReserveInjectsNothing(t *testing.T) {
	observation := runShimContainerLaunch(t, ConfineRequest{
		Argv:        []string{"podman", "run", "alpine"},
		DelegateRAM: true,
	})
	for _, argument := range observation.targetArgv {
		if strings.HasPrefix(argument, "--memory=") {
			t.Fatalf("ci-shim delegate with no declared reserve injected a cap %q into the caller's container: %q",
				argument, observation.targetArgv)
		}
	}
	if observation.status.ContainerMemory == "injected=536870912" {
		t.Fatalf("container-memory facet claims an injection that was never declared: %q", observation.status.ContainerMemory)
	}
}
