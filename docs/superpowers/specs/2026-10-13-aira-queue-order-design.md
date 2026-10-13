# Admission queue order: manual push to front (AIRA-309) and a bounded small-job bypass (AIRA-307)

Status: design, awaiting plan review. Release v0.37. Owner decisions (A) and (B) of 2026-10-13 are
final and are restated here, not reopened. Builds on AIRA-308 (queued confine rows), merged at
e0f3eeca.

## 1. Problem

Two complaints from the 2026-10-12 queue, from opposite ends of the same mechanism:

- **Small jobs stall behind a big head.** A queued job that does not fit (the "head") freezes every
  job behind it, even jobs that would fit right now. The AIRA-59 duty cycle bounds that freeze to
  2 minutes on, 2 minutes off.
- **Big heads starve anyway.** In the 2-minute yield windows, smaller jobs fill whatever space the
  freeze had saved up, so a 12 GiB head can wait through many cycles.
- **No operator control.** An operator cannot say "this job goes next".

### 1.1 What actually blocked the jobs (replay of the 2026-10-12 incident)

**Sources.**
- The daemon journal, saved to `~/tmp/aira-309-307/daemon-journal.txt`. It holds every freeze-phase
  transition and the throttled-ceiling samples.
- The client wait lines in `~/tmp/dr/pushC.log`, one every 15 seconds.

`~/tmp/gb-cut/` is a v0.34 cutover sandbox and holds no evidence about this incident.

**Anchoring the client lines to clock time.** The pushC job is `make ci` with a 1497M estimated
reserve.
- The log's mtime is 19:41:03. Its trailer says `wall=12m52s`. The difference puts the start of the
  run at 19:28:11.
- Assumption: `wall` is run time after the grant. It matches the slowest test package (758s) plus
  vet and build, so it cannot include a 6-minute admission wait.
- The journal shows a hold-to-yield transition at 19:28:10 for head seq=3419 (reserve 12 GiB). The
  job was granted at that edge.
- Working back at 15 seconds per line, waiting began at about 19:22:10.

**How to read the "slice ceiling" figure.** The ceiling on the client line is already net of
headroom (`confine_manage.go:260`, `effective - admitSliceHeadroom(jobs+1)`). So the room for one
more job is the ceiling minus the granted total.

| Waited | Freeze phase (journal) | Granted / ceiling | Room vs 1497M | Blocker |
|---|---|---|---|---|
| 15-105s | HOLD from 19:22:09 | 48769M / 50624-50880M | 1.8-2.1 GB, fits | **freeze** |
| 120-240s | YIELD 19:24:09-19:26:09 | 50305M / 49920-50688M | at most 0.4 GB | full ledger |
| 255-345s | HOLD 19:26:09-19:28:10 | 43008-45529M / 49792-51520M | 4.2-8.5 GB, fits | **freeze** |

**Finding.**

1. **For the small job, the freeze was the direct blocker for about two thirds of its wait.** It fit
   in both hold windows and was granted at the first yield edge after its last one.
   - The one yield window it sat through fell while the ledger was full, so the duty cycle helped
     neither side.
   - The longest small-job wait in the saved logs is at least 345 seconds. The "1 GiB job waited
     about 20 minutes" in AIRA-307 is **reported as** such; it is not in the saved logs.
   - The 20-30 minute waits in the logs belong to the **12 GiB heads**: seq=3419 at least 23m38s,
     seq=2895 at least 29m8s, seq=2903 at least 30m16s, seq=3667 at least 23m2s. Each of these
     starved through repeated hold and yield cycles.
2. **The throttled ceiling (bound-by=system-pressure) was not the gate during the holds.** It cost
   capacity throughout:
   - effective ceiling 52-56 GiB against 64 GiB configured;
   - effective is approximately `sliceAnon + MemAvailable - freeMin`, quantised.

   Without it, the 12 GiB head would have fit at about 43 GB granted. The throttle is honest: about
   20 GB of the 84 GB box was in use outside the slice.
