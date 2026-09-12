---
{"schema":1,"id":"AIRA-230","project":"aira","title":"aitest v0.7 Stage S1 — foundation + aggregate outer-cap guard + measurement","status":"planned","kind":"feature","severity":"P2","assignee":null,"milestone":null,"labels":["admission","aitest","v0.7"],"hold":false,"relations":[]}
---
Stage S1 of the aitest v0.7 design (`docs/superpowers/specs/2026-09-12-aitest-v07-class-sized-workers-design.md` §11). Slice plan: `docs/superpowers/plans/2026-09-12-aitest-v07-s1-plan.md`.

Ordered two-loop slices (Opus builds, Fable reviews, each mutation-verified):
- **v7-1** — the client-side aggregate outer-cap Σ-guard + Tier-1 dual-ceiling (fixes AIRA-229; the load-bearing correctness slice; lands FIRST, works on the shipped flat request, mode-dependent for ci-shim). Also fixes the stale worker_scope_linux.go:52 doc comment.
- **v7-2** — the `aira_mem` marker reader + `nodeid → bytes` map (pure-additive foundation; unwired).
- **v7-3** — size-class ladder + per-worker request sizing (pure function; unwired — wiring + per-class dispatch are S2).
- **v7-4** — measurement instrumentation + committed repro harness (fixes the tunables: 256 MiB default, per-worker headroom, watermark fraction, supervisor allowance, the MAX_TESTS fork).

DEFERRED to S2 (plan D1/D3, §11 refinement): per-class spawn sizing, per-class dispatch, the growth claim, the ~10 s age-cap flip, and the `Σ(pending requests)` guard term + its mutation guard (inert in S1's single-threaded ≤1-pending model). NON-goal: batch/prune (retired per the design's §2; reinstate path OD1, S2-binding only).

No daemon protocol change. Python-side + one Go doc-comment fix.
