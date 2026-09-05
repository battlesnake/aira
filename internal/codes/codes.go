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
package codes

var ExitCodes = map[string]int{
	"E_CONFIG_MISSING": 2, "E_CONFIG_INVALID": 2, "E_NOT_PROJECT": 2,
	"E_ID_INVALID": 2, "E_SELECTOR_INVALID": 2, "E_NOT_FOUND": 2,
	"E_SELECTOR_AMBIGUOUS": 2, "E_UNKNOWN_VERB": 2,
	"E_NOT_ADOPTED": 2, "E_NO_PROJECT": 2,
	"E_ALREADY_INITIALIZED": 2,
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
	"E_RANT_INVALID": 2, "E_RANT_TOO_LARGE": 2, "E_RANT_IDEMPOTENCY_CONFLICT": 2, "E_RANT_REF_INVALID": 2,
	"E_RANT_REDACTED": 1, "E_RANT_REDACTION_INCOMPLETE": 1,
	"E_COMMAND_INVALID": 1,
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
	"E_RUN_RECONCILE_REQUIRED": 1, "E_RUN_TELEMETRY_CONFLICT": 1, "E_RUN_USAGE_READ": 1,
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
	// (daemon.CodeAdmitTooLarge / CodeAdmitSaturated). They are registered at the
	// exit they already take via the ExitForCode default, so cataloguing them
	// changes no behaviour; whether either deserves its own bucket the way
	// E_ADMIT_WAIT_TOO_LONG does is a contract decision, not this move (AIRA-87).
	"E_ADMIT_TOO_LARGE": 1, "E_ADMIT_SATURATED": 1,
	// AIRA-101's two exclusive-admission refusals. E_ADMIT_EXCLUSIVE_ACTIVE is an
	// ordinary refusal — another benchmark holds the slice, retry later — so it
	// takes 1. U_ADMIT_EXCLUSIVE_UNESTABLISHED exits 3 with the rest of the U_
	// vocabulary because it is genuinely UNEVALUATED: the daemon could not read
	// the slice to establish emptiness, which is not the same claim as "the slice
	// is busy" and must never be reported as one.
	"E_ADMIT_EXCLUSIVE_ACTIVE": 1, "U_ADMIT_EXCLUSIVE_UNESTABLISHED": 3,
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
	"E_PATH_INTENT_BUSY":  1,
	"E_CLOCK_UNAVAILABLE": 1,
	"E_TRACE_DANGLING":    1,
	"W_TRACE_UNCOVERED":   0, "W_TRACE_UNVERIFIED": 0,
	"U_TRACE_UNSCANNED": 3, "U_TRACE_EMPTY": 3,
	"E_GATE_INVALID": 2, "E_GATE_KIND_INVALID": 2, "E_GATE_CANARY_INVALID": 2,
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
