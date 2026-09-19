---
{"schema":1,"id":"AIRA-265","project":"aira","title":"aira top: SESSION column showing the confining session's name","status":"planned","kind":"feature","severity":"P3","assignee":null,"milestone":"v0.20","labels":["confine"],"hold":false,"relations":[]}
---
Owner (2026-09-19): sessions set an env var to their human-friendly name when calling aira confine, but it isn't shown in aira top. Owner decisions: (1) new SESSION column in aira top; (2) KEEP the existing --owner flag + AIRA_CONFINE_OWNER env var (no new param) — just surface it.

## Diagnosis (grounded)
aira top's table columns are SLOT/NAME/PID/LIVE/AGE/RESERVATION/RAM/CPU CORES/COMMAND (tui_top.go:613); OWNER was removed in AIRA-135 because owners are sometimes a 50+ char hex hash that crowded the table. The session identity IS the job's owner (resolveConfineOwner: --owner flag -> AIRA_CONFINE_OWNER env -> discovered hash), which already flows to ConfineRecord.Owner and shows in 'aira confine --list' — just not in aira top.

## Poll of 7 sessions (field/fly/speed/split/deploy/subpipe/qual; 'spice' not a live session)
- Only field sets anything today: AIRA_CONFINE_OWNER=claude-stoner (env var, no flag). The other six set NOTHING (bare aira confine); speed occasionally uses --name as a per-JOB label (gate183-merge), not a session name.
- Unanimous: the mechanism must be an ENV VAR exported once per session (inherited by every descendant aira confine), not a per-call flag — most confined jobs are launched by SUBAGENTS via briefs that never thread flags.
- KEY UPSTREAM FINDING (deploy+qual verified): the human name ('deploy','qual') is NOT in any env var — it lives only in the Claude session registry (ListAgents/cc-socks). The only stable env id is CLAUDE_CODE_SESSION_ID (a UUID). aira CANNOT map UUID->friendly-name (that registry is the harness's, not aira's). So for the column to populate fleet-wide, each session (or the launcher/harness) must export AIRA_CONFINE_OWNER=<name>. aira reads+displays; populating the env is UPSTREAM of aira.
- subpipe: a project CLAUDE.md already documents AIRA_CONFINE_OWNER as the intended mechanism.

## Design (approved: keep --owner/AIRA_CONFINE_OWNER)
- Add a SESSION column to aira top's table (tui_top.go topViewModel), sourced from record.Owner.
- SUPPRESS hash-style owners (the discovered default, a 32/64-char hex) — render '—' — so the 50+ char hex never re-crowds the table (the AIRA-135 concern). Show human-friendly owners verbatim (truncated to a sane width).
- Unset/hash -> '—'; the existing PID column already attributes un-named jobs, so '—' is honest (no fabricated name). (subpipe wanted a non-blank fallback, but PID already attributes.)
- Column placement: keep COMMAND last (unbounded width); put SESSION as a narrow column (before COMMAND). Watch total width per the AIRA-135 clamp note.

## Out of scope / upstream (surface to owner, not built here)
- Populating AIRA_CONFINE_OWNER fleet-wide (harness/launcher injecting the friendly name into each session's env). aira can't do it. Offer to raise with whoever owns session launching.

Build after v0.19 (AIRA-264) ships. Small display change; still Opus-builds/Fable-reviews per aira convention.

## Peer input (2026-09-19 poll + v0.19 release acks)
- **field**: sets `AIRA_CONFINE_OWNER=claude-stoner` (env, human slug) — the ONLY session doing so today.
- **deploy**: knows its name ("deploy") from the messaging layer, NOT its process env; will set `AIRA_CONFINE_OWNER=deploy` per-invocation once the column ships; strongly favours harness injection.
- **speed**: will set `--owner speed` per-command + mandate it in builder briefs.
- **subpipe**: env DOESN'T persist across its Bash invocations (each re-inits from profile) → can't do one sticky export, will prefix per-command, so mostly UN-ANNOTATED; won't edit the user's profile (correct). Explicitly requests the SESSION cell NOT be blank for un-annotated jobs.
- **fly / qual**: set nothing; friendly name is not in env (only CLAUDE_CODE_SESSION_ID UUID). qual suggested aira map UUID→friendly-name — INFEASIBLE (that registry is the Claude harness's, aira has no access).

## Fallback for un-annotated jobs (decision needed at build)
Honest default: DO NOT fabricate a session name. `aira top` already has a PID column that attributes every job. Options: (a) render `—` for un-annotated/hash owners (matches the owner's approved "elided" for hashes; PID attributes); (b) per subpipe's ask, resolve the SESSION cell to a real handle (short scope-id or supervisor-PID) so the column stays useful when most jobs are unset. Leaning (b)-with-a-real-handle to satisfy subpipe without fabricating a name; confirm with owner during the build.

## Strong cross-session recommendation → owner (upstream of aira)
deploy + fly + qual + speed all point at the same real fleet-wide fix: the HARNESS/launcher injecting `AIRA_CONFINE_OWNER=<friendly-name>` into each session's env, which `aira confine` already auto-reads. aira CANNOT do the injection (the name isn't in the env and the registry is the harness's). Without it the column is mostly `—`/handles. Raise with whoever owns session launching.
