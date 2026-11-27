# Session priority tiers: high / medium / low sessions in admission order (AIRA-381)

Status: plan, round 6 (final cut), for the Fable plan gate; order only (D6). AIRA-381, v0.47;
correctness-critical (two-loop, Opus builds, Fable and Sol review). Builds on AIRA-307/309
(`2026-10-13-aira-queue-order-design.md`), AIRA-340 (`--priority high`) and AIRA-350
(`2026-11-18-fixed-small-lane-design.md`). Code references: master at `dc19915c`.

## Summary for the owner

You mark a session HIGH or LOW from `aira top` (key `t`); unmarked sessions are MEDIUM. A mark
changes one thing, **the order in which waiting jobs are considered**, exactly as `--priority high`
does today one level up. It adds no rule that stops a job starting; running jobs are untouched.

| Waiting job | Considered |
|---|---|
| pushed with `aira confine --front` (any session) | 1st (latest push first, as today) |
| HIGH session, `--priority high` job | 2nd |
| HIGH session, ordinary job | 3rd |
| MEDIUM session (the default), `--priority high` job | 4th |
| MEDIUM session, ordinary job | 5th |
| LOW session, `--priority high` job | 6th |
| LOW session, ordinary job | 7th |

Inside each row, oldest first. With no marks the order, the small-job lane included, is exactly
today's. Examples, on a box where memory is short and jobs queue:
- You mark session `field` HIGH. Its next `make ci` is considered before the 15 medium jobs that
  queued earlier and starts at once if it fits and nothing ahead is stuck; if not, it becomes the
  front job the box protects under today's rules (jobs behind it may start past it up to half its
  booking, plus the 5-minute catch-up pass, plus small jobs that leave it room), as a
  `--priority high` job does today.
