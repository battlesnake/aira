# AIRA-313 — a Claude Code session owns its confine jobs, wherever it runs from

Ticket: AIRA-313 (bug, P2, v0.40). Rants: RANT-48, -49, -50, -51, -53.
Related: AIRA-23 / AIRA-52 (owner chain, owner in the scope id), AIRA-302
(retired: `@cwd` refusal is by design), AIRA-265 / AIRA-266 / AIRA-277 (SESSION
column, owner steer, tmux-window fallback).
Correctness-critical (ownership guard): two-loop review.

Revision 2 (2026-10-25): answers the plan review (Sol) and plan gate (Fable),
both REVISE. Changes: the session-id-change and MCP rows are corrected from a
measurement and the MCP case is recorded as a named, accepted gap (G1); the
refusal text no longer suggests `--owner` as a remedy anywhere; T11 gains
mandatory real-cgroup gate evidence; T10 uses the real status reader; T1/T5/T8
wording fixed; RANT-49 softened; two missed text touch points added; the runtime
assert and the `internal/daemon` scrub are removed.

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
is the AIRA-23 hazard ("two sessions in one directory").

The refusal text names two hashes and "pass --steal" and never says why or what
to do instead.

The right identity is the agent SESSION, not the directory. Claude Code already
gives every session one: `CLAUDE_CODE_SESSION_ID`, a UUID.

Measured on this box (2026-10-25):

* From `/proc/<pid>/environ`: this workflow subagent's Bash env has
  `CLAUDE_CODE_SESSION_ID=b6b9daa7-...`, the same as its coordinator
  (`CLAUDE_CODE_CHILD_SESSION=1`, same `CLAUDE_PID`). Subagents inherit the
  parent session's id.
* All live `aira mcp` servers carry `CLAUDE_CODE_SESSION_ID`, each a different
  value. The plan gate re-measured 13 of 13 servers agreeing with their parent
  claude process's current id in `~/.claude/sessions/<pid>.json`. So the MCP face
  and the CLI face of one claude process agree, until the id changes (below).
* The `claude` processes' own exec environ does NOT carry it. Claude Code injects
  it into each child when the child is spawned.
* **The id does change.** In `~/.claude/history.jsonl` (18602 rows, sorted by
  timestamp; reproduction script below), for each `/clear` or `/resume` entry the
  next row in the same project carries a different `sessionId` in 16 of 16
  `/clear` cases and 139 of 148 `/resume` cases (3 had no next row). Heuristic
  caveat: the next row in a project may come from a concurrent session, but the
  16/16 rate leaves no doubt that `/clear` issues a new id. And `claude --resume
  X` in another process makes that process present id X again, so **one id can be
  live in two processes at once**.
* **Not verified:** whether a running claude process's Bash tool env picks up the
  new id after `/clear` (no `/clear` on a live process was observed). Either way
  the CLI resolves whatever its env holds.

Reproduction of the history measurement (run with `python3 -I <script>
~/.claude/history.jsonl`): load every row, sort by `timestamp`; for each row whose
trimmed `display` is the command or starts with it plus a space, find the next
row with the same `project` and compare `sessionId`. The script used is
`~/tmp/aira-313/idchange.py` (scratch, not committed: it reads private harness
data, not repo data).

## Greenfield minimum

One identity per agent session, read the same way at launch and at kill, plus a
refusal that says why. Concretely: in the default owner chain, after the
explicit `--owner` / `AIRA_CONFINE_OWNER` and before the worktree id, use
`cc-<CLAUDE_CODE_SESSION_ID>` when that variable holds a valid session id. Then
make `CheckConfineOwner`'s refusal explain itself.

### Gap between the minimum and this plan, each with a live justification

