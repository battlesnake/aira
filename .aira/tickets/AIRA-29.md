---
{"schema":1,"id":"AIRA-29","project":"aira","title":"Dynamic reserve: charge admission by live memory.current + headroom (track-actual), fill the slice to real capacity","status":"in-review","kind":"feature","severity":"P1","assignee":null,"milestone":null,"labels":["admission","confine","oom","scheduler","shared-slice","utilisation"],"hold":false,"relations":[]}
---
Owner pivot 2026-09-01 (utilisation): the reservation model over-provisions and wastes the machine. MEASURED LIVE: a non-delegate `aira confine -- make merge-gate` reserved 33.6G (reserve-basis=estimate:p90-prior, airtight) while using 2.6G RSS for 62min; slice ledger 39.4G granted / 63.2G ceiling while physical aira.slice memory.current was only 15.4G/64G — so admission blocks new jobs though ~48G RAM + most CPU sit idle (FASTEST_XDIST_WORKERS=8 → 8/16 cores → "half busy, half idle"). ROOT: admission reserves the ESTIMATED PEAK and holds it for the whole job LIFETIME; peaks are brief + rarely coincide, so the ledger saturates long before physical RAM does.

DIRECTION (owner-chosen over airtight AIRA-28 + over deliberate-oversubscription): DYNAMIC RESERVE — the admission ledger charges each confine scope its LIVE memory.current + a growth-headroom margin, re-evaluated each daemon tick, instead of the static peak. So the money job charges ~4G not 33.6G, freeing ~29G of ledger; the slice fills to actual capacity. memory.max stays the per-scope hard cap (containment). Elegantly SIDESTEPS the anonymous per-test-lease blocker (daemon reads the whole scope's memory.current — no per-test scope_id attribution needed) → likely drops the #69 per-test RAM charge (keep the CPU governor). Note: checkedAvailable already charges max(effectiveCurrent, outstanding+adopted), and #74 adoption already re-derives a delegate scope's charge from memory.current+margin on restart — this GENERALISES that to the steady state.

RISK: the growth-race — jobs growing toward their peaks between re-eval ticks can transiently push Σ(actual) over the ceiling. NOT airtight by design; absorbed by the graduated backstop: memory.high reclaim/throttle (widen the current 60G→64G 4G runway), 8G swap, and AIRA-27 Option A oom_score_adj=800 steering a hard OOM onto the over-grower. Re-eval cadence must bound the over-commit to what the backstop absorbs.

SUPERSEDES AIRA-28 Approach A (airtight whole-suite charge — built on branch aira27-delegate-bound @ f356622, NOT deployed; the review/deploy-gate caught the wrong direction before prod). Reuses AIRA-27 Option A (oom_score_adj, deployed) as the backstop. relates AIRA-24 (saturation UX), AIRA-25 (ledger split — now SUBSUMED), AIRA-16 (watchdog). Two-loop: understand (w5pf8g0zh) → spec → plan-review → owner-gate → build.

**SPEC v2 REVIEWED (2026-09-01, branch `aira29-dynamic-reserve` @ `c66ee0d`, spec docs/superpowers/specs/2026-09-01-aira29-dynamic-reserve-plan.md).** 3-lineage plan-review (Sol+DeepSeek+Fable) all BLOCK/GATE-FAIL but **direction confirmed sound** (Fable verified every citation to the line). v2 folds 5 MF + the per-scope-memory.high upgrade: (1) effectiveCharge ledger (conservation-safe, no outstanding drift); (2) class-correct cold floor — delegate uses the suite-peak ceiling not the 512MiB overhead (the biggest cold-start hole, in the over-commit class); (3) MF3 one-rule: dynamic-replace only with a usable scan record else frozen reserve (closes the grant→create window + nested-cgroup leaf-skip + bad reads, over-charge direction); (4) LIFETIME peakSoFar ratchet (keeps the win — money job charges actual run-peak ~4G not 33.6G — AND removes the re-growth race); (5) DeepSeek's per-scope memory.high=charge kernel soft-throttle + memory.max=cap self-OOM containment (a single runaway self-OOMs before reaching the slice); corrected Slice-2 compliance = RSS vs ADMITTED ESTIMATE (RSS-vs-charge can't fire) on a daemon-wide loop w/ subtree pid enumeration + restore-down. **SHIP-TOGETHER PRECONDITION (Slice1+2): Slice 1 alone is a strict safety regression for the currently-airtight non-delegate class → re-opens the AIRA-27 P1.** NOT airtight by design (owner pivot) — bounded over-subscription + graduated backstop.

