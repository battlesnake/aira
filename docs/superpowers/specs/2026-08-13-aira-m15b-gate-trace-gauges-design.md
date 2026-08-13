# M15b — gate + traceability insight gauges (`ratchet-status`, `traceability-status`)

Status: PLAN v2 (incorporates Sol plan-review round 1: P0 + 3×P1). Awaiting Sol re-review → gate → build.
Milestone: Phase 4, follows M15 (insights framework). Branch `codex-aira-m15b` off master `ccb7325`.
Depends on: M15 (gauge framework), M13b (ratchet gate), M9c (covers/verifies traceability).

## 0. Premise and the key finding

M15 shipped the insights gauge framework and six live-query gauges, and **deferred**
`ratchet-status` + `traceability-status` with the recorded blocker: *"need a pure read-only
gate/traceability evaluator seam — GateCheck reads the stored projection, RunGate mutates,
check reconciles."*

**Code-map finding that de-risks this milestone: the pure read-only evaluators ALREADY EXIST.**

- **Ratchet:** `func (s *Store) evaluateRatchet(ctx, def gate.GateDefinition, root string)
  (DimensionEvaluation, error)` (`internal/store/gate_ratchet.go:111`) is **already
  side-effect-free** — `ResolveGateBaseline` opens the audit **non-writable**
  (`OpenGateAudit(s.commonDir, false)`, gate_ratchet.go:404-409; Sol-confirmed `key(false)`
  cannot mint, and read-only open avoids `MkdirAll`); `loadAllTestReports`/`flakyExclusions`
  are reads. The audit append + HMAC-key mint happen only in **callers** (`RunGate`,
  `AttestGate`, `PinGateBaseline`), never in the evaluator.
- **Traceability:** `checkTraceability` (`traceability.go:244`) and the shared primitives
  `captureTraceSnapshot`/`parseTraceabilitySnapshot`/`resolveTraceabilityEdges`
  (traceability.go:54/157/340) are pure over persistent state (git `ls-files --cached` snapshot
  + file reads; they mutate only an in-memory `*CheckReport`).

So **M15b is not a seam refactor** — it is two gauges over these existing pure evaluators, plus
the honesty layer (universe, per-source watermark, unevaluated discipline, exhaustive closed
classification, and the load-bearing *a-gauge-never-mints-gate-trust* guarantee).

## 1. Scope

**IN:**
1. Gauge `ratchet-status` — the **live** distribution of ratchet-KIND gate verdicts.
2. Gauge `traceability-status` — the **live** distribution of requirement coverage statuses,
   **matching `aira check` semantics** (project-wide, multi-worktree).
3. The read-only invariant + its load-bearing test (a **gauge** NEVER mints the HMAC key, NEVER
   appends to the gate audit ledger, NEVER writes durable state).
4. Universe + per-source watermark (`as_of`) + `unevaluated` honesty per gauge.
5. Exhaustive, closed status classification over BOTH the returned `DimensionEvaluation.Code`
   AND any returned error's stable code — no verdict/error ever silently counted as pass.
6. Drilldown that reproduces the value.
7. Registration in `insightRegistry` (faces `insights ls|show`, MCP schema, agent guide are
   generic over the registry — the two gauges auto-appear; a parity/coverage assertion confirms it).
8. Real-binary e2e including real `git-common-dir` verification of the read-only guarantee.

**OUT (written-down deferrals, reviewer-accepted):**
- `command`/`dimension` gate-status gauges — running a command gate has side effects + real cost;
  not gauge-safe. Only pure, cheap evaluators (ratchet, traceability) back gauges. (D1)
- A stored-attestation view (per-gate last-passed audit `Seq`) — the auditable "what was attested"
  companion to the live "what is true now" gauge. (D2)
- Remaining §17 gauges (estimate-vs-actual, master-red-duration, collision). (D3)

## 2. Design

