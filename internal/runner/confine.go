package runner

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

const (
	DefaultConfineSlice = "aira.slice"
	// A project-less invocation has no per-project peak-RSS history. Four GiB is
	// a conservative #50 no-history fallback; injected callers may override it.
	DefaultConfineMemoryReserve = int64(4 << 30)
	// DefaultConfineCPUCores is the CPU-core reservation a plain `aira confine`
	// declares to the admission ledger (design §9). One core is the confine default;
	// the daemon charges it against the machine-wide 2×NumCPU CPU ceiling (design
	// §7). Accounting only — no cpu.max is written. Per-command CPU annotation
	// (aitest workers) is a later slice; the confine path always sends this.
	DefaultConfineCPUCores = int64(1)
	// Under --delegate-ram the suite's OWN reserve must be a small PINNED
	// framework overhead — never the unpinned whole-command estimate, which
	// could inflate via history to reject the whole suite E_ADMIT_TOO_LARGE.
	// Until AIRA-33 the reason was that the deleted pytest plugin took a
	// separate per-test reservation the whole-command estimate would double-book
	// in queue.outstanding. That caller is gone; the constant and its value are
	// unchanged, and the reason is now aitest: a delegate job's containment is
	// per-WORKER, in nested sub-scopes granted by worker-admit under this job's
	// own outer ceiling, so charging the slice a whole-suite peak on top would
	// reserve for growth the slice ledger never sees.
	DefaultDelegateRAMOverhead = int64(512 << 20)
	// DefaultDelegateRAMScopeCeiling is the compiled-in containment cap used
	// whenever a daemon ceiling is unavailable. It is not an admission charge.
	DefaultDelegateRAMScopeCeiling = int64(48 << 30)
	// AdmitWaitCeiling is the single upper bound on a requested admission wait,
	// shared by the CLI, this runner, and the daemon (AIRA-58). It is a TYPO
	// GUARD, not a policy: real waits on a contended shared slice routinely run
	// into the hundreds or thousands of seconds, and the resources a long wait
	// actually consumes are already bounded elsewhere (the daemon's per-slice
	// admitMaxWaiters and its global admitGlobalMax connection gate).
	//
	// It is ONE constant on purpose. Three independent 30-minute ceilings had
	// drifted into the codebase — daemon admit, daemon worker-admit, and this
	// runner — and the runner's silently clamped every request BEFORE it reached
	// the daemon. A caller that exceeds this is REFUSED and told the ceiling, never
	// silently substituted. Its live callers since S13 are `confine-reserve
	// --max-wait` (which applies its value as a real ctx deadline) and the
	// `run.admission_max_wait` project-config key (validated here, then vestigial —
	// see DefaultConfineAdmissionWait).
	AdmitWaitCeiling = 24 * time.Hour
	// DefaultConfineAdmissionWait was how long a confine launch waited to be ADMITTED
	// when no --admit-timeout was given. S13 removed --admit-timeout and the admission
	// wait no longer self-expires (design §4/§6: a blocking wait ends on the grant or
	// on ctx cancellation, never a client-side timeout), so this constant no longer
	// bounds anything: admitConfine still assigns it to the runner's admissionMaxWait,
	// but that field is now read only by the vestigial wait-ceiling TYPO GUARD in
	// admit(). It is a flagged §6 collision (owner question), kept rather than deleted
	// pending that decision. ConfineRequest.Timeout — a real job deadline — is a
	// different quantity and the two are routinely confused.
	DefaultConfineAdmissionWait = 30 * time.Minute
	// MinPinnedScopeCap is the smallest reserve a caller may DECLARE. It mirrors
	// the minimum `aira confine --memory-reserve` already accepts, so the CLI, the
	// runner and the daemon all agree on one bound.
	//
	// A declared reserve below it is REFUSED at the runner boundary, not silently
	// launched uncapped: a declared reserve becomes the scope memory.max, so
	// quietly dropping the cap for a sub-minimum value would mean the same request
	// is contained when the daemon answers and uncontained when it does not —
	// the divergence this whole change exists to remove, and another instance of
	// the silent substitution AIRA-58 removed.
	//
	// Note this is NOT what protects callers that pass a token reserve (several
	// tests pass 1): those never set MemoryReservePinned, so provenance alone
	// excludes them. The two mechanisms are independent.
	MinPinnedScopeCap = int64(1 << 20)
)

// ResolveConfineReserve is the SINGLE place that decides what a confine job
// charges the shared admission ledger, and whether that number is pinned (the
// daemon honours it verbatim) or a hint (the daemon resolves its own
// history-based estimate). Every launch path resolves through here.
//
// It is deliberately a pure function of the request, exported, and in the
// portable file: AIRA-62 was a bug of exactly the shape that arises when this
// decision is duplicated. `cmd/aira` used to pre-resolve it in the CLI —
// `if maximum > 0 { reserve, reservePinned = maximum, true }`, unconditional and
// with no delegate-ram guard — which ran BEFORE the runner and left the runner's
// own correct `!DelegateRAM` carve-out below as dead code for the only non-test
// producer of a ConfineRequest there is. The result: a
// `--delegate-ram --memory-max 32G --memory-reserve 512M` suite charged the
// ledger 32G instead of the 512M it explicitly asked for, a 64x silent
// over-reservation on a shared 63G slice (AIRA-24, AIRA-59, AIRA-67).
//
// So the CLI now transcribes and this decides. Being exported is what lets the
// CLI's own tests prove the COMPOSITION — that the request `cmd/aira` builds
// charges what the operator asked for — against this production code rather
// than against a restatement of it.
//
// The two rules, unchanged in substance from the runner code this replaces:
//
//   - No reserve given: a delegate-ram job pins a small framework overhead,
//     because its per-test children reserve individually and charging the
//     whole-command estimate would double-book them. Anything else takes the
//     unpinned no-history fallback, which the daemon is free to re-estimate.
//   - A non-delegate `--memory-max` SETS the reserve to the cap. That is
//     deliberate and documented (internal/core/skill.go:318): such a scope may
//     genuinely grow to its cap and nothing else reserves on its behalf, so
//     booking less would under-book the shared ledger. Note it sets rather than
//     raises: it is an UP-charge in the case the docs describe (reserve below
//     cap, "you cannot cap high and reserve low"), but a declared reserve LARGER
//     than the cap is lowered to the cap — still exact, never under-booked,
//     since the scope cannot exceed its own memory.max. A delegate-ram cap is a
//     containment CEILING, not a reserve, so it must not do either.
//
// verifies: AIRA-62
func ResolveConfineReserve(request ConfineRequest) (reserve int64, pinned bool) {
	reserve = request.MemoryReserve
	pinned = request.MemoryReservePinned || reserve > 0
	if reserve <= 0 {
		if request.DelegateRAM {
			reserve = DefaultDelegateRAMOverhead
			pinned = true
		} else {
			reserve = DefaultConfineMemoryReserve
		}
	}
	if !request.DelegateRAM && request.ScopeMemoryMax > 0 {
		reserve = request.ScopeMemoryMax
		pinned = true
	}
	return reserve, pinned
}

type ConfineAdmission string

const (
	ConfineAdmissionAdmitted    ConfineAdmission = "admitted"
	ConfineAdmissionTimeout     ConfineAdmission = "timeout"
	ConfineAdmissionUnevaluated ConfineAdmission = "unevaluated"
)

type ConfineCap string

const (
	ConfineCapEnforced    ConfineCap = "enforced"
	ConfineCapUnevaluated ConfineCap = "unevaluated"
)

type ConfineScope string

const (
	ConfineScopePlaced     ConfineScope = "placed"
	ConfineScopeUnverified ConfineScope = "unverified"
)

type ConfineOOMGroup string

const (
	ConfineOOMGroupSet        ConfineOOMGroup = "set"
	ConfineOOMGroupUnverified ConfineOOMGroup = "unverified"
)

// ConfineSwapCapUnevaluated is the AIRA-110 swap-bound disposition for a launch
// that never reached the memory.swap.max write. The three ESTABLISHED values are
// the WorkerAdmitSwapCap* catalogue (enforced / not-applicable / unavailable) --
// one vocabulary for one primitive, rather than a second set of spellings for
// the same three kernel facts. This value is deliberately not in that catalogue:
// it is the absence of an answer, not an answer.
const ConfineSwapCapUnevaluated = "unevaluated"

type ConfinePriorities string

