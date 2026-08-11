# AIRA M10 — gates, proof-of-fire, and canaries

Status: plan (design only; no implementation in M10 spec)
Date: 2026-08-11
Prerequisites: M9c requirements + `covers`/`verifies` traceability check; the
existing `CheckReport` / stable-code contract; M8a/M8b descriptor-generated
CLI, MCP, and Skill faces.

This is the M10 milestone design for the gates slice of Phase 3. It is a
companion to the whole-product design in
[`2026-08-07-aira-design.md`](2026-08-07-aira-design.md), not a replacement
for it. It specifies the content model, evaluation protocol, durability
boundary, verdict contract, faces, and tests. It does not implement Go code.

## 0.1 Accepted deviations, recorded on the record

The following boundaries are intentional M10 decisions, not omissions:

- Gate policy is advisory in Phase 3. A failed or unevaluated gate makes a
  ticket report “not ready”, but no gate refuses a ticket write, status
  transition, lease, or commit. Structural store-integrity errors retain their
  existing fail-closed behaviour.
- `aira check` does not launch arbitrary checks or mutate the worktree. It
  validates gate definitions and folds the latest applicable durable gate
  evidence into the check report. Evaluation and canary execution are
  explicit `aira gate` operations. This preserves `check` as a repeatable
  consistency pass and prevents a hidden subprocess from creating a green
  result.
- Gate definitions and canary declarations are committed content. Results,
  attestations, proof-of-fire records, and active ratchet-baseline pins are
  machine-shared audit records outside the commit graph, with SQLite holding
  only rebuildable projections and query indexes.
- M10 defines an evaluator and runner seam, but does not claim the full
  Phase-5 subprocess runner, detached execution, live stdin, or model-based
  review. The exact amount of runner-lite and canary execution that lands in
  M10 remains listed in “Open decisions for the architect”.

Any implementation that departs from these four boundaries must add an
equivalent accepted-deviation subsection to the implementation plan and name
the affected invariant and test.

## 1. Scope

M10 makes a gate a durable, inspectable policy object rather than an informal
sentence in a review. It provides four connected capabilities:

1. **Gate definitions** for checkable, manual, and ratchet gates, with
   kind-specific configuration that cannot describe an impossible gate.
2. **Evaluation records** with exactly `pass`, `fail`, or `unevaluated`, plus
   evidence, subject commit, lane/config identity, and stable findings.
3. **Proof-of-fire**: a green gate is not trusted until the same gate has been
   demonstrated to fail on a known-bad input. Proof is dated, evidence-linked,
   lane-bound, and expires according to policy.
4. **Canaries**: declared known-bad inputs that are run through the same gate
   lane. A canary that does not cause the gate to fail is itself a fail-closed
   gate failure.

The milestone integrates gates into `aira check`, `aira ready`, the existing
stable response/exit contract, and the descriptor-generated CLI/MCP/Skill
faces. A manual gate also produces a structured review/attestation request;
the full review-routing feature remains a later face.

### 1.1 M10 success condition

For every applicable gate at a subject commit, an agent or human can answer:

- what the gate is supposed to establish;
- which evaluator and lane ran;
- what exact evidence was considered;
- whether the result is pass, fail, or unevaluated;
- whether the gate has ever fired on known-bad input and whether that proof is
  still valid; and
- why `ready` or `check` included or excluded the gate from its verdict.

No result is inferred from a missing row, an empty test set, an absent
baseline, an evicted report, or a command that was never observed to run.

## 2. Non-goals / explicit deferrals

- No merge blocking, branch protection, or mandatory lifecycle enforcement in
  Phase 3. A project may consume AIRA's stable gate codes in its own harness,
  but AIRA remains advisory-first.
- No model, reviewer, or semantic judgment inside AIRA. A manual gate emits a
  prompt and records a human attestation; it does not decide whether the
  reviewed change is good.
- No arbitrary shell script is treated as a trustworthy gate merely because it
  exits zero. Any command-backed evaluator must go through the declared lane,
  capture provenance, and meet the non-empty/parser-complete evidence rules.
  Whether command-backed evaluation is in the M10 slice is open.
- No automatic ratchet re-baselining. A failed comparison never moves its own
  floor, and a passing comparison never silently replaces the baseline.
- No merge-conflicting numeral committed to a gate file. Threshold policy may
  be committed; the active measurement and baseline evidence are audit-class
  records selected by an explicit pin.
- No attempt to prove that the evaluator was operated by a biological human.
  M10 can authenticate an AIRA-generated attestation record and bind it to an
  actor/session; operator identity and human presence remain a policy concern.
- No cross-machine or remote attestation authority. The common-dir audit home
  is machine-local, as in the whole-product design.
- No general gate composition, arbitrary boolean expressions, automatic gate
  dependency graph, or policy DSL. M10 supports one gate evaluation and its
  declared canary; composition and routing are deferred unless the architect
  selects them explicitly.
- No full test-report archive or flaky-test classifier. M10 consumes the
  existing/planned `TestReport` shape and marks missing comparability or
  coverage as unevaluated. The richer archive and flaky-rate gauges remain
  Phase 4 work.

