package runner

import (
	"errors"
	"fmt"
	"net/url"
	"sort"
	"strconv"
	"strings"
)

// The worker-admit outcome vocabulary: ONE structured channel between the
// daemon, the `aira worker-admit` CLI relay, and the aitest supervisor
// (internal/pylib/aitest/supervisor.py). It replaces the prose stderr channel
// the supervisor used to re-classify with eleven substring probes whose
// fallthrough default was the maximally-unsafe outcome — "the daemon is gone,
// run the rest of the suite UNCONFINED" (AIRA-42, AIRA-45, AIRA-83(b)).
//
// Why this vocabulary lives in internal/runner and not internal/daemon: the
// daemon already imports the runner (runner.WorkerScopeChildPath,
// runner.KillConfine) and the runner may not import the daemon — the same
// layering constraint that made runner.DaemonProtocolVersion a pinned constant
// rather than a derived one (AIRA-83 item 3). Defining it once here, where
// every Go producer can see it, is the alternative to a second hand-copied
// table, which is the exact class AIRA-83 item 3 exists to prevent.
//
// The Python consumer necessarily holds its own copy of the class and state
// sets; TestWorkerAdmitOutcomeVocabularyMatchesTheSupervisor (internal/pylib)
// holds the two equal in both directions, so drift on either side fails the
// build rather than silently misclassifying at runtime.
const (
	// WorkerAdmitOutcomeMarker is the first whitespace token of the single
	// machine-readable line `aira worker-admit` writes to stdout in EVERY
	// outcome. Consumers compare it with equality — it is a frame marker,
	// never a prefix or substring search over prose.
	WorkerAdmitOutcomeMarker = "aira-worker-admit"
)

// State values: WHAT happened. Diagnostic, plus the granted/non-granted
// switch. The first four are the daemon's own verdicts (they are also the
// wire values of WorkerAdmitResponse.State); the last three are facts only a
// client can establish.
const (
	WorkerAdmitStateGranted         = "granted"
	WorkerAdmitStateDenied          = "denied"
	WorkerAdmitStateTimeout         = "timeout"
	WorkerAdmitStateUnevaluated     = "unevaluated"
	WorkerAdmitStateUnavailable     = "unavailable"
	WorkerAdmitStateArgumentInvalid = "argument-invalid"
	WorkerAdmitStatePlacementFailed = "placement-failed"
)

