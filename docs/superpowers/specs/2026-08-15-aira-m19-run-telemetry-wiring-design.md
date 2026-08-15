# AIRA M19 — run → telemetry/gate auto-wiring

Status: PLAN (v1, for Sol adversarial plan-review)
Date: 2026-08-15
Milestone: Phase 5 · M19 — telemetry/gate auto-wiring
Depends on: M12 runner, M16 rusage, M13 test-reports, M14 compute telemetry, M10b command gate
(all landed, master `bd965be`)
Design authority: 2026-08-07-aira-design.md §14 (Launch/Wiring lines 146,155), §12 compute,
§13 test-reports, §9 gate honesty (line 120: `tests-green` ⇐ exit==0 **and** parsed report
test-count>0).

## 0. The gap

M12–M18b give `aira run` a launch/capture/rusage record, but an agent still has to run three
*separate* commands to make a run count: `test-report add`, `spend add` (ComputeEvent), and a
gate attestation. §14 line 155 defines the wiring: a `run --phase … --tool …` auto-emits a
ComputeEvent; a `run --report <fmt>` creates a §13 report and attests `tests-green` **only** per
the exit0-and-count>0 rule. M19 wires the existing engines onto `aira run` — it invents no new
telemetry, only connects what M13/M14/M10b already own, honestly.

## 1. Scope

**In:**
- **Run provenance metadata**: `Request.{Ticket,Phase,Label,Tool}` + `RunRecord.{Ticket,Phase,
  Label,Tool}`; faces `--ticket/--phase/--label/--tool` on `aira run` (CLI + MCP). Recorded on
  the first ledger event, carried in `mergeEvidence` (the M16/M18a field-loss trap). Pure
  metadata — never changes launch/capture behaviour.
- **`--report <fmt>` auto-ingest**: after the run terminates, read the captured output and ingest
  it as a §13 TestReport (reuse the M13 parser + `AddTestReport`), stamping the run's provenance:
  `RunRef=<run id>`, `TicketID/Phase` from the flags, `Commit/Branch/WorktreeID` from the store's
  git context, `Format=<fmt>`, `EnvDigest` from the run. Honesty: `ParserComplete` reflects the
  **real** parse (a malformed/partial capture → `parser_complete=false`, report still recorded,
  excluded from flaky/ratchet — never faked); a run whose capture is **incomplete**
  (`CaptureComplete=false`) yields `parser_complete=false`. Report-ingest failure is surfaced as a
  **warning on the run response**, never silently dropped and **never fails the run** (the run
  already happened — recording is best-effort telemetry).
- **`--tool <t>` ComputeEvent auto-emit**: a run with `--tool` emits a §12 ComputeEvent (reuse
  `AddComputeEvent`) keyed by `(Ticket, Phase, Model=<tool>)`, capturing the run's **resource**
  usage — wall (`ended-started`), `cpu_user`, `cpu_sys`, `peak_rss` (from M16, `*int64`, nil ⇒
  unevaluated). **Token buckets are `unevaluated`** in v1: parsing a tool's `--json` token usage
  is provider-specific and deferred (D1) — recording fabricated token counts would violate the
  M14 honesty contract, so the honest default is resource-usage-with-tokens-unevaluated. Emit
  failure → warning, never fails the run.
- **`tests-green` attestation** (the gate half): a `run --report --attest <gate-id>` records a
  `tests-green` attestation **only** when `exit==0` **AND** the parsed report has
  `test-count>0` (§9 line 120). Otherwise it does **not** attest and records the honest reason
  (`U_GATE_TESTS_GREEN_UNPROVEN`: exit≠0, zero tests, or an incomplete/failed parse). The
  attestation is **evidence-linked to the run + report** (§9 line 118 — a green gate must have
  actually fired and the proof must be evidence-linked), so it is not a bare manual verdict
  (§3.4 details the linkage decision).

**Out (deferred, written down):**
- **D1 provider token-usage parsing** — a per-tool `--json` usage parser feeding the ComputeEvent
  token buckets; v1 records resource usage only, tokens `unevaluated`.
