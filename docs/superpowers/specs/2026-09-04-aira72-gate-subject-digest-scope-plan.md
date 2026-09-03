# AIRA-72 — the gate subject digest must cover the whole tracked subject

Status: v3 — as-built record. v1 BLOCKed by Codex/Sol; v2 gated PASS-WITH-CHANGES by Fable; v3 applies the cut list and records the results.
Ticket: AIRA-72 (P0, bug, labels: dogfood, gate, honesty)
Branch: `aira72-gate-digest-scope` off `9a65d47`

## 1. The defect

`digestEvaluationRoot` (`internal/store/gate_eval.go:63-79`) computes a gate
result's `subject` / `subject_scope` — the digest binding a stored verdict to
the state of the thing it claims to have proven. It hashes exactly the paths
returned by `trackedTracePaths` (`internal/store/traceability.go:88-112`):
tracked `*.go` (non-`vendor/`) plus `.aira/requirements/*.md`. Nothing else.

`GateCheck` keys the stored result by `gateID + "\x00" + currentSubjectDigest`
(`gate_eval.go:659`) and, for a pass, additionally requires
`record.Fields["subject_scope"] == currentSubjectDigest` (line 672). Both are
satisfied forever on a repo whose gated logic is not Go: the digest is constant
with respect to the code the gate is about. A stored trusted `pass` is
re-served indefinitely. `ProofPolicy.MaxAgeSecs` is the only remaining bound and
defaults to `0` — no expiry (`gate_eval.go:706` guards on `> 0`).

Measured on AIRA's own tree: 518 tracked files / 6,850,658 bytes, of which the
digest covers 4,350,099 bytes (63.5%). **190 tracked files (36.7%) are
invisible**: 149 `.md`, 14 `.py`, 5 `.conf`, 4 `.sh`, 3 `.yml`, `Makefile`,
`go.mod`, `go.sum`, the `pre-commit`/`pre-push` hooks, `.aira/config`. This repo
has no `.aira/requirements/` directory at all, so here the digest is *purely*
tracked non-vendor `.go`.

This is the fourth instance tonight of the same honesty class (AIRA-53/54's
empty-set fake pass, `checkGatesReadOnly` downgrading genuine passes,
`gate prove` unwrapping to a fabricated OK). It is a **false pass** — the worst
direction — live on any non-Go-dominant project.

There is already a latent, unasserted demonstration in the suite:
`ratchetCommitCurrent` (`gate_ratchet_test.go:209`) writes and `git add`s a
tracked `current.txt` into a ratchet-gated tree, and the subject digest does not
move. The tests pass only because none of them assert on it (§5.1).

Secondary instance of the same mislabelling: `insights.go:582` publishes this
value as `tracked_worktree_digest`, a claim it does not support.

## 2. Archaeology — why the scope is narrow

**`trackedTracePaths` is a `go/parser` input selector, and for its own consumer
the narrowness is correct and must not change.** Introduced by `87565d0`
("feat(M9c): covers/verifies traceability graph check", 2026-08-11 14:37) and
never modified since. `parseTraceabilitySnapshot` (`traceability.go:157-200`)
runs `go/parser` over every non-requirement file in the set — a non-Go file
there is a hard `U_TRACE_UNSCANNED` parse error, not a skip. The in-code
rationale (`traceability.go:41-43`) is about parsing: *"Parsing the Go syntax
first is intentional: a string containing `covers: AR-1` is data, not an
annotation."* The M9 design doc (`2026-08-09-aira-m9-traceability-design.md:253-263`)
frames the whole selector as annotation discovery. `vendor/` is explicit noise
exclusion.

**No cost concern exists anywhere.** No commit message, design doc, plan, or
comment mentions index size, `git ls-files` cost, or digest cost. The narrowness
is answer (a) — traceability semantics — with a dash of (c), vendor noise. With
respect to *digesting*, it is simply unexplained: it was designed as a parser
input selector and never reconsidered as a subject-identity selector.

