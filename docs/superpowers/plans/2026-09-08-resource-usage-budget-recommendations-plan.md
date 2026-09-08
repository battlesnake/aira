# Resource usage vs budget tracking + recommendations, for tasks and test groups

Status: **plan — not yet gated, not yet built.** Owner asked for a design,
not a build, matching tonight's AIRA-176 treatment.

## 0. Problem, stated precisely

Owner (2026-09-08): "I see a lot of sessions are setting explicit RAM
reservations for tests and then having to adjust them either due to OOMs or
lack of admission — instead of being able to just use aitest, let aira
govern things, and have memory usage annotations on tests where needed."
Refined, after discussion, to the authoritative scope: **"aira should track
real resource usage for tasks and groups of tests vs budget, and make
recommendations to agents in projects about adding or adjusting quotas for
tests and/or tasks."**

Three hard constraints from that framing:

1. **Report/recommend only — never auto-adjust.** No code path in this
   design writes a quota on an agent's behalf.
2. **Both granularities.** Individual tasks (an `aira run` / `aira confine`
   invocation) *and* groups of tests (an aitest worker pool run) need
   usage-vs-budget tracking, not just one.
3. **Over time, against budget** — not a single-run report. The comparison
   is actual measured usage accumulated across runs vs. whatever quota is
   currently configured for that subject.

This is not a hypothetical problem. Tonight alone, three independent peer
sessions (qual, subpipe, split) reported the caller-pinned-reserve and
admission-starvation pain this feature targets, and AIRA-160 already
verified a real, narrower memory-blindness gap in aitest's worker sizing.
Fresh, concrete evidence from tonight (subpipe, 2026-09-08): six confined
runs pinned at `--memory-reserve 8G` peaked at 52 MiB–841 MiB actual RSS —
roughly 156x over-reservation on the worst run, each one holding headroom
eleven other waiters needed on a saturated slice.

## 1. What already exists to build on (verified from source)

- **The outer-scope estimator is a proven, reusable precedent.**
  `confine_peak_history(signature, peak_rss, oom, at)`
  (`internal/store/confine_peak_history.go:12-48`) keeps the newest 20
  samples per `runner.ResourceSignature` (`internal/runner/resource_estimate.go:69-79`,
  command-signature-agnostic — go/make/cargo/semgrep alike), pruned on
  insert. `EstimateMemoryReserve` (`resource_estimate.go:83-124`) derives a
  reserve from that history with a 1.15 safety factor, expressed on the wire
  as `reserve-basis=estimate:max=N,n=S,f=115`. **This half of the problem —
  per-task usage history vs. a derived budget — already exists and is
  already load-bearing for admission.** What is missing is a *report*
  surface over it: nothing today tells an operator "signature X has been
  granted a 4G reserve but has peaked at 200M across its last 18 runs".

- **aitest has none of this.** Worker admission
  (`internal/daemon/worker_admit.go`) grants a fixed per-worker
  `memory.max` from the single static env var `AIRA_AITEST_ESTIMATED_BYTES`
  (`internal/pylib/aitest/__init__.py:231-283`, `_resolve_estimated_bytes`;
  512 MiB default, clamped [1 MiB, 1 PiB], no pytest ini option exists to
  hold it — `pytest_addoption` registers only `--aitest-workers`). No
  per-suite/signature sizing exists; it is explicitly named as deferred in
  `docs/superpowers/specs/2026-09-01-aitest-design.md:238`.
  `workerAdmitRequest.signature` is accepted on the wire and **unused**
  (`worker_admit.go:90-97`). Nothing records a worker's actual peak RSS
  anywhere durable: `supervisor.py`'s `_forget_worker_scope`
  (`~1493-1518`) `rmdir`s each worker's cgroup with no prior read of
  `memory.peak`. The only resource-adjacent signal that *is* captured is
  qualitative — `_describe_worker_death` (`~2044-2084`) distinguishes a
  genuine per-worker OOM (reads that worker's own `memory.events`
  `oom_group_kill`) from an ordinary crash, but throws it away after
  synthesising the unevaluated test report.

- **The `aira_mem()` per-test marker is not a precedent to build on — it is
  a deliberately retired one, and this design must not resurrect it.** Its
  only consumer was the deleted `aira_xdist_governor` (removed by AIRA-33,
  commit `d979517`). `internal/core/skill_test.go:584-610`
  (`TestSkillNamesNothingFromTheRetiredXdistGovernor`) is a build-breaking
  test asserting `"aira_mem("` is **absent** from generated `SKILL.md` and
  the agent guide. `skill.go:318,333` actively teaches the opposite: "A
  delegate-ram job needs no per-test RAM annotation of any kind." Any new
  design that reintroduces mandatory per-test manual sizing would directly
  regress a tested, deliberate architectural position. This design stays
  advisory and does not touch that boundary — see §3.

