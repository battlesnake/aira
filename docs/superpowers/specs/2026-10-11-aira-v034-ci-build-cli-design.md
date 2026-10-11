# AIRA v0.34 ci-build CLI batch: ticket voiding, inverse-link refusal, index warnings, snap note, gate environment

Status: PLAN v1 (2026-10-11), awaiting plan review. Batch of the four remaining ci-build
field reports (owner rule: batch small independent fixes, one review and test cycle).
Tickets, in the peer's priority order: AIRA-290, AIRA-291, AIRA-292, AIRA-289. Base: master
`2e5b72c` (the v0.33 code, `cf323d9`, plus the ticket-close commit). The installed box
client and daemon are both `cf323d9` (`aira version`).

Requested by the peer session ci-build (repository `~/claude/tooling`, prefix `CI`, 116
tickets, one command gate `clops-gate` whose command runs a nested `aira confine`).

**One item is correctness-critical: 289-B (one daemon per state directory).** It changes
which file the daemon lock lives in and how a client reports an unreachable daemon. It is
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
| 290(a) no way to delete or void a throwaway ticket | No: the void exists, it is undocumented | `aira mv PX-8 retired` from `planned` -> `ok`, rc 0 (measured). `retired` is a legal next status from every open status and from `done` (`internal/domain/ticket.go:174-182`); a retired ticket is never ready (`workable`, `relation_ready.go:433-435`) and satisfies any `blocks` edge pointing at it (`satisfied`, `relation_ready.go:437-439`). The design already rules out deletion: "an allocated ID resolves to a live ticket or an explicit `retired`/`superseded` one, never nothing" (`2026-08-07-aira-design.md:106`). Deleting a file instead (measured, `rm .aira/tickets/PX-7.md`): `aira get PX-7` -> `E_TICKET_NOT_IN_WORKTREE`; `aira check` -> `fail` (`E_RELATION_TARGET_MISSING` on two relations, `E_RELATION_INDEX_DIVERGENCE`); `aira ready` -> `FAIL`. The installed skill never mentions `retired` (0 hits). | Skill text only; no delete verb |
| 290(b) `aira link A blocked-by B` refusal does not say what to type | Yes, measured | `link PX-3 blocked-by PX-5` and `unlink PX-2 blocked-by PX-1` -> `E_RELATION_INVALID: inverse relation kinds are query-only`, exit 1; same text for `child`, `duplicated-by`, `superseded-by`, `resolved-by`. Source: `Store.Link` and `Store.Unlink`, `internal/store/relation_ready.go:127-129` and `222-224`. | Build |
| 290(c) closing a ticket takes three `aira mv` calls | Yes, by design | `mv PX-8 done` from `planned` -> `E_TRANSITION_INVALID: planned -> done is not a legal transition; from planned the legal next statuses are: in-progress, retired, superseded` (measured; successor list shipped in v0.31). AIRA-215 decided "the `planned -> done` edge stays refused (decided, not deferred)". `mv` enforces nothing beyond the transition table (`core.go:1686-1696`), so the table is the whole lifecycle gate. | Skill text only; no shortcut |
| 290(d) `aira <verb> --help` errors | No: fixed in v0.31 (AIRA-211) | `link --help`, `mv --help`, `create --help`, `ready --help`, `reconcile --help` each print the verb's usage (measured). | Drop |
| 291 bulk `aira link` leaves `E_RELATION_INDEX_DIVERGENCE` and `aira ready` UNEVALUATED until `aira reconcile` | The divergence is real but is not caused by `link`; the UNEVALUATED was the gate and is fixed in v0.33 | See 1.1. | Build: message + honesty pin + skill text |
| 292 snap-packaged tools escape confine | Yes, measured | `aira confine -- terraform version` (`/snap/bin/terraform`, box daemon, v0.33) -> trailer `containment=enforced cap=enforced(64G) ... scope=placed scope-integrity=migrated ...`, rc 0. `migrated` means the job's own process left the scope (`classifyLaunchScopeIntegrity`, `internal/runner/runner_linux.go:1594-1600`); AIRA-225 recorded `descendant-escaped` for the same tools. The skill never mentions `scope-integrity` (0 hits). The policy fork (loud signal, refusal, or documented gap) is AIRA-225's and stays with the owner. | Skill text only |
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
   and no divergence afterwards (measured).
