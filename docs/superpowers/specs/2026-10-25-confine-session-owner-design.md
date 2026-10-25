# AIRA-313 — a Claude Code session owns its confine jobs, wherever it runs from

Ticket: AIRA-313 (bug, P2, v0.40). Rants: RANT-48, -49, -50, -51, -53.
Related: AIRA-23 / AIRA-52 (owner chain, owner in the scope id), AIRA-302
(retired: `@cwd` refusal is by design), AIRA-265 / AIRA-266 / AIRA-277 (SESSION
column, owner steer, tmux-window fallback).
Correctness-critical (ownership guard): two-loop review.

## Problem, restated

`aira confine --kill/--front/--resize` refuse a session's OWN job.

With no `--owner` and no `AIRA_CONFINE_OWNER`, the job's owner (at launch) and
the caller (at kill) are both the git worktree id of the process's CURRENT
directory (`cmd/aira/main.go:resolveOwnerIn` -> `app.Discover(".").WorktreeID`,
sha256 of that worktree's own git dir). An agent's current directory is not
stable: subagent Bash calls reset their cwd, `cd wt && aira confine ... &` runs
the launch somewhere else, and sessions move between the repo root and feature
worktrees. So one session presents two identities and `CheckConfineOwner`
refuses it. Reproduced by the investigator (launch in a linked worktree, `--kill`
from the repo root -> `E_CONFINE_OWNER_UNVERIFIED owner=<A> caller=<B>`; from a
subdirectory of the launch worktree -> killed). RANT-50/51/53 are exactly that
shape; RANT-49 adds "the coordinator cannot kill its subagents' jobs".

The same per-directory identity is also a false pass: every session whose cwd is
the same worktree (in practice the repo root on master, where sessions start)
shares one ATTESTED owner and can kill the others' jobs without `--steal`. That
is the AIRA-23 hazard ("two sessions in one directory") that AIRA-23 used to
justify keeping `@cwd` unattested.

The refusal text names two hashes and "pass --steal" and never says why or what
to do instead.

The right identity is the agent SESSION, not the directory. Claude Code already
gives every session one: `CLAUDE_CODE_SESSION_ID`, a UUID.

Measured on this box (2026-10-25, read from `/proc/<pid>/environ`):

* This workflow subagent's Bash env has `CLAUDE_CODE_SESSION_ID=b6b9daa7-...`,
  the same as its coordinator (`CLAUDE_CODE_CHILD_SESSION=1`, same
  `CLAUDE_PID`). Subagents inherit the parent session's id.
* All 12 live `aira mcp` servers carry `CLAUDE_CODE_SESSION_ID`, each a
  different value, and the server under this session's claude process
  (pid 2148926, parent 2148498) carries `b6b9daa7-...`, the same as the Bash
  env. So the MCP face sees the same id as the CLI face of the same session.
* The `claude` processes' own exec environ does NOT carry it. Claude Code injects
  it into each child when the child is spawned.

## Greenfield minimum

One identity per agent session, read the same way at launch and at kill, plus a
refusal that says why. Concretely: in the default owner chain, after the
explicit `--owner` / `AIRA_CONFINE_OWNER` and before the worktree id, use
`cc-<CLAUDE_CODE_SESSION_ID>` when that variable holds a valid session id. Then
make `CheckConfineOwner`'s refusal explain itself.

### Gap between the minimum and this plan, each with a live justification

| Part beyond the minimum | Why it is needed now |
|---|---|
| Strict shape check on the session id (canonical lowercase UUID), with fall-through to the old chain | Requirement (e): an empty, padded, oversized or odd value must never error or panic, and must behave exactly like today. The exact shape also gives `aira top` an exact recognizer (below) instead of a "starts with cc-" heuristic that would swallow a human owner named `cc-deploy`. |
| `aira top` SESSION cell treats a `cc-<uuid>` owner like a worktree hash (tmux window name first, else `cc-` + 8 hex) | Without it every Claude-launched job shows a 39-character id in SESSION instead of the tmux window name, undoing AIRA-277 for the most common launcher on this box. One condition in `topSessionCell`. |
| Skill text in `internal/core/skill.go` | The current text says the default is the worktree and that only `AIRA_CONFINE_OWNER` gives an attested owner. After this change that is false, and the skill is the only place agents learn the owner rules. |
| `TestMain` in `cmd/aira` that unsets `CLAUDE_CODE_SESSION_ID` | The gate is run from inside Claude sessions, where the variable is set; GitHub CI has none. Without the scrub every existing owner-chain test (e.g. `TestConfineOwnerDerivationChain`, the `tui_top_*` owner tests) changes result depending on who runs it. Same pattern as `internal/app/git_env_test.go`'s `GIT_*` scrub (AIRA-93). New tests set the variable explicitly with `t.Setenv`. |
| One docs-only sentence in the skill: `--kill` acts on running jobs; a queued job is stopped by ending its own `aira confine` process | RANT-49's `E_CONFINE_NOT_FOUND` (see "RANT-49 finding") is that case. It states current behaviour; no mechanism. |

Nothing else is added. The ownership rule itself (`CheckConfineOwner`: both
sides attested and equal, or `--steal`) does not change.

## Scope

1. New owner-chain step, ONLY in `resolveConfineOwner` (launch, management,
   `--status`, `confine-log`, `confine-input`, `--dump`, `drain`, `aira top`'s
   own-owner, and the MCP confine faces all call it). `resolveOwnerIn` (used by
   `worktree register` with an explicit directory) is NOT changed: a worktree
   binding is per checkout by design (AIRA-176).
2. `CheckConfineOwner` refusal text distinguishes three cases and says what to do.
   It stays the ONE guard for `--kill` (real and ci-shim), `--front`, `--resize`.
3. `aira top` SESSION cell for a session owner.
4. Skill text.
5. Test environment scrub.

## Design

### Owner chain (new order)

1. `--owner <id>` (validated; invalid -> `E_CONFINE_ARGUMENT_INVALID`, unchanged)
2. `AIRA_CONFINE_OWNER` (trimmed; invalid -> error, unchanged)
3. **NEW:** `CLAUDE_CODE_SESSION_ID`, trimmed. If it is exactly a canonical
   lowercase UUID (`8-4-4-4-12` of `[0-9a-f]`), owner = `"cc-" + id`. Anything
   else (unset, empty, whitespace only, wrong length, uppercase, any other
   character) is ignored and the chain continues. Never an error.
4. discovered project worktree id (unchanged)
5. inferred `@cwd-<basename>` or `unknown` (unchanged)

Placement, to keep `resolveOwnerIn` byte-identical:

```go
func resolveConfineOwner(ctx context.Context, explicit string) (string, error) {
	if explicit == "" && strings.TrimSpace(os.Getenv("AIRA_CONFINE_OWNER")) == "" {
		if owner, ok := runner.ConfineSessionOwner(os.Getenv("CLAUDE_CODE_SESSION_ID")); ok {
			return owner, nil
		}
	}
	return resolveOwnerIn(ctx, explicit, "")
}
```

`runner.ConfineSessionOwner(raw string) (string, bool)` and
`runner.IsConfineSessionOwner(owner string) bool` live in the portable
`internal/runner/confine.go` next to `InferConfineOwner`, with
`const ConfineSessionOwnerPrefix = "cc-"`. `IsConfineSessionOwner` is true only
for `"cc-"` + a canonical lowercase UUID. `ConfineSessionOwner` also asserts its
result passes `ValidateConfineIdentity` and `ValidateConfineOwner` (it does by
construction; the assert makes a future prefix edit fail loudly in tests).

A `cc-` owner is ATTESTED (`ConfineOwnerIsAttested` is true: no `@`). This is
correct: "attested" in AIRA-23's sense means "an identity that tells sessions
apart", which a per-session UUID does and a per-directory inference does not.

### Refusal text (three cases)

`CheckConfineOwner(scopeID, owner, callerOwner, steal)` keeps its signature, its
verdict, and its leading `E_CONFINE_OWNER_UNVERIFIED: scope=%s owner=%s caller=%s`
fields (existing callers and greps key on them). It appends one reason, chosen in
this order:

1. **Job owner unattested** (`@...`, `unknown`, empty): "the job's owner was
   inferred from its launch directory (or is unknown), which never proves
   ownership, even to a caller in the same directory; if it is your job, pass
   --steal. Launch with AIRA_CONFINE_OWNER=<name> (or from a Claude Code session)
   to get an owner you can manage without --steal."
2. **Caller unattested**: "your own identity is inferred (no --owner,
   AIRA_CONFINE_OWNER, Claude Code session or aira project worktree here), so it
   cannot match an owner; run this from the session or worktree that launched the
   job, or with the AIRA_CONFINE_OWNER it was launched with, or pass --steal."
