# Discounted admission charge for pinned jobs, and a fast lane for quick jobs

Tickets: AIRA-323 (completion telemetry), AIRA-324 (flat charge discount for pinned jobs),
AIRA-325 (history refinement, gated), AIRA-326 (fast-lane queue order). All held, milestone
v0.41. Status: plan, not built. Process class: **correctness-critical admission core** (it
changes what the ledger charges and the order the evaluator runs): full two-loop, Opus builds
TDD, Fable reviews in a throwaway detached worktree. Daemon-side change: the shared-daemon
restart care applies at cutover.

Revision 2. Revision 1 proposed a history predictor with a pressure valve. Plan review (Sol,
REVISE) and plan gate (Fable, REVISE) found: the headline gain ignored the plan's own
OOM rule; the valve was unspecified and redundant with the existing physical floor; the charge
lifecycle (resize, re-declare, head eligibility, bypass budget) was incomplete; and a flat
0.8 x pin discount, with no history at all, gives almost the same capacity. Revision 2 builds
the flat discount first and demotes history to a gated refinement. Change log in section 11.

Reproduction of every measurement: `docs/dev/aira-history-charge-replay.py` (read-only on
`~/.local/state/aira/state.db`; run it under `aira confine`).

## 1. The problem, in plain terms

Jobs queue (often 20-30 deep) while the box is nearly idle. A snapshot taken for this plan,
2026-11-13, 64 GB slice on an 80 GB machine:

| Measure | Value |
|---|---|
| Running jobs | 20 |
| Booked against the slice (sum of reserves) | 44.4 GB |
| Actual RSS of those jobs | 13 GB (their peaks so far sum to 17.6 GB) |
| Queued jobs | 26, asking for 48 GB |
| Admission ceiling | 45.7 GB (system-pressure throttle, down from 64 GB) |

Admission books each job's **reserve** until it ends, and the reserve is sized for the job's
worst case. Over the last two weeks a finished job peaked at a median of about a third of what
it reserved. Pinned reserves (`--memory-reserve`, about 60% of jobs) are used as is, for the
memory cap and for the booking (`resolveAdmitReserve`: a pinned request returns
`request.reserve`); history is never read for them. Auto-sized reserves use history only for
an exact-argv match with 3+ clean samples (about 15% of jobs: argv carries temp paths and file
lists, 34,731 distinct signatures in two weeks).

A second complaint (owner, 2026-11-13): jobs that are historically quick (under 5 minutes)
and small (under 4 GB) should rarely wait long. The queue is FIFO after manual pushes
(`admitOrderLocked`) plus a bounded bypass past a blocked head (AIRA-307). Nothing knows a job
is quick, and the history table has no duration.

## 2. What the data says

