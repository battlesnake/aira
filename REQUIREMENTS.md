# AIRA requirements registry

This seed registry is deliberately small. The seven Phase 0 requirements below
record the process contract; their IDs are stable handles, not a count of
implementation progress, so they stay `designed` even though the store,
coordination primitives, runner, telemetry, gates, and daemon that satisfy them
are now built. Later phases append new requirements rather than restating these.

Ticket IDs are allocated by the live `aira id` allocator; the earlier hand-kept
seed range (and the external `make id` allocator it was to migrate from) no
longer mint IDs for this repo. `covers:` belongs in Go implementation
documentation and `verifies:` belongs in tests; the enforcing traceability graph
checker is implemented and runs as the `traceability` gate.

| ID | Requirement | Status | Implemented-by | Verified-by |
|---|---|---|---|---|
| AR-1 | Development happens in a dedicated feature worktree; the repository root remains on `master`. | designed | `CLAUDE.md` | — |
| AR-2 | AIRA work follows the documented plan, review, implementation, and merge loop. | designed | `docs/dev/agentic-development-loop.md` | — |
| AR-3 | Correctness-critical changes use two-loop adversarial verification and durable counterexamples. | designed | `docs/adversarial-verification.md` | — |
| AR-4 | Review and merge require independent correctness review, final-gate review, and an explicit coverage account. | designed | `docs/review-and-merge-policy.md` | — |
| AR-5 | AIRA reports `pass`, `fail`, or `unevaluated` and refuses ambiguous or integrity-invalid operations. | designed | `docs/superpowers/specs/2026-08-07-aira-design.md` | — |
| AR-6 | Phase 1 is limited to coordination MVP and the blocked-by ready queue; later runner, telemetry, gates, and review emission remain deferred. | designed | `CLAUDE.md` | — |
| AR-7 | The release target is one static Go binary without cgo. | designed | `CLAUDE.md` | — |