| Part beyond the minimum | Why it is needed now |
|---|---|
| Strict shape check on the session id (canonical lowercase UUID), with fall-through to the old chain | Requirement (e): an empty, padded, oversized or odd value must never error or panic, and must behave exactly like today. The exact shape also gives `aira top` an exact recognizer instead of a "starts with cc-" heuristic that would swallow a human owner named `cc-deploy`. |
| `aira top` SESSION cell treats a `cc-<uuid>` owner like a worktree hash (tmux window name first, else `cc-` + 8 hex) | Without it every Claude-launched job shows a 39-character id in SESSION instead of the tmux window name, undoing AIRA-277 for the most common launcher on this box. One condition in `topSessionCell`. |
| Skill text (`internal/core/skill.go`) and the generated `owner` flag text (`internal/core/core.go`) and two stale doc comments | The current texts say the default is the worktree and that only `AIRA_CONFINE_OWNER` gives an attested owner. After this change that is false, and the skill and generated help/MCP schema are where agents learn the owner rules. Text only. |
| `TestMain` in `cmd/aira` that unsets `CLAUDE_CODE_SESSION_ID` | The gate is run from inside Claude sessions, where the variable is set; GitHub CI has none. Without the scrub `TestConfineOwnerDerivationChain` (`cmd/aira/confine_test.go:83`, asserts the worktree id with `AIRA_CONFINE_OWNER` empty) fails whenever the suite runs inside a Claude session. Same pattern as `internal/app/git_env_test.go`'s `GIT_*` scrub (AIRA-93). New tests set the variable explicitly with `t.Setenv`. |
| Skill sentences: the G1 gap, and how to end a queued job | G1 is an accepted gap and must be written where agents read it. The queued-job sentence answers the RANT-49 `E_CONFINE_NOT_FOUND` half (see "RANT-49 finding"). Text only. |

Removed from revision 1 (no live justification): the runtime assert inside
`ConfineSessionOwner` (validity is by construction and pinned by T6/T8, so it was
a production branch for a test-time property), and the `internal/daemon` TestMain
scrub (the three daemon tests that build the `aira` binary were checked:
`worker_admit_cli_granted_linux_test.go`, `worker_admit_cli_boundary_test.go`,
`confine_reserve_wait_bound_linux_test.go`; only the last passes an owner and it
is explicit, `aira108`; none found that depends on the env-less chain, so none
added).

Nothing else is added. The ownership rule itself (`CheckConfineOwner`: both
sides attested and equal, or `--steal`) does not change.

## Scope

1. New owner-chain step, ONLY in `resolveConfineOwner` (launch, management,
   `--status`, `confine-log`, `confine-input`, `--dump`, `drain`, `aira top`'s
   own owner, and the MCP confine faces all call it). `resolveOwnerIn` (used by
   `worktree register` with an explicit directory) is NOT changed: a worktree
   binding is per checkout by design (AIRA-176).
2. `CheckConfineOwner` refusal text distinguishes three cases and says what to do.
   It stays the ONE guard for `--kill` (real and ci-shim), `--front`, `--resize`.
3. `aira top` SESSION cell for a session owner.
4. Skill text, generated owner-flag text, two doc comments.
5. Test environment scrub in `cmd/aira`.

## Design

### Owner chain (new order)

1. `--owner <id>` (validated; invalid -> `E_CONFINE_ARGUMENT_INVALID`, unchanged)
2. `AIRA_CONFINE_OWNER` (trimmed; invalid -> error, unchanged)
3. **NEW:** `CLAUDE_CODE_SESSION_ID`, trimmed of surrounding whitespace. If the
   result is exactly a canonical lowercase UUID (`8-4-4-4-12` of `[0-9a-f]`),
   owner = `"cc-" + id`. Anything else is ignored and the chain continues. Never
   an error.
4. discovered project worktree id (unchanged)
5. inferred `@cwd-<basename>` or `unknown` (unchanged)

Placement, to keep `resolveOwnerIn` byte-identical (`resolveConfineOwner` is
today a one-line wrapper over `resolveOwnerIn(ctx, explicit, "")`, checked at
`main.go:2273`):

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
for `"cc-"` + a canonical lowercase UUID. No runtime assert: T6 and T8 pin that
every result passes `ValidateConfineIdentity` and `ValidateConfineOwner`.

A `cc-` owner is ATTESTED (`ConfineOwnerIsAttested` is true: no `@`). This is
correct: "attested" in AIRA-23's sense means "an identity that tells sessions
apart", which a per-session UUID does and a per-directory inference does not.
The guard is cooperative, not authentication (AIRA-23, `ValidateConfineIdentity`
doc): it prevents accidents between sessions that follow the rules.

### Session ids: accepted and refused values

Accepted (owner = `cc-` + the trimmed id):

* a canonical lowercase UUID;
* the same with leading/trailing spaces, tabs or newlines (trimmed first).

Falls through to the old chain (`err == nil`, no panic, result identical to the
variable being unset):

* `""`, whitespace only;
* an uppercase or mixed-case UUID;
* 35 or 37 characters, a valid UUID plus `x`, a UUID with a hyphen missing or
  moved;
* `../x`, any value containing `@`, `/`, an embedded space or tab inside the id;
* a value containing NUL or an embedded newline;
* a 200-character value.

