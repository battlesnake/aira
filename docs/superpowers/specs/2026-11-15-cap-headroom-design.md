# Cap headroom for auto-sized confine jobs: book the estimate, cap above it

Ticket: AIRA-347 (held, milestone v0.45; AIRA-324 blocks it). Status: plan, not built.
Companion to [`2026-11-13-history-charge-design.md`](2026-11-13-history-charge-design.md)
(AIRA-323..326), whose section 5.2 (AIRA-324) splits the booking from the cap for **pinned** jobs.
This document does the same for **auto-sized** jobs. It is a separate file, not a fifth revision of
that spec, because that spec carries AIRA-323's plan-gate record and binding build amendments and
should stay stable; the two share machinery only at the points listed in section 7.

Process class: **correctness-critical admission core** (it changes what the kernel enforces for
most unpinned jobs and what the estimator learns): full two-loop, Opus builds TDD, Fable reviews in
a throwaway detached worktree.

Reproduction: `docs/dev/aira-cap-headroom-replay.py` on a copy of the state DB (never the live
file); its output is committed as `docs/dev/aira-cap-headroom-replay.out.txt` with the snapshot's
identity.

**Recommendation, in one paragraph.** Build it, in two steps, and only with three conditions met.
The rule as proposed (book C, cap L = min(k x C, physical free), k = 2) is sound for the kills it
targets: on this box's history a proxy puts 45-81% of auto-sized OOM kills within reach of
k = 2 (a proxy range, not bounds: the killed run's own need is censored, section 2) without raising
any booking. But as
written it has one silent defect: **with the current estimator it turns AIRA-318's 15%-per-run
page-cache ratchet into a doubling per run** (section 4.1, by arithmetic, and 1,715 signatures are
exposed), and AIRA-318's own proposed rule does not stop it at k > 1. So step 1 is a learning fix
(store the booking per row; a clean run that reached its limit never raises the booking above the
booking it ran under), shippable alone at k = 1, where it also closes AIRA-318's ratchet. Step 2 is
the headroom itself. Conditions: (a) step 1 first; (b) OOM steering switched on before k > 1 is
enforced, because it is **off on this box** and without it a slice-level OOM kills the largest job,
not the one using headroom (this applies to AIRA-324 too, whose spec cites steering as a
mitigation); (c) the owner decides whether cold-start jobs are included (68% of auto kills have no
retained earlier row, section 2). Also (d): AIRA-341 (delete the p90 prior, already un-held) lands
before k > 1, because that prior reads raw maxima across every signature and would carry
cache-filled peaks into cold-start bookings (section 4.2).

## 1. The problem, restated

An auto-sized confine job (no `--memory-reserve`/`--memory-max`) gets one number from the daemon,
its reserve, and the client uses it twice: as the **booking** the admission ledger holds against
the slice, and as the scope's `memory.max` (`confine_linux.go:1125-1127`, cap-source
`auto:daemon-reserve`). So a job that needs a little more than its estimate is OOM-killed even
when the slice has tens of gigabytes of real free memory, most of it unused space inside other
jobs' bookings. Each kill is a wasted run and, for a gate, a spurious red leg. The kill also
teaches the estimator almost nothing: a run killed at its cap records a peak equal to the cap
(the data is censored: the need is only known to be above it), so the estimator climbs 1.5x per
kill (AIRA-273) rather than to the real need.

Owner direction (2026-11-15): the number that **admits** a job and the number that **limits** it
should differ; the limit should be higher than the booking. AIRA-324 already does this for pinned
jobs (charge 0.8 x pin, limit = pin). This extends it to jobs AIRA sizes itself.

## 2. Evidence

Snapshot: a `.backup` copy of the live `state.db` taken 2026-11-15T14:26Z, 234,630 confine rows,
latest `at` 2026-11-15T12:26:52Z. Script exit 0; full output in the `.out.txt` file. All rows are
`kind='confine'`. "Auto" means cap-source `cap:auto:daemon-reserve` (the `budget_basis` column);
for these rows `budget` is the cap, which today equals the booking.

### 2.1 The quoted figures, re-derived

| Measure | Quoted (task) | Re-derived |
|---|---|---|
| OOM rows with a recorded budget | 4,208 | 4,210 |
| ... whose signature has a clean run recorded | 2,792 | 2,795 |
| ... clean max peak <= 1.5x the killed budget | 1,509 | 1,510 |
| ... <= 2x | 2,083 | 2,084 |
| ... <= 4x | 2,644 | 2,648 |
| distinct signatures among OOM rows with a budget | 3,515 | 3,517 |