- **D2 auto gate creation** — `--attest` names an **existing** gate; M19 never mints a gate.
- **D3 detach / supervisor** — the `--detach` shim (§14 line 153) is its own milestone; M19 wires
  the foreground run only.
- **D4 `run-input`** — post-shim.

## 2. Invariants (each → a discriminating test)

- **I1 — metadata is orthogonal.** `--ticket/--phase/--label/--tool` change only recorded fields;
  a run with and without them has the **same** `env_digest`, capture, and exit behaviour.
- **I2 — the report reflects the real parse, never faked.** A `--report go-json` run of a suite
  that emits N results records a report with those N results and `parser_complete=true`; a
  malformed/truncated capture records `parser_complete=false` (report kept, flaky/ratchet
  exclude it); an **incomplete capture** (`CaptureComplete=false`) → `parser_complete=false`.
- **I3 — `tests-green` obeys §120 exactly.** `--report --attest G`: `exit==0` + report
  test-count>0 → a `tests-green` attestation linked to the run+report; `exit==0` + **zero** tests
  (e.g. `go test` with no test files) → **NOT** attested (`U_GATE_TESTS_GREEN_UNPROVEN`); `exit≠0`
  → NOT attested. A bare exit code never forges the attestation.
- **I4 — wiring failures never corrupt the run.** A report-ingest / ComputeEvent / attest failure
  is a **warning** on the run response; the run's own record (status/exit/capture) is unchanged
  and the run is not re-run or failed. Telemetry is best-effort; the launch record is authority.
