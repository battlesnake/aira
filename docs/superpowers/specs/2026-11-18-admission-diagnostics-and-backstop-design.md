# Admission diagnostics (AIRA-353) and the backstop pass behind a pushed head (AIRA-352)

Status: plan, for plan review (Codex Sol at `xhigh`, Gemini) and the Fable plan gate. One release
change, milestone v0.46. Both tickets touch the evaluator (`internal/daemon/admit.go`), so the
change gets the full two-loop: Opus builds, Fable reviews.

**The owner's decision (2026-11-18).** AIRA-350's small-job lane is held: measure first (353), and
add the small backstop fix (352). This spec designs no lane and builds none.

Code references are to the worktree at `a7484074` (master). The data is the same as AIRA-350's
and is not committed, because it names other sessions' jobs: the journal extract
`~/tmp/aira-350/freeze-journal.txt` and the store copy `~/tmp/aira-350/state-copy.db`. The committed
reproduction is `docs/dev/aira-352-pushed-backstops.py`, with its output in
`docs/dev/aira-352-pushed-backstops.out.txt`.

## 0. Summary

- **352.** Today the backstop pass clears the AIRA-59 anchor when it starts (`admit.go:2538`). The
  fix delays that clear to the end of the pass and skips it when the pass's head is a pushed
  waiter. Net change: two lines, plus one conjunct on the transition log.
- **Correction to AIRA-352 and to AIRA-350 §8.** Both call the 13:21:07 loss "the incident's
  largest single loss". The fix would **not** have changed the 13:21:07 incident (§2.3). In the
  3.5-day window it would have changed at least 3 of 456 backstop passes; 2 more are unresolved.
- **The brief asks for a proof that no waiter granted today waits longer. That proof is false for
  this fix, and for any fix that restores the lost pass** (§2.6). The deferred pass is a backstop
  pass, and a backstop pass can delay the head it passes. That cost is the one AIRA-307 accepts.
  What is proved instead: nothing passes a pushed head, every pass decides exactly as today except
  one, and there is still at most one backfilling pass per hold. B9 and B12 pin the residuals.
- **353.** One journal line per waited job-level waiter, at its grant or when it leaves: its
  booking, its total wait, its time held while fitting (by budget, by push) and its budget span
  (§3.4). A script reads the journal alone and applies the rule fixed in §3.6.

## 1. The greenfield minimum, and the gap

**352 alone.** A backstop pass whose head is pushed grants nothing past that head, so it should
not use up the anchor. Minimum: keep the anchor when the pass's head is pushed. That is two lines.

| Addition over the minimum | Why it is needed | Size |
|---|---|---|
| The pass that defers the backstop writes no transition log line | Otherwise the `yield` line dates the backstop to a pass that admitted nothing, and names the pushed head. The AIRA-350 scripts count `yield` lines as backstop passes. | one conjunct |

**353 alone.** "Measure what a lane could address" needs, per waiter: its booking, its wait, and
how long it fitted but the budget held it. That means one counter, one open-interval instant, a
close, an open, and one line at the end.

| Addition over the minimum | Why it is needed | Size |
|---|---|---|
| A second counter for time held by a pushed head | A lane never passes a pushed head (AIRA-309). Without the split, push holds inflate the budget figure. | one bool, one counter |
| `budget-span-ms` | The held time alone is not what a lane would save first-order (§3.1, H1). The DROP side of the rule needs the larger figure. | one instant |
| A line when a waiter leaves ungranted | Without it, held time and wait are lost from both sides of the ratio, and the population is incomplete. The brief prefers this if it is small, and it is: one call at the single removal point. | about 6 lines |
| Close every open interval at the start of every pass | Passes that return early, or skip the waiter, would otherwise credit an outage to the last verdict (findings c and e). | one loop |
| The guard tests H6 and H7 | The fields must be diagnosis only, and a guard that cannot fail proves nothing (finding d). | tests only |

## 2. AIRA-352: the backstop pass behind a pushed head

### 2.1 What the backstop is documented to guarantee

- **AIRA-59's origin** (`2026-09-03-aira58-59-admission-wait-and-freeze-plan.md` §5, guarantee 2):
  fairness never blocks the queue for more than a bounded share of wall time, under any holder
  churn. AIRA-307 replaced its yield window with the single backfilling pass.
- **The AIRA-307 bounds** (queue-order spec §3.5):
  - "Head (unpushed): it is bypassed by at most H/2 bytes before strict protection begins. After
    that, only by one backfilling pass per `maxHold` of strict hold."
  - "Everyone else: the slice is never strictly frozen for longer than `maxHold` without one full
    backfill pass."
  - "Pushed head: it is never bypassed by new job-level work. It is not covered by the backstop."
