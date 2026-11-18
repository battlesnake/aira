# Fixed small-job lane (AIRA-350)

Status: plan, round 2, for plan review (Codex Sol at `xhigh`, Gemini) and the Fable plan gate.
Round 1 was re-based, not patched (§14). Ticket AIRA-350, milestone v0.46. Correctness-critical
(admission, host memory): two-loop, Opus builds, Fable reviews.

**Owner decision, 2026-11-18, verbatim:** "let's go back to a fixed slice that is separate to the
main slice reservation. Its max size is always fixed. The main slice reduces its admission ceiling
based on available RAM as it currently already does." Earlier wording: "in addition to the main
slice, we have a fixed slice of x GB (4 or 8), which jobs requesting <= y GB (1 GB) can be admitted
to ... smaller jobs hit the separate small-job slice by default, unless it's full, and then they hit
the main slice. This should only be in default mode, not in CI mode."

**What this replaces.** The dynamic lane of `2026-11-18-small-job-lane-design.md` (four rounds, not
approved) let small jobs take room from the main ceiling, so it could not show that a big head cannot
starve. Its evidence (§1) is reused and not restated.

**How round 2 reads the owner's words.** The lane is a separately booked pool of fixed size `x`. The
main pool's ceiling still falls with available RAM exactly as today, and it is today's ceiling less
`x`: the lane comes out of today's ceiling, not on top of it. Round 1 put the lane on top. That breaks
a decision already coded in the throttle (§2.3), so round 2 makes the partition the default and puts
the fork to the owner (§12 Q1).

