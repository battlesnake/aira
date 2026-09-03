# AIRA-53 + AIRA-54 — gate materialization and empty-set honesty

Date: 2026-09-03
Tickets: AIRA-53 (P1, `gate add`/`set` materialize nothing), AIRA-54 (P1, `gate
check` fake-passes an empty gate set)
Starting commit: `994abee`
Branch: `aira53-54-gate-honesty`

## Why these land together

The two defects compose into an observed, not hypothetical, silent green board.
A peer session's verification step was "run `aira gate check`, expect `pass`".
`gate add` registered nothing (AIRA-53) and the resulting empty set reported
`pass` (AIRA-54), so the verification of a failed registration reported success.
AIRA-53 alone is insufficient: any *other* future silent-registration failure
would still be masked by AIRA-54's vacuous pass. AIRA-54 alone is insufficient:
the documented creation verb would still not create.

## Source of truth

Gate files under `.aira/gates/<id>.json` (and canary declarations under
`.aira/gates/canaries/<canary-id>.json`) remain the single authenticated source
of truth. Every gate read path already reads files, not SQLite:
`ListGates`/`gate ls`, `gate show`, `GateCheck`, and `RunGate` all go through
`discoverGates`. This plan does not move authority into the database and does not
let a caller mint a verdict from input fields.

## AIRA-54 — empty gate set is `unevaluated`, not `pass`

`internal/store/gate_eval.go:604-613` short-circuits an empty discovery to
`gate.VerdictPass`. This violates the CLAUDE.md hard rule: "A check that cannot
establish its result reports `unevaluated`, never a fake pass or zero." An
unpopulated gate set establishes nothing; `pass` asserts a positive fact
(nothing failed) that was never evaluated.

### Behavior

- `GateCheckReport` gains a `Code string` field carrying the report-level reason.
- Zero discovered gates ⇒ `Verdict: gate.VerdictUnevaluated`, `Code:
  "U_GATE_SET_EMPTY"`, `Results: []` (empty, not nil), all three counters 0.
- A non-empty set is unchanged in every respect, including the existing
  `finishGateReport` fold (any `fail` ⇒ `fail`; else any `unevaluated` ⇒
  `unevaluated`; else `pass`).

### Why this actually bites

`internal/core/core.go:2684` `verdictExit` maps `unevaluated` ⇒ **exit 3**, and
the response `Code` becomes `UNEVALUATED` (core.go:565). So `aira gate check &&
merge` stops on an empty set instead of proceeding. The fix is effective at the
process boundary, not only in the JSON payload. This exit-code transition is
itself a required test assertion, not an incidental detail.

`U_GATE_SET_EMPTY` is a verdict *reason* code carried inside the report data, in
the same family as the existing `U_GATE_NO_RESULT` / `U_GATE_UNPROVEN` /
`U_GATE_PROOF_STALE`. Like those, it is not added to `store.ExitCodes`; the
response-level code is the verdict class (`UNEVALUATED`), which already maps to 3.

### Aggregate `aira check` propagation (REVISED after review round 1)

The v1 plan proposed narrowing this with `hasGateContent()`, on the stated
premise that a gate-less project simply has no `gates` dimension entry. **That
premise was false.** `Check` (check.go:127-132) *pre-seeds*
`Dimensions["gates"] = "pass"`. So today a project with zero gates does not get
an absent dimension — it gets an affirmative, fabricated `gates: pass` in its
output. That is the identical AIRA-54 fabricated-pass bug reached through a
second face, and it must be fixed, not narrowed.

Decisive in-repo precedent: the sibling `traceability` dimension already treats
an empty registry as unevaluated, and `traceability_test.go:312` asserts that an
empty requirement registry makes the **aggregate** report
`Verdict == "unevaluated"`, `Dimensions["traceability"] == "unevaluated"`, plus a
`U_TRACE_EMPTY` unevaluated finding. The project has therefore already decided
this exact question for a sibling dimension, with no "feature not in use"
exemption. Gates follow the same shape:

