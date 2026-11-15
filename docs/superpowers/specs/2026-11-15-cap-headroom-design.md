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

**Recommendation, in one paragraph.** Build it, with one change to the proposed rule and only with
its conditions met. Book C (the estimate, unchanged); cap at **L = min(k x C, C + 1 GiB)**, k = 2.
The owner's proposed physical-fit term (cap L at the memory physically free at grant) is decision
D4 and is **not recommended**: it does not protect the case it was credited with (a burst of one
command granted before any member has allocated, section 5.3), it rarely binds on this box, and an
additive 1 GiB bound does its other job (no 20 GiB job given a 40 GiB limit) with no grant-time
read (section 3, option G). On this box's history a proxy puts 42-78% of auto-sized OOM kills
within reach of the recommended rule (45-81% for a plain 2C; a proxy range, not bounds: the killed
run's own need is censored, section 2). The rule has one silent defect as proposed: **with the
current estimator, any k > 1 turns AIRA-318's 15%-per-run page-cache ratchet into 2.3x per run**
for a job whose page cache fills any limit (section 4.1, by arithmetic: a hypothetical
counterexample; such jobs are reported in RANT-73/87/111, and the store holds 136 consecutive
at-cap pairs over 106 signatures that fit the pattern, but cap proximity cannot tell page cache
from anonymous growth, so these are candidates, not a measured count), and AIRA-318's own proposed
rule still doubles per run. So the feature carries a learning rule (store the booking per row; a
clean run that reached its limit **while its limit was above its booking** never raises the
booking above the booking it ran under, **but only while the current k still gives that booking at
least the limit the run had**; once k is lowered, as in a rollback, the row is read as today, so
no job is ever capped below a peak it has already completed). The learning rule is inert at k = 1
for every row, including rows written at k > 1, and is never enabled apart from the headroom:
alone at k = 1 it would trade the ratchet for OOM kills of jobs that genuinely grew (section 4.4,
measured). One accepted cost remains: a job whose page cache is finite and lands below 95% of its
new limit steps its booking up once, to at most 1.0925 x L, where L = min(k x C, C + 1 GiB) is
the limit it ran under (section 4.3). Conditions: (a) a small step 0 first: install flags for k and
for OOM steering, which today can only be set by hand-editing the unit; (b) OOM steering switched
on (observe, then enforce) before k > 1, because it is **off on this box** and without it a
slice-level OOM kills the largest job, not the one using headroom (this applies to AIRA-324 too,
whose spec cites steering as a mitigation); (c) the owner decides whether cold-start jobs are
included (68% of auto kills have no retained earlier row, section 2); (d) AIRA-341 (delete the p90
prior, already un-held) lands before k > 1, because that prior reads raw maxima across every
signature and would carry cache-filled peaks into cold-start bookings (section 4.2).

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

So at k = 2 the proxy range is 45-81% of auto kills; under the recommended L = min(2C, C + 1 GiB)
it is 42.3% within, 22.1% beyond, 35.6% undetermined, a range of 42-78% (output, part 6: 97.7% of
auto kills had a cap under 2 GiB, where the 1 GiB bound costs little). The category is a **retained-evidence
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
  have touched. Their share is **unmeasured**; the rollout metric counts own-limit kills only
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
slack. Only steering limits that case once it happens (section 5.3); no grant-time term does.
Coverage limits: draws are independent and not duration-weighted (AIRA-323's timing columns began
on 2026-11-15: 720 rows, too few to replay a timeline); the table models a plain k x C, and under
the recommended L = min(k x C, C + 1 GiB) every job's use is at most its k x C figure, so the k = 2
rows are at or above what that rule gives; the physical-fit term (D4) cannot be replayed because
the store has no record of real use at each grant; page cache is reclaimable, so in the "cache"
rows the filled part is not memory a kill would be needed to recover.

## 3. The greenfield minimum, and the alternatives

Only this problem: "an auto-sized job is killed at its estimate while the box has free memory."
The from-scratch minimum is **set the cap higher than the booking**: one multiplication where the
client writes `memory.max`, with the booking untouched. Everything else in this document is either
a consequence the minimum would get wrong (the cache ratchet, the restart booking), a bound on the
largest jobs (the 1 GiB term), or the means to switch it on and off (step 0).

| Option | Kills avoided | Booking (capacity) cost | New machinery | Verdict |
|---|---|---|---|---|
| A. Fixed k only | proxy 45-81% at k = 2 | none | one grant field | the from-scratch minimum; a 20 GiB job gets a 40 GiB limit |
| **G. L = min(k x C, C + 1 GiB)** | proxy 42-78% at k = 2 (93.8% of A's "within" kills; C + 2 GiB keeps 99.2%) | none | A + one constant | **recommended**, with the learning rule; total headroom over all auto jobs is 0.84 of A's (most jobs are under 1 GiB), so it bounds the big jobs, not the sum |
| B. k with the physical-fit term (owner's proposal) | at most A's | none | A + a grant-time read of real use, its fit-bound state and tests | **D4, not recommended**: every grant of a burst made before the burst allocates sees the same free figure, so it does not limit the burst (5.3); it binds only when the slice's real use is high, when the ceiling throttle already cuts admission |
| C. Fix the estimator only: AIRA-273 A+B, AIRA-331 cold-start cap | repeat kills only; never the first kill of a size not yet seen | raises bookings: AIRA-331 alone books ~2.9x the real peak for ~30% of cold runs | key chain, window rules | dominated for kills; partly superseded (below) |
| D. Only raise `memory.max`, never the charge | this is G | - | - | G already never changes the charge |
| E. No per-job cap for auto jobs (k = infinity), slice cap + steering only | all | none | none | rejected: gives up per-job containment; a slice-level OOM kills the largest process, and steering is off here |
| F. `memory.high` = C, `memory.max` = k x C | kills become throttling | none | one more write | rejected: swap is off in every scope, so anonymous memory above `high` cannot be reclaimed and the job stalls; sustained reclaim PSI is what trips the oomd backstop (AIRA-91 Part B) |

**What this removes or supersedes.**

- **AIRA-331** (size an unseen argv at max(1 GiB, coarse-key prediction)): **not shown to be
  superseded.** If D1 includes cold start, the proxy puts 41-82% of cold-start kills within k = 2
  (most were killed at the 1 GiB default, where the recommended rule equals 2C) with no booking
  increase, against 331's ~34% at a 2.9x booking for ~30% of cold runs; but a
  doubled 1 GiB cap cannot hold a 3 GiB first run that a coarse prediction would size correctly,
  and the two figures come from different methods. Keep 331 held and re-judge it on the k > 1
  enforce data.
- **AIRA-273 option A** (escalate a kill to the machine-wide p90 prior): the prior is being deleted
  (AIRA-341), and headroom covers the same within-k kills without over-booking the cheap mode.
  **Option B** (remember the big mode longer than 20 rows) is **not** superseded: it is the only
  fix for the "beyond k" 19% (22% under the 1 GiB bound; bimodal ratios above the limit).
- **Observe mode**, which AIRA-324 has: cut here. An observe mode could only log the limit it
  would have written; the effect that matters (a kill that did not happen) is visible only in
  enforce. One knob, `k`, with 1.0 meaning exactly today; the 1 GiB term is a constant, not a knob.
- **The physical-fit term**, if D4 declines it: the grant-time read of real use, the handling of
  grants earlier in the same pass, a trailer state, test H2 (cut) and the AIRA-103 argument (5.1).
- **A headroom label in the trailer** (`k` vs `fit-bound`): cut. The client sees only C and L and
  cannot tell why L is what it is, so it reports the two numbers and no reason (section 6).

**Decisions this rests on.** D1 and D4 are open (owner); D2 and D3 are this document's.

- **D1 (owner): which auto grants get headroom.** (i) Every daemon-sized auto grant
  (cap-source `auto:daemon-reserve`, all rungs including the 1 GiB default and, until AIRA-341
  lands, the p90 prior), or (ii) only the history rungs (`estimate:max`, `own-max`,
  `oom-escalated`). (i) is less code (the daemon does not branch on the basis) and covers 3x the
  kills (by the retained-evidence category, a proxy for the rung); it changes the cap,
  not the booking, of cold runs, so it does not reopen AIRA-304's booking default but does change
  what a cold run may use (2 GiB at k = 2). **Recommended: (i).**
- **D2: the OOM escalation stays as it is.** It is 1.5 x the peak recorded at the kill
  (`admit.go:1536-1541`), and that peak is now about L, so after a kill at L = 2C the booking
  becomes 3C and the limit min(6C, 3C + 1 GiB). The alternative, 1.5 x the killed run's booking (needs the learning rule's
  column), books less but takes more kills (and see 4.3: with cache-filled clean runs the 3C floor
  outlives the OOM row): for RANT-136's clippy (auto ~320M, cold need ~1.2G)
  the kept rule kills once (L 640M, then C 960M, L 1.92G), the alternative twice, today four
  times. The over-booking lasts while the OOM row is in the 20-row window, as today.
- **D3: user pins never get headroom.** A `--memory-reserve` or `--memory-max` is the operator's
  stated cap; AIRA-324 discounts its booking and leaves the cap alone.
- **D4 (owner): the physical-fit term.** (i) Not built (recommended): L = min(k x C, C + 1 GiB).
  (ii) Also clamp L at the memory physically free at grant, as the owner proposed (specified in
  5.1). The owner asked for (ii) so that "on a busy box L shrinks to C". What it actually does: it
  reads the slice's real use, which does not yet include jobs granted moments earlier, so all
  members of a fast burst see the same free figure and get the full limit; and on this box the
  ledger is often full while real use is far lower (`aira confine --list`, 2026-11-15T14:44Z:
  46.7 GB booked against a 46.3 GB throttled ceiling, 12 jobs queued, while the slice's
  `memory.current` was 28.3 GB of which 13.2 GB reclaimable, and `MemAvailable` 43.7 GB; the free
  figure would have been about 31 GB less the headroom), so it would hand almost every job its full
  limit. It does protect a box whose real use is already high, but that is when the ceiling
  throttle cuts new admissions anyway. The additive bound G caps each job's extra at 1 GiB on any
  box.

## 4. The learning rule: what the estimator learns when the cap is above the booking

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

The rows of this table are a **hypothetical** fills-any-limit job: arithmetic, not a measurement.
What the store can show is only cap proximity, which cannot tell page cache that fills a cap from
anonymous memory that grew about 15% to the next cap; both give the same rows. Exposure today
(output, part 3): 1,996 clean auto rows (2.3%) sit at >= 95% of their cap, over 1,715 signatures
(205 of them have two or more such rows, consecutive or not). **Candidate patterns, not
established ratchets:** 136 consecutive at-cap pairs of one signature, over 106 signatures, cap
ratio median 1.150: each is what a running ratchet looks like, and equally what a job whose
anonymous need grew 15% once looks like. If any of them is a fills-any-limit job, k = 2 without
the booking rule takes it up 2.3x per run until E_ADMIT_TOO_LARGE. What the next retained row of
the same signature did after each at-cap row:

| Next retained row after a clean auto row at >= 95% of its cap | Rows | Share |
|---|---|---|
| none retained (unestablished) | 1,114 | 55.8% |
| not an auto row / an auto OOM / cap did not grow | 176 / 20 / 56 | 12.6% |
| cap grew, peak at or below the **old** cap (does not follow the cap: not a fills-any-limit job) | 457 | 22.9% |
| peak above the old cap and at its own new cap (candidate ratchet step: cache or anonymous growth) | 108 | 5.4% |
| peak above the old cap, below 95% of the new cap (real growth, section 4.4) | 65 | 3.3% |

So the store neither confirms nor rules out the ratchet: at most the 108 candidate steps are
ratchets, and the reported cases (RANT-73/87/111, AIRA-318) are cache-fillers. The decision does
not rest on a count: by the arithmetic above, **one** fills-any-limit signature at k = 2 runs away
in a few runs, and the rule that stops it costs nothing on the rows that are not one (4.2, 4.4).
**The headroom must not be enabled at k > 1 without the booking rule.**

### 4.2 The rule

- **Store the booking per row.** `confine_peak_history` gains one nullable column, `reserve`
  (the booking the run held). `budget` keeps its meaning: the cap the run ran under
  (`memory.max`, cap-source in `budget_basis`), which is what censors its peak. The report gains
  an optional `reserve` field, sent only for a daemon-granted run.
- **A clean run reached its limit with headroom** when all four hold: its `budget_basis` is a
  kernel cap (`cap:*`); its stored `reserve` is below its `budget` (it ran with a limit above its
  booking); its peak is at least 95% of its `budget` (the criterion AIRA-318 measured; 4.4 says
  why 95% is kept); and **the daemon's current rule gives that booking at least the limit the run
  had**: `limit_k(reserve) >= budget`, where `limit_k(c) = max(c, min(ceil(k x c), c + 1 GiB))` is
  section 5.1's formula at the daemon's current k (without D4's free-memory term, which describes
  the box at one grant, not the rule). No new kernel read. Every other row is read exactly as
  today: a row with NULL `reserve` (written before this lands, a pinned run, the pool face); a
  row whose `reserve` is at or above its `budget` (k = 1, a grant without headroom, or k = 1 with
  an unaligned booking, where `budget` is the kernel's page floor of C and so a few bytes below
  `reserve`); a row whose `budget` was not a kernel cap (`reserve:*`, an uncapped run, or an empty
  basis; an uncapped 4 GiB peak under a 1 GiB booking is a real 4 GiB need); and a row whose limit
  the current k no longer gives its booking (every headroom row after a rollback to k = 1; after
  k is lowered from 2 to 1.5, the rows where the k term bound, not those where the 1 GiB term
  did). So **the rule
  is inert at k = 1 for every row**, including rows written at k > 1, and AIRA-318's k = 1 ratchet
  stays AIRA-318's (section 4.4).
- **Such a run never raises the estimate above the booking it ran under.** Its effective peak,
  for every reader of clean peaks (the ordinary `estimate:max` and AIRA-314's `own-max` floor;
  one place: the stats `ResourcePeakHistory` returns, which therefore takes the daemon's k), is
  min(peak, reserve / 1.15), so the estimate it produces is at most that `reserve`.
- **Why the k condition (the invariant it keeps).** Today a retained clean peak P guarantees the
  next limit is at least 1.15 x P, so no job is capped below a peak it has completed. Without the
  condition the rule breaks that after a rollback: a job of anonymous need 1.95 GiB, booked
  C = 1 GiB under L = 2 GiB at k = 2, finishes at 1.95 GiB (>= 95% of L) and keeps the 1 GiB
  booking; rolled back to k = 1 its next limit is 1 GiB, and it is killed twice (1 GiB, 1.5 GiB)
  before the escalation reaches 2.25 GiB, where master books 2.24 GiB from the same peak (output,
  part 5, "rev2" against "rev3"). With the condition the rule keeps a weaker form of the guarantee:
  **while a clean row is retained, the next limit is at least its peak.** A censored row
  contributes min(1.15 x P, reserve); in the first case the next booking is at least 1.15 x P, in
  the second the next booking is at least `reserve`, so (`limit_k` being non-decreasing) the next
  limit is at least `limit_k(reserve) >= budget >= P`. An uncensored row contributes 1.15 x P. The
  exceptions are today's: a booking clamped to fit the slice (AIRA-151) and, under D4 only, a free
  figure below the row's peak. After a rollback the rows written at k > 1 read raw, so the next
  grants are what master grants from the same rows: an anonymous grower books 1.15 x P, and a
  cache-filled row steps its booking once to 1.15 x its old limit (at most 1.15 x (C + 1 GiB)),
  after which AIRA-318's k = 1 ratchet is back, exactly as on master.
- **The basis says so.** When an effective (reduced) peak is the value that set the reserve, the
  basis prints that value as its peak and appends `,at-limit` (for example
  `estimate:max=<effective peak>,n=5,f=115,at-limit` or `estimate:own-max=...,f=115,at-limit`),
  meaning "the largest peak came from a run that reached a limit above its booking; it counts as at
  most the booking it ran under". A basis without `,at-limit` keeps today's meaning: the printed
  peak is a measured `memory.peak`. The token is added to the basis catalogue next to
  `,oom-on-record` (`admit.go:1446-1571`, the trailing-token list at `admit.go:3073`).
- **The guidance says so.** The skill guide's promise (`internal/core/skill.go:354`) that an
  unpinned reserve "is never below 1.15x that peak while that peak is among its 20 most recent
  records" becomes false for such a run. It is revised to: "...never below 1.15x that peak, except
  a peak from a run that reached a limit set above its booking, which counts as at most that
  booking (basis `,at-limit`)". `--budget`'s description of the "largest peak actually observed"
  keeps the raw peak (it is a measurement) and gains the same one-line note. Both are prose
  generated from `skill.go`; test H14 asserts them.
- **The other reader of raw peaks.** `ConfinePeakP90` (`confine_peak_history.go`, the machine-wide
  p90 prior) takes raw maxima across every signature, so cache-filled enlarged caps would lift
  cold-start bookings for every command. AIRA-341 deletes it; it must land before k > 1 (condition
  (d)). If it has not, this rule applies to that reader too.
- **OOM rows are unchanged** (D2).

### 4.3 What it costs, accepted

- **Under-booking.** A job whose anonymous need is between C and 0.95 x L and whose page cache also
  fills L is under-booked (booked C, using up to L) and survives. Bounded by L - C (at most 1 GiB
  under the recommended rule), and the slice-level reading in `checkedAvailable` (the larger of
  real use and the ledger) sees its real use at every later admission.
- **A one-time step up for a finite cache.** A job whose page cache is finite (it reads F bytes of
  files once per run, anonymous need tiny) with C < F < 0.95 x L records a real-looking peak F, so
  its next booking is 1.15 x F with no real need behind it. It is one step, not a ratchet: the next
  limit is above 1.15 x F, the cache still stops at F, and the booking stays at 1.15 x F (output,
  part 5: F = 1.5 GiB from C = 1 GiB at k = 2 steps once to 1.725 GiB and stays). The bound is
  F < 0.95 x L, so the new booking is below 1.15 x 0.95 x L = **1.0925 x min(k x C, C + 1 GiB)**:
  2.19 C at k = 2 and 1.64 C at k = 1.5 while the k term binds (C up to 1 GiB at k = 2), and
  1.0925 x (C + 1 GiB) above that, which grows with C. It is **not** bounded by C plus a constant:
  C = 10 GiB at k = 2 (L = 11 GiB) with F = 10.44 GiB steps once to 12.006 GiB, 2.0 GiB above C
  (output, part 5). It is also the fixed point today's estimator reaches for the same job at k = 1,
  in several 15% steps instead of one. The store cannot tell this job from a genuine anonymous
  peak; the deferred anonymous-peak sampler (section 11) would. Test H8 pins it, the large case
  included.
- **A held-up floor after an OOM.** A booking raised by an OOM escalation (C to 3C, D2) and
  followed by cache-filled clean runs keeps 3C in every replacement row, so it does **not** fade
  when the OOM row is evicted; it persists until a run finishes below 95% of its limit. Accepted (it
  is a floor, not growth); a decay rule is a deferral, and test H8 pins the persistence so a later
  change to it is deliberate.

### 4.4 Why the learning rule is never enabled alone, and why 95% is kept

At k = 1 a run's limit is its booking, so "never raise the booking past the one it ran under"
would also stop jobs that genuinely grew a little. Measured (output, part 3, the next-row table):
65 retained cases where a clean auto run at >= 95% of its cap was followed by a clean run peaking
**above** the old cap but below 95% of the new one (next peak / old cap: median 1.026, p90 1.071,
max 1.307). Under a k = 1 learning rule each would have kept the old booking and, if the growth was
anonymous memory, been OOM-killed and then escalated 1.5x. That is the same order as the 108
candidate ratchet steps the rule would stop (some may themselves be anonymous growth, 4.1). At
k = 1.5 all 65 fit under the limit (their largest ratio is 1.307). Hence the rule applies only to
rows that ran with headroom and only while the current k still gives them that headroom (4.2),
which makes it inert at k = 1, and it ships in the same build as the headroom. AIRA-318 keeps its k = 1 ratchet and its own
ticket.

95% rather than a tighter threshold: 485 of the 1,996 at-cap rows sit at 95-99.9% of their cap
rather than at it (1,511 at >= 99.9%), so 95% also catches near-cap anonymous jobs. But at 99% the
real-growth cases fall only from 65 to 63 (max ratio 1.166) while the candidate ratchet steps
fall from 108 to 86 (output, part 3). The threshold does not separate the two populations;
the headroom does, because it lets a real grower finish below 95% of its limit.

## 5. The headroom rule

### 5.1 The rule

For a grant whose cap the client will take from the daemon (cap-source `auto:daemon-reserve`;
D1 decides which rungs):

```
booking C = the reserve resolveAdmitReserve returns today (unchanged; the ledger charges C)
limit   L = max(C, min(ceil(k x C), C + 1 GiB))      (bytes, no rounding; L == C when k == 1)
```

`k` is an install knob (`AIRA_DAEMON_CAP_HEADROOM`, written by step 0's `--cap-headroom` flag,
default 1.0 at first release, raised to 1.5 then 2 by the rollout in section 8; the value must be
in [1, 4]; read once at daemon start, so changing it means re-running install and restarting the
daemon). The 1 GiB term is a constant (option G, section 3).

**Rounding: the wire value and the enforced cap are different numbers, and only the wire value is
the daemon's.** The daemon computes C and L in whole bytes and never page-rounds either; `ceil`
applies only to the product k x C. The client validates the wire values (`limit >= reserve`,
positive) and writes `limit` to `memory.max` unrounded, exactly as it writes `reserve` today
(`writeScopeMemoryCap` writes the raw value and verifies against `floorMemoryPage`,
`confine_linux.go:2239-2260`). The kernel floors both to a page, and flooring is monotone, so
`floor(L) >= floor(C)`: an admissible grant can never become an invalid one through rounding. A
daemon-side page floor is the bug this forbids: with 4096-byte pages, C = 1,073,741,825 and a
bound of C + 1,000 would floor L to 1,073,741,824 < C and the client would refuse a grant that
fitted (test H1/H6 use these unaligned values).

**Who gets it.** Every grant whose cap the client takes from the daemon, which includes
`--delegate-ram` parents: since S2a a delegate job is an ordinary confine job, the daemon reads no
delegate flag (`protocol.go:129-132`, `admit.go:3202-3204`), its parent scope is sized for the
supervisor while its pytest workers reserve as sibling scopes through `worker-admit`
(`confine_linux.go:1115-1127`), and its signature prefix only namespaces its history. No reason
to exclude it survives, and excluding it would need a new admit field an older daemon refuses
(AIRA-340 pattern) or a daemon branch on a signature prefix. Not covered: pinned jobs (D3), aitest
workers (their grants come from the separate `worker-admit` verb, whose response gains no field),
ci-shim mode (it writes no kernel caps). Exclusive jobs are covered (they are alone on the slice).

**If D4 keeps the physical-fit term** (not recommended), L gains one more term:

```
free = ceiling - headroom - effectiveCurrent
L    = max(C, min(ceil(k x C), C + 1 GiB, free))
```

`effectiveCurrent` is the slice's `memory.current` less reclaimable cache, the reading
`checkedAvailable` already takes in the same pass; `ceiling` is the (possibly throttled) admission
ceiling. Earlier grants of the same pass are **not** subtracted: that would treat one arbitrary
subset of granted-but-not-yet-allocated jobs differently from the jobs granted one pass earlier,
which are equally absent from `memory.current`; subtracting all of them is subtracting the ledger,
which removes the very headroom the owner asked for. The term is a grant-time snapshot, honestly
described as such. It is compatible with AIRA-103 (which keeps throttled values out of
kernel-enforced numbers) for that rule's three reasons: the throttle can only take headroom away,
never push L below the booking; L is written once, before the job starts; nothing re-writes it, so
there is no restore path. Lowering L on a running job is out of scope for exactly those reasons.

### 5.2 Retired invariant and the bound that replaces it

Today, for auto jobs, `memory.max` = booking, so the fit check's `sum(booking) <= ceiling` also
bounds their caps. With headroom it bounds only the booking. What replaces it is a **grant-time
bound, not a standing invariant**: at the instant an auto job is granted, its
L <= min(k x C, C + 1 GiB), so `sum(memory.max of auto jobs) <= sum(min(k x C, C + 1 GiB))
<= k x sum(their booking)`. With AIRA-324: `sum(memory.max) <= max(k, 1/0.8) x sum(charge)` at
grant. It is valid only while k is fixed and
no exception applies: `--resize` (lowers the booking, not the cap), a ceiling that falls after the
grant (the throttle), a restart re-declare charged above the ceiling, a change of k (new grants
only). It is used to reason about how bad an overrun can be, never as a gate.

### 5.3 Failure modes

| Failure | First symptom | What limits it |
|---|---|---|
| Jobs grow into headroom together and the slice passes its throttled ceiling | MemAvailable falls into the throttle's free-memory margin (`freeMin` 8 GiB); the ceiling throttle cuts new admissions | the throttle (new grants only); each job's extra is at most min(C, 1 GiB) |
| ... and passes the slice's `MemoryHigh` (60G here) | `memory.high` reclaim and throttling of every job in the slice, cache first; PSI rises | page cache absorbs much of it; the rollback triggers in 8 |
| ... and reaches the slice's `MemoryMax` (64G here) | **slice-level OOM**; with `memory.oom.group` set, the whole scope of the chosen task dies | the kernel picks the task with the highest badness (largest RSS) unless OOM steering raised the job in its headroom to 1000; **steering is off on this box** (no `AIRA_DAEMON_OOM_STEER_MODE` in the unit; default off, `paths.go:243`), so an innocent large job is the likely victim |
| Outside-slice memory is large (the reason the ceiling is throttled) | the box, not the slice, runs short: swap use by processes outside the slice | the throttle, the watchdog (which exempts confined work, so it is no backstop here) |
| Correlated burst of one OOM-prone command | section 2.4: 2.4% of 20-job bursts over the booking at k = 2, p99 1.87 | **steering is the only mitigation** once it happens (once on, it prefers the jobs in their headroom); the 1 GiB term bounds each member's extra. No grant-time term helps: members granted before any has allocated all see the same real use, so a physical-fit term (D4) would give each the full limit; it would cut L only for members granted after earlier ones have ramped up |
| Cache-filling job | ratchet (section 4.1) | the learning rule (4.2) |

Steering's budget is the booking (`admitScopeBudgets`, `oomsteer.go:453`, through
`ledgerCharge`), so a job in its headroom reads as over budget, with two limits the build must
test before enforce: a scope counts as an offender only past its booking **plus a 64 MiB floor**
(`oomSteerOverrunFloorDefault`), so a small job using its headroom (32 MiB booked, 64 MiB cap) is
never raised; and steering activates at 90% of the slice's **kernel** `memory.max` (64G here), not
the throttled admission ceiling, so it does nothing while outside-slice pressure is the problem.
No change to steering's logic is proposed. Switching it on is condition (b), and today it can only
be done by hand-editing the unit: `AIRA_DAEMON_OOM_STEER_MODE` has no install flag
(`install.go:53`, "forced off, with no flag yet"), the live unit carries no steering line, and the
default is off (`paths.go:242-243`); a hand edit is overwritten by the next install that rewrites
the unit. Step 0 (section 8) adds the flag. Steering has never run in enforce on this box, which is
why the rollout runs it in observe first. Its doc comment already anticipates "any scope whose cap exceeds its
reserve" (`oomsteer.go:23-26`) and needs only a pointer.

## 6. Every site that reads the booking or the cap

Rounds 1-3 of review each found one more reader that took the stored or enforced cap for the
booking, so this table is built from a search, not from memory. The build re-runs it and adds a
row for any hit not listed here, or fails review:

```
grep -rn --include=*.go -E '\.Budget\b|\.BudgetBasis\b|budget_basis|ScopeMemoryMax|\bCap\b|CapBytes|confine_peak_history' internal cmd | grep -v _test.go
```

(run on master at the spec's base, 2026-11-15: 33 files. Every hit is a row below except those that
are not this feature's quantity: the gate output caps (`gate/command.go`, `store/gate_command.go`,
`store/gate_write.go`), `worktree/audit.go`'s lease budget, the `--budget` flag's own parsing in
`cmd/aira/main.go` (`:303`, `:1480`, `:1544`, `:2372`), `run_input_linux.go`'s backoff
comment, the minimum-cap constant (`memory_size.go:13`, `:94`), struct and comment hits
(`runner/types.go`, `runner/confine_manage.go`, `runner/confine_dump.go`, `daemon/admit.go:43`).
The client half of the report writer is `admission_linux.go:902-904`, which gains the `reserve`
argument with the writer row.)

**Booking readers (the ledger).**

| Site | Decision |
|---|---|
| `resolveAdmitReserve`, `ledgerCharge`, fit check, head eligibility (`admitCouldFitEmptyLocked`), bypass budget, queue position, queued-ahead bytes | the booking C, unchanged. L never enters admission arithmetic. Head eligibility needs no change: L >= C is chosen at grant and cannot block. |
| Grant frame | `reserve` stays C. New optional field `limit` = L, **omitted when L == C**. |
| Re-declare after reconnect or daemon restart | unchanged: the frozen ARDR record carries `grant.Reserve` (`lease_keeper_linux.go:102`), which is C, so a restart books the booking, not the enlarged limit. The scope's `memory.max` (L) survives in the kernel. **Nothing in the daemon reads a scope's `memory.max` to rebuild a booking** (S12 deleted scan adoption; the daemon's `memory.max` reads are of the slice, `admit.go:3541`, `sliceceiling.go:402`, and the watchdog's `watchdog.go:836`); the build must not add one, and test H5 pins it. |
| `--resize` (`confine_resize.go`) | lowers the booking only, as today; "must name a smaller reserve" is judged against C; the floor stays live use, which may now be above C. L is untouched. |
| OOM steering (`admitScopeBudgets`) | the booking C (section 5.3). |
| AIRA-340 priority | none: priority orders the queue; L is computed at grant whatever the tier. |
| Dump waiter and queue rows (`daemon/ci_dump.go`, `buildConfineDumpQueuesAndWaiters`) and the dump's slice `memory.max` (`ci_dump.go:248`) | ledger bookings and the slice's own cap; unchanged. |

**Stored history readers and writers** (`confine_peak_history`: `budget`, `budget_basis`, the new
`reserve`).

| Site | Today | Decision |
|---|---|---|
| Estimator stats `ResourcePeakHistory` (`confine_peak_history.go:205`, via `ConfinePeakHistory` `:199`) | peaks and the OOM bit only | the learning rule (4.2): it now reads `budget`, `budget_basis`, `reserve` and takes the daemon's k. The only reader whose arithmetic changes for admission. |
| `ConfinePeakP90` (`confine_peak_history.go:226`) | raw maxima across signatures | AIRA-341 deletes it before k > 1 (condition (d)); otherwise the learning rule applies (4.2). |
| `ClassifyResourceBudget` (`resource_budget.go:97`), behind `confine --budget` (`daemon/confine_budget.go:46-52`, paged `confine_history_page.go:152-155`), the resource-budget gauge (`resource_budget.go:350-378`) and the renderer (`cmd/aira/confine_budget_render.go:74`) | the newest row's `budget` is the "current budget"; every direction compares it with the clean max (`resource_budget.go:113-134`, `:239-250`) and the sentence says "granted <budget>" (`:299-315`) | **6.1 below**: the fit and lowering direction compares the booking, the raising and OOM direction the cap; both numbers printed. |
| Dump admission rows `buildConfineDumpAdmissions` (`daemon/ci_dump.go:101-121`, paged `confine_history_page.go:133`) | `budget` published as `declared_reserve_bytes` / `declared_reserve_basis` | `declared_reserve_bytes` = the row's `reserve` when it is non-NULL and below `budget`, else `budget` (today's value, the same at k = 1); new optional `cap_bytes` = `budget` on such a row. A headroom row's cap never appears under a reserve label. |
| Writer: `ConfineBudgetTerm` (`runner/confine_budget.go:77`) and the report handler (`daemon/confine_report.go:24-112`) | `budget` = the scope's `memory.max` as the kernel holds it (the page floor of the written value) | `budget` unchanged (now the floor of L); the report gains optional `reserve` = the grant's C, sent only when cap-source is `auto:daemon-reserve`. The handler accepts any positive `reserve` and does **not** require `reserve <= budget`: at k = 1 an unaligned C gives `budget` = floor(C) < `reserve`, which the learning rule reads as "no headroom", correctly. |
| Writer: the aitest relay `aira confine-report` flags (`cmd/aira/main.go:1367-1434`) | pytest-worker rows with `budget` | no `reserve`; worker grants get no headroom (section 5.1). Unchanged. |
| Retention trim (`confine_peak_history.go:178`), schema (`store.go:874-886`, `:1322-1330`), test fixture insert (`testsupport.go:48`) | | one nullable column added in the DDL and the column list (section 9); the trim reads no budget. |