// Class values: WHAT TO DO about it. This is the load-bearing field — the one
// the supervisor dispatches on — and it replaces the old `reject:`/`fallback:`
// reason-string prefix convention outright. Exactly one class is produced for
// every reachable outcome; there is no default.
const (
	// WorkerAdmitClassGranted accompanies State == granted and nothing else.
	WorkerAdmitClassGranted = "granted"
	// WorkerAdmitClassContended is transient: the daemon is reachable and
	// answered (or would have), there is simply no room / no answer right
	// now. Retriable; containment is preserved.
	WorkerAdmitClassContended = "contended"
	// WorkerAdmitClassRequestInvalid is the TERMINAL-BUT-DAEMON-HEALTHY
	// disposition: waiting cannot change this answer, so the affected queued
	// work is failed loudly and reported unevaluated, while the daemon stays
	// available for everything else and containment is NOT stripped.
	//
	// Like WorkerAdmitClassAdmissionUnusable below, it names the DISPOSITION,
	// not a diagnosis. Most members really are static facts about the request
	// (exceeds-ceiling, the CLI's own argument rejections), but AIRA-39 added
	// two that are not: worker-scope-create-failed and
	// worker-id-space-exhausted are daemon-side infrastructure facts about the
	// outer scope. They land here because the alternatives are worse and were
	// weighed in AIRA-39's own review — `contended` would retry a broken
	// cgroupfs INDEFINITELY and stall every aitest run on the machine, and
	// `admission-unusable` would strip containment for a run whose daemon is
	// answering perfectly well. Terminal-and-loud is the honest middle.
	WorkerAdmitClassRequestInvalid = "request-invalid"
	// WorkerAdmitClassAdmissionUnusable means daemon-backed admission is not
	// usable for THIS RUN. Together with WorkerAdmitClassPlacementFailed
	// below it is one of exactly two containment-stripping classes: the
	// supervisor abandons daemon-backed admission and runs the rest of the
	// suite unconfined. Produced only from positive evidence of its own
	// named condition — the exchange demonstrably did not happen (dial or
	// send failure), the client and daemon demonstrably speak different
	// protocol versions, or the scope being asked about is demonstrably not
	// a real daemon-admitted scope (outer-scope-unbounded) — never as a
	// default for something unrecognised.
	//
	// Deliberately NOT named "daemon-unusable": the outer-scope-unbounded
	// row reaches it with a perfectly healthy daemon (design spec
	// 2026-09-01-aitest-design.md §3.7 classifies that case as
	// fallback-triggering on purpose, because the workers stay
	// hierarchically bounded by the real outer confine job either way and
	// falling back beats hanging forever against a scope that will never
	// become capped). The class names the disposition, not a diagnosis of
	// the daemon's health.
	WorkerAdmitClassAdmissionUnusable = "admission-unusable"
	// WorkerAdmitClassPlacementFailed means the daemon granted and the LOCAL
	// cgroup placement mechanism then failed. The second containment-
	// stripping class. Distinct from every daemon verdict above so the
	// diagnostic never blames a healthy daemon.
	//
	// AIRA-39 (Fix 1, merged into this fix mid-review) removed this class's
	// only RELAY producer: `aira worker-admit` no longer creates the worker
	// scope, so a creation failure is now the daemon's own
	// worker-scope-create-failed verdict above, not a local placement failure
	// the CLI discovers after a grant. The pair is deliberately KEPT rather
	// than deleted: supervisor.py still raises WorkerPlacementFailed from its
	// own fork/placement-ack path, and that disposition should have ONE name
	// across both sides of this channel rather than a channel name and a
	// separate Python-only name — which is the two-vocabularies shape this fix
	// exists to remove. What is true today is that only the supervisor
	// produces it, and only locally.
	WorkerAdmitClassPlacementFailed = "placement-failed"
	// WorkerAdmitClassContractViolation means the two sides of this channel
	// disagree about the channel itself: an outcome value outside these
	// catalogues, a state/class pair that contradicts itself, a line that
	// does not parse, an unintelligible daemon frame, or a daemon error code
	// this client does not know. It is terminal and loud, NEVER a silent
	// fallback to unconfined — the whole point of AIRA-42 is that an
	// unrecognised shape must not strip containment.
	WorkerAdmitClassContractViolation = "contract-violation"
)