- Zero discovered gates ⇒ `Dimensions["gates"] = "unevaluated"` and an
  `unevaluated` finding `{Code: "U_GATE_SET_EMPTY", Subject: "gates"}`, which via
  `addFinding` sets `report.Unevaluated` and folds the aggregate verdict to
  `unevaluated`. No `hasGateContent` narrowing.

### Prerequisite: `checkGatesReadOnly` downgrades genuine passes (load-bearing)

`checkGatesReadOnly` (gate_eval.go:737-743) builds every finding with
`Kind: "unevaluated"` and only promotes it to `"fail"` when the verdict is
`fail`. A **genuinely passing, proof-validated, trusted** gate therefore becomes
an `unevaluated` finding with an empty code, flipping `aira check` to
`unevaluated`. `GateCheck` already guarantees a `pass` result is trusted
(gate_eval.go:702 downgrades any untrusted pass), so this discards established
truth — a false-fail in the opposite direction. `Ready` gets this right at
relation_ready.go:582 by skipping non-fail/non-unevaluated results;
`checkGatesReadOnly` looks like an oversight.

This is not scope creep: without fixing it, `Dimensions["gates"]` is
`unevaluated` in essentially every scenario, which makes the new empty-set signal
**unobservable** and my own aggregate test vacuous. Fix: skip `pass` results (no
finding), matching the `Ready` precedent. Regression tests assert all three
directions — genuine pass ⇒ `gates: pass`; fail ⇒ `fail`; unevaluated ⇒
`unevaluated`.

## AIRA-53 — `add`/`set` materialize a real definition

`GateActionWithFields` (gate_eval.go:463) discards `fields` (`_ = fields`) and
falls through to `GateAction`, whose `"add"`/`"set"` case is a `ListGates()`
lookup returning `E_NOT_FOUND: gate not found` when the file does not already
exist. Meanwhile the dispatch table (core.go:2016) documents `add` as "Add a gate
definition" with a full flag example, `Safety: SafetyMutate`.

Direction (a) from the ticket — real materialization — is chosen. This is
completion of existing machinery, not new machinery: the flags are already
declared (`gateDefinitionOperationArgs`), already parsed
(`gateDefinitionInputFields`, core.go:2344), and already plumbed to a
`...WithFields` seam. Only the terminal write is missing. The presence of
`--mutation-*` flags in the *`add`* argument list is direct evidence that the
original intent was gate + canary materialization from these flags.

### `add`

Builds a `gate.GateDefinition` from input fields, validates, then writes.
Declared defaults (documented, not silently invented):

| Field | Value |
|---|---|
| `schema_version` | `gate.CurrentSchemaVersion` (2) |
| `id` | `gate_id` (already required; `ValidateGate` enforces the slug pattern) |
| `name` | `gate_id` |
| `applies_to` | `{all: true}` — the broadest and safest selector; a gate applying to everything can only add checks, never remove them. Narrower selectors are hand-authored or edited. |
| `lane` | `{name: gate_id, checker: <--checker>, evaluator_version: "1"}` |
| `proof_policy` | `{mode: required, max_age_secs: 604800, require_current_canary: true}` |
| `canary_ids` | `[<--canary-id>]`, default `<gate_id>-canary` |
| `enabled` | `true` |
| `advisory`, `advisory_in_ready` | `false` |

Payload by `--checker`:

- `command` ⇒ `Command{Argv, Cwd, EnvAllow, TimeoutMS, OutputCapBytes, Parser,
  Predicate}` from flags. `Command.Validate` already enforces positive
  `timeout_ms`, the cap range, predicate/parser pairing, sorted-unique
  `env_allow`, and `PATH`-for-relative-argv0. Missing `--timeout-ms` is therefore
  a loud `E_GATE_INVALID`, never a silent default.
- `check-dimension` ⇒ `Checkable{Dimension: <--dimension>}`. **One new flag,
  `--dimension`**, is required: the field is unrepresentable today and the gate
  cannot be valid without it.
