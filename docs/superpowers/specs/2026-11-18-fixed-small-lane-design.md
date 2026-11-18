# Small-job priority lane inside the one ceiling (AIRA-350)

Status: plan, round 5: round 4's design with its review fixed (appendix §A7). Ticket AIRA-350,
v0.46. Correctness-critical (admission order, head fairness): two-loop, Opus builds, Fable
reviews, Sol `xhigh`. History, review logs, data: `2026-11-18-fixed-small-lane-design-appendix.md`.

**Owner decision, 2026-11-19, verbatim:** "up to 8GiB permitted for the slice, stolen from the main
slice (at higher priority than main slice jobs regarding admission), for jobs with 1GB or less
request. When this slice is full, then small jobs can be admitted either to this slice (when
available) or to the main slice (if space available)." Earlier: default mode only, not CI mode.

Code references at `45f68afd` (master). **M** measured, **E** estimate, **R** reported (not
re-derived here). Replay: `docs/dev/aira-350-fixed-lane-replay.py` and `.out.txt`, section 1i.

## 0. The greenfield minimum, and the cut list

**The problem.** Small jobs wait minutes behind a large job at the head of the queue (the AIRA-307
hold), or behind larger earlier waiters for freed room. While the ledger is full nothing can help
them without adding memory, which the owner ruled out: the lane is "stolen from the main slice".

**The bare minimum.** (1) A waiter predicate: books at most `y`, ordinary job-level work. (2) A
grant-only pre-pass before today's walk: each such waiter, in today's evaluation order, is granted
on today's fit alone, with no head rule, while the small leases' bookings stay within `x`. (3) That
sum, derived by a walk. One ledger, one ceiling, and every reader of them unchanged.

| Addition beyond the minimum | Failure it closes | Size |
|---|---|---|
| Guard G (§2.3) | Small leases filling room a head needs beside them, so that a head above `E - H - x`, or a slot- or VRAM-bound head, could not fit even after every main lease ends. G does not make small-job turnover help a head (R1) | head eligibility with an occupancy argument |
| Closed behind a pushed head (§2.2) | AIRA-309: only another push passes a pushed head | one conjunct |
| Stop at a fitting sub-reservation (§2.2) | The lane taking room a queued suite worker or `confine-reserve` hold fits into now and would get today (R6) | one conjunct |
| Suite parents and unsigned jobs not eligible (§2.1) | A parent's workers are head-exempt main grants: 13 worker scopes booking 28.0 GiB in one worktree beside a 1 GiB and a 2 GiB job (M, 13:55; appendix §A6) | one bool frozen at enqueue |
| Scope-less waiters not eligible | Real use is not capped by the reserve, so G's sum of pins would not bound it | one conjunct |
| Configuration, off in ci-shim (§2.5) | Owner: default mode only, install-settable | the pin-charge pattern |
| Diagnosis fields, wait-line clause, one journal line (§2.6) | "lane full" and "held behind the head" look alike; the acceptance screen needs lane grants | optional fields |

**Cut** (reasons in appendix §A2): the lane on top of the ceiling (owner); the partition (`P`,
`W`, `K`, `E_m`, overflow charge, parent-move fit); a second cgroup slice; a stored pool flag (R8);
the live-child clause; lane grants obeying the fairness hold (73.5% of small wait-hours fell inside
one, §1); a lane head rule, eviction, ageing; exemption inside today's walk (no priority, T21); G
against every waiter ahead (R2's veto); a lane in ci-shim mode; a lane CPU sub-cap.

## 1. Evidence

- **Incident** (R, withdrawn spec §1.2): 2026-11-18 about 13:20 EET, 32 jobs booked 41.5-45.4 GiB
  of a ~45 GiB ceiling on 32 of 32 CPU slots; 13-14 jobs of 0.07-1 GiB waited 4-6 min behind a
  4 GiB head. **21:20** (M): 43.95 GiB granted of 44.56 (0.61 GiB of room), 26 jobs, phase `hold`,
  real use 11.03 GiB. No design inside the ceiling admits a job above 0.61 GiB there.
- **Holds** (M, replay 1i): 507 holds covered 42.0 h of the 82.3 h window (51.1%); 94.2 h of
  small-job wait fell inside one (87.3% of eligible small jobs' 107.9 h, 73.5% of all 128.2 small
  wait-hours; overlap, not cause). **Heads** (M, 520 hold lines): reserve p50 4.00 GiB, p99 14.00,
  max 25.32. **Ceiling** (M, 93.2 h): 38.0-52.5 GiB when throttled. **Suite parents** (M): 753 of
  8349 small rows (9.0%), 634 booking 1 GiB.

## 2. Design

### 2.1 Data model: no pool is stored

- **Small lease** (derived): `w.state == admitGranted && w.accounted && !w.isSubReservation() &&
  w.ledgerCharge() <= y`. However it was granted (lane, main, budget, backstop, re-declare), suite
  parents and scope-less leases included. Workers and `confine-reserve` holds are sub-reservations:
  never small, booked in main and head-exempt as today (`admit.go:2675-2688`).
- **`smallLeasesLocked(queue, y)`** walks `queue.waiters` and returns the small leases' summed
  booking `S` (`ledgerCharge`), pins `S_pin` (`reserve`), `cpu`, `vram` and count `J_s`. It is
  cached nowhere: "derived, never incremented", like `rederiveLedgerLocked` (`admit.go:401`).
  `queue.outstanding` and the other ledger fields are untouched.
- **`admitWaiter.laneKind bool`**, frozen in the enqueue literal (`admit.go:2418`):
  `request.signature != "" && !strings.HasPrefix(request.signature, AitestParentSignaturePrefix)`.
  Every signed `--delegate-ram` parent carries the prefix (`confine_linux.go:692-694`); unsigned
  jobs are excluded too (0 of 244,929 rows unsigned, R); the daemon has no other parent signal
  (`protocol.go:129-132`). A re-declared lease gets `false` (the ARDR record has no signature,
  `internal/redeclare/redeclare.go:74-79`) and is never re-queued.
- **Eligibility:** `laneOn && !w.isSubReservation() && !w.exclusive && w.scopeID != "" &&
  w.laneKind && w.ledgerCharge() <= y`, with `laneOn = s.smallLaneCap > 0 && !s.shimMode()`.
- **Diagnosis only** (AIRA-149, `admit.go:261-280`; no decision reads them, T27): `laneWait`
  (`""`, `full`, `kept`, `pushed`, `no-room`), cleared and rewritten by the pre-pass; `heldByHead`,
  set where today's head rule turns a fit into a refusal (`admit.go:2689-2697`), else cleared.

### 2.2 The admission rule: a grant-only pre-pass

In `evaluateAdmitQueue` (`admit.go:2472`), after `gate` and `restartFrozen` are derived and before
today's loop (`admit.go:2581`). Today's fit (`admit.go:2622-2674`) moves verbatim into
`s.admitFitLocked(queue, w, current, E, reclaimable)` and today's grant body (`admit.go:2700-2724`,
without the `head.bypassed` line) into `s.grantWaiterLocked(queue, w, physicalFree)`. Both callers
use them, so a lane grant faces exactly the RAM ledger, physical, CPU and VRAM fit of a main grant.