## 3. The gate model and terminology

### 3.1 Gate kinds

| Kind | Positive evidence | Negative evidence | Missing evidence |
|---|---|---|---|
| `checkable` | AIRA's deterministic evaluator establishes the predicate, and the gate is proven-to-fire | The evaluator establishes the predicate is false, or its canary does not fire | `unevaluated`; never a pass |
| `manual` | A valid, AIRA-issued attestation says pass, with subject/evidence/lane binding, and proof-of-fire is valid | A valid attestation says fail | `unevaluated`; no free-form “LGTM” field counts |
| `ratchet` | Current measurement is comparable to the explicitly pinned durable baseline and satisfies the policy, with proof-of-fire valid | A comparable measurement violates the policy, or its canary does not fire | `unevaluated`; missing baseline/measurement is not zero |

“Gate fired” means the evaluator produced the expected negative verdict on a
known-bad input in the same effective lane. It does not mean that an ordinary
production failure happened to occur. The negative evidence is what proves the
guard is connected to the thing it claims to guard.

### 3.2 Subjects and lanes

Every evaluation is bound to a **subject** and a **lane**:

- Subject: project, ticket if applicable, commit/tree digest, and the gate
  definition digest. A result for another commit cannot make the current
  commit green.
- Lane: evaluator identity/version, suite or checker ID, configuration digest,
  environment digest where relevant, worktree identity, and runner mode. A
  proof for `go test ./...` with one configuration does not prove a different
  command, exclude set, or parser.

The lane identity is part of proof matching and ratchet comparability. A
changed evaluator or configuration starts a new proof requirement unless the
architect deliberately defines a compatibility migration.

## 4. Data model and durability

The domain model uses closed enums, tagged kind-specific payloads, canonical
digests, and constructors that reject cross-kind fields. The persisted format
is JSON-in-`---` frontmatter where the existing AIRA content format applies;
JSONL audit records use canonical field ordering and explicit schema versions.

### 4.1 Gate definition — git-durable content truth

