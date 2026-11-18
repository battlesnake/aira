# Admission diagnostics (AIRA-353), and why AIRA-352 is proposed for a cut

Status: plan, round 3, for re-review and the Fable plan gate. **The release change is AIRA-353
alone.** AIRA-352 is proposed for a cut, pending the owner's answer (§2, §12 Q2). AIRA-353 touches
the evaluator (`internal/daemon/admit.go`), so it gets the full two-loop: Opus builds, Fable
reviews.

**The owner's decision (2026-11-18).** AIRA-350's small-job lane is held: measure first (353), plus
the small backstop fix (352). This spec designs no lane and builds none. Round 2 showed that 352
cannot meet the brief's own acceptance criterion. That is a property of any such fix, not a defect
to patch, so this spec collapses 352 rather than patching it (§2).

Code references are to `a7484074` (master). The data is not committed, because it names other
sessions' jobs: the journal extract `~/tmp/aira-350/freeze-journal.txt` and the store copy
`~/tmp/aira-350/state-copy.db`. The committed reproduction is `docs/dev/aira-352-pushed-backstops.py`,
with its output in `docs/dev/aira-352-pushed-backstops.out.txt`.

## 0. Summary

- **352: cut from this change (proposed).** The brief requires a proof that "no waiter granted
  today waits longer". That proof is false for the candidate fix, and for any fix that restores the
  lost pass (§2, counterexample). The fix would not have changed the 13:21:07 incident. Its yield
  is an estimate of about 3 of 456 backstop passes. Building it needs the owner's explicit waiver.
- **The corrections are made in this change.** The AIRA-352 ticket and the lane spec's §8 no longer
  call 13:21:07 "the incident's largest single loss".
- **353.** One journal line per waited job-level waiter, written when it is granted or removed. It
  carries the booking, the total wait, the time held while fitting (by the budget, by a push) and
  the budget span. A script reads the journal alone and applies the rule fixed in §3.6. Only the
  span can produce an outcome (DROP). The other figures are reported to the owner.

## 1. The greenfield minimum, and the gap

**353 alone.** To "measure what a lane could address" against the rule in §3.6, each waiter needs
its booking, its wait, and the instant the budget first held it while it fitted. That is one
instant and one line, at the grant or at the removal.

| Addition over the minimum | Why it is needed | Size |
|---|---|---|
| Held time by budget (`budget-ms`) and by push (`push-ms`), counted per pass interval | AIRA-353's named deliverable ("how long each waiter fitted ... but was held"). Reported only; it decides nothing (§3.6). | one struct, two methods, one close loop, one open |
| A line for a waiter that leaves ungranted | Without it, the population is incomplete. One call at the single removal point (`admit.go:3060`). | about 8 lines |
| Outcome `stopped` | A graceful daemon stop would otherwise read as a client closing. The validity check needs the split. | one non-blocking receive |
| Credit only at a pass start, only to a still-queued waiter, at most 30 s | Credit only what the evaluator observed (round-3 findings c and e; round-2 findings 3 and 4). | one conjunct, one `min` |
| Guard tests H6 and H7 | The fields must stay diagnostic, and a guard that cannot fail proves nothing (finding d). | tests only |

## 2. AIRA-352: proposed cut

**The defect, confirmed.** The backstop pass clears the AIRA-59 anchor at its start
(`admit.go:2538`), before the head is known. When the head is pushed, the pushed case comes before
the yield case (`admit.go:2691`), so the pass grants nothing behind it, and the anchor is already
gone. The next budget withholding re-arms a fresh hold (`admit.go:2762-2769`). The candidate fix
deferred the clear to the first pass whose head is not pushed. Its full design is at `5f2e596e` §2.

**What the backstop is documented to guarantee** (queue-order spec §3.5):
- unpushed head: one backfilling pass per `maxHold` of strict hold;
- everyone else: the slice is never strictly frozen for longer than `maxHold` without one backfill
  pass;
- pushed head: "not covered by the backstop", the owner's "drain the slice".