The small differences are rows written between the two snapshots (the store is live). The quoted
figures cover **every** cap source; the rule touches only auto rows, and the quoted test ("clean
max peak <= f x killed budget") is not an estimate of kills saved, for the reason in 2.2.

### 2.2 The honest estimate for auto rows

2,355 auto OOM rows (1,488 since 2026-10-20, against 33,013 auto jobs in that period: a 4.5% kill
rate). A kill proves the run needed **more than** its cap, so a clean peak at or below the killed
cap says nothing about it; the quoted test counts such peaks as "fits", which is why its "clean
runs before the kill" variant is flat at about 25% for every k (output, part 2). The usable split,
by the largest clean peak P of the same signature at any time:

- **within k**: P in (cap, k x cap]. Some run of this command needed more than the killed cap and
  still fitted k x cap; the proxy counts the kill as saved.
- **beyond k**: P > k x cap. A mode of this command does not fit even k x cap; counted as not
  saved, although the killed run may have been a smaller mode that k would have saved.
- **undetermined**: no clean peak above the cap (including 642 rows with no clean run at all).
  The need is censored; the high end of the range counts these as saved.

These are **proxy scenarios, not bounds**: a "within" kill may have been a bigger mode than any
run that ever completed, and a "beyond" kill may have been a small one.

| Population (auto OOM rows) | Rows | k = 1.5 within / beyond / undet. | k = 2 | k = 3 |
|---|---|---|---|---|
| all auto | 2,355 | 28.8 / 35.6 / 35.6% | **45.1 / 19.3 / 35.6%** | 56.6 / 7.8 / 35.6% |
| history rungs (own-max, estimate:max, oom-escalated) | 760 | 38.0 / 37.6 / 24.3% | **53.7 / 22.0 / 24.3%** | 65.8 / 9.9 / 24.3% |
| no retained earlier row ("cold start") | 1,595 | 24.4 / 34.6 / 41.0% | **41.0 / 18.0 / 41.0%** | 52.2 / 6.8 / 41.0% |

So at k = 2 the proxy range is 45-81% of auto kills. The category is a **retained-evidence
category, not the estimator's actual basis** (the basis is not stored; the real escalation can lose
to the ordinary estimate or the own-max floor): it is reconstructed walk-forward from **retained** rows (the store keeps 20 per signature), so "no
retained earlier row" also contains runs whose history was evicted; 978 of its 1,595 rows were
killed at exactly 1 GiB, the client default (AIRA-304). The owner's direction named history-sized
jobs; **two thirds of auto kills have no retained history behind them** (section 3, decision D1).

A live instance, seen while writing this (2026-11-15, this worktree): `make fmt-check vet` was
auto-sized at 653M (`estimate:max`, n = 5) and OOM-killed at that cap with the box showing about
50 GB `MemAvailable`; the identical re-run (`estimate:oom-escalated`, 980M) finished clean with a
peak of 658M. The killed run needed a few megabytes more than its estimate; k = 1.5 would have
saved it, and its next booking would have stayed near 653M instead of 980M.

The 1.5x-per-kill creep (AIRA-273), measured: of the runs of consecutive auto OOM rows of one
signature that ended with a clean run (any cap source ends a run; a non-auto OOM restarts it), 1,267
had one kill, 131 two, 10 three, 3 four and 1 five. Multi-kill creep is real
(RANT-136, RANT-109, qual's lint gate) but the commonest auto kill is a single one.

### 2.3 Limits of this evidence

- **Page cache.** `peak_rss` is the scope's `memory.peak`, which counts page cache (AIRA-318). A
  clean peak is an upper bound on anonymous need, so "within k" can be pessimistic (a
  cache-inflated P lands "beyond"), and a peak at the cap is itself censored. Restricting to
  clean runs below 95% of their own cap moves the quoted-style figure by about 2 points (output,
  part 2), so cache-filled caps are not what drives it.
- **Censoring.** The killed run's own need is never known. "Within k" assumes it was the mode of
  the largest clean run; a killed run of a bigger, never-completed mode is counted wrongly in
  either direction.
- **Retention.** 20 rows per exact signature: frequent commands have lost rows, the rung
  reconstruction sees only retained rows, and an eviction can hide a clean big-mode run.
- **Bigger pins after a kill.** The clean runs that dominate the "within" figures mostly came
  after the kill (the after-kill-only variant is within 3 points of "any"), often under an
  escalated cap or an operator pin, so they ran under a cap bigger than the killed one. That is
  why only P above the killed cap is counted, and why it is a proxy, not a replay.
- **Bimodal commands.** A command whose modes differ by more than k (qual's 17 MB vs 417 MB lint
  gate, 24x) is "beyond k" and keeps AIRA-273's creep, starting from a cap k times higher.
- **Failed runs** are not recorded (AIRA-264); their need is invisible.
- **Kill attribution is not stored.** `oom = 1` is recorded for any OOM kill under the scope,
  including a kill under a descendant's own limit (a container's `--memory`) and a kill under an
  ancestor's limit (the slice); the trailer tells them apart (`confine_linux.go:1509-1518`) but the
  row does not. A larger scope cap cannot prevent the descendant kind and may not prevent the
  ancestor kind, so the kill rate and every "saved" figure above include kills headroom would not
  have touched. Their share is **unmeasured**; the step-2 metric counts own-limit kills only
  (section 8).

### 2.4 The downside: the slice running over its ceiling

