---
{"schema":1,"id":"AIRA-82","project":"aira","title":"Rants filed from a feature worktree are misattributed to master's git context","status":"planned","kind":"bug","severity":"P2","assignee":null,"milestone":null,"labels":["dogfood","honesty","rant"],"hold":false,"relations":[]}
---
Found live while dogfooding during AIRA-71's fix (RANT-18, filed from worktree `/home/mark/claude/aira-aitest-skill`). Its recorded `git_context` reads `worktree_path=/home/mark/claude/aira`, `head_ref=refs/heads/master`, `head_hash=9a65d47` — the shared root, not the actual worktree the rant was filed from. Same "confidently wrong recorded metadata" class as AIRA-71 itself (a mechanism reporting something specific and provably incorrect rather than failing honestly). Not investigated further — needs tracing to wherever rant capture resolves its git context (likely resolving from the daemon's own process cwd/registry entry rather than the calling client's actual worktree) and a fix so a rant's recorded provenance matches where it was actually filed from.

## Re-scoped (2026-09-04, backlog-remediation Phase 0, plan section 2) — text only, the fix is Phase 2

**"Confidently wrong recorded metadata" is the wrong frame, and this correction
is the point of the re-scope: nothing fabricates anything.** The daemon records
faithfully what the FACE handed it.

Verified against source: `stampGitContext` (`cmd/aira/dispatcher.go:691`)
resolves the git context from the `daemon.WorktreeScope` it is given, and that
scope comes from `scopeForCWD` -> `app.Discover(ctx, cwd)` — the *calling
process's* working directory. For a CLI invocation inside a worktree that is
correct. For a long-lived face whose process cwd is not the caller's worktree —
the MCP server being the obvious one — every request carries that process's cwd,
so a rant filed "from" a feature worktree is recorded against whatever directory
the face happens to live in. The mechanism is a missing per-call scope, not a
fabricated one.

**The real fix therefore moves to Phase 2** (plan section 4): an explicit
per-call scope override on the MCP/CLI faces. It touches face-layer code, not a
pure deletion, which is why it is not in this phase.

**Second candidate mechanism, worth ruling out first when that fix is built.**
AIRA-93 (landed in this same PR) found that `app.Discover`'s git helper inherited
`GIT_DIR` from its environment, and an inherited `GIT_DIR` OVERRIDES the explicit
`git -C <dir>` — resolving a *different repository* than the directory asked
about. That produces exactly this ticket's symptom (`worktree_path=` the shared
root, `head_ref=refs/heads/master`, filed from a feature worktree) and it is
demonstrated by a mutation test in `internal/app/git_env_test.go`. That path is
now scrubbed, so whoever builds the Phase 2 fix should re-check whether RANT-18's
misattribution is still reproducible at all before designing around the scope
override alone.
