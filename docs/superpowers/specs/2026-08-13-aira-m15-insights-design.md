# AIRA M15 — insights (honest, drillable gauges) (design)

Status: DRAFT (pre plan-review). Author: Opus (coordinator). Milestone: Phase 4 · M15.
Base: master `d5db2d5`. Building delegated to Codex; gate order (owner): Sol plan-review →
approve → Opus/Fable plan-final → build → Sol build-review → approve → Opus/Fable final →
merge.

Authoritative parent: `docs/superpowers/specs/2026-08-07-aira-design.md` §17 (insights,
metrics, reporting — honest, drillable) and the subsystems each gauge reads: §10 findings,
§13 test-reports, §12 compute, §9 gates/ratchet, §15 traceability, §8 leases/area, §11 events.

## 1b. Sol plan-review resolutions (thread 019ffa09 — BLOCK, all incorporated)

- **R1 (P0, refined R2) — per-source watermarks; NO global seq, NO cross-source atomicity
  claim.** Compute/test-report/quota writes advance their own per-table `at_seq` and are DB-only
  (not journaled, §11), so `max(events.seq)` is not a global watermark. Also a single SQLite
  snapshot cannot span the **git-file-backed** `ListFindings`/`List` reads nor the
  self-opening `FlakyTests` sub-reads — true cross-source atomicity is impossible daemonlessly.
  So the honest model: **each source is read ONCE (a single scan/query per source, internally
  consistent), and its own watermark is recorded**; `as_of` is the **map** of just the sources
  the gauge touched (`{events_seq?, compute_at_seq?, test_report_at_seq?, quota_at_seq?,
  findings_scan?}`). A gauge makes **no** claim of atomic consistency ACROSS sources (source A
  may be read just before a write and source B just after) — the per-source watermarks state
  exactly what was seen. `events.seq` appears only for sources that actually advance it. (This
  is honest read-labelling, not stored-aggregate data — D1 stands.) (§2, §5, §6.)
- **R2 (P1) — present-bucket COUNT, not just overflow.** A compute sum must track the number
  of **present** buckets separately; all-absent ⇒ the sum is `unevaluated` (0 present buckets),
  never `0 tokens`. `nil` differs from pointer-to-`0`. `review-loop-economics` sums only present
  buckets and a phase/gauge with zero present buckets across its events reads `unevaluated`.
  (§3.5; T-all-absent.)
- **R3 (P1) — flaky-rate is CELL-EXACT, reusing the evaluator honestly.** M13 `FlakyTests`
  marks a test flaky if ANY cell is flaky even when another cell is `unevaluated`. The gauge's
  **unit is the identity cell** (`FlakyCell`), not the test-level aggregate: numerator = cells
  in `FlakyStateFlaky`; denominator = cells in `flaky ∪ clean` (i.e. with **sufficient**
  first-pass parser-complete evidence); `unevaluated` cells are in neither. Zero
  sufficiently-evidenced cells ⇒ gauge `unevaluated`. (§3.3; mixed-cell test.)
- **R4 (P1) — explicit source + worktree universe; drilldown matches scope.** There is **no
  source registry** (source is an open vocabulary), so `reviewer-verdict-ratio` reports only
  over **sources that have findings**; a source with **no** findings is simply **absent** from
  the breakdown (NOT an `unevaluated` cell — we cannot enumerate non-existent sources). The
  real-0-vs-unevaluated distinction lives WITHIN a present source: `refuted=0` with
  `confirmed+refuted>0` ⇒ real kill-rate `0`; `confirmed+refuted=0` (only `plausible`) ⇒
  kill-rate `unevaluated`. **Scope is PER-SOURCE, stated per gauge in `universe.scope`:** the
  finding + ticket gauges (`reviewer-verdict-ratio`, `recurring-mistakes`, `wip`) are
  **current-worktree** (matching `ListFindings`/`List`); the telemetry gauges (`flaky-rate`,
  `review-loop-economics`, `quota-burn`) are **project-scoped** (matching `ListComputeEvents`/
  `ListQuotaSnapshots`/`FlakyTests`, which key on `project_id`). Each gauge's drilldown runs in
  that gauge's own scope so it reproduces the value. (A cross-scope unification is deferred,
  D2.) (§3, §4.)
