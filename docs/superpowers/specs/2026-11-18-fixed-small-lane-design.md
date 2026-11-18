# Fixed small-job lane (AIRA-350)

Status: plan, round 3. **Not ready for the plan gate: blocked on the owner's answer to Q1 (§12).**
Round 2's non-convergence tripwire fired (§14), so round 3 states the fork the reviews keep finding,
re-bases the accounting on the reviewer's derived form, and costs each branch in the same units.
Ticket AIRA-350, v0.46. Correctness-critical: two-loop, Opus builds, Fable reviews.

**Owner decision, 2026-11-18, verbatim:** "let's go back to a fixed slice that is separate to the
main slice reservation. Its max size is always fixed. The main slice reduces its admission ceiling
based on available RAM as it currently already does." Earlier wording: "in addition to the main
slice, we have a fixed slice of x GB (4 or 8), which jobs requesting <= y GB (1 GB) can be admitted
to ... smaller jobs hit the separate small-job slice by default, unless it's full, and then they hit
the main slice. This should only be in default mode, not in CI mode."

It replaces the withdrawn dynamic lane (`2026-11-18-small-job-lane-design.md`), whose evidence it
reuses. **Reading of the owner's words (round 3).** "Always fixed" means the lane admits whenever its own
room, a CPU slot and real memory allow. "As it currently already does" means the main pool keeps
today's ceiling. Together they put the lane on top of today's ceiling: row A of §2.0. Round 2 made
the other choice (the lane out of the ceiling, closing when the ceiling falls) the default, which
reversed both clauses. Row A is the provisional default. Its host exposure (§2.3) is the reason the
gate waits for the owner to confirm it.

