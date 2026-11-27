# Session priority tiers: high / medium / low sessions in admission order (AIRA-381)

Status: plan, round 5, for plan review (Sol plus one Claude lineage) and the Fable plan gate; order
only (D6). AIRA-381, v0.47; correctness-critical (two-loop, Opus builds, Fable and Sol review).
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
  queued earlier and starts at once if it fits and nothing ahead is stuck; if not, it becomes the
  front job the box protects under today's rules (jobs behind it may start past it up to half its
  booking, plus the 5-minute catch-up pass, plus small jobs that leave it room), as a
  `--priority high` job does today.
- You mark a sweep session LOW. Its 4 GiB job is considered after every waiting medium and high
  job; past a stuck medium 10 GiB job it can still start within that job's 5 GiB allowance, after
  the medium and high jobs behind it. On a quiet box it starts at once: LOW means "behind everyone
  else waiting", never "only when idle".

What tiers do not do, said plainly:
- **A lower-tier job can still start ahead of a stuck higher-tier job** (the allowance and the
  catch-up pass). You chose this (order only): no new way for a job to get stuck.
- **Small jobs ignore tiers.** Jobs booking 1 GiB or less (default) go through the small-job lane
  in arrival order (a push still first), whatever the tier or `--priority`; about 59% of long
  waits are such jobs (4,821 of 8,233, reported by the reviewer, unverified). Tiers reorder them
  only when the lane is full and they queue in the main line.
- **Test-suite worker claims are untiered** (L2): a suite's workers' claims are MEDIUM whatever its
  session's tier. A push (`--front`) goes first whatever the tier (L9).
- **A mark follows an owner string exactly** (the SESSION cell). `cc-<session id>` may change after
  `/clear` or `/resume` (unverified); `AIRA_CONFINE_OWNER=field` is the predictable key.
- **Marks survive a daemon restart** (L3; 21 restarts in 14 days, reported) until set back to
  MEDIUM. At most 64. Finished sessions' marks stay as dim rows in `aira top` until removed.

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
(11%) 5 min or more; 413 waited 30 min or more; longest 7,457 s. Verdict: **build**; even without
the lane-class 59% (reported), roughly 11% of runs is reachable, ten times the threshold.

**Greenfield minimum.** A daemon table `owner -> high|low` (medium is no row), consulted by the
ONE ordering function at evaluation time; one setter verb; the `t` key; the tier on rows. Nothing
per waiter, so a mark reorders queued jobs on the next pass. That is this design, plus:

| Addition | Why it stays |
|---|---|
| Marks stored in `state.db` (L3) | Without it every mark is silently wiped about every 16 hours (21 restarts in 14 days). One table in a database the daemon already opens; §3.1. |
| 64-row cap, refusal only | With stored marks and no automatic prune, the only bound on the table, the reply and the idle rows. |
| Idle-mark rows in `aira top` | A stored mark whose owner has no listed job could otherwise not be seen or removed from `aira top`. |
| A writer mutex | Two concurrent setters would otherwise lose an acknowledged mark. |

Cut: any hold or budget change (D6); the `confine --session-tier` flag; a second confirmation
page; `set_at`/`set_by` (the journal records the setter); `jobs_now`; a `queued_tier` field; a
load-failure warning field (nil is unevaluated); a per-waiter tier; a tier on the admit request;
an observe-only mode (an empty table is today, T1); an MCP setter (L6); automatic pruning (R9).

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
no `medium` row exists) and `tiersMu sync.Mutex`. The map is immutable once published. A nil
pointer means **unevaluated** (a failed load, ci-shim mode, a `Server` built without `Serve`): the
evaluator orders it as an empty table, the list reply omits `session_tiers` (§3.5).

**One load per reader, by structure.** `s.tiers.Load()` has exactly three call sites: an
evaluator pass's start, the `confine-list` case's start, the setter. `admitOrderLocked`, the lane
pre-pass, the walk and `admitSliceSnapshotFor` take the map as an argument and never read
`s.tiers`. The build review checks the three sites by grep; no test is claimed for it.

