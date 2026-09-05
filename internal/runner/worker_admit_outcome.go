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

// WorkerAdmitGrantFields are the placement coordinates appended to a granted
// outcome line. They are the same four keys the supervisor has always parsed.
type WorkerAdmitGrantFields struct {
	ScopePath  string
	WorkerID   string
	MemoryMax  int64
	MemoryHigh int64
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
		builder.WriteString(" scope=")
		builder.WriteString(url.QueryEscape(grant.ScopePath))
		builder.WriteString(" worker_id=")
		builder.WriteString(url.QueryEscape(grant.WorkerID))
		builder.WriteString(" memory_max=")
		builder.WriteString(strconv.FormatInt(grant.MemoryMax, 10))
		builder.WriteString(" memory_high=")
		builder.WriteString(strconv.FormatInt(grant.MemoryHigh, 10))
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
	return fields, nil
}