- `manual-attestation` ⇒ `Manual{}` (all its fields are optional).
- `ratchet` ⇒ **refused** with `E_GATE_INVALID`: ratchet policy (metric,
  comparator, comparison key, baseline selection) has no flag surface at all, and
  inventing one is out of scope. Honest refusal beats a half-built definition.
- absent/unknown `--checker` ⇒ `E_GATE_INVALID` naming the accepted checkers.

`max_age_secs: 604800` and `evaluator_version: "1"` were changed from v1's `0`
and `""` on review: `0` permits an indefinitely reusable proof (the maximally
permissive choice for a freshly created gate) and an empty evaluator version
means the proof binding carries no evaluator identity even though `GateCheck`
compares that field. Both new values match the only in-repo precedent
(`testTraceGate`, gate_eval_test.go:48-51), so they are adopted convention rather
than invented policy.

Validation is `gate.RenderGate`, which validates then renders the exact
frontmatter form `ParseGate`/`discoverGates` expect. **Nothing is written unless
validation passes**, so a rejected `add` leaves no partial file.

`Enabled: true` is set for honesty of intent, but note a verified gap: **nothing
in any evaluation path consults `Enabled` or `Advisory`** — `discoverGates`
returns every definition regardless, and only `AdvisoryInReady` is read
(relation_ready.go:575). This plan does not invent enforcement for those fields;
the gap is recorded here rather than left implicit.

Existing gate ⇒ `E_GATE_EXISTS` (new code, registered in `store.ExitCodes` at
exit 1, matching the existing `E_RELATION_EXISTS`). `add` never silently
overwrites.

### `set`

Reads the existing definition, applies only the fields actually present in
`fields`, re-validates, rewrites. Absent gate ⇒ `E_NOT_FOUND`, which is now the
*correct* meaning of that code for `set`. This matches the documented example
`set unit-tests --timeout-ms 60000`.

### Canary materialization

When mutation fields are present (`mutation_kind` at minimum), `add`/`set` also
write `.aira/gates/canaries/<canary-id>.json`:
`CanaryDeclaration{SchemaVersion: 2, ID: canary_id, GateID: id, Mode: mutation,
Mutation: &MutationSeed{SchemaVersion: 1, ...}, ExpectedGateResult: fail,
LaneBinding: <lane>, Isolation: isolated-temp-git, Cadence: on-demand}`,
validated by `ValidateCanary` before any write. `MutationSeed.SchemaVersion` must
be 1 (`validateMutation`), which is pinned deliberately, not copied from the gate
schema version.

Without mutation fields no canary is written. The gate is then registered but
unprovable, and every path says so loudly and accurately: `gate run` fails with
the existing `E_GATE_CANARY_INVALID: referenced canary declaration is missing`,
and `gate check` reports that gate `unevaluated`/`U_GATE_NO_RESULT`. This is the
correct composition of the two fixes — `add` then `check` yields *unevaluated*,
never `pass`.

### Flag plumbing (three sites, not one)

`--dimension` must be added in all three places or the flag silently does
nothing: the verb-level `ArgSpec` list (core.go:1851-1858), the field extractor
`gateDefinitionInputFields` (core.go:2344), and the generated operation schema
`gateDefinitionOperationArgs` (core.go:2370). `canary_id` already exists as a
verb-level `ArgSpec` but is absent from `gateDefinitionOperationArgs`, so it is
added there too, making the documented override real. A parity test asserts the
generated schema and the extractor agree, so a future flag cannot be
half-wired.

### Write mechanics

Reuse the established content-write pattern rather than inventing a stronger one:
`acquireLock(s.pathLockFor(s.worktreeID, path))`, then existence probe via
`fileDigest`, then `writeAtomic(path, data, nonce)` (which already does
`MkdirAll`, `O_EXCL|O_NOFOLLOW` temp, fsync, rename, parent-dir fsync). The
check-then-rename limitation against a non-cooperative concurrent editor is
already documented at store.go:1860 and is accepted here on the same terms.