3. **Both attested, different**: "the job belongs to a different session or
   worktree. With no --owner or AIRA_CONFINE_OWNER, an owner defaults to the
   Claude Code session that launched the job (cc-<session id>, shared with its
   subagents), else to the git worktree of the directory it was launched from. If
   it is yours, run this from that session or worktree, or with the same
   --owner/AIRA_CONFINE_OWNER; launching with AIRA_CONFINE_OWNER=<name> avoids
   this. Otherwise --steal acts on another session's job deliberately."

Exact wording is the builder's; the tests pin the case-distinguishing phrases
(not whole sentences). A launch-directory hint (`record.Cwd`) is deferred (see
Deferrals).

### `aira top` SESSION cell

In `cmd/aira/tui_top.go:topSessionCell`:

* `chosen := runner.ConfineOwnerIsAttested(owner) && !isWorktreeHash(owner) && !runner.IsConfineSessionOwner(owner)`,
  so a session owner falls to the tmux window name when the job has one (AIRA-277).
* With no pane label, a session owner renders as `"cc-" + first 8 hex`
  (e.g. `cc-b6b9daa7`): short, groups one session's jobs, and visibly not a
  worktree hash.

`aira top`'s own owner (`cmd/aira/tui.go`, resolved once via
`resolveConfineOwner`) follows the chain: a human running `aira top` in a plain
tmux pane has no session id, so Claude-launched jobs are foreign there and `f` /
`e` show the existing `--steal` confirmation. That is intended (it is the
shared-worktree false pass closed).

