---
{"schema":1,"id":"AIRA-56","project":"aira","title":"ready treats an unpopulated gate set as no constraint; needs a durable gates-were-configured signal","status":"planned","kind":"bug","severity":"P2","assignee":null,"milestone":null,"labels":["gate","honesty","ready"],"hold":false,"relations":[]}
---
## Symptom

`Ready` gates its entire gate contribution behind `len(gateReport.Results) > 0`
(`internal/store/relation_ready.go:571`). With zero discovered gates the gate
block is skipped wholesale, so ready records stay green and a caller reading
`ready: true` gets no signal that nothing gate-shaped was established.

## Relationship to AIRA-54

Found while fixing AIRA-54 (empty gate set fake-passed `aira gate check`) and
raised by the Codex/Sol plan-review lineage. AIRA-54 fixed the two paths that
make an *affirmative* fabricated claim:

- `GateCheck` returned `pass` for an empty set — now `unevaluated` /
  `U_GATE_SET_EMPTY`.
- the aggregate `Check` pre-seeded `Dimensions["gates"] = "pass"`
  (`internal/store/check.go:131`) — now overwritten to `unevaluated` for an
  empty set.

`ready` was deliberately left out of AIRA-54's scope, with the reasoning
recorded in `docs/plans/2026-09-03-aira53-54-gate-honesty-plan.md`. Unlike the
two above, `ready` asserts nothing about gates when there are none — it adds no
constraint rather than claiming `pass` — so the CLAUDE.md fabricated-pass rule
does not bite it in the same direct way. It is still a place where "nothing was
checked" is indistinguishable from "nothing is wrong".

## Why it was not fixed in AIRA-54

There is no sound filesystem predicate for "this project uses gates":

- `hasGateContent()` (`internal/store/gate_index.go:45`) is `len(entries) > 0` on
  `.aira/gates`, which is **false for an accidentally emptied gates directory** —
  precisely the regression the ticket cares about. It would therefore miss the
  real case while penalising every project that never adopted gates.
- Unconditionally blocking readiness when no gates exist would make the ready
  queue unusable for every project not using the opt-in gates feature.

## Suggested direction

Use durable evidence of prior gate activity rather than the current filesystem
state: the authenticated gate audit ledger in the common directory
(`OpenGateAudit` / `GateAuditRecord`). If the ledger holds prior `result` or
`proof-of-fire` records for this project but discovery now yields zero gates,
the gate set regressed — attach a `U_GATE_SET_EMPTY` unevaluated finding and
hold readiness, matching the existing per-gate unevaluated treatment at
`relation_ready.go:582-595`. If the ledger has never recorded gate activity,
gates are genuinely not part of this project's readiness contract and `ready`
stays unconstrained.

Tests must cover both directions: a project that never used gates is unaffected,
and a project whose gates were deleted after recording results is held
`unevaluated`.