**Write order is load-bearing.** The canary is written *first* and the gate
definition *last*. Only the gate file makes the set non-empty and discoverable
(`discoverGates` skips directories, so `canaries/` is invisible to discovery), so
a crash or error between the two writes leaves no discoverable gate — never a
gate whose canary silently failed to land. `add` refuses with `E_GATE_EXISTS` if
either target path already exists.

### Index refresh (v1 deferral withdrawn)

v1 deferred the SQLite gate-projection rebuild. Review correctly objected: a
successful `add` would be immediately unusable by another documented operation,
because `rant --gate <id>` validates the ref against the `gates` table
(rant.go:510, verified). `add`/`set` therefore call `rebuildGateProjection` after
a successful write, and the returned result carries an honest three-state
`IndexStatus` (`"refreshed"` | `"stale"` with the reason). A failed refresh does
not become an error — the definition really was written — and it never claims
`refreshed` when the rebuild failed.

### Return shape

`add`/`set` return a typed `GateWriteResult{GateID, Operation ("created" |
"updated"), Path, Definition, CanaryID, CanaryPath, CanaryStatus ("materialized"
| "absent")}`. `CanaryStatus: "absent"` is the honest, machine-readable statement
that the gate cannot yet be proven. No existing test or caller depends on the
current return shape (`GateAction` has zero test coverage today), so this is
free. `show` keeps its current definition-only return.

## Third fabricated green found in review: `gate prove` exits 0 on unevaluated

Not in either ticket, found by review round 1 and verified. `GateAction`'s
`"prove"` case (gate_eval.go:376-386) calls `GateCheck`, finds the result for the
gate id, and returns it as a bare `any`. The core handler (core.go:1935-1943)
returns that value with **no `Verdict` set**, so `Response` falls through to
core.go:567 and reports `Code: "OK"`, `Exit: 0`. So `aira gate prove <id>` on an
`unevaluated`, unproven gate exits 0 and says OK — the same fabricated-green
class as AIRA-54, at the exact seam AIRA-53 already modifies.

Fix, small and contained: when the gate action result is a
`store.GateCheckResult`, return `handlerData{Data: result, Verdict:
result.Verdict}` so the established verdict reaches the exit code (`unevaluated`
⇒ 3, `fail` ⇒ 1). A test asserts `prove` on an unproven gate exits 3, not 0.

Separately, `prove`'s dispatch `Summary` claims "Record proof of fire", which it
does not do — proof-of-fire is minted only by `RunGate`/`AttestGate` from actual
canary evidence, which is the deliberate core of the security model. Implementing
on-demand proof minting would *break* that model, so the honest fix here is
remedy (b) from AIRA-53 applied to `prove`: correct the `Summary` to say it reads
back the latest recorded gate result and its proof linkage. Docs move to match
reality; the security model does not move.

## Deferrals (explicit)

- **`ready` does not treat an unpopulated gate set as a constraint.**
  `Ready` (relation_ready.go:571) gates its whole contribution behind
  `len(gateReport.Results) > 0`, so an empty set contributes nothing and ready
  records stay green. This is deliberately **not** fixed here, and the reasoning
  is written down rather than silent: unlike `check`, `ready` makes no
  affirmative gate claim when there are no gates (it adds no constraint rather
  than asserting `pass`), so the CLAUDE.md fabricated-pass rule does not bite the
  same way. Fixing it properly needs a durable "gates were configured in this
  project" signal, and no sound one exists from the filesystem: `hasGateContent`
  is false for an *accidentally emptied* gates directory, which is exactly the
  case the ticket cares about, so it would miss the real regression while
  penalising projects that never used gates. The sound signal is prior gate
  activity in the authenticated audit ledger. Deferred to a follow-up ticket
  (AIRA-56) rather than guessed at under this ticket's scope. A caller must not
  read `ready: true` as "gates proven".
- No `--name`, `--lane`, `--max-age-secs`, or `applies_to` flag family. Defaults
  above; hand-edit the file for anything exotic. Only `--dimension` is added,
  because without it a `check-dimension` gate is unrepresentable.
- Ratchet gates remain hand-authored.
- Fixture canaries (seed file trees) remain hand-authored; only mutation canaries
  are materializable from flags.
