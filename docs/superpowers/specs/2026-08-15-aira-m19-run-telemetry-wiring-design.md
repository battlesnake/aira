# AIRA M19 — run → telemetry auto-wiring (report + compute + honest observation)

Status: PLAN (v2 — Sol plan-review r1: drop gate attestation (a run-observed pass is not
§118 proven-to-fire), add report identity fields, truncation→parser_complete=false, --usage
authoritative tokens, strict-wiring opt-in)
Date: 2026-08-15
Milestone: Phase 5 · M19 — telemetry/gate auto-wiring
Depends on: M12 runner, M16 rusage, M13 test-reports, M14 compute telemetry (all landed, `bd965be`)
Design authority: 2026-08-07-aira-design.md §14 (Launch/Wiring 146,155), §12, §13, §9 (line 118
proven-to-fire; line 120 tests-green ⇐ exit0 AND count>0).

## 0. The gap, and the honest boundary M19 respects

An agent must run `test-report add` and `spend add` separately to make a run count. §14 line 155
wires those onto `aira run`. But §9 line 118 forbids marking a **checkable gate** green without a
**dated, evidence-linked, decaying proven-to-fire** attestation — and a `--report` run only proves
**one run passed**; it never fired the gate's lane on a known-bad input (Sol plan-review r1 P0).
So M19 deliberately **does not attest any gate verdict**. It:

1. produces the durable §13 **report** (the input a gate/ratchet later consumes), and
2. records honest **run-observation facts** (`tests_green_observed` = exit0 AND count>0),

and leaves the proven-to-fire gate verdict where it belongs — the M10b command-gate's own lane +
continuous canary. M19 is telemetry wiring; the "gate" half is *feeding* gates, not deciding them.

## 1. Scope

**In:**
- **Run provenance**: `Request.{Ticket,Phase,Label,Tool}` + `RunRecord.{Ticket,Phase,Label,Tool}`;
  faces `--ticket/--phase/--label/--tool` (CLI+MCP). Recorded on the first ledger event, carried
  in `mergeEvidence` (the M16/M18a field-loss trap). Pure metadata — never alters launch/capture.
- **`--report <fmt>` auto-ingest** with the §6 identity fields as inputs so the report is
  *comparable*: `--suite <id>`, `--config-env K=V` (→ env-scoped config digest, as M13),
  `--shard <s>`, `--retry <n>` (default 0). After the run terminates, read the **full** captured
  output (§3.2 — uncapped, with truncation detection), build a `domain.TestReportInput` with
  provenance (`RunRef=<id>`, `TicketID/Phase` from flags, `Commit/Branch/WorktreeID` from the
  store git context, `SuiteID/Config/Shard/RetryIndex/EnvDigest`, `Format`, `Raw`), and
  `AddTestReport` (the M13 parser fills `Results/ParserComplete`). **Comparability honesty**: when
  a required §6 identity field (`suite_id`, config, shard) is unknown, the report is still
  recorded but is **ineligible for flaky/ratchet comparison** — this is already M13's rule (those
  require all identity fields non-empty+equal); M19 does not fabricate identities to force
  comparability.
- **`--tool <t>` ComputeEvent** keyed `(Ticket,Phase,Model=<tool>)` capturing **resource** usage
  (wall=`ended-started`, `cpu_user/cpu_sys/peak_rss` from M16; nil→unevaluated). Authoritative
  **token** usage is recorded **only from an explicit `--usage <file|->`** (a provider usage JSON,
  parsed by M14's per-provider normaliser — reusing M14's honesty: never fabricate a bucket);
  absent `--usage` ⇒ token buckets **unevaluated** (§14 "absent → …unevaluated"). Auto-extracting
  usage from the tool's stdout is D1 (fragile per-tool scraping) — deferred, never silently
  guessed.
- **Run-observation facts** (not a gate verdict): the run response reports
  `tests_green_observed` = (`exit==0` AND the ingested report's `test_count>0` AND
  `parser_complete`), else the honest reason (`exit≠0` / `zero-tests` / `parse-incomplete` /
  `no-report`). This is a *run observation*, explicitly **not** a gate attestation (§0).
