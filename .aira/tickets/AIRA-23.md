---
{"schema":1,"id":"AIRA-23","project":"aira","title":"Default/infer confine owner to prevent cross-session kill misattribution","status":"planned","kind":"chore","severity":"P2","assignee":null,"milestone":null,"labels":["confine","dogfood","safety"],"hold":false,"relations":[]}
---
Reported by two dogfooding sessions 2026-08-31: `aira confine --list` shows OWNER "unknown" for jobs
launched outside an adopted project without --owner/AIRA_CONFINE_OWNER. One session nearly `pgrep`-killed
TWO sibling sessions' gates, mistaking them for its own — caught only by inspecting each proc's cwd.

CURRENT BEHAVIOUR (confirmed): resolveConfineOwner (cmd/aira/main.go:890-909) precedence is
  --owner flag → AIRA_CONFINE_OWNER env → discovered project WorktreeID → "unknown" (runner.ConfineUnknownOwner,
confine.go:124). So a confine launched outside a project worktree with neither flag nor env → "unknown".
Filesystem-scan-only discovery (no live daemon registry entry) also renders "unknown"
(confine_manage_linux.go:79,204).

DIRECTION: make owner default to a stable, session-distinguishing identity so --list is actionable and
cross-kill hazard drops, WITHOUT weakening the existing kill ownership-guard (#68). Candidates: derive from
the launching session (e.g. the CC socket / session id), the login+pid-namespace, or the supervisor's cwd
repo — whatever is cheap and stable. Keep "unknown" only as a genuine last resort, and never let a fabricated
owner bypass the cross-session kill guard. Small, high-safety hygiene item. relates: #68 (confine --list/--kill
ownership guard), the detached-confine ticket (a persisted owner-keyed record helps here too).
