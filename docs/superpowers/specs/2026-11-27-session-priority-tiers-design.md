# Session priority tiers: high / medium / low sessions in admission order (AIRA-381)

Status: plan, round 2 (after review round 1; see the Review log), for plan review (Sol or Astra,
plus one independent Claude lineage) and the Fable plan gate (the premise measurement in §1 also
needs its one method-and-verdict check, per the 2026-11-19 audit rules). Ticket AIRA-381, milestone
v0.47. Correctness-critical (admission queue order): two-loop, Opus builds, Fable and Sol review.
Builds on AIRA-307/309 (queue order, `2026-10-13-aira-queue-order-design.md`), AIRA-340
(`--priority high`), AIRA-350 (small-job lane, `2026-11-18-fixed-small-lane-design.md`) and AIRA-370
(wait diagnosis). Code references are to master at `dc19915c`.

## Summary for the owner

You can mark a session HIGH or LOW from `aira top`. Every session you have not marked is MEDIUM,
so nothing changes until you mark one. A mark does two things to waiting jobs, and nothing to
running ones:
1. **Order.** Waiting jobs are considered high-tier sessions first, then medium, then low. Inside
   each tier a `--priority high` job (hooks, short pre-commit steps) goes before that tier's
   ordinary jobs, oldest first. A job pushed with `--front` goes before everything.
2. **Hold.** When a job is waiting for room at the front of the line, no job of a LOWER tier starts
   past it in the main queue, even one that would fit now. Jobs of its own tier may still start
   past it, within today's limits (half its booking, plus one catch-up pass every 5 minutes).

| Waiting job | Order |
|---|---|
| pushed with `aira confine --front` (any session) | 1st (latest push first, as today) |
| HIGH session, `--priority high` job | 2nd |
| HIGH session, ordinary job | 3rd |
| MEDIUM session (the default), `--priority high` job | 4th |
| MEDIUM session, ordinary job | 5th |
| LOW session, `--priority high` job | 6th |
| LOW session, ordinary job | 7th |

Examples, on a box where memory is short and jobs queue:
- You mark session `field` HIGH. Its next `make ci` goes before the 15 medium jobs that queued
  before it. If it does not fit yet, none of those medium jobs starts in the main queue until it
  does; only small jobs (1 GiB or less) through the small-job lane can, and only when they leave
  `field`'s job the room it needs.
- You mark a long-running sweep session LOW. Its 4 GiB job waits while any medium job of 10 GiB is
  waiting for room, even when 4 GiB is free. On a quiet box (nothing of a higher tier waiting) it
  still starts at once: LOW means "behind everyone else who is waiting", not "only when idle".
- A LOW session's pre-commit hook job (`--priority high`) still waits behind every waiting MEDIUM
  job.
- A small job (1 GiB or less) of any session can still go through the small-job lane exactly as
  before: the lane ignores tiers and `--priority high`. HIGH gives no lane advantage, LOW no lane
  penalty.
- Jobs already running are untouched: no job is stopped, slowed or resized by a tier.

What you give up, said plainly:
- **LOW can wait indefinitely.** While medium and high sessions keep jobs waiting, a LOW job does
  not start in the main queue. No age limit, no automatic promotion (your decision D3).
- **A busy HIGH session can make every unmarked session wait without bound** in the same way: a
  HIGH session that keeps big jobs queued keeps medium jobs waiting behind them. That is the
  hold doing what "session wins" means; un-mark it to undo.
- **A HIGH session's new jobs also go before running test suites' worker claims** (`aitest` workers
  and `confine-reserve` holds). Those claims are always MEDIUM (L2), so a suite of an unmarked
  session can progress more slowly while a HIGH session has jobs waiting. They are never held by
  the hold, only ordered after HIGH jobs.
- **A push still beats a tier.** `--front` goes first whatever the session's tier, and a session
  may push its own job without `--steal`, so a LOW session can push its own job past everyone
  (decision D2, owner line L9).
- **A mark follows an owner string exactly.** A Claude session's string is `cc-<session id>`.
  `/clear` (and `--fork-session`) give the session a new id, so the mark does not follow; mark the
  new one. `/resume X` switches to conversation X's existing id, so X's mark, if any, applies
  again. A session that sets a stable name (`AIRA_CONFINE_OWNER=field`) keeps its mark for as long
  as the name is used.
- **One mark per name.** Every session launching jobs under the same owner name shares one tier.
- **At most 64 marks.** A 65th is refused; remove one (set it to medium) first. Nothing is
  removed automatically.

Marks are kept across a daemon restart and a machine restart (owner line L3).

## 1. The problem, and the greenfield minimum

**Problem.** The owner wants a standing, per-session say in who is admitted first, set by hand,
with a LOW tier the box has never had. Today's levers are per job: `--front` (an operator's one-off
push of one queued job) and `--priority high` (set at launch by hooks and Makefiles). Neither can
demote a session, and neither applies to a session's future jobs. The owner's words: tier is keyed
on the owner string when possible; the session tier wins, with job priorities as sub-levels under
it; low-tier sessions wait; lanes ignore priorities and tiers.

**Premise measurement (decision rule written before the query).** Rule: if, over the last 14 days,
fewer than 1% of confined runs waited 60 s or more for admission, queue order rarely decides
anything and this spec recommends stopping; otherwise build. Query, read-only, against the live
store (`confine_peak_history.wait_ms`, AIRA-323):

