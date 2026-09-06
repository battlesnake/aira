package daemon

import (
	"context"
	"fmt"
	"os"
	"strconv"
	"strings"

	"aira/internal/runner"
)

// AIRA-121. ci-shim mode inside the daemon.
//
// The claim this file is built on, and the one to check in review: THE EXISTING
// sliceQueue ALREADY IS THE LEDGER THIS TICKET ASKS FOR. It touches a cgroup
// through exactly three seams -- the slice-path resolver, the slice-memory
// reader, and the confine scan -- and shim mode re-sources those three. No new
// ledger data structure is introduced, the AIRA-67 per-signature peak-RSS
// estimator is reused by NOT EDITING IT, and the queue, the fairness freeze, the
// headroom arithmetic and E_ADMIT_TOO_LARGE are all unchanged.

// shimBudget is the daemon's copy of the recorded container RAM budget. It is
// set once at start and never mutated: the durable record is written at
// image-build time and cannot change under a running container.
type shimBudget struct {
	Bytes  int64
	Source string
	// CgroupPath is the container's OWN cgroup, when one with a finite
	// memory.max was found at install time. It is read for LIVE usage
	// (memory.current + the memory.stat file-LRU discount) at every admission
	// pass -- the daemon never writes it and never creates anything under it.
	CgroupPath string
}

func (s *Server) shimMode() bool { return s.confineMode == runner.ConfineModeShim }

// sliceResolver returns the slice-path resolver for this daemon's mode. Every
// caller goes through it rather than through `if s.admitResolveSlice == nil`, so
// there is exactly one place where "which resolver does this mode use" is
// decided and a new subsystem cannot silently get the real one.
func (s *Server) sliceResolver() func(string) (string, bool, string) {
	if s.admitResolveSlice != nil {
		return s.admitResolveSlice
	}
	if s.shimMode() {
		return resolveShimSlicePath
	}
	return resolveAdmitSlicePath
}

// memoryReader returns the slice-memory reader for this daemon's mode, on the
// same single-decision-point rule as sliceResolver.
func (s *Server) memoryReader() func(string) (int64, int64, int64, bool, string) {
	if s.admitReadMemory != nil {
		return s.admitReadMemory
	}
	if s.shimMode() {
		return s.readShimMemory
	}
	return readSliceMemory
}

// resolveShimSlicePath answers with the sentinel for ANY requested slice name.
// A shim container has one budget and one queue; a caller naming aira.slice, a
// cgroup path, or nothing at all is asking about the same thing.
func resolveShimSlicePath(string) (string, bool, string) {
	return runner.ShimConfineSlice, true, ""
}

// readShimMemory is the ledger's live reading in shim mode. The path argument is
// the sentinel and is ignored.
//
// Preference order, and why (requirement 4's documented choice):
//
//  1. The container's OWN cgroup, when install recorded a readable, finite
//     memory.max. That delegates to readSliceMemory, so memory.current and the
//     AIRA-21 memory.stat file-LRU discount are real kernel numbers and nothing
//     is reimplemented. `max` is then min(live memory.max, recorded budget): the
//     recorded budget is a BOUND, never a raise, so a runtime that widened the
//     container's limit after install cannot silently widen the ledger, and a
//     declared --memory-max cannot be exceeded by a bigger cgroup limit.
//
//  2. /proc/meminfo, as MemTotal - MemAvailable for current usage with max set
//     to the recorded budget. reclaimable is deliberately ZERO here: MemAvailable
//     already credits reclaimable page cache, so applying the AIRA-21 discount on
//     top would double-count it in the permissive direction.
//
// Any failure reports ok=false, which the existing code turns into a fail-CLOSED
// outcome: waiters stay queued and each one's own maxWait still fires
// E_ADMIT_SATURATED. Identical honesty to the real path, with no new code.
func (s *Server) readShimMemory(string) (int64, int64, int64, bool, string) {
	budget := s.shimBudget
	if budget.Bytes <= 0 {
		return 0, 0, 0, false, "ci-shim budget unestablished"
	}
	if strings.TrimSpace(budget.CgroupPath) != "" {
		if current, liveMax, reclaimable, ok, _ := readSliceMemory(budget.CgroupPath); ok {
			maximum := budget.Bytes
			if liveMax > 0 && liveMax < maximum {
				maximum = liveMax
			}
			return current, maximum, reclaimable, true, ""
		}
	}
	total, totalOK := readMemTotal()
	available, availableOK, reason := readMemAvailable()
	if !totalOK || !availableOK {
		if reason == "" {
			reason = "meminfo-unreadable"
		}
		return 0, 0, 0, false, reason
	}
	current := total - available
	if current < 0 {
		current = 0
	}
	return current, budget.Bytes, 0, true, ""
}