NUL cannot be placed in the process environment through `t.Setenv` (Go's
`syscall.Setenv` rejects it), so NUL and embedded-newline values are tested by
calling `runner.ConfineSessionOwner` directly (T8). The CLI-level test (T1) uses
the values `t.Setenv` accepts.

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
2. **Caller unattested**: "your own identity was inferred from this directory (no
   AIRA_CONFINE_OWNER, Claude Code session or aira project worktree applies
   here), so it cannot match any owner; run this from the Claude Code session or
   worktree that launched the job, or with the AIRA_CONFINE_OWNER name you
   launched it with, or pass --steal to act on it deliberately."
3. **Both attested, different**: "the job belongs to a different session or
   worktree. Unless AIRA_CONFINE_OWNER was set, an owner defaults to the Claude
   Code session that launched the job (shared with its subagents), else to the
   git worktree of the directory it was launched from. If you launched it, run
   this from that session or worktree, or with the AIRA_CONFINE_OWNER name you
   launched it with. If it is another session's job, --steal acts on it
   deliberately and is recorded as a steal."

None of the three texts contains `--owner`. The printed `owner=` value is the
victim's identity; telling a blocked agent to pass it back as `--owner` would
turn every refusal into a one-step override that the daemon's audit line
(`internal/daemon/confine_manage.go:415`) records as `killer=<victim's owner>
steal=false`: hidden, and attributed to the victim. `--steal` does the same job
and is recorded as `steal=true`. The texts only ever name an
`AIRA_CONFINE_OWNER` name the caller itself chose.

Exact wording is the builder's; T4 pins the case-distinguishing phrases (not
whole sentences) and the absence of `--owner`. A launch-directory hint
(`record.Cwd`) is deferred.

### `aira top` SESSION cell

In `cmd/aira/tui_top.go:topSessionCell` (line 1030):

* `chosen := runner.ConfineOwnerIsAttested(owner) && !isWorktreeHash(owner) && !runner.IsConfineSessionOwner(owner)`,
  so a session owner falls to the tmux window name when the job has one (AIRA-277).
* With no pane label, a session owner renders as `"cc-" + first 8 hex`
  (e.g. `cc-b6b9daa7`): short, groups one session's jobs, and visibly not a
  worktree hash.

`aira top`'s own owner (`cmd/aira/tui.go:171`, resolved once via
`resolveConfineOwner`) follows the chain: a human running `aira top` in a plain
tmux pane has no session id, so Claude-launched jobs are foreign there and `f` /
`e` show the existing `--steal` confirmation. Intended (it is the
shared-worktree false pass closed).

### Skill text (`internal/core/skill.go:320`, the confine paragraph)

Replace "Set `AIRA_CONFINE_OWNER` to get a real, attested owner you can kill
without `--steal`." and update the SESSION-fallback parenthesis to say:

* the default chain: `--owner` / `AIRA_CONFINE_OWNER`, else your Claude Code
  session (`cc-<session id>`, shared by your subagents, so from any directory you
  can manage your own and your subagents' jobs), else the git worktree of the
  CURRENT directory, else an inferred `@cwd-...` owner;
* outside a Claude session, a job launched from one worktree is not yours when
  you run `--kill` from another: run it from the launch worktree or set the owner
  inline at launch;
* `/clear` and resuming a conversation give it a NEW session id, so jobs launched
  before that are no longer yours by default: use `--steal` (recorded) to act on
  them, or launch long or detached jobs with an `AIRA_CONFINE_OWNER` name (and
  keep the scope id for `--status`);
* the MCP face (G1): the `aira` MCP server keeps the session id it was started
  with; after `/clear` or a resume, manage jobs from the CLI face (Bash) or
  restart the MCP connection, because the MCP face still acts as the OLD session;
* the `@` rule unchanged (still "never counts as ownership", `--steal` even from
  the same directory);
* `--kill` acts on running jobs only: a job still waiting for admission has no
  scope and gives `E_CONFINE_NOT_FOUND`; end it by finding its supervisor pid in
  the queued rows of `aira confine --list` and running plain `kill <pid>` on that
  supervisor (never `kill -9`).

### Generated text and doc comments

* `internal/core/core.go:1937` (the `confine` verb's `owner` ArgSpec; generated
  help and MCP schema come from this table): add "default: your Claude Code
  session (cc-<session id>), else the worktree of the current directory".
* `internal/core/core.go:1568` (`worktree-register` `owner`): semantics
  unchanged; append "(never the Claude Code session: a worktree binding is per
  checkout)" so the two chains are not confused.
* `cmd/aira/tui_controller.go:198-204` (`ConfineOwner` doc comment): chain becomes
  AIRA_CONFINE_OWNER, Claude Code session, worktree id, `@cwd`.