- **Wiring outcome + opt-in strictness**: a `wiring` block on the response with a **stable code
  per auto-action** and an overall `wiring_complete` bool (§4). By default a wiring failure is a
  **warning** and does not fail the run (the launch record is authority). An opt-in
  `--strict-wiring` makes an *incomplete* wiring a non-zero **AIRA** exit (the child already ran;
  AIRA signals the telemetry gap) for workflows that require the evidence.

**Out (deferred, written down):**
- **D1 tool stdout usage auto-extraction** — v1 takes authoritative tokens only via `--usage`.
- **D2 gate attestation / verdict** — a run observation is not §118 proven-to-fire; the gate
  verdict stays with the M10b command-gate lane + canary. M19 feeds gates the report only.
- **D3 detach/shim**, **D4 run-input**.

## 2. Invariants (each → a discriminating test)

- **I1 — metadata orthogonal.** `--ticket/--phase/--label/--tool` change only recorded fields; a
  run with and without them has identical `env_digest`, capture, and exit behaviour.
- **I2 — report reflects the REAL parse.** A clean `go-json`/`junit` capture → N results,
  `parser_complete=true`; a malformed capture → `parser_complete=false` (report kept, comparison-
  excluded); an **incomplete capture** (`CaptureComplete=false`) OR a **truncated read** →
  `parser_complete=false`, never true (§3.2).
