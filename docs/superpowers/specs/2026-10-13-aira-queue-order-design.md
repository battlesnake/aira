# Admission queue order: manual push to front (AIRA-309) and a bounded small-job bypass (AIRA-307)

Status: built (see §10 for the decisions taken at build time). Release v0.37. Owner decisions (A) and (B) of 2026-10-13 are
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
   - **CPU was checked and is unlikely to be the blocker.** The CPU admission ceiling was 32 slots
     (the journal logs "2 per core x 16 cores"). There were 14-19 admitted jobs, and the default
     declaration is 1 core per job (`DefaultConfineCPUCores`).
     - Jobs that declared more cores cannot be ruled out from the saved data, because the wait line
       carries no CPU figure.
     - A CPU-blocked job would also have had to clear its CPU block exactly at the 19:28:10
       hold-to-yield edge. The timing points to the freeze.
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
- it **could fit an empty slice now, in every resource**:
  - RAM: `reserve <= effectiveMaximum - admitSliceHeadroom(1)`;
  - CPU: `cpu <= cpuCeiling()`;
  - VRAM: `vram == 0`, or the VRAM snapshot is fresh and evaluated and `vram` is within its
    ceiling. An unevaluated or stale VRAM reading means **not a head**.

**The last clause is new.** A job that could not fit even an empty slice at the current, possibly
throttled, ceilings cannot be helped by freezing anyone, so it never freezes. This applies to pushed
and unpushed jobs alike.
- The VRAM arm matters on its own. `vramWaiterFitsLocked` returns false on an unevaluated snapshot.
  Without the arm, a pushed GPU job would freeze the whole machine for as long as nvidia-smi was
  failing, and a pushed head has no backstop.
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
   - **Yield (the single backstop pass):** W is granted if it fits.
   - **Hold or idle phase:** W is granted if it fits **and** `H.bypassed + W.reserve <= H.reserve/2`.
   - Every grant past H, in any phase, adds W.reserve to `H.bypassed`.
   - If W fits but the budget refuses it, the freeze is now actually withholding a grant. That is
     the arm condition: if the phase is idle, set `freezeArmedAt = now`.

The budget is counted in RAM bytes for every head, including one refused on CPU or VRAM. Using bytes
as the proxy is a documented simplification, the same as the existing RAM-only `lastGrantable`
diagnostic.

### 3.5 Interaction with the AIRA-59 duty cycle (decision)

**The budget replaces the time-based backfill windows. A time cap remains only as a backstop.**

