# Session priority tiers: high / medium / low sessions in admission order (AIRA-381)

Status: plan, round 3 (after review rounds 1 and 2; see the Review log), for plan review (Sol or
Astra, plus one independent Claude lineage) and the Fable plan gate (the premise measurement in §1
also needs its one method-and-verdict check, per the 2026-11-19 audit rules). Round 2 triggered the
re-evaluation rule (a round-1 "design change" drew a second blocker of its class); the re-base is
recorded in the log and needs an independent confirmation. Ticket AIRA-381, milestone v0.47.
Correctness-critical (admission queue order): two-loop, Opus builds, Fable and Sol review. Builds on
AIRA-307/309 (queue order, `2026-10-13-aira-queue-order-design.md`), AIRA-340 (`--priority high`),
AIRA-350 (small-job lane, `2026-11-18-fixed-small-lane-design.md`) and AIRA-370 (wait diagnosis).
Code references are to master at `dc19915c`.

## Summary for the owner

You can mark a session HIGH or LOW from `aira top`. Every session you have not marked is MEDIUM.
Apart from one small change to the small-job lane's order (below, L5), nothing changes until you
mark a session. A mark does two things to waiting jobs, and nothing to running ones:
1. **Order.** Waiting jobs are considered high-tier sessions first, then medium, then low. Inside
   each tier a `--priority high` job (hooks, short pre-commit steps) goes before that tier's
   ordinary jobs, oldest first. A job pushed with `--front` goes before everything.
