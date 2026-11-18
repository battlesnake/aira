# Small-job lane: small jobs pass a held head within a per-head allowance (AIRA-350)

Status: plan, round 4, for plan review (Codex Sol at `xhigh`, Gemini) and the Fable plan gate.
**Not buildable until the owner answers.** The measurement (§M) needs §13 Q1; the allowance (§3)
needs §13 Q1-Q4. This round finds that no lane design meets the owner's requirement as stated, and
recommends measuring before building (below).
Ticket AIRA-350, milestone v0.46. Correctness-critical (admission): two-loop, Opus builds, Fable
reviews. Builds on AIRA-307/309 (queue order, `docs/superpowers/specs/2026-10-13-aira-queue-order-design.md`)
and AIRA-228 (one queue, one ledger). AIRA-351 (warm-starting cold subjects) is separate, held, and
blocked by this ticket.

**Which approvals cover what.** The owner's "Ok sounds good" greenlit the round-1 lane (a concurrent
cap). The owner's later "Option (A) after that's released also if it makes sense to you" (2026-11-18)
names AIRA-351's warm start, to follow the lane's release; it is not an option of this spec's §13.
Both predate rounds 3 and 4, so neither covers the changes listed in §2.2 (C0a-C0c).

Code references are to the worktree at `ae446ef1` (master). Data:
- committed: `docs/dev/aira-350-*` scripts and their `.out.txt` outputs;
- not committed, because they hold other sessions' job names and commands: `~/tmp/aira-350/`
  (the 13:55 `confine --list --json` snapshot, the journal extract, a copy of the store taken at
  14:44 on 2026-11-18).

## Round 4: no lane meets "the head cannot starve", so measure first

### The plain statement

The owner asked for two things at once:
- small jobs pass a head whose AIRA-307 budget is spent;
- a proof that the head cannot starve.

No rule gives both without runtime estimates or preemption, and AIRA has neither. The witness is
from the round-3 review (finding 2), re-derived here against `admitSliceHeadroom`
(`internal/daemon/admit.go:1041-1051`: 2048 MiB, plus 64 MiB for each granted job and one for the
candidate):
- **Setup.** Effective ceiling 49152 MiB. One permanent lease of 42880 MiB. One transient lease of
  1024 MiB that ends at 10 s. A 4096 MiB head whose budget is spent. One eligible 1024 MiB small
  job that never ends (a server). Physical use is below the bookings, so the ledger term decides
  (`checkedAvailable`, `admit.go:2906-2925`), and CPU is free.
- **Before the release**, room is `49152 - (2048 + 3 x 64) - 42880 - 1024 = 3008` MiB. The small job
  fits; the head does not.
- **Lane off.** The spent budget withholds the small job (`admit.go:2694-2696`). At 10 s the
  transient lease ends, room is `49152 - (2048 + 2 x 64) - 42880 = 4096` MiB, and the head is granted.
- **Any lane** (the approved concurrent cap, this spec's per-head allowance, or Alternative B's time
  box): the small job is granted at once. After the release, room is
  `49152 - (2048 + 3 x 64) - 42880 - 1024 = 3008` MiB for as long as the server lives. The head is
  never granted. Backstop passes do not help: the head is evaluated first, and it does not fit.

Today's rule has the same hole, but only through an unspent budget: with `bypassed = 0` the
1024 MiB server passes the 4096 MiB head inside its 2048 MiB budget. AIRA-307 accepts that hole
(queue-order spec §3.5; premise 4 in §3.3 here). **Every lane extends it to every head whose
budget is spent.** Long-lived small jobs are real: the 13:55 snapshot held 8 job-level small scopes
older than 13 hours (§7.1).

So either the requirement changes, or no lane is built. The achievable guarantee is AIRA-307's
kind: a bound on the bytes that pass each head, with the residual cases named. T22 pins the witness
on the evaluator seam, so a build cannot hide it.

### The review loop has not converged

- **Three consecutive rounds with a blocker.** Round 2 had 14 findings, 4 of them induced by the
  round-1 fix. Round 3 re-based, and its review has 16 findings, 5 of them P1.
- **A "closed" claim followed by a blocker of the same class.** Round 3 said the collapse restored
  AIRA-307's bound. The round-3 review then found a starvation witness (finding 2) and a weakened
  strict phase (finding 9).
- **Round 3's findings by root cause** (§14.0):
  - A. Contract changes: 1, 8.
  - B. The head's wait: 2, 9, 10, 11.
  - C. Evidence and cost: 12, 14, 15.
  - D. Tests that cannot fail, or that pin incidental values: 4, 5, 13, 16.
  - E. Spec errors: 3, 6, 7.
- **Fix-induced: 6 of 16.** Findings 1, 8, 9, 11 and 12 come from round 3's re-base. Finding 16
  comes from round 3's answer to round-2 finding 10.
- **The base design that makes family B impossible** admits nothing past a head that today's rule
  holds. That is no lane. Every lane design in rounds 1-3 produced a new witness of family B,
  because each is a new way past the head.

### Is this the XY problem?

The owner's problem is "a long queue of small reservation requests while at 37 GB RAM usage". The
evidence says the head rule explains a small part of it:
- **Over-booking.** The ledger books 2-5x what jobs use. At 13:55, 41.4 GiB was booked against about
  8.7 GiB in use (§8).
- **CPU.** Both live states had 28-32 of 32 CPU slots taken (§7.2), and no lane passes the CPU fit.
- **The freeze's share.** Small waits that end at a backstop pass, the part a lane addresses, are
  about 5 of 115.6 wait-hours in 3.5 days (§7.1). That is an association, not a measured saving.
- **The AIRA-324 trial** (reported in `.aira/tickets/AIRA-324.md:8`, not re-measured here). While
  discounted jobs were booked below their pin, the queue is reported to have fallen from 13-23
  waiting to 0-11.

The lane treats a minor cause of the symptom. Over-booking (AIRA-347, AIRA-318) is the major one.
The store cannot separate the two (§7.2), so the minor cause's size is unmeasured.

### An independent check

Gemini (`gemini-pro-latest`, an independent model lineage, advisory) was given:
- the restated problem;
- the witness;
- the evidence above;
- three options: the allowance, the time box, or measure first.

Its verdict: "build diagnostics only; preventing head starvation is mathematically impossible if
jobs can bypass without preemption or runtime limits". It named over-booking as the larger lever.
It also called strict FIFO the only structurally safe base. That part is not adopted: strict FIFO
would reverse AIRA-307, whose budget and backstop pass are owner decisions. The Fable plan gate is
the independent judge of this collapse.

### Recommendation (§13 Q1)

1. **Now: measure (§M).** No admission change.
   - For each waiter, the evaluator adds up the time it fitted RAM, CPU and VRAM but was held by the
     head rule, and logs that once when the waiter is granted.
   - A week of it measures the population any lane could address, which the store cannot do today.
   - The fields are diagnosis only, guarded like `lastGrantable`.
2. **Then decide on the figures.**
   - Either build the per-head allowance (§3) as a declared trial with a rollback trigger (§7.4), as
     AIRA-324 was run;
   - or drop the lane, and spend the effort on over-booking.

   The allowance needs the owner's yes on three changes in any case (§13 Q2-Q4).
3. **Not recommended: building the allowance now.** Its benefit is not established (§7.3), and its
   cost includes the starvation class above.

**This is a partial fix for the incident in any case.** At 13:21:07 the backstop pass was lost to a
pushed head. No option here changes that (§8). *Correction, 2026-11-18: restoring that pass would
have changed no grant in the incident (`2026-11-18-admission-diagnostics-and-backstop-design.md`
§2).*

**Layout.** §M specifies the measurement, which can be built on the owner's yes. §0-§12 specify the
allowance with round 3's findings fixed, so a trial can be built without another plan round if the
owner chooses it.

## Round 3, for the record: the concurrent lane was withdrawn

- **The round-2 concurrent lane was unsound in production.** In the model, heads that today's rules
  grant in 30-350 s were never granted (W2, W3, W12 and W8c; §3.3.1).
  - The round-3 reviewer reran a plain concurrent design with no guard (finding 16), and reports the
    same: W12 never, W2 3016 s, and W3 never at offsets 0 and 280.
- **The re-base.** Round 3 replaced it with a per-head allowance: at most `x` bytes in total of jobs
  of at most `y` pass each head. That deleted:
  - the lane-member flag and the derived lane sum;
  - guard G;
  - the GPU exclusion (C3);
  - child counting (C7).
- **The wrong claim.** Round 3 said AIRA-307's bound "applies" to the allowance. AIRA-307's bound has
  two parts: a byte budget, then strict protection. The allowance weakens the second part
  (finding 9; §3.3).

## M. The recommended first step: measure what a lane could address

### M.1 The question

Of the time small jobs wait, how much do they spend **fitting RAM, CPU and VRAM but held by the head
rule**? That is an upper bound on what any lane could save them directly. Today neither the store
nor the journal can tell it apart from a wait for room (§7.2).

### M.2 The change (diagnosis only, about 30 lines)

**New fields on `admitWaiter`.** They follow the discipline of `lastGrantable` (`admit.go:261-280`):
written only by the evaluator under `queue.mu`, and read by no admission decision.
- **`heldFitBudgetMS`, `heldFitPushMS int64`.** The time the waiter fitted and was held:
  - by the budget (`budgetWithheld`, `admit.go:2694-2696`), or
  - by a pushed head (`case head.pushSeq > 0`, `admit.go:2691-2692`). That case needs one local
    flag, `pushHeld`.

  "Fitted" is the `fits` of `admit.go:2674`.
- **`heldFitSince time.Time` and `heldFitCause`.** The instant and cause of the last pass that found
  the waiter held while it fitted. Zero otherwise.

**Accounting.** Time is counted interval by interval, and each interval goes to the verdict of the
pass that opened it:
1. **At the top of the per-waiter body**, before the restart-freeze check (`admit.go:2592`): if
   `heldFitSince` is set, add `now - heldFitSince` to the counter of `heldFitCause`, then clear it.
2. **In the refusal branch** (`admit.go:2726-2769`): if the waiter fitted and was held, set
   `heldFitSince = now` and the cause.
3. **In the grant branch** (`admit.go:2699-2725`): step 1 has already closed the last interval. If
   either counter is non-zero, log one journal line:
   `aira daemon: held-while-fitting seq=<n> booking=<bytes> waited=<ms> by-budget=<ms> by-push=<ms>`.

**What is not logged.** A waiter that leaves without a grant (a timeout, a closed client, a kill)
logs nothing, and its held time is lost. That is a stated undercount (M.5).