Twenty auto jobs drawn together (since 2026-10-20). Use under k: a clean run's peak is unchanged
(variant "cache": a run at >= 95% of its cap fills its whole k x C limit with page cache); a
killed run uses min(k x C, the largest clean peak above its cap), or the whole k x C when no such
peak is known (worst case, censored). The ratio is total use over total booking. When the ledger
is full, total booking is about the admission ceiling, so the share of draws with ratio > 1 is a
**model** of how often the jobs' own use would push a full slice above its (possibly throttled)
ceiling. It is a model only: a draw is 20 arbitrary finished rows (a median 20.6 GiB of bookings),
not a ceiling-saturated admitted population (about 43 such jobs at a 45 GiB ceiling); the ratio is
scale-free, but the spread of a 43-job total differs from a 20-job one in either direction (it
narrows around the group's mean, which raises the exceedance when that mean is above 1), so the
figures are scenario outputs under these assumptions, not bounds, and say nothing about who was
actually running together. "OOM-command burst" draws
20 jobs of one command+subcommand that has an OOM on record (53 such groups).

Counts are draws over 1 out of 4,000; then p99 and max of the ratio.

| k | cache | mixed n / p99 / max | burst n / p99 / max | OOM-command burst n / p99 / max |
|---|---|---|---|---|
| 1 (today) | no | 0 / 0.56 / 0.71 | 0 / 0.72 / 0.82 | 0 / 0.92 / 1.00 |
| 1.5 | no | 0 / 0.59 / 0.75 | 0 / 0.76 / 0.88 | 72 / 1.37 / 1.50 |
| 2 | no | 0 / 0.65 / 0.83 | 0 / 0.81 / 0.96 | 95 / 1.87 / 2.00 |
| 2 | yes | 3 / 0.79 / 1.16 | 21 / 0.95 / 1.24 | 98 / 1.82 / 1.96 |
| 3 | no | 0 / 0.76 / 0.96 | 20 / 0.95 / 1.45 | 89 / 2.54 / 2.82 |
| 4 | no | 12 / 0.86 / 1.13 | 57 / 1.05 / 1.81 | 130 / 3.26 / 3.60 |

Stacked with AIRA-324 (auto and `--memory-reserve` jobs in their real proportion, 62% pinned,
pinned charged at 0.8 x pin), draws over 1 out of 4,000: mixed 0, 1 and 0 at k = 1, 2 and 3
(max 0.91, 1.06, 0.91); burst 1, 2 and 14 (p99 0.87, 0.89 and 0.93). AIRA-324 alone (k = 1) already
has a burst draw over 1. The two features spend the same slack, and together at k = 2 the burst p99
is 0.89 against 0.87 for AIRA-324 alone.

Reading it: on ordinary mixes k = 2 costs almost nothing. The risk is concentrated where it is
expected: **many concurrent instances of a command that is already being OOM-killed**, which
today die one by one inside their own caps and with headroom would run together into the shared
slack. That is the case the physical-fit term and steering exist for (section 5). Coverage limits:
draws are independent and not duration-weighted (AIRA-323's timing columns began on 2026-11-15:
720 rows, too few to replay a timeline); the physical-fit term cannot be replayed because the store
has no record of real use at each grant, so this table models fixed k without that term; page
cache is reclaimable, so in the "cache" rows the filled part is not memory a kill would be needed
to recover.

## 3. The greenfield minimum, and the alternatives

Only this problem: "an auto-sized job is killed at its estimate while the box has free memory."
The from-scratch minimum is **set the cap higher than the booking**: one multiplication where the
client writes `memory.max`, with the booking untouched. Everything else in this document is either
a consequence the minimum would get wrong (the cache ratchet, the restart booking) or a safety term
the owner asked for (physical fit).

| Option | Kills avoided | Booking (capacity) cost | New machinery | Verdict |
|---|---|---|---|---|
| A. Fixed k only, no physical-fit term | as B | none | one grant field | the from-scratch minimum; preserves the k-bound exactly; on a busy box a big job's headroom has nothing behind it |
| **B. k with the physical-fit term (proposed)** | proxy 45-81% of auto kills at k = 2 | none | A + one `min` with a value admission already reads | **recommended**, with step 1; the physical term is the first thing the plan gate may cut (5.1) |
| C. Fix the estimator only: AIRA-273 A+B, AIRA-331 cold-start cap | repeat kills only; never the first kill of a size not yet seen | raises bookings: AIRA-331 alone books ~2.9x the real peak for ~30% of cold runs | key chain, window rules | dominated for kills; partly superseded (below) |
| D. Only raise `memory.max`, never the charge | this is B | - | - | B already never changes the charge |
| E. No per-job cap for auto jobs (k = infinity), slice cap + steering only | all | none | none | rejected: gives up per-job containment; a slice-level OOM kills the largest process, and steering is off here |
| F. `memory.high` = C, `memory.max` = k x C | kills become throttling | none | one more write | rejected: swap is off in every scope, so anonymous memory above `high` cannot be reclaimed and the job stalls; sustained reclaim PSI is what trips the oomd backstop (AIRA-91 Part B) |

**What this removes or supersedes.**

- **AIRA-331** (size an unseen argv at max(1 GiB, coarse-key prediction)): **not shown to be
  superseded.** If D1 includes cold start, the proxy puts 41-82% of cold-start kills within k = 2
  with no booking increase, against 331's ~34% at a 2.9x booking for ~30% of cold runs; but a
  doubled 1 GiB cap cannot hold a 3 GiB first run that a coarse prediction would size correctly,
  and the two figures come from different methods. Keep 331 held and re-judge it on step 2's
  enforce data.
- **AIRA-273 option A** (escalate a kill to the machine-wide p90 prior): the prior is being deleted
  (AIRA-341), and headroom covers the same within-k kills without over-booking the cheap mode.
  **Option B** (remember the big mode longer than 20 rows) is **not** superseded: it is the only
  fix for the "beyond k" 19% (bimodal ratios above k).
- **Observe mode**, which AIRA-324 has: cut here. An observe mode could only log the limit it
  would have written; the effect that matters (a kill that did not happen) is visible only in
  enforce. One knob, `k`, with 1.0 meaning exactly today.

**Decisions this rests on.** D1 is open; D2 and D3 are this document's.

- **D1 (owner): which auto grants get headroom.** (i) Every daemon-sized auto grant
  (cap-source `auto:daemon-reserve`, all rungs including the 1 GiB default and, until AIRA-341
  lands, the p90 prior), or (ii) only the history rungs (`estimate:max`, `own-max`,
  `oom-escalated`). (i) is less code (the daemon does not branch on the basis) and covers 3x the
  kills (by the retained-evidence category, a proxy for the rung); it changes the cap,
  not the booking, of cold runs, so it does not reopen AIRA-304's booking default but does change
  what a cold run may use (2 GiB at k = 2). **Recommended: (i).**
- **D2: the OOM escalation stays as it is.** It is 1.5 x the peak recorded at the kill
  (`admit.go:1536-1541`), and that peak is now about L, so after a kill at L = 2C the booking
  becomes 3C and the limit 6C. The alternative, 1.5 x the killed run's booking (needs step 1's
  column), books less but takes more kills (and see 4.2: with cache-filled clean runs the 3C floor
  outlives the OOM row): for RANT-136's clippy (auto ~320M, cold need ~1.2G)
  the kept rule kills once (L 640M, then C 960M, L 1.92G), the alternative twice, today four
  times. The over-booking lasts while the OOM row is in the 20-row window, as today.