3. **The root of the "full" slice is reserves held on paper.**
   - The ledger held 43-52 GB granted while the slice's actual anonymous memory (`sliceAnon`) was
     1.6-14.7 GB.
   - At the same time MemAvailable was about 50-66 GB.
   - Admission is declared-only by design (`ledgerCharge` is the declared reserve). Over-declared
     reserves therefore fill the ledger long before memory fills.

**What this design fixes and what it does not.**
- The budget (AIRA-307) fixes finding 1: the small-job stall during holds.
- It replaces the yield windows that starved the heads with a bounded allowance.
- It does **not** fix finding 3. Paper reserves come from the estimator and from unpinned defaults,
  so they are an estimate-accuracy problem. That belongs in its own ticket (see §9), not in queue
  order.

## 2. Owner decisions (restated)

- **(A) AIRA-309: manual push.** `aira confine --front <name|pid|scope-id>` puts a queued job at the
  head of the admission queue.
  - It is owner-checked exactly like `--kill`. Another session's job needs `--steal`.
  - `aira top` gets a key on queued rows, behind a confirmation.
  - With several pushes, the latest push goes first.
  - A pushed job at the head freezes everything behind it until it fits ("drain the slice for this
    job").
- **(B) AIRA-307: small-job bypass.** A queued job may be admitted past a waiting head, but only
  within a budget.
  - The budget is the total of jumpers' reserves: 50% of the head's own requested reserve, counted
    while that job is head and reset when the head is granted.
  - A jumper must fit the free ceiling now **and** the remaining budget.
  - Once the budget is spent, the strict freeze resumes.
  - Nothing bypasses a pushed head except another push.

## 3. Design

### 3.1 One evaluation order

`admitOrderLocked(queue) []*admitWaiter` returns the queued waiters in this order:
1. pushed waiters, latest push first (descending `pushSeq`);
2. then every other waiter in enqueue order, the order `queue.waiters` already holds.

It is derived on every call. `queue.waiters` is never reordered.

**Three readers, one function.** The order must be read in all three places, or the client's
"queue position N of M" line, the queued-ahead total and the `aira top` row order would describe a
queue the evaluator does not run:
1. the evaluator;
2. `admitSliceSnapshotFor` (position, queued-ahead bytes, and the AIRA-308 `queuedEntries`);
3. through those entries, the AIRA-308 rows.

### 3.2 State: two fields on the waiter, nothing on the queue that can outlive one

Both new fields live on `admitWaiter`. They are written only under `queue.mu`: by the evaluator, and
by the push handler for `pushSeq`.

- `pushSeq int64`
  - Zero means not pushed.
  - A push sets it from the queue's existing monotonic `queue.seq` counter (incremented first). Push
    order is therefore a counter, never wall time.
  - Pushing a waiter that is already pushed moves it to the front again.
- `bypassed int64`
  - The reserve bytes granted past this waiter while it was the head. This is the AIRA-307 budget
    counter.

**Crash safety (the AIRA-101 pattern).**
- Both are properties of a waiter, so neither can outlive it.
- A waiter dies with its admission connection or its grant. The new head is a different waiter
  whose counter starts at zero, and that is the whole of the "reset when the head is granted" rule.
  No code path clears it.
- A daemon restart starts with an empty queue map, so pushes and counters are dropped. This
  **fails open**: a re-enqueued job is unpushed with a fresh budget. Re-push it if it still matters.
- No operator-clearable state exists, and no queue-level flag can wedge the slice.
- The queue-level duty-cycle anchor (`freezeArmedAt`) is unchanged in kind. Only its arm condition
  moves (§3.4).

**A head displaced by a push.** If waiter A is head, is displaced by a push, and later becomes head
again, its `bypassed` resumes where it stopped. The counter covers this waiter's whole time as head,
not each separate stretch. That is the stricter reading of "counted since it became head", and it
needs no extra state.

### 3.3 Who is the head

In each pass, the head is the first waiter in `admitOrderLocked` order that meets all of:
- it is not blocked by the exclusivity gate;
- it is not a sub-reservation;
- it is refused on capacity (RAM, CPU or VRAM);
- it **could fit an empty slice now**: `reserve <= effectiveMaximum - admitSliceHeadroom(1)`.

**The last clause is new.** A job larger than the current, possibly throttled, ceiling cannot be
helped by freezing anyone, so it never freezes. This applies to pushed and unpushed jobs alike.
- It closes the "pushed job that can never fit wedges the machine" hole.
  `E_ADMIT_TOO_LARGE` is decided against the unthrottled maximum (`admit.go`, the comment at the
  `admitEffectiveMaximum` call), so such a job can be queued and pushed.
- The job's own wait line already says "larger than the whole slice ceiling, so it is blocked by its
  own size" (`confineOwnReserveNote`).
- Once the ceiling recovers, the job becomes a head again on the next pass.

### 3.4 The grant rule behind the head

For each later waiter W in the pass, in order:

1. **Exclusivity gate.** Unchanged, and evaluated first.
2. **Sub-reservations (`parentScopeID != ""`)** are exempt from every form of freeze and are
   evaluated on fit alone. Their grants are not counted in `bypassed`.
   - This is new for the fairness freeze. It reuses `exclusiveGate.blocks`' drain argument: a
     sub-reservation is an admitted job's internal progress.
   - Without the exemption, a pushed head (which has no yield windows) deadlocks. Running
     `--delegate-ram` suites cannot obtain per-test reservations, so they cannot finish, so their
     leases never release, so the head never fits.
   - Today only the yield windows rescue them.
3. **Pushed head:** W waits. No budget, no yield, no anchor.
4. **Unpushed head H:**
   - **Yield phase:** W is granted if it fits.
   - **Hold or idle phase:** W is granted if it fits **and** `H.bypassed + W.reserve <= H.reserve/2`.
   - Every grant past H, in any phase, adds W.reserve to `H.bypassed`.
   - If W fits but the budget refuses it, the freeze is now actually withholding a grant. That is
     the arm condition: if the phase is idle, set `freezeArmedAt = now`.

The budget is counted in RAM bytes for every head, including one refused on CPU or VRAM. Using bytes
as the proxy is a documented simplification, the same as the existing RAM-only `lastGrantable`
diagnostic.

### 3.5 Interaction with the AIRA-59 duty cycle (decision)

**The budget replaces the time-based backfill windows. The duty cycle remains only as a backstop.**
- **Deleted: `admitBackfillGrace`** (AIRA-4's 1-minute "backfill freely before freezing" window) and
  its `AIRA_DAEMON_ADMIT_BACKFILL_GRACE` setting. The budget expresses the same allowance in bytes
  instead of minutes.
  - Keeping both would let a head be bypassed without limit for its first minute and by up to H/2
    more after that.
  - AIRA has no compatibility obligation, so deleting is preferred to layering.
- **Kept unchanged:** `admitFreezePhaseAt`, the single anchor, the two writes and
  `AIRA_DAEMON_ADMIT_FREEZE_MAX_HOLD`.
- **Changed:** the anchor now arms when the budget first withholds a fitting grant, instead of when a
  head past the grace is first refused.

**Why a backstop is still needed.**
- S13 removed waiter timeouts, so a strict freeze would otherwise end only when its head is granted
  or its client leaves.
- A head can be blocked by something no freeze can drain:
  - the physical floor in `checkedAvailable` (the 2026-09-03 correction to the AIRA-4 spec);
  - a long-lived granted lease, such as a confined dev server;
  - a stream of jobs that each exceed the remaining budget while the head never fits.

**Bounds, one line each:**
- **Head (unpushed):** it is bypassed by at most H/2 bytes before strict protection begins. After
  that, only by the backstop's yield passes.
- **Everyone else:** the slice is never strictly frozen for longer than `maxHold` without a full
  backfill pass, and the budget phase admits up to H/2 before any freeze starts.
- **Pushed head:** it is never bypassed by new job-level work. It is not covered by the backstop;
  only a later push, a kill or its client leaving ends it. That is the owner's "drain the slice".
  The §3.3 empty-slice clause and the §3.4 sub-reservation exemption are what keep a pushed head
  from wedging the slice by construction.

**What changes for heads.** This protects the head differently, not more than today.
- Today a head gets 2-minute strict holds and loses everything in 2-minute yields.
- Under this design it gets a bounded bypass first, then strict protection that ends only at its
  grant or at a backstop cycle.
- In the incident, the 12 GiB heads would have been bypassed by at most 6 GiB and then held
  strictly, instead of being refilled every 2 minutes.

**The three defects listed in the `freezeArmedAt` comment stay impossible to write:**
1. **Renewing an active hold every pass.** The arm is still guarded by derived phase == idle, and
   the budget refusal only reaches the arm through that guard.
2. **Clearing the anchor when the head fits.** Nothing new writes the anchor. A head granted
   mid-hold leaves the anchor in place, exactly as now, and the next head's budget is its own
   waiter's field.
3. **Re-anchoring on a change of holder.** A new head exhausting its budget during an active hold
   finds phase != idle and does not re-arm.

The completed-cycle clear ("at least one backfilling pass per cycle") is untouched.

`maxHold <= 0` (disabled) keeps its meaning: an unbounded strict freeze once the budget is spent.

### 3.6 The verb: `aira confine --front`

- **Usage:** `confine --front <name|supervisor-pid|scope-id> [--steal] [--slice S] [--owner ID]`.
  - Canonical verb `confine-front`.
  - Generated into help and MCP from the dispatch table, with safety `SafetyExecute`. It is not
    destructive: nothing is killed and nothing is charged.
- **Routing:** routed beside `confine-kill` in `server.go` to `confineManagement`. Works the same in
  ci-shim mode, because it is pure queue state.
- **Resolution (daemon, under `queue.mu`):** against **queued** waiters with a scope id only.
  `killConfineWithDeps` cannot be reused, because it matches live cgroup scopes and a queued job has
  none.
  - An exact scope id always matches.
  - A name or supervisor pid (both decoded with `runner.ParseConfineScopeID`) matches only
    non-worker, non-sub-reservation waiters, mirroring the kill selector's worker filter.
- **Outcomes:**
  - 0 matches, and the selector names a granted (running) lease: `E_CONFINE_NOT_QUEUED` ("already
    admitted").
  - 0 matches otherwise: `E_CONFINE_NOT_FOUND`.
  - More than 1 match: `E_SELECTOR_AMBIGUOUS` with the sorted scope ids.
  - A sub-reservation named by its explicit scope id is refused with `E_CONFINE_NOT_QUEUED` and a
    reason of "follows its parent".
- **Owner guard:** a byte-for-byte copy of the kill rule.
  - Both owners must be attested (`ConfineOwnerIsAttested`; an `@`-inferred owner is unattested)
    and must be equal.
  - Otherwise `E_CONFINE_OWNER_UNVERIFIED ... pass --steal`.
  - The rule is shared as one helper with kill, not duplicated.
- **Effect:**
  - `queue.seq++` and `waiter.pushSeq = queue.seq`;
  - `queue.signal()`;
  - one audit log line, `confine-front: pusher=… steal=… target-scope=… target-name=…
    target-owner=…`, written only after success, like `confine-kill`.
  - The reply carries the scope id and the new position (1).
- **Display:**
  - Queued rows gain `pushed` (bool, omitempty).
  - `confine --list` and `aira top` mark pushed rows, which are already in evaluation order (§3.1).

### 3.7 `aira top` key

- **Selection.** Queued rows become selectable, with `ID = scope id` (the same id the row keeps once
  admitted).
- **Row kind.** A new `tableRow.Queued bool` marks them.
- **Default-deny.** The inline-action table gains a row-kind requirement that defaults to **live
  rows only**. `onInlineActionStart` refuses an action whose kind does not match the selected row.
  - So AIRA-308's "a future row action must guard on an empty ID" becomes "a future action targets
    queued rows only by opting in". The AIRA-308 comment is updated.
- **The `f` key ("push to front")** is the only queued-row action. It opens the existing confirmation
  page.
  - The page shows the job's name, owner, requested reserve and position.
  - If the row's owner is not the viewer's attested owner, the page says so ("owned by X; confirming
    overrides the owner check") and confirming sends `steal=true`. That page is the explicit steal
    acknowledgement a human gives in `top`.
- **Stale target.** If the job was admitted between selection and confirmation, the row is now live,
  and the kind check or the daemon's `E_CONFINE_NOT_QUEUED` refuses the action visibly.

## 4. Invariants

1. **The ledger is untouched.** `outstanding`, `cpuOutstanding`, `vramOutstanding` and
   `outstandingJobs` are still written only by `rederiveLedgerLocked`. A push or a budget decision
   only changes which queued waiter is granted next, never what a grant charges.
   `Σ(reserve) <= ceiling - headroom` holds as before.
2. **An unpushed head is bypassed by at most `reserve/2` bytes of new job-level grants** outside the
   backstop's yield passes, across its whole lifetime as head.
3. **A pushed head is never bypassed** by a non-sub-reservation waiter while it can fit an empty
   slice. Only a later push goes ahead of it.
4. **All new state is derived from waiters.** It cannot outlive its waiter, a restart fails open,
   and nothing needs operator clearing.
5. **The order is one function** read by the evaluator, the snapshot and the rows. They cannot
   disagree.
6. **Unchanged precedence:** the restart freeze, the fail-fast latch and the exclusivity gate all
   still come before any queue-order logic.

## 5. Wire and protocol impact

**ProtocolVersion stays 15. Every change is additive:**
- **New verb `confine-front`.** An old daemon reaches the fall-through after the confine-management
  routes. To verify at build time: that path must return a stable error code, never a silent
  success. Add a test that pins it.
- **New optional `pushed` on queued `ConfineRecord` rows.** The confine-list reply decode is lenient
  (no `DisallowUnknownFields`; checked), so an old client ignores it.
- **The confine-admit and ARDR frames are unchanged.** The budget and push state are daemon-internal.

**The behaviour change is daemon-side only, so a client-only install changes nothing.** The new
admission policy takes effect only after a daemon restart, and the rollout must name it (per the
shared daemon-restart care memory).

## 6. Tests required (each must fail against the wrong implementation)

**Evaluator** (fake memory reader and clock, driving passes directly with the goroutine retired):
1. **Budget fill.** Head 24 GiB refused, ample room. Three 4 GiB jumpers are granted (12 GiB), the
   fourth (fits room) waits, and `head.bypassed == 12 GiB`. Mutation: drop the budget term, and the
   fourth is granted.
2. **Budget boundary.** A jumper exactly equal to the remaining budget is granted; one byte more
   waits.
3. **Room is required.** A jumper within budget but not within room waits, and the counter does not
   move.
4. **Reset by succession.** The head is granted. The next head starts at 0, and its budget is its
   own reserve/2.
5. **Pushed head.**
   - A fitting jumper waits, in hold, idle **and yield** phases.
   - The anchor is never armed by a pushed head.
6. **Push order.** Pushing A then B evaluates B, then A, then FIFO. A re-push of A puts A first.
7. **Sub-reservation exemption.** It is granted behind a pushed head and behind an exhausted budget,
   and never counted.
8. **Unfittable head.** A head larger than the empty-slice ceiling freezes nobody. The next waiter
   that fits is granted. When the ceiling rises, it becomes head again.
9. **Backstop.**
   - Budget exhausted plus a withheld fitting grant arms the anchor.
   - After `maxHold`, one yield pass grants freely, and those grants are counted.
   - The existing AIRA-59 tests (no renew, no clear-on-fit, no re-anchor, at least one yield pass)
     still pass, adapted from grace to budget.
10. **Grace deleted.** The setting and its parser are removed, and a test pins that a just-enqueued
    head already limits jumpers to its budget.
11. **Incident replay.** A deterministic scenario using the 19:22 window's figures: ceiling net of
    headroom 50880M, 48769M granted, a 12 GiB unpushed head, and a 1497M jumper. The jumper is
    granted at once (budget 6 GiB). On the old code it waited for the yield. This is the regression
    pin for AIRA-307.

**Snapshot and rows:**

12. Queue position, queued-ahead bytes and the `queuedEntries` order follow `admitOrderLocked`.
    Mutation: iterate `queue.waiters`, and the test fails.

**Verb:**

13. Owner guard:
    - an attested match passes;
    - an `@`-inferred owner is refused;
    - a mismatch is refused;
    - `--steal` passes.

    Also: ambiguity, not-found, not-queued (granted), and a sub-reservation or worker reached by
    name or pid.
14. The audit line appears only on success. A push kicks an evaluation (`signal`).
15. CLI parsing: `--front` is mutually exclusive with `--kill` and `--list`. An unknown-verb reply
    from an old daemon is rendered as an error.

**TUI:**

16. A queued row is selectable. `f` on it opens the confirmation, and confirming dispatches
    `confine-front` with the scope id.
17. `f` on a live row is refused by the kind check.
18. A synthetic live-only action is refused on a queued row (the default-deny is pinned).
19. The confirmation sends `steal=true` only when the row's owner differs from the viewer's, and
    says so.

## 7. The from-scratch minimum, and the gap

**Minimum:**
- one push counter and one bypassed-bytes counter on `admitWaiter`;
- one ordering function;
- one fit-plus-budget check in the evaluator;
- one management verb;
- one `top` key.

**Additions beyond it, each with its reason:**
- **Empty-slice head clause (§3.3):** without it a pushed job larger than the throttled ceiling
  wedges every session. One predicate, reused for unpushed heads, so there is no second rule.
- **Sub-reservation exemption (§3.4):** without it a pushed head can deadlock against running
  `--delegate-ram` suites. One `isSubReservation()` branch, the same as the exclusivity drain.
- **Keeping the duty cycle as a backstop (§3.5):** without a timeout, a strict freeze with no
  backstop has no end. The machinery already exists and only its arm condition moves.
- **Row-kind default-deny in `top` (§3.7):** turns AIRA-308's "remember to guard" into
  "impossible by default". One field and one check.

**Removed:** `admitBackfillGrace` and its setting.

**Not built:**
- a separate queue-level "pushed" flag or list;
- a un-push verb (push another job, or kill);
- push persistence across a daemon restart;
- a head-budget diagnostic on the wait line;
- refusing a push at push time on a ceiling check (§3.3 covers the hazard on every pass).

## 8. Risks

- **A pushed head stalls the slice for as long as it takes running jobs to finish.** That can be a
  12-minute `make ci`. This is the owner's intended cost. `aira top` marks the row as pushed, so an
  operator can see why the slice is frozen.
- **Strict protection after the budget can stall small jobs for up to `maxHold` per cycle,** which
  is the same as today's hold. The budget phase in front of it is new capacity for small jobs, not a
  loss.
- **Sub-reservations behind a big head can slow it.** They are bounded by the lifetimes of running
  suites. Accepted, and noted on the exemption.

## 9. Follow-up (not in this change)

- **Paper reserves (finding 3).** 43-52 GB granted against 2-15 GB used. Admission is declared-only
  by design, so the lever is estimate accuracy:
  - the unpinned default;
  - the p90 prior;
  - pinned `--memory-reserve` values that may be over-declared. The heads in the journal are
    exactly 12 GiB again and again, which suggests a pinned value (not verified).

  This should be its own ticket, measured from the peak-RSS history against the granted reserves. It
  is outside queue order.