const (
	ConfinePrioritiesApplied    ConfinePriorities = "applied"
	ConfinePrioritiesUnverified ConfinePriorities = "unverified"
)

// ConfineCPUWeight reports whether the foreground supervisor established the
// optional CPU-weight aging control. It is deliberately independent of the
// fail-closed memory confinement facets.
type ConfineCPUWeight string

const (
	ConfineCPUWeightAging       ConfineCPUWeight = "aging"
	ConfineCPUWeightUnavailable ConfineCPUWeight = "unavailable"
)

// ConfineContainment (AIRA-121) names WHAT KIND of containment this launch
// actually has. It exists because every other facet on this trailer describes a
// property of a cgroup scope, and in ci-shim mode there IS no scope: without
// this facet a shim job's trailer would say `cap=unevaluated scope=unverified`
// -- a set of absences an operator reads as "something failed", not as "this
// machine has no kernel backstop at all, by design".
//
// Three values, never two. The plan proposed exactly `enforced` and `advisory`
// on the argument that both are always established; that is untrue for a launch
// that aborted before the finite-cap gate ran, and rendering `enforced` for such
// a status would be a fabricated containment claim -- the single most misleading
// value this trailer could carry.
type ConfineContainment string

const (
	// ConfineContainmentEnforced: a per-job cgroup scope exists under a
	// finite-capped slice, with memory.oom.group and cgroup.kill behind it.
	ConfineContainmentEnforced ConfineContainment = "enforced"
	// ConfineContainmentAdvisory: ci-shim. Admission is a RAM-budget ledger and
	// nothing else -- no cgroup, no memory.max, no kill backstop. A job that
	// exceeds its booked reserve is not killed. The value spells out all three
	// absences because the whole point of the facet is that "advisory" alone
	// would be read as a weaker flavour of the same guarantee.
	ConfineContainmentAdvisory ConfineContainment = "advisory(ci-shim,no-cgroup,no-kill-backstop)"
	// ConfineContainmentUnevaluated: the launch never got far enough to
	// establish either. Never rendered as enforced.
	ConfineContainmentUnevaluated ConfineContainment = "unevaluated"
)

// AitestBackendCanFunction reports whether aitest's per-worker RAM admission
// backend can work for a launch in this mode, and NAMES the backend that would
// serve it. It is the ONE gate on publishing the AIRA_AITEST_* coordinates to a
// child, and the returned grade is what the launch DIAGNOSTIC then states.
//
// A consumer's conftest.py uses the PRESENCE of AIRA_AITEST_LIB alone as the
// guard that activates the aitest plugin. AIRA-121 therefore withheld it in
// ci-shim mode: worker-admit could only mean "nest a kernel-enforced cgroup
// sub-scope", nothing could nest one there, and a heavy suite would have run
// under an apparent governance mechanism with no backstop -- "invisible until
// something OOMs".
//
// AIRA-123 changed the fact that reasoning rested on, so the rule changes with
// it. worker-admit now makes a real ADMISSION decision in ci-shim mode against
// the in-daemon RAM-budget ledger -- no cgroup, no memory.max, no kill backstop,
// and reported as `advisory(ci-shim,no-cgroup,no-kill-backstop)` on every single
// grant. That is genuinely weaker than the real path, and it is genuinely better
// than the alternative it replaced: withholding the variable falls the consumer
// through to plain pytest-xdist, where per-worker RAM is invisible to everything
// and nothing prevents over-subscription at all -- this project's own AIRA-11
// incident class (a parallel leg observed peaking ~35 GiB RSS with no per-test
// awareness). The rule is therefore CONDITIONAL, never a flat never: export when
// the backend can actually function, and say which backend it is.
//
// An unrecognised mode reports false. That is the fail-closed direction and the
// reason this returns a pair rather than a bare true: a future mode gets no
// coordinates until someone decides which backend serves it.
func AitestBackendCanFunction(mode string) (string, bool) {
	switch mode {
	case ConfineModeReal:
		return AitestAdmissionSubScope, true
	case ConfineModeShim:
		return AitestAdmissionLedgerOnly, true
	default:
		return "", false
	}
}

// ConfineStatus keeps the independently verified cap, admission, placement,
// OOM-group, and priority facets separate. In particular, a successful
// admission never fabricates a cap snapshot or implies priority success.
// The json tags exist for AIRA-22's durable detached-job record, which stores the
// STRUCTURED status rather than the rendered trailer so that FormatConfineStatus
// stays the single operator-facing projection and a detached job's trailer cannot
// drift from a foreground one's.
type ConfineStatus struct {
	Slice string `json:"slice,omitempty"`
	// Containment is AIRA-121's kind-of-containment facet. Always rendered on
	// the trailer (see FormatConfineStatus); empty reads as unevaluated, never
	// as enforced.
	Containment          ConfineContainment        `json:"containment,omitempty"`
	Cap                  ConfineCap                `json:"cap,omitempty"`
	Admission            ConfineAdmission          `json:"admission,omitempty"`
	AdmissionState       string                    `json:"admission_state,omitempty"`
	AdmissionWaitedMS    int64                     `json:"admission_waited_ms,omitempty"`
	CapBytes             int64                     `json:"cap_bytes,omitempty"`
	ReserveBytes         int64                     `json:"reserve_bytes,omitempty"`
	Scope                ConfineScope              `json:"scope,omitempty"`
	ScopeIntegrity       ScopeIntegrity            `json:"scope_integrity,omitempty"`
	DescendantEscape     *DescendantEscapeEvidence `json:"descendant_escape,omitempty"`
	OOMGroup             ConfineOOMGroup           `json:"oom_group,omitempty"`
	Priorities           ConfinePriorities         `json:"priorities,omitempty"`
	CPUWeight            ConfineCPUWeight          `json:"cpu_weight,omitempty"`
	ScopeMemoryMax       int64                     `json:"scope_memory_max,omitempty"`
	ScopeMemoryHigh      int64                     `json:"scope_memory_high,omitempty"`
	ScopeMemoryBinding   string                    `json:"scope_memory_binding,omitempty"`
	ScopeMemoryEffective int64                     `json:"scope_memory_effective,omitempty"`
	// ScopeMemoryCapSource is AIRA-133's cap-PROVENANCE facet: WHOSE number the
	// enforced scope memory.max is. One of the ConfineCapSource* values below.
	//
	// It exists because a job killed at a cap AIRA estimated and a job killed at
	// a cap the operator typed produced byte-identical output, while wanting
	// opposite responses: the first self-heals on an identical re-run (the OOM is
	// recorded against the command's signature and the next admission is sized
	// higher), the second never will, because nothing about a `--memory-max` the
	// operator chose changes when the command is run again.
	//
	// The value is recorded at the single branch in confineWithDeps that CHOOSES
	// the cap, so it is the provenance itself rather than a re-derivation: no
	// caller decodes the reserve-basis string, and no heuristic infers "this
	// looks operator-supplied". Empty whenever no cap was enforced (there is then
	// no provenance to state, and `scope-memory.max=not-requested` already says
	// so); an enforced cap with no recorded source renders as unevaluated, never
	// as either party's choice.
	ScopeMemoryCapSource string `json:"scope_memory_cap_source,omitempty"`
	// ScopeSwapCap is AIRA-110's swap-bound disposition: one of the
	// WorkerAdmitSwapCap* values (the vocabulary AIRA-35 minted for the same
	// primitive on aitest worker scopes). It exists because `scope-memory.max=
	// enforced=N` is true as written but reads as a containment guarantee, and on
	// a host where swap could NOT be bounded it is not one -- the job can grow
	// past N into swap without ever being killed. Empty means the write never
	// ran (a launch that failed earlier) and renders as unevaluated, never as a
	// claim that swap is bounded.
	ScopeSwapCap string `json:"scope_swap_cap,omitempty"`
	ReserveBasis string `json:"reserve_basis,omitempty"`
	// PeakRSS, CPUUser, and CPUSys are AIRA-104's whole-subtree resource
	// counters: cgroup v2's own memory.peak and cpu.stat for this scope and
	// every descendant ever charged to it (aitest worker sub-scopes and a
	// podman --cgroups=split child included), read once at the same teardown
	// point so a job that forks many short-lived subprocesses is still
	// measured completely. Nil means unestablished (an unreadable file, or a
	// kernel too old to carry it), never a fabricated zero -- the same
	// discipline as every other optional facet on this struct. Named to
	// mirror RunRecord's existing CPUUser/CPUSys rather than inventing a
	// single combined usage field.
	PeakRSS *int64 `json:"peak_rss,omitempty"`
	CPUUser *int64 `json:"cpu_user,omitempty"`
	CPUSys  *int64 `json:"cpu_sys,omitempty"`
	// TerminatedBy is the terminal-attribution facet (AIRA-70, AIRA-91 Part A).
	// It carries a json tag like its siblings because AIRA-22's durable record
	// stores this struct, and this is the facet such a record most needs.
	// Empty means the classifier never ran -- every launch that failed before
	// the child was waited on -- and renders as "unevaluated", never as a claim
	// that the job ended cleanly.
	TerminatedBy string `json:"terminated_by,omitempty"`
	// Container and ContainerMemory are AIRA-102's container-integration facets.
	// They are ABSENT unless a `docker run`/`podman run` was detected, so every
	// ordinary trailer is byte-identical to before -- the same discipline the
	// Exclusive facet below follows. json tags because AIRA-22's durable detached
	// record stores this struct.
	//
	// Container names the ACTION confine took, never an outcome it did not
	// observe: injecting `--cgroups=split` establishes that AIRA asked podman to
	// nest the container, not that podman honoured it (an absent or pre-2.0
	// podman rejects the flag and exits).
	Container       string `json:"container,omitempty"`
	ContainerMemory string `json:"container_memory,omitempty"`
	// Exclusive is AIRA-101's must-know result: whether this run actually had the
	// slice to itself. Empty for a run that never asked; otherwise one of the
	// ConfineExclusive* values below.
	//
	// It exists because of a real incident: an hour of benchmark throughput
	// numbers was invalidated by contention nobody noticed. A run that quietly
	// loses exclusivity produces numbers that LOOK clean, so the outcome has to
	// travel on the exit path where a harness can read it, not merely in prose.
	//
	// ExclusiveDrainedMS is how long the job waited for the slice to drain — the
	// acquisition condition, carried with the result rather than left to the
	// operator's memory.
	Exclusive          string `json:"exclusive,omitempty"`
	ExclusiveDrainedMS int64  `json:"exclusive_drained_ms,omitempty"`
	// AIRA-138. The two job-deadline facets, one per bound, on the same
	// absent-unless-requested discipline as Exclusive above: a bound nobody asked
	// for has no outcome to hide, so omitting the field fabricates nothing and
	// every existing trailer stays byte-identical.
	//
	// They are LOAD-BEARING rather than decorative. A confine deadline never
	// causes Confine to return an error — the exit code stays a pass-through of
	// the job's own (§5.4 of the plan) — and ConfineResult has no ErrorCodes
	// array, so this field is the ONLY machine-readable carrier of "a bound
	// fired". The budget travels with the state so the trailer names the knob to
	// change; a zero budget means the bound was not requested and nothing renders.
	TimeoutBudget    time.Duration        `json:"timeout_budget,omitempty"`
	Timeout          ConfineDeadlineState `json:"timeout,omitempty"`
	CPUTimeoutBudget time.Duration        `json:"cpu_timeout_budget,omitempty"`
	CPUTimeout       ConfineDeadlineState `json:"cpu_timeout,omitempty"`
}