Walk-forward replay (`--memory-reserve` pins only, 50,006 jobs finished since 2026-10-20).
"Charge" is what would be booked; the pin stays the cap. **Above** = share of jobs whose actual
peak (the cgroup's `memory.peak`) exceeded the charge. **Gain** = sum of pins over sum of
charges. The risk columns draw 20 jobs and report how often their total peak exceeded their
total charge, for 20 mixed jobs and for 20 jobs of one command and subcommand ("burst"), and
the 99th percentile of total peak over total charge. OOM-ed rows are in the history and veto a
key, as the history rule below requires.

| Charge rule | Above | Gain | Mixed P(over) / p99 | Burst P(over) / p99 |
|---|---|---|---|---|
| pin (today) | 0% | 1.00x | 0 / 0.61 | 0 / n.a. |
| flat 0.9 x pin | 2.9% | 1.11x | 0 / 0.68 | 0 / 0.78 |
| **flat 0.8 x pin** | 6.1% | **1.25x** | 0 / 0.78 | 0.1% / 0.87 |
| flat 0.7 x pin | 12.4% | 1.43x | 0.1% / 0.90 | 0.9% / 1.00 |
| flat 0.6 x pin | 21.3% | 1.67x | 1.8% / 1.03 | 11.8% / 1.16 |
| history: max x 1.25, else pin (72% of jobs covered) | 3.4% | 1.28x | 0.1% / 0.80 | 0.1% / 0.80 |
| min(0.8 x pin, history) | 8.7% | 1.52x | 0.4% / 0.93 | 0.2% / 0.92 |

Reading it:

- **Flat 0.8 x pin gets 1.25x with a burst p99 of 0.87; history gets 1.28x at 0.80.** The gain
  is the same to within noise. History buys a lower "above" rate (3.4% against 6.1%), not
  capacity. The combination reaches 1.5x but its p99 of 0.92-0.93 leaves little margin.
- 20 jobs at today's pins already peak at only 0.61 of their total pins (p99). Auto-sized jobs
  (0.51) are not touched: their cap is their charge, so a job above it is a real OOM.
- The 3x gap seen in section 1 cannot be closed by prediction: a prediction must cover the
  biggest run of a command, so a typical run uses a quarter to a half of it.
- Coverage for history is 72% of pinned jobs once the OOM veto (any OOM in the key's last 20
  rows) and the `aitest-parent` namespace (kept separate, as `AitestParentSignaturePrefix`
  requires) are honoured. Before that correction the table read 90% and 1.34x.

Limits of this evidence, stated so no one reads more into it:

- The table holds job-**end** rows only. Concurrency is simulated by independent draws, not
  duration-weighted; real clustering in time (a burst of builds starting together) is not
  captured.
- Rows are ordered by end time, so a job is predicted from peers that finished while it was
  still running. That is optimistic for history, worst for same-command bursts. It does not
  touch the flat rows.
- The store keeps 20 rows per exact signature; the replay keeps 20 per coarse key. The build's
  window query must be what is measured (AIRA-325).
- "Pinned" here is `cap:operator:--memory-reserve` rows only.

Section 6 closes these with AIRA-323's start times before anything is enforced.

## 3. Owner decisions this plan rests on (restated, not reopened)

- Dynamic overcommit of unused reservation is wanted, tending to no overcommit as real use
  nears the limit (2026-11-13).
- Historically quick (under 5 min) and small (under 4 GB) jobs go higher in the queue
  (2026-11-13).
- Prior decisions kept: the admission ledger is a counter derived from the waiter set (the
  2026-09-10 rebuild); AIRA-103 keeps every throttle out of anything the kernel enforces; the
  AIRA-307/309 queue rules (pushed first, bounded bypass) stay.

## 4. The from-scratch minimum, and the gap

Only the problem "queued jobs wait while real memory is idle, and quick jobs wait too long".
Greenfield minimum: (1) record how long and how big each job really was; (2) book a pinned job
at a fixed fraction of its pin, never letting the booking exceed what real free memory could
back; (3) run quick jobs first.

Revision 1 was the plan with a per-command history predictor and a pressure valve. Gap
against the minimum, and where it went:

- **History predictor**: same capacity as the flat fraction (section 2), more machinery (key
  chain, window query, index, OOM veto, retention). Kept as a gated refinement (AIRA-325), only
  if the enforced flat discount shows the margin to go lower. The key function is still built
  now, because the fast lane (AIRA-326) needs a per-command class.
- **Pressure valve `f(p)`**: dropped. `checkedAvailable` already books the larger of real use
  and the ledger (`admit.go:2699-2715`), so once real use rises the physical term dominates;
  revision 1 wrongly said live reads had been removed. The one thing the ledger cannot see is
  whether a job's full cap could be backed by real free memory, so the flat discount adds one
  rule for that (section 5.2), and nothing is tuned.
- **Monthly predictor re-selection**: dropped.
- **Per-job live usage in the ledger**: still not built; revisit only if the enforce data says
  the flat discount is not enough.
- **Reserved headroom for the fast lane**: not built; measure fast-class waits first.

## 5. Design

### 5.1 Telemetry (AIRA-323)

Add to each `confine_peak_history` row, written at the existing teardown site
(`confine_report.go` -> `RecordConfinePeak`): `wall_ms` (run time after the grant), `wait_ms`
(admission wait), `started_at` (grant time, so a row is predictable from rows that ended
before it started), and `key` (the normalised command key, below). Nullable; old rows stay NULL
and are never treated as zero. Index `(kind, key, at)`. The 20-row retention stays per exact
signature (unchanged); the per-key history is read with a window query
(`WHERE kind=? AND key=? ORDER BY at DESC LIMIT 20`), one indexed read, never a table scan (the
machine-wide p90 prior's full-table `GROUP BY` already fails its 250 ms deadline on this box,
AIRA-314; this must not repeat).

**Key function.** The command's effective argv, then:
- unwrap only `env` (with its `VAR=value` arguments) and `/usr/bin/time` / `time` (with its
  known option set `-o FILE -f FMT -p -v -a`); a wrapper whose options are not recognised is
  left as the command (measured: a naive unwrap keyed 535 jobs under `-f|...`);
- `aitest-parent` stays the first token: it is a deliberate separate namespace; `timeout
  DURATION`, `nice -n N`, `flock FILE`, `uv run`, `make` are left as the command;
- basename of argv[0]; mask tokens that are paths, numbers or hashes;
- three levels: masked argv (first 14 tokens), first 4 tokens, command + first positional.

The reference is `keys()` in the replay script; the build reproduces it on committed golden
fixtures and the script is updated to match.

### 5.2 Flat charge discount for pinned jobs (AIRA-324)

**Who is discounted.** Only a waiter that is scope-backed AND whose pin the operator gave with
`--memory-reserve` (cap-source `operator:--memory-reserve`). Not `--memory-max`, not `aira run`
admissions (their pin has no flag to attribute), not scope-less `confine-reserve` waiters
(they have no cap), not a `--delegate-ram` parent, not an exclusive job, not aitest workers.
The client sends one new boolean on the admit request, `discountable`, set only in that case;
the daemon never infers it from `pinned`. In ci-shim mode (no physical reading) nothing is
discounted.

**The rule.** `charge = clamp(ceil(factor x pin), 1 MiB, pin)`; `factor` defaults to 0.8
(install knob; 1.0 = off). The grant still echoes the pin, the client still sets `memory.max`
= pin. **No change to the cap.**

**Physical fit (replaces the valve).** A discounted job is granted only if its **full pin** fits
real free memory: `pin <= ceiling - headroom - effectiveCurrent`, where `effectiveCurrent` is
the slice's `memory.current` less reclaimable cache (the reading `checkedAvailable` already
takes). So as real use climbs, large pins stop being admitted before the discount can
overcommit them, and an idle box admits as freely as the ledger allows. No tuned constants.

