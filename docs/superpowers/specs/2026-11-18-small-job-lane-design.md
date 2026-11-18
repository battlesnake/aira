# Small-job lane: small jobs pass a held head within a per-head allowance (AIRA-350)

Status: plan, round 3, for plan review (Codex Sol at `xhigh`, Gemini) and the Fable plan gate. This
round re-bases the design (below) and needs the owner's yes on one changed decision (§13 Q1).
Ticket AIRA-350, milestone v0.46. Correctness-critical (admission): two-loop, Opus builds, Fable
reviews. Builds on AIRA-307/309 (queue order, `docs/superpowers/specs/2026-10-13-aira-queue-order-design.md`)
and AIRA-228 (one queue, one ledger). AIRA-351 (warm-starting cold subjects) is separate, held, and
blocked by this ticket.

Code references are to the worktree at `ae446ef1` (master). Data:
- committed: `docs/dev/aira-350-*` scripts and their `.out.txt` outputs;
- not committed, because they hold other sessions' job names and commands: `~/tmp/aira-350/`
  (the 13:55 `confine --list --json` snapshot, the journal extract, a copy of the store taken at
  14:44 on 2026-11-18).

## Round 3: the concurrent lane is withdrawn, and the design collapses

- **The round-2 design was unsound against its own requirement.** The owner asked for a proof that
  the head cannot starve. Round 2 gave one (its §3.3), but it held only outside production. It
  assumed that no backstop pass refills the slice, that every lease outside the lane ends, and
  that the lane grows only through checked grants. The round-2 review gave a counterexample against
  each assumption. A model simulation reproduces all of them (`docs/dev/aira-350-lane-sim.out.txt`,
  §3.3.1): heads that today's rules grant in 30-350 s are never granted, or wait until small-job
  arrivals stop.
- **The review loop is not converging.** Round 1 found a lane-admitted test suite whose workers pass
  the head (case 13). Its fix, counting a member's children in the lane (C7), restated the head's
  bound. Round 2 found the bound still fails for that same case, and 4 of its 14 findings were
  induced by the C7 fix (§14). That is the non-convergence trigger, so this round re-bases instead
  of patching.
- **The root cause.** A concurrent cap on what small jobs hold past a head lowers that head's room
  by up to `x` for as long as small-job demand keeps refilling it.
  - Production has four mechanisms that hold the other bookings near the head's threshold:
    backstop refills, exempt workers, high-priority or pushed arrivals, and leases that never end.
  - In a band of width `x`, "granted today" therefore becomes "starved".
  - No guard on the lane's size closes that band, because the band exists exactly while the lane
    is at its allowed size. Only a bound per head, in bytes or in time, closes it.
- **The collapse.** Small jobs pass a head within a **per-head allowance**: at most `x` bytes in
  total of jobs of at most `y` while that waiter is the head.
  - This is AIRA-307's own mechanism, a byte counter on the head waiter, with a second counter used
    only by small jobs. AIRA-307's argument then applies unchanged (§3.3).
  - It deletes the lane-member flag, the derived lane sum, guard G with its RAM, per-job-headroom
    and CPU arms, the GPU exclusion (C3) and the child counting (C7).
- **It changes an owner decision.** The owner asked for "small-lane **outstanding** bookings
  `<= x`", which is concurrent. The allowance is cumulative per head, so it needs the owner's yes.
  If the owner keeps concurrency, Alternative B (§3.8) bounds the lane in time instead: the lane
  closes for a head `2 x maxHold` after that head was enqueued.

## 0. The greenfield minimum, and the gap

**The problem in one line.** A queued job that does not fit (the head) holds smaller jobs that do
fit, for minutes, while the slice has room for them.

**The bare minimum.** Exempt every job-level waiter with a booking `<= y` from the head rule. That is
one more conjunct on the line that already exempts sub-reservations
(`protected := head != nil && !waiter.isSubReservation()`, `internal/daemon/admit.go:2688`).

**Why the minimum is not enough.** Unbounded, it is the starvation AIRA-307 ended: a steady stream
of small jobs keeps the head's room below its size (queue-order spec §1.1: 12 GiB heads waited
23-30 minutes through exactly that refill).

**The smallest bound.** AIRA-307 already bounds what passes a head. It keeps a counter on the head
waiter (`bypassed`, `admit.go:283-300`) and compares it with a byte budget (`reserve/2`,
`admit.go:485-490`). The cheapest bound for the small-job exemption is a second counter of the same
kind, `smallBypassed`, compared with `x`, and used only by jobs of at most `y`. That is the whole
admission change.

| Addition | Why it is needed | Size |
|---|---|---|
| `smallBypassed` on the head waiter, bounded by `x` | Bounds what small jobs take past each head, so AIRA-307's argument holds. | one field, one `case`, one increment |
| Off in ci-shim mode | Owner decision. | one conjunct |
| `x`, `y` set at install | Owner decision ("settable like `--pin-charge`"). | env and install plumbing, copied pattern |
| Allowance figures and a per-waiter "held by" token | Without them, "held behind the head" cannot be told apart from "no room". | optional reply fields |

**Not built:**
- a second slice, queue or ledger (AIRA-228 collapsed to one);
- a concurrent cap, lane-member flags or a derived lane sum (round 2, withdrawn);
- guard G, the per-job headroom term, the GPU exclusion, child counting (§2.2);
- any state on the queue, or carried across a daemon restart.

§11 lists everything that was cut.

## 1. Problem and evidence

### 1.1 The mechanism today (read from the code)

In each evaluator pass (`evaluateAdmitQueue`, `internal/daemon/admit.go:2472`), waiters are evaluated
in `admitOrderLocked` order (`admit.go:452-476`): pushed waiters (latest push first), then
`--priority high` (FIFO), then the rest (FIFO).
- **Who is the head.** The head is the first job-level waiter refused on capacity that could fit an
  empty slice (`admit.go:2770-2779`, `admitCouldFitEmptyLocked` at `admit.go:2843`).
- **What a later fitting waiter gets.** "Fits" is RAM, CPU and VRAM together (`admit.go:2663-2674`).
  Every later job-level waiter that fits is then judged by the head rule (`admit.go:2689-2698`):
  - **pushed head** (`head.pushSeq > 0`): it waits;
  - **backstop pass** (`phase == admitFreezeYield`): it is granted;
  - **otherwise:** it is granted only while `head.bypassed + ledgerCharge <= head.ledgerCharge()/2`
    (`admitBypassBudget`, `admit.go:485-490`), else withheld (`budgetWithheld`).
- **The hold.** A budget withhold arms the AIRA-59 hold (`admit.go:2762-2769`). After `maxHold`
  (default 5 minutes, `internal/daemon/paths.go:66`), exactly one backstop pass grants everything
  that fits (`admit.go:2525-2540`).
- **Sub-reservations** (aitest workers, `confine-reserve` per-test holds) are exempt and uncounted
  (`admit.go:2688`).

So once a head's budget (half its booking) is spent, a small job that fits waits up to `maxHold` for
the next backstop pass. If a pushed head is in front at that instant, the backstop pass grants
nothing behind it.

### 1.2 The 2026-11-18 incident, as a timeline

Sources:
- the daemon journal: the freeze lines in `~/tmp/aira-350/freeze-journal.txt`, and the
  `confine-front` lines read with `journalctl --user -u aira-daemon.service`;
- `confine_peak_history` in a copy of the store, by `docs/dev/aira-350-round3-queries.sql` part 1.
  Rows are written when a job ends and only 20 are kept per signature, so this is a sample. Enqueue
  time is grant time less `wait_ms`.

Times are EET. M marks a measured fact, C a fact read from the code.