// ConfineDeadlineState is the CLOSED trailer vocabulary for one requested job
// bound (AIRA-138). The empty string means the bound was not requested and the
// field is not rendered at all.
//
// The three fired-kill-* / fired-not-executed values describe the KILL
// OPERATION, not the cause of death: `terminated-by` owns causation, and the two
// are allowed to disagree. `cpu-timeout=10m:fired-kill-completed` beside
// `terminated-by=normal` and `exit 0` is not a contradiction — it says AIRA wrote
// cgroup.kill, the scope did end up empty, and the job had already exited on its
// own between the membership read and the write.
type ConfineDeadlineState string

const (
	// ConfineDeadlineNotReached: requested, and THIS bound did not fire. For the
	// CPU bound it additionally means the final established total is under budget
	// — a two-sided proof that the bound held.
	ConfineDeadlineNotReached ConfineDeadlineState = "not-reached"
	// ConfineDeadlineFiredKillCompleted: this bound fired, cgroup.kill was written
	// and waitEmpty then CONFIRMED the scope empty.
	ConfineDeadlineFiredKillCompleted ConfineDeadlineState = "fired-kill-completed"
	// ConfineDeadlineFiredKillUnconfirmed: this bound fired and the cgroup.kill
	// write succeeded, but emptiness was not confirmed afterwards.
	ConfineDeadlineFiredKillUnconfirmed ConfineDeadlineState = "fired-kill-unconfirmed"
	// ConfineDeadlineFiredNotExecuted: this bound fired and provably delivered NO
	// signal — the scope was verified empty by two independent reads before any
	// write and the job's leader was proved dead — so the child's own exit is
	// reported. AIRA-126's arbitrated exit, in confine's currency.
	ConfineDeadlineFiredNotExecuted ConfineDeadlineState = "fired-not-executed"
	// ConfineDeadlineFiredUnevaluated: this bound fired and AIRA cannot establish
	// what its kill did. Never rendered as either a kill or a no-op.
	ConfineDeadlineFiredUnevaluated ConfineDeadlineState = "fired-unevaluated"
	// ConfineDeadlineUnenforced: CPU bound only. It did not fire, and either
	// nothing was ever measured or the final established total reached the budget
	// with no executed CPU-budget kill. The wall bound has no such state: a wall
	// timer either fired or the job ended first, and there is no measurement that
	// can be unavailable.
	ConfineDeadlineUnenforced ConfineDeadlineState = "unenforced"
)

// ConfineDeadlineLabel names the FLAG a deadline facet belongs to, so the
// trailer FIELD and the fire-time diagnostic both point at the knob an operator
// would change. Two renderers, one vocabulary.
const (
	ConfineDeadlineLabelWall = "timeout"
	ConfineDeadlineLabelCPU  = "cpu-timeout"
)

// The `terminated-by=deadline:<bound>` suffixes, which are deliberately NOT the
// flag names above. `terminated-by` answers "what ended this job", and it reads
// as a CAUSE — `deadline:wall` and `deadline:cpu` name the quantity that was
// exceeded, where `deadline:timeout` would name a knob and say nothing about
// which clock ran out. The flag names still appear on the same trailer line, on
// the `timeout=`/`cpu-timeout=` fields, so nothing is lost: the operator reads
// the cause on one field and the knob on the other.
const (
	ConfineDeadlineBoundWall = "wall"
	ConfineDeadlineBoundCPU  = "cpu"
)

// The cap-provenance vocabulary (AIRA-133). Rendered as `cap-source=` beside
// `scope-memory.max=enforced=N`, and read by formatConfineReserveAdvisory to
// choose which next step an OOM at that cap actually warrants.
//
// The split that matters is operator: vs auto:. Everything under operator: is a
// number the caller typed, which an identical re-run cannot change; everything
// under auto: is a number AIRA chose, which an identical re-run can. The
// finer-grained value inside each half names the exact flag or mechanism, so the
// advisory can point at the right knob without a second lookup.
const (
	// ConfineCapSourceMemoryMax: the operator passed --memory-max. It wins over
	// every other source (confineWithDeps tries it first), including the
	// delegate-ram learned ceiling.
	ConfineCapSourceMemoryMax = "operator:--memory-max"
	// ConfineCapSourceMemoryReserve: no --memory-max, but the operator DECLARED a
	// --memory-reserve, which is enforced as the scope cap. Keyed on the
	// declared-ness captured before ResolveConfineReserve widens `pinned`, so a
	// token reserve some caller passed without declaring it never lands here.
	ConfineCapSourceMemoryReserve = "operator:--memory-reserve"
	// ConfineCapSourceDaemonReserve: the cap is the reserve the DAEMON resolved
	// and granted for an unpinned job. Deliberately not called "estimate": the
	// grant is usually history-derived (`reserve-basis=estimate:*`) but can be a
	// fallback the daemon returned when history was unusable, and the trailer's
	// own reserve-basis field beside this one says which. What this value claims
	// is only what it can establish — AIRA chose this number, not the caller.
	ConfineCapSourceDaemonReserve = "auto:daemon-reserve"
	// ConfineCapSourceDelegateRAM: a --delegate-ram job with no --memory-max of
	// its own, capped at the daemon's learned scope ceiling or, when the daemon
	// supplied none, at the compiled-in fallback. Also AIRA's number, not the
	// caller's, but a whole-scope CEILING rather than the job's reserve — so an
	// OOM here says the suite outgrew its ceiling, not that a reserve estimate
	// was low.
	ConfineCapSourceDelegateRAM = "auto:delegate-ram"
	// ConfineCapSourceUnevaluated: a cap IS enforced but no branch recorded where
	// it came from. Never rendered as either party's choice.
	ConfineCapSourceUnevaluated = "unevaluated"
)

