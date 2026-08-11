# AIRA M10a — gate honesty engine (implementation plan)

Status: plan (implementation plan; scopes and resolves the M10 design)
Date: 2026-08-11
Base: master `357964e` (M12 runner-lite merged)
Companion design: [`2026-08-11-aira-m10-gates-design.md`](2026-08-11-aira-m10-gates-design.md)
Prerequisites: M9c traceability check; `CheckReport`/stable-code contract; M8a/M8b
descriptor-generated CLI/MCP/Skill faces; M9a durable-allocation + M12 durable-ledger
patterns (reused, not reinvented).

This plan does three things the companion design deliberately left to "the
architect": it **resolves all 21 open decisions**, it **splits M10 into gateable
sub-milestones**, and it gives the **concrete build map** for the first slice
(M10a). The 760-line companion design remains the authority on the content model,
verdict table, invariants, and test matrix; this plan does not restate it, only
scopes and pins it.

## 1. Scope split (owner decision, recorded)

M10 as designed is the whole gate/proof/canary/ratchet system. It is too large
for one build and its ratchet slice is coupled to a `TestReport` archive that
does not yet exist (Phase 4). It is split:

- **M10a — the honesty engine (THIS PLAN).** Gate + canary content model
  (kinds `checkable` and `manual`), the HMAC-authenticated common-dir audit
  layer (results, attestations, proof-of-fire) with DB projection and rebuild,
  the `check-dimension` checker, fixture and attestation-challenge canaries,
  proof-of-fire, the full `aira gate` grouped verb across all faces, and folding
  into `check` and `ready`. No subprocess execution. No ratchet.
- **M10b — command-backed evaluation.** A `command` checker that runs a declared
  argv through the **M12 runner** (scoped cgroup, capture integrity, whole-scope
  kill, a gate-owned timeout composed from `Launch`+timer+`Kill`), the `tests-green`
  predicate (exit 0 ∧ parser-complete ∧ discovered-count > 0 on a single run),
  and the `mutation` canary mode. Reuses the entire M10a audit/verdict engine.
- **Ratchet gates → Phase 4.** The `ratchet` kind, baselines
  (`gate baseline pin/show`), and the test-report comparator require the Phase-4
  `TestReport` archive. Deferred with the recorded deviation in §2 below. The
  full `E_GATE_RATCHET_*`/`U_GATE_BASELINE_*`/`U_GATE_INCOMPARABLE` codes are
  **still registered now** so the stable contract does not churn later.

## 2. Accepted deviations from the companion design (M10a)

These extend the design's §0.1. Any build that departs from them must record an
equivalent subsection naming the invariant and test affected.

1. **`kind` enum is `{checkable, manual}` in M10a.** `ratchet` is not a decodable
   kind until the Phase-4 ratchet milestone; a gate file declaring `ratchet` is
   `E_GATE_KIND_INVALID` in M10a (an honest "not yet supported", never a silent
   pass). The enum is designed to extend by adding the value; the versioned
   decoder makes that a normal schema evolution. Affected invariant: design §8.1
   (three verdicts) is unaffected — a rejected definition never yields a pass.
2. **No subprocess execution in M10a.** The only checker is `check-dimension`,
   which folds an existing deterministic `aira check` dimension. Command-backed
   evaluation is M10b. Affected invariant: design §8.7 (non-empty evidence) is
   satisfied by the dimension's own findings, not by a parsed process output.
3. **Ratchet codes registered but unreachable.** `E_GATE_RATCHET_REGRESSED`,
   `U_GATE_BASELINE_MISSING`, `U_GATE_INCOMPARABLE`, `E_GATE_BASELINE_INVALID`
   are in the catalog for contract stability but no M10a code path emits them.
   A test asserts they are registered; none asserts they are produced.
4. **Canary modes in M10a are `fixture` and `attestation-challenge` only.**
   `mutation` (M10b) and `synthetic-report` (Phase 4) are rejected at canary
   load with `E_GATE_CANARY_INVALID`. Affected test: design §9.4 #17–23 run
   against the fixture path; the mutation/synthetic rows are M10b/Phase-4.

## 3. Resolution of the 21 open decisions

Pinned answers. Where the design offered a menu, the chosen option is stated and
the rest are recorded as deferrals.