Code references are to the worktree at `45f68afd` (master). Data: a store copy and a `confine --list
--json` snapshot from 2026-11-18 21:12 and 21:20 EET, kept in `~/tmp/aira-lane2-store/` and not
committed (they name other sessions' jobs). Committed: `docs/dev/aira-350-fixed-lane-replay.py` and
`docs/dev/aira-350-lane-sim.py`, each with its `.out.txt`. M marks a measured figure, E an estimate
(the replay and the sim give only estimates), R a figure reported by someone else.

## 0. The greenfield minimum, and the gap

**The problem in one line.** Small jobs wait minutes while the main ledger is full (over-booked) or
frozen behind a head that does not fit.

**What this does not address.** When every CPU slot is held, nothing is admitted, lane or no lane.
The incident is reported as 32 jobs on 32 slots (R), but at least one small job fitted RAM, CPU and
VRAM at 13:22:22, since the hold re-armed then (withdrawn spec, timeline at 13:22:22). So the incident
was partly slot-bound. The lane helps only in the instants when a slot is free. The replay estimates
RAM-side waits only, and gives a range for the CPU side (§6).

**The bare minimum.** A second booking pool of fixed size `x`, taken out of today's ceiling. A
job-level waiter whose booking is at most `y` is granted in it, before the head rule, whenever the
pool has room and today's fit test passes. That needs a boolean on the lease, sums derived from the
leases, one more term on main grants, and one branch in the evaluator.

| Addition beyond the minimum | Failure it closes | Size |
|---|---|---|
| Main headroom from main jobs only (§2.1) | Each lane grant would add 64 MiB to the main headroom (`admitSliceHeadroom`, `admit.go:1041-1051`), taking main room. | one subtraction |
| Lane occupancy counted at the cap (`reserve` + 64 MiB) | With occupancy at the discounted charge, lane real use could exceed `x` and reach main's room (§3, proof). | unit choice |
| A lane grant leaves the head's CPU slots and VRAM (§2.2) | CPU and VRAM are one shared ledger, so a lane grant could take the slot a head waits for. | two conjuncts |
| Suite parents (`--delegate-ram`) are not eligible | A 1 GiB parent brings exempt workers past the head: reported as 13 worker scopes totalling 28 GiB (R). | one frozen boolean |
| Closed behind a pushed head (§12 Q2) | AIRA-309's owner decision: only another push passes a pushed head. | one conjunct |
| An order-independent restart rule (§2.4) | The ARDR frame carries no pool, so after a restart a former lane server could book main room for good. | one branch |
| Off in ci-shim mode, set at install, suspended on a tiny slice (§2.5) | Owner decisions; a main pool too small to admit anything. | the pin-charge pattern |
| Reply fields and one journal line (§2.6) | Without them "lane full" cannot be told from "held behind the head", and the yield cannot be checked. | optional fields |

**Not built:** a second cgroup slice (§2.3), any incremented counter, eviction or migration between
pools, a lane-internal head rule, a lane in ci-shim mode. §10 lists the cuts.

## 1. Problem and evidence

- **The symptom** (owner): "a long queue of small reservation requests while at 37 GB RAM usage".
- **The incident** (R, prior spec §1.2): 2026-11-18 about 13:20 EET, 32 running jobs booked 41.5-45.4
  GiB against a ceiling of about 45 GiB while their real peaks summed to about 22 GiB; 13-14 queued
  jobs of 0.07-1 GiB waited 4-6 minutes behind a 4 GiB head (seq 14800). With 32 one-slot jobs on 32
  slots (`internal/runner/confine.go:25`; "2 per core x 16 cores", journal 2026-11-17) no job fits.
- **The same shape now** (M, snapshot 21:20 EET): 43.95 GiB granted of a 44.56 GiB ceiling (effective
  maximum 48.25 GiB), 26 jobs, 2 queued, freeze phase `hold`, real use 11.03 GiB.
- **Small jobs' share** (M, replay §0, 82.3 h): 9281 retained small rows (reserve <= 1 GiB) waited
  128.2 h in total; 2660 large rows waited 83.7 h.
- **Which ceiling term binds** (M, journal 2026-11-15 to 18): all 4733 `slice ceiling throttled`
  samples were `bound-by=system-pressure`, effective 38-52.5 GiB, median 48.75 GiB.

## 2. Design

### 2.1 Data model: one queue, one ledger, two pools

A partition of the one queue and ledger (AIRA-228), not a second cgroup (§2.3).

- **`admitWaiter.lane bool`.** The lease is booked in the lane. Written under `queue.mu` only by a lane
  grant (§2.2) and by the restart establish (§2.4). Never set on a sub-reservation, an exclusive waiter
  or a suite parent at grant time.
- **`admitWaiter.suiteParent bool`.** Frozen at enqueue in the waiter literal (`admit.go:2418`) as
  `strings.HasPrefix(request.signature, runner.AitestParentSignaturePrefix)`. Only `--delegate-ram`
  launches carry that prefix (`internal/runner/confine_linux.go:692-694`). It reads
  `request.signature`, not the bounded diagnostic copy `waiter.signature` (`admit.go:199-217`).
- **`sliceQueue.laneReserve`, `laneCharge int64`, `laneJobs int`.** A derived cache like `outstanding`
  (`admit.go:736-757`): sums of `reserve` and `ledgerCharge()`, and the count, over granted, accounted,
  `lane` waiters. One `queue.rederiveLocked()` writes these and the four existing fields, replacing
  the five `rederiveLedgerLocked` assignments (`admit.go:2330`, `2374`, `2709`, `3083`;
  `confine_resize.go:143`).
- **Per-pass quantities**, under `queue.mu`, with `H(n) = admitSliceHeadroom(n)` and `E` today's
  effective maximum (`admit.go:2561-2564`):
  - lane occupancy `L = laneReserve + 64 MiB x laneJobs`;
  - main bookings `B_m = outstanding - laneCharge`, main jobs `J_m = outstandingJobs - laneJobs`;
  - **`W`**, today's `available`, computed exactly as today on the whole slice
    (`checkedAvailable(current, E, reclaimable, outstanding, H(outstandingJobs + 1))`, `admit.go:2622-2639`);
  - **`P`**, the main pool's ledger room, `ledgerAvailable(E - x, B_m, H(J_m + 1))` (`admit.go:2931`):
    signed, no physical term.
- **`laneOn`** per pass: `x > 0 && !s.shimMode() && maximum - x > sliceCeilingUsableFloor()`
  (`sliceceiling.go:299`; `maximum` is the per-pass `memory.max`). While false, everything is today's.
- **`laneReserveMax`**: the largest reserve a lane grant can carry: `y`, or in pin-charge enforce mode
  `largestDiscountedPin(y, pinChargePercent)` (`pin_charge.go:147`).

**Every reader of the whole-slice fields is classified** (T26 pins the list; T30 drives the non-
evaluator rows):

| Site | Today | With the lane on |
|---|---|---|
| `sliceProvablyEmpty` (`admit.go:586-588`), residuals (`admit.go:1284-1290`) | whole slice | unchanged |
| evaluator RAM room (`admit.go:2622-2663`) | `available` | main waiter: `min(W, P)`; lane grant: `W` |
| discounted physical rule, `physicalFree` (`admit.go:2659-2666`) | whole slice | unchanged, both pools |
| head eligibility, `admitCouldFitEmptyLocked` (`admit.go:2770`, `2843`) | `E` | `E - x` |
| too-large: enqueue (`admit.go:2386`), `admitConnection` (`admit.go:1891-1898`), `admitCeiling` (`admit.go:1490`) | `maximum`, whole jobs | `maximum - x`, `J_m`; a reserve `<= laneReserveMax` is never too large |
| worker probe (`worker_admit.go:244-254`); worker claims go through the evaluator | `available` | `min(W, P)` |
| `CeilingBytes` (`confine_manage.go:261`) | `E - H(J+1)` | `(E - x) - H(J_m + 1)`, what a non-small job faces |
| `GrantedBytes`, `Jobs`, `ScopeJobs`, `ScopeBytes` (`confine_manage.go:242-288`) | whole slice | main pool only; the lane in its own fields (§2.6) |
| clients of `CeilingBytes`: the push hint (`confine_queue_position_linux.go:216`), the "N of M granted" clause (`:239-240`), the own-size note (`:259-261`), the exclusive wait (`:456-457`), `--list` (`cmd/aira/main.go:3752`, `3760`), the drain warning (`main.go:3902`), `aira top` (`cmd/aira/tui_top.go:1528-1532`) | — | unchanged code; each now compares main with main. One change: the own-size note is not printed for a reserve `<= lane_job_max_bytes` while `lane_capacity_bytes > 0` |
| `ci_dump.go:196`, `205` | whole slice | unchanged (lane off in shim) |

### 2.2 The admission rule

In `evaluateAdmitQueue` (`admit.go:2472`), after the restart freeze (`admit.go:2592`) and the
exclusivity gate (`admit.go:2604`), before the head `switch` (`admit.go:2688`):

```
W := today's available;  physicalFree := today's (admit.go:2661)    // whole slice, unchanged
if laneOn && laneEligible(w) {
    if laneFits(w, head, W, physicalFree) {
        grant as admit.go:2700-2724 with w.lane = true before the re-derive; no head.bypassed,
        no budgetWithheld, no anchor write; log the lane-grant line; continue
    }
    w.laneFull = L + w.reserve + 64 MiB > x                        // diagnosis only
}
available := W; if laneOn { available = min(W, P) }                 // the main path, as today
... today's code from admit.go:2663 on, with `available` and, for the head, E - x
```

**`laneEligible(w)`:** `!w.isSubReservation() && !w.exclusive && !w.suiteParent && w.ledgerCharge() <= y`.
Eligibility is in booking bytes, as the ticket states.

**`laneFits`** needs all five. `cpuH` and `vramH` are the head's `cpu` and `vram` when this pass has
found a head (`admit.go:2770-2779`), else 0.
1. **Not behind a pushed head:** `head == nil || head.pushSeq == 0`.
2. **Lane room, at the cap:** `L + w.reserve + 64 MiB <= x`.
3. **Today's fit on the whole slice:** `w.ledgerCharge() <= W`, and for a discounted waiter
   `w.reserve <= physicalFree` (`admit.go:2663-2666`). This is the check every grant passes today.
4. **CPU, leaving the head's:** `w.cpu + cpuH <= cpuAvailable(cpuCeiling, cpuOutstanding)`.
5. **VRAM, leaving the head's:** `w.vram == 0 || vramWaiterFitsLocked(w.vram + vramH, vramOutstanding)`.

Round 1's rule 3 reserved the head's physical need. It is gone: the head's RAM room no longer
depends on the lane (§3), so the term only closed the lane behind large heads.

**A waiter the lane refuses** goes on to the main path. It is in no pool while queued, so every pass
tries the lane first again (T19). **Placement:** lane grants pass an unpushed head's budget and hold,
including in a backstop pass; lane-first applies with no head too (T21). **Inside the lane:** first
fit in evaluation order (`admitOrderLocked`, `admit.go:452-476`), no lane-internal head rule (T20).

### 2.3 Host protection

**Every grant, in either pool, passes today's fit test on today's ceiling** (rule 3 and `min(W, P)`).
So at every grant the slice's total bookings plus headroom stay within today's effective maximum,
exactly today's guarantee, in the pressure regime and the static one alike. A lane cannot add
booking beyond what today's throttle allows, and the worst-case free-memory floor stays where it is
today. The static cap needs no separate rule: `E <= memory.max`.

**The prior decision this keeps.** The throttle refuses any setting whose target state lies inside the
watchdog's kill band (`sliceceiling.go:319-325`, "the throttle would target a state inside the
watchdog's kill band"; trip 8 GiB, `watchdog.go:24`). Round 1's additive form put that target at
`freeMin + 2 GiB - x`, 6 GiB at `x = 4` (§14). Additive is safe by that standard only for
`x <= freeMin + 2 GiB - 8 GiB = 2 GiB`, where the replay's estimate is 60.5% and 16.5% with silting
(replay §1, §1b). The partition keeps the decision, so it is the default.

**The cost** is booking room, not real RAM. Main's room is `x` lower at every instant, less the
bookings the lane takes out of main: an estimated loss of 2.88 GiB on average at `x = 4` (p90 4.00)
and 6.77 at `x = 8` (replay §1f, E). The sim's control `off-x` (today's rules, ceiling `x` lower, no
lane) shows heads waiting exactly as long under the partition as under `off-x`, and the lane adds
nothing beyond it (§6).

