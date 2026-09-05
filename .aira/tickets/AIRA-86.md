---
{"schema":1,"id":"AIRA-86","project":"aira","title":"check seeds all fourteen honesty dimensions as pass, so an unwired dimension reports a fabricated green","status":"done","kind":"bug","severity":"P2","assignee":null,"milestone":null,"labels":["check","honesty","store"],"hold":false,"relations":[]}
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

## Resolution (2026-09-05): the headline `check.go` seed site

Closed. `Check` no longer seeds any result. `newCheckReport` starts with an
empty `Dimensions` map, `checkDimensions` is the canonical list of the fourteen
(a list of names, not of results), and a dimension is reported only because
something put a result there:

- `establishDimension` raises a dimension to `pass`, called only where a checker
  ran and established a clean result. It never overwrites a recorded fail,
  warning or unevaluated, in either order.
- `finaliseDimensions` reports every dimension nothing established as
  `unevaluated` with a `U_CHECK_UNEVALUATED` finding naming it, and that demotes
  the rollup verdict the way any other unevaluated result does. An unwired
  checker, a checker that returns early, or a dimension added to the list ahead
  of its evaluator now reads as unevaluated instead of a fabricated green.
- `unevaluateDimension` records "no result established" for the callers that
  hold the reason as a finding of their own; it keeps a recorded `fail` rather
  than laundering it into an unevaluated.

The mandatory condition this ticket set -- a test that each dimension's pass path
still reaches pass, so the fix does not trade a silent false green for a silent
false unevaluated -- is
`TestCheckReportsEveryDimensionPassWhenEveryCheckerEstablishesIt`: one fixture
(git worktree + requirement registry + covered and verified built requirement +
proven gate) in which all fourteen establish, asserted per dimension. Deleting
any single establishment site fails it, verified by mutation.

### Behaviour changes this exposed, not papered over

- **A non-git root now reports traceability `unevaluated`, not `pass`.**
  `checkTraceability` returned early for a non-git ticket-only project without
  evaluating anything, and the seed left `pass` behind -- the exact defect, live.
  That arm now records `U_TRACE_UNSCANNED` with its reason. Ticket-only non-git
  fixtures therefore report an unevaluated verdict where they used to report
  pass; real projects are git worktrees and are unaffected.
- **`checkDuplicateIDs` marks `ticket-file-integrity` unevaluated for a worktree
  it could not scan.** The same failed ticket scan is the only evidence either
  dimension has for that worktree, so establishing one and not the other would
  have kept a narrower version of the same fabricated pass.
- **`checkGatesReadOnly` establishes its own `gates` dimension**, guarded on the
  gate report carrying at least one result, like `checkTraceability`. Honest
  coverage gap: a results-empty report with any code other than
  `U_GATE_SET_EMPTY` is unreachable today (every discovered gate contributes
  exactly one result), so that guard arm is untested.
- `U_CHECK_UNEVALUATED` is now registered in the exit-code catalog. It was
  already emitted on the cancelled-context path without being catalogued.

### Build review (DeepSeek-pro, independent lineage): two confirmed, two not

Both confirmed findings were real holes the establishment sites would otherwise
have perpetuated, and each is now a regression test:

- **A partly-unreadable requirement registry reported traceability `pass`.** A
  malformed node is an `E_REQUIREMENT_INVALID` fail finding carried with no
  dimension of its own, and only an edge that references it demoted the
  dimension. One readable requirement plus one unreadable one that nothing
  annotates reached the establishment arm and claimed the whole graph.
  `checkTraceability` now records `U_TRACE_UNSCANNED` whenever any node is
  unreadable, not only when none is readable
  (`TestTraceabilityIsNotEstablishedWhileARequirementIsUnreadable`; the
  byte-for-byte golden was updated deliberately, with the reason in the test).
- **`finaliseDimensions` depended on `addFinding` to write the dimension**, and
  `addFinding`'s unevaluated branch dedupes on (Code, Subject) and returns
  before it touches the dimension. Unreachable today, but the one place whose
  entire job is to never leave a dimension unset should not depend on a dedupe.
  The dimension is now written first
  (`TestFinaliseDimensionsWritesTheDimensionEvenWhenItsFindingIsADuplicate`).

