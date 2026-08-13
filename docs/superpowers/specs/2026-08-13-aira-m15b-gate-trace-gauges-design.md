# M15b — gate + traceability insight gauges (`ratchet-status`, `traceability-status`)

Status: APPROVED (Sol plan-review, 5 rounds → APPROVE-PLAN on v6). Building.
v-history: r1 P0+3×P1 (read-only invariant, error codes, trace scope, digest naming); r2 4×P1
(precedence, error paths, findings-not-faithful, malformed vs scan-tear); r3 confirmed R6 precedence +
buckets closed + seam lossiness; r4 confirmed edge+valid-node sufficient + malformed-node payload gaps;
r5 the ID-less-malformed diagnostic compat. Now implementing via TDD; Sol build-review → Opus
real-common-dir e2e → merge.
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
- **Classify (R6, closed set + explicit precedence).** The precedence is grounded in
  `evaluateRatchet`'s actual returns (gate_ratchet.go:111-203): **it sets `eval.Predicate` on every
  path EXCEPT line 128** (a `ResolveGateBaseline` error that is not `U_GATE_BASELINE_MISSING` — i.e.
  `E_JOURNAL_CORRUPT`/audit-IO — returns a bare error with empty `Predicate`). The report-load error
  (line 137) and unknown-comparator (line 199) both **set** `Predicate` (masking the underlying error
  into `U_GATE_INCOMPARABLE`/`E_GATE_INVALID`, exactly as `RunGate` tolerates them). So:
  > **Rule:** classify by `eval.Code` whenever `eval.Predicate != ""` (Pass/Fail/Unevaluated are all
  > usable). ONLY when `eval.Predicate == ""` AND `err != nil` classify by `ErrorCode(err)`.

  | bucket | condition |
  |---|---|
  | `pass` | `eval.Predicate == Pass` |
  | `regressed` | `eval.Predicate == Fail` (Code `E_GATE_RATCHET_REGRESSED`, gate_ratchet.go:83/106) |
  | `baseline_missing` | `eval.Code == U_GATE_BASELINE_MISSING` |
  | `incomparable` | `eval.Code == U_GATE_INCOMPARABLE` (also masks a report-load `E_INTERNAL`, line 137 — deliberate, matches `gate run`) |
  | `proof_stale` | `eval.Code == U_GATE_PROOF_STALE` |
  | `evidence_unavailable` | `eval.Code == U_GATE_EVIDENCE_UNAVAILABLE` |
  | `invalid` | `eval.Code == E_GATE_INVALID` |
  | `corrupt` | bare error (`Predicate==""`), `ErrorCode(err) == E_JOURNAL_CORRUPT` |
  | `unclassified` | any other `eval.Code`, OR bare-error `ErrorCode(err)` not above (incl. `E_INTERNAL` from a non-prefixed audit/IO error) — **carries the raw stable code**, never `pass` |
  - `E_GATE_BASELINE_INVALID` is NOT reachable from `evaluateRatchet`/`ResolveGateBaseline` (baseline
    decode errors wrap to `U_GATE_BASELINE_MISSING`, gate_ratchet.go:434-440; it is emitted only by
    baseline pin/derivation, :297-390). It is intentionally omitted from the closed set; if it ever
    surfaces it lands in `unclassified` carrying its code (defensive, documented-unreachable-today).
  - One gate's error/corrupt status NEVER aborts the gauge and NEVER fails/passes the other gates —
    it is that gate's own cell bucket.
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

