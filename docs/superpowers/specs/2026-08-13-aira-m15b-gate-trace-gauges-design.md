# M15b — gate + traceability insight gauges (`ratchet-status`, `traceability-status`)

Status: PLAN (awaiting Sol plan-review → gate → build)
Milestone: Phase 4, follows M15 (insights framework). Branch `codex-aira-m15b` off master `ccb7325`.
Depends on: M15 (gauge framework), M13b (ratchet gate), M9c (covers/verifies traceability).

## 0. Premise and the key finding

M15 shipped the insights gauge framework and the six live-query gauges, and **deferred**
`ratchet-status` + `traceability-status` with the recorded blocker: *"need a pure read-only
gate/traceability evaluator seam — GateCheck reads the stored projection, RunGate mutates,
check reconciles."*

**Code-map finding that de-risks this milestone: the pure read-only evaluators ALREADY EXIST.**
The verdict computation is already structurally separated from the audit/proof mutation:

- **Ratchet:** `func (s *Store) evaluateRatchet(ctx, def gate.GateDefinition, root string)
  (DimensionEvaluation, error)` (`internal/store/gate_ratchet.go:111`) is **already
  side-effect-free** — it only reads (`ResolveGateBaseline` opens the audit **non-writable**;
  `loadAllTestReports` is a SQLite read; `flakyExclusions`/`computeFlakyTests` are reads). The
  audit append + HMAC-key mint happen only in the **caller** `RunGate` (gate_eval.go:138), never
  in the evaluator. Pure comparators `compareNoNewFailures`/`compareCoverage` (gate_ratchet.go:56/88).
- **Traceability:** `func EvaluateDimension(root, dimension string) (DimensionEvaluation, error)`
  (`gate_eval.go:81`), `checkTraceability` (`traceability.go:244`), and `resolveTraceabilityEdges`
  (`traceability.go:340`) are pure over persistent state (git `ls-files --cached` snapshot + file
  reads; they mutate only the in-memory `*CheckReport` handed in). Shared pure primitives:
  `scanTraceability` (traceability.go:46), `captureTraceSnapshot` (:54), `parseTraceabilitySnapshot`
  (:157), `trackedTracePaths` (:88).

So **M15b is not a refactor to create a seam** — it is two gauges that call these existing pure
evaluators, plus the honesty layer (universe, per-source watermark, unevaluated discipline,
exhaustive classification, and the load-bearing *never-mint-gate-trust* read-only guarantee).

## 1. Scope

**IN:**
1. Gauge `ratchet-status` — the **live** distribution of ratchet-KIND gate verdicts.
2. Gauge `traceability-status` — the **live** distribution of requirement coverage statuses.
3. The read-only invariant + its load-bearing test (a gauge NEVER mints the HMAC key, NEVER
   appends to the gate audit ledger, NEVER writes any durable state).
4. Universe + per-source watermark (`as_of`) + `unevaluated` honesty per gauge.
5. Exhaustive, closed status classification (no verdict silently counted as pass).
6. Drilldown that reproduces the value.
7. Registration in `insightRegistry` (faces `insights ls|show`, MCP schema, agent guide are
   generic over the registry — the two gauges auto-appear; a parity/coverage assertion confirms it).
8. Real-binary e2e including real `git-common-dir` verification of the read-only guarantee.

**OUT (written-down deferrals, reviewer-accepted):**
- `command`/`dimension` gate-status gauges — running a command gate has side effects + real cost;
  it is **not gauge-safe**. Only pure, cheap evaluators (ratchet, traceability) back gauges. (D1)
- A stored-attestation view (per-gate last-passed audit `Seq`) — the auditable "what was attested"
  companion to the live "what is true now" gauge. (D2)
- The remaining §17 gauges (estimate-vs-actual, master-red-duration, collision). (D3)

## 2. Design

Both gauges are `GaugeKind = distribution`, register as `Gauge{Name,Title,Kind}` in
`insightRegistry` (insights.go:98-105) with `Compute` wired by name in `init()` (:107-124), and
return `GaugeResult{Value, Breakdown, Unevaluated, UnevaluatedReason, Universe, Drilldown}`
exactly like `computeFlakyRate` (insights.go:309).

### 2.1 `ratchet-status`

- **Enumerate** gate definitions (the enumerator the existing gate code uses — `s.ListGates()`
  at gate_index.go:111, or `discoverGates()` if a digest is needed); filter `def.Kind ==
  gate.KindRatchet`.
