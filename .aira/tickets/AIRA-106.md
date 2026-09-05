---
{"schema":1,"id":"AIRA-106","project":"aira","title":"Dynamic slice ceiling: replace single-headroom formula with min(TotalRAM-reserveMax, usage+(MemAvailable-freeMin))","status":"in-progress","kind":"feature","severity":"P1","assignee":null,"milestone":null,"labels":["admission","confine","memory-safety"],"hold":false,"relations":[]}
---
Owner decision (2026-09-05), replacing AIRA-103's own headroom formula with a better-specified one, as part of closing AIRA-91 Part B.

## What was asked, verbatim

Presented with a choice between "flip AIRA-103 to enforce as-is" (effective ceiling ~38-43GB against a configured 64GB, essentially always) or "lower `--memory-max` to match reality," the owner rejected both and specified a different, better model:

> "Currently we specify to leave 16GB for the system. Instead, we should specify a maximum amount to leave and an amount to leave free — so 'leave 16GB on the table' and 'leave 8GB free', meaning the slice would take min(total-16GB, free-8GB)"

## The two parameters, as I understand them — confirm during planning, don't assume

- **`reserveMax`** (example: 16GB) — "leave this much on the table": a static upper bound on how much of the machine the slice may ever claim, regardless of how idle everything else is. Equivalent in spirit to today's fixed `--memory-max` sizing, just named as a reserve rather than a cap.
- **`freeMin`** (example: 8GB) — "leave this much genuinely free": a dynamic floor. The slice's effective ceiling should tighten so that real system `MemAvailable` never has to drop below this, responsive to whatever else (desktop, other processes, previously-uncontained Docker containers per AIRA-102) is consuming memory right now.
- **Effective ceiling** = `min(TotalRAM − reserveMax, currentSliceUsage + (MemAvailable − freeMin))`.

The first term is a fixed cap independent of current conditions. The second is the dynamic term — it says "the slice may grow until doing so would push system-wide free memory below `freeMin`," expressed as current usage plus remaining headroom above the floor, so it composes with whatever the slice already holds rather than being a raw absolute figure.

## Relation to AIRA-103

This does not redesign AIRA-103's mechanism — the in-process capacity-throttle actuator (no kernel-enforced write, verified safe by two adversarial review rounds) stays exactly as built. What changes is the **formula that computes the published ceiling**: replace `desired = affordable − min(MemTotal/4, 16 GiB)` (a single blended headroom, `internal/daemon/sliceceiling.go`) with the explicit two-parameter model above, with `reserveMax`/`freeMin` as configurable values (default 16GB/8GB per the owner's own example — confirm whether these should be named constants, daemon config, or `aira install` flags).

Once built and verified, this should also **flip AIRA-103 out of `mode=off`** — the owner's answer to the ceiling question was a formula refinement, not a decision to leave the mechanism dormant; enabling it (observe first, then enforce, matching the existing mode ladder) is the natural completion of this ticket.

## Not decided here

- Exact configuration surface for `reserveMax`/`freeMin` (env vars matching `AIRA_DAEMON_SLICE_CEILING_MODE`'s existing pattern, `aira install` flags, or a config file value) — plan should pick the one most consistent with how AIRA-103 already exposes its own mode.
- Whether `sliceAnon`/`currentSliceUsage` in the dynamic term should be derived exactly as AIRA-103 already computes it (its own signal-derivation code, `sliceceiling.go`) — reuse that, don't rederive.
- The observe→enforce rollout sequencing (how long to run in `observe` before flipping to `enforce`, and who decides that) — flag for the owner rather than default.

## Full context

See AIRA-91 (Part B, now closed) and AIRA-103 (the mechanism this refines) for the complete history: why a kernel-enforced write was rejected, the measured non-slice footprint on this machine, and the two adversarial review rounds AIRA-103 already went through.