```
if laneOn && !restartFrozen {
  order := admitOrderLocked(queue)                       // pushed, high, normal (admit.go:452)
  clear laneWait on every queued waiter
  r := nextRef(order, 0)                                 // the reference waiter, below
  for i, w := range order {
    if w.isSubReservation() && !gate.blocks(queue, w) && admitFitLocked(...).all() { break }
    if !laneEligible(w) || gate.blocks(queue, w) { continue }  // gate: today's diagnosis
    ref := order[r] or nil
    if ref != nil && ref != w && ref.pushSeq > 0 { w.laneWait = "pushed"; continue }
    occ := smallLeasesLocked(queue, y)
    if occ.S + w.ledgerCharge() > x                { w.laneWait = "full";    continue }
    if !admitFitLocked(...).all()                  { w.laneWait = "no-room"; continue }
    if ref != nil && ref != w && !couldFitBeside(ref, E, occ + w) { w.laneWait = "kept"; continue }
    grantWaiterLocked(queue, w, physicalFree)           // no bypassed, budgetWithheld or anchor
    log the small-lane grant line
    if i == r { r = nextRef(order, i+1) }
  }
}
// today's loop, unchanged: an eligible waiter the pre-pass did not grant is an ordinary waiter
```

- **The reference waiter** `nextRef(order, k)`: the first index `>= k` whose waiter is queued, not
  a sub-reservation, not gated, and passes `admitCouldFitEmptyLocked(w, E)` (`admit.go:2843`), from
  this pass's queue. Today's walk would make it the head if it does not fit, or grant it if it
  does. It advances only when the lane grants it (T25).
- **A fitting sub-reservation ends the pre-pass.** At the first queued, ungated sub-reservation
  (suite worker, `confine-reserve` hold) that fits now, the pre-pass stops and today's walk runs,
  so the lane never takes room such a waiter would get today. Small waiters behind it are tried in
  main this pass and in the lane on the next (T26).
- **Pushed:** behind a pushed reference the lane is closed; a pushed eligible reference may use the
  lane. A pushed reference that fits is granted by the main walk, so the lane waits one pass.
- **Priority:** freed room goes to small waiters first, even past an earlier main waiter that fits
  (T21). Inside the lane: first fit in evaluation order (`--priority high` first, as the order is).
- **Lane full or kept back:** an ordinary main waiter in today's walk (room, head rule, AIRA-307
  budget, hold, backstop). Granted there, it counts in `S`; still queued, it retries the lane.

### 2.3 Guard G, and the bounded guarantee

**G.** A lane grant to `w` past a reference `ref != w` needs `ref` to fit a slice holding only the
small leases and `w`: `s.admitCouldFitBesideLocked(ref, E, occ)`, `occ` the small leases plus `w`:
- RAM, on the pin: `ref.reserve <= E - H(occ.J_s + 1) - occ.S_pin`, where `H(n) = 2 GiB + 64 MiB
  x n` (`admitSliceHeadroom`, `admit.go:1041`; defaults `admit.go:67-68`), `occ.J_s` counting `w`;
- CPU: `ref.cpu <= cpuCeiling() - occ.cpu`;
- VRAM: `ref.vram == 0`, or a fresh evaluated snapshot with
  `ref.vram <= vramAdmitAvailable(budget, total, total - occ.vram, headroom, occ.vram)`. Today's
  VRAM fit charges a running job twice, in the card's `free` and in the ledger (`vram.go:53-63`,
  `191`), so G assumes each small lease uses its whole declaration.

