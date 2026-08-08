# AIRA Phase 1 — coordination MVP design

- **Status:** proposed for plan review; no Phase-1 implementation code exists.
- **Date:** 2026-08-08.
- **Parent:** [`2026-08-07-aira-design.md`](2026-08-07-aira-design.md), especially §§5–9, 11, 19–21.
- **Scope:** git-file tickets, machine-wide coordination/index state, common-dir
  receipts and event journal, reconciliation, IDs, status, relations, leases,
  advisory area hints, the blocked-by ready queue, CLI/query, `aira check`,
  `aira backlog`, and first `aira stats`.
- **Explicitly deferred:** subprocess runner, telemetry, test-report ingestion,
  gates, review emission, MCP/Skill surfaces, TUI, daemon, and remote transport.

This document closes every Phase-1-spec deliverable in §21. The decisions are
deliberately concrete so implementation can test them rather than rediscovering
them at the persistence boundary.

## 1. Store-engine decision and evidence

Use `modernc.org/sqlite` with WAL mode and no cgo. Pin the implementation to
`modernc.org/sqlite v1.54.0` and Go 1.25 or newer; v1.54.0 declares that toolchain
floor. The release build remains `CGO_ENABLED=0` and produces one static binary.
The Go toolchain version is pinned in the Phase-1 build metadata when code is
started; the spike used the available Go toolchain's automatic Go 1.25.12
selection.

### Spike

The disposable harness was outside the worktree at
`/tmp/aira-sqlite-spike`. Each worker was a separate short-lived process. It
opened the same database in WAL mode, set `busy_timeout=5000`, executed an
explicit `BEGIN IMMEDIATE`, selected `MAX(id)+1`, held the write transaction for
25 ms, inserted the ID, and committed. The verifier required one row per worker,
all IDs distinct, and `MAX(id)` equal to the worker count.

Successful build command:

```text
whale-run /home/user/.local/bin/go build -buildvcs=false -o /tmp/aira-sqlite-spike/probe .
```

Single contention run:

```text
whale-run /tmp/aira-sqlite-spike/run.sh /tmp/aira-sqlite-spike/coordination.db 32
```

Result:

```text
SPIKE_PASS workers=32 rows=32
```

Repeat command:

```text
whale-run /bin/sh -c 'i=1; while [ "$i" -le 8 ]; do /tmp/aira-sqlite-spike/run.sh "/tmp/aira-sqlite-spike/repeat-$i.db" 32; i=$((i + 1)); done'
```

Result: eight `SPIKE_PASS workers=32 rows=32` lines, with no `database is
locked`, `BEGIN IMMEDIATE`, insert, or commit failures.

This is sufficient evidence for the tested cross-process locking contract, not
evidence about power-loss durability or every filesystem. Those cases are
covered by the crash-recovery tests in Phase 1. If a supported driver/version
ever fails this spike, the implementation must stop using SQLite for the
contended operation and use the documented common-dir `flock` critical section
around allocation/lease commit. bbolt and cgo SQLite remain fallback studies,
not parallel implementations.

## 2. Storage authority and write protocol

There are two authorities with different scopes:

1. **Ticket content authority:** the committed git file is authoritative for the
   ticket's frontmatter and body. SQLite stores an index and coordination state,
   never a second steady-state copy of ticket content.
2. **Mutation sequencing and coordination authority:** SQLite is authoritative
   for committed coordination mutations, the machine-wide ID counter, lease CAS,
   pending materialisation intents, and the per-machine event `seq`. The
   common-dir journal and receipts are durable append-only audit projections of
   those committed SQLite decisions.

The pending intent is an outbox, not a second content authority. It contains the
exact bytes/digest AIRA intended to write so a crash can be repaired. Once a git
file exists with a different digest, the file wins and the reconciler records a
conflict instead of overwriting user content.

### Mutation protocol

Every mutation that changes a git file follows this sequence:

1. Discover the project and worktree; load and validate config; reconcile the
   relevant project scope before acting.
2. Parse the existing canonical file, validate the requested change, validate
   relation targets, and compute the new exact file bytes and SHA-256 digest.