- You mark a sweep session LOW. Its 4 GiB job is considered after every waiting medium and high
  job (past a stuck medium 10 GiB job it can still start within that job's 5 GiB allowance). On a
  quiet box it starts at once: LOW means "behind everyone else waiting", never "only when idle".

What tiers do not do, said plainly:
- **A lower-tier job can still start ahead of a stuck higher-tier job** (the allowance and the
  catch-up pass). You chose this (order only): no new way for a job to get stuck.
- **A LOW session's running job keeps its memory while higher tiers keep queueing.** A nested
  `aira confine` inside a LOW job is itself LOW (same owner), so while medium and high jobs keep
  arriving and fitting it can wait indefinitely, and its parent holds its reservation meanwhile.
- **The small-job lane still lets small jobs pass larger ones, in the same order.** Jobs booking
  1 GiB or less (default) still go ahead of larger queued jobs through the lane whatever their
  tier, as long as the lane has room and they leave room for the next job in line; about 59% of
  long waits are such jobs (4,821 of 8,233, reported, unverified). The lane offers its room in the
  order above, so when it is short a HIGH session's small job gets it before a LOW one's. (Amended
  at build review, D4: a lane in arrival order changed grants for sessions that set no mark.)
- **Test-suite worker claims are untiered** (L2): ordered MEDIUM whatever their session's tier,
  though their rows show the session's mark. A push (`--front`) goes first whatever the tier.
- **A mark follows an owner string exactly** (the owner in the job's scope id; the SESSION cell
  may show a tmux name or a shortened id instead). `cc-<session id>` may change after `/clear` or
  `/resume` (unverified); `AIRA_CONFINE_OWNER=field` is the predictable key.
- **Marks survive a daemon restart** (L3; 21 restarts in 14 days, reported) until set back to
  MEDIUM. At most 64. A mark whose session has no jobs is not shown in `aira top`;
  `aira confine --list` lists every mark and `aira confine-session-tier <owner> medium` removes one.

## 1. The problem, and the greenfield minimum

**Problem.** The owner wants a standing, per-session say in who is admitted first, set by hand,
with a LOW tier the box has never had. Today's levers are per job: `--front` (a one-off push) and
`--priority high` (set at launch). Neither can demote a session, and neither applies to a
session's future jobs.

**Premise measurement** (decision rule written before the query: stop if fewer than 1% of confined
runs in 14 days waited 60 s or more). Query, read-only, `confine_peak_history.wait_ms` (AIRA-323):

```bash
aira confine --memory-reserve 256M -- sqlite3 -readonly ~/.local/state/aira/state.db \
 "SELECT count(*), sum(wait_ms>=60000), sum(wait_ms>=300000), sum(wait_ms>=1800000), max(wait_ms)/1000
  FROM confine_peak_history WHERE wait_ms IS NOT NULL
  AND at >= strftime('%Y-%m-%dT%H:%M:%SZ','now','-14 days');"
```

Result 2026-11-27 (data from 2026-11-15): 31,167 runs; 8,224 (26%) waited 60 s or more; 3,379
(11%) 5 min or more; 413 30 min or more; longest 7,457 s. Verdict: **build** (ten times the bar).

**Greenfield minimum.** A daemon table `owner -> high|low` (medium is no row), consulted by the
ONE ordering function at evaluation time; one setter verb; the `t` key; the tier on rows. Nothing
per waiter, so a mark reorders queued jobs on the next pass. That is this design, plus:

| Addition | Why it stays |
|---|---|
| Marks stored in `state.db` (L3) | Without it every mark is silently wiped about every 16 hours (21 restarts in 14 days). One table in a database the daemon already opens; §3.1. |
| 64-row cap, refusal only | With stored marks and no automatic prune, the only bound on the table and the reply. |
| A writer mutex | Two concurrent setters would otherwise lose an acknowledged mark. |
| The CLI form of the verb | The only way to see (via `confine --list`) and remove a mark whose session has no rows in `aira top`. |

Cut: any hold or budget change (D6); the `confine --session-tier` flag; a second confirmation
page; `set_at`/`set_by` (the journal records the setter); `jobs_now`; a `queued_tier` field; a
load-failure warning field (nil is unevaluated); a per-waiter tier; a tier on the admit request;
an observe-only mode (an empty table is today, T1); an MCP or Skill setter (L6); automatic
pruning (R9); idle-mark rows in `aira top`; the mark list in the `aira top` footer; any recovery
from a failed load other than a daemon restart.

## 2. Decided by the owner (2026-11-27), recorded, not reopened

- **D1 Identity.** Keyed on the owner string `aira confine` puts in the scope id
  (`resolveConfineOwner`, `cmd/aira/main.go:2291`: `--owner`, `AIRA_CONFINE_OWNER`,
  `cc-<CLAUDE_CODE_SESSION_ID>`, worktree id, inferred `@cwd-<dir>`); subagents share it.
- **D2 Composition.** Pushed first (latest first); then tier HIGH, MEDIUM, LOW; inside a tier
  `--priority high` before normal; FIFO inside each bucket.
- **D3** LOW waits (no age cap, no promotion). **D4** (as decided) The lane ignores tiers and job
  priorities. **Amended at build review, for the owner to confirm or veto (§3.4):** the lane
  offers its room in evaluation order, as today; a small job of any tier still passes larger
  queued jobs through it.
  **D5** Running jobs are unaffected (no preemption, reservation or cpu.weight change).
- **D6 Order only** (owner line L8, option A). Tiers change only `admitOrderLocked`, exactly as
  `--priority high` does (AIRA-340: "queue order only, never the head rule"). No zero-budget rule,
  no strict hold, no new anchor, phase or diagnosis token; the head rule, the AIRA-307 budget and
  the backstop are unchanged.
- **D7 Marks persist** across daemon restarts (L3).

## 3. Design

### 3.1 The table and its storage

`Server` holds `tiers atomic.Pointer[map[string]sessionTier]` (`sessionTier` is `high` or `low`;
no `medium` row exists) and `tiersMu sync.Mutex`. The map is immutable once published. A nil
pointer means **unevaluated** (a failed load, ci-shim mode, a `Server` built without `Serve`): the
evaluator orders it as an empty table, the list reply omits `session_tiers` (§3.5), and every set
is refused (below).

**One load per reader, by structure.** `s.tiers.Load()` has exactly three call sites: an
evaluator pass's start, the `confine-list` case's start, the setter. `admitOrderLocked`, the lane
pre-pass, the walk and `admitSliceSnapshotFor` take the map as an argument and never read
`s.tiers`; the aggregate `admitSliceSnapshot` (`admit.go:1342`, used only by
`admitQueueDiagnostics` at `:1560` for the queued count and phase, both order-independent)
passes nil. The build review checks the three sites by grep; no test is claimed for it.

**Set.** Under `tiersMu`, for the whole operation: a nil pointer (ci-shim, where a container is
one session and nothing is loaded, or a failed load) refuses with ONE refusal,
`E_CONFINE_UNAVAILABLE` exit 4, whose message names both causes ("session tiers are unevaluated on
this daemon: it runs in ci-shim mode (a container is one session), or the stored marks did not
load at daemon start (see the daemon journal, fix the rows it names and restart the daemon)"),
touching nothing. Otherwise copy the map; apply the change (`medium` deletes the row); over 64 rows refuse
`E_CONFINE_SESSION_TIERS_FULL` (a new row only; re-set and remove always pass); write the copy to
the store; only then publish the pointer and write the audit line
`confine-session-tier: setter=<caller> session=<owner> tier=<old>-><new>` (like
`logConfineFront`). A store failure refuses `E_CONFINE_UNAVAILABLE: store session tiers: <cause>`
and publishes nothing. No `queue.mu` (no lock-order edge); the store write has a bounded context
(10 s). Accepted gap: a commit that lands but whose call then reports the timeout refuses the set
while the row is stored; memory stays the old table, and the next acknowledged set (one
delete-all-and-insert transaction from memory) or a restart's load brings the two together.
The new order applies on the next evaluator pass (the existing poll tick).

**Store.** One table in `state.db`, added to the schema list beside `confine_peak_history`
(`internal/store/store.go:878`):

```sql
CREATE TABLE IF NOT EXISTS confine_session_tiers (
    owner TEXT PRIMARY KEY, tier TEXT NOT NULL CHECK (tier IN ('high','low')))
```

Two store methods: `ConfineSessionTiers(ctx) (map[string]string, error)` and
`ReplaceConfineSessionTiers(ctx, map[string]string) error`, the latter one transaction deleting
every row and inserting the map (at most 64 rows). Because a set runs only on a table loaded from
the store, the replace never deletes a row the daemon did not read, and after every acknowledged
set the store equals memory by construction. Not a file: the daemon already opens `state.db` as
its single writer; a file needs a new path, an encoding and a temp-fsync-rename writer.

**Load.** Once in `Serve`, after `OpenDB` (`server.go:493`) and before `net.Listen` (`:514`), so
the first pass and every re-declared survivor see the stored marks (structural: nothing enqueues
before `Listen`). Any failure (query error, an owner `runner.ValidateConfineOwner` refuses, more
than 64 rows) leaves the pointer **nil**: one journal line naming the cause and the manual
remedy (`sqlite3 <state.db> "SELECT owner, tier FROM confine_session_tiers"`, then
`DELETE FROM confine_session_tiers WHERE owner='<owner>'` for each bad row, then restart the
daemon), `session_tiers` absent, sets refused, never a refusal to start (a schema failure inside `OpenDB` refuses start
today, unchanged). The only way out is a daemon restart; nothing deletes rows the daemon did not
read. ci-shim neither loads nor sets (nil throughout). No automatic prune (R9).

**Valid keys.** What `runner.ValidateConfineOwner` accepts, except empty and `unknown` (refused
`E_CONFINE_ARGUMENT_INVALID`); `@cwd-<dir>` accepted (L4); exact match, never by prefix.

### 3.2 The order

**Whose tier.** A waiter's key is the owner embedded in its scope id,
`runner.ParseConfineScopeID(waiter.scopeID)` (`internal/runner/confine.go:1476`), the parse every
row display and the `--front`/kill guards use; never `waiter.owner`, the request's claim, which
`validateAdmitArgs` accepts when the scope id has no owner tail (I6).

**Untiered, always MEDIUM:** a sub-reservation (`isSubReservation()`, `admit.go:453`: aitest
workers, `confine-reserve` holds; a running job's internal progress, D5); a scope-less waiter
(`aira run`, no owner; R7); a scoped waiter with no owner tail.

**The function.** `admitOrderLocked(queue)` (`internal/daemon/admit.go:475`) becomes
`admitOrderLocked(queue, tiers)`: one pass over `queue.waiters` into seven buckets (pushed;
HIGH/job-high; HIGH/normal; MEDIUM/job-high; MEDIUM/normal; LOW/job-high; LOW/normal), pushed
sorted by descending `pushSeq` as today, every other bucket in `queue.waiters` (enqueue) order. With
an empty table the MEDIUM buckets hold every unpushed waiter and the result is today's
`pushed, high, rest` element for element. Its callers pass the map their reader loaded (§3.1):
the evaluator walk (`admit.go:2676`), the snapshot (`admit.go:1405`, through a new `tiers`
argument of `admitSliceSnapshotFor`) and the lane's reference (§3.4). Nothing else changes.

### 3.3 The accepted consequence, and its bound

The head rule is untouched: the head is still the first visited job-level, ungated waiter refused
on capacity that could fit an empty slice, whatever its tier, and every later job-level waiter is
judged against it by the unchanged switch (`admit.go:2738-2750`). So a lower-tier waiter can be
granted ahead of a blocked higher-tier head. Exactly how much, from the code:

1. **Budget.** In ordinary passes, a fitting job-level waiter behind an unpushed head H is granted
   only while `H.bypassed + charge <= H.charge / 2` (`admitBypassBudget`, `admit.go:508`);
   `bypassed` is written only at `admit.go:2757`, never reset, so over H's wait such grants total
   at most half H's booking, all tiers together; higher tiers are offered it first.
2. **Backstop.** The first pass at or after the anchor plus `maxHold` (default 5 min;
   `AIRA_DAEMON_ADMIT_FREEZE_MAX_HOLD`, minimum 1 s, or disabled, `paths.go:297-322`) grants every
   fitting waiter behind an unpushed head; the next withheld fit re-arms (`admit.go:2609-2624`).
3. **Lane.** A small job is granted in the pre-pass only if guard G leaves the reference (§3.4)
   its room. **Sub-reservations** are granted on fit, uncounted. **A pushed head** grants nothing
   past itself.

Each applies today to a normal job behind a `--priority high` head; D6 extends that contract.

**Wedge check.** No waiter is ever refused by a tier, so the rules that can keep a fitting waiter
waiting are today's (budget, pushed head, restart freeze, exclusivity gate). A nested `aira
confine` child C of a running MEDIUM job R (`cmd/aira/confine_nested.go:9-14`) behind a HIGH head
H is an ordinary waiter: granted within H's budget, else in the next backstop pass, so R finishes
and H fits. The only strict cases are today's operator choices (a pushed head; `maxHold` disabled
with H's budget spent), which `--priority high` can already put in front of C. A child of a LOW
job is LOW (its parent's owner): it is not wedged by a rule, but while higher-tier waiters keep
arriving and fitting it is considered last and can wait indefinitely, its parent holding its
reservation throughout (D3, D5; stated in the summary; R2).

### 3.4 The small lane (D4 amended)

`smallLanePassLocked` (`internal/daemon/small_lane.go`) keeps today's single order:
`order := admitOrderLocked(queue, tiers)` for both its candidates and the reference waiter guard
G protects, with today's `if index == reference { reference = nextRef(index + 1) }`. The only
change is the `tiers` argument. With an empty table the lane is today's, byte for byte (T1 on the
order; `TestSessionTiersLaneNoMarksIsTodays` on the grants).

**Why not the decided arrival-order candidates (D4 as decided).** G judges a candidate against
ONE waiter, the reference, which is sound only while every waiter ahead of the candidate in the
walk's order has been visited. With candidates in a second order an older small job is judged
against a newer small reference, the lane grants both, and the large waiter behind them loses the
room G kept for it. Counterexample with **no marks** (build review, probe on master `7905c619`
passes, on `595c94b3` fails): 32 GiB, zero headroom, lane cap 2 GiB; enqueue M 31.5 GiB, L 0.5,
H 0.5 `--priority high`. Today: H lane-granted, L kept, M granted (32). Arrival order: L judged
against H only, L and H lane-granted, M refused (32.5 > 32). With marks the same path lets an
older LOW small job squeeze out a MEDIUM or HIGH large job. It is a property of two orders in one
pass, not of the cursor (an exact arrival-order G still leaves M refused, because the walk grants
H ahead of M), so it was closed by deleting the second order (`laneCandidatesLocked`, the cursor
rewrite, T6b, T6c, T7c and L5) rather than by more guard machinery.

**What the owner gives up** (veto restores D4 as decided, with this consequence stated): a LOW
session's small job no longer gets the last lane room before a HIGH or MEDIUM one, and a
`--priority high` small job keeps today's place in the lane. A small job of any tier still passes
large queued jobs through the lane, subject to G.

### 3.5 The verb and the wire

A new project-less confine management verb `confine-session-tier` (SafetyExecute, like its
siblings): args `session` (target owner), `tier` (a descriptor enum `high|medium|low`, which
`aira top`'s picker reads) and the siblings' `owner` (caller). CLI: `aira confine-session-tier
<owner> <high|medium|low>`; reply `{session, tier, previous}`. No `slice`: tiers are machine-wide,
and an accepted-and-ignored `--slice` is what AIRA-82/196 refuse. So the daemon **refuses a
`slice` key** (any value) with `E_CONFINE_ARGUMENT_INVALID`, the CLI skips the slice insertion
`dispatchConfineManagementRequest` makes for every sibling (`main.go:2386-2387`), and `aira top`
sends no `slice` (unlike `tui_inline.go:134`); a missed skip is a loud refusal in T19a.

Every allowlist site, and the test that turns red when its entry is removed. T19a runs the CLI
from outside any project against a real `daemon.NewServer` on a temp socket
(`startCommandDaemon`, as `confine_history_e2e_test.go:51`), never a stub, and asserts the
published map and the store hold the mark.

| Site | Change | Red test |
|---|---|---|
| `internal/daemon/server.go:939` route | add the verb | T19a |
| `confine_manage.go:153` `confineManagement` | branch after the caller-owner check, before slice resolution; refuses a `slice` key | T19a; T19c (a `slice` key, empty or not, refused) |
| `cmd/aira/main.go:381` verb form; `:2769` `buildRequest` (positionals `session`, `tier`, the daemon's validator) | add | T19a, T18 |
| `main.go:880` option allowlist `{"owner": true}` | add | T19a passes `--owner`; T19c: `--slice` refused |
| `main.go:2386` slice insertion | skip for this verb | T19a (the daemon refuses the key); T19c asserts the sent map has no `slice` |
| `main.go:1613` `confineHelpVerbs` | add | T19d: `aira confine --help` lists the verb |
| `dispatcher.go:162` `Dispatch` | add | T19a |
| `dispatcher.go:223` hint condition; `:763` hint text | add; the text names `aira confine-session-tier` | T25 |
| `dispatcher.go:523` `DispatchPalette` | add | T19b |
| `internal/core/routing.go:51`, `:135`; `cmd/aira/scope_dir.go:154` | add | T19e: `Classify` gives `RouteClient`, `StoreFreeCarved` true, `--scope-dir` refused |
| `internal/core/core.go`: descriptor beside `confine-front` (`:2203`) with no `MCPTool`; metadata beside `:2620`; the name added to the CLI-only exclusion derivation (`:2719-2724`, the `drain`/`install` precedent, `:2026-2033`) | in generated help and the agent guide; never `Include`, so never a Skill action or MCP tool (L6) | T21 |

`main.go:2369` is NOT touched (flag spelling cut). Dispatch-table summary: "Set a session's
admission order tier (queue order only). Set by the owner from `aira top`; agents do not set tiers."

**Error codes.** `E_CONFINE_ARGUMENT_INVALID` (exit 2: bad `session`, `tier` or a `slice` key),
`E_CONFINE_UNAVAILABLE` (exit 4: an unevaluated table, ci-shim or a failed load, in one message;
a store write failure), and one new code, its catalogue entry and producer landing together
(`produced_test.go`, both ways): `E_CONFINE_SESSION_TIERS_FULL`, exit 1 (a state condition, like
`E_CONFINE_NOT_QUEUED`, `internal/codes/codes.go:381`).

**List reply.** `confine-list` gains one field, `session_tiers` (`*[]{owner, tier}`, sorted by
owner, `omitempty`): absent when the pointer is nil or the daemon is old; `[]` when loaded and
empty. The case loads the pointer once at its start (§3.1) and sets the field on every
`ConfineListResult` it returns, the `UNEVALUATED` early return included
(`confine_manage.go:165-185`), from the `Server` table, never the queue snapshot, so a mark with
no waiters or no live queue is listed. The `--list` renderer prints its line on both paths, the
unevaluated early return (`main.go:3655`) included (T17c); `aira top`'s (`tui_top.go:657`) draws
no rows and needs no tier state.

**Protocol version stays 15** (`internal/daemon/protocol.go:218`, pinned by `protocol_test.go:128`
and `protocol_pin_linux_test.go`): no admit, re-declare or store-op frame changes; a comment entry
in the AIRA-340 style. Skew, measured by the builder against the live pre-381 daemon: new client,
old daemon: the unknown verb falls through to project dispatch (`server.go:1040`), outside
`E_CONFINE_*`, so `withOldDaemonManagementHint` keeps the daemon's code and message and appends
"the running aira daemon may predate `aira confine-session-tier`; restart it" (T25); old client,
new daemon: the old decoder is not `DisallowUnknownFields` on this reply (the builder confirms).

### 3.6 `aira top` and `confine --list`

- **SESSION cell** (`topSessionCell`, `cmd/aira/tui_top.go:1237`) of every row gains ` (high)` or
  ` (low)` when the owner parsed from its scope id is marked. The suffix marks the **session**,
  not the row's order: a suite worker's scope id carries its parent's owner
  (`internal/daemon/worker_admit.go:446`) and its queued row is listed (`admit.go:1434`), so a LOW
  suite's worker row shows ` (low)` while ordered MEDIUM (L2). The LIVE cell keeps `pushed`,
  `high` (job priority) and the AIRA-370 reason. A mark with no running or queued row is not shown.
- **The `t` key** (`viewTop` key switch, `cmd/aira/tui_inline.go:106`) is an inline action with
  `ValueSource: inlineValueEnum` on `confine-session-tier.tier` and one new field `AnyRow`, set on
  `t` alone: the row-kind gate (`:338`) becomes `!action.AnyRow && row.Queued != action.QueuedRow`,
  so `t` reaches running and queued rows and every other action's gating is unchanged. The owner
  is parsed from the row id (`runner.ParseConfineScopeID`, the daemon's key, I6; no new row
  field); a failed parse, empty or `unknown` owner answers "this job has no session owner to
  mark", sending nothing. Otherwise the existing enum picker (`:384-393`) offers `high`, `medium`,
  `low`; the pick branches in `onInlineActionPick` (as `confine-resize` does from the mini-form)
  to `topTierConfirm`, one `paletteConfirm` request like `topFrontConfirm` (`:129`): `session`
  (the parsed full owner, never the cell text), `tier`, `owner` = `state.ConfineOwner`, no
  `slice`; page text "Set session `field` to LOW. Changes the order of its waiting and future jobs
  only; running jobs are not affected. Kept until changed." The result line shows
  `session field: medium -> low` or the error.
- **`aira top` footer:** when the reply has no `session_tiers`, `tiers unevaluated` is the
  footer's first part, ahead of `box CPU busy` and the granted clause (`tui_top.go:1737-1740`:
  the footer is one unwrapped line and later parts are clipped at 80 columns), on BOTH footer
  paths, the early return for a reply with no slice reserve included; otherwise the footer gains
  nothing. The full mark list lives only in `confine --list`.
- **`confine --list`**, under the slice summary, with FULL owner strings (never the SESSION cell's
  shortened form, so what is printed is what `aira confine-session-tier` takes):
  `session tiers: high field; low cc-1a2b3c4d-0000-4000-8000-000000000001`,
  `session tiers: none set`, or `session tiers: unevaluated (old daemon, ci-shim, or stored marks
  did not load: see the daemon journal)`. `--json` carries the field as sent.

## 4. Invariants

- I1 Order: §3.2's seven buckets; an empty table gives today's order element by element (T1);
  all-HIGH: pushed, owned job-high, owned normal, untiered job-high, untiered normal; all-LOW:
  pushed, untiered job-high, untiered normal, owned job-high, owned normal (T2).
- I2 Order only: each pass makes exactly the grants, `bypassed`, anchor, phase and diagnosis
  writes today's evaluator makes walking the tier order (T10, T29, T30).
- I3 No per-waiter state; a mark reorders already-queued waiters on the next pass (T8).
- I4 One load per reader, by structure (§3.1, three `Load` sites); `session_tiers` is on every
  `ConfineListResult` the case returns and `confine --list` prints it on both paths (T4, T17c).
- I5 Lane: candidates and reference in the one evaluation order, as today; empty table: today's
  grants (T6, T7, T7b, `TestSessionTiersLaneNoMarksIsTodays`).
- I6 Identity: a waiter's tier key, the owner `t` sends and the owner whose mark the SESSION cell
  shows are one value, the owner parsed from the scope id (T24, T19b).
- I7 Writes: a set changes only the table and the store; every acknowledged set is in the
  published map and the store equals it; nothing is published before the store write commits
  (T15, T12c; a set takes no `queue.mu` and touches no ledger or head state by construction:
  `setSessionTier` reads only `s.tiers` and the store).
- I8 Persistence: after a restart the table equals the store; a failed load gives a nil table
  (ordered as empty, reported unevaluated), a running daemon and refused sets, with the stored
  rows untouched, until a restart (T12, T12b).
- I9 Honesty: absent `session_tiers` renders unevaluated, visibly at 80 columns (T17b).

## 5. Tests: the failure each kills

"False order": a job goes ahead when it should not; "false starve": a job is withheld when it
should not be. The builder runs each named mutation and records it red. Generated queues (T1, T2,
T29) contain pushed, job-high and normal owned, sub-reservation, scope-less and ownerless waiters.

| # | Dir | Wrong implementation (mutation) | Test |
|---|---|---|---|
| T1 | order | absent row defaults to LOW; or job-high bucket dropped | random queues, empty table: equal element by element to an oracle copy of today's `pushed, high, rest` function |
| T2 | order | untiered waiters tiered by the table | the I1 all-HIGH and all-LOW orders written out per bucket |
| T3 | order | tier and job-priority keys swapped | {H,M,L} x {job-high, normal} x {pushed, not}: full order asserted; LOW job-high after MEDIUM normal |
| T4 | order | snapshot ordered without the table | lane off, equal sizes, room for one at a time, mixed tiers: pass-then-release grants follow the snapshot's position order exactly |
| T5 | order | tier ranked above push; pushes not latest-first | pushed LOW A then B behind waiting HIGH jobs: grant order B, A, then the HIGH jobs |
| T8 | order | tier copied at enqueue | enqueue MEDIUM, then mark LOW: next pass reorders; remove: order restored |
| T9 | starve | sub-reservation tiered by its parent | parent owner LOW; its sub-reservation (older) then a MEDIUM job-level waiter, room for one: the sub-reservation is granted, as with an empty table |
| T24 | order | key read from `waiter.owner` | scope id without owner tail, request owner `field`, `field` HIGH: the waiter is MEDIUM and its row shows unowned |
| T10 | both | no tier order (FIFO); zero-budget tier arm (false starve); budget skipped for lower tiers (false order) | lane off, 14 of 32 GiB running, no pin discount; HIGH head H 20 GiB refused; behind it, enqueued in this order, fitting LOW L1 4, LOW L2 4, MEDIUM M 4 GiB: M and L1 granted (8 <= 10), L2 withheld (12 > 10), anchor armed, phase `hold`; next backstop pass grants L2. FIFO grants L1, L2 and withholds M; a zero-budget arm withholds L1; no budget grants L2 |
| T29 | both | any tier-aware condition in the head switch | production is its own oracle: random queues, tables and passes, lane off; run A with the table, run B with the same waiters enqueued in A's tier order, an empty table and `high` cleared (pushes kept): per pass identical grants, `bypassed`, anchor and phase |
| T13 | starve | tier used as a gate | LOW job alone on an empty slice: granted in the first pass |
| T30 | starve | a tier hold of any kind | running MEDIUM R (14 of 32 GiB); HIGH head H 20 GiB; R's nested 2 GiB child C, lane off: C granted within H's budget; R released; H granted |
| T6 | order | lane candidates in arrival order (D4 as decided) | lane room for one: older LOW and newer HIGH small jobs: HIGH lane-granted (`smallLaneGrantObserver`); LOW `laneWait` is `full` |
| T6n | order | lane candidates in arrival order | NO marks, zero headroom, 32 GiB, lane cap 2 GiB; enqueue M 31.5, L 0.5, H 0.5 `--priority high`: lane grants H only (observer); L `kept`; M granted by the walk (`TestSessionTiersLaneNoMarksIsTodays`) |
| T7 | starve | lane ordered without the table | 32 GiB, 26 granted, no small leases; enqueue MEDIUM 8, HIGH 31.5, LOW 1 GiB small: the reference is HIGH (31.5 + 1 > 32), so the small job's `laneWait` is `kept` and the observer never sees it; an untiered lane order (reference MEDIUM, 8 + 1 <= 32) lane-grants it |
| T7b | false order | lane candidates in arrival order | T6n's box with marks: MEDIUM M 31.5, LOW L 0.5, HIGH H 0.5 (evaluation H, M, L): lane grants H only; L `kept`; M granted by the walk. Mutation: L judged against H alone, L and H lane-granted, M refused (32.5 > 32) |
| T12 | order | marks not loaded | set `field` HIGH, restart the Server on the same state dir with `field`'s survivor re-declaring and a new `field` waiter queued behind MEDIUM ones: the first pass orders it HIGH; `confine --list` reads `high field`; ARDR lease unchanged |
| T12b | starve | load failure refuses start, reported "none set", or a set accepted on an unloaded table | injected failing load: daemon serves; pointer nil, order as empty; reply has no `session_tiers`; `--list` reads unevaluated, the footer `tiers unevaluated`; journal names the cause and the manual sqlite remedy; a set is refused `E_CONFINE_UNAVAILABLE` exit 4 (the one nil-table refusal) and the stored rows are unchanged |
| T12c | order | table published before the store write | injected failing write: set refused `E_CONFINE_UNAVAILABLE`, published table and store both unchanged; with the writer paused inside the store write, a concurrent `confine-list` sees the old map |
| T15 | order | `tiersMu` dropped | on a served Server, the paused-writer technique of T12c (no timing over `-count`): while one set is paused inside its store write a second set does not complete; resumed, both are in the final map and the store; 63 rows plus 2 concurrent new owners: exactly one refused `E_CONFINE_SESSION_TIERS_FULL` |
| T17 | order | empty list dropped or sent as null | no marks: `session_tiers` is `[]` |
| T17b | order | absent rendered "none set", or the `aira top` state placed where it is clipped | a reply without `session_tiers`: `--list` prints `session tiers: unevaluated`; `aira top` drawn on a simulation screen at 80 and 120 columns (as `tui_top_diagnosis_test.go:128-139`) shows `tiers unevaluated` on screen, on both footer paths (with a slice reserve, and the early return for a reply with none) |
| T17c | order | `session_tiers` filled from the snapshot, only on the OK path, or dropped by the `--list` early return | through a real Server on a socket: a mark with no waiters and no live queue is listed; on the `UNEVALUATED` return the field is present and the rendered `--list` text (`main.go:3655` path) carries `session tiers: high field`; a `cc-<uuid>` mark is printed with its FULL owner string |
| T18 | order | `unknown`, empty, over-long or bad-tier values accepted | each refused `E_CONFINE_ARGUMENT_INVALID`, exit 2, by client and daemon |
| T19 | order | verb missing at an allowlist site (§3.5 table); `t` sends the cell text, reaches one row kind, or widens another action's gate; `slice` sent or ignored | (a) `aira confine-session-tier field low --owner me` from outside any project, against a real Server on a socket: OK, map and store hold `field: low`; (b) through the `aira top` model and `DispatchPalette` to the same real Server: `t` then a pick on a running row and on a queued row each sets the row's full owner (rows showing a tmux name or a shortened `cc-` id included), the sent map has no `slice`; an ownerless row sends nothing; `f` on a running row and `e` on a queued row are still refused `E_TUI_INLINE_ROW_KIND`, and `f`, `e`, `KK` on their own row kinds send exactly today's requests; (c) daemon refuses a `slice` key (empty and non-empty), CLI refuses `--slice`, the CLI's sent map has no `slice`; (d) `aira confine --help` lists the verb; (e) `Classify`, `StoreFreeCarved`, `--scope-dir` refusal |
| T20 | order | cap not enforced, or enforced on re-set/remove | 64 rows: a new owner refused `E_CONFINE_SESSION_TIERS_FULL` exit 1, nothing changed; re-set and remove succeed |
| T21 | order | the setter reaches MCP or the Skill | the descriptor has `Include` false and an empty `MCPTool`; the generated MCP tool list and Skill actions lack it; generated help and the agent guide list it with "agents do not set tiers" |
| T22 | order | `(high)` shown for job priority | queued `--priority high` row of a medium session: SESSION cell unmarked, LIVE cell `high` |
| T25 | order | old daemon's refusal claimed as success, its code replaced, or no hint | stub old daemon (the one stub use) answering with a project-dispatch error: non-zero exit with that code's status, message kept, hint naming `aira confine-session-tier` appended |

## 6. Owner lines (accept or veto, one line each)

- **L1 Minimum.** The table consulted at evaluation time; no per-waiter state, no admit-wire
  change, no observe mode. Recommended: accept.
- **L2 Who is untiered.** Suite worker claims, `aira run` jobs and jobs whose scope id has no owner
  are always MEDIUM. So marking every owner HIGH is not the same as no marks. Recommended: accept.
- **L3, L9** are decided (D7, D2): marks persist (load failure: §3.1); a push beats a tier, with
  no `--steal` for one's own job.
- **L5 (reopened at build review).** D4 amended: the lane keeps today's single order, tier-aware
  (§3.4). Recommended: accept; a veto restores arrival order with its stated consequence.
- **L4 `@cwd-` keys.** Allowed (covers every unattributed launch from that directory). Accept.
- **L6 Trust and surface.** CLI and `aira top` only; no MCP tool or Skill action; no ownership
  guard (marking other sessions is the feature); every set audit-logged with its setter in the
  daemon journal. Any agent on this UID can still run the CLI, as it can pass `--priority high`
  today; the help and agent guide say agents never set tiers. Recommended: accept.
- **L7 Names.** Verb `confine-session-tier` (bare "tier" is taken by review tiers), key `t`. Accept.

## 7. Deferrals, each with the trigger that would justify it

| Deferred | Trigger |
|---|---|
| R1 cpu.weight or other runtime effect by tier | a measured case where a LOW session's running job materially slows a HIGH session's job |
| R2 age cap or promotion for LOW | the owner reports a LOW job waiting longer than wanted, including a LOW job's nested child waiting while its parent holds memory |
| R3 a hold for lower tiers (L8 options strict / zero budget) | the owner finds lower-tier jobs starting past stuck higher-tier jobs a real cost |
| R4 tier by project or ticket | the owner keeps marking every session of one project the same way |
| R6 an MCP setter | the owner often asks an agent to set tiers |
| R7 tiers for `aira run` | `aira run` jobs seen queueing (needs an owner on its admit request) |
| R8 a tier following a conversation across `/clear` or `/resume` | re-marking found a real cost (needs a stable session name) |
| R9 automatic prune of marks with no jobs | the 64 cap is reached |

## 8. Release and rollout

- Milestone v0.47. Client install AND `systemctl --user restart aira-daemon.service`, after
  `aira confine --list` and a heads-up. Unmarked sessions: order, lane and head rule unchanged
  (T1, T29, T6n).
- Peer heads-up at the tagged release (deploy, speed): "The owner can mark sessions high or low in
  `aira top`; a mark only reorders waiting jobs, like `--priority high`, the small-job lane
  included; marks survive daemon restarts; agents must not set tiers. With no marks nothing
  changes."

## Review log

Rounds 1-3 (9, 12, 6 blockers) built, then re-based, a hold for lower tiers; it went to the owner
as L8. Round 4 applied the answer (order only, D6) and deleted the hold, the `--session-tier`
flag, the second page and `slice` on the verb. Round 5 re-modelled the lane cursor (T7b, T7c),
kept push in the lane candidates (T6c) and made the nil pointer the unevaluated state.

### Round 6 (final cut): 6 findings and 7 actionable nits, all verified against the code; closed by deletion

Five blocking rounds, none against the admission-order core (Fable confirmed every cited line);
every blocker sat in the surface around it, so each family is closed by deleting surface.
- **Idle-mark rows deleted** (a third row kind in a two-kind gate, `tui_inline.go:338`), with
  their gating, T31 and "listed": such a mark is seen in `confine --list`, removed by the CLI. `t`
  is an `AnyRow` enum-picker action keyed on the row id's owner; T19b covers both kinds and f/e/KK.
- **Footer list deleted** (clipped at 80 columns, `tui_top.go:1737-1740`): `aira top` shows only
  `tiers unevaluated`, first; T17b asserts it on screen.
- **Failed-load recovery deleted** (the next set deleted unread rows): sets refused
  `E_CONFINE_UNAVAILABLE` until a restart; T12b re-pointed.
- **Dispatch row** (P1: `Include: true` with no `MCPTool` fails `skill_test.go:463-468`): the verb
  joins the CLI-only exclusion derivation; T21 asserts `Include` false.
- **Stated, no design change:** a LOW job's nested child is LOW and its parent holds memory
  meanwhile (summary, §3.3, R2).
- **Nits:** reopened L3/L5/L9 alternatives deleted; SESSION identity, worker rows (I6), the
  nil-passing aggregate snapshot stated; T29 uses production as its oracle; T6/T6b/T7 assert
  `laneWait` or the observer; T12c pauses the writer; T12's ordering and T17c's shim case cut.

### Build (AIRA-381): P3 nits folded in

- `confine --list` prints FULL owner strings, never the SESSION cell's shortened form (§3.6); a
  `cc-<uuid>` mark is in the list tests (T17c).
- `tiers unevaluated` is also on the footer's early return for a reply with no slice reserve
  (§3.6); T17b covers both paths.
- Simplified: `E_CONFINE_SESSION_TIERS_UNLOADED` and the separate ci-shim branch deleted; a nil
  table is one refusal, `E_CONFINE_UNAVAILABLE`, naming both causes (§3.1, §3.5). T14 dropped: a
  set takes no `queue.mu` and reads nothing but `s.tiers` and the store, so it cannot touch ledger
  or head state (I7).
- T15 uses T12c's paused-writer technique rather than goroutines over `-count`.
- The failed-load journal line names the manual sqlite remedy; the accepted gap of a commit that
  lands after its timeout fired is stated (§3.1).
- Unchanged by the build and stated for the reviewer: the `aira top` key legend does not gain
  `t tier` (TestTopKillLegendFitsOrdinaryWidths pins the 80-column legend, which is full); the key
  is named in the generated agent guide's session-tiers paragraph instead.

### Build review round 1: 2 findings, both fixed

- **Lane in a second order changed unmarked grants** (P2, probe above, §3.4): closed by a design
  change that deletes code: `laneCandidatesLocked`, the cursor rewrite, T6b, T6c, T7c and L5.
  D4 amended for the owner to confirm (§2, §6 L5). T6 inverted, T7b re-pointed at the
  counterexample with marks, T6n (`TestSessionTiersLaneNoMarksIsTodays`) pins the unmarked one;
  each fails against arrival-order candidates (run), T6, T7 and T7b against an untiered lane order.
- **Generated guidance** said a `high` job goes ahead of every `normal` job, and that the lane
  ignores tiers: `ConfinePriorityGuidance` now says job priority is a sub-level inside a session
  tier, the session-tiers paragraph that the lane offers its room in the same order;
  `TestConfinePriorityGuidanceIsOnEveryFace` checks both in the skill and the agent guide.
