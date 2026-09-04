//go:build linux

package runner

import (
	"context"
	"fmt"
	"io"
	"log"
	"os"
)

// CreateWorkerScope creates one worker's cgroup as a child of outerScope
// (already delegated by BootstrapAitestSupervisor), with a hard memory.max
// cap and a memory.high soft-throttle watermark below it, plus
// memory.oom.group=1 so a runaway inside this one worker self-contains
// (spec 3.3: per-worker hard cap, not a pool-level cap only).
func CreateWorkerScope(ctx context.Context, outerScope, workerID string, memoryMax, memoryHigh int64) (string, error) {
	// Enforced HERE, not in the shared writeScopeMemoryCap (confine_linux.go,
	// used by other scope kinds too with their own semantics): spec 3.3
	// requires memory.high be "set below its cap" for a worker scope
	// specifically, a soft throttle before the hard memory.max cap. This
	// currently holds only because evaluateWorkerAdmit (the sole caller) always
	// computes memoryHigh = estimatedBytes*4/5 < memoryMax by construction --
	// CreateWorkerScope itself had no independent check, so a future call site
	// or daemon-side formula change producing memory_high >= memory_max would
	// be silently accepted, defeating the soft-throttle-before-hard-cap design
	// (found by Sol build-review, AIRA-38 review wave). memoryHigh <= 0 is
	// still valid -- writeScopeMemoryCap already treats that as "no watermark
	// configured, skip memory.high entirely".
	if memoryHigh > 0 && memoryHigh >= memoryMax {
		return "", fmt.Errorf("aitest worker scope: memory_high (%d) must be below memory_max (%d)", memoryHigh, memoryMax)
	}
	backend := newDefaultBackend(outerScope)
	scope, err := backend.Create(ctx, "worker-"+workerID)
	if err != nil {
		return "", fmt.Errorf("aitest worker scope: create: %w", err)
	}
	// This function returns a PATH, not the scope, so nothing that outlives it
	// needs the directory FD backend.Create opened. Closing it is required, not
	// tidiness: since AIRA-39 the caller is the long-lived DAEMON rather than a
	// short-lived `aira worker-admit` process, so the FD is no longer reclaimed
	// by process exit, and one accumulates per aitest worker created on the
	// machine until the *os.File finalizer happens to run. Measured before the
	// fix: 15 leaked FDs across 30 creations (found by Sol build-review).
	// Deferred, so the memory-cap failure path below closes too.
	if closer, ok := scope.(io.Closer); ok {
		defer func() { _ = closer.Close() }()
	}
	if err := writeScopeMemoryCap(scope, memoryMax, memoryHigh, true); err != nil {
		// Nothing has entered this just-created scope yet, so remove its
		// capless directory directly rather than asking Scope.Remove to read
		// cgroup.events and prove it empty. Leaving it behind would permit a
		// later process to enter without this worker's required memory.max.
		if removeErr := os.Remove(scope.Reference()); removeErr != nil {
			log.Printf("aitest worker scope: memory cap cleanup: remove %q: %v", scope.Reference(), removeErr)
		}
		return "", fmt.Errorf("aitest worker scope: memory cap: %w", err)
	}
	return WorkerScopeChildPath(outerScope, "worker-"+workerID), nil
}