- **D3: user pins never get headroom.** A `--memory-reserve` or `--memory-max` is the operator's
  stated cap; AIRA-324 discounts its booking and leaves the cap alone.

## 4. Step 1: what the estimator learns when the cap is above the booking

### 4.1 The counterexample

A job that streams more file data than its limit (AIRA-318: RANT-73/87/111) records a
`memory.peak` equal to its limit on a clean run, because the kernel lets page cache fill a cgroup
up to `memory.max` before reclaiming it. The next booking is 1.15 x that peak. Six runs from a
1 GiB booking (output, part 5):

| Learning rule for a clean run that reached its limit | k = 1 | k = 2 |
|---|---|---|
| today: 1.15 x peak | 1.15, 1.32, ... 2.31 GiB (the 318 ratchet, 15%/run) | 2.30, 5.29, 12.2, 28.0, 64.4 GiB (**2.3x per run**) |
| AIRA-318's proposed rule: never above the cap it ran under | stable at 1.00 | 2, 4, 8, 16, 32 GiB (**doubles per run**) |
| **booking rule: never above the booking it ran under** | stable at 1.00 | **stable at 1.00** |

Exposure today: 1,996 clean auto rows (2.3%) sit at >= 95% of their cap, over 1,715 signatures;
136 consecutive at-cap pairs of one signature show the ratchet running now, cap ratio median 1.150
(output, part 3). Under k = 2 each of those signatures would hit E_ADMIT_TOO_LARGE within about
five runs. **The rule must not be enabled at k > 1 without the booking rule.**

### 4.2 The rule

- **Store the booking per row.** `confine_peak_history` gains one nullable column, `reserve`
  (the booking the run held). `budget` keeps its meaning: the cap the run ran under
  (`memory.max`, cap-source in `budget_basis`), which is what censors its peak. The report gains
  an optional `reserve` field, sent only for a daemon-granted run.
- **A clean run reached its limit** when its `budget_basis` is a kernel cap (`cap:*`) and its
  peak is at least 95% of its `budget` (the criterion AIRA-318 measured). No new kernel read. A row
  whose `budget` was not a kernel cap (`reserve:*`, an uncapped run, or an empty basis) is never
  treated as censored: an uncapped 4 GiB peak under a 1 GiB booking is a real 4 GiB need.
- **Such a run never raises the estimate above the booking it ran under.** Its effective peak,
  for every reader of clean peaks (the ordinary `estimate:max` and AIRA-314's `own-max` floor;
  one place: the stats `ResourcePeakHistory` returns), is min(peak, reserve / 1.15), so the
  estimate it produces is at most that `reserve`. A capped row with NULL `reserve` (written before
  step 1, a pinned run, the pool face) uses `budget` in its place, which is exactly AIRA-318's rule.