**Volume.** There is at most one line per granted waiter that was held while it fitted. The line
count is bounded by job arrivals, never by the pass rate, which reaches 4 passes a second. That is
the same order as the pin-charge observe line (`logPinChargeGrantLocked`, `admit.go:2717`).

**Modes.** The line is written in ci-shim mode too. Skipping it there would cost a conjunct and a
test, and nobody reads CI daemon journals.

### M.3 Invariants

1. No admission decision reads the new fields. This is checked by a guard test, as in
   `internal/daemon/admit_pin_charge_guard_test.go`.
2. Grants, the ledger, the head, `bypassed` and the anchor are unchanged. Every existing test passes
   unchanged.
3. Each interval between two passes is counted once, to the verdict of the pass that opened it.

### M.4 Tests (on the AIRA-228 seam, §6 common settings)

| Test | Setup | Assertion | Mutation it kills |
|---|---|---|---|
| **D1 attribution** | One small waiter behind a head whose budget is spent. At t=0 it fits and is withheld; at t=10 it is refused on capacity; at t=20 it fits and is withheld; at t=30 it is granted. | `by-budget=20000`, `by-push=0`, and one line. | Attribute each interval to the pass that closes it: 10000. Count from the first held pass: 30000. |
| **D2 pushed head** | A waiter that fits behind a pushed head at t=0 and t=10. The pushed head is granted at t=20, and so is the waiter. | `by-push=20000`, `by-budget=0`. | One counter for both causes. |
| **D3 no line** | An immediate grant, and a waiter refused only on capacity and then granted. | No line. | A line on every grant. |
| **D4 gate and freeze passes close the interval** | Withheld while fitting at t=0, gate-blocked by an exclusive drain at t=10, granted at t=20. The same with the restart freeze at t=10. | `by-budget=10000`. | Close the open interval in the refusal and grant branches instead of at the top of the body: 20000. |
| **D5 guard** | — | No decision reads the fields. | A decision that reads them. |

### M.5 Analysis, and the decision rule fixed before the data

**The script.** `docs/dev/aira-350-held-fit.py` reads `journalctl --user -u aira-daemon.service`.
`--since` and `--until` are both required, so a rerun cannot inherit an old default (round-3
finding 6). It reports:
- for small jobs (booking `<= 1 GiB`) and for larger jobs: the count, the hours held by the budget,
  and the hours held by a push;
- the denominator, small jobs' wait-hours over the same window, from
  `docs/dev/aira-350-lane-yield-estimate.py --since ... --until ...` (part 1).

It compares totals over the window, with no row join, because a history row carries no scope id
(`internal/store/store.go:878-882`).

**Labels.**
- **By-budget hours are an upper bound** on what a lane saves small jobs directly. Several waiters
  can fit the same room in the same pass, and each lane grant changes the room of every later
  waiter.
- **They are also an undercount** of held time, because waiters that leave ungranted are not
  logged.
- **They say nothing about the cost to heads.** That cost is measured only by a trial (§7.4).

**Proposed decision rule** (the owner sets the numbers). Build the allowance as a trial only if, over
7 days, small jobs' by-budget hours are both:
- at least 10% of small jobs' wait-hours, and
- at least 10 hours.

Otherwise drop the lane and report. Round 3's association figure was about 5 wait-hours in 3.5
days, or about 4% of small wait-hours. That is a hint of where the result may land, not a forecast.

By-push hours size the backstop pass lost to a pushed head (§8), which is a separate ticket.

### M.6 Scope, review and rollout

- **Files.**
  - `internal/daemon/admit.go`: the fields, the three sites and the log line.
  - Tests in `internal/daemon/`.
  - The script.

  No protocol, wire, configuration or face change.
- **Review.** The change sits inside the evaluator but changes no decision. It gets one build review
  (Codex Sol at `high`) and the Fable final review. The guard test is the load-bearing check. It
  does not need the full two-loop.
- **Rollout.** Give peer sessions the shared daemon-restart heads-up first. Then run
  `aira install`, which restarts a present daemon whose binary is stale (`install.go:1291-1314`).
- **Risks.**
  - The evaluator panics, or a field is written in the wrong place. The existing suite covers it.
  - Journal volume, which is bounded by job arrivals.

## 0. The greenfield minimum, and the gap

**The problem in one line.** A queued job that does not fit (the head) holds smaller jobs that do
fit, for minutes, while the slice has room for them.

**The bare minimum for the question the evidence leaves open** is a measurement: how long do small
jobs wait while they fit and the head rule holds them (§M)? It changes no decision. Everything from
here to §12 is the allowance, for the case where the owner chooses to build it.

**The bare minimum build.** Exempt every job-level waiter with a booking `<= y` from the head rule. That is
one more conjunct on the line that already exempts sub-reservations
(`protected := head != nil && !waiter.isSubReservation()`, `internal/daemon/admit.go:2688`).

**Why the minimum is not enough.** Unbounded, it is the starvation AIRA-307 ended: a steady stream
of small jobs keeps the head's room below its size (queue-order spec §1.1: 12 GiB heads waited
23-30 minutes through exactly that refill).

**The smallest bound.** AIRA-307 already bounds what passes a head. It keeps a counter on the head
waiter (`bypassed`, `admit.go:283-300`) and compares it with a byte budget (`reserve/2`,
`admit.go:485-490`). The cheapest bound for the small-job exemption is a second counter of the same
kind, `smallBypassed`, compared with `min(x, head booking)`, and used only by jobs of at most `y`.
That is the whole admission change. Even bounded, it extends today's starvation class to heads
whose budget is spent (Round 4).

| Addition | Why it is needed | Size |
|---|---|---|
| `smallBypassed` on the head waiter, bounded by `min(x, head booking)` | Bounds the bytes small jobs take past each head. It does not bound the head's wait (§3.3). | one field, one `case`, one increment |
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
- A backstop pass can be lost entirely to a pushed head. No option in this spec changes that (§8).
- **The hold that covered 14800's whole spent-budget window was armed under the previous head.**
  The journal's hold line at 13:16:07 names head 14782, and the next freeze line is the yield at
  13:21:07 (`~/tmp/aira-350/freeze-journal.txt`). So 13:17:49-13:21:07 was hold phase while 14800
  was the head. This decides finding 9's option (a) (§3.3).

