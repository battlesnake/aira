---
{"schema":1,"id":"AIRA-86","project":"aira","title":"check seeds all fourteen honesty dimensions as pass, so an unwired dimension reports a fabricated green","status":"planned","kind":"bug","severity":"P2","assignee":null,"milestone":null,"labels":["check","honesty","store"],"hold":false,"relations":[]}
---
PR #12 finding **B8 / P9** / plan candidate **76**, filed by the simplification programme's
Phase 0 (plan §4.3). Source-verified against master `22cedd6`.

## The defect

`internal/store/check.go:130-134` builds the report by seeding every dimension `pass`:

    report := CheckReport{Verdict: "pass", Dimensions: map[string]string{
        "allocated-id-file": "pass", "duplicate-id": "pass", "stale-index": "pass",
        "orphan-worktree": "pass", "ticket-file-integrity": "pass", "reconcile-integrity": "pass",
        "rebuild-integrity": "pass", "relation-integrity": "pass", "finding-integrity": "pass",
        "lease-integrity": "pass", "area-overlap": "pass",
        "traceability": "pass", "gates": "pass", "compute": "pass",
    ...

A dimension is then demoted to `fail`/`unevaluated` only by code that actually runs. So a
dimension whose checker never executes — not wired, an early return, a new dimension added to
the map before its checker exists — reports **`pass`**. That is a fabricated green in exactly
the class AIRA-53, AIRA-54 and AIRA-72 were filed for, and it is the default for all fourteen
at once.

**AIRA-54 (done) fixed one dimension by hand.** Seeding `unevaluated` instead makes that
whole bug shape unrepresentable: a dimension is green only because something established it.

## Direction

Seed `unevaluated`, and let each checker raise its own dimension to `pass` when it has
actually established the result. The verdict rollup already knows how to treat
`unevaluated` (`check.go:139-141` demotes on it today).

## Rigor: Tier B with a mandatory per-dimension test

The programme's plan considered Tier A and **declined it, with the reason recorded rather
than asserted**: a botched seed change fails in the **false-`unevaluated`** direction, which
is loud and testable, not the false-`pass` direction Tier A exists for. The condition is a
test per dimension asserting its `pass` path still reaches `pass` — otherwise the fix trades
a silent false-green for a silent false-`unevaluated` and nobody notices either.

Scheduled in the programme's Phase 3b (moved out of the Phase 0 mechanical pass on review,
because it changes the default of every honesty dimension simultaneously).

## Partial resolution (2026-09-04): the gate_eval.go / gate_ratchet.go sites

STILL OPEN for its headline site, `internal/store/check.go:130` (the fourteen
honesty dimensions), which remains a standalone item with this ticket's own
mandatory per-dimension test condition.

Closed here, as part of the captured-subject fix, are the seeded-pass sites in
the two gate files:

- `gate_eval.go` `evaluateDimension`: the scratch report and the returned
  predicate are seeded `unevaluated`, and pass is raised in the arm that
  establishes it. Honest scope note: the scratch report's Verdict/Dimensions are
  never read in that function, so that half is shape-hardening with no
  observable behaviour change.
- `gate_ratchet.go` `compareNoNewFailures`: seeded `unevaluated`, raised to pass
  in the else-arm, matching `compareCoverage`. Behaviour-preserving by
  construction -- mutation testing confirms no test can distinguish it, reported
  rather than papered over. This ticket's mandatory condition (the pass path
  still reaches pass) is asserted by
  `TestRatchetComparatorStillReachesPassWhenNothingRegressed`.
- `gate_eval.go` `GateCheck`/`finishGateReport`: the report is seeded
  `unevaluated` and the rollup is established positively. Both plan-review
  lineages independently found a residual hole this closes: a result's Verdict
  is a raw string read out of the audit ledger, so a report holding one genuine
  pass and one unrecognised verdict counted nothing for the latter and reported
  PASS. The counting switch now has an explicit `default: Unevaluated++`, and a
  results-empty report is unevaluated rather than vacuously green.

Plan: docs/superpowers/plans/2026-09-04-aira80-81-60-86-captured-subject-plan.md
