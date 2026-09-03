---
{"schema":1,"id":"AIRA-55","project":"aira","title":"Canary mutation kinds are Go-only, making command gates unusable on non-Go repos","status":"done","kind":"bug","severity":"P2","assignee":null,"milestone":null,"labels":["dogfood","gate"],"hold":false,"relations":[]}
---
## Symptom

Reported by peer session `field` dogfooding gates on a Rust project; verified directly against source. A command-checker gate always auto-runs its required canary mutation as part of evaluation, and a canary that never fires is `E_GATE_CANARY_DID_NOT_FIRE` — a permanent hard fail, not `unevaluated`. On a non-Go repo there is no mutation kind capable of making a checker fail, so such a gate can never reach a trusted pass.

## Root cause (verified by direct source read)

`internal/store/gate_command.go:413-421`:
```go
func applyMutation(root string, mutation gate.MutationSeed) error {
	switch mutation.Kind {
	case "go-inject-failing-test":
		return injectFailingTest(root, mutation)
	case "go-negate-assertion":
		return negateAssertion(root, mutation)
	default:
		return errors.New("unsupported mutation kind")
	}
}
```
Both real cases invoke `go/parser` against `.go` files (per `injectFailingTest`/`negateAssertion`). `internal/gate/canary.go` validates only these same two kind strings. There is no mutation kind for any other language/toolchain.

The fixture escape hatch (`CanaryFixture`, seeded via `Seed.Files`/`Seed.Path` in `gate_eval.go`'s `runCanary`) does not rescue this for a whole real workspace: the copy routine (`safeFixturePath`, gate_eval.go ~548-561) refuses any path segment named `.git`, so a project's actual tracked tree cannot be used as a fixture seed via `Seed.Path` — a caller would have to inline the entire tracked tree as literal JSON strings in `Seed.Files`, which is impractical for anything beyond a toy fixture.

## Secondary findings folded in (same report, verified)

1. `internal/gate/command.go` hard-requires `parser == "go-test-json-v1"` when `predicate == "tests-green"`, and hard-forbids any parser when `predicate == "exit-zero"` (lines ~76, ~79: `"E_GATE_INVALID: tests-green requires go-test-json-v1"` / `"E_GATE_INVALID: exit-zero does not accept a parser"`). This makes `exit-zero` the only legal predicate for any non-Go command — true today, but undocumented; the skill/dispatch docs should say this explicitly rather than leaving it to be discovered via trial and error.
2. A relative `argv[0]` (e.g. `make`) requires `"PATH"` in `env_allow` to resolve, and none of the documented gate examples include it — worth adding to the example so a copy-pasted gate definition doesn't silently fail to find its own command.

## Impact

Gates are effectively Go-only in practice today: any project using `predicate: tests-green` or relying on canary-proven command gates in another language cannot reach a trusted pass state through the documented mechanism. `field`'s workaround was to drop the aira gate mechanism entirely for a Rust project and implement the same canary discipline (mutate → assert fail → revert → assert pass) as a hand-rolled `make canary` target instead — which is the right call given a canary that can't fire is worse than none, but it means the gate subsystem currently provides no real value outside Go.

## Suggested direction

Not urgent to design today (P2, known gap rather than active harm — nobody is being silently misled the way findings AIRA-53/AIRA-54 do, since `E_GATE_CANARY_DID_NOT_FIRE` fails loud rather than lying). If/when this is picked up: either generalize the mutation-kind mechanism past Go (e.g. a generic "flip a designated exit code" or "corrupt a designated assertion via a project-supplied sed/patch script" primitive), or make the fixture-seed path usable for a whole real tracked tree (e.g. relax the `.git`-segment ban to specifically exclude only `.git` at the seed root rather than banning the segment anywhere in the tree, if that's what's actually blocking it) so language-specific mutation isn't the only route to a valid canary.

---

## Status: done + deployed

Branch `aira55-gate-mutation-portability`, PR
<https://github.com/battlesnake/aira/pull/6> (commits `13cffa3`, `b805cba`),
squash-merged as `cd02ddac708925396cb81c71a1082c555b8347bc`. `make ci`
(fmt-check, vet, build, `go test ./... -count=1`) exited **0** with every
package green.

## Resolution (2026-09-03)

Two of the three items are fixed. The fixture-seed alternative floated in
"Suggested direction" above was investigated in depth and is deliberately
**not** implemented — it is both insufficient and actively unsafe; the specific
reason is recorded below so nobody re-proposes it.

### Fixed — `inject-file`, the language-agnostic mutation kind

A third member of the closed `MutationSeed` union: it creates a declared file
with a declared literal body inside the isolated mutation snapshot. No plugin,
no extension point, no script or patch execution — the body is literal bytes
written verbatim, so §4.7's no-executable-content invariant still holds.

- `internal/gate/canary.go` — `MutationSeed.Content` (`json:"content"`), bounded
  at 64 KiB and required valid UTF-8; `validateMutation` gains an `inject-file`
  case, and both `go-` kinds now also require `Content == ""` so the union stays
  genuinely closed rather than silently ignoring cross-kind data.
- `internal/store/gate_command.go` — `injectFile`, create-only via
  `O_CREATE|O_EXCL` (atomic, no `Lstat` TOCTOU). An existing target is refused,
  never overwritten, so the mutation is provably additive: it can neither
  destroy subject content nor report an injection it did not make. The apply
  step re-checks the path with `safeSnapshotPath` — the same predicate the
  tracked snapshot already uses — rather than trusting the declaration
  validator. The `.git`-segment half of that predicate is the load-bearing
  part: a write into the snapshot's own `.git` (a config carrying
  `core.fsmonitor`, or a hook) would be executed by the `git add` that
  re-stages the mutation. Pinned by an explicit unsafe-target loop in the store
  test, including `.git/hooks/aira-evil`, which does not exist in a fresh
  snapshot and so would be created happily by `O_EXCL` alone.