// Reason tokens: stable, exact-match identifiers. Nothing branches on their
// spelling as a substring; they exist so a human (and a test) can name the
// exact condition. Free-text elaboration belongs in Detail, which nothing
// parses.
const (
	// Daemon-side. Re-derived against AIRA-39's rewrite of evaluateWorkerAdmit
	// (Fix 1, which landed while this fix was in review), not against the
	// shape this fix was originally designed on: AIRA-39 deleted the
	// outer-scope-owner binding entirely (the ledger now sums the outer
	// scope's real children, so two jobs sharing one outer scope are counted
	// together rather than the second refused), and added the four
	// scope-creation and slot conditions below, which the daemon did not have
	// a way to express before it created worker scopes itself.
	WorkerAdmitReasonOuterScopeUnreadable      = "outer-scope-unreadable"
	WorkerAdmitReasonOuterScopeUnbounded       = "outer-scope-unbounded"
	WorkerAdmitReasonExceedsCeiling            = "exceeds-ceiling"
	WorkerAdmitReasonInsufficientHeadroom      = "insufficient-headroom"
	WorkerAdmitReasonSupervisorScopeUnreadable = "supervisor-scope-unreadable"
	WorkerAdmitReasonWorkerScopesUnreadable    = "worker-scopes-unreadable"
	WorkerAdmitReasonAggregateCapExceeded      = "aggregate-cap-exceeded"
	WorkerAdmitReasonWorkerIDSpaceExhausted    = "worker-id-space-exhausted"
	WorkerAdmitReasonWorkerScopeIDCollision    = "worker-scope-id-collision"
	WorkerAdmitReasonAdmitSlotsSaturated       = "admit-slots-saturated"
	WorkerAdmitReasonSaturated                 = "saturated"
	// AIRA-123. The ledger-only (ci-shim) admission path's own conditions.
	//
	// AIRA-121 answered every shim-mode worker-admit with
	// `ci-shim-no-sub-scope` / admission-unusable, because worker-admit then
	// did exactly one thing: nest a kernel-enforced cgroup sub-scope. AIRA-123
	// separates the two things that were fused there — cgroups give
	// ENFORCEMENT, a ledger gives ADMISSION — so the reason token is gone with
	// the refusal it carried, and these three name what the degraded backend
	// can actually decide.
	//
	// ledger-budget-unreadable is class=CONTENDED (retriable): the shim ledger's
	// live reading can fail transiently (an unreadable /proc/meminfo, a
	// momentarily unreadable container cgroup), and the recorded budget itself
	// cannot be absent — a shim daemon refuses to start without a positive one.
	// So waiting really can change this answer, exactly like the real path's
	// outer-scope-unreadable.
	//
	// ledger-budget-exceeded is class=CONTENDED for the same reason
	// aggregate-cap-exceeded is: another worker retiring releases its
	// reservation and this request then fits.
	//
	// confine-mode-mismatch is class=ADMISSION-UNUSABLE and TERMINAL. It fires
	// when the client and the daemon disagree about which mode this box is in
	// (a real-mode client's absolute outer-scope path reaching a shim daemon, or
	// the shim sentinel reaching a real-mode daemon). Waiting cannot reconcile
	// two processes that resolved different install-mode records, and the honest
	// answer is that daemon-backed admission is not usable for this run — the
	// bare-fork fallback with one loud warning — rather than a poll loop against
	// a daemon that will never agree.
	WorkerAdmitReasonLedgerBudgetUnreadable = "ledger-budget-unreadable"
	WorkerAdmitReasonLedgerBudgetExceeded   = "ledger-budget-exceeded"
	WorkerAdmitReasonConfineModeMismatch    = "confine-mode-mismatch"
	// AIRA-64. cpu-slots-saturated is the machine-wide CPU-concurrency bound
	// declining one more worker; admit-locks-busy is a SPECULATIVE request
	// (max_wait_ms == 0) refusing to wait on a lock another job holds. Both are
	// class=contended: retriable, containment preserved, never a verdict about
	// the request or the daemon.
	WorkerAdmitReasonCPUSlotsSaturated = "cpu-slots-saturated"
	WorkerAdmitReasonAdmitLocksBusy    = "admit-locks-busy"

	// AIRA-101. Another job holds this slice EXCLUSIVELY (`aira confine
	// --exclusive`, for uncontended benchmarking), so no worker may be placed
	// under any outer scope but the holder's own.
	//
	// class=contended, deliberately and load-bearingly: the hold ends when the
	// benchmark ends, so this is retriable and the supervisor should keep polling.
	// Classing it terminal would strip containment for the whole suite — the
	// AIRA-63 regression shape. It is its OWN reason token rather than reusing a
	// saturation one because an operator waiting behind a benchmark needs to know
	// a benchmark is running, not think the machine is merely full.
	WorkerAdmitReasonSliceExclusive = "slice-exclusive"

	// Client-side (transport and response classification).
	WorkerAdmitReasonDialFailed              = "dial-failed"
	WorkerAdmitReasonRequestSendFailed       = "request-send-failed"
	WorkerAdmitReasonResponseTimeout         = "response-timeout"
	WorkerAdmitReasonResponseInterrupted     = "response-interrupted"
	WorkerAdmitReasonResponseFailed          = "response-failed"
	WorkerAdmitReasonMalformedResponse       = "malformed-response"
	WorkerAdmitReasonProtocolVersionMismatch = "protocol-version-mismatch"
	WorkerAdmitReasonRequestRejected         = "request-rejected"
	WorkerAdmitReasonDaemonError             = "daemon-error"
	WorkerAdmitReasonUnknownDaemonOutcome    = "unknown-daemon-outcome"
	WorkerAdmitReasonMalformedGrant          = "malformed-grant"
	WorkerAdmitReasonDialResourceExhausted   = "dial-resource-exhausted"

	// Daemon-side, and CLI-side before AIRA-39. AIRA-39 moved worker-scope
	// creation out of `aira worker-admit` and into the daemon's own critical
	// section, so this reason moved with it: it is now a daemon verdict, not a
	// local placement failure the relay discovers after a grant.
	WorkerAdmitReasonWorkerScopeCreateFailed = "worker-scope-create-failed"

	// CLI-side.
	WorkerAdmitReasonArgumentsInvalid         = "arguments-invalid"
	WorkerAdmitReasonEstimatedBytesOutOfRange = "estimated-bytes-out-of-range"
	WorkerAdmitReasonMaxWaitInvalid           = "max-wait-invalid"
	WorkerAdmitReasonDaemonPathsUnavailable   = "daemon-paths-unavailable"
)

