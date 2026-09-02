---
{"schema":1,"id":"AIRA-46","project":"aira","title":"TestRewriteResolverMatchesInstalledGitFixtures corrupts the real repo when run under a git hook (inherited GIT_DIR)","status":"done","kind":"bug","severity":"P0","assignee":null,"milestone":null,"labels":["git-safety","testing"],"hold":false,"relations":[{"kind":"duplicates","from":"AIRA-48","to":"AIRA-46"}]}
---
**Live-confirmed 2026-09-02, on the shared machine, via a real `git push`.**
A `git push` from the aitest-design worktree triggered `.githooks/pre-push`
(`exec make -C "$ROOT_DIR" ci`, which runs `go test ./... -count=1`,
UNCONFINED, not through `aira confine`). `internal/gitremote/
rewrite_fixture_test.go`'s `TestRewriteResolverMatchesInstalledGitFixtures`
ran during that suite and:

1. Wrote garbage directly into the SHARED, repo-wide
   `/home/mark/claude/aira/.git/config` (used by every worktree): flipped
   `core.bare` to `true`, overwrote `remote.origin.url` to the literal
   fixture string `conditional:repo`, hijacked `user.name`/`user.email` to
   `AIRA`/`aira@example.test`, and appended the test's own bogus
   `url.*.insteadOf`/`pushInsteadOf` rules plus `include`/`includeIf`
   directives pointing at `/tmp/TestRewriteResolverMatchesInstalledGit
   Fixtures.../` (ephemeral, WSL-`/tmp`-volatile paths per this project's
   own temp-file discipline).
2. Committed 12 commits directly onto `refs/heads/worktree-aitest-design`
   (author `Terra`/`AIRA`/`AIRA Test`, messages `launch`/`coverage
   baseline`/`current`/`empty`) that DELETE most of the real repository --
   all of `internal/store` (79 files), every file under `.aira/tickets/`
   (29 tickets), `.aira/config`, `cmd/aira/main.go`, `CLAUDE.md`,
   `Makefile`, every CI workflow -- replaced with a handful of unrelated
   `.aira/gates`/`.aira/requirements` scaffold files that belong to some
   OTHER project's fixture data, not AIRA's own.
3. This is the direct, confirmed cause of the SEPARATE panic seen in the
   same `make test` run: `internal/store`'s
   `TestTraceabilityStatusEnumeratesEveryBucketAndDangling` (nil map
   interface conversion) -- it ran AFTER the corruption above and hit a
   suddenly-empty/mangled ticket store it never expected. That panic is a
   cascading SYMPTOM, not an independent bug.

**Root cause, fully diagnosed.** `rewrite_fixture_test.go`'s own isolation
design is otherwise correct -- a real `t.TempDir()`, `git -C root init -q`,
every subsequent git call using `-C root`. But its `runGit`/`runGitOutput`
helpers (lines 85-100) build `exec.Command("git", ...)` WITHOUT ever
setting `cmd.Env` -- Go's default (`cmd.Env == nil`) inherits the full
parent environment verbatim. `-C root` changes where git *looks for* a
repo, but does **not** override an already-set `GIT_DIR` environment
variable -- and Git's own documented contract sets `GIT_DIR` (and
`GIT_WORK_TREE`/`GIT_INDEX_FILE`/etc.) in the environment of every hook
invocation. So when this test runs from `go test` invoked by `make ci`
invoked by `.githooks/pre-push` (a genuine git hook context), every `git
-C root ...` call in the test silently operates on the REAL repository
identified by the inherited `GIT_DIR`, not the intended isolated `root`.
Confirmed by the leaked evidence itself: the corrupted config's `include`/
`includeIf` paths are literally this test's own `t.TempDir()` fixture
paths for its "include" and "conditional include" sub-tests.

This is why the bug never surfaced in ordinary `aira confine -- go test
./...` runs (no `GIT_DIR` in that environment, so `-C root`'s normal
discovery works correctly) -- it is specifically triggered by running the
suite from inside a git hook. `internal/gitremote/exec.go` (the PRODUCTION
code this test exercises) is NOT at fault -- it correctly threads a
caller-supplied `cmd.Env`/`cmd.Dir` (`request.Env, request.Dir`); only the
test's own unsanitized subprocess environment is the bug.

**Impact.** Any `git push`/`git commit` (or anything else invoking
`.githooks/pre-push` or `.githooks/pre-commit`, if that hook has the same
`make ci`/`make test` shape) on ANY branch, from ANY worktree on this
shared machine, can silently corrupt the repo-wide git config and delete
most of the tracked repository from whatever branch happens to be checked
out in the worktree that triggered the hook -- with no visible error other
than the downstream `internal/store` panic (easy to misdiagnose as an
unrelated flake). Recovered by hand this time (shared config repaired via
targeted `git config` edits; the contaminated branch ref reset back to its
last known-good commit -- the actual working-tree files were never
touched, since the corrupting commits were pure git-object/ref writes with
no real checkout).

**Fix.** In `runGit`/`runGitOutput` (rewrite_fixture_test.go), explicitly
set `cmd.Env` to a sanitized copy of `os.Environ()` with every `GIT_*`
variable stripped before every subprocess `git` call.

Not related to AIRA-30/aitest in any way -- found purely as a side effect
of pushing that branch. `internal/gitremote` is untouched by AIRA-30.

**DONE + DEPLOYED (2026-09-02, master via PR #1, squash commit `3c519b9`),
standalone from AIRA-30.** Added `sanitizedGitEnv()` stripping `GIT_`-prefixed
vars, wired into both `runGit` and `runGitOutput`. Verified via `aira
confine -- go test ./internal/gitremote/...` (all pass) then re-checking
the shared .git/config and affected branch refs were unchanged after the
confined run.

Deployed as part of the same binary swap + daemon restart as AIRA-30.

RESOLVED residual risk (was open as of the initial fix): the local
pre-push hook's `make ci` always builds/tests against the shared
/home/mark/claude/aira checkout specifically (ROOT_DIR is derived from
the hook script's own absolute, shared hooksPath, not the pushing
worktree) — so the hook stayed a live risk for any future push, from any
worktree, until that checkout's own working tree had the fix. Owner ran
`git -C /home/mark/claude/aira pull --ff-only origin master` in a plain
shell outside the harness on 2026-09-02, confirmed present via direct
read of the file afterward. Hook is safe again.

Still open, not designed here: (a) grep the rest of the tree for the same
`exec.Command("git"` pattern without a sanitized `cmd.Env`, to confirm
this was the only instance; (b) the hook testing whatever the shared
checkout happens to have checked out, rather than what's actually being
pushed, is a separate latent design surprise worth a look sometime.

Duplicate ticket AIRA-48 was briefly created for this by a later,
context-compacted continuation of this same session that had lost track
of this original ticket; retired as a duplicate.