Code references: worktree at `45f68afd` (master). Data (in `~/tmp/aira-lane2-store/`, not committed:
they name other sessions' jobs): a store copy (21:12 EET), a `confine --list --json` snapshot (21:20)
and a journal capture (2026-11-15 00:00 to 2026-11-18 21:12). Committed: `docs/dev/aira-350-` +
`fixed-lane-replay`, `lane-sim`, `ceiling-drop` (`.py`, `.out.txt`). M measured, E estimate, R reported.

## 0. The greenfield minimum, and the gap

**The problem in one line.** Small jobs wait minutes while the main ledger is full (over-booked) or
frozen behind a head that does not fit.

**Not addressed:** with every CPU slot held nothing is admitted. The incident (32 jobs on 32 slots, R)
was partly slot-bound: a small job fitted RAM, CPU and VRAM at 13:22:22 (withdrawn spec, timeline).

**The bare minimum.** A second booking pool of fixed size `x`, from which a job-level waiter booking
at most `y` is granted before the head rule whenever it has room: derived sums, one more term on main
grants, one evaluator branch. Where its room comes from is the one decision (§2.0).

| Addition beyond the minimum | Failure it closes | Size |
|---|---|---|
| Lane occupancy at the cap (`reserve` + 64 MiB); main headroom from main jobs only | At the discounted charge lane real use could exceed `x`; each lane job would add 64 MiB to main's headroom (`admit.go:1041-1051`). | units |
| Overflow charged to main, `max(0, S - x)` (§2.1) | Without it the lane's accounting could exceed `x` after a restart or an overflow. | one term |
| A lane grant leaves the head's CPU slots and VRAM (§2.2) | CPU and VRAM are one shared ledger, so a lane grant could take the slot a head waits for. | two conjuncts |
| The kernel-cap conjunct `K` (§2.2) | Without it a fixed lane could push total bookings past the slice's `memory.max`, so an OOM could hit the slice instead of one scope. | one conjunct |
| Suite parents and unsigned jobs not eligible; a lease with a live sub-reservation not small | A small parent brings exempt workers past the head: reported as 13 worker scopes totalling 28 GiB (R). | one frozen boolean, one set |
| Closed behind a pushed head (§12 Q2) | AIRA-309's owner decision: only another push passes a pushed head. | one conjunct |
| Off in ci-shim mode, set at install, suspended on a tiny slice (§2.5) | Owner decisions; a main pool too small to admit anything. | the pin-charge pattern |
| Reply fields and one journal line (§2.6) | Without them "lane full" cannot be told from "held behind the head", and the yield cannot be checked. | optional fields |

**Not built:** a second cgroup slice (§2.3), any stored pool flag or incremented counter, eviction,
a lane-internal head rule, a lane in ci-shim mode. §10 lists the cuts.

## 1. Problem and evidence

- **The symptom** (owner): "a long queue of small reservation requests while at 37 GB RAM usage".
- **The incident** (R, withdrawn spec §1.2): 2026-11-18 about 13:20 EET, 32 jobs booked 41.5-45.4 GiB
  of a ~45 GiB ceiling (real peaks ~22 GiB); 13-14 jobs of 0.07-1 GiB waited 4-6 min behind a 4 GiB head.
- **The same shape later** (M, snapshot 21:20 EET): 43.95 GiB granted of a 44.56 GiB ceiling, 26 jobs,
  2 queued, freeze phase `hold`, real use 11.03 GiB.
- **Small jobs' share** (M, replay §0, 82.3 h): 9281 retained small rows waited 128.2 h in total; 2660
  large rows waited 83.7 h.
- **The ceiling** (M, `aira-350-ceiling-drop.out.txt`, 93.2 h): 4731 throttled samples (84.6% of the
  minutes; what the other minutes were, static-bound, warm-up or unlogged, is not established), all
  `bound-by=system-pressure`; effective 38.0-52.5 GiB, median 48.75. MemAvailable never went below
  29.44 GiB (p1 36.41). The daemon started 10 times.

## 2. Design

### 2.0 The fork

**Three properties cannot all hold while the ceiling `E` can fall:**
- **F (fixed):** a lane grant needs only lane room, a CPU slot, VRAM and real memory;
- **H (host as today):** at every grant, bookings plus headroom stay within today's `E`;
- **N (no head starvation through the lane):** the main pool's room does not depend on lane state.

*Proof.* By N, main's room depends only on `E` and main's own bookings. Take an instant when main has
booked all of it and the lane is empty. By F the lane can then take `x`, so by H main's room is at
most `E - x`. Now let `E` fall by more than `x` less the lane's occupancy while those leases run
(leases are never evicted): F still grants the lane and H is broken. ∎ The fall is not rare: `E` sat
at least 2.88 GiB below its trailing 10-minute peak in 10.9% of throttled samples and at least 4 GiB
below it in 2.5% (M, `aira-350-ceiling-drop.out.txt`; 18.2% and 6.0% against a 60-minute peak).
Dropping N is the withdrawn design. So the owner chooses which of F and H to relax, and by how much,
through two constants fixed in the spec (not install options):
- **`a`**, the share of `x` on top of today's ceiling (`0 <= a <= x`); the rest comes out of main;
- **`G`**, the lane's host gate. `yield` means today's whole-slice fit `W`; `fixed` means real memory
  plus the kernel cap.

| Row | `a` | `G` | Owner's words | Relaxes |
|---|---|---|---|---|
| **A (provisional default)** | `x` | fixed | both clauses | H, by `x` (§2.3) |
| B (round 2) | 0 | yield | neither | F: the lane shuts when `E` falls after main filled |
| C | 0 | fixed | "always fixed" only | H, only after a fall: the lane can keep re-booking up to `x` that today would drain |
| D (hybrid) | `x/2` | fixed | "always fixed"; main loses `x/2` | H, by `x/2` |

Row C also gives an always-open lane (partition, `W` dropped from the lane gate), against round 2's
finding 2. Costs: §2.3 and §12.

### 2.1 Data model: one queue, one ledger, derived pools

A partition of the one queue and ledger (AIRA-228), not a second cgroup (§2.3). **No pool is stored
on a lease.** Every pass derives it from fields the ARDR frame carries, so a restart gives the same
pools as before.

- **Small lease** (accounting): granted and accounted, `scopeID != ""`, `parentScopeID == ""`,
  `reserve <= laneReserveMax`, and **no granted lease names it as its parent**. These read only the
  frame's `ScopeID`, `RAMBytes` and `ParentScopeID` (`internal/redeclare/redeclare.go:74-79`), and the
  re-declared RAM is the pin, not a discounted charge (`internal/runner/lease_keeper_linux.go:88-113`,
  `admission_linux.go:162`). Exclusive and scope-less leases never re-declare (`lease_keeper_linux.go:94-98`)
  and leave both pools on a restart, as they leave today's ledger. A small lease granted in main
  (overflow, unsigned, exclusive, a parent between workers) is small too: accounting cannot ask how.
- **`admitWaiter.laneKind bool`.** Frozen at enqueue in the waiter literal (`admit.go:2418`):
  `request.signature != "" && !strings.HasPrefix(request.signature, runner.AitestParentSignaturePrefix)`.
  It is read only for eligibility. Every `--delegate-ram` parent with a signature carries the prefix
  (`internal/runner/confine_linux.go:692-694`), so a suite parent is never eligible, signed or not.
- **Derived per pass**, by one `queue.rederiveLocked()` that replaces the five `rederiveLedgerLocked`
  assignments (`admit.go:2330`, `2374`, `2709`, `3083`; `confine_resize.go:143`):
  - `S` is the sum of `reserve + 64 MiB` over small leases, `J_s` their count and `S_c` the sum of their
    `ledgerCharge()`;
  - `B_big = outstanding - S_c` and `J_big = outstandingJobs - J_s`.
- **Per-pass quantities** under `queue.mu`. `H(n) = admitSliceHeadroom(n)`, `E` is today's effective
  maximum (`admit.go:2561-2564`, at most `memory.max`, `sliceceiling.go:752-758`) and `M` is this pass's
  `memory.max`.
  - Main ceiling: `E_m = min(E - x + a, M - x)`.
  - Whole-slice ceiling: `E_w = E_m + x = min(E + a, M)`.
  - **`P`**, main's ledger room: `ledgerAvailable(E_m, B_big + max(0, S - x), H(J_big + 1))`
    (`admit.go:2931`). The lane holds `min(S, x)`, and main is charged the rest.
  - **`W`**, the whole-slice fit `checkedAvailable(current, E_w, reclaimable, outstanding, H(J + 1))`
    (`admit.go:2906`), and `physicalFree_w`, the same with outstanding 0; with `a = 0` these are
    today's `available` and `physicalFree` (`admit.go:2661`). `J` is `outstandingJobs`.
  - **`K`**, the kernel cap: `ledgerAvailable(M, outstanding, H(J + 1))`.
- **`laneOn`** per pass: `x > 0 && !s.shimMode() && M - x > sliceCeilingUsableFloor()`
  (`sliceceiling.go:299`). While it is false, everything is today's and small leases are ordinary.
- **`laneReserveMax`**: `y`, or in pin-charge enforce mode `largestDiscountedPin(y, pinChargePercent)`
  (`pin_charge.go:147`).

**Readers of the whole-slice fields** (T26 pins the list; T30 drives the non-evaluator rows):

| Site | Today | With the lane on |
|---|---|---|
| `sliceProvablyEmpty` (`admit.go:586-588`), residuals (`admit.go:1284-1290`) | whole slice | unchanged |
| evaluator RAM room (`admit.go:2622-2663`) | `available` | main waiter `min(W, P)`; lane grant per §2.2 |
| discounted physical rule (`admit.go:2659-2666`) | `physicalFree` | `physicalFree_w`, both pools |
| head eligibility (`admit.go:2770`, `2843`) | `E` | `E_m` |
| too large: enqueue (`admit.go:2386`), `admitConnection` (`admit.go:1891-1898`), `admitCeiling` (`admit.go:1490`) | `maximum`, all jobs | `M - x`, `J_big`; a reserve `<= laneReserveMax` is never too large |
| worker probe (`worker_admit.go:244-254`); claims go through the evaluator | `available` | `min(W, P)` |
| `CeilingBytes` (`confine_manage.go:261`) | `E - H(J+1)` | `E_m - H(J_big + 1)` |
| `CeilingWouldBeBytes` (`confine_manage.go:270`, `sliceceiling.go:795`) | the same less `H(J+1)` | headroom argument `H(J_big + 1) + x - a`, so it equals `CeilingBytes` in enforce mode; `ceiling_effective_bytes` and the other `ceiling_*` stay whole-slice |
| `GrantedBytes`, `Jobs`, `ScopeJobs`, `ScopeBytes` (`confine_manage.go:242-288`) | whole slice | main: `B_big + max(0, S - x)`, `J_big`; the lane in its own fields (§2.6) |
| clients of `CeilingBytes` (push hint `confine_queue_position_linux.go:216`, "N of M granted" `:239-240`, own-size note `:259-261`, exclusive wait `:456-457`, `--list` `cmd/aira/main.go:3752`, `3760`, drain warning `main.go:3902`, `aira top` `cmd/aira/tui_top.go:1528-1532`) | — | unchanged code, now main against main; the own-size note is not printed for a reserve `<= lane_job_max_bytes` while the lane is on |
| `ci_dump.go:196`, `205` | whole slice | unchanged (lane off in shim) |

### 2.2 The admission rule

In `evaluateAdmitQueue` (`admit.go:2472`), after the restart freeze (`admit.go:2592`) and the
exclusivity gate (`admit.go:2604`), before the head `switch` (`admit.go:2688`):

```
if laneOn && laneEligible(w) {
    if laneFits(w, head) { grant as admit.go:2700-2724; no head.bypassed, no budgetWithheld,
                           no anchor write; log the lane-grant line; continue }
    record laneFull or laneShut (diagnosis only)
}
available := W; if laneOn { available = min(W, P) }       // the main path, as today
... today's code from admit.go:2663 on, with `available`, physicalFree_w and, for the head, E_m;
    the RAM fit is `charge <= W` and the after-grant main fit (below)
```

**Main fit is judged on the state after the grant:** `B_big' + max(0, S' - x) + H(J_big') <= E_m`,
the primed sums derived as if `w` were granted (computed directly, not by a trial walk). For an
ordinary waiter this is today's `w.ledgerCharge() <= P`. An overflowing small waiter is charged only
its part above `x`. A sub-reservation whose parent is small with no other live child also carries
the parent's move out of `S`. Without that term a suite's first worker would put its parent in main
unchecked, and the freed lane room could be re-booked past `E_w`. With it, a worker's condition
equals today's (`P` is larger by exactly the parent's booking), so no new wait or deadlock arises (T14g).

**`laneEligible(w)`:** `w.scopeID != "" && !w.isSubReservation() && !w.exclusive && w.laneKind &&
w.ledgerCharge() <= y`. A scope-less waiter has no kernel cap (`admit.go:3551`), so its real use is
not bounded by its reserve and §3's proof fails for it.

