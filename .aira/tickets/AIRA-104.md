---
{"schema":1,"id":"AIRA-104","project":"aira","title":"aira confine job resource profile: surface durable peak-RSS (already captured) and add CPU-time/pids-peak, for exclusive-mode benchmarking","status":"in-progress","kind":"feature","severity":"P2","assignee":null,"milestone":null,"labels":["confine","dogfood","observability"],"hold":false,"relations":[]}
---
Direct owner request (2026-09-05), motivated by AIRA-101 (`aira confine --exclusive`): after an exclusive run finishes and the slice goes back to empty, make it easy to get a resource-usage measurement for the WHOLE job — peak RSS and other things — covering every subprocess it spawned over its life, not just its own top-level process. The stated purpose: exclusive mode should be usable to measure the total resource requirements of a job that may fork many short-lived subprocesses during its run.

## Good news: most of the foundation already exists — this is smaller than it looks

Traced against current source before writing this ticket, rather than assuming a from-scratch build is needed:

- **Peak RSS is already correctly harvested from the right primitive.** `internal/runner/usage_linux.go` already reads `<scope>/memory.peak` — cgroup v2's own high-water-mark of `memory.current` for the WHOLE cgroup subtree. This is exactly right for "a job that spawns many subprocesses": it is a kernel-maintained running maximum across every process ever charged to that cgroup, including ones that have already exited, with no polling and no risk of missing a spike between samples. `internal/runner/types.go`'s `ConfineStatus.PeakRSS *int64` already carries it, and `internal/runner/confine_linux.go`'s reserve-sizing advisory already reads it (`formatConfineReserveAdvisory`).
- **The durable-record machinery to query it after the fact already exists.** AIRA-22's `ConfineDetachRecord.Status *ConfineStatus` (`internal/runner/confine_detach.go`) already embeds the whole `ConfineStatus` struct — including `PeakRSS` — into the durable per-job record `aira confine --status` reads back after a detached job finishes. AIRA-101 already extended this same struct with its own `Exclusive`/`ExclusiveDrainedMS` facets, so this is an actively-maintained pattern, not a stale one.
- **Confirmed live on this kernel** (6.18): `memory.peak`, `cpu.stat` (`usage_usec`/`user_usec`/`system_usec`), and `pids.peak` are all present and readable on a real slice cgroup here.

## What's actually missing

1. **`aira confine --status`'s human-readable renderer does not print `PeakRSS` at all**, even though the durable record already carries it (`grep` for "PeakRSS"/"peak_rss" across `cmd/aira/*.go` finds zero matches). The data has been sitting in the record, unsurfaced, since AIRA-22. This is the most direct, cheapest fix: render what's already captured.
2. **No CPU-time capture exists anywhere.** `cpu.stat`'s `usage_usec` (total CPU time, user+system, for the whole cgroup subtree — the CPU analogue of `memory.peak`, same subprocess-churn-proof property) is not read, not stored, not rendered. Add it following the *exact* pattern `PeakRSS` already established: read in `usage_linux.go` at the same teardown point, add a field to `ConfineStatus`, carry it through the durable record, render it on the trailer and in `--status`.
3. **`pids.peak`** (peak concurrent process count in the subtree) is a natural, cheap third addition — "how parallel did this job get" — same read-once-at-teardown pattern. Include it unless the plan finds a reason not to; it's not load-bearing like the first two.
4. **Tie it to the exclusive-benchmarking story explicitly.** When `Exclusive == "granted"`, the resource numbers on the trailer/`--status` output are the number the owner actually wants — make sure the presentation makes that connection obvious (e.g. grouped with or immediately following the `exclusive=` facet), rather than leaving the caller to correlate two separate pieces of output themselves. This is a rendering/UX question for the plan, not a new mechanism.

## Design questions the plan should resolve

- **Where exactly is "teardown," precisely?** These cgroup files vanish the instant the scope directory is removed. Confirm the existing `PeakRSS` read already happens at the correct point (immediately before scope teardown, while the cgroup still exists) and add the new CPU-time/pids-peak reads at that exact same point — do not introduce a second read timing that could race the first.
- **Delegate-ram / nested-worker jobs.** For a `--delegate-ram` aitest run, is the outer confine scope's own `memory.peak`/`cpu.stat` already the correct whole-job aggregate (cgroup v2's hierarchical accounting should mean yes — a parent's `memory.peak` reflects its whole subtree, worker sub-scopes included), or does something about how workers are drained into a sub-scope (the `.aira-supervisor` drain pattern from the AIRA-91 investigation) complicate this? Verify against source rather than assume either way.
- **Honesty on read failure.** If a stat file can't be read (older kernel lacking `memory.peak`/`cpu.stat` — some of these are recent additions, or a genuinely-vanished scope), the existing `PeakRSS` pattern already does this right (`*int64`, nil means unestablished, rendered as absent/unevaluated never a fabricated zero) — follow the identical discipline for the new fields, don't invent a different failure convention.
- **`--json` output.** Confirm the JSON record already carries `peak_rss` (it should, per the struct tag) and that the new fields get equivalent `omitempty` JSON tags, consistent with every other optional facet on this struct.

## Not exclusive-only, but exclusive is what makes it trustworthy

This capability is generically useful for any confine job (a plain `aira confine -- make merge-gate` benefits from knowing its own peak RSS/CPU time just as much). What AIRA-101's exclusive mode adds is confidence that the number reflects the job alone, uncontended — worth stating in whatever documentation/help text describes this, so a caller understands why an exclusive run's numbers are meaningful in a way a contended run's aren't.

## Relation to other tickets

Builds directly on **AIRA-101** (the motivating use case, and the struct/pattern this extends) and **AIRA-22** (the durable-record/`--status` machinery this surfaces the new data through). Not related to AIRA-102/AIRA-103.