3. Open the machine DB and execute `BEGIN IMMEDIATE`. In that transaction,
   allocate `seq`, apply the coordination change, reserve/update any ID or lease,
   and insert an outbox row containing project, path, precondition digest,
   intended digest, intended bytes, verb, actor, and event payload digest.
   Commit. No git file is changed while this transaction is open.
4. Write the intended file to a same-directory temporary file, `fsync` it,
   atomically rename it into place, and `fsync` the directory. A file delete is
   represented by an explicit tombstone state in the outbox; Phase 1 does not
   silently delete a ticket file.
5. Open a new `BEGIN IMMEDIATE`, verify the file digest and precondition, mark
   the outbox materialised, update the rebuildable index, and commit. If the
   precondition no longer holds, leave the intent pending and report
   `E_WRITE_CONFLICT`.
6. Append the canonical event record identified by `(project, seq, digest)` to
   the project repository's common-dir journal under its append lock, flush and
   `fsync` it. A second short DB transaction marks the event journaled. Repeating
   this step is idempotent by the key and digest.

Pure DB mutations, including leases and heartbeats, use the same DB event/outbox
sequence and journal step but have no git-file materialisation step.

The journal record is canonical JSONL with a framing prefix containing `seq` and
the SHA-256 of the canonical JSON payload. A partial final line is not treated as
an event: the reconciler preserves the corrupt tail as evidence, emits
`E_JOURNAL_CORRUPT`, and replays the complete event from the DB outbox. A
checksum mismatch before the final record stops replay at that point and is
reported; the reconciler never skips an unknown middle record.

For a project, the physical audit paths are
`<git-common-dir>/aira/journal.jsonl`,
`<git-common-dir>/aira/receipts.jsonl`, and the sibling lock files used for
append and rebuild. These paths are outside every worktree and therefore outside
the commit graph.

### Crash matrix and reconciler action

| Crash point | Reconciler action |
|---|---|
| Before the first DB commit | A file already present is treated as git-authoritative and imported with a new sequence; no uncommitted intent exists. |
| After intent commit, before file rename | Replay the intended bytes if the path is absent and the precondition still matches; otherwise record `E_WRITE_CONFLICT`. An allocated ID becomes a visible pending receipt until materialised or retired. |
| During temp write | Ignore an unrenamed temp file after recording its digest if recoverable; replay from the outbox. |
| After rename, before materialised commit | Verify the digest, rebuild/index the file, and mark the outbox materialised. A different digest is a conflict; never overwrite it. |
| After materialised commit, before journal append | Append the missing event from the DB event row, then mark journaled. |
| During journal append | Validate the final frame; re-emit an absent/partial frame, or stop on a corrupt middle frame with `E_JOURNAL_CORRUPT`. |
| After journal append, before journaled mark | Keyed replay observes the existing frame and marks it journaled. |
| DB loss | Recreate the schema, scan common-dir journals/receipts, all registered worktree files, and all refs, then rebuild index, counters, and reconciliation findings. |

The reconciler is idempotent. It never invents a relation to a missing ticket,
never silently drops a receipt, and never turns an unrun repair into `pass`.

## 3. Machine database and project/worktree discovery

There is one SQLite DB per machine, shared by all registered AIRA projects and
all worktrees. Its location is:

1. `$XDG_STATE_HOME/aira/state.db` when `XDG_STATE_HOME` is set;
2. otherwise `$HOME/.local/state/aira/state.db` on Linux;
3. on another platform, the OS equivalent of the user's state directory under an
   `aira` child.

The path is derived, never configured in a committed project file. The DB uses
WAL, `busy_timeout=5000`, foreign keys, and a schema version. A machine DB row
keys every project by a canonical project identity and every worktree by its
stable git worktree identity.

### Project discovery

From the caller's current directory, AIRA:

1. walks upward to the nearest `.aira/config` without crossing a filesystem
   root;
2. asks git for the worktree top level, per-worktree git directory, and
   `--git-common-dir`;
3. canonicalises those paths without following a ticket-file symlink;
4. derives `project_id = SHA-256(canonical git common-dir)`, and
   `worktree_id = SHA-256(canonical per-worktree git-dir)`;
5. verifies that the config's declared project slug and prefixes agree with the
   DB registration.

The common-dir path is local identity, not a remote identity. A project may be
registered explicitly with `aira init`; commands refuse to guess when two
configs or two git roots could be the current project.

