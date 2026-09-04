//go:build linux

package runner

import (
	"context"
	"errors"
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
// Idempotent for a repeat call with the SAME outerScope: the membership guard
// below accepts a supervisor already relocated into <outer>/.aira-supervisor,
// Create is EEXIST-tolerant and reopens, the drain finds outer already empty,
// and delegation is a no-op write. That matters because a confine job may run
// aitest-enabled pytest more than once (an ordinary multi-suite Makefile), and
// the second run's process tree is already inside the first run's supervisor
// scope.
//
// What is NOT safe is calling it with a SELF-DISCOVERED outerScope after a prior
// success: the caller's own current cgroup is by then <outer>/.aira-supervisor,
// so it would nest a second supervisor scope inside the first and admit workers
// against a deliberately-uncapped cgroup (AIRA-44). That is why callers pass the
// launcher's AIRA_AITEST_OUTER_SCOPE rather than CurrentCgroupPath() whenever it
// is available.
func BootstrapAitestSupervisor(ctx context.Context, outerScope string, supervisorPID int) (string, error) {
	if supervisorPID <= 0 {
		return "", fmt.Errorf("aitest bootstrap: invalid supervisor pid %d", supervisorPID)
	}
	// Guard BEFORE the drain below, not just after it (Fable build-review,
	// final gate): the normal flow always satisfies this trivially (aira
	// confine --delegate-ram placed supervisorPID directly into outerScope
	// via clone3(CLONE_INTO_CGROUP) before this verb ever runs, so it is
	// already listed in outerScope's cgroup.procs at this point) -- but
	// without this check, a caller invoking this verb by hand with a
	// mismatched outerScope (e.g. from an interactive shell, whose current
	// cgroup is a shared systemd session scope, not an aira-confine-owned
	// one) relocates EVERY process sharing that cgroup -- other shells, an
	// IDE, other agents -- into a fresh child scope before ever
	// discovering the mismatch, with no undo on the later failure. A
	// membership check this cheap belongs before any process is moved,
	// not after.
	supervisorScopePath := WorkerScopeChildPath(outerScope, "supervisor")
	if !scopeContainsPID(outerScope, supervisorPID) {
		// The idempotent re-call (AIRA-44): a prior aitest run in this same
		// confine job already drained everything — including this process's
		// `make` and shell ancestors — into <outer>/.aira-supervisor, so a
		// second run's supervisor is a member of the child, not of outer.
		// Without accepting that, handing the bootstrap the REAL outer scope
		// (which is the fix) would turn AIRA-44's silent wrong-scope into a hard
		// refusal.
		//
		// This second route requires POSITIVE PROOF that outerScope really is a
		// daemon-admitted confine scope, not merely that it has a child with the
		// right name (build-review, Sol): `.aira-supervisor` is a predictable
		// name, so membership in <X>/.aira-supervisor alone would let a caller
		// name any shared cgroup X — a systemd session scope holding an IDE,
		// other shells, other agents — and have every direct member of X drained
		// into a fresh child. A finite memory.max is that proof and is the exact
		// invariant the aitest design already relies on: a real confine-launched
		// outer scope is always given a finite memory.max by the daemon in the
		// same atomic grant that launches it, while a shared session scope and a
		// nested .aira-supervisor are both deliberately uncapped.
		if !scopeContainsPID(supervisorScopePath, supervisorPID) {
			return "", fmt.Errorf("aitest bootstrap: supervisor pid %d is a member of neither %s nor %s", supervisorPID, outerScope, supervisorScopePath)
		}
		if !scopeHasFiniteMemoryMax(outerScope) {
			return "", fmt.Errorf("aitest bootstrap: supervisor pid %d is in %s, but %s has no finite memory.max, so it is not a daemon-admitted confine scope", supervisorPID, supervisorScopePath, outerScope)
		}
	}
	backend := newDefaultBackend(outerScope)
	if err := backend.Probe(ctx); err != nil {
		return "", fmt.Errorf("aitest bootstrap: probe outer scope: %w", err)
	}
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

// moveIntoScope treats ESRCH as success: a process read from outer's
// cgroup.procs may exit, or remain as a not-yet-reaped zombie, before this
// write reaches the kernel. Propagating that harmless race would abort the
// whole bootstrap and disable cgroup admission and per-worker caps.
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
		if errors.Is(err, unix.ESRCH) {
			return nil
		}
		return fmt.Errorf("write cgroup.procs: %w", err)
	}
	return nil
}

// scopeHasFiniteMemoryMax reports whether scopePath carries a real, finite
// memory.max. It is fail-CLOSED in every uncertain direction — an unreadable
// file, the literal "max", a zero or unparseable value all report false —
// because its only caller uses it as positive proof before relocating other
// people's processes.
func scopeHasFiniteMemoryMax(scopePath string) bool {
	data, err := os.ReadFile(scopePath + "/memory.max")
	if err != nil {
		return false
	}
	value := strings.TrimSpace(string(data))
	if value == "" || value == "max" {
		return false
	}
	limit, err := strconv.ParseInt(value, 10, 64)
	return err == nil && limit > 0
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
