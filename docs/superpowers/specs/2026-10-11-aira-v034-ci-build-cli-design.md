# AIRA v0.34 ci-build CLI batch: ticket voiding, inverse-link refusal, index warnings, snap note, gate environment

Status: PLAN v2 (2026-10-11), revised after plan review (Sol: PASS_WITH_CHANGES, verdict
line REVISE; second reviewer: PASS_WITH_CHANGES, one P1); awaiting the plan gate. Changes
from v1 are listed in section 11. Batch of the four remaining ci-build
field reports (owner rule: batch small independent fixes, one review and test cycle).
Tickets, in the peer's priority order: AIRA-290, AIRA-291, AIRA-292, AIRA-289. Base: master
`2e5b72c` (the v0.33 code, `cf323d9`, plus the ticket-close commit). The installed box
client and daemon are both `cf323d9` (`aira version`). Protocol is 15 at the base
(`internal/daemon/protocol.go:180`) and stays 15.

Requested by the peer session ci-build (repository `~/claude/tooling`, prefix `CI`, 116
tickets, one command gate `clops-gate` whose command runs a nested `aira confine`).

**One item is correctness-critical: 289-B (one daemon per state directory).** It changes
which file the daemon lock lives in (keeping the old runtime-directory lock held as a
guard against older binaries during the migration) and how a client reports an
unreachable daemon. It is
built as its own commit set with the two-loop adversarial review (Opus builds, Fable
reviews), in the same release. Everything else is a refusal message, one warning message,
and skill text.

## 1. Problem, in plain terms, and what still reproduces

Every item was re-checked against `2e5b72c`. "Measured" means run on a binary built from
this worktree (and, where named, from tag `v0.32`) against a private daemon: private
`HOME`, `XDG_STATE_HOME` and `XDG_RUNTIME_DIR` under `~/tmp/aira291/`, never the box daemon
or the box `state.db`. The probe scripts are `~/tmp/aira291/probe1.sh` to `probe4.sh`
(scratch, not committed; the tests in section 5 are the committed reproductions).

| Item | Still reproduces? | Evidence (2026-10-11) | Verdict |
|---|---|---|---|
| 290(a) no way to delete or void a throwaway ticket | No: the void exists, it is undocumented | `aira mv PX-8 retired` from `planned` -> `ok`, rc 0 (measured). `retired` is a legal next status from every open status and from `done` (`internal/domain/ticket.go:174-182`); a retired ticket is never ready (`workable`, `relation_ready.go:433-435`) and no longer blocks: in `A blocks B`, retiring the blocker `A` unblocks `B` (`satisfied` applied to the relation's `From`, `relation_ready.go:437-439`; direction corrected in build review round 3). The design already rules out deletion: "an allocated ID resolves to a live ticket or an explicit `retired`/`superseded` one, never nothing" (`2026-08-07-aira-design.md:106`). Deleting a file instead (measured, `rm .aira/tickets/PX-7.md`, where PX-7 was the *target* of relations stored in other files): `aira get PX-7` -> `E_TICKET_NOT_IN_WORKTREE`; `aira check` -> `fail` (`E_RELATION_TARGET_MISSING` on two relations, `E_RELATION_INDEX_DIVERGENCE`); `aira ready` -> `FAIL`. Deleting a ticket that *owns* a relation instead drops that relation with the file: the ticket it blocked becomes `ready:true` with verdict `pass` and only a divergence warning (by read, pinned by `TestReadyCanonicalReadinessIgnoresDivergenceForDeletedRelationOwner`, `internal/store/m3_test.go:1022`). The installed skill never mentions `retired` (0 hits). | Skill text only; no delete verb |
| 290(b) `aira link A blocked-by B` refusal does not say what to type | Yes, measured | `link PX-3 blocked-by PX-5` and `unlink PX-2 blocked-by PX-1` -> `E_RELATION_INVALID: inverse relation kinds are query-only`, exit 1; same text for `child`, `duplicated-by`, `superseded-by`, `resolved-by`. Source: `Store.Link` and `Store.Unlink`, `internal/store/relation_ready.go:127-129` and `222-224`. | Build |
| 290(c) closing a ticket takes three `aira mv` calls | Yes, by design | `mv PX-8 done` from `planned` -> `E_TRANSITION_INVALID: planned -> done is not a legal transition; from planned the legal next statuses are: in-progress, retired, superseded` (measured; successor list shipped in v0.31). AIRA-215 decided "the `planned -> done` edge stays refused (decided, not deferred)". `mv` enforces nothing beyond the transition table (`core.go:1686-1696`), so the table is the whole lifecycle gate. | Skill text only; no shortcut |
| 290(d) `aira <verb> --help` errors | No: fixed in v0.31 (AIRA-211) | `link --help`, `mv --help`, `create --help`, `ready --help`, `reconcile --help` each print the verb's usage (measured). | Drop |
| 291 bulk `aira link` leaves `E_RELATION_INDEX_DIVERGENCE` and `aira ready` UNEVALUATED until `aira reconcile` | The divergence is real but is not caused by `link`; the UNEVALUATED was the gate and is fixed in v0.33 | See 1.1. | Build: message + honesty pin + skill text |
| 292 snap-packaged tools escape confine | Yes, measured | `aira confine -- terraform version` (`/snap/bin/terraform`, box daemon, v0.33) -> trailer `containment=enforced cap=enforced(64G) ... scope=placed scope-integrity=migrated ...`, rc 0. `migrated` means the job's own process left the scope (`classifyLaunchScopeIntegrity`, `internal/runner/runner_linux.go:1594-1600`); AIRA-225 reported `descendant-escaped` for snap tools (reported as, not re-measured here). `contained` is a sampling result, not a proof: a descendant that leaves and exits within one 50 ms sample is never seen (`runner_linux.go:1582-1592`, an accepted coverage gap). The skill never mentions `scope-integrity` (0 hits). The policy fork (loud signal, refusal, or documented gap) is AIRA-225's and stays with the owner. | Skill text only |
| 289(a) a gate command's snapshot has no git history | Yes, by code | `materializeSubject` (`internal/store/gate_command.go:325-362`) copies the tracked files into `os.MkdirTemp("", "aira-gate-subject-")`, runs `git init -q` and `git add -A -f`, and never commits: no `HEAD`, branches, remotes or history. Anything needing a commit or ref fails there. | Skill text only |
| 289(b) a nested `aira confine` inside a gate "hangs silently" unless `env_allow` carries `XDG_RUNTIME_DIR` and `DBUS_SESSION_BUS_ADDRESS` | The hang did not reproduce; a worse, silent defect did | See 1.2. | Build (289-B) + skill text |
| 289(c) admission wait counts against `timeout_ms` | Yes, by code | The gate launches its command with `Timeout: timeout_ms` (`gate_command.go:77`); a nested confine's queue wait happens inside that command; a timeout reads `U_GATE_COMMAND_TIMEOUT`, unevaluated (`gate_command.go:97-100`). The gate runner has no view of a child's admission state. | Skill text; no new code (section 2) |

### 1.1 AIRA-291 root cause, measured

The orchestrator's question was: a real divergence bug in `link`'s write path, or a stale
index being read? Answer: neither is the cause of an UNEVALUATED `ready`, and `link`'s own
writes never diverge.

1. **`link` already reconciles its own writes.** A link writes the owner ticket file and
   then, in one SQLite transaction, its ticket row and all of that file's relation rows
   (`materialiseIntent` -> `markTicketMaterialised` -> `replaceRelationIndex`,
   `internal/store/store.go:2331-2441`). Measured: 13 sequential `aira link` calls (a
   7-edge `blocks` chain plus 6 `relates` from one owner) -> `aira ready` has no divergence
   finding and `aira check` reports `relation-integrity: pass`.
2. **Parallel links to one owner file are refused, not diverged.** 11 concurrent
   `aira link PY-1 relates PY-n` -> 2 `OK`, 9 `E_PATH_INTENT_BUSY` (exit 1, bare message),
   and no divergence afterwards (measured). Review of the build re-measured it at 60 ways
   (private daemon, about 9 bursts): 2 `OK`, 57 `E_PATH_INTENT_BUSY` and, in 3 of the 9
   bursts, one `E_WRITE_CONFLICT`; the 11-way run never produced it. A writer that read
   the owner file before a peer materialised and prepared its own intent after it left a
   pending outbox row whose precondition could never hold, which wedged the path
   (`E_PATH_INTENT_BUSY` for every later write, `reconcile` itself `E_WRITE_CONFLICT`)
   until `aira intent-retire`. Fixed in 3.7: the refusal now comes before the row exists.
3. **The divergence comes from git, not from aira.** The relation index is per worktree. A
   ticket file changed by git (merge, pull, checkout, rebase) leaves that worktree's index
   behind the file until something reconciles. Measured: links made and committed in a
   linked worktree, then `git merge --ff-only` into the main checkout -> `aira ready` in
   the main checkout: envelope `PASS`, five rows carry `E_RELATION_INDEX_DIVERGENCE` with
   kind `warning`, and the newly blocked tickets are correctly `ready:false` with their
   blocker listed. `aira check` then reports `relation-integrity: fail` and rebuilds the
   index; the next `aira ready` has no divergence finding. Plain `aira reconcile` does
   **not** clear it: `Store.Reconcile` only materialises pending outbox rows
   (`store.go:2557-2664`); the relation index is rebuilt only by `Store.Rebuild`
   (`store.go:2756`, rows at `:3055`), which runs under `aira reconcile --rebuild`
   (`core.go:2223`) and inside `aira check` (`internal/store/check.go:245`), by read. ci-build's flow matches: tooling keeps a
   ticket worktree (`~/tmp/wt-tickets`) and lands ticket commits through PRs (tooling
   `d902ae9`, "tickets for every ClankerOps build unit ... (#73)"). Tooling's index today is
   consistent (199 canonical relations, 199 index rows, read-only query of `state.db`).
4. **`ready` never reads the index to decide.** Readiness and blockers come from the ticket
   files scanned in that call (`Store.Ready`, `relation_ready.go:441-546`); divergence
   findings are forced to `warning` (`relation_ready.go:496-501`, unchanged since
   `1999e48`, 2026-08-09). The index's only other use in `ready` is the conservative
   `U_RELATION_OWNER_UNREADABLE` hint, which can only add `unevaluated`
   (`canonicalFindingsForIndexedRelations`, `relation_ready.go:665-702`). The `relations`
   table has no other reader (`indexedRelations`, `relation_ready.go:373`).
5. **The UNEVALUATED was the gate.** With a proven command gate, `aira link` (a ticket-file
   edit) on a **v0.32** build -> `aira ready` `UNEVALUATED`, every row
   `U_GATE_NO_RESULT`; the same probe on the **v0.33** build -> `PASS` (measured). That is
   AIRA-285 (v0.33): ticket files left the gate subject. ci-build's `clops-gate` is a
   proven gate with `applies_to: all`.

So the remaining defect is presentation: an `E_`-prefixed warning that does not say it is
harmless or how to clear it, which the peer reasonably read as the cause of UNEVALUATED.

**Decision D1 (recorded; reversible, so not escalated): `ready` stays `PASS` on a diverged
index.** The orchestrator's constraint was "ready must never be PASS on a diverged index".
This plan does not adopt it, for these reasons:

- An envelope `PASS` means the evaluation was established, not that any ticket is
  `ready:true`. Relation blockers come from the ticket files scanned in that call; the index
  can only add a warning or the conservative `U_RELATION_OWNER_UNREADABLE` (unevaluated).
  Gate results and findings also feed each row's verdict; the index never does.
- Ordinary index lag (the git-merge case above) leaves the files blocking correctly, so
  making it `unevaluated` would be a false unevaluated: the reported symptom, for a
  condition that does not change the answer. It would also reverse the Phase-1 decision
  pinned by `TestReadyUsesCanonicalRelationsAndReportsStaleIndex`.
- The one case where the index "knows" a blocker the files do not is a relation owner whose
  file is gone (deleted, or absent on the checked-out branch). Phase 1 decided (commit
  `ef8202f`, pinned by `TestReadyCanonicalReadinessIgnoresDivergenceForDeletedRelationOwner`)
  that this follows the files: deleting the owner is an edit that removes its relations, and
  retiring that owner gives the same readiness (a `retired` blocker is satisfied). A
  dangling symlink owner, which cannot be read, is `unevaluated` (`U_RELATION_OWNER_UNREADABLE`).
  This plan does not re-open that decision; it makes the skill say it (3.1) instead of
  claiming that deletion always fails `ready`.

What honesty does require is a pin that a lagging index can never make a ticket ready that
the files block. No such test exists today (the existing one covers only the opposite
direction); 5.2 adds it.

### 1.2 AIRA-289(b) root cause, measured

The daemon's single-instance lock lives in the runtime directory:
`LockPath = <XDG_RUNTIME_DIR>/aira/<state id>/daemon.lock`, or under
`<state home>/aira/run/` when `XDG_RUNTIME_DIR` is unset (`PathsFromEnvironment`,
`internal/daemon/paths.go:426-444`). So "only one daemon" is enforced per runtime
directory, not per `state.db`. A client whose `XDG_RUNTIME_DIR` differs from the running
daemon's looks for a socket that does not exist, wins a lock nobody holds, and starts a
second daemon on the same `state.db` (`dispatcher.go:744-816`, `server.go:385-403`).

Measured in the private sandbox (`probe4.sh`): one client with `XDG_RUNTIME_DIR=run4`,
then one with it unset (the shape of a gate command whose `env_allow` has `HOME` but not
`XDG_RUNTIME_DIR`). Result: two daemon processes with the same `XDG_STATE_HOME`, two
sockets (`run4/aira/866b.../daemon.sock` and `state4/aira/run/aira/866b.../daemon.sock`),
and both clients' `aira create` succeeded against the one `state.db` (ids PW-1 and PW-2).
`aira confine -- /bin/true` from the scrubbed environment (no `XDG_RUNTIME_DIR`, no
`DBUS_SESSION_BUS_ADDRESS`) then ran with `admission=immediate scope=placed
scope-integrity=contained`: scope placement does not need D-Bus, and admission came from
the second daemon, whose ledger knows none of the first daemon's grants. On the box this
would mean a job admitted against an empty ledger while the slice is full (the slice cap
still binds, so it would be an in-slice OOM, not a box OOM).

The "hang" was not reproduced by the probe above, which ran `aira create` first: that call
started the second daemon, and the confine that followed was admitted by it. Review of the
build reproduced it by code and by measurement when the nested `aira confine` is the ONLY
`aira` call: `aira confine` dials `paths.SocketPath` itself (`cmd/aira/main.go`,
`internal/runner/admission_linux.go` `admitThroughDaemon`), never goes through the daemon
dispatcher, and on a transport failure reconnects every 500 ms with no deadline, printing
`waiting for the memory-admission daemon to become reachable`. With a state lock that fences
a second daemon, a mismatched `XDG_RUNTIME_DIR` therefore means waiting forever (a gate
reads `U_GATE_COMMAND_TIMEOUT`; a cron job never returns). Fixed in 3.5 step 6: the confine
launch refuses first. On this box no second daemon has ever run with the default state:
`~/.local/state/aira/run/` does not exist. Before v0.33 a failing gate showed no output
(AIRA-288), and a queued confine inside a gate looks like a hang until `timeout_ms` fires
(289(c)). The peer's gate now lists `DBUS_SESSION_BUS_ADDRESS`, `HOME` and
`XDG_RUNTIME_DIR` (sorted), so it reaches the box daemon.

Mixed binaries (by read, from both reviews). Moving the lock alone is not enough while
older binaries run on the box (worktree builds, an `aira mcp` server that keeps the binary
it started with). Those binaries still take the old runtime-directory lock. Two cases:

- A v0.34 daemon is running and a pre-v0.34 client with the same `XDG_RUNTIME_DIR` finds no
  socket (a restart window): it wins the old lock, nobody holding it, and spawns a
  pre-v0.34 daemon, which unlinks the socket path and binds (`server.go:410`): two daemons
  on one `state.db`.
- A pre-v0.34 daemon is still running (before the box cutover) and a v0.34 client with a
  different `XDG_RUNTIME_DIR` finds no socket: it wins the new state lock, nobody holding
  it, and starts a second daemon.

The first is closed by having a v0.34 daemon hold the old lock as well (3.5). The second
cannot be detected by the new binary (it does not know the other runtime directory); it
ends when the box daemon is restarted onto v0.34, which the cutover does explicitly
(section 10). The same holds for any pre-v0.34 binary run with a different
`XDG_RUNTIME_DIR` from the daemon's: it never consults the state lock, so it keeps today's
defect until it is rebuilt.

### 1.3 Incidental finding (not in this batch)

During the probes, a daemon started by the client (`spawnDaemon`, `cmd/aira/dispatcher.go:95-118`,
not the systemd service) died twice in the middle of a sequence of `aira link` calls, and
the client in flight got `E_DAEMON_UNAVAILABLE: EOF`. A daemon started in the foreground
with stderr sent to a file survived the same sequence. Likely mechanism (assumption, not
verified): `spawnDaemon` connects the child's stderr to a pipe read by the spawning client
(`child.Stderr = &childStderr`); once that client exits the pipe has no reader, and a Go
program exits on SIGPIPE at its next write to fd 2 (the daemon logs to stderr). The box
daemon is started by systemd (stderr to the journal), so it is not affected; fresh machines
without `aira install`, and anything else that relies on the client starting the daemon,
are. Suggest a held ticket (section 7).

## 2. Greenfield minimum, and rejected alternatives

If each problem had to be solved alone, from nothing:

- **290(a):** tell people the void that exists: `aira mv <id> retired`.
- **290(b):** when the kind is an inverse, print the command that stores the same relation:
  the forward kind with the endpoints swapped.
- **290(c):** tell people the close path; the edge refusal is a decided rule.
- **291:** nothing to fix in `link`. Make the warning say what it is and how to clear it,
  pin that the index can never unblock a ticket, and document the git case.
- **292:** one paragraph in the skill: read `scope-integrity=`, not `containment=`.
- **289(a), 289(c):** one skill section on what a gate command sees, plus the simplest
  confinement: wrap the gate run (`aira confine -- aira gate run <id>`), which does its
  admission wait before `timeout_ms` starts, provided the gate command does not run its
  own nested `aira confine` (and if the build precondition in 3.6 holds).
- **289(b):** key the daemon lock on the state directory (one path in
  `PathsFromEnvironment`), so two v0.34 daemons can never share a `state.db`; while older
  binaries exist, have the daemon also hold the old runtime-directory lock; and have a
  client that cannot reach the lock holder say why instead of timing out generically.

The plan below is that minimum. It adds no verb, no flag, no stable code and no wire field;
the only storage change is one field in the daemon's lock file (the holder's socket path,
used only to word an error, 3.5). It deletes no behaviour anyone relies on.