3. **The divergence comes from git, not from aira.** The relation index is per worktree. A
   ticket file changed by git (merge, pull, checkout, rebase) leaves that worktree's index
   behind the file until something reconciles. Measured: links made and committed in a
   linked worktree, then `git merge --ff-only` into the main checkout -> `aira ready` in
   the main checkout: envelope `PASS`, five rows carry `E_RELATION_INDEX_DIVERGENCE` with
   kind `warning`, and the newly blocked tickets are correctly `ready:false` with their
   blocker listed. `aira check` then reports `relation-integrity: fail` and reconciles;
   the next `aira ready` has no divergence finding. ci-build's flow matches: tooling keeps a
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

On the orchestrator's constraint "ready must never be PASS on a diverged index": `ready`'s
PASS is established from the files, never from the index, so a lagging index cannot make it
wrong. Turning divergence into `unevaluated` would be a false unevaluated: exactly the
symptom reported, for a condition that cannot change the answer, and it would reverse the
Phase-1 decision pinned by `TestReadyUsesCanonicalRelationsAndReportsStaleIndex`. What
honesty does require is a pin that the index can never make a ticket ready that the files
block. No such test exists today (the existing one covers only the opposite direction);
5.2 adds it.

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

Not reproduced: the "hang". On this box no second daemon has ever run with the default
state: `~/.local/state/aira/run/` does not exist, and the daemon never deletes its runtime
directory. Before v0.33 a failing gate showed no output (AIRA-288), and a queued confine
inside a gate looks like a hang until `timeout_ms` fires (289(c)); either may explain the
report. The peer's gate now lists `DBUS_SESSION_BUS_ADDRESS`, `HOME` and `XDG_RUNTIME_DIR`
(sorted), so it reaches the box daemon.

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
  confinement: wrap the gate run (`aira confine -- aira gate run <id>`), which needs no
  extra environment and does its admission wait before `timeout_ms` starts.
- **289(b):** key the daemon lock on the state directory (one path in
  `PathsFromEnvironment`), so a second daemon on one `state.db` is impossible, and have a
  client that cannot reach the lock holder say why instead of timing out generically.

The plan below is that minimum. It adds no verb, no flag, no stable code, no wire field and
no storage, and it deletes no behaviour anyone relies on.

