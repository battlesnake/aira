//go:build linux

package runner

import (
	"context"
	"fmt"
)

// CreateWorkerScope creates one worker's cgroup as a child of outerScope
// (already delegated by BootstrapAitestSupervisor), with a hard memory.max
// cap and a memory.high soft-throttle watermark below it, plus
// memory.oom.group=1 so a runaway inside this one worker self-contains
// (spec 3.3: per-worker hard cap, not a pool-level cap only).
func CreateWorkerScope(ctx context.Context, outerScope, workerID string, memoryMax, memoryHigh int64) (string, error) {
	backend := newDefaultBackend(outerScope)
	scope, err := backend.Create(ctx, "worker-"+workerID)
	if err != nil {
		return "", fmt.Errorf("aitest worker scope: create: %w", err)
	}
	if err := writeScopeMemoryCap(scope, memoryMax, memoryHigh, true); err != nil {
		return "", fmt.Errorf("aitest worker scope: memory cap: %w", err)
	}
	return WorkerScopeChildPath(outerScope, "worker-"+workerID), nil
}
