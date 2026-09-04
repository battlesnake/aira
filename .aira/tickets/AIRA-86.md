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