No document says that a push consumes the backstop.

**Why cut it:**
1. **The brief's criterion cannot be met.** The restored pass grants what today's code withholds,
   and that room can be the room an unpushed head behind the push needed. The counterexample below
   was traced against `admit.go:2538`, `2691-2697` and `2762-2769`.
   - Setup: a 64 GiB slice with leases of 49, 6 and 3 GiB (room 6). At the head is a pushed P of
     7 GiB. Behind it are an unpushed H of 8 GiB with its budget spent, and W of 1 GiB.
   - The backstop pass at t=0 is lost to P. The 6 GiB lease ends before t=20, and the 3 GiB lease
     before t=30.
   - Today: at t=20, P is granted (room 5). The phase is idle, so the budget withholds W and arms a
     hold. At t=30 the room is 8, and H is granted.
   - With the fix: at t=20, W is backfilled (room 4). At t=30 the room is 7, which is less than 8,
     so H waits.

   Any fix that grants at the restored pass has this property, so this is an owner decision, not a
   design defect.
2. **It would not have changed the incident it was filed for.**
   - The 13:16:07 hold's backstop pass, at 13:21:07, named the pushed head 14816. The phase was idle
     from 13:21:08, and the next `hold` was at 13:22:22 (journal extract).
   - So no pass from 13:21:08 to 13:22:22 withheld a fitting waiter on budget, since that would
     have armed a hold (`admit.go:2762`). The deferred pass would have been the 13:21:47 pass, which
     granted 14816 and had nothing to backfill.
   - Every grant in the incident is unchanged. The loss was the push itself: the owner's drain.
3. **Its yield is small, and only estimated.** Of 456 backstop passes in 3.5 days, 8 are
   candidates (`aira-352-pushed-backstops.out.txt`).
   - The candidate rule cannot tell a push from a `--priority high` head, or from an older waiter
     that could not be a head (`aira-352-pushed-backstops.py:14-18`). So "the fix changes 3 of
     them" is an estimate, not a floor. 2 more candidates are unresolved.
   - Waiter-hours are not measured. A conditional scenario, and not a bound: if all 74 waiters that
     the three re-arm lines name as also queued (14, 32 and 28, re-checked in the extract) each
     gained 5 minutes, that is 6.2 waiter-hours. That is 3.2% of the 192 retained wait-hours, a
     figure reported by AIRA-350. But a waiter withheld at today's fresh hold need not fit at the
     fix's backfill, so the 5 minutes is not a cap either.
4. **It does not restore the documented bound.** A push freezes everyone behind it, outside the
   backstop (§3.5, "Pushed head"). So with the fix, the strict freeze is the hold plus the push,
   which still exceeds the "everyone else" bound. The fix shortens the wait after a push by up to
   `maxHold`. It does not restore the bound.
5. **It accepts a second regression.** A pushed waiter that drops out of head eligibility for one
   pass (a throttled-ceiling dip, or a stale VRAM snapshot, `admit.go:2770`) is passed by the
   deferred backfill (`5f2e596e` B12).

**If the owner waives the criterion**, the build starts from `5f2e596e` §2, with round 2's
confirmed 352 findings applied first (§11: findings 2, 6, 7, 10, 12 and 13). A lost backstop pass
under an exclusive drain or hold is the same class, and it stays out of scope with 352.

## 3. AIRA-353: held-while-fitting diagnostics

### 3.1 What each figure means

All figures are per job-level waiter, and diagnosis only.
- **`waited-ms`**: from the enqueue to the terminal transition (the grant, the rejection, or the
  removal of a still-queued waiter).
- **`budget-ms`**: the time the waiter fitted RAM, CPU and VRAM and the head's budget withheld it
  (`budgetWithheld`, `admit.go:2694-2696`). This is an attribution of observed waiting, not a
  saving.
- **`push-ms`**: the same, withheld by a pushed head (`admit.go:2691-2692`). It is the owner's drain
  cost, which no lane addresses.
