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

## Design (early sketch — SUPERSEDED by "FINALIZED design" below)
NOTE: the '—'/truncation choices in this section were the early sketch. The BUILT behaviour is the "## FINALIZED design (grounded in live data)" section below: hex → 8-char PREFIX (not '—'), human/@cwd verbatim with NO viewmodel truncation, unset → '#<pid>'. Kept for history; read FINALIZED for what shipped.
- Add a SESSION column to aira top's table (tui_top.go topViewModel), sourced from record.Owner.
- ~~SUPPRESS hash-style owners — render '—'~~ → BUILT AS: 8-char prefix (groups a worktree's jobs). Show human-friendly owners verbatim (~~truncated~~ → NOT truncated; the table clamps).
- ~~Unset/hash -> '—'~~ → BUILT AS: hex → 8-prefix, empty/unknown → '#<pid>' (subpipe's non-blank ask honoured with a real handle, per owner's "short scope-id / PID").
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

## Fallback for un-annotated jobs (RESOLVED — see "Fallback decision RESOLVED" below)
Honest default: DO NOT fabricate a session name. Options considered: (a) render `—`; (b) a real handle (short scope-id or PID). RESOLVED to a hybrid of (b): hex → 8-char grouping prefix, empty/unknown → `#<pid>`. See the "Fallback decision RESOLVED" section below.

## Strong cross-session recommendation → owner (upstream of aira)
deploy + fly + qual + speed all point at the same real fleet-wide fix: the HARNESS/launcher injecting `AIRA_CONFINE_OWNER=<friendly-name>` into each session's env, which `aira confine` already auto-reads. aira CANNOT do the injection (the name isn't in the env and the registry is the harness's). Without it the column is mostly `—`/handles. Raise with whoever owns session launching.

## FINALIZED design (grounded in live data, 2026-09-19 — build)
Grounded the actual owner shapes that reach `ConfineRecord.Owner` (resolveOwnerIn, main.go): (1) an ATTESTED human name from `--owner`/`AIRA_CONFINE_OWNER` (e.g. `claude-stoner`); (2) `project.WorktreeID` = `hashID(gitDir)` = `hex(sha256)` = EXACTLY 64 lowercase hex chars (confirmed: `internal/app/project.go:195,850` + live `confine --list` showed `4f9ec70c…8d7aa`, `37d4080e…e3c6`); (3) `InferConfineOwner` = `@cwd-<sanitized-dir-basename>` (short, HUMAN-READABLE, `@`-prefixed to mark it un-attested — confine.go:736); (4) `ConfineUnknownOwner` = `"unknown"`.

**`topSessionCell(record)` (cmd/aira/tui_top.go), three arms:**
- **64-hex WorktreeID hash** (`isWorktreeHash` = `len==64 && all-lowercase-hex`, precise match to sha256 — NOT a heuristic): show the **8-char prefix** (`4f9ec70c`). This is the AIRA-135 fix — the full hex crowded the table off screen — and the prefix GROUPS a worktree's un-annotated jobs (two jobs sharing a worktree share a prefix; the PID column can't show that grouping). Within owner's approved "short scope-id / PID".
- **empty or `unknown`**: `#<supervisor-pid>` (honest, never fabricated, never blank — satisfies subpipe). Rare (resolveOwnerIn almost always yields a name / worktree-hash / @cwd).
- **otherwise** (attested human name, OR `@cwd-<dir>` inferred hint): show the owner **VERBATIM**. Both are short + human-readable; the `@` stays on screen so an inferred owner is not mistaken for a claimed session. **No truncation in the viewmodel** — the table (tview) clamps to terminal width; tui_top_test.go:843 pins the "viewmodel never pre-truncates" invariant, so a cap here would violate it. A user who sets a long `AIRA_CONFINE_OWNER` gets what they chose, clamped by the table.

**Column placement:** SESSION inserted **before COMMAND** (COMMAND stays last — it is the unbounded-width cell that absorbs the clamp). Headers + row cells at tui_top.go:613/662. Updates the index-based `wantCells` test (tui_top_test.go:838) + the header-order/dropped-columns assertions in the same test.

**SKILL sharpening** (skill.go:318 already documents `export AIRA_CONFINE_OWNER=<stable-session-id>`): add that it is what surfaces the session in the new `aira top` **SESSION** column (and `confine --list`), so agents know WHY to set it. MCP confine descriptor likewise if it carries the same guidance. skill_test.go pin for the new phrase.

**Release:** client-only v0.20 (top + skill are client-side; daemon unchanged). 6 live confine scopes on the box at build time → **NO daemon restart** (would blip peers). SKILL needs a separate `aira skill install`.

## Fallback decision RESOLVED (was "decision needed at build")
Chose a hybrid of the two options: NOT a bare `—` (subpipe's ask honoured) and NOT fabricated. Hex → 8-char grouping prefix; empty/unknown → PID handle; `@cwd-` shown through (readable + honest). This is within the owner's AskUserQuestion answer ("Short scope-id / PID") for the unset cell.

## Fable review — APPROVE-WITH-NITS (2026-09-19)
14/15 mutations red; tree left pristine at db79db1. No false-pass/false-fail/honesty regression. Addressed: (1) REQUIRED — the columns test's hash-prefix pin was porous (owner `9f3ac1de`×8 coincided with the scope-id's own last 8 bytes, so a mutation rendering `ScopeID[-8:]` PASSED, measured M16). Fixed: owner → `4f9ec70c`×8 (decoupled from the scope id), expected prefix `4f9ec70c`; M16 re-confirmed RED then reverted. (2) SKILL prose accuracy — the "worktree-hash prefix / bare PID" fallback describes `aira top` only, not `--list`; reworded. (3) ticket design sections reconciled (early '—'/truncate sketch marked SUPERSEDED by FINALIZED). Kept by design, with reasoning: the 8-char hash prefix (Fable finding 2 — a better handle than a literal scope-id/PID: groups a worktree's jobs incl. aitest workers, kills the AIRA-135 hex; within owner's "short scope-id / PID"); the `#<pid>` arm (declined Fable's simplify-to-`unknown` — the owner explicitly chose PID over blank/—, subpipe explicitly asked non-blank). ACCEPTED gap (Fable finding 3, low): the CLI-only `--owner` help description (enriched) has no regression pin — `confine` isn't an MCP tool, only generated CLI help; the primary agent surface (SKILL) IS pinned. Fable ran the per-package command, not `make ci`; the builder ran the full `aira confine -- make ci` gate (MAKE_EXIT=0) before commit.

## Release hand-off to deploy (owner 2026-09-19)
On release, notify deploy so it adopts the feature across fastest.ee tooling. Owner confirmed the MECHANISM is the SESSION NAME via the ENV VAR (`AIRA_CONFINE_OWNER`), not per-call flags: a session `export`s it ONCE and it propagates to every descendant `aira confine`, including scripted calls — so deploy sets it once in the fastest.ee tooling environment rather than editing every call site (some may already set it). Jobs then show that name in the `SESSION` column of `aira top`. The MCP/SKILL update (this ticket) is what steers all sessions to start doing this. (Distinct from the still-open, separate `--name`-per-JOB-in-CI-traces offer — the owner's instruction here is the session/owner identity, not a per-job task label.)