Not accepted, with reasons:

- "The rollup ignores direct dimension failures" — refuted against source: all
  nine `Dimensions["allocated-id-file"] = "fail"` sites append a finding in the
  same block, so the verdict is `fail`.
- "`addFinding`'s unevaluated branch overwrites a recorded `fail`" — real, and
  inconsistent with the traceability rank system where `fail` outranks
  `unevaluated`, but pre-existing, out of this ticket's scope, and not a false
  pass (the verdict stays `fail` because the findings list is non-empty). Filed
  as an observation here rather than changed under an AIRA-86 PR. Note the new
  `unevaluateDimension` takes the opposite (fail-preserving) rule deliberately.

### Named, not fixed

`test-reports` is a fifteenth dimension that materialises in the map only when a
flaky-report finding demotes it: it is never seeded and never established, so a
consumer cannot tell "evaluated, nothing wrong" from "not evaluated". That is
not a fabricated pass (an absent key claims nothing) and it is out of this
ticket's fourteen-dimension scope, so it is reported here rather than changed.

### Independent verification (2026-09-05): two porous spots, now closed

An independent pass re-derived the fix against source in a detached worktree at
the merge commit (`2b3a3e0`) rather than trusting the build report. The fix
itself holds:

- The deviation from the plan's literal wording (the plan said "seed all
  fourteen `unevaluated`"; the fix starts the map empty and `finaliseDimensions`
  reports the unestablished) is sound and strictly more informative. Verified
  structurally: `Check` has exactly two `return report, ...` paths — the
  cancelled-context one, which writes all fourteen `unevaluated` itself, and the
  final one, which runs after `finaliseDimensions`. Every other return is
  `CheckReport{}, err`, an empty report the caller reports as an error. So no
  reachable path returns a report with a dimension silently absent.
- No regression in the `eject` durability gate: `ejectDurabilityFinding`
  (`internal/daemon/eject.go:239-244`) consults eight dimensions, all of which
  are established unconditionally on `Check`'s success path, and it already
  treats an absent dimension (`""`) as nothing to report.
- The refuted build-review finding is correctly refuted: all nine direct
  `Dimensions["allocated-id-file"] = "fail"` sites append a finding in the same
  block, checked line by line.
- Re-run at `2b3a3e0`: `go build ./...` exit 0, `go vet ./internal/... ./cmd/...`
  exit 0, `go test ./... -count=1` (whole module) exit 0.

A ten-mutation sweep (five re-deriving the build report's, five new) found
**three survivors**, of which two were real coverage gaps in the merged work —
the fix's behaviour was right, the tests did not pin it:

- **Deleting the `finaliseDimensions(&report)` call from `Check` was invisible
  to the whole module.** Every dimension shipped today is established by its
  checker or demoted by one of its own findings, so the map is fully populated
  with or without the call, and the existing test exercises the helper in
  isolation rather than its wiring. The case the call exists for — a dimension
  in the canonical list that no checker establishes, which is the *only*
  behaviour the AIRA-86 fix adds over the status quo — had no coverage at all.
  Now pinned by `TestCheckReportsADimensionNoCheckerEstablishedAsUnevaluated`.
- **Deleting both `unevaluateDimension(report, "ticket-file-integrity")` calls
  in `checkDuplicateIDs` was invisible to the whole module**, and the mutant
  reports `ticket-file-integrity: pass` for a worktree whose ticket scan was
  inconclusive — the same fabricated green as the seed, one dimension wide.
  (`checkStaleIndex`'s own inconclusive arm records only `stale-index`, so it
  cannot stand in for it.) Now pinned by
  `TestCheckDoesNotEstablishTicketFileIntegrityForAWorktreeItCouldNotScan`,
  which drives the real `scanReadHook` seam rather than asserting on a
  hand-built report.

Both new tests were themselves mutation-verified: each fails against its
mutation and passes against the shipped code.

The third survivor — removing the `len(gateReport.Results) > 0` guard in
`checkGatesReadOnly` — is an equivalent mutant today, exactly as this ticket's
"honest coverage gap" note above already states. It stays named, not fixed.

Plan: docs/superpowers/plans/2026-09-04-backlog-remediation-plan.md (Phase 2)