1. **M10 vs runner-lite.** Resolved earlier: runner-first. M12 exists, so
   command-backed checks are *possible*; M10a still ships **no** execution (the
   `check-dimension` checker is pure). Execution lands in **M10b** on the M12
   runner lane.
2. **Initial checker catalog.** M10a: **`check-dimension`** only (wraps one of
   the existing `aira check` dimensions — `traceability`, `relations`,
   `allocation`, etc. — as a gate predicate). M10b adds **`command`** and
   **`tests-green`**. Coverage/report predicates deferred to Phase 4.
3. **Command-backed gates.** Allowed, but **M10b only**, and **only through the
   M12 runner lane** (declared argv + cwd policy + explicit env allow-list with
   recorded env-digest + byte-capped capture + gate-owned timeout via
   `Launch`+timer+`Kill`). Every command gate still requires proof-of-fire via a
   canary. No raw `exec` outside the runner.
4. **`aira check` execution policy.** **Read-only, no `--run`.** `check` folds the
   latest durable gate result and validates gate files; it never launches an
   evaluator, mutates the tree, pins a baseline, or creates proof. `gate run` is
   the only evaluate-and-record verb. (design §5.3, invariant 10, risk row.)
5. **Canary representation.** M10a real modes: **`fixture`** (a seeded known-bad
   tree the gate must fail on) and **`attestation-challenge`** (manual gates).
   `mutation` → M10b. `synthetic-report` → Phase 4. A synthetic report is
   **never** acceptable for a production gate (it cannot prove the real evaluator
   ran). Fixture maintenance is the gate author's; fixtures are committed content.
6. **Canary cadence.** Default **`every-evaluation`** (the strongest honesty:
   proof is refreshed on every `gate run`, so it cannot go stale). **`on-demand`**
   allowed with a max-age proof. **No `periodic`** in M10 (needs the Phase-5
   daemon scheduler). Max age of a one-time proof: **7 days** default,
   per-gate-configurable.
7. **Proof expiry.** Default `max_age` = **7 days**. A current `every-evaluation`
   canary **replaces** proof each evaluation (staleness impossible while the
   canary keeps firing). An `on-demand` proof decays to `U_GATE_PROOF_STALE`
   past `max_age`. A failed canary can never refresh proof.
8. **Manual authenticity.** **AIRA-issued HMAC-SHA-256** record bound to
   actor/session/subject/lane/challenge-nonce is sufficient for M10's local
   threat model. No external/device/TTY signature in M10. Generated guidance
   states the limit verbatim: **AIRA authenticates that it issued the record and
   binds it to an actor/session; it does not prove biological-human presence**;
   for MCP, "human-attested" means bound to the MCP session actor, nothing more.
9. **Baseline storage.** (Ratchet, Phase 4.) Resolved for when it lands:
   **common-dir content-addressed snapshot is authoritative**; no git notes, no
   separate store. Project+lane scoped in the shared common-dir (correct across
   worktrees of one repo). Cross-machine out of scope.
10. **Baseline promotion.** (Ratchet, Phase 4.) `gate baseline pin` requires
    **actor + reason**; may require a manual attestation per
    `proof_policy.baseline_requires_attestation`. Rollback = re-pin an older
    evidence set (new immutable record, history preserved).
11. **Ratchet metrics.** **Deferred to Phase 4** with the `TestReport` archive.
    M10a/M10b ship no ratchet comparator. (Biggest de-risking decision.)
12. **Flaky tests.** **Deferred** (Phase-4 archive/classifier). Contradictory
    observations are `U_GATE_INCOMPARABLE`, never silently excluded.
13. **Gate identifiers.** **Project-local author-chosen slug names**, filename ==
    `id`, **not** the AIRA allocator. Rationale: a gate id is a *name* (like a
    config key — "go-test", "traceability"), authored policy content, low-volume;
    the allocator is for high-volume machine-generated entities. This does not
    violate the "never hand-pick allocator IDs" rule — a gate name is not an
    allocated ID. Validation: `^[a-z0-9][a-z0-9-]{0,63}$`, unique, filename match.
    Revisit if cross-project gate references are ever required.
14. **Gate policy location.** **In each gate file** (`applies_to` selector), never
    a shared `.aira/config` registry — so two agents editing different gates never
    conflict (design §4.1 invariant).
