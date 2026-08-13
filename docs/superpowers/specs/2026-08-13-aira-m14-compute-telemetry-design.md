# AIRA M14 — compute telemetry (phase- and model-attributed) (design)

Status: DRAFT (pre plan-review). Author: Opus (coordinator). Milestone: Phase 4 · M14.
Base: master `92248f9`. Building delegated to Codex; gate order (owner): Sol plan-review →
approve → Opus/Fable plan-final → build → Sol build-review → approve → Opus/Fable final →
merge.

Authoritative parent: `docs/superpowers/specs/2026-08-07-aira-design.md` §12 (compute
telemetry), §6 (`ComputeEvent` / `QuotaSnapshot` — operational telemetry), §5.3 (durability),
§9 (the phase vocabulary), §17 (the insights this feeds — deferred to M15). §21 leaves cost
derivation undecided (deferred).

## 1b. Sol plan-review resolutions (thread 019ff9b3 — BLOCK, all incorporated)

- **R1 (P0) — conservation is TRI-STATE, `reported_total` is nullable.** Replace `conserved
  bool` with `conservation ∈ {checked, mismatch, unevaluated}`. `unevaluated` = no
  `reported_total`, OR a present-bucket set too incomplete to check. `checked` = a total was
  present AND the present disjoint buckets reconcile. `mismatch` = a total present and the
  present buckets do not reconcile. **The check is presence-aware: absent buckets are never
  summed as 0**, so a partial payload with a total does not falsely `mismatch` — it is
  `unevaluated` for conservation unless the present buckets alone exceed the total (a genuine
  impossibility ⇒ `mismatch`). (§2.1, §3.)
- **R2 (P0) — presence-aware normaliser truth table; NEVER fabricate a bucket.** OpenAI/
  Gemini/Codex derivations only run when BOTH operands are present: `fresh_input = prompt −
  cached` **only if both `prompt` and `cached` are present**; if `cached` absent ⇒ `fresh_input
  = prompt` is WRONG (cached unknown) ⇒ leave `fresh_input` **unevaluated** and `cache_read`
  unevaluated (a payload that omits cache detail cannot claim `cache_read=0`). `cached > prompt`
  ⇒ `fresh_input` **unevaluated** (never negative/clamped-0); the impossibility yields
  `conservation=mismatch` **only when `reported_total` is present** (a conservation outcome
  requires a total, R1), else `conservation=unevaluated`. Each provider
  gets an explicit present→derived truth table; a derived bucket is emitted only when justified
  by present inputs. (§3; T-truth-table.)
- **R3 (P0) — conservation finding lifecycle = M13b disposable projection.** In the SAME DB tx
  as insert/retention-evict, **delete all `E_COMPUTE_CONSERVATION` reconciliation findings for
  the project and regenerate them from the retained `conservation=mismatch` events**, keyed
  deterministically by `CE-id` (subject encodes the CE id). `check` runs this reconcile before
  reporting. Findings are built ONLY via `NewReconciliationFinding` (`Code=E_COMPUTE_CONSERVATION`,
  `Subject`, `Details` — no review fields), classified as **warnings, never ingest failures**.
  A finding never outlives its event: evicting the event removes its finding on the next
  reconcile. (§3, T11.)
- **R4 (P1) — exact per-provider parser schemas.** Anthropic: `input_tokens ·
  cache_read_input_tokens · cache_creation_input_tokens · output_tokens` (separate). OpenAI:
  `prompt_tokens · prompt_tokens_details.cached_tokens · completion_tokens ·
  completion_tokens_details.reasoning_tokens`. Gemini: `promptTokenCount ·
  cachedContentTokenCount · candidatesTokenCount · thoughtsTokenCount`. **Reported-total field:**
  OpenAI `total_tokens`; Gemini `totalTokenCount`; Anthropic has none (leave `reported_total`
  absent unless supplied). **Codex** `--json` emits an OpenAI-style `usage` (`prompt_tokens`/
  `completion_tokens`/`total_tokens` + `prompt_tokens_details.cached_tokens`/`completion_tokens_
  details.reasoning_tokens`) — reuse the OpenAI parser; the build MUST verify the exact field
  names against `~/repos/codex` and add a dedicated parser if Codex diverges (a written-down
  build step, not left open). For EACH provider state whether **`reasoning` is additive to
  `output` or a
  subset of it** (drives the conservation sum): OpenAI/Codex reasoning is a *subset* of
  completion/output; Gemini `thoughtsTokenCount` is *additive* (separate from candidates) —
  encode per-provider. Unknown-provider explicit `--bucket`s carry an explicit disjointness
  contract (the caller asserts they are already-disjoint; reasoning additivity declared via
  `--reasoning-subset`/default additive). (§3.)