`git worktree list --porcelain` is the authoritative enumeration of sibling
worktrees for this local clone. Missing paths remain registered as historical
worktrees until reconciliation records them as inactive; their receipts remain
searchable.

### Prefix ownership

`.aira/config` declares one or more uppercase ticket prefixes. Registration takes
the machine DB's `prefix_ownership` write lock and enforces:

```text
prefix → exactly one project_id on this machine
```

Re-registering the same prefix for the same project is idempotent. Registering a
prefix for another project fails with `E_PREFIX_OWNERSHIP_CONFLICT`; AIRA never
resolves this by choosing the first config found. Prefix ownership is local to a
machine; cross-clone/remote allocation is outside Phase 1 and remains a later
protocol decision.

## 4. `.aira/config` and ticket-file format

### Config

`.aira/config` is committed UTF-8 strict JSON, with no comments, no unknown keys,
and `schema: 1`. AIRA writes it canonically with a trailing newline and replaces
it through the same-directory temp-file/fsync/rename protocol. The Phase-1
schema is:

```json
{
  "schema": 1,
  "project": {
    "slug": "aira",
    "prefixes": ["AIRA"]
  },
  "lease": {
    "ttl_seconds": 900,
    "heartbeat_seconds": 30
  }
}
```

`project.slug` is lowercase ASCII `[a-z][a-z0-9-]{1,62}`. Prefixes are unique
within the file, uppercase ASCII letters of length two or more, and are checked
against machine-wide ownership. Lease values are optional and default to the
values shown; explicit values must be positive, `heartbeat_seconds <
ttl_seconds`, TTL must be at least 60 seconds, and TTL must not exceed 24 hours.
The config does not contain DB paths, absolute machine paths, lease tokens, or
machine identity.

### Ticket file

Each ticket is one committed file at `.aira/tickets/<ID>.md`. The file is UTF-8
with LF newlines, begins with `---`, contains a single canonical JSON object,
then a second `---`, one newline, and the Markdown body. The frontmatter object
has exactly these Phase-1 fields:

```json
{
  "schema": 1,
  "id": "AIRA-42",
  "project": "aira",
  "title": "Implement the ready queue",
  "status": "planned",
  "kind": "task",
  "severity": "P2",
  "assignee": null,
  "milestone": null,
  "labels": [],
  "hold": false,
  "relations": []
}
```

`id`, `project`, `title`, `status`, `kind`, `severity`, `assignee`,
`milestone`, `labels`, `hold`, and `relations` are required. `assignee`,
`milestone`, and `labels` may be empty/null as shown. Status and severity are
closed enums from the parent spec. Phase 1 permits `kind` values `task`,
`bug`, `design`, and `chore`; unknown kinds are refused. Labels are sorted,
unique, lowercase strings. The body may be empty but must end in one newline.

Timestamps are not copied into the file: event `seq` is the ordering authority,
and event wall time is advisory. The DB index stores derived timestamps and can
be rebuilt.

### Canonical relation storage

Relations are stored exactly once, in the ticket file whose ID is the lower
canonical bytewise ID, comparing the uppercase prefix first and the numeric
component numerically. The stored relation contains both directed endpoints so
that orientation is not inferred from storage location:

```json
{"kind":"blocks","from":"AIRA-42","to":"AIRA-43"}
```

The relation list is sorted by `(kind, from, to)`. `from` and `to` must be the
two distinct existing ticket IDs in the same project. The canonical kinds are
`blocks`, `parent`, `relates`, `duplicates`, `supersedes`, and `resolves`.
Inverse forms (`blocked-by`, `child`, `duplicated-by`, `superseded-by`, and
`resolved-by`) are query/render projections, never stored rows. `relates` is
its own inverse. A duplicate relation is rejected with `E_RELATION_EXISTS`;
removing the relation edits only its canonical-side file. A missing target or a
relation whose stored side is not the lower ID is an integrity failure, not an
automatic rewrite.

## 5. IDs and counter rebuild

The machine DB is the cross-worktree allocation authority. The allocation
transaction is:

```text
BEGIN IMMEDIATE
  verify prefix ownership
  next = max(counter.next, observed receipt high-water mark) + 1
  increment counter
  insert allocation receipt and file-materialisation intent
  allocate event seq
COMMIT
```