15. **Gate composition/dependencies.** **None in M10.** One gate per evaluation
    plus its declared canary. No boolean composition, no gate-to-gate deps, no
    gate-as-`blocked-by`. Deferred (design §non-goals).
16. **Ready semantics.** An applicable **unevaluated** gate makes `ready=false`
    (honest default). A gate may set `advisory_in_ready: true` to appear in
    `check` but be ignored by `ready`. Default: counts in `ready`.
17. **Phase-3 enforcement.** **Never** a write/merge block in Phase 3 (invariant
    11). Enforcement is a future (Phase 5+) per-project policy with waiver
    attestations, out of M10.
18. **Review emission boundary.** **M10a owns `gate review`** — it emits the
    structured challenge/review-request record (gate purpose, subject, lane,
    required evidence, failure guidance, challenge id). It does **not** route or
    judge; M11's `review` verb routes. Rendering is a face concern.
19. **Evidence retention.** Proof-of-fire evidence is **pinned** (retention-exempt
    while it backs a valid proof). Ordinary-pass evidence is pinned only while it
    is the latest-by-subject projection; an evicted older result re-inspected is
    `U_GATE_EVIDENCE_UNAVAILABLE`, never a silent pass. M10a `check-dimension`
    evidence is the small structured finding set (no raw output); byte-caps apply
    in M10b.
20. **Remote boundary.** Attestations/baselines are **machine-local receipts**
    (common-dir). No cross-machine promise.
21. **Migration/versioning.** A project with no `.aira/gates/` has **no gates**:
    the `gates` check dimension reports "0 gates defined" — an honest empty set,
    **not** an implicit pass of anything. The HMAC audit key is generated on the
    first gate write (0600, common-dir, like the M9a/M12 durable keys). No seeded
    gates, no implicit migration, no pre-existing `tests-green` to import
    (greenfield).

## 4. M10a build map

Downward-layered, mirroring where M9/M12 put their pieces. New package
`internal/gate/` holds the pure content/verdict domain; the durable audit layer
lives in `internal/store/` beside the existing ledger/receipt machinery it reuses;
faces are descriptor entries consumed by `core.Do`.

### 4.1 `internal/gate/` — pure domain (no I/O)

- `gate.go` — `GateDefinition` (closed enums, tagged kind payload
  `Checkable`/`Manual`, `AppliesTo` selector, `Lane`, `ProofPolicy`, `CanaryIDs`,
  `Enabled`/`Advisory`/`AdvisoryInReady`), constructors that **reject illegal
  states** (two payloads, no payload, unknown kind incl. `ratchet` in M10a,
  bad selector, unknown checker id, filename≠id, canary ref to another gate).
  `RenderGate`/`ParseGate` = JSON-in-`---` frontmatter, canonical field order,
  `schema_version` with a versioned decoder (unknown required version → invalid).
- `canary.go` — `CanaryDeclaration` (`Mode` ∈ {fixture, attestation-challenge}
  in M10a, `Seed` typed ref, `ExpectedGateResult`, `LaneBinding`, `Isolation`,
  `Cadence` ∈ {on-demand, every-evaluation}, `Description`), constructor
  validation, round-trip.
- `verdict.go` — the §6.1 per-gate verdict fold: `(predicate, proofState,
  canaryHealth, evidenceAvail) → (verdict ∈ pass|fail|unevaluated, code, trusted,
  suspect)`. Pure function, table-tested. `trusted` derived, never caller-set;
  `pass` impossible without valid proof.
- `digest.go` — canonical record digests + the HMAC canonical-payload builder
  (field ordering only; the key/HMAC call lives in the store writer). Reuse the
  M9a/M12 digest conventions (sorted `\x00`-joined fields, sha256).

### 4.2 `internal/store/` — durable audit + projection (reuses M9a/M12)

- `gate_audit.go` — the common-dir append-only audit writer for **result**,
  **attestation**, and **proof-of-fire** records under
  `$(git-common-dir)/aira/gates/`. **Reuse the M12 framed+checksummed+fsynced
  ledger pattern and the M9a per-record CAS/reconcile**; add HMAC-SHA-256 over the
  canonical payload with a machine-local 0600 project key, a previous-record digest
  chain, and a one-time nonce. Rebuild reproduces the authenticated record or fails
  `E_JOURNAL_CORRUPT`; a tampered tag / duplicate nonce / reordered record /
  changed subject is rejected. SQLite has **no authority to mint** a record.