- **Match `aira check` via a PURE STATUS SEAM (mandatory — Sol P1c/P1d).** `checkTraceability`
  (traceability.go:244-337) scans ALL discovered worktrees; `EvaluateDimension(root)` scans one, so
  single-root can't reproduce `check`. AND a findings-only derivation is **not faithful**: no finding
  carries a requirement's lifecycle status, so `not_built` needs `traceRequirement.status` directly
  (traceability.go:364), and a covered non-built requirement (which emits no warning) would be
  indistinguishable from `covered_verified`. Therefore extract a **pure scan-core** that returns
  **LOSSLESS ORDERED raw data** (Sol P1: a digested shape would stop `checkTraceability` reconstructing
  its exact findings and would lose malformed nodes whose filename yields no ID) — shared by BOTH
  `checkTraceability` and the gauge:
  ```
  // One per malformed requirement FILE (unique Subject). IDs is the ordered alias set used for edge
  // resolution: {filename-derived ID if the filename is a valid ID} ∪ {frontmatter requirement.ID if
  // the frontmatter parsed}. May be empty (invalid filename AND parse error) — the node still counts
  // in the gauge universe by Subject. Message is the exact E_REQUIREMENT_INVALID finding text.
  type malformedNode struct { Subject string; IDs []string; Message string }
  // Carries the exact diagnostic so checkTraceability reproduces the finding verbatim.
  type traceUnevaluated struct { Code string; Subject string; Message string }
  type traceScan struct {
      edges        []traceEdge                 // ORDERED raw (path,line,kind,id) — lossless for check findings
      requirements map[string]traceRequirement // valid nodes: id -> {status, path}
      malformed    []malformedNode             // ORDERED, one per file (unique Subject), collapse-free
      unevaluated  *traceUnevaluated           // genuine scan failure / empty registry ONLY (NOT all-malformed)
  }
  func (s *Store) scanTraceabilityGraph() (traceScan, error)
  ```
  It runs the identical registry-read + `discoverWorktrees` + `captureTraceSnapshot` +
  `parseTraceabilitySnapshot` scan. The exact insertion contract (matching traceability.go:306-320):
  `Message` = `parseErr.Error()` when the frontmatter failed to parse, else
  `"E_REQUIREMENT_INVALID: filename/frontmatter mismatch"`; `IDs` = the filename-derived ID (when
  `ticketIDFromFilename` succeeds) plus, when the frontmatter parsed, `requirement.ID`.
  - **`checkTraceability` refactor (M9c findings byte-for-byte preserved — HARD constraint):** on
    `unevaluated` emit its `{Code, Subject, Message}` verbatim (preserve the no-prefix/non-git early
    returns traceability.go:250-265 and the empty-registry returns :327/331); else emit the per-node
    `E_REQUIREMENT_INVALID` findings from `malformed` **in order** using each node's `Subject` +
    `Message` (:306-313), emit the all-nodes-malformed `U_TRACE_UNSCANNED` diagnostic when
    `len(requirements)==0 && len(malformedIDMap)>0` — **keyed on the ID-BEARING map, not the file
    slice** (Sol r5: today's malformed map is ID-keyed, so a project whose ONLY malformed file has an
    invalid filename AND unparsable content has an empty map and keeps `U_TRACE_EMPTY` at :330-332, NOT
    `U_TRACE_UNSCANNED` at :334-335). Then call **`resolveTraceabilityEdges(report,
    edges, requirements, malformedIDMap)` UNCHANGED**. Build `malformedIDMap` (`id -> Subject`) by
    iterating `malformed` in order and inserting every `ID` in each node's `IDs` → its `Subject`,
    **last-occurrence-wins** (reproduces today's last-write-wins map, :314-319). The existing M9c
    traceability tests guard exact output; the builder must not alter `resolveTraceabilityEdges` nor
    finding order/subjects/messages.
  - **gauge derivation:** fold covers/verifies from `edges` with the identical switch as
    `resolveTraceabilityEdges` (traceability.go:345-354) — via a shared pure helper if it can be
    extracted without changing `check` findings, else a faithful re-walk; `dangling` = edges whose ID
    is in neither `requirements` nor the union of all `malformed[*].IDs` (matches the resolver's
    dangling precedence — a malformed-ID edge is `U_TRACE_UNSCANNED`, not dangling). Per valid
    requirement → bucket by (status,covers,verifies); per `malformed` node (one per file) → a per-item
    `unevaluated` cell keyed by its `Subject` (ID-less nodes included). **Universe = len(requirements) +
    len(malformed)** (breakdown key = requirement ID for valid nodes, `Subject` for malformed nodes).
    No findings round-trip.
- **`unevaluated` signal (gauge-level) is set ONLY for a genuine scan failure**, and carries the exact
  `{Code, Subject, Message}` so `check` reproduces the finding verbatim: registry unreadable
  (traceability.go:246), `discoverWorktrees` error (:268), snapshot tore (:285), parse error (:297),
  or empty requirement registry (U_TRACE_EMPTY, :327/331). It is **NOT** set for the
  "all-discovered-nodes-malformed" case (:334 — enumeration succeeded), which the gauge represents as
  per-item `unevaluated` buckets (R11).
- **Per-requirement buckets (R6, closed set), derived from `(status, covers, verifies)` exactly as
  traceability.go:362-377 classifies:**
  | bucket | condition |
  |---|---|
  | `covered_verified` | `status==Built` && covers && verifies |
  | `unverified` | `status==Built` && covers && !verifies (`W_TRACE_UNVERIFIED`) |
  | `uncovered` | (`status==Built` or `Partial`) && !covers (`W_TRACE_UNCOVERED`) |
  | `partial_covered` | `status==Partial` && covers (satisfied; Partial has no verify expectation) |
  | `not_built` | `status ∉ {Built, Partial}` (no coverage expectation) — from `traceRequirement.status` |
  | `unevaluated` | node in `malformed` (per-item; enumeration succeeded but the file is unreadable) |
  A `Built` requirement that is both uncovered and unverified buckets as `uncovered` (the headline —
  no coverage at all); this is stated so the mapping is deterministic and exhaustive.
- **`dangling` count** (top-level `Fields` field, not a per-requirement bucket): `len(traceScan.
  dangling)` — annotations pointing at an absent requirement (`E_TRACE_DANGLING`, traceability.go:349).
- **Value** = distribution `map[bucket]int` (+ `dangling` count in `Fields`); **Breakdown** =
  `GaugeCell` keyed by requirement ID for valid nodes, and by `Subject` for malformed nodes (which may
  lack an ID) — `{Value: bucket}`.
- **Universe:** `Count = len(traceScan.requirements) + len(traceScan.malformed)` (every enumerated
  requirement node, valid or malformed); `Scope = "project"` (matches `check`'s multi-worktree
  discovery); `AsOf = {trace_scan: insightScanID()}` (+ optionally a per-worktree tracked digest). No
  sequence.
- **Watermark honesty (R4):** traceability has **NO monotone watermark** (a live snapshot). `as_of`
  carries ONLY the wall-clock scan marker `insightScanID()` (+ optional tracked digests) and
  **fabricates no sequence**. No cross-call monotonicity claimed.
- **Unevaluated (R2/R3/R11):** gauge-level `Unevaluated` iff `traceScan.unevaluated` is set (genuine
  scan failure or empty registry, per the signal bullet above) — reason cites the specific cause;
  never a partial/fake distribution. The **all-nodes-malformed** case (requirements empty, malformed
  non-empty) is NOT gauge-unevaluated — it is an evaluated gauge whose distribution is entirely
  per-item `unevaluated` buckets (universe = malformed count), so N malformed requirements are
  surfaced, not hidden (R11).
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
- **R3 (genuine scan failure → gauge-unevaluated).** Gauge-level `Unevaluated` iff the traceability
  scan-core sets its `unevaluated` signal (registry unreadable / `discoverWorktrees` error / snapshot
  tore / parse error / empty registry). Distinct from per-node malformed (R11).
- **R4 (watermark honesty).** ratchet-status: monotone gate-audit `Seq` + `test_report_at_seq`.
  traceability-status: wall-clock scan marker only; no fabricated sequence.
- **R5 (scope — REVISED per Sol P1).** ratchet-status = `project`; traceability-status = `project`
  (multi-worktree, matching `aira check`) — NOT current-worktree as v1 had it.
- **R6 (exhaustive closed classification with explicit precedence — per Sol P1a/P1b).** Precedence
  (grounded in gate_ratchet.go:111-203): classify by `eval.Code` whenever `eval.Predicate != ""`;
  only the single bare-error path (line 128, empty Predicate) uses `ErrorCode(err)`. The report-load
  `E_INTERNAL` (line 137) and unknown-comparator `E_GATE_INVALID` (line 199) are masked into
  `U_GATE_INCOMPARABLE`/`E_GATE_INVALID` by the evaluator (Predicate set) — so they bucket as
  `incomparable`/`invalid`, matching `gate run`. The bare-error path buckets `E_JOURNAL_CORRUPT`→
  `corrupt`, else (incl. non-prefixed → `E_INTERNAL`)→`unclassified` carrying the raw code.
  `E_GATE_BASELINE_INVALID` is documented-unreachable (omitted from the closed set; falls to
  `unclassified` if it ever appears). Never silently dropped, never `pass`, never aborts the gauge.
- **R7 (live vs stored).** ratchet-status live-evaluates (`evaluateRatchet`), NOT the stored gate
  projection. Documented; `gate check` is the separate stored+reconciled view.
- **R8 (only pure evaluators are gauge-safe).** command/dimension gate-status excluded (D1).
- **R9 (no traceability drift — via a LOSSLESS scan-core seam, per Sol P1c/P1).** The gauge and
  `checkTraceability` share the pure `scanTraceabilityGraph()` returning ORDERED raw `edges`, valid
  `requirements` (with status), ordered `malformed` nodes keyed by unique `Subject`, and an
  `unevaluated` signal — NOT a findings round-trip (can't reconstruct `not_built` from `status`) and
  NOT a digested map (would lose edge IDs/kinds/lines/order and ID-less malformed nodes).
  `checkTraceability` reconstructs its exact M9c findings from this (order/subjects preserved;
  `resolveTraceabilityEdges` unchanged, fed the id→subject map) — regression-guarded by the existing
  traceability tests. A drift test asserts the gauge's buckets are consistent with `aira check`'s
  traceability outcome for the same fixture, **including a sibling-worktree fixture**.
- **R11 (malformed-node ≠ scan-tear — per Sol P1d).** A node that enumerated but failed to parse
  (`E_REQUIREMENT_INVALID`) is a **per-item `unevaluated` bucket** (the requirement is known to exist,
  unreadable); the gauge stays evaluated (universe counts it). Only a genuine scan failure (R3) makes
  the gauge unevaluated. The existing all-nodes-malformed `U_TRACE_UNSCANNED` finding (traceability.go:
  334) is a `check`-side diagnostic; the gauge represents that same state as an all-`unevaluated`
  distribution over the malformed nodes, never as gauge-level unevaluated.
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
5. **`ratchet-status` error-path (R6, discriminating):** (a) induce a gate whose
   `ResolveGateBaseline` audit verification yields a bare `E_JOURNAL_CORRUPT` (corrupt an audit
   record so `audit.Read()` fails, the empty-Predicate line-128 path) → that gate's cell = `corrupt`
   carrying `E_JOURNAL_CORRUPT`; the gauge still returns and OTHER gates are unaffected; assert NOT
   folded into `pass`. (b) A report-load failure (Predicate set → `U_GATE_INCOMPARABLE`) buckets as
   `incomparable`, NOT `unclassified` — proves the precedence rule. (A naive `default: pass` or a
   naive "any err → unclassified" classifier fails one of these.)
6. `traceability-status` fixture exercising ALL buckets — `covered_verified` / `unverified` /
   `uncovered` / `partial_covered` / `not_built` + a malformed node (per-item `unevaluated`) + a
   dangling annotation → distribution + `dangling` count; universe = `len(requirements)+len(malformed)`,
   scope `project`, `as_of` has `trace_scan` and **no** sequence key. Assert a Built+uncovered+unverified
   requirement buckets as `uncovered` (deterministic headline). **Includes an ID-less malformed node
   (invalid filename):** its breakdown cell is keyed by `Subject` and it counts in the universe —
   proves collapse-free, no node lost (a naive `map[id]` seam drops it).
7. `traceability-status` empty registry → gauge `Unevaluated` ("no requirements").
8. `traceability-status` genuine scan failure (registry unreadable / snapshot tear, `unevaluated`
   signal) → gauge `Unevaluated` (reason cites the cause), NOT a distribution.
8b. **R11 discriminating:** all-nodes-malformed project (≥1 requirement file, all unparseable) →
   gauge **evaluated**, distribution == {`unevaluated`: N}, universe N — NOT gauge-level unevaluated
   (a naive "U_TRACE_UNSCANNED → gauge unevaluated" mapping fails this).
8c. **M9c compatibility (Sol r5, discriminating):** a project whose ONLY malformed requirement file
   has an invalid filename AND unparsable content (no derivable ID) → `aira check` STILL emits
   `U_TRACE_EMPTY` (not `U_TRACE_UNSCANNED`), proving the diagnostic keys on the ID-bearing map; AND
   the gauge is evaluated with that file as a per-item `unevaluated` cell keyed by `Subject`, universe
   1 (the gauge counts the ID-less file even though the check diagnostic does not). A naive
   `len(malformedSlice)>0` predicate flips the check finding and fails this.
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
- `internal/store/traceability.go`: extract the pure `scanTraceabilityGraph()` scan-core returning
  LOSSLESS ordered `edges` + valid `requirements` (with status) + ordered `malformed` nodes
  (`{Subject, IDs []string, Message}`, one per file) + an `unevaluated` `{Code, Subject, Message}`
  signal, called by BOTH `checkTraceability` and the gauge (R9/R11); `resolveTraceabilityEdges`
  UNCHANGED (fed a last-occurrence-wins id→subject map rebuilt from `malformed[*].IDs`);
  `checkTraceability`'s M9c findings preserved byte-for-byte (existing tests guard it).
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