// ConfineCapSourceIsOperator reports whether an enforced cap is a number the
// CALLER supplied. False for `unevaluated` and for the empty string: an
// unestablished provenance must never be read as "the operator asked for this",
// which is the half that tells a reader re-running is pointless.
func ConfineCapSourceIsOperator(source string) bool {
	return source == ConfineCapSourceMemoryMax || source == ConfineCapSourceMemoryReserve
}

// The exclusivity-outcome vocabulary (AIRA-101). Rendered on the trailer
// whenever --exclusive was requested, in the same always-rendered discipline as
// TerminatedBy: before that facet existed a SIGKILLed job's trailer was
// byte-identical to a clean one's, and the same trap applies here — a contended
// benchmark whose trailer looks like an uncontended one is precisely the failure
// this facet exists to end.
const (
	// ConfineExclusiveGranted: the daemon granted exclusivity and the lease was
	// still held at teardown. As uncontended as this primitive can establish —
	// subject to the documented coverage limits (processes placed in the slice by
	// hand, and Docker containers, which live outside the slice entirely).
	ConfineExclusiveGranted = "granted"
	// ConfineExclusiveLost: exclusivity WAS granted, then the admission lease
	// closed mid-run (a daemon restart or stop). The job was no longer scheduled
	// alone; treat any measurement from it as contended. Never a reason to kill
	// the job — killing a benchmark because the daemon restarted is worse than
	// reporting it.
	ConfineExclusiveLost = "lost"
	// ConfineExclusiveUnevaluated: exclusivity was requested and the outcome could
	// not be established. Never rendered as "granted" by default.
	ConfineExclusiveUnevaluated = "unevaluated"
)

// The terminal-attribution vocabulary. Each value names exactly what its
// evidence establishes and nothing further; classifyConfineTermination
// (confine_linux.go) documents the order in which the evidence is weighed.
const (
	// ConfineTerminatedNormal: the child exited of its own accord. Any exit
	// code -- the facet reports HOW a job ended, not whether it succeeded.
	ConfineTerminatedNormal = "normal"
	// ConfineTerminatedOOM: the kernel's OOM killer killed a signalled child in
	// this scope (memory.events oom_kill readable and positive).
	ConfineTerminatedOOM = "oom"
	// ConfineTerminatedUnattributedSIGKILL: a SIGKILL that neither this
	// supervisor nor this scope's own OOM counter accounts for. NOT named
	// "external-cgroup-kill", which the earlier plan draft proposed: an
	// external kill -9 on the leader and a job that SIGKILLs itself share this
	// exact signature, so naming one mechanism would assert a cause the
	// evidence cannot establish. The candidate mechanisms are named on the
	// trailer's candidates line instead.
	//
	// An ancestor-cgroup OOM is NOT among them, contrary to earlier revisions
	// of this comment: oom_kill is keyed on the VICTIM's cgroup, so an ancestor
	// OOM that killed our processes raises OUR local counter and lands on
	// ConfineTerminatedOOM instead. Measured, not reasoned --
	// TestMemoryEventsLocalDistinguishesOwnLimitFromDescendantOOM.
	ConfineTerminatedUnattributedSIGKILL = "unattributed-sigkill"
	// ConfineTerminatedUnevaluated: the wait status could not be decoded, or the
	// child was SIGKILLed and memory.events could not be read -- in which case
	// an OOM kill and an unattributed one are indistinguishable, and reporting
	// either would be a fabricated zero.
	ConfineTerminatedUnevaluated = "unevaluated"
	// ConfineTerminatedSupervisorSignalPrefix: this confine supervisor itself
	// received the named signal during the run and tore the job down.
	ConfineTerminatedSupervisorSignalPrefix = "supervisor-signal:"
	// ConfineTerminatedChildSignalPrefix: the child died of the named signal,
	// which is not SIGKILL -- so neither cgroup.kill nor memory.oom.group, both
	// of which deliver SIGKILL and nothing else, can have been the cause. Who
	// sent it is not established.
	ConfineTerminatedChildSignalPrefix = "child-signal:"
	// ConfineTerminatedDeadlinePrefix: a job deadline this supervisor was asked
	// for fired and its cgroup.kill write SUCCEEDED, so SIGKILL was delivered to
	// every member of the scope (AIRA-138). Suffixed with the QUANTITY that was
	// exceeded — ConfineDeadlineBoundWall or ConfineDeadlineBoundCPU, i.e.
	// `deadline:wall` / `deadline:cpu` — never with the flag name, because
	// terminated-by states a cause and the flag names live on the same line's
	// `timeout=`/`cpu-timeout=` fields.
	//
	// It sits BELOW `oom` and `supervisor-signal:` in the classifier, and above
	// `unattributed-sigkill`, whose advisory positively claims the supervisor
	// "sent no signal itself" — a sentence that becomes false the instant a
	// deadline kill writes cgroup.kill.
	ConfineTerminatedDeadlinePrefix = "deadline:"
)