var workerAdmitStateSet = map[string]struct{}{
	WorkerAdmitStateGranted:         {},
	WorkerAdmitStateDenied:          {},
	WorkerAdmitStateTimeout:         {},
	WorkerAdmitStateUnevaluated:     {},
	WorkerAdmitStateUnavailable:     {},
	WorkerAdmitStateArgumentInvalid: {},
	WorkerAdmitStatePlacementFailed: {},
}

var workerAdmitClassSet = map[string]struct{}{
	WorkerAdmitClassGranted:           {},
	WorkerAdmitClassContended:         {},
	WorkerAdmitClassRequestInvalid:    {},
	WorkerAdmitClassAdmissionUnusable: {},
	WorkerAdmitClassPlacementFailed:   {},
	WorkerAdmitClassContractViolation: {},
}

// WorkerAdmitStates and WorkerAdmitClasses return the catalogues, sorted, for
// the cross-language equality test and for generated documentation.
func WorkerAdmitStates() []string { return sortedKeys(workerAdmitStateSet) }

// WorkerAdmitClasses returns the class catalogue, sorted.
func WorkerAdmitClasses() []string { return sortedKeys(workerAdmitClassSet) }

func sortedKeys(set map[string]struct{}) []string {
	out := make([]string, 0, len(set))
	for key := range set {
		out = append(out, key)
	}
	sort.Strings(out)
	return out
}

// IsWorkerAdmitState and IsWorkerAdmitClass report catalogue membership. A
// value that fails either check is a contract violation, never a value to
// default.
func IsWorkerAdmitState(value string) bool {
	_, ok := workerAdmitStateSet[value]
	return ok
}

// IsWorkerAdmitClass reports whether value is a catalogued class.
func IsWorkerAdmitClass(value string) bool {
	_, ok := workerAdmitClassSet[value]
	return ok
}

// WorkerAdmitOutcome is the classified result of one worker-admit attempt.
// Every producer in this repository returns one of these rather than an
// unclassified error, so "unclassified" is not a representable state: the type
// is what enforces AIRA-42's requirement that the unsafe outcome
// (daemon-unusable) be explicitly produced and never fallen through to.
type WorkerAdmitOutcome struct {
	State  string
	Class  string
	Reason string
	// Detail is free text for humans. NOTHING parses it. It is
	// query-escaped on the wire so it can never break the line's
	// tokenisation nor be mistaken for a field of its own.
	Detail string
	// Lease is non-nil exactly when State == WorkerAdmitStateGranted.
	Lease *WorkerAdmitLease
}

// Granted reports whether this outcome carries a usable grant.
func (o WorkerAdmitOutcome) Granted() bool { return o.State == WorkerAdmitStateGranted }