**ON HOLD — BUILD NOT STARTED (owner 2026-09-01): the xdist-based test substrate is being retired for a new purpose-built tool "aitest" (separate session).** Much of this design (per-test leases, delegate-ram cold floor, whole-suite modelling, the CPU-governor interaction) is xdist-specific, so building it against a substrate being replaced would be wasted. Plan v3 is COMPLETE + twice-reviewed (3-lineage plan-review + Fable re-check; direction sound, all MF folded) and BANKED on branch `aira29-dynamic-reserve` @ HEAD (spec docs/superpowers/specs/2026-09-01-aira29-dynamic-reserve-plan.md). **The DURABLE INSIGHT survives the pivot and may inform aitest's own worker-RAM design:** admission that reserves the estimated PEAK for the whole LIFETIME under-utilises (measured: 33.6G reserved / 2.6G used, half the box idle while jobs blocked); the fix is to charge LIVE usage + margin (track-actual), enforce per-scope via `memory.high=charge` (kernel soft-throttle) + `memory.max=cap` (self-OOM containment), and steer the residual burst-aggregate OOM by `RSS−effectiveCharge`. Revisit if/when aitest needs machine-wide RAM admission. The over-reservation ALSO affects NON-delegate confine jobs (the money case was non-delegate), so if aitest doesn't touch the confine admission layer the utilisation problem persists there.