### Skill text (`internal/core/skill.go`, the confine paragraph)

Replace "Set `AIRA_CONFINE_OWNER` to get a real, attested owner you can kill
without `--steal`" and the SESSION-fallback parenthesis with:

* the default chain: `--owner` / `AIRA_CONFINE_OWNER`, else your Claude Code
  session (`cc-<session id>`, shared by your subagents, so from any directory you
  can manage your own and your subagents' jobs), else the git worktree of the
  CURRENT directory, else an inferred `@cwd-...` owner;
* outside a Claude session, a job launched from one worktree is not yours when
  you run `--kill` from another: run it from the launch worktree or set the owner
  inline;
* a job launched by another Claude session (or by this conversation before
  `/clear` or a resume, if the session id changed) needs `--steal` or its own
  owner name; a detached job you will manage from a LATER session should be
  launched with an `AIRA_CONFINE_OWNER` name or have its scope id kept;
* the `@` rule unchanged (still "never counts as ownership");
* `--kill` acts on running jobs only; a queued job holds nothing and is stopped
  by ending its own `aira confine` process.

## Invariants

* I1. With `CLAUDE_CODE_SESSION_ID` unset or invalid, every resolved owner is
  byte-identical to today's.
* I2. `--owner` and `AIRA_CONFINE_OWNER` always win over the session id; an
  invalid `AIRA_CONFINE_OWNER` still errors (the session id does not mask it).
* I3. `worktree register`'s owner never depends on `CLAUDE_CODE_SESSION_ID`.
* I4. The guard verdict is unchanged as a function of `(owner, caller, steal)`;
  only the inputs change. `CheckConfineOwner` stays the single guard for kill
  (real + shim), front and resize.
* I5. Two different valid session ids never resolve to the same owner;
  one session id resolves to the same owner from any directory.
* I6. An inferred owner (`@...`) is still never attested and still unforgeable.
* I7. A `cc-` owner round-trips through the scope id (`MintConfineScopeID`,
  `MintWorkerScopeID`, `ParseConfineScopeID`, `bindConfineScopeID`) unchanged.

## Code touch points (opened and checked on `origin/master` ff03d8aa)

| File:function | Change |
|---|---|
| `cmd/aira/main.go:resolveConfineOwner` (line 2273) | new session step before delegating to `resolveOwnerIn` |
| `cmd/aira/main.go:resolveOwnerIn` (2286) | unchanged (worktree register via `cmd/aira/worktree.go:stampWorktreeOwner`, line 54) |
| `internal/runner/confine.go` (near `InferConfineOwner`, 811; `ConfineOwnerIsAttested`, 836) | add `ConfineSessionOwnerPrefix`, `ConfineSessionOwner`, `IsConfineSessionOwner` |
| `internal/runner/confine_manage.go:CheckConfineOwner` (68) | three-case reason; verdict unchanged |
| `cmd/aira/tui_top.go:topSessionCell` (1030) | session-owner arm |
| `internal/core/skill.go` (line 320 paragraph) | text |
| `internal/core/skill_test.go` | pins for the new phrases |
| `cmd/aira/main_test.go` | `TestMain` scrubbing `CLAUDE_CODE_SESSION_ID` (no `TestMain` exists in `cmd/aira` today) |
| `internal/daemon/main_test.go:TestMain` (24) and any other package whose tests exec the built `aira` binary and assert an owner | same scrub, only where the builder finds such a test (`grep -rn 'buildAiraBinary\|"aira", "confine"'`) |

Unchanged but on the path, checked:

* `cmd/aira/mcp_project.go` lines 63 and 77: MCP confine faces call
  `resolveConfineOwner` in the MCP server process; the server inherits the
  session's id at spawn (measured above).
* `internal/runner/confine_detach.go:confineDetachOwnerMatches` (254),
  `ResolveConfineDetachStatus` (314): string equality against the CLI-resolved
  caller; `confine-input` dials the run-input server with the same resolved owner
  (`internal/runner/confine_input_linux.go:25,33`). All receive the SAME
  `resolveConfineOwner` value, so they gain the cwd independence too.
* `internal/runner/confine.go`: `maxConfineOwnerLen = 64`; `cc-` + 36 = 39.
  `ValidateConfineIdentity` alphabet `[A-Za-z0-9._-]` covers it. The scope id
  splits at the FIRST `@` and `cc-<uuid>` has none. Worst-case scope directory
  `.aira-CONFINE-<name 100>-<pid 7>-<stamp 13>@<owner 39>` = 176 bytes < 255.
* `internal/runner/confine_queue_position_linux.go:350`: `ValidateConfineOwner`
  passes; the daemon's `confineManagement` validates the caller the same way
  (`internal/daemon/confine_manage.go:157`).
* `MintWorkerScopeID` (`confine.go:1543`) copies the parent owner; the daemon
  checks it against the parent (`internal/daemon/admit.go:3347`). A worker of a
  `cc-` job is owned by `cc-`.
* Reserve history is keyed by `runner.ResourceSignature(argv)`, not owner, so
  learned reservations are not reset by the owner change. `confine --budget` does
  not filter by owner (`internal/daemon/confine_budget.go:21-29`).

## Attack surface (false pass / false fail)

### False pass: can one session act on another's job without `--steal`?

| Case | Result | Note |
|---|---|---|
| Two Claude sessions, distinct ids, same worktree (repo root) | **refused** (today: allowed) | The shared-root hole closes for Claude sessions. Test T3. |
| Two Claude sessions, distinct ids, different worktrees | refused | as today |
| Coordinator and its subagents (same id) | allowed | Intended unit: fixes RANT-49 part 2. Consequence: one subagent can kill a sibling subagent's job. Accepted (same conversation). |
| Non-Claude shells sharing a worktree | allowed (unchanged) | Residual false pass; deleting the worktree default is deferred. |
| Process forging `--owner cc-<other id>` | allowed | Same as copying a worktree hash today. The guard prevents accidents, it does not authenticate (AIRA-23, `ValidateConfineIdentity` doc). Not made unforgeable, because `--owner cc-<old id>` is also the legitimate recovery path for a session whose id changed. |
| A non-Claude tool (codex, make, a script) run from a Claude Bash call | owns as that session | It inherits the id. It is that session's work. |
| A nested `claude` started from a Claude Bash call | assumed to inject its own id into its children (not verified) | If it inherits instead, the nested session shares the parent's identity: same human-launched tree. Either way no unrelated session is opened. |
| A stale MCP server id (see below) | can only match jobs of the same claude process | Each claude process spawns its own stdio MCP server; ids are never shared across processes. |

### False fail: is a session refused on its own job?

| Case | Result | Disposition |
|---|---|---|
| Same session, launch and kill from different cwds | **allowed** (today: refused) | the fix; tests T3, T11 |
| Coordinator kills its subagent's job | allowed | |
| `--status` / `confine-log` / `confine-input` by name from another cwd, same session | **found** (today: "matched other owners") | same resolved caller; test T10 |
| CLI-launched job managed over MCP, same session | allowed | MCP server carries the same id (measured); test T11 (iii) |
| After `/clear` or `--resume`, if Claude Code issues a new session id | refused, needs `--steal` or `--owner cc-<old id>` | Not verified whether the id changes. Fail-safe direction. The MCP server keeps its spawn-time id, so after an id change the CLI and MCP faces of one conversation can disagree; also fail-safe. Documented in the skill. |
| A detached job managed from a LATER session | refused by name; `--status` with no selector no longer lists it | Today this worked if the later session sat in the same worktree. Remedy: launch long detached jobs with `AIRA_CONFINE_OWNER=<name>`, or keep the scope id (a scope-id selector always matches in `--status`). Stated in the skill and in refusal case 3. |
| Jobs launched before the upgrade (owner = worktree id), killed after it by a Claude session | refused (case 3 text) | One-off transition; jobs are short-lived; `--steal` or `--owner <hash>`. |
| A human in a plain tmux pane (no id) managing Claude-launched jobs in the same worktree | refused (today: allowed) | Intended; this was the false pass. `aira top` already offers a `--steal` confirmation. |
| Session sets `AIRA_CONFINE_OWNER` at launch but not at kill | refused | Unchanged in kind (was a worktree id, now a `cc-` id); case 3 text names the cause. |
| Harness renames or reformats `CLAUDE_CODE_SESSION_ID` | the step goes silent, old behaviour | No worse than today. |

### Scope-id encoding

Checked under "Unchanged but on the path". Test T6 pins the round trip and the
worst-case directory length.

### Invalid session ids

`""`, `"   "`, a valid UUID with surrounding whitespace (accepted after trim),
uppercase UUID, 35/37 characters, a valid UUID plus `x`, `../x`, a value with `@`,
`/`, a NUL or a newline, a 200-character value: all fall through, `err == nil`,
no panic. Test T1.

## RANT-49 finding: `E_CONFINE_NOT_FOUND` is not an owner problem

`killConfineWithDeps` (`internal/runner/confine_manage_linux.go:733`) matches the
selector only against `listed.Scopes`, with no owner filter. A QUEUED job has no
scope directory yet: the daemon lists it separately in
`ConfineListResult.Queued` (`internal/daemon/confine_manage.go:queuedConfineRows`,
which drops ids already live). So `--kill <name>` on a job that is still queued,
or that has already exited, returns exactly
`E_CONFINE_NOT_FOUND: selector "<name>" matched no confine scope`. The rant's
box was contended at the time (the investigator saw 5 queued, freeze hold), so a
queued job is the likely cause. This is established by reading the code; the
incident itself is **unreproduced** (no record of the job survives). It is
behaviour by design (a kill needs a populated scope to prove empty), not a bug,
so no mechanism changes here. The skill gains one sentence; a friendlier
not-found message ("N queued jobs match; end the job's own `aira confine`
process") is deferred.

## Test plan

Every new test must be shown red against the wrong implementation (the mutation
named) and green against the fix; the commit message records each mutation run.

| # | Test (file) | What it pins | Mutation that must turn it red |
|---|---|---|---|
| T1 | `TestConfineOwnerClaudeSessionStep` (`cmd/aira/confine_test.go`) | valid id -> `cc-<id>`, attested; `--owner` and `AIRA_CONFINE_OWNER` win; invalid `AIRA_CONFINE_OWNER` still errors with a valid session id set; every invalid session id from the list above falls through to the worktree id (inside a project) and to `@cwd-...` (outside), `err == nil`; padded valid id accepted | (a) delete the step; (b) move the step above `AIRA_CONFINE_OWNER`; (c) accept any non-empty value; (d) return an error on an invalid value |
| T2 | `TestWorktreeRegisterOwnerIgnoresClaudeSession` (`cmd/aira/worktree_test.go`) | with the id set, `stampWorktreeOwner` records the worktree id | add the step to `resolveOwnerIn` |
| T3 | `TestConfineSessionOwnerIsCwdIndependentAndSessionDistinct` (`cmd/aira/confine_test.go`) | real repo + `git worktree add` (both with `.aira/config`, so the worktree ids are attested and distinct): same id resolved in A and B -> equal, `CheckConfineOwner` passes; ids X and Y in the SAME worktree -> `CheckConfineOwner` refuses without steal, passes with steal | delete the step (A != B and X == Y both flip) |
| T4 | `TestCheckConfineOwnerRefusalSaysWhy` (`internal/runner/confine_manage_test.go`) | three cases each carry the code, `scope=`, `owner=`, `caller=` and their own distinguishing phrase; case 3 names `AIRA_CONFINE_OWNER`, "session" and "worktree"; owner-and-caller-both-unattested reports case 1; `steal` -> nil in all three | restore the one-line message; swap case 2 and 3 texts |
| T5 | `TestCheckConfineOwnerVerdictWithSessionOwners` (`internal/runner/confine_manage_test.go`; no direct `CheckConfineOwner` unit test exists today, only the kill-path `TestConfineKillRefusesAnInferredOwnerWithoutSteal`) | verdict over `(owner, caller, steal)` with `cc-` values, worktree hashes, `@cwd`, `unknown`, `""` | drop the attestation test on either side (`owner == caller` only) |
| T6 | `TestConfineScopeIDRoundTripsSessionOwner` (`internal/runner/confine_scope_bind_test.go` or neighbour) | `MintConfineScopeID` and `MintWorkerScopeID` with `cc-<uuid>` -> `ParseConfineScopeID` returns it; `bindConfineScopeID` accepts; `.aira-` + id with a 100-char name is <= 255 bytes | set `maxConfineOwnerLen` to 38 |
| T7 | `TestTopSessionCellClaudeSessionOwner` (`cmd/aira/tui_top_test.go`) | `cc-<uuid>` + pane `deploy` -> `deploy`; no pane -> `cc-` + 8 hex; human owner `cc-deploy` + pane -> `cc-deploy` (a chosen name beats the pane) | (a) drop the recognizer from `chosen`; (b) render the session owner verbatim; (c) recognize by `HasPrefix("cc-")` only |
| T8 | `TestIsConfineSessionOwnerIsExact` (`internal/runner/confine_manage_test.go` or neighbour) | exact shape only | loosen to prefix or length check |
| T9 | `internal/core/skill_test.go` pins | new phrases present (session default, subagents, queued-kill sentence); the `@`-rule phrase still present; the removed sentence absent | revert `skill.go` |
| T10 | `TestConfineStatusSameSessionAcrossWorktrees` (`cmd/aira/confine_detach_test.go` or neighbour) | through the CLI `--status <name>` entry (`runConfineStatusCommand`), with the record's owner produced by `resolveConfineOwner` in worktree A (and the record written by the runner's real detach writer where that tier is reachable from `cmd/aira`; otherwise via the `confineDetachStatusList` seam capturing the caller the CLI passes): from worktree B with the same id -> found; with a different id -> `matched other owners` | delete the step |
| T11 | `TestConfineKillSameClaudeSessionFromAnotherWorktree` (`cmd/aira/dispatcher_inprocess_test.go`, the real-cgroup tier of `TestMCPConfineKillOutsideProjectKeepsOwnershipAndStealChecks`) | private daemon (`XDG_STATE_HOME`, `XDG_RUNTIME_DIR` under the test dir), installed helper as `SelfPath`; `CLAUDE_CODE_SESSION_ID=X`, `AIRA_CONFINE_OWNER` unset, cwd = worktree A; launch through the CLI entry `runWithInput(["confine","--name",n,"--","sleep","60"])` with `runConfined` wrapped only to set `SelfPath` / private paths (it asserts `request.Owner == "cc-X"` and never sets it); wait for the placed state; `chdir` to worktree B; (i) id Y -> CLI `confine --kill n` -> `E_CONFINE_OWNER_UNVERIFIED` with the case-3 phrase; (ii) id X -> killed; (iii) a second job, killed over MCP (`runMCPWithDispatcher`) from B with id X -> killed. Skips through `cgrouptest.SkipOrFailRealCgroup` like its neighbour | delete the step ((ii) and (iii) refuse) |

Gate: `aira confine -- make ci` from the worktree, run inside a Claude session
(so `CLAUDE_CODE_SESSION_ID` is set: the adversarial environment for the scrub),
plus `env -u CLAUDE_CODE_SESSION_ID aira confine -- go test ./cmd/aira/...` (the
CI environment). Both exit codes recorded.

## Risks

* **Coupling to a Claude Code variable.** If it is renamed, dropped or reformatted,
  the step goes silent and behaviour returns to today's. No worse failure.
* **Test environment leakage.** Mitigated by the `TestMain` scrub and by running
  the gate both with and without the variable.
* **Behaviour change for shared-worktree workflows.** A second session (or a human
  pane) that used to clean up a root-launched Claude job now needs `--steal`.
  Intended; stated in the skill and the refusal.
* **Detached jobs across sessions** lose name-based `--status` / `--kill` from a
  later session. Mitigated by the skill guidance (owner name or scope id).
* **Session id change on `/clear` / resume** (not verified): fail-safe refusals.

## Expected yield

* The five rants' failure class (same session, different cwd; coordinator vs
  subagent) disappears for Claude sessions, which launch nearly every confined job
  on this box (12 live Claude MCP servers measured, each with a distinct id).
