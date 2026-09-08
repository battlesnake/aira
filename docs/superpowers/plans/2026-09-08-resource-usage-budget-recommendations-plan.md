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

## 5r. §5 resolved — gate decisions (2026-09-09)

All five open questions are decided below. Every citation in §1–§4 was
re-verified against `master` before deciding; four of them were wrong or
incomplete, and two of the corrections are load-bearing enough that they
change the answers. The corrections come first, because Decision 1 exists
only because of correction (a).

### 5r.0 Corrections to §1–§4 (re-verified from source)

**(a) §3.1's "no new capture needed" is FALSE for `aira confine` — and
confine is where the reported pain is.** The confine-report wire frame
carries only `{signature, oom, peak_rss}`
(`internal/runner/admission_linux.go:794-797` →
`internal/daemon/confine_report.go:11-45` → `RecordConfinePeak`). The
reserve a confine job was actually *granted* is persisted nowhere; the
source says so in as many words at
`internal/runner/confine_shim_linux.go:471-476` ("the confine-report wire
frame carries only signature/oom/peak_rss and the runs projection has no
provenance column"). The claim is true only of `aira run`, whose ledger
projection does carry `admission_reserve` and `admission_reserve_basis`
(`internal/runner/ledger.go:640-651`). Consequence: with today's schema the
**over-provisioned direction is unevaluable for confine** — the feature
could not reproduce its own headline evidence (subpipe's
`--memory-reserve 8G` against a 52 MiB–841 MiB actual). That is what
Decision 1 fixes.

**(b) §3.2's `Observed{P90, …}` does not exist per subject.**
`ConfinePeakHistory` returns aggregates only — total count, usable-sample
count, **MAX** peak, OOM count, max-OOM peak
(`internal/store/confine_peak_history.go:52-69`). `ConfinePeakP90`
(`:71-102`) is a *different quantity*: the cross-signature p90 of
per-signature maxima, and it is read live by admission as the machine-wide
prior for a job with no history of its own
(`internal/daemon/admit.go:1854`, `cachedAdmitPeakP90` at `:1954`). It is
not this subject's p90 and must not be borrowed as one. Decision 4 resolves
this by classifying against MAX — the same statistic `EstimateMemoryReserve`
itself uses (`internal/runner/resource_estimate.go:94-116`) — so a
recommendation and the automatic estimate can never be talking about
different numbers.

**(c) §1/§3.4 mis-describe the `.aira/config` precedent.** `RunConfig`
(`internal/app/project.go:65-75`) carries `memory_headroom` as a byte-count
string but `memory_estimate` as a **bool** — the switch that enables the
estimator, not a value. And `RunConfig` is the `run.` section governing
`aira run` specifically (`Config` sections are schema/project/lease/run/git,
`project.go:25-31`); an aitest knob does not belong under it. Feeds
Decision 3.

**(d) §3.1 names the wrong capture point in `supervisor.py`.**
`_forget_worker_scope` (`:1493-1518`) receives only the scope *path* — not
the grant, therefore not the worker's `memory_max` (the budget term) and not
the pool key. The grant is in hand one frame up, in `_retire_worker`
(`:1487`), and the supervisor already has a precedent for exactly this
ordering: `_describe_worker_death` is called BEFORE `_retire_worker`
(`:2010-2013`) for the stated reason that "once it is gone the evidence for
WHY this worker died is gone with it". Capture belongs beside that call, not
inside the rmdir.

**(e) Unrecorded in §3.1: there is no back-channel to report a pool peak.**
The supervisor's only daemon transport is the per-worker `aira worker-admit`
relay subprocess. A pool-peak report needs a new report path — the analogue
of the `confine-report` verb — and v1 must scope that explicitly rather than
discover it during the build.

**(f) Q1 is not fully open: the authoritative aitest spec already answers
its direction.** `docs/superpowers/specs/2026-09-01-aitest-design.md:232-236`
already prescribes per-suite sizing by "reuse of
`internal/store/confine_peak_history.go`'s existing table" under a signature
`pytest-worker:<suite-hash>`. Per CLAUDE.md ("read the applicable design
spec in full; do not infer a missing decision from an implementation
convenience"), the gate's job here is to confirm that decision against
current source and name what it does not cover — not to re-open it.

### 5r.1 Decision 1 — extend `confine_peak_history`, with a `kind`
discriminator and budget columns

**Decided: extend the existing table. Add three columns:**

```sql
ALTER TABLE confine_peak_history ADD COLUMN kind TEXT NOT NULL DEFAULT 'confine';
ALTER TABLE confine_peak_history ADD COLUMN budget INTEGER;       -- nullable
ALTER TABLE confine_peak_history ADD COLUMN budget_basis TEXT;    -- nullable
```

with the index rebuilt on `(kind, signature)` and every existing reader
filtered on `kind`.

Why one table, not two:

- The authoritative aitest spec already prescribes reuse (correction (f)).
- The two subjects have byte-identical shape — subject key, observed peak,
  OOM flag, timestamp — and now the same budget pair.
- The 20-newest-per-key retention (`confinePeakHistoryLimit`,
  `confine_peak_history.go:12,43-48`) is a correctness-critical invariant
  with tests behind it. A second table duplicates it, and duplicated
  retention invariants drift.
- `EstimateMemoryReserve`/`ConfinePeakHistory` already read this shape; a
  second table means a parallel reader the classifier has to union anyway.

Why `kind` is **not optional polish — it is the decisive term.**
`ConfinePeakP90` scans the table with **no per-signature filter at all**
(`WHERE peak_rss>0 GROUP BY signature HAVING COUNT(peak_rss)>=3`,
`:77-79`) and its result feeds **live admission** for every job with no
history of its own (`admit.go:1854`). Dropping aitest worker rows —
hundreds of small 512 MiB-capped workers — into that table without a filter
would silently drag the machine-wide admission prior down. That is a
behaviour change to the live admission path, arriving through a
report-only feature, invisible to every test that does not already assert
on the prior. A `kind` **column** rather than a `pytest-worker:` string
prefix because a prefix is a convention a single-element argv can in
principle collide with (a `ResourceSignature` for a one-element argv
contains no NUL separator, `resource_estimate.go:69-79`), whereas a column
cannot collide: illegal-unrepresentable, the project's stated
data-model-first review bias.

Why the budget columns: without them correction (a) stands and the
over-provisioned direction — the reported pain — is unevaluable. `budget`
is the reserve/`memory.max` actually granted for that sample; `budget_basis`
its provenance in the existing `family:name[:params]` grammar
(`pinned:client`, `estimate:max=…`, `env:default`, `env:set`). Both
nullable, because a sample whose budget could not be established must read
`unevaluated`, never a fabricated zero — the same discipline `peak_rss`
already has.

The migration is the established idempotent `tableHasColumn`-guarded
ADD COLUMN pattern already used for exactly this shape at
`internal/store/store.go:1239`
(`ALTER TABLE outbox ADD COLUMN kind TEXT NOT NULL DEFAULT 'ticket-file'`).
`DEFAULT 'confine'` backfills every existing row correctly by construction:
they *are* confine rows.

**This forbids:** any reader of `confine_peak_history` that does not name a
`kind`. The build must audit all four existing call sites
(`RecordConfinePeak`, `ConfinePeakHistory`, `ConfinePeakP90`, and the
retention DELETE) and add a regression test that a `pytest-worker` row
cannot move `ConfinePeakP90`'s answer.

### 5r.2 Decision 2 — `resource-budget`, on **two** faces over one
classifier: project-scoped `aira insights`, project-less `aira confine`

**Decided.** Gauge/classifier name: `resource-budget` — kebab, no verb,
matching the existing gauge-name grammar (`admission-reserve-adequacy`,
`quota-burn`, `flaky-rate`, `insights.go:91-100`).

- **Face 1 — `aira insights show resource-budget`** (project-scoped). One
  row in `insightRegistry` plus one `Compute` func. No new verb, no new CLI
  `parseArgs` arm, no new MCP tool (it reaches MCP through the existing
  `aira_insights` tool's `name` argument), no new Skill registration beyond
  appearing in `insights ls`. This is the cheapest possible addition
  consistent with the dispatch-table rule.
- **Face 2 — a new management form on `aira confine`** (project-**less**),
  answered through the daemon exactly as `--list` already is. This is the
  pre-flight answer: "for this command, what does history say, and what
  would be granted".

§5's question assumed one home. One is wrong, and this is the gate's most
substantive change to the plan. `aira insights` is part of the coordination
surface, which by design returns `E_CONFIG_MISSING` outside a project the
user has run `aira init` in (`internal/core/skill.go:336`). But half this
gauge's subject — `aira confine` — is deliberately project-less, and AIRA
has already made and written down exactly this call once: `aira top` is
dispatched *before* project discovery because "confine state is machine-wide
and `confine --list` needs none. Routing it through the scope resolution
below would make a machine-wide monitor refuse to start outside an AIRA
project — **which is most of the directories an operator watching the slice
is standing in**" (`cmd/aira/main.go:167-172`). qual, subpipe, split and wt
were all standing in exactly such directories. An insights-only v1 would
answer `E_CONFIG_MISSING` to the four sessions whose pain motivated the
ticket. (This objection was independently raised by a DeepSeek-pro
adversarial pass on the draft decisions and is accepted.)

Two faces is not complexity stacking: it is the project's stated
architecture — "the core is one downward-layered implementation behind
`core.Do`; CLI, MCP, Skill, daemon, and TUI are thin faces" (CLAUDE.md) —
and it is what §3.3 already proposed. The **classifier is one pure function**
in `internal/store` beside `classifyAdmissionAdequacy`, and the faces hold
no logic.

**Honesty requirement on Face 1.** This is the first gauge to report
**machine-wide** data behind a project-scoped verb. Existing gauges are
per-repo: `admission-reserve-adequacy` reads the common-dir `runs.db`
(`admission_insight.go:239`, `estimate_actual.go:34`), and the common dir is
the git common dir. `confine_peak_history` lives in the single machine-wide
`state.db` and has no `project_id`. `GaugeUniverse.Scope` must say so
verbatim, and so must the cross-project limitation in 5r.6.

### 5r.3 Decision 3 — the durable `.aira/config` aitest knob is DEFERRED

**Decided: not in v1.** Four reasons, in descending weight:

1. **It is a write-path change wearing a read-path plan's clothes.** This
   feature's first hard constraint is report/recommend-only. A durable
   config key that admission reads is a new *admission input*, and shipping
   it under this plan would put a behaviour change through a gate that
   reviewed a reporting change.
2. **The Python side cannot read it.** `internal/pylib/aitest` is env-var
   only — verified: the only knobs are `AIRA_AITEST_ESTIMATED_BYTES`
   (`__init__.py:231-283`), `AIRA_AITEST_MAX_WORKERS_FALLBACK`,
   `AIRA_AITEST_BOOTSTRAP_CMD`, `AIRA_AITEST_WORKER_ADMIT_CMD`. There is no
   `.aira/config` reader anywhere in the package. v1 would have to mint a
   new Go→Python configuration channel — unscoped work with its own failure
   modes, for a knob nothing yet recommends.
3. **§3.4 proposed the wrong home anyway** (correction (c)): `RunConfig` is
   the `run.` section for `aira run`. An aitest knob would need its own
   `aitest` section on `Config`, which is a larger, separately reviewable
   surface decision.
4. **Recommending into a knob that does not exist is worse than
   recommending into one that does.** The existing knobs
   (`--memory-reserve`/`--memory-max`, `AIRA_AITEST_ESTIMATED_BYTES`) are
   actionable today. v1's recommendation text names those.

Recorded consequence, to be stated in the gauge rather than left implied:
until that ticket lands, an aitest subject's `Budget.Origin` can only be
`env:set` or `env:default`, and there is no project-scoped place to put the
answer — a caller acting durably still has only a wrapper-script `export`.

### 5r.4 Decision 4 — reuse `marginBucket`'s boundaries; mint no new constant

**Decided.** The band is the one the codebase already has, applied to
`budget / observed-MAX`:

| ratio | classification | recommendation |
| --- | --- | --- |
| any OOM on record for the subject | **under-provisioned** | raise, regardless of ratio |
| `< 1.0` (shortfall) | **under-provisioned** | raise |
| `[1.0, 1.25)` | **well-fitted** | none |
| `[1.25, 2.0)` | **acceptable** | none |
| `>= 2.0` | **over-provisioned** | lower |

Why this exactly:

- `marginBucket` (`internal/store/admission_insight.go:218-236`) already
  defines these four boundaries, is already the tested vocabulary for
  reserve-vs-peak, and computes them by **exact big.Int
  cross-multiplication** specifically so no float64 rounding can misclassify
  a value at 1.25 or 2.0 (`:218-222`). Reusing it inherits that correctness
  and mints zero constants — the same "no new constant enters the codebase"
  discipline `SliceFittedReserve` states for itself
  (`resource_estimate.go:17-35`).
- `[1.0, 1.25)` comfortably contains the estimator's own 1.15 safety factor
  (`memoryEstimateSafetyPct`, `resource_estimate.go:10-13`), so a subject
  sized by AIRA's own estimator classifies as well-fitted by construction —
  the feature never recommends against the estimator's own output.
- `[1.25, 2.0)` is deliberately classified but **not recommended on**. A
  dead band matters here: without it, every subject is permanently either
  too big or too small and the surface becomes noise. 2.0 is where subpipe's
  156x and AIRA-186's ~35.7 GiB-vs-~22 GiB both sit far outside, so the band
  is wide enough to be quiet and still catch every reported case.
- Against MAX, not a p90 — see correction (b). Recorded limitation, from
  AIRA-186: a MAX-based comparison inherits MAX's tail sensitivity (a 31 GiB
  outlier in a 20-sample window drives a 35.7 GiB estimate). The classifier
  therefore publishes `SampleCount` and the OOM count beside every
  classification so the operator can see whether a single tail sample is
  driving it; it does not silently smooth the tail away, because the live
  estimator does not either and the two must not disagree.

### 5r.5 Decision 5 — no persistence threshold; reuse the existing ≥3-sample
gate, with an OOM bypass

**Decided: no N-consecutive-runs streak counter.**

- A streak counter is **new durable state** — a per-subject counter that
  must be persisted, pruned, invalidated when the budget changes, and
  reconstructed after a daemon restart. That is precisely the machinery the
  owner's architectural-simplicity rule says not to stack for an advisory
  signal.
- The evidence gate already exists and is already the codebase's answer to
  "too little evidence": `memoryEstimateMinSamples = 3`
  (`resource_estimate.go:11`), mirrored by `ConfinePeakP90`'s
  `HAVING COUNT(peak_rss)>=3`. Below three usable samples the subject is
  `unevaluated` with the existing reason shape
  (`fallback:insufficient-samples:n=N`), never a Warning.
- Single-run noise is **already asymmetric in the safe direction**, because
  the comparison is against MAX of a 20-sample window (Decision 4). One
  atypically *low* run cannot move MAX at all, so it can never manufacture a
  spurious "over-provisioned, lower it" — the only direction where a wrong
  recommendation causes an OOM. One atypically *high* run moves MAX up,
  i.e. toward "raise it" — the direction whose failure mode is wasted
  headroom, not a dead job. A streak counter would buy nothing the window
  and the ≥3 gate do not already buy.

**One bypass, accepted from the DeepSeek-pro pass:** a recorded OOM
classifies under-provisioned **on the first occurrence**, bypassing the ≥3
gate. An OOM is not a noisy sample — it is realised harm, it is already
recorded (`confine_peak_history.oom`; and, on the aitest side,
`_describe_worker_death`'s `oom_group_kill` read, `supervisor.py:2044-2084`),
and the estimator already escalates on it unconditionally
(`resource_estimate.go:107-111`). Waiting for a third OOM before saying so
would be the feature withholding the one thing it is certain about.

### 5r.6 What v1 does NOT ship, and why (written down, not silent)

- **The `aira check` `resource-budget` dimension (§3.3) is deferred.**
  `checkDimensions` (`internal/store/check.go:44-49`) is the
  *project-consistency* vocabulary, and `check`'s Warnings are consumed as
  project evidence. `confine_peak_history` has no `project_id`, and
  `ResourceSignature` is argv-only with no cwd term
  (`resource_estimate.go:69-79`) — so `make test` in repo A and repo B are
  **one row set**. A machine-wide, cross-project advisory landing in a
  project's `aira check` would let one repo's command warn in another's.
  Defer until the data is project-attributable; the two query faces cover
  the reported pain without it.
- **The project-less pre-flight verb is Face 2 on `confine`, not a new
  top-level verb.** A new top-level verb would need a CLI arm, an MCP
  schema, and a Skill registration for a question `confine` is already the
  home of.
- **Per-test capture stays out**, unchanged from §3.1/§3.4 — and the build
  must not regress `TestSkillNamesNothingFromTheRetiredXdistGovernor`.
- **Named limitation the gauge must state, not imply:** signatures are
  machine-wide and cross-project (above), and `Universe.Scope` must say so.
  This is a pre-existing property — the live estimator already sizes
  admission from the same collided history — so it is a disclosure
  requirement, not a defect this feature introduces.

### 5r.7 Build scope implied by these decisions

1. Schema: three guarded ADD COLUMNs + `(kind, signature)` index; all four
   existing readers filtered on `kind`; regression test that a
   `pytest-worker` row cannot move `ConfinePeakP90`.
2. Capture, confine side: carry `budget`/`budget_basis` on the
   `confine-report` frame (the two fields correction (a) shows are missing)
   and persist them.
3. Capture, aitest side: read `memory.peak` beside the existing
   `_describe_worker_death` call in `_handle_worker_death`'s ordering — grant
   in hand — and add the pool-peak report path correction (e) names.
4. One pure classifier in `internal/store`, `GaugeResult`-shaped, reusing
   `marginBucket`.
5. Two thin faces: `insightRegistry` row; `confine` management form.

## 5s. Plan-gate round 2 — Astra + Fable blocking findings, resolved

Two orthogonal review passes ran against 5r. Both raised blocking findings;
all four are accepted and resolved below. Every one is a correction to 5r,
not a redesign, and each is re-verified against `master` at `c7e4e7b`.

### 5s.1 Astra — pool capture was scoped to the CRASH path only (ACCEPTED)

5r.7.3 said "read `memory.peak` beside the existing `_describe_worker_death`
call in `_handle_worker_exit`'s ordering". Re-verified: `_describe_worker_death`
has exactly one caller, `_handle_worker_exit` (`supervisor.py:2012`), and
`_handle_worker_exit` is reached **only** when a worker stops reporting
without a terminating record — a crash. The two ORDINARY retirement paths,
recycling after a completed test (`:1844`) and the end-of-run `__stop__`
broadcast (`:2293`), never reach it. A clean run with zero worker crashes —
the common case, and precisely the case needed to observe over-provisioning —
would record nothing at all, leaving a sample set skewed entirely toward
crashes, which can only ever look under-provisioned.

**Resolved: capture moves into `_retire_worker`**, beside its own existing
`grant = state.get("grant")` fetch (`:1485`) and before
`_forget_worker_scope`'s `os.rmdir`. That is the single point every
retirement funnels through — all three call sites — and the grant (hence
`memory_max`, the budget term) is already in hand there.

### 5s.2 Aggregation: one report per RUN, not one per retirement

Following directly from 5s.1, and settling correction (e)'s "new report
path" concretely. `_retire_worker` fires on every recycle, so a report
subprocess per retirement would put a fork on the dispatch loop's hot path.
Instead `_retire_worker` folds each worker's `memory.peak` and its own
`memory.events` `oom_group_kill` into a per-run accumulator (max peak, any-OOM,
the granted per-worker `memory.max`), and `run()` emits **one** pool sample
after the dispatch loop through a new CLI verb `aira worker-peak`, invoked
with the binary already named by `AIRA_AITEST_WORKER_ADMIT_CMD`. One
subprocess per suite run, not per worker.

This is also the honest granularity. The budget is per-worker
(`AIRA_AITEST_ESTIMATED_BYTES` sizes each worker's `memory.max`), so the
like-for-like observation is the largest peak any worker in the pool reached,
compared against that per-worker cap. One sample per run matches the confine
side exactly, so the shared ≥3-sample gate means the same thing on both.

Fail-open throughout: an unreadable `memory.peak`, an absent grant, a missing
relay binary or a non-zero relay exit are all silent no-ops on the suite. A
sample is never fabricated — an unreadable peak records nothing rather than a
zero.

**Pool key defined** (Fable's third non-blocking point). The aitest subject
key is the pytest **rootdir** followed by the invocation arguments, NUL-joined
— the same grammar `runner.ResourceSignature` uses for argv. Unlike a confine
signature, which is argv-only and therefore collides across projects (5r.6), a
key that leads with the rootdir does **not** collide across projects; the gauge
states both facts rather than implying either. `pytest-worker:<suite-hash>`
from the aitest spec is not used: the `kind` column already namespaces these
rows (Decision 1), so a prefix would be a second, redundant namespacing
mechanism, and an opaque hash would make the subject unreadable on the
operator surface for no gain.

### 5s.3 Fable blocking 1 — per-ROW budget, classified as if it were one
budget (ACCEPTED)

Decision 1 persists `budget` per row, but Decision 4 computed a single
`budget / MAX(window)` and Decision 5 said "any OOM on record ⇒ under,
regardless". `classifyAdmissionAdequacy` buckets per sample
(`admission_insight.go:38-50`) precisely so this cannot happen. Fable's
counterexample is the ticket's own qual sequence: 40G → 30G (OOM) → 40G. The
30G OOM row sits in the 20-row window for up to 20 more runs and the surface
says "under-provisioned, raise" for all of them — after the caller already
raised.

**Resolved, with no new durable state:**

- **Current budget** is the newest sample carrying a non-NULL budget (Face 2
  may override it with the budget a caller is about to request — the
  pre-flight question). If no sample carries one, the subject is
  `unevaluated`, never zero.
- **Every row is bucketed individually** into exactly one of: its
  `marginBucket(budget, peak)` bucket, or one named exclusion
  (`budget_unknown`, `basis_family_mismatch`, `oom_at_or_above_current`,
  `oom_below_current`, `missing_peak`). The bucket counts are published.
- **The OOM bypass is scoped**: only an OOM row whose own budget is
  `>= current budget` is evidence that the CURRENT budget is short. An OOM at
  a smaller budget is recorded and counted (`oom_below_current`) but proves
  nothing about a larger one.
- **OOM rows never feed the over-provisioned direction.** An OOM-killed run's
  peak is truncated at its own cap — a lower bound on demand, not a
  measurement — so treating it as a peak could produce "over-provisioned,
  lower it" from evidence that the job was killed. Only clean rows contribute
  to the observed max.
- The headline direction is `marginBucket(currentBudget, maxCleanPeak)`, with
  the per-row buckets published beside it as the evidence.

### 5s.4 Fable blocking 2 — `budget` conflated reserve with `memory.max`
(ACCEPTED)

5r.1 wrote "the reserve/`memory.max` actually granted" as if one quantity.
They diverge on real launches (AIRA-192 was filed the same day on exactly this
distinction). Verified where: for a NON-delegate job `ResolveConfineReserve`
sets the reserve **to** `--memory-max` (`confine.go:124-127`) and the cap is
then the declared reserve or the daemon grant, so the two agree. They diverge
for `--delegate-ram`, whose reserve is deliberately framework overhead
(`DefaultDelegateRAMOverhead`) while the cap is the learned scope ceiling.

**Resolved: one quantity per row, with the family named in `budget_basis`,
and families never mixed inside one classification.**

- `budget` is the **enforced scope cap** when one was written —
  `ScopeMemoryMax > 0` — with `budget_basis = "cap:" + ScopeMemoryCapSource`,
  reusing the existing AIRA-133 cap-source vocabulary
  (`operator:--memory-max`, `operator:--memory-reserve`, `daemon:reserve`,
  `delegate-ram`) rather than minting a second one.
- Otherwise it is the **granted reserve**, `budget_basis = "reserve:" +
  ReserveBasis`. This is the genuinely uncapped case (an unpinned,
  non-daemon-admitted job): the number is a ledger booking, not a bound, and
  saying so is the point.
- aitest rows are always `cap:aitest:env:set` or `cap:aitest:env:default` —
  the per-worker `memory.max` really is a kernel-enforced bound.
- Neither term available ⇒ `budget` is NULL and the row reads
  `budget_unknown`, never a fabricated zero.
- The classifier partitions a subject's rows by basis family and classifies
  only the current family, counting the rest as `basis_family_mismatch`. So a
  `cap:`-derived ratio can never be summarised together with a `reserve:`-derived
  one even when one signature has been launched both ways.

### 5s.5 Fable blocking 3 — tests named per invariant (ACCEPTED)

5r.7 named one test. The build ships one named test per invariant below;
each is listed with the direction it guards.

1. `TestResourceBudgetNeverWrites` — the classifier and both faces issue zero
   writes (read-only DB handle), and `Recommendation` is text + `Drilldown`
   only. False-pass direction: a classifier that silently applied a reserve.
2. `TestResourceBudgetBoundariesReuseMarginBucket` — exact 1.25 and 2.0
   boundaries, reusing the existing table's values.
3. `TestResourceBudgetOOMAtOrAboveCurrentBypassesSampleGate` — one OOM at
   `budget >= current` classifies under-provisioned at n=1.
4. `TestResourceBudgetOOMBelowCurrentDoesNotRecommendRaising` — Fable's own
   40G → 30G(OOM) → 40G counterexample; must NOT say "raise".
5. `TestResourceBudgetInsufficientSamplesIsUnevaluatedNotWarning` — n<3 with
   no OOM reads `fallback:insufficient-samples:n=N`.
6. `TestResourceBudgetNullBudgetIsUnevaluatedNotZero`.
7. `TestResourceBudgetMigrationBackfillsKindConfine` — a pre-existing row
   reads `kind='confine'`, budget NULL, under the AIRA-97 concurrent-opener
   guard.
8. `TestConfinePeakRetentionIsPerKindAndSignature` — a 21st aitest row must
   not evict a confine row sharing the signature string.
9. `TestConfinePeakP90IgnoresNonConfineKinds` — a `pytest-worker` row cannot
   move the live admission prior.
10. `TestResourceBudgetRefusesMixedBasisFamilies` — `cap:` and `reserve:`
    rows are never summarised together.
11. Python: `test_retire_worker_records_pool_peak_on_every_path` (all three
    retirement call sites), `test_pool_peak_read_precedes_rmdir`,
    `test_unreadable_pool_peak_records_nothing` (nil, not zero).
12. `TestSkillNamesNothingFromTheRetiredXdistGovernor` stays green.

### 5s.6 Fable non-blocking, accepted

- Face 2 is a NEW dispatch verb `confine-budget` with its own CLI arm, MCP
  tool `aira_confine_budget` and Skill registration — exactly as
  `confine-list`/`confine-kill` are their own verbs. v1 answers for every
  subject at once, sorted worst-first, so no signature selector has to be
  typed; a NUL-joined argv is not a usable command-line argument.
- Face 1's data path is stated: the peak-history readers live on `*DB`
  (`store.go:114`) and gauges on `*Store` (`:136`), reached through the
  `owner` handle `NewScope` already stores. A `*Store` with no owner reads
  `unevaluated`, never empty. Face 2 goes via the daemon, which holds the
  same `*DB`.

### 5s.7 Revised build scope (supersedes 5r.7)

1. Schema: three guarded ADD COLUMNs + `(kind, signature)` index; all four
   existing readers filtered on `kind`; retention DELETE scoped to
   `(kind, signature)`.
2. Capture, confine side: `budget`/`budget_basis` on the `confine-report`
   frame, sourced per 5s.4.
3. Capture, aitest side: per-worker fold in `_retire_worker` (5s.1), one
   pool sample per run through the new `aira worker-peak` verb (5s.2).
4. One pure classifier in `internal/store`, `GaugeResult`-shaped, reusing
   `marginBucket`, per-row bucketing per 5s.3.
5. Two faces: `insightRegistry` row `resource-budget`; dispatch verb
   `confine-budget` on CLI + MCP + Skill.

## 6. Status

Planning resolved — **§5 resolved (5r) 2026-09-09; review round 2 resolved
(5s) 2026-09-09**. The five gate decisions, the four source corrections in
5r.0, the four review resolutions in 5s.1–5s.5, and the deferrals in 5r.6 are
binding on the build.
