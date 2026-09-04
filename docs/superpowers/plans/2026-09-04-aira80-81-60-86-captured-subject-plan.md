# Captured-subject type for gate evaluators — AIRA-80, AIRA-81, AIRA-60, AIRA-86 (partial)

Phase 1, Fix 3 of the backlog remediation plan
([`2026-09-04-backlog-remediation-plan.md`](2026-09-04-backlog-remediation-plan.md) §3.5).
Tier A: full two-loop. Not gated on Phase 0 (§3.5); its only gate is the §5
item 2 owner-decision default, which defaults to *proceed at Tier A*.

**Closes:** AIRA-80 (P1), AIRA-81 (P2), AIRA-60 (P2), and the
`gate_eval.go`/`gate_ratchet.go` portion of AIRA-86 (P2). AIRA-86's remaining
`check.go:130` seed site stays a standalone Phase 2 item. AIRA-78 is
explicitly **not** in scope (§3.5, §5 item 2 of the parent plan).

## 0. One sentence

Every gate evaluator today takes a *root path* and re-reads the tree for
itself; this replaces that with a *captured subject* — one read of the tracked
tree plus the digest of exactly those bytes — so a verdict can only ever be
bound to the bytes that produced it, and fixes the three defects that
divergence is currently causing.

## 1. The defects, source-verified against `8996999`

### AIRA-80 (P1) — the dimension lane digests one read and evaluates another

`EvaluateDimension` (`internal/store/gate_eval.go:62-117`):

```go
digest, err := subjectTreeDigest(root)          // read 1 (whole tracked tree)
...
snapshot, err := captureTraceSnapshot(root, nil) // reads 2 and 3 (Go + requirements subset)
```

`EvaluationRoot{Path: root, Digest: digest}` is returned alongside a predicate
derived from `snapshot`. Nothing ties the two together: any change to the tree
between the reads yields a verdict bound to a digest of a state that was never
evaluated.

Direction (both severities are real, and the plan does not pretend otherwise):

- The overwhelmingly likely outcome is a **self-healing false fail** — a digest
  over a torn read matches no coherent tree state, so `GateCheck`'s
  `subject_scope` comparison fails and the result is re-served as
  `U_GATE_NO_RESULT`, never as a pass. This is AIRA-80's own recorded
  assessment and is why it is P1, not P0.
- A **false pass** is nonetheless constructible and is what makes the fix
  worth Tier A: if the tree is broken at read 1, fixed before read 3, and
  restored afterwards, the audit ledger stores `verdict=pass` bound to the
  digest of the **broken** tree, and every later `GateCheck` over the broken
  tree serves that pass as current and trusted. The regression test below
  encodes exactly this ordering.

### AIRA-81 (P2) — canary re-materialization drops tracked-but-ignored files

`materializeTrackedSnapshot` (`gate_command.go:290-332`) copies the captured
tracked entries to a temp dir and then stages them with `git add -A`
(`:327`). `git add -A` **skips files matched by the copied `.gitignore` or the
user's `core.excludesFile`**. A file that is both tracked in the source and
ignored by it (entirely legal in git: `git add -f`) is therefore on disk in the
materialised tree but absent from its *index*.

The mutation-canary lane (`gate_eval.go:512-529`) then materialises a second
time from that tree — `runCommandChecker` → `materializeTrackedSnapshot(mutationRoot)`
→ `git ls-files --cached` — and the file is gone. The canary can fire because a
file **disappeared**, not because the declared mutation perturbed anything, and
`proof-of-fire` is what licenses a trusted pass. The proof is real; it attests
to the wrong perturbation.

This is the *opposite* direction from AIRA-55's documented sibling case (an
*injected* file matched by excludes is dropped and surfaces loudly as
`E_GATE_CANARY_DID_NOT_FIRE`, pinned by
`TestInjectFileCanaryIntoIgnoredPathDoesNotFire`). That documented behaviour is
preserved here deliberately — see §3.

### AIRA-60 (P2) — three path predicates, one of them not applied at declaration time

Three near-identical normalizing predicates exist:

| predicate | file | rejects |
|---|---|---|
| `safeFixturePath` | `store/gate_eval.go:540` | empty, absolute, `.`/`..`/`../…`, any `.git` or `..` segment |
| `safeSnapshotPath` | `store/gate_command.go:358` | as above plus `.`/empty segments |
| `safeMutationPath` | `gate/canary.go:189` | as above plus NUL |

`ValidateCanary` (`gate/canary.go:149-153`) uses **none** of them for the seed:
it applies a non-normalizing literal prefix test to `Seed.Files` keys and never
checks `Seed.Path`'s shape at all. It therefore accepts and digests
`.git/config`, `.git/hooks/pre-commit`, `sub/.git/config`, `a/../../etc/passwd`
and `./../x`, which evaluation then refuses. Fail-closed today; the safety
lives at two call sites inside `runCanary` rather than in the type's validator.

### AIRA-86 (partial) — seeded-pass shapes in the two gate files

Three sites in this fix's files seed a positive verdict and demote it later, so
a code path that never runs reports green:

- `gate_eval.go:100` — `report := CheckReport{Verdict: "pass", Dimensions: {"traceability": "pass"}}`
  and `:107` — `predicate := gate.PredicatePass`, both then demoted.
  **Honest scope note:** the seeded `report.Verdict`/`report.Dimensions` are
  never read in this function (the returned predicate is derived from
  `Findings`/`Unevaluated`), so that half is a *shape* fix with no observable
  behaviour change. `predicate := gate.PredicatePass` at `:107` is the live half.
- `gate_ratchet.go:80` — `comparison := RatchetComparison{Predicate: gate.PredicatePass, …}`,
  demoted to fail only if `len(newFailures) > 0`.
- `gate_eval.go:602` — `report := GateCheckReport{Verdict: gate.VerdictPass, …}`;
  `finishGateReport` (`:706`) only ever *demotes*, so a report that reaches it
  with zero results keeps the seeded pass.

## 2. Invariants this fix establishes

- **I1 — one read.** Every evaluator receives an already-captured subject. No
  evaluator re-reads the tree it is evaluating.
- **I2 — the digest is of the evaluated bytes.** `capturedSubject.digest` is
  computed from `capturedSubject.entries`; the entries are the only evaluation
  input. Binding a verdict to bytes that were not evaluated becomes
  unrepresentable rather than merely avoided.
- **I3 — materialisation preserves index membership.** Capturing a subject,
  materialising it, and capturing the materialised tree yields the same entry
  set and the same digest. (AIRA-81.)
- **I4 — one path predicate.** Exactly one normalizing relative-path predicate
  exists in the gate subsystem, applied at declaration time *and* evaluation
  time. (AIRA-60.)