The receipt and counter update commit together. The ticket file is then written
by the protocol in §2. A receipt without a file is never silently forgotten: the
reconciler reports a pending allocation and offers the explicit choices
`materialise` or `retire`, after which the ID is permanently consumed.

Every connection sets `PRAGMA busy_timeout=5000`; contended writers use an
explicit `BEGIN IMMEDIATE`. A timeout returns `E_DB_BUSY` with `retryable: true`
before the operation is considered committed. No client guesses whether a
timed-out allocation succeeded; it reconciles by receipt/sequence before retry.

### Multi-worktree rebuild

When the DB is absent, corrupt, or explicitly rebuilt, AIRA acquires the
common-dir `rebuild.lock`, opens a new DB, starts `BEGIN IMMEDIATE`, and while
holding that writer transaction:

1. enumerates every worktree from `git worktree list --porcelain` and scans each
   existing `.aira/tickets/` working tree, including uncommitted files;
2. scans every common-dir allocation receipt and event journal;
3. scans every reachable ref with `git for-each-ref` and `git ls-tree` for
   committed `.aira/tickets/` files;
4. parses IDs and frontmatter with the one shared parser, refusing duplicate
   definitions and recording malformed files;
5. computes each prefix's maximum from all working-tree files, committed refs,
   receipts, and journal allocations;
6. writes counters strictly above those maxima, imports the index, and records
   reconciliation findings before committing the rebuild.

The writer transaction blocks normal allocators during the scan, and
`rebuild.lock` serialises bootstrap/rebuild attempts when the DB itself is
missing. Scanning working trees is mandatory: refs alone cannot see a sibling's
uncommitted `AIRA-50`, and a single-checkout maximum can re-mint it.

## 6. Selectors and anchors

The Phase-1 ID grammar is:

```text
PREFIX := [A-Z]{2,}
NUMBER := [1-9][0-9]*
ID := PREFIX "-" NUMBER
```

The canonical form uppercases the prefix and uses no numeric leading zeroes.
The filename anchor is exactly `.aira/tickets/` + `ID` + `.md`, after resolving
the path relative to the discovered project root. Symlinked ticket paths are
refused. A relation endpoint is an exact `ID`; titles, prefixes without a
number, filenames outside the project, and free text are never silently
coerced into IDs.

Singular commands (`show`, `set`, `mv`, `claim`, `release`, `heartbeat`,
`ready`, and either endpoint of `link`) accept only an exact ID or an exact file
anchor. Plural `ls`/`count` queries use this Phase-1 grammar:

```text
query := term { SP term }
term  := field ":" value | "text:" quoted
field := id | status | kind | severity | assignee | milestone | label | project
value := bare | quoted
```

Bare values contain `[A-Za-z0-9._/-]+`; quoted values use JSON string escaping.
Terms are ANDed. No implicit fuzzy title matching exists in Phase 1.

A selector is **ambiguous** exactly when it resolves to more than one distinct
ticket identity in a command requiring one result, or when one exact ID/file
anchor has multiple definitions (for example, duplicate files/frontmatter). A
zero-result singular selector is `E_NOT_FOUND`; a multi-result singular selector
is `E_SELECTOR_AMBIGUOUS`; a duplicate definition is `E_DUPLICATE_ID`. Plural
queries may return many rows and are not ambiguous. A selector that cannot be
parsed is `E_SELECTOR_INVALID`. AIRA refuses all three error cases rather than
choosing a row.

## 7. Leases and advisory area hints

### Holder identity

`aira claim` creates a 32-byte token from `crypto/rand`, returns it once as a
base64url string, and stores only `SHA-256(token)` in the DB. The client may
save the clear token in a mode-0600 local lease file, but it is not committed or
journaled. Release, heartbeat, and steal must present the token; a ticket ID,
worktree ID, PID, or actor name alone cannot release or refresh another holder's
lease. The holder record also contains project, ticket, worktree ID, actor, and a
generation number for audit/debugging. This is unforgeable within the Phase-1
same-user protocol; a user who can directly edit the DB is outside AIRA's local
store threat model.

### Monotonic clock

