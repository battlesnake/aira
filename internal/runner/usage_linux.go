//go:build linux

package runner

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// cgroupUsage keeps OOM evidence private to the terminal classifier. Both
// counters are nullable because an unreadable or unsupported memory.events file
// is unevaluated, not evidence of zero OOM kills.
type cgroupUsage struct {
	PeakRSS *int64
	CPUUser *int64
	CPUSys  *int64
	// OOMKill is memory.events' HIERARCHICAL oom_kill: this cgroup AND its
	// descendants. Good for "did anything under this scope get OOM-killed",
	// which is what the reserve advisory and the peak-RSS report ask.
	OOMKill *int64
	// OOMKillLocal and OOMGroupKillLocal come from memory.events.LOCAL, which
	// excludes descendants. Together they answer the question the hierarchical
	// counter cannot: "did the OOM killer kill processes belonging to THIS
	// scope?" -- as opposed to a worker sub-cgroup OOM-killed at ITS cap, which
	// propagates upward onto this scope's hierarchical counter while this
	// scope's own processes are untouched. That is not a corner case here:
	// AIRA's own aitest worker scopes are exactly such children, and it is the
	// configuration AIRA-91 investigated. Whose LIMIT fired is a different
	// question and a different counter -- see OOMLocal below.
	//
	// BOTH fields are needed, and which one fires depends on where the victim
	// lived, not on where the limit was. Measured directly on this project's
	// kernel (6.18) and pinned by
	// TestMemoryEventsLocalDistinguishesOwnLimitFromDescendantOOM:
	//
	//	victim directly in the capped scope   -> local oom_kill > 0
	//	victim in a sub-cgroup of the capped
	//	  scope, memory.oom.group=1 on it     -> local oom_kill == 0,
	//	                                         local oom_group_kill > 0
	//	OOM at a DESCENDANT's own cap          -> local oom_kill == 0 and
	//	                                         local oom_group_kill == 0
	//	                                         (hierarchical oom_kill > 0)
	//	external cgroup.kill                   -> every counter stays 0
	//
	// The middle row is the one that matters most in practice: aitest drains the
	// confined leader into a `.aira-supervisor` sub-cgroup, so `oom_kill` alone
	// would miss a genuine OOM at the confine scope's own cap. `oom_group_kill`
	// is keyed on the cgroup whose memory.oom.group was honoured -- this scope --
	// and every confine scope sets memory.oom.group=1 fail-closed.
	OOMKillLocal      *int64
	OOMGroupKillLocal *int64
	// OOMLocal is memory.events.local's `oom`: the max-breach declaration, which
	// the kernel records on the cgroup WHOSE LIMIT FIRED, not on the victim's.
	// It is the only counter that separates "our own cap was hit" from "an
	// ancestor's cap was hit and our processes were the collateral" -- the
	// AIRA-27 slice-OOM shape. Measured on this kernel by
	// TestMemoryEventsLocalDistinguishesOwnLimitFromDescendantOOM:
	//
	//	our own cap fired            -> our local oom > 0
	//	an ancestor's cap fired and
	//	  our processes were killed  -> our local oom == 0, oom_kill > 0
	//
	// It is deliberately NOT part of LocalOOM: on its own it counts OOM
	// declarations that killed nothing, and gating the verdict on it would turn
	// a survivable pressure event into a reported death.
	OOMLocal *int64
}

// OwnLimitOOM reports whether the OOM that killed this job fired at THIS
// cgroup's own limit, as opposed to an ancestor's -- a slice-level OOM whose
// victim happened to be us. Only meaningful once LocalOOM has established that
// an OOM kill happened at all. `evaluated` is false when memory.events.local
// could not be read, in which case the trailer says nothing about whose limit
// fired rather than guessing.
func (u cgroupUsage) OwnLimitOOM() (own bool, evaluated bool) {
	if u.OOMLocal == nil {
		return false, false
	}
	return *u.OOMLocal > 0, true
}

// LocalOOM reports whether the kernel's OOM killer killed processes belonging to
// THIS cgroup, and whether that could be established at all. See the field
// comments above for why it is a disjunction rather than a single counter.
//
// The one measured shape it misses: a scope WITHOUT memory.oom.group whose
// victim lived in a sub-cgroup leaves both local kill counters at zero (only the
// softer `oom` counter rises). Confine sets memory.oom.group fail-closed on
// every scope it creates, so that shape cannot arise for a confined job.
//
// It says the OOM KILLER KILLED OUR PROCESSES. It does NOT say whose limit
// fired -- OwnLimitOOM answers that, and note that a slice-level OOM caused by a
// neighbour's usage reaches this the same way our own cap would, because
// `oom_kill` is keyed on the victim's cgroup.
//
// A POSITIVE answer needs only one counter -- either is definitive on its own. A
// NEGATIVE answer needs BOTH, because they cover different shapes: on a kernel
// old enough to publish `oom_kill` without `oom_group_kill`, a zero `oom_kill`
// cannot rule out the drained-leader OOM above, so the honest answer there is
// unevaluated rather than "no OOM".
func (u cgroupUsage) LocalOOM() (killed bool, evaluated bool) {
	if u.OOMKillLocal != nil && *u.OOMKillLocal > 0 {
		return true, true
	}
	if u.OOMGroupKillLocal != nil && *u.OOMGroupKillLocal > 0 {
		return true, true
	}
	if u.OOMKillLocal == nil || u.OOMGroupKillLocal == nil {
		return false, false
	}
	return false, true
}