| Item | Alternative | Why rejected |
|---|---|---|
| 290(a) | An `aira delete` / `aira void` verb | The design requires every allocated ID to resolve to a live, `retired` or `superseded` ticket, never nothing (`aira-design.md:106`). Removing the file leaves `E_TICKET_NOT_IN_WORKTREE` and dangling relations (measured above); git history keeps the file anyway. `retired` already does what a void needs in one hop, and is reversible only by a new ticket, which is the honest record. |
| 290(a) | Hide `retired` tickets from `aira list` by default | Not asked; `aira list status:planned` (or any status filter) already narrows the view. A default filter would make "where did my ticket go" a new question. |
| 290(b) | Accept the inverse spelling and store the forward relation (normalise on input) | The Phase-1 rule is that inverse kinds are query projections and are never written (`ValidRelationKind` doc, `ticket.go:318-319`); one written spelling keeps CLI, MCP and the journal target (`B:blocks:A`) identical. The peer asked for the error to name the canonical form; a refusal that names the exact command costs one retry and changes no stored state. |
| 290(b) | A second relation table mapping inverse to forward | `Inverse()` already encodes the pairs; looping over the five forward kinds that have a distinct inverse reuses it (3.2). |
| 290(c) | `aira mv <id> done --through` (walk every legal hop in one call) | Reaches `done` from `planned` in one call, which is what AIRA-215 decided to refuse; `mv` has no gate but the transition table, so a multi-hop flag would empty `in-review` of meaning. The close path stays three journalled hops; throwaways need one (`retired`). |
| 291 | Have `link` call reconcile after writing | `link` already writes its file's index rows in the same transaction (1.1); a reconcile would only re-do that, plus scan every worktree with git. It would not touch the cause (git edits). |
| 291 | Let `ready` (daemon-routed) refresh the index for files whose digest changed | Makes a read verb write; the index changes no readiness answer, so the write buys only the removal of a warning. `aira reconcile --rebuild` / `aira check` already do it, and they are client-routed (`routing.go:62`) because reconciling runs git (`git worktree list`, `for-each-ref`), which the daemon does not do in callers' repositories. |
| 291 | Make divergence `unevaluated` in `ready` | A false unevaluated for ordinary lag (D1, 1.1): the reported symptom, for a condition that does not change the answer. |
| 291 | Make a relation owner whose allocated file is missing turn its far endpoint `unevaluated` | Reverses a settled Phase-1 decision (`ef8202f`, D1) and would also fire on a legitimate checkout of a branch where the owner does not exist yet. Retiring the owner gives the same readiness as deleting it. Documented in the skill instead. |
| 291 | Drop the divergence warning from `ready` (keep it in `check`) | Removes the only `ready`-side signal that the index lags; the cheaper fix is a message that explains itself. Kept, reworded. |
| 291 | Rename the warning `W_RELATION_INDEX_DIVERGENCE` in `ready` | Two codes for one condition (it is a `fail` in `check`, by an existing decision). The message carries the meaning instead. |
| 292 | Decide AIRA-225 here (loud signal or refusal for snap escapes) | An open owner fork on another ticket; 292 asks only for the note. |
| 289(b) | Document "put `XDG_RUNTIME_DIR` in `env_allow`" and change nothing | Leaves a silent second daemon with a blind admission ledger for every caller that forgets: gates, cron, an `ssh` session without `pam_systemd`, `env -i`. "Not established" must be loud; here it is silent. |
| 289(b) | When `XDG_RUNTIME_DIR` is unset, fall back to `/run/user/<uid>` if it exists | Fixes the common case but not the invariant: any other differing value still starts a second daemon. Not needed once the lock is per state; deferred. |
| 289(b) | Record the socket path in the lock file and let the client dial the recorded socket | Makes mismatched clients "just work", but adds a discovery path and a trust question (dialling a path read from a file). The loud refusal is smaller; deferred (section 7). The recorded path *is* now written, but only to choose and word the error (3.5); nothing dials it. |
| 289(b) | Move the lock and drop the runtime-directory lock outright | Opens the mixed-binary hole in 1.2 (a pre-v0.34 client in the daemon's own runtime directory starts a second daemon in a restart window). The daemon keeps holding the old lock as a migration guard; it is one extra `flock` and is removed in a later release (section 7). The *rule* is one lock (the state lock); the old one is only a fence for binaries that do not know the rule. |
| 289(b) | Have the client, not only the daemon, also try the old lock | The daemon's own locks are the guarantee; the client's startup lock only serialises spawning. A v0.34 client spawning past a held old lock gets a daemon that refuses (`ErrAlreadyRunning`), and keeps polling. |
| 289(b) | A new code such as `E_DAEMON_RUNTIME_MISMATCH` | The condition is "no daemon reachable from here", which `E_DAEMON_UNAVAILABLE` (exit 4) already names; the message carries the cause. No catalogue change. |
| 289(c) | A distinct code for "timed out while queued in confine admission" | Not cheap and not honest: the gate runner sees only a child process. Knowing that the child was queued would mean parsing its stderr (judgement on text) or asking the daemon about waiters by process ancestry (new machinery). The run output already shows the queue (`confine: waiting for memory admission ...` lines, readable with `aira run-log <RUN-id> --stream err`; the run id is in the gate result since v0.33), and wrapping the gate run moves the wait outside `timeout_ms`. |
| 289(c) | Re-add a confine admission timeout | Removed deliberately in S13 (`d2a32db`): admission waits are unbounded but fair. |
| 289 | Sort `env_allow` on `gate add`/`gate set` | The refusal is already loud and names the rule (`E_GATE_INVALID: env_allow must be sorted and unique`, `internal/gate/command.go:85-89`); hand-written definitions would still need sorting. |

## 3. Design

### 3.1 AIRA-290(a), (c): ticket lifecycle in the skill

`internal/core/skill.go`, `renderMarkdownBody`: the "Backlog and release discipline"
section (`skill.go:350-358`) gains one paragraph, "Closing, voiding and linking tickets":

- The close path is `aira mv <id> in-progress`, then `in-review`, then `done`, one hop per
  call; each hop is journalled. There is no `planned -> done` shortcut (decided in
  AIRA-215). A refused move names the legal next statuses.
- A throwaway or mistaken ticket is voided with `aira mv <id> retired`: one hop from any
  open status (and from `done`). `retired` is terminal and never ready, and a retired
  blocker no longer blocks: in `A blocks B`, retiring `A` unblocks `B`. There is no delete: IDs are never reused, and every
  allocated ID must resolve to a live, retired or superseded ticket. Deleting the file
  instead leaves `aira get` answering `E_TICKET_NOT_IN_WORKTREE`; relations stored in
  other tickets' files that point at it dangle, so `aira ready` fails with
  `E_RELATION_TARGET_MISSING`; and relations stored in the deleted file vanish with it, so
  a ticket it blocked becomes ready.
- Relations are written only in their forward form (`blocks`, `parent`, `relates`,
  `duplicates`, `supersedes`, `resolves`); the inverse names (`blocked-by`, `child`, ...)
  are read-only views, and a refusal names the forward command to run instead.
- A relation is stored in the lower-ID ticket's file, so concurrent writes to one file are
  refused `E_PATH_INTENT_BUSY`: run bulk links one after another, or retry the refused ones.
- When git changes ticket files (a merge, pull, checkout or rebase), `aira ready` may carry
  `E_RELATION_INDEX_DIVERGENCE` warnings until `aira reconcile --rebuild` (or `aira check`)
  rebuilds the derived index; plain `aira reconcile` does not. Relation blockers are always
  computed from the ticket files, so these warnings never change a verdict.

(The last two bullets are AIRA-291's documentation half.)

### 3.2 AIRA-290(b): the inverse-kind refusal names the forward command

`internal/domain/ticket.go`, next to `Inverse` (`ticket.go:322-342`):

    // Forward returns the stored forward kind whose query-only inverse is k, and
    // whether k is one of the five inverse kinds. relates is its own inverse and
    // is forward, so it is not in the list.
    func (k RelationKind) Forward() (RelationKind, bool) {
        for _, forward := range []RelationKind{RelationBlocks, RelationParent, RelationDuplicates, RelationSupersedes, RelationResolves} {
            if forward.Inverse() == k {
                return forward, true
            }
        }
        return "", false
    }

`internal/store/relation_ready.go`, `Store.Link` (line 127) and `Store.Unlink` (line 222),
replacing the one-line refusal. IDs are the canonical ones (after `s.canonicalID`, which
both functions already apply on entry):

- inverse kind (`kind.Forward()` true): `E_RELATION_INVALID: blocked-by is a query-only
  inverse kind and is never stored; write the same relation as: aira link CI-5 blocks CI-3`
  for `link CI-3 blocked-by CI-5`. Unlink: `...; remove it as: aira unlink CI-5 blocks
  CI-3`. Endpoints swapped, forward kind named.
- any other non-forward kind (unreachable through the CLI and MCP enums, kept correct
  anyway): `E_RELATION_INVALID: unknown relation kind "<k>"; the writable kinds are:
  blocks, duplicates, parent, relates, resolves, supersedes`.

The refusal still happens before any read or write (no outbox row, no event, no file
change). Code and exit unchanged (`E_RELATION_INVALID`, exit 1).

### 3.3 AIRA-291: the divergence warning explains itself, and the index can never unblock a ticket

`internal/store/relation_ready.go`:

- `relationIndexDivergence` (`relation_ready.go:405-416`): both messages gain one shared
  suffix (shown verbatim; the backticks are part of the message so a test can extract the
  command):

      ; the derived index lags the ticket files (for example after a git merge, pull or checkout); run `aira reconcile --rebuild` (or `aira check`) to rebuild it

  This text reaches `check` and `ready` alike. (v1 named plain `aira reconcile`, which does not touch the
  relation index: section 11, row 2.)
- `Store.Ready`, in the existing loop that forces divergence findings to `warning`
  (`relation_ready.go:496-501`): also append `; ready computed relation blockers from the
  ticket files, so this warning does not change them`.

No behaviour change: same codes, same kinds, same verdicts. The new test in 5.2 pins the
false-pass direction (a `blocks` edge in a file but absent from the index still blocks),
and a second test runs the command the message names and checks the warning is gone.
`link` itself is unchanged; its root-cause measurement becomes a committed test (5.2).

### 3.4 AIRA-292: read `scope-integrity`, not `containment`

`internal/core/skill.go`, section "Reading a confined job's outcome" (`skill.go:328-334`),
one new paragraph after the `ran=no` paragraph:

- `containment=enforced` describes the scope AIRA created; whether your process stayed in
  it is `scope-integrity=`. `contained` means no escape was observed: the job's process was
  seen in the scope and no process was seen leaving it, checked every 50 ms, so a child
  that leaves and exits between two checks is not seen. `descendant-killed` means a
  straggler left in the scope at the end was killed. `migrated` (the job's own process was
  seen outside the scope) or `descendant-escaped` (a child was seen outside it) means the
  memory cap does not bind the moved process and `aira confine --kill` cannot reach it.
  `unverified` means it could not be established; read it as "not shown to be contained".
- Snap-packaged tools can do this: snapd moves a program run from `/snap/bin` into its own
  `snap.*.scope`. Measured on this box: `aira confine -- terraform version`
  (`/snap/bin/terraform`) -> `containment=enforced ... scope-integrity=migrated`, exit 0.
  Check the trailer the first time you confine a tool; for heavy work prefer a non-snap
  build (a distribution package or the vendor's tarball). Whether AIRA should refuse or
  flag this is open (AIRA-225).

### 3.5 AIRA-289-B: one daemon per state directory (correctness-critical)

**Invariant:** among v0.34 and later binaries, at most one daemon serves a given state
directory, whatever `XDG_RUNTIME_DIR` each process has; a client that cannot reach that
daemon says so loudly and never starts a second one. Against pre-v0.34 binaries the
guarantee is narrower (1.2): a running v0.34 daemon fences off any older binary that uses
the same `XDG_RUNTIME_DIR` as the daemon; an older binary with a different
`XDG_RUNTIME_DIR`, or an older daemon still running when a v0.34 client arrives, is
outside it until the cutover restart and the rebuild of old binaries.

1. `internal/daemon/paths.go`, `PathsFromEnvironment`: `LockPath` becomes
   `filepath.Join(stateDir, "daemon.lock")` (`stateDir` = `<state home>/aira`, the
   directory that holds `state.db`), instead of `filepath.Join(daemonDir, "daemon.lock")`.
   A new field `LegacyLockPath` keeps the old value, `filepath.Join(daemonDir,
   "daemon.lock")`. `SocketPath` and `RuntimeDir` are unchanged. Comment the reason (one
   daemon per `state.db`; AIRA-289; legacy lock until pre-v0.34 binaries are gone). The
   lock users that follow `LockPath`: the daemon's single-instance lock
   (`server.go:385-403`), the client's startup lock (`tryStartupLock`,
   `dispatcher.go:848`) and `Status`/`Stop` (`paths.go:524-549`). Only the daemon uses
   `LegacyLockPath`.
2. `internal/daemon/paths.go`, `LockInfo` (`paths.go:479`) gains
   `Socket string` (`json:"socket,omitempty"`), and `writeLockInfo` records
   `s.Paths.SocketPath`. It is read only to choose and word a client error (step 4);
   nothing dials it.
3. `internal/daemon/server.go`, the lock sequence (`server.go:379-410`):
   - before opening the lock, `os.MkdirAll(filepath.Dir(s.Paths.LockPath), 0o700)`: on a
     fresh machine the state directory may not exist yet when the runtime directory does;
   - take the state lock (`LockPath`) exactly as today (non-blocking; held ->
     `ErrAlreadyRunning`);
   - then take the legacy lock (`LegacyLockPath`, in the runtime directory the server
     already creates) the same way, non-blocking; held -> release the state lock and
     return `ErrAlreadyRunning` (a pre-v0.34 daemon serves this state from this runtime
     directory). Fixed order, state lock first, so two v0.34 daemons cannot deadlock;
   - write the same `LockInfo` to both files (older `aira daemon status`/`stop` read the
     legacy file), hold both for the daemon's lifetime and release both on exit, through
     the existing `retainInstance`/`lockHeld` cleanup.
4. `cmd/aira/dispatcher.go`, the connect loop (`dispatcher.go:744-846`):
   - beside the existing `MkdirAll(d.paths.RuntimeDir, 0o700)` (`:747`), also create
     `filepath.Dir(d.paths.LockPath)` (same error handling: `E_DAEMON_UNAVAILABLE`, request
     not sent). `tryStartupLock` itself is unchanged (a failed open still reads as "not
     won" and the client polls).
   - at the deadline, before the existing generic `E_DAEMON_TIMEOUT`, if this client never
     won the lock (`!spawned`) and the client's own socket does not answer (`Status.Ready` false;
     a socket FILE left by a killed stray is not a daemon), read the lock info
     (`readLockInfo(d.paths.LockPath)`):
     - **holder's recorded socket differs from this client's:** return
       (request-not-sent wrapped, as the surrounding returns are)
       `E_DAEMON_UNAVAILABLE` (exit 4): `no daemon is listening at <SocketPath>; this
       state's daemon (pid <PID>) listens at <holder socket>, so this process's
       XDG_RUNTIME_DIR (<value, or unset>) differs from the daemon's; run with the
       daemon's XDG_RUNTIME_DIR (a gate command that calls aira needs it in env_allow)`.
       The value is read through the dispatcher's existing `environment()` seam.
     - **otherwise** (same socket: the holder is a daemon still opening the database or
       restarting; or nothing recorded): keep today's `E_DAEMON_TIMEOUT` (exit 3) and its
       text, appending `; the daemon lock is held by pid <PID>, which has not bound
       <SocketPath> yet` when a PID is recorded. Nothing is blamed on `XDG_RUNTIME_DIR`.
   The lock info is written by the daemon right after it wins the lock and before it
   binds, so while a daemon holds the lock the info is that daemon's. The only other
   holder is a client's startup lock, held for microseconds and released before it forks;
   a stale record seen in that window can at worst pick the wrong one of these two
   messages, never start a daemon.
5. Nothing else changes: the socket path, the handshake (which already refuses a foreign
   state id), `ServiceIdentityMatches`, install's `daemon SocketPath divergence` check
   (`install.go:2154-2160`) and AIRA-203's `MainPID` comparison.

6. `cmd/aira/dispatcher.go`, `spawnDaemon`, and `daemon.ManagedServicePaths` (new;
   `ServiceIdentityMatches` is now `ManagedServicePaths` plus a socket comparison). The state
   lock fences a RUNNING daemon only. While the enabled `aira-daemon.service` is down (a
   restart, a crash, boot, the cutover restart), a client with a different or unset
   `XDG_RUNTIME_DIR` used to fork its own daemon: that stray won the state lock, the service
   then lost it, exited 0 and was restarted every 2 s indefinitely, every correctly
   configured client was refused, and the stray ran without the unit's
   `AIRA_DAEMON_WATCHDOG_MODE=enforce` and `SLICE_CEILING_MODE=enforce` (the box watchdog
   off for as long as it lived). Now, when the enabled aira-managed unit serves THIS state
   (same state id) through a socket other than the client's, `spawnDaemon` refuses
   (`E_DAEMON_UNAVAILABLE`, request not sent: `aira-daemon.service serves this state at
   <socket>, which this process (looking at <socket>, XDG_RUNTIME_DIR <value>) cannot
   reach; start the service or run with its XDG_RUNTIME_DIR`). A client whose identity
   matches the service still runs `systemctl --user start`; a service serving another state
   is ignored (a developer's own daemon on a private `XDG_STATE_HOME` still forks). Its
   cost: a client that cannot establish the unit's identity (no user manager reachable
   either through its own environment or at `/run/user/<uid>`, no `systemctl` on `PATH`,
   `/usr/bin` or `/bin`, or an unreadable unit file) still forks, as before. The identity
   probe does not depend on the caller's session variables: `daemon.RunSystemctl` repeats a
   failed `systemctl --user` once in the manager's own environment
   (`XDG_RUNTIME_DIR=/run/user/<uid>` and no `DBUS_SESSION_BUS_ADDRESS`, because the real
   tool goes through that address whenever it is present, even empty or stale: measured,
   systemd 255), because a cron job, `env -i`, or a gate whose `env_allow` omits the
   session variables otherwise gets "Failed to connect to bus" and would fork (build review
   rounds 2 and 3); it finds `systemctl` at `/usr/bin` or `/bin` when `PATH` has none. The
   unit file read is the one the manager has loaded (`systemctl --user show -p
   FragmentPath`), never one chosen from the caller's `HOME`: is-enabled, show-environment
   and `start` all reach the uid's real manager, so a private-`HOME` sandbox holding an
   aira-managed unit of its own must not take that file for the manager's unit (round 3).
   The same rule, "the enabled unit serving this state owns it"
   (`daemon.ManagedServiceForState`: same state id, any socket), also guards `aira daemon
   serve` (refused, exit 4, when the unit serves this state at another socket) and `aira
   daemon stop` (redirected to `systemctl --user stop`) (round 3).
7. `cmd/aira/main.go`, `runConfineCommand`: after resolving paths, and before any
   admission, `unreachableDaemonError` returns `E_DAEMON_UNAVAILABLE` (the same
   `ran=no` refusal as an argument error, exit 4) when the client's own socket does not
   answer and either the state's running lock holder recorded a different socket, or the
   enabled unit serves this state elsewhere. It dials only the client's own socket (100 ms,
   through `Status`; never the recorded path) and returns nil whenever it cannot establish that, so a
   daemon that is restarting or starting stays a wait (admission is unbounded by design,
   S13). Same wording as step 4. `aira drain wait` and `confine-reserve` dial the same way
   and are not covered (open, section 7).

Consequences, stated: `aira daemon status` from a mismatched process now reports the
lock holder as running and the socket as not ready (true). `aira daemon stop` from such a
process is redirected to `systemctl --user stop` when the enabled unit serves this state
(round 3); when no enabled unit serving this state can be identified from that process, it
SIGTERMs the state's lock holder (it is that state's daemon, which on a box with the
service is the service's daemon: release note, section 10). `aira daemon` started by hand
while a daemon owns the state gets `ErrAlreadyRunning` and exits 0 without serving
(`daemon_command.go:47-52`), instead of starting a second one; started by hand from
another runtime directory for the enabled unit's state, it is refused (exit 4) whether or
not the service is up.

### 3.6 AIRA-289(a), (c) and the env half of (b): command gates in the skill

`internal/core/skill.go`: a new short section, "Command gates: what the command sees",
placed after "Coordination is opt-in per project":

- The command runs in a temporary copy of the tracked files, not in your worktree: a fresh
  git repository with every tracked file staged and no commits, branches, remotes or
  history (and, since v0.33, without `.aira/tickets/` and `.aira/findings/`). Untracked
  files are absent. `git ls-files` and `git diff --cached` work; anything that needs a
  commit or a ref (`git log`, `git merge-base`, `git describe`, `git diff HEAD`,
  `origin/...`) fails there. A gate proof is bound to those bytes, so compute
  history-dependent inputs outside the gate.
- The command's environment holds only names listed in `env_allow` (sorted and unique, or
  the definition is refused), and of those only the ones set in the process that runs the
  gate; AIRA's own launch-coordination variables are removed even if listed
  (`allowListedEnvironment`, `gate_command.go:167-181`). Listing `PATH` makes `aira ready`
  read the proof stale whenever the `PATH` it sees differs from the one at `gate run`
  (the daemon's usually does; `gate run` prints a warning); set `PATH` inside the command
  instead.
- To memory-confine a gate, wrap the run: `aira confine -- aira gate run <id>`, and do not
  also call `aira confine` inside the command. Admission then happens before the command
  starts, so the queue wait is outside `timeout_ms`, and the command needs no extra
  environment. The command's run scope is created under the caller's cgroup, so it stays
  inside the outer scope; this does not hold if the project sets `run.cgroup_parent` in
  `.aira/config`, which places it elsewhere (`linuxScopeBackend.Create`,
  `cgroup_linux.go:173-192`; ci-build's config does not set it). If the command itself
  calls `aira` (for example a nested `aira confine`, which puts its queue wait back inside
  `timeout_ms`), `env_allow` must include `HOME` and `XDG_RUNTIME_DIR` so the inner call
  reaches this machine's daemon; without `XDG_RUNTIME_DIR` it is refused with
  `E_DAEMON_UNAVAILABLE` naming the daemon's socket (for the nested `aira confine` itself,
  at launch with `ran=no`, by 3.5 step 7; this needs the holder's recorded socket, or the
  user manager reachable so the unit can be identified; `HOME` and `XDG_STATE_HOME` are
  needed anyway for the inner `aira` to find this machine's state). `DBUS_SESSION_BUS_ADDRESS` is not
  needed (measured: a scope is placed without it).
  **Build precondition (not yet measured):** before this paragraph is written, the builder
  runs `aira confine -- aira gate run <id>` against a private daemon (private `HOME`,
  `XDG_STATE_HOME`, `XDG_RUNTIME_DIR` under `~/tmp/`) and records the gate run's outcome
  and its run's scope placement. If the inner run scope cannot be created under the outer
  confine scope (for example cgroup v2's no-internal-processes rule), the wrap
  recommendation is dropped and the paragraph documents only the nested form and its
  `timeout_ms` cost; section 11 records which.
- `timeout_ms` covers the whole command, including time a nested `aira confine` spends
  queued for admission. A timed-out gate reads `U_GATE_COMMAND_TIMEOUT` (unevaluated);
  `aira run-log <RUN-id> --stream err` (the run id is in the gate result) shows
  `confine: waiting for memory admission` lines when the time went to queueing.

### 3.7 Stale precondition refused before a row exists (found by build review)

`preparePathMutationEventKind` (`internal/store/store.go`) inserts a pending outbox row
whose `precondition_digest` is the digest the caller read. Callers (`Link`, `Unlink`,
`UpdateTicketContent`, finding and requirement writes, import) read the file, compute the
new bytes, and only then prepare, outside any transaction. A peer that materialised in
between left the precondition stale; materialise could then only refuse the row
(`E_WRITE_CONFLICT`) and the row stayed pending, so every later write to the path was
`E_PATH_INTENT_BUSY` and `reconcile` itself `E_WRITE_CONFLICT`, until `aira intent-retire`.
Inside the immediate transaction that would insert the row, after the existing pending-row
check, prepare now compares the on-disk digest to the precondition and returns
`E_WRITE_CONFLICT` without inserting (an on-disk digest equal to the intended one passes,
the idempotent re-run). A file is written only while its writer's row is pending, so this
closes the window between the read and the insert for AIRA writers; a non-cooperative
editor is still caught at materialise as before. Nothing is lost silently: the refused
writer stores no relation and the caller re-runs. That materialise-time refusal is the one
`E_WRITE_CONFLICT` that still leaves its row pending, and the path then answers
`E_PATH_INTENT_BUSY` until `aira intent-retire <path>`: a non-aira change (git, an editor)
between prepare and materialise, or `aira create` onto a path a file already occupies
(create prepares through its own path, which this check does not cover). The skill says
so (round 3).

## 4. Invariants

1. `ready`'s relation blockers come only from the ticket files read in that call (gate
   results and findings also feed each row's verdict; the derived relation index never
   does). The index can add a warning or the conservative `U_RELATION_OWNER_UNREADABLE`;
   it can never remove a blocker or turn `fail` or `unevaluated` into `pass`. Pinned in
   both directions (5.2). An envelope `PASS` means the evaluation was established, not that
   any ticket is ready (D1).
2. A ticket write (`link`, `unlink`, `create`, `mv`, `set`) leaves the written file's
   relation index rows equal to the file's relations in the same materialisation; a
   sequence of `link` calls in one worktree produces no divergence finding.
3. An inverse relation kind is never stored. Its refusal names a command that stores the
   same relation (forward kind, endpoints swapped), and running that command succeeds.
4. Among v0.34+ binaries, at most one daemon serves a state directory, whatever
   `XDG_RUNTIME_DIR` each process has; a running v0.34 daemon also holds the legacy
   runtime-directory lock, so no pre-v0.34 binary sharing its `XDG_RUNTIME_DIR` can start
   a second one. A client that cannot reach the lock holder starts no daemon and returns,
   within its start wait, `E_DAEMON_UNAVAILABLE` when the holder recorded a different
   socket, or today's `E_DAEMON_TIMEOUT` (with the holder's PID) otherwise. The residual
   (1.2: pre-v0.34 binaries with a different `XDG_RUNTIME_DIR`, or a pre-v0.34 daemon
   still running) is stated, not claimed closed.
5. A client on a machine with no state directory yet still starts the daemon (the lock's
   directory is created first, by the client and by the daemon).
6. Every behavioural claim the new skill text makes is one this plan measured or read in
   code (section 1), or that the builder measures first (the wrap precondition, 3.6), and
   each paragraph is pinned by a test. The command the divergence message names is run by
   a test and clears the warning.
7. No new stable code, no protocol change (stays 15), no wire field. The only storage
   change is the `socket` field in the daemon lock file.

## 5. Tests (TDD; each must fail against the wrong implementation)

Write each test first and record it RED on `2e5b72c`, or RED against the named mutation
where today's behaviour is already right. Each mutation must turn at least one test RED.

### 5.1 AIRA-290(b) (`internal/domain`, `internal/store`)

- `TestRelationKindForward`: `blocked-by -> blocks`, `child -> parent`,
  `duplicated-by -> duplicates`, `superseded-by -> supersedes`, `resolved-by -> resolves`,
  all `true`; the six forward kinds, `""` and `"bogus"` -> `false`. Mutation: return `k`
  itself -> RED.
- `TestLinkInverseKindNamesTheForwardCommand` (table over the five inverse kinds, link and
  unlink): code `E_RELATION_INVALID`, and the message contains the command with the
  endpoints swapped and the forward kind (`aira link CI-5 blocks CI-3`, or `aira unlink
  ...` for unlink) (RED today); the owner file's digest unchanged, no new outbox row, no
  new event. No full-string pin. Mutations: endpoints not swapped; inverse kind named
  instead of forward; `Unlink` left with the old text -> each RED.
- `TestLinkInverseRefusalAdviceWorks`: run the command the refusal names (parse it from the
  message) -> `OK`, and `Relations(A)` shows `blocked-by B`. Mutation: wrong pair in
  `Forward` (e.g. `child -> blocks`) -> RED.
- Exact-text pins elsewhere: grep for `inverse relation kinds are query-only` in tests and
  update them (none found by `grep` outside `relation_ready.go`; builder re-checks).

### 5.2 AIRA-291 (`internal/store`)

- `TestReadyBlocksOnARelationTheIndexHasNotSeen` (the git-merge shape): create A and B;
  write A's file with `A blocks B` directly (`os.WriteFile`, no aira write, so the index
  has no row); `Ready("")` -> B `ready:false`, blockers `[B blocked-by A]`, verdict
  `pass`, an `E_RELATION_INDEX_DIVERGENCE` finding of kind `warning` whose message contains
  `computed relation blockers from the ticket files` (RED today); A `ready:true`. After
  `Check` (which rebuilds) -> no divergence finding, B still blocked. Mutation M1: build
  `blockers` from `s.indexedRelations()` instead of `snapshot.relations` -> B ready -> RED.
