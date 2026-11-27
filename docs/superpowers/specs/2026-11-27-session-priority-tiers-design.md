# Session priority tiers: high / medium / low sessions in admission order (AIRA-381)

Status: plan, round 1, for plan review (Sol or Astra, plus one independent Claude lineage) and the
Fable plan gate (the premise measurement in §1 also needs its one method-and-verdict check, per the
2026-11-19 audit rules). Ticket AIRA-381, milestone v0.47. Correctness-critical (admission queue order):
two-loop, Opus builds, Fable and Sol review. Builds on AIRA-307/309 (queue order,
`2026-10-13-aira-queue-order-design.md`), AIRA-340 (`--priority high`), AIRA-350 (small-job lane,
`2026-11-18-fixed-small-lane-design.md`) and AIRA-370 (wait diagnosis). Code references are to
master at `dc19915c`.

## Summary for the owner

You can mark a session HIGH or LOW from `aira top`. Every session you have not marked is MEDIUM,
so nothing changes until you mark one. The mark only changes the ORDER in which waiting jobs are
considered for admission: high-tier sessions' jobs first, then medium, then low. Inside each tier,
a job launched with `--priority high` (hooks, short pre-commit steps) still goes before that tier's
ordinary jobs, oldest first. A job pushed with `--front` still goes before everything.

| Waiting job | Considered |
|---|---|
| pushed with `aira confine --front` (any session) | 1st (latest push first, as today) |
| HIGH session, `--priority high` job | 2nd |
| HIGH session, ordinary job | 3rd |
| MEDIUM session (the default), `--priority high` job | 4th |
| MEDIUM session, ordinary job | 5th |
| LOW session, `--priority high` job | 6th |
| LOW session, ordinary job | 7th |

Examples, on a box where memory is short and jobs queue:
- You mark session `field` HIGH. Its next `make ci` waits only behind pushed jobs and other HIGH
  jobs; it no longer waits behind the 15 medium jobs that queued before it.
- You mark a long-running sweep session LOW. Its queued jobs are now considered after every other
  session's queued jobs. On a quiet box it still starts at once: LOW means "last in line", not
  "only when the box is idle".
- A LOW session's pre-commit hook job (`--priority high`) still waits behind every MEDIUM job.
- A LOW session's small job (1 GiB or less) can still go through the small-job lane exactly as
  before; HIGH gives no lane advantage.
- Jobs already running are untouched: no job is stopped, slowed or resized by a tier.

What you give up, said plainly:
- **LOW can wait indefinitely.** While medium and high sessions keep the box full, a LOW job does
  not start. There is no age limit and no automatic promotion (your decision D3).
- **A Claude session's identity is its session id** (`cc-1a2b3c4d…`). Restart or resume a Claude
  session and it gets a new id, so its mark does not follow it; mark the new one. A session that
  sets a stable name (`AIRA_CONFINE_OWNER=field`) keeps its mark for as long as the name is used.
- **One mark per name.** Every session launching jobs under the same owner name shares one tier.

Marks are kept across a daemon restart and a machine restart (owner line L3).

## 1. The problem, and the greenfield minimum

**Problem.** The owner wants a standing, per-session say in who is admitted first, set by hand,
with a LOW tier the box has never had. Today's levers are per job: `--front` (an operator's one-off
push of one queued job) and `--priority high` (set at launch by hooks and Makefiles). Neither can
demote a session, and neither applies to a session's future jobs.

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
read by the ONE ordering function at evaluation time, plus one verb to set a row and one place to
see the table. Nothing is stored per waiter, so a mark applies to already-queued jobs on the next
evaluator pass and to every future job of that owner. That is this design. The gaps from it, each
justified:

| Addition beyond the minimum | Why it stays |
|---|---|
| Rows persisted in `state.db` (L3) | The stable-name case is the one D1 exists for; losing marks at every release cutover would reset them exactly when the box is busiest. One small table, no per-waiter state. |
| A 64-row cap with eviction of idle rows (L3) | `cc-<uuid>` owners die with their session; without a bound the table grows forever. The eviction is computed at set time from state the daemon already has; no timestamps are tracked. |
| Idle-mark rows in `aira top` (L6) | Without them a mark on an owner with no jobs could not be seen or removed from `aira top`. Reuses the queued-row machinery (AIRA-308). |
| Tier-blind lane order (D4, L5) | Required by D4. Two lines in the lane pre-pass. |