| Time | Event | Basis |
|---|---|---|
| 13:16:07 | Hold armed, head seq 14782 (4 GiB, queued since 13:09:57). A burst of 13 retained `make` jobs (37 MiB to 4 GiB) is enqueued in the same second. | M |
| 13:16:02-13:16:30 | Also enqueued: seq 14800 (4 GiB); jobs of 1 GiB, 512 MiB and 1 GiB, the last being seq 14816. | M |
| 13:17:49 | 14782 is granted after 472 s. 14800 becomes head, with a 2 GiB budget. | M |
| 13:17:49-13:18:22 | 8 jobs totalling 2002 MiB (84, 62, 182, 37, 141, 146, 70 and 1280; seven of them small) are granted past 14800, spread over 33 s rather than in one pass, so room or CPU was freeing. The budget is spent. | M |
| 13:18:18-13:18:23 | Three more small jobs are enqueued (1024, 256 and 256 MiB). | M |
| 13:18:22-13:21:00 | 14800 is unpushed with its budget spent, and the hold is active. Ten retained small jobs (5788 MiB in total) wait. **Whether they fitted is not recorded**: the store and journal keep no room or CPU figure per instant. | M (waits) |
| 13:21:00 | 14816 (1 GiB) is pushed with `--front`. | M |
| 13:21:07 | The backstop pass of the 13:16:07 hold runs with the pushed head in front, so it grants nothing behind it (`admit.go:2691-2692`). The journal line names head 14816, queued 4m38s. | M, C |
| 13:21:47 | 14816 is granted after 317 s. | M |
| 13:22:02 | Ceiling sample: 52881784832 effective, `sliceAnon` 14.9 GB. | M |
| 13:22:22 | The hold re-arms for head 14800, "13 also queued". An arm needs a waiter that fitted RAM, CPU and VRAM and was withheld by the budget (`admit.go:2762`), so at this pass at least one small job fitted. | M, C |
| 13:22:32 | 14800 is granted after 390 s. | M |
| 13:22:49 | A 256 MiB job is pushed. It is granted at 13:23:01, so for 12 s not even 256 MiB, or one CPU slot, was free. | M |
| 13:23:10-13:25:37 | The held small jobs are granted one at a time over 2.5 minutes, after 422-522 s waits. That pattern points to room or CPU freeing, not to a freeze. | M |

**Reported and not observed together.** The coordinating session reported 32 running jobs booking
41.5-45.4 GiB, 13-14 queued small jobs and about 3.4 GiB free. These cannot all hold at one
instant:
- every confine job declares one CPU slot (`DefaultConfineCPUCores = 1`,
  `internal/runner/confine.go:25`, sent at `internal/runner/admission_linux.go:455-463`);
- aitest workers are floored to one slot (`internal/runner/worker_admit_client_linux.go:116-117`);
- the CPU ceiling is 32 slots (journal, 2026-11-17 02:01:25: "2 per core x 16 cores");
- so with 32 jobs running no one-slot job fits, with or without a lane (`cpuFits`, `admit.go:2667`).
  The comment at `admit.go:178` saying a delegate suite reserves 0 cores is stale since S2a.

The ceiling figure is a measured sample with an assumed job count. 52881784832 bytes is 50432 MiB
effective at 13:22:02. Less headroom for 32 jobs, `2048 + 33 x 64` MiB (`admitSliceHeadroom`,
`admit.go:1041-1051`), that is 46272 MiB, or 45.19 GiB.

**What a lane could have changed.**
- It could only have helped in two windows: 13:18:22-13:21:00 and 13:21:47-13:22:32, and in those
  only at instants with RAM and CPU room. The 13:22:22 arm proves at least one such instant.
- In 13:21:00-13:21:47 and 13:22:49-13:23:01 a pushed head was in front, which closes the lane
  (§3.4).
- After 13:23:01 the waits were capacity-bound.

**What the history does establish.**
- The budget-exhaustion mechanism: a 2002 MiB burst spent a 2 GiB budget in 33 s.
- A backstop pass can be lost entirely to a pushed head.

**Under this plan** (`x = 4 GiB`), the burst's seven small jobs (722 MiB) would have used the
allowance, and only its 1280 MiB job the budget. That leaves 3374 MiB of allowance and 768 MiB of
budget for the ten held jobs (5788 MiB). Up to about 4.1 GiB of them could then have passed in
13:18:22-13:21:00, if room and CPU allowed, which is not established.

### 1.3 The 13:55 instance (the primary replay)

From `aira confine --list --json` at 13:55 (`~/tmp/aira-350/live-snapshot-1355.json`):
- **Ledger:** 42443 MiB granted against a net ceiling of 44736 MiB, so 2293 MiB of room for the
  next job. 28 jobs, 13 of them aitest worker scopes. At one slot each, that is 28 of 32 CPU slots.
  Freeze phase `hold`.
- **Real use:** the slice's own memory was 18234 MiB current less 9361 MiB reclaimable, about
  8.7 GiB.
- **Queue:** a 5120 MiB head, queued 552 s. Behind it, in order:
  - two mid-size jobs, 2154 MiB and 2700 MiB;
  - six small jobs: 1024, 256, 512, 512, 512 and 1024 MiB.
- **Under this plan,** assuming the head's budget was spent (the hold phase suggests it but does
  not prove it, since a hold can be armed under an earlier head) and its allowance unused:
  - The 2154 MiB job fits the room but is larger than `y`, so the budget withholds it as today.
  - The 2700 MiB job does not fit.
  - The 1024, 256 and 512 MiB jobs pass through the allowance (1792 MiB, CPU 31 of 32).
  - The other three wait for room.

  T1 replays exactly this.

### 1.4 Over three and a half days

Measured by `docs/dev/aira-350-lane-yield-estimate.py` (output in
`docs/dev/aira-350-lane-yield-estimate.out.txt`), over the window 2026-11-15 00:00 to 2026-11-18
12:00 UTC and the retained history rows that carry `wait_ms`:
- **Small jobs (booking `<= 1 GiB`):** 34.4% of 8344 waited at least 10 s, 115.6 wait-hours in
  total. Of the waits of 10 s or more, the median was 75 s and the 90th percentile 352 s.
- **Large jobs:** 52.8% of 2401 waited at least 10 s, 76.4 wait-hours.

§7 says how much of this a lane can address. The answer is far less than these totals.

## 2. Owner decisions, and the changes this plan makes

### 2.1 Restated (owner, 2026-11-18)

1. **The lane.** A small-job lane lives inside the one evaluator, as a rule in the one existing queue
   and ledger. There is no second slice, queue or ledger.
   - A job-level waiter whose `ledgerCharge() <= y` (default 1 GiB) is exempt from the head's freeze
     and its bypass budget while the lane's outstanding bookings plus its own stay `<= x` (default
     4 GiB).
   - It must still fit the ceiling (RAM), CPU and VRAM exactly as today.
   - If the lane is full, or the job is larger than `y`, today's behaviour applies.
   - Lane grants do not count against `head.bypassed`.
2. **Carved out of the ceiling.** Bookings never exceed the ceiling. Nothing is reserved while no
   small job runs.
3. **Default daemon mode only.** The lane is off in ci-shim mode.
4. **Configuration.** `x` and `y` are settable at install, like `--pin-charge`. `off` disables the
   lane. The defaults are on.

### 2.2 Changes made here (each open to veto at the gate)