| Item | Alternative | Why rejected |
|---|---|---|
| 290(a) | An `aira delete` / `aira void` verb | The design requires every allocated ID to resolve to a live, `retired` or `superseded` ticket, never nothing (`aira-design.md:106`). Removing the file leaves `E_TICKET_NOT_IN_WORKTREE` and dangling relations (measured above); git history keeps the file anyway. `retired` already does what a void needs in one hop, and is reversible only by a new ticket, which is the honest record. |
| 290(a) | Hide `retired` tickets from `aira list` by default | Not asked; `aira list status:planned` (or any status filter) already narrows the view. A default filter would make "where did my ticket go" a new question. |
| 290(b) | Accept the inverse spelling and store the forward relation (normalise on input) | The Phase-1 rule is that inverse kinds are query projections and are never written (`ValidRelationKind` doc, `ticket.go:318-319`); one written spelling keeps CLI, MCP and the journal target (`B:blocks:A`) identical. The peer asked for the error to name the canonical form; a refusal that names the exact command costs one retry and changes no stored state. |
| 290(b) | A second relation table mapping inverse to forward | `Inverse()` already encodes the pairs; looping over the five forward kinds that have a distinct inverse reuses it (3.2). |
| 290(c) | `aira mv <id> done --through` (walk every legal hop in one call) | Reaches `done` from `planned` in one call, which is what AIRA-215 decided to refuse; `mv` has no gate but the transition table, so a multi-hop flag would empty `in-review` of meaning. The close path stays three journalled hops; throwaways need one (`retired`). |
| 291 | Have `link` call reconcile after writing | `link` already writes its file's index rows in the same transaction (1.1); a reconcile would only re-do that, plus scan every worktree with git. It would not touch the cause (git edits). |
| 291 | Let `ready` (daemon-routed) refresh the index for files whose digest changed | Makes a read verb write; the index changes no readiness answer, so the write buys only the removal of a warning. `aira reconcile` / `aira check` already do it, and they are client-routed (`routing.go:62`) because reconciling runs git (`git worktree list`, `for-each-ref`), which the daemon does not do in callers' repositories. |
| 291 | Make divergence `unevaluated` in `ready` | A false unevaluated (1.1, last paragraph): the reported symptom, for a condition that cannot change the answer. |
| 291 | Drop the divergence warning from `ready` (keep it in `check`) | Removes the only `ready`-side signal that the index lags; the cheaper fix is a message that explains itself. Kept, reworded. |
| 291 | Rename the warning `W_RELATION_INDEX_DIVERGENCE` in `ready` | Two codes for one condition (it is a `fail` in `check`, by an existing decision). The message carries the meaning instead. |
| 292 | Decide AIRA-225 here (loud signal or refusal for snap escapes) | An open owner fork on another ticket; 292 asks only for the note. |
| 289(b) | Document "put `XDG_RUNTIME_DIR` in `env_allow`" and change nothing | Leaves a silent second daemon with a blind admission ledger for every caller that forgets: gates, cron, an `ssh` session without `pam_systemd`, `env -i`. "Not established" must be loud; here it is silent. |
| 289(b) | When `XDG_RUNTIME_DIR` is unset, fall back to `/run/user/<uid>` if it exists | Fixes the common case but not the invariant: any other differing value still starts a second daemon. Not needed once the lock is per state; deferred. |
| 289(b) | Record the socket path in the lock file and let the client dial the recorded socket | Makes mismatched clients "just work", but adds a discovery path and a trust question (dialling a path read from a file). The loud refusal is smaller; deferred (section 7). |
| 289(b) | A second lock in the state directory, keeping the runtime one | Two locks for one rule. Moving the one lock is a single path change; the socket keeps its directory. |
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
  open status (and from `done`). `retired` is terminal, never ready, and satisfies any
  `blocks` relation that points at it. There is no delete: IDs are never reused, and every
  allocated ID must resolve to a live, retired or superseded ticket. Deleting the file
  instead leaves `aira get` answering `E_TICKET_NOT_IN_WORKTREE` and any relations
  dangling, so `aira ready` fails with `E_RELATION_TARGET_MISSING`.
- Relations are written only in their forward form (`blocks`, `parent`, `relates`,
  `duplicates`, `supersedes`, `resolves`); the inverse names (`blocked-by`, `child`, ...)
  are read-only views, and a refusal names the forward command to run instead.
- A relation is stored in the lower-ID ticket's file, so concurrent writes to one file are
  refused `E_PATH_INTENT_BUSY`: run bulk links one after another, or retry the refused ones.
- When git changes ticket files (a merge, pull, checkout or rebase), `aira ready` may carry
  `E_RELATION_INDEX_DIVERGENCE` warnings until `aira reconcile` (or `aira check`) refreshes
  the derived index. Readiness is always computed from the ticket files, so these warnings
  never change a verdict.

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
  suffix, `; the derived index lags the ticket files (for example after a git merge, pull
  or checkout); aira reconcile refreshes it`. This text reaches `check` and `ready` alike.
- `Store.Ready`, in the existing loop that forces divergence findings to `warning`
  (`relation_ready.go:496-501`): also append `; ready computed this verdict from the
  ticket files, so this warning does not change it`.

No behaviour change: same codes, same kinds, same verdicts. The new test in 5.2 pins the
false-pass direction (a `blocks` edge in a file but absent from the index still blocks).
`link` itself is unchanged; its root-cause measurement becomes a committed test (5.2).

### 3.4 AIRA-292: read `scope-integrity`, not `containment`

`internal/core/skill.go`, section "Reading a confined job's outcome" (`skill.go:328-334`),
one new paragraph after the `ran=no` paragraph:

