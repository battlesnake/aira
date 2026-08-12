# AIRA M13 — test-report archive + ingestion + flaky detection (design)

Status: DRAFT (pre plan-review). Author: Opus (coordinator). Milestone: Phase 4 · M13
(first Phase-4 milestone). Base: master `1669857` (Phase 3 complete).

Authoritative parent: `docs/superpowers/specs/2026-08-07-aira-design.md` §13 (test reports),
§6 (`TestReport` + per-test schema), §5.3 (durability classes), §12 (telemetry framing), §21
(format-normalisation breadth is explicit latitude). This spec resolves the M13-specific
decisions and fixes the adversarial test matrix. It does not re-open settled architecture.

Review gate for this plan (owner, 2026-08-12): **Sol plan-review → approve → then an
Opus-or-Fable final review → build.** Build gate: **Sol build-review → approve → Opus/Fable
final → merge.** Building is delegated to Codex to conserve Anthropic quota.

## 1b. Sol plan-review resolutions (thread 019ff4e8 — BLOCK, all incorporated)

- **R1 (P0) — comparability requires ALL identity components present/known.** A per-test
  result is *comparable* only when its report has **non-empty** `commit`, `suite_id`,
  `config`, `env_digest`, AND `shard`. Any missing/empty component ⇒ the result is **not
  comparable** ⇒ contributes to no flaky/clean verdict (→ `unevaluated`), never compared.
  Empty `env_digest` is **not** "comparable to other empties" (that would false-flaky two
  different lanes). `shard` encodes its partitioning as `"i/n"` (e.g. `"3/8"` ≠ `"3/4"`);
  default single-shard is `"1/1"`. (§2, §4; negative tests per-component R8.)
- **R2 (P0) — typed parser classification: malformed refuses, truncated stores incomplete.**
  The parser returns `(parsed{results, complete bool}, error)`. `error != nil` ⇒ **malformed**
  ⇒ `E_TESTREPORT_INVALID`, refuse, **zero rows written**. `complete == false` (well-formed
  but the test set did not fully terminate) ⇒ stored with `parser_complete=false`, excluded
  from comparison. go-json: a non-JSON line or a JSON line with a bad `Action` after a valid
  prefix ⇒ malformed/refuse; a valid-but-truncated stream (not every started test reached a
  terminal action) ⇒ incomplete/store. JUnit: XML is atomic — any XML parse failure
  (including truncation) ⇒ malformed/refuse; the only JUnit *incomplete* case is a
  **well-formed** doc whose `<testcase>` count is below its declared `<testsuite tests=N>`
  count ⇒ `parser_complete=false`/store. (§3; T2/T4.)
- **R3 (P0) — `TR-n` is a DB transaction-local counter, full stop.** A per-project
  `test_report_counter` row (mirroring the runner ledger counter), incremented inside the
  same `BEGIN IMMEDIATE` tx as the report insert; monotonic, unique, concurrency-safe. It
  writes **no** allocation receipt, **no** common-dir file, **no** journal event, and does
  **not** touch the git ID allocator. The earlier "via aira id" wording is struck. (§2.1.)
- **R4 (P0) — `cmd/aira/main.go` is in scope.** `parseArgs`/`buildRequest` gain the
  `test-report` verb, its subverbs, and options; `add` reads the report from **stdin or a
  file arg** (define: positional `-`/absent ⇒ stdin; a path ⇒ read file); `flaky` gains
  `--explain`. The core `Store` interface (the seam `core.Do` calls) is extended with
  `AddTestReport`/`ListTestReports`/`GetTestReport`/`FlakyTests`. Without this the binary
  face cannot build. (§6, §9.)
