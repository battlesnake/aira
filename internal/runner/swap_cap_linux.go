//go:build linux

package runner

import (
	"errors"
	"fmt"
	"os"

	"golang.org/x/sys/unix"
)

// writeScopeSwapCap sets memory.swap.max=0 on scope and reports which of three
// HONEST dispositions was established. It never returns a disposition it could
// not prove, per AIRA's "a check that cannot establish its result reports
// unevaluated, never a fake pass" rule.
//
// AIRA-110 promoted this from an aitest-worker detail to the shared primitive
// every AIRA scope goes through, because the property it defends is not
// worker-specific: cgroup-v2's memory.max bounds a cgroup's MEMORY (anon + file
// + kernel), NOT memory+swap. memory.swap.max is a separate limit that defaults
// to inherited-unbounded, so a scope carrying only a memory.max does not contain
// a runaway at all on a host with swap -- it is reclaimed into swap and never
// killed. Measured on this project's own host (WSL2, 20 GiB swap active): a
// process allocating and touching 512 MiB inside a 32 MiB memory.max with
// memory.oom.group=1 was NEVER OOM-killed and exited 0, with ~520 MiB written to
// the swap device; the same scope with memory.swap.max=0 group-killed it in
// 0.03-0.48 s. So without this write:
//
//   - the scope cap is not the containment bound its own status line implies;
//   - the daemon's reserve ledger and its Sigma(reserve) <= cap - headroom
//     accounting, both stated over memory.max, cannot bound a footprint that
//     escapes into swap;
//   - peak-RSS history records a DEFLATED peak for a job that swapped, so the
//     next estimate for that signature is systematically low -- a feedback loop
//     toward under-reservation.
//
// The dispositions, whose vocabulary is the WorkerAdmitSwapCap* catalogue this
// primitive was first minted for (AIRA-35) and which the worker-admit grant line
// still carries on the wire:
//
//   - WorkerAdmitSwapCapEnforced: written and verified. Swap is bounded, so
//     memory.max is the whole footprint bound.
//   - WorkerAdmitSwapCapNotApplicable: memory.swap.max does not exist AND
//     /proc/swaps is PROVED absent. Both are registered by the same CONFIG_SWAP
//     build, so this kernel cannot swap at all and memory.max already bounds
//     everything. Nothing to warn about.
//   - WorkerAdmitSwapCapUnavailable: memory.swap.max does not exist on a kernel
//     that can swap (legacy swapaccount=0), or the question could not be
//     answered. Containment is NOT provable; the launch still proceeds -- denying
//     would stall every job on such a host, and AIRA has no better bound to offer
//     there than it had before -- but the disposition travels to the caller so
//     nothing claims a containment it did not establish.
//
// Every OTHER failure -- a permission error, a failed write, a read-back
// mismatch -- returns an error and fails the caller closed, exactly as a failed
// memory.max does. A scope whose controls are unreliable must never be handed
// out claiming containment.
//
// ORDER IS LOAD-BEARING at every call site: call this only AFTER a write to
// another memory.* file on the same scope has succeeded (memory.max for a worker
// or an `aira run` scope, memory.oom.group for a confine scope). A cgroup with no
// +memory in its parent's subtree_control exposes NO memory.* files at all and
// would return ENOENT here, which would then be misread as "this kernel has no
// swap support" rather than "this cgroup has no memory controller". An earlier
// successful memory.* write proves the controller is present, so a subsequent
// ENOENT can only be about the swap control specifically.
//
// verifies: AIRA-110
func writeScopeSwapCap(scope Scope) (string, error) {
	fd, err := unix.Openat(scope.FD(), "memory.swap.max", unix.O_WRONLY|unix.O_CLOEXEC, 0)
	if err != nil {
		if errors.Is(err, unix.ENOENT) {
			return classifyAbsentSwapControl(), nil
		}
		return "", fmt.Errorf("open memory.swap.max: %w", err)
	}
	file := os.NewFile(uintptr(fd), "memory.swap.max")
	if file == nil {
		_ = unix.Close(fd)
		return "", errors.New("open memory.swap.max")
	}
	if _, err := file.WriteString("0\n"); err != nil {
		_ = file.Close()
		return "", fmt.Errorf("write memory.swap.max: %w", err)
	}
	if err := file.Close(); err != nil {
		return "", fmt.Errorf("close memory.swap.max: %w", err)
	}
	if err := verifyScopeMemoryValue(scope, "memory.swap.max", 0); err != nil {
		return "", err
	}
	return WorkerAdmitSwapCapEnforced, nil
}

// procSwapsPath and procSelfStatPath are variables so the ENOENT
// disambiguation below is testable without a kernel that lacks CONFIG_SWAP.
var (
	procSwapsPath    = "/proc/swaps"
	procSelfStatPath = "/proc/self/stat"
)

// classifyAbsentSwapControl decides between "this kernel cannot swap" and "this
// host can swap but would not let us bound it", using POSITIVE evidence for the
// former and defaulting to the honest, reported disposition otherwise.
//
// /proc/swaps is created by mm/swapfile.c, which is compiled only under
// CONFIG_SWAP -- the same build that registers the memory.swap.* cgroup files.
// So memory.swap.max absent AND /proc/swaps absent means this kernel has no
// swap support, and a scope's memory.max really is its whole footprint bound.
//
// The /proc/self/stat control read is what keeps that from being a fake pass: a
// missing or unmounted /proc would make EVERY path under it return ENOENT, and
// concluding "this kernel cannot swap" from a failure to look is precisely the
// fabricated result AIRA forbids. Only a proved-absent /proc/swaps inside a
// demonstrably-mounted /proc earns the not-applicable verdict.
func classifyAbsentSwapControl() string {
	if _, err := os.Stat(procSwapsPath); err != nil && errors.Is(err, os.ErrNotExist) {
		if _, controlErr := os.Stat(procSelfStatPath); controlErr == nil {
			return WorkerAdmitSwapCapNotApplicable
		}
	}
	return WorkerAdmitSwapCapUnavailable
}