- **`test-report` tracks timing history per identity cell, never memory.**
  `test_report_results(project_id, report_id, name, outcome, duration_ns,
  message)` (`internal/store/testreport.go:202-205`; `domain.TestResult`,
  `internal/domain/testreport.go:53-56`). Durations are parsed from JUnit
  and go-test JSON. Headers carry commit/branch/suite/config/env_digest/
  shard/retry_index (`testreport.go:161-189`), so a per-(test, identity
  cell) longitudinal series already exists for duration — the direct
  precedent for what a per-test-group *memory* series should look like —
  but there is no RSS/memory column anywhere in the schema, and aitest does
  not call `test-report add` at all today.

- **No durable, project-scoped home exists for a memory budget below
  `aira run`'s single project-wide scalar.** `.aira/config`'s `RunConfig`
  (`internal/app/project.go:65-75`) already carries `memory_headroom` /
  `memory_estimate`, consumed by `parsedRunAdmission` (`:651-682`) as a
  `pinned:client` reserve origin (`internal/daemon/admit.go:2952-2954`) —
  but it is one scalar for the whole project's `aira run`, not per-signature
  and not reachable by confine or aitest. `.aira/config` decodes with
  `DisallowUnknownFields` (`project.go:530-552`), so adding a new key is a
  small, explicit Go change, not a config-only extension. The only other
  durable expression of a memory budget today is an `export` in a wrapper
  script (this repo's own `.githooks/common.sh:62` hard-codes *no* quota at
  all).

## 2. House style for a report/recommend-only surface (verified precedent)

Three existing surfaces establish the same pattern independently, so a new
one should follow it rather than invent a fourth shape:

- **Diagnostic lines** (`cmd/aira/main.go`, `internal/daemon/sliceceiling.go`):
  observed delta → named implication → an explicit "nothing changed"
  clause, guarded on a positive precondition. `sliceCeilingWouldBeBytes`
  (the counterfactual) and `sliceCeilingReportedEffective` (what admission
  actually uses) are kept as *separate* fields with a comment stating "the
  two must not be conflated" (`sliceceiling.go:803-834`). Observe mode says
  "not applied" explicitly, twice, rather than leaving it implied
  (`main.go:2874,2879`).
- **The AIRA-52 adequacy gauge family** (`internal/store/admission_insight.go`,
  `internal/store/insights.go`): pure classifiers over already-persisted
  evidence, never reading or mutating runner state. `GaugeResult` carries
  `Universe`, `Direction`, `Unevaluated`/`UnevaluatedReason`, and
  `Drilldown{Verb, Query}` — the next command the *operator* runs, never an
  action the code takes. `computeQuotaBurn` (`insights.go:427-487`) is the
  closest existing precedent to a usage-vs-budget feature: per-subject
  used/limit/remaining, each absent term individually unevaluated with its
  own reason.
- **`aira check`'s graded dimensions** (`internal/store/check.go`):
  `Dimensions` starts empty and a dimension exists only because a checker
  established it; `finaliseDimensions` turns anything nothing established
  into `unevaluated` with a named reason. Three grades, three sinks
  (`Findings`/fail, `Warnings`/warning, `UnevaluatedFindings`/unevaluated),
  deduped on (Code, Subject). Never a fabricated pass.

**Synthesised pattern this design follows:** compute purely from persisted
evidence; publish per-subject cells with sample counts and every exclusion
named; `unevaluated` with a specific reason wherever a term is absent
(never a zero, never silently dropped); state the recommended figure as a
counterfactual, structurally separate from whatever is currently applied;
phrase it as observed → implication → explicit "not applied, nothing
changed" clause; offer the next step as a `Drilldown{Verb, Query}` an agent
can choose to run, never an action taken automatically.

## 3. Proposed shape

### 3.1 Two subjects, two different data-collection states

- **Task subject** (an `aira run`/`aira confine` invocation), keyed by the
  existing `runner.ResourceSignature`. **No new capture needed** — the data
  (`confine_peak_history`, 20-sample window, plus whatever reserve was
  granted at admission) already exists. This is purely a new *comparison
  and reporting* layer over data AIRA already keeps.

- **Test-group subject** (an aitest worker pool run), keyed by a *new*
  pool-level signature — **not per individual test**. Rationale, stated
  explicitly because it is the load-bearing scoping call for the plan-gate:
  a worker runs many tests sequentially inside one cgroup with one
  `memory.max`; the budget that is actually granted and actually violated
  is per-worker, not per-test. Tracking true per-test RSS would need
  sampling *inside* each test (new, invasive instrumentation) and would
  reintroduce exactly the per-test annotation burden §1 confirms AIRA
  deliberately removed. Tracking per-worker-pool peak RSS is cheap, matches
  the granularity budgets are actually set at (`AIRA_AITEST_ESTIMATED_BYTES`
  is per-worker), and is the natural reading of the owner's own phrase
  "groups of tests". **New capture required:** at the existing
  `_forget_worker_scope` teardown point (`supervisor.py:~1493`), read
  `memory.peak` before the `rmdir` and record it — reusing
  `RecordConfinePeak`'s exact shape (a new small store table or a
  signature-namespaced extension of `confine_peak_history`, for the
  plan-gate to decide) — plus the OOM/non-OOM verdict
  `_describe_worker_death` already computes but currently discards.