- `gate_index.go` — rebuildable DB projection: `gates` (definitions discovered by
  scanning `.aira/gates/*.json` on a coherent `git ls-files` snapshot, mirroring
  the M9c requirement scan), `gate_results` latest-by-subject, `gate_proofs`,
  `gate_attestations`. Disposable — DELETE-per-project + reinsert on Rebuild.
  Duplicate gate id → fail closed.
- `gate_eval.go` — the `check-dimension` checker + evaluation protocol (design §5):
  load+validate definition/canary → resolve subject commit/tree + lane digest →
  evaluate the named `aira check` dimension as the predicate → run the fixture
  canary in an **isolated boundary** (materialise the seed tree under a temp dir,
  run the same dimension, assert the expected `fail`; caller worktree byte-for-byte
  unchanged) → if the canary fired, append proof-of-fire → fold → append result +
  update index. Manual gates: no valid attestation → `unevaluated` + emit review
  request (`gate review`), `attest` consumes an AIRA challenge and appends an
  authenticated attestation.
- `check.go` — register the full `E_GATE_*`/`U_GATE_*`/`W_GATE_*` catalog (design
  §6.2, incl. the Phase-4 ratchet codes per deviation §2.3); add the `gates`
  dimension to `CheckReport` (fail→Findings, warn→Warnings, unevaluated→
  UnevaluatedFindings+Unevaluated bit; precedence fail>unevaluated>pass preserved).
  `ready` folding (design §6.4): fail/unevaluated → `ready=false` (respecting
  `advisory_in_ready`), warnings shown but not unready, "N failed, M unevaluated,
  K passed" summary. **`check` stays read-only** — a test asserts no evaluator
  runs and no write happens.

### 4.3 faces — descriptor entries (design §7)

- `internal/core/core.go` — `gate` grouped verb (`Operations`: add, ls, show, set,
  run, check, attest, prove, review, canary-run, canary-show) with per-op
  `Summary`/`Safety`/`Include`/`Example` (mirror the `find`/`req` M8 pattern), the
  handlers dispatching to the store audit/eval layer, and the response contract
  (per-gate verdict, overall verdict, evidence/proof detail, exit mapping).
- MCP: one grouped `aira_gate` tool with an operation discriminator (mirror
  `aira_requirement`). Skill/guide: the same operation set from descriptors.
- Tests per the M8b lesson: descriptor drift (handler-read args == declared),
  CLI↔core↔MCP parity for every op incl. nested canary ops, full-coverage E2E that
  every documented example reaches core (not an arg-construction error), guide/skill
  action set == descriptor ops. **Verify every generated artifact, not a sample.**

### 4.4 tests (design §9, M10a subset)

All of §9.1–9.3 (domain/content, audit/auth/recovery, verdict/honesty matrix),
§9.4 #17–23 on the **fixture** path (+ the attestation-challenge row for manual),
§9.6 (integration/ready/faces). §9.5 (ratchet) is Phase 4. Every confirmed
counterexample from the adversarial gate becomes a regression test. The
correctness-critical set — **HMAC chain integrity, nonce/replay, rebuild
determinism, canary isolation, proof freshness, and the read-only `check`
guarantee** — gets the full adversarial two-loop.

## 5. Process

design (this plan) → **Sol plan-review** (the delicate parts: HMAC record chain +
nonce/replay, canary isolation, proof-freshness fold, the read-only-`check`
guarantee, the ratchet-deferral seams) → fix → **delegated build** (Codex,
self-contained multi-stage TDD brief; likely split `internal/gate/` +
`internal/store/gate_*` as one thread and faces as a second concurrent thread on
disjoint files) → **Sol build-review + my independent real-binary e2e** (incl. the
common-dir audit + rebuild + canary-isolation paths that only run in my shell) →
gate → `git -C ~/aira merge --ff-only`.

## 6. Expected yield

M10a makes "this check is green" a claim with an authenticated, replayable,
proof-of-fire-backed audit record that `check` and `ready` consume honestly,
across all three faces — without any subprocess, ratchet, or `TestReport`
dependency. It is the honesty engine; M10b bolts real command execution onto it
via the runner, and Phase 4 adds ratchets on the `TestReport` archive.
