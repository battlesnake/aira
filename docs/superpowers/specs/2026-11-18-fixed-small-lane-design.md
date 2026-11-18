# Fixed small-job lane (AIRA-350)

Status: plan, round 1, for plan review (Codex Sol at `xhigh`, Gemini) and the Fable plan gate.
Ticket AIRA-350, milestone v0.46. Correctness-critical (admission, host memory): two-loop, Opus
builds, Fable reviews.

**Owner decision, 2026-11-18, verbatim:** "let's go back to a fixed slice that is separate to the
main slice reservation. Its max size is always fixed. The main slice reduces its admission ceiling
based on available RAM as it currently already does." Earlier wording: "in addition to the main
slice, we have a fixed slice of x GB (4 or 8), which jobs requesting <= y GB (1 GB) can be admitted
to ... smaller jobs hit the separate small-job slice by default, unless it's full, and then they hit
the main slice. This should only be in default mode, not in CI mode."

**What this replaces.** The dynamic lane of `2026-11-18-small-job-lane-design.md` (four rounds, not
approved) let small jobs take room from the main ceiling, so no version of it could show that a big
head cannot starve. That spec's evidence (§1) is reused here and not restated. This lane books in its
own fixed pool, so it never takes main ledger room by construction, and it also works when the main
ledger is full of over-booked leases, which the dynamic lane could not.