### 3.2 The classifier (store-level, pure, no side effects)

A new pure function alongside `computeQuotaBurn`, e.g.
`classifyResourceBudgetAdequacy(subject)`, producing a `GaugeResult`-shaped
cell per subject:

- `Observed{P90, Max, SampleCount}` from the history (task: existing
  `confine_peak_history`; test-group: the new worker-pool capture above).
- `Budget{Value, Origin}` — whatever is currently granted (task: the live
  reserve/estimate basis; test-group: `AIRA_AITEST_ESTIMATED_BYTES`'s
  resolved value, origin `env:default` or `env:set` since there is no more
  specific origin yet — see §3.4).
- `Direction` — over-provisioned / under-provisioned / well-fitted /
  `Unevaluated` (with a named reason: too few samples, budget unknown,
  signature never seen, etc.) — never a bare number with no evidence
  behind it.
- `Recommendation` — a specific counterfactual figure when, and only when,
  there is enough evidence (mirroring the existing 20-sample window):
  `"signature X: granted reserve 4096M, observed P90 across 18 samples =
  512M (12.5%% utilised) — consider --memory-reserve 768M"`, or for a
  test-group with recorded OOM deaths: `"pool Y: 3 of last 20 worker deaths
  were OOM at the 512M cap — consider AIRA_AITEST_ESTIMATED_BYTES=896M"`.
  This field is a string + a `Drilldown{Verb, Query}`, never a value AIRA
  writes anywhere itself.

### 3.3 Two surfaces over the same classifier

- **A new `aira check` dimension** (e.g. `resource-budget`) for passive,
  routine visibility — an agent already runs `aira check` regularly, so a
  persistently over- or under-provisioned subject surfaces as a `Warning`
  (never a `Finding`/fail — this is advisory, not a defect) the same way
  every other dimension does, with `unevaluated` where history is thin.
- **A query verb** (naming deferred to the plan-gate — most likely `aira
  insights resource-budget [selector]`, alongside the existing insights
  family) for on-demand drilldown when an agent wants the specific number
  before choosing a reserve, rather than after the fact. This is the direct
  answer to tonight's convergent peer pain (qual: "no way to ask what a
  request needs to be in order to be admitted"; subpipe/wt/split: caller
  guesses a reserve, then adjusts after an OOM or a stall) — the
  recommendation is available *before* a caller pins a number, not only in
  retrospect.

### 3.4 Reconciling with the `aira_mem` boundary (explicit, load-bearing)

The default path — no annotation, no manual sizing — must keep working
exactly as `skill.go` currently teaches. This design adds a *second*,
opt-in path: an agent that reads a recommendation may act on it through an
**existing** durable knob (`--memory-reserve`/`--memory-max` on confine,
`AIRA_AITEST_ESTIMATED_BYTES` in a wrapper) — never a new per-test marker.
The one gap worth naming for the plan-gate: `AIRA_AITEST_ESTIMATED_BYTES`
has no durable, project-scoped home today (§1) — a caller wanting to act on
a recommendation durably has only a wrapper-script `export` to put it in.
Whether to add a project-scoped `aitest.memory_estimate_bytes` to
`.aira/config`'s `RunConfig` (small, explicit, `DisallowUnknownFields`-safe
Go change) as part of this feature, or leave "set the env var" as the v1
answer and defer the config surface, is left open for the plan-gate —
matching how AIRA-176 left its own scoping calls to the gate rather than
settling them here.

## 4. MCP / Skill integration

Standard dispatch-table path, same as confirmed for AIRA-176: the new
`aira check` dimension needs no new verb (already dispatched through
`aira check`'s existing CLI/MCP/Skill faces). The new query verb, if built
as its own subcommand, gets the standard one-registration treatment — a
generated CLI subcommand skeleton, a generated MCP tool schema, and a
golden-tested Skill/guide mention — with only the CLI `parseArgs`/
`buildRequest` arm and the contextual "when/why" prose in
`renderMarkdownBody` hand-written.

## 5. Open questions for the plan-gate (deliberately not settled here)

1. New table vs. extending `confine_peak_history` with a subject-kind
   discriminator, for the test-group capture.
2. Exact verb name and whether it lives under `aira insights` or is its own
   top-level verb.
3. Whether `.aira/config`'s durable `aitest.memory_estimate_bytes` ships in
   v1 or is deferred (§3.4).
4. Threshold/shape for "well-fitted" vs. "over/under-provisioned" —
   probably a configurable band around the observed P90 similar to the
   existing 1.15 safety factor, but the exact multiple is a gate decision,
   not a design one.
5. Whether a `Warning` in `aira check` should fire on every run once a
   subject is classified over/under-provisioned, or only above a
   persistence threshold (N consecutive runs) to avoid noise on a single
   atypical run.

## 6. Status

Planning only. Not yet gated, not yet built, per the owner's explicit
request.