- **R5 (P1, revised twice) — the DB-resident flaky reconciliation finding, encoded to the
  ACTUAL finding model.** `domain.NewReconciliationFinding` requires an `E_`-pattern `Code`
  and **forbids** review fields (`Category`/`Severity`/`Source`/… must be empty —
  finding.go:166). So flaky emits a reconciliation finding with **`Code = E_TESTREPORT_FLAKY`**
  (new, registered) and an **injectively-encoded `Subject`**: because `name`, `suite_id`, and
  the opaque `config` may contain any delimiter, the cell identity `(name, commit, suite_id,
  config, env_digest, shard)` is encoded **collision-free** — `Subject = "flaky:" +
  hex(sha256(len-prefixed canonical tuple))` (every component length-prefixed so no value can
  forge a boundary). **`Details`** carries the human-readable cell + witnesses (e.g.
  `test=<name> cell=[commit=.. suite=.. config=.. env=.. shard=..] pass:TR-3 fail:TR-7`).
  **`TicketID` is always empty** for a flaky finding (a cell's witnesses may carry different or
  absent tickets; an empty ticket keeps the `Key` cell-stable across recomputes). The `Key` is
  `NewReconciliationFinding`'s own `reconciliationFindingKey(code, subject, "")` — **not**
  hand-rolled.
  **Never git-durable, never journaled.** Staleness via the **disposable-projection** pattern
  (M9b requirements/relations index): a `reconcileFlaky` step **recomputes the full flaky set
  and REPLACES** all `E_TESTREPORT_FLAKY` reconciliation findings for the project in one tx
  (delete-all-`E_TESTREPORT_FLAKY` → reinsert-current) — so a no-longer-flaky test (new
  passing evidence, evicted witness) is cleared with no per-key upsert. The live `flaky` query
  and these findings both derive from the one `FlakyTests` computation. Ordering + hook in R9.
  (§4, §2.3, §6.)
- **R9 (P1, re-review-3) — `reconcileFlaky` ordering + Store seam.** A new store operation
  `ReconcileFlaky()` performs the full recompute-and-replace in a single tx. It runs
  **(a) inside `AddTestReport` AFTER the report/results insert and AFTER retention eviction**
  (so evicted witnesses are already absent), and **(b) at `aira check`** (the reconciliation
  trigger, alongside the existing reconcilers). `FlakyTests` (read-only, for the `flaky`
  query) and `ReconcileFlaky` (write, the finding replacement) share the same pure
  comparability/aggregation core. (§4, §9.)