- **I5 — no seeded green.** A verdict is `pass` only where something
  established it. (AIRA-86's two sites here.)
- **I6 — the tracked path set is a set.** A path appears at most once in a
  capture, so a capture is idempotent under materialisation. (Added on
  plan-review; see §3.7.)

## 3. Design

### 3.1 `capturedSubject` (`internal/store/gate_subject.go`)

```go
type capturedSubject struct {
    root    string
    entries []subjectEntry
    digest  string
}

func captureSubject(root string) (capturedSubject, error)
```

`captureSubject` is `stableSubjectEntries` (the existing double-read agreement
check) plus `digestSubjectEntries` over the very entries it returns. Every lane
gets the stable capture, not just the command lane: the dimension lane now
*evaluates* the captured bytes, so a torn read there is no longer merely a
digest-mismatch, and refusing it is the fail-closed direction.

`GateCheck` deliberately keeps the cheaper single-read `subjectTreeDigest`: it
computes a lookup key, not evidence, and a torn read there can only fail to
match a stored result (`U_GATE_NO_RESULT`), never fabricate one. This is
already the reasoning recorded on `stableSubjectEntries`.

### 3.2 Evaluator signatures

| before | after |
|---|---|
| `evaluateChecker(ctx, def, root string)` | `evaluateChecker(ctx, def, subject capturedSubject)` |
| `EvaluateDimension(root, dimension string)` | `evaluateDimension(subject, dimension string)` |
| `runCommandChecker(ctx, def, sourceRoot string)` | `runCommandChecker(ctx, def, subject capturedSubject)` |
| `evaluateRatchet(ctx, def, root string)` | `evaluateRatchet(ctx, def, subject capturedSubject)` |
| `materializeTrackedSnapshot(sourceRoot) (dir, digest, cleanup, err)` | `materializeSubject(subject) (dir, cleanup, err)` |
| `runCanary(ctx, c, def)` | `runCanary(ctx, c, def, subject capturedSubject)` |

`EvaluateDimension` is unexported in the same move: it has no caller outside
`internal/store` (verified), and an exported function taking an unexported type
is not a usable API. AIRA has no users and no compatibility obligation
([[aira-not-live-no-compat]]).

Call-site consequences:

- `RunGate` captures `s.root` **once** and hands the same subject to the
  subject evaluation and to `runCanary`. Today `RunGate` performs, for a
  command gate with a mutation canary, five separate tree reads
  (`evaluateChecker` → `materializeTrackedSnapshot` ×2 for the stable check,
  `runCanary` → `materializeTrackedSnapshot` ×2, plus `GateCheck`'s own
  digest); after this it performs two for the subject plus two for the mutated
  tree, which must be re-captured because the mutation changed it.
- `insights.go:574-584` already computes `subjectTreeDigest(s.root)` and then
  calls `evaluateRatchet(ctx, def, s.root)` per ratchet gate — one capture now
  serves both, and the per-gate re-read disappears.
- `GateAction("canary-run")` captures **only for `CanaryMutation`**, the one
  mode that materialises the caller's tree. A `synthetic-ratchet` canary
  evaluates entirely in memory and touches no tree today; capturing
  unconditionally would make it newly fail on a non-git root, which is a
  fail-closed *regression*, not a hardening. (Raised on plan-review.)
- `runFixtureCanary` (the M10a test seam) keeps its signature and passes a zero
  `capturedSubject`: fixture mode never reads the caller's subject.

**Capture failure in `RunGate` becomes a hard error, deliberately.** Today the
three lanes disagree: the dimension lane already hard-errors (`EvaluateDimension`
returns a *zero-value* predicate on digest failure, which `RunGate:171` does not
treat as `PredicateUnevaluated`), while the command and ratchet lanes record an
unevaluated result. That record is inert: it is keyed on an empty `subject`, so
`GateCheck`'s `latest[gateID+"\x00"+digest]` lookup can never find it and always
reports `U_GATE_NO_RESULT` regardless. One lineage (DeepSeek) argued for
standardising on the record instead, on the "cannot establish → report
unevaluated" rule. **Decision: standardise on the error**, because the rule is
about not fabricating a *verdict* — the error fabricates nothing, is louder, is
observably identical at `GateCheck`, and deletes a code path rather than adding
one. The dissent is recorded here rather than dropped.

### 3.3 AIRA-80 — the trace snapshot is derived from the capture

```go
func traceSnapshotFromSubject(subject capturedSubject) (traceSnapshot, error)
```

filters `subject.entries` through **the same** path predicate
`trackedTracePaths` uses, extracted as `isTracePath(path string) bool` so the
two cannot drift. `readTraceSnapshotFiles`' non-regular refusal is preserved:
a tracked symlink among the trace paths still yields `U_TRACE_UNSCANNED`
rather than being parsed as if it were a file.

`validateTraceRequirementsDirectory(subject.root)` is **kept** in the gate
lane. It is a directory readability probe, not a content read, so it cannot
produce a torn verdict, and dropping it would silently widen the lane in the
one case it catches (an unreadable `.aira/requirements` holding nothing
tracked).

**`captureTraceSnapshot` is deliberately NOT rebased onto the whole-tree
capture** — a considered deviation from AIRA-80's own "Direction" wording,
recorded rather than silent. It serves `check`'s traceability dimension
(`scanTraceabilityGraph`), which walks *every registered worktree* and has **no
persisted digest binding**, so a torn read there cannot mint or re-serve a
stale pass — it can only produce a one-shot report. (Corrected on plan-review
from a stronger and wrong "nothing to tear": `scanTraceabilityGraph` snapshots
worktrees *sequentially*, so its aggregate across worktrees is non-atomic
today. That is a pre-existing property of the check lane, unchanged by this fix
and out of its scope; it is named here rather than glossed.) Widening it to the whole tracked
tree would (a) read every byte of every worktree on every `aira check`, and (b)
import `captureSubjectEntries`' gitlink refusal (AIRA-72's accepted regression,
tracked by AIRA-79) into a lane that does not need it — making `aira check`
fail closed on any repository with a submodule. The shared piece that actually
prevents drift is the path predicate, and that *is* unified.

