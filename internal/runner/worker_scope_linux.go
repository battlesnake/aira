//go:build linux

package runner

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"os"

	"golang.org/x/sys/unix"
)

// CreateWorkerScope creates one worker's cgroup as a child of outerScope
// (already delegated by BootstrapAitestSupervisor), with a hard memory.max
// cap, memory.swap.max=0, and memory.oom.group=1 so a runaway inside this one
// worker self-contains (spec 3.3: per-worker hard cap, not a pool-level cap
// only). It returns the scope path and the SWAP-CAP DISPOSITION, one of the
// WorkerAdmitSwapCap* values, which the daemon puts on the grant line.
//
// AIRA-35 removed this scope's memory.high, and it is worth saying why rather
// than leaving a reader to wonder where the soft throttle went. Two measured
// facts drove it (docs/superpowers/specs/2026-09-06-aira35-worker-oom-convergence-plan.md
// carries the full tables, and TestWorkerScopeOOMGroupKillConvergesPromptly is
// the committed reproduction):
//
//  1. Without memory.swap.max, this scope did not contain a runaway AT ALL. A
//     512 MiB allocation inside a 32 MiB memory.max was never OOM-killed: it
//     was reclaimed into swap and the process exited 0, with half a gigabyte
//     written to the swap device. cgroup-v2's memory.max bounds MEMORY, not
//     memory+swap; swap is a separate limit that defaults to unbounded. The
//     old e2e test appeared to prove containment only because its own HARNESS
//     wrote memory.swap.max=0 on an ancestor -- it was proving the harness.
//     Capping swap here is also what makes the daemon's aggregate guard sound:
//     a sum over memory.max cannot bound a footprint that escapes into swap.
//
//  2. memory.high was the convergence stall, and narrowing the gap only moved
//     it. At the old 80% split a deliberate leaker did not converge in 420
//     SECONDS (5475 memory.high events, ZERO memory.max events -- the kernel
//     held the cgroup pinned below its hard cap, which is exactly what
//     memory.high promises to do). At a 95% split the delay tracks the
//     ABSOLUTE width of the window, so it grows with the cap: ~1 s at 32 MiB
//     but 16-18 s at the 512 MiB default this product actually ships
//     (internal/pylib/aitest/__init__.py's _resolve_estimated_bytes). With no
//     memory.high at all it is 0.03-0.48 s across that whole range.
//     memory.high is a throttle designed for a cgroup whose USERSPACE
//     supervisor acts on the resulting pressure signal; nothing above a worker
//     scope acts on it, so it was a livelock by construction -- and the
//     unkillable D-state AIRA-35 reports is a hazard of that same reclaim
//     path.
//
// What memory.high was claimed to buy is provided elsewhere: the outer scope
// is protected by the daemon's aggregate admission guard (committed memory.max
// summed, plus live supervisor usage, under the ceiling less headroom), not by
// the throttle, and the proactive-recycle watermark is a USERSPACE comparison
// in worker.py that needs a number, not a kernel throttle -- it now reads
// memory.max.
func CreateWorkerScope(ctx context.Context, outerScope, workerID string, memoryMax int64) (string, string, error) {
	backend := newDefaultBackend(outerScope)
	scope, err := backend.Create(ctx, "worker-"+workerID)
	if err != nil {
		return "", "", fmt.Errorf("aitest worker scope: create: %w", err)
	}
	// This function returns a PATH, not the scope, so nothing that outlives it
	// needs the directory FD backend.Create opened. Closing it is required, not
	// tidiness: since AIRA-39 the caller is the long-lived DAEMON rather than a
	// short-lived `aira worker-admit` process, so the FD is no longer reclaimed
	// by process exit, and one accumulates per aitest worker created on the
	// machine until the *os.File finalizer happens to run. Measured before the
	// fix: 15 leaked FDs across 30 creations (found by Sol build-review).
	// Deferred, so the failure paths below close too.
	if closer, ok := scope.(io.Closer); ok {
		defer func() { _ = closer.Close() }()
	}
	// Nothing has entered this just-created scope yet, so a failure removes its
	// under-configured directory directly rather than asking Scope.Remove to
	// read cgroup.events and prove it empty. Leaving it behind would permit a
	// later process to enter without this worker's required limits.
	removeUnusableScope := func(reason string, cause error) (string, string, error) {
		if removeErr := os.Remove(scope.Reference()); removeErr != nil {
			log.Printf("aitest worker scope: %s cleanup: remove %q: %v", reason, scope.Reference(), removeErr)
		}
		return "", "", fmt.Errorf("aitest worker scope: %s: %w", reason, cause)
	}
	// memory.high is deliberately 0 (not written) -- see this function's own
	// doc comment. writeScopeMemoryCap keeps its high>0 branch for `aira
	// confine --memory-high`, which is an unrelated, explicitly-requested flag.
	if err := writeScopeMemoryCap(scope, memoryMax, 0, true); err != nil {
		return removeUnusableScope("memory cap", err)
	}
	// ORDER IS LOAD-BEARING: the swap cap is written only AFTER the memory cap
	// has succeeded. Run it first and an outer scope with no +memory in its
	// subtree_control -- which exposes NO memory.* files at all -- would return
	// ENOENT for memory.swap.max, and that ENOENT would be misread below as
	// "this kernel has no swap support" rather than "this cgroup has no memory
	// controller". A successful memory.max write proves the controller is
	// present, so a subsequent ENOENT can only be about the swap control
	// specifically.
	swapCap, err := writeWorkerScopeSwapCap(scope)
	if err != nil {
		return removeUnusableScope("swap cap", err)
	}
	return WorkerScopeChildPath(outerScope, "worker-"+workerID), swapCap, nil
}

// writeWorkerScopeSwapCap sets memory.swap.max=0 on a worker scope and reports
// which of three HONEST dispositions was established. It never returns a
// disposition it could not prove, per AIRA's "a check that cannot establish its
// result reports unevaluated, never a fake pass" rule.
//
//   - WorkerAdmitSwapCapEnforced: written and verified. Swap is bounded, so
//     memory.max is the whole footprint bound.
//   - WorkerAdmitSwapCapNotApplicable: memory.swap.max does not exist AND
//     /proc/swaps is PROVED absent. Both are registered by the same CONFIG_SWAP
//     build, so this kernel cannot swap at all and memory.max already bounds
//     everything. Nothing to warn about.
//   - WorkerAdmitSwapCapUnavailable: memory.swap.max does not exist on a kernel
//     that can swap (legacy swapaccount=0), or the question could not be
//     answered. Containment is NOT provable; the grant still proceeds -- denying
//     would stall every aitest run on such a host -- but the disposition travels
//     to the supervisor, which says so once on the suite's own output.
//
// Every OTHER failure -- a permission error, a failed write, a read-back
// mismatch -- returns an error and fails the whole scope creation closed,
// exactly as a failed memory.max does. A scope whose controls are unreliable
// must never be handed out claiming containment.
func writeWorkerScopeSwapCap(scope Scope) (string, error) {
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
// swap support, and a worker's memory.max really is its whole footprint bound.
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