```bash
aira confine --memory-reserve 256M -- sqlite3 -readonly ~/.local/state/aira/state.db \
 "SELECT count(*), sum(wait_ms>=60000), sum(wait_ms>=300000), sum(wait_ms>=1800000), max(wait_ms)/1000
  FROM confine_peak_history WHERE wait_ms IS NOT NULL
  AND at >= strftime('%Y-%m-%dT%H:%M:%SZ','now','-14 days');"
```

Result 2026-11-27 (data starts 2026-11-15): 31,167 runs; 8,224 (26%) waited 60 s or more; 3,379
(11%) 5 min or more; 413 waited 30 min or more; longest 7,457 s; 1,136 job-hours spent waiting; 13
distinct days with a wait of 60 s or more. The probe's own 256M job queued 15 s, 15th of 15.
Verdict: **build** (26% against a 1% stop threshold). Queue order decides a quarter of all runs.

**Greenfield minimum.** A daemon-side table `owner -> high|low` (medium is the absence of a row),
read by the ONE ordering function at evaluation time, one comparison in the head rule ("a head
holds every lower-tier job-level waiter"), plus one verb to set a row and one place to see the
table. Nothing is stored per waiter, so a mark applies to already-queued jobs on the next
evaluator pass and to every future job of that owner. That is this design. The gaps from it, each
justified:

| Addition beyond the minimum | Why it stays |
|---|---|
| Rows persisted in `state.db` (L3) | The stable-name case is the one D1 exists for; losing marks at every release cutover would reset them exactly when the box is busiest. One small table, no per-waiter state. |
| A 64-row cap, refusal only (L3) | Bounds the table, the footer and the idle-mark rows against a scripted loop. One count of a map the setter already holds; no eviction, no timestamps consulted. |
| Idle-mark rows in `aira top` (L6) | Without them a mark on an owner with no jobs could not be seen or removed from `aira top`. Reuses the queued-row machinery (AIRA-308). |
| Tier-blind lane order (D4, L5) | Required by D4. Two lines in the lane pre-pass. |
| A writer mutex for the setter | Two concurrent setters would otherwise lose an acknowledged mark (§3.1). Readers stay lock-free. |

Cut, considered and rejected: a per-waiter tier field (staleness and a second source of order); a
tier on the admit request (would need the client to know its tier, and a restart race); an
observe-only daemon mode (an empty table is already today's behaviour, pinned by test T1, so the
first mark the owner sets is the enforce step); an ownership guard on the verb (L6); automatic
eviction of rows (round 1: it removed exactly the long-lived stable-name marks persistence exists
for); a `queued_tier` field on the wait line (round 1: an absent field could not be told from
medium, and the position and `--list` already show the tier; deleted with its tests).

## 2. Decided by the owner (2026-11-27), recorded, not reopened

- **D1 Identity.** A tier is keyed on the job's owner string, the same string `aira confine` puts
  in the scope id (owner chain `resolveConfineOwner`, `cmd/aira/main.go:2290`: `--owner`, then
  `AIRA_CONFINE_OWNER`, then `cc-<CLAUDE_CODE_SESSION_ID>`, then the worktree id, then an inferred
  `@cwd-<dir>`). Subagents inherit their parent's session id, so they share its tier. "When
  possible": a waiter whose scope id carries no owner is untiered (§3.2).
- **D2 Composition.** Lexicographic key: pushed first (latest push first); then session tier HIGH,
  MEDIUM, LOW; inside a tier, `--priority high` before normal; FIFO (enqueue order) inside each
  bucket. With no marks the order is element-for-element today's (pushed, job-high, rest).
- **D3 Starvation.** LOW simply waits: no age cap, no promotion (analysis in §5). The two ways a
  LOW job still starts while higher-tier jobs wait are both the owner's own decisions: the lane
  (D4) and a push (D2).
- **D4 Small lane.** The lane ignores tiers and job priorities entirely (§6).
- **D5 Running jobs.** Admission order only: no preemption, no reservation change, no cpu.weight
  change.

## 3. Design

### 3.1 The table

In memory: an immutable `map[string]sessionTier` (values `high`, `low`; no `medium` row ever
exists) held by an `atomic.Pointer` on `Server`. Readers (evaluator, snapshot, lane, list) load the
pointer ONCE per call, lock-free, so one evaluator pass or one snapshot sees one table.

Writers are serialised by one dedicated `tiersMu sync.Mutex`, held for the whole
read-copy-commit-publish of a set: load the current map, check the cap, write the store row
(upsert, or delete for `medium`) and, only after the write commits, publish the new map. So
publication order is commit order, and two concurrent setters cannot each publish a copy missing
the other's row (round 1, P1). `tiersMu` is never taken while `queue.mu` is held, and `queue.mu` is
never taken while `tiersMu` is held (the reply's job count is read after `tiersMu` is released), so
there is no lock-order edge at all. A failed write publishes nothing and answers the store's error
code (T15).

Persisted: a machine-wide `state.db` table, written through the same `s.db` handle
`confine-report` uses for `confine_peak_history`:

```sql
CREATE TABLE IF NOT EXISTS confine_session_tiers (
  owner TEXT PRIMARY KEY, tier TEXT NOT NULL CHECK (tier IN ('high','low')),
  set_at_ms INTEGER NOT NULL, set_by TEXT NOT NULL, CHECK (length(owner) BETWEEN 1 AND 64))
```

At most 64 rows of at most about 150 bytes: under 10 KiB. Read once at daemon start. One daemon per
state dir (v0.34) is the only writer, so under `tiersMu` the map equals the rows. A load failure at
start does not stop the daemon (a tier is never worth refusing all admission): it starts with an
empty table (today's order), logs the error, reports the table unevaluated (§3.3), and refuses
every set with `E_CONFINE_UNAVAILABLE` until the next daemon start, so a mark can never be shown as
set while the stored table is unknown (T16).

Cap (L3): a set that would add a 65th row is refused with `E_CONFINE_SESSION_TIERS_FULL` (new code,
exit 1, a state condition like `E_CONFINE_NOT_QUEUED`), changing nothing. Re-setting or removing an
existing row is always allowed. No row is ever removed except by a `medium` set.

Valid keys: anything `runner.ValidateConfineOwner` accepts, except `unknown` (the marker for "no
owner", which names no session) and the empty string; both are refused `E_CONFINE_ARGUMENT_INVALID`.
An inferred `@cwd-<dir>` key is accepted: it groups every unattributed launch from that directory,
and the reply says so. The key is matched exactly, never by prefix, so no selector can be
ambiguous; a typo creates a row for an owner with no jobs, and the reply's job count (`0 jobs now`)
plus the idle row in `aira top` make that visible.

### 3.2 The ordering function and the hold

**Whose tier.** A waiter's tier key is the owner EMBEDDED in its scope id,
`runner.ParseConfineScopeID(waiter.scopeID)`, the same parse every row display (`--list`,
`aira top`, `confine_kill_queued.go:216`) and the `--front` and kill ownership guards
(`confine_front.go:113`) already use. It is NOT `waiter.owner`, the request's claim:
`validateAdmitArgs` (`admit.go:3796-3802`) accepts a claim when the scope id has no owner tail (the
pre-AIRA-52 compatibility path), so keying on the claim would let a job be treated HIGH while every
display shows it unowned and no `t` could reach it (round 1, P2). Keying on the one parse every
reader uses makes the tier and the shown owner the same string by construction. A tier is looked up
as `tiers[key]`, defaulting to MEDIUM.

**Untiered waiters, MEDIUM whatever the table says:**
- a sub-reservation (`isSubReservation()`: aitest workers, `confine-reserve` holds). It is a running
  job's internal progress, so demoting it would act on a running job (D5); it already skips the
  head rule, and so it also skips the hold;
- a scope-less job-level waiter (`aira run`), which carries no owner (deferral R7);
- a scoped waiter whose scope id has no owner tail (shown as unowned everywhere; nothing to key on).

The consequence, written out (round 1, G2): because untiered waiters sit at MEDIUM, "every session
in the same tier" equals today's order only for the MEDIUM tier (the empty table). With every owner
HIGH, owned job-level waiters all sort before every untiered one; with every owner LOW, after. I1
and T2 state this exactly.

**Order.** `admitOrderLocked(queue)` (`internal/daemon/admit.go:475`) becomes
`admitOrderLocked(queue, tiers)`: one pass over `queue.waiters` sorting queued waiters into seven
buckets (pushed; HIGH/job-high; HIGH/normal; MEDIUM/job-high; MEDIUM/normal; LOW/job-high;
LOW/normal), pushed sorted by descending `pushSeq` as today, the rest in `queue.waiters` order.

It stays the single source for all its readers. Today three call sites: the evaluator
(`admit.go:2676`), the snapshot (`admit.go:1405`: queue position, queued-ahead bytes, `aira top`
and `--list` rows) and the lane's reference waiter (`small_lane.go:157`). Each passes the table it
loaded; the snapshot copies that same table pointer out, so the rows, their order and the tiers
shown beside them come from one table (T4).

**The hold (round 1, G1).** One new arm in the head-protection switch (`admit.go:2740`), placed
after the pushed arm and BEFORE the backstop (`phase == admitFreezeYield`) arm:

```go
case head.pushSeq > 0: // unchanged: strict
case tierOf(waiter) < tierOf(head): // new: a lower-tier job-level waiter is held strictly
	fits = false
	waiter.heldByHead = true
case phase == admitFreezeYield: // unchanged: backstop, same tier only now
case <budget exhausted>:        // unchanged: budget, same tier only now
```

`protected` is unchanged (`head != nil && !waiter.isSubReservation()`), so sub-reservations are
never held. The new arm sets neither `budgetWithheld` nor the anchor, exactly like the pushed arm,
so a tier hold never arms or renews the AIRA-307 fairness hold. A waiter of a higher tier than the
head cannot be behind it (it sorts first); only pushes can, and a pushed waiter is never behind an
unpushed head. The `--priority high` / normal split inside one tier keeps today's budget, as
AIRA-340 does.

### 3.3 The set verb and the wire

A new project-less confine management verb, `confine-session-tier`, sibling of `confine-front` and
`confine-resize`, routed through `confineManagement` (`internal/daemon/confine_manage.go`) and the
same client-side lists (`cmd/aira/main.go:381`, `dispatcher.go:162/223/523`,
`internal/core/routing.go:51/135`, `scope_dir.go:154`; AIRA-309 found a missing entry once, so a
test asserts the verb reaches the daemon: T19). Args: `session` (target owner), `tier`
(`high|medium|low`), plus the siblings' `owner` (caller) and `slice`.

CLI: `aira confine --session-tier <owner>=<high|medium|low>`; `medium` removes the mark (the one way
to remove it: one name, one meaning). `=` is outside the owner alphabet, so the split is exact.
Reply: `{session, tier, previous, jobs_now}`. Daemon audit line on success only, like
`logConfineFront`: `confine-session-tier: setter=<caller> session=<owner> tier=<old>-><new>`.
ci-shim mode refuses the verb with `E_CONFINE_UNAVAILABLE` (a CI container is one session).

Error codes and exit statuses, each pinned by a test (T18, T20, T25): `E_CONFINE_ARGUMENT_INVALID`
(bad value), `E_CONFINE_UNAVAILABLE` (load failure, ci-shim), `E_CONFINE_SESSION_TIERS_FULL` (cap),
the store's own code on a failed write; each with `codes.ExitForCode`.

Dispatch table (`internal/core/core.go`, beside `confine-front` at :2203): `Include: true` so the
generated help and agent guide describe it; NO `MCPTool` (L6). Its summary says: queue order and
hold only; set by the owner from `aira top`; agents do not set tiers.

Reads, one additive reply field, no request change:
- `confine-list` gains `session_tiers`: a list of `{owner, tier, set_at_ms, set_by}`. An empty
  list is a positive "none set"; an ABSENT field is unevaluated (old daemon, or load failure with
  `session_tiers_error`). The field is never `omitempty`-dropped when empty (T17). Clients render an
  absent field as `session tiers: unevaluated`, never as "none set" (T17b). Job counts per owner are
  derived by the client from the same reply's rows, so the list carries no second count.

**Protocol version stays 15.** Precedent: AIRA-312 added `confine-resize` and AIRA-340/324/323 added
fields without a bump, because a bump refuses every confine during a client/daemon skew while the
old side already fails loudly per request. The admit request and the ARDR re-declare frame are
unchanged, so a version-frozen client/daemon pair across a restart re-declares exactly as today;
after the restart the reloaded table applies to every re-enqueued waiter on its first pass (T12).
`TestRunnerDaemonProtocolVersionMatchesTheDaemon` and the `protocol_test.go` pin stay at 15 and must
pass unchanged; `protocol.go` gets a comment entry in the AIRA-340 style. Skew, measured by the
builder against the live pre-381 daemon before claiming it (AIRA-340 did the same):
- new client, old daemon: an old daemon does not know the verb, and an unknown management verb
  falls through to project dispatch (`server.go:1041`), so its error code is whatever that path
  answers (round 1: not a reliable "unknown verb"). The client keeps the daemon's code and message
  verbatim, exits non-zero with that code's exit status, and appends a qualified hint: "if the
  daemon predates session tiers (v0.47), restart aira-daemon.service". Never a claimed success
  (T25). `aira top` and `--list` show `session tiers: unevaluated` (T17b).
- old client, new daemon: unchanged behaviour; the new list field is ignored (the builder confirms
  the list decoder is not `DisallowUnknownFields`).

### 3.4 `aira top`

- **Where a tier shows.** The SESSION cell (`topSessionCell`, `cmd/aira/tui_top.go:1238`) of every
  row, live and queued, gains ` (high)` or ` (low)` when the row's owner has a mark; medium shows
  nothing. A queued row's LIVE cell keeps its words (`pushed`, `high` for `--priority high`, the
  AIRA-370 reason; a tier-held waiter reads `behind head`, which is now literally true), so the two
  kinds of "high" stay in different columns.
- **Idle marks.** Each mark whose owner has no rows is shown as a dim row after the queued rows:
  `session <owner>: tier low, no jobs (set by <setter>, <age> ago)`. A new row kind like `Queued`,
  whose only action is `t`.
- **Recognising a session.** The SESSION cell shows a chosen name verbatim, else the tmux window
  name, else `cc-` plus 8 hex digits, else the worktree hash prefix (AIRA-277/313). The `t` dialog
  adds the full owner string and the job counts, which is the minimum to tell two sessions apart.
- **The `t` key.** On any row with a valid owner (live, queued or idle-mark), `t` opens a three-way
  choice (h/m/l) and then a confirmation page built like `topFrontConfirm` (`tui_inline.go:129`):
  "Set session field (owner field: 2 running, 3 queued; now medium) to LOW. Its queued jobs will
  wait behind every waiting medium and high job; running jobs are not affected. Applies at once and
  is kept across daemon restarts." The request's `session` is the row record's full owner string
  (parsed from the scope-id tail, `record.Owner`), never the SESSION cell's text, which may be a
  tmux window name or a shortened `cc-` id (T26). A row whose owner is empty or `unknown` answers
  "this job has no session owner to mark" and sends nothing.
- **Footer.** One line: `session tiers: high field; low cc-1a2b3c4d` or `session tiers: none set`,
  or `session tiers: unevaluated (<reason>)`.

### 3.5 `confine --list` and the wait line

`confine --list` prints the same footer line under the slice summary; `--json` carries
`session_tiers`. Queued rows get no new field: a row's tier is its owner's entry in the same reply's
`session_tiers`. The client wait line is unchanged (the round-1 `queued_tier` field is cut).

The AIRA-370 diagnosis code gains no token: a tier-held waiter sets the existing `heldByHead`, so
its token is the existing `behind head`, which is now exactly the gate that held it.

## 4. Invariants

- I1 Order: for every queue, `admitOrderLocked` returns the seven-bucket order of §3.2. With an
  empty table it equals today's three-bucket order element by element, untiered waiters included
  (T1). With every owner HIGH it is: pushed; owned job-high; owned normal; untiered job-high;
  untiered normal. With every owner LOW: pushed; untiered job-high; untiered normal; owned job-high;
  owned normal (each bucket FIFO) (T2).
- I2 No per-waiter state: nothing about tiers is written to `admitWaiter`; a mark changes the order
  and the hold of already-queued waiters on the next pass (T8).
- I3 One ordering source: the evaluator, the snapshot and the lane reference call the one function,
  each with one table load; the snapshot reports that same table (T4).
- I4 Ledger untouched: Σ granted charges, the head's `bypassed`, the fairness anchor and the
  exclusive gate are written exactly where they are today; a set writes only the table (T14). A
  tier hold never arms the anchor (T23).
- I5 Head rule: a head holds pushed-head-strictly every job-level waiter of a lower tier; a
  same-tier waiter gets today's AIRA-307 budget and backstop; a pushed head holds everything, as
  today; sub-reservations are never held (T10, T11, T23).
- I6 Lane: candidate order ignores tier and job priority; the reference is the evaluation order's
  (§6; T6, T7).
- I7 Honesty: an unestablished table is reported unevaluated, never "none", by the daemon and by
  every client (T16, T17, T17b).
- I8 Identity: the tier key of a waiter is the owner every row displays for it (T24).
- I9 Writes: after any set of concurrent setters returns, the published map equals the stored
  rows, and every acknowledged set is in both (T15b).

## 5. What LOW waiting means on a saturated box (D3)

What the code does, per evaluator pass (`evaluateAdmitQueue`, `admit.go:2546-2830`), with the hold:
1. The lane pre-pass grants eligible small jobs (tier-blind, §6), each only if guard G leaves the
   reference waiter its room.
2. The walk visits waiters in evaluation order. Before a head is set, any waiter that fits is
   granted, so tiers are an order, not a gate, on a box with room: a LOW job alone is granted in the
   same pass (T13).
3. The head is the first visited job-level, ungated waiter that is refused on capacity and could fit
   an empty slice (`admit.go:2805`). After it: a lower-tier job-level waiter is held (new arm); a
   same-tier one is granted only within the head's budget (half its booking) or in the one backstop
   pass per `maxHold` (`admit.go:2609-2624`); a pushed head grants nothing past itself
   (`admit.go:2741`); sub-reservations are granted on fit.

"The head is a LOW waiter that does not fit while a MEDIUM waiter behind it would fit" **cannot
occur within a pass**: every MEDIUM waiter is visited before every LOW one. A MEDIUM job that
arrives while a LOW waiter is head sorts ahead of it on the next pass: if it fits it is granted (a
head protects only the waiters after it); if it does not fit it becomes the head, and the LOW
waiter, now behind a higher-tier head, is held strictly. The displaced LOW head keeps its `bypassed`
count and resumes it if it is head again (today's rule for a head displaced by a push) (T27).

What bounds what:
- The budget and backstop bound how long a head holds jobs of ITS OWN tier behind it.
- Nothing bounds how long a lower-tier job waits behind a higher-tier job, waiting or fitting. If
  higher tiers keep jobs waiting, a LOW job never starts in the main queue. That is D3, intended,
  and it applies equally to MEDIUM behind a busy HIGH session (owner summary).
- Wedge check. The hold is the pushed hold restricted to lower tiers, so it adds no case pushes do
  not already have: head eligibility still needs an empty-slice fit, so an oversized waiter of any
  tier cannot become a head and hold anyone; while a head holds, running jobs only release, the
  head's own tier is bounded by the budget, sub-reservations and lane grants are bounded by guard G
  and the slice, so the head fits after finitely many releases; exclusivity and the restart freeze
  are gated before order and do not read it; a tier change only re-sorts and can only displace
  heads, which pushes already do.

## 6. The small lane and the tiers (D4)

The lane is NOT independent of the ordering function today. `smallLanePassLocked`
(`small_lane.go:153-219`) uses `admitOrderLocked` for two things:
- **candidate order**: which eligible small job gets lane room first when the lane cap (8 GiB) is
  nearly full (the loop at :182);
- **the reference waiter** that guard G protects (`nextRef`, :171-180), and the pushed-reference
  closure (:192).

The design separates them:
- Candidate order becomes tier-blind and priority-blind: pushed first (latest first), then enqueue
  order. That is D4: a LOW session's small job is lane-eligible and competes FIFO; a HIGH session's
  small job gets no lane advantage. It is computed by the same function with tiers and job priority
  neutralised (one implementation of the pushed rule, so the two orders cannot drift on pushes).
- The reference stays the evaluation order's (tier-aware): it is "the waiter today's walk would make
  the head or grant", and G exists to stop small leases taking a head's room. A tier-blind
  reference would protect a waiter the walk does not treat as head and leave the real head (say a
  HIGH job) unprotected, which would make tiers cost HIGH jobs room. The loop's
  `index == reference` test becomes a waiter-identity test, since the two lists differ.

**Where they meet**: guard G. A LOW small job can be granted past a HIGH head only if the HIGH head
still fits beside the small leases (today's G, unchanged). The main-walk hold does not apply in the
lane (D4: "lanes ignore priorities/tiers").

**One visible change for sessions that do nothing** (L5): today the lane walks job-high small jobs
first; under D4 it walks them FIFO. Effect: only when the lane cap is contended, an older ordinary
small job gets the last lane room before a newer `--priority high` small job, which then goes in the
main walk at its job-high place. Main-walk order, and so every non-lane admission, is unchanged.

## 7. Failure modes and the test that kills each

"False order": a job goes ahead when it should not. "False starve": a job is held when it should not
be. Every test must fail against the named wrong implementation; the builder runs that mutation.
Every generated queue (T1, T2) contains pushed waiters, job-high and normal owned waiters,
sub-reservations, scope-less waiters and ownerless-scope waiters.

| # | Dir | Counterexample (wrong implementation) | Test |
|---|---|---|---|
| T1 | order | empty table changes order (any bucket bug), or an absent row defaults to LOW | random queues (generator above), empty table: equal to an oracle copy of today's three-bucket function, element by element |
| T2 | order | untiered waiters tiered, or all-one-tier order mis-sorted | the I1 all-HIGH and all-LOW orders, written out per bucket, asserted element by element |
| T3 | order | job priority used as the primary key | table over {H,M,L} x {job-high, normal} x {pushed, not}: LOW job-high after MEDIUM normal; full expected order asserted |
| T4 | order | snapshot or `--list` order differs from grant order | lane off, every waiter the same size, slice with room for exactly one at a time, mixed tiers: repeated pass-then-release grants waiters in exactly the snapshot's position order; reported `session_tiers` is the table the order used |
| T5 | order | pushed LOW job not first, or pushes not latest-first | two pushed LOW jobs (pushed in order A then B) behind HIGH jobs: grant order B, A, then the HIGH jobs |
| T6 | order | lane walks the evaluation order (HIGH small gets lane room first) | lane room for one: older LOW small job is granted, newer HIGH small job is not lane-granted |
| T7 | starve | lane reference tier-blind | effective maximum 32 GiB, 26 GiB granted, no small leases; queued in enqueue order: MEDIUM 8 GiB, HIGH 31.5 GiB, LOW 1 GiB small. The tier-aware reference is the HIGH job (31.5 + 1 > 32: G fails), so the small job is `kept`; a tier-blind reference is the MEDIUM job (8 + 1 <= 32: G holds) and grants it |
| T8 | order | tier copied at enqueue | enqueue, then mark LOW: next pass reorders and holds; remove the mark: order and hold restored |
| T9 | starve | sub-reservation tiered by its parent | parent owner LOW; queue: the parent's sub-reservation (older), then a MEDIUM job-level waiter, room for one: the sub-reservation is granted, as with an empty table |
| T10 | starve | a LOW head given the strict hold over its own tier | LOW head refused; a fitting LOW job behind it within budget is granted; with the budget exhausted it is withheld and then granted in the backstop pass |
| T11 | starve | a HIGH head strict over its own tier | as AIRA-340's `TestAdmitHighHeadUsesTheBypassBudgetNotThePushedRule`, for a HIGH-tier head and a HIGH waiter |
| T12 | starve | marks lost across a daemon restart | set, restart the server on the same store: a queued client's fresh admit after reconnect is ordered by the mark; an ARDR re-declared (granted) lease is unchanged |
| T13 | starve | tier used as a gate without a head | LOW job alone on an empty slice: granted in the first pass |
| T14 | order | set touches ledger or head state | ledger, `bypassed`, anchor, exclusive gate identical before and after a set; grant-vs-cancel race `-race -count=20` still XOR |
| T15 | order | map published although the store write failed | failing store: reply carries the store's code and exit status, map and order unchanged |
| T15b | order | setters not serialised | injected store hook blocks setter 1 inside its commit; setter 2 (another owner) must not publish before setter 1 does; after both return the map holds both rows and equals the store. Plus N goroutines setting random owners/tiers, `-race -count=20`: final map equals the stored rows |
| T16 | starve | load failure stops the daemon, reports "none", or still accepts sets | corrupt table: daemon serves, order is today's, `session_tiers` absent, `session_tiers_error` set, every set refused `E_CONFINE_UNAVAILABLE` with its exit status |
| T17 | order | empty list dropped by `omitempty` | no marks: `session_tiers` is `[]`, not absent |
| T17b | order | client renders an absent field as "none set" | `--list` and the `aira top` footer, fed a reply without `session_tiers`, print `session tiers: unevaluated` |
| T18 | order | `unknown`, empty, over-long or `=`-less values accepted | each refused `E_CONFINE_ARGUMENT_INVALID`, exit status pinned |
| T19 | order | verb routed to the project transport | CLI `--session-tier` reaches the daemon handler from outside any project (AIRA-309 regression class) |
| T20 | order | cap not enforced, or enforced on re-set/remove | 64 rows: a new owner is refused `E_CONFINE_SESSION_TIERS_FULL` (exit status pinned), no row changed; re-setting and removing an existing row succeed |
| T21 | order | MCP exposes the setter | generated MCP tool list has no session-tier tool; help and agent guide carry the "owner sets, agents do not" sentence |
| T22 | order | `aira top` "(high)" for a job-priority high | queued `--priority high` row in a medium session: SESSION cell unmarked, LIVE cell `high` |
| T23 | starve/order | hold missing, after the backstop arm, applied to sub-reservations, or arming the anchor | MEDIUM head that does not fit; behind it a fitting LOW job, a fitting scope-less job and a fitting sub-reservation: in an ordinary pass with budget unspent, and in the backstop pass, the LOW and scope-less jobs are held (`behind head`), the sub-reservation is granted, `bypassed` and the anchor unchanged. Same with a HIGH head and a fitting owned MEDIUM job |
| T24 | order | tier keyed on the request's owner claim | scope id without owner tail, request owner `field`, `field` marked HIGH: the waiter is MEDIUM and its row shows unowned |
| T25 | order | old daemon's refusal claimed as success or its code replaced | client against a stub answering the verb with a project-dispatch error: exit non-zero with that code's status, message kept, qualified restart hint appended |
| T26 | order | `t` sends the SESSION cell's text | rows whose cell shows a tmux window name and a shortened `cc-` id: the request's `session` is the full owner from the scope-id tail; an ownerless row sends no request |
| T27 | starve | displaced LOW head loses or leaks its budget | LOW head with `bypassed` X; a MEDIUM job that does not fit arrives and becomes head; a fitting LOW job is now held; when the MEDIUM job is granted, the LOW head resumes with `bypassed` X |

Planned mutations (each must turn at least one test red): swap the tier and job-priority keys
(T3); default an absent owner to LOW (T1, T3); tier untiered waiters by the table (T2); read tier
from the parent scope id (T9); key on `waiter.owner` (T24); store tier on the waiter at enqueue
(T8); lane candidates from the evaluation order (T6); lane reference from the tier-blind order
(T7); give a same-tier head the strict arm (T10/T11); delete the hold arm (T23); move it after the
backstop arm (T23); drop the `!isSubReservation()` exemption (T23); let it set `budgetWithheld`
(T23); publish before the store write (T15); drop `tiersMu` (T15b); drop the empty list (T17);
render absent as none (T17b); remove one routing-list entry (T19); send the cell text from `t`
(T26).

## 8. Owner lines (accept or veto, one line each)

- **L1 Minimum.** Accept the table-at-evaluation design of §1; no per-waiter state, no admit-wire
  change, no observe mode. Recommended: accept.
- **L2 Who is untiered.** Sub-reservations, `aira run` jobs and jobs whose scope id has no owner are
  always MEDIUM. So a HIGH session's waiting jobs go before running suites' worker claims (ordered
  after, never held), and every owner marked HIGH is not the same as no marks. Recommended: accept
  (D5 for the first; the others have no owner to key on).
- **L3 Persistence.** Marks persist in `state.db`, at most 64, a 65th refused, nothing removed
  automatically. Recommended: accept. Alternative: in memory only, dropped at every daemon restart
  like `--front` (saves one table and the load path; costs re-marking after every release
  cutover).
- **L4 `@cwd-` keys.** Allowed, with the reply saying the mark covers every unattributed launch from
  that directory. Recommended: accept.
- **L5 Lane order.** Follow D4 literally: lane candidates FIFO, ignoring `--priority high` too, the
  one change to today's behaviour (§6). Recommended: accept. Alternative: keep job priority in the
  lane order and ignore only tiers, so sessions that do nothing see no change at all.
- **L6 Trust and surface.** CLI and `aira top` only; no MCP tool; no ownership guard (marking OTHER
  sessions is the feature, so `--steal` on every use would be ceremony); every set audit-logged and
  shown with its setter. Any agent on this UID can still run the CLI and promote itself, exactly as
  it can pass `--priority high` today; the skill text says agents never set tiers, and misuse is
  visible in `aira top` (`set by`). Recommended: accept.
- **L7 Names.** Verb `confine-session-tier`, flag `--session-tier`, not `--tier` (the store already
  uses "tier" for review tiers), key `t` in `aira top`. Recommended: accept.
- **L8 The hold.** A waiting head holds every lower-tier job in the main queue strictly (§3.2), so
  "session wins" and "low waits" are literal. Example: a MEDIUM 10 GiB job waits for room; a LOW
  4 GiB job behind it that fits now waits too. Recommended: accept. Alternatives: (a) hold only LOW
  (HIGH then only reorders, and a MEDIUM job can start past a waiting HIGH head within its budget);
  (b) no hold, order only (a LOW 4 GiB job starts past the waiting MEDIUM head within half its
  booking and in every 5-minute catch-up pass), which keeps the box busier but makes "low waits"
  false whenever a higher-tier head is waiting.
- **L9 A push beats a tier.** `--front` still goes first whatever the tier, and a session pushing
  its own job needs no `--steal`, so a LOW session (or its agent, through the MCP
  `aira_confine_front` tool) can push its own job past everyone. Recommended: accept (D2 says pushed
  first; a push is one job, visible as `pushed` in `aira top`). Alternative: a push of a LOW
  session's job requires `--steal`.

## 9. Deferrals, each with the trigger that would justify it

| Deferred | Trigger |
|---|---|
| R1 cpu.weight or other runtime effect by tier | a measured case where a LOW session's running job slows a HIGH session's job materially |
| R2 age cap or promotion for LOW | the owner reports a LOW job waiting longer than wanted, or a rant shows it |
| R3 per-ticket tiers | the owner wants a tier to follow a ticket across sessions |
| R4 tier by project | the owner keeps marking every session of one project the same way |
| R5 auto-demotion of heavy sessions | evidence that one session repeatedly holds most of the slice (it is judgement; likely never) |
| R6 an MCP setter | the owner asks an agent to set tiers on his behalf, often |
| R7 tiers for `aira run` | `aira run` jobs are seen queueing (needs an owner on its admit request) |
| R8 a tier following a Claude session across `/clear` | the owner finds re-marking after a `/clear` a real cost (needs a stable session name the harness does not give; `/resume` already restores the resumed id's mark) |

## 10. Release and rollout

- Milestone v0.47. Daemon and client change: cut over with the client install AND
  `systemctl --user restart aira-daemon.service`, after `aira confine --list` and a heads-up to live
  sessions (shared daemon-restart care).
- Sessions that do nothing: no change in the main admission order or hold (T1). The one change is
  §6's lane order for `--priority high` small jobs when the lane is contended (L5).
- Nothing changes for anyone until the owner sets the first mark; that first mark is the
  observe-to-enforce step.
- Peer heads-up at the tagged release (deploy, speed): "The owner can now mark sessions high or low
  from `aira top`; a waiting job holds back every lower-tier session's jobs (except through the
  small-job lane); nothing changes unless a session is marked; agents must not set tiers."
- Skill text (`aira install` template, from the dispatch table): one sentence, "Session tiers are set
  by the owner from `aira top`; agents do not set them."

## Review log

### Round 1 (Sol / Claude lineage, 9 blocking findings) -> round 2

Every finding was checked against master `dc19915c`; all nine hold. Grouped by root cause:

| Group | Findings | Root cause | Closed by |
|---|---|---|---|
| A. Tier is only a sort key | G1 (P1) | the head rule ignored tiers, so budget and backstop let lower tiers start past a waiting higher-tier head (`admit.go:2738-2759`, `2609-2624`); "waits only behind HIGH" and "no escape hatch" were false | **Design change:** one hold arm, lower tier held strictly (§3.2, L8). Deletes the false claims and the §5 "budget helps LOW" line. The lane (`small_lane.go:182-210`) and pushes stay as escapes by the owner's own D4/D2, now stated (summary, L9). Tests T23, T27. |
| B. Two owner sources | P2 identity | the tier read the request's claim while every display and guard read the scope-id tail; `validateAdmitArgs` (`admit.go:3796-3802`) accepts a claim with no tail | **Design change:** key on `ParseConfineScopeID` tail, the one parse every reader uses; tail-less waiters untiered. Closes the family by construction (I8, T24). |
| C. Untiered pinned at MEDIUM | G2 (P1) | fixed-MEDIUM untiered waiters make "all one tier = today" false outside MEDIUM | **Kept, stated:** no design makes it true without tiering running jobs' claims (D5). I1/T2 now write the orders out; L2 and the summary state the HIGH-over-suites effect; generator mandated; mutation map corrected. |
| D. Eviction and lifetime | P2 lifetime, G3 | eviction removed the long-lived named marks persistence exists for; resume claim wrong (`2026-10-25-confine-session-owner-design.md:80-85`: `/clear` mints a new id, `/resume` reuses the existing one) | **Deleted:** automatic eviction, its job count under `queue.mu`, the `evicted` reply field and old T20. Cap is refusal only. Lifetime text rewritten as exact owner-string reuse; R8 retargeted to `/clear`. |
| E. Unserialised writers | P1 concurrency | copy-on-write publish with concurrent connections (`server.go:659`) lets setters lose rows and reorder publication | **Needs code (small):** `tiersMu` around read-copy-commit-publish, readers lock-free, no lock edge with `queue.mu`; I9, T15b. Eviction's removal means a set is one row write, so no multi-statement transaction is needed. |
| F. Wire honesty | P2 compat, T17 gap | an old daemon's unknown verb falls through project dispatch (`server.go:1041`); absent `queued_tier` read as medium | **Deleted:** `queued_tier` and the wait-line change. **Code:** client keeps the daemon's code with a qualified hint (T25); absent `session_tiers` rendered unevaluated client-side (T17b); exit statuses pinned. Protocol 15 kept (finding agreed it is defensible). |
| G. Porous oracles | P2 tests, P2 tests (G) | T2 vacuous or wrong; T4 used snapshot order as grant order (`admit.go:1402` is evaluation order); T7 sizes unset; T9 no competitor; T20 one row; no TUI request or client skew test | T2, T4, T7, T9 rewritten with fixtures; T5 latest-of-multiple pushes; T10 exhausted-budget backstop; T16 failed-load setters; T26 TUI setter route; T27 displaced LOW head; T20 replaced with the cap. |

Convergence: round 1 of at most 3; two design changes (A, B) and two deletions (D, F) close five
of the seven groups; no re-base indicated.
