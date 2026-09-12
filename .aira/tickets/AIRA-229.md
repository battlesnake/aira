---
{"schema":1,"id":"AIRA-229","project":"aira","title":"Delegate outer-cap aggregate guard is gone: Σ(worker memory.max) bounded only by the slice, so a --delegate-ram suite can be whole-suite oom.group-killed","status":"planned","kind":"bug","severity":"P2","assignee":null,"milestone":null,"labels":["admission","aitest","confine","v0.6"],"hold":false,"relations":[{"kind":"relates","from":"AIRA-230","to":"AIRA-229"},{"kind":"relates","from":"AIRA-232","to":"AIRA-229"}]}
---
> **UPDATE 2026-09-12 — PARTIALLY fixed by v0.7 S1 slice v7-1; this ticket does NOT
> close.** v7-1 landed a CLIENT-side aggregate outer-cap guard in the aitest supervisor
> (`supervisor.py` `_would_breach_outer_cap`, consulted at the top of `spawn_worker`) that
> refuses an over-admitting spawn before it is forked — for ONE supervisor per outer scope
> (subpipe Stage-C's shape). It sums only THIS supervisor's own live worker caps. The
> N-supervisors-under-one-outer case (the design's `make -j` use case: several pytest targets
> in ONE `--delegate-ram` confine job) is NOT covered — each supervisor guards only its own Σ
> and their combined Σ can still breach the shared outer cap. That remaining half is **AIRA-232**
> (latent at v0.6 defaults — 2·nCPU·512 MiB ≪ the 48 GiB default outer — and a pre-S2 fork:
> client sibling-sum+live-current vs a daemon-side term). So the "PROPOSED FIX" below is
> realised for the single-supervisor case only; do not read a merged v7-1 as closing this.
>
> Found 2026-09-12 during the v0.7 aitest design review (three-lens adversarial),
> grounded against the shipped v0.6 code (#130). Latent in the released v0.6:
> invisible under the flat-uniform interim, becomes live under any per-test RAM
> sizing (v0.7) OR any large --delegate-ram / --memory-max run today.

## SYMPTOM

A `--delegate-ram` suite's outer scope always has a finite `memory.max` with
`memory.oom.group=1` (`internal/runner/confine_linux.go` refuses to launch a
delegate scope without one). Nothing bounds the SUM of the worker sub-scopes'
`memory.max` against that outer cap: Σ(live worker caps) is bounded only by the
*slice* ceiling. So a suite whose Σ(worker caps) exceeds its own outer cap —
reachable under an explicit small `--delegate-ram`/`--memory-max`, or the 48 GiB
default outer vs a 64 GiB slice on a large run — admits workers past the OUTER
cap. When the outer scope's resident sum crosses its `memory.max`, its
`oom.group=1` fires and **group-kills the whole delegate subtree**: the aitest
supervisor plus every sibling worker. The whole suite dies, not one worker.

## ROOT CAUSE

The admission-counter rebuild's S15 slice **deleted the daemon-side outer-cap
aggregate scan** at `internal/daemon/worker_admit.go:461-466` (the old scan that
summed granted caps under an outer scope and refused a grant that would breach
it). The daemon now checks a worker request only against the **slice** ceiling
(`worker_admit.go:473`). `CreateWorkerScope` writes the requested `memory.max`
to the worker scope **verbatim** (`internal/runner/worker_scope_linux.go:57,87`)
with **no per-worker headroom and no aggregate check**. The doc comment at
`worker_scope_linux.go:52` still cites "the daemon's aggregate admission guard"
— **stale as-built** (the guard it names was deleted).

Why it was invisible in v0.6: the shipped aitest interim uses a flat uniform
`memory.max` = `AIRA_AITEST_ESTIMATED_BYTES` (512 MiB default). 512 MiB × nCPU is
far below any realistic outer cap, so Σ never approached it. Per-test sizing (or
a large delegate run) removes that accidental safety.

## WHY IT MATTERS

v0.6 is the release CI is being told to pin (subpipe Stage-C uses
`--delegate-ram`). Today the exposure is bounded (48 GiB default outer < 64 GiB
slice is safe for typical runs), which is why this is P2 not P1 — but it is a
real whole-suite-kill hazard for any explicit small `--delegate-ram`/`--memory-max`
and it becomes routine the moment v0.7 sizes workers per-test. It is the exact
catastrophe the original aitest §4 "Σ(granted caps under an outer scope) ≤ outer
cap" invariant existed to prevent.

## PROPOSED FIX

The v0.7 design (`docs/superpowers/specs/2026-09-12-aitest-v07-class-sized-workers-design.md`,
§7 item 1 / OD3) fixes it **client-side, in the aitest supervisor**: before
spawning any worker, require

  Σ(this suite's live worker memory.max)
    + Σ(issued-but-ungranted requests)
    + supervisor allowance
    + request
  ≤ effective_outer_cap − headroom

where `effective_outer_cap` is the **min over the scope's cgroup ancestry**
(`effectiveConfineCap`-style, `internal/runner/confine_linux.go`), read once at
startup — NOT a bare `<outer_scope>/memory.max`. The `Σ(pending requests)` term
is load-bearing: without it two concurrently-pending class claims each pass the
guard against the same Σ-live and both get granted, re-opening the kill. Because
the kernel bounds each worker at its own cap, Σ(caps) ≥ Σ(RSS), so bounding
Σ(caps) keeps the outer `oom.group` from firing on the running sum.

This is deliberately client-side: the daemon-side per-outer-scope ledger is a
larger change, and the client already sizes and counts every worker, so the
guard is nearly free there. If a daemon-side guard is preferred instead
(defence for non-aitest delegate consumers), that is a distinct, larger option
for an owner decision.

Also fix the **stale doc comment** at `worker_scope_linux.go:52` regardless of
which path is chosen — it currently promises a guard that no longer exists.

## HOW TO TEST

Real-cgroup: a delegate suite with a small outer `--memory-max` and enough
size-classed workers that Σ(caps) would exceed it. Assert the aggregate guard
REFUSES the over-admitting spawn and the outer `oom.group` NEVER fires. Mutation
guard: removing the `Σ(pending requests)` term must red a test that issues two
class claims concurrently. This is the load-bearing v0.7 S1 correctness test.

## STATUS

Tracked as the load-bearing correctness item of v0.7 S1 (spec §7/§8/OD3). Filing
independently so it is visible as a known v0.6 issue even if v0.7 slips.