- **R5 (P1) — identity/counters/retention resolved.** Spend ingestion is **intentionally
  non-idempotent** — every successful `spend add` creates a distinct `CE` (no source-digest
  dedup; a re-run of the same phase IS new spend). `CE-n` and `QS-n` (quota) are **separate DB
  transaction-local per-project counters**; `at_seq` is a **per-table** ingress counter (not
  shared) for each table's retention order. Retention: `compute.max_events` (default 20000) +
  `compute.max_age_days` (0=off) for events; `compute.max_quota_snapshots` (default 5000) for
  quota — the two-DELETE pinned-free pattern from M13 (nothing pinned here). (§2, §5.)
- **R6 (P1) — NO stored aggregates/trend numerals.** `spend ls`/`quota ls` return **raw rows**
  only (optionally a `--by` distribution computed live, like `find ls --by`, never persisted).
  No stored sum/average/trend column. Insight gauges stay live queries (D1, §17). (§4.)
- **R7 (P1) — input handling contract.** `spend add` input precedence + mutual exclusion:
  exactly one of {`--usage-file <path>`, stdin payload, one-or-more `--bucket k=v`}; combining
  a payload with `--bucket` ⇒ `E_COMPUTE_INVALID`. `main.go`'s stdin path (currently
  test-report-only) is extended for `spend`; the option map must **preserve repeated
  `--bucket`** (a list arg) and **reject a duplicate bucket key** ⇒ `E_COMPUTE_INVALID`. Bucket
  values parse as non-negative int64; an explicit `0` is a real zero, a missing bucket is
  absent. Provider payloads are parsed with strict schemas (unknown top-level fields tolerated
  — vendors add fields — but the token fields are read by exact name per R4). (§4.)
- **R8 (P1) — added tests.** reasoning subset-vs-additive per provider; partial payloads
  (missing cached ⇒ fresh_input unevaluated, not fabricated); nullable DB round-trips (absent
  bucket stays absent through store→load); `reported_total=0`; unknown-provider explicit
  buckets + reasoning contract; duplicate-`spend add` ⇒ two distinct CEs; finding DISAPPEARS
  after the offending event is evicted by retention. (§5.)

---

## 1. Scope

Per-ticket LLM compute, phase- and model-attributed, ingested **out-of-core**, so
review-loop economics and estimate-vs-actual become real (§17). DB-only, retention-capped
telemetry (§5.3).

1. **`ComputeEvent` schema + DB storage** — a superset schema with **disjoint buckets**
   (`fresh_input · cache_read · cache_write · output · reasoning`), attributed to a ticket,
   a §9 phase, a model/provider.
2. **The disjoint-bucket normaliser (the §12 HARD CONSTRAINT)** — providers report "cached"
   differently (a *subset* of input on OpenAI/Gemini/Codex; *separate* on Anthropic). The
   ingester normalises to disjoint buckets and, **on a conservation mismatch vs the reported
   total, records the datum anyway and raises a reconciliation finding** — a conservation
   *warning*, **never** a fail-closed drop of drifting vendor telemetry.
3. **`aira spend add`** — ingest one event from a provider usage payload (the caller extracts
   it from Claude Code `Stop`/`SubagentStop` usage or OTEL, Codex `--json`, or a direct-API
   response `usage`; **Antigravity uncovered** — manual reports accepted). **AIRA records what
   an ingester hands it, never scrapes.**
4. **`aira quota`** — record an opt-in provider-supplied `QuotaSnapshot` (a burn-rate gauge).
5. **`aira spend ls`** — query the events (raw data; the trended *gauges* are M15).
6. **Retention cap** (telemetry class).

### 1.1 Non-goals / deferrals (written down; do not build)

- **D1 — the §17 insight GAUGES (review-loop economics, estimate-vs-actual, quota-burn-rate
  trend) → M15.** M14 provides the *data* + the raw `spend ls`/`quota ls` query; the drillable
  live-query gauges are the insights milestone.
- **D2 — automatic capture hooks** (a Claude Code `Stop` hook, an OTEL collector). Capture is
  **out-of-core** by design — the caller/harness extracts usage and calls `spend add`. M14
  ships the ingest verb + the parsers for the common provider payload shapes, not the hooks.
- **D3 — cost derivation (price table vs harness `cost_usd`) → §21 undecided, deferred.** M14
  records token buckets + an optional caller-supplied `cost_usd` passthrough, never derives a
  price.