**Scope-cap readers** (the client's `ScopeMemoryMax`, or the kernel's scope `memory.max`).

| Site | Today | Decision |
|---|---|---|
| Client cap write (`confine_linux.go:1119-1127`) | `memory.max` = the grant's `reserve` | `memory.max` = `limit` when present and valid (`reserve <= limit`, positive, both compared as the wire's whole bytes), else `reserve`; written unrounded, the kernel floors it (5.1, rounding); an invalid `limit` makes the grant invalid (the existing `validRunnerAdmitGrant` path, refuse never repair). cap-source stays `auto:daemon-reserve`. |
| Trailer `scope-memory.max=enforced=`, `binding=`, `effective=` (`confine.go:1306-1309`, `confine_linux.go:1133-1147`) | the scope cap | L as the kernel holds it; `reserve=` stays C (two keys exist already). **No reason label**: the client receives only C and L and cannot tell whether L came from k, from the 1 GiB term or (under D4) from free memory, so it reports the two numbers and nothing that claims a cause. |
| OOM advisory `formatConfineReserveAdvisory` (`confine_linux.go:1618`, `:1815-1918`) | the cap as "its memory cap"; `confineSuggestedReserve(cap, peak)` for the pin it offers | the raising direction keeps the cap: the kill was at L, so the suggested pin stays 1.5 x this run's figure. The head names both ("OOM-killed at its memory cap L, booked C") when L > C, and the auto branch's "this cap is AIRA's OWN AUTO-ESTIMATE" becomes "AIRA's own limit for an auto-sized job: its estimate C plus headroom". The re-run advice stays first; a pin taken from it gives up the headroom (D3). |
| AIRA-334 over-reserve hint (`confine.go:1180-1240`) | the booking `ReserveBytes`, only when it equals the operator's pin | pins only (D3); unaffected. Its comment's promise (the next unpinned run is never below 1.15x the largest retained clean peak) holds for the peak it cites: that run was pinned, its row has NULL `reserve`, and the learning rule reads it raw. |
| `--summary-file` (`confine_summary.go:65-85`) | `reserve_bytes` only | add `cap_bytes` (the scope's `memory.max`, or `unevaluated`); `reserve_bytes` keeps its meaning, the booking. No reason field. |
| `confine --list` (`confine_manage_linux.go:318-324`, printed at `cmd/aira/main.go:3705-3719`) | RESERVE (ledger) and CAP (kernel) columns | already two numbers; unchanged. Sums use the booking; no renderer may label L as reserved. |
| `aira top` (`cmd/aira/tui_top.go:108-153`, `:714`, `:933-947`) | draws the ledger booking only, never the cap (AIRA-192); a scope's use is clamped to its booking in the bar, documented as a transient overshoot | the bar stays the booking. With headroom the clamp can hide **sustained** use above the booking (drawn as a wholly solid span, "using at least its whole charge"), so the clamp's comment is revised to say so; the table's live-RAM cell (`topRAMCell`) is unclamped and shows the real use. No cap column (deferral). |
| `ResolveConfineReserve` (`confine.go:108-118`) | the operator's `--memory-max` | pinned (D3); unaffected. `--memory-high` requires `--memory-max` (`memory_size.go:88-91`), so neither meets headroom. |
| `aira run` caps and estimate (`runner_linux.go:968-1000`, `runner_shim_linux.go:250-254`, `detach_linux.go:194`, `:292`, `evidence.go:39`, `core.go:1826`; its estimate `core/resource_estimate.go:18` reads the `runs` table through `runner/peak_rss_history.go:23`) | the operator's `--memory-max` only; its own history | no daemon grant cap and no read of `confine_peak_history`; unaffected. |
| ci-shim (`confine_shim_linux.go:70`, `:225`) | writes no kernel cap | unaffected. |
| `CapBytes` of the launch and detach records, `ConfineStatus.Cap` (`confine_detach*.go`, `confine.go:267`, `:1245-1270`, `confine_linux.go:632`) | the **slice's** cap | not a scope cap; unaffected. |

**AIRA-345** (held: hint below a recent OOM peak) would read OOM rows' `budget`, which is now about
L: the right floor for that hint, since the kill was at L.

### 6.1 The `--budget` classifier with headroom rows

The round-3 review measured what reading `budget` as the current budget would do once it holds L:
on a copy of the store with every auto row's budget rescaled to min(2C, C + 1 GiB) and peaks
unchanged, 2,417 of 2,672 well-fitted subjects turn over-provisioned (ratio about 2.3), each with
advice to pin `--memory-reserve` near C, which (D3) removes its headroom; 28 under-provisioned
subjects turn acceptable or well-fitted. The classifier therefore splits the two directions:

- **Current cap K** = the newest row's `budget`, as today. **Current booking B** = that row's
  `reserve` when it is non-NULL and below K, else K. Every row without a `reserve` gives B = K,
  so a store with no headroom rows classifies byte-for-byte as today.
- **Raising and OOM direction: the cap.** OOM rows are bucketed at-or-above versus below K; the
  OOM bypass and `recommend:raise` are unchanged (a kill happens at the cap). A clean max M above K
  is under-provisioned, as today.
- **Fit and lowering direction: the booking.** Otherwise the margin is B / M: below 1.25
  well-fitted, 1.25-2 acceptable, 2 or more over-provisioned with `recommend:lower` computed from
  B. In steady state B = C, about 1.15 x M, so the ratio and the direction are today's whatever k
  is; the probe's flip to over-provisioned does not arise (this follows from the definition and
  is pinned by H16, not re-measured).
