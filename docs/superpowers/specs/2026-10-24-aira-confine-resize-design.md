# AIRA-312 — reduce the admission reserve of a running confine job

Owner request, 2026-10-24: a way, from the CLI and from `aira top`, to *manually
reduce the reservation size (used for admission)* of an existing task that is
clearly massively over-specified. Editing the real cgroup limit is optional ("if
not practical, we don't need to").

## Problem, restated

A granted job charges its declared reserve to the slice ledger for its whole life
(`admitWaiter.ledgerCharge()`, summed by `rederiveLedgerLocked`). A six-hour job
that reserved 24G and uses 2G keeps 22G out of everyone else's reach, and the only
remedy today is to kill it. The operator needs to hand back the unused part.

## Greenfield minimum

One daemon verb that lowers `waiter.reserve` of one granted job, re-derives the
ledger and wakes the evaluator. Everything else (CLI flag, MCP tool, `aira top`
key) is a thin face over that verb, shaped exactly like `confine-front` (AIRA-309).

## Decisions

| Question | Decision | Why |
|---|---|---|
| Ledger only, or also `memory.max`? | **Ledger only** in this ticket. `memory.max` is not touched. | Owner said the cap is optional. Lowering the cap below a long job's future peak turns a resize into a surprise OOM-kill of hours of work; leaving it means the job itself is not killed, at the cost of an under-counted ledger (see Honesty). |
| Increase allowed? | **No — reduce only** (`new < current`). | An increase can fail to fit; that is a queueing problem, not an edit. |
| Floor | `new >= current memory usage of the job's scope`, read live by the daemon; refuse when it cannot be read (`unevaluated`, never a guess). | Declaring less than the job is using right now is plainly wrong. Not a guarantee about future peaks. |
| Which jobs | Job-level granted, accounted waiters only. Refuse sub-reservations (`confine-reserve`) and aitest workers. | They follow their parent; a per-worker edit has no meaning. |
| Selector | Same grammar as `--kill`/`--front`: name, supervisor PID, or scope id; ambiguous refused. | Reuse; one language. |
| Ownership | The shared `CheckConfineOwner` guard (`--steal` for another session's job). | Same as kill/front. |
| New size argument | `--memory-reserve SZ` (existing flag, existing parser). Required with `--resize`. | No new size grammar. |
| Lifetime of a reduction | Dropped on ANY re-declare, not only a daemon restart: the lease keeper freezes the original grant (`lease_keeper_linux.go:100`) and a re-anchor overwrites `existing.reserve` (`admit.go:2148`). No scope-`memory.max` reconstruction path exists (scan adoption was deleted). Fail-safe direction (back to larger). | Same stance as `--front`: no persistence machinery. |
| Floor read | Daemon reads the resolved scope's `memory.current` with the existing reader (`sliceceiling.go:418`); malformed/unreadable → refuse; a measured 0 is accepted; ci-shim mode and scopes that are missing or still pending → refuse with a stated reason. It is an observed floor, not a guarantee about later allocation. | Honest `unevaluated`. |
| CPU / VRAM charges | Unchanged. | Out of scope; RAM only. |

## Honesty (what this does NOT do)

* The gap is **actual `memory.max` minus the new reserve**, and cumulative reductions widen it. Lowering the reserve also lowers the job's OOM-steering budget (`oomsteer.go:477`), so a later overrun can raise its OOM priority (`oomsteer.go:252`) and the job becomes the preferred victim under pressure. CLI help and the `aira top` confirmation page say so; a test pins it.

* The job's `memory.max` still equals its original reserve, so after a resize the
  ledger can under-count what the job may legally use, by up to
  `old - new`. This is the same bounded-not-airtight envelope as any operator
  override; the slice `MemoryMax` and the memory watchdog remain the backstops.
  `aira confine --list` shows the new reserve; the cap facet is unchanged and is
  the visible evidence of the gap. The skill text states this.
* The job's own trailer (printed at exit by the supervisor) still reads the
  reserve it was admitted with.
* A reduction is not learned by the peak-RSS estimator (it ignores reserve values); recorded budget evidence keeps the original supervisor figure. Documented beside the trailer note.

## Surface

* Daemon verb `confine-resize` (args: `selector`, `reserve_bytes`, `steal`,
  `slice`, `owner`) in `internal/daemon`, beside `confine_front.go`. Under
  `queue.mu`: resolve → ownership guard → validate (granted, job-level,
  `0 < new < old`, `new >= floor`) → set `waiter.reserve` → `rederiveLedgerLocked`
  → after unlock, `queue.signal()`. Audit line `aira daemon: confine-resize:
  actor=… old=… new=…`. Result type `runner.ConfineResizeResult` (scope id, name,
  owner, old, new).
* Codes: reuse `CodeConfineNotFound`, ambiguous and owner-unverified codes; add
  one for "not running" (queued job: points at `--front`/kill), one for
  "not a reduction", one for "below live usage".
* CLI: `aira confine --resize <selector> --memory-reserve <SZ> [--steal]`; help,
  verb table (`core.go`), routing, skill text. The MCP tool is generated from the verb table like `aira_confine_front`; no bespoke MCP work.
  Management-flag family: add to `confineManagementFlags` and the option checks
  (AIRA-301 refuses unknown options per verb).
* `aira top`: key **`e`** (edit reserve; NOT `r`, which is the global refresh and inline actions intercept keys before it — verify `e` is free in `tui_controller.go` and pick another letter if not) on a RUNNING row (queued rows keep `f`) opens a confirmation
  page showing job, owner, current reserve, live usage, the proposed size and the
  `--steal` acknowledgement exactly as `topFrontConfirm` does. The proposed size is
  entered as text on that page; the implementer picks the existing input mechanism
  in `tui_palette.go`/`tui_inline.go` and records the choice.

## Tests (each must be able to fail against the wrong implementation)

1. Ledger arithmetic: after a resize `queue.outstanding` equals Σ reserve with the
   new figure, and a queued waiter that did not fit before now fits and is granted
   (wake-up is real, not just a number change).
2. Every refusal leaves the ledger byte-identical: not found, ambiguous, queued
   job, released job, sub-reservation, increase, equal, zero, below live usage,
   usage unreadable, foreign owner without `--steal`.
3. Ownership: reuse `CheckConfineOwner`; one test per outcome (own attested job, foreign without `--steal`, foreign with `--steal`), not the full matrix again.
4. Race: resize concurrent with release of the same job and with a grant — `-race`
   clean, no negative ledger, no double discharge.
5. Re-declare (reconnect WITHOUT a restart, and after one) restores the original reserve, in both resize/re-declare orderings, plus stale-anchor EOF (documents the drop; fail-safe). All four re-derived totals are assigned together.
6. CLI face: option refusal (`--resize` without `--memory-reserve`, with a
   command), JSON shape, stable codes; MCP schema generated from the table.
7. `aira top`: key only on running rows; row-kind default-deny both ways; steal
   wording; confirm sends the exact request.

## Deferrals

Lowering `memory.max`; raising a reserve; CPU/VRAM edits; persisting a resize
across a daemon restart; per-worker (aitest) resizes.
