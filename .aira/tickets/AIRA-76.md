---
{"schema":1,"id":"AIRA-76","project":"aira","title":".githooks/pre-commit runs unconfined against the shared root checkout, same class as the known pre-push flaw","status":"planned","kind":"bug","severity":"P1","assignee":null,"milestone":null,"labels":["dogfood","tooling"],"hold":false,"relations":[]}
---
Found live during the whole-project simplification review (PR #12) — the review agent's own commit was briefly blocked because `.githooks/pre-commit` ran `make fmt-check vet build` unconfined against the SHARED ROOT checkout (`/home/mark/claude/aira`), not the pushing worktree, and failed on a completely unrelated concurrent session's unformatted `internal/daemon/server.go`. Both of that agent's commits needed `--no-verify` as a result.

This is the exact same structural flaw already known and worked around all night for the pre-PUSH hook (every build brief tonight had to mandate `--no-verify` on push for this reason) — except this is pre-COMMIT, a separate, previously-undocumented instance of the same bug. `ROOT_DIR` in the hook script presumably resolves to the shared root regardless of which worktree is actually committing, same root cause as the push-side version.

Fix: same as whatever gets decided for the push-side hook — resolve the actual worktree root rather than a hardcoded/discovered shared path, or run confined against the correct tree. Given this has now bitten multiple sessions across both hook types tonight, worth fixing properly rather than continuing to route around it with `--no-verify` reminders in every brief.
