---
{"schema":1,"id":"AIRA-232","project":"aira","title":"Multi-supervisor outer-cap aggregate: N aitest supervisors under one outer scope each guard only their own Σ (v7-1 client guard covers one-supervisor-per-outer only)","status":"planned","kind":"bug","severity":"P2","assignee":null,"milestone":null,"labels":["admission","aitest","confine","v0.7"],"hold":false,"relations":[]}
---
> Found 2026-09-12 in the v7-1 build-review (Fable). v7-1 (AIRA-229) landed a CLIENT-side aggregate outer-cap guard that sums THIS supervisor's live worker caps (`self.workers`). It closes one-supervisor-per-outer (subpipe Stage-C's shape). This ticket tracks the OTHER half.

## SYMPTOM

Two (or more) aitest supervisors under ONE outer scope is a daemon-supported configuration (`internal/runner/aitest_bootstrap_linux.go:24-25` accepts a second supervisor already in `<outer>/.aira-supervisor`; `internal/daemon/worker_admit.go` reseeds worker-id collisions) — and it is exactly the design's stated use case: `aira confine --delegate-ram -- make -j` running several pytest targets inside ONE confine job. Each supervisor's v7-1 guard sums only ITS OWN workers, so their combined Σ(caps) can jointly breach the shared outer cap and fire the outer `oom.group` (the very whole-suite kill AIRA-229 is about). The S15-deleted daemon scan summed the outer's REAL children, so it covered this; the client-side replacement does not.

## LATENT AT v0.6 DEFAULTS, LIVE UNDER v0.7 S2

On a 16-core box the CPU ledger bounds total workers to 2·nCPU=32, each 512 MiB flat = 16 GiB < 48 GiB default delegate outer < 64 GiB slice → the aggregate cannot breach at v0.6 defaults (same latent profile as AIRA-229 itself). It goes LIVE under v0.7 S2's per-class sizing (e.g. 32 × 2 GiB = 64 GiB > a 48 GiB outer). So this is an **S2 PREREQUISITE**, not a v7-1 blocker.

## THE FORK (owner decision, pre-S2 — alongside OD1 batch-reinstate)

The spec §7 premise "the client already sizes and counts every worker" is TRUE for one supervisor, FALSE for N. Two options:
1. **Client-side sibling-sum + live-current read + a race-sized margin.** Sum `os.listdir(outer_scope)` children's `memory.max` (not `self.workers`). But (a) the allowance model breaks: N supervisors share one `.aira-supervisor`, so `base + N_live×per_relay` is wrong by an unknowable factor — the natural fix is a live `.aira-supervisor/memory.current` read, which is exactly what the S15-deleted daemon scan did; and (b) a cross-supervisor TOCTOU: two supervisors read children, both see room, both fork, the daemon (slice-ceiling only) grants both — the Σ(pending) problem, but across processes with NO shared map to hold it.
2. **A daemon-side per-outer-scope aggregate term** — a cleaner restore of what S15 deleted (the daemon serialises grants, so no client TOCTOU). Larger change.

Record as a spec-§7 amendment; decide before S2 wires per-class sizing.

## BACKSTOP MEANWHILE

The daemon ledger (Σ leases ≤ slice ceiling) + the MemAvailable watchdog + the outer `oom.group` (a whole-suite kill, the thing we are trying to avoid) — i.e. the spec §8 "bounded, not airtight" envelope, same as v0.6 had (v0.6 had NO client guard at all, so v7-1 is a strict improvement).