* `cmd/aira/tui_top.go:619` and the `topSessionCell` doc comment (1006-1028):
  mention the session-owner arm.

## Invariants

* I1. With `CLAUDE_CODE_SESSION_ID` unset or invalid, every resolved owner is
  byte-identical to today's.
* I2. `--owner` and `AIRA_CONFINE_OWNER` always win over the session id; an
  invalid `AIRA_CONFINE_OWNER` still errors (the session id does not mask it).
* I3. `worktree register`'s owner never depends on `CLAUDE_CODE_SESSION_ID`.
* I4. The guard verdict is unchanged as a function of `(owner, caller, steal)`;
  only the inputs change. `CheckConfineOwner` stays the single guard for kill
  (real + shim), front and resize.
* I5. Two different valid session ids never resolve to the same owner; one
  session id resolves to the same owner from any directory, inside or outside an
  aira project.
* I6. An inferred owner (`@...`) is still never attested.
* I7. A `cc-` owner round-trips through the scope id (`MintConfineScopeID`,
  `MintWorkerScopeID`, `ParseConfineScopeID`, `bindConfineScopeID`) unchanged.
* I8. No refusal text suggests `--owner` as a remedy.

## Code touch points (opened and checked on `origin/master` ff03d8aa)

| File:function | Change |
|---|---|
| `cmd/aira/main.go:resolveConfineOwner` (2273) | new session step before delegating to `resolveOwnerIn` |
| `cmd/aira/main.go:resolveOwnerIn` (2286) | unchanged (worktree register via `cmd/aira/worktree.go:stampWorktreeOwner`, line 54) |
| `internal/runner/confine.go` (near `InferConfineOwner`, 811; `ConfineOwnerIsAttested`, 836) | add `ConfineSessionOwnerPrefix`, `ConfineSessionOwner`, `IsConfineSessionOwner` |
| `internal/runner/confine_manage.go:CheckConfineOwner` (68) | three-case reason; verdict unchanged |
| `cmd/aira/tui_top.go:topSessionCell` (1030) and comments (619, 1006-1028) | session-owner arm; comment text |
| `cmd/aira/tui_controller.go` (`ConfineOwner` comment, 198-204) | comment text |
| `internal/core/skill.go` (line 320 paragraph) | text |
| `internal/core/skill_test.go` | pins for the new phrases |
| `internal/core/core.go` (1937 `confine` owner; 1568 `worktree-register` owner) | ArgSpec help text |
| `cmd/aira/main_test.go` (new file; no `TestMain` exists in `cmd/aira` today) | `TestMain` scrubbing `CLAUDE_CODE_SESSION_ID` |

Unchanged but on the path, checked:

* `cmd/aira/mcp_project.go` lines 63 and 77: MCP confine faces call
  `resolveConfineOwner` in the MCP SERVER process, so they read the server's own
  env, fixed at spawn (G1).
* `cmd/aira/main.go:runConfineStatusCommand`: `--status <selector>` calls
  `confineDetachStatusFor` (1989, seam over `runner.ConfineDetachStatusFor`);
  `confineDetachStatusList` (1972) runs only with no selector. Both receive the
  `resolveConfineOwner` value. `runner.ResolveConfineDetachStatus`
  (`confine_detach.go:314`) filters name/pid selectors by
  `confineDetachOwnerMatches` (254, string equality); a scope-id selector always
  matches. The record's owner comes from the DIRECTORY NAME
  (`ListConfineDetachRecords` -> `parseConfineScopeID`,
  `confine_detach_linux.go:371-404`), which the real launcher creates from the
  minted scope id.
* `confine-input` resolves its caller with the same `resolveConfineOwner`
  (`main.go:426`) and the run-input server compares the same string
  (`run_input_server_linux.go:199`).
* `internal/runner/confine.go`: `maxConfineOwnerLen = 64`; `cc-` + 36 = 39.
  `ValidateConfineIdentity` alphabet `[A-Za-z0-9._-]` covers it. The scope id
  splits at the FIRST `@` (`confine.go:1400`) and `cc-<uuid>` has none. Worst-case
  scope directory `.aira-CONFINE-<name 100>-<pid 7>-<stamp 13>@<owner 39>` = 176
  bytes < 255.
* `internal/runner/confine_queue_position_linux.go:350`: `ValidateConfineOwner`
  passes; the daemon's `confineManagement` validates the caller the same way
  (`internal/daemon/confine_manage.go:157`).
