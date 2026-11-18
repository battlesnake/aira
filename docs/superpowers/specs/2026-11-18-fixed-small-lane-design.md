# Small-job priority lane inside the one ceiling (AIRA-350)

Status: plan, round 4: a new design, set by the owner's decision below. Ticket AIRA-350, v0.46.
Correctness-critical (admission order, head fairness): two-loop, Opus builds, Fable reviews, Sol
`xhigh`. Rounds 1-3 (the lane on top of the ceiling, the partition) and the withdrawn dynamic lane
are in the appendix, `2026-11-18-fixed-small-lane-design-appendix.md`, with their review logs.

**Owner decision, 2026-11-19, verbatim:** "up to 8GiB permitted for the slice, stolen from the main
slice (at higher priority than main slice jobs regarding admission), for jobs with 1GB or less
request. When this slice is full, then small jobs can be admitted either to this slice (when
available) or to the main slice (if space available)." Earlier: default mode only, not CI mode.

Code references at `45f68afd` (master). **M** measured, **E** estimate, **R** reported (not
re-derived here). Data, not committed (they name other sessions' jobs): `~/tmp/aira-lane2-store/`
(store copy and `confine --list` snapshot of 2026-11-18 21:12 and 21:20 EET, journal from 11-15
00:00) and `~/tmp/aira-350/live-snapshot-1355.json`. Committed: `docs/dev/aira-350-fixed-lane-replay.py`
and `.out.txt` (section 1i is this round's).

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
| Guard G (§2.3) | A steady stream of small jobs keeps a head out for ever: in RAM a head above `E - H - x`, in CPU or VRAM any head waiting for slots or VRAM | one function, the head-eligibility test with an occupancy argument |
| Closed behind a pushed head (§2.2) | AIRA-309: only another push passes a pushed head | one conjunct |
| Suite parents and unsigned jobs not eligible (§2.1) | A parent's workers are head-exempt main grants: 13 worker scopes booking 28.0 GiB in one worktree beside a 1 GiB and a 2 GiB job (M, 13:55; rows carry no parent id, so "a 1 GiB parent's" is R) | one bool frozen at enqueue |
| Scope-less waiters not eligible | Real use is not capped by the reserve, so G's sum of pins would not bound it | one conjunct |
| Configuration, off in ci-shim (§2.5) | Owner: default mode only, install-settable | the pin-charge pattern |
| Diagnosis fields, wait-line clause, one journal line (§2.6) | "lane full" and "held behind the head" look alike; the acceptance screen needs lane grants | optional fields |

**Cut** (each was in an earlier round; appendix §A2):

| Cut | Why |
|---|---|
| The lane on top of the ceiling (round 3 rows A, D) | Owner 2026-11-19. It put worst-case MemAvailable inside the watchdog's 8 GiB band. |
| The partition (rows B, C): main room `P`, whole-slice fit `W`, kernel cap `K`, `E_m`, overflow charge, parent-move fit, tiny-slice suspension, the reader table | With one ceiling, main's room is today's `available` and every reader stays correct. |
| A second cgroup slice | Per-scope caps already bound use; every tool would learn a second slice. |
| A stored per-lease pool flag | It needs a restart rule; the derived sum is re-derived from whatever re-declares (I6). Cost at `x = 8`: 0 to 1 point (R8). |
| The live-child clause (a lease with a live sub-reservation is not small) | Parents are not eligible. Counting them in `S` costs 0.0 points at `x = 8` (1.0 with silt) (E, replay 1h vs 1h'). |
| Lane grants obeying the fairness hold; a lane-internal head rule; eviction or ageing | The first removes the benefit (holds held 73.5% of small wait, §1). The other two are not needed: the head is protected by G and the main path is today's. |
| Exempting small waiters inside today's walk instead of a pre-pass | It gives no priority over an earlier main waiter that fits (the owner asked for priority; test T21). |
| A lane in ci-shim mode; a lane CPU sub-cap | Owner; G's CPU conjunct protects the head, and slots are not the lane's to cap. |

## 1. Evidence

- **Incident** (R, withdrawn spec §1.2): 2026-11-18 about 13:20 EET, 32 jobs booked 41.5-45.4 GiB of
  a ~45 GiB ceiling on 32 of 32 CPU slots; 13-14 jobs of 0.07-1 GiB waited 4-6 min behind a 4 GiB head.
- **21:20** (M): 43.95 GiB granted of 44.56 (0.61 GiB of room), 26 jobs, phase `hold`, real use
  11.03 GiB. No design inside the ceiling admits a job above 0.61 GiB there.
- **Holds** (M, replay 1i): 507 fairness holds covered 42.0 h of the 82.3 h window (51.1%); 94.2 h of
  small-job wait fell inside one, 73.5% of all 128.2 small wait-hours. **Heads** (M, 520 hold lines):
  reserve p50 4.00 GiB, p99 14.00, max 25.32. **Ceiling** (M, 93.2 h): 38.0-52.5 GiB when throttled.
  **Suite parents** (M): 753 of 8349 small rows (9.0%), 634 of them booking exactly 1 GiB.

## 2. Design

### 2.1 Data model: no pool is stored

- **Small lease** (derived): `w.state == admitGranted && w.accounted && !w.isSubReservation() &&
  w.ledgerCharge() <= y`. However it was granted (lane, main, budget, backstop, re-declare), suite
  parents and scope-less leases included. Workers and `confine-reserve` holds are sub-reservations:
  never small, booked in main and head-exempt as today (`admit.go:2675-2688`).
- **`smallLeasesLocked(queue, y)`** walks `queue.waiters` and returns the small leases' summed
  booking `S` (`ledgerCharge`), pins `S_pin` (`reserve`), `cpu`, `vram` and count `J_s`. It is called
  where needed and cached nowhere: "derived, never incremented", like `rederiveLedgerLocked`
  (`admit.go:401`). `queue.outstanding` and the other ledger fields are untouched.
- **`admitWaiter.laneKind bool`**, frozen in the enqueue literal (`admit.go:2418`):
  `request.signature != "" && !strings.HasPrefix(request.signature, runner.AitestParentSignaturePrefix)`.
  Every signed `--delegate-ram` parent carries the prefix (`internal/runner/confine_linux.go:692-694`);
  an unsigned one does not, so unsigned jobs are excluded too (round 3 measured 0 of 244,929 confine
  rows unsigned, R). The daemon has no other way to tell a parent: `delegate_ram` left the admit
  allowlist (`internal/daemon/protocol.go:129-132`). A re-declared lease gets `false` (the ARDR
  frame carries no signature, `internal/redeclare/redeclare.go:74-79`); it is never re-queued, so
  the value is never read.
- **Eligibility** of a queued waiter: `laneOn && !w.isSubReservation() && !w.exclusive &&
  w.scopeID != "" && w.laneKind && w.ledgerCharge() <= y`, with `laneOn = s.smallLaneCap > 0 &&
  !s.shimMode()`.
- **Diagnosis only** (the AIRA-149 rule, `admit.go:261-280`): `laneWait string` (`""`, `full`,
  `kept`, `pushed`, `no-room`), rewritten by the pre-pass, and `heldByHead bool`, set in today's
  loop where the head rule turns a fit into a refusal (`admit.go:2689-2697`) and cleared at each
  evaluation. No decision reads them (T26).

### 2.2 The admission rule: a grant-only pre-pass

In `evaluateAdmitQueue` (`admit.go:2472`), after `gate` and `restartFrozen` are derived and before
today's loop (`admit.go:2581`). Today's fit (`admit.go:2622-2674`) moves verbatim into
`s.admitFitLocked(queue, w, current, E, reclaimable)` and today's grant body (`admit.go:2700-2724`,
without the `head.bypassed` line) into `s.grantWaiterLocked(queue, w, physicalFree)`; both callers
use them, so a lane grant faces exactly the RAM ledger, physical, CPU and VRAM fit a main grant does.

```
if laneOn && !restartFrozen {
  order := admitOrderLocked(queue)                       // pushed, high, normal (admit.go:452)
  r := nextRef(order, 0)                                 // the reference waiter, below
  for i, w := range order {
    if !laneEligible(w) { continue }
    w.laneWait = ""
    if gate.blocks(queue, w) { continue }                // drain / hold: today's diagnosis
    ref := order[r] or nil
    if ref != nil && ref != w && ref.pushSeq > 0 { w.laneWait = "pushed"; continue }
    occ := smallLeasesLocked(queue, y)
    if occ.S + w.ledgerCharge() > x                { w.laneWait = "full";    continue }
    if !admitFitLocked(...).all()                  { w.laneWait = "no-room"; continue }
    if ref != nil && ref != w && !couldFitBeside(ref, E, occ + w) { w.laneWait = "kept"; continue }
    grantWaiterLocked(queue, w, physicalFree)            // no bypassed, no budgetWithheld, no anchor
    log the small-lane grant line
    if i == r { r = nextRef(order, i+1) }
  }
}
// today's loop, unchanged: an eligible waiter the pre-pass did not grant is an ordinary waiter
```

- **The reference waiter** `nextRef(order, k)`: the first index `>= k` whose waiter is queued, not a
  sub-reservation, not blocked by `gate`, and passes `admitCouldFitEmptyLocked(w, E)`
  (`admit.go:2843`), derived from this pass's queue before any grant (no earlier-pass state). Every
  waiter the pre-pass can grant is at or after it (one that fits now could fit an empty slice), so
  it changes only when the reference itself is granted. It is the waiter today's walk would make
  the head if it does not fit, or grant if it does.
- **Pushed:** behind a pushed reference the lane is closed; a pushed eligible reference may use the
  lane. A pushed reference that fits is granted by the main walk, so the lane waits one pass.
- **Priority:** freed room goes to small waiters first, even past an earlier main waiter that fits
  (T21). Inside the lane: first fit in evaluation order (`--priority high` first, as the order is).
- **Lane full or kept back:** the waiter is an ordinary main waiter in today's walk: ceiling room,
  head rule, AIRA-307 budget, hold and backstop. Granted there, it is a small lease and counts in
  `S`. Still queued, it is tried in the lane again on every later pass.

### 2.3 Guard G, and the bounded guarantee

**G.** A lane grant to `w` past a reference `ref != w` needs `ref` to fit a slice holding only the
small leases and `w`: `s.admitCouldFitBesideLocked(ref, E, occ)`, with `occ` the small leases plus `w`:
- RAM, on the pin: `ref.reserve <= E - H(occ.J_s + 1) - occ.S_pin`, where `H(n) = 2 GiB + 64 MiB x n`
  (`admitSliceHeadroom`, `admit.go:1041`; defaults `admit.go:67-68`) and `occ.J_s` counts `w`;
- CPU: `ref.cpu <= cpuCeiling() - occ.cpu`;
- VRAM: `ref.vram == 0`, or a fresh evaluated snapshot with
  `ref.vram <= vramAdmitAvailable(budget, total, total, headroom, occ.vram)` (`internal/daemon/vram.go:53`).

`admitCouldFitEmptyLocked(w, E)` becomes `admitCouldFitBesideLocked(w, E, zero)`: one function, so
head eligibility and G cannot drift, and a change to either (AIRA-354) moves both. Pins rather than
bookings: scoped small leases use at most their pins (scope `memory.max`), so the head's physical
term is covered too. That is the reason scope-less waiters are not eligible.

**The guarantee (bounded; not a no-starvation proof).** Let `L` be the live leases the lane granted
while `ref` was the reference. At the last of those grants, G held for every small lease then live,
a superset of `L`. Pins, counts, CPU and VRAM only fall after a grant (a resize lowers the booking,
never the pin). So:
- `ref` fits beside `L` alone in every resource, at that grant's ceiling, for as long as it stays
  the reference.
- Hence **`ref` is granted once every lease outside `L` has ended**, provided the ceiling has not
  fallen and VRAM is free as it was. "Outside `L`" includes main leases, sub-reservations, small
  leases granted before it became the reference, and small leases granted past it by today's
  budget or backstop.
- **What G does not give.** The head waits for up to the small occupancy the lane builds while it
  waits to end as well: at most `min(x, E - H - ref.reserve)` of bookings in RAM, and up to all but
  `ref.cpu` slots in CPU. Whether other leases end at all is today's question.

**How often G binds (M, replay 1i):** never in RAM here. Over 520 holds, at the ceiling sampled within
120 s before each, a lane full at `x` and headroom for 33 jobs, the named head had 11.36 GiB to spare
at `x = 8` (15.36 at 4). G binds only above about `E - 4 GiB - x`, 26 GiB at the lowest ceiling
(38.0), against a largest retained reserve of 25.3 GiB. CPU and VRAM: unmeasured.

### 2.4 Interactions

| Interaction | Decision | Test |
|---|---|---|
| Pushed head (`--front`) | Lane closed behind a pushed reference (AIRA-309). It is the operator's lever for every head residual in §4. | T7a |
| Pushed small waiter | It is the reference, or behind a later push; as the reference it may use the lane. | T7b, T7c |
| `--exclusive` drain or hold | `gate.blocks` runs first, so a drain or hold stops lane grants as it stops main ones. A drain still waits for small leases (`sliceProvablyEmpty`, `admit.go:586`). Exclusive waiters are not eligible. | T9 |
| `--priority high` | Order only. A high head is an unpushed reference, so the lane stays open past it. | T8 |
| Fairness freeze, backstop | The pre-pass reads no phase. Lane grants never write `head.bypassed`, `budgetWithheld` or `freezeArmedAt`, so they never arm or spend a hold. The backstop pass is today's. | T11 |
| Restart freeze (S11) | The pre-pass is skipped while `restartFrozen`, as every new admission is (`admit.go:2592`). | T10 |
| AIRA-324 (enforce at 0.9 since 2026-11-19 00:10, R) | `y` and `x` are in bookings (`ledgerCharge`), so a pin up to `y` / factor is eligible: 1137 MiB at 0.9. G and the physical rule use pins. | T12 |
| ARDR, restart (§3 I6) | An establish books the pin (`newEstablishedWaiter`, `admit.go:2114-2125`, sets no charge), and an in-place SET (`admit.go:2318-2335`) restores the grant-time booking. So a discounted or resized lease whose pin is above `y` leaves `S`, and one whose pin is at most `y` stays and re-books higher: `S` is re-derived and may rise above `x`. The lane is then shut until `S` falls; the host is unaffected (I1). | T13 |
| `confine --resize` | Lowers the booking (`confine_resize.go:128-142`), so `S` falls or a large lease becomes small. Either is a re-derive. | T14 |
| Suite parents | Not eligible (`laneKind`). Granted in main, a parent of at most `y` counts in `S`. Its workers are head-exempt main sub-reservations, as today. A plain `make` wrapper that runs a nested `--delegate-ram` suite is eligible, but the workers name the nested parent: only `--delegate-ram` launches get worker-admit (`confine_linux.go:1233-1238`, `1255-1256`). | T15 |
| ci-shim | `laneOn` is false (`s.shimMode()`); `install --ci` renders `off` (`internal/install/mode.go:321` pattern). | T17 |
| AIRA-354 (overcommit; ticket only, untracked in `~/tmp/aira-overcommit`, spec not yet written) | The lane reads room only through `admitFitLocked` and `admitCouldFitBesideLocked`, so a higher room function lifts both and the lane stays a cap inside it. 354 must bound the `y` / factor pin admission above. | n/a |

### 2.5 Configuration (the pin-charge pattern)

- **Daemon** (`internal/daemon/small_lane.go`, beside `pinChargeConfigFromEnv`,
  `internal/daemon/pin_charge.go:72-90`; read in Serve beside the pin-charge read,
  `internal/daemon/server.go:416-429`). `AIRA_DAEMON_SMALL_LANE` sets `x`: unset, empty or `off`
  turn the lane off (the raw daemon default, so every existing test is unchanged). Otherwise a size
  (`runner.ParseMemorySize`, `internal/runner/memory_size.go:82`) from `y` to `admitMaxReserve`.
  `AIRA_DAEMON_SMALL_LANE_JOB_MAX` sets `y`, read only when the lane is on: default 1G, from 1M
  (`runner.MinPinnedScopeCap`) to `x`. Anything else is `E_CONFIG_INVALID` and the daemon does not
  start. Shim mode forces off. One startup line: `aira daemon: small-job lane 8G, jobs booking at
  most 1G` or `small-job lane off`.
- **Install**: `--small-lane SIZE|off` (renders `8G`) and `--small-lane-job-max SIZE` (renders `1G`),
  refused with `E_INSTALL_ARGUMENT_INVALID` (`internal/install/install.go:32`), at each `--pin-charge`
  site (`install.go:154-158`, `218-219`, `471-492`, `552`, `640-650`, `695-706`, `890-895`, `1063`,
  `1140-1143`, `1632-1670`, `2507-2508`; `cmd/aira/main.go:1039`; `mode.go:321`, CI renders `off`;
  usage `internal/core/core.go:2241`). No new error code.

### 2.6 Observability

- **`confine --list` reply** (`runner.ConfineSliceReserve`, `internal/runner/confine_manage.go:586`;
  filled in `admitSliceSnapshotFor`, `admit.go:1302`), only while `laneOn`, all `omitempty`:
  `lane_capacity_bytes` (`x`), `lane_job_max_bytes` (`y`), `lane_used_bytes` (`S`), `lane_jobs`
  (`J_s`). Every existing field keeps its meaning: one ledger. **Per scope row**
  (`ConfineRecord`, `confine_manage.go:146`): `small: true` for a small lease. **Queued rows**
  (`ConfineQueuedEntry`, `internal/runner/confine_queued.go:33`) and the named-scope probe (AIRA-24,
  `admit.go:1352-1364`): `lane_wait` and `held_by_head`.
- **Text.** After `slice queue:` (`cmd/aira/main.go:3777`): `small-job lane: 3.0G of 8G booked by 5
  jobs (jobs booking at most 1G go first)`; a small row's reserve is followed by ` small`.
- **Wait line** (`confineQueueNote`, `internal/runner/confine_queue_position_linux.go:150`): one
  clause, `held_by_head` first: `, fits but held behind the head of the queue`; `, small-job lane
  full`; `, small-job lane kept back so a larger job ahead can fit`; `, small-job lane closed behind
  a pushed job`; `, no room or CPU slot in the slice`.
- **Journal:** one line per lane grant: `aira daemon: small-lane grant seq=<n> reserve=<bytes>
  booking=<bytes> waited=<ms> lane=<S>/<x> jobs=<J_s>`. About 100 an hour (E: 8351 lane grants in
  82.3 h, replay 1i, `x = 8`, no gate).

### 2.7 Generated text

The AIRA-340 pattern (`internal/core/skill.go:305-339`): `ConfineSmallLaneGuidance` (skill, install
help) and `ConfineSmallLaneSummary` (`confine` summary `internal/core/core.go:2607`, MCP `cmd/aira/mcp.go:29`),
pinned by `TestConfineSmallLaneGuidanceIsOnEveryFace` (beside `cmd/aira/confine_priority_test.go:100`).
Long: "Small-job lane (on by default after `aira install`, never in CI mode): a job booking at most
1G is admitted ahead of larger queued jobs, without waiting behind the head of the queue, while
small jobs book at most 8G of the slice in total. It adds no memory: the job still needs room under
the slice ceiling and a CPU slot. It closes behind a `--front` job and never serves `--delegate-ram`
suites. Keep short commands' reserves small." Short: "Jobs booking at most 1G are admitted ahead
of larger queued jobs (up to 8G of them at once), so keep short commands' reserves small."
Keywords: `small-job lane`, `1G`, `8G`, `ahead of larger`, `CI`, `--front`, `--delegate-ram`.

## 3. Invariants

- **I1 One ceiling.** Every lane grant passes `admitFitLocked` with every lease counted. So at every
  grant, lane or main, `outstanding + H(outstandingJobs) <= E` holds as today, and only re-declares
  pass it, as today. The lane adds no host exposure. *Proof:* the pre-pass grants only through the
  fit that today's walk uses, after `rederiveLedgerLocked` has folded in every earlier grant.
- **I2 Lane cap.** At every lane grant `S_after <= x`. `S` passes `x` only through main grants of
  small waiters, a resize that makes a lease small, or a re-booking (a restart or SET puts a
  discounted or resized small lease back at its pin or grant value). Then no lane grant happens
  until it falls.
- **I3 Priority is bounded.** At each lane grant the small leases' bookings, the new one included,
  are at most `x`. Afterwards lane-granted bookings grow only by re-booking, by at most their pins
  less their bookings (I6). **I4 G** (§2.3) holds at every lane grant; it is on pins, which no
  resize, SET or restart changes, so re-booking does not weaken it.
- **I5 Head bookkeeping untouched.** Lane grants never change `bypassed`, `budgetWithheld`,
  `freezeArmedAt` or `freezeHolderSeq`; the main walk is today's code, so AIRA-307's bound on
  main grants past an unpushed head holds unchanged (`admit.go:485-490`).
- **I6 Restart.** After a restart `S` is the booking sum of the surviving leases whose pin is at most
  `y`, independent of re-declare order. It can be above or below the old `S`: discounted pins at
  0.9 re-book about 11% higher (a full `S = 8 GiB` becomes about 8.9 GiB, E), resized ones at their
  reserve. While `S > x` no lane grant happens; no booking passes the ceiling beyond today's
  re-declare window (I1).
- **I7 Precedence:** fail-fast trip, slice read, restart freeze, exclusivity gate, pushed reference,
  lane cap, fit, G, then today's walk. **I8 Off is today:** with `laneOn` false the pre-pass does
  nothing, and every existing test passes unchanged.

## 4. Residuals: accept or veto, one line each

- **R1 Big heads wait longer.** A head may now wait for up to `min(x, E - H - its reserve)` more
  of other jobs' bookings to end than today. Small-lease occupancy: time-weighted mean 1.58 GiB,
  p90 3.5 GiB (E, replay 1i, `x = 8`). At the window's average release of about 2.7 GB/min (E,
  round-3 query part 4) that is about 0.6 min, 1.4 min and, with `x` full, 3.2 min per head (E).
  Queued suite workers also see freed room go to the lane first, up to `x`. Seen as the head's
  queue position with `small-job lane: S of x` in `--list`; lever: `--front` the head.
  *Accept this as the price of priority, or veto by lowering `x`.*
- **R2 Heads that cannot fit beside the small leases already running** (above about `E - H - S`,
  mostly very large heads) wait for those to end; G keeps the lane shut past them meanwhile, so
  small jobs follow today's rules. The wait line says `kept back`. *Accept.*
- **R3 Silting.** Never-ending small jobs (servers) count in `S` and in G's occupancy. Measured:
  5 small job-level scopes about 128 h old, booking 1.56 GiB (M, 21:20 snapshot). They shrink the
  lane to about 6.4 GiB and are seen as `small` rows with a large age in `confine --list`.
  *Accept.*
- **R4 CPU- or VRAM-bound heads.** Lane jobs take freed slots first, up to all but the head's
  need. Such a head may then wait for every non-small slot to free. Unmeasured. Seen as many
  `small` rows and small waiters `kept back`; lever: `--front` the head.
  *Accept, or veto for round 1's rule (leave the head's slots free now), which shuts the lane
  whenever the head is slot-bound, the incident's state.*