Cut, considered and rejected: a per-waiter tier field (staleness and a second source of order); a
tier on the admit request (would need the client to know its tier, and a restart race); an
observe-only daemon mode (an empty table is already today's behaviour, pinned by test T1, so the
first mark the owner sets is the enforce step); an ownership guard on the verb (L6).

## 2. Decided by the owner (2026-11-27), recorded, not reopened

- **D1 Identity.** A tier is keyed on the job's owner string, the same string `aira confine` puts
  in the scope id (owner chain `resolveConfineOwner`, `cmd/aira/main.go:2290`: `--owner`, then
  `AIRA_CONFINE_OWNER`, then `cc-<CLAUDE_CODE_SESSION_ID>`, then the worktree id, then an inferred
  `@cwd-<dir>`). Subagents inherit their parent's session id, so they share its tier.
- **D2 Composition.** Lexicographic key: pushed first (latest push first); then session tier HIGH,
  MEDIUM, LOW; inside a tier, `--priority high` before normal; FIFO (enqueue order) inside each
  bucket. With no marks the order is byte-for-byte today's (pushed, job-high, rest).
- **D3 Starvation.** LOW simply waits: no age cap, no promotion, no escape hatch (analysis in §5).
- **D4 Small lane.** The lane ignores tiers and job priorities entirely (§6).
- **D5 Running jobs.** Admission order only: no preemption, no reservation change, no cpu.weight
  change.

## 3. Design

### 3.1 The table

In memory: an immutable `map[string]sessionTier` (values `high`, `low`; no `medium` row ever
exists) held by an `atomic.Pointer` on `Server`, replaced whole (copy-on-write) by the set verb.
Each reader loads the pointer ONCE per call, so one evaluator pass or one snapshot sees one table,
and no new mutex is ever taken under `queue.mu` (no lock-order change).

Persisted: a machine-wide `state.db` table, written through the same `s.db` handle
`confine-report` uses for `confine_peak_history`:

```sql
CREATE TABLE IF NOT EXISTS confine_session_tiers (
  owner TEXT PRIMARY KEY, tier TEXT NOT NULL CHECK (tier IN ('high','low')),
  set_at_ms INTEGER NOT NULL, set_by TEXT NOT NULL, CHECK (length(owner) BETWEEN 1 AND 64))
```