- **I5 — ComputeEvent tokens are honestly unevaluated.** A `--tool` run's ComputeEvent has real
  resource usage (or nil→unevaluated where M16 couldn't read it) and token buckets `unevaluated`
  — never a fabricated 0 or a guessed count.
- **I6 — no gate-lane recursion.** The auto-wiring runs in the `aira run` **face handler** after
  `Launch` returns; the M10b command-gate lane (which itself calls the runner) constructs its
  `runner.Request` without M19 flags, so a gate's own command run does not recursively auto-emit
  telemetry.

## 3. Design

### 3.1 Where the wiring runs

The `run` **core handler** (`internal/core/core.go`), after `c.runner.Launch(ctx, req)` returns a
terminal `RunRecord`, performs the wiring against `c.store`: read capture (via the runner's
`ReadOutput`), ingest report, emit ComputeEvent, attest. The handler already has both `c.runner`
and `c.store`. The runner is **not** modified to do telemetry (it stays a pure recorder, §14
line 146); the metadata fields are the only runner change. The wiring is skipped when the
relevant flag is absent, and entirely for the gate lane (I6).

### 3.2 `--report` ingest

1. Only if `--report` set. Read the captured bytes: the merged/`out` stream via
   `runner.ReadOutput` (bounded by the output cap — a report larger than the cap → parser
   handles truncation → `parser_complete=false`, honest).
2. Build `domain.TestReportInput{Format, Raw, TicketID, Phase, RunRef=<id>, Commit, Branch,
   WorktreeID, EnvDigest, At, …}` — provenance from the store's git context + the run.
3. `s.AddTestReport(ctx, input)` (the M13 parser fills `Results`/`ParserComplete`). Return the
   report id + parser-complete on the run response; a non-nil error → a `warning` (I4).

### 3.3 `--tool` ComputeEvent

Only if `--tool` set. `domain.ComputeEventInput{TicketID, Phase, Model=<tool>, At, Source="run",
Raw: <resource usage: wall/cpu_user/cpu_sys/peak_rss; token fields unset ⇒ unevaluated>}`.
`s.AddComputeEvent`. Failure → warning (I4). (RawUsage's token buckets are `*int64`; leaving them
nil is the M14 "unevaluated, not zero" contract — §1 D1.)

### 3.4 `tests-green` attestation — the evidence-linkage decision (Sol, please scrutinise)

The current `AttestGate(id, verdict, actor)` is a **manual** verdict with no evidence linkage,
which §9 line 118 forbids for a green *checkable* gate. Two candidate mechanisms:

- **(A) proposed:** extend the attestation path with an **evidence ref** (run id + report id +
  the exit0/count>0 facts), so M19 records a `tests-green` attestation that is dated and
  evidence-linked, decaying per the existing attestation max-age. `--attest G` on a `--report`
  run calls this only when `exit==0 && report.TestCount>0`; else records
  `U_GATE_TESTS_GREEN_UNPROVEN`. This keeps M19 honest (§120) and reuses the audit-durability
  M10a already has, adding only the evidence ref.
- **(B) alternative:** M19 does **not** attest; it only produces the report (the durable
  evidence), and a separate `gate` verb consumes it. This is smaller but leaves the "attest"
  half of §155 to a follow-up.

v1 proposes **(A)** with the minimal evidence-ref extension; if Sol judges the linkage too large
for this cut, we fall back to **(B)** and file the attest as M19b. Either way the §120 rule
(exit0 + count>0) is the hard gate on any `tests-green` signal.

## 4. Faces + response

- CLI `aira run`: `--ticket <id> --phase <p> --label <l> --tool <t> --report <fmt> --attest <gate>`.
- MCP `aira_run`: same as string/bool args.
- The run response gains a `wiring` block: `{report_id?, report_parser_complete?, compute_event_id?,
  tests_green?(attested|unproven|not_requested with reason), warnings:[…]}` — every auto-action's
  outcome is visible and honest (a skipped/failed action says why).
- `--report`/`--attest`/`--tool` on a run through the `--json`/MCP face behave identically (the
  wiring is in the core handler, not a text-face concern).

## 5. Tests (TDD; discriminating)

- **I1** metadata orthogonality: env_digest identical with/without `--ticket/--phase/--label/--tool`.
- **I2** report ingest: real go-json/junit capture → N results, parser_complete=true; a truncated
  capture → parser_complete=false (report still recorded); an incomplete-capture run → false.
- **I3** tests-green honesty (the load-bearing discriminators): exit0+count>0 → attested+linked;
  exit0+**zero tests** → `U_GATE_TESTS_GREEN_UNPROVEN`, NOT attested (a bare-exit0 impl fails this);
  exit≠0 → not attested.
- **I4** failure isolation: an injected AddTestReport/AddComputeEvent/AttestGate error → warning on
  the response, run record unchanged, exit code preserved.
- **I5** ComputeEvent tokens unevaluated: `--tool` run → ComputeEvent with resource usage set,
  token buckets nil/unevaluated (never 0).
- **I6** no gate-lane recursion: an M10b command-gate run does not emit an M19 report/ComputeEvent.
- Faces/golden: `--ticket/--phase/--label/--tool/--report/--attest` in the dispatch + skill
  goldens; MCP==CLI request equivalence for the new args.
- **Real-binary e2e** (`~/tmp/aira-m19-e2e.sh`, committed): `aira run --report go-json --attest G
  -- go test -json ./somepkg` → a report is created, tests-green attested when green+count>0; a
  zero-test run → report recorded but tests-green UNPROVEN; a failing run → not attested; a
  `--tool codex` run → a ComputeEvent with resource usage + unevaluated tokens. The load-bearing
  check the Go seam-tests can't: config→core→runner→store wiring end-to-end through the CLI.

## 6. Invariants for the build review (both directions)

1. `tests-green` is attested **iff** exit==0 AND parsed report test-count>0 (§120) — never from a
   bare exit code, never on an incomplete/failed parse.
2. Telemetry wiring is best-effort: a wiring failure is a warning, never a run failure or a record
   mutation (the launch record is authority).
3. Every auto-action's outcome (done / skipped-why / failed-why) is visible in the `wiring` block —
   no silent success or silent drop.
4. ComputeEvent token buckets are `unevaluated`, never a fabricated 0 (M14 contract).
5. Metadata is orthogonal to launch/capture (`env_digest` unchanged).
6. The gate lane does not recursively auto-emit telemetry.
