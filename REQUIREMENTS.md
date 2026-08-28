# AIRA requirements registry

This registry records the process contract AIRA holds itself to. IDs are stable
handles, not a measure of implementation progress. The seven seed requirements
below are the Phase 0 process contract, and they remain the operating rules now
that the coordination MVP and the later runner, telemetry, gate, daemon, and
review surfaces are built.

Ticket IDs are allocated by the live `aira id` allocator; the earlier hand-kept
seed range (and the external `make id` allocator it was to migrate from) are no
longer used to mint IDs for this repo. `covers:` belongs in Go implementation
documentation and `verifies:` belongs in tests; the enforcing traceability graph
checker is implemented and runs as the `traceability` gate rather than being a
deferred promise.

| ID | Requirement | Status | Implemented-by | Verified-by |
|---|---|---|---|---|
| AR-1 | Development happens in a dedicated feature worktree; the repository root remains on `master`. | active | `CLAUDE.md` | — |
| AR-2 | AIRA work follows the documented plan, review, implementation, and merge loop. | active | `docs/dev/agentic-development-loop.md` | — |
| AR-3 | Correctness-critical changes use two-loop adversarial verification and durable counterexamples. | active | `docs/adversarial-verification.md` | — |
| AR-4 | Review and merge require independent correctness review, final-gate review, and an explicit coverage account. | active | `docs/review-and-merge-policy.md` | — |
| AR-5 | AIRA reports `pass`, `fail`, or `unevaluated` and refuses ambiguous or integrity-invalid operations. | implemented | `internal/core`, `internal/store` | `internal/store` stable-code and refusal tests |
| AR-6 | The coordination MVP and blocked-by ready queue ship first; the runner, telemetry, gates, and review surfaces follow in later phases. | met | phased delivery (coordination MVP → runner/telemetry/gates/daemon) | — |
| AR-7 | The release target is one static Go binary without cgo. | implemented | `cmd/aira`, `Makefile` | `go build` (cgo-free) |