At most 64 rows of at most about 150 bytes: under 10 KiB. Read once at daemon start. A set writes
the row (upsert, or delete for `medium`) FIRST, and swaps the in-memory map only after the write
commits; a failed write answers the store's error code and changes nothing (T15). A load failure
at start does not stop the daemon (a tier is never worth refusing all admission): it starts with an
empty table (today's order), logs the error, reports the table unevaluated (§3.3), and refuses
every set with `E_CONFINE_UNAVAILABLE` until the next daemon start, so a mark can never be shown
as set while the stored table is unknown.

Eviction (L3): a set that would add a 65th row first deletes the row with the oldest `set_at_ms`
whose owner has no queued or granted waiter right now; the reply names the evicted row. If every
row's owner has a job, the set is refused with `E_CONFINE_SESSION_TIERS_FULL` (new code, exit 1, a
state condition like `E_CONFINE_NOT_QUEUED`). The job count is taken under `queue.mu`, and the
store write happens after `queue.mu` is released (no SQLite I/O under the evaluator's lock); a job
of the evicted owner that arrives in between is simply MEDIUM, which is what the eviction meant.

Valid keys: anything `runner.ValidateConfineOwner` accepts, except `unknown` (the marker for "no
owner", which names no session) and the empty string; both are refused `E_CONFINE_ARGUMENT_INVALID`.
An inferred `@cwd-<dir>` key is accepted: it groups every unattributed launch from that directory,
and the reply says so. The key is matched exactly, never by prefix, so no selector can be
ambiguous; a typo creates a row for an owner with no jobs, and the reply's job count (`0 jobs now`)
plus the idle row in `aira top` make that visible.

### 3.2 The ordering function

`admitOrderLocked(queue)` (`internal/daemon/admit.go:475`) becomes
`admitOrderLocked(queue, tiers)`: one pass over `queue.waiters` sorting queued waiters into seven
buckets (pushed; HIGH/job-high; HIGH/normal; MEDIUM/job-high; MEDIUM/normal; LOW/job-high;
LOW/normal), pushed sorted by descending `pushSeq` as today, the rest in `queue.waiters` order. A
waiter's tier is `tiers[waiter.owner]`, defaulting to MEDIUM, with two fixed exceptions that are
MEDIUM whatever the table says:
- a sub-reservation (`isSubReservation()`: aitest workers, `confine-reserve` holds). It is a running
  job's internal progress, so demoting it would act on a running job (D5), and it already skips the
  head rule;
- a scope-less job-level waiter (`aira run`), which carries no owner on its admit request (deferral
  R7).

`waiter.owner` is the request's owner, which `validateAdmitArgs` (`admit.go:3755-3803`) binds to the
owner embedded in the scope id, so the tier the evaluator uses and the owner every row displays
are the same string.

It stays the single source for all its readers. Today three call sites: the evaluator
(`admit.go:2676`), the snapshot (`admit.go:1405`: queue position, queued-ahead bytes, `aira top`
and `--list` rows) and the lane's reference waiter (`small_lane.go:157`). Each passes the table it
loaded; the snapshot copies that same table pointer out, so the rows, their order and the tiers
shown beside them come from one table (T4).

### 3.3 The set verb and the wire

A new project-less confine management verb, `confine-session-tier`, sibling of `confine-front` and
`confine-resize`, routed through `confineManagement` (`internal/daemon/confine_manage.go`) and the
same client-side lists (`cmd/aira/main.go:381`, `dispatcher.go:162/223/523`,
`internal/core/routing.go:51/135`, `scope_dir.go:154`; AIRA-309 found a missing entry once, so a
test asserts the verb reaches the daemon: T19). Args: `session` (target owner), `tier`
(`high|medium|low`), plus the siblings' `owner` (caller) and `slice`.

CLI: `aira confine --session-tier <owner>=<high|medium|low>`; `medium` removes the mark (the one way
to remove it: one name, one meaning). `=` is outside the owner alphabet, so the split is exact.
Reply: `{session, tier, previous, jobs_now, evicted}`. Daemon audit line on success only, like
`logConfineFront`: `confine-session-tier: setter=<caller> session=<owner> tier=<old>-><new>`.
ci-shim mode refuses the verb with `E_CONFINE_UNAVAILABLE` (a CI container is one session).

Dispatch table (`internal/core/core.go`, beside `confine-front` at :2203): `Include: true` so the
generated help and agent guide describe it; NO `MCPTool` (L6). Its summary says: queue order only;
set by the owner from `aira top`; agents do not set tiers.

Reads, additive reply fields, no request change:
- `confine-list` gains `session_tiers`: a list of `{owner, tier, set_at_ms, set_by, jobs_now}`. An
  empty list is a positive "none set"; an ABSENT field is unevaluated (old daemon, or load failure
  with `session_tiers_error`). The field is never `omitempty`-dropped when empty (T17).
- The queue-position probe (the client's wait line) gains `queued_tier` for the probing waiter,
  taken at the same match as its position.

**Protocol version stays 15.** Precedent: AIRA-312 added `confine-resize` and AIRA-340/324/323 added
fields without a bump, because a bump refuses every confine during a client/daemon skew while the
old side already fails loudly per request. The admit request and the ARDR re-declare frame are
unchanged, so a version-frozen client/daemon pair across a restart re-declares exactly as today;
after the restart the reloaded table applies to every re-enqueued waiter on its first pass (T12).
`TestRunnerDaemonProtocolVersionMatchesTheDaemon` and the `protocol_test.go` pin stay at 15 and must
pass unchanged; `protocol.go` gets a comment entry in the AIRA-340 style. Skew, measured by the
builder against the live pre-381 daemon before claiming it (AIRA-340 did the same):
- new client, old daemon: `--session-tier` gets the old daemon's refusal of an unknown verb; the
  client renders it as "the daemon predates session tiers; restart aira-daemon.service", exit
  non-zero, never a claimed success. `aira top` and `--list` show "session tiers: unevaluated".
- old client, new daemon: unchanged behaviour; the new reply fields are ignored (the builder
  confirms the list and probe decoders are not `DisallowUnknownFields`).

### 3.4 `aira top`

- **Where a tier shows.** The SESSION cell (`topSessionCell`, `cmd/aira/tui_top.go:1238`) of every
  row, live and queued, gains ` (high)` or ` (low)` when the row's owner has a mark; medium shows
  nothing. A queued row's LIVE cell keeps its words (`pushed`, `high` for `--priority high`, the
  AIRA-370 reason), so the two kinds of "high" stay in different columns.
- **Idle marks.** Each mark whose owner has no rows is shown as a dim row after the queued rows:
  `session <owner>: tier low, no jobs (set by <setter>, <age> ago)`. A new row kind like `Queued`,
  whose only action is `t`.
- **Recognising a session.** The SESSION cell already shows a chosen name verbatim, else the tmux
  window name, else `cc-` plus 8 hex digits, else the worktree hash prefix (AIRA-277/313). The `t`
  dialog adds the full owner string and the job counts, which is the minimum to tell two sessions
  apart.
- **The `t` key.** On any row with a valid owner (live, queued or idle-mark), `t` opens a three-way
  choice (h/m/l) and then a confirmation page built like `topFrontConfirm` (`tui_inline.go:129`):
  "Set session field (owner field: 2 running, 3 queued; now medium) to LOW. Its queued jobs will be
  considered after every medium and high session's jobs; running jobs are not affected. Applies at
  once and is kept across daemon restarts." A row whose owner is empty or `unknown` answers "this
  job has no session owner to mark". No `--steal` clause: the verb has no ownership guard (L6).
- **Footer.** One line: `session tiers: high field; low cc-1a2b3c4d` or `session tiers: none set`,
  or `session tiers: unevaluated (<reason>)`.

### 3.5 `confine --list` and the wait line

`confine --list` prints the same footer line under the slice summary; `--json` carries
`session_tiers`. Queued rows get no new field: a row's tier is its owner's entry in the same reply's
`session_tiers`. The client wait line adds `, session tier low` after `N of M queued` when
`queued_tier` is not medium, and nothing when it is medium or absent.

The AIRA-370 diagnosis code is NOT changed and gains no `tier` token: its tokens name the gate that
refused a waiter in the last pass (`ram`, `behind head`, …), and "a higher tier took the room" is
not a gate the evaluator applies; it is the order. The tier word on the wait line and the position
already say "you are behind higher tiers", from the snapshot, without the diagnosis making a causal
claim it cannot establish.

## 4. Invariants

- I1 Order: for every queue, `admitOrderLocked` returns the seven-bucket order of §3.2; with an
  empty table it equals today's three-bucket order element by element (T1, T2).
- I2 No per-waiter state: nothing about tiers is written to `admitWaiter`; a mark changes the order
  of already-queued waiters on the next pass (T8).
- I3 One ordering source: the evaluator, the snapshot and the lane reference call the one function,
  each with one table load; the snapshot reports that same table (T4).
- I4 Ledger untouched: Σ granted charges, the head's `bypassed`, the fairness anchor and the
  exclusive gate are written exactly where they are today; a set writes only the table (T14).
- I5 The head rule is unchanged: a head of any tier is an ordinary unpushed head with the AIRA-307
  budget and backstop; only a push gets the strict hold (T10, T11).
- I6 Lane: candidate order ignores tier and job priority; the reference is the evaluation order's
  (§6; T6, T7).
- I7 Honesty: an unestablished table is reported unevaluated, never "none" (T16, T17).

## 5. What LOW waiting means on a saturated box (D3)

What the code does, per evaluator pass (`evaluateAdmitQueue`, `admit.go:2546-2830`):
1. The lane pre-pass grants eligible small jobs (tier-blind, §6).
2. The walk visits waiters in evaluation order. Any waiter that fits is granted, unless a head is
   already set and protects it. So tiers are an order, not a gate: a LOW job on a box with room is
   granted in the same pass (T13).
3. The head is the first visited job-level, ungated waiter that is refused on capacity and could fit
   an empty slice (`admit.go:2805`). Every LATER job-level waiter that fits is then granted only
   within the head's budget (half its booking), or in the one backstop pass per `maxHold`
   (`admit.go:2609-2624`); a pushed head grants nothing past itself (`admit.go:2741`).

The case the brief asks about, "the head is a LOW waiter that does not fit while a MEDIUM waiter
behind it would fit", **cannot occur within a pass**: every MEDIUM waiter is visited before every
LOW one, so it is never behind a LOW head. A MEDIUM job that arrives while a LOW waiter is head
sorts ahead of it on the next pass: if it fits it is granted (a head protects only the waiters
after it), and if it does not fit it becomes the head and the LOW waiter waits behind it. The
displaced LOW head keeps its `bypassed` count and resumes it if it is head again (today's rule for a
head displaced by a push). So a LOW head constrains only other LOW waiters, and each MEDIUM or HIGH
waiter is visited before it on every pass.

What bounds what:
- The budget and backstop bound how long a head holds the jobs BEHIND it. They help LOW jobs: a LOW
  job that fits is granted within a MEDIUM head's budget, or in the backstop pass.
- Nothing bounds how long a LOW job waits behind HIGH and MEDIUM jobs that fit. If higher tiers keep
  the box full, a LOW job never starts. That is D3, intended.
- Wedge check: none found. A tier adds no strict hold (only a push has one, unchanged); head
  eligibility still needs an empty-slice fit, so an oversized waiter of any tier cannot become a
  head; exclusivity and the restart freeze are gated before order and do not read it; tier changes
  can only displace heads, and displacing is what pushes already do. No new machinery added.

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
still fits beside the small leases (today's G, unchanged).

**One visible change for sessions that do nothing** (L5): today the lane walks job-high small jobs
first; under D4 it walks them FIFO. Effect: only when the lane cap is contended, an older ordinary
small job gets the last lane room before a newer `--priority high` small job, which then goes in the
main walk at its job-high place. Main-walk order, and so every non-lane admission, is unchanged.

## 7. Failure modes and the test that kills each

"False order": a job goes ahead when it should not. "False starve": a job is held when it should not
be. Every test must fail against the named wrong implementation; the builder runs that mutation.

| # | Dir | Counterexample (wrong implementation) | Test |
|---|---|---|---|
| T1 | order | empty table changes order (any bucket bug) | random queues, empty table: equal to an oracle copy of today's three-bucket function, element by element |
| T2 | order | all HIGH, or all LOW, differs from today | same oracle, table marking every owner HIGH, then every owner LOW |
| T3 | order | job priority used as the primary key | table over {H,M,L} x {job-high, normal} x {pushed, not}: LOW job-high after MEDIUM normal; full expected order asserted |
| T4 | order | snapshot or `--list` order differs from grant order | mixed queue: snapshot positions and rows equal evaluator grant order on a slice with room for one at a time; reported `session_tiers` is the table the order used |
| T5 | order | pushed LOW job not first | push a LOW job behind HIGH jobs: granted first |
| T6 | order | lane walks the evaluation order (HIGH small gets lane room first) | lane room for one: older LOW small job is granted, newer HIGH small job is not lane-granted |
| T7 | starve | lane reference tier-blind | HIGH big head, older MEDIUM big job, LOW small job that would break G for the HIGH head: small job `kept` |
| T8 | order | tier copied at enqueue | enqueue, then mark LOW: next pass reorders; remove the mark: order restored |
| T9 | starve | sub-reservation tiered by its parent | parent owner LOW: its sub-reservation's order equals the empty-table case |
| T10 | starve | a LOW head given the strict pushed hold | LOW head refused; a fitting LOW job behind it within budget is granted |
| T11 | starve | a HIGH head given the strict pushed hold | as AIRA-340's `TestAdmitHighHeadUsesTheBypassBudgetNotThePushedRule`, for a HIGH-tier head |
| T12 | starve | marks lost across a daemon restart | set, restart the server on the same store: a queued client's fresh admit after reconnect is ordered by the mark; an ARDR re-declared (granted) lease is unchanged |
| T13 | starve | tier used as a gate | LOW job alone on an empty slice: granted in the first pass |
| T14 | order | set touches ledger or head state | ledger, `bypassed`, anchor, exclusive gate identical before and after a set; grant-vs-cancel race `-race -count=20` still XOR |
| T15 | order | map swapped although the store write failed | failing store: reply carries the error, order unchanged |
| T16 | starve | load failure stops the daemon, or reports "none" | corrupt table: daemon serves, order is today's, `session_tiers` absent, `session_tiers_error` set |
| T17 | order | empty list dropped by `omitempty` | no marks: `session_tiers` is `[]`, not absent |
| T18 | order | `unknown`, empty, over-long or `=`-less values accepted | each refused with `E_CONFINE_ARGUMENT_INVALID` |
| T19 | order | verb routed to the project transport | CLI `--session-tier` reaches the daemon handler from outside any project (AIRA-309 regression class) |
| T20 | starve | eviction drops a row whose owner has jobs | 64 rows, one idle: the idle one is evicted; none idle: `E_CONFINE_SESSION_TIERS_FULL` |
| T21 | order | MCP exposes the setter | generated MCP tool list has no session-tier tool; help and agent guide carry the "owner sets, agents do not" sentence |
| T22 | order | `aira top` "(high)" for a job-priority high | queued `--priority high` row in a medium session: SESSION cell unmarked, LIVE cell `high` |

Planned mutations (each must turn at least one test red): swap the tier and job-priority keys
(T3); default an absent owner to LOW (T1); read tier from the parent scope id (T9); store tier on
the waiter at enqueue (T8); lane candidates from the evaluation order (T6); lane reference from the
tier-blind order (T7); give a tiered head the `pushSeq > 0` arm (T10/T11); swap the map before the
store write (T15); drop the empty list (T17); remove one routing-list entry (T19).

## 8. Owner lines (accept or veto, one line each)

- **L1 Minimum.** Accept the table-at-evaluation design of §1; no per-waiter state, no admit-wire
  change, no observe mode. Recommended: accept.
- **L2 Who is untiered.** Sub-reservations and `aira run` jobs are always MEDIUM. Recommended:
  accept (D5 for the first; the second has no owner to key on).
- **L3 Persistence.** Marks persist in `state.db`, 64 rows, the oldest idle row evicted when full.
  Recommended: accept. Alternative: in memory only, dropped at every daemon restart like `--front`
  (saves one table and the load path; costs re-marking after every release cutover).
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
| R8 a tier following a resumed Claude session | the owner finds re-marking after a resume a real cost (needs a stable session name the harness does not give) |

## 10. Release and rollout

- Milestone v0.47. Daemon and client change: cut over with the client install AND
  `systemctl --user restart aira-daemon.service`, after `aira confine --list` and a heads-up to live
  sessions (shared daemon-restart care).
- Sessions that do nothing: no change in the main admission order (T1). The one change is §6's
  lane order for `--priority high` small jobs when the lane is contended (L5).
- Nothing changes for anyone until the owner sets the first mark; that first mark is the
  observe-to-enforce step.
- Peer heads-up at the tagged release (deploy, speed): "The owner can now mark sessions high or low
  from `aira top`; a low session's queued jobs go after everyone else's; nothing changes unless you
  are marked; agents must not set tiers."
- Skill text (`aira install` template, from the dispatch table): one sentence, "Session tiers are set
  by the owner from `aira top`; agents do not set them."

## Review log