- **A clean max between the booking and the cap** (B < M <= K: the job ran into its headroom and
  finished) is well-fitted, not under-provisioned: it completed under its limit, and the
  estimator books 1.15 x M next unless the row is an at-limit row the learning rule holds at its
  booking (4.2). Per-row buckets for a headroom row compare its own `reserve` with its peak, and a
  peak above the booking goes to a new bucket `headroom-used` rather than `shortfall(<1.0)`, so
  the published bucket counts say how often headroom was used.
- **Wording.** When B < K the sentence reads "booked B, limit K (basis)" instead of "granted K",
  and an over-provisioned recommendation adds that a pin gives up the headroom. The wire row
  (`ConfineBudgetRow`) gains optional `reserve` (B) beside `budget` (K); the renderer prints both.
  The caller's pre-flight request (`ResourceBudgetRequest`) carries one figure and keeps B = K.

## 7. Interaction with AIRA-324, and order

Shared: the idea that what the ledger charges and what the kernel enforces differ; the restated
grant-time bound (5.2); the steering comment; step 0's steering install flag, which AIRA-324's
enforce also needs (whichever lands first carries it). Under D4 only, the physical free figure
(324 requires `pin <= ceiling - headroom - effectiveCurrent`; D4 uses the same quantity as an
upper clamp), which should then be one helper. Not shared: 324 adds a `charge` on the waiter and a `discountable` admit
field and leaves the grant echo at the pin; this leaves the waiter's charge at the reserve and adds
a `limit` to the grant. Neither changes the ARDR record.

