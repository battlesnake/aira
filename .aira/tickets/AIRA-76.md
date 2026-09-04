---
{"schema":1,"id":"AIRA-76","project":"aira","title":".githooks/pre-commit runs unconfined against the shared root checkout, same class as the known pre-push flaw","status":"done","kind":"bug","severity":"P1","assignee":null,"milestone":null,"labels":["dogfood","tooling"],"hold":false,"relations":[]}
---
Found live during the whole-project simplification review (PR #12) — the review agent's own commit was briefly blocked because `.githooks/pre-commit` ran `make fmt-check vet build` unconfined against the SHARED ROOT checkout (`/home/mark/claude/aira`), not the pushing worktree, and failed on a completely unrelated concurrent session's unformatted `internal/daemon/server.go`. Both of that agent's commits needed `--no-verify` as a result.

This is the exact same structural flaw already known and worked around all night for the pre-PUSH hook (every build brief tonight had to mandate `--no-verify` on push for this reason) — except this is pre-COMMIT, a separate, previously-undocumented instance of the same bug. `ROOT_DIR` in the hook script presumably resolves to the shared root regardless of which worktree is actually committing, same root cause as the push-side version.

Fix: same as whatever gets decided for the push-side hook — resolve the actual worktree root rather than a hardcoded/discovered shared path, or run confined against the correct tree. Given this has now bitten multiple sessions across both hook types tonight, worth fixing properly rather than continuing to route around it with `--no-verify` reminders in every brief.

## Fixed — simplification programme Phase 0 (branch `aira-phase0-mechanical`)

Root cause confirmed exactly as suspected, and demonstrated rather than reasoned
about: `core.hooksPath` is one absolute path (`/home/mark/claude/aira/.githooks`)
shared by the primary checkout and every linked worktree, so
`ROOT_DIR="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)"` resolved to the
primary checkout no matter which worktree was committing. A probe in a throwaway
repo showed a commit made in a linked worktree resolving `ROOT_DIR` to the
primary checkout while git's own cwd and `GIT_DIR` both correctly named the
linked worktree.

The fix, in `.githooks/common.sh` shared by both hooks:

1. `ROOT_DIR` comes from `git rev-parse --show-toplevel`. Verified empirically
   that git runs hooks from the top of the triggering worktree and exports a
   `GIT_DIR` naming its admin directory, so this is correct even when committing
   from a subdirectory.
2. The heavy targets run under `aira confine --`, per CLAUDE.md.
3. **GIT_* is scrubbed out of the build's environment.** This goes beyond the
   ticket's wording and closes AIRA-46's residual item (b): git exports `GIT_DIR`
   and `GIT_INDEX_FILE` into a hook, and any test that shells out to git inherits
   them and writes to the real repository even when it passes
   `git -C <its own temp dir>`. That corrupted the shared repo once already.
4. The hook fails closed when `aira` is absent, rather than silently running the
   heavy targets unconfined.

`internal/daemon/server.go` was committed unformatted in `9a65d47`, so
`make fmt-check` failed on master and the fixed hook would still have blocked
every commit; gofmt applied in the same change.

Regression tests (`internal/install/githooks_test.go`) build a throwaway
repository with a primary checkout and a linked worktree, install the real hook
files at a shared absolute hooksPath, and run a real `git commit` / `git push`
from the linked worktree with recording shims for `make` and `aira`. All five
were confirmed to FAIL against the pre-fix hooks: they record `-C <primary>`, no
`aira confine`, and twelve leaked `GIT_*` variables.

Note for anyone committing before this merges: `--no-verify` is still required
in any worktree whose checked-out `.githooks` predates the fix, because
`core.hooksPath` points at the primary checkout's copy.
