# Session priority tiers: high / medium / low sessions in admission order (AIRA-381)

Status: plan, round 4, for plan review (Sol plus one Claude lineage) and the Fable plan gate; the
owner settled the hold question (order only, D6) and this revision deletes the hold. Ticket
AIRA-381, milestone v0.47; correctness-critical (two-loop, Opus builds, Fable and Sol review).
Builds on AIRA-307/309 (`2026-10-13-aira-queue-order-design.md`), AIRA-340 (`--priority high`)
and AIRA-350 (`2026-11-18-fixed-small-lane-design.md`). Code references: master at `dc19915c`.

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

Inside each row, oldest first. With no marks the main queue's order is exactly today's (one
small-lane change, L5). Examples, on a box where memory is short and jobs queue:
- You mark session `field` HIGH. Its next `make ci` is considered before the 15 medium jobs that
  queued earlier, and starts at once if it fits and nothing ahead of it is stuck. If not, it
  becomes the front job the box protects,
  under today's rules: jobs behind it that fit may still start past it up to half its booking in
  total, plus whatever fits in the catch-up pass (every 5 minutes by default; a daemon setting,
  can be disabled), plus small jobs through the lane that leave it room. That is what a
  `--priority high` job gets today.
- You mark a sweep session LOW. Its 4 GiB job is considered after every waiting medium and high
  job. If a medium 10 GiB job at the front is stuck, the LOW job can still start past it within
  that job's 5 GiB allowance, but medium and high jobs behind the stuck job are offered it first.
  On a quiet box the LOW job starts at once: LOW means "behind everyone else waiting", never "only
  when idle". Its `--priority high` hook jobs are considered after every waiting medium job.

What tiers do not do, said plainly:
- **A lower-tier job can still start ahead of a stuck higher-tier job** (the allowance and the
  catch-up pass above). You chose this (order only): no new rule holds a job back, so there is no
  new way for a job to get stuck, and the box stays as busy as today.
- **Small jobs ignore tiers.** Jobs booking 1 GiB or less (default) go through the small-job lane
  in arrival order, whatever the tier or `--priority`. About 59% of long waits are such jobs
  (4,821 of 8,233 waits of 60 s or more in 14 days; reported by the reviewer, column semantics
  unverified); tiers reorder them only when the lane is full and they queue in the main line.
- **Test-suite worker claims are untiered** (L2): a HIGH session's suite is HIGH for its own start
  only; its workers' claims are MEDIUM. A LOW session's running suite gets worker room as today.
  And a push (`--front`) goes first whatever the tier (L9).
- **A mark follows an owner string exactly** (the SESSION cell in `aira top`). Claude sessions use
  `cc-<session id>`, which may change after `/clear` or `/resume` (unverified): check the new
  jobs' cell. `AIRA_CONFINE_OWNER=field` is the predictable key; sessions sharing it share a tier.
- **Marks survive a daemon restart** (recommended, L3: 21 restarts in the 14 days to 2026-11-27,
  mostly cutovers, as reported by the coordinator) until set back to MEDIUM. At most 64; a 65th is
  refused. Finished sessions' marks stay as dim rows in `aira top` until removed with `t`.

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

Result 2026-11-27 (data starts 2026-11-15): 31,167 runs; 8,224 (26%) waited 60 s or more; 3,379
(11%) 5 min or more; 413 waited 30 min or more; longest 7,457 s. Verdict: **build**. Caveat: about
59% of the long waits are lane-class small jobs (reported by the reviewer, see the summary), so the
reachable share is roughly 11% of runs, still an order of magnitude over the threshold.

**Greenfield minimum.** A daemon table `owner -> high|low` (medium is the absence of a row),
consulted by the ONE ordering function at evaluation time; one setter verb; the `t` key in
`aira top`; the tier visible on rows. Nothing stored per waiter, so a mark applies to
already-queued jobs on the next evaluator pass. That is this design. Additions, each justified:

| Addition | Why it stays |
|---|---|
| Marks stored in `state.db` (L3) | Without it every mark is silently wiped about every 16 hours (21 restarts in 14 days). One table in a database the daemon already opens; §3.1. |
| 64-row cap, refusal only | With stored marks and no automatic prune, the only bound on the table, the reply and the idle rows. |
| Idle-mark rows in `aira top` | A stored mark whose owner has no listed job could otherwise not be seen or removed from `aira top`. |
| A writer mutex | Two concurrent setters would otherwise lose an acknowledged mark. |

