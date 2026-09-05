package runner

import (
	"strconv"
	"strings"
)

// Container integration for `aira confine` (AIRA-102).
//
// This file is PORTABLE and PURE on purpose. Deciding what to do with a wrapped
// `docker run` / `podman run` argv is string analysis over a value that crosses
// the CLI, MCP and daemon boundaries; keeping it Linux-only would force a second
// copy of the grammar somewhere else, which is the shape of bug this repository
// has been bitten by before (see the scope-id parser's comment in confine.go).
//
// THE SCOPE IS DELIBERATELY NARROW AND IS NOT AN INVITATION TO GROW.
// Detection fires only when the runtime binary is literally argv[0] and `run` is
// literally argv[1]. `docker compose` / `podman-compose`, `docker container run`,
// any global-flag form (`docker --context X run`), `podman-remote`, `sudo podman
// run`, and every shell-wrapped invocation (`sh -c "docker run ..."`) are OUT OF
// SCOPE: they are neither rewritten nor warned about. This is stated here, in
// the SKILL text, and in the plan, because a reader who assumes wider coverage
// would believe a container was accounted for when nothing looked at it.
//
// The two runtimes differ in kind, not degree:
//   - podman (rootless) forks the container itself, so `--cgroups=split` nests it
//     INSIDE this confine job's own cgroup scope. That is real, kernel-enforced
//     containment, measured live (plan F1).
//   - docker's container is spawned by the system-managed dockerd in a different
//     cgroup tree entirely. NOTHING in this file contains it, and nothing here
//     may ever report that it did.
//
// covers: AIRA-102
type ContainerRuntime string

const (
	ContainerRuntimeNone   ContainerRuntime = ""
	ContainerRuntimeDocker ContainerRuntime = "docker"
	ContainerRuntimePodman ContainerRuntime = "podman"
)

// containerMemoryFloor is the smallest limit worth injecting. Measured on both
// runtimes (plan F7/F12): podman refuses to start a container at 1 MiB
// ("container init was OOM-killed"), docker rejects 1 MiB outright against its
// documented 6 MB minimum, and both accept 6 MiB. Injecting below this would
// turn a working command into a failing one, so confine declines and says so.
const containerMemoryFloor = int64(6 << 20)

// The `container=` trailer facet. Each value names the ACTION confine took, not
// an outcome it did not observe: injecting `--cgroups=split` establishes that
// AIRA asked for nesting, never that podman honoured it (an absent or pre-2.0
// podman rejects the flag and exits). This mirrors the discipline every other
// facet follows and was a plan-gate MUST-FIX.
const (
	ContainerPlacementPodmanSplitInjected = "podman:split-injected"
	ContainerPlacementPodmanCallerSplit   = "podman:caller-split"
	ContainerPlacementPodmanCallerCgroup  = "podman:caller-cgroup"
	ContainerPlacementDockerNotContained  = "docker:not-contained"
)

// Reasons a caller-declared container limit did NOT raise this job's ledger
// charge. Each is reported on the trailer rather than left as silence.
const (
	// ContainerReserveSkipPodman: the container is nested inside this job's own
	// scope, so its memory is already inside whatever the job reserved. Raising
	// would either over-book past a binding cap -- charging the shared slice for
	// memory the kernel forbids the job to use -- or, when unpinned, replace the
	// daemon's history-derived estimate with a client-pinned number. There is
	// only ever one charge per job, so this is NOT "double booking".
	ContainerReserveSkipPodman = "podman"
	// ContainerReserveSkipDeclared: the caller declared confine's own limit, so
	// their number is authoritative and is reported against the container's
	// rather than overridden by it.
	ContainerReserveSkipDeclared = "declared"
	// ContainerReserveSkipDelegateRAM: a delegate-ram job's pinned reserve is
	// deliberately a small framework overhead because its per-test children
	// reserve individually (AIRA-62). Raising it would double-book them.
	ContainerReserveSkipDelegateRAM = "delegate-ram"
	// ContainerReserveSkipExceedsSlice: the declared container limit is larger
	// than the whole slice, so charging it would make the daemon terminally
	// reject the admission and REFUSE a launch the caller never asked to be
	// gated on that number. Reported, never silently clamped to a smaller
	// figure that would look like an accepted charge.
	ContainerReserveSkipExceedsSlice = "exceeds-slice-cap"
)