- `internal/core/core.go` — `inject-file` added to the `mutation_kind` enum plus
  a `mutation_content` field, for parity with the existing `mutation_*` fields.

No CLI plumbing was needed to make the kind usable: gate and canary declarations
are hand-authored JSON under `.aira/gates/`, and `gate add`'s `mutation_*` fields
are an explicitly discarded transport seam (`GateActionWithFields` does
`_ = fields`). The new arg description says so, so nobody expects
`gate add --mutation-content` to write a declaration.

A non-Go project can now reach a trusted, canary-proven command-gate pass:
declare an `exit-zero` gate over `cargo test` / `pytest` / `npm test` plus an
`inject-file` canary that drops a failing test into the conventional test
directory. Pinned end-to-end by
`TestNonGoCommandGateReachesTrustedPassViaInjectFileCanary`
(`internal/store/gate_command_integration_test.go`), whose fixture contains no
Go source, uses no parser, and asserts `trusted=true` with an empty code.

**Documented, accepted limitation — this kind's proof is weaker than
`go-inject-failing-test`'s.** That kind injects a compiling, failing test, so its
fire proves the whole test-failure-to-nonzero-exit pathway. `inject-file` proves
only that the declared perturbation produces a nonzero exit. The concrete
honest-mistake false pass it admits: a `make test` recipe that aborts on a
compile error but swallows real test failures fires the canary on a
syntax-broken injection and would never fire on a real failing test, so the lane
earns a trusted pass it cannot back. A declaration must therefore inject a
compiling, failing test in the subject's own language — a body that merely
breaks the build proves only that the build breaks. Recorded in the m10b design
spec, the `injectFile` doc comment, and the agent-facing `mutation_kind`
description.

