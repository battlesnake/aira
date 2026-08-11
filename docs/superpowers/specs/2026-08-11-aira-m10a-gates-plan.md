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
   pass). Adding `ratchet` in Phase 4 **requires a `schema_version` bump**, not a
   bare enum widening — catalog registration of the ratchet codes (deviation §2.3)
   must **not** be read as forward-compatibility of the enum, and the versioned
   decoder must gate the new value on the new schema version. Affected invariant:
   design §8.1 (three verdicts) is unaffected — a rejected definition never yields
   a pass. (Sol P1-6.)
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
7. **Proof expiry and proof scope.** Default `max_age` = **7 days**. Proof is
   scoped by `(gate-definition digest, evaluator version, lane, config digest,
   canary ID, **canary-declaration/seed digest**, **canary-tree digest**)` — the
   seed/declaration digest is included so that **editing the committed canary seed
   under the same ID immediately invalidates prior proof** (without it an on-demand
   proof would wrongly survive a weakened canary for 7 days). For an
   `every-evaluation` gate, `gate run` **requires a current canary-health = pass in
   the same run**; a canary `fail`/`unevaluated` result makes the gate not-pass in
   that evaluation regardless of any prior proof (a prior proof cannot rescue a
   currently-broken guard). An `on-demand` proof decays to `U_GATE_PROOF_STALE`
   past `max_age`. A failed canary can never refresh or preserve proof. (Sol P1-3.)
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
  states**: two payloads, no payload, unknown kind incl. `ratchet` in M10a, bad
  selector, unknown checker id, filename≠id, canary ref to another gate, **an
  empty `canary_ids` set** (a trustworthy gate must declare a known-bad proof
  path — design §4.1), and a **`check-dimension` checker whose target dimension is
  `gates` itself or the aggregate `check`** (would recurse / double-count — reject
  at load; the checker targets exactly one non-gate dimension — Sol P1-4).
  `RenderGate`/`ParseGate` = JSON-in-`---` frontmatter, canonical field order,
  `schema_version` with a versioned decoder (unknown required version → invalid).
- `canary.go` — `CanaryDeclaration` (`Mode` ∈ {fixture, attestation-challenge}
  in M10a, `Seed` typed ref, `ExpectedGateResult`, `LaneBinding`, `Isolation`,
  `Cadence` ∈ {on-demand, every-evaluation}, `Description`), constructor
  validation, round-trip. **A proof-eligible canary (any mode that produces
  proof-of-fire) whose `ExpectedGateResult` is not `fail` is `E_GATE_CANARY_INVALID`**
  — proof requires a known-bad input that is *expected to fail* the gate (Sol P1-6).
  A `canary_declaration_digest` over the canonical declaration (incl. the resolved
  seed content digest) is part of the proof scope (§3 decision 7).
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
  canonical payload with a machine-local 0600 project key and a one-time nonce.
  **The ledger is one totally-ordered authenticated chain (Sol P0-1):** a genesis
  record, a strictly-monotonic authenticated `seq` on every record, each record
  binding the previous record digest, and a **durable, authenticated head/commit
  marker** written+fsynced after each append. Verification checks the chain from
  genesis to the durable head; **a valid-suffix truncation (records removed from
  the tail) is detected because the head no longer matches the last chained record**
  — it is `E_JOURNAL_CORRUPT`, never a silent reversion to an older record. The
  projection derives "latest" strictly by **authenticated `seq` order, never by
  timestamp** (a removed later `fail`/canary-non-fire cannot resurrect an earlier
  `pass`). Rebuild reproduces the authenticated records or fails `E_JOURNAL_CORRUPT`;
  a tampered tag / duplicate nonce / reordered record / changed subject / truncated
  suffix is rejected. SQLite has **no authority to mint** a record.
- `gate_index.go` — rebuildable DB projection: `gates` (definitions discovered by
  scanning `.aira/gates/*.json` on a coherent `git ls-files` snapshot, mirroring
  the M9c requirement scan), `gate_results` **latest-by-subject selected by
  authenticated `seq`** (not timestamp), `gate_proofs`, `gate_attestations`.
  Disposable — DELETE-per-project + reinsert on Rebuild. Duplicate gate id → fail
  closed.