// Short flags that take an ATTACHED value. `-v/home/mark:/x` and `-eTERM=xterm`
// contain an 'm' but are not clusters, so treating them as one would make
// confine decline to inject a cap on ordinary invocations.
const containerValueTakingShorts = "evpwulhac"

// Short flags that are booleans, and may therefore legitimately cluster.
const containerBooleanShorts = "itdqP"

// ContainerPlan is the pure analysis of one wrapped argv. Every field records
// what was ESTABLISHED; nothing here is a guess.
type ContainerPlan struct {
	Runtime ContainerRuntime

	// MemoryPresent is deliberately OVER-INCLUSIVE. Its only consequence is to
	// make confine DECLINE to inject, which is always safe; the alternative
	// (missing a caller's limit and injecting a duplicate) would make the
	// trailer describe a value that is not the effective one.
	MemoryPresent bool
	// MemoryEstablished is deliberately STRICT: exactly one occurrence, a value
	// the shared parser accepts, no ambiguity marker, positive, and inside the
	// runtime's own option region. Anything else is unevaluated with a reason.
	MemoryEstablished bool
	MemoryBytes       int64
	MemoryReason      string

	// CallerCgroupFlag names the placement flag the caller set, if any. Podman
	// refuses `--cgroups=split` together with `--cgroup-parent` (plan F3), so
	// injecting over a caller's choice would break their command outright.
	CallerCgroupFlag string
	CallerSplit      bool

	Detach bool
}

// Detected reports whether this argv is one of the two in-scope invocations.
func (plan ContainerPlan) Detected() bool { return plan.Runtime != ContainerRuntimeNone }

// NestsInJobScope reports whether the container will be a cgroup DESCENDANT of
// this confine job's own scope -- the premise on which every podman-specific
// decision here rests.
//
// It is NOT the same as "the runtime is podman" (build review, Sol P1). When the
// caller supplied their own `--cgroup-parent` / `--cgroups` / `--pod`, AIRA
// injects no placement flag, so the container goes wherever the caller sent it:
// `podman run --cgroup-parent=aira.slice` makes it a SIBLING of this job's
// scope, bound by the slice cap and not by the job's. Treating that as nested
// would skip the ledger charge for a footprint outside the job's reservation and
// would claim the kernel binds a container it does not bind.
func (plan ContainerPlan) NestsInJobScope() bool {
	if plan.Runtime != ContainerRuntimePodman {
		return false
	}
	return plan.CallerSplit || plan.CallerCgroupFlag == ""
}