- **I3 — comparability is honest.** A report missing a required §6 identity field is recorded but
  flagged ineligible for flaky/ratchet comparison (M13's rule); M19 never fabricates suite/config
  to force a comparison.
- **I4 — `tests_green_observed` obeys §120 and is a fact not a verdict.** exit0 + count>0 +
  parser_complete → observed; exit0 + **zero tests** → not-observed(`zero-tests`); exit≠0 → not.
  The response never labels this a gate pass; no gate audit row is written.
- **I5 — tokens honestly unevaluated.** A `--tool` run without `--usage` → ComputeEvent with real
  resource usage and token buckets nil/unevaluated (never a fabricated 0); with `--usage` → the
  M14 normaliser fills buckets, still never fabricating an absent one.
- **I6 — wiring failures never corrupt the run.** An injected report/compute failure → a coded
  warning + `wiring_complete=false`; the run record (status/exit/capture) is unchanged and the run
  is not re-run. Default exit is the child's; `--strict-wiring` makes AIRA exit non-zero on
  incomplete wiring (documented, opt-in).
- **I7 — no gate-lane recursion.** The wiring runs in the `aira run` core handler after `Launch`;
  the M10b command-gate builds its `runner.Request` without M19 flags, so a gate's own command run
  emits no M19 report/ComputeEvent.

## 3. Design

### 3.1 Where the wiring runs

The `run` **core handler** (`internal/core/core.go`), after `c.runner.Launch` returns a terminal
`RunRecord`, wires against `c.store`. The runner stays a pure recorder (§14 line 146) — only the
four metadata fields change there. Each auto-action is gated on its flag; the whole block is
absent for the gate lane (I7).

### 3.2 `--report` full-capture read with truncation honesty (Sol r1 P0)

The report must parse the **whole** captured stream, and any dropped bytes must force
`parser_complete=false`:
- Read the captured `out`/merged stream **uncapped** (the M12 capture file holds the full bytes;
  read the file, not a capped `ReadOutput`). If a read bound is unavoidable, `ReadOutput` must
  **return truncation metadata** and any truncation → `parser_complete=false`.
- If `RunRecord.CaptureComplete=false` (forced-close/partial capture, e.g. an M17/M18b bounded
  drain) → `parser_complete=false` regardless of what parsed. A syntactically-valid prefix of a
  truncated capture must **never** yield `parser_complete=true`.
- Build `TestReportInput` (provenance + identity §1), `AddTestReport`. Error → coded warning (I6).

### 3.3 `--tool` ComputeEvent + authoritative `--usage`

`domain.ComputeEventInput{TicketID,Phase,Model=<tool>,At,Source="run",Raw:<resource usage>}`.
Resource fields from the record (nil→unevaluated). If `--usage <file|->` is given, its provider
usage JSON is parsed by the **M14 normaliser** and merged into `Raw` (tokens authoritative,
still never fabricating an absent bucket). `AddComputeEvent`. Error → coded warning (I6).

### 3.4 Run-observation facts, NOT a gate verdict (the §118 resolution)

M19 records `tests_green_observed` as a **run-level observation** on the response and (optionally)
as a field on the ingested report — it is exit0 AND report.test_count>0 AND parser_complete. It is
**not** written to the gate audit, **not** an `AttestGate` call, and **not** a
proven-to-fire attestation, because a run that passed once has not fired the gate's lane on a
known-bad input (§118). A caller that wants a gate verdict uses the M10b command-gate (its own
lane + canary) or pins this report into a ratchet baseline (M13b) — both consume the report M19
produced. This keeps M19 decision-complete and §118-honest.

## 4. Faces + response

- CLI `aira run`: `--ticket <id> --phase <p> --label <l> --tool <t> --report <fmt> --suite <id>
  --config-env K=V --shard <s> --retry <n> --usage <file|-> --strict-wiring`.
- MCP `aira_run`: the same as string/bool/list args.
- Response `wiring` block: `{report:{id?,parser_complete?,comparable?,code}, compute:{id?,tokens:
  authoritative|unevaluated,code}, tests_green_observed:{observed|not:reason}, wiring_complete:bool,
  warnings:[{action,code,message}]}`. Every auto-action's outcome — done / skipped-why / failed-why
  — is visible with a stable code; no silent success or drop. `--strict-wiring` + `wiring_complete
  =false` → AIRA process exit non-zero (distinct from the child's exit, clearly labelled).

## 5. Tests (TDD; discriminating)

- **I1** env_digest identical with/without metadata flags.
- **I2** real go-json capture → N results, complete; malformed → parser_complete=false, kept;
  a `CaptureComplete=false` run → parser_complete=false even with a valid prefix (seam-inject a
  truncated capture — a "parse the prefix and claim complete" impl must fail this).
- **I3** report with empty suite_id → recorded but flagged comparison-ineligible (M13 rule).
- **I4** tests_green_observed: exit0+count>0 → observed; exit0+**zero tests** → not(zero-tests) and
  **no gate audit row written** (assert the gate audit is untouched — proves M19 is not a verdict);
  exit≠0 → not.
- **I5** `--tool` no `--usage` → ComputeEvent resource-set, tokens nil; `--usage` present → M14
  normaliser fills tokens, an absent bucket stays nil (not 0).
- **I6** injected AddTestReport/AddComputeEvent error → coded warning, wiring_complete=false, run
  record unchanged, child exit preserved; `--strict-wiring` → AIRA exit non-zero.
- **I7** an M10b command-gate run emits no M19 report/ComputeEvent.
- Faces/golden: the new args in dispatch + skill goldens; MCP==CLI request equivalence.
- **Real-binary e2e** (`~/tmp/aira-m19-e2e.sh`): `aira run --report go-json --ticket AIRA-1
  --phase work -- go test -json ./pkg` → report created + tests_green_observed; a zero-test run →
  report kept, observed=not(zero-tests), gate audit empty; a failing run → not observed; a
  `--tool codex --usage usage.json` run → ComputeEvent tokens authoritative; without `--usage` →
  tokens unevaluated. The config→core→runner→store wiring the Go seam-tests can't see.

## 6. Invariants for the build review (both directions)

1. M19 writes **no gate verdict / no gate-audit row** — a run-observed green is a fact, never a
   §118 proven-to-fire attestation (the load-bearing honesty line).
2. `parser_complete=true` is impossible when the capture was incomplete or the read truncated.
3. Token buckets are `unevaluated` unless an explicit `--usage` provided them; never a fabricated 0.
4. A report missing a required §6 identity is recorded but comparison-ineligible; identities are
   never fabricated.
5. Wiring failures are coded warnings + `wiring_complete=false`, never a run-record mutation;
   `--strict-wiring` is the only path to a non-zero AIRA exit and is clearly distinguished from the
   child's exit.
6. Metadata is orthogonal to launch/capture; the gate lane emits no M19 telemetry.
