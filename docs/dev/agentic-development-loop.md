# Agentic development loop

This repository uses a plan-first, review-gated V-model. The process keeps a
cheap design correction from becoming an expensive implementation rewrite and
makes correctness evidence durable.

## Standard loop

```text
start → plan → plan-review → plan gate → plan-fix → implement
      → work-review → work-fix → build gate → PR → merge
```

| Step | Who | Required output or gate |
|---|---|---|
| Start | owner/agent | Clean feature worktree, starting commit, checked prerequisites |
| Plan | implementer | Dated design spec with scope, invariants, seams, risks, tests, and deferrals |
| Plan review | Codex + Gemini, when available | Independent findings on shape, omissions, usability, and yield; review-only |
| Plan gate | Fable | Adversarial correctness, simplification, boundary, and abstraction review; approval before code |
| Plan fix | implementer | Findings folded into the spec; unresolved items are explicit |
| Implement | implementer | TDD implementation in the approved scope; exact command exit codes |
| Work review | Codex, then Fable | False-pass/false-fail review; two-loop red-team for correctness-critical work |
| Work fix | implementer | Every confirmed counterexample gets a regression test |
| Build gate | Fable + local evidence | Tests, coverage rationale, traceability, and review approval |
| PR / merge | owner or delegated gate | Feature branch only; the approved review record travels with the change |

The current Phase-1 plan gate is hard: Phase-1 implementation code is not
written until the SQLite result and every §21 design decision have been reviewed
and approved.

## How to work the loop

The plan must name the source of truth, the failure behaviour, the transaction or
ownership boundary, the observable evidence, and the things deliberately left
out. Design around prerequisites: the `blocked-by` ready queue is an early
feature because it prevents work behind an unlanded dependency.

Implementation starts with the smallest never-wrong cases. Tests cover both
directions of each invariant: valid input is accepted and invalid or ambiguous
input is refused. Correctness-critical changes use adversarial probes against
short-lived processes and crash/interleaving scenarios, not only in-process happy
paths.

## Lighter path for trivial changes

A typo, link correction, comment-only edit, or mechanical change with no
behavioural effect may use: inspect → edit → diff check → relevant lightweight
validation → one reviewer sanity check. It still stays in a worktree, records
the exact validation result, and must be escalated to the full loop when it
touches correctness, user-facing behaviour, persistence, IDs, leases, or the
agent workflow itself.

## Evidence discipline

Never infer `pass` from silence, a truncated log, an exit code without a parsed
result, or a test suite that did not actually exercise its target. Use the three
verdicts `pass`, `fail`, and `unevaluated`. A review finding is not closed by
prose alone: reproduce it, fix it, and pin the counterexample in a test or an
explicit durable design decision.
