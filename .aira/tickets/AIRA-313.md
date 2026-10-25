---
{"schema":1,"id":"AIRA-313","project":"aira","title":"confine --kill/--front/--resize refuse a session's own job: default owner is per-worktree, so launch and kill from different cwds disagree (RANT-48/49/50/51/53)","status":"done","kind":"bug","severity":"P2","assignee":null,"milestone":"v0.40","labels":[],"hold":false,"relations":[{"kind":"relates","from":"AIRA-319","to":"AIRA-313"}]}
---
From the 2026-10-25 rant triage; mechanism verified by reproduction.

With no AIRA_CONFINE_OWNER, both the job's owner and the management caller are the WorktreeID of the CURRENT DIRECTORY (cmd/aira/main.go resolveOwnerIn; internal/app/project.go gitIdentity: sha256 of that worktree's own git-dir). Agents cd between worktrees, subagent cwd resets between Bash calls, and backgrounded 'cd wt && aira confine' chains run elsewhere, so the same session presents two identities and CheckConfineOwner (internal/runner/confine_manage.go) refuses its own job. Repro: launch from a linked worktree, --kill from the repo root -> E_CONFINE_OWNER_UNVERIFIED owner=<hash A> caller=<hash B>; from a subdirectory of the launch worktree it succeeds.

Secondary false-pass: every session whose cwd is the same worktree (notably the repo root on master) shares an ATTESTED owner and can kill each other's jobs without --steal -- the AIRA-23 hazard.

AIRA-302 (retired) covered only the @cwd inferred-owner case (RANT-48), which stays by design.

Chosen design: (C) default owner for a Claude Code session = 'cc-'+CLAUDE_CODE_SESSION_ID (subagents inherit the parent id), inserted after --owner/AIRA_CONFINE_OWNER and before the worktree id, ONLY in resolveConfineOwner (not the dir!='' form used by worktree register); plus (A) a refusal text that says why (owner defaults to the worktree of the launch dir) and the fix. Rejected: compare ProjectID (high false-pass), delete the worktree-id default (friction, not a fix). aira top must show a cc- owner via the tmux-window fallback like a worktree hash. Skill text must change. RANT-49's E_CONFINE_NOT_FOUND is NOT explained by owner (queued or already-exited job suspected); verify it separately in the build.

Correctness-critical (ownership guard): two-loop.