**Deleted: `admitBackfillGrace`** (AIRA-4's 1-minute "backfill freely before freezing" window) and
its `AIRA_DAEMON_ADMIT_BACKFILL_GRACE` setting. The budget expresses the same allowance in bytes
instead of minutes.
- Keeping both would let a head be bypassed without limit for its first minute and by up to H/2
  more after that.
- AIRA has no compatibility obligation, so deleting is preferred to layering.
- This goes beyond the owner's decisions and can be vetoed at the plan gate.

**Deleted: the yield window.** `admitFreezePhaseAt` keeps the single anchor and returns `hold` for
`[armed, armed+maxHold)` and `idle` after that.
- There is no longer a `maxHold`-long unrestricted yield.
- The existing completed-cycle clear turns the first pass after a hold into exactly **one**
  backfilling pass (phase = yield for that pass alone, anchor cleared). That clear exists today, and
  it is now the whole backstop.

**Why not keep the 2-minute yield window.** The replay shows it reproduces the starvation it is
meant to bound.
- A full 2-minute strict hold (19:26:09 to 19:28:10) raised ledger room only from about 0 to
  8.5 GB. The 12 GiB head needed 12.9 GB.
- So with a 2-minute hold, the backstop would fire on every big head. Its 2-minute unrestricted yield
  would then refill the slice from the 53 GB backlog, exactly as on 2026-10-12.
- A single pass still breaks any wedge, because everything that fits at that instant is admitted.
  But it hands the saved-up room away only once per hold, not for two minutes.

**Default `maxHold`: 2 minutes, proposed to rise to 5 minutes.** This is an open decision for the
plan gate.
- The basis is the replay's release rate. In the 19:26-19:28 hold, the granted total fell by
  7.3 GB in 2 minutes, about 3.6 GB per minute.
- At that rate, a 12 GiB head starting from a full ledger needs about 3.6 minutes of strict hold.
- In the 19:22-19:24 hold, the granted total was flat for 90 seconds, so the rate varies. 5 minutes
  leaves margin without letting a stuck head freeze others for a long time.
- `AIRA_DAEMON_ADMIT_FREEZE_MAX_HOLD` remains the owner's tuning knob.

**Changed:** the anchor arms when the budget first withholds a fitting grant, instead of when a head
past the grace is first refused.

**Why a backstop is still needed.**
- S13 removed waiter timeouts, so a strict freeze would otherwise end only when its head is granted
  or its client leaves.
- A head can be blocked by something no freeze can drain:
  - the physical floor in `checkedAvailable` (the 2026-09-03 correction to the AIRA-4 spec);
  - a long-lived granted lease, such as a confined dev server;
  - a stream of jobs that each exceed the remaining budget while the head never fits.

**Bounds, one line each:**
- **Head (unpushed):** it is bypassed by at most H/2 bytes before strict protection begins. After
  that, only by one backfilling pass per `maxHold` of strict hold.
- **Everyone else:** the slice is never strictly frozen for longer than `maxHold` without one full
  backfill pass, and the budget phase admits up to H/2 before any freeze starts.
- **Pushed head:** it is never bypassed by new job-level work. It is not covered by the backstop;
  only a later push, a kill or its client leaving ends it. That is the owner's "drain the slice".

**Hazards a pushed head can still hit.**
- **Closed:**
  - by the §3.3 conjunctive empty-slice clause: a job too big for the current ceilings, or a job
    whose VRAM cannot be evaluated;
  - by the §3.4 exemption: a deadlock against sub-reservations.
- **Accepted gaps, not "by construction"** (no freeze can drain these):
  - the physical floor in `checkedAvailable` (slice memory the ledger did not grant);
  - a long-lived granted lease, such as a confined dev server.

  In both cases a pushed head stays frozen until the operator kills it, pushes another job, or the
  blocker goes away. `aira top` shows the row as pushed.

**What changes for heads.** This protects the head differently, not more than today.
- Today a head gets 2-minute strict holds and loses everything in 2-minute yields.
- Under this design it gets:
  1. a bounded bypass first;
  2. then strict protection;
  3. which `maxHold` interrupts only with single backfill passes.
- The incident improves only if `maxHold` is longer than the time releases take to make room for the
  head. That is the reason for the proposed 5-minute default.
- At the current 2-minute default, the replayed 12 GiB head would still be refilled once per hold,
  by a single pass rather than by a 2-minute window. Head starvation would be reduced, not removed.

**The three defects listed in the `freezeArmedAt` comment stay impossible to write.** This is
re-checked against the yield-less `admitFreezePhaseAt`:
1. **Renewing an active hold every pass.** The arm is still guarded by derived phase == idle, and
   the budget refusal only reaches the arm through that guard.
2. **Clearing the anchor when the head fits.** Nothing new writes the anchor. A head granted
   mid-hold leaves the anchor in place, exactly as now, and the next head's budget is its own
   waiter's field.
3. **Re-anchoring on a change of holder.** A new head exhausting its budget during an active hold
   finds phase != idle and does not re-arm.

**The completed-cycle clear still runs only from derived idle.** It now fires on the first pass at
or after `armed + maxHold` rather than at `armed + 2*maxHold`. As today, that pass is the forced
backfilling pass. The next pass with a withheld fitting grant re-arms from idle.

`maxHold <= 0` (disabled) keeps its meaning: the anchor is never written, so once the budget is spent
the strict freeze is unbounded.

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
  - **`aira top` has no owner identity.** It dispatches confine verbs as `ConfineUnknownOwner`
    (`tui_data.go`), which is unattested, so every push from `top` is a steal.
  - The page says so plainly: "aira top has no owner identity; confirming overrides the owner check
    (--steal) for <owner>'s job". Confirming always sends `steal=true`.
  - That page is the explicit steal acknowledgement a human gives in `top`. The ownership check
    without `--steal` still applies to the CLI and MCP.
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
   - Repeat for CPU (`cpu > cpuCeiling`).
   - Repeat for VRAM: with an unevaluated or stale VRAM snapshot, a **pushed** GPU job is not a
     head, and a fitting non-GPU waiter behind it is granted. Mutation: drop the VRAM arm, and the
     queue freezes.
9. **Backstop.**
   - Budget exhausted plus a withheld fitting grant arms the anchor.
   - At `armed + maxHold`, exactly one pass grants freely, and those grants are counted.
   - The pass after that, with a withheld fitting grant, re-arms the strict hold.
   - Mutation: restore the `maxHold`-long yield window, and a second pass inside it grants freely,
     which fails the test.
   - The existing AIRA-59 tests (no renew, no clear-on-fit, no re-anchor, at least one backfilling
     pass) still pass, adapted from grace to budget and from the yield window to the single pass.
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
19. The confirmation always sends `steal=true` and states that `top` has no owner identity. The
    CLI without `--steal` is still refused for another session's job (test 13).

## 7. The from-scratch minimum, and the gap

**Minimum:**
- one push counter and one bypassed-bytes counter on `admitWaiter`;
- one ordering function;
- one fit-plus-budget check in the evaluator;
- one management verb;
- one `top` key.

**Additions beyond it, each with its reason:**
- **Empty-slice head clause (§3.3):** without it, a pushed job larger than the throttled ceiling, or
  one whose VRAM cannot be evaluated, wedges every session. It is one conjunctive predicate, reused
  for unpushed heads, so there is no second rule.
- **Sub-reservation exemption (§3.4):** without it a pushed head can deadlock against running
  `--delegate-ram` suites. One `isSubReservation()` branch, the same as the exclusivity drain.
- **Keeping a time backstop (§3.5):** S13 removed waiter timeouts, so a strict freeze with no
  backstop has no end. It reuses the existing anchor and completed-cycle clear. The yield window is
  removed, so the duty cycle gets smaller.
- **Row-kind default-deny in `top` (§3.7):** turns AIRA-308's "remember to guard" into
  "impossible by default". One field and one check.

**Removed:**
- `admitBackfillGrace` and its setting;
- the `maxHold`-long yield window.

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
- **Strict protection after the budget can stall small jobs for up to `maxHold` at a time,** and then
  they get one backfilling pass.
  - At 5 minutes this is a longer stall per cycle than today's 2-on, 2-off cycle.
  - The trade is deliberate: big heads stop starving.
  - The budget phase in front of the stall is new capacity for small jobs.
  - The `maxHold` default is an open decision (§3.5).
- **A pushed head stays frozen behind the physical floor or a long-lived lease.** These are the
  accepted gaps listed in §3.5.
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

## 10. Build-time decisions and deviations

- **`maxHold` default: 5 minutes** (was 2). Taken in its own commit so it reverts
  cleanly. Basis: §3.5's replay release rate (about 3.6 GB per minute in the
  19:26 hold), so a 12 GiB head starting from a full ledger needs about 3.6
  minutes of strict hold. At 2 minutes every big head would still be refilled by
  the backstop pass once per hold. `AIRA_DAEMON_ADMIT_FREEZE_MAX_HOLD` remains
  the tuning knob.
- **`AIRA_DAEMON_ADMIT_BACKFILL_GRACE` is removed outright**, not refused: a set
  value is now ignored like any unknown variable. Nothing on the box set it.
- **`confine --list` text output still shows no queued rows** (AIRA-308 added
  them to `aira top` and to the JSON only). The `pushed` flag is therefore
  visible in `aira top` (LIVE column reads `pushed`) and in
  `confine --list --json`, not in the text table.
- **`aira top` selection is re-read at the keypress** from the highlighted table
  row, because the table refreshes every tick and tview keeps its highlight by
  row index. The confirmation names the job's *listed* position, which can
  undercount when scope-less reservations are queued ahead.
- **The VRAM empty-slice clause** compares against the card's configured budget
  (or total) less headroom, not against current free VRAM, so a pushed GPU job
  still drains other AIRA GPU jobs (§3.3).
- **Old-daemon fall-through** was checked by reading `server.go`: an unrouted
  `confine-front` reaches `coreForRequest` with an empty scope and is refused
  with an error code; the client renders any such refusal as a failure (pinned by
  `TestConfineFrontOldDaemonRefusalRendersAsAnError`).