// ConfineOOMAttribution names WHOSE memory limit the OOM kills recorded under
// this scope are attributable to (AIRA-102).
//
// It exists because the operator-facing advisory used to be gated on the raw
// HIERARCHICAL `memory.events` oom_kill, which counts kills anywhere in the
// subtree. That made confine print "job OOM-killed at its memory cap <cap>" for
// a job that exited 0 whose CONTAINER hit its own `--memory` -- measured live
// while building AIRA-102. Container nesting turns that from a rare misreport
// into a routine one, so the attribution is now classified rather than assumed.
//
// Every branch reports only what its counters establish, and there is no silent
// branch: an OOM under this scope always produces some line.
type ConfineOOMAttribution string

const (
	// ConfineOOMNone: no OOM kill is recorded under this scope at all.
	ConfineOOMNone ConfineOOMAttribution = ""
	// ConfineOOMOwnLimit: THIS scope's own memory.max fired and killed our
	// processes. The only case that may say "at its memory cap".
	ConfineOOMOwnLimit ConfineOOMAttribution = "own-limit"
	// ConfineOOMDescendant: kills happened beneath this scope but the OOM killer
	// killed nothing belonging to this scope itself -- a descendant's own limit,
	// e.g. a container's --memory.
	ConfineOOMDescendant ConfineOOMAttribution = "descendant"
	// ConfineOOMAncestor: our processes were killed, but our own limit did not
	// declare the breach -- an ancestor's cap fired and we were the collateral
	// (the AIRA-27 slice-OOM shape). Reporting this as "at its memory cap" would
	// send an operator to raise a cap that was never the binding one.
	ConfineOOMAncestor ConfineOOMAttribution = "ancestor"
	// ConfineOOMUnestablished: an OOM kill occurred under this scope but
	// memory.events.local could not be read, so whose limit fired cannot be
	// established. Never a fabricated attribution.
	ConfineOOMUnestablished ConfineOOMAttribution = "unestablished"
)

// classifyConfineOOM is the single attribution decision. It deliberately reads
// the hierarchical counter FIRST as the "did anything die under here at all"
// gate, then narrows with the local counters.
//
// covers: AIRA-102
func classifyConfineOOM(usage cgroupUsage) ConfineOOMAttribution {
	if usage.OOMKill == nil || *usage.OOMKill <= 0 {
		return ConfineOOMNone
	}
	killed, evaluated := usage.LocalOOM()
	if !evaluated {
		return ConfineOOMUnestablished
	}
	if !killed {
		return ConfineOOMDescendant
	}
	own, ownEvaluated := usage.OwnLimitOOM()
	if !ownEvaluated {
		return ConfineOOMUnestablished
	}
	if own {
		return ConfineOOMOwnLimit
	}
	return ConfineOOMAncestor
}

func readCgroupUsage(scopePath string) cgroupUsage {
	var usage cgroupUsage
	if scopePath == "" {
		return usage
	}
	if data, err := os.ReadFile(filepath.Join(scopePath, "memory.peak")); err == nil {
		usage.PeakRSS = parseMemoryPeak(data)
	}
	if data, err := os.ReadFile(filepath.Join(scopePath, "cpu.stat")); err == nil {
		usage.CPUUser, usage.CPUSys = parseCPUStat(data)
	}
	if data, err := os.ReadFile(filepath.Join(scopePath, "memory.events")); err == nil {
		usage.OOMKill = parseMemoryEvents(data)
	}
	if data, err := os.ReadFile(filepath.Join(scopePath, "memory.events.local")); err == nil {
		local := parseCgroupKeyValues(data)
		usage.OOMKillLocal = local["oom_kill"]
		usage.OOMGroupKillLocal = local["oom_group_kill"]
		usage.OOMLocal = local["oom"]
	}
	return usage
}

func parseMemoryPeak(data []byte) *int64 {
	fields := strings.Fields(string(data))
	if len(fields) != 1 {
		return nil
	}
	return parseNonNegativeInt(fields[0])
}

func parseCPUStat(data []byte) (user, system *int64) {
	values := parseCgroupKeyValues(data)
	return values["user_usec"], values["system_usec"]
}

func parseMemoryEvents(data []byte) *int64 {
	return parseCgroupKeyValues(data)["oom_kill"]
}

func parseCgroupKeyValues(data []byte) map[string]*int64 {
	values := make(map[string]*int64)
	for _, line := range strings.Split(string(data), "\n") {
		fields := strings.Fields(line)
		if len(fields) != 2 {
			continue
		}
		if value := parseNonNegativeInt(fields[1]); value != nil {
			values[fields[0]] = value
		}
	}
	return values
}

func parseNonNegativeInt(value string) *int64 {
	parsed, err := strconv.ParseInt(value, 10, 64)
	if err != nil || parsed < 0 {
		return nil
	}
	return &parsed
}

func classifyOOMKilled(status Status, usage cgroupUsage, explicitKill bool) Status {
	if usage.OOMKill != nil && *usage.OOMKill > 0 && !(status == StatusKilled && explicitKill) {
		return StatusOOMKilled
	}
	return status
}