`admitCouldFitEmptyLocked(w, E)` becomes `admitCouldFitBesideLocked(w, E, zero)` (at zero occupancy
the VRAM term is today's, `admit.go:2857`): one function, so head eligibility and G cannot drift.
Pins rather than bookings: scoped small leases use at most their pins (scope `memory.max`), so the
head's physical term is covered too. That is why scope-less waiters are not eligible.

**The guarantee (bounded; not a no-starvation proof).** Let `L` be the live leases the lane granted
while `ref` was the reference. At the last of those grants G held for every small lease then live,
a superset of `L`. Pins, counts, CPU and VRAM declarations only fall after a grant (a resize lowers
the booking, never the pin; a re-declare carries no VRAM). So:
- `ref` fits beside `L` alone in every resource, at that grant's ceiling, while it stays the
  reference. Hence **`ref` is granted once every lease outside `L` has ended**, provided the ceiling
  has not fallen and no GPU memory is used beyond the leases' declarations. "Outside `L`" includes
  main leases, sub-reservations, small leases granted before it became the reference, and small
  leases granted past it by today's budget or backstop.
- **What G does not give.** The head also waits for the small occupancy the lane builds meanwhile
  to end: in RAM up to `min(x + 64 MiB x J_s, E - H(1) - ref.reserve)` (bookings plus per-job
  headroom), in CPU up to all but `ref.cpu` slots. While small waiters fit and `S < x`, small-job
  turnover never helps it (R1). Whether main leases end at all is today's question.

**How often G binds (M, replay 1i):** never in RAM over 520 holds (ceiling sampled within 120 s,
lane full, headroom for 33 jobs): 11.36 GiB to spare at `x = 8`, 15.36 at 4. CPU, VRAM: unmeasured.

### 2.4 Interactions

| Interaction | Decision | Test |
|---|---|---|
| Pushed head (`--front`) | Lane closed behind a pushed reference (AIRA-309). It is the operator's lever for every head residual in §4. | T7a |
| Pushed small waiter | It is the reference, or behind a later push; as the reference it may use the lane. | T7b, T7c |
| `--exclusive` drain or hold | `gate.blocks` runs first, so a drain or hold stops lane grants as it stops main ones. A drain still waits for small leases (`sliceProvablyEmpty`, `admit.go:586`). Exclusive waiters are not eligible. | T9 |
| `--priority high` | Order only. A high head is an unpushed reference, so the lane stays open past it. | T8 |
| Fairness freeze, backstop | The pre-pass reads no phase. Lane grants never write `head.bypassed`, `budgetWithheld` or `freezeArmedAt`, so they never arm or spend a hold. The backstop pass is today's. | T11 |
| Restart freeze (S11) | The pre-pass is skipped while `restartFrozen`, as every new admission is (`admit.go:2592`). | T10 |
| Sub-reservations (suite workers, `confine-reserve`) | Never eligible, never in `S`. A fitting one ends the pre-pass (§2.2). One that does not fit is passed by fitting small waiters as by any fitting job today: sub-reservations are never heads (`admit.go:2688`, `2770`). Worker claims wait without deadline (`worker_admit.go:493-503`); a `confine-reserve` hold gives up after 300 s (`internal/runner/confine_reserve.go:13`) and its test runs unreserved (R6). | T26 |
| AIRA-324 (enforce at 0.9 since 2026-11-19 00:10, R) | `y` and `x` are in bookings (`ledgerCharge`), so a pin up to `y` / factor is eligible: 1137 MiB at 0.9. G and the physical rule use pins. With pin-charge off or observe the booking is the pin, so `y` applies to the pin. | T12, T23 |
| ARDR establish (daemon restart) | `newEstablishedWaiter` books the pin (`admit.go:2114-2125`: no charge, not discountable). A discounted or resized small lease whose pin is above `y` leaves `S`; one whose pin is at most `y` re-books at its pin, so `S` may rise above `x`. The lane is then shut until `S` falls; bookings above the ceiling are today's re-declare window (I1). | T13a |
| ARDR SET (reconnect, lease present) | A discounted lease keeps its discounted charge (`admit.go:2326`); a resize is dropped back to the grant-time booking. So only resized small leases change: one whose pin is above `y` leaves `S`, one whose pin is at most `y` re-books higher. | T13b |
| `confine --resize` | Lowers the booking (`confine_resize.go:128-142`), so `S` falls or a large lease becomes small. Either is a re-derive. | T14 |
| Suite parents | Not eligible (`laneKind`). Granted in main, a parent of at most `y` counts in `S`. A plain `make` wrapper that runs a nested `--delegate-ram` suite is eligible; its workers name the nested parent (`confine_linux.go:1233-1238`, `1255-1256`). | T15 |
| ci-shim | `laneOn` is false (`s.shimMode()`); `install --ci` renders `off` (`internal/install/mode.go:321` pattern). | T17 |
| AIRA-354 (measured overbooking; spec `docs/superpowers/specs/2026-11-19-measured-overcommit-design.md` on branch `aira-overcommit-spec`, `e9077760`, not merged) | 354 §3.2 replaces the `checkedAvailable` / `ledgerAvailable` switch with one helper, `admitAvailable`, at every site, and keeps head eligibility on `L`. The lane's room is `admitFitLocked`, which calls the main walk's helper, so lane and main room cannot differ (contract T23). G is head eligibility with an occupancy argument, so it stays on `L`, never `L + over`; 354's build re-runs T4 under overbook enforce with unchanged outcomes. 354 §3.3 retires AIRA-324 to observe: `y` then applies to the pin (T12's observe arm) and the physical-pin rule is idle. 354 §10 recommends building 354 first; the build order is an owner line (§8). | T4, T12, T23 |

