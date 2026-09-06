---
{"schema":1,"id":"AIRA-32","project":"aira","title":"aitest Slice 3 — worker-admit generalisation polish + field-tuned watermarks","status":"done","kind":"feature","severity":"P2","assignee":null,"milestone":null,"labels":["admission","aitest","pytest"],"hold":false,"relations":[]}
---
Spec: docs/superpowers/specs/2026-09-01-aitest-design.md (§6 open questions, §5 Slice 3).

From Slice 1/2 field data: pick the memory.high-crossing proactive-recycle
watermark fraction, review worker-suite signature definition for peak-history
keying, and review the worker-admit wire shape for genuine language-agnostic
generality (only client today is aitest itself). Blocked by Slice 2.

## Precondition restated (2026-09-04, backlog-remediation Phase 0, plan section 2) — text only

**The stated blocker is satisfied and therefore misleading**, and there are two
further preconditions this ticket never named. It reads "Blocked by Slice 2";
Slice 2 is AIRA-31, which is **done**, so a reader coming here directly would
conclude it is ready to build.

**The real, current preconditions are three, all outstanding:**

1. **AIRA-33's precondition, shared:** AIRA-91 Part A built + fastest-ee
   re-verified + the `FASTEST_NO_AITEST=1` pin removed. This ticket's field-tuning
   wants Slice 1/2 field data from a suite actually running on aitest.
2. **AIRA-91 Part B decided** (the oomd-vs-admission policy call, backlog-
   remediation plan section 5 item 5 — explicit owner sign-off required, not a
   review-and-proceed decision).
3. **AIRA-35 landed.** This ticket's scope includes picking "the
   `memory.high`-crossing proactive-recycle watermark fraction" — and AIRA-35,
   contingent on the Part B decision, may remove worker-scope `memory.high`
   altogether (`internal/daemon/worker_admit.go:285`). Tuning a watermark on a
   mechanism that may be deleted is wasted work, and the plan carries AIRA-35 as
   gated on exactly that decision.

Preconditions 2 and 3 are this ticket's own, beyond the AIRA-33 chain: an earlier
draft of the remediation plan attributed the block to the AIRA-91/AIRA-33 chain
alone, which was incomplete.

## Resolution (2026-09-05, backlog-completion triage)

Verified against current master (3251bed). All three scope items are either already satisfied by inspection, have no consumer to act on, or are a soft-tuning knob that already exists and whose substrate is under a different ticket's decision.

(1) Watermark fraction: the proactive-recycle watermark already ships as an operator-tunable knob — internal/pylib/aitest/worker.py:9 `_DEFAULT_HIGH_WATERMARK_PCT = 80`, env override `AIRA_AITEST_WORKER_HIGH_WATERMARK_PCT`, evaluated at worker.py:257-265 against the scope's `memory.high`. There is no telemetry anywhere capturing per-worker memory.current-vs-high at recycle, so no "field data" exists to tune from, and ~a week of real fastest-ee runs on aitest (AIRA-64, AIRA-100, AIRA-91 incidents) surfaced OOM/PSI/timeout problems but never a recycle-timing symptom. The mechanism itself is a soft heuristic: containment is memory.max + oom.group, and the spec (§3, line 318) says a worker OOM-killed by its own memory.max is "a normal, expected event, not an incident". The substrate is also still in flux — worker `memory.high` is armed at internal/daemon/worker_admit.go:734 (`estimatedBytes*4/5`; ticket's `:285` reference is stale), and AIRA-35 (still `planned`, still undecided between narrowing the gap and deleting worker memory.high) owns that; the remediation plan's AIRA-35 row already moves the watermark read to memory.max if deletion wins. Tuning a number on a knob that is already tunable, with no data and a substrate another ticket may replace, is exactly what the owner's architectural-simplicity HARD rule and CLAUDE.md's "primitives, not judgement" bar say not to spend a milestone on.

(2) Signature definition review: nothing to review. worker_admit.go:67-73 documents `signature` as accepted-on-wire but UNUSED; no peak-history/resolveAdmitReserve call exists in worker_admit.go at all; __init__.py:137-147 sizes workers from `AIRA_AITEST_ESTIMATED_BYTES` (default 512 MiB) and states the same deferral. Spec §6 itself grades this "low risk either way". The owner's AIRA-91 Part B decision (4abedae) directs sizing toward AIRA-29's track-actual (charge live usage), which makes static per-signature peak estimates less relevant. If per-suite worker sizing is ever wanted, that is a new feature ticket under AIRA-29's direction, not a "review".