- `TestDivergenceAdviceClearsIt` (`internal/core`, through `core.Do`): produce the same
  divergence; take the `ready` finding's message, extract the first backticked `aira ...`
  command, dispatch it through `core.Do` with the parsed verb and flags; then `ready` has
  no divergence finding. RED today (no command in the message). Mutation: the message
  names plain `` `aira reconcile` `` -> `Reconcile` runs without `Rebuild`, the
  divergence survives -> RED.
- `TestSequentialLinksLeaveNoIndexDivergence` (the root-cause measurement as a pin): six
  tickets, ten sequential `Link` calls (a chain plus four from one owner) -> `Check`
  reports `relation-integrity: pass` with no divergence finding, and `Ready("")` has none.
  Mutation M3: skip `replaceRelationIndex` in `markTicketMaterialised` -> RED.
- `TestCheckDivergenceMessageNamesRebuild`: the `check` finding's message contains
  `aira reconcile --rebuild` and does not contain the `ready`-only clause. RED today.
  Mutation: append the `ready` clause in `relationIndexDivergence` -> RED.
- Mutation M2: delete the force-to-`warning` loop in `Ready` -> RED
  (`TestReadyUsesCanonicalRelationsAndReportsStaleIndex` and the first test above).

### 5.3 AIRA-289-B (`internal/daemon`, `cmd/aira`)