Each gate is a separate file under `.aira/gates/<gate-id>.json` (or the
project's established JSON-frontmatter equivalent), so two agents changing
different gates do not edit a shared registry. The committed definition has:

| Field | Contract |
|---|---|
| `schema_version` | Versioned decoder; unknown required versions are invalid, never guessed |
| `id` / `name` | Stable project-local ID and display name; filename must equal `id` |
| `kind` | Exactly one of `checkable`, `manual`, `ratchet` |
| `applies_to` | Closed subject selector: lifecycle step, ticket/milestone/label selectors, and optional path/suite scope |
| `lane` | Named lane plus evaluator/checker identity and configuration digest inputs |
| `proof_policy` | Required proof mode, max age, and whether a current canary is required |
| `canary_ids` | Non-empty for a trustworthy gate; references must resolve to declarations |
| kind payload | Exactly one of `checkable`, `manual`, or `ratchet` details |
| `enabled` / `advisory` | Disabled gates are reported as policy warnings and do not silently count as pass |
| `description` / `failure_guidance` | Human/agent-readable explanation emitted with a result |

The tagged payload is:

- `checkable`: an AIRA-owned checker ID and typed parameters. M10's first
  checker set should include the existing `check` dimensions and the
  non-empty/parser-complete test-report predicate. A command-backed checker,
  if selected, is still required to declare argv, cwd policy, environment
  digest policy, output parser, and expected non-empty evidence.
- `manual`: required evaluator role, evidence kinds accepted, and the review
  prompt template ID. It never contains a boolean `passed: true` authored in
  the file.
- `ratchet`: a typed metric/policy pair, comparison key, and a baseline
  selector. Examples are “no new failing tests”, “maximum failure count”, and
  “minimum line coverage”, but a metric with no source field is invalid.

Illegal states rejected at definition load/write include: two kind payloads,
no kind payload, a ratchet with no metric or comparator, a manual gate with a
machine-only evaluator, an unknown checker, an invalid selector, a canary
reference to another project, and a definition whose filename and ID differ.

### 4.2 Canary definition — git-durable declaration

Canaries live with the gate declaration or in separate
`.aira/gates/canaries/<canary-id>.json` files. A canary declaration contains:

| Field | Contract |
|---|---|
| `id`, `gate_id` | Stable local identity; one canary belongs to one gate |
| `mode` | `fixture`, `mutation`, `synthetic-report`, or `attestation-challenge`; closed and capability-checked |
| `seed` | A typed seed reference, never an unreviewed arbitrary numeral |
| `expected_gate_result` | Usually `fail`; the expected negative result is explicit |
| `lane_binding` | Must resolve to the gate's evaluator/config lane |
| `isolation` | Required isolated worktree/fixture/report boundary and cleanup policy |
| `cadence` | `on-demand`, `periodic`, or `every-evaluation` policy; default proposed as `every-evaluation` |
| `description` | What defect class the canary represents and what a non-fire means |

The canary runner constructs the seeded bad input in an isolated boundary,
runs the same evaluator and lane, captures evidence, and inverts the observed
result: expected `fail` means canary health `pass`; expected `pass` means
`E_GATE_CANARY_DID_NOT_FIRE`; inability to establish either result means
`U_GATE_CANARY_UNEVALUATED`. It never treats “the seed was accepted” as proof
that the gate ran.

For a manual gate, an `attestation-challenge` asks the evaluator to attest the
expected negative outcome against a known-bad review fixture. The challenge
record, evidence reference, actor, and signature are all required; a normal
manual pass cannot prove its own gate.

### 4.3 Gate result — audit record plus DB projection

Each evaluation appends an immutable result record to the common-dir audit
store, for example under `$(git rev-parse --git-common-dir)/aira/gates/`.
SQLite stores an index and the latest-by-subject projection. A result contains:

`result_id`, `gate_id`, gate-definition digest, subject, lane identity,
`verdict`, `trusted`/`suspect`, evaluator version, start/end timestamps,
evidence references, canary result if run, proof reference, baseline reference
if ratchet, stable findings, and a canonical record digest.

`trusted` is derived, not caller-set: a positive result is trusted only if all
proof, evidence, subject, lane, and freshness rules pass. A result can be
`fail` without proof when the failure is established; it cannot be `pass`
without proof. `suspect` is true for `U_GATE_UNPROVEN`, stale proof, stale
canary, or any other “the predicate looked green but the guard is not
trustworthy” case.

Results are append-only. Re-evaluation creates a new result; it never edits
history or overwrites the baseline. An audit record whose evidence is missing
or tampered is not silently dropped: the projection reports an appropriate
unevaluated/corruption finding.

### 4.4 Attestation and proof-of-fire records — audit durability

The existing attestation shape is extended, not replaced:

`type` (`manual` or `proven-to-fire`), record ID, gate/canary ID, subject,
evaluator/actor, session ID, lane identity, gate-definition digest, commit/tree
digest, result/evidence references, issued-at, expiry, nonce, previous-record
digest, record digest, and authentication tag.

The common-dir audit writer serializes records under a lock, fsyncs the record,
and uses a machine-local project key with restrictive permissions to authenticate
the canonical payload (HMAC-SHA-256 is the proposed mechanism). The previous
digest makes deletion/reordering detectable during audit verification. A
one-time nonce prevents replaying an attestation for another subject or lane.
SQLite has no authority to mint or modify an attestation; rebuilding it from
the audit log must reproduce the same authenticated record or fail with
`E_JOURNAL_CORRUPT`.

This makes a valid record unforgeable within the supported local threat model:
ordinary callers cannot manufacture a record that AIRA did not issue or alter
one without detection. It does not claim protection from a privileged user who
can replace the local key, binary, and audit directory, nor does it prove that
the named actor was a human. Those limits are surfaced in the open decisions.

`aira gate attest` must therefore issue/consume an AIRA challenge and append a
record; importing a JSON blob, setting a DB boolean, or writing a gate file
cannot create a valid attestation.

### 4.5 Ratchet baseline — durable evidence, not a committed numeral

The baseline is a **pinned evidence identity**, not a number in
`.aira/gates/<id>.json`:

- A baseline record names the gate, metric/comparator version, lane/comparison
  key, source test-report or measurement IDs, source commit, evidence digest,
  derived snapshot digest, and an explicit pin actor/time/reason.
- The derived snapshot may contain the small failing-test set, counts, or
  coverage value needed for deterministic comparison. It lives in the common-dir
  audit class and is content-addressed; it is not a merge-conflicting project
  file and is retention-exempt while pinned.
- The gate definition commits only the policy and baseline-selection rule, such
  as “active explicitly pinned baseline for this gate and lane”. It contains no
  mutable `baseline_count: 17` or similar stored numeral.
- `aira gate baseline pin` creates a new immutable baseline record and advances
  an active pointer in the audit log. It never edits the old record. Pinning is
  explicit, visible in history, and can require a manual attestation according
  to project policy.
- A baseline is valid only if its source evidence is present, parser-complete,
  provenance-matched, and comparable with the current evidence. Missing or
  evicted source data is `U_GATE_BASELINE_MISSING`, never a zero baseline.
- A failed ratchet never updates the baseline. A pass does not update it either.
  This is the direct anti-goal of stored numerals that merge-conflict and need
  constant re-baselining.

The baseline is a deliberate exception to ordinary live metrics: a ratchet
floor must survive telemetry retention. All other gauges remain live queries
with universe and as-of, and an uncomputable gauge remains unevaluated.

## 5. Evaluation protocol

The proposed lifecycle is:

1. Load and validate the committed definition and canary declarations.
2. Resolve the subject commit/tree and compute the effective lane/config
   digest. Refuse an ambiguous selector as an invocation error.
3. Run the declared evaluator or consume only evidence it explicitly names.
   Record start/end, parser completeness, result, and every evidence digest.
4. For a ratchet, resolve the active pinned baseline and compare only fields
   with matching identity. Do not coerce missing counts, coverage, or tests to
   zero.
5. Run the declared canary when requested or required by cadence. It must use
   the same evaluator, lane, and configuration and an isolated seeded input.
6. If the negative canary result is demonstrated, append a proof-of-fire
   attestation. A one-time `gate prove` operation may record an already
   captured AIRA-issued canary result, but it cannot accept a caller-supplied
   claim without evidence.
7. Fold the predicate, proof freshness, canary health, evidence availability,
   and baseline comparison into one gate result. Append the audit result and
   update only the rebuildable index.

### 5.1 Proof freshness

Proof is scoped by `(gate definition digest, evaluator version, lane,
configuration digest, canary ID)`. A proof from a different lane is not close
enough. A proof older than `max_age` becomes `U_GATE_PROOF_STALE`; a proof
whose record or evidence cannot be authenticated becomes
`U_GATE_PROOF_UNAVAILABLE`. If a current continuous canary fires, it refreshes
the proof for that evaluation. Otherwise the last valid proof can be used only
within the declared age.

The default should favour a continuous canary over a long max age. A January
proof must not make a June check green after an exclude or parser change.

### 5.2 Manual gates and review emission

When a manual gate has no valid current attestation, `gate run` returns
`unevaluated` and emits a structured review request containing gate purpose,
subject, lane/config digest, required evidence kinds, failure guidance, and a
challenge ID. It is an AIRA record, not a model call. A human-facing or agent
face can render that request; the later `review` verb may route it.

`aira gate attest` accepts only the challenge ID, explicit result, actor,
evidence references, subject commit, and confirmation required by the policy.
It validates that the evidence is present and that the attestation is bound to
the challenge. A stale, wrong-commit, wrong-lane, reused, or unauthenticated
attestation is not a pass.

### 5.3 Checkable gates and `aira check`

M10 should expose existing AIRA-owned deterministic checks as named evaluator
IDs rather than copying their logic into gate definitions. The traceability
dimension is one such evaluator. A test-report evaluator must require exit 0,
parser-complete evidence, and test-count > 0; a bare successful process is not
`tests-green`.

The dedicated `aira gate run` evaluates and records. `aira check` remains
side-effect-free: it validates the gate files, verifies audit/projection
integrity, selects the latest result applicable to the current subject, and
reports the gate dimension. A missing applicable result is
`U_GATE_NO_RESULT`; it does not launch an implicit check and cannot pass.

### 5.4 Proposed ratchet comparison contract

The first ratchet comparator should be deterministic and deliberately small:

- Normalize each test report by suite ID, test name, outcome, retry index,
  shard, configuration digest, parser-complete flag, and commit. Only
  comparable reports with parser-complete true and a non-zero discovered test
  count enter the comparison.
- For a no-new-failures ratchet, derive `new_failures` as the current failing
  test identity set minus the pinned baseline failing set. A non-empty set is
  `E_GATE_RATCHET_REGRESSED`; failures already present at baseline do not become
  new failures merely because they remain red.
- A current report with an absent outcome, missing test identity, or an
  incompatible suite/config/shard identity is `U_GATE_INCOMPARABLE`, not a
  passing empty set. Retry results remain distinct; a later retry pass cannot
  erase a first-attempt failure unless the declared comparator explicitly says
  how retries fold.
- A coverage ratchet compares a declared field (for example line coverage)
  using the committed comparator policy and the pinned baseline's matching
  field. Missing either field is `U_GATE_INCOMPARABLE`; it is never coerced to
  zero. The comparison must report the field, baseline evidence ref, current
  evidence ref, and observed delta in the gate result.
- Known-flaky exclusions are not invented by the comparator. Until the Phase-4
  archive/classifier exists, a project may provide an explicit, reviewed
  lane-bound exclusion; otherwise contradictory observations remain evidence
  for the architect's chosen flaky policy and cannot silently disappear.

The exact metric catalog, retry folding, and flaky policy remain open
decisions, but every implementation must preserve the missing-is-unevaluated
and no-automatic-rebaseline rules.

## 6. Verdict and honesty contract

### 6.1 Per-gate verdict table

| Situation | Gate verdict | Stable code | Trust |
|---|---:|---|---|
| Predicate established and valid proof/canary is current | `pass` | none | trusted |
| Predicate established false | `fail` | `E_GATE_FAILED` | known negative |
| Ratchet comparison detects regression | `fail` | `E_GATE_RATCHET_REGRESSED` | known negative |
| Canary expected failure did not occur | `fail` | `E_GATE_CANARY_DID_NOT_FIRE` | fail-closed |
| No current proof-of-fire | `unevaluated` | `U_GATE_UNPROVEN` | suspect |
| Proof is outside max age or lane/config changed | `unevaluated` | `U_GATE_PROOF_STALE` | suspect |
| No applicable result | `unevaluated` | `U_GATE_NO_RESULT` | suspect |
| Evidence cannot be read, authenticated, or parsed completely | `unevaluated` | `U_GATE_EVIDENCE_UNAVAILABLE` | suspect |
| Ratchet baseline is absent/evicted/incomparable | `unevaluated` | `U_GATE_BASELINE_MISSING` / `U_GATE_INCOMPARABLE` | suspect |
| Canary cannot run or its result cannot be established | `unevaluated` | `U_GATE_CANARY_UNEVALUATED` | suspect |

Warnings are orthogonal. Examples include `W_GATE_DISABLED` and
`W_GATE_PROOF_EXPIRING`; they never turn an unevaluated gate into pass and never
turn a warning into fail.

### 6.2 Proposed stable code and exit additions

The implementation must add these to the single `internal/store.ExitCodes`
catalog and generated response contract, with no adapter-specific aliases:

| Code family | Proposed codes | Exit class |
|---|---|---:|
| Invocation/definition | `E_GATE_INVALID`, `E_GATE_KIND_INVALID`, `E_GATE_CANARY_INVALID`, `E_GATE_ATTESTATION_INVALID`, `E_GATE_BASELINE_INVALID` | 2 |
| Established gate failure | `E_GATE_FAILED`, `E_GATE_RATCHET_REGRESSED`, `E_GATE_CANARY_DID_NOT_FIRE` | 1 |
| Warning | `W_GATE_DISABLED`, `W_GATE_PROOF_EXPIRING` | 0 |
| Unevaluated | `U_GATE_NO_RESULT`, `U_GATE_UNPROVEN`, `U_GATE_PROOF_STALE`, `U_GATE_PROOF_UNAVAILABLE`, `U_GATE_EVIDENCE_UNAVAILABLE`, `U_GATE_BASELINE_MISSING`, `U_GATE_INCOMPARABLE`, `U_GATE_CANARY_UNEVALUATED` | 3 |
| Audit/runtime integrity | existing `E_JOURNAL_CORRUPT`, `E_RECEIPT_IO`, `E_DB_CORRUPT`, `E_INTERNAL` | 4 |

The exact catalog names may be consolidated, but their meanings and exit
classes must remain stable. `E_GATE_CANARY_DID_NOT_FIRE` is intentionally an
error-class failure, not a warning. `U_GATE_*` is never converted to a zero,
pass, or an ordinary warning.

### 6.3 Folding into `CheckReport`

Add a `gates` dimension to the existing `CheckReport.Dimensions`. Each gate
finding is placed in the existing separate collection according to its kind:

- `fail` findings go to `Findings`;
- warnings go to `Warnings`;
- `unevaluated` findings go to `UnevaluatedFindings` and set the report's
  `Unevaluated` bit.

The existing precedence remains `fail > unevaluated > pass`; warnings do not
change it. A report with a failed relation and an unevaluated gate must retain
both dimensions and both findings. A report with only an unevaluated gate
returns verdict `unevaluated` and exit 3. A report with only gate warnings
returns `pass` and exit 0.

The gate dimension is not a replacement for gate detail. The response should
include a bounded, deterministically ordered list of gate summaries with ID,
kind, verdict, trust/suspect flag, codes, subject, evidence refs, and baseline
ref where applicable. MCP follows the existing overflow/distribution rules;
truncation must be explicit.

### 6.4 Folding into `aira ready`

For each applicable ticket, `ready` evaluates the current gate projection in
addition to the existing `blocked-by`, hold, integrity, and traceability
findings:

- gate `fail` makes the ticket `ready=false` and adds a gate failure finding;
- gate `unevaluated` makes `ready=false` and adds the unevaluated code;
- gate warnings are shown but do not make a ticket unready;
- no applicable gate means no gate finding;
- a stale or missing cached result is unevaluated, not an implicit rerun.

This is a report of readiness, not an enforcement hook. The existing ready
summary should say how many prerequisites and gates are failed,
unevaluated, and passed, preserving the whole-product “not ready — 2 failed,
1 unevaluated, 3 passed” shape.

## 7. Faces and dispatch surface

All operations are entries in the same dispatch table consumed by `core.Do`,
the CLI parser, MCP schema generator, Skill action catalog, and generated guide.
No gate face gets a private store path or verdict folding logic.

### 7.1 Proposed CLI grouped verb

`aira gate` is a grouped verb with these operations:

| Operation | Purpose | Safety |
|---|---|---|
| `gate add` | Create a validated gate definition and its declared canary refs | mutate |
| `gate ls` / `gate show` | List or inspect definitions and current projections | read |
| `gate set` | Change policy/configuration through a validated content mutation | mutate |
| `gate run` | Evaluate one/all applicable gates; optional explicit canary run | reconcile |
| `gate check` | Read current result/proof state without running a check | read |
| `gate attest` | Answer a manual challenge and append an authenticated attestation | mutate |
| `gate prove` | Promote an AIRA-issued canary failure to proof-of-fire | mutate |
| `gate canary run` | Run a named canary in its isolation boundary | reconcile |
| `gate canary show` | Inspect canary declaration and latest result | read |
| `gate baseline pin` | Explicitly pin a comparable ratchet evidence set | mutate |
| `gate baseline show` | Inspect active/ historical baseline records | read |
| `gate review` | Emit a manual-gate review request/challenge without judging it | read |

The exact CLI spellings may use `gate canary ...` and `gate baseline ...` as
nested operation metadata, but they must remain one dispatch family. `aira
check` remains the existing single-shape verb with the added `gates`
dimension; it does not grow an implicit `--run` mode in this milestone.

### 7.2 MCP and Skill

The preferred MCP surface is one grouped `aira_gate` tool with a discriminator
for the operations above, matching `aira_requirement`, `aira_finding`, and
`aira_link`. If the existing MCP generator cannot represent nested operations,
flatten only at the descriptor projection; the core verb remains `gate`.

M8b metadata must declare each operation's summary, safety class, canonical
arguments, requiredness, and explicit example argv. The Skill and guide must
list the same operation set. Parity tests must prove that every documented
example reaches the same `core.Request` as MCP and the real CLI.

All faces must expose the same response contract: stable code, per-gate
verdict, overall verdict, evidence/proof detail, and exit mapping. The Skill
guide must state that `unevaluated` is not pass and not zero, and that a
canary non-fire is fail-closed.

## 8. Invariants

1. **Three verdicts, never two.** Every applicable gate result is exactly
   `pass`, `fail`, or `unevaluated`; no missing value is treated as false,
   zero, or pass.
2. **Green requires trust.** A gate cannot report pass unless its predicate,
   evidence, subject/lane binding, and proof-of-fire/freshness all establish
   trust.
3. **Canary non-fire fails closed.** If the expected known-bad input does not
   make the gate fail, the gate result is fail with
   `E_GATE_CANARY_DID_NOT_FIRE`; it is never a warning or unevaluated.
4. **Proof is evidence-bound.** Proof is tied to gate digest, evaluator,
   lane/config, canary, subject scope, timestamp, and an authenticated audit
   record. A caller-supplied boolean cannot create proof.
5. **Ratchet baseline is not a project numeral.** Gate content commits policy
   and selection rules; the active floor is an explicit, immutable,
   content-addressed audit record derived from provenance-bearing evidence.
6. **No automatic re-baselining.** Neither failure nor success changes an
   active baseline without an explicit pin operation and audit record.
7. **Evidence is non-empty and complete.** A successful process with zero
   tests, a skipped scanner, an incomplete parser, or absent coverage cannot
   satisfy the corresponding gate.
8. **One source of truth.** Gate definitions are read from git content; audit
   records are the authority for attestations/results/baselines; SQLite is a
   rebuildable projection. No DB-only pass bit exists.
9. **Subject identity is exact.** A result for another commit, worktree,
   definition digest, or incomparable lane does not satisfy the current gate.
10. **Check folding is orthogonal.** Gate findings use the existing
    `CheckReport` collections and preserve independent dimensions under the
    existing fail > unevaluated > pass precedence.
11. **Ready remains advisory.** Gate results can make `ready=false` and emit
    guidance, but cannot refuse an otherwise structurally valid mutation.
12. **Generated faces cannot drift.** CLI, MCP, Skill, help, and guide derive
    operation names and argument contracts from the dispatch descriptors.
13. **Audit replay is deterministic.** Rebuilding SQLite from common-dir
    records yields the same latest projections, or reports stable corruption;
    it never invents a result to repair a missing record.
14. **Canary isolation.** Seeded failure execution cannot modify the caller's
    working tree or silently become evidence for a different subject.

## 9. Tests (TDD; every confirmed counterexample becomes a regression test)

### 9.1 Domain and content

1. Gate definition round-trip preserves canonical JSON and rejects unknown
   kind, duplicate kind payloads, absent kind payloads, invalid selectors,
   unknown evaluators, invalid proof policy, and filename/ID mismatch.
2. Kind-specific validation rejects a manual payload on a ratchet gate, a
   ratchet without metric/comparator/baseline selector, and a canary belonging
   to another gate/project.
3. Two gate files edited independently do not share a registry file; rebuild
   discovers both deterministically and duplicate IDs fail closed.
4. Canary declarations round-trip and reject an invalid mode, seed, expected
   verdict, lane binding, isolation policy, or cadence.

### 9.2 Audit, authentication, and recovery

5. Attestation/result writes are lock-protected, fsynced, canonical, and
   replayable; duplicate nonce, changed subject, changed evidence, reordered
   record, and modified authentication tag are rejected.
6. Crash-window tests cover DB projection before audit append, audit append
   before projection, and result append before evidence promotion. Reconcile
   repairs only the documented derived side and never mints a pass.
7. Drop SQLite and rebuild from common-dir gate records. Definitions remain
   sourced from git; results, proof, canary history, and active baseline
   pointer are reconstructed exactly.
8. Missing audit record, missing HMAC key, malformed JSONL tail, or tampered
   chain produces `E_JOURNAL_CORRUPT`/`E_RECEIPT_IO` as appropriate, not a
   green projection.
9. An imported or DB-edited “attested=true” row cannot satisfy `gate check`.

### 9.3 Verdict and honesty matrix

10. Table-test every gate kind for pass, established fail, and unevaluated.
    Assert exact code, `trusted`/`suspect`, evidence references, and exit
    class.
11. A checkable predicate that returns pass with no proof becomes
    `U_GATE_UNPROVEN`; a stale proof becomes `U_GATE_PROOF_STALE`; a wrong
    lane/config proof does not match.
12. A manual gate with no challenge response emits a review request and
    `U_GATE_NO_RESULT`/`U_GATE_UNPROVEN`; a valid authenticated response can
    pass only the matching subject and lane.
13. A test-report result with exit 0 but zero tests, skipped parser, or
    incomplete report is unevaluated, never `tests-green`.
14. A failed gate remains fail even if proof is absent; an established failure
    is not hidden behind suspect status.
15. Mixed report: unrelated integrity fail + gate unevaluated + gate warning
    retains all dimensions and folds overall to fail. Gate-only unevaluated
    folds to exit 3; gate-only warning folds to pass/exit 0.
16. `aira check` is read-only and does not invoke an evaluator, alter the
    worktree, pin a baseline, or create proof. `gate run` does each permitted
    write exactly once.

### 9.4 Canary matrix

17. A canary that produces the expected gate failure records canary pass and a
    dated proof-of-fire linked to the run/evidence.
18. A canary whose evaluator returns pass produces
    `E_GATE_CANARY_DID_NOT_FIRE`, gate fail, overall fail, and no proof.
19. A canary that cannot run, is parser-incomplete, or loses evidence produces
    `U_GATE_CANARY_UNEVALUATED`, never a canary pass.
20. Canary uses the same evaluator version, config, lane, and gate digest as
    the normal gate; changing any one prevents proof reuse.
21. Canary isolation leaves the caller worktree byte-for-byte unchanged and
    cleans up its temporary boundary on success and failure.
22. Continuous-cadence canary runs on every requested evaluation; stale
    on-demand proof decays according to max age; a failed canary cannot refresh
    proof.
23. A manual attestation-challenge canary requires the challenge-bound,
    authenticated negative attestation; an ordinary manual pass cannot prove
    fire.

### 9.5 Ratchet matrix

24. Baseline pin stores provenance and derived snapshot in audit storage; the
    gate file contains no active numeral and remains merge-independent.
25. Missing, evicted, malformed, wrong-lane, wrong-suite, wrong-config, or
    parser-incomplete baseline evidence is unevaluated, never an empty set or
    zero.
26. No-new-failures detects a genuinely new failure; a coverage ratchet
    refuses to evaluate when current or baseline coverage is absent; a
    threshold regression returns `E_GATE_RATCHET_REGRESSED`.
27. A pass and a fail do not move the active baseline. Explicit pin creates a
    new immutable record, preserves history, and requires the configured
    actor/reason fields.
28. Same test/commit with incompatible config is incomparable rather than
    flaky or a valid ratchet comparison. Retry results retain retry identity.
29. A pinned baseline survives DB loss and telemetry retention while an
    unpinned operational report may be evicted and then makes a dependent gate
    unevaluated.

### 9.6 Integration, ready, and faces

30. Gate definitions and latest evidence appear in the `gates` check
    dimension; `aira check` preserves existing traceability findings and
    precedence.
31. `ready` marks a ticket unready for a gate fail or unevaluated, shows gate
    codes and counts, ignores gate warnings for readiness, and still never
    refuses a ticket mutation.
32. Dispatch descriptor tests cover every `gate` operation, exact safety
    class, argument read set, requiredness, and explicit example. Missing
    metadata fails generation.
33. CLI↔core↔MCP parity covers nested canary/baseline operations and gate
    verdict responses. Skill/guide action set equals descriptor operations.
34. End-to-end generated examples reach core and return stable AIRA outcomes,
    not argument-construction failures. MCP exposes bounded gate details with
    explicit truncation/distribution if needed.
35. Static/no-cgo build remains valid; ordinary gate inspection needs no daemon
    or network and evaluation uses only the declared local lane.

## 10. Risks and mitigations

| Risk | Mitigation |
|---|---|
| A gate passes because its checker silently skipped input | Require parser-complete/non-empty evidence, exact lane identity, proof-of-fire, and canary non-fire failure |
| Proof from an old lane survives a checker/config change | Bind proof to definition/evaluator/lane/config digests and decay it to unevaluated |
| Canary is a synthetic lie that bypasses the real gate | Run the same evaluator and lane against an isolated seeded input; record full provenance; test non-fire and lane mismatch |
| Ratchet baseline creates merge churn | Keep policy in per-gate git content; keep active derived baseline as immutable common-dir evidence/pointer; never auto-rebaseline |
| DB loss turns missing audit data into a green default | Audit is authority; missing/invalid records produce stable corruption or unevaluated findings |
| Manual attestation becomes untraceable prose | Challenge-bound, authenticated, append-only record with actor/session/commit/evidence and replay tests |
| HMAC is mistaken for proof of a human | State the local threat model and operator-identity limit in generated guidance; keep human role/policy explicit |
| `check` unexpectedly runs expensive or mutating commands | Separate read-only `check` from explicit `gate run`; test no subprocess/no write behaviour |
| Gates unexpectedly become merge blockers | Keep `advisory` policy and ready reporting separate from write validation; add an explicit future policy decision |
| Test-report retention invalidates a ratchet | Promote pinned baseline evidence to audit/retention-exempt storage; missing unpinned evidence is unevaluated |
| CLI/MCP/Skill gate operations drift | One grouped dispatch descriptor, operation metadata, request-parity and real-entrypoint tests |

## 11. Expected yield

M10 turns “the guard is green” into a claim with inspectable evidence. AIRA
can distinguish a real failure, a trustworthy pass, a stale/unproven guard,
and a check that never established its result. Canaries expose silent skips;
proof-of-fire exposes dead gates; ratchets retain a durable floor without
committing merge-conflicting numerals; manual review remains a recorded human
decision rather than fake automation. `check` and `ready` become honest
consumers of that record while preserving AIRA's advisory-first Phase-3
boundary.

## 12. Open decisions for the architect

These are intentionally not resolved unilaterally. Each can change M10's
implementation boundary or its durable contract.

1. **M10 versus runner-lite:** Does M10 ship the local evaluator/runner needed
   for command-backed checks and isolated canaries, or only the gate engine and
   evidence interface, with execution landing in the runner milestone?
2. **Initial checker catalog:** Which AIRA-owned checkers are in scope first
   (`traceability`, `tests-green`, file/selector checks, report predicates),
   and which are explicitly deferred?
3. **Command-backed gates:** Are arbitrary declared commands allowed in M10,
   or must every M10 check be a closed built-in evaluator? If allowed, what
   sandbox, timeout, environment, output cap, and trust policy applies?
4. **`aira check` execution policy:** Is the proposed cached/read-only model
   correct, or should `aira check` synchronously run some cheap gates? If it
   runs them, which side effects, timeout, and cancellation semantics apply?
5. **Canary representation:** Which canary modes are real in M10—fixture,
   mutation, synthetic report, and manual challenge—and who owns fixture
   maintenance? Is a synthetic report acceptable for any production gate?
6. **Canary cadence:** Is `every-evaluation` the default, how are periodic
   canaries scheduled without a daemon, and what is the maximum age of a
   one-time proof?
7. **Proof expiry:** What exact default `max_age` applies, and does a current
   continuous canary replace rather than merely refresh one-time proof?
8. **Manual authenticity:** Is an AIRA-issued HMAC/OS-protected record enough,
   or is a user/device key, external signature, TTY confirmation, or separate
   human approval system required? What does “human-attested” mean for MCP?
9. **Baseline storage:** Should the proposed common-dir content-addressed
   snapshot be authoritative, or should baselines live in a separate durable
   local store, git notes, or another machine-shared artifact? How should
   cross-worktree and future cross-machine promotion work?
10. **Baseline promotion:** Who may pin/revoke/rollback a baseline? Is a reason
    sufficient, or does every pin require a manual attestation and review?
11. **Ratchet metrics:** Which metrics and comparison rules are load-bearing in
    M10, especially coverage and failure-set identity? Is the planned
    `TestReport` schema available soon enough, or should coverage ratchets be
    deferred entirely?
12. **Flaky tests:** Should known-flaky classification be consumed by M10,
    treated as incomparable/unevaluated, or deferred until the Phase-4 archive
    and classifier exist?
13. **Gate identifiers:** Are project-local slug IDs preferable, or should
    gates use the shared AIRA allocator and a durable gate prefix? The choice
    affects migration and cross-project references.
14. **Gate policy location:** Should applicability live in `.aira/config`, in
    each gate file, or in a separate committed project policy? How are paths,
    lifecycle steps, milestones, and ticket selectors combined?
15. **Gate composition/dependencies:** Is one gate per evaluation enough, or
    must M10 support boolean composition, gate-to-gate dependencies, or gates
    as `blocked-by` prerequisites? If so, what prevents cycles and vacuous
    composition?
16. **Ready semantics:** Should any applicable unevaluated gate make `ready`
    false as proposed, or should some gates be advisory-only in readiness while
    still appearing in `check`?
17. **Phase-3 enforcement:** When, if ever, may a project turn a gate failure
    into a write/merge block? Is that a per-project policy with an explicit
    exception/waiver attestation, and does it remain outside M10?
18. **Review emission boundary:** Does M10 own `gate review` and challenge
    rendering, or should manual-gate prompts wait for M11's `review` verb and
    routing policy? Which face owns the human prompt?
19. **Evidence retention:** Are all evidence refs for active passes pinned, or
    only proof and ratchet sources? What are the raw-output size, compression,
    and eviction rules before the full runner lands?
20. **Remote boundary:** When AIRA becomes remote-capable, are attestations and
    baseline pins machine-local receipts or project-shared signed records? No
    cross-machine promise is made by this M10 design.
21. **Migration/versioning:** How are pre-M10 projects with no gate directory,
    no audit key, or existing informal `tests-green` records surfaced? They
    must not receive an implicit pass, but the architect must choose whether
    initialization creates no gates, seeded gates, or an explicit migration.
