// Package codes is the leaf home of AIRA's stable error-code catalogue.
//
// The catalogue is the exit contract every face publishes: the CLI exit status,
// the daemon and MCP response envelopes, and the generated Skill contract all
// derive from it. It therefore belongs below every layer that needs it, with no
// dependencies of its own, rather than inside internal/store beside the check
// verb that happened to introduce it (AIRA-87).
//
// A code the tree can actually emit must be catalogued, and a catalogued code
// must be emittable. produced_test.go enforces both directions against a static
// scan of the source tree, and every accepted divergence is written down there
// with its reason.
//
// The exit buckets are:
//
//	0  warning only; the operation succeeded (every W_ code)
//	1  the operation genuinely failed
//	2  the request was bad: argument, selector, or a named target that cannot
//	   serve it
//	3  the result is unevaluated — not a pass and not a fail (every U_ code)
//	4  internal or infrastructure failure: the machine, store, or daemon could
//	   not carry the request out
//
// The 1-versus-2 line is the one the catalogue gets asked about most, so it is
// written down rather than left to per-code taste: a refusal is 1 when the
// request is well formed and some durable state refuses it *now* (already
// exists, held, conflicted, not in the state the operation needs — change the
// state and the same request succeeds), and 2 when no change of state would let
// that request through as written (a malformed argument, or a named target that
// can never serve it). E_RELATION_EXISTS, E_LEASE_HELD, E_WRITE_CONFLICT,
// E_PREFIX_OWNERSHIP_CONFLICT, E_TRANSITION_INVALID and E_INTENT_NOT_PENDING are
// the 1 side; E_NOT_FOUND, E_SELECTOR_INVALID and the E_*_ARGUMENT_INVALID
// family are the 2 side. Two entries predated the rule and sat on the other side
// of it — E_ALREADY_INITIALIZED and E_RANT_IDEMPOTENCY_CONFLICT, both catalogued
// at 2 before the rule was written down, and both state conflicts by it.
// AIRA-125 moved both to 1 rather than record an exception at either, because an
// exception is what the next author would cite: two entries pointing away from
// the stated rule is exactly the ambiguity that left AIRA-87's eleven undecided.
// The catalogue now has no 1-versus-2 counter-example in either direction, and
// TestStateConflictCodesExitOne in produced_test.go pins that.
//
// A code's bucket is a published contract, so choosing one is a decision to be
// made deliberately rather than by falling through ExitForCode's default.
// AIRA-107 decided a bucket for the eleven E_ codes AIRA-87 had catalogued at
// that default; each of those entries carries its reasoning inline below, and
// TestRebucketedCodesFollowTheKindConvention pins every one of them. Four of the
// eleven are decided AT 1, which is a decision and not a non-answer: the defect
// AIRA-107 exists to fix is a bucket nobody chose, not the number 1.
//
// One rule constrains that choice from outside the catalogue. A code that only
// ever travels as a CheckFinding — never raised as an error, never assigned to
// Response.Code — cannot pick its bucket freely, because `aira check` derives
// its exit from the report verdict (core.exitCode), not from the finding's code.
// Its catalogued exit must therefore be the exit the verdict actually produces,
// or the generated Skill/agent-guide contract publishes a number no face emits.
// core.TestFindingOnlyCodesExitAsTheirCheckVerdictDoes enforces that.
package codes