- **R6 (P1) — define `at_seq`, provenance defaults, retention precedence, and the M13b pin
  column NOW.** `at_seq` = a per-project monotonic **ingress sequence**, allocated from a DB
  counter **inside the same `BEGIN IMMEDIATE` tx as the report insert**, giving retention a
  total eviction order independent of wall-clock. **An idempotent byte-identical re-add
  (returns the existing id) does NOT consume an `at_seq`** (no new row). Provenance:
  `commit`/`branch` = flag, else best-effort `git rev-parse HEAD`/`--abbrev-ref` (recorded
  as-derived), else empty; `runner`/`agent`/`session` = flag, else empty. Retention = the two
  independent `pinned=0` DELETEs defined exactly in §5 (a count DELETE keeping the newest 5000
  unpinned by `at_seq`, and a separate age-only DELETE when `max_age_days>0`); pinned rows
  never count toward the cap and are never evicted (in M13 every row is `pinned=0`; the clause
  exists so M13b's pin is a value flip, **no migration**). Eviction response reports
  `{evicted_count, remaining}`. (§2, §5.)
- **R7 (P1, refined) — cell-level states + explicit aggregation.** Evidence counted per test
  `name` is **first-pass** (`retry_index==0`), **parser-complete**, **non-`skip`** results,
  grouped into **cells** (a distinct full R1 tuple). Per **cell**: **flaky** = ≥2 such results
  with both a pass and a fail/error; **clean** = ≥2 such results, no pass/fail disagreement;
  **unevaluated** = <2 such results. **Aggregate a test across the cells it appears in with
  precedence `flaky > unevaluated > clean`**: flaky if any cell flaky; else unevaluated if any
  cell unevaluated; else clean (every cell clean). This closes "two passes in cell A + one
  incomparable result in cell B ⇒ clean" (cell B is unevaluated ⇒ the test is unevaluated), and
  **`skip` never counts toward the ≥2** (so skip-only ⇒ unevaluated). `flaky` lists flaky
  tests; `flaky --explain <name>` returns the state + reason and, for an unevaluated test,
  exit code `U_TESTREPORT_INCOMPARABLE` (3). T9 (retry-only) and T11 (single) assert
  **`unevaluated`**. (§4.)
- **R8 (P1) — added regression tests.** T15 JUnit `classname`+`name` identity (same `name`,
  different `classname` ⇒ distinct tests); T16 per-tuple-component mismatch negatives (suite,
  env_digest, shard each mismatched ⇒ not comparable ⇒ `unevaluated`, not flaky); T17 evict a
  flaky witness ⇒ the cell drops below 2 comparable ⇒ `flaky` recomputes to `unevaluated`
  **and `ReconcileFlaky` removes the now-stale `E_TESTREPORT_FLAKY` reconciliation finding**
  (assert it is gone), per R5/R9. (§8.)

---

## 1. Scope

A durable, trended, checkable archive of test outcomes, plus the checkable primitives it
enables that **do not require the runner**.

1. **`TestReport` schema + DB storage** (operational telemetry, retention-capped, §5.3) —
   report header + per-test results, with the full identity fields (suite / config /
   env-digest / shard / retry-index / parser-complete / coverage / provenance) **without
   which flakiness and ratchet comparisons are invalid** (§13).
2. **Ingestion, out-of-core:** `aira test-report add --format <go-json|junit> < report` —
   normalise a CI report to the one schema. Two formats in M13 (the two AIRA most needs):
   **go-test-json** (reuse/extend the M10b `ParseGoTestJSONV1`) and **JUnit XML**. Other
   formats (`tap`, `pytest-json`) are deferred (§21 latitude).
3. **Query:** `aira test-report ls|show` over the archive.
4. **Flaky detection primitive:** `aira test-report flaky [selector]` — a **live query**
   over the current corpus: same test, same commit, **same config**, appearing both `pass`
   and `fail` across **first-pass** results → flaky. Honest: insufficient/incomparable data
   reads `unevaluated`, never a false flaky and never a false clean.
5. **Retention cap** — evict oldest reports beyond a configurable cap (telemetry class).

### 1.1 Non-goals / deferrals (written down; do not build)

- **D1 — ratchet baseline + ratchet gate → M13b.** The §9 `ratchet` gate (deferred from
  M10) needs the *pinned, provenanced, audit-class* baseline (§5.3 exception) and the M10
  gate engine. M13 builds the archive it will rest on, **not** the pin or the gate. M13's
  retention evicts by telemetry rules only; the pin+exemption lands in M13b. (Forward hook:
  the schema carries everything a baseline needs, so M13b pins without a migration.)
- **D2 — auto-ingest from a run (`aira run --report`, §14) → Phase 5.** The runner's
  `--report` wiring is Phase 5. M13 ingests from **stdin/file only**. `run-ref` is an
  optional schema field, left empty in M13.
- **D3 — master-red-duration + §17 insight gauges → later Phase-4 insights.** M13 exposes
  the *data* (queryable reports); the "red on master since Y" duration gauge and the flaky
  §17 gauge dashboards are the insights milestone. (`flaky` the primitive is in M13; the
  trended *gauge* is not.)
- **D4 — coverage ratchet → M13b.** `coverage` is a stored schema field in M13 (so a report
  carries it), but no gate consumes it yet.
- **D5 — extra formats (`tap`, `pytest-json`, generic).** Deferred; the parser dispatch is
  built so adding one later is a new parser, not a schema change.
- **D6 — no compute/telemetry (§12) wiring.** Separate subsystem.

---

## 2. Data model

### 2.1 `TestReport` (header) — DB telemetry, retention-capped

`{id, ticket?, phase?, commit, branch, worktree_id, agent?, session?, at, run_ref?,
suite_id, runner, config, env_digest, shard, retry_index, parser_complete(bool),
coverage?, format, source_digest, at_seq}`

- `id` — `TR-n` from a **DB transaction-local per-project counter** (`test_report_counter`
  row, incremented inside the report-insert `BEGIN IMMEDIATE` tx; monotonic, unique,
  concurrency-safe). **Never** the git ID allocator, **never** hand-picked; writes no
  receipt/common-dir/journal (R3). Mirrors the M12 `RUN-n` ledger counter.
- **Identity tuple** = `(commit, suite_id, config, env_digest, shard)` + `retry_index`. Two
  results are *comparable* only when **every** one of `commit`, `suite_id`, `config`,
  `env_digest`, `shard` is **present/non-empty and equal** across the two reports (R1) — a
  missing component makes a result incomparable (`unevaluated`), never a default that
  false-matches another lane. A differing `config`/`env_digest`/`shard` is concurrency- or
  environment-sensitivity, **not** flakiness (§13). `shard` = `"i/n"` (default `"1/1"`).
  `retry_index > 0` marks a retry, excluded from first-pass flaky logic.
- `parser_complete` — `false` only for a **well-formed but incomplete** report (go-json: a
  started test with no terminal action at EOF; JUnit: `<testcase>` count below the declared
  `<testsuite tests=N>`). A **malformed** input never reaches this state — it is refused with
  `E_TESTREPORT_INVALID` (R2), never stored as incomplete. A `parser_complete=false` report is
  **stored** (never dropped) but **excluded from flaky/ratchet comparison** (`unevaluated`) —
  the direct analogue of the M10b `U_GATE_PARSER_INCOMPLETE` honesty rule.
- `config` — a caller-supplied opaque string (e.g. `-race`, `GOARCH=arm64`); required to be
  non-empty for a report to be comparable (R1). `env_digest` — `sha256(sorted LF KEY=VALUE)`
  over a caller-declared config env subset (reuse the runner's `EnvDigest`), or empty if not
  supplied. **An empty `env_digest` makes the report incomparable (excluded from flaky,
  `unevaluated`) — it does NOT match other empty-digest reports** (R1: two different lanes that
  both omitted the digest must not false-match).
- `shard` — `"i/n"` (validated strictly against that shape); the canonical unsharded default
  is the explicit literal `"1/1"`.
- `coverage` — optional `{pct?, lines_covered?, lines_total?}`; stored, unused in M13 (D4).
- `source_digest` — `sha256` of the raw ingested bytes (idempotency / provenance).

### 2.2 Per-test result — DB, child of a report

`{report_id, name, outcome(pass|fail|skip|error), duration_ns?, message?}` (closed outcome
enum). `name` is the fully-qualified test name (package + test) as the parser yields it.

### 2.3 Durability

Both tables are **DB-only, retention-capped operational telemetry** (§5.3). Loss on a rare
rebuild is tolerable; there is **no git-durable or common-dir write** in M13 (the audit-class
promotion is M13b's job when a report is pinned as a ratchet baseline). **A report ingest is
not journaled at all** — reports are **not** §11-significant mutations (like runs/spend, they
would swamp the journal, §11). The one derived write M13 makes is the DB-resident `flaky-test`
reconciliation finding (§4, R5) — DB-only, non-git, non-journaled.

---

## 3. Ingestion + normalisation

`aira test-report add --format <go-json|junit> [--ticket ID] [--phase P] [--commit SHA]
[--branch B] [--suite S] [--config C] [--shard N] [--retry N] [--config-env K=V,…] < report`

- **Parser dispatch** on `--format`; unknown format → `E_ARGUMENT_INVALID`.
- **go-test-json** — a **per-test** parser sharing the M10b `ParseGoTestJSONV1` strictness
  discipline (every discovered test must reach a terminal action; else `parser_complete=
  false`) but emitting `{name, outcome, duration}` per test rather than the aggregate
  `DiscoveredCount`/`FailedCount` M10b needs. Map `pass|fail|skip` → outcome; a build/compile
  failure or an unterminated test → `parser_complete=false`. Factor the shared line/event
  decoding so both the gate's aggregate view and M13's per-test view use one decoder (no
  second, divergent go-json parser).
- **JUnit XML** — parse `<testsuite>/<testcase>`; `<failure>`→fail, `<error>`→error,
  `<skipped>`→skip, else pass; `time`→duration. **XML is atomic: any XML parse failure —
  including truncation — → `E_TESTREPORT_INVALID` (refuse, zero rows).** A *well-formed* doc
  whose `<testcase>` count is **below** its `<testsuite tests="N">` declared count →
  `parser_complete=false` (well-formed but incomplete — the only JUnit incomplete case);
  a well-formed doc with count matching (or no declared count) → complete.
- **Provenance:** `commit`/`branch` from flags, else best-effort `git rev-parse HEAD` /
  `--abbrev-ref` (recorded as-derived); if neither → stored empty (a report with no commit
  is ingestible but **excluded from same-commit comparisons**, unevaluated for flaky).
- **Idempotency:** a re-`add` of byte-identical input (same `source_digest`) under the same
  identity is a no-op returning the existing `id` (not a duplicate) — reruns don't inflate
  the corpus. (Distinct bytes → a new report, even same identity: it *is* a new run.)
- Ingest is **atomic** (one tx: report header + all results); a mid-ingest crash leaves no
  partial report.

---

## 4. Flaky detection (the checkable primitive)

`aira test-report flaky [selector] [--explain <name>]` (store method `FlakyTests`) — a **live
query**, not a stored numeral (§17 law). Per test `name`, within each comparability **cell**
(a distinct value of the full R1 tuple `(commit, suite_id, config, env_digest, shard)`):

- Consider only **comparable, first-pass, parser-complete** results — `retry_index == 0`,
  `parser_complete == true`, and every tuple component present (R1). `skip` never contributes.
- **Three-state result (R7)** — compute a state **per cell** (evidence = first-pass,
  parser-complete, non-`skip` results), then **aggregate over the cells the test appears in**
  with precedence **`flaky > unevaluated > clean`**:
  - **cell flaky** — ≥2 evidence results with both a `pass` and a `fail`/`error`.
  - **cell clean** — ≥2 evidence results, no pass/fail disagreement.
  - **cell unevaluated** — <2 evidence results (single, retry-only, parser-incomplete, or
    the cell arises only from cross-config/-env/-shard/missing-identity data).
  - **test = flaky** if any cell flaky (report the cell(s) + witnessing report ids); **else
    unevaluated** if any cell unevaluated; **else clean** (every cell clean). So a two-pass
    cell plus one incomparable cell ⇒ **unevaluated**, and a skip-only test ⇒ **unevaluated**.
- **Honesty:** we never call a test **clean** from insufficient evidence (that is
  `unevaluated`), and never **flaky** from cross-config/-env/-shard disagreement (that is
  concurrency/environment sensitivity, §13). `flaky` lists the flaky set; `flaky --explain
  <name>` reports the test's state + the reason (which cells, why unevaluated).

**DB-resident flaky finding (R5, §13-required).** Flaky detection emits a `flaky-test`
finding, **subtype `reconciliation`** (DB-resident, §6) — **never git-durable, never
journaled**. Staleness is handled by the **disposable-projection** pattern (as the
requirements/relations index, M9b): the reconciler recomputes the full flaky set and
**replaces** all `flaky-test` reconciliation findings for the project (delete-all →
reinsert-current), so a test that is no longer flaky (new passing evidence, or an evicted
witness) is cleared automatically — no per-key upsert. Recompute runs at `aira check` and
after `test-report add`. The live `flaky` query and the reconciliation findings both derive
from the one `FlakyTests` computation (one source of truth).

---

## 5. Retention

Exact policy (`.aira/config` `test_reports`) — **two independent DELETEs**, each over
`pinned=0` rows only, a row evicted if **either** applies:
- **count** — `max_reports` (default **5000**): keep the newest 5000 **unpinned** rows by
  `at_seq`, evict the rest. (Pinned rows do not count toward the cap and are never evicted.)
- **age** — `max_age_days` (default **0 = disabled**): when >0, evict every `pinned=0` row
  whose `at` predates `now − max_age_days`. This is a separate age-only DELETE, **not**
  qualified by the count rule.

Both cascade-delete the report's results; the ingest response carries `{evicted_count,
remaining}` (never silent). `at_seq` is allocated in the report-insert tx (R6); an idempotent
re-add consumes none.
**Telemetry-class eviction only** — M13 has nothing pinned (every row `pinned=0`), but the
`pinned=0` clause is present so M13b's pin is a value flip. (M13b will set + exempt a
pinned ratchet baseline; the retention query is written so that exemption is a one-clause
addition.) Eviction is logged as a count, never silent (§ honesty: a truncation that hides
data must be visible).

---

## 6. Faces

New top-level grouped verb `test-report` (mirroring `find`/`req`): operations `add | ls |
show | flaky`. `add` = `SafetyMutate` (writes telemetry); `ls`/`show`/`flaky` = `SafetyRead`.
`MCPTool: aira_test_report`, `MCPOperation: subverb`. Descriptor `Summary`/`Safety`/`Include`/
`Operations`/`Example` per the M8 dispatch model; **every example machine-verified** (drift +
parity + full-coverage E2E, per the M8b lesson — a generated example that doesn't parse/reach
core is a shipped lie). Register new codes in `store.ExitCodes` / `core.ResponseContract`:
`E_TESTREPORT_INVALID` (2, malformed input), `U_TESTREPORT_INCOMPARABLE` (3, the flaky-
unevaluated code), and `E_TESTREPORT_FLAKY` (the reconciliation-finding code, R5 — a
`flaky-test` DB-resident finding, surfaced at `check` like other reconciliation records;
its exit-class placement follows the existing reconciliation-finding convention).

---

## 7. Honesty invariants (the properties the matrix defends)

1. **parser-incomplete never compared.** A `parser_complete=false` report is stored but
   contributes to no flaky/ratchet verdict (→ `unevaluated`), never a false pass/flaky.
2. **flaky requires same commit AND same config.** Cross-config pass/fail is *not* flaky
   (concurrency-sensitivity). A drop of the config guard is the load-bearing bug.
3. **retries are not first-passes.** A passed `retry_index>0` result never turns a failing
   first-pass into "flaky-resolved" or a pass.
4. **insufficient evidence ≠ clean.** A single/all-same/incomparable test is absent from the
   flaky set and reportable as `unevaluated`, never asserted clean.
5. **malformed input refuses.** Malformed XML/JSON → `E_TESTREPORT_INVALID`, never a partial
   silent parse producing a bogus green report.
6. **idempotent ingest.** Byte-identical re-add is a no-op (no corpus inflation, no
   double-counting in flaky).
7. **eviction is visible.** Retention drops are counted/reported, never silent.
8. **ids allocated, never hand-picked** (local counter, `RUN-n` precedent).

---

## 8. Adversarial test matrix (every confirmed counterexample → a regression test)

Parser/ingest:
- **T1 go-json round-trip** — a real `go test -json` stream (pass+fail+skip) ingests with
  correct per-test outcomes + `parser_complete=true`.
- **T2 go-json truncated** — a stream cut mid-test → `parser_complete=false`, stored,
  excluded from flaky.
- **T3 JUnit round-trip** — failure/error/skipped/pass + `time` map correctly.
- **T4 malformed XML/JSON → `E_TESTREPORT_INVALID`** (refuse, no partial report written).
- **T5 idempotent add** — byte-identical re-add returns the same id, corpus size unchanged;
  distinct bytes same-identity → a new report.
- **T6 atomic ingest** — a forced mid-ingest failure leaves zero partial rows.

Flaky (the honesty core):
- **T7 flaky same-commit-same-config** — pass in R1, fail in R2 (same commit/suite/config/
  env/shard, both first-pass, both complete) → flaky, witnesses R1+R2.
- **T8 NOT flaky cross-config** — pass under config A, fail under config B, same commit →
  **not** flaky (T2 §13 concurrency-sensitivity). The load-bearing negative.
- **T9 retries excluded → unevaluated** — a first-pass fail + a retry (`retry_index>0`) pass,
  no other first-pass → **`unevaluated`** (R7), not "not flaky" and not "clean": a flaky claim
  rests only on first-pass disagreement, and one first-pass result is insufficient.
- **T10 parser-incomplete excluded** — a pass + a fail where one report is
  `parser_complete=false` → the incomplete one doesn't count; if that leaves <2 comparable →
  **unevaluated**, never flaky/clean off the incomplete report.
- **T11 single result → unevaluated (≠ clean)** — one result for a test → **`unevaluated`**;
  `flaky --explain <name>` reports `unevaluated` + reason, never "clean". Two comparable
  all-pass first-pass results → **clean**.
- **T12 missing commit excluded** — an empty-`commit` report is incomparable (R1) →
  `unevaluated`, not compared.
- **T15 JUnit classname identity** — two `<testcase>` with the same `name` but different
  `classname` are **distinct** tests (name = `classname`-qualified); a pass on one and fail on
  the other is **not** flaky.
- **T16 per-component mismatch negatives (R1/R8)** — pass+fail differing only in `suite_id`
  (then only in `env_digest`, then only in `shard`) each → **not comparable** →
  **unevaluated**, not flaky. Not only the `config` case (T8).
- **T17 evict-a-witness recomputes + clears the finding (R8/R5/R9)** — a flaky pair, then
  retention evicts one witness → the cell drops below 2 comparable → `flaky` recomputes to
  **unevaluated** AND the `ReconcileFlaky` replacement **removes** the now-stale
  `E_TESTREPORT_FLAKY` reconciliation finding (assert it is gone from the DB).

Retention + faces:
- **T13 retention evicts oldest, exempts nothing (M13), counts the drop** — cap N, ingest
  N+1 → oldest gone, results cascade-deleted, drop count reported.
- **T14 faces** — drift (handler reads == declared op args), parity (Example → same
  core.Request via CLI + MCP), E2E (every Example reaches core, not a parse error), and the
  agent-guide example is **valid**.

Real-binary e2e (Opus/Fable final, post-build): `aira init`; `test-report add --format
go-json` a real go-test stream; `ls`/`show` it; add a second same-commit-same-config report
with a flipped outcome → `flaky` lists the test; add a cross-config report → still only the
same-config cell flaky; malformed input → `E_TESTREPORT_INVALID`.

---

## 9. Layering & files

- `internal/domain/testreport.go` — `TestReport` + `TestResult` types, outcome enum,
  validation (`E_TESTREPORT_INVALID`), and the pure normalisation targets.
- `internal/store/testreport.go` — schema (reports + results + `test_report_counter` +
  `at_seq` counter, all telemetry), `AddTestReport` (atomic insert → retention evict →
  `ReconcileFlaky`, one logical op), `ListTestReports`/`GetTestReport`, `FlakyTests` (read-only
  comparability + aggregation), `ReconcileFlaky` (recompute + full-replace the
  `E_TESTREPORT_FLAKY` reconciliation findings, R5/R9), local `TR-n` counter. `check` calls
  `ReconcileFlaky` alongside the existing reconcilers.
- `internal/store/testreport_parse.go` — `parseGoTestJSON` (extend M10b) + `parseJUnitXML`,
  dispatched by format; `parser_complete` determination.
- `internal/core/core.go` — the `test-report` grouped descriptor + handler + metadata (agent
  guide); **extend the core `Store` interface** with `AddTestReport`/`ListTestReports`/
  `GetTestReport`/`FlakyTests` (the seam `core.Do` calls) (R4).
- `cmd/aira/main.go` — `parseArgs`/`buildRequest` gain the `test-report` verb + subverbs +
  options; `add` reads the report from **stdin** (positional `-` or absent) **or a file arg**
  (a path); `flaky` gains `--explain` (R4).
- config plumbing in `internal/app/project.go` (`test_reports.max_reports`/`max_age_days`).
- Schema forward-hook: the reports table carries `pinned INTEGER NOT NULL DEFAULT 0` now
  (unused in M13; M13b flips it, no migration — R6).
- No runner/gate/telemetry-compute/daemon. No git-durable or common-dir write. No journal
  event per report ingest (reports are not §11-significant, like runs/spend).

## 10. Build plan (delegated to Codex, frequent commits, TDD)

1. domain `TestReport`/`TestResult` + validation (+ tests).
2. store schema + `AddTestReport` (atomic, idempotent) + go-json parser (reuse M10b) + T1/T2/
   T5/T6.
3. JUnit parser + T3/T4.
4. `FlakyTests` comparability + flaky logic + T7–T12 (the honesty core — heaviest tests).
5. retention (T13) + config plumbing.
6. `test-report` faces + metadata + agent-guide (T14) + full `make ci` green.

Gate: Sol plan-review (this doc) → approve → Opus/Fable plan-final → build → Sol build-review
(weight the false-**green** direction: a bogus flaky-clean or a parser-incomplete counted as
comparable) → approve → Opus/Fable final + real-binary e2e → merge `--ff-only`.