- **The other reader of raw peaks.** `ConfinePeakP90` (`confine_peak_history.go`, the machine-wide
  p90 prior) takes raw maxima across every signature, so cache-filled enlarged caps would lift
  cold-start bookings for every command. AIRA-341 deletes it; it must land before k > 1 (condition
  (d)). If it has not, this rule applies to that reader too.
- **OOM rows are unchanged** (D2).

What it costs, accepted: a job whose anonymous need is between C and L and whose page cache also
fills L is under-booked (booked C, using up to L) and survives. The converse also holds: a booking
raised by an OOM escalation (C to 3C, D2) and followed by cache-filled clean runs keeps 3C in every
replacement row, so it does **not** fade when the OOM row is evicted; it persists until a run
finishes below 95% of its cap. Accepted (it is a floor, not growth); a decay rule is a deferral,
and test H8 pins the persistence so a later change to it is deliberate. The under-booking is bounded by k,
and the slice-level reading in `checkedAvailable` (the larger of real use and the ledger) sees its
real use at every later admission.

Step 1 ships alone at k = 1, where it changes only at-cap clean rows (2.3% of clean auto rows) and
stops AIRA-318's ratchet; the AIRA-318 ticket's ratchet half is then done.

## 5. Step 2: the headroom rule

### 5.1 The rule

For a grant whose cap the client will take from the daemon (cap-source `auto:daemon-reserve`;
D1 decides which rungs):

```
booking C = the reserve resolveAdmitReserve returns today (unchanged; the ledger charges C)
free      = ceiling - headroom - effectiveCurrent - (sum of C granted earlier in this pass)
limit  L  = clamp(free, C, ceil(k x C))      (page-floored; L == C when k == 1)
```

`effectiveCurrent` is the slice's `memory.current` less reclaimable cache, the reading
`checkedAvailable` already takes in the same pass; `ceiling` is the (possibly throttled) admission
ceiling. Earlier grants of the same pass are subtracted by their booking because their use is not
yet in `memory.current`. `k` is an install knob (`AIRA_DAEMON_CAP_HEADROOM`, default 1.0 at first
release, raised to 1.5 then 2 by the rollout in section 8; the value must be in [1, 4]).

Not covered: pinned jobs (D3), `--delegate-ram` parents and aitest workers (`worker_admit.go`
sizes workers by its own rules; deferred), exclusive jobs are covered (they are alone on the
slice, where headroom is safest), ci-shim mode gets none (no physical reading, as AIRA-324).

**Why the physical term is kept, and what it is not.** It is not needed to solve the problem
(option A is the minimum) and is the first thing the plan gate may cut; it is kept because the
owner asked for it and it costs one `min` with a number the same pass already reads, and it is the only thing that stops a 20 GiB job being given a 40 GiB limit on
a box with 25 GiB physically free. It is a **grant-time** snapshot: other jobs grow after the grant
and nothing lowers L later. It is compatible with AIRA-103, whose rule keeps throttled values out
of kernel-enforced numbers, for that rule's three reasons: the throttle can only take headroom
away, never push L below the booking (so no admitted job is made to fail); L is written once, when
the scope is created and before the job starts, never near `memory.current`; and nothing re-writes
it, so there is no restore path. Lowering L on a running job is out of scope for exactly those
reasons.

### 5.2 Retired invariant and the bound that replaces it

Today, for auto jobs, `memory.max` = booking, so the fit check's `sum(booking) <= ceiling` also
bounds their caps. After step 2 it bounds only the booking. What replaces it is a **grant-time
bound, not a standing invariant**: at the instant an auto job is granted, its L <= k x C, so
`sum(memory.max of auto jobs) <= k x sum(their booking)`. With AIRA-324:
`sum(memory.max) <= max(k, 1/0.8) x sum(charge)` at grant. It is valid only while k is fixed and
no exception applies: `--resize` (lowers the booking, not the cap), a ceiling that falls after the
grant (the throttle), a restart re-declare charged above the ceiling, a change of k (new grants
only). It is used to reason about how bad an overrun can be, never as a gate.

### 5.3 Failure modes

| Failure | First symptom | What limits it |
|---|---|---|
| Jobs grow into headroom together and the slice passes its throttled ceiling | MemAvailable falls into the throttle's free-memory margin (`freeMin` 8 GiB); the ceiling throttle cuts new admissions | the throttle (new grants only), the physical term (grant time only) |
| ... and passes the slice's `MemoryHigh` (60G here) | `memory.high` reclaim and throttling of every job in the slice, cache first; PSI rises | page cache absorbs much of it; the rollback triggers in 8 |
| ... and reaches the slice's `MemoryMax` (64G here) | **slice-level OOM**; with `memory.oom.group` set, the whole scope of the chosen task dies | the kernel picks the task with the highest badness (largest RSS) unless OOM steering raised the job in its headroom to 1000; **steering is off on this box** (no `AIRA_DAEMON_OOM_STEER_MODE` in the unit; default off, `paths.go:243`), so an innocent large job is the likely victim |
| Outside-slice memory is large (the reason the ceiling is throttled) | the box, not the slice, runs short: swap use by processes outside the slice | the throttle, the watchdog (which exempts confined work, so it is no backstop here) |
| Correlated burst of one OOM-prone command | section 2.4: 2.4% of 20-job bursts over the booking at k = 2, p99 1.87 | steering (once on) prefers the jobs in their headroom; the physical term cuts L for later grants of the burst |
| Cache-filling job | ratchet (section 4.1) | step 1 |