**`laneFits`** requires all of the following. `cpuH` and `vramH` are the head's `cpu` and `vram` when
this pass has found a head (`admit.go:2770-2779`), and 0 otherwise.
1. **Not behind a pushed head:** `head == nil || head.pushSeq == 0`.
2. **Lane room, at the cap:** `S + w.reserve + 64 MiB <= x`. Failing it sets `laneFull`.
3. **Host gate `G`.** Failing it sets `laneShut`.
   - `yield`: `w.ledgerCharge() <= W`, plus `w.reserve <= physicalFree_w` for a discounted waiter.
   - `fixed`: `w.reserve <= physicalFree_w` and `w.ledgerCharge() <= K`.
4. **CPU, leaving the head's:** `w.cpu + cpuH <= cpuAvailable(cpuCeiling, cpuOutstanding)`.
5. **VRAM, leaving the head's:** `w.vram == 0 || vramWaiterFitsLocked(w.vram + vramH, vramOutstanding)`.

**A waiter the lane refuses** goes on to the main path; a queued waiter is in no pool, so every pass
tries the lane first again (T19), past an unpushed head's budget and hold, backstop pass included,
and with no head too (T21). **Inside the lane:** first fit in evaluation order (`admitOrderLocked`,
`admit.go:452-476`), no lane-internal head rule (T20). **Overflow:** a small waiter granted in main
counts in `S`; while `S > x` main is charged `S - x`, so freed lane room goes first to running
overflow leases, not to new waiters (§6, replay §1h'). The bypass budget counts `w.ledgerCharge()`, as
today.

### 2.3 Host protection, by row

| Bound | A (`a = x`) | B | C | D (`a = x/2`) |
|---|---|---|---|---|
| Bookings + headroom at a main grant (`W`) | `<= min(E + x, M)` | `<= E` | `<= E` | `<= min(E + x/2, M)` |
| At a lane grant | `<= M` (`K`) | `<= E` (`W`) | `<= M` (`K`); total bookings `<=` the `E` at main's last grant | `<= M` |
| Worst-case MemAvailable, steady ceiling, every booking fully used: `freeMin + H(J) - a` (8 GiB, `H(J) = 2 GiB + 64 MiB x J`) | 6 GiB + 64 MiB x J: 7.6 GiB at J = 26, under the 8 GiB watchdog trip until J = 32 | 11.6 GiB at J = 26, as today | as B; after a fall of `d`, today's transient (as deep as `d`) can last while the lane re-books up to `min(d, x)` | 9.6 GiB at J = 26 |
| Static regime (machine term 62.5 GiB, `M` 64 GiB) | main capped at `M - x` = 60 GiB; total `<= M`; the host keeps 14.5 GiB, not 16 | as today | as today | main `<= min(E - 2, 60)` |
| Realistic low, MemAvailable over 93.2 h (M) less lane real peak 3.07 GiB (E, replay §1) | about 26 GiB | 29.44 GiB (M) | about 29 GiB | about 28 GiB |

- **The kernel cap is a prior decision.** "Keep `Σ reserve <= cap − headroom` so the slice-level OOM
  never fires and every kill is per-scope contained"
  (`docs/superpowers/reviews/2026-09-03-subprocess-slice-management-review.md:132`;
  `docs/superpowers/specs/2026-08-25-confine-estimate-reserve-design.md:23`). `K` and `E_w <= M` keep
  it in every row. In particular `K` refuses the lane in an ARDR deficit near `M`, which `W` used to
  do (T22b).
- **The watchdog** kills heavy processes outside AIRA below MemAvailable 8 GiB (`watchdog.go:24`). Row
  A's worst case lies in that band, the realistic low does not: the worst case needs every booking
  fully used at once, and real use runs at about a quarter of bookings (M, §1). The principle it breaks
  is the one `sliceceiling.go:319-325` encodes for `freeMin`; there is no prior decision about a lane.
- **Lane real use** is bounded by `S` (scope caps), and the pressure term does not move with the
  slice's own anon (`sliceceiling.go:224-230`), so lane growth does not lower `E`.
- **Row A's physical floor is `E + x`, not `E`.** Main's physical term is the whole slice's, on
  `E_w`. When real memory is short, main can therefore use up to `x` of physical room that today's
  rule would refuse; the `E + x` bound is unchanged. Putting main's physical term on `E` would let
  lane real use lower main's room (cut, §10).
- **Every reclassification is checked or bounded:** a parent's move into main by the after-grant fit (§2.2); a
  return to `S` only moves bookings; a late re-declare skips every gate, as today, and `K` bounds the
  lane refill it can free (CE11).
- **A ledger partition, not a second cgroup:** a real `aira-lane.slice` would make the client, `--list`,
  `--kill`, the reaper, `--resize`, the worker paths and the CPU and VRAM ledgers (`admit.go:742-757`)
  learn a second slice, for a cap the per-scope caps already give.

### 2.4 Interactions

| Interaction | Decision | Where / test |
|---|---|---|
| Pushed head (`--front`) | Lane closed behind it (rule 1). A pushed small waiter is evaluated first and takes the lane if it fits. | AIRA-309; Q2; T8 |
| `--exclusive` | The gate `continue`s before the lane (`admit.go:2604-2621`). Exclusive waiters are not eligible. A drain waits for lane leases (`sliceProvablyEmpty`). | T10 |
| `--priority high` | Order only; an unpushed head. | T9 |
| Fairness freeze, AIRA-59 backstop | Lane grants never arm, never count in `bypassed`, never set `budgetWithheld`. | T11, T12 |
| AIRA-324 | Eligibility on `ledgerCharge()` (the ticket's unit); occupancy on the cap. In observe mode, which `aira install` renders (`install.go:62`), the two are equal (`pin_charge.go:120-125`). A restart re-declares the pin, so a discounted charge returns at full reserve, as today (CE9). | T15 |
| aitest, `confine-reserve` | Workers and holds are sub-reservations: never eligible, booked in main, head-exempt as today (`admit.go:2675-2688`). A parent is not eligible (`laneKind`), and while it has a live sub-reservation it is not small. | T14 |
| Restart (ARDR) | No rule: the establish (`admit.go:2343-2378`) and the SET (`admit.go:2318`) are unchanged, and the next re-derive classifies them. The same lease set gives the same `S`, `B_big` and `J_s`. | T17 |
| `confine --resize` | Lowers `charge` only (`confine_resize.go:139`); `S` counts the cap, so lane room and `P` are unchanged. | T16 |
| Long-lived lane jobs | First come, no eviction. Measured: 5 small job-level scopes about 128 h old, 1.56 GiB of footprint (M, replay §3). | §6 |
| Overflowed waiter, lane room later | A queued waiter takes the lane on the next pass. A granted overflow lease is not moved, but its charge to main falls as `S - x` falls. | T19 |
| Restart freeze; ci-shim | Frozen waiters `continue` before the lane (`admit.go:2592-2595`). Shim: Serve forces `x = 0` and `laneOn` checks `!s.shimMode()` (`shim.go:37`). | T27, T23 |

**Deviations from the brief.** (1) Item 5's per-job pool: pools are derived in aggregate, so a row
shows `small: true` and the summary `min(S, x)` in the lane, `max(0, S - x)` charged to main. Rows by
first fit in sequence order were rejected: a release could then push a later lease into main and lower
`P`. (2) "A granted main lease never moves": an overflow lease's booking moves from main's charge to
the lane as room frees. Only accounting moves; nothing is evicted.

### 2.5 Configuration (the pin-charge pattern)

**Daemon** (`internal/daemon/small_lane.go`, beside `pinChargeConfigFromEnv`, `pin_charge.go:53-90`):
- `AIRA_DAEMON_SMALL_LANE`: `x`. Unset, empty or `off` is off. Otherwise a size (`runner.ParseMemorySize`,
  `internal/runner/memory_size.go:82`) from `y + 64M` to `8G`, else `E_CONFIG_INVALID`.
- `AIRA_DAEMON_SMALL_LANE_JOB_MAX`: `y`, read only when the lane is on, default 1G, from 1M
  (`runner.MinPinnedScopeCap`) to `x - 64M`, else `E_CONFIG_INVALID`. No new error code.
- Serve reads both beside the pin-charge read (`server.go:416-429`) and logs `x`, `y` and the row at
  startup; `a` and `G` are build constants. **Suspension:** a pass with `M - x <=
  sliceCeilingUsableFloor()` is a lane-off pass, one journal line per transition.

**Install** copies each pin-charge site, as round 2: `--small-lane SIZE|off`, `--small-lane-job-max SIZE`
(`install.go:154-158`, `471-492`, `640-650`, `695-706`, `890-895`, `1632`, `2507-2508`; `mode.go:321`;
`core.go:2241`), refused with `E_INSTALL_ARGUMENT_INVALID`, ship defaults `4G`/`1G`. Where `MemoryMax - x
<= sliceCeilingUsableFloor()` (`install.go:68` allows 4 GiB) the default is `off`; an explicit value is refused.

### 2.6 Observability (minimal)

- **`confine --list` reply** (`runner.ConfineSliceReserve`, `confine_manage.go:586`; filled in
  `admitSliceSnapshotFor`, `admit.go:1302`, only while `laneOn`): `lane_capacity_bytes` (`x`),
  `lane_used_bytes` (`min(S, x)`), `lane_over_bytes` (`max(0, S - x)`), `lane_jobs` (`J_s`),
  `lane_job_max_bytes` (`y`). The summary's ceiling, granted and job fields describe the main pool;
  residuals stay whole-slice, so `jobs = scope_jobs + reservation_jobs + residual` holds. Per scope
  row: `small: true` (omitempty).
- **For the named queued scope** (the AIRA-24 probe, `admit.go:1352-1364`): `lane_full`, `lane_shut`
  and `held_by_head`, diagnosis-only, written beside `noteGrantableLocked` (`admit.go:2760`) every
  pass; no decision reads them (`admit.go:261-280`).
- **Text:** `small-job lane: 3.0G of 4G used, 3 jobs (jobs booking at most 1G)`, plus `, 0.4G over,
  counted in the main pool` when `S > x`, after `slice queue:` (`cmd/aira/main.go:3777`). The wait line
  (`confine_queue_position_linux.go:151`) appends `, small-job lane full`, `, small-job lane closed: no
  room in the slice` or `, fits but held behind the head of the queue`.
- **Journal:** one line per lane grant (about 110 an hour, M): `aira daemon: small-lane grant seq=<n>
  reserve=<bytes> waited=<ms> lane=<used>/<cap> jobs=<n>`.

### 2.7 Generated text (faces)

The AIRA-340 pattern (`internal/core/skill.go:305-318`): `ConfineSmallLaneGuidance` (skill at
`skill.go:338`, install help) and `ConfineSmallLaneSummary` (`confine` summary `core.go:2607`, MCP
instructions `cmd/aira/mcp.go:29`), keywords pinned by `TestConfineSmallLaneGuidanceIsOnEveryFace`
(`cmd/aira/confine_priority_test.go:100`). Long draft: "Small-job lane (default on, not in CI mode): a
job booking at most 1G is admitted first from a fixed 4G lane, without waiting behind larger queued
jobs; when it is full the job queues for the main pool. It still needs a CPU slot. Keep small jobs
small; give a server an honest small reserve. `--delegate-ram` suites never use the lane." Short:
"Jobs booking at most 1G use a separate small-job lane first, so keep short commands' reserves small."

## 3. Invariants

1. **Lane accounting `min(S, x) <= x` always**, restart included. Lane grants keep `S <= x` (rule 2).
   `S` can pass `x` only through main-path grants of small waiters and accounting changes such as a
   parent's last child ending; main is charged the excess.
2. **Host bounds by row** (§2.3): at every main grant `outstanding + H(outstandingJobs) <= E_w`; at
   every lane grant the same under `yield`, and `<= M` under `fixed`. Only a re-declare passes `M`
   minus headroom, as today (CE11).
3. **`P` reads lane state only through `max(0, S - x)`**, which a lane grant leaves at 0.
4. **Head independence.** While `S <= x` and the slice's real use outside small scopes is at most
   `B_big`, a main waiter's RAM room is exactly `P`. No lane grant, release (observe mode), real-use
   growth or resize lowers it; main grants lower it, as today, each within its after-grant fit.
5. **Head CPU and VRAM.** A lane grant past head `H` leaves `H`'s slots and VRAM free, and never changes
   `H.bypassed`, `budgetWithheld` or `freezeArmedAt`.
6. **AIRA-307's bound** on main grants past an unpushed head holds unchanged (`admit.go:485-490`).
7. **No lane grant behind a pushed head.**
8. **Off is today.** `laneOn` false (unset, `off`, ci-shim, suspended) gives `available = W` on `E`
   and head eligibility on `E`; every existing test passes unchanged.
9. **Precedence:** restart freeze, exclusivity gate, lane, head rule.
10. **Restart neutrality.** The pools are a function of the lease set's frame fields, so re-declaring
    the same leases, in any order, gives the same `S`, `B_big` and `P`.

**Proof of 4.** `H(J+1) - H(J_big+1) = 64 MiB x J_s` and `E_w = E_m + x`, so
`W - P = x - 64 MiB x J_s + B_big - max(effectiveCurrent, B_big + S_c)`. Small scopes use at most their
reserves, since each has a scope and its `memory.max` is its reserve. The rest of the slice uses at
most `B_big` (the assumption), and `S_c` is at most the sum of reserves. So the max is at most
`B_big + S - 64 MiB x J_s`, and `W - P >= x - S >= 0`. Then `min(W, P) = P`. A lane grant needs
`S + f <= x`, so `max(0, S - x)` stays 0. `E` does not move with lane growth (§2.3). ∎

**Counterexample hunt, and what remains.** The head faces `E_m`: `E - x` in rows B and C, `E` in row
A (below `M - x`). Each residual below is pinned by a test.
- **CE1-CE3** (T13, T12, T3b): small jobs that spent `H`'s budget now use the lane, so a mid-size job
  can pass `H` within it; lane grants do not arm, so holds arm at other instants (the sim's W3 head
  starves in 29 of 30 phases today, 30 of 30 under every lane design); lane jobs granted before `H`
  became the head keep their slots.
- **CE4, main over its own booking** (T29): if non-small real use exceeds `B_big` by more than `x - S`
  plus the small scopes' unused cap, `W` binds and lane real use lowers `H`'s room. Possible only with
  discounted main leases (enforce, not installed) or use that no booking covers.
- **CE5, overflow first** (T19b): while `S > x`, freed room goes to running overflow leases. It is
  measured (§6). At rollout the running small leases count at once: at most 12 scopes, 6.90 GiB at the
  snapshot (M, replay §3). The lane stays closed until enough end; main is charged the excess, so its
  room is today's (rows B, C) or more (row A).
- **CE6, hand-made sub-reservations** (T14f): `AIRA_CONFINE_SCOPE_ID` reaches every confined job
  (`confine_linux.go:1250`; `confine_reserve_linux.go:117`) and worker-admit accepts any canonical
  parent (`worker_admit.go:411-441`), so a lane job running `aira confine-reserve` by hand gets
  head-exempt holds in main (and stops being small). The worker-admit command and aitest coordinates
  go only to `--delegate-ram` launches (`confine_linux.go:1232-1237`, `1256`), and nothing in this
  repository calls `confine-reserve` otherwise. Today any job that passed the head within its budget
  has the same path.
- **CE7, the partition's cost** (T22d, rows B, C, D): a head that fits today's room but not `P` waits
  for the ceiling or for main leases to end. In the sim's W12 the head is never granted under the
  partition while 1.5 GiB of never-ending main leases stand (210 s today).
- **CE8, CE9:** while suspended, small leases are ordinary (T31); a pin-charge mode change across a
  restart can move a discounted lease above `y` out of `S` (documented, not tested). In enforce mode
  (not installed), a discounted parent returning to `S` while `S > x` raises main's charge by its
  discount, so `P` can fall on a child's release; T25 runs in observe mode.
- **CE11, late re-declares** (T17b): a re-declare skips every gate today. One arriving after the
  restart freeze (a worker, or a SET with a changed reserve) can move a lease out of `S` unchecked, and
  the freed lane room can then be re-booked. The overshoot is at most that lease's footprint, and `K`
  keeps total bookings within `M`.
- **CE10, ceiling falls** (T33): in row B, after main fills and `E` falls by more than `x - S`, the lane
  is shut (`laneShut`) with lane room free, until main drains. In rows A, C and D it stays open, and
  bookings can sit above the fallen ceiling (§2.3).

## 4. Wire and protocol

- **ProtocolVersion stays 15** (`internal/daemon/protocol.go:218`). The admit request, the grant frame
  and the ARDR frame are unchanged.
- **The `confine-list` reply gains optional fields** (§2.6), decoded with `json.Unmarshal`
  (`confine_queue_position_linux.go:388`); an old client ignores them.
- **Meaning change while the lane is on:** the summary's ceiling, granted and job fields describe the
  main pool. The release notes say so.

## 5. Tests (each must fail against the named wrong implementation)

**Common settings** on the AIRA-228/307 seam (`queueOf`, `admit_queue_order_test.go:41`), driving
`evaluateAdmitQueue`: production headroom (2 GiB + 64 MiB a job; `admitTestServer` zeroes it, so the
helper restores it); 32 CPU slots, held leases at `cpu = 1` (`heldLease` uses 0,
`admit_queue_order_test.go:37`); `memory.max` 65536, `x` 4096, `y` 1024, real use 12288, a hold pass
(`freezeArmedAt = now - 60 s`, `maxHold` 5 min), the head's budget spent, `E` 49152; "main leases"
reserve above `y`. All sizes in MiB. Every test asserts the granted set and each lease's `small`
classification. **Rows:** written for rows B and C; for A and D each test raises main's bookings by
`a`, which leaves `P` and `W` unchanged, so ledger assertions carry over. Physical and kernel-cap
assertions (T5, T22, T33) state each row; head eligibility reads `E_m`.

| Test | Setup | Assertions | Wrong implementation it fails |
|---|---|---|---|
| **T1 incident, RAM side** | 20 main leases, `B_big` 38182, so `P = 45056 - 3392 - 38182 = 3482`. A 4096 head. 14 small: 72, 256, 512, 1024, 512, 256, 1024, 72, 512, 1024, 256, 512, 72, 1024. | Lane grants 1-6, 8, 9, 11; `S` 4048. 7, 10, 12, 13, 14 `laneFull` and `heldByHead`; `lastGrantable` 3482. `bypassed` unchanged. | Lane off: none. No 64 MiB: 1-8, 11, 13. Lane jobs in main headroom: 2906. Main on `W` alone: 3530. Main on `E` in rows B/C: 7578. Grants in `bypassed`. |
| **T1b incident, slots** | T1 with 32 of 32 slots held. | No grant. | Lane without rule 4. |
| **T2 main pool full** | 22 main leases, `B_big` 41400 (`P` 136). Four 1024 small. | Three lane grants (`W` 4232, 3144, 2056 before each). The fourth is `laneFull` and becomes the head. | Lane judged on `P`: none. |
| **T3 CPU** | `E` 48640; 28 main leases, `B_big` 38347 (`P` 2293), 28 of 32 slots. A 5120 head. Then 2154, 2700, 1024, 256, 512, 512, 512, 1024. | Lane: 1024, 256, first 512 (31 slots). The rest of the small ones `heldByHead`; 2154 held; 2700 refused. | No `cpuH`: a fourth grant. |
| **T3b CE3** | 30 main + 2 small leases (granted before any head) hold 32 slots. A 5120 head refused on `P`; a 256 small. One small lease ends. | The small waiter is not granted, before or after. | No `cpuH`. |
| **T4 lane growth** | Main 38784 permanent, 1024 released at t=10. A 4096 head. A never-ending 1024 small waiter. At t=10 real use is main 38784 plus lane 1024 (both at their caps), and in a twin run lane 0. | t=0: small in the lane; head refused, `P` 3008. t=10: head granted in both runs (`P` 4096). With a 4097 head instead: refused in both, `lastGrantable` 4096. | Main on `checkedAvailable(current, E_m, ...)`: 3072. Small in main's ledger: refused. Lane in main headroom: 4032. |
| **T5 host gate** | No head. (a) `W` 1023, then 1024, physical room ample. (b) Physical room 1023, then 1024, ledger ample. A 1024 small waiter. | (a) `yield`: refused, then granted; `fixed`: granted both times. (b) Refused, then granted, under both. | `G` swapped; `<` for `<=`; no physical conjunct under `fixed`. |
| **T6 large head, lane open** | `J_big` 10, `B_big` 38208 (`P` 4096), real use 38208. An 8192 head refused. One 1024 small. | Granted in the lane. | A physical reservation for the head: refused. |
| **T7 VRAM** | Head needs 4096 VRAM, refused on it; 5120 free. Small waiters needing 2048 and 0 VRAM. | Only the second in the lane. | No `vramH`. |
| **T8 pushed** | A pushed head refused, a small waiter behind it; separately a pushed small waiter. | First held (`heldByHead`); second in the lane. | Lane before rule 1. |
| **T9 high** | An unpushed `high` head refused; small waiters. | In the lane, head's slot kept. | High treated as pushed. |
| **T10 exclusivity** | (a) Exclusive holder granted, a small waiter. (b) A drain queued while a small lease runs. | (a) Not granted. (b) Drain granted only after the release. | (a) Lane before the gate. (b) Emptiness on main jobs. |
| **T11 backstop** | `freezeArmedAt = now - maxHold`; an eligible small waiter. | In the lane; `bypassed` unchanged. | Lane after the head `switch`. |
| **T12 CE2** | Idle phase, budget spent. Pass 1: two small waiters, lane on; twin lane off. Pass 2 (lane on): lane full, a small waiter that fits `P`. | Lane on: pass 1 no arm, pass 2 arms. Lane off: pass 1 arms. | An anchor write on a lane grant. |
| **T13 CE1** | `J_big` 10, `B_big` 34112 (`P` 8192; lane off `W` 12288). A 16384 head, `bypassed` 0. Four 1024 small, then a 5120. | Lane on: three in the lane, the fourth and 5120 via the budget (`bypassed` 6144). Lane off: four via the budget, 5120 withheld. | Pinned residual. |
| **T14 eligibility** | (a) A 512 sub-reservation. (b) `signature = AitestParentSignaturePrefix + "x"`, 1024, behind a held head. (c) Plain signature. (d) Empty signature. (e) Scope-less, parentless 512. (f) CE6: a lane lease, then a 512 sub-reservation naming it, behind a held head. | (a) Main, not small. (b) Held; once granted on the main path with no live child, small. (c) Lane. (d) Not eligible. (e) Main, not small. (f) The child is granted head-exempt in main, and the parent leaves `S` while it lives (`S` down 1088; `B_big` up 1024 plus the child's 512). | (a) Ignoring `parentScopeID`. (b) Eligibility without the prefix. (d) Eligibility without `signature != ""`. (e) Ignoring `scopeID`. (f) A small predicate without the child clause. |
| **T14g parent move** | Row C. `J_big` 20, `B_big` 41152 (`P` 512); `S` 4096 over four small leases, one a childless 1024 parent. A 512 worker naming it, then a 1024 lane waiter. Twin: `B_big` 40064 (`P` 1600). | Worker refused (main after the grant 42688 + 3456 > 45056); the lane waiter `laneFull`. Twin: the worker is granted, then the lane waiter, and bookings plus headroom end at 49152 = `E_w`. | No parent term: the worker is granted, `P` goes to -1152, and the refill puts bookings plus headroom at 50240. |
| **T15 AIRA-324** | Enforce, 80%. (a) Pin 1228 booked at 983. (b) Pin 1024 booked at 820. | (a) In the lane, `S` 1292. (b) In the lane, `S` 1088. | Eligibility on `reserve`: (a) not in the lane. Occupancy on charge: 1047, 884. |
| **T16 resize** | A 1024 small lease resized to 256. | `S`, `B_big`, `P` unchanged; `outstanding` down 768. | Occupancy on charge. |
| **T17 restart neutrality** | A lease set with two lane grants, an overflowed small lease, a small parent with a worker, an exclusive small lease and three main leases. Re-derive; then drop every lease and re-establish the scope-bearing, non-exclusive ones (`enqueueReDeclare`) in each of several orders. | `S`, `J_s`, `B_big`, `P` identical across orders. They equal the pre-restart values less the exclusive lease, which also leaves `outstanding` (as today). (b) CE11: the worker re-declared after the freeze: `P` falls by the parent's charge plus 64, and a lane refill stays within `K`. | Any stored pool flag; classification by arrival order or room. (b) No `K`. |
| **T19 overflow retry** | Pass 1: `S` 4048, a 1024 small waiter held. A small lease ends. | Pass 2: in the lane. | A pool fixed at first evaluation. |
| **T19b CE5** | `S` 4500 (an overflow lease of 1088 running). A small lease of 576 ends; a 1024 small waiter. | Not granted (`S` 3924 + 1088 > 4096); main's charge `max(0, S - x)` falls from 404 to 0, so `P` rises by 404. | Freed room to the new waiter first. |
| **T20 first fit** | Lane residue 600; waiters 1024 then 256. | 256 in the lane; 1024 to main. | A lane head-of-line rule. |
| **T21 lane first** | No head, ample room, one small waiter. | Lane; `B_big` unchanged. | Main first. |
| **T22 host** | (a) A 59392 waiter; on `memory.max` 8192 with 25 main jobs, a 1024 waiter. (b) A re-declared 64512 main lease, three 1024 small. (c) `E` 45056, main at `P = 0`, small waiters until the lane is full. (d) CE7: real use 1000, 2000 of main leases, a 42000 head. | (a) The first refused too large (`> 65536 - 4096 - 2112`); the second not. (b) None in the lane, `W < 0` (`yield`), `K < 0` (`fixed`). (c) After every grant, bookings plus headroom `<= E_w`. (d) Rows B, C: head refused (`P` 40880). | (a) No `M - x`; small reserves not exempt. (b) No `K` under `fixed`. (d) Main on `W` alone. |
| **T23 ci-shim** | A shim server with the lane fields set. | No lane grant. | Without `!shimMode()`. |
| **T24 off is today** | `x = 0`; a table of inputs, degenerate ones included. | `available`, `physicalFree`, grants equal today's; the existing suite passes. | Any leak when off. |
| **T25 derived and bounded** | Observe mode; random grant, release, establish, SET, resize, child grant and child end. | Sums equal a fresh walk. After every lane grant `S <= x`. While `S > x` no lane grant occurs. Invariant 2 holds after every grant. `P` never falls on a lane grant or a release. | An incremented counter; `P` lowered by a release. |
| **T26 guards** | Syntactic allow-lists on the `admit_pin_charge_guard_test.go` pattern. | Every selector of `outstanding`, `outstandingJobs` and the derived sums is classified (§2.1). No decision reads `laneFull`, `laneShut`, `heldByHead` or `signature`. | A new unclassified reader. |
| **T27 restart freeze** | Freeze active, empty lane, an eligible waiter. | Not granted; `waited` set. | Lane before the freeze gate. |
| **T28 boundaries** | (a) `S + f == x`, then `x` 1 MiB smaller. (b) `w.cpu + cpuH == cpuAvailable`. (c) A main waiter at `min(W, P)`, then 1 MiB over. | (a) Granted; `laneFull`. (b) Granted. (c) Granted; refused. | `<` for `<=`. |
| **T29 CE4** | Main 38784 with real use 42000; a small server 1024; a 4096 head. Lane real use 1024, then 0. | Refused at 1024 (`W` 3888); granted at 0 (`W` 4912). | Pinned residual. |
| **T30 readers** | A worker probe and claim, `admitCeiling`, the enqueue and `admitConnection` too-large checks, with small leases present. | Probe `AvailableBytes = min(W, P)`; claim granted iff it fits; ceilings on `M - x` and `J_big`. | A site left on whole-slice values. |
| **T31 suspension** | `memory.max` such that `M - x <= floor`, one small lease. | No lane grant; `available = W`; one transition line. | No suspension. |
| **T32 client** | Reply with the lane on: a 3 GiB job facing a 2.5 GiB `ceiling_bytes`; a 1 GiB job facing 0.8 GiB; small leases with main below its ceiling; enforce mode. | First: no push hint, own-size note. Second: no own-size note. Third: no drain warning. `ceiling_would_be_bytes == ceiling_bytes`. | A whole-slice summary field. |
| **T33 CE10 ceiling fall** | Main fills `P` at `E` 49152 (`B_big` 41664, 20 jobs). `E` falls to 45056. Lane empty; a 1024 small waiter. Then main leases totalling 4096 end. | `yield`: refused, `laneShut`, not `laneFull`; granted after the releases. `fixed`: granted at once. With `B_big` 63000: `fixed` refused on `K`. | `G` swapped; `laneShut` reported as `laneFull`; no `K`. |

**Configuration, install, observability:** every §2.5 value, refusal and site (env `off`, `4G`; refused
`0`, `9G`, `abc`, `x < y + 64M`; shim forces off); every §2.6 field, line and clause, present only
while on and absent otherwise; `TestConfineSmallLaneGuidanceIsOnEveryFace`.

**Mutations, by direction.** The gate runs each and records it RED with a committed
`docs/dev/aira-350-mutation-check.py`, on the `docs/dev/aira-324-mutation-check.py` pattern.
- **False pass:** no 64 MiB (T1); `G` swapped (T5, T33); no `K` (T22b, T33); no `cpuH` (T3, T3b, T1b);
  no `vramH` (T7); lane before rule 1 (T8), the gate (T10a) or the freeze (T27); eligibility ignoring
  `parentScopeID` (T14a), the prefix (T14b), the empty signature (T14d) or `scopeID` (T14e); occupancy
  on charge (T15, T16); main on `W` alone (T1, T22d); no `M - x` (T22a); on in shim (T23); emptiness on
  main jobs (T10b); off-by-one (T28); the main fit without the parent's move (T14g).
- **False fail:** lane deleted (T1, T2); lane in main headroom (T1, T4); small in main's ledger (T4);
  lane judged on `P` (T2); a head physical reservation (T6); high as pushed (T9); anchor write (T12);
  eligibility on `reserve` (T15a); a stored pool or order-dependent classification (T17); pool fixed
  at first evaluation (T19); freed room to new waiters first (T19b); lane head-of-line (T20); main
  first (T21); small reserves too large (T22a); `<` for `<=` (T5, T28); no suspension (T31); no child
  clause (T14f).
- **Moved:** lane after the head `switch` (T11); lane grants in `bypassed` (T1).
- **Pinned residuals, not mutations:** T13 (CE1), T29 (CE4), T14f (CE6), T22d (CE7).

## 6. Expected yield: estimates, not bounds

`docs/dev/aira-350-fixed-lane-replay.py` replays each retained confine job (arrival `started_at_ms -
wait_ms`, hold `wall_ms`, footprint `budget + 64 MiB`), lane first, first fit, until its historical
grant. **Simulated estimates** (E), gross (small jobs only). Round 3's rule is §1h': overflow counts
against `x` until it ends; suite parents with live workers do not.

| `x` | Small wait-h removed, of 128.2 (§1h') | With 1.56 GiB silting | Round 2's stored flag (§1, §1b) | Parents counted too (§1h) | Lane full (1 GiB) | Partition's main room lost, mean (p90) | Lane real p99 / peak (§1) |
|---|---|---|---|---|---|---|---|
| 2 GiB | 57.4 h (44.8%) | 6.1% | 60.5% / 16.5% | 28.0% / 3.5% | 58.1% | 1.03 (2.00) GiB | 1.32 / 1.73 GiB |
| 4 GiB | 99.1 h (77.3%) | 59.7% | 79.5% / 68.6% | 69.2% / 43.2% | 12.7% | 2.80 (4.00) GiB | 2.11 / 3.07 GiB |
| 8 GiB | 107.9 h (84.1%) | 84.0% | 84.1% / 84.0% | 84.1% / 83.0% | 0.1% | 6.77 (8.00) GiB | 2.61 / 4.71 GiB |

- **The derived form's cost** against round 2's stored flag: overflow first, 2.2 points at `x = 4`
  (8.9 with silting); counting parents would cost 10.3 (25.4), hence the child clause; nothing at
  `x = 8`. §1h' assumes a parent always has a live worker (overstated by its worker-less time).
- **CPU (§1e, round 2's per-lease model, not re-run with derived accounting):** the store misses about
  18-27 of 32 slots (5 retained rows running at the incident). At `x = 4` the saving falls to 79%,
  71%, 32% at allowances 20, 24, 27. **Range at `x = 4`: about 30-77% (the low end from that model),
  and 0 in any instant with all 32 slots held.**
- **The ceiling (`aira-350-ceiling-drop.out.txt`).** The replay has no ceiling series, and in row B a
  lane grant also needs `W`, which shuts once `E` falls below what main booked. With no booked-bytes
  series the journal bounds only the falls: `E` sat at least 2.88 GiB (`x` less a mean occupancy of about 1.1 GiB) below
  its trailing 10-minute peak in 10.9% of throttled samples (12.0% within 5 minutes after a hold line,
  a head waiting), and 18.2% (20.8%) against a 60-minute peak. **So row B's lane may be shut for up to
  about a fifth of the time a head waits (E), an unsized upward bias in its yield; A, C, D lack it.**
- **Other biases.** Upward: no CPU gate in §1-§1d and §1h, no reservation of the head's CPU and VRAM,
  retention thinning (186 signatures at the cap). Downward: jobs leaving main free room for others.
- **The sim** (`aira-350-lane-sim.out.txt`, round 2's rules; the head's room is unchanged while `S <=
  x`): the partition delays heads as the control `off-x` does (W0 240 s against 60 s today, W8 60 s
  against 20 s, W12 never against 210 s); `fixed-add` keeps today's. W0's small jobs wait 47 s
  (`fixed`), 66 s (`fixed-add`), 157 s today.
- **Recommendation: `x = 4 GiB`, `y = 1 GiB`, in every row.** `x = 8` buys 6.8 points (24.3 with
  silting) but puts row A's worst case at 2 GiB + 64 MiB per job, or costs rows B and C 6.8 GiB of
  main room on average. The idle cost is booking room, not RAM: in rows B-D main's ceiling is `x - a`
  lower even with the lane empty.

**Acceptance after rollout (§11).** Over 7 days of `confine_peak_history` against the 82.3 h window
(M: all jobs 63.9 s waited per job, small 49.7 s, large 113.3 s): **keep the lane if wait per job over
all jobs falls and small-job wait per job falls by at least 30%.** Otherwise report to the owner with
the lane-grant count, `lane_full`/`lane_shut` shares, `lane_used_bytes`/`lane_over_bytes` peaks and
daemon starts. Load differs between weeks: a screen, not a measurement of cause.

## 7. What this does not fix

- **Ledger over-booking**, the larger cause: leases book 2-5x what they use (R; M, a quarter at 21:20).
  The levers are AIRA-318 and AIRA-347 (both held) and the `AIRA_AITEST_WORKER_OVERHEAD_BYTES` floors
  fastest-ee sets (R).
- **CPU slot saturation**: nothing is admitted with all slots held (§0).
- **Large jobs** in rows B, C and D: they lose up to `x - a` of room (CE7).
- **Waits behind a pushed head** (Q2) or an exclusive drain. **Not reopened:** AIRA-351, measured NO-GO
  (`docs/dev/aira-351-cold-start-decision-rule.md:56`, `docs/dev/aira-351-cold-start-measure.out.txt`);
  AIRA-352/353, held (`docs/superpowers/specs/2026-11-18-admission-diagnostics-and-backstop-design.md`).

## 8. Risks

R1 host, row A's worst case in the watchdog band (§2.3; watch MemAvailable). R2 main capacity, `x - a`
less room (CE7). R3 silting and overflow (1.56 GiB measured; CE5). R4 missed readers (T26, T30). R5 the
summary means the main pool (§4). R6 cold-default 1 GiB jobs fill `x = 4` three at a time (intended:
the incident's population). R7 lane jobs hold slots (p99 5, peak 13, E); rule 4 protects only the head.

## 9. Deferrals

A lane cgroup; eviction or ageing of lane leases; recognising hand-made sub-reservations (CE6); a
lane in ci-shim mode; a per-instant CPU record and a booked-bytes journal series (without them the
CPU side and row B's shut time stay estimates).

## 10. Cut list

| Option | Why cut |
|---|---|
| The dynamic lane (inside the main ceiling) | Withdrawn after four rounds. |
| A stored per-lease pool flag (round 2) with an establish rule and a SET rule | It needs a restart rule that cannot read `laneOn` (`enqueueReDeclare` passes `maximum` 0, `admit.go:2239-2241`), and every restart re-silts the lane (10 starts in 93.2 h, M). It would buy back 2.2 points at `x = 4` (§6). |
| The derived form without the child clause | Suite parents fill the lane: 10.3 points lost at `x = 4`, 25.4 with silting (§6). |
| Per-row pool membership by first fit in sequence order | A release could then lower `P` (§2.4). |
| A grant-provenance flag to strip a lane parent's children of the head exemption | It is stored state, lost on restart, and when `S > x` behind a pushed head it can deadlock a parent waiting on its own child. CE6 is hand-made only. |
| Lane real use subtracted from the pressure term, or main's physical term on `E` in row A | Either couples lane to main by real use: a lane server keeps a head out. |
| A physical reservation for the head (round 1 rule 3) | Not needed once main's room is `P`, and it shut the lane behind large heads. |
| Lane occupancy at the booking (charge) | Lane real use could then exceed `x` and reach main's room (CE4's mechanism). |
| Main first, lane as overflow | Small jobs within a head's budget would spend main room and the budget; the owner said lane first. |
| A lane-internal head rule | It puts the freeze back inside the lane, and an overflowing job still has main. |
| `a` and `G` as install options | One decision, not a knob: the owner picks a row. |

## 11. Rollout

The owner answers Q1, the spec takes that row's constants, the plan gate runs. Then: restart heads-up
to peers; `aira install` (`4G`/`1G`, or `off` on a small slice; restarts the daemon,
`install.go:1287-1290`) and `aira skill install`; check `--status`, the startup line and the
`small-job lane:` line (closed at first if running small leases exceed `x`, CE5). After 7 days rerun
the replay and the ceiling-drop script and apply §6's screen. Release notes name the options, the
reply fields, the summary's main-pool meaning and where the lane's room comes from; protocol 15.

## 12. Questions for the owner

**Q1. Where does the lane's 4 GiB come from, and does the lane close when free memory drops?** The
partition rows used to be the default. This table puts every row in the same units, at `x = 4 GiB`
with 26 jobs running.

| | A: on top, always open (your words, provisional default) | B: out of the main pool, closes when RAM tightens (round 2) | C: out of the main pool, always open | D: 2 GiB on top + 2 GiB out of main, always open |
|---|---|---|---|---|
| Big jobs' room vs today | Same or more (small jobs move off it). With no memory pressure, up to 2.5 GiB less, to stay under the 64 GiB slice cap. When RAM is short, main may use up to 4 GiB more real memory than today. | 2.8 GiB less on average (up to 4) | as B | about 0.8 GiB less on average (arithmetic, not modelled) |
| Big job at the head, model cases (today 60 s / 210 s) | 60 s / 210 s | 240 s / never | 240 s / never | between (not simulated) |
| Small-job lane open when free RAM has just dropped? | yes | no: shut for up to about a fifth of busy time (estimate) | yes | yes |
| Free RAM if every job used its full booking at once | 7.6 GiB, below the 8 GiB at which AIRA's watchdog kills heavy programs outside AIRA (IDE, browser) | 11.6 GiB, as today | 11.6 GiB normally; after a sudden drop the lane can keep up to 4 GiB booked that today would drain | 9.6 GiB |
| Free RAM, realistic (last 4 days' low was 29.4 GiB, M) | about 26 GiB | 29.4 GiB | about 29 GiB | about 28 GiB (arithmetic) |
| Small-job wait removed (estimate, before CPU limits) | about 77% | lower than 77% by the shut time | about 77% | about 77% |

Example, the 13:20 incident's shape in T1's sizes (about 3.4 GiB of room today, a 4 GiB head, 14
small jobs waiting, none running): rows A, C and D admit nine small jobs at once, CPU slots
permitting; row B only what fits today's 3.4 GiB. The head's room is today's in row A (plus any
running small jobs' bookings that now count in the lane), up to 4 GiB less in B and C, up to 2 GiB
less in D. Every row's rules are in §2.2 and §2.3, so no further spec round is needed; another split
only changes `a`.

**Q2. Pushed heads.** Default: no lane grant behind a `--front` head (AIRA-309). Opening it would cost
the pushed head no RAM, only CPU and VRAM, which rules 4 and 5 reserve (about 23 push lines in 82 h, M).
**Q3. `x`.** Recommended 4 GiB in every row (§6). Open to veto at the gate: eligibility on the
booking, occupancy on the cap; suite parents and unsigned jobs excluded; lane first every pass;
derived pools with the child clause; overflow returns first.

## 13. Provenance

Under `aira confine`, named, exit 0, no `hint:` line: rounds 1-2's queries, replay, probe and sim;
round 3's replay (`aira350-replay-r3` to `-r3e`; §1h, §1h' added, all else byte-identical), ceiling-drop
(`aira350-r3-drop`, journal pinned to 2026-11-15 00:00 .. 2026-11-18 21:12) and signature count
(`aira350-r3-q1`: 0 of 244,929 confine rows unsigned; whether unsigned runs are recorded was not
checked). Not confined: `journalctl`, `systemctl --user show aira.slice` (MemoryMax 64 GiB), one
read-only `aira confine --list --json`.

## 14. Review log

**Round 1** (Sol `xhigh` and a second reviewer; 6 P1, 12 P2, none fix-induced): A, host (lane on top;
re-based to the partition, every grant on `W`); B, head independence (`min(W, P)`); C, restart (an
order-independent establish); D-H, P2s. Round 2's tripwire: "If round 2 finds a blocker in family A or
B, the right next step is owner re-scoping, not a third patch."

**Round 2** (one reviewer; 9 findings: 2 P1, 6 P2, one process). Codex Sol was OOM-killed at its
auto-sized 1 GiB cap (peak 1048752 KiB; the trailer suggests an identical re-run or `--memory-reserve
1537M`), so round 2 had no Sol review and the next round re-runs it. By root cause, each finding
checked against round 1 (`git show 955543c9:<spec>`). Six of the eight substantive findings are
fix-induced (families A and C):

| Family | Findings | Root cause | Fix-induced? | Disposition |
|---|---|---|---|---|
| A. The fork | P1 lane not fixed; P1 default reverses the owner; P2 Q1 one-sided; P2 §6 omits the `W` gate | Round 2 gated lane grants on `W` and made the partition the default. F, H and N cannot all hold while `E` falls (§2.0). | Yes, all four: round 1 had neither the `W` gate nor the partition | Not patched. The fork is stated with a proof; rows A-D are costed in one table (§2.3, §12); row A, the owner's words, is the provisional default; CE10 and T33 added; the ceiling falls measured and added to §6. One correction: finding 2's "only the additive form gives an always-available x" is wrong, since row C does too. |
| C. Restart | P2 establish cannot read `laneOn`; P2 re-silting on every restart | A stored pool flag and a frozen frame. Verified: `enqueueReDeclare` passes `maximum` 0 (`admit.go:2239-2241`), and the daemon started 10 times in 93.2 h (M). | Yes, both: round 2's establish rule | Closed by removing the state: the reviewer's derived form (finding 8), with pools from frame fields only (invariant 10, T17). T18, the establish and SET rows and the old CE5 are deleted. |
| B. Lane state reaching main | P2 a lane parent brings head-exempt workers | A client hint (signature prefix), skipped for an empty signature | No: round 1's CE6 | Stateless closure: unsigned jobs are not eligible (T14d flipped), and a lease with a live sub-reservation is not small. The hand-made residual is stated with evidence (CE6). The finding's other remedy, a stored lane-parent flag, was cut (§10). |
| Simplification | P2 derived form | — | — | Adopted with the child clause. Its measured cost is in §6. Per-row membership by greedy first fit was not built, as the finding warned. |
| Process | P2 Sol unavailable | Reviewer OOM at 1 GiB | — | Logged; the re-run is for the next round. |

**Non-convergence.** With the withdrawn design, AIRA-350 has had six rounds without approval. Round
2's claim that "every grant passes today's fit" closes family A was followed by two P1s in that family,
all from round 2's own fixes; so this round stops patching. The recurring question was never "how is
the lane guarded?" but "who pays for `x`?", which only the owner can answer (Q1). Pools derived from
frame fields alone remove family C. If the next round finds a blocker in the chosen row's host or head
rule, the next step is to cut the lane to row B's guarantees or drop it, not a fourth design.