- **Evaluate each** with `s.evaluateRatchet(ctx, def, s.root)` — the **same root `RunGate` uses**
  (`s.root`) — yielding `DimensionEvaluation{Predicate, Code}`.
- **Classify** `(Predicate, Code)` into exactly one closed bucket (R6):
  | bucket | condition |
  |---|---|
  | `pass` | `Predicate == Pass` |
  | `regressed` | `Code == E_GATE_RATCHET_REGRESSED` (Predicate Fail) |
  | `baseline_missing` | `U_GATE_BASELINE_MISSING` |
  | `incomparable` | `U_GATE_INCOMPARABLE` |
  | `proof_stale` | `U_GATE_PROOF_STALE` |
  | `evidence_unavailable` | `U_GATE_EVIDENCE_UNAVAILABLE` |
  | `invalid` | `E_GATE_INVALID` |
  | `unclassified` | any code not in the closed set above — **carries the raw code**, never counted as pass |
- **Value** = the distribution `map[bucket]int` over the ratchet gates.
- **Breakdown** = per-gate `GaugeCell` keyed by gate ID: `{Value: bucket, Fields: {code, root_digest,
  baseline_seq?}}`. A per-gate unevaluated STATUS (e.g. `baseline_missing`) is a **real bucket**,
  not gauge-level unevaluated.
- **Universe:** `Count` = number of ratchet gate defs; `Scope = "project"` (the gate set + baseline
  ledger are common-dir/project scoped — the evaluation is against the **current worktree's HEAD**,
  made explicit by `root_digest` in `as_of`); `AsOf = {gate_audit_seq: <ledger head Seq, read-only;
  absent if no ledger>, test_report_at_seq: <MAX; absent if none>, root_digest:
  digestEvaluationRoot(s.root)}`.
- **Unevaluated:** **zero ratchet gates** ⇒ gauge `Unevaluated=true`, reason `"no ratchet gates
  configured"` (empty universe — nothing to distribute; never a fake `0`/empty distribution). ≥1
  gate ⇒ gauge evaluated (buckets carry the per-gate statuses).
- **Watermark honesty (R4):** `gate_audit_seq` is the **monotone** append-only ledger head
  (`gateAuditHead.Seq`, gate_audit.go:39) obtained read-only via `OpenGateAudit(commonDir, false)`;
  `test_report_at_seq` is the monotone SQLite `test_reports.at_seq`. Per-source; **no cross-source
  atomicity claimed** (M15 discipline).
- **Read-only guarantee (R1 — load-bearing):** the entire path (`evaluateRatchet` →
  `ResolveGateBaseline` → `OpenGateAudit(commonDir, false)`) must **never** call `MkdirAll`/`key(true)`
  (mint) nor `Append`. Computing this gauge on a project that never ran a gate must create **no**
  `hmac.key`, **no** audit ledger, and mutate no SQLite. Verified by unit test + real-`git-common-dir`
  e2e.

### 2.2 `traceability-status`

- **Evaluate** the covers/verifies graph over `s.root`'s tracked-file snapshot, **reusing the exact
  primitives** `checkTraceability`/`EvaluateDimension` use — no re-implementation (R9). Preferred:
  extract a pure helper `traceabilityStatuses(root)` (in traceability.go) returning
  `(statuses map[reqID]string, dangling []string, unevaluated *TraceUnevaluated)`, and have BOTH
  `checkTraceability` and the gauge call it, so a future change to the covers/verifies rules updates
  the gauge automatically. Fallback (if extraction is too invasive): run the existing resolver against
  a throwaway `*CheckReport` and derive statuses from its findings + the requirement registry — but
  this couples to finding strings and is the less-preferred path; the plan mandates the shared helper
  unless the reviewer accepts the fallback.
- **Per-requirement buckets** over the requirement registry (R6, closed set):
  | bucket | condition |
  |---|---|
  | `covered_verified` | built requirement with **both** a `covers:` and a `verifies:` annotation |
  | `unverified` | built requirement, `covers:` present, **no** `verifies:` (the `W_TRACE_UNVERIFIED` condition) |
  | `uncovered` | built/partial requirement, **no** `covers:` (the `W_TRACE_UNCOVERED` condition) |
  | `not_built` | requirement not in a *built* status ⇒ no coverage expectation |
  | `unevaluated` | requirement node malformed (`E_REQUIREMENT_INVALID`) ⇒ real per-item unevaluated bucket |
- **`dangling` count** (top-level field, not a per-requirement bucket): annotations pointing at an
  **absent** requirement (the `E_TRACE_DANGLING` condition) — a code-side signal.