type ConfineRequest struct {
	Slice               string
	Name                string
	Owner               string
	ScopeID             string
	Argv                []string
	Env                 []string
	RuntimeDir          string
	AdmitSocketPath     string
	MemoryReserve       int64
	MemoryReservePinned bool
	DelegateRAM         bool
	// AIRA-222. RequireAdmission fails the launch CLOSED when memory admission
	// could not be evaluated (admission=unevaluated), instead of running the job
	// UNGOVERNED and exiting 0 — the defect where "governed, slice was empty" and
	// "not governed at all" shared an exit code, so a mis-provisioned CI host ran
	// every job ungoverned and looked fine. Opt-in and default false: only a
	// caller that sets it (CI or anything unattended, where the single stderr
	// warning has no reader) is refused; ordinary launches, and a transient
	// daemon-restart window, are unaffected.
	RequireAdmission bool
	// AIRA-101. Exclusive asks the daemon to schedule this job ALONE in its slice,
	// for uncontended benchmarking. Fail-closed end to end: if exclusivity cannot
	// be established the launch is REFUSED, never silently downgraded, because a
	// benchmark that runs contended while believing otherwise produces numbers
	// that look clean — the incident this flag exists to prevent.
	Exclusive bool
	// AIRA-185. ExclusiveReason is a free-text label for WHY the slice is being
	// held exclusively ("deploy: slice-ceiling flip"), which `aira drain wait
	// --reason` supplies so `confine --list` can attribute the hold to a purpose
	// rather than to a placeholder Name. Name cannot carry it: it must be a valid
	// confine identity (no spaces, no colons) and must match the scope id.
	//
	// DIAGNOSTIC ONLY, and IGNORED unless Exclusive is set — see
	// Request.ExclusiveReason for why forwarding it on a non-exclusive request
	// would turn a harmless label into a refused launch.
	ExclusiveReason  string
	ScopeMemoryMax   int64
	ScopeMemoryHigh  int64
	AdmissionMaxWait time.Duration
	// AIRA-138. Timeout and CPUTimeout are the JOB's two bounds, and neither is
	// AdmissionMaxWait: that one bounds the ADMISSION WAIT and nothing else.
	//
	// Both clocks start at the RELEASE WRITE — the instant the setup shim is
	// released to execve the job — so neither includes the admission wait (which
	// on a contended shared box can legitimately be minutes) nor the setup
	// handshake. That is not incidental placement: a wall bound measured from
	// invocation would fire against a job that had not run for its budget at all,
	// a fabrication in the EARLY direction, which is the one direction AIRA-136's
	// invariant forbids.
	//
	// CPUTimeout is cumulative user+system CPU-time over the scope AND every
	// descendant, sampled from cpu.stat — the quantity `ulimit -t` (per-process)
	// and cpu.max (a bandwidth throttle, never an end) both fail to express.
	Timeout           time.Duration
	CPUTimeout        time.Duration
	PollInterval      time.Duration
	HandshakeTimeout  time.Duration
	Stdin             io.Reader `json:"-"`
	Stdout            io.Writer `json:"-"`
	Stderr            io.Writer `json:"-"`
	SelfPath          string
	ResourceSignature string
	// DetachStateDir is the durable record-store root a detached launch writes
	// under (AIRA-22). The runner never derives it: internal/runner must not
	// import internal/daemon, so the CLI transcribes it exactly as it already
	// transcribes RuntimeDir and AdmitSocketPath.
	DetachStateDir string

	// StdinConnect asks a DETACHED supervisor to give its job a real stdin --
	// one end of a pipe fed by a per-job socket -- instead of /dev/null, so
	// `aira confine-input` can write to it (AIRA-196).
	//
	// It defaults to FALSE and must stay that way. A live pipe on the stdin of a
	// job that never expected input is a new hang class: anything that reads
	// stdin (a prompt, a `read`, a tool probing for a terminal) blocks forever
	// waiting for a writer that will never connect, where /dev/null returns EOF
	// immediately. Nothing here infers the flag; the launcher asks for it
	// explicitly or the job gets /dev/null.
	//
	// It is meaningful ONLY on the detached path. A foreground `aira confine`
	// already passes the caller's own stdin through, so the CLI refuses the flag
	// there rather than accepting and ignoring it.
	StdinConnect bool

	// There is deliberately NO `Detach bool` here. admitConfine transcribes
	// ConfineRequest fields onto a runner.Request, and Request.Detach arms
	// checkDetachAdmission, which dereferences r.ledger -- nil in confine's
	// project-less admitter. A detach flag on this struct would therefore be one
	// careless transcription away from a panic inside the detached supervisor,
	// where nothing would see it. Detachedness is carried by the ENTRY POINT
	// (Confine versus LaunchConfineDetached), so the trap cannot exist.

	// presetScopeID is the scope id a detached supervisor minted for ITSELF
	// before calling Confine, so that the pid embedded in the cgroup directory
	// name is the surviving supervisor's rather than the launcher's (AIRA-22
	// §3.2). It is UNEXPORTED on purpose: only package runner may set it, and
	// confineWithDeps additionally binds it to the request with
	// bindConfineScopeID, because the scope-id grammar alone happily accepts an
	// id naming a foreign pid, owner, or delegate class.
	presetScopeID string

	// BeforeAdmit is invoked exactly once, immediately before admission, i.e.
	// after every precondition confine checks synchronously (argv, reserve,
	// owner, slice resolution, backend probe, the finite-cap refusal, memory
	// delegation, scope-id mint) and before the only unbounded wait there is.
	// A non-nil error aborts the launch there: nothing has been admitted, no
	// scope exists and no child has started, so there is nothing to tear down.
	//
	// It exists so a detached launcher can be told "every synchronous
	// precondition passed" rather than being handed a premature exit 0 that
	// hides a failure the foreground form reports immediately.
	BeforeAdmit func(ConfineLaunchInfo) error `json:"-"`

	// OnPlaced is invoked exactly once at the point placement is PROVEN -- the
	// child is started and verified to be a member of the scope. It is the first
	// instant at which "running" is a fact rather than a guess, which is why a
	// detached job's status distinguishes `admitting` from `running` at all.
	// There is no error return: nothing remains to abort.
	OnPlaced func(ConfineLaunchInfo) `json:"-"`
}

// ConfineLaunchInfo is what the launch callbacks carry: resolved facts, never
// requested ones. In particular Slice is the slice actually resolved (which may
// be the whale.slice compatibility path, not what the caller typed) and CapBytes
// is the effective ceiling read out of the cgroup ancestry.
type ConfineLaunchInfo struct {
	ScopeID       string `json:"scope_id"`
	Slice         string `json:"slice"`
	CapBytes      int64  `json:"cap_bytes"`
	SupervisorPID int    `json:"supervisor_pid"`
}

const ConfineUnknownOwner = "unknown"

// ConfineInferredOwnerPrefix marks an owner AIRA derived for a caller that
// supplied none, rather than one the caller attested to (AIRA-23). '@' can never
// appear in a caller-supplied identity — ValidateConfineIdentity's alphabet
// excludes it — so an inferred owner cannot be forged into an attested one, and
// the two are distinguishable forever, including after a daemon restart.
const ConfineInferredOwnerPrefix = "@"

// maxConfineOwnerLen bounds the owner component so that the worst-case scope
// DIRECTORY name — ".aira-CONFINE-@dr-<name(100)>-<pid(7)>-<stamp(13)>@<owner>"
// — stays comfortably inside NAME_MAX (255). Names keep the wider 100-character
// identity bound because parseConfineScopeID must still accept every name ever
// minted; only the owner half is newly embedded, so only it is newly bounded.
const maxConfineOwnerLen = 64

// ValidateConfineIdentity applies the deliberately small scope-id component
// alphabet to cooperative owner identities. It prevents control characters or
// path-shaped values from crossing the CLI/admission boundary; it is not an
// authentication mechanism.
func ValidateConfineIdentity(value string) error {
	if value == "" {
		return errors.New("identity is empty")
	}
	if len(value) > 100 {
		return errors.New("identity is too long")
	}
	for _, r := range value {
		if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || strings.ContainsRune("_.-", r) {
			continue
		}
		return errors.New("identity requires letters, digits, '.', '_', or '-'")
	}
	return nil
}

// ValidateConfineOwner accepts BOTH owner forms — an attested identity, and an
// inferred one carrying ConfineInferredOwnerPrefix — under the tighter length
// bound an owner needs to be safe as a scope-directory-name component.
//
// Callers validating a CALLER-SUPPLIED owner (--owner, AIRA_CONFINE_OWNER, a
// discovered worktree id) must keep using ValidateConfineIdentity, whose
// alphabet excludes '@': that is what makes an inferred owner unforgeable.
// This one is for a value that may already have been inferred — the management
// verbs' caller identity, and the owner decoded back out of a scope id.
func ValidateConfineOwner(value string) error {
	if err := ValidateConfineIdentity(strings.TrimPrefix(value, ConfineInferredOwnerPrefix)); err != nil {
		return err
	}
	if len(value) > maxConfineOwnerLen {
		return fmt.Errorf("owner is too long (max %d)", maxConfineOwnerLen)
	}
	return nil
}

// InferConfineOwner derives a stable, launch-site-distinguishing owner for a
// caller that supplied neither --owner, AIRA_CONFINE_OWNER, nor a discoverable
// project worktree (AIRA-23). The reported hazard was a session about to
// pgrep-kill two SIBLING sessions' jobs, all showing OWNER "unknown", and being
// saved only by inspecting each process's cwd by hand — so cwd is exactly the
// discriminator that mattered, and printing it beats printing "unknown".
//
// It is deliberately PREFIXED and therefore never attested: see
// ConfineOwnerIsAttested. Two sessions sharing one directory infer the same
// value, so treating an inferred owner as proof of ownership would let one kill
// the other's job without --steal — the weakening AIRA-23 explicitly forbids.
// Returns ConfineUnknownOwner when the cwd yields nothing usable, because a
// fabricated identity is worse than an honest unknown.
func InferConfineOwner(cwd string) string {
	base := filepath.Base(strings.TrimSpace(cwd))
	var sanitized strings.Builder
	for _, r := range base {
		if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || strings.ContainsRune("_.-", r) {
			sanitized.WriteRune(r)
			continue
		}
		sanitized.WriteRune('-')
	}
	value := strings.Trim(sanitized.String(), "-.")
	if value == "" || value == "unknown" {
		return ConfineUnknownOwner
	}
	inferred := ConfineInferredOwnerPrefix + "cwd-" + value
	if len(inferred) > maxConfineOwnerLen {
		inferred = inferred[:maxConfineOwnerLen]
	}
	return inferred
}

// ConfineOwnerIsAttested reports whether owner is an identity its holder
// actually claimed, and is therefore usable as the kill guard's ownership
// proof. An empty owner, the literal "unknown", and any inferred owner are all
// unattested: the caller must pass --steal to kill such a job.
func ConfineOwnerIsAttested(owner string) bool {
	owner = strings.TrimSpace(owner)
	return owner != "" && owner != ConfineUnknownOwner && !strings.HasPrefix(owner, ConfineInferredOwnerPrefix)
}

type ConfineResult struct {
	Exit   int
	Status ConfineStatus
}