Lease liveness uses Linux `CLOCK_MONOTONIC` read through the pure-Go
`golang.org/x/sys/unix` syscall wrapper, plus `/proc/sys/kernel/random/boot_id`.
The DB stores `boot_id`, `last_heartbeat_mono_ns`, and the configured TTL. A
lease is live only when the current boot ID matches and:

```text
current_mono_ns < last_heartbeat_mono_ns + ttl_ns
```

After a reboot, old leases are expired immediately because the boot ID differs.
On a platform without a system-wide cross-process monotonic clock, Phase 1
returns `E_CLOCK_UNAVAILABLE` and refuses lease operations; it never falls back
to wall-clock time. Wall-clock `at` is retained only as advisory journal data.

### Atomic operations and defaults

The default TTL is **15 minutes** and the default heartbeat interval is **30
seconds**. A heartbeat should be emitted at least every interval; the lease is
not extended by a read or by an area hint. Configured TTL/heartbeat values are
validated by §4.

Claim, heartbeat, release, and expired-steal each execute one `BEGIN IMMEDIATE`
transaction. The CAS predicates include ticket ID, lease generation, holder
hash where appropriate, boot ID, and the monotonic expiry condition. A live
lease gives `E_LEASE_HELD`; a token mismatch gives `E_LEASE_TOKEN`; a stale
heartbeat gives `E_LEASE_EXPIRED`; `--steal` wins only when the predicate proves
the prior lease expired. The winning operation increments generation and emits
one event. Heartbeats are DB-only and are not journaled individually.

Area hints are glob strings associated with a live claim. They are normalised,
sorted, and stored in the DB. Overlap with another live claim emits
`W_AREA_OVERLAP` and is never an AIRA refusal. The project harness may promote
that warning to its own policy; AIRA does not create hard file leases.

## 8. Reconciliation, checks, codes, and exit status

### Trigger and scope

With no daemon in Phase 1, reconciliation runs:

- before every mutating command and before a singular read that relies on
  integrity;
- after a failed materialisation or journal append;
- at explicit `aira reconcile`;
- as the first stage of `aira check`.

The default scope is the discovered project: all registered worktrees for its
common git directory, their current `.aira/tickets/` trees, reachable refs,
that common-dir journal/receipts area, and that project's DB rows. `--all`
scans every project registered in the machine DB. AIRA never scans arbitrary
filesystem paths outside registered project/worktree roots.

Reconciliation checks pending outbox intents, duplicate/malformed IDs, file and
index digests, relation targets and canonical-side placement, journal continuity,
receipt/file resolution, prefix ownership, orphan worktrees, and expired lease
metadata. It may rebuild derived index rows and append reconciliation findings;
it does not delete user files or silently repair a conflicting file.

### Stable code catalog

Every response has a stable `code`, a human message, structured details, and a
`retryable` flag. The Phase-1 catalog is:

| Code | Meaning | Class |
|---|---|---|
| `E_CONFIG_MISSING` | No `.aira/config` was found in the discovered project. | error |
| `E_CONFIG_INVALID` | Config or ticket frontmatter violates schema/encoding rules. | error |
| `E_NOT_PROJECT` | Git/project discovery is absent or contradictory. | error |
| `E_PREFIX_OWNERSHIP_CONFLICT` | A prefix is registered to another project on this machine. | error |
| `E_DB_BUSY` | The SQLite writer lock exceeded the 5-second busy timeout. | error, retryable |
| `E_DB_CORRUPT` | The DB cannot be opened or schema integrity fails. | error |
| `E_ID_INVALID` | ID or prefix grammar is invalid. | error |
| `E_DUPLICATE_ID` | More than one ticket definition has the same ID. | integrity fail |
| `E_ID_UNRESOLVED` | An allocation receipt has neither a ticket nor retirement. | integrity fail |
| `E_NOT_FOUND` | A singular selector found no ticket. | error |
| `E_SELECTOR_INVALID` | Selector syntax is invalid. | error |
| `E_SELECTOR_AMBIGUOUS` | A singular selector resolved to multiple tickets. | integrity fail |
| `E_RELATION_TARGET_MISSING` | A relation points at no existing ticket. | integrity fail |
| `E_RELATION_INVALID` | Relation kind, direction, or canonical side is invalid. | integrity fail |
| `E_RELATION_EXISTS` | The canonical relation already exists. | error |
| `E_CROSS_PROJECT_RELATION` | Phase-1 relation endpoints are not in one project. | error |
| `E_WRITE_CONFLICT` | The file changed after the intent precondition. | integrity fail |
| `E_JOURNAL_CORRUPT` | A journal frame is missing, partial, or checksum-invalid. | integrity fail |
| `E_RECONCILE_REQUIRED` | An operation cannot proceed until a repair is resolved. | integrity fail |
| `E_CLOCK_UNAVAILABLE` | No cross-process monotonic clock is available. | unevaluated/error |
| `E_LEASE_HELD` | Another live holder owns the ticket. | error |
| `E_LEASE_TOKEN` | The supplied holder token does not match. | integrity fail |
| `E_LEASE_EXPIRED` | The supplied heartbeat/release token is stale. | error |
| `W_AREA_OVERLAP` | Advisory area hints overlap a live claim. | warning |
| `W_ORPHAN_WORKTREE` | A registered worktree path is missing or inactive. | warning |
| `W_STALE_INDEX` | A derived index row was rebuilt from git content. | warning |
| `U_CHECK_UNEVALUATED` | A requested check could not establish a verdict. | unevaluated |
| `E_INTERNAL` | An unexpected internal or filesystem error occurred. | error |