The one place they could conflict is the bound and OOM steering: with both, a pinned job's charge
is 0.8 x pin and an auto job's is C, both below their caps, and steering ranks both by charge, so
the conflict is only in the restated text, which whichever lands second updates. **Order: AIRA-324
first** (owner's sequencing, and its enforce weeks calibrate the slack both spend), then this
feature's build (the learning rule and the headroom together, section 4.4). Step 0 (the install
flags) does not depend on 324 and may land earlier.

## 8. Rollout, gates and rollback

0. **Step 0: install flags.** `aira install --cap-headroom K` writes `AIRA_DAEMON_CAP_HEADROOM`
   (refused outside [1, 4]) and `--oom-steer-mode off|observe|enforce` writes
   `AIRA_DAEMON_OOM_STEER_MODE`, both following the existing slice-ceiling-mode pattern
   (`resolveDaemonModes` keeps the installed value when the flag is omitted, so a later install
   does not silently reset it; the daemon-mode report compares the live environment with the unit,
   `install.go:2436`). Small and independent of everything else here; AIRA-324's enforce needs the
   steering flag too, so whichever lands first carries it. Test H15.
1. **Switch OOM steering on**: observe for a week (it logs who it would raise), then enforce. It has
   never run in enforce on this box. This is also a precondition AIRA-324's enforce should state.