- **`budget-span-ms`**: from the first pass at which the budget held the waiter while it fitted, to
  its terminal transition. It is the first-order saving of an unbounded small-job exemption for this
  one waiter, had it been granted at that first pass with everything else unchanged.
  `budget-ms <= budget-span-ms <= waited-ms` (H1).
- **No figure bounds a lane's net effect, in either direction.**
  - A lane's grants take room from others, and lane jobs that end early free room early.
  - Withheld waiters do not change `outstanding` or `cpuOutstanding` within a pass. So every waiter
    behind the head is judged against the same room, and the held figures overcount joint
    feasibility by up to the queue length (ten 1 GiB waiters all "fit" 2 GiB).
  - A graceful daemon stop undercounts the span (§3.3).

  These errors pull in different directions, so §3.6's thresholds are heuristics, and the spec
  claims no safe direction for them.

### 3.2 The change

- **A struct on `admitWaiter`.** It is a named field `heldFit admitHeldFit`, never embedded. Its
  fields are `openAt time.Time` (the start of the pass that last found the waiter fitting and held),
  `openPush bool` (that pass's cause), `budgetMS` and `pushMS int64`, and `firstBudgetAt time.Time`.
  It has exactly two methods, and neither returns a result:
  - `close(now)`: if `openAt` is set, adds `min(elapsedMilliseconds(openAt, now), heldFitMaxCreditMS)`
    to the open cause's counter, then zeroes `openAt`. `heldFitMaxCreditMS = 30000`
    (`elapsedMilliseconds`, `admit.go:3324`, clamps a negative to 0).
  - `open(now, push)`: sets `openAt` and `openPush`, and sets `firstBudgetAt` on the first budget
    open.
- **The close, the only one.** At the start of every pass, right after `now := s.admitNowTime()`
  (`admit.go:2484`) and before the fail-fast return (`2494`):
  `for _, w := range queue.waiters { if w != nil && w.state == admitQueued { w.heldFit.close(now) } }`.
  There are at most `admitMaxWaiters = 256` waiters (`admit.go:31`). A rejection between passes and
  a removal close nothing.
- **The open.** A local `pushHeld` is set in the pushed case. In the refusal branch, after
  `waiter.waited = true` (`admit.go:2736`):
  `if budgetWithheld || pushHeld { waiter.heldFit.open(now, pushHeld) }`. Both flags imply that
  `fits` was true before the `switch` (`admit.go:2689`).
- **The line at grant.** In the grant branch, after `waitedMS` is set (`admit.go:2718-2720`):
  `if waiter.waited && !waiter.isSubReservation() { logHeldFitLocked(waiter, "granted", waiter.waitedMS) }`.
- **The line at removal.** `releaseAdmitWaiterLocked` (`admit.go:3060`) is the single point that
  removes a waiter from `queue.waiters`. Before the state is set to released, it logs a waiter that
  meets all of these: never granted (state `admitQueued` or `admitRejected`), `waited` set, not a
  sub-reservation, and `queue.server != nil`.
  - **Rejected.** `waited-ms = waiter.waitedMS`, which is frozen at the rejection (`admit.go:2501`,
    `2977`; `confine_kill_queued.go:225`). The outcome is `waiter.outcome`: `failfast`, `saturated`
    or `cancelled`. `saturated` is only the non-blocking snapshot return, because a blocking wait
    has no deadline (`admit.go:1955-1960`).
  - **Still queued.** `waited-ms = elapsedMilliseconds(enqueued, queue.server.admitNowTime())`. The
    outcome is `stopped` if a non-blocking receive on `queue.server.stopping` succeeds (it is closed
    at `server.go:651`, and nil in the seam tests unless a test sets it), else `closed`.
- **`budget-span-ms`**, computed in `logHeldFitLocked` for every outcome:
  `waited-ms - elapsedMilliseconds(enqueued, firstBudgetAt)` when `firstBudgetAt` is set, else 0.
  So the span ends at the same instant as the wait.
- **Volume control.**
  - A granted waiter is never logged again at its release.
  - An immediately granted waiter (`waited == false`) is never logged.
  - Sub-reservations, the aitest workers among them (`worker_admit.go:458`), are never logged.

### 3.3 The accounting rule (round-3 findings c and e; round-2 findings 3, 4 and 5)

- **Credit only what the evaluator observed.** An interval is credited only at a pass start, only to
  a waiter still `admitQueued` there, and only to the verdict of the pass that opened it. Each
  credit is at most 30 s. A waiter queued at a pass start was queued for the whole interval since
  the previous pass: only a pass grants, and a rejection or a removal makes it non-queued.
- **What gets no credit:**
  - time after a pass that did not evaluate the waiter: the restart freeze (`2592-2595`), the
    exclusivity gate (`2604-2621`), an unreadable slice (`2507-2517`), or the fail-fast latch;
  - time after a rejection or a removal. The figures are frozen at the terminal transition, even
    when the handler releases the waiter later (`admit.go:1984-2000`, then the deferred release
    at `1945-1952`).
- **So `budget-ms + push-ms <= waited-ms` always.** The rest of the wait is "other": capacity, the
  gate, the restart freeze, or unobserved time.
- **The error.**
  - **Over-credit.** At most one interval per change of verdict, of at most 30 s. The expected gap
    is the poll interval: 250 ms by default, configurable in [250ms, 10s) (`paths.go:47`, `291`).
    Nothing bounds the pass rate: the ticker only signals (`admit.go:2436-2458`), and a pass takes
    `queue.mu`. So 30 s is the cap, not the expected error. It is three times the largest accepted
    poll interval, so a normal gap is never cut.
  - **Under-credit.** For every ungranted waiter, the interval from its last pass to its terminal
    transition. Plus, for every interval, any excess over 30 s.
- **A graceful stop splits a wait.**
  - What happens: Serve closes `stopping` (`server.go:651`). The handler returns
    (`admit.go:1976-1977`), and its deferred release logs each queued waited waiter as `stopped`.
    The client reconnects and re-enqueues as a new waiter (`internal/runner/admission_linux.go:253`,
    `327-335`), which gets its own line.
  - Effect: W and B survive, split over the two lines. S restarts in the new segment, so each split
    undercounts S by at most the restart gap plus the new segment's time before its first budget
    hold. That biases the rule toward DROP.
  - Frequency: the cutover is one such stop. This box's journal shows 12 graceful restarts in the 4
    days to 2026-11-18 (systemd's `Stopped aira-daemon.service`, reported from one query).
- **An unclean end loses the waits in flight.** A crash, a SIGKILL or a host restart writes no line.
  This box's journal has 3 `Started` messages with no `Stopped` since the previous one, between
  2026-10-27 and 2026-11-18 (WSL restarts). It has no `Failed with result` message. The daemon-crash
  pattern is therefore assumed, not verified.

### 3.4 The line, its modes and its volume

```
aira daemon: held-fit seq=<n> outcome=<granted|saturated|cancelled|failfast|closed|stopped> booking=<bytes> waited-ms=<n> budget-ms=<n> push-ms=<n> budget-span-ms=<n>
```

- `booking` is `ledgerCharge()`: the unit of the budget and of the old lane's eligibility.
- **Both modes, with no branch.** No decision reads the line in either mode. In ci-shim mode the
  daemon logs to its own `shim-daemon.log` (`internal/install/mode.go:360-376`), not to CI output.
  A branch would cost a conjunct and a test for no reader, and the freeze transitions are already
  logged in both modes (`admit.go:2784`).
- **Volume.** One line per waited job-level waiter, never one per pass. Both figures are estimates
  from proxies:
  - floor: the store retained 4145 confine rows with `wait_ms >= 10 s` in 84 h, about 1,200 a day;
  - ceiling: every enqueue, sub-reservations included: 7,300-17,800 a day by the seq rate per
    daemon process in the journal extract.

### 3.5 Tests (AIRA-228 seam)

**Setup.** `queueOrderServer`, `queueOf`, `heldLease` and `queuedWaiter`
(`admit_queue_order_test.go:22-50`, `admit_freeze_test.go:30-32`): a 64 GiB slice, zero headroom,
`maxHold` = 1 minute, and the clock stepped by hand. The log is captured as in
`admit_freeze_test.go:653-705`.

| # | Setup | Assert | Mutations it kills |
|---|---|---|---|
| H1 meaning | W is enqueued at t=-5, behind an unpushed head with its budget spent. t=0: it fits and is withheld. t=10: refused on capacity. t=20: it fits and is withheld. The head is removed before t=30, and W is granted at t=30. | One line: `outcome=granted waited-ms=35000 budget-ms=20000 push-ms=0 budget-span-ms=30000`. | Right-endpoint attribution (10000). Held counted from the first held pass (30000). Span from the enqueue (35000). |
| H2 cause split | W fits behind a pushed P at t=0 and t=10. P is granted at t=20, and H (budget spent) withholds W at t=20. H is removed before t=30, and W is granted at t=30. | `push-ms=20000 budget-ms=10000 budget-span-ms=10000`. | One counter (30000). The causes swapped. |
| H3 unevaluated passes | W is withheld by the budget at t=0. At t=10 the memory read fails. At t=20, (a) the restart freeze or (b) an exclusive drain blocks it. All are gone before t=30, and W is granted at t=30. | `budget-ms=10000 waited-ms=30000`. | A close inside the waiter loop: the t=10 pass returns early, giving 20000. A close only in the refusal and grant branches: 30000. |
| H4 terminal freeze | W is enqueued and withheld at t=0 and t=10. It ends by: (a) `timeoutAdmitWaiter` at t=14; (b) a queued kill at t=14 (`cancelQueuedConfineLocked`), then a pass at t=15; (c) client EOF at t=14 (`releaseAdmitWaiterAnchored` with its own conn); (d) `server.stopping` closed, then a release at t=14; (e) the fail-fast latch at the t=14 pass. (a), (b) and (e) are released at t=19. | (a) to (d): `budget-ms=10000 waited-ms=14000 budget-span-ms=14000`, outcome `saturated`, `cancelled`, `closed` or `stopped`. (e): `budget-ms=14000 waited-ms=14000 budget-span-ms=14000`, `failfast` (the t=14 pass credits before it rejects). | No removal line. A close at removal (14000, or 19000). The close loop without the queued filter: (b) gives 15000. `waited-ms` recomputed at removal: 19000. No `stopped` outcome: (d) reads `closed`. |
| H5 exactly one line, only job-level | An immediate grant. A waited grant, released later. A waited sub-reservation, granted and then released. | Exactly one line, for the waited grant, at its grant. | A line on every grant. A second line at release. Sub-reservations logged. |
| H6 guard, structural | An AST scan, as in `admit_pin_charge_guard_test.go`. | Every `.heldFit` selector is on an allow-list keyed by function, with exact counts: `evaluateAdmitQueue` 2, `logHeldFitLocked` n. `admitHeldFit` has exactly the methods `close` and `open`, with no results. No function takes or returns `admitHeldFit`, and it is embedded nowhere. | `fits = fits && waiter.heldFit.budgetMS == 0` in `evaluateAdmitQueue`, an unlisted selector. A method on `admitHeldFit` that returns a value. A new reader function. |
| H7 guard, differential | Two scenarios, each run twice: once as is, and once with every waiter's `heldFit` poisoned before each pass (`openAt` and `firstBudgetAt` = now - 1h, counters at `1 << 40`). Scenario (a): one 52 GiB lease (room 12). H is 16 GiB with its budget spent; W1 is 1 GiB and W2 is 2 GiB. Passes at t=0 (arm), t=30 (hold) and t=61 (backstop: W1 and W2 granted). W3 (1 GiB) arrives at t=62 and re-arms. Scenario (b): H2's passes. | The same granted set, `bypassed`, anchor and `freezeHolderSeq` after every pass. | The H6 mutation, through any path. |
| H8 unobserved gap | The duty cycle is off (`admitFreezeMaxHold = 0`), so no backstop pass intervenes. W is behind an unpushed head with its budget spent. It is withheld at t=0, t=30 and t=61 (a 31 s gap). The head is removed before t=71, and W is granted at t=71. Separately, W' is withheld at t=0, and the next pass is at t=3600, after its head is removed; W' is granted there. | W: `budget-ms=70000`, which is 30000 + 30000 + 10000. W': `budget-ms=30000`. | No cap: 71000 and 3600000. Dropping instead of capping: 40000 (10000 with a strict `<`). |

The gate runs each named mutation with `docs/dev/aira-353-mutation-check.py` (on the pattern of
`docs/dev/aira-324-mutation-check.py`) and records it RED.

### 3.6 The analysis script, and the decision rule fixed before the data

**The script.** `docs/dev/aira-353-held-fit.py` reads the output of
`journalctl --user -u aira-daemon.service -o short-unix --since S --until U`, or a saved extract.
The same query carries systemd's `Started`, `Stopped` and `Failed with result` messages for the
unit. `--since` and `--until` are both required. The script reads the journal only: no store and
no row join.
- **Per class** (small: `booking <= 1 GiB`; large: the rest). It reports line counts by outcome
  and, in hours, `W` = Σ `waited-ms`, `B` = Σ `budget-ms`, `S` = Σ `budget-span-ms` and
  `P` = Σ `push-ms`.
- **It reports separately:**
  - the `stopped` lines (count, W and S);
  - the number of graceful stops (`Stopped` messages);
  - unclean ends: a `Started` with no `Stopped` since the previous `Started`, or any
    `Failed with result` message;
  - lines whose start (the line's time minus `waited-ms`) is before `--since`.
- **`--self-test`** runs the script on embedded synthetic lines with known totals, including both
  unclean-end patterns, so that a parsing error shows.

**The rule.** The owner may change any number marked (O) before the cutover, never after.
1. **Window.** `S` is the cutover's `Started` instant. `U` is the `Stopped` instant of the first
   graceful stop at least 7 days (O) later. So no wait is in flight at either end: each one ends in
   a line inside the window or a `stopped` line.
2. **Population.** The lines in the window. The decision class is small jobs, `booking <= 1 GiB`
   (O), AIRA-350's `y`.
3. **Unevaluated** (the owner decides; there is no verdict) if any of these holds:
   - a daemon process wrote a `fairness-freeze` line but no `held-fit` line (an old binary);
   - a line starts before `S`;
   - the cut share is above 5% (O). All its terms are small-class. The cut share is the
     stopped-line W, plus the unclean ends times the stopped-line W per graceful stop, over the
     class's W. A graceful stop's `stopped` lines measure the waits in flight at one instant, which
     is what an unclean end loses instead. `U` is a graceful stop, so the divisor is never zero.
4. **DROP the lane** (close AIRA-350 and report) if `S < 10%` of `W` (O) or `S < 10 h` (O). The
   thresholds are heuristics (§3.1).
5. **Otherwise, there is no outcome.** W, B, S and P per class go to the owner for AIRA-350. B and P
   never decide anything: they are reported because AIRA-353 asks for the split.

**Not reused.** AIRA-350's association figure (about 5 of 115.6 small wait-hours, from
retention-capped rows) is not a forecast. Nor are the suite-parent figures (round-3 finding f).

## 4. Invariants

1. **No admission decision reads `heldFit`** (H6, H7). AIRA-353 changes no grant, ledger, head,
   `bypassed`, anchor or existing log line. Every existing test passes unchanged.
2. **Only observed time is credited.** Credit is given only between two pass starts at which the
   waiter was queued, and is at most 30 s each. So `budget-ms + push-ms <= waited-ms`, and
   `budget-ms <= budget-span-ms <= waited-ms` (H1-H4, H8).
3. **Exactly one line per waited job-level waiter**, at its grant or at its removal. Its figures are
   frozen at the terminal transition (H4, H5).

## 5. Risks

- **R1. Volume** (§3.4).
- **R2. Diagnosis leaking into decisions** (H6, H7).
- **R3. A window that ends `unevaluated`.** WSL restarts occur: 3 in about three weeks of this
  journal. Each one costs the waits in flight. If the cut share fails, the owner either reruns the
  window or reads the figures anyway.

## 6. Expected yield

| Change | What it addresses | Share | Basis |
|---|---|---|---|
| 353 | No decision changes. It measures `S` (DROP or not) and reports `B`, `P` against `W`, so AIRA-350 is decided on the figures. | — | — |
| 352 (cut) | Backstop passes lost to a pushed head | about 3 of 456 backstop passes in 3.5 days; waiter-hours unmeasured (§2) | estimated |
| 352 (cut) | The 13:21:07 incident | none | measured (§2, item 2) |

## 7. Deferrals

- **AIRA-352**, and a backstop pass lost to an exclusive drain or hold: cut, pending the owner (§2).
- **AIRA-350's lane:** held until §3.6's rule is read.
- **A CPU diagnostic for capacity refusals.** This is the pre-existing gap at `admit.go:2729-2735`:
  "other" time cannot yet be split into RAM, CPU and VRAM.
- **Waits lost to an unclean end:** accepted. The cut share bounds their effect (§3.6, rule 3).

## 8. Cut list

| Option | Why cut |
|---|---|
| AIRA-352 in this change | §2. |
| Building any lane | Owner decision (AIRA-350 held). |
| A room-consistent held counter | It would not make B sufficient: it ignores changed completion times (round-2 finding 8). B decides nothing. |
| A TRIAL threshold on B | B cannot decide alone, so the threshold was decorative (round-2 finding 14). |
| A per-pass log line | Passes reach 4 a second. The per-waiter line carries the totals. |
| A store column or telemetry row | History rows are retention-capped and carry no scope id (round-3 finding b). |
| Closing intervals at a rejection or removal | It adds two sites, plus the removal. Dropping the last interval costs at most one capped interval per ungranted waiter (§3.3). |
| Dropping, rather than capping, a long interval | A discontinuity at the bound, for no gain in honesty. |
| Logging immediate grants, or sub-reservations | They add zero to both sums, or are exempt from the head rule and would multiply the volume. |
| A ci-shim branch | It has no reader, and costs a conjunct and a test. |
| A daemon shutdown log line | systemd's `Stopped` message already marks a clean end, in the same journal query. |
| The old D1-D5, and §M.6's single-review path | D1 pinned the wrong meaning, and D5 could not fail. This is an evaluator change, so it gets the full two-loop. |

## 9. Files, review and rollout

- **Files.**
  - `internal/daemon/admit.go`: the struct, the close loop, the open, the two log sites, and
    `logHeldFitLocked`.
  - Tests H1-H8 in `internal/daemon/`.
  - `docs/dev/aira-353-held-fit.py` and `docs/dev/aira-353-mutation-check.py`.
  - There is no protocol, wire, configuration, CLI, MCP, TUI or generated-text change.
- **Review.** The full two-loop: Codex Sol at `xhigh` reviews the build, and Fable gives the final
  review.
- **Rollout.**
  - Send the shared daemon-restart heads-up to peer sessions, after `aira confine --list`.
  - Run `aira install`. It restarts a present daemon whose binary is stale, and the cutover's
    `Started` instant opens §3.6's window. Record that instant.
  - No skill reinstall is needed.

## 10. Round-3 findings on the old §M, and their disposition

| Finding | Disposition |
|---|---|
| (a) "By-budget hours" was called an upper bound, and D1 pinned it | §3.1 redefines the figures and claims no bound. H1 pins them. |
| (b) Mixed populations: a journal numerator over a retention-capped store denominator | W is in the same line (§3.4). The script reads the journal only. |
| (c) Unevaluated outages were counted as held | Credit only at a pass start, to a queued waiter, capped (§3.3); H3, H8. |
| (d) D5 could not fail, and the single-review path | H6 (structural, exact counts) and H7 (differential). The mutations are recorded RED. The full two-loop. |
| (e) Accounting across passes (interpreted: the old in-loop close left an interval open across an early return) | As (c). H3 kills the in-loop close. |
| (f) The 31% and 9% suite-parent figures | Not reused. |
| Ungranted waiters lost their held time | A line at removal, frozen at the terminal transition (§3.2); H4. |

## 11. Round-2 review log (this spec at `5f2e596e`)

| # | Sev | Finding | Checked against | Disposition |
|---|---|---|---|---|
| 1, 11, 12 | P1, P2, P1 | "No waiter granted today waits longer" is unmet; the incident is unchanged; "restores the bound" | `admit.go:2538`, `2691-2697`, `2762-2769`; the counterexample in §2 holds; queue-order §3.5 "Pushed head" | **Confirmed. 352 is collapsed** (§2): proposed for a cut, with an owner waiver as the only route to a build. Corrections are made in the ticket and in lane spec §8. |
| 2, 13 | P2 | J2 and invariant 5 are false after D | B9's t=30 pass | Confirmed. Moot by the cut. On revival: "the decision function differs only at D, given equal state; afterwards the trajectories diverge". |
| 3 | P2 | Removal accounting credited time after the rejection | `admit.go:2975-2977`, `1984-2000`; `confine_kill_queued.go:223-225` | Confirmed. The figures are frozen at the terminal transition: credit only to queued waiters at a pass start, and `waited-ms` from the rejection (§3.2, §3.3); H4. |
| 4 | P2 | The 250 ms pass rate is not guaranteed | `admit.go:2436-2458`; `paths.go:291` (up to 10 s) | Confirmed. A 30 s cap per interval (§3.3); H8. This reverses round 2's cut of a cap. |
| 5, 9 | P2 | A graceful stop logs `closed`; crash censoring; the PID check cannot see it | `server.go:651`; `admit.go:1945-1952`, `1976-1977`; `admission_linux.go:253`, `327-335` | Confirmed. Outcome `stopped`; a window bounded by daemon starts and stops; unclean ends from systemd's messages; the cut-share validity rule (§3.3, §3.6); H4(d). |
| 6 | P2 | 352's yield overstated | `aira-352-pushed-backstops.py:14-18` | Confirmed. Relabelled as an estimate and a conditional scenario (§2, item 3). |
| 7 | P2 | B2 does not kill its named mutation | B2 grants P at t=20 | Confirmed. Moot by the cut. On revival: make B2 a pass that grants nothing behind the head and must still consume the anchor. |
| 8 | P2 | Causal claims in the decision rule | §3.1 "safe"; old Q4 | Confirmed. "Safe" is withdrawn, the thresholds are labelled heuristic, and the counter is cut (§8). |
| 10 | P2 | No test of a deferred pass that grants something | `admit.go:2688`; `admitOrderLocked` `452-476` | Confirmed. Moot by the cut. On revival: add a backstop pass with a pushed head in which a sub-reservation, or a more recently pushed waiter, is granted; assert that the anchor is unchanged. |
| 14 | P2 | Only S decides, so B's threshold is decorative | §3.6, rules 4 and 5 | Confirmed. Merged into "no outcome; report to the owner". B and P are justified by the ticket (§1). |

## 12. Open questions (owner)

1. **The rule's numbers** (O in §3.6): the 7-day window, the 1 GiB class, 10% and 10 h for DROP,
   and the 5% cut share.
2. **AIRA-352: cut (recommended), or waive "no waiter granted today waits longer" and build?** The
   fix shortens the wait after a push by up to `maxHold`, in an estimated 3 of 456 backstop
   passes. It would not have changed the 13:21 incident. It accepts the §2 regressions. If it is
   cut, should the ticket be held or closed?