- The pre-existing absence of `E_GATE_*` codes from `store.ExitCodes` is not
  broadly fixed here; only the newly introduced `E_GATE_EXISTS` is registered.

## Tests

TDD, in the existing files and conventions (`writeGateFixture`, `testTraceGate`,
`testStore`, `gitRun` in `internal/store/gate_eval_test.go`).

AIRA-54:
1. Empty gate set (no `.aira/gates` at all) ⇒ verdict `unevaluated`, code
   `U_GATE_SET_EMPTY`, counters all 0, `Results` empty. **Explicitly asserts the
   verdict is not `pass`.**
2. Gates directory present but containing no definitions ⇒ same.
3. Exit-code test at the core boundary: `gate check` on an empty set ⇒ response
   `Code == "UNEVALUATED"` and `Exit == 3`. This is the assertion that proves the
   merge-gating hazard is closed.
4. Non-empty regression: one real gate, no result yet ⇒ still
   `unevaluated`/`U_GATE_NO_RESULT` per gate (unchanged), and a genuine
   all-passed set still folds to `pass` with `Passed == 1` — guarding the
   false-fail direction, so the fix cannot be "return unevaluated always".
5. Aggregate `Check`: gates dir present but empty ⇒ `gates` dimension
   `unevaluated`; no gates dir ⇒ dimension absent and verdict unaffected.

AIRA-53:
6. `add` with full command flags ⇒ file exists at `.aira/gates/<id>.json`, is
   parseable by `gate.ParseGate`, `ListGates` returns it, and the round-tripped
   definition matches field-for-field (checker, predicate, argv, cwd, env_allow,
   timeout, cap, parser).
7. `add` + mutation flags ⇒ canary file written, `ValidateCanary` passes,
   `canaryFor(def)` resolves it (proving lane binding and canary-id agreement),
   `CanaryStatus == "materialized"`.
8. `add` without mutation flags ⇒ `CanaryStatus == "absent"`; `RunGate` fails
   with `E_GATE_CANARY_INVALID`; `gate check` reports `unevaluated`, **not**
   `pass` — the composed-fix test.
9. `add` twice ⇒ second call `E_GATE_EXISTS`, and the on-disk bytes are byte-identical
   to after the first call (no partial overwrite).
10. Invalid input (no `--checker`; `--checker command` with no `--timeout-ms`;
    `--checker ratchet`; `--checker check-dimension` with no `--dimension`;
    bad slug id) ⇒ `E_GATE_INVALID` **and no file created** — asserted by
    statting the gate directory.
11. `set` on an existing gate ⇒ only the named field changes, everything else
    byte-preserved; `set` on an absent gate ⇒ `E_NOT_FOUND`.
12. End-to-end through `core.Do` for `add` and `check`, so the fix is proven at
    the face the reporter actually used, not only at the store method.

Added in review round 1:
13. `checkGatesReadOnly` all three directions: a genuinely passing proven gate ⇒
    aggregate `Dimensions["gates"] == "pass"` and no finding (the false-fail
    guard); a failing gate ⇒ `"fail"`; an unproven gate ⇒ `"unevaluated"`.
14. Aggregate `Check` with zero gates ⇒ `Dimensions["gates"] == "unevaluated"`,
    a `U_GATE_SET_EMPTY` unevaluated finding, and aggregate verdict
    `unevaluated` — asserting the pre-seeded `"pass"` at check.go:131 is
    actually overwritten.
15. `gate prove` on a registered-but-unproven gate ⇒ response `Code
    "UNEVALUATED"` and `Exit == 3`, not `OK`/0.
16. `add` then `rant --gate <id>` with no intervening reconcile ⇒ accepted,
    proving `IndexStatus == "refreshed"` is a real claim.
17. Flag-parity test: every name in `gateDefinitionOperationArgs` is extracted by
    `gateDefinitionInputFields` and declared in the verb `ArgSpec`, so a
    half-wired flag fails the suite.
