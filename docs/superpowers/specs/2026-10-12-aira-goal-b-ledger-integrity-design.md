# AIRA ledger and receipt integrity: foreign-state writers, requirement re-point, registry growth

Status: PLAN v2 (2026-10-12; v1 reviewed by two lineages, findings applied or rejected in
section 11). Tickets: AIRA-301 (P2 bug), AIRA-297 (P3 bug), AIRA-296 (P3 chore, rescoped by
triage). Base: master `4098ad9` (v0.34 plus ticket-housekeeping merges).

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
`E_WRITE_CONFLICT` (the file already exists) still leaves its receipt. The receipt writer
skips a receipt whose project, ID and sequence number all match an existing line
(`store.go:2540`; other fields are not compared). Why 71 creates left 68 lines was not
established; the truncated file is gone.

**Root cause: two rules the design states but nothing enforces.**

1. *A database may hand out numbers only after it has read the log.* The phase-1 spec says
   reconciliation "never silently proceeds on a stale projection" and returns retryable
   `E_RECONCILE_REQUIRED` (`2026-08-08-aira-phase1-design.md:567`, `:624`), and that a new
   machine database is rebuilt from the common-dir journals and receipts (`:205`). Nothing
   implements it: `registerDB` seeds a never-seen project's sequence counter at 1
   (`store.go:2064-2065`) whatever the log already holds, `nextSequence` invents a counter
   row at 1 when none exists (`store.go:3434-3438`), and `E_RECONCILE_REQUIRED` is
   catalogued but produced nowhere (`internal/codes/produced_test.go:86`).
2. *One state writes a repository's audit log.* AIRA's design is one machine-wide state
   (one daemon per state directory, AIRA-289), but nothing stops a second state from
   appending to a repository's shared files.