Both gauges are `GaugeKind = distribution`, register as `Gauge{Name,Title,Kind}` in
`insightRegistry` (insights.go:98-105) with `Compute` wired by name in `init()` (:107-124), and
return `GaugeResult{Value, Breakdown, Unevaluated, UnevaluatedReason, Universe, Drilldown}` like
`computeFlakyRate` (insights.go:309). `Drilldown` is `GaugeDrilldown{Verb, Query}` (insights.go:31)
— use those exact fields (not free-text). Helpers available: `gaugeUniverse`, `unevaluatedGauge`,
`gaugeCellUnevaluated`, `insightScanID` (insights.go:74-88,189).

### 2.1 `ratchet-status`

- **Enumerate** gate definitions via the enumerator the gate code uses (`s.ListGates()`
  gate_index.go:111, or `discoverGates()` if a digest is needed); filter `def.Kind ==
  gate.KindRatchet`.
- **Evaluate each** with `s.evaluateRatchet(ctx, def, s.root)` — the **same root `RunGate` uses**
  (`s.root`, populated by `store.Open`/app wiring, Sol-confirmed) — yielding `(eval
  DimensionEvaluation, err error)`.
- **Classify (R6, closed set + error paths).** Follow `RunGate`'s tolerate-if-unevaluated
  pattern: an `err` is tolerated when `eval.Predicate == Unevaluated` (use `eval.Code`); otherwise
  classify by the **stable code** of `err` (`ErrorCode(err)`). Classification is over the union of
  `eval.Code` and any returned error code:
  | bucket | condition |
  |---|---|
  | `pass` | `eval.Predicate == Pass`, no error |
  | `regressed` | `E_GATE_RATCHET_REGRESSED` |
  | `baseline_missing` | `U_GATE_BASELINE_MISSING` |
  | `incomparable` | `U_GATE_INCOMPARABLE` |
  | `proof_stale` | `U_GATE_PROOF_STALE` |
  | `evidence_unavailable` | `U_GATE_EVIDENCE_UNAVAILABLE` |
  | `invalid` | `E_GATE_INVALID`, `E_GATE_BASELINE_INVALID` |
  | `corrupt` | `E_JOURNAL_CORRUPT` (from read-only audit verification, gate_audit.go:355-366 via ResolveGateBaseline) |
  | `unclassified` | ANY other code (returned error OR eval code) — **carries the raw stable code**, never `pass` |
  - One gate's error/corrupt status NEVER aborts the gauge and NEVER fails/passes the other gates —
    it is that gate's own cell bucket. A returned error with an empty/unusable eval → classify by
    `ErrorCode(err)` into `corrupt`/`invalid`/`unclassified` as above.
- **Value** = distribution `map[bucket]int` over the ratchet gates. **Breakdown** = per-gate
  `GaugeCell` keyed by gate ID: `{Value: bucket, Fields: {code, tracked_worktree_digest,
  baseline_seq?}}`. A per-gate unevaluated STATUS is a **real bucket**, not gauge-level unevaluated.
- **Universe:** `Count` = number of ratchet gate defs; `Scope = "project"` (gate set + baseline
  ledger are common-dir/project scoped; evaluation is against the current worktree's tracked tree —
  made explicit by `tracked_worktree_digest`); `AsOf = {gate_audit_seq: <ledger head Seq, read-only;
  absent if no ledger>, test_report_at_seq: <MAX; absent if none>, tracked_worktree_digest:
  digestEvaluationRoot(s.root)}`.
- **Unevaluated (R2):** **zero ratchet gates** ⇒ gauge `Unevaluated=true`, reason `"no ratchet
  gates configured"` (empty universe; never a fake `0`/empty distribution). ≥1 gate ⇒ gauge
  evaluated (buckets carry per-gate statuses).
- **Watermark honesty (R4):** `gate_audit_seq` is the **monotone** append-only ledger head
  (`gateAuditHead.Seq`, gate_audit.go:39) via `OpenGateAudit(commonDir, false)`;
  `test_report_at_seq` is monotone SQLite `test_reports.at_seq`. Per-source; **no cross-source
  atomicity claimed**.
- **Read-only guarantee (R1 — load-bearing):** the entire ratchet path is side-effect-free (all
  audit opens are `OpenGateAudit(_, false)`). See R1 for the precise, corrected invariant.

### 2.2 `traceability-status`