**`digestEvaluationRoot` reused it silently.** Introduced by `554c344`
("feat(M10a): gate honesty engine", 2026-08-11 20:45) — six hours after the
traceability filter, same author, born already calling `trackedTracePaths` on
its first line. The M10a commit message and plan discuss the evaluation root
only as an *isolation* property ("the checker takes an explicit immutable
EvaluationRoot and MUST read only from it"), never as a *content-scope*
property. None of the M10a adversarial review findings (6 plan + 8 build, 2
Opus) touched digest scope. The M10a requirement was "a target-subject scope
distinct from the fixture scope", which *any* differing digest satisfies; the M9
function was the nearest existing "list the tracked files" helper.

**The design specs contradict the implementation.** No doc anywhere defines the
subject digest as a filtered subset:

- `2026-08-11-aira-m10-gates-design.md:124-133` — *"Subject: project, ticket if
  applicable, **commit/tree digest**, and the gate definition digest."*
- `:538-540`, invariant 9 — *"**Subject identity is exact.** A result for
  another commit, worktree, definition digest, or incomparable lane does not
  satisfy the current gate."*
- `2026-08-11-aira-m10b-command-gates-design.md:118` — *"**The tree digest
  remains the identity of the evaluated subject**."*
- `2026-08-07-aira-design.md:98` — the attestation record names `commit` as a
  first-class binding field. The implementation substitutes a filtered content
  digest for it.

So this fix **restores spec conformance**; it does not invent policy.

**The codebase already contains the right primitive.** `b5cc38f`
("feat(M10b): command-backed gates", 2026-08-12) gave command gates their own
path set: `trackedSnapshotPaths` (`gate_command.go:335-353`) — `git ls-files -z
--cached`, **every** tracked path, filtered only by `safeSnapshotPath`, which is
a *refusal* (absolute/`..`/`.git`) rather than a content policy — and
`digestTrackedRoot` over it. `GateCheck` carries the resulting asymmetry as an
explicit special case (`gate_eval.go:652-658`): command gates get the whole-tree
digest, every other checker gets the Go-only one. Later, `cd02dda` (AIRA-55,
"canary-proven command gates outside Go") fixed Go-only-ness *on the command
path only* — the path that was already correct — and never looked at the
dimension/manual/ratchet path. That is exactly the gap AIRA-72 names.

## 3. Direction chosen

**Make the gate subject digest the whole tracked tree for every checker, on a
single dedicated primitive, and delete the command-checker special case.**

Built as:

1. `subjectTreeDigest(root)` in `internal/store/gate_subject.go` — the sole
   producer of every gate subject digest.
2. `digestEvaluationRoot` and `digestTrackedRoot` both deleted; every call site
   now names `subjectTreeDigest`. One primitive, one name, so a second scope
   cannot be reintroduced by accident.
3. The `Lane.Checker == CheckerCommand` branch in `GateCheck` deleted.
4. `trackedTracePaths` untouched.

Net effect is **fewer** moving parts than before: two digest implementations and
one per-gate branch replaced by one function.

### 3.1 What the digest covers

Per tracked path, in path-sorted order, length-prefixed and kind-tagged:

```
for each entry (sorted by path):
    u64(len(path)) || path || u8(kind) || u64(len(payload)) || payload
```

- **Length prefixing** removes a real framing ambiguity. The old
  `path NUL data NUL` framing collided: one file `{"a": "b\0c\0d"}` and two
  files `{"a": "b", "c": "d"}` both serialise to `a\0b\0c\0d\0`. Git paths
  cannot contain NUL but content can, so both trees are constructible and shared
  a digest — a stale pass servable against a genuinely different tree.
- **`kind`** distinguishes a regular file (payload = content), an **executable**
  regular file, and a **symlink** (payload = link target). It mirrors git's own
  tree model (100644 / 100755 / 120000). The executable bit matters: without it
  `chmod -x run.sh` breaks a shell command gate while `GateCheck` re-serves the
  stored pass — AIRA-72 in miniature, aimed at exactly the non-Go subjects this
  fix serves.
- A tracked **gitlink** (mode `160000`) or any other non-regular, non-symlink
  entry **fails closed**. See §7.4: this is an accepted regression with a ticket.

**Correction to the review record.** Codex/Sol raised the framing collision but
supplied a vector that does not actually collide — `{"a": "b\0c"}` vs
`{"a": "b", "c": ""}` differ, because the empty file still emits its terminator
(6 bytes vs 7). The Fable gate caught this. Had the weaker vector been used the
regression test would have passed against the very framing it exists to reject.
Both near-miss pairs are now asserted in the test alongside the real one, so the
strong vector cannot be silently downgraded later.

**Rejected: `git write-tree` as the subject identity.** It would encode path,
mode, symlink and gitlink natively and need no framing of ours, and it matches
the design docs' "commit/tree digest" language most literally. Rejected because
it reports the *index*, and the index honours `assume-unchanged` and
`skip-worktree`: under either bit a real working-tree edit is invisible to git's
tree hash. For a digest whose entire job is to notice that the subject changed,
reading the working tree directly is the more faithful witness — adopting git's
tree id would reintroduce the silent-staleness class this ticket closes.

### 3.2 Command-lane agreement, by construction

`runCommandChecker` used to digest the **materialised snapshot** while
`GateCheck` digested the **source root**. Those are not the same tracked set:
`materializeTrackedSnapshot` copies the source's tracked files into a temp dir
and re-indexes with `git add -A`, which drops any file matched by the copied
`.gitignore` or the user's `core.excludesFile`. A file both tracked and ignored
in the source (legal in git) was present in the check-time digest and absent
from the proof-time one, permanently invalidating a genuine pass — a live
**false fail**.

`materializeTrackedSnapshot` now returns the digest of the very entries it
captured and copied. Proof time and check time therefore run the same pure
function over the same entries, and invariant I3 holds by construction.

Note the alternative considered and rejected: re-reading `sourceRoot` after
materialising. That fixes the tracked-but-ignored divergence but opens a window
in which the tree changes between the bytes that ran and the bytes that were
bound — a small **false pass**, which is the worse direction. The Fable gate
caught this in the v2 plan before it was written.

### 3.3 One capture, two consumers

`captureSubjectEntries` is the single tracked-tree reader, shared by the digest
and by the materialiser (`readTrackedSnapshot` and `trackedSnapshotFile` are
deleted). The materialiser wraps it in `stableSubjectEntries`, which keeps the
double-read agreement check it already had, and refuses any entry it cannot
reproduce faithfully — it builds a real working tree, so it must not dereference
or silently drop a link.

The digest deliberately does **not** double-read. It buys no honesty: a digest
over a torn read corresponds to no coherent tree state, so it can only fail to
match a stored result, never fabricate a pass. Paying 2x I/O on the
`check`/`ready`/insights path for a self-healing false fail is not worth it.
(v2 proposed the double-read on Sol's P0-4; the Fable gate cut it with this
reasoning, and the residual is filed as AIRA-80.)

### 3.4 Rejected: narrow the digest by `AppliesTo.Paths`

`gate.AppliesTo` carries `Paths []string` (`internal/gate/gate.go:57-64`).
Rejected:

1. It is a **subject selector** — which tickets/lifecycle steps/paths a gate
   applies to — not a content scope. Archaeology confirms it is consumed
   *nowhere*: `ValidateGate` checks it, `CanonicalSelectorFields` folds it into a
   string and **has zero callers**, and there is no selector-evaluation code in
   the repository at all.
2. Every gate AIRA creates today is `AppliesTo{All: true}`
   (`gate_write.go:236`), which carries no paths, so the narrowing would not
   apply to the gates most at risk.
3. It would create a **new false-pass vector**, the opposite of the ticket. A
   gate declaring `paths: ["src/api/"]` would stop invalidating when a
   dependency under `src/lib/` changed. An author-supplied narrowing of an
   honesty digest is an author-supplied licence to go stale.

### 3.5 Rejected: exclude `.aira/gates/**` from the subject digest

Tempting — a gate's definition and canary are already bound precisely by
`definition_digest` / `declaration_digest`, so including them is redundant and
blurs "the subject moved" (`U_GATE_NO_RESULT`) against "the gate's own binding
moved" (`U_GATE_PROOF_STALE`). Confirmed not self-referential (no digest is
stored inside those files; audit writes live outside the tracked tree), so the
objection is purely cost/benefit.