- `containment=enforced` describes the scope AIRA created; whether your process stayed in
  it is `scope-integrity=`. `contained` (or `descendant-killed`: a straggler left at the
  end was killed) means the cap and `aira confine --kill` reached the job. `migrated` (the
  job's own process left the scope) or `descendant-escaped` (a child left it) means they
  did not: the memory cap does not bind the moved process and `--kill` cannot reach it.
  `unverified` means it could not be established; read it as "not shown to be contained".
- Snap-packaged tools always do this: snapd moves anything run from `/snap/bin` (on this
  box, `terraform` and `gcloud`) into its own `snap.*.scope`. Measured:
  `aira confine -- terraform version` -> `containment=enforced ... scope-integrity=migrated`,
  exit 0. For heavy work prefer a non-snap build (a distribution package or the vendor's
  tarball). Whether AIRA should refuse or flag this is open (AIRA-225).

### 3.5 AIRA-289-B: one daemon per state directory (correctness-critical)

**Invariant:** at most one daemon serves a given state directory, whatever
`XDG_RUNTIME_DIR` each process has; a client that cannot reach that daemon says so loudly
and never starts a second one.

1. `internal/daemon/paths.go`, `PathsFromEnvironment`: `LockPath` becomes
   `filepath.Join(stateDir, "daemon.lock")` (`stateDir` = `<state home>/aira`, the
   directory that holds `state.db`), instead of `filepath.Join(daemonDir, "daemon.lock")`.
   `SocketPath` and `RuntimeDir` are unchanged. Comment the reason (one daemon per
   `state.db`; AIRA-289). Every lock user follows automatically: the daemon's
   single-instance lock (`server.go:385-403`), the client's startup lock
   (`tryStartupLock`, `dispatcher.go:848`) and `Status`/`Stop` (`paths.go:524-549`).
2. `internal/daemon/server.go`, before opening the lock (`server.go:385`):
   `os.MkdirAll(filepath.Dir(s.Paths.LockPath), 0o700)`. On a fresh machine the state
   directory may not exist yet when the runtime directory does.
3. `cmd/aira/dispatcher.go`, the connect loop (`dispatcher.go:744-846`):
   - beside the existing `MkdirAll(d.paths.RuntimeDir, 0o700)` (`:747`), also create
     `filepath.Dir(d.paths.LockPath)` (same error handling: `E_DAEMON_UNAVAILABLE`, request
     not sent);
   - `tryStartupLock` distinguishes its outcomes: `(file, nil)` won; `(nil, errLockHeld)`
     when `flock` returns `EWOULDBLOCK`/`EAGAIN`; `(nil, err)` for an open or other flock
     error. An open/other error is returned at once as `E_DAEMON_UNAVAILABLE: cannot take
     the daemon lock <LockPath>: <err>` (today it is swallowed and the client polls until
     the generic timeout). "Held" keeps today's behaviour: poll the socket.
   - at the deadline, before the existing generic `E_DAEMON_TIMEOUT: daemon did not accept
     before deadline`: if this client never won the lock (`!spawned`) and
     `os.Lstat(d.paths.SocketPath)` reports not-exist, return (request-not-sent wrapped,
     as the surrounding returns are) `E_DAEMON_UNAVAILABLE: no daemon is listening at
     <SocketPath>, but the daemon lock for this state (<LockPath>) is held by another
     process; a daemon for this state is probably running with a different
     XDG_RUNTIME_DIR (this process has XDG_RUNTIME_DIR=<value, or unset>); run with the
     daemon's XDG_RUNTIME_DIR (a gate command needs it in env_allow)`. The value is read
     through the dispatcher's existing `environment()` seam. A daemon that is merely slow
     to bind still gets the polling window it has today (`startWait`, 5 s).
4. Nothing else changes: the socket path, the handshake (which already refuses a foreign
   state id), `ServiceIdentityMatches`, install's `daemon SocketPath divergence` check
   (`install.go:2154-2160`) and AIRA-203's `MainPID` comparison.

Consequences, stated: `aira daemon status` from a mismatched process now reports the
lock holder as running and the socket as not ready (true), and `aira daemon stop` from
such a process stops the daemon that owns this state (it is that state's daemon).
`aira daemon` started by hand while a daemon owns the state gets `ErrAlreadyRunning` and
exits 0 without serving (`daemon_command.go:47-52`), instead of starting a second one.

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
- The command's environment is exactly the names in `env_allow` (sorted and unique, or the
  definition is refused). Listing `PATH` makes `aira ready` read the proof stale, because
  the daemon's `PATH` differs (the warning `gate run` prints); set `PATH` inside the
  command instead.
- To memory-confine a gate, wrap the run: `aira confine -- aira gate run <id>`. Admission
  then happens before the command starts, so the queue wait is outside `timeout_ms`, and
  the command needs no extra environment. If the command itself calls `aira` (for example
  a nested `aira confine`), `env_allow` must include `HOME` and `XDG_RUNTIME_DIR` so the
  inner call reaches this machine's daemon; without `XDG_RUNTIME_DIR` it now fails with
  `E_DAEMON_UNAVAILABLE` naming the cause. `DBUS_SESSION_BUS_ADDRESS` is not needed
  (measured: a scope is placed without it).
- `timeout_ms` covers the whole command, including time a nested `aira confine` spends
  queued for admission. A timed-out gate reads `U_GATE_COMMAND_TIMEOUT` (unevaluated);
  `aira run-log <RUN-id> --stream err` (the run id is in the gate result) shows
  `confine: waiting for memory admission` lines when the time went to queueing.

## 4. Invariants

1. `ready`'s verdict and blockers come only from the ticket files read in that call. The
   derived relation index can add a warning or the conservative
   `U_RELATION_OWNER_UNREADABLE`; it can never remove a blocker or turn `fail` or
   `unevaluated` into `pass`. Pinned in both directions (5.2).
2. A ticket write (`link`, `unlink`, `create`, `mv`, `set`) leaves the written file's
   relation index rows equal to the file's relations in the same materialisation; a
   sequence of `link` calls in one worktree produces no divergence finding.
3. An inverse relation kind is never stored. Its refusal names a command that stores the
   same relation (forward kind, endpoints swapped), and running that command succeeds.
4. At most one daemon serves a state directory, whatever `XDG_RUNTIME_DIR` each process
   has. A client that cannot reach the lock holder returns `E_DAEMON_UNAVAILABLE` within
   its start wait and starts no daemon.
5. A client on a machine with no state directory yet still starts the daemon (the lock's
   directory is created first), and an unopenable lock is reported at once, never as a
   generic timeout.
6. Every behavioural claim the new skill text makes is one this plan measured or read in
   code (section 1), and each paragraph is pinned by a test.
7. No new stable code, no protocol change (14), no wire field, no storage change.

## 5. Tests (TDD; each must fail against the wrong implementation)

Write each test first and record it RED on `2e5b72c`, or RED against the named mutation
where today's behaviour is already right. Each mutation must turn at least one test RED.

### 5.1 AIRA-290(b) (`internal/domain`, `internal/store`)

- `TestRelationKindForward`: `blocked-by -> blocks`, `child -> parent`,
  `duplicated-by -> duplicates`, `superseded-by -> supersedes`, `resolved-by -> resolves`,
  all `true`; the six forward kinds, `""` and `"bogus"` -> `false`. Mutation: return `k`
  itself -> RED.
- `TestLinkInverseKindNamesTheForwardCommand` (table over the five inverse kinds): the
  full error string, with the endpoints swapped and the forward kind named (RED today); the
  owner file's digest unchanged, no new outbox row, no new event. Unlink twin asserts
  `aira unlink`. Mutations: endpoints not swapped; inverse kind named instead of forward;
  `Unlink` left with the old text -> each RED.
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
  `aira reconcile` and `computed this verdict from the ticket files` (message part RED
  today); A `ready:true`. After `Check` (which reconciles) -> no divergence finding, B
  still blocked. Mutation M1: build `blockers` from `s.indexedRelations()` instead of
  `snapshot.relations` -> B ready -> RED.
- `TestSequentialLinksLeaveNoIndexDivergence` (the root-cause measurement as a pin): six
  tickets, ten sequential `Link` calls (a chain plus four from one owner) -> `Check`
  reports `relation-integrity: pass` with no divergence finding, and `Ready("")` has none.
  Mutation M3: skip `replaceRelationIndex` in `markTicketMaterialised` -> RED.
- `TestCheckDivergenceMessageNamesReconcile`: the `check` finding's message contains
  `aira reconcile` and does not contain the `ready`-only clause. RED today. Mutation:
  append the `ready` clause in `relationIndexDivergence` -> RED.
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
- `TestClientWithAMismatchedRuntimeDirFailsLoudly`: an in-process daemon on `(S, runtimeA)`;
  a `daemonDispatcher` on `(S, runtimeB)` with a short `startWait` and a `spawn` hook that
  fails the test if called -> the response is `E_DAEMON_UNAVAILABLE`, exit 4, its message
  names `runtimeB`'s socket, the state's `LockPath` and `XDG_RUNTIME_DIR` (with the
  injected `getenv` value); no socket exists at `runtimeB`. RED today (the hook is called:
  the client starts a second daemon). Mutations: no hint (generic `E_DAEMON_TIMEOUT`) ->
  RED; hint fires on an open error instead of a held lock -> the fresh-machine test RED.
- `TestClientOnAFreshMachineCreatesTheStateDirAndStarts`: state directory absent,
  `XDG_RUNTIME_DIR` elsewhere; the client creates the lock's directory, wins the lock and
  calls the `spawn` hook (which starts an in-process daemon) -> the request succeeds.
  Mutation: drop the client's `MkdirAll` for the lock directory -> RED (refused at once by
  the open-error rule; today it would hang until the generic timeout).
