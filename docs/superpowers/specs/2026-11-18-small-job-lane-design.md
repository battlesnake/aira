# Small-job lane: small jobs pass a held head within a bounded share of the ledger (AIRA-350)

Status: plan, for plan review (Codex Sol at `xhigh`, Gemini) and the Fable plan gate. Ticket
AIRA-350, milestone v0.46. Correctness-critical (admission): two-loop, Opus builds, Fable reviews.
The owner's decisions of 2026-11-18 are restated in §2 and not reopened; §2.2 lists the changes this
plan makes to them for soundness, each one open to veto at the gate. Builds on AIRA-307/309 (queue
order, `docs/superpowers/specs/2026-10-13-aira-queue-order-design.md`) and AIRA-228 (one queue, one
ledger). AIRA-351 (warm-starting cold subjects) is separate, held, and blocked by this ticket.

Code references are to the worktree at `ae446ef1` (master).

## 0. The greenfield minimum, and the gap

**The problem in one line.** A queued job that does not fit (the head) holds smaller jobs that do fit,
for minutes, while the slice has room for them.

**The bare minimum.** Exempt every job-level waiter with `reserve <= y` from the head rule. That is one
more conjunct on the line that already exempts sub-reservations
(`protected := head != nil && !waiter.isSubReservation()`, `internal/daemon/admit.go:2688`). Three
lines of code.

**Why the minimum is not enough.** An unbounded exemption is the starvation AIRA-307 was built to end:
a steady stream of small jobs keeps the free room below the head's size forever. (The 2026-10-12
replay in the queue-order spec §1.1 shows 12 GiB heads waiting 23-30 minutes through exactly that
refill.) So the exemption needs a bound. Every addition below is the cheapest piece that keeps the
head from starving, or something the owner asked for.

| Addition | Why it is needed | Size |
|---|---|---|
| Cap `x` on what the exempted jobs may hold at once | Without it, a stream of small jobs starves the head (above). | one comparison |
| A per-waiter `laneGranted` flag, summed on demand | The cap must count only bookings that used the exemption. "Every granted job of size `<= y`" fails the incident: dozens of small jobs granted the ordinary way would fill the lane before any head appeared (§2.2 C4). | one bool, one derived sum |
| Guard G: the head must fit beside the lane | Without it, a head larger than `ceiling - x`, or a head short of CPU, is starved by lane refill (§3.3 cases 5 and 6). | one predicate, a sibling of `admitCouldFitEmptyLocked` |
| GPU jobs (`vram > 0`) are not lane-eligible | Without it, small-RAM GPU jobs refill VRAM in front of a VRAM-short head. | one conjunct |
| Off in ci-shim mode | Owner decision. | one conjunct |
| `x`, `y` set at install | Owner decision ("settable like `--pin-charge`"). | env + install plumbing, copied pattern |
| Lane figures and a per-waiter "held by" token | Owner and coordinator asked for them. Without them, "held behind the head" cannot be told apart from "no room". | optional reply fields, one diagnosis field |

**Not built:** a second slice, queue or ledger (AIRA-228 collapsed to one); any counter on the queue;
lane state carried across a daemon restart; a per-head lane; lane expiry; runtime-based backfill.
§11 lists everything that was cut.

## 1. Problem and evidence

### 1.1 The mechanism today (read from the code)

In each evaluator pass (`evaluateAdmitQueue`, `internal/daemon/admit.go:2472`), waiters are evaluated
in `admitOrderLocked` order (`admit.go:452-476`):
- **Who is the head.** The head is the first job-level waiter refused on capacity that could fit an
  empty slice (`admit.go:2770-2779`, `admitCouldFitEmptyLocked` at `admit.go:2843`).
- **What a later fitting waiter gets.** Every later job-level waiter that fits is then judged by the
  head rule (`admit.go:2687-2698`):
  - **pushed head** (`head.pushSeq > 0`): it waits;
  - **backstop pass** (`phase == admitFreezeYield`): it is granted;
  - **otherwise:** it is granted only while `head.bypassed + ledgerCharge <= head.ledgerCharge()/2`
    (`admitBypassBudget`, `admit.go:485-490`), else withheld (`budgetWithheld`).
- **The hold.** A budget withhold arms the AIRA-59 hold (`admit.go:2762-2769`). After `maxHold`
  (default 5 minutes, `internal/daemon/paths.go:66`), exactly one backstop pass grants everything that
  fits (`admit.go:2525-2540`).
- **The hold itself withholds nothing.** Hold and idle are judged identically. Only a pushed head, the
  budget, or the absence of a backstop pass keeps a fitting job waiting.
- **Sub-reservations** (aitest workers, `confine-reserve` per-test holds) are exempt and uncounted
  (`admit.go:2688`).

So once a head's budget (half its reserve) is spent, a small job that fits waits up to `maxHold` for
the next backstop pass.

### 1.2 The 2026-11-18 13:2x incident

Facts **reported** by the coordinating session, re-derived where marked:
- The admission ceiling was 45.2 GiB.
  - **Re-derived** from the daemon journal at 13:22:02: `52881784832 effective` (49.25 GiB), minus
    headroom `2 GiB + 64 MiB x (32+1) jobs` (`admitSliceHeadroom`, `admit.go:1041-1051`; defaults at
    `admit.go:67-68`), gives 45.19 GiB.
- 32 running jobs booked 41.5-45.4 GiB (reported). The slice's real anonymous memory was 14.9-16.2 GB
  (journal `sliceAnon` at 13:22-13:23, **measured**).
- 13-14 queued jobs of 0.07-1.0 GiB waited 4-6 minutes while about 3.4 GiB was free, behind a 4 GiB
  head that did not fit (reported).
  - The journal shows the hold `13:22:22 ... fairness-freeze hold ... head seq=14800
    reserve=4294967296 queued-for=6m20s (13 also queued)`.
  - It also shows the previous backstop pass at 13:21:07 (**measured**).

### 1.3 A second live instance, 2026-11-18 13:55 (snapshot taken while writing this plan)

From `aira confine --list --json`, saved to `~/tmp/aira-350/live-snapshot-1355.json`:
- **Ledger:** 42443 MiB granted against a 44736 MiB ceiling, so 2293 MiB of room. 28 jobs. Freeze
  phase `hold`.