Second documented limitation: a target matched by the subject's git excludes
(its own `.gitignore`, or the user's `core.excludesFile`) is dropped by the
`git add -A` that re-stages the mutated snapshot and never reaches the checker's
`git ls-files --cached` view, so the gate reports the hard
`E_GATE_CANARY_DID_NOT_FIRE` — loud, never a false pass. Pinned by
`TestInjectFileCanaryIntoIgnoredPathDoesNotFire`.

### Fixed — the two documentation findings, with one correction

Both were real gaps in the *agent-facing* surface, and both are fixed in the
`gate` dispatch `ArgSpec` descriptions (`internal/core/core.go`), which render
into generated CLI help, MCP tool schemas, the agent guide and `SKILL.md`:

- `predicate` now states that `tests-green` requires `go-test-json-v1`, which
  reads `go test -json` only, so `exit-zero` is the only predicate a non-Go
  command can use.
- `parser` now states it is required by `tests-green` and refused with
  `exit-zero`.
- `env_allow` now states that a relative `argv[0]` such as `make` resolves only
  when `PATH` is listed, and that a definition without it is refused.

**Correction to finding 2 as filed.** `internal/gate/command.go:95-97` already
*enforces* the PATH rule with a hard `E_GATE_INVALID: relative argv[0] requires
PATH in env_allow`, so a copy-pasted gate does not "silently fail to find its
own command" — it is refused at validation. And the canonical `gate add` example
(`internal/core/core.go`) already passed `--env-allow PATH`. The real gap was
that the *rule* was documented only in the m10b design spec, never on the
surface an agent reads. That is what changed.

### Deliberately NOT done — relaxing `safeFixturePath`'s `.git` ban

The "Suggested direction" alternative was traced through and rejected:

1. **It would be unsafe, and this is the decisive reason.** `safeFixturePath`
   guards three sites, not one: `Seed.Path`, every `Seed.Files` key, and every
   walked entry of `copyFixtureSeed`. Relaxing the segment ban therefore also
   admits `Seed.Files[".git/config"] = "[core]\n\tfsmonitor = <cmd>"` —
   `ValidateCanary` only rejects the exact key `.git`, not `.git/config` — and
   the unconditional `git add -A` in `runCanary` then *executes* that command.
   That converts seed content into an executed command, the exact invariant the
   `MutationSeed`/fixture design forbids. Separately, a seed subdirectory that is
   itself a linked git worktree contains a `.git` *file* holding an absolute
   `gitdir:` pointer; copying it and running `git init` + `git add -A` was
   verified to silently rewrite the **real** worktree's index.
2. **It would be insufficient anyway.** A whole-tree seed needs `Seed.Path` to be
   the repo root, but `safeFixturePath` rejects `clean == "."` independently of
   the `.git` rule. Relaxing the segment ban alone changes nothing for the stated
   use case.
3. **It is unnecessary.** The mutation path does not evaluate the checker against
   the seed at all — it re-materializes the real tracked tree via
   `materializeTrackedSnapshot(s.root)`. So `inject-file` already delivers what
   the whole-tree fixture seed was wanted for. (`copyFixtureSeed` also writes
   every file `0o644`, dropping the executable bit, so a real tree's scripts
   would be broken by that route regardless.)

### Left open, with recommended direction

1. **`tests-green` is still Go-only.** The parser vocabulary remains
   `go-test-json-v1` alone, so a non-Go gate is limited to `exit-zero` — now
   documented rather than discovered by trial and error. Generalizing it is a
   real design item, not something to improvise: the concrete opening is that a
   JUnit XML decoder **already exists** in `internal/store/testreport_parse.go`
   (from the AIRA-31 report work), so a `junit-xml-v1` command parser would
   mostly be a lift of that decoder into `internal/gate` beside
   `ParseGoTestJSONV1`, plus the `Command.Validate` predicate/parser pairing and
   a discovered/failed count contract that keeps zero-count → `unevaluated`.
   That is the highest-value follow-up if gates get real non-Go use.
2. **No `replace-text` / negate-an-existing-assertion kind.** `inject-file`
   covers test runners that discover tests by convention (cargo, pytest, jest).
   A project whose test target runs an explicit file list may need to perturb an
   existing assertion instead. The shape would be: declared file, declared
   literal `from`/`to`, declared occurrence index, refusing unless that exact
   occurrence exists — still typed data, no sed or patch execution. Not built
   because nothing has asked for it yet.
3. **Declaration/evaluation validation asymmetry (own follow-up ticket).**
   `ValidateCanary` (`internal/gate/canary.go:149-153`) checks seed paths with a
   non-normalizing prefix test, and never checks `Seed.Path`'s shape at all,
   while `safeFixturePath` (`internal/store/gate_eval.go:548`) is the real
   normalizing check at the two evaluation call sites. So `ValidateCanary`
   accepts and digests values that evaluation then refuses, including
   `.git/config`, `.git/hooks/pre-commit`, `sub/.git/config`,
   `a/../../etc/passwd` and `./../x` (the last two normalize to escapes but
   carry no literal `../` prefix). Fail-closed today, so not a defect — but
   fail-closed-today is not safe-forever: the safety lives at two call sites in
   one function rather than in the type's validator, so any future consumer of
   `Seed.Files`/`Seed.Path` that reasonably assumes a validated declaration
   inherits an unvalidated path, and the blast radius is command execution via
   `core.fsmonitor`, not merely a bad write. The fix is to have
   `ValidateCanary` use the same normalizing predicate (and apply it to
   `Seed.Path` too), moving the refusal to declaration time where an author
   sees it. Left out of AIRA-55 deliberately to keep that change small.