- **D4 — estimate side of estimate-vs-actual** (a per-ticket estimate field) → M15/later.
- **D5 — no journal event per spend/quota ingest** (like runs/test-reports, they would swamp
  the journal, §11). The one derived write is the DB-resident conservation reconciliation
  finding (§3).

---

## 2. Data model

### 2.1 `ComputeEvent` — DB telemetry, retention-capped

`{id, ticket?, phase?, model, provider, at, session?, agent?, source, buckets{fresh_input?,
cache_read?, cache_write?, output?, reasoning?}, reported_total?(*int64), cost_usd?,
conservation(checked|mismatch|unevaluated), at_seq}`

- **Buckets are `*int64` (presence-distinguishing)** — a bucket the provider **did not
  report** is **absent (`unevaluated`), NOT zero** (§12: provider-absent → unevaluated, never
  zero). A reported `0` is a real zero. This is the load-bearing honesty distinction.
- `phase` ∈ the §9 vocabulary (`plan · plan-review · plan-fix · implement · work-review ·
  work-fix`) or empty; validated against the closed set.
- `provider` — an open registered vocabulary (`anthropic · openai · gemini · codex ·
  deepseek · …`), like the finding `source` field; the normaliser dispatches on it.
- `source` — the ingest origin (`claude-code-usage · codex-json · api-usage · manual`).
- `reported_total` — the provider's own reported total tokens, **nullable** (`*int64`); used
  only for the conservation check.
- `conservation` — tri-state `{checked, mismatch, unevaluated}` (R1), NOT a bool. `unevaluated`
  = no `reported_total` or too few present buckets to check; `checked` = total present and the
  **present** buckets reconcile (absent buckets never summed as 0); `mismatch` = total present
  and present buckets cannot reconcile (or present buckets alone exceed the total). A
  `mismatch` datum is still stored + raises a finding (§3).
- `id` = `CE-n` from a DB transaction-local per-project counter (the M13 `TR-n` / M12 `RUN-n`
  precedent — never the git allocator, never hand-picked). `at_seq` = a **per-table**
  monotonic ingress sequence for retention order (R5; not shared with the quota table).

### 2.2 `QuotaSnapshot` — DB telemetry

`{id, provider, at, window?, used?, limit?, remaining?, reset_at?, source, at_seq}` — an
opt-in point-in-time burn gauge the ingester hands AIRA. Numeric fields are `*` (absent ≠
zero). AIRA records it verbatim; it never scrapes a provider.

### 2.3 Durability

Both are **DB-only, retention-capped operational telemetry** (§5.3) — no git-durable/
common-dir write, **no journal event per ingest** (D5). The only derived write is the
DB-resident conservation reconciliation finding (§3).

---

## 3. The disjoint-bucket normaliser (§12 hard constraint — the correctness core)

`NormalizeUsage(provider string, raw RawUsage) (buckets, reported_total *int64, conservation, error)`
where `conservation ∈ {checked, mismatch, unevaluated}` (R1).
`RawUsage` is the provider-neutral parsed payload the ingester supplies.

- **Anthropic** — `input_tokens`, `cache_read_input_tokens`, `cache_creation_input_tokens`,
  `output_tokens` are already **separate/disjoint** → map directly (`fresh_input =
  input_tokens`; `cache_read`/`cache_write`/`output` direct). A field absent ⇒ that bucket
  absent (unevaluated), not zero.
- **OpenAI / Codex** — "cached" is a **subset of input** (`prompt_tokens` incl. cached +
  `prompt_tokens_details.cached_tokens`; `completion_tokens`;
  `completion_tokens_details.reasoning_tokens` — reasoning a **subset of output**).
  **Presence-aware (R2): derive only when both operands present.** `fresh_input = prompt −
  cached` **only if both present**; if `cached` absent ⇒ `fresh_input` and `cache_read` are
  **unevaluated** (a payload omitting cache detail cannot claim `cache_read=0` or `fresh_input=
  prompt`). `cached > prompt` ⇒ `fresh_input` **unevaluated** (never a negative or a clamped 0);
  `conservation=mismatch` only if `reported_total` present, else `unevaluated` (R1/R2).
- **Gemini** — `promptTokenCount · cachedContentTokenCount · candidatesTokenCount ·
  thoughtsTokenCount`; `thoughtsTokenCount` (reasoning) is **additive** (separate from
  candidates/output). Same presence-aware derivation for the cached subset.
