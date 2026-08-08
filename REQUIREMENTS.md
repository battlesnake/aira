# AIRA requirements registry

This seed registry is deliberately small. IDs are stable handles, not a count of
implementation progress. Phase 0 records the process contract; Phase 1 will
extend the registry as the store and coordination primitives become designed and
built. Until `aira id` exists, these seed IDs are maintained manually. The
planned migration from the an earlier project `make id` allocator to `aira id` is itself
part of the Phase-1 design.

`covers:` belongs in Go implementation documentation and `verifies:` belongs in
tests. The enforcing graph checker is a Phase-3 feature, so Phase 0 does not
pretend that an empty graph is a passing implementation gate.

| ID | Requirement | Status | Implemented-by | Verified-by |
|---|---|---|---|---|
| AR-1 | Development happens in a dedicated feature worktree; the repository root remains on `master`. | designed | `CLAUDE.md` | — |
| AR-2 | AIRA work follows the documented plan, review, implementation, and merge loop. | designed | `docs/dev/agentic-development-loop.md` | — |
| AR-3 | Correctness-critical changes use two-loop adversarial verification and durable counterexamples. | designed | `docs/adversarial-verification.md` | — |
| AR-4 | Review and merge require independent correctness review, final-gate review, and an explicit coverage account. | designed | `docs/review-and-merge-policy.md` | — |
| AR-5 | AIRA reports `pass`, `fail`, or `unevaluated` and refuses ambiguous or integrity-invalid operations. | designed | `docs/superpowers/specs/2026-08-07-aira-design.md` | — |
| AR-6 | Phase 1 is limited to coordination MVP and the blocked-by ready queue; later runner, telemetry, gates, and review emission remain deferred. | designed | `CLAUDE.md` | — |
| AR-7 | The release target is one static Go binary without cgo. | designed | `CLAUDE.md` | — |