### 2.5 Configuration (the pin-charge pattern)

- **Daemon** (`internal/daemon/small_lane.go`, the `pinChargeConfigFromEnv` pattern,
  `internal/daemon/pin_charge.go:72-90`, read in Serve, `internal/daemon/server.go:416-429`):
  `AIRA_DAEMON_SMALL_LANE` sets `x`; unset, empty or `off` is off (the raw default, so existing
  tests are unchanged); else a size (`runner.ParseMemorySize`) from `y` to `admitMaxReserve`.
  `AIRA_DAEMON_SMALL_LANE_JOB_MAX` sets `y` (default 1G, from 1M to `x`), read only when on.
  Anything else is `E_CONFIG_INVALID` and the daemon does not start; shim mode forces off. Startup
  line: `aira daemon: small-job lane 8G, jobs booking at most 1G` or `small-job lane off`.
- **Install:** `--small-lane SIZE|off` (renders `8G`) and `--small-lane-job-max SIZE` (`1G`),
  refused with `E_INSTALL_ARGUMENT_INVALID` (`internal/install/install.go:32`), at every
  `--pin-charge` site (appendix §A8); CI renders `off`. No new error code.

### 2.6 Observability

- **`confine --list` reply** (`runner.ConfineSliceReserve`, `confine_manage.go:586`, filled in
  `admitSliceSnapshotFor`, `admit.go:1302`), only while on, `omitempty`: `lane_capacity_bytes`
  (`x`), `lane_job_max_bytes` (`y`), `lane_used_bytes` (`S`), `lane_jobs` (`J_s`); scope rows
  (`ConfineRecord`, `confine_manage.go:146`) `small: true`; queued rows (`confine_queued.go:33`) and
  the AIRA-24 probe (`admit.go:1352-1364`) `lane_wait`, `held_by_head`. No existing field changes.
- **Text** after `slice queue:` (`cmd/aira/main.go:3777`): `small-job lane: 3.0G of 8G booked by 5
  jobs (jobs booking at most 1G go first)`; a small row's reserve is followed by ` small`.
- **Wait line** (`confineQueueNote`, `confine_queue_position_linux.go:150`), one clause,
  `held_by_head` first: `, fits but held behind the head of the queue`; `, small-job lane full`;
  `, small-job lane kept back so a larger job ahead can fit`; `, small-job lane closed behind a
  pushed job`; `, no room or CPU slot in the slice`.
- **Journal**, one line per lane grant: `aira daemon: small-lane grant seq=<n> reserve=<bytes>
  booking=<bytes> waited=<ms> lane=<S>/<x> jobs=<J_s>`; about 100 an hour (E, replay 1i, `x = 8`).

### 2.7 Generated text

AIRA-340 pattern (`internal/core/skill.go:305-339`): `ConfineSmallLaneGuidance` (skill, guide,
install help) and `ConfineSmallLaneSummary` (`core.go:2607`, MCP `mcp.go:29`), pinned by
`TestConfineSmallLaneGuidanceIsOnEveryFace` (`confine_priority_test.go:100`). Long: "Small-job
lane (on by default after `aira install`, never in CI mode): a job booking at most 1G is admitted
ahead of larger queued jobs, not waiting behind the head of the queue, while small jobs hold less
than 8G in total. Beyond that, or when admitting it would stop a larger job at the front from ever
fitting, it queues as an ordinary job, so the small total can pass 8G (also after a daemon
restart). It adds no memory: the job still needs room under the slice ceiling and a CPU slot. It
closes behind a `--front` job, lets a waiting suite worker that fits go first, and never serves
`--delegate-ram` suites. Keep short commands' reserves small." Short: "Jobs booking at most 1G go
ahead of larger queued jobs while small jobs hold less than 8G; beyond that, or when it would stop
a larger job at the front from fitting, they queue as ordinary jobs. Keep short reserves small."
Keywords on every face: `1G`, `8G`, `ahead of larger`, `ordinary`, `larger job at the front`;
long only: `small-job lane`, `restart`, `CI`, `--front`, `worker`, `--delegate-ram`.

## 3. Invariants

- **I1 One ceiling, at grant time.** Every grant, lane or main, passes the same fit with every
  lease counted, after `rederiveLedgerLocked` has folded in every earlier grant, so at each grant
  `outstanding + H(outstandingJobs) <= E` holds as today. Between grants bookings stand above the
  ceiling only through today's two events: a re-declare (I6) and a fall of the pressure ceiling
  below existing bookings (`sliceceiling.go:531`). `available` is then negative and no grant of
  either kind happens until releases or recovery (T2c). The lane adds no booking above the
  ceiling; use beyond bookings stays today's (VRAM has no cap; a timed-out `confine-reserve`, R6).
- **I2 Lane cap.** At every lane grant `S_after <= x`. `S` passes `x` only through main grants of
  small waiters, a resize that makes a lease small, an establish that re-books a small lease at its
  pin, or a SET that drops a resize. Then no lane grant happens until it falls.
