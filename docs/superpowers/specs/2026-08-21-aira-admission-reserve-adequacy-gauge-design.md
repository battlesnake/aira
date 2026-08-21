# AIRA §17 admission estimate-vs-actual insight gauge — design

Status: PLAN v3 — **APPROVED + BUILT** (build reviewed: Sol build-review
CHANGES-NEEDED [4 P1 + 2 P2] folded — OOM-first already correct; folded: exact
integer margin bucketing (no float rounding at 1.25/2.0), nullable signature
`*string` with a distinct `"(signature absent)"` sentinel (NULL ≠ empty ≠ literal
`"(unsigned)"`), `Direction:"up"` on the absent path, an injectable read-only
opener seam proving the DSN is used, corrected `shortfall(<1.0)` key, and
de-porous isolated per-form/per-status/margin-boundary tests). Sol plan-review r1
CHANGES-NEEDED + Fable
code-gate r1 GATE-PASS-conditional folded → v2; Sol r2 CHANGES-NEEDED [1 P0
classification order + grammar/signature/reason] + Fable re-gate
GATE-PASS-conditional [kill/cancel/lost truncation + nullable signature] folded
→ v3; **Sol r3 APPROVE-PLAN**). Milestone task #52.
Deferral #2 of the peak-RSS admission-estimate milestone (#50,
`docs/superpowers/specs/2026-08-20-aira-peak-rss-admission-estimate-design.md`,
§12.2). Constrained by the no-compat rule (AIRA is not live).

## 1. Motivation and scope