**Set.** Under `tiersMu`, for the whole operation: copy the current map (nil copies as empty);
apply the change (`medium` deletes the row); if the copy has more than 64 rows, refuse
`E_CONFINE_SESSION_TIERS_FULL` (a new row only; re-set and remove always pass); write the copy to
the store; only then publish the pointer and write the audit line
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
equals memory by construction. Not a file: the daemon already opens `state.db` as its single
writer; a file needs a new path, an encoding and a temp-fsync-rename writer.

**Load.** Once in `Serve`, after `OpenDB` (`server.go:493`) and before `net.Listen` (`:514`), so
the first pass and every re-declared survivor see the stored marks. Any failure (query error, an
owner `runner.ValidateConfineOwner` refuses, more than 64 rows) leaves the pointer **nil**: one
journal line naming the cause, the reply's `session_tiers` absent (rendered unevaluated), never a
refusal to start (a schema failure inside `OpenDB` refuses start today, unchanged). **The first
successful set after a failed load replaces the store with memory**, deleting the stored rows
that did not load; until then `aira top` shows `unevaluated`, never "none set". ci-shim mode
neither loads nor sets (nil throughout). No automatic prune: finished sessions' marks stay,
bounded by the cap and shown as idle rows (§3.6, R9).

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
with H's budget spent), which `--priority high` can already put in front of C.

### 3.4 The small lane (D4, L5)

`smallLanePassLocked` (`internal/daemon/small_lane.go:153-219`) uses `admitOrderLocked` today for
two things, which this design separates:

- **Candidate order** (the loop at `:182`): pushed waiters latest push first, exactly today's
  prefix, then every other waiter in `queue.waiters` (enqueue) order, ignoring tier and
  `--priority high` (D4's two keys only). Both lists share the pushed prefix, so every push outcome
  is today's. Dropping push too would not: an older small job visited while a newer pushed one is
  the reference takes `laneWaitPushed` (`:191-195`), is not revisited, and a head in the walk
  charges it to its budget (T6c).
- **The reference** (the waiter guard G protects, `nextRef` at `:171-180`) stays in evaluation
  order (tier-aware): G keeps the room of the waiter the main walk will grant or make the head.
  It is an index into `order := admitOrderLocked(queue, tiers)` only. After **every** lane grant,
  `reference = nextRef(reference)`; the comparison at `:215-216` is deleted. `nextRef` skips a
  waiter no longer `admitQueued` (`grantWaiterLocked` sets `admitGranted`, `admit.go:2940`), so
  the reference moves exactly when it was the one granted, and no candidate index is ever used.

Counterexamples a cursor built from the candidate index fails, each a test: **T7b** (advance from
the candidate position): zero headroom, 32 GiB ceiling, lane cap at least 1.5 GiB; enqueue LOW L
0.5, MEDIUM M 31, HIGH H 0.5, LOW S 0.5 GiB. Evaluation order H, M, L, S; candidates L, M, H, S.
L and H are lane-granted; M becomes the reference; S is `kept` (31 + 1.5 > 32); the walk grants
M (31 + 1 = 32). Advancing from H's candidate index 2 lands on S, grants S and leaves M refused.
**T7c** (today's `index == reference` lines left in place): same box; enqueue LOW A 0.5, HIGH B
31.5, MEDIUM C 0.5. Evaluation B, C, A; candidates A, B, C. A is lane-granted (B fits beside);
B stays the reference; C is `kept` (31.5 + 1 > 32); the walk grants B (32 <= 32). With the old
lines, A's candidate index 0 equals the reference index 0, the reference jumps to C, C is
lane-granted and B is refused (32.5 > 32). T7b alone passes against the old lines by accident.

**The one visible change for sessions that do nothing** (L5): when the lane cap is contended, an
older ordinary small job gets the last lane room before a newer `--priority high` one, which then
goes in the main walk at its job-high place.

### 3.5 The verb and the wire

A new project-less confine management verb `confine-session-tier`, args `session` (target owner),
`tier` (`high|medium|low`) and the siblings' `owner` (caller). No `slice`: tiers are machine-wide,
and an accepted-and-ignored `--slice` is what AIRA-82/196 refuse. So the daemon branch **refuses
a request carrying a `slice` key** (any value, empty included) with `E_CONFINE_ARGUMENT_INVALID`,
and the CLI skips the slice insertion that `dispatchConfineManagementRequest` makes for every
sibling (`main.go:2386-2387`) for this verb; `aira top` builds its args without `slice` (unlike
`tui_inline.go:134`). A missed skip is then a loud refusal in T19a, not a silently ignored key.
CLI: `aira confine-session-tier <owner> <high|medium|low>`. Reply `{session, tier, previous}`.

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
| `internal/core/core.go` beside `confine-front` (`:2203`) | `Include: true`, no `MCPTool` (L6) | T21 |

`main.go:2369` is NOT touched (flag spelling cut). Dispatch-table summary: "Set a session's
admission order tier (queue order only). Set by the owner from `aira top`; agents do not set tiers."

**Error codes.** `E_CONFINE_ARGUMENT_INVALID` (exit 2: bad `session`, `tier` or a `slice` key),
`E_CONFINE_UNAVAILABLE` (exit 4: ci-shim, where a container is one session; store failure), and
new `E_CONFINE_SESSION_TIERS_FULL`, exit 1 (a state condition, like `E_CONFINE_NOT_QUEUED`,
`codes.go:381`); its catalogue entry and producer land together (`produced_test.go`, both ways).

**List reply.** `confine-list` gains one field, `session_tiers` (`*[]{owner, tier}`, sorted by
owner, `omitempty`): absent when the pointer is nil or the daemon is old; `[]` when loaded and
empty. The case loads the pointer once at its start (§3.1) and sets the field on every
`ConfineListResult` it returns, the `UNEVALUATED` early return included
(`confine_manage.go:165-185`). It comes from the `Server` table, never the queue snapshot, so a
mark with no waiters or no live queue is still listed. Absent renders `unevaluated`, never "none
set". Queued rows gain no field. Both renderers' unevaluated early returns, `main.go:3655`
(before the slice summary) and `tui_top.go:657` (before the normal footer), print the
`session tiers:` line too (T17c).

**Protocol version stays 15** (`internal/daemon/protocol.go:218`, pinned by `protocol_test.go:128`
and `protocol_pin_linux_test.go`): no admit, re-declare or store-op frame changes; a comment entry
in the AIRA-340 style. Skew, measured by the builder against the live pre-381 daemon:
- new client, old daemon: the unknown verb falls through to project dispatch
  (`server.go:1040`), whose code is outside `E_CONFINE_*`, so `withOldDaemonManagementHint`
  keeps the daemon's code and message and appends "the running aira daemon may predate
  `aira confine-session-tier`; restart it"; never a claimed success (T25);
- old client, new daemon: the new list field is unknown to the old client's decoder, which is
  not `DisallowUnknownFields` on this reply (the builder confirms).

### 3.6 `aira top` and `confine --list`

- **SESSION cell** (`topSessionCell`, `cmd/aira/tui_top.go:1237`) of every row gains ` (high)` or
  ` (low)` for a marked owner; the LIVE cell keeps `pushed`, `high` (job priority) and the
  AIRA-370 reason, so the two kinds of "high" stay in different columns.
- **Idle-mark row.** Each mark whose owner has no LISTED row is drawn as a dim row after the queued
  rows: `session <owner>: tier low, no listed jobs`. "Listed", because queued rows are capped at
  64 (`runner.ConfineQueuedRowLimit`); when the queue is longer the existing "showing N of M" line
  says so. Its only action is `t`.
- **The `t` key** (`viewTop` key switch, `cmd/aira/tui_inline.go:106`): one prompt, built like
  `topFrontConfirm` (`:129`): "Session field (owner `field`) is MEDIUM. h: HIGH, m: MEDIUM
  (removes the mark), l: LOW, Esc: cancel. Changes the order of its waiting and future jobs only;
  running jobs are not affected. Kept until changed." The keypress sends (`DispatchPalette`,
  caller `state.ConfineOwner`, no `slice`) the row record's full owner (`record.Owner`), never
  the cell text; the result line shows `session field: medium -> low` or the error. A row whose
  owner is empty or `unknown` answers "this job has no session owner to mark", sending nothing.
- **Footer**, and the same line under `confine --list`'s slice summary:
  `session tiers: high field; low cc-1a2b3c4d`, `session tiers: none set`, or
  `session tiers: unevaluated (old daemon, ci-shim, or stored marks did not load: see the daemon
  journal)`. `--json` carries the field as sent.

## 4. Invariants

- I1 Order: the seven-bucket order of §3.2; with an empty table, today's order element by element
  (T1); all-HIGH: pushed, owned job-high, owned normal, untiered job-high, untiered normal; all-LOW:
  pushed, untiered job-high, untiered normal, owned job-high, owned normal (T2).
- I2 Order only: each pass makes exactly the grants, `bypassed`, anchor, phase and diagnosis
  writes today's evaluator makes walking the tier order (T10, T29, T30).
- I3 No per-waiter state; a mark reorders already-queued waiters on the next pass (T8).
- I4 One load per reader, by structure (§3.1, three `Load` sites); `session_tiers` is on every
  `ConfineListResult` the case returns and both renderers print it (T4, T17c).
- I5 Lane: candidates pushed-first then FIFO, blind to tier and job priority; the reference is in
  evaluation order and moves only by `nextRef(reference)` (T6, T6b, T6c, T7, T7b, T7c).
- I6 Identity: a waiter's tier key is the owner its row displays (T24).
- I7 Writes: a set changes only the table and the store; every acknowledged set is in the
  published map and the store equals it (T14, T15, T12c).
- I8 Persistence: after a restart the table equals the store; a failed load gives a nil table
  (ordered as empty, reported unevaluated) and a running daemon; the next set replaces the store
  (T12, T12b).
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
| T4 | order | snapshot ordered without the table | lane off, equal sizes, room for one at a time, mixed tiers: pass-then-release grants follow the snapshot's position order exactly (the single load is structural, §3.1) |
| T5 | order | tier ranked above push; pushes not latest-first | pushed LOW A then B behind waiting HIGH jobs: grant order B, A, then the HIGH jobs |
| T8 | order | tier copied at enqueue | enqueue MEDIUM, then mark LOW: next pass reorders; remove: order restored |
| T9 | starve | sub-reservation tiered by its parent | parent owner LOW; its sub-reservation (older) then a MEDIUM job-level waiter, room for one: the sub-reservation is granted, as with an empty table |
| T24 | order | key read from `waiter.owner` | scope id without owner tail, request owner `field`, `field` HIGH: the waiter is MEDIUM and its row shows unowned |
| T10 | both | no tier order (FIFO); zero-budget tier arm (false starve); budget skipped for lower tiers (false order) | lane off, 14 of 32 GiB running, no pin discount; HIGH head H 20 GiB refused; behind it, enqueued in this order, fitting LOW L1 4, LOW L2 4, MEDIUM M 4 GiB: M and L1 granted (8 <= 10), L2 withheld (12 > 10), anchor armed, phase `hold`; next backstop pass grants L2. FIFO grants L1, L2 and withholds M; a zero-budget arm withholds L1; no budget grants L2 |
| T29 | both | any tier-aware condition in the head switch | random queues, random tables, random passes: per pass grants, `bypassed`, anchor, phase, `heldByHead` and `blocked` identical to a reference evaluator holding today's walk verbatim, fed `admitOrderLocked(queue, tiers)` |
| T13 | starve | tier used as a gate | LOW job alone on an empty slice: granted in the first pass |
| T30 | starve | a tier hold of any kind | running MEDIUM R (14 of 32 GiB); HIGH head H 20 GiB; R's nested 2 GiB child C, lane off: C granted within H's budget; R released; H granted |
| T6 | order | lane candidates in evaluation order | lane room for one: older LOW small job lane-granted, newer HIGH small job not (asserted via `smallLaneGrantObserver`) |
| T6b | order | lane candidates keep job-high first | lane room for one: older normal small job, newer `--priority high` small job, same session: the older is lane-granted (observer); the newer is not lane-granted in that pass |
| T6c | false order | lane candidates ignore push (plain FIFO) | zero headroom, 32 GiB, 14 running; enqueue MEDIUM small A 0.5, HIGH B 20 (refused), then push small P 0.5: P and A lane-granted in one pass (observer) and B's `bypassed` stays 0; FIFO candidates leave A `laneWaitPushed`, and the walk grants it charged to B (`bypassed` 0.5) |
| T7 | starve | lane reference taken from the candidate (FIFO) order | 32 GiB, 26 granted, no small leases; enqueue MEDIUM 8, HIGH 31.5, LOW 1 GiB small: the reference is HIGH (31.5 + 1 > 32), so the small job is `kept`; a FIFO reference (MEDIUM, 8 + 1 <= 32) would grant it |
| T7b | false order | reference advanced from the candidate position (`nextRef(index + 1)`) | §3.4 T7b case, one pass: L and H lane-granted (observer); S's `laneWait` is `kept`; M granted by the walk. Mutation: S lane-granted, M refused |
| T7c | false order | today's `if index == reference` lines kept | §3.4 T7c case, one pass: A lane-granted; C `kept`; B granted by the walk. Mutation: C lane-granted, B refused |
| T12 | order | marks not loaded, or loaded after the first pass | set `field` HIGH, restart the Server on the same state dir with `field`'s survivor re-declaring and a new `field` waiter queued behind MEDIUM ones: the first pass orders it HIGH; footer lists `high field`; ARDR lease unchanged |
| T12b | starve | load failure refuses start, or reported "none set" | injected failing load: daemon serves; pointer nil, order as empty; reply has no `session_tiers`; `--list` and footer read `unevaluated`; journal names the cause; then a set succeeds, the reply lists exactly it, and the store equals it (the unloaded rows are gone) |
| T12c | order | table published before the store write | injected failing write: set refused `E_CONFINE_UNAVAILABLE`, published table and store both unchanged |
| T14 | order | a set touches ledger or head state | ledger, `bypassed`, anchor, exclusive gate identical before and after a set with no pass between |
| T15 | order | `tiersMu` dropped | N goroutines setting distinct owners, `-race -count=20`: every acknowledged set in the final map and the store; 63 rows plus 2 concurrent new owners: exactly one refused `E_CONFINE_SESSION_TIERS_FULL` |
| T17 | order | empty list dropped or sent as null | no marks: `session_tiers` is `[]` |
| T17b | order | absent rendered "none set" | `--list` and the `aira top` footer, fed a reply without `session_tiers`: `session tiers: unevaluated` |
| T17c | order | `session_tiers` filled from the snapshot, only on the OK path, or dropped by an early-return renderer | through a real Server on a socket: a mark with no waiters and no live queue is listed; on the `UNEVALUATED` return the field is present and the rendered `--list` text (`main.go:3655` path) and `aira top` footer (`tui_top.go:657` path) both carry `session tiers: high field`; shim path: field absent |
| T18 | order | `unknown`, empty, over-long or bad-tier values accepted | each refused `E_CONFINE_ARGUMENT_INVALID`, exit 2, by client and daemon |
| T19 | order | verb missing at an allowlist site (§3.5 table); `t` sends the cell text; `slice` sent or ignored | (a) `aira confine-session-tier field low --owner me` from outside any project, against a real Server on a socket: OK, map and store hold `field: low`; (b) the `t` key through the `aira top` model and `DispatchPalette` to the same real Server: rows showing a tmux name or a shortened `cc-` id send the full owner, an ownerless row sends nothing, the sent map has no `slice`; (c) daemon refuses a `slice` key (empty and non-empty), CLI refuses `--slice`, the CLI's sent map has no `slice`; (d) `aira confine --help` lists the verb; (e) `Classify`, `StoreFreeCarved`, `--scope-dir` refusal |
| T20 | order | cap not enforced, or enforced on re-set/remove | 64 rows: a new owner refused `E_CONFINE_SESSION_TIERS_FULL` exit 1, nothing changed; re-set and remove succeed |
| T21 | order | MCP exposes the setter | generated MCP tool list has no session-tier tool; help and agent guide carry "agents do not set tiers" |
| T22 | order | `(high)` shown for job priority | queued `--priority high` row of a medium session: SESSION cell unmarked, LIVE cell `high` |
| T25 | order | old daemon's refusal claimed as success, its code replaced, or no hint | stub old daemon (the one stub use) answering with a project-dispatch error: non-zero exit with that code's status, message kept, hint naming `aira confine-session-tier` appended |
| T31 | order | "no jobs" derived from capped rows | 70 queued; a LOW owner whose only jobs are rows 65-70: idle row reads `no listed jobs`; "showing 64 of 70" present |

## 6. Owner lines (accept or veto, one line each)

- **L1 Minimum.** The table consulted at evaluation time; no per-waiter state, no admit-wire
  change, no observe mode. Recommended: accept.
- **L2 Who is untiered.** Suite worker claims, `aira run` jobs and jobs whose scope id has no owner
  are always MEDIUM. So marking every owner HIGH is not the same as no marks. Recommended: accept.
- **L3 Lifetime.** Marks are stored in `state.db` and survive daemon restarts, until set back to
  MEDIUM; at most 64; no automatic prune (finished sessions' marks stay as dim rows until removed);
  a load failure starts the daemon with no marks, reported `unevaluated`. Recommended: accept (21
  restarts in 14 days, reported). Alternative: marks in memory only, wiped at every restart like
  `--front` pushes; deletes the table, the two store methods, the load, the nil state and T12b,
  T12c; costs re-marking after each cutover, silently.
- **L4 `@cwd-` keys.** Allowed (covers every unattributed launch from that directory). Accept.
- **L5 Lane order.** Lane candidates pushed first, then FIFO, ignoring `--priority high` too: the
  one change for sessions that do nothing (§3.4). Recommended: accept. Alternative: keep job
  priority in the lane order and ignore only tiers.
- **L6 Trust and surface.** CLI and `aira top` only; no MCP tool; no ownership guard (marking
  other sessions is the feature); every set audit-logged with its setter in the daemon journal.
  Any agent on this UID can still run the CLI, as it can pass `--priority high` today; the skill
  text says agents never set tiers. Recommended: accept.
- **L7 Names.** Verb `confine-session-tier` (bare "tier" is taken by review tiers), key `t`. Accept.
- **L9 A push beats a tier.** `--front` goes first whatever the tier; a session pushing its own job
  needs no `--steal`. Recommended: accept (D2). Alternative: a LOW session's push needs `--steal`.

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

- Milestone v0.47. Client install AND `systemctl --user restart aira-daemon.service`, after
  `aira confine --list` and a heads-up. Unmarked sessions: main-queue order and head rule
  unchanged (T1, T29); the one change is the lane's arrival-order offer (L5).
- Peer heads-up at the tagged release (deploy, speed): "The owner can mark sessions high or low in
  `aira top`; a mark only reorders waiting jobs, like `--priority high`; the small-job lane ignores
  it; marks survive daemon restarts; agents must not set tiers. The lane now takes `--priority
  high` small jobs in arrival order." Skill text (dispatch table): "Session tiers are set by the
  owner from `aira top`; agents do not set them."

## Review log

### Rounds 1-4

Rounds 1-3 (9, 12 and 6 blockers) built, then re-based, a hold for lower tiers; a hold-class
blocker every round met the re-evaluation rule, so the hold went to the owner as L8. Round 4
applied the answer (order only, D6): the hold, the `--session-tier` flag, the second confirmation
page, `set_at`/`set_by`/`jobs_now` and `slice` on the verb were deleted; §3.3, the lane cursor,
every allowlist site, the Server-table list field, persistence, T10, T29 and T30 were added.

### Round 5 (review round 1 of revision 2): 8 findings, all verified against the code, accepted

Closed by a design change that deletes a family:
- **Lane cursor (P1, P2).** Re-modelled at `small_lane.go:171-218`: T7b passes against today's
  `index == reference` lines by accident, the A/B/C case fails them, and the observer runs only
  after a grant (`:210-214`). Now `reference = nextRef(reference)` after every lane grant: the
  comparison and every candidate index are deleted (§3.4); T7b asserts `kept` plus the walk's
  grant; T7c added.
- **Lane candidates (P2).** Plain FIFO does change push outcomes (`:192-195`): candidates are now
  pushed first, then FIFO, so the push claim holds by construction (T6c).
- **Second load (P2).** Single load is structural (§3.1, three `Load` sites); the race test is cut.
- **Load failure (P2).** Nil pointer = unevaluated, cause in the journal; deletes
  `session_tiers_warning`, `tiersWarning` and the footer suffix. The first set after a failed load
  deleting the unloaded rows is stated (§3.1, T12b).

Needs code: `slice` inserted at `main.go:2386` (daemon refuses the key, CLI skips it, T19c);
stub-only T19 (site-to-test table, real Server, T19d, T25); the two renderer early returns
(T17c); T10 enqueued in tier order (now LOW, LOW, MEDIUM).