- `TestUnopenableLockIsReportedAtOnce`: lock path made unopenable (a directory at
  `LockPath`) -> `E_DAEMON_UNAVAILABLE` naming the path well before `startWait`. RED today
  (swallowed, then generic timeout).
- `TestStatusFromAMismatchedRuntimeSeesTheLockHolder`: `Status` on `(S, runtimeB)` while
  `(S, runtimeA)` runs -> `Running: true`, `Ready: false`. Pin.
- Subprocess test (the measured shape, using the helper-process pattern in
  `cmd/aira/dispatcher_inprocess_test.go:377`): two `aira` client processes with one
  `XDG_STATE_HOME` and different `XDG_RUNTIME_DIR` -> exactly one daemon process exists
  afterwards, and the second client exits 4 with the hint. RED today (two daemons).
- Existing tests that construct a daemon lock or read `Status` (`binary_stale_test.go`,
  `shim_daemon_detach_test.go`, `state_dir_agreement_test.go`,
  `dispatcher_inprocess_test.go`) are re-run; any that assumed the lock sits beside the
  socket are updated, not deleted.

### 5.4 Skill text (`internal/core/skill_test.go`, in the existing "skill teaches X" style)

One test per paragraph, each RED today (the phrases are absent):

- 290: `aira mv <id> retired`, the three-hop close path in order, "no delete",
  `E_TICKET_NOT_IN_WORKTREE`, "query-only" (or "read-only views") with `blocked-by`.