- **Match `aira check`.** `checkTraceability` (traceability.go:244-289) discovers and scans ALL
  registered/discovered worktrees; `EvaluateDimension(root)` scans only one root — so a single-root
  helper could NOT reproduce `check` (Sol P1). The gauge therefore uses the **same multi-worktree
  computation** `aira check` uses, via a shared seam:
  - Extract `func (s *Store) traceabilityReport() (*CheckReport, error)` (or equivalent) — the
    multi-worktree scan+resolve core that `checkTraceability` currently performs inline — and have
    BOTH `checkTraceability` (writing into the caller's report) and the gauge call it. The gauge
    runs it into its own throwaway `*CheckReport` and derives the distribution from that report's
    **stable-coded** findings + the requirement universe. Reading stable codes (`W_TRACE_UNCOVERED`,
    `W_TRACE_UNVERIFIED`, `E_TRACE_DANGLING`, `U_TRACE_*`) is acceptable because those are AIRA's
    contract AND the identical scan guarantees parity with `check` by construction. (A pure
    per-requirement `traceabilityStatuses()` returning statuses directly is an acceptable
    alternative if the builder finds it cleaner and keeps the shared-code / no-drift property.)
- **Per-requirement buckets (R6, closed set), faithful to traceability.go's real classification:**
  | bucket | source condition |
  |---|---|
  | `covered_verified` | built requirement with a `covers:` AND a `verifies:` (no W_TRACE_* for it) |
  | `unverified` | `W_TRACE_UNVERIFIED` (traceability.go:370): built, covered, not verified |
  | `uncovered` | `W_TRACE_UNCOVERED` (traceability.go:367/374): built/partial, not covered |
  | `not_built` | requirement not in a *built* status (no coverage expectation) — match the real status gate |
  | `unevaluated` | requirement node malformed (`E_REQUIREMENT_INVALID`, traceability.go:313) or its edges `U_TRACE_*` — real per-item bucket |
- **`dangling` count** (top-level `Value`/`Fields` field, not a per-requirement bucket):
  `E_TRACE_DANGLING` (traceability.go:349) annotations pointing at an absent requirement.
- **Value** = distribution `map[bucket]int` (+ `dangling` count in `Fields`); **Breakdown** =
  per-requirement `GaugeCell` keyed by requirement ID (`{Value: bucket}`).
- **Universe:** `Count` = number of requirements across the scanned worktrees; `Scope = "project"`
  (matches `check`'s multi-worktree discovery); `AsOf = {trace_scan: insightScanID()}` (+ optionally
  a per-worktree tracked digest). No sequence.
- **Watermark honesty (R4):** traceability has **NO monotone watermark** (a live snapshot). `as_of`
  carries ONLY the wall-clock scan marker `insightScanID()` (+ optional tracked digests) and
  **fabricates no sequence**. No cross-call monotonicity claimed.
- **Unevaluated (R2/R3):** empty requirement registry (`U_TRACE_EMPTY`, gate_eval.go:120 /
  traceability.go:327) ⇒ gauge `Unevaluated`, reason `"no requirements"`. Torn snapshot / unreadable
  registry (`U_TRACE_UNSCANNED`) ⇒ gauge `Unevaluated`, reason cites the tear — **never** a
  partial/fake distribution.
- **Read-only:** pure scan; writes nothing.

### 2.3 Drilldown reproduces the value (R-drill)

Both universes are small and fully enumerated in the breakdown — no default `ls` cap to escape.
Each gauge carries a `GaugeDrilldown{Verb, Query}` pointer for auditability:

- `ratchet-status`: `Drilldown{Verb: "gate", Query: "check"}`. **Honest caveat (R7), documented in
  the gauge doc:** the gauge is **live** (`evaluateRatchet`), whereas `aira gate check` is the
  **stored+reconciled** attestation view; they can differ when no `gate run` has occurred since the
  gate/baseline/HEAD last changed. The gauge answers *"what is each ratchet gate's verdict right
  now?"*; each cell carries `baseline_seq` + `tracked_worktree_digest` to independently recompute.
- `traceability-status`: `Drilldown{Verb: "check", Query: ""}`. Because the gauge runs the identical
  multi-worktree computation as `aira check`, `check`'s traceability dimension reproduces the
  per-requirement statuses exactly (R9 drift test proves this, incl. a sibling-worktree fixture).

## 3. §1b — resolutions (round 1 incorporated)

- **R1 (read-only is HARD — CORRECTED per Sol P0).** The invariant is: **a gauge never mints the
  HMAC key, never appends to the gate audit ledger, never writes durable state.** The audit chain
  grows ONLY via authenticated gate **operations** — `RunGate`, `AttestGate`, `PinGateBaseline` (all
  open `OpenGateAudit(_, true)` → `Append` → `key(true)`, gate_ratchet.go:224 / gate_eval.go:138,269
  / gate_audit.go:169-180). `ratchet-status` uses only `OpenGateAudit(_, false)` paths. The
  load-bearing test: compute each gauge on a fresh git project on which **no gate operation has been
  performed** (no run, no attest, no baseline pin) → assert **no** file under
  `$(git rev-parse --git-common-dir)/aira/gates/` (no `hmac.key`, no audit ledger) and SQLite
  unchanged. (The earlier "only gate run mints" phrasing was wrong — `PinGateBaseline` also mints;
  no M13b change is needed, only this corrected invariant + test framing.)
- **R2 (empty universe → gauge-unevaluated, not fake 0).** Zero ratchet gates / empty requirement
  registry ⇒ gauge-level `Unevaluated`. Per-item unevaluated statuses are real buckets inside an
  evaluated gauge. (Consistent with existing gauges, insights.go:257-259/373-375.)
- **R3 (torn scan → unevaluated).** `U_TRACE_UNSCANNED` ⇒ gauge `Unevaluated` citing the tear.
- **R4 (watermark honesty).** ratchet-status: monotone gate-audit `Seq` + `test_report_at_seq`.
  traceability-status: wall-clock scan marker only; no fabricated sequence.
- **R5 (scope — REVISED per Sol P1).** ratchet-status = `project`; traceability-status = `project`
  (multi-worktree, matching `aira check`) — NOT current-worktree as v1 had it.
- **R6 (exhaustive closed classification, incl. error paths — EXPANDED per Sol P1).** Every code the
  evaluators can emit — including `evaluateRatchet`'s **returned error** codes (`E_JOURNAL_CORRUPT`
  from read-only audit verification; report-loading errors returned alongside `U_GATE_INCOMPARABLE`,
  gate_ratchet.go:135-138) — maps to exactly one named bucket; unrecognised codes → `unclassified`
  carrying the raw code. Never silently dropped, never miscounted as `pass`, never aborts the gauge.