18. Canary-first write ordering: with an intentionally invalid gate payload but a
    valid mutation seed, no discoverable gate exists afterwards
    (`ListGates` empty) — the partial-write guard.

## Mutation testing (mandatory, per adversarial-verification.md)

Each fix is reverted in a throwaway copy and the new tests must fail:
- `GateCheck` restored to `Verdict: gate.VerdictPass` on empty ⇒ tests 1, 2, 3
  must fail.
- `GateActionWithFields` restored to `_ = fields` ⇒ tests 6-12 must fail.
- `add` made to overwrite silently ⇒ test 9 must fail.
- `add` made to write before validating ⇒ test 10 must fail.
- Fix over-corrected to "always unevaluated" ⇒ test 4 must fail.

A test that cannot fail against the reintroduced bug proves nothing and will be
rewritten. Exact exit codes are recorded; no green claim from truncated output.

## Risks

- **Behavior change for any caller relying on empty-set `pass`.** Intended: that
  is the bug. AIRA has no external users or data ([[aira-not-live-no-compat]]),
  so no migration is owed.
- **Concurrent edit of a gate file during add/set.** Bounded by the path flock;
  the residual check-then-rename gap is the pre-existing documented limitation.
- **Merge conflict with the sibling AIRA-55 worktree** (canary mutation
  portability) in the same files. Resolve by rebase onto latest master; never
  force-push over their work.
- **Default invention.** Mitigated by tabulating every default above and refusing
  rather than guessing wherever a field has no safe default (ratchet, dimension,
  timeout).
- **Aggregate-`check` blast radius.** Making a gate-less project's `check` report
  `unevaluated` may break existing tests that assert `pass` while authoring no
  gates. Those assertions were asserting a fabricated pass, so the correct
  response is to update them — but the count is unknown until the suite runs and
  will be reported as evidence, not assumed. If the count is large enough to
  suggest the unnarrowed reading is wrong, that measurement gets escalated rather
  than silently worked around.

## Review record

### Round 1 — Codex/Sol (verdict: BLOCK)

Every claim was re-verified against source before adoption rather than taken on
trust; two of its cited line numbers pointed at the right defect via a slightly
different mechanism than stated.

| Sol finding | Adjudication | Response |
|---|---|---|
| `gate prove` returns unevaluated as OK/exit 0 | CONFIRMED (gate_eval.go:376, core.go:1935→567) | Fixed: verdict plumbed through `handlerData`; `Summary` corrected |
| Aggregate `Check` pre-seeds `gates: pass`; `hasGateContent` false for an empty dir; genuine passes downgraded to unevaluated | CONFIRMED (check.go:131; gate_eval.go:737) | v1 narrowing **withdrawn**; unnarrowed propagation adopted on the `traceability_test.go:312` precedent; pass-downgrade fixed as a prerequisite |
| `Ready` ignores an empty gate report | CONFIRMED (relation_ready.go:571) | Deferred with written rationale + follow-up ticket; `hasGateContent` rejected as an unsound predicate |
| `max_age_secs: 0` and empty evaluator version too permissive | CONFIRMED as a weak default | Adopted 604800 + `"1"` per in-repo precedent |
| `add` unusable by `rant --gate` without reconcile | CONFIRMED (rant.go:510) | Deferral withdrawn; projection refreshed with honest `IndexStatus` |
| Two-file write lacks collision/partial-write semantics | CONFIRMED as a gap | Canary-first ordering + `E_GATE_EXISTS` on either path + test 18 |
| `--dimension`/`canary_id` need three wiring sites | CONFIRMED (core.go:1851/2344/2370) | Adopted + parity test 17 |
| Test 4 passes against unfixed code | CONFIRMED, by design | It is the false-fail guard; kept and labelled as such |

Sol's claim that `Enabled` creates a "readiness-blocking gate" was **REFUTED** in
part: `Enabled` is consulted by nothing at all (only `AdvisoryInReady` is read),
so it cannot block readiness. The underlying observation — that `add` creates a
globally-applicable unproven gate that does affect `ready` — stands, and is
correct behavior: an unproven gate should hold readiness.