- **The code's own reason** (`admit.go:2532-2537`). The pass is "at least one backfilling pass per
  hold, which is what actually admits waiters, rather than wall time spent nominally yielding".
- **No document says a push consumes the backstop.** Owner decision (A) makes a pushed head freeze
  everything behind it ("drain the slice"); that freeze is outside the backstop, not a use of it.

### 2.2 The defect

The completed-cycle clear runs at the top of the pass, before the head is known
(`admit.go:2525-2540`). When the head is pushed, the pushed case precedes the yield case
(`admit.go:2691-2693`), so the pass grants nothing behind it, and the anchor is already gone. After
the push, a waiter held by an unpushed head's spent budget re-arms a fresh hold
(`admit.go:2762-2769`): up to two `maxHold`s of unpushed strict hold with no backfilling pass, plus
the push. That breaks the "everyone else" bound.

### 2.3 Evidence, and the corrected claim

**The 13:21:07 incident** (EET, journal extract):
- the hold armed at 13:16:07 (head 14782), and the yield at 13:21:07 named the pushed head 14816;
- the next line is `idle` at 13:21:08, and the next `hold` is at 13:22:22 (head 14800);
- 14816 was granted at 13:21:47 (store row, `wait_ms` 317 s).

The phase was idle from 13:21:08 to 13:22:22. Any pass in which a waiter fitted and was withheld by
the budget would have armed (`admit.go:2762`). None did until 13:22:22, the 13:21:47 pass included.
With the fix, the deferred backstop pass is the 13:21:47 pass, which had nothing to backfill. **Every
grant in the incident is unchanged.** The loss at 13:21:07 was the push itself: the owner's drain
semantics, not the anchor clear.

**The window**, 2026-11-15 00:00 to 2026-11-18 12:00 UTC, from
`docs/dev/aira-352-pushed-backstops.out.txt`:
- **Backstop passes:** 456 yield lines, 384 of them naming a head.
- **Candidates** for a lost backstop: 8. These are yield lines whose head is newer than the head
  named by the next hold line in the same daemon process, so an older waiter was still queued
  behind it. A `--front` push or a `--priority high` waiter produces that pattern, so these are
  candidates, not verified pushes.