var ExitCodes = map[string]int{
	"E_CONFIG_MISSING": 2, "E_CONFIG_INVALID": 2, "E_NOT_PROJECT": 2,
	"E_ID_INVALID": 2, "E_SELECTOR_INVALID": 2, "E_NOT_FOUND": 2,
	"E_SELECTOR_AMBIGUOUS": 2, "E_UNKNOWN_VERB": 2,
	"E_NOT_ADOPTED": 2, "E_NO_PROJECT": 2,
	// AIRA-125 moved this from 2 to 1. Both emitters refuse a well-formed request
	// because durable state already holds the record `init` would create:
	// app.Init finds .aira/config on disk (app/project.go:422) and
	// store.PreflightAdoption finds a projects row for this project
	// (store/lifecycle.go:162). Remove the config, or eject the project, and the
	// identical request succeeds — which is the 1 side of the rule in the package
	// comment, and the same already-exists shape as E_RELATION_EXISTS,
	// E_GATE_EXISTS and E_PREFIX_OWNERSHIP_CONFLICT (all 1). The counter-argument
	// AIRA-125 considered and rejected is that `init` is the bootstrap surface,
	// below any record, so a caller that has not initialised has nothing to
	// observe; that is true of the surface but not of this refusal, which only
	// happens when the record does exist and the caller can observe it. Carving a
	// one-off exception to keep it at 2 would leave the counter-example the ticket
	// exists to remove, so consistency with the rule wins.
	"E_ALREADY_INITIALIZED": 1,
	"E_GLOB_INVALID":        2,
	"E_DAEMON_UNAVAILABLE":  4, "E_DAEMON_BUSY": 4, "E_DAEMON_TIMEOUT": 3,
	"E_DAEMON_PROJECT_INVALID": 2, "E_DAEMON_PROTOCOL": 2, "E_DAEMON_INTERNAL": 4,
	"U_DAEMON_OUTCOME_UNKNOWN": 3,
	"E_DB_BUSY":                4, "E_DB_CORRUPT": 4, "E_RECEIPT_IO": 4,
	"E_RECONCILE_REQUIRED": 4, "E_GIT_SCAN": 4, "E_INTERNAL": 4,
	"E_JOURNAL_CORRUPT": 4,
	"E_SCHEMA_INVALID":  4, "E_EJECT_LIVE_STATE": 1, "E_EJECT_UNVERIFIED": 3, "E_PURGE_DIRTY": 1,
	"E_FINDING_INVALID": 2, "E_WAIVER_REASON_REQUIRED": 2, "E_QUERY_INVALID": 2,
	"E_REQUIREMENT_INVALID": 2,
	"E_COMPUTE_INVALID":     2, "E_COMPUTE_PROVIDER_UNKNOWN": 2, "E_COMPUTE_CONSERVATION": 0,
	"E_IMPORT_INVALID": 2, "E_ARGUMENT_INVALID": 2,
	"E_TESTREPORT_INVALID": 2, "E_TESTREPORT_FLAKY": 1,
	"E_RANT_INVALID": 2, "E_RANT_TOO_LARGE": 2, "E_RANT_REF_INVALID": 2,
	// AIRA-125 moved this from 2 to 1, splitting it off the E_RANT_* bad-request
	// family above. Its neighbours there reject what the caller wrote — a body that
	// fails validation, one over the size bound, a malformed ref — and no state
	// change makes any of those requests succeed as written. This one is different
	// in kind: the request is well formed, and it is refused only because a stored
	// rant already claims that idempotency key with different input
	// (store/rant.go:66). The key is the caller's to choose, so a replay against
	// fresh input succeeds and the stored row is observable state. That is the
	// 1 side of the rule in the package comment, and the same shape as
	// E_WRITE_CONFLICT and E_RUN_TELEMETRY_CONFLICT.
	"E_RANT_IDEMPOTENCY_CONFLICT": 1,
	// AIRA-107 split the redaction pair, which AIRA-87 had left together at the
	// default. E_RANT_REDACTED refuses an operation on a rant whose body is gone:
	// the caller named a target that cannot serve the request, which is the same
	// bad-request shape as the E_RANT_* family above and E_NOT_FOUND, so 2.
	// E_RANT_REDACTION_INCOMPLETE is the opposite kind of answer entirely — the
	// store could not complete the physical erasure (a held WAL keeps the old
	// bytes reachable) — which is a storage-infrastructure failure, not anything
	// the caller wrote, so it joins E_DB_BUSY / E_RECEIPT_IO at 4.
	"E_RANT_REDACTED": 2, "E_RANT_REDACTION_INCOMPLETE": 4,
	// A malformed command-language program is an argument error like every other
	// invalid input above (AIRA-107; it sat at the default only because AIRA-87
	// catalogued it without re-bucketing). The bucket is only honest because
	// AIRA-107 also split the code's second emitter off it: store's
	// nextCommandNumbers used to raise E_COMMAND_INVALID when the persisted
	// command-event counter row held number<1 or seq<1, which has nothing to do
	// with what the caller wrote, so a 2 there would have told the caller to fix
	// a request that was fine. That path is E_COMMAND_COUNTER_CORRUPT below.
	"E_COMMAND_INVALID": 2,
	// The counter-row invariant AddCommandEvent depends on: next_number and
	// next_seq are monotonic and start at 1, so a row below that is persisted
	// state AIRA wrote and can no longer trust — an infrastructure failure, 4.
	// It gets its own code rather than reusing E_DB_CORRUPT, which the phase-1
	// spec designs for "the DB cannot be opened or schema integrity fails" and
	// which is still honestly recorded as unproduced in produced_test.go's
	// cataloguedNotProduced table: wiring that spec'd code to this one narrow
	// invariant would erase that record while leaving its designed producer just
	// as missing, and would tell an operator "the database is corrupt" when what
	// AIRA knows is exactly which row is.
	"E_COMMAND_COUNTER_CORRUPT": 4,
	// E_SKILL_INSTALL is registered at 2 because that is what the skill
	// installer actually returns: cmd/aira/skill.go writes the error and
	// returns 2 directly rather than routing through ExitForCode.
	"E_SKILL_INSTALL":              2,
	"E_INDEX_UNEVALUATED":          3,
	"U_INDEX_UNESTABLISHED":        3,
	"U_CHECK_UNEVALUATED":          3,
	"U_COMPUTE_UNEVALUATED":        3,
	"U_INSIGHT_UNEVALUATED":        3,
	"U_TESTREPORT_INCOMPARABLE":    3,
	"U_TESTREPORT_INCOMPLETE":      3,
	"U_REVIEW_SECTION_UNEVALUATED": 3,
	// Check-report vocabulary. These codes travel in the response payload —
	// CheckFinding.Code and TicketRecord.Warnings — rather than in Response.Code,
	// but they are part of the same published contract every face renders, and
	// U_RELATION_GRAPH_UNESTABLISHED is additionally raised as an error by
	// scan_read.go exactly like its catalogued twin U_INDEX_UNESTABLISHED, so it
	// must exit 3 rather than falling through to the default (AIRA-87).
	// Both divergence codes report that a derived index disagrees with the
	// canonical git-file truth it is projected from. AIRA-107 decided them at 1,
	// and the reason is the finding-only rule in the package comment rather than
	// a family resemblance: neither code is ever raised as an error. Every
	// emission is a CheckFinding{Kind:"fail"} (store/finding.go:624-645,
	// store/relation_ready.go:402/408), and `aira check` takes its exit from the
	// report verdict, not from a finding's code — core.exitCode maps verdict
	// "fail" to 1. Phase-1 spec §8 agrees: exit 1 is "at least one selected check
	// is fail or a fail-closed integrity error exists", while 4 is reserved for
	// "store/reconciliation failed BEFORE the requested checks could be
	// evaluated". A detected divergence is an evaluated failing check, so 4 would
	// publish, through the generated Skill and agent-guide contract, an exit no
	// face can produce. They sit at 1 with their fellow check-report failure
	// codes E_RELATION_TARGET_MISSING and E_DUPLICATE_ID below.
	//
	// The store-integrity family (E_JOURNAL_CORRUPT, E_RECONCILE_REQUIRED,
	// E_DB_CORRUPT) is a different kind of thing despite the similar subject
	// matter: those are raised as errors and abort the request, which is what
	// earns them 4.
	"E_FINDING_INDEX_DIVERGENCE": 1, "E_RELATION_INDEX_DIVERGENCE": 1,
	"U_RELATION_GRAPH_UNESTABLISHED": 3, "U_RELATION_OWNER_UNREADABLE": 3,
	"W_STALE_INDEX": 0, "W_ORPHAN_WORKTREE": 0, "W_WORKTREE_DIVERGENCE": 0,
	"W_AREA_OVERLAP": 0, "W_RELATION_TARGET_MISSING": 0, "W_RELATION_INVALID": 0,
	"W_CROSS_PROJECT_RELATION": 0, "W_RELATION_UNOBSERVABLE": 0,
	// Runner-lite lifecycle and containment codes. These are deliberately kept
	// in the single catalog so every face gets the same exit contract.
	"E_RUN_ARGUMENT_INVALID": 2, "E_RUN_PREFIX_INVALID": 2, "E_RUN_CWD_INVALID": 2,
	"E_RUN_ENV_INVALID": 2, "E_RUN_STDIN_INVALID": 2, "E_RUN_NOT_FOUND": 2,
	"E_RUN_FAILED": 1, "E_RUN_KILLED": 1, "E_RUN_TIMEOUT": 3, "E_RUN_FOREIGN_OWNER": 1,
	"E_RUN_OOM_KILLED": 1, "E_RUN_PTY_UNAVAILABLE": 1,
	"E_RUN_OUTPUT_OPEN": 4, "E_RUN_OUTPUT_DISK_FULL": 4, "E_RUN_CAPTURE_FAILED": 4,
	"E_RUN_SCOPE_UNAVAILABLE": 4, "E_RUN_CAP_UNAVAILABLE": 4, "E_RUN_SCOPE_INVALID": 4, "E_RUN_SCOPE_HANDOFF": 4,
	"E_RUN_SCOPE_MIGRATION": 4, "E_RUN_DESCENDANT_KILLED": 4, "E_RUN_LAUNCH_FAILED": 4,
	"U_RUN_EXIT_UNKNOWN": 3, "U_RUN_OUTPUT_UNAVAILABLE": 3, "U_RUN_RECONCILE_REQUIRED": 3,
	// AIRA-107 decided all three, which had sat at the default.
	//
	// E_RUN_RECONCILE_REQUIRED is the runner analogue of E_RECONCILE_REQUIRED (4)
	// and the error twin of U_RUN_RECONCILE_REQUIRED (3): the run ledger cannot be
	// trusted until it is reconciled, which is an infrastructure state, so 4.
	//
	// E_RUN_TELEMETRY_CONFLICT stays at 1. Both emissions are state conflicts, not
	// malformed requests — "telemetry is already settled" and "run has no pending
	// telemetry envelope" (runner_linux.go:2037/2040) — and the 1-versus-2 rule in
	// the package comment puts a well-formed request that durable state refuses at
	// 1. The catalogue already carries that convention for exactly this shape in
	// the supervisor-lease triple below, which splits CONFLICT: 1 /
	// LEASE_INVALID: 2 / LEASE_FAILED: 4, and again at E_LEASE_HELD,
	// E_WRITE_CONFLICT, E_RELATION_EXISTS and E_PREFIX_OWNERSHIP_CONFLICT (all 1).
	// E_RANT_IDEMPOTENCY_CONFLICT was the lone precedent the other way when this
	// was decided; AIRA-125 moved it to 1, so the alignment is now unanimous.
	//
	// E_RUN_USAGE_READ is a warning-only code and is bucketed on that basis. Its
	// single emitter (core/run_wiring.go:374) sets a wiring warning that lands in
	// runWiring.Compute.Code and runWiring.Warnings; it is never assigned to
	// Response.Code and never reaches ExitForCode, so no face exits with it and
	// the bucket is documentation of KIND for the generated contract rather than a
	// process exit. As a kind it is a bad request: it is one arm of a switch whose
	// every other arm is 2 (E_RUN_ARGUMENT_INVALID for `--usage -`,
	// E_RUN_USAGE_PROVIDER_REQUIRED for a missing --provider, E_COMPUTE_INVALID for
	// an unparsable payload), and its dominant cause is a caller-named --usage path
	// that could not be read, which is the M7 precedent "missing caller file →
	// E_NOT_FOUND, exit 2". A genuine disk fault would reach the same arm and is
	// indistinguishable there; the caller-error reading is the common one, and
	// bucketing it 4 next to E_RUN_OUTPUT_OPEN/E_RUN_CAPTURE_FAILED would be wrong
	// twice over, since those two are AIRA-owned capture files rather than a path
	// the caller typed.
	"E_RUN_RECONCILE_REQUIRED": 4, "E_RUN_TELEMETRY_CONFLICT": 1, "E_RUN_USAGE_READ": 2,
	"U_RUN_REPORT_NOT_REQUESTED": 3, "U_RUN_COMPUTE_NOT_REQUESTED": 3,
	"U_RUN_REPORT_CAPTURE_INCOMPLETE": 3, "U_RUN_TELEMETRY_PENDING": 3,
	"E_RUN_DETACH_FAILED": 4, "E_RUN_IDENTITY_UNAVAILABLE": 4,
	"U_RUN_DETACH_CANCELLED": 3, "U_RUN_QUIESCE_FORCED": 3, "U_RUN_CAPTURE_INCOMPLETE": 3,
	"U_RUN_SUPERVISOR_STALLED": 3, "U_RUN_LAUNCH_STALLED": 3, "U_RUN_EXIT_CONFLICT": 3,
	"E_RUN_SUPERVISOR_LEASE_CONFLICT": 1, "E_RUN_SUPERVISOR_LEASE_INVALID": 2, "E_RUN_SUPERVISOR_LEASE_FAILED": 4,
	"U_RUN_SUPERVISOR_LEASE_UNHEALTHY": 3,
	"E_RUN_INPUT_UNAVAILABLE":          1, "E_RUN_INPUT_NOT_READY": 3, "E_RUN_INPUT_UNREACHABLE": 4,
	"E_RUN_INPUT_CLOSED": 1, "E_RUN_INPUT_BUSY": 1, "E_RUN_INPUT_FOREIGN_OWNER": 1,
	"E_RUN_INPUT_PARTIAL": 1, "E_RUN_INPUT_OUTCOME_UNKNOWN": 3, "E_RUN_INPUT_PROTOCOL": 2,
	"E_RUN_INPUT_PATH_TOO_LONG":  2,
	"E_CONFINE_ARGUMENT_INVALID": 2, "E_CONFINE_UNAVAILABLE": 4,
	// A refused over-ceiling admission wait is a bad request, like any other
	// argument error, so it exits 2 rather than falling through to a default.
	"E_ADMIT_WAIT_TOO_LONG": 2,
	// The other two terminal admission rejections the daemon can answer with
	// (daemon.CodeAdmitTooLarge / CodeAdmitSaturated). AIRA-87 catalogued them at
	// the default without deciding a bucket; AIRA-107 made that decision, and it
	// splits them because they are not the same kind of "no". E_ADMIT_TOO_LARGE
	// says the request can never be satisfied on this slice at any time — the
	// reserve exceeds the ceiling itself — which is a bad request exactly like
	// E_ADMIT_WAIT_TOO_LONG above, so 2. E_ADMIT_SATURATED says the request is
	// well-formed and the machine ran out of capacity for it within the wait: a
	// resource-exhaustion condition about the host, not about the argv, so it
	// joins E_DAEMON_BUSY and E_DB_BUSY at 4. The distinction is the one an agent
	// actually needs from an exit status alone: 2 means fix the request, 4 means
	// retry when the box is free.
	"E_ADMIT_TOO_LARGE": 2, "E_ADMIT_SATURATED": 4,
	// AIRA-101's two exclusive-admission refusals. AIRA-101 catalogued
	// E_ADMIT_EXCLUSIVE_ACTIVE at 1 as "an ordinary refusal — another benchmark
	// holds the slice, retry later". AIRA-124 moved it to 4, because that reading
	// splits ONE machine condition across two exit statuses. The condition is
	// "an exclusive job holds this slice"; it reaches a caller down two paths:
	// a second `--exclusive` request is refused with E_ADMIT_EXCLUSIVE_ACTIVE,
	// while an ordinary job that waits out its admission budget behind the same
	// holder is refused with E_ADMIT_SATURATED, whose message says literally
	// "the slice is held exclusively by another job for benchmarking; retry when
	// it finishes" (runner/admission_linux.go, rejection.Exclusive == "held").
	// E_ADMIT_SATURATED is 4 by AIRA-107 as host capacity exhaustion, and that is
	// what this is too: temporary capacity exhaustion of the whole slice, cured
	// by waiting rather than by editing the request or the project's durable
	// state. An agent branching on the exit status alone — the thing the buckets
	// exist for — must see one number for one condition, and 4 already means
	// "retry when the box is free". The confine face agrees: the runner wraps
	// this refusal as E_CONFINE_UNAVAILABLE, which is 4, so 1 here was also out
	// of line with the exit the CLI actually produces for the same event.
	//
	// U_ADMIT_EXCLUSIVE_UNESTABLISHED exits 3 with the rest of the U_ vocabulary
	// because it is genuinely UNEVALUATED: the daemon could not read the slice to
	// establish emptiness, which is not the same claim as "the slice is busy" and
	// must never be reported as one. AIRA-124 did not touch it.
	"E_ADMIT_EXCLUSIVE_ACTIVE": 4, "U_ADMIT_EXCLUSIVE_UNESTABLISHED": 3,
	"E_CONFINE_OWNER_UNVERIFIED": 1, "E_CONFINE_NOT_FOUND": 2,
	"U_CONFINE_NOT_LAUNCHED": 3, "U_CONFINE_KILL_UNCONFIRMED": 3,
	// AIRA-22's detached confine surface, mirroring the run-detach pair
	// (E_RUN_DETACH_FAILED: 4, U_RUN_DETACH_CANCELLED: 3) so a caller that knows
	// one knows the other. U_CONFINE_OUTCOME_UNKNOWN is the unevaluated verdict
	// `confine --status` reports for a job whose supervisor is gone without
	// having written an outcome; it is never a claim that the job failed.
	"E_CONFINE_DETACH_FAILED": 4, "U_CONFINE_DETACH_CANCELLED": 3,
	"U_CONFINE_OUTCOME_UNKNOWN":  3,
	"E_INSTALL_ARGUMENT_INVALID": 2, "E_INSTALL_UNAVAILABLE": 4,
	"E_INSTALL_OVERCOMMIT":    1,
	"E_RUN_WIRING_INCOMPLETE": 4, "E_RUN_USAGE_PROVIDER_REQUIRED": 2, "E_RUN_CONFIG_ENV_INVALID": 2,
	"U_RUN_REPORT_TOO_LARGE": 3,
	// Bounded git network/auth operations.
	"E_GIT_SSH_UNAVAILABLE": 1, "E_GIT_GH_UNAVAILABLE": 1, "E_GIT_AUTH_FAILED": 1,
	"E_GIT_FALLBACK_BLOCKED": 1, "E_GIT_REMOTE_UNSUPPORTED": 1, "E_GIT_REMOTE_UNRESOLVED": 1,
	"E_GIT_TIMEOUT": 3, "E_GIT_FAILED": 1, "E_GIT_ARG_INVALID": 2,
	// Domain-operation failure codes. These already exited 1 via the default
	// below; registering them here documents them so generated response
	// contracts (e.g. the Skill face) do not present an incomplete vocabulary.
	"E_LEASE_TOKEN": 1, "E_LEASE_HELD": 1, "E_LEASE_EXPIRED": 1, "E_TOKEN_WORKTREE": 1,
	"E_TRANSITION_INVALID": 1,
	"E_RELATION_INVALID":   1, "E_RELATION_EXISTS": 1, "E_CROSS_PROJECT_RELATION": 1,
	"E_RELATION_TARGET_MISSING": 1, "E_RELATION_UNOBSERVABLE": 1,
	"E_WRITE_CONFLICT": 1, "E_PROJECT_MISMATCH": 1,
	"E_ID_UNRESOLVED": 1, "E_DUPLICATE_ID": 1, "E_PREFIX_OWNERSHIP_CONFLICT": 1,
	// E_PATH_INTENT_UNRESOLVED is deliberately absent (AIRA-73): it meant "this
	// intent needs explicit materialise/retire resolution", the vocabulary of
	// the deleted outbox.resolution mechanism. Nothing ever produced it, and
	// after the deletion nothing can, so cataloguing it would advertise a
	// vocabulary item to the Skill face that cannot occur.
	"E_PATH_INTENT_BUSY": 1,
	// The explicit-retire vocabulary that closes AIRA-73's other half. Both
	// refusals sit in the same family as E_WRITE_CONFLICT and E_PATH_INTENT_BUSY
	// above — a durable-state refusal, not a usage error — so both exit 1.
	// U_INTENT_UNEVALUATED is the third answer the honesty rule requires: when
	// the on-disk state cannot be read, the intent's disposition is not
	// established, and reporting that as a conflict (a fake fail) or as a
	// successful retire (a fake pass) is exactly what AIRA refuses to do.
	"E_INTENT_NOT_PENDING": 1, "E_INTENT_REPLAYABLE": 1,
	"U_INTENT_UNEVALUATED": 3,
	"E_CLOCK_UNAVAILABLE":  1,
	"E_TRACE_DANGLING":     1,
	"W_TRACE_UNCOVERED":    0, "W_TRACE_UNVERIFIED": 0,
	"U_TRACE_UNSCANNED": 3, "U_TRACE_EMPTY": 3,
	"E_GATE_INVALID": 2, "E_GATE_KIND_INVALID": 2, "E_GATE_CANARY_INVALID": 2,
	// `gate add` on an id that already exists (gate_write.go:149/169) is decided at
	// 1 by the 1-versus-2 rule in the package comment: the request is well formed
	// and refused only because durable project state already holds that record, and
	// deleting the record makes the identical request succeed. Its nearest sibling
	// is E_RELATION_EXISTS (1) below — the same "this record already exists in the
	// project store" refusal — and E_LEASE_HELD, E_WRITE_CONFLICT and
	// E_PREFIX_OWNERSHIP_CONFLICT are the same family. That is also what the code
	// was designed for: the AIRA-53/54 gate-honesty plan introduced it "at exit 1,
	// matching the existing E_RELATION_EXISTS"
	// (docs/plans/2026-09-03-aira53-54-gate-honesty-plan.md:188), so 1 here is the
	// committed intent rather than a fresh guess — it only ever *looked* undecided
	// because AIRA-87 catalogued it at a default that happened to equal it. The one
	// 2-precedent then available was E_ALREADY_INITIALIZED, and it was deliberately
	// not followed here; AIRA-125 has since moved that code to 1 for the same
	// reason, so the two now agree.
	"E_GATE_EXISTS": 1, "U_GATE_SET_EMPTY": 3,
	"E_GATE_ATTESTATION_INVALID": 2,
	"E_GATE_FAILED":              1, "E_GATE_RATCHET_REGRESSED": 1, "E_GATE_CANARY_DID_NOT_FIRE": 1,
	"E_GATE_COMMAND_FAILED": 1,
	"W_GATE_DISABLED":       0, "W_GATE_PROOF_EXPIRING": 0,
	"U_GATE_NO_RESULT": 3, "U_GATE_UNPROVEN": 3, "U_GATE_PROOF_STALE": 3,
	"U_GATE_PROOF_UNAVAILABLE": 3, "U_GATE_EVIDENCE_UNAVAILABLE": 3,
	"U_GATE_CANARY_UNEVALUATED": 3,
	"U_GATE_COMMAND_TIMEOUT":    3, "U_GATE_OUTPUT_OVERFLOW": 3, "U_GATE_PARSER_INCOMPLETE": 3,
	"U_GATE_COMMAND_RUN_UNEVALUATED": 3, "U_GATE_MUTATION_APPLY_FAILED": 3,
}

// ExitForCode maps a stable code to its documented process exit status. A code
// that is not catalogued falls back to 1, so an unlisted failure still exits
// non-zero rather than being reported as success.
func ExitForCode(code string) int {
	if exit, ok := ExitCodes[code]; ok {
		return exit
	}
	return 1
}
