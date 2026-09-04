---
{"schema":1,"id":"AIRA-59","project":"aira","title":"Admission fairness-freeze (1-minute backfill grace) can stall the entire queue despite abundant free RAM/CPU","status":"done","kind":"bug","severity":"P1","assignee":null,"milestone":null,"labels":["confine","daemon","dogfood"],"hold":false,"relations":[]}
---
## Symptom

Live-observed by the project owner tonight: multiple concurrent sessions have `aira confine` jobs waiting on admission, yet the machine shows idle CPU (load average 3.5 on a 16-core box) and abundant free RAM (`MemAvailable` ~44GiB of 78GiB total) — and the `aira.slice` cgroup itself has real headroom too (30.6GB current / 64GB max, and the admission ledger shows only 33280M granted against a 63296M ceiling, i.e. ~30GB of ledger headroom). Despite all of that, small, easily-fittable requests (e.g. a 4G reserve from one of tonight's build agents) sit queued for many minutes with nothing being admitted.

## Root cause (verified by direct source read, mechanism confirmed — live trigger not directly observed)

`internal/daemon/admit.go`'s per-tick admission evaluation (~lines 710-729) walks queued waiters in order and, for the first one that doesn't currently fit (`waiter.reserve > available`), allows later/smaller waiters to backfill around it ONLY until that head waiter has been queued longer than `s.admitBackfillGrace`. Once that grace elapses, the loop sets `frozen = true`, and every waiter after it in the queue is skipped for the rest of that admission pass — REGARDLESS of whether they'd individually fit in the visibly-available headroom. This is deliberate, documented fairness behavior (protect the head-of-line job from being perpetually starved by smaller jobs cutting in), but:

`internal/daemon/paths.go:36`: `defaultAdmitBackfillGrace = time.Minute` — the grace period is only 60 SECONDS. Any request that can't be satisfied within one minute of being enqueued freezes the ENTIRE rest of the queue behind it, no matter how small they are or how much headroom exists for them specifically.

This compounds directly with **AIRA-58** (`--admit-timeout` silently clamped to a hardcoded 30-minute daemon-side ceiling): a stuck head-of-queue waiter that can't be satisfied doesn't get removed from the queue (and stop blocking everyone behind it) until it finally rejects — which, per AIRA-58, takes up to 30 minutes regardless of what timeout anyone requested. So the realistic worst case today is: one request that doesn't fit within 60 seconds can freeze every other session's admission on the shared slice for up to 30 minutes, with the machine sitting visibly idle the entire time.

## What's confirmed vs. inferred

- CONFIRMED (source): the freeze mechanism exists exactly as described, and its grace period is 60 seconds.
- CONFIRMED (live system check tonight): the machine currently has abundant free CPU/RAM/slice headroom while jobs sit queued.
- NOT DIRECTLY CONFIRMED: I don't have a tool to introspect the daemon's live in-memory waiter queue (only `aira confine --list` exists, and it shows admitted jobs, not pending/queued ones), so I cannot show the exact head-of-queue waiter currently triggering a freeze at this moment. The mechanism is the single most plausible explanation given everything else confirmed, but whoever picks this up should add a repro (or at least queue introspection) before treating this as fully closed, not just fix the number and assume it worked.

## Impact

This is a serious usability/throughput problem on a busy shared box (exactly tonight's conditions: 15+ concurrent sessions). A single slow-to-admit or oversized request can stall every other job on the machine for up to half an hour, even though the whole point of building `aira confine`'s admission-queueing (replacing the old OOM-only `whale-run`) was to let jobs share the slice fairly rather than either failing outright or fighting over uncoordinated RAM. As currently tuned, the fairness mechanism can produce WORSE throughput than no fairness mechanism at all.

## Suggested direction

Several independent, likely-complementary angles, not mutually exclusive:
1. `admitBackfillGrace` at 1 minute is almost certainly too aggressive for a system where admission waits routinely run into the hundreds or thousands of seconds under real contention (observed directly, repeatedly, tonight). Reconsider the default — either substantially longer, or scaled to the actual observed wait-time distribution rather than a flat 60s.
2. Consider whether the freeze should be unconditional at all, or scoped more narrowly — e.g. only freeze OTHER waiters requesting a comparable-or-larger reserve than the stuck head waiter, rather than freezing a trivially-small request that could never meaningfully compete with the head waiter for the same capacity.
3. Fixing AIRA-58 (honoring or at least fast-failing a stuck request rather than silently riding out a fixed 30-minute clamp) directly bounds the worst case here even without touching the freeze logic itself.
4. Add queue-introspection (even daemon-log-only, if a full CLI surface is too much) so this class of problem is diagnosable live next time, rather than requiring source-reading and inference to explain what's currently stuck and why.

relates AIRA-58 (same admission subsystem, compounding failure).

## Resolution

Fixed on branch `aira58-59-admission-fairness`, together with AIRA-58.

**The mechanism was confirmed, and the live trigger identified.** The ticket
correctly flagged that it lacked a repro. There is now one:
`TestGovernorPerTestReservationsAreNotStalledByALargeNeighbourHead` drives eight
governor-shaped ~1G pinned per-test reservations concurrently through the real
`admitConnection` behind an unfittable 32G head on a 64G slice with ~30G free. It
admits **0 of 8 against the unfixed code and 8 of 8 on the fix** — the reported
"idle box, everything queued", reproduced deterministically.

Two facts settled the root cause. First, `confine-reserve` (the pytest RAM
governor's per-test reservation) was traced to the **same** `sliceQueue` and the
same freeze loop as a whole-job `aira confine` — there is no separate per-test
admission path — so hundreds of small per-test waiters can be frozen behind one
unrelated large head. Second, the source of those large heads is
`cmd/aira/main.go:857-859` silently overriding `--memory-reserve` with
`--memory-max`, which matches the observed ledger exactly (33280M = 32768M +
512M). That is filed separately as **AIRA-62**.

**Suggestion (2) was rejected, with reasoning.** Freezing only comparable-or-
larger waiters is backwards: large-head starvation comes *specifically* from
small waiters, since they are the only ones that fit in the crumbs the head is
accumulating. Exempting them makes the freeze a no-op in exactly the case it
exists for. The HPC "reserve the head's need and backfill the surplus" variant
degenerates to the current freeze, because the surplus is negative by
construction. Size is the wrong axis; time is the right one.

**Suggestion (3) was half-right and half-dangerous.** Fixing AIRA-58 does not
bound this — done naively it makes it *worse*. The original backfill design
(`2026-08-27-admission-backfill-gc-design.md:61`) rests its starvation-freedom
argument on "ultimately bounded by the 30-min cap", so that buggy clamp was the
freeze's only bound. Raising it to 24h without giving the freeze a bound of its
own would have extended the worst-case slice-wide stall to whatever any caller
requested. The two tickets had to ship together.

**Fix.** The freeze now has its own bound: a queue-level duty cycle derived from
a single anchor instant — hold for at most `admitFreezeMaxHold` (default 2m,
`AIRA_DAEMON_ADMIT_FREEZE_MAX_HOLD`, `0`/`disabled` restores the old unbounded
behaviour), then yield for the same duration, then re-arm. Deriving the phase
rather than storing it makes three defects *unrepresentable* that prose would
otherwise have to forbid: renewing an active hold every pass, clearing the phase
when the head happens to fit, and re-anchoring when the protected waiter changes
— each of which silently restores a ~100% freeze. The phase is deliberately
holder-independent, so a stream of unfittable heads cannot chain fresh holds.
A completed cycle also guarantees at least one *backfilling pass*, not merely
wall time nominally spent yielding.

**Suggestion (1) was not taken:** the 60s grace is unchanged. The duty bound
subsumes it, and raising it would weaken the head-of-line protection this
subsystem currently gets right.

**Suggestion (4) is implemented, bounded:** transition-only daemon logs on
hold/yield/idle naming the protected head, and `confine --list` now reports
queued waiters and freeze phase (read in one locked snapshot with the ledger, so
the summary cannot contradict itself). That is exactly the introspection whose
absence forced this ticket to be root-caused by source reading.

**Honestly stated guarantee.** No single continuous fairness hold exceeds
`admitFreezeMaxHold`, and over completed cycles fairness costs at most half of
wall time. It does **not** guarantee a large head is eventually admitted — and
neither did the old design, because `checkedAvailable` charges real RSS and
adopted scopes the queue cannot drain. The original design doc's
starvation-freedom claim has been corrected in place rather than left to
contradict the code.

Plan and full review history:
`docs/superpowers/specs/2026-09-03-aira58-59-admission-wait-and-freeze-plan.md`.

## Deployed

See AIRA-58's "Deployed" note — same binary, same restart, same reasoning for the timing.