2. **The build** (the learning rule, the `reserve` column and the headroom, after AIRA-324), shipped
   at k = 1.0, where it is inert: grants and caps are byte-identical to master (H7) and the learning
   rule reads every row as today (4.2).
3. **k = 1.5 for a week, then 2.** Gate to start: the replay of section 2.4 re-run on
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
   `some avg10` (a moving average, compared as sampled); ceiling throttle cuts; steering raises;
   the booking ratio of consecutive clean rows of one signature that reached a limit above their
   booking (expected 1.00: the learning rule holding).
   Auto kills are counted only when the confine trailer attributes them to the scope's **own**
   limit (the runner already distinguishes own-limit, descendant and ancestor kills,
   `confine_linux.go:1509-1518`); the stored `oom` bit does not, so the store alone cannot give
   this metric (section 2.3).
5. **Rollback** (any one sets k back to 1.0, which writes exactly today's caps for new grants, and
   files a finding). **It is a manual check**, not automated: the agent driving the rollout reads
   the triggers once a day during the k = 1.5 and k = 2 weeks and weekly after (an automatic
   rollback is a deferral). **Changing k needs a daemon restart**: the knob is read at daemon
   start, so a rollback is `aira install --cap-headroom 1` followed by an explicit
   `systemctl --user restart aira-daemon.service` (an install alone may not restart it), after
   `aira confine --list` and a heads-up to the sessions using the box. A restart is job-safe: live
   leases re-declare their booking C and their scopes keep the L they were created with, so a
   rollback stops new headroom and running jobs keep theirs until they end. **What a rollback does
   to history:** rows written at the higher k no longer meet the learning rule's k condition (4.2),
   so they are read raw and the next grants are master's from the same rows: no job is capped below
   a peak it completed (a 1.95 GiB anonymous run under C = 1 GiB, L = 2 GiB books 2.24 GiB at
   k = 1, not 1 GiB; output, part 5), and a cache-filled row steps its booking once to 1.15 x the
   limit it filled, at most 1.15 x (C + 1 GiB) per signature, then follows AIRA-318's k = 1
   ratchet as on master. That one-time step is the rollback's cost and is visible as booking growth
   in the week after (the third trigger below reads it; it is expected after a rollback, not a new
   ratchet). Lowering k from 2 to 1.5 does the same for the rows whose limit 1.5 no longer gives.
   Triggers: any increment of aira.slice's own `memory.events.local` `oom` (the slice's
   **own** limit was reached, the kernel's attribution, which a scope's local kill counter cannot
   give because a kill under an ancestor limit also counts in the victim scope); the hourly
   increment of the slice's `memory.events.local` `high`, or PSI `some avg10`, above its
   pre-headroom hourly 99th percentile for two consecutive hours (that baseline is recorded during
   rollout steps 1-2); the median booking of repeat signatures growing more than 15% in a week (a
   ratchet the learning rule missed).

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
- `--budget` row `reserve` and dump row `cap_bytes` (section 6): optional fields in daemon
  responses, omitted on rows without headroom; an old client ignores them and prints today's
  numbers, which for a headroom row are the cap (the honest reading is from a new client).