// ResolveConfineSlice applies only explicit flag > environment precedence.
// Linux resolves the machine default from live, injected state; portable code
// must not probe cgroups or silently select a compatibility slice.
func ResolveConfineSlice(flagValue string) string {
	if flagValue != "" {
		return flagValue
	}
	if value := os.Getenv("AIRA_CONFINE_SLICE"); value != "" {
		return value
	}
	return ""
}

// ConfineNeverRanFacet is the token that opens AIRA-147's never-ran trailer and
// appears on NO other line confine emits — FormatConfineStatus has no `ran=`
// facet, so `ran=no` is unambiguous by construction and a reader may key on it
// with a fixed-string match rather than a pattern.
const ConfineNeverRanFacet = "ran=no"

// FormatConfineNeverRan is the never-ran counterpart of FormatConfineStatus:
// the single operator-facing projection for a confine that returned an error
// (AIRA-147).
//
// It exists because the two outcomes were asymmetric in exactly the direction
// that misleads. A job that RAN gets a full trailer whose `terminated-by=`
// facet the Skill guide already teaches agents to read before the job's own
// output; a job that NEVER ran got no trailer at all — only a free-text error
// line and an exit code. And that exit code is deliberately NOT unique:
// AIRA-138 §5.4 passes a job's own status through verbatim, so a job that
// really ran and exited 4 is byte-identical at the exit-code layer to
// E_ADMIT_SATURATED never having started one. The reserved codes 1, 2 and 3
// collide the same way. This line is the machine-readable carrier of the one
// fact the exit code cannot carry.
//
// The claim is safe because it is not a new invariant: confine reserves error
// returns for "the confinement could not be established", every error return
// happens before the release write that lets the setup shim exec the target,
// and every path that reaches the target returns a nil error carrying its exit
// code. So a non-nil error from Confine means the target argv never executed,
// and this line states that and nothing more.
//
// Facets follow FormatConfineStatus's discipline: each is always rendered, and
// a value that was never established reads `unevaluated` rather than being
// omitted — a field silently missing from a diagnosis line is exactly the
// ambiguity these trailers exist to end.
func FormatConfineNeverRan(status ConfineStatus, err error) string {
	code := confineErrorCode(err)
	if code == "" {
		code = "unevaluated"
	}
	slice := status.Slice
	if slice == "" {
		slice = "unevaluated"
	}
	// The admission facet reads off AdmissionState, the wire-level state the
	// runner records BEFORE it returns an admission error, so the saturated
	// rejection this ticket is about renders `admission=saturated`. It is what
	// separates "the box was full, retry when it frees up" from "this launch was
	// never admissible as asked" — the same distinction the exit-code bucketing
	// in internal/codes already draws, made legible without parsing prose.
	admission := status.AdmissionState
	if admission == "" {
		admission = string(status.Admission)
	}
	if admission == "" {
		admission = "unevaluated"
	}
	return "confine: " + ConfineNeverRanFacet + " code=" + code + " slice=" + slice + " admission=" + admission
}

// confineErrorCode extracts the stable leading error code from a confine error.
// internal/runner must not import internal/store, so the extraction is local;
// the grammar is the project's own "CODE: detail" convention.
func confineErrorCode(err error) string {
	if err == nil {
		return ""
	}
	text := err.Error()
	colon := strings.IndexByte(text, ':')
	if colon <= 0 {
		return ""
	}
	candidate := text[:colon]
	if !strings.HasPrefix(candidate, "E_") && !strings.HasPrefix(candidate, "U_") && !strings.HasPrefix(candidate, "W_") {
		return ""
	}
	for _, r := range candidate {
		if r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '_' {
			continue
		}
		return ""
	}
	return candidate
}

// Confine runs a foreground command in a newly-created cgroup scope. Platform
// implementations must fail closed when the scope cannot be established.
//
// AIRA-147: this is the ONE funnel every confine launch passes through — the
// real Linux path, the ci-shim path, the non-Linux stub, and the detached
// supervisor — so the never-ran trailer is emitted HERE rather than at the ~25
// scattered confineUnavailable sites, which could not have stayed in step. The
// exit-code passthrough contract (AIRA-138 §5.4) is untouched: this adds one
// diagnostic line and changes no exit code, on any arm.
func Confine(ctx context.Context, request ConfineRequest) (ConfineResult, error) {
	result, err := confine(ctx, request)
	if err != nil {
		diagnostics := request.Stderr
		if diagnostics == nil {
			diagnostics = os.Stderr
		}
		_, _ = fmt.Fprintln(diagnostics, FormatConfineNeverRan(result.Status, err))
	}
	return result, err
}

func FormatConfineBytes(value int64) string {
	if value <= 0 {
		return "unknown"
	}
	for _, unit := range []struct {
		suffix string
		bytes  int64
	}{{"T", 1 << 40}, {"G", 1 << 30}, {"M", 1 << 20}, {"K", 1 << 10}} {
		if value%unit.bytes == 0 {
			return strconv.FormatInt(value/unit.bytes, 10) + unit.suffix
		}
	}
	return strconv.FormatInt(value, 10)
}