### 3.4 AIRA-81 — force-stage the materialised set

`materializeSubject` stages with `git add -A -f` instead of `git add -A`.

The materialised directory contains exactly the captured entries and nothing
else (we wrote every one of them, and `git init` adds only `.git`), so `-f`
makes the resulting index exactly the captured set — including an entry the
copied `.gitignore` or the user's `core.excludesFile` matches. A re-capture of
the materialised tree is then faithful by construction (I3).

Two things this deliberately does **not** change:

- The **fixture** canary's `git add -A` on its own seeded temp tree
  (`gate_eval.go:507`) — a different tree, a different set, and out of
  AIRA-81's scope.
- The **post-mutation** re-stage `git add -A` (`gate_eval.go:524`). The
  mutation's *own* new file is not in the captured set, so an inject-file
  target matched by the subject's excludes is still dropped and still surfaces
  as the loud `E_GATE_CANARY_DID_NOT_FIRE` that AIRA-55 documented and
  `TestInjectFileCanaryIntoIgnoredPathDoesNotFire` pins. Only files that were
  tracked in the *source* are rescued, which is exactly AIRA-81's claim.

### 3.5 AIRA-60 — one exported predicate

`gate.SafeRelativePath(path string) bool` in `internal/gate/canary.go` is the
**conjunction of every rejection** the three predicates make — not a
disjunction of their acceptances, which would loosen it (flagged by both
review lineages). It rejects: empty, leading `/`, `filepath.IsAbs`, NUL, and
after `Clean`: `.`, `..`, a `../` prefix, and any `.git`/`..`/`.`/empty
segment. `safeFixturePath`, `safeSnapshotPath` and `safeMutationPath` are
deleted in favour of it; `safeMutationPackagePath` becomes
`path == "." || SafeRelativePath(path)`.

Behaviour delta is refusal-only, and is **exactly one rejection: NUL**
(corrected on plan-review — a draft claimed empty-string rejection was also
new; all three predicates already reject `""`, two explicitly and
`safeSnapshotPath` via `Clean("") == "."`). Every accepted path stays accepted:
the `.`/empty-segment differences are unreachable after `filepath.Clean`. NUL
is unreachable from `git ls-files -z` (it is the separator) but reachable from
a canary declaration, which is the call site AIRA-60 is about.

`ValidateCanary` applies it to every `Seed.Files` key **and** to `Seed.Path`
when set, moving the refusal to declaration time where a gate author sees it.

### 3.6 AIRA-86 — the two seed sites in these files

- `evaluateDimension`: scratch report seeded `unevaluated`; the returned
  predicate becomes an explicit three-arm switch whose `pass` arm is the
  default-last case rather than the seeded first.
- `compareNoNewFailures`: seeded `PredicateUnevaluated`, raised to `pass` only
  in the else-arm that establishes it.
- `GateCheck`/`finishGateReport`: the report is seeded `unevaluated` and
  `finishGateReport` computes the verdict positively. **Strengthened on
  plan-review — both lineages independently found the same residual hole:** the
  counting loop's `switch` had no default, so a `GateCheckResult.Verdict` that
  is neither `pass`/`fail`/`unevaluated` — and it is a raw string read straight
  out of the audit ledger (`gate_eval.go:647`), so a missing field yields `""` —
  incremented nothing and left a `[pass, ""]` report reporting **pass**. The
  counting loop therefore gets an explicit `default: report.Unevaluated++`, and
  the rollup is `Failed>0 → fail`; `Unevaluated>0 || Passed==0 → unevaluated`;
  else `pass`. An unknown verdict string is now unevaluated by construction,
  which also makes `Passed == len(Results)` hold whenever the verdict is pass.

