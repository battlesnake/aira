# AIRA M13b — ratchet gate (design)

Status: DRAFT (pre plan-review). Author: Opus (coordinator). Milestone: Phase 4 · M13b.
Base: master `fd6f9e5` (M13 done). Building delegated to Codex; gate order (owner): Sol
plan-review → approve → Opus/Fable plan-final → build → Sol build-review → approve →
Opus/Fable final → merge.

Authoritative parents:
- `docs/superpowers/specs/2026-08-11-aira-m10-gates-design.md` §4.5 (ratchet baseline —
  durable evidence), §5 (evaluation protocol, step 4), §5.4 (ratchet comparison contract),
  §6.1 (verdict table). **These are authoritative for the baseline shape and comparator; this
  spec reconciles them with the now-existing M13 archive and fixes the build decisions.**
- `docs/superpowers/specs/2026-08-12-aira-m13-test-reports-design.md` (the archive + the
  flaky classifier this ratchet consumes; the `pinned` column already exists — M13 R6).
- Main spec §9 (ratchet gates compute over a pinned, provenanced, run-linked derived baseline,
  never evictable telemetry; missing baseline → unevaluated; known-flaky not a new failure).

This implements the **deferred `ratchet` gate KIND**: the enum value exists and its codes are
registered, but the checker path is refused today (`internal/gate/gate.go:111`; the guard test
`TestGateCatalogRegistersDeferredRatchetCodesWithoutRatchetPath`). M13b turns it on.

## 1b. Sol plan-review resolutions (thread 019ff7d6 — BLOCK, all incorporated)

- **R1 (P0) — the audit SNAPSHOT is the authoritative durable baseline; source reports are
  provenance only.** The contradiction (snapshot is audit-durable, but M13 source reports are
  DB-only and lost on rebuild → the floor wasn't operationally durable) is resolved by making
  the **authenticated `snapshot` (failing_set + counts + coverage) THE baseline**. Comparison
  reads the snapshot, **never** re-derives from the source reports. A baseline is valid iff its
  active pointer resolves to a baseline record in the audit chain whose `snapshot_digest`
  verifies — **not** iff the source reports still exist. `pinned=1` on the source rows is a
  best-effort provenance convenience; its loss never invalidates the baseline. `U_GATE_BASELINE_
  MISSING` is only: no active pointer, or the pointed record is absent/corrupt/digest-mismatch.
  (§2, §3.3, T10.)
- **R2 (P0) — exact, unambiguous current evidence; incomplete/ambiguous ⇒ incomparable, never
  filtered away.** The comparator does NOT silently drop parser-incomplete reports. It selects
  the **exact** set of current reports matching the baseline `comparison_key` at the subject
  commit; if **any** matching report is parser-incomplete, zero-discovered, missing identity,
  or the set is ambiguous (conflicting reports for the one cell with no resolution rule) ⇒
  **`U_GATE_INCOMPARABLE`**. A truncated report whose missing tail could hold a new failure can
  never be filtered out to yield an empty (passing) set. (§3.1, new T14.)
- **R3 (P0) — single-cell baseline; a pin must be one comparison_key + one commit.** `--report`
  names one or more reports that MUST share `(suite_id, config, env_digest, shard, commit)`;
  mixed keys/commits ⇒ `E_GATE_BASELINE_INVALID` (a gate declares one lane/cell; multi-cell
  ratchets are deferred). `failing_set` = the union of first-pass parser-complete fail/error
  identities across the named same-cell reports. (§2, §5; cross-shard/mixed-commit pin tests.)
- **R4 (P0) — flaky exclusion is CELL-EXACT, not the test-level aggregate.** M13 `FlakyTests`
  reports test-level `State=flaky` if ANY cell is flaky; using that directly could exclude a
  clean regression in a different cell. The comparator excludes a failing identity from
  `new_failures` **only if its own `(name, commit, suite_id, config, env_digest, shard)` cell**
  is `FlakyStateFlaky` (read `FlakyCell.State`, not `FlakyTest.State`). `unevaluated`/`clean`
  cells never qualify. (§3.1; cross-cell false-pass test.)
- **R5 (P0) — verdict code + canary must fit the real APIs.** (a) `FoldVerdict` maps a failed
  predicate to `E_GATE_FAILED`; the ratchet path must carry its **own** failure code so a
  regression folds as **`E_GATE_RATCHET_REGRESSED`** (extend the predicate/evaluation result to
  carry a code, or a ratchet branch in the fold — do NOT emit the generic code). (b) The command
  canary (temp-filesystem mutation) cannot create a ratchet regression. A **ratchet canary is a
  synthetic, isolated baseline+current report PAIR** (a seeded known regression) that the
  comparator must flag `E_GATE_RATCHET_REGRESSED`; non-fire ⇒ `E_GATE_CANARY_DID_NOT_FIRE`. It
  runs entirely in-memory/isolated, touching no real DB reports. (§4; T13 rewritten.)
- **R6 (P1) — audit replay is part of Rebuild.** `rebuildGateProjection` (results/proofs/
  attestations today) must ALSO replay baseline records + active-pointer entries from the audit
  chain, validate each `snapshot_digest`, and record an unresolved active pointer as such.
  Replay is best-effort and does **not** hard-error; a missing/mismatched/dangling pointer makes
  the gate **evaluation** read `U_GATE_BASELINE_MISSING` (audit-authoritative), per §7. The
  projection tables are specified in §7. (§2, §7.)
- **R7 (P1) — pin validation is exact.** Reject (`E_GATE_BASELINE_INVALID`) a pin whose named
  reports include a retry (`retry_index!=0`), a parser-incomplete report, a zero-discovered
  report, a duplicate ID, or mixed cell/commit (R3). No-auto-rebaseline retained (D2). (§5.)

---

## 1. Scope

A `ratchet` gate compares the **current** comparable test-report evidence against an
**explicitly pinned, durable, audit-class baseline** and fails on regression. Two comparators
in M13b: **no-new-failures** (primary) and **coverage-drop**.

1. **Baseline pin** — `aira gate baseline pin <gate_id> --report <TR-id>[,<TR-id>…] [--reason
   R] [--actor A]`: derive the small comparison snapshot (failing-test identity set + counts,
   and/or the coverage field) from the named test-report(s), write an **immutable baseline
   record to the common-dir audit chain** (the M10a HMAC-authenticated log), advance the
   gate's **active-baseline pointer**, and set `pinned=1` on the source report rows (M13's
   retention-exemption forward hook) so the evidence survives eviction. No committed numeral;
   no project-file edit.