// The two grades of worker-admit grant, carried by
// WorkerAdmitGrantFields.Containment on EVERY granted line (AIRA-123).
//
// This token is REQUIRED, and its being required is the whole honesty
// requirement of AIRA-123 in one field: an admission-only grant must never be
// readable as the real thing, and an ABSENT token must never be readable as
// either. Both sides refuse a granted line without it, so the failure mode is a
// loud contract violation rather than a suite that believes it is contained.
//
// The values are deliberately the SAME strings `aira confine` already prints for
// a whole job (ConfineContainmentEnforced / ConfineContainmentAdvisory), so an
// operator reading a worker line and a job trailer is reading one vocabulary
// rather than two that happen to mean the same thing.
const (
	// WorkerAdmitContainmentEnforced: the daemon created a real cgroup sub-scope
	// with a verified memory.max, and the kernel kills a worker that exceeds it.
	// A grant carrying this MUST carry a scope path and a positive memory_max.
	WorkerAdmitContainmentEnforced = string(ConfineContainmentEnforced)
	// WorkerAdmitContainmentAdvisory: ledger-only admission (ci-shim). The
	// daemon booked this worker's DECLARED need against the container's RAM
	// budget and nothing else — there is no cgroup, no memory.max, and no kill
	// backstop, so a worker that exceeds its declared reservation is not killed
	// and the container's own OOM killer is the only thing left. A grant
	// carrying this MUST carry NO scope path and NO memory_max, because there is
	// no cgroup to name and no cap to report; it carries `reserved` instead,
	// which is a booking, not a bound.
	WorkerAdmitContainmentAdvisory = string(ConfineContainmentAdvisory)
)

var workerAdmitContainmentSet = map[string]struct{}{
	WorkerAdmitContainmentEnforced: {},
	WorkerAdmitContainmentAdvisory: {},
}

// WorkerAdmitContainments returns the containment catalogue, sorted, for the
// cross-language equality test and for generated documentation.
func WorkerAdmitContainments() []string { return sortedKeys(workerAdmitContainmentSet) }

// There is deliberately no IsWorkerAdmitContainment predicate to pair with
// IsWorkerAdmitState/Class/SwapCap above. Catalogue membership is not a
// standalone question for this field: an unrecognised grade and a recognised
// grade carrying the wrong coordinates are the same defect, and both are decided
// in ONE place (workerAdmitGrantShapeProblem). A second entry point would be a
// way to check half of it.

// WorkerAdmitGrantFields are the coordinates appended to a granted outcome
// line. `containment` and `worker_id` are required on EVERY grant; which of the
// remaining keys are required, permitted and forbidden is decided by
// containment alone (see WorkerAdmitOutcomeLine).
//
// memory_high was one of the required keys until AIRA-35, which stopped writing
// memory.high on worker scopes entirely (see CreateWorkerScope for the measured
// reasons). Keeping a wire field named after a cgroup control that is
// deliberately not written would be a lie in the protocol, so it was removed
// rather than zeroed. AIRA-123 applies the same rule to the whole advisory
// grant: scope and memory_max are ABSENT there rather than zeroed, because a
// zero would be a value and there is no cgroup to have one.
type WorkerAdmitGrantFields struct {
	ScopePath string
	WorkerID  string
	MemoryMax int64
	// Containment is WorkerAdmitContainmentEnforced or
	// WorkerAdmitContainmentAdvisory. Required; empty is refused.
	Containment string
	// Reserved is the advisory grant's booked reservation in bytes: what the
	// daemon's ledger charged for this worker. Positive on an advisory grant and
	// zero on an enforced one, where memory_max is both the booking AND the
	// enforced bound and a second number would only invite them to disagree.
	Reserved int64
	// SwapCap (AIRA-35) is one of the WorkerAdmitSwapCap* values, omitted from
	// the line when empty. It answers the one question memory_max alone cannot:
	// is this worker's memory.max actually its whole footprint bound, or can
	// the worker escape it into swap? cgroup-v2's memory.max bounds memory, not
	// memory+swap, and before AIRA-35 nothing capped worker swap at all -- a
	// 512 MiB allocation inside a 32 MiB cap was measured exiting 0 with half a
	// gigabyte paged out, never killed. Diagnostic only, exactly like CPUSlots:
	// nothing branches on it, and an absent token means "an older daemon", not
	// "ok".
	SwapCap string
	// CPUSlots (AIRA-64) is WorkerAdmitCPUSlotsOK or
	// WorkerAdmitCPUSlotsUnevaluated, and is omitted from the line when empty.
	// It answers one question the four fields above cannot: was this grant
	// actually subject to the CPU-concurrency bound, or did that dimension
	// fail open? A governance dimension whose fail-open is invisible to the
	// run it affects is how a subsystem ships inert. Diagnostic only: nothing
	// branches on it, and an absent token means "an older daemon", not "ok".
	CPUSlots string
}