Code references are to the worktree at `45f68afd` (master). Data: a store copy and a
`confine --list --json` snapshot taken 2026-11-18 21:12 and 21:20 EET, kept in
`~/tmp/aira-lane2-store/` and not committed (they name other sessions' jobs). The committed replay is
`docs/dev/aira-350-fixed-lane-replay.py` with its output `.out.txt`. M marks a measured figure, E an
estimate, R a figure reported by someone else and not re-measured here.

## 0. The greenfield minimum, and the gap

**The problem in one line.** Small jobs wait minutes while the main ledger is full (over-booked) or
frozen behind a head that does not fit.

**The bare minimum.** A second booking pool of fixed size `x`. A job-level waiter whose reserve is at
most `y` is granted in it, before the head rule, whenever the pool has room. That is a boolean on the
lease, a sum derived from the leases, and one branch in the evaluator.

**Why the minimum is not enough.** Each addition below closes a concrete failure of the minimum; none
is speculative.

| Addition | Failure of the bare minimum it closes | Size |
|---|---|---|
| Main headroom and main outstanding computed from main leases only (§2.1) | Each lane grant would add 64 MiB to the main headroom (`admitSliceHeadroom`, `admit.go:1041-1051`), so the lane would take main room. | two subtractions per main site |
| Lane counts each lease's cap (`reserve`) plus 64 MiB | Counting `ledgerCharge` lets a resize or a pin-charge discount put the cap above the booking, so lane real use could exceed `x` (§2.3). | unit choice |
| The main pool's static maximum is `memory.max - x` (§2.3) | Without it, bookings could reach `memory.max - headroom + x`, past the slice's kernel cap: AIRA-15's random-victim OOM (`admit.go:2509-2516`). | one helper at four read sites |
| A lane grant leaves the current head's physical RAM, CPU and VRAM need (§2.2) | CPU and VRAM are one shared ledger and real memory is one cgroup: a lane grant could take the slot or bytes a head waits for (§3, counterexamples). | three conjuncts |
| Suite parents (`--delegate-ram`) are not eligible | A 1 GiB parent brings exempt workers past the head: reported as 13 worker scopes totalling 28 GiB (R). | one frozen boolean |
| Closed behind a pushed head (default; §12 Q2) | AIRA-309's owner decision: nothing passes a pushed head except another push. | one conjunct |
| Restart classification and re-declare normalisation (§2.4) | An ARDR re-declare carries no pool, so `lane <= x` and the total would not survive a restart. | two small branches |
| Off in ci-shim mode; set at install (§2.5) | Owner decisions. | the pin-charge pattern |
| Minimal reply fields and one journal line (§2.6) | Without them "lane full" cannot be told from "held behind the head", and the replay's upper bound cannot be checked. | optional fields |

**Not built:** a second cgroup slice (§2.3), any counter that is incremented rather than derived,
eviction or migration between pools, a lane-internal head rule, a lane for ci-shim mode. §10 lists the
cuts.

## 1. Problem and evidence

- **The symptom** (owner): "a long queue of small reservation requests while at 37 GB RAM usage".
- **The incident** (R, from the brief and the prior spec §1.2): on 2026-11-18 at about 13:20 EET,
  32 running jobs booked 41.5-45.4 GiB against a ceiling of about 45 GiB, while their real peaks summed
  to about 22 GiB. 13-14 queued jobs of 0.07-1 GiB waited 4-6 minutes behind a head that did not fit.
- **The same shape now** (M, snapshot 21:20 EET): 43.95 GiB granted of a 44.56 GiB ceiling (effective
  maximum 48.25 GiB), 26 jobs (21 job-level, 5 aitest workers holding 14.00 GiB), 2 queued, freeze phase
  `hold`. Real use of the slice (current less reclaimable) is 11.03 GiB, a quarter of the booking.
- **Small jobs' share of waiting** (M, replay §0, 82.3 h from 2026-11-15 08:56 UTC): 9281 retained
  small rows (reserve <= 1 GiB) waited 128.2 hours in total; 2660 large rows waited 83.7 hours.
- **Which ceiling term binds** (M, journal 2026-11-15 to 2026-11-18): all 4733 `slice ceiling
  throttled` samples name `bound-by=system-pressure`. The static term never bound in the window.

## 2. Design

### 2.1 Data model: one queue, one ledger, two pools

The lane is a partition of the one existing queue and ledger (AIRA-228), not a second cgroup (§2.3
says why). The whole-slice fields keep their meaning, so every reader that is not changed stays
correct for emptiness and residual checks, and errs conservative for capacity.

- **`admitWaiter.lane bool`.** The lease is booked in the lane. Written under `queue.mu` only by the
  lane grant (§2.2), the restart establish (§2.4) and the re-declare normalisation (§2.4). Never set on a
  sub-reservation, an exclusive waiter or a suite parent.
- **`admitWaiter.suiteParent bool`.** Frozen at enqueue in the waiter literal (`admit.go:2418`) as
  `strings.HasPrefix(request.signature, runner.AitestParentSignaturePrefix)`. Only `--delegate-ram`
  launches carry that prefix (`internal/runner/confine_linux.go:692-694`) and only they publish the aitest
  coordinates that let a job spawn workers (`confine_linux.go:1233-1269`). It reads `request.signature`,
  not `waiter.signature`, which is a bounded copy documented as diagnostic-only (`admit.go:199-217`).
- **`sliceQueue.laneReserve`, `laneCharge int64`, `laneJobs int`.** A derived cache, exactly like
  `outstanding` (`admit.go:736-757`): the sums of `reserve` and `ledgerCharge()` and the count over
  granted, accounted, `lane` waiters. One method `queue.rederiveLocked()` writes these three and the four
  existing fields, and replaces the five `rederiveLedgerLocked` assignments (`admit.go:2330`, `2374`,
  `2709`, `3083`; `confine_resize.go:143`). No incremented counter exists.
- **Derived quantities**, all under `queue.mu`:
  - lane used `L = laneReserve + 64 MiB x laneJobs` (the per-job term is
    `admitSliceHeadroomSupervisorDefault`, `admit.go:68`);
  - main outstanding `B_m = outstanding - laneCharge`, main jobs `J_m = outstandingJobs - laneJobs`;
  - main static maximum `M_m = maximum - x` when the lane is on, else `maximum` (§2.3).
- **`Server.smallLaneCap` (`x`), `smallLaneJobMax` (`y`).** Read once in Serve; 0 means off (§2.5).

**Every reader of the whole-slice fields is classified** (a syntactic allow-list test pins it, §5):

| Site | Today reads | With the lane |
|---|---|---|
| `sliceProvablyEmpty` (`admit.go:586-588`) | `outstandingJobs` | unchanged: a lane job is a running job, so an exclusive drain waits for it |
| `residualJobs`/`residualBytes` (`admit.go:1284-1290`), snapshot sums (`admit.go:1398`, `1417`) | whole slice | unchanged |
| `ci_dump.go:196`, `205` | whole slice | unchanged (shim only; the lane is off there) |
| evaluator (`admit.go:2563`, `2622`, `2634`) | `maximum`, `outstandingJobs`, `outstanding` | `M_m`, `J_m`, `B_m` |
| `admitOutstandingJobs` (`admit.go:1053`) feeding `admitCeiling` (`admit.go:1490-1492`) and the too-large check in `admitConnection` (`admit.go:1891-1898`) | whole-slice jobs, `maximum` | `J_m`, `M_m` |
| enqueue too-large (`admit.go:2386`) | `outstandingJobs`, caller's `maximum` | `J_m`; callers pass `M_m` |
| worker probe and enqueue (`worker_admit.go:244-254`, `353`) | whole slice, `maximum` | `B_m`, `J_m`, `M_m` (workers book in main) |
| `confine_manage.go:220-272` (`CeilingBytes`, `Jobs`) | whole slice | `CeilingBytes` = main ceiling less main headroom, plus `x`; `GrantedBytes` and `Jobs` whole-slice; lane fields added (§2.6) |

### 2.2 The admission rule

In `evaluateAdmitQueue` (`admit.go:2472`), after the restart freeze (`admit.go:2592`) and the
exclusivity gate (`admit.go:2604`), and before today's fit (`admit.go:2663`):

```
// per pass:  laneOn := s.smallLaneCap > 0 && !s.shimMode()
//            effectiveMaximum is computed from M_m instead of maximum (admit.go:2561-2564)
jobs := J_m + 1; headroom := H(jobs); outstanding := B_m          // main-only inputs
available, physicalFree := today's expressions (admit.go:2635-2662) on these inputs
if laneOn && laneEligible(w) {
    if laneFits(w, head, physicalFree) {
        grant as admit.go:2700-2724, with w.lane = true set before the re-derive;
        no head.bypassed, no budgetWithheld, no anchor write; log the lane-grant line; continue
    }
    w.laneFull = !(L + w.reserve + 64 MiB <= x)                    // diagnosis only
}
... today's code from admit.go:2663 on, unchanged
```

**`laneEligible(w)`:** `!w.isSubReservation() && !w.exclusive && !w.suiteParent && w.reserve <= y`.

**`laneFits(w, head, physicalFree)`** is true only when all of these hold. `need`, `cpuH` and `vramH`
are the head's `reserve + 64 MiB`, `cpu` and `vram` when a head exists in this pass, else 0. "The head"
is today's per-pass head (`admit.go:2770-2779`), so a waiter evaluated before any head is found faces
no reservation; it is ahead of the head in evaluation order anyway.
1. **Not behind a pushed head:** `head == nil || head.pushSeq == 0` (default; §12 Q2).
2. **Lane room:** `L + w.reserve + 64 MiB <= x`.
3. **Physical room for the whole lane at its caps, plus the head:**
   `L + w.reserve + 64 MiB + need <= physicalFree`. `physicalFree` is today's physical-only term,
   `checkedAvailable(current, effectiveMaximum, reclaimable, 0, headroom)` (`admit.go:2659-2662`). It
   counts lane jobs' real use twice (in `current` and in `L`); that is the conservative direction, and it
   is what makes invariant 4 hold under later growth (§3).
4. **CPU, leaving the head's:** `w.cpu + cpuH <= cpuAvailable(cpuCeiling, cpuOutstanding)`.
5. **VRAM, leaving the head's:** `w.vram == 0 || vramWaiterFitsLocked(w.vram + vramH, vramOutstanding)`
   (`internal/daemon/vram.go:186-192`).

**A waiter the lane refuses** goes on to today's main path unchanged. There it may be granted, held by
the head rule (and arm the hold, `admit.go:2762-2769`), refused on capacity, or become the head. It is in
no pool while queued, so **every pass tries the lane first again**: a small waiter that overflowed is
granted in the lane on the first pass with lane room (§12 decided, T19).

**Placement.** The lane branch precedes the head `switch` (`admit.go:2687-2698`), so lane grants pass
an unpushed head's budget and hold, including in a backstop pass. Lane-first applies with no head too,
so small jobs keep out of the main pool's room (§10, "main first").

**Inside the lane:** evaluation order (pushed, then high, then FIFO; `admitOrderLocked`,
`admit.go:452-476`), first fit. A small job that does not fit the lane residue is not a lane head: later,
smaller waiters may take the residue, and it still has the main pool every pass. No lane-internal head
rule (§10).

### 2.3 Host protection

**The trade, stated once.** Three properties are wanted: a booking guarantee as strong as today's,
no main capacity lost while the lane is idle, and no lane-to-main coupling (the property that made the
dynamic lane unsound). If the main ceiling is today's and the lane adds `x`, the total booking exceeds
today's ceiling by up to `x`. Bounding the total by today's ceiling then requires main to shrink by `x`
always (idle cost) or by the lane's current use (coupling). So any two of the three can be had, not all
three.

**This spec takes "additive" for the pressure term and "partition" for the static cap:**

| Term | Rule | Effect |
|---|---|---|
| Slice `memory.max` (kernel cap; 64 GiB here, M) | Main uses `M_m = memory.max - x` in place of `memory.max` at every main site (§2.1). | Bookings of both pools stay `<= memory.max - headroom`: AIRA-15's invariant holds exactly. |
| Pressure term (`sliceCeilingDesired`, `sliceceiling.go:241-247`) | Unchanged, as the owner decided. The lane's `x` is added on top. | Bookings may exceed today's pressure ceiling by up to `x`. |
| Physical floor (`checkedAvailable`, `admit.go:2906-2922`) | Unchanged for main; lane grants need rule 3 of §2.2. | At every grant of either pool, the slice's real use plus the new cap fits under the main effective maximum (today's, while pressure binds) less headroom. |