2. **Ratchet evaluator** — a new checker path for `kind: ratchet`: resolve the active pinned
   baseline; gather the current comparable evidence (parser-complete reports matching the
   gate's lane/comparison key at the subject commit); compute the comparator; fold into the
   M10 gate verdict.
3. **Flaky exclusion (now enabled by M13)** — the M10 design left known-flaky exclusion open
   "until the Phase-4 archive/classifier exists". It exists (M13 `FlakyTests`). A test the
   classifier reports **flaky** for the comparison cell is **excluded from `new_failures`** (a
   known-noisy test's red is not a regression). The exclusion is derived from the classifier,
   **never invented** by the comparator.

### 1.1 Non-goals / deferrals

- **D1 — one comparator family only:** no-new-failures + coverage-drop. The full metric
  catalog, custom retry-folding policies, and multi-metric ratchets are deferred (§5.4 leaves
  these open). M13b's comparator is **first-pass only** (retry results distinct; a retry pass
  never erases a first-attempt failure), matching M13 flaky semantics.
- **D2 — no auto-baseline / auto-rebaseline, ever** (spec anti-goal). A pass does not update
  the baseline; a fail does not update it. Re-baselining is an explicit new `pin`.
- **D3 — manual-attestation-on-pin is policy, not required:** a project *may* require a manual
  attestation to pin (per gate policy); M13b supports pinning without it by default.
- **D4 — no new telemetry/insight gauges** (separate subsystem).

---

## 2. Baseline record (audit class, durable)

Per M10 §4.5, a baseline is a **pinned evidence identity**, not a number. The record
(appended to the common-dir audit chain, content-addressed, retention-exempt while active):

`{gate_id, comparator, comparator_version, lane, comparison_key, source_report_ids[],
source_commit, evidence_digest, snapshot_digest, snapshot{failing_set[], discovered_count,
coverage?}, pin_actor, pin_at, pin_reason, seq}`

- **`snapshot.failing_set`** = the set of **test names** that were **fail/error** (first-pass,
  parser-complete) in the source report(s). Because a baseline is single-cell (R3), all its
  reports share `(suite_id, config, env_digest, shard, source_commit)` — so the fixed cell IS
  the identity context and a member is just its test name (M13 forbids duplicate names within a
  report). Names are stored length-prefixed in the canonicalised `snapshot` that feeds
  `snapshot_digest`, so no name can forge a set boundary. This is the durable floor. It is
  compared **only** within the authenticated cell (below), current commit differing from
  `source_commit`.
- **`comparison_key`** = the lane/identity fields a current report must match to be comparable
  (`suite_id, config, env_digest, shard`) — the same R1 tuple family M13 uses (minus commit,
  which differs between baseline and current).
- Appended via the **M10a audit-append** (authenticated seq + prev-digest + HMAC tag), so a
  truncated/tampered baseline log is detected (`E_JOURNAL_CORRUPT`/audit codes), never a
  silent zero baseline.
- **Active pointer**: a separate audit entry `{gate_id, active_baseline_seq}`; `pin` appends a
  new baseline then a new pointer. The old baseline record is never edited (immutable history).
- Rebuild of the DB projection re-reads the audit chain; the baseline is **not** DB-authoritative.

---

## 3. Comparators (§5.4, the false-pass direction is the danger)

### 3.1 no-new-failures

- Gather current comparable evidence: reports whose `(suite_id, config, env_digest, shard)`
  match the baseline `comparison_key`, `parser_complete==true`, discovered-count > 0,
  first-pass (`retry_index==0`), at the subject commit.
- `current_failing` = the identity set of fail/error results in that evidence.
- **`new_failures` = current_failing − baseline.snapshot.failing_set − flaky_excluded**, where
  `flaky_excluded` = identities the M13 classifier reports **flaky** for their cell.
- `new_failures` **non-empty ⇒ `E_GATE_RATCHET_REGRESSED` (fail)**. Empty ⇒ predicate-pass
  (still subject to proof/canary fold, §4).
- A test **failing at baseline that stays failing is NOT a new failure** (set difference).
- **Incomparable current evidence** — no comparable report, or a matched report is
  parser-incomplete / zero-discovered / missing identity ⇒ **`U_GATE_INCOMPARABLE`**, never a
  passing empty set.

### 3.2 coverage-drop

- Baseline `snapshot.coverage` vs the current comparable report's coverage field, per the
  committed comparator policy (default: current `pct` ≥ baseline `pct`).
- **Coverage must be unambiguous on both sides.** At **pin**, if a multi-report same-cell pin's
  reports carry **differing** coverage values, the pin is rejected `E_GATE_BASELINE_INVALID`
  (no implementation-dependent selection); `snapshot.coverage` is that single agreed value (or
  absent if no report carried coverage). At **eval**, if the current comparable set has more
  than one distinct coverage value ⇒ `U_GATE_INCOMPARABLE`.
- Current < baseline ⇒ fail (`E_GATE_RATCHET_REGRESSED`). Missing **either** coverage field ⇒
  **`U_GATE_INCOMPARABLE`** (never coerced to zero). The result reports the field, both
  evidence refs, and the observed delta.

### 3.3 Universal honesty (both comparators)

- **`U_GATE_BASELINE_MISSING` is defined solely by the audit chain (R1):** no active pointer,
  or the pointed baseline record is absent/corrupt or its `snapshot_digest` fails to verify.
  **The presence of the source test-reports is NOT a validity condition** — the authenticated
  snapshot is authoritative and survives DB rebuild/retention; `pinned=1` is provenance-only and
  its loss never invalidates the baseline. (This replaces the earlier source-presence rule.)
- Never a **zero baseline** (which would make every current failure "new", or nothing new —
  both wrong). **No coercion of missing counts/coverage/tests to zero** (§5 step 4).
- **No auto-rebaseline** (D2).

---

## 4. Verdict fold (reuse M10 FoldVerdict)

A ratchet gate's result folds into the existing `FoldVerdict` alongside proof-of-fire and
canary (M10a):

The fold takes the §3 comparator result and combines it with proof-of-fire and canary health
(it does not re-derive the comparison):
- comparator result = regression (`E_GATE_RATCHET_REGRESSED`) ⇒ **fail** (carrying that code,
  R5 — not the generic `E_GATE_FAILED`).
- comparator result = predicate-pass **and** valid current proof-of-fire/canary ⇒ **pass**.
- comparator result = `U_GATE_BASELINE_MISSING` / `U_GATE_INCOMPARABLE` ⇒ **unevaluated**.
- canary declared and did not fire ⇒ **fail** `E_GATE_CANARY_DID_NOT_FIRE` (fail-closed).
Ratchet gates evaluate through the same `gate run` / `check` / `ready` folding as
checkable/command gates (M10a/b) — no separate surface.

**Ratchet canary — a new declaration MODE (`internal/gate/canary.go`, in scope).** The existing
canary modes (fixture / attestation-challenge / mutation) run a checker against a temporary
filesystem and cannot create a ratchet regression, so M13b adds a `synthetic-ratchet` mode:
`CanaryDeclaration{mode: "synthetic-ratchet", baseline_failing:[names], current_failing:[names],
expected: "regressed"}`. `ValidateCanary` requires `current_failing` to introduce **≥1** name
absent from `baseline_failing` (an actual seeded regression), else `E_GATE_CANARY_INVALID`.
Dispatch (`runCanary` ratchet branch): build an **isolated in-memory** baseline snapshot from
`baseline_failing` and a synthetic current failing set from `current_failing`, run the **same
no-new-failures comparator** with an empty flaky-exclusion set, and require it to yield
`E_GATE_RATCHET_REGRESSED`. It reads **no** real DB reports and writes none. A fired canary
appends a proof-of-fire bound to `(gate digest, comparator version, lane, config digest, canary
id)` exactly like other proofs; non-fire (or a comparator that returns pass/incomparable on the
seeded regression) ⇒ `E_GATE_CANARY_DID_NOT_FIRE`. Canary integration tests are in scope.

---

## 5. Faces

- `aira gate baseline pin <gate_id> --report <ids> [--reason] [--actor]` (SafetyMutate — writes
  the audit baseline + pointer + sets pinned). Optionally `gate baseline show <gate_id>`
  (SafetyRead — the active baseline record). Extend the existing `gate` grouped verb
  (M10a) — new operations `baseline-pin` / `baseline-show`.
- Ratchet gate **definition** gains the kind payload (§ M10 4: metric/comparator + comparison
  key + baseline-selection rule = "active explicitly pinned baseline for this gate+lane").
  `internal/gate/gate.go` validation: allow `Checker == ratchet` bound to `Kind == ratchet`
  with a ratchet payload (currently rejected at line 111/160); a ratchet with no metric/
  comparator is `E_GATE_INVALID` (the existing §ValidateDefinition test extends).
- Register/confirm codes: `E_GATE_RATCHET_REGRESSED`, `U_GATE_BASELINE_MISSING`,
  `U_GATE_INCOMPARABLE` (already registered as deferred — wire them to the live path; the guard
  test `TestGateCatalogRegistersDeferredRatchetCodesWithoutRatchetPath` is REPLACED by real
  ratchet-path tests). Every generated face example machine-verified (M8b lesson).

---

## 6. Adversarial test matrix (false-pass — a regression slipping through — weighted heaviest)

- **T1 pin creates immutable audit baseline + pointer + pinned rows.** `baseline pin` writes
  the audit record, advances the pointer, sets `pinned=1`; a re-pin appends a NEW record +
  pointer, the old record byte-unchanged.
- **T2 still-red is not new.** baseline failing {A}; current failing {A} ⇒ **pass** (A red at
  baseline, stays red — not a new failure).
- **T3 new failure regresses.** baseline {A}; current {A,B} ⇒ **fail `E_GATE_RATCHET_REGRESSED`**
  (B new). And baseline {A}; current {B} (A now passes, B fails) ⇒ fail (B new). *The
  load-bearing false-pass test.*
- **T4 fixed test doesn't block.** baseline {A}; current {} ⇒ **pass** (A fixed, no new).
- **T5 flaky exclusion.** baseline {A}; current {A,B}, B classified flaky by M13 ⇒ **pass** (B
  excluded); B classified clean+failing ⇒ fail. Exclusion derived from the classifier only.
- **T6 missing baseline ⇒ U_GATE_BASELINE_MISSING** (never pass, never zero baseline).
- **T7 incomparable current ⇒ U_GATE_INCOMPARABLE** — no matching report / parser-incomplete /
  zero-discovered / mismatched suite/config/shard ⇒ unevaluated, never passing-empty.
- **T8 retries distinct.** a first-attempt fail of B + a retry pass ⇒ B still counts as failing
  (no retry-fold) ⇒ regression if B not at baseline.
- **T9 coverage-drop.** baseline 80%; current 75% ⇒ fail; 85% ⇒ pass; current coverage absent
  ⇒ U_GATE_INCOMPARABLE (not zero).
- **T10 durability (snapshot-authoritative, R1).** After a pin, **evict the source
  test-report(s) entirely** (simulate retention/rebuild loss) → the ratchet still evaluates
  correctly from the audit snapshot (baseline valid, comparison runs), NOT
  `U_GATE_BASELINE_MISSING`. Separately, a full DB rebuild re-projects the baseline + pointer
  from the audit chain. `pinned=1` is asserted only as best-effort provenance (its absence does
  not invalidate the baseline).
- **T11 no auto-rebaseline.** after a fail AND after a pass, the active baseline record +
  pointer are unchanged.
- **T12 fold + faces.** ratchet regression folds to `check`/`ready` as a failed gate; missing
  baseline folds as unevaluated; the `gate baseline pin` example parses/reaches core (drift/
  parity/E2E) and the agent-guide example is valid.
- **T13 canary (if declared).** a seeded known-regression canary that the comparator must flag;
  non-fire ⇒ `E_GATE_CANARY_DID_NOT_FIRE` fail-closed.

Real-binary e2e (Opus/Fable final): init; ingest a baseline report (A failing) + pin it; ingest
a current report with A-still-failing ⇒ ratchet pass; ingest one with a new failure B ⇒
`E_GATE_RATCHET_REGRESSED`; a flaky B ⇒ pass; drop the baseline ⇒ U_GATE_BASELINE_MISSING.

---

## 7. Layering & files

- `internal/gate/gate.go` — allow the `ratchet` checker/kind + ratchet payload validation
  (metric, comparator, comparison key, baseline-selection rule).
- `internal/store/gate_ratchet.go` (new) — baseline derive/pin (audit append + pointer +
  pinned), active-baseline resolve, the no-new-failures + coverage comparators (consuming
  `FlakyTests` for exclusion and the M13 test-report tables), `U_GATE_*` honesty.
- `internal/store/gate_eval.go` / `gate_command.go` — dispatch `ratchet` in `evaluateChecker`;
  fold via existing `FoldVerdict`.
- **Rebuild replay (R6) — concrete projection.** `rebuildGateProjection` gains two disposable
  projection tables, replayed deterministically from the authenticated audit chain (audit is
  authoritative; these are rebuildable):
  - `gate_baselines(project_id, gate_id, baseline_seq, comparator, comparator_version,
    comparison_key, source_commit, snapshot_digest, snapshot_json)` — one row per baseline-pin
    audit entry; on replay each `snapshot_json` is re-hashed and **must equal** `snapshot_digest`
    (mismatch ⇒ the row is marked invalid, not inserted as usable).
  - `gate_baseline_active(project_id, gate_id, active_baseline_seq)` — upserted from each
    active-pointer audit entry (last pointer wins by audit seq).
  Replay is **best-effort projection and does not hard-error** on a dangling/mismatched pointer
  (consistent with the reconciler): a pointer resolving to no valid `gate_baselines` row means
  the *evaluation* of that gate reads **`U_GATE_BASELINE_MISSING`** (fail-closed at eval), never
  a rebuild failure and never a silent pass. Baseline resolution at eval always re-checks the
  audit chain as the authority; the projection is only an index.
- `internal/store/gate_audit.go` — extend the audit record types for baseline + pointer entries
  (reuse the M10a authenticated append).
- `internal/core/core.go` + `cmd/aira/main.go` — `gate baseline pin|show` operations + Store
  interface; metadata/agent-guide.
- Replace the deferred-path guard test with real ratchet tests. No runner/telemetry/daemon.

## 8. Build plan (delegated, TDD, frequent commits)

1. gate.go ratchet payload validation + codes wiring (replace the deferred guard).
2. baseline derive + audit pin + active pointer + pinned rows (T1, T10, T11).
3. no-new-failures comparator + flaky exclusion (T2–T5, T7, T8) — the correctness core.
4. coverage comparator (T9).
5. `evaluateChecker` ratchet dispatch + `FoldVerdict` (T6, T12) + canary (T13).
6. faces (`gate baseline pin|show`) + metadata/agent-guide + full `make ci`.

Gate: Sol plan-review → approve → Opus/Fable plan-final → Codex build (real-cgroup-independent;
pure Go + SQLite + common-dir over temp dirs, so Codex can self-verify) → Sol build-review
(weight false-pass: a regression that folds to pass; a zero-coerced baseline) → approve →
Opus/Fable final + real-binary e2e → merge `--ff-only`.