// PlanContainerIntegration analyses a wrapped argv. It never mutates its input.
func PlanContainerIntegration(argv []string) ContainerPlan {
	plan := ContainerPlan{}
	if len(argv) < 2 || argv[1] != "run" {
		return plan
	}
	switch containerBase(argv[0]) {
	case "docker":
		plan.Runtime = ContainerRuntimeDocker
	case "podman":
		plan.Runtime = ContainerRuntimePodman
	default:
		return plan
	}

	rest := argv[2:]
	// boundaryOpen tracks whether we are PROVABLY still inside the runtime's own
	// option region. The image argument is the boundary: everything after it
	// belongs to the container's own command, where a `-m` is qemu's or python's
	// flag and not docker's. We cannot locate the image exactly without a full
	// flag-arity table (explicitly out of scope), so we use the earliest position
	// it could occupy: the first token that is neither `-`-prefixed nor
	// immediately preceded by a `-`-prefixed token.
	//
	// This is what stops the scan ACTING WRONGLY rather than merely declining --
	// both plan reviewers independently produced argv that defeated the previous
	// rule (`docker run --rm qemu-image qemu-system-x86_64 -m 4G`, whose `-m` is
	// qemu's; `docker run alpine echo --memory=8g`). The residual is documented:
	// `docker run --rm alpine -m 4g` still establishes, because `alpine` follows
	// a `-`-prefixed token and is indistinguishable from that flag's value.
	boundaryOpen := true
	occurrences := 0
	var value string
	var valueFound, valueMissing bool
	ambiguous := ""

	// A standalone `--` is pflag's definitive end-of-options marker, so the token
	// after it is unambiguously the image and everything beyond that is the
	// container's own command. Without this, `docker run -- alpine -m 8g`
	// establishes the CONTAINER's `-m` as docker's and charges the ledger 8G --
	// a wrong reservation rather than a decline (build review, Sol P1).
	imageAt := -1
	for index, token := range rest {
		if token == "--" {
			imageAt = index + 1
			break
		}
	}

	for index := 0; index < len(rest); index++ {
		token := rest[index]
		// `>=`, not `>`: the token AT imageAt is the image itself, never an
		// option. `docker run -- -m 8g` names an image literally called `-m`, and
		// with `>` that token would have been read as docker's memory flag.
		if imageAt >= 0 && index >= imageAt {
			boundaryOpen = false
		}
		if boundaryOpen && !strings.HasPrefix(token, "-") {
			if index == 0 || !strings.HasPrefix(rest[index-1], "-") {
				boundaryOpen = false
			}
		}
		if !strings.HasPrefix(token, "-") || token == "-" || token == "--" {
			continue
		}

		if strings.HasPrefix(token, "--") {
			name, inline, hasInline := strings.Cut(token, "=")
			switch name {
			case "--memory":
				plan.MemoryPresent = true
				if boundaryOpen {
					occurrences++
					if hasInline {
						value, valueFound = inline, true
					} else if index+1 < len(rest) {
						value, valueFound = rest[index+1], true
					} else {
						valueMissing = true
					}
				}
			case "--cgroups", "--cgroup-parent", "--pod", "--pod-id-file":
				// Exact-token matching only. A prefix match would also catch
				// `--cgroupns` and podman's `--cgroup-conf` and withhold split
				// placement for no reason.
				//
				// Boundary-gated like the memory scan (build review, Sol P1):
				// `podman run alpine echo --cgroup-parent=x` is the CONTAINER's
				// argument, and treating it as the caller's placement choice
				// would silently withhold containment from a job that asked for
				// none of it. A genuine placement flag always precedes the
				// image, so gating here cannot miss a real one.
				if !boundaryOpen {
					continue
				}
				if plan.CallerCgroupFlag == "" {
					plan.CallerCgroupFlag = name
				}
				if name == "--cgroups" {
					if hasInline {
						plan.CallerSplit = plan.CallerSplit || inline == "split"
					} else if index+1 < len(rest) {
						plan.CallerSplit = plan.CallerSplit || rest[index+1] == "split"
					}
				}
			case "--detach":
				// Boundary-gated for the same reason as the flags above: a
				// `--detach` in the container's own command is not a request to
				// detach the CONTAINER, and the lifetime warning must describe
				// what the caller actually asked for.
				if boundaryOpen {
					plan.Detach = true && (!hasInline || inline != "false")
				}
			}
			// --memory-swap, --memory-reservation and --memory-swappiness fall
			// through here deliberately: they are NOT the limit, and matching
			// them would make confine decline to inject on ordinary invocations.
			continue
		}

		// Single-dash token.
		body := token[1:]
		switch {
		case token == "-m":
			plan.MemoryPresent = true
			if boundaryOpen {
				occurrences++
				if index+1 < len(rest) {
					value, valueFound = rest[index+1], true
				} else {
					valueMissing = true
				}
			}
		case body[0] == 'm':
			// `-m4g`, but also `-memory`, whose remainder will not parse and is
			// therefore reported unevaluated rather than guessed.
			plan.MemoryPresent = true
			if boundaryOpen {
				occurrences++
				value, valueFound = body[1:], true
			}
		case strings.ContainsRune(containerValueTakingShorts, rune(body[0])):
			// An attached-value flag, not a cluster. Consumes nothing further.
		case containerAllIn(body, containerBooleanShorts):
			if strings.ContainsRune(body, 'd') {
				plan.Detach = true
			}
		case containerAllIn(body, containerBooleanShorts+"m"):
			// A boolean cluster containing 'm' (e.g. `-itm 4g`). The value's
			// position is not determinable without a full arity table.
			plan.MemoryPresent = true
			ambiguous = token
			if strings.ContainsRune(body, 'd') {
				plan.Detach = true
			}
		default:
			// Unknown second character. Unknown never silently means "no memory
			// flag here": if the token contains an 'm' at all, say so.
			if strings.ContainsRune(body, 'm') {
				plan.MemoryPresent = true
				ambiguous = token
			}
		}
	}

	switch {
	case !plan.MemoryPresent:
		return plan
	case ambiguous != "":
		plan.MemoryReason = "ambiguous-token=" + ambiguous
	case occurrences > 1:
		plan.MemoryReason = "multiple-memory-flags"
	case valueMissing:
		plan.MemoryReason = "memory-flag-without-value"
	case occurrences == 0 || !valueFound:
		plan.MemoryReason = "after-image-candidate"
	default:
		bytes, err := parseMemorySize(value)
		switch {
		case err != nil:
			plan.MemoryReason = "unparsable-value=" + value
		case bytes <= 0:
			// `-m 0` means UNLIMITED to docker. Reporting it as a 0-byte limit
			// would be a category error, and must never reach the ledger.
			plan.MemoryReason = "zero-means-unlimited"
		default:
			plan.MemoryEstablished = true
			plan.MemoryBytes = bytes
		}
	}
	return plan
}