The catalog is a dispatch-table input. CLI help, MCP schemas, and the future
agent guide are generated from it; they are not separately transcribed.

### `aira check` exit codes

`aira check` emits a structured result with one verdict per check and exits:

| Exit | Meaning |
|---:|---|
| 0 | All selected checks are `pass`; warnings may be present. |
| 1 | At least one selected check is `fail` or a fail-closed integrity error exists. |
| 2 | Invocation, selector, or config syntax is invalid; no check verdict is claimed. |
| 3 | At least one selected check is `unevaluated`, with no `fail`. |
| 4 | Store/reconciliation failed before the requested checks could be evaluated. |

If both `fail` and `unevaluated` occur, exit 1 wins. An unrun check cannot
produce exit 0. `W_AREA_OVERLAP`, `W_ORPHAN_WORKTREE`, and `W_STALE_INDEX` are
visible warnings and do not fail the default check; a future project policy may
promote them.

## 9. Phase-1 database shape

The schema is rebuildable and keyed by `schema_version`. The essential tables
are:

- `projects(project_id, slug, common_dir, config_digest, ...)`;
- `worktrees(worktree_id, project_id, git_dir, root, active, ...)`;
- `prefix_ownership(prefix PRIMARY KEY, project_id, registered_seq)`;
- `id_counters(project_id, prefix, next_number)`;
- `allocations(project_id, prefix, number, worktree_id, state, seq, ...)`;
- `tickets(project_id, id, path, digest, status, hold, ...)` as a derived index;
- `relations(project_id, kind, from_id, to_id, canonical_file, ...)` as a
  derived index;
- `leases(project_id, ticket_id PRIMARY KEY, token_hash, holder, worktree_id,
  generation, boot_id, last_heartbeat_mono_ns, ttl_ns, ...)`;
- `area_hints(project_id, ticket_id, glob, ...)`;
- `outbox(seq PRIMARY KEY, project_id, verb, path, precondition_digest, intended_digest,
  intended_bytes, materialised, journaled, ...)`;
- `events(seq PRIMARY KEY, project_id, at_wall, actor, verb, target,
  payload_digest, journaled, ...)`;
- `findings(project_id, id, subtype, code, subject, ...)` for reconciliation
  findings in Phase 1.

`tickets` and `relations` are disposable projections. `allocations`, `events`,
and outbox rows are recovery aids; the common-dir receipts/journal are the
durable audit projection used to rebuild them after DB loss.

## 10. Ready-queue semantics

The ready queue is derived, never hand-maintained. A ticket is ready when its
status is `planned` or `in-progress`, `hold` is false, and every incoming
`blocked-by` prerequisite is in `done`, `retired`, or `superseded`. A missing,
duplicate, or malformed prerequisite is an integrity failure and makes the
ticket not ready. `aira ready <ID>` returns the ticket, its blockers, and the
three-verdict check summary; `aira ready --list` returns the current ready set
with the same fields. A ticket that is valid but blocked is a normal `pass`
response for the query with `ready=false`, not a failed database operation.

