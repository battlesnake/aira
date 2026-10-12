# AIRA ledger and receipt integrity: foreign-state writers, requirement re-point, registry growth

Status: PLAN v1 (2026-10-12). Tickets: AIRA-301 (P2 bug), AIRA-297 (P3 bug), AIRA-296 (P3
chore, rescoped by triage). Base: master `4098ad9` (v0.34 plus ticket-housekeeping merges).

**Correctness-critical: all of AIRA-301 (ID allocation, the shared audit log) and AIRA-297
(Rebuild's allocation projection).** Both go through the two-loop adversarial build review.
AIRA-296 is dropped from this batch with evidence (section 7).

Note on the brief: `internal/store/ledger.go` does not exist on master. The receipt writer is
`appendReceiptIfMissing` (`internal/store/store.go:2506`); `internal/runner/ledger.go` is the
run ledger, which this batch does not touch.

## 1. Problem, in plain terms, and what still reproduces

Every repository's shared git directory holds two append-only files that AIRA uses for crash
recovery and for rebuilding its database: `<git-common-dir>/aira/receipts.jsonl` (one line
per ID handed out: the ID and its sequence number) and `<git-common-dir>/aira/journal.jsonl`
(one line per event). All worktrees of the repository share them. They are meant to be
written by one AIRA database (the machine-wide "state", `~/.local/state/aira/state.db`),
whose counters hand out IDs and sequence numbers.

**The incident (2026-10-11), restated because AIRA-301's ticket body is empty.** Reported
(v0.34 release note, not re-measured): a review agent's sandbox `cd` failed, so it ran 71
`aira create` inside the real aira checkout against a private daemon with its own state, and
68 bogus receipts landed in the real `receipts.jsonl`. Every `Rebuild` then failed
`E_JOURNAL_CORRUPT`, and `aira check` (which starts with a Rebuild, `check.go:245`) reported
it in every worktree, until the owner truncated the file by hand (`truncate -s 93327`, 300
lines kept). Agents cannot do that repair: they are blocked from editing `.git`. The
mechanism, reproduced below: the private database had never seen this repository, so its
counters started at 1 and handed out AIRA-1, AIRA-2, ... with sequence numbers 1, 2, ....
A create appends its receipt *before* it writes the ticket file, so a create that then fails
`E_WRITE_CONFLICT` (the file already exists) still leaves its receipt. A receipt that is
byte-identical to a real one is deduplicated, which is consistent with 71 creates leaving 68
lines (inferred, not checked against the truncated file).

**Root cause: two rules the design states but nothing enforces.**

1. *A database may hand out numbers only after it has read the log.* The phase-1 spec says
   reconciliation "never silently proceeds on a stale projection" and returns retryable
   `E_RECONCILE_REQUIRED` (`2026-08-08-aira-phase1-design.md:567`, `:624`), and that a new
   machine database is rebuilt from the common-dir journals and receipts (`:205`). Nothing
   implements it: `registerDB` seeds a never-seen project's sequence counter at 1
   (`store.go:2064-2065`) whatever the log already holds, and `E_RECONCILE_REQUIRED` is
   catalogued but produced nowhere (`internal/codes/produced_test.go:86`).
2. *One state writes a repository's audit log.* AIRA's design is one machine-wide state
   (one daemon per state directory, AIRA-289), but nothing stops a second state from
   appending to a repository's shared files.

| Ticket | Still reproduces? | Evidence (2026-10-12, `4098ad9`) | Verdict |
|---|---|---|---|
| AIRA-301 foreign state appends receipts | Yes, measured | Store-level reproduction (probe in `~/tmp`, not committed): owner state creates 3 tickets, updates one twice, creates 2 more. A second state (different `state.db`, same common dir, same checkout) runs 5 creates: all 5 return `E_WRITE_CONFLICT`, yet `receipts.jsonl` grows from 5 to 7 lines (AIRA-4 seq 4, AIRA-5 seq 5; the first three were byte-identical twins of the owner's receipts and were deduplicated). Owner `Rebuild`: `E_JOURNAL_CORRUPT: receipt AIRA-4 has seq 4 but allocation has seq 6`; owner `Check`: verdict fail, `rebuild-integrity` fail. Variant from an empty checkout of the same repository: the second state writes 4 ticket files and 4 receipts, its journal appends are refused (`journal key conflict`, the existing guard at `store.go:3882-3885`), and the owner's Rebuild fails the same way. | Build |
| AIRA-297 requirement allocation never re-pointed | Yes, measured | Requirement AR-1 allocated in `wt-a` (`AllocateID("AR")`), a valid `AR-1.md` only in `wt-c`; `c.Check()` reports `E_ID_UNRESOLVED AR-1 allocation has no materialised requirement file`, and the allocation path still names `wt-a`. Control in the same test: ticket AIRA-1 in the identical shape is re-pointed to `wt-c` and resolves (the ticket loop's guarded UPDATE, `store.go:3127-3131`; the requirement loop, `store.go:3141-3201`, has none). No crash and no worktree removal are needed to show it. | Build |
| AIRA-296 registry growth | Growth is real; the cheap remainder has a cost | On a copy of `~/.local/state/aira/registry.jsonl` taken 2026-10-12: 2,366 records, 738,108 bytes, 232 distinct identities, 228 roots of which 33 still exist. 2,134 records (90.2 %) are identical to the latest earlier record for the same project and root. Last 7 days: 42-151 records per day, 38-150 of them such duplicates. A prototype that skips an identical breadcrumb removes that growth but breaks three tests that use the registry line count as their registration counter (section 7). | Drop from this batch, design recorded |

## 2. Greenfield minimum, and rejected alternatives

If AIRA-301 had to be solved alone, from nothing: **a database whose counters were never set
from the log must not hand out a number.** One "not yet set" value in the counter row and
one refusal where numbers are handed out. A 21-line prototype of exactly that (section 5.4)
stops the incident: the foreign state writes zero receipt lines, zero journal lines and zero
ticket files, and the owner's Rebuild stays clean.

That alone leaves one gap that this batch also closes, because the fix itself would open it:
the refusal tells the caller to run `aira reconcile --rebuild`. An agent in the wrong
directory that follows that advice turns its private state into an up-to-date *second
writer* of the real log. A second writer that is up to date collides with the owner later,
in the sequence-number space: the owner's next event gets a sequence number the foreign
state already journaled, the journal refuses the owner's line, and the owner's own database
then disagrees with the journal, so the **owner's** Rebuild fails. The only recovery is to
move the machine-wide `state.db` aside, which loses every project's leases and telemetry. So
the second rule (one writer per log) is built too, as a small marker file naming the owning
state.

AIRA-297's minimum is the one guarded UPDATE the ticket loop already has. The two Rebuild
loops are 60-line near-duplicates, so the UPDATE goes into one shared helper both loops call,
which deletes the duplicate and a one-line alias.

| Item | Alternative | Why rejected |
|---|---|---|
| 301 | Only refuse a receipt whose ID or sequence number already appears with a different partner (check at append) | Fires only after the foreign database has committed its allocation, so that database is left inconsistent; misses a sequence number that collides with a journal line rather than a receipt; does nothing for non-ID writes (`aira set` writes the ticket file before its journal append). The counter rule refuses inside the minting transaction, before anything is committed. |
| 301 | Before every mint, read the log's highest sequence number and refuse if the counter is not above it | Also catches a database restored from an old copy, but adds a full journal read to every mutation (the journal append already does one), and still allows an up-to-date second writer. |
| 301 | Rebuild automatically when a database first meets a project (the phase-1 spec's "full scan during machine DB creation") | A foreign state would then silently adopt the real repository and become the up-to-date second writer described above. Refuse instead; rebuilding stays an explicit act. |
| 301 | Identify the owner by a random token stored in the database instead of the state directory path | Would also catch a recreated database in the owner's own directory, but then every database-loss recovery needs a takeover step. The counter rule already covers a recreated database, so the path is enough and keeps recovery as it is (`aira reconcile --rebuild`). |
| 301 | Claim the marker only at `aira init` | Every existing repository would stay unguarded until someone runs init in it. Claiming at the owner's first registration covers them all at cutover. |
| 301 | A takeover verb (`aira init --take-ownership`) | Needed only when the owner deliberately moves their state directory. A documented manual step is enough; a verb would also be a verb a wrong-directory agent could run. |
| 301 | Make Rebuild skip a contradicting receipt and report it as a finding | Deferred with its design recorded (section 7). After both rules ship, no current writer can produce a contradiction; today's behaviour on one is already fail-closed and named in `aira check`; tolerance needs a precedence rule (first line wins, journal wins over receipts) inside the correctness-critical Rebuild. Instead, the existing refusal is made to name the contradicting receipt's worktree, path and time (3.1 A.3), so a hand repair is a one-line search. |
| 297 | Copy the guarded UPDATE into the requirement loop | Two copies of a 60-line block that must stay identical. One helper, and one mutation then reds both kinds' pins. |
| 296 | Compact or prune dead breadcrumbs | Declined by triage: AIRA-88 and AIRA-209 decided a dead breadcrumb stays as recoverable evidence. |
| 296 | Skip writing a breadcrumb identical to the latest one for the same project and root | Measured and prototyped; deferred for its test-seam cost (section 7). |

## 3. Design

### 3.1 AIRA-301 part A: absorb the log before handing out numbers

Files: `internal/store/store.go`, `internal/codes/produced_test.go`.

**A.1 Honest counter seed.** `registerDB` (`store.go:2021`) computes, before its transaction,
`seed := 1`, or `0` when `auditLogPresent(s.auditDir)` is true, and inserts
`event_counters(project_id, next_seq) VALUES(?, seed) ON CONFLICT(project_id) DO NOTHING`
(today the literal `1`, `store.go:2064-2065`).

- New helper `auditLogPresent(auditDir string) bool`: true when `receipts.jsonl` or
  `journal.jsonl` in `auditDir` has size > 0, or when either cannot be `stat`ed for any
  reason other than "does not exist" (an unreadable log is treated as present: the
  conservative direction, which costs at most one rebuild).
- It does not filter by project id. In production one common directory is one project (the
  project id is the hash of the canonical common directory, `store.go:595`), so a non-empty
  log there is this project's. A test that shares a common directory between two project ids
  would see a conservative "rebuild first"; the prototype run found none.
- Existing rows are untouched (`DO NOTHING`): the box's database already has a counter row
  for every project it serves, so there is no upgrade step.
- The value 0 is not a valid next sequence number (sequence numbers start at 1), so it reads
  as "not yet set from the log" without a new column. Rebuild's existing counter upsert
  (`store.go:2990-2994`, `CASE WHEN event_counters.next_seq < excluded.next_seq`) lifts 0 to
  the log's high water plus one, which is what "absorbed" means. `aira check` and
  `aira reconcile --rebuild` both run Rebuild; init adoption runs Rebuild before
  `RegisterBootstrap`, whose `DO NOTHING` keeps Rebuild's value.

**A.2 Refuse at the one place numbers are handed out.** `nextSequence` (`store.go:3431`):
after reading the row, `next < 1` returns
`E_RECONCILE_REQUIRED: this AIRA state has not read the audit log of project <id> yet, so its counters cannot be trusted; run 'aira reconcile --rebuild' (or 'aira check') first. Nothing was written.`

- Every sequence number in the store comes from `nextSequence` (20 call sites, all in
  `internal/store`: tickets, IDs, requirements, imports, leases, supervisor leases, rants,
  retire, watch). So one check covers ID allocation and every journaled event. The refusal
  happens inside the caller's `BEGIN IMMEDIATE` transaction, which rolls back, so nothing is
  committed: no allocation row, no receipt, no ticket file, no journal line.
- The no-row branch (`INSERT ... 2`, return 1) is left as it is: every production path that
  mints runs in a registered scope (whose `registerDB` seeded the row) or after Rebuild's
  upsert created it.
- Because of part B, a *foreign* state never reaches this refusal (it is stopped at
  registration). This message is seen only by the owner's own recreated database, where its
  advice is right.

**A.3 Name the contradicting receipt.** `ensureReceiptAllocation`'s seq-mismatch refusal
(`store.go:3267`) appends the receipt's `worktree_id`, `path` and `at` and the file it came
from (`inspect <common>/aira/receipts.jsonl`), as the AIRA-93 message in
`ensureAllocationEvent` already does for the journal (`store.go:3329`). Code unchanged.

**Codes.** Delete `E_RECONCILE_REQUIRED` from `cataloguedNotProduced`
(`produced_test.go:86`); measured: `TestDivergenceTablesAreCurrent` fails until it is
removed. Its exit bucket stays 4 (pinned at `produced_test.go:282`).

### 3.2 AIRA-301 part B: one owner per audit log

New file `internal/store/audit_owner.go`; call sites in `store.go`; a new code.

**The marker.** `<git-common-dir>/aira/owner.json`, one JSON line
`{"state_dir":"<canonical directory holding state.db>"}`. The identity is
`canonicalPath(filepath.Dir(s.dbPath))` (symlinks resolved, `store.go:682`), the same for
the daemon and for an in-process store on the same state.

**Functions.**

- `readAuditOwner() (owner string, present bool, err error)`: "does not exist" is absent. A
  read error, invalid JSON, an empty `state_dir`, or a directory in place of the file is an
  error, never "absent": `E_STATE_NOT_OWNER: the owner marker <path> is unreadable (<detail>); refusing to write. Nothing was written.`
- `refuseForeignOwner() error`: present and not equal to this state's directory returns
  `E_STATE_NOT_OWNER` (message below).
- `claimAuditOwner() error`: write the JSON to a temporary file in the audit directory,
  `fsync`, then `os.Link` it to `owner.json` (atomic and never overwrites), remove the
  temporary file, `fsync` the directory. On "already exists", re-read and compare
  (`refuseForeignOwner`). Never `os.WriteFile`/truncate.
- `requireAuditOwner(ctx) error`: `refuseForeignOwner`; if the marker is absent and this
  state's counter for the project is set (`event_counters.next_seq >= 1`), claim; if absent
  and the counter is not set, return nil (part A stops that state from minting, so it can
  never append).

**Call sites (the guard is the two writers of the log, plus an early door).**

1. `Register` (`store.go:1975`) and `RegisterBootstrap` (`store.go:2003`): call
   `refuseForeignOwner` first (after the ejection check, before the breadcrumb), then, after
   `registerDB`, `requireAuditOwner`. A foreign state is therefore refused before it writes
   anything: no breadcrumb, no database row, no worktree file, no audit line. Every verb
   that opens a project scope, reads included, fails with the reason. That is intended: the
   incident was an agent that did not know it was in the real repository.
2. `appendReceiptIfMissing` (`store.go:2506`, the only writer of `receipts.jsonl`, verified
   by grep), before it takes the receipts lock.
3. `journalEvent` and `journalEventBounded` (`store.go:2469`, `:2488`, the only callers of
   `appendEventIfMissing*`, the only writers of `journal.jsonl`), before the append.

The append-time checks are what make the rule structural: they also cover
`NewUnregisteredScope` (init adoption runs Rebuild, which may append recovered receipts,
before `RegisterBootstrap`) and a daemon's cached scope if the marker changes underneath it.

**The claim rule, in one sentence:** a state whose counter is set claims an unowned log at its
first registration or first append, whichever comes first; a state whose counter is not set
never claims; nothing ever overwrites the marker. At cutover the box daemon's discovery pass
re-registers every registered worktree, so every live repository is claimed by the owner
within one pass (about 33 s).

**Refusal text.** `E_STATE_NOT_OWNER: <common>/aira belongs to the AIRA state at <owner>; this process uses <ours> (HOME or XDG_STATE_HOME differ). Nothing was written. If this is not the repository you meant, check your working directory. To move this repository to this state on purpose, stop the other state's daemon, then remove <common>/aira/owner.json.`

**Daemon.** The check runs in the store, i.e. in the process that owns the database. A client
with a foreign `XDG_STATE_HOME` runs its own daemon on its own state (AIRA-289), and that
daemon is refused. Read-only stores (`OpenReadOnly`) and `NewUnregisteredScope` lifecycle
checks write nothing and are not checked.

**What the marker guards.** `receipts.jsonl` and `journal.jsonl` only. Other common-directory
writers (the runner's `runs/` ledger, the gate audit, `locks/`) are outside it (section 7).

### 3.3 AIRA-297: one shared "scanned file to allocation" step

`internal/store/store.go`, `Rebuild`.

- New method `(s *Store) reconcileScannedAllocation(ctx, conn, id, worktreeID, path,
  pathKind string, journal []eventRecord, receiptKeys map[string]bool) (seq int64,
  recovered *AllocationReceipt, err error)`. Its body is the ticket loop's allocation block
  (`store.go:3075-3136`) with `kindTicket` replaced by `pathKind`: look up the allocation;
  no row: reconcile the kind against `pathKind`, mint a `recovered` row and its event; a row:
  reconcile the kind against the row, validate or rebuild its event, **re-point a
  still-`allocated` row at the scanned file** (the guarded UPDATE from `store.go:3127-3131`),
  and return a recovered receipt when the receipts log lacks this allocation's key.
- Both loops call it after their own index upsert (tickets: `INSERT INTO tickets` and
  `replaceRelationIndex`; requirements: `INSERT INTO requirements`) and keep the
  `maxSeq` update and the `recovered` append.
- Delete `ensureRecoveredEvent` (`store.go:3306`), a one-line alias of
  `ensureAllocationEvent` with reordered parameters whose four callers all move into the
  helper.
- Behaviour change, exactly one: a requirement allocation row still `allocated` is now
  re-pointed at the last-scanned file, as ticket rows have been since AIRA-175.
  `materialised` and `retired` rows are never moved (the `state='allocated'` guard).
- Stated side effect: `importRequirementRow` refuses an existing allocation whose path is not
  this worktree's path (`import_requirements.go:236-240`). Re-pointing an `allocated`
  requirement row therefore moves which single worktree may re-import it, from the allocating
  worktree to the last-scanned one. Today exactly one worktree is accepted either way; only a
  requirement that is `allocated` (crash window, or `aira id` without a create) and whose
  file is in several worktrees is affected. Ticket imports adopt across worktrees and are
  unaffected.

## 4. Invariants

1. A state whose counter for a project was never set from that project's audit log hands
   out no sequence number for it, and therefore no ID, event, receipt or journal line. The
   refusal is inside the minting transaction: nothing is committed anywhere.
2. A counter is set from the log only by Rebuild (which reads every receipt, journal line,
   scanned file and ref) or by the first registration of a project whose log is empty.
3. `receipts.jsonl` and `journal.jsonl` are appended to only by the state named in
   `owner.json`. Nothing overwrites `owner.json`; a missing one is claimed only by a state
   whose counter is set.
4. An unreadable or malformed marker refuses every registration and append; it is never
   treated as missing.
5. A foreign state is refused at registration, before it writes a breadcrumb, a database
   row, a worktree file or an audit line.
6. Rebuild's receipt handling is otherwise unchanged: a contradicting receipt still aborts
   it with `E_JOURNAL_CORRUPT`, now naming that receipt's worktree, path and time. No
   receipt is ever skipped.
7. A scanned entity file re-points its allocation row only while the row is `allocated`, by
   one code path for tickets and requirements.
8. No existing database needs a migration. Set counters and unmarked logs keep working; the
   owner's first registration after the upgrade claims the marker.

## 5. Tests (TDD; each must fail against the wrong implementation)

Write each test first and record it RED on `4098ad9` (or RED against the named mutation).
Every mutation listed must turn at least one test RED. **Isolation:** tests use temp
directories only; any probe or e2e run happens in a throwaway worktree under `~/tmp` with
private `HOME`/`XDG_*`; no aira verb runs in a real checkout.

### 5.1 Part A (`internal/store`)

- `TestARecreatedStateCannotMintBeforeItReadsTheLog` (same state directory, the
  lost-database shape): owner creates tickets, updates one, allocates an `AR` ID; close;
  delete `state.db*`; reopen in the same directory. `CreateTicket`, `AllocateID` (ticket and
  requirement prefixes), `CreateRequirement`, `ImportTicketsBytes`, `UpdateTicket`, a lease
  claim and a rant each return `E_RECONCILE_REQUIRED`; `receipts.jsonl` and `journal.jsonl`
  are byte-identical before and after; no new file under `.aira/`. Then `Rebuild`;
  `CreateTicket` returns the ID after the log's highest and its allocation seq is after the
  log's highest; a second `Rebuild` is clean. RED on `4098ad9` (the mint succeeds and its
  receipt contradicts the log). Mutations: seed 1 always; delete the `next < 1` check.
- `TestAJournalOnlyLogAlsoBlocksMinting`: the log holds only journal lines (a rant), no
  `receipts.jsonl`; a recreated database's create is refused. Mutation: `auditLogPresent`
  looks at `receipts.jsonl` only.
- `TestFirstRegistrationOnAnEmptyLogMintsAtOnce` (false-fail twin): a new project creates
  without a rebuild. Mutation: seed 0 always.
- `TestInitAdoptionLeavesTheCounterSet`: `NewUnregisteredScope` + `Rebuild` +
  `RegisterBootstrap` on a repository with a log, then a create succeeds. Mutation:
  `registerDB` uses `DO UPDATE SET next_seq=excluded.next_seq`.
- `TestRebuildContradictionNamesTheReceipt` (A.3): a hand-written contradicting receipt
  makes Rebuild fail with a message containing that receipt's path, worktree and `at`.
  Mutation: drop the fields.
- Codes: `TestDivergenceTablesAreCurrent` is RED until the `cataloguedNotProduced` entry is
  removed (measured on the prototype).

### 5.2 Part B (`internal/store`, plus one `cmd/aira` e2e)

- `TestAForeignStateIsRefusedAtRegistration`: owner (state X) creates a ticket (claims);
  state Y with the same common directory and checkout: `Open` returns `E_STATE_NOT_OWNER`;
  both audit files byte-identical; Y's `registry.jsonl` gains no line; no file written. RED
  on `4098ad9`. Mutation: drop the `Register` check (Open succeeds).
- `TestAnUpToDateForeignStateCannotAppend`: Y through `NewUnregisteredScope` + `Rebuild`
  (counter set, never registered): `CreateTicket` returns `E_STATE_NOT_OWNER` at the receipt
  append and `receipts.jsonl` is byte-identical; `UpdateTicket` on an existing ticket returns
  `E_STATE_NOT_OWNER` at the journal append and `journal.jsonl` is byte-identical. (The
  ticket file `UpdateTicket` writes before its journal append is the residual of this
  unregistered test seam; production scopes are registered and refused earlier.) Mutations:
  drop the receipt-append check; drop the journal-append check.
- `TestTheOwnerMarkerIsClaimedOnlyByAStateWithASetCounter`: (a) new project: after the first
  registration `owner.json` names the canonical state directory, also when the state
  directory is reached through a symlink; (b) unmarked repository with a log: the owner
  (counter set) reopens and the marker is created at registration; (c) a database in another
  directory, log present, marker absent: `Open` succeeds, no marker is created,
  `CreateTicket` returns `E_RECONCILE_REQUIRED`, the marker is still absent. Mutation: claim
  regardless of the counter (RED in c).
- `TestConcurrentClaimsLeaveOneOwner`: two states race `claimAuditOwner` (goroutines,
  repeated over fresh common directories): exactly one marker, its content never changes,
  the loser gets `E_STATE_NOT_OWNER`. Mutation: write the marker with `os.WriteFile`.
- `TestAnUnreadableOwnerMarkerRefusesWrites`: invalid JSON, an empty `state_dir`, a
  directory named `owner.json`: registration and appends refused, marker unchanged.
  Mutation: treat an unreadable marker as absent.
- `TestAForeignInitAdoptionIsRefused`: Y runs `NewUnregisteredScope` + `Rebuild` +
  `RegisterBootstrap` with no receipt-less files (so Rebuild appends nothing):
  `RegisterBootstrap` returns `E_STATE_NOT_OWNER`, the marker is unchanged. Mutation: drop
  the `RegisterBootstrap` check.
- e2e, `cmd/aira` (in-process dispatcher with a test daemon, as
  `TestStoreFreeRealCLIRegistersOnlyThroughDaemon` does): a temp repository initialised and
  one ticket created under `XDG_STATE_HOME=A`; then with `XDG_STATE_HOME=B` and a separate
  `XDG_RUNTIME_DIR`, `create` and `list` both exit 1 with `E_STATE_NOT_OWNER` and
  `receipts.jsonl` is byte-identical. The incident, at the binary boundary. RED on
  `4098ad9`.
- Codes: `E_STATE_NOT_OWNER` catalogued at 1 and added to `TestStateConflictCodesExitOne`'s
  table (`produced_test.go:371`).

### 5.3 AIRA-297 (`internal/store`)

- `TestCheckResolvesAnAllocatedRequirementFromTheCheckingWorktree`: the mirror of the
  AIRA-175 pin (`import_tickets_test.go:695`): `AR-1` allocated in `wt-a`, valid file only in
  `wt-c`: no `E_ID_UNRESOLVED`, `allocated-id-file` passes, the allocation path is re-pointed
  to `wt-c`. RED on `4098ad9` (measured).
- `TestRebuildNeverRepointsAMaterialisedAllocation`, table-driven over ticket and
  requirement: materialised in `wt-a`, a copy in `wt-c`: the path stays `wt-a`. Mutation:
  drop `state='allocated'` from the UPDATE.
- Single-site mutation: delete the UPDATE from the helper: the AIRA-175 ticket pin and the
  new requirement pin both go RED.
- Every existing Rebuild test stays green unchanged (the ticket path is a pure extraction).

### 5.4 Prototype measurement (evidence for this plan; not the build)

In a detached throwaway worktree of `4098ad9` under `~/tmp`, parts A and B were prototyped
(A as in 3.1; B with the marker checked at `Register`, `RegisterBootstrap`, the receipt
append and both journal appends, claiming at append):

- the two AIRA-301 probes: every foreign create refused; zero bytes added to either audit
  file; zero ticket files; owner Rebuild clean (A alone: `E_RECONCILE_REQUIRED`; A+B: refused
  at `Open` with `E_STATE_NOT_OWNER`);
- `go test` of `internal/store`, `internal/core`, `internal/app`, `cmd/aira`: all `ok`;
- `internal/codes`: only the two expected catalogue edits (the stale entry; the new code);
- `internal/daemon` (3 tests) and `internal/runner` (20 tests) fail identically on unmodified
  `4098ad9` when run inside an `aira confine` job on this box (exclusive-drain and
  confine-admission tests): environmental, not this batch. The build gate is `make ci`, and
  any failure must be compared against the base before it is attributed.

## 6. Expected yield

- The 2026-10-11 incident cannot recur: on the prototype the foreign state writes nothing,
  where master writes 2 bogus receipts (same checkout) or 4 receipts and 4 ticket files
  (another checkout of the same repository) and leaves Rebuild failing in every worktree.
- A recreated state database can no longer write into a repository's log before its first
  rebuild (the same mechanism as the incident, without any foreign state).
- An agent in the wrong directory, or with a private `HOME`, is told so by name at its first
  aira command in a real repository, and writes nothing.
- `E_RECONCILE_REQUIRED`, specified in phase 1, is produced for the first time.
- AIRA-297: one false `E_ID_UNRESOLVED` class removed, with a net deletion of the duplicated
  allocation block and the alias.

## 7. Deferrals and "not in this batch"

| Item | Why not here |
|---|---|
| AIRA-296: skip a breadcrumb identical to the latest one for its project and root | Measured yield 90.2 % of records. Cost: three tests count registry lines as registrations (`TestStoreFreeRealCLIRegistersOnlyThroughDaemon`, `cmd/aira/dispatcher_inprocess_test.go:1364`, a cross-process e2e with no equally cheap counter; `TestDiscoverRegistryPassQuarantinesPersistentStoreFailure`, `discovery_test.go:314`; `TestEnsureScopeRegistersFreshAndRefreshesCachedScopeExactlyOnce`, `server_test.go:143`; all three RED on the prototype). Present cost is small: about 740 KB decoded five times per `aira check` (`readRegistry` callers: `Rebuild`, `checkDuplicateIDs`, `findingIndexDivergence`, `relationFindings`, traceability) and once per discovery pass. Suggested trigger: registry over 5 MB (about a year at the measured rate), or when a registration-counter seam exists. Compaction by deadness stays declined (AIRA-88, AIRA-209). Recommend: keep AIRA-296 held with this record. |
| Rebuild tolerance for contradicting receipts | Design recorded so it is not re-derived: a pure pass over receipts and journal in file order adopts a receipt only if no journal line holds its sequence number for a different ID and no earlier adopted receipt pairs its ID or its sequence number differently; the rest are skipped with a failing `E_RECEIPT_CONFLICT` reconciliation finding (cleared and recomputed each Rebuild) reported under `rebuild-integrity`; high-water marks keep counting the skipped lines; the log is never rewritten. Safety property: it changes the outcome only for logs that fail today. Deferred because after parts A and B no current writer can produce a contradiction. |
| A database restored from an old copy | Its counter is set, so part A passes it, and its numbers would collide. Rebuild still fails closed, now naming the receipt. No backup feature exists to produce this. |
| Upgrade window | An up-to-date foreign state could claim an unmarked repository before the owner's first registration after cutover. The owner's daemon claims every registered worktree in its first discovery pass. |
| `aira eject` releasing `owner.json` | Not needed for the bug; moving a repository between states is a manual step. |
| Other common-directory writers (`runs/` ledger, gate audit, `locks/`) | Not part of the receipt/journal audit log the incident corrupted. |
| Refusing reads from a state that has not read the log | Reads show that state's own (empty or stale) index; part B already stops foreign states at registration. |
| `readRegistry` called five times per `aira check` | A performance clean-up, independent of growth. |
| The daemon and runner test failures under `aira confine` (5.4) | Pre-existing on `4098ad9`; suggest a ticket if none exists. |

## 8. Risks

- **A legitimate second state is now refused** (the owner changes `HOME` or
  `XDG_STATE_HOME`; a container bind-mounting the same checkout path with its own state).
  Checked: fastest-ee's in-container gate runs only machine-level verbs
  (`aira install --ci=shim`, `aira confine`) and containers use their own clones, which have
  their own common directory. The refusal names the manual step.
- **Takeover is a hand step inside `.git`** (agents cannot do it). It is needed only for a
  deliberate state move, or if a foreign state wins the upgrade window.
- **Fixture churn:** none measured on the prototype; the final code may differ.
- **Exit codes:** `E_RECONCILE_REQUIRED` stays 4 (pinned). `E_STATE_NOT_OWNER` is 1 by the
  catalogue's rule (`codes.go:25-30`): a well-formed request refused by durable state, which
  succeeds once the state changes.
- **Cost:** one small-file read per audit append and per registration; two `stat` calls per
  registration. Both are noise beside the full journal decode every journal append already
  does (`store.go:3871-3888`).
- **Cutover:** store code runs in the daemon, so the daemon must restart (`aira install`
  does it since AIRA-203). The first discovery pass then writes `owner.json` into every live
  repository's common directory. Protocol unchanged; old clients unaffected.
- **Build and review isolation:** the incident came from an agent running aira verbs in the
  real checkout. Builders and reviewers run probes only in throwaway worktrees under `~/tmp`
  with private `HOME`/`XDG_*`, chain setup with `&&`, and never run aira write verbs in a
  real checkout.

## 9. Stable codes

| Code | Status | Exit | Where |
|---|---|---|---|
| `E_RECONCILE_REQUIRED` | existing, now produced | 4 | `nextSequence` on a counter not yet set from the log |
| `E_STATE_NOT_OWNER` | new | 1 | registration and the two audit-log writers, when `owner.json` names another state or cannot be read |
| `E_JOURNAL_CORRUPT` | existing, message only | 4 | `ensureReceiptAllocation` now names the receipt's worktree, path and time |

No other code changes. Protocol unchanged.

## 10. Release notes (for the build)

Protocol unchanged. Daemon restart required (performed by `aira install`). After cutover,
verify on the box that `<common>/aira/owner.json` exists in the aira and fastest-ee
repositories and names `~/.local/state/aira`. Skill: add one line to the agent guide's
isolation advice: a probe in a private state that lands in a real repository is now refused
with `E_STATE_NOT_OWNER`, and `aira reconcile --rebuild` must not be run to "fix" it.
Notify deploy and speed per the release rule (new code `E_STATE_NOT_OWNER`;
`E_RECONCILE_REQUIRED` now produced).

## 11. Review log

(empty: plan v1)