**Fresh live confirmation, non-delegate class (peer session `deploy`, 2026-09-04).** `aira confine -- make merge-gate` requesting a 4G reserve sat queued 28+ minutes (`freeze yield`, not stuck-freeze — genuinely didn't fit) while `systemctl --user show aira.slice` showed ~42-44GB used of a 64GiB cap (~20GB apparent headroom) the whole time. Live `aira confine --list` at the time: `slice reserve: 64579878092 granted / 63168M ceiling across 4 admitted jobs` — ledger headroom ~1.5GiB, insufficient for the 4G request, even though real aggregate RSS was only ~41.5GB. Root-caused live, not inferred: two other legitimate (not test fixtures, not orphaned) `aira confine -- make merge-gate` jobs, each ~1h15m+ old, each held a **27,994,968,064-byte (~26GiB) CAP**, but each was only actually using ~19-21GiB RSS — the ledger charges the full CAP per job, not live usage, so ~10GiB of real headroom across those two jobs alone was invisible to admission the whole time. Textbook instance of this ticket's own problem statement, this time with the starved waiter being an outer confine job rather than aitest-internal, and with `deploy` correctly distinguishing it from a wedged daemon by checking `MemoryCurrent` against the slice cap directly before asking.

**Owner decision, AIRA-91 Part B (2026-09-05): the oomd backstop stays as configured, is never to be weakened or told to avoid `aira.slice`** — "it's there for if Aira fails in its duty." That makes this ticket's track-actual direction the load-bearing fix for AIRA-91 Part B too, not just a utilisation nicety: an inaccurately-sized (over-provisioned) grant is exactly what forces a job into sustained `memory.high` reclaim, which is what generates the PSI pressure that trips the oomd backstop in the first place. Full reasoning and constraint recorded on AIRA-91. This does not lift the ON-HOLD status below on its own — the banked v3 plan predates aitest and AIRA-39/41/63's now-landed real-cgroup-tree ledger, and needs re-validating against the current layout before it can be built, not resumed verbatim.

## Resolution

**Built and merged 2026-09-06.** Branch `aira29-dynamic-reserve-v2` off master `f1f699a`.
Plan: `docs/superpowers/specs/2026-09-06-aira29-dynamic-reserve-plan.md` (v8) — re-based
from scratch on current source, **superseding the banked v3** (`aira29-dynamic-reserve` @
`388cfb0`) rather than resuming it: v3's §3.5 per-scope `memory.high` re-writer is
owner-foreclosed, and its cited line numbers were stale.

### What was built

The admission ledger charges each confine scope its LIVE hierarchical `memory.current` plus
a margin, refreshed inside `evaluateAdmitQueue`'s existing `<=1s` `ListConfines` scan and
under the lock it already takes. **No new locks, no new daemon subsystem, no new loop, and
no `memory.high` written anywhere.**

- `admitWaiter.ledgerCharge()` is the single quantity every ledger site uses — the grant's
  add, the release's subtract, the scan's delta, and `confine --list`'s three sums — so
  `outstanding == Σ ledgerCharge` is one statement about one function rather than an
  agreement between six call sites. It returns the frozen `reserve` until a usable scan
  reading replaces it, which makes a granted-but-uncharged waiter *unrepresentable* rather
  than merely avoided.
- `applyChargeDelta` REFUSES an overflowing increase instead of saturating; a saturating
  clamp breaks conservation permanently and drives the ledger negative on later releases.
  It is deliberately not floored at zero, so `residualBytes()` still exposes a lost
  decrement.
- Charge = `min(cap, max(trackedRatchet, coldFloor))` where `trackedRatchet` is a monotone
  high-water mark of `min(cap, peakSoFar + margin)` and `margin = max(256 MiB, 12% of peak,
  one interval's OBSERVED growth)`. The growth term is where the owner's ruling leaves the
  growth race — at the ledger, not at a kernel throttle. The ratchet is what stops a bursty
  job swinging the shared ledger by gigabytes every second.
- A 90 s cold floor holds the full estimate for a job that has not allocated yet.
- **One rule:** only a usable scan record may move a charge; anything else holds the last
  established value. Deliberately NOT gated on leaf `Populated`, which reads 0 for a busy
  aitest outer scope. `Cap` is treated as a three-way reading (nil / `"max"` / decimal).
- A scope with live `confine-reserve` sub-reservations is excluded from dynamic charging —
  its `memory.current` is hierarchical and already contains what the children charge
  separately.
- Post-restart adoption tracks actual for non-delegate orphans too, treating an
  unestablished scope age as young; without it the win regressed on every daemon restart.
- `AIRA_DAEMON_DYNAMIC_RESERVE=disabled` reverts the WHOLE change (live charge and adoption
  margin) on a live daemon without a rebuild; an unrecognised value is refused at start.

### Deliberately NOT built, with reasoning (each filed as its own ticket)

- **AIRA-113** — dynamic per-scope `oom_score_adj` steering. The owner's named containment
  (AIRA-27's deployed 500/800 class steering) already exists and is untouched; the proposed
  trigger is near-inert inside the existing scan; catching its target population needs a new
  faster-than-1s subsystem plus a recursive cgroup pid walker that does not exist. Recorded
  there: restore-down IS permitted for a non-root daemon (probed), so the reason is
  proportionality, not permission.
- **AIRA-114** — an aggregate `Σcap <= factor × ceiling` bound. The existing scan cannot
  supply a correct cap total, and treating a locally-uncapped scope honestly would WEDGE the
  shared slice.
- **AIRA-115** (found here, pre-existing) — `confine-reserve` defaults its slice instead of
  inheriting the parent's.
- **AIRA-116** (found here, pre-existing) — no test proves `Serve` applies any parsed env
  setting, for any of the four knobs.

### Reviews

**Plan gate — Sol, four rounds:** GATE-FAIL (2 P0 + 7 P1) → GATE-FAIL (5 P1) → GATE-FAIL
(1 P1 + 1 P2) → **PASS**. It killed the over-subscription factor bound (the scan cannot
supply `Σcap`), removed a `scopeCeiling` data race by making the plan read the scan record's
own `Cap`, caught a saturating-`addClamp` conservation break, refuted an over-claimed
inertness argument, and found four porous tests before any code was written. A second
lineage (Gemini) then found the charge OSCILLATION — the growth term is non-monotone, so a
bursty job would have swung the shared ledger every second — fixed by `trackedRatchet`.

**Adversarial build review — Sol, against the landed code, two rounds:** BLOCK (2 P1 + 5
P2) → BLOCK (1 P1 + 2 P2). Every finding was ground-checked against the source before
acting. The load-bearing one: a scope's hierarchical `memory.current` was charged live while
its `confine-reserve` children still charged their own reserves — a DOUBLE-BOOK that would
have refused a healthy 4 G job with half the slice physically free, i.e. this ticket's own
problem reintroduced by its fix. Three tests were confirmed porous and fixed, including the
conservation test, which seeded granted waiters and so would have passed against a grant
that marked a waiter tracked with a ZERO charge — exactly the grant-before-scope-creation
under-charge. Full disposition of all rounds in §8/§8b/§8c/§9 of the plan.

**Anti-inert evidence:** a real-cgroup test drives the PRODUCTION `runner.ListConfines` scan
and `readSliceMemory` against a real allocating process in a real `.aira-CONFINE-*` scope,
asserting both that the charge falls to the kernel's own `memory.current` + margin and that
a queued waiter which did NOT fit before is admitted after. It was verified to FAIL against
an inert build. Beyond that, an 18-mutant battery deliberately breaks every load-bearing
behaviour; **18 of 18 caught, 0 survivors**. Two of those mutants exist only because the
first battery run surfaced genuinely porous tests.

### Exit codes (exact, on the merge candidate)

| command | exit |
|---|---|
| `go build ./...` | 0 |
| `go vet ./...` | 0 |
| `go test ./...` (AIRA_REAL_CGROUP=1, all 14 packages) | 0 |
| `go test -race ./internal/daemon/` excluding `TestSliceCeilingRealCgroupSignalTracksRealAccounting` | 0 |
| mutation battery (18 mutants) | 0 survivors |

`go test -race ./internal/daemon/` including that one test exits 1 on
`TestSliceCeilingRealCgroupSignalTracksRealAccounting` ("helper did not acknowledge anon
growth"). **Verified PRE-EXISTING**: the identical failure reproduces at base commit
`f1f699a` in a clean detached worktree. It is the AIRA-20 wall-clock-tight `-race` flake
class, not a regression, and zero DATA RACE reports were emitted in any run.

### Open design questions from the brief, as resolved

- **Is the non-delegate confine class the right scope?** Both classes, not just
  non-delegate. The formula is class-agnostic and the code path identical; excluding
  delegate would have left the larger documented over-commit hole open. It does not
  double-book against the AIRA-39/41/63 worker ledger (that accounts for children of the
  outer scope against the outer scope's cap; this accounts for the outer scope against the
  slice) — but it DID double-book against `confine-reserve` sub-reservations, which the
  build review caught and which is now excluded explicitly.
- **Slice-2 OOM steering — build it or scope it out?** Scoped out, reasoning above, filed as
  AIRA-113. Confirmed at P2 by the plan gate: "dropping the factor is not P0... the design
  is not left uncontained".
- **The `memory.high` misconception.** Not repeated. Nothing writes `memory.high`, and
  `worker_admit.go` (AIRA-35's territory) was not touched.