**Retired invariant, named.** Today `sum(reserve) <= ceiling` also bounds `sum(memory.max)`
(`admit.go:2338`). After this change `sum(charge) <= ceiling` bounds only the booking. What
replaces it is a **grant-time bound, not a standing invariant**: at the instant a discounted
job is granted, its cap is at most `charge / factor`, so the caps of the discounted jobs are at
most `sum(charge) / factor` (1.25 x at 0.8), and non-discounted jobs have cap = charge. It is
valid only while the factor is fixed and no exception applies. Exceptions, each of which breaks
the bound for the jobs it touches: `--resize` (lowers the booking, not the cap, by design); the
AIRA-325 history charge (below `factor x pin`: it must set its own floor ratio, proposed 0.5,
and restate the bound as `ceiling / 0.5`); a ceiling that falls after the grant (the throttle);
and a restart re-declare charged above the ceiling. It is used only to reason about how bad a
slice-level overrun can be, never as a gate. The OOM-steering argument that a job's cap is
within its budget (`oomsteer.go:424-433`) must be restated against it.

**What can still go wrong, accepted explicitly.** Per-job caps cannot cause a new kill. A
**slice-level** OOM can now kill a job that is below its own pin, because the caps can
collectively exceed the slice's limit; steering prefers the job that is over its charge
(`oomsteer.go:252`), but only biases the victim. The watchdog exempts confined work
(`watchdog.go:816`) and is no backstop. The first symptom is more likely memory.high
throttling (`MemoryHigh=60G`) than a kill, so the gates watch both (section 6).

**Charge lifecycle, every site decided** (build must also add a guard test that fails if
`.reserve` is read in admission arithmetic outside this allow-list):

| Site | Decision |
|---|---|
| grant echo / scope `memory.max` | pin (`reserve`), unchanged |
| `ledgerCharge()` and every sum through it | charge |
| fit check (`admit.go:2478`) | charge, plus the physical-fit rule |
| head eligibility `admitCouldFitEmptyLocked` (`:2637`) | ledger part in charge bytes, AND the physical part in pin bytes: a discounted waiter is head-eligible only if `charge <= effectiveMaximum - headroom` **and** `pin <= effectiveMaximum - headroom` (the full pin must be feasible on an empty slice). Charge alone would make a pin above the throttled ceiling a head it can never leave, and a pushed one would wedge the machine (`admit.go:2569` exists to prevent exactly that) |
| bypass budget `admitBypassBudget` (`:425`) and `head.bypassed` | charge bytes throughout |
| queued-ahead bytes, queue position, `--list`, `aira top`, `ci_dump` | show both pin and charge; sums use charge |
| `confine --resize` (`confine_resize.go:121-132`) | writes the charge; "must name a smaller reserve" is judged against the charge; floor stays the job's live use |
| re-declare after reconnect or daemon restart | the ARDR re-declare record is frozen and carries only scope id, RAM bytes (the granted pin), cores and parent (`redeclare.go:74`; the keeper sends `grant.Reserve`, `lease_keeper_linux.go:100`), so **eligibility cannot be reconstructed**. Reconnect to a live daemon: the granted waiter is re-anchored in place and **keeps its existing charge**. After a daemon restart (no memory of the grant): charged the **full pin** (fail-safe, as `--resize` today). Accepted consequence: a restart under load makes the ledger jump to the sum of pins, signed and possibly over the ceiling, so new admissions wait until releases recover it; it is the existing restart behaviour, now with more discount to lose. The frozen ARDR record is not changed by this plan. A change of factor or mode applies to new grants only |
| OOM-steering budget (`admitScopeBudgets`) | charge, as `--resize` does today |
| aitest workers (`worker_admit.go`) | never discounted; they consume the same ledger sums |