- **Anthropic reasoning** is not separately reported (folded in output); leave `reasoning`
  unevaluated unless a caller supplies it explicitly.
- Every provider has an explicit **present→derived truth table**; a derived bucket is emitted
  **only when justified by present inputs**, else it is unevaluated (R2). A field absent ⇒
  bucket absent, never 0.
- **Conservation check (R1, tri-state, presence-aware)** — only when `reported_total` is
  present AND enough disjoint buckets are present to bound the sum. Sum the **present**
  disjoint buckets, respecting per-provider reasoning additivity (subset ⇒ not added; additive
  ⇒ added). `checked` if they reconcile; `mismatch` if present buckets exceed the total or a
  present-set that should be complete does not reconcile; `unevaluated` if too few buckets are
  present to judge (absent buckets are **never** summed as 0). On `mismatch` ⇒ **store the
  event anyway** and raise a DB-resident reconciliation finding (`E_COMPUTE_CONSERVATION`, R3)
  naming the CE id, provider, computed present-sum, and reported total. **Never drop the datum,
  never fail the ingest** (§12 explicit — a conservation *warning*).
- **Unknown provider** ⇒ store the raw buckets **only if the caller supplied already-disjoint
  buckets explicitly**; otherwise `E_COMPUTE_PROVIDER_UNKNOWN` (refuse to *guess* a
  normalisation). A caller may always pass explicit disjoint buckets directly (the escape
  hatch), which are stored verbatim (conservation-checked if a total is given).

**Honesty invariants:**
1. **Provider-absent bucket ⇒ absent/unevaluated, never zero.** A missing `output` is not
   `output=0`.
2. **Conservation mismatch ⇒ record + finding, never drop** (§12 explicit — a warning, not a
   fail-closed drop of drifting vendor telemetry). The finding is DB-resident reconciliation.
3. **Never scrape/infer.** AIRA normalises what it is *handed*; an unknown provider without
   explicit disjoint buckets is refused, not guessed.
4. **Deterministic normalisation** per (provider, raw) — no clock, pure.

---

## 4. Faces

- `aira spend add --provider P [--ticket ID --phase PH --model M --source S --cost-usd C]
  [--total N] [--reasoning-subset] (--usage-file f | < payload | --bucket fresh_input=…
  --bucket cache_read=… …)` — `SafetyMutate`. **Exactly one** input mode (R7): `--usage-file`,
  stdin payload, or one-or-more `--bucket k=v` (repeated, duplicate key ⇒ `E_COMPUTE_INVALID`);
  a payload combined with `--bucket` ⇒ `E_COMPUTE_INVALID`. `--reasoning-subset` (for explicit
  `--bucket`s / unknown provider) declares reasoning is a subset of output for the conservation
  sum (default additive). Normalises, stores, emits the conservation finding on mismatch.
  Reports `{id, buckets, conservation}`.
- `aira spend ls [query]` — `SafetyRead`, raw events (by ticket/phase/provider). Distribution
  by field like `find ls --by`.
- `aira quota add --provider P [--used --limit --remaining --reset-at --window --source]` +
  `aira quota ls` — record/list snapshots.
- Grouped verb `spend` (add|ls) + `quota` (add|ls), mirroring `find`/`req`; descriptor
  Summary/Safety/Include/Operations/Example; **every example machine-verified** (drift +
  parity + full-coverage E2E, the M8b lesson). Register codes `E_COMPUTE_INVALID` (2),
  `E_COMPUTE_PROVIDER_UNKNOWN` (2), `E_COMPUTE_CONSERVATION` (the reconciliation-finding code),
  `U_COMPUTE_UNEVALUATED` (3, a bucket read as unevaluated where a caller expected a value).

---

## 5. Adversarial test matrix (every confirmed counterexample → a regression test)

Normaliser (the honesty core):
- **T1 Anthropic direct** — separate input/cache_read/cache_write/output map to disjoint
  buckets; a total that reconciles ⇒ `conservation=checked`.
- **T2 OpenAI subset** — `prompt=1000, cached=300, completion=200` ⇒ `fresh_input=700,
  cache_read=300, output=200`; total 1200 reconciles ⇒ `conservation=checked`.
- **T3 conservation mismatch ⇒ record + finding, NOT dropped** — present buckets that exceed
  `reported_total` ⇒ event stored, `conservation=mismatch`, an `E_COMPUTE_CONSERVATION`
  reconciliation finding created; the ingest still succeeds (the load-bearing test).
- **T4 provider-absent ≠ zero** — a payload with no `output` ⇒ `output` bucket **absent**
  (nil), not `0`; `spend ls` shows it unevaluated, `spend add` never fabricates 0.