- **R5 Pushed heads close the lane:** small jobs wait behind a `--front` job, which is also the
  operator's lever for R1-R4. *Accept, or veto to keep the lane open behind pushes (G then is a
  pushed head's only protection).*
- **R6 Suite parents never use the lane.** A hand-made `aira confine-reserve` inside a lane job
  gets head-exempt main sub-reservations, as any running job can today (`pylib/env.go:220-223`).
  *Accept.*
- **R7 After a restart** small leases re-book at their pins: `S` can rise above `x` by up to about
  0.9 GiB at `x = 8` (enforce at 0.9, E), plus any resize, and the lane stays shut until it falls
  (`lane_used_bytes` above `lane_capacity_bytes` in `--list`). Leases with pins above `y` leave `S`.
  Pushes are dropped (AIRA-309), so a former pushed head no longer closes the lane. *Accept.*
- **R8 Overflow and parents count.** Small jobs granted in main (lane full, suite parents) count
  against `x`, so the lane reopens only as small leases end. Cost against a stored flag: 0.0 points
  at `x = 8` (1.0 with silt), 10.3 at `x = 4` (E, replay 1 vs 1h, round 3's footprint).
  *Accept, or veto for a stored per-lease flag with a restart rule.*
- **R9 First fit inside the lane.** A 1 GiB small job can be passed by smaller ones while room is
  fragmented. It is protected only as a reference (G) and by today's main path. *Accept.*

## 5. Wire and protocol

`ProtocolVersion` stays 15 (`internal/daemon/protocol.go:218`). The admit request, the grant frame
and the ARDR frame are unchanged. The `confine-list` reply and the queue-position probe gain only
optional fields, decoded with `json.Unmarshal` (`confine_queue_position_linux.go:388`), so an old
client ignores them. No existing field changes meaning.

## 6. Tests (each must fail against the named wrong implementation)

**Seam:** AIRA-228/307 (`queueOrderServer`, `queueOf`, `heldLease`; `admit_queue_order_test.go:25-50`),
driving `evaluateAdmitQueue` directly. Production headroom restored (`admitTestServer` zeroes it,
`admit_test.go:23-24`). The ceiling `E` is the test server's `memory.max`, assuming the seam's slice
ceiling is off. Real use is 0 unless stated, so the ledger binds. `x` 8192, `y` 1024; 32 CPU
slots where stated, with `cpu = 1` on leases and waiters. Sizes in MiB. "Small" waiters are scoped
and `laneKind`; "main leases" book above `y`. Every test asserts the granted set, `S`, `bypassed`,
`freezeArmedAt` and each waiter's `laneWait`.

| Test | Setup | Assertions | Wrong implementation it fails |
|---|---|---|---|
| **T1 incident** | `E` 45056; 20 main leases booking 38182 (room `45056 - H(21) - 38182 = 3482`). A 4096 head queued first, `bypassed` 2048 (budget spent), hold phase. 14 small, in order: 72, 256, 512, 1024, 512, 256, 1024, 72, 512, 1024, 256, 512, 72, 1024. | Lane grants 1-6, 8 and 11 (`S` 2960: each grant costs its booking plus 64 of headroom). 7, 9, 10, 12, 13 and 14 `no-room`. Head refused; `bypassed` 2048; anchor unchanged. | Lane off or behind the head rule: none. Room computed once, no 64 per grant: also 9. Lane judged on `x` alone (on top): all 14. Lane grants in `bypassed`: 5008. |
| **T1b slots** | T1 with 32 of 32 slots held. | No grant; all `no-room`. | Lane fit without CPU. |
| **T2 ledger full** | Room 0, then -512 (an ARDR deficit). `S` 0; four 1024 small, no head. | No grant; `no-room`. | Lane on top of the ceiling: four grants. |
| **T3 lane full** | `S` 7680 (eight 960 small leases), room 6000, no head; a 1024 small, then a 512. Then an 8192 head (refused) with its budget spent, and another 512. | 1024: `full`, granted in main (`S` 8704); 512: `full`, granted in main. With the head: 512 `full` and `heldByHead`, not granted. | A waiter refused by the lane never tried in main; the cap ignored (the last is granted). |
| **T4 guard, silt** | `E` 45056. A 40960 head, budget spent, hold. A never-ending 1024 small lease; main leases 2 x 1536 (room 38656). Small waiters 1024, 512, 320, 256. Then both main leases end. (b) The never-ending lease a discounted pin 1137 booking 1024. (c) Twin without G; each pass one non-silt small lease ends and a 1024 small waiter is queued. (d) Four never-ending 1024 small leases. | Pass 1: 1024 `kept` (`45056 - H(3) - 2048 = 40768 < 40960`) and `heldByHead`; 512 granted (41280); 320 `kept` (40896); 256 granted at equality (`45056 - H(4) - 1792 = 40960`). Pass 2: head granted (room 40960). (b) 256 `kept` (pins 1905: 40847). (c) Head never granted in 50 passes. (d) Head waits; every small waiter `kept` (R2). | No G (c); G on bookings (b); `<` for `<=` (256); `H` without the head's job (320 granted, then the head refused in pass 2). |
| **T5 guard, CPU** | 32 slots: small leases 26, main 6; a head needing 4 slots, RAM ample, budget spent. Main leases end one by one; a 1-slot small waiter is queued before each pass. | Grants at 27 and at 28 small slots (`28 + 4 = 32`); the next `kept`; the head is granted once 4 slots are free. | G without the CPU conjunct: the head is never granted. |
| **T6 guard, VRAM** | VRAM ceiling 20480 after headroom; small leases hold 2048; a head needing 16384 refused on VRAM (main leases hold 8192); small waiters needing 4096 and 0. | 4096: `kept` (`2048 + 4096 + 16384 > 20480`); the 0-VRAM one granted. | G without the VRAM conjunct. |
| **T7 pushed** | (a) A pushed 4096 head refused, small waiters behind. (b) A pushed small waiter, an unpushed head refused. (c) A pushed small waiter behind a later-pushed refused head. | (a) None while the head is refused (`pushed`); once it fits, today's walk grants it and then them in one pass. (b) Granted in the lane. (c) `pushed`. | Lane ignoring pushes; G or the push rule applied when `w` is the reference. |
| **T8 high** | An unpushed `high` head refused, budget spent; small waiters. | Lane grants. | High treated as pushed. |
| **T9 exclusivity** | (a) An exclusive holder granted; a small waiter not its own. (b) A drain queued while small leases run. (c) An exclusive small waiter. | (a) Not granted. (b) The drain is granted only after the small leases release. (c) Not eligible. | The pre-pass before `gate.blocks`; emptiness ignoring small leases; exclusive eligible. |
| **T10 restart freeze** | Freeze active, room ample, a small waiter. | Not granted; `waited` set. | The pre-pass during the freeze. |
| **T11 freeze and backstop** | (a) Idle phase, a head with its budget spent, small waiters that fit. Twin with the lane off. (b) Backstop pass. (c) Hold phase. | (a) Lane grants; `freezeArmedAt` stays zero. Twin: it arms. (b) The lane grants first, then today's backfill; `bypassed` counts only main grants. (c) Lane grants. | An anchor write or `bypassed` on a lane grant. |
| **T12 AIRA-324** | Enforce, 90%. Pins 1137 and 1138 (bookings 1023.3 and 1024.2). | 1137 is eligible, `S` rises by its booking, G and the physical rule see 1137. 1138 is not eligible. | Eligibility on the pin; `S` on pins. |
| **T13 restart** | Small leases: two lane-granted; one main-granted; discounted pin 1100 (booked 990) and pin 1000 (booked 900); resized 4096 to 512 and 1024 to 256. Three main leases. Drop the leases and re-establish them by `enqueueReDeclare` in several orders. Separately, in-place SETs with the same reserve. (b) `S` 7500 including the pin-1000 and 1024-to-256 leases, none that leave; a 256 small waiter. | After, in every order, `S` = before, less 990 and 512 (pins above `y` leave), plus 100 and 768 (re-booked at the pin). SETs: the same per lease. (b) The 256 fits before (7756); after, `S` is 8368 and it is `full`. | Any stored pool or counter; classification by arrival; "a restart only lowers `S`" (the 256 granted). |
| **T14 resize** | A 1024 small lease resized to 256; a 4096 lease resized to 1024. | `S` falls 768; then rises 1024. | `S` on pins. |
| **T15 parents** | Small waiters with the parent prefix, with an empty signature, and plain; a granted parent and its 512 worker. | Only the plain one is eligible. The parent counts in `S`; the worker does not, and is head-exempt. | Eligibility without the prefix or the signature check; workers in `S`. |
| **T16 scope-less** | A scope-less, parentless 512 waiter. | Not eligible. | Eligibility without `scopeID`. |
| **T17 ci-shim** | A shim server with the lane configured. | No lane grant; Serve logs it off. | `laneOn` without `!shimMode()`. |
| **T18 off is today** | `x = 0`; a table of queues, degenerate ones included. | Grants, `bypassed`, anchor and diagnoses equal today's; the suite passes. | Any leak when off. |
| **T19 derived** | Random grants, releases, establishes, SETs, resizes and worker grants, lane on. | After every step `S` equals a fresh walk; after every lane grant `S <= x`, G held and I1 held. | An incremented counter; a missing re-derive. |
| **T20 boundaries** | `S + w == x`, then `x` less 1; booking `== y`, then 1 more. | Granted; `full`. Eligible; not. | `<` for `<=`. |
| **T21 priority** | Room 1500; a 1400 main waiter queued first, then a 1024 small waiter. | The small waiter is granted; the main waiter becomes the head. | Exemption inside today's walk. |
| **T22 first fit** | Room 600; small waiters 1024, then 256. | 256 granted; 1024 `no-room`. | A lane head-of-line rule. |

**Also:** every §2.5 value and refusal (`off`, empty, `8G`; `abc`, `0`, `x < y`, `y = 0`; shim off;
install `8G`/`1G`, CI `off`, `--status`); every §2.6 field only while on, each wait clause, the
`--list` line; the faces test. **T26:** `laneWait`, `heldByHead` written only in `evaluateAdmitQueue`,
read only by the snapshot (the `admit_pin_charge_guard_test.go` allow-list pattern).

**Mutations, by direction.** Each is run and recorded RED by a committed
`docs/dev/aira-350-mutation-check.py` (the `aira-324-mutation-check.py` pattern).
- **False pass:** lane on top of the ceiling (T1, T2); room computed once (T1); no CPU fit (T1b); no
  G (T4); G on bookings (T4); G without CPU (T5) or VRAM (T6); lane ignoring pushes (T7a); pre-pass
  before the gate (T9a) or during the restart freeze (T10); eligibility without the prefix (T15),
  the signature check (T15), `scopeID` (T16) or `!exclusive` (T9c); on in shim (T17); lane cap
  ignored (T3).
- **False fail:** lane deleted or behind the head rule (T1); a refused small waiter never tried in
  main (T3); lane grants in `bypassed` or arming (T1, T11); high as pushed (T8); push rule or G
  applied when `w` is the reference (T7b); eligibility on pins (T12); `S` on pins (T12, T14); a
  stored pool (T13, T19); in-walk exemption (T21); a lane head-of-line rule (T22); `<` for `<=`
  (T4, T20).

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

The 2026-11-18 incident window, hold-gated (E): 16 small jobs that waited at least 10 s waited 71.0
min in total (M). The lane leaves 2.3 min at `x = 4` and 0.6 min at `x = 8`, assuming room and slots.

- **Why the hold-gated row.** A hold is the one state in which today's evaluator withholds a job that
  fits (`admit.go:2762-2769`); outside holds the lane only reorders who gets freed room. The journal
  records no bookings or room at grant, so no row gates on ceiling room.
- **Biases. Upward:** no ceiling-room gate (a hold proves room for one withheld job, not for all the
  replay admits); no CPU gate in the first two rows; the 20-row retention cap thins small jobs (186
  signatures at the cap, M), so lane occupancy is understated; servers absent (replay 1b). G never
  bound in RAM (§2.3), so leaving it out biases nothing. **Downward:** in the hold-gated rows, small
  jobs taking freed room before earlier larger waiters outside holds; small jobs moved to the lane
  leave room for others. **Not modelled:** the cost to large jobs (R1).
- **CPU rows:** replay 1e's slot model (5 retained jobs ran at the incident, M; 32 reported, R).
  `x = 8` buys 9-10 points over 4 without a CPU limit, 4 at 24 unseen slots, 0 at 27 (E).

## 8. Rollout, acceptance, deferrals

Heads-up to peers; `aira install` (`8G`/`1G`, restarts the daemon), `aira skill install`; check
`--status`, the startup line and `small-job lane:`. **Acceptance after 7 days** (M: 63.9 s waited per
job over all jobs, small 49.7 s, large 113.3 s): keep the lane if small-job wait per job falls by
at least 30% and large-job wait per job rises by at most 1.4 min (R1's p90). Otherwise report to
the owner with the lane-grant count, the `lane_used_bytes` peak and the daemon starts. Load differs
between weeks, so this is a screen, not a measurement of cause. **Deferred:** a per-instant CPU and
booked-bytes record (without it R4 and the room bias stay unmeasured); an `aira top` lane panel;
recognising hand-made sub-reservations (R6); a lane in ci-shim mode.