// ContainerInjection is what confine will actually launch, plus the facets that
// say so on the trailer.
type ContainerInjection struct {
	Argv        []string
	Placement   string
	MemoryFacet string
	// InjectedMemory is the cap AIRA added, 0 when it added none.
	InjectedMemory int64
}

// Inject builds the effective argv. It ALWAYS returns a fresh slice and never
// writes through the caller's backing array: the durable detached-job record
// keeps the caller's original argv, and an `append(argv[:2], ...)` on a slice
// with spare capacity would silently corrupt it.
//
// declaredCap is the memory limit the CALLER declared (`--memory-max`, or a
// declared `--memory-reserve`), NOT the job's resolved scope cap. The
// distinction is load-bearing and was a build-review P0 (Fable): an earlier
// build injected the resolved cap, which on an unpinned daemon grant is a
// peak-RSS ESTIMATE. For docker that estimate is derived from the docker CLI's
// own footprint -- the container's memory lives in dockerd's tree and never
// reaches this scope's counters -- so it converges to tens of megabytes. AIRA
// would then inject `--memory=36175872` into the user's container, which is
// OOM-killed inside docker forever; the kill is invisible here (the job still
// reports `terminated-by=normal`), so the history never escalates and it NEVER
// SELF-HEALS. That is precisely the "shim becomes the trap it counters" failure
// this ticket exists to avoid. Only a number the caller actually chose is safe
// to impose on their container.
func (plan ContainerPlan) Inject(argv []string, declaredCap int64) ContainerInjection {
	if !plan.Detected() {
		return ContainerInjection{Argv: argv}
	}
	injection := ContainerInjection{}

	var inserted []string
	switch {
	case plan.Runtime == ContainerRuntimeDocker:
		// There is nothing to place a docker container into. Saying so is the
		// entire point of the docker half of this feature.
		injection.Placement = ContainerPlacementDockerNotContained
	case plan.CallerSplit:
		injection.Placement = ContainerPlacementPodmanCallerSplit
	case plan.CallerCgroupFlag != "":
		// podman refuses split together with --cgroup-parent (F3), and a --pod
		// has its own cgroup parent whose interaction with split was not
		// measured. Either way: inject nothing, claim nothing.
		injection.Placement = ContainerPlacementPodmanCallerCgroup
	default:
		inserted = append(inserted, "--cgroups=split")
		injection.Placement = ContainerPlacementPodmanSplitInjected
	}

	switch {
	case plan.MemoryPresent && plan.MemoryEstablished:
		injection.MemoryFacet = "caller=" + strconv.FormatInt(plan.MemoryBytes, 10)
	case plan.MemoryPresent:
		injection.MemoryFacet = "caller=unevaluated:" + plan.MemoryReason
	case declaredCap <= 0:
		injection.MemoryFacet = "none"
	case declaredCap < containerMemoryFloor:
		injection.MemoryFacet = "not-injected:below-runtime-minimum"
	default:
		inserted = append(inserted, "--memory="+strconv.FormatInt(declaredCap, 10))
		injection.InjectedMemory = declaredCap
		injection.MemoryFacet = "injected=" + strconv.FormatInt(declaredCap, 10)
	}

	// Index 2 -- directly after the `run` subcommand -- is a valid flag position
	// for both runtimes regardless of what follows.
	effective := make([]string, 0, len(argv)+len(inserted))
	effective = append(effective, argv[:2]...)
	effective = append(effective, inserted...)
	effective = append(effective, argv[2:]...)
	injection.Argv = effective
	return injection
}