- **I3 Priority is bounded by `x` plus headroom.** The lane's take from main is `S + 64 MiB x J_s`;
  at each lane grant `S <= x`, and G caps `S_pin + 64 MiB x J_s` at `E - H(1) - ref.reserve`. `J_s`
  is bounded by CPU slots only when each small job declares a core (the confine client's default,
  `admit.go:178`; the daemon's is 0, `admit.go:3383-3394`): 10 GiB at `x = 8` with 32 one-core
  jobs. Afterwards bookings grow only by re-booking (I6). **I4 G** (§2.3) holds at every lane
  grant; it is on pins, which no resize, SET or establish raises.
- **I5 Head bookkeeping untouched.** Lane grants never change `bypassed`, `budgetWithheld`,
  `freezeArmedAt` or `freezeHolderSeq`; the main walk is today's code, so AIRA-307's bound on main
  grants past an unpushed head holds unchanged (`admit.go:485-490`).
- **I6 Restart and reconnect.** After a daemon restart `S` is the booking sum of the re-established
  leases whose pin is at most `y`, independent of order: discounted pins at 0.9 re-book about 11%
  higher (a full `S = 8 GiB` becomes about 8.9 GiB, E), resized ones at their pin. After a SET only
  resized leases change. While `S > x` no lane grant happens (I1 bounds the rest).
- **I7 Precedence:** fail-fast trip, slice read, restart freeze, a fitting sub-reservation (stop),
  exclusivity gate, pushed reference, lane cap, fit, G, then today's walk. **I8 Off is today:**
  with `laneOn` false the pre-pass does nothing, and every existing test passes unchanged.

## 4. Residuals: accept or veto, one line each

- **R1 Small-job turnover never helps a head.** Under sustained small demand, while small waiters
  fit and `S < x`, every freed byte goes to them first: a head is granted only when main leases
  free its room, and the lane takes up to `x` plus 64 MiB per small job (I3). T1: today the 4096
  head needs 614 MiB freed by any job (small jobs drain behind the hold, `admit.go:2689-2697`); with
  the lane, 8638 MiB of main releases (4552 for the six small waiters still queued, then its 4086),
  more while small jobs arrive. With long-lived main leases (21:20: 43.95 of 44.56 GiB booked, M)
  that wait is unbounded by anything small jobs do; no minutes figure is claimed. Seen as the head's
  unchanged position beside `small-job lane: S of x`; lever: `--front` the head. *Accept as the
  price of priority, or veto by lowering `x` (the take scales with it).*
- **R2 G guards only the reference.** A larger job further back is guarded only once it is the
  reference, then waits for the small leases already running, including those the lane admitted
  meanwhile, to end (for ever if they never do); G keeps the lane shut past it (`kept back`). The
  same holds behind a `high` or a starved small reference. T24. Lever: `--front`. *Accept, or veto
  to test G against every could-fit-empty job-level waiter ahead of `w` (one loop; T24's pass 1
  then grants 5 of 8; yield unmeasured).*
- **R3 Silting.** Never-ending small jobs (servers) count in `S` and in G's occupancy: 5 small
  job-level scopes about 128 h old booking 1.56 GiB (M, 21:20), shrinking the lane to about 6.4 GiB,
  seen as `small` rows with a large age in `confine --list`. *Accept.*
- **R4 CPU- or VRAM-bound heads.** Lane jobs take freed slots first, up to all but the head's need;
  such a head may wait for every non-small slot to free. G assumes no GPU use beyond declarations;
  VRAM has no per-job cap and a re-declare forgets a lease's VRAM (`server.go:1116-1121`), so past
  that a GPU head waits for small leases to end. Unmeasured. Seen as `small` rows and `kept back`;
  lever: `--front`. *Accept, or veto for round 1's rule (leave the head's slots free now), which
  shuts the lane whenever the head is slot-bound, the incident's state.*
- **R5 Pushed heads close the lane:** small jobs wait behind a `--front` job, which is also the
  operator's lever for R1-R4. *Accept, or veto to keep the lane open behind pushes.*
- **R6 Sub-reservations that do not fit yet** are passed by small jobs as by any fitting job today,
  and under sustained small demand gain room only from main releases (R1). Witness: 32 slots, a
  running one-slot suite parent, a queued 31-slot worker, a later persistent one-slot small job: if
  slots free one at a time the small job takes one, and worker and parent wait for its end (freed at
  once, the stop rule grants the worker). A `confine-reserve` hold past 300 s runs its test
  unreserved in the parent's scope, capped at its booking or, for a `--delegate-ram` parent, a
  containment ceiling many times it (`admit.go:1189-1192`), as on any timeout today. *Accept, or
  veto for no lane grant past any queued sub-reservation (shut whenever a worker waits).*
- **R7 After a restart** small leases re-book at their pins: `S` can rise above `x` by about 0.9
  GiB at `x = 8` (enforce at 0.9, E), plus any resize, and the lane stays shut until it falls
  (`lane_used_bytes` above `lane_capacity_bytes`). Leases with pins above `y` leave `S`. A reconnect
  (SET) changes only resized leases. Pushes are dropped (AIRA-309). *Accept.*
- **R8 Overflow and parents count.** Small jobs granted in main (lane full, suite parents) count
  against `x`, so the lane reopens only as small leases end. Cost against a stored flag: 0.0 points
  at `x = 8` (1.0 with silt), 10.3 at `x = 4` (E, replay 1 vs 1h). *Accept, or veto for a stored
  per-lease flag with a restart rule.*
- **R9 First fit inside the lane.** A 1 GiB small job can be passed by smaller ones while room is
  fragmented: G does not protect it (G against a small reference is vacuous). It gets in when
  the smaller ones stop fitting, the lane fills, or by today's main path, where it may be the head
  and hold main waiters by today's budget and hold. *Accept.*

## 5. Wire and protocol