// The CPU-governance states carried by WorkerAdmitGrantFields.CPUSlots.
const (
	WorkerAdmitCPUSlotsOK          = "ok"
	WorkerAdmitCPUSlotsUnevaluated = "unevaluated"
)

// The swap-containment states carried by WorkerAdmitGrantFields.SwapCap
// (AIRA-35). Each is a POSITIVE claim about what was established; see
// writeWorkerScopeSwapCap for how each one is proved.
const (
	// WorkerAdmitSwapCapEnforced: memory.swap.max=0 was written and verified,
	// so memory.max is this worker's whole footprint bound.
	WorkerAdmitSwapCapEnforced = "enforced"
	// WorkerAdmitSwapCapNotApplicable: this kernel has no swap support at all
	// (proved: no memory.swap.max AND no /proc/swaps, inside a mounted /proc),
	// so memory.max already bounds everything. Not a warning.
	WorkerAdmitSwapCapNotApplicable = "not-applicable"
	// WorkerAdmitSwapCapUnavailable: swap could not be bounded, so a worker may
	// exceed its memory.max without being killed. The grant still proceeds --
	// refusing would stall every aitest run on such a host -- but the
	// supervisor says so once on the suite's own output rather than letting a
	// lost guarantee pass silently.
	WorkerAdmitSwapCapUnavailable = "unavailable"
)

// IsWorkerAdmitSwapCap reports whether value is a catalogued swap-cap state.
func IsWorkerAdmitSwapCap(value string) bool {
	switch value {
	case WorkerAdmitSwapCapEnforced, WorkerAdmitSwapCapNotApplicable, WorkerAdmitSwapCapUnavailable:
		return true
	default:
		return false
	}
}

// WorkerAdmitOutcomeLine renders the single machine-readable line. grant must
// be non-nil exactly when the outcome is granted; the function does not
// silently paper over a mismatch, because a granted line without placement
// coordinates (or a declined line carrying them) is precisely the sort of
// half-formed message this channel exists to make impossible.
func WorkerAdmitOutcomeLine(outcome WorkerAdmitOutcome, grant *WorkerAdmitGrantFields) (string, error) {
	if !IsWorkerAdmitState(outcome.State) {
		return "", fmt.Errorf("worker-admit outcome state %q is not catalogued", outcome.State)
	}
	if !IsWorkerAdmitClass(outcome.Class) {
		return "", fmt.Errorf("worker-admit outcome class %q is not catalogued", outcome.Class)
	}
	if (outcome.State == WorkerAdmitStateGranted) != (outcome.Class == WorkerAdmitClassGranted) {
		return "", fmt.Errorf("worker-admit outcome state %q and class %q disagree about grantedness", outcome.State, outcome.Class)
	}
	if outcome.Granted() != (grant != nil) {
		return "", errors.New("worker-admit granted outcomes carry placement fields and declined outcomes do not")
	}
	// AIRA-35: an uncatalogued swap_cap is refused here for the same reason an
	// uncatalogued state or class is, one line up. This token is the only signal
	// that a run has lost its per-worker containment guarantee, so a value no
	// consumer recognises would be read as "some daemon said something", i.e.
	// silence — which is exactly what an absent token legitimately means. Empty
	// stays legal: that is the honest "this daemon predates the field".
	if grant != nil && grant.SwapCap != "" && !IsWorkerAdmitSwapCap(grant.SwapCap) {
		return "", fmt.Errorf("worker-admit swap_cap %q is not catalogued", grant.SwapCap)
	}
	if grant != nil {
		if problem := workerAdmitGrantShapeProblem(*grant); problem != "" {
			return "", errors.New(problem)
		}
	}
	var builder strings.Builder
	builder.WriteString(WorkerAdmitOutcomeMarker)
	builder.WriteString(" state=")
	builder.WriteString(outcome.State)
	builder.WriteString(" class=")
	builder.WriteString(outcome.Class)
	if outcome.Reason != "" {
		builder.WriteString(" reason=")
		builder.WriteString(url.QueryEscape(outcome.Reason))
	}
	if grant != nil {
		builder.WriteString(" containment=")
		builder.WriteString(url.QueryEscape(grant.Containment))
		builder.WriteString(" worker_id=")
		builder.WriteString(url.QueryEscape(grant.WorkerID))
		// AIRA-123. scope/memory_max are emitted ONLY for an enforced grant and
		// `reserved` ONLY for an advisory one. An advisory line that carried
		// `scope=` or `memory_max=0` would hand every existing reader — the
		// supervisor's grant parser, an operator's eye, a log grep — a field
		// whose whole meaning is "there is a cgroup here with this cap".
		if grant.Containment == WorkerAdmitContainmentAdvisory {
			builder.WriteString(" reserved=")
			builder.WriteString(strconv.FormatInt(grant.Reserved, 10))
		} else {
			builder.WriteString(" scope=")
			builder.WriteString(url.QueryEscape(grant.ScopePath))
			builder.WriteString(" memory_max=")
			builder.WriteString(strconv.FormatInt(grant.MemoryMax, 10))
		}
		if grant.SwapCap != "" {
			builder.WriteString(" swap_cap=")
			builder.WriteString(url.QueryEscape(grant.SwapCap))
		}
		if grant.CPUSlots != "" {
			builder.WriteString(" cpu_slots=")
			builder.WriteString(url.QueryEscape(grant.CPUSlots))
		}
	}
	if outcome.Detail != "" {
		builder.WriteString(" detail=")
		builder.WriteString(url.QueryEscape(outcome.Detail))
	}
	return builder.String(), nil
}