- `TestSecondDaemonOnTheSameStateIsRefusedWhateverTheRuntimeDir`: start a `Server` on
  `PathsFromEnvironment(S, runtimeA, ...)`; a second `Server` on `(S, runtimeB)` returns
  `ErrAlreadyRunning` and never binds `runtimeB`'s socket (RED today: it starts).
  Mutation: `LockPath` back under the runtime directory -> RED.
- `TestDaemonsOnDifferentStatesBothRun`: `(S1, runtimeA)` and `(S2, runtimeA)` both start
  (their socket paths differ by state id). Mutation: a lock path that ignores the state
  (for example `runtime/aira/daemon.lock`) -> RED.
- `TestNewDaemonRefusesWhileALegacyDaemonHoldsTheRuntimeLock` (the mixed-version
  regression, 1.2): the test holds `flock` on `<runtimeA>/aira/<id>/daemon.lock` (the
  shape of a running pre-v0.34 daemon); a `Server` on `(S, runtimeA)` returns
  `ErrAlreadyRunning`, binds no socket, and leaves the state lock free afterwards.
  Mutation: skip the legacy lock -> RED; return without releasing the state lock -> RED.
- `TestRunningDaemonHoldsTheLegacyLock`: a `Server` on `(S, runtimeA)` running -> a
  non-blocking `flock` on `LegacyLockPath` gets `EWOULDBLOCK` (the shape of a pre-v0.34
  client's startup lock), and the legacy file's `LockInfo` has the daemon's PID. Mutation:
  release the legacy lock after start, or write no info to it -> RED.
- `TestClientWithAMismatchedRuntimeDirFailsLoudly`: an in-process daemon on `(S, runtimeA)`;
  a `daemonDispatcher` on `(S, runtimeB)` with a short `startWait` and a `spawn` hook that
  fails the test if called -> the response is `E_DAEMON_UNAVAILABLE`, exit 4, its message
  names `runtimeB`'s socket, `runtimeA`'s socket, the daemon's PID and `XDG_RUNTIME_DIR`
  (with the injected `getenv` value); no socket exists at `runtimeB`. RED today (the hook
  is called: the client starts a second daemon). Mutation: no hint (generic
  `E_DAEMON_TIMEOUT`) -> RED.
- `TestClientWaitingOnASlowDaemonKeepsTheTimeout` (the restart / slow database-open
  window): the test holds the state lock and writes `LockInfo` with the client's own socket
  path and its PID, binding nothing; a `daemonDispatcher` on the same paths with a short
  `startWait` -> `E_DAEMON_TIMEOUT`, exit 3, message names the PID and does not mention
  `XDG_RUNTIME_DIR`. Mutation: hint fires on "socket absent" alone, ignoring the recorded
  socket -> RED.
- `TestClientOnAFreshMachineCreatesTheStateDirAndStarts`: state directory absent,
  `XDG_RUNTIME_DIR` elsewhere; the client creates the lock's directory, wins the lock and
  calls the `spawn` hook (which starts an in-process daemon) -> the request succeeds.
  Mutation: drop the client's `MkdirAll` for the lock directory -> RED (the lock never
  opens, the hook is never called, generic timeout). A twin for the daemon's own
  `MkdirAll` (`Server` started with the state directory absent) -> starts; mutation:
  drop it -> RED.
