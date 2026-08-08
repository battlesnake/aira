# Review and merge policy

This policy applies to AIRA design documents, code, tests, and user-facing
surfaces. Review is part of the engineering record, not an informal last look.

## The gates

The normal order is:

`implement → local evidence → Codex review → Fable review → merge`

For a new subsystem, the same order applies to the plan before implementation:
Codex and Gemini review the plan when available, then Fable is the mandatory
plan gate. Fable is also the final build reviewer. Reviewer unavailability is
reported explicitly and retried where practical; it is not silently represented
as approval.

- Codex supplies an independent GPT-lineage correctness pass and reviews both
  false-fail and false-pass directions.
- Fable is the final gate and carries correctness, simplification, boundary,
  coupling, and abstraction lenses. An abstraction without a second real caller
  or a boundary crossing needs a concrete justification.
- Gemini reviews user-facing wording and, when available, adds an independent
  plan or product-surface opinion. It does not replace the correctness gate.

The owner may delegate the gate, but a merge requires the approved review record,
local validation, and an explicit account of deferred work. Repository CI is a
signal, not a substitute for the local evidence and review record.

## Coverage and test evidence

Coverage is judged vertically (unit, integration, functional, scenario, and
property layers where the risk warrants them) and horizontally across the paths
the change touches. A significant absent layer or breadth gap is written down
with a reason and must be accepted by the reviewers. Every confirmed adversarial
counterexample becomes a regression test.

For heavy commands, use `whale-run` and record the exact command and exit code.
An unavailable or unrun check is `unevaluated`; it is never reported as green.

## Phase-1 plan gate

The Phase-1 design must settle the §21 deliverables before implementation:

- write ordering and crash recovery across DB, git files, and the journal;
- SQLite atomicity and multi-worktree ID rebuild;
- lease CAS, holder identity, and clock source;
- selector grammar and ambiguity;
- file/config schemas and canonical relation storage;
- commit, discovery, prefix ownership, and machine-wide DB semantics;
- lease timing, stable codes, `aira check`, and reconcile scope/trigger.

The store-engine concurrency spike is evidence for the first two items and for
lease design. A failed spike blocks the design from assuming SQLite semantics.

## Merge record

A change handoff records the files, base and head commits, worktree and branch,
validation commands and exit codes, reviewer results, coverage rationale,
traceability state, unresolved questions, and explicit deferrals. The Phase-1
handoff must also say that no implementation code was written.