`ProtocolVersion` stays 15 (`internal/daemon/protocol.go:218`). The admit request, grant and ARDR
frames are unchanged. The `confine-list` reply and the queue-position probe gain only optional
fields (`json.Unmarshal`, `confine_queue_position_linux.go:388`); no existing field changes meaning.

## 6. Tests (each must fail against the named wrong implementation)

**Seam:** AIRA-228/307 (`queueOrderServer`, `queueOf`, `heldLease`;
`admit_queue_order_test.go:25-50`), one `evaluateAdmitQueue` call per pass, production headroom
restored (`admitTestServer` zeroes it, `admit_test.go:23-24`). `E` is the test reader's
`memory.max`, the seam's slice ceiling off (T2c sets it). Real use 0 unless stated. `x` 8192, `y`
1024; 32 slots where stated, `cpu = 1` on leases and waiters. Sizes in MiB. "Small" waiters are
scoped and `laneKind`; "main leases" book above `y`. Every test asserts the granted set, `S`,
`bypassed`, `freezeArmedAt` and each `laneWait`. One pass unless passes are named.

| Test | Setup | Assertions | Wrong implementation it fails |
|---|---|---|---|
| **T1 incident** | `E` 45056; 20 main leases booking 38182 (room `45056 - H(21) - 38182 = 3482`). A 4096 head queued first, `bypassed` 2048 (budget spent), hold phase. 14 small, in order: 72, 256, 512, 1024, 512, 256, 1024, 72, 512, 1024, 256, 512, 72, 1024. | Lane grants 1-6, 8 and 11 (`S` 2960; room left 10: each grant costs its booking plus 64). 7, 9, 10, 12, 13, 14 `no-room`. Head refused; `bypassed` 2048; anchor unchanged. | Lane off or behind the head rule: none. Room computed once, no 64 per grant: also 9. Lane judged on `x` alone (on top): all 14. Lane grants in `bypassed`: 5008. |
| **T1b slots** | T1 with 32 of 32 slots held. | No grant; all `no-room`. | Lane fit without CPU. |
| **T2 ledger full** | (a) Room 0; (b) -512 (an ARDR deficit); four 1024 small, no head. (c) Room 2000 at `memory.max`, but the enforced ceiling (`admitEffectiveMaximum`, set through the slice-ceiling seam) 3000 lower, below bookings; then restored. | (a), (b) No grant; `no-room`. (c) No grant while lowered; one grant after. | Lane on top of the ceiling: four grants. Lane fit on `memory.max` instead of the enforced ceiling: (c) granted while lowered. |
| **T3 lane full** | `S` 7680 (eight 960 small leases), room 6000, no head. Pass 1: a 1024 small, then a 512. Pass 2, hold phase: an 8192 waiter (refused, budget spent: the head), then another 512 small. | Pass 1: 1024 `full`; 512 lane-granted at equality (`S` 8192); then today's walk grants the 1024 in main (`S` 9216). Pass 2: the 512 `full` and `heldByHead`, not granted. | Lane and main interleaved per waiter: 512 `full`. `>=` for `>`: 512 `full`. A refused waiter never tried in main: 1024 not granted. Cap ignored: pass-2 512 granted. |
| **T4 guard, silt** | `E` 45056. A 40960 head, budget spent, hold. A never-ending 1024 small lease; main leases 2 x 1536 (room 38656). Small waiters 1024, 512, 320, 256. Pass 2: both main leases end. (b) The never-ending lease a discounted pin 1137 booking 1024. (c) Twin without G; each pass one non-silt small lease ends and a 1024 small waiter is queued. (d) Four never-ending 1024 small leases. | Pass 1: 1024 `kept` (`45056 - H(3) - 2048 = 40768 < 40960`) and `heldByHead`; 512 granted (41280); 320 `kept` (40896); 256 granted at equality (`45056 - H(4) - 1792 = 40960`). Pass 2: head granted (room 40960). (b) 256 `kept` (pins 1905: 40847). (c) Head never granted in 50 passes. (d) Head waits; every small waiter `kept`. | No G (c); G on bookings (b); `<` for `<=` (256); `H` without the head's job (320 granted, then the head refused in pass 2). |
| **T5 guard, CPU** | 32 slots: small leases 26, main 6; a head needing 4 slots, RAM ample, budget spent. Main leases end one per pass; a 1-slot small waiter is queued before each pass. | Grants at 27 and 28 small slots (`28 + 4 = 32`); the next `kept`; the head is granted once 4 slots are free. | G without the CPU conjunct: head never granted. |
| **T6 guard, VRAM** | RAM ample. Card 21504, headroom 1024, budget auto. Main leases declare and use 8192 (snapshot `free` 13312); a 16384 head refused (fit `12288 - 8192 = 4096`). Pass 1: small waiters needing 2048, 2048, 0. Pass 2: main leases end; the granted small lease uses 2048; fresh snapshot (`free` 19456). | Pass 1: first 2048 granted (G: `min(21504, 21504 - 2048 - 1024) - 2048 = 16384`); second `kept` (own fit 2048 passes; G 12288); the 0 granted. Pass 2: head granted (`18432 - 2048 = 16384`). | G without VRAM; G with `free = total` (round 4): second granted, then both use 2048 and the head's fit is 12288 while they live. |
| **T7 pushed** | (a) A pushed 4096 head refused, small waiters behind. (b) A pushed small waiter, an unpushed head refused. (c) A pushed small waiter behind a later-pushed refused head. | (a) None while the head is refused (`pushed`); in the pass it fits, today's walk grants it, the small waiters the next pass. (b) Granted in the lane. (c) `pushed`. | Lane ignoring pushes; G or the push rule applied when `w` is the reference. |
| **T8 high** | An unpushed `high` head refused, budget spent; small waiters. | Lane grants. | High treated as pushed. |
| **T9 exclusivity** | (a) An exclusive holder granted; a small waiter not its own. (b) A drain queued while small leases run. (c) An exclusive small waiter. | (a) Not granted. (b) The drain is granted only after the small leases release. (c) Not eligible. | The pre-pass before `gate.blocks`; emptiness ignoring small leases; exclusive eligible. |
| **T10 restart freeze** | Freeze active, room ample, a small waiter. | Not granted; `waited` set. | The pre-pass during the freeze. |
| **T11 freeze and backstop** | (a) Idle phase, a head with its budget spent, small waiters that fit. Twin with the lane off. (b) Backstop pass. (c) Hold phase. | (a) Lane grants; `freezeArmedAt` stays zero. Twin: it arms. (b) The lane grants first, then today's backfill; `bypassed` counts only main grants. (c) Lane grants. | An anchor write or `bypassed` on a lane grant. |
| **T12 AIRA-324** | Enforce, 90%: pins 1137 and 1138 (bookings 1023.3 and 1024.2). Observe: pins 1024 and 1025. | Enforce: 1137 eligible, `S` rises by its booking, G and the physical rule see 1137; 1138 not. Observe: 1024 eligible, 1025 not. | Eligibility on the pin under enforce; `S` on pins. |
| **T13 restart, reconnect** | Small leases: two lane-granted; one main-granted; discounted pin 1100 (booked 990) and pin 1000 (booked 900); resized 4096 to 512 and 1024 to 256. Three main leases. (a) Drop every lease and re-establish by `enqueueReDeclare`, in several orders. (b) In-place SETs with the same reserves. (c) `S` 7500 including the pin-1000 and 1024-to-256 leases and none that leave; a 256 small waiter, before and after (a), and after (b). | (a) In every order `S` = before - 990 - 512 + 100 + 768. (b) `S` = before - 512 + 768; the discounted leases keep 990 and 900. (c) Before: 256 granted (7756). After (a): `S` 8368, `full`. After (b): `S` 8268, `full`. | Any stored pool or counter; classification by arrival; a SET treated as an establish (b: 990 leaves); "a restart only lowers `S`" (256 granted). |
| **T14 resize** | A 1024 small lease resized to 256; a 4096 lease resized to 1024. | `S` falls 768; then rises 1024. | `S` on pins. |
| **T15 parents** | Small waiters with the parent prefix, with an empty signature, and plain; a granted parent and its 512 worker. | Only the plain one is eligible. The parent counts in `S`; the worker does not, and is head-exempt. | Eligibility without the prefix or the signature check; workers in `S`. |
| **T16 scope-less** | A scope-less, parentless 512 waiter. | Not eligible. | Eligibility without `scopeID`. |
| **T17 ci-shim** | A shim server with the lane configured. | No lane grant; Serve logs it off. | `laneOn` without `!shimMode()`. |
| **T18 off is today** | `x = 0`; a table of queues, degenerate ones included. | Grants, `bypassed`, anchor and diagnoses equal today's; the suite passes. | Any leak when off. |
| **T19 derived** | Random grants, releases, establishes, SETs, resizes and worker grants, lane on. | After every step `S` equals a fresh walk; after every lane grant `S <= x`, G held and I1 held. | An incremented counter; a missing re-derive. |
| **T20 boundaries** | `S + w == x`, then `x` less 1; booking `== y`, then 1 more. | Granted; `full`. Eligible; not. | `<` for `<=`. |
| **T21 priority** | Room 1500; a 1400 main waiter queued first, then a 1024 small waiter. | The small waiter is granted; the main waiter becomes the head. | Exemption inside today's walk. |
| **T22 first fit** | Room 600; small waiters 1024, then 256. | 256 granted; 1024 `no-room`. | A lane head-of-line rule. |
| **T23 one room** | Enforce 90%, `E` 45056, one 4096 main lease; a small discounted pin 1137 (booking 1023.3). (a) Real use 41780 (physical room `45056 - H(2) - 41780 = 1100`, ledger room 1100). (b) Real use 41743 (1137). (c) Parity: over a table of real use, reclaimable, bookings, discount, CPU and VRAM, a lone small waiter with `S` 0 and no head is lane-granted exactly when the lane-off twin grants it. | (a) `no-room`. (b) Granted at equality. (c) Equal verdicts on every row. | Lane fit without the full-pin physical rule: (a) granted. Any lane-only copy of the fit: (c). |
| **T24 G masked (R2)** | `E` 45056, room 10000 (one main lease books 2048). Order: A 2048 main, B 36864 main, eight 1024 small. Pass 2: the 2048 main lease ends. Pass 3: one small lease ends; a 512 small waiter. | Pass 1: all eight lane-granted (`S` 8192; G against A vacuous); A refused (room 1296), the head. Pass 2: A granted in main. Pass 3: B is the reference; the 512 `kept` (`45056 - H(9) - 7680 = 34752 < 36864`). | G against B from pass 1 (the veto form): pass 1 grants 5. |
| **T25 reference** | `E` 45056; a never-ending 1024 small lease, main 2 x 1536 (room 38656) unless stated. (a) Order: 512 small, a 40960 waiter, 1024 small. (b) A queued 39000 worker (refused), a 40960 waiter, 1024 small. (c) Throttled `E` 38912 (`memory.max` 45056), no leases: a 40960 waiter (cannot fit empty), 1024 small. (d) An exclusive holder booking 2048, no other leases: a non-holder 42000 waiter (gated), a 1024 small carrying the holder's token. | (a) 512 granted, then 1024 `kept` (`45056 - H(4) - 2560 = 40192 < 40960`). (b) 1024 `kept` (40768). (c) 1024 granted. (d) 1024 granted. | (a) Reference not advanced after its grant: granted. (b) Reference may be a sub-reservation: granted. (c) No could-fit-empty filter: `kept`. (d) Reference may be gated: `kept` (41856). |
| **T26 sub-reservation** | (a) Room 1100: a queued 1024 worker of a granted parent, then a 1024 small. (b) The worker needs 2048. | (a) The worker granted by today's walk; the small waiter not, `laneWait` "". (b) The small waiter lane-granted. | No stop: (a) small granted, worker refused. Stop at any sub-reservation: (b) not granted. |