* `MintWorkerScopeID` (`confine.go:1543`) is called with the parent owner parsed
  from the parent scope id (`internal/daemon/worker_admit.go:426-446`); the daemon
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
| Process passing `--owner cc-<other id>` or `--owner <other worktree hash>` | allowed | Same as copying a worktree hash today. The guard prevents accidents, it does not authenticate (AIRA-23). AIRA itself never suggests this (I8, T4). |
| A non-Claude tool (codex, make, a script) run from a Claude Bash call | owns as that session | It inherits the id. It is that session's work. |
| A nested `claude` started from a Claude Bash call | not verified whether it injects its own id or inherits | The plan gate tried to measure it; the candidate processes exited first. Either outcome keeps the identity inside one human-launched tree. Not load-bearing. |
| **G1. MCP owner fixed for the MCP server's life**, combined with `claude --resume` of an old id elsewhere | **allowed** (accepted gap) | See G1 below. |

#### G1 (accepted gap): the MCP face keeps its spawn-time session id

* **Trigger.** Process P starts with id X; its `aira mcp` server is spawned with
  X. The user runs `/clear` or `/resume` in P, so P's conversation now has id Y
  (measured: `/clear` always changes the id). Later, another process Q runs
  `claude --resume X`, so Q presents X and launches job `build2` owned by `cc-X`.
  P's agent calls `aira_confine_kill selector=build2` over MCP.
* **Effect.** P's MCP server resolves `cc-X` == Q's owner, so the job is killed
  without `--steal`, and the daemon audit line records `killer=cc-X steal=false`.
  The kill stays inside one conversation lineage (X is an ancestor of both P's
  current conversation and Q's), needs an id change, a resume of the old id in a
  second process and a matching selector, and is far narrower than today's
  shared-repo-root false pass, which any two sessions starting at the root hit.
* **Why it is not closed.** Closing it means either (a) dropping the session step
  from the MCP faces, which brings back the false fail for every CLI-launched job
  managed over MCP (attack item (b)), or (b) reading Claude Code's private
  `~/.claude/sessions/<pid>.json` to follow the live id, coupling AIRA to an
  undocumented harness file. Both cost more than the gap.
* **Mitigation.** Skill text: after `/clear` or a resume, manage jobs from the
  CLI face or restart the MCP connection.
* The reverse direction (P's MCP face refused on jobs P's CLI launched after the
  id change) is a false fail, below.

### False fail: is a session refused on its own job?

| Case | Result | Disposition |
|---|---|---|
| Same session, launch and kill from different cwds | **allowed** (today: refused) | the fix; tests T3, T11 |
| Same session, two non-project directories (RANT-48 shape, e.g. `~/tmp/kit-fix`) | **allowed** (today: refused, `@cwd` owner) | T3b |
| Coordinator kills its subagent's job | allowed | |
| `--status` / `confine-log` / `confine-input` by name from another cwd, same session | **found** (today: "matched other owners") | same resolved caller; tests T10 and T11 (iv). `confine-input` covered by the shared resolver only (accepted coverage gap, see Test plan). |
| CLI-launched job managed over MCP, same session, no id change since the MCP server started | allowed | MCP server carries the same id (measured 13/13); test T11 (iii) |
| After `/clear` or a resume (id changes, measured): jobs launched before it | refused; needs `--steal` (recorded) | Fail-safe. Remedy in the skill and refusal case 3: `--steal`, or launch long jobs with an `AIRA_CONFINE_OWNER` name. For `--status`, a scope-id selector always matches. |
| After `/clear` or a resume: jobs the CLI launches under the new id, managed over the still-old MCP face | refused (G1 reverse) | Fail-safe; skill says use the CLI face or restart the MCP connection. Whether the Bash env follows the new id is not verified; if it does not, CLI and MCP agree on the old id and nothing is refused. |
| A detached job managed from a LATER session | refused by name; `--status` with no selector no longer lists it | Today this worked if the later session sat in the same worktree. Remedy: launch long detached jobs with `AIRA_CONFINE_OWNER=<name>`, or keep the scope id (a scope-id selector always matches in `--status`). Stated in the skill. |
| Jobs launched before the upgrade (owner = worktree id), killed after it by a Claude session | refused (case 3 text) | One-off transition; jobs are short-lived; `--steal`. |
| A human in a plain tmux pane (no id) managing Claude-launched jobs in the same worktree | refused (today: allowed) | Intended; this was the false pass. `aira top` already offers a `--steal` confirmation. |
| Session sets `AIRA_CONFINE_OWNER` at launch but not at kill | refused | Unchanged in kind; case 3 text names the cause. |
| Harness renames or reformats `CLAUDE_CODE_SESSION_ID` | the step goes silent, old behaviour | No worse than today. |

### Scope-id encoding

Checked under "Unchanged but on the path". T6 pins the round trip and the
worst-case directory length.

## RANT-49 finding: `E_CONFINE_NOT_FOUND` is not explained by owner

`killConfineWithDeps` (`internal/runner/confine_manage_linux.go:733`, matching at
742-760) matches the selector only against `listed.Scopes`, with no owner filter.
`activeConfines` (`internal/daemon/confine_manage.go:63-80`) lists only waiters in
the `admitGranted` state; a QUEUED job is listed separately in
`ConfineListResult.Queued` (`queuedConfineRows`, which drops ids already live).
So `--kill <name>` on a job that is still queued, already exited, or on a
different `--slice` returns
`E_CONFINE_NOT_FOUND: selector "<name>" matched no confine scope`. This is
consistent with the code as read. The rant's box was contended at the time (the
investigator saw 5 queued and a freeze hold), so a queued job is a plausible
cause, but the incident itself is **unreproduced** (no record of the job
survives). RANT-49's `E_CONFINE_OWNER_UNVERIFIED` half fits the cwd-reset cause
this plan fixes.

