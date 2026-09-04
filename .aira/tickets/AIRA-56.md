---
{"schema":1,"id":"AIRA-56","project":"aira","title":"ready treats an unpopulated gate set as no constraint; needs a durable gates-were-configured signal","status":"done","kind":"bug","severity":"P2","assignee":null,"milestone":null,"labels":["gate","honesty","ready"],"hold":false,"relations":[]}
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

## Resolution (2026-09-04, backlog-remediation Phase 0, plan section 2)

`ready` now surfaces the **existing** `U_GATE_SET_EMPTY` primitive (AIRA-54's,
not a new one) as an **advisory** finding when the gate set is unpopulated. It
attaches the finding and does NOT touch `Ready` or `Verdict`.

It attaches only to records that are still **ready**. Those are the ones making
the claim this ticket is about — `ready: true` with no gate signal, where
"nothing was checked" reads as "nothing is wrong". A record already reporting
`fail` or `unevaluated` is not making that claim, and a second, weaker finding on
it would be noise stacked on the reason a reader actually needs. (Found by an
existing core test that asserts exactly one finding on an unevaluated-graph
record: the right answer was to narrow the attachment, not to relax that test.)

That is the whole design, and both halves are load-bearing:

- **Surfaced**, because the defect is real: with zero discovered gates the gate
  block was skipped wholesale, so a caller reading `ready: true` got no signal
  that nothing gate-shaped was established. "Nothing was checked" was
  indistinguishable from "nothing is wrong".
- **Not blocking**, because this ticket's own analysis rules the alternative out:
  "Unconditionally blocking readiness when no gates exist would make the ready
  queue unusable for every project not using the opt-in gates feature." The
  filesystem has no sound "this project uses gates" predicate to narrow it with
  either — `hasGateContent`, the candidate, is false for exactly the
  accidentally-emptied directory this ticket cares about, and is deleted in the
  same PR (AIRA-89, landed once, not double-counted).

### The suggested direction is deliberately NOT taken, and that is a gap, not an oversight

The ticket proposes consulting the authenticated gate audit ledger for prior
`result`/`proof-of-fire` records and HOLDING readiness when discovery now yields
zero gates. That is a real design, and it is declined here: it makes readiness
depend on durable ledger memory — new state on the readiness path — which is more
machinery than this honesty gap justifies under this repository's
architectural-simplicity rule. Recorded as an **accepted coverage gap**: a
project whose gates were deleted after recording results is now SURFACED but not
HELD. Anyone who wants the hold has the ledger reader and the finding code
already in place to build it on.

Kind is `warning`, not `unevaluated`: the finding's role in the ready report is
advisory, while the code's own `U_` prefix carries the "nothing was evaluated"
fact. A record that is genuinely unevaluated for gate reasons still comes through
the `U_GATE_EVIDENCE_UNAVAILABLE` branch above, which does block.

### Test

`TestReadySurfacesAnUnpopulatedGateSetWithoutBlocking`
(`internal/store/gate_honesty_test.go`) asserts BOTH halves on the same rows,
because each alone passes against a wrong implementation. Mutation-verified:
disabling the new branch fails it with "an unpopulated gate set is still
invisible" (the exact pre-fix behaviour), and making the finding set
`Ready = false` fails it with "was BLOCKED by an empty gate set".

The ticket's other requirement — "a project that never used gates is unaffected"
— is what the non-blocking half IS: the fixture has no `.aira/gates` directory at
all, which is both the never-adopted shape and the emptied shape.

AIRA-56 -> done. `make ci`: exit 0.

### Build-review (Sol, 2026-09-04) — one named gap, no code change

Sol confirmed the store branch is reachable, correctly ordered against the other
two arms, and deduplicated, and that the CLI, JSON and MCP faces all surface the
finding while preserving `pass` and exit 0.

**Named gap: the TUI does not show it.** `cmd/aira/tui_data.go` decodes only
`Response.Data` and discards `Response.Warnings`, and the Ready view renders
ready/verdict columns without each row's findings
(`cmd/aira/tui_viewmodel.go`). A TUI user therefore still sees `yes / pass` with
no empty-gate signal.

Not fixed here, deliberately: the TUI's Ready view ignores **every** finding, not
just this one, so the fix is a TUI view change (surface row findings at all)
rather than anything about AIRA-56 — and making it about this one finding would
be the wrong shape. Recorded so the gap is written down rather than discovered
later by someone who trusted this ticket's closure. The faces that agents and
scripts actually consume — CLI text, JSON, MCP — do surface it.