Cut: any hold or budget change (D6); the `confine --session-tier` flag (the verb form is the CLI;
tests need no more); a second confirmation page (the h/m/l prompt is the confirmation; a tier is
reversible and touches no running job); `set_at`/`set_by` metadata (the journal's audit line
records the setter); `jobs_now` (the idle row shows a mark with no listed jobs); a `queued_tier`
field and wait-line text (a row's tier is its owner's entry in the same reply); a per-waiter tier;
a tier on the admit request; an observe-only mode (an empty table is today, T1); an MCP setter
(L6); automatic pruning (R9).

## 2. Decided by the owner (2026-11-27), recorded, not reopened

- **D1 Identity.** Keyed on the owner string `aira confine` puts in the scope id
  (`resolveConfineOwner`, `cmd/aira/main.go:2291`: `--owner`, `AIRA_CONFINE_OWNER`,
  `cc-<CLAUDE_CODE_SESSION_ID>`, worktree id, inferred `@cwd-<dir>`); subagents share it.
- **D2 Composition.** Pushed first (latest first); then tier HIGH, MEDIUM, LOW; inside a tier
  `--priority high` before normal; FIFO inside each bucket.
- **D3** LOW waits (no age cap, no promotion). **D4** The lane ignores tiers and job priorities.
  **D5** Running jobs are unaffected (no preemption, reservation or cpu.weight change).
- **D6 Order only** (owner line L8, option A). Tiers change only `admitOrderLocked`, exactly as
  `--priority high` does (AIRA-340: "queue order only, never the head rule"). No zero-budget rule,
  no strict hold, no new anchor, phase or diagnosis token; the head rule, the AIRA-307 budget and
  the backstop are unchanged.

## 3. Design

### 3.1 The table and its storage

`Server` holds `tiers atomic.Pointer[map[string]sessionTier]` (`sessionTier` is `high` or `low`;
no `medium` row exists) and `tiersMu sync.Mutex`. The map is immutable once published. Every
reader loads the pointer once, lock-free: an evaluator pass once, passing it to the lane pre-pass
and the walk (so the lane's reference and the walk's head come from one order); a list reply once.

**Set.** Under `tiersMu`, for the whole operation: copy the current map; apply the change (`medium`
deletes the row); if the copy has more than 64 rows, refuse `E_CONFINE_SESSION_TIERS_FULL` (a new
row only; re-set and remove always pass the check); write the copy to the store (below); only then
publish the pointer and write the audit line
`confine-session-tier: setter=<caller> session=<owner> tier=<old>-><new>` (like
`logConfineFront`). A store failure refuses the set with
`E_CONFINE_UNAVAILABLE: store session tiers: <cause>` and publishes nothing, so memory and store
stay equal. The set takes no `queue.mu` (no lock-order edge); its store write has a bounded
context. The new order applies on the next evaluator pass (the existing poll tick).

**Store (L3 recommended).** One table in `state.db`, added to the schema list beside
`confine_peak_history` (`internal/store/store.go:878`):

```sql
CREATE TABLE IF NOT EXISTS confine_session_tiers (
    owner TEXT PRIMARY KEY, tier TEXT NOT NULL CHECK (tier IN ('high','low')))
```

Two store methods: `ConfineSessionTiers(ctx) (map[string]string, error)` and
`ReplaceConfineSessionTiers(ctx, map[string]string) error`, the latter one transaction deleting
every row and inserting the map (at most 64 rows), so after every acknowledged set the store
equals memory by construction, even after a failed load. Why not a file: the daemon already opens
`state.db` at start as its single writer; a file needs a new path, an encoding and a
temp-fsync-rename writer the daemon package does not have.

**Load.** Once in `Serve`, after `OpenDB` (`server.go:493`) and before `net.Listen` (`:514`), so
the first pass and every re-declared survivor see the stored marks. Any failure (query error, an
owner `runner.ValidateConfineOwner` refuses, more than 64 rows) means **no marks**: an empty table,
one log line, and a fixed `tiersWarning` string the list reply carries (§3.5); never a refusal to
start. (A schema failure inside `OpenDB` refuses start today, unchanged.) ci-shim mode neither
loads nor sets. No automatic prune: finished sessions' marks stay, bounded by the cap and shown as
idle rows (§3.6, R9).