A namesake hazard was found while reading: a queued job that shares its name with
a RUNNING job selects the running one, giving `E_CONFINE_OWNER_UNVERIFIED` if
another session owns the running one, or killing the wrong job if the same
session owns both. Not a new hole (the guard still applies), and distinct names
are already the skill's advice for detached jobs; recorded in the deferred
friendlier-not-found follow-up.

No mechanism changes here. The skill gains the queued-job sentence (how to end
one: its supervisor pid from the queued rows of `aira confine --list`, then plain
`kill <pid>`).

## Test plan

Every new test must be shown red against the wrong implementation (the mutation
named) and green against the fix; the commit message records each mutation run
(command, exit code, the failing assertion).

| # | Test (file) | What it pins | Mutation that must turn it red |
|---|---|---|---|
| T1 | `TestConfineOwnerClaudeSessionStep` (`cmd/aira/confine_test.go`) | through `resolveConfineOwner` with `t.Setenv`: valid id -> `cc-<id>`, attested; padded valid id (leading/trailing space, tab, trailing newline) -> `cc-<trimmed id>`; `--owner` and `AIRA_CONFINE_OWNER` win; invalid `AIRA_CONFINE_OWNER` still errors with a valid session id set; every Setenv-able fall-through value (list above, minus NUL) resolves to the worktree id inside a project and to `@cwd-...` outside, `err == nil`, equal to the result with the variable unset | (a) delete the step; (b) move the step above `AIRA_CONFINE_OWNER`; (c) accept any non-empty value; (d) return an error on an invalid value; (e) drop the trim (padded id falls through) |
| T2 | `TestWorktreeRegisterOwnerIgnoresClaudeSession` (`cmd/aira/worktree_test.go`) | with the id set, `stampWorktreeOwner` records the worktree id | add the step to `resolveOwnerIn` |
| T3 | `TestConfineSessionOwnerIsCwdIndependentAndSessionDistinct` (`cmd/aira/confine_test.go`) | real repo + `git worktree add` (both with `.aira/config`, so worktree ids are attested and distinct): same id resolved in A and B -> equal, `CheckConfineOwner` passes; ids X and Y in the SAME worktree -> `CheckConfineOwner` refuses without steal, passes with steal | delete the step (A != B and X == Y both flip) |
| T3b | `TestConfineSessionOwnerOutsideAnyProject` (`cmd/aira/confine_test.go`) | same id resolved in two non-project temp directories -> equal `cc-` owner, `CheckConfineOwner` passes without steal (RANT-48 outcome) | delete the step (both resolve to distinct `@cwd-...`, guard refuses) |
| T4 | `TestCheckConfineOwnerRefusalSaysWhy` (`internal/runner/confine_manage_test.go`) | three cases each carry the code, `scope=`, `owner=`, `caller=` and their own distinguishing phrase; case 3 names `AIRA_CONFINE_OWNER`, "session", "worktree" and `--steal`; owner-and-caller-both-unattested reports case 1; NONE of the three texts contains `--owner`; `steal` -> nil in all three | restore the one-line message; swap case 2 and 3 texts; add "or pass --owner <owner>" to case 3 |
| T5 | `TestCheckConfineOwnerVerdictWithSessionOwners` (`internal/runner/confine_manage_test.go`; no direct `CheckConfineOwner` unit test exists today) | verdict over `(owner, caller, steal)` with `cc-` values, worktree hashes, `@cwd`, `unknown`, `""` | drop BOTH attestation checks (keep `owner == caller` only): must turn red on `@cwd-x`/`@cwd-x` and `unknown`/`unknown` (dropping one side alone is an equivalent mutant once equality is required) |
| T6 | `TestConfineScopeIDRoundTripsSessionOwner` (`internal/runner/confine_scope_bind_test.go` or neighbour) | `MintConfineScopeID` and `MintWorkerScopeID` with `cc-<uuid>` -> `ParseConfineScopeID` returns it; `bindConfineScopeID` accepts; `ValidateConfineIdentity` and `ValidateConfineOwner` accept; `.aira-` + id with a 100-char name is <= 255 bytes | set `maxConfineOwnerLen` to 38 |
| T7 | `TestTopSessionCellClaudeSessionOwner` (`cmd/aira/tui_top_test.go`) | `cc-<uuid>` + pane `deploy` -> `deploy`; no pane -> `cc-` + 8 hex; human owner `cc-deploy` + pane -> `cc-deploy` (a chosen name beats the pane) | (a) drop the recognizer from `chosen`; (b) render the session owner verbatim; (c) recognize by `HasPrefix("cc-")` only |
| T8 | `TestConfineSessionOwnerShape` (`internal/runner/confine_test.go` or neighbour) | calls `ConfineSessionOwner` and `IsConfineSessionOwner` DIRECTLY: accepted values (canonical, padded); every fall-through value including a NUL byte, an embedded newline, an embedded space and an embedded tab inside the id (separate cases from trimmed padding) -> `ok == false`, no panic; every `ok == true` result passes both validators | loosen to a prefix or length check; trim inner whitespace (embedded space accepted) |
| T9 | `internal/core/skill_test.go` pins | new phrases present (session default, subagents, `/clear`-or-resume new id, MCP face keeps its id, queued job ended by its supervisor pid); the `@`-rule phrase still present; the removed sentence absent; `core.go` `confine` owner help mentions the session default | revert `skill.go`; revert the `core.go` help text |
| T10 | `TestConfineStatusSameSessionAcrossWorktrees` (`cmd/aira/confine_detach_test.go`) | the `confineDetachStatusFor` seam is NOT replaced: the real `runner.ConfineDetachStatusFor` reads a detach directory on disk under a private `XDG_STATE_HOME`, named with `runner.MintConfineScopeID` and the owner returned by `resolveConfineOwner` in worktree A with id X (the directory name is the authoritative owner the real reader parses); the caller comes from the real CLI entry `runWithInput(["confine","--status",name])` run in worktree B: id X -> found (exit not 2, no NOT_FOUND); id Y -> `E_CONFINE_NOT_FOUND ... matched other owners`; the scope-id selector with id Y -> found | delete the step (X from B no longer matches) |
| T11 | `TestConfineKillSameClaudeSessionFromAnotherWorktree` (`cmd/aira/dispatcher_inprocess_test.go`, the real-cgroup tier of `TestMCPConfineKillOutsideProjectKeepsOwnershipAndStealChecks`) | private daemon (`XDG_STATE_HOME`, `XDG_RUNTIME_DIR` under the test dir), installed helper as `SelfPath`; `CLAUDE_CODE_SESSION_ID=X`, `AIRA_CONFINE_OWNER` unset, cwd = worktree A; launch through the CLI entry `runWithInput(["confine","--name",n,"--","sleep","60"])` with `runConfined` wrapped only to set `SelfPath` / private paths (it asserts `request.Owner == "cc-X"` and never sets it); wait for the placed state; `chdir` to worktree B; (i) id Y -> CLI `confine --kill n` -> `E_CONFINE_OWNER_UNVERIFIED` with the case-3 phrase; (ii) id X -> killed; (iii) a second job, killed over MCP (`runMCPWithDispatcher`) from B with id X -> killed; (iv) a third job launched with `--detach` from A with id X (real detach writer), `--status <name>` from B with id X -> found, with id Y -> matched other owners; then killed. Skips through `cgrouptest.SkipOrFailRealCgroup` like its neighbour, and with a plain skip when the installed helper `~/.local/bin/aira` is absent | delete the step ((ii), (iii) and (iv)-found refuse) |