// workerAdmitGrantShapeProblem states why a grant's coordinates are unusable,
// or "" if they are fine. It is the ONE place the shape rules live: the
// renderer refuses to produce a line that breaks them and the client refuses to
// accept a payload that breaks them, so producer and consumer cannot drift into
// disagreeing about what a grant is.
//
// The rules are stated as what each containment grade MUST and MUST NOT carry,
// in both directions. The MUST NOT half is the load-bearing one (AIRA-123): a
// grant that carried both a scope path and containment=advisory would be
// readable as either, and every consumer that reads scope first — which is all
// of them, since that is what the field was for — would read it as enforced.
func workerAdmitGrantShapeProblem(grant WorkerAdmitGrantFields) string {
	if grant.WorkerID == "" {
		return "granted outcome carries no worker_id"
	}
	switch grant.Containment {
	case "":
		return "granted outcome carries no containment token; an absent grade must never be read as enforced"
	case WorkerAdmitContainmentEnforced:
		switch {
		case grant.ScopePath == "":
			return "granted outcome claims containment=" + WorkerAdmitContainmentEnforced + " but carries no scope_path"
		case grant.MemoryMax <= 0:
			return "granted outcome memory_max must be positive, got " + strconv.FormatInt(grant.MemoryMax, 10)
		case grant.Reserved != 0:
			return "granted outcome claims containment=" + WorkerAdmitContainmentEnforced +
				" and also carries an advisory reservation; memory_max is both the booking and the bound there"
		}
		return ""
	case WorkerAdmitContainmentAdvisory:
		switch {
		case grant.ScopePath != "":
			return "granted outcome claims containment=" + WorkerAdmitContainmentAdvisory +
				" and also names a cgroup scope " + grant.ScopePath + "; there is no cgroup in ledger-only admission"
		case grant.MemoryMax != 0:
			return "granted outcome claims containment=" + WorkerAdmitContainmentAdvisory +
				" and also reports a memory_max; nothing enforces one in ledger-only admission"
		case grant.Reserved <= 0:
			return "granted outcome reserved must be positive, got " + strconv.FormatInt(grant.Reserved, 10)
		}
		return ""
	default:
		return "granted outcome containment " + strconv.Quote(grant.Containment) + " is not catalogued"
	}
}