- 291: `E_PATH_INTENT_BUSY`, `aira reconcile`, and the statement that readiness is
  computed from the ticket files.
- 292: `scope-integrity`, `migrated`, `descendant-escaped`, `snap`, `containment=enforced`
  named as not sufficient.
- 289: "no commits", `merge-base`, `env_allow`, `XDG_RUNTIME_DIR`,
  `aira confine -- aira gate run`, `timeout_ms`, `aira run-log`, and that
  `DBUS_SESSION_BUS_ADDRESS` is not needed.

Mutation: delete any one paragraph -> its test RED. The SKILL.md and guide bytes change,
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
- A second daemon on one `state.db` becomes impossible. A gate, cron job or `ssh` session
  without the daemon's `XDG_RUNTIME_DIR` gets an actionable `E_DAEMON_UNAVAILABLE` in at
  most 5 s, instead of being admitted silently by a daemon whose ledger cannot see the
  box's jobs.
- Gate authors learn the snapshot, environment and timeout rules from the skill, and the
  wrap form removes both the environment requirement and the queued-timeout trap.
- Snap escapes become something an agent reads, without deciding AIRA-225.

## 7. Deferrals and "not in this batch"

| Item | Why not here |
|---|---|
| AIRA-225 (snap escape: loud signal, refusal, or documented gap) | Open owner fork; 292 documents current behaviour only. |
| Client finds the daemon's socket from the lock file (mismatched clients "just work") | The loud refusal meets the invariant with less machinery; revisit if the refusal proves common. |
| `XDG_RUNTIME_DIR` fallback to `/run/user/<uid>` | Same reason; partial (other differing values still need the refusal). |
| Distinct code for "gate timed out while queued in admission" | Rejected (section 2): needs output parsing or daemon-side ancestry. |
| `mv --through`, a delete/void verb, normalising inverse kinds on input | Rejected (section 2). |
| Bare `E_PATH_INTENT_BUSY` message (no path, no "retry") | Small, separate; the skill now explains it. Suggest folding into the next small batch. |
| **New finding: client-started daemon dies on SIGPIPE after its client exits (1.3)** | Measured symptom, mechanism assumed; affects machines without the systemd service. Suggest a held ticket: give the spawned daemon a stderr that outlives the client (a log file under the state directory, or `/dev/null` plus its existing logging), and a regression test that a client-spawned daemon survives its spawner and later logs. |
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
- **Upgrade window.** The v0.33 daemon holds its lock in the runtime directory; v0.34 takes
  it in the state directory. `aira install` restarts the daemon when the binary changed
  (AIRA-203), and systemd stops the old one before starting the new one. A v0.34 client
  that finds no socket during that window polls for up to 5 s, as today.