- **C0 (round 3; needs the owner's yes). Cumulative per head, not concurrent.** "Outstanding bookings
  `<= x`" becomes "at most `x` bytes passed this head". This is the re-base above (§13 Q1).
  Everything else in decision 1 stands:
  - the booking units;
  - the fit on RAM, CPU and VRAM;
  - today's behaviour once the allowance is spent;
  - not counting against `head.bypassed`.
- **C1 (round 3: reverted to the approved contract). Measured in booking bytes.**
  - Round 2 measured eligibility and lane size on the pin (`reserve`). That reversed the approved
    contract ("a waiter whose `ledgerCharge()` <= y", `.aira/tickets/AIRA-350.md`). It also gave a
    false fail: a 983 MiB booking with a 1228 MiB pin was refused.
  - Now eligibility is `ledgerCharge() <= y`, and the allowance counts `ledgerCharge()`. That is the
    unit `head.bypassed` already uses (`admit.go:2714`, `admit.go:483-490`).
  - Round 2's reason was that resize and discount lower a booking below the pin, so a *concurrent*
    lane could exceed its physical bound. That does not arise for a cumulative counter: it counts
    bytes when they are granted, and a later resize gives nothing back.
  - Today the two are identical. `aira install` renders pin-charge `observe`
    (`internal/install/install.go:62`), and observe books at the reserve
    (`internal/daemon/pin_charge.go:120-125`).
- **C2, C3, C7 and guard G: deleted.**
  - Each of them bounded the concurrent lane's footprint. A per-head allowance needs none of them for
    its bound (§3.3).
  - In the model, none changes a head's grant time under the allowance: `cum-min` and `cum-g` agree
    in every witness (§3.3.1).
  - With C3 gone, a small GPU job is eligible, and it must still fit VRAM exactly as today (owner
    decision 1).
- **C4 (membership by flag): deleted.** There is no membership.
- **C5. The allowance is closed behind a pushed head.** Kept. AIRA-309's owner decision (A) is
  "nothing bypasses a pushed head except another push" (queue-order spec §2).
- **C6. The daemon's raw default is off; `aira install` renders the lane on (4G, 1G).** Kept: the
  pin-charge pattern (`pin_charge.go:31-33`).
- **Corrected premise: `--delegate-ram`.**
  - Round 2 said a delegate parent's scope cap is an AIRA-15 containment ceiling many times its
    reserve, citing the comment at `admit.go:1189-1193`.
  - That comment is stale. Since S2a a delegate job is an ordinary confine job whose `memory.max`
    is its reserve, and its pytest workers are separately admitted sibling scopes
    (`internal/daemon/protocol.go:129-136`, `internal/runner/confine_linux.go:1124-1131`).
  - It no longer bears on the design, because the allowance does not count children (§3.4).

## 3. Design

### 3.1 The rule

In the evaluator, inside `if protected && fits` (`admit.go:2689`), one case is added between the
backstop and the budget:

```
switch {
case head.pushSeq > 0:                              // unchanged: nothing passes a pushed head (C5)
    fits = false; heldBy = "head"
case phase == admitFreezeYield:                     // unchanged: the backstop pass
case s.smallLaneAdmitsLocked(head, waiter, &eligible):
    viaSmall = true                                 // allowance grant: not counted in head.bypassed
case addClamp(head.bypassed, waiter.ledgerCharge()) > admitBypassBudget(head):
    fits = false; budgetWithheld = true             // unchanged
    heldBy = "head"; if eligible { heldBy = "small-lane-full" }
}
```

**On a grant:**
- with `viaSmall`: `head.smallBypassed = addClamp(head.smallBypassed, waiter.ledgerCharge())`, plus
  the journal line (§3.6);
- otherwise today's code: `head.bypassed += waiter.ledgerCharge()` when protected
  (`admit.go:2710-2715`).

**`smallLaneAdmitsLocked(head, w, &eligible)`** is true only when all of these hold:
1. **On:** `s.smallLaneCap > 0 && !s.shimMode()`. This is belt and braces with Serve's forced-off,
   as `pinChargeFor` does at `pin_charge.go:121` beside `server.go:420-425`.
2. **Eligible:** `w.ledgerCharge() <= s.smallLaneJobMax`. It sets `eligible = true`. Being
   job-level is already implied by `protected`.
3. **Within the allowance:** `addClamp(head.smallBypassed, w.ledgerCharge()) <= s.smallLaneCap`.

**Placement.**
- **After the backstop case**, so a backstop pass is exactly today's, and its grants count in
  `bypassed` as now.
- **Before the budget case**, so small jobs do not spend the budget that larger jumpers need (T4).
- **The fall-through is today's rule.** A small job the allowance refuses goes on to the budget: it
  can still pass within `reserve/2`. If the budget refuses it too, it is withheld and arms the hold
  exactly as today.

**`heldBy`** is a per-waiter, **diagnosis-only** string, written in the refusal branch next to
`noteGrantableLocked` (`admit.go:2760`). No admission decision reads it, the same discipline as
`lastGrantable` (`admit.go:261-280`).
- It is `""` when the waiter was refused on capacity, or when no head protected it.
- It is overwritten every pass, so it describes the last evaluation.

### 3.2 State

- **`admitWaiter.smallBypassed int64`.** This is AIRA-307's `bypassed` pattern verbatim
  (`admit.go:283-300`).
  - It is written only by the evaluator, under `queue.mu`.
  - "Reset when the head is granted" needs no code: the next head is a different waiter.
  - A head displaced by a push or by a high waiter resumes its counter when it is head again.
  - A daemon restart drops it with its waiter. This fails open: a re-enqueued head starts at zero,
    like `bypassed`.
- **`admitWaiter.heldBy string`.** Diagnosis only (§3.1).
- **`Server.smallLaneCap`, `Server.smallLaneJobMax` (int64).** Read once in Serve (§3.5). Zero means
  off.
- **Nothing else.** Nothing is stored on the queue, nothing is derived, and no lease carries lane
  state.

### 3.3 The bound

**Claim.** Let H be an unpushed head. While H is the head, the job-level grants made past it outside
backstop passes total at most:
- `H.ledgerCharge()/2` of any size (the AIRA-307 budget), plus
- `x` of jobs whose booking is at most `y` (the allowance).

Both counters live on H's waiter and only grow.

**Proof.** Every job-level grant past H passes through the `switch`, and each case is accounted:
- **Pushed head:** none.
- **Backstop pass:** excluded from the claim, as in AIRA-307.
- **Allowance:** the grant is made only if `smallBypassed + charge <= x`, and then adds its charge.
- **Budget:** unchanged.

Sub-reservations are not job-level, and the exclusivity gate and the restart freeze precede the
`switch` (`admit.go:2592-2621`). ∎ This is AIRA-307 invariant 2 with a second term.

**What it means for H's wait.** Compared with today, H can find at most `x` more bytes of job-level
bookings ahead of it, granted since it became head. Each is a lease that ends when its job ends. So
the allowance delays H by at most the longer of:
- the time those leases take to end;
- the time the other bookings take to drain an extra `x`, when H would otherwise have fitted
  sooner.

**What it does not change.** These are the premises AIRA-307 already accepts (queue-order spec
§3.5), now stated as premises rather than excluded:
1. **Backstop passes grant every fit once per hold, ahead of the head.**
   - In the model (W3), today's rules starve the reviewer's backstop-refill head in 7 of 8 phases.
   - The allowance also starves it in 7 of 8 (a different phase is spared).
   - The round-2 concurrent lane starves it in all 8.
2. **Sub-reservations are exempt and uncounted.** A suite parent that passes H, through the budget
   today or through the allowance, brings its workers past H for the suite's lifetime.
   - In the model, W8c (allowance) and W8d (today, fresh budget) both give 900 s.
   - The allowance raises how many suite parents can pass a head (§9 R1).
   - **Measured:** 753 of 8349 small confine rows in the window (9.0%) are `aitest-parent` rows
     (`aira-350-round3-queries.out.txt` part 2). That is a lower bound on suite parents, because a
     `make test-engine` parent does not carry that signature.
3. **High-priority and pushed arrivals are evaluated ahead of H** (`admitOrderLocked`). Each has its
   own counters, so a stream of them can pass H without limit. This is pre-existing.
4. **Leases that never end, and the physical floor** in `checkedAvailable` (`admit.go:2906-2925`).
   Both are pre-existing.

**What the allowance costs a head (estimated).**
- **The drain.** The extra drain is at most `x` per head episode, and only when the allowance is
  used and the head would otherwise have fitted sooner.
  - The window's average release rate is 2.71 GB a minute: 12711 GiB of bookings started in 84 h
    (`aira-350-round3-queries.out.txt` part 4).
  - That is a lower bound on the rate, since rows are lost.
  - So `x = 4 GiB` is up to about 1.6 minutes.
  - The 3.6 GB a minute in the queue-order spec is from 2026-10-12 and is not used here.
- **A suite lifetime**, when a suite parent passes through the allowance (premise 2).
- **The model only (W0, not evidence):**
  - on the 13:55 queue the head waits 240 s instead of 60 s;
  - the two mid-size jobs wait 60 s longer;
  - the small jobs' mean wait falls from 157 s to 47 s.

#### 3.3.1 Counterexamples, run in the model

`docs/dev/aira-350-lane-sim.py` models the evaluator's rules (it is not the Go evaluator) with a 5
minute `maxHold`, backstop passes and one pass per second. It reports the head's grant time. Its
designs are:
- `off`: today.
- `spec`: the round-2 design (concurrent lane, guard G, C2, C3, C7, sum cached).
- `cum-min`: this plan.
- `tb-min`: Alternative B.

`spec-fresh`, `tb-full` and `cum-g` are in the output.

| # | Witness (round-2 finding) | `off` | `spec` | `cum-min` | `tb-min` |
|---|---|---|---|---|---|
| W1 | Stale lane sum: a member's child grants inside the pass (1) | — | candidate granted, lane tree 5120 MiB over `x`, head plus tree 13312 MiB over a 12288 MiB ceiling | no lane sum exists | sum recomputed per decision |
| W2 | Zero-VRAM lane parents with GPU children, VRAM-short head (2) | 30 s | 3016 s (arrivals stop at 3000 s) | 76 s | 616 s |
| W3 | Backstop refill of rotating 5 GiB leases (3), phase sweep of 8 | starved in 7 of 8 | starved in 8 of 8 | starved in 7 of 8 | 605 s in all 8 |
| W8 | Lane-admitted suites with 4 short-lived workers (8) | 20 s (fresh budget: 140 s) | 70 s | 150 s | 150 s |
| W8c | Suites with a long-lived 32-worker pool (8) | 20 s (fresh budget: 900 s) | 900 s | 900 s | 900 s |
| W12 | 1.5 GiB of never-ending leases, 39 GiB head (12) | 210 s | never | 210 s | 610 s |

Other cases:

| Case | Under this plan |
|---|---|
| Repeated refill by small jobs | At most `x` per head, then today's rule (W2, W12). |
| High-priority head | An ordinary unpushed head with its own allowance (AIRA-340; T18). |
| Pushed head | Closed (C5; T8). |
| Head larger than `ceiling - x` | No longer special: at most `x` of small jobs pass it, and each ends. |
| CPU- or VRAM-short head | At most `x` bytes of small jobs pass it. They take slots or VRAM until they end, which is the same RAM-bytes proxy AIRA-307's budget already uses for such heads (`admit.go:478-481`). |
| `y > head` | The proof does not use `y`. |
| Ceiling shrinks | As today: H stops being head if it can no longer fit an empty slice (`admit.go:2770`). |
| Daemon restart | Counters are dropped with their waiters, which fails open (§3.2). No lease carries lane state. |
| Many tiny jobs | `x` of tiny jobs also raises the headroom by 64 MiB each (`admit.go:1041-1051`). Their number is bounded by the CPU ceiling, one slot each: 32 slots is at most 2 GiB. Each ends. |
| Sub-reservations | Unchanged: exempt, uncounted, never use the allowance (T11). |

### 3.4 Interactions

| Interaction | Decision | Reason / code |
|---|---|---|
| Pushed head (`--front`) | Closed. | C5; the pushed case precedes the allowance case. |
| `--exclusive` drain or hold | Unchanged: the allowance never applies to a gate-blocked waiter. Tested (T17). | The gate `continue`s before the head logic (`admit.go:2604-2621`). |
| `--priority high` | A high head has its own allowance. Small waiters behind it use that one. Tested (T18). | A high head is an unpushed head (AIRA-340, `admit.go:302-311`). |
| Fairness-freeze duty cycle | Allowance grants never set `budgetWithheld` and never arm. The backstop pass is unchanged because the allowance case follows it. A small job the allowance refuses and the budget withholds arms as today. Tested (T12, T19). | The arm stays guarded by idle (`admit.go:2762`); no new anchor write. |
| AIRA-324 discount | Eligibility and the counter are in `ledgerCharge()`. The physical-fit rule is unchanged, because it is part of `fits`, computed before the head rule (`admit.go:2663-2666`). | C1 reverted. |
| Sub-reservations, worker-admit | Exempt from the head rule, never use the allowance, never counted. | `worker_admit.go:162`, `worker_admit.go:475`. |
| Re-declare onto a live lease | Nothing to carry: no lease holds lane state. | — |
| Daemon restart, ARDR establish | Counters are lost with their waiters (fail-open). No frame change. | §3.2 |
| `confine --resize` | No effect: the counter records bytes at grant. | — |
| `confine --kill`, reaper, client EOF | No effect on the counter, which is cumulative. | T6 |
| Restart freeze | Unchanged: no grants at all during it. | `admit.go:2592-2595` |
| Fail-fast latch | Not reachable: shim only, and the lane is off in shim. | `admit.go:2494` |
| ci-shim mode | Off: `smallLaneCap` is forced to 0 in Serve, and the predicate checks `!shimMode()`. | Owner decision 3. ci-shim is ledger-only (`admit.go:2636-2640`). |

### 3.5 Configuration (the pin-charge pattern)

**Daemon.** `smallLaneConfigFromEnv()` lives in a new `internal/daemon/small_lane.go`, beside
`pinChargeConfigFromEnv` (`pin_charge.go:53-90`), and has the same mode-then-the-rest shape:
- **`AIRA_DAEMON_SMALL_LANE`** is the per-head allowance `x`:
  - unset, empty or `off` means off (the raw default, C6);
  - otherwise the value is parsed by `runner.ParseMemorySize`;
  - zero or a parse error is refused:
    `E_CONFIG_INVALID: AIRA_DAEMON_SMALL_LANE must be off or a byte size such as 4G`.
- **`AIRA_DAEMON_SMALL_LANE_JOB_MAX`** is `y`:
  - it is read only when the lane is on, so a typo cannot stop a daemon that did not ask for the
    lane (the `pin_charge.go:69-73` ordering);
  - unset means 1 GiB;
  - anything below 1 MiB (`runner.MinPinnedScopeCap`) or above the lane size is refused:
    `E_CONFIG_INVALID: AIRA_DAEMON_SMALL_LANE_JOB_MAX must be a byte size from 1M up to the lane size,
    such as 1G`.
- **Serve** reads both next to the pin-charge read (`internal/daemon/server.go:416-429`):
  - in shim mode it forces the lane off;
  - it logs `aira daemon: small-job lane 4G per held head for jobs up to 1G` when the lane is on.

**Install.** Everything below copies the pin-charge pattern:

| Step | Pin-charge site copied | What the lane adds |
|---|---|---|
| Flags | `install.go:154-158` | `--small-lane SIZE\|off` and `--small-lane-job-max SIZE`, parsed with the pin-charge flags (`install.go:471-492`) and refused with `E_INSTALL_ARGUMENT_INVALID` (`install.go:32`, `argumentInvalid` at `install.go:2568`). |
| Option not given | `install.go:640-650` | The installed unit's value (`installedEnvironmentRE`) is kept. |
| Ship defaults | `install.go:695-706` | `4G` and `1G`, validated as the daemon does. |
| Re-exec forwarding | `install.go:890-895` | Given flags only. |
| Unit render | `renderDaemonUnit`, `install.go:1632-1675` | Re-validates. Two placeholders and two lines in `internal/install/assets/aira-daemon.service.in` (beside lines 12-13). |
| `--status` | `install.go:2505-2508` | Two `reportDaemonMode` lines. |
| Shim environment | `internal/install/mode.go:321` | `AIRA_DAEMON_SMALL_LANE=off` is added. |
| Usage string | `internal/core/core.go:2241` | The two options. |

No upper bound is placed on `x`. A larger `x` is a direct trade against heads: each head can be
passed by that many more bytes of small jobs. The help text says so.

### 3.6 Observability (minimal)

- **Reply fields.** `runner.ConfineSliceReserve` (`internal/runner/confine_manage.go:586`) gains
  three optional fields, filled in `admitSliceSnapshotFor` (`admit.go:1302`) in the same locked
  walk:
  - `small_lane_cap_bytes` and `small_lane_job_max_bytes`: present when the lane is on;
  - `small_lane_used_bytes`: the current head's `smallBypassed`. It is present only while a head
    exists, read from the waiter `queue.freezeHolderSeq` names (`admit.go:828`, as
    `admitReportedPhaseLocked` does at `admit.go:2809-2821`).
  - When the cap is absent (lane off, ci-shim, or an older daemon), nothing is rendered.
- **One per-waiter field.** `held_by` (`head` or `small-lane-full`) is taken at the same
  queued-scope match as `queue_position` (`admit.go:1352-1364`), and only for a caller that named
  its scope id.
- **`aira confine --list` text.** One line after `slice queue:` (`cmd/aira/main.go:3765-3779`), when
  the cap is present:
  - with a head: `small-job lane: 1792M of 4096M used past the current head (jobs of at most 1024M
    pass a held head until it is used)`;
  - without one: `small-job lane: 4096M per held head for jobs of at most 1024M`.
  - The `hold` note on the `slice queue:` line (`main.go:3773`) gains "jobs of at most 1024M still
    pass through the small-job lane until it is used".
- **Wait line** (`confineQueueNote`, `internal/runner/confine_queue_position_linux.go:151-221`):
  - `held_by=head` adds ", fit at the last check but held behind the head of the queue";
  - `held_by=small-lane-full` adds ", fit at the last check but the small-job lane past this head is
    used up (4096M of 4096M)";
  - absent adds nothing.
- **`aira top` footer** (`cmd/aira/tui_top.go:1546-1547`): `lane 1792M/4096M` is appended while a
  head exists. One conditional append.
- **Daemon journal.** One line per allowance grant:
  `aira daemon: small-lane grant scope=<id> booking=<n> head seq=<n> used=<bytes>/<cap>`. This is the
  post-ship measurement hook (§7.4). It is bounded by job arrivals, not by pass rate.

### 3.7 Generated text (faces)

The pattern is AIRA-340's (`internal/core/skill.go:305-318`): a long `ConfineSmallLaneGuidance` and a
short `ConfineSmallLaneSummary` in `internal/core/skill.go`.
- **The skill:** the sentence at `skill.go:338` ("Without a push, a waiting job that does not fit lets
  smaller jobs past it only up to half its own reserve, ...") is extended with the long form.
- **The install option descriptions** carry the long form, so `aira install --help` and the MCP
  schema have it.
- **The `confine` verb summary** (`core.go:2607`) and the MCP instructions (`cmd/aira/mcp.go:26-29`)
  append the short form.
- **The test:** `TestConfineSmallLaneGuidanceIsOnEveryFace` pins the wording across the faces,
  modelled on `TestConfinePriorityGuidanceIsOnEveryFace` (`cmd/aira/confine_priority_test.go:100`).

Draft text:
- **Long:** "A small job (a booking of at most the small-job lane's job size, 1G by default) can pass
  a waiting head that does not fit without using that head's budget, until the small jobs passed
  this way add up to the lane size for that head (4G by default). It is still admitted only if it
  fits the slice's memory, CPU and VRAM ceilings. It never passes a job pushed with `--front` or an
  exclusive drain. Default mode only: off under `--ci=shim`. Set at install with
  `--small-lane SIZE|off` and `--small-lane-job-max SIZE`."
- **Short:** "Small jobs (default at most 1G) can pass a waiting head through the small-job lane
  (default 4G per head), still only when they fit."

### 3.8 Alternative B, if the owner keeps a concurrent lane

**The rule.**
- A concurrent cap over lane members: a `laneGranted` flag, and a lane sum derived from the waiters
  and recomputed before every lane decision (finding 1).
- The lane closes for a head once `now - head.enqueued >= 2 x maxHold`, which is 10 minutes by
  default. `enqueued` is an existing field.
- No G, C2, C3 or C7. In the model, adding all four (`tb-full`) changes one grant time (W8: 70 s
  against 150 s) and none of the bounds.

**The bound.** Lane grants past H stop at `H.enqueued + T`. After that, H's wait is today's plus at
most the lifetimes of the lane members granted before then, a suite lifetime included.

**Measured** (`docs/dev/aira-350-hold-age.out.txt`, 461 holds):
- the head's age when its hold arms has a median of 154 s and a 90th percentile of 612 s;
- with `T = 600 s` the lane is open for 81% of hold time;
- with `T = 300 s`, for 44%.

The 13:16:07 head was already 6m10s old when its hold armed.

**In the model:** W2 616 s, W3 605 s in all 8 phases, W12 610 s, against 30 s, 7 starved, and 210 s
today. In W3 the time-box does better than today. The likely reason is that small jobs no longer arm
holds, so fewer backstop passes refill the slice. That is a property of the model's witness, not
established for production.

**Against the recommendation.**
- More state: a flag on every member, a derived sum, and round 2's restart and re-declare cases.
- A head pays up to `T` even where the allowance would have stopped sooner (W2, W12).

## 4. Invariants

1. **The ledger is untouched.** `outstanding`, `cpuOutstanding`, `vramOutstanding` and
   `outstandingJobs` are still written only by `rederiveLedgerLocked`. Every allowance grant passed
   today's `fits`, so `Σ booking <= ceiling - headroom` holds as before (carved out of the ceiling).
2. **The per-head bound.** For every unpushed head H, outside backstop passes:
   - allowance grants past H total at most `x`;
   - other job-level grants past H total at most `H.ledgerCharge()/2`.

   Both counters only grow.
3. **Allowance grants never change `head.bypassed`, never set `budgetWithheld`, and never arm the
   hold.** AIRA-307 invariant 2 holds verbatim for non-allowance grants.
4. **A pushed head is never bypassed** by a job-level waiter (AIRA-309 invariant 3, unchanged).
5. **A backstop pass is exactly today's.**
6. **All lane state is on the head waiter.** It cannot outlive the waiter. A restart fails open, and
   nothing needs operator clearing.
7. **Off is today's behaviour exactly.** `smallLaneCap == 0` (the raw default and ci-shim) makes the
   allowance case unreachable, and every existing test passes unchanged.
8. **Precedence is unchanged.** The restart freeze, the fail-fast latch, the exclusivity gate, the
   pushed head and the backstop pass all come before the allowance.

## 5. Wire and protocol impact

- **ProtocolVersion stays 15** (`internal/daemon/protocol.go:218`). The admit request, the admit
  grant and the ARDR re-declare frames are unchanged.
- **The `confine-list` reply gains four optional fields** (§3.6): three on `slice_reserve`, and
  `held_by`.
  - The client decodes it with `json.Unmarshal` (`confine_queue_position_linux.go:388`), not with
    `DisallowUnknownFields`, so an old client ignores them.
  - A new client against an old daemon finds them absent and renders nothing.
- **The behaviour change is daemon-side.** It takes effect only when the installed unit carries the
  new `Environment=` lines and the daemon restarts (§12).

## 6. Tests (each must fail against the wrong implementation)

All evaluator tests use the AIRA-228/307 seam: a hand-built queue (`queueOf`,
`internal/daemon/admit_queue_order_test.go:42-50`), a fake memory reader and clock, and
`evaluateAdmitQueue` driven directly with no evaluator goroutine.

Common settings, unless a test says otherwise:
- **Headroom:** production, 2 GiB base and 64 MiB per job. `admitTestServer` zeroes both
  (`internal/daemon/admit_test.go:23-24`).
- **CPU:** 16 cores at 2 slots per core (32 slots), through the core-counter seam. **Every held
  lease carries `cpu = 1`.** That is production (one slot per confine job, §1.2). The existing
  `heldLease` has `cpu = 0` (`admit_queue_order_test.go:44-46`) and must not be used unmodified.
- **Lane:** on, `x = 4096 MiB`, `y = 1024 MiB`, set on the Server fields directly.
- **Freeze:** `admitFreezeMaxHold` is the production 5 minutes (`paths.go:66`), and
  `freezeArmedAt = now - 60s`, so the pass is a hold pass.
  - The existing helper `queueOrderServer` sets one minute (`admit_queue_order_test.go:30`). With
    that, the pass would be a backstop pass, and the "today" column would be wrong.
- **Head budget:** preset as spent (`bypassed = admitBypassBudget(head)`).

### 6.1 Evaluator

Every test asserts the exact set of granted waiters, and `smallBypassed` and `bypassed` on the
head.

| Test | Setup | Assertions | Today's code, or the named mutation |
|---|---|---|---|
| **T1 13:55 replay** (regression pin) | `effectiveMaximum = 48640 MiB`. 28 held leases sum to 42443 MiB, one slot each, so room is 2293 MiB (`48640 - (2048 + 29 x 64) - 42443`). A 5120 MiB head. Waiters in order, in MiB: 2154, 2700, 1024, 256, 512, 512, 512, 1024. | Granted exactly the 1024, the 256 and the first 512 (allowance), so `smallBypassed == 1792` and `bypassed` is unchanged. The 2154 is withheld with `heldBy == "head"`. The 2700 and the last three are refused on capacity (room 309 MiB after the grants), with `heldBy == ""`. CPU is at 31 of 32. | Today's code grants **none**. |
| **T1b CPU full** | As T1, but 32 held leases summing 42187 MiB (room still 2293 MiB), so CPU is at 32 of 32. | Nothing granted, and every `heldBy == ""`. | Allowance grants that skip `cpuFits`: three granted. |
| **T2 the allowance binds** | 16 held leases. A 10240 MiB head. Room 8192 MiB. The 14 small waiters of the incident, in MiB: 72, 256, 512, 1024, 512, 256, 1024, 72, 512, 1024, 256, 512, 72, 1024. | Granted: 1-8, 11 and 13, so `smallBypassed == 4056` with 10 jobs. Waiters 9, 10, 12 and 14 are withheld with `heldBy == "small-lane-full"`. | Remove the allowance check: all 14 granted. |
| **T3 boundaries** | — | `smallBypassed + charge == x` is granted, and one byte more is `small-lane-full`. `charge == y` is eligible; `y + 1` is not (`heldBy == "head"`). | `<` / `<=` swaps. |
| **T4 small jobs keep the budget** | Zero headroom. An 8 GiB head with `bypassed = 0`. Room 7 GiB. Three 1 GiB small waiters, then a 3 GiB waiter. | All four granted. `smallBypassed == 3 GiB`, `bypassed == 3 GiB`. | Count allowance grants in `bypassed`, or put the budget case first: the 3 GiB waiter is withheld. |
| **T5 per head** | (a) Head A with `smallBypassed = x` and its budget spent, and a held small waiter. A is granted in pass 1. In pass 2 a new non-fitting head B (budget spent) is ahead of the small waiter. (b) Head A with `smallBypassed = 2 GiB` is displaced by a pushed waiter, which is then granted, so A is head again. | (a) In pass 2 the small waiter passes B, and `B.smallBypassed` equals its booking. (b) Two 1 GiB small jobs pass A, and a third is `small-lane-full`. | (a) A counter on the queue: held. (b) Reset the counter on a change of head: three pass. |
| **T6 cumulative, not concurrent** | The allowance used up by four 1 GiB jobs. Two of them are released through `releaseAdmitWaiterLocked`. A 1 GiB small waiter. | Next pass: still `small-lane-full`. | Give the allowance back on release (the round-2 concurrent lane): granted. |
| **T7 pushed head** | A small waiter behind a pushed head. | It waits in hold, idle and backstop passes, with `heldBy == "head"`. | Evaluate the allowance case before the pushed case: granted. |
| **T8 ci-shim** | A shim server with the lane fields set directly. A small waiter behind a head whose budget is spent. | Withheld. | Drop the `!shimMode()` conjunct: granted. |
| **T9 GPU small jobs** | Two small waiters with `vram > 0`: one fits the VRAM snapshot, one does not. | The first is granted through the allowance. The second is refused on capacity (`heldBy == ""`). | Reinstate C3: the first is withheld. Skip the VRAM fit on allowance grants: the second is granted. |
| **T10 eligibility on the booking (AIRA-324)** | Pin-charge enforce. A waiter with a 1228 MiB pin and a 983 MiB booking that passes the physical-fit rule. | Granted through the allowance, and `smallBypassed` increases by 983 MiB. | Judge eligibility on the reserve: withheld. |
| **T11 sub-reservations** | A 2 GiB sub-reservation of a granted parent, queued behind a head; it fits. | Granted on fit. `smallBypassed` and `bypassed` are unchanged. | Route sub-reservations through the allowance, or count them: a counter changes. |
| **T12 no arm from allowance grants** | Idle phase, only allowance grants. | `freezeArmedAt` stays zero. | Set `budgetWithheld` on the allowance path: armed. |
| **T13 resize** | An allowance-admitted lease's `charge` is lowered to 256 MiB. | `smallBypassed` is unchanged and no extra grant is made. | Recompute the allowance from current charges: one extra grant. |
| **T14 restart** | A head re-enqueued after a daemon restart (a fresh waiter through `enqueueAdmitInternal`), and a lease re-established through `enqueueReDeclare`. | The new head's `smallBypassed == 0`. The lease carries no lane state. | — |
| **T15 diagnosis** | — | `heldBy` is set on refusal and cleared on a capacity refusal, and no decision reads it (the guard-test pattern of `admit_pin_charge_guard_test.go`). | A decision that reads `heldBy`. |
| **T16 fall-through to the budget** | The allowance spent, the budget not. A 1 GiB small waiter. | Granted through the budget, and `bypassed` increases. | End the `switch` on an allowance refusal: withheld. |
| **T17 exclusivity** | (a) An exclusive holder granted (hold). (b) An exclusive drain queued (drain). A small eligible waiter behind each. | The waiter is gate-blocked and not granted. No counter changes. | Evaluate the allowance before the gate: granted. |
| **T18 high-priority head** | A `--priority high` waiter enqueued after three small normal waiters. It does not fit, so it is the head. | The small waiters pass it through its allowance, and its `smallBypassed` holds their total. | Exclude a high head from the allowance: withheld. |
| **T19 backstop pass** | `freezeArmedAt = now - maxHold`, so this is the backstop pass. Small eligible waiters fit. | Granted, counted in `bypassed`. `smallBypassed` is unchanged. | Put the allowance case before the backstop case: `smallBypassed` changes. |

### 6.2 Bounded runs in Go (the model's witnesses, against the real evaluator)

Deterministic multi-pass runs on the same seam, with production `maxHold` and backstop passes and a
one-second clock step. Each runs the lane-off evaluator for comparison. The model's figures
(§3.3.1) are what the builder should expect. Where the Go result differs, the builder reconciles the
model or the test before the gate, because the model is only a model.

| Run | Shape (as the model's witness) | Assertion |
|---|---|---|
| **S1** | W12: 1.5 GiB of never-ending leases, 10 GiB of transient leases, a 39 GiB head, 512 MiB small jobs every 5 s. | The head is granted at the same pass with the allowance as with the lane off. The concurrent mutation (T6's) is still not granted after 6000 passes. |
| **S2** | W2: a VRAM-short head, zero-VRAM 1 GiB parents with 1 GiB-VRAM children, staggered. | The head is granted once the 4 allowance-admitted parents have ended. The concurrent mutation is not granted while parents keep arriving. |
| **S3** | W3: rotating 5 GiB leases refilled by backstop passes, 8 phases. | The allowance starves the head in no more phases than the lane-off run does. The concurrent mutation starves it in all 8. |
| **S4** | W8c: suites with a long-lived worker pool. | With the head's budget spent, the allowance run grants the head at the same pass as the lane-off run with a fresh budget. This pins the suite residual as today's class (§3.3 premise 2). |

**Property check after every pass, in every run:**
- `outstanding <= ceiling - headroom` (the ledger);
- `smallBypassed <= x` on every waiter;
- for every unpushed head, the non-allowance job-level grants made past it outside backstop passes
  total at most `reserve/2`.

### 6.3 Configuration and install

- **Environment table:**
  - `AIRA_DAEMON_SMALL_LANE` unset or `off` is off, and `4G` is 4 GiB; `0`, `-1` and `abc` give
    `E_CONFIG_INVALID`;
  - `AIRA_DAEMON_SMALL_LANE_JOB_MAX` is ignored when the lane is off (a typo is not refused). It is
    refused below 1 MiB and above `x`, and defaults to 1 GiB.
- **Serve:** shim mode forces the lane off. Startup log line.
- **Install:**
  - the flags parse, and bad values are refused with `E_INSTALL_ARGUMENT_INVALID`;
  - a value is kept from the installed unit when not given, and the ship defaults `4G`/`1G` are
    rendered;
  - `renderDaemonUnit` re-validates, and given flags are forwarded on re-exec;
  - `--status` prints both lines, and the shim environment carries `AIRA_DAEMON_SMALL_LANE=off`.

### 6.4 Observability and faces

- **Snapshot:** cap and job max are present with the lane on and absent with it off. The used
  figure is present only while a head exists. `held_by` is reported only for the named queued
  scope.
- **Renderers:** both `--list` line forms and the `hold` note, the wait-line clauses for both
  `held_by` values and for absence, the `aira top` footer, and the journal line.
- **Faces:** `TestConfineSmallLaneGuidanceIsOnEveryFace` covers the skill, `install --help` and its
  JSON, the `confine` summary, and the MCP instructions.

### 6.5 Mutation list, by direction

- **False pass** (grants what must wait):
  - remove the allowance check (T2);
  - give the allowance back on release, i.e. a concurrent lane (T6, S1, S2, S3);
  - the allowance case before the pushed case (T7);
  - on in shim (T8);
  - skip the CPU or VRAM fit on allowance grants (T1b, T9);
  - route sub-reservations through the allowance (T11);
  - the allowance before the exclusivity gate (T17);
  - recompute the allowance from current charges (T13);
  - reset the counter on a change of head (T5b);
  - `<` / `<=` swaps (T3).
- **False fail** (holds what should be granted):
  - delete the allowance case (T1, T2);
  - count allowance grants in `bypassed`, or the budget case first (T4);
  - a counter on the queue (T5a);
  - judge eligibility on the reserve (T10);
  - reinstate C3 (T9);
  - no fall-through to the budget (T16);
  - exclude a high head (T18);
  - set `budgetWithheld` or arm on allowance grants (T12, which over-arms; a false fail for later
    waiters);
  - a `heldBy` value read by a decision (T15).
- **Moved:** the allowance case before the backstop case (T19).
- **The gate** runs each mutation and records it RED, by the `docs/dev/aira-324-mutation-check.py`
  pattern: a committed `docs/dev/aira-350-mutation-check.py`.

## 7. Expected yield

### 7.1 Measured

From `docs/dev/aira-350-lane-yield-estimate.out.txt` (window 2026-11-15 00:00 to 2026-11-18 12:00
UTC; the journal holds 461 holds and 456 backstop passes):
- **Population** (§1.4). Small jobs: 115.6 wait-hours, waits of 10 s or more with a median of 75 s.
  Large jobs: 76.4 wait-hours.
- **Overlap with a hold does not discriminate.**
  - 90.4% of small waits of 10 s or more overlapped a hold window, covering 100.6 of 114.4
    wait-hours.
  - For large jobs, which no lane affects, the figures are 91.5% and 87.8%.
  - So "overlaps a hold" marks a busy slice, not a wait the freeze caused. 100.6 hours is **not** a
    useful upper bound on what a lane can save.
- **Small grants at backstop passes (an association, not a saving).**
  - 130 small waits ended within [-1 s, +3 s] of a backstop pass. Shifted control windows (+60 s
    and -60 s) catch 25 and 34.
  - So about 100 grants are associated with backstop passes: 6.1 against 1.2 wait-hours, about 5
    wait-hours in excess.
  - A job granted at a backstop pass may first have fitted at that pass, so not all of this is
    saveable. The script says so (`aira-350-lane-yield-estimate.py:35`).
  - These jobs had a median wait of 130 s. They waited through part of a hold, not a whole
    5-minute hold.
- **Most holds ended with no small job released.** Only 85 of 456 backstop passes have a retained
  small-job grant within [-1 s, +3 s] of them. At the other 371, no retained small job was granted
  (rows are lost, so this is of the sample).
- **Batch size at those 85 passes:** the released small jobs' footprint (booking + 64 MiB) had a
  median of 1.06 GiB, a 90th percentile of 2.12 GiB and a maximum of 6.00 GiB. In 82 of them (96%)
  it was at most 4 GiB, so one head's default allowance would have covered the batch.
- **Heads:** the median reserve was 4 GiB, the 90th percentile 10.9 GiB and the maximum 25.3 GiB.
  The head's age when its hold armed had a median of 154 s and a 90th percentile of 612 s
  (`aira-350-hold-age.out.txt`).
- **Small jobs' run time** (`aira-350-round3-queries.out.txt` parts 2-3):
  - the median is 4 s and the 90th percentile 125 s;
  - 5 of 8349 ran over an hour, 3.9 of 110.7 GiB-hours.
  - Rows are written when a job ends, so the 5-day servers of the 13:55 snapshot are absent: 8
    servers older than 13 hours, 1536 MiB.

### 7.2 Confounders the store cannot separate

- **CPU slot saturation.** Both live states had 28-32 jobs against 32 slots (§1.2, §1.3). Every
  confine job takes at least one slot, so the job count is a lower bound on slots in use. No CPU
  ledger is logged, so a CPU-refused small job cannot be told apart from a freeze-held one, except
  that a backstop-pass grant needed CPU at that pass.
- **A full ledger.** At 13:47 a 256 MiB job waited with "45847M of 45760M granted" (client wait
  line).
- **Pushed heads.** The 13:21:07 backstop pass was lost to one (§1.2).

### 7.3 Benefit against cost

| Side | Figure | Basis |
|---|---|---|
| Benefit: small waits associated with backstop passes | about 100 waits, about 5 wait-hours in 3.5 days, not all saveable | measured association (§7.1) |
| Benefit: waits ended by a head's grant rather than a backstop pass | not caught by the script | unknown |
| Benefit: the 13:55 queue, in the model | small mean wait 157 s down to 47 s | model (W0) |
| Cost: a head episode in which the allowance is used | at most `x` of extra drain, about 1.6 minutes at 2.71 GB a minute | estimated (§3.3) |
| Cost: across all 461 holds, if every head lost the full drain | at most about 12 head-hours | estimated upper bound |
| Cost: a suite parent passing through the allowance | up to a suite lifetime for that head (W8c: 900 s) | model; today's class |
| Cost: mid-size waiters (`y` < booking < head) | they lose room to allowance grants (W0: +60 s each) | model |

**Best estimate.** The benefit is near the low end, a few wait-hours in 3.5 days, plus the waits
that end at a head's grant, which the store cannot see. The worst-case cost bound is larger than
that. **The evidence does not establish a net gain.** The case for building is:
- the change is one counter;
- it is switchable off (`aira install --small-lane off`);
- §7.4 measures it after shipping.

### 7.4 Measured after shipping (no new telemetry beyond the journal line)

- Re-run `aira-350-lane-yield-estimate.py` with `--since <cutover>` over 7 days, and compare with
  the 3.5-day baseline:
  - the mean wait per small job and per large job;
  - the share of small jobs waiting at least 10 s;
  - holds per day.
- Join the journal's `small-lane grant` lines with `wait_ms` to see what the allowance admitted
  directly.
- **Keep threshold.** Let `saved = (small mean wait before - after) x small jobs after` and
  `cost = (large mean wait after - before) x large jobs after`.
  - Keep the lane if `saved > cost` and `saved` is at least 10% of the small jobs' wait-hours after.
  - Otherwise switch it off and report.
  - This is reported to the owner. It is not an automatic gate.

**Limits.** The store keeps 20 rows per exact signature, so frequent commands have lost rows: these
are shares of the retained sample. `budget` (the scope cap) stands in for the booking, which is exact
for ordinary confine jobs and with pin-charge in observe.

## 8. What this does NOT fix

- **Ledger over-booking.** On this box the ledger books 2-5x what jobs use:
  - at 13:22, about 45 GiB booked against a `sliceAnon` of about 15-16 GB (journal);
  - at 13:55, 41.4 GiB booked against about 8.7 GiB in use (§1.3).

  When the ledger is full, the allowance admits nothing. The levers are estimate accuracy:
  - AIRA-318 (page-cache peaks inflate history, held);
  - AIRA-347 (cap headroom for auto-sized jobs, held);
  - AIRA-341 (the cold default after the p90 prior's deletion, done).
- **CPU slot saturation** (§7.2). The allowance never bypasses the CPU fit.
- **AIRA-351** (warm-starting a cold subject from the same repo's history). Separate, held,
  measure-first, and blocked by this ticket.
- **Waits** on an exclusive drain or hold, or behind a pushed head (by design, C5).
- **Large jobs' own waits.** The allowance can lengthen a head's wait (§3.3).

## 9. Risks

- **R1. Suite parents through the allowance.** A test suite started through the allowance brings its
  exempt workers past the head for the suite's lifetime. Today a suite started through the budget
  does the same (W8c and W8d). The allowance raises the number of suite parents that can pass each
  head, from about `R/2` to about `R/2 + x` of 1 GiB parents. 9.0% of small rows are suite parents,
  at least. §13 Q2.
- **R2. Heads wait longer** by up to `x` of drain per episode (§3.3, §7.3). Watched by §7.4.
- **R3. Mid-size waiters** (`y` < booking < head) lose room that allowance grants take, before the
  next backstop pass.
- **R4. `y` is inclusive at 1 GiB, which is exactly the cold default.** Every unpinned first run
  (`fallback:no-history`, 1 GiB) is eligible. That is intended, since they are the incident's
  population, but it means four cold jobs use a head's whole default allowance.
- **R5. Operator surprise:** `held_by=head` for a small job means the pushed head, ineligibility,
  or the budget held it. The wording "held behind the head" is true in each case.
- **R6. Diagnosis drift:** `heldBy` and the used figure describe the last pass, the same property as
  `lastGrantable`.
- **R7. A stream of high-priority heads** each brings a fresh allowance. That is the same shape as
  today's per-head budget (§3.3 premise 3).

## 10. Deferrals (explicit, not built here)

- **Children of allowance-admitted parents subject to the head rule** (against R1). This needs a
  deadlock analysis against pushed heads, which is why AIRA-307 exempted sub-reservations. Build it
  only if §7.4 shows the suite cost.
- **A CPU-outstanding field in `slice_reserve`,** and a per-instant record of room and CPU. Without
  them a freeze wait cannot be told apart from a capacity wait (§7.2).
- **Alternative B** (§3.8), unless the owner keeps the concurrent semantics.
- **A lane for ci-shim mode.**
- **Telemetry columns in `confine_peak_history`** (for example an allowance-grant flag); the journal
  line covers the measurement.

## 11. Cut list (considered and cut)

| Option | Why cut |
|---|---|
| A second slice or ledger for small jobs (the owner's first idea) | AIRA-228 collapsed to one queue and ledger, and the owner approved a rule in the one ledger instead. |
| **The concurrent lane (round 2)** | Unsound in production: witnesses W2, W3, W12 and W8c, run in the model (§3.3.1). |
| Guard G, with its RAM, per-job-headroom (C2) and CPU arms | It bounded the concurrent lane's footprint; the allowance needs no footprint bound. |
| GPU jobs ineligible (C3) | Not needed for the bound; the owner's rule is "fit VRAM exactly as today". |
| Counting a member's children (C7) | Did not stop the suite case (W8c: 900 s with it); induced findings 1, 2 and 13. |
| Lane membership flags and a derived lane sum | Only a concurrent lane needs them. |
| Eligibility and size on the pin (round-2 C1) | Reversed the approved contract (finding 4). |
| Raise the AIRA-307 budget for every job (e.g. `max(R/2, 4 GiB)`) | Large jumpers would spend it too. The allowance is the same bound, kept for small jobs. |
| Alternative B as the primary design | More state, and a head pays up to `T` (§3.8). It is kept as the owner's option. |
| Runtime-aware backfill (start a small job only if it ends before the head could fit) | Needs runtime predictions, a judgement AIRA does not make. |
| A lane-grant column in history | Telemetry only; the journal line suffices (§7.4). |
| A "lane full" refusal code | The allowance never refuses; a refused job falls through to today's rule and waits. |

## 12. Rollout

The change is on the daemon side, and it needs the new unit lines:
1. `aira install`, which re-renders the unit with the defaults `4G`/`1G` and keeps the other options'
   installed values;
2. `systemctl --user restart aira-daemon.service`, run explicitly because an idempotent install skips
   the restart;
3. `aira skill install`, for the generated text.

Before the restart, follow the shared daemon-restart care: check `aira confine --list` and give the
peer sessions a heads-up. After it, `aira install --status` shows the two lane lines and
`aira confine --list` shows the `small-job lane:` line.

Release notes name the new install options, the four reply fields and the journal line. Protocol 15
is unchanged.

## 13. Open questions (for the owner, through the plan gate)

1. **C0, the re-base.** Choose one:

   | Option | What small jobs get | What heads pay | State |
   |---|---|---|---|
   | (a) **Per-head allowance** (recommended) | Up to `x` past each head, then today's wait for a backstop pass | At most `x` per head, the AIRA-307 class | one counter on the head |
   | (b) Concurrent lane closed `2 x maxHold` after the head's enqueue (§3.8) | Continuous passing for up to 10 minutes of each head's wait | Up to `T` plus lane members' lifetimes | member flag, derived sum, `T` |
   | (c) Do not build yet | Nothing | Nothing | none |

   For (c): the evidence that the freeze, rather than CPU or a full ledger, is what holds small jobs
   is weak (§7). Over-booking (AIRA-347, AIRA-318) is the larger lever.
2. **The suite residual (R1).** Accept it as today's class, or ask for the deferred rule (§10).
3. **The default `x = 4 GiB` per head.** It covered 96% of the batches released at backstop passes.
   It is tunable at install.
4. **C5:** the allowance is closed behind a pushed head.
5. **C6:** the raw daemon default is off, and the product default is on through install.
6. **Inclusive `y`** captures the 1 GiB cold default (R4).
7. **The journal line per allowance grant:** keep it, or measure only from `wait_ms` before and after.

## 14. Review log

### 14.1 Round 2 (14 findings), grouped by root cause

**A. Induced by the round-1 fix C7** (the lane tree includes exempt children): findings 1, 2, 8 and
13, and the property halves of 5 and 10.
- Finding 8 breaks the bound that the round-1 fix commit (`d90f2ac4`) claimed C7 restored. That is
  the non-convergence trigger.
- **Closed by the collapse:** there is no lane tree.

**B. A theorem true only outside production:** findings 3, 12, and the chaining in 8.
- The claim "the head cannot starve" and the unconditional 1.3-minute cost are **withdrawn**.
- The allowance restores AIRA-307's argument, and the production mechanisms are stated as premises
  (§3.3).

**C. Evidence overclaims:** findings 6, 9, 11 and 14. Rewritten (§1.2, §7).

**D. Contract and premise:** finding 4 (C1 reverted) and finding 7 (premise corrected).

**E. Tests that cannot fail, or fail on a correct build:** findings 5, 10 and 13. Rewritten (§6).

### 14.2 Per finding

| # | Sev | Verdict | Evidence checked | Disposition |
|---|---|---|---|---|
| 1 | P1 | Confirmed | Children grant inside the pass at `admit.go:2688`/`2699`. Model W1: candidate granted, tree 5120 MiB. | Closed: no lane sum. |
| 2 | P1 | Confirmed | `admit.go:3395-3402` accepts `vram` and `admit.go:3494-3500` accepts `parent_scope_id` on the same request. Model W2: `spec` 3016 s. | Closed: the allowance gives 76 s, and children pass a head as today's children of budget-admitted parents do. |
| 3 | P1 | Confirmed for the concurrent lane (W3: 8 of 8 phases starved). Partly wrong: "without the lane the head fits at the first release" holds in 1 of 8 phases. | W3 phase sweep: today's rules starve 7 of 8, the backstop-refill class of queue-order §3.5. | Claim withdrawn. The allowance matches today (7 of 8). The cost bound is restated (§3.3). |
| 4 | P2 | Confirmed | `.aira/tickets/AIRA-350.md:4` says "ledger charge <= y". | C1 reverted to `ledgerCharge()`. T10 now asserts eligibility on the booking. |
| 5 | P2 | Confirmed | Round-2 §6.2's property contradicted invariant 2. There were no exclusive, high-head or backstop tests. | Property rewritten (§6.2). T17, T18, T19 and T16 added. |
| 6 | P2 | Confirmed | The script's own LIMITS (`:35`); the matched median wait is 130 s (`out.txt` part 2). | Relabelled an association. "Whole 5-minute hold" removed (§7.1). |
| 7 | P2 | Confirmed | `protocol.go:129-136`, `confine_linux.go:1124-1131`; the `admit.go:1189-1193` comment is stale. | Premise corrected (§2.2). C7 deleted in any case. |
| 8 | P1 | Confirmed | Model W8c: `spec` (with C7) 900 s against 20 s today with the budget spent. **Also:** today's rules with a fresh budget give the same 900 s (W8d). | The suite term is today's class, widened by the allowance. Stated as residual R1, and as §13 Q2. |
| 9 | P2 | Mostly confirmed. One figure corrected: the snapshot shows 8 (not 9) of the 12 job-level small scopes older than 13 hours, and 5 (not 6) at about 5 days (ages 433762 x 4 and 432637; then 79205, 57496 and 50147 s). | `round3-queries` parts 2-4: small rows' run time, the `aitest-parent` share and the release rate. | Benefit and cost in one table (§7.3). Silting and a lane-closing suite do not apply to a per-head allowance. Mid-size waiters added to the cost (R3). |
| 10 | P2 | Confirmed | Round-2 T6 mutations are equivalent at its numbers. The 6.2(a) steady state is below `x`. 6.2(d) had no pass number. | G is deleted, so its mutations are moot. New tests cover the head change, the backstop classification, exclusivity and high-priority heads. S1-S4 assert pass numbers against lane-off runs. |
| 11 | P1 | Confirmed | Journal `confine-front` at 13:21:00 and 13:22:49. The yield line at 13:21:07 names 14816. History: 14816 granted at 13:21:47 after 317 s. One slot per job (`admission_linux.go:455`); `heldLease` has `cpu = 0`. | §1.2 rewritten as a timeline. T1 is built from 13:55 with one-slot leases, and T1b covers 32 leases. |
| 12 | P2 | Confirmed | Model W12: `spec` never, today 210 s. | Closed: the allowance gives 210 s. Option (a) of the fix hint is Alternative B (§3.8). |
| 13 | P2 | Confirmed (same as 5) | — | Property rewritten. |
| 14 | P2 | Confirmed | Large-job control: 91.5% overlap. Only 85 of 456 passes released a small job. | Bound called non-discriminating. Best estimate at the low end. CPU named as a confounder. Keep threshold given (§7.4). |

**Fix-induced findings:** 4 of 14 (1, 2, 8 and 13) came from the round-1 C7 fix.

**Commands run this round** (all under `aira confine`; no `hint:` line appeared in any trailer):
- the model (`docs/dev/aira-350-lane-sim.py`);
- the hold-age script;
- `docs/dev/aira-350-round3-queries.sql` against a store copy;
- `journalctl` reads.