- **R5 (P1) — drilldowns are UNCAPPED distribution reads; specify the NEW read APIs (the
  existing ones don't suffice).** The selector grammar has no `OR` and requires `field:value`
  per term. The **ticket** distribution already has an uncapped path — `CountResult` (`list
  --by <field>` counts ALL rows) — so `wip` uses `list --by status`/`--by assignee` (no `OR`;
  the gauge value IS the non-terminal cells). But `CountResult` is **ticket-only**, and `find
  ls` **caps/omits** finding distributions — so M15 **adds an uncapped FINDING distribution
  store read** `CountFindings(query, by)` (the finding analogue of `CountResult`, counting all
  rows), surfaced as `find ls <query> --by <field>` returning the full distribution (not the
  50-capped list). `reviewer-verdict-ratio` ⇒ `find ls "source:<s>" --by verdict`;
  `recurring-mistakes` ⇒ `find ls "disposition:open" --by category` — both via `CountFindings`.
  The gauge and its drilldown call the SAME uncapped store read, tested with **>50 rows**.
  (§3, §4, §7.)
- **R6 (P1) — quota nullable ⇒ unevaluated; ratchet-status + traceability-status DEFERRED (need
  a read-only-evaluator seam).** `quota-burn` renders each absent numeric field as `unevaluated`
  (never 0). But `GateCheck` reads the STORED gate-audit projection (not a live re-evaluation)
  and `RunGate` MUTATES audit state — neither is a live read-only fold, so a `ratchet-status`
  gauge cannot honestly re-evaluate without either a stale/stored read or a mutation. Likewise
  the `check` traceability path reconciles (writes findings). **Both `ratchet-status` and
  `traceability-status` are therefore MOVED to the deferred set (D2):** they require first
  factoring a **pure read-only evaluator** shared by `GateCheck`/`RunGate` (and a read-only
  traceability computation) — a clean but separate refactor, its own milestone-lite. M15's
  initial set is the six gauges cleanly computable from existing read-only paths. (§3, §7.)
- **R8 (P1, re-review) — the drilldown reproduces the VALUE via an authoritative uncapped
  aggregate query; add the two aggregates the value-gauges need.** A drilldown must recompute
  the gauge's value, not merely list source rows:
  - **distribution/count gauges** (`recurring-mistakes`, `reviewer-verdict-ratio` counts, `wip`)
    reproduce via the existing uncapped `CountResult` `--by` distribution (all rows). `wip` is a
    **distribution gauge** over `status`/`assignee`; its drilldown `list --by status` returns
    the full distribution and the gauge's value IS its non-terminal cells (the human reads those
    rows — the numbers are present, not a hidden filtered scalar). `reviewer-verdict-ratio`
    kill-rate is deterministic arithmetic on the reproduced `--by verdict` counts.
  - **`review-loop-economics`** needs a new authoritative **`spend ls --by phase` aggregate**:
    per-phase **SQL `SUM` of each bucket AND `SUM(cost_usd)`** (SQLite `SUM` ignores NULL ⇒
    present-only sum for free; a phase whose bucket/cost is all-NULL ⇒ `SUM` returns NULL ⇒ that
    cell `unevaluated`, R2). Cost is a first-class part of economics — the aggregate + drilldown
    both carry it. One live query, no stored numeral (D1).
  - **`flaky-rate`** drilldown is a **`test-report flaky --all`** cell-state summary (flaky /
    clean / unevaluated cell counts) — the authoritative denominator, not just the flaky cells;
    the rate = flaky ÷ (flaky+clean) reproduces from it.
  - **DB-source consistency:** the telemetry aggregates (`spend --by phase`, `flaky --all`,
    quota) read their tables inside **one SQLite read transaction** (all DB-backed, so a read tx
    DOES span them — unlike the git-file finding/ticket gauges, which are best-effort
    single-scan, R1). `flaky --all` loads report headers + results in that one read tx (a
    concurrent `test-report add` cannot tear it).
  T9 asserts each gauge's value equals its drilldown's recomputation, tested with **>50 rows**
  (the case a 50-row-capped `ls` would get wrong). (§3, §4.)