Rejected because an allowlist over the subject is the mechanism that produced
AIRA-72, and the benefit is cosmetic: both outcomes are `unevaluated` +
`suspect` and both correctly refuse the pass. Command gates have included
`.aira/gates/**` in their subject digest since `b5cc38f` without incident.

The real accepted cost is **cross-gate invalidation**: editing one tracked gate
file invalidates every other gate's stored pass. Accepted, and pinned by
`TestTrackedGateFileEditInvalidatesStoredPass`.

### 3.6 Rejected: refuse proof when the canary tree equals the subject

v2 proposed refusing to mint proof-of-fire when `canary_tree_digest ==
subject_scope`. Cut by the Fable gate on code-grounded reasoning: a canary tree
byte-identical to the subject cannot make a deterministic checker produce a
different verdict, so it already lands on `E_GATE_CANARY_DID_NOT_FIRE`, which is
a hard fail. The guard would have been unreachable machinery.

## 4. Invariants

- **I1 (no false pass).** For any tracked entry, a change to its content, its
  link target, its executable bit, or its presence changes the subject digest,
  so a pass keyed to the old digest is no longer served. Holds for every
  `Lane.Checker` and every `Kind`.
- **I2 (no fabricated evidence).** Every failure to establish the digest yields
  an error, never a zero or partial digest.
