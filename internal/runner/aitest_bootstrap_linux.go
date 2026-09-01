//go:build linux

package runner

import (
	"context"
	"fmt"
	"os"
	"strconv"
	"strings"

	"golang.org/x/sys/unix"
)

// BootstrapAitestSupervisor relocates supervisorPID (the caller's own parent
// process — the pytest supervisor, which aira confine placed directly into
// outerScope) into a fresh child scope, then delegates memory/cpu on
// outerScope's own cgroup.subtree_control. This order is load-bearing:
// cgroup v2 forbids a cgroup from delegating controllers to children while
// it still holds member processes of its own.
//
// Safe for exactly one call per process tree. NOT safe to retry from a
// fresh CLI invocation after a prior partial success: a retry would
// self-discover its outer scope from INSIDE the already-relocated
// supervisor scope (CurrentCgroupPath, Task 3) and nest incorrectly rather
// than reopening the original. Slice 1's supervisor (internal/pylib/aitest,
// Task 11) never retries this call for exactly this reason.
func BootstrapAitestSupervisor(ctx context.Context, outerScope string, supervisorPID int) (string, error) {
	if supervisorPID <= 0 {
		return "", fmt.Errorf("aitest bootstrap: invalid supervisor pid %d", supervisorPID)
	}
	backend := newDefaultBackend(outerScope)
	if err := backend.Probe(ctx); err != nil {
		return "", fmt.Errorf("aitest bootstrap: probe outer scope: %w", err)
	}
	supervisorScopePath := WorkerScopeChildPath(outerScope, "supervisor")
	scope, err := backend.Create(ctx, "supervisor")
	if err != nil {
		existing, openErr := backend.Open(ctx, supervisorScopePath)
		if openErr != nil {
			return "", fmt.Errorf("aitest bootstrap: create supervisor scope: %w (reopen: %v)", err, openErr)
		}
		scope = existing
	}
	// Drain EVERY pid in outer, not just supervisorPID: a transient child
	// racing the bootstrap moment can otherwise leave outer non-empty, which
	// EBUSYs the subtree_control write below nondeterministically (confirmed
	// live in a real spike: a supervisor shell plus two short-lived children
	// were all present in outer at once).
	if err := drainIntoScope(outerScope, scope); err != nil {
		return "", fmt.Errorf("aitest bootstrap: drain outer scope: %w", err)
	}
	// A final-state read on the SUPERVISOR scope, not a check against the
	// just-drained set: correct on both a first call and an idempotent
	// re-call (where outer is already empty and nothing gets drained this
	// time, but supervisorPID's earlier relocation still shows up here).
	if !scopeContainsPID(supervisorScopePath, supervisorPID) {
		return "", fmt.Errorf("aitest bootstrap: supervisor pid %d is not a member of %s after relocation", supervisorPID, supervisorScopePath)
	}
	if _, err := ensureConfineDelegation(outerScope); err != nil {
		return "", fmt.Errorf("aitest bootstrap: delegate outer scope controllers: %w", err)
	}
	return supervisorScopePath, nil
}

const maxDrainAttempts = 20

// drainIntoScope repeatedly reads outer's cgroup.procs and moves every pid
// found into scope, until a read comes back empty. Looping (rather than one
// read-then-move pass) is what makes this safe against a pid that appears
// between the read and the move.
func drainIntoScope(outerScope string, scope Scope) error {
	for attempt := 0; attempt < maxDrainAttempts; attempt++ {
		data, err := os.ReadFile(outerScope + "/cgroup.procs")
		if err != nil {
			return fmt.Errorf("read outer cgroup.procs: %w", err)
		}
		pids := strings.Fields(string(data))
		if len(pids) == 0 {
			return nil
		}
		for _, pid := range pids {
			if err := moveIntoScope(scope, pid); err != nil {
				return fmt.Errorf("move pid %s: %w", pid, err)
			}
		}
	}
	return fmt.Errorf("outer scope still had member processes after %d drain attempts", maxDrainAttempts)
}

func moveIntoScope(scope Scope, pid string) error {
	fd, err := unix.Openat(scope.FD(), "cgroup.procs", unix.O_WRONLY|unix.O_CLOEXEC, 0)
	if err != nil {
		return fmt.Errorf("open cgroup.procs: %w", err)
	}
	file := os.NewFile(uintptr(fd), "cgroup.procs")
	if file == nil {
		_ = unix.Close(fd)
		return fmt.Errorf("open cgroup.procs")
	}
	defer file.Close()
	if _, err := file.WriteString(pid + "\n"); err != nil {
		return fmt.Errorf("write cgroup.procs: %w", err)
	}
	return nil
}

func scopeContainsPID(scopePath string, pid int) bool {
	data, err := os.ReadFile(scopePath + "/cgroup.procs")
	if err != nil {
		return false
	}
	target := strconv.Itoa(pid)
	for _, field := range strings.Fields(string(data)) {
		if field == target {
			return true
		}
	}
	return false
}