(3) Wire-shape generality review: satisfied by inspection. Request fields job_id/outer_scope/signature/estimated_bytes/max_wait_ms and response state/class/reason/detail/waited_ms/worker_id/scope_path/memory_max/memory_high/cpu_slots (worker_admit.go:41-76) carry nothing pytest-specific; the only pytest coupling is client-side (the Python supervisor parsing the `aira worker-admit` CLI relay's stdout line, supervisor.py:316-395). The sole client is aitest; no second client exists or is filed. A generality review for a hypothetical client is speculative work the architectural-simplicity rule forbids; when a real second client appears, it reviews the shape against its own needs.

Preconditions: (1) AIRA-91 Part A is built and deployed (PR #35/#36, 2026-09-05); whether fastest-ee's FASTEST_NO_AITEST=1 pin is removed is not establishable from this repo (it lives in fastest-ee), though AIRA-64's data shows fastest-ee's merge_gate engine leg already runs on aitest. (2) Part B is decided (owner 2026-09-05: oomd stays; fix on AIRA's sizing side) but that did NOT decide worker memory.high's fate. (3) AIRA-35 has not landed. None of this changes the disposition: even with all three satisfied, the three scope items above still resolve to "knob exists / no consumer / already true".

Not needs-owner-decision: the only genuine fork (keep/narrow/remove worker memory.high) belongs to AIRA-35, not this ticket. Not build-small: any "build" here would be inventing scope (e.g. recycle telemetry) for a soft heuristic that another ticket may remove. Not close-superseded: AIRA-35 has not yet deleted memory.high, so nothing has actually superseded this; it is simply not needed on its own merits.

Resolution note to record on close: watermark is tunable via AIRA_AITEST_WORKER_HIGH_WATERMARK_PCT (default 80); signature is an unused wire field pending any per-suite sizing feature (would be a new ticket under AIRA-29's track-actual direction); wire shape verified non-pytest-specific; if AIRA-35 keeps memory.high and a real recycle-timing symptom is ever reported, file a fresh ticket with that data. Also flag to AIRA-35's triager that the `worker_admit.go:285` line reference in the plan and tickets is stale (now :734).


*Disposition: Closed — not needed, reached via a source-verified triage pass (Fable model) as part of the backlog-completion push, independently spot-checked by the coordinating session before closing.*

## Amendment (AIRA-35, 2026-09-06)

This ticket's closing resolution names `AIRA_AITEST_WORKER_HIGH_WATERMARK_PCT`
and `_DEFAULT_HIGH_WATERMARK_PCT = 80`, evaluated against the scope's
`memory.high`. AIRA-35 landed the fork this resolution said belonged to it, and
chose REMOVAL: worker scopes no longer carry `memory.high` at all (it was
measured as a convergence livelock — 420 s without converging at 80%, 16-18 s at
95% at the shipped 512 MiB cap), and they now carry `memory.swap.max=0` so
`memory.max` actually contains.

So the knob this resolution describes has moved, exactly as the resolution
anticipated ("the remediation plan's AIRA-35 row already moves the watermark
read to memory.max if deletion wins"):

- env var: `AIRA_AITEST_WORKER_HIGH_WATERMARK_PCT` -> `AIRA_AITEST_WORKER_MEMORY_WATERMARK_PCT`
- constant: `_DEFAULT_HIGH_WATERMARK_PCT = 80` -> `_DEFAULT_MEMORY_WATERMARK_PCT = 64`
- read from: the scope's `memory.high` -> the scope's `memory.max`

The change of default from 80 to 64 is a NO-OP by construction, not a retune:
the old check fired at 80% of a `memory.high` the daemon set to 80% of
`memory.max`, i.e. at 64% of `memory.max`. AIRA-35 deliberately did not touch
the fraction, so this ticket's disposition ("tuning a number on a knob that is
already tunable, with no data") stands unchanged -- only the knob's name and the
file it reads have moved. If a real recycle-timing symptom is ever reported,
file a fresh ticket with that data, against `memory.max`.

relates AIRA-35.