- `TestStatusFromAMismatchedRuntimeSeesTheLockHolder`: `Status` on `(S, runtimeB)` while
  `(S, runtimeA)` runs -> `Running: true`, `Ready: false`. Pin.
- Subprocess test (the measured shape, using the helper-process pattern in
  `cmd/aira/dispatcher_inprocess_test.go:377`): two `aira` client processes with one
  `XDG_STATE_HOME` and different `XDG_RUNTIME_DIR` -> exactly one daemon process exists
  afterwards, and the second client exits 4 with the hint. RED today (two daemons).
  Heavy: run under `aira confine`, serially, with private `HOME`/`XDG_*` under the test's
  temporary directory, never the box daemon.
- Existing tests that construct a daemon lock or read `Status` (`binary_stale_test.go`,
  `shim_daemon_detach_test.go`, `state_dir_agreement_test.go`,
  `dispatcher_inprocess_test.go`) are re-run; any that assumed the lock sits beside the
  socket are updated, not deleted.

### 5.4 Skill text (`internal/core/skill_test.go`, in the existing "skill teaches X" style)

One table-driven test, `TestSkillTeachesV034Rules`, one row per paragraph, each row RED
today (the phrases are absent). Rows assert the facts an agent acts on, not prose:

- 290: `aira mv <id> retired`, the three-hop close path in order, "no delete",
  `E_TICKET_NOT_IN_WORKTREE`, that a deleted file's own relations vanish, "read-only
  views" with `blocked-by`.
- 291: `E_PATH_INTENT_BUSY`, `aira reconcile --rebuild`, and the statement that relation
  blockers are computed from the ticket files.
- 292: `scope-integrity`, `migrated`, `descendant-escaped`, `snap`, `containment=enforced`
  named as not sufficient, and `contained` described as observed (sampled), not proven.
- 289: "no commits", `merge-base`, `env_allow`, `XDG_RUNTIME_DIR`,
  `aira confine -- aira gate run` (only if the 3.6 precondition held), `timeout_ms`,
  `aira run-log`, and that `DBUS_SESSION_BUS_ADDRESS` is not needed.

Mutation: delete any one paragraph -> its row RED. The SKILL.md and guide bytes change,
so `TestSkillVersionCoversArtifactBytes` moves the version, as designed.