- Store: one nullable column, in the DDL and the `ensureConfinePeakHistorySubject` list, no CHECK,
  no data migration (old rows NULL, which the learning rule reads exactly as today).
- ARDR re-declare record: unchanged.
- Step 0's install flags: environment lines in the daemon unit, not protocol.

## 10. Tests required (each must fail against the wrong implementation)

- **H1. Grant.** At k = 2: an auto grant with C = 512 MiB carries `limit` = 1 GiB (k bound); with
  C = 3 GiB, `limit` = 4 GiB (1 GiB bound); at the crossover C = 1 GiB both give 2 GiB; at k = 1,
  no `limit`. Unaligned bookings: C = 1,073,741,825 at k = 1.5 gives `limit` = ceil(1.5 x C) =
  1,610,612,738, and at k = 2 gives C + 1 GiB = 2,147,483,649, each carried unrounded and each
  >= `reserve`. Mutations, each red: page-floor L in the daemon (the unaligned case then sends
  `limit` < `reserve` and H6 refuses it); drop the 1 GiB term; floor instead of ceil k x C; send
  `limit` == `reserve`. **Under D4 only**, add: free between C and the k/1 GiB bound gives
  `limit` = free, including free = C + 1,000 with the unaligned C above (must be >= C on the wire);
  free below C gives no `limit`; mutation: ignore the physical term. (Test H2, the same-pass
  subtraction, is cut with that subtraction, 5.1.)