- **R7 (live vs stored).** ratchet-status live-evaluates (`evaluateRatchet`), NOT the stored gate
  projection. Documented; `gate check` is the separate stored+reconciled view.
- **R8 (only pure evaluators are gauge-safe).** command/dimension gate-status excluded (D1).
- **R9 (no traceability drift — STRENGTHENED per Sol P1).** The gauge calls the SAME multi-worktree
  scan+resolve seam as `checkTraceability`; a drift test asserts the gauge's per-requirement statuses
  are consistent with `aira check`'s traceability findings for the same fixture, **including a
  sibling-worktree fixture** (so the multi-worktree universe is exercised, not just single-root).
- **R10 (digest naming — per Sol P1).** `digestEvaluationRoot` hashes the **tracked working-tree**
  contents of `git ls-files --cached` paths (gate_eval.go:63-78), NOT the committed HEAD tree. The
  `as_of` key is named `tracked_worktree_digest` and described accurately.

## 4. Tests

TDD. Regression-shaped where a naive impl would pass; property-shaped for invariants.

**Unit (`internal/store/insights_test.go`, + a traceability seam test):**
1. `ratchet-status` two gates (one `pass`, one `baseline_missing`) → `Value == {pass:1,
   baseline_missing:1}`, universe count 2 scope `project`, `as_of` has `gate_audit_seq` +
   `test_report_at_seq`; breakdown keyed by gate ID.
2. `ratchet-status` regressed (new failing test vs baseline) → `regressed`; coverage-drop →
   `regressed`.