Gate: `aira confine -- make ci` (the gate's exact command), plus `go test -race` on
`./internal/daemon/...` and `./cmd/aira/...`.

## 6. Expected yield

- ci-build's AIRA-291 resolves to a clear answer: the UNEVALUATED was the gate subject
  (fixed in v0.33, measured on both builds), and the remaining warning now says what it is
  and how to clear it. The false-pass direction of the index gets its first pin.
- An inverse-kind link becomes a one-step self-correction instead of a source read.
- Throwaway tickets have a documented one-hop void; the close path and its reason are in
  the installed skill, not only in this repository's docs.
- Once the box daemon runs v0.34 and callers use v0.34 binaries, a second daemon on one
  `state.db` cannot start (and older binaries sharing the daemon's `XDG_RUNTIME_DIR` are
  fenced off). A gate, cron job or `ssh` session without the daemon's `XDG_RUNTIME_DIR`
  gets an actionable `E_DAEMON_UNAVAILABLE` naming the daemon's socket (the daemon-routed
  verbs within their start wait, at most 5 s; a nested `aira confine` at launch),
  instead of being admitted silently by a daemon whose ledger cannot see the box's jobs.
- Gate authors learn the snapshot, environment and timeout rules from the skill; if the
  wrap precondition (3.6) holds, the wrap form removes both the environment requirement
  and the queued-timeout trap for gates that drop their nested `aira confine`.
- Snap escapes become something an agent reads, without deciding AIRA-225.

## 7. Deferrals and "not in this batch"

| Item | Why not here |
|---|---|
| AIRA-225 (snap escape: loud signal, refusal, or documented gap) | Open owner fork; 292 documents current behaviour only. |
| Client dials the daemon's socket recorded in the lock file (mismatched clients "just work") | The loud refusal meets the invariant with less machinery; the path is recorded now but only used to word the error. Revisit if the refusal proves common. |
| Remove the legacy runtime-directory lock (3.5) | Needed while pre-v0.34 binaries can run on the box. Suggest a held ticket for a later release; removing it is one deletion plus its two tests. |
| `XDG_RUNTIME_DIR` fallback to `/run/user/<uid>` | Same reason; partial (other differing values still need the refusal). |
| Distinct code for "gate timed out while queued in admission" | Rejected (section 2): needs output parsing or daemon-side ancestry. |
| `mv --through`, a delete/void verb, normalising inverse kinds on input | Rejected (section 2). |
| Bare `E_PATH_INTENT_BUSY` message (no path, no "retry") | Small, separate; the skill now explains it. Suggest folding into the next small batch. |
| **New finding: client-started daemon dies on SIGPIPE after its client exits (1.3)** | Measured symptom, mechanism assumed; affects machines without the systemd service. Suggest a held ticket: give the spawned daemon a stderr that outlives the client (a log file under the state directory, or `/dev/null` plus its existing logging), and a regression test that a client-spawned daemon survives its spawner and later logs. |
| `aira drain wait` / `aira confine-reserve` with a mismatched `XDG_RUNTIME_DIR` still wait for a daemon they cannot reach (3.5 step 7 covers `aira confine` only) | Same defect, other verbs; the finding was about `aira confine`. Suggest a held ticket: reuse `unreachableDaemonError` at their launch. |
| `aira check` reports a lagging index as `relation-integrity: fail` and then heals it in the same call | Existing decision; unchanged. |

## 8. Risks

- **Lock move (289-B).** Any process whose `XDG_RUNTIME_DIR` differs from the service's
  now gets exit 4 instead of silently running against a daemon of its own. On this box no
  such daemon has ever started with the default state (`~/.local/state/aira/run/` is
  absent; only the service daemon runs), so no current workflow relies on it. Cron jobs
  and `ssh` sessions without `XDG_RUNTIME_DIR` must export it
  (`XDG_RUNTIME_DIR=/run/user/$(id -u)`); release notes say so. CI containers (ci-shim)
  are consistent within the container and unaffected. Two containers sharing one state
  directory through a bind mount on one host now get one daemon and a refusal for the
  other (previously two daemons on one SQLite file).
- **Upgrade window and mixed binaries (1.2).** Until the box daemon is restarted onto
  v0.34, a v0.34 client with a different `XDG_RUNTIME_DIR` from the daemon's can still
  start a second daemon (it cannot see the old daemon's lock). The cutover therefore
  restarts the service explicitly (`systemctl --user restart aira-daemon.service`; an
  idempotent `aira install` has skipped the restart before) and confirms the daemon's build
  with `aira version`. After that, a v0.34 daemon holds both locks, so a pre-v0.34 binary
  with the daemon's `XDG_RUNTIME_DIR` (an old worktree build, an `aira mcp` server started
  before the upgrade) waits on the socket instead of starting a daemon. A pre-v0.34 binary
  with a *different* `XDG_RUNTIME_DIR` keeps today's defect until it is rebuilt; release
  notes say "rebuild worktree binaries and restart `aira mcp` servers". The restart itself
  is the owner's or the release session's cutover step, never this build's (no box daemon
  restart from a build session).
- **Service down, mismatched client.** While the unit is down, a mismatched client is now
  refused instead of starting a stray (3.5 step 6); the restart window of the cutover
  therefore refuses such callers rather than forking. A mismatched client that cannot
  identify the unit still forks: the user manager is reachable neither through the
  caller's environment nor at `/run/user/<uid>` (a container without the user's systemd),
  `systemctl` is on neither `PATH`, `/usr/bin` nor `/bin`, or the loaded unit file cannot
  be read. A missing, empty or stale `XDG_RUNTIME_DIR` or `DBUS_SESSION_BUS_ADDRESS`, a
  missing `PATH` and a missing or private `HOME` are NOT such cases (3.5 step 6). That
  residual is the pre-v0.34 behaviour and ends when the unit is up (the state lock then
  fences it).
- **Lock-file record.** `LockInfo.Socket` is new; a lock file written by an older daemon
  has none, and the client then keeps today's `E_DAEMON_TIMEOUT`. No compat obligation
  beyond that.
- **`aira daemon stop` reach.** The lock is per state, so from a mismatched process
  `daemon.Stop` would SIGTERM the state's real daemon. When the enabled unit serves that
  state, `stop` redirects to `systemctl --user stop` instead (round 3); when the unit cannot
  be identified from that process, it still SIGTERMs the lock holder, which on the box is
  the shared service daemon (systemd restarts it after 2 s). Release notes warn (10).
- **Message text.** `E_RELATION_INVALID` and the divergence message change text; codes and
  exits do not. Tests pinning the old strings are updated.
- **Skill length.** About 30 lines of new skill text; each paragraph is pinned by one row
  of one table-driven test and states only measured or read behaviour.

## 9. Stable codes

No new codes; the catalogue (`internal/codes/codes.go`) is unchanged.

| Code | Exit | Use in this plan |
|---|---|---|
| `E_RELATION_INVALID` | 1 | Inverse-kind refusal, now naming the forward command; unknown-kind refusal with the writable list. |
| `E_RELATION_INDEX_DIVERGENCE` | 1 in `check` (`fail`); `warning` in `ready` | Message gains how to clear it; kinds unchanged. |
| `E_DAEMON_UNAVAILABLE` | 4 | Client cannot reach the state's lock holder, and the holder recorded a different socket (mismatched `XDG_RUNTIME_DIR`). |
| `E_DAEMON_TIMEOUT` | 3 | Unchanged otherwise (lock won and the daemon never answered; or the holder recorded this client's own socket and has not bound it yet, now with the holder's PID). |
| `E_TRANSITION_INVALID`, `E_TICKET_NOT_IN_WORKTREE`, `E_PATH_INTENT_BUSY`, `U_GATE_COMMAND_TIMEOUT`, `E_GATE_INVALID` | unchanged | Named in skill text. |

## 10. Release notes (for the build)

Proto unchanged (15). Daemon restart required for 289-B: restart the service explicitly
(`systemctl --user restart aira-daemon.service`) and confirm with `aira version`; do not
rely on `aira install` to do it. Skill reinstall required
(`aira skill install ~/.claude/skills/aira --force`). Rebuild worktree `aira` binaries and
restart `aira mcp` servers: a pre-v0.34 binary run with a different `XDG_RUNTIME_DIR` from
the daemon's can still start a second daemon.
Warn every peer: **stop the service with `systemctl --user stop aira-daemon.service`, never
with `aira daemon stop` from a process whose `XDG_RUNTIME_DIR` differs from the
service's (or is unset).** The daemon lock is now per state directory, so such a process
reads the service's own lock record; v0.34 redirects it to systemctl when it can identify
the enabled unit, but where it cannot (no user manager reachable from that process) it
SIGTERMs the shared service daemon, interrupting every session's daemon work until systemd
restarts it 2 s later. Likewise `aira daemon`
run by hand from another runtime directory for the service's state is now refused (exit 4)
instead of serving a stray.
Release note for every peer: a process that runs `aira` with a different
`XDG_RUNTIME_DIR` from the daemon's (or none) now fails with `E_DAEMON_UNAVAILABLE` naming
the cause, instead of starting a second daemon (and `aira confine` refuses at launch
instead of waiting for a daemon it cannot reach); export `XDG_RUNTIME_DIR` in cron jobs, and
list it in a gate's `env_allow` when the gate command calls `aira`. The service is identified through the user's systemd manager (the unit file it has loaded, its `FragmentPath`), reached through the caller's environment or else at `/run/user/<uid>` with `systemctl` from `PATH` or `/usr/bin`, `/bin`: a missing, empty or stale `XDG_RUNTIME_DIR` or session bus address, a missing `PATH`, or a private `HOME` does not hide it. Reply to ci-build per
ticket: 290(a) use `aira mv <id> retired`, no delete by design; 290(b) refusal now names
the command; 290(c) close path stays three hops by decision (AIRA-215); 290(d) already
shipped in v0.31; 291 the UNEVALUATED was the gate subject, fixed in v0.33, and the
divergence after `git pull` is cleared by `aira reconcile --rebuild` (or `aira check`),
not plain `aira reconcile`, and never changes readiness;
292 read `scope-integrity`, prefer non-snap tools; 289 skill section, wrap form, and the
new loud refusal (daemon-routed verbs after at most 5 s; `aira confine` at launch, `ran=no`). Notify deploy and speed per the release rule.

## 11. Review log

Standing questions for reviewers: what can be removed while still solving the four
reports; what is the from-scratch minimum; can a lagging or stale relation index ever make
a ticket ready that the files block (the false-pass direction); can two daemons ever serve
one `state.db`.

### Plan review, round 1 (on v1, `4788ae0`)

Reviewers: Sol (`high`, static, read-only; did not reproduce the measurements): verdict
line **REVISE**, filed as PASS_WITH_CHANGES. Second reviewer: **PASS_WITH_CHANGES**, one
P1. Every finding was checked against the code before it was accepted.