The ready query folds Phase-1 integrity checks and returns `unevaluated` when
reconciliation could not establish the graph. It never treats an absent blocker
as satisfied.

## 11. §21 closure matrix

| §21 deliverable | Phase-1 decision |
|---|---|
| DB ↔ git-file ↔ journal ordering and crash recovery | SQLite commits intent/coordination/seq first; an atomic git-file write follows; index/outbox is marked materialised; common-dir journal is appended and fsynced; the reconciler replays each missing stage idempotently and records conflicts. Git is content authority, SQLite is transactional coordination/sequence authority, journal is durable audit projection. |
| ID atomicity | `modernc.org/sqlite v1.54.0`, WAL, `busy_timeout=5000`, explicit `BEGIN IMMEDIATE`; counter and receipt commit in one transaction. |
| Multi-worktree counter rebuild | Under `rebuild.lock` plus a DB writer transaction, scan every registered worktree's working tree, all common-dir receipts/journals, and all refs. Working-tree scanning sees uncommitted sibling IDs. |
| Lease atomicity | Claim, heartbeat, release, and expired steal are single `BEGIN IMMEDIATE` CAS transactions with generation and expiry predicates. |
| Unforgeable holder identity | 32-byte `crypto/rand` token; only its SHA-256 is stored; every holder mutation requires the clear token. |
| Monotonic clock | Linux `CLOCK_MONOTONIC` via pure-Go syscall plus boot ID; no wall-clock fallback; unsupported platforms return `E_CLOCK_UNAVAILABLE`. |
| Selector/anchor grammar | Exact `PREFIX-N` IDs, exact `.aira/tickets/ID.md` anchors, and explicit ANDed field queries; no fuzzy singular resolution. |
| Selector ambiguity | A singular selector is ambiguous only when it resolves to multiple ticket identities; duplicate definitions are a separate integrity failure. Zero, invalid, and multi-result cases have distinct stable codes. |
| `.aira` frontmatter/config | Strict canonical JSON with schema 1, committed config, one ticket per Markdown file, fixed fields, no unknown keys, atomic file replacement. |
| Canonical relation side | One relation row in the lower-ID ticket file; `from`/`to` preserve direction; inverse is derived. |
| Git commit semantics | AIRA writes and fsyncs the working tree; the agent commits. Receipts, journal, and operational DB state live outside the commit graph in the git common dir/machine state. |
| Project/worktree discovery | Nearest config + git top-level/common-dir/per-worktree git-dir; project and worktree IDs are hashes of canonical git identities. |
| Prefix ownership uniqueness | Machine DB enforces `prefix → exactly one project_id`; conflicts refuse rather than guess. |
| DB placement | One machine-wide DB under the XDG/OS state directory, with project/worktree keys. |
| Lease TTL/heartbeat | Defaults: TTL 15 minutes, heartbeat 30 seconds; bounded config overrides. |
| Stable-code catalog | Phase-1 `E_`, `W_`, and `U_` codes are listed in §8 and generated surfaces consume the dispatch table. |
| `aira check` exit codes | 0 pass, 1 fail, 2 invalid invocation/config, 3 unevaluated, 4 store/reconcile error; fail wins over unevaluated. |
| Reconcile trigger/scope | Before mutating/integrity-sensitive commands, after failed writes, explicit command, and first in `aira check`; default current project/all its worktrees/refs/common dir, `--all` for machine-wide. |

## 12. Verification plan before Phase-1 code

The approved implementation plan must begin with tests for:

1. 32+ short-lived processes contending on allocation and lease CAS, including
   timeout and retry classification;
2. every crash-matrix boundary, including partial journal tails and conflicting
   post-crash user edits;
3. counter rebuild with an uncommitted sibling-worktree ticket and a committed
   ref containing a different maximum;
4. duplicate IDs, missing relations, wrong canonical relation side, malformed
   frontmatter, and all selector cardinalities;
5. forged/mismatched lease tokens, heartbeat-vs-steal races, and reboot/boot-ID
   expiry;
6. `aira check` verdict/exit combinations, proving an unevaluated check never
   exits green.

No Phase-1 implementation code is part of this document or the current plan
handoff.