**Modes** `off | observe | enforce` on the install-record pattern of the slice ceiling. Observe
computes and logs `would-charge=X charged=Y` per admission and changes nothing. Factor rolls
out 0.9 then 0.8.

### 5.3 History refinement (AIRA-325, gated)

`charge = min(factor x pin, max x 1.25 of the last 20 clean peaks at the finest key with 3+
clean samples)`. **"OOM on record"** means any row with `oom=1` in that key's window of 20,
at the key level being used; such a key is charged its flat charge. At 0.8 it reaches 1.52x
with a p99 of 0.92-0.93 (section 2), a margin too thin to enable on the replay alone. Ships only if
the enforce data on the flat discount, replayed by `started_at`, shows headroom, and decided
with a measured exceedance target, not by default.

### 5.4 Fast-lane queue order (AIRA-326)

A waiter is **fast** if, at enqueue, its key has 3+ clean samples, no OOM in the window, p90
`wall_ms` under 5 minutes, p90 peak under 4 GiB, **and its own ledger charge is at most
4 GiB** (a quick job pinned at 32 GB would otherwise become a blocked head with a large
bypass budget). Unknown is never fast. The class and the bonus are **frozen on the waiter at
enqueue**, from the history read the daemon already makes before taking `queue.mu`; the
evaluator never reads the database.

`admitOrderLocked` already derives the order and stores nothing. Its second tier changes from
enqueue order to ascending `enqueued - bonus x fast`, ties by `seq`. `bonus` defaults to 10
minutes (install knob; 0 = today's order exactly). Pushed waiters still go first, latest push
first. Worker waiters have no key and are never fast. One order source remains for the
evaluator, the "queue position" line and `aira top`.

**What the bonus guarantees, precisely.** A heavy waiter that has waited more than `bonus`
is ahead of every job that arrives after it. It does **not** prevent delay: a fast job arriving
in the heavy job's first `bonus` minutes goes ahead, and, being ahead rather than behind a
head, is not metered by the AIRA-307 budget. That is bounded (the window ends) and cheap (fast
jobs are small). Head selection, the freeze duty cycle, the exclusive gate and bypass
accounting derive from the order each pass and tolerate displacement (a push already does
this). The client's position can move backwards when a fast job arrives; that is displayed,
not hidden.

It fixes who gets freed memory first, not capacity; capacity comes from 5.2.

## 6. Rollout and the gates that decide it

1. AIRA-323 alone, first. Pure data collection; weeks to accrue. Un-hold it first.
2. AIRA-324 in observe for a week of normal load; then enforce at 0.9 for a week, then 0.8.
   **Gate to start enforce:** a counterfactual replay of the real arrival/start/end timeline
   (now available from AIRA-323) that books jobs at the discounted charge, admits more jobs
   than before, and shows the concurrent total of `memory.peak` stays under 0.9 x the slice's
   `MemoryHigh` at the busiest hour, using burst-weighted and duration-weighted draws. The
   pre-enforce kill count is **not** a gate (zero kills before enforce proves nothing about
   after).
   **Rollback triggers** (any one flips the mode back to observe and files a finding): an
   in-slice `oom_kill` of a job whose peak was under its pin; `memory.events` `high` count or
   `memory.pressure` `some avg10` over their pre-enforce weekly 99th percentile for two
   consecutive hours; more than 5% of discounted jobs above their charge in a week.
3. AIRA-325 only after (2) has two clean weeks at 0.8, same gates, factor lowered only via the
   combined rule.
4. AIRA-326 once AIRA-323 has two weeks of `wall_ms`; independent of 324/325. Gate: fast-class
   p95 `wait_ms` falls by half against the prior fortnight, heavy-class p95 `wait_ms` rises by
   at most 10%.

## 7. Invariants the build must preserve

- A job's `memory.max` is never below what its client asked for; only the booking shrinks.
- `sum(charge) <= ceiling - headroom`, signed, as today; no clamp that hides a deficit. At each
  discounted grant, that job's cap <= charge / factor (the grant-time bound of section 5.2,
  with its exceptions; not a standing invariant).