**Also:** every §2.5 value and refusal (`off`, empty, `8G`; `abc`, `0`, `x < y`, `y = 0`; shim off;
install `8G`/`1G`, CI `off`, `--status`); every §2.6 field only while on, each wait clause, the
`--list` line; the faces test and its keywords (§2.7). **T27:** `laneWait`, `heldByHead` written
only in `evaluateAdmitQueue`, read only by the snapshot (`admit_pin_charge_guard_test.go` pattern).

**Mutations.** Every entry in the last column is one, run and recorded RED by a committed
`docs/dev/aira-350-mutation-check.py` (the `aira-324-mutation-check.py` pattern). **False fail**
(a correct grant refused): lane deleted or behind the head rule (T1); never tried in main,
interleaved, `>=` for `>` (T3); `bypassed` or arming (T1, T11); high as pushed (T8); push rule or G
when `w` is the reference (T7b); eligibility or `S` on pins (T12, T14); stored pool, SET as
establish (T13, T19); in-walk exemption (T21); lane head-of-line (T22); reference unfiltered or
gated (T25c, d); stop at any sub-reservation (T26b); `<` for `<=` (T4, T20). **False pass** (a
wrong grant made): every other entry, among them T2c, T4-T6, T23, T25a-b and T26a.

## 7. Expected yield: estimates, not bounds