- **`aira daemon stop` reach.** From a mismatched process it now stops the state's real
  daemon (it reads the state's lock). Correct, but new; noted in release notes.
- **Message text.** `E_RELATION_INVALID` and the divergence message change text; codes and
  exits do not. Tests pinning the old strings are updated.
- **Skill length.** About 30 lines of new skill text; each paragraph is pinned and states
  only measured behaviour.

## 9. Stable codes

No new codes; the catalogue (`internal/codes/codes.go`) is unchanged.

| Code | Exit | Use in this plan |
|---|---|---|
| `E_RELATION_INVALID` | 1 | Inverse-kind refusal, now naming the forward command; unknown-kind refusal with the writable list. |
| `E_RELATION_INDEX_DIVERGENCE` | 1 in `check` (`fail`); `warning` in `ready` | Message gains how to clear it; kinds unchanged. |
| `E_DAEMON_UNAVAILABLE` | 4 | Client cannot reach the state's lock holder (mismatched `XDG_RUNTIME_DIR`), or cannot open the lock. |
| `E_DAEMON_TIMEOUT` | 3 | Unchanged generic case (lock won, daemon never answered). |
| `E_TRANSITION_INVALID`, `E_TICKET_NOT_IN_WORKTREE`, `E_PATH_INTENT_BUSY`, `U_GATE_COMMAND_TIMEOUT`, `E_GATE_INVALID` | unchanged | Named in skill text. |

## 10. Release notes (for the build)

Proto unchanged (14). Daemon restart required for 289-B; `aira install` performs it
(AIRA-203). Skill reinstall required (`aira skill install ~/.claude/skills/aira --force`).
Release note for every peer: a process that runs `aira` with a different
`XDG_RUNTIME_DIR` from the daemon's (or none) now fails with `E_DAEMON_UNAVAILABLE` naming
the cause, instead of starting a second daemon; export `XDG_RUNTIME_DIR` in cron jobs, and
list it in a gate's `env_allow` when the gate command calls `aira`. Reply to ci-build per
ticket: 290(a) use `aira mv <id> retired`, no delete by design; 290(b) refusal now names
the command; 290(c) close path stays three hops by decision (AIRA-215); 290(d) already
shipped in v0.31; 291 the UNEVALUATED was the gate subject, fixed in v0.33, and the
divergence after `git pull` is cleared by `aira reconcile` and never changes readiness;
292 read `scope-integrity`, prefer non-snap tools; 289 skill section, wrap form, and the
new loud refusal. Notify deploy and speed per the release rule.
