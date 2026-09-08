---
{"schema":1,"id":"AIRA-180","project":"aira","title":"Track real resource usage for tasks and groups of tests vs budget, and recommend (never auto-adjust) quota changes","status":"planned","kind":"feature","severity":"P2","assignee":null,"milestone":null,"labels":["admission","aitest","confine","insights"],"hold":false,"relations":[{"kind":"relates","from":"AIRA-180","to":"AIRA-181"}]}
---

Owner observation (2026-09-08): "I see a lot of sessions are setting explicit
RAM reservations for tests and then having to adjust them either due to
OOMs or lack of admission — instead of being able to just use aitest, let
aira govern things, and have memory usage annotations on tests where
needed." Refined to the authoritative scope after discussion: "aira should
track real resource usage for tasks and groups of tests vs budget, and make
recommendations to agents in projects about adding or adjusting quotas for
tests and/or tasks." Owner asked for a plan first, not a build.

## Plan

Full design: `docs/superpowers/plans/2026-09-08-resource-usage-budget-recommendations-plan.md`.

Summary of the proposal:

- **Report/recommend only — no code path auto-adjusts a quota.** Follows
  the house style already established by the AIRA-52 adequacy-gauge family
  (`internal/store/admission_insight.go`), `aira check`'s graded dimensions,
  and the sliceceiling/main.go diagnostic-line pattern: compute purely from
  persisted evidence, publish per-subject cells with sample counts and
  every exclusion named, `unevaluated` with a specific reason wherever a
  term is absent, state the recommendation as a counterfactual kept
  structurally separate from anything applied.
- **Two subjects, two collection states.** Task-level (an `aira run`/
  `aira confine` invocation, keyed by the existing `runner.ResourceSignature`)
  already has usage history via `confine_peak_history` — this is purely a
  new comparison/reporting layer, no new capture needed. Test-group level
  (an aitest worker pool) has zero history today — `AIRA_AITEST_ESTIMATED_BYTES`
  is a single static env var with no per-suite sizing and no durable
  project-scoped home — and needs new capture at `supervisor.py`'s existing
  `_forget_worker_scope` teardown point (read `memory.peak` before rmdir),
  at worker-pool granularity, deliberately NOT per individual test.
- **Explicitly does not reintroduce per-test `aira_mem()` annotation.**
  Verified that marker is deleted (AIRA-33) and actively forbidden by a
  build-breaking test (`skill_test.go:584-610`) asserting it is absent from
  generated docs; `skill.go` teaches "no per-test RAM annotation of any
  kind" as current, deliberate guidance. This design stays advisory and
  routes any recommendation through existing durable knobs
  (`--memory-reserve`/`--memory-max`, `AIRA_AITEST_ESTIMATED_BYTES`), never
  a new per-test marker.
- Two surfaces over one pure classifier: a new `aira check` dimension for
  passive routine visibility (`Warning`, never `Finding` — advisory, not a
  defect), and an on-demand query verb (name deferred to the gate, likely
  under `aira insights`) so an agent can ask "what would currently be
  recommended" *before* pinning a reserve, not only after an OOM.
- MCP/Skill integration via the standard dispatch-table path (confirmed
  reusable, same as AIRA-176's finding).

Five open questions recorded in the plan's §5 for the gate (new table vs.
extending `confine_peak_history`; verb name/home; whether the durable
`.aira/config` knob ships in v1; the over/under-provisioned threshold; and
whether a `Warning` needs a persistence threshold to avoid single-run
noise) — none blocking, all deliberately left for the build to decide.

## Evidence this addresses real, current pain

[[AIRA-160]] already verified a narrower, real gap (aitest worker sizing is
memory-blind, CPU-only auto-sizing). Tonight, three independent peer
sessions reported the caller-side symptom this design targets: qual had no
way to ask what reserve would currently admit (guessed 40G→30G, the lower
guess then OOM-killed with zero test results produced); subpipe measured
six confined runs at `--memory-reserve 8G` peaking 52 MiB–841 MiB actual
RSS (up to ~156x over-reservation, each one holding headroom other waiters
needed on a saturated slice); split independently reported the same
guess-then-adjust cycle. None of this is a hypothetical.

## Status

Planning only. Not yet gated, not yet built, per the owner's explicit
request.