// confineScan is the daemon's ONE confine-scan entry point. In shim mode it
// returns an EMPTY BUT SUCCESSFUL result, which is the true reading and not a
// suppressed failure: there are no cgroup scopes, so zero adopted reserve and
// zero adopted jobs is what is actually there.
//
// The emptiness would ordinarily let sliceProvablyEmpty grant --exclusive on
// fabricated grounds -- an UNCONFINED job told it was running alone. That is
// closed at the other end (admitConnection refuses --exclusive outright in shim
// mode, before the request is ever queued), which is why this can honestly
// report success instead of forcing a scan FAILURE it would then have to
// pretend was real: a failure here would log "confine reserve scan failed" every
// second and arm the exclusive abort anchor against a slice that is fine.
func (s *Server) confineScan(path string) (runner.ConfineListResult, error) {
	if s.shimMode() {
		return runner.ConfineListResult{Verdict: "ok", Scopes: []runner.ConfineRecord{}}, nil
	}
	return runner.ListConfines(context.Background(), path, nil)
}

// resolveDaemonConfineMode decides THIS daemon process's mode.
//
// AIRA-121 gate condition C5: the mode may not come from the environment the
// install's start stage happened to transcribe, because two other launch paths
// exist -- cmd/aira's dispatcher spawns `/proc/self/exe daemon` whenever a
// daemon-routed verb finds no socket, and an operator can run `aira daemon
// serve` by hand. Either would produce a REAL-mode daemon in a shim-installed
// home, against which a shim client's sentinel slice fails to resolve, the
// client falls to its flock fallback, and the job LAUNCHES UNGATED.
//
// So the durable record is authoritative and is read by the daemon ITSELF. The
// AIRA_DAEMON_CONFINE_MODE / _SHIM_BUDGET_* variables are retained purely as the
// test and override seam, and are validated with the established E_CONFIG_INVALID
// idiom rather than silently ignored when malformed.
func resolveDaemonConfineMode(paths Paths) (string, shimBudget, error) {
	mode := strings.TrimSpace(os.Getenv("AIRA_DAEMON_CONFINE_MODE"))
	if mode != "" {
		switch mode {
		case runner.ConfineModeReal:
			return runner.ConfineModeReal, shimBudget{}, nil
		case runner.ConfineModeShim:
			budget, err := shimBudgetFromEnv()
			if err != nil {
				return "", shimBudget{}, err
			}
			return runner.ConfineModeShim, budget, nil
		default:
			return "", shimBudget{}, fmt.Errorf("E_CONFIG_INVALID: AIRA_DAEMON_CONFINE_MODE must be %s or %s", runner.ConfineModeReal, runner.ConfineModeShim)
		}
	}
	record, ok := runner.ReadInstallModeRecord(runner.InstallModePathFor(paths.StateHome))
	if !ok || record.Mode != runner.ConfineModeShim {
		// Absent, unreadable, malformed, or real: the real path, which is what
		// every already-installed box has today.
		return runner.ConfineModeReal, shimBudget{}, nil
	}
	// ReadInstallModeRecord already refuses a shim record without a positive
	// budget and a catalogued source, so these are established here.
	return runner.ConfineModeShim, shimBudget{
		Bytes: record.ShimBudgetBytes, Source: record.ShimBudgetSource, CgroupPath: record.ShimCgroupPath,
	}, nil
}

func shimBudgetFromEnv() (shimBudget, error) {
	raw := strings.TrimSpace(os.Getenv("AIRA_DAEMON_SHIM_BUDGET_BYTES"))
	bytes, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || bytes <= 0 {
		return shimBudget{}, fmt.Errorf("E_CONFIG_INVALID: AIRA_DAEMON_SHIM_BUDGET_BYTES must be a positive byte count")
	}
	source := strings.TrimSpace(os.Getenv("AIRA_DAEMON_SHIM_BUDGET_SOURCE"))
	if !runner.ValidShimBudgetSource(source) {
		return shimBudget{}, fmt.Errorf("E_CONFIG_INVALID: AIRA_DAEMON_SHIM_BUDGET_SOURCE must be %s, %s, or %s",
			runner.ShimBudgetSourceDeclared, runner.ShimBudgetSourceCgroupMemoryMax, runner.ShimBudgetSourceMemTotal)
	}
	return shimBudget{
		Bytes: bytes, Source: source,
		CgroupPath: strings.TrimSpace(os.Getenv("AIRA_DAEMON_SHIM_CGROUP_PATH")),
	}, nil
}
