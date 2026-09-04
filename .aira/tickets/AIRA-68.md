---
{"schema":1,"id":"AIRA-68","project":"aira","title":"Admission ledger reserve leak: ~60GB granted across 23 \"admitted\" jobs, only 3 actually live","status":"done","kind":"bug","severity":"P0","assignee":null,"milestone":null,"labels":["admission","daemon","dogfood"],"hold":false,"relations":[]}
---
## Symptom (found during the AIRA-67 investigation, independently re-verified live)

`aira confine --list` right now: `63138336K granted / 61952M ceiling across 23 admitted jobs` — but only 3 scopes actually exist and are live. This is worse than when first noticed minutes earlier (48GB/14 jobs), i.e. actively growing, not a one-off snapshot artifact.

## Why this matters, urgently

The slice is at ~99.5% of its entire ceiling committed to reserves that don't correspond to any real running job. This directly causes the admission saturation and `reserve-basis=fallback:daemon-unavailable` events several sessions hit tonight (including the job investigated in AIRA-67, immediately before it died) — real jobs with genuinely small requests cannot get admitted because the ledger believes the slice is nearly full, when the actual live footprint is a small fraction of that.

## Relationship to prior work

AIRA-49 (lease-TTL reclaim sweep) and AIRA-74 (restart reserve-ledger reconstruction) both addressed related but distinct failure modes — stuck leases past a TTL, and losing the ledger across a daemon restart. This is a separate, still-open leak: reserves that were legitimately granted are not being released when their owning job actually exits, accumulating over the daemon's uptime rather than at restart time or via TTL expiry.

## Suggested direction (not investigated in depth — flagging for the two-loop, not prescribing)

Likely candidates worth checking first: whether `releaseAdmitWaiter`/the outstanding-reserve accounting has a path where a job's exit is never observed (e.g. a connection close that isn't detected, or a release RPC that's dropped/never sent — note AIRA-67 found related silent-failure modes on the confine-kill dispatch path, which may or may not be connected here); whether the AIRA-74 "adopted" reconstruction only runs at daemon start and never re-validates itself against still-current live state during normal uptime; and whether any of tonight's own dogfooding (many confine jobs launched and killed by build agents, including some that hit connection/auth interruptions) is what's actually populating this specific instance of the leak, as opposed to it being a rare, hard-to-trigger path.

A live restart will very likely clear the immediate leak (reconstruction rebuilds from actual live scopes, so ghost reserves with no backing scope won't be recounted) — but that's a mitigation, not a fix, since whatever causes reserves to leak during normal uptime will presumably resume leaking afterward.

## Resolution (2026-09-04, PR #20, merged `f1b6a78`)

**The 23-vs-3 reading was a misdiagnosis, and the diagnostic that produced it was the first real defect.** `confine --list` fused three structurally different populations into one job count, printed directly beneath a table that lists only scopes: `aira confine` jobs (a row), `aira confine-reserve` per-test reservations (NO cgroup scope at all, so no row), and scan-adopted scopes (a row). 20 of the 23 "admitted jobs" were healthy per-test reservations from a running `--delegate-ram` pytest suite; `Jobs > len(Scopes)` is the EXPECTED shape while such a suite runs.

Ten live samples (`docs/dev/aira68-ledger-sample.sh`) show the aggregate falling as well as rising (21→13 jobs, 64GB→43.5GB, repeatedly). The alleged accumulation is **not reproduced**. Whether a *residual* leaked subset exists is recorded as **`unevaluated`**, not "refuted" — a cardinality comparison cannot establish a per-lease property. No charge-without-discharge path was found by three independent readings of the source, reported as a negative result rather than proof.

Fixed instead:
- **D1** — the fused report: per-population counts and bytes, a `Vanished*` counter (a subset of the scope-backed population), and a **signed** jobs+bytes residual cross-check. Signed and independent because the most plausible regression is byte-only.
- **D2** — a provably unreclaimable lease class: `ReapScopeIfEmpty` returns ENOENT when the scope is already gone, which the old sweep treated as "skip", **every pass, forever** — exactly this ticket's stated failure shape. Now reclaimed on a scan-derived **seen→gone transition** (plain absence is unsafe: a launcher stalled before scope creation would lose its lease, then run entirely uncharged — the AIRA-67 class). Also closed a pre-existing scope-id **ABA** in that sweep.
- **D3** — scope-less reservations still have no backstop. Deliberately **not built** (no observed instance; `architectural-simplicity` prefers documenting the gap); D1 makes the population countable so a genuine wedge is visible.

Both build-review lineages GATE-FAILed and independently found the same three P0s, all addressed: a validate-unlock-discharge race (the vanished discharge is now one critical section; the reap branch re-validates atomically after the syscall); a stale `scopeVanished` acted on while the scan is failing (now requires `!adoptedScanFailed`, fail-closed, resumes on recovery); and cgroup v2 `rename(2)` meaning an absent scope id is not unconditionally a removed cgroup (accepted and documented, bounded like the migrated-leader gap — the release is ledger-only and the memory is still charged via `max(current − reclaimable, Σ reserve)`).

Mutation testing caught **four porous tests before either reviewer ran**, including both headline regression tests for this very leak: they waited for the ledger to reach (0 jobs, 0 bytes), but the last release empties `queue.waiters`, `pruneAdmitQueue` deletes the queue, and the snapshot honestly returns the absent-queue zero — so deleting the discharge arithmetic outright left them green. Final: **21 mutations, all caught, none porous** (`docs/dev/aira68-mutation-check.sh`).

The suggested "restart the daemon to clear it" mitigation is **not needed and was never performed**: there was no ghost aggregate to clear.

## Deployed

Binary rebuilt from merged master (`f1b6a78` + the follow-up ticket commit
`b110c02`), confined build, smoke-tested, installed; `aira-daemon.service`
restarted. Live-verified the fix is actually in effect (not just merged):
`aira confine --list` now prints the split breakdown — `slice reserve: ...
across N admitted jobs` followed by `of which: X confine scopes, Y
scope-less reservations, Z adopted scopes` — replacing the old fused count
that this ticket's original P0 misread as a leak.

Independent verify agent's other findings, both actioned: the flaky-test
disclosure gap (2 more distinct `internal/runner` flakes it hit that the
build agent hadn't named) recorded on AIRA-20; the `aira reconcile --rebuild`
journal corruption (`E_JOURNAL_CORRUPT: duplicate project/seq ... AIRA-1 /
LIFE-1`) filed as its own ticket, AIRA-93, rather than left as a dangling
"worth its own ticket" note.