- **Real use:** the slice's own memory was 18234 MiB current less 9361 MiB reclaimable, about
  8.7 GiB.
- **Queue:** a 5120 MiB head had been queued 552 s. Behind it:
  - two jobs larger than 1 GiB (2154 MiB and 2700 MiB);
  - six jobs of 256-1024 MiB, queued 84-218 s (one of them this plan's own measurement job).
- **Under this design,** with `x = 4 GiB` and `y = 1 GiB`, the first three small jobs in evaluation
  order (1024, 256 and 512 MiB) are admitted at once. The other three wait for room, not for the head
  (worked as in §6.1 T1).

### 1.4 Over three and a half days

Measured by `docs/dev/aira-350-lane-yield-estimate.py` (output in
`docs/dev/aira-350-lane-yield-estimate.out.txt`); details in §7. The window is
2026-11-15 00:00 to 2026-11-18 12:00 UTC, over the retained history rows that carry `wait_ms`.
- **Small jobs (reserve `<= 1 GiB`):** 34.4% of 8344 waited at least 10 s, totalling 115.6
  wait-hours. Of the waits of 10 s or more, the median was 75 s and the 90th percentile 352 s.
- **Large jobs:** 52.8% of 2401 waited at least 10 s, totalling 76.4 wait-hours.

## 2. Owner decisions, and the changes this plan makes

### 2.1 Restated (owner, 2026-11-18)

1. **The lane.** A small-job lane lives inside the one evaluator, as a rule in the one existing queue
   and ledger. There is no second slice, queue or ledger.
   - A job-level waiter of size `<= y` (default 1 GiB) is exempt from the head's freeze and its bypass
     budget while the lane's outstanding bookings plus its own stay `<= x` (default 4 GiB).
   - It must still fit the ceiling (RAM), CPU and VRAM exactly as today.
   - If the lane is full, or the job is larger than `y`, today's behaviour applies.
   - Lane grants do not count against `head.bypassed`.
2. **Carved out of the ceiling.** Bookings never exceed the ceiling.
   - Nothing is reserved while no small job runs. The lane is an entitlement cap, not a partition.
3. **Default daemon mode only.** The lane is off in ci-shim mode.
4. **Configuration.** `x` and `y` are settable at install, like `--pin-charge`. `off` disables the
   lane. The defaults are on.

### 2.2 Changes made here for soundness (each open to veto at the gate)

- **C1. Size and eligibility are measured on `reserve` (the pin, the scope's `memory.max`), not on
  `ledgerCharge()`.**
  - The owner's own words were "jobs **requesting** <= y GB", and the reserve is what a job requests.
  - `ledgerCharge()` can drop below the reserve in two ways (`admit.go:357-365`):
    - a `confine --resize`, which is reduce-only and leaves `memory.max` unchanged (dispatch text,
      `internal/core/core.go:2620`);
    - the AIRA-324 discount in enforce mode.
  - Counting bookings would let either one open lane room while the lane jobs can still use their full
    pins. A re-anchor that restores a resized booking (`admit.go:2318-2326`) could then push the lane
    past `x`.
  - Counted in reserve, the lane is unaffected by resize, discount and re-anchor. For scope-capped
    members it also bounds physical use: `memory.max = reserve`.
  - **Today the two are identical for every queued waiter.** `aira install` renders pin-charge
    `observe` (`internal/install/install.go:62`), the enforce trial was rolled back (`c4db60da`), and
    observe books at the reserve (`internal/daemon/pin_charge.go:120-125`).
- **C2. A per-job headroom term in the head guard (G, §3.1).** Every granted job also raises the
  headroom by 64 MiB (`admit.go:1041-1051`).
  - A full lane of many tiny jobs therefore takes more than `x` from the head's room. For example,
    4 GiB of 72 MiB jobs is 56 jobs, which is 3.5 GiB of extra headroom.
  - The cap `x` stays in pure reserve bytes, as the owner stated it, so 4 x 1 GiB fits.
  - The headroom is charged inside guard G instead, which keeps the head able to fit.
- **C3. GPU jobs are not lane-eligible (`vram == 0`).** This closes VRAM starvation of a GPU head
  (§3.3 case 6) at the cost of one conjunct. GPU jobs are rare and are never "small" in the sense
  that matters.
- **C4. Lane membership is decided at grant time and recorded on the waiter.** It is never inferred
  from size.
  - The owner's "small-lane outstanding bookings" read literally as "every granted job of size
    `<= y`" makes the lane useless in the incident it was asked for.
  - At 13:55 (§1.3 file), 12 job-level running scopes had a reserve of at most 1 GiB, 3392 MiB
    together. (This excludes the `aitest-w*` worker scopes, by name, an assumption.) 8 of them, 1536
    MiB, were servers older than an hour, 5 of them older than 5 days.
  - A lane derived from size would have had about 700 MiB left for the six queued small jobs. The
    flag design had the whole 4 GiB.
  - The lane is the owner's own framing: "an entitlement cap on how much of the ledger small jobs may
    take past a frozen head". So only grants made past a head count.
- **C5. The lane is closed behind a pushed head.** AIRA-309's owner decision (A) is "nothing bypasses
  a pushed head except another push" (queue-order spec §2), and a push means "drain the slice for this
  job". The lane decision was about the ordinary head's freeze and budget.
- **C6. The daemon's raw default is off; `aira install` renders the lane on (4G, 1G).** This is
  exactly the pin-charge pattern (`pin_charge.go:31-33`). A daemon started outside the installed unit,
  and every existing test, is unchanged. The product default is on.

## 3. Design

### 3.1 The rule

In the evaluator, inside `if protected && fits` (`admit.go:2689`), with one case added before the
budget:

```
switch {
case head.pushSeq > 0:                                   // unchanged: nothing passes a pushed head (C5)
    fits = false; heldBy = "head"
case s.smallLaneAdmitsLocked(head, waiter, lane, effectiveMaximum, &heldBy):
    viaLane = true                                       // lane grant: not counted in head.bypassed
case phase == admitFreezeYield:                          // unchanged: the backstop pass
case addClamp(head.bypassed, waiter.ledgerCharge()) > admitBypassBudget(head):
    fits = false; budgetWithheld = true                  // unchanged
    if heldBy == "" { heldBy = "head" }
}
```

On a grant:
- `viaLane` sets `waiter.laneGranted = true`, skips `head.bypassed`, re-derives `lane`, and logs one
  line (§3.6).
- Otherwise the code is today's: `head.bypassed += waiter.ledgerCharge()` when protected
  (`admit.go:2710-2715`).

`smallLaneAdmitsLocked(head, w, lane, effectiveMaximum)` is true only when all of these hold:

1. **On:** `s.smallLaneCap > 0 && !s.shimMode()`. Belt and braces with Serve's forced-off, as
   `pinChargeFor` does at `pin_charge.go:121` beside `server.go:420-425`.
2. **Eligible:** `w.reserve <= s.smallLaneJobMax && w.vram == 0`.
   - Being job-level is already implied by `protected`.
3. **Entitled:** `lane.reserve + w.reserve <= s.smallLaneCap`.
   - If this fails, set `heldBy = "small-lane-full"` and return false.
4. **Guard G, the head still fits beside the lane after this grant, in RAM and CPU:**
   - RAM: `head.reserve + (lane.reserve + w.reserve) + (lane.jobs + 1) x perJobHeadroom
     <= subtractFloor(effectiveMaximum, s.admitSliceHeadroom(1))`
   - CPU: `head.cpu + lane.cpu + w.cpu <= s.cpuCeiling()`

   VRAM needs no arm, because lane members hold none (C3). G is `admitCouldFitEmptyLocked`
   generalised from "an empty slice" to "a slice holding only the lane". Like that function it judges
   RAM on the head's pin (`admit.go:2838-2842`). If G fails, set `heldBy = "head"` and return false.

`lane` is a **pass-local** value: `laneUsedLocked(queue)` is called once before the waiter loop and
again after each lane grant. It returns `(reserve, cpu, jobs)`, summed over waiters with
`state == admitGranted && accounted && laneGranted`.
- Nothing is incremented and nothing is stored on the queue.
- The evaluator holds `queue.mu` for the whole pass, and the only writer of grants is this pass, so
  re-deriving after each lane grant is exact. Releases and re-anchors happen between passes, under the
  same lock.

`heldBy` is a per-waiter **diagnosis-only** string, written in the refusal branch next to
`noteGrantableLocked` (`admit.go:2760`). No admission decision reads it, the same discipline as
`lastGrantable` (`admit.go:261-280`).
- It is `""` when the waiter was refused on capacity, or when no head protected it.
- It is overwritten every pass, so it describes the last evaluation.

### 3.2 State

- **`admitWaiter.laneGranted bool`.** It is written once, by the evaluator under `queue.mu`, at the
  grant, and never cleared. It is the only new admission state.
  - It cannot outlive its waiter. A release makes the waiter `admitReleased`, and the derived sum drops
    it.
- **`admitWaiter.heldBy string`.** Diagnosis only (§3.1).
- **`Server.smallLaneCap`, `Server.smallLaneJobMax` (int64).** Read once in Serve (§3.5). Zero means
  off.
- **No queue field, no counter, no running total.** "The lane counter is derived from the waiter set"
  holds by construction, on the same argument as `rederiveLedgerLocked` (`admit.go:374-412`).

### 3.3 Why the head cannot starve

**Claim.** Let H be an unpushed head that stays the head. Assume:
- the effective ceiling does not shrink;
- every lease that is not a lane member ends in finite time;
- after H's budget is spent, nothing outside the lane is granted past H.

Then H is granted in finite time. The third assumption excludes the AIRA-307 backstop pass,
sub-reservations and re-declares; §3.3.1 covers each.

**Argument.**
1. Let `N` be the granted leases that are not lane members, sub-reservations included. Under the
   assumptions `N` only shrinks, so it empties in finite time.
2. Every lane grant made while H is head satisfies G for H. Removing a member preserves G, because it
   only lowers G's left side, and the right side does not depend on the lane.
3. Lane members granted under an earlier head may violate G for H. No new lane grant is made while G
   fails, and those members end in finite time (assumption 2). So from some moment on, G holds for H
   at every pass.
4. Once `N` is empty:
   - `outstanding = Σ ledgerCharge(lane) <= lane.reserve`;
   - `outstandingJobs = lane.jobs`;
   - `admitSliceHeadroom(lane.jobs + 1) = admitSliceHeadroom(1) + lane.jobs x perJob`.
5. So the room H sees is `effectiveMaximum - admitSliceHeadroom(1) - lane.jobs x perJob - outstanding`.
   After a lane grant, G for the enlarged lane reads
   `head.reserve + lane.reserve + lane.jobs x perJob <= effectiveMaximum - admitSliceHeadroom(1)`.
   With `outstanding <= lane.reserve`, that makes the room at least `head.reserve`. H's CPU fits by
   G's CPU arm. VRAM is as for an empty slice, because lane members hold none and `N`
   is empty. `head.ledgerCharge() <= head.reserve`.
6. Steps 4 and 5 assume the ledger term dominates `checkedAvailable` (`admit.go:2906-2925`).
   `effectiveCurrent > outstanding` is the existing accepted physical-floor gap (queue-order spec §3.5).
   So H fits. ∎

**What the lane costs a head.** Compared with today, H's worst case is "the ceiling is smaller by the
lane's footprint". That footprint is `lane.reserve + lane.jobs x 64 MiB`, at most `x` plus the
per-job headroom, and never more than `ceilingNet(1) - head.reserve` (by G).
- **Estimated, not measured:** at the release rate the queue-order spec reports for 2026-10-12, about
  3.6 GB a minute, draining an extra 4.5 GiB of other bookings takes about 1.3 minutes per head
  episode.
- **Partly offsetting it:** small jobs no longer exhaust the budget, so fewer holds arm and fewer
  backstop passes refill the slice in front of heads (§1.1: the backstop pass grants everything that
  fits).

#### 3.3.1 Counterexamples examined

| # | Case | Without the guard named | Under this design |
|---|---|---|---|
| 1 | **Repeated lane refill:** small jobs keep arriving and replacing each other | Unbounded refill starves H (the §0 minimum). | Lane reserve `<= x` at every instant, and G keeps H's slot. H fits once `N` drains (§3.3). |
| 2 | **Long-running small jobs** (a confined dev server) granted through the lane | — | They hold lane room for their lifetime ("silting"). Bounded by `x`, so heads are unaffected beyond the §3.3 cost, but small-job relief shrinks. Visible as lane bytes (§3.6). Accepted (§9 R2). |
| 3 | **High-priority head** (`--priority high`) | — | An ordinary unpushed head (AIRA-340, `admit.go:302-311`). The lane applies and the proof is unchanged. A high waiter is never behind a normal head (`admitOrderLocked` tier 2). |
| 4 | **Pushed head** | — | Lane closed (C5). AIRA-309 invariant 3 is unchanged. |
| 5 | **Head larger than `ceiling - x`**, e.g. 46 GiB on a 47.2 GiB empty-slice ceiling | Lane refill keeps more than `ceiling - R` occupied, so H never fits, and the backstop does not help it. | G's RAM arm closes the lane once the footprint reaches `ceiling - R`. Behind such a head its budget is large anyway (`R/2 >= (ceiling - x)/2`). |
| 6 | **CPU-short head** (e.g. a 16-core head, 1-slot lane jobs) | Lane jobs take a slot each: up to `x / (reserve + 64 MiB)` jobs, which can exceed the 32-slot ceiling (`admit.go:2950-2958`, 2 x 16 on this box per the 2026-11-17 journal). Refill starves H. AIRA-307's budget bounds this today only because it is cumulative. | G's CPU arm: `head.cpu + lane.cpu + w.cpu <= cpuCeiling`. |
| 6b | **VRAM-short head** | Small-RAM GPU jobs refill VRAM. | Not lane-eligible (C3). |
| 7 | **`y > head`** (a 1 GiB head, 0.5 GiB lane jobs) | — | The lane passes a small head. G holds trivially, and the proof does not use `y`. |
| 8 | **Ceiling shrinks** under system pressure after the lane filled | — | G is re-evaluated every pass with the current `effectiveMaximum`, so the lane closes and members drain. If H can no longer fit an empty slice it stops being head (`admit.go:2770`), exactly as today. |
| 9 | **Daemon restart** | — | Flags are lost. A re-established lease is built by `newEstablishedWaiter` (`admit.go:2114-2125`) with `laneGranted == false`, so former lane members become ordinary bookings in `N` and drain. The lane can refill up to `x` once more, so a head may wait one extra lane's worth once per restart. Fail-open for small jobs, ledger-safe (§3.4). |
| 10 | **Many tiny jobs** fill `x` with headroom | 56 jobs of 72 MiB take 4 GiB plus 3.5 GiB of headroom. | G charges 64 MiB per member (C2). |
| 11 | **Backstop pass** | — | Unchanged. The lane case is evaluated first, so a lane-eligible job in the backstop pass is a lane grant while the lane has room. Lane grants never arm a hold. |
| 12 | **Sub-reservations past H** (aitest workers) | — | Unchanged: exempt and uncounted (AIRA-307 accepted gap). Never lane members, because `protected` is false for them. |

### 3.4 Interactions

| Interaction | Decision | Reason / code |
|---|---|---|
| Pushed head (`--front`) | Lane closed. | C5; the existing `case head.pushSeq > 0` precedes the lane case. |
| `--exclusive` drain or hold | Unchanged: the lane never applies to a gate-blocked waiter. | The gate `continue`s before the head logic (`admit.go:2604-2621`). |
| `--priority high` | The lane applies behind a high head. | A high head is an unpushed head (AIRA-340). |
| Fairness-freeze duty cycle | Lane grants are made in every phase, never set `budgetWithheld`, and never arm. A lane-full or ineligible job that the budget then withholds arms as today. | The arm stays guarded by idle (`admit.go:2762`). The three defects in the `freezeArmedAt` comment (`admit.go:812-822`) stay unwritable: no new anchor write. |
| AIRA-324 discount | Eligibility and lane size are in reserve (C1). The physical-fit rule is unchanged, because it is part of `fits`, computed before the head rule (`admit.go:2663-2666`). | C1. |
| Sub-reservations and worker-admit | Unchanged: exempt, never lane members. | worker-admit requires `parent_scope_id` (`internal/daemon/worker_admit.go:162`) and enqueues through the same path (`worker_admit.go:475`). |
| Re-declare onto a live lease (same daemon) | `laneGranted` is kept: same waiter, no code. Its lane size follows the re-declared reserve, which a client sends as the reserve it was granted. | `admit.go:2318`. |
| Re-declare after a restart (ARDR establish) | Not a lane member. | Case 9; no ARDR frame change. |
| `confine --resize` | No effect on the lane. | Reserve-based (C1); resize writes only `target.charge` (`internal/daemon/confine_resize.go:139`). |
| `confine --kill`, reaper, client EOF | Release, then the derived sum drops the member. | The `admitReleased` state. |
| Restart freeze | Unchanged: no grants at all during it. | `admit.go:2592-2595`. |
| Fail-fast latch | Not reachable: shim-only, and the lane is off in shim. | `admit.go:2494`. |
| ci-shim mode | Off: `smallLaneCap` forced to 0 in Serve, and the predicate checks `!shimMode()`. | Owner decision 3. Also, ci-shim is ledger-only (`admit.go:2636-2640`) and one CI cohort's order is the pipeline's own choice. |

### 3.5 Configuration (the pin-charge pattern)

**Daemon.** `smallLaneConfigFromEnv()` lives in a new `internal/daemon/small_lane.go`, beside
`pinChargeConfigFromEnv` (`pin_charge.go:53-90`), and has the same mode-then-the-rest shape:
- **`AIRA_DAEMON_SMALL_LANE`:**
  - unset or empty means off (the raw default, C6), and so does `off`;
  - otherwise the value is parsed by `runner.ParseMemorySize`;
  - zero or a parse error is refused:
    `E_CONFIG_INVALID: AIRA_DAEMON_SMALL_LANE must be off or a byte size such as 4G`.
- **`AIRA_DAEMON_SMALL_LANE_JOB_MAX`:**
  - read only when the lane is on, so a typo cannot refuse to start a daemon that did not ask for the
    lane (the `pin_charge.go:69-73` ordering);
  - unset means 1 GiB;
  - anything below 1 MiB (`runner.MinPinnedScopeCap`) or above the lane size is refused:
    `E_CONFIG_INVALID: AIRA_DAEMON_SMALL_LANE_JOB_MAX must be a byte size from 1M up to the lane size,
    such as 1G`.
- **Serve** reads both next to the pin-charge read (`internal/daemon/server.go:416-429`):
  - in shim mode it forces the lane off;
  - it logs `aira daemon: small-job lane 4G for jobs up to 1G` when the lane is on.

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

No upper bound is placed on `x`. An `x` near the ceiling is self-limiting, because guard G closes the
lane behind any head with `head + x` above the empty-slice ceiling. That makes it an ineffective
setting, not an unsafe one. The help text says so.

### 3.6 Observability (minimal)

- **Reply fields.** `runner.ConfineSliceReserve` (`internal/runner/confine_manage.go:586`) gains four
  optional fields, filled in `admitSliceSnapshotFor` (`admit.go:1302`) in the same locked walk, from
  `laneUsedLocked`:
  - `small_lane_bytes`, `small_lane_jobs`, `small_lane_cap_bytes`, `small_lane_job_max_bytes`.
  - The cap is the honesty bit. When the cap is present, the bytes and jobs are established even when
    zero.
  - When the cap is absent (lane off, ci-shim, or an older daemon), nothing is rendered.
- **One per-waiter field.** `held_by` (`head` or `small-lane-full`) is taken at the same queued-scope
  match as `queue_position` (`admit.go:1352-1364`), and only for a caller that named its scope id.
- **`aira confine --list` text.** One line after `slice queue:` (`cmd/aira/main.go:3765-3779`), when
  the cap is present:
  `small-job lane: 2960M of 4096M held by 8 jobs (jobs of at most 1024M pass a held head while the lane
  has room)`.
  - The `hold` note on the `slice queue:` line (`main.go:3773`) gains "jobs of at most 1024M still pass
    through the small-job lane while it has room".
- **Wait line** (`confineQueueNote`, `internal/runner/confine_queue_position_linux.go:151-221`):
  - `held_by=head` adds ", fit at the last check but held behind the head of the queue";
  - `held_by=small-lane-full` adds ", fit at the last check but the small-job lane is full
    (3984M of 4096M)";
  - absent adds nothing.
- **`aira top` footer** (`cmd/aira/tui_top.go:1546-1547`): `lane 2960M/4096M` is appended when the cap
  is present. One conditional append.
- **Daemon journal.** One line per lane grant:
  `aira daemon: small-lane grant scope=<id> reserve=<n> lane=<bytes>/<cap> head seq=<n> reserve=<n>`.
  This is the post-ship measurement hook (§7.3). It is bounded by job arrivals, not by pass rate.

### 3.7 Generated text (faces)

The pattern is AIRA-340's (`internal/core/skill.go:305-318`): a long `ConfineSmallLaneGuidance` and a
short `ConfineSmallLaneSummary` in `internal/core/skill.go`.
- **The skill:** the sentence at `skill.go:338` ("Without a push, a waiting job that does not fit lets
  smaller jobs past it only up to half its own reserve, ...") is extended with the long form.
- **The install option descriptions** carry the long form, so `aira install --help` and the MCP schema
  have it.
- **The `confine` verb summary** (`core.go:2607`) and the MCP instructions (`cmd/aira/mcp.go:26-29`)
  append the short form.
- **The test:** `TestConfineSmallLaneGuidanceIsOnEveryFace` pins the wording across the faces,
  modelled on `TestConfinePriorityGuidanceIsOnEveryFace` (`cmd/aira/confine_priority_test.go:100`).

Draft text:
- **Long:** "A small job (reserve at most the small-job lane's job size, 1G by default, and no
  `--vram`) passes a waiting head that does not fit, without using that head's budget, while the jobs
  admitted this way hold at most the lane size together (4G by default). It is still admitted only if
  it fits the slice's memory, CPU and VRAM ceilings, and never when the head could then no longer fit
  beside the lane. It never passes a job pushed with `--front` or an exclusive drain. Default mode only:
  off under `--ci=shim`. Set at install with `--small-lane SIZE|off` and `--small-lane-job-max SIZE`."
- **Short:** "Small jobs (default at most 1G) pass a waiting head through a bounded small-job lane
  (default 4G in total), still only when they fit."

## 4. Invariants

1. **The ledger is untouched.** `outstanding`, `cpuOutstanding`, `vramOutstanding` and
   `outstandingJobs` are still written only by `rederiveLedgerLocked`. Every lane grant passed today's
   `fits`, so `Σ booking <= ceiling - headroom` holds as before (carved out of the ceiling).
2. **Lane size `<= x` after every lane grant.** Only lane grants add members, and each is checked.
   Releases only remove members, and resize does not change a reserve. The one exception is a
   re-declare of a live lease with a larger reserve than it was granted (same-uid only), which can
   exceed `x`; no lane grant is then made until the lane is below the cap again.
3. **Guard G holds for the current head after every lane grant made while it is head** (§3.3).
4. **Lane grants never change `head.bypassed`, never set `budgetWithheld`, and never arm the hold.**
   AIRA-307 invariant 2 (an unpushed head is bypassed by at most `reserve/2` of non-lane job-level
   grants outside backstop passes) still holds verbatim, now for non-lane grants.
5. **A pushed head is never bypassed** by a job-level waiter (AIRA-309 invariant 3, unchanged).
6. **All lane state is derived from waiters.** It cannot outlive its waiter. A restart fails open, and
   nothing needs operator clearing.
7. **Off is today's behaviour exactly.** `smallLaneCap == 0` (the raw default and ci-shim) makes the
   lane case unreachable, and every existing test passes unchanged.
8. **Precedence is unchanged.** The restart freeze, the fail-fast latch, the exclusivity gate and the
   pushed head all come before the lane.

## 5. Wire and protocol impact

- **ProtocolVersion stays 15** (`internal/daemon/protocol.go:218`). The admit request, the admit
  grant and the ARDR re-declare frames are unchanged.
- **The `confine-list` reply gains five optional fields** (§3.6).
  - The client decodes it with `json.Unmarshal` (`confine_queue_position_linux.go:388`), not with
    `DisallowUnknownFields`, so an old client ignores them.
  - A new client against an old daemon finds them absent and renders nothing.
- **The behaviour change is daemon-side.** It takes effect only when the installed unit carries the new
  `Environment=` lines and the daemon restarts (§12).

## 6. Tests (each must fail against the wrong implementation)

All evaluator tests use the AIRA-228/307 seam: a hand-built queue (`queueOf`,
`internal/daemon/admit_queue_order_test.go:42-50`), a fake memory reader and clock, and
`evaluateAdmitQueue` driven directly with no evaluator goroutine.
- **Headroom.** Unless a test says otherwise, it sets production headroom (2 GiB base, 64 MiB per
  job) so that the per-job term is exercised. `admitTestServer` zeroes both
  (`internal/daemon/admit_test.go:23-24`).
- **Lane settings.** Lane on, `x = 4096 MiB`, `y = 1024 MiB`, set on the Server fields directly.
- **Head budget.** Unless a test says otherwise, the head's budget is preset as spent
  (`bypassed = admitBypassBudget(head)`). A waiter the lane refuses then shows as a withhold, instead
  of being granted through the budget.

### 6.1 Evaluator

Every test in this table asserts the exact set of granted waiters.

| Test | Setup | Assertions | Today's code, or the named mutation |
|---|---|---|---|
| **T1 incident replay** (regression pin) | `effectiveMaximum = 50432 MiB` (the 13:22:02 sample). 32 held leases sum to 42790 MiB, which leaves room 3482 MiB = 50432 - (2048 + 33 x 64) - 42790 (ceiling net 46272 MiB = 45.19 GiB). A 4096 MiB head with `bypassed` preset to 2048 MiB (budget spent) and `freezeArmedAt = now - 60s` (hold). 14 small waiters in this order, in MiB: 72, 256, 512, 1024, 512, 256, 1024, 72, 512, 1024, 256, 512, 72, 1024. | Lane grants: waiters 1, 2, 3, 4, 5, 6, 8 and 11, totalling 2960 MiB. Waiters 7, 9, 10, 12, 13 and 14 are refused on capacity (room after each grant is `3482 - Σgranted - 64 x grants`), with `heldBy == ""`. `head.bypassed` is still 2048 MiB. Lane is 2960 MiB with 8 jobs. `outstanding` is at most the ceiling. | Today's code grants **0** of 14. |
| **T2 lane cap binds** | As T1, but the head is 10240 MiB (`bypassed` preset to 5120) and the held leases leave room 8192 MiB. | Granted: 1, 2, 3, 4, 5, 6, 7, 8, 11 and 13, a lane of 4056 MiB with 10 jobs. Waiters 9, 10, 12 and 14 are held with `heldBy == "small-lane-full"`. | Remove the cap: all 14 granted. |
| **T3 boundaries** | — | `lane + reserve == x` is granted, one byte more is lane-full. `reserve == y` is eligible, `y + 1` is not (`heldBy == "head"`). G at equality is granted, one byte more is held. | — |
| **T4 lane-first keeps the budget** | Zero headroom. An 8 GiB head with `bypassed = 0`, room 7 GiB. Three 1 GiB small waiters, then a 3 GiB waiter. | All four granted; `head.bypassed == 3 GiB`. | Count lane grants in `bypassed`: the 3 GiB waiter is withheld. |
| **T5 membership is the flag, not size** | No head at first: five 1 GiB jobs are granted ordinarily (`laneGranted == false`). Then a non-fitting 8 GiB head arrives with its budget spent, followed by a 512 MiB waiter. | The 512 MiB waiter is granted as lane. | Derive lane membership from size: the lane reads 5 GiB and the waiter is withheld. |
| **T6 G's RAM arm** | `effectiveMaximum = 50432 MiB`, so the ceiling net for one job is 48320 MiB. A 45056 MiB head that could fit an empty slice. Held leases leave room 5120 MiB. Four 1024 MiB waiters. | Three granted (footprint 3 x 1088 = 3264 MiB, G at equality). The fourth (room 1856 MiB, so it fits) is held with `heldBy == "head"`, although the lane cap would allow it. | Drop the arm: the fourth is granted. |
| **T7 G's CPU arm** | 16 cores at 2 slots per core (32 slots), set through the core-counter seam. Held leases hold 14 slots. The head needs 16 slots and is RAM-short. 20 small waiters of 1 MiB at 1 slot each. | 16 lane grants; the 17th is held with `heldBy == "head"`. | Drop the arm: 18 granted, and the head is left CPU-short. |
| **T8 pushed head** | A lane-eligible waiter behind a pushed head. | It waits in hold, idle and backstop passes, with `heldBy == "head"`. | Evaluate the lane case before the pushed case: granted. |
| **T9 ci-shim** | A shim server with the lane fields set directly. A small waiter behind a head whose budget is spent. | Withheld. | Drop the `!shimMode()` conjunct: granted. |
| **T10 GPU job** | A small waiter with `vram > 0`. | Not a lane grant. | — |
| **T11 sub-reservation** | A granted sub-reservation past the head. | It never sets `laneGranted` and is never counted in `laneUsedLocked`. | — |
| **T12 no arm from lane grants** | Idle phase, only lane grants. | `freezeArmedAt` stays zero. | Set `budgetWithheld` on the lane path: armed. |
| **T13 release is derived** | A full lane, and a waiter held as lane-full. Two members are released through `releaseAdmitWaiterLocked`. | The next pass grants the waiter as lane. | Use a lane counter that is incremented and never decremented: still held. |
| **T14 restart and re-anchor** | Re-declare: `enqueueReDeclare` establishes a lease. Re-anchor: a lane member re-anchored through the SET path (`admit.go:2269-2335`). | The established lease has `laneGranted == false`, and the lane reads 0 afterwards. The re-anchored member keeps `laneGranted`, and its lane size equals the re-declared reserve. | — |
| **T15 resize** | A lane member's `charge` is lowered to 256 MiB. | `laneUsedLocked` is unchanged and no extra lane grant is made. | Count the lane in `ledgerCharge`: one extra grant. |
| **T16 discount** | Pin-charge enforce. A waiter with a 1228 MiB pin and a 983 MiB booking. | Not lane-eligible. | Judge eligibility on `ledgerCharge`: granted. |
| **T17 diagnosis** | — | `heldBy` is set on refusal, cleared on a capacity refusal, and never read by any decision (the guard-test pattern of `admit_pin_charge_guard_test.go`). | — |

### 6.2 Starvation simulation (the proof, run)

- **Setup:**
  - deterministic passes with `maxHold = 0`, so there are no backstop passes;
  - a head with its budget preset as spent;
  - 30 non-lane leases of 1 GiB, released one per pass;
  - two 512 MiB small jobs arriving every pass, each released three passes after its grant.
- **(a) Head of 4 GiB:** it is granted by pass 31 at the latest.
- **(b) Head larger than `ceiling - x`:** a 47104 MiB head against a ceiling net for one job of
  48320 MiB. G caps the lane footprint at 1216 MiB (two members), and the head is granted by pass 31.
  - Drop G's RAM arm and the head is still queued after 1000 passes.
- **(c) CPU variant of (b),** with 1-slot lane jobs and a 16-slot head.
- **Property check:** after every pass, `outstanding <= ceiling` and `lane.reserve <= x`.

### 6.3 Configuration and install

- **Environment table:**
  - `AIRA_DAEMON_SMALL_LANE` unset or `off` is off; `4G` is 4 GiB; `0`, `-1` and `abc` give
    `E_CONFIG_INVALID`;
  - `AIRA_DAEMON_SMALL_LANE_JOB_MAX` is ignored when the lane is off (a typo is not refused); it is
    refused below 1 MiB and above `x`, and defaults to 1 GiB.
- **Serve:** shim mode forces the lane off. Startup log line.
- **Install:**
  - the flags parse and are refused with `E_INSTALL_ARGUMENT_INVALID`;
  - a value is kept from the installed unit when not given; ship defaults `4G`/`1G` are rendered;
  - `renderDaemonUnit` re-validates; given flags are forwarded on re-exec;
  - `--status` prints both lines; the shim environment carries `AIRA_DAEMON_SMALL_LANE=off`.

### 6.4 Observability and faces

- **Snapshot:** the lane figures are present with the lane on (zero included) and absent with it off.
  `held_by` is reported only for the named queued scope.
- **Renderers:** the `--list` text line and the `hold` note, the wait-line clauses for both `held_by`
  values and for absence, the `aira top` footer, and the journal line.
- **Faces:** `TestConfineSmallLaneGuidanceIsOnEveryFace` covers the skill, `install --help` and its
  JSON, the `confine` summary, and the MCP instructions.

### 6.5 Mutation list, by direction

- **False pass** (grants what must wait):
  - remove the cap (T2);
  - drop G's RAM arm (T6, 6.2b);
  - drop G's CPU arm (T7, 6.2c);
  - lane case before the pushed case (T8);
  - lane on in shim (T9);
  - allow `vram > 0` (T10);
  - judge eligibility on `ledgerCharge` (T16);
  - size the lane in `ledgerCharge` (T15);
  - `<` / `<=` swaps at the boundaries (T3).
- **False fail** (holds what should be granted):
  - delete the lane case (T1);
  - count lane grants in `bypassed` (T4);
  - membership by size (T5);
  - an incremented counter (T13);
  - set `budgetWithheld` or arm on lane grants (T12);
  - drop the per-pass re-derive, so a later lane grant in the same pass reads a stale sum (T1, T2:
    over-grant or under-grant);
  - a `heldBy` value read by a decision (T17).
- **The gate** runs each mutation and records it RED, by the `docs/dev/aira-324-mutation-check.py`
  pattern: a committed `docs/dev/aira-350-mutation-check.py`.

## 7. Expected yield

### 7.1 Measured (`docs/dev/aira-350-lane-yield-estimate.out.txt`)

The window is 2026-11-15 00:00 to 2026-11-18 12:00 UTC. The rows are `confine` rows in
`confine_peak_history` that carry `wait_ms` and `started_at_ms` (AIRA-323 columns). The journal holds
461 holds and 456 backstop passes in the window.

- **Population.**
  - Small jobs (`budget <= 1 GiB`): 8344 rows. 2874 (34.4%) waited at least 10 s, 1607 at least 60 s
    and 543 at least 240 s. 115.6 wait-hours in total.
  - Large jobs: 2401 rows, 52.8% waited at least 10 s, 76.4 wait-hours.
- **Upper bound.** 2598 of the 2874 small waits (90.4%) overlapped a hold window, and 100.6 of the
  114.4 wait-hours (87.9%) fell inside one.
  - A hold is armed only when the budget withholds a job that fits (`admit.go:2762`), so these are the
    hours in which the freeze could have been the blocker.
  - Some of that time the ledger was simply full. Holds cover at most about 46% of the wall-clock
    time (461 x at most 5 min in 84 h), so overlap is common by chance alone.
- **Positive identification (lower bound).**
  - 130 small waits ended within [-1 s, +3 s] of a backstop pass. Shifted control windows (+60 s and
    -60 s) catch 25 and 34, so about 100 waits (about 3.5%, roughly 5 wait-hours) are positively
    identified as held by the freeze while they fitted.
  - These are only the waits that sat through a whole 5-minute hold. A job released when the head was
    granted is not caught.
- **Lane size against the batches.** For the 85 backstop passes that released at least one small job,
  the released small jobs' lane footprint (reserve + 64 MiB) had a median of 1.06 GiB, a 90th
  percentile of 2.12 GiB and a maximum of 6.00 GiB. In 96% of passes it was at most `x = 4 GiB`, so
  the default lane would have admitted the whole batch at once, room permitting (room not checked).
- **Heads.**
  - From the hold lines, head reserves had a median of 4 GiB, a 90th percentile of 10.9 GiB and a
    maximum of 25.3 GiB. 4 GiB was the most common head (97 of 461).
  - 29% of holds had a head whose budget (`R/2`) was already at least 4 GiB.
  - The static form of G (`R + x` above the empty-slice ceiling) would have closed the lane behind
    **no** head in the window.

### 7.2 Estimated

- The lane addresses between about 5 and about 100 of the 115.6 small-job wait-hours in 3.5 days.
  The store cannot narrow this: it records no ceiling room per instant, and no CPU ledger is logged
  anywhere (`slice_reserve` has no CPU-outstanding field).
- The two live instances (§1.2 and §1.3) are both inside the band: room for the waiters, a budget
  spent, a 4-5 GiB head.
- The cost side is the extra head delay of §3.3, about 1.3 minutes per head episode (estimated).

### 7.3 Measured after shipping (no new telemetry beyond the journal line)

- Re-run the script with `--since <cutover>` and compare:
  - the small-job share waiting at least 10 s, and the small wait-hours per day;
  - the large-job (head) waits, which are the cost;
  - the holds per day.
- Join the journal's `small-lane grant` lines with `wait_ms` to measure what the lane admitted
  directly.
- Report to the owner. This is not an automatic gate.

**Limits.** The store keeps 20 rows per exact signature, so frequent commands have lost rows: these
are shares of the retained sample. `budget` (the scope cap) stands in for the reserve, which is exact
for ordinary confine jobs and with pin-charge in observe.

## 8. What this does NOT fix

- **Ledger over-booking.** On this box the ledger books 2-5x what jobs use: at 13:22, about 45 GiB
  booked against `sliceAnon` of about 15-16 GB (journal); at 13:55, 41.4 GiB booked against about
  8.7 GiB in use (§1.3).
  - When the ledger is full, the lane admits nothing. While writing this plan, a 256 MiB job waited
    with "45847M of 45760M granted" (client wait line, about 13:47).
  - The levers are estimate accuracy: AIRA-318 (page-cache peaks inflate history, held), AIRA-347
    (cap headroom for auto-sized jobs, held) and AIRA-341 (the cold default after the p90 prior's
    deletion, done).
- **AIRA-351** (warm-starting a cold subject from the same repo's history). Separate, held,
  measure-first, and blocked by this ticket.
- **Waits on CPU,** on an exclusive drain or hold, or behind a pushed head (by design, C5).
- **Large jobs' own waits.** The lane can lengthen a head's wait by at most the §3.3 cost.

## 9. Risks

- **R1. Heads wait longer** by up to one lane footprint's drain time per episode (§3.3). Partly offset
  by fewer holds and backstop refills. Watched by §7.3.
- **R2. Silting.** Long-lived small jobs granted through the lane (dev servers) hold lane room for
  their lifetime. Bounded by `x` and harmless to heads, but small-job relief shrinks. Visible as lane
  bytes in `--list` and `top`. Mitigation, if it bites, is deferred (§10).
- **R3. The one-time extra lane after a restart** (case 9): a head may wait one extra lane's worth
  once per daemon restart.
- **R4. `y` is inclusive at 1 GiB, which is exactly the cold default.** Every unpinned first run
  (`fallback:no-history`, 1 GiB) is lane-eligible, so three 1 GiB cold jobs plus small ones fill the
  lane. This is intended (they are the incident's population) but worth naming.
- **R5. Operator surprise:** `held_by=head` for a small job beside a lane that is not full means
  guard G or C3 refused it. The wait-line wording says "held behind the head", which is true.
- **R6. Diagnosis drift:** `heldBy` describes the last pass. A grant between the probe and the render
  makes it stale for one line. That is the same property as `lastGrantable`.

## 10. Deferrals (explicit, not built here)

- Lane expiry, or a per-member age cap, against silting (R2): build only if §7.3 shows it.
- A CPU-outstanding field in `slice_reserve`. It would let §7 separate CPU waits; it is out of scope.
- Carrying `laneGranted` across a restart (it needs an ARDR frame change).
- A lane for ci-shim mode.
- A per-head lane or per-head small-job budget (rejected in §11; recorded so it is not re-proposed
  without new evidence).
- Telemetry columns in `confine_peak_history` (for example a lane-grant flag); the journal line covers
  the measurement.

## 11. Cut list (considered and cut)

| Option | Why cut |
|---|---|
| A second slice or ledger for small jobs (the owner's first idea) | AIRA-228 collapsed to one queue and ledger; the owner approved a rule in the one ledger instead. |
| Raise the AIRA-307 budget (e.g. `max(R/2, 4 GiB)`) | The budget is cumulative per head. 14 small jobs of about 7 GiB exhaust a 4 GiB budget and the 5-minute hold returns. The lane is concurrent. |
| Per-head lane counter | It is cumulative like the budget, and a stream of heads would each grant a fresh `x`. A queue-level derived sum bounds what any head sees. |
| Lane membership by size | Fails the incident (C4). |
| Lane sized in `ledgerCharge` | Resize, discount and re-anchor drift (C1). |
| A static guard `x + R <= ceiling` | Ignores per-job headroom and CPU; the dynamic G covers both in one predicate. |
| A separate CPU sub-cap for the lane | G's CPU arm bounds the only thing that matters (the head's slots) without a new knob. |
| Runtime-aware backfill (start a small job only if it ends before the head could fit) | Needs runtime predictions, a judgement AIRA does not make. |
| A lane-grant column in history | Telemetry-only; the journal line suffices (§7.3). |
| A "lane full" refusal code | The lane never refuses; a lane-full job falls through to today's rule and waits. |

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

Release notes name the new install options, the five reply fields and the journal line. Protocol 15
is unchanged.

## 13. Open questions for the plan gate

1. **C1, reserve rather than `ledgerCharge()`** for eligibility and lane size. The two are identical
   today (observe). Confirm, or keep the booking and accept the resize and re-anchor drift.
2. **C5, the lane closed behind a pushed head.** The alternative is a pushed head that still lets the
   lane pass, which weakens AIRA-309 invariant 3.
3. **C6, the raw daemon default off,** with the product default on via install (the pin-charge
   pattern). The alternative is the raw default on.
4. **Inclusive `y`** captures the 1 GiB cold default (R4).
5. **The one journal line per lane grant.** Keep it, or drop it and measure only from `wait_ms`
   before and after.