- `gate_eval.go` — the `check-dimension` checker + evaluation protocol (design §5).
  **The checker takes an explicit immutable `EvaluationRoot` (a resolved tree/
  snapshot) and MUST read only from it** — it never reaches back to the caller
  worktree or the common-dir for content (Sol P0-2). Protocol: load+validate
  definition/canary → resolve subject commit/tree + lane digest → evaluate the
  named single non-gate `check` dimension against the **subject** `EvaluationRoot`
  as the predicate → run the fixture canary by **materialising the seed tree in an
  isolated temp boundary and evaluating the SAME dimension against the FIXTURE
  `EvaluationRoot`** (not the caller root), asserting the expected `fail`. The
  proof-of-fire is bound to the **canary-tree digest** and a **target-subject scope
  distinct from the fixture scope**, so a fixture proof can never be mis-bound to
  the real subject. Isolation hardening: reject symlink / `.git` / parent (`..`)
  escapes out of the fixture root; the caller worktree is byte-for-byte unchanged.
  A **canary non-fire is `E_GATE_CANARY_DID_NOT_FIRE`, gate fail, fail-closed** —
  never a warning or unevaluated; a canary that cannot establish a result is
  `U_GATE_CANARY_UNEVALUATED`. If the canary fired, append proof-of-fire → fold →
  append result + update index. Manual gates: no valid attestation → `unevaluated`
  + emit review request (`gate review`); `attest` consumes an AIRA challenge and
  appends an authenticated attestation. **A distinct-root sentinel test** (fixture
  and caller trees each carry a different marker) proves the evaluator actually
  consumed the fixture, not the caller repo.
- `check.go` — register the full `E_GATE_*`/`U_GATE_*`/`W_GATE_*` catalog (design
  §6.2, incl. the Phase-4 ratchet codes per deviation §2.3); add the `gates`
  dimension to `CheckReport` (fail→Findings, warn→Warnings, unevaluated→
  UnevaluatedFindings+Unevaluated bit; precedence fail>unevaluated>pass preserved).
  `ready` folding (design §6.4): fail/unevaluated → `ready=false` (respecting
  `advisory_in_ready`), warnings shown but not unready, "N failed, M unevaluated,
  K passed" summary. **`check` is STRICTLY read-only (Sol P1-5):** it folds the
  latest durable gate result via a read-only verify-and-project path and **never
  runs an evaluator, acquires a write lock, rebuilds SQLite, creates the HMAC key,
  or heals in place**. A missing/stale projection is `U_GATE_NO_RESULT`; a
  corrupt/unverifiable audit chain is an integrity `unevaluated`/`E_JOURNAL_CORRUPT`
  finding — never a silent repair and never a pass. Tests assert `check` performs
  no evaluator subprocess, no write, and no key creation.

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

**Mandatory regression tests from the Sol plan-review (each finding → a test):**

- **T-trunc (P0-1):** append `pass` then `fail`/canary-non-fire; delete the tail
  record(s) from the ledger; rebuild/verify must report `E_JOURNAL_CORRUPT` (head
  ≠ last chained record), NOT surface the earlier `pass`. Also: latest-by-`seq`
  never picks a lower-`seq` record even if it has a later timestamp.
- **T-fixture-sentinel (P0-2):** fixture tree and caller worktree carry different
  sentinels; assert the canary evaluation consumed the FIXTURE sentinel; assert
  the caller worktree is byte-for-byte unchanged; assert `.git`/symlink/`..`
  escape out of the fixture root is refused; assert proof is bound to the
  canary-tree digest and cannot satisfy the real subject scope.
- **T-seed-invalidates (P1-3):** an on-demand proof is invalidated the moment the
  committed canary seed/declaration digest changes under the same canary ID; an
  `every-evaluation` gate whose current canary `fail`s/`unevaluated`s does not pass
  on a prior proof.
- **T-no-recursion (P1-4):** a gate whose `check-dimension` checker targets `gates`
  or the aggregate `check` is rejected at load; a valid checker's findings are not
  double-counted in the `gates` dimension.
- **T-check-readonly (P1-5):** `aira check` on a project with gates performs no
  evaluator run, no DB write, no rebuild, and no HMAC-key creation (assert the key
  file does not appear); a missing projection yields `U_GATE_NO_RESULT`, a corrupt
  chain yields an integrity finding — never a heal or a pass.
- **T-constructor (P1-6):** empty `canary_ids` rejected; a proof-eligible canary
  with `expected_gate_result != fail` rejected; the ratchet codes are registered
  but no M10a path emits them; a `ratchet` kind is `E_GATE_KIND_INVALID` and adding
  it later is gated on a `schema_version` bump.

## 5. Process

**Sol plan-review status: DONE — verdict BLOCK, 6 findings (2 P0, 4 P1), ALL
incorporated above** (thread `019ff196-ed53-7812-95fe-5dd8a9f80bce`): P0-1 ledger
suffix-truncation → total-ordered authenticated-`seq` chain + durable head (§4.2);
P0-2 canary must provably consume the fixture root → explicit `EvaluationRoot` +
canary-tree-bound proof + escape guard + sentinel test (§4.2); P1-3 proof binds
seed/declaration digest + every-evaluation requires current canary health (§3.7);
P1-4 checker rejects `gates`/aggregate targets (§4.1); P1-5 `check` strictly
read-only, no key/rebuild/heal (§4.2); P1-6 reject empty `canary_ids` + non-`fail`
proof canary + ratchet needs schema bump (§2.1, §4.1). Each has a mandatory
regression test in §4.4.

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