- **I3 (producer/consumer agreement).** The digest written at proof time and the
  digest recomputed at check time come from the same pure function over the same
  captured entries, for every checker. §3.2 is what makes this true.
- **I4 (traceability untouched).** `trackedTracePaths` keeps its exact previous
  behaviour. No non-Go file reaches `go/parser`.
- **I5 (unambiguous framing).** No two distinct tracked trees share a digest by
  framing ambiguity: entries are length-prefixed and kind-tagged.

## 5. Tests

All ten new tests were written first, seen to fail, and then mutation-tested:
the fix was reverted in a throwaway copy under `~/tmp/` and each test confirmed
to fail for its own reason. Results in §6.

### 5.1 Non-Go subject invalidation, on two producers

The checker matters. A **command** gate is a negative control, not the headline:
it already digested the whole tree on both sides, so it passes on `master` and
proves nothing. The gates that exercise the defect are:

- `TestGateSubjectDigestInvalidatesOnNonGoChange` — **manual attestation**, the
  scenario the ticket names (a hand-authored pass with `MaxAgeSecs: 0`, no
  expiry). Subject: `run.sh`, `lib/handler.py`, `Makefile`, `pyproject.toml`,
  `.aira/requirements/AR-1.md`, and **no Go file at all**. Binds through
  `AttestGate`. Four subcases, one per mutated non-Go file.
- `TestDimensionGateSubjectDigestInvalidatesOnNonGoChange` — **check-dimension**,
  binding through `RunGate` → `evaluateChecker` → `EvaluateDimension`, a
  different call site of the same primitive. It reaches a genuine trusted pass on
  a Go-free subject because the requirement node is tracked (so the scan is not
  `U_TRACE_EMPTY`) and the fixture canary still fires on its own seeded tree.

Both assert the gate really passed first, so "the pass is gone" cannot be
satisfied by a gate that never passed.

The **ratchet** producer calls the identical primitive and is covered by
construction rather than by its own end-to-end test; its independent evidence
defect is AIRA-78 (§7.3).

### 5.2 Digest primitive

`TestSubjectDigestCoversEveryTrackedFile` (table over `.py`, `.sh`, `.md`,
`Makefile`, `.toml`, `.yml`, `.sql`, nested, `.go`),
`TestSubjectDigestFramingRejectsNulCollision` (real vector plus both near-miss
pairs), `TestSubjectDigestFramingBeatsSupersededNulFraming` (keeps the old
framing as an executable counterexample and asserts it *does* collide, so the
vector cannot rot), `TestSubjectDigestBindsExecutableBit`,
`TestSubjectDigestBindsSymlinkTarget`, `TestSubjectDigestGitlinkFailsClosed`,
`TestSubjectDigestIgnoresUntrackedFiles` (pins §7.2),
`TestTrackedGateFileEditInvalidatesStoredPass` (pins §3.5).

### 5.3 Existing tests: variable isolated, assertion never relaxed

Two tests began reporting `U_GATE_NO_RESULT` instead of `U_GATE_PROOF_STALE`,
because each rewrites a *tracked* gate/canary file, which under a whole-tree
digest also moves the subject. Both are honest refusals, but these tests exist
to prove the declaration/definition binding.

- `TestSeedDigestInvalidatesOnDemandProof` (`gate_eval_test.go`)
- `TestGateCheckRejectsPassAfterDefinitionBindingChanges` (`gate_eval_test.go`)
  — flagged by the Fable gate; the v2 plan had missed it.

Fix in both: order the fixture write **after** `git add`, so the gate files stay
untracked and the subject digest is constant across the edit. Each test then
proves exactly what it was written to prove and **keeps its strict assertion**.
No assertion in either test is weaker than before. The behaviour isolated out of
them is covered by the new `TestTrackedGateFileEditInvalidatesStoredPass`, which
asserts on the subject digest directly — a verdict-only assertion there would
still have passed against the narrow scope, via `definition_digest`, and proved
nothing.

## 6. Results

Full suite `./internal/... ./cmd/...`: **exit 0**.

Mutation testing, fix reverted in `~/tmp/aira72-mutation/repo`:

| Mutation | Tests killed |
|---|---|
| scope narrowed back to `trackedTracePaths` | manual non-Go (4 subcases), dimension non-Go, covers-every-tracked-file, tracked-gate-file |
| framing reverted to `path NUL data NUL` | both framing tests, executable bit, symlink |
| executable-bit distinction removed only | executable bit (symlink survives — independent) |
| gitlink branch skips instead of refusing | gitlink fails-closed |

Every new test failed against its own defect and passes against the fix.

### Performance

Measured with the committed `BenchmarkSubjectTreeDigest`:

| Tree | Per digest |
|---|---|
| 500 files / 7 MB (AIRA's own scale: 518 files / 6.85 MB) | **10.2 ms** |
| 5000 files / 70 MB (10x) | **91.3 ms** |

The widening is ~1.57x on this repo (4.35 MB → 6.85 MB). The change is a net
**improvement** for command gates: `GateCheck` previously rehashed the entire
tree once per command gate inside the per-gate loop; it is now computed once.

No caching ticket is filed. The measurement does not justify one, and inventing
speculative machinery would contradict the project's simplicity rule. If a real
repository makes this hurt, cache keyed on git index state — never a narrower
digest, because a narrower digest is the bug.

## 7. Findings deferred, each with a filed ticket

Real, confirmed, deliberately not fixed here — recorded rather than omitted.

### 7.1 Untracked files are outside the subject (accepted boundary, no ticket)

Both digests read `git ls-files --cached`: a working-tree file never `git add`ed
is invisible. Accepted and deliberate — the subject of a gate is the tracked
tree, and every checker evaluates only tracked content, so the digest describes
exactly what was evaluated. Identical to the previous behaviour for Go files.
Pinned by `TestSubjectDigestIgnoresUntrackedFiles`.

### 7.2 Dimension gate digests one read and evaluates another — **AIRA-80** (P1)

`EvaluateDimension` digests the root and then separately captures the content it
evaluates. A verdict can be bound to a state that was never evaluated. Assessed
P1 rather than P0 because the torn outcome is a self-healing false fail, not a
fabricated pass. The command lane no longer has this defect (§3.2).

### 7.3 Ratchet evidence is selected by HEAD — **AIRA-78** (P0)

`evaluateRatchet` binds to working-tree bytes but selects test reports by
`git HEAD`, so a dirty tree can mint a verdict from reports describing different
code. This fix strictly improves it — a dirty tree now yields a different subject
digest, so a stored pass is no longer re-served — but does not close it, because
the freshly computed verdict still consumes HEAD-keyed evidence. It is a fake
pass, hence P0, and it is why §9's yield claim is scoped.

### 7.4 Tracked submodule now fails closed — **AIRA-79** (P2)

A repository with a tracked gitlink evaluated before (gitlinks were not `.go`
files, so the old digest skipped them) and now reports
`U_GATE_EVIDENCE_UNAVAILABLE`. A false fail — the safe direction, and loud rather
than silent, since skipping the entry would make the digest claim coverage it
does not have. AIRA-79 will digest the pinned submodule commit.

### 7.5 Canary re-materialization drops tracked-but-ignored files — **AIRA-81** (P2)

Pre-existing, found by the plan gate. The mutation path materialises twice; the
intermediate `git add -A` drops a tracked-but-ignored file, so a canary can fire
because a file disappeared rather than because the mutation perturbed anything —
minting proof-of-fire for the wrong perturbation. §3.2 removed the digest half of
this divergence; AIRA-81 is the remaining execution half.

## 8. Behaviour notes

- With a whole-tree digest, any tracked edit between `gate review` →
  attest-fail → attest-pass yields `U_GATE_UNPROVEN`. Correct — the attestation
  chain is bound to a subject that moved — and worth knowing before it surprises
  someone mid-review.
- Previously stored gate results key to a digest nobody recomputes and now read
  as `U_GATE_NO_RESULT`. Fail-closed, the correct direction, and per the standing
  no-compat rule no migration is provided.

## 9. Yield

One P0 false-pass class closed for every non-Go gate subject on the manual,
dimension and ratchet checkers — with the explicit exception of AIRA-78, where a
ratchet on a dirty tree can still *mint* (not re-serve) a pass from HEAD-keyed
evidence. Two narrower false-pass vectors closed inside the digest itself
(framing collision, executable bit) and one live false fail (tracked-but-ignored
files under command gates). Two digest implementations, one tracked-tree reader
and one per-gate branch removed. One mislabelled insights field
(`tracked_worktree_digest`) made true. Four confirmed adjacent defects filed
rather than buried.