Steering's budget is the booking (`admitScopeBudgets`, `oomsteer.go:453`, through
`ledgerCharge`), so a job in its headroom reads as over budget, with two limits the build must
test before enforce: a scope counts as an offender only past its booking **plus a 64 MiB floor**
(`oomSteerOverrunFloorDefault`), so a small job using its headroom (32 MiB booked, 64 MiB cap) is
never raised; and steering activates at 90% of the slice's **kernel** `memory.max` (64G here), not
the throttled admission ceiling, so it does nothing while outside-slice pressure is the problem.
No steering change is proposed; switching it on is the condition. Its doc comment already anticipates "any scope whose cap exceeds its
reserve" (`oomsteer.go:23-26`) and needs only a pointer.

## 6. Every site that reads the booking or the cap

| Site | Decision |
|---|---|
| `resolveAdmitReserve`, `ledgerCharge`, fit check, head eligibility (`admitCouldFitEmptyLocked`), bypass budget, queue position, queued-ahead bytes | the booking C, unchanged. L never enters admission arithmetic. Head eligibility needs no change: L >= C is chosen at grant and cannot block. |
| Grant frame | `reserve` stays C. New optional field `limit` = L, **omitted when L == C**. |
| Client cap (`confine_linux.go:1125-1127`) | `memory.max` = `limit` when present and valid (`reserve <= limit`, positive), else `reserve`; an invalid `limit` makes the grant invalid (the existing `validRunnerAdmitGrant` path, refuse never repair). cap-source stays `auto:daemon-reserve`. |
| Re-declare after reconnect or daemon restart | unchanged: the frozen ARDR record carries `grant.Reserve` (`lease_keeper_linux.go:102`), which is C, so a restart books the booking, not the enlarged limit. The scope's `memory.max` (L) survives in the kernel. **Nothing in the daemon reads a scope's `memory.max` to rebuild a booking** (S12 deleted scan adoption; the daemon's `memory.max` reads are of the slice, `admit.go:3541`, `sliceceiling.go:402`, and the watchdog's `watchdog.go:836`); the build must not add one, and test H5 pins it. |
| `--resize` (`confine_resize.go`) | lowers the booking only, as today; "must name a smaller reserve" is judged against C; the floor stays live use, which may now be above C. L is untouched. |
| OOM steering (`admitScopeBudgets`) | the booking C (above). |
| `--memory-max` / `--memory-high` | pinned paths; `--memory-high` requires `--memory-max` (`memory_size.go:88-91`), so neither meets headroom. |
| AIRA-340 priority | none: priority orders the queue; L is computed at grant whatever the tier. |
| `confine --list`, `aira top`, `ci_dump` | already show the scope's `memory.max` (`Cap`, read from the cgroup, `confine_manage_linux.go:318`) apart from the ledger's `ReserveBytes` (AIRA-192: summing caps over-states the claim). Keep them apart; sums use the booking; no renderer may label L as reserved. |
| Trailer | `reserve=` stays C and `scope-memory.max=enforced=` is L (two keys exist already). New key `cap-headroom=` with `none` (L == C, whatever the reason), `k` (L = k x C) or `fit-bound` (the physical term set C < L < k x C). The OOM advisory names both: killed at the cap L, booked C. |
| `--summary-file` | today's record has `reserve_bytes` and no cap field (`confine_summary.go:65-85`). Add `cap_bytes` (the scope's `memory.max`, or `unevaluated`) and `cap_headroom`; `reserve_bytes` keeps its meaning, the booking. |
| Peak report and store | `budget` = L (the cap); new optional `reserve` = C (step 1). |
| Hints (AIRA-334's over-reserve hint, AIRA-345's floor) | over-reservation is judged against the booking; AIRA-345's "recent OOM peak" is now about L, which is the right floor. |
| Estimator | step 1 for clean at-limit rows; OOM escalation unchanged (D2). |

## 7. Interaction with AIRA-324, and order

Shared: the idea that what the ledger charges and what the kernel enforces differ; the physical
free figure (324 requires `pin <= ceiling - headroom - effectiveCurrent`; this uses the same
quantity as an upper clamp), which should be one helper; the restated grant-time bound (5.2) and
the steering comment. Not shared: 324 adds a `charge` on the waiter and a `discountable` admit
field and leaves the grant echo at the pin; this leaves the waiter's charge at the reserve and adds
a `limit` to the grant. Neither changes the ARDR record.