* The shared-repo-root false pass closes for Claude sessions.
* The remaining refusals (non-Claude shells, foreign sessions, transition) say why
  and what to do, so the next RANT-50 does not need an investigator.

## Deferrals (not built, and why)

* **Delete the worktree-id default** (option D). With the session step in place it
  is the source of the remaining non-Claude false pass and false fail, but removing
  it pushes every non-Claude shell to `@cwd` + `--steal`. Separate decision.
* **Launch-directory hint in the refusal** (`record.Cwd`). Adds a parameter to the
  one guard at four call sites; after this change the cwd mismatch is mostly a
  non-Claude case. Revisit if rants continue.
* **Friendlier `E_CONFINE_NOT_FOUND` for queued jobs** (RANT-49): message-only
  follow-up, worth a ticket; not an owner bug.
* **Making `cc-<uuid>` unforgeable via `--owner`**: it would block the legitimate
  recovery path; the guard is not authentication.
* **`aira run` / `run-kill` ownership** (`internal/runner/runner_linux.go`,
  `r.owner`): a separate identity, not part of the confine guard. Not touched.
* **Mapping the session id to a friendly name** (AIRA-265 rejected it: the registry
  is the harness's). Display uses the tmux window name instead.
* **The investigator's side observation** that a job killed by its owner reports
  `terminated-by=unattributed-sigkill` naming "another session's kill" as a
  candidate: unrelated to owner resolution; worth its own rant/ticket.