### 3.7 Unmerged index — added on plan-review (Sol [P1])

`git ls-files --cached` emits an unmerged path **once per stage**. Verified
empirically (`~/tmp/aira80-probe/probe.sh`): a conflicted `f.txt` is listed
three times. `trackedSnapshotPaths` passes all three through, so
`captureSubjectEntries` reads and digests the file three times, while
`git add -A -f` in the materialised tree collapses them to one stage-0 entry —
breaking I3 round-trip idempotence precisely when a repository is mid-conflict.

`trackedSnapshotPaths` therefore **refuses a duplicate path**
(`U_GATE_EVIDENCE_UNAVAILABLE`): a conflicted index has no single coherent
content for that path, so there is no well-defined subject to bind a verdict
to. Fail-closed, loud, and it removes the one construction under which
capture → materialise → capture is not the identity.

Out of scope, named not glossed: `trackedTracePaths` (the *check* lane, §3.3)
has the same duplicate behaviour. It has no digest to bind and its duplicate
edges collapse in the `covers`/`verifies` maps, so it is left alone.

## 4. Tests

TDD: every test below is written and observed **failing** (or
non-compiling-then-failing, where a signature moved) before the corresponding
change.

**The shared property test the parent plan asks for** —
`TestStoredPassIsInvalidatedByAnyTrackedMutation`: for every gate kind that can
reach a trusted stored pass (manual, dimension, command, ratchet) × every
mutation shape, mutating the subject must stop `GateCheck` serving the stored
pass. One test, four kinds, four mutation shapes — and it covers the seed-site
fix too, because an `unevaluated` seed can never masquerade as an invalidated
pass.

The four shapes are: content of an ordinary tracked file; content of a
**tracked-but-gitignored** file; a mode bit (`chmod +x`); and a tracked regular
file **replaced by** a symlink. **Corrected on plan-review (Sol [P1]):** a
draft used "retarget an existing tracked symlink", which is not constructible
for the command kind — `materializeSubject` refuses a non-regular entry
(`gate_command.go:305`), so a command gate over a tree that already contains a
tracked symlink can never reach the trusted pass the shape needs to invalidate.
Replacing a regular file with a symlink keeps the pre-pass tree all-regular, is
constructible for all four kinds, and is the stronger assertion anyway: it
exercises the `subjectEntryKind` byte that stops a symlink colliding with a
regular file holding its target's bytes.

Per-defect:

| # | test | fails before because |
|---|---|---|
| 1 | `TestDimensionEvaluationReadsTheCapturedBytesNotTheDisk` | the evaluator re-read the disk; with a captured subject that disagrees with disk, the verdict must follow the capture and `Root.Digest` must equal `subject.digest` |
| 2 | `TestTornSubjectReadCannotMintAPassBoundToAnotherTree` | the AIRA-80 false-pass ordering (broken → fixed during evaluation → broken again): master stores `pass` under the broken tree's digest and re-serves it; after the fix the torn read is refused |
| 3 | `TestCaptureSubjectRefusesATornRead` | `stableSubjectEntries`' agreement check is currently **untested**; the dimension lane's fail-closed behaviour now depends on it |
| 4 | `TestMaterializationPreservesTrackedButIgnoredFiles` | round-trip capture → materialise → capture changes the digest on master (I3) |
| 5 | `TestIgnoredTrackedFileDropDoesNotMintProofOfFire` | end-to-end: a mutation that perturbs nothing the checker looks at still fires the canary on master and mints a **trusted pass**; after the fix it is the honest `E_GATE_CANARY_DID_NOT_FIRE` |
| 6 | `TestInjectFileCanaryIntoIgnoredPathDoesNotFire` (existing) | must keep passing — the AIRA-55 boundary §3.4 preserves |
| 7 | `TestValidateCanaryRefusesUnsafeSeedPathsAtDeclarationTime` | AIRA-60's five listed vectors, in `Seed.Files` and in `Seed.Path` |
| 8 | `TestSafeRelativePathMatchesTheSupersededPredicates` | the unification is refusal-only: a table of paths where all three superseded predicates agreed must be unchanged |
| 9 | `TestGateCheckWithNoPassingResultIsNotPass` | `finishGateReport`'s seeded pass survives a results-empty report on master |
| 10 | `TestRatchetComparatorStillReachesPassWhenNothingRegressed` | AIRA-86's mandatory condition: the `pass` path must still reach `pass` after the seed flips |
| 11 | `TestGateCheckUnknownStoredVerdictIsNotPass` | §3.6's residual hole: `[pass, ""]` rolls up to pass on master |
| 12 | `TestCaptureRefusesAnUnmergedIndex` | §3.7: a conflicted index digests one path three times and is not idempotent under materialisation |