// FormatConfineStatus is the single operator-facing honesty projection.
func FormatConfineStatus(status ConfineStatus) string {
	capFacet := status.Cap
	if capFacet == "" {
		capFacet = ConfineCapUnevaluated
	}
	slice := status.Slice
	if slice == "" {
		slice = "unevaluated"
	}
	// AIRA-121. Always rendered, on the same discipline as terminated-by and
	// scope-swap.max: a trailer silent about WHAT KIND of containment it had is
	// indistinguishable between a kernel-enforced scope and a ci-shim job with no
	// backstop at all, which is exactly the silence this facet exists to end.
	containment := status.Containment
	if containment == "" {
		containment = ConfineContainmentUnevaluated
	}
	line := "confine: slice=" + slice + " containment=" + string(containment) + " cap=" + string(capFacet)
	if status.CapBytes > 0 {
		line += "(" + FormatConfineBytes(status.CapBytes) + ")"
	}
	if status.ReserveBytes > 0 {
		line += " reserve=" + FormatConfineBytes(status.ReserveBytes)
		if status.ReserveBasis != "" {
			line += " reserve-basis=" + status.ReserveBasis
		}
	}
	admissionFacet := status.AdmissionState
	if admissionFacet == "" {
		admissionFacet = string(status.Admission)
	}
	if admissionFacet != "" {
		line += " admission=" + admissionFacet
	}
	line += " scope=" + string(status.Scope)
	if status.ScopeIntegrity != "" {
		line += " scope-integrity=" + string(status.ScopeIntegrity)
	}
	if status.DescendantEscape != nil {
		line += " escaped-pid=" + strconv.Itoa(status.DescendantEscape.PIDIdentity.PID)
		line += " escaped-cgroup=" + status.DescendantEscape.Cgroup
	}
	line += " oom.group=" + string(status.OOMGroup)
	line += " priorities=" + string(status.Priorities)
	cpuWeight := status.CPUWeight
	if cpuWeight == "" {
		cpuWeight = ConfineCPUWeightUnavailable
	}
	line += " cpu-weight=" + string(cpuWeight)
	if status.ScopeMemoryMax <= 0 {
		line += " scope-memory.max=not-requested"
	} else {
		line += " scope-memory.max=enforced=" + strconv.FormatInt(status.ScopeMemoryMax, 10)
		// AIRA-133. Always rendered alongside an ENFORCED cap, on the same
		// discipline as terminated-by: before this facet existed, a cap AIRA
		// estimated and a cap the operator typed were byte-identical here, and
		// omitting it when the source is unknown would reproduce exactly the
		// ambiguity it ends. Not rendered at all for an unenforced scope, where
		// there is no cap to have a source and `not-requested` above says so.
		capSource := status.ScopeMemoryCapSource
		if capSource == "" {
			capSource = ConfineCapSourceUnevaluated
		}
		line += " cap-source=" + capSource
		if status.ScopeMemoryBinding != "" {
			line += " binding=" + status.ScopeMemoryBinding
		}
		if status.ScopeMemoryEffective > 0 {
			line += " effective=" + strconv.FormatInt(status.ScopeMemoryEffective, 10)
		}
		if status.ScopeMemoryHigh > 0 {
			line += " memory.high=reclaim-pressure=" + strconv.FormatInt(status.ScopeMemoryHigh, 10)
		}
	}
	// AIRA-110, always rendered on the same discipline as terminated-by below:
	// this facet exists precisely because a trailer that says nothing about swap
	// is indistinguishable from one whose cap really is the whole footprint
	// bound. Omitting it when the answer is unknown would reproduce the silence
	// it ends, so an unset value reads as unevaluated rather than as bounded.
	swapCap := status.ScopeSwapCap
	if swapCap == "" {
		swapCap = ConfineSwapCapUnevaluated
	}
	line += " scope-swap.max=" + swapCap
	// AIRA-70 / AIRA-91 Part A. Always rendered: before this facet existed, a
	// SIGKILLed job's trailer was byte-identical to a clean run's, so omitting
	// the facet when it is unknown would reproduce exactly the silence it
	// exists to end. An unset field reads as unevaluated, never as "normal".
	terminated := status.TerminatedBy
	if terminated == "" {
		terminated = ConfineTerminatedUnevaluated
	}
	line += " terminated-by=" + terminated
	// AIRA-138, sited directly after terminated-by because the two answer
	// adjacent questions and are allowed to disagree (see ConfineDeadlineState).
	// Rendered ONLY for a bound that was actually requested, so a confine with no
	// deadline produces a byte-identical trailer to before this facet existed —
	// there is no fabrication risk in omitting a field for a bound nobody asked
	// for, which is why this is not on terminated-by's always-rendered discipline.
	line += formatConfineDeadlineFacet(ConfineDeadlineLabelWall, status.TimeoutBudget, status.Timeout)
	line += formatConfineDeadlineFacet(ConfineDeadlineLabelCPU, status.CPUTimeoutBudget, status.CPUTimeout)
	// AIRA-102, on the same absent-unless-relevant discipline as the exclusive
	// facet below: rendered only when a container runtime was actually detected,
	// so every trailer for an ordinary job is unchanged.
	if status.Container != "" {
		line += " container=" + status.Container
	}
	if status.ContainerMemory != "" {
		line += " container-memory=" + status.ContainerMemory
	}
	// AIRA-101, on exactly the same always-rendered discipline as terminated-by
	// above and for the same reason: a contended benchmark whose trailer is
	// byte-identical to an uncontended one is the silence this facet exists to
	// end. Rendered whenever exclusivity was REQUESTED — including when the
	// outcome could not be established, which reads as unevaluated and never as
	// granted.
	//
	// Absent entirely when --exclusive was not asked for, so ordinary confine
	// trailers are unchanged.
	if status.Exclusive != "" {
		line += " exclusive=" + status.Exclusive
		// The acquisition condition travels with the result. Only meaningful for a
		// run that actually got the slice; a lost or unevaluated run has no honest
		// drain figure to report.
		if status.Exclusive == ConfineExclusiveGranted && status.ExclusiveDrainedMS > 0 {
			line += " drained-for=" + (time.Duration(status.ExclusiveDrainedMS) * time.Millisecond).Round(time.Second).String()
		}
	}
	// AIRA-104, placed directly after the exclusive facet so a granted
	// exclusive run's resource numbers sit next to its exclusivity
	// attestation -- the motivating use case (AIRA-101 benchmarking), though
	// these two facets are rendered for every confine job, not only exclusive
	// ones. Always rendered, on exactly the same discipline as terminated-by
	// above: an unestablished counter reads as unevaluated, never as a silent
	// omission or a fabricated zero.
	if status.PeakRSS != nil {
		line += " peak-rss=" + FormatConfineBytes(*status.PeakRSS)
	} else {
		line += " peak-rss=unevaluated"
	}
	if status.CPUUser != nil && status.CPUSys != nil {
		line += " cpu=" + formatConfineCPUUsec(*status.CPUUser) + "+" + formatConfineCPUUsec(*status.CPUSys)
	} else {
		line += " cpu=unevaluated"
	}
	return line
}

// formatConfineDeadlineFacet renders ONE bound's trailer field, or "" when that
// bound was never requested (AIRA-138).
//
// The field NAME is the flag name, so an operator reading `cpu-timeout=10m0s:
// fired-kill-completed` knows which knob produced it. Two keys rather than one
// shared `deadline=` key, so requesting both bounds cannot produce duplicates.
//
// A requested bound whose state was never established renders `unevaluated`
// rather than being dropped: the budget is proof the operator asked, and a
// silent omission there would be indistinguishable from not having asked.
func formatConfineDeadlineFacet(label string, budget time.Duration, state ConfineDeadlineState) string {
	if budget <= 0 {
		return ""
	}
	if state == "" {
		state = ConfineDeadlineState(ConfineTerminatedUnevaluated)
	}
	return " " + label + "=" + budget.String() + ":" + string(state)
}

// formatConfineCPUUsec renders a cpu.stat usec counter (user or system) as a
// duration, exact rather than rounded: the CPU-time benchmarking this exists
// for (AIRA-104) can matter down to sub-second jobs, and rounding to whole
// seconds -- as the unrelated exclusive drained-for figure above does -- would
// silently zero those out.
func formatConfineCPUUsec(usec int64) string {
	if usec < 0 {
		return "unknown"
	}
	// The usec*1000 -> Duration(ns) multiply only overflows int64 above roughly
	// 292,471 years of accumulated CPU time -- not reachable by any real
	// cgroup, so this is treated as unrepresentable rather than guarded against.
	return (time.Duration(usec) * time.Microsecond).String()
}

// delegateRAMScopeIDMarker, the scope-id parser and its helpers live in this
// PORTABLE file, not in confine_linux.go, because they are pure string
// manipulation over a value that crosses the daemon boundary. Keeping them
// Linux-only forced internal/daemon to carry a SECOND, regex-shaped definition
// of the same grammar, and the two accepted different languages — an id the
// daemon admitted could then be invisible to every scan (build-review, Sol).
// One parser, one language.
const delegateRAMScopeIDMarker = "@dr"

// ParseConfineScopeID is the exported form for internal/daemon, which must
// validate an id a client supplied and bind its embedded name and owner to the
// separately-supplied ones. It never returns a partially-decoded id: ok=false
// means every other result is meaningless.
func ParseConfineScopeID(scopeID string) (name string, pid int, stamp int64, owner string, ok bool) {
	return parseConfineScopeID(scopeID)
}

func validConfineScopeID(scopeID string) bool {
	_, _, _, _, ok := parseConfineScopeID(scopeID)
	return ok
}

// parseConfineScopeID decomposes a scope id into name, supervisor pid, launch
// stamp and — since AIRA-52 — the launching owner, which is the empty string for
// an id minted with no owner (an id from before the suffix existed parses
// identically, which is why the encoding is an optional suffix rather than a
// new mandatory field).
func parseConfineScopeID(scopeID string) (string, int, int64, string, bool) {
	if !strings.HasPrefix(scopeID, "CONFINE-") || strings.Contains(scopeID, "/") {
		return "", 0, 0, "", false
	}
	rest := strings.TrimPrefix(scopeID, "CONFINE-")
	if strings.HasPrefix(rest, delegateRAMScopeIDMarker+"-") {
		rest = strings.TrimPrefix(rest, delegateRAMScopeIDMarker+"-")
	}
	// Split at the FIRST remaining '@', keeping the remainder verbatim: an
	// inferred owner starts with its own '@' (ConfineInferredOwnerPrefix) and
	// must survive intact. The "@dr" marker was already stripped above, so this
	// delimiter is unambiguous — neither a name nor an owner may contain '@'.
	owner := ""
	if at := strings.IndexByte(rest, '@'); at >= 0 {
		owner = rest[at+1:]
		rest = rest[:at]
		if ValidateConfineOwner(owner) != nil {
			return "", 0, 0, "", false
		}
	}
	last := strings.LastIndexByte(rest, '-')
	if last <= 0 || last == len(rest)-1 {
		return "", 0, 0, "", false
	}
	// CANONICAL round-trip, not merely parseable: strconv.ParseInt accepts an
	// uppercase base-36 stamp, a sign and leading zeros, none of which
	// confineScopeID can ever mint. Requiring the text to be exactly what
	// FormatInt would produce keeps this the single, canonical grammar, so the
	// daemon's admission check and this scanner cannot disagree about an id
	// (build-review, Sol: they previously did, in both directions).
	stampText := rest[last+1:]
	stamp, err := strconv.ParseInt(stampText, 36, 64)
	if err != nil || stamp <= 0 || strconv.FormatInt(stamp, 36) != stampText {
		return "", 0, 0, "", false
	}
	rest = rest[:last]
	last = strings.LastIndexByte(rest, '-')
	if last <= 0 || last == len(rest)-1 {
		return "", 0, 0, "", false
	}
	name := rest[:last]
	pidText := rest[last+1:]
	pid64, err := strconv.ParseInt(pidText, 10, 32)
	if err != nil || pid64 <= 0 || strconv.FormatInt(pid64, 10) != pidText || ValidateConfineIdentity(name) != nil {
		return "", 0, 0, "", false
	}
	return name, int(pid64), stamp, owner, true
}