// ResolveReserve decides whether a caller-declared CONTAINER limit changes this
// job's admission charge. It is the single decision site, and it deliberately
// answers "no" far more often than v1 of the plan did -- see the skip constants
// for why each refusal is the correct one rather than a missing feature.
// sliceCap is the slice's own finite memory ceiling. A container limit ABOVE it
// is never charged: the daemon terminally rejects a pinned reserve that exceeds
// the ceiling (E_ADMIT_TOO_LARGE), which the runner surfaces as a REFUSED
// launch. Refusing `docker run -m 70g` on a 64 GiB slice -- over a reserve the
// caller never declared, for a container that does not live in the slice at all
// -- directly contradicts the "never refuse the launch, report instead" policy
// this feature was designed around (build review, Fable P1). A footprint larger
// than the whole slice cannot be meaningfully accounted for anyway, so it is
// reported and skipped rather than allowed to block the job.
func (plan ContainerPlan) ResolveReserve(reserve int64, pinned, delegateRAM bool, sliceCap int64) (int64, bool, string) {
	if !plan.Detected() || !plan.MemoryEstablished {
		return reserve, pinned, ""
	}
	switch {
	case plan.NestsInJobScope():
		// Keyed on NESTING, not on the runtime: a podman container the caller
		// placed elsewhere is not inside this job's reservation and must be
		// charged like any other escapee (build review, Sol P1).
		return reserve, pinned, ContainerReserveSkipPodman
	case delegateRAM:
		return reserve, pinned, ContainerReserveSkipDelegateRAM
	case pinned:
		return reserve, pinned, ContainerReserveSkipDeclared
	case sliceCap > 0 && plan.MemoryBytes > sliceCap:
		return reserve, pinned, ContainerReserveSkipExceedsSlice
	}
	// Docker, unpinned. The container really does live outside the slice, so a
	// footprint AIRA already knows about is charged rather than ignored. `max`
	// keeps an accepted over-book when the container limit is BELOW the client
	// hint, so the resulting scope cap can never squeeze the docker CLI itself.
	if plan.MemoryBytes > reserve {
		reserve = plan.MemoryBytes
	}
	return reserve, true, ""
}