Replay section 1i (`docs/dev/aira-350-fixed-lane-replay.py --journal ...`, run under `aira confine`,
exit 0). Arrivals from retained `confine_peak_history` rows (82.3 h), holds from the journal. Lane
first, first fit; footprint = booking (the window ran pin-charge observe); `S` counts every small
job-level lease, suite parents included. Share of 128.2 small wait-hours (M) removed:

| Variant (all E) | `x = 4 GiB` | `x = 8 GiB` |
|---|---|---|
| Upper: lane grant whenever `S` has room (no ceiling-room or CPU gate) | 95.5 h, 74.5% | 107.9 h, 84.1% |
| Hold-gated: lane grants only inside a fairness hold | 90.0 h, 70.2% | 101.7 h, 79.3% |
| Hold-gated, 24 of 32 slots held by jobs the store does not see | 83.3 h, 65.0% | 88.1 h, 68.7% |
| Hold-gated, 27 of 32 slots held | 38.6 h, 30.1% | 38.9 h, 30.3% |
| **Lower: all slots held, or no ceiling room** (the incident's 32 of 32 slots, R; 21:20's 0.61 GiB, M) | **0** | **0** |

**Which row the evidence supports.** The measured live states had 28-32 of 32 slots taken (R,
withdrawn spec §1) and 0.61 GiB of room at 21:20 (M): between the "27 held" and "all held" rows,
0 to 30%. The earlier association figure, small waits ending at a backstop pass, was about 5 of
115.6 wait-hours, about 4% (R, withdrawn spec §1; AIRA-354 §10 cites it). The upper rows assume
free slots and room those states did not have. Incident window, hold-gated (E): 16 small jobs that
waited at least 10 s waited 71.0 min (M); the lane leaves 2.3 min at `x = 4`, 0.6 at `x = 8`.
**Biases** (appendix §A8). Upward: no ceiling-room gate; no CPU gate in the first two rows; the
retention cap thins small jobs; servers absent. Downward: small jobs taking freed room outside
holds; room left for others. Not modelled: the cost to large jobs (R1) and sub-reservations (R6).

## 8. Rollout, acceptance, deferrals

**Build order (owner line).** AIRA-354 §10 recommends building 354 first and the lane only if
hold-time small waits remain material; this spec follows the owner's 2026-11-19 decision to build
the lane at v0.46. *Accept, or veto to build 354 first and re-run the replay after its week.*

Heads-up to peers; `aira install` (`8G`/`1G`, restarts the daemon), `aira skill install`; check
`--status`, the startup line and `small-job lane:`. **Acceptance after 7 days** (M: 63.9 s waited
per job, small 49.7 s, large 113.3 s): keep the lane if small-job wait per job falls by at least
30% and large-job wait per job rises by at most 50% (to 170 s; R1 gives no a-priori bound, so this
threshold is the owner's). Otherwise report the lane-grant count, the `lane_used_bytes` peak and
the daemon starts to the owner. A screen, not a measurement of cause. **Deferred:** a per-instant
CPU and booked-bytes record (R4 and the room bias stay unmeasured); an `aira top` lane panel;
recognising hand-made sub-reservations; a lane in ci-shim mode.
