---
{"schema":1,"id":"AIRA-60","project":"aira","title":"ValidateCanary doesn't apply the same normalizing path check that evaluation relies on for seed safety","status":"done","kind":"bug","severity":"P2","assignee":null,"milestone":null,"labels":["dogfood","gate"],"hold":false,"relations":[]}
---
Follow-up from AIRA-55, deliberately left out of that ticket to keep it small.

`ValidateCanary` (`internal/gate/canary.go:149-153`) checks seed paths with a non-normalizing prefix test, and never checks `Seed.Path`'s shape at all. `safeFixturePath` (`internal/store/gate_eval.go:548`) is the real normalizing check, applied at the two evaluation call sites (`gate_eval.go:491` and `:500`). So `ValidateCanary` accepts and digests values that evaluation then refuses, including `.git/config`, `.git/hooks/pre-commit`, `sub/.git/config`, `a/../../etc/passwd` and `./../x` (the last two normalize to escapes but carry no literal `../` prefix).

Fail-closed today, so not a live defect — evaluation's own check still catches everything declaration validation misses. But fail-closed-today is not safe-forever: the safety lives at two call sites inside one function (`runCanary`) rather than in the type's own validator, so any future consumer of `Seed.Files`/`Seed.Path` that reasonably assumes a validated declaration is actually safe inherits an unvalidated path. The blast radius if that assumption is ever wrong is command execution (a `.git/config` carrying `core.fsmonitor`, executed by the unconditional `git add -A` in `runCanary`), not merely a bad write — see AIRA-55's own "deliberately NOT done" section for why that specific vector is real and already verified, not theoretical.

Fix: have `ValidateCanary` use the same normalizing predicate `safeFixturePath` already implements (and apply it to `Seed.Path` too, which it currently doesn't check the shape of at all), moving the refusal to declaration time where a gate author actually sees it, rather than only at evaluation time.

## Resolution (2026-09-04)

Closed. `safeFixturePath`, `safeSnapshotPath` and `safeMutationPath` are one
exported `gate.SafeRelativePath`, the conjunction of every rejection the three
made (never a disjunction of their acceptances, which would loosen it).
`ValidateCanary` applies it to every `Seed.Files` key AND to `Seed.Path`, whose
shape it never checked at all -- so the refusal now lands at declaration time
where a gate author sees it, not only at evaluation time.

Behaviour delta is refusal-only and is exactly one case: NUL, which only the
mutation copy checked. Unreachable from `git ls-files -z` (NUL is the
separator), reachable from a hand-authored declaration.

All five vectors this ticket names -- .git/config, .git/hooks/pre-commit,
sub/.git/config, a/../../etc/passwd, ./../x -- are committed as test cases in
both the Seed.Files and Seed.Path positions, and the three superseded predicates
are kept verbatim in the test as executable equivalence counterexamples.

Plan: docs/superpowers/plans/2026-09-04-aira80-81-60-86-captured-subject-plan.md