- **R7 (P2) — added tests** (§6): all-nil compute buckets ⇒ unevaluated; partial quota fields;
  mixed flaky identity cells; current-worktree scope stated + drilldown matches; a concurrent
  mutation during a gauge read yields a per-source-consistent result (each source read once);
  the gauge makes no cross-source atomicity claim; an absent finding
  source is absent (not an unevaluated cell); drilldown reproduces the value (not just the
  universe) with >50 rows (the count path, not the capped ls projection). T6 rewritten for the per-source-watermark model; T9 asserts the
  reproduced VALUE.

---

## 1. Scope

The useful JIRA subset recast for agents — **the drill-down is the point**. M15 delivers the
**gauge framework** (the §17 honesty discipline made mechanical) plus a curated initial set of
gauges over the data the earlier milestones already produce. It adds **no new stored data** —
every gauge is a **live query** computed on demand.

**The §17 discipline (the correctness core — every gauge obeys ALL of these):**
1. **A live query, NEVER a stored numeral.** Computed from the current tables at call time; no
   gauge value is persisted.
2. **Carries universe + as-of.** The set it computed over (count + scope: project / all
   worktrees) and the point in time — a **per-source watermark map** (each source's own read
   point; NOT a single global seq, R1), plus wall-clock `at`.
3. **Uncomputable ⇒ `unevaluated`, never a fake 0/pass.** An empty universe, absent provider
   data, or fewer-than-N observations reads `unevaluated` with a reason — it is NEVER rendered
   as `0`, `100%`, or `pass`. (The load-bearing honesty rule: distinguish "codex refuted 0 of
   5" [a real kill-rate of 0] from "codex has no findings" [unevaluated].)
4. **Direction vs a baseline (when defined).** A gauge that has a meaningful baseline reports a
   direction (`up`/`down`/`flat`/`unevaluated`); one without omits it (never fabricates a
   trend).
5. **Itself a drillable saved query.** Each gauge exposes the exact reproducing query
   (`{verb, query}`, e.g. `find ls "source:codex verdict:refuted"`) so the number is
   reproducible by hand — "write the query, not the numeral."

### 1.1 Non-goals / deferrals (written down; do not build)

- **D1 — no new stored data / no aggregate columns.** M15 is read-only over existing tables.
  Anything that would persist a computed numeral is out (violates §17 rule 1).
- **D2 — a curated initial gauge set, not the full §17 catalogue.** Adding a gauge later is a
  new registered live query + tests, not a schema change (the framework makes it cheap). The
  initial set (§3) spans the data sources; the rest (**`ratchet-status`** + **`traceability-
  status`** — both need a pure read-only gate/traceability evaluator seam, R6; stage-dwell
  timeline, collision/churn history, run wall-clock/peak-RSS per phase, master-red-duration
  timeline, area-overlap gauge) are deferred as follow-on gauges, explicitly listed.
- **D3 — no TUI / dashboard rendering** (Phase 4/5 TUI). M15 emits structured gauge results
  (`--json`) + a plain text table; the interactive dashboard is later.
- **D4 — no alerting/thresholds.** A gauge reports value + direction; policy thresholds
  ("burn > X ⇒ warn") are a project concern, not built in.

---

## 2. The gauge model

`GaugeResult` (returned by each gauge, rendered by the face):

`{name, title, kind(count|ratio|rate|duration|distribution), value?(number|string),
breakdown?(map[string]GaugeCell), unevaluated(bool), unevaluated_reason?, universe{count,
scope(per-gauge: current-worktree | project, R4), as_of(map: per-source watermarks it read), at}, direction?(up|down|
flat|unevaluated), baseline?, drilldown{verb, query}}`

- `value` is present **only** when `unevaluated=false`. A `ratio`/`rate` gauge with an empty
  denominator is `unevaluated` (never `0/0=0`).
- `breakdown` — a per-key distribution (e.g. per-source verdict ratios), each cell itself
  carrying `unevaluated` where that key lacks data. A key present with a real `0` is distinct
  from an absent/unevaluated key.
- `universe` is **mandatory and always populated** — even for an `unevaluated` gauge it states
  what was examined (e.g. `{count:0, scope:project, as_of:{…}}`), so "unevaluated" is never
  bare. `scope` is **per-gauge** — `current-worktree` for finding/ticket gauges, `project` for
  telemetry gauges (R4).
- **`as_of` is a per-source watermark map (R1), NOT a single global seq.** Because telemetry
  (compute/test-report/quota) advances its own per-table `at_seq` and is not journaled (§11),
  `events.seq` does not move on those writes. So `as_of` reports the watermarks of the sources
  the gauge actually read — e.g. `{events_seq, compute_at_seq, test_report_at_seq,
  quota_at_seq}` (only those it touched); `at` (wall-clock) is advisory. **No cross-source
  atomicity is claimed** (a single SQLite snapshot cannot span git-file-backed reads nor the
  multi-query `FlakyTests`, R1) — each source is read once, its own watermark labelling exactly
  what was seen.
- `direction` present only when the gauge defines a baseline it can compute; else omitted.

A **registry** maps `name → Gauge` (a function `Compute(store, opts) (GaugeResult, error)`).
`aira insights` runs all registered gauges; `aira insights <name>` runs one. Registry-driven,
like the dispatch descriptors — so `aira insights ls` lists gauge names + titles.

---

## 3. Initial gauge set (six gauges over read-only paths; each honest)

*(`ratchet-status` and `traceability-status` are DEFERRED — R6 — until a pure read-only
gate/traceability evaluator seam exists; they are in the §1.1 deferred list.)*

1. **`reviewer-verdict-ratio`** (findings, §10) — breakdown over **sources that have findings**
   (no source registry exists, so a source with no findings is simply **absent**, not an
   `unevaluated` cell — R4). Per present source: confirmed/refuted/plausible counts and
   **kill-rate** = `refuted / (confirmed+refuted)`. `refuted=0` with `confirmed+refuted>0` ⇒
   real kill-rate `0`; `confirmed+refuted=0` (only `plausible`) ⇒ kill-rate `unevaluated` (not
   `0/0`). Current-worktree scope; drilldown (per source, uncapped distribution) `find ls
   "source:<s>" --by verdict`. The real quality/disjointness signal.
2. **`recurring-mistakes`** (findings, §10) — distribution of **open** findings by `category`
   (the trigger to derive a lint). `unevaluated` if zero findings. Drilldown: `find ls
   "disposition:open" --by category`.
3. **`flaky-rate`** (test-reports, §13) — the unit is the **identity cell** (`FlakyCell`), not
   the test-level aggregate (R3): numerator = cells in `FlakyStateFlaky`; denominator = cells
   with **sufficient** first-pass parser-complete evidence (`flaky ∪ clean`); `unevaluated`
   cells are in neither. Zero sufficiently-evidenced cells ⇒ gauge `unevaluated`. Reuses M13
   `FlakyTests` (its `FlakyCell.State`). Drilldown: the authoritative **`test-report flaky
   --all`** cell-state summary (flaky/clean/unevaluated cell counts — the denominator, not just
   the flaky cells); the rate reproduces as flaky ÷ (flaky+clean) (R8).
4. **`wip`** (tickets, §8) — distribution of **all non-terminal** tickets
   (`draft·planned·in-progress·in-review`) by `status` and by `assignee`. `unevaluated` if zero
   non-terminal tickets. Uncapped count/distribution (R5). Drilldown (grammar-valid, uncapped):
   `list --by status` and `list --by assignee` (the full distribution over all tickets from the
   count path; the gauge filters to non-terminal statuses from that distribution — no `OR`
   term, which the grammar forbids).
5. **`review-loop-economics`** (compute, §12) — present-only bucket sums + cost by **§9 phase**
   (esp. `plan-review`/`work-review` vs `implement`), summing only **present** disjoint buckets
   with a **present-count** (R2 — an all-absent phase never renders `0 tokens`; a phase with no
   present buckets ⇒ its cell `unevaluated`). Zero compute events overall ⇒ gauge `unevaluated`
   (the M14 payoff, honestly). Drilldown: the authoritative **`spend ls --by phase` aggregate**
   (per-phase `SUM` of each bucket, NULL-ignoring = present-only, all-NULL ⇒ unevaluated cell) —
   the gauge and drilldown share this one query (R8).
6. **`quota-burn`** (quota, §12) — the latest `QuotaSnapshot` per provider; each absent numeric
   field renders `unevaluated`, never `0` (R6). A burn/direction needs two snapshots — one
   snapshot ⇒ values present, direction `unevaluated`; no snapshots ⇒ `unevaluated`. Drilldown:
   `quota ls`.
*(deferred — R6:* `ratchet-status` *and* `traceability-status` *land once a pure read-only
gate/traceability evaluator seam exists — see §1.1 D2.)*

Each gauge is a thin function over an existing store read (or the existing check/flaky/ratchet
evaluators) — **no new tables, no new persisted numerals** (D1).

---

## 4. Faces

- `aira insights [--json]` — run all gauges, render each (text table: name · value-or-
  `unevaluated(reason)` · universe · direction · drilldown). `SafetyRead`.
- `aira insights <name> [--json]` — one gauge (full detail incl. breakdown).
- `aira insights ls` — list registered gauge names + titles (registry projection).
- Grouped verb `insights` (ls | show `<name>` | default=all), mirroring the dispatch model;
  descriptor Summary/Safety/Include/Example; **examples machine-verified** (drift/parity/E2E,
  M8b lesson). Register `U_INSIGHT_UNEVALUATED` (3) — a gauge value read where unevaluated (for
  the response contract); gauges themselves never error on empty data, they report unevaluated.
- **Two authoritative uncapped aggregate queries the value-gauges + their drilldowns share
  (R8), both `SafetyRead`, live (no stored numeral):** `spend ls --by phase` — extend the M14
  `spend ls` with a `--by phase` mode returning per-phase `SUM` of each bucket (SQL `SUM`,
  NULL-ignoring ⇒ present-only; all-NULL bucket ⇒ NULL ⇒ unevaluated cell); and `test-report
  flaky --all` — extend the M13 `flaky` op with an `--all` cell-state summary (flaky/clean/
  unevaluated cell counts). Both are the drilldowns AND the gauge's own computation source, so
  the value provably reproduces.

---

## 5. Honesty invariants (the properties the matrix defends)

1. **No fake zero.** A ratio/rate with an empty denominator ⇒ `unevaluated`, never `0`. A
   per-key cell with no data ⇒ `unevaluated`, distinct from a real `0`.
2. **Live, never stored.** Two calls at different table states give different results; nothing
   is persisted; each source's own watermark in the `as_of` map moves when that source changes.
3. **Universe always stated**, even when unevaluated (what was examined + as-of).
4. **Direction only when a baseline is computable**, else omitted (never a fabricated trend).
5. **Drilldown reproduces the number.** The emitted `{verb, query}` run by hand yields the
   same universe.
6. **Reuses the authoritative READ-ONLY evaluators.** flaky via M13 `FlakyTests` (read-only),
   compute buckets via M14 (present-only sum) — a gauge never re-implements (and possibly
   contradicts) an existing honest computation, and never calls a MUTATING or stored-projection
   path (that is exactly why `ratchet-status`/`traceability-status` are deferred until a pure
   read-only seam exists, R6).

---

## 6. Adversarial test matrix (every confirmed counterexample → a regression test)

- **T1 empty universe ⇒ unevaluated, not 0** — a fresh project: every gauge reads
  `unevaluated` with a reason + `universe.count=0`, never `value:0`/`0%`/`pass`.
- **T2 real 0 ≠ unevaluated; absent source ≠ unevaluated (R4)** — a source with findings all
  `confirmed` (0 refuted, denom>0) ⇒ kill-rate `0` (present); a source with only `plausible`
  (denom 0) ⇒ kill-rate `unevaluated`; a source with **NO findings** ⇒ **absent from the
  breakdown** (not an `unevaluated` cell — no source registry). The load-bearing test.
- **T3 mixed flaky identity cells (R3)** — a test flaky in one cell but `unevaluated` in
  another: the gauge counts only the flaky **cell** in the numerator, the unevaluated cell in
  neither — the test-level aggregate is NOT the unit.
- **T4 flaky-rate cell denominator** — cells with insufficient evidence are in neither
  numerator nor denominator; zero sufficiently-evidenced cells ⇒ gauge unevaluated.
- **T4b all-absent compute buckets ⇒ unevaluated (R2)** — a compute event with every bucket
  nil contributes nothing and a phase of only such events reads `unevaluated`, never `0 tokens`
  (present-bucket count = 0).
- **T5 review-loop-economics present-only sum** — a phase whose compute events omit a bucket
  never counts it as 0 (M14 honesty carried through); a phase with no events ⇒ cell
  unevaluated; no events at all ⇒ gauge unevaluated (not `0 tokens`).
- **T6 live/as-of per-source watermark (R1)** — add a compute event, re-run `review-loop-
  economics`: the value AND `as_of.compute_at_seq` change (even though the gauge reports no
  `events_seq`); nothing was persisted (verify no new rows/columns). Each source is read once
  (internally consistent); no cross-source atomicity is claimed (R1).
- **T7 quota-burn direction** — one snapshot ⇒ value present, direction unevaluated; two ⇒
  direction computed from the two most recent; none ⇒ unevaluated.
- **T8 wip covers all non-terminal statuses (R5)** — tickets across draft/planned/in-progress/
  in-review counted; a terminal (done/retired) ticket excluded; the drilldown `list --by
  status` distribution reproduces the gauge's counts.
- **T9 drilldown reproduces the VALUE (R5/R7)** — the emitted `{verb, query}` for
  `reviewer-verdict-ratio`/`recurring-mistakes`/`wip` run through `Run()` yields a
  universe/distribution matching the gauge's computed value (not just its universe count),
  including with **>50 rows** (the uncapped count path, not the capped `ls` projection).
- **T10 universe always populated** — even an unevaluated gauge carries `universe` (count +
  scope + `as_of` map), never a bare "unevaluated".
- **T11 faces** — drift/parity/E2E for `insights`/`insights ls`/`insights <name>` + valid
  agent-guide examples; `insights ls` matches the registry.

Real-binary e2e (Opus/Fable final): fresh project ⇒ all gauges unevaluated with universe;
after adding findings (mixed verdicts incl. a zero-refuted source and a no-finding source),
test-reports (a flaky pair), compute events (two phases, one with a missing bucket), a quota
snapshot, and a ratchet gate ⇒ each gauge shows a live, honest value with the real-0-vs-
unevaluated (and absent-source) distinction, a working drilldown that reproduces the value,
and a per-source `as_of` watermark that moves with the source it read.

---

## 7. Layering & files

- `internal/store/insights.go` (new) — the `Gauge`/`GaugeResult` types, the registry, and the
  six gauge functions (all **read-only**). `InsightGauges()`/`ComputeGauge(name)`/
  `ComputeAllGauges()`. Each source read once, its watermark into the `as_of` map.
- **New uncapped read APIs (R5/R8)** the gauges + their drilldowns share:
  `CountFindings(query, by)` (finding analogue of the ticket-only `CountResult`, all rows;
  surfaced as uncapped `find ls … --by`); a `spend ls --by phase` aggregate (per-phase
  `SUM(bucket…)`+`SUM(cost_usd)`, NULL-aware, in one read tx); `test-report flaky --all`
  cell-state summary (in one read tx over headers+results). These live in
  `internal/store/finding.go`, `internal/store/compute.go`, `internal/store/testreport.go`
  respectively; `insights.go` calls them.
- `internal/core/core.go` + `cmd/aira/main.go` — the `insights` grouped verb + Store iface +
  metadata/agent-guide.
- No new tables, no new persisted data, **no mutation** (read-only). Reuses M13 `FlakyTests` +
  M14 compute reads. `ratchet-status`/`traceability-status` (which would need a MUTATING or
  stored-projection path) are deferred until a pure read-only seam exists (R6).

## 8. Build plan (delegated to Codex, TDD, frequent commits)

1. `Gauge`/`GaugeResult` + registry + honesty scaffolding (universe/per-source-`as_of`/
   unevaluated/drilldown), empty-universe ⇒ unevaluated (T1, T10).
2. findings gauges: `reviewer-verdict-ratio` + `recurring-mistakes` (T2, T3, T9 — the
   real-0-vs-unevaluated + absent-source core) + `wip` (T8, uncapped distribution).
3. test-report + compute + quota gauges: `flaky-rate` (T3/T4 cell-exact), `review-loop-
   economics` (T4b/T5 present-count), `quota-burn` (T7) — reuse M13/M14 read-only readers.
4. `insights` faces + metadata/agent-guide (T11) + per-source-watermark liveness (T6) + full
   `make ci`.

Gate: Sol plan-review (this doc) → approve → Opus/Fable plan-final → Codex build (sandbox-
verifiable) → Sol build-review (weight the honesty core: a fake 0, a stored numeral, a gauge
that contradicts its authoritative evaluator, a missing universe) → approve → Opus/Fable final
+ real-binary e2e → merge `--ff-only`.