| # | Source, severity | Finding | Checked against | Disposition |
|---|---|---|---|---|
| 1 | Sol high | The lock move fixes differing runtime dirs only among new daemons; a v0.33 daemon on the old lock plus a new client with another runtime dir still starts a second daemon. Needs migration protection and a mixed-version test before claiming "impossible". | `paths.go:443` (lock beside the socket); `server.go:385-403` (one lock). True. | **Accepted.** 1.2 mixed-binary analysis; the daemon also holds the legacy lock (3.5 step 3); tests `TestNewDaemonRefusesWhileALegacyDaemonHoldsTheRuntimeLock`, `TestRunningDaemonHoldsTheLegacyLock`; "impossible" removed from 2, 4, 6; the residual (old daemon still running, old binary with another runtime dir) is stated and closed by an explicit cutover restart (8, 10). |
| 2 | 2nd P1 | The divergence message and skill say plain `aira reconcile` refreshes the index; it does not. | `Store.Reconcile` = outbox only (`store.go:2557-2664`); index rebuilt only by `Store.Rebuild` (`store.go:2756`, `:3055`) under `reconcile --rebuild` (`core.go:2223`) and `check` (`internal/store/check.go:245`). True. | **Accepted.** Message, skill, 1.1, 2, release notes name `aira reconcile --rebuild` (or `aira check`); `TestDivergenceAdviceClearsIt` runs the named command through `core.Do`, with the plain-`reconcile` mutation. |
| 3 | Sol high | Diverged-index PASS needs an explicit policy decision; deleting the relation owner yields `ready:true`/PASS (`m3_test.go:1022`), so "deleting the file makes ready fail" is not general. | `m3_test.go:1022-1048`; decided in `ef8202f` (Phase 1). True. | **Accepted as decision D1 (1.1)**: PASS stays (files are canonical; prior decision not re-opened); the skill and the 290(a) row now say a deleted owner's relations vanish. An alternative (missing owner -> unevaluated) is in section 2 as rejected. Reversible, so not escalated to the owner. |
| 4 | Sol medium | Containment wording overclaims: `contained` is sampled (50 ms) and neither it nor `descendant-killed` proves the whole tree stayed capped; "snap tools always do this" overgeneralises. | `runner_linux.go:1582-1592` (accepted coverage gap, documented). True. | **Accepted.** 3.4 says "observed", names the 50 ms sampling, and limits the snap claim to the measured `terraform` case; the 292 row says AIRA-225's `descendant-escaped` is reported-as; skill-test row asserts the "observed" wording. |
| 5 | Sol medium | Protocol is already 15; readiness also uses gate results; `env_allow` passes only present, non-coordination variables; PATH staleness needs differing values; wrapping fails if a configured cgroup parent puts the checker outside the outer scope. | `protocol.go:180` (15); `allowListedEnvironment` (`gate_command.go:167-181`, `os.LookupEnv`, `StripCoordinationEnv`); `linuxScopeBackend.Create` (`cgroup_linux.go:173-192`: `run.cgroup_parent` overrides the caller's cgroup). All true. | **Accepted.** Header, I7 and 10 say 15; I1 and D1 restrict the claim to relation blockers; 3.6 env and PATH bullets qualified; wrap bullet names `run.cgroup_parent` (not set in ci-build's config, by grep). Self-found while checking this: the wrap form itself was never measured (cgroup v2 may refuse a child scope under a populated outer scope), so 3.6 now has a build precondition with a fallback. |
| 6 | 2nd P2 | The wrap removes the queued-timeout trap only if the gate command drops its own nested `aira confine`. | 3.6 v1. True. | **Accepted.** Stated in 2, 3.6 and 6. |
| 7 | 2nd P2 | The hint condition `!spawned && socket absent` also matches a same-runtime daemon still binding (restart, slow database open), changing exit 3 to 4 and blaming `XDG_RUNTIME_DIR`. Name both causes and the holder PID. | `server.go:407-434` (lock info written at :407, socket removed at :410, `OpenDB` at :413, bind at :434). True. | **Accepted, refined.** Instead of a message naming two guesses, the daemon records its socket path in `LockInfo`; the client says `E_DAEMON_UNAVAILABLE` only when the holder's socket differs, else keeps `E_DAEMON_TIMEOUT` plus the holder PID (3.5 step 4). Test `TestClientWaitingOnASlowDaemonKeepsTheTimeout`. Cost: one JSON field, no dialling. |
| 8 | 2nd P3 | Mixed-binary hazard after the lock move: a pre-v0.34 client finding no socket wins the old lock and `server.go:410` unlinks the live daemon's socket. Release note: rebuild worktree binaries. | `server.go:410` (`os.Remove(SocketPath)` after winning the lock); old clients' `tryStartupLock`. True. | **Accepted.** Same fix as row 1 (legacy lock held by the daemon) plus the release note. |
| 9 | 2nd P3 | "Unopenable lock reported at once" and its test are extra scope in the correctness-critical set. | 3.5 v1 step 3. | **Accepted.** The three-way `tryStartupLock` split and `TestUnopenableLockIsReportedAtOnce` are removed; I5 trimmed. The fresh-machine test still goes RED without the client's `MkdirAll` (the lock never opens, generic timeout). |
| 10 | 2nd P3 | Four prose-grep skill tests are ceremony; one table-driven test. | 5.4 v1. | **Accepted.** One table-driven test, one row per paragraph. |
| 11 | Sol low | Removable scope: no delete verb, shortcut, automatic reconciliation, relation table, discovery protocol, timeout machinery; trim repeated rationale and exact-prose pins; keep behavioural regressions. | Plan v1. | **Accepted.** None of those was in v1; full-string error pins replaced by command-substring and behavioural assertions (5.1, 5.2). Rationale left as is where it records a decision. |
| 12 | Sol low | From-scratch minimum (files-based blockers, explicit uncertainty, disposable index, actionable messages, one state-scoped lock with safe migration, concise docs). | Section 2. | **Accepted**; matches section 2 after rows 1 and 7. |
| 13 | Sol low | Missing evidence and timeout stay unevaluated; PASS means evaluation succeeded, not `ready:true`; the missing-index-blocker test is valuable. | 1.1, 5.2. | **Accepted**; wording in D1 and I1. |
| 14 | 2nd INFO | Root causes and code citations verified; lock move is the minimum; skill-only items collapse correctly. | n/a | Noted. |

No finding was rejected. Owner-level forks: none irreversible. D1 (PASS on a diverged
index, following the Phase-1 decision) and the legacy lock (temporary, removable) are
recorded here and are reversible.

### Build record (2026-10-11)

- **3.6 wrap precondition: held.** Against a private daemon (private `HOME`, `XDG_STATE_HOME`,
  `XDG_RUNTIME_DIR` under `~/tmp/cli34/`; probe `~/tmp/cli34/probe_wrap.sh`),
  `aira confine -- aira gate run g1` returned `PASS` (trusted, RUN-3, canary RUN-4), and the
  gate's run scope was `.../aira.slice/.aira-CONFINE-<outer>/.aira-RUN-3`, nested under the outer
  confine scope. A plain `gate run` nests under the caller's own cgroup (RUN-1). The wrap
  recommendation is therefore kept. (The private `HOME` needed a copy of `aira.slice`'s unit
  file for `aira confine` to resolve the slice; that is a property of the probe, not of the wrap.)
- Built as planned: no deviations from the plan's design. The plan's `E_DAEMON_UNAVAILABLE`
  wording is the test-pinned message; the exact string is the one in `lockHolderError`.

### Build review, round 1 (on `1d562c6`) and fixes

Three P2 findings from the build reviewers; each was checked against the code and the first
two re-measured before the fix. All three accepted.

| # | Finding | Checked against | Disposition |
|---|---|---|---|
| 1 | Concurrent `aira link` to one owner file can fail `E_WRITE_CONFLICT` and leave a pending row; every later write is `E_PATH_INTENT_BUSY` until `aira intent-retire`, which the skill never mentions. | `relation_ready.go` (read, then prepare), `store.go` `preparePathMutationEventKind` (checks only for a pending row), `materialiseIntent` (`ErrWriteConflict`). True; the pre-fix tests are RED. | **Fixed (3.7)** at the root, not by documenting `intent-retire`: prepare refuses a stale precondition before inserting. Tests `TestStalePreconditionIsRefusedWithoutLeavingAPendingIntent`, `TestStaleRelationPreconditionIsRefusedWithoutLeavingAPendingIntent`. The skill names `E_WRITE_CONFLICT` and says a refused write leaves nothing pending. Two existing fixtures that built a conflict through this very window (`seedConflictedRoutingIntent`, `TestRebuildResurrectsARetiredIntentOnlyAsMaterialised`) now build it from an occupied path / the real digest. |
| 2 | A nested `aira confine` without the daemon's `XDG_RUNTIME_DIR` is not refused; it waits with no deadline. The skill, 3.6, 6, 10 and the ci-build reply claimed otherwise; 1.2's "not reproduced" was an artefact of the probe. | `cmd/aira/main.go` (`AdmitSocketPath = paths.SocketPath`), `admission_linux.go` (`warnDaemonWait`, reconnect without deadline). True. | **Fixed (3.5 step 7), option (a):** refuse at launch when the holder or the unit is known to serve another socket. 1.2, 3.6, 6 and 10 corrected; test `TestConfineWithAMismatchedRuntimeDirIsRefusedNotHeld`. `drain wait`/`confine-reserve` left open (section 7). |
| 3 | While the service is down, a mismatched client forks a stray that takes the state lock; the service then restarts every 2 s and the box watchdog stays off. | `daemon_command.go:47-52` (ErrAlreadyRunning exits 0), unit `Restart=always`, `spawnDaemon`. True by reading; the first half reproduced in-process by the reviewer. | **Fixed (3.5 step 6)**: `spawnDaemon` refuses when the enabled unit serves this state through another socket. Tests `TestClientNeverForksAStrayWhenTheServiceServesThisStateElsewhere`, `TestClientStillForksItsOwnDaemonWhenTheServiceServesAnotherState`; `TestSpawnDaemonDefersOnlyWhenEnabledAndIdentityMatches` row "identity divergent" now expects the refusal. Residual in section 8. |

### Build review, round 2 (on `d8bc5b3`) and fixes

Five P2 findings from four reviewers; each was checked against the code, and the first
three were re-measured by the reviewers against the real `systemctl`. All accepted.