Accepted coverage gap: `confine-input` by name from another cwd has no dedicated
test. It resolves its caller through the same `resolveConfineOwner`
(`main.go:426`) and the run-input server compares the same string
(`run_input_server_linux.go:199`); T1/T3 pin the resolver and T10/T11 pin the
equality path for `--status`. Low risk; written down here rather than silent.

### Gate evidence (all recorded with exact exit codes)

1. `aira confine -- make ci` from the worktree, run inside a Claude session (so
   `CLAUDE_CODE_SESSION_ID` is set: the adversarial environment for the scrub).
2. `env -u CLAUDE_CODE_SESSION_ID aira confine -- go test ./cmd/aira/...` (the CI
   environment).
3. **T11 must actually run, not skip.** CI does not set `AIRA_REAL_CGROUP`
   (`.github/workflows/ci.yml:43,72`), and `make ci` neither, so T11 can skip
   while 1 and 2 exit 0; a skip is unevaluated. Required:
   `AIRA_REAL_CGROUP=1 aira confine -- go test -count=1 -run 'TestConfineKillSameClaudeSessionFromAnotherWorktree' -v ./cmd/aira`,
   with `--- PASS: TestConfineKillSameClaudeSessionFromAnotherWorktree` (not
   `SKIP`) in the recorded output and exit 0. Precondition, recorded: the
   installed helper `~/.local/bin/aira` exists (the test skips without it, and
   `AIRA_REAL_CGROUP=1` does not turn that skip into a failure).