**A ledger partition, not a second cgroup slice.** A real `aira-lane.slice` would give a kernel cap on
lane real use, but the confine client, `--list`, `--kill`, the reaper, `--resize` and the worker paths
would all have to learn a second slice, and the CPU and VRAM ledgers would have to become
machine-wide (`admit.go:742-757`). Each lane job's scope cap (`memory.max = reserve`) already bounds
the lane's real use by `L <= x`, which is all the proof needs.

### 2.4 Interactions

| Interaction | Decision | Where / test |
|---|---|---|
| Pushed head (`--front`) | Lane closed behind it (rule 1). A pushed small waiter is evaluated first and takes the lane if it fits. | AIRA-309; Q2; T8 |
| `--exclusive` | The gate `continue`s before the lane (`admit.go:2604-2621`). Exclusive waiters are not eligible. A drain waits for lane leases (`sliceProvablyEmpty` counts them). | T10 |
| `--priority high` | Order only; an unpushed head. | T9 |
| Fairness freeze, AIRA-59 backstop | Lane grants never arm, never count in `bypassed`, never set `budgetWithheld`. A waiter the lane refuses arms exactly as today. | T11, T12 |
| AIRA-324 | Eligibility on `ledgerCharge()` (the ticket's unit); lane occupancy on the cap, since the proof needs lane real use `<= L`. In observe mode, which `aira install` renders (`install.go:62`), the two are equal (`pin_charge.go:120-125`). | T15 |
| aitest | Workers and `confine-reserve` holds are sub-reservations: never eligible, booked in main, exempt from the head as today (`admit.go:2675-2688`). Suite parents are not eligible. | T14 |
| Restart (ARDR establish, `admit.go:2343-2378`) | An absent lease is established **in the lane iff `laneOn`, `parentScopeID == ""` and `reserve <= laneReserveMax`**, without a room check; otherwise in main. The rule is order-independent and returns every former lane lease to the lane. It may also put a former main small lease or a suite parent there (the frame carries no signature, `redeclare.go:74-79`), so `L` may exceed `x` until those end; no lane grant happens meanwhile. A re-declared lease is already running and never passes the evaluator, so a suite parent in the lane is an accounting matter, not a head bypass: its workers book in main exactly as before. | CE5; T17 |
| Re-anchoring SET (`admit.go:2318`) | Keeps `lane` as it is, whatever the re-declared reserve. Moving the lease to main would cost the head its whole reserve; keeping it costs at most `L - x`. | T18 |
| `confine --resize` | Lowers `charge` only (`confine_resize.go:139`). `L` counts the cap, so lane room and `P` are unchanged. | T16 |
| Long-lived lane jobs | First come, no eviction, no migration. Measured silting: 5 small job-level scopes about 128 h old, 1.56 GiB of footprint (M). | §6 |
| Overflowed waiter, lane room later | Takes the lane on the next pass. A granted main lease never moves. | T19 |
| Restart freeze | Earlier than the lane: frozen waiters `continue` at `admit.go:2592-2595`. | T27 |
| ci-shim mode | Off: Serve forces `x = 0` and `laneOn` checks `!s.shimMode()` (`shim.go:37`). | T23 |

### 2.5 Configuration (the pin-charge pattern)

**Daemon** (`internal/daemon/small_lane.go`, beside `pinChargeConfigFromEnv`, `pin_charge.go:53-90`):
- `AIRA_DAEMON_SMALL_LANE`: `x`. Unset, empty or `off` is off (the raw default). Otherwise a size
  parsed by `runner.ParseMemorySize` (`internal/runner/memory_size.go:82`), from `y + 64M` to `8G` (the
  owner's range). Else `E_CONFIG_INVALID: AIRA_DAEMON_SMALL_LANE must be off or a size from <y+64M> to 8G, such as 4G`.
- `AIRA_DAEMON_SMALL_LANE_JOB_MAX`: `y`, read only when the lane is on, default 1G, from 1M
  (`runner.MinPinnedScopeCap`) to `x - 64M`; else `E_CONFIG_INVALID`.
- Serve reads both beside the pin-charge read (`server.go:416-429`) and logs `aira daemon: small-job lane
  4G for jobs booking at most 1G, taken from the main pool's ceiling`. No new error code.
- **Suspension.** A pass with `maximum - x <= sliceCeilingUsableFloor()` (a `memory.max` lowered at run
  time) is a lane-off pass: no lane grant, `available = W`, lane leases count in main. One journal line
  per transition: `aira daemon: small-job lane suspended: memory.max <m> leaves no main pool beside <x>`.

**Install** copies each pin-charge site: flags `--small-lane SIZE|off` and `--small-lane-job-max SIZE`
(`install.go:154-158`, parsed beside `install.go:471-492`, refused with `E_INSTALL_ARGUMENT_INVALID`); an
option not given keeps the installed value (`install.go:640-650`); ship defaults `4G` and `1G`
(`install.go:695-706`); re-exec forwarding (`install.go:890-895`); `renderDaemonUnit` re-validation
(`install.go:1632`); two placeholders and lines in `internal/install/assets/aira-daemon.service.in`;
`--status` lines (`install.go:2507-2508`); `AIRA_DAEMON_SMALL_LANE=off` in the shim environment
(`internal/install/mode.go:321`); the usage string (`internal/core/core.go:2241`). **Small slices:** where
`MemoryMax - x <= sliceCeilingUsableFloor()` (install allows 4 GiB, `install.go:68`), the default renders
`off` with the message `small-job lane off: MemoryMax <m> leaves no main pool beside a <x> lane`; an
explicit `--small-lane` that large is refused.

### 2.6 Observability (minimal)

- **`confine --list` reply** (`runner.ConfineSliceReserve`, `internal/runner/confine_manage.go:586`),
  filled in `admitSliceSnapshotFor` (`admit.go:1302`), present only on a `laneOn` reading:
  `lane_capacity_bytes` (`x`), `lane_used_bytes` (`L`), `lane_jobs`, `lane_job_max_bytes` (`y`).
  While the lane is on, `ceiling_bytes`, `granted_bytes`, `jobs`, `scope_jobs` and `scope_bytes`
  describe the main pool, so every client comparison of granted with ceiling stays like with like.
  Residuals stay whole-slice, so the identity `jobs = scope_jobs + reservation_jobs + residual` holds,
  as lane leases are always scope jobs. An old client sees a consistent main pool and no lane.
- **Per scope row:** `lane: true` (omitempty).
- **For the named queued scope** (the AIRA-24 probe, `admit.go:1352-1364`): `lane_full` and
  `held_by_head` (omitempty), from diagnosis-only fields written beside `noteGrantableLocked`
  (`admit.go:2760`) and overwritten every pass; no decision reads them (`admit.go:261-280` discipline).
- **Text:** `small-job lane: 3.0G of 4G used, 3 jobs (jobs booking at most 1G)` after `slice queue:`
  (`cmd/aira/main.go:3777`); the wait line (`confine_queue_position_linux.go:151`) appends `, small-job
  lane full` and/or `, fits but held behind the head of the queue`.
- **Journal:** one line per lane grant (about 110 an hour, M): `aira daemon: small-lane grant seq=<n>
  reserve=<bytes> waited=<ms> lane=<used>/<cap> jobs=<n>`.

### 2.7 Generated text (faces)

On the AIRA-340 pattern (`internal/core/skill.go:305-318`): `ConfineSmallLaneGuidance` (long, the skill
at `skill.go:338` and the install help) and `ConfineSmallLaneSummary` (short, the `confine` verb summary
at `core.go:2607` and the MCP instructions at `cmd/aira/mcp.go:29`).
`TestConfineSmallLaneGuidanceIsOnEveryFace` pins the keywords (`cmd/aira/confine_priority_test.go:100`).
- **Long (draft):** "Small-job lane (default on, not in CI mode): a job booking at most 1G (`aira install
  --small-lane-job-max`) is admitted first from a fixed 4G share of the slice (`--small-lane`), without
  waiting behind larger queued jobs; when that share is full it queues for the rest of the slice as
  before. It still needs a free CPU slot, and it never takes the slot or GPU memory the job at the head
  of the queue is waiting for. Keep small jobs small: a short command pinned above 1G loses the lane.
  Long-running servers hold their share of the lane for their whole life, so give a server an honest
  small reserve and stop it when done. `--delegate-ram` suites and their workers never use the lane."
- **Short (draft):** "Jobs booking at most 1G are admitted from a separate small-job lane first, so keep
  short commands' reserves small."

## 3. Invariants

1. **`L <= x` after every lane grant** (rule 2). Only a restart establish or a SET can raise `L` past
   `x`; while it is, no lane grant happens and `L` only falls.
2. **Today's fit at every grant.** Every grant in either pool passes `W` (and the discounted physical
   rule, CPU and VRAM). So right after every grant `outstanding + H(outstandingJobs) <= E`, counting
   the new job, exactly as today.
3. **`P` reads no lane state:** `B_m`, `J_m` exclude lane leases, and `x` is constant.
4. **Head independence.** While `L <= x` and the slice's real use outside the lane's scopes is at most
   `B_m`, a main waiter's RAM room is exactly `P`. No lane grant, release, real-use growth or resize
   changes it.
5. **Head CPU and VRAM.** A lane grant past head `H` leaves `H`'s slots and VRAM free, and never changes
   `H.bypassed`, `budgetWithheld` or `freezeArmedAt`.
6. **AIRA-307's bound** on main grants past an unpushed head holds unchanged (`admit.go:485-490`).
7. **No lane grant behind a pushed head.**
8. **Off is today.** `laneOn` false (unset, `off`, ci-shim, suspended) gives `available = W` and head
   eligibility on `E`; every existing test passes unchanged.
9. **Precedence:** restart freeze, exclusivity gate, lane, head rule.
10. **Restart.** Every former lane lease is re-established in the lane, so main's room `min(W, P)` after
    a restart is at least what it was before, for the bookings as re-declared.

**Proof of 4.** `H(J+1) - H(J_m+1) = 64 MiB x laneJobs`, so
`W - P = x - 64 MiB x laneJobs + B_m - max(effectiveCurrent, B_m + laneCharge)`. Lane scopes use at
most `laneReserve` (each is capped at its reserve), the rest of the slice at most `B_m` (assumption),
and `laneCharge <= laneReserve`. So the max is at most `B_m + laneReserve`, and
`W - P >= x - L >= 0`. Then `min(W, P) = P`. `E` does not move with lane growth either: the pressure term
is invariant to the slice's own anon growth (`sliceceiling.go:228-230`). ∎

**Proof of 10.** A lane grant needs `ledgerCharge <= y` and no parent, so its reserve is at most
`laneReserveMax`; the establish rule puts it back in the lane. A former main lease moved into the lane
lowers `B_m` and `J_m` and raises `L` by the same footprint: `P` rises, `W` is unchanged. ∎

**What they do not give.** The head still faces a main pool `x` smaller than today's ceiling, always;
lane state does not move it further. The counterexamples, each pinned by a test:
- **CE1, the untouched budget** (T13): small jobs that spent `H`'s budget now go to the lane, so a
  mid-size job can pass `H` within the budget.
- **CE2, the duty cycle** (T12): lane grants do not arm, so holds arm at other instants. In the sim's W3
  sweep this loses today's one lucky phase: today starves the head in 29 of 30 phases, `fixed` in 30,
  as do `off-x` and `fixed-add` (§6).
- **CE3, slots held from before** (T3b): lane jobs granted before `H` became the head hold their slots;
  from then on rule 4 reserves `H`'s.
- **CE4, main over its own booking** (T29): if the slice's non-lane real use exceeds `B_m` by more than
  `x - L` plus the lane's unused cap, `W` binds and lane real use lowers `H`'s room. Possible only with
  discounted main leases (enforce, not installed) or use in the slice that no booking covers.
- **CE5, restart silting** (T17): former main small leases and suite parents may take lane room after a
  restart, with `L > x`, until they end.
- **CE6, a suite parent with an empty signature** (T14d): an argv that cannot be resolved makes a
  `--delegate-ram` parent eligible; not measurable from the store.
- **CE7, the partition's cost** (T22d): a head that fits today's room but not `P` waits for the ceiling
  to rise or main leases to end. The sim's W12 head is never granted under `fixed` or `off-x` while
  1.5 GiB of never-ending main leases stand (210 s today); with those leases in the lane, 300 s. The
  largest retained reserve is 25.3 GiB (M, replay §1g), below the main pool's empty limit
  `E - x - H(1)` at the lowest measured ceiling (38 - 4 - 2.06 = 31.9 GiB).
- **CE8, suspension** (T31): while suspended, lane leases count in main.
- **CE9, a pin-charge mode change from enforce to observe or off across a restart** returns a
  discounted lane lease above `y` to main. Documented, not tested (it needs two daemon configurations).

## 4. Wire and protocol

- **ProtocolVersion stays 15** (`internal/daemon/protocol.go:218`). The admit request, grant frame and
  ARDR frame are unchanged.
- **The `confine-list` reply gains optional fields** (§2.6), decoded with `json.Unmarshal`
  (`confine_queue_position_linux.go:388`): an old client ignores them.
- **Meaning change while the lane is on:** `ceiling_bytes`, `granted_bytes`, `jobs`, `scope_jobs`,
  `scope_bytes` describe the main pool. An old client then sees the main pool consistently; the
  release notes name this.

## 5. Tests (each must fail against the named wrong implementation)

**Common settings** on the AIRA-228/307 seam (`queueOf`, `admit_queue_order_test.go:41`), driving
`evaluateAdmitQueue`. Headroom is production (2 GiB + 64 MiB a job; `admitTestServer` zeroes it, so the
helper restores it). CPU 32 slots; held leases carry `cpu = 1` (not `heldLease`'s 0,
`admit_queue_order_test.go:37`). `memory.max` 65536 MiB, `x` 4096, `y` 1024, real use 12288 MiB, a hold
pass (`freezeArmedAt = now - 60 s`, `maxHold` 5 min) and a head budget preset as spent, unless stated.
Every test states `E` and asserts the granted set and each grant's `lane`. All sizes in MiB.

| Test | Setup | Assertions | Wrong implementation it fails |
|---|---|---|---|
| **T1 incident, RAM side** | `E` 49152; 20 main leases, `B_m` 38182, so `P = 45056 - 3392 - 38182 = 3482`. A 4096 head. 14 small: 72, 256, 512, 1024, 512, 256, 1024, 72, 512, 1024, 256, 512, 72, 1024. | Lane grants 1-6, 8, 9, 11; `L` 4048. 7, 10, 12, 13, 14 `laneFull` and `heldByHead`, `lastGrantable` 3482. `bypassed` unchanged. | Lane off: none. No 64 MiB: 1-8, 11, 13. Lane jobs in main headroom: 2906. Main on `W` alone: 3530. Round 1's additive main: 7578. Grants in `bypassed`. |
| **T1b incident, slots** | T1 with 32 of 32 slots held. | No grant. | Lane without rule 4. |
| **T2 main pool full** | `E` 49152; 22 main leases, `B_m` 41400 (`P` 136). Four 1024 small, nothing else. | Three lane grants (`W` 4232, 3144, 2056 before each). The fourth: `laneFull`, refused on capacity, becomes the head. | Lane judged on `P`: none. |
| **T3 CPU** | `E` 48640; 28 main leases, `B_m` 38347 (`P` 2293), 28 of 32 slots. A 5120 head. Then 2154, 2700, 1024, 256, 512, 512, 512, 1024. | Lane: 1024, 256, first 512 (31 slots). Other 512s and 1024 `heldByHead`, not `laneFull`; 2154 held; 2700 refused. | No `cpuH`: a fourth grant. |
| **T3b CE3** | 30 main + 2 lane leases (granted before any head) hold 32 slots. A 5120 head refused on `P`; a 256 small. Then one lane lease ends. | Before and after the release: the small waiter is not granted. | No `cpuH`: granted after the release. |
| **T4 lane growth** | `E` 49152. Main: 38784 permanent, 1024 released at t=10. A 4096 head. A never-ending 1024 small waiter. At t=10 real use is set to main 38784 plus lane 1024 (both at their caps), and in a twin run lane 0. | t=0: small in the lane; head refused, `P` 3008. t=10: head granted in both runs (`P` 4096, `W` 7104). With a 4097 head instead: refused in both, `lastGrantable` 4096 in both. | Main on `checkedAvailable(current, E - x, ...)`: 3072, refused. Lane in main's ledger: refused. Lane in main headroom: 4032. |
| **T5 rule 3** | `E` 49152, no head, real use such that `W` is 1023, then 1024. A 1024 small waiter. | Refused at 1023; granted at 1024. | Lane without `W`. `<` for `<=`. |
| **T6 large head, lane open** | `E` 49152, `J_m` 10, `B_m` 38208 (`P` 4096), real use 38208 (`W` 8192). An 8192 head refused. One 1024 small. | Granted in the lane. | Round 1's rule 3 (`1088 + 8256 >` physical room 8192): refused. |
| **T7 VRAM** | Head needs 4096 VRAM, refused on it; 5120 free. Small waiters needing 2048 and 0 VRAM. | Only the second in the lane. | No `vramH`. |
| **T8 pushed** | A pushed head refused, a small waiter behind it; separately a pushed small waiter. | First held (`heldByHead`); second in the lane. | Lane before rule 1. |
| **T9 high** | An unpushed `high` head refused; small waiters. | In the lane, head's slot kept. | High treated as pushed. |
| **T10 exclusivity** | (a) Exclusive holder granted, a small waiter. (b) A drain queued while a lane lease runs. | (a) Not granted. (b) Drain granted only after the release. | (a) Lane before the gate. (b) Emptiness on main jobs. |
| **T11 backstop** | `freezeArmedAt = now - maxHold`; an eligible small waiter. | In the lane; `bypassed` unchanged. | Lane after the head `switch`. |
| **T12 CE2** | Idle phase, budget spent. Pass 1: two small waiters, lane on; twin with lane off. Pass 2 (lane on): lane full, a small waiter that fits `P`. | Lane on: pass 1 no arm, pass 2 arms. Lane off: pass 1 arms. | An anchor write on a lane grant. |
| **T13 CE1** | `E` 49152, `J_m` 10, `B_m` 34112 (`P` 8192; lane off `W` 12288). A 16384 head, `bypassed` 0. Four 1024 small, then a 5120. | Lane on: three in the lane, fourth and 5120 via the budget (`bypassed` 6144). Lane off: four via the budget, 5120 withheld. | Pinned residual; a build claiming to remove it changes this knowingly. |
| **T14 aitest** | (a) A 512 sub-reservation. (b) Enqueue with `signature = AitestParentSignaturePrefix + "x"`, 1024, behind a held head. (c) Plain signature. (d) A `--delegate-ram` request with an empty signature. | (a) Main, `lane` false. (b) `suiteParent`, held. (c) Lane. (d) Lane (CE6 pinned). | (a) Ignoring `parentScopeID`. (b) Ignoring `suiteParent`. |
| **T15 AIRA-324** | Enforce, 80%. (a) Pin 1228 booked at 983. (b) Pin 1024 booked at 820. | (a) In the lane, `L` 1292. (b) In the lane, `L` 1088. | Eligibility on `reserve`: (a) not in the lane. Occupancy on charge: `L` 1047, 884. |
| **T16 resize** | A 1024 lane lease resized to 256. | `L`, `B_m`, `P` unchanged; `outstanding` down 768. | Occupancy on charge. `B_m` including lane charge. |
| **T17 establish** | (a) Re-declare 512, no parent, empty queue. (b) `L` 3800, then the same. (c) With a parent. (d) A (lane server 1024) and B (former main 1024), both parentless, in both orders. (e) A suite parent 1024 and its parented worker. (f) Enforce: reserve `laneReserveMax`, and 1 MiB above. | (a) Lane. (b) Lane, `L` 4376; a new 256 small waiter is `laneFull`; `P` unchanged. (c) Main. (d) Both in the lane in both orders; identical `B_m`, `L`. (e) Parent lane, worker main. (f) Lane; main. | Room-limited (round 1): (b) main, (d) order-dependent. Always-main: (a). |
| **T18 SET** | A 512 lane lease re-anchored with reserve 2048. | Stays in the lane; `P` unchanged. | Normalised to main: `P` down 2112. |
| **T19 overflow retry** | Pass 1: `L` 4048, a 1024 small waiter held. A lane lease ends. | Pass 2: in the lane. | A pool fixed at first evaluation. |
| **T20 first fit** | Lane residue 600; waiters 1024 then 256. | 256 in the lane; 1024 to main. | A lane head-of-line rule. |
| **T21 lane first** | No head, ample room, one small waiter. | Lane; `B_m` unchanged. | Main first. |
| **T22 host** | (a) `E` 65536: a 59392 waiter; and on `memory.max` 8192 with 25 main jobs, a 1024 waiter. (b) `E` 49152, a re-declared 64512 main lease, three 1024 small waiters. (c) `E` 45056 (throttled), main at `P = 0`, small waiters until the lane is full. (d) CE7: `E` 49152, real use 1000, 2000 of main leases, a 42000 head. | (a) The first refused too large (`> 65536 - 4096 - 2112`); the second not. (b) None in the lane (`W < 0`). (c) Right after every grant `outstanding + H(outstandingJobs) <= E`. (d) Head refused (`P` 40880; `W` 44976). | (a) No `maximum - x`; small reserves not exempt. (b), (c) Lane without `W`. (d) Main on `W` alone. |
| **T23 ci-shim** | A shim server with the lane fields set. | No lane grant. | Without `!shimMode()`. |
| **T24 off is today** | `x = 0`; a table of inputs, degenerate ones included. | `available`, `physicalFree`, grants equal today's; the existing suite passes. | Any leak when off. |
| **T25 derived and bounded** | Random grant, release, establish, SET, resize. | Lane sums equal a fresh walk; `L <= x` after every lane grant; `L > x` only after an establish or SET; invariant 2 after every grant. | An incremented counter missed on one path. |
| **T26 guards** | Syntactic allow-lists on the `admit_pin_charge_guard_test.go` pattern. | Every selector of `outstanding`, `outstandingJobs` and the lane sums is classified (§2.1); no decision reads `laneFull`, `heldByHead` or `signature`. | A new unclassified reader. |
| **T27 restart freeze** | Freeze active, empty lane, an eligible waiter. | Not granted; `waited` set. | Lane branch before the freeze gate. |
| **T28 boundaries** | (a) `L + f == x`, then the same with `x` 1 MiB smaller. (b) `w.cpu + cpuH == cpuAvailable`. (c) A main waiter at `min(W, P)` exactly, then 1 MiB over. | (a) Granted; `laneFull`. (b) Granted. (c) Granted; refused. | `<` for `<=` in rules 2 and 4 and the main check. |
| **T29 CE4** | `E` 49152; main 38784 with real use 42000; a lane server 1024; a 4096 head. Lane real use 1024, then 0. | Refused at 1024 (`W` 3888); granted at 0 (`W` 4912). | Pinned residual. |
| **T30 readers** | A worker probe and a worker claim, `admitCeiling`, the enqueue and `admitConnection` too-large, with lane leases present. | Probe `AvailableBytes = min(W, P)`, unchanged by a lane grant while `P` binds; claim granted iff it fits `min(W, P)`; ceilings on `maximum - x` and `J_m`. | A site left on whole-slice values. |
| **T31 suspension** | `memory.max` such that `maximum - x <= floor`, one lane lease. | No lane grant; `available = W`; one transition line. | No suspension: main room `P <= 0` freezes the slice. |
| **T32 client** | Reply with the lane on: a 3 GiB job facing a 2.5 GiB `ceiling_bytes`; a 1 GiB job facing 0.8 GiB; lane leases with main below its ceiling. | First: no push hint, own-size note. Second: no own-size note. Third: no drain warning. | A whole-slice `ceiling_bytes` or `granted_bytes`. |

**Configuration and install.** Environment: `off`, `4G`; refused `0`, `9G`, `abc`, `x < y + 64M`; `y`
ignored while off; shim forces off; the startup line. Install flags, kept value, ship defaults, re-exec
forwarding, render re-validation, `--status`, shim environment, the small-slice default `off` with its
message, and the refusal of an explicit value too large. **Observability and faces:** reply fields only
while on; the `lane` row flag; `lane_full`/`held_by_head` only for the named scope; the `--list` line,
both wait clauses and their absence; the journal lines; `TestConfineSmallLaneGuidanceIsOnEveryFace`.

**Mutations, by direction.** The gate runs each and records it RED, with a committed
`docs/dev/aira-350-mutation-check.py` on the `docs/dev/aira-324-mutation-check.py` pattern.
- **False pass:** no 64 MiB (T1); lane without `W` (T5, T22b, T22c); no `cpuH` (T3, T3b, T1b); no
  `vramH` (T7); lane before rule 1 (T8), the gate (T10a) or the freeze (T27); eligibility ignoring
  `parentScopeID` (T14a) or `suiteParent` (T14b); occupancy on charge (T15, T16); main on `W` alone
  (T1, T22d); no `maximum - x` (T22a); on in shim (T23); emptiness on main jobs (T10b); an off-by-one
  that grants 1 MiB over the room (T28a, T28c).
- **False fail:** lane deleted (T1, T2); lane in main headroom (T1, T4); lane in main's ledger (T4); main
  on whole `checkedAvailable` at `E - x` (T4); lane judged on `P` (T2); round 1's rule 3 (T6); high as
  pushed (T9); anchor write on a lane grant (T12); eligibility on `reserve` (T15a); room-limited or
  always-main establish (T17); SET normalisation (T18); pool fixed at first evaluation (T19); lane
  head-of-line (T20); main first (T21); small reserves too large (T22a); `<` in place of `<=` (T5, T28);
  no suspension (T31).
- **Moved:** lane after the head `switch` (T11); lane grants in `bypassed` (T1).
- **Pinned residuals, not mutations:** T13 (CE1), T29 (CE4), T14d (CE6), T22d (CE7).

## 6. Expected yield: estimates, not bounds

`docs/dev/aira-350-fixed-lane-replay.py` replays each retained confine job (arrival `started_at_ms -
wait_ms`, hold `wall_ms`, footprint `budget + 64 MiB`) and tries eligible jobs in the lane first, first
fit, until their historical grant. All figures are **simulated estimates** (E): first fit is not
monotone, so dropping a gate is not guaranteed to raise the saving (§1c's main-first variant saves
more with fewer lane jobs). The saving is **gross**: it counts small jobs only and does not model the
extra wait that `x` less main room causes for large and overflow jobs.

| `x` | Small wait-h removed, of 128.2 | With 1.56 GiB silting | CPU allowance 20 / 24 / 27 | Lane full (1 GiB) | Main room lost, mean (p90) | Lane real p99 / peak | Lane slots p99 / peak |
|---|---|---|---|---|---|---|---|
| 4 GiB | 101.9 h (79.5%) | 68.6% | 78.8% / 71.4% / 32.3% | 9.7% | 2.88 (4.00) GiB | 2.11 / 3.07 GiB | 5 / 13 |
| 8 GiB | 107.9 h (84.1%) | 84.0% | 83.1% / 73.0% / 32.4% | 0.1% | 6.77 (8.00) GiB | 2.61 / 4.71 GiB | 6 / 15 |

Replay §1, §1b, §1e, §1f; `x` of 2 and 6 GiB are in the output.

- **CPU (§1e).** Retained rows show 5 jobs running at the incident against 32 reported, and 8 at the
  copy against 26 live. That is an allowance of about 18-27 slots that the store does not show (aitest
  workers, rows lost to the 20-row cap, servers still running). The estimate falls off a cliff between
  24 (71%) and 27 (32%). At allowance 27, `x = 4` and `x = 8` are the same. **Range at `x = 4`: about
  32-80%, and 0 in any instant with all 32 slots held.**
- **Biases.** Upward: no CPU gate in §1-§1d, and no reservation of the head's CPU and VRAM; retention
  thinning understates lane occupancy (186 signatures at the cap); servers absent (§1b adds 1.56 GiB of
  them). Downward: jobs leaving main free main room for others. Not modelled: the partition's cost to
  main (§1f gives its size, not its wait).
- **The sim** (`aira-350-lane-sim.out.txt`; a model of the rules, not the Go code). Head grant times:
  in W2, W3, W8, W8b, W8c, W8d and W12, `fixed` equals the control `off-x` (today's rules, ceiling `x`
  lower, no lane). So every delay against `off` (W8 and W8c 60 s against 20 s; W12 never against 210 s)
  is the partition's capacity cost, and the lane adds none. In W12b (the never-ending leases in the
  lane) `fixed` grants at 300 s, `off-x` never. W0 (illustrative): head 240 s under `fixed` and `off-x`
  (60 s today); small jobs' mean wait 47 s (157 s today, 215 s `off-x`).
- **Recommendation: `x = 4 GiB`, `y = 1 GiB`, chosen on cost.** 8 buys 4.6 points of gross small-job
  yield (15.4 with silting) and none at allowance 27, for 3.9 GiB more main room lost on average.
- **The idle cost** is booking room, not RAM: an idle lane holds no memory, but main's room is `x`
  lower all the same. At the measured quarter of real use per booked byte (§1), 4 GiB of booking stands
  for about 1 GiB of real use (E).

**Acceptance after rollout (§11).** Over 7 days from `confine_peak_history`, against the 82.3 h window
(M: all jobs 63.9 s waited per job, small 49.7 s, large 113.3 s): **keep the lane on if wait per job
over all jobs falls, and small-job wait per job falls by at least 30%.** Anything else goes to the owner
with the lane-grant line count, the lane-full share and `lane_used_bytes` peaks. Load differs between
weeks, so this is a screen, not a measurement of cause.

## 7. What this does not fix

- **Ledger over-booking**, the larger cause: leases book 2-5x what they use (R; M, a quarter at 21:20).
  The levers are AIRA-318 and AIRA-347 (both held) and the `AIRA_AITEST_WORKER_OVERHEAD_BYTES` floors
  fastest-ee sets (R).
- **CPU slot saturation**: nothing is admitted with all slots held (§0).
- **Large jobs**: they lose `x` of room (§2.3, CE7).
- **Not reopened:** AIRA-351, measured NO-GO (`docs/dev/aira-351-cold-start-decision-rule.md:56`,
  `docs/dev/aira-351-cold-start-measure.out.txt`); AIRA-352 and AIRA-353, held
  (`docs/superpowers/specs/2026-11-18-admission-diagnostics-and-backstop-design.md`).
- **Waits behind a pushed head** (Q2) or an exclusive drain.

## 8. Risks

- **R1. Main capacity.** Large jobs and heads lose `x` of booking room (2.88 GiB net mean at `x = 4`,
  E); heads near main's room wait for the ceiling (CE7). Watched through the acceptance screen.
- **R2. Silting.** Long-lived small jobs, and after a restart former main small leases and suite
  parents, hold lane room. Measured at 1.56 GiB now.
- **R3. Missed readers.** Closed by T26 and T30.
- **R4. The summary means the main pool** while the lane is on (§4); the `small-job lane:` line carries
  the rest.
- **R5. Cold-default jobs** (1 GiB, `fallback:no-history`) are eligible with footprint 1088 MiB, so
  three fill `x = 4`. Intended: they are the incident's population.
- **R6. CPU.** Lane jobs hold slots: p99 5, peak 13 at `x = 4` (replay §1, E). Rule 4 protects only the
  head; other main waiters can find the slots taken.

## 9. Deferrals

- A sibling or nested cgroup for the lane; eviction or ageing of lane leases; moving a granted lease
  between pools.
- A pool field on the ARDR frame (it would remove CE5 and CE9; the frame is frozen, `redeclare.go`).
- A per-instant CPU record, without which the replay's CPU side stays a sensitivity range.
- Recognising a suite parent with an empty signature (CE6). A lane in ci-shim mode.

## 10. Cut list

| Option | Why cut |
|---|---|
| The dynamic lane (inside the main ceiling) | Withdrawn after four rounds. |
| Additive (round 1: the lane on top of today's ceiling) | Puts the throttle's worst-case target inside the watchdog band, which `sliceceiling.go:319-325` refuses; safe only at `x <= 2 GiB`. Owner opt-in only (Q1). |
| Lane real use subtracted from the pressure term | Couples lane to main by real use (a lane server keeps a head out) and still lets lane growth after main's grants exceed today's bound. |
| A physical reservation for the head (round 1 rule 3) | Not needed once main's room is `P`; it shut the lane behind large heads. |
| Lane occupancy at the booking (charge) | Lane real use could then exceed `x` and reach main's room (CE4's mechanism). |
| Restart classification by lane room (round 1) | Order-dependent: a former lane server can land in main for good. |
| Always-main restart establish | The same, for every lane server. |
| SET normalisation to main | Costs the head the whole reserve instead of at most `L - x`. |
| Main first, lane as overflow | Small jobs within a head's budget would spend main room and the budget; the owner said lane first. |
| A lane-internal head rule | Puts the freeze back inside the lane; an overflowing job still has main. |
| A `lane` field on the grant frame | `--list` rows carry the pool. |

## 11. Rollout

1. Peers get the shared daemon-restart heads-up, after `aira confine --list`.
2. `aira install` renders `4G`/`1G` (or `off` on a small slice); the changed unit restarts the daemon
   (`install.go:1287-1290`). Then `aira skill install`.
3. Check: `aira install --status`, the startup line, a `small-job lane:` line in `aira confine --list`.
4. After 7 days: rerun the replay with explicit window ends, count lane-grant lines, and apply the
   acceptance screen (§6). Report to the owner either way.

Release notes name the two install options, the reply fields and the summary's main-pool meaning, the
journal lines, and that the lane comes out of the ceiling. Protocol 15 is unchanged.

## 12. Questions for the owner

1. **Q1, where the lane's 4 GiB comes from.** Default: out of today's ceiling, so the machine is
   protected exactly as today and large jobs get about 3 GiB less room on average (§2.3). The
   alternative is on top of today's ceiling: no cost to large jobs, but in the worst case free memory
   can fall to 6 GiB, below the 8 GiB at which the watchdog starts killing heavy processes outside
   AIRA, which the throttle today refuses to allow. On top is safe only for a lane of 2 GiB or less.
2. **Q2, pushed heads.** Default: no lane grant behind a `--front` head (AIRA-309). Opening it would cost
   the pushed head nothing in RAM (main's room does not depend on the lane) and only CPU and VRAM,
   which rules 4 and 5 reserve. About 23 push-related journal lines in 82 h (M).
3. **Q3, `x`.** Recommended 4 GiB (§6). 8 GiB costs large jobs about 4 GiB more room for 5-15 points
   of small-job yield, and nothing when CPU slots are the limit.

Decided here, open to veto at the gate: eligibility on the booking and occupancy on the cap; suite
parents excluded; lane first every pass; the order-independent restart rule; the SET keeping the pool.

## 13. Provenance

- **Under `aira confine`**, each named, no `hint:` line in any trailer: round 1's store queries; round
  2's replay (`aira350-replay-r2`, `-r2b`; exit 0), the concurrency probe (`aira350-conc-r2`) and the sim
  (`aira350-sim-r2`, `-r2b`; exit 0). The old designs' sim rows are unchanged from round 3's output
  (checked by diff with the new columns removed).
- **Trivial reads, not confined:** `journalctl` greps from 2026-11-15 (4733 throttled lines, all
  `system-pressure`; effective 38-52.5 GiB, median 48.75), `systemctl --user show aira.slice`
  (MemoryMax 64 GiB), `/proc/meminfo`, one read-only `aira confine --list --json`, and one read-only
  `sqlite3` maximum of `budget` (25.3 GiB).

## 14. Review log

**Round 1** (Codex Sol `xhigh` and a second reviewer; 19 findings, 7 P1). No finding was fix-induced:
this was the first review of this spec. Grouped by root cause:

| Family | Findings | Root cause | Disposition |
|---|---|---|---|
| A. Host protection | P1 x4: watchdog band and Q1 left open (two), no pressure-regime rule, static cap after an ARDR deficit | Round 1 added `x` on top of today's ceiling, reversing `sliceceiling.go:319-325` without citing it, and relied on a static-cap rule that ARDR deficits bypass (`admit.go:2343`). | Re-based: every grant passes today's fit (invariant 2); the partition is the default; Q1 rewritten as the owner's fork. Closes the static case too (T22b). |
| B. Head independence | P1: lane growth starves the head through physical RAM; P2: rule 3 closes the lane behind large heads; P2: prior lane leases' growth missing from the CE list | Main read lane real use through `checkedAvailable`; round 1 patched it with an instant-of-grant reservation, which cannot cover later growth. | Main's room is `min(W, P)` with `P` ledger-only on `E - x`; proof of invariant 4; rule 3's head term deleted (T4, T6, T29). The P2's head figure "14800 MiB" is the head's sequence number; that head reserved 4 GiB (withdrawn spec, timeline). |
| C. Restart | P1: pool and suite identity lost on re-declare; P2: restart invariant untruthful; P2: no freeze-precedence test | A room-limited, order-dependent establish. | An order-independent rule that returns every former lane lease to the lane (invariant 10, T17). Identity is not carried on the wire: the ARDR frame is frozen (`redeclare.go:74-79`), and the rule already rules out the harmful direction. A suite parent placed in the lane after a restart is only accounting, since re-declares never pass the evaluator (`admit.go:2343-2378`). T27 added. |
| D. Units | P2: eligibility contradicts the ticket | Eligibility and protective accounting shared one unit. | Eligibility on `ledgerCharge()` (ticket); occupancy on the cap (proof). Ticket updated. |
| E. Evidence | P2 x3: "upper bounds" unproven, yield not established, necessity under CPU | First fit is not monotone; no CPU gate; the incident was partly slot-bound. | Relabelled estimates; CPU sensitivity (§1e) gives 32-80%; gross, not net; `x` chosen on cost (§1f); acceptance screen defined; §0 states the slot-bound limit; T1b. |
| F. Client wire | P2 x2: `ceiling_bytes` decisions | Round 1 grew `ceiling_bytes` by `x`, and the client decides on it. | The summary describes the main pool; every client consumer listed (§2.1); T32. |
| G. Test plan | P2 x2: CE list not pinned, readers and boundaries untested | — | CE1-CE8 each pinned; T28, T30, T31 added; every test states `E`; the sim gained `fixed`, `fixed-add` and the control `off-x`. |
| H. Configuration | P2: no daemon twin of the install floor; 4G default on a small slice | — | Per-pass suspension (T31); install renders `off` on a small slice. |

**Non-convergence check.** Round 1 claimed that the lane "never takes main ledger room by construction".
Family B's P1 is a blocker of that class, so the approach was re-evaluated rather than patched. The
recurring family across both designs is "lane state reaches main's room, or total bookings pass today's
ceiling". The base design that makes it impossible: every grant passes today's fit, and main also passes
a ledger-only room on `E - x`. This is the second base design for AIRA-350. If round 2 finds a blocker in
family A or B, the right next step is owner re-scoping, not a third patch.