// ParseWorkerAdmitOutcomeLine is the Go mirror of the supervisor's parser,
// used by the Go tests that pin this channel end to end. It performs the same
// checks in the same order, so a divergence between the two implementations
// shows up as a test failure rather than as a runtime misclassification.
func ParseWorkerAdmitOutcomeLine(line string) (map[string]string, error) {
	tokens := strings.Fields(strings.TrimSpace(line))
	if len(tokens) == 0 || tokens[0] != WorkerAdmitOutcomeMarker {
		return nil, errors.New("line does not carry the worker-admit outcome marker")
	}
	fields := make(map[string]string, len(tokens))
	for _, token := range tokens[1:] {
		key, rawValue, found := strings.Cut(token, "=")
		if !found || key == "" {
			return nil, fmt.Errorf("worker-admit outcome token %q is not key=value", token)
		}
		value, err := url.QueryUnescape(rawValue)
		if err != nil {
			return nil, fmt.Errorf("worker-admit outcome field %q is not decodable: %w", key, err)
		}
		fields[key] = value
	}
	if !IsWorkerAdmitState(fields["state"]) {
		return nil, fmt.Errorf("worker-admit outcome state %q is not catalogued", fields["state"])
	}
	if !IsWorkerAdmitClass(fields["class"]) {
		return nil, fmt.Errorf("worker-admit outcome class %q is not catalogued", fields["class"])
	}
	if (fields["state"] == WorkerAdmitStateGranted) != (fields["class"] == WorkerAdmitClassGranted) {
		return nil, errors.New("worker-admit outcome state and class disagree about grantedness")
	}
	if fields["state"] == WorkerAdmitStateGranted {
		// AIRA-123. A granted line is checked against the SAME shape rules the
		// renderer enforces, from the raw tokens: absence of a key is a real,
		// distinguishable state here (unlike in the struct, where an absent
		// scope and an empty one look alike), which is exactly what "advisory
		// grants carry no scope" needs in order to be checkable at all.
		var parsed WorkerAdmitGrantFields
		parsed.WorkerID = fields["worker_id"]
		parsed.Containment = fields["containment"]
		parsed.ScopePath = fields["scope"]
		for key, target := range map[string]*int64{"memory_max": &parsed.MemoryMax, "reserved": &parsed.Reserved} {
			raw, present := fields[key]
			if !present {
				continue
			}
			value, err := strconv.ParseInt(raw, 10, 64)
			if err != nil {
				return nil, fmt.Errorf("worker-admit outcome %s %q is not an integer", key, raw)
			}
			*target = value
		}
		if problem := workerAdmitGrantShapeProblem(parsed); problem != "" {
			return nil, errors.New(problem)
		}
	}
	return fields, nil
}

// The aitest ADMISSION BACKEND grades, reported by the `aitest-bootstrap` verb
// on its own stdout line and recorded by supervisor.py for the whole run
// (AIRA-123).
//
// This is a different channel from the per-grant containment token above and
// answers a different question — "which backend is this run using at all",
// asked once at startup, versus "what did THIS grant actually establish", asked
// per worker — but the two must never disagree, and the supervisor checks that
// they do not: a ledger-only run that received an `enforced` grant, or a
// sub-scope run that received an `advisory` one, is a contract violation rather
// than something to reconcile.
const (
	// AitestAdmissionSubScope: the real path. Each worker gets its own
	// kernel-enforced cgroup sub-scope nested under the job's outer scope.
	AitestAdmissionSubScope = "cgroup-sub-scope"
	// AitestAdmissionLedgerOnly: ci-shim. Workers are admitted against the
	// container's RAM budget by an in-daemon ledger, with no cgroup, no
	// memory.max and no kill backstop.
	AitestAdmissionLedgerOnly = "ledger-only"
)

// AitestAdmissionGrades returns the catalogue, sorted, for the cross-language
// equality test.
func AitestAdmissionGrades() []string {
	return sortedKeys(map[string]struct{}{
		AitestAdmissionSubScope: {}, AitestAdmissionLedgerOnly: {},
	})
}

// AitestAdmissionForContainment maps a per-grant containment grade to the
// backend grade that must have produced it. It is what lets the supervisor hold
// the two channels consistent from ONE definition rather than a second table.
func AitestAdmissionForContainment(containment string) (string, bool) {
	switch containment {
	case WorkerAdmitContainmentEnforced:
		return AitestAdmissionSubScope, true
	case WorkerAdmitContainmentAdvisory:
		return AitestAdmissionLedgerOnly, true
	default:
		return "", false
	}
}