**Why the static rule costs nothing here.** It binds only while the published ceiling is above
`memory.max - x` (60 GiB at `x = 4 GiB`). The published ceiling was pressure-bound in all 4733 samples,
between 38 and 52.5 GiB with a median of 48.75 GiB (M, journal). The machine term `MemTotal - 16 GiB`
is 62.55 GiB here (M: `MemTotal` 84338868224 bytes); with the rule, a static-bound total can exceed it
by `memory.max - machine term` = 1.45 GiB, against `x` without it.

**The residual exposure, derived.** Take a ceiling sample `C <= MemAvailable + sliceAnon - freeMin`
(`sliceceiling.go:241-247`; the max-over-window damping can make `C` larger than the newest sample, as
today). After the last main grant, `B_m <= C - (2 GiB + 64 MiB x J_m)`. Suppose every job then grows to
its cap, each supervisor uses its 64 MiB, lane jobs use their whole footprint `L <= x`, and nothing
outside the slice changes. Then `sliceAnon` rises to at most `B_m + 64 MiB x J_m + L`, and
`MemAvailable >= freeMin + 2 GiB - x`.

| `x` | Worst-case MemAvailable floor (E, derived) | Against the 8 GiB watchdog trip (`watchdog.go:24`) |
|---|---|---|
| 0 (today) | 10 GiB | 2 GiB above |
| 4 GiB | 6 GiB | 2 GiB inside |
| 8 GiB | 2 GiB | 6 GiB inside; near the kernel's own reclaim and OOM territory |

The watchdog acts only on uncapped, agent-attributable processes of at least 2 GiB RSS
(`watchdog.go:351-370`). Inside its band, it could kill such a process outside the slice that is not
the cause. **No rule on current real use gives a hard bound on this growth**, because jobs grow after
they are admitted. What is realistic:
- the slice's real use is about a quarter of its booking (M, snapshot: 11.03 of 43.95 GiB);
- the lane's real use, as the sum of its residents' peak RSS held for each resident's whole stay (an
  upper bound), has a p99 of 2.11 GiB and a peak of 3.07 GiB at `x = 4`, and 2.61 and 4.71 GiB at
  `x = 8` (M, replay §1);
- small jobs' peak-to-reserve ratio has a median of 0.17 and a p90 of 0.83 (M, replay §0).

**The alternative (§12 Q1): partition on the pressure term too.** Main uses
`min(today's effective maximum, M_m) - x`, so total bookings never exceed today's ceiling and the
worst-case floor stays at 10 GiB. The cost is `x` of main booking room whenever the pressure term binds,
which was always in the window: about 8% of the 48.75 GiB median effective maximum at `x = 4`, 16% at
8. It is one expression different from the recommended form, so the owner's answer changes nothing else
here.

**A ledger partition, not a second cgroup slice.** A real `aira-lane.slice` (or a nested sub-slice)
would give a kernel cap on lane real use, and with a sibling slice the pressure term would see lane use
as memory outside the main slice. But:
- the confine client creates its scope under the slice it was admitted to, so the grant frame, the
  client, `--list`, `--kill`, the reaper, `--resize` and the worker paths would all have to learn a second
  slice;
- the CPU and VRAM ledgers are per queue on the one-slice assertion (`admit.go:742-757`); two slices
  would need a machine-wide CPU ledger first;
- install would create and supervise a second unit.

The per-scope `memory.max = reserve` already caps each lane job, so the lane's real use is bounded by
`L <= x` without a lane cgroup. The one gain, the pressure term reacting to lane real use, is the
coupling that was decided against.

### 2.4 Interactions