The one place they could conflict is the bound and OOM steering: with both, a pinned job's charge
is 0.8 x pin and an auto job's is C, both below their caps, and steering ranks both by charge, so
the conflict is only in the restated text, which whichever lands second updates. **Order: AIRA-324
first** (owner's sequencing, and its enforce weeks calibrate the slack both spend), then step 1,
then step 2. Step 1 does not depend on 324 and may land earlier.

## 8. Rollout, gates and rollback

1. **Step 1** (learning rule and the `reserve` column), k stays 1.0. Gate: none beyond tests; it
   only stops the ratchet. Metric: the at-cap pair ratio of part 3 falls from 1.15 to 1.00.
2. **Switch OOM steering on**: observe for a week (it logs who it would raise), then enforce. This
   is also a precondition AIRA-324's enforce should state.
3. **Step 2 at k = 1.5 for a week, then 2.** Gate to start: the replay of section 2.4 re-run on
   AIRA-323's timeline (start = `started_at_ms`, end = start + `wall_ms`, two weeks of rows),
   duration-weighted, showing the concurrent total use stays under 0.9 x the slice's `MemoryHigh`
   at the busiest hour with the auto kills' censored use set to k x C. This replay is **advisory**:
   completion rows miss failed runs (AIRA-264), jobs still running, and the rows retention evicts
   from frequent commands, so it can pass through missing load. The **binding** gate is the live
   slice-level evidence during the k = 1.5 week (the rollback triggers below, which count every
   job in the slice whatever was recorded). A complete lifecycle log (grant and end events, failures
   included) would make the replay binding; it is a deferral.
4. **Metrics**, weekly against the prior fortnight: auto OOM kills per 1,000 auto jobs (expected to
   fall by roughly half at k = 2); share of auto jobs whose peak exceeded their booking (headroom
   used); hourly **increments** of the slice's `memory.events.local` `high`, `max` and `oom` (the
   counters are cumulative, so only deltas over fixed windows are compared, and a counter that
   goes backwards, as after a slice re-creation, starts a new baseline); slice `memory.pressure`
   `some avg10` (a moving average, compared as sampled); ceiling throttle cuts; steering raises.
   Auto kills are counted only when the confine trailer attributes them to the scope's **own**
   limit (the runner already distinguishes own-limit, descendant and ancestor kills,
   `confine_linux.go:1509-1518`); the stored `oom` bit does not, so the store alone cannot give
   this metric (section 2.3).
5. **Rollback** (any one sets k back to 1.0, which writes exactly today's caps for new grants, and
   files a finding): any increment of aira.slice's own `memory.events.local` `oom` (the slice's
   **own** limit was reached, the kernel's attribution, which a scope's local kill counter cannot
   give because a kill under an ancestor limit also counts in the victim scope); the hourly
   increment of the slice's `memory.events.local` `high`, or PSI `some avg10`, above its
   pre-step-2 hourly 99th percentile for two consecutive hours; the median booking of repeat signatures growing more than 15% in a week (a
   ratchet step 1 missed).

## 9. Protocol and storage

No protocol bump. Each change follows a measured no-bump pattern:

- Grant `limit`: an optional field in the daemon's answer. An old client ignores it (the grant is
  decoded with plain `json.Unmarshal`, `admission_linux.go:685`, which drops unknown fields) and
  writes `memory.max` = `reserve`, exactly today. A new client facing an old daemon sees no
  `limit` and does the same. The daemon cannot tell which client it is talking to and does not
  need to: the booking is C either way; the trailer of an old client shows L == C.
- Report `reserve`: an optional `confine-report` field, with AIRA-323's once-only re-send against
  an old daemon (section 5.1.6 of the companion spec). That re-send drops "the four new fields";
  the build generalises it to every optional field added after master's six-field set, so the
  peak and OOM bit are still kept.
- Store: one nullable column, in the DDL and the `ensureConfinePeakHistorySubject` list, no CHECK,
  no data migration (old rows NULL, which step 1 reads as "use budget").
- ARDR re-declare record: unchanged.

## 10. Tests required (each must fail against the wrong implementation)

- **H1. Grant.** An auto grant with physical free above k x C carries `limit` = ceil(k x C); with
  free between C and k x C, `limit` = free; with free below C, no `limit`; at k = 1, no `limit`.
  Mutations, each red: ignore the physical term; let L fall below C; send `limit` == `reserve`.
- **H2. Same pass.** Two auto grants in one evaluator pass: the second's free figure is reduced by
  the first's booking. Mutation: not reduced (the second gets the same L).
- **H3. Who gets it.** Pinned (`--memory-reserve`, `--memory-max`), delegate parent, aitest worker,
  ci-shim: no `limit`. D1's choice pinned by one case per rung. Mutation: headroom for a pin.
- **H4. Admission arithmetic never reads L.** A guard test (the same allow-list form as AIRA-324's
  `.reserve` guard) fails if the waiter's limit is read outside the grant echo and the display
  paths; ledger, fit, head eligibility, bypass and steering budgets equal C with L = 2C.