- **H3. Who gets it.** Pinned (`--memory-reserve`, `--memory-max`), aitest worker (`worker-admit`),
  ci-shim: no `limit`. A `--delegate-ram` parent with no pin **does** get one, through the real
  admit exchange, with no new admit field (the daemon refuses `delegate_ram`). D1's choice pinned
  by one case per rung. Mutations: headroom for a pin; no headroom for the delegate parent.
- **H4. Admission arithmetic never reads L.** A guard test (the same allow-list form as AIRA-324's
  `.reserve` guard) fails if the waiter's limit is read outside the grant echo and the display
  paths; ledger, fit, head eligibility, bypass and steering budgets equal C with L = 2C.
- **H5. Restart.** After a daemon restart, the re-declared lease is booked C (not L) and the scope's
  `memory.max` is still L. Mutation: the keeper re-declares the limit.
- **H6. Client.** `memory.max` = `limit` when present; = `reserve` when absent; a `limit` below
  `reserve` or non-positive makes the grant invalid. Unaligned: `reserve` = 1,073,741,825 with
  `limit` = `reserve` + 1,000 is valid, written unrounded, and the kernel's `memory.max` reads back
  `floorMemoryPage` of it (equal to the floor of `reserve`; the write must not be refused as a
  mismatch). Mutations: write `reserve` always; accept `limit < reserve`; page-round before the
  `limit >= reserve` comparison.
- **H7. Kill switch.** (a) At k = 1.0 the grant frames and every scope's `memory.max` are
  byte-identical to master's for a table of auto, pinned and fallback cases. (b) **Rollback with
  history written at k > 1:** a store holding rows written at k = 2 (clean at-limit rows with
  `reserve` = C, `budget` = 2C: one anonymous at 0.975 x 2C, one cache-filled at 2C; a clean row
  below 95%; an OOM row at 2C), read by a daemon at k = 1.0, gives the same reserve and basis as
  master's estimator given the same rows without the `reserve` column, and the grant carries no
  `limit`. Mutation: drop the k condition of 4.2 (the anonymous case books C, below its completed
  peak; red).
- **H8. Learning rule** (unit tests over the `ResourcePeakHistory` stats). (a) The six-run
  cache-filling job of section 4.1 stays at its booking at k = 1.5 and k = 2. (b) Rows with NULL
  `reserve`, and rows with `reserve` == `budget` (k = 1), are read exactly as today: an at-cap
  clean row at k = 1 still raises the estimate to 1.15 x its peak. (c) A clean run below 95% of its
  limit still raises the estimate to 1.15 x its peak. (d) An uncapped row (`reserve:*` or empty
  basis) at 4x its booking is not censored. (e) The finite-cache case of 4.3: a clean peak of
  1.5 x C under `budget` = 2C, `reserve` = C gives the next estimate 1.725 C, and a second such row
  under the new booking leaves it at 1.725 C (one step, pinned); and the large case, C = 10 GiB at
  k = 2 (`budget` = 11 GiB) with a clean peak of 10.44 GiB gives 12.006 GiB, above C + 1.15 GiB,
  and stays there. (f) An escalated 3C booking followed by 20 cache-filled clean rows stays at 3C
  after the OOM row is evicted (4.3). (g) **k lowered:** a clean row with `reserve` = 1 GiB,
  `budget` = 2 GiB, peak 1.95 GiB is censored at k = 2 (estimate 1 GiB, next limit 2 GiB >= the
  peak) and read raw at k = 1.5 and k = 1 (estimate 1.15 x 1.95 GiB); with C = 3 GiB,
  `budget` = 4 GiB it stays censored when k drops from 2 to 1.5, where the 1 GiB term still gives
  4 GiB. (h) An unaligned k = 1 row (`reserve` = 1,073,741,825, `budget` = its page floor, peak at
  the budget) is read raw. Property: for every retained clean row, the next limit computed from the
  stats at the current k is at least that row's peak (randomised over C, k_then, k_now in [1, 4],
  peaks in [0.5 x C, `budget`]).
  Mutations, each red: AIRA-318's cap-based rule (doubles at k = 2); no cap (2.3x per run); the cap
  applied to rows with NULL `reserve` or `reserve` >= `budget` (b and h go red); the cap applied to
  OOM rows; to rows below 95%; to an uncapped row; the k condition dropped (g and the property go
  red).
