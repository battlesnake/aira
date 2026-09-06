# AIRA-29 v8 — dynamic reserve: charge admission by live cgroup usage

**Status:** plan v8, written against master `f1f699a` (2026-09-06), **Sol plan-gate PASS**
at v7. Three review rounds: v4 GATE-FAIL (2 P0 + 7 P1) -> v5 GATE-FAIL (5 P1, both scoping
decisions confirmed at P2) -> v6 GATE-FAIL (1 P1 + 1 P2) -> v7 PASS. See §8/§8b/§8c for the
disposition of every finding.
Re-based from scratch on current source; the banked v3 (`aira29-dynamic-reserve` @
`388cfb0`) is superseded, not resumed — its §3.5 is owner-foreclosed and its line numbers
are stale.
**Ticket:** AIRA-29 (P1). Branch: `aira29-dynamic-reserve-v2`, off `f1f699a`.
**Owner ruling being implemented (recorded on the ticket, not mine to revisit):** accept
bounded over-subscription — real usage charged instead of a frozen peak estimate —
contained ONLY by each scope's own `memory.max` and steered `oom_score_adj`, NOT by any
`memory.high` soft-throttle.

## 1. Problem, restated against current master

`admitWaiter.reserve` is frozen at enqueue (`admit.go:140`), added to `queue.outstanding`
at grant (`:1635`) and subtracted at release (`:1755`), and never re-read in between. So
admission holds the *estimated peak* for the whole *lifetime*.

Two measured incidents, both **non-delegate**:

- `make merge-gate` reserved 33.6 G (`estimate:p90-prior`) while using 2.6 G for 62 min;
  ledger 39.4 G granted / 63.2 G ceiling with physical slice usage 15.4 G / 64 G.
- 2026-09-04: a 4 G request sat queued 28+ min behind two `make merge-gate` jobs each
  holding a 27,994,968,064-byte cap while actually using 19–21 G — ~10 G of real headroom
  invisible to admission, with the slice at ~42–44 G of 64 G.

## 2. The seam (re-verified at `f1f699a`)

`evaluateAdmitQueue` (`admit.go:1347`) already:

- runs a `<=1s`-throttled `ListConfines` scan **lock-free before `queue.mu`**
  (`:1356-1374`, lock at `:1376`);
- inside the scan-success branch, walks granted scope-backed waiters and matches them
  against scan membership by `scopeID` (`:1405-1426` — the `present`/`held` maps);