**Under the allowance** (`x = 2 GiB`, so 14800's allowance is `min(2048, 4096) = 2048` MiB):
- the burst's seven small jobs (722 MiB) would have used the allowance, and only its 1280 MiB job
  the budget;
- that leaves 1326 MiB of allowance and 768 MiB of budget for the ten held jobs (5788 MiB);
- so up to about 2.0 GiB of them could have passed in 13:18:22-13:21:00, if room and CPU allowed,
  which is not established. At `x = 4 GiB` the figure is about 4.1 GiB.

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
  - The 1024, 256 and 512 MiB jobs pass through the allowance (1792 MiB of the head's
    `min(2048, 5120) = 2048`; CPU 31 of 32).
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

Three of these need the owner's explicit yes before any build (§13 Q2-Q4). The round-1 approval
does not cover them.

- **C0a (needs the owner's yes). Cumulative per head, not concurrent.** "Outstanding bookings
  `<= x`" becomes "at most `min(x, head booking)` bytes passed this head" (§13 Q2).
  - **Why not return to the approved concurrent cap** (round-3 finding 1's first option)? It is the
    round-2 design, which is unsound in production (W2, W3, W12 and W8c in §3.3.1). The round-3
    reviewer's own rerun of a plain concurrent cap reports the same (finding 16). It also fails the
    Round-4 witness. Going back to it does not meet the owner's requirement.
  - Everything else in decision 1 stands:
    - the booking units;
    - the fit on RAM, CPU and VRAM;
    - today's behaviour once the allowance is spent;
    - not counting against `head.bypassed`.
- **C0b (needs the owner's yes). An incremented counter, not a derived one.** The brief asked for the
  lane's counter to be derived from the waiter set, "never an incremented counter that can drift".
  `smallBypassed` is incremented on each allowance grant (§3.2). The case for it (§13 Q3):
  - A derived figure needs a ground truth to derive from. "Bytes granted past this head while it was
    head" is a history, not a state of the waiter set, so it has no ground truth.
  - The counter only grows, belongs to one waiter, and dies with it. There is nothing for it to
    drift from.
  - A restart loses it with its waiter, which fails open (a re-enqueued head starts at zero).
  - That is exactly AIRA-307's `bypassed` (`admit.go:283-300`), already accepted.
- **C0c (needs the owner's yes). The guarantee.** The owner asked for a proof that the head cannot
  starve. No lane design can give one (Round 4). What the allowance gives is a byte bound per head,
  with named residual cases (§3.3) and a weaker strict phase (§3.3, finding 9) (§13 Q4).
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
- **C6. The daemon's raw default is off; `aira install` renders the lane on (2G, 1G).** Kept: the
  pin-charge pattern (`pin_charge.go:31-33`).
- **C8 (round 4). A head's allowance is `min(x, head booking)`.** A head is never passed by more
  small-job bytes than its own size (round-3 finding 11).
  - Without this, a 1 GiB head could be passed by `x + 512 MiB` against 512 MiB today: 9x at
    `x = 4 GiB`.
  - 37 of 461 heads were at or below 1 GiB, and 289 (62.7%) at or below 4 GiB
    (`docs/dev/aira-350-round4-queries.out.txt` part 1).
- **C9 (round 4). The default `x` is 2 GiB, not 4 GiB** (round-3 finding 12; §7.3, §13 Q5).
- **C10 (round 4). The allowance stays open while a hold is armed.** This weakens AIRA-307's strict
  phase. Closing the allowance during a hold was checked and rejected: it removes the incident's
  benefit (§3.3).
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
3. **Within the allowance:** `addClamp(head.smallBypassed, w.ledgerCharge()) <=
   min(s.smallLaneCap, head.ledgerCharge())` (C8).

It reads no phase: the allowance is open in idle and hold phases alike (C10, §3.3).

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
  (`admit.go:283-300`). It is incremented, not derived: deviation C0b, an owner question (§13 Q3).
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

**Claim.** Let H be an unpushed head with booking R. While H is the head, the job-level grants made
past it outside backstop passes total at most:
- `R/2` of any size (the AIRA-307 budget), plus
- `min(x, R)` of jobs whose booking is at most `y` (the allowance).

Both counters live on H's waiter and only grow.

**Proof.** Every job-level grant past H passes through the `switch`, and each case is accounted:
- **Pushed head:** none.
- **Backstop pass:** excluded from the claim, as in AIRA-307.
- **Allowance:** the grant is made only if `smallBypassed + charge <= min(x, R)`, and then adds its
  charge.
- **Budget:** unchanged.

Sub-reservations are not job-level, and the exclusivity gate and the restart freeze precede the
`switch` (`admit.go:2592-2621`). ∎ This is AIRA-307 invariant 2 with a second term.

**What it weakens: AIRA-307's strict phase** (round-3 finding 9).
- **AIRA-307's bound has two parts** (queue-order spec §3.5, "Bounds"). A head "is bypassed by at most
  H/2 bytes before strict protection begins. After that, only by one backfilling pass per `maxHold`
  of strict hold."
- **With the allowance, strict protection begins only once both the budget and the allowance are
  spent.** Allowance grants are made inside an armed hold (C10). Round 3's claim that AIRA-307's
  bound applies to the allowance was wrong on this second part.
- **Why it matters.** `maxHold` was sized on strictness. A 12 GiB head starting from a full ledger
  needs about 3.6 minutes of strict hold at the 2026-10-12 rate of 3.6 GB a minute
  (`internal/daemon/paths.go:60-66`). A head that misses its hold meets a backstop pass that refills
  the slice ahead of it: the starvation class that AIRA-307 was built against (queue-order spec
  §1.1).
- **The band of heads affected**, as an order of magnitude (`docs/dev/aira-350-round4-queries.out.txt`
  part 1). At the window's average release rate of 2.71 GB a minute, one 5-minute hold frees about
  12.6 GiB. Heads for which `R + min(x, R)` exceeds that, among the 461 holds:

  | Allowance taken in the hold | Heads that need more than one hold | Smallest such head |
  |---|---|---|
  | none (today) | 8 (1.7%) | 14.0 GiB |
  | `min(2 GiB, R)` (this plan's default) | 55 (11.9%) | 10.8 GiB |
  | `min(4 GiB, R)` | 103 (22.3%) | 9.0 GiB |

  The figure is not a bound either way:
  - the rate is an average, and busy periods release faster;
  - a head rarely starts from a full ledger;
  - the allowance is not always used.
- **Options considered:**
  - **(a) Close the allowance while a hold is armed. Rejected, because it removes the benefit.**
    - The hold is a property of the queue, not of the head. An arm stays in force after its head is
      granted. In the incident, the hold armed at 13:16:07 under head 14782 ran until 13:21:07, so it
      covered all of 14800's spent-budget window, 13:18:22-13:21:00 (§1.2). The round-3 review's
      "still fixes the incident's first window" does not hold.
    - The arm also happens inside a pass: `phase = admitFreezeHold` at `admit.go:2762-2769`. So in
      T1's queue, the 2154 MiB job ahead of the small ones would close the allowance for them even
      from idle, and nothing would be granted.
  - **(b) A smaller `x`.** Adopted in part: `x = 2 GiB` (C9) halves the band against 4 GiB.
  - **(c) Accept, with the band quantified.** Adopted. It is an owner question (§13 Q4).
- **The seam run S5 pins the effect** with figures derived by hand (§6.2).

**What it does not say: H's wait is not bounded.** An earlier draft of round 3 claimed the
allowance delays H by at most the time its `x` bytes take to end or to drain. Its own model output
contradicts that (W3 at offset 0), so the claim is withdrawn. Three effects defeat any such bound:
- **A small job that never ends.** This is the Round-4 witness: one 1024 MiB server passed through
  the allowance keeps a 4096 MiB head out for the server's life, where today the head is granted at
  10 s. It holds for every lane design (T22).
- **The duty cycle moves.** Allowance grants never arm the hold (invariant 3). So with the
  allowance, holds arm later or not at all, and every later backstop pass falls at a different
  instant. A backstop pass grants every fit, and which waiters fit there depends on its instant.
  So the allowance changes which refills land in front of H: in W3 that turns a head granted today
  at 350 s into one that is never granted, and the reverse in 3 other phases.
- **Suite parents.** After the budget is spent, the allowance still lets suite parents pass, and
  their workers pass H for the suite's lifetime (premise 2).

What remains is the byte bound above. Every job-level byte that passes H outside backstop passes is
counted on H, and there are at most `R/2 + min(x, R)` of them.

**What it does not change.** These are the premises AIRA-307 already accepts (queue-order spec
§3.5), now stated as premises rather than excluded:
1. **Backstop passes grant every fit once per hold, ahead of the head.** In the model (W3, 30
   phases):
   - today's rules starve the reviewer's backstop-refill head in 29 of 30 phases, granting it only
     at offset 0;
   - the allowance starves it in 27 of 30, granting it only at offsets 270-290;
   - the round-2 concurrent lane starves it in all 30.
2. **Sub-reservations are exempt and uncounted.** A suite parent that passes H, through the budget
   today or through the allowance, brings its workers past H for the suite's lifetime.
   - In the model, W8c (allowance) and W8d (today, fresh budget) both give 900 s. With the budget
     spent, today gives 20 s, because no suite parent can pass.
   - So the allowance re-opens this exposure for each head whose budget is spent, and raises how
     many suite parents can pass a head (§9 R1).
   - **Measured:** 753 of 8349 small confine rows in the window (9.0%) are `aitest-parent` rows
     (`aira-350-round3-queries.out.txt` part 2). That is a lower bound on suite parents, because a
     `make test-engine` parent does not carry that signature.
   - **Their bookings** (`aira-350-round4-queries.out.txt` part 2): 634 of the 753 book exactly
     1 GiB (the cold default), 110 book 512 MiB or less, and the median is 1024 MiB. So parent
     counts in 1 GiB units are close; for the eighth that book 512 MiB or less they are higher.
   - **How many parents can pass a head**, in 1 GiB parents:

     | Head | Budget only (today) | With the allowance at `x = 2 GiB` | At `x = 4 GiB` |
     |---|---|---|---|
     | 4 GiB (the median) | 2 | 4 | 6 |
     | 1 GiB | 0 | 1 | 1 |

   - **How often.** If k small jobs pass a head, at least one is a suite parent with probability at
     least `1 - 0.91^k`: 17% for k = 2, 31% for k = 4 (`aira-350-round4-queries.out.txt` part 2).
   - The daemon cannot tell a suite parent at admission, because `delegate_ram` left the admit
     allowlist (`internal/daemon/protocol.go:129-132`). So no cheap eligibility rule excludes them.
3. **High-priority and pushed arrivals are evaluated ahead of H** (`admitOrderLocked`). Each has its
   own counters, so a stream of them can pass H without limit. This is pre-existing.
4. **Leases that never end, and the physical floor** in `checkedAvailable` (`admit.go:2906-2925`).
   Both are pre-existing. The allowance extends the never-ending case from heads with an unspent
   budget to every head (the Round-4 witness, T22).

**What the allowance costs a head (estimated, not a bound).**
- **The drain, as an order of magnitude.** When H would fit once the other bookings fell by the
  allowance, those bytes ahead of it cost the time to release them.
  - The window's average release rate is 2.71 GB a minute: 12711 GiB of bookings started in 84 h
    (`aira-350-round3-queries.out.txt` part 4). Rows are lost, so the true rate is higher.
  - At that rate `x = 2 GiB` is about 0.8 minutes, and 4 GiB about 1.6 minutes.
  - **This uses the slice-wide release rate, not the lifetimes of what the allowance admits.** Small
    jobs' median run time is 4 s, but 43.9 of their 110.7 GiB-hours come from jobs that ran over
    10 minutes (`aira-350-round3-queries.out.txt` part 2). For a given head the drain can be far
    longer.
  - The 3.6 GB a minute in the queue-order spec is from 2026-10-12 and is not used here.
- **A different duty cycle.** Allowance grants do not arm, so holds arm at other instants. A head
  that today's duty cycle grants can starve under the allowance's, and the reverse (W3: 1 phase of
  30 one way, 3 the other). Separately, the allowance taken inside a hold costs large heads their
  hold (the band above). That effect only ever removes room from the head.
- **A suite lifetime**, when a suite parent passes through the allowance (premise 2).
- **For ever**, when a small job that never ends passes (the Round-4 witness).
- **The model only (W0, at `x = 4 GiB`, not evidence):**
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

`spec-fresh`, `tb-full` and `cum-g` are in the output. The model runs use `x = 4 GiB` and no
`min(x, R)`. They were not rerun for round 4, because round 4 recommends measuring instead (§M).

| # | Witness (round-2 finding) | `off` | `spec` | `cum-min` | `tb-min` |
|---|---|---|---|---|---|
| W1 | Stale lane sum: a member's child grants inside the pass (1) | — | candidate granted, lane tree 5120 MiB over `x`, head plus tree 13312 MiB over a 12288 MiB ceiling | no lane sum exists | sum recomputed per decision |
| W2 | Zero-VRAM lane parents with GPU children, VRAM-short head (2) | 30 s | 3016 s (arrivals stop at 3000 s) | 76 s | 616 s |
| W3 | Backstop refill of rotating 5 GiB leases (3), at the reviewer's phase (offset 0) | 350 s | never | **never** | 605 s |
| W3 sweep | The same over 30 phases (offsets 0-290 s) | starved in 29; granted only at 0 | starved in 30 | starved in 27; granted only at 270-290 | 605 s in all 30 |
| W8 | Lane-admitted suites with 4 short-lived workers (8) | 20 s (fresh budget: 140 s) | 70 s | 150 s | 150 s |
| W8c | Suites with a long-lived 32-worker pool (8) | 20 s (fresh budget: 900 s) | 900 s | 900 s | 900 s |
| W12 | 1.5 GiB of never-ending leases, 39 GiB head (12) | 210 s | never | 210 s | 610 s |

Other cases:

| Case | Under this plan |
|---|---|
| Repeated refill by small jobs | At most `min(x, R)` per head, then today's rule (W2, W12). |
| A small job that never ends | The head can starve for the job's life; today only while its budget is unspent (Round 4; T22). |
| Large head inside an armed hold | Strict protection starts only after the allowance is spent too; 55 of 461 holds are in the band at `x = 2 GiB` (§3.3; S5). |
| High-priority head | An ordinary unpushed head with its own allowance (AIRA-340; T18). A stream of high heads each brings a fresh allowance (R7). |
| Pushed head | Closed (C5; T7). |
| Head larger than `ceiling - x` | No longer special: at most `min(x, R)` of small jobs pass it. It starves only if one of them never ends, as above. |
| CPU- or VRAM-short head | At most `min(x, R)` bytes of small jobs pass it. They take slots or VRAM until they end, which is the same RAM-bytes proxy AIRA-307's budget already uses for such heads (`admit.go:478-481`). |
| A head no larger than `y` (or `x`) | Passed by at most its own size of small jobs, plus its budget: `1.5 R` in all (C8; T20). Each head in a chain of small heads has its own allowance, so a large waiter behind the chain pays each in turn. |
| Ceiling shrinks | As today: H stops being head if it can no longer fit an empty slice (`admit.go:2770`). |
| Daemon restart | Counters are dropped with their waiters, which fails open (§3.2). No lease carries lane state. |
| Many tiny jobs | The allowance's worth of tiny jobs also raises the headroom by 64 MiB each (`admit.go:1041-1051`). Their number is bounded by the CPU ceiling, one slot each: 32 slots is at most 2 GiB. |
| Ceiling falls below the bookings, or an ARDR establish past it | `available` is negative, so nothing is granted, allowance included (T23). |
| Sub-reservations | Unchanged: exempt, uncounted, never use the allowance (T11). |

### 3.4 Interactions

| Interaction | Decision | Reason / code |
|---|---|---|
| Pushed head (`--front`) | Closed. | C5; the pushed case precedes the allowance case. |
| `--exclusive` drain or hold | Unchanged: the allowance never applies to a gate-blocked waiter. Tested (T17). | The gate `continue`s before the head logic (`admit.go:2604-2621`). |
| `--priority high` | A high head has its own allowance. Small waiters behind it use that one. Tested (T18). | A high head is an unpushed head (AIRA-340, `admit.go:302-311`). |
| Fairness-freeze duty cycle | Allowance grants never arm. The allowance is open during a hold (C10, §3.3). The backstop pass is unchanged because the allowance case follows it. A small job the allowance refuses and the budget withholds arms as today. Tested (T12, T19, S5). | The arm stays guarded by idle (`admit.go:2762`); no new anchor write. |
| AIRA-324 discount | Eligibility and the counter are in `ledgerCharge()`. The physical-fit rule is unchanged, because it is part of `fits`, computed before the head rule (`admit.go:2663-2666`). Tested in both directions (T10, T10b). | C1 reverted. |
| Sub-reservations, worker-admit | Exempt from the head rule, never use the allowance, never counted. | `worker_admit.go:162`, `worker_admit.go:475`. |
| Re-declare onto a live lease | Nothing to carry: no lease holds lane state. | — |
| Daemon restart, ARDR establish | Counters are lost with their waiters (fail-open; T21). No frame change. An establish past the ceiling leaves `available` negative, and the allowance grants nothing (T23). | §3.2; `admit.go:2343-2349` |
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
    `E_CONFIG_INVALID: AIRA_DAEMON_SMALL_LANE must be off or a byte size such as 2G`.
- **`AIRA_DAEMON_SMALL_LANE_JOB_MAX`** is `y`:
  - it is read only when the lane is on, so a typo cannot stop a daemon that did not ask for the
    lane (the `pin_charge.go:69-73` ordering);
  - unset means 1 GiB;
  - anything below 1 MiB (`runner.MinPinnedScopeCap`) or above the lane size is refused:
    `E_CONFIG_INVALID: AIRA_DAEMON_SMALL_LANE_JOB_MAX must be a byte size from 1M up to the lane size,
    such as 1G`.
- **Serve** reads both next to the pin-charge read (`internal/daemon/server.go:416-429`):
  - in shim mode it forces the lane off;
  - it logs `aira daemon: small-job lane 2G per held head (or the head's booking if smaller) for
    jobs up to 1G` when the lane is on.

**Install.** Everything below copies the pin-charge pattern:

| Step | Pin-charge site copied | What the lane adds |
|---|---|---|
| Flags | `install.go:154-158` | `--small-lane SIZE\|off` and `--small-lane-job-max SIZE`, parsed with the pin-charge flags (`install.go:471-492`) and refused with `E_INSTALL_ARGUMENT_INVALID` (`install.go:32`, `argumentInvalid` at `install.go:2568`). |
| Option not given | `install.go:640-650` | The installed unit's value (`installedEnvironmentRE`) is kept. |
| Ship defaults | `install.go:695-706` | `2G` and `1G`, validated as the daemon does. |
| Re-exec forwarding | `install.go:890-895` | Given flags only. |
| Unit render | `renderDaemonUnit`, `install.go:1632-1675` | Re-validates. Two placeholders and two lines in `internal/install/assets/aira-daemon.service.in` (beside lines 12-13). |
| `--status` | `install.go:2505-2508` | Two `reportDaemonMode` lines. |
| Shim environment | `internal/install/mode.go:321` | `AIRA_DAEMON_SMALL_LANE=off` is added. |
| Usage string | `internal/core/core.go:2241` | The two options. |

No upper bound is placed on `x`. A larger `x` is a direct trade against heads: each head can be
passed by that many more bytes of small jobs, and the band of large heads that miss their hold
widens (§3.3). The help text says so.

### 3.6 Observability (minimal)

Round 4 cut the `aira top` footer and the `--list` line form for a queue with no head (round-3
finding 15).
- **Reply fields.** `runner.ConfineSliceReserve` (`internal/runner/confine_manage.go:586`) gains
  three optional fields, filled in `admitSliceSnapshotFor` (`admit.go:1302`) in the same locked
  walk. All three are present only while the lane is on and a head exists, read from the waiter
  `queue.freezeHolderSeq` names (`admit.go:828`, as `admitReportedPhaseLocked` does at
  `admit.go:2809-2821`):
  - `small_lane_used_bytes`: the head's `smallBypassed`;
  - `small_lane_allowance_bytes`: the head's `min(x, R)`;
  - `small_lane_job_max_bytes`: `y`.

  When they are absent (no head, lane off, ci-shim, or an older daemon), nothing is rendered.
- **One per-waiter field.** `held_by` (`head` or `small-lane-full`) is taken at the same
  queued-scope match as `queue_position` (`admit.go:1352-1364`), and only for a caller that named
  its scope id.
- **`aira confine --list` text.** One line after `slice queue:` (`cmd/aira/main.go:3765-3779`), when
  the fields are present: `small-job lane: 1792M of 2048M used past the current head (jobs of at
  most 1024M pass a held head until it is used)`.
- **Wait line** (`confineQueueNote`, `internal/runner/confine_queue_position_linux.go:151-221`):
  - `held_by=head` adds ", fit at the last check but held behind the head of the queue";
  - `held_by=small-lane-full` adds ", fit at the last check but the small-job lane past this head is
    used up";
  - absent adds nothing.
- **Daemon journal**, the trial's measurement hooks (§7.4). Both are bounded by job arrivals, not by
  the pass rate:
  - one line per allowance grant:
    `aira daemon: small-lane grant seq=<n> booking=<n> waited=<ms> head-seq=<n> used=<bytes>/<allowance>`;
  - one line when a waiter is granted with `smallBypassed > 0`, which gives the cost to heads:
    `aira daemon: small-lane head granted seq=<n> booking=<n> waited=<ms> small-passed=<bytes> budget-passed=<bytes>`.

  §M's `held-while-fitting` line stays in, so the trial and the baseline are measured the same way.

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
  this way add up to the lane size for that head (2G by default, or the head's own booking if that
  is smaller). It is still admitted only if it
  fits the slice's memory, CPU and VRAM ceilings. It never passes a job pushed with `--front` or an
  exclusive drain. Default mode only: off under `--ci=shim`. Set at install with
  `--small-lane SIZE|off` and `--small-lane-job-max SIZE`."
- **Short:** "Small jobs (default at most 1G) can pass a waiting head through the small-job lane
  (default 2G per head), still only when they fit."

### 3.8 Alternative B, if the owner keeps a concurrent lane

**The rule.**
- A concurrent cap over lane members: a `laneGranted` flag, and a lane sum derived from the waiters
  and recomputed before every lane decision (finding 1).
- The lane closes for a head once `now - head.enqueued >= 2 x maxHold`, which is 10 minutes by
  default. `enqueued` is an existing field.
- No G, C2, C3 or C7. In the model, adding all four (`tb-full`) changes one grant time (W8: 70 s
  against 150 s) and none of the bounds.

**The bound.** Lane grants past H stop at `H.enqueued + T`, and the members granted before then
end within their lifetimes, a suite lifetime included. From then on only today's rules act, but from
a different state (the duty cycle has moved, as in §3.3), so this is not a bound on H's wait either.

**Measured** (`docs/dev/aira-350-hold-age.out.txt`, 461 holds):
- the head's age when its hold arms has a median of 154 s and a 90th percentile of 612 s;
- with `T = 600 s` the lane is open for 81% of hold time;
- with `T = 300 s`, for 44%.

The 13:16:07 head was already 6m10s old when its hold armed.

**In the model:** W2 616 s, W3 605 s in all 30 phases, W12 610 s, against 30 s, 29 of 30 starved,
and 210 s today. In W3 the time-box does better than today. The likely reason is that small jobs no longer arm
holds, so fewer backstop passes refill the slice. That is a property of the model's witness, not
established for production.

**Against the recommendation.**
- More state: a flag on every member, a derived sum, and round 2's restart and re-declare cases.
- A head pays up to `T` even where the allowance would have stopped sooner (W2, W12).
- It fails the Round-4 witness like every lane: a server admitted before `T` keeps the head out
  after `T`.

## 4. Invariants

1. **The ledger is untouched, and a new grant is carved out of the ceiling.** `outstanding`,
   `cpuOutstanding`, `vramOutstanding` and `outstandingJobs` are still written only by
   `rederiveLedgerLocked`. Every allowance grant passes today's `fits`, so at the instant it is made
   its booking is at most the signed `available` of that pass (`admit.go:2634-2666`).
   - **This is a postcondition of new grants, not a state invariant.** The ledger can already stand
     above `ceiling - headroom` in two ways:
     - the pressure ceiling can fall below the existing bookings (`sliceCeilingEffectiveMaximum`,
       `internal/daemon/sliceceiling.go:752-760`);
     - an ARDR re-declare establishes a lease past the ceiling (`admit.go:2343-2349`).
   - In both cases `available` stays negative (signed, `checkedAvailable`, `admit.go:2893-2925`),
     and nothing is granted, allowance included, until releases recover it (T23).
2. **The per-head bound.** For every unpushed head H with booking R, outside backstop passes:
   - allowance grants past H total at most `min(x, R)`;
   - other job-level grants past H total at most `R/2`.

   Both counters only grow.
3. **Allowance grants never change `head.bypassed`, never set `budgetWithheld`, and never write the
   anchor.** AIRA-307 invariant 2 holds verbatim for non-allowance grants. AIRA-307's strict phase
   does not: it begins only once the allowance is spent too (§3.3).
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
- **Lane:** on, `x = 2048 MiB` (the default), `y = 1024 MiB`, set on the Server fields directly.
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
| **T1 13:55 replay** (regression pin) | `effectiveMaximum = 48640 MiB`. 28 held leases sum to 42443 MiB, one slot each, so room is 2293 MiB (`48640 - (2048 + 29 x 64) - 42443`). A 5120 MiB head (allowance `min(2048, 5120) = 2048`). Waiters in order, in MiB: 2154, 2700, 1024, 256, 512, 512, 512, 1024. | Granted exactly the 1024, the 256 and the first 512 (allowance), so `smallBypassed == 1792` and `bypassed` is unchanged. The 2154 is withheld with `heldBy == "head"`. The 2700 and the last three are refused on capacity (room 309 MiB after the grants), with `heldBy == ""`. CPU is at 31 of 32. | Today's code grants **none**. So does the budget case placed first (a reordered `switch`). |
| **T1b CPU full** | As T1, but 32 held leases summing 42187 MiB (room still 2293 MiB), so CPU is at 32 of 32. | Nothing granted, and every `heldBy == ""`. | Allowance grants that skip `cpuFits`: three granted. |
| **T2 the brief's incident replay; the allowance binds** | `effectiveMaximum = 49152 MiB`. 20 held leases sum to 42278 MiB, one slot each, so room is 3482 MiB, about 3.4 GiB (`49152 - (2048 + 21 x 64) - 42278`). A 4096 MiB head with its budget spent (allowance `min(2048, 4096) = 2048`). The 14 small waiters, in MiB: 72, 256, 512, 1024, 512, 256, 1024, 72, 512, 1024, 256, 512, 72, 1024. | Granted exactly waiters 1, 2, 3, 4, 8 and 13, so `smallBypassed == 2008`. Room is 1090 MiB after them, so the other 8 fit and are withheld with `heldBy == "small-lane-full"`. CPU is at 26 of 32. | Today's code: none granted, all 14 with `heldBy == "head"`. Remove the allowance check: waiters 1-6, 8 and 11 granted (room falls to 10 MiB). The budget case first: none. |
| **T3 boundaries** | — | `smallBypassed + charge == min(x, R)` is granted and one byte more is `small-lane-full`, for a head larger than `x` and for one smaller than `x`. `charge == y` is eligible; `y + 1` is not (`heldBy == "head"`). | `<` / `<=` swaps; `x` in place of `min(x, R)`. |
| **T4 small jobs keep the budget** | Zero headroom. An 8 GiB head with `bypassed = 0` (budget 4 GiB, allowance 2 GiB). Room 7 GiB. Two 1 GiB small waiters, then a 4 GiB waiter. | All three granted. `smallBypassed == 2 GiB`, `bypassed == 4 GiB`. | Count allowance grants in `bypassed`: the 4 GiB waiter is withheld. (The budget case first is killed by T1 and T2 instead: with a budget left, a reordered `switch` still sends these small jobs to the allowance.) |
| **T5 per head** | (a) Head A with its allowance and budget spent, and a held small waiter. A is granted in pass 1. In pass 2 a new non-fitting head B (budget spent) is ahead of the small waiter. (b) Head A (8 GiB) with `smallBypassed = 1 GiB` is displaced by a pushed waiter, which is then granted, so A is head again. | (a) In pass 2 the small waiter passes B, and `B.smallBypassed` equals its booking. (b) One 1 GiB small job passes A, and a second is `small-lane-full`. | (a) A counter on the queue: held. (b) Reset the counter on a change of head: two pass. |
| **T6 cumulative, not concurrent** (encodes C0a) | An 8 GiB head whose allowance was used up by two 1 GiB jobs. Both are released through `releaseAdmitWaiterLocked`. A 1 GiB small waiter. | Next pass: still `small-lane-full`. | Give the allowance back on release (the round-2 concurrent lane): granted. If the owner keeps the concurrent semantics (§13 Q2), this design is not built. |
| **T7 pushed head** | A small waiter behind a pushed head. | It waits in hold, idle and backstop passes, with `heldBy == "head"`. | Evaluate the allowance case before the pushed case: granted. |
| **T8 ci-shim** | A shim server with the lane fields set directly. A small waiter behind a head whose budget is spent. | Withheld. | Drop the `!shimMode()` conjunct: granted. |
| **T9 GPU small jobs** | Two small waiters with `vram > 0`: one fits the VRAM snapshot, one does not. | The first is granted through the allowance. The second is refused on capacity (`heldBy == ""`). | Reinstate C3: the first is withheld. Skip the VRAM fit on allowance grants: the second is granted. |
| **T10 eligibility on the booking (AIRA-324)** | Pin-charge enforce. A waiter with a 1228 MiB pin and a 983 MiB booking that passes the physical-fit rule. | Granted through the allowance, and `smallBypassed` increases by 983 MiB. | Judge eligibility on the reserve: withheld. |
| **T10b the full pin must still fit (AIRA-324)** | Pin-charge enforce, lane on, head budget spent. A waiter with a 1228 MiB pin and a 983 MiB booking. Ledger room 1500 MiB, so the booking fits. Physical room 1100 MiB, so the full pin does not (`admit.go:2664-2666`). | Refused on capacity, `heldBy == ""`, `smallBypassed` unchanged. | An allowance path that skips the physical-fit rule: granted. |
| **T11 sub-reservations** | A 2 GiB sub-reservation of a granted parent, queued behind a head; it fits. | Granted on fit. `smallBypassed` and `bypassed` are unchanged. | Route sub-reservations through the allowance, or count them: a counter changes. |
| **T12 no anchor write from allowance grants** | Idle phase (`freezeArmedAt` zero). A head with its budget spent and an unused 2 GiB allowance. Pass 1: two 1 GiB small waiters that fit. Pass 2: a third 1 GiB small waiter that fits. | Pass 1: both granted through the allowance; `freezeArmedAt` stays zero and the phase stays idle. Pass 2: the third is withheld (`small-lane-full`) and arms the hold, as today. | Write the anchor in the allowance case (arm on an allowance grant when idle): armed in pass 1. (Setting `budgetWithheld` on a grant is not a usable mutation: a grant `continue`s at `admit.go:2725`, before the arm at `admit.go:2762`.) |
| **T13 resize** | An allowance-admitted lease's `charge` is lowered to 256 MiB. | `smallBypassed` is unchanged and no extra grant is made. | Recompute the allowance from current charges: one extra grant. |
| **T14 the counter is the head's** | Three 1 GiB small waiters behind an 8 GiB head with an unused allowance. Room 8 GiB. | The first two are granted. The head's `smallBypassed == 2 GiB`, each granted waiter's own counter is 0, and the third is `small-lane-full`. | Add the booking to the granted waiter's counter instead of the head's: all three granted. |
| **T15 diagnosis** | — | `heldBy` is set on refusal and cleared on a capacity refusal, and no decision reads it (the guard-test pattern of `admit_pin_charge_guard_test.go`). | A decision that reads `heldBy`. |
| **T16 fall-through to the budget** | The allowance spent, the budget not. A 1 GiB small waiter. | Granted through the budget, and `bypassed` increases. | End the `switch` on an allowance refusal: withheld. |
| **T17 exclusivity** | (a) An exclusive holder granted (hold). (b) An exclusive drain queued (drain). A small eligible waiter behind each. | The waiter is gate-blocked and not granted. No counter changes. | Evaluate the allowance before the gate: granted. |
| **T18 high-priority head** | A `--priority high` waiter enqueued after three small normal waiters. It does not fit, so it is the head. | The small waiters pass it through its allowance, and its `smallBypassed` holds their total. | Exclude a high head from the allowance: withheld. |
| **T19 backstop pass** | `freezeArmedAt = now - maxHold`, so this is the backstop pass. Small eligible waiters fit. | Granted, counted in `bypassed`. `smallBypassed` is unchanged. | Put the allowance case before the backstop case: `smallBypassed` changes. |
| **T20 a small head** (C8) | A 1024 MiB head with a fresh budget (512 MiB; allowance `min(2048, 1024) = 1024`). Idle at the start. Ten passes, one second apart. Before each pass, one 256 MiB small waiter is enqueued and the previous pass's grant, if any, is released, so room is 600 MiB at every pass. | Passes 1-4 grant through the allowance (`smallBypassed == 1024`). Passes 5-6 grant through the budget (`bypassed == 512`). Pass 7 withholds and arms the hold. Passes 8-10 withhold. 1536 MiB in all passed the head, 1.5 times its booking. | `x` in place of `min(x, R)`: passes 1-8 through the allowance and 9-10 through the budget, with no arm. |
| **T21 restart or re-enqueue** | An 8 GiB head with `smallBypassed = 2 GiB` leaves the queue. It is enqueued again as a new waiter for the same scope, as when a client reconnects after a daemon restart. A 1 GiB small waiter behind it. | The new head's `smallBypassed` starts at 0, and the small waiter passes through the allowance. This pins fail-open (§3.2), as for `bypassed`. | Carry the counter across a re-enqueue by scope id: withheld. |
| **T22 the Round-4 witness** (C0c) | `effectiveMaximum = 49152 MiB`, production headroom. Held: one 42880 MiB lease that never ends, and one 1024 MiB lease released between the pass at t=0 and the pass at t=10. A 4096 MiB head with its budget spent. One 1024 MiB small waiter, never released once granted. Passes every 10 s to t=610. | Lane off: the small waiter is withheld at t=0, and the head is granted at the t=10 pass (room 4096 MiB). Lane on: the small waiter is granted at t=0 through the allowance; the head is granted at no pass up to t=610, and its `lastGrantable` is 3008 MiB from t=10 on. | It pins the accepted residual. A build that claims to remove it must change this test knowingly. Lane accidentally off: the head is granted at t=10. |
| **T23 ledger above the ceiling** (invariant 1) | (a) The effective maximum falls below the existing bookings, through the ceiling seam. (b) An ARDR re-declare establishes a lease past the ceiling (`admit.go:2343-2349`). In each, a 4096 MiB head and an eligible 256 MiB small waiter, with the allowance unused. | Nothing is granted; the counters are unchanged; the small waiter's `heldBy == ""`. | Allowance grants judged on physical room only, without the ledger term: granted. |

### 6.2 Bounded runs in Go (against the real evaluator)

Deterministic multi-pass runs on the same seam, with production `maxHold`, backstop passes and a
one-second clock step. Each also runs with the lane off for comparison.
- **Asserted grant passes only where they follow from arithmetic stated here** (S5).
- **Elsewhere the head's grant pass is printed, not asserted.** §3.3 says the allowance moves the
  duty cycle, so an equal pass in the two runs would be incidental (round-3 finding 16). The model's
  figures (§3.3.1) are printed beside it for the reviewer, and neither side is tuned to the other.

| Run | Shape | Asserted | Printed only |
|---|---|---|---|
| **S1** | W12: 1.5 GiB of never-ending leases, 10 GiB of transient leases, a 39 GiB head, 512 MiB small jobs every 5 s. | The per-pass properties. The concurrent mutation (T6's) is not granted within 6000 passes (the model's nearest design, `spec`, never grants it). | The head's grant pass with the allowance and with the lane off (model: 210 s for both). |
| **S2** | W2: a VRAM-short head, zero-VRAM 1 GiB parents with 1 GiB-VRAM children, staggered. | The per-pass properties. The concurrent mutation is not granted while parents keep arriving (model `spec`: 3016 s, after arrivals stop at 3000 s). | The grant passes (model: 76 s with the allowance, 30 s with the lane off). |
| **S3** | W3: rotating 5 GiB leases refilled by backstop passes, at offsets 0 and 280. | The per-pass properties. | The grant passes with the allowance, with the lane off and for the concurrent mutation. The model gives never, 350 s and never at offset 0, and 350 s, never and never at offset 280. No ordering is asserted, because none holds across phases. |
| **S4** | W8c: suites with a long-lived worker pool. | The per-pass properties. | The allowance run with the budget spent, against the lane-off run with a fresh budget (model: 900 s for both). |
| **S5 the hold band** (round-3 finding 9) | See below. | Lane off: the head is granted at the t=300 pass. Allowance at 2 GiB: the head is granted at the t=420 pass. | — |

**S5, derived by hand.** Production headroom, `effectiveMaximum = 44800 MiB`.
- **Held:**
  - one 16384 MiB lease that never ends;
  - ten 2560 MiB leases, the k-th released just before the pass at t = 60k.

  Room at t=0 is `44800 - (2048 + 12 x 64) - (16384 + 10 x 2560) = 0`. Each release adds 2624 MiB:
  the lease plus one job's headroom.
- **Queue:**
  - a 12288 MiB head, enqueued first, with its budget spent;
  - `freezeArmedAt = t0`, so the pass at t=300 is the backstop pass (`admit.go:840-850`);
  - four 1024 MiB small waiters enqueued at t=30, which never end once granted.
- **Lane off.**
  - The small waiters are withheld by the budget from t=60.
  - At t=300 room is 13120 MiB, and the head, first in order, is granted.
  - Room is then 768 MiB, and the small waiters are refused on capacity.
- **Allowance at 2 GiB.**
  - At t=60 (room 2624 MiB) two small waiters are granted through the allowance, leaving 448 MiB.
  - From t=120 the other two fit and are `small-lane-full`.
  - At t=300 room is 10944 MiB. The head is refused. The backstop pass grants the two held small
    waiters, leaving 8768 MiB.
  - Room reaches 11392 MiB at t=360 and 14016 MiB at t=420, when the head is granted.
- **Allowance at 4 GiB.** All four are granted through the allowance by t=120, and the head is
  again granted at t=420.

This is the effect of finding 9: the allowance taken inside the hold makes the head miss its hold,
and the backstop pass then refills the slice ahead of it.

**Property checks after every pass, in every run:**
- no new grant leaves `outstanding` above `effectiveMaximum - headroom`. These runs hold the ceiling
  fixed; T23 covers a falling ceiling and an ARDR establish;
- `smallBypassed <= min(x, R)` on every waiter;
- for every unpushed head, the non-allowance job-level grants made past it outside backstop passes
  total at most `R/2`.

### 6.3 Configuration and install

- **Environment table:**
  - `AIRA_DAEMON_SMALL_LANE` unset or `off` is off, and `2G` is 2 GiB; `0`, `-1` and `abc` give
    `E_CONFIG_INVALID`;
  - `AIRA_DAEMON_SMALL_LANE_JOB_MAX` is ignored when the lane is off (a typo is not refused). It is
    refused below 1 MiB and above `x`, and defaults to 1 GiB.
- **Serve:** shim mode forces the lane off. Startup log line.
- **Install:**
  - the flags parse, and bad values are refused with `E_INSTALL_ARGUMENT_INVALID`;
  - a value is kept from the installed unit when not given, and the ship defaults `2G`/`1G` are
    rendered;
  - `renderDaemonUnit` re-validates, and given flags are forwarded on re-exec;
  - `--status` prints both lines, and the shim environment carries `AIRA_DAEMON_SMALL_LANE=off`.

### 6.4 Observability and faces

- **Snapshot:** the three `small_lane_*` fields are present only with the lane on and a head, and
  absent otherwise. `held_by` is reported only for the named queued scope.
- **Renderers:** the `--list` line, the wait-line clauses for both `held_by` values and for absence,
  and both journal lines.
- **Faces:** `TestConfineSmallLaneGuidanceIsOnEveryFace` covers the skill, `install --help` and its
  JSON, the `confine` summary, and the MCP instructions.

### 6.5 Mutation list, by direction

- **False pass** (grants what must wait):
  - remove the allowance check (T2);
  - `x` in place of `min(x, R)` (T3, T20);
  - give the allowance back on release, i.e. a concurrent lane (T6, S1, S2);
  - the allowance case before the pushed case (T7);
  - on in shim (T8);
  - skip the CPU or VRAM fit on allowance grants (T1b, T9);
  - skip the physical-fit rule on allowance grants (T10b);
  - judge allowance grants without the ledger term (T23);
  - route sub-reservations through the allowance (T11);
  - the allowance before the exclusivity gate (T17);
  - recompute the allowance from current charges (T13);
  - reset the counter on a change of head (T5b);
  - `<` / `<=` swaps (T3).
- **False fail** (holds what should be granted):
  - delete the allowance case (T1, T2);
  - the budget case first (T1, T2);
  - count allowance grants in `bypassed` (T4);
  - a counter on the queue (T5a);
  - carry the counter across a re-enqueue (T21);
  - judge eligibility on the reserve (T10);
  - reinstate C3 (T9);
  - no fall-through to the budget (T16);
  - exclude a high head (T18);
  - write the anchor on an allowance grant (T12, which over-arms: a false fail for later waiters);
  - a `heldBy` value read by a decision (T15).
- **Moved:** the allowance case before the backstop case (T19).
- **Pinned residuals, not mutations:** T22 (the Round-4 witness) and S5 (the hold band).
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
  it was at most 4 GiB. Just under 90% were at most 2 GiB, the default `x` (C9).
- **Heads:** the median reserve was 4 GiB, the 90th percentile 10.9 GiB and the maximum 25.3 GiB.
  37 heads (8.0%) were at or below 1 GiB, and 289 (62.7%) at or below 4 GiB
  (`aira-350-round4-queries.out.txt` part 1; the round-3 review estimated "about 200" from the
  histogram's top eight values).
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
| Cost: a head episode in which the allowance is used | the drain of the allowance, about 0.8 minutes at `x = 2 GiB` (1.6 at 4 GiB) at the slice-wide 2.71 GB a minute; longer when what passed runs long (§3.3) | estimated (§3.3) |
| Cost: across all 461 holds, if every head lost exactly that drain | about 6 head-hours at 2 GiB (about 12 at 4 GiB) | estimated, not a bound |
| Cost: large heads that miss their hold because the allowance is taken inside it | 55 of 461 holds in the band at 2 GiB (103 at 4 GiB, 8 today); each such head meets a refilling backstop pass | estimated band (§3.3); S5 |
| Cost: a head the changed duty cycle starves where today grants it (and the reverse) | unbounded for that head; 1 phase of 30 one way, 3 the other, in W3 | model |
| Cost: a suite parent passing through the allowance | up to a suite lifetime for that head (W8c: 900 s) | model; today's class |
| Cost: a small job that never ends passing a spent-budget head | the head waits for the job's life | Round-4 witness; T22 |
| Cost: mid-size waiters (`y` < booking < head) | they lose room to allowance grants (W0: +60 s each) | model |

Neither side is precise, and their biases differ (round-3 finding 12):
- **The head-hours row is high.** It counts every hold as if its head lost the whole drain, and the
  allowance is not used in every hold.
- **The benefit row is biased both ways.** It is high, because not every wait that ends at a
  backstop pass was saveable. It is low, because it cannot see waits that end at a head's grant.
- **Halving `x` to 2 GiB halves the drain row and the hold band**, while covering just under 90% of
  the batches against 96%.

**Best estimate.** The benefit is near the low end: a few wait-hours in 3.5 days, plus the waits
that end at a head's grant, which the store cannot see. The estimated cost is of the same order or
larger, and its worst case is not bounded. **The evidence does not establish a net gain.** So this
spec does not recommend building the allowance now (Round 4).
- The measurement (§M) answers the benefit side first.
- If the owner then chooses the allowance, it ships as a declared trial with a rollback trigger
  (§7.4), as AIRA-324 did.

### 7.4 The trial, if the owner chooses it after §M

- **Baseline.** The §M week, measured by the same `held-while-fitting` line, which stays in.
- **Trial.** Seven days with the allowance on. Every rerun states both ends:
  - `docs/dev/aira-350-lane-yield-estimate.py --since <cutover> --until <cutover + 7 days>`;
  - `docs/dev/aira-350-held-fit.py --since <cutover> --until <cutover + 7 days>`.

  The yield script's `--until` default is a fixed pre-shipping instant
  (`aira-350-lane-yield-estimate.py:132`), so it must never be left to default.
- **What is compared** (window totals only, with no row join; a history row has no scope id,
  `internal/store/store.go:878-882`):
  - the mean wait per small job and per large job, from history;
  - small jobs' by-budget hours, from the `held-while-fitting` lines;
  - heads' cost, from the `small-lane head granted` lines: their wait, and how many small-job bytes
    passed them;
  - holds per day.
- **Keep threshold.** Let `saved = (small mean wait before - after) x small jobs after` and
  `cost = (large mean wait after - before) x large jobs after`.
  - Keep the lane if `saved > cost` and `saved` is at least 10% of the small jobs' wait-hours after.
  - Otherwise switch it off with `aira install --small-lane off` and report.
- **Rollback trigger, checked daily from day 2.** Roll back early if:
  - the running `cost` exceeds the running `saved`, or
  - any `small-lane head granted` line shows a wait over 30 minutes.

  30 minutes is the top of the 23-30 minute waits that AIRA-307 was built to end (queue-order spec
  §1.1). The check is a daily command for the operator, not a session cron: AIRA-324's latch was read
  3.5 hours late when a cron lapsed (`.aira/tickets/AIRA-324.md:8`).
- This is reported to the owner. It is not an automatic gate.

**Limits.** The store keeps 20 rows per exact signature, so frequent commands have lost rows: these
are shares of the retained sample. `budget` (the scope cap) stands in for the booking, which is exact
for ordinary confine jobs and with pin-charge in observe. The journal lines are complete for granted
waiters and miss waiters that leave ungranted.

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
- **The backstop pass lost to a pushed head** (round-3 finding 14). This was the incident's largest
  single loss. *Correction, 2026-11-18: it was not. Restoring that pass would have changed no grant
  in the incident, because no hold armed between 13:21:08 and 13:22:22. See
  `2026-11-18-admission-diagnostics-and-backstop-design.md` §2.*
  - At 13:21:07 the backstop pass ran with a pushed head in front and granted nothing behind it
    (`admit.go:2691-2692`).
  - The anchor is cleared at the start of that pass (`admit.go:2538`). So the next hold needs a
    fresh arm and another 5 minutes.
  - The allowance is closed in exactly that window (C5), so it does not help.
  - A candidate fix is its own ticket: do not clear the anchor on a backstop pass that a pushed head
    blocks. §M's by-push hours size it.
- **So this is a partial fix for the incident at best.**
  - In the incident the allowance could act only in 13:18:22-13:21:00 and 13:21:47-13:22:32, and only
    at instants with room and CPU.
  - After 13:23:01 the waits were capacity-bound (§1.2).
  - Of the ten jobs held at 13:18-13:21 (5788 MiB), about 3.6 GiB would still have waited at
    `x = 2 GiB` (5788 - 1326 - 768 = 3694 MiB), and about 1.6 GiB at 4 GiB.
- **Large jobs' own waits.** The allowance can lengthen a head's wait (§3.3).

## 9. Risks

- **R1. Suite parents through the allowance.** A test suite started through the allowance brings its
  exempt workers past the head for the suite's lifetime. Today a suite started through the budget
  does the same (W8c and W8d).
  - The allowance raises the number of 1 GiB suite parents that can pass a head. For the median
    4 GiB head that is 2 today, 4 at `x = 2 GiB` and 6 at 4 GiB. For a 1 GiB head it is 0 today and
    1 with the allowance (§3.3 premise 2).
  - At least 9.0% of small rows are suite parents. So when four small jobs pass a head, at least one
    is a suite parent with probability of at least 31%.
  - §13 Q6.
- **R2. Heads can wait longer, without a bound.** At most `min(x, R)` more bytes pass each head, but:
  - a small job that never ends can starve a head (Round 4);
  - large heads lose their hold (the band in §3.3);
  - the duty cycle changes, so a head that is granted today can starve, and the reverse (W3).

  Watched by the trial's rollback trigger (§7.4).
- **R3. Mid-size waiters** (`y` < booking < head) lose room that allowance grants take, before the
  next backstop pass.
- **R4. `y` is inclusive at 1 GiB, which is exactly the cold default.** Every unpinned first run
  (`fallback:no-history`, 1 GiB) is eligible. That is intended, since they are the incident's
  population, but it means two cold jobs use a head's whole default allowance.
- **R5. Operator surprise:** `held_by=head` for a small job means the pushed head, ineligibility,
  or the budget held it. The wording "held behind the head" is true in each case.
- **R6. Diagnosis drift:** `heldBy` and the used figure describe the last pass, the same property as
  `lastGrantable`.
- **R7. A stream of high-priority heads** each brings a fresh allowance. That is the same shape as
  today's per-head budget (§3.3 premise 3).
- **R8. Chains of small heads.** Each head has its own allowance, which is derived per pass
  (`admit.go:2770-2779`), so a large waiter queued behind a chain of small heads pays each head's
  allowance in turn. `min(x, R)` keeps each payment to the head's own size (C8).

## 10. Deferrals (explicit, not built here)

- **The allowance itself** (§3), until the owner decides after §M.
- **Do not clear the anchor on a backstop pass that a pushed head blocks** (§8). This is its own
  ticket, sized by §M's by-push hours.
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
| **The concurrent lane (round 2), and a return to it (round-3 finding 1)** | Unsound in production: witnesses W2, W3, W12 and W8c, run in the model (§3.3.1), and reproduced by the round-3 reviewer. It also fails the Round-4 witness. |
| **Building the allowance now** (round 4) | The benefit is not established (§7.3), and no lane meets "the head cannot starve" (Round 4). Measure first (§M). |
| Close the allowance while a hold is armed (round-3 finding 9, option a) | It removes the benefit: the incident's hold was armed under the previous head and covered 14800's whole spent-budget window. An arm inside a pass would also close it for T1's queue (§3.3). |
| Strict FIFO (Gemini's structurally safe base) | It reverses AIRA-307's budget and backstop pass, which are owner decisions. |
| The `aira top` footer and the `--list` line for a queue with no head | Not needed to tell "held behind the head" from "no room" (round-3 finding 15). |
| Guard G, with its RAM, per-job-headroom (C2) and CPU arms | It bounded the concurrent lane's footprint; the allowance needs no footprint bound. |
| GPU jobs ineligible (C3) | Not needed for the bound; the owner's rule is "fit VRAM exactly as today". |
| Counting a member's children (C7) | Did not stop the suite case (W8c: 900 s with it); induced findings 1, 2 and 13. |
| Lane membership flags and a derived lane sum | Only a concurrent lane needs them. |
| Eligibility and size on the pin (round-2 C1) | Reversed the approved contract (finding 4). |
| Raise the AIRA-307 budget for every job (e.g. `max(R/2, 4 GiB)`) | Large jumpers would spend it too. **This round reverses the earlier cut of a cumulative allowance.** Rounds 1-2 cut it because the incident's small jobs exhaust it, and that is still partly true: of the ten jobs held at 13:18-13:21 (5788 MiB), about 3.6 GiB would still have waited at `x = 2 GiB`, and 1.6 GiB at 4 GiB (§8). The concurrent alternative turned out unsound. |
| Alternative B as the primary design | More state, and a head pays up to `T` (§3.8). It is kept as the owner's option. |
| Runtime-aware backfill (start a small job only if it ends before the head could fit) | Needs runtime predictions, a judgement AIRA does not make. |
| A lane-grant column in history | Telemetry only; the journal line suffices (§7.4). |
| A "lane full" refusal code | The allowance never refuses; a refused job falls through to today's rule and waits. |

## 12. Rollout (of the allowance trial, if chosen; §M.6 has the measurement's)

The change is on the daemon side, and it needs the new unit lines. In order:
1. **Coordinate first.** Follow the shared daemon-restart care: check `aira confine --list` and give
   the peer sessions a heads-up **before** the install, because the install restarts the daemon.
2. **`aira install`.** It re-renders the unit with the defaults `2G`/`1G` and keeps the other
   options' installed values. The new `Environment=` lines change the unit's bytes, so the install
   restarts the present daemon itself (`internal/install/install.go:1287-1290`). No explicit
   `systemctl restart` follows: a second restart would drop the queue state and the counters again
   (round-3 finding 7).
3. **`aira skill install`**, for the generated text.

After it, `aira install --status` shows the two lane lines. The `small-job lane:` line in
`aira confine --list` appears while a head exists. The journal shows the startup line.

Release notes name the new install options, the four reply fields and the journal lines. Protocol 15
is unchanged.

## 13. Open questions (for the owner, through the plan gate)

These are labelled with words, not letters, so they cannot be confused with the owner's
"Option (A)", which is AIRA-351's warm start. Q1 decides what is built next. Q2-Q4 must all be
answered yes before the allowance can be built at all.

1. **What to do next.** Choose one:

   | Path | What it builds | What small jobs get | What heads pay |
   |---|---|---|---|
   | **Measure first** (recommended) | §M: diagnostics only, about 30 lines; then a decision on the figures (§M.5) | nothing yet; a measured answer in about a week | nothing |
   | **Trial now** | §3, the per-head allowance, with the trial and rollback trigger of §7.4; needs Q2-Q4 | up to `min(x, R)` past each held head, then today's wait for a backstop pass | at most `min(x, R)` more bytes ahead of each head, but an unbounded wait in the named cases (Round 4, §3.3) |
   | **Drop the lane** | nothing here; the effort goes to over-booking (AIRA-347, AIRA-318) | nothing from this ticket | nothing |

   For dropping: the evidence that the head rule, rather than CPU or a full ledger, is what holds
   small jobs is weak (§7). Over-booking is the larger lever (Round 4).
2. **C0a: cumulative per head, not concurrent.** The approved contract says "outstanding bookings
   `<= x`". The concurrent form is unsound (§3.3.1), so the choice is the cumulative form or no lane.
3. **C0b: an incremented counter, not a derived one.** The brief said the lane's counter must be
   derived, "never an incremented counter that can drift". `smallBypassed` is incremented. The case
   for accepting it (§2.2):
   - it is monotone and never decremented;
   - it belongs to one waiter and dies with it;
   - a restart loses it, which fails open;
   - it has no ground truth to drift from;
   - it is the same pattern as `bypassed`.
4. **C0c: the guarantee.** "The head cannot starve" is not achievable by any lane (Round 4). Accept,
   in its place, a byte bound of `min(x, R)` per head, plus these residual cases:
   - a small job that never ends starves the head it passed (T22);
   - large heads can miss their hold: 55 of 461 holds at `x = 2 GiB` against 8 today (§3.3, S5);
   - suite parents (Q6);
   - a changed duty cycle (W3).
5. **The default `x = 2 GiB` per head** (C9). It covers just under 90% of the batches released at
   backstop passes, against 96% for 4 GiB. It halves the hold band and the drain against 4 GiB. It is
   tunable at install.
6. **The suite residual (R1).** At `x = 2 GiB`, a median head can be passed by 4 one-GiB suite
   parents instead of 2. When four small jobs pass a head, at least one is a suite parent with
   probability of at least 31%. Accept it, or ask for the deferred child rule (§10).
7. **C5:** the allowance is closed behind a pushed head.
8. **C6:** the raw daemon default is off, and the product default is on through install.
9. **Inclusive `y`** captures the 1 GiB cold default (R4).
10. **§M's decision rule:** 10% of small wait-hours and 10 hours in 7 days (M.5), or other numbers.

## 14. Review log

### 14.0 Round 3 (16 findings), answered in round 4

**Grouped by root cause.**

| Group | Findings | Root cause | Fix-induced? |
|---|---|---|---|
| A. Contract | 1, 8 | The re-base changed approved semantics without the owner's yes. | Yes, by the round-3 re-base. |
| B. The head's wait | 2, 9, 10, 11 | Every lane is a new way past the head. Finding 2 holds for every lane design. | 9 and 11 by the re-base; 10 is round-2 finding 8, still open; 2 is design-independent. |
| C. Evidence and cost | 12, 14, 15 | The benefit was never measured, and `x` was carried over unexamined. | 12 by the re-base. |
| D. Tests | 4, 5, 13, 16 | Equivalent mutants, a missing refusal direction, and pinned incidental values. | 16, by round 3's answer to round-2 finding 10. |
| E. Spec errors | 3, 6, 7 | Claims not checked against the code. | No. |

**Fix-induced: 6 of 16** (1, 8, 9, 11, 12 and 16). Three consecutive rounds have had a blocker, so
the non-convergence rule applies. Round 4 does not patch the allowance into a recommendation. It
states that the requirement cannot be met by any lane, and it recommends measuring first (Round 4).

**Per finding.**

| # | Sev | Verdict | Evidence checked | Disposition |
|---|---|---|---|---|
| 1 | P1 | Confirmed, as a contract change. The fix hint's first branch (return to the concurrent cap) is rejected. | `.aira/tickets/AIRA-350.md:4` says "outstanding bookings". The concurrent cap fails W2, W3, W12 and W8c (§3.3.1), as the reviewer's own rerun shows (finding 16), and fails the Round-4 witness. | C0a is an owner question (§13 Q2). The spec is not buildable until it is answered. T6 is marked as encoding C0a. |
| 2 | P1 | Confirmed and re-derived: room 3008 MiB before the release; 4096 MiB after it with the lane off; 3008 MiB for ever with any lane. | `admitSliceHeadroom` (`admit.go:1041-1051`), the budget case (`admit.go:2694-2696`), `checkedAvailable` (`admit.go:2893-2925`). | Not fixable by any lane: stated in Round 4 and C0c, owner question §13 Q4, and pinned as T22. |
| 3 | P2 | Confirmed. | `sliceCeilingEffectiveMaximum` (`sliceceiling.go:752-760`) can return a ceiling below the bookings. The ARDR establish skips the ceiling (`admit.go:2343-2349`). | Invariant 1 restated as a postcondition of new grants, with signed deficits. T23 added. |
| 4 | P2 | Confirmed: an equivalent mutant. | The grant `continue`s at `admit.go:2725`, before the arm at `admit.go:2762`. | T12 now mutates a reachable anchor write. |
| 5 | P2 | Confirmed. | The physical-fit rule is at `admit.go:2664-2666`. | T10b added. |
| 6 | P2 | Confirmed. | `--until` defaults to `2026-11-18T12:00` (`aira-350-lane-yield-estimate.py:132`). `confine_peak_history` has no scope id (`store.go:878-882`). | Both bounds are required. The comparison uses window totals with no join. The journal lines carry their own booking and wait (§7.4, §M.5). |
| 7 | P2 | Confirmed. | The install restarts a present daemon whose unit bytes changed (`install.go:1287-1290`). | §12: peers are told before the install, and the explicit restart is dropped. |
| 8 | P1 | Confirmed. | The brief requires a derived counter; §3.2 increments one. | C0b is added as its own question (§13 Q3). The spec is marked not buildable until Q1-Q4 are answered. |
| 9 | P1 | Confirmed in substance. The fix hint's option (a) was checked and is rejected on evidence. | The journal's 13:16:07 hold, under head 14782, lasted to 13:21:07, so it covered 14800's whole spent-budget window. The review's "still fixes the incident's first window" does not hold. The in-pass arm (`admit.go:2762-2769`) also closes the allowance for T1's queue. | The weakened bound is stated (§3.3). The band is quantified with `docs/dev/aira-350-round4-queries.py` part 1: 8, 55 and 103 of 461 holds. The default is `x = 2 GiB`. S5 is the seam run. §13 Q4. |
| 10 | P1 | Confirmed. | Parent bookings: p50 1024 MiB, 634 of 753 at exactly 1 GiB, 110 at 512 MiB or less (round-4 part 2). `delegate_ram` was removed from the admit allowlist (`protocol.go:129-132`). | Multiplier and frequency in §3.3 premise 2, R1 and §13 Q6. |
| 11 | P2 | Confirmed. One figure corrected: 37 heads (8.0%) at or below 1 GiB, and 289 (62.7%) at or below 4 GiB, not "about 200". | Round-4 part 1. | C8: the allowance is `min(x, R)`. T20 added. R8 covers chains of small heads. |
| 12 | P2 | Confirmed. | Batch p90 is 2.12 GiB (yield part 3). | C9: `x = 2 GiB`. The cost table is re-done with both sides' biases (§7.3). The allowance becomes a trial with a rollback trigger, after §M. |
| 13 | P2 | Confirmed, all five parts. | (1) As 4. (2) S3 had no oracle. (3) A reordered `switch` sends in-budget small jobs to the allowance, so T4 could not kill it. (4) The brief's replay was missing. (5) No restart or small-head case. | (1) T12. (2) S3 prints only. (3) T4's mutation column is corrected; T1 and T2 kill the reorder. (4) T2 is now the brief's replay: 20 leases, room 3482 MiB, 14 small waiters, exact sets for the correct build and two mutations. (5) T20, T21, and S5 for the in-hold case. |
| 14 | P2 | Confirmed. | `admit.go:2691-2692`; the anchor clear at `admit.go:2538`. | Named in §8 as not fixed, and as the incident's largest single loss. Deferred as its own ticket (§10). §M's by-push hours size it. The header states the partial fix. |
| 15 | P2 | Confirmed. | §7: about 5 of 115.6 wait-hours; the freeze's share is not measurable from the store. | Adopted: measure first (§M). The `aira top` footer and the no-head `--list` line are cut. |
| 16 | P2 | Confirmed. | §3.3 says the duty cycle moves, so equal passes are incidental. | Equal-pass assertions removed (S1, S4). S3 is printed only. The "reconcile the model or the test" instruction is removed. Per-pass properties and the concurrent-mutation assertions are kept. |

**Independent check of the collapse.** Gemini (`gemini-pro-latest`, advisory) was given the restated
problem, the witness, the evidence and the three paths. Its verdict was "build diagnostics only". It
agreed that no bypass rule can prevent starvation without preemption or runtime limits. Its "strict
FIFO" base is not adopted (§11). The Fable plan gate remains the independent judge.

**Commands run in round 4:**
- under `aira confine` (named, a 256M reservation, exit 0, no `hint:` line in the trailer):
  `docs/dev/aira-350-round4-queries.py` against the store copy and the freeze-journal extract;
- not confined, as trivial reads: `grep` over the freeze-journal extract and the code.

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
| 3 | P1 | Confirmed: at the reviewer's phase the concurrent lane starves the head and today grants it at 350 s. Across 30 phases today also starves it in 29 (the backstop-refill class of queue-order §3.5). | W3 at offset 0 and the 30-phase sweep. | Claim withdrawn. **Not closed by the collapse:** at offset 0 the allowance also starves the head, because its grants do not arm the hold and the backstop pass moves (traced in the sim output). The wait bound is withdrawn too (§3.3); only the byte bound remains. |
| 4 | P2 | Confirmed | `.aira/tickets/AIRA-350.md:4` says "ledger charge <= y". | C1 reverted to `ledgerCharge()`. T10 now asserts eligibility on the booking. |
| 5 | P2 | Confirmed | Round-2 §6.2's property contradicted invariant 2. There were no exclusive, high-head or backstop tests. | Property rewritten (§6.2). T17, T18, T19 and T16 added. |
| 6 | P2 | Confirmed | The script's own LIMITS (`:35`); the matched median wait is 130 s (`out.txt` part 2). | Relabelled an association. "Whole 5-minute hold" removed (§7.1). |
| 7 | P2 | Confirmed | `protocol.go:129-136`, `confine_linux.go:1124-1131`; the `admit.go:1189-1193` comment is stale. | Premise corrected (§2.2). C7 deleted in any case. |
| 8 | P1 | Confirmed | Model W8c: `spec` (with C7) 900 s against 20 s today with the budget spent. Today's rules with a fresh budget give the same 900 s (W8d). | **Not closed by the collapse:** with the budget spent, the allowance also gives 150 s (W8) or 900 s (W8c). It is today's class, re-opened for every head with a spent budget. Stated as residual R1, in the header, and as round 3's §13 Q1 and Q2 (round 4's Q4 and Q6). |
| 9 | P2 | Mostly confirmed. One figure corrected: the snapshot shows 8 (not 9) of the 12 job-level small scopes older than 13 hours, and 5 (not 6) at about 5 days (ages 433762 x 4 and 432637; then 79205, 57496 and 50147 s). | `round3-queries` parts 2-4: small rows' run time, the `aitest-parent` share and the release rate. | Benefit and cost in one table (§7.3). Silting and a lane-closing suite do not apply to a per-head allowance. Mid-size waiters added to the cost (R3). |
| 10 | P2 | Confirmed | Round-2 T6 mutations are equivalent at its numbers. The 6.2(a) steady state is below `x`. 6.2(d) had no pass number. | G is deleted, so its mutations are moot. New tests cover the head change, the backstop classification, exclusivity and high-priority heads. S1-S4 assert pass numbers against lane-off runs. |
| 11 | P1 | Confirmed | Journal `confine-front` at 13:21:00 and 13:22:49. The yield line at 13:21:07 names 14816. History: 14816 granted at 13:21:47 after 317 s. One slot per job (`admission_linux.go:455`); `heldLease` has `cpu = 0`. | §1.2 rewritten as a timeline. T1 is built from 13:55 with one-slot leases, and T1b covers 32 leases. |
| 12 | P2 | Confirmed | Model W12: `spec` never, today 210 s. | Closed: the allowance gives 210 s. Option (a) of the fix hint is Alternative B (§3.8). |
| 13 | P2 | Confirmed (same as 5) | — | Property rewritten. |
| 14 | P2 | Confirmed | Large-job control: 91.5% overlap. Only 85 of 456 passes released a small job. | Bound called non-discriminating. Best estimate at the low end. CPU named as a confounder. Keep threshold given (§7.4). |

**Fix-induced findings:** 4 of 14 (1, 2, 8 and 13) came from the round-1 C7 fix.

**Commands run this round:**
- under `aira confine`, with no `hint:` line in any trailer: the model
  (`docs/dev/aira-350-lane-sim.py`), the hold-age script, the store copy, and
  `docs/dev/aira-350-round3-queries.sql`;
- not confined, as trivial reads: `journalctl` over a 16-minute window.

**Within this round.** The first commit of round 3 still claimed a bound on the head's wait, which
its own W3 output contradicted at offset 0. A second commit withdrew that claim, reported W3 per
phase, and stated findings 3 and 8 as not closed. **The choice of collapse has not been reviewed
independently of its author.**