- **The fix changes 3 of them** (11-16 00:39:59Z, 02:44:27Z, 02:51:17Z; estimated, since the store
  row's start time stands in for the grant): the candidate head started within 0.05 s of today's
  re-arm, so a fitting waiter was budget-withheld at that pass, and the fix backfills it up to
  `maxHold` (5 minutes) earlier. The journal does not say how many waiters.
- **4 unchanged**, the incident included (the next arm came 1.7-35 s after the head's start), and
  **1 has no retained row.**
- **A floor, not a count.** The rule misses a pushed head whose older waiters had all left. Two
  yields within seconds to minutes of a push fail the rule and stay unresolved: head 3791 at
  11-16 00:51:11Z and head 6746 at 15:46:56Z.

### 2.4 The change

Delete the clear at `admit.go:2538`; the backstop branch then only sets `phase = admitFreezeYield`.
After the waiter loop, before the transition log (`admit.go:2781`):

```go
// AIRA-352. A backstop pass whose head is PUSHED admitted nothing past it, so it does not consume
// the hold: the first pass with an unpushed head (or none) is the one backfilling pass. phase ==
// yield only on a backstop pass (admitFreezePhaseAt never returns it; the arm needs idle).
deferred := phase == admitFreezeYield && head != nil && head.pushSeq > 0
if phase == admitFreezeYield && !deferred {
	queue.freezeArmedAt = time.Time{}
}
```

The transition log condition becomes `maxHold > 0 && phase != queue.freezeLogged && !deferred`.
So a deferred pass logs nothing and leaves `freezeLogged` as it was. The `yield` line is written
by the pass that actually backfills, and names that pass's head.

**Comments to update:**
- the backstop branch (`admit.go:2526-2537`);
- the anchor writers list (`admit.go:798-812`): write 2 now runs at the end of a backstop pass
  whose head is not pushed;
- a one-line amendment note in queue-order spec §3.5.

**Behaviour change, stated exactly.** When a backstop pass finds a pushed head, today's code drops
the backstop. With the fix, the next pass whose head is not pushed is the backstop pass.

**What does not change:** no protocol, wire, frame, configuration, install, CLI, MCP or TUI
change. The generated text stays true as written, so no wording or keyword test changes:
`internal/core/skill.go:338` ("...before one backfilling pass") and `cmd/aira/main.go:3773` ("one
backfilling pass follows the hold"). `ci_dump.go` prints no anchor (lines 47 and 172 are comments).

### 2.5 Interactions

| With | Decision | Reason / code |
|---|---|---|
| Unpushed head | Unchanged: the pass is consumed. | `deferred` is false. |
| `--priority high` head | Unchanged: consumed. | A high head is unpushed (AIRA-340, `admit.go:302-311`); B5c. |
| Pushed waiter that cannot fit an empty slice | Not a head (`admit.go:2770`, `admitCouldFitEmptyLocked` `2843-2857`), so the pass is consumed past it, as AIRA-307 §3.3 documents today. During a deferral this includes a pushed head that stops being head-eligible for one pass (a ceiling dip, a stale VRAM snapshot): that pass becomes the backfill (§2.6). | B5a, B12 |
| Pushed head granted in the backstop pass itself | It is not the head, so the next head decides. | B5b |
| Pushed head that leaves (kill, EOF, timeout) | The next pass decides. | B6 |
| Sub-reservations | Unchanged: exempt and granted on fit (`admit.go:2688`), in deferred passes too. | |
| Exclusive drain or hold | Unchanged: every job-level waiter is gate-blocked (`admit.go:2604-2621`), so there is no head and the pass is consumed. It is the same class of loss, but not in scope, and there is no evidence it occurs (§8). | B11 pins it. |
| Restart freeze | Cannot interact. The anchor is zero throughout the freeze: the freeze is armed once at listen-ready (`reload.go:31-36`), the queue is created after start (`admit.go:2170-2190`), and the arm is skipped while frozen (`admit.go:2592-2595`). | — |
| ci-shim | Same rule. The duty cycle has no mode branch. | B10 |
| Drain reset, `maxHold <= 0`, fail-fast latch, unreadable slice | Unchanged: the reset clears the anchor (`admit.go:3102-3114`); a disabled cycle never writes it (`841`); the other two return before the backstop branch (`2494-2517`). | |
| Reported phase | Unchanged. During a deferral `admitReportedPhaseLocked` reports `pushed` (`admit.go:2809-2816`). Afterwards the retained anchor derives as `idle`, as the cleared one does today. | |

### 2.6 What is proved, and what is not

**J1. Nothing passes a pushed head.** The `switch` is untouched: the pushed case comes first
(`admit.go:2691`). Only a pass whose head is not pushed can backfill.

**J2. Every pass decides exactly as today except one.** Call it D: the first pass after a deferred
backstop pass whose head is not pushed.
- Before D, each pass has a pushed head, so `phase == yield` changes no decision:
  - waiters before the head are unprotected in any phase;
  - after the head, the pushed case withholds every fitting job-level waiter;
  - the budget case, and so the arm, cannot be reached.
- At D, today's phase is idle and the fix's is yield. So exactly the fitting job-level waiters that
  today's budget withholds behind D's head are granted: a backstop pass's grants. Today's code
  would have armed a fresh hold at D instead.
- If D has no head, the phase decides nothing, and D equals today.
- **D need not come after the push ends.** If the pushed waiter is still queued but cannot fit an
  empty slice at one pass (a throttled-ceiling dip, an unevaluated VRAM snapshot), it is not the
  head there. That pass is D, and it backfills past it. Today's code would grant there only within
  the budget, and then arm. This matches AIRA-307 §3.3 (a waiter that cannot fit an empty slice
  freezes nobody, pushed or not), but by the plain reading a pushed waiter is passed. B12 pins it.

**J3. At most one backfilling pass per hold.** No anchor write is added, the arm guard is
unchanged, and each arm is cleared exactly once. So delivered backfilling passes stay at least
`maxHold` apart.

**Not provable: "no waiter granted today waits longer".** It is false. In B9 the deferred backfill
at D takes room that the unpushed head behind the push would have used ten seconds later; today
that head gets a fresh strict hold at D and is granted. Every later arm instant shifts too. This is
the cost AIRA-307 accepts for every backstop pass ("one backfilling pass per `maxHold` of strict
hold"), and the head at D has already had at least `maxHold` of protection (hold, then push), so
the documented bound holds for it. Measured incidence: 3 passes in 3.5 days (§2.3); owner
question §12 Q2.

### 2.7 Tests (AIRA-228 seam)

**Setup.** The tests use `queueOrderServer`, `queueOf`, `heldLease` and `queuedWaiter`
(`admit_queue_order_test.go:22-50`, `admit_freeze_test.go:30-32`): a 64 GiB slice, zero headroom,
`maxHold` = 1 minute, and the clock stepped by hand. The anchor is armed 61 s before the first pass,
so that pass is a backstop pass. "Today" means the code at `a7484074`.

| # | Setup | Assert (fixed build) | Today, or the named mutation |
|---|---|---|---|
| B1 deferred backfill | Leases of 52 GiB and 6 GiB (room 6). P is a pushed 7 GiB waiter. H is 8 GiB with its budget spent (`bypassed = 4 GiB`). W is 1 GiB. Passes at t=0 and t=10. Before t=20 the 6 GiB lease is released. | t=0 and t=10: W queued and the anchor unchanged. t=20: P granted, H the head, W granted, `H.bypassed` +1 GiB, anchor zero. | Today: W queued at t=20 and the anchor re-armed to t=20. Re-arming to `now` on a deferred pass: t=20 is a hold, so W is queued. |
| B2 incident replay (no change) | As B1, but the 52 GiB lease is split into 50 GiB and 2 GiB, and W is 6 GiB, so after P's grant (room 5) nothing fits behind H at t=20. Before t=55 the 2 GiB lease is released (room 7: W fits, H does not). | t=20: nothing granted behind H, anchor zero. t=55: W withheld and the hold armed at t=55, as today. t=114: W queued. t=115: W granted. | Keep the anchor until a backstop grants something: W granted at t=55. Clear only when the head is nil: the same. |
| B3 no head consumes | Backstop pass where every waiter fits (no head). Then at t=5 a new 8 GiB head with its budget spent and a fitting 1 GiB waiter. | The anchor is cleared at t=0. At t=5 the waiter is withheld and a fresh hold armed. | Keep the anchor unless an unpushed head was protected: the waiter is granted at t=5. |
| B4 a pushed head is never passed | P at the head across 5 deferred passes, 1 s apart. Behind it, fitting unpushed, `high` and normal waiters. | None granted. `bypassed` unchanged. The anchor equals its armed instant. | The yield case before the pushed case: granted. |
| B5 head not pushed, so consumed | (a) P cannot fit an empty slice. (b) P fits and is granted in the pass. (c) The head is unpushed `high`. (d) P is removed before the pass. Each has H, a fitting W, and a W2 that first fits at t=1. | W granted at t=0, anchor zero after t=0, W2 withheld at t=1 arming a hold. | Keep the anchor while any pushed waiter is queued: (a) W2 granted at t=1. Treat `high` like pushed: (c) the same. |
| B6 the push ends by leaving | As B1, but after the t=10 pass P is cancelled through the queued-kill path (`confine_kill_queued.go:223-226`) and released. | At t=11: H the head, W granted, anchor zero. | Clear only when P is granted: the anchor kept at t=11, and W2 granted later with no hold. |
| B7 one backfill per hold | After B1's t=20, W2 first fits at t=21. | t=21: W2 withheld and the anchor armed at t=21. t=80: queued. t=81: granted. | Never clear after a deferral: W2 granted at t=21. |
| B8 log | B1, with the log captured as in `admit_freeze_test.go:653-705`. | No `fairness-freeze yield` line at t=0 or t=10. Exactly one at t=20, naming H's seq. `idle` at the next pass. | Today's logging: a `yield` line at t=0 naming P's seq. |
| B9 accepted residual (pinned) | Leases of 49, 6 and 3 GiB (room 6). P 7 GiB, H 8 GiB with its budget spent, W 1 GiB. The 6 GiB lease is released before t=20, the 3 GiB before t=30. | t=20: W granted (room 4). t=30: H queued (room 7 < 8). | Today: H granted at t=30. This pins the cost of §2.6. A build that claims to remove it must change this test knowingly. |
| B10 ci-shim | B1 on a server with `confineMode = runner.ConfineModeShim` (`shim.go:37`). | The same as B1. | The deferral only outside shim: fails like today. |
| B11 exclusive hold (pinned) | A granted exclusive holder. Backstop pass with fitting job-level waiters, all gate-blocked. | The anchor is cleared, as today. | Pins the §8 deferral. Keeping the anchor when everything was gate-blocked would turn this red. |
| B12 head-eligibility blip (pinned) | One 20 GiB lease (room 44). P is pushed, 50 GiB. H is 46 GiB with its budget spent. W is 1 GiB. Deferred at t=0. Before t=10 the slice maximum is lowered to 48 GiB through the reader seam, so P cannot fit an empty slice (room 28; H can, and does not fit). | t=10: P is not the head, H is, W is backfilled and the anchor cleared. | Today: W withheld and the hold armed at t=10. Pins the §2.6 residual. |

The existing pushed-head test (`admit_queue_order_test.go:170-203`, the `yield` case) passes
unchanged: it does not assert the anchor after a yield.

## 3. AIRA-353: held-while-fitting diagnostics

### 3.1 What each figure means (fixes finding a)

All figures are per job-level waiter, and diagnosis only.
- **`budget-ms`**: the time the waiter fitted RAM, CPU and VRAM and the budget withheld it
  (`budgetWithheld`, `admit.go:2694-2696`). This is an **attribution** of observed waiting: the part
  of the wait that the budget, not capacity, decided. It is not a saving.
- **`push-ms`**: the same, withheld by a pushed head (`admit.go:2691-2692`). This is the owner's
  drain cost; no lane addresses it.
- **`budget-span-ms`**: from the first pass at which the budget held the waiter while it fitted, to
  its end.
  - It is the **first-order saving of an unbounded small-job exemption**: what this one waiter would
    have saved had it been granted at that first pass, with everything else unchanged.
  - `budget-ms <= budget-span-ms` by construction (H1: 20 s and 30 s).
- **Neither figure bounds a lane's net effect, in either direction.** A lane's grants take room
  from others, and lane jobs that end early free room early. Round 3's "upper bound" label is
  withdrawn.
- **Both overcount, by up to the queue length.** Withheld waiters do not change `outstanding` or
  `cpuOutstanding` within a pass, so every waiter behind the head is judged against the same room.
  Ten 1 GiB waiters all "fit" 2 GiB, or one free CPU slot. This makes DROP on `S` safe, since it
  errs toward keeping the lane, and makes TRIAL on `B` necessary but not sufficient (§3.6). A
  room-consistent counter is an owner option (§8, §12).

### 3.2 The change

- **A struct on `admitWaiter`.** `heldFit admitHeldFit`, a named field, never embedded: `openAt
  time.Time` (the start of the pass that last found the waiter fitting and held), `openPush bool`
  (that pass's cause), `budgetMS`, `pushMS int64`, `firstBudgetAt time.Time`. Exactly two
  methods, neither with a result:
  - `close(now)` adds `elapsedMilliseconds(openAt, now)` (`admit.go:3324-3329`) to the open cause's
    counter, then zeroes `openAt`;
  - `open(now, push)` sets `openAt` and `openPush`, and sets `firstBudgetAt` on the first budget open.
- **The close.** At the start of every pass, right after `now := s.admitNowTime()`
  (`admit.go:2484`): `for _, w := range queue.waiters { w.heldFit.close(now) }` (nil-checked). This
  runs before the fail-fast return (`2494`) and the unreadable-slice return (`2507-2517`). The loop
  is O(waiters), and there are at most `admitMaxWaiters = 256` (`admit.go:31`).
- **The open.** A local `pushHeld` is set in the pushed case. In the refusal branch, after
  `waiter.waited = true` (`admit.go:2736`):
  `if budgetWithheld || pushHeld { waiter.heldFit.open(now, pushHeld) }`. Both flags imply that
  `fits` was true before the `switch` (`admit.go:2689`).
- **The line at grant.** In the grant branch, after `waitedMS` is set (`admit.go:2718-2720`):
  `if waiter.waited && !waiter.isSubReservation() { logHeldFitLocked(waiter, "granted", waiter.waitedMS, now) }`.
- **The line at departure.** In `releaseAdmitWaiterLocked` (`admit.go:3060`), the single point that
  removes a waiter from `queue.waiters`, before the state is set to released. Only for a waiter
  never granted (state `admitQueued` or `admitRejected`), with `waited` set, not a sub-reservation,
  and `queue.server != nil`. It reads `now` from `queue.server.admitNowTime()` (the queue keeps its
  server, `admit.go:772`), calls `close(now)`, and logs `waited-ms = elapsedMilliseconds(enqueued,
  now)` with the outcome: `waiter.outcome` for a rejected waiter (`saturated`, `cancelled` or
  `failfast`: `admit.go:2975`, `confine_kill_queued.go:224`, `admit.go:2500`), `closed` otherwise.
- **Volume control.** A granted waiter is never logged again at its release. An immediately granted
  waiter has `waited == false` and is not logged. Sub-reservations, the aitest workers among them
  (`worker_admit.go:458`), are never logged.

### 3.3 The accounting rule (fixes findings c and e)

- **The interval rule.** Each interval between consecutive pass starts, or between a pass start and
  the waiter's removal, goes to the verdict of the pass that opened it.
- **A pass that does not evaluate the waiter opens nothing**: the restart freeze (`2592-2595`), the
  exclusivity gate (`2604-2621`), an unreadable slice, the fail-fast latch. Time after such a pass
  is not attributed to any cause.
- **So `budget-ms + push-ms <= waited-ms`, always.** The rest of the wait is "other": capacity, gate,
  restart freeze, or an outage.
- **The error.** Passes run at least every 250 ms while the queue is non-empty
  (`admit.go:2445-2458`, `paths.go:47`), so each change of verdict misattributes at most one pass
  gap.
- **A daemon restart** ends every waiter without a line. The client re-enqueues as a new waiter,
  whose `waited-ms` starts again. Both sides of the ratio lose the same span. The script reports the
  daemon processes in the window.

### 3.4 The line, its modes and its volume

```
aira daemon: held-fit seq=<n> outcome=<granted|saturated|cancelled|failfast|closed> booking=<bytes> waited-ms=<n> budget-ms=<n> push-ms=<n> budget-span-ms=<n>
```

- `booking` is `ledgerCharge()`, the unit of the budget and of the old lane's eligibility.
- **Both modes, with no branch.** No decision reads the line in either mode. In ci-shim the daemon
  logs to its own `shim-daemon.log` (`internal/install/mode.go:360-376`), not to CI output. A mode
  branch would cost a conjunct and a test for no reader, and the freeze transitions are logged in
  both modes already (`admit.go:2784`).
- **Volume** is one line per waited job-level waiter, never per pass. Both figures are estimates
  from proxies:
  - floor: the store retained 4145 confine rows with `wait_ms >= 10 s` in 84 h, about 1,200 a day.
    Store `wait_ms` is not the evaluator's `waited` (10736 of 10746 rows are above zero), so this
    is a floor only;
  - ceiling: every enqueue, sub-reservations included. The seq rate within each daemon process of
    the journal extract is 7,300-17,800 a day.

### 3.5 Tests (AIRA-228 seam; the log is captured as in B8)

| # | Setup | Assert | Mutation it kills |
|---|---|---|---|
| H1 meaning | W is enqueued at t=-5, behind an unpushed head with its budget spent. t=0: fits and is withheld. t=10: refused on capacity. t=20: fits and is withheld. The head is removed before t=30, and W is granted at t=30. | One line: `waited-ms=35000 budget-ms=20000 push-ms=0 budget-span-ms=30000 outcome=granted`. | Right-endpoint attribution (10000). Held counted from the first held pass (30000). Span from enqueue (35000). |
| H2 cause split | W fits behind pushed P at t=0 and t=10. P is granted at t=20, and H (budget spent) withholds W at t=20. H is removed before t=30, and W is granted at t=30. | `push-ms=20000 budget-ms=10000 budget-span-ms=10000`. | One counter (30000). Causes swapped. |
| H3 unevaluated passes | W is withheld by the budget at t=0. t=10: a pass whose memory read fails. t=20: (a) the restart freeze, (b) gate-blocked by an exclusive drain. The freeze, the drain and the head are gone before t=30, and W is granted at t=30. | `budget-ms=10000`, `waited-ms=30000`. | Closing inside the waiter loop: the t=10 pass returns early, so 20000. Closing only in the refusal and grant branches: 30000. |
| H4 departure | W is withheld at t=0 and t=10. At t=14 it leaves by (a) timeout, (b) queued kill, (c) client EOF (`releaseAdmitWaiterAnchored` with its own conn). | One line each: `budget-ms=14000`, `waited-ms=14000`, outcome `saturated`, `cancelled` or `closed`. | No departure line. A departure line without closing the open interval (10000). |
| H5 exactly one line, only job-level | An immediate grant. A waited grant later released. A waited sub-reservation. | Exactly one line, for the waited grant, at its grant. | A line on every grant. A second line at release. Sub-reservations logged. |
| H6 guard, structural | AST scan as in `admit_pin_charge_guard_test.go`. | Every `.heldFit` selector is on an allow-list keyed by function with an exact count: `evaluateAdmitQueue` 2, `releaseAdmitWaiterLocked` 1, `logHeldFitLocked` n. `admitHeldFit` has exactly the methods `close` and `open`, with no results. No function takes or returns `admitHeldFit`, and it is embedded nowhere. | `fits = fits && waiter.heldFit.budgetMS == 0` in `evaluateAdmitQueue`: an unlisted selector. A method on `admitHeldFit` that returns a value. A new reader function. |
| H7 guard, differential | The passes of B1 and H2, run twice: once as they are, once with every waiter's `heldFit` poisoned before each pass (`openAt = now - 1h`, counters at `1 << 40`, `firstBudgetAt = now - 1h`). | The same granted set, `bypassed`, anchor and `freezeHolderSeq` after every pass. | The H6 mutation, through any path. |

The gate runs each named mutation of §6 and records it RED.

### 3.6 The analysis script, and the decision rule fixed before the data (fixes finding b)

**The script.** `docs/dev/aira-353-held-fit.py` reads the output of
`journalctl --user -u aira-daemon.service -o short-unix --since S --until U`, or a saved extract.
`--since` and `--until` are both required. It reads the journal only; there is no store and no
row join.
- **Per class** (small: `booking <= 1 GiB`; large: the rest): line counts by outcome, and in hours
  `W` = Σ `waited-ms`, `B` = Σ `budget-ms`, `S` = Σ `budget-span-ms`, `P` = Σ `push-ms`.
- **Validity:** each daemon process (`aira[PID]`) that wrote a `fairness-freeze` line in the window
  must also have written a `held-fit` line. If not, it ran an older binary, and the result is
  invalid.
- **`--self-test`** runs the script on embedded synthetic lines with known totals, so a parsing
  error shows.

**The rule.** The owner may change any number marked (O) before the cutover, never after.
1. **Window:** 7 full days (O), starting at the cutover.
2. **Population:** small jobs, `booking <= 1 GiB` (O): AIRA-350's `y`.
3. **DROP the lane** (close AIRA-350 and report) if `S < 10%` of `W` (O) or `S < 10 h` (O). Even
   the generous first-order figure is small.
4. **A TRIAL may be considered** only if `B >= 10%` of `W` (O) and `B >= 10 h` (O). This is
   necessary, not sufficient, because `B` overcounts (§3.1). The trial still needs the owner's yes
   on AIRA-350's Q2-Q4.
5. **Otherwise, the owner decides on the figures.** This spec recommends no build.

**Not reused.** AIRA-350's association figure is not a forecast: about 5 of 115.6 small wait-hours,
from retention-capped rows. Nor are the suite-parent figures (round-3 finding f).

## 4. Invariants

1. **No admission decision reads `heldFit`** (H6, H7). AIRA-353 changes no grant, ledger, head,
   `bypassed` or anchor. Every existing test passes unchanged.
2. **Every waiter's figures partition its wait.** `budget-ms + push-ms <= waited-ms`. Each pass gap
   is attributed at most once, and nothing is attributed after a pass that did not evaluate the
   waiter.
3. **Exactly one line per waited job-level waiter**, at its grant or at its removal.
4. **A pushed head is never bypassed while it can fit an empty slice** (AIRA-309 inv. 3; J1; B12).
5. **Decisions differ from today only at the pass D of §2.6** (J2).
6. **At most one backfilling pass per arm** (J3); AIRA-352 adds no anchor write.

## 5. Risks

- **R1. A wedge from the deferral.** Impossible: a retained anchor derives as idle, so it never
  makes a pass hold; it only marks the next pass without a pushed head as the backfill (B3, B7).
- **R2.** The deferred backfill delays an unpushed head (B9; at least 3 times in 3.5 days).
  **R3.** Volume (§3.4). **R4.** Diagnosis leaking into decisions (H6, H7).

## 6. Mutation list (the gate runs each and records RED)

The B and H tables name each mutation beside the test that kills it. The gate script is
`docs/dev/aira-352-353-mutation-check.py`, on the pattern of `docs/dev/aira-324-mutation-check.py`.
- **False pass** (grants or backfills what must wait): B2, B3, B4, B5a, B5c, B7.
- **False fail** (withholds what the fix must grant): B1 (today's clear, re-arm to `now`), B6, B10.
- **Diagnostics:** B8, H1-H7.
- **Pinned residuals, not mutations:** B9 and B12 (§2.6), and B11 (§8).

## 7. Expected yield

| Change | What it addresses | Share | Basis |
|---|---|---|---|
| 352 | Backstop passes whose head is pushed and after which a fitting waiter was held for a fresh hold | At least 3 of 456 backstop passes in 3.5 days (2 unresolved). Their re-arm lines show 14, 32 and 28 also queued. If every one of those 74 waiters saved the full 5 minutes, that is at most 6.2 waiter-hours: 3.2% of the 192 retained wait-hours (115.6 small plus 76.4 large). Only fitting, withheld waiters gain, so the true figure is lower | count measured (floor); wait share estimated, upper |
| 352 | The 13:21:07 incident | none | measured: no arm from 13:21:08 to 13:22:22 (§2.3) |
| 353 | No decision changes. It measures `B`, `S` and `P` against `W`, so AIRA-350 is decided on the figures | — | — |

## 8. Deferrals

- **A backstop pass lost to an exclusive drain or hold.** It is the same class as 352. Covering it
  needs a "the gate blocked every job-level waiter" flag, and it would also keep the anchor through
  long exclusive holds. There is no evidence it occurs. B11 pins today's behaviour.
- **AIRA-350's lane:** held until §3.6's rule is read.
- **A CPU diagnostic for capacity refusals.** This is the pre-existing gap at `admit.go:2729-2735`:
  "other" time cannot yet be split into RAM, CPU and VRAM.
- **Waits cut by a daemon restart:** accepted, and reported by the script.
- **A room-consistent held counter** (owner option). A pass-local running total of the bookings,
  cores and VRAM of the waiters already counted as held-fit in that pass. A later waiter counts only
  if it fits net of that total. It is about 5 lines, diagnosis only, and makes `B` a joint-feasible
  figure in evaluation order.

## 9. Cut list

| Option | Why cut |
|---|---|
| Building any lane | Owner decision (AIRA-350 held). |
| A per-pass log line | Passes reach 4 a second. The per-waiter line carries the totals. |
| A store column or telemetry row | History rows are retention-capped and carry no scope id (round-3 finding b). The journal line is complete. |
| A cap on each interval at the poll interval | A heuristic. The close at the start of every pass makes it unnecessary. |
| Logging immediate grants | They add zero to both sums. |
| Logging sub-reservations | They are exempt from the head rule and would multiply the volume. |
| A ci-shim branch | No reader; it costs a conjunct and a test. |
| Pausing the hold clock during a push (moving the anchor by the push's length) | More state. Waiters behind the push would wait longer than with the fix, and AIRA-307 already counts the push as protection for the head. |
| A dedicated "backstop deferred" log line | The deferral can be read from the journal: a `yield` more than `maxHold` after its `hold`. |
| The old D1-D5, and §M.6's single-review path | D1 pinned the wrong meaning, and D5 could not fail. This is an evaluator change, so it gets the full two-loop. |

## 10. Files, review and rollout

- **Files:** `internal/daemon/admit.go` (§2.4; §3.2's struct, close, open, two log sites and
  `logHeldFitLocked`); tests B1-B11 and H1-H7 in `internal/daemon/`;
  `docs/dev/aira-353-held-fit.py`; `docs/dev/aira-352-353-mutation-check.py`; the amendment note
  in the queue-order spec.
- **Review:** the full two-loop. Codex Sol at `xhigh` reviews the build; Fable gives the final
  review.
- **Rollout:** the shared daemon-restart heads-up to peer sessions (`aira confine --list` first);
  `aira install`, which restarts a present daemon whose binary is stale; record the cutover
  instant, where §3.6's window starts. No skill reinstall: no generated text changes.

## 11. The round-3 findings on the old §M, and their disposition

| Finding | Checked against | Disposition |
|---|---|---|
| (a) "By-budget hours" called an upper bound; D1 pinned it | D1's case: held 20 s, span 30 s | §3.1 redefines both figures and withdraws the bound; H1 pins both |
| (b) Mixed populations: journal numerator, retention-capped store denominator | `confine_peak_history` has no scope id and keeps 20 rows per signature | The denominator is in the same line (§3.4); the script reads the journal only (§3.6) |
| (c) Unevaluated outages counted as held | Early returns at `admit.go:2494-2517`; continues at `2592-2595` and `2604-2621` | The close at the start of every pass (§3.3); H3 |
| (d) D5 could not fail; the single-review path | — | H6 (structural, exact counts) and H7 (differential); the mutations are recorded RED; the full two-loop |
| (e) Accounting across passes | The review text was not available here; this is an interpretation. The old close sat inside the waiter loop, so a pass that returned early left the interval open | As (c); H3 kills the in-loop close |
| (f) The 31% / 9% suite-parent figures | Retention-biased | Not reused |
| Ungranted waiters lose their held time | `releaseAdmitWaiterLocked` is the one removal point | A line at departure (§3.2); H4 |

## 12. Open questions (owner)

1. **The decision rule's numbers** (O in §3.6): 7 days, 1 GiB, 10% and 10 h for both thresholds.
2. **AIRA-352 cannot meet "no waiter granted today waits longer"** (§2.6). It trades one fresh hold
   for one backfilling pass, the AIRA-307 trade. That happened at least 3 times in 3.5 days, and it would
   not have changed the 13:21 incident. It also backfills during a one-pass head-eligibility blip
   of a pushed waiter (B12). **Build it (recommended)**, because it restores the
   documented "one backfilling pass per `maxHold`" bound in two lines. **Or cut it**, because its
   measured yield is small.
3. **The backstop lost to an exclusive drain or hold** stays deferred (§8) unless the owner wants it
   in this change.
4. **The room-consistent held counter** (§8): add it now so that `B` can be sufficient as well as
   necessary, or leave it out.
