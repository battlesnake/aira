---
{"schema":1,"id":"AIRA-180","project":"aira","title":"Track real resource usage for tasks and groups of tests vs budget, and recommend (never auto-adjust) quota changes","status":"done","kind":"feature","severity":"P2","assignee":null,"milestone":null,"labels":["admission","aitest","confine","insights"],"hold":false,"relations":[{"kind":"relates","from":"AIRA-180","to":"AIRA-181"}]}
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

## §5 resolved (gate, 2026-09-09)

All five decided in the plan's new **§5r**, after re-verifying every §1–§4
citation against `master`. Four citations were wrong; two of the
corrections changed an answer. Summary:

1. **Extend `confine_peak_history`** — plus a `kind` discriminator column
   and nullable `budget`/`budget_basis`. `kind` is decisive, not polish:
   `ConfinePeakP90` scans the table unfiltered and feeds LIVE admission, so
   unnamespaced aitest rows would silently drag the machine-wide prior. The
   budget columns are required because the granted reserve for a confine job
   is persisted **nowhere** today — so §3.1's "no new capture needed" was
   false, and the over-provisioned direction (this ticket's own headline
   evidence) was unevaluable.
2. **`resource-budget`, on TWO faces over one pure classifier** —
   project-scoped `aira insights show resource-budget`, and a project-LESS
   management form on `aira confine`. One home was the wrong shape:
   `insights` answers `E_CONFIG_MISSING` outside an `aira init` project, and
   AIRA-127 already made this exact call for `aira top` ("most of the
   directories an operator watching the slice is standing in"). All four
   reporting sessions were in such directories.
3. **Durable `.aira/config` aitest knob DEFERRED** — it is a write-path
   change under a report-only plan; the Python side is env-var-only with no
   config reader; and §3.4 proposed the wrong section (`run.` governs
   `aira run`).
4. **Reuse `marginBucket`'s existing boundaries, mint no constant** —
   `<1.0` under, `[1.0,1.25)` well-fitted (contains the estimator's own
   1.15), `[1.25,2.0)` a deliberate quiet band, `>=2.0` over; any OOM on
   record ⇒ under regardless.
5. **No persistence threshold** — a streak counter is new durable state for
   an advisory signal. Reuse the existing ≥3-usable-samples gate; noise is
   already asymmetric in the safe direction because the comparison is
   against MAX of a 20-sample window. One bypass: a recorded OOM classifies
   on the first occurrence.

Also deferred with reasons recorded: the `aira check` dimension (signatures
are cross-project — `make test` in two repos is one row set — and `check`'s
warnings are project evidence).

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

## Follow-up ground truth (wt, 2026-09-08) — whole-job peak vs. the outer reservation

`FASTEST_XDIST_WORKERS=3 aira confine --memory-max 16G -- make test-engine`
completed with `peak-rss=11256984K` (≈10.7 GiB) at 3 xdist workers; a
4-worker attempt at `--memory-max 10G` was OOM-killed at 51% before
reaching whole-job peak. Flagged as directly relevant to this ticket's
per-worker capture design: the whole-job peak (10.7 GiB at 3 workers) is
far above the 3 GiB `AIRA_AITEST_ESTIMATED_BYTES` outer reservation this
repo's own `scripts/aitest_engine_estimated_bytes.sh` pins for the same
suite — a >3x gap. These measure different things (a per-scope admission
estimate vs. whole-job RSS), so this is not itself a bug, but is exactly
the kind of aggregate-vs-per-worker discrepancy §3.1's pool-granularity
capture (not per-test) is designed to make legible rather than leave
implicit. Reporter's own caveat: box was heavily contended throughout
(42 confined processes at points), so treat as real but noisy, not a lab
figure — directionally useful for scoping the capture design, not a
precise calibration input.

## Review round 2 (Astra + Fable, 2026-09-09) — four blocking findings resolved

Both passes BLOCKED. All four accepted and resolved in the plan's new **§5s**;
none needed a redesign.

1. **Astra — pool capture was scoped to the CRASH path only.** §5r.7.3 put the
   read beside `_describe_worker_death`, whose only caller is
   `_handle_worker_exit`, which is reached only when a worker crashes. The two
   ordinary retirement paths (recycle, end-of-run stop) never reach it, so a
   clean run — the only kind that can show over-provisioning — would have
   recorded nothing. Capture moved into `_retire_worker`, the single point every
   retirement funnels through, beside its own `grant` fetch and before
   `_forget_worker_scope`'s rmdir.
2. **Fable — a per-ROW budget was being classified as if it were one budget.**
   Every row is now bucketed individually against the CURRENT budget (the newest
   budgeted row, or Face 2's pre-flight request); the OOM bypass is scoped to
   OOM rows whose own budget is at or above the current one; OOM peaks, which
   are truncated lower bounds, never feed the lowering direction; null-budget
   rows get their own counted exclusion.
3. **Fable — "budget" conflated the granted reserve with the scope
   `memory.max`.** One quantity per row, with the family named in
   `budget_basis` (`cap:` for a kernel-enforced bound, `reserve:` for a ledger
   booking), and the classifier partitions by family so the two are never
   summarised together.
4. **Fable — tests were not named per invariant.** Twelve named, each with the
   direction it guards; see §5s.5.

Also settled: one pool sample per RUN rather than per retirement (a relay fork
on the dispatch loop's hot path would have been the cost of the correct call
site), and the aitest pool key defined — the pytest rootdir plus invocation
arguments, unit-separator joined, which unlike an argv-only confine signature
does not collide across projects.

## Status

Built on `aira180-resource-budget-tracking`. Report-only throughout, and that
constraint is asserted as a SQLite write count on BOTH faces rather than left
to review.

## Review (Fable final build-review gate, 2026-09-09) — MERGE after two in-review fixes

PR #121 merged as `967256b` (branch head `cf07956` = build `f13d97a` +
one review-fix commit). Ticket driven planned → in-progress → in-review →
done via `aira mv`.

**Independently re-derived from the diff, not taken from the build's notes:**

- Report/recommend-only: no write path anywhere in `ClassifyResourceBudget`,
  `computeResourceBudget`, or the daemon's `confineBudget`; the only writes in
  the whole diff are sample capture (`RecordConfinePeak`) and the guarded
  schema migration. Asserted on both faces as SQLite `total_changes()` on the
  store's single pinned connection (`SetMaxOpenConns(1)` verified).
- `aira_mem` boundary: zero mentions in the diff; recommendations name only
  `--memory-reserve` / `--memory-max` / `AIRA_AITEST_ESTIMATED_BYTES`;
  `TestSkillNamesNothingFromTheRetiredXdistGovernor` PASS (verbose, exit 0).
- Pool capture on every retirement: `_retire_worker` has exactly three
  callers on the branch — recycle (`:2040`), crash `_handle_worker_exit`
  (`:2209`), end-of-run stop broadcast (`:2470`) — and folds before
  `_forget_worker_scope`'s rmdir. The two out-of-band `_forget_worker_scope`
  calls (`:983`, `:1373`) are placement failures for workers that ran no
  test. One sample per run, emitted after the loop.
- cap:/reserve: families never summarised together: the classifier
  partitions on the current basis's family and counts the rest as
  `basis_family_mismatch`; `usable`, `observedMax`, `cleanStats`, `allStats`
  accumulate from the current family only. `ConfineBudgetTerm` records one
  quantity per row with its family named.
- Both faces are real: `confine-budget` is a dispatch-table verb (MCP tool
  `aira_confine_budget`, SafetyRead, routed client-side, project-less in
  `mcp_project.go`/`scope_dir.go`, served in `server.go`, CLI `--budget`
  management arm); `resource-budget` is an `insightRegistry` row with a
  `Compute`. Manifest 74→75, tool list and dispatch-name goldens updated.
- Schema: `kind`/`budget`/`budget_basis` via the guarded `ensureColumnAdded`
  pattern, `(kind, signature)` index created after the columns, all four
  readers and the retention DELETE filter on `kind`; migration test covers
  the legacy shape and the AIRA-97 second-opener no-op.

**Two findings survived, both fixed before merge (commit `cf07956`):**

1. **Porous load-bearing test** — `TestConfinePeakP90IgnoresNonConfineKinds`
   passed against a mutant with the `kind` filter removed from
   `ConfinePeakP90` (reviewer-run mutation: exit 0). Nearest-rank p90 over 12
   maxima `[1,1,100..1000]` still indexes 900. Hardened to 30 distinct pool
   signatures (unfiltered answer 600); mutant now fails on the p90-moved
   assertion (before=900 after=600, exit 1). Production code unchanged.
2. **Wrong advice on the common suite launch shape** — a
   `cap:auto:delegate-ram` row (ceiling daemon-derived, floor 4G; slice
   booking = pinned overhead) read `over-provisioned, consider
   --memory-reserve N` for any delegate suite peaking under 2G, and that knob
   on a delegate-ram job sets the framework overhead, not the ceiling. Now
   `unevaluated` / `delegate-ram:ceiling-not-a-budget` with evidence still
   published; OOM at the ceiling still classifies under-provisioned and names
   `--memory-max`. `TestResourceBudgetDelegateRAMCeilingIsNotABudget`.

Plus: Face 2's human render now prints each unevaluated subject's reason
(was `--json`-only) and has its first tests; plan §5s.2 separator wording
corrected; §5t written.

**Verification (review worktree, exact exit codes):** `make fmt-check` 0,
`make vet` 0, `make build` 0, `AIRA_REAL_PYTEST=1 make test` exit 0
(pytest harness hard-fail mode, real pytest 9.0.3 — CI's runner has no pytest
and SKIPS these, so CI green did not cover the 8 new Python tests; the local
run does), targeted 16 AIRA-180 pytest tests 16 passed. CI on the merged head:
build+vet+gofmt pass, test pass, race pass on `cf07956` (CI run 34291624126); merge state CLEAN.

**Accepted gaps, written down:**

- The Python tests prove the fold by calling `_retire_worker` directly for
  three workers, not by driving the recycle/crash/stop paths end-to-end; the
  funnel property is a source-level fact and was verified that way.
- `ResourceBudgetRequest` (the pre-flight budget override) is implemented and
  unit-tested in the classifier but not reachable from either face in v1
  (confine-budget takes no selector by design, §5s.6); Face 2 answers "was
  the last run sized right", not yet "if I ask for N now".
- An older non-delegate `cap:` row and a newer delegate-ram row share a
  family; the per-row buckets stay honest, and the headline is only ever
  taken against the newest basis, so no wrong advice results — but a subject
  launched both ways is a mixed population and reads as such.
- Startup-window/aggregation caveat inherited from AIRA-186: MAX-based, tail
  sensitive; `UsableSamples`/OOM counts are published beside every verdict
  rather than smoothed.