- **Value** = the distribution `map[bucket]int`; **Breakdown** = per-requirement `GaugeCell` keyed by
  requirement ID (`{Value: bucket}`); `Fields: {dangling: <n>}` on the result.
- **Universe:** `Count` = number of requirements in the registry; `Scope = "current-worktree"` (the
  `git ls-files --cached` scan is per-worktree — matches M15 findings/tickets scope); `AsOf =
  {trace_scan: insightScanID(), root_digest: <HEAD tree digest>}`.
- **Watermark honesty (R4):** traceability has **NO monotone watermark** (a live snapshot). `as_of`
  carries **only** a wall-clock scan marker (`insightScanID()`, the same shape used by the findings
  gauges) + the root digest, and **must not fabricate a sequence**. No cross-call monotonicity claimed.
- **Unevaluated (R2/R3):** empty registry (`U_TRACE_EMPTY`) ⇒ gauge `Unevaluated`, reason `"no
  requirements"`. Torn snapshot / unreadable registry (`U_TRACE_UNSCANNED`) ⇒ gauge `Unevaluated`,
  reason cites the tear — **never** a partial/fake distribution computed from a torn scan.
- **Read-only:** pure scan; writes nothing.

### 2.3 Drilldown reproduces the value (R-drill)

For both gauges the **universe is small and the breakdown enumerates it in full** — there is no
default `ls` cap to escape (the M15 "uncapped read" concern applied to gauges that aggregate many
rows past the cap: findings, spend, flaky cells). So the distribution is *self-reproducing* from the
breakdown. Each gauge still carries a `Drilldown` pointer for auditability:

- `ratchet-status`: `Drilldown{Command: "aira gate check", Note: "per-gate live verdicts; each cell
  carries baseline_seq + root_digest to recompute"}`. **Honest caveat, stated in the gauge doc and
  the plan:** the gauge is **live** (`evaluateRatchet`), whereas `aira gate check` is the
  **stored+reconciled** attestation view; they can differ when no `gate run` has occurred since the
  gate/baseline/HEAD last changed. The gauge answers *"what is each ratchet gate's verdict right
  now?"*; the stored view is the separate auditable attestation. This is deliberate (R7).
- `traceability-status`: `Drilldown{Command: "aira check", Note: "traceability dimension; same
  resolver → same per-requirement status"}`. `aira check` uses the identical resolver (R9), so it
  reproduces the per-requirement statuses exactly.

## 3. §1b — pre-empted resolutions (anticipating Sol)

- **R1 (read-only is HARD).** Both gauges are provably side-effect-free. The load-bearing test:
  compute each gauge on a fresh git project that never ran a gate → assert **no** file under
  `$(git rev-parse --git-common-dir)/aira/gates/` (no `hmac.key`, no audit ledger), and SQLite is
  unchanged. The gauge reads the already-opened store's current projection; it does **not** trigger
  a `Rebuild` (Open already reconciled) and it does **not** open the audit writable. Confirm
  `evaluateRatchet`/`ResolveGateBaseline` use `OpenGateAudit(_, false)`.
- **R2 (empty universe → gauge-unevaluated, not fake 0).** Zero ratchet gates / empty requirement
  registry ⇒ gauge-level `Unevaluated`. Per-item unevaluated statuses are real buckets inside an
  evaluated gauge (M15 present-bucket-count discipline).
- **R3 (torn scan → unevaluated).** A `U_TRACE_UNSCANNED` snapshot tear yields gauge `Unevaluated`
  citing the tear — never a distribution over a torn scan.
- **R4 (watermark honesty).** ratchet-status carries the monotone gate-audit ledger head `Seq` +
  `test_report_at_seq`; traceability-status carries ONLY a wall-clock scan marker + root digest and
  fabricates no sequence. Per-source; documented.
- **R5 (scope).** ratchet-status = `project`; traceability-status = `current-worktree`. Matches M15.
- **R6 (exhaustive closed classification).** Every code the evaluators can emit maps to exactly one
  named bucket; an unrecognised code → an explicit `unclassified` bucket carrying the raw code —
  never silently dropped, never miscounted as `pass`. This is honesty-first "no fake pass" at the
  classifier.
- **R7 (live vs stored).** ratchet-status live-evaluates (`evaluateRatchet`), NOT the stored gate
  projection (which can be stale). Documented; `gate check` is the separate stored+reconciled view.
- **R8 (only pure evaluators are gauge-safe).** command/dimension gate-status excluded (side effects,
  cost). Written-down deferral D1.
