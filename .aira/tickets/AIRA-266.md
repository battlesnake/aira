---
{"schema":1,"id":"AIRA-266","project":"aira","title":"Confine SKILL: correct the AIRA_CONFINE_OWNER steer (inline env prefix, not a bare/shared export)","status":"done","kind":"bug","severity":"P3","assignee":null,"milestone":"v0.21","labels":["confine","skill"],"hold":false,"relations":[]}
---
Follow-up to AIRA-265 (v0.20). The SKILL's long-standing owner steer — "For long-lived agent sessions, `export AIRA_CONFINE_OWNER=<stable-session-id>` before launching or managing jobs" — is INACCURATE for Claude sessions and unsafe on a shared box. AIRA-265 sharpened it (SESSION-column motivation) but kept the misleading "export" framing.

## Why it's wrong (verified)
- **A one-off `export` does not persist in a Claude session.** The Claude Code Bash tool re-initialises the shell from the profile on EVERY call (stated in the Bash-tool contract: "Shell state (env vars, functions) does not persist; the shell is initialized from the user's profile"). So `export AIRA_CONFINE_OWNER=x` in one tool call is gone by the next, and subagents get fresh shells. speed + subpipe both reported this independently.
- **A SHARED-profile export mislabels every session.** On this multi-session, single-uid box every session re-sources `~/.bashrc`, so an `export AIRA_CONFINE_OWNER=speed` there would tag EVERY session's jobs "speed" — false attribution, worse than the `#<pid>` fallback (speed flagged this footgun; deploy confirmed the same for committed tooling).

## Owner's intended pattern (2026-09-19)
- `--owner` (session) is typically set by ENV VAR because `aira confine` calls are often several layers deep in scripts/subshells/makefiles. The caller sets it INLINE on the command that launches the work: `AIRA_CONFINE_OWNER=some_name make -j`, `AIRA_CONFINE_OWNER=some_name ./my_script.sh`. That is per-invocation (correctly scoped to one process tree, no cross-session bleed, agent-self-applicable) AND it propagates into every nested `aira confine` with no per-call-site edits — which also solves deploy's "~dozen call sites, no wrapper" problem.
- `--name` (per-task label) is typically set at the specific call site, since it identifies the task.

## Fix (SKILL text only — internal/core/skill.go)
Reword the owner steer to:
- Steer the INLINE env-var prefix on the launch command (`AIRA_CONFINE_OWNER=<name> make …` / `… ./script.sh`) as the default for agents, OR `--owner <name>` on a bare `aira confine`.
- WARN: a bare one-off `export` does not persist across Claude Bash calls; and NEVER put `export AIRA_CONFINE_OWNER=…` in a SHARED shell profile on a multi-session box (mislabels every session).
- Note the per-session launcher-injected base env is the zero-touch persistent route (how field's `claude-stoner` shows), but agents don't control their own launcher.
- Keep `--name` framed as the per-task label (distinct field).
- Update the skill_test pins (keep "`SESSION` column of `aira top`"; pin the new inline-prefix + shared-profile-warning phrases).

Client-side, drop-in, SKILL-only → v0.21. Lighter review path (doc; content pre-vetted by owner + speed + deploy) + full make ci green. Also closes out AIRA-265 (status → done).
