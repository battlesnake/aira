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

// confineModeName renders this daemon's mode for a diagnostic. A Server that
// never went through Serve (a unit test constructing one directly) has an empty
// mode; it is reported as `real-slice`, which is what NewServer sets and what
// every already-installed box is.
func (s *Server) confineModeName() string {
	if s.confineMode == "" {
		return runner.ConfineModeReal
	}
	return s.confineMode
}

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

// memoryHighReader returns the SOFT-limit reporting read (AIRA-127), on the same
// nil-checks-to-the-package-default rule as the readers around it. There is no
// shim branch: shim mode has no cgroup, and confineManagement never asks for a
// memory.high it cannot have.
func (s *Server) memoryHighReader() func(string) (int64, string) {
	if s.admitReadMemoryHigh != nil {
		return s.admitReadMemoryHigh
	}
	return readSliceMemoryHigh
}

// memTotalReader and memAvailableReader are the daemon's /proc/meminfo seams,
// on the same nil-checks-to-the-package-default rule as sliceResolver and
// memoryReader above. Production reaches the real /proc/meminfo readers;
// tests inject a synthetic pair (SetShimMeminfoForTest) so a reading's routing
// is exercised without depending on this host's actual memory state.
//
// Two callers, deliberately sharing one pair: readShimMemory's host-wide
// fallback (AIRA-121 F3), and AIRA-127's system frame for `aira top`. The
// `shim`-prefixed field names below predate the second caller and are kept
// rather than churned; neither reader is shim-specific.
func (s *Server) memTotalReader() func() (int64, bool) {
	if s.shimReadMemTotal != nil {
		return s.shimReadMemTotal
	}
	return readMemTotal
}

func (s *Server) memAvailableReader() func() (int64, bool, string) {
	if s.shimReadMemAvailable != nil {
		return s.shimReadMemAvailable
	}
	return readMemAvailable
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
//  1. The container's OWN cgroup, whenever install recorded one whose
//     memory.current is readable. That delegates to readSliceMemoryUsage, so
//     memory.current and the AIRA-21 memory.stat file-LRU discount are real
//     kernel numbers and nothing is reimplemented. `max` is then min(live
//     memory.max, recorded budget): the recorded budget is a BOUND, never a
//     raise, so a runtime that widened the container's limit after install
//     cannot silently widen the ledger, and a declared --memory-max cannot be
//     exceeded by a bigger cgroup limit.
//
//     A cgroup whose memory.max is `max` is USED here, not refused (AIRA-121
//     F1). That combination — a declared --memory-max budget over a container
//     with no per-container memory.max — is the headline case the flag exists
//     for (several tasks on one node, as GCP Batch with taskCountPerNode > 1),
//     and the cgroup's memory.current is a real, namespaced reading regardless
//     of whether a limit is set on it. Refusing it dropped every such container
//     to the host-wide meminfo fall-back below, where an unnamespaced
//     MemTotal-MemAvailable dwarfs the declared budget and every job in the
//     container answers E_ADMIT_TOO_LARGE with cap_minus_headroom=0 for the
//     container's whole life: fail-closed, but inoperable.
//
//  2. /proc/meminfo, as MemTotal - MemAvailable for current usage with max set
//     to the recorded budget. Reached only when there is NO own-cgroup reading,
//     AND the budget's own source is ITSELF host-wide
//     (ShimBudgetSourceMemTotal): a host-wide budget paired with a host-wide
//     live reading is the same scope on both sides of checkedAvailable, which
//     is the AIRA-120 shape and already works end to end. reclaimable is
//     deliberately ZERO here: MemAvailable already credits reclaimable page
//     cache, so applying the AIRA-21 discount on top would double-count it in
//     the permissive direction.
//
//  3. current=0, reclaimable=0 (booked-reserve-only), when there is no
//     own-cgroup reading AND the budget is CONTAINER-scoped (declared or the
//     container's own cgroup memory.max — ShimBudgetSourceDeclared or
//     ShimBudgetSourceCgroupMemoryMax). This is AIRA-121 F3: with no cgroup of
//     its own (CgroupPath=="" from the probe, or a path with no readable
//     memory.current -- a cgroup-v1-only host), the OLD code fell all the way
//     through to case 2 regardless of source, pairing a container-scoped
//     `maximum` with a HOST-WIDE `current` (MemTotal-MemAvailable is not
//     namespaced). On the normal multi-tenant node this host-wide current
//     dwarfs a small declared/container budget, checkedAvailable's
//     charge=max(current,outstanding) pins to it, available collapses to 0,
//     and every job in the container answers E_ADMIT_TOO_LARGE
//     cap_minus_headroom=0 for the container's ENTIRE LIFE -- fail-closed, but
//     silently and permanently inoperable rather than a transient contention
//     wait. Reporting current=0 here instead makes admission booked-reserve-
//     only: checkedAvailable's own outstanding-charge logic (untouched) still
//     correctly gates on whatever THIS ledger has actually granted; it is
//     simply blind to usage outside what this ledger itself booked, which is
//     the honest fact when there is no readable own-cgroup number to see it
//     with. The alternative considered and rejected here was refusing install
//     outright whenever a declared budget has no own-cgroup usage to pair
//     with; booked-reserve-only was preferred because it still lets the
//     documented cgroup-v1-only targets (Amazon Linux 2 / legacy Fargate for
//     AWS Batch) admit at all, which a hard install-time refusal would not.
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
		if current, liveMax, reclaimable, ok, _ := readSliceMemoryUsage(budget.CgroupPath); ok {
			maximum := budget.Bytes
			if liveMax > 0 && liveMax < maximum {
				maximum = liveMax
			}
			return current, maximum, reclaimable, true, ""
		}
	}
	if budget.Source != runner.ShimBudgetSourceMemTotal {
		// F3: a container-scoped budget with no own-cgroup reading to pair it
		// with. See case 3 above -- host-wide meminfo must never stand in for
		// this container's usage.
		return 0, budget.Bytes, 0, true, ""
	}
	total, totalOK := s.memTotalReader()()
	available, availableOK, reason := s.memAvailableReader()()
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
