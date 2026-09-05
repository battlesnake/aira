---
{"schema":1,"id":"AIRA-87","project":"aira","title":"The ExitCodes catalogue can drift from the codes actually produced, with nothing testing either direction","status":"done","kind":"chore","severity":"P2","assignee":null,"milestone":null,"labels":["codes","honesty","layering","store"],"hold":false,"relations":[]}
---
PR #12 finding **B9** / plan candidate **75**, filed by the simplification programme's Phase 0
(plan §4.3). Source-verified against master `22cedd6`.

Suggested severity in the plan is **P3**; this repository's ticket schema has no P3, so it is
filed P2. Treat it as the lowest of the three P2s filed by this sweep.

## The defect

`internal/store/check.go:14` holds `var ExitCodes = map[string]int{...}` — **168 entries** —
inside the same file as `CheckReport` and the `check` verb's implementation. It is the
authority for `ExitForCode` (`check.go:100`) and is republished to every face through
`internal/core/response_contract.go:26`, which then reaches the generated skill contract
(`internal/core/skill.go:338`).

Nothing checks it against reality in either direction:

- A code **produced** by the tree but absent from the catalogue silently gets a default exit.
- A code **catalogued** but never produced is documented to agents as part of the contract and
  is simply dead — a promise the binary does not keep.

Both are honesty drift on the error-code surface, which is the one thing the CLI/MCP/Skill
faces are contractually supposed to be exact about. `W_GATE_DISABLED` (plan candidate 43) is
an already-identified instance of the second direction: catalogued, never emitted.

## Direction

1. Move the catalogue to a leaf package (`internal/codes`) so `store` stops being the home of
   a table every layer above it needs; `check.go` is not the right owner.
2. Add a produced-vs-catalogued test: enumerate the codes the tree can emit and assert the
   two sets agree, or that each divergence is explicitly listed with a reason.

## Sequencing

**Sequence with AIRA-45**, which concerns `E_DAEMON_PROTOCOL` classifier granularity in the
same area — the new produced/catalogued test must not encode the bucketing AIRA-45 changes.
Scheduled in the programme's Phase 5c (layering moves, done last because they touch package
boundaries every other phase edits inside).

## Resolution

Built as PR #41 (`588aeea`). `ExitCodes`/`ExitForCode` moved verbatim (171
entries, zero key/value changed, diffed against the pre-move file) to a new
leaf package `internal/codes`; ~140 call sites across 24 files rewritten.
`internal/codes/produced_test.go` does a real `go/ast` scan in both
directions, with per-code documented divergence tables rather than family
wildcards, so new drift still fails the build. Found 48 real divergences the
ticket's own snapshot didn't have (28 catalogued with zero behaviour change,
13 produced-but-uncatalogued, 7 catalogued-but-never-produced) — including
one genuine pre-existing honesty bug independent of this ticket:
`U_RELATION_GRAPH_UNESTABLISHED` was exiting 1 (generic failure) instead of
3 (unevaluated), now corrected. Independent Opus + DeepSeek-pro review
(CONCERNS, no P0) found one real P1 — pinning `W_`⇒0 by convention without
a structural guard meant a future `"W_X: ..."` error could exit 0 — fixed
with `TestNoWarningCodeIsRaisedAsAnError`. 8-direction mutation testing, all
caught.

Residual findings, not actioned here, recorded as AIRA-99: the `store.
ErrorCode`/`core.Do` structural gap behind that P1 (the test guards it, the
code doesn't yet forbid it), a dead `E_GIT_` classification branch in
`cmd/aira/tui_execute.go:467`, and the eleven re-bucketed `E_` exit codes
whose correct value is a contract decision outside a layering PR's scope.
