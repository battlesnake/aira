# AIRA §17 admission estimate-vs-actual insight gauge — design

Status: PLAN v1 (pre-review). Milestone task #52. Deferral #2 of the peak-RSS
admission-estimate milestone (#50, `docs/superpowers/specs/2026-08-20-aira-peak-rss-admission-estimate-design.md`,
§12.2). Constrained by the no-compat rule (AIRA is not live; no data migration,
no cross-version schema support).

## 1. Motivation and scope

The peak-RSS milestone (#50) made `aira run` derive its admission **reserve**
from per-signature history and stamped the provenance (`admission_reserve`,
`admission_reserve_basis`) on every run record, explicitly as *"the interim
substitute for the deferred §17 gauge"* (#50 §8). This milestone builds that
gauge: a read-only insight that answers the operator-trust question the estimate
raises — **did the reserved headroom actually cover each run's real peak RSS, or
did the estimate under-call?**

In scope (v1):

- One new insight gauge, `admission-reserve-adequacy`, in the existing
  `internal/store` insight registry, computed live (never stored), surfaced
  automatically through every insight face (`insights ls` / `insights show` /
  the TUI gauges panel / MCP `aira_insights`).
- It reads the already-persisted `(admission_reserve, admission_reserve_basis,
  status, peak_rss, resource_signature)` columns from the runner-owned
  per-project `runs.db`. **No schema change** — the data exists as of #50 + M16.

Explicitly out of scope (unchanged deferrals from #50 §12; this milestone does
not touch them):

- `+memory` enablement / eliminating honest-nil peaks under the ambient parent
  (deferral #1). This gauge *consumes* peaks where they exist and is honest
  where they do not.
- Recency-windowed / percentile / EWMA estimators (deferrals #4, #5).
- Per-run `memory.max` caps (#3).
- A time-ordered `as_of` watermark — `runs.db` still has no sortable column
  (#50 §3: `rowid` order is not recency).

## 2. What it measures — the metric model

Per estimate-mode run, the columns in `runs` (in `runs.db`) are:

- `admission_reserve` (INTEGER, nullable) — bytes of free RAM the estimate
  required be kept free: the **reserve threshold**. For every `estimate:*` basis
  this is strictly positive by construction (`estimateReserve` returns
  `override=true` with `reserve>0`; `estimate:capped` stores the `1<<50` cap).
- `admission_reserve_basis` (TEXT, nullable) — provenance vocabulary from
  `internal/core/resource_estimate.go`:
  - `estimate:max=…` / `estimate:oom:max=…` / `estimate:capped` — the estimate
    produced a positive override that was enforced. **These are the evaluable
    population.**
  - `fallback:*` — opted in but used the fixed headroom (no override). Excluded
    from adequacy; counted only as context.
  - `disabled:*` / NULL — estimate off. Excluded entirely.
- `peak_rss` (INTEGER, nullable) — the run's actual peak RSS, **honestly nil**
  where the run's cgroup parent does not delegate `+memory` (#50 §2, the crux of
  that milestone). A nil peak is *unknown*, never zero.
- `status` (TEXT) — includes the first-class terminal value `oom-killed`
  (`internal/runner/types.go:21`): the ground-truth memory failure the estimate
  exists to prevent.

**Evaluable run** = `admission_reserve_basis LIKE 'estimate%'` AND
`admission_reserve` present and `> 0` AND `peak_rss` present and `> 0`.

For each evaluable run the adequacy question is a direct stored comparison:

- **adequate**: `peak_rss <= admission_reserve` — the reserve covered the run.
- **under-reserved**: `peak_rss > admission_reserve` — the dangerous direction;
  the estimate under-called the headroom the run needed.
- **margin ratio**: `admission_reserve / peak_rss` — slack (`>1`) vs shortfall
  (`<1`), bucketed so capped/huge reserves land in the top bucket without
  skewing a mean.

**Framing nuance (reviewers: do not mis-scope this as predictor accuracy).**
`admission_reserve` is a *reserve threshold* (free bytes to keep), computed as
predicted-peak + 15 % or an OOM/cap floor — **not** the predicted peak itself.
We deliberately compare the stored reserve against the run's own peak as an
**adequacy** check ("did we keep enough free for this run's footprint"), which is
the meaningful, robust operator signal. We do **not** reverse-engineer the
predicted peak from the `max=<bytes>` basis substring — that parse is fragile and
adds nothing the direct comparison lacks.

Gauge shape (`GaugeKindRatio`):

- Headline `Value` = `adequacy_rate` = adequate / evaluable (Direction: up is
  desirable). `Unevaluated` when the evaluable population is empty (never 0.0 or
  1.0 fabricated from no data).
- `Breakdown` keyed by `resource_signature` (the estimate is per-signature):
  each cell carries `Count` (evaluable), an `adequate`/`under_reserved` `Counts`
  pair, the cell `Value` = per-signature adequacy rate (unevaluated if that
  signature has zero evaluable runs), an `oom_killed` field, and an
  `excluded_nil_peak` field.
- `Distributions["margin"]` = margin-ratio bucket histogram over evaluable runs:
  `shortfall (<1.0)`, `1.0–1.25`, `1.25–2.0`, `≥2.0`.
- `Fields`: `evaluable`, `adequate`, `under_reserved`, `excluded_nil_peak`
  (estimate-mode runs whose peak was nil — reported, never bucketed),
  `oom_killed` (estimate-mode runs terminating `oom-killed`, counted even when
  peak is nil, since an OOM is itself ground truth of inadequacy).

## 3. Honesty edges (the load-bearing part)

- **nil / non-positive `peak_rss` is EXCLUDED from adequacy** and surfaced as
  `excluded_nil_peak`; it is never coerced to 0. A `COALESCE(peak_rss,0)` would
  fabricate a huge false-adequate margin — the exact honesty failure #50 exists
  to avoid. This is the primary discriminating invariant.
- **zero evaluable runs** (no estimate-mode runs, or every one has a nil peak) →
  `Unevaluated=true`, `Value=nil`, with a precise reason
  (`"no estimate-mode runs with a captured peak"`). Never a fabricated rate.
- **`runs.db` absent** (nothing has ever run in this scope) → `Unevaluated` with
  reason `"no run history (runs.db absent)"`. This is expected fresh state, **not
  an error** — the gauge returns cleanly.
- **`runs` table / columns absent or malformed** (torn/old projection) →
  `Unevaluated` with reason; the read is defensive and never crashes the gauge
  (degrade like #50's `fallback:malformed`).
- **read error / `SQLITE_BUSY`** → `Unevaluated` with reason. The open is
  read-only with `busy_timeout(0)` so it fails fast and can never import a
  DB-lock wait into the daemon read path (mirrors #50 §9).
- **`oom_killed` is reported even with a nil peak.** An OOM is unambiguous
  evidence the reservation was inadequate; hiding it because the peak sample is
  nil would understate the risk.

## 4. Layering — where the code lives, and the coupling it accepts

The insight registry lives in `internal/store` and every face enumerates it
uniformly (`store.InsightGauges` / `GaugeRegistryRows` / `ComputeAllGauges`;
dispatched by `core.go`'s `insights` verb → `c.store.ComputeGauge`). To keep the
gauge in that uniform registry, its `Compute(*Store)` reads the runner-owned
per-project `runs.db` directly:

- Path: `filepath.Join(s.commonDir, "aira", "runs", "runs.db")` — the ledger's
  own projection path (`internal/runner/ledger.go:66`), derivable from the
  `Store`'s `commonDir`.
- Opened **read-only**, mirroring #50's `readOnlyHistoryDSN`: a `file:` URI with
  `mode=ro`, `_pragma=query_only(ON)`, `_pragma=busy_timeout(0)`. One
  stat-guarded, deadline-bounded aggregate query; the handle is closed
  immediately.

This introduces a **filesystem/DB coupling** — `internal/store` now knows the
`runs.db` relative path, three stable column names, and the `estimate%` /
`oom-killed` vocabulary — but **no Go import of `internal/runner`**, so there is
no layering cycle (store stays the base layer). The coupling is accepted because:

1. `runs.db` is a stable, already-shared read target and AIRA is not live, so
   there is no cross-version schema-compat concern (a column rename is a
   same-PR, same-repo change).
2. The alternative — a dependency-inversion interface injected by `core` (which
   may import both layers) — is disproportionate machinery for one read-only
   gauge and would fragment the uniform `Compute(*Store)` registry, so
   `insights ls` / `GaugeRegistryRows` would no longer see it.
3. The knowledge is **confined to one new file** (`internal/store/admission_insight.go`)
   carrying a `covers:` doc comment; if the runner projection ever changes, the
   blast radius is that one file and the gauge degrades to `Unevaluated` rather
   than crashing.

New file `internal/store/admission_insight.go`:

- `admissionReserveAdequacy(commonDir string) (adequacyAggregate, error)` — a
  package-private read-only opener + single aggregate scan + in-Go aggregation
  (read once, derive all cells in memory to avoid a tear between the universe
  count and the per-signature cells, exactly as `computeReviewerVerdictRatio`
  does). Returns a typed aggregate; distinguishes "db absent" from "read error"
  from "no evaluable rows" so the gauge can pick the right honest reason.
- `computeAdmissionReserveAdequacy(s *Store) (GaugeResult, error)` — the
  registry `Compute` func: calls the aggregator, assembles the `GaugeResult`,
  applies the honesty edges of §3.

Registration: add `{Name: "admission-reserve-adequacy", Title: "Admission
reserve vs actual peak RSS", Kind: GaugeKindRatio}` to `insightRegistry` and its
`init()` `Compute` wiring in `internal/store/insights.go`. It then flows through
`ComputeAllGauges` (so `insights show` with no name, and the TUI gauges panel,
pick it up with zero extra wiring). No new verb, no descriptor/help/MCP-schema
change beyond the data-driven registry row.

## 5. Query design — single read-only scan

```sql
SELECT resource_signature, admission_reserve_basis, status,
       admission_reserve, peak_rss
FROM runs
WHERE admission_reserve_basis LIKE 'estimate%';
```

All bucketing (evaluable / adequate / under-reserved / excluded-nil / oom /
margin) is derived in Go from this one result set — no second scan, so the
universe count and the cell counts cannot tear. `Universe.Count` = evaluable
runs; `Universe.Scope` = `"recorded estimate-mode runs only"`;
`Universe.AsOf` = `{"estimate_runs": <evaluable count>}` — an honest count, with
no implied time ordering (`runs.db` has no sortable column, §1).

`Drilldown` points at the real run-listing verb (confirmed during build; e.g.
`{Verb: "run ls", Query: ""}`) as an advisory follow-up; if no run-listing verb
cleanly filters by basis, the drilldown stays a bare `run ls` rather than
inventing an unsupported query.

## 6. Faces

Nothing bespoke. The gauge is data in the existing registry:

- `aira insights ls` lists it (alphabetical registry row).
- `aira insights show admission-reserve-adequacy` computes just it.
- `aira insights show` (no name) includes it via `ComputeAllGauges`.
- The TUI gauges panel renders it automatically (it iterates `ComputeAllGauges`).
- MCP `aira_insights` exposes it (same dispatch).

If a golden test pins the registry projection (`insights ls` / descriptor
goldens), that golden is updated to include the new row — the only "surface"
change.

## 7. Testing — honesty-discriminating, each proven red

Aggregation unit tests seed a real `runs.db` the way `peak_rss_history_test.go`
does (append `RunRecord`s to the ledger, `project()` to build `runs.db`), then
assert the gauge output. Every case below must be **proven red** against the
plausible wrong implementation named with it:

1. **nil-peak estimate-mode run EXCLUDED from adequacy** (red vs a
   `COALESCE(peak,0)` impl that would mis-bucket it adequate) — counted only in
   `excluded_nil_peak`.
2. **under-reserved** (`peak > reserve`) → `under_reserved`, lowers
   `adequacy_rate` (red vs an impl using `>=`/reversed comparison).
3. **adequate incl. boundary** `peak == reserve` → adequate (red vs a strict
   `<` boundary).
4. **zero evaluable → Unevaluated, `Value==nil`** (red vs a fabricated `0.0`).
5. **runs.db absent → Unevaluated with reason, no error, no panic** (red vs an
   impl that surfaces the open error).
6. **malformed/short runs.db** (missing column / not-a-db) → Unevaluated, no
   panic.
7. **oom-killed estimate-mode run surfaced in `oom_killed`** even when its peak
   is nil (red vs an impl that drops nil-peak rows before counting OOMs).
8. **`fallback:*` / `disabled:*` / NULL rows EXCLUDED** from evaluable (red vs a
   `basis IS NOT NULL` filter).
9. **per-signature separation** — two signatures do not cross-contaminate their
   adequacy cells.
10. **read-only proof** — assert the DSN string contains `mode=ro` +
    `busy_timeout(0)` and that the run's main `runs.db` size is unchanged after
    the gauge runs. (Do **not** assert absence of a `-shm` sidecar: a legitimate
    `mode=ro` reader of a WAL database may create one — the exact WAL-invalid
    mistake caught in #50's confirm round.)

Registry test: the gauge appears in `InsightGauges()` / `GaugeRegistryRows()`
and `ComputeAllGauges` computes it without error on an empty scope (Unevaluated,
not a crash).

## 8. Risks

- **runs.db schema coupling** — mitigated by confinement to one file + honest
  degradation to Unevaluated (§4).
- **daemon read-path cost** — one stat-guarded, `busy_timeout(0)`, read-only
  aggregate per gauge computation; bounded and non-blocking (§3).
- **capped reserves** (`estimate:capped`, reserve = `1<<50`) trivially adequate
  with an astronomical margin — lands in the `≥2.0` top bucket, so it cannot
  skew the (bucketed, not averaged) margin distribution; counted as adequate,
  which is honest (the cap did cover the run).
- **mis-framing as predictor accuracy** — the spec fixes the framing as reserve
  adequacy (§2); the reviewer gate should hold that line.

## 9. Two-loop plan

Sol plan-review (inline diff/spec, no repo access) + Fable code-grounded
code-gate → fold → Terra build (TDD, self-review) → Sol build-review (both
false-fail and false-pass directions) → Sol confirm → Opus real-HW verify
(build/vet/`CGO_ENABLED=0`/test ×2/`-race` all green, discriminating tests
proven red against the wrong impl) → merge to master (fast-forward, prune
worktree).