| # | Finding | Checked against | Disposition |
|---|---|---|---|
| 1 | Round-1 finding 3 (stray daemon while the service is down) is fixed only for a client that reaches the user bus. The fence and the confine launch check both start with `systemctl --user is-enabled`, which fails with "Failed to connect to bus: No medium found" when `XDG_RUNTIME_DIR` and `DBUS_SESSION_BUS_ADDRESS` are both unset (cron, `env -i`, a gate whose `env_allow` omits them), so `managed=false` and the client forks a stray, as before. Three reviewers measured it with the real `systemctl`. Round-1 finding 2 stays open for a confine launched while the service is down (no holder, unit invisible). The spec and the skill overclaimed ("HOME not visible" was named the only residual). | `internal/daemon/service.go` (`ServiceIsEnabled`, `ManagedServicePaths`, `RunSystemctl`). True. The production unit does not bake `XDG_RUNTIME_DIR`, so the `show-environment` call needed the same reach. | **Fixed at the runner:** `daemon.RunSystemctl` repeats a failed run once with `XDG_RUNTIME_DIR=/run/user/<uid>` (the manager's own directory) when the caller's differs or is unset; one run when the caller already reaches the manager. Covers `is-enabled`, `show-environment` and `start`. Tests (a `systemctl` stub on `PATH` that fails like the real tool without the bus, not a stub that answers "enabled" whatever the environment): `TestSystemctlProbeReachesTheUserManagerWithoutARuntimeDirOrBus`, `...FromAnotherRuntimeDir`, `...RunsOnceWhenTheCallerEnvironmentWorks`, `...KeepsADisabledUnitDisabled`, `TestManagedServicePathsResolvesTheUnitWithoutARuntimeDirOrBus`, `TestClientWithNoBusNeverForksAStrayWhenTheServiceServesThisState`, `TestConfineLaunchWithNoBusIsRefusedWhenTheServiceServesThisState`. Spec 3.5 step 6, section 8, the skill and the release note state the real residual. |
| 2 | `os.Lstat(paths.SocketPath)` in `unreachableDaemonError` and `lockHolderError` skips the refusal whenever any file sits at the client's socket path. With `XDG_RUNTIME_DIR` unset that path is under the state directory, so a stray killed uncleanly leaves it across reboots; every confine then waits forever again. Mutation MC2 (delete the guard) stayed green. | `dispatcher.go` both guards; `Status` dials the socket (100 ms) and sets `Ready`. True. | **Fixed:** both guards use `Status.Ready` (a socket that answers is reachable; a file is not a daemon). Tests `TestADeadSocketFileDoesNotHideAnUnreachableDaemonFromConfine`, `...FromAVerb`, `TestAnAnsweringSocketIsNeverRefusedByTheLaunchCheck` (mutation: restore the Lstat guard, or drop the Ready guard, -> RED). |
| 3 | Mutations MC3 (service branch disabled), MC4 (empty-recorded-socket guard dropped), MC5 (`Running` guard dropped), MC6 (state-id check dropped), MC7 (holder that recorded the client's own socket refused), MC8 (unit serving the client's own socket refused) all survived the confine launch check. | `dispatcher.go` `unreachableDaemonError`. True (measured by the reviewer). | **Fixed:** `TestConfineLaunchCheckDecisionTable`, eight rows, each a mutation's target (holder elsewhere refuse; holder on own socket wait; holder recording nothing wait; stale record with no lock wait; unit for this state elsewhere refuse; unit on own socket wait; unit for another state wait; unit not enabled wait). Each of MC3-MC8 is now RED. |

Findings 1 and 2 of the round are one root cause (reviewer duplicates), noted once.

### Build review, round 3 (on `1a6685a`) and fixes

Fourteen P3 items from three reviewers, several of them correctness-relevant. Each was
checked against the code, and the `systemctl` claims against the real tool (systemd 255,
read-only `is-enabled`/`show` queries only). The measured connection rule of `systemctl
--user`: with `DBUS_SESSION_BUS_ADDRESS` present it goes through that address only (a stale
address fails even with `XDG_RUNTIME_DIR=/run/user/1000`, and an EMPTY address fails too,
"Connection refused"); without it, it reaches the manager only with
`XDG_RUNTIME_DIR=/run/user/<uid>`. `is-enabled` of a missing unit exits 4 (`not-found`);
`show -p FragmentPath` of a missing unit prints `FragmentPath=` and exits 0. The test stubs
now model this rule (round 2's stub counted any non-empty address as reachable). Each
mutation below was applied alone to the committed fix and turned the named test RED
(harness `~/tmp/c34fix/mut.py`, scratch).

| # | Finding | Checked against | Disposition |
|---|---|---|---|
| 1 | The retry keeps a wrong `DBUS_SESSION_BUS_ADDRESS`, so it fails too and the client forks; the short-circuit also skips it when `XDG_RUNTIME_DIR` is already the manager's. | Real tool: stale and empty addresses fail with the manager's runtime dir (measured). True. | **Fixed:** the retry runs with `XDG_RUNTIME_DIR=/run/user/<uid>` and no bus address, and is skipped only when the caller's environment is already exactly that. `TestSystemctlProbeDropsAStaleBusAddressOnTheRetry` (4 rows). Mutations b1 (retry keeps the address) and b2 (old short-circuit) RED. |
| 2, 13 | The probe reads the unit FILE from `$HOME` but asks the real manager, so a private-`HOME` sandbox with its own aira-managed unit makes the serve guard `systemctl --user start` the box's real unit. | `service.go` (`readFile(home/.config/...)`), the `daemon_command.go` serve guard; the reviewer's three real starts. True. | **Fixed (the cleaner form, item 13):** the unit file read is the manager's `FragmentPath`; `HOME` no longer identifies the unit, which also removes the "no HOME" residual. Measured end to end with the built binary in a private-`HOME` sandbox (real `systemctl` behind a logging wrapper that refuses start/stop/restart; stale bus address): is-enabled, show FragmentPath and show-environment each failed once and succeeded on the retry; the FragmentPath named the box's unit (another state); the sandbox served its own daemon; no `start` was attempted. Tests `TestManagedServicePathsReadsTheUnitTheManagerLoaded`, `TestDaemonServeInAPrivateHomeNeverStartsTheManagersUnit`; mutation a1 (read the unit under `HOME`) RED in both. |
| 3 | 3.7's idempotent clause is unpinned (MS2 green). | `store.go` `preparePathMutationEventKind`. True. | **Fixed:** `TestAnIdenticalRacedWriteIsNotAConflict` (two writers, same bytes from one stale read: both OK, no pending row). Mutation d1 (MS2) RED. |
| 4 | The skill says, unqualified, that a refused write leaves nothing pending. A materialise-time `E_WRITE_CONFLICT` (a non-aira change, or `aira create` onto an occupied path) leaves the row pending until `aira intent-retire`, which the skill never names. "Satisfies any `blocks` relation that points at it" has the direction backwards. | `materialiseIntent`; `seedConflictedRoutingIntent`; `satisfied(prerequisite.Status)` with the prerequisite being `relation.From`. True. | **Fixed:** the skill names the pending case and `aira intent-retire <path>` (the path selector of `resolvePendingIntent`); the retired wording is now "after `aira link A blocks B`, `B` stops being held back once `A` is retired". Spec 1, 3.1 and 3.7 corrected. Skill-test rows added; mutations e1 and e2 RED. |
| 5, 9 | With no `PATH` the probe is blind (`exec.LookPath` fails at once), the gate shape the skill recommends. | `RunSystemctl` runs a bare `"systemctl"`. True. | **Fixed:** fall back to `/usr/bin/systemctl`, then `/bin/systemctl`. `TestSystemctlIsFoundWithoutPATH` (stub at a fallback path, NOT on `PATH`, `PATH` unset) and `TestRealSystemctlIsFoundWithoutPATH` (real tool, `PATH` unset, location checked independently of the list). Mutations c1 (no fallback) and c2 (empty list) RED. c2 was GREEN at first because the test skipped on the very list it tests; fixed. |
| 6, 10 | The `status.Ready` guard in `lockHolderError` is unpinned (MB/D1 green). | `dispatcher.go` `lockHolderError`. True. | **Fixed:** `TestAnAnsweringSocketKeepsThePlainTimeout` (the socket answers, the exchange times out, the holder recorded another socket or this one: plain `E_DAEMON_TIMEOUT`, exit 3, no false "has not bound"). Mutation d2 (MB) RED. |
| 7 | `aira daemon stop` from another `XDG_RUNTIME_DIR` SIGTERMs the box service daemon, and the release note does not warn. | `daemon_command.go` stop (`ServiceIdentityMatches` compares sockets); `daemon.Stop` reads the state lock. True. | **Fixed in code, and warned:** stop redirects to systemctl whenever the enabled unit serves this state (`ManagedServiceForState`); the release note warns about the residual where the unit cannot be identified. `TestDaemonStopFromAnotherRuntimeDirNeverSignalsTheServicesDaemon` (a child `sleep` plays the daemon and must survive); mutation f2 RED. The same rule now guards serve (a reviewer gap: a hand-run serve from another runtime dir served a stray while the service was down): `TestDaemonServeFromAnotherRuntimeDirRefusesTheServicesState`, mutation f1 RED. |
| 8 | The comment and round-2 row 1 ("one run when the caller already reaches the manager") overstate: a disabled or missing unit always costs two runs. | `is-enabled` exits 1 or 4 for a disabled or missing unit (measured). True. | **Fixed:** the comment states the real cost (one run on success; two on failure unless the caller's environment is already the manager's). Round-2 row 1 is history and stays as written; this row corrects it. |
| 11 | When both runs fail an error must be returned; S6 (the retry swallows it) was green. | `RunSystemctl`. True. | **Fixed:** `TestSystemctlFailureInBothEnvironmentsIsReported` (an error is returned, and it is the first run's). Mutations S6 and S6b (return the retry's error) RED. |
| 12 | Simplification: S3 (no short-circuit) and S4b (always one forced run) were green; pin the reason for trying the caller's environment first, or reduce to one run. | `RunSystemctl`. True. | **Pinned, not reduced:** the caller's environment is the only way to a manager that is not at `/run/user/<uid>` (`TestSystemctlProbeTriesTheCallersEnvironmentFirst`, S4b RED); the short-circuit skips only an identical retry (`TestSystemctlProbeDoesNotRepeatAnIdenticalRun`, S3 RED). |
| 14 | Nit: an unused parameter in `TestConfineLaunchCheckDecisionTable`'s `unit`. | True. | **Fixed:** one `enabledUnit` stub value. |

### Accepted residuals (v0.34)

Left on purpose; each is bounded and stated:

- **Unit identity not establishable** from a process: no user manager reachable through
  its own environment or at `/run/user/<uid>`, no `systemctl` on `PATH`, `/usr/bin` or
  `/bin`, or the loaded unit file unreadable. While the service is down such a client
  forks as before v0.34; `aira daemon serve` serves; `aira daemon stop` SIGTERMs the
  state's lock holder (release-noted). All three end once the unit is up and the state
  lock fences.
- **A stray already holding the service's state** (forked by such a process) cannot be
  stopped with `aira daemon stop` from a process that does identify the unit: that is
  redirected to systemctl, which does not own the stray. Kill it by the PID that `aira
  daemon status` prints.
- **A materialise-time `E_WRITE_CONFLICT` leaves its row pending** until `aira
  intent-retire <path>` (3.7): non-aira edits in the prepare-to-materialise window, and
  `aira create` onto an occupied path. The skill says so.
- **Status's momentary flock probe** (now on every confine launch whose socket does not
  answer) can make a daemon starting at that instant read `ErrAlreadyRunning` and exit 0;
  systemd restarts the service 2 s later. Same pattern as the client's startup lock; not
  measured.
- **Two `systemctl` execs** for a disabled or missing unit when the caller's environment
  is not exactly the manager's (milliseconds; only at spawn and at a confine launch whose
  socket does not answer).
- **Drop-ins are not read:** the identity comes from the loaded unit's main file
  (`FragmentPath`); an `Environment=XDG_STATE_HOME` override in a drop-in is not seen.
  `aira install` writes none.
- `aira drain wait` and `aira confine-reserve` (section 7) are still not covered by the
  launch refusal.