// IsDelegateRAMScopeID reports the restart-surviving cap type carrier. The
// marker uses '@', which cannot occur in a user-supplied confine name, so it is
// unambiguous even though names themselves may contain '-'.
func IsDelegateRAMScopeID(scopeID string) bool {
	return strings.HasPrefix(scopeID, "CONFINE-"+delegateRAMScopeIDMarker+"-")
}

// validateConfineName lives in the PORTABLE file, alongside the scope-id parser
// and for the same reason: it is pure string validation over a value that
// crosses process and daemon boundaries, and normalizeConfineIdentity (also
// portable) is its only caller besides confineWithDeps.
func validateConfineName(name string) error {
	if name == "" {
		return nil
	}
	if len(name) > 100 {
		return errors.New("E_CONFINE_ARGUMENT_INVALID: --name is too long")
	}
	for _, r := range name {
		if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || strings.ContainsRune("_.-", r) {
			continue
		}
		return errors.New("E_CONFINE_ARGUMENT_INVALID: --name requires letters, digits, '.', '_', or '-'")
	}
	return nil
}

// normalizeConfineIdentity is the SINGLE place a confine request's name and
// owner acquire their defaults and are validated. It exists because AIRA-22 gave
// the scope id a second minting site (a detached supervisor mints its own,
// before calling Confine), and two copies of "empty name means job, empty owner
// means unknown" held together by a parity test is strictly weaker than one
// copy called from both places: a shared normaliser cannot drift.
//
// verifies: AIRA-22
func normalizeConfineIdentity(request ConfineRequest) (name, owner string, err error) {
	name = request.Name
	if err := validateConfineName(name); err != nil {
		return "", "", err
	}
	if name == "" {
		name = "job"
	}
	owner = request.Owner
	if owner == "" {
		owner = ConfineUnknownOwner
	}
	if err := ValidateConfineOwner(owner); err != nil {
		return "", "", fmt.Errorf("E_CONFINE_ARGUMENT_INVALID: --owner: %w", err)
	}
	return name, owner, nil
}

// MintConfineScopeID produces the scope id a detached supervisor will run under,
// using the SAME normalisation and the SAME minting function confineWithDeps
// uses, so a supervisor can never mint an id its own launch would then refuse.
// The embedded pid is this process's, which is the entire point: the cgroup
// directory name is what `confine --kill <pid>`, `confine --list`'s
// SupervisorPID column, and the orphan reaper's liveness predicate read, and for
// a detached job the process that matters is the supervisor, not the launcher.
//
// covers: AIRA-22
func MintConfineScopeID(request ConfineRequest) (string, error) {
	name, owner, err := normalizeConfineIdentity(request)
	if err != nil {
		return "", err
	}
	return confineScopeID(name, owner, request.DelegateRAM), nil
}

// confineScopeIDWithPID is the ONE place the confine scope-id grammar is minted
// (portable, next to its parser, for the same "one language" reason
// parseConfineScopeID lives here). confineScopeID passes os.Getpid(); callers
// that mint an id NAMING ANOTHER PROCESS — the daemon minting a worker scope on
// behalf of its parent supervisor — pass that process's pid explicitly. The
// embedded pid is load-bearing: the orphan reaper's liveness predicate and the
// S2a escape exemption both read it, so a mis-stamped pid is a correctness bug,
// not a cosmetic one.
func confineScopeIDWithPID(name, owner string, pid int, delegateRAM bool) string {
	if name == "" {
		name = "job"
	}
	id := "CONFINE-"
	if delegateRAM {
		id += delegateRAMScopeIDMarker + "-"
	}
	id += name + "-" + strconv.Itoa(pid) + "-" + strconv.FormatInt(time.Now().UnixNano(), 36)
	// An unknown owner is encoded as the ABSENCE of a suffix, never as
	// "@unknown": a reader must not be able to confuse "nobody claimed this" with
	// a claim, and an id minted before this change parses identically.
	if owner != "" && owner != ConfineUnknownOwner && ValidateConfineOwner(owner) == nil {
		id += "@" + owner
	}
	return id
}

// MintWorkerScopeID mints the first-class confine scope id for one aitest worker
// (S2a §16a). Unlike a job scope, its pid slot is the PARENT SUPERVISOR's pid —
// the pid the daemon copies out of the worker's parent_scope_id — not the
// daemon's own, because the S2a escape exemption is a purely local
// parseConfineScopeID(basename).pid == os.Getpid() check on the monitor process
// (§16.1/§16.2). The name is aitest-w<seq>; seq (a daemon-monotonic counter)
// makes (name, parentPid, stamp) unique by construction, so there is no
// cross-scope counter, no reseed, and no EEXIST path. Owner is empty and the
// delegate-ram marker is not used: a worker carries neither. The result is
// parseable by parseConfineScopeID, so the worker is reaped / listed / killable
// like any confine scope.
func MintWorkerScopeID(seq, parentPid int) string {
	return confineScopeIDWithPID("aitest-w"+strconv.Itoa(seq), "", parentPid, false)
}

// aitestWorkerNamePrefix is the confine NAME prefix every aitest worker scope
// carries (MintWorkerScopeID mints "aitest-w"+seq). Minted here beside the
// minter so the recogniser and the minter cannot drift.
const aitestWorkerNamePrefix = "aitest-w"

// IsAitestWorkerScopeName reports whether a confine scope NAME (the first field
// parseConfineScopeID returns, e.g. "aitest-w3") is an aitest worker scope. S2a
// §16.1: with workers as first-class sibling scopes, every one embeds its PARENT
// supervisor's pid, so `confine --kill <supervisor-pid>` would otherwise match the
// parent AND every worker (E_SELECTOR_AMBIGUOUS); the default `--list`/`--kill`
// pid/name selector filters worker rows out (a worker is reachable only by its
// explicit scope-id, or by killing its parent). The suffix must be a NON-EMPTY run
// of digits (the seq), so a user's ordinary confine job named e.g. "aitest-wrapper"
// is NOT mistaken for a worker.
func IsAitestWorkerScopeName(name string) bool {
	suffix, ok := strings.CutPrefix(name, aitestWorkerNamePrefix)
	if !ok || suffix == "" {
		return false
	}
	for _, r := range suffix {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

// bindConfineScopeID refuses a pre-minted scope id that does not describe THIS
// process running THIS request. Syntax is not enough and never was: the grammar
// accepts any canonical pid, any valid owner, and either delegate class, so a
// merely-parseable id can name a scope after a foreign supervisor. Each facet is
// checked and named separately so a refusal says which one was wrong.
//
// Fail closed, never re-mint: silently minting a different id would put the job
// in a scope directory the durable record does not name, which is precisely the
// "the record and reality disagree" failure AIRA-22 exists to end.
//
// covers: AIRA-22
func bindConfineScopeID(scopeID, name, owner string, delegateRAM bool) error {
	embeddedName, pid, _, embeddedOwner, ok := parseConfineScopeID(scopeID)
	if !ok {
		return fmt.Errorf("scope id %q is malformed", scopeID)
	}
	if pid != os.Getpid() {
		return fmt.Errorf("scope id %q names supervisor pid %d, not this process (%d)", scopeID, pid, os.Getpid())
	}
	if embeddedName != name {
		return fmt.Errorf("scope id %q carries name %q, not %q", scopeID, embeddedName, name)
	}
	// An unknown owner is encoded as the ABSENCE of a suffix (AIRA-52), so the
	// comparison is against that encoding rather than against the literal word.
	wantOwner := owner
	if wantOwner == ConfineUnknownOwner {
		wantOwner = ""
	}
	if embeddedOwner != wantOwner {
		return fmt.Errorf("scope id %q carries owner %q, not %q", scopeID, embeddedOwner, wantOwner)
	}
	if IsDelegateRAMScopeID(scopeID) != delegateRAM {
		return fmt.Errorf("scope id %q is in the wrong delegate-ram class (id says %v, request says %v)",
			scopeID, IsDelegateRAMScopeID(scopeID), delegateRAM)
	}
	return nil
}