- **R9 (no traceability drift).** The gauge calls the same scan/resolve primitives as
  `checkTraceability`/`EvaluateDimension` (shared helper), asserted by a test that the gauge's
  per-requirement statuses equal `aira check`'s traceability findings for the same fixture.

## 4. Tests

TDD. Regression-shaped where a wrong implementation would pass a naive test; property-shaped for
invariants.

**Unit (`internal/store/insights_test.go`, plus a small `traceability_test.go` helper test):**
1. `ratchet-status` two gates (one `pass`, one `baseline_missing`) → `Value == {pass:1,
   baseline_missing:1}`, universe count 2 scope `project`, `as_of` has `gate_audit_seq` +
   `test_report_at_seq`; breakdown keyed by gate ID.
2. `ratchet-status` regressed (new failing test vs baseline) → bucket `regressed`; and coverage-drop
   → `regressed`.
3. `ratchet-status` zero ratchet gates → gauge `Unevaluated` (reason "no ratchet gates"), universe 0.
4. **`ratchet-status` READ-ONLY (load-bearing):** fresh project, no gate run; compute gauge; assert
   `hmac.key` and the audit ledger do NOT exist under the common-dir gates dir afterwards.
5. `ratchet-status` `unclassified`: force an evaluation returning a code outside the closed set →
   `unclassified` bucket carries that code (asserts it is NOT folded into `pass`). (Discriminating —
   a naive `default: pass` classifier fails this.)
6. `traceability-status` fixture with covered_verified / unverified / uncovered / not_built + a
   dangling annotation → distribution + `dangling` count; universe = requirement count, scope
   `current-worktree`, `as_of` has `trace_scan` + `root_digest` and **no** sequence key.
7. `traceability-status` empty registry → gauge `Unevaluated` (no requirements).
8. `traceability-status` torn snapshot (induce `U_TRACE_UNSCANNED`) → gauge `Unevaluated` (reason
   cites tear), NOT a distribution.
9. `traceability-status` drift guard (R9): the gauge's per-requirement statuses equal the
   `aira check` traceability dimension outcome for the same fixture (shared resolver).
10. drilldown/enumeration: each gauge's breakdown cardinality == universe count (full enumeration).

**Real-binary e2e (`~/tmp/aira-m15b-e2e.sh`, committed reproducible):**
- fresh project: both gauges `Unevaluated` with universe + reason.
- define a ratchet gate + `gate baseline pin` + add test reports → `ratchet-status` `pass`; add a
  new failing test → `regressed`; **assert the GAUGE minted no `hmac.key`** (only an explicit
  `gate run` does), via a project where the gauge is computed but no gate was run.
- add requirements + `covers:`/`verifies:` annotations → `traceability-status` distribution; a
  dangling `covers:` → `dangling` count; delete all requirements → gauge `Unevaluated`.

## 5. Files

- `internal/store/insights.go`: `computeRatchetStatus`, `computeTraceabilityStatus`; two
  `insightRegistry` entries; two `init()` wirings.
- `internal/store/traceability.go`: extract pure `traceabilityStatuses(...)` shared by
  `checkTraceability` and the gauge (R9).
- `internal/store/insights_test.go` (+ traceability helper test): §4 unit tests.
- `docs/superpowers/specs/2026-08-13-aira-m15b-gate-trace-gauges-design.md` (this file).
- No faces edits (registry-generic). A parity/coverage assertion confirms both gauges appear in
  `insights ls`, the MCP schema, and the agent guide.

## 6. Risks / expected yield

1. **Read-only guarantee (highest).** A gauge that mints the HMAC key or appends to the gate audit
   ledger is a serious honesty + security violation — the authenticated audit chain must grow ONLY
   via real gate runs. R1 test + Opus real-`git-common-dir` e2e are load-bearing (the Codex sandbox
   may not exercise the real common-dir key path — the M10a lesson).
2. **Classifier exhaustiveness (R6).** An unmapped verdict counted as `pass` is a fake-pass. Closed
   mapping + `unclassified` bucket + discriminating test.
3. **Watermark honesty (R4).** Fabricating a sequence for traceability would be dishonest; scan
   marker only.
4. **Traceability drift (R9).** Reuse the resolver; do not fork covers/verifies logic.

## 7. Deferrals (written down)

- D1: command/dimension gate-status gauges (side-effectful evaluators).
- D2: stored-attestation (per-gate last-passed audit `Seq`) view.
- D3: estimate-vs-actual, master-red-duration, collision gauges (§17 remainder).