| Ticket | Still reproduces? | Evidence (2026-10-12, `4098ad9`) | Verdict |
|---|---|---|---|
| AIRA-301 foreign state appends receipts | Yes, measured | Store-level reproduction (probe in `~/tmp`, not committed): owner state creates 3 tickets, updates one twice, creates 2 more. A second state (different `state.db`, same common dir, same checkout) runs 5 creates: all 5 return `E_WRITE_CONFLICT`, yet `receipts.jsonl` grows from 5 to 7 lines (AIRA-4 seq 4, AIRA-5 seq 5; the first three matched the owner's receipts on project, ID and sequence number and were skipped). Owner `Rebuild`: `E_JOURNAL_CORRUPT: receipt AIRA-4 has seq 4 but allocation has seq 6`; owner `Check`: verdict fail, `rebuild-integrity` fail. Variant from an empty checkout of the same repository: the second state writes 4 ticket files and 4 receipts, its journal appends are refused (`journal key conflict`, the existing guard at `store.go:3882-3885`), and the owner's Rebuild fails the same way. | Build |
| AIRA-297 requirement allocation never re-pointed | Yes, measured | Requirement AR-1 allocated in `wt-a` (`AllocateID("AR")`), a valid `AR-1.md` only in `wt-c`; `c.Check()` reports `E_ID_UNRESOLVED AR-1 allocation has no materialised requirement file`, and the allocation path still names `wt-a`. Control in the same test: ticket AIRA-1 in the identical shape is re-pointed to `wt-c` and resolves (the ticket loop's guarded UPDATE, `store.go:3127-3131`; the requirement loop, `store.go:3141-3201`, has none). No crash and no worktree removal are needed to show it. | Build |
| AIRA-296 registry growth | Growth is real; the cheap remainder has a cost | On a copy of `~/.local/state/aira/registry.jsonl` taken 2026-10-12: 2,366 records, 738,108 bytes, 14 distinct project ids (and 14 common directories), 228 distinct worktree ids and 228 distinct roots, of which 33 roots still exist. 2,134 records (90.2 %) are identical, apart from `at`, to the latest earlier record for the same project and root. Last 7 days: 42-151 records per day, 38-150 of them such duplicates. A prototype that skips an identical breadcrumb removes that growth but breaks three tests that use the registry line count as their registration counter (section 7). | Drop from this batch, design recorded |

## 2. Greenfield minimum, and rejected alternatives

If AIRA-301 had to be solved alone, from nothing, the minimum is three pieces:

1. **A counter that was never set from the log is explicit, and refused.** A missing counter
   row or the value 0 means "not yet set"; the one place numbers are handed out refuses it
   with `E_RECONCILE_REQUIRED`, inside the minting transaction, before any side effect.
2. **One atomic ownership claim per repository, decided before side effects.** A marker file
   names the owning state. It is created with an exclusive create, never overwritten, and is
   decided at registration, before the registration writes anything.
3. **A claim needs proof that the state has absorbed the log**, not just a positive counter.
   A database written before this change may hold a counter seeded at 1 against a populated
   log (`store.go:2064`), indistinguishable from an honest counter by its value. The proof is
   cheap and needs no new column: the counter must exceed the highest sequence number the
   log holds for the project (the "high water"). The state that wrote the log always passes;
   a stale state does not. The test runs only while a repository is unmarked, so its full
   log read happens once per repository for the owner.

Why piece 1 alone is not enough: its refusal advises `aira reconcile --rebuild`. An agent in
the wrong directory that follows that advice turns its private state into an up-to-date
*second writer*, which collides with the owner later in sequence-number space (the owner's
next event gets a sequence number the foreign state already journaled; the journal refuses
the owner's line; the owner's Rebuild fails; the only recovery is moving the machine-wide
`state.db` aside, which loses every project's leases and telemetry). Pieces 2 and 3 close
that for every repository the owner has claimed. They do not close it for a repository that
is still unmarked (section 7, residual 1).

AIRA-297's minimum is the one guarded UPDATE the ticket loop already has. The two Rebuild
loops are 60-line near-duplicates, so the UPDATE goes into one shared helper both loops call,
which deletes the duplicate and a one-line alias.

| Item | Alternative | Why rejected |
|---|---|---|
| 301 | Only refuse a receipt whose ID or sequence number already appears with a different partner (check at append) | Fires only after the foreign database has committed its allocation, so that database is left inconsistent; misses a sequence number that collides with a journal line rather than a receipt; does nothing for non-ID writes (`aira set` writes the ticket file before its journal append). The counter rule refuses inside the minting transaction, before anything is committed. |
| 301 | Before every mint, read the log's high water and refuse if the counter is not above it | Also catches a database restored from an old copy, but adds a full log read to every mutation. v2 does this read only while a repository is unmarked (the claim test), not per mint. |
| 301 | Claim when the counter is merely set (`next_seq >= 1`), as v1 did | A pre-upgrade database seeded at 1 against a populated log passes it and could claim an unmarked repository and mint colliding numbers (review finding, section 11). Replaced by "counter above the log's high water". |
| 301 | A "hydrated" column, set only by Rebuild or by a first registration on an empty log | Equivalent proof, but every existing counter row on the box would start un-hydrated, so the owner's daemon would refuse every mutation in every repository until someone runs `aira check` there. The high-water test gives the same proof with no schema change and no cutover outage. |
| 301 | Rebuild automatically when a database first meets a project (the phase-1 spec's "full scan during machine DB creation") | A foreign state would then silently adopt the real repository and become the up-to-date second writer. Refuse instead; rebuilding stays an explicit act. |
| 301 | Identify the owner by a random token stored in the database instead of the state directory path | Would also catch a recreated database in the owner's own directory and states at the same path in different mount namespaces, but then every database-loss recovery needs a takeover step. The counter rule already covers a recreated database; the path residual is stated (section 7). |
| 301 | Claim at the first audit append (v1) | Lets any append path, including Rebuild under `aira check`, take ownership, and puts claims in three places. v2 claims in one function, called at registration and once inside Rebuild (3.2), under one rule. |
| 301 | Claim only at registration, never inside Rebuild (a review suggestion) | Rebuild appends recovered receipts after its transaction commits. With appends requiring the marker, a recovering owner in an unmarked repository would fail its first Rebuild and succeed only on the next command. Calling the same claim function after Rebuild's commit changes nothing about who may claim (Rebuild has just made the counter exceed the high water, so the next registration would claim anyway) and removes the failing first run. |
| 301 | Claim only at `aira init` | Every existing repository would stay unguarded until someone runs init in it. Claiming at the owner's first registration covers them all at cutover. |
| 301 | Write the marker to a temporary file and hard-link it into place (v1) | Hard links are doubtful on DrvFS. An exclusive create (`O_CREAT|O_EXCL`) is simpler; the price is a zero-length file visible for the moment between create and write, handled in 3.2. |
| 301 | A mint-time marker read in `nextSequence` | Every CLI command re-runs `Register` before it acts (cached daemon scopes too, `server.go:1357-1358`), so ownership is already decided before each command's first mutation. A per-mint file read would only cover a marker changed by hand in the middle of one command; the append-time refusals cover the shared log in that window (3.2). |
| 301 | A takeover verb (`aira init --take-ownership`) | Needed only when the owner deliberately moves their state directory. A documented manual step is enough; a verb would also be a verb a wrong-directory agent could run. |
| 301 | Make Rebuild skip a contradicting receipt and report it as a finding | Deferred with its design recorded (section 7). Today's behaviour on one is already fail-closed and named in `aira check`; tolerance needs a precedence rule inside the correctness-critical Rebuild. Instead, the existing refusal is made to name the contradicting receipt's worktree, path and time (3.1 A.4), so a hand repair is a one-line search. |
| 301 | Keep `nextSequence`'s no-row branch (v1) | It contradicts rule 1 directly: a row that does not exist was never set from the log, yet it hands out 1. Deleted (3.1 A.2). Measured on a probe: `internal/core`, `internal/app` and `cmd/aira` stay `ok`; the one store test found to depend on it builds a `Store` by hand without a counter row (`watch_test.go:107`) and is fixed by seeding the row (section 5.4). |
| 297 | Copy the guarded UPDATE into the requirement loop | Two copies of a 60-line block that must stay identical. One helper, and one mutation then reds both kinds' pins. |
| 296 | Compact or prune dead breadcrumbs | Declined by triage: AIRA-88 and AIRA-209 decided a dead breadcrumb stays as recoverable evidence. |
| 296 | Skip writing a breadcrumb identical to the latest one for the same project and root | Measured and prototyped; deferred for its test-seam cost (section 7). |

## 3. Design

### 3.1 AIRA-301 part A: absorb the log before handing out numbers

Files: `internal/store/store.go`, `internal/codes/produced_test.go`.

**A.1 The log's high water.** New helper
`auditHighWater(auditDir, projectID string) (h int64, known bool)` (built as `readAuditLog` plus `logHighWater`, the read error returned as itself): the largest `seq` over the
lines of `receipts.jsonl` and `journal.jsonl` whose project id is `projectID`, using the same
decoders Rebuild uses. Both files absent: `(0, true)`. Any other read or decode error:
`known=false`, which every caller treats in the conservative direction (no seed of 1, no
claim, and at registration in an unmarked repository a refusal carrying the read error,
"Nothing was written", never a demotion: build review R3.2). It reads the whole log, so it is called only where 3.2 says: for a counter row about
to be created, and while a repository is unmarked.

**A.2 Honest counter seed, and no invented counter.**

- `registerDB` (`store.go:2021`) seeds a new `event_counters` row with 1 when the high water
  is known and 0, otherwise with 0 (today the literal `1`, `store.go:2064-2065`). Existing
  rows are untouched by the seed (`DO NOTHING`); the one change to an existing row is the
  demotion in 3.2.
- `nextSequence` (`store.go:3431`): no row, or `next < 1`, returns
  `E_RECONCILE_REQUIRED: this AIRA state has not read the audit log of project <id>, so its counters cannot be trusted. If this is your repository and your usual AIRA state, run 'aira reconcile --rebuild' (or 'aira check'). If you did not expect to be in this repository, stop: rebuilding would make this state a writer of its log. Nothing was committed.`
  The no-row branch that inserts 2 and returns 1 (`store.go:3434-3438`) is deleted.
- The value 0 is not a valid next sequence number (sequence numbers start at 1), so it reads
  as "not yet set from the log" without a new column. Rebuild's counter upsert
  (`store.go:2990-2994`, `CASE WHEN event_counters.next_seq < excluded.next_seq`) lifts 0 to
  the log's high water plus one, which is what "absorbed" means. That upsert runs *before*
  Rebuild's scanned-file loops, which themselves mint recovered allocations
  (`store.go:3089`, `:3164`); the order is load-bearing and gets its own test and mutation
  (5.1). `aira check` and `aira reconcile --rebuild` both run Rebuild.
- Every sequence number in the store comes from `nextSequence` (20 call sites, all in
  `internal/store`: tickets, IDs, requirements, imports, leases, supervisor leases, rants,
  retire, watch). The refusal happens inside the caller's `BEGIN IMMEDIATE` transaction,
  which rolls back. "Nothing was committed" holds only if each call site mints before it
  writes a file or a log line; the creates and path mutations read for this plan do
  (`prepareCreate`, `preparePathMutation` run before `appendReceiptIfMissing` and
  `materialiseIntent`, `store.go:2151-2160`, `:2337-2345`). The build checks the remaining
  sites one by one, and any site that writes before minting is a finding, not a silent pass.

**A.3 Codes.** Delete `E_RECONCILE_REQUIRED` from `cataloguedNotProduced`
(`produced_test.go:86`); measured: `TestDivergenceTablesAreCurrent` fails until it is
removed. Its exit stays 4. That is a deliberate carry-over, not the 1-versus-2 rule of
`codes.go:25-30` (by that rule a refusal that succeeds once the state changes would be 1):
the code is pinned at 4 in the store-integrity family with its runner analogue
`E_RUN_RECONCILE_REQUIRED` (`produced_test.go:282`, `codes.go:203`, `:265`), and moving it
would move that family.

**A.4 Name the contradicting receipt.** `ensureReceiptAllocation`'s seq-mismatch refusal
(`store.go:3267`) appends the receipt's `worktree_id`, `path` and `at` and the file it came
from (`inspect <common>/aira/receipts.jsonl`), as the AIRA-93 message in
`ensureAllocationEvent` already does for the journal (`store.go:3329`). Code unchanged.

### 3.2 AIRA-301 part B: one owner per audit log

New file `internal/store/audit_owner.go`; call sites in `store.go`; a new code.

**The marker.** `<git-common-dir>/aira/owner.json`, one JSON line
`{"state_dir":"<canonical directory holding state.db>"}`. The identity is
`canonicalPath(filepath.Dir(s.dbPath))` (symlinks resolved, `store.go:682`), the same for
the daemon and for an in-process store on the same state.

**Reading it.** `readAuditOwner()` returns one of: absent (does not exist), ours, other
(names a different directory), unreadable (read error, invalid JSON, empty `state_dir`, a
directory in its place). A zero-length file is re-read up to three times 20 ms apart, because
an exclusive create is visible for a moment before its content is written; still empty after
that, it is unreadable (a claim that crashed between create and write). Unreadable is never
treated as absent.

**Claiming it.** `claimAuditOwnerIfEligible(ctx, counter int64) (claimed bool, high int64, err error)`, the
only function that creates the marker:

1. Eligible only if the high water is known, `counter > highWater` (3.1 A.1), and this
   state's database holds every line of the log for the project: each journal line as an
   event with the same sequence number and the same line identity (actor, time, verb, target
   and digest, as `appendEventIfMissing` compares; the digest covers only verb and target, so
   two states' updates of one ticket at one sequence number differ only in actor and time,
   build review R4.1), and each receipt Rebuild
   would adopt (valid ID, positive sequence number) as an allocation with the same sequence
   number (`holdsAuditLog`; build review R3.1: a counter advanced by appends that were
   refused can pass the high water without the state having read another state's lines).
   Not eligible: return `false, nil`.
2. `os.MkdirAll(<common>/aira)` (a fresh repository may register before anything else has
   created the directory), then `open(owner.json, O_CREAT|O_EXCL|O_WRONLY)`, write the line,
   `fsync` the file, `fsync` the directory.
3. "Already exists" (another process won the race): re-read; ours is success, anything else
   is `E_STATE_NOT_OWNER`. Nothing ever truncates or rewrites the marker.

**Refusal text.** `E_STATE_NOT_OWNER: <common>/aira belongs to the AIRA state at <owner>; this process uses <ours> (HOME or XDG_STATE_HOME differ). If this is not the repository you meant, check your working directory. To move this repository to this state on purpose: run 'aira check' here with the other state first (it appends that state's deferred lines to the log), stop its daemon, then remove <common>/aira/owner.json.` (Without the first step, the other state's deferred lines stay in its database only, and its sequence numbers can collide with the new owner's: build review R4.1.) For an unreadable marker: `E_STATE_NOT_OWNER: the owner marker <path> is unreadable (<detail>); refusing to write. If no AIRA process is claiming this repository right now, remove <path> by hand.` Each call site below adds what it did and did not write.

**Call sites.**

1. **Registration decides ownership before it writes anything.** `Register`
   (`store.go:1975`) and `RegisterBootstrap` (`store.go:2003`), after the ejection check and
   before the breadcrumb:

   | Marker | Action | What this registration writes |
   |---|---|---|
   | unreadable or other | refuse `E_STATE_NOT_OWNER` ("Nothing was written.") | nothing |
   | ours | proceed | breadcrumb, rows; a new counter row is seeded by A.2 |
   | absent | `counter` = the existing row's value, or for a new row the A.2 seed; call `claimAuditOwnerIfEligible(counter)`; if it refuses, refuse ("Nothing was written."); if it claims, proceed | breadcrumb, rows (only after the claim succeeded) |
   | absent, and the log cannot be read | refuse with the read error ("Nothing was written."); ownership is unknown, so nothing is claimed or demoted | nothing |
   | absent, not eligible (counter not above the high water, or the database does not hold the log) | proceed without claiming, and **demote**: `registerDB` sets this project's counter to 0 (guarded by `next_seq <= max(high, counter read)`, so a concurrent Rebuild's lift above both is not undone) | breadcrumb, rows, counter = 0 |

   A counter above the high water whose database does not hold the log is demoted too (build
   review R4.4): left as it was, it minted, committed to its own database and worktree, and
   was refused only at the append. Its Rebuild meets the line it lacks (adopts it, or fails
   `E_JOURNAL_CORRUPT` naming it), and leaves the counter above every sequence number the
   database already holds as well as above the log's high water, since numbers it burned
   while its appends were refused are appended by its replay after the claim.

   Demotion is what makes a stale pre-upgrade counter safe: a state whose counter is not
   above the log's high water cannot mint (A.2) until it rebuilds. The loser of a concurrent
   claim is refused before its breadcrumb, so two racing states never both write a
   registration. Since every CLI command re-runs `Register` before it acts
   (`server.go:1357-1358` for cached daemon scopes; `NewScope` for fresh ones), a foreign
   state is refused at the start of every command, reads included. That read refusal is a
   policy choice, not a requirement of write integrity: it is kept because ownership has to
   be decided at registration anyway, so refusing there is the one cheapest site, and
   because the incident was an agent that did not know it was in the real repository. Its
   false-fail cost is listed in section 8.
2. **Rebuild never touches a foreign log, and claims only after it has absorbed it.** At its
   start, Rebuild refuses an unreadable or other marker (read only, before its own database
   is touched; this covers init adoption, which runs Rebuild in an unregistered scope before
   `RegisterBootstrap`, and `aira check`). After its transaction commits and before it
   appends recovered receipts (`store.go:3220-3224`), if the marker is absent it calls
   `claimAuditOwnerIfEligible` with its freshly upserted counter. Not eligible is then a bug
   in Rebuild's own counter, and is refused rather than appended. Rebuild's replay of
   deferred journal lines (`replayUnjournaledEvents`) runs at its start only when the marker
   is already ours; in an unmarked repository it runs after the claim, since an append
   before it would be refused and leave the recovering state unable to ever claim (build
   review R3.3). `aira check` and `aira reconcile --rebuild`, the two remedies the refusal
   texts name, reach this claim: in an unmarked repository `check` runs Rebuild before its
   reconcile step, and `reconcile --rebuild` runs Rebuild first when its reconcile is refused
   `E_STATE_NOT_OWNER`; `check` reports `E_STATE_NOT_OWNER` as an integrity finding beside
   the rest of its report (build review R4.2).
3. **Every append requires the marker to name this state.** `appendReceiptIfMissing`
   (`store.go:2506`, the only writer of `receipts.jsonl`, verified by grep), before it takes
   the receipts lock, and `journalEvent` / `journalEventBounded` (`store.go:2469`, `:2488`,
   the only callers of `appendEventIfMissing*`, the only writers of `journal.jsonl`), before
   the append. Absent, other or unreadable: `E_STATE_NOT_OWNER`. These checks never claim.
   With sites 1 and 2 in place they fire only when the marker changed by hand while a
   command was running, or on a later replay of rows written before such a change, so their
   message is narrower: `... The shared log was not changed.
   This state's own database, and for an update the ticket file, may already hold this
   operation (a create commits its allocation before its receipt; an update writes its file
   before its journal line, store.go:2151-2160, :2337-2345); run 'aira check' once ownership
   is resolved.`

**The claim rule, in one sentence:** the marker is created only by a state whose counter for
the project exceeds the log's high water and whose database holds every line of the log, at
registration or after Rebuild's commit, by one exclusive create that never overwrites; every
other state is either refused (marker names someone else) or demoted to "must rebuild first"
(marker absent).

**Daemon.** The check runs in the store, i.e. in the process that owns the database. A client
with a foreign `XDG_STATE_HOME` runs its own daemon on its own state (AIRA-289), and that
daemon is refused. Read-only stores (`OpenReadOnly`) and `NewUnregisteredScope` lifecycle
checks that write nothing are not checked.

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

1. A state hands out a sequence number for a project only from a counter row that exists
   and is at least 1. The refusal is inside the minting transaction, which rolls back.
2. A counter is set from the log only by Rebuild (which reads every receipt, journal line,
   scanned file and ref) or by the first registration of a project whose log high water is
   known to be 0. A registration in an unmarked repository whose counter is not above the
   high water sets it to 0.
3. `receipts.jsonl` and `journal.jsonl` are appended to only by a process whose state is
   named in `owner.json` at the time of the append.
4. `owner.json` is created only by `claimAuditOwnerIfEligible`, by an exclusive create, by a
   state whose counter exceeds the log's high water and whose database holds every line of
   the log; nothing overwrites or truncates it. A log that cannot be read is never claimed
   and never demotes a counter.
5. An unreadable, malformed or (after retries) empty marker refuses registration, Rebuild
   and every append; it is never treated as missing.
6. A state refused at registration (marker names another state or is unreadable, including
   the loser of a concurrent claim) has written no breadcrumb, database row, worktree file
   or audit line.
7. Rebuild's handling of a current-project receipt that contradicts an allocation is
   unchanged: it aborts with `E_JOURNAL_CORRUPT`, now naming that receipt's worktree, path and
   time. Not changed by this batch, and stated so it is not mistaken for a guarantee:
   Rebuild still skips, without a finding, receipts of another project and current-project
   receipts with an invalid ID or a sequence number of 0 or less (`store.go:3016-3018`;
   section 7).
8. A scanned entity file re-points its allocation row only while the row is `allocated`, by
   one code path for tickets and requirements.
9. No schema migration. Existing counter rows keep their values except for the demotion in
   invariant 2; unmarked repositories are claimed by the first eligible registration.

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
  log's highest; a second `Rebuild` is clean. RED on `4098ad9`. Mutations: seed 1 always;
  delete the `next < 1` check.
- `TestRebuildOnARecreatedStateMintsRecoveredAllocations` (the real crash-recovery shape,
  false-fail guard): log present, a ticket file with no receipt (the crash window), database
  recreated (counter 0). `Rebuild` succeeds, mints the recovered allocation with a seq above
  the log's high water and appends its receipt; a second `Rebuild` is clean. Mutation: move
  the `store.go:2990-2994` counter upsert after the scanned-file loops (Rebuild then fails
  `E_RECONCILE_REQUIRED` exactly when it is the advised remedy).
- `TestNoCounterRowIsRefused`: a store whose project has no `event_counters` row (built the
  way `watch_test.go:107` builds one): a mint returns `E_RECONCILE_REQUIRED` and no row is
  created. Mutation: restore the no-row branch.
- `TestAJournalOnlyLogAlsoBlocksMinting`: the log holds only journal lines (a rant), no
  `receipts.jsonl`; a recreated database's create is refused. Mutation: the high water
  reads `receipts.jsonl` only.
- `TestFirstRegistrationOnAnEmptyLogMintsAtOnce` (false-fail twin): a new project creates
  without a rebuild. Mutation: seed 0 always.
- `TestInitAdoptionLeavesTheCounterSet`: `NewUnregisteredScope` + `Rebuild` +
  `RegisterBootstrap` on a repository with a log, then a create succeeds. Mutation:
  `registerDB` uses `DO UPDATE SET next_seq=excluded.next_seq`.
- `TestRebuildContradictionNamesTheReceipt` (A.4): a hand-written contradicting receipt
  makes Rebuild fail with a message containing that receipt's path, worktree and `at`.
  Mutation: drop the fields.
- Codes: `TestDivergenceTablesAreCurrent` is RED until the `cataloguedNotProduced` entry is
  removed (measured on the v1 prototype).

### 5.2 Part B (`internal/store`, plus one `cmd/aira` e2e)

- `TestAForeignStateIsRefusedAtRegistration`: owner (state X) creates a ticket (claims);
  state Y with the same common directory and checkout: `Open` returns `E_STATE_NOT_OWNER`;
  both audit files byte-identical; Y's `registry.jsonl` gains no line and Y's database has
  no `projects`, `worktrees` or `event_counters` row for the project; no file written. RED
  on `4098ad9`. Mutation: drop the `Register` check (Open succeeds).
- `TestAPreUpgradeCounterDoesNotProveReconciliation` (review P1): unmarked repository whose
  log high water is 6; state Y's database holds a counter row of 1 inserted directly (the
  pre-upgrade seed). Y registers: no marker is created, Y's counter is 0, `CreateTicket` and
  `AllocateID` return `E_RECONCILE_REQUIRED`, both audit files byte-identical. Mutations:
  eligibility tests `counter >= 1` instead of `counter > highWater` (Y claims and mints);
  drop the demotion (Y mints seq 1).
- `TestTheOwnerClaimsAtItsFirstRegistrationAfterUpgrade` (false-fail twin): unmarked
  repository, owner database with a counter of high water + 1: registration creates the
  marker naming the canonical state directory (also when the state directory is reached
  through a symlink); a create succeeds; the counter was not demoted. Mutation: demote always.
- `TestANewProjectIsClaimedAtItsFirstRegistration`: empty log, no `<common>/aira`
  directory: registration creates the directory and the marker. Mutation: drop the
  `MkdirAll`.
- `TestConcurrentRegistrationsLeaveOneOwnerAndOneRegistration` (review P1): two states,
  both eligible (empty log), race `Open` on fresh common directories, repeated: exactly one
  marker, its content never changes after creation, the loser gets `E_STATE_NOT_OWNER` and
  has no breadcrumb and no database rows for the project. Mutations: write the marker with
  `os.WriteFile`; write the breadcrumb before the claim.
- `TestAForeignStateCannotRegisterWhileTheOwnerCreates`: the owner creates tickets in a
  loop while Y repeatedly tries to register: every Y attempt is refused, every owner create
  succeeds, the owner's `Rebuild` afterwards is clean.
- `TestAppendsRequireTheMarkerToNameThisState`: a registered owner scope; the marker is then
  replaced by hand with one naming another directory, or removed. `CreateTicket` returns
  `E_STATE_NOT_OWNER` at the receipt append and `receipts.jsonl` is byte-identical;
  `UpdateTicket` returns it at the journal append and `journal.jsonl` is byte-identical; the
  refusal text says the shared log was not changed and does not say nothing was written.
  Mutations: drop the receipt-append check; drop the journal-append check; let an absent
  marker pass at append.
- `TestRebuildRefusesAForeignMarkerBeforeTouchingItsDatabase`: Y (counter set, never
  registered) through `NewUnregisteredScope` + `Rebuild` on a marked repository: refused;
  Y's database unchanged; audit files byte-identical. Mutation: drop Rebuild's start check
  (Rebuild then appends Y's recovered receipts, or reaches the append refusal after
  changing Y's database).
- `TestAdoptionClaimsAnUnmarkedRepositoryAfterRebuild`: `NewUnregisteredScope` + `Rebuild`
  + `RegisterBootstrap` on an unmarked repository with a receipt-less ticket file: Rebuild
  claims, appends the recovered receipt, `RegisterBootstrap` proceeds. Mutation: drop
  Rebuild's claim (Rebuild fails at its append).
- `TestAnUnreadableOwnerMarkerRefusesWrites`: invalid JSON, an empty `state_dir`, a
  directory named `owner.json`, and a zero-length file (the torn claim): registration,
  Rebuild and appends refused, marker unchanged, the message names the hand step. Mutations:
  treat an unreadable marker as absent; treat a zero-length marker as absent.
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

### 5.4 Measurements behind this plan (not the build)

v1 prototype, in a detached throwaway worktree of `4098ad9` under `~/tmp` (A as in v1; B with
the marker checked at `Register`, `RegisterBootstrap`, the receipt append and both journal
appends, claiming at append):

- the two AIRA-301 probes: every foreign create refused; zero bytes added to either audit
  file; zero ticket files; owner Rebuild clean (A alone: `E_RECONCILE_REQUIRED`; A+B: refused
  at `Open` with `E_STATE_NOT_OWNER`);
- `go test` of `internal/store`, `internal/core`, `internal/app`, `cmd/aira`: all `ok`;
- `internal/codes`: only the two expected catalogue edits (the stale entry; the new code);
- `internal/daemon` (3 tests) and `internal/runner` (20 tests) fail identically on unmodified
  `4098ad9` when run inside an `aira confine` job on this box (exclusive-drain and
  confine-admission tests): environmental, not this batch.

v2 changes the claim rule (high water instead of "counter set"; claim before the breadcrumb;
no claim at append), so the v1 prototype numbers are evidence for the mechanism, not for the
v2 code; the build re-measures.

No-row branch deletion, measured for v2 on `4098ad9` in a throwaway worktree with private
`HOME`/`XDG_*` (the branch replaced by an `E_RECONCILE_REQUIRED` return):
`internal/core`, `internal/app`, `cmd/aira`: `ok`. `internal/store`: timed out at 10 min in
`TestEventSequenceCommitOrderAcrossIndependentHandles` (`watch_test.go:107`), which inserts
a `projects` row by hand and no counter row, then waits on a commit hook that the refused
mint never reaches. The rest of `internal/store` with that test skipped (`-skip`): `ok` (404.9 s, exit 0).
The build seeds the counter row in that test's setup (a test-seam fix, not a behaviour
change).

## 6. Expected yield

- In both measured incident shapes (same checkout; another checkout of the same
  repository), a foreign state writes nothing to the shared log, no ticket file, and no
  registration, where master writes 2 bogus receipts (same checkout) or 4 receipts and 4
  ticket files (another checkout) and leaves Rebuild failing in every worktree. This holds
  for every repository with a marker; the residuals in section 7 are where it does not.
- A pre-upgrade or recreated state database can no longer write into a repository's log
  before its first rebuild (the incident's mechanism without any foreign state).
- An agent in the wrong directory, or with a private `HOME`, is told so by name at its first
  aira command in a claimed repository, and writes nothing.
- `E_RECONCILE_REQUIRED`, specified in phase 1, is produced for the first time.
- AIRA-297: one false `E_ID_UNRESOLVED` class removed, with a net deletion of the duplicated
  allocation block, the alias and `nextSequence`'s no-row branch.

## 7. Residuals, deferrals and "not in this batch"

| Item | Why not here |
|---|---|
| **Residual 1: an unmarked repository can be claimed by an up-to-date foreign state.** | A foreign state that rebuilds an unmarked repository (`aira check`, or `aira reconcile --rebuild` following the `E_RECONCILE_REQUIRED` advice) gets a counter above the high water and then claims at its next registration. Nothing distinguishes it from the owner at that point. Bounded by: the owner's daemon registers every registry root that still exists and matches its identity on each discovery pass (`discovery.go:58-72`), so every repository the owner has a live worktree registration for is claimed in the first pass after cutover; a repository with no live registry root stays unmarked until the owner's next aira command there. The `E_RECONCILE_REQUIRED` text tells a wrong-directory agent not to rebuild. |
| **Residual 2: the owner identity is a path string.** | Two states at the same canonical path in different mount namespaces (containers sharing a bind-mounted checkout, each with its own state at an identical path) look like one owner: an ownership false pass. fastest-ee's containers use their own clones, with their own common directory, and run only machine-level verbs. A random owner token was rejected (section 2). |
| **Residual 3: a database restored from an old copy, in the owner's own directory.** | The marker names it, its counter is set, so neither part stops it; its numbers would collide. Rebuild still fails closed, now naming the receipt. No backup feature exists to produce this. |
| **Residual 4: the append-time refusal is after side effects in the state's own database and worktree.** | Only reachable when the marker is changed by hand during a command, or on a later replay of a row written before such a change (3.2 site 3); a state that does not hold the log is demoted at registration and cannot mint (build review R4.4). The shared log is protected; the refusal says so and does not claim nothing was written. |
| Rebuild silently skips malformed current-project receipts (`store.go:3016-3018`) | Pre-existing false pass (review finding): a current-project receipt with an invalid ID or a sequence number of 0 or less is skipped without a finding, and `aira check` can still report `rebuild-integrity` pass. Not changed here: turning it fail-closed inside the correctness-critical Rebuild could fail every box `aira check` if such lines exist, and the box logs have not been measured (this plan does not read the real `.git`). Suggest a ticket: measure the box's logs, then report such lines as a failing reconciliation finding. |
| Discovery reports `(0, 0, 0)` when the registry cannot be read (`discovery.go:34-37`) | Pre-existing: an unknown reported as a measured zero (review finding). Its counters are observability-only today, but the residual-1 bound above depends on discovery having read the registry, so the release step checks the markers directly (section 10). Suggest a ticket: report the pass as unevaluated. |
| AIRA-296: skip a breadcrumb identical to the latest one for its project and root | Measured yield 90.2 % of records. Cost: three tests count registry lines as registrations (`TestStoreFreeRealCLIRegistersOnlyThroughDaemon`, `cmd/aira/dispatcher_inprocess_test.go:1364`, a cross-process e2e with no equally cheap counter; `TestDiscoverRegistryPassQuarantinesPersistentStoreFailure`, `discovery_test.go:314`; `TestEnsureScopeRegistersFreshAndRefreshesCachedScopeExactlyOnce`, `server_test.go:143`; all three RED on the prototype). Present cost is small: about 740 KB decoded five times per `aira check` (`readRegistry` callers: `Rebuild`, `checkDuplicateIDs`, `findingIndexDivergence`, `relationFindings`, traceability) and once per discovery pass. Suggested trigger: registry over 5 MB (about a year at the measured rate), or when a registration-counter seam exists. Compaction by deadness stays declined (AIRA-88, AIRA-209). Recommend: keep AIRA-296 held with this record. |
| Rebuild tolerance for contradicting receipts | Design recorded so it is not re-derived: a pure pass over receipts and journal in file order adopts a receipt only if no journal line holds its sequence number for a different ID and no earlier adopted receipt pairs its ID or its sequence number differently; the rest are skipped with a failing `E_RECEIPT_CONFLICT` reconciliation finding (cleared and recomputed each Rebuild) reported under `rebuild-integrity`; high-water marks keep counting the skipped lines; the log is never rewritten. Safety property: it changes the outcome only for logs that fail today. Deferred because after parts A and B no current writer in a claimed repository can produce a contradiction. |
| `aira eject` releasing `owner.json` | Not needed for the bug; moving a repository between states is a manual step. |
| Other common-directory writers (`runs/` ledger, gate audit, `locks/`) | Not part of the receipt/journal audit log the incident corrupted. |
| Refusing reads from a state that has not read the log (counter 0, marker absent or ours) | Reads show that state's own (empty or stale) index; it cannot write. Foreign states are already refused at registration. |
| `readRegistry` called five times per `aira check` | A performance clean-up, independent of growth. |
| The daemon and runner test failures under `aira confine` (5.4) | Pre-existing on `4098ad9`; suggest a ticket if none exists. |

## 8. Risks

- **False fails (stated limits).** Refused at every command, reads included, once a
  repository is claimed: the owner after changing `HOME` or `XDG_STATE_HOME`, or after
  moving the state directory; a legitimate second state on the same repository (a second
  user account, a container bind-mounting the same checkout with its own state). Each gets
  `E_STATE_NOT_OWNER` naming both states and the manual step. Tests are unaffected: they use
  temporary repositories, each claimed by its own test state. Not refused: a different clone
  of the same project (its own common directory).
- **Cutover demotion.** A repository whose log holds a higher sequence number than the
  owner's counter (only possible if the log already contains another state's lines) is
  demoted at the owner's first registration: the owner then gets `E_RECONCILE_REQUIRED` there
  until `aira check`, whose Rebuild either absorbs the log or fails `E_JOURNAL_CORRUPT`
  naming the bad receipt. That is the honest outcome: those numbers would collide.
- **Takeover is a hand step inside `.git`** (agents cannot do it). It is needed only for a
  deliberate state move, a torn marker, or if a foreign state claimed an unmarked repository
  (residual 1).
- **Fixture churn:** one measured (`watch_test.go:107` needs a counter row); the final code
  may find more hand-built stores.
- **Exit codes:** `E_RECONCILE_REQUIRED` stays 4, a deliberate carry-over (3.1 A.3).
  `E_STATE_NOT_OWNER` is 1 by the catalogue's rule (`codes.go:25-30`): a well-formed request
  refused by durable state, which succeeds once the state changes.
- **Cost:** one small-file read per registration, per Rebuild and per audit append. A full log
  read (the high water) only when a counter row is created and while a repository is
  unmarked; a non-owner state that keeps running commands in an unmarked repository repeats
  it per command, which is acceptable for a state that cannot write. Both are noise beside
  the full journal decode every journal append already does (`store.go:3871-3888`).
- **Cutover:** store code runs in the daemon, so the daemon must run the new binary.
  `aira install` restarts a daemon whose running binary differs from the installed one
  (AIRA-203, `install.go:1234`), but a 2026-10-08 release recorded a client install that left
  the old daemon running; the release step therefore checks and restarts explicitly
  (section 10). The first discovery pass then writes `owner.json` into every live
  repository's common directory. Protocol unchanged; old clients unaffected.
- **Build and review isolation:** the incident came from an agent running aira verbs in the
  real checkout. Builders and reviewers run probes only in throwaway worktrees under `~/tmp`
  with private `HOME`/`XDG_*`, chain setup with `&&`, and never run aira write verbs in a
  real checkout.

## 9. Stable codes

| Code | Status | Exit | Where |
|---|---|---|---|
| `E_RECONCILE_REQUIRED` | existing, now produced | 4 (carry-over) | `nextSequence` on a missing counter row or a counter not yet set from the log |
| `E_STATE_NOT_OWNER` | new | 1 | registration, Rebuild's start, and the two audit-log writers, when `owner.json` names another state or cannot be read; the writers also when it is absent |
| `E_JOURNAL_CORRUPT` | existing, message only | 4 | `ensureReceiptAllocation` now names the receipt's worktree, path and time |

No other code changes. Protocol unchanged.

## 10. Release notes (for the build)

Protocol unchanged. Daemon must run the new binary: after `aira install`, confirm the running
daemon's binary is the new one and otherwise run `systemctl --user restart
aira-daemon.service` (owner step; agents do not restart the shared daemon). Then verify on
the box that `<common>/aira/owner.json` exists in the aira and fastest-ee repositories and
names `~/.local/state/aira`, and that `aira check` passes in each (a demoted repository shows
`E_RECONCILE_REQUIRED` on writes until then). Skill: add one line to the agent guide's
isolation advice: a probe in a private state that lands in a real repository is now refused
with `E_STATE_NOT_OWNER`, and `aira reconcile --rebuild` must not be run to "fix" it or an
`E_RECONCILE_REQUIRED` in a repository you did not expect to be in. Notify deploy and speed
per the release rule (new code `E_STATE_NOT_OWNER`; `E_RECONCILE_REQUIRED` now produced).

## 11. Review log

v1 was reviewed by two lineages (both PASS_WITH_CHANGES). Each finding was checked against
the code at `4098ad9` before it was accepted.

| # | Finding (severity) | Verified | Disposition |
|---|---|---|---|
| R1.1 | A positive counter does not prove reconciliation: a pre-upgrade row seeded at 1 against a populated log could claim an unmarked repository and mint colliding numbers (P1) | Yes: `store.go:2064-2065` seeds 1 regardless of the log, and v1's claim test was `next_seq >= 1` | Accepted. Claim needs `counter > log high water` (2, 3.2); an ineligible registration demotes the counter to 0; new tests `TestAPreUpgradeCounterDoesNotProveReconciliation` and its owner twin. A "hydrated" column was considered and rejected (cutover outage, section 2). |
| R1.2 | "Nothing was written" is false at append-time refusals; concurrent registrations can write breadcrumbs and rows before losing the claim (P1) | Yes: create commits its allocation before its receipt (`store.go:2151-2160`); update writes its file before its journal line (`:2337-2345`); v1 claimed after the breadcrumb | Accepted. The claim is decided before the breadcrumb (3.2 site 1); append-time refusals never claim and say only that the shared log was not changed; racing tests added (5.2). A per-mint marker read was considered and rejected (section 2). |
| R1.3 | Rebuild silently skips current-project receipts with invalid IDs or non-positive seq, so "no receipt is ever skipped" is untrue; discovery returns `(0,0,0)` on a registry read failure (P2) | Yes: `store.go:3016-3018`; `discovery.go:34-37` | Accepted as wording: invariant 7 now states the skip; both recorded as pre-existing, with suggested tickets (section 7). Not fixed here: changing Rebuild's skip to fail-closed is unmeasured against the box logs. |
| R1.4 | False-fail exposure needs explicit limits; identical path strings across mount namespaces may be different databases; avoid "cannot recur" (P2) | Yes (design reasoning) | Accepted: section 8 lists the false fails; residual 2 states the namespace false pass; section 6 no longer claims the incident cannot recur. |
| R1.5 | Receipt dedup compares only project, ID and seq, not bytes, so the "byte-identical twins" explanation is unsupported (P2) | Yes: `store.go:2540` | Accepted: section 1 corrected; the 71-to-68 gap is now stated as not established. |
| R1.6 | From-scratch minimum (P3) | n/a | Adopted as the section 2 minimum. |
| R1.7 | Read refusal is optional policy, not needed for write integrity; keep registry compaction, receipt tolerance and takeover out of scope (P3) | n/a | Partly accepted: the read refusal is kept but now stated as a deliberate policy with its reasons and false-fail cost (3.2 site 1, section 8); the out-of-scope items were already deferred. |
| R2.1 | All cited code claims true; AIRA-296's "232 identities" is not project ids (14); say what was counted (INFO) | Re-measured on a fresh registry copy: 14 project ids, 14 common directories, 228 worktree ids, 228 roots, 33 roots existing; 2,134 of 2,366 records (90.2 %) duplicates | Accepted: section 1 now gives the measured counts; "232" was not reproduced and is removed. |
| R2.2 | `nextSequence`'s no-row branch contradicts invariant 1; delete it and run the suite (P2) | Yes: `store.go:3434-3438` | Accepted and measured (5.4): one store test depends on it through a hand-built store; fixed by seeding its row. |
| R2.3 | No test pins that a Rebuild on a recreated database mints recovered allocations; moving the early counter upsert after the loops would break the advised remedy (P2) | Yes: upsert at `store.go:2990-2994`, mints at `:3089`, `:3164` | Accepted: `TestRebuildOnARecreatedStateMintsRecoveredAllocations` with that mutation (5.1). |
| R2.4 | A counter-0 foreign state can seize any unmarked repository by rebuilding; state the residual; consider one claim site (P2) | Yes: `discovery.go:58-72` registers only live, matching roots | Residual accepted and stated (section 7, residual 1). Claim sites: one function, no claim at append; it is also called after Rebuild's commit, for the reason in section 2 (a claim-only-at-registration design makes a recovering owner's first Rebuild fail). |
| R2.5 | Fresh init may register before the audit directory exists; prefer `O_CREAT|O_EXCL` to tmp+link; add a torn-marker test (P3) | Plausible (not traced end to end); `MkdirAll` is harmless either way | Accepted: `MkdirAll` before the claim, exclusive create with a bounded re-read of a zero-length marker, torn-marker and new-directory tests (3.2, 5.2). |
| R2.6 | "`aira install` restarts the daemon since AIRA-203" needs checking; `E_RECONCILE_REQUIRED` at exit 4 contradicts `codes.go:25-30` (P3) | AIRA-203 restarts a daemon whose running binary differs (`install.go:1234`); a 2026-10-08 release recorded an install that did not restart. Exit 4 is pinned in the store-integrity family (`produced_test.go:282`) | Accepted: section 10 checks and restarts explicitly; exit 4 stated as a deliberate carry-over (3.1 A.3). |
| R3.1 | Build review: the claim test (counter above the high water) is satisfied by sequence numbers burned in the state's own database while its appends were refused, so a state that never read another state's lines could claim and append (P2) | Yes, reproduced (`TestABurnedCounterDoesNotProveAbsorption` RED on `e3aebfa`) | Fixed: the claim also needs the database to hold every log line (`holdsAuditLog`). Such a state is not claimed (and, since R4.4, is demoted); the state that did absorb the log still claims (same test). Mutation: drop the check, RED. |
| R3.2 | Build review: an unreadable log at registration wrote an unconditional counter 0 (a durable value for an unknown) (P2) | Yes, reproduced (`TestAnUnreadableLogRefusesRegistrationAndKeepsTheCounter` RED on `e3aebfa`) | Fixed: registration in an unmarked repository refuses with the read error and writes nothing; the unguarded demotion is deleted. Mutation: treat the read error as not eligible, RED. |
| R3.3 | Build review: Rebuild replayed deferred journal lines before its claim, so a demoted state with one in an unmarked repository was refused forever by the remedy the refusal advises (P2) | Yes, reproduced (`TestRebuildClaimsBeforeItReplaysDeferredJournalLines` RED on `e3aebfa`) | Fixed: replay at the start only when the marker is ours, otherwise after the claim; the absent-marker append text now names `aira check` as the step that claims (true at the verb boundary only after R4.2). Mutation: replay first again, RED. |
| R3.4 | Build review: no test for invariant 6 with a state that cannot claim over an unreadable marker (P2, test gap) | Yes (mutation O13 survived the suite) | Test added: `TestAnUnreadableMarkerRefusesAFreshState`; O13 RED. |
| R3.5 | Build review: no test pins counting invalid-ID receipts toward Rebuild's counter (P2, test gap) | Yes (mutation O01 survived the suite) | Tests added: `TestAnInvalidIDReceiptAtTheHighWaterStillLetsRebuildClaim`, `TestAnInvalidIDReceiptSequenceNumberIsNotReissued`; O01 RED on both. |
| R4.1 | Build review: `holdsAuditLog` and Rebuild's journal loop compared verb, target and digest only; the digest is H(verb, target), so a state's burned update of a ticket matched another state's update of it at the same sequence number, and the state claimed a log it had not absorbed (P2) | Yes, reproduced (`TestABurnedUpdateOfTheSameTicketDoesNotProveAbsorption` RED on `ab7c0c48`) | Fixed: both compare the full line identity (actor and time too, `sameJournalLine`, as `appendEventIfMissing` already did); Rebuild then fails `E_JOURNAL_CORRUPT` naming the sequence number. The takeover text now says to run `aira check` with the old state first. |
| R4.2 | Build review: R3.3 was fixed for a direct Rebuild only; `aira check` and `aira reconcile --rebuild` run reconcile first, whose replay is refused in an unmarked repository, so neither remedy could claim (P2) | Yes, reproduced (`TestCheckClaimsAnUnmarkedRepositoryWithADeferredLine`, `TestCheckNamesTheLineThatStopsTheClaim`, `TestReconcileRebuildClaimsAnUnmarkedRepositoryWithADeferredLine` RED on `ab7c0c48`) | Fixed: `check` rebuilds first in an unmarked repository; `reconcile --rebuild` rebuilds first when its reconcile is refused `E_STATE_NOT_OWNER`; `check` classifies `E_STATE_NOT_OWNER` as an integrity finding. |
| R4.3, R4.5 | Build review: the receipt half of `holdsAuditLog` and most of its sub-checks were unpinned (P2, test gap) | Yes (mutations survived the suite) | Pinned: `TestEveryHoldsAuditLogComparisonIsPinned`, one refused case per comparison (receipt ID, receipt seq, journal digest, verb, target, actor, time, missing event row) and false-fail guards (seq-0 receipt, another project's lines). |
| R4.4 | Build review: a counter above the high water whose database does not hold the log was neither claimed nor demoted, so it minted and was refused only at the append, after side effects (P2) | Yes, reproduced (`TestAStateThatDoesNotHoldTheLogIsDemotedBeforeItWrites` RED on `ab7c0c48`) | Fixed: such a state is demoted too; Rebuild's counter also stays above the database's own sequence numbers (`TestRebuildAfterADemotionCountsAboveItsOwnEvents`). Residual 4 and the claim rule restated. |