4. The T11 mutation under the same command: delete the session step, re-run, and
   record (ii), (iii) and (iv)-found red (non-zero exit, the failing assertions
   quoted), then restore.

## Risks

* **Coupling to a Claude Code variable.** If it is renamed, dropped or
  reformatted, the step goes silent and behaviour returns to today's. No worse
  failure.
* **G1** (MCP id fixed for the server's life; one id live in two processes after
  a resume). Accepted, narrow, documented in the skill.
* **Test environment leakage.** Mitigated by the `TestMain` scrub and by running
  the gate both with and without the variable.
* **Behaviour change for shared-worktree workflows.** A second session (or a human
  pane) that used to clean up a root-launched Claude job now needs `--steal`.
  Intended; stated in the skill and the refusal.
* **Detached jobs across sessions** (including across `/clear`) lose name-based
  `--status` / `--kill`. Mitigated by the skill guidance (owner name or scope id).

## Expected yield

* The five rants' failure class (same session, different cwd; coordinator vs
  subagent) disappears for Claude sessions, which launch nearly every confined job
  on this box (every live `aira mcp` server measured carries a distinct id).
* RANT-48's refusal also goes away for Claude sessions: launching from a
  directory that is not an aira project (its `~/tmp/kit-fix`) now gives an
  attested `cc-` owner instead of `@cwd-...`, so the same session can kill it
  from anywhere without `--steal` (T3b). For non-Claude shells RANT-48 stays as
  AIRA-302 decided.
* The shared-repo-root false pass closes for Claude sessions.
* The remaining refusals (non-Claude shells, foreign sessions, transition,
  post-`/clear`) say why and what to do without inviting a hidden override, so the
  next RANT-50 does not need an investigator.

## Deferrals (not built, and why)

* **Delete the worktree-id default** (option D). With the session step in place it
  is the source of the remaining non-Claude false pass and false fail, but removing
  it pushes every non-Claude shell to `@cwd` + `--steal`. Separate decision.
* **Closing G1** (follow the live session id in the MCP server): needs either
  dropping the session step from MCP (brings back the false fail) or reading
  Claude Code's private sessions file. Not worth it for the narrow trigger.
* **Launch-directory hint in the refusal** (`record.Cwd`). Adds a parameter to the
  one guard at four call sites; after this change the cwd mismatch is mostly a
  non-Claude case. Revisit if rants continue.
* **Friendlier `E_CONFINE_NOT_FOUND`** (RANT-49): say "N queued jobs match; end
  the supervisor pid shown in `aira confine --list`", and flag the namesake case
  (a queued job sharing its name with a running one). Message-only follow-up,
  worth a ticket; not an owner bug.
* **Making `cc-<uuid>` unforgeable via `--owner`**: the guard is cooperative, not
  authentication (AIRA-23); any process on the box can already copy a worktree
  hash. AIRA's own texts never suggest the override (I8).
* **`aira run` / `run-kill` ownership** (`internal/runner/runner_linux.go`,
  `r.owner`): a separate identity, not part of the confine guard. Not touched.
* **Mapping the session id to a friendly name** (AIRA-265 rejected it: the registry
  is the harness's). Display uses the tmux window name instead.
* **The investigator's side observation** that a job killed by its owner reports
  `terminated-by=unattributed-sigkill` naming "another session's kill" as a
  candidate: unrelated to owner resolution; worth its own rant/ticket.