**Valid keys.** What `runner.ValidateConfineOwner` accepts, except empty and `unknown` (refused
`E_CONFINE_ARGUMENT_INVALID`); `@cwd-<dir>` accepted (L4); exact match, never by prefix.

### 3.2 The order

**Whose tier.** A waiter's key is the owner embedded in its scope id,
`runner.ParseConfineScopeID(waiter.scopeID)` (`internal/runner/confine.go:1476`), the parse every
row display and the `--front`/kill guards use; never `waiter.owner`, the request's claim, which
`validateAdmitArgs` accepts when the scope id has no owner tail (I6).

**Untiered, always MEDIUM:** a sub-reservation (`isSubReservation()`: aitest workers,
`confine-reserve` holds; a running job's internal progress, D5); a scope-less waiter (`aira run`,
no owner; R7); a scoped waiter with no owner tail.

**The function.** `admitOrderLocked(queue)` (`internal/daemon/admit.go:475`) becomes
`admitOrderLocked(queue, tiers)`: one pass over `queue.waiters` into seven buckets (pushed;
HIGH/job-high; HIGH/normal; MEDIUM/job-high; MEDIUM/normal; LOW/job-high; LOW/normal), pushed
sorted by descending `pushSeq` as today, every other bucket in `queue.waiters` (enqueue) order. With
an empty table the MEDIUM buckets hold every unpushed waiter and the result is today's
`pushed, high, rest` element for element. Its readers each pass one table load: the evaluator walk
(`admit.go:2676`), the snapshot (`admit.go:1405`, which gains a `tiers` argument through
`admitSliceSnapshotFor`) and the lane's reference (§3.4). Nothing else in the evaluator changes.

### 3.3 The accepted consequence, and its bound

The head rule is untouched: the head is still the first visited job-level, ungated waiter refused
on capacity that could fit an empty slice, whatever its tier, and every later job-level waiter is
judged against it by the unchanged switch (`admit.go:2738-2750`). So a lower-tier waiter can be
granted ahead of a blocked higher-tier head. Exactly how much, from the code:

1. **Budget.** In ordinary passes, a fitting job-level waiter behind an unpushed head H is granted
   only while `H.bypassed + charge <= H.charge / 2` (`admitBypassBudget`, `admit.go:508`).
   `bypassed` is a field of H, written only at `admit.go:2757` and never reset, so over H's whole
   wait the job-level grants past it in ordinary passes total at most half H's ledger booking, all
   tiers together. Tiers decide who gets that allowance first: higher tiers are visited first.
2. **Backstop.** The first pass at or after the anchor plus `maxHold` grants every fitting waiter
   behind an unpushed head, then the next withheld fit re-arms (`admit.go:2609-2624`, `:2802`).
   `maxHold` defaults to 5 minutes; `AIRA_DAEMON_ADMIT_FREEZE_MAX_HOLD` sets it (minimum 1 s) or
   disables it, which makes the budget hold unbounded by that setting's definition
   (`paths.go:297-322`).
3. **Lane.** A small job is granted in the pre-pass only if guard G leaves the reference (§3.4)
   its room. **Sub-reservations** are granted on fit, uncounted. **A pushed head** grants nothing
   past itself.

Each applies today to a normal job behind a `--priority high` head; D6 extends that contract.

**Wedge check.** Order-only adds no hold: no waiter is ever refused by a tier, so the set of rules
that can keep a fitting waiter waiting is today's set (budget, pushed head, restart freeze,
exclusivity gate). A nested `aira confine` child C of a running MEDIUM job R (an ordinary job-level
admit, `cmd/aira/confine_nested.go:9-14`) behind a HIGH head H is an ordinary waiter behind a head:
granted within H's budget, else in the next backstop pass, so R can finish and H can then fit. The
only strict cases are today's operator choices: a pushed head, or `maxHold` disabled with H's
budget spent. Reordering can make such a head appear in front of C where FIFO would not have, but
`--priority high` already can, and both are the documented meaning of those choices.