- **H9. Report and store.** `reserve` round-trips through the real `ReportPeakSample` and handler;
  absent is NULL; against an old daemon the re-send keeps the peak and the OOM bit and drops
  `reserve` (AIRA-323's T9 cases extended). Mutation: the re-send keeps the new field.
- **H10. Honesty.** The trailer, `--summary-file` (`reserve_bytes`, new `cap_bytes`) and `--list`
  show `reserve` = C and the cap = L as two numbers, with no reason label; the OOM advisory names
  both. Two grants with identical C and L (one bound by k, one by the 1 GiB term or, under D4, by
  free memory) produce identical client output, through the real admit exchange. Mutations:
  `reserve=` shows L; `cap_bytes` shows C.
- **H11. Real cgroup** (skips without delegation): an auto job allocating 1.5 x its booking of
  anonymous memory completes at k = 2 and is OOM-killed at k = 1; the first run's recorded peak is
  about 1.5 x C and its `reserve` is C.
- **H13. Steering sees headroom users** (before enforce, section 5.3): with steering on, a scope
  using its headroom by more than the 64 MiB floor is raised when the slice passes 90% of its
  kernel maximum; a 32 MiB-booked scope using 64 MiB is not (documents the floor's gap).
- **H12. Escalation unchanged** (D2): a kill at L = 2C escalates to 3C. Mutation: escalate from the
  booking.
- **H14. Basis and guidance.** An estimate set by an at-limit-with-headroom row carries
  `,at-limit` and prints the effective peak; one set by an ordinary row does not. The generated
  skill guide carries the revised own-max sentence and the `,at-limit` meaning, and no longer the
  unqualified "never below 1.15x that peak" (asserted on the generated text, as the existing skill
  tests do). Mutations: omit the token; keep the old sentence.
- **H16. Readers of the stored cap** (section 6). `ClassifyResourceBudget` on a subject whose
  newest rows have `reserve` = C, `budget` = 2C and clean peaks of C / 1.15 is well-fitted, and its
  sentence says "booked C, limit 2C"; the same subject with no `reserve` column is what master
  says. An OOM row at 2C with current cap 2C still takes the OOM bypass to under-provisioned with
  `recommend:raise` from the cap. A clean peak between C and 2C is well-fitted and counted in
  `headroom-used`, not `shortfall(<1.0)`. A subject booked at 2.5 x its clean max (any cap) is
  over-provisioned with `recommend:lower` from the booking. The dump row of a headroom row carries
  `declared_reserve_bytes` = C and `cap_bytes` = 2C. The OOM advisory names both numbers.
  Mutations, each red: the classifier reads `budget` for the fit direction (the well-fitted subject
  turns over-provisioned); it reads `reserve` for the OOM comparison (the bypass is lost); the dump
  publishes `budget` as the declared reserve; the advisory prints only the cap.
- **H15. Install flags** (step 0). `--cap-headroom` and `--oom-steer-mode` write their unit lines;
  out-of-range or unknown values are refused with a stable code; omitting a flag on a later install
  keeps the installed value; the daemon-mode report shows a live value that differs from the unit.
  Mutations: an omitted flag resets the value; k = 0.5 accepted.

## 11. Deferrals

- Headroom for aitest workers (`worker_admit.go` has its own sizing).
- An automatic rollback (the triggers of section 8 are read by hand).
- The physical-fit term, if D4 declines it.
- Lowering L on a running job when the box gets busy (AIRA-103's reasons, section 5.1).
- An anonymous-memory peak (a sampler on the monitor tick) that would let a cache-filled run teach
  its real need, and would remove the finite-cache step of 4.3; the learning rule accepts the
  bounded under-booking and that step instead.
- Per-command k, or k learned from history.
- A cap column in `aira top` (the table shows the unclamped live use; `--list` shows the cap).
- A decay for a booking held up by cache-filled rows after an OOM escalation (4.3).
- A complete job lifecycle log (grants, ends, failures) that would make the rollout replay a
  binding gate (section 8).
- AIRA-273 option B (remember the big mode longer), still needed for modes beyond k.
- Recording failed runs (AIRA-264 decided against it).
- The timeline replay itself: it needs two weeks of AIRA-323 rows and is the advisory gate of
  rollout step 3.

## 12. Change log

- 2026-11-15, rev 3 after review round 3 (4 P2, all taken): the learning rule gains a k condition
  (a row is censored only while `limit_k(reserve) >= budget` at the daemon's current k), so a
  rollback or a lowered k reads rows written at a higher k as master would and never caps a job
  below a peak it completed (output, part 5, rev2 vs rev3; H7(b), H8(g)); the finite-cache step's
  bound corrected to 1.0925 x min(k x C, C + 1 GiB), not C + 1.15 GiB (C = 10 GiB steps to
  12.006 GiB; script part 5 now uses the recommended limit and the large case; H8(e)); the ratchet
  exposure relabelled candidate patterns (136 pairs over 106 signatures, not 205; cap proximity
  cannot separate cache from anonymous growth), the fills-any-limit job kept as a hypothetical
  counterexample; section 6 rebuilt from a grep of every reader of stored `budget`/`budget_basis`,
  `ScopeMemoryMax` and `Cap`, which adds the `--budget` classifier and gauge (fit and lowering
  direction compares the booking, raising and OOM the cap, both printed; 6.1), the dump's
  `declared_reserve_bytes` (now the booking, plus `cap_bytes`), the OOM advisory's wording and
  `aira top`'s clamp (H16); the earlier row claiming `aira top` and the dump show the scope cap was
  wrong and is gone.

- 2026-11-15, rev 2 after Fable round 1 (9 P2, all taken): recommended rule is now
  L = min(k x C, C + 1 GiB), the physical-fit term moved to owner decision D4 (not recommended: it
  does not limit a fast burst; the 5.3 burst row corrected) and its same-pass subtraction dropped;
  page rounding defined (daemon sends whole bytes, the kernel floors both values; H1/H6 unaligned
  cases); the trailer's `cap-headroom=` reason label and `cap_headroom` summary field cut (the
  client cannot know the reason); `,at-limit` basis token and the revised skill-guide sentence
  specified (H14); exposure restated as established (136 pairs, 205 signatures) with the next-row
  table added to the script (part 3); the learning rule applies only to rows that ran with
  headroom, so it is inert at k = 1 and ships with the headroom (4.4: alone at k = 1 it would turn
  65 measured real-growth cases into likely kills; 95% kept, 99% does not separate the two); the
  finite-cache one-time step stated as an accepted cost with its bound and pinned by H8(e); step 0
  adds install flags for k and steering (H15), rollback stated as manual with a daemon restart;
  `--delegate-ram` parents included (the daemon cannot tell them apart and there is no reason to).
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