The peak-RSS milestone (#50) made `aira run` derive its admission **reserve**
from per-signature history and stamped the provenance (`admission_reserve`,
`admission_reserve_basis`) on every run record, explicitly as *"the interim
substitute for the deferred §17 gauge"* (#50 §8). This milestone builds that
gauge: a read-only insight over how the stamped reserve threshold compared to
each estimate-mode run's **observed** peak RSS and terminal outcome.

In scope (v1):

- One new insight gauge, `admission-reserve-adequacy`, in the existing
  `internal/store` insight registry, computed live (never stored), surfaced
  automatically through every insight face (`insights ls` / `insights show` /
  the TUI gauges panel / MCP `aira_insights`).
- It consumes the already-persisted `(resource_signature,
  admission_reserve_basis, status, admission_reserve, peak_rss)` columns of the
  runner-owned per-project `runs.db`. **No schema change** — the data exists as
  of #50 + M16.

Out of scope (unchanged #50 §12 deferrals; untouched here): `+memory` enablement
/ eliminating honest-nil peaks (#1); recency/percentile/EWMA estimators (#4/#5);
per-run `memory.max` caps (#3); a time-ordered `as_of` watermark (`runs.db` has
no sortable column, #50 §3).

## 2. What it measures — semantics stated honestly

**This gauge is descriptive, not a causal guarantee.** `admission_reserve` is
the amount of free RAM the estimate required be kept free *at admission time*;
it is **not** memory reserved for the run's lifetime, and `reserve >= observed
peak` does **not** prove the run was protected from OOM (concurrency, timing,
and the real-free-RAM gate all intervene). The gauge therefore reports two
distinct honest things:

1. an **observed-peak envelope** comparison — how often the stamped reserve
   threshold met or exceeded the run's observed peak RSS; and
2. the **ground-truth OOM outcome** — estimate-mode runs that terminated
   `oom-killed`, which is direct evidence the reservation was *inadequate*
   regardless of any peak sample.

Title: **"Admission reserve vs observed peak RSS"**. No "kept free" / "protected"
language anywhere in title, field names, or operator guidance.

### 2.1 Per-run classification (the load-bearing logic)

Candidate population = runs whose `admission_reserve_basis` begins `estimate`
(coarse SQL prefilter). Each candidate is then classified in Go into **exactly
one rate-partition** — `{adequate, inadequate, excluded}` — with OOM handled as
a **primary orthogonal axis evaluated first** (Sol r2 P0: bucketing capped /
invalid-reserve *before* OOM let an OOM-killed run escape `inadequate`, so the
headline could still read 100 %). The two truncation arguments (Sol on OOM,
Fable on kill/cancel/lost) unify to one rule: **an observed peak is a lower
bound; only `exited` runs have a complete footprint; any OOM is inadequate.**

Step 1 — **strict basis grammar** (Sol r2 P1; `LIKE 'estimate%'` is only a
coarse prefilter and is not trusted). Anchored full-match against the exact forms
`internal/core/resource_estimate.go` emits, else `malformed_basis` (excluded,
surfaced):
- `estimate:capped` — exact literal.
- `estimate:max=<n>,n=<n>,f=115` — anchored `^estimate:max=\d+,n=\d+,f=115$`.
- `estimate:oom:max=<n>,n=<n>,oom=<n>,f=115` — anchored
  `^estimate:oom:max=\d+,n=\d+,oom=\d+,f=115$`.
A near-miss (`estimate:max=x…`, truncated, trailing suffix, nonnumeric field,
unknown `estimate:*`) → `malformed_basis`. (The embedded numbers are **not**
consumed — classification uses the stored `admission_reserve`/`peak_rss` columns,
not the basis string; the grammar only authenticates the form.)

Step 2 — **OOM axis (primary, first).** For any run that passed step 1 (capped,
max, or oom:max) with `status == "oom-killed"` → **inadequate**, unconditionally
(count `oom_killed`; if the form was capped also count `capped_oom`). OOM rows
are never placed in the margin distribution (peak truncated at the limit).

Step 3 — **non-OOM classification** (only runs not already inadequate by step 2):
- `capped` → **excluded/capped** (its `1<<50` reserve trivially exceeds any peak;
  says nothing about estimate quality — Sol r1 P1-3).
- `invalid_reserve` — `admission_reserve` NULL or `<= 0` → **excluded/invalid_reserve**
  (invariant violation / corruption; code guarantees `>0`, resource_estimate.go:81-83).
- valid reserve (`> 0`):
  - `peak > reserve` (peak present, `> 0`) → **inadequate**, for **any** status
    (Fable P1: a truncated peak already exceeding reserve is a conclusive lower
    bound). In the margin distribution only if `status == "exited"`.
  - `peak` present, `0 < peak <= reserve`:
    - `status == "exited"` → **adequate** (complete footprint conclusively within
      the envelope). In the margin distribution.
    - other terminal status (`killed`/`cancelled`/`lost`) → **excluded/truncated_inconclusive**
      (peak is a lower bound; "under reserve" is not conclusive — Fable P1).
    - non-terminal (`starting`/`running`) → **excluded/nonterminal** (defensive).
  - `peak` NULL → **excluded/missing_peak** (honest-nil, unknown — never coerced
    to 0; the primary discriminating invariant).
  - `peak` present `<= 0` → **excluded/invalid_peak** (corruption; Sol r1 P1-1:
    not lumped with `missing_peak`).

**Evaluable** = adequate + inadequate. **adequacy_rate** = adequate / evaluable.
When evaluable == 0 → `Unevaluated`, `Value=nil` (never a fabricated 0.0/1.0)
with a **neutral** reason (Sol r2 P1): `"no estimate-mode runs recorded"` when
`candidate == 0`, else `"no eligible evaluable runs — see exclusion counts"`
(the `Fields` exclusion counts explain why, without a claim the counts may
contradict).

### 2.2 Gauge shape (`GaugeKindRatio`)

- `Value` = adequacy_rate (float) or absent when Unevaluated. `Direction: "up"`.
- `Fields` (all honest integer counts, so partial-corruption is visible, not
  hidden). The **exclusive rate-partition** counts sum to `candidate`:
  `adequate`, `inadequate`, and the excluded buckets `capped`, `invalid_reserve`,
  `missing_peak`, `invalid_peak`, `truncated_inconclusive`, `nonterminal`,
  `malformed_basis`; plus `candidate` and `evaluable` (= adequate + inadequate).
  Two **overlay** counts (not part of the partition, may overlap it): `oom_killed`
  (all estimate-form runs that OOM-killed — every one is in `inadequate`) and
  `capped_oom` (capped runs that OOM-killed — in `inadequate`, not in `capped`).
  Plus `semantics` (a short string: "reserve threshold vs observed peak; OOM and
  observed-peak-over-reserve = inadequate; adequate requires a completed
  (exited) run; not a causal OOM-protection guarantee").
- `Breakdown` keyed by `resource_signature` (a run with a NULL signature buckets
  under `"(signature absent)"`; a present-empty signature and a literal `"(unsigned)"` run name stay distinct, Sol build-review P1): per-signature `Count` (evaluable) and a
  `Counts` map {adequate, inadequate, oom_killed, missing_peak, excluded}, cell
  `Value` = per-signature adequacy_rate, or `Unevaluated` when that signature has
  zero evaluable runs. Per-signature cells intentionally summarise the evaluable
  outcome plus a rolled-up `excluded`; the full exclusion breakdown lives in the
  top-level `Fields` (documented, not a silent omission — Sol r2 P2).
- `Distributions["margin"]` = reserve/peak bucket histogram over the
  peak-bearing, non-OOM, uncapped runs (adequate ∪ envelope-exceeded):
  `shortfall(<1.0)`, `1.0–1.25`, `1.25–2.0`, `>=2.0`. Bucketed (not averaged),
  so a huge margin cannot skew it.
- `Universe`: `Count` = evaluable; `Scope` = `"evaluable estimate-mode runs
  (uncapped; peak-or-OOM determinable)"` (Sol P1-4: scope matches Count, not
  "all estimate-mode runs"). `AsOf` passed as `nil` → `gaugeUniverse` stores
  `{}` (an honest empty as-of; no misused watermark, no implied time order, and
  still non-nil so the empty-universe invariant test at insights_test.go passes).
- `Drilldown`: **empty** (`GaugeDrilldown{}`). There is no run-listing verb —
  runs surface only via `reconcile` (returns records) and `run-log <id>`
  (core.go:1447/1661/1702); fabricating a `run ls --by …` would be dishonest, so
  the gauge offers no drill hint rather than an unsupported one.

## 3. Honesty edges (restated as invariants the tests pin)

- nil / non-positive `peak_rss` is **never** coerced to 0; it is `missing_peak`
  or `invalid_peak` and excluded from the rate. (Primary discriminating case.)
- an OOM with a nil peak still counts as **inadequate** and in `oom_killed` — an
  OOM is ground truth, so the headline can never read 100 % while estimate-mode
  runs were OOM-killed (Sol P0-1).
- zero evaluable → `Unevaluated`, `Value` absent, precise reason.
- `runs.db` absent → `Unevaluated`, reason `"no run history"` — expected fresh
  state, returned cleanly, **not** an error (Fable: reader returns present=false).
- `runs` table / columns absent or malformed → `Unevaluated` with reason; the
  read degrades, never panics.
- read error / `SQLITE_BUSY` → `Unevaluated` with reason; the open uses
  `busy_timeout(0)` (fail-fast) so it can never import a DB-lock wait into the
  daemon read path.

## 4. Layering — an exported runner reader + a pure store classifier

**Corrected premise (Fable P1).** The dependency direction is `internal/store →
internal/runner`: store already imports runner (store.go:24, rant.go:14,
gate_command.go:20; e.g. `runner.HasRun(s.commonDir, id)` at rant.go:513), and
runner imports store **nowhere**. There is no cycle. So the gauge does **not**
open `runs.db` by hand from store; instead all `runs.db` path/schema/DSN
knowledge stays in the runner package (which owns that DB), exposed as one
exported reader, and the store gauge consumes typed rows. This is strictly
cleaner than v1's by-path duplication and than an injected interface (Sol P2 —
no `Compute(*Store)` change, no per-scope injection wiring), and it mirrors the
existing `runner.HasRun(commonDir, …)` precedent.

New runner code (`internal/runner/estimate_actual.go`):

```go
type AdmissionSample struct {
    Signature string // resource_signature; "" when SQL NULL (scanned via
                     // sql.NullString so one NULL row cannot error the whole
                     // read — Sol r2 P1 / Fable P2; nil signature → "(signature absent)" bucket)
    Basis     string // admission_reserve_basis (estimate-prefixed, by query)
    Status    string // runs.status, e.g. "oom-killed" (NOT NULL by schema)
    Reserve   *int64 // admission_reserve  (nil == SQL NULL)
    Peak      *int64 // peak_rss           (nil == honest-nil, unknown)
}

// EstimateAdmissionSamples opens the per-project runs.db READ-ONLY and returns
// every estimate-prefixed run row. present is false (with nil samples, nil err)
// when runs.db does not exist — expected fresh state, not an error. A read/scan
// error is returned as err with present=true so the caller degrades to
// Unevaluated rather than crashing.
func EstimateAdmissionSamples(ctx context.Context, commonDir string) (samples []AdmissionSample, present bool, err error)
```

- Path `filepath.Join(commonDir, "aira", "runs", "runs.db")` (ledger.go:66);
  stat-guard for absence; open via the existing `readOnlyHistoryDSN`
  (peak_rss_history.go:19 — `mode=ro&_pragma=query_only(ON)&_pragma=busy_timeout(0)`,
  reused in-package, **not** store's `OpenReadOnly` DSN which uses
  `busy_timeout(5000)`, Fable-noted). One query, handle closed immediately:
  `SELECT resource_signature, admission_reserve_basis, status, admission_reserve,
  peak_rss FROM runs WHERE admission_reserve_basis LIKE 'estimate%'`.

New store code (`internal/store/admission_insight.go`):

```go
// pure: no DB, no I/O — the entire honesty logic of §2.1/§2.2, exhaustively
// table-testable with hand-built samples.
func classifyAdmissionAdequacy(samples []runner.AdmissionSample, present bool) GaugeResult

func computeAdmissionReserveAdequacy(s *Store) (GaugeResult, error) {
    samples, present, err := runner.EstimateAdmissionSamples(context.Background(), s.commonDir)
    if err != nil { return unevaluated…("run history unreadable: "+code), nil } // never propagate
    return classifyAdmissionAdequacy(samples, present), nil
}
```

Registration (`internal/store/insights.go`): add `{Name:
"admission-reserve-adequacy", Title: "Admission reserve vs observed peak RSS",
Kind: GaugeKindRatio}` to `insightRegistry` + its `init()` Compute wiring. It
then flows through `ComputeAllGauges` automatically (no verb / descriptor / MCP
schema change beyond the data-driven registry row).

## 5. Faces

Nothing bespoke — the gauge is a registry row: `insights ls` lists it,
`insights show [name]` computes it, the TUI gauges panel iterates
`ComputeAllGauges` and renders it, MCP `aira_insights` dispatches it.

## 6. Testing — honesty-discriminating, each proven red

Split to match the layering, which also makes the load-bearing logic pure:

**A. `internal/store/admission_insight_test.go` (package store) — the pure
classifier, exhaustive table tests** over hand-built `[]runner.AdmissionSample`
(no DB). Each case proven red against the named wrong impl:

1. nil-peak, non-OOM estimate run → `missing_peak`, excluded from rate (red vs
   `COALESCE(peak,0)` counting it adequate). **Primary invariant.**
2. **nil-peak, oom-killed** estimate run → `inadequate` + `oom_killed`; a
   population of {this} ⇒ adequacy_rate 0.0, NOT Unevaluated and NOT 100 %
   (red vs v1 which excluded nil-peak before counting OOM — Sol P0-1).
3. peak `> reserve`, non-OOM → `inadequate`, margin `shortfall` bucket (red vs
   `>=` / reversed comparison).
4. peak `== reserve`, non-OOM → `adequate` (red vs strict `<`).
5. `estimate:capped`, non-OOM → `capped`, excluded; a capped-only population ⇒
   Unevaluated (evaluable 0), not 100 % (red vs counting capped adequate — Sol
   r1 P1-3).
6. `invalid_reserve` (nil/0 reserve) and `invalid_peak` (≤0 peak) each land in
   their own count, not `missing_peak` (red vs one lumped bucket — Sol r1 P1-1).
7. `ESTIMATE:...` / `estimated-x` / `estimate:max=x,n=5,f=115` (nonnumeric) /
   `estimate:max=1,n=2` (truncated) / `estimate:oom:max=1,n=2,f=115` (missing
   `oom=`) / trailing-suffix forms → `malformed_basis`, not evaluable (red vs an
   unanchored prefix/`LIKE 'estimate%'` match — Sol r2 P1). Assert the three
   canonical forms parse.
8. **OOM is orthogonal-and-first** (Sol r2 P0): a `estimate:capped` run that
   `oom-killed`, and an `invalid_reserve` estimate run that `oom-killed`, each →
   `inadequate` (+ `oom_killed`, capped one also `capped_oom`), NOT `capped` /
   `invalid_reserve`; a population of only these ⇒ adequacy_rate 0.0, never 100 %
   or Unevaluated (red vs classifying capped/invalid before OOM).
9. **truncated non-exited** (Fable P1): a `killed`/`cancelled`/`lost` run with
   peak `<= reserve`, non-OOM → `truncated_inconclusive`, excluded; a population
   of only such runs ⇒ Unevaluated, and one added to an otherwise-adequate set
   must NOT raise the rate (red vs counting peak≤reserve adequate for any
   status). A `killed` run with peak `> reserve` → `inadequate` (conclusive).
   A `starting`/`running` row → `nonterminal`, excluded.
10. `fallback:*` / `disabled:*` / `""` never reach the classifier (query filters
    them) — a defensive test that a stray fallback row is ignored.
11. NULL signature (absent) → `"(signature absent)"` bucket, kept distinct from a present-empty `""` and a literal `"(unsigned)"` run name; mixed NULL/non-NULL
    signatures keep independent cells (Sol r2 P1 / Fable P2).
12. per-signature separation: two signatures keep independent adequacy cells.
13. mixed population (Sol r1 P1-5): valid+invalid reserve, unknown/near-valid
    basis, ±peak, OOM-with-present-peak-below-reserve (still inadequate), capped,
    capped-OOM, killed-under-reserve, exact margin boundaries `1.0`/`1.25`/`2.0`
    — asserts the full Fields partition (sums to `candidate`) + Breakdown +
    margin histogram at once.
14. zero-evaluable **reason is neutral** (Sol r2 P1): a capped-only / malformed-
    only / invalid-reserve-only population ⇒ Unevaluated with reason "no eligible
    evaluable runs — see exclusion counts" (red vs a "no captured peak, no OOM"
    reason those counts would contradict); an empty candidate ⇒ "no estimate-mode
    runs recorded".
15. empty population → Unevaluated, `Value` nil, `Universe.Count` 0, `Scope`
    set, `AsOf` `{}` (non-nil), `At` set (conforms to insights_test.go's
    empty-universe invariant).

**B. `internal/runner/estimate_actual_test.go` (package runner) — the DB
reader**, seeding `runs.db` via the existing `ledger.append`+`project` harness
(reused from peak_rss_history_test.go; only reachable from package runner —
Fable P1 on §7):

- estimate rows with nil peak / present peak / oom-killed / capped / killed are
  returned; fallback/disabled/NULL-basis rows are excluded by the query.
- a row with a NULL `resource_signature` is returned with `Signature==""` (scan
  via `sql.NullString`, not a scan error blanking the whole read — Sol r2 P1).
- `runs.db` absent → `present=false`, nil samples, nil err.
- `runs.db` present but the `runs` table missing, or a required column missing,
  or a non-db/garbage file → `present=true`, err (caller degrades — Sol r2 P2).
- `SQLITE_BUSY`/locked → the `busy_timeout(0)` open fails fast, err (does not
  block the daemon read path — Sol r2 P2).
- read-only proof: assert `readOnlyHistoryDSN` carries `mode=ro` +
  `busy_timeout(0)` and the main `runs.db` size is unchanged after the read.
  **Do not** assert `-shm` absence (a mode=ro WAL reader may create one — the
  WAL-invalid mistake from #50's confirm round).

**B2. store-level degradation** (`admission_insight_test.go`): a
`computeAdmissionReserveAdequacy` over a scope whose reader returns an error →
`GaugeResult` Unevaluated with a reason and a **nil** compute error (never
propagate; Sol r2 P2). Seed by pointing the scope's common-dir at a garbage
`runs.db`.

**C. Registry**: bump `internal/store/insights_test.go:33` from `!= 9` to `!=
10` (the only count-pin; Fable P2 — core/insights_test.go self-derives, the TUI
smoke fabricates payloads). Confirm the new gauge is in
`InsightGauges()`/`GaugeRegistryRows()` and that `ComputeAllGauges` returns it
Unevaluated (not erroring) on an empty scope.

## 7. Risks

- **runs.db read on the daemon read path** — one stat-guarded, `busy_timeout(0)`,
  read-only aggregate per gauge computation; bounded, non-blocking.
- **schema knowledge** now lives only in the runner package that owns `runs.db`;
  a column change is a same-package edit (and AIRA is not live).
- **capped inflation / OOM-masking (incl. capped-OOM order) / nil-as-zero /
  truncated-peak false-pass** — all closed by §2.1 (OOM orthogonal-and-first;
  adequate requires `exited`) and pinned by tests A2/A5/A8/A9.
- **mis-framing as OOM protection** — closed by the §2 semantics statement, the
  retitle, and the `semantics` field.

## 8. Two-loop plan

Plan review COMPLETE: Sol r1 (2 P0 + 5 P1 + 1 P2) + Fable r1 (2 P1 + 1 P2) →
v2; Sol r2 (1 P0 + 3 P1 + 1 P2) + Fable re-gate (1 P1 + 1 P2) → v3. A final Sol
r3 confirms the v3 folds. Then: Terra build (TDD, self-review) → Sol build-review
(false-fail and false-pass) → Sol confirm → Opus real-HW verify
(build/vet/`CGO_ENABLED=0`/test ×2/`-race` all green, discriminating tests
proven red) → merge to master (fast-forward, prune worktree).