- **H5. Restart.** After a daemon restart, the re-declared lease is booked C (not L) and the scope's
  `memory.max` is still L. Mutation: the keeper re-declares the limit.
- **H6. Client.** `memory.max` = `limit` when present; = `reserve` when absent; a `limit` below
  `reserve` or non-positive makes the grant invalid. Mutations: write `reserve` always; accept
  `limit < reserve`.
- **H7. Kill switch.** At k = 1.0 the grant frames and every scope's `memory.max` are byte-identical
  to master's for a table of auto, pinned and fallback cases.
- **H8. Step 1 learning.** The six-run cache-filling job of section 4.1 (as a unit test over
  `ResourcePeakHistory` stats) stays at its booking at k = 1 and k = 2; a row with NULL `reserve`
  uses `budget`; a clean run below 95% of its cap still raises the estimate to 1.15 x its peak; an
  uncapped row (`reserve:*` or empty basis) at 4x its booking is not censored; an escalated 3C
  booking followed by 20 cache-filled clean rows stays at 3C after the OOM row is evicted (4.2).
  Mutations, each red: AIRA-318's cap-based rule (doubles at k = 2); no cap (2.3x per run); the cap
  applied to OOM rows; the cap applied to rows below 95%; the cap applied to an uncapped row.
- **H9. Report and store.** `reserve` round-trips through the real `ReportPeakSample` and handler;
  absent is NULL; against an old daemon the re-send keeps the peak and the OOM bit and drops
  `reserve` (AIRA-323's T9 cases extended). Mutation: the re-send keeps the new field.
- **H10. Honesty.** The trailer, `--summary-file` (`reserve_bytes`, new `cap_bytes`) and `--list` show `reserve` = C and the cap = L
  with `cap-headroom=k`/`fit-bound`/`none`; the OOM advisory names both. Mutations: `reserve=` shows
  L; `cap-headroom=k` when the physical term bound it.
- **H11. Real cgroup** (skips without delegation): an auto job allocating 1.5 x its booking of
  anonymous memory completes at k = 2 and is OOM-killed at k = 1; the first run's recorded peak is
  about 1.5 x C and its `reserve` is C.
- **H13. Steering sees headroom users** (before enforce, section 5.3): with steering on, a scope
  using its headroom by more than the 64 MiB floor is raised when the slice passes 90% of its
  kernel maximum; a 32 MiB-booked scope using 64 MiB is not (documents the floor's gap).
- **H12. Escalation unchanged** (D2): a kill at L = 2C escalates to 3C. Mutation: escalate from the
  booking.

## 11. Deferrals

- Headroom for `--delegate-ram` parents and aitest workers (`worker_admit.go` has its own sizing).
- Lowering L on a running job when the box gets busy (AIRA-103's reasons, section 5.1).
- An anonymous-memory peak (a sampler on the monitor tick) that would let a cache-filled run teach
  its real need; step 1 accepts the bounded under-booking instead.
- Per-command k, or k learned from history.
- A decay for a booking held up by cache-filled rows after an OOM escalation (4.2).
- A complete job lifecycle log (grants, ends, failures) that would make the step-2 replay a
  binding gate (section 8).
- AIRA-273 option B (remember the big mode longer), still needed for modes beyond k.
- Recording failed runs (AIRA-264 decided against it).
- The timeline replay itself: it needs two weeks of AIRA-323 rows and is the gate of step 2.

## 12. Change log

- 2026-11-15, rev 1 after Sol round 2 (BLOCK, 3 P2 + 3 P3, all taken): rollback keyed on the
  slice's own `memory.events.local` `oom`; cumulative counters compared as hourly deltas;
  unstored kill attribution stated as an evidence limit and the step-2 metric restricted to
  own-limit kills; streak loop restarts at a non-auto OOM (counts re-derived); 150 -> 136; the
  Monte-Carlo "conservative" and "upper bound" claims removed.
- 2026-11-15, rev 1 after Sol (high) review, verdict BLOCK, 11 findings, all taken: censoring only
  for kernel-capped rows; AIRA-341 before k > 1 (the p90 prior reads raw peaks); "lower/upper
  bound" renamed a proxy range; the timeline replay demoted to advisory with the live slice
  evidence as the binding gate; steering's 64 MiB floor and kernel-max trigger stated; Monte-Carlo
  counts printed and the model's sampling stated; the script's streak and at-cap-pair loops fixed to
  break on a row of any source (streak counts changed from 575/60/3/2 to 1,262/131/10/3/1, then 1,267 after round 2; pairs
  150 to 136); the reconstruction renamed a retained-evidence category; the persistent 3C floor
  acknowledged; AIRA-331 no longer claimed superseded; the summary-file cap field specified.
- 2026-11-15, rev 1: written from the owner's 2026-11-15 direction, with the challenge findings
  folded in (the cache-ratchet counterexample and the booking rule; steering off on this box;
  two thirds of auto kills are cold start; the quoted "clean max <= f x budget" figure replaced
  by a within/beyond/undetermined split; observe mode cut; AIRA-331 and AIRA-273 option A
  superseded if D1 is (i)).