2. **Hold.** When a job is waiting for room at the front of the line, a job of a LOWER tier does not
   start past it in the main queue, even one that would fit now, except in the catch-up pass the
   box already runs once every 5 minutes while it is holding jobs back (today's rule). Jobs of the
   front job's own tier may start past it as today: within half its booking, and in the catch-up
   pass.

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
  before it. If it does not fit yet, those medium jobs start in the main queue only in the 5-minute
  catch-up pass; small jobs (1 GiB or less) can also start through the small-job lane, when they
  leave `field`'s job the room it needs. If that `make ci` runs a `--delegate-ram` suite, only the
  suite's own admission is HIGH: its test workers' claims are MEDIUM (see below).
- You mark a long-running sweep session LOW. Its 4 GiB job waits while any medium job of 10 GiB is
  waiting for room, even when 4 GiB is free, until the next catch-up pass. On a quiet box (nothing
  of a higher tier waiting) it starts at once: LOW means "behind everyone else who is waiting", not
  "only when idle".
- A LOW session's pre-commit hook job (`--priority high`) still waits behind every waiting MEDIUM
  job.
- A small job (1 GiB or less) of any session can still go through the small-job lane: the lane
  ignores tiers and `--priority high`. HIGH gives no lane advantage, LOW no lane penalty.
- Jobs already running are untouched: no job is stopped, slowed or resized by a tier.

What you give up, said plainly:
- **LOW can wait indefinitely.** While medium and high sessions keep jobs waiting, a LOW job starts
  in the main queue only in a catch-up pass, and only if it fits at that moment. No age limit, no
  automatic promotion (your decision D3).
- **A busy HIGH session slows every unmarked session the same way**: while it has a big job
  waiting, medium jobs start in the main queue only in the catch-up pass. Un-mark it to undo.
- **A job waiting only for GPU memory or CPU also holds lower tiers back.** Example: a HIGH job
  waiting two hours for the GPU lets medium jobs that need only RAM start just once per catch-up
  pass, every 5 minutes, for those two hours. (Today the same job lets them start within half its
  booking as well.)
- **Test-suite worker claims are untiered, both ways** (`aitest` workers, `confine-reserve` holds,
  L2). A HIGH session's suite gets priority only for its own admission; its workers' claims are
  MEDIUM, ordered after other HIGH jobs. A LOW session's running suite keeps getting worker room
  whenever it fits, even while HIGH jobs wait, exactly as suites get room past any waiting job
  today. Claims are never held, so a running suite can always finish.
- **A push still beats a tier.** `--front` goes first whatever the session's tier, and a session
  may push its own job without `--steal`, so a LOW session can push its own job past everyone
  (decision D2, owner line L9).
- **A mark follows an owner string exactly**: the string a job's launch puts in its scope id,
  shown in `aira top`'s SESSION cell. A Claude session's launches use `cc-<session id>` read from
  the launching shell's environment. `/clear` gives the conversation a new id, but whether the
  session's later Bash launches pick it up has not been verified (the 2026-10-25 identity study),
  and the `aira` MCP server keeps the id it started with. So after a `/clear` or `/resume`, check
  the SESSION cell of the session's new jobs and mark that string. A stable name
  (`AIRA_CONFINE_OWNER=field`) is the only predictable key.
- **One mark per name.** Every session launching jobs under the same owner name shares one tier.
- **Marks last until the daemon restarts** (a release cutover or a machine restart), like `--front`
  pushes; re-mark after a restart. At most 64 marks at once; a 65th is refused. (L3; the
  alternative is to store marks on disk.)

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

**Greenfield minimum.** An in-memory daemon table `owner -> high|low` (medium is the absence of a
row), read by the ONE ordering function at evaluation time, one comparison in the head rule ("a
lower-tier waiter gets no bypass budget"), plus one verb to set a row and one place to see the
table. Nothing is stored per waiter, so a mark applies to already-queued jobs on the next evaluator
pass and to every future job of that owner. That is this design. The gaps from it, each justified:

| Addition beyond the minimum | Why it stays |
|---|---|
| A 64-row cap, refusal only | Bounds the reply, the footer and the idle-mark rows against a scripted loop. One count of a map the setter already holds. |
| Idle-mark rows in `aira top` | Without them a mark on an owner with no listed jobs could not be seen or removed from `aira top`. Reuses the queued-row machinery (AIRA-308). |
| Tier-blind lane order (D4, L5) | Required by D4. Two lines in the lane pre-pass. |
| A writer mutex for the setter | Two concurrent setters would otherwise lose an acknowledged mark (§3.1). Readers stay lock-free. |

Cut, considered and rejected: a per-waiter tier field (staleness and a second source of order); a
tier on the admit request (the client would need to know its tier); an observe-only daemon mode (an
empty table is today's behaviour, pinned by T1 and T29, so the first mark is the enforce step);
an ownership guard on the verb (L6); automatic eviction (round 1); a `queued_tier` wait-line field
(round 1); a new strict hold kind (round 2: the zero-budget hold reuses the existing duty cycle and
its bound); persistence in `state.db` (round 2: its table, load path, load-failure state and
store-failure path existed only for L3, and it kept dead `cc-` marks forever); client-derived job
counts (round 2: the queued rows are capped at 64).

## 2. Decided by the owner (2026-11-27), recorded, not reopened

- **D1 Identity.** A tier is keyed on the job's owner string, the same string `aira confine` puts
  in the scope id (owner chain `resolveConfineOwner`, `cmd/aira/main.go:2291`: `--owner`, then
  `AIRA_CONFINE_OWNER`, then `cc-<CLAUDE_CODE_SESSION_ID>`, then the worktree id, then an inferred
  `@cwd-<dir>`). Subagents inherit their parent's session id, so they share its tier. "When
  possible": a waiter whose scope id carries no owner is untiered (§3.2).
- **D2 Composition.** Lexicographic key: pushed first (latest push first); then session tier HIGH,
  MEDIUM, LOW; inside a tier, `--priority high` before normal; FIFO (enqueue order) inside each
  bucket. With no marks the order is element-for-element today's (pushed, job-high, rest).
- **D3 Starvation.** LOW simply waits: no age cap, no promotion (§5). The ways a LOW job still
  starts while higher-tier jobs wait are the lane (D4), a push (D2) and today's catch-up pass, which
  is a per-hold time bound shared by every tier, not an age rule for LOW (L8).
- **D4 Small lane.** The lane ignores tiers and job priorities entirely (§6).
- **D5 Running jobs.** Admission order only: no preemption, no reservation change, no cpu.weight
  change.

## 3. Design

### 3.1 The table

An immutable `map[string]sessionEntry` (`tier` is `high` or `low`, plus `set_at_ms`, `set_by`; no
`medium` row ever exists) held by an `atomic.Pointer` on `Server`. Readers (evaluator, snapshot,
lane, list) load the pointer ONCE per call, lock-free, so one evaluator pass or one snapshot sees one
table. In memory only: a daemon start begins with an empty table (today's order), exactly as
`--front` pushes do.

Writers are serialised by one dedicated `tiersMu sync.Mutex`, held for the whole
read-copy-check-publish of a set, so two concurrent setters cannot each publish a copy missing the
other's row. `tiersMu` is never taken while `queue.mu` is held, and `queue.mu` is never taken while
`tiersMu` is held (the reply's job count is read after `tiersMu` is released), so there is no
lock-order edge.

Cap: a set that would add a 65th row is refused with `E_CONFINE_SESSION_TIERS_FULL` (new code, exit
1, a state condition like `E_CONFINE_NOT_QUEUED`), changing nothing. Re-setting or removing an
existing row is always allowed. No row is removed except by a `medium` set or a daemon restart.

Valid keys: anything `runner.ValidateConfineOwner` accepts, except `unknown` (the marker for "no
owner") and the empty string; both are refused `E_CONFINE_ARGUMENT_INVALID`. An inferred
`@cwd-<dir>` key is accepted: it groups every unattributed launch from that directory, and the reply
says so. The key is matched exactly, never by prefix; a typo creates a row for an owner with no
jobs, and the reply's `jobs_now: 0` plus the idle row in `aira top` make that visible.

### 3.2 The ordering function and the hold

**Whose tier.** A waiter's tier key is the owner EMBEDDED in its scope id,
`runner.ParseConfineScopeID(waiter.scopeID)`, the same parse every row display (`--list`,
`aira top`, `confine_kill_queued.go:216`) and the `--front` and kill ownership guards
(`confine_front.go:113`) use. It is NOT `waiter.owner`, the request's claim: `validateAdmitArgs`
(`admit.go:3796-3802`) accepts a claim when the scope id has no owner tail. Keying on the one parse
every reader uses makes the tier and the shown owner the same string by construction. Lookup is
`tiers[key]`, defaulting to MEDIUM.

**Untiered waiters, MEDIUM whatever the table says:**
- a sub-reservation (`isSubReservation()`: aitest workers, `confine-reserve` holds). It is a running
  job's internal progress, so demoting it would act on a running job (D5); it already skips the
  head rule, and so it also skips the hold;
- a scope-less job-level waiter (`aira run`), which carries no owner (deferral R7);
- a scoped waiter whose scope id has no owner tail (shown as unowned everywhere).

Because untiered waiters sit at MEDIUM, "every session in the same tier" equals today's order only
for MEDIUM (the empty table). With every owner HIGH, owned job-level waiters sort before every
untiered one; with every owner LOW, after (I1, T2).

**Order.** `admitOrderLocked(queue)` (`internal/daemon/admit.go:475`) becomes
`admitOrderLocked(queue, tiers)`: one pass over `queue.waiters` sorting queued waiters into seven
buckets (pushed; HIGH/job-high; HIGH/normal; MEDIUM/job-high; MEDIUM/normal; LOW/job-high;
LOW/normal), pushed sorted by descending `pushSeq` as today, the rest in `queue.waiters` order. It
stays the single source for its three readers: the evaluator (`admit.go:2676`), the snapshot
(`admit.go:1405`: position, queued-ahead bytes, `aira top` and `--list` rows) and the lane's
reference waiter (`small_lane.go:157`). Each passes the table it loaded; the snapshot copies that
same pointer out, so the rows, their order and the tiers shown come from one table (T4).

**The hold: a lower tier has a bypass budget of zero.** One new arm in the head-protection switch
(`admit.go:2740`), placed AFTER the backstop arm and before the budget arm:

```go
case head.pushSeq > 0:              // unchanged: strict
case phase == admitFreezeYield:     // unchanged: the backstop pass grants on fit, every tier
case tierOf(waiter) < tierOf(head): // new: a lower tier gets no budget
	fits = false
	budgetWithheld = true
	waiter.heldByHead = true
case <budget exhausted>:            // unchanged: same tier
```

A lower-tier waiter is treated exactly as a same-tier waiter whose budget is spent: it is withheld,
`budgetWithheld` arms the AIRA-307 anchor from idle (`admit.go:2797-2804`), the reported phase is
`hold`, and the one backstop pass per `maxHold` (`admit.go:2609-2624`, default 5 min) grants it on
fit, counting it against `bypassed` like any backstop grant. So the tier hold has no state, phase,
token or bound of its own: it is the existing bounded hold. `protected` is unchanged
(`head != nil && !waiter.isSubReservation()`), so sub-reservations are never held. A waiter of a
higher tier than the head cannot be behind it (it sorts first); only pushes can, and a pushed waiter
is never behind an unpushed head. The job-high / normal split inside one tier keeps today's budget,
as AIRA-340 does.

Accepted side effect, with marks set only: a tier hold arms the queue's anchor earlier than a spent
same-tier budget would, so the backstop pass may come sooner for the head's own tier too. With an
empty table no waiter is ever lower-tier than a head, so the arm never fires (T29).

### 3.3 The set verb and the wire

A new project-less confine management verb, `confine-session-tier`, sibling of `confine-front` and
`confine-resize`, routed through `confineManagement` (`internal/daemon/confine_manage.go`). It must
be added to BOTH client transports: the CLI's `Dispatch` list (`cmd/aira/dispatcher.go:162`) and
`aira top`'s `DispatchPalette` list (`dispatcher.go:523`), plus `cmd/aira/main.go:381`,
`dispatcher.go:223`, `internal/core/routing.go:51/135` and `scope_dir.go:154`. AIRA-309 found a
missing entry once; T19 drives both transports. Args: `session` (target owner), `tier`
(`high|medium|low`), plus the siblings' `owner` (caller) and `slice`.

CLI: `aira confine --session-tier <owner>=<high|medium|low>`; `medium` removes the mark (the one way
to remove it). `=` is outside the owner alphabet, so the split is exact. Reply:
`{session, tier, previous, jobs_now}`, where `jobs_now` is the daemon's exact count of queued and
granted waiters on the slice whose key is `session`, taken from a full walk under `queue.mu`, never
from capped rows. Daemon audit line on success only, like `logConfineFront`:
`confine-session-tier: setter=<caller> session=<owner> tier=<old>-><new>`. ci-shim mode refuses the
verb with `E_CONFINE_UNAVAILABLE` (a CI container is one session).

Error codes, each with `codes.ExitForCode` and pinned by a test (T18, T20, T25):
`E_CONFINE_ARGUMENT_INVALID` (bad value), `E_CONFINE_UNAVAILABLE` (ci-shim),
`E_CONFINE_SESSION_TIERS_FULL` (cap).

Dispatch table (`internal/core/core.go`, beside `confine-front` at :2203): `Include: true` so the
generated help and agent guide describe it; NO `MCPTool` (L6). Its summary says: queue order and
hold only; set by the owner from `aira top`; agents do not set tiers.

Reads, one additive reply field, no request change: `confine-list` gains `session_tiers`, a list of
`{owner, tier, set_at_ms, set_by}`. An empty list is a positive "none set" and is never
`omitempty`-dropped (T17). An ABSENT field means an old daemon: clients render it
`session tiers: unevaluated`, never "none set" (T17b).

**Protocol version stays 15.** Precedent: AIRA-312 added `confine-resize` and AIRA-340/324/323 added
fields without a bump, because a bump refuses every confine during a client/daemon skew while the
old side already fails loudly per request. The admit request and the ARDR re-declare frame are
unchanged; after a daemon restart the table is empty (T12). `TestRunnerDaemonProtocolVersionMatchesTheDaemon`
and the `protocol_test.go` pin stay at 15 and pass unchanged; `protocol.go` gets a comment entry in
the AIRA-340 style. Skew, measured by the builder against the live pre-381 daemon before claiming it:
- new client, old daemon: an unknown management verb falls through to project dispatch
  (`server.go:1041`), so its code is whatever that path answers. The client keeps the daemon's code
  and message verbatim, exits with that code's status, and appends a qualified hint: "if the daemon
  predates session tiers (v0.47), restart aira-daemon.service". Never a claimed success (T25).
- old client, new daemon: the new list field is ignored (the builder confirms the list decoder is
  not `DisallowUnknownFields`).

### 3.4 `aira top`

- **Where a tier shows.** The SESSION cell (`topSessionCell`, `cmd/aira/tui_top.go:1238`) of every
  row, live and queued, gains ` (high)` or ` (low)` when the row's owner has a mark; medium shows
  nothing. A queued row's LIVE cell keeps its words (`pushed`, `high` for `--priority high`, the
  AIRA-370 reason; a tier-held waiter reads `behind head`, the same bounded hold as a spent budget),
  so the two kinds of "high" stay in different columns.
- **Idle marks.** Each mark whose owner has no LISTED row is shown as a dim row after the queued
  rows: `session <owner>: tier low, no listed jobs (set by <setter>, <age> ago)`. The wording is
  "listed" because the queued rows are capped at 64 (`runner.ConfineQueuedRowLimit`); when the
  queue is longer, the existing "showing N of M" line says so (T31). A new row kind like `Queued`,
  whose only action is `t`.
- **Recognising a session.** The SESSION cell shows a chosen name verbatim, else the tmux window
  name, else `cc-` plus 8 hex digits, else the worktree hash prefix (AIRA-277/313). The `t` dialog
  adds the full owner string. It shows no job counts: counts from capped rows could be false, and
  the daemon's exact `jobs_now` comes back in the reply, shown on the result line.
- **The `t` key.** On any row with a valid owner (live, queued or idle-mark), `t` opens a three-way
  choice (h/m/l) and then a confirmation page built like `topFrontConfirm` (`tui_inline.go:129`):
  "Set session field (owner `field`, now medium) to LOW. Its queued jobs will wait behind every
  waiting medium and high job, except in the 5-minute catch-up pass; running jobs are not affected.
  Applies at once, until the daemon restarts." The request's `session` is the row record's full
  owner (`record.Owner`, from the scope-id tail), never the SESSION cell's text. A row whose owner
  is empty or `unknown` answers "this job has no session owner to mark" and sends nothing.
- **Footer.** One line: `session tiers: high field; low cc-1a2b3c4d`, `session tiers: none set`, or
  `session tiers: unevaluated (daemon predates session tiers)`.

### 3.5 `confine --list` and the wait line

`confine --list` prints the same footer line under the slice summary; `--json` carries
`session_tiers`. Queued rows get no new field: a row's tier is its owner's entry in the same reply's
`session_tiers`. The client wait line is unchanged. The AIRA-370 diagnosis gains no token: a
tier-held waiter sets `heldByHead` and reads `behind head`, and the freeze phase reads `hold`,
both true because the tier hold is the existing hold.

## 4. Invariants

- I1 Order: `admitOrderLocked` returns the seven-bucket order of §3.2. With an empty table it equals
  today's three-bucket order element by element, untiered waiters included (T1). With every owner
  HIGH: pushed; owned job-high; owned normal; untiered job-high; untiered normal. With every owner
  LOW: pushed; untiered job-high; untiered normal; owned job-high; owned normal (each FIFO) (T2).
- I2 No per-waiter state: nothing about tiers is written to `admitWaiter`; a mark changes the order
  and the hold of already-queued waiters on the next pass (T8).
- I3 One ordering source: evaluator, snapshot and lane reference call the one function, each with
  one table load; the snapshot reports that same table (T4).
- I4 Ledger: a set writes only the table (T14). A tier hold writes the anchor, `bypassed` and
  `heldByHead` exactly where a spent-budget hold does, and nothing else (T23).
- I5 Head rule: a job-level waiter of a lower tier than an unpushed head has a budget of zero:
  withheld except in the backstop pass. A same-tier waiter has today's budget and backstop; a pushed
  head holds everything, as today; sub-reservations are never held (T10, T11, T23, T29).
- I6 Bound: every hold behind an unpushed head ends within one `maxHold` plus one pass for every
  waiter that fits at the backstop pass, whatever the tiers (T30).
- I7 Lane: candidate order ignores tier and job priority; the reference is the evaluation order's
  (§6; T6, T7).
- I8 Honesty: an absent `session_tiers` is rendered unevaluated, never "none" (T17, T17b); no count
  shown is derived from a capped row list (T31).
- I9 Identity: the tier key of a waiter is the owner every row displays for it (T24).
- I10 Writes: after concurrent setters return, every acknowledged set is in the published map (T15b).

## 5. What LOW waiting means on a saturated box (D3)

What the code does, per evaluator pass (`evaluateAdmitQueue`, `admit.go:2546-2830`):
1. The lane pre-pass grants eligible small jobs (tier-blind, §6), each only if guard G leaves the
   reference waiter its room.
2. The walk visits waiters in evaluation order. Before a head is set, any waiter that fits is
   granted, so on a box with room tiers are an order, not a gate: a LOW job alone is granted in the
   same pass (T13).
3. The head is the first visited job-level, ungated waiter that is refused on capacity (RAM, CPU or
   VRAM) and could fit an empty slice (`admit.go:2805`). After it: a lower-tier job-level waiter is
   withheld (zero budget) and arms the anchor; a same-tier one is granted within the head's budget
   (half its booking); in the backstop pass every fitting waiter is granted; a pushed head grants
   nothing past itself (`admit.go:2741`); sub-reservations are granted on fit.

"The head is a LOW waiter that does not fit while a MEDIUM waiter behind it would fit" cannot occur
within a pass: every MEDIUM waiter is visited first. A MEDIUM job arriving while a LOW waiter is head
sorts ahead of it on the next pass: if it fits it is granted, even when the LOW head's budget is
spent (a head protects only the waiters after it; T28); if it does not fit it becomes the head, and
the LOW waiter behind it is withheld. A displaced head keeps its `bypassed` count and resumes it if
it is head again (today's rule for a head displaced by a push) (T27).

What bounds what:
- The budget bounds how much of a head's own tier starts past it; the backstop pass bounds how long
  ANY waiter behind an unpushed head waits for a pass that grants it on fit (I6).
- Nothing bounds how long a lower-tier job waits in total: if every backstop pass finds no room for
  it, it never starts in the main queue. That is D3, and it applies equally to MEDIUM behind a busy
  HIGH session.
- Wedge check. The tier hold adds no unbounded case because it is the budget hold with a smaller
  budget. Concretely, the case round 2 found against a strict hold: a running MEDIUM job R (14 of
  32 GiB) launches a nested `aira confine` child C of 2 GiB (an ordinary job-level admit with no
  parent relation, `cmd/aira/confine_nested.go:9-14`); a HIGH head H needs 20 GiB. C is withheld
  behind H, but the backstop pass grants it within `maxHold`, so R can finish and H then fits (T30).
  The same bound covers a head refused only on CPU or VRAM. Two unbounded cases remain, both
  existing and both chosen by an operator: a PUSHED head holds a nested child strictly today; and
  with `AIRA_DAEMON_ADMIT_FREEZE_MAX_HOLD=disabled` the budget hold is unbounded by that setting's
  definition (`paths.go:297-301`), so the zero-budget tier hold is too. Head eligibility still needs
  an empty-slice fit, so an oversized waiter cannot hold anyone; exclusivity and the restart freeze
  are gated before order and do not read it.

## 6. The small lane and the tiers (D4)

The lane is NOT independent of the ordering function today. `smallLanePassLocked`
(`small_lane.go:153-219`) uses `admitOrderLocked` for the candidate order (which eligible small job
gets lane room first when the lane cap is nearly full, :182) and for the reference waiter guard G
protects (`nextRef`, :171-180, and the pushed-reference closure, :192). The design separates them:
- Candidate order becomes tier-blind and priority-blind: pushed first (latest first), then enqueue
  order (D4). It is computed by the same function with tiers and job priority neutralised, so the
  two orders cannot drift on pushes.
- The reference stays the evaluation order's (tier-aware): G exists to stop small leases taking the
  room of the waiter the walk will make the head or grant. A tier-blind reference would protect the
  wrong waiter and leave a HIGH head unprotected. The loop's `index == reference` test becomes a
  waiter-identity test, since the two lists differ.

A LOW small job can be granted past a HIGH head only if the HIGH head still fits beside the small
leases (today's G). The main-walk hold does not apply in the lane.

**The one visible change for sessions that do nothing** (L5): today the lane walks job-high small
jobs first; under D4 it walks them FIFO. Only when the lane cap is contended, an older ordinary
small job gets the last lane room before a newer `--priority high` small job, which then goes in the
main walk at its job-high place. Main-walk order and hold are unchanged with no marks (T1, T29).

## 7. Failure modes and the test that kills each

"False order": a job goes ahead when it should not. "False starve": a job is held when it should not
be. Every test must fail against the named wrong implementation; the builder runs that mutation.
Every generated queue (T1, T2, T29) contains pushed waiters, job-high and normal owned waiters,
sub-reservations, scope-less waiters and ownerless-scope waiters.

| # | Dir | Counterexample (wrong implementation) | Test |
|---|---|---|---|
| T1 | order | empty table changes order, or an absent row defaults to LOW | random queues (generator above), empty table: equal to an oracle copy of today's three-bucket function, element by element |
| T2 | order | untiered waiters tiered, or all-one-tier order mis-sorted | the I1 all-HIGH and all-LOW orders, written out per bucket, asserted element by element |
| T3 | order | job priority used as the primary key | table over {H,M,L} x {job-high, normal} x {pushed, not}: LOW job-high after MEDIUM normal; full order asserted |
| T4 | order | snapshot or `--list` order differs from grant order | lane off, equal sizes, room for one at a time, mixed tiers: repeated pass-then-release grants in exactly the snapshot's position order; reported `session_tiers` is the table the order used |
| T5 | order | pushed LOW job not first, or pushes not latest-first | two pushed LOW jobs (A then B) behind HIGH jobs: grant order B, A, then the HIGH jobs |
| T6 | order | lane walks the evaluation order | lane room for one: older LOW small job lane-granted, newer HIGH small job not |
| T7 | starve | lane reference tier-blind | effective maximum 32 GiB, 26 granted, no small leases; queued in enqueue order: MEDIUM 8 GiB, HIGH 31.5 GiB, LOW 1 GiB small. Tier-aware reference is the HIGH job (31.5 + 1 > 32: G fails), so the small job is `kept`; a tier-blind reference (MEDIUM, 8 + 1 <= 32) would grant it |
| T8 | order | tier copied at enqueue | enqueue, then mark LOW: next pass reorders and withholds; remove the mark: order and budget restored |
| T9 | starve | sub-reservation tiered by its parent | parent owner LOW; queue: its sub-reservation (older), then a MEDIUM job-level waiter, room for one: the sub-reservation is granted, as with an empty table |
| T10 | starve | a LOW head given zero budget over its own tier | LOW head refused; a fitting LOW job behind it within budget is granted; with the budget spent it is withheld, then granted in the backstop pass |
| T11 | starve | a HIGH head zero-budget over its own tier | as AIRA-340's `TestAdmitHighHeadUsesTheBypassBudgetNotThePushedRule`, for a HIGH head and a HIGH waiter |
| T12 | order | marks survive, or re-declared leases change, across a restart | set a mark, restart the server: table empty, footer `none set`, ARDR re-declared lease unchanged, re-admitted waiter ordered MEDIUM |
| T13 | starve | tier used as a gate without a head | LOW job alone on an empty slice: granted in the first pass |
| T14 | order | set touches ledger or head state | ledger, `bypassed`, anchor, exclusive gate identical before and after a set (no pass between); grant-vs-cancel race `-race -count=20` still XOR |
| T15b | order | setters not serialised | N goroutines setting distinct owners, `-race -count=20`: every acknowledged set is in the final map; 63 rows plus 2 concurrent new owners: exactly one succeeds, the other is refused `E_CONFINE_SESSION_TIERS_FULL` |
| T17 | order | empty list dropped by `omitempty` | no marks: `session_tiers` is `[]`, not absent |
| T17b | order | client renders absent as "none set" | `--list` and the `aira top` footer, fed a reply without `session_tiers`, print `session tiers: unevaluated` |
| T18 | order | `unknown`, empty, over-long or `=`-less values accepted | each refused `E_CONFINE_ARGUMENT_INVALID`, exit status pinned |
| T19 | order | verb missing from either client transport, or `t` sends the SESSION cell's text | (a) CLI `--session-tier` from outside any project reaches a stub daemon's handler through `Dispatch`; (b) the `t` key, driven through the `aira top` model and `DispatchPalette`, reaches the same stub: rows whose cell shows a tmux window name and a shortened `cc-` id send the full scope-id owner; an ownerless row sends nothing. Deleting the verb from either list turns its half red |
| T20 | order | cap not enforced, or enforced on re-set/remove | 64 rows: a new owner is refused `E_CONFINE_SESSION_TIERS_FULL` (exit status pinned), nothing changed; re-set and remove of an existing row succeed |
| T21 | order | MCP exposes the setter | generated MCP tool list has no session-tier tool; help and agent guide carry the "owner sets, agents do not" sentence |
| T22 | order | `(high)` shown for a job-priority high | queued `--priority high` row in a medium session: SESSION cell unmarked, LIVE cell `high` |
| T23 | starve/order | hold missing, before the backstop arm, applied to sub-reservations or same-tier waiters, or not arming the anchor | MEDIUM head that does not fit; behind it, fitting: a LOW job, a scope-less job, an owned MEDIUM job and a sub-reservation; budget unspent, anchor idle. Ordinary pass: LOW withheld (`behind head`), anchor armed, reported phase `hold`; scope-less and MEDIUM granted, `bypassed` grows by their charges; sub-reservation granted, not counted. Backstop pass (clock at armed + `maxHold`, a second fitting LOW job queued): the expired anchor is cleared, the LOW jobs are granted and counted in `bypassed`, the anchor is not re-armed in that pass. Then with a HIGH head: a fitting owned MEDIUM job and a fitting scope-less job are both withheld |
| T24 | order | tier keyed on the request's owner claim | scope id without owner tail, request owner `field`, `field` marked HIGH: the waiter is MEDIUM and its row shows unowned |
| T25 | order | old daemon's refusal claimed as success, or its code replaced | client against a stub answering the verb with a project-dispatch error: exit non-zero with that code's status, message kept, qualified hint appended |
| T27 | starve | displaced LOW head loses or leaks its budget | LOW head with `bypassed` X; a non-fitting MEDIUM job arrives and becomes head; a fitting LOW job is now withheld; when the MEDIUM job is granted, the LOW head resumes with `bypassed` X |
| T28 | starve | a spent LOW head holds a higher-tier arrival | LOW head with its budget spent and a fitting LOW job withheld behind it; a fitting MEDIUM job arrives: granted on the next pass, LOW head's `bypassed` unchanged |
| T29 | order | the head rule (not only the order) changes with no marks | random queues and passes with an empty table, against an evaluator built with the tier arm removed: identical grants, `bypassed`, anchor and phase per pass; plus a MEDIUM head with fitting scope-less and ownerless-scope waiters within budget: granted |
| T30 | starve | a lower-tier waiter wedged behind a head | running MEDIUM R (14 of 32 GiB); HIGH head H of 20 GiB; R's nested 2 GiB child C (same owner, no parent relation), lane off: C withheld, granted in the backstop pass; R released; H granted. Same with a HIGH head refused only on VRAM and a fitting LOW RAM-only job |
| T31 | order | a count or "no jobs" derived from capped rows | 70 queued; a LOW-marked owner whose only jobs are rows 65-70: idle row reads `no listed jobs`, "showing 64 of 70" present, `t` dialog shows no count, CLI set reply `jobs_now` is 6 |

Planned mutations (each must turn at least one test red): swap the tier and job-priority keys (T3);
default an absent owner to LOW (T1, T3); tier untiered waiters by the table (T2); read the tier from
the parent scope id (T9); key on `waiter.owner` (T24); store the tier at enqueue (T8); lane
candidates from the evaluation order (T6); lane reference tier-blind (T7); give a same-tier head the
zero-budget arm (T10, T11, T23); delete the tier arm (T23); move it before the backstop arm (T23,
T30); make it strict without `budgetWithheld` (T23, T30); drop the `!isSubReservation()` exemption
(T23); drop `tiersMu` (T15b); drop the empty list (T17); render absent as none (T17b); remove the
verb from either client list, or send the cell text from `t` (T19); count from rows (T31).

## 8. Owner lines (accept or veto, one line each)

- **L1 Minimum.** Accept the table-at-evaluation design of §1; no per-waiter state, no admit-wire
  change, no observe mode. Recommended: accept.
- **L2 Who is untiered.** Sub-reservations (suite worker claims), `aira run` jobs and jobs whose
  scope id has no owner are always MEDIUM. So a HIGH session's suite is HIGH only for its own
  admission, its workers' claims ordered after other HIGH jobs; a LOW session's running suite keeps
  getting worker room on fit while HIGH jobs wait, as suites do past any waiting job today; and
  marking every owner HIGH is not the same as no marks. Recommended: accept (D5 for claims; the
  others have no owner to key on).
- **L3 Lifetime.** Marks live in daemon memory, at most 64, a 65th refused, dropped at every daemon
  restart like `--front`. Recommended: accept (the from-scratch minimum). Cost: re-mark the
  sessions you care about after each release cutover. Alternative: store marks in `state.db`; adds
  a table, a start-up load with its own failure state and an unevaluated report, a store-failure
  path on every set and three tests, and keeps marks on dead `cc-` sessions until removed by hand
  (each holds one of the 64 slots and an idle row).
- **L4 `@cwd-` keys.** Allowed, with the reply saying the mark covers every unattributed launch from
  that directory. Recommended: accept.
- **L5 Lane order.** Follow D4 literally: lane candidates FIFO, ignoring `--priority high` too, the
  one change for sessions that do nothing (§6). Recommended: accept. Alternative: keep job priority
  in the lane order and ignore only tiers, so sessions that do nothing see no change at all.
- **L6 Trust and surface.** CLI and `aira top` only; no MCP tool; no ownership guard (marking OTHER
  sessions is the feature); every set audit-logged and shown with its setter. Any agent on this UID
  can still run the CLI and promote itself, as it can pass `--priority high` today; the skill text
  says agents never set tiers, and misuse is visible in `aira top` (`set by`). Recommended: accept.
- **L7 Names.** Verb `confine-session-tier`, flag `--session-tier`, not `--tier` (the store already
  uses "tier" for review tiers), key `t` in `aira top`. Recommended: accept.
- **L8 The hold.** A lower tier gets no bypass budget behind a waiting head: it waits, except in the
  catch-up pass every 5 minutes (§3.2). Example: a MEDIUM 10 GiB job waits for room; a LOW 4 GiB
  job behind it that fits now waits until the next catch-up pass. Recommended: accept: every hold
  stays bounded (I6). Alternatives: (a) strict, no catch-up for lower tiers: "low waits" becomes
  literal, but a nested `aira confine` launched by a running medium job behind a waiting HIGH job
  can wait forever, and so can that HIGH job (§5 example), and a job waiting only for GPU memory
  holds lower tiers without bound; (b) order only, no hold: a LOW 4 GiB job also starts past a
  waiting MEDIUM job within half its booking, which keeps the box busier but makes "low waits"
  false whenever a higher-tier job is waiting.
- **L9 A push beats a tier.** `--front` goes first whatever the tier, and a session pushing its own
  job needs no `--steal`, so a LOW session (or its agent, through the MCP `aira_confine_front`
  tool) can push its own job past everyone. Recommended: accept (D2; a push is one job, shown as
  `pushed`). Alternative: a push of a LOW session's job requires `--steal`.

## 9. Deferrals, each with the trigger that would justify it

| Deferred | Trigger |
|---|---|
| R1 cpu.weight or other runtime effect by tier | a measured case where a LOW session's running job slows a HIGH session's job materially |
| R2 age cap or promotion for LOW | the owner reports a LOW job waiting longer than wanted, or a rant shows it |
| R3 per-ticket tiers | the owner wants a tier to follow a ticket across sessions |
| R4 tier by project | the owner keeps marking every session of one project the same way |
| R5 auto-demotion of heavy sessions | evidence that one session repeatedly holds most of the slice (judgement; likely never) |
| R6 an MCP setter | the owner asks an agent to set tiers on his behalf, often |
| R7 tiers for `aira run` | `aira run` jobs are seen queueing (needs an owner on its admit request) |
| R8 a tier following a Claude conversation across `/clear` or `/resume` | the owner finds re-marking a real cost (needs a stable session name the harness does not give) |
| R9 marks stored across daemon restarts (L3 alternative) | the owner finds re-marking after cutovers a real cost |

## 10. Release and rollout

- Milestone v0.47. Daemon and client change: cut over with the client install AND
  `systemctl --user restart aira-daemon.service`, after `aira confine --list` and a heads-up to live
  sessions (shared daemon-restart care).
- Sessions that do nothing: no change in the main admission order or hold (T1, T29). The one change
  is §6's lane order for `--priority high` small jobs when the lane is contended (L5). The first mark
  the owner sets is the observe-to-enforce step for everything else.
- Peer heads-up at the tagged release (deploy, speed): "The owner can now mark sessions high or low
  from `aira top`; a waiting job holds back lower-tier sessions' jobs except in the 5-minute
  catch-up pass and the small-job lane; marks are dropped at a daemon restart; agents must not set
  tiers. Unmarked: main-queue order is unchanged; the small-job lane now takes `--priority high`
  small jobs in plain arrival order."
- Skill text (`aira install` template, from the dispatch table): one sentence, "Session tiers are set
  by the owner from `aira top`; agents do not set them."

## Review log

### Round 1 (Sol / Claude lineage, 9 blocking findings) -> round 2

All nine held against `dc19915c`. A (tier only a sort key, G1): added a strict hold arm. B (two
owner sources): keyed on the `ParseConfineScopeID` tail (I9, T24). C (untiered at MEDIUM, G2):
kept and stated (I1, T2, L2). D (eviction, lifetime): eviction deleted; lifetime rewritten. E
(unserialised writers): `tiersMu` (T15b). F (wire honesty): `queued_tier` deleted; client keeps the
daemon's code (T25); absent field rendered unevaluated (T17b). G (porous oracles): T2, T4, T7, T9
rewritten; T5, T10, T16, T26, T27 added.

### Round 2 (12 blocking findings, 2 P1 groups) -> round 3

Every finding was checked against `dc19915c`; all hold. Evidence re-read: the head switch and
anchor arm (`admit.go:2738-2804`), the backstop clearing the anchor (`:2609-2624`), the phase report
(`:2844-2857`), nested confine as an ordinary admit (`confine_nested.go:9-14`), the 64-row cap
(`confine_queued.go:22-28`, `admit.go:1435`), the palette allowlist (`dispatcher.go:523`), the
unverified `/clear` env refresh and the MCP server's frozen id (owner study `:95`, `:322`),
`AIRA_DAEMON_ADMIT_FREEZE_MAX_HOLD=disabled` meaning unbounded (`paths.go:297-301`).

**Re-evaluation rule triggered.** Round 1 closed group A with a "design change" (the strict hold),
and round 2 found five blockers of that class: the nested-confine wedge, the unbounded CPU/VRAM
head, the false `idle` phase, and both T23 oracle errors (scope-less held; anchor "unchanged"). All
five were induced by the round-1 fix, as were the persistence costs of group D (round 1 kept
persistence when deleting eviction). Real problem, restated: let the owner reorder sessions and make
lower tiers yield to higher ones, without a new kind of hold. Base design that makes the recurring
families impossible: express the tier hold in the existing hold machinery (zero budget) rather than
copying the pushed arm, and hold marks in memory. Builder-independent confirmation of this re-base
is requested from round 3.

| Group | Findings | Root cause | Closed by |
|---|---|---|---|
| A. New strict hold kind | nested wedge (P1), CPU/VRAM head (P2), phase `idle` (two P2), T23 anchor (P1 part) | the hold copied the pushed arm, an operator's one-off, into a standing rule with no bound, phase or token of its own | **Design change, deletes the new hold kind:** a lower tier has zero budget (§3.2), so it arms the existing anchor, reports `hold`, and is bounded by the existing backstop (I6). Deletes the "never arms the anchor" rule and every claim and test built on it. §5 wedge check rewritten with the nested example; remaining unbounded cases (pushed head, `disabled` setting) are existing operator choices. L8 offers strict and order-only with their costs. T23 rewritten, T30 added. |
| B. Porous or wrong oracles | T23 scope-less (P1), T19 one transport, T26 construction only, no MEDIUM-ahead-of-spent-LOW case (P1) | T23 contradicted §3.2/I5; T19 drove only `Dispatch` | T23 split same-tier grants from lower-tier holds and expects anchor expiry without re-arming; T19 drives both `Dispatch` and `DispatchPalette` through the `t` key (T26 merged); T28 MEDIUM arrival ahead of a spent LOW head; T29 empty-table head rule against an evaluator without the arm. |
| C. Counts from capped rows | idle `no jobs`, `t` dialog counts (two P2) | client counts derived from rows capped at 64 | **Deleted:** client-side counts. Idle rows say `no listed jobs` beside the existing "showing N of M"; the only count is the daemon's exact `jobs_now` (I8, T31). |
| D. Persistence beyond the minimum | P2 | stored marks carried the table, load failure, `session_tiers_error`, store-failure path, T12/T15/T16, and kept dead `cc-` rows forever; the owner never asked for it | **Deleted:** in memory only (§3.1, L3 recommended), persistence offered as the L3 alternative with its cost and R9. T15, T16 removed; T12 now pins marks dropped at restart. |
| E. Owner-facing claims | lifetime (P2), rollout (P2), suite claims (P2) | claims stated beyond the evidence or one-sided | Lifetime stated as emitted owner strings, with the unverified `/clear` refresh and frozen MCP id; "nothing changes" qualified by the lane exception in the summary and §10, including the peer message; suite claims stated both ways in the summary, example 1 and L2. No code. |

Convergence: round 2 of at most 3, with the re-base above. If round 3 still finds a blocker in the
hold (group A) class, the next step is option L8(b), order only, which deletes the hold arm, T23,
T27, T28 and T30.