3. `ratchet-status` zero ratchet gates → gauge `Unevaluated` ("no ratchet gates"), universe 0.
4. **`ratchet-status` READ-ONLY (load-bearing, R1):** fresh project, **no gate operation performed**;
   compute gauge; assert `hmac.key` and the audit ledger do NOT exist under the common-dir gates dir,
   SQLite unchanged.
5. **`ratchet-status` error-path (R6, discriminating):** induce a gate whose `ResolveGateBaseline`
   verification yields `E_JOURNAL_CORRUPT` (corrupt a baseline record's digest) → that gate's cell =
   `corrupt` carrying `E_JOURNAL_CORRUPT`; the gauge still returns and other gates are unaffected;
   assert it is NOT folded into `pass`. Also a code outside the closed set → `unclassified` carrying
   the raw code. (A naive `default: pass` classifier fails both.)
6. `traceability-status` fixture with covered_verified / unverified / uncovered / not_built + a
   dangling annotation → distribution + `dangling` count; universe = requirement count, scope
   `project`, `as_of` has `trace_scan` and **no** sequence key.
7. `traceability-status` empty registry → gauge `Unevaluated` ("no requirements").
8. `traceability-status` torn snapshot (induce `U_TRACE_UNSCANNED`) → gauge `Unevaluated` (reason
   cites tear), NOT a distribution.
9. **`traceability-status` drift/parity (R9):** gauge per-requirement statuses consistent with
   `aira check` traceability dimension for the same fixture, **including a sibling worktree** with
   its own requirements/annotations (exercises the multi-worktree universe).
10. enumeration: each gauge's breakdown cardinality == universe count.

**Real-binary e2e (`~/tmp/aira-m15b-e2e.sh`, committed reproducible):**
- fresh project: both gauges `Unevaluated` (no ratchet gates / no requirements) with universe + reason.
- **read-only at the binary level:** run `aira insights show ratchet-status` on a project on which
  no gate operation was ever performed → `$(git-common-dir)/aira/gates/` has no `hmac.key` and no
  audit ledger.
- define a ratchet gate + `gate baseline pin` + add test reports → `ratchet-status` `pass`; add a new
  failing test → `regressed`.
- add requirements + `covers:`/`verifies:` annotations across two worktrees → `traceability-status`
  distribution + `dangling` count for a dangling `covers:`; delete all requirements → gauge
  `Unevaluated`.

## 5. Files

- `internal/store/insights.go`: `computeRatchetStatus`, `computeTraceabilityStatus`; two
  `insightRegistry` entries; two `init()` wirings.
- `internal/store/traceability.go`: extract the shared multi-worktree traceability seam
  (`traceabilityReport()` or a pure `traceabilityStatuses()`) called by BOTH `checkTraceability` and
  the gauge (R9) — no forked covers/verifies logic; guard against regressing M9c `check` behaviour.
- `internal/store/insights_test.go` (+ traceability seam test): §4 unit tests.
- `docs/.../2026-08-13-aira-m15b-gate-trace-gauges-design.md` (this file).
- No faces edits (registry-generic). A parity/coverage assertion confirms both gauges appear in
  `insights ls`, the MCP schema, and the agent guide.

## 6. Risks / expected yield

1. **Read-only guarantee (highest).** A gauge that mints the HMAC key or appends to the gate audit
   ledger is a serious honesty + security violation — the authenticated chain must grow only via
   real gate operations. R1 test + Opus real-`git-common-dir` e2e are load-bearing (the Codex sandbox
   may not exercise the real common-dir key path — M10a lesson).
2. **Classifier exhaustiveness incl. error paths (R6).** An unmapped verdict/error counted as `pass`
   is a fake-pass. Closed mapping + `corrupt`/`unclassified` buckets + discriminating tests.
3. **Traceability parity/drift (R9).** Reuse the multi-worktree seam; a single-root helper silently
   under-reports vs `check`. Sibling-worktree fixture proves parity.
4. **Watermark honesty (R4) + digest naming (R10).** No fabricated sequence for traceability; the
   worktree digest is named for what it hashes.

## 7. Deferrals (written down)

- D1: command/dimension gate-status gauges (side-effectful evaluators).
- D2: stored-attestation (per-gate last-passed audit `Seq`) view.
- D3: estimate-vs-actual, master-red-duration, collision gauges (§17 remainder).