| Interaction | Decision | Why / where |
|---|---|---|
| Pushed head (`--front`) | Lane closed behind it (rule 1). A **pushed small waiter** is evaluated first and takes the lane at once if it fits. A pushed small head that the lane and main both refuse waits; it is itself tried in the lane every pass, so it cannot wedge. | AIRA-309 owner decision; §12 Q2 asks whether to open it. |
| `--exclusive` drain or hold | Unchanged: the gate `continue`s before the lane branch (`admit.go:2604-2621`). Exclusive waiters are not eligible. A drain waits for lane leases too (`sliceProvablyEmpty` counts them). | T10 |
| `--priority high` | Order only. A high head is an unpushed head: the lane stays open and reserves its needs. | `admit.go:302-311`; T9 |
| Fairness freeze and AIRA-59 backstop | Lane grants never arm, never count in `bypassed`, never set `budgetWithheld`. In a backstop pass an eligible waiter still tries the lane first. A waiter the lane refuses arms exactly as today. | T11, T12 |
| AIRA-324 discount | Eligibility (`reserve <= y`) and lane use are in **cap bytes** (`reserve`), the owner's "requesting" and the physical bound. Main counts `ledgerCharge` as today. In observe mode, which `aira install` renders (`install.go:62`), the two are equal (`pin_charge.go:120-125`). | T15 |
| aitest | Workers and `confine-reserve` holds are sub-reservations: never eligible, booked in main, exempt from the head as today (`admit.go:2675-2688`). Suite parents are not eligible (`suiteParent`). | T14 |
| Daemon restart, ARDR establish (`admit.go:2339-2378`) | The frame has no pool or signature (`internal/redeclare/redeclare.go:74-79`). An absent lease is established in the lane iff the lane is on, `parentScopeID == ""`, `reserve <= y` and `L + reserve + 64 MiB <= x`; otherwise in main, which may go negative as today (the restart design's Invariant 6, `admit.go:2339-2350`). The total booking is the pre-restart total; only the split can shift (CE5). "Always main" was considered: it would move every lane server into main and let the lane refill, up to `2x` over the main ceiling until they end. | T17 |
| Re-anchoring SET (`admit.go:2318`) | It may rewrite `reserve`. If a lane lease then has `reserve > y` or makes `L > x`, `lane` is cleared and it books in main. Normally the keeper re-declares the granted reserve, so this does not fire. | T18 |
| `confine --resize` | Lowers `charge` only (`confine_resize.go:139`). The lane counts `reserve`, and main excludes lane leases, so resizing a lane lease frees nothing in either pool; `--list` shows the lane unchanged. Allowed, and documented. | T16 |
| Long-lived lane jobs (servers) | First come, no eviction, no migration. A server admitted to the lane holds its footprint for its life. Measured silting: 5 small job-level scopes about 128 h old, 1.56 GiB of footprint (M, snapshot). §6 replays its effect. | §6 |
| Overflowed waiter, lane room later | Takes the lane on the next pass (§2.2). A **granted** main lease never moves to the lane. | T19 |
| Lane ordering | First fit in evaluation order; one small job can be passed in the lane by smaller ones, but it keeps the main path. | T20 |
| Restart freeze, fail-fast latch | Unchanged and earlier (`admit.go:2494-2505`, `2592-2595`). The latch is shim-only. | — |
| ci-shim mode | Off: Serve forces `x = 0` and `laneOn` checks `!s.shimMode()` (`shim.go:37`). | T23 |
| Too-large refusal, scope-cap clamp | Against `M_m`: a job above `memory.max - x - headroom` is refused at once rather than queued for ever. | T22 |

### 2.5 Configuration (the pin-charge pattern)

**Daemon** (`internal/daemon/small_lane.go`, beside `pinChargeConfigFromEnv`, `pin_charge.go:53-90`):
- `AIRA_DAEMON_SMALL_LANE`: `x`. Unset, empty or `off` is off (the raw default). Otherwise a size
  parsed by `runner.ParseMemorySize` (`internal/runner/memory_size.go:82`), from `y + 64M` to `8G`.
  Anything else: `E_CONFIG_INVALID: AIRA_DAEMON_SMALL_LANE must be off or a size from <y+64M> to 8G, such as 4G`.
- `AIRA_DAEMON_SMALL_LANE_JOB_MAX`: `y`, read only when the lane is on (the `pin_charge.go:69-73`
  ordering), default 1G, from 1M (`runner.MinPinnedScopeCap`) to `x - 64M`. Anything else is
  `E_CONFIG_INVALID`.
- The 8 GiB bound keeps the worst-case floor of §2.3 at or above the 2 GiB base headroom.
- Serve reads both beside the pin-charge read (`server.go:416-429`). Shim mode forces the lane off. When
  on, it logs `aira daemon: small-job lane 4G for jobs reserving at most 1G (main pool static maximum is
  memory.max less 4G)`. No new error code.

**Install** copies each pin-charge site:
- flags `--small-lane SIZE|off` and `--small-lane-job-max SIZE` (`install.go:154-158`, parse beside
  `install.go:471-492`, refused with `E_INSTALL_ARGUMENT_INVALID`);
- an option not given keeps the installed unit's value (`install.go:640-650`);
- ship defaults `4G` and `1G` (`install.go:695-706`), so the product default is on;
- re-exec forwarding (`install.go:890-895`), `renderDaemonUnit` re-validation (`install.go:1632`), two
  placeholders and two lines in `internal/install/assets/aira-daemon.service.in` (beside lines 12-13);
- `--status` lines (`install.go:2507-2508`); the shim environment adds `AIRA_DAEMON_SMALL_LANE=off`
  (`internal/install/mode.go:321`); the usage string (`internal/core/core.go:2241`);
- install also refuses an `x` that leaves the main pool's static maximum below
  `sliceCeilingUsableFloor()` (`sliceceiling.go:299-301`) for the rendered `MemoryMax`.

### 2.6 Observability (minimal)

- **`confine --list` reply** (`runner.ConfineSliceReserve`, `internal/runner/confine_manage.go:586`),
  filled in `admitSliceSnapshotFor` (`admit.go:1302`), present only while the lane is on:
  `lane_capacity_bytes` (`x`), `lane_used_bytes` (`L`), `lane_jobs`, `lane_job_max_bytes` (`y`).
  `granted_bytes`, `jobs` and the scope and reservation splits stay whole-slice, so the AIRA-68 totals
  and the residual checks keep their identities. While the lane is on, `ceiling_bytes` is the slice's
  whole booking capacity, the main pool's ceiling less main headroom plus `x`, so `granted_bytes`
  still compares with it; the other `ceiling_*` fields keep describing the pressure ceiling.
- **Per scope row:** `lane: true` (omitempty) on a lane lease, so `--list` shows which pool a job holds.
- **For the named queued scope** (the AIRA-24 probe, `admit.go:1352-1364`): `lane_full` and
  `held_by_head` (omitempty booleans), from the diagnosis-only fields `laneFull` (rule 2 failed) and
  `heldByHead` (the waiter fitted RAM, CPU and VRAM and the head `switch`, `admit.go:2689-2697`, withheld
  it), written in the refusal branch beside `noteGrantableLocked` (`admit.go:2760`) and overwritten
  every pass. No decision reads them (the `lastGrantable` discipline, `admit.go:261-280`).
- **Text.** `--list` prints `small-job lane: 3.0G of 4G used, 3 jobs (jobs reserving at most 1G)` after
  `slice queue:` (`cmd/aira/main.go:3777`). The wait line (`confineQueueNote`,
  `internal/runner/confine_queue_position_linux.go:151`) appends `, small-job lane full` and/or
  `, fits but held behind the head of the queue`.
- **Journal:** one line per lane grant, bounded by arrivals (about 110 small jobs an hour, M):
  `aira daemon: small-lane grant seq=<n> reserve=<bytes> waited=<ms> lane=<used>/<cap> jobs=<n>`. It is
  what measures the realised yield against the replay's upper bound (§6).

### 2.7 Generated text (faces)

On the AIRA-340 pattern (`internal/core/skill.go:305-318`): `ConfineSmallLaneGuidance` (long) and
`ConfineSmallLaneSummary` (short) in `skill.go`. The long form extends the sentence "Without a push, a
waiting job that does not fit lets smaller jobs past it ..." in the skill (`skill.go:338`) and is the
install options' help. The short form is appended to the `confine` verb summary (`core.go:2607`) and the
MCP instructions (`cmd/aira/mcp.go:29`). `TestConfineSmallLaneGuidanceIsOnEveryFace` pins the
keywords, modelled on `cmd/aira/confine_priority_test.go:100`.

- **Long (draft):** "Small-job lane (default on, not in CI mode): a job reserving at most 1G (`aira
  install --small-lane-job-max`) is admitted first from a separate fixed 4G pool (`--small-lane`), without
  waiting behind larger queued jobs; when that pool is full it queues in the main pool as before. It still
  needs a free CPU slot and real free memory, and it never takes what the job at the head of the queue is
  waiting for. Keep small jobs small: do not pin a short command's `--memory-reserve` above 1G, or it
  loses the lane. Long-running servers in the lane hold their share of it for their whole life, so give a
  server an honest small reserve and stop it when done. `--delegate-ram` suites and their workers never
  use the lane."
- **Short (draft):** "Jobs reserving at most 1G are admitted from a separate small-job lane first, so
  keep short commands' reserves small."

## 3. Invariants

1. **`L <= x` always.** `L` is derived from the lease set. Grants check rule 2; the establish checks it;
   the SET normalisation clears `lane` when it would break; releases only lower it.
2. **The main ledger term reads no lane state.** `B_m` and `J_m` exclude lane leases, and `M_m` is a
   constant. A lane grant, release or resize changes none of them.
3. **Kernel cap.** At every grant, `B_m + L <= memory.max - headroom(J_m)` for undiscounted main leases,
   as today's `Σ <= cap - headroom` (`admit.go:2509-2516`).
4. **Head needs.** At the instant of every lane grant past a head `H`: `H`'s CPU slots and VRAM remain
   free, and `H`'s pin plus 64 MiB still fits the physical room even with every lane lease grown to its
   cap. Lane grants never change `H.bypassed`, `budgetWithheld` or `freezeArmedAt`.
5. **AIRA-307's per-head bound holds unchanged:** main job-level grants past an unpushed head, outside
   backstop passes, total at most `R/2` (`admitBypassBudget`, `admit.go:485-490`).
6. **No lane grant behind a pushed head** (default; §12 Q2).
7. **Off is today.** `x = 0` (raw default and ci-shim) makes `laneOn` false, `laneJobs` zero and
   `M_m = maximum`; every existing test passes unchanged.
8. **Precedence:** restart freeze, fail-fast latch, exclusivity gate, then the lane, then the head rule.

**Proof of 4.** Let the lane grant `G` be made in a pass with head `H` and physical reading `current`.
Rules 4 and 5 leave `cpuH` and `vramH` free after `G`. Rule 3 gives
`L' + need <= checkedAvailable(current, E, reclaimable, 0, h)`, where `L'` includes `G`. Later, the lane's
real use can rise by at most `L'`, since each lane job is capped at its reserve and each supervisor is
covered by its 64 MiB. So `H`'s physical room falls below `need` only if main's own real use grows. That
is today's case, not the lane's. ∎

**What 2 and 4 give, and what they do not.** No lane lease enters `H`'s main ledger term. No lane grant
takes a shared resource `H` was short of, or pushes it short. **They do not say `H` waits no longer than
today:** that is false. The cases, each pinned by a test so a build cannot hide them:
- **CE1, the untouched budget** (T13). Small jobs that spent `H`'s budget today now go to the lane. A
  mid-size job (`y < reserve <= R/2`) can then pass `H` within the budget. AIRA-307's bound still holds;
  only who uses it changes.
- **CE2, the duty cycle.** Lane grants do not arm, so holds arm at other instants and backstop passes
  fall elsewhere. Every main grant past `H` is still one AIRA-307 permits; the trace can differ from
  today either way.
- **CE3, slots held from before.** Lane jobs granted before `H` became the head hold CPU slots. `H`
  waits for the first slot to free; from then on rule 4 reserves it.
- **CE4, main's own growth** after a lane grant can shrink `H`'s physical room. This is today's case.
- **CE5, restart.** If re-declares arrive in an order that puts a former main lease in the lane, a former
  lane lease books in main, which can stand above its ceiling until it ends.
- **CE6, suite parents not recognised.** A `--delegate-ram` parent with an empty signature (an argv
  that cannot be resolved) is eligible, and its exempt workers then pass `H`. Its frequency is not
  measurable from the store.

**Compared with the withdrawn design.** Its Round-4 witness (a 1 GiB never-ending small job keeps a
4 GiB head out for good) does not hold here: T4 grants the head on the first pass after the transient
release.

## 4. Wire and protocol

- **ProtocolVersion stays 15** (`internal/daemon/protocol.go:218`). The admit request (the signature is
  already carried), the grant frame and the ARDR frame are unchanged.
- **The `confine-list` reply gains optional fields** (§2.6). The client decodes with `json.Unmarshal`
  (`confine_queue_position_linux.go:388`), so an old client ignores them, and a new client against an
  old daemon renders nothing.
- **`ceiling_bytes` grows by `x`** while the lane is on (§2.6). The client renders it and decides
  nothing on it.
- **The behaviour is daemon-side** and takes effect when the unit carries the new `Environment=` lines
  and the daemon restarts (§11).

## 5. Tests (each must fail against the named wrong implementation)

**Common settings** on the AIRA-228/307 seam: `queueOf` (`admit_queue_order_test.go:41`), a fake
memory reader and clock, `evaluateAdmitQueue` driven directly.
- **Headroom** is production: 2 GiB plus 64 MiB a job. `admitTestServer` zeroes both, so the helper must
  restore them.
- **CPU** is 16 cores at 2 slots a core (32), through the core-counter seam. Held leases carry `cpu = 1`,
  as production does; `heldLease` (`admit_queue_order_test.go:37`) has 0 and must not be used unchanged.
- **Lane:** `x = 4096 MiB`, `y = 1024 MiB`.
- **Memory reader:** `memory.max` 64 GiB, real use (current less reclaimable) 12 GiB unless stated, and
  a published ceiling giving the stated effective maximum.
- **Freeze:** `maxHold` 5 minutes and `freezeArmedAt = now - 60 s` (a hold pass) unless stated.
- **Head:** budget preset as spent (`bypassed = admitBypassBudget(head)`) unless stated.

Every test asserts the exact granted set and, per granted waiter, `lane`.

| Test | Setup | Assertions | Wrong implementation it fails |
|---|---|---|---|
| **T1 the incident** | Effective 49152 MiB; 20 main leases summing 42278 MiB, so main room is `49152 - (2048 + 21x64) - 42278 = 3482`. A 4096 MiB head. 14 small waiters (MiB): 72, 256, 512, 1024, 512, 256, 1024, 72, 512, 1024, 256, 512, 72, 1024. | Lane grants waiters 1-6, 8, 9 and 11; `L = 4048`. Waiters 7, 10, 12, 13 and 14 are withheld with `laneFull` and `heldByHead`, and their `lastGrantable` is 3482. `head.bypassed` is unchanged. | Lane off or deleted: none granted. Footprint without the 64 MiB: waiters 1-8, 11 and 13. Lane jobs in main headroom: `lastGrantable` 2906. Lane grants counted in `bypassed`. |
| **T2 main ledger full** | 22 main leases summing 46000 MiB, so main room is -368 MiB. Four 1024 MiB small waiters, no other waiter. | Three lane grants. The fourth has `laneFull`, is refused on capacity and becomes the head. | Lane judged on main's `available`: none. |
| **T3 13:55, CPU-bound** | Effective 48640 MiB; 28 main leases summing 42443 MiB (room 2293, 28 of 32 slots). A 5120 MiB head. Waiters (MiB): 2154, 2700, 1024, 256, 512, 512, 512, 1024. | The lane grants 1024, 256 and the first 512 (31 of 32 slots). The other three are held by the head, without `laneFull`; 2154 is held by the head; 2700 is refused on capacity. | No head CPU reservation (rule 4): a fourth grant. |
| **T4 the withdrawn design's witness** | Effective 49152 MiB. A 42880 MiB lease that never ends; a 1024 MiB lease released between t=0 and t=10. A 4096 MiB head. A never-ending 1024 MiB small waiter. | t=0: small waiter in the lane; head refused (main room 3008). t=10: head granted (main room 4096). | Lane booking counted in main: head refused at t=10. Lane job in main headroom: room 4032, refused. |
| **T5 physical gate** | No head. Real use leaves `physicalFree` at 2500 MiB. Three 1024 MiB small waiters. | Two lane grants; the third is granted in main. | Lane without rule 3: three lane grants. |
| **T6 head's physical need** | An 8192 MiB head refused on the main ledger (room 4096 MiB); `physicalFree` 9216 MiB; one 1024 MiB small waiter. | Not granted in the lane (`1088 + 8256 > 9216`); held by the head. | Rule 3 without `need`: granted. |
| **T7 VRAM** | A head needing 4 GiB of VRAM, refused on VRAM; 5 GiB free. A small waiter needing 2 GiB of VRAM, and one needing none. | The VRAM waiter is not granted in the lane; the other is. | Rule 5 without `vramH`: both granted. |
| **T8 pushed head** | A pushed head refused; a small waiter behind it with lane room. Separately, a pushed small waiter with lane room. | The first is not granted (`heldByHead`). The pushed small waiter is granted in the lane. | Lane branch before rule 1: the first is granted. |
| **T9 high head** | A `--priority high` head, unpushed and refused; small waiters behind it. | Granted in the lane, with its CPU reserved. | High treated as pushed: none. |
| **T10 exclusivity** | (a) An exclusive holder granted; a small waiter. (b) An exclusive drain queued while one lane lease runs. | (a) Gate-blocked, not granted. (b) The drain is not granted until the lane lease is released, then it is. | (a) Lane before the gate. (b) `sliceProvablyEmpty` on main jobs only: the drain is granted beside the lane job. |
| **T11 backstop pass** | `freezeArmedAt = now - maxHold`. An eligible small waiter with lane room. | Granted in the lane; `head.bypassed` unchanged. | Lane after the head `switch`: granted in main, `bypassed` up. |
| **T12 no arm** | Idle phase, head budget spent. Pass 1: two small waiters with lane room. Pass 2: lane full; a small waiter that fits main. | Pass 1: both in the lane, `freezeArmedAt` zero. Pass 2: withheld, and the hold arms. | An anchor write on a lane grant: armed in pass 1. |
| **T13 CE1, pinned** | An 8192 MiB head with `bypassed = 0`; main room 8000 MiB, so the head is refused. Four 1024 MiB small waiters, then a 3072 MiB long-lived waiter. | Lane on: three small in the lane, the fourth via the budget, and the 3072 MiB job granted via the budget (`bypassed` 4096). Lane off: four small via the budget; the 3072 MiB job withheld. | A pinned residual. A build that claims to remove it changes this test knowingly. |
| **T14 aitest** | (a) A 512 MiB sub-reservation with lane room. (b) An admit through `enqueueAdmitInternal` with `signature = AitestParentSignaturePrefix + "x"`, 1024 MiB, behind a held head. (c) The same with a plain signature. | (a) Granted in main on fit, `lane` false. (b) `suiteParent` true; held by the head. (c) Granted in the lane. | (a) Eligibility ignoring `parentScopeID`. (b) Eligibility ignoring `suiteParent`. |
| **T15 AIRA-324 units** | Enforce mode. (a) A 1228 MiB pin booked at 983 MiB. (b) A discountable 1024 MiB pin booked at 820 MiB. | (a) Not eligible: held by the head. (b) In the lane, `L` = 1088. | (a) Eligibility on `ledgerCharge`. (b) Lane counting `ledgerCharge`: `L` = 884. |
| **T16 resize** | A 1024 MiB lane lease resized to 256 MiB. | `L` unchanged; main room unchanged; whole-slice `granted_bytes` down 768 MiB. | Lane counting `ledgerCharge`. `B_m` including lane charges. |
| **T17 restart establish** | (a) Empty queue; re-declare 512 MiB, no parent. (b) `L` = 3800 MiB, then the same re-declare. (c) A re-declare with a parent. | (a) Lane. (b) Main, so `L` stays 3800. (c) Main. | Always main: (a) fails. Ignoring lane room: (b) gives `L` = 4376. |
| **T18 SET normalisation** | A 512 MiB lane lease re-anchored with reserve 2048 MiB. | `lane` false; the lease books in main. | Flag kept: a 2 GiB lease in the lane. |
| **T19 overflow retry** | Pass 1: lane at 4048 MiB; a 1024 MiB small waiter held by the head. Then a lane lease is released. | Pass 2: granted in the lane. | A pool fixed at the first evaluation (for example a decision that reads `laneFull`): held. |
| **T20 first fit** | Lane residue 600 MiB; waiters 1024 then 256 MiB. | 256 in the lane; 1024 goes to the main path. | A lane head-of-line rule: 256 not in the lane. |
| **T21 lane first** | No head; main room ample; one small waiter. | Granted with `lane` true; `B_m` unchanged. | Main first: granted in main. |
| **T22 static cap** | Unthrottled (effective = `memory.max` = 64 GiB). A 58 GiB waiter. Then main filled to its ceiling and the lane to `x`. | The 58 GiB waiter is refused too large (`58 > 64 - 4 - 2.0625`). Total bookings `<= memory.max - headroom`. | No `M_m`: the 58 GiB waiter is admitted and the total exceeds the kernel cap. |
| **T23 ci-shim** | A shim server with the lane fields set directly. | No lane grant. | Without `!shimMode()`. |
| **T24 off is today** | `x = 0`. A table of memory, ledger and CPU inputs, degenerate ones included. | `available`, `physicalFree` and the granted set equal today's formula's, and the existing suite passes unchanged. | Any formula change that leaks when the lane is off. |
| **T25 derived and bounded** | Random sequences of grant, release, establish, SET and resize. | After each step the lane sums equal a fresh walk, and `L <= x`. | An incremented counter missed on one path. |
| **T26 guards** | Syntactic allow-lists, on the `admit_pin_charge_guard_test.go` pattern. | Every selector of `outstanding`, `outstandingJobs`, `laneReserve`, `laneCharge` and `laneJobs` is classified (§2.1). No decision reads `laneFull`, `heldByHead` or `signature`. | A new unclassified reader, or a decision on a diagnostic field. |

**Configuration and install.** The environment table: `off`, `4G`; refused `0`, `9G`, `abc`, and `x <
y + 64M`; `y` ignored while the lane is off. Shim forces it off. The startup line. The install flags, a
kept installed value, the ship defaults, re-exec forwarding, render re-validation, `--status`, the shim
environment, and the refusal of an `x` too large for `MemoryMax`.

**Observability and faces.** The reply fields are present only with the lane on. The `lane` row flag;
`lane_full` and `held_by_head` only for the named queued scope. The `--list` line, both wait-line
clauses and their absence, and the journal line. `TestConfineSmallLaneGuidanceIsOnEveryFace`.

**Mutations, by direction.** The gate runs each one and records it RED, with a committed
`docs/dev/aira-350-mutation-check.py` on the `docs/dev/aira-324-mutation-check.py` pattern.
- **False pass** (grants what must wait): footprint without the 64 MiB (T1); rule 3 deleted or without
  `need` (T5, T6); rule 4 without `cpuH` (T3); rule 5 without `vramH` (T7); the lane before rule 1 (T8) or
  before the gate (T10a); eligibility ignoring `parentScopeID` (T14a) or `suiteParent` (T14b), or on
  `ledgerCharge` (T15a); lane counting `ledgerCharge` (T15b, T16); no `M_m` (T22); on in shim (T23); the
  establish ignoring lane room (T17b); the SET keeping the flag (T18); emptiness on main jobs only (T10b).
- **False fail** (holds what should be granted): the lane deleted (T1, T2); lane jobs in main headroom
  (T1, T4); lane booking in main (T4, T16); the lane judged on main's `available` (T2); high treated as
  pushed (T9); an anchor write on a lane grant (T12); always-main establish (T17a); a pool fixed at first
  evaluation (T19); a lane head-of-line rule (T20); main first (T21).
- **Moved:** the lane after the head `switch` (T11); lane grants counted in `bypassed` (T1). **Pinned
  residual, not a mutation:** T13 (CE1).

## 6. Expected yield (read-only replay)

`docs/dev/aira-350-fixed-lane-replay.py` against the store copy. It replays each retained confine job:
arrival = `started_at_ms - wait_ms`, hold = `wall_ms`, footprint = `budget + 64 MiB`. Eligible jobs (budget
`<= 1 GiB`, suite parents excluded) try the lane first, at every instant from arrival to their historical
grant, first fit. A job not admitted by then runs in main as it did. All figures are M from retained rows
and are **upper bounds**: the model has no CPU gate, no physical gate and no head reservation.

| `x` | Small wait-hours removed (share of all 128.2) | Lane full for a 1 GiB job | Eligible jobs overflowing to main (of which waited >= 10 s) | Lane occupancy mean / p90 / peak | Lane real use p99 / peak |
|---|---|---|---|---|---|
| 2 GiB | 77.6 h (60.5%) | 47.8% of the time | 1960 (658) | 0.68 / 1.62 / 2.00 GiB | 1.32 / 1.73 GiB |
| **4 GiB** | **101.9 h (79.5%)** | **9.7%** | **391 (107)** | **1.12 / 2.87 / 4.00 GiB** | **2.11 / 3.07 GiB** |
| 6 GiB | 107.6 h (83.9%) | 1.2% | 47 (6) | 1.22 / 3.19 / 6.00 GiB | 2.54 / 4.31 GiB |
| **8 GiB** | **107.9 h (84.1%)** | **0.1%** | **5 (0)** | **1.23 / 3.19 / 7.83 GiB** | **2.61 / 4.71 GiB** |

- **Silting** (replay §1b), with the snapshot's 1.56 GiB of five ~128 h servers held throughout: 68.6% at
  `x = 4` (lane full 30.6% of the time), 84.0% at `x = 8` (0.6%). In the replay itself, jobs running 10
  minutes or more hold 44% of lane occupancy, and jobs of an hour or more hold 9%.
- **Per day** (replay §1d): lane full 17.6%, 9.9%, 5.6% and 8.3% of the time at `x = 4`, and 0.0-0.2%
  at `x = 8`. The share does not climb late in the window.
- **The incident window** (replay §2, 13:09-13:30 EET): 16 eligible jobs waited at least 10 s, 71.0
  minutes in total. The replay admits all 16, leaving 5.8 minutes at `x = 4` and none at 8. **That
  cannot be claimed for the incident**, which reportedly had 32 jobs on 32 slots (R): rule 4 would have
  admitted nothing while no slot was free.
- **The one live instant** (M, snapshot 21:20): small job-level scopes hold a 6.90 GiB footprint (12
  scopes, 1.96 GiB real). That is 1.75 GiB of servers plus about 4.1-5.2 GiB of transient jobs, depending
  on whether a 1 GiB pytest job is a suite parent. Lane-first would have filled `x = 4` and left about
  1 GiB at `x = 8`. The replay's mean of 1.1 GiB is low by construction.

**Limits of the data.** 186 signatures sit at the 20-row cap (`internal/store/confine_peak_history.go:13`)
with their oldest retained row inside the window, so frequent commands lost rows. 3188 pytest-worker
rows and 2 confine rows have no timing. Jobs still running at the copy have no row. CPU is the dominant
unmodelled gate: retained rows show at most 22 concurrent jobs against 26-32 in the live states, so it
cannot be replayed honestly. Booking is the scope cap, the reserve for an ordinary confine job; pin-charge
is in observe mode.

**Recommendation: `x = 4 GiB`, `y = 1 GiB`.**
- Even with silting, `x = 4` removes most of what any `x` could (upper bound 69-80% of small wait-hours,
  against 84% at 8).
- It keeps the worst-case floor of §2.3 at 6 GiB rather than 2 GiB.
- The snapshot says 4 is tight at busy instants. 8 buys 5-15 points of upper-bound yield for 4 GiB more
  worst-case exposure. That is the owner's call (§12 Q3), and with the partition form (Q1) 8 is safe.
- The lane-grant journal line measures the realised yield; rerun this replay with its window ends set
  explicitly, and compare.

**The idle cost** is booking room, not real RAM: an idle lane holds no memory.
- Recommended form: the main pool loses nothing while the pressure term binds (always, in the window).
  Under the static cap it loses up to `x` of booking room, only while the published ceiling is above
  `memory.max - x`.
- Partition form (Q1): `x` of main booking room whenever the pressure term binds. At the measured
  quarter of real use per booked byte (§1), that booking room stands for about 1 GiB of real use at
  `x = 4` and 2 GiB at `x = 8` (E).

## 7. What this does not fix

- **Ledger over-booking**, the larger cause. Leases book 2-5x what they use (R; M, a quarter at 21:20).
  The levers are estimate accuracy: AIRA-318 and AIRA-347 (both held), and the
  `AIRA_AITEST_WORKER_OVERHEAD_BYTES` floors fastest-ee sets (R). The lane gives small jobs a fixed share
  of booking room; it does not make bookings smaller.
- **CPU slot saturation.** Each confine job takes at least one of 32 slots. The lane never bypasses the
  CPU fit, and it reserves the head's slot.
- **Not reopened:** AIRA-351, warm-starting cold subjects, measured NO-GO
  (`docs/dev/aira-351-cold-start-decision-rule.md:56`, `docs/dev/aira-351-cold-start-measure.out.txt`);
  AIRA-352 and AIRA-353, held (`docs/superpowers/specs/2026-11-18-admission-diagnostics-and-backstop-design.md`).
- **Mid-size jobs** (`y` < reserve < the head) and **large heads' own waits.** They are unchanged in
  rules, and their traces can move either way (CE1, CE2).
- **Waits behind a pushed head** (default closed, Q2) or an exclusive drain.

## 8. Risks

- **R1. Host exposure** of up to `x` in the pressure regime (§2.3). Watched through `lane_used_bytes`
  and the journal; Q1 removes it.
- **R2. Silting.** Long-lived small jobs can fill the lane. Measured at 1.56 GiB now (§6). The guidance
  asks for honest reserves on servers.
- **R3. Missed readers.** A whole-slice field read as main, or the reverse. Closed by T26's allow-list,
  and the failure direction of an unconverted reader is conservative (§2.1).
- **R4. `ceiling_bytes` includes the lane**, so a bar of `granted_bytes` against it can show room that
  only small jobs can use. The `small-job lane:` line says how much of it is the lane's.
- **R5. Cold-default jobs** (1 GiB, `fallback:no-history`) are exactly `y` and eligible, with footprint
  1088 MiB, so three fill `x = 4`. Intended: they are the incident's population.
- **R6. CPU.** More concurrent small jobs make the 32 slots the binding gate sooner. Rule 4 protects only
  the head.

## 9. Deferrals

- A sibling or nested cgroup for the lane (§2.3).
- Eviction or ageing of long-lived lane leases, and moving a granted main lease into the lane.
- A per-instant CPU record, without which the replay cannot be tightened (§6).
- Recognising a suite parent with an empty signature (CE6).
- A lane in ci-shim mode.
- History columns for the pool; the journal line covers the measurement.

## 10. Cut list

| Option | Why cut |
|---|---|
| The dynamic lane (lane bookings inside the main ceiling) | Withdrawn after four rounds: no proof the head cannot starve. Cited, not restated. |
| Lane real use subtracted from the pressure term (the owner's literal "separate slice" reading) | Needs per-scope reads of the lane's scopes in the sampler, couples lane to main under pressure (always, here), and still gives no hard bound. |
| Lane bookings subtracted from the pressure term | The withdrawn coupling by another route: a never-ending lane job keeps a head out. |
| A "deficit" rule (the lane yields while main stands above its ceiling) | Shrinks `x` when RAM is short, against the owner's "always fixed"; the exposure it saves is today's, not the lane's. |
| Counting the lane in `ledgerCharge` | A cap can exceed the booking (resize, discount), so the lane's real use would not be bounded by `x`. |
| Main first, lane as overflow | It saves the same in the replay (79.7% against 79.5% at `x = 4`; §1c) with lower occupancy, but small jobs within a head's budget would spend main room and the budget. Lane-first is also the owner's wording. |
| A lane-internal head rule | Puts the freeze back inside the lane; an overflowing job still has main. |
| GPU jobs ineligible | Rule 5's head reservation covers VRAM. |
| Always-main restart establish | Moves lane servers into main and lets the lane refill: up to `2x` transient over the main ceiling. |
| A `lane` field on the grant frame, or trailer text | Not needed for "which pool": the `--list` row carries it. |
| A new refusal code for a resize of a lane lease | It is harmless (§2.4). |

## 11. Rollout

1. Peers get the shared daemon-restart heads-up, after checking `aira confine --list`.
2. `aira install` renders `4G`/`1G` and keeps the other installed options. The new `Environment=` lines
   change the unit, so the install restarts the daemon itself (`install.go:1287-1290`). No second
   `systemctl restart`.
3. `aira skill install`, for the generated text.
4. Check: `aira install --status` shows both lines; the startup journal line; a `small-job lane:` line in
   `aira confine --list`.
5. After 7 days, rerun the replay and the lane-grant line count with explicit `--since`/`--until`, and
   report realised against upper-bound yield, lane-full share and `lane_used_bytes` peaks to the owner.

Release notes name the two install options, the reply fields, the journal line and the `ceiling_bytes`
change. Protocol 15 is unchanged.

## 12. Open questions for the owner

1. **Q1, host exposure (§2.3).** The recommended form adds the lane on top of today's pressure ceiling:
   no main capacity lost, and a theoretical worst-case MemAvailable floor of 6 GiB at `x = 4` (10 GiB
   today), reached only if every admitted job grows to its cap at once. The partition form keeps 10 GiB
   and costs `x` of main booking room under pressure (always, in the window). Recommended: additive.
2. **Q2, pushed heads.** Default: no lane grant behind a `--front` head, keeping AIRA-309's decision.
   Opening it would cost the pushed head nothing at the grant instant (rules 3-5 reserve its needs), and
   would keep small jobs flowing during a push. About 23 push-related journal lines in 82 h (M).
3. **Q3, `x`.** Recommended 4 GiB. 8 GiB removes 5-15 more points of upper-bound small-job wait, with a
   2 GiB worst-case floor under the additive form, or with no exposure under Q1's partition form.

Decided here, open to veto at the gate:
- eligibility and lane use in cap bytes;
- suite parents excluded;
- lane-first in every pass, with no migration of granted leases;
- the restart classification;
- the static-cap rule.

## 13. Provenance

- **Under `aira confine`**, each named, no `hint:` line in any trailer: the store queries, and the replay
  with `--db` and `--snapshot` (exit 0; rerun once, identical results).
- **Not confined, as trivial reads:** `journalctl` greps from 2026-11-15 00:00 (4733 throttled ceiling
  lines, all `system-pressure`; re-read about 20 minutes later, 4762 lines, effective 38-52.5 GiB, median
  48.75; push lines), `systemctl --user show aira.slice` (MemoryMax 64 GiB, MemoryHigh 60 GiB),
  `/proc/meminfo`, and one read-only `aira confine --list --json`.