Mutation testing (adversarial pass): each of the five source changes is
reverted in isolation and the suite re-run, to prove the tests actually
discriminate rather than passing against either implementation.

## 5. Risks and deferrals

- **Cost.** `RunGate` now performs a stable (double) capture where the
  dimension and ratchet lanes previously did a single read. Net across a whole
  `RunGate` this is fewer reads, not more (§3.2), and `GateCheck` — the hot,
  read-only path — is untouched. `BenchmarkSubjectTreeDigest` remains the
  committed reproduction for the underlying cost claim.
- **`git add -A -f` scope.** Bounded by the fact that the temp tree contains
  only what we wrote. Asserted by test 4 rather than argued.
- **Deferred, recorded, not silent:** AIRA-86's `check.go:130` seed site (14
  dimensions, its own Phase 2 item with a mandatory per-dimension test);
  AIRA-78's ratchet gate kind (§5 item 2 of the parent plan — an owner
  keep-vs-delete fork, not this fix's call); AIRA-79's gitlink digesting;
  `captureTraceSnapshot`'s check lane (§3.3).
- **Production footprint.** The entire gate subsystem has zero rows in
  `~/.local/state/aira/state.db` (parent plan §5 item 2). This fix is justified
  on honesty-defect merit and is net-negative in lines; the owner's
  defer-or-delete answer, if it arrives, overrides.

## 6. Plan-review record

Two orthogonal lineages reviewed this plan against the real source. **Fable
(the project's usual Claude plan gate) was not available as a tool in this
session** — recorded as a gap, not silently skipped.

- **Codex / GPT-5.6-Sol** (repo read-access): **GATE-FAIL**, two P1s. Both
  adopted: the unmerged-index non-idempotence (§3.7, empirically confirmed) and
  the non-constructible symlink cell in the property matrix (§4). It confirmed
  the AIRA-80 false-pass ordering is constructible, confirmed `git add -A -f`
  is sufficient and safe for stage-0 inputs, confirmed the post-mutation
  `add -A` must stay unforced, confirmed the path unification is refusal-only,
  and confirmed `finishGateReport` breaks no existing test. It corrected the
  empty-string claim (§3.5) and the "nothing to tear" claim (§3.3), and raised
  the `[pass, unknown-verdict]` rollup hole (§3.6).
- **DeepSeek-V4-pro** (inline context): independently raised the same
  unknown-verdict hole and insisted `SafeRelativePath` be a conjunction of
  rejections, both adopted (§3.5, §3.6). Its `[P1]` preference for an
  unevaluated *record* over a hard error on capture failure was considered and
  **declined with reason** (§3.2). Its `[P0]` — "GateCheck ignores the
  `subjectTreeDigest` error" — was an artefact of this plan's own abridged
  source excerpt, which wrote `subjectDigest,_ :=`; the real code at
  `gate_eval.go:632-635` returns the error. Recorded because a review finding
  that turns out to be wrong is still evidence about the brief.
