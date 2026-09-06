package runner

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"
)

// mergeConfineRegistry adds a Pending row for every admitted scope that is not
// (yet) on disk. It no longer supplies name or owner: both come from the scope
// id itself, which is authoritative and restart-surviving (AIRA-52). The
// conflict/agreement dance this function used to perform — two waiters claiming
// one scope id with different owners collapsing to "unknown" — went with it,
// since one scope id can now only decode to one owner by construction.
//
// Moved here from the Linux-only scan file by AIRA-121: shim mode's `--list`
// builds its rows from this and NOTHING else (there is no cgroup directory to
// walk), and that path must compile on every platform the daemon does.
func mergeConfineRegistry(byID map[string]ConfineRecord, registry []ConfineRegistryEntry) {
	for _, entry := range registry {
		if _, exists := byID[entry.ScopeID]; exists {
			continue
		}
		name, pid, stamp, owner, ok := parseConfineScopeID(entry.ScopeID)
		if !ok {
			continue
		}
		if owner == "" {
			owner = ConfineUnknownOwner
		}
		// AIRA-135 named "command" here on exactly the same grounds as the fields
		// beside it, and AIRA-137 adds "cpu" on the same grounds again: this
		// function performs NO live read of any kind (it is the cross-platform path
		// the ci-shim daemon builds its whole listing from, and it must compile
		// where there is no /proc or cgroup to read), so the wrapped command and the
		// CPU usage are as unestablished here as the rss and the cap are. Naming
		// them unevaluated is what stops a renderer printing an absence as a fact.
		record := ConfineRecord{Name: name, Owner: owner, ScopeID: entry.ScopeID, SupervisorPID: &pid, Pending: true, UnevaluatedFields: []string{"populated", "rss", "cap", "command", "cpu"}}
		if age := time.Since(time.Unix(0, stamp)); age >= 0 {
			seconds := int64(age / time.Second)
			record.AgeSeconds = &seconds
		}
		byID[entry.ScopeID] = record
	}
}

// ShimConfineList renders `aira confine --list` in ci-shim mode (AIRA-121 gate
// condition C2).
//
// The plan claimed `--list` and `--kill` would get the containment facet
// "without further work". That was FALSE and this function is the correction:
// the daemon's list path calls runner.ListConfines, which os.ReadDir's the slice
// cgroup directory and reports Verdict=unevaluated when it cannot. In shim mode
// there is no such directory, so `--list` would have answered
// UNEVALUATED/E_CONFINE_UNAVAILABLE for every invocation — an operator surface
// that never works is worse than one that says plainly what it can and cannot
// see.
//
// What it CAN see is exactly the daemon's own granted-waiter registry, which is
// the whole ledger in shim mode. Every row is therefore Pending, with
// populated/rss/cap/command named as unevaluated rather than fabricated: there is
// no cgroup to read them from, and printing zeros would state that the jobs are
// idle and uncapped.
func ShimConfineList(registry []ConfineRegistryEntry) ConfineListResult {
	byID := make(map[string]ConfineRecord, len(registry))
	mergeConfineRegistry(byID, registry)
	scopes := make([]ConfineRecord, 0, len(byID))
	for _, record := range byID {
		scopes = append(scopes, record)
	}
	sort.Slice(scopes, func(i, j int) bool { return scopes[i].ScopeID < scopes[j].ScopeID })
	return ConfineListResult{Verdict: "ok", Scopes: scopes}
}

// ShimConfineKill answers `aira confine --kill` in ci-shim mode (AIRA-121 gate
// condition C2).
//
// It NEVER reports a kill. The real path's kill is cgroup.kill, which is the one
// mechanism that reaches a job's whole subtree atomically; shim mode has no
// cgroup at all, so there is nothing this daemon can kill and nothing it can
// confirm empty afterwards. Fabricating a "killed" status here — or firing a
// best-effort signal and calling it one — would be exactly the false-pass this
// project's honesty discipline forbids, and the operator would find the job
// still running.
//
// So it refuses, and the refusal is USEFUL rather than merely correct: it
// resolves the selector through the same matching and ownership rules the real
// path applies, and names the supervisor PID to signal. That supervisor's own
// forwarder delivers to the job's process GROUP (requirement 8), which is the
// strongest teardown a non-cgroup mechanism has.
func ShimConfineKill(selector, callerOwner string, steal bool, registry []ConfineRegistryEntry) (ConfineKillResult, error) {
	listed := ShimConfineList(registry)
	selector = strings.TrimSpace(selector)
	var matches []ConfineRecord
	for _, record := range listed.Scopes {
		pid := ""
		if record.SupervisorPID != nil {
			pid = strconv.Itoa(*record.SupervisorPID)
		}
		if selector == record.ScopeID || selector == record.Name || selector == pid {
			matches = append(matches, record)
		}
	}
	if len(matches) == 0 {
		return ConfineKillResult{}, fmt.Errorf("%s: selector %q matched no confine job (ci-shim mode lists only jobs holding a daemon admission lease)", CodeConfineNotFound, selector)
	}
	if len(matches) > 1 {
		ids := make([]string, len(matches))
		for i := range matches {
			ids[i] = matches[i].ScopeID
		}
		sort.Strings(ids)
		return ConfineKillResult{}, fmt.Errorf("E_SELECTOR_AMBIGUOUS: selector %q matched %s", selector, strings.Join(ids, ", "))
	}
	record := matches[0]
	owner := strings.TrimSpace(record.Owner)
	if owner == "" {
		owner = ConfineUnknownOwner
	}
	// The ownership guard runs BEFORE the PID is disclosed, exactly as on the
	// real path: a refusal that leaks another session's supervisor PID would let
	// an unauthorised caller do by hand what the guard just refused to do for it.
	if !steal && (!ConfineOwnerIsAttested(owner) || !ConfineOwnerIsAttested(callerOwner) || owner != callerOwner) {
		return ConfineKillResult{}, fmt.Errorf("%s: scope=%s owner=%s caller=%s; pass --steal to override", CodeConfineOwnerUnverified, record.ScopeID, owner, callerOwner)
	}
	pid := 0
	if record.SupervisorPID != nil {
		pid = *record.SupervisorPID
	}
	return ConfineKillResult{}, fmt.Errorf(
		"%s: ci-shim mode has no cgroup.kill backstop, so this daemon cannot kill %s and cannot confirm it dead; signal its supervisor directly with `kill %d` (the supervisor forwards to the job's whole process group, except any descendant that has setsid'd away)",
		CodeConfineKillUnconfirmed, record.ScopeID, pid)
}