- A discounted job's full pin fits `ceiling - headroom - effectiveCurrent` at grant.
- The order has one source; `queue.waiters` is never reordered; the class is frozen at enqueue.
- Absent or insufficient history means "no discount from history", never a guess or a zero.
- A check that cannot establish its result reports `unevaluated`.

## 8. Tests required (each must fail against the wrong implementation)

1. Discountable job: ledger charge = ceil(0.8 x pin), grant echoes the pin, scope
   `memory.max` = pin. Mutation: grant echoes the charge -> red.
2. Non-discountable (`--memory-max`, `aira run`, scope-less, delegate parent, exclusive,
   worker, shim mode): charge = pin. Mutation: infer from `pinned` -> red.
3. Physical fit: with real free memory below the pin the job waits though the ledger has room;
   a mutation removing the rule -> red.
4. Every row of the lifecycle table has a test; plus a guard test failing on any new
   `.reserve` use in admission arithmetic outside the allow-list.
5. Resize against the charge; reconnect to a live daemon keeps the existing charge; after a
   daemon restart the re-declared lease is charged the full pin (not f(pin), not the resized
   value); a restart under load leaves a signed, possibly negative, available that recovers on
   release; a factor or mode change leaves granted charges alone.
6. Head eligibility: a discounted waiter whose pin is above the (throttled) ceiling is never a
   head even when its charge fits, pushed or not (mutation: charge-only eligibility -> a pushed
   one freezes the queue -> red); bypass budget accumulates in charge bytes over several passes.
7. Key function golden table (wrappers, `time` flags, `timeout`, `nice`, `flock`, `aitest-parent`
   preserved, temp paths, ids, hashes), identical in Go and in the replay script; an unknown
   wrapper option leaves the argv unchanged.
8. Window query: 200k-row fixture, one indexed read, under 250 ms.
9. Order: fast job enqueued 5 min after a heavy one goes ahead, one enqueued 11 min after does
   not; a fast job whose own charge is over 4 GiB is not fast; pushed first; position line and
   `aira top` equal `admitOrderLocked`; class frozen (history changes after enqueue do not move
   it); `bonus = 0` equals today's order (property test over random queues); worker waiters
   never fast; NULL `wall_ms` never fast. Multi-pass: freeze anchor, accumulated bypass and
   yield refills unchanged by displacement.
10. A heavy waiter older than `bonus` is ahead of every later arrival, any arrival rate.

## 9. Risks

- The independence assumption and end-time look-ahead (section 2) are the weak points of the
  evidence; section 6's counterfactual replay on real timestamps is the control, and the plan
  stops at observe if it fails.
- Slice-level OOM can kill a job below its pin (section 5.2): accepted, gated, reversible by a
  mode switch with no migration.
- A first large run of a command after only small ones is invisible to a history charge; the
  flat discount does not depend on history, which is part of why it comes first.
- The discount adds a client-side flag and an admit wire field (protocol bump; AIRA has no
  compatibility obligation).
- Re-opens, narrowly, the live-number-in-admission trade of 2026-09-10: the physical-fit rule
  reads `effectiveCurrent`, which `checkedAvailable` already reads. No per-job read.

## 10. Follow-ups, deliberately not in this plan

- Live per-job usage correction, only if the enforce data says the discount is not enough.
- Held-back headroom for the fast lane, only if its wait target is missed.
- aitest worker pools keep their own cost-ranked ordering (AIRA-261).

## 11. Change log

- Rev 2 after confirmation review: re-declare reconstruction fixed (frozen ARDR record: live
  reconnect keeps the charge, restart charges the full pin); head eligibility needs full-pin
  feasibility; the cap bound restated as a grant-time bound with its exceptions; replay `time`
  option parsing fixed.
- Rev 1 -> 2: flat discount first; history predictor demoted (AIRA-325) with "OOM on record"
  defined; valve dropped for the physical-fit rule; lifecycle table; retired invariant named
  and restated as `sum(memory.max) <= ceiling / factor`; discount restricted to scope-backed
  `--memory-reserve` pins via a new client flag; fast class frozen at enqueue and charge-capped;
  starvation claim corrected; `aitest-parent` namespace kept, `time` flags handled; replay
  corrected (OOM veto, `memory.peak`, leakage and retention caveats stated); gates rebuilt
  (counterfactual replay, memory.high/PSI, explicit rollback); monthly re-selection removed.
