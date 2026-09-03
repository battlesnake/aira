---
{"schema":1,"id":"AIRA-72","project":"aira","title":"Gate attestation digest scope is Go-only — a stored pass never invalidates on non-Go changes","status":"planned","kind":"bug","severity":"P0","assignee":null,"milestone":null,"labels":["dogfood","gate","honesty"],"hold":false,"relations":[]}
---
## Symptom (found during the whole-project simplification review, PR #12, verified independently against source)

Manual/ratchet/dimension gates bind their stored attestation result to `digestEvaluationRoot(root)` (`internal/store/gate_eval.go:63-79`) — a SHA256 over every path returned by `trackedTracePaths` (`internal/store/traceability.go:88-112`). That function returns ONLY tracked `*.go` files (excluding `vendor/`) and `.aira/requirements/*.md` files — nothing else. Every other tracked file — Python, JS/TS, Rust, shell, Makefiles, config, documentation outside `.aira/requirements/` — is invisible to this digest.

## Why this matters

A gate's stored `pass` is meant to attest "the subject was in this state when proven." On a repo where the actual logic under test isn't Go (fastest-ee, for instance — a real Python project using this system in production right now), the digest never changes when the actual code changes. A stored `pass` from a manual attestation or a ratchet ships and then simply never invalidates — it reads as current forever, bounded only by `MaxAgeSecs`, which defaults to 0 (no expiry) for hand-authored gates. This is the same fabricated-green class tonight already found and fixed three times over (AIRA-53/54, `checkGatesReadOnly`, `gate prove`) — a different specific mechanism, not caught by any of those fixes, and live on any non-Go-dominant project using gates today.

## Suggested direction

`digestEvaluationRoot` needs to cover the actual tracked tree relevant to what's being gated, not a hardcoded Go+requirements allowlist — or, if there's a real reason for the narrow scope (index size, `git ls-files` cost, deliberately excluding generated/vendored content), that reason needs to generalize past "this project's own source happens to be Go" rather than silently assuming every gated subject is a Go codebase. Needs the same rigor as the other gate-honesty fixes tonight (external review, TDD, mutation testing against a synthetic non-Go fixture) given it's directly in the class of bug this project has spent tonight specifically hunting down.

## Done — PR #13, 2026-09-04

`digestEvaluationRoot` and `digestTrackedRoot` are both gone. One primitive,
`subjectTreeDigest` (`internal/store/gate_subject.go`), produces every gate
subject digest over the **whole tracked tree**, and the `CheckerCommand` special
case in `GateCheck` is deleted. Two digest implementations, one tracked-tree
reader and one per-gate branch removed; the fix is a net simplification.

Archaeology settled why the scope was narrow, and it was not a cost decision.
`trackedTracePaths` (`87565d0`, M9c) is a **go/parser input selector**: a non-Go
file in that set is a hard `U_TRACE_UNSCANNED` parse error, so the `.go` filter
is load-bearing *for traceability* and is untouched here. `digestEvaluationRoot`
(`554c344`, M10a, six hours later) reused it silently — the M10a plan discusses
the evaluation root only as an isolation property, never a content scope, and
none of its 16 adversarial findings touched digest scope. No commit, doc or
comment anywhere mentions cost. The design specs already said the subject is the
"commit/tree digest" (m10-gates-design.md:124) and that "the tree digest remains
the identity of the evaluated subject" (m10b:118), so this **restores spec
conformance** rather than inventing policy. On AIRA's own tree the old digest
covered 63.5% of tracked bytes; 190 files including all 14 `.py`, all 4 `.sh`,
`Makefile`, `go.mod` and `go.sum` were invisible.

`AppliesTo.Paths` was considered and rejected as the scope. It is a subject
selector, not a content scope; it is consumed nowhere (`CanonicalSelectorFields`
has zero callers, every gate AIRA writes is `All: true`); and narrowing an
honesty digest by author declaration would create a *new* false pass — a gate
scoped to `src/api/` would stop invalidating when `src/lib/` changed.

The two-loop found three further defects inside the digest itself:

- **Framing collision.** `path NUL data NUL` was ambiguous: one file
  `{"a":"b\0c\0d"}` and two files `{"a":"b","c":"d"}` both serialise to
  `a\0b\0c\0d\0`. Entries are now length-prefixed. Sol raised this with a vector
  that does *not* collide (`{"a":"b\0c"}` vs `{"a":"b","c":""}` — the empty file
  still emits its terminator); the Fable gate caught the error. Had the weak
  vector been used the regression test would have passed against the very framing
  it exists to reject. Both near-misses are now asserted alongside the real one.
- **The executable bit was not bound.** `chmod -x run.sh` broke a shell command
  gate while `GateCheck` re-served the stored pass — AIRA-72 in miniature, aimed
  at exactly the non-Go subjects this fix serves. Entry kind now mirrors git's
  tree model (100644/100755/120000), so symlinks bind by target too.
- **A live false fail on the command lane.** `runCommandChecker` digested the
  re-indexed materialised snapshot while `GateCheck` digested the source root;
  `git add -A` in the temp tree drops any file matched by the copied `.gitignore`,
  so a tracked-but-ignored file made a genuine command-gate pass permanently
  unservable. `materializeTrackedSnapshot` now returns the digest of the very
  entries it captured and copied, so proof time and check time agree by
  construction rather than by coincidence.

Tests: ten new, all written first, seen to fail, and mutation-tested — the fix
reverted in a throwaway copy under `~/tmp/`, each test confirmed to fail for its
own reason (scope narrowed back: 7 killed; framing reverted: 4; exec bit only: 1;
gitlink skip: 1). The headline is a **manual-attestation** gate over a subject
with no Go file at all — the scenario this ticket names, with `MaxAgeSecs: 0` —
plus a **dimension** gate binding through a different producer. A command gate is
a deliberate negative control: it already digested the whole tree on both sides
and passes on master, so it proves nothing about this fix.

Two existing tests (`TestSeedDigestInvalidatesOnDemandProof`, and
`TestGateCheckRejectsPassAfterDefinitionBindingChanges` — which the plan had
missed until the Fable gate flagged it) began reporting `U_GATE_NO_RESULT`
instead of `U_GATE_PROOF_STALE`, because each rewrites a *tracked* gate file.
Both were fixed by **isolating the variable, not relaxing the assertion**: the
fixture is now written after `git add` so the gate files stay untracked. No
assertion is weaker than before, and the behaviour isolated out of them is
covered by a new test that asserts on the subject digest directly — a
verdict-only assertion there would still have passed against the narrow scope via
`definition_digest` and proved nothing.

Also fixed: `insights.go` published this value as `tracked_worktree_digest`,
which was not true of a Go-only digest. It is now.

Measured, not assumed (committed `BenchmarkSubjectTreeDigest`): 10.2 ms for a
500-file/7 MB tree — AIRA's own scale — and 91.3 ms at 5000 files/70 MB. The
change is a net improvement for command gates, which previously rehashed the
whole tree once per gate inside `GateCheck`'s loop. No caching ticket filed: the
measurement does not justify speculative machinery.

Accepted boundary, pinned by a test: untracked files are outside the subject.
Every checker evaluates only tracked content, so the digest describes exactly
what was evaluated.

Four adjacent defects were confirmed and filed rather than buried:
**AIRA-78** (P0 — ratchet selects evidence by git HEAD, so a dirty tree can still
*mint* a fake pass; this fix stops it being *re-served* but does not close it,
which is why the yield claim is scoped), **AIRA-79** (P2 — a tracked submodule now
fails closed; accepted false-fail regression, will digest the pinned gitlink
commit), **AIRA-80** (P1 — the dimension lane digests one read and evaluates
another), **AIRA-81** (P2 — canary re-materialization drops tracked-but-ignored
files, so a canary can fire on the drop rather than on its mutation).

Full plan, archaeology, rejected alternatives and review record:
`docs/superpowers/specs/2026-09-04-aira72-gate-subject-digest-scope-plan.md`.