### 3.4 The small lane (D4, L5)

`smallLanePassLocked` (`internal/daemon/small_lane.go:153-219`) uses `admitOrderLocked` today for
two things, which this design separates:

- **Candidate order** (which eligible small job gets lane room first when the lane cap is nearly
  full, the loop at `:182`) becomes plain FIFO: queued waiters in `queue.waiters` order, ignoring
  tier, `--priority high` and push. Ignoring push changes no outcome: a pushed reference already
  closes the lane to every candidate but itself (`:191-195`), and a pushed small job the FIFO loop
  passes before it becomes the reference reaches the main walk, where it is first in line and no
  head precedes it.
- **The reference** (the waiter guard G protects, `nextRef` at `:171-180`) stays in evaluation
  order (tier-aware): G exists to keep the room of the waiter the main walk will grant or make the
  head. The cursor is an index into the EVALUATION order `order := admitOrderLocked(queue, tiers)`,
  never into the candidate list. After a lane grant of waiter w, `if w == order[ref] { ref =
  nextRef(ref + 1) }`, both in evaluation-order coordinates; today's `if index == reference {
  reference = nextRef(index + 1) }` (`:215-216`) compares and advances with the candidate index,
  which is correct only while the two lists are the same list.

Counterexample for the cursor (T7b): zero headroom, 32 GiB ceiling, lane cap at least 1.5 GiB;
enqueue LOW L 0.5, MEDIUM M 31, HIGH H 0.5, LOW S 0.5 GiB. Evaluation order H, M, L, S; candidate
order L, M, H, S. L is lane-granted (H fits beside it). H is lane-granted and is the reference.
Advancing from H's candidate index 2 lands on S and skips M, so S is granted although
31 + 1.5 > 32. Advancing in evaluation order makes M the reference, and S is `kept`.

**The one visible change for sessions that do nothing** (L5): today the lane offers room to
`--priority high` small jobs first; under D4 it offers it in arrival order. Only when the lane cap
is contended does an older ordinary small job get the last lane room before a newer
`--priority high` one, which then goes in the main walk at its job-high place.

### 3.5 The verb and the wire

A new project-less confine management verb `confine-session-tier`, args `session` (target owner),
`tier` (`high|medium|low`) and the siblings' `owner` (caller). No `slice`: tiers are machine-wide,
and an accepted-and-ignored `--slice` is what AIRA-82/196 refuse. CLI:
`aira confine-session-tier <owner> <high|medium|low>`. Reply `{session, tier, previous}`.

It must be added at every allowlist site, each with a test that fails when the entry is removed:
- daemon route: `internal/daemon/server.go:939`, and a branch in `confineManagement`
  (`internal/daemon/confine_manage.go:153`) after the caller-owner check and before the slice
  path resolution;
- CLI: `cmd/aira/main.go:381` (verb form to `buildRequest`), `:880` (option allowlist
  `{"owner": true}`), `:2769` (`buildRequest`: positionals `session` and `tier`, validated by the
  function the daemon uses), `:1613` (`confineHelpVerbs`); `:2369` is NOT touched (the
  `confine --session-tier` flag spelling is cut);
- client transports: `cmd/aira/dispatcher.go:162` (`Dispatch`), `:223` (old-daemon hint),
  `:523` (`DispatchPalette`, for `aira top`), and `withOldDaemonManagementHint` (`:763`);
- routing: `internal/core/routing.go:51` and `:135`, `cmd/aira/scope_dir.go:154`;
- dispatch table: `internal/core/core.go`, beside `confine-front` (`:2203`), `Include: true`, no
  `MCPTool` (L6); summary: "Set a session's admission order tier (queue order only). Set by the
  owner from `aira top`; agents do not set tiers."

ci-shim mode refuses the verb with `E_CONFINE_UNAVAILABLE` (a CI container is one session).

**Error codes.** `E_CONFINE_ARGUMENT_INVALID` (exit 2: bad `session` or `tier`),
`E_CONFINE_UNAVAILABLE` (exit 4: ci-shim, store failure), and one new code
`E_CONFINE_SESSION_TIERS_FULL`, exit 1 (a state condition, like `E_CONFINE_NOT_QUEUED`,
`internal/codes/codes.go:381`). It needs its catalogue entry in `codes.go`;
`internal/codes/produced_test.go` checks both directions (produced but uncatalogued, catalogued but
unproduced), so the entry and its producer land together.

**List reply.** `confine-list` gains `session_tiers` (a list of `{owner, tier}`, sorted by owner)
and `session_tiers_warning` (string, `omitempty`; the load-failure text of §3.1). The
`confine-list` case loads the table pointer ONCE at its start, before the shim/live branch, and
sets both fields on the result before every return, the `UNEVALUATED` early return included
(`confine_manage.go:165-185`). It passes the same pointer to `admitSliceSnapshotFor`, so the
rows' order and the reported tiers come from one table. The fields are filled from the `Server`
table, never from the queue snapshot: a mark with no waiters, or on a box with no live queue
(`snapshot.present` false), is still listed. `session_tiers` is a non-nil slice and never
`omitempty`, so "none set" is `[]`; the client decodes it into a pointer, and an absent field (an
old daemon) renders `session tiers: unevaluated`, never "none set". Queued rows gain no field.

**Protocol version stays 15** (`internal/daemon/protocol.go:218`, pinned by
`TestProtocolVersionIsPinned`, `protocol_test.go:128`, and the runner pin
`protocol_pin_linux_test.go`): no admit, re-declare or store-op frame changes. `protocol.go` gets a
comment entry in the AIRA-340 style. Skew, measured by the builder against the live pre-381
daemon before claiming it:
- new client, old daemon: the unknown verb falls through to project dispatch
  (`server.go:1040`), whose code is outside `E_CONFINE_*`, so `withOldDaemonManagementHint`
  keeps the daemon's code and message and appends "the running aira daemon may predate
  `aira confine-session-tier`; restart it"; never a claimed success (T25);
- old client, new daemon: the two new list fields are unknown to the old client's decoder, which is
  not `DisallowUnknownFields` on this reply (the builder confirms).

### 3.6 `aira top` and `confine --list`

- **SESSION cell** (`topSessionCell`, `cmd/aira/tui_top.go:1237`) of every row gains ` (high)` or
  ` (low)` for a marked owner; the LIVE cell keeps `pushed`, `high` (job priority) and the
  AIRA-370 reason, so the two kinds of "high" stay in different columns.
- **Idle-mark row.** Each mark whose owner has no LISTED row is drawn as a dim row after the queued
  rows: `session <owner>: tier low, no listed jobs`. "Listed", because queued rows are capped at
  64 (`runner.ConfineQueuedRowLimit`); when the queue is longer the existing "showing N of M" line
  says so. Its only action is `t`.
- **The `t` key** (in the `viewTop` key switch, `cmd/aira/tui_inline.go:106`), on any row with a
  valid owner: one prompt, built like `topFrontConfirm` (`:129`): "Session field (owner `field`)
  is MEDIUM. h: HIGH, m: MEDIUM (removes the mark), l: LOW, Esc: cancel. Changes the order of its
  waiting and future jobs only; running jobs are not affected. Kept until changed." The keypress
  sends; the result line shows `session field: medium -> low` or the error. The request's
  `session` is the row record's full owner (`record.Owner`, from the scope-id tail), never the
  cell text; a row whose owner is empty or `unknown` answers "this job has no session owner to
  mark" and sends nothing. Sent through `DispatchPalette` with the caller as `state.ConfineOwner`.
- **Footer**, and the same line under `confine --list`'s slice summary:
  `session tiers: high field; low cc-1a2b3c4d`, `session tiers: none set`, or
  `session tiers: unevaluated (daemon predates session tiers)`; with a warning, the line ends
  `(stored marks did not load: <cause>)`. `--json` carries both fields.

## 4. Invariants

- I1 Order: the seven-bucket order of §3.2; with an empty table, today's order element by element
  (T1); all-HIGH: pushed, owned job-high, owned normal, untiered job-high, untiered normal; all-LOW:
  pushed, untiered job-high, untiered normal, owned job-high, owned normal (T2).
- I2 Order only: each pass makes exactly the grants, `bypassed`, anchor, phase and diagnosis
  writes today's evaluator makes walking the tier order (T10, T29, T30).
- I3 No per-waiter state; a mark reorders already-queued waiters on the next pass (T8).
- I4 One table per reader call; a list reply's rows and `session_tiers` come from one load, on
  every reply path (T4, T17c).
- I5 Lane: candidates FIFO, blind to tier, job priority and push; reference and cursor in
  evaluation order (T6, T6b, T7, T7b).
- I6 Identity: a waiter's tier key is the owner its row displays (T24).
- I7 Writes: a set changes only the table and the store; every acknowledged set is in the
  published map and the store equals it (T14, T15, T12c).
- I8 Persistence: after a restart the table equals the store; a failed load gives an empty table,
  a warning and a running daemon (T12, T12b).
- I9 Honesty: absent `session_tiers` renders unevaluated; no count comes from capped rows (T17b,
  T31).

## 5. Tests: the failure each kills

"False order": a job goes ahead when it should not; "false starve": a job is withheld when it
should not be. The builder runs each named mutation and records it red. Generated queues (T1, T2,
T29) contain pushed, job-high and normal owned, sub-reservation, scope-less and ownerless waiters.

| # | Dir | Wrong implementation (mutation) | Test |
|---|---|---|---|
| T1 | order | absent row defaults to LOW; or job-high bucket dropped | random queues, empty table: equal element by element to an oracle copy of today's `pushed, high, rest` function |
| T2 | order | untiered waiters tiered by the table | the I1 all-HIGH and all-LOW orders written out per bucket |
| T3 | order | tier and job-priority keys swapped | {H,M,L} x {job-high, normal} x {pushed, not}: full order asserted; LOW job-high after MEDIUM normal |
| T4 | order | snapshot ordered without the table, or with a second load | lane off, equal sizes, room for one at a time, mixed tiers: pass-then-release grants follow the snapshot's position order exactly; reply `session_tiers` is the table that order used (a set injected between two loads would split them) |
| T5 | order | tier ranked above push; pushes not latest-first | pushed LOW A then B behind waiting HIGH jobs: grant order B, A, then the HIGH jobs |
| T8 | order | tier copied at enqueue | enqueue MEDIUM, then mark LOW: next pass reorders; remove: order restored |
| T9 | starve | sub-reservation tiered by its parent | parent owner LOW; its sub-reservation (older) then a MEDIUM job-level waiter, room for one: the sub-reservation is granted, as with an empty table |
| T24 | order | key read from `waiter.owner` | scope id without owner tail, request owner `field`, `field` HIGH: the waiter is MEDIUM and its row shows unowned |
| T10 | both | zero-budget tier arm added (false starve) or budget skipped for lower tiers (false order) | lane off, 14 of 32 GiB running, no pin discount; HIGH head H 20 GiB refused; behind it fitting MEDIUM 4, LOW 4, LOW 4 GiB: MEDIUM and the first LOW granted (8 <= 10), the second LOW withheld (12 > 10), anchor armed, phase `hold`; next backstop pass grants it. With a zero-budget arm the first LOW is withheld; without the budget the second is granted |
| T29 | both | any tier-aware condition in the head switch | random queues, random tables, random passes: per pass grants, `bypassed`, anchor, phase, `heldByHead` and `blocked` identical to a reference evaluator holding today's walk verbatim, fed `admitOrderLocked(queue, tiers)` |
| T13 | starve | tier used as a gate | LOW job alone on an empty slice: granted in the first pass |
| T30 | starve | a tier hold of any kind | running MEDIUM R (14 of 32 GiB); HIGH head H 20 GiB; R's nested 2 GiB child C, lane off: C granted within H's budget; R released; H granted |
| T6 | order | lane candidates in evaluation order | lane room for one: older LOW small job lane-granted, newer HIGH small job not (asserted via `smallLaneGrantObserver`) |
| T6b | order | lane candidates keep job-high first | lane room for one: older normal small job, newer `--priority high` small job, same session: the older is lane-granted (observer); the newer is not lane-granted in that pass |
| T7 | starve | lane reference taken from the candidate (FIFO) order | 32 GiB, 26 granted, no small leases; enqueue MEDIUM 8, HIGH 31.5, LOW 1 GiB small: the reference is HIGH (31.5 + 1 > 32), so the small job is `kept`; a FIFO reference (MEDIUM, 8 + 1 <= 32) would grant it |
| T7b | false order | cursor advanced by candidate index (`nextRef(index + 1)`) | §3.4 counterexample: L and H lane-granted, S `kept`, observer's reference for S is M; with the mutation S is granted |
| T12 | order | marks not loaded, or loaded after the first pass | set `field` HIGH, restart the Server on the same state dir with `field`'s survivor re-declaring and a new `field` waiter queued behind MEDIUM ones: the first pass orders it HIGH; footer lists `high field`; ARDR lease unchanged |
| T12b | starve | load failure refuses start, or shows "none set" without the warning | injected failing load: daemon serves; table empty; reply carries the warning; footer ends `(stored marks did not load: ...)`; a later set rewrites the store whole |
| T12c | order | table published before the store write | injected failing write: set refused `E_CONFINE_UNAVAILABLE`, published table and store both unchanged |
| T14 | order | a set touches ledger or head state | ledger, `bypassed`, anchor, exclusive gate identical before and after a set with no pass between |
| T15 | order | `tiersMu` dropped | N goroutines setting distinct owners, `-race -count=20`: every acknowledged set in the final map and the store; 63 rows plus 2 concurrent new owners: exactly one refused `E_CONFINE_SESSION_TIERS_FULL` |
| T17 | order | empty list dropped or sent as null | no marks: `session_tiers` is `[]` |
| T17b | order | absent rendered "none set" | `--list` and the `aira top` footer, fed a reply without `session_tiers`: `session tiers: unevaluated` |
| T17c | order | `session_tiers` filled from the snapshot, or only on the OK path | through the real Server connection path (socket, `confineManagement`): a mark with no waiters and no live queue is listed; also on the shim path and the `UNEVALUATED` return |
| T18 | order | `unknown`, empty, over-long or bad-tier values accepted | each refused `E_CONFINE_ARGUMENT_INVALID`, exit 2, by client and daemon |
| T19 | order | verb missing at any allowlist site; `t` sends the cell text | (a) `aira confine-session-tier field low` from outside any project reaches a stub daemon's handler; (b) the `t` key through the `aira top` model and `DispatchPalette` reaches it; rows showing a tmux name or a shortened `cc-` id send the full owner; an ownerless row sends nothing. Removing the entry at any §3.5 site turns a half red |
| T20 | order | cap not enforced, or enforced on re-set/remove | 64 rows: a new owner refused `E_CONFINE_SESSION_TIERS_FULL` exit 1, nothing changed; re-set and remove succeed |
| T21 | order | MCP exposes the setter | generated MCP tool list has no session-tier tool; help and agent guide carry "agents do not set tiers" |
| T22 | order | `(high)` shown for job priority | queued `--priority high` row of a medium session: SESSION cell unmarked, LIVE cell `high` |
| T25 | order | old daemon's refusal claimed as success or its code replaced | stub answering the verb with a project-dispatch error: non-zero exit with that code's status, message kept, hint appended |
| T31 | order | "no jobs" derived from capped rows | 70 queued; a LOW owner whose only jobs are rows 65-70: idle row reads `no listed jobs`; "showing 64 of 70" present |

## 6. Owner lines (accept or veto, one line each)

- **L1 Minimum.** The table consulted at evaluation time; no per-waiter state, no admit-wire
  change, no observe mode. Recommended: accept.
- **L2 Who is untiered.** Suite worker claims, `aira run` jobs and jobs whose scope id has no owner
  are always MEDIUM. So marking every owner HIGH is not the same as no marks. Recommended: accept.
- **L3 Lifetime.** Marks are stored in `state.db` and survive daemon restarts, until set back to
  MEDIUM; at most 64; no automatic prune (finished sessions' marks stay as dim rows until removed);
  a load failure starts the daemon with no marks and a visible warning. Recommended: accept (21
  restarts in 14 days, reported). Alternative: marks in memory only, wiped at every restart like
  `--front` pushes; deletes the table, the two store methods, the load, the warning field and T12b,
  T12c; costs re-marking after each cutover, silently.
- **L4 `@cwd-` keys.** Allowed (covers every unattributed launch from that directory). Accept.
- **L5 Lane order.** Lane candidates plain FIFO, ignoring `--priority high` too: the one change for
  sessions that do nothing (§3.4). Recommended: accept. Alternative: keep job priority in the lane
  order and ignore only tiers.
- **L6 Trust and surface.** CLI and `aira top` only; no MCP tool; no ownership guard (marking
  other sessions is the feature); every set audit-logged with its setter in the daemon journal.
  Any agent on this UID can still run the CLI, as it can pass `--priority high` today; the skill
  text says agents never set tiers. Recommended: accept.
- **L7 Names.** Verb `confine-session-tier` (bare "tier" is taken by review tiers), key `t`. Accept.
- **L9 A push beats a tier.** `--front` goes first whatever the tier, and a session pushing its own
  job needs no `--steal`. Recommended: accept (D2). Alternative: a LOW session's push needs
  `--steal`.

## 7. Deferrals, each with the trigger that would justify it

| Deferred | Trigger |
|---|---|
| R1 cpu.weight or other runtime effect by tier | a measured case where a LOW session's running job materially slows a HIGH session's job |
| R2 age cap or promotion for LOW | the owner reports a LOW job waiting longer than wanted |
| R3 a hold for lower tiers (L8 options strict / zero budget) | the owner finds lower-tier jobs starting past stuck higher-tier jobs a real cost |
| R4 tier by project or ticket | the owner keeps marking every session of one project the same way |
| R6 an MCP setter | the owner often asks an agent to set tiers |
| R7 tiers for `aira run` | `aira run` jobs seen queueing (needs an owner on its admit request) |
| R8 a tier following a conversation across `/clear` or `/resume` | re-marking found a real cost (needs a stable session name) |
| R9 automatic prune of idle marks | the 64 cap is reached, or idle rows crowd `aira top` |

## 8. Release and rollout

- Milestone v0.47. Daemon, schema and client change: client install AND
  `systemctl --user restart aira-daemon.service`, after `aira confine --list` and a heads-up
  (shared daemon-restart care). Unmarked sessions: main-queue order and head rule unchanged (T1,
  T29); the one change is the lane's arrival-order offer (L5).
- Peer heads-up at the tagged release (deploy, speed): "The owner can mark sessions high or low in
  `aira top`; a mark only reorders waiting jobs, like `--priority high`; the small-job lane ignores
  it; marks survive daemon restarts; agents must not set tiers. The lane now takes `--priority
  high` small jobs in plain arrival order."
- Skill text (from the dispatch table): "Session tiers are set by the owner from `aira top`;
  agents do not set them."

## Review log

### Rounds 1-3 (9, 12 and 6 blocking findings)

Round 1 added a strict hold, keyed on the `ParseConfineScopeID` tail, added `tiersMu`. Round 2
found five blockers induced by the strict hold and re-based it onto a zero bypass budget, deleting
persistence. Round 3 (Sol) found 6, the P1 being the lane cursor (§3.4), plus missing allowlist
sites, the list-reply source and unqualified owner claims. A hold-class blocker in every round met
the re-evaluation rule; the root cause was the hold itself, so it went to the owner as L8.

### Round 4 (this revision): owner decisions applied, scope cut

Owner, 2026-11-27: L8 option A, order only (D6); D1-D5 confirmed. L3 stays accept-or-veto,
recommended stored (reported restart count). Deleted: the zero-budget arm and all built on it (hold
invariants, summary and examples, the LOW-waiting analysis, the nested-wedge example, old tests
T10, T11, T12, T23, T27, T28, T30); the `--session-tier` flag and its `=` grammar; the second
confirmation page; `set_at_ms`, `set_by`, `jobs_now`; `slice` on the verb. Added: §3.3, the
accepted consequence with its bound read from the code, and a one-paragraph wedge check; the lane
cursor in evaluation-order coordinates (Sol P1, re-verified at `small_lane.go:171-216`; T7b) and
FIFO candidates (T6b); every allowlist site (`main.go:2369` untouched, flag cut); `session_tiers`
from the Server table on every reply path (T17c); persistence in `state.db` (T12, T12b, T12c);
the catalogue entry for the one new code; protocol 15 checked against `protocol.go:218` and both
pin tests; new T10, T29, T30 pin order-only in both directions.