- **T5 reported `0` is a real zero** — an explicit `output_tokens: 0` ⇒ `output=0` (present),
  distinct from T4's absent.
- **T6 cached > input (drift)** — `cached=1200 > prompt=1000` ⇒ `fresh_input` **unevaluated**
  (never negative, never clamped-0); WITH a `reported_total` ⇒ `conservation=mismatch` + finding,
  stored; WITHOUT a total ⇒ `conservation=unevaluated`, stored (no finding). Both keep the
  event.
- **T6b partial payload ⇒ unevaluated, not fabricated/mismatch** — OpenAI payload with
  `prompt` but NO `cached` ⇒ `fresh_input`+`cache_read` unevaluated (not `fresh_input=prompt`);
  with a total, `conservation=unevaluated` (not a false mismatch).
- **T6c reasoning additivity** — OpenAI/Codex reasoning is a subset of output (not added to the
  sum); Gemini `thoughtsTokenCount` is additive — each provider's conservation sum respects it.
- **T7 unknown provider** — no explicit buckets ⇒ `E_COMPUTE_PROVIDER_UNKNOWN` (refuse, no
  guess); explicit `--bucket`s ⇒ stored verbatim (`--reasoning-subset` honoured).
- **T8 phase validation** — an unknown `--phase` ⇒ `E_COMPUTE_INVALID` (closed §9 set).
- **T9 non-idempotent + retention (R5)** — two `spend add` of the same payload ⇒ **two distinct
  CEs** (no dedup); retention evicts oldest by per-table `at_seq`, counts the drop.
- **T10 quota round-trip** — `quota add`/`ls` records verbatim; absent numeric fields absent.
- **T11 conservation finding lifecycle (R3)** — a DB-resident `E_COMPUTE_CONSERVATION`
  reconciliation finding appears at `check`; after the offending event is **evicted by
  retention**, the next reconcile (at `add`/`check`) **removes** the finding (disposable
  projection keyed by CE id) — assert it disappears; the finding never outlives its event.

Faces: **T12** drift/parity/E2E for `spend`/`quota` + valid agent-guide examples.

Real-binary e2e (Opus/Fable final): `aira spend add` an Anthropic payload + an OpenAI payload;
`spend ls` shows disjoint buckets + a provider-absent bucket as unevaluated; a mismatched
payload records + surfaces `E_COMPUTE_CONSERVATION` at `check`; `quota add`/`ls` round-trips.

---

## 6. Layering & files

- `internal/domain/compute.go` — `ComputeEvent`, `QuotaSnapshot`, bucket types (`*int64`),
  phase/provider validation, `RawUsage` + the pure `NormalizeUsage` (per-provider), the
  conservation check.
- `internal/store/compute.go` — schema (2 tables + `CE-n`/`at_seq` counters, telemetry),
  `AddComputeEvent` (normalise → store → conservation finding), `ListComputeEvents`,
  `AddQuotaSnapshot`/`ListQuotaSnapshots`, retention, the reconciliation-finding write via
  `NewReconciliationFinding` (`E_COMPUTE_CONSERVATION`), wired into `check`.
- `internal/core/core.go` + `cmd/aira/main.go` — `spend`/`quota` grouped verbs + Store iface
  + metadata/agent-guide; stdin/`--usage-file`/`--bucket` input handling.
- `internal/app/project.go` — retention config (`compute.max_events` default 20000 /
  `compute.max_age_days` 0=off / `compute.max_quota_snapshots` default 5000) (R5).
- Register the new codes. No runner/gate/daemon. No git-durable/common-dir write.

## 7. Build plan (delegated to Codex, TDD, frequent commits)

1. domain `ComputeEvent`/`QuotaSnapshot` + `NormalizeUsage` per-provider + conservation
   (T1–T8 — the honesty core, heaviest tests).
2. store schema + `AddComputeEvent` (normalise → store → conservation finding) + counters +
   retention (T3, T9, T11).
3. `AddQuotaSnapshot` + list queries (T10).
4. `spend`/`quota` faces + metadata/agent-guide (T12) + config plumbing + full `make ci`.

Gate: Sol plan-review (this doc) → approve → Opus/Fable plan-final → Codex build (sandbox-
verifiable: pure Go + SQLite over temp dirs) → Sol build-review (weight the honesty core: a
dropped datum, a fabricated zero, a silently-outlived finding) → approve → Opus/Fable final +
real-binary e2e → merge `--ff-only`.