// ContainerAdvisories are the operator-facing lines emitted BEFORE the child
// starts, so a caller sees them even on a long or externally-killed job.
//
// The docker warning is UNCONDITIONAL for every detected `docker run`, whatever
// was injected or reserved. That is the requirement this whole shim turns on:
// injecting a `--memory` or charging the ledger must never read as "the
// container is now contained", because it is not and cannot be.
func ContainerAdvisories(plan ContainerPlan, injection ContainerInjection, scopeMemoryMax int64, reserveBasis string) []string {
	if !plan.Detected() {
		return nil
	}
	var lines []string
	if plan.Runtime == ContainerRuntimeDocker {
		lines = append(lines, "confine: docker run detected — the container runs under the system docker daemon, "+
			"OUTSIDE aira.slice. AIRA cannot contain it: nothing here bounds it against the slice cap, and this "+
			"remains true whatever confine injected or reserved. Use 'podman run' for kernel-enforced containment.")
	}

	// A podman container the CALLER placed is not known to be inside this job's
	// scope, so AIRA must not imply it governs it (build review, Sol P1). Said
	// once, plainly, rather than left to be inferred from the facet.
	if plan.Runtime == ContainerRuntimePodman && !plan.NestsInJobScope() {
		lines = append(lines, "confine: this container's placement was left to your own "+
			plan.CallerCgroupFlag+"; AIRA injected no placement flag and cannot establish that the container "+
			"is inside this job's scope or bounded by its cap.")
	}

	// The false-belief direction, named in BOTH runtimes rather than only for
	// docker: a caller who typed a container limit above this job's own limit is
	// wrong in both cases, just wrong differently.
	if plan.MemoryEstablished && scopeMemoryMax > 0 && plan.MemoryBytes > scopeMemoryMax {
		container := FormatConfineBytes(plan.MemoryBytes)
		job := FormatConfineBytes(scopeMemoryMax)
		switch {
		case plan.NestsInJobScope():
			// Only a NESTED container is actually bound by this job's cap, so
			// only here may AIRA say the kernel binds it. The basis is
			// load-bearing: this job's limit is very often an AIRA ESTIMATE
			// rather than anything the caller declared, and presenting an
			// estimate as "this job's cap" without saying where it came from
			// invites the same false belief the line exists to correct.
			basis := strings.TrimSpace(reserveBasis)
			if basis == "" {
				basis = "basis unevaluated"
			}
			lines = append(lines, "confine: container --memory="+container+" exceeds this job's cap "+job+
				" ("+basis+"); the kernel binds the container at "+job+".")
		case plan.Runtime == ContainerRuntimeDocker:
			lines = append(lines, "confine: docker --memory="+container+" exceeds this job's own limit "+job+
				"; the container is NOT bound by "+job+", nor by aira.slice.")
		default:
			lines = append(lines, "confine: container --memory="+container+" exceeds this job's own limit "+job+
				"; whether anything bounds the container depends on the placement you chose.")
		}
	}

	if plan.Detach {
		switch {
		case plan.NestsInJobScope():
			// Nested, therefore inside the scope cgroup.kill takes down. True for
			// a caller-supplied `--cgroups=split` exactly as for an injected one
			// (build review, Sol P2).
			lines = append(lines, "confine: this container will be killed when the confine job exits.")
		case plan.Runtime == ContainerRuntimeDocker:
			lines = append(lines, "confine: this container outlives the confine job; any reservation made here is "+
				"released when the job exits and does not cover the container's lifetime.")
		default:
			lines = append(lines, "confine: whether this container outlives the confine job depends on the "+
				"placement you chose; AIRA cannot establish it.")
		}
	}
	return lines
}

// ContainerMemoryFacet finishes the `container-memory=` trailer value. The
// reserve half is appended ONLY to a caller-declared limit, and `:reserved` is
// claimed only when the daemon actually granted the charge -- the flock fallback
// reports an "immediate" admission that is a slice free-memory check, not a
// ledger charge, so keying this on the admission STATE would claim a charge that
// never happened.
func ContainerMemoryFacet(plan ContainerPlan, injection ContainerInjection, skip string, ledgerCharged bool) string {
	facet := injection.MemoryFacet
	if !plan.Detected() || !plan.MemoryEstablished {
		return facet
	}
	switch {
	case skip != "":
		return facet + ":reserve-skipped:" + skip
	case ledgerCharged:
		return facet + ":reserved"
	default:
		return facet + ":reserve-requested"
	}
}

func containerBase(path string) string {
	if index := strings.LastIndexByte(path, '/'); index >= 0 {
		return path[index+1:]
	}
	return path
}

func containerAllIn(body, allowed string) bool {
	if body == "" {
		return false
	}
	for _, r := range body {
		if !strings.ContainsRune(allowed, r) {
			return false
		}
	}
	return true
}