- carries per-scope `RSSBytes` (hierarchical `memory.current`,
  `confine_manage_linux.go:133-138`) and `Cap` (the scope's real `memory.max`) on every
  record;
- computes `available = checkedAvailable(current, effectiveMaximum, reclaimable,
  outstanding+adopted, headroom)` (`:1588`), where `checkedAvailable` takes
  `charge = max(current - reclaimable, outstanding+adopted)` (`:1694-1711`);
- already does per-scope `min(cap, RSS+64MiB)` reconstruction, but **only** for `@dr-`
  delegate orphans (`:1481-1493`); non-delegate orphans adopt full `Cap` (`:1494`).

So the charge can become dynamic **inside the existing locked scan block, with zero new
locks and no change to the `admitRegistryMu -> queue.mu` order**. The too-large guard
(`:1277`) and headroom (`:1587`) are count-only, so a byte change does not disturb them.

## 3. Design

### 3.1 Per-waiter `effectiveCharge` — the ledger model

Add to `admitWaiter`, all read and written **only under `queue.mu`**:

| field | meaning |
|---|---|
| `effectiveCharge int64` | what this waiter currently charges `queue.outstanding`. Meaningful only while `state == admitGranted && accounted`; set at grant, never before. |
| `peakSoFar int64` | max `memory.current` observed for this scope across this run's scans. Lifetime ratchet, monotonic non-decreasing. |
| `lastRSS int64` | the previous scan's `memory.current`, valid only when `havePrevRSS`. |
| `havePrevRSS bool` | false until the first usable record is seen; makes the first sample's `growth` term 0 rather than `rss - 0 == rss`. |
| `trackedRatchet int64` | the monotone non-decreasing `min(cap, peak + margin)` high-water mark, excluding the cold floor. See §3.2. |

- **Grant** (`:1632-1636`): `effectiveCharge = reserve`; `outstanding += effectiveCharge`.
- **Scan** (successful, `<=1/s`, under `queue.mu`), per granted scope-backed waiter with a
  usable record (§3.3): `next, ok := applyChargeDelta(outstanding, effectiveCharge,
  newCharge)`; only when `ok` do both `outstanding = next` and `effectiveCharge = newCharge`
  happen — they are one atomic step or neither.
- **Release** (`:1754-1757`): `outstanding -= effectiveCharge` — the *current* value under
  the identical `admitGranted && accounted` guard, so add and subtract are symmetric.
- The delta form above is the ONLY writer between grant and release. `outstandingJobs`
  stays a pure count.

**Conservation invariant:** `outstanding == Σ effectiveCharge` over granted+accounted
waiters at every point; it returns to exactly 0 after all releases and is never negative.

Two properties make that exact rather than approximate:

- **`applyChargeDelta` REFUSES rather than saturates.** A saturating `addClamp` here would
  break conservation permanently: with another waiter contributing 60, a move of this
  waiter's charge to `MaxInt64` clamps `outstanding` to `MaxInt64` while `Σ effectiveCharge`
  is `MaxInt64 + 60`, and the subsequent releases drive `outstanding` negative. So on an
  increasing delta that would overflow, the function returns `(outstanding, false)` and the
  caller leaves BOTH the ledger and the charge untouched — the §3.3 "hold" outcome.
- **It is deliberately not floored at zero.** Flooring would hide exactly the lost/double
  decrement `residualBytes()` exists to expose (`:766-775`). The decreasing direction cannot
  wrap, because `outstanding >= effectiveCharge` holds by construction.

**`admitSliceSnapshotFor` must move with it** (`:839-852`): `scopeBytes`,
`reservationBytes` and `vanishedBytes` sum `effectiveCharge` for granted+accounted waiters,
or `residualBytes()` (`:780`) — a real invariant checker surfaced by `confine --list` —
would report a fabricated ledger defect on every dynamic pass. `queuedBytes` (`:832`) keeps
using `reserve`: a queued waiter has no charge, and AIRA-24's "bytes queued ahead of me" is
a statement about reserves.

Waiters with no `scopeID` (plain `admit`, `confine-reserve`) are **never** dynamically
replaced — there is no scope to read.

**Nor is a scope that HAS live `confine-reserve` sub-reservations** (build review P1). Those
children are scope-less waiters in this same queue, each charging its own reserve, while the
parent's `memory.current` is hierarchical and already contains everything they allocated.
Charging the parent its live usage as well would count the same bytes twice — a suite's
28 GiB of per-test reservations sitting in the ledger beside a parent charge that already
included them — and would then refuse a healthy 4 GiB job with half the slice physically
free. That is the very over-reservation this ticket exists to remove, reintroduced by its
own fix. Such a parent keeps its pinned framework overhead exactly as today; the exclusion
is a property of the current waiter population, not a latch, so the parent becomes
chargeable again once its last sub-reservation releases.

### 3.2 The charge formula

Computed only from a usable scan record, under `queue.mu`. **`cap` is the record's own
`Cap`** — the scope's own local, kernel-enforced `memory.max` as read by the scan — not a
value reconstructed from `reserve`/`scopeCeiling`. That is more accurate (it is the enforced
number, page-floored) and it removes any need to read `waiter.scopeCeiling`, which is
written unlocked at `:1142` and would otherwise be a data race. It is the scope's LOCAL cap
only, not an ancestor-aware effective cap; that is fine here, because the ancestor (the
slice) is already accounted for by `checkedAvailable`'s own `current` term.

`Cap` is a three-way reading (`confine_manage_linux.go:143-153`) and each value means
something different:

| `record.Cap` | meaning | charge treatment |
|---|---|---|
| nil (unreadable or malformed `memory.max`) | the enforced ceiling is **unknown** | record is **not usable** — hold the last charge (§3.3) |
| the literal `"max"` | positively known to have **no local cap** | usable, no clamp |
| a decimal string | the enforced ceiling | usable, clamp `newCharge` to it |

Treating a nil `Cap` as "no clamp" would be a fabricated reading, and it has a concrete
cost: a near-cap non-delegate scope would then charge `peak + margin` *above* its own frozen
reserve for as long as the read fails, and flap back the moment it recovers — breaking §4's
one-way invariant on a diagnostic failure. Note the `"max"` row cannot arise for a
connection-held **non-delegate** waiter: an admitted non-delegate scope is always given
`memory.max = reserve` (`confine_linux.go:855-867`), and an uncapped job is one that never
went through admission and so has no waiter here at all.

```
peakSoFar = max(peakSoFar, rss)                        // LIFETIME ratchet
growth    = 0; if havePrevRSS { growth = max(0, rss - lastRSS) }
lastRSS = rss; havePrevRSS = true
margin    = max(chargeMarginFloor, pctClamp(peakSoFar, chargeMarginPct), growth)
tracked   = addClamp(peakSoFar, margin)
if capFinite && tracked > cap { tracked = cap }
trackedRatchet = max(trackedRatchet, tracked)          // MONOTONE, excludes the cold floor
newCharge = max(trackedRatchet, coldFloor(waiter, now))
if capFinite && newCharge > cap { newCharge = cap }
```

**`trackedRatchet` is why the charge never oscillates** (Gemini second-lineage P0). Without
it the `growth` term makes the charge non-monotone: RSS 1 G -> 5 G gives
`margin = growth = 4 G` and a charge of 9 G; the very next flat scan gives `growth = 0` and
a charge of 5.6 G. A bursty job would then swing its charge by gigabytes every second, and
admission would grant neighbours precisely during the lulls — exactly when the estimate is
least conservative. Ratcheting the *tracked* term (and only that term — the cold floor must
still be able to expire, or the feature could never engage at all) makes the whole charge
monotone non-decreasing for a job's lifetime after its cold floor lapses. The cost is
conservatism: a job that once added 10 G in one second keeps that 10 G of budget. That is
the honest reading of a job which has demonstrated it can do so, and it is still clamped by
`cap`, so it degrades at worst to today's behaviour for that job.

- **`peakSoFar` is a lifetime ratchet.** The charge never falls below a level the job has
  already demonstrated, which removes the peak-drop-then-regrow race entirely. It is
  daemon-side max-across-scans, not `memory.peak`: portable, no kernel floor, forgotten
  across a restart (re-warmed by §3.4).
- **`growth` in the margin is the one-scan-race absorber, and it is self-tuning.** A job
  that grew 4 G in the last second is charged its peak + 4 G, so the next second's growth is
  already budgeted; a flat job is charged peak + 256 MiB. This is where the owner's ruling
  leaves the growth-race problem — at the *ledger*, not at a kernel throttle.
- **`coldFloor`**: `waiter.reserve` while `now - grantedAt < chargeColdFloorWindow`, else 0.
  One class-agnostic rule. It closes the real cold-start hole for the class that has one: a
  non-delegate job admitted on a 33.6 G estimate that has not allocated yet must not
  immediately free 33.6 G of ledger and then allocate 20 G. Deliberately **not** v3's
  delegate-specific `scopeCeiling` floor with a no-history basis flag — the delegate class's
  cold-start containment is now aitest's own per-worker ledger (AIRA-39/41/63), so that whole
  complication is dropped. **Stated honestly (Gemini second-lineage):** for a delegate waiter
  `reserve` is the pinned 512 MiB framework overhead, so the cold floor gives that class
  almost nothing during its first seconds of ramp. That is not a regression — a delegate
  scope charges a flat 512 MiB for its *entire* life today, whereas here it tracks upward
  from the first usable scan a second later — but it is a residual (§4g), not a claim of
  cold-start protection for that class.
  Note the cold floor is a FLOOR, never a ceiling: once `peak + margin` exceeds it the charge
  tracks upward through it in the same pass.
- All arithmetic uses the existing `addClamp` plus a new overflow-safe `pctClamp`.

Constants, each with a stated default: `chargeMarginFloor = 256 MiB`,
`chargeMarginPct = 12`, `chargeColdFloorWindow = 90s`. They are **compile-time**, not
env-tunable: three tuning knobs would be three more parsers, refusal paths and tests for
values that are reasoned about here and cheap to change with a rebuild.

**One env control does exist, and it is a kill switch rather than a knob:**
`AIRA_DAEMON_DYNAMIC_RESERVE=disabled` reverts the whole of AIRA-29 — the live charge *and*
the adoption margin — to the frozen-reserve behaviour. It earns its place where the tuning
knobs do not, because this change deliberately accepts a new bounded over-subscription on a
slice every session on the machine shares: reverting it must not require rebuilding and
redeploying a daemon under load at the moment it is misbehaving. An unrecognised value is
REFUSED at daemon start, never silently substituted (the AIRA-58 rule) — the operator
reaching for this variable is the last person who should be quietly given the behaviour they
were trying to turn off.

### 3.3 The one rule for when a charge may move

**Only a usable scan record may move a charge. Otherwise the last established charge stands
unchanged.**

"Usable" = this pass refreshed the scan **and** the scan succeeded **and** the waiter's
`scopeID` is present in `scanResult.Scopes` **and** that record's `RSSBytes != nil && >= 0`
**and** its `Cap` is established (a finite value or the literal `"max"`, never nil) **and**
the resulting ledger delta does not overflow (§3.1).

Consequences, all safe-direction:

- **grant -> `backend.Create` window** (every launch has it): no record yet, so the charge
  stays at the frozen `reserve`. This is the window that would otherwise let a job be
  charged ~0 before it exists.
- **scan failure / unreadable `memory.current`**: charges frozen at their last established
  values. No pass invents a number.
- **teardown before lease close**: charge holds its last value until release. Under-charge
  bounded by (reserve - lastCharge), and the memory is genuinely gone, so
  `checkedAvailable`'s physical `current` term covers reality.

Deliberately **not** gated on `record.Populated`. That gate exists in the *adoption* loop
(`:1463`) as a liveness heuristic and it is leaf-only: `BootstrapAitestSupervisor` drains
every pid out of an outer scope, so a fully busy suite reads `Populated == 0`
(`confine_manage.go:35-46`). A connection-held waiter already proves liveness, and
`memory.current` is hierarchical, so the reading is correct regardless. Gating on
`Populated` here would silently drop live scopes out of the charge — the one under-charge
direction this rule exists to prevent.

Chosen over "revert to `reserve` when the record is missing": reverting makes the ledger
oscillate by tens of gigabytes between adjacent passes on a transient scan gap, which would
flap admission. **Hold is not strictly equal to revert in safety, and the plan does not
claim it is** (Sol re-gate P1). There is one concrete state where revert would charge more:
a warm 33 G-capped job whose charge has been tracked down to 3 G, whose `memory.current`
then becomes *persistently* unreadable while it grows. Hold keeps it at 3 G; revert would
restore 33 G. `checkedAvailable`'s physical `current` term still charges whatever the slice
has actually allocated (`:1701-1706`), so what is genuinely un-charged is only that job's
growth after the last readable sample. Hold is chosen anyway — a per-pass 30 G ledger
oscillation on a transient diagnostic gap is a worse and far more frequent failure than a
bounded under-charge on a persistent one — and it is recorded as residual §4f, not as an
equivalence.

### 3.4 Post-restart adoption must track actual too

Non-delegate orphans currently adopt full `Cap` (`:1494`), so **every daemon restart**
re-pins every live job's full estimate until it exits — the headline win would regress on
every deploy. Generalise `:1481-1493` to all classes, and give it the same margin policy as
§3.2 rather than the bare 64 MiB constant:

```
adoptMargin = max(chargeMarginFloor, pctClamp(rss, chargeMarginPct))   // no growth term
if AgeSeconds == nil || *AgeSeconds < coldFloorWindowSeconds -> adopt cap
else if RSSBytes usable -> adopt min(cap, addClamp(rss, adoptMargin))
else -> delegate: skip (today's behaviour); non-delegate: adopt cap (today's behaviour)
```

**An unestablished age is treated as young, not as old** (Sol re-gate P1). `AgeSeconds` is
nil whenever the scope's `stat` could not be taken (`confine_manage_linux.go:93-98`), and a
clock rollback or skew can also make a brand-new scope's timestamp unusable. Reading an
unknown age as "warm enough to track actual" would hand a cold, not-yet-allocated job the
full under-charge this cold floor exists to prevent — the same fabricated-reading class the
rest of this file refuses. Unknown age therefore adopts the full cap.

The scope-directory age keys the cold floor without any daemon memory. Raising the delegate
adoption margin from 64 MiB to `max(256 MiB, 12%)` is a behaviour change for that class in
the **over-charge** (safe) direction, and is deliberate: it makes one margin policy, not two.

Adopted scopes get **no** `peakSoFar` ratchet and no `growth` term — the daemon has no
memory of their history and inventing one would be a fabricated reading. Stated as a
residual: an adopted scope's charge can fall and then have to climb again. That is exactly
today's `@dr-` behaviour, unchanged in kind.

### 3.5 No over-subscription factor bound (v4's Fork A — DROPPED)

v4 proposed gating admission on `Σ(scope memory.max) <= factor * ceiling` to make
"bounded" literal. **Dropped**, on Sol's P0 plus the simplicity rule:

- The existing scan cannot supply the cap total correctly. The adoption loop skips
  leaf-`Populated == 0` scopes (`:1463`) — i.e. every busy aitest outer scope — and skips
  non-finite caps (`:1474`), so `Σcap` derived from it silently under-counts and the "bound"
  would not bind. Deriving it correctly needs a *second*, subtree-live cap accounting.
- Every conservative treatment of the residual cases makes it worse. A scope with no finite
  `memory.max` (reachable: the flock fallback path launches uncapped when the daemon is
  unavailable — only admitted or declared jobs get a scope cap,
  `confine_linux.go:855-882`) has an unbounded cap; counting it honestly wedges the shared
  slice, and counting it as zero makes the bound porous. A wedge of this machine-wide slice
  is the worst failure mode in this subsystem — AIRA-101's entire design exists to make one
  unrepresentable.
- The owner's ruling already names the containment, and it is kernel-enforced. Being precise
  about which cap (Sol re-gate P2): every confine launch requires a finite **effective**
  (ancestor-inclusive) cap (`confine_linux.go:456-461`), and every *admitted* scope
  additionally gets its own finite **local** `memory.max` (`confine_linux.go:855-882`) — the
  uncapped fallback scopes named above have only the former. So aggregate demand is bounded
  by `Σ(local memory.max)` **for the admitted population**, and by the slice's own finite cap
  for everything else; it is never unbounded, but it is not `Σ(local memory.max)` across the
  board. That, plus the deployed AIRA-27 500/800 `oom_score_adj` class steering, is the
  containment.
- Architectural simplicity is a HARD project rule.

**The residual is therefore stated, not bounded by a new mechanism** (§4e), and filed as its
own ticket rather than silently dropped.

### 3.6 Scoped OUT, with reasoning: dynamic per-scope `oom_score_adj` steering

v3 §3.6 proposed a daemon-wide loop raising a bursting scope's `oom_score_adj` toward 1000
on an `RSS - effectiveCharge` baseline, with a recursive subtree walker and a restore-down
pass. **Not built.** Reasons:

1. **The owner's named containment already exists and is deployed.** The ruling says
   containment is `memory.max` plus *steered* `oom_score_adj`. AIRA-27 (`2e8f237`,
   deployed) is that steering: non-delegate 500, delegate 800, set by the confined child at
   exec (`confine_linux.go:2092`) and inherited by descendants (`:36-37,1752-1774`). This
   change does not weaken it, and every confined job remains a hugely preferred victim over
   anything unconfined.
2. **The proposed trigger is near-inert within the existing scan pass.** With
   `charge = min(cap, max(coldFloor, peakSoFar + margin))` and `rss <= peakSoFar` by the
   ratchet, `rss - charge > 0` is reachable in the same pass only in the narrow window where
   `rss > cap` (`memory.current` can transiently exceed `memory.max`) — which is *not* the
   burst-race population the steering exists to catch. (v4 claimed this was structurally
   unreachable; Sol correctly refuted that, and the corrected claim is what stands.)
   Catching the intended population needs a *separate, faster-than-1s* loop reading RSS
   between charge updates: a new daemon subsystem, plus a recursive child-cgroup pid walker
   that does not exist (`Members()` is leaf-only, `cgroup_linux.go:248`), plus per-pid
   `/proc/<pid>/oom_score_adj` writes at >1 Hz, plus a restore-down re-walk, plus
   real-cgroup tests in both directions.
3. **Its purpose is served in a simpler place.** The `growth` term in §3.2's margin catches
   the same population — a scope outrunning what admission accounts for — at the *ledger*,
   one interval earlier, with no new subsystem and no `/proc` writes.
4. **Architectural simplicity is a HARD rule** in this repo. The marginal effect of the
   proposed loop is a `<=1 s`-stale bias between two already-high adj values.
5. The residual is **written down, not dropped**: filed as its own ticket at build time.

Empirically checked, because permission would have been a blocking objection: a uid-1000
process with `CapEff: 0` **can** lower another process's `oom_score_adj` through
`/proc/<pid>/oom_score_adj` (probe on this kernel: 0 -> 700 -> 300, all permitted; the
`CAP_SYS_RESOURCE` restriction applies only to the legacy `/proc/<pid>/oom_adj` file). So
restore-down is *feasible* — the reason not to build it is proportionality, not permission,
and the plan must not claim otherwise.

### 3.7 Explicitly not doing

- **Any `memory.high` writing, per-scope or slice-wide.** Owner-foreclosed, and
  independently foreclosed by AIRA-103's own design principle: "nothing kernel-enforced ever
  moves, which makes 'this mechanism can never pressure a running job' structural rather
  than something to prove" (`sliceceiling.go:46-53`).
- **`internal/daemon/worker_admit.go`.** The 4/5 PSI rule lives there and is AIRA-35's
  territory, being built concurrently. Untouched.
- **`memory.max` re-writing.** Stays the per-scope hard cap and sole containment.
- **Making the adoption loop subtree-live** (`:1463`'s leaf-`Populated` skip, already
  labelled "a v2 item" at `:1462`). It is a pre-existing under-count in the safe direction;
  fixing it would *tighten* admission for the aitest class and deserves its own ticket.

### 3.8 Class scope: both, and why

The dynamic charge applies to every scope-backed waiter, delegate and non-delegate. The
formula is class-agnostic and the code path is identical; excluding delegate would leave the
*larger* documented over-commit hole open (a delegate scope's `memory.max` is a generous
ceiling, not its reserve, so a suite already grows past what the slice ledger accounts for —
stated in `core/skill.go`'s own guidance). Including it does not double-book against
AIRA-39/41/63's worker ledger: that ledger accounts for *children of* the outer scope
against the outer scope's own cap, while this one accounts for the outer scope against the
slice. And it is not a utilisation regression for delegate, because the slice's physical
`current` term in `checkedAvailable` already charged that usage; only the margin is new.

## 4. Safety and invariants

- **Utilisation:** a warm scope charges `~peak + margin <= cap`, so `Σcharge` tracks
  physical usage and admission stops near real capacity instead of `Σpeak`.
- **Ledger conservation:** `outstanding = Σ effectiveCharge`; returns to exactly 0; never
  negative; `residualBytes() == 0` and `residualJobs() == 0` throughout.
- **Fail-closed:** no usable scan record -> last established charge stands (§3.3); slice
  read fails -> waiters stay queued (`:1507-1516`, unchanged); scan fails -> no charge moves;
  peak forgotten across restart -> §3.4 cold floor by scope age.
- **One-way for the non-delegate ledger:** `newCharge <= cap`, and for a non-delegate scope
  the kernel cap *is* the admission reserve (`confine_linux.go:855-867`), so that class's
  charge can only ever be <= today's.
- **Residuals, stated:**
  (a) a genuine aggregate-full case — many within-cap scopes racing `Σ(actual)` past the
  slice cap faster than one scan — can still fire a slice OOM, biased only by AIRA-27's
  class steering;
  (b) a late-peaking job over-reserves after its own peak, bounded by its demonstrated peak;
  (c) an adopted (post-restart) scope has no peak ratchet and no growth term;
  (d) a sub-scan-interval burst outruns the `growth` margin;
  (e) **the over-subscription factor is not bounded by any new mechanism (§3.5)** — the
  aggregate is bounded only by `Σ(local memory.max)` over the *admitted* population plus the
  slice's own finite cap for everything else (§3.5's per-population statement), and that sum
  can exceed the slice cap.
  The concrete failure path (Sol): several scopes expand between scans until `aira.slice`
  reaches its own finite cap, producing a **memcg** OOM inside the slice, biased by the
  deployed 800/500 class steering — not an uncontained host OOM. This is the property the
  currently-airtight non-delegate class gives up, and it is the owner's explicit ruling.
  Filed as its own ticket;
  (f) a scope whose `memory.current` or `memory.max` becomes *persistently* unreadable holds
  its last charge and so under-charges its subsequent growth (§3.3), bounded below by the
  slice's own physical `current` term;
  (h) **the post-restart age gate is a heuristic, not a proof of age.** `AgeSeconds` comes
  from the SCOPE ID stamp, and a scope id is minted *before* admission, so a job that
  queued half an hour and started moments ago already reads as old. If the daemon restarts
  inside that window such a job is adopted at its live usage rather than its full estimate.
  The alternatives are worse (directory mtime was refuted for this purpose by the AIRA-64
  real-cgroup tier; per-scope history is exactly what a restart destroys), and the residual
  is bounded by one scan interval's growth because adoption is re-derived every pass — the
  same bound the steady-state path carries;
  (g) the cold floor is only materially protective for the **non-delegate** class, whose
  `reserve` is the whole-job estimate. A delegate waiter's `reserve` is the pinned 512 MiB
  overhead, so its first seconds of ramp are under-charged until the next scan observes them
  — strictly better than today's flat-512 MiB-for-life, but not cold-start protection.

**Two things that are deliberately safe rather than defended by new machinery**, both
raised by the second lineage and both checked against the source rather than argued:

- **The 90 s cold-floor cliff cannot over-grant a herd.** When a job's floor lapses and tens
  of gigabytes of ledger free at once, the grant loop recomputes `jobs`, `headroom` and
  `available` *per waiter* against the already-updated `queue.outstanding` (`:1586-1588`), so
  a burst of grants in one pass is serialised against the ledger exactly as a burst across
  passes would be — and `checkedAvailable`'s `max(effectiveCurrent, ...)` still floors the
  whole thing at real physical usage. Cliffs are also per-job, keyed on each waiter's own
  `grantedAt`, so they stagger in practice. No decay curve is added.
- **The lock-free scan window cannot resurrect a departed scope.** The charge update
  iterates `queue.waiters` and looks records up by `scopeID`, never the reverse. A waiter
  released while the scan was running is already out of `queue.waiters` by the time the lock
  is taken, so its stale record matches nothing; a waiter granted during that window is not
  yet in the scan, so it is "not usable" and holds its `reserve` (§3.3). The direction of the
  join is what makes this safe, and it is therefore load-bearing, not incidental.

## 5. Tests (TDD; each asserts behaviour, and each names its false-pass guard)

1. **Charge tracks actual (unit):** warm held waiter, RSS 2.6 G, reserve+cap 33.6 G ->
   `outstanding` equals the **exact** `peak + max(floor, 12%, growth)`, and is strictly
   less than cap. *Guard:* asserting only "< reserve" would pass against an
   always-charge-cap/2 implementation.
2. **Cold floor (unit), both halves:** within the window with RSS ~0 the charge equals
   exactly `reserve`; one tick after the window it equals exactly `peak + margin`.
   *Guard:* the warm half alone passes against an implementation with no cold floor at all,
   so the fresh half asserting the exact `reserve` is required.
3. **Lifetime ratchet (unit), exact:** RSS 1 G -> 8 G -> 1 G; after the third scan the
   charge equals exactly the value established at the 8 G sample and is **strictly below
   cap**. *Guard:* "not below 8 G" passes against frozen-at-reserve and against always-cap;
   the exact equality plus the strict-below-cap assertion kills both.
4. **Growth margin (unit), exact:** a scope going 1 G -> 5 G in one interval charges exactly
   `5 G + 4 G` (growth dominates floor and pct). *Guard:* "at least peak+4 G" passes against
   always-cap; the exact value kills it. Also asserts `havePrevRSS` makes the FIRST sample's
   growth 0, not `rss`.
4b. **Monotone charge — no oscillation (unit):** the same scope's *next* flat scan
   (`growth == 0`, so the raw formula would give `5 G + 600 M`) must leave the charge at
   exactly `9 G`, unchanged. Then a drop to RSS 2 G must also leave it at exactly `9 G`.
   *Guard:* this is the only test that fails against the un-ratcheted formula, which is a
   correct-looking implementation that flaps the shared ledger by gigabytes every second.
5. **Conservation (unit):** grant/scan/scan/release across several waiters ->
   `outstanding == 0` exactly at the end and never negative at any intermediate point; a
   release taken *between* scans subtracts the last effective value; a waiter with no
   `scopeID` stays static at `reserve` throughout.
6. **Residual invariant (unit):** `admitSliceSnapshot().residualBytes() == 0` and
   `residualJobs() == 0` after dynamic passes, **plus a direct assertion that
   `vanishedBytes == effectiveCharge`** for a `scopeVanished` waiter. *Guard:* `residualBytes()`
   does not include `vanishedBytes` (`:780-782`), so residual alone would pass against an
   implementation that left `vanishedBytes` summing `reserve` — the direct assertion is what
   kills that.
7. **The §3.3 rule (unit), six cases:** no record (grant-before-create); `RSSBytes == nil`;
   scan error; `Cap == nil` with a usable RSS; a delegate record with `Cap == "max"` and a
   usable RSS; and a record with `Populated == 0` but a usable RSS. The `"max"` and
   `Populated == 0` cases must **still** update (the latter is the anti-regression assertion
   for the leaf-`Populated` trap); the other four must leave the charge byte-identical to its
   previous value.
8. **Post-restart adoption (unit):** a non-delegate orphan older than the window with usable
   RSS adopts exactly `min(cap, rss + max(256 MiB, 12%))`, not full `Cap`; a younger one
   adopts full `Cap`; one with **nil `AgeSeconds`** but a usable low RSS and a finite cap
   adopts full `Cap` (unknown age is young, never warm); one with unusable RSS adopts full
   `Cap`; a delegate orphan uses the same margin (not 64 MiB).
9. **Overflow and clamping (unit):** `pctClamp` and `applyChargeDelta` at `math.MaxInt64`
   inputs never produce a negative or wrapped charge; specifically, an increasing delta that
   would overflow **refuses** — with a second accounted waiter present, `outstanding` and
   the refusing waiter's `effectiveCharge` are both unchanged, and after releasing **every**
   waiter `outstanding` is exactly 0. *Guard:* a saturating implementation passes a
   single-waiter test and only goes negative once the second waiter releases.
9b. **`rss > cap` (unit):** a record whose `memory.current` exceeds its `memory.max`
   (reachable transiently) charges exactly `cap`, never `rss`, and never a negative value.
10. **Real cgroup, Linux — the load-bearing test.** Build a real slice with a real
    `.aira-CONFINE-*` scope via `internal/cgrouptest`, place a real process in it that
    actually allocates, run the **production** `runner.ListConfines` scan through
    `evaluateAdmitQueue`, and assert (a) the charge falls from the frozen reserve to within
    the margin of the kernel's own `memory.current`, and (b) a second waiter that did
    **not** fit before the drop is granted after it. This is what proves the mechanism is
    not inert against a real tree — the AIRA-59/AIRA-64 lesson. Every other test here runs
    against a seam and would keep passing if the real scan could never establish a reading.
11. **Kill switch (unit), both halves:** with `dynamicReserve` off, a held waiter charges
    exactly its frozen reserve and the adoption margin reverts to the pre-AIRA-29 64 MiB —
    a switch that reverted the charge but left the margin changed would be a partial
    rollback presented as a whole one. Plus `dynamicReserveFromEnv` refusing an
    unrecognised value with a stable code rather than substituting a default.
12. **Unreconstructable delegate orphan (unit):** a delegate scope with no readable
    `memory.current` contributes neither bytes nor a headroom job — its cap is a
    containment ceiling, not an estimate. This is the one adoption arm no other case
    reaches.
13. Full `go test ./...` green, plus `go test -race ./internal/daemon/` for the new
    concurrency-adjacent fields (AIRA-20 keeps `-race` out of CI for wall-clock-tight
    flakes; run it targeted and record the exact exit code either way). `go vet ./...`.
14. **Mutation check, not just green:** every load-bearing behaviour above is verified by
    deliberately breaking the implementation and confirming a test fails — the
    trackedRatchet, the `Populated` gate, both ledger sites, both snapshot sums, the cold
    floor, the growth term, the cap clamp, the overflow refusal, the adoption age gate and
    the first-sample growth. A green suite is not evidence that a test would have caught
    the wrong build.

## 6. Rollout

Daemon-only change (`internal/daemon/admit.go` plus tests). No wire, schema, CLI or install
change; `confine --list`'s granted line simply becomes an honest dynamic sum. Requires a
daemon restart to take effect; §3.4 makes post-restart reconstruction track-actual so the
restart itself does not re-pin every estimate. Deploy is the coordinating session's, not
this build's.

## 7. Deferrals / open

- Dynamic per-scope `oom_score_adj` steering (§3.6) — filed as its own ticket.
- An explicit aggregate over-subscription bound (§3.5, residual 4e) — filed as its own
  ticket, since it needs a subtree-live cap accounting and an uncapped-scope policy that do
  not exist today.
- Subtree-live adoption (`admit.go:1462`'s own "v2 item") — pre-existing, safe-direction.
- AIRA-25 (peak/delta ledger split) is subsumed by this and should be closed.

## 8. Disposition of the Sol plan-review (v4 -> v5)

| Sol finding | Disposition |
|---|---|
| P0 `scopeCeiling` written unlocked at `:1142`, would race | **Fixed by removing the read.** §3.2 clamps to the scan record's own parsed `Cap` (the enforced `memory.max`), so no new reader of `scopeCeiling` exists. |
| P0 `adoptedCaps` cannot enforce the factor (leaf-`Populated` skip, non-finite caps) | **Accepted; the factor bound is dropped entirely** (§3.5), with the residual stated (§4e) and ticketed. |
| P1 `factor = 0` would permit unbounded over-subscription | Moot — dropped. |
| P1 factor total must be a running local total per grant | Moot — dropped. |
| P1 `chargeTracked` underspecified | **Fixed.** Replaced by `havePrevRSS` with an exact stated meaning and a test (§5.4). |
| P1 overflow-safe arithmetic needed | **Fixed.** `pctClamp` + `applyChargeDelta`, non-floored by design, with an explicit overflow test (§5.9). |
| P1 `rss - effectiveCharge > 0` claimed structurally unreachable — false | **Corrected** in §3.6.2: reachable only in the narrow `rss > cap` window, which is not the target population. The scope-out decision stands (Sol P2 agrees). |
| P1 adopted scopes keep the bare 64 MiB margin | **Fixed.** §3.4 uses the same floor/pct margin policy; the delegate change is over-charge (safe) and deliberate. |
| P1 tests 3/4 porous ("not below" / "at least") | **Fixed.** §5.1-5.4 assert exact charges plus strictly-below-cap, and name the guard each kills. |
| P1 add `-race` | **Fixed.** §5.11. |
| P2 scan/lock structure and conservation model sound | Acknowledged; §3.1 states the lock discipline explicitly. |
| P2 non-delegate cap == reserve only modulo page-flooring / lower ancestor cap | Moot — the plan now reads the kernel's own value rather than reconstructing it. |

## 8b. Disposition of the Sol re-gate (v5 -> v6)

| Sol re-gate finding | Disposition |
|---|---|
| P1 a saturating `applyChargeDelta` breaks conservation (clamped `outstanding` vs `Σ charge`, then negative on release) | **Fixed.** §3.1: the function REFUSES on an overflowing increase and the caller leaves both ledger and charge untouched; §5.9 tests it with a second accounted waiter and a full release to exactly 0. |
| P1 `residualBytes()` excludes `vanishedBytes`, so §5.6 was porous | **Fixed.** §5.6 adds a direct `vanishedBytes == effectiveCharge` assertion and names the guard. |
| P1 "hold" is not equally fail-safe; names the persistent-unreadable growing-scope state | **Accepted; claim corrected.** §3.3 states the concrete state and why hold is still chosen; recorded as residual §4f. |
| P1 an unreadable `Cap` lets the charge move unclamped | **Fixed.** §3.2 makes `Cap` a three-way reading: nil -> not usable (hold), `"max"` -> usable and unclamped, finite -> clamp. §3.3 folds it into "usable". |
| P1 tests omit `Cap == "max"`, unreadable `Cap`, `rss > cap`; overflow test needs a second waiter | **Fixed.** §5.7 now has six cases, §5.9 has the second waiter and full release, §5.9b covers `rss > cap`. |
| P2 dropping the factor bound is not a P0; names the concrete memcg-OOM failure path | **Accepted.** Recorded verbatim as residual §4e. |
| P2 the record's `Cap` is the LOCAL `memory.max`, not ancestor-aware | **Claim corrected** in §3.2; the ancestor is already covered by `checkedAvailable`'s `current` term. |

## 8c. Disposition of the Sol re-gate of v6

| Sol finding | Disposition |
|---|---|
| P1 adoption treats `AgeSeconds == nil` as warm, reopening the cold-start under-charge | **Fixed.** §3.4's rule is now `AgeSeconds == nil \|\| *AgeSeconds < window -> adopt cap`, with the reasoning stated and an exact test added (§5.8). |
| P2 §3.5 prose conflates the finite *effective* ancestor cap with a finite *local* `memory.max` | **Claim corrected** in §3.5: the launch precondition is the effective cap; only admitted scopes also get a local one; the bound is stated per-population. |

## 8d. Disposition of the second-lineage (Gemini) review of v7

Sol had repo access and passed at v7; this lineage had the design inline only. Two findings
survived grounding, three did not — each checked against the source rather than argued.

| Finding | Disposition |
|---|---|
| P0 the `growth` term double-counts against `peakSoFar`, so the charge jumps by ~2Δ and then drops back the next flat scan — "artificial wedges followed by flap-releases" | **REAL, and fixed.** §3.2 adds `trackedRatchet`, making the charge monotone non-decreasing after the cold floor lapses. New test §5.4b is the only one that fails against the un-ratcheted formula. |
| P0 the delegate cold floor is the pinned 512 MiB, leaving a delegate job "unshielded" up to its 48 GiB cap for 90 s | **Partly real; the framing is wrong, the residual is recorded.** A floor cannot clamp a charge downward — once `peak + margin` exceeds it the charge tracks up through it at the next scan, ~1 s later, not 90 s. And it is not a regression: a delegate scope charges a flat 512 MiB for its whole life today. Recorded as residual §4g and stated in §3.2. |
| P1 the 90 s cliff frees tens of gigabytes at once and triggers a thundering-herd grant burst | **Refuted, and the reason written down.** `available` is recomputed per waiter against the already-updated `outstanding` (`admit.go:1586-1588`), and `checkedAvailable` still floors at real physical usage, so a burst inside one pass is serialised exactly as one across passes. Cliffs are per-job and stagger. No decay added. |
| P1 a waiter released during the lock-free scan window leaves stale scan metrics that must be dropped | **Refuted, and the reason written down.** The update iterates `queue.waiters` and looks records up by `scopeID`, never the reverse; a released waiter is already out of the list under the same lock, so its record matches nothing. The join direction is now stated as load-bearing (§4). |
| P2 overflow refusal leaves `outstanding` misaligned with physical state, potentially wedging | **Refuted.** Refusal is what *preserves* `outstanding == Σ effectiveCharge`; it is a defensive path that needs a charge near `MaxInt64`, unreachable with a 64 GiB slice cap. Admission is floored by the physical term regardless. |

## 9. Disposition of the adversarial build review (Sol, against commit `744dc56`)

A genuinely different pass: the plan gate reviewed a design, this one read the landed code
in both the false-pass and false-fail directions and was told not to re-litigate the two
scoped-out decisions. Verdict BLOCK, 2 P1 + 5 P2. Every finding was ground-checked against
the source before acting; none were dismissed, and none were accepted on the reviewer's
say-so alone.

| Finding | Ground check | Disposition |
|---|---|---|
| **P1** a scope's live hierarchical `memory.current` is charged while its `confine-reserve` children still charge their full reserves — a DOUBLE-BOOK that can refuse a healthy 4 GiB job with ~32 GiB physically free | **Confirmed.** `confine-reserve` is live (`admit.go` `parentScopeID`, `pylib/env.go`); sub-reservations are scope-less waiters in the same queue; a delegate parent's own reserve is the small pinned overhead precisely because the children carry the real charge, so before this change there was no double-book | **Fixed.** A scope with live sub-reservations is excluded from dynamic charging (§3.1); new test asserts the parent keeps its overhead, the ledger stays at overhead+children, the child is listed *before* its parent, and the exclusion un-latches on release |
| **P1** the post-restart age gate reads the SCOPE ID stamp, which is minted before admission, so a long-queued job that just started reads as warm | **Confirmed** (`confine_manage_linux.go` derives `AgeSeconds` from the stamp; `confine_linux.go` mints the id before the admit call) | **Claim corrected, not papered over.** The alternatives are worse (directory mtime already refuted by the AIRA-64 tier; per-scope history is what a restart destroys), and the residual is bounded by one scan interval because adoption is re-derived every pass. Written into the function's own doc and residual §4h |
| **P2** the effective charge is not monotone across a `memory.max` change, contradicting the commentary | Reachable only by an external rewrite of a scope's cap; nothing in AIRA moves a kernel-enforced value after launch (AIRA-103's own principle) | **Comment scoped** to state the fixed-cap assumption rather than claim unconditional monotonicity |
| **P2** `pctClamp`'s overflow branch drops a remainder, and the test only checked broad bounds | **Confirmed porous** — a 12x-too-large version passed `got >= 0` | **Fixed.** The test now asserts the overflow branch agrees with the exact answer to within its stated error (a remainder below `pct`), computed by splitting the multiply so it does not restate the implementation |
| **P2** the env test's "unset" case actually sets the variable to `""` | **Confirmed** — `t.Setenv` cannot unset | **Fixed.** `t.Setenv` registers the restore, then `os.Unsetenv` gives a genuinely absent variable, so absent and empty are distinguished |
| **P2** nothing proves `Serve` actually applies the parsed kill switch | **Confirmed, and pre-existing for all four of this file's env knobs** — `admitFreezeMaxHoldFromEnv`, `admitBackfillGrace`, and the watch/poll intervals are all tested at the parser and never through `Serve` | **Accepted coverage gap, written down rather than singled out.** Testing one of the four through `Serve` would need a socket, a DB and a live daemon for a one-line assignment; the honest fix is one test covering all four wirings, which belongs to its own change |
| **P2** `TestDynamicChargeConservesTheLedger` seeds granted waiters rather than tracing a real grant, so a grant that set `chargeTracked` with a zero charge would pass | **Confirmed porous**, and this is the dangerous grant→scope-creation window | **Fixed.** `TestDynamicChargeFreesSpaceForAQueuedWaiter` drives a real grant and now asserts the granted waiter is UNTRACKED and charges exactly its reserve; a new mutant ("grant marks the waiter tracked with a zero charge") pins it |
| **P2** the modified pre-existing reconstruction test asserts only an aggregate total, so equal-and-opposite class errors cancel — while its new comment claimed the assertions were separate | **Confirmed; the comment overstated what the test did** | **Fixed.** Split into two independent sub-tests, one scope each, so neither class can hide behind the other |

**Mutation battery after the fixes: 18 deliberate breakages, 18 caught, 0 survivors.**
